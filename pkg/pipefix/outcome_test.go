package pipefix

import (
	"reflect"
	"testing"

	"github.com/gonk-factory/gonk/pkg/gate"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// The verdict -> state -> meter outcome table of plan 7.1 and 7.3, written
// out by hand. `paid` is the outcome when meter's ledger shows the model
// answered (tokens > 0), `unpaid` when it shows nothing.
var outcomeTable = []struct {
	verdict   Verdict
	state     State
	present   bool
	paid      string
	unpaid    string
	escalates bool
}{
	{VerdictForged, StateNotFixed, false, meterapi.OutcomeGateFailed, meterapi.OutcomeInfraFailed, true},
	{VerdictNotFixed, StateNotFixed, false, meterapi.OutcomeGateFailed, meterapi.OutcomeInfraFailed, true},
	{VerdictFlaky, StateFlaky, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
	{VerdictUnverifiable, StateUnverifiable, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
	{VerdictRegressed, StateRegressed, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
	{VerdictFixedNewFailure, StateFixedNewFailure, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
	{VerdictFixed, StateFixed, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
}

func TestVerdictToStateToMeterOutcome(t *testing.T) {
	if len(outcomeTable) != len(Verdicts()) {
		t.Fatalf("the table has %d verdicts, Verdicts() %d", len(outcomeTable), len(Verdicts()))
	}
	for i, c := range outcomeTable {
		t.Run(string(c.verdict), func(t *testing.T) {
			if Verdicts()[i] != c.verdict {
				t.Errorf("Verdicts()[%d] = %q, want %q (J5's order)", i, Verdicts()[i], c.verdict)
			}
			st, known := StateFor(c.verdict)
			if !known || st != c.state {
				t.Fatalf("StateFor = %q, %v; want %q", st, known, c.state)
			}
			present, reported := ArtifactPresent(st)
			if !reported || present != c.present {
				t.Fatalf("ArtifactPresent(%q) = %v, %v; want %v, reported", st, present, reported, c.present)
			}
			paid, ok1 := VerdictOutcome(c.verdict, 1200)
			unpaid, ok2 := VerdictOutcome(c.verdict, 0)
			if !ok1 || !ok2 || paid != c.paid || unpaid != c.unpaid {
				t.Fatalf("outcome paid=%q unpaid=%q (reported %v %v); want %q %q", paid, unpaid, ok1, ok2, c.paid, c.unpaid)
			}
			if gate.Escalates(paid) != c.escalates {
				t.Errorf("escalates = %v, want %v", gate.Escalates(paid), c.escalates)
			}
			if gate.Escalates(unpaid) {
				t.Errorf("an attempt nobody paid for escalates")
			}
		})
	}
}

// Every state of 7.1: is an outcome reported from it, and which.
func TestStateOutcomes(t *testing.T) {
	type row struct {
		reported bool
		present  bool
		terminal bool
	}
	want := map[State]row{
		StateProposing:       {false, false, false},
		StateNeedsScope:      {true, true, true},
		StateAwaitingRun:     {false, false, false},
		StateVerifying:       {false, false, false},
		StateFixed:           {true, true, true},
		StateFixedNewFailure: {true, true, true},
		StateNotFixed:        {true, false, false},
		StateFlaky:           {true, true, true},
		StateUnverifiable:    {true, true, true},
		StateRegressed:       {true, true, true},
		StateDenied:          {false, false, true},
		StateTakenOver:       {true, true, true},
		StateClosedUnmerged:  {true, true, true},
		StateMerged:          {true, true, true},
		StateConfirmed:       {false, false, true},
		StateRecurred:        {false, false, true},
	}
	if len(want) != len(States()) {
		t.Fatalf("the table has %d states, States() %d", len(want), len(States()))
	}
	for _, s := range States() {
		w, listed := want[s]
		if !listed {
			t.Errorf("state %q is not in the table", s)
			continue
		}
		t.Run(string(s), func(t *testing.T) {
			if !KnownState(s) {
				t.Errorf("KnownState = false")
			}
			present, reported := ArtifactPresent(s)
			if reported != w.reported || present != w.present {
				t.Errorf("ArtifactPresent = %v, %v; want %v, %v", present, reported, w.present, w.reported)
			}
			if s.Terminal() != w.terminal {
				t.Errorf("Terminal = %v, want %v", s.Terminal(), w.terminal)
			}
			out, ok := MeterOutcome(s, 500)
			if ok != w.reported {
				t.Fatalf("MeterOutcome reported = %v, want %v", ok, w.reported)
			}
			if !w.reported {
				if out != "" {
					t.Errorf("an unreported state has outcome %q", out)
				}
				return
			}
			wantOut := gate.Classify(gate.Signals{ArtifactPresent: w.present, ModelTokens: 500})
			if out != wantOut {
				t.Errorf("MeterOutcome = %q, want gate.Classify's %q", out, wantOut)
			}
			if w.present && out != meterapi.OutcomeSuccess {
				t.Errorf("outcome %q, want success", out)
			}
			if !w.present && out != meterapi.OutcomeGateFailed {
				t.Errorf("outcome %q, want gate-failed", out)
			}
		})
	}
}

func TestOnlyNotFixedEscalates(t *testing.T) {
	var escalating []State
	for _, s := range States() {
		if out, ok := MeterOutcome(s, 1); ok && gate.Escalates(out) {
			escalating = append(escalating, s)
		}
	}
	if !reflect.DeepEqual(escalating, []State{StateNotFixed}) {
		t.Errorf("states that buy a rung: %v, want only not-fixed", escalating)
	}
}

func TestWhatIsNotAVerdictOrAStateHasNoOutcome(t *testing.T) {
	for _, v := range []Verdict{"", "PASSED", "fixed", "Fixed"} {
		if st, ok := StateFor(v); ok || st != "" {
			t.Errorf("StateFor(%q) = %q, %v", v, st, ok)
		}
		if out, ok := VerdictOutcome(v, 100); ok || out != "" {
			t.Errorf("VerdictOutcome(%q) = %q, %v", v, out, ok)
		}
	}
	for _, s := range []State{"", "done", "FIXED"} {
		if KnownState(s) || s.Terminal() {
			t.Errorf("state %q: known=%v terminal=%v", s, KnownState(s), s.Terminal())
		}
		if _, reported := ArtifactPresent(s); reported {
			t.Errorf("ArtifactPresent(%q) is reported", s)
		}
		if out, ok := MeterOutcome(s, 100); ok || out != "" {
			t.Errorf("MeterOutcome(%q) = %q, %v", s, out, ok)
		}
	}
}
