package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// fakeOutcomeMeter is a fake gonk-meter that answers /v1/policy/outcome (and,
// separately, the spend-sync/session-cost pair) and records every /outcome
// request it saw -- the sweeper's tests assert on the REQUEST, not an echo.
type fakeOutcomeMeter struct {
	outcomeNext string // OutcomeResponse.Next to answer with
	sessionCost meterapi.SessionCostResponse
	spendSynced bool // if true, CostSession answers sessionCost as-is; else always stale

	mu       sync.Mutex
	outcomes []meterapi.OutcomeRequest
	syncs    int
}

func (f *fakeOutcomeMeter) server(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(meterapi.OutcomePath, func(w http.ResponseWriter, r *http.Request) {
		var req meterapi.OutcomeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.outcomes = append(f.outcomes, req)
		f.mu.Unlock()
		writeJSONTest(w, meterapi.OutcomeResponse{OK: true, Next: f.outcomeNext})
	})
	mux.HandleFunc(meterapi.AdminSpendSyncPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.syncs++
		f.spendSynced = true
		f.mu.Unlock()
		writeJSONTest(w, meterapi.SpendSyncResponse{Synced: true, SpendAsOf: f.sessionCost.AsOf})
	})
	mux.HandleFunc("/v1/cost/session/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, f.sessionCost)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeOutcomeMeter) requests() []meterapi.OutcomeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]meterapi.OutcomeRequest, len(f.outcomes))
	copy(out, f.outcomes)
	return out
}

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func baseRunningRecord() beadstore.Record {
	ended := time.Now().Add(-time.Minute)
	return beadstore.Record{
		BeadAnchor: "gonk:42:issue:3", BeadID: "gk-1a2b", Project: "group/repo",
		ProjectID: 42, Rig: "group-repo", SessionKey: "gonk-42-issue-3",
		Trigger: "issue-triage", IssueIID: 3, State: beadstore.StateRunning,
		Rung: "cheap", Attempt: 1, ReservationID: "rsv-1", SessionEndedAt: ended,
	}
}

func TestSweepClassifiesSuccessAndFinishes(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	gl.AddNote(p.ID, 3, gl.Me, "<!-- gonk:bead:gk-1a2b -->", false)

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	rec.ProjectID = p.ID
	_ = store.Put(context.Background(), rec)

	fm := &fakeOutcomeMeter{outcomeNext: "done"}
	gc := gcapitest.New(t)

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(),
		Store: store, BotUsername: "gonk",
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcome requests = %+v, want one success", reqs)
	}
	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StateDone {
		t.Fatalf("state = %q, want done", got.State)
	}
	if len(gc.Poured) != 0 || len(gc.PouredNames()) != 0 {
		t.Fatal("a successful bead must not re-fire anything")
	}
}

func TestSweepEscalatesOnGateFailure(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	// no marker

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	rec.ProjectID = p.ID
	_ = store.Put(context.Background(), rec)

	fm := &fakeOutcomeMeter{
		outcomeNext: "escalate",
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 500, AsOf: time.Now()},
	}
	gc := gcapitest.New(t)

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(),
		Store: store, BotUsername: "gonk", SpendPollInterval: time.Millisecond, SpendDeadline: 50 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome requests = %+v, want one gate-failed", reqs)
	}
	names := gc.PouredNames()
	if len(names) != 1 || names[0] != dispatchOrderName {
		t.Fatalf("re-fired = %+v, want exactly one gonk-dispatch", names)
	}
}

// *** HB-4's whole point. If this ever flips, escalations become free. ***
func TestSweepDoesNotEscalateOnInfraFailure(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	// no marker, and ZERO tokens

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	rec.ProjectID = p.ID
	_ = store.Put(context.Background(), rec)

	fm := &fakeOutcomeMeter{
		outcomeNext: "retry",
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 0, AsOf: time.Now()},
	}
	gc := gcapitest.New(t)

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(),
		Store: store, BotUsername: "gonk", SpendPollInterval: time.Millisecond, SpendDeadline: 50 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %+v, want infra-failed (zero tokens proves nothing was spent)", reqs)
	}
	names := gc.PouredNames()
	if len(names) != 1 || names[0] != dispatchOrderName {
		t.Fatalf("a retry still re-fires gonk-dispatch (at the SAME rung, meter's choice): got %+v", names)
	}
}

func TestSweepTreatsStaleSpendAsInfraNotGate(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	// no marker

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	rec.ProjectID = p.ID
	_ = store.Put(context.Background(), rec)

	// spend_as_of never advances past SessionEndedAt.
	fm := &fakeOutcomeMeter{
		outcomeNext: "retry",
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 500, AsOf: rec.SessionEndedAt.Add(-time.Hour)},
	}
	gc := gcapitest.New(t)

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(),
		Store: store, BotUsername: "gonk", SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %+v, want infra-failed -- we could not PROVE the model answered", reqs)
	}
	if fm.syncs < 1 {
		t.Fatal("sweep must force a spend sync (HB-2) before trusting a token count")
	}
}

func TestSweepTreatsGitLabOutageAsInfraNotGate(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	gl := glab.New(down.URL, "tok")
	gl.RetryBackoff = func(int) time.Duration { return 0 }

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	_ = store.Put(context.Background(), rec)

	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	gc := gcapitest.New(t)

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl,
		Store: store, BotUsername: "gonk", SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %+v, want infra-failed -- an outage of OURS must never buy an escalation", reqs)
	}
}

func TestSweepUnparksOnlyWhenRetryAfterHasPassed(t *testing.T) {
	store := beadstore.NewMemory()
	now := time.Now()
	due := beadstore.Record{
		BeadAnchor: "gonk:1:issue:1", Project: "group/repo", ProjectID: 1, Rig: "group-repo",
		SessionKey: "s1", Trigger: "issue-triage", IssueIID: 1,
		State: beadstore.StateParked, RetryAfter: now.Add(-time.Minute),
	}
	notDue := beadstore.Record{
		BeadAnchor: "gonk:1:issue:2", Project: "group/repo", ProjectID: 1, Rig: "group-repo",
		SessionKey: "s2", Trigger: "issue-triage", IssueIID: 2,
		State: beadstore.StateParked, RetryAfter: now.Add(time.Hour),
	}
	_ = store.Put(context.Background(), due)
	_ = store.Put(context.Background(), notDue)

	gc := gcapitest.New(t)
	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient((&fakeOutcomeMeter{}).server(t)), GC: gc.Client("gonk-city"),
		GL: glabtest.New(t).Client(), Store: store, Now: func() time.Time { return now },
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	names := gc.PouredNames()
	if len(names) != 1 || names[0] != dispatchOrderName {
		t.Fatalf("re-fired = %+v, want exactly one gonk-dispatch (only the due bead)", names)
	}
}

func TestSweepNeverTouchesNeedsHumanBeads(t *testing.T) {
	store := beadstore.NewMemory()
	_ = store.Put(context.Background(), beadstore.Record{
		BeadAnchor: "gonk:1:issue:1", State: beadstore.StateNeedsHuman,
	})
	fm := &fakeOutcomeMeter{outcomeNext: "escalate"}
	gc := gcapitest.New(t)

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: glabtest.New(t).Client(), Store: store,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(fm.requests()) != 0 {
		t.Fatal("a needs-human bead must never be classified or spent on")
	}
	if len(gc.Poured) != 0 {
		t.Fatal("a needs-human bead must never be re-fired")
	}
}

func TestSweepBindsOutcomeToMetersReservation(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	gl.AddNote(p.ID, 3, gl.Me, "<!-- gonk:bead:gk-1a2b -->", false)

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	rec.ProjectID = p.ID
	rec.ReservationID = "rsv-from-meter"
	_ = store.Put(context.Background(), rec)

	fm := &fakeOutcomeMeter{outcomeNext: "done"}
	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gcapitest.New(t).Client("gonk-city"), GL: gl.Client(), Store: store, BotUsername: "gonk",
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].ReservationID != "rsv-from-meter" {
		t.Fatalf("outcome reservation_id = %+v, want the record's meter-minted reservation", reqs)
	}
}

func TestSweepIsIdempotentAcrossRuns(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	gl.AddNote(p.ID, 3, gl.Me, "<!-- gonk:bead:gk-1a2b -->", false)

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	rec.ProjectID = p.ID
	_ = store.Put(context.Background(), rec)

	fm := &fakeOutcomeMeter{outcomeNext: "done"}
	gc := gcapitest.New(t)
	deps := sweepDeps{Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Store: store, BotUsername: "gonk"}

	if code := runSweep(context.Background(), deps); code != 0 {
		t.Fatalf("first sweep exit = %d", code)
	}
	if code := runSweep(context.Background(), deps); code != 0 {
		t.Fatalf("second sweep exit = %d", code)
	}

	reqs := fm.requests()
	if len(reqs) != 1 {
		t.Fatalf("POST /outcome fired %d times, want exactly 1 -- a cooldown order fires every 30s "+
			"and a non-idempotent sweeper walks a project to the top of its ladder in minutes", len(reqs))
	}
	if len(gc.Poured) != 0 {
		t.Fatalf("re-fired %d times, want 0 (this bead succeeded)", len(gc.Poured))
	}
}

// The escalate/retry path is the one where non-idempotency actually costs
// money: a cooldown order fires every 30s, and a sweeper that keeps reporting
// the same finished session as gate-failed would walk a project up its ladder
// once per tick. This drives runSweep twice over the SAME store without any
// dispatch process ever actually running in between (the fake supervisor only
// records the pour) -- exactly the async gap between "order queued" and
// "order executed" that a real cooldown tick can land in.
func TestSweepIsIdempotentAcrossRunsOnEscalation(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	// no marker -> gate-failed, given tokens > 0 below

	store := beadstore.NewMemory()
	rec := baseRunningRecord()
	rec.ProjectID = p.ID
	_ = store.Put(context.Background(), rec)

	fm := &fakeOutcomeMeter{
		outcomeNext: "escalate",
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 500, AsOf: time.Now()},
	}
	gc := gcapitest.New(t)
	deps := sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Store: store,
		BotUsername: "gonk", SpendPollInterval: time.Millisecond, SpendDeadline: 50 * time.Millisecond,
	}

	if code := runSweep(context.Background(), deps); code != 0 {
		t.Fatalf("first sweep exit = %d", code)
	}
	if code := runSweep(context.Background(), deps); code != 0 {
		t.Fatalf("second sweep exit = %d", code)
	}

	reqs := fm.requests()
	if len(reqs) != 1 {
		t.Fatalf("POST /outcome fired %d times, want exactly 1 -- re-reporting the same "+
			"finished session as gate-failed every tick is a free ride up the ladder", len(reqs))
	}
	names := gc.PouredNames()
	if len(names) != 1 {
		t.Fatalf("re-fired %d times, want exactly 1", len(names))
	}
}
