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

// runDispatch-level tests of the START under the SessionRuntime (gonk-t55):
// what dispatch does when the runtime admits, refuses, or cannot be reached in
// time, and what it leaves behind when Gas City's order-timeout SIGTERM lands
// around it. They replace the Gas City delivery-wait tests (the wait no longer
// exists: the prompt is mounted, not fetched).

func startDeps(t *testing.T, rt runtime.SessionRuntime, fm *fakeMeter, logged *strings.Builder) dispatchDeps {
	t.Helper()
	return dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: beadstore.NewMemory(),
		Forge: promptTestForge(), Args: baseDispatchArgs(),
		Log:   testLogger(logged),
		Clock: newFakeClock(),
	}
}

func runMeter() *fakeMeter {
	return &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun,
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Rung:     "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
}

// testBudget is an order budget whose start deadline lands exactly `start`
// after process start, with 1s stop and release reserves and no pre-exec or
// exit slack (1ns: zero would mean "production default").
func testBudget(start time.Duration) orderBudget {
	return orderBudget{
		Timeout: start + 2*time.Second + 2*time.Nanosecond,
		PreExec: time.Nanosecond, Exit: time.Nanosecond,
		Close: time.Second, Release: time.Second,
	}
}

// A START ERROR IS A DISPATCH FAILURE, NEVER SILENT (plan P3 item 4; gonk-alw's
// surviving requirement). The runtime refused to admit the session: dispatch
// exits 1 (infra -- the re-sling decides again), says so at ERROR, and puts
// back EXACTLY the record its reservation replaced -- here a parked bead,
// which must stay parked so the sweep still unparks it (T-19).
func TestDispatchStartErrorExitsOneAndRestoresTheRecord(t *testing.T) {
	rt := newTestRuntime()
	rt.SetStartErr(errors.New("admission webhook denied the request"))
	var logged strings.Builder
	deps := startDeps(t, rt, runMeter(), &logged)
	parked := beadstore.Record{
		BeadAnchor: baseDispatchArgs().BeadAnchor, State: beadstore.StateParked,
		RetryAfter: time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC), Attempt: 1, Trigger: "issue-triage",
	}
	if err := deps.Store.Put(context.Background(), parked); err != nil {
		t.Fatal(err)
	}

	if code := runDispatch(context.Background(), deps); code != 1 {
		t.Fatalf("exit = %d, want 1 -- a refused start is an infra failure the re-sling retries", code)
	}
	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("Start called %d times, want exactly 1 (no retry loop inside dispatch)", n)
	}
	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("live sessions = %v after a refused start, want none", live)
	}
	got, ok, err := deps.Store.Get(context.Background(), parked.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("record gone (ok=%v err=%v)", ok, err)
	}
	if got.State != beadstore.StateParked || !got.RetryAfter.Equal(parked.RetryAfter) || got.SessionID != "" {
		t.Fatalf("record = %+v, want the parked record the reservation replaced, restored exactly", got)
	}
	out := logged.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "session start failed") ||
		!strings.Contains(out, "admission webhook denied") {
		t.Fatalf("the refused start was not reported at ERROR with its cause; log:\n%s", out)
	}
}

// Pre-start work can eat the order budget (56s was observed live between the
// order run and the create). With the start deadline already past, no session
// is started at all -- and the reservation is released as on any other failed
// dispatch.
func TestDispatchStartsNoSessionWithoutTimeToRecordIt(t *testing.T) {
	rt := newTestRuntime()
	var logged strings.Builder
	deps := startDeps(t, rt, runMeter(), &logged)
	clk := deps.Clock.(*fakeClock)
	// 211s already gone of the 210s the defaults allow before a start.
	deps.StartedAt = clk.Now().Add(-211 * time.Second)
	if code := runDispatch(context.Background(), deps); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if n := len(rt.Starts()); n != 0 {
		t.Fatalf("Start called %d times past the start deadline, want none", n)
	}
	rec, ok, err := deps.Store.Get(context.Background(), baseDispatchArgs().BeadAnchor)
	if err != nil {
		t.Fatal(err)
	}
	if ok && rec.State == beadstore.StatePendingPrompt {
		t.Fatalf("reservation left in place (%+v); a refused start must release it", rec)
	}
	if !strings.Contains(logged.String(), "too little of the order timeout is left") {
		t.Fatalf("refusal not logged; log:\n%s", logged.String())
	}
}

// ...and one second inside the deadline, it does start: the check is the
// budget, not a blanket refusal.
func TestDispatchStartsASessionInsideTheStartDeadline(t *testing.T) {
	rt := newTestRuntime()
	var logged strings.Builder
	deps := startDeps(t, rt, runMeter(), &logged)
	clk := deps.Clock.(*fakeClock)
	deps.StartedAt = clk.Now().Add(-209 * time.Second)
	if code := runDispatch(context.Background(), deps); code != 0 {
		t.Fatalf("exit = %d, want 0; log:\n%s", code, logged.String())
	}
	if live := rt.live(t); len(live) != 1 || live[0] != runtime.Handle(testSession) {
		t.Fatalf("live = %v, want [%s]", live, testSession)
	}
}

// slowStore charges each WRITE after the first `free` ones a fixed duration on
// the fake clock -- a bead-store write taking its observed worst case -- and
// fails the writes listed in failAt (1-based, Put and PutIf counted together).
type slowStore struct {
	beadstore.Store
	clk    *fakeClock
	free   int
	cost   time.Duration
	failAt map[int]bool
	puts   int
}

func (s *slowStore) charge(ctx context.Context) error {
	s.puts++
	if s.puts > s.free {
		_ = s.clk.Sleep(ctx, s.cost)
	}
	if s.failAt[s.puts] {
		return fmt.Errorf("slowStore: write %d failed", s.puts)
	}
	return nil
}

func (s *slowStore) Put(ctx context.Context, r beadstore.Record) error {
	if err := s.charge(ctx); err != nil {
		return err
	}
	return s.Store.Put(ctx, r)
}

func (s *slowStore) PutIf(ctx context.Context, r beadstore.Record, ifRevision string) (string, error) {
	if err := s.charge(ctx); err != nil {
		return "", err
	}
	return s.Store.PutIf(ctx, r, ifRevision)
}

// THE WORST OBSERVED TIMELINE, end to end on a fake clock, with every phase
// charged its measured maximum from the 2026-09-13 controller log:
//
//   - pre-start 56s: MPKO, order run 202 at 15:49:13 -> POST /sessions 15:50:09
//   - a store write 44s: i77.a1's release write plus exit
//   - the teardown 19.3s: POST .../close 200 19.292437s at 15:48:30 (a Job
//     delete is one API call; the Gas City close is the slowest teardown gonk
//     has measured, so the budget is still sized on it)
//
// against a session whose running-record write FAILS, so dispatch must stop
// the session and release the reservation. It must be finished before
// hardStop -- the order timeout less the pre-exec and exit reserves -- with the
// stop and the release both run to completion.
func TestDispatchWorstObservedTimelineFinishesInsideTheOrderTimeout(t *testing.T) {
	const preStart, writeTook, stopTook = 56 * time.Second, 44 * time.Second, 19300 * time.Millisecond

	rt := newTestRuntime()
	var logged strings.Builder
	deps := startDeps(t, rt, runMeter(), &logged)
	clk := deps.Clock.(*fakeClock)
	started := clk.Now().Add(-preStart)
	deps.StartedAt = started
	// Write 1 is the reservation (part of the pre-start already charged);
	// write 2, the running record, takes the observed maximum and FAILS; write
	// 3, the release, takes the observed maximum too.
	store := &slowStore{Store: beadstore.NewMemory(), clk: clk, free: 1, cost: writeTook, failAt: map[int]bool{2: true}}
	deps.Store = store
	rt.onStop = func(runtime.Handle) { _ = clk.Sleep(context.Background(), stopTook) }

	if code := runDispatch(context.Background(), deps); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	b := orderBudget{}.withDefaults()
	took := clk.Now().Sub(started)
	if limit := b.Timeout - b.PreExec - b.Exit; took > limit {
		t.Fatalf("dispatch ran %v from process start, past the %v the %v order timeout leaves after "+
			"its pre-exec and exit reserves", took, limit, b.Timeout)
	}
	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("live = %v, want the session whose record failed stopped", live)
	}
	if store.puts != 3 {
		t.Fatalf("store writes = %d, want 3 (reservation, running record, release)", store.puts)
	}
	rec, ok, err := store.Get(context.Background(), baseDispatchArgs().BeadAnchor)
	if err != nil {
		t.Fatal(err)
	}
	if ok && rec.State == beadstore.StatePendingPrompt {
		t.Fatalf("release did not land: record still %+v", rec)
	}
	t.Logf("worst observed timeline: finished %v after process start (hard stop %v)", took, b.Timeout-b.PreExec-b.Exit)
}

// SIGTERM after the session was ADMITTED but before the running record is
// written: no write is started and the session is NOT stopped. The reservation
// already carries the session's name, so the sweep's reclaim promotes it and
// judges the session.
func TestDispatchTerminatedAfterStartWritesNothingAndStopsNothing(t *testing.T) {
	rt := newTestRuntime()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	rt.afterStart = func(runtime.Handle) { cancel(fmt.Errorf("%w: test SIGTERM", errTerminated)) }
	var logged strings.Builder
	deps := startDeps(t, rt, runMeter(), &logged)

	if code := runDispatch(ctx, deps); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("%d stops after termination, want the admitted session left running", n)
	}
	rec, ok, err := deps.Store.Get(context.Background(), baseDispatchArgs().BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("record gone (ok=%v err=%v)", ok, err)
	}
	if rec.State != beadstore.StatePendingPrompt || rec.SessionID != testSession {
		t.Fatalf("record = %+v, want the pending-prompt reservation naming %s, not a running record", rec, testSession)
	}
	if !strings.Contains(logged.String(), "NOT writing the running record") {
		t.Fatalf("termination not logged; log:\n%s", logged.String())
	}
}

// failingPutStore fails its nth WRITE (Put or PutIf, counted together), first
// firing a termination -- SIGTERM landing while the StateRunning write is in
// flight.
type failingPutStore struct {
	beadstore.Store
	mu     sync.Mutex
	n      int
	puts   int
	cancel context.CancelCauseFunc
}

func (s *failingPutStore) fail(ctx context.Context) error {
	s.mu.Lock()
	s.puts++
	hit := s.puts == s.n
	s.mu.Unlock()
	if hit {
		s.cancel(fmt.Errorf("%w: test SIGTERM", errTerminated))
		return fmt.Errorf("bd killed mid-write: %w", context.Cause(ctx))
	}
	return nil
}

func (s *failingPutStore) Put(ctx context.Context, r beadstore.Record) error {
	if err := s.fail(ctx); err != nil {
		return err
	}
	return s.Store.Put(ctx, r)
}

func (s *failingPutStore) PutIf(ctx context.Context, r beadstore.Record, ifRevision string) (string, error) {
	if err := s.fail(ctx); err != nil {
		return "", err
	}
	return s.Store.PutIf(ctx, r, ifRevision)
}

// SIGTERM while the running-record write is in flight: that write fails, and
// the follow-up teardown must not START a stop either (abandonSession's own
// guard -- the callers' guards do not cover this path).
func TestDispatchTerminatedDuringTheRunningWriteStartsNoStop(t *testing.T) {
	rt := newTestRuntime()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var logged strings.Builder
	deps := startDeps(t, rt, runMeter(), &logged)
	deps.Store = &failingPutStore{Store: beadstore.NewMemory(), n: 2, cancel: cancel}
	if code := runDispatch(ctx, deps); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("%d stops after termination during the running write, want none started", n)
	}
	if !strings.Contains(logged.String(), "NOT stopping the abandoned session") {
		t.Fatalf("termination not logged; log:\n%s", logged.String())
	}
}

// A running record that fails to land leaves a session no sweep will judge,
// so dispatch stops it -- and a session the runtime no longer knows is
// "already gone" at INFO, not a leak at ERROR (plan D9, gonk-wmb7 polarity).
func TestDispatchStopsTheSessionWhenItsRecordFailsAndCallsNotFoundAlreadyGone(t *testing.T) {
	rt := newTestRuntime()
	// The session is gone by the time dispatch stops it (a TTL, a human).
	rt.onStop = func(h runtime.Handle) { _ = rt.Runtime.Stop(context.Background(), h) }
	var logged strings.Builder
	deps := startDeps(t, rt, runMeter(), &logged)
	deps.Store = &slowStore{Store: beadstore.NewMemory(), clk: deps.Clock.(*fakeClock), free: 99,
		failAt: map[int]bool{2: true}}
	if code := runDispatch(context.Background(), deps); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if n := rt.stopCount(); n != 1 {
		t.Fatalf("stops = %d, want exactly 1 for the session whose record failed", n)
	}
	out := logged.String()
	if !strings.Contains(out, "abandoned session already gone") {
		t.Fatalf("a not-found stop was not logged as already gone; log:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "abandoned session") && strings.Contains(line, "level=ERROR") {
			t.Fatalf("a session that is already gone was reported at ERROR: %s", line)
		}
	}
}
