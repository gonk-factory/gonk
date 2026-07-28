// Package integration_test is L1: the in-process integration suite. It wires the
// REAL pkg/intake reconciler and the REAL internal/meter/service in ONE process,
// talking to each other over REAL HTTP through the REAL pkg/meterapi client --
// which is what closes P3-10 (every Plan 02 intake test ran against a fakeMeter;
// this is the first time the real client speaks to the real server). Around them
// sit the deterministic building blocks the earlier tasks shipped: test/stubmodel
// (the model), test/harness (SyntheticSession, creds, FakeClock), test/ledger
// (the three-way assertion), test/corpus (the hostile .gonk.yml set), plus the
// in-memory glabtest GitLab, a fake LiteLLM admin+spend API, and the memory Store.
//
// NO BUILD TAG. This runs in the standing `go test` gate on every commit, with
// -race, in well under a minute, with zero containers and zero time.Sleep: every
// non-determinism source (the clock, the spend poll, the reconcile pass) has an
// explicit seam the harness drives.
package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/keysink"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/service"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
	"gitlab.orac.local/agentic/gonk-project/test/harness"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

// ---------------------------------------------------------------- operator config

// baseOperatorYAML is L1's operator layer. The rung catalog matches the plan's
// L2 price table (qwen-local $0.25/1M synthetic, glm $0.40/attempt, sonnet
// $2.00/attempt) so a scenario asserted here reads the same up the ladder. The
// ONLY value that is common to both L1 and the chart's L3 catalog is qwen-local
// at $0.25 synthetic; everything else is this layer's own choice.
const baseOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local, glm, sonnet]
# The integration ladder climbs into cloud rungs (glm, sonnet); these tests
# predate and are not about the cloud-allowance gate (Stream B), so cloud is
# allowed here. Tests that specifically want cloud denied override via
# WithOperatorYAML. The gate's own behavior is covered in pkg/rung/decide_test.go.
cloud_allowance:
  enabled: true
rungs:
  - { name: qwen-local, kind: local, model: stub-local,  est_cost_usd: 0,    est_tokens: "50K",  synthetic_usd_per_1m_tokens: 0.25 }
  - { name: glm,        kind: cloud, model: stub-cloud,   est_cost_usd: 0.40, est_tokens: "200K" }
  - { name: sonnet,     kind: cloud, model: stub-sonnet,  est_cost_usd: 2.00, est_tokens: "100K" }
meter:
  max_spend_staleness: 5m
  reservation_ttl: 60m
  max_clock_skew: 5m
  key_retry_backoff: 5m
  max_infra_retries: 3
  enforce_ladder_order: true
`

// operatorYAMLInstanceEnabled renders the base operator config with the instance
// kill switch flipped -- SAME rung catalog, SAME everything else. It is how the
// P2-9 test flips a project to `disabled` WITHOUT the project's own .gonk.yml
// bytes changing (the config_hash short-circuit hazard).
func operatorYAMLInstanceEnabled(enabled bool) string {
	return strings.Replace(baseOperatorYAML,
		"instance:\n  enabled: true", "instance:\n  enabled: "+boolStr(enabled), 1)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// baseClock is a fixed instant well inside a calendar month, so a month window
// exists and reservation/quiet-hours arithmetic is stable across runs.
var baseClock = time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)

// onboardingLadder is the instance ladder intake seeds into the onboarding MR's
// .gonk.yml template (OD-B). It is deliberately LOCAL-ONLY -- the conservative
// spec-5.3 default the brick test guards.
var onboardingLadder = []string{"qwen-local"}

// ---------------------------------------------------------------- World

// World is the whole system minus the things that need a container. Everything is
// REAL except GitLab (glabtest), LiteLLM (fakeLLM), the model (stubmodel), and
// the controller (stubController): the real meter service and the real intake
// reconciler talk over real HTTP via the real meterapi client.
type World struct {
	t *testing.T

	GitLab     *glabtest.Server
	Model      *stubmodel.Server
	LLM        *fakeLLM
	Clock      *harness.FakeClock
	Store      *store.Memory
	Meter      *meterHandle
	Controller *stubController

	cfg       *opercfg.OperatorConfig
	svc       *service.Service
	meterURL  string
	modelURL  string
	token     string
	authTrans http.RoundTripper

	// project/rig is the "current" project Session binds to. Every L1 scenario
	// operates on a single project per World; the onboard/register helpers set it.
	project string
	rig     string

	reconciler *intake.Reconciler
	cache      *intake.Cache
	intakeURL  string // private listener base URL
	cancel     context.CancelFunc

	Views ledger.Views
}

// Option configures a World at construction.
type Option func(*worldOpts)

type worldOpts struct {
	operatorYAML string
	clock        time.Time
}

// WithOperatorYAML overrides the operator config the meter is built with.
func WithOperatorYAML(y string) Option { return func(o *worldOpts) { o.operatorYAML = y } }

// WithClock places the World's clock at a specific instant (for the quiet-hours
// window). The initial spend sync then happens at that instant.
func WithClock(t time.Time) Option { return func(o *worldOpts) { o.clock = t } }

// NewWorld builds and wires the whole in-process system.
func NewWorld(t *testing.T, opts ...Option) *World {
	t.Helper()
	wo := worldOpts{operatorYAML: baseOperatorYAML, clock: baseClock}
	for _, o := range opts {
		o(&wo)
	}

	cfg, err := opercfg.Load([]byte(wo.operatorYAML))
	if err != nil {
		t.Fatalf("opercfg.Load: %v", err)
	}

	w := &World{
		t:     t,
		Clock: harness.NewFakeClock(wo.clock),
		Store: store.NewMemory(),
		Model: stubmodel.New(),
		cfg:   cfg,
		cache: intake.NewCache(),
		token: "meter-bearer-token-abc123",
	}
	w.GitLab = glabtest.New(t)
	w.GitLab.Me = glab.User{ID: botUserID, Username: botUsername}

	// The stub model on its own httptest listener; the pack (SyntheticSession)
	// calls it directly at L1.
	modelSrv := httptest.NewServer(w.Model)
	t.Cleanup(modelSrv.Close)
	w.modelURL = modelSrv.URL

	// ---- the fake LiteLLM: admin (virtual keys) + spend (derived from the stub).
	admin := litellm.NewFake()
	w.LLM = &fakeLLM{
		admin:   admin,
		stub:    w.Model,
		catalog: cfg.Catalog,
		now:     w.Clock.Now,
		at:      map[int]time.Time{},
	}

	// ---- the REAL meter service, on an httptest listener.
	keys := keysink.NewMemory()
	w.svc = service.New(cfg, w.Store, admin, w.LLM, keys, w.Clock.Now)
	mux, err := service.NewMux(w.svc, w.token, "", nil)
	if err != nil {
		t.Fatalf("service.NewMux: %v", err)
	}
	meterSrv := httptest.NewServer(mux)
	t.Cleanup(meterSrv.Close)
	w.meterURL = meterSrv.URL
	w.Meter = &meterHandle{w: w}

	// Every meter call from the harness carries the bearer token; the model calls
	// carry it too, harmlessly (the stub ignores auth).
	w.authTrans = &authRoundTripper{token: w.token, base: http.DefaultTransport}

	// Prime one spend sync so meter is Ready() (synced && skewOK): otherwise every
	// budgeted decision defers on spend-stale before any spend has even happened.
	w.SyncSpend(t)

	// ---- the stub controller (grant-gated), on an httptest listener.
	w.Controller = newStubController(t, wa(t).PublicKey(), writeAuthKID)

	// ---- the REAL intake reconciler, pointed at the real meter over HTTP.
	glClient := w.GitLab.Client()
	meterClient := intake.NewMeterClient(w.meterURL, w.token, &http.Client{Timeout: 10 * time.Second})

	signer, err := wa(t).Signer()
	if err != nil {
		t.Fatalf("write-auth signer: %v", err)
	}
	dispatcher := intake.NewHTTPDispatcher(w.Controller.URL(), signer, nil)

	dispatch := &intake.Dispatch{
		Dispatcher:  dispatcher,
		Meter:       meterClient,
		Cache:       w.cache,
		BotUsername: botUsername,
		Obs:         intake.NopObserver{},
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         w.Clock.Now,
	}
	onboarder := &intake.GitLabOnboarder{
		GL:             glClient,
		BotUserID:      botUserID,
		BotUsername:    botUsername,
		Version:        "test",
		InstanceLadder: onboardingLadder,
		Obs:            intake.NopObserver{},
	}
	w.reconciler = &intake.Reconciler{
		GL:        glClient,
		Meter:     meterClient,
		Cache:     w.cache,
		Onboarder: onboarder,
		Dispatch:  dispatch,
		Obs:       intake.NopObserver{},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUserID: botUserID,
		HookURL:   "http://intake.invalid/hook/gitlab",
		HookToken: "webhook-secret-token-01234567890123456789",
		TokenGen:  "1",
		SSLVerify: false,
		// A tiny resync interval forces intake to re-PUT every pass even when the
		// project's .gonk.yml bytes have not changed -- the production defense
		// (default 1h) against the config_hash short-circuit swallowing an operator
		// kill switch, accelerated so one pass suffices without a sleep (P2-9).
		MeterResyncInterval: time.Nanosecond,
	}

	// Run the real intake HTTP server (Private listener) and drive reconcile
	// through POST /admin/reconcile?wait=true (HB-1), exercising the real
	// WaitForNextPass predicate rather than a sleep.
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	t.Cleanup(cancel)
	go w.reconciler.Loop(ctx, 24*time.Hour)

	intakeSrv := httptest.NewServer(intake.NewServer(intake.ServerConfig{
		Reconcile: w.reconciler,
	}).Private())
	t.Cleanup(intakeSrv.Close)
	w.intakeURL = intakeSrv.URL

	// The three-way ledger views: the stub's own log, LiteLLM's spend log (the
	// fake, derived from the stub), and meter's cost API (the real service).
	w.Views = ledger.Views{
		Stub:    w.Model.Log(),
		Spend:   w.LLM,
		Cost:    &costAdapter{svc: w.svc},
		Catalog: catalogKinds(cfg),
	}

	return w
}

// ---- the per-run write-auth credential (D1). Minted once per process; the
// keypair never touches the repo (secrets-scan would catch it).
var (
	waOnce sync.Once
	waCred *harness.WriteAuthCreds
	waErr  error
)

const (
	botUserID    = int64(1000)
	botUsername  = "gonk"
	writeAuthKID = "gonk-e2e-l1"
)

func wa(t *testing.T) *harness.WriteAuthCreds {
	t.Helper()
	waOnce.Do(func() {
		c, err := harness.NewCreds("", "l1-integration")
		if err != nil {
			waErr = err
			return
		}
		waCred, waErr = c.NewWriteAuth(writeAuthKID, "")
	})
	if waErr != nil {
		t.Fatalf("mint write-auth keypair: %v", waErr)
	}
	return waCred
}

// ---------------------------------------------------------------- reconcile / sync

// Reconcile forces exactly one intake pass and blocks until it has completed
// (HB-1), through the real intake HTTP admin surface.
func (w *World) Reconcile(t *testing.T) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.intakeURL+"/admin/reconcile?wait=true", nil)
	if err != nil {
		t.Fatalf("Reconcile: new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Reconcile: status %d: %s", resp.StatusCode, body)
	}
}

// SyncSpend forces one meter spend poll (HB-2), ingesting whatever the fake
// LiteLLM currently reports.
func (w *World) SyncSpend(t *testing.T) {
	t.Helper()
	if err := w.svc.SyncSpend(context.Background()); err != nil {
		t.Fatalf("SyncSpend: %v", err)
	}
}

// Session builds a SyntheticSession bound to this World's CURRENT project and
// meter, authenticated, pointed at the stub model directly (L1: no LiteLLM in
// the request path).
func (w *World) Session(bead, sess, trigger string) *harness.SyntheticSession {
	return w.SessionFor(w.project, bead, sess, trigger)
}

// SessionFor is Session for an explicitly named project.
func (w *World) SessionFor(project, bead, sess, trigger string) *harness.SyntheticSession {
	return &harness.SyntheticSession{
		HTTP:       &http.Client{Timeout: 30 * time.Second, Transport: w.authTrans},
		MeterURL:   w.meterURL,
		ModelURL:   w.modelURL,
		Project:    project,
		Rig:        rigFor(project),
		BeadID:     bead,
		SessionKey: sess,
		Trigger:    trigger,
	}
}

// ---------------------------------------------------------------- helpers

// authRoundTripper injects the meter bearer on every request. The stub model
// ignores the header, so one client can serve both meter and model calls.
type authRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (a *authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return a.base.RoundTrip(r)
}

func rigFor(project string) string { return strings.ReplaceAll(project, "/", "-") }

// catalogKinds projects the operator catalog onto the rung->kind map the ledger
// currency firewall reads.
func catalogKinds(cfg *opercfg.OperatorConfig) map[string]string {
	out := map[string]string{}
	for name, r := range cfg.Catalog {
		if r.Kind == opercfg.KindLocal {
			out[name] = ledger.KindLocal
		} else {
			out[name] = ledger.KindCloud
		}
	}
	return out
}

// ---------------------------------------------------------------- fake LiteLLM

// fakeLLM implements litellm.SpendSource (Since) by DERIVING one spend row per
// successful stub model call, using the verbatim attribution metadata the pack
// stamped and the operator rung catalog to price it. It also implements the
// ledger.SpendSource view (Rows). Admin (virtual keys) is delegated to the
// embedded *litellm.Fake, which the real meter service was constructed with.
//
// At L1 the model calls hit the stub DIRECTLY, so nothing but this derivation
// connects a call to a priced, attributed ledger row -- which is exactly the
// "one uncomfortable substitution" the plan flags: L1 proves the WIRING, L2
// proves LiteLLM actually behaves this way.
type fakeLLM struct {
	admin   *litellm.Fake
	stub    *stubmodel.Server
	catalog map[string]opercfg.RungSpec
	now     func() time.Time

	mu sync.Mutex
	at map[int]time.Time // stub call index -> assigned row timestamp (stable across polls)
}

// Since is litellm.SpendSource: rows at or after t, plus the source clock.
func (f *fakeLLM) Since(_ context.Context, t time.Time) ([]spend.Row, time.Time, error) {
	rows := f.rows()
	var out []spend.Row
	for _, r := range rows {
		if !r.At.Before(t) {
			out = append(out, r)
		}
	}
	return out, f.now(), nil
}

// Rows is the ledger.SpendSource view: every derived row, unfiltered.
func (f *fakeLLM) Rows(_ context.Context) ([]spend.Row, error) { return f.rows(), nil }

func (f *fakeLLM) rows() []spend.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.stub.Log().Calls()
	var out []spend.Row
	for i, c := range calls {
		if c.Status < 200 || c.Status >= 300 {
			continue // a failed model call bills nothing
		}
		if len(c.Metadata) == 0 {
			continue // unattributable: no tags stamped
		}
		tags, err := atags.FromMetadata(c.Metadata)
		if err != nil {
			continue // an invalid tag set never becomes a billable row
		}
		spec, ok := f.catalog[tags.Rung]
		if !ok {
			continue
		}
		at, ok := f.at[i]
		if !ok {
			at = f.now()
			f.at[i] = at
		}
		tokens := int64(c.Usage.PromptTokens + c.Usage.CompletionTokens)
		out = append(out, spend.Row{
			CallID:           fmt.Sprintf("stubcall-%d", i),
			Tags:             tags,
			CostUSD:          spec.PricePerToken() * float64(tokens),
			PromptTokens:     int64(c.Usage.PromptTokens),
			CompletionTokens: int64(c.Usage.CompletionTokens),
			At:               at,
			Synthetic:        spec.Kind == opercfg.KindLocal,
		})
	}
	return out
}

// HasKeyFor reports whether the fake LiteLLM holds a live virtual key for a
// project (alias gonk-<rig>). A broken/disabled project must not.
func (f *fakeLLM) HasKeyFor(project string) bool {
	_, ok := f.admin.Keys["gonk-"+rigFor(project)]
	return ok
}

// ---------------------------------------------------------------- meter handle

type meterHandle struct{ w *World }

// rawProject is the result of a direct PUT to meter: the wire status and body.
type rawProject struct {
	Status int
	Body   meterapi.ProjectResponse
}

// registerRaw PUTs a .gonk.yml to meter over real HTTP and returns the status
// and body. This is the seam the 422-not-400 test reads directly.
func (m *meterHandle) registerRaw(t *testing.T, project string, projectID int64, yml string) rawProject {
	t.Helper()
	req := meterapi.ProjectRequest{
		Project: project, ProjectID: projectID, Rig: rigFor(project), GonkYML: yml,
	}
	var out meterapi.ProjectResponse
	status := m.do(t, http.MethodPut, meterapi.ProjectPath(project), req, &out)
	return rawProject{Status: status, Body: out}
}

// GetProject reads a project's current registration from meter.
func (m *meterHandle) GetProject(t *testing.T, project string) meterapi.ProjectResponse {
	t.Helper()
	var out meterapi.ProjectResponse
	status := m.do(t, http.MethodGet, meterapi.ProjectPath(project), nil, &out)
	if status != http.StatusOK {
		t.Fatalf("meter GET %s: status %d", project, status)
	}
	return out
}

// State returns a project's meter state, or "" if it is not registered.
func (m *meterHandle) State(t *testing.T, project string) meterapi.State {
	t.Helper()
	var out meterapi.ProjectResponse
	status := m.do(t, http.MethodGet, meterapi.ProjectPath(project), nil, &out)
	if status == http.StatusNotFound {
		return ""
	}
	if status != http.StatusOK {
		t.Fatalf("meter GET %s: status %d", project, status)
	}
	return out.State
}

// SetOperatorConfig hot-swaps the operator config AND re-resolves every project
// against it -- the meter's reresolve ticker firing after a config reload. This
// is what lets an instance kill switch reach a project whose OWN config never
// changed.
func (m *meterHandle) SetOperatorConfig(t *testing.T, y string) {
	t.Helper()
	cfg, err := opercfg.Load([]byte(y))
	if err != nil {
		t.Fatalf("SetOperatorConfig: opercfg.Load: %v", err)
	}
	m.w.svc.SetConfig(cfg)
	if err := m.w.svc.Reresolve(context.Background()); err != nil {
		t.Fatalf("SetOperatorConfig: reresolve: %v", err)
	}
}

// RunJanitor expires past-TTL, unsettled reservations (recording an infra-failed
// attempt for each dead session).
func (m *meterHandle) RunJanitor(t *testing.T) {
	t.Helper()
	if err := m.w.svc.Janitor(context.Background()); err != nil {
		t.Fatalf("RunJanitor: %v", err)
	}
}

func (m *meterHandle) do(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("meter %s %s: marshal: %v", method, path, err)
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, m.w.meterURL+path, rdr)
	if err != nil {
		t.Fatalf("meter %s %s: new request: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+m.w.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("meter %s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out) // a 4xx body may not match `out`; the status carries it
	}
	return resp.StatusCode
}

// openReservationCostUSD sums the REAL dollars held by open reservations for a
// project at the current clock -- the persisted-headroom invariant the race test
// checks against the ceiling.
func (w *World) openReservationCostUSD(t *testing.T, project string) float64 {
	t.Helper()
	open, err := w.Store.OpenReservations(context.Background(), project, w.Clock.Now())
	if err != nil {
		t.Fatalf("OpenReservations: %v", err)
	}
	var total float64
	for _, r := range open {
		total += r.CostUSD
	}
	return total
}

// ---------------------------------------------------------------- cost adapter

// costAdapter exposes the real meter service's cost API as a ledger.CostSource.
type costAdapter struct{ svc *service.Service }

func (c *costAdapter) BeadCost(ctx context.Context, beadID string) (meterapi.BeadCostResponse, error) {
	resp, found, err := c.svc.BeadCost(ctx, beadID)
	if err != nil {
		return meterapi.BeadCostResponse{}, err
	}
	if !found {
		return meterapi.BeadCostResponse{}, fmt.Errorf("no attempts recorded for bead %q", beadID)
	}
	return resp, nil
}

func (c *costAdapter) ProjectCost(ctx context.Context, project string) (meterapi.ProjectCostResponse, error) {
	return c.svc.ProjectCost(ctx, project)
}

// projectCost reads meter's windowed project cost (the brick test's real/synthetic split).
func (w *World) projectCost(t *testing.T, project string) meterapi.ProjectCostResponse {
	t.Helper()
	resp, err := w.svc.ProjectCost(context.Background(), project)
	if err != nil {
		t.Fatalf("ProjectCost: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------- stub controller (D1)

// stubController is the grant-gated Gas City supervisor stand-in: it VERIFIES the
// ed25519 X-GC-City-Write grant the real intake dispatcher (pkg/gcapi) mints, so
// L1 exercises the real signing path end to end (D1) rather than just asserting a
// 401. gcapitest.Server does not verify grants; this one does.
type stubController struct {
	srv *httptest.Server
	pub ed25519.PublicKey
	kid string

	mu    sync.Mutex
	pours []controllerPour
}

type controllerPour struct {
	Order string
	Vars  map[string]string
}

func newStubController(t *testing.T, pub ed25519.PublicKey, kid string) *stubController {
	t.Helper()
	c := &stubController{pub: pub, kid: kid}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *stubController) URL() string { return c.srv.URL }

func (c *stubController) pouredNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.pours))
	for i, p := range c.pours {
		out[i] = p.Order
	}
	return out
}

func (c *stubController) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_, order, ok := parseRunPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	// A grant-gated route: no grant at all is a 401 (the no-Signer path); a grant
	// without the CSRF header is a 403; a grant that fails to verify is a 403.
	token := r.Header.Get("X-GC-City-Write")
	if token == "" {
		http.Error(w, "missing X-GC-City-Write grant", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("X-GC-Request") != "true" {
		http.Error(w, "csrf: X-GC-Request missing", http.StatusForbidden)
		return
	}
	if err := c.verifyGrant(token, r.Method, r.URL.Path, r.URL.RawQuery, body); err != nil {
		http.Error(w, "write grant rejected: "+err.Error(), http.StatusForbidden)
		return
	}

	var parsed struct {
		Vars map[string]string `json:"vars"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.pours = append(c.pours, controllerPour{Order: order, Vars: parsed.Vars})
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gcapi.RunResult{Status: "queued", ScopedName: "gonk/" + order, TrackingID: "trk-1"})
}

// grantClaims mirrors pkg/gcapi's signed payload. Field order is not significant
// for VERIFICATION (we verify over the exact received bytes, then read claims).
type grantClaims struct {
	Kid string `json:"kid"`
	Aud string `json:"aud"`
	Req string `json:"req"`
}

// verifyGrant reproduces the server side of the write-auth contract: verify the
// ed25519 signature over the EXACT payload bytes, then confirm the request
// binding (method+path+query+body digest) and the kid/audience. This proves the
// real dispatcher attached a valid, request-bound grant.
func (c *stubController) verifyGrant(token, method, path, rawQuery string, body []byte) error {
	payloadB64, sigB64, ok := strings.Cut(token, ".")
	if !ok {
		return fmt.Errorf("malformed grant token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return fmt.Errorf("payload not base64url: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("sig not base64url: %w", err)
	}
	if !ed25519.Verify(c.pub, payload, sig) {
		return fmt.Errorf("bad signature")
	}
	var g grantClaims
	if err := json.Unmarshal(payload, &g); err != nil {
		return fmt.Errorf("payload not JSON: %w", err)
	}
	if g.Kid != c.kid {
		return fmt.Errorf("unexpected kid %q", g.Kid)
	}
	if g.Aud != "gc-city-write.v2" {
		return fmt.Errorf("unexpected aud %q", g.Aud)
	}
	if want := reqDigest(method, path, rawQuery, body); g.Req != want {
		return fmt.Errorf("request-binding mismatch: grant %q wire %q", g.Req, want)
	}
	return nil
}

// reqDigest reproduces pkg/gcapi.reqDigest so the stub can confirm the grant is
// bound to the exact method/path/query/body on the wire.
func reqDigest(method, path, rawQuery string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	var p strings.Builder
	p.WriteString(method)
	p.WriteByte('\n')
	p.WriteString(path)
	if cq := canonicalizeQuery(rawQuery); cq != "" {
		p.WriteByte('\n')
		p.WriteString(cq)
	}
	p.WriteByte('\n')
	p.WriteString(hex.EncodeToString(bodyHash[:]))
	sum := sha256.Sum256([]byte(p.String()))
	return hex.EncodeToString(sum[:])
}

func canonicalizeQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	return vals.Encode()
}

func parseRunPath(p string) (city, order string, ok bool) {
	rest, found := strings.CutPrefix(p, "/v0/city/")
	if !found {
		return "", "", false
	}
	city, rest, found = strings.Cut(rest, "/order/")
	if !found || city == "" {
		return "", "", false
	}
	order, found = strings.CutSuffix(rest, "/run")
	if !found || order == "" {
		return "", "", false
	}
	return city, order, true
}
