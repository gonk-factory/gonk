package pipefix

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// Every fixture in this package is INVENTED (plan section 0): a project that
// counts crates, with jobs compile / unit-tests / lint / package.

const (
	lineOld     = "FAILED test_tally - crate tally 3 != 4"
	lineOldVar  = "FAILED test_tally - crate tally 17 != 21" // the same failure, volatile parts aside
	lineNew     = "error: tally() takes no positional arguments"
	lineGeneric = "ERROR: Job failed: exit code 1"

	shaBase  = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
	shaFix   = "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1"
	shaOther = "0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d"
)

// target is a job of T that failed with `line` as its first error.
func target(name, line string) Target {
	return Target{
		Key: pipelinectx.JobFailureKey{
			Stage:          "test",
			Name:           name,
			FailureReason:  "script_failure",
			ErrorSignature: pipelinectx.ErrorSignature(line),
		},
		Generic: pipelinectx.GenericFirstError(line),
	}
}

// eligibleFacts is a run for which every row of 3.2 holds.
func eligibleFacts() EligibleFacts {
	t := target("unit-tests", lineOld)
	return EligibleFacts{
		Config: ConfigFacts{
			Loaded: true, PipelineFixes: true, Pipelines: true,
			Targets:  []string{pipelinectx.TargetKindRef},
			Classes:  []string{ClassCode, ClassTest},
			MaxChain: 5,
		},
		Run: RunFacts{
			Read: true, TargetKind: pipelinectx.TargetKindRef,
			NoteState: pipelinectx.RunStateDiagnosed, Open: true,
			Class: ClassCode, Targets: []Target{t},
		},
		Scope: ScopeFacts{
			Folded:   true,
			Envelope: []string{string(scope.ClassSource), string(scope.ClassDocs)},
			Cited:    []CitedFile{{Path: "crates/tally.py", Class: string(scope.ClassSource)}},
		},
		Head:    HeadFacts{Read: true, TargetSHA: shaBase, NewestFailingSHA: shaBase},
		Current: CurrentFacts{Read: true, FailedKeys: []pipelinectx.JobFailureKey{t.Key}},
		Dedup:   DedupFacts{Read: true},
		Serial:  SerialFacts{Read: true, OpenFixMRs: 0, MergedSinceGreen: 4},
	}
}

func TestTheBaselineRunIsEligible(t *testing.T) {
	ok, why := Eligible(eligibleFacts())
	if !ok || !reflect.DeepEqual(why, Reason{}) {
		t.Fatalf("Eligible(baseline) = %v, %+v; want true and the zero Reason", ok, why)
	}
	if why.Row() != "" {
		t.Errorf("Row() of the zero Reason = %q, want \"\"", why.Row())
	}
}

type eligibleCase struct {
	name string
	row  string
	code ReasonCode
	mut  func(*EligibleFacts)
	// what the plan's "On failure" column says
	note, needsHuman, needsScope bool
	text                         string // exact, when the plan gives the words
}

// eligibleRows: one fixture per predicate of every row E1-E12. Each is the
// eligible baseline with ONE fact changed, so it fails exactly one predicate,
// and the test asserts WHICH by code.
var eligibleRows = []eligibleCase{
	{name: "E1 pipeline_fixes is off", row: "E1", code: E1FixesOff,
		mut: func(f *EligibleFacts) { f.Config.PipelineFixes = false }},
	{name: "E1 pipelines is off", row: "E1", code: E1PipelinesOff,
		mut: func(f *EligibleFacts) { f.Config.Pipelines = false }},
	{name: "E2 an mr target on a project that enabled ref only", row: "E2", code: E2TargetKind,
		mut: func(f *EligibleFacts) { f.Run.TargetKind = pipelinectx.TargetKindMR }},
	{name: "E2 no target kind enabled", row: "E2", code: E2TargetKind,
		mut: func(f *EligibleFacts) { f.Config.Targets = nil }},
	{name: "E3 the run note is still diagnosing", row: "E3", code: E3NoteState,
		mut: func(f *EligibleFacts) { f.Run.NoteState = pipelinectx.RunStateDiagnosing }},
	{name: "E3 the run note is resolved", row: "E3", code: E3NoteState,
		mut: func(f *EligibleFacts) { f.Run.NoteState = pipelinectx.RunStateResolved }},
	{name: "E3 the run is not open", row: "E3", code: E3RunClosed,
		mut: func(f *EligibleFacts) { f.Run.Open = false }},
	{name: "E3 the note says diagnosed and the record holds no diagnosis", row: "E3", code: E3NoDiagnosis,
		mut: func(f *EligibleFacts) { f.Run.Class = "" }},
	{name: "E4 flaky", row: "E4", code: E4NeverEligible, note: true,
		text: "no fix attempted: classified `flaky`",
		mut:  func(f *EligibleFacts) { f.Run.Class = string(effects.ClassFlaky) }},
	{name: "E4 infrastructure", row: "E4", code: E4NeverEligible, note: true,
		text: "no fix attempted: classified `infrastructure`",
		mut:  func(f *EligibleFacts) { f.Run.Class = string(effects.ClassInfrastructure) }},
	{name: "E4 unknown", row: "E4", code: E4NeverEligible, note: true,
		text: "no fix attempted: classified `unknown`",
		mut:  func(f *EligibleFacts) { f.Run.Class = string(effects.ClassUnknown) }},
	{name: "E4 a class nobody defined", row: "E4", code: E4NeverEligible, note: true,
		text: "no fix attempted: classified `cosmic-rays`",
		mut:  func(f *EligibleFacts) { f.Run.Class = "cosmic-rays" }},
	{name: "E4 configuration, which this project did not enable", row: "E4", code: E4NotEnabled, note: true,
		text: "no fix attempted: classified `configuration`",
		mut:  func(f *EligibleFacts) { f.Run.Class = ClassConfiguration }},
	{name: "E5 the diagnosis cites no job", row: "E5", code: E5NoTargets, note: true,
		mut: func(f *EligibleFacts) { f.Run.Targets = nil; f.Current.FailedKeys = nil }},
	{name: "E5 a cited job cannot be judged", row: "E5", code: E5Unjudgeable, note: true,
		mut: func(f *EligibleFacts) {
			f.Run.TargetProblems = []string{`job "lint" has no recorded failure facts`}
		}},
	{name: "E6 a cited file is a test, and the class is code", row: "E6", code: E6OutsideEnvelope,
		note: true, needsHuman: true, needsScope: true,
		mut: func(f *EligibleFacts) {
			f.Scope.Cited = append(f.Scope.Cited, CitedFile{Path: "crates/tally_test.py", Class: string(scope.ClassTest)})
		}},
	{name: "E6 the only cited file is the CI definition", row: "E6", code: E6OutsideEnvelope,
		note: true, needsHuman: true, needsScope: true,
		mut: func(f *EligibleFacts) {
			f.Scope.Cited = []CitedFile{{Path: ".gitlab-ci.yml", Class: string(scope.ClassCI)}}
		}},
	{name: "E6 the envelope is empty", row: "E6", code: E6OutsideEnvelope,
		note: true, needsHuman: true, needsScope: true,
		mut: func(f *EligibleFacts) { f.Scope.Envelope = nil }},
	{name: "E6 the diagnosis cites no file", row: "E6", code: E6NoCitedFiles,
		note: true, needsHuman: true, needsScope: true,
		mut: func(f *EligibleFacts) { f.Scope.Cited = nil }},
	{name: "E7 the target moved past the newest failing pipeline", row: "E7", code: E7HeadMoved,
		mut: func(f *EligibleFacts) { f.Head.TargetSHA = shaOther }},
	{name: "E8 the cited job now fails with another error", row: "E8", code: E8Drifted, note: true,
		text: "the same jobs now fail differently; not fixing a stale diagnosis",
		mut: func(f *EligibleFacts) {
			f.Current.FailedKeys = []pipelinectx.JobFailureKey{target("unit-tests", lineNew).Key}
		}},
	{name: "E8 the cited job no longer fails; another one does", row: "E8", code: E8Drifted, note: true,
		text: "the same jobs now fail differently; not fixing a stale diagnosis",
		mut: func(f *EligibleFacts) {
			f.Current.FailedKeys = []pipelinectx.JobFailureKey{target("lint", lineOld).Key}
		}},
	{name: "E8 the cited job fails for another failure_reason", row: "E8", code: E8Drifted, note: true,
		text: "the same jobs now fail differently; not fixing a stale diagnosis",
		mut: func(f *EligibleFacts) {
			k := target("unit-tests", lineOld).Key
			k.FailureReason = "job_execution_timeout"
			f.Current.FailedKeys = []pipelinectx.JobFailureKey{k}
		}},
	{name: "E9 the fix already ended (flaky)", row: "E9", code: E9FixTerminal,
		mut: func(f *EligibleFacts) { f.Dedup.FixState = StateFlaky }},
	{name: "E9 the fix already ended (closed-unmerged)", row: "E9", code: E9FixTerminal,
		mut: func(f *EligibleFacts) { f.Dedup.FixState = StateClosedUnmerged }},
	{name: "E9 a fix is in flight (proposing)", row: "E9", code: E9FixInFlight,
		mut: func(f *EligibleFacts) { f.Dedup.FixState = StateProposing }},
	{name: "E9 a fix is in flight (not-fixed, awaiting its next attempt)", row: "E9", code: E9FixInFlight,
		mut: func(f *EligibleFacts) { f.Dedup.FixState = StateNotFixed }},
	{name: "E9 a human closed a fix for this cause", row: "E9", code: E9ClosedUnmerged, note: true,
		mut: func(f *EligibleFacts) { f.Dedup.ClosedUnmergedCauseMRs = 1 }},
	{name: "E9 a fix MR for this cause is open", row: "E9", code: E9OpenCauseMR,
		mut: func(f *EligibleFacts) { f.Dedup.OpenCauseMRs = 1 }},
	{name: "E10 another fix MR is open on this target", row: "E10", code: E10OpenFixMR,
		mut: func(f *EligibleFacts) { f.Serial.OpenFixMRs = 1 }},
	{name: "E11 the chain is at max_chain", row: "E11", code: E11ChainExhausted, note: true, needsHuman: true,
		mut: func(f *EligibleFacts) { f.Serial.MergedSinceGreen = 5 }},
	{name: "E11 the chain is past max_chain", row: "E11", code: E11ChainExhausted, note: true, needsHuman: true,
		mut: func(f *EligibleFacts) { f.Serial.MergedSinceGreen = 9 }},
	{name: "E11 max_chain is zero", row: "E11", code: E11ChainExhausted, note: true, needsHuman: true,
		mut: func(f *EligibleFacts) { f.Config.MaxChain = 0; f.Serial.MergedSinceGreen = 0 }},
	{name: "E12 the run is held by a cross-project roll-up", row: "E12", code: E12CorrelationHold, note: true,
		mut: func(f *EligibleFacts) { f.CorrelationHold = true }},
}

func TestEligibleRows(t *testing.T) {
	for _, c := range eligibleRows {
		t.Run(c.name, func(t *testing.T) {
			f := eligibleFacts()
			c.mut(&f)
			ok, why := Eligible(f)
			if ok {
				t.Fatalf("eligible; want refused by %s", c.code)
			}
			if why.Code != c.code {
				t.Fatalf("refused by %q (%s); want %q", why.Code, why.Text, c.code)
			}
			if why.Row() != c.row {
				t.Errorf("Row() = %q, want %q", why.Row(), c.row)
			}
			if why.Unknown {
				t.Errorf("Unknown is set: a row that was EVALUATED is a decision, not a missing fact")
			}
			if why.Note != c.note || why.NeedsHuman != c.needsHuman || why.NeedsScope != c.needsScope {
				t.Errorf("note=%v needs-human=%v needs-scope=%v; want %v %v %v",
					why.Note, why.NeedsHuman, why.NeedsScope, c.note, c.needsHuman, c.needsScope)
			}
			if why.Text == "" {
				t.Errorf("no reason text")
			}
			if c.text != "" && why.Text != c.text {
				t.Errorf("text = %q, want %q", why.Text, c.text)
			}
		})
	}
}

// No configuration makes flaky / infrastructure / unknown eligible (E4, owner
// decision in 14.1): the project listing the class changes nothing.
func TestNoConfigMakesAnUnfixableClassEligible(t *testing.T) {
	for _, class := range effects.Classifications() {
		fixable := class == effects.ClassCode || class == effects.ClassTest || class == effects.ClassConfiguration
		t.Run(string(class), func(t *testing.T) {
			f := eligibleFacts()
			f.Run.Class = string(class)
			f.Config.Classes = []string{string(class)}
			f.Scope.Envelope = []string{string(scope.ClassSource)}
			ok, why := Eligible(f)
			if ok != fixable {
				t.Fatalf("class %q listed in pipeline_fix.classes: eligible=%v (%s), want %v", class, ok, why.Code, fixable)
			}
			if !fixable && why.Code != E4NeverEligible {
				t.Errorf("refused by %q, want %q", why.Code, E4NeverEligible)
			}
		})
	}
}

func TestFixableClassesAreThePlansThree(t *testing.T) {
	want := []string{string(effects.ClassCode), string(effects.ClassTest), string(effects.ClassConfiguration)}
	if got := FixableClasses(); !reflect.DeepEqual(got, want) {
		t.Errorf("FixableClasses() = %v, want %v", got, want)
	}
	if LiftCI != string(scope.ClassCI) {
		t.Errorf("LiftCI = %q, want scope.ClassCI %q", LiftCI, scope.ClassCI)
	}
}

// A row whose facts were not read refuses as Unknown: nothing decided,
// nothing noted, and never eligible.
func TestAnUnreadFactIsNeverEligibility(t *testing.T) {
	cases := []struct {
		name string
		code ReasonCode
		mut  func(*EligibleFacts)
	}{
		{"the config was not loaded", E1Unread, func(f *EligibleFacts) { f.Config.Loaded = false }},
		{"the run was not read", E2Unread, func(f *EligibleFacts) { f.Run.Read = false }},
		{"the envelope was not folded", E6Unread, func(f *EligibleFacts) { f.Scope.Folded = false }},
		{"a cited file was not classified", E6Unread, func(f *EligibleFacts) { f.Scope.Cited[0].Class = "" }},
		{"the head was not read", E7Unread, func(f *EligibleFacts) { f.Head.Read = false }},
		{"the head SHA is empty", E7Unread, func(f *EligibleFacts) { f.Head.TargetSHA = "" }},
		{"the newest failing SHA is empty", E7Unread, func(f *EligibleFacts) { f.Head.NewestFailingSHA = "" }},
		{"both SHAs are empty (equal, and not a fact)", E7Unread, func(f *EligibleFacts) {
			f.Head.TargetSHA, f.Head.NewestFailingSHA = "", ""
		}},
		{"the current failure keys were not computed", E8Unread, func(f *EligibleFacts) { f.Current.Read = false }},
		{"the fix record and MRs were not read", E9Unread, func(f *EligibleFacts) { f.Dedup.Read = false }},
		{"the fix record holds a state nobody defined", E9Unread, func(f *EligibleFacts) { f.Dedup.FixState = "half-done" }},
		{"the target's fix MRs were not read", E10Unread, func(f *EligibleFacts) { f.Serial.Read = false }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := eligibleFacts()
			c.mut(&f)
			ok, why := Eligible(f)
			if ok {
				t.Fatalf("eligible with a fact unread")
			}
			if why.Code != c.code || !why.Unknown {
				t.Fatalf("got %q unknown=%v, want %q unknown=true", why.Code, why.Unknown, c.code)
			}
			if why.Note || why.NeedsHuman || why.NeedsScope {
				t.Errorf("an unread fact wrote something: %+v", why)
			}
		})
	}
	t.Run("the zero value", func(t *testing.T) {
		ok, why := Eligible(EligibleFacts{})
		if ok || !why.Unknown || why.Code != E1Unread {
			t.Fatalf("Eligible(zero) = %v, %+v; want refused, unknown, %s", ok, why, E1Unread)
		}
	})
}

// The plan's order IS the contract ("in this order; the first failure is the
// logged reason"). Start from a run that fails EVERY row and repair one row
// at a time: the reason must walk E1 .. E12 and end at eligible.
func TestTheFirstFailingRowIsTheReason(t *testing.T) {
	f := eligibleFacts()
	f.Config.PipelineFixes = false                     // E1
	f.Run.TargetKind = pipelinectx.TargetKindMR        // E2
	f.Run.Open = false                                 // E3
	f.Run.Class = string(effects.ClassFlaky)           // E4
	f.Run.Targets = nil                                // E5
	f.Scope.Cited[0].Class = string(scope.ClassDeploy) // E6
	f.Head.TargetSHA = shaOther                        // E7
	f.Current.FailedKeys = nil                         // E8 (once T is back)
	f.Dedup.ClosedUnmergedCauseMRs = 1                 // E9
	f.Serial.OpenFixMRs = 2                            // E10
	f.Serial.MergedSinceGreen = 5                      // E11
	f.CorrelationHold = true                           // E12
	good := eligibleFacts()
	steps := []struct {
		want   string
		repair func()
	}{
		{"E1", func() { f.Config.PipelineFixes = true }},
		{"E2", func() { f.Run.TargetKind = good.Run.TargetKind }},
		{"E3", func() { f.Run.Open = true }},
		{"E4", func() { f.Run.Class = good.Run.Class }},
		{"E5", func() { f.Run.Targets = good.Run.Targets }},
		{"E6", func() { f.Scope = good.Scope }},
		{"E7", func() { f.Head = good.Head }},
		{"E8", func() { f.Current = good.Current }},
		{"E9", func() { f.Dedup = good.Dedup }},
		{"E10", func() { f.Serial.OpenFixMRs = 0 }},
		{"E11", func() { f.Serial.MergedSinceGreen = 0 }},
		{"E12", func() { f.CorrelationHold = false }},
	}
	for _, s := range steps {
		ok, why := Eligible(f)
		if ok || why.Row() != s.want {
			t.Fatalf("with rows %s.. still failing: eligible=%v reason=%q, want row %s", s.want, ok, why.Code, s.want)
		}
		s.repair()
	}
	if ok, why := Eligible(f); !ok {
		t.Fatalf("every row repaired, still refused: %+v", why)
	}
}

// An unread fact fails AT ITS PLACE in the order: an earlier row that is
// decidable still decides (a project that did not opt in is answered
// silently even when GitLab is down).
func TestAnUnreadFactDoesNotHideAnEarlierDecision(t *testing.T) {
	f := eligibleFacts()
	f.Config.PipelineFixes = false
	f.Head.Read, f.Current.Read, f.Dedup.Read, f.Serial.Read = false, false, false, false
	if ok, why := Eligible(f); ok || why.Code != E1FixesOff || why.Unknown {
		t.Fatalf("got %v %+v, want refused by %s, decided", ok, why, E1FixesOff)
	}
	f = eligibleFacts()
	f.Head.Read = false
	f.Serial.OpenFixMRs = 3
	if ok, why := Eligible(f); ok || why.Code != E7Unread {
		t.Fatalf("got %v %+v, want %s: E10 may not be reached past an unread E7", ok, why, E7Unread)
	}
}

func TestEveryTargetMustStillFailTheSameWay(t *testing.T) {
	f := eligibleFacts()
	second := target("lint", lineNew)
	f.Run.Targets = append(f.Run.Targets, second)
	// only the first target's key is among the current failures
	if ok, why := Eligible(f); ok || why.Code != E8Drifted {
		t.Fatalf("one of two targets no longer fails: got %v %q, want %s", ok, why.Code, E8Drifted)
	}
	f.Current.FailedKeys = append(f.Current.FailedKeys, second.Key)
	// an uncited job failing as well changes nothing
	f.Current.FailedKeys = append(f.Current.FailedKeys, target("package", lineNew).Key)
	if ok, why := Eligible(f); !ok {
		t.Fatalf("both targets fail as recorded: refused by %q", why.Code)
	}
}

func TestMaxChainIsAStrictBound(t *testing.T) {
	for merged, want := range map[int]bool{0: true, 4: true, 5: false, 6: false} {
		f := eligibleFacts()
		f.Serial.MergedSinceGreen = merged
		if ok, _ := Eligible(f); ok != want {
			t.Errorf("max_chain 5, %d merged since green: eligible=%v, want %v", merged, ok, want)
		}
	}
}

// ---- T ----

func recorded(name, line string) RecordedJob {
	return RecordedJob{
		Name: name, Stage: "test", FailureReason: "script_failure",
		ErrorSignature: pipelinectx.ErrorSignature(line),
		Generic:        pipelinectx.GenericFirstError(line),
		FactsRecorded:  true,
	}
}

func TestTargetSet(t *testing.T) {
	jobs := []RecordedJob{recorded("unit-tests", lineOld), recorded("lint", lineGeneric), recorded("package", lineNew)}
	t.Run("the cited jobs, in first-cited order, uncited ones left out", func(t *testing.T) {
		got, problems := TargetSet(jobs, []string{"lint", "unit-tests", "lint"})
		want := []Target{target("lint", lineGeneric), target("unit-tests", lineOld)}
		if !reflect.DeepEqual(got, want) || len(problems) != 0 {
			t.Fatalf("TargetSet = %+v, problems %v\nwant %+v, none", got, problems, want)
		}
		if !got[0].Generic || got[1].Generic {
			t.Errorf("generic flags = %v %v, want true false", got[0].Generic, got[1].Generic)
		}
		if got[1].Name() != "unit-tests" {
			t.Errorf("Name() = %q", got[1].Name())
		}
	})
	t.Run("a job that was allowed to fail is not in T", func(t *testing.T) {
		js := append([]RecordedJob(nil), jobs...)
		js[0].AllowFailure = true
		got, problems := TargetSet(js, []string{"unit-tests", "package"})
		if len(got) != 1 || got[0].Name() != "package" || len(problems) != 0 {
			t.Fatalf("TargetSet = %+v, problems %v; want [package], none", got, problems)
		}
	})
	t.Run("a job with no signature is generic whatever the flag says", func(t *testing.T) {
		js := []RecordedJob{{Name: "unit-tests", Stage: "test", FailureReason: "script_failure", FactsRecorded: true}}
		got, _ := TargetSet(js, []string{"unit-tests"})
		if len(got) != 1 || !got[0].Generic {
			t.Fatalf("TargetSet = %+v; want one generic target", got)
		}
	})
	t.Run("a cited job that is not recorded is a problem", func(t *testing.T) {
		got, problems := TargetSet(jobs, []string{"unit-tests", "deploy"})
		if len(got) != 1 || len(problems) != 1 || !strings.Contains(problems[0], `"deploy"`) {
			t.Fatalf("TargetSet = %+v, problems %v; want one target and a problem naming deploy", got, problems)
		}
	})
	t.Run("a job recorded twice is a problem", func(t *testing.T) {
		js := append([]RecordedJob{recorded("unit-tests", lineNew)}, jobs...)
		got, problems := TargetSet(js, []string{"unit-tests"})
		if len(got) != 0 || len(problems) != 1 {
			t.Fatalf("TargetSet = %+v, problems %v; want no target and one problem", got, problems)
		}
	})
	t.Run("a job recorded before the failure facts existed is a problem", func(t *testing.T) {
		js := []RecordedJob{{Name: "unit-tests"}}
		got, problems := TargetSet(js, []string{"unit-tests"})
		if len(got) != 0 || len(problems) != 1 {
			t.Fatalf("TargetSet = %+v, problems %v; want no target and one problem", got, problems)
		}
	})
	t.Run("no evidence job: an empty T and no problem", func(t *testing.T) {
		got, problems := TargetSet(jobs, nil)
		if len(got) != 0 || len(problems) != 0 {
			t.Fatalf("TargetSet = %+v, problems %v", got, problems)
		}
	})
}

func TestCauseKeyIsPipelinectxsOverTheTargetKeys(t *testing.T) {
	ts := []Target{target("unit-tests", lineOld), target("lint", lineNew)}
	want := pipelinectx.CauseKey([]pipelinectx.JobFailureKey{ts[0].Key, ts[1].Key})
	if got := CauseKey(ts); got != want || got == "" {
		t.Errorf("CauseKey = %q, want %q", got, want)
	}
	if got := CauseKey(nil); got != "" {
		t.Errorf("CauseKey(nil) = %q, want \"\"", got)
	}
}

// ---- the rules page's table and this test's table agree ----
//
// The E and C tables are the product's rules and live in the reference, which
// is published with the code (they used to be sections 3.2 and 6.4 of a build
// plan, which is not). The tests keep their names.

const rulesFile = "../../docs/reference/gonk-pipeline-fix-rules.md"

// planRows returns the first-column codes (E1, C7, ...) of the table rows
// that start with the given letter, in the section of the rules page whose
// heading is `heading` (up to the next "## " heading).
func planRows(t *testing.T, heading, letter string) []string {
	t.Helper()
	b, err := os.ReadFile(rulesFile)
	if err != nil {
		t.Fatalf("read the rules page: %v", err)
	}
	text := string(b)
	i := strings.Index(text, "\n"+heading+"\n")
	if i < 0 {
		t.Fatalf("%s has no heading %q", rulesFile, heading)
	}
	text = text[i+1+len(heading):]
	if j := strings.Index(text, "\n## "); j >= 0 {
		text = text[:j]
	}
	re := regexp.MustCompile(`(?m)^\| (` + letter + `\d+) \|`)
	var rows []string
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		rows = append(rows, m[1])
	}
	return rows
}

func TestEveryPlanERowHasAFixture(t *testing.T) {
	want := planRows(t, "## 1. Eligibility: what starts a fix dispatch (E rows)", "E")
	if len(want) != 12 {
		t.Fatalf("the rules page lists %d E rows (%v), expected 12: its section 1 changed, re-read it", len(want), want)
	}
	have := map[string]bool{}
	for _, c := range eligibleRows {
		have[c.row] = true
		if !strings.HasPrefix(string(c.code), c.row+".") {
			t.Errorf("fixture %q: code %q is not of row %s", c.name, c.code, c.row)
		}
	}
	var got []string
	for r := range have {
		got = append(got, r)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Errorf("rows with a fixture: %v\nrows on the page:     %v", got, sorted)
	}
}
