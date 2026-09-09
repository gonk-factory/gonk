package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
)

// THE ORPHAN REAPER (gonk-4d7). gonk-xkm closed the three teardown paths gonk
// controls directly. This covers the one none of them can: a dispatch process
// that DIES between creating the session and reaching any close call -- pod
// evicted, order timeout, controller restarted mid-dispatch. Such a session has
// no bead in StateRunning, so gonk-sweep will never look at it. It is invisible,
// not merely long-lived.
//
// EVERY TEST BELOW IS ABOUT WHAT THE REAPER MUST NOT KILL. That asymmetry is
// deliberate: a leaked pod costs money and eventually wedges the scheduler, but
// a reaper that is too eager kills a live agent mid-turn, destroying work that
// was in progress and burning a ladder attempt for nothing. The leak is the
// cheaper failure, so every ambiguity here resolves toward leaving things alone.

const reapNow = "2026-08-03T12:00:00Z"

// testNonce is a syntactically valid nonce segment -- 26 characters from
// brokerSessionAlias's base32 alphabet, matching the length its 128-bit
// crypto/rand suffix always produces -- for aliases this file hand-builds
// rather than minting through brokerSessionAlias itself.
const testNonce = "AAAAAAAAAAAAAAAAAAAAAAAAAA"

func reapClock(t *testing.T) func() time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, reapNow)
	if err != nil {
		t.Fatalf("bad test clock: %v", err)
	}
	return func() time.Time { return now }
}

func reapDeps(t *testing.T, gc *gcapitest.Server, store beadstore.Store, log *strings.Builder) sweepDeps {
	t.Helper()
	d := sweepDeps{
		GC: gc.Client("gonk-city"), Store: store, Now: reapClock(t),
	}
	if log != nil {
		d.Log = testLogger(log)
	}
	return d
}

// The case the reaper exists for: a session gonk created, with nothing in the
// bead store claiming it, old enough to be past the grace window.
func TestReaperClosesAnOrphanedSession(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 3, 1)
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("orphan survived the reaper: %v", live)
	}
	if len(gc.Closed) != 1 || gc.Closed[0] != alias {
		t.Fatalf("CloseSession was not called for the orphan: Closed = %v, want [%s]", gc.Closed, alias)
	}
}

// THE LOAD-BEARING ASSERTION. A session whose bead is in StateRunning is a live
// agent doing work. Reaping it destroys the batch it has not written yet and
// burns the attempt. This must never happen, and it is worth more than every
// leak the reaper prevents.
func TestReaperNeverTouchesASessionWithARunningBead(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 3, 1)
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(42)
	rec.SessionID = alias
	rec.State = beadstore.StateRunning
	_ = store.Put(context.Background(), rec)

	runReap(context.Background(), reapDeps(t, gc, store, nil))

	if live := gc.LiveSessions(); len(live) != 1 || live[0] != alias {
		t.Fatalf("THE REAPER KILLED A LIVE AGENT. live = %v, want [%s]", live, alias)
	}
	if len(gc.Closed) != 0 {
		t.Fatalf("CloseSession was called for a session a running bead claims: Closed = %v", gc.Closed)
	}
}

// The grace window. A dispatch creates the session and only writes its record
// after prompt delivery, which is bounded by defaultSubmitDeadline (240s). For
// that entire window a perfectly healthy session has no record -- and looks
// exactly like an orphan. The window must comfortably exceed that deadline.
func TestReaperLeavesYoungSessionsAloneEvenWithNoRecord(t *testing.T) {
	gc := gcapitest.New(t)
	// Created one minute ago: mid-dispatch, no record written yet.
	gc.CreateSessionAt("gonk.triage.p42.i9.a1."+testNonce, gcapitest.SessionRunning, "2026-08-03T11:59:00Z")

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 1 {
		t.Fatal("the reaper killed a session that a dispatch was still setting up: " +
			"the create precedes the record by the whole prompt-delivery window")
	}
}

func TestReaperGraceWindowComfortablyExceedsTheDeliveryDeadline(t *testing.T) {
	if reapGrace <= defaultSubmitDeadline {
		t.Fatalf("reapGrace %v must exceed defaultSubmitDeadline %v, or the reaper races "+
			"every dispatch it overlaps with", reapGrace, defaultSubmitDeadline)
	}
}

// gonkSessionAlias must match every shape brokerSessionAlias actually mints:
// the issue-scoped triage form and the project-scoped scaffold form (issue 0).
// A reaper that cannot match either of these matches nothing gonk creates.
func TestGonkSessionAliasMatchesBrokerAliases(t *testing.T) {
	triage := brokerSessionAlias("triage", 75, 35, 1)
	if !gonkSessionAlias.MatchString(triage) {
		t.Errorf("gonkSessionAlias did not match a triage alias: %q", triage)
	}

	scaffold := brokerSessionAlias("scaffold", 75, 0, 1)
	if !gonkSessionAlias.MatchString(scaffold) {
		t.Errorf("gonkSessionAlias did not match a scaffold alias: %q", scaffold)
	}
}

// The nonce is REQUIRED, not optional. An alias in the pre-nonce shape is
// either stale (nothing mints it any more) or not ours, and a near-miss on the
// prefix must still fail even with a syntactically valid trailer.
func TestGonkSessionAliasRejectsAliasesWithoutTheNonce(t *testing.T) {
	for _, alias := range []string{
		"gonk.triage.p1.a1",    // old-style, pre-nonce, project-scoped
		"gonk.triage.p1.i1.a1", // old-style, pre-nonce, issue-scoped
		"gonkish.triage.p1.a1", // near-miss on the prefix
	} {
		if gonkSessionAlias.MatchString(alias) {
			t.Errorf("gonkSessionAlias matched %q; want no match", alias)
		}
	}
}

// Match on OUR alias scheme only. The city may hold sessions gonk did not create
// -- pool sessions, the control-dispatcher, a human's ad-hoc session. Closing
// someone else's session is not a leak fix, it is an outage.
func TestReaperNeverTouchesSessionsGonkDidNotCreate(t *testing.T) {
	gc := gcapitest.New(t)
	for _, id := range []string{
		"control-dispatcher",      // Gas City's own
		"scaffold-1",              // a pool session
		"triage-adhoc-58f5c50f2a", // someone's manual probe
		"gonkish.triage.p1.a1",    // near-miss on our prefix
		"notgonk.triage.p1.a1",
		"gonk.triage.p1.a1", // old-style, pre-nonce: no broker mints this any more
	} {
		gc.CreateSessionAt(id, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")
	}

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 6 {
		t.Fatalf("the reaper closed sessions gonk did not create. survivors = %v", live)
	}
}

// A record in a TERMINAL state means the session should not be alive: sweep
// judged it and its close either failed or never happened. Reaping is correct --
// this is the recovery path for gonk-xkm's loud "ITS POD IS LEAKED" error.
func TestReaperClosesASessionWhoseBeadIsAlreadyDone(t *testing.T) {
	gc := gcapitest.New(t)
	alias := "gonk.triage.p42.i3.a1." + testNonce
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(42)
	rec.SessionID = alias
	rec.State = beadstore.StateDone
	_ = store.Put(context.Background(), rec)

	runReap(context.Background(), reapDeps(t, gc, store, nil))

	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("a session whose bead is done should be reaped: %v", live)
	}
}

// IF WE CANNOT READ THE BEAD STORE, WE KNOW NOTHING ABOUT WHAT IS LIVE. Reaping
// on a failed store read would close every gonk session in the city, because
// every one of them would look unclaimed. This is the single most destructive
// thing the reaper could do and it must fail closed.
func TestReaperRefusesToActWhenTheBeadStoreCannotBeRead(t *testing.T) {
	gc := gcapitest.New(t)
	gc.CreateSessionAt("gonk.triage.p42.i3.a1."+testNonce, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	var logged strings.Builder
	d := reapDeps(t, gc, failingStore{}, &logged)
	runReap(context.Background(), d)

	if live := gc.LiveSessions(); len(live) != 1 {
		t.Fatal("THE REAPER CLOSED SESSIONS WITHOUT KNOWING WHICH WERE LIVE. " +
			"An unreadable bead store makes every session look unclaimed.")
	}
	if !strings.Contains(logged.String(), "reap: cannot read the bead store") {
		t.Fatalf("failing closed must be reported. log:\n%s", logged.String())
	}
}

// Pagination. The real route is keyset-paginated with a server cap, so a reaper
// that reads one page and stops silently under-reaps -- reporting success having
// missed most of what it exists to find.
func TestReaperFollowsPaginationToTheEnd(t *testing.T) {
	gc := gcapitest.New(t)
	gc.ListPageSize = 2
	for _, id := range []string{
		"gonk.triage.p1.i1.a1." + testNonce, "gonk.triage.p1.i2.a1." + testNonce, "gonk.triage.p1.i3.a1." + testNonce,
		"gonk.triage.p1.i4.a1." + testNonce, "gonk.triage.p1.i5.a1." + testNonce,
	} {
		gc.CreateSessionAt(id, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")
	}

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("the reaper stopped at the first page: %v still live", live)
	}
}

// A session with an unparseable created_at must be treated as TOO YOUNG, not as
// the zero time (which would read as "created in year 1, therefore ancient" and
// reap it immediately). Unknown age resolves toward leaving it alone.
func TestReaperTreatsAnUnreadableAgeAsTooYoung(t *testing.T) {
	gc := gcapitest.New(t)
	gc.CreateSessionAt("gonk.triage.p42.i3.a1."+testNonce, gcapitest.SessionRunning, "not-a-timestamp")

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 1 {
		t.Fatal("a session whose age could not be read was reaped; unknown age must fail safe")
	}
}

// THE WIRING. Everything above tests runReap directly, which proves the logic
// and nothing about whether it ever runs. A reaper that is never called is a
// silent no-op -- indistinguishable from a clean cluster, and exactly the shape
// of bug this codebase keeps producing. So: drive runSweep, not runReap, and
// assert the orphan is gone.
func TestSweepActuallyRunsTheReaper(t *testing.T) {
	gc := gcapitest.New(t)
	gc.CreateSessionAt("gonk.triage.p42.i3.a1."+testNonce, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	// No running and no parked beads, so the sweep's own passes are no-ops and
	// the only thing that can close this session is the reaper.
	d := reapDeps(t, gc, beadstore.NewMemory(), nil)
	if code := runSweep(context.Background(), d); code != 0 {
		t.Fatalf("sweep exit = %d, want 0", code)
	}

	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("runSweep did not reap: %v still live. The reaper is not wired in.", live)
	}
}

// failingStore makes every List fail, so the reaper's fail-closed path can be
// driven. Put/Get are unused by the reaper.
type failingStore struct{}

func (failingStore) Put(context.Context, beadstore.Record) error { return errStoreDown }
func (failingStore) Get(context.Context, string) (beadstore.Record, bool, error) {
	return beadstore.Record{}, false, errStoreDown
}
func (failingStore) List(context.Context, beadstore.State) ([]beadstore.Record, error) {
	return nil, errStoreDown
}
