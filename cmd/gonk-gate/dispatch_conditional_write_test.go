package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// gonk-y4m7. gonk-p8w8 made the SWEEP's store writes conditional; these pin the
// same property for DISPATCH's own writes in runBrokerDispatch: the
// pending-prompt reservation and, above all, the release that runs when a
// dispatch fails.
//
// The incident, from the bd comment log of go-3tbq (gonk:75:issue:73) on
// 2026-09-13 -- see docs/plans/2026-09-13-y4m7-dispatch-conditional-writes.md:
//
//	15:49:04  sweep A writes running a1        <- what MPKO reads as its prior
//	15:49:24  MPKO writes its reservation for attempt 2
//	15:50:36  sweep B's stale write erases it   (fixed by p8w8)
//	15:52:00  FYKO reads running a1, passes the guard, writes its reservation
//	15:52:31  MPKO fails; its release puts back running a1 OVER FYKO's reservation
//
// Every test here is deterministic: the other writer acts from inside a store
// call or a meter request the dispatch under test is making, never on a timer.

// attemptMeter is a meter that says run, for the given attempt, on the key the
// dispatch can resolve.
func attemptMeter(attempt int) *fakeMeter {
	return &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m",
		Attempt: attempt, ReservationID: fmt.Sprintf("rsv-a%d", attempt),
	}}
}

// liveDispatchDeps is a dispatch that can succeed: key resolvable, forge
// answering.
func liveDispatchDeps(t *testing.T, rt *testRuntime, fm *fakeMeter, store beadstore.Store, log io.Writer) dispatchDeps {
	t.Helper()
	return dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Forge: promptTestForge(), Args: baseDispatchArgs(),
		Log: testLogger(log),
	}
}

// sweepAResling is the record sweep A wrote at 15:49:04: the attempt-1 broker
// record, back in running with its outcome reported and its re-sling fired.
func sweepAResling() beadstore.Record {
	a := baseDispatchArgs()
	return beadstore.Record{
		BeadAnchor: a.BeadAnchor, BeadID: a.BeadID, Project: a.Project, ProjectID: a.ProjectID,
		Rig: a.Rig, SessionKey: a.SessionKey, Trigger: a.Trigger, IssueIID: a.IssueIID,
		State: beadstore.StateRunning, Rung: "cheap", Model: "m", Attempt: 1,
		ReservationID: "rsv-a1", SessionID: testSession,
		ReslungAt: time.Now().Add(-time.Minute).UTC().Truncate(time.Second),
	}
}

// hookStore is a Memory store that lets a test act at an exact store call.
// Hooks run WITHOUT the lock and may call the store (or runDispatch) again;
// each fires on the call number it is given, so a nested call does not re-fire
// it.
type hookStore struct {
	*beadstore.Memory

	mu     sync.Mutex
	gets   int
	putIfs int

	getErr      error                                                       // every Get fails with this, when set
	afterGet    func(n int)                                                 // after the nth Get has read
	beforePutIf func(n int)                                                 // before the nth PutIf
	putIf       func(n int, r beadstore.Record, rev string) (string, error) // replaces the nth PutIf when it returns handled
}

func (s *hookStore) Get(ctx context.Context, k string) (beadstore.Record, bool, error) {
	s.mu.Lock()
	s.gets++
	n := s.gets
	s.mu.Unlock()
	if s.getErr != nil {
		return beadstore.Record{}, false, s.getErr
	}
	r, ok, err := s.Memory.Get(ctx, k)
	if s.afterGet != nil {
		s.afterGet(n)
	}
	return r, ok, err
}

func (s *hookStore) PutIf(ctx context.Context, r beadstore.Record, rev string) (string, error) {
	s.mu.Lock()
	s.putIfs++
	n := s.putIfs
	s.mu.Unlock()
	if s.beforePutIf != nil {
		s.beforePutIf(n)
	}
	if s.putIf != nil {
		return s.putIf(n, r, rev)
	}
	return s.Memory.PutIf(ctx, r, rev)
}

// THE 15:52:31 WRITE, REPLAYED. MPKO reserves; a stale write erases the
// reservation; a REAL second dispatch (FYKO) reads that, reserves, and is held
// inside its Start; MPKO's Start is refused, so MPKO fails and releases. The
// release must leave FYKO's reservation -- and FYKO's session -- alone.
//
// Under the SessionRuntime both dispatches are attempt 2 of one bead, so they
// share a session NAME (plan D2). What tells the reservations apart is the
// store revision, which is exactly what the release's compare-and-swap is on.
func TestAFailedDispatchsReleaseDoesNotEraseTheReservationOfTheDispatchAfterIt(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	mem := beadstore.NewMemory()
	anchor := baseDispatchArgs().BeadAnchor

	if err := mem.Put(ctx, sweepAResling()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var fyLog, mpLog syncBuilder
	fyDeps := liveDispatchDeps(t, rt, attemptMeter(2), mem, &fyLog)
	fyDone := make(chan int, 1)
	fyHeld := make(chan struct{})
	fyGo := make(chan struct{})
	var goOnce sync.Once
	letFYKOGo := func() { goOnce.Do(func() { close(fyGo) }) }
	defer letFYKOGo()

	// Start #1 is MPKO's; the rest of the incident happens inside it. Start #2
	// is FYKO's, held until the test lets it go.
	var mu sync.Mutex
	calls := 0
	rt.onStart = func(runtime.Spec) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1: // MPKO: its reservation is written.
			// 15:50:36: the stale write (pre-p8w8 sweep B) erases it.
			if err := mem.Put(ctx, sweepAResling()); err != nil {
				t.Errorf("stale write: %v", err)
			}
			// 15:52:00: FYKO, run until it is inside its own Start.
			go func() { fyDone <- runDispatch(ctx, fyDeps) }()
			select {
			case <-fyHeld:
			case code := <-fyDone:
				t.Errorf("FYKO returned %d before reaching Start; log:\n%s", code, fyLog.String())
				fyDone <- code
			}
			return errors.New("the API server refused the Job")
		case 2: // FYKO
			close(fyHeld)
			<-fyGo
		}
		return nil
	}

	if code := runDispatch(ctx, liveDispatchDeps(t, rt, attemptMeter(2), mem, &mpLog)); code != 1 {
		t.Fatalf("MPKO exit = %d, want 1 (its start was refused); log:\n%s", code, mpLog.String())
	}
	got, ok, err := mem.Get(ctx, anchor)
	if err != nil || !ok {
		t.Fatalf("record after MPKO's release: ok=%v err=%v", ok, err)
	}
	if got.State != beadstore.StatePendingPrompt || got.SessionID != "gonk-triage-p42-i3-a2" || got.Attempt != 2 {
		t.Fatalf("after MPKO failed and released, the record is %s %s Attempt %d; want FYKO's reservation "+
			"pending-prompt Attempt 2.\nMPKO's release put back the record it read before it started, over a "+
			"reservation another dispatch wrote since: the 15:52:31 write in go-3tbq's log (gonk-y4m7).\nMPKO log:\n%s",
			got.State, got.SessionID, got.Attempt, mpLog.String())
	}
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("stops = %d; MPKO never admitted a session, so it must stop none (and never FYKO's)", n)
	}
	if !strings.Contains(mpLog.String(), "the record changed after this dispatch reserved it") {
		t.Fatalf("a release that declined to overwrite must say so; MPKO log:\n%s", mpLog.String())
	}

	// And FYKO, untouched, finishes normally.
	letFYKOGo()
	if code := <-fyDone; code != 0 {
		t.Fatalf("FYKO exit = %d, want 0; log:\n%s", code, fyLog.String())
	}
	final, _, _ := mem.Get(ctx, anchor)
	if final.State != beadstore.StateRunning || final.SessionID != "gonk-triage-p42-i3-a2" {
		t.Fatalf("final record = %s %s, want FYKO running gonk-triage-p42-i3-a2", final.State, final.SessionID)
	}
	if live := rt.live(t); len(live) != 1 || live[0] != "gonk-triage-p42-i3-a2" {
		t.Fatalf("live sessions = %v, want only FYKO's", live)
	}
}

// THE RESERVATION IS A COMPARE-AND-SWAP. Two dispatches for one attempt that
// both read the record before either reserves used to both pass the guard.
// Here B runs start to finish between A's read and A's reservation.
func TestTwoDispatchesThatBothReadBeforeEitherReservesCreateOneSession(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	store := &hookStore{Memory: beadstore.NewMemory()}
	fm := attemptMeter(1)
	var bLog, aLog syncBuilder
	store.afterGet = func(n int) {
		// Get #1 is runDispatch's read before /decide (gonk-b2jk); #2 is the
		// reservation's read, which is the one this test acts after.
		if n != 2 {
			return
		}
		if code := runDispatch(ctx, liveDispatchDeps(t, rt, fm, store, &bLog)); code != 0 {
			t.Errorf("dispatch B exit = %d; log:\n%s", code, bLog.String())
		}
	}
	if code := runDispatch(ctx, liveDispatchDeps(t, rt, fm, store, &aLog)); code != 0 {
		t.Fatalf("dispatch A exit = %d, want 0 (a duplicate is a no-op); log:\n%s", code, aLog.String())
	}
	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("two dispatches for attempt 1 called Start %d times: A reserved over a record that had "+
			"changed since it read it; log:\n%s", n, aLog.String())
	}
	got, _, _ := store.Memory.Get(ctx, baseDispatchArgs().BeadAnchor)
	if got.State != beadstore.StateRunning || got.SessionID != testSession {
		t.Fatalf("record = %s %s, want B's running record %s", got.State, got.SessionID, testSession)
	}
}

// stampBetweenReadAndReserve makes the dispatch's reservation read (its second Get) be followed,
// before its reservation, by a write that is NOT a reservation: a gonk-sweep
// re-sling write re-stamping the record (a later pass re-slinging the same
// running record once its window ended, while this dispatch -- from intake, or
// an earlier fire -- was between its read and its reservation). It is
// conditional on the revision the dispatch just read.
//
// (This used to be the sweep's ReslingFiredAt confirming write, which p8w8
// removed when the fire-by deadline replaced it. The dispatch-side question is
// unchanged: a changed revision is not a competing reservation.)
func stampBetweenReadAndReserve(t *testing.T, store *hookStore, stamp time.Time) {
	store.afterGet = func(n int) {
		// Get #1 is runDispatch's read before /decide (gonk-b2jk); #2 is the
		// reservation's read, which is the one this test acts after.
		if n != 2 {
			return
		}
		cur, _, _ := store.Memory.Get(context.Background(), baseDispatchArgs().BeadAnchor)
		cur.ReslungAt, cur.ReslingFireBy = stamp, reslingFireBy(stamp)
		if _, err := store.Memory.PutIf(context.Background(), cur, cur.Revision); err != nil {
			t.Errorf("sweep's re-sling write: %v", err)
		}
	}
}

// An ErrStale on the reservation is NOT "someone else reserved". A write that
// is not a reservation -- a sweep re-stamping the running record -- can land
// between the dispatch's read and its reservation. Backing off there would drop
// a legitimate dispatch: no session, and the bead waits out a window.
func TestANonReservationWriteBeforeTheReservationDoesNotDropTheDispatch(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	store := &hookStore{Memory: beadstore.NewMemory()}
	if err := store.Put(ctx, sweepAResling()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stampBetweenReadAndReserve(t, store, time.Now().UTC().Truncate(time.Second))

	var log syncBuilder
	if code := runDispatch(ctx, liveDispatchDeps(t, rt, attemptMeter(2), store, &log)); code != 0 {
		t.Fatalf("exit = %d, want 0; log:\n%s", code, log.String())
	}
	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("started %d sessions, want 1: a re-stamp is not a competing reservation; log:\n%s",
			n, log.String())
	}
	got, _, _ := store.Memory.Get(ctx, baseDispatchArgs().BeadAnchor)
	if got.State != beadstore.StateRunning || got.SessionID != "gonk-triage-p42-i3-a2" || got.Attempt != 2 {
		t.Fatalf("record = %s %s Attempt %d, want this dispatch running for attempt 2", got.State, got.SessionID, got.Attempt)
	}
}

// ...and when that dispatch then FAILS, its release restores the record its
// reservation actually replaced -- the re-stamped one -- not the older copy it
// read first. The sweep measures its re-sling window from that record's
// fire-by deadline.
func TestAFailedDispatchRestoresTheRecordItsReservationReplaced(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	store := &hookStore{Memory: beadstore.NewMemory()}
	if err := store.Put(ctx, sweepAResling()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stamp := time.Now().UTC().Truncate(time.Second)
	stampBetweenReadAndReserve(t, store, stamp)

	if code := runDispatch(ctx, failingDispatchDeps(t, store, rt, 2)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	got, _, _ := store.Memory.Get(ctx, baseDispatchArgs().BeadAnchor)
	if got.State != beadstore.StateRunning || got.Attempt != 1 || !got.ReslingFireBy.Equal(reslingFireBy(stamp)) {
		t.Fatalf("record = %s Attempt %d ReslingFireBy %v, want the re-stamped running a1 record (fire-by %v) "+
			"restored", got.State, got.Attempt, got.ReslingFireBy, reslingFireBy(stamp))
	}
}

// A record that keeps changing between every read and every reservation is
// being written by someone. Dispatch gives up WITHOUT a session rather than
// create one it could not reserve, and writes nothing over that writer.
func TestADispatchThatCannotReserveAgainstAChangingRecordCreatesNoSession(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	store := &hookStore{Memory: beadstore.NewMemory()}
	foreign := sweepAResling()
	if err := store.Put(ctx, foreign); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store.beforePutIf = func(int) {
		if err := store.Put(ctx, foreign); err != nil {
			t.Errorf("foreign write: %v", err)
		}
	}
	var log syncBuilder
	if code := runDispatch(ctx, liveDispatchDeps(t, rt, attemptMeter(2), store, &log)); code != 1 {
		t.Fatalf("exit = %d, want 1; log:\n%s", code, log.String())
	}
	if n := len(rt.Starts()); n != 0 {
		t.Fatalf("started %d sessions without a reservation it could hold", n)
	}
	got, _, _ := store.Memory.Get(ctx, foreign.BeadAnchor)
	if got.State != foreign.State || got.SessionID != foreign.SessionID {
		t.Fatalf("record = %s %s, want the other writer's %s %s", got.State, got.SessionID, foreign.State, foreign.SessionID)
	}
	if !strings.Contains(log.String(), "kept changing") {
		t.Fatalf("giving up on a contended record must be logged; log:\n%s", log.String())
	}
}

// A store this dispatch cannot READ gives it no revision to reserve against.
// It must not write over a record it never saw -- here a parked bead the sweep
// would unpark -- and a failed dispatch must leave that record as it was.
func TestAnUnreadableStoreDoesNotLetADispatchOverwriteARecordItNeverSaw(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	store := &hookStore{Memory: beadstore.NewMemory(), getErr: errors.New("bd: dolt unavailable")}
	parked := sweepAResling()
	parked.State, parked.SessionID = beadstore.StateParked, ""
	parked.RetryAfter = time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	if err := store.Put(ctx, parked); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var log syncBuilder
	deps := failingDispatchDeps(t, store, rt, 2)
	deps.Log = testLogger(&log)
	if code := runDispatch(ctx, deps); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	got, _, _ := store.Memory.Get(ctx, parked.BeadAnchor)
	if got.State != beadstore.StateParked || !got.RetryAfter.Equal(parked.RetryAfter) {
		t.Fatalf("record = %s RetryAfter %v, want the parked record untouched; log:\n%s",
			got.State, got.RetryAfter, log.String())
	}
	if !strings.Contains(log.String(), "could not read") {
		t.Fatalf("log:\n%s", log.String())
	}
}

// A reservation write that REPORTS an error may still have landed (BdCLI can
// fail after its append). The release then finds its own session in the store and
// undoes it, instead of leaving a dead reservation for the sweep's reclaim.
func TestAReservationThatErroredButLandedIsStillReleased(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	store := &hookStore{Memory: beadstore.NewMemory()}
	prior := sweepAResling()
	if err := store.Put(ctx, prior); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store.putIf = func(n int, r beadstore.Record, rev string) (string, error) {
		if n == 1 {
			if _, err := store.Memory.PutIf(ctx, r, rev); err != nil {
				t.Errorf("underlying reservation write: %v", err)
			}
			return "", errors.New("bd: the comment was appended but re-reading the log failed")
		}
		return store.Memory.PutIf(ctx, r, rev)
	}
	if code := runDispatch(ctx, failingDispatchDeps(t, store, rt, 2)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	got, _, _ := store.Memory.Get(ctx, prior.BeadAnchor)
	if got.State != prior.State || got.SessionID != prior.SessionID || got.Attempt != prior.Attempt {
		t.Fatalf("record = %s %s Attempt %d, want the prior %s %s restored", got.State, got.SessionID,
			got.Attempt, prior.State, prior.SessionID)
	}
}

// ...but a reservation write that errored and did NOT land leaves the dispatch
// nothing of its own to release. A record another writer puts there meanwhile
// must survive the failed dispatch.
func TestAReservationThatNeverLandedReleasesNothingOverALaterWriter(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	store := &hookStore{Memory: beadstore.NewMemory()}
	if err := store.Put(ctx, sweepAResling()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	later := sweepAResling()
	later.State, later.Attempt = beadstore.StatePendingPrompt, 2
	later.SessionID = "gonk-triage-p42-i3-a2"
	later.ReservationID = "rsv-later"
	store.putIf = func(n int, r beadstore.Record, rev string) (string, error) {
		if n == 1 {
			// Nothing written -- and another dispatch reserves right after.
			if err := store.Put(ctx, later); err != nil {
				t.Errorf("later write: %v", err)
			}
			return "", errors.New("bd: label add failed")
		}
		return store.Memory.PutIf(ctx, r, rev)
	}
	if code := runDispatch(ctx, failingDispatchDeps(t, store, rt, 2)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	got, _, _ := store.Memory.Get(ctx, later.BeadAnchor)
	if got.State != later.State || got.SessionID != later.SessionID {
		t.Fatalf("record = %s %s, want the later writer's reservation %s untouched", got.State, got.SessionID, later.SessionID)
	}
}

// The RUNNING write stays unconditional, deliberately (plan section 3): the
// session is admitted, and a record that does not land leaves a live agent no
// sweep will ever see. A change to the record after the reservation -- here
// the reservation promoted by someone else while the session was starting --
// must neither stop that write nor stop the agent.
func TestARecordChangedAfterTheReservationDoesNotStopTheRunningWriteOrStopTheAgent(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	mem := beadstore.NewMemory()
	var once sync.Once
	rt.afterStart = func(runtime.Handle) {
		once.Do(func() {
			cur, _, _ := mem.Get(ctx, baseDispatchArgs().BeadAnchor)
			cur.State = beadstore.StateRunning
			if _, err := mem.PutIf(ctx, cur, cur.Revision); err != nil {
				t.Errorf("other writer: %v", err)
			}
		})
	}
	var log syncBuilder
	if code := runDispatch(ctx, liveDispatchDeps(t, rt, attemptMeter(1), mem, &log)); code != 0 {
		t.Fatalf("exit = %d, want 0; log:\n%s", code, log.String())
	}
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("%d stops: a live, admitted session was torn down", n)
	}
	got, _, _ := mem.Get(ctx, baseDispatchArgs().BeadAnchor)
	if got.State != beadstore.StateRunning || got.SessionID != testSession {
		t.Fatalf("record = %s %s, want this dispatch's running record", got.State, got.SessionID)
	}
}
