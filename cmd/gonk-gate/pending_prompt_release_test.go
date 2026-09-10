package main

import (
	"context"
	"strings"
	"testing"
	"time"

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
	// beadstore.Memory stamps UpdatedAt on Put, so the zero-time case has to be
	// staged through a store that does not.
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
// test can stage a record whose write time is genuinely unknown (which is what
// beadstore.BdCLI would produce for a caller that never set one).
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
	if pendingPromptGrace < defaultSubmitDeadline {
		t.Fatalf("pendingPromptGrace %v is shorter than defaultSubmitDeadline %v: the sweep "+
			"would reclaim reservations out from under healthy dispatches",
			pendingPromptGrace, defaultSubmitDeadline)
	}
	if pendingPromptGrace >= reapGrace {
		t.Fatalf("pendingPromptGrace %v must be comfortably under reapGrace %v, or a "+
			"pending-prompt session can be reaped before the sweep ever claims it",
			pendingPromptGrace, reapGrace)
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
