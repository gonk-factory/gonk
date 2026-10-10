package main

// UNREFERENCED FROM DISPATCH since gonk-t55: the Jobs runtime mounts the
// prompt, so there is no fetch to wait for. Left, with its tests, for T-56 to
// delete with the rest of the Gas City surface.

import (
	"fmt"
	"time"

	"github.com/gonk-factory/gonk/pkg/gcapi"
)

// THE DELIVERY WAIT, AND WHO IT BLAMES (gonk-alw, gonk-cw3).
//
// After an accepted create, dispatch waits for the agent pod to fetch its
// prompt. It used to wait a fixed 40 checks x 3s from the create and then
// abandon the session with "prompt was never fetched" -- regardless of whether
// anything had tried to start a pod.
//
// That is wrong under the runtime gonk actually has. Gas City does not start a
// session when it accepts the create. GASCITY_REF
// internal/api/huma_handlers_sessions_command.go creates the session bead
// start-pending and Pokes the reconciler; the start is enqueued only inside
// CityRuntime.tick (cmd/gc/session_lifecycle_parallel.go, "outcome=
// start_enqueued"), and tick() skips itself -- poke ticks included -- whenever
// /proc/pressure/io some avg60 exceeds 50, forcing one tick only after five
// skips (cmd/gc/fs_pressure.go). The patrol interval is 30s. On a loaded worker node, where
// every gonk pod runs and avg60 sits at 60-98, the controller log shows a real
// tick every 180s. A create that lands just after one waits ~150s before any
// start is attempted, plus the tick body (~20s) and the pod start (4-38s). The
// old window ended at ~123s. Issue 66 (2026-09-07) was abandoned 23s before its
// pod started; issues !44 and #69 were abandoned before any pod was created.
//
// So the wait now has two budgets:
//
//   - Deadline: the hard cap on the whole wait. It is the order budget's
//     delivery deadline (order_budget.go): fixed from PROCESS START so that the
//     session close and the release write after it still finish inside the
//     300s order timeout -- 210s after process start with the defaults. (The
//     first cut used a 240s deadline from the start of the wait, which on the
//     live pre-wait times ran ~303-318s against the 300s timeout.)
//   - FetchWindow: how long the AGENT gets, measured from the moment the
//     controller is first seen to have started the session -- not from the
//     create. Time the controller spends not starting a session is not the
//     agent's to spend.
//
// and, when it gives up, it says whose failure it was, from the session's
// lifecycle STATE -- not from `running`, which was read as false one second
// before the pod fetched (see startPhaseOf). Only a runtime the controller
// confirmed live, with the meter answering "not fetched" at the end of the
// agent's window, is booked as the agent's fault.
//
// This logic is deliberately expressed over a small lifecycle observation rather
// than over gcapi types, so T-55's SessionRuntime.Status can drive it unchanged.
// Only startPhaseOf is Gas City specific, and it goes with T-56.

const (
	// defaultPromptFetchWindow is the AGENT's patience: 120s from the observed
	// start, the same 40 x 3s the old loop allowed from the create. A pod
	// measured on the reference cluster fetches 5-38s after the start is enqueued.
	defaultPromptFetchWindow = 120 * time.Second
	// defaultPromptPollInterval is flat, not exponential: the wait is for a
	// roughly fixed latency, not a struggling server to back off from.
	defaultPromptPollInterval = 3 * time.Second
	// defaultPromptRequestTimeout bounds ONE read of the meter or the session
	// inside the wait. The controller's own reads were measured taking 12-57s at
	// the worst of the 2026-09-13 pressure, so it is generous; the deadline, not
	// this, is what bounds the wait.
	defaultPromptRequestTimeout = 10 * time.Second //nolint:unused // unreferenced since gonk-t55 (plan D7); T-56 deletes this file
	// promptRequestTimeoutFloor is the least a read is given even when the
	// deadline is (nearly) spent, so the final poll can still ask. Two reads per
	// poll make 2x this the most the wait can overshoot its deadline.
	promptRequestTimeoutFloor = 2 * time.Second //nolint:unused // unreferenced since gonk-t55 (plan D7); T-56 deletes this file
)

// sessionStartPhase is what dispatch can tell, from one read, about whether the
// runtime has tried to start a session and whether it confirmed one live.
type sessionStartPhase int

const (
	// startUnobserved: the session could not be read at all.
	startUnobserved sessionStartPhase = iota
	// startNotAttempted: accepted, but the controller has not attempted a
	// start. No pod exists and none is being created.
	startNotAttempted
	// startInFlight: the provider Start call is in flight (upstream's own
	// definition of `creating`). A pod exists or is being created; nothing has
	// confirmed it alive yet.
	startInFlight
	// runtimeLive: the controller has confirmed a live runtime.
	runtimeLive
	// startNotLive: any other state -- a start that was attempted and ended
	// without a live runtime (failed-create, asleep after a rollback,
	// quarantined, drained, ...), or a state this code does not know. None of
	// them is evidence that an agent ran.
	startNotLive
)

func (p sessionStartPhase) String() string {
	switch p {
	case startNotAttempted:
		return "not-attempted"
	case startInFlight:
		return "in-flight"
	case runtimeLive:
		return "live"
	case startNotLive:
		return "not-live"
	default:
		return "unobserved"
	}
}

// startPhaseOf reads Gas City's session view. ONLY `state` decides it, against
// upstream's own definitions (GASCITY_REF internal/session/manager.go):
// start-pending "no provider Start call is currently in flight"; creating "the
// provider Start call is in flight and the runtime process has not yet been
// confirmed alive"; active/awake "a live runtime session".
//
// `running` is not used. Observed live on project 75 issue 73, polling
// GET /v0/city/gonk/session/<alias> every 5s:
//
//	15:18:19..15:19:15  state=start-pending running=False
//	15:19:20..15:19:25  state=creating      running=False
//	15:19:26            prompt fetched by the agent pod
//
// The pod fetched one second after a read that still said creating/running=
// false -- so a pod can be up and working while `running` says false, and the
// old running-based diagnosis would have called it "NO AGENT EVER RAN".
//
// The empty state is treated as not attempted because upstream's
// WaitForSessionCommandable (cmd/gc/api_state.go) groups "" with start-pending
// and creating as not yet commandable. "running" is accepted as live because
// the gcapitest fake, and broker_apply.go, use it.
func startPhaseOf(view *gcapi.SessionView) sessionStartPhase {
	if view == nil {
		return startUnobserved
	}
	switch view.State {
	case "start-pending", "":
		return startNotAttempted
	case "creating":
		return startInFlight
	case "active", "awake", "running":
		return runtimeLive
	default:
		return startNotLive
	}
}

// promptWaitFailure names why the wait ended without a fetch.
type promptWaitFailure string

const (
	// failNoStartAttempted: every read said the controller had not started the
	// session, all the way to the deadline. No pod was ever created.
	failNoStartAttempted promptWaitFailure = "start_never_attempted"
	// failStartedTooLate: the controller started the session, but so late that
	// the deadline arrived before the agent's fetch window had run.
	failStartedTooLate promptWaitFailure = "start_attempted_late"
	// failStartNotConfirmed: a start was attempted and the window ran out, but
	// the controller never confirmed a live runtime (stuck creating, or rolled
	// back to failed-create/asleep). gonk cannot tell a pod that never ran from
	// one that ran broken, so this is NOT booked as the agent's fault.
	failStartNotConfirmed promptWaitFailure = "start_not_confirmed"
	// failAgentDidNotFetch: the controller confirmed a live runtime, the agent
	// had its full fetch window after the start, and the meter ANSWERED "not
	// fetched" at the end of it. The one failure that is the agent side's.
	failAgentDidNotFetch promptWaitFailure = "agent_did_not_fetch"
	// failPromptStatusUnobservable: as above, except the meter could not be
	// asked at the end -- so whether the pod fetched is unknown.
	failPromptStatusUnobservable promptWaitFailure = "prompt_status_unobservable"
	// failUnobservable: no read of the session ever succeeded, so there are no
	// grounds to blame anyone in particular.
	failUnobservable promptWaitFailure = "session_unobservable"
)

// agentFault reports whether this failure is the agent pod's rather than the
// controller's, or gonk's own inability to see what happened.
//
// It is DIAGNOSTIC ONLY. Nothing consumes it beyond the abandon log line: every
// class exits 1 and restores the prior record identically, so a wrong answer
// here misleads an operator but does not change what happens to the bead or its
// ladder. Keep it honest for that reader; do not read it as protecting anything.
func (f promptWaitFailure) agentFault() bool { return f == failAgentDidNotFetch }

// abandonMessage is the ERROR line dispatch logs when it gives up. It names the
// failure in words, because the old single message -- "prompt was never
// fetched" -- read as an agent fault in exactly the cases where no agent existed.
func (f promptWaitFailure) abandonMessage() string { //nolint:unused // unreferenced since gonk-t55 (plan D7); T-56 deletes this file
	switch f {
	case failNoStartAttempted:
		return "no agent pod was ever started for this session: the controller accepted it " +
			"but never attempted a start before the delivery deadline -- NOT an agent fault; " +
			"abandoning, re-sling will retry"
	case failStartedTooLate:
		return "the controller started the agent pod too late for it to fetch its prompt " +
			"before the delivery deadline -- NOT an agent fault; abandoning, re-sling will retry"
	case failStartNotConfirmed:
		return "the controller attempted a start but never confirmed a live agent runtime " +
			"within the fetch window -- not booked as an agent fault; abandoning, re-sling will retry"
	case failAgentDidNotFetch:
		return "prompt was never fetched: the agent runtime was live and did not fetch its " +
			"prompt within its window; session will idle -- re-sling will retry"
	case failPromptStatusUnobservable:
		return "prompt fetch status could not be read from the meter at the end of the agent's " +
			"window, so whether the pod fetched is unknown; abandoning, re-sling will retry"
	default:
		return "prompt was never fetched and the session's state could not be read, so whether " +
			"a pod ever started is unknown; abandoning, re-sling will retry"
	}
}

type promptWaitBudget struct {
	Deadline    time.Duration
	FetchWindow time.Duration
}

// promptWait is the pure decision: fed observations and instants, it says when
// to stop and why. It holds no clock and does no I/O, so it is testable
// exactly, at the instants that matter.
type promptWait struct {
	budget     promptWaitBudget
	begun      time.Time
	observed   bool
	startedAt  time.Time // first instant a start was observed; zero until then
	everLive   bool
	lastState  string
	meterOK    bool // whether the most recent prompt-status check got an answer
	meterAsked bool
	warned     bool
}

// newPromptWait starts the clock. begun is the start of the wait, which is the
// create's 202 plus awaitCreate's short check -- not the create itself, and not
// process start (the deadline carries that).
func newPromptWait(b promptWaitBudget, begun time.Time) *promptWait {
	return &promptWait{budget: b, begun: begun}
}

// observe records one read of the session. It returns true exactly once: on the
// first observation that the controller has attempted a start.
//
// A start, once seen, is never unseen, and neither is a live runtime -- a later
// failed or stale read must not reset the agent's window, or a flaky read would
// let a broken agent run on.
func (w *promptWait) observe(now time.Time, phase sessionStartPhase, state string) (firstStart bool) {
	if phase == startUnobserved {
		return false
	}
	w.observed = true
	w.lastState = state
	if phase == runtimeLive {
		w.everLive = true
	}
	if phase != startNotAttempted && w.startedAt.IsZero() {
		w.startedAt = now
		return true
	}
	return false
}

// fetchChecked records whether the latest prompt-status check got an answer
// from the meter (a "not fetched" answer counts; a transport error does not).
func (w *promptWait) fetchChecked(answered bool) {
	w.meterAsked = true
	w.meterOK = answered
}

// verdict reports whether to stop waiting at now, and if so, why.
func (w *promptWait) verdict(now time.Time) (bool, promptWaitFailure) {
	if !w.startedAt.IsZero() && now.Sub(w.startedAt) >= w.budget.FetchWindow {
		switch {
		case !w.everLive:
			return true, failStartNotConfirmed
		case !w.meterAsked || !w.meterOK:
			return true, failPromptStatusUnobservable
		default:
			return true, failAgentDidNotFetch
		}
	}
	if now.Sub(w.begun) < w.budget.Deadline {
		return false, ""
	}
	switch {
	case !w.observed:
		return true, failUnobservable
	case w.startedAt.IsZero():
		return true, failNoStartAttempted
	default:
		return true, failStartedTooLate
	}
}

// strandedWarningDue reports, once, that the session is still not started at
// the point the old fixed window would have abandoned it. That is the moment a
// slow start stops being routine and becomes worth an operator's attention.
// It needs at least one successful read: a session gonk cannot see is not known
// to be unstarted, and saying so would be the same guess this file removes.
func (w *promptWait) strandedWarningDue(now time.Time) bool {
	if w.warned || !w.observed || !w.startedAt.IsZero() || now.Sub(w.begun) < w.budget.FetchWindow {
		return false
	}
	w.warned = true
	return true
}

// startedAfter is how long the session sat before a start was observed, or -1
// when none was.
func (w *promptWait) startedAfter() time.Duration { //nolint:unused // unreferenced since gonk-t55 (plan D7); T-56 deletes this file
	if w.startedAt.IsZero() {
		return -1
	}
	return w.startedAt.Sub(w.begun)
}

// promptNotFetchedError is awaitPromptFetched's give-up, carrying the
// classification so the caller can log it as fields rather than parse a string.
type promptNotFetchedError struct { //nolint:unused // unreferenced since gonk-t55 (plan D7); T-56 deletes this file
	Class        promptWaitFailure
	Waited       time.Duration
	StartedAfter time.Duration // -1: no start was ever observed
	LastState    string
	LastErr      error
}

func (e *promptNotFetchedError) Error() string { //nolint:unused // unreferenced since gonk-t55 (plan D7); T-56 deletes this file
	started := "no start was ever observed"
	if e.StartedAfter >= 0 {
		started = fmt.Sprintf("start observed %s into the wait", e.StartedAfter.Round(time.Second))
	}
	return fmt.Sprintf("prompt not fetched after %s (%s; %s; last session state %q): %v",
		e.Waited.Round(time.Second), e.Class, started, e.LastState, e.LastErr)
}

func (e *promptNotFetchedError) Unwrap() error { return e.LastErr } //nolint:unused // unreferenced since gonk-t55 (plan D7); T-56 deletes this file
