package pipefix

import (
	"github.com/gonk-factory/gonk/pkg/gate"
)

// Verdict is J5's answer (plan 6.3). The zero value is not a verdict: a
// Result nobody decided can never read as one.
type Verdict string

// The verdicts, in J5's first-match order.
const (
	// VerdictForged: the change altered what it is judged by (a definition
	// violation, J2) or a diagnosed job was allowed to fail (WEAKENED, J3).
	VerdictForged Verdict = "FORGED"
	// VerdictNotFixed: a diagnosed job fails the same way (RECURS) and never
	// passed.
	VerdictNotFixed Verdict = "NOT_FIXED"
	// VerdictFlaky: a diagnosed job both failed and succeeded across its
	// attempts. Never PASSED, whoever retried it.
	VerdictFlaky Verdict = "FLAKY"
	// VerdictUnverifiable: the criterion could not be observed (the job did
	// not run, the pipeline never settled, the definition cannot be
	// compared, a fact stayed unreadable past the deadline). Ours, not the
	// agent's fault -- and not a pass.
	VerdictUnverifiable Verdict = "UNVERIFIABLE"
	// VerdictRegressed: a job that passed on the base pipeline now fails.
	VerdictRegressed Verdict = "REGRESSED"
	// VerdictFixedNewFailure: the diagnosed failure is gone and something
	// else fails -- the same job differently (owner decision Q3), a failure
	// that was already there, or one the fix revealed. The criterion is MET,
	// on a red pipeline.
	VerdictFixedNewFailure Verdict = "FIXED_NEW_FAILURE"
	// VerdictFixed: every diagnosed job passed on every attempt and every
	// candidate pipeline succeeded.
	VerdictFixed Verdict = "FIXED"
)

// Verdicts returns the closed set in J5's first-match order.
func Verdicts() []Verdict {
	return []Verdict{VerdictForged, VerdictNotFixed, VerdictFlaky, VerdictUnverifiable,
		VerdictRegressed, VerdictFixedNewFailure, VerdictFixed}
}

// State is a fix state of plan 7.1.
type State string

// The fix states of plan 7.1, in the table's order.
const (
	StateProposing       State = "proposing"
	StateNeedsScope      State = "needs-scope"
	StateAwaitingRun     State = "awaiting-run"
	StateVerifying       State = "verifying"
	StateFixed           State = "fixed"
	StateFixedNewFailure State = "fixed-new-failure"
	StateNotFixed        State = "not-fixed"
	StateFlaky           State = "flaky"
	StateUnverifiable    State = "unverifiable"
	StateRegressed       State = "regressed"
	StateDenied          State = "denied"
	StateTakenOver       State = "taken-over"
	StateClosedUnmerged  State = "closed-unmerged"
	StateMerged          State = "merged"
	StateConfirmed       State = "confirmed"
	StateRecurred        State = "recurred"
)

// States returns the closed set in plan 7.1's order.
func States() []State {
	return []State{StateProposing, StateNeedsScope, StateAwaitingRun, StateVerifying,
		StateFixed, StateFixedNewFailure, StateNotFixed, StateFlaky, StateUnverifiable,
		StateRegressed, StateDenied, StateTakenOver, StateClosedUnmerged, StateMerged,
		StateConfirmed, StateRecurred}
}

// KnownState reports whether s is one of States.
func KnownState(s State) bool {
	for _, k := range States() {
		if k == s {
			return true
		}
	}
	return false
}

// Terminal is plan 7.1's "Terminal?" column: true for a state the workflow
// does not leave by itself. `fixed` and `fixed-new-failure` ("until merge")
// are terminal here: the fix was judged and waits for a merge. Not terminal:
// proposing, awaiting-run, verifying, and not-fixed (the next attempt, or
// denied). False for a state outside the closed set.
func (s State) Terminal() bool {
	switch s {
	case StateProposing, StateAwaitingRun, StateVerifying, StateNotFixed:
		return false
	}
	return KnownState(s)
}

// StateFor is J5's verdict -> fix state (plan 7.1). FORGED and NOT_FIXED
// share `not-fixed`. false for anything that is not a Verdict.
func StateFor(v Verdict) (State, bool) {
	switch v {
	case VerdictForged, VerdictNotFixed:
		return StateNotFixed, true
	case VerdictFlaky:
		return StateFlaky, true
	case VerdictUnverifiable:
		return StateUnverifiable, true
	case VerdictRegressed:
		return StateRegressed, true
	case VerdictFixedNewFailure:
		return StateFixedNewFailure, true
	case VerdictFixed:
		return StateFixed, true
	}
	return "", false
}

// ArtifactPresent is plan 7.3's meter signal for a fix state: the value of
// gate.Signals.ArtifactPresent the attempt is reported with. reported is false
// for a state that reports no outcome from the exit check -- the attempt is
// still in flight (proposing, awaiting-run, verifying), was ended by meter
// itself (denied), or was already reported (confirmed, recurred).
//
//	needs-scope, fixed, fixed-new-failure, flaky, unverifiable, regressed,
//	taken-over, closed-unmerged, merged      -> present  (success)
//	not-fixed (NOT_FIXED and FORGED)         -> absent   (gate-failed if paid)
//
// "present" means "an in-scope MR exists and more tokens cannot answer the
// open question", not "the pipeline is fixed" (7.3, said plainly there).
// `merged` is not a row of 7.3's table; see the plan's evidence log.
func ArtifactPresent(s State) (present, reported bool) {
	switch s {
	case StateNotFixed:
		return false, true
	case StateNeedsScope, StateFixed, StateFixedNewFailure, StateFlaky, StateUnverifiable,
		StateRegressed, StateTakenOver, StateClosedUnmerged, StateMerged:
		return true, true
	}
	return false, false
}

// MeterOutcome is plan 7.3 as a pure function: the meterapi outcome a fix
// state is reported with, given the tokens meter's ledger shows for the
// session. It goes through gate.Classify, so "an escalation must be paid
// for" stays single-sourced: not-fixed with zero tokens is infra-failed, not
// gate-failed. false when the state reports no outcome (ArtifactPresent).
//
// This covers the rows of 7.3 that follow from a fix state. The Phase A
// refusals and the infra rows (ArtifactUnknown, an expired reservation) are
// signals of their own that the caller sets on gate.Signals.
func MeterOutcome(s State, modelTokens int64) (string, bool) {
	present, reported := ArtifactPresent(s)
	if !reported {
		return "", false
	}
	return gate.Classify(gate.Signals{ArtifactPresent: present, ModelTokens: modelTokens}), true
}

// VerdictOutcome is MeterOutcome for a verdict: StateFor, then MeterOutcome.
func VerdictOutcome(v Verdict, modelTokens int64) (string, bool) {
	s, known := StateFor(v)
	if !known {
		return "", false
	}
	return MeterOutcome(s, modelTokens)
}
