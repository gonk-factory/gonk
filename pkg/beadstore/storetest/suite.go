// Package storetest is the single conformance suite every beadstore.Store
// implementation must satisfy. Run(t, func(t *testing.T) beadstore.Store) runs the whole
// suite against a fresh, empty store; it runs against Memory, BdCLI (see
// ../conformance_test.go) and Postgres (../postgres_test.go, build tag
// integration), so a behaviour one store has and another does not cannot hide
// behind green tests. RunCompareAndSwap is the concurrent part; see its comment
// for which stores pass it.
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
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// Run executes the conformance suite against a fresh store from newStore.
//
// newStore takes the SUBTEST's *testing.T, not the caller's (gonk-upxg): a
// Postgres-backed factory registers its pool's Close (and its database's DROP)
// with t.Cleanup, and with the outer T every one of these stores stayed open
// until the whole suite finished -- dozens of live pools against one server,
// which exhausted its connection slots when this package and
// internal/meter/store ran together (SQLSTATE 53300). Per-subtest teardown
// keeps at most one store alive at a time.
func Run(t *testing.T, newStore func(t *testing.T) beadstore.Store) {
	t.Helper()
	t.Run("PutStampsAReservationThatCarriesNoWriteTime", func(t *testing.T) {
		putStampsAReservationThatCarriesNoWriteTime(t, newStore(t))
	})
	t.Run("PutRestampsOnEveryWrite", func(t *testing.T) {
		putRestampsOnEveryWrite(t, newStore(t))
	})
	t.Run("PutPreservesAnExistingReservationsAge", func(t *testing.T) {
		putPreservesAnExistingReservationsAge(t, newStore(t))
	})
	t.Run("PutRestampsWhenAReservationIsPromotedOutOfPendingPrompt", func(t *testing.T) {
		putRestampsWhenAReservationIsPromotedOutOfPendingPrompt(t, newStore(t))
	})
	t.Run("ListSeesTheStampedWriteTime", func(t *testing.T) {
		listSeesTheStampedWriteTime(t, newStore(t))
	})

	// gonk-p8w8: revisions and the conditional write.
	t.Run("PutAssignsAFreshRevisionOnEveryWrite", func(t *testing.T) {
		putAssignsAFreshRevisionOnEveryWrite(t, newStore(t))
	})
	t.Run("ListCarriesTheRevisionGetDoes", func(t *testing.T) {
		listCarriesTheRevisionGetDoes(t, newStore(t))
	})
	t.Run("PutIfWritesWhenTheRecordIsUnchanged", func(t *testing.T) {
		putIfWritesWhenTheRecordIsUnchanged(t, newStore(t))
	})
	t.Run("PutIfRefusesARecordThatChangedAfterItWasListed", func(t *testing.T) {
		putIfRefusesARecordThatChangedAfterItWasListed(t, newStore(t))
	})
	t.Run("PutIfOnAnAbsentRecord", func(t *testing.T) {
		putIfOnAnAbsentRecord(t, newStore(t))
	})
	t.Run("PutIfRefusesARevisionItHasItselfReplaced", func(t *testing.T) {
		putIfRefusesARevisionItHasItselfReplaced(t, newStore(t))
	})
	t.Run("ReslungAtRoundTrips", func(t *testing.T) {
		reslungAtRoundTrips(t, newStore(t))
	})
	// gonk-kx3 / gonk-t41: List is the sweep's whole view of the world.
	t.Run("ListIsNotTruncated", func(t *testing.T) {
		listIsNotTruncated(t, newStore(t))
	})
	// gonk-tios verify report R6: run maintenance reads only open runs.
	t.Run("ListOpenPipelineRunsReturnsOnlyOpenRuns", func(t *testing.T) {
		listOpenPipelineRunsReturnsOnlyOpenRuns(t, newStore(t))
	})
	// gonk-2usw FACTS: what a pipeline-fix is judged against must survive the
	// store, through Get, List and ListOpenPipelineRuns alike.
	t.Run("PipelineFailureFactsRoundTrip", func(t *testing.T) {
		pipelineFailureFactsRoundTrip(t, newStore(t))
	})
}

// pipelineFailureFactsRoundTrip: the per-job failure facts and the landed
// diagnosis come back from every read exactly as written -- every field, in
// order -- and a record that never had them reads as "not recorded".
// Synthetic values throughout.
func pipelineFailureFactsRoundTrip(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	rec := reservation()
	rec.BeadAnchor = "gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880"
	rec.Trigger, rec.State, rec.PipelineRunOpen = "pipeline-diagnosis", beadstore.StateDone, true
	rec.PipelineID, rec.PipelineTarget = 900, "ref:main"
	rec.PipelineJobs = []beadstore.PipelineJob{
		{ID: 9002, Name: "unit-tests", Excerpted: true, ExcerptSHA256: "aa", Groundable: true,
			Stage: "test", FailureReason: "script_failure", ErrorSignature: "es1-0123456789abcdef", SignatureVersion: 1},
		{ID: 9003, Name: "lint", Stage: "check", FailureReason: "runner_system_failure", AllowFailure: true,
			ErrorGeneric: true, SignatureVersion: 1},
	}
	rec.PipelineDiagnosis = beadstore.PipelineDiagnosis{
		Class: "code", Files: []string{"calc/sum.py", "calc/total.py"}, EvidenceJobs: []string{"unit-tests"},
		Summary: "The helper returns a different total than the test expects.",
		Remedy:  "Make the helper add every element.",
	}
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	check := func(how string, got beadstore.Record) {
		t.Helper()
		if fmt.Sprintf("%+v", got.PipelineJobs) != fmt.Sprintf("%+v", rec.PipelineJobs) {
			t.Fatalf("%s: PipelineJobs\n got  %+v\n want %+v", how, got.PipelineJobs, rec.PipelineJobs)
		}
		if fmt.Sprintf("%+v", got.PipelineDiagnosis) != fmt.Sprintf("%+v", rec.PipelineDiagnosis) {
			t.Fatalf("%s: PipelineDiagnosis\n got  %+v\n want %+v", how, got.PipelineDiagnosis, rec.PipelineDiagnosis)
		}
		if !got.PipelineDiagnosis.Recorded() || !got.PipelineJobs[0].FailureFactsRecorded() {
			t.Fatalf("%s: the facts read back as not recorded: %+v", how, got)
		}
	}
	got, ok, err := s.Get(ctx, rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	check("Get", got)
	listed, err := s.List(ctx, beadstore.StateDone)
	if err != nil || len(listed) != 1 {
		t.Fatalf("List = %d records, %v", len(listed), err)
	}
	check("List", listed[0])
	open, err := s.ListOpenPipelineRuns(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("ListOpenPipelineRuns = %d records, %v", len(open), err)
	}
	check("ListOpenPipelineRuns", open[0])

	// A record that never had the facts: nothing is invented on the way back.
	plain := reservation()
	plain.BeadAnchor = "gonk:42:pipeline:ref-0d6e4079:fedcba9876543210:s880"
	plain.PipelineJobs = []beadstore.PipelineJob{{ID: 9100, Name: "unit-tests", Excerpted: true, ExcerptSHA256: "bb", Groundable: true}}
	if err := s.Put(ctx, plain); err != nil {
		t.Fatalf("Put = %v", err)
	}
	got, ok, err = s.Get(ctx, plain.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if got.PipelineDiagnosis.Recorded() || len(got.PipelineJobs) != 1 || got.PipelineJobs[0].FailureFactsRecorded() {
		t.Fatalf("a record without failure facts read back with some: %+v", got)
	}
}

// listOpenPipelineRunsReturnsOnlyOpenRuns: every record with PipelineRunOpen,
// in every state, in anchor order -- and nothing else: not a closed run, not a
// record of another trigger, not one that has never had the field. A run that
// closes drops out on its next write.
func listOpenPipelineRunsReturnsOnlyOpenRuns(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	var want []string
	for i, st := range beadstore.AllStates {
		for _, open := range []bool{true, false} {
			rec := reservation()
			rec.BeadAnchor = fmt.Sprintf("gonk:42:pipeline:ref-0d6e4079:%02d%v:s1", i, open)
			rec.Trigger, rec.State, rec.PipelineRunOpen = "pipeline-diagnosis", st, open
			rec.PipelineID, rec.PipelineTarget = 900, "ref:main"
			if err := s.Put(ctx, rec); err != nil {
				t.Fatalf("Put = %v", err)
			}
			if open {
				want = append(want, rec.BeadAnchor)
			}
		}
	}
	plain := reservation() // an issue record: the field was never set
	plain.State = beadstore.StateDone
	if err := s.Put(ctx, plain); err != nil {
		t.Fatalf("Put = %v", err)
	}
	anchors := func() []string {
		recs, err := s.ListOpenPipelineRuns(ctx)
		if err != nil {
			t.Fatalf("ListOpenPipelineRuns = %v", err)
		}
		var out []string
		for _, r := range recs {
			if !r.PipelineRunOpen || r.Revision == "" {
				t.Fatalf("ListOpenPipelineRuns returned %s open=%v revision=%q", r.BeadAnchor, r.PipelineRunOpen, r.Revision)
			}
			out = append(out, r.BeadAnchor)
		}
		return out
	}
	sort.Strings(want)
	if got := anchors(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ListOpenPipelineRuns = %v, want exactly the open runs %v in anchor order", got, want)
	}
	closing := mustGet(t, s, want[0])
	closing.PipelineRunOpen = false
	if _, err := s.PutIf(ctx, closing, closing.Revision); err != nil {
		t.Fatalf("PutIf = %v", err)
	}
	if got := anchors(); fmt.Sprint(got) != fmt.Sprint(want[1:]) {
		t.Fatalf("after %s closed, ListOpenPipelineRuns = %v, want %v", want[0], got, want[1:])
	}
}

// gonk-kx3. bd's `list` defaults to 50 rows, and BdCLI.List passed no limit, so
// past 50 running records the sweep silently judged only 50 -- the rest were
// never closed, and that looked exactly like "nothing to do". A store whose List
// stops anywhere short of every record in the state fails here. 120 is past
// bd's 50 and past any plausible page size of 100.
func listIsNotTruncated(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	const n = 120
	for i := 0; i < n; i++ {
		rec := reservation()
		rec.BeadAnchor = fmt.Sprintf("gonk:42:issue:%d", 1000+i)
		rec.State = beadstore.StateRunning
		if err := s.Put(ctx, rec); err != nil {
			t.Fatalf("Put %d = %v", i, err)
		}
	}
	other := reservation()
	other.BeadAnchor, other.State = "gonk:42:issue:1", beadstore.StateParked
	if err := s.Put(ctx, other); err != nil {
		t.Fatalf("Put parked = %v", err)
	}
	recs, err := s.List(ctx, beadstore.StateRunning)
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if len(recs) != n {
		t.Fatalf("List(running) returned %d records, want all %d: a truncated List is a sweep that "+
			"silently never judges the rest", len(recs), n)
	}
	seen := map[string]bool{}
	for _, r := range recs {
		if r.State != beadstore.StateRunning || seen[r.BeadAnchor] {
			t.Fatalf("List(running) returned %s in state %q (duplicate=%v)", r.BeadAnchor, r.State, seen[r.BeadAnchor])
		}
		seen[r.BeadAnchor] = true
	}
}

// RunCompareAndSwap is the part of the contract that needs a REAL
// compare-and-swap underneath: concurrent PutIf writers whose applied writes
// must form one chain, and stay applied (gonk-vred). Every store gonk-gate may
// select for its state must pass it; Memory and Postgres do.
//
// BdCLI does not, and is not run against it (see
// ../bd_notfinal_test.go, which pins exactly how it fails). That is why
// gonk-gate no longer selects BdCLI for state (gonk-t41): the owner's decision
// was to move the state, not to hold BdCLI to a contract bd cannot give it.
func RunCompareAndSwap(t *testing.T, newStore func(t *testing.T) beadstore.Store) {
	t.Helper()
	t.Run("PutIfSuccessIsFinal", func(t *testing.T) {
		putIfSuccessIsFinal(t, newStore(t))
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

func mustGet(t *testing.T, s beadstore.Store, anchor string) beadstore.Record {
	t.Helper()
	got, ok, err := s.Get(context.Background(), anchor)
	if err != nil || !ok {
		t.Fatalf("Get(%s) = %+v, %v, %v", anchor, got, ok, err)
	}
	return got
}

// A revision names one write. Every write gets a new one -- including a
// pending-prompt re-write that keeps its UpdatedAt, because keeping the write
// TIME is about a reservation's age, while two writes of it are still two
// writes -- and a value the caller passes in is not believed.
func putAssignsAFreshRevisionOnEveryWrite(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	rec := reservation()
	rec.Revision = "chosen-by-the-caller"
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	first := mustGet(t, s, rec.BeadAnchor)
	if first.Revision == "" || first.Revision == "chosen-by-the-caller" {
		t.Fatalf("Revision = %q after Put, want a store-assigned value", first.Revision)
	}
	if err := s.Put(ctx, first); err != nil { // the same reservation, written back
		t.Fatalf("second Put = %v", err)
	}
	second := mustGet(t, s, rec.BeadAnchor)
	if second.Revision == "" || second.Revision == first.Revision {
		t.Fatalf("Revision %q -> %q across two writes, want a new one", first.Revision, second.Revision)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("UpdatedAt moved (%v -> %v): the revision change must not disturb the reservation-age rule",
			first.UpdatedAt, second.UpdatedAt)
	}
}

// gonk-sweep conditions its writes on the copy it LISTED, not on a Get. A
// revision that survived Get but not List would make every sweep write stale.
func listCarriesTheRevisionGetDoes(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	rec := reservation()
	rec.State = beadstore.StateRunning
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	recs, err := s.List(ctx, beadstore.StateRunning)
	if err != nil || len(recs) != 1 {
		t.Fatalf("List = %+v, %v", recs, err)
	}
	if got := mustGet(t, s, rec.BeadAnchor); recs[0].Revision == "" || recs[0].Revision != got.Revision {
		t.Fatalf("List revision %q, Get revision %q: want the same, non-empty", recs[0].Revision, got.Revision)
	}
}

func putIfWritesWhenTheRecordIsUnchanged(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	rec := reservation()
	rec.State = beadstore.StateRunning
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	read := mustGet(t, s, rec.BeadAnchor)
	read.State = beadstore.StateDone
	rev, err := s.PutIf(ctx, read, read.Revision)
	if err != nil {
		t.Fatalf("PutIf against the revision just read = %v, want it written", err)
	}
	got := mustGet(t, s, rec.BeadAnchor)
	if got.State != beadstore.StateDone {
		t.Fatalf("State = %q, want %q", got.State, beadstore.StateDone)
	}
	if rev == "" || rev != got.Revision || rev == read.Revision {
		t.Fatalf("PutIf returned revision %q; stored %q; read %q -- want the new stored one", rev, got.Revision, read.Revision)
	}
}

// THE INCIDENT, at the store boundary (gonk-p8w8, go-3tbq's comment log). A
// sweep lists `running a1`; a dispatch then writes its pending-prompt
// reservation for attempt 2; the sweep writes its listed copy back. That write
// must be refused, and the reservation must be what every reader still sees.
func putIfRefusesARecordThatChangedAfterItWasListed(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	a1 := reservation()
	a1.State = beadstore.StateRunning
	if err := s.Put(ctx, a1); err != nil {
		t.Fatalf("Put a1 = %v", err)
	}
	listed, err := s.List(ctx, beadstore.StateRunning)
	if err != nil || len(listed) != 1 {
		t.Fatalf("List = %+v, %v", listed, err)
	}

	a2 := reservation()
	a2.Attempt, a2.SessionID, a2.ReservationID = 2, "gonk.triage.p42.i3.a2", "rsv-2"
	if err := s.Put(ctx, a2); err != nil { // the dispatch
		t.Fatalf("Put a2 reservation = %v", err)
	}

	stale := listed[0]
	stale.ReslungAt = time.Now()
	_, err = s.PutIf(ctx, stale, stale.Revision)
	if !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("PutIf of the copy listed before the reservation = %v, want ErrStale", err)
	}
	got := mustGet(t, s, a1.BeadAnchor)
	if got.State != beadstore.StatePendingPrompt || got.Attempt != 2 || got.SessionID != a2.SessionID {
		t.Fatalf("after a refused stale write the record is %s Attempt %d %s, want the reservation "+
			"(pending-prompt, Attempt 2, %s): a refused write must change nothing",
			got.State, got.Attempt, got.SessionID, a2.SessionID)
	}
	if recs, err := s.List(ctx, beadstore.StateRunning); err != nil || len(recs) != 0 {
		t.Fatalf("List(running) = %+v, %v after the refused write, want nothing", recs, err)
	}
	if recs, err := s.List(ctx, beadstore.StatePendingPrompt); err != nil || len(recs) != 1 {
		t.Fatalf("List(pending-prompt) = %+v, %v, want the reservation", recs, err)
	}
}

// "" is a real precondition, not "no precondition": it means "nothing is
// stored yet" (or only a record from before revisions existed).
func putIfOnAnAbsentRecord(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	rec := reservation()
	if _, err := s.PutIf(ctx, rec, "some-revision"); !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("PutIf on an absent record with a non-empty precondition = %v, want ErrStale", err)
	}
	if got, ok, err := s.Get(ctx, rec.BeadAnchor); err != nil || ok {
		t.Fatalf("a refused PutIf created a record: %+v, %v, %v", got, ok, err)
	}
	if _, err := s.PutIf(ctx, rec, ""); err != nil {
		t.Fatalf("PutIf on an absent record with the empty precondition = %v, want it written", err)
	}
	if _, err := s.PutIf(ctx, rec, ""); !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("PutIf with the empty precondition on a record that now exists = %v, want ErrStale", err)
	}
}

// The revision PutIf returns is the one to condition the NEXT write on, and
// the one it replaced is stale from then on -- a caller cannot win twice with
// one read.
func putIfRefusesARevisionItHasItselfReplaced(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	rec := reservation()
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	read := mustGet(t, s, rec.BeadAnchor)
	read.State = beadstore.StateRunning
	rev, err := s.PutIf(ctx, read, read.Revision)
	if err != nil {
		t.Fatalf("first PutIf = %v", err)
	}
	if _, err := s.PutIf(ctx, read, read.Revision); !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("second PutIf on the same read = %v, want ErrStale", err)
	}
	read.State = beadstore.StateDone
	if _, err := s.PutIf(ctx, read, rev); err != nil {
		t.Fatalf("PutIf chained on the returned revision = %v", err)
	}
	if got := mustGet(t, s, rec.BeadAnchor); got.State != beadstore.StateDone {
		t.Fatalf("State = %q, want done", got.State)
	}
}

func reslungAtRoundTrips(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 9, 13, 15, 49, 0, 87479798, time.UTC)
	fired := at.Add(240 * time.Second) // the deadline a re-sling at `at` passes
	rec := reservation()
	rec.State, rec.ReslungAt, rec.ReslingFireBy = beadstore.StateRunning, at, fired
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	if got := mustGet(t, s, rec.BeadAnchor); !got.ReslungAt.Equal(at) || !got.ReslingFireBy.Equal(fired) {
		t.Fatalf("Get ReslungAt = %v, ReslingFireBy = %v, want %v and %v", got.ReslungAt, got.ReslingFireBy, at, fired)
	}
	recs, err := s.List(ctx, beadstore.StateRunning)
	if err != nil || len(recs) != 1 || !recs[0].ReslungAt.Equal(at) || !recs[0].ReslingFireBy.Equal(fired) {
		t.Fatalf("List = %+v, %v, want ReslungAt %v and ReslingFireBy %v", recs, err, at, fired)
	}
}

// SUCCESS IS FINAL (gonk-vred). Every guarantee built on PutIf -- the
// dispatch's reservation compare-and-swap (one session when two dispatches
// fire on time), its release undoing only its own reservation, the sweep firing
// only after its re-sling write applied -- assumes that a PutIf which returned a
// revision stays the write that followed the revision it was conditioned on,
// whatever the store does afterwards. BdCLI once broke that: bd orders comments
// by a timestamp taken before commit, so a stalled write could land in front of
// one that had already returned, and both were told "applied".
//
// Expressed through the Store API alone: writers race read-increment-PutIf
// from an ABSENT record (so first writers race to create it too). Each applied
// write claims the attempt number one past what it read. With a real
// compare-and-swap the applied writes form a single chain, so after everything
// has finished:
//
//   - no two applied writes claim the same attempt (two writers both told
//     applied on one revision);
//   - the applied attempts are exactly 1..n, and the stored record is attempt n
//     under the revision its writer was given (nothing that returned was
//     overturned by what landed after it);
//   - every earlier applied revision is refused as stale from then on.
func putIfSuccessIsFinal(t *testing.T, s beadstore.Store) {
	t.Helper()
	ctx := context.Background()
	anchor := reservation().BeadAnchor
	const writers, rounds = 6, 6

	type applied struct {
		attempt int
		rev, by string
	}
	var (
		mu   sync.Mutex
		wins []applied
		wg   sync.WaitGroup
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				cur, found, err := s.Get(ctx, anchor)
				if err != nil {
					t.Errorf("writer %d: Get = %v", w, err)
					return
				}
				next := reservation()
				next.State, next.Attempt, next.SessionID = beadstore.StateRunning, 1, fmt.Sprintf("w%d.r%d", w, i)
				ifRevision := ""
				if found {
					next.Attempt, ifRevision = cur.Attempt+1, cur.Revision
				}
				rev, err := s.PutIf(ctx, next, ifRevision)
				switch {
				case err == nil:
					mu.Lock()
					wins = append(wins, applied{next.Attempt, rev, next.SessionID})
					mu.Unlock()
				case errors.Is(err, beadstore.ErrStale):
				default:
					t.Errorf("writer %d: PutIf = %v, want applied or ErrStale", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if len(wins) == 0 {
		t.Fatal("no write applied")
	}

	byAttempt := map[int]applied{}
	for _, w := range wins {
		if prev, dup := byAttempt[w.attempt]; dup {
			t.Errorf("%s and %s were BOTH told applied as attempt %d: two writes conditioned on the same "+
				"revision both succeeded", prev.by, w.by, w.attempt)
		}
		byAttempt[w.attempt] = w
	}
	for a := 1; a <= len(wins); a++ {
		if _, ok := byAttempt[a]; !ok {
			t.Errorf("%d writes applied but none as attempt %d: the applied writes are not one chain", len(wins), a)
			break
		}
	}
	got := mustGet(t, s, anchor)
	last, ok := byAttempt[len(wins)]
	if !ok || got.Attempt != last.attempt || got.Revision != last.rev || got.SessionID != last.by {
		t.Fatalf("stored record is attempt %d rev %q (%s); the last applied write was attempt %d rev %q (%s): "+
			"a write that was told it applied is not the record", got.Attempt, got.Revision, got.SessionID,
			last.attempt, last.rev, last.by)
	}
	for _, w := range wins {
		if w.rev == last.rev {
			continue
		}
		stale := reservation()
		stale.State = beadstore.StateDone
		if _, err := s.PutIf(ctx, stale, w.rev); !errors.Is(err, beadstore.ErrStale) {
			t.Fatalf("PutIf on %s's superseded revision = %v, want ErrStale", w.by, err)
		}
	}
	if again := mustGet(t, s, anchor); again.Revision != last.rev {
		t.Fatalf("record moved to %q after only refused writes", again.Revision)
	}
}
