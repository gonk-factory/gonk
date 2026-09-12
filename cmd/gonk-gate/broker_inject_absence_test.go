package main

import (
	"strings"
	"testing"
)

// THE FAILURE THESE TESTS EXIST FOR (gonk-cl4p).
//
// 2026-09-12, project 75 -- six files, one of which (internal/paging/sort.go)
// ignores the desc flag in its SortByDate case and admits it in its own doc
// comment. Two issues describing that bug, same prompt shape, same rung:
//
//	!71  9 model calls, 62,653 prompt tokens -> named the file, the case, the
//	     comparison and the fix. gonk::verdict-code-change.
//	!70  6 model calls, 38,486 prompt tokens -> "I cannot find any relevant code
//	     files in the repository." gonk::verdict-reply-only.
//
// Every file was present in /workspace on the !70 run (verified by exec'ing the
// pod), and five tool round-trips had run. The model looked, stopped, and
// reported ABSENCE rather than UNCERTAINTY.
//
// These tests assert the PROPERTIES the prompt now claims, not its prose. Each
// one names the property it defends, so a future edit that deletes a clause has
// to argue with the reason rather than with a string.

// checkoutPrompt is the granted-checkout rendering -- the only path where a
// search procedure is meaningful, because it is the only path where there is a
// tree to search.
func checkoutPrompt() string {
	return renderTriagePrompt("acme/widget", 12, "",
		"Title: dates sort wrong\n\nBody: descending does nothing",
		"http://intake:9090/rig/a.tar.gz")
}

// GOAL 1. A negative conclusion must be structurally hard to reach: the prompt
// must carry an ORDERED procedure and, crucially, a PRECONDITION on the negative
// -- "you may not say the code is absent unless you finished the steps and said
// what you searched". Advice alone is what !70 already had and ignored.
func TestTriagePromptGatesTheNegativeConclusionOnAnEnumeratedSearch(t *testing.T) {
	got := checkoutPrompt()

	// The procedure exists and is ordered.
	if !strings.Contains(got, "SEARCH PROCEDURE") {
		t.Error("no named search procedure: a triage run has nothing to be held to")
	}
	if !strings.Contains(got, "IN ORDER") {
		t.Error("the procedure is not stated as ordered; an unordered checklist is " +
			"the variance the 6-vs-9-round-trip split came from")
	}
	for _, step := range []string{"  1. ", "  2. ", "  3. ", "  4. ", "  5. "} {
		if !strings.Contains(got, step) {
			t.Errorf("procedure step %q missing", strings.TrimSpace(step))
		}
	}
	// Step 1 must be an inventory of what is actually there, not a guess.
	if !strings.Contains(got, "List the top level") {
		t.Error("the procedure does not begin by listing the tree")
	}

	// THE PRECONDITION. This is the load-bearing clause: without it the steps
	// are advice.
	if !strings.Contains(got, "YOU MAY NOT REPORT THAT THE RELEVANT CODE DOES NOT EXIST") {
		t.Error("nothing forbids an unsupported negative conclusion")
	}
	if !strings.Contains(got, "NAMES THE DIRECTORIES YOU LISTED") ||
		!strings.Contains(got, "TERMS YOU SEARCHED FOR") {
		t.Error("a negative conclusion is not required to state what was searched, " +
			"so an unsupported one is indistinguishable from a thorough one")
	}

	// A failed search must be named as evidence about the SEARCH, not about the
	// repository. This is the exact inference !70 got backwards.
	if !strings.Contains(got, "NOT evidence that the code is absent") {
		t.Error("the prompt does not tell the agent that a failed search is not an absence proof")
	}
	if !strings.Contains(got, "is a statement about your") {
		t.Error("the prompt does not reframe \"I could not find it\" as a claim about the search")
	}
}

// GOAL 2. "I looked and found nothing" and "I could not finish looking" are
// different outcomes. The verdict vocabulary is closed and is NOT widened here,
// so the distinction rides in the comment as a fixed leading marker, coupled to
// a prohibition on asserting absence.
func TestTriagePromptSeparatesAnIncompleteSearchFromAnEmptyOne(t *testing.T) {
	got := checkoutPrompt()

	if !strings.Contains(got, searchIncompleteMarker) {
		t.Fatalf("the prompt never names the %q marker, so an unfinished run has no "+
			"way to report itself as one", searchIncompleteMarker)
	}
	if !strings.Contains(got, searchIncompleteLabel) {
		t.Errorf("the incomplete-search label %q is not requested, so the fact is "+
			"invisible on the board", searchIncompleteLabel)
	}
	// The marker must be tied to NOT asserting absence -- a marker that merely
	// decorates the same confident negative buys nothing.
	i := strings.Index(got, searchIncompleteMarker)
	tail := got[i:]
	if !strings.Contains(tail, "do NOT state that the code does not exist") {
		t.Error("the incomplete-search marker is not coupled to a prohibition on " +
			"asserting absence; the two outcomes still collapse")
	}
	if !strings.Contains(got, "IF YOU COULD NOT FINISH THE SEARCH") {
		t.Error("no instruction distinguishes an unfinished search from an empty one")
	}
	// Running out of road must be named as a legitimate report, or the model
	// will prefer the confident negative that reads as an answer.
	if !strings.Contains(got, "A DIFFERENT\nANSWER, NOT A WORSE ONE") {
		t.Error("reporting an unfinished search is not stated to be an acceptable " +
			"outcome, so the confident negative stays the attractive one")
	}
}

// GOAL 4, AND THE ONE THIS BEAD MUST NOT FAIL. Every clause added for goals 1-3
// is pressure to LOOK. None of it may become pressure to FIND: a small model
// pushed to produce a finding will invent one, and a confident wrong bug report
// is worse than a question.
//
// The interlock is two-sided and this test asserts both sides. The checklist
// raises the cost of giving up; the code-change CITATION precondition raises the
// cost of making something up.
func TestTriagePromptDoesNotPressureTheAgentIntoFindingADefect(t *testing.T) {
	got := checkoutPrompt()

	if !strings.Contains(got, "FINDING NO DEFECT IS A CORRECT OUTCOME") {
		t.Error("nothing tells the agent that a clean result is a success; the " +
			"procedure alone reads as \"keep going until you find something\"")
	}
	if !strings.Contains(got, "how hard you LOOK, not about what you must CONCLUDE") {
		t.Error("the procedure is not scoped to effort, so it reads as a demand for a finding")
	}
	if !strings.Contains(got, "Never assert a\nfault you have not actually read in the code") {
		t.Error("nothing forbids asserting a fault the agent has not read")
	}
	if !strings.Contains(got, "DO NOT MANUFACTURE A DEFECT") {
		t.Error("the verdict section does not warn against inventing a defect")
	}

	// The citation precondition. A code-change verdict must point at real code;
	// a defect that cannot be located is downgraded to reply-only, not reported.
	if !strings.Contains(got, "NAME THE FILE AND THE FUNCTION OR SYMBOL") {
		t.Error("code-change carries no citation requirement, so an invented defect " +
			"costs the model nothing to report")
	}
	if !strings.Contains(got, "it is NOT a code-change") {
		t.Error("nothing routes an uncitable suspicion away from code-change")
	}
}

// The citation and no-manufacture clauses live in the SHARED body, so they hold
// on the no-checkout path too -- where the agent cannot have read any code and
// so must never claim to have.
func TestNoCheckoutPromptStillForbidsAnUncitedDefect(t *testing.T) {
	got := renderTriagePrompt("acme/widget", 12, "", "Title: x", "")

	for _, want := range []string{
		"NAME THE FILE AND THE FUNCTION OR SYMBOL",
		"it is NOT a code-change",
		"DO NOT MANUFACTURE A DEFECT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the no-checkout prompt is missing %q; with no repository the "+
				"agent has even less business asserting a defect", want)
		}
	}
	// ...and it must still not invent a search procedure for a tree that is not
	// there. That is the gonk-msz failure: a model told to list an empty
	// directory fills the gap itself.
	if strings.Contains(got, "SEARCH PROCEDURE") {
		t.Error("the no-checkout prompt orders a search of a working directory it " +
			"does not have")
	}
	if strings.Contains(got, searchIncompleteMarker) {
		t.Error("the no-checkout prompt offers an incomplete-search report for a " +
			"search it was never asked to run")
	}
}

// THE CONTRACT THE PROMPT MUST NOT BREAK. pack/agents/triage/effect-shape.toml
// says comment {1,1} and label {0,N}; pkg/effects closes the verdict vocabulary;
// broker_apply.go parses the fenced batch. Wording changes must leave all of it
// intact, and this asserts each piece against the thing that actually enforces
// it wherever one exists.
func TestTriagePromptKeepsTheEffectShapeContract(t *testing.T) {
	for _, got := range []string{checkoutPrompt(), renderTriagePrompt("acme/widget", 12, "", "Title: x", "")} {
		// The fence sentinels, from the constants broker_apply.go parses with.
		if !strings.Contains(got, batchStartSentinel) || !strings.Contains(got, batchEndSentinel) {
			t.Fatal("the prompt no longer asks for the fenced batch broker_apply.go parses")
		}
		// The verdict vocabulary, verbatim.
		for _, v := range []string{`"reply-only"`, `"code-change"`, `"close"`} {
			if !strings.Contains(got, v) {
				t.Errorf("verdict %s dropped from the prompt", v)
			}
		}
		// Cardinality: exactly one comment, zero or more labels.
		if !strings.Contains(got, "exactly one comment effect and zero or more label effects") {
			t.Error("the prompt no longer states the effect-shape cardinality")
		}
		// The label namespace, and the single-line JSON rule the parser needs.
		if !strings.Contains(got, "gonk::") {
			t.Error("the label prefix instruction is gone")
		}
		if !strings.Contains(got, "valid JSON on a SINGLE line") {
			t.Error("the single-line JSON rule is gone; a multi-line batch costs the whole run")
		}
		// The pod holds no credentials. This is the broker's whole premise.
		if !strings.Contains(got, "do NOT fetch anything yourself") ||
			!strings.Contains(got, "you hold no credentials") {
			t.Error("the no-credentials framing is gone")
		}
	}
}

// The incomplete-search label is proposed by the AGENT, and an agent-proposed
// label that collides with the broker's reserved set REFUSES THE WHOLE BATCH
// (broker_label.go). Reserving it would therefore convert an honest "I could not
// finish" into a dropped run -- the precise inversion of its purpose.
//
// Asserted against ReservedLabels itself, not against a restatement of it, so
// reserving the name later fails here rather than in production.
func TestSearchIncompleteLabelIsNotReserved(t *testing.T) {
	suffix := strings.ToLower(strings.TrimPrefix(searchIncompleteLabel, gonkLabelPrefix))
	if ReservedLabels[suffix] {
		t.Fatalf("%q is in ReservedLabels; an agent proposing it would have its whole "+
			"batch refused, so an unfinished search would be reported as nothing at all",
			searchIncompleteLabel)
	}
	// It must also survive normalisation unchanged -- it is already in the
	// shipped namespace, so a project on the default prefix gets it verbatim.
	got, err := normaliseLabel(searchIncompleteLabel, gonkLabelPrefix)
	if err != nil {
		t.Fatalf("normaliseLabel(%q) = error %v; the agent cannot propose a label the "+
			"broker refuses", searchIncompleteLabel, err)
	}
	if got != searchIncompleteLabel {
		t.Errorf("normaliseLabel(%q) = %q, want it unchanged", searchIncompleteLabel, got)
	}
}
