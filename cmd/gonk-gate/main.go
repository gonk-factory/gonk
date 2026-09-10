// Command gonk-gate is Gate 2, the sweeper, and (Task 8) the commit-trailer
// generator -- one static binary with no model call in it anywhere. It ships
// in both the controller image (Task 6, where its `dispatch`/`sweep`
// subcommands back the two exec orders) and the agent image (Task 5, where
// only `trailers` runs).
//
// Subcommands:
//
//	gonk-gate dispatch   GATE 2. Re-decides via meter, runs/parks/denies.
//	gonk-gate sweep      Classify finished sessions, report outcomes, re-sling.
//	gonk-gate trailers   The prepare-commit-msg hook's body (Task 8): renders
//	                     and splices the commit-provenance trailer block.
//
// There used to be a fourth subcommand, `check` -- the body of a formula's
// [steps.check], a re-run verification loop that asked "is the marker on the
// artifact yet?". ADR-007 §3 deleted the whole formula layer it belonged to
// (formulas, [steps.check], and the pack's control-dispatcher agent that
// routed their workflow-control beads); the same marker check that used to
// run in that loop now runs once, inline, from gonk-sweep (artifact.go).
//
// The exit-code contract (part of the pack's contract, not an implementation
// detail -- the orders' shell wrappers depend on it):
//
//	0  the command did what it was asked (for dispatch: run|defer|deny are
//	   all successful DECISIONS)
//	1  infrastructure error (meter unreachable, supervisor 5xx, bead store
//	   failure) -- the order retries per Gas City's own policy
//	2  misconfiguration (GONK_CITY unset, no meter token file, etc) -- never
//	   retry, a human must fix it
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/rig"
)

// version is stamped at build time (`-ldflags -X main.version=...`) by both
// images/Dockerfile.agent and images/Dockerfile.controller, to GONK_TAG.
// "dev" is what a plain `go build`/`go run` gets, never a released image.
// Task 5 and Task 6's own smoke-test wishlists both wanted `--version`
// (flagged as a known gap in both, closed here): images/Dockerfile.controller's
// controller_smoke_test.go asserts this output equals GONK_TAG.
var version = "dev"

func main() {
	// Records go to stderr AND to a durable sink, because as a Gas City exec
	// order stderr alone is a dead end: Gas City buffers stdout+stderr together
	// and drops the buffer when the order SUCCEEDS, which gonk-sweep always
	// does by design. See logsink.go for the mechanism (gonk-6a6).
	logOut, closeLogSink, logSinkNote := openLogSink(logSinkPath(os.LookupEnv), os.Stderr)
	defer closeLogSink()
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// The package default too: a few call sites (the GONK_BEAD_REPO_DIR
	// warning below among them) log through slog.Default(), and those were
	// being discarded for the same reason.
	slog.SetDefault(log)
	if logSinkNote != "" {
		log.Warn("log sink degraded", "detail", logSinkNote)
	}
	if len(os.Args) < 2 {
		log.Error("usage: gonk-gate dispatch|sweep|trailers")
		os.Exit(2)
	}

	// --version/-version: answered before loadGateConfig, deliberately --
	// unlike every real subcommand this needs no GONK_CITY/meter-token
	// config, and must work in a bare `podman run --entrypoint gonk-gate
	// <image> --version` smoke test with no env set at all.
	if os.Args[1] == "--version" || os.Args[1] == "-version" {
		fmt.Println(version)
		os.Exit(0)
	}

	// trailers is answered BEFORE loadGateConfig, deliberately -- unlike every
	// other subcommand it is invoked directly by a git hook inside the agent
	// pod (not as a Gas City exec order), and it needs neither GONK_CITY nor
	// GONK_SUPERVISOR_URL (it never calls gcapi). Task 8's own contract, named
	// three separate times in the spec that added it: a trailer lookup must
	// NEVER fail a commit. Routing it through loadGateConfig's hard
	// requirements would mean a plain `git commit` in a pod that has not yet
	// been given GONK_CITY exits 2 and (absent the hook's own `|| true`)
	// blocks the commit -- exactly the failure mode this subcommand exists to
	// rule out. It still WANTS a meter URL/token (to look up the project's
	// provenance policy and, if asked, its session cost), so it builds its
	// own minimal meter client straight from env, best-effort.
	if os.Args[1] == "trailers" {
		meterTok, _ := readSecretFile(os.Getenv("GONK_METER_TOKEN_FILE")) // "" is fine -- a 401 degrades to the shipped default
		os.Exit(runTrailers(context.Background(), trailersDeps{
			Meter:   newMeterAPI(os.Getenv("GONK_METER_URL"), meterTok),
			Log:     log,
			Version: version,
		}, trailersArgs{
			CommitMsgFile: trailersCommitMsgFile(os.Args[2:]),
			Model:         envArg("model"),
			MetadataJSON:  envArg("metadata_json"),
		}))
	}

	cfg, err := loadGateConfig()
	if err != nil {
		log.Error("misconfiguration", "err", err)
		os.Exit(2)
	}

	ctx := context.Background()
	var code int
	switch os.Args[1] {
	case "dispatch":
		code = runDispatch(ctx, dispatchDeps{
			Meter: cfg.meter(), GC: cfg.gc(), Store: cfg.store(), Forge: cfg.gl(), Log: log,
			// The canned status comment's write side (gonk-yrs).
			Apply: cfg.gl(), GL: cfg.gl(), BotUsername: cfg.BotUsername,
			// Follows the meter's KeyRef to the project's virtual key, so the
			// agent stops receiving the proxy admin key (gonk-8gb).
			Keys: newSecretReader(os.Getenv("GONK_KEYSINK_NAMESPACE")),
			// The per-session CHECKOUT (pkg/rig, gonk-msz). Both halves come from
			// GONK_RIG_BASE_URL: gonk-gate POSTs the grant here, and the agent pod
			// GETs from the same base with its own GC_ALIAS appended. Unset simply
			// means no checkout is granted -- scaffold then falls back to
			// controller-side repository context, never to inventing one.
			Rig:        cfg.rig(),
			RigBaseURL: os.Getenv("GONK_RIG_BASE_URL"),
			Args: dispatchArgs{
				Project:       envArg("project"),
				ProjectID:     envArgInt64("project_id"),
				Rig:           envArg("rig"),
				IssueIID:      envArgInt64("issue_iid"),
				BeadAnchor:    envArg("bead_anchor"),
				BeadID:        envArg("bead_id"),
				SessionKey:    envArg("session_key"),
				Trigger:       envArg("trigger"),
				ConfigHash:    envArg("config_hash"),
				Rung:          envArg("rung"),
				Model:         envArg("model"),
				ReservationID: envArg("reservation_id"),
			},
		})
	case "sweep":
		code = runSweep(ctx, sweepDeps{
			Meter: cfg.meter(), GC: cfg.gc(), GL: cfg.gl(), Apply: cfg.gl(), Store: cfg.store(), Log: log,
			BotUsername: cfg.BotUsername, PackDir: cfg.PackDir,
			// Off unless explicitly enabled: the fifth gate observes and logs
			// what it WOULD reject until its false-positive rate is measured
			// on real sessions (gonk-hsb).
			EnforceTrajectory: os.Getenv("GONK_ENFORCE_TRAJECTORY") == "1",
		})
	default:
		log.Error("unknown subcommand", "arg", os.Args[1])
		code = 2
	}
	os.Exit(code)
}

// gateConfig is every GONK_* env var gonk-gate reads. Secrets are FILE PATHS,
// never values (the same house rule cmd/gonk-intake follows): the chain is
// Vault/1Password -> ExternalSecret -> k8s Secret -> projected file mount.
type gateConfig struct {
	City          string // GONK_CITY (OD-1: no default)
	SupervisorURL string // GONK_SUPERVISOR_URL

	// Write-auth for the grant-gated order-run route. dispatch/sweep both POST
	// order runs (gonk-dispatch, re-slings), so both need a signer when the
	// controller is grant-gated. The KEY IS A FILE MOUNT, never an env value,
	// never logged (the same house rule as every other secret). Absent a key
	// file, no grant is sent and a grant-gated controller rejects the pour --
	// a deploy responsibility. signer is built once in loadGateConfig so a bad
	// key file fails loudly as a misconfiguration (exit 2) rather than at pour
	// time.
	WriteKeyFile string // GONK_GC_WRITE_KEY_FILE (PEM PKCS#8 ed25519 private key mount)
	WriteKeyID   string // GONK_GC_WRITE_KEY_ID   (kid)
	WriteCID     string // GONK_GC_WRITE_CID      (optional tenancy cid)
	signer       *gcapi.Signer

	MeterURL       string
	MeterTokenFile string

	GitLabURL       string
	GitLabTokenFile string

	BotUsername string // GONK_BOT_USERNAME, default "gonk"

	// BdBin/BeadRepoDir select the persistent bead store (pkg/beadstore.BdCLI):
	// gonk-gate dispatch/sweep/check are each a SEPARATE process invocation, so
	// an in-memory store would lose every record between calls. BeadRepoDir
	// empty falls back to an in-memory store with a loud warning -- usable only
	// for a smoke test, never in the controller.
	BdBin       string
	BeadRepoDir string

	// PackDir is the baked pack root the broker reads effect-shape.toml from.
	// GONK_PACK_DIR, default /opt/gonk/pack (where Dockerfile.controller bakes it).
	PackDir string
}

func loadGateConfig() (gateConfig, error) {
	cfg := gateConfig{
		City:          os.Getenv("GONK_CITY"),
		SupervisorURL: os.Getenv("GONK_SUPERVISOR_URL"),
		WriteKeyFile:  os.Getenv("GONK_GC_WRITE_KEY_FILE"),
		WriteKeyID:    os.Getenv("GONK_GC_WRITE_KEY_ID"),
		WriteCID:      os.Getenv("GONK_GC_WRITE_CID"),
		MeterURL:      os.Getenv("GONK_METER_URL"),
		// GONK_METER_TOKEN_FILE works for gonk-intake, but the in-controller exec
		// orders (dispatch/sweep) run under Gas City, which STRIPS inherited env
		// whose key contains a secret marker (internal/execenv.IsSensitiveKey:
		// "TOKEN" among them). So the controller ALSO exports GONK_METER_BEARER_FILE
		// -- same path, a name with no secret marker -- which survives the strip.
		// (A [order.env] override does not: `gc init` drops it from the city copy.)
		MeterTokenFile: firstNonEmpty(os.Getenv("GONK_METER_TOKEN_FILE"), os.Getenv("GONK_METER_BEARER_FILE")),
		GitLabURL:      os.Getenv("GONK_GITLAB_URL"),
		// Same exec-order strip as the meter token above: GONK_GITLAB_TOKEN_FILE
		// carries the "TOKEN" marker, so execenv.IsSensitiveKey strips it from
		// dispatch/sweep (both exec orders) and cfg.gl() would get an empty path
		// -> glab.New with no token -> every broker forge write 401s. The
		// controller also exports the SAME bot-token path under the marker-free
		// name GONK_BOT_FILE; reuse it as the fallback so the broker's own
		// bot-PAT access survives the strip.
		GitLabTokenFile: firstNonEmpty(os.Getenv("GONK_GITLAB_TOKEN_FILE"), os.Getenv("GONK_BOT_FILE")),
		BotUsername:     os.Getenv("GONK_BOT_USERNAME"),
		BdBin:           os.Getenv("GONK_BD_BIN"),
		BeadRepoDir:     os.Getenv("GONK_BEAD_REPO_DIR"),
		PackDir:         firstNonEmpty(os.Getenv("GONK_PACK_DIR"), "/opt/gonk/pack"),
	}
	if cfg.BotUsername == "" {
		cfg.BotUsername = "gonk"
	}
	if cfg.City == "" {
		// OD-1: no default city, ever. dispatch/sweep will fail this the moment
		// they try to build a *gcapi.Client, but refusing here is louder.
		return gateConfig{}, fmt.Errorf("GONK_CITY is unset (OD-1: there is no default)")
	}
	if cfg.MeterTokenFile == "" {
		return gateConfig{}, fmt.Errorf("GONK_METER_TOKEN_FILE is unset")
	}
	// Build the write-auth signer once, here, so an unreadable/invalid key file
	// is a loud misconfiguration (exit 2) rather than a per-pour surprise. An
	// unset key file leaves signer nil -- no grant, the loopback default.
	if cfg.WriteKeyFile != "" {
		var opts []gcapi.SignerOption
		if cfg.WriteCID != "" {
			opts = append(opts, gcapi.WithCID(cfg.WriteCID))
		}
		s, err := gcapi.LoadSigner(cfg.WriteKeyFile, cfg.WriteKeyID, opts...)
		if err != nil {
			return gateConfig{}, fmt.Errorf("load write-auth signer: %w", err)
		}
		cfg.signer = s
	}
	return cfg, nil
}

func (c gateConfig) meter() *meterAPI {
	tok, err := readSecretFile(c.MeterTokenFile)
	if err != nil {
		// meter.Decide/Outcome will fail closed on the first call; there is no
		// safe way to "guess" a token, so an empty one is what an unreadable
		// file becomes -- every request will 401, which is a loud, safe failure.
		tok = ""
	}
	return newMeterAPI(c.MeterURL, tok)
}

func (c gateConfig) gc() *gcapi.Client {
	client := gcapi.New(c.SupervisorURL, c.City)
	// nil when no key file was configured -- gcapi then sends no grant, the
	// unchanged loopback path. When set, every RunOrder POST is grant-signed.
	client.Signer = c.signer
	return client
}

func (c gateConfig) gl() *glab.Client {
	tok, _ := readSecretFile(c.GitLabTokenFile)
	return glab.New(c.GitLabURL, tok)
}

// rig builds the client that registers a per-session checkout with gonk-intake.
//
// Returns nil when GONK_RIG_BASE_URL is unset, which is a legitimate
// configuration and not a warning: the caller treats a nil rig as "grant no
// checkout" and falls back to controller-side repository context. Nothing here
// is fatal -- a session without a working copy still runs, on a prompt that
// says so (gonk-msz).
func (c gateConfig) rig() *rig.Client {
	base := os.Getenv("GONK_RIG_BASE_URL")
	if base == "" {
		return nil
	}
	return &rig.Client{BaseURL: base}
}

func (c gateConfig) store() beadstore.Store {
	if c.BeadRepoDir == "" {
		slog.Default().Warn("GONK_BEAD_REPO_DIR is unset; using an in-memory bead store " +
			"that does NOT survive between gonk-gate invocations -- fine for a smoke test, never for the controller")
		return beadstore.NewMemory()
	}
	return &beadstore.BdCLI{Bin: c.BdBin, Dir: c.BeadRepoDir}
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// readSecretFile reads a mounted secret, trimming exactly one trailing
// newline. An empty path is legal (an unused optional value) and returns "",
// nil; an unreadable or empty file is an error the caller decides how to
// treat.
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

// envArg reads one of an order's declared [order.params]. Gas City namespaces
// them into the exec's environment as GC_WEBHOOK_ARG_<name> -- WITH THE PARAM
// NAME VERBATIM, NOT UPPERCASED. internal/webhookmatch/extract.go's ExecEnvVars
// is a plain `out[ExecEnvArgPrefix+k] = v` over the declared param names, and
// every gonk [order.params] key is lower_snake_case, so the real variable is
// GC_WEBHOOK_ARG_trigger -- never GC_WEBHOOK_ARG_TRIGGER.
//
// This read USED to uppercase unconditionally, so EVERY arg came back "" and
// gonk-dispatch died on `unknown trigger; pouring nothing` with trigger="" --
// after meter had already been asked, i.e. it burned a decision and poured
// nothing. Verbatim is tried first and the uppercase spelling is kept only as a
// fallback, so a future Gas City that does normalize keys still works.
func envArg(name string) string {
	if v := os.Getenv("GC_WEBHOOK_ARG_" + name); v != "" {
		return v
	}
	return os.Getenv("GC_WEBHOOK_ARG_" + strings.ToUpper(name))
}

func envArgInt64(name string) int64 {
	v, _ := strconv.ParseInt(envArg(name), 10, 64)
	return v
}
