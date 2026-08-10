// Command gonk-meter is the metering/budget authority: it registers projects,
// decides the rung/budget for every attempt (POST /v1/policy/decide, the
// money path), records outcomes, and serves cost. See internal/meter/service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // quiet_hours needs IANA data; the image has no /usr/share/zoneinfo

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/keysink"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/metrics"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/service"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// internal/meter/service has no logger of its own to thread through, and
	// the one thing it must be able to say -- "I could not provision this
	// project's virtual key" -- was silent, which cost a whole investigation
	// (gonk-zp3). Make its slog.Default() calls land in the same JSON stream
	// as everything else here rather than the text default.
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// Config is every knob main reads. SECRETS ARE FILE PATHS, never values: the
// environment leaks into `ps`, /proc/<pid>/environ, crash dumps, and every
// child process. A flag is visible in `ps` to anything on the node. The chain
// is Vault/1Password -> ExternalSecret -> k8s Secret -> projected FILE MOUNT.
type Config struct {
	OperatorConfigPath string // --operator-config (required)
	Listen             string // --listen (default :8080)

	LiteLLMURL              string // LITELLM_URL (required)
	LiteLLMAdminKeyFile     string // LITELLM_ADMIN_KEY_FILE (required)
	LiteLLMAdminKeyPrevFile string // LITELLM_ADMIN_KEY_PREVIOUS_FILE (optional rotation slot 2)

	MeterTokenFile     string // GONK_METER_TOKEN_FILE (required)
	MeterTokenPrevFile string // GONK_METER_TOKEN_PREVIOUS_FILE (optional rotation slot 2)

	StoreBackend string // GONK_METER_STORE_BACKEND (required; no default -- see openStore)
	StoreDSNFile string // GONK_METER_STORE_DSN_FILE (required)

	KeysinkNamespace string // GONK_KEYSINK_NAMESPACE (unset -> memory sink + a LOUD error log)
	KeysinkPrefix    string // GONK_KEYSINK_PREFIX (default "gonk-key-")

	SyncSpendInterval     time.Duration // GONK_SYNC_SPEND_INTERVAL (default 30s)
	JanitorInterval       time.Duration // GONK_JANITOR_INTERVAL (default 1m)
	ReconcileKeysInterval time.Duration // GONK_RECONCILE_KEYS_INTERVAL (default 1m)
	ReresolveInterval     time.Duration // GONK_RERESOLVE_INTERVAL (default 5m)
	RefreshGaugesInterval time.Duration // GONK_REFRESH_GAUGES_INTERVAL (default 15s)
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	operCfg, err := loadOperatorConfig(cfg.OperatorConfigPath)
	if err != nil {
		// opercfg.Load failing is FATAL: meter must not start on a config it
		// cannot validate, because every budget decision flows from it.
		return fmt.Errorf("load operator config: %w", err)
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}

	adminKey, err := readSecretFile(cfg.LiteLLMAdminKeyFile)
	if err != nil {
		return err
	}
	_, err = readSecretFile(cfg.LiteLLMAdminKeyPrevFile) // optional; validated, not yet wired into a two-slot admin client
	if err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	admin := litellm.NewHTTPAdmin(cfg.LiteLLMURL, adminKey, httpClient)
	spendSource := litellm.NewHTTPSpendSource(cfg.LiteLLMURL, adminKey, httpClient, litellm.WithRungCatalog(operCfg.Catalog))

	keySink, err := openKeysink(cfg, log)
	if err != nil {
		return err
	}

	svc := service.New(operCfg, st, admin, spendSource, keySink, Now)

	// A PRIVATE registry (never prometheus.DefaultRegisterer): no default
	// go_*/process_* series unless deliberately added, matching
	// cmd/gonk-intake's house style (pkg/intake/metrics.go).
	reg := prometheus.NewRegistry()
	mtr := metrics.New(reg)
	svc.SetMetrics(mtr)

	token, err := readSecretFile(cfg.MeterTokenFile)
	if err != nil {
		return err
	}
	prevToken, err := readSecretFile(cfg.MeterTokenPrevFile)
	if err != nil {
		return err
	}
	mux, err := service.NewMux(svc, token, prevToken, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	if err != nil {
		return err
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go runLoop(ctx, log, "sync-spend", cfg.SyncSpendInterval, svc.SyncSpend)
	go runLoop(ctx, log, "janitor", cfg.JanitorInterval, svc.Janitor)
	go runLoop(ctx, log, "reconcile-keys", cfg.ReconcileKeysInterval, svc.ReconcileKeys)
	go runLoop(ctx, log, "refresh-gauges", cfg.RefreshGaugesInterval, svc.RefreshGauges)
	go runLoop(ctx, log, "reresolve", cfg.ReresolveInterval, func(rctx context.Context) error {
		// The operator config is re-read from disk on every tick (the chart
		// mounts it as a ConfigMap; a ConfigMap update is a file change, not a
		// restart). If it fails to validate, keep the previous config and
		// alert -- never fall back to an unvalidated config or to no config.
		next, err := loadOperatorConfig(cfg.OperatorConfigPath)
		if err != nil {
			log.Error("operator config reload failed; keeping the previous config", "err", err)
		} else {
			svc.SetConfig(next)
		}
		return svc.Reresolve(rctx)
	})

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listener: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		stop()
		shutdown(srv)
		return err
	}

	shutdown(srv)
	return nil
}

func shutdown(srv *http.Server) {
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

// runLoop ticks fn on interval until ctx is done. Every loop is independent:
// one failing does not stop the others, and a failure is logged, never
// panicked -- a crash-looping meter is worse than a meter that logs and
// retries next tick.
func runLoop(ctx context.Context, log *slog.Logger, name string, interval time.Duration, fn func(context.Context) error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := fn(ctx); err != nil {
				log.Error("loop failed", "loop", name, "err", err)
			}
		}
	}
}

func loadOperatorConfig(path string) (*opercfg.OperatorConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read operator config %s: %w", path, err)
	}
	return opercfg.Load(raw)
}

// openStore selects the durable store. THERE IS NO DEFAULT, AND `memory` IS
// NOT SELECTABLE.
//
// The in-memory store loses reservations on restart, which is FAIL-OPEN on
// headroom for the length of the spend-log lag -- so a typo in an env var
// must never be able to produce it. An unknown backend, an empty DSN file, or
// a store that will not connect is a FATAL startup error.
//
// Only "postgres" is implemented: the owner settled the ledger backend on
// Postgres (docs/spikes/dolt-reservation-isolation.md -- Dolt did not
// serialize the concurrent-reservation race under any tested strategy).
// "dolt" is deliberately NOT a case here: there is no store.OpenDolt, and
// naming an unimplemented backend as selectable would be worse than refusing
// it outright.
func openStore(ctx context.Context, cfg Config) (store.Store, error) {
	dsn, err := readSecretFile(cfg.StoreDSNFile)
	if err != nil {
		return nil, fmt.Errorf("store DSN: %w", err)
	}
	switch cfg.StoreBackend {
	case "postgres":
		return store.NewPostgres(ctx, dsn)
	case "":
		return nil, errors.New("GONK_METER_STORE_BACKEND is required (postgres); there is no default, " +
			"because defaulting to the in-memory store would lose reservations on restart and fail OPEN on budget headroom")
	default:
		return nil, fmt.Errorf("GONK_METER_STORE_BACKEND=%q is not a store (want postgres); "+
			"`memory` is deliberately not selectable", cfg.StoreBackend)
	}
}

// openKeysink builds the virtual-key delivery sink. Without GONK_KEYSINK_NAMESPACE
// meter falls back to an in-process memory sink and logs a LOUD error: keys
// are then not being delivered to any pod, ever (AD-1).
func openKeysink(cfg Config, log *slog.Logger) (keysink.KeySink, error) {
	if cfg.KeysinkNamespace == "" {
		log.Error("GONK_KEYSINK_NAMESPACE is unset; virtual keys will NOT be delivered to any pod " +
			"(falling back to an in-process memory sink -- this is fine for a test, never for a real deployment)")
		return keysink.NewMemory(), nil
	}
	kcfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("keysink: in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(kcfg)
	if err != nil {
		return nil, fmt.Errorf("keysink: build clientset: %w", err)
	}
	prefix := cfg.KeysinkPrefix
	if prefix == "" {
		prefix = "gonk-key-"
	}
	return keysink.NewK8s(cs, cfg.KeysinkNamespace, prefix), nil
}

func loadConfig() (Config, error) {
	var cfg Config
	flag.StringVar(&cfg.OperatorConfigPath, "operator-config", "", "path to the mounted operator config YAML (required)")
	flag.StringVar(&cfg.Listen, "listen", ":8080", "listen address")
	flag.Parse()

	if cfg.OperatorConfigPath == "" {
		return Config{}, errors.New("--operator-config is required")
	}

	cfg.LiteLLMURL = os.Getenv("LITELLM_URL")
	cfg.LiteLLMAdminKeyFile = os.Getenv("LITELLM_ADMIN_KEY_FILE")
	cfg.LiteLLMAdminKeyPrevFile = os.Getenv("LITELLM_ADMIN_KEY_PREVIOUS_FILE")
	cfg.MeterTokenFile = os.Getenv("GONK_METER_TOKEN_FILE")
	cfg.MeterTokenPrevFile = os.Getenv("GONK_METER_TOKEN_PREVIOUS_FILE")
	cfg.StoreBackend = os.Getenv("GONK_METER_STORE_BACKEND")
	cfg.StoreDSNFile = os.Getenv("GONK_METER_STORE_DSN_FILE")
	cfg.KeysinkNamespace = os.Getenv("GONK_KEYSINK_NAMESPACE")
	cfg.KeysinkPrefix = os.Getenv("GONK_KEYSINK_PREFIX")

	cfg.SyncSpendInterval = 30 * time.Second
	cfg.JanitorInterval = time.Minute
	cfg.ReconcileKeysInterval = time.Minute
	cfg.ReresolveInterval = 5 * time.Minute
	cfg.RefreshGaugesInterval = 15 * time.Second
	for _, d := range []struct {
		env string
		dst *time.Duration
	}{
		{"GONK_SYNC_SPEND_INTERVAL", &cfg.SyncSpendInterval},
		{"GONK_JANITOR_INTERVAL", &cfg.JanitorInterval},
		{"GONK_RECONCILE_KEYS_INTERVAL", &cfg.ReconcileKeysInterval},
		{"GONK_RERESOLVE_INTERVAL", &cfg.ReresolveInterval},
		{"GONK_REFRESH_GAUGES_INTERVAL", &cfg.RefreshGaugesInterval},
	} {
		if v := os.Getenv(d.env); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", d.env, err)
			}
			*d.dst = parsed
		}
	}

	for name, v := range map[string]string{
		"LITELLM_URL":               strings.TrimSpace(cfg.LiteLLMURL),
		"LITELLM_ADMIN_KEY_FILE":    cfg.LiteLLMAdminKeyFile,
		"GONK_METER_TOKEN_FILE":     cfg.MeterTokenFile,
		"GONK_METER_STORE_BACKEND":  cfg.StoreBackend,
		"GONK_METER_STORE_DSN_FILE": cfg.StoreDSNFile,
	} {
		if v == "" {
			return Config{}, fmt.Errorf("%s is required", name)
		}
	}

	return cfg, nil
}

// readSecretFile reads a mounted secret. It trims exactly one trailing
// newline and refuses an empty file: an empty bearer token would accept
// every request; an empty DSN would silently fall back to nothing. An empty
// path is legal (an unused, optional rotation slot) and returns "", nil.
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret %s: %w", path, err)
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return s, nil
}
