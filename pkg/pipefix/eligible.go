package pipefix

import (
	"fmt"
	"strings"

	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// The diagnosis classes Eligible knows (effects.Classification values; a test
// pins them to that package). FixableClasses is the CLOSED set of plan 3.2
// E4: no configuration can make any other class eligible.
const (
	ClassCode          = "code"
	ClassTest          = "test"
	ClassConfiguration = "configuration"
)

// FixableClasses returns E4's closed set: the only diagnosis classes a fix is
// ever dispatched for. `flaky`, `infrastructure` and `unknown` are not in it
// and nothing in EligibleFacts can add them.
func FixableClasses() []string { return []string{ClassCode, ClassTest, ClassConfiguration} }

// EligibleFacts is everything plan 3.2's predicate reads. Each group is
// filled by the controller from one source and carries a marker saying it
// was read; the zero value of a group is "not read".
type EligibleFacts struct {
	Config  ConfigFacts
	Run     RunFacts
	Scope   ScopeFacts
	Head    HeadFacts
	Current CurrentFacts
	Dedup   DedupFacts
	Serial  SerialFacts
	// CorrelationHold (E12): the run is linked into a roll-up whose common
	// cause is outside this project. Nothing sets it until gonk-lreg exists;
	// it is the one fact whose zero value is deliberately "holds nothing".
	CorrelationHold bool
}

// ConfigFacts is the EFFECTIVE configuration (after the instance / group /
// project fold), read at the pinned base SHA of the default branch.
type ConfigFacts struct {
	Loaded bool
	// PipelineFixes, Pipelines: `actions.pipeline_fixes`, `actions.pipelines`.
	PipelineFixes bool
	Pipelines     bool
	// Targets: `pipeline_fix.targets`, a subset of [ref, mr].
	Targets []string
	// Classes: `pipeline_fix.classes`.
	Classes []string
	// MaxChain: `pipeline_fix.max_chain`. Below 1 nothing is eligible.
	MaxChain int
}

// RunFacts is the diagnosed run, from the record and its run note.
type RunFacts struct {
	Read bool
	// TargetKind: pipelinectx.TargetKindRef or TargetKindMR.
	TargetKind string
	// NoteState: the run note's state (pipelinectx.RunState*).
	NoteState string
	// Open: Record.PipelineRunOpen.
	Open bool
	// Class: Record.PipelineDiagnosis.Class; "" when no diagnosis is on the
	// record.
	Class string
	// Targets, TargetProblems: TargetSet over the record.
	Targets        []Target
	TargetProblems []string
}

// ScopeFacts is E6's input: the envelope the controller chose for this
// diagnosis class (pack row AND operator AND project config, plan 4.2) and
// the path class of every file the diagnosis cites.
type ScopeFacts struct {
	Folded bool
	// Envelope: the writable path classes (pkg/scope Class values).
	Envelope []string
	// Cited: EVERY file of Record.PipelineDiagnosis.Files with its path
	// class. A file whose class is "" was not classified.
	Cited []CitedFile
}

// CitedFile is one file the diagnosis cites and its path class.
type CitedFile struct {
	Path  string
	Class string
}

// HeadFacts is E7's input.
type HeadFacts struct {
	Read bool
	// TargetSHA: the head SHA of the target (branch or MR) now.
	TargetSHA string
	// NewestFailingSHA: the SHA of the run's newest failing pipeline
	// (Record.PipelineLatestFailed).
	NewestFailingSHA string
}

// CurrentFacts is E8's input: the job failure key, recomputed controller-side
// from GitLab's job object and trace, of every FAILED job of the run's newest
// failing pipeline. Read is false when any of those could not be computed (a
// trace that could not be read).
type CurrentFacts struct {
	Read       bool
	FailedKeys []pipelinectx.JobFailureKey
}

// DedupFacts is E9's input.
type DedupFacts struct {
	Read bool
	// FixState: the state of the fix record for this fix anchor; "" when
	// there is none.
	FixState State
	// OpenCauseMRs, ClosedUnmergedCauseMRs: bot-authored merge requests whose
	// metadata block carries this run's cause key.
	OpenCauseMRs           int
	ClosedUnmergedCauseMRs int
}

// SerialFacts is E10's and E11's input, both about the TARGET.
type SerialFacts struct {
	Read bool
	// OpenFixMRs: open bot-authored fix MRs on this target, any cause.
	OpenFixMRs int
	// MergedSinceGreen: fix MRs merged on this target since its last green
	// pipeline.
	MergedSinceGreen int
}

// ReasonCode names the one predicate that refused a dispatch: "<row>.<what>".
type ReasonCode string

// Reason codes, in evaluation order. The part before the dot is the plan's
// row. A code ending in ".unread" means the row's facts were not read.
const (
	E1Unread           ReasonCode = "E1.unread"
	E1FixesOff         ReasonCode = "E1.pipeline-fixes-off"
	E1PipelinesOff     ReasonCode = "E1.pipelines-off"
	E2Unread           ReasonCode = "E2.unread"
	E2TargetKind       ReasonCode = "E2.target-kind-not-enabled"
	E3NoteState        ReasonCode = "E3.run-not-diagnosed"
	E3RunClosed        ReasonCode = "E3.run-not-open"
	E3NoDiagnosis      ReasonCode = "E3.no-recorded-diagnosis"
	E4NeverEligible    ReasonCode = "E4.class-never-eligible"
	E4NotEnabled       ReasonCode = "E4.class-not-enabled"
	E5NoTargets        ReasonCode = "E5.no-target-jobs"
	E5Unjudgeable      ReasonCode = "E5.target-not-judgeable"
	E6Unread           ReasonCode = "E6.unread"
	E6NoCitedFiles     ReasonCode = "E6.no-cited-files"
	E6OutsideEnvelope  ReasonCode = "E6.outside-envelope"
	E7Unread           ReasonCode = "E7.unread"
	E7HeadMoved        ReasonCode = "E7.head-not-newest-failing"
	E8Unread           ReasonCode = "E8.unread"
	E8Drifted          ReasonCode = "E8.cause-drifted"
	E9Unread           ReasonCode = "E9.unread"
	E9FixTerminal      ReasonCode = "E9.fix-terminal"
	E9FixInFlight      ReasonCode = "E9.fix-in-flight"
	E9ClosedUnmerged   ReasonCode = "E9.closed-unmerged"
	E9OpenCauseMR      ReasonCode = "E9.cause-mr-open"
	E10Unread          ReasonCode = "E10.unread"
	E10OpenFixMR       ReasonCode = "E10.open-fix-mr-on-target"
	E11ChainExhausted  ReasonCode = "E11.chain-exhausted"
	E12CorrelationHold ReasonCode = "E12.correlation-hold"
)

// Reason is why a dispatch was refused. The zero value means "eligible".
type Reason struct {
	Code ReasonCode
	// Text is the sentence for the log and, when Note is set, for the note.
	Text string
	// Note: the plan marks this row N -- the reason is shown on the diagnosis
	// landing. False for the silent rows.
	Note bool
	// NeedsHuman: the row asks for the label gonk::needs-human (E6, E11).
	NeedsHuman bool
	// NeedsScope: E6 -- the fix state is `needs-scope`, decided at dispatch
	// for zero tokens.
	NeedsScope bool
	// Unknown: the row could not be evaluated because its facts were not
	// read. Nothing is decided and nothing is noted; look again.
	Unknown bool
}

// Row is the plan's row of the reason: "E1" .. "E12"; "" when eligible.
func (r Reason) Row() string {
	row, _, _ := strings.Cut(string(r.Code), ".")
	return row
}

// Eligible is plan 3.2's predicate: may a pipeline-fix be dispatched for this
// diagnosed run? All rows must hold, evaluated in the plan's order E1..E12;
// the FIRST one that fails is the reason, and a row whose facts were not read
// fails as Unknown at its place in that order (so "not opted in" is still
// answered silently when later facts are missing, and a missing fact never
// reads as eligibility).
//
// true comes with the zero Reason.
func Eligible(f EligibleFacts) (bool, Reason) {
	silent := func(code ReasonCode, text string) (bool, Reason) {
		return false, Reason{Code: code, Text: text}
	}
	noted := func(code ReasonCode, text string) (bool, Reason) {
		return false, Reason{Code: code, Text: text, Note: true}
	}
	unread := func(code ReasonCode, text string) (bool, Reason) {
		return false, Reason{Code: code, Text: text, Unknown: true}
	}

	// E1: opted in.
	if !f.Config.Loaded {
		return unread(E1Unread, "the effective configuration was not loaded")
	}
	if !f.Config.PipelineFixes {
		return silent(E1FixesOff, "actions.pipeline_fixes is not enabled")
	}
	if !f.Config.Pipelines {
		return silent(E1PipelinesOff, "actions.pipelines is not enabled")
	}

	// E2: the target kind.
	if !f.Run.Read {
		return unread(E2Unread, "the diagnosed run was not read")
	}
	if !contains(f.Config.Targets, f.Run.TargetKind) {
		return silent(E2TargetKind, fmt.Sprintf("target kind %q is not in pipeline_fix.targets", f.Run.TargetKind))
	}

	// E3: a diagnosed, open run.
	if f.Run.NoteState != pipelinectx.RunStateDiagnosed {
		return silent(E3NoteState, fmt.Sprintf("the run note is in state %q, not %q", f.Run.NoteState, pipelinectx.RunStateDiagnosed))
	}
	if !f.Run.Open {
		return silent(E3RunClosed, "the run is no longer open")
	}
	if f.Run.Class == "" {
		return silent(E3NoDiagnosis, "the record holds no landed diagnosis")
	}

	// E4: the class. The closed set first: no configuration reaches past it.
	classNote := fmt.Sprintf("no fix attempted: classified `%s`", f.Run.Class)
	if !contains(FixableClasses(), f.Run.Class) {
		return noted(E4NeverEligible, classNote)
	}
	if !contains(f.Config.Classes, f.Run.Class) {
		return noted(E4NotEnabled, classNote)
	}

	// E5: a job to judge.
	if len(f.Run.Targets) == 0 {
		return noted(E5NoTargets, "no fix attempted: the diagnosis cites no failed job, so there is no job to judge a fix on")
	}
	if len(f.Run.TargetProblems) > 0 {
		return noted(E5Unjudgeable, "no fix attempted: "+strings.Join(f.Run.TargetProblems, "; "))
	}

	// E6: every cited file inside the envelope. needs-scope, decided here.
	if !f.Scope.Folded {
		return unread(E6Unread, "the scope envelope was not folded")
	}
	for _, c := range f.Scope.Cited {
		if c.Class == "" {
			return unread(E6Unread, fmt.Sprintf("the cited file %q was not classified", c.Path))
		}
	}
	scope := func(code ReasonCode, text string) (bool, Reason) {
		return false, Reason{Code: code, Text: text, Note: true, NeedsHuman: true, NeedsScope: true}
	}
	if len(f.Scope.Cited) == 0 {
		return scope(E6NoCitedFiles, "needs scope: the diagnosis cites no file, so a fix has nothing it may change")
	}
	for _, c := range f.Scope.Cited {
		if !contains(f.Scope.Envelope, c.Class) {
			return scope(E6OutsideEnvelope, fmt.Sprintf(
				"needs scope: the cited file `%s` is of class `%s`, which a `%s` fix may not change here", c.Path, c.Class, f.Run.Class))
		}
	}

	// E7: the fix is written against a tree whose pipeline is known red.
	if !f.Head.Read || f.Head.TargetSHA == "" || f.Head.NewestFailingSHA == "" {
		return unread(E7Unread, "the target head or the newest failing pipeline's SHA was not read")
	}
	if f.Head.TargetSHA != f.Head.NewestFailingSHA {
		return silent(E7HeadMoved, "the target head is not the commit of the run's newest failing pipeline; waiting for the head's pipeline")
	}

	// E8: the cause has not drifted.
	if !f.Current.Read {
		return unread(E8Unread, "the newest failing pipeline's job failure keys were not computed")
	}
	for _, t := range f.Run.Targets {
		if !containsKey(f.Current.FailedKeys, t.Key) {
			return noted(E8Drifted, "the same jobs now fail differently; not fixing a stale diagnosis")
		}
	}

	// E9: dedup.
	if !f.Dedup.Read || (f.Dedup.FixState != "" && !KnownState(f.Dedup.FixState)) {
		return unread(E9Unread, "the fix record or the merge requests of this cause were not read")
	}
	if f.Dedup.FixState != "" && f.Dedup.FixState.Terminal() {
		return silent(E9FixTerminal, fmt.Sprintf("a fix for this run already ended in state %q", f.Dedup.FixState))
	}
	if f.Dedup.FixState != "" {
		return silent(E9FixInFlight, fmt.Sprintf("a fix for this run is in flight (state %q)", f.Dedup.FixState))
	}
	if f.Dedup.ClosedUnmergedCauseMRs != 0 {
		return noted(E9ClosedUnmerged, "no fix attempted: a fix merge request for this cause was closed without being merged")
	}
	if f.Dedup.OpenCauseMRs != 0 {
		return silent(E9OpenCauseMR, "a fix merge request for this cause is already open")
	}

	// E10: serial.
	if !f.Serial.Read {
		return unread(E10Unread, "the target's fix merge requests were not read")
	}
	if f.Serial.OpenFixMRs != 0 {
		return silent(E10OpenFixMR, "another fix merge request is open on this target")
	}

	// E11: chain.
	if f.Serial.MergedSinceGreen >= f.Config.MaxChain {
		return false, Reason{
			Code: E11ChainExhausted, Note: true, NeedsHuman: true,
			Text: fmt.Sprintf("no fix attempted: %d fix merge requests were merged on this target since its last green pipeline (max_chain %d)",
				f.Serial.MergedSinceGreen, f.Config.MaxChain),
		}
	}

	// E12: correlation hold.
	if f.CorrelationHold {
		return noted(E12CorrelationHold, "no fix attempted: this run is part of a failure whose common cause is outside this project")
	}
	return true, Reason{}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsKey(list []pipelinectx.JobFailureKey, k pipelinectx.JobFailureKey) bool {
	for _, x := range list {
		if x == k {
			return true
		}
	}
	return false
}
