package main

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// IMAGE SKEW IS NOT THE AGENT'S FAILURE (gonk-t55 plan section 3.3,
// "handshake" item 3).
//
// gonk-gate builds every session's Job and stamps it with the agent image
// that is deployed; the two images are released separately, and dev has run
// them at different versions for ~20 minutes at a time. When they disagree
// about the session contract (runtime.SessionContract), the agent's entrypoint
// refuses before it reads anything, fetches anything or starts opencode, and
// exits runtime.ExitSessionContractMismatch.
//
// Judging that session like any other would be wrong three ways: it would
// report an outcome to meter (charging the attempt -- and a batch-less,
// token-less session is infra-failed, which spends max_infra_retries toward a
// needs-human DENY), it would re-sling onto a new attempt, and it would do so
// quietly, as one more infra failure among many. So instead:
//
//   - it is logged at ERROR, naming both contract versions;
//   - NO outcome is reported. The attempt counter is meter's, advanced only
//     when an attempt is RECORDED (/v1/policy/outcome, or the janitor when an
//     unsettled reservation expires). Reporting nothing leaves the
//     reservation open, and meter's /decide hands an open reservation back
//     verbatim -- same attempt, same rung, same deadline -- so the retry is
//     attempt N again, under the same session name;
//   - the bead is PARKED with a RetryAfter (the existing defer path: the
//     sweep's unpark pass re-fires gonk-dispatch, which decides again), with
//     no session on the record, so the retry's dedupe guard does not mistake
//     the refused session for a live one; and
//   - the refused session is STOPPED, so the retry's Start creates a fresh Job
//     under the same name rather than adopting the dead one (the jobs runtime
//     also refuses to adopt a Job that is still being deleted).
//
// THE ONE LIMIT, which gonk-gate cannot remove without changing meter's
// /decide contract (the bead forbids that): if the skew outlasts the
// reservation, meter's janitor expires it and records the attempt
// infra-failed, and the next /decide is attempt N+1. HOW MUCH MARGIN THAT
// LEAVES DEPENDS ON THE INSTALL'S reservation_ttl, and it is thin on dev: at
// the 60m chart default a skew must last about three times the widest one
// observed (~20 min) to be charged, but dev runs reservation_ttl: 30m, where
// it is only about 1.5 times -- and the reservation was already running down
// before the refused session started. The retry, even when it is not charged,
// inherits only what is left of the reservation as its deadline, so a retry
// after a long skew can itself end DeadlineExceeded (infra-failed). The ERROR
// line carries the reservation's expiry so an operator can see it coming.

// skewRetryAfter is how long a skew-parked bead waits before the unpark pass
// re-dispatches it. Short, because the retry is cheap -- the refusal happens
// before any model call, so a still-skewed retry costs one pod start -- and
// because every minute spent parked is a minute off the reservation the retry
// runs inside.
const skewRetryAfter = 2 * time.Minute

// skewRefusalLine is the entrypoint's closing line for a contract refusal
// (images/agent/entrypoint.sh). The exit code ALONE is not proof: in jobs mode
// the entrypoint passes opencode's own rc through, and opencode could exit 8.
// The refusal line is written only by the refusal.
const skewRefusalLine = "session end: refused (session contract mismatch)"

// skewVersions pulls the two versions out of the entrypoint's refusal line:
// what the Job was sent, and what the image supports.
var skewVersions = regexp.MustCompile(`session contract mismatch: sent=(\S*) supported=\[([^\]]*)\]`)

// skewVerdict is what parkIfImageSkew decided.
type skewVerdict int

const (
	// notSkew: judge the session as usual.
	notSkew skewVerdict = iota
	// skewHandled: the bead was parked (or the attempt to park it was logged);
	// this tick is done with it.
	skewHandled
	// skewUnreadable: the session exited with the skew code but its output
	// could not be read to confirm it, inside the reservation: try next tick.
	skewUnreadable
)

// isSkewExit: did the session end with the entrypoint's contract-refusal exit?
func isSkewExit(st runtime.Status) bool {
	return st.Phase == runtime.PhaseFailed && st.ExitCode != nil && *st.ExitCode == runtime.ExitSessionContractMismatch
}

// parkIfImageSkew handles a session that exited with the skew code. See the
// block comment above for why none of this reports an outcome.
func parkIfImageSkew(ctx context.Context, d sweepDeps, rec beadstore.Record, readRevision string, st runtime.Status, expired bool) skewVerdict {
	h := runtime.Handle(rec.SessionID)
	out, err := d.Runtime.Output(ctx, h)
	if err != nil {
		if !expired {
			d.Log.Warn("sweep: session exited with the session-contract code but its output cannot be read to confirm it; trying again next tick",
				"bead", rec.BeadAnchor, "session", rec.SessionID, "err", err)
			return skewUnreadable
		}
		d.Log.Warn("sweep: session exited with the session-contract code, its output is unreadable, and its reservation has passed; judging it as usual",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "err", err)
		return notSkew
	}
	text := string(out)
	if !strings.Contains(text, skewRefusalLine) {
		d.Log.Warn("sweep: session exited with the session-contract code WITHOUT the entrypoint's contract refusal; judging it as an ordinary failure",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "exit_code", runtime.ExitSessionContractMismatch)
		return notSkew
	}
	sent, supported := "unknown", "unknown"
	if m := skewVersions.FindStringSubmatch(text); m != nil {
		sent, supported = m[1], m[2]
	}

	now := d.Now()
	retryAfter := now.Add(skewRetryAfter)
	deadline := runningDeadline(rec)
	attrs := []any{
		"bead", rec.BeadAnchor, "session", rec.SessionID, "attempt", rec.Attempt,
		"controller_session_contract", strconv.Itoa(runtime.SessionContract),
		"job_session_contract", sent, "agent_supported_session_contracts", supported,
		"retry_after", retryAfter, "reservation_expires_at", deadline,
	}
	d.Log.Error("sweep: IMAGE SKEW -- the agent image refused the session contract this session was built with; "+
		"not charged, not an attempt: parking the bead to run again once the controller and agent images agree",
		attrs...)
	if !deadline.IsZero() && !retryAfter.Before(deadline) {
		d.Log.Warn("sweep: the skew-parked bead's reservation expires before its retry; meter's janitor will record this attempt infra-failed and the retry will be the next attempt",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "reservation_expires_at", deadline, "retry_after", retryAfter)
	}

	// NO SESSION ON THE RECORD, no re-sling stamps, the reservation kept: see
	// parkedForRetry (sweep_legacy.go).
	parked := parkedForRetry(rec, retryAfter)

	_, err = d.Store.PutIf(ctx, parked, readRevision)
	switch {
	case errors.Is(err, beadstore.ErrStale):
		// Another writer owns the record now; its session is its business.
		d.Log.Warn("sweep: record changed after this pass read it; not parking the skewed bead over the newer record",
			"bead", rec.BeadAnchor, "session", rec.SessionID)
		return skewHandled
	case err != nil:
		// Not parked, so not stopped either: the record still names this
		// session, and the next tick finds the same refusal and tries again.
		d.Log.Error("sweep: could not park the skewed bead; it is judged again next tick",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "err", err)
		return skewHandled
	}
	// Parked: now remove the refused session, so the retry's Start creates a
	// fresh Job under the same name.
	closeSession(ctx, d, rec)
	return skewHandled
}
