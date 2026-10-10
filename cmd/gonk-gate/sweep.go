package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/gate"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// dispatchOrderName is the exec order that wraps `gonk-gate dispatch` (Gate
// 2). Re-firing it -- not calling runDispatch in-process -- is what makes the
// re-sling and the unpark go through the SAME re-decide path a first dispatch
// does: Gas City runs a fresh `gonk-gate dispatch` process, which asks meter
// /v1/policy/decide again. There is exactly one other RunOrder call site in
// this tree (dispatch.go, the pour); this is not a second pour path because
// "gonk-dispatch" is never a formula-order.
const dispatchOrderName = "gonk-dispatch"

// orderRunner fires one of gonk's own exec orders -- in production, Gas City's
// order-run route (gcapi.Client.RunOrder, adapted in main.go).
//
// IT IS NOT THE SESSION RUNTIME, and that is why it is its own interface
// (gonk-t55 plan D8). Gas City is still gonk-gate's SCHEDULER until T-57:
// gonk-dispatch and gonk-sweep are Gas City exec orders, and a re-sling or an
// unpark is "run gonk-dispatch again". That has nothing to do with where agent
// sessions run. Putting it behind this one-method interface is what keeps the
// Gas City client package out of every dispatch and sweep file, while main.go
// -- the stated exception -- wires that client in.
type orderRunner interface {
	RunOrder(ctx context.Context, name string, vars map[string]string) error
}

// sweepDeps is gonk-sweep's dependency set. Every field has a safe default
// applied in runSweep so a caller only needs to set what a given test cares
// about.
type sweepDeps struct {
	Meter *meterAPI
	// Runtime is where the agent sessions live (gonk-t55): the sweep asks it
	// whether a session has finished, reads its output, and stops it. Nothing
	// else in the sweep names a Job or a Gas City alias.
	Runtime runtime.SessionRuntime
	// Orders re-fires gonk-dispatch for a re-sling or an unpark (plan D8).
	Orders orderRunner
	GL     gitlabQuerier
	Store  beadstore.Store
	Log    *slog.Logger

	// LegacyPods finds and deletes the Gas City pods of sessions the old
	// controller started before the cutover (sweep_legacy.go, plan 3.4 C1),
	// by their gc-agent=<alias> label. It is the reaper's pod lister
	// (reap_pods.go, Role gonk-gate-podreap: pods list/delete), armed by the
	// same GONK_POD_REAP_NAMESPACE. Nil: no legacy pod is deleted (said once
	// per legacy park).
	LegacyPods podLister
	// LegacyCloser closes the Gas City session of a legacy alias before its
	// pod is deleted, so Gas City does not restart it (sweep_legacy.go). An
	// interface implemented in main.go by the gcapi client, like Orders, so
	// no sweep file imports the Gas City client package. Nil: not closed (said per park).
	LegacyCloser legacySessionCloser

	// THERE IS NO GAS CITY REAPER HERE ANY MORE (gonk-t55 plan D7). runReap's
	// work is LISTING Gas City's sessions and closing the ones no record
	// claims, and the sweep holds no Gas City client by design (exit criterion
	// 2; only the one-method Orders and LegacyCloser interfaces); there are
	// no Gas City sessions left to reap once dispatch starts Jobs, except the
	// ones in flight at the cutover, which the legacy park above handles one
	// by one. (Its pod half would NOT have touched Job pods: it acts only on
	// pods whose gc-agent label is an old-style alias, and Job pods carry no
	// gc-agent label.) Orphan Jobs are the Job's own business
	// (activeDeadlineSeconds + ttlSecondsAfterFinished), with
	// reapStaleSessions as the backstop. reap.go stays in the tree,
	// unreferenced, until T-56 deletes it; TestSweepNeverRunsTheGasCityReaper
	// walks runSweep's call graph to keep it off this path.
	//
	// Nor a checkout revoke. The rig grant is keyed by a nonce only the
	// session itself was handed (grantCheckout), so the sweep cannot name it;
	// it expires on its own TTL, and Stop deletes the only pod holding it.

	// Apply is the WRITE side of pkg/glab the v2 broker uses to apply a
	// validated proposed-effects batch under the bot PAT. Only the broker path
	// (a running record with a SessionID for a ported trigger) uses it.
	// Satisfied by *glab.Client.
	Apply brokerApplier
	// Pipelines is the pipeline path's GitLab (gonk-tios): the reads its
	// validation chain makes (traces, cited files), the run note's writes and
	// the run maintenance (sweepPipelineRuns). Nil: a pipeline batch is an
	// UNKNOWN (retry) and no run is maintained. Satisfied by *glab.Client.
	Pipelines pipelineForge
	// PackDir is the baked pack root (/opt/gonk/pack) the broker reads
	// effect-shape.toml from. Defaulted in withDefaults.
	PackDir string

	// FixesLive turns on the pipeline-fix dispatch pass (gonk-2usw,
	// sweep_pipefix.go): after a diagnosed run is maintained, decide whether
	// a fix may be dispatched and fire its order. FALSE MEANS THE PASS DOES
	// NOT EXIST for this sweep: no read, no write, no order. main.go sets it
	// only when a fix session can be prepared (pipefixPrepare). Even when
	// set, a project that has not opted in (actions.pipeline_fixes) costs one
	// read of its meter registration per look and nothing else.
	FixesLive bool
	// FixConfig answers a project's effective configuration for that pass.
	// Nil means gonk-meter (Meter.Project); tests pass a fake.
	FixConfig pipefixConfigSource
	// FixHold is plan 3.2's row E12, the correlation hold: it reports whether
	// a run is linked into a roll-up whose common cause is outside its
	// project. Nil -- production, until gonk-lreg exists -- holds nothing.
	FixHold func(ctx context.Context, run beadstore.Record) bool
	// FixSecrets are the values the controller holds that must never appear
	// in a fix's added lines (plan 4.3 SC-SECRET, exact match): the bot's
	// GitLab token and the meter bearer, set by main.go. The high-confidence
	// key SHAPES are checked whatever this holds.
	FixSecrets []effects.Secret

	// TranscriptDir turns on the DEV-ONLY session transcript archive
	// (GONK_TRANSCRIPT_DIR; see transcript_archive.go). EMPTY MEANS OFF, and
	// that is the production default -- the archive holds untrusted model
	// output over untrusted user input, so it has to be asked for by name. The
	// always-on `transcript read` metadata record does not depend on it.
	TranscriptDir string

	// EnforceTrajectory turns the fifth gate from observing into rejecting
	// (gonk-hsb). OFF by default and deliberately so: a predicate enabled on
	// unmeasured evidence rejects honest batches, and a rejected batch on the
	// triage path re-slings the bead onto a pricier rung. Turn it on when the
	// false-positive rate has been measured on real sessions, not before.
	EnforceTrajectory bool

	// BotUsername authenticates the marker-carrying comment (a human quoting
	// the marker must not satisfy the gate).
	BotUsername string

	// HealthFile is where every pass records whether the outcome path is alive
	// (GONK_SWEEP_HEALTH_FILE; see sweep_health.go). It is what the controller's
	// readiness probe reads, and it is why a sweep that cannot reach its store
	// shows up as `0/1 READY` rather than as an ERROR line nobody reads.
	// cmd/gonk-gate/main.go always sets it (sweepHealthFile() never returns
	// ""); "" disables the record, which is what a unit test wants.
	HealthFile string

	// LockFile is the single-pass lease (GONK_SWEEP_LOCK_FILE; see
	// sweep_lease.go). A pass that finds it held skips the tick. cmd/gonk-gate/
	// main.go always sets it; "" disables it, which is what a unit test wants.
	LockFile string

	// Now is the sweeper's clock. Defaults to time.Now.
	Now func() time.Time
	// SpendPollInterval/SpendDeadline bound the "wait for meter's spend view
	// to catch up" loop (HB-2). Production defaults are 1s/60s; tests set
	// both short so a stale-spend test does not sleep for a minute.
	SpendPollInterval time.Duration
	SpendDeadline     time.Duration
}

func (d *sweepDeps) withDefaults() sweepDeps {
	out := *d
	if out.Log == nil {
		out.Log = slog.Default()
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	if out.SpendPollInterval <= 0 {
		out.SpendPollInterval = time.Second
	}
	if out.SpendDeadline <= 0 {
		out.SpendDeadline = 60 * time.Second
	}
	if out.PackDir == "" {
		out.PackDir = "/opt/gonk/pack"
	}
	return out
}

// runSweep is `gonk-sweep`'s body: the cooldown exec order that runs every
// 30s and, deterministically, (1) classifies every `running` bead whose
// session has ended, reports the outcome, and re-slings an escalation or
// retry; (2) unparks every `parked` bead whose RetryAfter has passed. NO
// MODEL CALL ANYWHERE.
//
// Exit codes: 0 = the pass completed (individual beads may have hit infra
// errors that are logged and skipped -- one bad bead must not block the
// whole sweep); 1 = the pass could not even start (store unreachable).
func runSweep(ctx context.Context, d sweepDeps) int {
	dd := d.withDefaults()

	// ONE PASS AT A TIME (gonk-p8w8; sweep_lease.go says why Gas City does not
	// guarantee it). A refused pass is not a failed one: exit 0 and leave the
	// health record to the pass that is actually running.
	release, err := acquireSweepLease(dd.LockFile, dd.Now())
	switch {
	case errors.Is(err, errSweepLeaseHeld):
		dd.Log.Warn("sweep: another pass is still running; skipping this tick rather than sweeping the same beads twice",
			"lease", dd.LockFile, "holder", err.Error())
		return 0
	case err != nil:
		// Run anyway. A sweep that stops sweeping because of its own lock file
		// is a dead outcome path (gonk-p7qh), while an unguarded pass is still
		// protected where it matters: every store write below is conditional
		// on the revision this pass read.
		dd.Log.Error("sweep: could not take the single-pass lease; running unguarded",
			"lease", dd.LockFile, "err", err)
	default:
		defer release()
	}

	// A SWEEP THAT CANNOT LIST BEADS IS NOT A DEGRADED SWEEP, IT IS A DEAD
	// OUTCOME PATH (gonk-p7qh). Every `return 1` below records the outage and
	// escalates; the pass that succeeds clears it. sweep_health.go argues why
	// the signal is readiness rather than a metric or a louder log line.
	running, err := dd.Store.List(ctx, beadstore.StateRunning)
	if err != nil {
		recordSweepFailure(dd.Log, dd.HealthFile, "list_running", dd.Now(), err)
		return 1
	}
	for _, rec := range running {
		sweepRunning(ctx, dd, rec)
	}
	// Pipeline runs whose diagnosis is done (or parked, or denied): resolved
	// when the target goes green, superseded by a different failure (gonk-tios
	// D9). Before the parked pass, so a parked run that has resolved is retired
	// rather than unparked into a diagnosis nobody needs. Zero tokens.
	sweepPipelineRuns(ctx, dd)

	parked, err := dd.Store.List(ctx, beadstore.StateParked)
	if err != nil {
		recordSweepFailure(dd.Log, dd.HealthFile, "list_parked", dd.Now(), err)
		return 1
	}
	now := dd.Now()
	for _, rec := range parked {
		if now.Before(rec.RetryAfter) {
			continue
		}
		refire(ctx, dd, rec)
	}

	// THE THIRD PASS, and the reason StatePendingPrompt is not a state the
	// sweep can afford to ignore (T-19). See reclaimPendingPrompt.
	pending, err := dd.Store.List(ctx, beadstore.StatePendingPrompt)
	if err != nil {
		recordSweepFailure(dd.Log, dd.HealthFile, "list_pending_prompt", dd.Now(), err)
		return 1
	}
	for _, rec := range pending {
		reclaimPendingPrompt(ctx, dd, rec, now)
	}

	// LAST, and deliberately after all three passes above: the stale-session
	// backstop decides what to stop by ABSENCE -- a session no record claims
	// -- so it runs once this tick has made the store's picture of what is
	// running as accurate as it is going to get. It reports nothing and cannot
	// fail the sweep. See reapStaleSessions for why it is NOT the old reaper.
	reapStaleSessions(ctx, dd)
	// And the Gas City pods of sessions from before the cutover, once no
	// record claims them -- after the running pass has parked them (plan 3.4
	// C1, sweep_legacy.go).
	reapLegacyGasCityPods(ctx, dd)

	// The pass completed. Clear any failure run and say so once, if it is news.
	recordSweepSuccess(dd.Log, dd.HealthFile, dd.Now())
	return 0
}

// staleSessionAfter is how long after it started -- or, once finished, after
// it finished -- a session NO record claims is left alone before
// reapStaleSessions stops it.
//
// It is the default reservation TTL plus the Job's ttlSecondsAfterFinished
// (plan D6: 3600s). A session is bounded by its Job twice already: its
// activeDeadlineSeconds is its reservation's deadline, and its TTL deletes it
// an hour after it finishes. So an unclaimed session older than both has
// escaped both bounds -- a TTL controller that is not running, a deadline that
// was never set -- and nothing else will ever remove it. Anything younger is
// left to the Job, because the cost of being wrong is asymmetric exactly as it
// was for the Gas City reaper: a leaked pod costs money; a wrongly stopped one
// kills a live agent mid-turn.
const staleSessionAfter = opercfg.DefaultReservationTTL + time.Hour

// reapStaleSessions is the sweep's small List-based backstop for sessions the
// Job's own bounds did not remove (gonk-t55 plan D7).
//
// IT IS NOT runReap, AND runReap MUST NOT COME BACK ON THIS PATH. runReap
// closes Gas City sessions through the supervisor API, which the sweep does not
// hold (exit criterion 2), and gonk no longer creates Gas City sessions for it
// to find; the ones in flight at the cutover are sweep_legacy.go's. (It would
// not have deleted Job pods: its pod half matches only gonk-alias gc-agent
// labels, which Job pods lack.) This pass acts only through the
// SessionRuntime, only on sessions the runtime itself owns (List is
// label-selected), and only through Stop.
//
// THE BIAS IS TOWARD DOING NOTHING. A session is stopped only when ALL of:
// the store could be read (a failed read makes every session look unclaimed,
// so it stops the pass); no running or pending-prompt record names it (a
// pending-prompt record names the session its dispatch is about to start,
// which is how a just-started session is protected before its running record
// lands); the runtime can say how old it is (a session with no start time is
// treated as too young, as the reaper treated an unparseable created_at); and
// it is older than staleSessionAfter.
func reapStaleSessions(ctx context.Context, d sweepDeps) {
	claimed := map[runtime.Handle]struct{}{}
	for _, state := range []beadstore.State{beadstore.StateRunning, beadstore.StatePendingPrompt} {
		recs, err := d.Store.List(ctx, state)
		if err != nil {
			d.Log.Error("sweep: cannot read the bead store; stopping NO stale sessions this tick", "err", err)
			return
		}
		for _, rec := range recs {
			if rec.SessionID != "" {
				claimed[runtime.Handle(rec.SessionID)] = struct{}{}
			}
		}
	}
	handles, err := d.Runtime.List(ctx)
	if err != nil {
		d.Log.Warn("sweep: could not list sessions; stopping no stale sessions this tick", "err", err)
		return
	}
	now := d.Now()
	for _, h := range handles {
		if _, ok := claimed[h]; ok {
			continue
		}
		st, err := d.Runtime.Status(ctx, h)
		if err != nil || st.Phase == runtime.PhaseNotFound {
			continue // unreadable, or gone since the List: nothing to do
		}
		since := st.StartedAt
		if !st.FinishedAt.IsZero() {
			since = st.FinishedAt
		}
		if since.IsZero() {
			continue // unknown age reads as too young
		}
		age := now.Sub(since)
		if age < staleSessionAfter {
			continue
		}
		// A pipefix session's own Job deadline is the FIX reservation's,
		// which is longer by up to the verify-timeout ceiling: it has not
		// "outlived its Job's deadline" until that has passed too.
		if strings.HasPrefix(string(h), "gonk-"+pipefixAgent+"-") && age < staleFixSessionAfter { // stale:pipefix
			continue
		}
		if err := d.Runtime.Stop(ctx, h); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			d.Log.Error("sweep: could not stop a stale unclaimed session", "session", string(h), "err", err)
			continue
		}
		d.Log.Warn("sweep: stopped a STALE session no record claims -- it outlived its Job's own deadline and TTL",
			append([]any{"session", string(h), "age", age.Round(time.Second).String()}, sessionStatusAttrs(st)...)...)
	}
}

// pendingPromptGrace is how long a StatePendingPrompt reservation may sit
// before runSweep concludes no dispatch is behind it any more.
//
// It is dispatchOrderTimeout (order_budget.go), gonk-dispatch's own order
// timeout (pack/orders/gonk-dispatch.toml): the reservation is written after
// the order started, and Gas City SIGTERMs the process at the timeout, so a
// reservation older than this has no dispatch behind it that can still start a
// write: alw's dispatch starts none after SIGTERM, and SIGKILL follows
// gcKillGrace (2s) later. Strictly the process outlives the timeout by those
// 2s, and a write already IN FLIGHT at SIGTERM can land in them; alw's order
// budget (order_budget.go) is what keeps a write from being in flight then, not
// this grace. (It was
// defaultSubmitDeadline plus a minute, the same 300s by construction; since
// gonk-alw the dispatch is budgeted against the order timeout directly.)
//
// TestPendingPromptGraceCoversTheDispatchOrdersTimeout enforces the claim in
// this comment that the grace is at least the order timeout
// pack/orders/gonk-dispatch.toml actually declares.
//
// (It used to also have to stay comfortably under the Gas City reaper's
// grace, because that reaper treated only StateRunning records as claiming a
// session. The reaper is off the sweep path since gonk-t55 (plan D7), and the
// stale-session pass that replaced it counts a pending-prompt record as a
// claim, so a reservation's session is protected for as long as it sits here.)
const pendingPromptGrace = dispatchOrderTimeout

// reslingFireAllowance is how long after its re-sling write a re-slung
// gonk-dispatch may still START: the `fire_by` deadline is ReslungAt plus this.
//
// It must cover what lies between the stamp and the dispatch process starting:
// the conditional store write (~9 bd calls under BdCLI), the order-run POST
// including gcapi's retries, gc writing its tracking bead before it fires, and
// the fork/exec of gonk-gate. On 2026-09-13 stamp-to-202 was 85s with the old
// five-call Put (sweep B: stamp 15:50:11, 202 at 15:51:36), and gc's store
// writes alone took 12-57s. 240s is that observation with room for PutIf's
// extra reads.
//
// WHAT GETTING IT WRONG COSTS IS ASYMMETRIC, and that is why a generous value is
// right. Too SHORT refuses a slow but legitimate re-sling: the dispatch exits
// without writing, the record keeps its stamp, and the sweep re-judges and
// re-slings it again when the window ends -- a delay, never a duplicate. Too
// LONG only lengthens that window, which matters only when the re-sling's
// dispatch failed (a dispatch that succeeds replaces the record).
const reslingFireAllowance = 240 * time.Second

// gcKillGrace is Gas City's wait between an exec order's SIGTERM, at its
// timeout, and SIGKILL to the process group.
const gcKillGrace = 2 * time.Second

// wallClockMargin absorbs a wall-clock step between the sweep writing
// `fire_by` and the dispatch reading the clock. Both are processes in the same
// controller container, reading the same kernel CLOCK_REALTIME, so the only
// disagreement is an NTP step between the two readings.
const wallClockMargin = 5 * time.Second

// reslingFireBy is the deadline passed with a re-sling fired at stamp.
func reslingFireBy(stamp time.Time) time.Time { return stamp.Add(reslingFireAllowance) }

// reslingWindowEnd is when a re-slung record may be judged again. ok is false
// for a record with no re-sling stamp.
//
// THE ARGUMENT, ENFORCED END TO END. A dispatch fired by this re-sling either
// starts after ReslingFireBy -- and then exits before writing anything
// (refuseLateFire, dispatch.go) -- or starts at or before it. Gas City starts
// that dispatch's timeout before it forks the process (dispatchOne:
// context.WithTimeout, then exec), so its clock started no later than
// ReslingFireBy, and it was SIGTERMed by ReslingFireBy + dispatchOrderTimeout
// and SIGKILLed, with its process group, gcKillGrace later. So after
//
//	ReslingFireBy + dispatchOrderTimeout + gcKillGrace (+ wallClockMargin)
//
// no dispatch fired by this re-sling can be alive. None of that depends on
// when gc fired the order or how many POSTs it took: gc's order-run handler is
// detached from the client (context.WithoutCancel) and writes its tracking bead
// before firing, with no server-side bound, and gcapi retries a POST that timed
// out client-side -- so a POST can fire late, or twice (gonk-fjqq). A late one is
// refused at start; two on time are both inside this window and are the CAS
// reservation's to separate (gonk-y4m7).
//
// What it still assumes: that gc delivers its timeout kill (a Go timer in the
// gc process, not a store operation), and a wall clock that does not step by
// more than wallClockMargin between the two processes' readings.
//
// Why a window at all, and not a skip: re-judging a re-slung record whose
// dispatch failed and put it back is how that failed re-sling gets retried.
//
// A record carrying ReslungAt but no ReslingFireBy was written by a sweep that
// passed no deadline; only pre-release builds of this branch did, and none was
// deployed. It gets the same window from the deadline such a sweep would have
// passed.
func reslingWindowEnd(rec beadstore.Record) (time.Time, bool) {
	fireBy := rec.ReslingFireBy
	if fireBy.IsZero() {
		if rec.ReslungAt.IsZero() {
			return time.Time{}, false
		}
		fireBy = reslingFireBy(rec.ReslungAt)
	}
	return fireBy.Add(dispatchOrderTimeout + gcKillGrace + wallClockMargin), true
}

// reclaimPendingPrompt rescues a reservation whose dispatch never came back.
//
// runBrokerDispatch (broker_inject.go) writes a StatePendingPrompt record
// before it creates a session, so a re-dispatch landing inside the delivery
// window sees the reservation and backs off instead of minting a second
// session under the same alias (gonk-u6p). Its deferred release restores the
// prior record on every failure path -- but a dispatch that is KILLED runs no
// defer, and then the record sits in pending-prompt forever: the meter's
// reservation for that attempt is never settled (a slow budget leak) and the
// work item silently stops progressing, because nothing else in this tree
// looks at that state.
//
// Reclaiming means promoting the record to StateRunning with its alias and
// reservation intact, which hands it to sweepRunning -- the code that already
// knows how to settle exactly this: it reads the session, and past
// ReservationExpiresAt classifies a session that is unreadable or still
// grinding as infra-failed, reports the outcome to meter, and closes the pod.
// A wedged agent that never received its prompt is a case sweepRunning was
// written for; this pass is what lets it see one.
//
// THE BIAS IS TOWARD DOING NOTHING, like the reaper's. Promoting a record
// whose dispatch is still working would race that dispatch, so the age cutoff
// is generous and an unknown age is treated as too young.
func reclaimPendingPrompt(ctx context.Context, d sweepDeps, rec beadstore.Record, now time.Time) {
	if rec.UpdatedAt.IsZero() {
		// Unknown age reads as TOO YOUNG, for the same reason the reaper does
		// it with an unparseable created_at: the zero time would read as
		// "written in year 1, therefore ancient" and reclaim a reservation
		// written a second ago, out from under a dispatch that is mid-create.
		d.Log.Warn("sweep: pending-prompt record has no write time; leaving it alone",
			"bead", rec.BeadAnchor, "session", rec.SessionID)
		return
	}
	if age := now.Sub(rec.UpdatedAt); age < pendingPromptGrace {
		return // a dispatch may still be inside its delivery window
	}
	rec.State = beadstore.StateRunning
	// The Put restamps UpdatedAt: leaving StatePendingPrompt is a real change,
	// and beadstore's write-time rule only preserves the age of a record still
	// sitting in that state (stampWriteTime, pkg/beadstore/store.go). This
	// function does not stamp it itself -- the store owns the field, so no call
	// site has to remember to.
	//
	// CONDITIONAL on the revision this pass listed (gonk-p8w8). If the dispatch
	// behind the reservation came back after all -- confirmed its session, or
	// released the reservation -- promoting the listed copy would write a stale
	// pending alias over that dispatch's own record.
	if _, err := d.Store.PutIf(ctx, rec, rec.Revision); errors.Is(err, beadstore.ErrStale) {
		d.Log.Info("sweep: pending-prompt reservation changed after this pass listed it; not reclaiming",
			"bead", rec.BeadAnchor, "session", rec.SessionID)
		return
	} else if err != nil {
		d.Log.Error("sweep: could not reclaim a stranded pending-prompt reservation",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "err", err)
		return
	}
	// WARN, not Info: reaching here means a dispatch died without running its
	// own release, which is a real (if recoverable) failure and should not be
	// invisible just because the sweep handled it.
	d.Log.Warn("sweep: reclaimed a pending-prompt reservation whose dispatch never returned",
		"bead", rec.BeadAnchor, "session", rec.SessionID,
		"reserved_at", rec.UpdatedAt, "age", now.Sub(rec.UpdatedAt).String())
}

// runningDeadline is when a StateRunning record stops being waited on: its
// reservation's deadline, as meter minted it -- or, WHEN IT HAS NONE, its last
// write plus opercfg.DefaultReservationTTL (gonk-72np).
//
// "None" is reachable. Both StateRunning writers (broker_inject.go) copy the
// deadline from meter's DecideResponse, whose wire field is `omitzero`, so a
// `run` decision without reservation extras lands here with a zero one; and the
// T-41 import copies a bd record verbatim, quarantining only records hidden from
// bd's list or older than a day, so a fresh record without one imports as it
// stands. Whatever the writer, zero used to mean "never expires", and never is
// the one answer sweepRunning must not give.
//
// WHY THIS BOUND, rather than refusing the write. The last StateRunning write
// happens after the agent has FETCHED its prompt; refusing it there leaves a
// live pod no record describes, and refusing the decision earlier turns a
// meter contract slip into a bead that does no work at all. Neither reaches an
// import or a legacy record, and this does: it is enforced where the record is
// READ, so it covers every writer, present and future. UpdatedAt is the right
// base because the store stamps it on every Put (pkg/beadstore stampWriteTime),
// so it is the moment this record's current state began; the default TTL is
// the age at which meter, on its defaults, would have expired the reservation
// that should have come with the record. An operator who has RAISED
// reservation_ttl gets the default here, not theirs -- the sweep does not load
// the operator config -- which only ever applies to a record already outside
// meter's contract, and the verdict it leads to is infra-failed, which does not
// escalate.
//
// A record with neither a deadline nor a write time predates the store's stamp
// (every current Put sets it), so nothing bounds it by time at all: it returns
// the zero time, which every clock is after -- expired now, not never.
func runningDeadline(rec beadstore.Record) time.Time {
	if !rec.ReservationExpiresAt.IsZero() {
		return rec.ReservationExpiresAt
	}
	if rec.UpdatedAt.IsZero() {
		return time.Time{}
	}
	// A pipeline-fix reservation is reservation_ttl + verify_timeout at meter
	// (rung.ReservationTTL), so its fallback is too (fixReservationSlack).
	return rec.UpdatedAt.Add(opercfg.DefaultReservationTTL + fixReservationSlack(rec))
}

// reservationExpired: is now past rec's runningDeadline?
func reservationExpired(rec beadstore.Record, now time.Time) bool {
	return now.After(runningDeadline(rec))
}

// sweepRunning classifies one running bead. It is a no-op for a bead whose
// session has not ended yet (SessionEndedAt is the zero value): "still
// running" is not this tick's job.
func sweepRunning(ctx context.Context, d sweepDeps, rec beadstore.Record) {
	// A PIPELINE FIX WHOSE MERGE REQUEST IS OPEN IS NOT JUDGED BY THIS
	// FUNCTION (gonk-2usw). Its SESSION is: everything below reads the
	// finished session, and applyBrokerBatch hands its batch to
	// applyPipefixBatch (the static gates, the commit, the merge request).
	// But once that apply has set a fix state -- awaiting-run or verifying --
	// the session is over and closed, and what remains is the exit check on
	// the merge request's pipeline, which is part VERIFY's. Every path below
	// would judge such a record as "no session" and buy another one. See
	// sweepRunningFix.
	if rec.Trigger == atags.TriggerPipelineFix && rec.PipelineFix.State != "" {
		sweepRunningFix(ctx, d, rec)
		return
	}
	// Every store write below is conditional on the revision this pass READ
	// (gonk-p8w8). The record can be minutes old by the time it is written back
	// -- the judgement in between reads sessions, transcripts, GitLab and meter
	// -- and an unconditional write-back of it is how a sweep erased a
	// dispatch's pending-prompt reservation on 2026-09-13 and let a second
	// session through for gonk:75:issue:73 attempt 2.
	readRevision := rec.Revision

	// The v1 path has no session to consult, so it still waits for the stamp.
	// The BROKER path does not: nothing in this tree has ever stamped
	// SessionEndedAt (gonk-u1p.5), so gating on it skipped every broker bead
	// forever. The live SessionView is both available and more truthful -- it is
	// the session itself saying whether it is done.
	agent, isBroker := agentForTrigger[rec.Trigger]
	broker := isBroker && rec.SessionID != ""

	// The reservation is the DEADLINE, and it is computed BEFORE EVERY GUARD IN
	// THIS FUNCTION -- not just before the still-running check further down.
	// Declining to judge must never mean waiting forever. A wedged agent -- one
	// that never received its prompt, say -- would otherwise hold its
	// reservation to TTL and leave the bead in StateRunning permanently, with no
	// outcome ever reported.
	//
	// AND EVERY RUNNING RECORD HAS ONE (gonk-72np). This used to read a zero
	// ReservationExpiresAt as "never expires", which made a record with no
	// deadline exactly as permanent as gonk:63:scaffold. See runningDeadline.
	deadline := runningDeadline(rec)
	expired := reservationExpired(rec, d.Now())
	if expired && rec.ReservationExpiresAt.IsZero() {
		// Say so: meter's contract is that a `run` carries its reservation's
		// deadline, so reaching here means a writer or an import dropped it.
		//
		// HOW OFTEN THIS FIRES, precisely (an earlier comment said "at most once
		// per re-sling window", which the verification found untrue): after a
		// successful re-sling, about once per default TTL, because the re-sling
		// write re-stamps UpdatedAt. But if meter REJECTS the outcome -- as it
		// does for an unknown or empty ReservationID, the realistic shape of a
		// legacy v1 record -- or the write fails or is stale, nothing is
		// re-stamped and this fires on EVERY tick. That record is marked expired
		// and still never settles: gonk-72np does not close that case.
		d.Log.Warn("sweep: running record has no reservation deadline; bounded by its last write plus the default reservation TTL, and that has passed",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "updated_at", rec.UpdatedAt,
			"fallback_ttl", opercfg.DefaultReservationTTL, "deadline", deadline)
	}

	// A RECORD THAT IS NOT ON THE BROKER PATH STILL HAS TO BE ABLE TO STOP
	// (gonk-r4u9). This guard used to be `!broker && rec.SessionEndedAt.IsZero()`
	// alone, and that condition is PERMANENT for such a record: nothing in this
	// tree writes SessionEndedAt except the broker branch below (which the record
	// cannot reach) and the re-sling, which clears it. So the record was listed
	// by every 30s tick, returned from on every one, and never reached the
	// reservation deadline above. gonk:63:scaffold -- a v1 formula pour, which
	// wrote StateRunning with no SessionID at all -- sat in `running` for 49
	// days that way, until the T-41 import quarantined it.
	//
	// Two shapes land here, and both are permanent without this: a record with
	// no SessionID, and a record whose Trigger has since LEFT agentForTrigger
	// (ADR-007 §3 removed mention-reply; anything running under it at that
	// moment became unjudgeable, and the reaper will not close its session
	// either, because a StateRunning record claims it -- reap.go's
	// claimedSessionIDs).
	//
	// Past the reservation it falls through with view nil: no session is read,
	// ReservationExpired classifies it infra-failed -- which does NOT escalate
	// the rung (pkg/gate/outcome.go Escalates, and pkg/rung counts gate failures
	// only) -- and the re-sling's dispatch re-decides through Gate 2, so a bead
	// meter now denies settles as needs-human. Inside its reservation it still
	// waits, exactly as before.
	//
	// WHAT BOUNDS THE LOOP, precisely, because an earlier draft of this comment
	// claimed more than the code delivers (gonk-r4u9 verification, D1):
	// max_infra_retries bounds it ONLY along the path where the re-slung
	// dispatch actually reaches /decide, which is where the count is read
	// (pkg/rung/decide.go). RecordAttempt is idempotent per reservation_id and
	// previewNext returns "retry" for infra-failed unconditionally, so on a path
	// where the dispatch never reaches /decide -- Gas City refuses the order,
	// the meter is unreachable, refuseLateFire, a GitLab error before the gate
	// -- the attempt history never grows and the count stays at 1. Such a record
	// re-slings once per re-sling window, indefinitely. It is RATE-bounded
	// (~6.6/hour), not COUNT-bounded. That loop is not new -- broker records
	// have always had it -- but this change brings a class of record into it
	// that previously did nothing at all.
	if !broker && rec.SessionEndedAt.IsZero() && !expired {
		return
	}

	// ...WHICH LEFT THE BROKER PATH WITH NO "ALREADY JUDGED" MARKER AT ALL
	// (gonk-p8w8). The re-sling below writes this record back in StateRunning
	// under its old alias, which is indistinguishable from a session still
	// waiting to be judged, so the next tick -- before the dispatch it fired has
	// replaced the record -- judged the same session again, reported the outcome
	// again and fired a second dispatch. ReslungAt is that marker.
	//
	// It is a WINDOW, not a skip, because re-judging a re-slung record is also
	// how a failed re-sling gets retried: a dispatch that fails puts back the
	// record it found (runBrokerDispatch's release), and this sweep's next look
	// at it fires the next attempt.
	//
	// THE WINDOW IS ENFORCED, NOT ASSUMED: it ends after the kill of any
	// dispatch that could have started by the deadline the re-sling passed it
	// (`fire_by`); one starting later writes nothing. See reslingWindowEnd, which
	// also names the two things it does still assume.
	//
	// IT APPLIES TO EVERY RE-SLUNG RECORD, NOT ONLY A BROKER ONE (gonk-r4u9).
	// It used to be `broker && ok`, because a v1 record was bounded another way:
	// the re-sling cleared SessionEndedAt and the guard above then skipped it
	// until a dispatch replaced it. Now that an expired reservation gets past
	// that guard, that bound is gone -- and without this the sweep would
	// re-judge and re-fire such a record on EVERY 30s tick, which is walking the
	// ladder for free. ReslungAt is the marker for both paths.
	if end, ok := reslingWindowEnd(rec); ok {
		if now := d.Now(); now.Before(end) {
			d.Log.Info("sweep: outcome already reported and re-sling fired; waiting for its dispatch",
				"bead", rec.BeadAnchor, "session", rec.SessionID, "reslung_at", rec.ReslungAt,
				"resling_fire_by", rec.ReslingFireBy, "window_ends", end)
			return
		}
		d.Log.Warn("sweep: a re-sling has not replaced this record within the dispatch window; judging it again so the bead is not stranded",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "reslung_at", rec.ReslungAt,
			"resling_fire_by", rec.ReslingFireBy)
	}

	// judged is the FINISHED (or absent) session whose output the broker path
	// reads below; nil means no output is read at all -- the session was
	// stopped mid-flight past its reservation, could not be read, or hit its
	// deadline -- and the ReservationExpired signal settles Classify.
	//
	// THE WHOLE LIVENESS LADDER IS GONE (gonk-t55 plan D9). Under Gas City the
	// sweep had to infer "finished" from a TUI that never exited: a peek for a
	// closed fence, a transcript read when the peek was too large, a deep
	// capture when the fence had scrolled out of both (brokerSessionView,
	// gonk-5k5 / gonk-lv3y / gonk-xflt). A Job reports its own phase, so
	// finished is a fact the runtime states, and nothing here is ported.
	// A GAS CITY SESSION FROM BEFORE THE CUTOVER (plan 3.4 C1): the runtime
	// has never heard of its name and would answer NotFound, which the switch
	// below would judge absent and charge. It is parked, not judged -- see
	// sweep_legacy.go.
	//
	// IT STAYS BELOW THE RE-SLING WINDOW CHECK (plan 3.5 R2). A legacy record
	// with ReslungAt is one the OLD controller already judged -- outcome
	// reported, attempt charged -- and whose re-sling dispatch owns the bead
	// until the window ends. Intercepting first would park it inside that
	// window: erase the marker, log "not judged, not charged" for an attempt
	// that was both, and fire a second gonk-dispatch beside the re-sling's.
	// TestALegacyRecordInsideItsReslingWindowWaitsAndIsNotParked fails if the
	// two are swapped.
	if broker && isLegacyGasCitySession(rec.SessionID) {
		parkLegacySession(ctx, d, rec, readRevision, expired)
		return
	}

	var judged *runtime.Status
	// deadlineExceeded: the runtime killed the session at its deadline, which
	// is the reservation's (plan D6). Classified infra-failed, never read.
	deadlineExceeded := false
	if broker {
		st, err := d.Runtime.Status(ctx, runtime.Handle(rec.SessionID))
		if err == nil && st.Phase == runtime.PhaseUnknown {
			// No implementation returns Unknown with a nil error (pkg/runtime);
			// one that did must not read as "still starting, wait".
			err = fmt.Errorf("runtime reported no phase for session %q", rec.SessionID)
		}
		// IMAGE SKEW FIRST (plan 3.3, handshake item 3): a session the
		// entrypoint refused for a session-contract mismatch is not judged,
		// not reported and not an attempt -- see sweep_skew.go.
		if err == nil && isSkewExit(st) {
			switch parkIfImageSkew(ctx, d, rec, readRevision, st, expired) {
			case skewHandled, skewUnreadable:
				return
			case notSkew:
				// An opencode that exited 8 on its own: judged below as usual.
			}
		}
		switch {
		case err != nil && !expired:
			// Could not read the session: unknown. Try again next tick rather
			// than classify on no information. Still the right call INSIDE the
			// reservation -- a transient API-server error must buy a retry, not
			// a verdict on a session that is probably still working.
			d.Log.Warn("sweep: could not read broker session", "bead", rec.BeadAnchor,
				"session", rec.SessionID, "err", err)
			return
		case err != nil:
			// Unreadable AND past its deadline. The reservation is the deadline
			// for THIS too (gonk-u6p): "try again next tick" must not become a
			// promise that is never kept. Fall through with judged nil: no batch
			// is read, and ReservationExpired classifies it infra-failed, which
			// does not escalate the rung. A bead that cannot be judged must
			// still be able to STOP.
			d.Log.Warn("sweep: broker session unreadable past its reservation; classifying rather than retrying forever",
				"bead", rec.BeadAnchor, "session", rec.SessionID,
				"reservation_expired_at", deadline, "err", err)
		case st.Phase == runtime.PhaseNotFound:
			// NO SESSION EXISTS UNDER THIS NAME: it was never admitted, was
			// stopped, or its Job's TTL removed it. There is genuinely nothing
			// coming, so it is judged as having produced nothing -- a re-sling
			// is the right response. applyBrokerBatch's Output read reports it
			// as the "no session" violation.
			d.Log.Info("sweep: no session exists under this name; judging it as having produced nothing",
				"bead", rec.BeadAnchor, "session", rec.SessionID, "released_by", releasedByAbsent)
			judged = &st
		case !st.Phase.Finished() && !expired:
			// Still working, still inside its reservation. Nothing to judge and
			// nothing to report -- exactly like the v1 guard above. Reporting an
			// outcome here would re-sling a live session every tick. The phase
			// and reason ride along: this is the line an operator reads to see
			// a session stuck Pending on an ImagePullBackOff.
			d.Log.Info("sweep: session still running; nothing to judge yet",
				append([]any{"bead", rec.BeadAnchor, "session", rec.SessionID},
					sessionStatusAttrs(st)...)...)
			return
		case !st.Phase.Finished():
			// Running PAST its reservation: wedged, or its runtime's deadline has
			// not fired yet. Stop it. judged stays nil so no batch is read, and
			// ReservationExpired is what classifies it.
			d.Log.Warn("sweep: session still running past its reservation; stopping rather than waiting",
				append([]any{"bead", rec.BeadAnchor, "session", rec.SessionID,
					"reservation_expired_at", deadline}, sessionStatusAttrs(st)...)...)
		case st.Phase == runtime.PhaseFailed && st.Reason == reasonDeadlineExceeded:
			// KILLED AT ITS DEADLINE (bead gonk-t55 exit criterion 5). The
			// deadline is the reservation's, so this is the runtime saying what
			// ReservationExpired says -- possibly a tick before the sweep's own
			// clock does. It is infra-failed however the output looks: the
			// session did not finish its work inside what was reserved for it.
			deadlineExceeded = true
			d.Log.Warn("sweep: session was killed at its deadline; classifying infra-failed without reading its output",
				append([]any{"bead", rec.BeadAnchor, "session", rec.SessionID},
					sessionStatusAttrs(st)...)...)
			if rec.SessionEndedAt.IsZero() {
				rec.SessionEndedAt = d.Now()
			}
		default:
			// FINISHED: Succeeded or Failed. Neither is the outcome -- opencode
			// exits 0 after a rejected tool call (plan D9) -- so both are read,
			// and the batch decides.
			d.Log.Info("sweep: session finished; judging it",
				append([]any{"bead", rec.BeadAnchor, "session", rec.SessionID,
					"released_by", releasedByFinished}, sessionStatusAttrs(st)...)...)
			judged = &st
		}
		if judged != nil && rec.SessionEndedAt.IsZero() {
			// We have now OBSERVED the finish, which is what the stamp means. It
			// feeds gatherSpend's staleness comparison below.
			rec.SessionEndedAt = d.Now()
		}
	}

	signals := gate.Signals{ReservationExpired: expired || deadlineExceeded}
	// unknownBecause names what made the artifact UNKNOWN, for the
	// infra-failed line below (gonk-i391: an infra-failed verdict once denied
	// an issue with no logged cause at all).
	var unknownBecause string
	// rejectedBecause is the batch violation, kept for the re-sling's status
	// note (gonk-l900 item 2), which turns it into plain words.
	var rejectedBecause string

	kind := artifactKindForTrigger[rec.Trigger]
	// Aborted (a human closed the bead) is only meaningful for an issue-scoped
	// trigger: scaffold's work item is the PROJECT (spec 5.3 -- it runs before
	// any issue exists), so there is no issue to close.
	aborted := false
	if kind == "comment" {
		issue, err := d.GL.GetIssue(ctx, rec.ProjectID, rec.IssueIID)
		if err != nil {
			signals.ArtifactUnknown = true
			unknownBecause = "issue read failed: " + err.Error()
		} else if issue.State == "closed" {
			aborted = true
		}
	}
	signals.Aborted = aborted

	if !signals.ArtifactUnknown && !signals.Aborted {
		switch {
		case broker && judged == nil:
			// Stopped mid-flight, unreadable, or killed at its deadline above:
			// there is no finished session to read a batch from, and
			// ReservationExpired already settles Classify.
		case broker:
			// v2 broker: sweep itself reads+validates+applies the agent's batch,
			// so the apply RESULT is the artifact signal -- no re-read of GitLab.
			// (A running record with no SessionID is a pre-v2 bead; it falls to
			// the artifactPresent path below.)
			applied, violation, aerr := applyBrokerBatch(ctx, d, agent, &rec, *judged)
			switch {
			case aerr != nil:
				// Could not read the session or load our shape: an UNKNOWN.
				// Uncertainty must not escalate (AD-6) -- classify as such.
				signals.ArtifactUnknown = true
				unknownBecause = "batch read/apply error: " + aerr.Error()
				d.Log.Warn("sweep: broker batch read/apply error", "bead", rec.BeadAnchor, "err", aerr)
			case applied && agent == pipefixAgent && fixOpenStates[rec.PipelineFix.State]:
				// THE FIX MERGE REQUEST IS OPEN AND NOTHING HAS JUDGED IT
				// (plan 7.3: the attempt is judged in two phases). No outcome
				// is reported here -- reporting success would tell meter the
				// criterion was met, and it has not been looked at. The
				// record keeps its reservation and stays `running` with the
				// fix state the apply set; the session is finished with.
				holdFixForVerification(ctx, d, rec, readRevision)
				return
			case applied:
				signals.ArtifactPresent = true
				// Say so. Every OTHER outcome of this switch is logged, so a
				// silent success is the one case a reader cannot distinguish
				// from "the sweep never looked at this bead at all" -- which is
				// exactly the ambiguity gonk-zp3 is stuck in. A log that only
				// speaks up on failure cannot tell you the happy path ran.
				switch agent {
				case pipelineAgent:
					d.Log.Info("sweep: pipeline diagnosis applied", "bead", rec.BeadAnchor, "agent", agent)
				case pipefixAgent:
					d.Log.Info("sweep: pipeline fix settled without an open merge request", "bead", rec.BeadAnchor,
						"fix_state", rec.PipelineFix.State)
				default:
					d.Log.Info("sweep: triage batch applied", "bead", rec.BeadAnchor, "agent", agent)
				}
			default:
				// Present-but-invalid or no batch: nothing applied. The bead
				// falls to the ladder (spend>0 => escalate/needs-human; else
				// retry). Record the violation for observability.
				rejectedBecause = violation
				if violation != "" {
					d.Log.Warn("sweep: triage batch rejected, applied nothing",
						"bead", rec.BeadAnchor, "violation", violation)
				}
			}
		default:
			present, unknown, _ := artifactPresent(ctx, d.GL, d.BotUsername, kind, rec.ProjectID, rec.IssueIID, rec.BeadID)
			if unknown {
				signals.ArtifactUnknown = true
				unknownBecause = "artifact check could not read the forge"
			} else {
				signals.ArtifactPresent = present
			}
		}
	}

	// Only spend real effort proving tokens were spent when it can change the
	// answer: Aborted/ReservationExpired/ArtifactUnknown/ArtifactPresent all
	// already settle Classify's verdict without it (see gate.Classify's
	// priority order), and asking meter for spend it does not need is exactly
	// the trap that would misclassify a genuine success as infra-failed the
	// moment a spend sync happened to lag.
	if !signals.Aborted && !signals.ReservationExpired && !signals.ArtifactUnknown && !signals.ArtifactPresent {
		tokens, stale := gatherSpend(ctx, d, rec)
		signals.SpendStale = stale
		signals.ModelTokens = tokens
	}

	outcome := gate.Classify(signals)
	var infraCause string
	switch outcome {
	case meterapi.OutcomeInfraFailed:
		// EVERY infra-failed verdict names its cause (gonk-i391). It is the
		// verdict that spends max_infra_retries and ends in a deny, so it is
		// the one an operator most needs to be able to explain afterwards.
		infraCause = infraFailedCause(signals, expired, deadlineExceeded, unknownBecause)
		d.Log.Warn("sweep: classified infra-failed",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "cause", infraCause)
	case meterapi.OutcomeGateFailed:
		// The verdict that buys a rung names its cause too -- in particular a
		// session the loop watchdog stopped names the repeated call
		// (gonk-un7z, sweep_loop.go).
		d.Log.Warn("sweep: classified gate-failed",
			"bead", rec.BeadAnchor, "session", rec.SessionID,
			"cause", gateFailedCause(ctx, d, rec, judged))
	}

	// WHAT DID THIS SESSION ACTUALLY COST, AND HOW MANY MODEL CALLS DID IT
	// MAKE? (gonk-pop3 item 3.) Learning that one run made 6 calls and another
	// 9 previously required port-forwarding gonk-meter and authenticating to
	// GET /v1/cost/bead/<anchor> with a bearer token. The sweep already holds
	// that token and already talks to the meter, so the answer is one GET away
	// from a process whose logs `kubectl logs` reaches. Best effort, after the
	// verdict: it changes no classification and can fail without consequence.
	logSessionSpend(ctx, d, rec, outcome)

	// Bind the outcome to the reservation METER minted at the /decide that put
	// this record into StateRunning. An unbound outcome is a forgery vector: a
	// caller-synthesized reservation_id could "confirm" a gate-failed on a
	// reservation it never held.
	resp, err := d.Meter.Outcome(ctx, meterapi.OutcomeRequest{
		Project: rec.Project, BeadID: rec.BeadAnchor, SessionKey: rec.SessionKey,
		Attempt: rec.Attempt, Rung: rec.Rung, ReservationID: rec.ReservationID, Outcome: outcome,
		// Advisory: lets meter's Next/NextRung preview know the trigger's ladder
		// floor and floor retries, so a gate failure that re-runs the same rung
		// comes back as "retry", not "escalate" (gonk-tios R2).
		Trigger: rec.Trigger,
	})
	if err != nil {
		d.Log.Error("sweep: POST /v1/policy/outcome failed", "err", err, "bead", rec.BeadAnchor)
		return
	}

	// The verdict is in, so gonk is FINISHED WITH THIS SESSION on every branch
	// below -- including the re-slings, which open a fresh attempt-suffixed
	// session rather than reusing this one. Tear it down after the switch.
	//
	// Not before: the store writes below are what make the outcome durable, and
	// a close is a remote call that can hang. Not inside the switch either --
	// three copies of the same teardown is how one branch ends up missing it.
	// The `default` (unknown Next) deliberately leaves the session ALIVE: we did
	// not understand the answer, so we have not established that the work is
	// over.
	finished := false

	switch resp.Next {
	case "done":
		finished = true
		rec.State = beadstore.StateDone
		if _, err := d.Store.PutIf(ctx, rec, readRevision); errors.Is(err, beadstore.ErrStale) {
			d.Log.Warn("sweep: record changed after this pass read it; leaving the newer record in place",
				"bead", rec.BeadAnchor, "outcome", outcome, "next", resp.Next)
		} else if err != nil {
			d.Log.Error("sweep: store Put failed", "err", err, "bead", rec.BeadAnchor)
		}
		d.Log.Info("sweep: bead done", "bead", rec.BeadAnchor, "outcome", outcome)
	case "escalate", "retry":
		finished = true
		// The re-sling: fire the gonk-dispatch order for the SAME BeadAnchor. It
		// goes through Gate 2 by construction -- Gas City runs a fresh
		// `gonk-gate dispatch`, which re-decides and replaces this record with
		// its pending-prompt reservation, a compare-and-swap against the
		// revision it read (gonk-y4m7), once it runs.
		//
		// But that happens in a SEPARATE, asynchronous process: the order-run
		// route only QUEUES the order (spec/gcapi: {status, tracking_id}), it
		// does not run it inline. Until it does, this record is still sitting
		// here with the SAME SessionEndedAt that just earned an outcome. Clear
		// it NOW, in the same Put that records the re-sling: a bead with a zero
		// SessionEndedAt reads as "still running" (sweepRunning's own guard) and
		// is skipped by every sweep tick until the fresh dispatch (or its
		// eventual session) stamps a real one again. Without this, a cooldown
		// order firing every 30s would re-report this outcome and re-fire the
		// re-sling once per tick -- walking the ladder for free.
		rec.SessionEndedAt = time.Time{}
		// Stamp the re-sling itself, so the next tick knows this outcome is
		// already reported (see the window at the top of this function).
		//
		// And stamp the DEADLINE the re-sling's dispatch must start by, in the
		// same write, before anything is fired: the fired dispatch refuses to
		// run after it, which is what makes the window at the top of this
		// function hold (reslingWindowEnd). It is written before the POST so a
		// record can never carry a fired re-sling without its deadline.
		rec.ReslungAt = d.Now()
		rec.ReslingFireBy = reslingFireBy(rec.ReslungAt)
		// CONDITIONAL, AND A LOST PRECONDITION MEANS NO RE-FIRE. If the record
		// moved while this pass was judging it, another writer now owns it: most
		// likely the dispatch the previous re-sling fired, holding its
		// pending-prompt reservation for the next attempt. Re-firing on top of
		// that is the second session gonk-p8w8 is about. The session judged here
		// is still finished, so it is still closed below.
		_, err := d.Store.PutIf(ctx, rec, readRevision)
		if errors.Is(err, beadstore.ErrStale) {
			d.Log.Warn("sweep: record changed after this pass read it; not re-slinging over the newer record",
				"bead", rec.BeadAnchor, "session", rec.SessionID, "outcome", outcome, "next", resp.Next)
			break
		}
		if err != nil {
			// NO FIRE AFTER A FAILED WRITE, EITHER (gonk-aavs). This used to
			// re-fire anyway, "because refusing would strand the bead on a
			// transient write failure". But if the write did not land, the
			// record still reads as never judged, so the next tick judges it
			// again -- while the dispatch fired here may be alive, and if that
			// dispatch's reservation also failed, its dedupe is gone: two
			// sessions. Not firing strands nothing: a write that did not land
			// leaves the record judgeable on the next tick, and one that landed
			// despite the error carries its stamp and deadline, so it is judged
			// again when the window ends. Either way the re-sling is retried,
			// with no dispatch of this pass's alive to race it.
			d.Log.Error("sweep: could not record the re-sling; not firing its dispatch -- the bead is judged again, "+
				"on the next tick if the write did not land or when the re-sling window ends if it did",
				"err", err, "bead", rec.BeadAnchor, "outcome", outcome, "next", resp.Next)
			break
		}
		// Tell the issue, now that the re-sling record has landed and before
		// its dispatch is fired: fired first, that dispatch's "triaging now"
		// note could land before this one and be overwritten by it.
		postReslingNotice(ctx, d, rec, outcome, attemptFailureText(outcome, rejectedBecause, infraCause),
			resp.Next, resp.NextRung)
		if refire(ctx, d, rec) {
			d.Log.Info("sweep: re-sling fired", "bead", rec.BeadAnchor, "outcome", outcome, "next", resp.Next,
				"fire_by", rec.ReslingFireBy)
		}
	default:
		d.Log.Error("sweep: meter returned an unknown Next; leaving the bead running", "next", resp.Next, "bead", rec.BeadAnchor)
	}

	if finished {
		closeSession(ctx, d, rec)
	}
}

// postReslingNotice edits the issue's one status note to say the attempt just
// judged did not produce an answer, why, and that a retry is starting
// (gonk-l900 item 2). Only issue triage has a thread to write to.
//
// BEST EFFORT and bounded by statusNoteTimeout: a GitLab failure is a WARN
// (upsertStatusNote's), and the re-sling fires regardless.
func postReslingNotice(ctx context.Context, d sweepDeps, rec beadstore.Record, outcome, why, next, nextRung string) {
	if d.Apply == nil || agentForTrigger[rec.Trigger] != "triage" || rec.ProjectID == 0 || rec.IssueIID == 0 {
		return
	}
	nctx, cancel := context.WithTimeout(ctx, statusNoteTimeout)
	defer cancel()
	upsertStatusNote(nctx, d, rec.ProjectID, rec.IssueIID, rec.BeadID,
		reslingBody(rec.Attempt, rec.Rung, outcome, why, next, nextRung, d.BotUsername))
}

// closeSession stops the bead's session. It is the last thing that happens to
// a bead's session and what gives the cluster its CPU and memory back; the Job
// is deleted with its pod and its mounted Secret.
//
// BEST-EFFORT BY CONSTRUCTION, AND LOUD WHEN IT FAILS. The outcome is already
// reported and the bead has already moved on, so a teardown failure must not
// fail the sweep, re-judge the bead, or block the next one -- but it must never
// pass quietly either. A failed Stop leaves the session running until its
// Job's deadline.
//
// A NOT-FOUND IS "ALREADY GONE", AND UNDER JOBS THAT IS TRUE (ADR-008 section
// 7, gonk-wmb7 polarity). Under Gas City a 404 on close meant only "no OPEN
// session bead by that name" while a pod could run behind it (gonk-mfsr), so it
// was reported as a leaked pod at ERROR. A Job owns its pod: the runtime not
// knowing the session means the Job -- and with it the pod -- is gone, most
// often removed by its own TTL between the finish and this Stop. That is the
// desired state, logged at INFO so it is visible without being an alarm.
func closeSession(ctx context.Context, d sweepDeps, rec beadstore.Record) {
	// The v1 formula path never held a session handle: gonk did not create the
	// session, Gas City's pool did. Those beads are not this function's business.
	if rec.SessionID == "" {
		return
	}
	if err := d.Runtime.Stop(ctx, runtime.Handle(rec.SessionID)); err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			d.Log.Info("sweep: session already gone", "bead", rec.BeadAnchor, "session", rec.SessionID)
			return
		}
		// ERROR, not Warn: the session keeps its pod, and spends, until its
		// deadline.
		d.Log.Error("sweep: session stop failed -- it keeps running until its deadline",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "err", err)
		return
	}
	d.Log.Info("sweep: session stopped", "bead", rec.BeadAnchor, "session", rec.SessionID)
}

// reasonDeadlineExceeded is the Status.Reason of a session the runtime killed
// at its deadline: the Kubernetes Job Failed condition's own reason
// (batchv1.JobReasonDeadlineExceeded), which the Jobs runtime passes through
// (plan P2 item 1).
const reasonDeadlineExceeded = "DeadlineExceeded"

// infraFailedCause names the signal that made gate.Classify answer
// infra-failed, in Classify's own priority order, so the log line says what
// the verdict was actually made of.
func infraFailedCause(s gate.Signals, reservationExpired, deadlineExceeded bool, unknownBecause string) string {
	switch {
	case deadlineExceeded:
		return "session_deadline_exceeded"
	case reservationExpired:
		return "reservation_expired"
	case s.ArtifactUnknown:
		if unknownBecause == "" {
			return "artifact_unknown"
		}
		return "artifact_unknown: " + unknownBecause
	case s.SpendStale:
		return "spend_stale: meter's spend view did not catch up with the session's end"
	default:
		return "no_model_tokens: no artifact and meter recorded no model spend for this session"
	}
}

// gatherSpend forces a spend sync (HB-2) and polls session cost until
// meter's view has caught up past SessionEndedAt, or the deadline elapses.
// On deadline it reports SpendStale: we could not PROVE the model answered,
// so we do not escalate (AD-6: uncertainty never escalates).
//
// THE LOOP IS BOUNDED TWICE (gonk-yryi): by the clock, and by a poll count.
// The clock alone is not a bound when the clock is INJECTED: a frozen Now --
// the natural way for a test to pin an age or a deadline exactly -- never
// reaches `deadline`, and the loop never ended (a gonk-7k73 verifier lost a
// 500s go-test timeout to it). Each poll sleeps SpendPollInterval on the real
// runtime timer, so SpendDeadline/SpendPollInterval polls IS the deadline in
// real time; the clock check stays so a clock that runs ahead (a test that
// advances it per call) still ends the wait early, as before.
//
// Production cannot hit the hazard: Now is time.Now there, whose monotonic
// reading is the same clock time.After sleeps on, so it advances by at least
// the interval every poll. The count is what makes that true for EVERY Now,
// rather than for the one a caller happened to pass.
func gatherSpend(ctx context.Context, d sweepDeps, rec beadstore.Record) (tokens int64, stale bool) {
	if _, err := d.Meter.SpendSync(ctx); err != nil {
		return 0, true
	}
	deadline := d.Now().Add(d.SpendDeadline)
	maxPolls := spendPollBudget(d.SpendDeadline, d.SpendPollInterval)
	for poll := 1; ; poll++ {
		cost, err := d.Meter.CostSession(ctx, rec.SessionKey)
		if err == nil && !cost.AsOf.Before(rec.SessionEndedAt) {
			return cost.TotalTokens, false
		}
		if !d.Now().Before(deadline) || poll >= maxPolls {
			return 0, true
		}
		select {
		case <-ctx.Done():
			return 0, true
		case <-time.After(d.SpendPollInterval):
		}
	}
}

// spendPollBudget is how many CostSession reads gatherSpend makes before it
// gives up whatever the clock says: one up front, then one after each of the
// deadline/interval sleeps that fit in the deadline. A non-positive interval
// (withDefaults never leaves one, but gatherSpend does not rely on that) is
// one poll, so the answer is never "unbounded".
func spendPollBudget(deadline, interval time.Duration) int {
	if interval <= 0 || deadline <= 0 {
		return 1
	}
	return int(deadline/interval) + 1
}

// logSessionSpend emits the one record that makes a session's model usage
// legible without a port-forward: the call count, the prompt/completion token
// split, the cost, and the per-rung breakdown.
//
// IT IS DELIBERATELY NOT A CLASSIFICATION INPUT. gatherSpend above is the one
// that feeds gate.Classify, and it has to poll until meter's spend view has
// caught up with the session's end, because a stale read there would
// misclassify a genuine success as infra-failed. This one takes whatever meter
// has RIGHT NOW and says so -- `complete` and `as_of` are on the record -- so
// it costs one GET and can never block a sweep tick or change a verdict.
//
// Why here and not a log line per model turn: gonk does not own the model loop.
// opencode runs inside the agent pod, and a per-turn line would have to come
// out of the pod that the reaper is about to destroy -- which is the failure
// mode this whole bead exists to fix. The turn count on the `transcript read`
// record (transcript_archive.go) plus the call count here answer the same
// question from processes that survive.
func logSessionSpend(ctx context.Context, d sweepDeps, rec beadstore.Record, outcome string) {
	if d.Meter == nil || rec.SessionKey == "" {
		return
	}
	cost, err := d.Meter.CostSession(ctx, rec.SessionKey)
	if err != nil || cost == nil {
		d.Log.Warn("session spend: could not read the meter's cost view",
			"bead", rec.BeadAnchor, "session_key", rec.SessionKey, "err", err)
		return
	}

	calls := 0
	rungs := make([]string, 0, len(cost.ByRung))
	for _, r := range cost.ByRung {
		calls += r.Calls
		rungs = append(rungs, fmt.Sprintf("%s:calls=%d,tokens=%d", r.Rung, r.Calls, r.TotalTokens))
	}
	d.Log.Info("session spend",
		"bead", rec.BeadAnchor, "session_key", rec.SessionKey,
		"attempt", rec.Attempt, "rung", rec.Rung, "trigger", rec.Trigger,
		"outcome", outcome,
		"model_calls", calls,
		"prompt_tokens", cost.PromptTokens,
		"completion_tokens", cost.CompletionTokens,
		"total_tokens", cost.TotalTokens,
		"cost_usd", cost.CostUSD,
		"synthetic_cost_usd", cost.SyntheticCostUSD,
		"by_rung", strings.Join(rungs, " "),
		// COMPLETE=false means an open reservation still exists for this scope,
		// so these numbers may not be final. Saying so is the difference
		// between a cost record and a misleading one.
		"complete", cost.Complete,
		"as_of", cost.AsOf)
}

// refire re-fires gonk-dispatch for rec.BeadAnchor -- the ONLY way a sweep
// causes more spend, and it goes through Gate 2 by construction (see
// dispatchOrderName's doc comment). It reports whether Gas City accepted the
// order; false does NOT prove nothing was fired (gc's handler is detached from
// the client, so a POST can be processed and its answer lost), which is why
// nothing may rely on it -- the `fire_by` deadline is what bounds a fire.
func refire(ctx context.Context, d sweepDeps, rec beadstore.Record) bool {
	vars := map[string]string{
		"project":     rec.Project,
		"project_id":  fmt.Sprint(rec.ProjectID),
		"rig":         rec.Rig,
		"issue_iid":   fmt.Sprint(rec.IssueIID),
		"bead_anchor": rec.BeadAnchor,
		"bead_id":     rec.BeadID,
		"session_key": rec.SessionKey,
		"trigger":     rec.Trigger,
		"config_hash": rec.ConfigHash,
	}
	// A pipeline run carries its run identity back (gonk-tios D12), exactly as
	// intake's order did: the anchor is rebuilt from it and must match.
	// pipeline_last_success 0 is ABSENT, as intake sends it.
	if rec.PipelineID != 0 {
		vars["pipeline_id"] = fmt.Sprint(rec.PipelineID)
		vars["pipeline_target"] = rec.PipelineTarget
		vars["pipeline_fingerprint"] = rec.PipelineFingerprint
		if rec.PipelineLastSuccess != 0 {
			vars["pipeline_last_success"] = fmt.Sprint(rec.PipelineLastSuccess)
		}
		// The fix epoch likewise (gonk-2usw 3.3): absent when the run follows
		// no merged fix, so such a re-fire is what it always was.
		if rec.PipelineFixEpoch != 0 {
			vars["pipeline_fix_epoch"] = fmt.Sprint(rec.PipelineFixEpoch)
		}
	}
	// A re-sling carries the deadline its dispatch must start by
	// (reslingWindowEnd). An unpark does not: no window waits on it.
	if !rec.ReslingFireBy.IsZero() {
		vars[fireByVar] = rec.ReslingFireBy.UTC().Format(time.RFC3339Nano)
	}
	if err := d.Orders.RunOrder(ctx, dispatchOrderName, vars); err != nil {
		d.Log.Error("sweep: re-fire gonk-dispatch failed", "err", err, "bead", rec.BeadAnchor)
		return false
	}
	return true
}
