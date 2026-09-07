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
	"gitlab.orac.local/agentic/gonk-project/pkg/rig"
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
	// Forge reads what the broker is about to work on, so the controller can
	// splice it into the injected prompt: the ISSUE for triage, the REPOSITORY
	// for scaffold. The agent pod has forge creds for neither, and for scaffold
	// it has no checkout either -- nothing clones one (gonk-msz).
	//
	// Optional, but the two failure modes differ deliberately. A triage fetch
	// failure degrades to a reference-only prompt, because naming a real issue
	// is still enough to reason about. A scaffold fetch failure does NOT
	// degrade: with no repository material the agent has nothing to be accurate
	// about, so renderScaffoldPrompt makes it refuse rather than invent .agent/
	// content that gonk would then commit and open an MR for.
	//
	// Only the broker path uses it. Satisfied by *glab.Client.
	Forge brokerForgeReader

	// Keys follows the meter's KeyRef to the project's own LiteLLM virtual key
	// (gonk-8gb). Nil means no cluster access, and resolveLiteLLMKey then fails
	// closed rather than substituting the controller's admin key.
	Keys *secretReader

	// Apply/GL/BotUsername exist ONLY for the canned status comment (gonk-yrs).
	// When a bead is parked -- unmetered, over budget, quiet hours -- the
	// reporter otherwise sees nothing at all, which is indistinguishable from
	// being ignored. All three are optional; nil simply means no status comment
	// is posted, and dispatch's real work is unaffected.
	Apply       brokerApplier
	GL          gitlabQuerier
	BotUsername string

	// Rig / RigBaseURL register and advertise the per-session CHECKOUT (pkg/rig).
	// Rig registers the grant controller-side at the decision point; RigBaseURL
	// is gonk-intake's PRIVATE listener as the agent pod addresses it, which is
	// what the pod fetches from.
	//
	// Both optional: unset simply means no checkout is granted, and the agent
	// falls back to controller-side repository context and then to an explicit
	// refusal. Never fatal -- a session without a checkout still runs.
	Rig        *rig.Client
	RigBaseURL string
	Log        *slog.Logger
	Args       dispatchArgs
	// SubmitAttempts / SubmitBackoff bound the wait for an async-created session
	// to exist before its prompt is fetched (see awaitPromptFetched). Zero
	// values mean the production defaults; tests set them to keep the retry
	// path fast and deterministic.
	SubmitAttempts int
	SubmitBackoff  func(attempt int) time.Duration
	// SubmitAwaitTimeout bounds how long ONE attempt waits for the submit's
	// TERMINAL EVENT before giving up on observing it. The 202 says nothing
	// about delivery, so this is the wait for the only answer that exists.
	SubmitAwaitTimeout time.Duration
	// SubmitDeadline is the hard cap on the whole deliver-and-confirm loop,
	// regardless of how the attempts and timeouts above divide it up. It is what
	// actually keeps dispatch inside its order timeout.
	SubmitDeadline time.Duration
	// CreateAwaitTimeout bounds the wait for the CREATE's terminal event. It is
	// separate from SubmitAwaitTimeout because the two events behave nothing
	// alike: a create failure is immediate validation, a create success waits on
	// the pod. See awaitCreate.
	CreateAwaitTimeout time.Duration
}

const (
	// The retry loop exists for the async-create window: agent-kind create
	// returns 202 with no session id and the pod takes its time. MEASURED on
	// orac 2026-08-01: ~90s from create to a commandable session. 40 x 3s = 120s
	// of patience covers that with margin.
	defaultSubmitAttempts = 40
	submitBackoffInterval = 3 * time.Second
	// defaultSubmitDeadline keeps the whole loop inside gonk-dispatch's order
	// timeout (pack/orders/gonk-dispatch.toml, 300s) with room for the meter
	// call, the issue fetch and the create that precede it. Overshooting it
	// turns a recoverable delivery failure into a killed order with no bead
	// update. It bounds the CONTEXT, so it caps in-flight requests too.
	defaultSubmitDeadline = 240 * time.Second
	// defaultCreateAwaitTimeout is short ON PURPOSE. It is a fast check for a
	// create that FAILED (validation, emitted immediately), not a wait for one
	// that succeeded (emitted only after the pod is commandable -- measured live
	// as NOT within 45s). Waiting longer would buy nothing and spend the budget
	// that deliverPrompt needs, since the retry loop covers the same window.
	defaultCreateAwaitTimeout = 3 * time.Second
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
		// Say so in the thread, at no token cost, and EDIT the same note on every
		// subsequent park so the issue carries one current status rather than a
		// growing pile of apologies (gonk-yrs).
		d.postDeferredNotice(ctx, a, dec.Reason, dec.Detail, dec.RetryAfter)
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
	// THE AGENT GETS THE PROJECT'S KEY, NOT THE CONTROLLER'S (gonk-8gb). Resolved
	// BEFORE the vars map is built so a failure stops the dispatch instead of
	// pouring a session that would authenticate as the proxy admin.
	litellmKey, kerr := resolveLiteLLMKey(ctx, d.Keys, dec.KeyRef.SecretName, dec.KeyRef.SecretKey)
	if kerr != nil {
		d.Log.Error("refusing to dispatch: no per-project LiteLLM key",
			"bead", a.BeadAnchor, "project", a.Project, "err", kerr)
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
		// (litellm_key below now FOLLOWS this pointer -- see resolveLiteLLMKey.)

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
		"litellm_key": litellmKey,
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

// postDeferredNotice publishes gonk's "I cannot answer yet" status comment.
//
// Deliberately BEST EFFORT and never fatal: this is courtesy, not correctness.
// A bead that is parked is parked whether or not the reporter was told, and
// failing the dispatch because a comment did not post would turn a polite
// gesture into an outage.
func (d dispatchDeps) postDeferredNotice(ctx context.Context, a dispatchArgs, reason, detail string, retryAfter time.Time) {
	if d.Apply == nil || a.ProjectID == 0 || a.IssueIID == 0 {
		return
	}
	sd := sweepDeps{Apply: d.Apply, GL: d.GL, BotUsername: d.BotUsername, Log: d.Log}
	upsertStatusNote(ctx, sd, a.ProjectID, a.IssueIID, a.BeadID,
		deferredBody(reason, detail, retryAfter, d.BotUsername))
}
