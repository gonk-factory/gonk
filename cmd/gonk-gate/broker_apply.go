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
)

// brokerApplier is the WRITE side of pkg/glab the broker uses to apply a
// validated batch under the controller's own bot PAT (the agent pod holds no
// forge creds). Satisfied by *glab.Client directly.
type brokerApplier interface {
	CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (*glab.Note, error)
	AddIssueLabel(ctx context.Context, projectID, issueIID int64, label string) error
}

// extractBatch pulls the JSON between the LAST GONK_BATCH_START and the
// following GONK_BATCH_END out of a session's output preview. The agent emits
// the fence as the last thing in its output, so the LAST start guards against
// the sentinels appearing earlier (echoed in the injected prompt, or in the
// model's own reasoning). ok=false if the fence is absent, out of order, or
// empty.
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
func applyBrokerBatch(ctx context.Context, d sweepDeps, agent string, rec beadstore.Record) (applied bool, violation string, err error) {
	if d.Apply == nil {
		return false, "", fmt.Errorf("broker applier not configured")
	}

	view, gerr := d.GC.GetSessionOutput(ctx, rec.SessionID, brokerPeekLines)
	if gerr != nil {
		if gcapi.IsNotFound(gerr) {
			// No session for this alias: the async create may have failed, or the
			// session never materialized. There is nothing to read -- treat it as
			// "produced no batch" (re-sling is correct), not an unknown.
			return false, "no session for alias " + rec.SessionID, nil
		}
		return false, "", gerr // transport error -> unknown -> retry
	}

	raw, ok := extractBatch(view.LastOutput)
	if !ok {
		return false, "no GONK_BATCH_START/END fence in session output", nil
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

	// Shape-valid. Apply comment(s) first (deterministic order, independent of
	// the agent's emit order), then labels.
	marker := fmt.Sprintf("<!-- gonk:bead:%s -->", rec.BeadID)
	for _, e := range batch.Effects {
		if e.Kind != effects.KindComment {
			continue
		}
		body := e.Body + "\n\n" + marker
		if _, aerr := d.Apply.CreateIssueNote(ctx, rec.ProjectID, targetIID(e, rec), body); aerr != nil {
			// The gate artifact failed to post; nothing durable applied yet.
			return false, "", aerr // -> unknown -> retry
		}
	}
	for _, e := range batch.Effects {
		if e.Kind != effects.KindLabel {
			continue
		}
		for _, lbl := range e.Add {
			if aerr := d.Apply.AddIssueLabel(ctx, rec.ProjectID, targetIID(e, rec), lbl); aerr != nil {
				d.Log.Warn("sweep: label apply failed (comment already posted)",
					"bead", rec.BeadAnchor, "label", lbl, "err", aerr)
			}
		}
	}
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
