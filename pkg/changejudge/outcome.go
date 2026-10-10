package changejudge

import (
	"github.com/gonk-factory/gonk/pkg/gate"
)

// Verdict is K5's answer (plan 2.5). The zero value is not a verdict: a
// Result nobody decided can never read as one.
type Verdict string

// The verdicts, in K5's first-match order.
const (
	// VerdictNotGreen: a job that must pass failed on every attempt.
	VerdictNotGreen Verdict = "NOT_GREEN"
	// VerdictTestsNotRun: the pipeline is green and the test report shows
	// the change's new tests were not collected.
	VerdictTestsNotRun Verdict = "TESTS_NOT_RUN"
	// VerdictFlaky: a job both failed and succeeded across its attempts.
	// Never green, whoever retried it.
	VerdictFlaky Verdict = "FLAKY"
	// VerdictUnverifiable: the pipeline could not be observed green on the
	// base's definition (it never settled, the definition differs, a status
	// nothing explains, a fact unreadable past the deadline), or the test
	// report contradicts the green (the suite shrank, a case newly fails),
	// or a required report is absent. Not a pass.
	VerdictUnverifiable Verdict = "UNVERIFIABLE"
	// VerdictGreenUnobserved: green, and the project publishes no test
	// report, so whether the tests ran was NOT observed.
	VerdictGreenUnobserved Verdict = "GREEN_UNOBSERVED"
	// VerdictGreen: green on gonk's exact commit on the base's definition
	// with no failed attempt; where the diff is not documentation only, the
	// suite did not shrink and the new tests ran and passed.
	VerdictGreen Verdict = "GREEN"
)

// Verdicts returns the closed set in K5's first-match order.
func Verdicts() []Verdict {
	return []Verdict{VerdictNotGreen, VerdictTestsNotRun, VerdictFlaky, VerdictUnverifiable,
		VerdictGreenUnobserved, VerdictGreen}
}

// State is a state of the change workflow.
type State string

// The change states: the ones plan 2.5's outcome table names (needs-scope,
// not-verified, verified, verified-unobserved, inconclusive), K0's three
// (taken-over, closed-unmerged, merged), and the in-flight and budget states
// the workflow shares with the pipeline fix (proposing, verifying, denied).
const (
	StateProposing          State = "proposing"
	StateNeedsScope         State = "needs-scope"
	StateVerifying          State = "verifying"
	StateNotVerified        State = "not-verified"
	StateVerified           State = "verified"
	StateVerifiedUnobserved State = "verified-unobserved"
	StateInconclusive       State = "inconclusive"
	StateDenied             State = "denied"
	StateTakenOver          State = "taken-over"
	StateClosedUnmerged     State = "closed-unmerged"
	StateMerged             State = "merged"
)

// States returns the closed set.
func States() []State {
	return []State{StateProposing, StateNeedsScope, StateVerifying, StateNotVerified,
		StateVerified, StateVerifiedUnobserved, StateInconclusive, StateDenied,
		StateTakenOver, StateClosedUnmerged, StateMerged}
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

// Terminal: true for a state the workflow does not leave by itself.
// `verified` and `verified-unobserved` are terminal here: the change was
// judged and waits for a human. Not terminal: proposing, verifying, and
// not-verified (the next attempt, or denied). False for a state outside the
// closed set.
func (s State) Terminal() bool {
	switch s {
	case StateProposing, StateVerifying, StateNotVerified:
		return false
	}
	return KnownState(s)
}

// StateFor is K5's verdict -> change state (plan 2.5, outcome table). false
// for anything that is not a Verdict.
func StateFor(v Verdict) (State, bool) {
	switch v {
	case VerdictNotGreen, VerdictTestsNotRun:
		return StateNotVerified, true
	case VerdictFlaky, VerdictUnverifiable:
		return StateInconclusive, true
	case VerdictGreenUnobserved:
		return StateVerifiedUnobserved, true
	case VerdictGreen:
		return StateVerified, true
	}
	return "", false
}

// ArtifactPresent is the meter signal for a change state: the value of
// gate.Signals.ArtifactPresent the attempt is reported with. reported is false
// for a state that reports no outcome from the exit check -- the attempt is
// still in flight (proposing, verifying) or was ended by meter itself
// (denied).
//
//	needs-scope, verified, verified-unobserved, inconclusive,
//	taken-over, closed-unmerged, merged      -> present  (success)
//	not-verified (NOT_GREEN, TESTS_NOT_RUN)  -> absent   (gate-failed if paid)
//
// "present" means "an in-scope MR exists and more tokens cannot answer the
// open question", not "the change is right". taken-over, closed-unmerged and
// merged are not rows of the plan's table; they follow the pipeline fix (see
// the plan's evidence log).
func ArtifactPresent(s State) (present, reported bool) {
	switch s {
	case StateNotVerified:
		return false, true
	case StateNeedsScope, StateVerified, StateVerifiedUnobserved, StateInconclusive,
		StateTakenOver, StateClosedUnmerged, StateMerged:
		return true, true
	}
	return false, false
}

// MeterOutcome is the outcome table of plan 2.5 as a pure function: the
// meterapi outcome a change state is reported with, given the tokens meter's
// ledger shows for the session. It goes through gate.Classify, so "an
// escalation must be paid for" stays single-sourced: not-verified with zero
// tokens is infra-failed, not gate-failed. false when the state reports no
// outcome (ArtifactPresent).
//
// This covers the rows that follow from a change state. The table's first
// row (the static gate refuses the batch) and its last (ours: config, base
// read, apply) are signals of their own that the caller sets on gate.Signals.
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

// The labels of the change workflow (plan 2.5 outcome table, and section 4).
// Every one of them is reserved: no agent batch may write it. LabelImplement
// is the one a HUMAN sets; the controller never writes it.
const (
	LabelImplement       = "gonk::implement"
	LabelVerifying       = "gonk::change-verifying"
	LabelVerified        = "gonk::change-verified"
	LabelTestsObserved   = "gonk::change-tests-observed"
	LabelTestsUnobserved = "gonk::change-tests-unobserved"
	LabelInconclusive    = "gonk::change-inconclusive"
	LabelNeedsHuman      = "gonk::needs-human"
)

// MRLabels returns the labels the controller writes on the merge request.
func MRLabels() []string {
	return []string{LabelVerifying, LabelVerified, LabelTestsObserved, LabelTestsUnobserved, LabelInconclusive}
}

// Labels is what a state shows: the merge request's change label set (the
// controller removes the other MRLabels) and whether the ISSUE is labelled
// gonk::needs-human.
type Labels struct {
	MR              []string
	IssueNeedsHuman bool
}

// LabelsFor is the "MR label" column of plan 2.5's outcome table. tests is
// Result.Tests.Finding of the judgement that selected the state.
//
// `verified` carries gonk::change-tests-observed ONLY when the tests were
// observed (TestsObserved). A GREEN on a documentation-only diff is verified
// with no claim about tests either way: nothing was looked for.
func LabelsFor(s State, tests Finding) Labels {
	switch s {
	case StateNeedsScope:
		return Labels{IssueNeedsHuman: true}
	case StateVerifying, StateNotVerified:
		return Labels{MR: []string{LabelVerifying}}
	case StateVerified:
		if tests == TestsObserved {
			return Labels{MR: []string{LabelVerified, LabelTestsObserved}}
		}
		return Labels{MR: []string{LabelVerified}}
	case StateVerifiedUnobserved:
		return Labels{MR: []string{LabelVerified, LabelTestsUnobserved}}
	case StateInconclusive:
		return Labels{MR: []string{LabelInconclusive}, IssueNeedsHuman: true}
	}
	return Labels{}
}
