package main

import (
	"context"
	"fmt"
	"strings"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/effects"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// The sentinels the agent fences its proposed-effects batch with (they match the
// prompt renderTriagePrompt injects). The batch is JSON between them.
const (
	batchStartSentinel = "GONK_BATCH_START"
	batchEndSentinel   = "GONK_BATCH_END"
	// brokerPeekLines is how many lines of the session's last-output preview to
	// ask for. It must be large enough to hold the whole fenced batch; C2's first
	// live run confirms this is enough (or resizes it) -- see the plan's
	// "Return-read observation".
	brokerPeekLines = 400
	// sessionStateRunning is gascity's SessionView.State for a session that is
	// still working. Checked alongside the boolean Running so a server that
	// populates only one of the two still reads as unfinished -- failing "safe"
	// here means declining to judge, which costs one more 30s sweep, whereas
	// failing the other way escalates a live session onto a pricier rung.
	sessionStateRunning = "running"
)

// brokerApplier is the WRITE side of pkg/glab the broker uses to apply a
// validated batch under the controller's own bot PAT (the agent pod holds no
// forge creds). Satisfied by *glab.Client directly.
type brokerApplier interface {
	CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (*glab.Note, error)
	AddIssueLabel(ctx context.Context, projectID, issueIID int64, label string) error
	// CloseIssue is the `close` verdict's action (gonk-aib). The broker performs
	// it; the model only proposes the conclusion.
	CloseIssue(ctx context.Context, projectID, issueIID int64) error
	// UpdateIssueNote rewrites a note gonk already posted. It is what lets the
	// canned status comment be edited in place and then REPLACED by the real
	// answer, so a thread ends with the answer rather than a stale apology
	// (gonk-yrs).
	UpdateIssueNote(ctx context.Context, projectID, issueIID, noteID int64, body string) (*glab.Note, error)
	// The scaffold half. The agent proposes .agent/ content and holds no
	// credentials, so the CONTROLLER commits it and opens the merge request --
	// the same inversion as triage, where the agent proposes a comment and the
	// controller posts it.
	CreateCommit(ctx context.Context, projectID int64, opts glab.CommitOptions) (*glab.Commit, error)
	CreateMergeRequest(ctx context.Context, projectID int64, opts glab.MROptions) (*glab.MergeRequest, error)
	ListMergeRequests(ctx context.Context, projectID int64, opts glab.MRListOptions) ([]glab.MergeRequest, error)
	GetProject(ctx context.Context, projectID int64) (*glab.Project, error)
}

// scaffoldBranch is the source branch the scaffold MR comes from. Fixed, not
// per-run: gonk-gate check looks for an MR from exactly this branch, and a
// re-sling must update the same branch rather than litter the repository with
// one branch per attempt.
const scaffoldBranch = "gonk/scaffold"

// applyScaffoldFiles commits a validated batch of file effects onto
// scaffoldBranch and opens exactly one merge request carrying the bead marker.
//
// Idempotent by construction, because a re-sling runs it again: the commit uses
// StartBranch so GitLab creates the branch on the first run and commits onto it
// afterwards, and an existing open MR from the branch is reused rather than
// duplicated. "Exactly one MR" is the gate's requirement, not a preference.
func applyScaffoldFiles(ctx context.Context, d sweepDeps, rec beadstore.Record, batch effects.Batch, marker string) (string, error) {
	actions := make([]glab.CommitAction, 0, len(batch.Effects))
	for _, e := range batch.Effects {
		if e.Kind != effects.KindFile {
			continue
		}
		// "update" fails when the file is absent and "create" fails when it is
		// present, and a re-scaffold legitimately hits both. GitLab does not
		// offer an upsert, so ask what is there: the branch may already carry a
		// previous attempt's .agent/.
		action := "create"
		// A 1-byte read is enough: we only need presence, not content, and the
		// cap keeps a hostile file from being pulled into memory to answer it.
		if _, err := d.GL.GetRawFile(ctx, rec.ProjectID, e.Path, scaffoldBranch, 1); err == nil {
			action = "update"
		}
		actions = append(actions, glab.CommitAction{Action: action, FilePath: e.Path, Content: e.Content})
	}
	if len(actions) == 0 {
		return "", nil
	}

	proj, perr := d.Apply.GetProject(ctx, rec.ProjectID)
	if perr != nil {
		return "", perr
	}
	base := proj.DefaultBranch
	if base == "" {
		base = "main"
	}

	if _, cerr := d.Apply.CreateCommit(ctx, rec.ProjectID, glab.CommitOptions{
		Branch:        scaffoldBranch,
		StartBranch:   base, // creates the branch when absent; ignored when it exists
		CommitMessage: "gonk: scaffold .agent/\n\n" + marker,
		Actions:       actions,
	}); cerr != nil {
		return "", cerr
	}

	// One MR, reused across re-slings. The marker must be in the DESCRIPTION on
	// its own line -- that is what gonk-gate check looks for.
	existing, lerr := d.Apply.ListMergeRequests(ctx, rec.ProjectID, glab.MRListOptions{
		SourceBranch: scaffoldBranch, State: "opened",
	})
	if lerr != nil {
		return "", lerr
	}
	if len(existing) > 0 {
		return existing[0].WebURL, nil
	}

	mr, merr := d.Apply.CreateMergeRequest(ctx, rec.ProjectID, glab.MROptions{
		SourceBranch: scaffoldBranch,
		TargetBranch: base,
		Title:        "gonk: scaffold .agent/",
		Description: "gonk read this repository and proposed durable context for future " +
			"triage and mention sessions.\n\nReview it as you would any MR: it is a proposal, not a fact.\n\n" + marker,
	})
	if merr != nil {
		return "", merr
	}
	return mr.WebURL, nil
}

// extractBatch pulls the JSON between the LAST GONK_BATCH_START and the
// following GONK_BATCH_END out of a session's output preview. The agent emits
// the fence as the last thing in its output, so the LAST start guards against
// the sentinels appearing earlier (echoed in the injected prompt, or in the
// model's own reasoning). ok=false if the fence is absent, out of order, or
// empty.
// isUnreadableTranscript reports whether a transcript carries no content at all,
// which means WE failed to read it rather than that the agent stayed silent. A
// real session always carries at least the harness banner, so this cannot hide a
// genuinely silent agent.
func isUnreadableTranscript(text string) bool {
	return strings.TrimSpace(text) == ""
}

func extractBatch(output string) ([]byte, bool) {
	start := strings.LastIndex(output, batchStartSentinel)
	if start < 0 {
		return nil, false
	}
	rest := output[start+len(batchStartSentinel):]
	end := strings.Index(rest, batchEndSentinel)
	if end < 0 {
		return nil, false
	}
	payload := strings.TrimSpace(rest[:end])
	if payload == "" {
		return nil, false
	}
	return []byte(payload), true
}

// applyBrokerBatch is the sweep-side broker: read the agent's session output,
// extract + parse + deterministically SHAPE-VALIDATE the proposed-effects batch,
// and -- only if it passes every gate -- apply it (post the comment with the
// bead marker + add labels) under the bot PAT.
//
// Return contract, chosen so the caller can fold the result into gate.Signals:
//   - applied=true  -> a valid batch was accepted and its comment posted. The
//     caller sets ArtifactPresent=true (=> success).
//   - applied=false, violation!="" -> the batch was READ but rejected (no fence,
//     unparseable, out of shape, bad target). Nothing was applied; the caller
//     leaves ArtifactPresent=false so the bead falls to the ladder (spend>0 =>
//     escalate/needs-human; else retry). violation is logged.
//   - err!=nil -> the output could not be READ or our own shape file is broken:
//     an UNKNOWN, not a rejection. The caller sets ArtifactUnknown (=> retry,
//     never escalate -- uncertainty must not walk a project up the ladder).
//
// It never half-applies a rejected batch: validation is complete before the
// first write. The single comment (shape guarantees exactly one) is applied
// first and MUST succeed; labels follow and a label failure does not un-apply
// the comment (it is already the human-visible triage result).
// The caller fetches the SessionView (see brokerSessionView) and passes it in,
// so the finished/not-finished decision and this read share ONE fetch and
// cannot disagree.
func applyBrokerBatch(ctx context.Context, d sweepDeps, agent string, rec beadstore.Record, view *gcapi.SessionView) (applied bool, violation string, err error) {
	if d.Apply == nil {
		return false, "", fmt.Errorf("broker applier not configured")
	}

	// The BATCH comes from the transcript, not from view.LastOutput: peek is a
	// bounded preview window (brokerPeekLines), so an agent that keeps talking
	// after the fence pushes it out of view and the batch reads as absent --
	// silently costing a re-sling onto a pricier rung with the agent's work
	// thrown away (gonk-u1p.3). The view is still what told us the session
	// finished; it is simply the wrong place to read the output of record from.
	tr, terr := d.GC.GetSessionTranscript(ctx, rec.SessionID)
	if terr != nil {
		if gcapi.IsNotFound(terr) {
			return false, "no session for alias " + rec.SessionID, nil
		}
		return false, "", terr // transport error -> unknown -> retry
	}
	if tr.Pagination != nil && tr.Pagination.HasMore {
		// We read a fragment. Say so rather than judging on it: "no batch"
		// derived from a partial transcript is the same silent loss in a new
		// costume. Unknown -> retry, never escalate.
		return false, "", fmt.Errorf("transcript for %q is paginated; refusing to judge a partial read", rec.SessionID)
	}
	// AN EMPTY TRANSCRIPT IS A FAILED READ, NOT A SILENT AGENT (gonk-2tb).
	// Judging it as "no fence" charges the agent for OUR inability to read it,
	// and the cost is not abstract: it burns the attempt, re-slings onto a
	// pricier rung, and eventually denies the bead as ladder-exhausted with the
	// agent's completed work sitting unread. Measured on issue !42 -- a valid
	// batch existed in the pod while the sweep recorded "no fence", because the
	// transcript is served through tmux and tmux had exited.
	//
	// This is the same rule the pagination check above already applies, for the
	// same reason: an incomplete read must resolve to UNKNOWN -> retry, never to
	// a verdict. A genuinely silent agent still produces a non-empty transcript
	// (the harness banner alone guarantees that), so this cannot mask one.
	if isUnreadableTranscript(tr.Text()) {
		return false, "", fmt.Errorf("transcript for %q is empty; refusing to judge an unreadable session", rec.SessionID)
	}
	raw, ok := extractBatch(tr.Text())
	if !ok {
		return false, "no GONK_BATCH_START/END fence in session transcript", nil
	}
	batch, perr := effects.ParseBatch(raw)
	if perr != nil {
		return false, "unparseable batch: " + perr.Error(), nil
	}
	shape, serr := effects.LoadShape(d.PackDir, agent)
	if serr != nil {
		// A missing/broken shape file is OUR misconfiguration, not the agent's
		// fault. Fail closed: never apply an unvalidated batch.
		return false, "", serr
	}
	if verr := effects.Validate(batch, shape); verr != nil {
		return false, "shape: " + verr.Error(), nil
	}
	if terr := effects.ValidateTargets(batch, map[int64]bool{rec.IssueIID: true}); terr != nil {
		return false, "target: " + terr.Error(), nil
	}
	// The path gate. Separate from Validate (which asks "is this the right shape
	// of batch for this agent") because this asks "are these writes allowed at
	// all" -- and unlike a comment, a file effect becomes a commit in someone's
	// repository. A batch must pass both.
	if perr := effects.ValidatePaths(batch); perr != nil {
		return false, "path: " + perr.Error(), nil
	}
	// The FIFTH GATE (gonk-hsb): not "is this batch well-formed" but "did the
	// session do the work it claims to report". Observing-only until
	// EnforceTrajectory is set -- see checkTrajectory.
	if v := checkTrajectory(ctx, d, rec, agent, batch); v != "" {
		return false, v, nil
	}
	// THE LABEL GATE (T-05, closes R-02): checked before ANY write -- comments
	// and the scaffold MR included -- so a batch with one bad label effect
	// produces ZERO GitLab writes, not a comment posted and a label silently
	// dropped. normaliseLabel refuses rather than repairs a comma, a
	// whitespace-only or oversize value, or an attempt to mint one of the
	// broker's own audit labels (ReservedLabels); refusing that one effect
	// must refuse the whole batch, because a half-applied batch is not one a
	// human can reconstruct the refusal reason for from the issue afterward.
	prefix := labelPrefix(ctx, d, rec)
	for _, e := range batch.Effects {
		if e.Kind != effects.KindLabel {
			continue
		}
		for _, lbl := range e.Add {
			if _, lerr := normaliseLabel(lbl, prefix); lerr != nil {
				return false, "label: " + lerr.Error(), nil
			}
		}
	}

	// Shape-valid. Apply comment(s) first (deterministic order, independent of
	// the agent's emit order), then labels.
	marker := fmt.Sprintf("<!-- gonk:bead:%s -->", rec.BeadID)

	// Scaffold's artifact is a merge request, not a comment: the agent proposed
	// .agent/ content and the controller commits it. Done before comments and
	// labels because it is this trigger's gate artifact -- if it fails, nothing
	// else about the run matters.
	if url, serr := applyScaffoldFiles(ctx, d, rec, batch, marker); serr != nil {
		return false, "", serr // -> unknown -> retry; nothing durable applied
	} else if url != "" {
		d.Log.Info("sweep: scaffold merge request ready", "bead", rec.BeadAnchor, "mr", url)
	}
	// THE ANSWER REPLACES THE PLACEHOLDER (gonk-yrs). If gonk previously said
	// "I cannot answer yet", that note is edited into the real answer rather
	// than left above it, so the thread ends with the answer and not the
	// apology. Only the FIRST comment claims the placeholder; anything further
	// is posted normally.
	replaced := false
	for _, e := range batch.Effects {
		if e.Kind != effects.KindComment {
			continue
		}
		iid := targetIID(e, rec)
		body := e.Body
		if f := mentionFooter(d.BotUsername); f != "" {
			body += "\n\n" + f
		}
		body += "\n\n" + marker
		if !replaced {
			if st := findStatusNote(ctx, d, rec.ProjectID, iid, rec.BeadID); st != nil {
				if _, uerr := d.Apply.UpdateIssueNote(ctx, rec.ProjectID, iid, st.ID, body); uerr == nil {
					replaced = true
					continue
				}
				// Fall through and post normally: a failed edit must not cost the
				// reporter the answer.
			}
			replaced = true
		}
		if _, aerr := d.Apply.CreateIssueNote(ctx, rec.ProjectID, iid, body); aerr != nil {
			// The gate artifact failed to post; nothing durable applied yet.
			return false, "", aerr // -> unknown -> retry
		}
	}
	for _, e := range batch.Effects {
		if e.Kind != effects.KindLabel {
			continue
		}
		for _, lbl := range e.Add {
			// The namespace is the ISSUE STORE'S, not the model's (gonk-prr).
			// The error is ignored here, not unchecked: the label gate above
			// already ran normaliseLabel over every effect in this batch with
			// this same prefix and refused the batch on the first error, so by
			// the time this loop runs every label in it is known-good.
			norm, _ := normaliseLabel(lbl, prefix)
			if aerr := d.Apply.AddIssueLabel(ctx, rec.ProjectID, targetIID(e, rec), norm); aerr != nil {
				d.Log.Warn("sweep: label apply failed (comment already posted)",
					"bead", rec.BeadAnchor, "label", norm, "err", aerr)
			}
		}
	}
	// The verdict acts LAST, so the reporter has the explanation in hand before
	// the issue changes state underneath them (gonk-aib).
	applyVerdict(ctx, d, rec, batch)
	return true, "", nil
}

// targetIID resolves an effect's target: an explicit, context-validated iid, or
// the bead's primary issue when the effect names none (TargetIID==0).
func targetIID(e effects.Effect, rec beadstore.Record) int64 {
	if e.TargetIID != 0 {
		return e.TargetIID
	}
	return rec.IssueIID
}

// brokerSessionView fetches the session the bead's alias names and decides
// whether there is anything to judge yet.
//
// It returns (nil, nil) when the session is STILL RUNNING: that is not a
// failure and not an absent artifact, it is "not knowable yet". gonk-sweep is a
// 30s cooldown order while a triage session takes minutes, so this is the
// expected answer for most of a session's life -- and treating it as "the agent
// produced no batch" would report a gate failure and re-sling the bead onto a
// more expensive rung while the original agent is still working (gonk-u1p.2).
//
// A 404 is NOT running-ness: no session exists for this alias at all (the async
// create failed, or it was reaped), so there genuinely is nothing coming. That
// is returned as a view with no output, which the caller judges as "no batch"
// -- a re-sling is the correct response there.
func brokerSessionView(ctx context.Context, d sweepDeps, rec beadstore.Record) (*gcapi.SessionView, error) {
	view, err := d.GC.GetSessionOutput(ctx, rec.SessionID, brokerPeekLines)
	if err != nil {
		if gcapi.IsNotFound(err) {
			return &gcapi.SessionView{ID: rec.SessionID}, nil
		}
		return nil, err // transport error -> unknown -> retry next tick
	}
	if view.Running || view.State == sessionStateRunning {
		// A CLOSED FENCE IS A COMPLETION SIGNAL IN ITS OWN RIGHT.
		//
		// Waiting for the provider to say "not running" was the single reason
		// gonk never posted a triage comment (gonk-5k5). opencode's TUI does
		// not exit after answering, so the session stays Running forever: the
		// agent emitted a complete batch in a 32.6s model turn and the sweep
		// still reported "nothing to judge yet" until the reservation expired
		// and the bead was classified infra-failed. The verdict was gated on a
		// process lifecycle detail rather than on the work.
		//
		// The prompt's contract is "Nothing after GONK_BATCH_END", so a closed
		// fence means the agent is done by definition -- whatever the harness
		// process does afterwards. Running non-interactively (so the process
		// exits) is the primary fix; this is the backstop, and it is the one
		// that holds when a model/harness pair pauses, prompts, or otherwise
		// hesitates instead of exiting.
		//
		// Detection only. LastOutput is a bounded peek, and judging a batch
		// from a peek was itself a past bug -- so this decides that the
		// session is JUDGEABLE and lets the normal path re-read the full
		// transcript before extracting anything. The fence is emitted last,
		// which is exactly what a tail-peek sees.
		if _, ok := extractBatch(view.LastOutput); ok {
			return view, nil
		}
		return nil, nil
	}
	return view, nil
}
