package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// issueReader is the sliver of pkg/glab the broker's inject needs: read the
// issue the controller is about to triage. Because the agent pod holds NO forge
// credentials (the broker's whole point), the agent cannot fetch the issue
// itself -- the CONTROLLER fetches it here (with its own PAT) and splices the
// context into the prompt. Satisfied by *glab.Client.
type issueReader interface {
	GetIssue(ctx context.Context, projectID, issueIID int64) (*glab.Issue, error)
}

// maxIssueBodyBytes caps the issue description spliced into the prompt. An issue
// body is untrusted, caller-controlled input of unbounded size; it must not be
// able to blow the model's context or the session-create request body. Over-cap
// bodies are truncated with an explicit marker (§11 OQ5).
const maxIssueBodyBytes = 8 << 10 // 8 KiB

// agentForTrigger maps a trigger to the broker AGENT that handles it. A trigger
// in this set is dispatched via the v2 broker: dispatch creates the agent
// session DIRECTLY (POST /v0/city/{city}/sessions), correlates it by a unique
// alias, and injects the rendered prompt as the session's initial message --
// instead of pouring a formula order. The agent produces a proposed-effects
// batch and posts nothing; gonk-sweep validates the batch's shape and applies
// it under the controller's own bot PAT. The pod holds no forge creds.
//
// Triage is the first (and, for this slice, only) ported trigger. scaffold and
// mention still pour their formulas in runDispatch until they are ported
// (Phase 6). An entry here takes precedence over orderForTrigger.
var agentForTrigger = map[string]string{
	"issue-triage": "triage",
}

// brokerSessionAlias is the correlation key stamped on the created session and
// recorded on the bead (Record.SessionID). gonk-sweep reads the session back by
// this alias via GetSessionOutput.
//
// It is deterministic and re-sling-stable EXCEPT for the attempt suffix, which
// is deliberate: gascity rejects a create whose alias is already taken, so a
// re-sling (same project+issue, next attempt) must get a fresh alias rather than
// collide with the prior attempt's still-present session. session.ValidateAlias
// forbids colons (bead anchors use them) and caps length at 64; this form uses
// only [a-z0-9.] and stays well under the cap.
func brokerSessionAlias(projectID, issueIID int64, attempt int) string {
	return fmt.Sprintf("gonk.triage.p%d.i%d.a%d", projectID, issueIID, attempt)
}

// renderTriagePrompt builds the session's initial message.
//
// IT CARRIES NO <!-- gonk:model / gonk:meta --> MARKER LINES, and must not.
// Those were read by gonk-agent-entrypoint out of its --prompt ARGUMENT, and on
// this backend there is no such argument: the k8s provider never composes
// PromptSuffix onto the launch command, so the prompt arrives by submit LONG
// AFTER opencode has booted and chosen its model from static pod env. A marker
// delivered that way can never be consumed -- and, far worse, "<!--" contains a
// "!", which puts opencode's composer into shell mode (see
// sanitizeForKeystrokeDelivery). They were pure harm here.
//
// The per-bead attribution those markers carried is therefore NOT reaching the
// pod on this path. gonk-agent-entrypoint already says so out loud ("spend rows
// for this session will NOT carry per-bead attribution") rather than failing;
// closing that gap needs a real out-of-band channel, tracked separately.
//
// The body instructs the agent to emit a proposed-effects batch and to call NO
// external API: with the broker, the agent has no forge credentials, so it must
// not (and cannot) post anything itself.
//
// The issue's title/body/labels are injected as issueContext (built by
// buildIssueContext from a controller-side fetch) because the pod cannot fetch
// them itself. issueContext is empty only when the fetch was unavailable or
// failed -- a degraded, reference-only prompt. The exact emit wording may be
// tightened after C2's first live run confirms the fenced batch survives the
// GetSession(peek) read (C5).
func renderTriagePrompt(project string, issueIID int64, issueContext string) string {
	context := issueContext
	if strings.TrimSpace(context) == "" {
		context = "(issue context unavailable -- triage from the issue reference alone)"
	}
	return fmt.Sprintf(`Triage GitLab issue #%d in project `+"`%s`"+`. Here is the issue, already
fetched for you -- do NOT fetch anything yourself:

%s

Decide the labels (each prefixed `+"`gonk::`"+`) and one short triage comment: a
brief analysis of what the issue asks for, with anything genuinely ambiguous
phrased as a direct question to the reporter.

Do NOT post anything yourself. Do NOT run glab, git, bd, or any external API --
you hold no credentials and any such call will fail. Instead, emit your decision
as a single proposed-effects batch as the LAST thing in your output, fenced
EXACTLY like this:

GONK_BATCH_START
{"effects":[{"kind":"comment","body":"<your comment>"},{"kind":"label","add":["gonk::<label>"]}]}
GONK_BATCH_END

Emit exactly one comment effect and zero or more label effects. Nothing after
GONK_BATCH_END.`, issueIID, project, context)
}

// sanitizeForKeystrokeDelivery makes a prompt safe to TYPE into opencode's TUI.
//
// The prompt is delivered by the supervisor as tmux `send-keys -l`, i.e. as
// KEYSTROKES into a running terminal UI -- and opencode's composer treats "!"
// as its shell-mode trigger. PROVEN LIVE 2026-08-01: sending
//
//	Hello there, see issue !42 and reply with exactly the word GOLF
//
// rendered as "$ Hello there, see issue 42 ..." (bang eaten, shell prompt shown)
// and produced "/bin/sh: 1: Hello: not found". The model never saw the message
// AT ALL. Every wedged triage session was this: the whole prompt executed as a
// shell command instead of being asked.
//
// THIS IS ALSO AN INJECTION BOUNDARY, which is why it is a hard strip rather
// than a tidy-up of gonk's own wording. The prompt embeds an UNTRUSTED GitLab
// issue title and body; a body containing a "!" followed by shell syntax would
// otherwise run in the agent pod. Sanitizing only the parts gonk writes would
// leave the half an attacker controls.
//
// Stripping is lossy -- exclamation marks and GitLab "!123" MR references do
// not survive -- and that is the right trade against executing issue text.
// The durable fix is to stop delivering prompts as keystrokes at all.
func sanitizeForKeystrokeDelivery(prompt string) string {
	return strings.ReplaceAll(prompt, "!", "")
}

// buildIssueContext fetches the issue and renders its title/labels/body into the
// prompt-embeddable block, with the body size-capped. It is best-effort: on any
// error (no reader configured, forge unreachable, issue gone) it returns "" and
// a non-nil err for the caller to log -- dispatch proceeds with a reference-only
// prompt rather than failing the whole run over a context fetch.
func buildIssueContext(ctx context.Context, r issueReader, projectID, issueIID int64) (string, error) {
	if r == nil {
		return "", fmt.Errorf("no issue reader configured")
	}
	iss, err := r.GetIssue(ctx, projectID, issueIID)
	if err != nil {
		return "", err
	}
	body := capBody(iss.Description, maxIssueBodyBytes)
	labels := "(none)"
	if len(iss.Labels) > 0 {
		labels = strings.Join(iss.Labels, ", ")
	}
	return fmt.Sprintf("Title: %s\nState: %s\nCurrent labels: %s\n\n%s",
		iss.Title, iss.State, labels, body), nil
}

// capBody truncates an untrusted issue body to at most max bytes, appending a
// visible marker so the agent (and a human reading the transcript) knows the
// body was cut. Truncation backs up to a rune boundary so the block stays valid
// UTF-8 (a multibyte rune may straddle max).
func capBody(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1] // a partial/continuation byte at the cut point
			continue
		}
		break
	}
	return fmt.Sprintf("%s\n\n[... issue body truncated by gonk: over %d bytes ...]", cut, max)
}

// runBrokerDispatch is the v2 broker's inject step: create the agent session,
// correlate by alias, deliver the rendered prompt. It mirrors the formula
// pour's post-decision bookkeeping (record the reservation + running state on
// the bead) but records the SESSION ALIAS as the correlation key sweep follows.
//
// Exit codes match runDispatch's contract: 0 acted, 1 infra, 2 misconfig.
func runBrokerDispatch(ctx context.Context, d dispatchDeps, agent string, dec meterapi.DecideResponse, base beadstore.Record) int {
	a := d.Args
	// The meter's attribution metadata is deliberately NOT put in the prompt any
	// more (see renderTriagePrompt): it rode a marker line the pod cannot read on
	// this delivery path. It is still marshalled and logged so the value that
	// SHOULD be reaching the pod is visible at the point it is lost, rather than
	// quietly disappearing from the code.
	if md, err := json.Marshal(dec.Metadata); err != nil {
		d.Log.Error("could not marshal meter metadata", "err", err, "bead", a.BeadAnchor)
		return 1
	} else {
		d.Log.Debug("attribution metadata is not deliverable to the pod on the submit path",
			"bead", a.BeadAnchor, "metadata", string(md))
	}
	alias := brokerSessionAlias(a.ProjectID, a.IssueIID, dec.Attempt)

	// Fetch the issue context controller-side (the pod has no forge creds).
	// Best-effort: a fetch failure degrades to a reference-only prompt rather
	// than failing the run -- a re-sling can try again, and the agent still has
	// the issue reference.
	issueContext, err := buildIssueContext(ctx, d.Forge, a.ProjectID, a.IssueIID)
	if err != nil {
		d.Log.Warn("triage context fetch failed; injecting reference-only prompt",
			"err", err, "bead", a.BeadAnchor, "issue", a.IssueIID)
	}
	prompt := sanitizeForKeystrokeDelivery(renderTriagePrompt(a.Project, a.IssueIID, issueContext))

	// NOTE the create carries NO Message. It used to, and that is exactly the
	// bug: `message` becomes template_overrides.initial_message, which Gas City
	// puts on runtime.Config.PromptSuffix for the PROVIDER to append to the
	// launch command -- and internal/runtime/k8s never does (tmux/acp/herdr/
	// t3bridge do). Every gonk session is a k8s pod, so the prompt was silently
	// dropped and the agent sat at opencode's idle splash forever, never
	// finishing and so never being swept. The prompt is delivered by the submit
	// below instead. Do NOT "restore" Message here once upstream (gonk-drf) is
	// fixed: that would deliver the prompt twice.
	created, err := d.GC.CreateSession(ctx, gcapi.CreateSessionRequest{
		Kind:  "agent",
		Name:  agent,
		Alias: alias,
		Async: true,
	})
	if err != nil {
		if gcapi.IsNotFound(err) {
			// A 404 on the sessions route is a wrong GONK_CITY (OD-1) or an
			// unloaded pack, same as the pour path -- a misconfiguration.
			d.Log.Error("supervisor 404 on create-session -- check GONK_CITY and that the pack loaded", "err", err)
			return 2
		}
		d.Log.Error("create-session failed", "agent", agent, "alias", alias, "err", err)
		return 1
	}
	// The create's 202 is no more a receipt than the submit's. Agent-kind create
	// is always-async: it validates and spawns AFTER answering, so a create that
	// fails outright -- a taken alias, an unknown agent -- is still a 202.
	// Observed live 2026-08-01: a colliding alias answered 202 and then emitted
	// request.failed error_code=create_failed "session alias already exists",
	// while dispatch logged "triage session created" and carried on to submit a
	// prompt into a session it had not created.
	ready, err := awaitCreate(ctx, d, alias, created)
	if err != nil {
		d.Log.Error("create-session did not succeed", "agent", agent, "alias", alias, "err", err)
		return 1
	}
	if ready {
		// The success event means the session is COMMANDABLE, so the prompt
		// should land on the first submit rather than after the retry loop
		// spends the pod-start window on resolve_failed.
		d.Log.Debug("session is commandable; delivering prompt", "alias", alias)
	}

	if err := deliverPrompt(ctx, d, alias, prompt); err != nil {
		// An undelivered prompt is a real failure, not a warning: the session
		// exists but will idle forever and never be swept. Fail as infra so the
		// existing re-sling decides again and retries with a fresh
		// attempt-suffixed alias.
		d.Log.Error("prompt delivery failed; session will idle -- re-sling will retry",
			"agent", agent, "alias", alias, "bead", a.BeadAnchor, "err", err)
		return 1
	}

	base.State = beadstore.StateRunning
	base.SessionID = alias
	base.Rung, base.Model, base.ReservationID = dec.Rung, dec.Model, dec.ReservationID
	base.ReservationExpiresAt = dec.ReservationExpiresAt
	if err := d.Store.Put(ctx, base); err != nil {
		d.Log.Error("bead store Put failed", "err", err, "bead", a.BeadAnchor)
		return 1
	}
	d.Log.Info("triage session created", "agent", agent, "alias", alias,
		"bead", a.BeadAnchor, "rung", dec.Rung, "attempt", dec.Attempt)
	return 0
}

// awaitCreate reads the create's terminal event off the city log. It returns
// ready=true once the session is confirmed COMMANDABLE, and an error only when
// the create is confirmed to have FAILED.
//
// The asymmetry is deliberate and comes from upstream's own control flow
// (GASCITY_REF internal/api/huma_handlers_sessions_command.go, verified):
//
//   - A create FAILURE (taken alias, bad config) is emitted immediately, before
//     any waiting -- it is plain validation. Observed live: sub-second.
//   - A create SUCCESS is emitted only after WaitForSessionCommandable, which
//     blocks up to 120s for the pod to start and its tmux to come up. Observed
//     live: NOT emitted within 45s.
//
// So a timeout here is NOT a failure and must not be treated as one -- it means
// the pod is still starting, which is the normal case. Delivery simply falls
// through to deliverPrompt's retry loop, which is built for exactly that window.
// Blocking on the success event instead would either exceed the order timeout or
// turn every slow-but-healthy pod start into a failed dispatch.
//
// A create failure is never retried: the alias is attempt-suffixed and
// deterministic, so every reason it could fail would fail identically on the
// next pass. The re-sling path decides again with a fresh attempt, which is the
// right level for that.
func awaitCreate(ctx context.Context, d dispatchDeps, alias string, ack *gcapi.CreateSessionResult) (bool, error) {
	if ack == nil || ack.RequestID == "" {
		return false, fmt.Errorf("create for session %q returned no request id: the outcome cannot be confirmed", alias)
	}
	timeout := d.CreateAwaitTimeout
	if timeout <= 0 {
		timeout = defaultCreateAwaitTimeout
	}
	outcome, err := d.GC.AwaitRequestOutcome(ctx, ack.RequestID, ack.EventCursor, gcapi.EventSessionCreateResult, timeout)
	if err != nil {
		// Unobserved within the window: the pod is still starting. Say so at
		// debug and let the delivery loop do the waiting.
		d.Log.Debug("create outcome not yet reported; the pod is still starting",
			"alias", alias, "err", err)
		return false, nil
	}
	if !outcome.OK {
		return false, fmt.Errorf("create of session %q %s", alias, outcome)
	}
	return true, nil
}

// sessionIsRunning reports whether the session's runtime is live yet. A 404 is
// "not there yet" -- agent create is async, so the alias legitimately does not
// resolve for the first seconds -- not an error.
func sessionIsRunning(ctx context.Context, d dispatchDeps, alias string) (bool, error) {
	// peekLines=1: this is a state check, not a read of the agent's output, and
	// the whole transcript would be pulled otherwise.
	view, err := d.GC.GetSessionOutput(ctx, alias, 1)
	if err != nil {
		if gcapi.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return view.Running, nil
}

// deliverPrompt submits the rendered prompt to the freshly-created session and
// then CONFIRMS, from the city event log, that it was actually delivered.
//
// THE 202 IS NOT A RECEIPT. POST .../session/{id}/submit resolves the session
// and delivers the message in a goroutine AFTER answering, so a submit against
// a session that does not exist yet is answered 202 exactly like one that
// lands, and the real outcome exists only as a terminal event keyed by the
// request id (gcapi.AwaitRequestOutcome). An earlier version of this function
// retried on 404 -- a status this route never returns -- so the first submit
// always "succeeded", dispatch recorded StateRunning, and the prompt was
// dropped whenever the async create had not materialized the session yet. Every
// agent then sat at opencode's idle splash forever. That was gonk-u1p.7; do not
// reintroduce a success path that does not read the outcome.
//
// The retry is not defensive padding: agent-kind create is ALWAYS-async
// upstream (202 with no session id), so the session genuinely does not exist
// for the first attempts -- a live run took ~31s from create to session start.
// Only a RETRYABLE outcome is retried (the session is not there / not live
// yet); a hard rejection is terminal, because retrying it just burns the
// order's timeout budget.
//
// The bound must stay comfortably inside gonk-dispatch's own 120s order timeout
// (pack/orders/gonk-dispatch.toml) -- overshooting it turns a recoverable
// delivery failure into a killed order with no bead update.
func deliverPrompt(ctx context.Context, d dispatchDeps, alias, prompt string) error {
	attempts, backoff := d.SubmitAttempts, d.SubmitBackoff
	if attempts <= 0 {
		attempts = defaultSubmitAttempts
	}
	if backoff == nil {
		backoff = defaultSubmitBackoff
	}
	awaitTimeout := d.SubmitAwaitTimeout
	if awaitTimeout <= 0 {
		awaitTimeout = defaultSubmitAwaitTimeout
	}
	budget := d.SubmitDeadline
	if budget <= 0 {
		budget = defaultSubmitDeadline
	}
	// The budget bounds the CONTEXT, not just the loop arithmetic, so it caps the
	// in-flight HTTP calls too. Without this a single slow round-trip started
	// just inside the deadline could run the whole delivery well past it -- a
	// live run overshot to 3m14s against a 90s budget exactly that way, which
	// would be a killed order rather than a legible failure.
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	deadline := time.Now().Add(budget)

	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(i)):
			}
		}
		// The hard cap on the whole loop. Checked before spending an attempt so
		// the budget bounds real work, not just the sleeps between it.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		// DO NOT SUBMIT INTO A SESSION THAT IS NOT RUNNING YET.
		//
		// Manager.submit parks a default-intent message on the nudge queue --
		// outcome.Queued -- when the session is still start_pending/creating
		// (GASCITY_REF internal/session/submit.go). Delivery then depends on a
		// separate poller process, and PROVEN LIVE 2026-08-01 it never arrived:
		// session go-57b took a queued prompt, its pod came up healthy, and
		// opencode still sat at the idle splash minutes later. A queued prompt is
		// a dropped prompt on this backend.
		//
		// Waiting for running=true avoids the queue entirely rather than trying
		// to recover from it, which also sidesteps the one thing a retry cannot
		// undo: a parked copy landing later and prompting the agent twice.
		running, err := sessionIsRunning(ctx, d, alias)
		if err != nil {
			// A state check that could not be answered is not a verdict. The
			// supervisor is single-threaded behind a session mutation lock and a
			// live run saw this GET exceed its client timeout while another
			// session was mid-turn -- failing the dispatch on that would throw
			// away a perfectly good session over a slow read. Retry within the
			// budget instead; if it never answers, the loop exhausts and fails.
			last = err
			d.Log.Debug("could not read session state; will retry", "alias", alias, "attempt", i+1, "err", err)
			continue
		}
		if !running {
			last = fmt.Errorf("session is not running yet")
			d.Log.Debug("session not running yet; holding the prompt back", "alias", alias, "attempt", i+1)
			continue
		}

		ack, err := d.GC.SubmitSession(ctx, alias, prompt, gcapi.SubmitIntentDefault)
		if err != nil {
			// A transport-level rejection really is synchronous (no grant,
			// wrong city, unrouted path) and never reaches the event log.
			return err
		}
		if ack == nil || ack.RequestID == "" {
			// No correlation handle means no way to confirm delivery, and an
			// unconfirmable prompt is exactly the failure being fixed here.
			return fmt.Errorf("submit for session %q returned no request id: delivery cannot be confirmed", alias)
		}

		// NOTE the await is NOT retried on timeout, and the prompt is NOT
		// resubmitted: a timeout means the outcome is unobserved, not that it
		// failed, and resubmitting would risk delivering the prompt twice while
		// still not knowing. An unknown outcome fails the dispatch, loudly.
		outcome, err := d.GC.AwaitRequestOutcome(ctx, ack.RequestID, ack.EventCursor, gcapi.EventSessionSubmitResult, min(awaitTimeout, remaining))
		if err != nil {
			return fmt.Errorf("confirming prompt delivery to session %q: %w", alias, err)
		}
		if outcome.OK && !outcome.Queued {
			return nil // typed into the live runtime, and observed to be
		}
		if outcome.OK {
			// Accepted but PARKED, not delivered -- see the running-check above
			// for why that is a dropped prompt here. The running gate should
			// make this unreachable, so reaching it is worth a warning, not a
			// silent retry.
			last = fmt.Errorf("prompt was queued rather than delivered live")
			d.Log.Warn("prompt accepted but QUEUED, not delivered live -- retrying",
				"alias", alias, "session", outcome.SessionID)
			continue
		}
		last = fmt.Errorf("%s", outcome.String())
		if !outcome.Retryable() {
			return fmt.Errorf("prompt delivery to session %q rejected: %w", alias, last)
		}
		d.Log.Debug("prompt not delivered yet; session still materializing",
			"alias", alias, "attempt", i+1, "outcome", outcome.String())
	}
	return fmt.Errorf("session %q never accepted its prompt after %d attempts: %w", alias, attempts, last)
}
