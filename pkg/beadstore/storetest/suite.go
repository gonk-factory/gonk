// Package storetest is the single conformance suite every beadstore.Store
// implementation must satisfy. Run(t, func() beadstore.Store) runs the whole
// suite against a fresh, empty store; it runs against BOTH Memory and BdCLI
// (see ../conformance_test.go), so a behaviour one store has and the other
// does not cannot hide behind green tests.
//
// It exists because that is exactly what happened. gonk-sweep's pending-prompt
// reclaim ages a record by Record.UpdatedAt, and for a long time only
// Memory.Put stamped that field -- BdCLI.Put marshalled the caller's record
// verbatim. Every test in cmd/gonk-gate runs against Memory, so the reclaim
// passed its tests while being inert against the real store, where the field
// it reads was always the zero time. A stamp asserted on ONE implementation is
// not a store contract; this suite is what makes it one.
//
// It follows the pattern internal/meter/store/storetest already sets for the
// meter's stores.
package storetest

import (
	"context"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
)

// Run executes the conformance suite against a fresh store from newStore.
func Run(t *testing.T, newStore func() beadstore.Store) {
	t.Helper()
	t.Run("PutStampsAReservationThatCarriesNoWriteTime", func(t *testing.T) {
		putStampsAReservationThatCarriesNoWriteTime(t, newStore())
	})
	t.Run("PutRestampsOnEveryWrite", func(t *testing.T) {
		putRestampsOnEveryWrite(t, newStore())
	})
	t.Run("PutPreservesAnExistingReservationsAge", func(t *testing.T) {
		putPreservesAnExistingReservationsAge(t, newStore())
	})
	t.Run("PutRestampsWhenAReservationIsPromotedOutOfPendingPrompt", func(t *testing.T) {
		putRestampsWhenAReservationIsPromotedOutOfPendingPrompt(t, newStore())
	})
	t.Run("ListSeesTheStampedWriteTime", func(t *testing.T) {
		listSeesTheStampedWriteTime(t, newStore())
	})
}

// reservation is a pending-prompt record exactly as runBrokerDispatch writes
// one (cmd/gonk-gate/broker_inject.go): alias stamped, reservation carried,
// and -- the point -- NO UpdatedAt, because the record it is copied from is
// rebuilt fresh from the webhook args on every dispatch.
func reservation() beadstore.Record {
	return beadstore.Record{
		BeadAnchor: "gonk:42:issue:3", BeadID: "gk-1a2b", Project: "group/repo",
		ProjectID: 42, Rig: "group-repo", SessionKey: "gonk-42-issue-3",
		Trigger: "issue-triage", IssueIID: 3, Attempt: 1,
		State: beadstore.StatePendingPrompt, Rung: "cheap", Model: "m",
		ReservationID: "rsv-1", SessionID: "gonk.triage.p42.i3.a1",
	}
}

// THE ONE THE PREVIOUS FIX GOT WRONG. gonk-sweep's reclaim treats a
// pending-prompt record with a zero UpdatedAt as of UNKNOWN age and therefore
// too young to touch -- forever. So a store that does not stamp does not fail
// loudly; it quietly makes the reclaim inert, and the reservation it was meant
// to rescue leaks its meter reservation and never moves again.
func putStampsAReservationThatCarriesNoWriteTime(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	rec := reservation()
	before := time.Now().Add(-time.Second)
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	got, ok, err := s.Get(ctx, rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt is the zero time after Put.\n" +
			"The store did not stamp it, so gonk-sweep's pending-prompt reclaim reads this " +
			"reservation as being of UNKNOWN age, leaves it alone on every tick, and never " +
			"reclaims it. That failure is invisible to any test that only asserts the record " +
			"came back.")
	}
	if got.UpdatedAt.Before(before) || got.UpdatedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("UpdatedAt = %v, want a stamp from this Put (around %v)", got.UpdatedAt, before)
	}
}

// UpdatedAt means "when the store last wrote this record", so a caller handing
// back a stale value must not be believed. Read-modify-write is the ordinary
// shape in cmd/gonk-gate's sweep -- Get, change State, Put -- and if the store
// kept whatever came in, every such record's write time would freeze at its
// first write.
func putRestampsOnEveryWrite(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	ancient := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := reservation()
	rec.State = beadstore.StateRunning
	rec.UpdatedAt = ancient
	before := time.Now().Add(-time.Second)
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	got, ok, err := s.Get(ctx, rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if !got.UpdatedAt.After(ancient) {
		t.Fatalf("UpdatedAt = %v, want a fresh stamp: the store took the caller's stale %v "+
			"instead of recording when it actually wrote", got.UpdatedAt, ancient)
	}
	if got.UpdatedAt.Before(before) {
		t.Fatalf("UpdatedAt = %v, want a stamp from this Put (around %v)", got.UpdatedAt, before)
	}
}

// THE RELEASE-PATH SEMANTICS, decided and pinned. runBrokerDispatch's deferred
// release restores the record as it stood BEFORE its reservation write. When
// what it restores is itself a STRANDED pending-prompt record, an
// unconditional stamp would reset that record's reclaim clock -- so every
// failed dispatch would push the sweep's reclaim out by another full grace
// window, for a reservation whose entire problem is that nobody is coming back
// for it. A reservation's age is measured from when it was reserved; putting
// the same reservation back is not reserving it again.
func putPreservesAnExistingReservationsAge(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	reserved := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	rec := reservation()
	rec.UpdatedAt = reserved
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	got, ok, err := s.Get(ctx, rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if !got.UpdatedAt.Equal(reserved) {
		t.Fatalf("UpdatedAt = %v, want the reservation's own time %v.\n"+
			"Re-writing a pending-prompt record restamped it, which is how a failed dispatch "+
			"restoring a stranded reservation defers gonk-sweep's reclaim of it indefinitely.",
			got.UpdatedAt, reserved)
	}
}

// The other side of the exception, so it stays an exception: the reclaim's own
// write -- pending-prompt promoted to running -- genuinely changes the record,
// and its write time must move with it. A rule that froze UpdatedAt on any
// record that already had one would freeze it here too.
func putRestampsWhenAReservationIsPromotedOutOfPendingPrompt(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	reserved := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	rec := reservation()
	rec.UpdatedAt = reserved
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("seed Put = %v", err)
	}
	rec.State = beadstore.StateRunning
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("promote Put = %v", err)
	}
	got, ok, err := s.Get(ctx, rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if !got.UpdatedAt.After(reserved) {
		t.Fatalf("UpdatedAt = %v after the promotion to %q, want a fresh stamp (was %v)",
			got.UpdatedAt, beadstore.StateRunning, reserved)
	}
}

// The sweep never Gets a pending-prompt record by anchor -- it Lists the state
// and ages whatever comes back. If the write time survived Get but not List,
// the reclaim would still see nothing.
func listSeesTheStampedWriteTime(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.Put(ctx, reservation()); err != nil {
		t.Fatalf("Put = %v", err)
	}
	recs, err := s.List(ctx, beadstore.StatePendingPrompt)
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("List returned %d records, want 1", len(recs))
	}
	if recs[0].UpdatedAt.IsZero() {
		t.Fatal("List returned a reservation with no write time -- this is the exact record " +
			"gonk-sweep ages, and it cannot age a zero time")
	}
	if recs[0].SessionID == "" || recs[0].ReservationID == "" {
		t.Fatalf("List dropped correlation fields: %+v", recs[0])
	}
}
