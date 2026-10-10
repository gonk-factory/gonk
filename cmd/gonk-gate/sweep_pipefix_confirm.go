package main

// AFTER THE FIX MERGE REQUEST MERGES (gonk-2usw plan 7.4; part VERIFY).
//
// The exit check ends with the merge request still open (`fixed`, or one of
// the inconclusive states): whether it merges is a person's decision, and it
// can take days. Nothing lists a `done` fix record on a sweep tick, so the
// watch hangs off what IS looked at for as long as it matters: the diagnosed
// RUN, which run maintenance visits at most every five minutes while it is
// open (sweepPipelineRuns). Before a run is maintained, confirmMergedFix:
//
//  1. finds the run's fix record (the fix anchor is derived from the run's);
//     a fix with no merge request, or one the exit check still owns, or one
//     that has ended (confirmed, recurred, closed-unmerged, needs-scope), is
//     not looked at: no GitLab request;
//  2. reads the merge request. Closed -> `closed-unmerged`. Merged ->
//     `merged`, with merged_at and the commits by which the fix can be on the
//     target recorded;
//  3. for a merged fix, finds the FIRST pipeline of the target that comes
//     after the merge (it ran on a landed commit, or was created at or after
//     merged_at -- the fix epoch's own test, pipelinectx.FixEpoch) and is
//     terminal for health, and asks pipefix.Confirm:
//
//	confirmed, target green     fix `confirmed`; the run is left to run
//	                            maintenance, which resolves it in this same
//	                            look (note `resolved`, tracking issue closed)
//	confirmed, target red       fix `confirmed`; THE RUN IS CLOSED HERE (note
//	                            `superseded`, fixEndedStatus). This is the gap
//	                            part EPOCH left: the fix epoch makes the next
//	                            failure a NEW run even with the same failing
//	                            jobs, so without this the old run stayed open,
//	                            its note stayed `diagnosed`, and run
//	                            maintenance kept marking it "still failing"
//	                            with the new run's pipelines.
//	recurred                    fix `recurred`, gonk::needs-human; the run
//	                            stays open (plan 7.4 row 3); no revert exists
//	not-observed, target red    the fix stays `merged`; that pipeline is
//	                            remembered and the next one after it decides
//	not-observed, target green  the fix stays `merged` and the note says it
//	                            is NOT confirmed; run maintenance resolves
//	                            the run in this look, which ends the watch
//	cannot-confirm              nothing; the same pipeline is read again
//
// WHILE A MERGED FIX IS UNCONFIRMED THE RUN IS HELD: confirmMergedFix reports
// hold=true and run maintenance does not look at the run in that pass. Every
// failing pipeline after the merge belongs to the NEW run the fix epoch
// starts (pkg/pipelinectx FixEpoch), so maintenance must not book one on the
// old run as "still failing", nor let one with other failing jobs supersede
// it before the fix was confirmed. The hold ends with the confirmation, a
// recurrence, or a green target pipeline.
//
// WHAT IS NOT FOLLOWED: a fix whose run closed before its merge request
// merged (the target went green, or failed differently, while the merge
// request sat open). Nothing visits that run again, so the fix keeps the
// state its verdict gave it. The fix epoch does not depend on this pass:
// intake reads merged fix merge requests from GitLab itself.
//
// ZERO TOKENS, and no meter call: the attempt's outcome was reported by the
// exit check. NOTHING HERE MERGES; the forge is pipefixVerifyForge.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// fixWatchedForMerge are the fix states in which the merge request may still
// merge and nothing else is watching it. `merged` itself is watched for its
// confirmation.
var fixWatchedForMerge = map[pipefix.State]bool{
	pipefix.StateFixed: true, pipefix.StateFixedNewFailure: true, pipefix.StateFlaky: true,
	pipefix.StateUnverifiable: true, pipefix.StateRegressed: true, pipefix.StateDenied: true,
	pipefix.StateTakenOver: true, pipefix.StateMerged: true,
}

// activePipelineStatuses: a pipeline in one of these has not finished and
// will, or may, still run by itself.
var activePipelineStatuses = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"}

// confirmMergedFix is the look described above. It returns the RUN record as
// it left it -- closed when a confirmed fix ended the run on a red target,
// and with its new revision whenever it wrote the run -- and hold: the run
// has a merged fix that is not confirmed yet, and run maintenance must leave
// the run alone in this pass.
func confirmMergedFix(ctx context.Context, d sweepDeps, l *pipelineLander, run beadstore.Record) (_ beadstore.Record, hold bool) {
	if run.Trigger != atags.TriggerPipelineDiagnosis || !run.PipelineRunOpen || run.State != beadstore.StateDone {
		return run, false
	}
	fixAnchor, err := pipelinectx.FixAnchor(run.BeadAnchor)
	if err != nil {
		return run, false
	}
	fix, found, err := d.Store.Get(ctx, fixAnchor)
	if err != nil || !found {
		return run, false
	}
	pf := fix.PipelineFix
	st := pipefix.State(pf.State)
	// (A fix the exit check still owns -- verifying, awaiting-run, or being
	// re-slung -- is in none of the states looked at below.)
	if pf.RunAnchor != run.BeadAnchor || pf.MRIID < 1 {
		return run, false
	}
	forge, ok := d.Pipelines.(pipefixVerifyForge)
	if !ok {
		return run, false
	}
	c := &fixConfirm{d: d, f: forge, l: l, run: run, fix: fix, readRevision: fix.Revision}
	if st == pipefix.StateConfirmed {
		// The fix is confirmed and its run is STILL OPEN: an earlier look
		// confirmed it and then could not close the run (or run maintenance
		// has not resolved it yet). Finish that.
		c.closeIfEnded(ctx)
		return c.run, false
	}
	if !fixWatchedForMerge[st] { // confirm:watched
		return run, false
	}
	if st != pipefix.StateMerged || pf.MergedAt.IsZero() {
		if !c.readMerge(ctx) {
			// Not merged (or not readable): nothing to hold -- unless the
			// record already says merged and only the merge facts are missing.
			return c.run, st == pipefix.StateMerged
		}
	}
	green := c.confirm(ctx)
	// Still merged and unconfirmed, and the target is not known green: held.
	return c.run, c.run.PipelineRunOpen && c.fix.PipelineFix.State == string(pipefix.StateMerged) && !green // confirm:hold
}

// holdRun stamps a held run as looked at, so the next look is one
// maintenance interval away like any other run's.
func holdRun(ctx context.Context, d sweepDeps, run beadstore.Record, now time.Time) {
	readRevision := run.Revision
	run.PipelineCheckedAt = now
	if _, err := d.Store.PutIf(ctx, run, readRevision); err != nil && !errors.Is(err, beadstore.ErrStale) {
		d.Log.Warn("sweep: a held pipeline run's record could not be stamped; it is looked at again on the next tick",
			"bead", run.BeadAnchor, "err", err)
	}
}

// fixConfirm is one look at one fix after its verdict.
type fixConfirm struct {
	d            sweepDeps
	f            pipefixVerifyForge
	l            *pipelineLander
	run          beadstore.Record
	fix          beadstore.Record
	readRevision string
}

// putFix writes the fix record over the revision this look holds, and keeps
// the new revision for a second write in the same look.
func (c *fixConfirm) putFix(ctx context.Context, why string) bool {
	rev, err := c.d.Store.PutIf(ctx, c.fix, c.readRevision)
	if errors.Is(err, beadstore.ErrStale) {
		c.d.Log.Info("sweep: pipeline fix: the record changed during the post-merge look; the next look repeats it",
			"bead", c.fix.BeadAnchor, "why", why)
		return false
	}
	if err != nil {
		c.d.Log.Warn("sweep: pipeline fix: the record could not be written; the next look repeats it",
			"bead", c.fix.BeadAnchor, "why", why, "err", err)
		return false
	}
	c.readRevision, c.fix.Revision = rev, rev
	return true
}

// note writes a post-verdict state on the landing: the fix note (forward
// only), gonk::needs-human where the state asks for a person, and the run
// note's fix line. It reports whether the fix note now says it.
func (c *fixConfirm) note(ctx context.Context, state pipefix.State, text string) bool {
	changed, ld, err := c.l.writeFixNote(ctx, c.run, c.fix.BeadAnchor, fixNote{State: string(state), Text: text, Verify: true}) // confirm:note
	if err != nil && !errors.Is(err, errFixNoteAdvanced) {
		c.d.Log.Warn("sweep: pipeline fix: the fix note could not be written", "bead", c.fix.BeadAnchor, "state", string(state), "err", err)
		return false
	}
	if changed && fixStateNeedsHuman(state) { // confirm:needs-human
		c.l.addLandingLabel(ctx, c.run, ld, labelNeedsHuman)
	}
	run, lerr := writeRunFixLine(ctx, c.d, c.l, c.run, c.fix.PipelineFix.MRIID, state)
	if lerr != nil {
		c.d.Log.Warn("sweep: pipeline fix: the run note's fix line could not be written", "bead", c.fix.BeadAnchor, "err", lerr)
	}
	c.run = run
	return true
}

// readMerge reads the merge request of a fix that is not yet known to be
// merged. It reports whether the fix is now `merged` with its merge facts
// recorded (so the confirmation can go on in this look).
func (c *fixConfirm) readMerge(ctx context.Context) bool {
	pf := &c.fix.PipelineFix
	branch, err := pipelinectx.FixBranch(c.fix.BeadAnchor)
	if err != nil {
		return false
	}
	mr, err := c.f.GetMergeRequest(ctx, c.fix.ProjectID, pf.MRIID)
	if err != nil || mr == nil || mr.IID != pf.MRIID { // a named field, not "it parsed"
		c.d.Log.Warn("sweep: pipeline fix: the merge request could not be read; looking again later",
			"bead", c.fix.BeadAnchor, "mr", pf.MRIID, "err", err)
		return false
	}
	if mr.Author.ID != c.l.me.ID || mr.SourceBranch != branch || mr.SourceProjectID != c.fix.ProjectID || mr.TargetProjectID != c.fix.ProjectID { // confirm:ours
		c.d.Log.Warn("sweep: pipeline fix: merge request is not this fix's; not watching it", "bead", c.fix.BeadAnchor, "mr", pf.MRIID)
		return false
	}
	switch mr.State {
	case pipefix.MRStateMerged:
		if mr.MergedAt == nil || mr.MergedAt.IsZero() { // confirm:merged-at
			c.d.Log.Warn("sweep: pipeline fix: the merged merge request carries no merged_at; looking again later",
				"bead", c.fix.BeadAnchor, "mr", pf.MRIID)
			return false
		}
		pf.MergedAt = *mr.MergedAt
		pf.MergedSHAs = nil
		for _, sha := range []string{mr.MergeCommitSHA, mr.SquashCommitSHA, mr.SHA} {
			if sha != "" && !slices.Contains(pf.MergedSHAs, sha) {
				pf.MergedSHAs = append(pf.MergedSHAs, sha)
			}
		}
		if pf.State != string(pipefix.StateMerged) {
			pf.State = string(pipefix.StateMerged)
			pf.ConfirmPipelineID = 0
			c.note(ctx, pipefix.StateMerged, fixMergedText(c.fix, ""))
			c.d.Log.Info("sweep: pipeline fix merge request merged; watching the target", "bead", c.fix.BeadAnchor,
				"mr", pf.MRIID, "merged_at", pf.MergedAt, "target", c.run.PipelineTarget)
		}
		return c.putFix(ctx, "merged")
	case pipefix.MRStateClosed:
		if pf.State == string(pipefix.StateMerged) {
			return false // merged once is merged; a closed reply is not believed over it
		}
		pf.State = string(pipefix.StateClosedUnmerged)
		if c.note(ctx, pipefix.StateClosedUnmerged, closedAfterVerdictText(c.fix)) {
			c.putFix(ctx, "closed-unmerged")
			c.d.Log.Info("sweep: pipeline fix merge request closed without a merge", "bead", c.fix.BeadAnchor, "mr", pf.MRIID)
		}
	}
	return false
}

func closedAfterVerdictText(fix beadstore.Record) string {
	return fmt.Sprintf("**!%d was closed without being merged.** gonk will not propose this fix again.\n\n%s", fix.PipelineFix.MRIID, fixNoteFooter)
}

// decidingPipeline is the first pipeline of the target that comes after the
// merge and is terminal for health. waiting is true when an EARLIER
// post-merge pipeline has not finished: it must be allowed to decide first.
func (c *fixConfirm) decidingPipeline(ctx context.Context) (p *glab.Pipeline, waiting bool, err error) {
	pf := c.fix.PipelineFix
	t, err := pipelineRunOf(c.run)
	if err != nil {
		return nil, false, err
	}
	var window []glab.Pipeline
	if t.Kind == pipelinectx.TargetKindMR {
		window, err = c.f.ListMRPipelines(ctx, c.run.ProjectID, t.MRIID)
	} else {
		window, err = c.f.ListPipelines(ctx, c.run.ProjectID, t.Ref, "", pipelineRunWindowRef)
	}
	if err != nil {
		return nil, false, fmt.Errorf("list the target's pipelines: %w", err)
	}
	var after []glab.Pipeline
	for _, w := range window {
		if w.ID < 1 || w.Source == pipefix.SourceParentPipeline || w.ID <= pf.ConfirmPipelineID { // confirm:skip-seen
			continue
		}
		landed := w.SHA != "" && slices.Contains(pf.MergedSHAs, w.SHA)
		if !landed && (w.CreatedAt.IsZero() || w.CreatedAt.Before(pf.MergedAt)) { // confirm:after-merge
			continue
		}
		after = append(after, w)
	}
	sort.Slice(after, func(i, j int) bool { return after[i].ID < after[j].ID })
	for i := range after {
		switch {
		case pipelinectx.TerminalForHealth(after[i].Status):
			return &after[i], false, nil
		case slices.Contains(activePipelineStatuses, after[i].Status): // confirm:first
			return nil, true, nil
		}
		// canceled, skipped, manual: says nothing about health; the next one.
	}
	return nil, false, nil
}

// confirm is step 3 for a fix that is `merged`. It reports whether the
// pipeline it decided on is GREEN (the run is then run maintenance's to
// resolve, whatever was or was not confirmed).
func (c *fixConfirm) confirm(ctx context.Context) (green bool) {
	pf := &c.fix.PipelineFix
	p, waiting, err := c.decidingPipeline(ctx)
	if err != nil {
		c.d.Log.Warn("sweep: pipeline fix: the target's pipelines could not be read; looking again later", "bead", c.fix.BeadAnchor, "err", err)
		return false
	}
	if p == nil {
		c.d.Log.Info("sweep: pipeline fix merged; no finished target pipeline after the merge yet", "bead", c.fix.BeadAnchor,
			"mr", pf.MRIID, "earlier_pipeline_still_running", waiting)
		return false
	}
	facts := fixFactsOf(c.fix)
	inT := map[string]bool{}
	for _, t := range facts.Targets {
		inT[t.Name()] = true
	}
	attempts, read, _ := readPipefixAttempts(ctx, c.f, c.fix.ProjectID, p.ID, inT)
	res := pipefix.Confirm(pipefix.ConfirmFacts{
		Targets: facts.Targets, ExcerptVersion: facts.ExcerptVersion, SignatureVersion: facts.SignatureVersion,
		Pipeline: pipefix.Pipeline{ID: p.ID, SHA: p.SHA, Source: p.Source, Status: p.Status,
			JobsRead: read, IncludeRetried: read, Jobs: attempts},
	})
	c.d.Log.Info("sweep: pipeline fix post-merge confirmation", "bead", c.fix.BeadAnchor, "mr", pf.MRIID, "pipeline", p.ID,
		"pipeline_status", p.Status, "confirmation", string(res.Confirmation), "detail", res.Detail)

	green = p.Status == pipefix.StatusSuccess
	switch res.Confirmation {
	case pipefix.ConfirmConfirmed:
		pf.State, pf.ConfirmPipelineID = string(pipefix.StateConfirmed), p.ID
		if !c.note(ctx, pipefix.StateConfirmed, fixConfirmedText(c.fix, p.ID, green, res)) {
			pf.State = string(pipefix.StateMerged) // not written: still merged, still held
			return false
		}
		if !c.putFix(ctx, "confirmed") {
			pf.State = string(pipefix.StateMerged)
			return false
		}
		if !green { // confirm:close-run
			c.closeEndedRun(ctx, p.ID)
		}
	case pipefix.ConfirmRecurred:
		pf.State, pf.ConfirmPipelineID = string(pipefix.StateRecurred), p.ID
		if !c.note(ctx, pipefix.StateRecurred, fixRecurredText(c.fix, p.ID, res)) || !c.putFix(ctx, "recurred") {
			pf.State = string(pipefix.StateMerged)
			return false
		}
	case pipefix.ConfirmNotObserved:
		// This pipeline decides nothing. Remember it so the next one after
		// it is looked at, and say why on the note. On a GREEN target the
		// run resolves in this look and nothing looks again: say that.
		pf.ConfirmPipelineID = p.ID
		waitingOn := fmt.Sprintf("%s finished without the diagnosed job running in it, so it confirms nothing; gonk is waiting for the next one.",
			pipelineRef(c.fix, p.ID))
		if green {
			waitingOn = fmt.Sprintf("%s passed, but the diagnosed job did not run in it, so the fix is NOT confirmed. "+
				"The target is green and gonk has stopped watching.", pipelineRef(c.fix, p.ID))
		}
		c.note(ctx, pipefix.StateMerged, fixMergedText(c.fix, waitingOn))
		c.putFix(ctx, "not-observed")
	case pipefix.ConfirmCannot:
		return false // read the same pipeline again; green or not, nothing is decided
	}
	return green
}

// closeIfEnded closes the run of a fix that is already `confirmed`, when the
// pipeline that confirmed it was red. A green one is run maintenance's to
// resolve.
func (c *fixConfirm) closeIfEnded(ctx context.Context) {
	id := c.fix.PipelineFix.ConfirmPipelineID
	if id < 1 {
		return
	}
	p, err := c.f.GetPipeline(ctx, c.fix.ProjectID, id)
	if err != nil || p == nil || p.ID != id {
		c.d.Log.Warn("sweep: pipeline fix: the confirming pipeline could not be read; the run stays open for now",
			"bead", c.fix.BeadAnchor, "pipeline", id, "err", err)
		return
	}
	if p.Status == pipefix.StatusFailed {
		c.closeEndedRun(ctx, id)
	}
}

// closeEndedRun closes the run a confirmed fix ended while the target is
// still red: the run note moves to `superseded` with fixEndedStatus, the
// landing's class label goes as it does for any superseded run, and the
// record's PipelineRunOpen is cleared. The tracking issue stays open: the
// failure that remains is a new run and lands on it.
func (c *fixConfirm) closeEndedRun(ctx context.Context, pipelineID int64) {
	run := c.run
	readRevision := run.Revision
	if !editRunNote(ctx, c.d, c.l, run, pipelinectx.RunStateSuperseded, fixEndedStatus(run, c.fix.PipelineFix.MRIID, pipelineID)) {
		return // the next look closes it (closeIfEnded): the fix is already `confirmed`
	}
	run.PipelineRunOpen = false
	rev, err := c.d.Store.PutIf(ctx, run, readRevision)
	if err != nil {
		c.d.Log.Warn("sweep: pipeline run ended by its fix could not be closed on the record; the next look repeats it",
			"bead", run.BeadAnchor, "err", err)
		return
	}
	run.Revision = rev
	c.run = run
	c.d.Log.Info("sweep: pipeline run closed: its fix merged and the diagnosed failure is gone", "bead", run.BeadAnchor,
		"fix", c.fix.BeadAnchor, "mr", c.fix.PipelineFix.MRIID, "pipeline", pipelineID)
}
