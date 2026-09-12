// Command gonk-intake is the GitLab side of gonk: it receives and verifies
// webhooks, reconciles the bot's project memberships and .gonk.yml state,
// opens the deterministic onboarding merge request, and dispatches work to the
// Gas City supervisor.
//
// It calls no language model. Ever.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "time/tzdata" // quiet_hours timezones are resolved by gonk-meter, not here; the image has no system tzdata

	"github.com/prometheus/client_golang/prometheus"

	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
	"gitlab.orac.local/agentic/gonk-project/pkg/rig"
)

// Config comes from the environment. SECRETS ARE READ FROM FILES, never from an
// env VALUE and never from a flag.
//
// The chain is Vault/1Password -> ExternalSecret -> k8s Secret -> projected FILE
// MOUNT. Env leaks into `ps`, into /proc/<pid>/environ, into crash dumps, and
// into every child process; a flag is visible in `ps` to anything on the node.
// The env vars below therefore carry PATHS, never material.
//
// TWO ROTATION SLOTS for every credential (the GitLab bot PAT, the webhook
// secret, the meter bearer token), so a rotation is not an outage: write the new
// secret to slot 1, the old to slot 2, roll, then drop slot 2.
//
// The two slots mean DIFFERENT THINGS in the two directions (AD-4b):
//   - a credential we VERIFY (the webhook token) -> accept EITHER slot;
//   - a credential we PRESENT (the GitLab PAT)   -> present slot 1, and on a
//     401/403 retry ONCE with slot 2 (glab.Client.SetPreviousToken).
//   - the meter bearer is presented by intake and VERIFIED BY METER, so intake
//     carries slot 1 only; meter (Plan 03 Task 8) holds slot 2 and accepts both.
//     GONK_METER_TOKEN_PREVIOUS_FILE is therefore METER's env var, not intake's.
//
// NO SECRET IS EVER COMMITTED. House rule; not negotiable.
type Config struct {
	GitLabURL             string // GONK_GITLAB_URL (https://gitlab.orac.local -- private CA, trusted via SSL_CERT_FILE)
	GitLabTokenFile       string // GONK_GITLAB_TOKEN_FILE          (bot PAT, rotation slot 1)
	GitLabTokenPrevFile   string // GONK_GITLAB_TOKEN_PREVIOUS_FILE (rotation slot 2, optional -- AD-4b)
	GitLabAdminTokenFile  string // GONK_GITLAB_ADMIN_TOKEN_FILE (optional; split-credential, AD-4; no previous slot)
	WebhookSecretFile     string // GONK_WEBHOOK_SECRET_FILE          (rotation slot 1)
	WebhookPrevSecretFile string // GONK_WEBHOOK_SECRET_PREVIOUS_FILE (rotation slot 2, optional)
	WebhookTokenGen       string // GONK_WEBHOOK_TOKEN_GEN            (bump on rotation)
	WebhookPublicURL      string // GONK_WEBHOOK_PUBLIC_URL  e.g. https://gonk.orac.local/hook/gitlab
	HookSSLVerify         bool   // GONK_WEBHOOK_SSL_VERIFY (default true; this is what GITLAB does, not what we do)
	BotUsername           string // GONK_BOT_USERNAME (default "gonk")
	MeterURL              string // GONK_METER_URL
	MeterTokenFile        string // GONK_METER_TOKEN_FILE (bearer we PRESENT; meter verifies both slots)
	SupervisorURL         string // GONK_SUPERVISOR_URL ("" -> LogDispatcher; else HTTPDispatcher -> gonk-dispatch order API, Plan 04 Task 2)

	// Write-auth (the grant-gated order-run route). The order-run POST carries a
	// fresh ed25519 X-GC-City-Write grant when a key is configured; the KEY IS A
	// FILE MOUNT, never an env value, never logged (same house rule as every
	// other secret above). Absent a key file, HTTPDispatcher sends no grant (a
	// grant-gated controller rejects it -- a deploy responsibility). The kid must
	// match the server's write_auth_verify_key entry; the cid is set only when
	// the controller is tenancy-scoped.
	WriteKeyFile string // GONK_GC_WRITE_KEY_FILE (PEM PKCS#8 ed25519 private key mount)
	WriteKeyID   string // GONK_GC_WRITE_KEY_ID   (kid)
	WriteCID     string // GONK_GC_WRITE_CID      (optional tenancy cid)

	// InstanceLadder is the operator ladder seeded into onboarding templates
	// (OD-B; the chart sets it from the same operator config meter resolves
	// from, Plan 05). GONK_INSTANCE_LADDER, comma-separated. REQUIRED and
	// MUST be non-empty: see the fail-closed check in loadConfig.
	InstanceLadder []string

	ReconcileInterval time.Duration // GONK_RECONCILE_INTERVAL (default 10m)
	// IssueSweepLimit and IssueSweepMaxAge bound the reconciler's issue sweep.
	// Zero leaves pkg/intake's defaults (5 per project per pass, 24h) in force.
	//
	// These are OPERATOR CONTROLS ON SPEND, and that is why they are env vars
	// rather than constants: every swept issue is a metered model call. Without
	// them, an operator watching an unexpected drain could not slow it, cap it
	// or stop it without a rebuild.
	//
	// Zero means "use the default", so neither can be set to zero to disable the
	// sweep. To wind it right down, set GONK_ISSUE_SWEEP_MAX_AGE very small
	// (e.g. 1s): nothing is recent enough to qualify and the sweep becomes a
	// no-op without touching the code path.
	IssueSweepLimit  int           // GONK_ISSUE_SWEEP_LIMIT
	IssueSweepMaxAge time.Duration // GONK_ISSUE_SWEEP_MAX_AGE
	// BlockedProjects names projects gonk must never work on, however it came
	// to see them (gonk-jn5). GONK_BLOCKED_PROJECTS, comma-separated, each
	// entry a path with namespace ("agentic/gonk-project") or a numeric id.
	//
	// gonk's OWN repository belongs here. `membership=true` includes projects
	// inherited through GROUP membership, and gonk-project sits in the same
	// group as the repos gonk works on -- so adding the bot to that group would
	// hand a code-writing agent a path to its own broker and gate, as a side
	// effect of onboarding rather than as a decision.
	BlockedProjects []string
	// StalenessWindow bounds how old a cached project entry may be before
	// dispatch refuses to fire for it (pkg/intake.Dispatch.StalenessWindow). Zero
	// resolves to intake's own safe default (30m) -- this is not a place to
	// "fix" a zero, just wire it through. GONK_DISPATCH_STALENESS_WINDOW.
	StalenessWindow time.Duration

	ListenAddr  string // GONK_LISTEN_ADDR  (default :8080, public: hook only)
	PrivateAddr string // GONK_PRIVATE_ADDR (default :9090, cluster-internal)
	Version     string // GONK_VERSION (build stamp)
}

func main() {
	// The `netpol-probe` subcommand is the chart's helm-test hook. It shares
	// this binary (and therefore the distroless intake image) rather than
	// needing a shell image; see netpolprobe.go for why.
	if len(os.Args) > 1 && os.Args[1] == "netpol-probe" {
		os.Exit(runNetpolProbe(os.Args[2:]))
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err // fail fast: a missing secret must not degrade into "no verification"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc, err := newService(ctx, cfg, log)
	if err != nil {
		return err
	}

	// events is bounded: a webhook flood must not grow the heap. A full queue
	// drops (with a metric) rather than blocking the handler -- reconciliation
	// remains the correctness path (spec 5.2). This consumer is the ONE place
	// queued events turn into Dispatch.Handle calls.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-svc.Events:
				svc.Dispatch.Handle(ctx, ev)
			}
		}
	}()

	pubSrv := &http.Server{Addr: cfg.ListenAddr, Handler: svc.Server.Public(), ReadHeaderTimeout: 5 * time.Second}
	privSrv := &http.Server{Addr: cfg.PrivateAddr, Handler: svc.Server.Private(), ReadHeaderTimeout: 5 * time.Second}

	go svc.Reconciler.Loop(ctx, cfg.ReconcileInterval)
	svc.Reconciler.Kick() // fire the first pass now rather than waiting a full interval
	go func() {
		// firstReconcileDone flips as soon as the FIRST pass (any pass; the Kick
		// above guarantees one starts immediately) completes, which is what
		// GET /readyz's "first reconcile completed" check reads.
		for {
			if _, ok := svc.Reconciler.LastSummary(); ok {
				svc.firstReconcileDone.Store(true)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()

	errCh := make(chan error, 2)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "role", "public")
		if err := pubSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("public listener: %w", err)
		}
	}()
	go func() {
		log.Info("listening", "addr", cfg.PrivateAddr, "role", "private")
		if err := privSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("private listener: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		stop()
		shutdown(pubSrv, privSrv)
		return err
	}

	shutdown(pubSrv, privSrv)
	return nil
}

// service is every piece newService wires up, short of the two listening
// sockets and signal handling (run()'s job). Splitting it out this way is what
// lets a test build the whole dependency graph against a fake GitLab and a
// fake meter and drive it through Server.Public()/Private() directly, with no
// real network involved.
type service struct {
	GL         *glab.Client
	Meter      *intake.MeterClient
	Metrics    *intake.Metrics
	Cache      *intake.Cache
	Dispatch   *intake.Dispatch
	Reconciler *intake.Reconciler
	Server     *intake.Server
	BotUserID  int64

	// Events is the bounded queue between the webhook handler's Sink and
	// Dispatch.Handle. run() drains it in a goroutine; a test may drain it
	// directly instead.
	Events chan *ghook.Event

	firstReconcileDone atomic.Bool
}

// newService builds the whole intake dependency graph: it reads every secret
// file, resolves the bot's GitLab identity (refusing to start on a mismatch),
// and wires the reconciler, Gate-1 dispatch, webhook handler, and the two
// listeners' mux (intake.Server). It does not open a listening socket.
func newService(ctx context.Context, cfg Config, log *slog.Logger) (*service, error) {
	botToken, err := readSecretFile(cfg.GitLabTokenFile)
	if err != nil {
		return nil, err
	}
	hookSecret, err := readSecretFile(cfg.WebhookSecretFile)
	if err != nil {
		return nil, err
	}
	prevSecret, err := readSecretFile(cfg.WebhookPrevSecretFile) // optional
	if err != nil {
		return nil, err
	}

	verifier, err := ghook.NewVerifier(hookSecret, prevSecret)
	if err != nil {
		return nil, err
	}

	gl := glab.New(cfg.GitLabURL, botToken)
	// AD-4b: rotation slot 2 for the PAT. Optional; absent in the steady state.
	prevPAT, err := readSecretFile(cfg.GitLabTokenPrevFile)
	if err != nil {
		return nil, err
	}
	if prevPAT != "" {
		gl.SetPreviousToken(prevPAT)
	}
	if cfg.GitLabAdminTokenFile != "" {
		adm, err := readSecretFile(cfg.GitLabAdminTokenFile)
		if err != nil {
			return nil, err
		}
		gl.AdminToken = adm
	}
	// NOTE: no TLS configuration anywhere. gitlab.orac.local's private CA is
	// trusted via SSL_CERT_FILE (set by the chart from the `trust-bundle`
	// ConfigMap). Disabling verification here is never the answer.

	// Resolve the bot's own user id. This is the loop guard: without it, gonk
	// would react to its own comments. Refuse to start without it, and refuse to
	// start if the token belongs to someone other than the configured bot.
	me, err := gl.CurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve bot identity: %w", err)
	}
	if me.Username != cfg.BotUsername {
		return nil, fmt.Errorf("token belongs to %q but GONK_BOT_USERNAME is %q; refusing to start",
			me.Username, cfg.BotUsername)
	}
	log.Info("bot identity", "username", me.Username, "id", me.ID)

	reg := prometheus.NewRegistry()
	metrics := intake.NewMetrics(reg)
	gl.AuthFallback = metrics.GitLabAuthFallback // AD-4b: a half-done rotation must be visible
	cache := intake.NewCache()

	var dispatcher intake.Dispatcher = intake.NewLogDispatcher(log)
	if cfg.SupervisorURL != "" {
		// OD-A is resolved (Plan 04, Task 2): POST /v0/city/{cityName}/order/gonk-dispatch/run,
		// body {"vars":{...}}. The route is grant-gated: build a Signer from the
		// file-mounted write-auth key so every order-run POST carries a fresh
		// ed25519 grant. No key file -> no signer (unsigned; a grant-gated
		// controller rejects it, which is the deploy's responsibility).
		var signer *gcapi.Signer
		if cfg.WriteKeyFile != "" {
			var opts []gcapi.SignerOption
			if cfg.WriteCID != "" {
				opts = append(opts, gcapi.WithCID(cfg.WriteCID))
			}
			signer, err = gcapi.LoadSigner(cfg.WriteKeyFile, cfg.WriteKeyID, opts...)
			if err != nil {
				return nil, fmt.Errorf("load write-auth signer: %w", err)
			}
		}
		dispatcher = intake.NewHTTPDispatcher(cfg.SupervisorURL, signer, nil)
	}

	meterToken, err := readSecretFile(cfg.MeterTokenFile)
	if err != nil {
		return nil, err
	}
	meter := intake.NewMeterClient(cfg.MeterURL, meterToken, nil)

	// Meter is the Gate-1 /policy/decide client too: the same seam intake uses for
	// project registration answers the pre-dispatch rung/budget decision. Labeler
	// is a thin glab wrapper that applies the idempotent `gonk::denied` label on a
	// Gate-1 `deny` (intake's ONLY GitLab write on the dispatch path).
	dp := &intake.Dispatch{
		Dispatcher: dispatcher, Meter: meter, Labeler: intake.NewDenyLabeler(gl),
		Cache: cache, BotUsername: cfg.BotUsername, Obs: metrics, Log: log,
		StalenessWindow: cfg.StalenessWindow,
	}

	// FAIL CLOSED at startup. An entry that will not parse is one that would
	// silently stop blocking, so refuse to start rather than run with a list
	// that blocks less than the operator wrote (gonk-jn5). This is the same rule
	// the chart applies to a missing Secret: fail LOUDLY, do not degrade.
	blocked, err := intake.ParseBlocklist(cfg.BlockedProjects)
	if err != nil {
		return nil, fmt.Errorf("blocked projects: %w", err)
	}
	if blocked.Empty() {
		log.Warn("NO PROJECTS ARE BLOCKED. gonk can act on every project its token can see, " +
			"including its own repository if the bot is ever added to that group. " +
			"Set GONK_BLOCKED_PROJECTS (intake.blockedProjects in the chart).")
	} else {
		log.Info("blocklist active", "projects", blocked.Entries())
	}

	// NewReconciler, not a bare literal: it refuses a zero BotUserID, the same
	// fail-closed rule ghook.NewHandler already applies below. me.ID came off
	// CurrentUser above and should never be zero, but "should never be" is
	// exactly the assumption a constructor exists to stop trusting.
	rec, err := intake.NewReconciler(&intake.Reconciler{
		GL: gl, Meter: meter, Cache: cache, Obs: metrics, Log: log,
		Onboarder: &intake.GitLabOnboarder{GL: gl, BotUserID: me.ID, BotUsername: cfg.BotUsername, Version: cfg.Version, InstanceLadder: cfg.InstanceLadder, Obs: metrics},
		Dispatch:  dp,
		// The issue sweep (spec 5.2: "Reconciliation is the correctness path;
		// webhooks are the latency optimization"). Same *Dispatch the webhook
		// worker below uses, so a swept issue takes the identical path through
		// the staleness window, Decide and the classification gate.
		//
		// Without this the reconciler is wired for projects only, and an issue
		// event that was ACKed and then lost -- a restart, a full queue -- is
		// lost permanently, because GitLab does not retry (gonk-vrf).
		Issues:           dp,
		Blocked:          blocked,
		IssueSweepLimit:  cfg.IssueSweepLimit,
		IssueSweepMaxAge: cfg.IssueSweepMaxAge,
		BotUserID:        me.ID, HookURL: cfg.WebhookPublicURL, HookToken: hookSecret,
		TokenGen: cfg.WebhookTokenGen, SSLVerify: cfg.HookSSLVerify,
		// NOTE: no Instance policy and no GroupPolicy. Intake does not hold
		// operator config and does not resolve -- gonk-meter does (Conflict A).
	})
	if err != nil {
		return nil, fmt.Errorf("build reconciler: %w", err)
	}
	dp.KickReconcile = rec.Kick // coalesced out-of-band pass

	events := make(chan *ghook.Event, 256)
	hook, err := ghook.NewHandler(verifier, ghook.NewDeduper(time.Hour, 8192), me.ID,
		func(e *ghook.Event) bool {
			select {
			case events <- e:
				return true
			default:
				return false
			}
		}, metrics)
	if err != nil {
		return nil, fmt.Errorf("build webhook handler: %w", err)
	}
	// One record per delivery, always on (gonk-pop3). Without it a delivery
	// that intake accepted and then dropped -- the !71 mention -- is invisible
	// except in GitLab's own hook delivery log.
	hook.Log = log

	svc := &service{
		GL: gl, Meter: meter, Metrics: metrics, Cache: cache,
		Dispatch: dp, Reconciler: rec, BotUserID: me.ID, Events: events,
	}
	// The per-session CHECKOUT (pkg/rig, gonk-msz). intake serves it because it
	// already holds the bot PAT and already runs an internal-only listener; the
	// agent pod holds neither, which is the point -- the pod gets a working copy
	// without a forge credential. Mounted on the PRIVATE listener only.
	svc.Server = intake.NewServer(intake.ServerConfig{
		Hook:      hook,
		Reg:       reg,
		Reconcile: rec,
		Ready:     &readiness{meter: meter, done: &svc.firstReconcileDone},
		Rig: &rig.Handler{
			Store: rig.NewStore(nil),
			Fetch: gl,
			Log:   log,
		},
	})
	return svc, nil
}

func shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
}

// readiness backs GET /readyz: bot identity resolved (implicit -- run() does
// not reach this point otherwise), the first reconcile pass completed, and
// meter reachable.
type readiness struct {
	meter *intake.MeterClient
	done  *atomic.Bool
}

func (r *readiness) Ready(ctx context.Context) error {
	if !r.done.Load() {
		return errors.New("first reconcile pass not yet complete")
	}
	if err := r.meter.Healthy(ctx); err != nil {
		return fmt.Errorf("meter unreachable: %w", err)
	}
	return nil
}

// loadConfig reads every GONK_* env var. Secrets are FILE PATHS here, never
// material -- see the Config doc comment.
func loadConfig() (Config, error) {
	cfg := Config{
		GitLabURL:             os.Getenv("GONK_GITLAB_URL"),
		GitLabTokenFile:       os.Getenv("GONK_GITLAB_TOKEN_FILE"),
		GitLabTokenPrevFile:   os.Getenv("GONK_GITLAB_TOKEN_PREVIOUS_FILE"),
		GitLabAdminTokenFile:  os.Getenv("GONK_GITLAB_ADMIN_TOKEN_FILE"),
		WebhookSecretFile:     os.Getenv("GONK_WEBHOOK_SECRET_FILE"),
		WebhookPrevSecretFile: os.Getenv("GONK_WEBHOOK_SECRET_PREVIOUS_FILE"),
		WebhookTokenGen:       os.Getenv("GONK_WEBHOOK_TOKEN_GEN"),
		WebhookPublicURL:      os.Getenv("GONK_WEBHOOK_PUBLIC_URL"),
		BotUsername:           os.Getenv("GONK_BOT_USERNAME"),
		MeterURL:              os.Getenv("GONK_METER_URL"),
		MeterTokenFile:        os.Getenv("GONK_METER_TOKEN_FILE"),
		SupervisorURL:         os.Getenv("GONK_SUPERVISOR_URL"),
		WriteKeyFile:          os.Getenv("GONK_GC_WRITE_KEY_FILE"),
		WriteKeyID:            os.Getenv("GONK_GC_WRITE_KEY_ID"),
		WriteCID:              os.Getenv("GONK_GC_WRITE_CID"),
		ListenAddr:            os.Getenv("GONK_LISTEN_ADDR"),
		PrivateAddr:           os.Getenv("GONK_PRIVATE_ADDR"),
		Version:               os.Getenv("GONK_VERSION"),
	}

	if cfg.BotUsername == "" {
		cfg.BotUsername = "gonk"
	}
	if cfg.WebhookTokenGen == "" {
		cfg.WebhookTokenGen = "1"
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.PrivateAddr == "" {
		cfg.PrivateAddr = ":9090"
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}

	cfg.HookSSLVerify = true
	if v := os.Getenv("GONK_WEBHOOK_SSL_VERIFY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("GONK_WEBHOOK_SSL_VERIFY: %w", err)
		}
		cfg.HookSSLVerify = b
	}

	cfg.ReconcileInterval = 10 * time.Minute
	if v := os.Getenv("GONK_RECONCILE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("GONK_RECONCILE_INTERVAL: %w", err)
		}
		cfg.ReconcileInterval = d
	}

	if v := os.Getenv("GONK_ISSUE_SWEEP_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("GONK_ISSUE_SWEEP_LIMIT: want a non-negative integer, got %q", v)
		}
		cfg.IssueSweepLimit = n
	}
	if v := os.Getenv("GONK_BLOCKED_PROJECTS"); v != "" {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				cfg.BlockedProjects = append(cfg.BlockedProjects, e)
			}
		}
	}
	if v := os.Getenv("GONK_ISSUE_SWEEP_MAX_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("GONK_ISSUE_SWEEP_MAX_AGE: want a non-negative duration, got %q", v)
		}
		cfg.IssueSweepMaxAge = d
	}
	if v := os.Getenv("GONK_DISPATCH_STALENESS_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("GONK_DISPATCH_STALENESS_WINDOW: %w", err)
		}
		cfg.StalenessWindow = d
	}

	// GONK_INSTANCE_LADDER FAILS CLOSED when unset or empty. This is intake's
	// OWN guard: intake does not run opercfg (meter does), so nothing else on
	// its side protects an empty operator ladder, which would otherwise let
	// RenderDefaultConfig emit a `.gonk.yml` whose `ladder:` is empty --
	// disabling every freshly-onboarded project (ADR-002). It mirrors
	// RenderDefaultConfig's own backstop, one layer earlier.
	var ladder []string
	for _, s := range strings.Split(os.Getenv("GONK_INSTANCE_LADDER"), ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			ladder = append(ladder, s)
		}
	}
	if len(ladder) == 0 {
		return Config{}, errors.New("GONK_INSTANCE_LADDER is unset or empty; refusing to start " +
			"(an empty instance ladder would onboard every new project disabled -- ADR-002)")
	}
	cfg.InstanceLadder = ladder

	for name, v := range map[string]string{
		"GONK_GITLAB_URL":          cfg.GitLabURL,
		"GONK_GITLAB_TOKEN_FILE":   cfg.GitLabTokenFile,
		"GONK_WEBHOOK_SECRET_FILE": cfg.WebhookSecretFile,
		"GONK_WEBHOOK_PUBLIC_URL":  cfg.WebhookPublicURL,
		"GONK_METER_URL":           cfg.MeterURL,
		"GONK_METER_TOKEN_FILE":    cfg.MeterTokenFile,
	} {
		if v == "" {
			return Config{}, fmt.Errorf("%s is required", name)
		}
	}

	return cfg, nil
}

// readSecretFile reads a mounted secret. It trims exactly one trailing newline
// (kubectl create secret --from-literal adds none; a shell heredoc adds one) and
// refuses an empty file: an empty webhook secret would accept every request. An
// empty path is legal (an unused, optional rotation slot) and returns "", nil.
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
