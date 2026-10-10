package main

// THE FIX NOTE (gonk-2usw plan 8.1): ONE note per fix on the diagnosis
// landing -- the ref's tracking issue, or the merge request of an MR target --
// carrying pipelinectx.FixMarker(fix anchor, state) and edited in place.
// Controller-written, zero tokens.
//
// This file writes the states the DISPATCH decides: `not-eligible` (why no fix
// is attempted), `needs-scope` (decided at dispatch), `proposing` (a session
// was started) and `denied` (meter stopped it). Parts APPLY and VERIFY write
// the later states through the same function, which is why it refuses to move
// a note BACKWARDS: a late dispatch-time write never replaces a state a later
// part wrote (errFixNoteAdvanced).
//
// LIKE THE RUN NOTE, IT IS FOUND BY ITS MARKER among notes the bot itself
// authored, never by an id kept elsewhere, and it is never invented: a run
// with no diagnosis note has no landing, and then nothing is written.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

const fixNoteFooter = "This note is updated in place. gonk merges nothing: a fix it proposes is a merge request for a person to review."

// fixNote is one rendering of a fix's note.
type fixNote struct {
	State string // a pipelinectx.FixNoteStates value
	Text  string
	// Apply marks a note written by the APPLY, after a session's answer was
	// judged (verifying, awaiting-run, a session's needs-scope). It may
	// replace a dispatch-time note and an earlier apply-time one; see
	// fixNoteMayReplace.
	Apply bool
	// Verify marks a note written by the EXIT CHECK or the post-merge
	// confirmation (part VERIFY): a verdict state, `merged`, `confirmed`,
	// `recurred`. It moves a note FORWARD only; see fixNoteMayReplace.
	Verify bool
}

// errFixNoteNoLanding: the diagnosed run has no note on GitLab to write under.
var errFixNoteNoLanding = errors.New("the diagnosed run has no note on its landing; a fix note is never written without one")

// errFixNoteAdvanced: the fix's note already carries a state a later part
// wrote; a dispatch-time state does not replace it.
var errFixNoteAdvanced = errors.New("the fix note already carries a later state; not rewriting it")

// errFixNoteForges: the text to write holds something marker-shaped.
var errFixNoteForges = errors.New("the fix note's text holds a gonk marker; not writing it")

// fixNoteDispatchStates are the states the dispatch itself writes. A note in
// any OTHER state belongs to a later part and is not overwritten by these.
var fixNoteDispatchStates = map[string]bool{
	pipelinectx.FixNoteNotEligible:  true,
	string(pipefix.StateProposing):  true,
	string(pipefix.StateNeedsScope): true,
	string(pipefix.StateDenied):     true,
}

// fixNoteApplyReplaces are the states an APPLY-time note may be written over,
// beside the dispatch-time ones: an earlier attempt's open merge request
// note, and `not-fixed` (plan 8.1: "attempt n did not fix it; trying again"),
// which is the only verdict a further attempt follows.
var fixNoteApplyReplaces = map[string]bool{
	string(pipefix.StateAwaitingRun): true,
	string(pipefix.StateVerifying):   true,
	string(pipefix.StateNotFixed):    true,
}

// fixNoteRank orders the fix-note states by how far a fix has come (plan
// 7.1). A note moves to a HIGHER rank, or is rewritten within its own state,
// and never to a lower one:
//
//	0  not-eligible, proposing, needs-scope, denied   decided at dispatch
//	1  awaiting-run, verifying, not-fixed             a merge request is open
//	                                                  and an attempt is in
//	                                                  flight or follows
//	2  fixed, fixed-new-failure, flaky, unverifiable, the exit check's
//	   regressed, taken-over, closed-unmerged         result
//	3  merged
//	4  confirmed, recurred                            after the merge (7.4)
//
// `denied` and `needs-scope` are rank 0 as EXISTING states whoever wrote them
// (they are terminal for the record, so nothing later is written over them in
// practice); a state this build does not know has no rank.
func fixNoteRank(state string) (int, bool) {
	switch pipefix.State(state) {
	case pipefix.StateProposing, pipefix.StateNeedsScope, pipefix.StateDenied:
		return 0, true
	case pipefix.StateAwaitingRun, pipefix.StateVerifying, pipefix.StateNotFixed:
		return 1, true
	case pipefix.StateFixed, pipefix.StateFixedNewFailure, pipefix.StateFlaky, pipefix.StateUnverifiable,
		pipefix.StateRegressed, pipefix.StateTakenOver, pipefix.StateClosedUnmerged:
		return 2, true
	case pipefix.StateMerged:
		return 3, true
	case pipefix.StateConfirmed, pipefix.StateRecurred:
		return 4, true
	}
	if state == pipelinectx.FixNoteNotEligible {
		return 0, true
	}
	return 0, false
}

// fixNoteMayReplace is THE rule that keeps a fix note from moving backwards
// (plan 8.1). A note in state `existing` is rewritten as n only when:
//
//   - n is a dispatch-time note and so is the existing one; or
//   - n is the dispatch's `denied` and the existing one is `not-fixed`: the
//     attempt that was to follow was refused (meter, or max_attempts), which
//     is plan 7.1's own transition; or
//   - n is the apply's and the existing one is a dispatch-time note, an
//     earlier open-merge-request note, or `not-fixed`; or
//   - n is the exit check's (Verify) and it does not move the note down:
//     its state ranks HIGHER than the existing one (fixNoteRank), or it is
//     the same state said again, or both are rank 1 (verifying, not-fixed
//     and awaiting-run follow one another while attempts are made). `denied`
//     written by the exit check ends a `not-fixed` fix and replaces rank 0
//     and rank 1 only.
//
// Everything else stays as it is: a verdict is not replaced by another
// verdict, nothing replaces `confirmed` or `recurred`, and a note in a state
// this build does not know is never rewritten.
func fixNoteMayReplace(n fixNote, existing string) bool {
	if n.Verify {
		have, known := fixNoteRank(existing)
		want, ok := fixNoteRank(n.State)
		switch {
		case !known || !ok:
			return false
		case n.State == string(pipefix.StateDenied):
			return have <= 1 // note:verify-denied
		case n.State == existing:
			return true
		case want == 1 && have == 1:
			return true
		}
		return want > have // note:verify-forward
	}
	if fixNoteDispatchStates[existing] {
		return n.Apply || fixNoteDispatchStates[n.State]
	}
	if existing == string(pipefix.StateNotFixed) && n.State == string(pipefix.StateDenied) { // note:denied-after-not-fixed
		return true
	}
	return n.Apply && fixNoteApplyReplaces[existing] // note:backwards
}

func (n fixNote) render(fixAnchor string) (string, error) {
	marker, err := pipelinectx.FixMarker(fixAnchor, n.State)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(n.Text)
	// The text is the controller's, but it can name a cited path. Nothing
	// marker-shaped may ride in on one: the readers match markers anywhere in
	// a bot note's body.
	if strings.Contains(text, "<!--") || strings.Contains(text, "gonk:pipeline-") {
		return "", errFixNoteForges
	}
	return text + "\n\n" + marker, nil
}

// fixNoteIn returns the newest bot-authored note in notes carrying a fix
// marker for exactly fixAnchor, and the state that marker carries.
func (l *pipelineLander) fixNoteIn(notes []glab.Note, fixAnchor string) (*glab.Note, string) {
	var found *glab.Note
	state := ""
	for i := range notes {
		n := notes[i]
		if !l.botNote(n) {
			continue
		}
		if states := pipelinectx.FixMarkerStates(n.Body, fixAnchor); len(states) > 0 {
			found, state = &n, states[len(states)-1]
		}
	}
	return found, state
}

// writeFixNote puts n on the diagnosed run's landing: edits the fix's note in
// place, or creates it. changed reports whether GitLab was written -- a note
// that already says exactly this is success with changed=false, which is what
// lets a look that repeats every five minutes write once.
func (l *pipelineLander) writeFixNote(ctx context.Context, run beadstore.Record, fixAnchor string, n fixNote) (changed bool, ld runLanding, err error) {
	t, err := pipelineRunOf(run)
	if err != nil {
		return false, ld, err
	}
	body, err := n.render(fixAnchor)
	if err != nil {
		return false, ld, err
	}
	ld, err = l.findRunLanding(ctx, run, t)
	if err != nil {
		return false, ld, err
	}
	if ld.note == nil || (t.Kind == pipelinectx.TargetKindRef && ld.issue == nil) {
		return false, ld, errFixNoteNoLanding
	}
	pid := run.ProjectID
	var notes []glab.Note
	if t.Kind == pipelinectx.TargetKindMR {
		notes, err = l.f.ListMRNotes(ctx, pid, t.MRIID)
	} else {
		notes, err = l.f.ListIssueNotes(ctx, pid, ld.issue.IID)
	}
	if err != nil {
		return false, ld, fmt.Errorf("read the landing's notes: %w", err)
	}
	existing, state := l.fixNoteIn(notes, fixAnchor)
	switch {
	case existing != nil && existing.Body == body:
		return false, ld, nil
	case existing != nil && !fixNoteMayReplace(n, state):
		return false, ld, errFixNoteAdvanced
	}
	switch {
	case t.Kind == pipelinectx.TargetKindMR && existing != nil:
		_, err = l.f.UpdateMRNote(ctx, pid, t.MRIID, existing.ID, body)
	case t.Kind == pipelinectx.TargetKindMR:
		_, err = l.f.CreateMRNote(ctx, pid, t.MRIID, body)
	case existing != nil:
		_, err = l.f.UpdateIssueNote(ctx, pid, ld.issue.IID, existing.ID, body)
	default:
		_, err = l.f.CreateIssueNote(ctx, pid, ld.issue.IID, body)
	}
	if err != nil {
		return false, ld, fmt.Errorf("write the fix note: %w", err)
	}
	return true, ld, nil
}

// addLandingLabel adds one label to the landing. BEST EFFORT, like every
// label edit after a note (D8): a failure is logged and never returned.
func (l *pipelineLander) addLandingLabel(ctx context.Context, run beadstore.Record, ld runLanding, label string) {
	var err error
	switch {
	case ld.target.Kind == pipelinectx.TargetKindMR:
		_, err = l.f.UpdateMRLabels(ctx, run.ProjectID, ld.target.MRIID, []string{label}, nil)
	case ld.issue != nil:
		_, err = l.f.UpdateIssue(ctx, run.ProjectID, ld.issue.IID, glab.IssueUpdate{AddLabels: []string{label}})
	}
	if err != nil {
		l.log.Warn("pipeline fix: landing label update failed (the fix note is already written)",
			"bead", run.BeadAnchor, "label", label, "err", err)
	}
}

// proposingText is plan 8.1's `proposing` row: gonk is preparing a fix merge
// request -- which attempt, on which model -- linked to the pipeline the fix
// is written against, as every run note links its pipelines.
func proposingText(fix beadstore.Record, attempt int, rung string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**gonk is preparing a fix merge request** for the failure diagnosed above (attempt %d, using %s).\n\n",
		max(attempt, 1), rungText(rung))
	fmt.Fprintf(&b, "- Written against: %s at commit %s\n", pipelineRef(fix, fix.PipelineFix.BasePipelineID),
		codeSpan(shortSHA(fix.PipelineFix.BaseSHA)))
	fmt.Fprintf(&b, "- It may change at most %d of the %d cited file(s), and at most %d lines.\n",
		fix.PipelineFix.MaxFiles, len(fix.PipelineFix.Files), fix.PipelineFix.MaxLines)
	b.WriteString("\nIf a fix needs more than that, gonk will say so here instead of opening a merge request.\n\n" + fixNoteFooter)
	return b.String()
}

// fixDeniedText is plan 8.1's `denied` row, at dispatch: meter stopped the fix
// before a session ran.
func fixDeniedText(fix beadstore.Record, reason, detail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**No fix merge request.** gonk has stopped and will not propose a fix for the failure diagnosed in %s. %s",
		pipelineRef(fix, fix.PipelineFix.BasePipelineID), denyReasonText(reason))
	if d := strings.TrimSpace(detail); d != "" {
		b.WriteString("\n\nDetails: " + d)
	}
	b.WriteString("\n\nA human is needed: gonk will not retry this on its own.\n\n" + fixNoteFooter)
	return b.String()
}

// fixCappedText is the `denied` note for a fix that ran out of the project's
// own attempts (pipeline_fix.max_attempts), not meter's.
func fixCappedText(fix beadstore.Record, detail string) string {
	return fmt.Sprintf("**No further fix attempt.** gonk has stopped proposing a fix for the failure diagnosed in %s: %s."+
		"\n\nA human is needed: gonk will not retry this on its own.\n\n%s",
		pipelineRef(fix, fix.PipelineFix.BasePipelineID), detail, fixNoteFooter)
}

// fixNothingEditableText is the needs-scope note of a fix whose cited files
// cannot be edited at the base commit (decided by gonk-dispatch, zero tokens).
func fixNothingEditableText(fix beadstore.Record) string {
	return "**No fix merge request: more scope is needed.** gonk will not open one for the failure diagnosed in " +
		pipelineRef(fix, fix.PipelineFix.BasePipelineID) +
		": none of the files the diagnosis cites can be changed by a fix here (a file is missing at that commit, " +
		"empty, not text, generated, or larger than a fix may edit)." +
		"\n\nA person can make the change. gonk will not retry this on its own.\n\n" + fixNoteFooter
}
