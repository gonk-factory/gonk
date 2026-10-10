package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// gonk-6n8. TWO dispatches for ONE bead+attempt must not create two sessions.
//
// MEASURED IN PRODUCTION 2026-08-18: go-93gk at 23:11:07Z and go-s4ug at
// 23:15:36Z, 4m29s apart, both carrying the alias `gonk.triage.p75.i24.a1`.
// Not a race -- a re-dispatch, at an interval no lock would have covered. The
// meter is idempotent here BY DESIGN (an open, unsettled reservation returns
// the same attempt number), so the second dispatch computed the same session
// and, under Gas City, created a second one under it (gonk-u6p wedged
// gonk:75:issue:24 for eight days).
//
// Under the SessionRuntime there are TWO guards, and each is tested alone:
// the record's reservation (this test: the second dispatch never even calls
// Start) and the deterministic name (TestAStartThatFindsTheSessionAlready...:
// a Start that does happen lands on the same session).
func TestASecondDispatchForTheSameAttemptCreatesNoSecondSession(t *testing.T) {
	rt := newTestRuntime()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	forge := stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}}
	store := beadstore.NewMemory()
	meter := meterClient(fm.server(t))

	deps := func() dispatchDeps {
		return dispatchDeps{
			Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
			Meter: meter, Runtime: rt, Store: store,
			Forge: forge, Args: baseDispatchArgs(),
		}
	}

	if code := runDispatch(context.Background(), deps()); code != 0 {
		t.Fatalf("first dispatch exit = %d, want 0", code)
	}
	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("first dispatch started %d sessions, want 1", n)
	}

	// The re-dispatch. Same bead, same attempt -- exactly what the meter returns
	// while the reservation is open and unsettled.
	if code := runDispatch(context.Background(), deps()); code != 0 {
		t.Fatalf("second dispatch exit = %d, want 0 (a duplicate is a no-op, not a failure)", code)
	}
	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("second dispatch called Start again (%d starts): the record guard did not hold", n)
	}
	if live := rt.live(t); len(live) != 1 || live[0] != runtime.Handle(testSession) {
		t.Fatalf("live = %v, want exactly [%s]", live, testSession)
	}
}

// PLAN P3 ITEM 4: a Start that finds the session ALREADY EXISTS makes no
// second session and no second record.
//
// The record guard above cannot see this case: the bead's record no longer
// names the session. That is what a failed dispatch's release produces when
// its session was admitted and the stop after it failed -- or a record put
// back by any other writer -- and under Gas City it was the gonk-u6p duplicate.
// Under the SessionRuntime the name is the idempotency key: the second Start
// reaches the SAME session, unchanged (its prompt is the first dispatch's),
// and dispatch records that one session in the bead's one record.
func TestAStartThatFindsTheSessionAlreadyThereMakesNoSecondSessionAndNoSecondRecord(t *testing.T) {
	rt := newTestRuntime()
	store := beadstore.NewMemory()
	// The session a lost dispatch admitted, still running, with a prompt of its
	// own. No record names it.
	if _, _, err := rt.Runtime.Start(context.Background(), runtime.Spec{
		Name: testSession, Agent: "triage", Prompt: []byte("the FIRST dispatch's prompt"),
	}); err != nil {
		t.Fatal(err)
	}
	rt.SetPhase(runtime.Handle(testSession), runtime.PhaseRunning, "")

	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	if code := runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Forge: promptTestForge(), Args: baseDispatchArgs(),
	}); code != 0 {
		t.Fatalf("exit = %d, want 0 -- an existing session is the idempotent answer, not a failure", code)
	}

	starts := rt.Starts()
	if len(starts) != 1 || starts[0].Name != testSession {
		t.Fatalf("dispatch starts = %+v, want exactly one Start under %s", starts, testSession)
	}
	if live := rt.live(t); len(live) != 1 {
		t.Fatalf("live = %v, want the ONE existing session", live)
	}
	spec, _ := rt.Spec(runtime.Handle(testSession))
	if string(spec.Prompt) != "the FIRST dispatch's prompt" {
		t.Fatalf("the existing session was replaced or changed: prompt now %q", spec.Prompt)
	}
	running, err := store.List(context.Background(), beadstore.StateRunning)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].SessionID != testSession || running[0].BeadAnchor != baseDispatchArgs().BeadAnchor {
		t.Fatalf("running records = %+v, want exactly one, naming %s", running, testSession)
	}
	pending, err := store.List(context.Background(), beadstore.StatePendingPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending-prompt records left behind: %+v", pending)
	}
}

// The guard must key on the ATTEMPT, not merely on "a session exists". A
// re-sling is a NEW attempt and legitimately needs its own session -- refusing
// there would strand every retried bead, which is a worse failure than the one
// being fixed.
func TestARealReSlingOnTheNextAttemptStillGetsItsOwnSession(t *testing.T) {
	rt := newTestRuntime()
	forge := stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}}
	store := beadstore.NewMemory()

	for attempt := 1; attempt <= 2; attempt++ {
		fm := &fakeMeter{resp: meterapi.DecideResponse{
			KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
			Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: attempt, ReservationID: "rsv",
		}}
		if code := runDispatch(context.Background(), dispatchDeps{
			Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
			Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
			Forge: forge, Args: baseDispatchArgs(),
		}); code != 0 {
			t.Fatalf("attempt %d exit = %d", attempt, code)
		}
	}

	live := rt.live(t)
	if len(live) != 2 {
		t.Fatalf("live sessions across two attempts = %v, want 2 -- a re-sling that "+
			"cannot open a session leaves the bead unable to progress", live)
	}
	if live[0] != "gonk-triage-p42-i3-a1" || live[1] != "gonk-triage-p42-i3-a2" {
		t.Fatalf("live = %v, want one session per attempt, each named for it", live)
	}
}

// gonk-6n8 / T-13. The first test above proves the record guard once the
// first dispatch has returned. gonk-u6p's second dispatch landed while the
// FIRST was still in flight, long before its final write. This proves the
// guard closes THAT gap too: a re-dispatch arriving while the first is between
// its reservation and its running record must see the reservation (State
// pending-prompt, SessionID already set) and back off, without calling Start.
func TestASecondDispatchWhileTheFirstIsStartingCallsNoSecondStart(t *testing.T) {
	rt := newTestRuntime()
	store := beadstore.NewMemory()
	forge := stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}}
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	meter := meterClient(fm.server(t))
	deps := dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meter, Runtime: rt, Store: store,
		Forge: forge, Args: baseDispatchArgs(),
		// No wall-clock budget can run out while the first dispatch is held.
		Budget: testBudget(time.Hour), Clock: newFakeClock(),
	}

	// SIGNAL AND HOLD, never a sleep (docs/testing-time.md rule 2): the first
	// Start signals and blocks -- after the first dispatch's reservation, before
	// its session exists -- until the second dispatch has returned.
	inStart := make(chan struct{})
	release := make(chan struct{})
	var entered, released sync.Once
	releaseFirst := func() { released.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)
	rt.onStart = func(runtime.Spec) error {
		first := false
		entered.Do(func() { first = true; close(inStart) })
		if first {
			<-release
		}
		return nil
	}

	firstCode := make(chan int, 1)
	go func() { firstCode <- runDispatch(context.Background(), deps) }()
	select {
	case <-inStart:
	case code := <-firstCode:
		t.Fatalf("the first dispatch returned %d before it ever reached Start", code)
	case <-time.After(30 * time.Second): // hang guard, not a budget
		t.Fatal("hang guard: the first dispatch never reached Start in 30s")
	}

	if code := runDispatch(context.Background(), deps); code != 0 {
		t.Fatalf("second dispatch (mid-start) exit = %d, want 0 (a duplicate is a no-op, not a failure)", code)
	}
	// The fake records a Start only once its hook returns, so the held first
	// Start is not in this count: any Start here is the second dispatch's.
	if n := len(rt.Starts()); n != 0 {
		t.Fatalf("the second dispatch called Start (%d recorded) while the first held its reservation", n)
	}
	releaseFirst()
	if got := <-firstCode; got != 0 {
		t.Fatalf("first dispatch exit = %d, want 0", got)
	}
	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("starts after both dispatches = %d, want exactly 1", n)
	}
}

// PLAN 3.4 C2: ADOPT, THEN A FAILED RECORD WRITE, MUST NOT STOP ANOTHER
// DISPATCH'S SESSION.
//
// The interleaving the verifier found (risk 5): dispatch A reserved, started
// the session and recorded it. Dispatch B, for the same bead and attempt, got
// past the reservation guard on the path that deliberately does not fail
// closed -- the store could not be read, and its PutIf against "" lost to A's
// record, so B proceeds WITHOUT a reservation (gonk-u6p) -- and its Start
// ADOPTED A's session under the deterministic name. B's running-record write
// then fails. Stopping "its" session there would stop A's: the sweep would
// find A's recorded session absent and charge the attempt. Only a session this
// dispatch CREATED is its to abandon.
func TestADispatchThatAdoptedASessionDoesNotStopItWhenItsRecordWriteFails(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	mem := beadstore.NewMemory()

	// Dispatch A's work: the session it created, running, and its record.
	if _, _, err := rt.Runtime.Start(ctx, runtime.Spec{Name: testSession, Agent: "triage", Prompt: []byte("A's prompt")}); err != nil {
		t.Fatal(err)
	}
	rt.SetPhase(runtime.Handle(testSession), runtime.PhaseRunning, "")
	aRec := beadstore.Record{
		BeadAnchor: baseDispatchArgs().BeadAnchor, Trigger: baseDispatchArgs().Trigger,
		ProjectID: baseDispatchArgs().ProjectID, IssueIID: baseDispatchArgs().IssueIID,
		State: beadstore.StateRunning, SessionID: testSession, Attempt: 1,
		Rung: "cheap", Model: "m", ReservationID: "rsv-1",
	}
	if err := mem.Put(ctx, aRec); err != nil {
		t.Fatal(err)
	}

	var logged strings.Builder
	deps := startDeps(t, rt, runMeter(), &logged)
	deps.Store = &unreadableUnwritableStore{Store: mem}

	if code := runDispatch(ctx, deps); code != 1 {
		t.Fatalf("exit = %d, want 1 (the running-record write failed); log:\n%s", code, logged.String())
	}
	if starts := rt.Starts(); len(starts) != 1 {
		t.Fatalf("dispatch B's starts = %d, want 1 (it must reach Start and adopt, or this test proves nothing); log:\n%s",
			len(starts), logged.String())
	}
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("dispatch B stopped %d session(s) it ADOPTED -- that is dispatch A's recorded session; log:\n%s", n, logged.String())
	}
	if live := rt.live(t); len(live) != 1 || live[0] != runtime.Handle(testSession) {
		t.Fatalf("live = %v, want A's session still running", live)
	}
	got, ok, err := mem.Get(ctx, aRec.BeadAnchor)
	if err != nil || !ok || got.State != beadstore.StateRunning || got.SessionID != testSession {
		t.Fatalf("A's record = %+v (ok=%v err=%v), want it untouched", got, ok, err)
	}
	if !strings.Contains(logged.String(), "this dispatch ADOPTED it") {
		t.Fatalf("the adopt-and-leave decision was not logged:\n%s", logged.String())
	}
}

// unreadableUnwritableStore: Get fails (the unreadable-store dispatch path)
// and Put fails (the running-record write). PutIf reaches the real store, so
// a reservation against "" loses to an existing record, as it does in bd.
type unreadableUnwritableStore struct{ beadstore.Store }

func (s *unreadableUnwritableStore) Get(context.Context, string) (beadstore.Record, bool, error) {
	return beadstore.Record{}, false, errors.New("bd: store unreadable")
}

func (s *unreadableUnwritableStore) Put(context.Context, beadstore.Record) error {
	return errors.New("bd: write failed")
}
