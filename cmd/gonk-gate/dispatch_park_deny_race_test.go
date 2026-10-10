package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// gonk-b2jk (and its duplicate gonk-ztd3). Meter decides "defer" or "deny" at
// one moment; dispatch's park/deny write lands LATER. When that write was an
// unconditional Put, a 'run' dispatch that reserved in between had its
// reservation erased, and a third dispatch for the same attempt then passed the
// dedupe guard: two sessions for one attempt.
//
// The interleaving, as reproduced on 2026-09-14:
//
//	D1 gets defer (or deny); its park (deny) write is HELD
//	D2 gets run, reserves, creates its session, is held mid-delivery
//	D1's write lands
//	D3 gets run for the same attempt and must NOT create a session
//
// Deterministic: every hold is a channel inside a store call or a meter
// request the dispatch under test is making. No sleeps.

// writeHoldStore is one dispatch's view of a SHARED store: every write of a
// record in state `hold` blocks until the test releases it -- whether dispatch
// makes it with Put or PutIf, so the same test runs against the bug and the
// fix. Reads and every other write pass straight through.
type writeHoldStore struct {
	beadstore.Store
	hold    beadstore.State
	held    chan struct{} // closed when the held write arrives
	release chan struct{} // close to let it proceed
	once    sync.Once
}

func newWriteHoldStore(s beadstore.Store, hold beadstore.State) *writeHoldStore {
	return &writeHoldStore{Store: s, hold: hold, held: make(chan struct{}), release: make(chan struct{})}
}

func (s *writeHoldStore) wait(r beadstore.Record) {
	if r.State != s.hold {
		return
	}
	s.once.Do(func() { close(s.held) })
	<-s.release
}

func (s *writeHoldStore) Put(ctx context.Context, r beadstore.Record) error {
	s.wait(r)
	return s.Store.Put(ctx, r)
}

func (s *writeHoldStore) PutIf(ctx context.Context, r beadstore.Record, rev string) (string, error) {
	s.wait(r)
	return s.Store.PutIf(ctx, r, rev)
}

// decisionMeter answers /decide with a defer or a deny for attempt 1.
func decisionMeter(decision string) *fakeMeter {
	resp := meterapi.DecideResponse{Decision: decision, Attempt: 1}
	switch decision {
	case meterapi.DecisionDefer:
		resp.Reason, resp.RetryAfter = "quiet_hours", time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	case meterapi.DecisionDeny:
		resp.Reason = "ladder_exhausted"
	}
	return &fakeMeter{resp: resp}
}

// parkedAtRetry is the record an unpark finds: parked by an earlier defer,
// RetryAfter passed, so gonk-sweep re-fires dispatch every tick.
func parkedAtRetry() beadstore.Record {
	a := baseDispatchArgs()
	return beadstore.Record{
		BeadAnchor: a.BeadAnchor, BeadID: a.BeadID, Project: a.Project, ProjectID: a.ProjectID,
		Rig: a.Rig, SessionKey: a.SessionKey, Trigger: a.Trigger, IssueIID: a.IssueIID,
		State: beadstore.StateParked, Attempt: 1,
		RetryAfter: time.Now().Add(-time.Minute).UTC().Truncate(time.Second),
	}
}

// Where D1 is held while D2 reserves.
type b2jkHold int

const (
	// holdD1Write: D1 has its decision and is held at its park/deny write --
	// the interleaving reproduced on 2026-09-14.
	holdD1Write b2jkHold = iota
	// holdD1Decide: D1 is held INSIDE meter's /decide. What this pins is WHEN
	// the revision the park/deny is conditional on is read: before /decide, so
	// a reservation landing while meter decides is still caught. Read after
	// /decide, that revision would be D2's reservation, and the PutIf would
	// overwrite it exactly as the unconditional Put did.
	holdD1Decide
)

// replayB2JK runs the interleaving with D1 answered `decision` by meter (its
// write is of a `hold`-state record), held at `at` until D2 has reserved and
// is inside its Start.
func replayB2JK(t *testing.T, decision string, hold beadstore.State, at b2jkHold) {
	t.Helper()
	ctx := context.Background()
	rt := newTestRuntime()
	mem := beadstore.NewMemory()
	anchor := baseDispatchArgs().BeadAnchor
	if err := mem.Put(ctx, parkedAtRetry()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// D1: meter says defer/deny; held at its write, or inside /decide.
	d1Store := newWriteHoldStore(mem, hold)
	d1Meter := decisionMeter(decision)
	d1Held, d1Release := d1Store.held, d1Store.release
	if at == holdD1Decide {
		d1Store.hold = "" // no record has an empty state: nothing held at the write
		d1Held, d1Release = make(chan struct{}), make(chan struct{})
		var once sync.Once
		d1Meter.onDecide = func() { once.Do(func() { close(d1Held); <-d1Release }) }
	}
	var d1Log syncBuilder
	d1Deps := liveDispatchDeps(t, rt, d1Meter, d1Store, &d1Log)
	d1Notes := &recordingApplier{}
	d1Deps.Apply = d1Notes
	d1Done := make(chan int, 1)
	go func() { d1Done <- runDispatch(ctx, d1Deps) }()
	select {
	case <-d1Held:
	case code := <-d1Done:
		t.Fatalf("PRECONDITION NOT REACHED: D1 exited %d before reaching its hold (%s write); log:\n%s",
			code, hold, d1Log.String())
	}

	// D2: run, attempt 1. Held inside its Start -- its reservation written,
	// its session not yet admitted.
	d2Held := make(chan string, 1)
	d2Go := make(chan struct{})
	var d2HoldOnce, d2GoOnce sync.Once
	rt.onStart = func(spec runtime.Spec) error {
		d2HoldOnce.Do(func() { d2Held <- spec.Name; <-d2Go })
		return nil
	}
	letD2Go := func() { d2GoOnce.Do(func() { close(d2Go) }) }
	defer letD2Go()
	var d2Log syncBuilder
	d2Deps := liveDispatchDeps(t, rt, attemptMeter(1), mem, &d2Log)
	d2Done := make(chan int, 1)
	go func() { d2Done <- runDispatch(ctx, d2Deps) }()
	var d2Alias string
	select {
	case d2Alias = <-d2Held:
	case code := <-d2Done:
		t.Fatalf("PRECONDITION NOT REACHED: D2 exited %d before its Start; log:\n%s", code, d2Log.String())
	}
	if got, _, _ := mem.Get(ctx, anchor); got.State != beadstore.StatePendingPrompt || got.SessionID != d2Alias {
		t.Fatalf("PRECONDITION NOT REACHED: after D2 reserved the record is %s %q, want pending-prompt %s",
			got.State, got.SessionID, d2Alias)
	}

	// D1 proceeds now: its write lands after D2's reservation.
	close(d1Release)
	if code := <-d1Done; code != 0 {
		t.Fatalf("D1 exit = %d, want 0 (a write that declines to overwrite a newer record is not an infra "+
			"failure); log:\n%s", code, d1Log.String())
	}
	if got, _, _ := mem.Get(ctx, anchor); got.State != beadstore.StatePendingPrompt || got.SessionID != d2Alias {
		t.Errorf("after D1's %s write the record is %s %q; want D2's reservation pending-prompt %s left in "+
			"place. D1's write was decided BEFORE D2 reserved and landed after it (gonk-b2jk).\nD1 log:\n%s",
			hold, got.State, got.SessionID, d2Alias, d1Log.String())
	}
	if !strings.Contains(d1Log.String(), "the record changed after this dispatch read it") {
		t.Errorf("D1 must say it declined to overwrite a newer record; log:\n%s", d1Log.String())
	}
	// Stepping aside means stopping: no "bead parked" / "bead denied" line,
	// which is what a stale write that fell through to the success path logs.
	// (For deny this is the only observable difference: it posts no note.)
	if strings.Contains(d1Log.String(), "bead parked") || strings.Contains(d1Log.String(), "bead denied") {
		t.Errorf("D1 declined its write but carried on as if it had landed; log:\n%s", d1Log.String())
	}
	// A "cannot answer yet" note on an issue a live session is now answering
	// would be false.
	d1Notes.mu.Lock()
	if len(d1Notes.notes) != 0 || len(d1Notes.updated) != 0 {
		t.Errorf("D1 posted a status note after declining its write: %v %v", d1Notes.notes, d1Notes.updated)
	}
	d1Notes.mu.Unlock()

	// D3: run, same attempt. The dedupe guard must see D2's reservation.
	var d3Log syncBuilder
	if code := runDispatch(ctx, liveDispatchDeps(t, rt, attemptMeter(1), mem, &d3Log)); code != 0 {
		t.Fatalf("D3 exit = %d, want 0; log:\n%s", code, d3Log.String())
	}

	letD2Go()
	if code := <-d2Done; code != 0 {
		t.Fatalf("D2 exit = %d, want 0; log:\n%s", code, d2Log.String())
	}

	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("TWO STARTS FOR ONE ATTEMPT: %d Start calls %+v. D1's %s write, decided before D2 "+
			"reserved, erased D2's reservation and D3 passed the dedupe guard (gonk-b2jk).\nD1 log:\n%s\nD3 log:\n%s",
			n, rt.Starts(), hold, d1Log.String(), d3Log.String())
	}
	final, _, _ := mem.Get(ctx, anchor)
	if final.State != beadstore.StateRunning || final.SessionID != d2Alias {
		t.Fatalf("final record = %s %q, want D2 running %s", final.State, final.SessionID, d2Alias)
	}
}

func TestAParkDecidedBeforeAReservationDoesNotEraseIt(t *testing.T) {
	replayB2JK(t, meterapi.DecisionDefer, beadstore.StateParked, holdD1Write)
}

func TestADenyDecidedBeforeAReservationDoesNotEraseIt(t *testing.T) {
	replayB2JK(t, meterapi.DecisionDeny, beadstore.StateNeedsHuman, holdD1Write)
}

func TestAParkWhoseDecisionOverlappedAReservationDoesNotEraseIt(t *testing.T) {
	replayB2JK(t, meterapi.DecisionDefer, beadstore.StateParked, holdD1Decide)
}

func TestADenyWhoseDecisionOverlappedAReservationDoesNotEraseIt(t *testing.T) {
	replayB2JK(t, meterapi.DecisionDeny, beadstore.StateNeedsHuman, holdD1Decide)
}

// An unreadable store before /decide: the defer/deny is written only if there
// is no record at all. A record this dispatch never saw is never overwritten
// (the same rule as reservePendingPrompt's unreadable read), and that is an
// infra failure, exit 1.
func TestADeferOrDenyAfterAnUnreadableReadDoesNotOverwriteAnExistingRecord(t *testing.T) {
	for _, tc := range []struct {
		decision string
		state    beadstore.State
	}{{meterapi.DecisionDefer, beadstore.StateParked}, {meterapi.DecisionDeny, beadstore.StateNeedsHuman}} {
		t.Run(tc.decision, func(t *testing.T) {
			ctx := context.Background()
			store := &hookStore{Memory: beadstore.NewMemory(), getErr: errors.New("workstore: connection refused")}
			seed := sweepAResling()
			if err := store.Put(ctx, seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			before, _, _ := store.Memory.Get(ctx, seed.BeadAnchor)
			var log syncBuilder
			d := liveDispatchDeps(t, newTestRuntime(), decisionMeter(tc.decision), store, &log)
			if code := runDispatch(ctx, d); code != 1 {
				t.Fatalf("exit = %d, want 1; log:\n%s", code, log.String())
			}
			after, _, _ := store.Memory.Get(ctx, seed.BeadAnchor)
			if after.Revision != before.Revision || after.State != beadstore.StateRunning {
				t.Fatalf("record = %s rev %s, want the running record it never saw left at rev %s; log:\n%s",
					after.State, after.Revision, before.Revision, log.String())
			}
			if !strings.Contains(log.String(), "will not overwrite unseen") {
				t.Fatalf("the refusal must say why; log:\n%s", log.String())
			}
		})
	}
}

// ...but with no record at all, the unreadable read changes nothing: a first
// dispatch that is deferred or denied still records it.
func TestADeferOrDenyAfterAnUnreadableReadWritesWhenThereIsNoRecord(t *testing.T) {
	for _, tc := range []struct {
		decision string
		state    beadstore.State
	}{{meterapi.DecisionDefer, beadstore.StateParked}, {meterapi.DecisionDeny, beadstore.StateNeedsHuman}} {
		t.Run(tc.decision, func(t *testing.T) {
			ctx := context.Background()
			store := &hookStore{Memory: beadstore.NewMemory(), getErr: errors.New("workstore: connection refused")}
			var log syncBuilder
			d := liveDispatchDeps(t, newTestRuntime(), decisionMeter(tc.decision), store, &log)
			if code := runDispatch(ctx, d); code != 0 {
				t.Fatalf("exit = %d, want 0; log:\n%s", code, log.String())
			}
			got, ok, _ := store.Memory.Get(ctx, baseDispatchArgs().BeadAnchor)
			if !ok || got.State != tc.state {
				t.Fatalf("record = %v %s, want %s; log:\n%s", ok, got.State, tc.state, log.String())
			}
		})
	}
}

// gonk-k3qy. The one writer that moves the record WITHOUT acting on the bead:
// the release of a dispatch that FAILED. On a bead with no prior record it
// writes a record with State "" -- "nothing dispatched yet" -- which no sweep
// List ever returns. If D1's park/deny, decided before that dispatch
// reserved, then stepped aside on ErrStale, the bead would be stranded for
// good; main's unconditional Put would have parked it. So a stale write whose
// newer record has State "" is retried, once, against that record.
//
// With a prior parked record the release restores it, the sweep unparks it,
// and D1 steps aside as for any other writer.
func TestAParkOrDenyOverAFailedDispatchsReleaseStillLands(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
		state    beadstore.State
		seed     bool
		want     beadstore.State
	}{
		{"defer/new-bead", meterapi.DecisionDefer, beadstore.StateParked, false, beadstore.StateParked},
		{"deny/new-bead", meterapi.DecisionDeny, beadstore.StateNeedsHuman, false, beadstore.StateNeedsHuman},
		{"defer/parked-bead", meterapi.DecisionDefer, beadstore.StateParked, true, beadstore.StateParked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rt := newTestRuntime()
			mem := beadstore.NewMemory()
			anchor := baseDispatchArgs().BeadAnchor
			if tc.seed {
				if err := mem.Put(ctx, parkedAtRetry()); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			d1Store := newWriteHoldStore(mem, tc.state)
			var d1Log syncBuilder
			d1Deps := liveDispatchDeps(t, rt, decisionMeter(tc.decision), d1Store, &d1Log)
			d1Done := make(chan int, 1)
			go func() { d1Done <- runDispatch(ctx, d1Deps) }()
			select {
			case <-d1Store.held:
			case code := <-d1Done:
				t.Fatalf("PRECONDITION NOT REACHED: D1 exited %d before its %s write; log:\n%s", code, tc.state, d1Log.String())
			}

			// D2: told run, reserves, fails (no key), releases.
			if code := runDispatch(ctx, failingDispatchDeps(t, mem, rt, 1)); code != 1 {
				t.Fatalf("PRECONDITION NOT REACHED: failing dispatch exit = %d, want 1", code)
			}
			mid, found, _ := mem.Get(ctx, anchor)
			if !found || mid.SessionID != "" || (tc.seed && mid.State != beadstore.StateParked) || (!tc.seed && mid.State != "") {
				t.Fatalf("PRECONDITION NOT REACHED: after the failed dispatch's release the record is found=%v %q %q",
					found, mid.State, mid.SessionID)
			}

			close(d1Store.release)
			if code := <-d1Done; code != 0 {
				t.Fatalf("D1 exit = %d, want 0; log:\n%s", code, d1Log.String())
			}
			got, _, _ := mem.Get(ctx, anchor)
			if got.State != tc.want {
				t.Fatalf("STRANDED: final record state %q, want %s. A failed dispatch's release is not a writer "+
					"acting on the bead; D1's %s must not step aside for it (gonk-k3qy).\nD1 log:\n%s",
					got.State, tc.want, tc.state, d1Log.String())
			}
		})
	}
}

// A park must not undo a deny. The retry of gonk-k3qy is only over a State ""
// record; a record another dispatch DENIED has no session either, and a park
// written over it would put a bead a human must decide back in the sweep's
// unpark loop.
func TestAParkDecidedBeforeAnotherDispatchsDenyDoesNotUndoIt(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	mem := beadstore.NewMemory()
	anchor := baseDispatchArgs().BeadAnchor
	d1Store := newWriteHoldStore(mem, beadstore.StateParked)
	var d1Log syncBuilder
	d1Done := make(chan int, 1)
	go func() {
		d1Done <- runDispatch(ctx, liveDispatchDeps(t, rt, decisionMeter(meterapi.DecisionDefer), d1Store, &d1Log))
	}()
	<-d1Store.held
	var d2Log syncBuilder
	if code := runDispatch(ctx, liveDispatchDeps(t, rt, decisionMeter(meterapi.DecisionDeny), mem, &d2Log)); code != 0 {
		t.Fatalf("denying dispatch exit = %d; log:\n%s", code, d2Log.String())
	}
	close(d1Store.release)
	if code := <-d1Done; code != 0 {
		t.Fatalf("D1 exit = %d; log:\n%s", code, d1Log.String())
	}
	if got, _, _ := mem.Get(ctx, anchor); got.State != beadstore.StateNeedsHuman {
		t.Fatalf("final record %q, want needs-human: a stale park overwrote another dispatch's deny; log:\n%s",
			got.State, d1Log.String())
	}
}

// retryRaceStore lets a test land another writer's write just before the nth
// park PutIf (1 = writeDecided's first write, 2.. = its retries).
type retryRaceStore struct {
	beadstore.Store
	parkPutIfs int
	onPutIf    func(n int)
}

func (s *retryRaceStore) PutIf(ctx context.Context, r beadstore.Record, rev string) (string, error) {
	if r.State == beadstore.StateParked {
		s.parkPutIfs++
		if s.onPutIf != nil {
			s.onPutIf(s.parkPutIfs)
		}
	}
	return s.Store.PutIf(ctx, r, rev)
}

// THE RETRIES ARE BOUNDED AT maxDecidedRetries (two). D1 read no record; a
// failed dispatch's release (State "") is in place, so D1's first write is
// stale and it retries. Then:
//   - reservation: a new dispatch reserves before the retry. It owns the bead:
//     D1 steps aside after two writes.
//   - second-release: another failed dispatch's release (State "" again) lands
//     before the retry. Still nobody's; D1 retries again and parks.
//   - releases-forever: a fresh release lands before EVERY retry. D1 stops after
//     three writes and the bead is left State "" -- stranded. That is the price
//     of the bound, pinned here so it is a visible choice and not an accident:
//     each further stale needs yet another dispatch to reserve and fail inside
//     the same milliseconds, so there is no livelock either way.
func TestTheParkRetriesOverAReleaseAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		from      int // releases/reservations land before park PutIf n for n >= from (and n <= to)
		to        int
		state     beadstore.State
		sessionID string
		wantPuts  int
		want      beadstore.State
	}{
		{"reservation", 2, 2, beadstore.StatePendingPrompt, "gonk.triage.p42.i3.a1.D4XXXXXXXXXXXXXXXXXXXXXXXX", 2, beadstore.StatePendingPrompt},
		{"second-release", 2, 2, "", "", 3, beadstore.StateParked},
		{"releases-forever", 2, 99, "", "", 3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rt := newTestRuntime()
			mem := beadstore.NewMemory()
			anchor := baseDispatchArgs().BeadAnchor
			released := parkedAtRetry()
			released.State, released.RetryAfter = "", time.Time{}
			if err := mem.Put(ctx, released); err != nil {
				t.Fatalf("seed: %v", err)
			}
			newer := released
			newer.State, newer.SessionID = tc.state, tc.sessionID
			store := &retryRaceStore{Store: mem}
			store.onPutIf = func(n int) {
				if n < tc.from || n > tc.to {
					return
				}
				if err := mem.Put(ctx, newer); err != nil { // a fresh revision
					t.Errorf("newer write: %v", err)
				}
			}
			pre := &readNothingOnceStore{retryRaceStore: store}
			var log syncBuilder
			if code := runDispatch(ctx, liveDispatchDeps(t, rt, decisionMeter(meterapi.DecisionDefer), pre, &log)); code != 0 {
				t.Fatalf("exit = %d; log:\n%s", code, log.String())
			}
			if store.parkPutIfs != tc.wantPuts {
				t.Fatalf("park PutIfs = %d, want %d; log:\n%s", store.parkPutIfs, tc.wantPuts, log.String())
			}
			if got, _, _ := mem.Get(ctx, anchor); got.State != tc.want {
				t.Fatalf("final record %q, want %q; log:\n%s", got.State, tc.want, log.String())
			}
		})
	}
}

// readNothingOnceStore answers the first Get with "no record": a read that
// happened before the failed dispatch's release was written.
type readNothingOnceStore struct {
	*retryRaceStore
	done bool
}

func (s *readNothingOnceStore) Get(ctx context.Context, k string) (beadstore.Record, bool, error) {
	if !s.done {
		s.done = true
		return beadstore.Record{}, false, nil
	}
	return s.retryRaceStore.Get(ctx, k)
}

// finishedIssue is the record a finished attempt leaves (the sweep's `done`),
// or a denied one (`needs-human`). The bead key is one per issue, so a NEW
// trigger on that issue dispatches against it.
func finishedIssue(st beadstore.State) beadstore.Record {
	a := baseDispatchArgs()
	r := beadstore.Record{
		BeadAnchor: a.BeadAnchor, BeadID: a.BeadID, Project: a.Project, ProjectID: a.ProjectID,
		Rig: a.Rig, SessionKey: a.SessionKey, Trigger: a.Trigger, IssueIID: a.IssueIID,
		State: st, Attempt: 1,
	}
	if st == beadstore.StateDone {
		r.SessionID = "gonk.triage.p42.i3.a1.OLDXXXXXXXXXXXXXXXXXXXXXXX"
	}
	return r
}

// gonk-gate K1 (verification of ed48031). A failed dispatch's release restores
// the PRIOR record, not only State "". A new trigger on a finished issue (prior
// `done`) or a denied one (prior `needs-human`) is told defer; a concurrent
// dispatch is told run, fails, and puts that prior record back under a new
// revision. Neither `done` nor `needs-human` is listed by any sweep, so a park
// that stepped aside there dropped the new trigger for good. Main's
// unconditional Put parked it, and so must this: the record is the one meter
// decided on, so the outcome must be the one it would have been had the
// failed dispatch never run.
func TestAParkOverAFailedDispatchsReleaseOfAFinishedIssueStillLands(t *testing.T) {
	for _, prior := range []beadstore.State{beadstore.StateDone, beadstore.StateNeedsHuman} {
		t.Run(string(prior), func(t *testing.T) {
			ctx := context.Background()
			rt := newTestRuntime()
			mem := beadstore.NewMemory()
			anchor := baseDispatchArgs().BeadAnchor
			if err := mem.Put(ctx, finishedIssue(prior)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			d1Store := newWriteHoldStore(mem, beadstore.StateParked)
			var d1Log syncBuilder
			d1Done := make(chan int, 1)
			go func() {
				d1Done <- runDispatch(ctx, liveDispatchDeps(t, rt, decisionMeter(meterapi.DecisionDefer), d1Store, &d1Log))
			}()
			select {
			case <-d1Store.held:
			case code := <-d1Done:
				t.Fatalf("PRECONDITION NOT REACHED: D1 exited %d before its park; log:\n%s", code, d1Log.String())
			}
			if code := runDispatch(ctx, failingDispatchDeps(t, mem, rt, 2)); code != 1 {
				t.Fatalf("PRECONDITION NOT REACHED: failing dispatch exit = %d, want 1", code)
			}
			if mid, _, _ := mem.Get(ctx, anchor); mid.State != prior {
				t.Fatalf("PRECONDITION NOT REACHED: after the release the record is %q, want %q restored", mid.State, prior)
			}
			close(d1Store.release)
			if code := <-d1Done; code != 0 {
				t.Fatalf("D1 exit = %d; log:\n%s", code, d1Log.String())
			}
			if got, _, _ := mem.Get(ctx, anchor); got.State != beadstore.StateParked {
				t.Fatalf("DROPPED: final record %q, want parked. A failed dispatch's release put back the %s "+
					"record meter decided on; the new trigger's park must not step aside for it.\nD1 log:\n%s",
					got.State, prior, d1Log.String())
			}
		})
	}
}

// The reasoned case: the SWEEP writes `done` (the previous attempt's session
// judged finished) between this dispatch's read and its park. A `done` record
// is a finished attempt, not a writer acting on the bead; the new trigger's
// park lands over it, as main's Put did. Covered whether `done` lands before
// the first write or between the re-read and the retry.
func TestAParkIsNotDroppedByTheSweepsDoneWrite(t *testing.T) {
	running := finishedIssue(beadstore.StateRunning)
	running.SessionID = "gonk.triage.p42.i3.a1.OLDXXXXXXXXXXXXXXXXXXXXXXX"
	for _, tc := range []struct {
		name string
		at   int // the park PutIf the sweep's done write lands before
	}{{"before-the-write", 1}, {"between-reread-and-retry", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rt := newTestRuntime()
			mem := beadstore.NewMemory()
			anchor := baseDispatchArgs().BeadAnchor
			if err := mem.Put(ctx, running); err != nil {
				t.Fatalf("seed: %v", err)
			}
			sweepDone := func() {
				cur, _, _ := mem.Get(ctx, anchor)
				cur.State = beadstore.StateDone
				if _, err := mem.PutIf(ctx, cur, cur.Revision); err != nil {
					t.Errorf("sweep done: %v", err)
				}
			}
			store := &retryRaceStore{Store: mem}
			store.onPutIf = func(n int) {
				switch {
				case tc.at == 1 && n == 1:
					sweepDone()
				case tc.at == 2 && n == 1:
					// Something moved the record first, so the write is stale
					// and re-reads: a failed dispatch's release (State "").
					cur, _, _ := mem.Get(ctx, anchor)
					cur.State, cur.SessionID = "", ""
					if err := mem.Put(ctx, cur); err != nil {
						t.Errorf("release: %v", err)
					}
				case tc.at == 2 && n == 2:
					sweepDone()
				}
			}
			var log syncBuilder
			if code := runDispatch(ctx, liveDispatchDeps(t, rt, decisionMeter(meterapi.DecisionDefer), store, &log)); code != 0 {
				t.Fatalf("exit = %d; log:\n%s", code, log.String())
			}
			if got, _, _ := mem.Get(ctx, anchor); got.State != beadstore.StateParked {
				t.Fatalf("DROPPED: final record %q, want parked; log:\n%s", got.State, log.String())
			}
		})
	}
}

// sessionRecord is a record holding a session for this bead: a pending-prompt
// reservation or a running record, with every field a dispatch sets.
func sessionRecord(alias string, attempt int, st beadstore.State) beadstore.Record {
	r := finishedIssue(st)
	r.State, r.SessionID, r.Attempt = st, alias, attempt
	r.Rung, r.Model, r.ReservationID = "cheap", "m", "rsv-"+alias
	r.ReservationExpiresAt = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	return r
}

// deferAgainst runs one dispatch that meter defers, against a store seeded
// with seed; onPutIf(mem, n) lands other writers' writes just before its nth
// park PutIf. It returns the final record and the dispatch's log.
func deferAgainst(t *testing.T, seed beadstore.Record, onPutIf func(mem beadstore.Store, n int)) (beadstore.Record, string) {
	t.Helper()
	ctx := context.Background()
	mem := beadstore.NewMemory()
	if err := mem.Put(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := &retryRaceStore{Store: mem}
	store.onPutIf = func(n int) { onPutIf(mem, n) }
	var log syncBuilder
	if code := runDispatch(ctx, liveDispatchDeps(t, newTestRuntime(), decisionMeter(meterapi.DecisionDefer), store, &log)); code != 0 {
		t.Fatalf("exit = %d; log:\n%s", code, log.String())
	}
	got, _, _ := mem.Get(ctx, seed.BeadAnchor)
	return got, log.String()
}

// writeOver writes r conditionally over whatever the store holds now, as
// another dispatch's reservation, release or deny does.
func writeOver(t *testing.T, mem beadstore.Store, r beadstore.Record) {
	t.Helper()
	ctx := context.Background()
	cur, _, _ := mem.Get(ctx, r.BeadAnchor)
	if _, err := mem.PutIf(ctx, r, cur.Revision); err != nil {
		t.Errorf("write %s %q: %v", r.State, r.SessionID, err)
	}
}

// THE SESSION ID IS PART OF "THE RECORD METER DECIDED ON". D1 decided on D2's
// reservation under alias X. D2 fails and restores its prior (parked); D4 then
// reserves the SAME attempt under a fresh alias Y, every other field equal. Only
// the SessionID tells D4's reservation from the one D1 read. A content check
// that ignored it would take D4's live reservation for "the decided-on record"
// and park over it -- and a fifth dispatch would pass the dedupe guard: the
// b2jk duplicate session itself.
func TestAParkDoesNotTakeAFreshReservationWithTheSameFieldsForTheOneItDecidedOn(t *testing.T) {
	rX := sessionRecord("gonk.triage.p42.i3.a1.XXXXXXXXXXXXXXXXXXXXXXXXXX", 1, beadstore.StatePendingPrompt)
	rY := rX
	rY.SessionID = "gonk.triage.p42.i3.a1.YYYYYYYYYYYYYYYYYYYYYYYYYY"
	got, log := deferAgainst(t, rX, func(mem beadstore.Store, n int) {
		if n != 1 {
			return
		}
		writeOver(t, mem, parkedAtRetry()) // D2's release, to its prior
		writeOver(t, mem, rY)              // D4's reservation
	})
	if got.State != beadstore.StatePendingPrompt || got.SessionID != rY.SessionID {
		t.Fatalf("DUPLICATE RISK: D4's live reservation %s was overwritten; final %q %q. A reservation that "+
			"differs from the decided-on record only in its session id is a NEW claim.\nlog:\n%s",
			rY.SessionID, got.State, got.SessionID, log)
	}
}

// The deny companion: D1 decided on a needs-human record (an earlier deny, for
// attempt 1). ANOTHER dispatch is then denied too, for attempt 2 -- same state,
// different record. That is a new meter decision D1's defer never saw; it wins.
// A content check on State alone would take it for the decided-on record and
// park over a fresh deny.
func TestAParkDoesNotOverwriteADifferentDenyOfANeedsHumanIssue(t *testing.T) {
	prior := finishedIssue(beadstore.StateNeedsHuman)
	laterDeny := prior
	laterDeny.Attempt = 2
	got, log := deferAgainst(t, prior, func(mem beadstore.Store, n int) {
		if n == 1 {
			writeOver(t, mem, laterDeny)
		}
	})
	if got.State != beadstore.StateNeedsHuman || got.Attempt != 2 {
		t.Fatalf("final record %q attempt %d, want the later deny (needs-human, attempt 2) left in place: a park "+
			"decided on an earlier deny overwrote a newer one.\nlog:\n%s", got.State, got.Attempt, log)
	}
}

// A fresh reservation survives wherever it lands among D1's writes: before its
// first write, or after one or two failed dispatches' releases (each restoring
// the needs-human prior D1 decided on, which D1 would write over).
func TestAFreshReservationSurvivesAtEverySlotOfTheParkRetries(t *testing.T) {
	prior := finishedIssue(beadstore.StateNeedsHuman)
	fresh := sessionRecord("gonk.triage.p42.i3.a2.D4XXXXXXXXXXXXXXXXXXXXXXXX", 2, beadstore.StatePendingPrompt)
	for slot := 1; slot <= 3; slot++ {
		t.Run(fmt.Sprintf("slot-%d", slot), func(t *testing.T) {
			got, log := deferAgainst(t, prior, func(mem beadstore.Store, n int) {
				if n < slot { // a dispatch reserves, fails, and restores the prior
					writeOver(t, mem, sessionRecord("gonk.triage.p42.i3.a2.D3XXXXXXXXXXXXXXXXXXXXXXXX", 2,
						beadstore.StatePendingPrompt))
					writeOver(t, mem, prior)
				}
				if n == slot {
					writeOver(t, mem, fresh)
				}
			})
			if got.State != beadstore.StatePendingPrompt || got.SessionID != fresh.SessionID {
				t.Fatalf("DUPLICATE RISK: the reservation landing before park write %d was overwritten; final %q %q"+
					"\nlog:\n%s", slot, got.State, got.SessionID, log)
			}
		})
	}
}
