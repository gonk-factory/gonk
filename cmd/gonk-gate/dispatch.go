package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// orderForTrigger maps a trigger to the formula-order that runs it. This is the
// ONLY mapping from work to formula, and it is a closed set: an unknown trigger
// pours nothing.
var orderForTrigger = map[string]string{
	"issue-triage":  "gonk-triage",
	"scaffold":      "gonk-scaffold",
	"mention-reply": "gonk-mention",
	// NOTE: "onboarding" is deliberately absent. The onboarding MR is
	// DETERMINISTIC AND ZERO-TOKEN (spec 5.3) -- intake writes it directly. There
	// is no formula, no agent and no rung for it, and adding one would put an LLM
	// on the one path that is specified not to have one.
}

// dispatchArgs is what GC_WEBHOOK_ARG_* handed main.go for this invocation
// (see pack/orders/gonk-dispatch.toml's [order.params], Task 4).
type dispatchArgs struct {
	Project   string
	ProjectID int64
	Rig       string
	IssueIID  int64
	// BeadAnchor is the deterministic, re-sling-stable identifier
	// (`gonk:{project_id}:issue:{iid}`). IT is what goes on the wire as
	// meterapi.DecideRequest.BeadID -- the budget/ladder/reservation key, identical
	// to the value intake sent at Gate 1.
	BeadAnchor string
	// BeadID is the Gas City internal bead id (e.g. gk-1a2b). It exists by Gate 2
	// (intake's order created the bead) and is used for the comment marker and the
	// bead-store record -- but it MUST NOT be sent to meter: it differs between the
	// gates and is not stable across re-slings.
	BeadID     string
	SessionKey string
	Trigger    string
	ConfigHash string

	// Rung / Model / ReservationID arrive from intake's own /decide (Gate 1).
	// THEY ARE A HINT FOR LOGS AND NOTHING ELSE. runDispatch re-decides and uses
	// meter's live answer. See TestDispatchAlwaysDecidesEvenWhenVarsCarryARung.
	Rung          string
	Model         string
	ReservationID string
}

type dispatchDeps struct {
	Meter *meterAPI // gonk-gate's thin wrapper over pkg/meterapi (see meter.go)
	GC    *gcapi.Client
	Store beadstore.Store
	// Forge reads the issue the broker is about to triage, so the controller can
	// splice its context into the injected prompt (the agent pod has no forge
	// creds). Optional: nil (or a fetch failure) degrades to a reference-only
	// prompt. Only the broker path uses it. Satisfied by *glab.Client.
	Forge issueReader
	Log   *slog.Logger
	Args  dispatchArgs
	// SubmitAttempts / SubmitBackoff bound the wait for an async-created session
	// to exist before its prompt can be submitted (see deliverPrompt). Zero
	// values mean the production defaults; tests set them to keep the retry
	// path fast and deterministic.
	SubmitAttempts int
	SubmitBackoff  func(attempt int) time.Duration
}

const (
	// defaultSubmitAttempts x defaultSubmitBackoff must stay well inside
	// gonk-dispatch's 120s order timeout. A live run took ~31s from create to
	// session start, so 20 x 3s = 60s leaves room for both the session to
	// appear and the rest of the order to finish.
	defaultSubmitAttempts = 20
	submitBackoffInterval = 3 * time.Second
)

// defaultSubmitBackoff is deliberately flat, not exponential: we are waiting on
// a roughly fixed pod-start latency, not backing off a struggling server, and an
// exponential curve would spend most of the budget asleep past the moment the
// session actually appeared.
func defaultSubmitBackoff(int) time.Duration { return submitBackoffInterval }

// runDispatch is GATE 2: the only code path in gonk that pours a formula.
//
// It ALWAYS asks gonk-meter /v1/policy/decide first -- on the first dispatch, on
// a re-sling after a gate failure, on an unpark after a defer, on every single
// pour without exception. There is no fast path, no cache, and no "intake already
// checked". A ladder escalation is controller-initiated and never passes intake,
// so if this function trusts its inputs, escalations spend unmetered.
//
// Exit codes: 0 = a decision was made and acted on (run|defer|deny); 1 = infra;
// 2 = misconfiguration.
func runDispatch(ctx context.Context, d dispatchDeps) int {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	a := d.Args

	order, ok := orderForTrigger[a.Trigger]
	if !ok {
		d.Log.Error("unknown trigger; pouring nothing", "trigger", a.Trigger)
		return 2
	}
	if d.GC == nil || d.GC.City == "" {
		d.Log.Error("GONK_CITY is unset; refusing to guess a city name (OD-1)")
		return 2
	}

	// ---- THE GATE. Every pour. No exceptions. -------------------------------
	// NOTE what is NOT in this request: an attempt count. Meter owns ladder state
	// (Plan 03, Decision 2); a caller-supplied attempt is a forgery vector.
	//
	// bead_id IS THE BeadAnchor -- the SAME value intake sent at Gate 1, NOT the Gas
	// City bead id (a.BeadID). Meter keys /decide idempotency, ladder state and the
	// reservation on bead_id; if Gate 1 sent the BeadAnchor and Gate 2 sent the Gas
	// City bead id, meter would see two different beads for one work item and mint a
	// SECOND reservation -- double budget headroom, and Gate 1's reservation leaks
	// until its TTL. The Gas City bead id stays in a.BeadID for the marker/record,
	// but it never reaches meter. See the cross-plan BeadAnchor contract.
	dec, err := d.Meter.Decide(ctx, meterapi.DecideRequest{
		Project:    a.Project,
		Rig:        a.Rig,
		BeadID:     a.BeadAnchor, // bead_id == BeadAnchor, the same value Gate 1 sent
		SessionKey: a.SessionKey,
		Trigger:    a.Trigger,
	})
	if err != nil {
		// FAIL CLOSED. An unreachable budget enforcer is not permission to spend.
		d.Log.Error("meter /decide failed; pouring nothing", "err", err, "bead", a.BeadAnchor)
		return 1
	}

	base := beadstore.Record{
		BeadAnchor: a.BeadAnchor, BeadID: a.BeadID, Project: a.Project, ProjectID: a.ProjectID,
		Rig: a.Rig, SessionKey: a.SessionKey, Trigger: a.Trigger, IssueIID: a.IssueIID,
		ConfigHash: a.ConfigHash, Attempt: dec.Attempt,
	}

	switch dec.Decision {
	case meterapi.DecisionDefer:
		// A NORMAL ANSWER (meterapi: defer and deny are HTTP 200). Quiet hours and
		// budget exhaustion both land here. Park it; gonk-sweep unparks it at
		// RetryAfter -- by calling THIS function again, which decides again.
		base.State = beadstore.StateParked
		base.RetryAfter = dec.RetryAfter
		if err := d.Store.Put(ctx, base); err != nil {
			d.Log.Error("bead store Put failed", "err", err, "bead", a.BeadAnchor)
			return 1
		}
		d.Log.Info("bead parked", "bead", a.BeadAnchor, "reason", dec.Reason,
			"detail", dec.Detail, "retry_after", dec.RetryAfter)
		return 0

	case meterapi.DecisionDeny:
		// The ladder is exhausted, or the action is not allowed, or the project is
		// not registered. Stop. A human decides what happens next; the sweeper
		// never picks a needs-human bead up again.
		base.State = beadstore.StateNeedsHuman
		if err := d.Store.Put(ctx, base); err != nil {
			d.Log.Error("bead store Put failed", "err", err, "bead", a.BeadAnchor)
			return 1
		}
		d.Log.Warn("bead denied", "bead", a.BeadAnchor, "reason", dec.Reason, "detail", dec.Detail)
		return 0

	case meterapi.DecisionRun:
		// fall through

	default:
		d.Log.Error("meter returned an unknown decision; pouring nothing", "decision", dec.Decision)
		return 1
	}

	// ---- v2 broker path. -----------------------------------------------------
	// A ported trigger (agentForTrigger) does NOT pour a formula: it creates the
	// agent session directly, injects the rendered prompt, and correlates by a
	// session alias recorded on the bead. The agent holds no forge creds and
	// posts nothing; gonk-sweep validates + applies its proposed-effects batch.
	// scaffold/mention are not ported yet and fall through to the formula pour.
	if agent, ok := agentForTrigger[a.Trigger]; ok {
		return runBrokerDispatch(ctx, d, agent, *dec, base)
	}

	// ---- Pour, with METER'S answer. Never with the caller's. ----------------
	md, err := json.Marshal(dec.Metadata)
	if err != nil {
		d.Log.Error("could not marshal meter metadata", "err", err, "bead", a.BeadAnchor)
		return 1
	}
	vars := map[string]string{
		"project":     a.Project,
		"project_id":  fmt.Sprint(a.ProjectID),
		"rig":         a.Rig,
		"issue_iid":   fmt.Sprint(a.IssueIID),
		"bead_anchor": a.BeadAnchor,
		// NOT "bead_id": formulas v2 reserves that exact key (alongside
		// convoy_id and the deprecated issue alias) and Gas City's
		// graphv2.PrepareInvocation rejects ANY caller-supplied vars map that
		// contains it -- "formulas v2 reserved variable \"bead_id\" cannot be
		// supplied by the caller" -- regardless of whether the formula
		// declares it. Every pour here targets a formula order
		// (orderForTrigger's values), so this map IS that caller-supplied
		// vars map. See pack/formulas/*.toml's matching comment.
		"city_bead_id": a.BeadID,
		"session_key":  a.SessionKey,
		"trigger":      a.Trigger,
		"config_hash":  a.ConfigHash,

		// From meter, verbatim. The pack does not choose a rung, does not choose a
		// model, and does not mint a tag.
		"rung":           dec.Rung,
		"model":          dec.Model,
		"attempt":        fmt.Sprint(dec.Attempt),
		"reservation_id": dec.ReservationID,
		"metadata_json":  string(md),

		// A POINTER to the key. NEVER the key. A credential in an order var is a
		// credential in the event bus and in every log line that echoes it.
		"key_secret_name": dec.KeyRef.SecretName,
		"key_secret_key":  dec.KeyRef.SecretKey,

		// v1-minimal session-config delivery (gonk-aql). Gas City's k8s session
		// provider mounts no gonk secrets and controller env does not flow to
		// sessions, so the agent's LiteLLM endpoint/key and the bot token are
		// forwarded from THIS controller as order vars. litellm_url is not secret
		// and rides a plain env. The two SECRETS are read from FILES, because Gas
		// City strips inherited env whose key contains a secret marker
		// (IsSensitiveKey: TOKEN/SECRET/...) from exec orders -- so a
		// GONK_*_TOKEN env would arrive empty. The path envs use non-secret names
		// (GONK_LITELLM_KEY_FILE has no marker; the bot token uses GONK_BOT_FILE,
		// NOT *_TOKEN_FILE, so it too survives) and the secret only ever lives in
		// the mounted file. litellm_key/bot_token DELIBERATELY violate the "never a
		// credential in an order var" rule above -- a v1-only compromise the v2
		// broker removes (it keeps all creds out of the pod).
		"litellm_url": os.Getenv("GONK_LITELLM_URL"),
		"litellm_key": readFileEnvValue("GONK_LITELLM_KEY_FILE"),
		"bot_token":   readFileEnvValue("GONK_BOT_FILE"),
	}

	if _, err := d.GC.RunOrder(ctx, order, vars); err != nil {
		if gcapi.IsNotFound(err) {
			// Almost always a wrong GONK_CITY (OD-1) or a pack that did not load.
			d.Log.Error("supervisor 404 -- check GONK_CITY and that the pack loaded", "err", err)
			return 2
		}
		d.Log.Error("pour failed", "order", order, "err", err)
		return 1
	}

	base.State = beadstore.StateRunning
	base.Rung, base.Model, base.ReservationID = dec.Rung, dec.Model, dec.ReservationID
	base.ReservationExpiresAt = dec.ReservationExpiresAt
	if err := d.Store.Put(ctx, base); err != nil {
		d.Log.Error("bead store Put failed", "err", err, "bead", a.BeadAnchor)
		return 1
	}
	d.Log.Info("poured", "order", order, "bead", a.BeadAnchor, "rung", dec.Rung, "attempt", dec.Attempt)
	return 0
}
