package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// reapStaleSessions (plan D7): the List-based backstop that replaced the Gas
// City reaper on the sweep path. It stops a session only when no record
// claims it AND the runtime says it is older than staleSessionAfter; every
// ambiguity leaves the session alone.

func staleSweep(t *testing.T, rt *testRuntime, store beadstore.Store, now time.Time) string {
	t.Helper()
	var log strings.Builder
	if code := runSweep(context.Background(), sweepDeps{
		Runtime: rt, Orders: &fakeOrders{}, Store: store, Log: testLogger(&log),
		Now: func() time.Time { return now },
	}); code != 0 {
		t.Fatalf("sweep exit = %d; log:\n%s", code, log.String())
	}
	return log.String()
}

func TestTheStaleSessionPassStopsOnlyOldUnclaimedSessions(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	rt := newTestRuntime()
	set := func(name string, st runtime.Status) runtime.Handle {
		h := rt.startNamed(t, name)
		rt.SetStatus(h, st)
		return h
	}
	oldFinished := set("gonk-triage-p1-i1-a1", runtime.Status{Phase: runtime.PhaseSucceeded,
		StartedAt: now.Add(-3 * staleSessionAfter), FinishedAt: now.Add(-staleSessionAfter - time.Minute)})
	oldRunning := set("gonk-triage-p1-i2-a1", runtime.Status{Phase: runtime.PhaseRunning,
		StartedAt: now.Add(-staleSessionAfter - time.Minute)})
	// Finished recently, though it STARTED long ago: its age is from the finish.
	recentFinish := set("gonk-triage-p1-i3-a1", runtime.Status{Phase: runtime.PhaseFailed,
		StartedAt: now.Add(-3 * staleSessionAfter), FinishedAt: now.Add(-staleSessionAfter + time.Minute)})
	young := set("gonk-triage-p1-i4-a1", runtime.Status{Phase: runtime.PhaseRunning, StartedAt: now.Add(-time.Minute)})
	noAge := set("gonk-triage-p1-i5-a1", runtime.Status{Phase: runtime.PhasePending})
	claimed := set("gonk-triage-p1-i6-a1", runtime.Status{Phase: runtime.PhaseRunning,
		StartedAt: now.Add(-3 * staleSessionAfter)})

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	rec.BeadAnchor, rec.SessionID = "gonk:1:issue:6", string(claimed)
	// A running record inside its reservation, so the sweep's own pass waits on
	// it rather than judging it.
	rec.ReservationExpiresAt = now.Add(time.Hour)
	rec.SessionEndedAt = time.Time{}
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	log := staleSweep(t, rt, store, now)

	live := map[runtime.Handle]bool{}
	for _, h := range rt.live(t) {
		live[h] = true
	}
	for _, h := range []runtime.Handle{oldFinished, oldRunning} {
		if live[h] {
			t.Errorf("%s is older than staleSessionAfter and unclaimed, and was left running", h)
		}
	}
	for _, h := range []runtime.Handle{recentFinish, young, noAge, claimed} {
		if !live[h] {
			t.Errorf("%s was stopped; it is young, of unknown age, or claimed", h)
		}
	}
	if !strings.Contains(log, "stopped a STALE session") {
		t.Errorf("a stale stop must be audible; log:\n%s", log)
	}
}

// FAIL CLOSED ON THE STORE: a failed read makes every session look
// unclaimed, so the pass stops nothing.
func TestTheStaleSessionPassStopsNothingWhenTheStoreCannotBeRead(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	rt := newTestRuntime()
	h := rt.startNamed(t, "gonk-triage-p1-i1-a1")
	rt.SetStatus(h, runtime.Status{Phase: runtime.PhaseRunning, StartedAt: now.Add(-3 * staleSessionAfter)})

	var log strings.Builder
	d := sweepDeps{Runtime: rt, Orders: &fakeOrders{}, Store: failingStore{}, Log: testLogger(&log),
		Now: func() time.Time { return now }}
	reapStaleSessions(context.Background(), d)
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("%d stops with an unreadable store", n)
	}
	if !strings.Contains(log.String(), "stopping NO stale sessions") {
		t.Fatalf("failing closed must be reported; log:\n%s", log.String())
	}
}
