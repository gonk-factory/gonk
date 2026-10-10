package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
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

// answerMarker identifies a note as gonk's FINAL ANSWER to a bead. A status
// note that carries it is no longer a placeholder: it is a finished round.
func answerMarker(beadID string) string {
	return fmt.Sprintf("<!-- gonk:bead:%s -->", beadID)
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
	// WHEN, AS AN ABSOLUTE UTC TIME (gonk-l900 item 3). Not "in an hour" -- a
	// note is read long after it is written -- and not RFC 3339's trailing "Z",
	// which a reporter cannot be expected to read as "UTC".
	if !retryAfter.IsZero() {
		fmt.Fprintf(&b, "I will look at this again after %s.\n\n", plainUTC(retryAfter))
	} else {
		b.WriteString("I will try again automatically.\n\n")
	}
	b.WriteString("This note is updated in place while I wait, and replaced by my answer when I have one.")
	if f := mentionFooter(botUsername); f != "" {
		b.WriteString("\n\n" + f)
	}
	return b.String()
}

// plainUTC renders an instant as an absolute UTC time a human reads without a
// format reference: "2026-11-01 00:00:00 UTC".
func plainUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
}

// deferReasonText translates the meter's machine reason into something a
// reporter can act on. An unrecognised reason yields NO sentence rather than a
// guess -- the raw reason still travels in the detail line.
//
// EVERY DEFER REASON pkg/rung.Decide CAN RETURN IS COVERED (gonk-l900 item 3).
// Before that bead this matched "budget-exhausted" and "over-budget" -- codes
// meter has never emitted -- so the three money defers it does emit
// (spend-data-stale, monthly-tokens-exhausted, monthly-cost-exhausted) got no
// sentence at all. TestEveryDeferReasonMeterEmitsIsExplained pins the set.
func deferReasonText(reason string) string {
	switch reason {
	case meterapi.ReasonKeyMissing:
		return "My budget key for this project is not provisioned yet, so I cannot spend anything on it."
	case meterapi.ReasonQuietHours:
		return "This project is in its configured quiet hours, so I am holding off until they end."
	case meterapi.ReasonSpendStale:
		return "My figures for how much of this project's budget has been used are out of date, " +
			"so I am waiting until they are current before spending more."
	case meterapi.ReasonMonthlyTokensExhausted:
		return "This project has used its token allowance for the current budget period. " +
			"The allowance resets at the time below."
	case meterapi.ReasonMonthlyCostExhausted:
		return "This project has used its spending allowance for the current budget period. " +
			"The allowance resets at the time below."
	default:
		return ""
	}
}

// deniedBody is what gonk says when it has STOPPED and a human is needed
// (gonk-wnz6). A dispatch-time deny used to write needs-human and tell nobody,
// so the one outcome that explicitly requires a person was the one the person
// never heard about.
//
// It is the same status note the deferred path edits (same marker), so a
// defer->deny or a repeated deny leaves ONE current note whose text is
// whatever is true now.
func deniedBody(reason, detail, botUsername string) string {
	var b strings.Builder
	b.WriteString("I have stopped working on this and need a human to look at it.\n\n")
	b.WriteString(denyReasonText(reason))
	b.WriteString("\n\n")
	if d := strings.TrimSpace(detail); d != "" {
		b.WriteString("Details: " + d + "\n\n")
	}
	b.WriteString("I will not retry this on my own. This note is updated in place if my status changes.")
	if f := mentionFooter(botUsername); f != "" {
		b.WriteString("\n\n" + f)
	}
	return b.String()
}

// denyReasonText translates the meter's machine deny reason into a sentence. An
// unrecognised or empty reason is printed VERBATIM rather than guessed at: a
// human told the exact code can look it up, one told a wrong sentence cannot.
func denyReasonText(reason string) string {
	switch reason {
	case meterapi.ReasonLadderExhausted:
		return "Every model rung I am allowed to use has been tried and none produced an answer that passed my checks."
	case meterapi.ReasonInfraRetriesExhausted:
		return "I kept hitting infrastructure failures (not bad answers) and have used up my retries."
	case meterapi.ReasonActionNotAllowed:
		return "This project's configuration does not allow me to take this action."
	case meterapi.ReasonNotRegistered:
		return "This project is not registered with me."
	case meterapi.ReasonDisabled:
		return "I am disabled for this project."
	case meterapi.ReasonInvalidConfig:
		return "This project's configuration is invalid, so I cannot act on it."
	case meterapi.ReasonCloudNotAllowed:
		return "The next step would use a paid cloud model and no cloud allowance is granted for this project."
	case meterapi.ReasonPerTaskTokensExhausted:
		return "This task has used its per-task token allowance."
	case meterapi.ReasonMonthlyTokensExhausted:
		return "This project has used its monthly token allowance."
	case meterapi.ReasonMonthlyCostExhausted:
		return "This project has used its monthly cost allowance."
	case "":
		return "The budget gate refused this work and gave no reason code."
	default:
		return "The budget gate refused this work with reason code: " + reason
	}
}

// ---- THE ONE STATUS NOTE'S LIFECYCLE (gonk-l900). ---------------------------
//
// One gonk note per issue, found by statusMarker(beadID) and EDITED through
// the bead's life, never appended to:
//
//	dispatch RUN          -> runningBody   ("I am triaging this issue now")
//	sweep re-sling        -> reslingBody   (why the attempt failed; retrying)
//	dispatch DEFER        -> deferredBody  (why, and when, as an absolute UTC time)
//	dispatch DENY         -> deniedBody    (a human is needed)
//	sweep applies a batch -> the triage comment ITSELF (applyBrokerBatch), still
//	                         carrying the status marker so a later lifecycle
//	                         event finds the same note
//
// The bead id IS the bead anchor (gonk:{project}:issue:{iid}; pkg/intake sends
// it as bead_id at Gate 1 and the sweep's re-sling passes the record's back),
// so the marker is stable across attempts, re-slings and a later re-triage of
// the same issue.
//
// Every write here is BEST EFFORT: a GitLab failure is logged at WARN and never
// changes an outcome, an exit code or a record. Each is made only AFTER the
// record write it reports has landed.

// statusNoteTimeout bounds one status-note write (a notes list plus one create
// or edit). The note is courtesy; it must never be what eats a dispatch's start
// budget or holds a sweep pass.
const statusNoteTimeout = 15 * time.Second

// errCannotFindStatusNote: there is no way to tell gonk's note from anyone
// else's (no reader, or no bot username), so nothing may be written -- a write
// that cannot find the existing note would post a second one.
var errCannotFindStatusNote = errors.New("cannot identify gonk's own notes: no GitLab reader or no bot username configured")

// findStatusNote returns gonk's existing status note on this issue: (nil, nil)
// when there is none, and an error when the thread could not be read -- which
// callers must NOT treat as "none", or a transient list failure posts a second
// note.
//
// Authorship is checked against the bot username and System notes are skipped,
// for the same reason the batch gate does it: a human can quote a marker, and a
// quoted marker must never let somebody else's comment be edited by the bot.
//
// If more than one bot note carries the marker (only reachable when an edit
// failed and applyBrokerBatch fell back to posting the answer fresh), the
// NEWEST wins: that is the answer, and a later lifecycle event must edit it
// rather than resurrect the stale placeholder above it.
func findStatusNote(ctx context.Context, d sweepDeps, projectID, issueIID int64, beadID string) (*glab.Note, error) {
	if d.GL == nil || d.BotUsername == "" {
		return nil, errCannotFindStatusNote
	}
	notes, err := d.GL.ListIssueNotes(ctx, projectID, issueIID)
	if err != nil {
		return nil, err
	}
	marker := statusMarker(beadID)
	var found *glab.Note
	for i := range notes {
		n := notes[i]
		if n.System || !strings.EqualFold(n.Author.Username, d.BotUsername) {
			continue
		}
		if strings.Contains(n.Body, marker) {
			found = &n
		}
	}
	return found, nil
}

// upsertStatusNote posts gonk's status comment, or EDITS the one already there.
//
// Editing rather than appending is the whole point: a bead that defers five
// times must leave one current note, not five stale apologies, so the thread
// always shows where things actually stand.
func upsertStatusNote(ctx context.Context, d sweepDeps, projectID, issueIID int64, beadID, body string) {
	upsertStatusNoteWith(ctx, d, projectID, issueIID, beadID, func(*glab.Note) string { return body })
}

// upsertStatusNoteWith is upsertStatusNote with a body that may depend on the
// note being replaced (nil when there is none) -- the RUN note carries forward
// the re-sling note's account of the previous attempt.
//
// A thread that cannot be READ is not written at all: posting blind is how a
// second gonk note appears, and a missed status update is repaired by the next
// lifecycle event while a duplicate note is permanent.
func upsertStatusNoteWith(ctx context.Context, d sweepDeps, projectID, issueIID int64, beadID string, build func(existing *glab.Note) string) {
	if d.Apply == nil {
		return
	}
	existing, err := findStatusNote(ctx, d, projectID, issueIID, beadID)
	if err != nil {
		d.Log.Warn("status note not written: could not read the issue's notes to find gonk's existing one, "+
			"and posting without it could leave a second note",
			"project_id", projectID, "issue", issueIID, "bead", beadID, "err", err)
		return
	}
	// A NEW ROUND (gonk-nrbx). If the live status note already holds a FINAL
	// answer (it carries the answer marker), an earlier round is finished and
	// this write belongs to a new one: retire that note (status marker off, text
	// untouched) and start a fresh note. Within one round the live note is never
	// final -- it is the placeholder, edited as before (gonk-l900) -- so a
	// repeated or restarted pass of the SAME round edits and never creates.
	if existing != nil && isFinalAnswer(existing, beadID) {
		retireStatusNote(ctx, d, projectID, issueIID, existing)
		existing = nil
	}
	full := build(existing) + "\n\n" + statusMarker(beadID)
	if existing != nil {
		if existing.Body == full {
			return // already says exactly this: a repeated pass is a no-op, not a write
		}
		if _, err := d.Apply.UpdateIssueNote(ctx, projectID, issueIID, existing.ID, full); err != nil {
			d.Log.Warn("status note update failed", "project_id", projectID, "issue", issueIID, "err", err)
		}
		return
	}
	if _, err := d.Apply.CreateIssueNote(ctx, projectID, issueIID, full); err != nil {
		d.Log.Warn("status note create failed", "project_id", projectID, "issue", issueIID, "err", err)
	}
}

// isFinalAnswer reports whether a status note is a finished round's answer
// rather than the live placeholder. The round rule rests on this: it is read
// from the thread itself, so it survives a restart and needs no extra record
// field.
func isFinalAnswer(n *glab.Note, beadID string) bool {
	return n != nil && strings.Contains(n.Body, answerMarker(beadID))
}

// retireStatusNote strips the status marker from an earlier round's answer so
// the new round's note is the single LIVE status note. The answer's text is
// otherwise unchanged. Best effort: on failure the new note is still posted,
// and findStatusNote prefers the newest, so the new note is the one found.
func retireStatusNote(ctx context.Context, d sweepDeps, projectID, issueIID int64, n *glab.Note) {
	if d.Apply == nil || n == nil {
		return
	}
	// Marker is appended as "\n\n<marker>"; remove that form first so the
	// answer text is byte-identical to what was posted minus the marker.
	body := n.Body
	for _, m := range []string{"\n\n" + statusMarkerOf(n.Body), statusMarkerOf(n.Body)} {
		if m != "" && strings.Contains(body, m) {
			body = strings.Replace(body, m, "", 1)
			break
		}
	}
	if body == n.Body {
		return
	}
	if _, err := d.Apply.UpdateIssueNote(ctx, projectID, issueIID, n.ID, body); err != nil {
		d.Log.Warn("could not retire the previous round's status marker; the new note is posted anyway",
			"project_id", projectID, "issue", issueIID, "note", n.ID, "err", err)
	}
}

// statusMarkerOf extracts the status marker comment present in a body ("" if none).
func statusMarkerOf(body string) string {
	const open = "<!-- gonk:status:"
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "-->")
	if j < 0 {
		return ""
	}
	return body[i : i+j+3]
}

// rungText names a model rung in the note. The operator rung catalog has no
// display name (opercfg.RungSpec.Name IS the id), so the id is the human name.
func rungText(rung string) string {
	if strings.TrimSpace(rung) == "" {
		return "the configured model"
	}
	return "model `" + rung + "`"
}

// prevOpen/prevClose fence the re-sling note's account of the previous
// attempt, so the RUN note that replaces it moments later can carry that
// sentence forward rather than erase it. The attempt number is IN the marker:
// only an account of attempt N-1 is carried into attempt N's note.
func prevOpen(attempt int) string { return fmt.Sprintf("<!-- gonk:prev:%d -->", attempt) }

const prevClose = "<!-- /gonk:prev -->"

// carriedPrevious returns the fenced previous-attempt account from an existing
// status note, if it is about attempt prevAttempt; "" otherwise.
func carriedPrevious(existing *glab.Note, prevAttempt int) string {
	if existing == nil || prevAttempt < 1 {
		return ""
	}
	open := prevOpen(prevAttempt)
	i := strings.Index(existing.Body, open)
	if i < 0 {
		return ""
	}
	rest := existing.Body[i:]
	j := strings.Index(rest, prevClose)
	if j < 0 {
		return ""
	}
	return rest[:j+len(prevClose)]
}

// runningBody is the RUN note (gonk-l900 item 1): posted the moment dispatch
// has reserved the attempt, before the session starts, so the reporter sees
// gonk working within seconds instead of an empty thread for minutes.
// previous is a carried, fenced account of the attempt before (or "").
func runningBody(attempt int, rung, previous, botUsername string) string {
	var b strings.Builder
	if previous != "" {
		b.WriteString(previous + "\n\n")
	}
	if attempt < 1 {
		attempt = 1
	}
	fmt.Fprintf(&b, "I am triaging this issue now (attempt %d, using %s).\n\n", attempt, rungText(rung))
	b.WriteString("This note is updated in place while I work, and replaced by my answer when I have one.")
	if f := mentionFooter(botUsername); f != "" {
		b.WriteString("\n\n" + f)
	}
	return b.String()
}

// reslingBody is the re-sling note (gonk-l900 item 2): the attempt that just
// ended did not produce an answer, why (in plain words), and that a retry is
// starting. next is meter's advisory Next ("escalate" or "retry") and nextRung
// its advisory NextRung ("" when meter named none).
func reslingBody(attempt int, rung, outcome, why, next, nextRung, botUsername string) string {
	var b strings.Builder
	b.WriteString(prevOpen(attempt) + "\n")
	if outcome == meterapi.OutcomeInfraFailed {
		fmt.Fprintf(&b, "Attempt %d (%s) did not finish because of an infrastructure problem, not a bad answer: %s.",
			attempt, rungText(rung), why)
	} else {
		fmt.Fprintf(&b, "Attempt %d (%s) did not produce a valid answer: %s.", attempt, rungText(rung), why)
	}
	b.WriteString("\n" + prevClose + "\n\n")
	switch {
	case next == "escalate" && nextRung != "":
		fmt.Fprintf(&b, "I am retrying now with a stronger model (attempt %d, using %s).", attempt+1, rungText(nextRung))
	case next == "escalate":
		fmt.Fprintf(&b, "I am retrying now with a stronger model if one is available (attempt %d).", attempt+1)
	default:
		fmt.Fprintf(&b, "I am retrying now with the same model (attempt %d).", attempt+1)
	}
	b.WriteString("\n\nThis note is updated in place while I work, and replaced by my answer when I have one.")
	if f := mentionFooter(botUsername); f != "" {
		b.WriteString("\n\n" + f)
	}
	return b.String()
}

// attemptFailureText turns the sweep's machine account of a failed attempt --
// the batch violation, or the infra-failed cause -- into one plain clause. It
// never includes the raw violation: those carry parser and validator
// internals (JSON offsets, field paths) that mean nothing to a reporter and
// are already in the sweep's own log line.
func attemptFailureText(outcome, violation, infraCause string) string {
	if outcome == meterapi.OutcomeInfraFailed {
		switch {
		case strings.HasPrefix(infraCause, "session_deadline_exceeded"), strings.HasPrefix(infraCause, "reservation_expired"):
			return "it ran out of the time allowed for one attempt"
		case strings.HasPrefix(infraCause, "artifact_unknown"):
			return "I could not read its result"
		case strings.HasPrefix(infraCause, "spend_stale"):
			return "I could not confirm in time that the model had answered"
		case strings.HasPrefix(infraCause, "no_model_tokens"):
			return "the model never answered"
		default:
			return "something outside the model went wrong"
		}
	}
	switch {
	case strings.HasPrefix(violation, "no GONK_BATCH_START/END fence"):
		return "its output did not contain a result in the format I require"
	case strings.HasPrefix(violation, "unparseable batch"):
		return "its result was malformed and could not be read"
	case strings.HasPrefix(violation, "shape:"):
		return "its result did not have the required parts"
	case strings.HasPrefix(violation, "target:"):
		return "its result tried to act on something other than this issue"
	case strings.HasPrefix(violation, "path:"):
		return "its result tried to change files it is not allowed to"
	case strings.HasPrefix(violation, "comment:"):
		return "its comment broke a posting rule (for example, a line GitLab would run as a command)"
	case strings.HasPrefix(violation, "label:"):
		return "it proposed a label it is not allowed to use"
	case strings.HasPrefix(violation, "trajectory:"):
		return "its answer was not backed by the investigation it reported"
	case strings.HasPrefix(violation, "no session"):
		return "its session ended without leaving any output"
	default:
		return "it did not produce an answer I could accept"
	}
}

// resultSummaryBody is what the status note becomes when an applied batch
// carried no comment for this issue (gonk-l900 item 4): the verdict in plain
// words. The triage shape requires one comment, so this is a backstop, not a
// path a valid triage batch takes.
func resultSummaryBody(v effects.Verdict, botUsername string) string {
	var b strings.Builder
	b.WriteString("I have finished triaging this issue. ")
	switch v {
	case effects.VerdictCodeChange:
		b.WriteString("My conclusion: this needs a code change.")
	case effects.VerdictClose:
		b.WriteString("My conclusion: this issue can be closed.")
	default:
		b.WriteString("My conclusion: no code change is needed.")
	}
	b.WriteString(" The labels on this issue record the result.")
	if f := mentionFooter(botUsername); f != "" {
		b.WriteString("\n\n" + f)
	}
	return b.String()
}
