package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gate"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/rig"
)

// dispatchOrderName is the exec order that wraps `gonk-gate dispatch` (Gate
// 2). Re-firing it -- not calling runDispatch in-process -- is what makes the
// re-sling and the unpark go through the SAME re-decide path a first dispatch
// does: Gas City runs a fresh `gonk-gate dispatch` process, which asks meter
// /v1/policy/decide again. There is exactly one other RunOrder call site in
// this tree (dispatch.go, the pour); this is not a second pour path because
// "gonk-dispatch" is never a formula-order.
const dispatchOrderName = "gonk-dispatch"

// sweepDeps is gonk-sweep's dependency set. Every field has a safe default
// applied in runSweep so a caller only needs to set what a given test cares
// about.
type sweepDeps struct {
	Meter *meterAPI
	GC    *gcapi.Client
	GL    gitlabQuerier
	Store beadstore.Store
	Log   *slog.Logger

	// Rig revokes a session's checkout grant at teardown (pkg/rig, gonk-msz).
	// Optional: nil simply means grants are left to expire on their own TTL.
	Rig *rig.Client

	// Apply is the WRITE side of pkg/glab the v2 broker uses to apply a
	// validated proposed-effects batch under the bot PAT. Only the broker path
	// (a running record with a SessionID for a ported trigger) uses it.
	// Satisfied by *glab.Client.
	Apply brokerApplier
	// PackDir is the baked pack root (/opt/gonk/pack) the broker reads
	// effect-shape.toml from. Defaulted in withDefaults.
	PackDir string

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

	// LAST, and deliberately after all three passes above. The reaper decides what to
	// destroy by ABSENCE -- a gonk session that no running bead claims -- so it
	// must run only once this tick has finished making the store's picture of
	// what is running as accurate as it is going to get.
	//
	// It reports nothing and cannot fail the sweep: an orphan left for the next
	// tick costs one pod for 30 seconds, while a sweep that aborted on the
	// reaper's behalf would strand real outcomes.
	runReap(ctx, dd)

	// The pass completed. Clear any failure run and say so once, if it is news.
	recordSweepSuccess(dd.Log, dd.HealthFile, dd.Now())
	return 0
}

// pendingPromptGrace is how long a StatePendingPrompt reservation may sit
// before runSweep concludes no dispatch is behind it any more.
//
// It is defaultSubmitDeadline (dispatch.go) -- the hard cap on the whole
// deliver-and-confirm loop -- plus a minute for everything that runs BEFORE
// that loop and after the reservation write: the issue or repository fetch,
// the checkout grant, the project-key read, PutPrompt, and the create itself.
// The sum is 300s, which is exactly gonk-dispatch's own order timeout
// (pack/orders/gonk-dispatch.toml): Gas City kills the process at that point,
// so a reservation older than this provably has no live dispatch behind it.
//
// It must also stay COMFORTABLY UNDER reapGrace (reap.go, 10 minutes), and
// TestPendingPromptGraceLandsInsideTheReaperGrace enforces that -- as a
// five-minute margin, which is what "comfortably" is worth here, not a bare
// `<` that 599s would satisfy. TestPendingPromptGraceCoversTheDispatchOrders-
// Timeout enforces the other claim in this comment, that the grace is at least
// the order timeout pack/orders/gonk-dispatch.toml actually declares. The reaper
// treats only StateRunning records as claiming a session, so a pending-prompt
// alias is unclaimed for as long as it sits in that state; reclaiming it at
// 300s promotes it to StateRunning -- and therefore into the claimed set --
// with 5 minutes to spare before the reaper would ever consider closing it.
const pendingPromptGrace = defaultSubmitDeadline + time.Minute

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
	if err := d.Store.Put(ctx, rec); err != nil {
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

// sweepRunning classifies one running bead. It is a no-op for a bead whose
// session has not ended yet (SessionEndedAt is the zero value): "still
// running" is not this tick's job.
func sweepRunning(ctx context.Context, d sweepDeps, rec beadstore.Record) {
	// The v1 path has no session to consult, so it still waits for the stamp.
	// The BROKER path does not: nothing in this tree has ever stamped
	// SessionEndedAt (gonk-u1p.5), so gating on it skipped every broker bead
	// forever. The live SessionView is both available and more truthful -- it is
	// the session itself saying whether it is done.
	agent, isBroker := agentForTrigger[rec.Trigger]
	broker := isBroker && rec.SessionID != ""
	if !broker && rec.SessionEndedAt.IsZero() {
		return
	}

	// The reservation is the DEADLINE, and it is computed BEFORE the
	// still-running check below: declining to judge a working session must not
	// mean waiting on it forever. A wedged agent -- one that never received its
	// prompt, say -- would otherwise hold its reservation to TTL and leave the
	// bead in StateRunning permanently, with no outcome ever reported.
	expired := !rec.ReservationExpiresAt.IsZero() && d.Now().After(rec.ReservationExpiresAt)

	var view *gcapi.SessionView
	if broker {
		v, err := brokerSessionView(ctx, d, rec)
		switch {
		case err != nil && !expired:
			// Could not read the session: unknown. Try again next tick rather
			// than classify on no information. Still the right call INSIDE the
			// reservation -- a transient 5xx or a restarting supervisor must buy
			// a retry, not a verdict on a session that is probably still working.
			d.Log.Warn("sweep: could not read broker session", "bead", rec.BeadAnchor, "err", err)
			return
		case err != nil:
			// Unreadable AND past its deadline. The reservation is the deadline
			// for THIS too (gonk-u6p): an alias that resolves to two sessions
			// 409s on every read and never stops, so "try again next tick" is a
			// promise that can never be kept -- the bead sat in StateRunning for
			// eight days being re-read every 60s. Fall through with view nil:
			// no batch is read, and ReservationExpired classifies it infra-failed,
			// which does not escalate the rung and IS bounded by
			// max_infra_retries. A bead that cannot be judged must still be able
			// to STOP.
			d.Log.Warn("sweep: broker session unreadable past its reservation; classifying rather than retrying forever",
				"bead", rec.BeadAnchor, "session", rec.SessionID,
				"reservation_expired_at", rec.ReservationExpiresAt, "err", err)
		case v == nil && !expired:
			// Still working, still inside its reservation. Nothing to judge and
			// nothing to report -- exactly like the v1 guard above. Reporting an
			// outcome here would re-sling a live session every 30s.
			d.Log.Info("sweep: session still running; nothing to judge yet",
				"bead", rec.BeadAnchor, "session", rec.SessionID)
			return
		case v == nil:
			// Running PAST its reservation: wedged. Reap it. view stays nil so
			// no batch is read, and ReservationExpired is what classifies it.
			d.Log.Warn("sweep: session still running past its reservation; reaping rather than waiting",
				"bead", rec.BeadAnchor, "session", rec.SessionID,
				"reservation_expired_at", rec.ReservationExpiresAt)
		default:
			view = v
			// We have now OBSERVED the finish, which is what the stamp means. It
			// feeds gatherSpend's staleness comparison below.
			if rec.SessionEndedAt.IsZero() {
				rec.SessionEndedAt = d.Now()
			}
		}
	}

	signals := gate.Signals{ReservationExpired: expired}

	kind := artifactKindForTrigger[rec.Trigger]
	// Aborted (a human closed the bead) is only meaningful for an issue-scoped
	// trigger: scaffold's work item is the PROJECT (spec 5.3 -- it runs before
	// any issue exists), so there is no issue to close.
	aborted := false
	if kind == "comment" {
		issue, err := d.GL.GetIssue(ctx, rec.ProjectID, rec.IssueIID)
		if err != nil {
			signals.ArtifactUnknown = true
		} else if issue.State == "closed" {
			aborted = true
		}
	}
	signals.Aborted = aborted

	if !signals.ArtifactUnknown && !signals.Aborted {
		switch {
		case broker && view == nil:
			// Reaped mid-flight above: there is no finished session to read a
			// batch from, and ReservationExpired already settles Classify.
		case broker:
			// v2 broker: sweep itself reads+validates+applies the agent's batch,
			// so the apply RESULT is the artifact signal -- no re-read of GitLab.
			// (A running record with no SessionID is a pre-v2 bead; it falls to
			// the artifactPresent path below.)
			applied, violation, aerr := applyBrokerBatch(ctx, d, agent, rec, view)
			switch {
			case aerr != nil:
				// Could not read the session or load our shape: an UNKNOWN.
				// Uncertainty must not escalate (AD-6) -- classify as such.
				signals.ArtifactUnknown = true
				d.Log.Warn("sweep: broker batch read/apply error", "bead", rec.BeadAnchor, "err", aerr)
			case applied:
				signals.ArtifactPresent = true
				// Say so. Every OTHER outcome of this switch is logged, so a
				// silent success is the one case a reader cannot distinguish
				// from "the sweep never looked at this bead at all" -- which is
				// exactly the ambiguity gonk-zp3 is stuck in. A log that only
				// speaks up on failure cannot tell you the happy path ran.
				d.Log.Info("sweep: triage batch applied", "bead", rec.BeadAnchor, "agent", agent)
			default:
				// Present-but-invalid or no batch: nothing applied. The bead
				// falls to the ladder (spend>0 => escalate/needs-human; else
				// retry). Record the violation for observability.
				if violation != "" {
					d.Log.Warn("sweep: triage batch rejected, applied nothing",
						"bead", rec.BeadAnchor, "violation", violation)
				}
			}
		default:
			present, unknown, _ := artifactPresent(ctx, d.GL, d.BotUsername, kind, rec.ProjectID, rec.IssueIID, rec.BeadID)
			if unknown {
				signals.ArtifactUnknown = true
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
		if err := d.Store.Put(ctx, rec); err != nil {
			d.Log.Error("sweep: store Put failed", "err", err, "bead", rec.BeadAnchor)
		}
		d.Log.Info("sweep: bead done", "bead", rec.BeadAnchor, "outcome", outcome)
	case "escalate", "retry":
		finished = true
		// The re-sling: fire the gonk-dispatch order for the SAME BeadAnchor. It
		// goes through Gate 2 by construction -- Gas City runs a fresh
		// `gonk-gate dispatch`, which re-decides and OVERWRITES this record via
		// its own Store.Put once it runs.
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
		if err := d.Store.Put(ctx, rec); err != nil {
			d.Log.Error("sweep: store Put failed", "err", err, "bead", rec.BeadAnchor)
		}
		refire(ctx, d, rec)
		d.Log.Info("sweep: re-sling fired", "bead", rec.BeadAnchor, "outcome", outcome, "next", resp.Next)
	default:
		d.Log.Error("sweep: meter returned an unknown Next; leaving the bead running", "next", resp.Next, "bead", rec.BeadAnchor)
	}

	if finished {
		closeSession(ctx, d, rec)
	}
}

// closeSession returns the pod. It is the last thing that happens to a bead's
// session and the only thing that gives the cluster its CPU and memory back.
//
// BEST-EFFORT BY CONSTRUCTION, AND LOUD WHEN IT FAILS. The outcome is already
// reported and the bead has already moved on, so a teardown failure must not
// fail the sweep, re-judge the bead, or block the next one -- but it must never
// pass quietly either. A silently swallowed close is the same class of bug as
// the missing close: something that did nothing and looked like success.
//
// A 404 IS SUCCESS. The session is already gone, which is precisely the state
// being asked for -- and it is the normal case on a second sweep of a bead whose
// close raced a reconciler. Treating it as an error would make every recovery
// path noisy for no reason.
func closeSession(ctx context.Context, d sweepDeps, rec beadstore.Record) {
	// The v1 formula path never held a session handle: gonk did not create the
	// session, Gas City's pool did, and there is no alias to close by. Those
	// beads are not this function's business.
	if rec.SessionID == "" {
		return
	}
	// Revoke the checkout grant next to the close, for the same reason: a pod
	// that outlives its work must stop being able to pull the tree. Grants carry
	// their own TTL, so a failure here costs at most that window -- hence Debug,
	// not Error, unlike the leaked-pod case below (pkg/rig, gonk-msz).
	if d.Rig != nil {
		if err := d.Rig.Revoke(ctx, rec.SessionID); err != nil {
			d.Log.Debug("sweep: checkout revoke failed; the grant will expire on its own",
				"bead", rec.BeadAnchor, "session", rec.SessionID, "err", err)
		}
	}
	if err := d.GC.CloseSession(ctx, rec.SessionID); err != nil {
		if gcapi.IsNotFound(err) {
			d.Log.Debug("sweep: session already gone at close",
				"bead", rec.BeadAnchor, "session", rec.SessionID)
			return
		}
		// ERROR, not Warn: this is a leaked pod, and enough of them stop the
		// cluster scheduling anything at all (gonk-xkm).
		d.Log.Error("sweep: session close failed -- ITS POD IS LEAKED",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "err", err)
		return
	}
	d.Log.Info("sweep: session closed", "bead", rec.BeadAnchor, "session", rec.SessionID)
}

// gatherSpend forces a spend sync (HB-2) and polls session cost until
// meter's view has caught up past SessionEndedAt, or the deadline elapses.
// On deadline it reports SpendStale: we could not PROVE the model answered,
// so we do not escalate (AD-6: uncertainty never escalates).
func gatherSpend(ctx context.Context, d sweepDeps, rec beadstore.Record) (tokens int64, stale bool) {
	if _, err := d.Meter.SpendSync(ctx); err != nil {
		return 0, true
	}
	deadline := d.Now().Add(d.SpendDeadline)
	for {
		cost, err := d.Meter.CostSession(ctx, rec.SessionKey)
		if err == nil && !cost.AsOf.Before(rec.SessionEndedAt) {
			return cost.TotalTokens, false
		}
		if !d.Now().Before(deadline) {
			return 0, true
		}
		select {
		case <-ctx.Done():
			return 0, true
		case <-time.After(d.SpendPollInterval):
		}
	}
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
// dispatchOrderName's doc comment).
func refire(ctx context.Context, d sweepDeps, rec beadstore.Record) {
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
	if _, err := d.GC.RunOrder(ctx, dispatchOrderName, vars); err != nil {
		d.Log.Error("sweep: re-fire gonk-dispatch failed", "err", err, "bead", rec.BeadAnchor)
	}
}
