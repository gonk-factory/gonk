// Package verify is the verified change pipeline's verdict engine: the pure
// function that decides whether a proposed code change passed the red-green
// sequence.
//
// THERE IS NO LLM IN THIS PACKAGE, AND THERE NEVER WILL BE, for the same reason
// pkg/gate has none (spec 6.3, "no LLM judges"). Everything here is a total
// function of evidence a non-agentic runner observed. The agent is never asked
// how it did.
//
// # What the sequence proves, and what it does not
//
//	step 0: base commit alone                    -> GREEN
//	step 1: base + test diff                     -> RED, in the NEW tests, by assertion
//	step 2: base + test diff + code diff         -> GREEN
//
// This converts "did the agent actually fix the bug" from a judgement into an
// experiment: step 1 proves the test exercises the defect, step 2 proves the
// code fixes it.
//
// It was originally claimed that these three steps make test-weakening a state
// the sequence CANNOT REACH. That was false, and review found three ways
// through:
//
//	(a) delete one test, add another  -- step 1 is red in the new test, step 2
//	    green, and whatever the deleted test guarded is now unchecked;
//	(b) the CODE diff touches test files -- the original design constrained only
//	    the test diff's paths and said nothing about the inverse;
//	(c) skips and deselection -- t.Skip, build tags, conftest.py, a Makefile
//	    target. GREEN DOES NOT MEAN "PASSED"; IT MEANS "NOTHING THAT RAN FAILED."
//
// All three are closed here by one rule, Monotonicity, which is why this package
// works in TEST ID SETS rather than booleans. A runner that reported only
// pass/fail could not express any of it, and the sets could not be retrofitted
// later without rewriting the runner -- which is why this is the first slice of
// the verifier rather than the last.
//
// # The honest limit
//
// Isolation (no credentials, no network, caps, fresh sandbox) stops escape and
// exfiltration. It does NOT make this verdict trustworthy: the evidence is
// produced by a test framework running in-process with the agent's own code, and
// a hostile diff can fabricate whatever the runner parses. VERIFIER-GREEN IS
// EVIDENCE FOR A HUMAN REVIEWING A MERGE REQUEST. It is not merge authority, and
// nothing downstream may promote it into one.
package verify

import "sort"

// TestID is a test framework's own identity for a single test -- whatever the
// runner can collect stably across runs (Go's "TestFoo/subcase", pytest's
// "path::Class::test"). Its only requirement is that the SAME test yields the
// SAME id at steps 0, 1 and 2, because every rule here is set arithmetic over
// these and an unstable id silently reads as "deleted and replaced".
type TestID string

// StepResult is what the runner observed for one step of the sequence. It
// describes WHAT HAPPENED, never what it means -- all interpretation is in
// Classify, so the runner cannot express a verdict and the verdict cannot
// depend on how the runner was implemented.
type StepResult struct {
	// Collected is every test the framework discovered, whether or not it ran.
	// A test that vanishes from Collected between steps was deleted or
	// deselected -- the distinction Monotonicity exists to catch.
	Collected []TestID
	// Passed is tests that RAN AND PASSED. A skipped test is NOT passed, and
	// keeping them apart is what closes the skip loophole: a skip keeps a test
	// in Collected while removing it from Passed.
	Passed []TestID
	// Failed is tests that ran and failed an assertion.
	Failed []TestID
	// Skipped is tests that were collected but did not run.
	Skipped []TestID
	// BuildError reports that the suite did not run at all: a compile error, an
	// import error, a collection error. THIS IS NOT A RED TEST. An agent that
	// writes a test that does not compile and then "fixes" it in the code diff
	// produces a perfect red-green trace having fixed nothing, so step 1 must
	// distinguish assertion failure from failure to build.
	BuildError bool
}

// Mode is the change class, decided upstream (by classification, not here) and
// supplied as input. It selects which rules apply.
type Mode string

const (
	// ModeRedGreen is the full sequence: bug fixes and features with testable
	// behaviour. A new test must fail before the fix and pass after it.
	ModeRedGreen Mode = "red-green"
	// ModeGreenGreen is for changes with no red-able test BY CONSTRUCTION --
	// refactors, documentation, dependency bumps, performance work. Step 0 and
	// step 2 must both be green and Monotonicity still holds, so a refactor
	// cannot quietly drop a test; there is simply no step 1 to be red.
	//
	// This mode exists because without it every such change GATE-FAILS, and a
	// gate failure escalates the ladder -- so the failure mode is not a stuck
	// bead but a spend loop climbing toward cloud rungs.
	ModeGreenGreen Mode = "green-green"
)

// Outcome is the CLOSED set of verdicts. The distinctions are not cosmetic:
// exactly one of these escalates a rung, and collapsing any other into it buys
// an escalation out of something the agent could not have fixed.
type Outcome string

const (
	// OutcomePass -- the sequence held. Evidence for a human reviewer.
	OutcomePass Outcome = "pass"
	// OutcomeGateFailed -- the agent completed and its change did not satisfy
	// the sequence. THIS IS THE ONLY OUTCOME HERE THAT MAY ESCALATE A RUNG,
	// because it is the only one that is the agent's failure.
	OutcomeGateFailed Outcome = "gate-failed"
	// OutcomeFlake -- bounded retries disagreed, so the evidence is
	// nondeterministic and no verdict is available. Recorded as its own class
	// and NEVER laundered: retrying step 2 until green turns a flake into a
	// pass, and retrying step 1 until red turns one into a valid red. Both
	// directions are laundering and both are bounded by the runner.
	OutcomeFlake Outcome = "flake"
	// OutcomeNoVerifierPath -- the repository declares no runnable suite, so
	// there is no experiment to perform. Distinct from gate-failed BY DESIGN so
	// it does not feed escalation: no rung buys a test suite that does not exist.
	OutcomeNoVerifierPath Outcome = "no-verifier-path"
	// OutcomeBaseBroken -- step 0 was not green, so the repository's suite was
	// already failing before the agent touched anything. Red-green is
	// MEANINGLESS on such a repo: step 1 is red for reasons unrelated to the
	// change, and step 2's green could be the agent fixing a pre-existing
	// breakage instead of the bug. Not the agent's failure, so it must not
	// escalate -- and it is the honest answer for a repository gonk should not
	// be writing to yet.
	OutcomeBaseBroken Outcome = "base-broken"
	// OutcomeInfraFailed -- the runner could not complete: wall-clock or
	// resource cap, harness error, missing step. We do not know how the change
	// would have fared, and uncertainty must never escalate.
	OutcomeInfraFailed Outcome = "infra-failed"
)

// Rule is a stable code for WHICH rule decided a verdict, so a failure can be
// reported to a human, counted in metrics, and asserted in a test without
// matching on prose.
type Rule string

const (
	RuleOK                Rule = "ok"
	RuleRunnerIncomplete  Rule = "runner-incomplete"
	RuleNondeterministic  Rule = "nondeterministic"
	RuleNoSuite           Rule = "no-suite-declared"
	RuleMissingStep       Rule = "missing-step"
	RuleBaseNotGreen      Rule = "base-not-green"
	RuleTestDiffNotRed    Rule = "test-diff-not-red"
	RuleRedByBuildError   Rule = "red-by-build-error"
	RuleRedInExistingTest Rule = "red-in-existing-test"
	RuleFinalNotGreen     Rule = "final-not-green"
	RuleMonotonicity      Rule = "monotonicity"
)

// Evidence is everything the verdict depends on. Every field is an observation;
// none of it comes from the agent.
type Evidence struct {
	// Mode is the change class. Supplied by classification upstream.
	Mode Mode
	// SuiteDeclared reports that the BASE COMMIT declares a runnable test
	// command. Read from the base commit and NEVER from the incoming batch:
	// `.agent/` is the one prefix agents may write, so a run that rewrites its
	// own test declarations in the same batch is a self-widening loophole.
	SuiteDeclared bool

	// Base is step 0, WithTests is step 1, WithCode is step 2. WithTests is nil
	// in ModeGreenGreen, where there is no test diff. A nil step that the mode
	// requires is a runner bug, not a gate failure -- see RuleMissingStep.
	Base      *StepResult
	WithTests *StepResult
	WithCode  *StepResult

	// Incomplete, when non-empty, is why the runner could not finish: a
	// wall-clock or memory cap, a sandbox failure, a crashed harness. It is
	// checked FIRST because none of the evidence below means anything when the
	// run did not complete.
	Incomplete string
	// Nondeterministic, when non-empty, is what disagreed across the runner's
	// bounded retries. Checked before the rules because a nondeterministic
	// suite makes every one of them a coin flip.
	Nondeterministic string
}

// Verdict is the decision plus enough to explain it to a human without
// re-deriving it.
type Verdict struct {
	Outcome Outcome
	Rule    Rule
	// Detail is human-readable and safe to put in an MR comment.
	Detail string
	// Regressed is the tests that were passing at step 0 (or were newly added at
	// step 1) and are not passing at step 2. Populated only for
	// RuleMonotonicity, where naming them is the whole value of the finding.
	Regressed []TestID
}

// Classify is TOTAL: every Evidence maps to exactly one Verdict, and the order
// of the checks below IS the priority order.
//
// The ordering principle, inherited from pkg/gate: ANYTHING WE COULD NOT
// OBSERVE OUTRANKS ANYTHING WE JUDGED. A run that did not finish, or finished
// differently each time, tells us nothing about the change -- and reporting a
// gate failure on absent evidence buys an escalation onto a pricier rung out of
// our own flakiness.
func Classify(e Evidence) Verdict {
	// 1. THE RUN DID NOT FINISH. Nothing below is evidence of anything.
	if e.Incomplete != "" {
		return Verdict{OutcomeInfraFailed, RuleRunnerIncomplete, e.Incomplete, nil}
	}

	// 2. THE RUN DISAGREED WITH ITSELF. Checked before every gate rule, and
	// deliberately even when the steps look like a textbook pass: that is the
	// case where accepting the happy reading IS the laundering.
	if e.Nondeterministic != "" {
		return Verdict{OutcomeFlake, RuleNondeterministic, e.Nondeterministic, nil}
	}

	// 3. THERE IS NO EXPERIMENT TO PERFORM. Not a failure of the change, so it
	// must not reach the ladder.
	if !e.SuiteDeclared {
		return Verdict{OutcomeNoVerifierPath, RuleNoSuite,
			"the base commit declares no runnable test suite", nil}
	}

	// 4. Which steps this mode requires. An unknown mode is a harness bug and
	// falls through to infra-failed rather than being waved past as a pass.
	needsTestStep := false
	switch e.Mode {
	case ModeRedGreen:
		needsTestStep = true
	case ModeGreenGreen:
	default:
		return Verdict{OutcomeInfraFailed, RuleMissingStep,
			"unknown change class " + string(e.Mode), nil}
	}
	if e.Base == nil || e.WithCode == nil || (needsTestStep && e.WithTests == nil) {
		return Verdict{OutcomeInfraFailed, RuleMissingStep,
			"the runner did not produce every step this change class requires", nil}
	}

	// 5. STEP 0. Without a green base, red-green is meaningless: step 1 would be
	// red for unrelated reasons and step 2's green could be the agent fixing a
	// pre-existing breakage.
	if e.Base.BuildError {
		return Verdict{OutcomeBaseBroken, RuleBaseNotGreen,
			"the base commit does not build; there is nothing to verify against", nil}
	}
	if len(e.Base.Failed) > 0 {
		return Verdict{OutcomeBaseBroken, RuleBaseNotGreen,
			"the base commit's suite is already failing: " + join(e.Base.Failed), nil}
	}

	baseCollected := setOf(e.Base.Collected)
	basePassed := setOf(e.Base.Passed)

	// newTests are those step 1 introduced. Computed from COLLECTED rather than
	// from the diff, so a test the framework did not actually pick up cannot be
	// claimed as new.
	newTests := map[TestID]struct{}{}

	// 6. STEP 1, in red-green only.
	if needsTestStep {
		s1 := e.WithTests
		// A suite that does not build is red, and it is the wrong red: an agent
		// can write a broken test, "fix" it in the code diff, and produce a
		// flawless trace having fixed nothing.
		if s1.BuildError {
			return Verdict{OutcomeGateFailed, RuleRedByBuildError,
				"the test diff does not build, so step 1's red says nothing about the defect", nil}
		}
		for id := range setOf(s1.Collected) {
			if _, existed := baseCollected[id]; !existed {
				newTests[id] = struct{}{}
			}
		}
		// Every failure must be a NEW test. A pre-existing test failing here is
		// a regression the test diff introduced, not evidence about the defect.
		var oldBroken []TestID
		for _, id := range s1.Failed {
			if _, isNew := newTests[id]; !isNew {
				oldBroken = append(oldBroken, id)
			}
		}
		if len(oldBroken) > 0 {
			return Verdict{OutcomeGateFailed, RuleRedInExistingTest,
				"the test diff broke tests that were passing on the base: " + join(oldBroken), nil}
		}
		// ...and at least one new test must actually fail, or the test does not
		// exercise the defect and step 2's green is unearned.
		if len(s1.Failed) == 0 {
			return Verdict{OutcomeGateFailed, RuleTestDiffNotRed,
				"no new test fails on the base commit, so nothing demonstrates the defect", nil}
		}
	}

	// 7. STEP 2 must be green.
	if e.WithCode.BuildError {
		return Verdict{OutcomeGateFailed, RuleFinalNotGreen,
			"the change does not build", nil}
	}
	if len(e.WithCode.Failed) > 0 {
		return Verdict{OutcomeGateFailed, RuleFinalNotGreen,
			"tests still failing after the code diff: " + join(e.WithCode.Failed), nil}
	}

	// 8. MONOTONICITY -- the rule that makes the claim true.
	//
	//	passed(step 2)  must be a superset of  passed(step 0) + new tests(step 1)
	//
	// Green at step 2 only means nothing that RAN failed. This is what makes a
	// deleted test, a test removed by the code diff, and a skipped test all fail
	// in the same place, whichever diff attempted it and whatever mechanism it
	// used. Skipped tests are absent from Passed by construction, so they are
	// caught here without a rule of their own -- which is the point: this is a
	// semantic check, not a list of known tricks.
	finalPassed := setOf(e.WithCode.Passed)
	lost := map[TestID]struct{}{}
	for id := range basePassed {
		if _, ok := finalPassed[id]; !ok {
			lost[id] = struct{}{}
		}
	}
	for id := range newTests {
		if _, ok := finalPassed[id]; !ok {
			lost[id] = struct{}{}
		}
	}
	if len(lost) > 0 {
		regressed := sortedIDs(lost)
		return Verdict{OutcomeGateFailed, RuleMonotonicity,
			"the change reduces the set of passing tests: " + join(regressed), regressed}
	}

	return Verdict{OutcomePass, RuleOK, "", nil}
}

// join renders test ids for a human, in a stable order.
func join(ids []TestID) string {
	out := make([]TestID, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	s := ""
	for i, id := range out {
		if i > 0 {
			s += ", "
		}
		s += string(id)
	}
	return s
}

// sortedIDs is a stable, deduplicated copy -- verdicts are compared in tests and
// shown to humans, so set differences must not come out in map order.
func sortedIDs(ids map[TestID]struct{}) []TestID {
	out := make([]TestID, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// setOf builds a set from a slice, tolerating duplicates.
func setOf(ids []TestID) map[TestID]struct{} {
	out := make(map[TestID]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}
