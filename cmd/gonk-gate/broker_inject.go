package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

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
// NOTE (Task 4.1): the issue's body and current labels are NOT yet injected
// here. Because the pod has no forge creds it cannot fetch them itself, so a
// working triage needs the controller to fetch them (with its own PAT) and
// splice them into this prompt. That is the next increment; this function is the
// seam. The exact emit wording may be tightened after C2's first live run
// confirms the fenced batch survives the GetSession(peek) read (C5).
func renderTriagePrompt(project string, issueIID int64, model, metadataJSON string) string {
	return fmt.Sprintf(`<!-- gonk:model:%s -->
<!-- gonk:meta:%s -->

Triage GitLab issue !%d in project `+"`%s`"+`.

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
GONK_BATCH_END.`, model, metadataJSON, issueIID, project)
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
	prompt := renderTriagePrompt(a.Project, a.IssueIID, dec.Model, string(md))

	if _, err := d.GC.CreateSession(ctx, gcapi.CreateSessionRequest{
		Kind:    "agent",
		Name:    agent,
		Alias:   alias,
		Message: prompt,
		Async:   true,
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
