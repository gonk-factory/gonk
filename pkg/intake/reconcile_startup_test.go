package intake

import (
	"context"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
)

// THE STARTUP WINDOW (gonk-fan). Loop used to call initPass and then block on
// the first tick, so the first reconcile happened a FULL INTERVAL after boot --
// ten minutes in production. For those ten minutes intake has no resolved
// project state, so every webhook is answered 200 and dropped as
// state_unsynced. GitLab records a successful delivery and the issue is simply
// never triaged; the only evidence is a counter nobody is watching.
//
// It fires on every restart: deploy, node move, OOM, crashloop. It was observed
// four times in one day on 2026-08-03 and again during the v0.1.0-80f906b34717
// deploy, where intake came up before meter was listening and logged
// "meter registration failed ... connect: connection refused" for both projects.
//
// The fix is NOT to dispatch while unsynced -- unsynced means meter has not
// confirmed the project's config, and dispatching on unresolved config spends
// against a budget nobody validated. It is to make the window short.

// The interval every test here uses. It is deliberately absurd: if any of these
// pass by waiting for a tick rather than by reconciling at startup, they hang
// instead of quietly succeeding for the wrong reason.
const neverTicks = time.Hour

// passesCompleted reads the completed-pass counter under passMu. Tests assert
// on this rather than on the meter fake's slices: the fake is written from the
// httptest handler goroutine and read from the test goroutine with no lock, so
// counting its requests is a data race waiting to be reported.
func passesCompleted(r *Reconciler) uint64 {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	return r.completedSeq
}

func syncedProjects(r *Reconciler) (synced, unsynced int) {
	for state, n := range r.Cache.CountByState() {
		if state == StateUnsynced {
			unsynced += n
			continue
		}
		synced += n
	}
	return synced, unsynced
}

// waitFor polls until cond or the deadline. Used instead of a fixed sleep so a
// slow machine does not turn a real pass into a flake.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond) //nolint:forbidigo // poll interval of waitFor, a wait-until with a deadline; nothing counts events in a window
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

// The headline: a project resolves at startup, with the tick interval set so
// long that reaching it would mean the fix is absent.
func TestLoopReconcilesAtStartupInsteadOfWaitingForTheInterval(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0, time.Millisecond, 2 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	waitFor(t, 5*time.Second, "the project to resolve at startup", func() bool {
		synced, unsynced := syncedProjects(r)
		return synced > 0 && unsynced == 0
	})
}

// THE CASE THAT ACTUALLY BIT, twice. Intake comes up before meter is listening,
// so the first registration fails with connection refused. One startup pass is
// therefore NOT enough -- without a ladder the project stays unsynced until the
// next tick, which is the whole ten-minute window again.
func TestStartupLadderSurvivesMeterNotBeingUpYet(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	m.failFirst = 2 // the first two passes cannot reach meter at all

	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0, time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	waitFor(t, 5*time.Second, "the ladder to outlast meter's absence", func() bool {
		synced, unsynced := syncedProjects(r)
		return synced > 0 && unsynced == 0
	})
}

// The ladder must STOP once there is nothing unsynced left. A ladder that runs
// to its end regardless is a burst of pointless GitLab and meter traffic on
// every single boot -- and on a big instance that is a self-inflicted rate limit.
func TestStartupLadderStopsAsSoonAsEverythingIsSynced(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	// A long tail of HOUR-long rungs: if the ladder does not stop early it sits
	// in the first of them and never reaches the ticker. Reaching the ticker is
	// the observable "the ladder is over" -- Loop creates it only after
	// runStartupLadder returns -- so nothing here waits on wall-clock time
	// (gonk-oxb8: the old version slept 50ms and counted passes, which a busy
	// CPU turns into a test that passes whether or not the ladder stopped).
	r.StartupLadder = []time.Duration{0, time.Hour, time.Hour, time.Hour, time.Hour}
	ft := newFakeTicker()
	r.newTicker = ft.new

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	ft.nextArm(t, "the startup ladder (it kept climbing after everything was synced)")
	if n := passesCompleted(r); n != 1 {
		t.Fatalf("ran %d passes, want exactly 1: the ladder kept climbing after everything was synced", n)
	}
	if synced, unsynced := syncedProjects(r); synced == 0 || unsynced != 0 {
		t.Fatalf("the one pass did not resolve the project: synced=%d unsynced=%d", synced, unsynced)
	}
}

// A ladder that outlives its context is a goroutine that keeps working through
// shutdown, and in a test suite it is a leak that shows up as a mystery
// elsewhere. Cancelling must stop it between rungs, not only during a pass.
func TestStartupLadderStopsOnContextCancel(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	m.failFirst = 100 // never succeeds, so the ladder would run to its end

	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0, time.Hour, time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Loop(ctx, neverTicks); close(done) }()

	waitFor(t, 5*time.Second, "the first rung to run", func() bool { return passesCompleted(r) > 0 })
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return after cancel: the ladder is sleeping through shutdown")
	}
}

// Zero projects is a SETTLED state, not an unresolved one. A fresh instance with
// nothing onboarded must not climb the whole ladder on every boot.
func TestStartupLadderTreatsNoProjectsAsSettled(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}

	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	// Hour-long rungs and a fake ticker, as in the test above: reaching the
	// ticker proves the ladder returned, with no wall-clock wait (gonk-oxb8).
	r.StartupLadder = []time.Duration{0, time.Hour, time.Hour, time.Hour}
	ft := newFakeTicker()
	r.newTicker = ft.new

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	ft.nextArm(t, "the startup ladder (an empty instance climbed it)")
	if seq := passesCompleted(r); seq != 1 {
		t.Fatalf("completed %d passes, want 1: an empty instance climbed the ladder", seq)
	}
}

// THE NON-BOOT HALF OF gonk-fan, and the one with no restart to correlate
// against: a meter 5xx during a ROUTINE pass downgrades a healthy project to
// unsynced, and without a short retry nothing dispatches until the next tick.
// The interval here is an hour, so reaching it would mean a full outage.
func TestAPassThatLeavesProjectsUnsyncedRetriesSoonNotNextInterval(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	m.failFirst = 3 // outlast the startup ladder, so recovery must come from the retry

	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0, time.Millisecond} // two rungs, both fail
	r.UnsettledRetryInterval = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	waitFor(t, 5*time.Second, "the short retry to resolve the project", func() bool {
		synced, unsynced := syncedProjects(r)
		return synced > 0 && unsynced == 0
	})
}

// fakeTicker stands in for the loop's *time.Ticker so a test can read the
// loop's CADENCE directly instead of inferring it from how many passes fit in
// a slice of wall-clock time (gonk-oxb8). Every duration the loop arms it with
// -- at creation and on each post-pass Reset -- is sent on arms, and a tick is
// delivered only when the test calls fire. Nothing here depends on how busy the
// machine is: the only timeouts are deadlock guards, never budgets.
type fakeTicker struct {
	arms chan time.Duration
	c    chan time.Time
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{arms: make(chan time.Duration, 64), c: make(chan time.Time)}
}

func (f *fakeTicker) new(d time.Duration) loopTicker { f.arms <- d; return f }
func (f *fakeTicker) C() <-chan time.Time            { return f.c }
func (f *fakeTicker) Reset(d time.Duration)          { f.arms <- d }
func (f *fakeTicker) Stop()                          {}

// nextArm returns the next interval the loop armed the ticker with. Loop
// re-arms only AFTER runPass returns, so receiving an arm also means the pass
// that preceded it has completed and its cache writes are visible.
func (f *fakeTicker) nextArm(t *testing.T, after string) time.Duration {
	t.Helper()
	select {
	case d := <-f.arms:
		return d
	case <-time.After(5 * time.Second): // deadlock guard, not a budget
		t.Fatalf("the loop never (re-)armed its ticker after %s", after)
		return 0
	}
}

// fire delivers one tick. The channel is unbuffered, so fire returns only once
// the loop has actually taken the tick.
func (f *fakeTicker) fire(t *testing.T) {
	t.Helper()
	select {
	case f.c <- time.Now():
	case <-time.After(5 * time.Second): // deadlock guard, not a budget
		t.Fatal("the loop never took the tick: it is not selecting on its ticker")
	}
}

// THE RE-ARM ITSELF, and it needs its own test: the two above both start
// UNSETTLED, so the ticker is already created at the retry interval and they
// pass even if the loop never re-arms. This one starts SETTLED -- ticker at the
// slow cadence -- and then breaks meter, which is the real mid-life outage. Only
// a re-arm after the failing pass can speed the loop back up.
//
// WHAT IT ASSERTS, AND WHY NOT PASSES-PER-MILLISECOND (gonk-oxb8). The property
// is that the loop's cadence CHANGES: after the pass that leaves the project
// unsynced, the next pass is due after the retry interval, not the slow one --
// and a tick at that cadence really runs a pass. The old version counted passes
// in a 100ms real-time window ("at least 5"), which is a statement about the
// scheduler: under a loaded `make gate` with -race each pass is slower than 20ms
// and it went red with the re-arm working perfectly. Here the ticker is a fake,
// the interval it is armed with is read directly, and no assertion depends on
// wall-clock time.
func TestLoopSpeedsUpAfterAHealthyProjectGoesUnsynced(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0} // one pass, succeeds -> SETTLED
	const retry = 30 * time.Second
	r.UnsettledRetryInterval = retry
	// A resync must be due on every pass, or meter's outage is never observed:
	// reconcileProject short-circuits on an unchanged config hash and reuses the
	// cached answer. That short-circuit is CORRECT -- it is why a meter blip is
	// survivable at all -- but it also means the downgrade this test is about
	// only happens when a re-registration is actually attempted.
	r.MeterResyncInterval = time.Nanosecond
	const slow = neverTicks
	ft := newFakeTicker()
	r.newTicker = ft.new

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, slow)

	// The ticker is created after the startup ladder: settled, so slow cadence.
	if d := ft.nextArm(t, "the startup ladder"); d != slow {
		t.Fatalf("armed at %v after a settled startup, want the normal cadence %v", d, slow)
	}
	if synced, unsynced := syncedProjects(r); synced == 0 || unsynced != 0 {
		t.Fatalf("startup did not settle: synced=%d unsynced=%d", synced, unsynced)
	}

	// Meter falls over. Force one pass so the loop observes it, exactly as the
	// next routine tick would.
	m.failNow.Store(true)
	r.Kick()
	d := ft.nextArm(t, "the pass that found meter down")
	if _, unsynced := syncedProjects(r); unsynced == 0 {
		t.Fatal("the pass with meter down left nothing unsynced; the test premise is broken")
	}
	if d != retry {
		t.Fatalf("after the project went unsynced the loop re-armed at %v, want the retry "+
			"interval %v: it did not speed up and is still waiting out the %v cadence", d, retry, slow)
	}

	// And a tick at that cadence runs a pass, which -- still unsynced -- keeps
	// the loop fast rather than falling back to the slow cadence.
	before := passesCompleted(r)
	ft.fire(t)
	if d := ft.nextArm(t, "the retry tick"); d != retry {
		t.Fatalf("still unsynced, but the loop re-armed at %v after the retry tick, want %v", d, retry)
	}
	if n := passesCompleted(r) - before; n != 1 {
		t.Fatalf("the retry tick ran %d passes, want 1", n)
	}
}

// ...and once it settles, the cadence must go back to normal rather than
// staying fast forever. A permanently-short interval is a self-inflicted load
// generator against GitLab and meter.
//
// Starts UNSETTLED (meter refuses the one startup pass) so the loop really is
// on the fast cadence, then lets the retry tick succeed and reads the interval
// the loop re-arms with. The old version slept 60ms and counted passes, which
// could not tell "relaxed" from "the CPU was too busy to run a 1ms ticker"
// (gonk-oxb8): under load it went green whether or not the code relaxed.
func TestRetryIntervalRelaxesOnceSettled(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	m.failFirst = 1 // the one startup pass fails; the retry succeeds
	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0}
	const retry = 30 * time.Second
	r.UnsettledRetryInterval = retry
	ft := newFakeTicker()
	r.newTicker = ft.new

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	if d := ft.nextArm(t, "the startup ladder"); d != retry {
		t.Fatalf("armed at %v after an unsettled startup, want the retry interval %v", d, retry)
	}
	ft.fire(t)
	d := ft.nextArm(t, "the retry tick")
	if synced, unsynced := syncedProjects(r); synced == 0 || unsynced != 0 {
		t.Fatalf("the retry pass did not settle: synced=%d unsynced=%d", synced, unsynced)
	}
	if d != neverTicks {
		t.Fatalf("settled, but the loop re-armed at %v, want the normal cadence %v: "+
			"it stayed at the unsettled retry after settling", d, neverTicks)
	}
}

// The periodic cadence must survive the ladder. Shortening the startup window is
// not a licence to stop reconciling afterwards -- MeterResyncInterval and config
// drift both depend on the tick continuing.
func TestTickerStillRunsAfterTheStartupLadder(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, 20*time.Millisecond)

	waitFor(t, 5*time.Second, "at least three passes (startup + two ticks)", func() bool {
		return passesCompleted(r) >= 3
	})
}
