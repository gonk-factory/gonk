// Package rung is gonk's wait-vs-spend brain: given a project's resolved
// config, its spend, and its attempt history, decide which ladder rung the next
// session attempt runs at -- or that it must wait, or that it must not run.
//
// Decide is a PURE FUNCTION. No network, no database, no time.Now() (the clock
// is an input). There are NO LLM judges anywhere in this package and there must
// never be: escalation is earned by failing an objective gate, not by a model's
// opinion (spec 6.3). Every decision is therefore reproducible from its inputs
// and exhaustively table-testable with zero infrastructure.
package rung

import (
	"fmt"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// Kind is what to do next.
type Kind string

const (
	// Run: spawn the attempt at Decision.Rung.
	Run Kind = "run"
	// Defer: not now. The bead parks in waiting-for-capacity and is retried at
	// RetryAfter (spec 6.2). Every Defer carries a RetryAfter -- a park with no
	// wake-up is a lost work item.
	Defer Kind = "defer"
	// Deny: this attempt must not run, and retrying will not change that. The
	// project is disabled, its config is invalid, or it has run out of ladder or
	// per-task budget. Spec 6.2 names only "rung or defer"; Deny exists because
	// parking a permanently-blocked bead in waiting-for-capacity is an infinite
	// loop, not a policy. See ADR-004.
	Deny Kind = "deny"
)

// Machine-readable decision reasons. A BOUNDED set on purpose: they are
// Prometheus label values AND part of the /v1/policy/decide wire contract.
//
// They are ALIASES of pkg/meterapi's constants, not copies. Two independent
// lists of the same bounded set is how one of them silently drifts, and the one
// that drifts is whichever the dashboards are not built on.
const (
	ReasonNotRegistered          = meterapi.ReasonNotRegistered
	ReasonDisabled               = meterapi.ReasonDisabled
	ReasonInvalidConfig          = meterapi.ReasonInvalidConfig
	ReasonActionNotAllowed       = meterapi.ReasonActionNotAllowed
	ReasonKeyMissing             = meterapi.ReasonKeyMissing
	ReasonQuietHours             = meterapi.ReasonQuietHours
	ReasonSpendStale             = meterapi.ReasonSpendStale
	ReasonLadderExhausted        = meterapi.ReasonLadderExhausted
	ReasonInfraRetriesExhausted  = meterapi.ReasonInfraRetriesExhausted
	ReasonPerTaskTokensExhausted = meterapi.ReasonPerTaskTokensExhausted
	ReasonMonthlyTokensExhausted = meterapi.ReasonMonthlyTokensExhausted
	ReasonMonthlyCostExhausted   = meterapi.ReasonMonthlyCostExhausted
)

// ActionFor maps an atags trigger onto the .gonk.yml action that gates it.
// Returning false means "no action gates this trigger".
//
// The spec does not state this mapping (see AD-5), and
// meter has to have one: /decide is the ONLY chokepoint before a session
// spawns, so if meter does not check Effective.Actions, `actions.triage: false`
// -- a documented veto surface in ADR-002 -- means nothing at all.
//
// `onboarding` is gated by NOTHING on purpose: the onboarding MR is
// deterministic and spends zero tokens (spec 5.3), and it must work on a
// project that has not opted into anything yet -- that is the entire point of it.
func ActionFor(eff gonkcfg.Effective, trigger string) (allowed bool, gated bool) {
	switch trigger {
	case atags.TriggerIssueTriage, atags.TriggerMentionReply, atags.TriggerScaffold:
		return eff.Actions.Triage, true
	case atags.TriggerOnboarding:
		return true, false
	}
	return false, true // an unknown trigger is not allowed to do anything
}

// Input is everything Decide is allowed to look at. If it is not in here, it
// cannot influence the decision -- which is the point.
type Input struct {
	// Effective MUST come from gonkcfg.Resolve. Per ADR-002 it is a plain
	// struct with no constructor, so a hand-built one silently violates every
	// invariant the resolver establishes -- which is exactly why Invalid is a
	// separate flag below rather than "a zero Effective".
	Effective  gonkcfg.Effective
	Catalog    map[string]opercfg.RungSpec
	Registered bool // meter has a registration for this project
	// Invalid: the project's .gonk.yml would not load, so Effective is not
	// meaningful. InvalidDetail carries the loader's error.
	Invalid       bool
	InvalidDetail string
	KeyReady      bool // the LiteLLM virtual key exists

	Trigger string // atags trigger; gates against Effective.Actions

	Prior []Attempt    // this bead's attempt history, oldest first
	Spend budget.Spend // observed + reserved, for THIS project and THIS bead

	SpendAsOf time.Time    // when the spend snapshot was last synced
	Now       time.Time    // the clock, as an input
	Window    spend.Window // the budget window in force

	QuietHours      *QuietHours
	MaxSpendStale   time.Duration
	KeyRetryBackoff time.Duration
	MaxInfraRetries int
}

// Decision is the answer. Invariants (asserted in the tests): a Run always
// names a Rung and carries no Reason; a Defer always carries a RetryAfter and a
// Reason; a Deny always carries a Reason.
type Decision struct {
	Kind       Kind      `json:"decision"`
	Rung       string    `json:"rung"`
	Model      string    `json:"model,omitempty"`
	Attempt    int       `json:"attempt"`
	Reason     string    `json:"reason"`
	Detail     string    `json:"detail"`
	RetryAfter time.Time `json:"retry_after,omitzero"`
}

// Decide is the whole policy. Read it top to bottom: the gates are ordered from
// "nothing can help" to "later might".
func Decide(in Input) Decision {
	attempt := len(in.Prior) + 1
	deny := func(reason, detail string) Decision {
		return Decision{Kind: Deny, Attempt: attempt, Reason: reason, Detail: detail}
	}
	deferTo := func(reason, detail string, until time.Time) Decision {
		// A defer with a RetryAfter in the past (or the zero time -- e.g. a
		// budget Window that has not been synced yet) is an infinite park: the
		// caller wakes immediately, gets the same defer, and spins. Clamp it.
		if !until.After(in.Now) {
			until = in.Now.Add(in.MaxSpendStale)
		}
		return Decision{Kind: Defer, Attempt: attempt, Reason: reason, Detail: detail, RetryAfter: until}
	}

	// 1. Is this project allowed to run at all? Retrying cannot fix any of these.
	if !in.Registered {
		return deny(ReasonNotRegistered, "no registration in gonk-meter; onboard the project first")
	}
	if in.Invalid {
		// The .gonk.yml would not load, so Effective is a zero value and means
		// nothing. Say the true thing rather than reporting a bogus "disabled".
		return deny(ReasonInvalidConfig, in.InvalidDetail)
	}
	if !in.Effective.Enabled {
		// gonkcfg already worked out WHY, and its invariant is that
		// DisabledReason is non-empty exactly when Enabled is false (ADR-002).
		return deny(ReasonDisabled, in.Effective.DisabledReason)
	}
	// The action veto. Without this, `actions.triage: false` -- at any layer --
	// is decoration: /decide is the only thing standing between a trigger and a
	// metered session.
	if allowed, gated := ActionFor(in.Effective, in.Trigger); gated && !allowed {
		return deny(ReasonActionNotAllowed,
			fmt.Sprintf("trigger %q is not enabled for this project", in.Trigger))
	}

	// 2. Is the door even there? No key, no metered call, no session. The
	//    reconcile loop retries provisioning, so this is a wait, not a refusal.
	if !in.KeyReady {
		return deferTo(ReasonKeyMissing, "LiteLLM virtual key not provisioned yet",
			in.Now.Add(in.KeyRetryBackoff))
	}

	// 3. Quiet hours. A wait-vs-spend decision, so it lands here (spec 5.4).
	if in.QuietHours != nil {
		if end, quiet := in.QuietHours.EndAfter(in.Now); quiet {
			return deferTo(ReasonQuietHours,
				fmt.Sprintf("quiet hours until %s", end.Format(time.RFC3339)), end)
		}
	}

	// 4. Where are we on the ladder? The rung index is the count of GATE
	//    failures. Infra failures retry the same rung -- a flaky pod is not
	//    evidence that a bigger model is needed (spec 6.3).
	idx := Escalations(in.Prior)
	if idx >= len(in.Effective.Ladder) {
		return deny(ReasonLadderExhausted,
			fmt.Sprintf("%d rungs, %d gate failures", len(in.Effective.Ladder), idx))
	}
	target := in.Effective.Ladder[idx]
	spec, ok := in.Catalog[target]
	if !ok {
		return deny(ReasonInvalidConfig,
			fmt.Sprintf("rung %q is not in the operator rung catalog", target))
	}
	if n := ConsecutiveInfraFailures(in.Prior, target); n >= in.MaxInfraRetries {
		return deny(ReasonInfraRetriesExhausted,
			fmt.Sprintf("rung %q failed on infrastructure %d times in a row", target, n))
	}

	// 5. Money. Every check below is against ceiling - (observed + RESERVED):
	//    the reservation is what makes this survive spend-log lag and two
	//    sessions racing the same headroom.
	bud := budget.FromEffective(in.Effective.Budget)
	rem := budget.Remain(bud, in.Spend)

	// Stale spend is not a number we may decide on. A project with no ceilings
	// at all has nothing to be stale about, so it runs.
	if !bud.AllUnlimited() && in.Now.Sub(in.SpendAsOf) > in.MaxSpendStale {
		return deferTo(ReasonSpendStale,
			fmt.Sprintf("spend snapshot is %s old (max %s)",
				in.Now.Sub(in.SpendAsOf).Round(time.Second), in.MaxSpendStale),
			in.SpendAsOf.Add(in.MaxSpendStale))
	}

	// Per-task tokens are a property of the WORK ITEM. A month rollover will not
	// refill them, so this is a deny, not a defer.
	if !rem.FitsTaskTokens(int64(spec.EstTokens)) {
		return deny(ReasonPerTaskTokensExhausted,
			fmt.Sprintf("bead needs ~%d tokens, %s remain of its per-task budget",
				spec.EstTokens, tokenStr(rem.PerTaskTokens)))
	}
	// Monthly ceilings DO refill. Park until the window rolls (spec 6.2).
	if !rem.FitsMonthTokens(int64(spec.EstTokens)) {
		return deferTo(ReasonMonthlyTokensExhausted,
			fmt.Sprintf("rung %q needs ~%d tokens, %s remain this window",
				target, spec.EstTokens, tokenStr(rem.MonthlyTokens)), in.Window.End)
	}
	// The cost gate applies ONLY to rungs with a positive REAL price. opercfg
	// guarantees cloud rungs have one (est_cost_usd > 0) and local rungs do not
	// (est_cost_usd == 0), so "monthly_cost_usd: 0 means local rungs only,
	// forever" falls straight out of the arithmetic -- which is exactly the
	// conservative default the onboarding MR ships (spec 5.3).
	//
	// *** THIS IS WHERE DECISION 9 COULD HAVE BROKEN EVERYTHING, AND MUST NOT. ***
	// Local rungs are now priced SYNTHETICALLY in LiteLLM, so that its USD
	// virtual-key ceiling is a hard door for tokens too. Those synthetic dollars
	// live in budget.Spend.SyntheticCostUSD and are NOT visible to rem.FitsCost,
	// which reads real dollars only. If they ever leaked in here, an onboarded
	// project (monthly_cost_usd: 0, ladder: [qwen-local]) could not afford its
	// own only rung and would be DEAD ON ARRIVAL. Do not "unify" the two.
	//
	// Note we do NOT fall back to a cheaper rung here. The cheaper rungs already
	// failed their gate; re-running them would loop. Spec 6.3: "defer applies
	// when only-cloud-rungs-remain and budget is exhausted."
	if spec.EstCostUSD > 0 && !rem.FitsCost(spec.EstCostUSD) {
		return deferTo(ReasonMonthlyCostExhausted,
			fmt.Sprintf("rung %q needs $%.2f, $%s remain this window",
				target, spec.EstCostUSD, costStr(rem.MonthlyCostUSD)), in.Window.End)
	}

	return Decision{Kind: Run, Rung: target, Model: spec.Model, Attempt: attempt}
}

// Reserve is what a Run decision commits: the estimated consumption held against
// the ceilings until the spend log catches up or the reservation expires.
//
// It returns BOTH currencies. costUSD is real money and is what the cost gate
// will see next time (zero for a local rung). syntheticUSD is what LiteLLM's
// virtual-key counter will actually be charged for a local rung (zero for a
// cloud rung, whose real price already is that charge). They are held separately
// and they never mix.
func Reserve(spec opercfg.RungSpec) (costUSD, syntheticUSD float64, tokens int64) {
	tokens = int64(spec.EstTokens)
	if spec.Kind == opercfg.KindLocal {
		return 0, spec.PricePerToken() * float64(tokens), tokens
	}
	return spec.EstCostUSD, 0, tokens
}

func tokenStr(t budget.TokenLimit) string {
	if t.Unlimited() {
		return "unlimited"
	}
	return fmt.Sprintf("%d", int64(t))
}

func costStr(c budget.CostLimit) string {
	if c.Unlimited() {
		return "unlimited"
	}
	return fmt.Sprintf("%.2f", float64(c))
}
