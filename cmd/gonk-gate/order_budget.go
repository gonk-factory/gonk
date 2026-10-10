package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// THE DISPATCH MUST FINISH INSIDE ITS ORDER TIMEOUT (gonk-alw review, owner
// decision 2026-09-13: fit inside 300s, do not raise the timeout).
//
// gonk-dispatch is a Gas City exec order with `timeout = "300s"`
// (pack/orders/gonk-dispatch.toml). At the timeout Gas City does NOT wait for
// gonk-gate to tidy up. GASCITY_REF cmd/gc/order_dispatch.go shellExecRunner
// starts the command in its own process group and, on context expiry,
// processgroup.TerminateCommand sends SIGTERM to the WHOLE GROUP, waits
// shellExecSignalGrace (2s), then SIGKILLs it (internal/processgroup/
// processgroup_unix.go Terminate). Observed live 2026-09-13 15:51:15:
// "terminate process group 160054: process group 160054 did not exit within 2s".
//
// Two seconds cannot fit a bead-store write: BdCLI.Put is up to five sequential
// `bd` commands (pkg/beadstore/bd.go), and the `bd` children are in the same
// process group, so they receive the SIGTERM too. A Put cut off between its
// label removal and label add leaves a record NO sweep List ever matches again
// -- the permanent-stranding class of gonk-p7qh. So the only real protection is
// never to be inside a write when the timeout lands: every phase below gets an
// explicit, conservative slice of the 300s, measured from PROCESS START.
//
// UNDER THE JOBS RUNTIME (gonk-t55) there is no delivery wait: Start returns
// once the API server admitted the session, and the prompt is mounted, not
// fetched. What is left of this budget is the part that was always about the
// writes: a session is started only while its running-record write AND a Stop
// after a failed write still fit (startDeadline), and each write is bounded by
// the hard stop. gonk-dispatch stays a 300s Gas City order until T-57, so the
// arithmetic stays until then.
//
// THE RESERVES SHAPE THE START DEADLINE; THEY ARE NOT TIMEOUTS ON WRITES. A
// store write is cut only at the point past which what must follow it cannot
// fit (the hard stop, or the hard stop less a stop). The stop IS capped at
// its reserve. A cut stop tears no record -- it is one API call and gonk writes
// nothing around it -- but it is NOT free: the session keeps running (and
// spending) until its Job's activeDeadlineSeconds ends it.
//
// Signal handling (terminationContext) is the second line, not the first: on
// SIGTERM, dispatch stops and does NOT START any further store write or session
// stop. It leaves the pending-prompt reservation exactly as written, which is
// the state gonk-sweep's reclaimPendingPrompt exists to rescue. A write already
// in flight when the signal lands cannot be saved in 2s by anything gonk-gate
// does; the budget is what keeps that from happening.

const (
	// dispatchOrderTimeout MUST equal pack/orders/gonk-dispatch.toml's timeout;
	// TestDispatchOrderTimeoutMatchesThePack pins it to that file.
	dispatchOrderTimeout = 300 * time.Second

	// budgetPreExec covers the time Gas City's clock runs before gonk-gate's
	// own process start is captured. The clock starts AT order.fired, which
	// matches the controller's `POST .../gonk-dispatch/run 202` log line to
	// within tens of ms (GASCITY_REF dispatchOne: context.WithTimeout, then
	// orderExecEnvWithError under context.Background, then fork sh, which execs
	// a 65 MB binary on a node under heavy I/O pressure). NOT YET MEASURED:
	// main now logs the launch gap (logLaunchGap) so it can be, before deploy.
	budgetPreExec = 10 * time.Second
	// budgetExit covers process exit, log-sink flush and Gas City collecting the
	// result. The 2s SIGTERM grace comes AFTER the timeout and is not counted on.
	budgetExit = 5 * time.Second
	// budgetClose is the session stop (abandonSession). Sized on Gas City's
	// close, the slowest teardown gonk has measured; a Job delete is one
	// API-server call. Observed maximum
	// 19.3s: POST .../session/<alias>/close 200 19.292437s at 15:48:30 on
	// 2026-09-13 under node I/O pressure. gcapi's own HTTP timeout is 30s; the
	// close is cut at this budget instead.
	budgetClose = 25 * time.Second
	// budgetRelease is the room reserved for ONE bead-store write (the deferred
	// release, or the success path's StateRunning Put) when sizing the start
	// deadline. It is NOT the write's timeout -- see the note above. Observed maximum 44s: i77.a1 'abandoned
	// session closed' 15:51:10.765 to Gas City logging the order's exit
	// 15:51:54.649 -- the release Put plus exit, so it is an upper bound on the
	// Put alone.
	budgetRelease = 50 * time.Second
)

// orderBudget divides one dispatch's order timeout between its phases. The zero
// value of any field means its production default, so tests override only what
// they exercise.
type orderBudget struct {
	Timeout, PreExec, Exit, Close, Release time.Duration
}

func (b orderBudget) withDefaults() orderBudget {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&b.Timeout, dispatchOrderTimeout)
	def(&b.PreExec, budgetPreExec)
	def(&b.Exit, budgetExit)
	def(&b.Close, budgetClose)
	def(&b.Release, budgetRelease)
	return b
}

// hardStop is the instant, measured from process start, by which gonk-gate must
// have finished everything, with the pre-exec and exit reserves taken off.
func (b orderBudget) hardStop(start time.Time) time.Time {
	return start.Add(b.Timeout - b.PreExec - b.Exit)
}

// startDeadline is the last instant a session may be started so that the
// running-record write AND a Stop (if that write fails) can both still run to
// their full budgets before hardStop. With the defaults: 300 - 10 - 5 - 25 -
// 50 = 210s after process start. (Under Gas City this was the end of the
// prompt-delivery wait; the arithmetic is the same.)
func (b orderBudget) startDeadline(start time.Time) time.Time {
	return b.hardStop(start).Add(-b.Close - b.Release)
}

// phaseTimeout is how long a phase that starts now may run: its own budget,
// but never past notAfter. Never negative.
func phaseTimeout(now, notAfter time.Time, budget time.Duration) time.Duration {
	t := budget
	if left := notAfter.Sub(now); left < t {
		t = left
	}
	if t < 0 {
		return 0
	}
	return t
}

// timeLeft is how long until notAfter, never negative. Store writes are bounded
// by this alone -- see the release and running-record writes in
// runBrokerDispatch for why a per-phase reserve must not cut them.
func timeLeft(now, notAfter time.Time) time.Duration {
	if d := notAfter.Sub(now); d > 0 {
		return d
	}
	return 0
}

// errTerminated is the cancellation cause when gonk-gate is signalled -- in
// production, Gas City's order-timeout SIGTERM.
var errTerminated = errors.New("gonk-gate was signalled to stop")

// terminated reports whether ctx was cancelled by a termination signal.
func terminated(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errTerminated)
}

// terminationContext returns a context cancelled, with an errTerminated cause
// naming the signal, when SIGTERM or SIGINT arrives. main uses it for dispatch.
func terminationContext(parent context.Context) (context.Context, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	ctx, stop := terminationContextFrom(parent, sigs)
	return ctx, func() {
		signal.Stop(sigs)
		stop()
	}
}

// terminationContextFrom is terminationContext over any signal source, so the
// cancellation logic is testable without signalling the test process.
func terminationContextFrom(parent context.Context, sigs <-chan os.Signal) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-sigs:
			cancel(fmt.Errorf("%w: received %v", errTerminated, s))
		case <-done:
		}
	}()
	return ctx, func() {
		close(done)
		cancel(context.Canceled)
	}
}
