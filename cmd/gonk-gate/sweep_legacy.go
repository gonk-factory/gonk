package main

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// A GAS CITY SESSION STILL IN FLIGHT AT THE CUTOVER IS NOT A JOB THAT
// VANISHED (gonk-t55 plan 3.4 C1).
//
// The deploy that moves dispatch to Kubernetes Jobs finds records the old
// controller wrote: `running` (and `pending-prompt`) records whose SessionID is
// a Gas City alias (brokerSessionAlias: gonk.<agent>.p<id>[.i<iid>].a<n>.<nonce>),
// not a runtime.Name. Asked about that name, the Jobs runtime answers
// PhaseNotFound -- there is no such Job, and never was -- and the ordinary path
// would judge it ABSENT: "no session" violation, gate-failed once the session
// has spent tokens (a live one has), the rung escalated, the attempt charged.
// Its Gas City pod would sit on the entrypoint's tmux hold at 500m/1Gi, with
// nothing left in gonk that ever looks at it again.
//
// None of that is the agent's doing, so none of it is charged:
//
//   - NO outcome is reported. The attempt counter is meter's and advances only
//     on /v1/policy/outcome or when its janitor expires an unsettled
//     reservation; with nothing reported, the reservation stays open and
//     meter's /decide hands it back verbatim (same attempt, rung, deadline).
//   - The bead is PARKED exactly as an image-skewed one is (sweep_skew.go):
//     no session on the record, no re-sling stamps, reservation fields kept --
//     with a RetryAfter of NOW, so this same sweep pass's unpark re-fires
//     gonk-dispatch, which decides again and starts the attempt as a Job.
//   - Its Gas City SESSION IS CLOSED (legacySessionCloser), and THEN its pod
//     is deleted by its `gc-agent=<alias>` label. Closing first is what stops
//     the pod coming back: Gas City's reconciler restarts the pod of an OPEN
//     session whose pod has gone, and does not look at closed ones (see
//     reapLegacyGasCityPods). A close answering 404 means the session bead is
//     already closed: INFO, and the pod delete runs. Any other close error is
//     a WARN and the pod delete STILL runs; reapLegacyGasCityPods, at the end
//     of every pass, is the backstop that removes a pod Gas City restarts for
//     a session that could not be closed.
//
// THE CLOSER IS AN INTERFACE, NOT A GAS CITY CLIENT (exit criterion 2: no
// sweep file imports the Gas City client package). main.go implements it with the gcapi client,
// as it does orderRunner -- gonk-gate still runs as a Gas City order until
// T-57, so Gas City is there anyway. It is NOT the Gas City reaper: it closes
// exactly the one alias a record named, never a session found by listing.
//
// THE LIMIT is the handshake's: a legacy session's reservation keeps running
// down while the old session held it, and the retry inherits only what is
// left. If that reservation has already passed -- or passes before the retry
// settles -- meter's janitor records the attempt infra-failed and the retry is
// attempt N+1. Nothing gonk-gate can do removes that without a meter change;
// the WARN below says when it is already the case.

// legacyGasCityAlias matches the Gas City alias shape brokerSessionAlias
// minted: `gonk.` then the agent, project, optional issue and attempt
// segments, then (normally) the nonce. Deliberately looser than the reaper's
// gonkSessionAlias -- the nonce is not required -- because on the record side
// a miss is the costly direction (a legacy session judged absent and charged),
// and no runtime.Name can match: those are DNS-1123 labels, which contain no
// dot. Pods are only ever matched against it AFTER the cutover, when nothing
// mints such an alias any more (see reapLegacyGasCityPods).
var legacyGasCityAlias = regexp.MustCompile(`^gonk\.[a-z][a-z0-9-]*\.p\d+(\.i\d+)?\.a\d+(\.|$)`)

// isLegacyGasCitySession: does this session name a Gas City session rather
// than a SessionRuntime handle?
func isLegacyGasCitySession(sessionID string) bool {
	return legacyGasCityAlias.MatchString(sessionID)
}

// legacyParkMessage is the ONE stable log line for a legacy park. The dev
// smoke test keys on it (plan 3.4 C1 as built), so it is a constant and
// TestALegacyGasCitySessionIsParkedNotChargedAndItsPodDeleted pins its text.
const legacyParkMessage = "sweep: legacy Gas City session at cutover; not judged, not charged: " +
	"parking the bead to re-dispatch as a Job"

// legacySessionCloser closes one Gas City session by its alias (in
// production: gcapi.Client.CloseSession, adapted in main.go). An alias Gas
// City has no OPEN session for must be reported as an error wrapping
// errLegacySessionGone, so the sweep can tell "already closed" from a failure
// without importing the Gas City client package.
type legacySessionCloser interface {
	CloseLegacySession(ctx context.Context, alias string) error
}

// errLegacySessionGone: the closer found no open Gas City session under the
// alias (a 404 -- the bead is already closed, or never existed).
var errLegacySessionGone = errors.New("no open Gas City session under this alias")

// legacyRetryAfter is how long a legacy-parked bead waits before the unpark
// pass re-dispatches it: not at all. Every minute parked is a minute off the
// reservation the retry runs inside.
const legacyRetryAfter = 0 * time.Second

// parkLegacySession handles a running record whose session is a Gas City
// alias. See the block comment above. It always ends this tick's handling of
// the record.
func parkLegacySession(ctx context.Context, d sweepDeps, rec beadstore.Record, readRevision string, expired bool) {
	alias := rec.SessionID
	deadline := runningDeadline(rec)
	now := d.Now()
	retryAfter := now.Add(legacyRetryAfter)
	_, err := d.Store.PutIf(ctx, parkedForRetry(rec, retryAfter), readRevision)
	switch {
	case errors.Is(err, beadstore.ErrStale):
		d.Log.Warn("sweep: record changed after this pass read it; not parking the legacy Gas City session's bead over the newer record",
			"bead", rec.BeadAnchor, "session", alias)
		return
	case err != nil:
		d.Log.Error("sweep: could not park the legacy Gas City session's bead; it is handled again next tick",
			"bead", rec.BeadAnchor, "session", alias, "err", err)
		return
	}
	d.Log.Warn(legacyParkMessage,
		"bead", rec.BeadAnchor, "session", alias, "attempt", rec.Attempt,
		"retry_after", retryAfter, "reservation_expires_at", deadline,
		"gas_city_pod_label", agentAliasLabel+"="+alias)
	closeLegacySession(ctx, d, rec.BeadAnchor, alias)
	deleteLegacyPodsOf(ctx, d, rec.BeadAnchor, alias)
	if expired {
		d.Log.Warn("sweep: the legacy Gas City session's reservation has already passed; meter's janitor records "+
			"that attempt infra-failed and the re-dispatch is the next attempt",
			"bead", rec.BeadAnchor, "session", alias, "reservation_expires_at", deadline)
	}
}

// closeLegacySession closes alias's Gas City session, so Gas City stops
// wanting its pod. Never fatal: see the block comment at the top.
func closeLegacySession(ctx context.Context, d sweepDeps, bead, alias string) {
	if d.LegacyCloser == nil {
		d.Log.Warn("sweep: no Gas City session closer configured; the legacy session stays open and Gas City may restart its pod "+
			"(the per-pass legacy pod pass deletes it again)", "bead", bead, "session", alias)
		return
	}
	err := d.LegacyCloser.CloseLegacySession(ctx, alias)
	switch {
	case err == nil:
		d.Log.Info("sweep: closed the legacy Gas City session", "bead", bead, "session", alias)
	case errors.Is(err, errLegacySessionGone):
		d.Log.Info("sweep: the legacy Gas City session was already closed", "bead", bead, "session", alias)
	default:
		d.Log.Warn("sweep: could not close the legacy Gas City session; deleting its pod anyway -- Gas City may restart it, "+
			"and the per-pass legacy pod pass deletes it again", "bead", bead, "session", alias, "err", err)
	}
}

// deleteLegacyPodsOf deletes the pods Gas City ran for alias, by its gc-agent
// label, right after its session was closed. A failure is logged; the
// per-pass legacy pod pass retries it (the record no longer claims the alias).
func deleteLegacyPodsOf(ctx context.Context, d sweepDeps, bead, alias string) {
	if d.LegacyPods == nil {
		d.Log.Warn("sweep: no pod cleanup configured (GONK_POD_REAP_NAMESPACE unset); the legacy session's Gas City pod, if any, is left running",
			"bead", bead, "session", alias, "label", agentAliasLabel+"="+alias)
		return
	}
	pods, err := d.LegacyPods.ListAgentPods(ctx)
	if err != nil {
		d.Log.Error("sweep: cannot list agent pods to delete the legacy session's pod; the per-pass legacy pod pass retries",
			"bead", bead, "session", alias, "err", err)
		return
	}
	for _, p := range pods {
		if p.Alias != alias || p.Terminating {
			continue
		}
		if err := d.LegacyPods.DeletePod(ctx, p.Name); err != nil {
			d.Log.Error("sweep: could not delete a legacy Gas City pod -- IT IS STILL RUNNING; trying again next tick",
				"pod", p.Name, "session", alias, "err", err)
			continue
		}
		d.Log.Warn(legacyPodDeletedMessage, "pod", p.Name, "session", alias, "phase", p.Phase)
	}
}

// legacyPodDeletedMessage is logged for every legacy Gas City pod deleted, by
// the park or by the per-pass pass.
const legacyPodDeletedMessage = "sweep: deleted a legacy Gas City pod"

// reapLegacyGasCityPods deletes the Gas City pods of legacy sessions nothing
// claims any more. It runs at the end of every sweep pass.
//
// WHY EVERY PASS, EVEN THOUGH THE PARK CLOSES THE SESSION. Gas City's
// reconciler treats an OPEN session bead as wanting to run: when its pod
// disappears it starts a fresh one, subject only to its crash-loop quarantine
// (5 rapid deaths -> 5 minutes' pause) -- read from Gas City source (see
// TestTheLegacyPodPassHasNothingToDeleteOnceTheSessionIsClosed for file:line),
// not observed on a cluster. A CLOSED session is not in its reconcile set, so
// once the park's close succeeds this pass finds nothing for that alias. It
// is the backstop for a close that failed (a pod Gas City restarts), a pod
// delete that failed, and a pod left by a record that never reached the park.
// Each such pod lives at most one sweep interval.
//
// WHY THIS IS SAFE TO RUN ON LABEL SHAPE ALONE. After the cutover nothing in
// gonk creates a Gas City session: dispatch starts Jobs, and the controller is
// one replica under a Recreate strategy, so no old-controller dispatch runs
// beside this sweep. A pod whose gc-agent label is a gonk alias is therefore
// a leftover -- unless a running or pending-prompt record still names it (a
// legacy record this pass has not parked yet: inside its re-sling window, or
// its park lost a race), and those are skipped. Job pods carry no gc-agent
// label; Gas City's own sessions (control-dispatcher, pools) carry aliases
// that are not gonk's shape.
//
// Fail-closed like every pod-deleting path here: an unreadable store or pod
// list deletes nothing.
func reapLegacyGasCityPods(ctx context.Context, d sweepDeps) {
	if d.LegacyPods == nil {
		return // not configured; each legacy park says so
	}
	claimed := map[string]struct{}{}
	for _, state := range []beadstore.State{beadstore.StateRunning, beadstore.StatePendingPrompt} {
		recs, err := d.Store.List(ctx, state)
		if err != nil {
			d.Log.Error("sweep: cannot read the bead store; deleting NO legacy Gas City pods this tick", "err", err)
			return
		}
		for _, rec := range recs {
			if rec.SessionID != "" {
				claimed[rec.SessionID] = struct{}{}
			}
		}
	}
	pods, err := d.LegacyPods.ListAgentPods(ctx)
	if err != nil {
		d.Log.Error("sweep: cannot list agent pods; deleting NO legacy Gas City pods this tick", "err", err)
		return
	}
	for _, p := range pods {
		if !isLegacyGasCitySession(p.Alias) || p.Terminating {
			continue
		}
		if _, ok := claimed[p.Alias]; ok {
			continue // a record still names it: the running pass parks it first
		}
		if err := d.LegacyPods.DeletePod(ctx, p.Name); err != nil {
			d.Log.Error("sweep: could not delete a legacy Gas City pod -- IT IS STILL RUNNING; trying again next tick",
				"pod", p.Name, "session", p.Alias, "err", err)
			continue
		}
		d.Log.Warn(legacyPodDeletedMessage, "pod", p.Name, "session", p.Alias, "phase", p.Phase,
			"by", "per-pass legacy pod pass")
	}
}

// parkedForRetry is rec parked to run again at retryAfter WITHOUT having been
// judged: the same attempt, so no session on the record (reservePendingPrompt
// refuses a dispatch whose record already names a session for its attempt),
// no re-sling stamps (a stale fire_by would make the unpark's dispatch refuse
// itself as late, refuseLateFire), and the reservation fields kept -- they are
// what the retry's /decide hands back. Shared by the image-skew park
// (sweep_skew.go) and the legacy park above.
func parkedForRetry(rec beadstore.Record, retryAfter time.Time) beadstore.Record {
	parked := rec
	parked.State = beadstore.StateParked
	parked.RetryAfter = retryAfter
	parked.SessionID = ""
	parked.SessionEndedAt = time.Time{}
	parked.ReslungAt, parked.ReslingFireBy = time.Time{}, time.Time{}
	return parked
}
