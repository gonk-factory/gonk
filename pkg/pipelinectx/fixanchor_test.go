package pipelinectx

import (
	"strings"
	"testing"
)

const (
	fixTestRun     = "gonk:84:pipeline:ref-0123abcd:00112233aabbccdd:s880"
	fixTestRunMR   = "gonk:84:pipeline:mr5:00112233aabbccdd:s0"
	fixTestRunEp   = "gonk:84:pipeline:mr5:00112233aabbccdd:s0:f12"
	fixTestFix     = "gonk:84:pipefix:ref-0123abcd:00112233aabbccdd:s880"
	fixTestFixEp   = "gonk:84:pipefix:mr5:00112233aabbccdd:s0:f12"
	fixTestCause   = "0011223344556677"
	fixTestCauseUp = "0011223344556677AA"
)

// The fix anchor is the run anchor with ":pipeline:" replaced by ":pipefix:"
// (plan 3.2), and nothing else changes: the pair maps one to one, both ways.
func TestFixAnchorRoundTrips(t *testing.T) {
	for _, tc := range []struct{ run, fix string }{
		{fixTestRun, fixTestFix},
		{fixTestRunMR, "gonk:84:pipefix:mr5:00112233aabbccdd:s0"},
		{fixTestRunEp, fixTestFixEp},
	} {
		got, err := FixAnchor(tc.run)
		if err != nil || got != tc.fix {
			t.Errorf("FixAnchor(%q) = %q, %v; want %q", tc.run, got, err, tc.fix)
		}
		back, err := RunAnchorOfFix(tc.fix)
		if err != nil || back != tc.run {
			t.Errorf("RunAnchorOfFix(%q) = %q, %v; want %q", tc.fix, back, err, tc.run)
		}
		if !IsFixAnchor(tc.fix) || IsFixAnchor(tc.run) {
			t.Errorf("IsFixAnchor: fix %v run %v, want true false", IsFixAnchor(tc.fix), IsFixAnchor(tc.run))
		}
	}
}

func TestFixAnchorRefusesWhatIsNotARunAnchor(t *testing.T) {
	for _, bad := range []string{"", "gonk:84:issue:7", fixTestFix, fixTestRun + " ", "gonk:84:pipeline:mr5:xyz:s0",
		"gonk:84:pipeline:mr5:00112233aabbccdd:s0:f0"} {
		if got, err := FixAnchor(bad); err == nil {
			t.Errorf("FixAnchor(%q) = %q, want an error", bad, got)
		}
	}
	for _, bad := range []string{"", fixTestRun, "gonk:84:pipefix:mr5:xyz:s0", fixTestFix + "\n"} {
		if got, err := RunAnchorOfFix(bad); err == nil {
			t.Errorf("RunAnchorOfFix(%q) = %q, want an error", bad, got)
		}
	}
}

// Golden: the branch and the session key of a fix are functions of the fix
// anchor alone, and a later change to either would orphan every open fix.
func TestFixBranchAndSessionKeyGolden(t *testing.T) {
	b, err := FixBranch(fixTestFix)
	if err != nil {
		t.Fatal(err)
	}
	if b != "gonk/pipefix-28869115532f" {
		t.Errorf("FixBranch = %q", b)
	}
	if !IsFixBranch(b) {
		t.Errorf("FixBranch %q is not what IsFixBranch (the merged-fix lookup) recognises", b)
	}
	k, err := FixSessionKey(fixTestFix)
	if err != nil {
		t.Fatal(err)
	}
	if k != "gonk-84-pf-28869115532f" {
		t.Errorf("FixSessionKey = %q", k)
	}
	// Never the diagnosis's own session key: the two sessions are metered
	// separately.
	if rk, _ := SessionKey(fixTestRun); rk == k {
		t.Errorf("the fix session key equals the run's (%q)", k)
	}
	for _, bad := range []string{fixTestRun, "", "x"} {
		if _, err := FixBranch(bad); err == nil {
			t.Errorf("FixBranch(%q): want an error", bad)
		}
		if _, err := FixSessionKey(bad); err == nil {
			t.Errorf("FixSessionKey(%q): want an error", bad)
		}
	}
}

func TestFixMarkerRoundTrips(t *testing.T) {
	for _, st := range FixNoteStates() {
		m, err := FixMarker(fixTestFix, st)
		if err != nil {
			t.Fatalf("FixMarker(%q): %v", st, err)
		}
		want := "<!-- gonk:pipeline-fix:" + fixTestFix + " state=" + st + " -->"
		if m != want {
			t.Errorf("FixMarker = %q, want %q", m, want)
		}
		got := FixMarkerStates("status\n\n"+m+"\n", fixTestFix)
		if len(got) != 1 || got[0] != st {
			t.Errorf("FixMarkerStates = %v, want [%s]", got, st)
		}
		if other := FixMarkerStates(m, fixTestFixEp); len(other) != 0 {
			t.Errorf("a marker for another fix anchor was read: %v", other)
		}
	}
}

func TestFixMarkerRefusesABadAnchorOrState(t *testing.T) {
	if _, err := FixMarker(fixTestRun, FixNoteProposing); err == nil {
		t.Error("a run anchor was accepted as a fix anchor")
	}
	for _, st := range []string{"", "Proposing", "diagnosed", "proposing -->"} {
		if _, err := FixMarker(fixTestFix, st); err == nil {
			t.Errorf("state %q accepted", st)
		}
	}
}

// A run marker is not a fix marker and the reverse: the two notes live on the
// same landing and must never be taken for each other.
func TestFixAndRunMarkersDoNotCross(t *testing.T) {
	fm, _ := FixMarker(fixTestFix, FixNoteProposing)
	rm, _ := RunMarker(fixTestRun, RunStateDiagnosed)
	if s := RunMarkerStates(fm, fixTestRun); len(s) != 0 {
		t.Errorf("a fix marker read as a run marker: %v", s)
	}
	if s := RunMarkerStates(fm, fixTestFix); len(s) != 0 {
		t.Errorf("a fix marker read as a run marker for the fix anchor: %v", s)
	}
	if s := FixMarkerStates(rm, fixTestFix); len(s) != 0 {
		t.Errorf("a run marker read as a fix marker: %v", s)
	}
}

func TestFixMRMarkerRoundTrips(t *testing.T) {
	m, err := FixMRMarker(fixTestFix, fixTestCause)
	if err != nil {
		t.Fatal(err)
	}
	want := "<!-- gonk:pipeline-fix-mr:" + fixTestFix + " cause=" + fixTestCause + " -->"
	if m != want {
		t.Fatalf("FixMRMarker = %q, want %q", m, want)
	}
	a, c, ok := ParseFixMRMarker("Title text\n\n" + m + "\n\nmore")
	if !ok || a != fixTestFix || c != fixTestCause {
		t.Errorf("ParseFixMRMarker = %q %q %v", a, c, ok)
	}
}

func TestFixMRMarkerFailsClosed(t *testing.T) {
	if _, err := FixMRMarker(fixTestRun, fixTestCause); err == nil {
		t.Error("a run anchor accepted")
	}
	for _, c := range []string{"", "xyz", fixTestCauseUp, strings.Repeat("a", 15)} {
		if _, err := FixMRMarker(fixTestFix, c); err == nil {
			t.Errorf("cause key %q accepted", c)
		}
	}
	// No marker, a malformed one, and TWO DIFFERENT ones are all "not read":
	// a description that names two causes names none.
	m1, _ := FixMRMarker(fixTestFix, fixTestCause)
	m2, _ := FixMRMarker(fixTestFix, "ffffffffffffffff")
	for _, d := range []string{"", "no marker here", "<!-- gonk:pipeline-fix-mr:nonsense cause=zz -->",
		"<!-- gonk:pipeline-fix-mr:nonsense cause=0011223344556677 -->",
		"<!-- gonk:pipeline-fix-mr:" + fixTestRun + " cause=0011223344556677 -->", m1 + "\n" + m2} {
		if a, c, ok := ParseFixMRMarker(d); ok {
			t.Errorf("ParseFixMRMarker(%q) = %q %q true, want not ok", d, a, c)
		}
	}
	// The same marker twice is one marker.
	if _, _, ok := ParseFixMRMarker(m1 + "\n" + m1); !ok {
		t.Error("a repeated identical marker was refused")
	}
}
