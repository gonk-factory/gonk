package intake

import (
	"context"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
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
		time.Sleep(2 * time.Millisecond)
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
	// A long tail: if the ladder does not stop early, these all run.
	r.StartupLadder = []time.Duration{0, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	waitFor(t, 5*time.Second, "the first pass to resolve the project", func() bool {
		synced, _ := syncedProjects(r)
		return synced > 0
	})
	// Let every remaining rung fire if it is going to.
	time.Sleep(50 * time.Millisecond)
	if n := passesCompleted(r); n != 1 {
		t.Fatalf("ran %d passes, want exactly 1: the ladder kept climbing after everything was synced", n)
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
	r.StartupLadder = []time.Duration{0, time.Millisecond, time.Millisecond, time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	waitFor(t, 5*time.Second, "the startup pass to run", func() bool { return passesCompleted(r) > 0 })
	time.Sleep(50 * time.Millisecond)
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

// THE RE-ARM ITSELF, and it needs its own test: the two above both start
// UNSETTLED, so the ticker is already created at the retry interval and they
// pass even if the loop never re-arms. This one starts SETTLED -- ticker at the
// slow cadence -- and then breaks meter, which is the real mid-life outage. Only
// a re-arm after the failing pass can speed the loop back up.
func TestLoopSpeedsUpAfterAHealthyProjectGoesUnsynced(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0} // one pass, succeeds -> SETTLED
	r.UnsettledRetryInterval = 2 * time.Millisecond
	// A resync must be due on every pass, or meter's outage is never observed:
	// reconcileProject short-circuits on an unchanged config hash and reuses the
	// cached answer. That short-circuit is CORRECT -- it is why a meter blip is
	// survivable at all -- but it also means the downgrade this test is about
	// only happens when a re-registration is actually attempted.
	r.MeterResyncInterval = time.Nanosecond
	const slow = 400 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, slow)

	waitFor(t, 5*time.Second, "the startup pass to settle", func() bool {
		synced, unsynced := syncedProjects(r)
		return synced > 0 && unsynced == 0
	})
	settledAt := passesCompleted(r)

	// Meter falls over. Force one pass so the loop observes it, exactly as the
	// next routine tick would.
	m.failNow.Store(true)
	r.Kick()
	waitFor(t, 5*time.Second, "the project to go unsynced", func() bool {
		_, unsynced := syncedProjects(r)
		return unsynced > 0
	})

	// If the loop re-armed, it is now running every 2ms and will pile up passes
	// well inside one slow tick. If it did not, it is still on the 400ms
	// cadence and almost nothing happens.
	time.Sleep(100 * time.Millisecond)
	if n := passesCompleted(r) - settledAt; n < 5 {
		t.Fatalf("only %d passes in 100ms after the project went unsynced; the loop did not "+
			"re-arm and is still waiting out the %v cadence", n, slow)
	}
}

// ...and once it settles, the cadence must go back to normal rather than
// staying fast forever. A permanently-short interval is a self-inflicted load
// generator against GitLab and meter.
func TestRetryIntervalRelaxesOnceSettled(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))

	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	r.StartupLadder = []time.Duration{0}
	r.UnsettledRetryInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, neverTicks)

	waitFor(t, 5*time.Second, "the startup pass", func() bool { return passesCompleted(r) >= 1 })
	time.Sleep(60 * time.Millisecond) // ~60 retry intervals, if it stayed fast
	if n := passesCompleted(r); n != 1 {
		t.Fatalf("ran %d passes, want 1: the interval stayed at the unsettled retry after settling", n)
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
