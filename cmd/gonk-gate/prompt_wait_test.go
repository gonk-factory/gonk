package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/gcapi"
	"github.com/gonk-factory/gonk/pkg/gcapi/gcapitest"
)

// These tests pin the DECISION of when dispatch stops waiting for a pod to fetch
// its prompt, and what it blames when it does (gonk-alw, gonk-cw3). They drive
// the decision with a fake clock and hand-written observations, because the
// failure is a timing race against Gas City's pressure-skipped reconciler ticks
// and a sleep-based test of a race proves nothing either way.
//
// The numbers below are the production defaults on purpose: the claim being
// tested is that THOSE numbers cover what was measured on the reference cluster, not that some
// abstract budget is respected.

// fakeClock is a deterministic clock for the delivery wait: Sleep advances Now
// instantly, so a 210s wait runs in microseconds and lands on exact instants.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 13, 15, 17, 56, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}

// defaultDelivery is how long the delivery wait may run with the production
// order budget when the wait starts at process start: 210s.
func defaultDelivery() time.Duration {
	var t0 time.Time
	return orderBudget{}.withDefaults().startDeadline(t0).Sub(t0)
}

func defaultBudget() promptWaitBudget {
	return promptWaitBudget{Deadline: defaultDelivery(), FetchWindow: defaultPromptFetchWindow}
}

// THE BUG, as a decision. Gas City does not start a session when it accepts the
// create: it starts it on a reconciler tick, and under sustained FS pressure it
// skips five ticks in six (city_runtime.go tick -> shouldSkipTickForFSPressure).
// Measured on the reference cluster 2026-09-13: a real tick every 180s. A create landing just
// after one sits start-pending for ~150s. The old loop gave up at ~120s from the
// create no matter what, so it abandoned sessions nobody had started yet.
func TestWaitDoesNotChargeTheAgentForTimeTheControllerSpentNotStartingIt(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)

	// 150s start-pending, observed every 3s -- past the old 120s give-up point.
	for s := 0; s <= 150; s += 3 {
		now := t0.Add(time.Duration(s) * time.Second)
		w.observe(now, startNotAttempted, "start-pending")
		if stop, class := w.verdict(now); stop {
			t.Fatalf("gave up at %ds with class %q while the session was still start-pending "+
				"and inside the %v deadline -- this is the abandonment gonk-alw reports", s, class, defaultDelivery())
		}
	}
	// The controller finally starts it; a pod takes ~38s to fetch (measured
	// start_call 37.7s on issue 72). Still inside the deadline, so still waiting.
	started := t0.Add(153 * time.Second)
	w.observe(started, startInFlight, "creating")
	if stop, class := w.verdict(started.Add(38 * time.Second)); stop {
		t.Fatalf("gave up 38s after the controller started the session (class %q); "+
			"the pod was about to fetch", class)
	}
}

func TestWaitGivesUpOnASessionTheControllerNeverStarts(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)
	var now time.Time
	for s := 0; ; s += 3 {
		now = t0.Add(time.Duration(s) * time.Second)
		w.observe(now, startNotAttempted, "start-pending")
		if stop, class := w.verdict(now); stop {
			if class != failNoStartAttempted {
				t.Fatalf("class = %q, want %q", class, failNoStartAttempted)
			}
			break
		}
		if s > 1000 {
			t.Fatal("never gave up on a session nobody started -- the wait must be bounded")
		}
	}
	if got := now.Sub(t0); got < defaultDelivery() || got > defaultDelivery()+defaultPromptPollInterval {
		t.Fatalf("gave up after %v, want the deadline %v (within one poll)", got, defaultDelivery())
	}
	if failNoStartAttempted.agentFault() {
		t.Fatal("a session the controller never started cannot be the agent's fault")
	}
}

// The patience must not become a way to hide a genuinely broken agent: a pod
// that is up and never fetches is given the same window it always had, measured
// from the START, and the wait does not sit out the rest of the deadline.
func TestWaitGivesUpOnAStartedAgentAfterTheFetchWindowNotTheDeadline(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)
	var now time.Time
	for s := 0; s <= 1000; s += 3 {
		now = t0.Add(time.Duration(s) * time.Second)
		w.observe(now, runtimeLive, "active")
		w.fetchChecked(true)
		if stop, class := w.verdict(now); stop {
			if class != failAgentDidNotFetch {
				t.Fatalf("class = %q, want %q", class, failAgentDidNotFetch)
			}
			break
		}
	}
	if got := now.Sub(t0); got != defaultPromptFetchWindow {
		t.Fatalf("gave up after %v, want exactly the fetch window %v from the start", got, defaultPromptFetchWindow)
	}
	if !failAgentDidNotFetch.agentFault() {
		t.Fatal("a pod that ran for the whole fetch window and never fetched IS the agent side")
	}
}

// Started, but so late that the deadline arrives before the agent's own window
// has run out: that is still the controller's lateness, not the agent's fault.
func TestWaitBlamesTheControllerWhenItStartedTheSessionTooLate(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)
	w.observe(t0, startNotAttempted, "start-pending")
	w.observe(t0.Add(200*time.Second), startInFlight, "creating")
	stop, class := w.verdict(t0.Add(defaultDelivery()))
	if !stop || class != failStartedTooLate {
		t.Fatalf("verdict at deadline = (%v, %q), want (true, %q)", stop, class, failStartedTooLate)
	}
	if failStartedTooLate.agentFault() {
		t.Fatal("an agent given 40s of a 120s window was not at fault")
	}
}

func TestWaitWithNoSuccessfulObservationSaysSoRatherThanGuessing(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)
	w.observe(t0, startUnobserved, "")
	stop, class := w.verdict(t0.Add(defaultDelivery()))
	if !stop || class != failUnobservable {
		t.Fatalf("verdict = (%v, %q), want (true, %q)", stop, class, failUnobservable)
	}
	if failUnobservable.agentFault() {
		t.Fatal("gonk could not see the session; it has no grounds to blame the agent")
	}
}

// Once a start has been seen, a later failed read must not un-start it, or a
// flaky read would reset the agent's window and let a broken agent run on.
func TestWaitAFailedReadAfterTheStartDoesNotResetTheWindow(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)
	w.observe(t0, runtimeLive, "active")
	w.observe(t0.Add(60*time.Second), startUnobserved, "")
	w.observe(t0.Add(90*time.Second), startNotAttempted, "start-pending")
	w.fetchChecked(true)
	if stop, class := w.verdict(t0.Add(defaultPromptFetchWindow)); !stop || class != failAgentDidNotFetch {
		t.Fatalf("verdict = (%v, %q), want the window measured from the FIRST start", stop, class)
	}
}

// The stranded warning is what makes this visible at default verbosity: it
// fires once, at the moment the old code would have abandoned the session.
func TestWaitWarnsOnceWhenTheSessionIsStillUnstartedAtTheFetchWindow(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)
	warned := 0
	for s := 0; s < 240; s += 3 {
		now := t0.Add(time.Duration(s) * time.Second)
		w.observe(now, startNotAttempted, "start-pending")
		if w.strandedWarningDue(now) {
			warned++
			if now.Sub(t0) < defaultPromptFetchWindow {
				t.Fatalf("warned at %v, before the fetch window -- a normal slow start is not news", now.Sub(t0))
			}
		}
	}
	if warned != 1 {
		t.Fatalf("stranded warning fired %d times, want exactly once", warned)
	}
}

// The state string, not `running`, says whether the controller started the
// session. Observed live on issue 73 (2026-09-13, polling GET /session every
// 5s): start-pending/running=false until the start was enqueued, then
// creating/running=false -- and the pod FETCHED ITS PROMPT while the view still
// said creating/running=false. The old describeSessionRuntime read that as "NO
// AGENT EVER RAN".
func TestStartPhaseIsReadFromStateNotRunning(t *testing.T) {
	cases := []struct {
		view *gcapi.SessionView
		want sessionStartPhase
	}{
		{nil, startUnobserved},
		{&gcapi.SessionView{State: "start-pending"}, startNotAttempted},
		{&gcapi.SessionView{State: ""}, startNotAttempted},
		{&gcapi.SessionView{State: "start-pending", Running: true}, startNotAttempted},
		{&gcapi.SessionView{State: "creating", Running: false}, startInFlight},
		{&gcapi.SessionView{State: "creating", Running: true}, startInFlight},
		{&gcapi.SessionView{State: "active", Running: false}, runtimeLive},
		{&gcapi.SessionView{State: "awake", Running: true}, runtimeLive},
		{&gcapi.SessionView{State: "running", Running: true}, runtimeLive},
		// Upstream's other states (internal/session/manager.go): none is
		// evidence an agent ran. failed-create and asleep are where a failed
		// start rolls back to.
		{&gcapi.SessionView{State: "failed-create"}, startNotLive},
		{&gcapi.SessionView{State: "asleep"}, startNotLive},
		{&gcapi.SessionView{State: "quarantined"}, startNotLive},
		{&gcapi.SessionView{State: "drained"}, startNotLive},
		{&gcapi.SessionView{State: "archived"}, startNotLive},
		{&gcapi.SessionView{State: "some-future-state"}, startNotLive},
	}
	for _, c := range cases {
		if got := startPhaseOf(c.view); got != c.want {
			t.Errorf("startPhaseOf(%+v) = %v, want %v", c.view, got, c.want)
		}
	}
}

// A start that gc attempted and rolled back -- creating, then failed-create --
// never produced a runtime the controller confirmed. gonk cannot tell a pod that
// never ran from one that ran broken, so it must not book this as the agent's
// fault (the reviewer's finding against the first cut, which did).
func TestWaitDoesNotBlameTheAgentForAStartThatNeverWentLive(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)
	w.observe(t0, startInFlight, "creating")
	w.observe(t0.Add(61*time.Second), startNotLive, "failed-create")
	w.fetchChecked(true)
	stop, class := w.verdict(t0.Add(defaultPromptFetchWindow))
	if !stop || class != failStartNotConfirmed {
		t.Fatalf("verdict = (%v, %q), want (true, %q)", stop, class, failStartNotConfirmed)
	}
	if failStartNotConfirmed.agentFault() {
		t.Fatal("a start the controller never confirmed live is not the agent's fault")
	}
}

// If the meter could not be asked at the end of the agent's window, whether the
// pod fetched is unknown -- the agent must not be blamed for gonk's blindness.
func TestWaitDoesNotBlameTheAgentWhenTheMeterCouldNotBeAsked(t *testing.T) {
	t0 := newFakeClock().Now()
	w := newPromptWait(defaultBudget(), t0)
	w.observe(t0, runtimeLive, "active")
	w.fetchChecked(true)
	w.fetchChecked(false)
	stop, class := w.verdict(t0.Add(defaultPromptFetchWindow))
	if !stop || class != failPromptStatusUnobservable {
		t.Fatalf("verdict = (%v, %q), want (true, %q)", stop, class, failPromptStatusUnobservable)
	}
	if failPromptStatusUnobservable.agentFault() {
		t.Fatal("an unanswered status check is not evidence against the agent")
	}
}

// The gcapitest NotRunningUntil knob must report upstream's hyphenated state. It
// used to emit "start_pending", which startPhaseOf reads as an unknown, NOT-LIVE
// state -- a trap for the next test that reached for it.
func TestFakeNotRunningUntilReportsAStartPendingState(t *testing.T) {
	gc := gcapitest.New(t)
	gc.RunSession("s1", "")
	gc.NotRunningUntil = map[string]int{"s1": 1}
	view, err := gc.Client("gonk-city").GetSessionOutput(context.Background(), "s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := startPhaseOf(view); got != startNotAttempted {
		t.Fatalf("NotRunningUntil view state %q reads as %v, want %v", view.State, got, startNotAttempted)
	}
}
