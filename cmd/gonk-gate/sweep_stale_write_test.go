package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// gonk-p8w8. These tests replay, deterministically, the interleaving that put
// two live agent sessions on one bead+attempt on 2026-09-13
// (gonk:75:issue:73 attempt 2, sessions MPKO and FYKO). The bd comment log of
// that bead (go-3tbq) is quoted in docs/plans/2026-09-13-p8w8-stale-sweep-
// overwrite.md; in short:
//
//	15:49:24  dispatch MPKO writes its pending-prompt reservation for attempt 2
//	15:50:36  sweep B writes back `running a1 Attempt 1` -- the copy it LISTED
//	          before 15:49:00 -- over that reservation, and re-fires dispatch
//	15:52:00  dispatch FYKO reads `running a1`, sees Attempt 1 != 2, and the
//	          dedupe guard lets a second session through
//
// No sleeps: the "dispatch reserves while the sweep is mid-pass" step runs
// inside the fake meter's /outcome handler, which is after the sweep's List
// and before its write by construction.

// mpkoReservation is the record runBrokerDispatch writes before creating a
// session (broker_inject.go, `pending`): base fields from the webhook args,
// State pending-prompt, the attempt's alias already stamped as SessionID.
func mpkoReservation() beadstore.Record {
	a := baseDispatchArgs()
	return beadstore.Record{
		BeadAnchor: a.BeadAnchor, BeadID: a.BeadID, Project: a.Project, ProjectID: a.ProjectID,
		Rig: a.Rig, SessionKey: a.SessionKey, Trigger: a.Trigger, IssueIID: a.IssueIID,
		Attempt: 2, State: beadstore.StatePendingPrompt,
		SessionID: "gonk-triage-p42-i3-a2",
		Rung:      "cheap", Model: "m", ReservationID: "rsv-a2",
		ReservationExpiresAt: time.Now().Add(30 * time.Minute),
	}
}

// staleSweepFixture is sweep B's world: a broker record for attempt 1 whose
// session is still running past its reservation (the "reaping rather than
// waiting" branch B took), so the verdict is infra-failed and meter says retry.
func staleSweepFixture(t *testing.T) (*testRuntime, *fakeOrders, *glabtest.Server, *beadstore.Memory) {
	t.Helper()
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.runSession(t, testSession, "")

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	rec.ProjectID = 42 // the dispatch args' project, so both processes name one bead
	rec.SessionEndedAt = time.Time{}
	rec.ReservationExpiresAt = time.Now().Add(-2 * time.Minute)
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return rt, orders, gl, store
}

func TestASweepThatReadBeforeADispatchReservedDoesNotEraseTheReservation(t *testing.T) {
	rt, orders, gl, store := staleSweepFixture(t)
	ctx := context.Background()

	// The reservation lands after the sweep listed the record and before it
	// writes it back -- MPKO at 15:49:24, between B's List and B's write.
	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	fm.onOutcome = func() {
		if err := store.Put(ctx, mpkoReservation()); err != nil {
			t.Errorf("dispatch reservation write: %v", err)
		}
	}

	if code := runSweep(ctx, sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	}); code != 0 {
		t.Fatalf("sweep exit = %d", code)
	}

	got, _, _ := store.Get(ctx, "gonk:42:issue:3")
	want := mpkoReservation()
	if got.State != want.State || got.SessionID != want.SessionID || got.Attempt != want.Attempt {
		t.Errorf("after the sweep the record is %s %s Attempt %d, want the dispatch's reservation %s %s Attempt %d.\n"+
			"The sweep wrote back the copy it had listed BEFORE the reservation existed. That is the "+
			"15:50:36 write in go-3tbq's comment log: it is what let a second session through.",
			got.State, got.SessionID, got.Attempt, want.State, want.SessionID, want.Attempt)
	}
	if n := len(orders.orders()); n != 0 {
		t.Errorf("the sweep re-fired dispatch %d time(s) (%v) for a record another writer had already moved on; want 0",
			n, orders.names())
	}

	// THE SYMPTOM, not the proxy for it: the next dispatch for attempt 2 --
	// whether the one the sweep just fired or any other -- must see the
	// reservation and create nothing.
	dm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 2, ReservationID: "rsv-a2",
	}}
	if code := runDispatch(ctx, dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(dm.server(t)), Runtime: rt, Store: store,
		Forge: stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}},
		Args:  baseDispatchArgs(),
	}); code != 0 {
		t.Fatalf("second dispatch exit = %d, want 0 (a duplicate is a no-op)", code)
	}
	if n := len(rt.Starts()); n != 0 {
		t.Fatalf("a dispatch for attempt 2 called Start %d time(s) %+v while MPKO's reservation for attempt 2 "+
			"was still open: two sessions for one bead+attempt (gonk-p8w8)", n, rt.Starts())
	}
}

// The done branch writes back the listed copy too. A verdict of done over a
// record a dispatch has since replaced must leave the dispatch's record alone,
// not mark its reservation done (the independent review found this branch had
// no test: reverting it to an unconditional Put passed everything).
func TestASweepDoneVerdictDoesNotOverwriteARecordThatMovedOn(t *testing.T) {
	rt, orders, gl, store := staleSweepFixture(t)
	ctx := context.Background()
	fm := &fakeOutcomeMeter{outcomeNext: "done"}
	fm.onOutcome = func() {
		if err := store.Put(ctx, mpkoReservation()); err != nil {
			t.Errorf("dispatch reservation write: %v", err)
		}
	}
	if code := runSweep(ctx, sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	}); code != 0 {
		t.Fatalf("sweep exit = %d", code)
	}
	got, _, _ := store.Get(ctx, "gonk:42:issue:3")
	if got.State != beadstore.StatePendingPrompt || got.SessionID != mpkoReservation().SessionID {
		t.Fatalf("record = %s %s, want the reservation untouched by a done verdict on the old session",
			got.State, got.SessionID)
	}
}

// THE BROKER SessionEndedAt GAP, sequential rather than concurrent. A v1 record
// is skipped once its re-sling clears SessionEndedAt; a broker record ignores
// that field (gonk-u1p.5), so the NEXT tick -- 30s later, before the re-fired
// dispatch has run -- judged the same finished session again, reported the
// outcome again and fired a second dispatch. No overlap needed.
func TestSweepBrokerDoesNotReslingTheSameOutcomeTwice(t *testing.T) {
	rt, orders, gl, store := staleSweepFixture(t)
	ctx := context.Background()
	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	deps := sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	}

	for i := 1; i <= 2; i++ {
		if code := runSweep(ctx, deps); code != 0 {
			t.Fatalf("sweep %d exit = %d", i, code)
		}
	}
	if n := len(fm.requests()); n != 1 {
		t.Errorf("outcome reported %d times for one session, want 1", n)
	}
	if n := len(orders.orders()); n != 1 {
		t.Fatalf("dispatch re-fired %d times for one outcome, want 1: %v", n, orders.names())
	}
}

// ...but the gap cannot be closed by skipping forever, because re-judging the
// restored record IS the retry for a re-sling whose dispatch failed: a failed
// dispatch puts back the record it found (runBrokerDispatch's release), and
// the sweep's next look at it is what fires the next attempt. Once the
// re-sling has had a full dispatch window to land, a record still sitting there
// is judged again.
func TestSweepBrokerRetriesAReslingWhoseDispatchFailedOnceTheWindowHasPassed(t *testing.T) {
	rt, orders, gl, store := staleSweepFixture(t)
	ctx := context.Background()
	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	deps := sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	}
	if code := runSweep(ctx, deps); code != 0 {
		t.Fatalf("first sweep exit = %d", code)
	}

	// The re-fired dispatch runs and fails after reserving; its release puts
	// back the record it read.
	if code := runDispatch(ctx, failingDispatchDeps(t, store, rt, 2)); code != 1 {
		t.Fatalf("dispatch exit = %d, want 1 (this scenario needs it to fail)", code)
	}
	if got, _, _ := store.Get(ctx, "gonk:42:issue:3"); got.State != beadstore.StateRunning {
		t.Fatalf("after the failed dispatch the record is %s, want the restored running record", got.State)
	}

	restored, _, _ := store.Get(ctx, "gonk:42:issue:3")
	later, ok := reslingWindowEnd(restored)
	if !ok {
		t.Fatal("the restored record carries no re-sling window")
	}
	deps.Now = func() time.Time { return later }
	if code := runSweep(ctx, deps); code != 0 {
		t.Fatalf("second sweep exit = %d", code)
	}
	if n := len(orders.orders()); n != 2 {
		t.Fatalf("dispatch fired %d times, want 2: a re-sling whose dispatch failed must be retried "+
			"once its window has passed, or the bead strands in running", n)
	}
}

// A reclaim is a write from a sweep pass too. The pending-prompt record it
// listed can be replaced by its own dispatch finishing (running) between the
// List and the promote; writing the listed copy back would put a
// pending-prompt alias over a live running record.
func TestReclaimDoesNotPromoteAReservationThatChangedAfterTheSweepListedIt(t *testing.T) {
	ctx := context.Background()
	mem := beadstore.NewMemory()
	res := mpkoReservation()
	res.UpdatedAt = time.Now().Add(-2 * pendingPromptGrace) // old enough to reclaim
	if err := mem.Put(ctx, res); err != nil {
		t.Fatalf("seed: %v", err)
	}
	finished := mpkoReservation()
	finished.State = beadstore.StateRunning
	finished.ReservationID = "rsv-a2-finished"

	store := &afterListStore{Store: mem, state: beadstore.StatePendingPrompt, hook: func() {
		if err := mem.Put(ctx, finished); err != nil {
			t.Errorf("dispatch's final write: %v", err)
		}
	}}
	var log strings.Builder
	if code := runSweep(ctx, sweepDeps{
		Meter:   meterClient((&fakeOutcomeMeter{outcomeNext: "done"}).server(t)),
		Runtime: newTestRuntime(), Orders: &fakeOrders{}, GL: glabtest.New(t).Client(),
		Store: store, Log: testLogger(&log), PackDir: repoPackDir,
	}); code != 0 {
		t.Fatalf("sweep exit = %d", code)
	}
	got, _, _ := mem.Get(ctx, res.BeadAnchor)
	if got.ReservationID != finished.ReservationID || got.State != beadstore.StateRunning {
		t.Fatalf("record = %s %s, want the dispatch's own running record %s -- the reclaim wrote a stale copy over it",
			got.State, got.ReservationID, finished.ReservationID)
	}
}

// afterListStore runs hook once, right after List(state) returns: another
// writer landing between a sweep pass's read and its write.
type afterListStore struct {
	beadstore.Store
	state beadstore.State
	hook  func()
	done  bool
}

func (s *afterListStore) List(ctx context.Context, st beadstore.State) ([]beadstore.Record, error) {
	recs, err := s.Store.List(ctx, st)
	if st == s.state && !s.done {
		s.done = true
		s.hook()
	}
	return recs, err
}
