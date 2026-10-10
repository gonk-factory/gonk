package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/intake"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/rig"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

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

	// FireBy is the `fire_by` order var, RFC 3339: the latest instant this
	// dispatch may START. Only gonk-sweep's re-sling sets it; empty means no
	// deadline (intake, unpark). See refuseLateFire.
	FireBy string

	// Rung / Model / ReservationID arrive from intake's own /decide (Gate 1).
	// THEY ARE A HINT FOR LOGS AND NOTHING ELSE. runDispatch re-decides and uses
	// meter's live answer. See TestDispatchAlwaysDecidesEvenWhenVarsCarryARung.
	Rung          string
	Model         string
	ReservationID string

	// pipeline-diagnosis orders only (gonk-tios D12): the run identity intake
	// computed (pkg/intake OrderRequest, all omitempty). BeadAnchor must be
	// exactly pipelinectx.Anchor(ProjectID, PipelineTarget, PipelineFingerprint,
	// PipelineLastSuccess, PipelineFixEpoch), or dispatch refuses the order
	// (pipelineIdentity). PipelineFixEpoch (gonk-2usw 3.3) is 0 -- and its var
	// absent -- for a run that follows no merged fix.
	PipelineID          int64
	PipelineTarget      string
	PipelineFingerprint string
	PipelineLastSuccess int64
	PipelineFixEpoch    int64
}

// dispatchArgsFrom builds dispatchArgs from an order's declared params, read
// through get (main.go passes envArg). One function, so the sweep's refire
// vars and what dispatch reads back cannot disagree on a name
// (TestPipelineRunIdentityRoundTripsThroughRefire).
func dispatchArgsFrom(get func(string) string) dispatchArgs {
	i64 := func(name string) int64 {
		v, _ := strconv.ParseInt(get(name), 10, 64)
		return v
	}
	return dispatchArgs{
		Project:       get("project"),
		ProjectID:     i64("project_id"),
		Rig:           get("rig"),
		IssueIID:      i64("issue_iid"),
		BeadAnchor:    get("bead_anchor"),
		BeadID:        get("bead_id"),
		SessionKey:    get("session_key"),
		Trigger:       get("trigger"),
		ConfigHash:    get("config_hash"),
		FireBy:        get(fireByVar),
		Rung:          get("rung"),
		Model:         get("model"),
		ReservationID: get("reservation_id"),

		PipelineID:          i64("pipeline_id"),
		PipelineTarget:      get("pipeline_target"),
		PipelineFingerprint: get("pipeline_fingerprint"),
		PipelineLastSuccess: i64("pipeline_last_success"),
		PipelineFixEpoch:    i64("pipeline_fix_epoch"),
	}
}

type dispatchDeps struct {
	Meter *meterAPI // gonk-gate's thin wrapper over pkg/meterapi (see meter.go)
	// Runtime starts the agent session (gonk-t55). It is the ONLY way dispatch
	// touches a session: dispatch names no Job and no Gas City alias. Nil is a
	// misconfiguration (exit 2). In production it is the Kubernetes Jobs
	// runtime (main.go, newSessionRuntime); tests use pkg/runtime/fake.
	Runtime runtime.SessionRuntime
	// PackDir is the baked pack root the session's environment is read from:
	// <PackDir>/agents/<agent>/agent.toml [env] (plan D5). Empty means
	// defaultPackDir.
	PackDir string
	// LiteLLMURL is the LiteLLM base URL the agent is pointed at
	// (GONK_LITELLM_URL, passed through to the session env). NOT a secret: the
	// key that authorizes it is mounted, never env.
	LiteLLMURL string
	Store      beadstore.Store
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
	// Pipelines is the pipeline path's GitLab (gonk-tios): the controller-side
	// context read at dispatch, and the run note's lifecycle writes
	// (diagnosing / deferred / denied). Nil: a pipeline dispatch has no
	// context to build and exits 1; its notes are not written. Satisfied by
	// *glab.Client.
	Pipelines pipelineForge
	// PrepareFix prepares a pipeline-fix session (gonk-2usw; the seam to part
	// AGENT, broker_pipefix_dispatch.go; production passes pipefixPrepare).
	// Nil refuses every pipeline-fix order BEFORE meter is asked: no
	// reservation, no record write, no session.
	PrepareFix pipefixPreparer
	// FixConfig answers a project's effective configuration for the fix
	// preparer's envelope fold. Nil means gonk-meter (Meter.Project); tests
	// pass a fake.
	FixConfig pipefixConfigSource
	// fixRun is the diagnosed run's record of the pipeline-fix order being
	// dispatched, read by pipefixPreflight and handed on to the preparer and
	// the fix note. Set by runDispatch only.
	fixRun beadstore.Record
	Log    *slog.Logger
	Args   dispatchArgs
	// StartedAt is when the gonk-gate PROCESS started (main sets it first
	// thing). The order budget -- the start deadline and the close/release
	// reserves -- is measured from it against the order timeout. Zero means
	// "now" at the start of runBrokerDispatch, which is what tests want.
	StartedAt time.Time
	// Budget slices the order timeout (order_budget.go). Zero fields mean the
	// production defaults.
	Budget orderBudget
	// Clock is dispatch's view of time. Nil means the real clock; tests pass a
	// fake so the order budget is exact and instant.
	Clock dispatchClock
}

func (d dispatchDeps) clock() dispatchClock {
	if d.Clock == nil {
		return realClock{}
	}
	return d.Clock
}

func (d dispatchDeps) packDir() string {
	if d.PackDir == "" {
		return defaultPackDir
	}
	return d.PackDir
}

// defaultPackDir is where images/Dockerfile.controller bakes the pack. It is a
// variable only so the package's tests can point it at the repository's own
// pack/ (TestMain), which is the file the image bakes.
var defaultPackDir = "/opt/gonk/pack"

// dispatchClock is dispatch's only view of time. Nil in dispatchDeps means the
// real clock; tests pass a fake whose Sleep advances Now.
type dispatchClock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runDispatch is GATE 2: the only code path in gonk that creates a broker
// agent session.
//
// It ALWAYS asks gonk-meter /v1/policy/decide first -- on the first dispatch, on
// a re-sling after a gate failure, on an unpark after a defer, on every single
// dispatch without exception. There is no fast path, no cache, and no "intake
// already checked". A ladder escalation is controller-initiated and never
// passes intake, so if this function trusts its inputs, escalations spend
// unmetered.
//
// Exit codes: 0 = a decision was made and acted on (run|defer|deny); 1 = infra;
// 2 = misconfiguration.
func runDispatch(ctx context.Context, d dispatchDeps) int {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	a := d.Args

	// FIRST, BEFORE ANY SIDE EFFECT: a re-sling that fired late does nothing.
	if code, refuse := refuseLateFire(d); refuse {
		return code
	}

	// agentForTrigger (broker_inject.go) is the ONLY mapping from work to an
	// agent, and -- since ADR-007 §3 deleted the formula path -- the ONLY
	// routing table left in gonk-gate at all. It is a closed set: an unknown
	// trigger dispatches nothing. That now includes "mention-reply", which
	// used to pour a formula that provably could not deliver its prompt
	// (upstream Gas City drops caller vars, gonk-6gs / #4668); ADR-007 §3
	// chose to fail it loudly here rather than keep idling it, until T-24
	// ports it onto the broker for real.
	agent, ok := agentForTrigger[a.Trigger]
	isFix := a.Trigger == atags.TriggerPipelineFix
	if isFix {
		// pipeline-fix is in agentForTrigger (the sweep judges its sessions),
		// but it is DISPATCHED only when a fix session can be prepared: a
		// dispatchDeps with no preparer refuses the order before meter is
		// asked -- exit 2, like any trigger that routes to no agent.
		agent, ok = pipefixAgent, d.PrepareFix != nil
	}
	if !ok {
		// NAME THE BEAD ANCHOR, not just the trigger (gonk-pop3). The anchor is
		// the one string that joins this refusal to intake's `intake decision`
		// record for the same work item -- and without it, the two halves of the
		// 2026-09-12 !71 investigation could not be joined: intake said it
		// dispatched a mention-reply and gonk-gate said it refused a trigger, and
		// nothing in either line said they were about the same comment. Listing
		// the triggers that DO route turns the line into its own diagnosis.
		d.Log.Error("dispatching nothing: this trigger routes to no agent",
			"trigger", a.Trigger, "bead", a.BeadAnchor, "project", a.Project,
			"issue_iid", a.IssueIID, "session_key", a.SessionKey,
			"routable_triggers", strings.Join(routableTriggers(), ","))
		return 2
	}
	if d.Runtime == nil {
		d.Log.Error("no session runtime is configured; dispatching nothing", "bead", a.BeadAnchor)
		return 2
	}
	// A PIPELINE ORDER MUST CARRY THE RUN IDENTITY ITS ANCHOR WAS BUILT FROM
	// (gonk-tios D2/D12), checked before meter is asked: the anchor is the
	// meter/ladder/landing key, the pipeline id names the session, and a pair
	// that disagree would diagnose one run under another's name -- or another
	// project's (the anchor carries the project id).
	if a.Trigger == atags.TriggerPipelineDiagnosis {
		if err := checkPipelineIdentity(a); err != nil {
			d.Log.Error("refusing a pipeline order whose run identity does not match its bead anchor; dispatching nothing",
				"bead", a.BeadAnchor, "project_id", a.ProjectID, "pipeline_id", a.PipelineID,
				"pipeline_target", a.PipelineTarget, "err", err)
			return 2
		}
	}

	// A PIPELINE-FIX ORDER LIKEWISE (gonk-2usw): its anchor must be the fix
	// anchor of the run its vars identify, and the pack must be able to give
	// the fix agent its environment -- both before meter is asked, so a
	// malformed order or an unbuilt pack never takes a reservation.
	if isFix {
		if err := checkPipelineFixIdentity(a); err != nil {
			d.Log.Error("refusing a pipeline-fix order whose run identity does not match its bead anchor; dispatching nothing",
				"bead", a.BeadAnchor, "project_id", a.ProjectID, "pipeline_id", a.PipelineID,
				"pipeline_target", a.PipelineTarget, "err", err)
			return 2
		}
		if _, err := packAgentEnv(d.packDir(), agent); err != nil {
			d.Log.Error("refusing a pipeline-fix order: the pack has no usable fix agent; dispatching nothing",
				"bead", a.BeadAnchor, "agent", agent, "err", err)
			return 2
		}
	}

	// ---- READ THE RECORD BEFORE METER DECIDES (gonk-b2jk). -------------------
	// A defer or a deny is written AFTER meter decided, and it is only true of
	// the record as it stood when meter decided. Meter never defers or denies
	// while an open reservation covers the bead -- but a 'run' dispatch can
	// reserve between that decision and this dispatch's write, and an
	// unconditional park/deny then erased the reservation, so a third dispatch
	// for the same attempt passed the dedupe guard: two sessions for one
	// attempt. So the park/deny is a PutIf against the revision read HERE,
	// before /decide. Reading after /decide would not do: the reservation could
	// already be in the record read, and the PutIf would then overwrite it.
	//
	// The run path does not use this read: reservePendingPrompt reads again,
	// fresh, for its own compare-and-swap. A failed read is not fatal here
	// either; it only changes what a park/deny may write (writeDecided).
	decidedOn, decidedOnFound, decidedOnErr := d.Store.Get(ctx, a.BeadAnchor)
	if decidedOnErr != nil {
		d.Log.Warn("could not read the bead record before asking meter; a defer or deny will be written "+
			"only if no record exists", "err", decidedOnErr, "bead", a.BeadAnchor)
	}
	decided := decidedOnRecord{rec: decidedOn, found: decidedOnFound, err: decidedOnErr}

	// A pipeline fix is dispatched ONLY from the record gonk-sweep wrote for
	// it, and only while the run it was decided against still stands -- both
	// checked from the store, before meter is asked (pipefixPreflight).
	if isFix {
		run, code, proceed := pipefixPreflight(ctx, d, decided)
		if !proceed {
			return code
		}
		d.fixRun = run
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
	if a.Trigger == atags.TriggerPipelineDiagnosis {
		setPipelineIdentity(&base, a, decided)
	}
	if isFix {
		setPipelineFixIdentity(&base, decided.rec)
	}

	switch dec.Decision {
	case meterapi.DecisionDefer:
		// A NORMAL ANSWER (meterapi: defer and deny are HTTP 200). Quiet hours and
		// budget exhaustion both land here. Park it; gonk-sweep unparks it at
		// RetryAfter -- by calling THIS function again, which decides again.
		base.State = beadstore.StateParked
		base.RetryAfter = dec.RetryAfter
		if code, written := writeDecided(ctx, d, base, decided); !written {
			return code
		}
		d.Log.Info("bead parked", "bead", a.BeadAnchor, "reason", dec.Reason,
			"detail", dec.Detail, "retry_after", dec.RetryAfter)
		if isFix {
			// No note: plan 8.1 has no row for a deferred fix, and the
			// diagnosis note above it already says what failed. The parked
			// record is unparked by the sweep like any other.
			return 0
		}
		if base.PipelineID != 0 { // the run note, never the issue status note
			base = d.withPipelineProjectURL(ctx, base) // for the note's link only (gonk-385g)
			d.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDeferred,
				Status: deferredStatus(base, dec.Reason, dec.Detail, dec.RetryAfter)})
			return 0
		}
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
		if code, written := writeDecided(ctx, d, base, decided); !written {
			return code
		}
		d.Log.Warn("bead denied", "bead", a.BeadAnchor, "reason", dec.Reason, "detail", dec.Detail)
		if isFix { // the FIX note on the diagnosis landing (8.1 `denied`), never a run note
			d.postFixNote(ctx, base, d.fixRun, fixDenied(base, dec.Reason, dec.Detail), true)
			return 0
		}
		if base.PipelineID != 0 { // the run note, never the issue status note
			base = d.withPipelineProjectURL(ctx, base) // for the note's link only (gonk-385g)
			d.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDenied,
				Status: deniedStatus(base, dec.Reason, dec.Detail)})
			return 0
		}
		// Tell the human who is now explicitly needed (gonk-wnz6). Only AFTER the
		// record is written: the record is the truth, the notice is courtesy.
		d.postDeniedNotice(ctx, a, dec.Reason, dec.Detail)
		return 0

	case meterapi.DecisionRun:
		// fall through

	default:
		d.Log.Error("meter returned an unknown decision; pouring nothing", "decision", dec.Decision)
		return 1
	}

	// `pipeline_fix.max_attempts` (gonk-2usw plan 3.4): meter's own ladder
	// decided run, but the project capped the attempts of ONE fix, because
	// each attempt also costs it a pipeline. Meter does not know that cap; the
	// fix record does. See capFixAttempts.
	if isFix && base.PipelineFix.MaxAttempts > 0 && dec.Attempt > base.PipelineFix.MaxAttempts {
		return capFixAttempts(ctx, d, base, decided, *dec)
	}

	// ---- v2 broker path: the ONLY dispatch path left (ADR-007 §3 deleted the
	// formula pour). It starts the agent session through the SessionRuntime
	// (gonk-t55) under a deterministic name recorded on the bead. The agent
	// holds no forge creds and posts nothing; gonk-sweep validates + applies
	// its proposed-effects batch.
	//
	// The session's project key is resolved inside runBrokerDispatch via
	// resolveLiteLLMKey and MOUNTED into the session (runtime.Spec.LiteLLMKey)
	// -- never env. There is no vars map here and no bot token: those were the
	// v1-only formula-pour compromise (a credential in an order var is a
	// credential in the event bus and in every log line that echoes an order),
	// and the broker path keeps every credential out of the pod's env by
	// construction.
	return runBrokerDispatch(ctx, d, agent, *dec, base)
}

// decidedOnRecord is what runDispatch read BEFORE /decide: the record meter's
// defer or deny was decided on (or that it was absent, or unreadable).
type decidedOnRecord struct {
	rec   beadstore.Record
	found bool
	err   error
}

func (r decidedOnRecord) revision() string {
	if r.err != nil || !r.found {
		return ""
	}
	return r.rec.Revision
}

// writeDecided writes a park or a deny ONLY over the record meter decided on:
// a PutIf against the revision runDispatch read before /decide (gonk-b2jk).
// written=false means dispatch must stop now and exit with code.
//
// ErrStale: the record moved after it was read, while meter was deciding or
// this write was in flight. writeDecided re-reads it and asks one question:
// has anything happened to this bead that meter's decision did not account
// for? (supersededBy)
//
//   - NO -- write again, against the record just read. That is the case when
//     the newer record is:
//   - State "": what a FAILED dispatch's release writes when the bead had no
//     prior record. Nobody is acting on it, and no sweep List returns it;
//     stepping aside strands the bead (gonk-k3qy).
//   - `done`: a finished attempt, whether the sweep just wrote it or a failed
//     dispatch's release put it back. Nothing lists it and nothing refuses
//     new work over it -- the run path's reservation overwrites it too -- so
//     a new trigger on a finished issue parks over it, as main's
//     unconditional Put did. Stepping aside drops the new trigger for good.
//   - CONTENT-EQUAL to the record meter decided on: every field gonk keeps
//     (SessionID and Attempt included) equal to the record this dispatch read
//     before /decide; only the store-stamped Revision and UpdatedAt differ.
//     The check does not establish that the newer record IS a restore -- it
//     cannot see who wrote it. It does not need to. The usual writer is a
//     failed dispatch's release putting back the prior record it read, but
//     any write of identical content qualifies (e.g. a dispatch's own running
//     Put rewriting what the sweep had already promoted). Either way meter
//     decided on exactly this content, and nothing gonk can read has changed
//     since; the outcome is the one had this dispatch read the record after
//     that write. A NEW claim always differs in content: a fresh reservation
//     carries a new SessionID (128-bit nonce) or attempt, and a later deny
//     its own attempt. This is what makes a new trigger on a
//     DENIED issue (needs-human) park rather than be dropped when a concurrent
//     dispatch fails: meter was asked about that needs-human record and said
//     defer, not deny, and the run path would equally have overwritten it. One
//     case this cannot tell apart: a SECOND deny, written by another dispatch
//     with the same fields. Two meter decisions made on the same record then
//     disagree, and the last write wins -- which is what main did for both.
//   - YES -- step aside. Any other newer record is a writer acting on the bead:
//     a dispatch that reserved (it owns the attempt; its reservation is exactly
//     what must survive), another dispatch's park or deny made on a record
//     other than the one this dispatch read (a park must not undo a deny), or
//     a gonk-sweep write that fires the next dispatch. The newer record is left
//     in place, dispatch logs it, exits 0 and posts no deferred notice.
//
// THE RETRIES ARE BOUNDED AT maxDecidedRetries (two; three writes in all, as
// reservePendingPrompt's maxReserveTries). This is a choice made on rarity, not
// a proof that the bound is enough. Every further stale needs yet another
// write between this dispatch's re-read and its write, so no bound can
// livelock; and past the bound the bead is left wherever that last writer left
// it -- which, if the last writer was a failed dispatch's release, is stranded,
// exactly the k3qy outcome one level deeper. Two retries cover a release
// followed by the sweep's `done`, or two failed dispatches; a third would need
// a fourth writer inside the same few milliseconds.
//
// If the RE-READ fails, dispatch steps aside without knowing what the newer
// record is. It says so: the bead may have been left in a state no sweep picks
// up, and this dispatch cannot tell.
//
// An unread record (d.err != nil) is written only if there is none at all
// (PutIf against ""), as reservePendingPrompt does: a record this dispatch
// never saw is never overwritten. If one exists, exit 1. What happens to the
// bead then depends on that record, which this dispatch never saw: a parked,
// running or pending-prompt record is picked up by the sweep, but a done,
// needs-human or State "" record is not, and this trigger is lost unless it
// is fired again.
//
// THE GUARANTEE IS POSTGRES'S. On the Postgres work store PutIf is a real
// compare-and-swap. On bd (BdCLI -- the store whenever GONK_WORKSTORE_DSN_FILE
// is unset, which is the chart default, workstore.enabled: false), PutIf is
// not: PutIf(r, "") on a bead with no record lists-then-creates, so two
// writers can both be told "applied", and conditional writes can lose after
// being told they won (gonk-vred). This path uses PutIf "" for a first-time
// bead and after a failed read. On bd, then, this narrows the window rather
// than closing it (docs/reference/gonk-state-record.md, "Writes").
func writeDecided(ctx context.Context, d dispatchDeps, rec beadstore.Record, decided decidedOnRecord) (code int, written bool) {
	a := d.Args
	ifRevision := decided.revision()
	for retry := 0; ; retry++ {
		_, err := d.Store.PutIf(ctx, rec, ifRevision)
		switch {
		case err == nil:
			return 0, true
		case errors.Is(err, beadstore.ErrStale) && decided.err != nil:
			d.Log.Error("not writing the "+string(rec.State)+" record: the bead store could not be read before "+
				"meter decided and a record exists, which this dispatch will not overwrite unseen; if that record "+
				"is done, needs-human or has no state, no sweep picks this bead up and this trigger is lost",
				"err", err, "read_err", decided.err, "bead", a.BeadAnchor)
			return 1, false
		case errors.Is(err, beadstore.ErrStale):
			if retry >= maxDecidedRetries {
				d.Log.Warn("not writing the "+string(rec.State)+" record: the record kept changing after this "+
					"dispatch read it; leaving the newest record in place (gonk-b2jk)",
					"err", err, "bead", a.BeadAnchor, "attempt", rec.Attempt, "retries", retry)
				return 0, false
			}
			cur, found, gerr := d.Store.Get(ctx, a.BeadAnchor)
			if gerr != nil {
				d.Log.Warn("not writing the "+string(rec.State)+" record: the record changed after this dispatch "+
					"read it, and re-reading it failed, so what replaced it is unknown; if that record is done, "+
					"needs-human or has no state, no sweep picks this bead up", "err", err, "read_err", gerr,
					"bead", a.BeadAnchor)
				return 0, false
			}
			if !found || supersededBy(decided, cur) {
				newer := "a " + string(cur.State) + " record another writer is acting on"
				if !found {
					newer = "no record at all, which this dispatch will not guess at"
				}
				d.Log.Warn("not writing the "+string(rec.State)+" record: the record changed after this dispatch "+
					"read it, before meter's decision could be written, to "+newer+"; the newer record is left "+
					"in place (gonk-b2jk)",
					"err", err, "bead", a.BeadAnchor, "attempt", rec.Attempt, "newer_session", cur.SessionID)
				return 0, false
			}
			d.Log.Info("the record changed after this dispatch read it, but to one meter's decision already "+
				"covers (state "+strconv.Quote(string(cur.State))+"); writing the "+string(rec.State)+
				" record over it (gonk-k3qy)", "bead", a.BeadAnchor, "retry", retry+1)
			ifRevision = cur.Revision
		default:
			d.Log.Error("bead store PutIf failed", "err", err, "bead", a.BeadAnchor, "state", string(rec.State))
			return 1, false
		}
	}
}

// maxDecidedRetries bounds writeDecided's re-writes after ErrStale. See
// writeDecided: a choice made on rarity, not a proof.
const maxDecidedRetries = 2

// supersededBy reports whether cur -- the record that replaced the one runDispatch
// read before /decide -- carries something meter's defer/deny did not account
// for. See writeDecided for each case.
func supersededBy(decided decidedOnRecord, cur beadstore.Record) bool {
	switch cur.State {
	case "", beadstore.StateDone:
		return false
	}
	if decided.found && sameRecordContent(decided.rec, cur) {
		return false
	}
	return true
}

// sameRecordContent reports whether two records are CONTENT-EQUAL: every field
// equal except the two the store stamps on every write (Revision, UpdatedAt).
// SessionID is compared like any other field, and must be: a fresh reservation
// that differs from the decided-on record only in its session id is a new
// claim (TestAParkDoesNotTakeAFreshReservationWithTheSameFieldsForTheOneItDecidedOn).
func sameRecordContent(x, y beadstore.Record) bool {
	x.Revision, y.Revision = "", ""
	x.UpdatedAt, y.UpdatedAt = time.Time{}, time.Time{}
	bx, errx := json.Marshal(x)
	by, erry := json.Marshal(y)
	return errx == nil && erry == nil && bytes.Equal(bx, by)
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

// postDeniedNotice tells the issue a human is needed: it applies the same
// gonk::denied label intake applies on a Gate-1 deny and posts-or-edits the ONE
// status note (the defer path's mechanism, same marker), so defer->deny and a
// repeated deny leave a single current note, and the label add is idempotent.
//
// BEST EFFORT, like postDeferredNotice: the needs-human record is already
// written, and a GitLab failure here is logged at WARN and swallowed (the
// notice helpers log their own failures). It runs under the dispatch ctx, which
// carries the same order budget and signal handling as the defer path.
//
// Only issue-triggered beads have somewhere to write: a bead with no project
// id or issue iid (scaffold, an MR-anchored mention) has no issue thread, so it
// gets a log line and nothing else; the needs-human record is its only signal.
func (d dispatchDeps) postDeniedNotice(ctx context.Context, a dispatchArgs, reason, detail string) {
	if d.Apply == nil {
		return
	}
	if a.ProjectID == 0 || a.IssueIID == 0 {
		d.Log.Warn("bead denied but has no issue to tell: needs-human record only",
			"bead", a.BeadAnchor, "trigger", a.Trigger)
		return
	}
	sd := sweepDeps{Apply: d.Apply, GL: d.GL, BotUsername: d.BotUsername, Log: d.Log}
	upsertStatusNote(ctx, sd, a.ProjectID, a.IssueIID, a.BeadID, deniedBody(reason, detail, d.BotUsername))
	// The same label, and the same fixed prefix, intake writes on a Gate-1 deny
	// (intake.DefaultDenyLabel is a literal; intake does not use the configured
	// triage label_prefix for it).
	if err := d.Apply.AddIssueLabel(ctx, a.ProjectID, a.IssueIID, intake.DefaultDenyLabel); err != nil {
		d.Log.Warn("deny label add failed", "project_id", a.ProjectID, "issue", a.IssueIID,
			"label", intake.DefaultDenyLabel, "err", err)
	}
}

// fireByVar is the order var gonk-sweep passes a re-sling's start deadline in.
// Declared in pack/orders/gonk-dispatch.toml.
const fireByVar = "fire_by"

// refuseLateFire enforces a re-sling's start deadline (gonk-p8w8). It runs
// before /decide, before any store read or write and before any session call,
// so a refused dispatch has had no effect at all: no meter reservation, no
// pending-prompt reservation (and so no release), no session, no park or deny
// record.
//
// WHY THE SWEEP NEEDS IT. gonk-sweep will not judge a re-slung record again
// until every dispatch the re-sling fired is dead, and it can only know that if
// no such dispatch can have started late. Gas City cannot promise when it fires:
// its order-run handler is detached from the client and writes a tracking bead
// before firing, with no server-side bound, and gcapi retries a POST that timed
// out client-side, so one re-sling can fire late or twice. This check turns
// "late" into "does nothing". The window arithmetic is in reslingWindowEnd
// (sweep.go).
//
// The comparison is against the clock NOW, which is after Gas City started this
// order's timeout (it does so before forking the process) and after process
// start, so "now <= fire_by" implies the timeout started by fire_by. It is the
// same wall clock the sweep stamped the deadline with: both are processes in the
// controller container.
//
// The order budget (order_budget.go) is untouched: it is measured from process
// start against the order timeout, and a dispatch allowed through here has
// exactly the budget it would have had without the check.
//
// Exit 0 for a refusal: it is the designed outcome of a late fire, not an infra
// failure. Exit 2 for a deadline that does not parse, which only a bug in the
// sweep can produce; the dispatch still writes nothing, and the sweep re-slings
// the record when its window ends.
func refuseLateFire(d dispatchDeps) (code int, refuse bool) {
	a := d.Args
	if a.FireBy == "" {
		return 0, false
	}
	fireBy, err := time.Parse(time.RFC3339Nano, a.FireBy)
	if err != nil {
		d.Log.Error("refusing to dispatch: the re-sling's fire_by deadline does not parse; writing nothing",
			"fire_by", a.FireBy, "err", err, "bead", a.BeadAnchor)
		return 2, true
	}
	if now := d.clock().Now(); now.After(fireBy) {
		d.Log.Warn("this re-sling's dispatch started after its fire_by deadline; exiting without writing anything "+
			"(gonk-sweep re-slings the bead when its window ends)",
			"fire_by", fireBy, "late_by", now.Sub(fireBy).String(), "bead", a.BeadAnchor, "trigger", a.Trigger)
		return 0, true
	}
	return 0, false
}
