package main

import (
	"context"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// gonk-r4u9. A `running` record that NEVER GOT A SESSION can never be settled.
//
// sweepRunning's first guard is
//
//	broker := isBroker && rec.SessionID != ""
//	if !broker && rec.SessionEndedAt.IsZero() { return }
//
// and NOTHING in this tree writes SessionEndedAt except sweepRunning's own
// broker branch (which this record cannot reach) and the re-sling, which
// clears it. So the guard's condition is PERMANENT: the record is listed by
// every 30s tick, returned from on every one, and never reaches the
// reservation deadline that sweepRunning itself calls the backstop ("a bead
// that cannot be judged must still be able to STOP", sweep.go).
//
// Observed live: gonk:63:scaffold sat in `running` from 2026-08-02 to the
// T-41 cutover on 2026-09-19 -- 49 days, visible to every List, with no
// outcome ever reported. It was written by the v1 formula pour, which set
// StateRunning with no SessionID at all (dispatch.go before ADR-007 §3).
//
// The fixture is that record: past its reservation by weeks, and untouchable.
func sessionlessRunningRecord(projectID int64, reservationExpired time.Time) beadstore.Record {
	return beadstore.Record{
		BeadAnchor: "gonk:63:scaffold", BeadID: "gk-63sc", Project: "group/repo",
		ProjectID: projectID, Rig: "group-repo", SessionKey: "gonk-63-scaffold",
		Trigger: "scaffold", State: beadstore.StateRunning,
		Rung: "cheap", Attempt: 1, ReservationID: "rsv-63",
		ReservationExpiresAt: reservationExpired,
		// As the v1 pour really left it: no session handle, no end stamp.
		SessionID: "", SessionEndedAt: time.Time{},
	}
}

func TestSweepSettlesARunningRecordThatNeverGotASession(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)

	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := beadstore.NewMemory()
	rec := sessionlessRunningRecord(p.ID, now.Add(-49*24*time.Hour))
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	rt := newTestRuntime()
	orders := &fakeOrders{}
	meter := meterClient(fm.server(t))

	// Seven weeks of 30s ticks, compressed: sweep it repeatedly and let the
	// clock advance the way production's did.
	clock := now
	for i := 0; i < 200; i++ {
		code := runSweep(context.Background(), sweepDeps{
			Meter: meter, Runtime: rt, Orders: orders, GL: gl.Client(),
			Store: store, BotUsername: "gonk", PackDir: repoPackDir,
			Now: func() time.Time { return clock },
		})
		if code != 0 {
			t.Fatalf("tick %d: exit = %d", i, code)
		}
		clock = clock.Add(30 * time.Second)
	}

	// THE CLAIM: a record past its reservation must reach meter. Asserting on
	// the /outcome POST rather than on the final state is deliberate -- the
	// fake meter answers whatever outcomeNext says, so the state would be
	// testing the fake; that meter was ASKED is what pins the behaviour.
	reqs := fm.requests()
	if len(reqs) == 0 {
		got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
		t.Fatalf("200 sweeps reported NO outcome for a record %v past its reservation; "+
			"it is still %q and will be forever", clock.Sub(rec.ReservationExpiresAt), got.State)
	}
	if reqs[0].Outcome != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %q, want infra-failed: an expired reservation is INFRA, it must not buy a rung",
			reqs[0].Outcome)
	}
	if reqs[0].ReservationID != rec.ReservationID {
		t.Fatalf("outcome reservation = %q, want %q -- an unbound outcome is a forgery vector",
			reqs[0].ReservationID, rec.ReservationID)
	}

	// ...AND IT MUST NOT WALK THE LADDER FOR FREE. The re-sling window is what
	// bounds this for a broker record; a sessionless one needs the same bound,
	// or a record nothing replaces is re-judged and re-fired on every tick.
	// 200 ticks span 100 minutes; the window is ~9 minutes, so a dozen is
	// generous and 200 is the bug.
	if len(reqs) > 20 {
		t.Fatalf("reported %d outcomes over 200 ticks -- one per tick walks the ladder for free", len(reqs))
	}
	if len(orders.orders()) > 20 {
		t.Fatalf("fired %d re-slings over 200 ticks: %v", len(orders.orders()), orders.names())
	}
}

// THE SECOND SHAPE, and the worse one: a running record whose Trigger has
// LEFT agentForTrigger. ADR-007 §3 deleted mention-reply from that map, and
// every record running under it at that moment stopped being judgeable --
// `broker` is false however good its SessionID is. That one is worse than the
// sessionless case because the record still CLAIMS its session
// (reap.go's claimedSessionIDs keys on SessionID != ""), so the reaper will
// not close the pod either: a live pod, held forever, by a record nothing can
// settle.
//
// Past its reservation it must now be judged, reported and CLOSED.
func TestSweepSettlesARunningRecordWhoseTriggerNoLongerRoutes(t *testing.T) {
	if _, routes := agentForTrigger["mention-reply"]; routes {
		t.Skip("mention-reply routes again (T-24); this fixture needs a trigger that does not")
	}
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	rt := newTestRuntime()
	orders := &fakeOrders{}
	alias := runtime.Name("mention", 42, runtime.KindIssue, 3, 1)
	rt.runSession(t, alias, "still sitting at the splash screen\n")

	store := beadstore.NewMemory()
	rec := sessionlessRunningRecord(p.ID, now.Add(-30*24*time.Hour))
	rec.BeadAnchor, rec.Trigger, rec.IssueIID, rec.SessionID = "gonk:42:issue:3", "mention-reply", 3, alias
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(),
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		Now: func() time.Time { return now },
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome requests = %+v, want one infra-failed", reqs)
	}
	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("live = %v, want session %s stopped: a running record claims it, so the stale-session "+
			"pass will never take it either", live, alias)
	}
}
