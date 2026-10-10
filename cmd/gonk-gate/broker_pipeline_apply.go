package main

// THE PIPELINE DIAGNOSIS'S VALIDATION CHAIN (gonk-tios D8), run by the sweep
// on a finished pipeline session's batch BEFORE ANY WRITE. applyBrokerBatch
// reads the session's output, extracts the fence and parses the batch (D8
// step 1), then routes here with one line.
//
// Every step refuses the WHOLE batch: nothing of it is written. A refusal is
// either GATE-FAILED (a violation: the answer is wrong, the ladder may
// escalate) or UNKNOWN (an error: gonk could not check, which must never
// escalate -- it retries). Each refusal is logged once, here, with its step
// and a cause code an operator can count:
//
//	step  check                                           refusal (cause)
//	2     effects.Validate vs the pipeline shape          gate-failed (shape)
//	3     effects.ValidateTargets, only the recorded      gate-failed (target)
//	      pipeline (or none)
//	4     effects.ValidateDiagnosis vs the RECORD's       gate-failed (diagnosis_invalid)
//	      failed jobs
//	5     re-fetch each excerpted job's trace with the    UNKNOWN (pipeline_context_changed)
//	      dispatch's own fetch and the recorded cap, and
//	      pipelinectx.VerifyExcerpt against the recorded
//	      hash and version
//	6     pipelinectx.Grounded for every quoted line,     gate-failed (evidence_ungrounded)
//	      against THAT job's re-derived excerpt
//	7     every cited file exists at the pipeline's SHA   gate-failed (cited_file_missing) on 404,
//	      (a 1-byte read: citations are never writes)     UNKNOWN (cited_file_unreadable) otherwise
//	8     render the landing note; refuse agent text      gate-failed (would_forge_marker,
//	      carrying a gonk marker, a rendered note in      would_post_secret, quick_action)
//	      which pipelinectx.Redact finds anything, and
//	      one the comment gate refuses
//	9     trajectory, keyed on the classification         gate-failed (trajectory), only when
//	                                                      enforcement is on
//
// Then the run note is written (it MUST land, else UNKNOWN) and the labels
// and tracking-issue description follow (WARN only).
//
// WHAT IS DELIBERATELY ABSENT: effects.ValidatePaths / ValidateProtectedPaths.
// A diagnosis's `files` are CITATIONS, not writes (P5): nothing on this path
// can write to a repository, and a diagnosis citing .gitlab-ci.yml is exactly
// what a configuration failure needs. Their sanity is step 4's, then 7's.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// pipelineAgent is the pipeline trigger's broker agent (agentForTrigger).
const pipelineAgent = "pipeline"

// Refusal causes (the `cause` attribute of the refusal log line).
const (
	causeShape            = "shape"
	causeTarget           = "target"
	causeDiagnosisInvalid = "diagnosis_invalid"
	causeContextChanged   = "pipeline_context_changed"
	causeUngrounded       = "evidence_ungrounded"
	causeFileMissing      = "cited_file_missing"
	causeFileUnreadable   = "cited_file_unreadable"
	causeForgedMarker     = "would_forge_marker"
	causeSecret           = "would_post_secret"
	causeQuickAction      = "quick_action"
	causeTrajectory       = "trajectory"
	causeNotConfigured    = "not_configured"
	causeLandingFailed    = "landing_failed"
)

// gonkMarkerRe finds anything that would read as one of gonk's own HTML
// markers (a run marker, a body fence, a bead or status marker). Agent text
// carrying one could forge a run state gonk-intake trusts -- a bot-authored
// note saying another run is "diagnosed" would silence it -- so it is refused,
// never escaped.
var gonkMarkerRe = regexp.MustCompile(`(?i)<!--\s*/?\s*gonk`)

// pipelineDiagnosisContext is ValidateDiagnosis's context, built ONLY from the
// record dispatch wrote -- never from the batch.
func pipelineDiagnosisContext(rec beadstore.Record) effects.DiagnosisContext {
	var dc effects.DiagnosisContext
	for _, j := range rec.PipelineJobs {
		dc.FailedJobs = append(dc.FailedJobs, effects.FailedJob{Name: j.Name, HasExcerpt: j.Groundable})
	}
	return dc
}

// applyPipelineBatch is D8 steps 2-9 and the apply. Same return contract as
// applyBrokerBatch: applied, a violation (gate-failed), or an error (UNKNOWN).
//
// ON SUCCESS -- and only then -- the landed diagnosis is recorded on *recp
// (PipelineDiagnosis, gonk-2usw). Nothing here writes the store: the caller's
// own record write, the one that follows its outcome report, persists it. A
// refused or uncheckable batch leaves *recp untouched.
func applyPipelineBatch(ctx context.Context, d sweepDeps, recp *beadstore.Record, batch effects.Batch) (applied bool, violation string, err error) {
	rec := *recp
	// fb is the controller-built word for the next attempt (retry_feedback.go).
	// Its code is the refusal's cause; `detail` -- which quotes the batch --
	// never reaches it.
	refuse := func(step int, cause, detail string, fb effects.Feedback) (bool, string, error) {
		d.Log.Warn("sweep: pipeline diagnosis refused, applied nothing",
			"bead", rec.BeadAnchor, "step", step, "cause", cause, "detail", detail)
		fb.Code = cause
		recordRetryFeedback(recp, fb)
		return false, cause + ": " + detail, nil
	}
	unknown := func(step int, cause string, e error) (bool, string, error) {
		d.Log.Warn("sweep: pipeline diagnosis could not be checked, applied nothing; retrying, not escalating",
			"bead", rec.BeadAnchor, "step", step, "cause", cause, "err", e)
		return false, "", fmt.Errorf("%s: %w", cause, e)
	}
	if d.Pipelines == nil {
		return unknown(0, causeNotConfigured, fmt.Errorf("no pipeline forge configured"))
	}

	// 2. Shape: exactly one diagnosis, nothing else.
	shape, serr := effects.LoadShape(d.PackDir, pipelineAgent)
	if serr != nil {
		return false, "", serr // OUR misconfiguration: never apply unvalidated
	}
	if verr := effects.Validate(batch, shape); verr != nil {
		fb := effects.ExplainShape(shape)
		if r := effects.AsRefusal(verr); r != nil {
			fb = effects.ExplainFix(r, batch)
		}
		return refuse(2, causeShape, verr.Error(), fb)
	}
	var diag effects.Effect
	for _, e := range batch.Effects {
		if e.Kind == effects.KindDiagnosis {
			diag = e
		}
	}

	// 3. Target: the recorded pipeline, or none. The landing is never the
	// agent's choice.
	if terr := effects.ValidateTargets(batch, effects.Targets{Pipelines: map[int64]bool{rec.PipelineID: true}}); terr != nil {
		return refuse(3, causeTarget, terr.Error(), effects.Feedback{What: feedbackDiagTarget})
	}

	// 4. The diagnosis's own rules, against the jobs the RECORD says failed.
	if derr := effects.ValidateDiagnosis(batch, pipelineDiagnosisContext(rec)); derr != nil {
		return refuse(4, causeDiagnosisInvalid, derr.Error(), effects.ExplainDiagnosis(derr, batch))
	}

	// 5. Context integrity: the excerpts the model was shown, re-derived now
	// with the SAME fetch and the RECORDED cap, must hash to what dispatch
	// recorded. Otherwise its quotes cannot be checked: UNKNOWN.
	excerpts := map[string][]string{} // job name -> re-derived excerpt texts
	for _, j := range rec.PipelineJobs {
		if !j.Excerpted {
			continue
		}
		tt, ferr := fetchPipelineTrace(ctx, d.Pipelines, rec.ProjectID, j.ID)
		if ferr != nil {
			return unknown(5, causeContextChanged, fmt.Errorf("re-read the log of job %q: %w", j.Name, ferr))
		}
		ex, verr := pipelinectx.VerifyExcerpt(tt.Data, pipelineExcerptOptions(tt, rec.PipelineExcerptMaxBytes),
			rec.PipelineExcerptVersion, j.ExcerptSHA256)
		if verr != nil {
			return unknown(5, causeContextChanged, fmt.Errorf("job %q: %w", j.Name, verr))
		}
		excerpts[j.Name] = append(excerpts[j.Name], ex.Text)
	}

	// 6. Grounding -- the hallucination check. Every quoted line must occur,
	// verbatim up to whitespace, on ONE line of THAT job's excerpt.
	for _, ln := range effects.DiagnosisLines(batch) {
		grounded := false
		for _, text := range excerpts[ln.Job] {
			if pipelinectx.Grounded(text, ln.Line) {
				grounded = true
				break
			}
		}
		if !grounded {
			return refuse(6, causeUngrounded, fmt.Sprintf("evidence item %d quotes a line that is not in the log excerpt of job %q",
				ln.Item, ln.Job), effects.Feedback{What: fmt.Sprintf("evidence item %d of %d quotes a line that is not, verbatim and on one line, "+
				"in the log excerpt of the job it names", ln.Item+1, len(diag.Evidence))})
		}
	}

	// 7. Cited files exist at the pipeline's commit. READ-ONLY: a citation is
	// never a write, and nothing here can turn one into one.
	if !glab.IsCommitSHA(rec.PipelineSHA) {
		return unknown(7, causeFileUnreadable, fmt.Errorf("the record carries no pipeline commit (%q)", rec.PipelineSHA))
	}
	for fi, f := range diag.Files {
		// A 1-byte read: a longer file answers ErrResponseTooLarge, which is a
		// 200 -- the file exists -- without pulling its content into memory.
		if _, ferr := d.Pipelines.GetRawFile(ctx, rec.ProjectID, f, rec.PipelineSHA, 1); ferr != nil && !errors.Is(ferr, glab.ErrResponseTooLarge) {
			if glab.IsNotFound(ferr) {
				return refuse(7, causeFileMissing, fmt.Sprintf("cited file %q does not exist at %s", f, rec.PipelineSHA),
					effects.Feedback{What: fmt.Sprintf("entry %d of %d in files does not exist in the repository at the pipeline's commit", fi+1, len(diag.Files))})
			}
			return unknown(7, causeFileUnreadable, fmt.Errorf("cited file %q: %w", f, ferr))
		}
	}

	// 8. What would be posted. Agent text may not carry a gonk marker; the
	// rendered note must hold no secret and pass the comment gate.
	for _, s := range diagnosisTexts(diag) {
		if gonkMarkerRe.MatchString(s) {
			return refuse(8, causeForgedMarker, "the diagnosis carries text that reads as a gonk marker", effects.Feedback{What: feedbackDiagMarker})
		}
	}
	note := runNote{State: pipelinectx.RunStateDiagnosed, Status: diagnosedStatus(rec), Body: renderDiagnosisBody(diag, rec)}
	body, rerr := note.render(rec.BeadAnchor)
	if rerr != nil {
		return false, "", rerr
	}
	if _, kinds := pipelinectx.Redact(body); len(kinds) > 0 {
		return refuse(8, causeSecret, "the rendered note holds what looks like a secret: "+strings.Join(kinds, ","), effects.Feedback{What: feedbackDiagSecret})
	}
	if cerr := effects.ValidateComments(effects.Batch{Effects: []effects.Effect{{Kind: effects.KindComment, Body: body}}}); cerr != nil {
		return refuse(8, causeQuickAction, cerr.Error(), effects.Feedback{What: feedbackDiagQuick})
	}

	// 9. Trajectory, keyed on the classification (trajectoryClaim).
	if v := checkTrajectory(ctx, d, rec, pipelineAgent, batch); v != "" {
		return refuse(9, causeTrajectory, v, effects.Feedback{What: feedbackDiagTrajectory})
	}

	// Apply. The run note MUST land; labels and the description follow.
	lander, lerr := newPipelineLander(ctx, d.Pipelines, d.Log)
	if lerr != nil {
		return unknown(10, causeLandingFailed, lerr)
	}
	ld, werr := lander.writeRunNote(ctx, rec, note)
	if werr != nil {
		return unknown(10, causeLandingFailed, werr)
	}
	// The note is on GitLab: the diagnosis is LANDED. Record it before the
	// best-effort writes below, which cannot un-land it.
	recp.PipelineDiagnosis = landedDiagnosis(diag)
	lander.applyLandingLabels(ctx, rec, ld, pipelinectx.RunStateDiagnosed, diag.Classification)
	d.Log.Info("sweep: pipeline diagnosis landed", "bead", rec.BeadAnchor,
		"target", rec.PipelineTarget, "classification", string(diag.Classification), "note", noteID(ld.note))
	return true, "", nil
}

func noteID(n *glab.Note) int64 {
	if n == nil {
		return 0
	}
	return n.ID
}

// landedDiagnosis is what the record keeps of a diagnosis that passed every
// step above and was written to its landing (Record.PipelineDiagnosis): the
// facts a later pipeline-fix dispatch needs without parsing the rendered note
// (gonk-2usw plan F2).
//
// The slices are copies -- the record must not alias the session's batch.
// EvidenceJobs are the distinct job names the evidence items name, in
// first-cited order; step 4 has already checked each against the RECORD's
// failed jobs, so every one is a PipelineJobs name. A pipeline-level item
// (pipeline_error with no job) names none.
//
// NUL characters are dropped from the prose: the work store's JSONB cannot
// hold one, and a record that cannot be written after its note has landed
// would be re-judged on every sweep, for ever.
func landedDiagnosis(e effects.Effect) beadstore.PipelineDiagnosis {
	d := beadstore.PipelineDiagnosis{
		Class:   string(e.Classification),
		Files:   append([]string(nil), e.Files...),
		Summary: strings.ReplaceAll(e.Summary, "\x00", ""),
		Remedy:  strings.ReplaceAll(e.Remedy, "\x00", ""),
	}
	seen := map[string]bool{}
	for _, ev := range e.Evidence {
		if ev.Job == "" || seen[ev.Job] {
			continue
		}
		seen[ev.Job] = true
		d.EvidenceJobs = append(d.EvidenceJobs, ev.Job)
	}
	return d
}

// diagnosisTexts is every agent-authored string of a diagnosis that reaches
// the rendered note.
func diagnosisTexts(e effects.Effect) []string {
	out := []string{string(e.Classification), e.Summary, e.Remedy}
	for _, ev := range e.Evidence {
		out = append(out, ev.Job, ev.Line, string(ev.Fact))
	}
	return append(out, e.Files...)
}
