package main

// RUN MAINTENANCE (gonk-tios D9): what happens to a pipeline run after its
// diagnosis. Zero tokens -- GitLab reads, the run note edited in place, the
// record updated. Called once per sweep tick; each open run is looked at no
// more than once per pipelineRunCheckEvery.
//
// For each record of the pipeline-diagnosis trigger whose run is still open
// (PipelineRunOpen) and that is done, needs-human or parked -- a run being
// diagnosed right now (running, pending-prompt) is left to its session -- the
// target's newest terminal-for-health pipeline decides:
//
//	green, newer than the run's last success   -> RESOLVED: the note says which
//	   (only the NEWEST one: an older green        pipeline passed, at which commit
//	   finishing late resolves nothing)            and when; the MR loses its class
//	                                               label / the tracking issue is
//	                                               closed; the run is closed.
//	failed, same fingerprint, newer pipeline    -> STILL FAILING: the note's
//	                                               "latest failing pipeline" line.
//	failed, a different fingerprint             -> SUPERSEDED at that pipeline; the
//	                                               run is closed (the new failure
//	                                               is a new run, which intake
//	                                               dispatches).
//	MR target no longer open                    -> the run is closed, nothing written.
//
// The note is edited only when it exists; a missing note is logged, never
// invented. GitLab is written before the record, so a lost record write is
// repaired by the next look (every edit is idempotent).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// pipelineRunCheckEvery bounds how often one open run is looked at.
const pipelineRunCheckEvery = 5 * time.Minute

// pipelineRunWindowRef is how many of a ref's newest pipelines are read to
// find the newest terminal-for-health one (D4's backfill uses the same 20).
const pipelineRunWindowRef = 20

// pipelineMaintainedStates are the record states whose open runs are kept.
var pipelineMaintainedStates = map[beadstore.State]bool{
	beadstore.StateDone: true, beadstore.StateNeedsHuman: true, beadstore.StateParked: true,
}

// sweepPipelineRuns maintains every open pipeline run. Best effort: one run's
// failure is logged and never stops the others or the sweep.
//
// It reads ONLY open runs (Store.ListOpenPipelineRuns; verify report R6): a
// closed run is never handed back, so the cost of a tick is the open runs,
// not every record that ever finished.
func sweepPipelineRuns(ctx context.Context, d sweepDeps) {
	if d.Pipelines == nil {
		return
	}
	now := d.Now()
	recs, err := d.Store.ListOpenPipelineRuns(ctx)
	if err != nil {
		d.Log.Warn("sweep: could not list open pipeline runs for maintenance", "err", err)
		return
	}
	// The pipeline-fix dispatch pass (gonk-2usw); nil when it is off.
	fixes := newPipefixSweep(d)
	var lander *pipelineLander
	for _, rec := range recs {
		if rec.Trigger != atags.TriggerPipelineDiagnosis || !rec.PipelineRunOpen || !pipelineMaintainedStates[rec.State] {
			continue
		}
		if !rec.PipelineCheckedAt.IsZero() && now.Sub(rec.PipelineCheckedAt) < pipelineRunCheckEvery {
			continue
		}
		if lander == nil {
			l, lerr := newPipelineLander(ctx, d.Pipelines, d.Log)
			if lerr != nil {
				d.Log.Warn("sweep: pipeline run maintenance skipped this tick", "err", lerr)
				return
			}
			lander = l
		}
		// FIRST, a fix this run already has: its merge request may have
		// merged, and then the target's next pipeline confirms it -- and a
		// confirmed fix ENDS this run even when the target is still red
		// (gonk-2usw plan 7.4, sweep_pipefix_confirm.go). It runs before
		// maintenance so that pipeline is not booked as "still failing" on
		// a run that is over. Only when the fix pass is on.
		if fixes != nil {
			var hold bool
			if rec, hold = confirmMergedFix(ctx, d, lander, rec); !rec.PipelineRunOpen {
				continue
			}
			if hold {
				// A merged fix not confirmed yet: every failing pipeline
				// after the merge is the NEW run's (the fix epoch), so
				// maintenance must not book one on this run.
				holdRun(ctx, d, rec, now)
				continue
			}
		}
		// A run that is still open and diagnosed after maintenance may get a
		// fix (plan 3.2: "again on each run-maintenance look").
		considerPipelineFix(ctx, d, fixes, lander, maintainPipelineRun(ctx, d, lander, rec, now), now)
	}
}

// pipelineObservation is what the target's pipelines say about a run now.
type pipelineObservation struct {
	closedMR bool           // the MR target is merged or closed
	green    *glab.Pipeline // the newest terminal pipeline, green and after the run's last success: resolved
	failed   *glab.Pipeline // the newest terminal pipeline, failed
}

// observePipelineTarget reads the target's newest pipelines.
func observePipelineTarget(ctx context.Context, f pipelineForge, rec beadstore.Record, t pipelinectx.Target) (pipelineObservation, error) {
	var obs pipelineObservation
	var window []glab.Pipeline
	var err error
	if t.Kind == pipelinectx.TargetKindMR {
		mr, merr := f.GetMergeRequest(ctx, rec.ProjectID, t.MRIID)
		if merr != nil {
			return obs, fmt.Errorf("read merge request !%d: %w", t.MRIID, merr)
		}
		if mr.State != "opened" {
			obs.closedMR = true
			return obs, nil
		}
		window, err = f.ListMRPipelines(ctx, rec.ProjectID, t.MRIID)
	} else {
		window, err = f.ListPipelines(ctx, rec.ProjectID, t.Ref, "", pipelineRunWindowRef)
	}
	if err != nil {
		return obs, fmt.Errorf("list the target's pipelines: %w", err)
	}
	// D9: the NEWEST terminal-for-health pipeline on the target decides (by id,
	// i.e. creation order, never by finishing order or list order). An older
	// pipeline that finishes green after a newer one failed says nothing about
	// the target's health now (verify report R5). Child pipelines are not the
	// target's (D2), and are passed over.
	var newest *glab.Pipeline
	for i := range window {
		p := window[i]
		if !pipelinectx.TerminalForHealth(p.Status) || p.Source == "parent_pipeline" {
			continue
		}
		if newest == nil || p.ID > newest.ID {
			newest = &window[i]
		}
	}
	if newest == nil {
		return obs, nil
	}
	switch newest.Status {
	case "success":
		if newest.ID > rec.PipelineLastSuccess {
			obs.green = newest
		}
	case "failed":
		obs.failed = newest
	}
	return obs, nil
}

// pipelineFingerprintOf computes D2's fingerprint of one pipeline, the way
// intake computes it (pkg/intake: latest attempts, allow_failure excluded,
// the pipeline's own failure reason when no job failed).
func pipelineFingerprintOf(ctx context.Context, f pipelineForge, pid int64, p glab.Pipeline) (string, error) {
	jobs, err := f.ListPipelineJobs(ctx, pid, p.ID, "", false)
	if err != nil {
		return "", err
	}
	var failed []pipelinectx.FailedJob
	for _, j := range jobs {
		if j.Status == "failed" && !j.AllowFailure {
			failed = append(failed, pipelinectx.FailedJob{Stage: j.Stage, Name: j.Name, FailureReason: j.FailureReason})
		}
	}
	reason := ""
	if len(failed) == 0 {
		full, gerr := f.GetPipeline(ctx, pid, p.ID)
		if gerr != nil {
			return "", gerr
		}
		reason = full.FailureReason
	}
	return pipelinectx.Fingerprint(failed, reason), nil
}

// maintainPipelineRun looks at one open run and acts on what it sees. It
// returns the record as it left it (PipelineRunOpen false once the run is
// closed), whether or not its write landed: the caller only reads it.
func maintainPipelineRun(ctx context.Context, d sweepDeps, l *pipelineLander, rec beadstore.Record, now time.Time) beadstore.Record {
	readRevision := rec.Revision
	rec.PipelineCheckedAt = now
	put := func(why string) {
		if _, err := d.Store.PutIf(ctx, rec, readRevision); errors.Is(err, beadstore.ErrStale) {
			d.Log.Info("sweep: pipeline run record changed while it was maintained; the next look repeats it",
				"bead", rec.BeadAnchor, "why", why)
		} else if err != nil {
			d.Log.Warn("sweep: pipeline run record write failed; the next look repeats it",
				"bead", rec.BeadAnchor, "why", why, "err", err)
		}
	}
	t, err := pipelineRunOf(rec)
	if err != nil {
		d.Log.Warn("sweep: pipeline run record is malformed; closing it without writing anything",
			"bead", rec.BeadAnchor, "err", err)
		rec.PipelineRunOpen = false
		put("malformed")
		return rec
	}
	obs, err := observePipelineTarget(ctx, d.Pipelines, rec, t)
	if err != nil {
		d.Log.Warn("sweep: could not read the pipeline run's target; looking again later", "bead", rec.BeadAnchor, "err", err)
		put("unreadable")
		return rec
	}

	// A record with no project URL (written before the field existed, or
	// parked/denied before any pipeline was read) learns it here, from the
	// pipelines just observed: the notes written below link, and the record
	// keeps the URL (gonk-385g).
	if rec.PipelineProjectURL == "" {
		for _, p := range []*glab.Pipeline{obs.green, obs.failed} {
			if p != nil {
				if u := projectWebURLOf(*p); u != "" {
					rec.PipelineProjectURL = u
					break
				}
			}
		}
	}

	switch {
	case obs.closedMR:
		rec.PipelineRunOpen = false
		d.Log.Info("sweep: pipeline run closed: its merge request is no longer open", "bead", rec.BeadAnchor)
		put("mr_closed")

	case obs.green != nil:
		if !editRunNote(ctx, d, l, rec, pipelinectx.RunStateResolved, resolvedStatus(rec, *obs.green)) {
			put("note_unwritten")
			return rec
		}
		rec.PipelineRunOpen = false
		if rec.State == beadstore.StateParked {
			rec.State = beadstore.StateDone // never unpark a run that has resolved
		}
		d.Log.Info("sweep: pipeline run resolved", "bead", rec.BeadAnchor, "green_pipeline", obs.green.ID)
		put("resolved")

	case obs.failed != nil && obs.failed.ID > max(rec.PipelineLatestFailed, rec.PipelineID):
		fp, ferr := pipelineFingerprintOf(ctx, d.Pipelines, rec.ProjectID, *obs.failed)
		if ferr != nil {
			d.Log.Warn("sweep: could not fingerprint the newest failed pipeline; looking again later",
				"bead", rec.BeadAnchor, "pipeline", obs.failed.ID, "err", ferr)
			put("unreadable")
			return rec
		}
		if fp != rec.PipelineFingerprint {
			if !editRunNote(ctx, d, l, rec, pipelinectx.RunStateSuperseded, supersededStatus(rec, obs.failed.ID)) {
				put("note_unwritten")
				return rec
			}
			rec.PipelineRunOpen = false
			d.Log.Info("sweep: pipeline run superseded by a different failure", "bead", rec.BeadAnchor,
				"pipeline", obs.failed.ID)
			put("superseded")
			return rec
		}
		prev := rec.PipelineLatestFailed
		rec.PipelineLatestFailed = obs.failed.ID
		if !stillFailing(ctx, d, l, rec) {
			rec.PipelineLatestFailed = prev
			put("note_unwritten")
			return rec
		}
		d.Log.Info("sweep: pipeline run still failing", "bead", rec.BeadAnchor, "latest_failed", obs.failed.ID)
		put("still_failing")

	default:
		put("unchanged")
	}
	return rec
}

// editRunNote moves the run's note to a closing state, keeping its preserved
// body, and updates the landing's labels (and closes the tracking issue on
// resolution). It reports whether the note is now in that state; a run whose
// note does not exist is treated as written (nothing to edit is not a reason
// to keep a run open forever).
func editRunNote(ctx context.Context, d sweepDeps, l *pipelineLander, rec beadstore.Record, state, status string) bool {
	t, _ := pipelineRunOf(rec)
	ld, err := l.findRunLanding(ctx, rec, t)
	if err != nil {
		d.Log.Warn("sweep: could not read the pipeline run's landing", "bead", rec.BeadAnchor, "err", err)
		return false
	}
	if ld.note == nil {
		d.Log.Warn("sweep: the pipeline run has no note to update; closing the run without one",
			"bead", rec.BeadAnchor, "state", state)
		return true
	}
	ld, err = l.writeRunNote(ctx, rec, runNote{State: state, Status: status, Body: preservedBody(ld.note.Body)})
	if err != nil {
		d.Log.Warn("sweep: could not update the pipeline run's note", "bead", rec.BeadAnchor, "state", state, "err", err)
		return false
	}
	l.applyLandingLabels(ctx, rec, ld, state, "")
	if state == pipelinectx.RunStateResolved && t.Kind == pipelinectx.TargetKindRef && ld.issue != nil && ld.issue.State == "opened" {
		if err := l.f.CloseIssue(ctx, rec.ProjectID, ld.issue.IID); err != nil {
			d.Log.Warn("sweep: could not close the tracking issue of a resolved run", "bead", rec.BeadAnchor,
				"issue", ld.issue.IID, "err", err)
		}
	}
	return true
}

// stillFailing re-renders a run's note with the newer latest failing pipeline.
// Only a DIAGNOSED note has that line; any other state is left as it is.
func stillFailing(ctx context.Context, d sweepDeps, l *pipelineLander, rec beadstore.Record) bool {
	t, _ := pipelineRunOf(rec)
	ld, err := l.findRunLanding(ctx, rec, t)
	if err != nil {
		d.Log.Warn("sweep: could not read the pipeline run's landing", "bead", rec.BeadAnchor, "err", err)
		return false
	}
	if ld.note == nil || ld.state != pipelinectx.RunStateDiagnosed {
		return true
	}
	ld, err = l.writeRunNote(ctx, rec, runNote{State: pipelinectx.RunStateDiagnosed, Status: diagnosedStatus(rec),
		Body: preservedBody(ld.note.Body)})
	if err != nil {
		d.Log.Warn("sweep: could not update the pipeline run's note", "bead", rec.BeadAnchor, "err", err)
		return false
	}
	l.applyLandingLabels(ctx, rec, ld, pipelinectx.RunStateDiagnosed, "")
	return true
}
