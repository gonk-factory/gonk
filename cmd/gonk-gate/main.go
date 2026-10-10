// Command gonk-gate is Gate 2 and the sweeper -- one static binary with no
// model call in it anywhere. It ships in the controller image (Task 6, where
// its `dispatch`/`sweep` subcommands back the two exec orders); the agent
// image no longer bundles it (the `trailers` subcommand it used to carry was
// deleted -- see below).
//
// Subcommands:
//
//	gonk-gate dispatch   GATE 2. Re-decides via meter, runs/parks/denies.
//	gonk-gate sweep      Classify finished sessions, report outcomes, re-sling.
//	gonk-gate pipefix-judge  READ-ONLY: the pipeline-fix exit check (plan
//	                     gonk-2usw 6.3) over live GitLab facts, printed as JSON
//	                     (pipefix_judge.go). Its own exit codes: 0 a decision,
//	                     1 cannot-judge, 2 usage.
//
// There used to be a `trailers` subcommand (Task 8): the prepare-commit-msg
// hook's body, rendering and splicing a commit-provenance trailer block. It
// was never reachable in practice -- the agent pod's checkout is a GitLab
// repository-archive tarball (pkg/rig, ArchiveFetcher.RepoArchive), which
// carries no `.git/`, so entrypoint.sh's `if [ -d "${RIG_DIR}/.git" ]` guard
// around installing the hook never once took the true branch in a real pod,
// and with no hook installed `gonk-gate trailers` was never invoked either.
// Deleted along with images/agent/prepare-commit-msg and the entrypoint
// install step (spec §6.1's trailer paragraph is now marked "not in v1").
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
	"time"

	"github.com/gonk-factory/gonk/internal/envname"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/gcapi"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/rig"
)

// version is stamped at build time (`-ldflags -X main.version=...`) by both
// images/Dockerfile.agent and images/Dockerfile.controller, to GONK_TAG.
// "dev" is what a plain `go build`/`go run` gets, never a released image.
// Task 5 and Task 6's own smoke-test wishlists both wanted `--version`
// (flagged as a known gap in both, closed here): images/Dockerfile.controller's
// controller_smoke_test.go asserts this output equals GONK_TAG.
var version = "dev"

func main() {
	// FIRST, before anything that can take time: dispatch budgets its order
	// timeout from process start (order_budget.go, gonk-alw).
	processStart := time.Now()
	launch := readLaunchTiming(processStart, os.ReadFile)
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
		log.Error("usage: gonk-gate dispatch|sweep|sweep-health|http-probe|pipefix-judge|capabilities|workstore-import|workstore-export|workstore-survey|workstore-resolve")
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

	// sweep-health is answered before loadGateConfig for the same reason
	// --version is, and one more: it is the controller's READINESS PROBE
	// (gonk-pop3 item 4). A probe that exits 2 on a missing GONK_CITY would
	// report a healthy outcome path as unready, and a probe that could be
	// broken by unrelated configuration is not a probe, it is a second way to
	// take the deployment down. It reads a file and optionally dials a port;
	// it needs nothing else.
	if os.Args[1] == "sweep-health" {
		os.Exit(runSweepHealthCheck(os.Args[2:], os.Stdout, os.Stderr))
	}

	// http-probe is answered before loadGateConfig for the same reason: it is
	// the body of the meter-reachable DOCTOR CHECK (gonk-hckm), which runs as a
	// warm-up in a container that need not have dispatch's configuration, and a
	// probe that could be broken by unrelated configuration is not a probe. It
	// performs one HTTP GET with a timeout and reads no environment at all.
	if os.Args[1] == "http-probe" {
		os.Exit(runHTTPProbe(os.Args[2:], os.Stdout, os.Stderr))
	}

	// pipefix-judge (gonk-2usw, proof rung L2) is answered before
	// loadGateConfig as well: it is a READ-ONLY look at GitLab that needs no
	// city, no meter and no store, and must not be able to reach any of them.
	if os.Args[1] == "pipefix-judge" {
		os.Exit(pipefixJudgeMain(os.Args[2:]))
	}

	// The work store's cutover subcommands (gonk-t41) are answered before
	// loadGateConfig too: the bd import runs in the controller's bootstrap
	// initContainer, which has none of dispatch's configuration (no GONK_CITY,
	// no meter token), only the work store DSN and bd's location.
	switch os.Args[1] {
	case "capabilities":
		os.Exit(runCapabilities(os.Stdout))
	case "workstore-survey":
		os.Exit(runWorkStoreSurvey(context.Background(), log, syncConfigFromEnv(), openPostgresPlanStore, os.Stdout))
	case "workstore-resolve":
		os.Exit(runWorkStoreResolve(context.Background(), os.Args[2:], log, syncConfigFromEnv(), openPostgresResolveStore))
	case "workstore-import", "workstore-export":
		sctx, stopSignals := terminationContext(context.Background())
		code := runWorkStoreSync(sctx, os.Args[1], log, syncConfigFromEnv(), openPostgresSyncStore)
		stopSignals()
		os.Exit(code)
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
		// Gas City's order timeout arrives as SIGTERM to the process group with
		// 2s before SIGKILL. Catch it so dispatch stops without starting a store
		// write it cannot finish (order_budget.go).
		dctx, stopSignals := terminationContext(ctx)
		logLaunchGap(log, launch, envArg("bead_anchor"))
		// THE SESSION RUNTIME (gonk-t55): Kubernetes Jobs. A runtime that
		// cannot be built is a misconfiguration of this controller, not a
		// transient -- exit 2, never a guess.
		rt, rerr := newSessionRuntime()
		if rerr != nil {
			log.Error("misconfiguration: no session runtime; dispatching nothing", "err", rerr,
				"bead", envArg("bead_anchor"))
			os.Exit(2)
		}
		store, closeStore, serr := cfg.openWorkStore(dctx, log)
		if serr != nil {
			log.Error("gonk-dispatch cannot open its work store; dispatching nothing", "err", serr,
				"bead", envArg("bead_anchor"))
			os.Exit(workStoreExitCode(serr))
		}
		code = runDispatch(dctx, dispatchDeps{
			StartedAt: processStart,
			Meter:     cfg.meter(), Runtime: rt, Store: store, Forge: cfg.gl(), Log: log,
			// The environment the session runs with (plan D5): the pack whose
			// agent.toml [env] is the session's env, and the LiteLLM URL passed
			// through beside it. The IMAGE is the runtime's (jobs.Config.Image,
			// from envname.AgentImage): one reader of that variable, not two.
			PackDir: cfg.PackDir, LiteLLMURL: cfg.LiteLLMURL,
			// The canned status comment's write side (gonk-yrs).
			Apply: cfg.gl(), GL: cfg.gl(), BotUsername: cfg.BotUsername,
			// Follows the meter's KeyRef to the project's virtual key, so the
			// agent stops receiving the proxy admin key (gonk-8gb).
			Keys: newSecretReader(os.Getenv(envname.KeysinkNamespace)),
			// The per-session CHECKOUT (pkg/rig, gonk-msz). Both halves come from
			// GONK_RIG_BASE_URL: gonk-gate POSTs the grant here, and hands the
			// session the full fetch URL under the same base -- MOUNTED as the
			// session's rig-url file (runtime.Spec.RigURL), never env.
			// Unset simply means no checkout is granted -- scaffold then falls
			// back to controller-side repository context, never to inventing one.
			Rig:        cfg.rig(),
			RigBaseURL: os.Getenv("GONK_RIG_BASE_URL"),
			// The pipeline path's GitLab (gonk-tios): context, checkout
			// commit, the run note's lifecycle writes.
			Pipelines: cfg.gl(),
			// The fix session's preparer (gonk-2usw): part AGENT's context
			// and prompt behind part DISPATCH's seam (prepareFixSession).
			PrepareFix: pipefixPrepare,
			Args:       dispatchArgsFrom(envArg),
		})
		stopSignals()
		closeStore()
	case "sweep":
		rt, rerr := newSessionRuntime()
		if rerr != nil {
			// Recorded where the readiness probe looks, like a dead store: a
			// sweep that cannot see sessions is a dead outcome path.
			recordSweepFailure(log, sweepHealthFile(), "session_runtime", time.Now(), rerr)
			os.Exit(2)
		}
		store, closeStore, serr := cfg.openWorkStore(ctx, log)
		if serr != nil {
			// Recorded where the readiness probe looks, so a sweep that cannot
			// reach its store turns the controller unready on this pass rather
			// than after sweepHealthMaxAge of silence (gonk-p7qh's lesson).
			recordSweepFailure(log, sweepHealthFile(), "open_work_store", time.Now(), serr)
			os.Exit(workStoreExitCode(serr))
		}
		code = runSweep(ctx, sweepDeps{
			Meter: cfg.meter(), Runtime: rt, GL: cfg.gl(), Apply: cfg.gl(), Store: store, Log: log,
			// The pipeline path's GitLab (gonk-tios): validation reads, run
			// notes, run maintenance.
			Pipelines: cfg.gl(),
			// The pipeline-fix pass (gonk-2usw): on whenever a fix session
			// can be prepared, which this build can. A project that has not
			// opted in (actions.pipeline_fixes) costs it one read of its
			// meter registration per look and nothing else.
			FixesLive: pipefixPrepare != nil,
			// The controller's own credentials, for the fix gate's exact
			// secret match: a change that carries one is refused.
			FixSecrets: cfg.fixSecrets(),
			// Re-slings and unparks re-fire gonk-dispatch through Gas City's
			// order route: Gas City is still gonk-gate's scheduler until T-57,
			// and this is the one gcapi use left on the sweep (plan D8).
			Orders:      gcOrderRunner{c: cfg.gc()},
			BotUsername: cfg.BotUsername, PackDir: cfg.PackDir,
			// Off unless explicitly enabled: the fifth gate observes and logs
			// what it WOULD reject until its false-positive rate is measured
			// on real sessions (gonk-hsb).
			EnforceTrajectory: os.Getenv("GONK_ENFORCE_TRAJECTORY") == "1",
			// DEV ONLY, and off unless named: the session transcript archive
			// (gonk-pop3 item 2). Unset is the production default.
			TranscriptDir: os.Getenv(transcriptDirEnv),
			// Where this pass records whether the outcome path is alive. The
			// controller's readiness probe reads it (gonk-pop3 item 4).
			HealthFile: sweepHealthFile(),
			// One pass at a time: gc does not guarantee it (gonk-p8w8,
			// sweep_lease.go).
			LockFile: sweepLockFile(),
			// The Gas City pods of sessions in flight at the cutover are
			// deleted by their gc-agent label (plan 3.4 C1, sweep_legacy.go),
			// with the pod reaper's list/delete and its namespace switch. The
			// Gas City REAPER itself stays off the sweep (plan D7).
			LegacyPods:   podReaperOrNil(newPodReaper(os.Getenv(podReapNamespaceEnv))),
			LegacyCloser: gcLegacyCloser{c: cfg.gc()},
		})
		closeStore()
	default:
		log.Error("unknown subcommand", "arg", os.Args[1], "known", "dispatch, sweep, sweep-health, pipefix-judge, capabilities, workstore-import, workstore-export, workstore-survey, workstore-resolve, --version")
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

	// WorkStoreDSNFile is the persistent work store (gonk-t41): a FILE holding
	// the Postgres DSN of the WORK STORE -- role gonk_workstore, schema
	// gonk_workstore, from the gonk-workstore Secret. It is NOT the meter's
	// ledger role and NOT the Secret the meter reads: with the meter's role the
	// controller could settle reservations, delete spend rows and read every
	// project's LiteLLM key (owner decision B4 on the gonk-t41 review).
	// gonk-gate dispatch/sweep are each a SEPARATE process invocation, so an
	// in-memory store would lose every record between calls. See openWorkStore
	// for what is selected when.
	//
	// The name carries none of Gas City's secret markers
	// (internal/execenv.IsSensitiveKey: PASSWORD, TOKEN, SECRET, PRIVATE_KEY,
	// API_KEY, ACCESS_KEY, CREDENTIAL, OAUTH, AUTH_JSON), so it survives the
	// exec-order env strip. Renaming it to include one breaks every dispatch.
	WorkStoreDSNFile string

	// BdBin/BeadRepoDir locate the bd store (pkg/beadstore.BdCLI). It is read by
	// the one-time import and written by the rollback export -- AND it is still
	// the store dispatch and sweep run on whenever WorkStoreDSNFile is unset
	// (owner decision B6, expand then contract: the chart release that mounts
	// the work-store Secret is what moves this binary to Postgres, so an image
	// that lands ahead of its chart keeps running on bd). selectWorkStore has
	// the selection table.
	BdBin       string
	BeadRepoDir string

	// PackDir is the baked pack root the broker reads effect-shape.toml from,
	// and dispatch reads each agent's session env from (agent.toml [env], plan
	// D5). GONK_PACK_DIR, default /opt/gonk/pack (where Dockerfile.controller
	// bakes it).
	PackDir string

	// LiteLLMURL is passed through to every session as GONK_LITELLM_URL. Not a
	// secret; the key is mounted.
	LiteLLMURL string
}

// gcOrderRunner adapts the Gas City client to orderRunner (sweep.go). It is
// the one place a sweep path reaches gcapi, and it is here, in main.go, on
// purpose (plan D8): gonk-dispatch runs as a Gas City order until T-57.
type gcOrderRunner struct{ c *gcapi.Client }

func (g gcOrderRunner) RunOrder(ctx context.Context, name string, vars map[string]string) error {
	_, err := g.c.RunOrder(ctx, name, vars)
	return err
}

// gcLegacyCloser adapts the Gas City client to legacySessionCloser
// (sweep_legacy.go), mapping Gas City's 404 onto errLegacySessionGone. Like
// gcOrderRunner it lives here so no sweep file imports pkg/gcapi (exit
// criterion 2). It closes ONE alias a work record named; it is not the reaper.
type gcLegacyCloser struct{ c *gcapi.Client }

func (g gcLegacyCloser) CloseLegacySession(ctx context.Context, alias string) error {
	err := g.c.CloseSession(ctx, alias)
	if gcapi.IsNotFound(err) {
		return fmt.Errorf("%w: %s: %w", errLegacySessionGone, alias, err)
	}
	return err
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
		GitLabTokenFile:  firstNonEmpty(os.Getenv("GONK_GITLAB_TOKEN_FILE"), os.Getenv("GONK_BOT_FILE")),
		BotUsername:      os.Getenv("GONK_BOT_USERNAME"),
		WorkStoreDSNFile: os.Getenv(workStoreDSNFileEnv),
		BdBin:            os.Getenv("GONK_BD_BIN"),
		BeadRepoDir:      os.Getenv("GONK_BEAD_REPO_DIR"),
		PackDir:          firstNonEmpty(os.Getenv("GONK_PACK_DIR"), defaultPackDir),
		LiteLLMURL:       os.Getenv("GONK_LITELLM_URL"),
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

// fixSecrets are the controller-held values a fix's added lines are checked
// against, by exact match (effects.FindSecret). An unreadable file yields no
// entry: the key-shape patterns still apply.
func (c gateConfig) fixSecrets() []effects.Secret {
	var out []effects.Secret
	for name, file := range map[string]string{"gitlab_bot_token": c.GitLabTokenFile, "meter_token": c.MeterTokenFile} {
		if tok, err := readSecretFile(file); err == nil && tok != "" {
			out = append(out, effects.NewSecret(name, tok))
		}
	}
	return out
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

func (c gateConfig) openWorkStore(ctx context.Context, log *slog.Logger) (beadstore.Store, func(), error) {
	return selectWorkStore(ctx, log, c.WorkStoreDSNFile, c.BeadRepoDir, c.BdBin, openPostgresWorkStore)
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
