package trace

import (
	"reflect"
	"testing"
)

// THE DISTINCTION THIS WHOLE PACKAGE EXISTS TO PRESERVE: a trace we could not
// collect is not a trace of an idle agent. Conflating them turns our own
// telemetry outage into somebody's rejected batch, and on the triage path a
// rejected batch re-slings the bead onto a pricier rung.
func TestAbsentIsNotTheSameAsDidNothing(t *testing.T) {
	didNothing := Trace{Completeness: Complete, Calls: nil}
	unobserved := Trace{Completeness: Absent, Calls: nil}

	if !didNothing.Observed() {
		t.Fatal("a complete trace with no calls IS an observation: the agent called nothing")
	}
	if unobserved.Observed() {
		t.Fatal("an absent trace must never read as an observation")
	}
	// They are indistinguishable by Calls alone, which is precisely why
	// Completeness cannot be retrofitted onto a consumer written against a list.
	if !reflect.DeepEqual(didNothing.Calls, unobserved.Calls) {
		t.Fatal("fixture no longer demonstrates the point")
	}
}

// Partial is still an observation -- some turns were seen. A predicate may
// legitimately act on a partial trace as long as it only REJECTS on positive
// evidence, never on absence within it.
func TestPartialCountsAsObserved(t *testing.T) {
	if !(Trace{Completeness: Partial}).Observed() {
		t.Fatal("partial must count as observed")
	}
}

// An unknown completeness must not default to Complete: a collector that grows
// a new state, or a corrupted row, must not silently become trustworthy.
func TestUnknownCompletenessIsNotValidAndIsNotObserved(t *testing.T) {
	c := Completeness("probably-fine")
	if c.Valid() {
		t.Fatal("unknown completeness must not validate")
	}
	if (Trace{Completeness: c}).Observed() {
		t.Fatal("unknown completeness must not read as observed")
	}
	for _, ok := range []Completeness{Complete, Partial, Absent} {
		if !ok.Valid() {
			t.Fatalf("%q must validate", ok)
		}
	}
}

func TestToolCountsAndToolsAreSetArithmetic(t *testing.T) {
	tr := Trace{
		Completeness: Complete,
		Turns:        2, // two assistant turns, one of which made two calls
		Calls: []Call{
			{Tool: "read", Target: "internal/paging/paging.go"},
			{Tool: "grep"},
			{Tool: "read", Target: "README.md"},
		},
	}
	want := map[string]int{"read": 2, "grep": 1}
	if got := tr.ToolCounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolCounts = %v, want %v", got, want)
	}
	// Sorted, so a golden test or a diff is stable.
	if got := tr.Tools(); !reflect.DeepEqual(got, []string{"grep", "read"}) {
		t.Fatalf("Tools = %v, want [grep read]", got)
	}
	// Turns is not len(Calls): one turn may make several calls or none.
	if tr.Turns == len(tr.Calls) {
		t.Fatal("fixture should keep Turns and len(Calls) distinct")
	}
}

func TestToolCountsOnAnEmptyTraceIsEmptyNotNil(t *testing.T) {
	got := Trace{Completeness: Absent}.ToolCounts()
	if got == nil || len(got) != 0 {
		t.Fatalf("ToolCounts = %v, want an empty non-nil map", got)
	}
}
