package main

import (
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/effects"
)

// The observed failure (gonk-prr): the agent typed a slash where the project
// configures "::", and the broker applied it verbatim.
func TestTheObservedSlashFormIsNormalised(t *testing.T) {
	for _, in := range []string{"gonk/backend", "gonk/bug", "gonk/paging"} {
		want := "gonk::" + in[len("gonk/"):]
		got, err := normaliseLabel(in, "gonk::")
		if err != nil {
			t.Fatalf("normaliseLabel(%q) = err %v, want %q", in, err, want)
		}
		if got != want {
			t.Fatalf("normaliseLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A label already in the namespace must be left exactly alone -- normalisation
// must never double-prefix. None of these are in ReservedLabels; the reserved
// ones (gonk::needs-maintainer, gonk::verdict-close, ...) are covered
// separately below, because those must now be REFUSED rather than passed
// through untouched (T-05).
func TestAlreadyNamespacedLabelsAreUntouched(t *testing.T) {
	for _, in := range []string{"gonk::bug", "gonk::triage", "gonk::frontend"} {
		got, err := normaliseLabel(in, "gonk::")
		if err != nil {
			t.Fatalf("normaliseLabel(%q) = err %v, want it unchanged", in, err)
		}
		if got != in {
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
		got, err := normaliseLabel(in, "gonk::")
		if err != nil {
			t.Fatalf("normaliseLabel(%q) = err %v, want %q", in, err, want)
		}
		if got != want {
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
		got, err := normaliseLabel(in, "gonk::")
		if err != nil {
			t.Fatalf("normaliseLabel(%q) = err %v, want %q", in, err, want)
		}
		if got != want {
			t.Fatalf("normaliseLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A different project prefix must be honoured -- this is the issue store's
// namespace, not a hardcoded gonk one.
func TestADifferentPrefixIsHonoured(t *testing.T) {
	if got, err := normaliseLabel("triage/bug", "triage-"); err != nil || got != "triage-bug" {
		t.Fatalf("got (%q, %v), want (triage-bug, nil)", got, err)
	}
	if got, err := normaliseLabel("bug", "bot::"); err != nil || got != "bot::bug" {
		t.Fatalf("got (%q, %v), want (bot::bug, nil)", got, err)
	}
}

// An empty prefix means the project configured no namespace: pass through
// rather than inventing one.
func TestAnEmptyPrefixDisablesNormalisation(t *testing.T) {
	got, err := normaliseLabel("anything/at-all", "")
	if err != nil || got != "anything/at-all" {
		t.Fatalf("got (%q, %v), want (anything/at-all, nil)", got, err)
	}
}

// Exit criterion 1: a comma anywhere in the label must refuse the batch, not
// silently apply two labels for one effect. This is the exact GitLab behaviour
// AddIssueLabel used to be exposed to when it sent add_labels as a query
// string (gonk-hnw / R-02): GitLab splits that on commas.
func TestACommaIsRefused(t *testing.T) {
	if _, err := normaliseLabel("bug,security::critical", "gonk::"); err == nil {
		t.Fatal("normaliseLabel with a comma = nil error, want a refusal")
	}
}

// Whitespace-only and empty labels are refused, not silently dropped.
func TestWhitespaceOnlyIsRefused(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if _, err := normaliseLabel(in, "gonk::"); err == nil {
			t.Fatalf("normaliseLabel(%q) = nil error, want a refusal", in)
		}
	}
}

// GitLab caps a label at 255 bytes; anything longer is refused rather than
// silently truncated (truncation could collide two distinct proposed labels
// into one, or into a reserved one).
func TestOverlengthLabelIsRefused(t *testing.T) {
	ok := strings.Repeat("a", 255)
	if _, err := normaliseLabel(ok, ""); err != nil {
		t.Fatalf("a 255-byte label was refused: %v", err)
	}
	tooLong := strings.Repeat("a", 256)
	if _, err := normaliseLabel(tooLong, ""); err == nil {
		t.Fatal("a 256-byte label = nil error, want a refusal")
	}
}

// The 255-byte cap must be checked on the label as it will actually be SENT,
// i.e. after the configured prefix is applied -- not on the raw input. A
// 255-byte bare label plus "gonk::" is 261 bytes on the wire, over GitLab's
// own cap, even though the raw label alone was exactly at it.
func TestOverlengthLabelIsRefusedAfterPrefixIsApplied(t *testing.T) {
	bare := strings.Repeat("a", 255)
	if _, err := normaliseLabel(bare, "gonk::"); err == nil {
		t.Fatalf("normaliseLabel(255-byte label, %q) = nil error, want a refusal (261 bytes after the prefix)", "gonk::")
	}
	// The boundary: a label that is exactly 255 bytes AFTER the prefix is
	// applied must be accepted, not refused by an off-by-one.
	fits := strings.Repeat("a", 255-len("gonk::"))
	got, err := normaliseLabel(fits, "gonk::")
	if err != nil {
		t.Fatalf("a label that is exactly 255 bytes after the prefix was refused: %v", err)
	}
	if len(got) != 255 {
		t.Fatalf("normalised label is %d bytes, want exactly 255", len(got))
	}
}

// The length check is byte-based, not rune-based: len() on a Go string
// already counts bytes, but a future switch to utf8.RuneCountInString would
// silently let a multi-byte label through GitLab's byte-denominated cap
// without any test here noticing. "é" is 2 bytes in UTF-8, so 150 of them is
// 300 bytes -- over the cap by byte count though only 150 runes.
func TestMultiByteLabelLengthIsByteBased(t *testing.T) {
	tooLong := strings.Repeat("é", 150) // 150 runes, 300 bytes
	if _, err := normaliseLabel(tooLong, ""); err == nil {
		t.Fatal("a 300-byte (150-rune) label = nil error, want a refusal")
	}
	ok := strings.Repeat("é", 100) // 100 runes, 200 bytes: under the cap
	if _, err := normaliseLabel(ok, ""); err != nil {
		t.Fatalf("a 200-byte (100-rune) label was refused: %v", err)
	}
}

// Exit criterion 2: an agent cannot mint one of the broker's own audit
// labels -- gonk::fix-queued is written by applyCodeChangeVerdict to route a
// finding to a maintainer queue, and it must mean that gonk concluded it, not
// that a model asked for it.
func TestReservedStateLabelsAreRefused(t *testing.T) {
	for _, in := range []string{
		"gonk::fix-queued",
		"gonk::needs-maintainer",
		"gonk::denied",
		"gonk::verdict-code-change",
		"gonk::verdict-close",
		"gonk::verdict-reply-only",
	} {
		if _, err := normaliseLabel(in, "gonk::"); err == nil {
			t.Fatalf("normaliseLabel(%q) = nil error, want a refusal", in)
		}
	}
}

// The refusal is on the value that WOULD land in the namespace, not just the
// exact gonk:: spelling: an agent that dodges the literal reserved string by
// typing the slash/casing variant the namespace-repair rules fold together
// must be caught the same way normaliseLabel would fold it.
func TestReservedLabelsAreRefusedThroughNamespaceRepair(t *testing.T) {
	for _, in := range []string{"gonk/fix-queued", "GONK::Denied", "fix-queued"} {
		if _, err := normaliseLabel(in, "gonk::"); err == nil {
			t.Fatalf("normaliseLabel(%q) = nil error, want a refusal", in)
		}
	}
}

// A reserved bare word is refused even with no namespace configured: the
// refusal checks are not part of namespace repair, which an empty prefix
// disables.
func TestReservedLabelsAreRefusedEvenWithNoPrefix(t *testing.T) {
	if _, err := normaliseLabel("denied", ""); err == nil {
		t.Fatal("normaliseLabel(\"denied\", \"\") = nil error, want a refusal")
	}
}

// gonk::onboarding (pkg/intake.OnboardingIssueLabel) is the marker
// GitLabOnboarder uses to find its own "I need Developer" issue again rather
// than opening a second one -- gonk-written, same class as fix-queued/denied,
// and just as forgeable if an agent could apply it to an arbitrary issue.
func TestOnboardingLabelIsRefused(t *testing.T) {
	if _, err := normaliseLabel("gonk::onboarding", "gonk::"); err == nil {
		t.Fatal("normaliseLabel(\"gonk::onboarding\", \"gonk::\") = nil error, want a refusal")
	}
}

// The drift guard: ReservedLabels is DERIVED from verdictLabel rather than
// restating its output (see buildReservedLabels), so this is deliberately a
// consistency check on that derivation rather than an independent
// enumeration -- pkg/effects exposes no closed list of verdicts to check
// against. What it DOES catch: a verdict added to reservedVerdicts
// (broker_label.go) whose verdictLabel output was mistyped or mis-stripped
// on the way into ReservedLabels, and -- via the unknown-verdict case below
// -- confirms verdictLabel's default branch (what an unrecognised Verdict
// maps to) is already covered rather than accidentally exempt.
func TestReservedLabelsCoverEveryVerdictLabel(t *testing.T) {
	for _, v := range reservedVerdicts {
		label := verdictLabel(v)
		suffix := strings.TrimPrefix(label, "gonk::")
		if !ReservedLabels[suffix] {
			t.Fatalf("verdictLabel(%q) = %q, but %q is not in ReservedLabels", v, label, suffix)
		}
		if _, err := normaliseLabel(label, "gonk::"); err == nil {
			t.Fatalf("normaliseLabel(%q) = nil error, want a refusal (it is verdictLabel(%q))", label, v)
		}
	}
	// An unrecognised Verdict must fall through verdictLabel's default branch
	// to reply-only -- itself in reservedVerdicts and asserted above -- rather
	// than to some new, unreserved label.
	unknown := effects.Verdict("some-future-verdict-nobody-taught-verdictLabel-about")
	if got, want := verdictLabel(unknown), verdictLabel(effects.VerdictReplyOnly); got != want {
		t.Fatalf("verdictLabel(unknown verdict) = %q, want the default %q", got, want)
	}
}

// Must be total: it runs inside the apply path on whatever a model emitted.
// "Total" now means "never panics", not "never errors" -- a hostile or
// malformed label is exactly the case this function must now refuse.
func TestNormaliseIsTotal(t *testing.T) {
	for _, in := range []string{"", "   ", "gonk", "gonk::", "gonk//", "::", "/", "a,b", strings.Repeat("x", 500)} {
		for _, p := range []string{"gonk::", "", ":"} {
			_, _ = normaliseLabel(in, p)
		}
	}
	// A bare stem with no remainder must not become a bare prefix, which would
	// be a meaningless label.
	if got, err := normaliseLabel("gonk", "gonk::"); err == nil && got == "gonk::" {
		t.Fatal("a bare stem became a bare prefix, which labels nothing")
	}
}
