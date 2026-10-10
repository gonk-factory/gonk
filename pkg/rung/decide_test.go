package rung

import (
	"math"
	"strings"
	"testing"
	"time"

	// Without tzdata, time.LoadLocation("America/New_York") fails in a scratch
	// container AND in any CI image with no zoneinfo -- and the quiet-hours case
	// would fail for a reason that has nothing to do with the policy.
	_ "time/tzdata"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/budget"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/spend"
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
			// gonk-tios D5: pipeline-diagnosis is gated by actions.pipelines ALONE.
			name: "pipeline-diagnosis is denied when pipelines is off, even with triage on",
			in: func(t *testing.T) Input {
				proj := projectPolicy() // Triage: true, Pipelines silent -> false
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerPipelineDiagnosis
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			name: "pipeline-diagnosis runs when pipelines is on, even with triage off",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{Triage: b(false), Pipelines: b(true)}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerPipelineDiagnosis
				return in
			},
			// The ladder floor (below) starts this trigger at position 2.
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 1},
		},
		{
			name: "an instance veto of pipelines beats a project opt-in (through Resolve)",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{Triage: b(true), Pipelines: b(true)}
				inst := instancePolicy()
				inst.Actions = gonkcfg.ActionsPolicy{Pipelines: b(false)}
				in := baseInput(t)
				in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerPipelineDiagnosis
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			// gonk-2usw 3.4: pipeline-fix is gated by actions.pipeline_fixes.
			// PLAN, must be seen red: "trigger not gated by the action".
			name: "pipeline-fix is denied when only pipelines is on (diagnosing is not fixing)",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{Triage: b(true), Pipelines: b(true)}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerPipelineFix
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			name: "pipeline-fix is denied on a project that opted in to nothing but triage",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Trigger = atags.TriggerPipelineFix
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			name: "pipeline-fix runs when pipelines and pipeline_fixes are on, even with triage off",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{Triage: b(false), Pipelines: b(true), PipelineFixes: b(true)}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerPipelineFix
				return in
			},
			// The ladder floor starts this trigger at position 2.
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 1},
		},
		{
			name: "an instance veto of pipeline_fixes beats a project opt-in (through Resolve)",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{Pipelines: b(true), PipelineFixes: b(true)}
				inst := instancePolicy()
				inst.Actions = gonkcfg.ActionsPolicy{PipelineFixes: b(false)}
				in := baseInput(t)
				in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerPipelineFix
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			name: "a group veto of PIPELINES denies pipeline-fix too",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{Pipelines: b(true), PipelineFixes: b(true)}
				grp := gonkcfg.Policy{Actions: gonkcfg.ActionsPolicy{Pipelines: b(false)}}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), grp, proj)
				in.Trigger = atags.TriggerPipelineFix
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			name: "pipeline_fixes on does not let pipeline-diagnosis or triage through on its own",
			in: func(t *testing.T) Input {
				// Built in Go, so Load's "fixes needs pipelines" rule never ran.
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{PipelineFixes: b(true)}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerPipelineDiagnosis
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
		// ---- floor retry (gonk-tios R2). In the TABLE, not only in the dedicated
		// tests below, so the saboteurs in mutation_test.go are held by them.
		{
			name: "pipeline diagnosis: the FIRST gate failure re-runs the floor rung",
			in: func(t *testing.T) Input {
				return pipelineInput(t, []string{"qwen-local", "glm", "sonnet"}, func(in *Input) {
					in.Prior = []Attempt{{Attempt: 1, Rung: "glm", Outcome: OutcomeGateFailed}}
				})
			},
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 2},
		},
		{
			name: "pipeline diagnosis: the SECOND gate failure escalates exactly one rung",
			in: func(t *testing.T) Input {
				return pipelineInput(t, []string{"qwen-local", "glm", "sonnet"}, func(in *Input) {
					in.Prior = []Attempt{
						{Attempt: 1, Rung: "glm", Outcome: OutcomeGateFailed},
						{Attempt: 2, Rung: "glm", Outcome: OutcomeGateFailed},
					}
				})
			},
			want: Decision{Kind: Run, Rung: "sonnet", Model: "claude-sonnet", Attempt: 3},
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

// ---- per-trigger ladder floor (gonk-tios Q1: pipeline diagnosis starts at rung 2)

func pipelineInput(t *testing.T, ladder []string, mut func(*Input)) Input {
	t.Helper()
	proj := projectPolicy()
	proj.Actions = gonkcfg.ActionsPolicy{Triage: b(true), Pipelines: b(true)}
	proj.Ladder = ladder
	in := baseInput(t)
	in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
	in.Trigger = atags.TriggerPipelineDiagnosis
	if mut != nil {
		mut(&in)
	}
	return in
}

// The two pipeline triggers have a floor (gonk-tios Q1, gonk-2usw 3.4), and so
// does issue-change (gonk-cuwn 2.9); nothing else does -- mr-review in
// particular, whose ladder is not the project's at all.
func TestLadderFloorsAreExactlyPipelineDiagnosis(t *testing.T) {
	if len(LadderFloors) != 3 || LadderFloors[atags.TriggerPipelineDiagnosis] != 1 || LadderFloors[atags.TriggerPipelineFix] != 1 ||
		LadderFloors[atags.TriggerIssueChange] != 1 {
		t.Fatalf("LadderFloors = %v, want exactly {pipeline-diagnosis: 1, pipeline-fix: 1, issue-change: 1}", LadderFloors)
	}
	if got := LadderFloor(atags.TriggerIssueChange); got != 1 {
		t.Errorf("LadderFloor(issue-change) = %d, want 1", got)
	}
	if got := LadderFloor(atags.TriggerMRReview); got != 0 {
		t.Errorf("LadderFloor(mr-review) = %d, want 0: its ladder is the one reviewer rung", got)
	}
	if got := LadderFloor(atags.TriggerPipelineFix); got != 1 {
		t.Errorf("LadderFloor(pipeline-fix) = %d, want 1", got)
	}
	for _, trig := range []string{atags.TriggerIssueTriage, atags.TriggerOnboarding,
		atags.TriggerScaffold, atags.TriggerMentionReply, "no-such-trigger"} {
		if got := LadderFloor(trig); got != 0 {
			t.Errorf("LadderFloor(%q) = %d, want 0", trig, got)
		}
	}
}

func TestPipelineDiagnosisStartsAtLadderFloor(t *testing.T) {
	got := Decide(pipelineInput(t, []string{"qwen-local", "glm", "sonnet"}, nil))
	if got.Kind != Run || got.Rung != "glm" || got.Model != "glm-5" {
		t.Fatalf("want Run on ladder index 1 (glm), got %+v", got)
	}
	if got.Attempt != 1 {
		t.Errorf("attempt = %d, want 1: the floor must not burn an attempt", got.Attempt)
	}
	if got.Escalated {
		t.Error("the first attempt at the floor is not an escalation")
	}
	for _, want := range []string{`"glm"`, "position 2 of 3", "pipeline-diagnosis"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q does not say %q (the floor must be visible in /decide)", got.Detail, want)
		}
	}
}

// ---- floor retry (gonk-tios R2, owner decision 2026-10-04): pipeline diagnosis
// gets ONE extra try at its floor rung before a gate failure escalates.

var pipelineLadder = []string{"qwen-local", "glm", "sonnet"}

// gateFails is a history of n gate failures, at the rungs the new rule runs
// them on for pipeline-diagnosis over pipelineLadder: glm, glm, sonnet.
func gateFails(n int) []Attempt {
	rungs := []string{"glm", "glm", "sonnet", "sonnet"}
	var p []Attempt
	for i := 0; i < n; i++ {
		p = append(p, Attempt{Attempt: i + 1, Rung: rungs[i], Outcome: OutcomeGateFailed})
	}
	return p
}

func TestFloorRetriesAreExactlyPipelineDiagnosis(t *testing.T) {
	if len(FloorRetries) != 4 || FloorRetries[atags.TriggerPipelineDiagnosis] != 1 || FloorRetries[atags.TriggerPipelineFix] != 1 ||
		FloorRetries[atags.TriggerIssueChange] != 1 || FloorRetries[atags.TriggerMRReview] != 1 {
		t.Fatalf("FloorRetries = %v, want exactly {pipeline-diagnosis: 1, pipeline-fix: 1, issue-change: 1, mr-review: 1}", FloorRetries)
	}
	for _, trig := range []string{atags.TriggerIssueChange, atags.TriggerMRReview} {
		if got := FloorRetry(trig); got != 1 {
			t.Errorf("FloorRetry(%s) = %d, want 1", trig, got)
		}
	}
	if got := FloorRetry(atags.TriggerPipelineFix); got != 1 {
		t.Errorf("FloorRetry(pipeline-fix) = %d, want 1", got)
	}
	if got := FloorRetry(atags.TriggerPipelineDiagnosis); got != 1 {
		t.Errorf("FloorRetry(pipeline-diagnosis) = %d, want 1", got)
	}
	for _, trig := range []string{atags.TriggerIssueTriage, atags.TriggerOnboarding,
		atags.TriggerScaffold, atags.TriggerMentionReply, "no-such-trigger", ""} {
		if got := FloorRetry(trig); got != 0 {
			t.Errorf("FloorRetry(%q) = %d, want 0", trig, got)
		}
	}
}

// The ladder invariant: every gate failure buys exactly one thing -- one floor
// retry or one rung -- and for a trigger with neither floor nor retries the
// rung index IS the gate-failure count, the rule the ladder has always had.
func TestLadderPositionEveryGateFailureBuysExactlyOneThing(t *testing.T) {
	for _, trig := range []string{atags.TriggerIssueTriage, atags.TriggerOnboarding, atags.TriggerScaffold,
		atags.TriggerMentionReply, atags.TriggerPipelineDiagnosis, atags.TriggerPipelineFix, "no-such-trigger"} {
		for g := 0; g <= 4; g++ {
			pos := LadderPosition(trig, gateFails(g))
			if pos.GateFailures != g || pos.RetriesUsed+pos.Climbs != g {
				t.Errorf("%s, %d gate failures: retries used %d + climbs %d != %d (a gate failure was counted twice or dropped)",
					trig, g, pos.RetriesUsed, pos.Climbs, g)
			}
			if pos.Index != pos.Floor+pos.Climbs {
				t.Errorf("%s, %d gate failures: index %d != floor %d + climbs %d", trig, g, pos.Index, pos.Floor, pos.Climbs)
			}
			if trig != atags.TriggerPipelineDiagnosis && trig != atags.TriggerPipelineFix && (pos.Index != g || pos.Floor != 0 || pos.Retries != 0 || pos.RetriesUsed != 0) {
				t.Errorf("%s, %d gate failures: position %+v, want index == gate failures and no floor/retry", trig, g, pos)
			}
		}
	}
	// pipeline-diagnosis, spelled out: floor 1, one retry.
	for g, want := range []Position{
		{Floor: 1, Retries: 1, GateFailures: 0, RetriesUsed: 0, Climbs: 0, Index: 1},
		{Floor: 1, Retries: 1, GateFailures: 1, RetriesUsed: 1, Climbs: 0, Index: 1},
		{Floor: 1, Retries: 1, GateFailures: 2, RetriesUsed: 1, Climbs: 1, Index: 2},
		{Floor: 1, Retries: 1, GateFailures: 3, RetriesUsed: 1, Climbs: 2, Index: 3},
	} {
		if got := LadderPosition(atags.TriggerPipelineDiagnosis, gateFails(g)); got != want {
			t.Errorf("pipeline-diagnosis, %d gate failures: position %+v, want %+v", g, got, want)
		}
	}
	// Infra failures, successes and aborts move nothing.
	noise := []Attempt{
		{Attempt: 1, Rung: "glm", Outcome: OutcomeInfraFailed},
		{Attempt: 2, Rung: "glm", Outcome: OutcomeAborted},
		{Attempt: 3, Rung: "glm", Outcome: OutcomeSuccess},
	}
	if got := LadderPosition(atags.TriggerPipelineDiagnosis, noise); got.Index != 1 || got.RetriesUsed != 0 || got.Climbs != 0 {
		t.Errorf("non-gate outcomes moved the position: %+v", got)
	}
}

func TestPipelineDiagnosisFirstGateFailureRetriesTheFloorRung(t *testing.T) {
	got := Decide(pipelineInput(t, pipelineLadder, func(in *Input) { in.Prior = gateFails(1) }))
	if got.Kind != Run || got.Rung != "glm" || got.Model != "glm-5" {
		t.Fatalf("want Run on the SAME floor rung (glm) after one gate failure, got %+v", got)
	}
	if got.Attempt != 2 {
		t.Errorf("attempt = %d, want 2: attempts count from 1 and the retry is attempt 2", got.Attempt)
	}
	if got.Escalated {
		t.Error("a floor retry re-runs the same rung; it must not be reported as an escalation")
	}
	want := `rung "glm" (ladder position 2 of 3): trigger "pipeline-diagnosis" starts at ladder position 2, ` +
		`1 gate failure(s), floor retry 1 of 1 (the same rung again, not an escalation)`
	if got.Detail != want {
		t.Errorf("detail\n got %q\nwant %q", got.Detail, want)
	}
}

func TestPipelineDiagnosisFirstAttemptDetailNamesTheUnusedFloorRetry(t *testing.T) {
	got := Decide(pipelineInput(t, pipelineLadder, nil))
	want := `rung "glm" (ladder position 2 of 3): trigger "pipeline-diagnosis" starts at ladder position 2, ` +
		`0 gate failure(s), 0 of 1 floor retries used`
	if got.Kind != Run || got.Attempt != 1 || got.Escalated || got.Detail != want {
		t.Fatalf("\n got %+v\nwant Run attempt 1, not escalated, detail %q", got, want)
	}
}

func TestPipelineDiagnosisSecondGateFailureEscalatesOneRung(t *testing.T) {
	got := Decide(pipelineInput(t, pipelineLadder, func(in *Input) { in.Prior = gateFails(2) }))
	if got.Kind != Run || got.Rung != "sonnet" || got.Attempt != 3 || !got.Escalated {
		t.Fatalf("want Run sonnet attempt 3 escalated (the retry did not consume the escalation), got %+v", got)
	}
	want := `rung "sonnet" (ladder position 3 of 3): trigger "pipeline-diagnosis" starts at ladder position 2, ` +
		`2 gate failure(s), 1 of 1 floor retries used, then 1 escalation(s)`
	if got.Detail != want {
		t.Errorf("detail\n got %q\nwant %q", got.Detail, want)
	}
}

func TestPipelineDiagnosisThirdGateFailureExhaustsTheLadder(t *testing.T) {
	got := Decide(pipelineInput(t, pipelineLadder, func(in *Input) { in.Prior = gateFails(3) }))
	want := `3 rungs, trigger "pipeline-diagnosis" starts at rung 2, 3 gate failures, 1 of 1 floor retries used, then 2 escalation(s)`
	if got.Kind != Deny || got.Reason != ReasonLadderExhausted || got.Attempt != 4 || got.Detail != want {
		t.Fatalf("\n got %+v\nwant Deny/%s attempt 4, detail %q", got, ReasonLadderExhausted, want)
	}
}

// localLocalPaid is the live instance's shape (R2): a ladder of two free local
// rungs and one paid rung, [qwen-local, qwen-local-2, sonnet].
func localLocalPaid(t *testing.T, trigger string, cloudAllowed bool, prior []Attempt) Input {
	t.Helper()
	in := baseInput(t)
	in.Trigger, in.CloudAllowed, in.Prior = trigger, cloudAllowed, prior
	in.Catalog["qwen-local-2"] = opercfg.RungSpec{
		Name: "qwen-local-2", Kind: opercfg.KindLocal, Model: "qwen-b",
		EstCostUSD: 0, EstTokens: 200_000, SyntheticUSDPer1MTokens: 0.20,
	}
	inst := instancePolicy()
	inst.Ladder = []string{"qwen-local", "qwen-local-2", "sonnet"}
	proj := projectPolicy()
	proj.Actions = gonkcfg.ActionsPolicy{Triage: b(true), Pipelines: b(true)}
	proj.Ladder = inst.Ladder
	in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
	return in
}

// R2 itself: a project with no cloud allowance, on [local, local, paid]. Before
// the floor retry, one gate failure ended the run `denied`.
func TestPipelineDiagnosisOnAZeroCloudProjectGetsTwoTriesBeforeTheCloudDeny(t *testing.T) {
	zeroCloud := func(prior []Attempt) Decision {
		return Decide(localLocalPaid(t, atags.TriggerPipelineDiagnosis, false, prior))
	}
	gf := func(n int) []Attempt {
		var p []Attempt
		for i := 0; i < n; i++ {
			p = append(p, Attempt{Attempt: i + 1, Rung: "qwen-local-2", Outcome: OutcomeGateFailed})
		}
		return p
	}
	for n := 0; n <= 1; n++ {
		if got := zeroCloud(gf(n)); got.Kind != Run || got.Rung != "qwen-local-2" || got.Attempt != n+1 || got.Escalated {
			t.Fatalf("%d gate failure(s): want Run qwen-local-2 attempt %d, not escalated; got %+v", n, n+1, got)
		}
	}
	if got := zeroCloud(gf(2)); got.Kind != Deny || got.Reason != ReasonCloudNotAllowed || got.Attempt != 3 {
		t.Fatalf("2 gate failures: want Deny/%s attempt 3 (the cloud deny is unchanged, it is only reached later); got %+v",
			ReasonCloudNotAllowed, got)
	}
}

// Every existing trigger, across its whole ladder walk, is exactly what it was
// before the floor and the floor retry existed: index == gate failures, an
// empty run detail, the old ladder-exhausted detail.
func TestExistingTriggersGetNoFloorRetry(t *testing.T) {
	for _, trig := range []string{atags.TriggerIssueTriage, atags.TriggerScaffold, atags.TriggerMentionReply} {
		walk := func(n int) Decision {
			return Decide(pipelineInput(t, pipelineLadder, func(in *Input) {
				in.Trigger = trig
				rungs := []string{"qwen-local", "glm", "sonnet"}
				for i := 0; i < n; i++ {
					in.Prior = append(in.Prior, Attempt{Attempt: i + 1, Rung: rungs[i], Outcome: OutcomeGateFailed})
				}
			}))
		}
		wants := []Decision{
			{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1, MaxTurns: opercfg.DefaultLocalTurns},
			{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 2, MaxTurns: opercfg.DefaultCloudTurns, Escalated: true},
			{Kind: Run, Rung: "sonnet", Model: "claude-sonnet", Attempt: 3, MaxTurns: opercfg.DefaultCloudTurns, Escalated: true},
			{Kind: Deny, Attempt: 4, Reason: ReasonLadderExhausted, Detail: "3 rungs, 3 gate failures"},
		}
		for n, want := range wants {
			if got := walk(n); got != want {
				t.Errorf("%s after %d gate failure(s):\n got %+v\nwant %+v", trig, n, got, want)
			}
		}
	}
}

func TestPipelineDiagnosisFloorRetryStillObeysThePerTaskTokenBudget(t *testing.T) {
	// The retry is a real attempt: it needs ~200K tokens of the bead's per-task
	// budget (2M) like any other. With 1.9M already spent it does not fit.
	got := Decide(pipelineInput(t, pipelineLadder, func(in *Input) {
		in.Prior = gateFails(1)
		in.Spend.TaskTokens = 1_900_000
	}))
	if got.Kind != Deny || got.Reason != ReasonPerTaskTokensExhausted || got.Attempt != 2 {
		t.Fatalf("want Deny/%s on the floor retry, got %+v", ReasonPerTaskTokensExhausted, got)
	}
}

func TestPipelineDiagnosisInfraFailuresDoNotSpendTheFloorRetry(t *testing.T) {
	infra := func(n, from int) []Attempt {
		var p []Attempt
		for i := 0; i < n; i++ {
			p = append(p, Attempt{Attempt: from + i, Rung: "glm", Outcome: OutcomeInfraFailed})
		}
		return p
	}
	decide := func(prior []Attempt) Decision {
		return Decide(pipelineInput(t, pipelineLadder, func(in *Input) { in.Prior = prior }))
	}
	// Infra failures alone: still attempt-1 territory, no retry used.
	if got := decide(infra(4, 1)); got.Kind != Run || got.Rung != "glm" || got.Attempt != 5 ||
		!strings.Contains(got.Detail, "0 of 1 floor retries used") {
		t.Fatalf("4 infra failures: want Run glm attempt 5 with the floor retry unused, got %+v", got)
	}
	// An infra failure DURING the floor retry: still the floor retry, not an escalation.
	prior := append(gateFails(1), infra(4, 2)...)
	if got := decide(prior); got.Kind != Run || got.Rung != "glm" || got.Attempt != 6 || got.Escalated ||
		!strings.Contains(got.Detail, "floor retry 1 of 1") {
		t.Fatalf("gate failure then 4 infra failures: want Run glm attempt 6, floor retry 1 of 1, got %+v", got)
	}
	// The infra cap is counted from the END of the history at the target rung, so
	// the gate failure between them separates the two runs of infra failures:
	// 4 before it and 4 after it is NOT 8 (which would deny at the cap of 5).
	prior = append(append(infra(4, 1), Attempt{Attempt: 5, Rung: "glm", Outcome: OutcomeGateFailed}), infra(4, 6)...)
	if got := decide(prior); got.Kind != Run || got.Rung != "glm" || got.Attempt != 10 {
		t.Fatalf("4 infra, gate failure, 4 infra: want Run glm attempt 10, got %+v", got)
	}
	// ...and five in a row during the retry is the cap, as on any rung.
	prior = append(gateFails(1), infra(5, 2)...)
	if got := decide(prior); got.Kind != Deny || got.Reason != ReasonInfraRetriesExhausted {
		t.Fatalf("gate failure then 5 infra failures: want Deny/%s, got %+v", ReasonInfraRetriesExhausted, got)
	}
}

// worstCaseSessions is the LARGEST number of sessions any outcome sequence can
// make Decide grant for one bead: an exhaustive walk over {infra-failed,
// gate-failed} after every Run (a success or an abort ends the bead, so
// neither can lengthen a path).
func worstCaseSessions(t *testing.T, mk func(prior []Attempt) Input) int {
	t.Helper()
	var walk func(prior []Attempt) int
	walk = func(prior []Attempt) int {
		if len(prior) > 64 {
			t.Fatal("the attempt history is unbounded")
		}
		d := Decide(mk(prior))
		if d.Kind != Run {
			return 0
		}
		best := 0
		for _, o := range []Outcome{OutcomeInfraFailed, OutcomeGateFailed} {
			next := append(append([]Attempt(nil), prior...), Attempt{Attempt: d.Attempt, Rung: d.Rung, Outcome: o})
			best = max(best, walk(next))
		}
		return 1 + best
	}
	return walk(nil)
}

// The combined bound. Each "slot" a gate failure opens (the floor try, the
// floor retry, each rung above) may see up to max_infra_retries sessions:
// M-1 infra failures and then the gate failure that closes it, or M infra
// failures and the deny. So the bound is slots * M, and the floor retry adds
// exactly ONE slot -- M sessions -- to pipeline-diagnosis and nothing to anyone
// else.
func TestWorstCaseSessionCountWithTheFloorRetry(t *testing.T) {
	const m = 3 // max_infra_retries; small so the exhaustive walk stays small
	mk := func(trigger string, cloud bool) func([]Attempt) Input {
		return func(prior []Attempt) Input {
			in := localLocalPaid(t, trigger, cloud, prior)
			in.MaxInfraRetries = m
			// No ceilings: this counts the ladder, not the money.
			in.Effective.Budget = gonkcfg.EffectiveBudget{
				MonthlyCostUSD: math.Inf(1),
				MonthlyTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
				PerTaskTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
			}
			return in
		}
	}
	for _, c := range []struct {
		name    string
		trigger string
		cloud   bool
		want    int
	}{
		// qwen-local-2 twice (the try and the floor retry), then sonnet: 3 slots.
		{"pipeline-diagnosis, cloud allowed", atags.TriggerPipelineDiagnosis, true, 3 * m},
		// qwen-local-2 twice; sonnet is denied: 2 slots.
		{"pipeline-diagnosis, no cloud allowance", atags.TriggerPipelineDiagnosis, false, 2 * m},
		// issue-triage: one slot per rung it may use, exactly as before.
		{"issue-triage, cloud allowed (unchanged)", atags.TriggerIssueTriage, true, 3 * m},
		{"issue-triage, no cloud allowance (unchanged)", atags.TriggerIssueTriage, false, 2 * m},
	} {
		if got := worstCaseSessions(t, mk(c.trigger, c.cloud)); got != c.want {
			t.Errorf("%s: worst case %d sessions, want %d", c.name, got, c.want)
		}
	}
}

func TestPipelineDiagnosisInfraFailureRetriesTheFloorRung(t *testing.T) {
	got := Decide(pipelineInput(t, []string{"qwen-local", "glm", "sonnet"}, func(in *Input) {
		in.Prior = []Attempt{{Attempt: 1, Rung: "glm", Outcome: OutcomeInfraFailed}}
	}))
	if got.Kind != Run || got.Rung != "glm" || got.Attempt != 2 || got.Escalated {
		t.Fatalf("want Run glm attempt 2 (infra retry stays on the floor rung), got %+v", got)
	}
}

func TestPipelineDiagnosisExhaustsATwoRungLadderAfterTheFloorRetry(t *testing.T) {
	two := []string{"qwen-local", "glm"}
	if got := Decide(pipelineInput(t, two, func(in *Input) { in.Prior = gateFails(1) })); got.Kind != Run || got.Rung != "glm" || got.Attempt != 2 {
		t.Fatalf("one gate failure on a two-rung ladder: want the floor retry (Run glm attempt 2), got %+v", got)
	}
	got := Decide(pipelineInput(t, two, func(in *Input) { in.Prior = gateFails(2) }))
	if got.Kind != Deny || got.Reason != ReasonLadderExhausted || got.Attempt != 3 {
		t.Fatalf("two gate failures on a two-rung ladder: want Deny/%s attempt 3, got %+v", ReasonLadderExhausted, got)
	}
}

func TestPipelineDiagnosisLadderShorterThanFloorDeniesClearly(t *testing.T) {
	// A one-rung ladder has no rung at position 2. Must be a deny with a reason
	// that names the floor, never an index panic or a silent run on rung 1.
	got := Decide(pipelineInput(t, []string{"qwen-local"}, nil))
	if got.Kind != Deny || got.Reason != ReasonLadderExhausted {
		t.Fatalf("want Deny/%s, got %+v", ReasonLadderExhausted, got)
	}
	for _, want := range []string{"pipeline-diagnosis", "rung 2", "has only 1 rung"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q does not say %q", got.Detail, want)
		}
	}
	if got.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", got.Attempt)
	}
	// The floor retry cannot conjure a rung either: with a gate failure on the
	// books the answer is the same deny, word for word.
	again := Decide(pipelineInput(t, []string{"qwen-local"}, func(in *Input) {
		in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
	}))
	if again.Kind != Deny || again.Reason != ReasonLadderExhausted || again.Detail != got.Detail {
		t.Errorf("with one gate failure: got %+v, want the same deny and detail %q", again, got.Detail)
	}
}

func TestPipelineDiagnosisFloorOnPaidRungNeedsCloudAllowance(t *testing.T) {
	got := Decide(pipelineInput(t, []string{"qwen-local", "glm"}, func(in *Input) {
		in.CloudAllowed = false
	}))
	if got.Kind != Deny || got.Reason != ReasonCloudNotAllowed {
		t.Fatalf("want Deny/%s (the floor rung is paid), got %+v", ReasonCloudNotAllowed, got)
	}
}

func TestExistingTriggersIgnoreTheLadderFloor(t *testing.T) {
	// Pipelines enabled too, so the ONLY difference from pipeline-diagnosis is the
	// trigger: every other trigger still starts at index 0 with an empty Detail.
	for _, trig := range []string{atags.TriggerIssueTriage, atags.TriggerScaffold, atags.TriggerMentionReply} {
		got := Decide(pipelineInput(t, []string{"qwen-local", "glm"}, func(in *Input) { in.Trigger = trig }))
		if got.Kind != Run || got.Rung != "qwen-local" || got.Attempt != 1 || got.Detail != "" || got.Escalated {
			t.Errorf("%s: want plain Run on qwen-local with empty detail, got %+v", trig, got)
		}
	}
}

// gonk-2usw 3.4: pipeline-fix walks the ladder exactly as pipeline-diagnosis
// does -- floor 1, one floor retry, then one rung per gate failure.
func TestPipelineFixLadderPositionMatchesPipelineDiagnosis(t *testing.T) {
	for g := 0; g <= 4; g++ {
		fix, diag := LadderPosition(atags.TriggerPipelineFix, gateFails(g)), LadderPosition(atags.TriggerPipelineDiagnosis, gateFails(g))
		if fix != diag {
			t.Errorf("%d gate failures: pipeline-fix %+v, pipeline-diagnosis %+v", g, fix, diag)
		}
	}
	for g, want := range []Position{
		{Floor: 1, Retries: 1, GateFailures: 0, RetriesUsed: 0, Climbs: 0, Index: 1},
		{Floor: 1, Retries: 1, GateFailures: 1, RetriesUsed: 1, Climbs: 0, Index: 1},
		{Floor: 1, Retries: 1, GateFailures: 2, RetriesUsed: 1, Climbs: 1, Index: 2},
	} {
		if got := LadderPosition(atags.TriggerPipelineFix, gateFails(g)); got != want {
			t.Errorf("%d gate failures: position %+v, want %+v", g, got, want)
		}
	}
}

// gonk-2usw 3.4 / 7.3: the per-trigger reservation TTL. pipeline-fix holds its
// reservation for base + the project's effective verify_timeout; every other
// trigger's TTL is the base, unchanged.
func TestReservationTTL(t *testing.T) {
	const base = 60 * time.Minute
	fix := func(d time.Duration) gonkcfg.Effective {
		return gonkcfg.Effective{PipelineFix: gonkcfg.EffectivePipelineFix{VerifyTimeout: d}}
	}
	resolved := func(yaml string) gonkcfg.Effective {
		t.Helper()
		pc, err := gonkcfg.Load([]byte("version: 1\nenabled: true\nactions: { pipelines: true, pipeline_fixes: true }\n" + yaml))
		if err != nil {
			t.Fatal(err)
		}
		return gonkcfg.Resolve(gonkcfg.Policy{}, gonkcfg.Policy{}, *pc)
	}
	for _, c := range []struct {
		name    string
		trigger string
		eff     gonkcfg.Effective
		want    time.Duration
	}{
		{"pipeline-fix, default verify_timeout (2h)", atags.TriggerPipelineFix, resolved(""), 3 * time.Hour},
		{"pipeline-fix, project sets 30m", atags.TriggerPipelineFix, resolved("pipeline_fix: { verify_timeout: 30m }\n"), 90 * time.Minute},
		{"pipeline-fix, project sets the ceiling", atags.TriggerPipelineFix, resolved("pipeline_fix: { verify_timeout: 6h }\n"), 7 * time.Hour},
		{"pipeline-fix, an unresolved Effective adds nothing", atags.TriggerPipelineFix, gonkcfg.Effective{}, base},
		{"pipeline-fix, a timeout past the ceiling is clamped to it", atags.TriggerPipelineFix, fix(1000 * time.Hour), 7 * time.Hour},
		{"pipeline-fix, a negative timeout never shortens the base", atags.TriggerPipelineFix, fix(-30 * time.Minute), base},
		{"pipeline-diagnosis is the base", atags.TriggerPipelineDiagnosis, resolved("pipeline_fix: { verify_timeout: 6h }\n"), base},
		{"issue-triage is the base", atags.TriggerIssueTriage, resolved("pipeline_fix: { verify_timeout: 6h }\n"), base},
		{"mention-reply is the base", atags.TriggerMentionReply, fix(6 * time.Hour), base},
		{"scaffold is the base", atags.TriggerScaffold, fix(6 * time.Hour), base},
		{"onboarding is the base", atags.TriggerOnboarding, fix(6 * time.Hour), base},
		{"an unknown trigger is the base", "no-such-trigger", fix(6 * time.Hour), base},
		{"no trigger is the base", "", fix(6 * time.Hour), base},
	} {
		if got := ReservationTTL(c.trigger, base, c.eff); got != c.want {
			t.Errorf("%s: ReservationTTL = %s, want %s", c.name, got, c.want)
		}
	}
}

// ActionFor, trigger by trigger: which action gates it, and that no OTHER
// action opens it.
func TestActionForIsOneActionPerTrigger(t *testing.T) {
	all := gonkcfg.Actions{Triage: true, Pipelines: true, PipelineFixes: true, Features: true, Scaffold: true, Reviews: true}
	for _, c := range []struct {
		trigger string
		gated   bool
		off     func(*gonkcfg.Actions) // switch off the ONE action that gates it
	}{
		{atags.TriggerIssueTriage, true, func(a *gonkcfg.Actions) { a.Triage = false }},
		{atags.TriggerMentionReply, true, func(a *gonkcfg.Actions) { a.Triage = false }},
		{atags.TriggerScaffold, true, func(a *gonkcfg.Actions) { a.Triage = false }},
		{atags.TriggerPipelineDiagnosis, true, func(a *gonkcfg.Actions) { a.Pipelines = false }},
		{atags.TriggerPipelineFix, true, func(a *gonkcfg.Actions) { a.PipelineFixes = false }},
		{atags.TriggerIssueChange, true, func(a *gonkcfg.Actions) { a.Features = false }},
		{atags.TriggerMRReview, true, func(a *gonkcfg.Actions) { a.Reviews = false }},
		{atags.TriggerOnboarding, false, nil},
	} {
		allowed, gated := ActionFor(gonkcfg.Effective{Actions: all}, c.trigger)
		if !allowed || gated != c.gated {
			t.Errorf("%s with every action on: allowed=%v gated=%v, want true/%v", c.trigger, allowed, gated, c.gated)
		}
		if c.off == nil {
			continue
		}
		// Only its own action off: denied.
		a := all
		c.off(&a)
		if allowed, gated := ActionFor(gonkcfg.Effective{Actions: a}, c.trigger); allowed || !gated {
			t.Errorf("%s with its own action off and every other on: allowed=%v gated=%v, want false/true", c.trigger, allowed, gated)
		}
		// Only its own action on: allowed.
		var only gonkcfg.Actions
		switch c.trigger {
		case atags.TriggerPipelineDiagnosis:
			only.Pipelines = true
		case atags.TriggerPipelineFix:
			only.PipelineFixes = true
		case atags.TriggerIssueChange:
			only.Features = true
		case atags.TriggerMRReview:
			only.Reviews = true
		default:
			only.Triage = true
		}
		if allowed, _ := ActionFor(gonkcfg.Effective{Actions: only}, c.trigger); !allowed {
			t.Errorf("%s with only its own action on: denied", c.trigger)
		}
	}
	if allowed, gated := ActionFor(gonkcfg.Effective{Actions: all}, "no-such-trigger"); allowed || !gated {
		t.Errorf("an unknown trigger: allowed=%v gated=%v, want false/true", allowed, gated)
	}
}
