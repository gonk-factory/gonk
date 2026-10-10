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

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/budget"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/spend"
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
	ReasonCloudNotAllowed        = meterapi.ReasonCloudNotAllowed
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
	case atags.TriggerPipelineDiagnosis:
		// Gated by its OWN action, not by triage: a project may diagnose
		// pipelines without triaging issues, and vice versa (gonk-tios D5).
		return eff.Actions.Pipelines, true
	case atags.TriggerPipelineFix:
		// Gated by its OWN action (gonk-2usw 3.4): diagnosing pipelines does
		// not authorise changing the repository. gonkcfg.Resolve makes
		// PipelineFixes false whenever Pipelines is, so a veto of diagnoses
		// is a veto of fixes too.
		return eff.Actions.PipelineFixes, true
	case atags.TriggerIssueChange:
		// Gated by actions.features (gonk-cuwn plan 2.1): the action that
		// authorises turning an issue into a merge request. WHO asked, and in
		// which form, is the dispatcher's check; this is the project's opt-in.
		return eff.Actions.Features, true
	case atags.TriggerMRReview:
		// Gated by its OWN action (gonk-cuwn plan 3.1): authoring merge
		// requests does not authorise spending on a second session to read
		// them. gonkcfg.Resolve makes Reviews false when neither author
		// workflow is on.
		return eff.Actions.Reviews, true
	case atags.TriggerOnboarding:
		return true, false
	}
	return false, true // an unknown trigger is not allowed to do anything
}

// LadderFloors is the per-trigger ladder floor: the index into the project's
// ladder at which a trigger's FIRST attempt runs. A trigger absent from the map
// has floor 0, i.e. the cheapest rung, exactly as before -- so every existing
// trigger's decisions are unchanged.
//
// pipeline-diagnosis starts at index 1: the rung at index 0 is a 16K-token
// window, too small for a diagnosis prompt (owner decision 2026-10-04, Q1 of
// the gonk-tios plan). The floor shifts WHICH rung attempt 1 uses; it does not
// burn an attempt (attempts still count from 1), and escalation proceeds from
// the floor (see Position for the exact arithmetic, and FloorRetries for the
// tries a trigger gets at its floor before it escalates).
//
// pipeline-fix starts at index 1 for the same reason: its prompt carries the
// content of the files it may edit (gonk-2usw 3.4).
//
// issue-change starts at index 1 too (gonk-cuwn plan 2.9, owner decision Q3:
// the 131072-token rungs are the v1 design window; a change prompt plus its
// file reads does not fit the 16K one). mr-review is absent on purpose: it
// does not use the project's ladder at all (see LadderFor).
var LadderFloors = map[string]int{
	atags.TriggerPipelineDiagnosis: 1,
	atags.TriggerPipelineFix:       1,
	atags.TriggerIssueChange:       1,
}

// LadderFloor returns the ladder floor for a trigger (0 if none is named).
func LadderFloor(trigger string) int { return LadderFloors[trigger] }

// FloorRetries is the per-trigger count of EXTRA tries at the trigger's floor
// rung: gate failures that re-run the SAME rung before any escalation. A
// trigger absent from the map has 0, so its first gate failure escalates,
// exactly as before -- every existing trigger's decisions are unchanged.
//
// pipeline-diagnosis has 1 (owner decision 2026-10-04, R2 of the gonk-tios
// verify report). Without it a diagnosis on [local, local, paid] ran its floor
// rung once, and ONE gate failure -- a single mis-escaped quote -- escalated to
// the paid rung, which a $0-cloud project is denied: the run ended `denied`
// after one try. With it the first gate failure re-runs the floor rung and only
// the second escalates.
//
// pipeline-fix has 1 as well (gonk-2usw 3.4): a mis-escaped batch must not buy
// a paid rung.
//
// issue-change and mr-review have 1 each (gonk-cuwn plan 2.1, 3.4). For
// mr-review that one retry is ALL there is: its ladder has one rung, so a
// second refused batch ends the review `denied` -- a veto must not buy rung
// spend.
var FloorRetries = map[string]int{
	atags.TriggerPipelineDiagnosis: 1,
	atags.TriggerPipelineFix:       1,
	atags.TriggerIssueChange:       1,
	atags.TriggerMRReview:          1,
}

// FloorRetry returns the extra tries a trigger gets at its floor rung (0 if
// none is named; a negative entry is read as 0).
func FloorRetry(trigger string) int { return max(FloorRetries[trigger], 0) }

// ReservationTTL is how long meter holds the reservation for ONE attempt of a
// trigger: base (the operator's meter.reservation_ttl) for every trigger but
// pipeline-fix, so every existing trigger's reservations are unchanged.
//
// pipeline-fix gets base + the project's effective pipeline_fix.verify_timeout
// (gonk-2usw 3.4, 7.3). A fix attempt is not judged when its session ends: the
// outcome is reported after the fix merge request's pipeline, which the exit
// check waits up to verify_timeout for. With the base TTL alone the
// reservation would expire first and every slow-but-correct fix would be
// booked as infra-failed. A reservation that outlives even this still expires,
// as today.
//
// The addition is bounded: verify_timeout is clamped to
// [0, gonkcfg.FixVerifyTimeoutCeiling] here as well as where it is resolved,
// so no project value can hold headroom longer than base + the ceiling.
//
// issue-change gets base + the project's effective change.verify_timeout, for
// the same reason and with the same clamp (gonk-cuwn plan 2.5: the change is
// judged on its merge request's pipeline). mr-review gets base: a review is
// judged when its session ends.
func ReservationTTL(trigger string, base time.Duration, eff gonkcfg.Effective) time.Duration {
	if trigger == atags.TriggerIssueChange {
		return base + min(max(eff.Change.VerifyTimeout, 0), gonkcfg.ChangeVerifyTimeoutCeiling)
	}
	if trigger != atags.TriggerPipelineFix {
		return base
	}
	return base + min(max(eff.PipelineFix.VerifyTimeout, 0), gonkcfg.FixVerifyTimeoutCeiling)
}

// LadderFor is the ladder a trigger's attempts run on.
//
// For every trigger but mr-review it is the project's effective ladder,
// unchanged. For mr-review it is the ONE rung the operator named as the
// reviewer (`review.rung`), and nothing else: which model reviews is an
// operator setting a project cannot influence, and a review never climbs
// (gonk-cuwn plan 3.2 N5, owner decision Q4). With no reviewer rung it is
// empty, and Decide denies.
func LadderFor(eff gonkcfg.Effective, trigger string) []string {
	if trigger != atags.TriggerMRReview {
		return eff.Ladder
	}
	if eff.Review.Rung == "" {
		return nil
	}
	return []string{eff.Review.Rung}
}

// ResolveReviewMerge completes Effective.Review's two merge statuses with what
// only the rung catalog knows: whether a reviewer on a model DISTINCT from the
// author's is configured at all. Meter calls it once, right after
// gonkcfg.Resolve; a status Resolve already settled (reviews off, no reviewer
// rung) is kept.
//
// For each author workflow, in order:
//
//   - the reviewer rung is not in the catalog: unavailable,
//     review-rung-not-in-catalog;
//   - the project ladder has no rung at or above the workflow's floor:
//     unavailable, no-author-rung;
//   - at least one rung the author can run on has a model other than the
//     reviewer's: available. Otherwise unavailable, no-distinct-model.
//
// "Available" means the configuration CAN produce a distinct-model review, not
// that a given merge request got one: that is decided per merge request from
// the recorded models (plan 5.1 M9). It is never inferred that a model is
// adequate for review; the operator says so by naming review.rung.
func ResolveReviewMerge(eff gonkcfg.Effective, catalog map[string]opercfg.RungSpec) gonkcfg.Effective {
	status := func(have gonkcfg.ReviewMergeStatus, authorTrigger string) gonkcfg.ReviewMergeStatus {
		if have.Reason != gonkcfg.ReviewMergeModelsNotCompared {
			return gonkcfg.ReviewMergeStatus{Reason: have.Reason}
		}
		reviewer, ok := catalog[eff.Review.Rung]
		if !ok {
			return gonkcfg.ReviewMergeStatus{Reason: gonkcfg.ReviewMergeRungUnknown}
		}
		floor := LadderFloor(authorTrigger)
		if floor >= len(eff.Ladder) {
			return gonkcfg.ReviewMergeStatus{Reason: gonkcfg.ReviewMergeNoAuthorRung}
		}
		for _, r := range eff.Ladder[floor:] {
			if spec, known := catalog[r]; known && spec.Model != reviewer.Model {
				return gonkcfg.ReviewMergeStatus{Available: true}
			}
		}
		return gonkcfg.ReviewMergeStatus{Reason: gonkcfg.ReviewMergeNoDistinctModel}
	}
	eff.Review.PipelineFixMerge = status(eff.Review.PipelineFixMerge, atags.TriggerPipelineFix)
	eff.Review.ChangeMerge = status(eff.Review.ChangeMerge, atags.TriggerIssueChange)
	return eff
}

// Position is where a trigger's NEXT attempt stands on the ladder, and why. It
// is the whole rung arithmetic, in one pure place, so that Decide and meter's
// advisory /outcome preview cannot disagree about it.
//
// THE INVARIANT (pkg/rung/outcome.go: "rung N is earned by failing the gate N
// times"), stated for a trigger with floor F and floor retries R, after G gate
// failures:
//
//	RetriesUsed = min(G, R)      gate failures spent re-running the floor rung
//	Climbs      = max(G - R, 0)  gate failures that each moved one rung up
//	Index       = F + Climbs     the ladder index the next attempt runs at
//
// so every gate failure is accounted for exactly once (G = RetriesUsed +
// Climbs): a floor retry is PAID FOR by a gate failure and is not also an
// escalation, and a rung above the floor is still earned by exactly one gate
// failure each. For every trigger with F = 0 and R = 0 -- all of them but
// pipeline-diagnosis -- this is Index = G, the old rule, unchanged.
//
// Only gate failures move any of these numbers. Infra failures, successes and
// aborts are not counted here at all.
type Position struct {
	Floor        int // LadderFloor(trigger)
	Retries      int // FloorRetry(trigger)
	GateFailures int // Escalations(prior)
	RetriesUsed  int
	Climbs       int
	Index        int
}

// LadderPosition computes the Position of the attempt that follows prior.
func LadderPosition(trigger string, prior []Attempt) Position {
	p := Position{
		Floor:        LadderFloor(trigger),
		Retries:      FloorRetry(trigger),
		GateFailures: Escalations(prior),
	}
	p.RetriesUsed = min(p.GateFailures, p.Retries)
	p.Climbs = p.GateFailures - p.RetriesUsed
	p.Index = p.Floor + p.Climbs
	return p
}

// FloorRetrying reports whether the next attempt is a floor retry: it re-runs
// the floor rung because of a gate failure, and has not climbed.
func (p Position) FloorRetrying() bool { return p.RetriesUsed > 0 && p.Climbs == 0 }

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

	// CloudAllowed carries opercfg.OperatorConfig.CloudAllowed() -- the human-
	// approved permission to cross into PAID cloud rungs. Default false. When the
	// next rung is a cloud rung and this is false, Decide denies to needs-human
	// rather than spend real money (spec 7.1). It has NO effect on local rungs.
	CloudAllowed bool

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
	// MaxTurns is the target rung's per-attempt turn cap, carried onto a Run so
	// the session is bounded by the rung's budget (stingier for cloud). Zero on a
	// defer/deny, which spawns no session.
	MaxTurns int `json:"max_turns,omitempty"`
	// Escalated marks a Run that CLIMBED the ladder (a gate failure pushed it past
	// the cheapest rung). It is purely for logging -- meter records that an
	// escalation happened; it does not change what runs. Never set on the first
	// rung, nor on a defer/deny.
	Escalated bool `json:"escalated,omitempty"`
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
	//    failures (from the trigger's floor, after its floor retries -- see
	//    Position; both are 0 for every trigger but the two pipeline ones). Infra
	//    failures retry the same rung -- a flaky pod is not evidence that a
	//    bigger model is needed (spec 6.3).
	// The ladder is the project's for every trigger but mr-review, whose
	// ladder is the operator's one reviewer rung (LadderFor).
	ladder := LadderFor(in.Effective, in.Trigger)
	if in.Trigger == atags.TriggerMRReview && len(ladder) == 0 {
		return deny(ReasonLadderExhausted,
			"no reviewer rung: the operator configuration names no review.rung (or two layers name different ones), so no model is declared adequate for review")
	}
	pos := LadderPosition(in.Trigger, in.Prior)
	floor, gateFailures, idx := pos.Floor, pos.GateFailures, pos.Index
	// special is true only for a trigger with a floor or floor retries. Every
	// branch it guards is unreachable for the other triggers, whose details and
	// decisions are therefore byte-for-byte what they were.
	special := floor > 0 || pos.Retries > 0
	if floor > 0 && len(ladder) <= floor {
		// The ladder has no rung at the floor. Say so; an index panic or a
		// silent fall back to a rung too small for the work are both worse.
		return deny(ReasonLadderExhausted,
			fmt.Sprintf("trigger %q starts at ladder rung %d, but the project ladder has only %d rung(s)",
				in.Trigger, floor+1, len(ladder)))
	}
	if idx >= len(ladder) {
		if special {
			return deny(ReasonLadderExhausted,
				fmt.Sprintf("%d rungs, trigger %q starts at rung %d, %d gate failures%s",
					len(ladder), in.Trigger, floor+1, gateFailures, pos.retryClause()))
		}
		return deny(ReasonLadderExhausted,
			fmt.Sprintf("%d rungs, %d gate failures", len(ladder), idx))
	}
	target := ladder[idx]
	spec, ok := in.Catalog[target]
	if !ok {
		return deny(ReasonInvalidConfig,
			fmt.Sprintf("rung %q is not in the operator rung catalog", target))
	}
	if n := ConsecutiveInfraFailures(in.Prior, target); n >= in.MaxInfraRetries {
		return deny(ReasonInfraRetriesExhausted,
			fmt.Sprintf("rung %q failed on infrastructure %d times in a row", target, n))
	}

	// 4b. The cost-class gate (spec 7.1). It sits IN FRONT OF the money section:
	// crossing into a PAID cloud rung needs a human-approved allowance, full stop
	// -- we never let the budget arithmetic alone authorise real spend on a model's
	// say-so. A local rung is free and passes straight through (its escalation is
	// logged, below). With no allowance, a cloud rung DENIES to needs-human here,
	// before any reservation is contemplated, so no headroom is minted for a run
	// that will not happen. This does NOT touch the reservation arithmetic (N4);
	// it decides whether that arithmetic is even reached.
	if spec.Kind == opercfg.KindCloud && !in.CloudAllowed {
		return deny(ReasonCloudNotAllowed,
			fmt.Sprintf("rung %q is a paid cloud rung and no cloud allowance is granted for this scope; a human must approve crossing into paid spend", target))
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

	detail := ""
	if special {
		// Visible in /decide: which rung was chosen and why -- the floor, where
		// this attempt stands among the floor retries, and the gate failures.
		// Empty for a trigger with neither, so every existing trigger's response
		// is unchanged.
		detail = fmt.Sprintf("rung %q (ladder position %d of %d): trigger %q starts at ladder position %d, %d gate failure(s)%s",
			target, idx+1, len(ladder), in.Trigger, floor+1, gateFailures, pos.retryClause())
	}
	return Decision{
		Kind:    Run,
		Rung:    target,
		Model:   spec.Model,
		Attempt: attempt,
		Detail:  detail,
		// The rung's turn cap rides the decision so the session is bounded by it.
		MaxTurns: spec.EffectiveMaxTurns(),
		// Climbs is the count of gate failures that moved a rung UP; > 0 means this
		// attempt has climbed the ladder past where the trigger starts, i.e. it is
		// an escalation worth logging (both local->local and an allowed cloud
		// crossing). A floor retry re-runs the same rung and is NOT one. For a
		// trigger with no floor retries Climbs is the gate-failure count, as before.
		Escalated: pos.Climbs > 0,
	}
}

// retryClause is the floor-retry part of a detail string: empty for a trigger
// with no floor retries (so a floor-only trigger's detail is what P4 wrote).
func (p Position) retryClause() string {
	switch {
	case p.Retries == 0:
		return ""
	case p.FloorRetrying():
		return fmt.Sprintf(", floor retry %d of %d (the same rung again, not an escalation)", p.RetriesUsed, p.Retries)
	case p.Climbs > 0:
		return fmt.Sprintf(", %d of %d floor retries used, then %d escalation(s)", p.RetriesUsed, p.Retries, p.Climbs)
	default:
		return fmt.Sprintf(", 0 of %d floor retries used", p.Retries)
	}
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
