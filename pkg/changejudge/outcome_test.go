package changejudge

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/gate"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// The verdict -> state -> meter outcome table of plan 2.5, written out by
// hand. `paid` is the outcome when meter's ledger shows the model answered
// (tokens > 0), `unpaid` when it shows nothing.
var outcomeTable = []struct {
	verdict   Verdict
	state     State
	present   bool
	paid      string
	unpaid    string
	escalates bool
}{
	{VerdictNotGreen, StateNotVerified, false, meterapi.OutcomeGateFailed, meterapi.OutcomeInfraFailed, true},
	{VerdictTestsNotRun, StateNotVerified, false, meterapi.OutcomeGateFailed, meterapi.OutcomeInfraFailed, true},
	{VerdictFlaky, StateInconclusive, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
	{VerdictUnverifiable, StateInconclusive, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
	{VerdictGreenUnobserved, StateVerifiedUnobserved, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
	{VerdictGreen, StateVerified, true, meterapi.OutcomeSuccess, meterapi.OutcomeSuccess, false},
}

func TestVerdictToStateToMeterOutcome(t *testing.T) {
	if len(outcomeTable) != len(Verdicts()) {
		t.Fatalf("the table has %d verdicts, Verdicts() %d", len(outcomeTable), len(Verdicts()))
	}
	for i, c := range outcomeTable {
		t.Run(string(c.verdict), func(t *testing.T) {
			if Verdicts()[i] != c.verdict {
				t.Errorf("Verdicts()[%d] = %q, want %q (K5's order)", i, Verdicts()[i], c.verdict)
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

// Every state: is an outcome reported from it, which, and what it shows.
func TestStateOutcomes(t *testing.T) {
	type row struct {
		reported   bool
		present    bool
		terminal   bool
		mr         []string
		needsHuman bool
	}
	want := map[State]row{
		StateProposing:          {false, false, false, nil, false},
		StateNeedsScope:         {true, true, true, nil, true},
		StateVerifying:          {false, false, false, []string{LabelVerifying}, false},
		StateNotVerified:        {true, false, false, []string{LabelVerifying}, false},
		StateVerified:           {true, true, true, []string{LabelVerified, LabelTestsObserved}, false},
		StateVerifiedUnobserved: {true, true, true, []string{LabelVerified, LabelTestsUnobserved}, false},
		StateInconclusive:       {true, true, true, []string{LabelInconclusive}, true},
		StateDenied:             {false, false, true, nil, false},
		StateTakenOver:          {true, true, true, nil, false},
		StateClosedUnmerged:     {true, true, true, nil, false},
		StateMerged:             {true, true, true, nil, false},
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
			labels := LabelsFor(s, TestsObserved)
			if !reflect.DeepEqual(labels.MR, w.mr) || labels.IssueNeedsHuman != w.needsHuman {
				t.Errorf("LabelsFor = %+v; want MR %v, needs-human %v", labels, w.mr, w.needsHuman)
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

func TestOnlyNotVerifiedEscalates(t *testing.T) {
	var escalating []State
	for _, s := range States() {
		if out, ok := MeterOutcome(s, 1); ok && gate.Escalates(out) {
			escalating = append(escalating, s)
		}
	}
	if !reflect.DeepEqual(escalating, []State{StateNotVerified}) {
		t.Errorf("states that buy a rung: %v, want only not-verified", escalating)
	}
}

func TestWhatIsNotAVerdictOrAStateHasNoOutcome(t *testing.T) {
	for _, v := range []Verdict{"", "PASSED", "green", "Green", "FIXED"} {
		if st, ok := StateFor(v); ok || st != "" {
			t.Errorf("StateFor(%q) = %q, %v", v, st, ok)
		}
		if out, ok := VerdictOutcome(v, 100); ok || out != "" {
			t.Errorf("VerdictOutcome(%q) = %q, %v", v, out, ok)
		}
	}
	for _, s := range []State{"", "done", "GREEN", "fixed"} {
		if KnownState(s) || s.Terminal() {
			t.Errorf("state %q: known=%v terminal=%v", s, KnownState(s), s.Terminal())
		}
		if _, reported := ArtifactPresent(s); reported {
			t.Errorf("ArtifactPresent(%q) is reported", s)
		}
		if out, ok := MeterOutcome(s, 100); ok || out != "" {
			t.Errorf("MeterOutcome(%q) = %q, %v", s, out, ok)
		}
		if l := LabelsFor(s, TestsObserved); len(l.MR) != 0 || l.IssueNeedsHuman {
			t.Errorf("LabelsFor(%q) = %+v", s, l)
		}
	}
}

// `verified` says "tests observed" only when they were. Whatever the finding,
// no other state ever carries that label, and `verified-unobserved` never
// does.
func TestTheTestsObservedLabelNeedsObservedTests(t *testing.T) {
	findings := []Finding{"", TestsNotApplicable, TestsUnknown, TestsUnobserved, TestsReduced, TestsNewNotRun, TestsFailed, TestsObserved}
	for _, s := range States() {
		for _, f := range findings {
			has := false
			for _, l := range LabelsFor(s, f).MR {
				has = has || l == LabelTestsObserved
			}
			if want := s == StateVerified && f == TestsObserved; has != want {
				t.Errorf("LabelsFor(%q, %q) carries tests-observed = %v, want %v", s, f, has, want)
			}
		}
	}
	docs := LabelsFor(StateVerified, TestsNotApplicable)
	if !reflect.DeepEqual(docs.MR, []string{LabelVerified}) {
		t.Errorf("a verified docs-only change is labelled %v; want only %s", docs.MR, LabelVerified)
	}
	// And from end to end: a docs-only GREEN.
	f := goodFacts()
	f.Change.DiffClasses = []string{ClassDocs}
	r := Judge(f, tEarly)
	if got := LabelsFor(r.State, r.Tests.Finding).MR; r.Verdict != VerdictGreen || !reflect.DeepEqual(got, []string{LabelVerified}) {
		t.Errorf("docs-only: verdict %q labels %v", r.Verdict, got)
	}
}

func TestTheLabels(t *testing.T) {
	want := []string{"gonk::change-verifying", "gonk::change-verified", "gonk::change-tests-observed",
		"gonk::change-tests-unobserved", "gonk::change-inconclusive"}
	if !reflect.DeepEqual(MRLabels(), want) {
		t.Fatalf("MRLabels() = %v, want %v", MRLabels(), want)
	}
	if LabelImplement != "gonk::implement" || LabelNeedsHuman != "gonk::needs-human" {
		t.Fatalf("implement %q, needs-human %q", LabelImplement, LabelNeedsHuman)
	}
	// Every label a state shows is one of MRLabels: the controller can only
	// remove what it knows it may have written. And the human's label is
	// never one of them.
	known := map[string]bool{}
	for _, l := range MRLabels() {
		known[l] = true
		if !strings.HasPrefix(l, "gonk::change-") {
			t.Errorf("label %q is outside the reserved gonk::change- prefix", l)
		}
	}
	if known[LabelImplement] {
		t.Errorf("the controller writes the label a human sets")
	}
	shown := map[string]bool{}
	for _, s := range States() {
		for _, f := range []Finding{TestsObserved, TestsNotApplicable, ""} {
			for _, l := range LabelsFor(s, f).MR {
				shown[l] = true
				if !known[l] {
					t.Errorf("state %q shows %q, which MRLabels() does not list", s, l)
				}
			}
		}
	}
	if len(shown) != len(known) {
		t.Errorf("labels shown %v; MRLabels() lists %v", shown, known)
	}
}
