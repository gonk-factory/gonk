package main

import "testing"

// The observed failure (gonk-prr): the agent typed a slash where the project
// configures "::", and the broker applied it verbatim.
func TestTheObservedSlashFormIsNormalised(t *testing.T) {
	for _, in := range []string{"gonk/backend", "gonk/bug", "gonk/paging"} {
		want := "gonk::" + in[len("gonk/"):]
		if got := normaliseLabel(in, "gonk::"); got != want {
			t.Fatalf("normaliseLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A label already in the namespace must be left exactly alone -- normalisation
// must never double-prefix.
func TestAlreadyNamespacedLabelsAreUntouched(t *testing.T) {
	for _, in := range []string{"gonk::bug", "gonk::needs-maintainer", "gonk::verdict-close"} {
		if got := normaliseLabel(in, "gonk::"); got != in {
			t.Fatalf("normaliseLabel(%q) = %q, want it unchanged", in, got)
		}
	}
}

// Anything else gets the prefix, so NOTHING an agent proposes can land outside
// the namespace -- which is the whole point of the broker owning this.
func TestABareLabelIsPulledIntoTheNamespace(t *testing.T) {
	for in, want := range map[string]string{
		"bug":        "gonk::bug",
		"frontend":   "gonk::frontend",
		"needs-info": "gonk::needs-info",
	} {
		if got := normaliseLabel(in, "gonk::"); got != want {
			t.Fatalf("normaliseLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Other separators the model might reach for, and a capitalised stem: a label
// is a human-facing string and Gonk/bug is the same mistake as gonk/bug.
func TestOtherSeparatorsAndCasing(t *testing.T) {
	for in, want := range map[string]string{
		"gonk:bug":  "gonk::bug",
		"gonk-bug":  "gonk::bug",
		"Gonk/bug":  "gonk::bug",
		"GONK::bug": "gonk::bug",
	} {
		if got := normaliseLabel(in, "gonk::"); got != want {
			t.Fatalf("normaliseLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A different project prefix must be honoured -- this is the issue store's
// namespace, not a hardcoded gonk one.
func TestADifferentPrefixIsHonoured(t *testing.T) {
	if got := normaliseLabel("triage/bug", "triage-"); got != "triage-bug" {
		t.Fatalf("got %q, want triage-bug", got)
	}
	if got := normaliseLabel("bug", "bot::"); got != "bot::bug" {
		t.Fatalf("got %q, want bot::bug", got)
	}
}

// An empty prefix means the project configured no namespace: pass through
// rather than inventing one.
func TestAnEmptyPrefixDisablesNormalisation(t *testing.T) {
	if got := normaliseLabel("anything/at-all", ""); got != "anything/at-all" {
		t.Fatalf("got %q, want it unchanged", got)
	}
}

// Must be total: it runs inside the apply path on whatever a model emitted.
func TestNormaliseIsTotal(t *testing.T) {
	for _, in := range []string{"", "   ", "gonk", "gonk::", "gonk//", "::", "/"} {
		for _, p := range []string{"gonk::", "", ":"} {
			_ = normaliseLabel(in, p)
		}
	}
	// A bare stem with no remainder must not become a bare prefix, which would
	// be a meaningless label.
	if got := normaliseLabel("gonk", "gonk::"); got == "gonk::" {
		t.Fatal("a bare stem became a bare prefix, which labels nothing")
	}
}
