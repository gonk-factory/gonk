package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// T-19. These cover the two halves of the pending-prompt reservation T-13
// introduced: what runBrokerDispatch puts back when its dispatch fails, and
// what gonk-sweep does about a reservation whose dispatch never put anything
// back at all.

// failingDispatchDeps builds a dispatch that is guaranteed to fail AFTER the
// pending-prompt reservation has been written and BEFORE any session is
// created. Keys is deliberately nil: resolveLiteLLMKey fails closed on it
// (gonk-8gb), which is one of the ordinary transient failures -- alongside a
// prompt PUT that 500s or a refused create -- that the old release handled
// wrongly. Nothing here needs a crash.
func failingDispatchDeps(t *testing.T, store beadstore.Store, gc *gcapitest.Server, attempt int) dispatchDeps {
	t.Helper()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m",
		Attempt: attempt, ReservationID: "rsv-1",
	}}
	return dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Forge: stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}},
		Args:  baseDispatchArgs(),
		// Keys intentionally unset -- see above.
	}
}

// THE REGRESSION T-13 SHIPPED. The reservation is a copy of `base`, which
// runDispatch rebuilds fresh from the webhook args on every dispatch, so it
// carries none of the state the bead was already in. Releasing that copy
// therefore OVERWROTE the prior state: a parked bead -- one runSweep does List
// and would have unparked at RetryAfter -- was downgraded to pending-prompt by
// a single failed re-dispatch and stopped being reclaimable at all. T-13 made a
// recoverable bead unrecoverable, which is worse than the duplicate session it
// was fixing.
func TestFailedReDispatchLeavesAParkedBeadParked(t *testing.T) {
	store := beadstore.NewMemory()
	gc := gcapitest.New(t)

	retryAfter := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	parked := beadstore.Record{
		BeadAnchor: "gonk:42:issue:3", BeadID: "gk-1a2b", Project: "group/repo",
		ProjectID: 42, Rig: "group-repo", SessionKey: "gonk-42-issue-3",
		Trigger: "issue-triage", IssueIID: 3,
		State: beadstore.StateParked, Attempt: 1, RetryAfter: retryAfter,
	}
	if err := store.Put(context.Background(), parked); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	if code := runDispatch(context.Background(), failingDispatchDeps(t, store, gc, 2)); code != 1 {
		t.Fatalf("dispatch exit = %d, want 1 (it must fail: that is the whole scenario)", code)
	}

	got, ok, err := store.Get(context.Background(), parked.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("get after failed dispatch: ok=%v err=%v", ok, err)
	}
	if got.State != beadstore.StateParked {
		t.Fatalf("state after a FAILED re-dispatch = %q, want %q.\n"+
			"A parked bead that gets downgraded to pending-prompt is invisible to "+
			"runSweep's unpark pass and never progresses again.",
			got.State, beadstore.StateParked)
	}
	if !got.RetryAfter.Equal(retryAfter) {
		t.Fatalf("RetryAfter = %v, want %v -- the unpark deadline must survive a failed re-dispatch",
			got.RetryAfter, retryAfter)
	}
	if got.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty: the reservation must not outlive the dispatch that took it", got.SessionID)
	}
}

// The first-dispatch shape of the same defect. There was no record before, so
// the honest restore is a record that claims nothing: no session id, and not
// left sitting in pending-prompt, which no sweep pass would ever have looked
// at.
func TestFailedFirstDispatchLeavesNoPendingPromptReservation(t *testing.T) {
	store := beadstore.NewMemory()
	gc := gcapitest.New(t)

	if code := runDispatch(context.Background(), failingDispatchDeps(t, store, gc, 1)); code != 1 {
		t.Fatalf("dispatch exit = %d, want 1", code)
	}

	got, ok, err := store.Get(context.Background(), "gonk:42:issue:3")
	if err != nil {
		t.Fatalf("get after failed dispatch: %v", err)
	}
	if !ok {
		return // no record at all is also "nothing is reserved"; nothing left to check
	}
	if got.State == beadstore.StatePendingPrompt {
		t.Fatalf("state = %q after a failed FIRST dispatch.\n"+
			"Releasing by clearing SessionID alone leaves the record in a state "+
			"nothing reclaims; it must be put back the way it was found.", got.State)
	}
	if got.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty", got.SessionID)
	}
}

// pendingPromptRecord is a reservation exactly as runBrokerDispatch writes one:
// pending-prompt, alias already stamped, reservation carried, and no
// SessionEndedAt because no session ever reported anything.
func pendingPromptRecord(alias string) beadstore.Record {
	return beadstore.Record{
		BeadAnchor: "gonk:42:issue:3", BeadID: "gk-1a2b", Project: "group/repo",
		ProjectID: 42, Rig: "group-repo", SessionKey: "gonk-42-issue-3",
		Trigger: "issue-triage", IssueIID: 3,
		State: beadstore.StatePendingPrompt, Rung: "cheap", Model: "m", Attempt: 1,
		ReservationID: "rsv-1", SessionID: alias,
	}
}

// The hard-crash case the deferred release above CANNOT cover: a dispatch
// process killed outright runs no defer, so its reservation sits in
// pending-prompt with nobody coming back for it. Until this pass existed
// nothing in the tree Listed that state, so the meter's reservation for the
// attempt was never settled (a slow budget leak) and the work item silently
// stopped progressing.
func TestSweepReclaimsAPendingPromptRecordPastTheDeliveryWindow(t *testing.T) {
	store := beadstore.NewMemory()
	alias := brokerSessionAlias("triage", 42, 3, 1)
	rec := pendingPromptRecord(alias)
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	gc := gcapitest.New(t)
	var log strings.Builder
	// The reservation was written now; look at it from beyond the window.
	future := time.Now().Add(pendingPromptGrace + time.Minute)

	code := runSweep(context.Background(), sweepDeps{
		GC: gc.Client("gonk-city"), Store: store, Log: testLogger(&log),
		Now: func() time.Time { return future },
	})
	if code != 0 {
		t.Fatalf("sweep exit = %d, want 0", code)
	}

	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StateRunning {
		t.Fatalf("state = %q, want %q.\n"+
			"A pending-prompt record older than %v has no dispatch behind it; leaving it "+
			"there means its reservation is never settled and the bead never moves again.",
			got.State, beadstore.StateRunning, pendingPromptGrace)
	}
	if got.SessionID != alias {
		t.Fatalf("SessionID = %q, want %q -- the alias is what lets sweepRunning read and close the session",
			got.SessionID, alias)
	}
	if got.ReservationID != rec.ReservationID {
		t.Fatalf("ReservationID = %q, want %q -- an outcome unbound to the reservation cannot settle it",
			got.ReservationID, rec.ReservationID)
	}
	if !strings.Contains(log.String(), "reclaimed a pending-prompt reservation") {
		t.Fatalf("the reclaim was silent; a dispatch that died without releasing is a real failure:\n%s", log.String())
	}
}

// The other half, and without it the age cutoff is untested: a
// reclaim-everything bug passes the test above and fails here. A reservation
// INSIDE the window belongs to a dispatch that is very probably still creating
// its session and waiting on the pod. Promoting it there would race that
// dispatch for no benefit.
func TestSweepLeavesAPendingPromptRecordInsideTheDeliveryWindowAlone(t *testing.T) {
	store := beadstore.NewMemory()
	alias := brokerSessionAlias("triage", 42, 3, 1)
	rec := pendingPromptRecord(alias)
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	gc := gcapitest.New(t)
	// One second short of the cutoff: still inside the window, by the smallest
	// margin the check has to get right.
	inside := time.Now().Add(pendingPromptGrace - time.Second)

	code := runSweep(context.Background(), sweepDeps{
		GC: gc.Client("gonk-city"), Store: store,
		Now: func() time.Time { return inside },
	})
	if code != 0 {
		t.Fatalf("sweep exit = %d, want 0", code)
	}

	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StatePendingPrompt {
		t.Fatalf("state = %q, want %q: the sweep reclaimed a reservation whose dispatch is "+
			"still inside its %v delivery window", got.State, beadstore.StatePendingPrompt, pendingPromptGrace)
	}
}

// A record with no write time has an UNKNOWN age, and unknown age must read as
// too young -- the zero time would otherwise read as "written in year 1,
// therefore ancient" and reclaim a reservation taken a second ago, out from
// under a dispatch mid-create. Same bias as the reaper's unparseable
// created_at.
func TestSweepLeavesAPendingPromptRecordWithNoWriteTimeAlone(t *testing.T) {
	alias := brokerSessionAlias("triage", 42, 3, 1)
	rec := pendingPromptRecord(alias)
	// Every beadstore implementation stamps UpdatedAt on Put (stampWriteTime),
	// so the zero-time case has to be staged through a store that does not.
	store := &noStampStore{recs: map[string]beadstore.Record{rec.BeadAnchor: rec}}

	gc := gcapitest.New(t)
	var log strings.Builder
	future := time.Now().Add(24 * time.Hour)

	if code := runSweep(context.Background(), sweepDeps{
		GC: gc.Client("gonk-city"), Store: store, Log: testLogger(&log),
		Now: func() time.Time { return future },
	}); code != 0 {
		t.Fatalf("sweep exit = %d, want 0", code)
	}

	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StatePendingPrompt {
		t.Fatalf("state = %q: a record of unknown age was reclaimed", got.State)
	}
	if !strings.Contains(log.String(), "no write time") {
		t.Fatalf("skipping an unknown-age record must be said out loud:\n%s", log.String())
	}
}

// noStampStore is beadstore.Memory's semantics minus the UpdatedAt stamp, so a
// test can stage a record whose write time is genuinely unknown -- a record
// written by a pre-stamp gonk, or read back from a bd bead whose gonk-state
// comment predates the field. No current Store implementation behaves this
// way; that is the point of pkg/beadstore/storetest.
type noStampStore struct{ recs map[string]beadstore.Record }

func (s *noStampStore) Put(_ context.Context, r beadstore.Record) error {
	s.recs[r.BeadAnchor] = r
	return nil
}

func (s *noStampStore) Get(_ context.Context, k string) (beadstore.Record, bool, error) {
	r, ok := s.recs[k]
	return r, ok, nil
}

func (s *noStampStore) List(_ context.Context, st beadstore.State) ([]beadstore.Record, error) {
	var out []beadstore.Record
	for _, r := range s.recs {
		if r.State == st {
			out = append(out, r)
		}
	}
	return out, nil
}

// THE MARGIN, asserted rather than assumed. claimedSessionIDs (reap.go) counts
// only StateRunning records as claiming a session, so a pending-prompt alias is
// unclaimed for exactly as long as it sits in that state. That is safe today
// only because reapGrace exceeds the delivery window -- and it stays safe only
// if the reclaim promotes the record to StateRunning well before the reaper
// would consider closing its session.
func TestPendingPromptGraceLandsInsideTheReaperGrace(t *testing.T) {
	// ASSERT THE MARGIN THE COMMENT CLAIMS, not a bare `<`. sweep.go says the
	// reclaim promotes a reservation into the claimed set "with 5 minutes to
	// spare"; under `<` alone a pendingPromptGrace of 599s would pass while
	// leaving one second of it, which is not a margin, it is a coincidence.
	//
	// There used to be a second check here, that pendingPromptGrace is not
	// shorter than defaultSubmitDeadline. It was deleted rather than kept:
	// pendingPromptGrace is DEFINED as defaultSubmitDeadline plus a positive
	// constant, so no edit to either value could ever make that comparison
	// false. It was a constant-expression tautology wearing an assertion's
	// clothes, and a check that cannot fail is worse than no check -- it reads
	// as coverage. The real version of that claim is the order-timeout test
	// below, which compares this constant against a value declared in another
	// file entirely.
	const wantMargin = 5 * time.Minute
	if margin := reapGrace - pendingPromptGrace; margin < wantMargin {
		t.Fatalf("reapGrace %v leaves only %v over pendingPromptGrace %v, want at least %v.\n"+
			"A pending-prompt session is unclaimed until the sweep promotes it, so the reaper "+
			"can close the session the sweep is about to settle.",
			reapGrace, margin, pendingPromptGrace, wantMargin)
	}
}

// The OTHER claim pendingPromptGrace's comment makes, and the one that can
// actually go stale: that the grace covers gonk-dispatch's own order timeout,
// so "Gas City kills the process at that point, so a reservation older than
// this provably has no live dispatch behind it". That timeout is declared in
// pack/orders/gonk-dispatch.toml, which nothing in this package reads and
// nothing else compares against. Raise it there without raising the grace and
// the sweep starts reclaiming reservations out from under dispatches that are
// still running -- racing the very dispatch it is meant to be cleaning up
// after.
func TestPendingPromptGraceCoversTheDispatchOrdersTimeout(t *testing.T) {
	var doc struct {
		Order struct {
			Timeout string `toml:"timeout"`
		} `toml:"order"`
	}
	path := filepath.Join(repoPackDir, "orders", "gonk-dispatch.toml")
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if doc.Order.Timeout == "" {
		t.Fatalf("%s declares no [order] timeout; pendingPromptGrace's whole justification "+
			"is that Gas City kills the dispatch at one", path)
	}
	timeout, err := time.ParseDuration(doc.Order.Timeout)
	if err != nil {
		t.Fatalf("%s timeout = %q: %v", path, doc.Order.Timeout, err)
	}
	if pendingPromptGrace < timeout {
		t.Fatalf("pendingPromptGrace %v is under gonk-dispatch's order timeout %v (%s).\n"+
			"A reservation younger than that timeout may still have a live dispatch behind it, "+
			"so reclaiming it races that dispatch for the same alias.",
			pendingPromptGrace, timeout, path)
	}
}

// The ordering claim, made concrete: the pending pass runs BEFORE runReap
// inside one tick, so a reservation the sweep reclaims is already in the
// claimed set by the time the reaper builds it -- and the reaper leaves its
// session alone even though the session is old enough to qualify as an orphan.
// Get the order wrong and this tick closes the pod it just decided to settle.
func TestReclaimedPendingPromptSessionSurvivesTheSameTicksReaper(t *testing.T) {
	store := beadstore.NewMemory()
	alias := brokerSessionAlias("triage", 42, 3, 1)
	if err := store.Put(context.Background(), pendingPromptRecord(alias)); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	gc := gcapitest.New(t)
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	// Far enough out that the reservation is past pendingPromptGrace AND the
	// session is past reapGrace -- both reclaim and reap are armed this tick.
	future := time.Now().Add(24 * time.Hour)
	if code := runSweep(context.Background(), sweepDeps{
		GC: gc.Client("gonk-city"), Store: store,
		Now: func() time.Time { return future },
	}); code != 0 {
		t.Fatalf("sweep exit = %d, want 0", code)
	}

	if live := gc.LiveSessions(); len(live) != 1 || live[0] != alias {
		t.Fatalf("the reaper closed the session the same tick reclaimed: live = %v, Closed = %v",
			live, gc.Closed)
	}
}

// THE RELEASE-PATH SEMANTICS, end to end. runBrokerDispatch's deferred release
// restores the record as it stood before its reservation write. When that
// prior record is ITSELF a stranded pending-prompt reservation, the restore
// must not reset its reclaim clock: the sweep ages that record by its write
// time, and a restore that restamped it would push the reclaim out by another
// full grace window on every failed dispatch -- indefinitely, for a
// reservation whose entire problem is that nobody is coming back for it. The
// store is what guarantees this (stampWriteTime, pkg/beadstore/store.go); this
// test is the guarantee seen from the gate, with the sweep that depends on it
// actually running.
func TestAFailedDispatchDoesNotDeferReclaimOfAStrandedReservationItRestores(t *testing.T) {
	store := beadstore.NewMemory()
	gc := gcapitest.New(t)

	// A reservation for attempt 1 that has already outlived its window: its
	// dispatch was killed and never released it. The guard in runBrokerDispatch
	// lets a dispatch for a DIFFERENT attempt past it, so this record is still
	// in the store to be restored when that dispatch fails.
	stranded := pendingPromptRecord(brokerSessionAlias("triage", 42, 3, 1))
	reservedAt := time.Now().Add(-(pendingPromptGrace + time.Minute)).UTC()
	stranded.UpdatedAt = reservedAt
	if err := store.Put(context.Background(), stranded); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	if seeded, _, _ := store.Get(context.Background(), stranded.BeadAnchor); !seeded.UpdatedAt.Equal(reservedAt) {
		t.Fatalf("seed UpdatedAt = %v, want %v -- the store restamped a pending-prompt record "+
			"on write, so this test cannot stage a stranded one at all", seeded.UpdatedAt, reservedAt)
	}

	if code := runDispatch(context.Background(), failingDispatchDeps(t, store, gc, 2)); code != 1 {
		t.Fatalf("dispatch exit = %d, want 1 (it must fail: that is the whole scenario)", code)
	}

	got, ok, err := store.Get(context.Background(), stranded.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("get after failed dispatch: ok=%v err=%v", ok, err)
	}
	if !got.UpdatedAt.Equal(reservedAt) {
		t.Fatalf("UpdatedAt = %v after the release, want the reservation's own %v.\n"+
			"Restoring the prior record restamped it, which resets the sweep's reclaim clock "+
			"on a reservation that was already stranded.", got.UpdatedAt, reservedAt)
	}

	// And the consequence, asserted rather than inferred: the very next sweep
	// still reclaims it.
	if code := runSweep(context.Background(), sweepDeps{
		GC: gc.Client("gonk-city"), Store: store,
	}); code != 0 {
		t.Fatalf("sweep exit = %d, want 0", code)
	}
	after, _, _ := store.Get(context.Background(), stranded.BeadAnchor)
	if after.State != beadstore.StateRunning {
		t.Fatalf("state after the sweep = %q, want %q: a failed dispatch deferred the reclaim "+
			"of a reservation that was already past its window", after.State, beadstore.StateRunning)
	}
}

// The reclaim's own write, which is the identical pattern one function away
// from the one that shipped unstamped. Promoting out of pending-prompt is a
// real change to the record, so its write time must move -- and no line in
// reclaimPendingPrompt sets it, deliberately: the store does.
func TestReclaimingAPendingPromptRecordRestampsItsWriteTime(t *testing.T) {
	store := beadstore.NewMemory()
	alias := brokerSessionAlias("triage", 42, 3, 1)
	rec := pendingPromptRecord(alias)
	reservedAt := time.Now().Add(-(pendingPromptGrace + time.Minute)).UTC()
	rec.UpdatedAt = reservedAt
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	gc := gcapitest.New(t)
	if code := runSweep(context.Background(), sweepDeps{
		GC: gc.Client("gonk-city"), Store: store,
	}); code != 0 {
		t.Fatalf("sweep exit = %d, want 0", code)
	}

	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StateRunning {
		t.Fatalf("state = %q, want %q", got.State, beadstore.StateRunning)
	}
	if !got.UpdatedAt.After(reservedAt) {
		t.Fatalf("UpdatedAt = %v after the reclaim, want a fresh stamp (the reservation was "+
			"written at %v). A record that keeps a stale write time through a state change "+
			"is how the pending-prompt reclaim came to age records by a field nobody set.",
			got.UpdatedAt, reservedAt)
	}
}
