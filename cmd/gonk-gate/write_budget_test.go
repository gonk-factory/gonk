package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// REAL-CLOCK tests of how the order budget bounds the stop and the store
// writes AFTER the session start (gonk-alw, second review; ported to the
// SessionRuntime by gonk-t55).
//
// They exist because the fake-clock tests could not see the bug they missed:
// slowStore charges a write its duration but ignores ctx, so no test could
// observe a write being CUT. Production's BdCLI runs every bd command under
// exec.CommandContext (pkg/beadstore/bd.go), so a write's timeout firing kills
// the bd child mid-Put. ctxStore below honours ctx the same way: a cancelled
// write stops and writes nothing.
//
// TIMING IS IN SECONDS, NOT MILLISECONDS (final review): at a few hundred ms
// the scheduler, not the code, decided these tests under CPU load with -race --
// 8 failures in ~180 runs with 3x-nproc CPU burners, including setup eating
// the whole scaled budget. Every margin between a boundary and the event that
// tests it is now >= 2s, and every test first asserts that it REACHED the path
// it is about, so a starved run fails as "PRECONDITION NOT REACHED" rather than
// blaming the code. The trade: the five tests take ~40s of wall time between
// them; they run in parallel with each other.
//
// The tests that need a failed dispatch AFTER the session started fail its
// running-record write at once (ctxStore.failAt), which is what sends dispatch
// down the stop-then-release path.

// ctxStore is a Memory store whose nth WRITE -- Put or PutIf, counted together,
// since dispatch's reservation and release are conditional writes (gonk-y4m7) --
// takes `took` (forever when negative) and is cut -- abandoned without writing --
// if its context ends first.
type ctxStore struct {
	beadstore.Store
	n    int
	took time.Duration
	// failAt, when > 0, fails that write at once (before any slow write).
	failAt int

	mu        sync.Mutex
	puts      int
	started   bool
	startedAt time.Time
	cut       bool
	cutAt     time.Time
}

func (s *ctxStore) Put(ctx context.Context, r beadstore.Record) error {
	if err := s.write(ctx); err != nil {
		return err
	}
	return s.Store.Put(ctx, r)
}

func (s *ctxStore) PutIf(ctx context.Context, r beadstore.Record, ifRevision string) (string, error) {
	if err := s.write(ctx); err != nil {
		return "", err
	}
	return s.Store.PutIf(ctx, r, ifRevision)
}

// write charges one store write: slow (and cuttable) when it is the nth.
func (s *ctxStore) write(ctx context.Context) error {
	s.mu.Lock()
	s.puts++
	if s.failAt > 0 && s.puts == s.failAt {
		s.mu.Unlock()
		return errors.New("bd: the running-record write failed")
	}
	slow := s.n > 0 && s.puts == s.n
	if slow {
		s.started, s.startedAt = true, time.Now()
	}
	s.mu.Unlock()
	if slow {
		var elapse <-chan time.Time
		if s.took >= 0 {
			elapse = time.After(s.took)
		}
		select {
		case <-elapse:
		case <-ctx.Done():
			s.mu.Lock()
			s.cut, s.cutAt = true, time.Now()
			s.mu.Unlock()
			return errors.New("bd child killed mid-write: " + ctx.Err().Error())
		}
	}
	return nil
}

func (s *ctxStore) state() (started bool, startedAt time.Time, cut bool, cutAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started, s.startedAt, s.cut, s.cutAt
}

// syncBuilder is a strings.Builder safe to read while dispatch may still log.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type budgetShape struct {
	timeout, closeRes, releaseRes time.Duration
}

// realBudgetDeps wires a real-clock dispatch with a seconds-scale order budget.
// hardStop = start + timeout; start deadline = hardStop - close - release.
func realBudgetDeps(t *testing.T, rt *testRuntime, fm *fakeMeter, store beadstore.Store, logged *syncBuilder,
	b budgetShape) dispatchDeps {
	t.Helper()
	return dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Forge: promptTestForge(), Args: baseDispatchArgs(),
		Log:       testLogger(logged),
		StartedAt: time.Now(),
		Budget: orderBudget{
			Timeout: b.timeout + 2*time.Nanosecond, PreExec: time.Nanosecond, Exit: time.Nanosecond,
			Close: b.closeRes, Release: b.releaseRes,
		},
	}
}

// runBounded runs dispatch and fails -- rather than hangs -- if it has not
// returned within limit: an uncapped write or stop blocks until killed.
func runBounded(t *testing.T, deps dispatchDeps, logged *syncBuilder, limit time.Duration) (int, time.Duration) {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- runDispatch(context.Background(), deps) }()
	select {
	case code := <-done:
		return code, time.Since(deps.StartedAt)
	case <-time.After(limit):
		t.Fatalf("dispatch still running %v after process start: a close or store write is not "+
			"bounded by the order budget\nlog:\n%s", limit, logged.String())
		return 0, 0
	}
}

// requireReached fails the test as a PRECONDITION failure -- naming the step the
// run never got to -- rather than letting a starved run blame the code path.
func requireReached(t *testing.T, ok bool, step string, logged *syncBuilder) {
	t.Helper()
	if !ok {
		t.Fatalf("PRECONDITION NOT REACHED: %s -- the run did not get to the path under test, so this "+
			"says nothing about it (starved setup?)\nlog:\n%s", step, logged.String())
	}
}

// A running-record write slower than its write RESERVE, with plenty of the
// order left, must COMPLETE. Cut at the reserve, the bd child dies mid-Put and
// the error path stops the session that was just admitted.
//
// Boundary: write 5s vs reserve 2s (3s apart); the write's real cap is 18s
// after start, and the test requires it to start with >= 7s of that left.
func TestRunningWriteSlowerThanItsReserveIsNotCut(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime()
	store := &ctxStore{Store: beadstore.NewMemory(), n: 2, took: 5 * time.Second}
	var logged syncBuilder
	const timeout, closeRes = 20 * time.Second, 2 * time.Second
	deps := realBudgetDeps(t, rt, runMeter(), store, &logged, budgetShape{timeout: timeout, closeRes: closeRes, releaseRes: 2 * time.Second})

	code, _ := runBounded(t, deps, &logged, 40*time.Second)
	started, startedAt, cut, _ := store.state()
	requireReached(t, len(rt.Starts()) == 1, "the session was started", &logged)
	requireReached(t, started, "the running-record write started", &logged)
	requireReached(t, deps.StartedAt.Add(timeout-closeRes).Sub(startedAt) >= 7*time.Second,
		"the running-record write started with >= 7s before its cap", &logged)
	if cut {
		t.Fatalf("the running-record write was CUT with seconds of the order left; log:\n%s", logged.String())
	}
	if code != 0 || rt.stopCount() != 0 {
		t.Fatalf("exit = %d stops = %d, want 0 and the admitted session left running; log:\n%s", code, rt.stopCount(), logged.String())
	}
}

// ...but a running-record write that would run past the point where a stop
// can still fit (hard stop less the stop reserve) IS cut there, and the stop
// runs. Boundary: cut expected at 10s-3s = 7s; the stop then has 3s.
func TestRunningWriteIsCutWhereTheStopMustStillFit(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime()
	store := &ctxStore{Store: beadstore.NewMemory(), n: 2, took: -1}
	var logged syncBuilder
	const timeout, closeRes = 10 * time.Second, 3 * time.Second
	deps := realBudgetDeps(t, rt, runMeter(), store, &logged, budgetShape{timeout: timeout, closeRes: closeRes, releaseRes: 2 * time.Second})

	code, took := runBounded(t, deps, &logged, timeout+5*time.Second)
	started, startedAt, cut, cutAt := store.state()
	requireReached(t, len(rt.Starts()) == 1, "the session was started", &logged)
	requireReached(t, started, "the running-record write started", &logged)
	requireReached(t, startedAt.Sub(deps.StartedAt) <= timeout-closeRes-2*time.Second,
		"the running-record write started >= 2s before its cap", &logged)
	if !cut || code != 1 {
		t.Fatalf("cut=%v exit=%d, want the write cut and exit 1; log:\n%s", cut, code, logged.String())
	}
	if cutAfter := cutAt.Sub(deps.StartedAt); cutAfter < timeout-closeRes-time.Second || cutAfter > timeout-closeRes+2*time.Second {
		t.Fatalf("write cut %v after start, want at hard stop less the close reserve (%v)", cutAfter, timeout-closeRes)
	}
	if rt.stopCount() != 1 || len(rt.live(t)) != 0 {
		t.Fatalf("stops=%d live=%v, want the orphaned session stopped after the cut write; log:\n%s",
			rt.stopCount(), rt.live(t), logged.String())
	}
	if took > timeout+2*time.Second {
		t.Fatalf("dispatch took %v, past the %v hard stop", took, timeout)
	}
}

// A release write slower than its reserve, with time left before the hard stop,
// must complete: cutting it tears the record for no reason.
// Boundary: write 5s vs reserve 2s; the test requires the write to start with
// >= 7s before the 24s hard stop.
func TestReleaseWriteSlowerThanItsReserveIsNotCut(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime()
	// Write 2 (the running record) fails at once; write 3 (the release) is slow.
	store := &ctxStore{Store: beadstore.NewMemory(), failAt: 2, n: 3, took: 5 * time.Second}
	var logged syncBuilder
	const timeout = 24 * time.Second
	deps := realBudgetDeps(t, rt, runMeter(), store, &logged, budgetShape{timeout: timeout, closeRes: 4 * time.Second, releaseRes: 2 * time.Second})

	code, _ := runBounded(t, deps, &logged, 45*time.Second)
	started, startedAt, cut, _ := store.state()
	requireReached(t, len(rt.Starts()) == 1, "the session was started", &logged)
	requireReached(t, rt.stopCount() == 1, "the abandoned session's stop was invoked", &logged)
	requireReached(t, started, "the release write started", &logged)
	requireReached(t, deps.StartedAt.Add(timeout).Sub(startedAt) >= 7*time.Second,
		"the release write started with >= 7s before the hard stop", &logged)
	if cut {
		t.Fatalf("the release write was CUT with time left before the hard stop; log:\n%s", logged.String())
	}
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	rec, ok, _ := store.Get(context.Background(), baseDispatchArgs().BeadAnchor)
	if ok && rec.State == beadstore.StatePendingPrompt {
		t.Fatalf("release did not land: %+v", rec)
	}
}

// ...but a release write that would run past the hard stop IS cut there, so the
// process is out before Gas City's SIGTERM/SIGKILL. Boundary: hard stop 12s.
func TestReleaseWriteIsCutAtTheHardStop(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime()
	store := &ctxStore{Store: beadstore.NewMemory(), failAt: 2, n: 3, took: -1}
	var logged syncBuilder
	const timeout = 12 * time.Second
	deps := realBudgetDeps(t, rt, runMeter(), store, &logged, budgetShape{timeout: timeout, closeRes: 2 * time.Second, releaseRes: 2 * time.Second})

	code, took := runBounded(t, deps, &logged, timeout+5*time.Second)
	started, startedAt, cut, cutAt := store.state()
	requireReached(t, len(rt.Starts()) == 1, "the session was started", &logged)
	requireReached(t, started, "the release write started", &logged)
	requireReached(t, startedAt.Sub(deps.StartedAt) <= timeout-2*time.Second,
		"the release write started >= 2s before the hard stop", &logged)
	if !cut || code != 1 {
		t.Fatalf("cut=%v exit=%d, want the release cut at the hard stop and exit 1; log:\n%s", cut, code, logged.String())
	}
	if cutAfter := cutAt.Sub(deps.StartedAt); cutAfter < timeout-time.Second {
		t.Fatalf("release cut %v after start, before the %v hard stop", cutAfter, timeout)
	}
	if took > timeout+2*time.Second {
		t.Fatalf("dispatch took %v, past the %v hard stop", took, timeout)
	}
}

// A stop that never answers is cut at ITS reserve, so the release after it
// still lands inside the hard stop. Boundary: stop reserve 3s; unbounded, the
// hung stop would block until the test's own limit.
func TestHungStopIsCutSoTheReleaseStillLands(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime()
	rt.stopHangs = true
	store := &ctxStore{Store: beadstore.NewMemory(), failAt: 2} // the running record fails; no slow put
	var logged syncBuilder
	const timeout = 14 * time.Second
	deps := realBudgetDeps(t, rt, runMeter(), store, &logged, budgetShape{timeout: timeout, closeRes: 3 * time.Second, releaseRes: 3 * time.Second})

	code, took := runBounded(t, deps, &logged, timeout+5*time.Second)
	requireReached(t, len(rt.Starts()) == 1, "the session was started", &logged)
	requireReached(t, rt.stopCount() == 1, "the stop under test was invoked", &logged)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if took > timeout+2*time.Second {
		t.Fatalf("dispatch took %v with a hung close, past the %v hard stop", took, timeout)
	}
	rec, ok, _ := store.Get(context.Background(), baseDispatchArgs().BeadAnchor)
	if ok && rec.State == beadstore.StatePendingPrompt {
		t.Fatalf("release did not land after the hung close: %+v", rec)
	}
	if !strings.Contains(logged.String(), "could not stop the abandoned session") {
		t.Fatalf("the cut stop was not reported; log:\n%s", logged.String())
	}
}
