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

// renderTriagePrompt builds the session's initial message. Two things ride at
// the top as HTML-comment marker lines that gonk-agent-entrypoint parses out of
// its --prompt argument and strips before opencode sees them (the ONLY
// per-session channel into a session pod -- GC_WEBHOOK_ARG_* is exec-order-only
// and does not reach an agent session; see pack/agents/triage/agent.toml):
//
//   - gonk:model -- the rung's model, chosen by meter, so the harness selects it.
//   - gonk:meta  -- the attribution metadata (verbatim meter JSON) for spend logs.
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
func renderTriagePrompt(project string, issueIID int64, model, metadataJSON, issueContext string) string {
	context := issueContext
	if strings.TrimSpace(context) == "" {
		context = "(issue context unavailable -- triage from the issue reference alone)"
	}
	return fmt.Sprintf(`<!-- gonk:model:%s -->
<!-- gonk:meta:%s -->

Triage GitLab issue !%d in project `+"`%s`"+`. Here is the issue, already
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
GONK_BATCH_END.`, model, metadataJSON, issueIID, project, context)
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
	md, err := json.Marshal(dec.Metadata)
	if err != nil {
		d.Log.Error("could not marshal meter metadata", "err", err, "bead", a.BeadAnchor)
		return 1
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
	prompt := renderTriagePrompt(a.Project, a.IssueIID, dec.Model, string(md), issueContext)

	// NOTE the create carries NO Message. It used to, and that is exactly the
	// bug: `message` becomes template_overrides.initial_message, which Gas City
	// puts on runtime.Config.PromptSuffix for the PROVIDER to append to the
	// launch command -- and internal/runtime/k8s never does (tmux/acp/herdr/
	// t3bridge do). Every gonk session is a k8s pod, so the prompt was silently
	// dropped and the agent sat at opencode's idle splash forever, never
	// finishing and so never being swept. The prompt is delivered by the submit
	// below instead. Do NOT "restore" Message here once upstream (gonk-drf) is
	// fixed: that would deliver the prompt twice.
	if _, err := d.GC.CreateSession(ctx, gcapi.CreateSessionRequest{
		Kind:  "agent",
		Name:  agent,
		Alias: alias,
		Async: true,
	}); err != nil {
		if gcapi.IsNotFound(err) {
			// A 404 on the sessions route is a wrong GONK_CITY (OD-1) or an
			// unloaded pack, same as the pour path -- a misconfiguration.
			d.Log.Error("supervisor 404 on create-session -- check GONK_CITY and that the pack loaded", "err", err)
			return 2
		}
		d.Log.Error("create-session failed", "agent", agent, "alias", alias, "err", err)
		return 1
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

// deliverPrompt submits the rendered prompt to the freshly-created session,
// retrying while the supervisor still answers 404.
//
// The retry is not defensive padding: agent-kind create is ALWAYS-async
// upstream (202 with no session id), so the session genuinely does not exist
// for the first attempts -- a live run took ~31s from create to session start.
// Only a 404 is retried; any other error is terminal, because retrying a 401 or
// a 5xx here just burns the order's timeout budget.
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
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(i)):
			}
		}
		err = d.GC.SubmitSession(ctx, alias, prompt, gcapi.SubmitIntentDefault)
		if err == nil {
			return nil
		}
		if !gcapi.IsNotFound(err) {
			return err
		}
	}
	return fmt.Errorf("session %q never materialized for prompt delivery after %d attempts: %w", alias, attempts, err)
}
