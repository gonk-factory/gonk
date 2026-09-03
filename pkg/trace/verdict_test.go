package trace

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gate"
)

func complete(calls ...Call) Trace {
	return Trace{Completeness: Complete, Calls: calls, Turns: len(calls)}
}

var triagePolicy = Policy{
	ReadTools:            []string{"read"},
	RequireTargetReadFor: []string{"reply-only", "close"},
}

// THE PREDICATE THE SLICE EXISTS FOR. reply-only and close produce no artifact,
// so nothing downstream can catch a confabulated diagnosis; this is the only
// thing standing between "the agent read the code" and "the agent sounded like
// it had".
func TestVerdictsWithNoArtifactRequireAnObservedRead(t *testing.T) {
	read := complete(Call{Tool: "read", Target: "/workspace/internal/paging/paging.go"})
	none := complete(Call{Tool: "glob"}, Call{Tool: "grep"})

	for _, verdict := range []string{"reply-only", "close", ""} {
		got := Classify(Input{Trace: read, Verdict: verdict, Target: "internal/paging/paging.go"}, triagePolicy)
		if got.Outcome != OutcomeSatisfied {
			t.Fatalf("verdict %q with a read of the target = %q (%s), want satisfied", verdict, got.Outcome, got.Reason)
		}
		got = Classify(Input{Trace: none, Verdict: verdict, Target: "internal/paging/paging.go"}, triagePolicy)
		if got.Outcome != OutcomeViolated {
			t.Fatalf("verdict %q with NO read of the target = %q, want violated", verdict, got.Outcome)
		}
	}
}

// code-change is not in the list: it produces a diff, which the verify pipeline
// can judge on outcome evidence. Process evidence is the weaker instrument and
// should not be applied where the stronger one exists.
func TestCodeChangeIsNotGatedOnAReadHere(t *testing.T) {
	got := Classify(Input{
		Trace: complete(Call{Tool: "glob"}), Verdict: "code-change", Target: "internal/paging/paging.go",
	}, triagePolicy)
	if got.Outcome != OutcomeSatisfied {
		t.Fatalf("code-change = %q (%s), want satisfied", got.Outcome, got.Reason)
	}
}

// The collected path is ABSOLUTE because that is what the tool was given, while
// a pack or an issue names a repo-relative path. Requiring equality would make
// this predicate false for every session ever recorded.
func TestTargetMatchingToleratesTheCheckoutPrefix(t *testing.T) {
	for _, collected := range []string{
		"/workspace/internal/paging/paging.go",
		"internal/paging/paging.go",
		"./internal/paging/paging.go",
	} {
		got := Classify(Input{
			Trace:   complete(Call{Tool: "read", Target: collected}),
			Verdict: "close", Target: "internal/paging/paging.go",
		}, triagePolicy)
		if got.Outcome != OutcomeSatisfied {
			t.Fatalf("collected %q did not match the target: %s", collected, got.Reason)
		}
	}
	// It must still be a real suffix match, not a substring one: a different
	// file whose name merely ends the same way is not the target.
	got := Classify(Input{
		Trace:   complete(Call{Tool: "read", Target: "/workspace/vendor/other/paging.go"}),
		Verdict: "close", Target: "internal/paging/paging.go",
	}, triagePolicy)
	if got.Outcome != OutcomeViolated {
		t.Fatalf("a different file matched the target: %q", got.Outcome)
	}
}

// ACCEPTANCE CRITERION 1: an unobserved trace must NOT reject. Rejecting here
// would convert our own telemetry outage into a re-slung bead and eventually an
// escalation onto a pricier rung -- exactly what gate.Classify exists to stop.
func TestUnobservedNeverRejects(t *testing.T) {
	for _, c := range []Completeness{Absent, Completeness("garbage"), ""} {
		got := Classify(Input{Trace: Trace{Completeness: c}, Verdict: "close", Target: "a.go"}, triagePolicy)
		if got.Outcome != OutcomeUnobserved {
			t.Fatalf("completeness %q = %q, want unobserved", c, got.Outcome)
		}
		if got.Rejects() {
			t.Fatalf("completeness %q REJECTED; missing evidence must never reject", c)
		}
	}
}

// A PARTIAL trace can support presence but never absence: a tool we did not see
// may be in the part we missed. So a forbidden tool still fires, and everything
// that reasons about absence does not.
func TestPartialSupportsPresenceButNotAbsence(t *testing.T) {
	partial := Trace{Completeness: Partial, Calls: []Call{{Tool: "bash"}}}

	got := Classify(Input{Trace: partial, Verdict: "close", Target: "a.go"},
		Policy{ForbiddenTools: []string{"bash"}, RequireTargetReadFor: []string{"close"}})
	if got.Outcome != OutcomeViolated {
		t.Fatalf("a forbidden tool SEEN in a partial trace = %q, want violated", got.Outcome)
	}

	got = Classify(Input{Trace: partial, Verdict: "close", Target: "a.go"}, triagePolicy)
	if got.Rejects() {
		t.Fatalf("a partial trace rejected on absence: %s", got.Reason)
	}
	if got.Outcome != OutcomeUnobserved {
		t.Fatalf("partial absence = %q, want unobserved", got.Outcome)
	}
}

// ACCEPTANCE CRITERION 2: a PASSING check grants nothing. Only one outcome
// rejects, and no outcome unlocks anything.
func TestOnlyViolatedRejectsAndNothingGrants(t *testing.T) {
	for _, o := range []Outcome{OutcomeSatisfied, OutcomeUnobserved, OutcomeNoPolicy} {
		if (Verdict{Outcome: o}).Rejects() {
			t.Fatalf("%q rejects; only violated may", o)
		}
	}
	if !(Verdict{Outcome: OutcomeViolated}).Rejects() {
		t.Fatal("violated must reject")
	}
}

// MECHANICAL DRIFT CHECK, in the style of gate.MayPour vs intake.MayFire: no
// trajectory outcome may be a value gate.Escalates accepts. If a MISSING trace
// could escalate a rung, we would have bought an escalation out of our own
// telemetry outage -- the precise failure gate.Classify exists to prevent.
func TestNoTrajectoryOutcomeCanEscalateARung(t *testing.T) {
	for _, o := range []Outcome{OutcomeSatisfied, OutcomeViolated, OutcomeUnobserved, OutcomeNoPolicy} {
		if gate.Escalates(string(o)) {
			t.Fatalf("trajectory outcome %q escalates a rung; these outcomes must be disjoint from gate's", o)
		}
	}
}

func TestNoPolicyIsDistinctFromSatisfied(t *testing.T) {
	got := Classify(Input{Trace: complete(), Verdict: "close", Target: "a.go"}, Policy{})
	if got.Outcome != OutcomeNoPolicy {
		t.Fatalf("empty policy = %q, want no-policy -- 'nothing was asked' is not 'everything held'", got.Outcome)
	}
	if got.Rejects() {
		t.Fatal("an agent with no declared predicates must not be rejected")
	}
}

func TestRequiredForbiddenAndMinimumCounts(t *testing.T) {
	tr := complete(Call{Tool: "read", Target: "a.go"}, Call{Tool: "read", Target: "b.go"}, Call{Tool: "grep"})

	if got := Classify(Input{Trace: tr}, Policy{RequiredTools: []string{"read", "grep"}}); got.Outcome != OutcomeSatisfied {
		t.Fatalf("required tools present = %q (%s)", got.Outcome, got.Reason)
	}
	if got := Classify(Input{Trace: tr}, Policy{RequiredTools: []string{"bash"}}); got.Outcome != OutcomeViolated {
		t.Fatalf("missing required tool = %q, want violated", got.Outcome)
	}
	if got := Classify(Input{Trace: tr}, Policy{ForbiddenTools: []string{"grep"}}); got.Outcome != OutcomeViolated {
		t.Fatalf("forbidden tool used = %q, want violated", got.Outcome)
	}
	if got := Classify(Input{Trace: tr}, Policy{MinCalls: map[string]int{"read": 2}}); got.Outcome != OutcomeSatisfied {
		t.Fatalf("min calls met = %q (%s)", got.Outcome, got.Reason)
	}
	if got := Classify(Input{Trace: tr}, Policy{MinCalls: map[string]int{"read": 3}}); got.Outcome != OutcomeViolated {
		t.Fatalf("min calls unmet = %q, want violated", got.Outcome)
	}
}

// Classify must be total: no input shape may panic, because it runs inside the
// broker's gate chain on whatever the collector happened to record.
func TestClassifyIsTotal(t *testing.T) {
	inputs := []Input{
		{},
		{Trace: Trace{Completeness: Complete}},
		{Trace: Trace{Completeness: Complete, Calls: []Call{{}}}, Verdict: "close", Target: "a.go"},
		{Trace: Trace{Completeness: Complete}, Target: "  "},
	}
	for _, in := range inputs {
		for _, p := range []Policy{{}, triagePolicy, {MinCalls: map[string]int{"": 1}}} {
			_ = Classify(in, p)
		}
	}
}
