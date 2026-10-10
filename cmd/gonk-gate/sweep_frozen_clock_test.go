package main

import (
	"context"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// gonk-yryi. gatherSpend used to bound its poll ONLY by the injected clock:
//
//	deadline := d.Now().Add(d.SpendDeadline)
//	for { ...; if !d.Now().Before(deadline) { return 0, true }; sleep }
//
// A FROZEN clock -- the natural way for a test to pin an age or a reservation
// deadline exactly -- never reaches that deadline, so the loop never ends. It
// cost a gonk-7k73 verifier a 500s go-test timeout. On main this test HANGS;
// run it under a short -timeout to see that as a failure.
//
// The spend view here never catches up (AsOf is an hour before the session
// ended), so the only way out of the loop is the bound.
func TestGatherSpendEndsUnderAFrozenClock(t *testing.T) {
	frozen := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	rec := baseRunningRecord()
	fm := &fakeOutcomeMeter{
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 500, AsOf: rec.SessionEndedAt.Add(-time.Hour)},
	}
	d := sweepDeps{
		Meter:             meterClient(fm.server(t)),
		Now:               func() time.Time { return frozen },
		SpendPollInterval: time.Millisecond,
		SpendDeadline:     20 * time.Millisecond,
	}

	// No deadline on ctx, deliberately: production's sweep context has none
	// either, so ctx.Done is not what may end this loop.
	done := make(chan struct{})
	var tokens int64
	var stale bool
	go func() {
		defer close(done)
		tokens, stale = gatherSpend(context.Background(), d, rec)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// 20ms of budget at 1ms per poll is ~21 polls; ten seconds is three
		// orders of magnitude of slack. Reaching here is the bug.
		t.Fatal("gatherSpend did not return within 10s under a frozen clock: its bound is the clock alone (gonk-yryi)")
	}
	if !stale || tokens != 0 {
		t.Fatalf("gatherSpend = (%d, stale=%v), want (0, stale=true): spend never caught up, "+
			"so we could not PROVE the model answered", tokens, stale)
	}
}

// The count is never "unbounded", whatever the durations: a caller that builds
// sweepDeps by hand without withDefaults gets one poll, not a spin.
func TestSpendPollBudget(t *testing.T) {
	for _, c := range []struct {
		deadline, interval time.Duration
		want               int
	}{
		{60 * time.Second, time.Second, 61},
		{20 * time.Millisecond, time.Millisecond, 21},
		{20 * time.Millisecond, 0, 1},
		{0, time.Millisecond, 1},
		{-time.Second, time.Millisecond, 1},
	} {
		if got := spendPollBudget(c.deadline, c.interval); got != c.want {
			t.Errorf("spendPollBudget(%v, %v) = %d, want %d", c.deadline, c.interval, got, c.want)
		}
	}
}
