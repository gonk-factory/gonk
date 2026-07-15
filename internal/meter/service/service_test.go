package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/keysink"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// testOperatorYAML is the shared operator config for most tests: instance
// ladder [qwen-local, glm, sonnet], NO instance budget (so
// TestRegisterUnlimitedBudgetOmitsMaxBudget stays true when nothing else sets
// one either), group "agentic" tightens to $50, group "agentic/experiments"
// only enables (so the nested-group fold test can prove the ancestor's
// ceiling survives it).
const testOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local, glm, sonnet]
groups:
  agentic:
    budget: { monthly_cost_usd: 50 }
  agentic/experiments:
    enabled: true
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0,    est_tokens: "50K",  synthetic_usd_per_1m_tokens: 1.0 }
  - { name: glm,        kind: cloud, model: glm-5,           est_cost_usd: 0.40, est_tokens: "200K" }
  - { name: sonnet,     kind: cloud, model: claude-sonnet,   est_cost_usd: 2.00, est_tokens: "100K" }
meter:
  max_spend_staleness: 5m
  reservation_ttl: 60m
  max_clock_skew: 5m
  key_retry_backoff: 5m
  max_infra_retries: 5
`

type fixture struct {
	t     *testing.T
	svc   *Service
	store *store.Memory
	admin *litellm.Fake
	keys  *keysink.Memory
	clock time.Time
}

func newTestServiceWithConfig(t *testing.T, operatorYAML string) *fixture {
	t.Helper()
	cfg, err := opercfg.Load([]byte(operatorYAML))
	if err != nil {
		t.Fatalf("opercfg.Load: %v", err)
	}
	f := &fixture{
		t:     t,
		store: store.NewMemory(),
		admin: litellm.NewFake(),
		keys:  keysink.NewMemory(),
		clock: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC),
	}
	f.svc = New(cfg, f.store, f.admin, f.admin, f.keys, func() time.Time { return f.clock })
	f.admin.Now = f.clock
	return f
}

func newTestService(t *testing.T) *fixture {
	return newTestServiceWithConfig(t, testOperatorYAML)
}

func (f *fixture) advance(d time.Duration) {
	f.clock = f.clock.Add(d)
	f.admin.Now = f.clock
}

// syncOnce runs one spend sync so Ready() becomes true (synced && skewOK).
func (f *fixture) syncOnce() {
	f.t.Helper()
	if err := f.svc.SyncSpend(context.Background()); err != nil {
		f.t.Fatalf("SyncSpend: %v", err)
	}
}

func (f *fixture) register(project, rig, yaml string) meterapi.ProjectResponse {
	f.t.Helper()
	resp, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: project, ProjectID: 1, Rig: rig, GonkYML: yaml,
	})
	if err != nil {
		f.t.Fatalf("Register(%q): %v", project, err)
	}
	if status != 200 {
		f.t.Fatalf("Register(%q) status = %d, resp=%+v", project, status, resp)
	}
	return resp
}

// simpleYAML builds a minimal valid .gonk.yml: triage enabled, the given
// ladder and monthly cost ceiling.
func simpleYAML(ladder string, monthlyCostUSD float64) string {
	return "version: 1\nenabled: true\nactions: { triage: true }\n" +
		"ladder: [" + ladder + "]\n" +
		"budget: { monthly_cost_usd: " + floatStr(monthlyCostUSD) + " }\n"
}

func floatStr(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

const trigger = atags.TriggerIssueTriage

func decideReq(project, bead, session string) meterapi.DecideRequest {
	return meterapi.DecideRequest{Project: project, Rig: "rig", BeadID: bead, SessionKey: session, Trigger: trigger}
}

// ==================================================================
// Registration
// ==================================================================

func TestRegisterProvisionsAKeyAndResolvesConfig(t *testing.T) {
	f := newTestServiceWithConfig(t, `
version: 1
instance:
  enabled: true
  ladder: [qwen-local, glm]
  budget: { monthly_cost_usd: 200 }
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0, est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
  - { name: glm,        kind: cloud, model: glm-5,           est_cost_usd: 0.40, est_tokens: "200K" }
meter: { max_spend_staleness: 5m, reservation_ttl: 60m }
`)
	resp := f.register("group/repo", "group-repo", simpleYAML("qwen-local, glm", 10))

	if resp.State != meterapi.StateActive {
		t.Fatalf("state = %q, want active: %+v", resp.State, resp)
	}
	if resp.Effective == nil || resp.Budget.MonthlyCostUSD == nil || *resp.Budget.MonthlyCostUSD != 10 {
		t.Fatalf("budget = %+v, want the TIGHTER project layer (10), not the instance's 200", resp.Budget)
	}
	k, ok := f.admin.Keys["gonk-group-repo"]
	if !ok {
		t.Fatalf("fake admin has no key aliased gonk-group-repo; keys=%+v", f.admin.Keys)
	}
	if k.Token == "" {
		t.Fatal("provisioned key has no token")
	}
	if f.keys.Token("group/repo") == "" {
		t.Fatal("keysink holds no token for group/repo")
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-") {
		t.Fatalf("ProjectResponse JSON leaks a token: %s", raw)
	}
}

func TestRegisterUnlimitedBudgetOmitsMaxBudget(t *testing.T) {
	f := newTestService(t) // no instance budget, project not in the "agentic" group
	f.register("solo/repo", "solo-repo", "version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\n")

	k, ok := f.admin.Keys["gonk-solo-repo"]
	if !ok {
		t.Fatal("no key provisioned")
	}
	if k.Alias == "" {
		t.Fatal("no alias")
	}
	// Re-derive the spec the way Register built it, by inspecting what EnsureKey
	// actually received is not directly observable via Fake -- assert instead
	// via the registration's resolved budget, which must be all-unlimited, and
	// via a direct call to litellm.MaxBudgetFor with the SAME inputs Register
	// used, mirroring the money-critical assertion: nil, not +Inf, not 0.
	reg, ok, err := f.store.GetRegistration(context.Background(), "solo/repo")
	if err != nil || !ok {
		t.Fatalf("GetRegistration: %v %v", ok, err)
	}
	max := litellm.MaxBudgetFor(reg.Effective.Budget, reg.Effective.Ladder, f.svc.Config().Catalog)
	if max != nil {
		t.Fatalf("MaxBudgetUSD = %v, want nil (unlimited budget must never become +Inf or 0)", *max)
	}
}

func TestRegisterRejectsInvalidGonkYML(t *testing.T) {
	f := newTestService(t)
	resp, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: "not: valid: yaml: at: all: [",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if status != 422 {
		t.Fatalf("status = %d, want 422", status)
	}
	if resp.State != meterapi.StateInvalid {
		t.Fatalf("state = %q, want invalid", resp.State)
	}
	if resp.Error == "" {
		t.Fatal("Error is empty on an invalid config")
	}
	if resp.Effective != nil {
		t.Fatal("Effective must be nil for an invalid config (ADR-002)")
	}
	if resp.Budget.MonthlyCostUSD == nil || *resp.Budget.MonthlyCostUSD != 0 ||
		resp.Budget.MonthlyTokens == nil || *resp.Budget.MonthlyTokens != 0 {
		t.Fatalf("budget = %+v, want ZeroBudget (not nulls, which mean unlimited)", resp.Budget)
	}
	if resp.KeyRef.SecretName != "" {
		t.Fatalf("key_ref = %+v, want empty on an invalid config", resp.KeyRef)
	}
}

func TestRegisterInvalidConfigDELETESAnExistingKey(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("qwen-local", 5))
	if _, ok := f.admin.Keys["gonk-group-repo"]; !ok {
		t.Fatal("setup: key was not provisioned")
	}

	_, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: "not valid [",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != 422 {
		t.Fatalf("status = %d, want 422", status)
	}
	if _, ok := f.admin.Keys["gonk-group-repo"]; ok {
		t.Fatal("the virtual key is still alive after the config went invalid -- a broken config can still spend")
	}
}

func TestRegisterResolvesUsingNESTEDGroupPolicy(t *testing.T) {
	f := newTestService(t)
	resp := f.register("agentic/experiments/spike", "agentic-experiments-spike",
		"version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\nbudget: { monthly_cost_usd: 200 }\n")
	if resp.Budget.MonthlyCostUSD == nil || *resp.Budget.MonthlyCostUSD != 50 {
		t.Fatalf("budget = %+v, want the ANCESTOR group's ceiling (50), not the instance's or the project's 200", resp.Budget)
	}
}

func TestRegisterDisabledProjectProvisionsNoKey(t *testing.T) {
	f := newTestService(t)
	resp := f.register("group/repo", "group-repo", "version: 1\nenabled: false\n")
	if resp.State != meterapi.StateDisabled {
		t.Fatalf("state = %q, want disabled", resp.State)
	}
	if resp.DisabledReason == "" {
		t.Fatal("DisabledReason must be set for a disabled project")
	}
	if _, ok := f.admin.Keys["gonk-group-repo"]; ok {
		t.Fatal("a disabled project must not hold a live credential")
	}
	if resp.Effective == nil {
		t.Fatal("Effective must be non-nil for a disabled (not invalid) project")
	}
}

func TestRegisterRejectsAReorderedLadder(t *testing.T) {
	f := newTestServiceWithConfig(t, `
version: 1
instance: { enabled: true, ladder: [qwen-local, glm, sonnet] }
rungs:
  - { name: qwen-local, kind: local, model: m1, est_cost_usd: 0,    est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
  - { name: glm,        kind: cloud, model: m2, est_cost_usd: 0.40, est_tokens: "200K" }
  - { name: sonnet,     kind: cloud, model: m3, est_cost_usd: 2.00, est_tokens: "100K" }
meter: { enforce_ladder_order: true }
`)
	_, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo",
		GonkYML: "version: 1\nenabled: true\nactions: { triage: true }\nladder: [sonnet, qwen-local]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != 400 && status != 422 {
		t.Fatalf("status = %d, want a rejection (400 or 422)", status)
	}
}

// TestRegisterRejectsABadTimezone deviates from the task sketch's literal
// "400" annotation: see the report's noted concern. A bad IANA timezone in
// the project's OWN .gonk.yml is the same class of failure as a
// schema-invalid or reordered-ladder config (the project's config will not
// resolve), so this implementation routes it through the SAME 422 path,
// consistent with "422 means a successful, idempotent registration of an
// invalid config" and "400 means nothing is recorded" -- recording
// state=invalid and returning 400 would violate that second invariant.
func TestRegisterRejectsABadTimezone(t *testing.T) {
	f := newTestService(t)
	resp, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo",
		GonkYML: "version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\n" +
			"schedule: { quiet_hours: \"22:00-07:00\", timezone: \"Mars/Olympus\" }\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != 422 {
		t.Fatalf("status = %d, want 422 (see the deviation note on this test)", status)
	}
	if resp.State != meterapi.StateInvalid {
		t.Fatalf("state = %q, want invalid", resp.State)
	}
}

func TestKeyProvisioningFailureIsFailClosed(t *testing.T) {
	f := newTestService(t)
	f.admin.AdminErr = errors.New("connection refused")

	resp := f.register("group/repo", "group-repo", simpleYAML("qwen-local, glm", 5))
	f.syncOnce()
	if resp.State != meterapi.StateKeyMissing {
		t.Fatalf("state = %q, want key-missing", resp.State)
	}

	d, extras, err := f.svc.Decide(context.Background(), decideReq("group/repo", "bead-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Defer || d.Reason != rung.ReasonKeyMissing {
		t.Fatalf("decision = %+v, want defer/virtual-key-missing", d)
	}
	if extras.Reservation.ID != "" {
		t.Fatal("a key-missing project must not get a reservation")
	}

	f.admin.AdminErr = nil
	if err := f.svc.ReconcileKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg, _, _ := f.store.GetRegistration(context.Background(), "group/repo")
	if reg.State != store.StateActive {
		t.Fatalf("state after reconcile = %q, want active", reg.State)
	}

	d2, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "bead-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != rung.Run {
		t.Fatalf("decision after reconcile = %+v, want run", d2)
	}
}

func TestRegisterDeleteIsIdempotentOnAnUnknownProject(t *testing.T) {
	f := newTestService(t)
	if err := f.svc.Delete(context.Background(), "never/registered"); err != nil {
		t.Fatalf("Delete of an unknown project = %v, want nil (204, not 404)", err)
	}
}

// ==================================================================
// Decide: idempotency (FIX-A) -- THE money-safety core of this task.
// ==================================================================

// TestDecideIsIdempotentOnOpenReservation is the exact bug FIX-A exists to
// prevent: intake (Gate 1) and the pack (Gate 2) both call /decide for the
// same work. A non-idempotent /decide double-charges headroom.
func TestDecideIsIdempotentOnOpenReservation(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("glm", 1.00))
	f.syncOnce()

	req := decideReq("group/repo", "gk-1a2b", "sess-9")
	d1, extras1, err := f.svc.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Kind != rung.Run {
		t.Fatalf("first decide = %+v, want run", d1)
	}
	if extras1.Reservation.ID == "" {
		t.Fatal("first decide minted no reservation")
	}

	d2, extras2, err := f.svc.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != rung.Run || d2.Attempt != d1.Attempt || d2.Rung != d1.Rung {
		t.Fatalf("second decide = %+v, want the SAME run decision as %+v", d2, d1)
	}
	if extras2.Reservation.ID != extras1.Reservation.ID {
		t.Fatalf("second decide minted a DIFFERENT reservation: %q vs %q -- this is FIX-A's bug",
			extras2.Reservation.ID, extras1.Reservation.ID)
	}

	open, err := f.store.OpenReservations(context.Background(), "group/repo", f.clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("store holds %d open reservations, want exactly 1 (non-vacuous: two /decide calls minted only one)", len(open))
	}
	if open[0].CostUSD != 0.40 {
		t.Fatalf("held cost = %v, want 0.40 (double-reservation would hold 0.80)", open[0].CostUSD)
	}
}

// TestDecideIsIdempotentAcrossDifferentSessionKeys proves the idempotency key
// is (project, bead, session) as the plan specifies, not bead alone: a
// DIFFERENT session for the same bead is a genuinely different attempt and
// must not be folded into the first one (that would be under-reserving, the
// opposite failure).
func TestDecideIsIdempotentOnlyForTheSameSessionKey(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("glm", 1.00))
	f.syncOnce()

	d1, e1, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-A"))
	if err != nil || d1.Kind != rung.Run {
		t.Fatalf("first decide = %+v %v", d1, err)
	}
	d2, e2, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-B"))
	if err != nil {
		t.Fatal(err)
	}
	// Attempt 1 is already reserved for the bead; a second, DIFFERENT session
	// asking about the same bead is not the idempotent-replay case (Prior is
	// unchanged, so rung.Decide computes the SAME attempt number again) --
	// meter must not silently merge it into session A's reservation.
	if d2.Kind == rung.Run && e2.Reservation.ID == e1.Reservation.ID {
		t.Fatal("a different session_key was folded into another session's reservation")
	}
}

// TestDecideIsIdempotentOnlyForTheSameBeadID is the BEAD half of the
// idempotency key (the session half is above): same session_key, DIFFERENT
// bead_id is genuinely different work and must get its OWN reservation, never
// be deduped into the first. (Both halves matter; keying on session alone would
// merge two unrelated beads that happened to share a session identifier.)
func TestDecideIsIdempotentOnlyForTheSameBeadID(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("glm", 5)) // headroom for many
	f.syncOnce()

	d1, e1, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-A", "sess-shared"))
	if err != nil || d1.Kind != rung.Run || e1.Reservation.ID == "" {
		t.Fatalf("first decide = %+v %+v %v", d1, e1, err)
	}
	d2, e2, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-B", "sess-shared"))
	if err != nil || d2.Kind != rung.Run || e2.Reservation.ID == "" {
		t.Fatalf("second decide (different bead) = %+v %+v %v", d2, e2, err)
	}
	if e2.Reservation.ID == e1.Reservation.ID {
		t.Fatal("a different bead_id was folded into another bead's reservation (idempotency must key on bead_id too)")
	}
	open, err := f.store.OpenReservations(context.Background(), "group/repo", f.clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("open reservations = %d, want 2 (two distinct beads, same session)", len(open))
	}
}

// TestDecideNeverTrustsACallerSuppliedAttempt confirms the attempt is derived
// SOLELY from meter's own store (recorded prior attempts): meterapi.DecideRequest
// carries no attempt field at all (Decision 2), and repeating /decide without
// any recorded outcome never advances the rung index on its own.
func TestDecideNeverTrustsACallerSuppliedAttempt(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("qwen-local, glm", 1.00))
	f.syncOnce()

	// Calling /decide many times for the SAME bead+session (no outcome
	// reported in between) must keep returning the identical attempt/rung --
	// there is no field on DecideRequest an attacker could set to skip ahead.
	var first rung.Decision
	for i := 0; i < 5; i++ {
		d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = d
			continue
		}
		if d.Attempt != first.Attempt || d.Rung != first.Rung {
			t.Fatalf("call %d: decision drifted to %+v from %+v with no outcome reported", i, d, first)
		}
	}
	if first.Attempt != 1 || first.Rung != "qwen-local" {
		t.Fatalf("first decision = %+v, want attempt 1 at qwen-local (the cheapest rung)", first)
	}
}

// ==================================================================
// Decide: per-project serialization under real concurrency (-race).
// ==================================================================

// TestDecideIsSerializedPerProject is THE test that proves the ceiling is not
// raceable. Fixture matters: ladder is [glm] ONLY, so every fresh bead
// targets a CLOUD rung (a local-only ladder would skip the cost gate
// entirely and the race would be invisible). Ceiling $1.00, glm $0.40/attempt
// -> headroom for exactly 2.
func TestDecideIsSerializedPerProject(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("glm", 1.00))
	f.syncOnce()

	const n = 20
	var wg sync.WaitGroup
	results := make([]rung.Decision, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", beadName(i), sessName(i)))
			if err != nil {
				t.Errorf("goroutine %d: Decide: %v", i, err)
				return
			}
			results[i] = d
		}(i)
	}
	wg.Wait()

	runCount, deferCount := 0, 0
	for _, d := range results {
		switch d.Kind {
		case rung.Run:
			runCount++
		case rung.Defer:
			deferCount++
			if d.Reason != rung.ReasonMonthlyCostExhausted {
				t.Errorf("defer reason = %q, want monthly-cost-exhausted", d.Reason)
			}
		default:
			t.Errorf("unexpected decision kind %q", d.Kind)
		}
	}
	if runCount != 2 {
		t.Fatalf("runCount = %d, want exactly 2 (headroom for 2 at $0.40 against a $1.00 ceiling)", runCount)
	}
	if deferCount != n-2 {
		t.Fatalf("deferCount = %d, want %d", deferCount, n-2)
	}

	open, err := f.store.OpenReservations(context.Background(), "group/repo", f.clock)
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, r := range open {
		sum += r.CostUSD
	}
	if len(open) != 2 || sum < 0.799 || sum > 0.801 {
		t.Fatalf("open reservations = %d totaling $%.4f, want 2 totaling $0.80", len(open), sum)
	}
}

// TestDecideIsSerializedPerProjectOnTokensToo is the same race proof against
// monthly_tokens: ladder [qwen-local] (a free local rung, so the cost gate
// never fires), est_tokens 200K, monthly_tokens 500K -> headroom for exactly
// 2. The LiteLLM backstop for tokens is only a LOOSE one (Decision 9), so if
// the reservation races, nothing catches the overshoot in time.
func TestDecideIsSerializedPerProjectOnTokensToo(t *testing.T) {
	// A dedicated operator config: qwen-local's est_tokens must be EXACTLY
	// 200K for the fixture's math (500K ceiling -> headroom for exactly 2) to
	// be non-vacuous; the shared testOperatorYAML's qwen-local carries a
	// different estimate for other tests' purposes.
	f := newTestServiceWithConfig(t, `
version: 1
instance:
  enabled: true
  ladder: [qwen-local]
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0, est_tokens: "200K", synthetic_usd_per_1m_tokens: 1.0 }
meter:
  max_spend_staleness: 5m
  reservation_ttl: 60m
  max_clock_skew: 5m
`)
	f.register("group/repo", "group-repo",
		"version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\n"+
			"budget: { monthly_tokens: \"500K\" }\n")
	f.syncOnce()

	const n = 20
	var wg sync.WaitGroup
	results := make([]rung.Decision, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", beadName(i), sessName(i)))
			if err != nil {
				t.Errorf("goroutine %d: Decide: %v", i, err)
				return
			}
			results[i] = d
		}(i)
	}
	wg.Wait()

	runCount := 0
	for _, d := range results {
		if d.Kind == rung.Run {
			runCount++
		} else if d.Kind == rung.Defer && d.Reason != rung.ReasonMonthlyTokensExhausted {
			t.Errorf("defer reason = %q, want monthly-tokens-exhausted", d.Reason)
		}
	}
	if runCount != 2 {
		t.Fatalf("runCount = %d, want exactly 2 (headroom for 2 at 200K tokens against a 500K ceiling)", runCount)
	}
}

func beadName(i int) string { return "bead-" + strconv.Itoa(i) }
func sessName(i int) string { return "sess-" + strconv.Itoa(i) }

// ==================================================================
// Decide: deny never mints an identity or a credential pointer.
// ==================================================================

func TestDeniedActionNeverMintsTagsOrReturnsAKeyRef(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo",
		"version: 1\nenabled: true\nactions: { triage: false }\nladder: [qwen-local]\n")

	d, extras, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonActionNotAllowed {
		t.Fatalf("decision = %+v, want deny/action-not-allowed", d)
	}
	if len(extras.Metadata) != 0 {
		t.Fatalf("metadata = %+v, want empty on a deny", extras.Metadata)
	}
	if extras.KeyRef != (store.KeyRef{}) {
		t.Fatalf("key_ref = %+v, want zero on a deny", extras.KeyRef)
	}
	if extras.Reservation.ID != "" {
		t.Fatal("a deny must not carry a reservation")
	}
}

func TestDecideDeniesAnUnregisteredProject(t *testing.T) {
	f := newTestService(t)
	d, extras, err := f.svc.Decide(context.Background(), decideReq("never/registered", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonNotRegistered {
		t.Fatalf("decision = %+v, want deny/project-not-registered", d)
	}
	if extras.Reservation.ID != "" || extras.KeyRef != (store.KeyRef{}) {
		t.Fatal("an unregistered project must not get a reservation or a key_ref")
	}
}

// ==================================================================
// Decide: fail closed on a store error (no run, no reservation).
// ==================================================================

type erroringStore struct {
	*store.Memory
	failGetRegistration bool
}

func (e *erroringStore) GetRegistration(ctx context.Context, project string) (store.Registration, bool, error) {
	if e.failGetRegistration {
		return store.Registration{}, false, errors.New("injected store failure")
	}
	return e.Memory.GetRegistration(ctx, project)
}

func TestDecideFailsClosedOnStoreError(t *testing.T) {
	mem := store.NewMemory()
	es := &erroringStore{Memory: mem}
	admin := litellm.NewFake()
	ks := keysink.NewMemory()
	cfg, err := opercfg.Load([]byte(testOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	svc := New(cfg, es, admin, admin, ks, func() time.Time { return now })

	resp, status, err := svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: simpleYAML("qwen-local", 5),
	})
	if err != nil || status != 200 {
		t.Fatalf("setup Register: %+v %d %v", resp, status, err)
	}

	es.failGetRegistration = true
	d, extras, err := svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err == nil {
		t.Fatalf("Decide with a failing store = (%+v, %+v, nil), want a non-nil error (fail closed)", d, extras)
	}
	if extras.Reservation.ID != "" {
		t.Fatal("a store error must not leave a reservation behind")
	}
	open, _ := mem.OpenReservations(context.Background(), "group/repo", now)
	if len(open) != 0 {
		t.Fatalf("open reservations = %d, want 0 after a fail-closed store error", len(open))
	}
}

// notFitsStore forces ReserveIfFits to report a lost race (Fits=false, no
// error), so a test can exercise the service's !fits branch deterministically
// without staging a real budget race. This is the branch that becomes the SOLE
// guard against an unbacked run if the in-process mutex is ever removed, so it
// must be tested directly.
type notFitsStore struct {
	*store.Memory
}

func (n *notFitsStore) ReserveIfFits(_ context.Context, _ string, _ budget.Budget, _ budget.Spend, _ store.Reservation) (store.ReserveResult, error) {
	return store.ReserveResult{}, nil // Fits=false, no error: a lost race
}

// TestDecideDefersWhenReserveDoesNotFit: when rung.Decide says run but the
// atomic store reserve does NOT fit (a concurrent session took the last of the
// headroom between the snapshot and the write), the decision MUST become a
// defer -- never a run carrying a reservation the store refused. This is the
// race-loser branch, and it is what protects the ceiling when the fast-path
// snapshot is stale.
func TestDecideDefersWhenReserveDoesNotFit(t *testing.T) {
	mem := store.NewMemory()
	ns := &notFitsStore{Memory: mem}
	cfg, err := opercfg.Load([]byte(testOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	admin := litellm.NewFake()
	admin.Now = now
	svc := New(cfg, ns, admin, admin, keysink.NewMemory(), func() time.Time { return now })

	if _, status, err := svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: simpleYAML("glm", 5),
	}); err != nil || status != 200 {
		t.Fatalf("setup register: %d %v", status, err)
	}
	if err := svc.SyncSpend(context.Background()); err != nil {
		t.Fatal(err)
	}

	d, extras, err := svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatalf("Decide = %v, want a defer with no error", err)
	}
	if d.Kind != rung.Defer || d.Reason != rung.ReasonMonthlyCostExhausted {
		t.Fatalf("decision = %+v, want defer/monthly-cost-exhausted (a lost reserve must NOT become a run)", d)
	}
	if extras.Reservation.ID != "" || len(extras.Metadata) != 0 || extras.KeyRef != (store.KeyRef{}) {
		t.Fatalf("a lost-reserve defer leaked extras: %+v", extras)
	}
}

// TestDecideMintFailureReleasesTheReservation: a post-reservation tagmint.Mint
// failure (a hostile bead_id/session_key/rig/trigger) must RELEASE the hold the
// reserve just took -- the session will never start, so its budget must not
// stay reserved until the TTL. The cleanup path (Settle into the past) is
// otherwise untested.
func TestDecideMintFailureReleasesTheReservation(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("glm", 5))
	f.syncOnce()

	// A hostile rig (embedded newline) passes registration -- which validates
	// the PROJECT path and the .gonk.yml, not the decide-time rig -- but trips
	// tagmint.Mint's charset gate AFTER ReserveIfFits has written the hold.
	req := meterapi.DecideRequest{
		Project: "group/repo", Rig: "bad\nrig", BeadID: "gk-1", SessionKey: "sess-1", Trigger: trigger,
	}
	d, extras, err := f.svc.Decide(context.Background(), req)
	if err == nil {
		t.Fatalf("Decide with a hostile rig = (%+v, %+v, nil), want a tagmint failure", d, extras)
	}
	if extras.Reservation.ID != "" {
		t.Fatal("a failed mint returned a reservation")
	}
	open, oerr := f.store.OpenReservations(context.Background(), "group/repo", f.clock)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if len(open) != 0 {
		t.Fatalf("open reservations after a mint failure = %+v, want none (the hold must be released, not left to age out over the TTL)", open)
	}
}

// ==================================================================
// Outcome
// ==================================================================

func TestOutcomeMustMatchAReservation(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("qwen-local, glm", 5))
	f.syncOnce()

	_, err := f.svc.Outcome(context.Background(), meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: 1,
		Rung: "qwen-local", ReservationID: "rsv-does-not-exist", Outcome: meterapi.OutcomeSuccess,
	})
	if !errors.Is(err, ErrUnknownReservation) {
		t.Fatalf("Outcome with an unknown reservation_id = %v, want ErrUnknownReservation", err)
	}
	if got, _ := f.store.Attempts(context.Background(), "group/repo", "gk-1"); len(got) != 0 {
		t.Fatalf("an unknown reservation recorded an attempt: %+v", got)
	}

	d, extras, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil || d.Kind != rung.Run {
		t.Fatalf("setup decide: %+v %v", d, err)
	}
	req := meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: d.Attempt,
		Rung: d.Rung, ReservationID: extras.Reservation.ID, Outcome: meterapi.OutcomeGateFailed,
	}
	if _, err := f.svc.Outcome(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Outcome(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	attempts, _ := f.store.Attempts(context.Background(), "group/repo", "gk-1")
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v, want exactly 1 (a replayed outcome is a free escalation otherwise)", attempts)
	}
	if rung.Escalations(attempts) != 1 {
		t.Fatalf("escalations = %d, want 1", rung.Escalations(attempts))
	}
}

func TestLateOutcomeSupersedesTheJanitorsInfraFailure(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("qwen-local, glm", 5))
	f.syncOnce()

	d, extras, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil || d.Kind != rung.Run {
		t.Fatalf("setup decide: %+v %v", d, err)
	}

	f.advance(2 * time.Hour) // past reservation_ttl (60m)
	if err := f.svc.Janitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, _ := f.store.Attempts(context.Background(), "group/repo", "gk-1")
	if len(attempts) != 1 || attempts[0].Outcome != rung.OutcomeInfraFailed {
		t.Fatalf("after the janitor tick, attempts = %+v, want one infra-failed", attempts)
	}

	// The session finally reports its real outcome for the SAME reservation.
	if _, err := f.svc.Outcome(context.Background(), meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: d.Attempt,
		Rung: d.Rung, ReservationID: extras.Reservation.ID, Outcome: meterapi.OutcomeGateFailed,
	}); err != nil {
		t.Fatal(err)
	}

	attempts, _ = f.store.Attempts(context.Background(), "group/repo", "gk-1")
	if len(attempts) != 1 || attempts[0].Outcome != rung.OutcomeGateFailed {
		t.Fatalf("attempts = %+v, want ONE attempt, gate-failed (the late real outcome supersedes the janitor's guess)", attempts)
	}

	f.syncOnce() // re-establish freshness after advancing the clock 2h past max_spend_staleness

	d2, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-2"))
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != rung.Run || d2.Rung != "glm" {
		t.Fatalf("next decide = %+v, want a run at glm (the escalation earned by the late gate-failed)", d2)
	}
}

func TestDecideReservesAndOutcomeSettlesWithoutFreeingTheBudget(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("glm", 5))
	f.syncOnce()

	d, extras, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil || d.Kind != rung.Run {
		t.Fatalf("setup decide: %+v %v", d, err)
	}
	if _, err := f.svc.Outcome(context.Background(), meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: d.Attempt,
		Rung: d.Rung, ReservationID: extras.Reservation.ID, Outcome: meterapi.OutcomeSuccess,
	}); err != nil {
		t.Fatal(err)
	}

	open, _ := f.store.OpenReservations(context.Background(), "group/repo", f.clock)
	if len(open) != 1 || open[0].CostUSD != 0.40 {
		t.Fatalf("open reservations right after outcome(success) = %+v, want the $0.40 hold STILL held", open)
	}

	f.advance(6 * time.Minute) // past max_spend_staleness (5m)
	open, _ = f.store.OpenReservations(context.Background(), "group/repo", f.clock)
	if len(open) != 0 {
		t.Fatalf("open reservations after max_spend_staleness = %+v, want none (the hold expired)", open)
	}
}

func TestSettledReservationIsNotJanitored(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("glm", 5))
	f.syncOnce()

	d, extras, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil || d.Kind != rung.Run {
		t.Fatalf("setup decide: %+v %v", d, err)
	}
	if _, err := f.svc.Outcome(context.Background(), meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: d.Attempt,
		Rung: d.Rung, ReservationID: extras.Reservation.ID, Outcome: meterapi.OutcomeSuccess,
	}); err != nil {
		t.Fatal(err)
	}

	f.advance(2 * time.Hour) // past reservation_ttl
	if err := f.svc.Janitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, _ := f.store.Attempts(context.Background(), "group/repo", "gk-1")
	if len(attempts) != 1 || attempts[0].Outcome != rung.OutcomeSuccess {
		t.Fatalf("attempts = %+v, want the ORIGINAL success only -- the janitor must not touch a settled reservation", attempts)
	}
}

func TestAnOutcomeCannotBeFlippedAfterTheFact(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("qwen-local, glm", 5))
	f.syncOnce()

	d, extras, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil || d.Kind != rung.Run {
		t.Fatalf("setup decide: %+v %v", d, err)
	}
	req := meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: d.Attempt,
		Rung: d.Rung, ReservationID: extras.Reservation.ID,
	}
	req.Outcome = meterapi.OutcomeSuccess
	if _, err := f.svc.Outcome(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Outcome = meterapi.OutcomeGateFailed
	if _, err := f.svc.Outcome(context.Background(), req); err != nil {
		t.Fatal(err) // rejected as a silent no-op, not an error -- see service.go's Outcome doc
	}

	attempts, _ := f.store.Attempts(context.Background(), "group/repo", "gk-1")
	if len(attempts) != 1 || attempts[0].Outcome != rung.OutcomeSuccess {
		t.Fatalf("attempts = %+v, want the outcome STILL success (unflippable)", attempts)
	}
	if rung.Escalations(attempts) != 0 {
		t.Fatal("the bead earned an escalation it must not have")
	}
}

func TestExpiredReservationBecomesAnInfraFailure(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("qwen-local, glm", 5))
	f.syncOnce()

	d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil || d.Kind != rung.Run || d.Rung != "qwen-local" {
		t.Fatalf("setup decide: %+v %v", d, err)
	}

	f.advance(2 * time.Hour) // past reservation_ttl; no outcome ever reported
	if err := f.svc.Janitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	open, _ := f.store.OpenReservations(context.Background(), "group/repo", f.clock)
	if len(open) != 0 {
		t.Fatalf("open reservations after expiry = %+v, want none", open)
	}
	attempts, _ := f.store.Attempts(context.Background(), "group/repo", "gk-1")
	if len(attempts) != 1 || attempts[0].Outcome != rung.OutcomeInfraFailed {
		t.Fatalf("attempts = %+v, want one infra-failed", attempts)
	}

	f.syncOnce() // re-establish freshness after advancing the clock 2h past max_spend_staleness
	d2, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-2"))
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != rung.Run || d2.Rung != "qwen-local" {
		t.Fatalf("next decide = %+v, want qwen-local again (a dead pod must never buy an escalation)", d2)
	}
}

// ==================================================================
// Operator config hot-reload (ADR-002's instance kill switch)
// ==================================================================

func TestOperatorConfigChangeReResolvesRegisteredProjects(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("qwen-local", 5))

	reg, ok, err := f.store.GetRegistration(context.Background(), "group/repo")
	if err != nil || !ok || reg.State != store.StateActive {
		t.Fatalf("setup: registration = %+v %v %v", reg, ok, err)
	}

	newCfg := *f.svc.Config()
	disabled := false
	newCfg.Instance.Enabled = &disabled
	f.svc.SetConfig(&newCfg)
	if err := f.svc.Reresolve(context.Background()); err != nil {
		t.Fatal(err)
	}

	reg, ok, err = f.store.GetRegistration(context.Background(), "group/repo")
	if err != nil || !ok {
		t.Fatalf("registration disappeared: %v %v", ok, err)
	}
	if reg.State != store.StateDisabled {
		t.Fatalf("state = %q, want disabled", reg.State)
	}
	if reg.Effective.DisabledReason != "disabled by instance policy" {
		t.Fatalf("disabled reason = %q", reg.Effective.DisabledReason)
	}

	d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonDisabled {
		t.Fatalf("decide after the kill switch = %+v, want deny/disabled", d)
	}
}

// ==================================================================
// Spend sync, clock skew, staleness, cold start
// ==================================================================

func TestSpendSyncDedupesAndAdvancesTheWindow(t *testing.T) {
	f := newTestService(t)
	f.register("group/repo", "group-repo", simpleYAML("glm", 0.50))

	row := func(id string, at time.Time) {
		f.admin.AddRows(spendRow(id, "group/repo", 0.10, at))
	}
	row("c1", f.clock.Add(-time.Minute))
	f.syncOnce()
	row("c1", f.clock.Add(-time.Minute)) // same CallID again: overlap is expected
	row("c2", f.clock.Add(-30*time.Second))
	f.syncOnce()

	rows, err := f.store.SpendRows(context.Background(), "group/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("stored rows = %d, want 2 (c1 must not be double-counted)", len(rows))
	}

	// Exhaust the cost ceiling so the project is deferring, then roll the month.
	d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Defer || d.Reason != rung.ReasonMonthlyCostExhausted {
		t.Fatalf("setup: decide = %+v, want defer/monthly-cost-exhausted ($0.20 observed + $0.40 wanted > $0.50)", d)
	}

	before, _ := f.store.Window(context.Background())

	f.advance(31 * 24 * time.Hour) // comfortably past month end
	f.syncOnce()

	after, _ := f.store.Window(context.Background())
	if !after.Start.After(before.Start) {
		t.Fatalf("window did not roll: before=%+v after=%+v", before, after)
	}

	d2, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-2", "sess-2"))
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != rung.Run {
		t.Fatalf("after the window rolled, decide = %+v, want run (spend reset)", d2)
	}

	// Now roll the clock BACKWARDS a month: the window must not move.
	rolled, _ := f.store.Window(context.Background())
	f.advance(-31 * 24 * time.Hour)
	f.syncOnce()
	stillRolled, _ := f.store.Window(context.Background())
	if !stillRolled.Start.Equal(rolled.Start) {
		t.Fatalf("window moved backwards: was %+v, now %+v", rolled, stillRolled)
	}
}

func TestClockSkewFailsReadinessAndDefersBudgetedProjects(t *testing.T) {
	f := newTestService(t)
	f.register("budgeted/repo", "budgeted-repo", simpleYAML("glm", 5))
	f.register("unlimited/repo", "unlimited-repo",
		"version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\n")

	f.admin.ClockOffset = 20 * time.Minute // > max_clock_skew (5m)
	f.syncOnce()

	if f.svc.Ready() {
		t.Fatal("Ready() = true with a 20m clock skew, want false")
	}

	d, _, err := f.svc.Decide(context.Background(), decideReq("budgeted/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Defer || d.Reason != rung.ReasonSpendStale {
		t.Fatalf("budgeted project during a clock skew = %+v, want defer/spend-data-stale", d)
	}

	// An all-unlimited project has nothing to protect and correctly keeps running.
	d2, _, err := f.svc.Decide(context.Background(), decideReq("unlimited/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != rung.Run {
		t.Fatalf("unlimited project during a clock skew = %+v, want run", d2)
	}
}

func TestSpendSourceOutageMakesSpendStale(t *testing.T) {
	f := newTestService(t)
	f.register("budgeted/repo", "budgeted-repo", simpleYAML("glm", 5))
	f.register("unlimited/repo", "unlimited-repo",
		"version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\n")
	f.syncOnce() // establish a clean synced baseline

	f.admin.SpendErr = errors.New("litellm unreachable")
	f.advance(6 * time.Minute) // past max_spend_staleness (5m)
	if err := f.svc.SyncSpend(context.Background()); err == nil {
		t.Fatal("SyncSpend during a spend-source outage returned nil, want an error")
	}

	d, _, err := f.svc.Decide(context.Background(), decideReq("budgeted/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Defer || d.Reason != rung.ReasonSpendStale {
		t.Fatalf("budgeted project during a spend outage = %+v, want defer/spend-data-stale", d)
	}

	d2, _, err := f.svc.Decide(context.Background(), decideReq("unlimited/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != rung.Run {
		t.Fatalf("unlimited project during a spend outage = %+v, want run", d2)
	}
}

func TestColdStartDefersUntilTheFirstSync(t *testing.T) {
	f := newTestService(t)
	f.register("budgeted/repo", "budgeted-repo", simpleYAML("glm", 5))

	if f.svc.Ready() {
		t.Fatal("a brand-new service reports Ready() = true before any sync")
	}
	d, _, err := f.svc.Decide(context.Background(), decideReq("budgeted/repo", "gk-1", "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Defer || d.Reason != rung.ReasonSpendStale {
		t.Fatalf("cold-start decide = %+v, want defer/spend-data-stale", d)
	}
}

// spendRow builds a minimal, correctly-tagged spend row for tests.
func spendRow(callID, project string, costUSD float64, at time.Time) spend.Row {
	return spend.Row{
		CallID:  callID,
		CostUSD: costUSD,
		At:      at,
		Tags: atags.Tags{
			Project: project, Rig: "rig", BeadID: "gk-1", SessionKey: "sess-1",
			Rung: "glm", Attempt: 1, Trigger: atags.TriggerIssueTriage,
		},
	}
}
