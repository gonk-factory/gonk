package rung

import (
	"testing"
	"time"

	// Without tzdata, time.LoadLocation("America/New_York") fails in a scratch
	// container AND in any CI image with no zoneinfo -- and the quiet-hours case
	// would fail for a reason that has nothing to do with the policy.
	_ "time/tzdata"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func b(v bool) *bool                                    { return &v }
func f(v float64) *float64                              { return &v }
func tq(v gonkcfg.TokenQuantity) *gonkcfg.TokenQuantity { return &v }

// catalog: qwen-local costs no REAL money (but carries a synthetic price, so
// LiteLLM's USD door still closes on it -- Decision 9); glm is $0.40/attempt and
// sonnet $1.20/attempt of real money.
func catalog() map[string]opercfg.RungSpec {
	return map[string]opercfg.RungSpec{
		"qwen-local": {Name: "qwen-local", Kind: opercfg.KindLocal, Model: "qwen3-coder-30b",
			EstCostUSD: 0, EstTokens: 200_000, SyntheticUSDPer1MTokens: 0.20},
		"glm":    {Name: "glm", Kind: opercfg.KindCloud, Model: "glm-5", EstCostUSD: 0.40, EstTokens: 200_000},
		"sonnet": {Name: "sonnet", Kind: opercfg.KindCloud, Model: "claude-sonnet", EstCostUSD: 1.20, EstTokens: 200_000},
	}
}

// effective builds an Effective the ONLY legitimate way: through Resolve.
// instBudget/projBudget are deliberately different so a resolver that ignored
// a layer would be caught here rather than in gonkcfg's own tests.
func effective(t *testing.T, instance, group gonkcfg.Policy, project gonkcfg.Policy) gonkcfg.Effective {
	t.Helper()
	return gonkcfg.Resolve(instance, group, gonkcfg.ProjectConfig{Version: 1, Policy: project})
}

func instancePolicy() gonkcfg.Policy {
	return gonkcfg.Policy{
		Enabled: b(true),
		Ladder:  []string{"qwen-local", "glm", "sonnet"},
		Budget:  gonkcfg.BudgetPolicy{MonthlyCostUSD: f(200), MonthlyTokens: tq(5_000_000_000)},
	}
}

func projectPolicy() gonkcfg.Policy {
	return gonkcfg.Policy{
		Enabled: b(true),
		Actions: gonkcfg.ActionsPolicy{Triage: b(true)},
		Ladder:  []string{"qwen-local", "glm"},
		Budget: gonkcfg.BudgetPolicy{
			MonthlyCostUSD: f(10),
			MonthlyTokens:  tq(50_000_000),
			PerTaskTokens:  tq(2_000_000),
		},
	}
}

type decideCase struct {
	name string
	in   func(t *testing.T) Input
	want Decision
}

func baseInput(t *testing.T) Input {
	t.Helper()
	return Input{
		Effective:       effective(t, instancePolicy(), gonkcfg.Policy{}, projectPolicy()),
		Catalog:         catalog(),
		Registered:      true,
		KeyReady:        true,
		Trigger:         atags.TriggerIssueTriage,
		Prior:           nil,
		Spend:           budget.Spend{},
		SpendAsOf:       at("2026-07-13T09:59:00Z"),
		Now:             at("2026-07-13T10:00:00Z"),
		Window:          spend.MonthWindow(at("2026-07-13T10:00:00Z")),
		MaxSpendStale:   5 * time.Minute,
		MaxInfraRetries: 5,
		// The cost-class gate (Phase 2.3) denies a cloud rung unless an allowance
		// is granted. The base case grants it, so every pre-existing cloud
		// escalation/defer row keeps its expected outcome; the gate's OWN behaviour
		// is proven by the dedicated tests below, which set this explicitly.
		CloudAllowed: true,
	}
}

func decideCases() []decideCase {
	return []decideCase{
		{
			name: "attempt 1 starts at the cheapest allowed rung",
			in:   baseInput,
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			name: "a gate failure escalates one rung",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				return in
			},
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 2},
		},
		{
			name: "an infra failure retries the SAME rung and does not escalate",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed}}
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 2},
		},
		{
			name: "infra failures never escalate, however many there are",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 3, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 4},
		},
		{
			name: "infra failures are capped, then deny",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.MaxInfraRetries = 3
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 3, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
				}
				return in
			},
			want: Decision{Kind: Deny, Attempt: 4, Reason: ReasonInfraRetriesExhausted},
		},
		{
			name: "a mixed history escalates only on the gate failures",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeGateFailed},
					{Attempt: 3, Rung: "glm", Outcome: OutcomeInfraFailed},
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 4},
		},
		{
			name: "ladder exhausted -> deny, never an infinite defer",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed},
					{Attempt: 2, Rung: "glm", Outcome: OutcomeGateFailed},
				}
				return in
			},
			want: Decision{Kind: Deny, Attempt: 3, Reason: ReasonLadderExhausted},
		},
		{
			name: "the PROJECT's tighter cost ceiling binds: $10 - $9.80 spent < $0.40 for glm",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{CostUSD: 9.80, Tokens: 3_000_000, TaskTokens: 200_000}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			name: "the INSTANCE's tighter token ceiling binds even though the project asked for more",
			in: func(t *testing.T) Input {
				inst := instancePolicy()
				inst.Budget.MonthlyTokens = tq(4_000_000) // tighter than the project's 50M
				proj := projectPolicy()
				in := baseInput(t)
				in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
				in.Spend = budget.Spend{Tokens: 3_900_000, TaskTokens: 100_000}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonMonthlyTokensExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			name: "a GROUP ceiling binds too",
			in: func(t *testing.T) Input {
				grp := gonkcfg.Policy{Budget: gonkcfg.BudgetPolicy{MonthlyCostUSD: f(1)}}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), grp, projectPolicy())
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{CostUSD: 0.80}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			// $10 ceiling, $9.00 billed -- $1.00 left, which fits glm's $0.40.
			// But two sessions are ALREADY committed to $0.40 each and their
			// spend rows have not landed. Net headroom is $0.20, so this one
			// must wait. Without the reservation term it runs, and all three
			// sessions overshoot together.
			name: "open reservations count against the cost ceiling (concurrency + spend-log lag)",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{CostUSD: 9.00, ReservedCostUSD: 0.80}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			// The SAME hole on the token side. Decision 9 gives tokens a hard
			// door (LiteLLM's USD ceiling, fed by synthetic prices), but that door
			// is a LOOSE backstop -- this reservation is the tight one. 50M ceiling,
			// 49.7M billed, 200K reserved -> 100K left, and the rung needs 200K.
			name: "open reservations count against the MONTHLY TOKEN ceiling",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Spend = budget.Spend{Tokens: 49_700_000, ReservedTokens: 200_000, TaskTokens: 100_000}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonMonthlyTokensExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			// ...and on the PER-TASK token ceiling. 2M ceiling, 1.7M billed for
			// this bead, 200K reserved by a sibling attempt -> 100K left, rung
			// needs 200K. A month rollover will not refill it, so: deny.
			name: "open reservations count against the PER-TASK token ceiling",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Spend = budget.Spend{TaskTokens: 1_700_000, ReservedTaskTokens: 200_000}
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonPerTaskTokensExhausted},
		},
		{
			name: "an action the project did not opt into is denied, however much budget it has",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{} // silent -> Resolve gives Triage: false
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			name: "a coarser layer's action veto is honored (ADR-002)",
			in: func(t *testing.T) Input {
				grp := gonkcfg.Policy{Actions: gonkcfg.ActionsPolicy{Triage: b(false)}}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), grp, projectPolicy())
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			// The onboarding MR spends zero tokens and must work on a project
			// that has opted into nothing -- that is the whole point of it
			// (spec 5.3). It must not be caught by the action gate.
			name: "the onboarding trigger is gated by no action",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerOnboarding
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			name: "a .gonk.yml that would not load denies with invalid-config, not 'disabled'",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Invalid = true
				in.InvalidDetail = ".gonk.yml: at '/budget/monthly_tokens': got string, want integer"
				in.Effective = gonkcfg.Effective{} // exactly what the service holds in this state
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonInvalidConfig},
		},
		{
			// The budget window is zero until the first spend sync. A defer that
			// says retry_after: 0001-01-01 is an infinite park -- the caller
			// wakes at once, gets the same answer, and spins.
			name: "a defer against an unsynced (zero) budget window still has a usable retry_after",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Window = spend.Window{}
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{CostUSD: 9.90}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-07-13T10:05:00Z")}, // Now + MaxSpendStale
		},
		{
			// ConsecutiveInfraFailures must count failures at THIS rung, not at
			// any rung: two infra failures on qwen-local followed by an
			// escalation must not count against glm's retry budget.
			name: "the infra-retry cap is per-rung, and escalating resets it",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.MaxInfraRetries = 3
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 3, Rung: "qwen-local", Outcome: OutcomeGateFailed},
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 4},
		},
		{
			// The trailing infra failures happened at a rung that is NO LONGER
			// the target -- which is not a hypothetical: the reresolve loop
			// re-runs Resolve when the operator config changes, so an operator
			// who tightens the instance ladder mid-bead moves the target under a
			// bead whose recent attempts ran somewhere else. Those failures must
			// not be charged against the new rung's retry budget.
			//
			// Real:      ConsecutiveInfraFailures(prior, "qwen-local") == 0 -> run.
			// Saboteur:  counts them anyway -> 2 >= MaxInfraRetries -> deny.
			// This row is the ONLY thing that catches the "counts infra retries
			// across ALL rungs" saboteur: in every other history, the trailing
			// infra run is already at the target rung, so the rung filter has
			// nothing to do and a saboteur that drops it is invisible.
			name: "infra failures at a rung that is no longer the target do not count",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.MaxInfraRetries = 2
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "glm", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "glm", Outcome: OutcomeInfraFailed},
				}
				return in
			},
			// Zero gate failures -> index 0 -> qwen-local, with a clean slate.
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 3},
		},
		{
			name: "a zero cost budget runs local rungs freely and can never reach a cloud rung",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Budget.MonthlyCostUSD = f(0) // the onboarding MR's conservative default
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Spend = budget.Spend{Tokens: 1_000_000, TaskTokens: 300_000}
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			// *** THE DECISION-9 REGRESSION GUARD. ***
			// The exact onboarding default (monthly_cost_usd: 0, local ladder), with
			// a MOUNTAIN of accumulated SYNTHETIC spend and synthetic reservations
			// from previous local sessions. It must STILL RUN. If synthetic dollars
			// ever reach the cost gate, this project cannot afford its own only
			// rung, every freshly-onboarded project bricks itself the moment it does
			// any work, and the entire onboarding flow is dead.
			//
			// This row is the whole reason budget.Spend keeps two separate cost
			// fields. Do not delete it, and do not "simplify" it.
			name: "synthetic spend NEVER blocks a local rung on a zero-cost budget",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Budget.MonthlyCostUSD = f(0)
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Spend = budget.Spend{
					SyntheticCostUSD:         987.65, // priced local inference, in accounting fiction
					ReservedSyntheticCostUSD: 43.21,
					Tokens:                   1_000_000,
					TaskTokens:               300_000,
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			name: "...and the moment it must escalate to a priced rung, it defers",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Budget.MonthlyCostUSD = f(0)
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			// The Decision-9 regression guard above only exercises a LOCAL target
			// rung, whose EstCostUSD is 0 -- so the cost gate is skipped there
			// regardless of what CostUSD holds, and a saboteur that folds
			// synthetic dollars into the real cost gate slips through unnoticed.
			// This row escalates to a PRICED (cloud) rung with real spend well
			// under its ceiling, but a mountain of synthetic spend that would
			// blow through the ceiling if it were ever added in. It must still
			// run: only real dollars gate a priced rung.
			name: "synthetic spend never blocks a PRICED rung either",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{
					CostUSD:                  5.00, // $10 ceiling, $5 remains -- fits glm's $0.40
					SyntheticCostUSD:         987.65,
					ReservedSyntheticCostUSD: 43.21,
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 2},
		},
		{
			name: "per-task tokens exhausted -> DENY, not defer: a month rollover will not refill it",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Spend = budget.Spend{TaskTokens: 1_950_000} // ceiling 2M, est 200K
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonPerTaskTokensExhausted},
		},
		{
			name: "stale spend data -> defer, because we would be deciding on numbers we know are wrong",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.SpendAsOf = at("2026-07-13T09:50:00Z") // 10m old, max 5m
				return in
			},
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonSpendStale,
				RetryAfter: at("2026-07-13T10:05:00Z")},
		},
		{
			name: "stale spend data does not stop a project with no ceilings at all",
			in: func(t *testing.T) Input {
				inst := gonkcfg.Policy{Enabled: b(true), Ladder: []string{"qwen-local", "glm"}}
				proj := gonkcfg.Policy{Enabled: b(true), Actions: gonkcfg.ActionsPolicy{Triage: b(true)},
					Ladder: []string{"qwen-local"}}
				in := baseInput(t)
				in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
				in.SpendAsOf = at("2026-07-13T08:00:00Z")
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			name: "a disabled project is DENIED and carries gonkcfg's own reason",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Effective = effective(t, gonkcfg.Policy{Enabled: b(false)}, gonkcfg.Policy{}, projectPolicy())
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonDisabled,
				Detail: "disabled by instance policy"},
		},
		{
			name: "an unregistered project is denied before anything else is even looked at",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Registered = false
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonNotRegistered},
		},
		{
			name: "no virtual key yet -> defer: provisioning is retried, the work should resume",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.KeyReady = false
				in.KeyRetryBackoff = 5 * time.Minute
				return in
			},
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonKeyMissing,
				RetryAfter: at("2026-07-13T10:05:00Z")},
		},
		{
			name: "quiet hours -> defer until the window ends, in the project's timezone",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Now = at("2026-07-14T03:30:00Z") // = 23:30 the 13th in New York
				in.Window = spend.MonthWindow(in.Now)
				in.SpendAsOf = in.Now
				q, err := ParseQuietHours("22:00-07:00", "America/New_York")
				if err != nil {
					t.Fatal(err)
				}
				in.QuietHours = q
				return in
			},
			// 07:00 New York on the 14th = 11:00Z.
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonQuietHours,
				RetryAfter: at("2026-07-14T11:00:00Z")},
		},
		{
			name: "a rung the catalog has never heard of is a config error, not a run",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				delete(in.Catalog, "qwen-local")
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonInvalidConfig},
		},
	}
}

// --- Phase 2 Task 2.3: cost-class-gated escalation -------------------------

// A local->local escalation is free: granted regardless of the cloud allowance,
// and flagged as a logged escalation so meter can record that the ladder climbed.
// It carries the (generous) local turn cap.
func TestDecideLocalEscalationGrantedAndLogged(t *testing.T) {
	in := baseInput(t)
	in.CloudAllowed = false // a local rung must not need the cloud allowance
	in.Catalog["qwen-local-2"] = opercfg.RungSpec{
		Name: "qwen-local-2", Kind: opercfg.KindLocal, Model: "qwen-b",
		EstCostUSD: 0, EstTokens: 200_000, SyntheticUSDPer1MTokens: 0.20,
	}
	inst := instancePolicy()
	inst.Ladder = []string{"qwen-local", "qwen-local-2", "glm", "sonnet"}
	proj := projectPolicy()
	proj.Ladder = []string{"qwen-local", "qwen-local-2"}
	in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
	in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}

	got := Decide(in)
	if got.Kind != Run || got.Rung != "qwen-local-2" {
		t.Fatalf("want Run qwen-local-2, got %+v", got)
	}
	if !got.Escalated {
		t.Fatal("a ladder-climbing decision must be flagged as a logged escalation")
	}
	if got.MaxTurns != opercfg.DefaultLocalTurns {
		t.Fatalf("local rung turn cap = %d, want %d", got.MaxTurns, opercfg.DefaultLocalTurns)
	}
}

// A cloud escalation with the allowance OFF must not spend: it denies (the Deny
// that dispatch maps to needs-human) with the bounded cloud-not-allowed reason.
// No reservation is minted -- the gate returns before the money section.
func TestDecideCloudEscalationDeniedWhenAllowanceOff(t *testing.T) {
	in := baseInput(t)
	in.CloudAllowed = false
	in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
	got := Decide(in)
	if got.Kind != Deny || got.Reason != ReasonCloudNotAllowed {
		t.Fatalf("want Deny/%s, got %+v", ReasonCloudNotAllowed, got)
	}
}

// With the allowance ON, the same cloud escalation is granted, and it carries
// the stingier cloud turn cap -- tighter than a local rung's.
func TestDecideCloudEscalationGrantedWhenAllowanceOn(t *testing.T) {
	in := baseInput(t)
	in.CloudAllowed = true
	in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
	got := Decide(in)
	if got.Kind != Run || got.Rung != "glm" {
		t.Fatalf("want Run glm, got %+v", got)
	}
	if got.MaxTurns != opercfg.DefaultCloudTurns {
		t.Fatalf("cloud rung turn cap = %d, want %d", got.MaxTurns, opercfg.DefaultCloudTurns)
	}
	if got.MaxTurns >= opercfg.DefaultLocalTurns {
		t.Fatalf("cloud turn cap %d is not stingier than local %d", got.MaxTurns, opercfg.DefaultLocalTurns)
	}
}

// The local ladder exhausted with no cloud allowance lands on needs-human: an
// all-local ladder with nowhere left to climb denies (ladder-exhausted), which
// dispatch maps to needs-human -- we never auto-cross into paid cloud.
func TestDecideLocalLadderExhaustedNoCloudNeedsHuman(t *testing.T) {
	in := baseInput(t)
	in.CloudAllowed = false
	inst := instancePolicy()
	inst.Ladder = []string{"qwen-local", "glm", "sonnet"}
	proj := projectPolicy()
	proj.Ladder = []string{"qwen-local"} // a single, all-local rung
	in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
	in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
	got := Decide(in)
	if got.Kind != Deny || got.Reason != ReasonLadderExhausted {
		t.Fatalf("want Deny/%s, got %+v", ReasonLadderExhausted, got)
	}
}

func TestDecide(t *testing.T) {
	for _, c := range decideCases() {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(c.in(t))
			if got.Kind != c.want.Kind || got.Rung != c.want.Rung || got.Attempt != c.want.Attempt ||
				got.Reason != c.want.Reason || !got.RetryAfter.Equal(c.want.RetryAfter) {
				t.Fatalf("\n got %+v\nwant %+v", got, c.want)
			}
			if c.want.Model != "" && got.Model != c.want.Model {
				t.Fatalf("model = %q, want %q", got.Model, c.want.Model)
			}
			if c.want.Detail != "" && got.Detail != c.want.Detail {
				t.Fatalf("detail = %q, want %q", got.Detail, c.want.Detail)
			}
			// Invariants that must hold for EVERY decision.
			if got.Kind == Run && got.Rung == "" {
				t.Fatal("a run decision with no rung")
			}
			if got.Kind == Defer && got.RetryAfter.IsZero() {
				t.Fatal("a defer with no retry_after is an infinite park")
			}
			if got.Kind != Run && got.Reason == "" {
				t.Fatal("a defer/deny with no machine reason")
			}
			if got.Kind == Run && got.Reason != "" {
				t.Fatalf("a run carrying a reason: %q", got.Reason)
			}
		})
	}
}

// Non-vacuity guard: the table must exercise every decision kind and a
// meaningful spread of reasons, or it is not constraining the policy.
func TestDecideTableCoversTheContract(t *testing.T) {
	kinds := map[Kind]int{}
	reasons := map[string]int{}
	for _, c := range decideCases() {
		kinds[c.want.Kind]++
		if c.want.Reason != "" {
			reasons[c.want.Reason]++
		}
	}
	for _, k := range []Kind{Run, Defer, Deny} {
		if kinds[k] == 0 {
			t.Errorf("no table row expects a %q decision", k)
		}
	}
	for _, r := range []string{
		ReasonNotRegistered, ReasonDisabled, ReasonInvalidConfig, ReasonActionNotAllowed,
		ReasonKeyMissing, ReasonQuietHours, ReasonSpendStale, ReasonLadderExhausted,
		ReasonInfraRetriesExhausted, ReasonPerTaskTokensExhausted, ReasonMonthlyTokensExhausted,
		ReasonMonthlyCostExhausted,
	} {
		if reasons[r] == 0 {
			t.Errorf("no table row expects reason %q; it is untested and may be unreachable", r)
		}
	}
}
