package main

// The dispatch half of the pipeline path (gonk-tios D6/D9/D12): the run
// identity an order must carry, the context and checkout a session is
// started with, what the running record keeps for the sweep to check the
// answer against, and the run note's dispatch-time lifecycle.

import (
	"context"
	"errors"
	"fmt"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// checkPipelineIdentity refuses a pipeline order whose run identity does not
// rebuild its own bead anchor: pipelinectx.Anchor(project_id, target,
// fingerprint, last_success, fix_epoch) must equal bead_anchor exactly, and
// the order must name a pipeline. The fix epoch (gonk-2usw 3.3) is checked
// like every other part: an order whose anchor carries ":f<iid>" must carry
// the same iid as pipeline_fix_epoch, and one whose anchor carries none must
// carry none.
func checkPipelineIdentity(a dispatchArgs) error {
	if a.PipelineID < 1 {
		return fmt.Errorf("no pipeline id (pipeline_id %d)", a.PipelineID)
	}
	t, err := pipelinectx.ParseTarget(a.PipelineTarget)
	if err != nil {
		return err
	}
	want, err := pipelinectx.Anchor(a.ProjectID, t, a.PipelineFingerprint, a.PipelineLastSuccess, a.PipelineFixEpoch)
	if err != nil {
		return err
	}
	if want != a.BeadAnchor {
		return fmt.Errorf("bead_anchor %q is not the anchor of this run identity (%q)", a.BeadAnchor, want)
	}
	return nil
}

// setPipelineIdentity puts the run identity on the record dispatch writes.
// The pipeline diagnosed is RECORDED and reused by every attempt and re-sling
// of the anchor (D6): if the record read before /decide already names one, it
// wins over the order's (a later failure of the same run arriving through
// intake must not rename the session mid-ladder). The anchor fixes everything
// else, so only the pipeline id can differ; the newest failing pipeline seen
// is kept as PipelineLatestFailed.
func setPipelineIdentity(base *beadstore.Record, a dispatchArgs, decided decidedOnRecord) {
	base.PipelineID = a.PipelineID
	base.PipelineTarget = a.PipelineTarget
	base.PipelineFingerprint = a.PipelineFingerprint
	base.PipelineLastSuccess = a.PipelineLastSuccess
	base.PipelineFixEpoch = a.PipelineFixEpoch
	base.PipelineLatestFailed = a.PipelineID
	if prior := decided.rec; decided.err == nil && decided.found && prior.BeadAnchor == a.BeadAnchor && prior.PipelineID != 0 {
		base.PipelineID = prior.PipelineID
		base.PipelineSHA = prior.PipelineSHA
		base.PipelineProjectURL = prior.PipelineProjectURL
		base.PipelineFirstFailed = prior.PipelineFirstFailed
		base.PipelineLatestFailed = max(prior.PipelineLatestFailed, prior.PipelineID, a.PipelineID)
		// What the sweep recorded about a refused earlier answer goes with
		// the run to its next attempt (retry_feedback.go).
		base.RetryFeedback = prior.RetryFeedback // dispatch:carry-feedback
	}
	base.PipelineRunOpen = true
}

// pipelineDispatch is what preparePipelineDispatch built for one session.
type pipelineDispatch struct {
	prompt      string
	checkoutURL string
	checkoutSHA string

	sha         string
	projectURL  string // the project's web URL, from the pipeline object ("" = unknown: no links)
	jobs        []beadstore.PipelineJob
	maxBytes    int
	firstFailed int64
}

// apply records, on the running record, everything the sweep needs to check
// this session's answer (D8 steps 4-7) without trusting the batch for any of
// it.
func (p pipelineDispatch) apply(rec beadstore.Record) beadstore.Record {
	rec.PipelineSHA = p.sha
	if p.projectURL != "" {
		rec.PipelineProjectURL = p.projectURL
	}
	rec.PipelineJobs = p.jobs
	rec.PipelineExcerptVersion = pipelinectx.ExcerptVersion
	rec.PipelineExcerptMaxBytes = p.maxBytes
	if p.firstFailed != 0 {
		rec.PipelineFirstFailed = p.firstFailed
	}
	rec.PipelineRunOpen = true
	return rec
}

// preparePipelineDispatch reads the failed pipeline's context, grants a
// checkout at the PIPELINE'S commit and renders the prompt within its cap.
// An error means no session can be started: the pipeline or its job list
// could not be read, or the prompt cannot be made to fit. A failed checkout
// grant is NOT an error -- the prompt then says there is no working copy.
func preparePipelineDispatch(ctx context.Context, d dispatchDeps, session string, rec beadstore.Record) (pipelineDispatch, error) {
	var out pipelineDispatch
	if d.Pipelines == nil {
		return out, errors.New("no pipeline forge configured")
	}
	t, err := pipelinectx.ParseTarget(rec.PipelineTarget)
	if err != nil {
		return out, err
	}
	pc, err := buildPipelineContext(ctx, d.Pipelines, pipelineContextRequest{
		ProjectID: rec.ProjectID, Project: rec.Project, PipelineID: rec.PipelineID,
		Target: t, LastSuccess: rec.PipelineLastSuccess,
		Attempt: rec.Attempt, Refused: rec.RetryFeedback,
	})
	if err != nil {
		return out, err
	}
	for _, u := range pc.Unavailable {
		// Logged, never rendered: the prompt says only "(unavailable)".
		d.Log.Warn("pipeline context block unavailable; the prompt says so", "bead", rec.BeadAnchor, "block", u)
	}
	out.sha = pc.Pipeline.SHA
	out.projectURL = projectWebURLOf(pc.Pipeline)
	if url, sha, gerr := grantCheckoutAt(ctx, d, pipelineAgent, session, pc.Pipeline.SHA); gerr != nil {
		d.Log.Warn("could not grant a checkout at the pipeline's commit; prompting without a working copy",
			"err", gerr, "bead", rec.BeadAnchor, "project", rec.Project)
	} else {
		out.checkoutURL, out.checkoutSHA = url, sha
	}
	prompt, err := fitPipelinePrompt(pc, out.checkoutSHA)
	if err != nil {
		return out, err
	}
	out.prompt = prompt
	out.maxBytes = pc.ExcerptMaxBytes
	for _, j := range pc.JobRecords() {
		out.jobs = append(out.jobs, beadstore.PipelineJob{
			ID: j.ID, Name: j.Name, Excerpted: j.Excerpted, ExcerptSHA256: j.ExcerptSHA256, Groundable: j.Groundable,
			// The failure facts a pipeline-fix is judged against (gonk-2usw).
			Stage: j.Stage, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure,
			ErrorSignature: j.ErrorSignature, ErrorGeneric: j.ErrorGeneric,
			SignatureVersion: pipelinectx.SignatureVersion,
		})
	}
	if pc.Run.First != nil {
		out.firstFailed = pc.Run.First.ID
	}
	return out, nil
}

// postPipelineRunNote writes the run note's dispatch-time state (diagnosing,
// deferred, denied) and the landing's labels/description. Called only AFTER
// the record write it reports has landed. BEST EFFORT, bounded by
// statusNoteTimeout: a failure is logged at WARN and never changes the exit
// code or the record.
func (d dispatchDeps) postPipelineRunNote(ctx context.Context, rec beadstore.Record, n runNote) {
	if d.Pipelines == nil || terminated(ctx) {
		return
	}
	nctx, cancel := context.WithTimeout(ctx, statusNoteTimeout)
	defer cancel()
	l, err := newPipelineLander(nctx, d.Pipelines, d.Log)
	if err != nil {
		d.Log.Warn("pipeline run note not written", "bead", rec.BeadAnchor, "state", n.State, "err", err)
		return
	}
	ld, err := l.writeRunNote(nctx, rec, n)
	if errors.Is(err, errRunClosed) {
		d.Log.Info("pipeline run note not rewritten: the run is already closed", "bead", rec.BeadAnchor, "state", n.State)
		return
	}
	if err != nil {
		d.Log.Warn("pipeline run note not written", "bead", rec.BeadAnchor, "state", n.State, "err", err)
		return
	}
	l.applyLandingLabels(nctx, rec, ld, n.State, "")
	d.Log.Info("pipeline run note written", "bead", rec.BeadAnchor, "state", n.State, "target", rec.PipelineTarget)
}
