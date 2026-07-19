//go:build component

// Package component_test is L2: the REAL cmd/gonk-meter binary against the REAL
// LiteLLM (ghcr.io/berriai/litellm-database:v1.92.0, OD-6) and the REAL Postgres
// ledger, with test/stubmodel behind LiteLLM. It is where every "we have never
// spoken to a real LiteLLM" question (P3-1..P3-9) finally gets an answer.
//
// It runs behind //go:build component -- the heavy layer, NOT the standing gate.
// The standing `go test ./...` excludes it. Determinism is preserved: no
// time.Sleep, every wait is harness.WaitFor on a predicate, and the clock is the
// testclock build of the meter binary driven by a file offset.
//
// Shared infra (one Postgres, one LiteLLM, one stub) boots once in TestMain;
// each test gets a FRESH ledger database (D2: a leaked reservation is a false
// pass) and, when it needs one, its own meter subprocess. Everything is torn
// down at the end -- containers included.
package component_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/test/harness"
	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

// The pinned images (OD-6 / docs/environment.md). A LiteLLM bump invalidates
// docs/spikes/litellm-verified.md and re-runs this task.
const (
	litellmImage  = "ghcr.io/berriai/litellm-database:v1.92.0"
	postgresImage = "docker.io/library/postgres:16"
	// componentPGPort avoids the dev box's often-occupied 5432 (the L1 fix work
	// used 5544).
	componentPGPort = 5544
)

// l2OperatorYAML is L2's operator layer. The rung catalog's local synthetic price
// ($0.25/1M for qwen-local) MUST equal the price configured in LiteLLM's model
// list (l2Models) -- TestSyntheticPricesAgree checks exactly that, and it is the
// only place in the system that reads both. The cloud rungs' real LiteLLM prices
// ($2/1M glm, $6/1M sonnet) are LiteLLM's own and are independent of est_cost_usd.
const l2OperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local, glm, sonnet]
rungs:
  - { name: qwen-local, kind: local, model: stub-qwen,   est_cost_usd: 0,    est_tokens: "50K",  synthetic_usd_per_1m_tokens: 0.25 }
  - { name: glm,        kind: cloud, model: stub-glm,     est_cost_usd: 0.40, est_tokens: "200K" }
  - { name: sonnet,     kind: cloud, model: stub-sonnet,  est_cost_usd: 2.00, est_tokens: "100K" }
meter:
  max_spend_staleness: 5m
  reservation_ttl: 60m
  max_clock_skew: 5m
  key_retry_backoff: 5m
  max_infra_retries: 3
  enforce_ladder_order: true
`

// l2Models is LiteLLM's model list. Typed independently of the operator config
// (as in production, two files), so P3-4 is a real agreement check, not a
// tautology. Local price MUST equal the catalog synthetic; cloud prices are real.
var l2Models = []harness.ModelUpstream{
	{ModelName: "stub-qwen", Price: harness.ModelPrice{InputPerToken: 0.25 / 1e6, OutputPerToken: 0.25 / 1e6}},
	{ModelName: "stub-glm", Price: harness.ModelPrice{InputPerToken: 2.0 / 1e6, OutputPerToken: 2.0 / 1e6}},
	{ModelName: "stub-sonnet", Price: harness.ModelPrice{InputPerToken: 6.0 / 1e6, OutputPerToken: 6.0 / 1e6}},
}

// ---------------------------------------------------------------- shared infra

var (
	shRuntime *harness.Runtime
	shCreds   *harness.Creds
	shPG      *harness.Postgres
	shLiteLLM *harness.LiteLLM
	shStub    *stubmodel.Server
	shCatalog map[string]opercfg.RungSpec
	meterBin  string // built once, -tags testclock
)

func TestMain(m *testing.M) {
	code, err := runMain(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "component TestMain: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runMain(m *testing.M) (int, error) {
	rt, err := harness.DetectRuntime()
	if err != nil {
		return 1, err
	}
	shRuntime = rt

	c, err := harness.NewCreds("", "l2-component")
	if err != nil {
		return 1, err
	}
	shCreds = c
	defer func() { _ = c.Cleanup() }()

	// Preflight the ports this layer binds under host networking.
	if err := harness.CheckPortsFree(harness.PortStubModel, harness.PortLiteLLM, componentPGPort); err != nil {
		return 1, err
	}

	cfg, err := opercfg.Load([]byte(l2OperatorYAML))
	if err != nil {
		return 1, fmt.Errorf("load l2 operator config: %w", err)
	}
	shCatalog = cfg.Catalog

	// The stub model on its fixed loopback port, reachable by the LiteLLM
	// container over host networking.
	shStub = stubmodel.New()
	stubStop, err := serveStub(shStub, harness.PortStubModel)
	if err != nil {
		return 1, err
	}
	defer stubStop()
	stubURL := "http://127.0.0.1:" + fmt.Sprintf("%d", harness.PortStubModel)

	pg, stopPG, err := harness.StartPostgres(rt, c, postgresImage, componentPGPort)
	if err != nil {
		return 1, err
	}
	shPG = pg
	defer stopPG()

	ll, stopLL, err := harness.StartLiteLLM(rt, c, litellmImage, pg.BaseDSN(), stubURL, l2Models)
	if err != nil {
		return 1, err
	}
	shLiteLLM = ll.WithCatalog(shCatalog)
	defer stopLL()

	// Build the meter binary ONCE, with the testclock seam.
	meterBin = c.Path("gonk-meter")
	if out, berr := exec.Command("go", "build", "-tags", "testclock", "-o", meterBin,
		"gitlab.orac.local/agentic/gonk-project/cmd/gonk-meter").CombinedOutput(); berr != nil {
		return 1, fmt.Errorf("build gonk-meter (testclock): %v: %s", berr, out)
	}

	return m.Run(), nil
}

// serveStub binds the stub on 127.0.0.1:port and serves until stop is called.
func serveStub(h http.Handler, port int) (stop func(), err error) {
	ln, err := harness.ListenLoopback(port)
	if err != nil {
		return nil, fmt.Errorf("harness: bind stub on %d: %w", port, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}

// ---------------------------------------------------------------- per-test world

// world is the shared infra plus per-test scoping. The heavy dependencies are
// package-global; a world just resets the stub and hands out a fresh ledger DSN
// and meter subprocesses.
type world struct {
	t       *testing.T
	LiteLLM *harness.LiteLLM
	Stub    *stubmodel.Server
	PG      *harness.Postgres
	Catalog map[string]opercfg.RungSpec
}

func newWorld(t *testing.T) *world {
	t.Helper()
	shStub.Reset()
	return &world{t: t, LiteLLM: shLiteLLM, Stub: shStub, PG: shPG, Catalog: shCatalog}
}

// alwaysAnswer scripts the stub to answer every request forever with the given
// per-call usage. Cost then = tokens x the model's configured LiteLLM price.
func alwaysAnswer(promptTokens, completionTokens int) []stubmodel.Step {
	return []stubmodel.Step{{
		Repeat:   stubmodel.Forever,
		Usage:    stubmodel.Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens},
		Response: stubmodel.Response{Content: "ok"},
	}}
}

func ptr[T any](v T) *T { return &v }
