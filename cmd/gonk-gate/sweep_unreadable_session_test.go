package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// gonk-u6p. A broker session whose alias resolves to TWO Gas City sessions 409s
// on every read, and the ambiguity NEVER ages out -- both sessions go on
// existing, so the name is permanently unusable.
//
// sweepRunning returned on any session-read error before it ever consulted the
// reservation, so such a bead sat in StateRunning being re-read every 60s
// forever. Measured in production 2026-08-18: gonk:75:issue:24 had been
// retrying since 2026-08-10, eight days, with no terminal state and no way to
// reach one.
//
// THE PRINCIPLE THIS RESTORES is gonk-u1p.6's, already written into the comment
// above the `expired` computation: the reservation is the DEADLINE, and
// declining to judge must not mean waiting forever. That was implemented for
// "the session is still running" (the branch below guards on `!expired`) and
// missed for "the session cannot be read at all" -- which is the worse case,
// because a running session at least finishes on its own.
func TestSweepClassifiesABrokerSessionItCanNeverRead(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.AmbiguousAlias("gonk.triage.p42.i3.a1")

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	// Past its deadline. This is the whole point: before the fix, this field was
	// computed and then thrown away on the read-error path.
	rec.ReservationExpiresAt = time.Now().Add(-time.Minute)
	_ = store.Put(context.Background(), rec)

	applier := &recordingApplier{}
	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	var log strings.Builder

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir, Log: testLogger(&log),
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	// THE ASSERTION. An outcome must be reported. Without one the bead cannot
	// leave StateRunning by any path, which is precisely the production stall.
	reqs := fm.requests()
	if len(reqs) != 1 {
		t.Fatalf("reported %d outcomes for an unreadable session, want 1 -- "+
			"the bead is stuck in StateRunning forever. log:\n%s", len(reqs), log.String())
	}

	// It must be INFRA-FAILED, not gate-failed. We never read the session, so we
	// have no evidence about the work: an unreadable session is our outage, and
	// AD-6 says uncertainty must not buy a rung. gate.Classify already yields
	// this from ReservationExpired alone -- asserting it here pins that a future
	// change cannot quietly turn a read failure into an escalation.
	if got := reqs[0].Outcome; got != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %q, want %q -- an unreadable session must never escalate the rung",
			got, meterapi.OutcomeInfraFailed)
	}

	// Nothing may be applied: we never saw a batch.
	if len(applier.notes) != 0 || len(applier.labels) != 0 {
		t.Fatalf("applied effects from a session it could not read: notes=%+v labels=%+v",
			applier.notes, applier.labels)
	}
}

// The other half of the same rule, and the reason the fix is a guard rather than
// a blanket "classify on any read error": INSIDE its reservation, an unreadable
// session is still just unknown. A transient 5xx or a restarting supervisor must
// buy a retry next tick, not a verdict -- classifying there would burn an infra
// attempt on a session that is very likely still working.
func TestSweepStillWaitsOnAnUnreadableSessionInsideItsReservation(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.AmbiguousAlias("gonk.triage.p42.i3.a1")

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	rec.ReservationExpiresAt = time.Now().Add(time.Hour) // still inside it
	_ = store.Put(context.Background(), rec)

	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	if reqs := fm.requests(); len(reqs) != 0 {
		t.Fatalf("reported %d outcome(s) for a session still inside its reservation: %+v", len(reqs), reqs)
	}
	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StateRunning {
		t.Fatalf("state = %q, want the bead left running for a later sweep", got.State)
	}
}
