package runtime_test

import (
	"testing"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// A zero Status must not read as "still pending": the sweep waits on
// Pending, so an implementation that forgot to set Phase would stall a bead
// until its reservation expired with nothing in the log saying why.
func TestZeroPhaseIsUnknownNotPending(t *testing.T) {
	var s runtime.Status
	if s.Phase != runtime.PhaseUnknown {
		t.Fatalf("zero Status.Phase = %v, want Unknown", s.Phase)
	}
	if s.Phase == runtime.PhasePending || s.Phase.Finished() {
		t.Fatalf("zero Status.Phase = %v reads as a real phase", s.Phase)
	}
}

func TestPhaseStringAndFinished(t *testing.T) {
	for _, tc := range []struct {
		p        runtime.Phase
		name     string
		finished bool
	}{
		{runtime.PhaseUnknown, "Unknown", false},
		{runtime.PhasePending, "Pending", false},
		{runtime.PhaseRunning, "Running", false},
		{runtime.PhaseSucceeded, "Succeeded", true},
		{runtime.PhaseFailed, "Failed", true},
		{runtime.PhaseNotFound, "NotFound", false},
		{runtime.Phase(99), "Phase(99)", false},
	} {
		if got := tc.p.String(); got != tc.name {
			t.Errorf("Phase(%d).String() = %q, want %q", int(tc.p), got, tc.name)
		}
		if got := tc.p.Finished(); got != tc.finished {
			t.Errorf("%s.Finished() = %v, want %v", tc.name, got, tc.finished)
		}
	}
}

func TestHandleIsTheName(t *testing.T) {
	n := runtime.Name("triage", 42, runtime.KindIssue, 7, 1)
	if h := runtime.Handle(n); h.String() != n {
		t.Fatalf("Handle(%q).String() = %q", n, h.String())
	}
}
