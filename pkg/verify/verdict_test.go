package verify

import (
	"reflect"
	"testing"
)

// green builds a step where everything collected ran and passed.
func green(ids ...TestID) *StepResult {
	return &StepResult{Collected: ids, Passed: ids}
}

// A textbook run: the repo is green, the new test fails on the base, and the
// code diff makes it pass without disturbing anything else.
func TestRedGreenHappyPath(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: green("a", "b"),
		WithTests: &StepResult{
			Collected: []TestID{"a", "b", "new"},
			Passed:    []TestID{"a", "b"},
			Failed:    []TestID{"new"},
		},
		WithCode: green("a", "b", "new"),
	})
	if v.Outcome != OutcomePass || v.Rule != RuleOK {
		t.Fatalf("verdict = %+v, want pass/ok", v)
	}
}

// --- the three loopholes the design's own correction found -------------------
//
// Each of these satisfies red -> green PERFECTLY. Every one of them was possible
// under the sequence as first written, and the single rule that closes all three
// is Monotonicity. If any of these ever goes green again, the pipeline is back
// to being a ritual that a well-meaning model can satisfy by weakening the suite.

// (a) DELETE ONE, ADD ONE. The test diff removes test "b" and adds "new".
// Step 0 green, step 1 red in the new test, step 2 green -- and whatever "b"
// guarded is now unchecked forever. A path gate cannot see this: the deletion
// is in a test file, which is exactly where the test diff is allowed to write.
func TestMonotonicityCatchesDeleteOneAddOne(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: green("a", "b"),
		WithTests: &StepResult{
			Collected: []TestID{"a", "new"}, // "b" is gone
			Passed:    []TestID{"a"},
			Failed:    []TestID{"new"},
		},
		WithCode: green("a", "new"),
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleMonotonicity {
		t.Fatalf("verdict = %+v, want gate-failed/monotonicity: a test that was passing was deleted", v)
	}
	if want := []TestID{"b"}; !reflect.DeepEqual(v.Regressed, want) {
		t.Fatalf("Regressed = %v, want %v -- naming the lost test is the value of this finding", v.Regressed, want)
	}
}

// (b) THE CODE DIFF DELETES TESTS. The test diff is honest; the code diff at
// step 2 removes a test that was passing. The original design constrained the
// test diff's paths and said nothing about the inverse, so this sailed through.
func TestMonotonicityCatchesCodeDiffDeletingTests(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: green("a", "b"),
		WithTests: &StepResult{
			Collected: []TestID{"a", "b", "new"},
			Passed:    []TestID{"a", "b"},
			Failed:    []TestID{"new"},
		},
		WithCode: green("a", "new"), // "b" deleted by the CODE diff
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleMonotonicity {
		t.Fatalf("verdict = %+v, want gate-failed/monotonicity", v)
	}
}

// (c) SKIPS AND DESELECTION. Nothing is deleted -- "b" is still collected, it
// just does not run. This is the subtlest of the three and the reason Passed and
// Skipped are separate fields: GREEN DOES NOT MEAN "PASSED", IT MEANS "NOTHING
// THAT RAN FAILED". A verifier tracking only failures reports a clean pass here.
func TestMonotonicityCatchesSkippedTest(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: green("a", "b"),
		WithTests: &StepResult{
			Collected: []TestID{"a", "b", "new"},
			Passed:    []TestID{"a", "b"},
			Failed:    []TestID{"new"},
		},
		WithCode: &StepResult{
			Collected: []TestID{"a", "b", "new"}, // still collected...
			Passed:    []TestID{"a", "new"},
			Skipped:   []TestID{"b"}, // ...but it no longer runs
		},
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleMonotonicity {
		t.Fatalf("verdict = %+v, want gate-failed/monotonicity: a skipped test is not a passing test", v)
	}
	if want := []TestID{"b"}; !reflect.DeepEqual(v.Regressed, want) {
		t.Fatalf("Regressed = %v, want %v", v.Regressed, want)
	}
}

// The new test itself must survive to step 2. Otherwise an agent adds a failing
// test, deletes it in the code diff, and both the red and the green are real.
func TestMonotonicityRequiresTheNewTestToPassAtStepTwo(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: green("a"),
		WithTests: &StepResult{
			Collected: []TestID{"a", "new"},
			Passed:    []TestID{"a"},
			Failed:    []TestID{"new"},
		},
		WithCode: green("a"), // "new" withdrawn rather than fixed
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleMonotonicity {
		t.Fatalf("verdict = %+v, want gate-failed/monotonicity", v)
	}
}

// --- red for the right reason ------------------------------------------------

// A test diff that does not COMPILE is also red. An agent that writes a broken
// test and then "fixes" it in the code diff produces a flawless red-green trace
// having fixed nothing at all.
func TestStepOneRedByBuildErrorIsNotARedTest(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base:      green("a"),
		WithTests: &StepResult{BuildError: true},
		WithCode:  green("a", "new"),
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleRedByBuildError {
		t.Fatalf("verdict = %+v, want gate-failed/red-by-build-error", v)
	}
}

// "Red" is not enough: the NEW tests must be the ones failing. A pre-existing
// unrelated failure would otherwise satisfy step 1 for free -- though step 0
// catches most of those, a test diff can also break an existing test, and that
// is a regression the test diff introduced, not evidence of anything.
func TestStepOneMustBeRedInTheNewTestsNotTheOldOnes(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: green("a", "b"),
		WithTests: &StepResult{
			Collected: []TestID{"a", "b", "new"},
			Passed:    []TestID{"a", "new"},
			Failed:    []TestID{"b"}, // the test diff broke an EXISTING test
		},
		WithCode: green("a", "b", "new"),
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleRedInExistingTest {
		t.Fatalf("verdict = %+v, want gate-failed/red-in-existing-test", v)
	}
}

// A test diff that is green proves nothing: the test does not exercise the
// defect, so step 2's green is unearned.
func TestStepOneGreenFailsTheGate(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base:      green("a"),
		WithTests: green("a", "new"),
		WithCode:  green("a", "new"),
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleTestDiffNotRed {
		t.Fatalf("verdict = %+v, want gate-failed/test-diff-not-red", v)
	}
}

func TestStepTwoMustBeGreen(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: green("a"),
		WithTests: &StepResult{
			Collected: []TestID{"a", "new"}, Passed: []TestID{"a"}, Failed: []TestID{"new"},
		},
		WithCode: &StepResult{
			Collected: []TestID{"a", "new"}, Passed: []TestID{"a"}, Failed: []TestID{"new"},
		},
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleFinalNotGreen {
		t.Fatalf("verdict = %+v, want gate-failed/final-not-green", v)
	}
}

// --- the outcomes that must NOT escalate -------------------------------------

// A repository whose suite is already failing cannot be verified: step 1 is red
// for reasons unrelated to the change and step 2's green could be the agent
// fixing a pre-existing breakage. It is also not the agent's failure, and no
// pricier rung buys a repository a working test suite -- so this must never be
// gate-failed.
func TestBaseNotGreenIsNotAGateFailure(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: &StepResult{
			Collected: []TestID{"a", "b"}, Passed: []TestID{"a"}, Failed: []TestID{"b"},
		},
		WithTests: &StepResult{
			Collected: []TestID{"a", "b", "new"}, Passed: []TestID{"a"},
			Failed: []TestID{"b", "new"},
		},
		WithCode: green("a", "b", "new"),
	})
	if v.Outcome != OutcomeBaseBroken || v.Rule != RuleBaseNotGreen {
		t.Fatalf("verdict = %+v, want base-broken/base-not-green -- a broken repo is not the agent's failure", v)
	}
	if v.Outcome == OutcomeGateFailed {
		t.Fatal("base-broken must never be gate-failed: gate failures escalate the ladder")
	}
}

// A base that does not even build is the same situation.
func TestBaseBuildErrorIsBaseBroken(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base:      &StepResult{BuildError: true},
		WithTests: &StepResult{Collected: []TestID{"new"}, Failed: []TestID{"new"}},
		WithCode:  green("new"),
	})
	if v.Outcome != OutcomeBaseBroken {
		t.Fatalf("verdict = %+v, want base-broken", v)
	}
}

// Refactors, docs and dependency bumps have no red-able test BY CONSTRUCTION.
// Without a distinct class every one of them gate-fails, and since a gate
// failure escalates the ladder the failure mode is a spend loop climbing toward
// cloud rungs -- not merely a stuck bead.
func TestNoSuiteDeclaredIsItsOwnOutcomeAndDoesNotEscalate(t *testing.T) {
	v := Classify(Evidence{Mode: ModeRedGreen, SuiteDeclared: false})
	if v.Outcome != OutcomeNoVerifierPath || v.Rule != RuleNoSuite {
		t.Fatalf("verdict = %+v, want no-verifier-path/no-suite-declared", v)
	}
}

// Retrying step 2 until green launders a flake into a pass; retrying step 1
// until red launders one into a valid red. The runner bounds both; this asserts
// the verdict refuses to judge at all once retries disagreed -- INCLUDING when
// every other rule would have said pass, which is the case that matters.
func TestNondeterminismIsRecordedNeverLaundered(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: green("a"),
		WithTests: &StepResult{
			Collected: []TestID{"a", "new"}, Passed: []TestID{"a"}, Failed: []TestID{"new"},
		},
		WithCode:         green("a", "new"), // a textbook pass...
		Nondeterministic: "step 2 disagreed across 3 attempts: new passed 2/3",
	})
	if v.Outcome != OutcomeFlake || v.Rule != RuleNondeterministic {
		t.Fatalf("verdict = %+v, want flake/nondeterministic even though the steps look perfect", v)
	}
}

// A run that hit its wall-clock or memory cap tells us nothing about the change.
// Uncertainty must never escalate (pkg/gate's invariant, and the same reasoning).
func TestIncompleteRunIsInfraNotGateFailure(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base:       green("a"),
		Incomplete: "wall-clock cap of 10m reached during step 1",
	})
	if v.Outcome != OutcomeInfraFailed || v.Rule != RuleRunnerIncomplete {
		t.Fatalf("verdict = %+v, want infra-failed/runner-incomplete", v)
	}
}

// Incompleteness outranks everything, including evidence that would gate-fail:
// we do not know what the change would have done, and a guess in either
// direction is wrong.
func TestIncompleteOutranksAFailingStep(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base: &StepResult{Collected: []TestID{"a"}, Failed: []TestID{"a"}},
		WithCode: &StepResult{
			Collected: []TestID{"a"}, Failed: []TestID{"a"},
		},
		Incomplete: "sandbox died",
	})
	if v.Outcome != OutcomeInfraFailed {
		t.Fatalf("verdict = %+v, want infra-failed to outrank the failing steps", v)
	}
}

// A step the mode requires but the runner did not produce is a HARNESS bug, not
// the agent's failure. Classifying it as gate-failed would escalate a rung out
// of our own defect.
func TestMissingRequiredStepIsInfraNotGateFailure(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeRedGreen, SuiteDeclared: true,
		Base:     green("a"),
		WithCode: green("a"),
		// WithTests omitted, and red-green requires it.
	})
	if v.Outcome != OutcomeInfraFailed || v.Rule != RuleMissingStep {
		t.Fatalf("verdict = %+v, want infra-failed/missing-step", v)
	}
}

// --- green-green ------------------------------------------------------------

// No step 1, because there is no test diff -- but step 0 and step 2 must both be
// green and Monotonicity still holds, so a refactor cannot quietly drop a test.
func TestGreenGreenHappyPath(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeGreenGreen, SuiteDeclared: true,
		Base:     green("a", "b"),
		WithCode: green("a", "b"),
	})
	if v.Outcome != OutcomePass || v.Rule != RuleOK {
		t.Fatalf("verdict = %+v, want pass/ok", v)
	}
}

// The reason green-green is not a free pass: it still may not lose a test.
func TestGreenGreenStillEnforcesMonotonicity(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeGreenGreen, SuiteDeclared: true,
		Base:     green("a", "b"),
		WithCode: green("a"),
	})
	if v.Outcome != OutcomeGateFailed || v.Rule != RuleMonotonicity {
		t.Fatalf("verdict = %+v, want gate-failed/monotonicity", v)
	}
	if want := []TestID{"b"}; !reflect.DeepEqual(v.Regressed, want) {
		t.Fatalf("Regressed = %v, want %v", v.Regressed, want)
	}
}

// Adding tests is always allowed -- Monotonicity is a floor, not an equality.
func TestAddingExtraPassingTestsIsFine(t *testing.T) {
	v := Classify(Evidence{
		Mode: ModeGreenGreen, SuiteDeclared: true,
		Base:     green("a"),
		WithCode: green("a", "bonus1", "bonus2"),
	})
	if v.Outcome != OutcomePass {
		t.Fatalf("verdict = %+v, want pass", v)
	}
}

// --- totality ---------------------------------------------------------------

// Classify must be TOTAL: no input panics and none returns an empty verdict.
// The zero Evidence is the case a caller hits first when wiring this up.
func TestClassifyIsTotal(t *testing.T) {
	cases := []Evidence{
		{},
		{Mode: ModeRedGreen},
		{Mode: ModeGreenGreen, SuiteDeclared: true},
		{Mode: "nonsense", SuiteDeclared: true, Base: green("a"), WithCode: green("a")},
		{Mode: ModeRedGreen, SuiteDeclared: true, Base: &StepResult{}, WithTests: &StepResult{}, WithCode: &StepResult{}},
	}
	for i, e := range cases {
		v := Classify(e)
		if v.Outcome == "" || v.Rule == "" {
			t.Fatalf("case %d (%+v): empty verdict %+v", i, e, v)
		}
	}
}

// An unknown Mode must not silently behave like one of the real ones. A future
// class added upstream and not handled here has to fail loudly rather than be
// waved through as a pass.
func TestUnknownModeIsInfraNotPass(t *testing.T) {
	v := Classify(Evidence{
		Mode: "some-future-class", SuiteDeclared: true,
		Base: green("a"), WithCode: green("a"),
	})
	if v.Outcome != OutcomeInfraFailed {
		t.Fatalf("verdict = %+v, want infra-failed for an unhandled mode", v)
	}
}
