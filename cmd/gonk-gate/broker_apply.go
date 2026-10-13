package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// The sentinels the agent fences its proposed-effects batch with (they match the
// prompt renderTriagePrompt injects). The batch is JSON between them.
const (
	batchStartSentinel = "GONK_BATCH_START"
	batchEndSentinel   = "GONK_BATCH_END"

	// WHY A SESSION WAS RELEASED FOR JUDGEMENT, as the `released_by` log
	// attribute spells it (gonk-w35m). Under Gas City there were five --
	// not_running, peek_fence, transcript_fence, deep_capture_fence and
	// absent -- because a session's liveness had to be inferred from a TUI that
	// never exited and a pane that scrolled. Under the Jobs runtime (gonk-t55,
	// plan D9) the runtime SAYS when a session is finished, so there are two:
	// the session finished (Succeeded or Failed -- the phase and reason are
	// logged beside it), or it no longer exists at all.
	//
	// EVERY judging return carries one, so `sum by (released_by)` accounts for
	// every session gonk judged. These are WIRE VALUES: operator queries are
	// written against the strings, so TestTheReleaseReasonVocabularyIsStable
	// pins them. "absent" keeps its old spelling and meaning.
	releasedByFinished = "finished"
	releasedByAbsent   = "absent"
)

// sessionStatusAttrs are the runtime's own two words about a session, as slog
// attributes: its phase and its reason. Neither can carry a prompt, a comment
// body or a model answer.
func sessionStatusAttrs(st runtime.Status) []any {
	attrs := []any{"session_phase", st.Phase.String()}
	if st.Reason != "" {
		attrs = append(attrs, "session_reason", st.Reason)
	}
	return attrs
}

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
	// effects.FileWrites is the only source of writes: it yields a `file`
	// effect's path and content and nothing from any other kind, so a
	// diagnosis's cited files can never reach a commit from here.
	writes := effects.FileWrites(batch)
	actions := make([]glab.CommitAction, 0, len(writes))
	for _, e := range writes {
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

// isUnreadableTranscript reports whether a session's output carries no content
// at all, which means WE failed to read it rather than that the agent stayed
// silent. A real session always carries at least the entrypoint's own
// "session start" line, so this cannot hide a genuinely silent agent.
func isUnreadableTranscript(text string) bool {
	return strings.TrimSpace(text) == ""
}

// extractBatch pulls the JSON between the LAST GONK_BATCH_START and the
// following GONK_BATCH_END out of a session's output. The agent emits the fence
// as the last thing in its output, so the LAST start guards against the
// sentinels appearing earlier (in the model's own reasoning, or in anything the
// harness echoes before it). ok=false if the fence is absent, out of order, or
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

// applyBrokerBatch is the sweep-side broker: read a FINISHED session's output,
// extract + parse + deterministically SHAPE-VALIDATE the proposed-effects batch,
// and -- only if it passes every gate -- apply it (post the comment with the
// bead marker + add labels) under the bot PAT.
//
// THE BATCH IS THE OUTCOME, NOT THE EXIT CODE (gonk-t55 plan D9; bead gonk-t55
// note (c)). `opencode run` exits 0 after a REJECTED tool call, so a session
// that Succeeded may have done nothing, and one that Failed may still have
// fenced a complete batch before it went. st is taken only to record what the
// runtime said; the judgement is the batch's alone.
//
// Return contract, chosen so the caller can fold the result into gate.Signals:
//   - applied=true  -> a valid batch was accepted and its comment posted. The
//     caller sets ArtifactPresent=true (=> success). For the pipeline agent
//     the landed diagnosis is then ALSO on *recp (PipelineDiagnosis), and the
//     caller's record write is what persists it.
//   - applied=false, violation!="" -> the batch was READ but rejected (no fence,
//     unparseable, out of shape, bad target), or there is no session at all.
//     Nothing was applied; the caller leaves ArtifactPresent=false so the bead
//     falls to the ladder (spend>0 => escalate/needs-human; else retry).
//   - err!=nil -> the output could not be READ or our own shape file is broken:
//     an UNKNOWN, not a rejection. The caller sets ArtifactUnknown (=> retry,
//     never escalate -- uncertainty must not walk a project up the ladder).
//
// It never half-applies a rejected batch: validation is complete before the
// first write. The single comment (shape guarantees exactly one) is applied
// first and MUST succeed; labels follow and a label failure does not un-apply
// the comment (it is already the human-visible triage result).
func applyBrokerBatch(ctx context.Context, d sweepDeps, agent string, recp *beadstore.Record, st runtime.Status) (applied bool, violation string, err error) {
	// recp is a pointer for ONE reason: an applied pipeline diagnosis is
	// recorded on it (applyPipelineBatch), for the caller's record write to
	// persist. Everything else here only reads the record.
	rec := *recp
	if d.Apply == nil && agent != pipelineAgent && agent != pipefixAgent { // the pipeline paths write through d.Pipelines
		return false, "", fmt.Errorf("broker applier not configured")
	}

	// THE WHOLE CONTAINER LOG (plan D1: Output has no size envelope). Under Gas
	// City this was a transcript served through tmux, capped at 4 MiB, with a
	// peek, a deep peek and a pane-width ladder in front of it because none of
	// them could be trusted to hold the fence (gonk-lv3y, gonk-xflt). A pod
	// log has no pane and no scrollback window, so there is one read.
	out, oerr := d.Runtime.Output(ctx, runtime.Handle(rec.SessionID))
	if oerr != nil {
		if errors.Is(oerr, runtime.ErrNotFound) {
			// SAY SO. The session finished a moment ago and is gone now (a
			// Stop, or its TTL): there is no output to judge, which is "no
			// batch", not an unknown.
			d.Log.Warn("session output read FAILED: no such session",
				"bead", rec.BeadAnchor, "session", rec.SessionID)
			return false, "no session " + rec.SessionID, nil
		}
		return false, "", fmt.Errorf("read session output: %w", oerr) // -> unknown -> retry
	}

	// THE FORENSIC POINT (gonk-pop3 item 2). This is the LAST moment the output
	// is certainly readable: the sweep Stops the session once this pass
	// classifies the bead, and the Job takes its pod log with it. Record its
	// shape always, and archive its content when the dev switch is on. Both
	// happen BEFORE any of the refusals below, so the hardest cases to
	// investigate -- empty, no fence -- are the ones that leave evidence.
	text := string(out)
	tdesc := describeTranscript(rec, text)
	tdesc.Archived = archiveTranscript(d.Log, d.TranscriptDir, rec, out)
	tdesc = tdesc.withStatus(st)
	tdesc.log(d.Log)

	// AN EMPTY OUTPUT IS A FAILED READ, NOT A SILENT AGENT (gonk-2tb). Judging
	// it as "no fence" charges the agent for OUR inability to read it -- a
	// burned attempt and a pricier rung with its work unread. The entrypoint's
	// own lifecycle lines guarantee a real session's log is never empty, so
	// this cannot mask a silent one. Unknown -> retry, never escalate.
	//
	// The Gas City "too young to have spoken" grace (gonk-7k73) is gone with
	// the path it guarded: output is read only once the runtime reports the
	// session FINISHED, so there is no younger state to be patient with.
	if isUnreadableTranscript(text) {
		return false, "", fmt.Errorf("output of finished session %q is empty; refusing to judge an unreadable session", rec.SessionID)
	}
	// RETRY FEEDBACK (gonk-nf9i, gonk-zxsx; retry_feedback.go): a refusal
	// below leaves the next attempt a controller-built word about it on the
	// record. fbKinds is nil for an agent that gets none.
	fbKinds := retryFeedbackKinds(agent)
	if !ownChainAgent(agent) {
		// An issue-shaped prompt has no OUTPUT heading for a sentence to point at.
		defer func() { recp.RetryFeedback.What = issueWording(recp.RetryFeedback.What) }()
	}
	raw, ok := extractBatch(text)
	if !ok {
		if fbKinds != nil { // feedback:no-batch
			recordRetryFeedback(recp, effects.ExplainNoBatch())
		}
		return false, "no GONK_BATCH_START/END fence in session output", nil
	}
	batch, perr := effects.ParseBatch(raw)
	if perr != nil {
		if fbKinds != nil { // feedback:parse
			recordRetryFeedback(recp, effects.ExplainParse(raw, perr, fbKinds))
		}
		return false, "unparseable batch: " + perr.Error(), nil
	}
	if ownChainAgent(agent) { // apply:own-chain
		// gonk-tios D8 / gonk-2usw 5.3, 5.4: each agent's own chain, before
		// any issue-shaped gate. An answer that was APPLIED ends the feedback
		// of an earlier refusal: there is nothing left to correct.
		apply := applyPipelineBatch
		if agent == pipefixAgent {
			apply = applyPipefixBatch
		}
		applied, violation, err = apply(ctx, d, recp, batch)
		if applied { // feedback:clear
			recp.RetryFeedback = beadstore.RetryFeedback{}
		}
		return applied, violation, err
	}
	shape, serr := effects.LoadShape(d.PackDir, agent)
	if serr != nil {
		// A missing/broken shape file is OUR misconfiguration, not the agent's
		// fault. Fail closed: never apply an unvalidated batch.
		return false, "", serr
	}
	// THE ISSUE-SHAPED CHAIN (triage, scaffold). Each refusal below leaves the
	// next attempt a word (gonk-hpgy): the gate's name as the code and a FIXED
	// sentence -- never `detail`, which quotes the batch.
	refuse := func(gate, detail string, fb effects.Feedback) (bool, string, error) {
		if fbKinds != nil { // feedback:issue-gate
			fb.Code = gate
			recordRetryFeedback(recp, fb)
		}
		return false, gate + ": " + detail, nil
	}
	gateFeedback := func(gate string) effects.Feedback { return effects.Feedback{What: issueGateFeedback[gate]} }
	if verr := effects.Validate(batch, shape); verr != nil {
		fb := effects.ExplainShape(shape)
		if r := effects.AsRefusal(verr); r != nil {
			fb = effects.ExplainFix(r, batch)
		}
		return refuse(issueGateShape, verr.Error(), fb)
	}
	if terr := effects.ValidateTargets(batch, effects.Targets{Issues: map[int64]bool{rec.IssueIID: true}}); terr != nil {
		return refuse(issueGateTarget, terr.Error(), gateFeedback(issueGateTarget))
	}
	// The path gate. Separate from Validate (which asks "is this the right shape
	// of batch for this agent") because this asks "are these writes allowed at
	// all" -- and unlike a comment, a file effect becomes a commit in someone's
	// repository. A batch must pass both.
	if perr := effects.ValidatePaths(batch); perr != nil {
		return refuse(issueGatePath, perr.Error(), gateFeedback(issueGatePath))
	}
	// THE COMMENT GATE (T-06, closes R-03/R-16): checked before ANY write, the
	// same discipline the path and label gates below use -- a batch with one
	// quick-action or oversize comment produces ZERO GitLab writes, not a
	// comment posted and a second one silently refused. ValidateComments
	// refuses rather than strips a quick-action line: see pkg/effects/comment.go
	// for exactly what the rule catches and what it deliberately does not.
	if cerr := effects.ValidateComments(batch); cerr != nil {
		return refuse(issueGateComment, cerr.Error(), gateFeedback(issueGateComment))
	}
	// The FIFTH GATE (gonk-hsb): not "is this batch well-formed" but "did the
	// session do the work it claims to report". Observing-only until
	// EnforceTrajectory is set -- see checkTrajectory.
	if v := checkTrajectory(ctx, d, rec, agent, batch); v != "" {
		if fbKinds != nil { // feedback:issue-trajectory
			recordRetryFeedback(recp, effects.Feedback{Code: issueGateTrajectory, What: issueGateFeedback[issueGateTrajectory]})
		}
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
				return refuse(issueGateLabel, lerr.Error(), gateFeedback(issueGateLabel))
			}
		}
	}

	// Shape-valid. Apply comment(s) first (deterministic order, independent of
	// the agent's emit order), then labels.
	marker := answerMarker(rec.BeadID)

	// Scaffold's artifact is a merge request, not a comment: the agent proposed
	// .agent/ content and the controller commits it. Done before comments and
	// labels because it is this trigger's gate artifact -- if it fails, nothing
	// else about the run matters.
	if url, serr := applyScaffoldFiles(ctx, d, rec, batch, marker); serr != nil {
		return false, "", serr // -> unknown -> retry; nothing durable applied
	} else if url != "" {
		d.Log.Info("sweep: scaffold merge request ready", "bead", rec.BeadAnchor, "mr", url)
	}
	// THE ANSWER REPLACES THE STATUS NOTE (gonk-yrs, gonk-l900 item 4). gonk's
	// one status note on the issue -- "triaging now", "retrying", "cannot
	// answer yet" -- is EDITED into the answer rather than left above it, so
	// the thread ends with exactly one gonk note: the answer.
	//
	// The answer KEEPS THE STATUS MARKER beside the bead marker. That is what
	// lets a later lifecycle event (a re-triage of the same issue) find the
	// same note, and what makes this apply idempotent: a pass that applies the
	// batch again -- the outcome POST failed, the record write was stale --
	// finds the answer by the marker and edits it to the same text instead of
	// posting a second copy.
	//
	// Only the FIRST comment aimed at the bead's own issue claims the note;
	// anything further is posted normally.
	claimed := false
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
		if !claimed && iid == rec.IssueIID {
			claimed = true
			body += "\n\n" + statusMarker(rec.BeadID)
			if replacedStatusNote(ctx, d, rec, body) {
				continue
			}
		}
		if _, aerr := d.Apply.CreateIssueNote(ctx, rec.ProjectID, iid, body); aerr != nil {
			// The gate artifact failed to post; nothing durable applied yet.
			return false, "", aerr // -> unknown -> retry
		}
	}
	// NO COMMENT FOR THIS ISSUE: the status note still says "triaging now", so
	// it becomes a short result summary instead (gonk-l900 item 4). Scaffold
	// has no issue thread and is skipped.
	if !claimed && rec.IssueIID != 0 && artifactKindForTrigger[rec.Trigger] == "comment" {
		// The summary is a FINAL answer like any other: it carries the answer
		// marker, so a re-apply replaces it in place and the next round
		// starts a fresh note (gonk-nrbx).
		body := resultSummaryBody(batch.EffectiveVerdict(), d.BotUsername) +
			"\n\n" + marker + "\n\n" + statusMarker(rec.BeadID)
		if !replacedStatusNote(ctx, d, rec, body) {
			if _, aerr := d.Apply.CreateIssueNote(ctx, rec.ProjectID, rec.IssueIID, body); aerr != nil {
				d.Log.Warn("sweep: result summary note failed", "bead", rec.BeadAnchor, "err", aerr)
			}
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
	// An answer that was APPLIED ends the feedback of an earlier refusal.
	recp.RetryFeedback = beadstore.RetryFeedback{} // feedback:issue-clear
	return true, "", nil
}

// replacedStatusNote edits gonk's status note on the bead's issue into the
// answer body. It reports true when the issue now carries the answer in that
// note -- edited, or already identical -- and false when the caller must post
// the answer as a new note: there is no status note, the thread could not be
// read, or the edit failed. The last two are logged at WARN; a failed read or
// edit must not cost the reporter the answer, and the price is a possible
// second gonk note (findStatusNote prefers the newest, so later lifecycle
// events edit the answer, not the stale note above it).
func replacedStatusNote(ctx context.Context, d sweepDeps, rec beadstore.Record, body string) bool {
	st, err := findStatusNote(ctx, d, rec.ProjectID, rec.IssueIID, rec.BeadID)
	if err != nil {
		d.Log.Warn("sweep: could not read the issue's notes to find gonk's status note; posting the answer as a new note",
			"bead", rec.BeadAnchor, "err", err)
		return false
	}
	if st == nil {
		return false
	}
	if st.Body == body {
		return true // already the answer: a repeated apply writes nothing
	}
	// A DIFFERENT final answer is an earlier round's (gonk-nrbx): this round's
	// placeholder never landed. Never overwrite it -- retire its status marker
	// and let the caller post this round's answer as a new note.
	if isFinalAnswer(st, rec.BeadID) {
		retireStatusNote(ctx, d, rec.ProjectID, rec.IssueIID, st)
		return false
	}
	if _, err := d.Apply.UpdateIssueNote(ctx, rec.ProjectID, rec.IssueIID, st.ID, body); err != nil {
		d.Log.Warn("sweep: could not edit gonk's status note into the answer; posting the answer as a new note",
			"bead", rec.BeadAnchor, "note", st.ID, "err", err)
		return false
	}
	return true
}

// targetIID resolves an effect's target: an explicit, context-validated iid, or
// the bead's primary issue when the effect names none (TargetIID==0).
func targetIID(e effects.Effect, rec beadstore.Record) int64 {
	if e.TargetIID != 0 {
		return e.TargetIID
	}
	return rec.IssueIID
}
