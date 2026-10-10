package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// gonk-p8w8, the re-sling window, ENFORCED.
//
// History, because the tests below exist to stop each step being undone:
//   - d0a1e89 measured the window from ReslungAt, stamped before the re-sling's
//     store write and order POST; gonk-alw's second review showed the fired
//     dispatch could outlive it (~66s).
//   - The fix recorded a fire time after the POST and assumed gc finishes an
//     in-flight POST within 30s. The independent review of that proved both
//     false at the pinned Gas City: the order-run handler is detached from the
//     client and writes a tracking bead before firing, with no server-side
//     bound, and gcapi retries a POST that timed out client-side, so one
//     re-sling can fire LATE or TWICE (gonk-fjqq).
//   - Now: the re-sling passes a `fire_by` deadline; a dispatch starting after
//     it writes nothing (refuseLateFire); the window ends a kill after it.
//     Nothing depends on when gc fires.

// fakeSweepClock is the sweep's clock, advanced by the slow operations below
// rather than by sleeping.
type fakeSweepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeSweepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeSweepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeSweepClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// slowPutIfStore advances the clock by delay inside every PutIf, as a BdCLI
// write does under Dolt pressure, and runs beforePutIf (if set) first.
type slowPutIfStore struct {
	beadstore.Store
	clock       *fakeSweepClock
	delay       time.Duration
	beforePutIf func(n int)
	n           int
}

func (s *slowPutIfStore) PutIf(ctx context.Context, r beadstore.Record, ifRevision string) (string, error) {
	s.n++
	if s.beforePutIf != nil {
		s.beforePutIf(s.n)
	}
	s.clock.Advance(s.delay)
	return s.Store.PutIf(ctx, r, ifRevision)
}

// slowOrders advances the clock by delay while an order-run POST is in
// flight: gc answers the 202 -- and starts the dispatch's clock -- at the END.
type slowOrders struct {
	*fakeOrders
	clock *fakeSweepClock
	delay time.Duration
}

func (o slowOrders) RunOrder(ctx context.Context, name string, vars map[string]string) error {
	defer o.clock.Advance(o.delay)
	return o.fakeOrders.RunOrder(ctx, name, vars)
}

const (
	observedPutIfLatency    = 44 * time.Second
	observedOrderRunLatency = 22 * time.Second
)

// slowReslingSweepAt is slowReslingSweep with the clock set to stamp and the
// write/POST latencies chosen by the test.
func slowReslingSweepAt(t *testing.T, stamp time.Time, putIf, post time.Duration) (sweepDeps, *slowPutIfStore, *fakeSweepClock, func() []string) {
	t.Helper()
	_, orders, gl, mem := staleSweepFixture(t)
	// The fixture's reservation expired two minutes before the REAL now; these
	// tests run on a fixed fake clock, so expire it relative to that instead.
	ctx := context.Background()
	rec, _, _ := mem.Get(ctx, "gonk:42:issue:3")
	rec.ReservationExpiresAt = stamp.Add(-2 * time.Minute)
	if err := mem.Put(ctx, rec); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	clock := &fakeSweepClock{now: stamp}
	store := &slowPutIfStore{Store: mem, clock: clock, delay: putIf}
	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	deps := sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: newTestRuntime(),
		Orders: slowOrders{fakeOrders: orders, clock: clock, delay: post}, GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		Now: clock.Now,
	}
	fireBys := func() []string {
		var out []string
		for _, p := range orders.orders() {
			out = append(out, p.Vars[fireByVar])
		}
		return out
	}
	return deps, store, clock, fireBys
}

// THE BOUNDARY, EXACTLY. Every term of the window is pinned by a literal here,
// so changing any of them -- the 240s start allowance, the 300s dispatch
// timeout, gc's 2s SIGTERM-to-SIGKILL grace, the 5s clock margin -- fails this
// test rather than sliding inside a range two probe points happen to straddle
// (the review of the previous cut found a fallback of 500s passed a test that
// only looked at +420s and +631s).
func TestSweepReslingWindowEndsExactlyAKillAfterTheFireByDeadline(t *testing.T) {
	const (
		startAllowance = 240 * time.Second
		dispatchTO     = 300 * time.Second
		killGrace      = 2 * time.Second
		clockMargin    = 5 * time.Second
	)
	stamp := time.Date(2026, 9, 13, 15, 50, 11, 659139223, time.UTC)
	deps, store, clock, fireBys := slowReslingSweepAt(t, stamp, observedPutIfLatency, observedOrderRunLatency)
	ctx := context.Background()

	if code := runSweep(ctx, deps); code != 0 {
		t.Fatalf("first sweep exit = %d", code)
	}
	wantFireBy := stamp.Add(startAllowance)
	got, _, _ := store.Get(ctx, "gonk:42:issue:3")
	if !got.ReslungAt.Equal(stamp) || !got.ReslingFireBy.Equal(wantFireBy) {
		t.Fatalf("record ReslungAt %v ReslingFireBy %v, want %v and %v", got.ReslungAt, got.ReslingFireBy, stamp, wantFireBy)
	}
	if f := fireBys(); len(f) != 1 || f[0] != wantFireBy.Format(time.RFC3339Nano) {
		t.Fatalf("fired with fire_by %v, want exactly [%s] -- the dispatch must be told the deadline the window assumes",
			f, wantFireBy.Format(time.RFC3339Nano))
	}

	end := wantFireBy.Add(dispatchTO + killGrace + clockMargin)
	clock.Set(end.Add(-time.Nanosecond))
	if code := runSweep(ctx, deps); code != 0 {
		t.Fatalf("sweep exit = %d", code)
	}
	if n := len(fireBys()); n != 1 {
		t.Fatalf("re-judged 1ns before %v (fire_by + %v): %d fires, want 1", end, dispatchTO+killGrace+clockMargin, n)
	}
	clock.Set(end)
	if code := runSweep(ctx, deps); code != 0 {
		t.Fatalf("sweep exit = %d", code)
	}
	if n := len(fireBys()); n != 2 {
		t.Fatalf("not re-judged at %v: %d fires, want 2 -- a failed re-sling must be retried when its window ends", end, n)
	}
}

// THE INVARIANT THE WINDOW RESTS ON, stated against the real check: the last
// instant a dispatch is allowed to start is the deadline itself, and a dispatch
// whose timeout started then is killed -- SIGTERM at +300s, SIGKILL 2s later --
// strictly before the sweep may judge the record again.
func TestADispatchAllowedToStartIsDeadBeforeTheSweepMayRejudge(t *testing.T) {
	stamp := time.Date(2026, 9, 13, 15, 50, 11, 0, time.UTC)
	rec := beadstore.Record{ReslungAt: stamp, ReslingFireBy: reslingFireBy(stamp)}
	end, ok := reslingWindowEnd(rec)
	if !ok {
		t.Fatal("no window for a re-slung record")
	}
	deps := func(now time.Time) dispatchDeps {
		a := baseDispatchArgs()
		a.FireBy = rec.ReslingFireBy.Format(time.RFC3339Nano)
		var log strings.Builder
		return dispatchDeps{Args: a, Clock: &fakeClock{now: now}, Log: testLogger(&log)}
	}
	if _, refused := refuseLateFire(deps(rec.ReslingFireBy)); refused {
		t.Fatal("a dispatch starting exactly at fire_by was refused; the deadline is inclusive")
	}
	if _, refused := refuseLateFire(deps(rec.ReslingFireBy.Add(time.Nanosecond))); !refused {
		t.Fatal("a dispatch starting 1ns after fire_by was allowed to run")
	}
	if dead := rec.ReslingFireBy.Add(dispatchOrderTimeout + gcKillGrace); !dead.Before(end) {
		t.Fatalf("the last dispatch allowed to start is killed by %v, but the sweep may re-judge from %v", dead, end)
	}
}

// B1 AND B2, END TO END. The re-sling's order fires late -- gc was still
// writing its tracking bead when the sweep's POST gave up, or it is the first
// of gcapi's retried POSTs landing after the second was answered -- so the
// dispatch process starts after fire_by. It must do NOTHING: no /decide, no
// store read or write, no session. The same order on time must still run.
func TestAReslingDispatchThatStartsAfterItsDeadlineDoesNothing(t *testing.T) {
	ctx := context.Background()
	stamp := time.Date(2026, 9, 13, 15, 50, 11, 0, time.UTC)
	deps, store, _, fireBys := slowReslingSweepAt(t, stamp, observedPutIfLatency, observedOrderRunLatency)
	if code := runSweep(ctx, deps); code != 0 {
		t.Fatalf("sweep exit = %d", code)
	}
	fireBy := fireBys()[0]
	before, _, _ := store.Get(ctx, "gonk:42:issue:3")
	deadline, _ := time.Parse(time.RFC3339Nano, fireBy)

	run := func(startedAt time.Time) (int, *fakeMeter, *testRuntime, string) {
		rt := newTestRuntime()
		fm := attemptMeter(2)
		var log syncBuilder
		d := liveDispatchDeps(t, rt, fm, store.Store, &log)
		d.Args.FireBy = fireBy
		d.Clock = &fakeClock{now: startedAt}
		return runDispatch(ctx, d), fm, rt, log.String()
	}

	code, fm, rt, log := run(deadline.Add(time.Second))
	if code != 0 {
		t.Fatalf("late dispatch exit = %d, want 0 (refusing is the designed outcome); log:\n%s", code, log)
	}
	fm.mu.Lock()
	decides := fm.calls
	fm.mu.Unlock()
	if decides != 0 || len(rt.Starts()) != 0 {
		t.Fatalf("a dispatch that started after fire_by called /decide %d time(s) and started %d session(s); log:\n%s",
			decides, len(rt.Starts()), log)
	}
	if after, _, _ := store.Get(ctx, "gonk:42:issue:3"); after.Revision != before.Revision {
		t.Fatalf("a dispatch that started after fire_by wrote the record (revision %s -> %s)", before.Revision, after.Revision)
	}

	code, _, rt, log = run(deadline)
	if code != 0 || len(rt.Starts()) != 1 {
		t.Fatalf("an on-time dispatch: exit %d, %d sessions, want 0 and 1; log:\n%s", code, len(rt.Starts()), log)
	}
}

// B2's OTHER HALF: gcapi's retry makes gc fire the SAME re-sling twice, both
// before fire_by. The deadline does not separate them; gonk-y4m7's
// compare-and-swap reservation does. Both dispatches read the record before
// either reserves; exactly one session must result.
func TestAReslingFiredTwiceOnTimeCreatesOneSession(t *testing.T) {
	ctx := context.Background()
	stamp := time.Now().UTC().Truncate(time.Second)
	deps, slow, _, fireBys := slowReslingSweepAt(t, stamp, 0, 0)
	if code := runSweep(ctx, deps); code != 0 {
		t.Fatalf("sweep exit = %d", code)
	}
	mem := slow.Store.(*beadstore.Memory)
	store := &hookStore{Memory: mem}
	rt := newTestRuntime()
	fm := attemptMeter(2)
	onTime := func(log *syncBuilder) dispatchDeps {
		d := liveDispatchDeps(t, rt, fm, store, log)
		d.Args.FireBy = fireBys()[0]
		d.Clock = &fakeClock{now: stamp.Add(time.Second)}
		return d
	}
	var aLog, bLog syncBuilder
	store.afterGet = func(n int) {
		// Get #1 is runDispatch's read before /decide (gonk-b2jk); #2 is the
		// reservation's read, which is the one this test acts after.
		if n != 2 {
			return
		}
		// The second fire runs to completion between the first's read and its
		// reservation.
		if code := runDispatch(ctx, onTime(&bLog)); code != 0 {
			t.Errorf("second fire exit = %d; log:\n%s", code, bLog.String())
		}
	}
	if code := runDispatch(ctx, onTime(&aLog)); code != 0 {
		t.Fatalf("first fire exit = %d, want 0 (a duplicate is a no-op); log:\n%s", code, aLog.String())
	}
	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("one re-sling fired twice on time called Start %d times, want 1; logs:\n%s\n---\n%s",
			n, aLog.String(), bLog.String())
	}
}

// gonk-aavs. A re-sling whose store write FAILED must not fire. Two failures:
// the write did not land (the record still reads as unjudged, so the next tick
// judges it again -- which must then fire, once), and the write landed but
// reported an error (the record carries its stamp, so it waits out the window).
func TestSweepDoesNotFireAReslingWhoseWriteFailed(t *testing.T) {
	for _, landed := range []bool{false, true} {
		t.Run(fmt.Sprintf("write-landed=%v", landed), func(t *testing.T) {
			ctx := context.Background()
			stamp := time.Date(2026, 9, 13, 15, 50, 11, 0, time.UTC)
			deps, slow, clock, fireBys := slowReslingSweepAt(t, stamp, 0, 0)
			mem := slow.Store
			failing := &failFirstPutIf{Store: mem, land: landed}
			deps.Store = failing
			if code := runSweep(ctx, deps); code != 0 {
				t.Fatalf("sweep exit = %d", code)
			}
			if n := len(fireBys()); n != 0 {
				t.Fatalf("fired %d dispatch(es) after the re-sling write failed, want 0: if the write did not "+
					"land, the next tick re-judges while that dispatch lives", n)
			}
			if !landed {
				if code := runSweep(ctx, deps); code != 0 {
					t.Fatalf("second sweep exit = %d", code)
				}
				if n := len(fireBys()); n != 1 {
					t.Fatalf("the next tick fired %d, want 1: a failed write that did not land is retried at once", n)
				}
				return
			}
			rec, _, _ := mem.Get(ctx, "gonk:42:issue:3")
			end, _ := reslingWindowEnd(rec)
			clock.Set(end.Add(-time.Second))
			if code := runSweep(ctx, deps); code != 0 {
				t.Fatalf("sweep exit = %d", code)
			}
			if n := len(fireBys()); n != 0 {
				t.Fatalf("a landed-but-errored re-sling was re-judged inside its window: %d fires", n)
			}
			clock.Set(end)
			if code := runSweep(ctx, deps); code != 0 {
				t.Fatalf("sweep exit = %d", code)
			}
			if n := len(fireBys()); n != 1 {
				t.Fatalf("at the window's end the re-sling was not retried: %d fires, want 1", n)
			}
		})
	}
}

// failFirstPutIf fails its first PutIf; land says whether that write happened
// anyway (BdCLI can append its comment and then fail a later bd call).
type failFirstPutIf struct {
	beadstore.Store
	land bool
	done bool
}

func (s *failFirstPutIf) PutIf(ctx context.Context, r beadstore.Record, rev string) (string, error) {
	if s.done {
		return s.Store.PutIf(ctx, r, rev)
	}
	s.done = true
	if s.land {
		if _, err := s.Store.PutIf(ctx, r, rev); err != nil {
			return "", err
		}
	}
	return "", errors.New("bd comments go-3tbq --json: Error 1105: context deadline exceeded")
}

// A re-slung record put back by a failed dispatch carries the OLD deadline. Its
// next re-sling must pass a NEW one, measured from its own stamp: an old
// deadline would refuse the new dispatch at start (so nothing ever retries until
// the next window) or, worse, pair a fresh fire with a window that has already
// ended.
func TestAReslingOfARestoredRecordPassesAFreshDeadline(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 17, 0, 0, 0, time.UTC)
	deps, slow, _, fireBys := slowReslingSweepAt(t, now, 0, 0)
	mem := slow.Store
	cur, _, _ := mem.Get(ctx, "gonk:42:issue:3")
	old := now.Add(-time.Hour)
	cur.ReslungAt, cur.ReslingFireBy = old, reslingFireBy(old)
	if err := mem.Put(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if code := runSweep(ctx, deps); code != 0 {
		t.Fatalf("sweep exit = %d", code)
	}
	want := reslingFireBy(now)
	got, _, _ := mem.Get(ctx, "gonk:42:issue:3")
	if !got.ReslingFireBy.Equal(want) {
		t.Fatalf("ReslingFireBy = %v after re-slinging a restored record, want the fresh %v", got.ReslingFireBy, want)
	}
	if f := fireBys(); len(f) != 1 || f[0] != want.Format(time.RFC3339Nano) {
		t.Fatalf("fired with %v, want [%s]", f, want.Format(time.RFC3339Nano))
	}
}

// The var must be declared where gc reads the order's params, under the name
// the sweep sends and the dispatch reads.
func TestGonkDispatchDeclaresTheFireByParam(t *testing.T) {
	var doc struct {
		Order struct {
			Params map[string]struct {
				Required bool `toml:"required"`
			} `toml:"params"`
		} `toml:"order"`
	}
	path := filepath.Join(repoPackDir, "orders", "gonk-dispatch.toml")
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	p, ok := doc.Order.Params[fireByVar]
	if !ok {
		t.Fatalf("%s does not declare %q", path, fireByVar)
	}
	if p.Required {
		t.Fatalf("%s declares %q required; intake and unpark dispatches do not send it", path, fireByVar)
	}
}
