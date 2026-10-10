package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// Observed maxima from the reference cluster's controller log, 2026-09-13, worker-node I/O pressure
// avg60 67-98. These are MEASUREMENTS; the budget is checked against them.
const (
	// MPKO: order run 202 at 15:49:13 -> POST /v0/city/gonk/sessions 15:50:09.
	// Measured from the ORDER CLOCK'S START: Gas City starts the timeout AT
	// order.fired (dispatchOne's context.WithTimeout), and every order.fired in
	// events.jsonl matched its `POST .../gonk-dispatch/run 202` log line within
	// -36ms/+6ms (second review, 23 events). It therefore INCLUDES the launch gap
	// before process start; the checks below charge all of it after process
	// start, which overstates it by that gap.
	observedPreWaitMax = 56 * time.Second
	// POST .../session/<alias>/close 200 19.292437s at 15:48:30.
	observedCloseMax = 19292 * time.Millisecond
	// i77.a1: 'abandoned session closed' 15:51:10.765 -> Gas City logged the
	// order's exit at 15:51:54.649. Release write plus process exit.
	observedReleaseMax = 43884 * time.Millisecond
)

// The budget is sized against the order timeout Gas City actually enforces, so
// it must be that timeout, read from the order file, not a copy that can drift.
func TestDispatchOrderTimeoutMatchesThePack(t *testing.T) {
	var doc struct {
		Order struct {
			Timeout string `toml:"timeout"`
		} `toml:"order"`
	}
	path := filepath.Join(repoPackDir, "orders", "gonk-dispatch.toml")
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got, err := time.ParseDuration(doc.Order.Timeout)
	if err != nil {
		t.Fatalf("%s timeout = %q: %v", path, doc.Order.Timeout, err)
	}
	if got != dispatchOrderTimeout {
		t.Fatalf("%s declares timeout %v but dispatchOrderTimeout is %v: the dispatch budget "+
			"would be sized against a timeout Gas City does not enforce", path, got, dispatchOrderTimeout)
	}
}

// Every reserve must cover what was observed. Under the SessionRuntime there is
// no delivery wait (the prompt is mounted), so what follows the start is at
// most: the running-record write, and -- if it fails -- the stop and the
// release. A session started at the very last instant allowed (startDeadline)
// must still fit the slowest stop and the slowest write seen before hardStop.
func TestOrderBudgetCoversTheObservedMaxima(t *testing.T) {
	b := orderBudget{}.withDefaults()
	var start time.Time

	if b.Release < observedReleaseMax {
		t.Fatalf("release budget %v is under the observed %v", b.Release, observedReleaseMax)
	}
	if b.Close < observedCloseMax {
		t.Fatalf("stop budget %v is under the slowest teardown observed, %v", b.Close, observedCloseMax)
	}
	// Started ON the deadline: the stop and the release after it both fit.
	if end := b.startDeadline(start).Add(observedCloseMax + observedReleaseMax); end.After(b.hardStop(start)) {
		t.Fatalf("a session started at the start deadline, then stopped (%v) and released (%v), ends %v "+
			"after the hard stop", observedCloseMax, observedReleaseMax, end.Sub(b.hardStop(start)))
	}
	// And with the slowest observed pre-start there is still room to start.
	if left := b.startDeadline(start).Sub(start.Add(observedPreWaitMax)); left <= 0 {
		t.Fatalf("after the observed %v pre-start the start deadline has already passed", observedPreWaitMax)
	}
	// The whole thing ends before the order timeout, with the exit reserve.
	if end := b.hardStop(start).Add(b.Exit + b.PreExec); end.Sub(start) != b.Timeout {
		t.Fatalf("reserves do not sum to the timeout: %v", end.Sub(start))
	}
}

func TestPhaseTimeoutNeverRunsPastItsLimitOrGoesNegative(t *testing.T) {
	var now time.Time
	if got := phaseTimeout(now, now.Add(10*time.Second), 25*time.Second); got != 10*time.Second {
		t.Fatalf("got %v, want the 10s left", got)
	}
	if got := phaseTimeout(now, now.Add(time.Minute), 25*time.Second); got != 25*time.Second {
		t.Fatalf("got %v, want the 25s budget", got)
	}
	if got := phaseTimeout(now, now.Add(-time.Second), 25*time.Second); got != 0 {
		t.Fatalf("got %v, want 0 past the limit", got)
	}
}

func TestTerminationContextCancelsWithATerminationCauseOnSignal(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	ctx, stop := terminationContextFrom(context.Background(), sigs)
	defer stop()
	if terminated(ctx) || ctx.Err() != nil {
		t.Fatal("terminated before any signal")
	}
	sigs <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context not cancelled by SIGTERM")
	}
	if !terminated(ctx) {
		t.Fatalf("cause = %v, want errTerminated", context.Cause(ctx))
	}
}

// A normal stop is not a termination: code that skips writes on termination
// must not skip them just because dispatch finished.
func TestTerminationContextStopIsNotATermination(t *testing.T) {
	ctx, stop := terminationContextFrom(context.Background(), make(chan os.Signal))
	stop()
	<-ctx.Done()
	if terminated(ctx) {
		t.Fatal("stop() read as a termination")
	}
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
}

// The REAL signal path main uses: a SIGTERM delivered to this process by the
// kernel cancels the context (and, because it is caught, does not kill the test
// binary). Gas City's timeout gives 2s before SIGKILL, so it must land well
// inside that.
func TestTerminationContextCatchesARealSIGTERM(t *testing.T) {
	ctx, stop := terminationContext(context.Background())
	defer stop()
	sent := time.Now()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("real SIGTERM did not cancel the termination context within 1s")
	}
	if !terminated(ctx) {
		t.Fatalf("cause = %v, want errTerminated", context.Cause(ctx))
	}
	t.Logf("SIGTERM observed after %v", time.Since(sent))
}
