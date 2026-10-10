package pipelinectx

import (
	"strings"
	"testing"
)

const testAnchor = "gonk:42:pipeline:mr8:0123456789abcdef:s77"

func TestRunMarkerRoundTrip(t *testing.T) {
	for _, st := range RunStates() {
		m, err := RunMarker(testAnchor, st)
		if err != nil {
			t.Fatalf("RunMarker(%s) = %v", st, err)
		}
		want := "<!-- gonk:pipeline-run:" + testAnchor + " state=" + st + " -->"
		if m != want {
			t.Fatalf("marker = %q, want %q (the format P7 writes and P3 reads)", m, want)
		}
		body := "some rendered text\n\n" + m + "\nmore text"
		got := RunMarkerStates(body, testAnchor)
		if len(got) != 1 || got[0] != st {
			t.Fatalf("RunMarkerStates = %v, want [%s]", got, st)
		}
	}
}

func TestRunMarkerStatesIgnoresOtherAnchors(t *testing.T) {
	other := "gonk:42:pipeline:mr8:0123456789abcdef:s78" // same run, different last success
	m, _ := RunMarker(other, RunStateDiagnosed)
	if got := RunMarkerStates(m, testAnchor); len(got) != 0 {
		t.Fatalf("a marker for %s matched %s: %v", other, testAnchor, got)
	}
	// prefix of the anchor must not match either
	prefix := "<!-- gonk:pipeline-run:" + testAnchor[:len(testAnchor)-1] + " state=diagnosed -->"
	if got := RunMarkerStates(prefix, testAnchor); len(got) != 0 {
		t.Fatalf("a marker for an anchor PREFIX matched: %v", got)
	}
}

func TestRunMarkerStatesReportsUnknownState(t *testing.T) {
	body := "<!-- gonk:pipeline-run:" + testAnchor + " state=paused -->"
	got := RunMarkerStates(body, testAnchor)
	if len(got) != 1 || got[0] != "paused" || KnownRunState(got[0]) {
		t.Fatalf("unknown state must be returned (so the reader can fail closed), got %v", got)
	}
}

func TestRunMarkerRefusesBadInput(t *testing.T) {
	if _, err := RunMarker("gonk:42:issue:3", RunStateDiagnosed); err == nil {
		t.Error("a non-pipeline anchor was accepted")
	}
	if _, err := RunMarker(testAnchor, "paused"); err == nil {
		t.Error("an unknown state was accepted")
	}
}

func TestRunHandled(t *testing.T) {
	want := map[string]bool{
		RunStateDiagnosing: false, RunStateDeferred: false,
		RunStateDenied: true, RunStateDiagnosed: true, RunStateResolved: true, RunStateSuperseded: true,
		"": false, "paused": false,
	}
	for st, w := range want {
		if RunHandled(st) != w {
			t.Errorf("RunHandled(%q) = %v, want %v", st, !w, w)
		}
	}
	if len(RunStates()) != 6 {
		t.Fatalf("RunStates = %v; adding a state needs a RunHandled decision", RunStates())
	}
}

func TestRefIssueMarker(t *testing.T) {
	if got := RefIssueMarker("main"); got != "<!-- gonk:pipeline-ref:main -->" {
		t.Fatalf("RefIssueMarker = %q", got)
	}
	if strings.Contains(RefIssueMarker("main"), RefIssueMarker("mai")) {
		t.Fatal("a ref's marker contains a shorter ref's marker")
	}
}
