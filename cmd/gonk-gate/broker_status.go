package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// CANNED, ZERO-TOKEN TEXT (gonk-yrs). None of this costs a model call: it is
// deterministic broker output, so it needs no rung, no budget and no shape gate.
// What it does need is the same discipline as any other write -- it is posted
// under the bot PAT into a human's issue thread, so it says only things the
// broker actually knows.

// statusMarker identifies a note as gonk's CURRENT-STATUS comment rather than an
// answer. It is deliberately distinct from the bead marker: the bead marker means
// "this is gonk's answer to this bead" and the gate reads it, while this one
// means "this is a placeholder that should be edited or replaced".
func statusMarker(beadID string) string {
	return fmt.Sprintf("<!-- gonk:status:%s -->", beadID)
}

// mentionFooter is appended to gonk's answers so a reporter can tell how to
// continue the conversation.
//
// THIS EXISTS BECAUSE THE AFFORDANCE WAS INVISIBLE. gonk answers an issue and
// then goes silent, and a reporter who replies in the thread -- the obvious
// thing to do -- gets nothing, because intake only dispatches on an actual
// at-mention (pkg/intake/dispatch.go: Reason=no_mention). The machinery worked
// perfectly and was undiscoverable, which for a conversational surface is the
// same as broken.
func mentionFooter(botUsername string) string {
	if botUsername == "" {
		return ""
	}
	return fmt.Sprintf("_Reply with `@%s` to send this back to me -- I only see comments that mention me._", botUsername)
}

// deferredBody is what gonk says when it KNOWS it must act and cannot yet.
//
// Silence is the wrong answer here: from the reporter's side an unmetered,
// budget-exhausted or quiet-hours bead looks exactly like being ignored. Saying
// so costs nothing and is honest about the fact that the work is queued rather
// than lost.
func deferredBody(reason, detail string, retryAfter time.Time, botUsername string) string {
	var b strings.Builder
	b.WriteString("I picked this up but cannot answer yet.\n\n")
	if human := deferReasonText(reason); human != "" {
		b.WriteString(human)
		b.WriteString("\n\n")
	}
	if d := strings.TrimSpace(detail); d != "" {
		b.WriteString("Details: " + d + "\n\n")
	}
	if !retryAfter.IsZero() {
		fmt.Fprintf(&b, "I will try again after %s.\n\n", retryAfter.UTC().Format(time.RFC3339))
	} else {
		b.WriteString("I will try again automatically.\n\n")
	}
	b.WriteString("This note is updated in place while I wait, and replaced by my answer when I have one.")
	if f := mentionFooter(botUsername); f != "" {
		b.WriteString("\n\n" + f)
	}
	return b.String()
}

// deferReasonText translates the meter's machine reason into something a
// reporter can act on. An unrecognised reason yields NO sentence rather than a
// guess -- the raw reason still travels in the detail line.
func deferReasonText(reason string) string {
	switch reason {
	case "virtual-key-missing":
		return "My budget key for this project is not provisioned yet, so I cannot spend anything on it."
	case "quiet-hours":
		return "This project is in its configured quiet hours, so I am holding off until they end."
	case "budget-exhausted", "over-budget":
		return "This project has reached its configured budget for now."
	case "":
		return ""
	default:
		return ""
	}
}

// findStatusNote returns gonk's existing status note on this issue, or nil.
//
// Authorship is checked against the bot username and System notes are skipped,
// for the same reason the batch gate does it: a human can quote a marker, and a
// quoted marker must never let somebody else's comment be edited by the bot.
func findStatusNote(ctx context.Context, d sweepDeps, projectID, issueIID int64, beadID string) *glab.Note {
	if d.GL == nil || d.BotUsername == "" {
		return nil
	}
	notes, err := d.GL.ListIssueNotes(ctx, projectID, issueIID)
	if err != nil {
		return nil
	}
	marker := statusMarker(beadID)
	for i := range notes {
		n := notes[i]
		if n.System || !strings.EqualFold(n.Author.Username, d.BotUsername) {
			continue
		}
		if strings.Contains(n.Body, marker) {
			return &n
		}
	}
	return nil
}

// upsertStatusNote posts gonk's status comment, or EDITS the one already there.
//
// Editing rather than appending is the whole point: a bead that defers five
// times must leave one current note, not five stale apologies, so the thread
// always shows where things actually stand.
func upsertStatusNote(ctx context.Context, d sweepDeps, projectID, issueIID int64, beadID, body string) {
	if d.Apply == nil {
		return
	}
	full := body + "\n\n" + statusMarker(beadID)
	if existing := findStatusNote(ctx, d, projectID, issueIID, beadID); existing != nil {
		if _, err := d.Apply.UpdateIssueNote(ctx, projectID, issueIID, existing.ID, full); err != nil {
			d.Log.Warn("status note update failed", "project_id", projectID, "issue", issueIID, "err", err)
		}
		return
	}
	if _, err := d.Apply.CreateIssueNote(ctx, projectID, issueIID, full); err != nil {
		d.Log.Warn("status note create failed", "project_id", projectID, "issue", issueIID, "err", err)
	}
}
