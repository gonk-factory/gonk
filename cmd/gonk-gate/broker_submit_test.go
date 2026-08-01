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

// The prompt is delivered by a SECOND signed call (POST .../session/{id}/submit),
// not as create-time template_overrides.initial_message, because Gas City's k8s
// runtime provider never composes runtime.Config.PromptSuffix onto the launch
// command -- only tmux/acp/herdr/t3bridge do. A pod-backed session created with
// Message set boots at opencode's idle splash with no prompt at all, never
// finishes, and so is never swept. See gonk-u1p.1 / upstream gonk-drf.

func runDispatchForSubmit(t *testing.T, gc *gcapitest.Server) int {
	t.Helper()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	forge := stubForge{iss: &glab.Issue{
		IID: 3, Title: "Login button does nothing on Safari", State: "opened",
		Labels:      []string{"needs-triage"},
		Description: "Steps: click login in Safari 17. Nothing happens.",
	}}
	return runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: forge, Args: baseDispatchArgs(),
		SubmitAttempts: 3, SubmitBackoff: zeroBackoff,
	})
}

func TestDispatchDeliversPromptViaSubmitNotCreate(t *testing.T) {
	gc := gcapitest.New(t)
	if code := runDispatchForSubmit(t, gc); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("created = %+v, want exactly one session", gc.Created)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %+v, want exactly one prompt", gc.Submitted)
	}
	// The prompt must ride the submit...
	msg := gc.Submitted[0].Message
	for _, want := range []string{
		"fetched for you",
		"Login button does nothing on Safari", // title
		"needs-triage",                        // current label
		"GONK_BATCH_START",                    // the proposed-effects contract
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("submitted prompt missing %q:\n%s", want, msg)
		}
	}
	// ...and NOT the create. Setting both would double-deliver the moment the
	// upstream k8s provider learns to honour initial_message (gonk-drf).
	if got := gc.Created[0].Message; got != "" {
		t.Errorf("create carried a message %q, want empty -- the prompt belongs on submit only", got)
	}
	// It must target the alias we stamped, which is the correlation handle.
	if got, want := gc.Submitted[0].ID, gc.Created[0].Alias; got != want {
		t.Errorf("submitted to %q, want the created alias %q", got, want)
	}
	if got := gc.Submitted[0].Intent; got != "default" {
		t.Errorf("intent = %q, want default for a first prompt", got)
	}
}

// THE REGRESSION TEST FOR gonk-u1p.7.
//
// Agent-kind create is always-async upstream: it returns 202 with no session id
// and spawns in the background, so the session does NOT exist for the first
// submits. Upstream does not report that with a 404 -- the submit route answers
// 202 BEFORE it resolves anything -- it reports it as a request.failed event
// with error_code=resolve_failed.
//
// Dispatch must therefore read the OUTCOME, not the status code. A version that
// trusts the 202 passes every other test in this file and drops the prompt of
// every real agent, because the first "success" here is a lie.
func TestDispatchRetriesSubmitUntilSessionMaterializes(t *testing.T) {
	gc := gcapitest.New(t)
	gc.SubmitUnresolvedUntil = map[string]int{brokerSessionAlias(42, 3, 1): 2}

	if code := runDispatchForSubmit(t, gc); code != 0 {
		t.Fatalf("exit = %d, want 0 (the resolve_failed outcomes are the async-create window, not a failure)", code)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %d, want exactly one accepted prompt after the retries", len(gc.Submitted))
	}
	if gc.SubmitAttemptsSeen != 3 {
		t.Fatalf("submit attempted %d times, want 3 -- two dropped, then one that landed. "+
			"Fewer means dispatch believed a 202 that delivered nothing", gc.SubmitAttemptsSeen)
	}
}

// The other retryable outcome: the session resolved but its pod is not live
// yet, which upstream reports as submit_failed carrying ErrSessionInactive's
// text. Same window, different half of it.
func TestDispatchRetriesWhileTheSessionIsNotLiveYet(t *testing.T) {
	gc := gcapitest.New(t)
	gc.SubmitInactiveUntil = map[string]int{brokerSessionAlias(42, 3, 1): 1}

	if code := runDispatchForSubmit(t, gc); code != 0 {
		t.Fatalf("exit = %d, want 0 (an inactive session is a pod still starting)", code)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %d, want exactly one accepted prompt", len(gc.Submitted))
	}
}

// If the prompt never lands, the run has genuinely failed: the session would sit
// idle forever and never be swept. Return infra-failure so the existing re-sling
// machinery retries with a fresh attempt-suffixed alias, rather than silently
// reporting success and stranding an idle session (which is exactly the bug this
// whole change exists to fix).
func TestDispatchFailsWhenPromptNeverDelivers(t *testing.T) {
	gc := gcapitest.New(t)
	gc.SubmitUnresolvedUntil = map[string]int{brokerSessionAlias(42, 3, 1): 99}

	if code := runDispatchForSubmit(t, gc); code != 1 {
		t.Fatalf("exit = %d, want 1 (infra) when the prompt could not be delivered", code)
	}
	if len(gc.Submitted) != 0 {
		t.Fatalf("submitted = %+v, want none accepted", gc.Submitted)
	}
}

// zeroBackoff keeps the retry path instant in tests: the delay models pod-start
// latency, which a fake supervisor does not have.
func zeroBackoff(int) time.Duration { return 0 }

// A prompt must not be fired into a session whose runtime is not up yet.
//
// Upstream parks a default-intent submit on the nudge queue when the session is
// still start_pending, answering ok with queued=true. PROVEN LIVE 2026-08-01:
// that parked prompt NEVER arrived -- session go-57b took one, its pod came up
// healthy, and opencode sat at the idle splash indefinitely. So dispatch waits
// for running=true rather than accepting a queued receipt.
//
// The queued=true assertion is the real one here: a version that submits
// immediately still exits 0, because upstream calls parking a success.
func TestDispatchWaitsForTheRuntimeBeforeSubmitting(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias(42, 3, 1)
	gc.NotRunningUntil = map[string]int{alias: 2}

	if code := runDispatchForSubmit(t, gc); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %d, want exactly one prompt", len(gc.Submitted))
	}
	if gc.SubmitAttemptsSeen != 1 {
		t.Fatalf("submit attempted %d times, want 1 -- the prompt must be HELD BACK until the "+
			"runtime is up, not fired into a start_pending session and parked", gc.SubmitAttemptsSeen)
	}
}

// CREATE is async in the handler too, so its 202 is not a receipt either. A
// create that fails outright -- a taken alias, an unknown agent -- is still
// answered 202 and reports the failure only as request.failed/create_failed.
//
// Observed live 2026-08-01: a colliding alias did exactly that while dispatch
// logged "triage session created" and went on to submit a prompt into a session
// it had not created. Dispatch must fail here, and must not go on to submit.
func TestDispatchFailsWhenTheCreateSilentlyFailed(t *testing.T) {
	gc := gcapitest.New(t)
	gc.CreateFailWith = `session alias already exists: "gonk.triage.p42.i3.a1" already belongs to go-d3y`

	if code := runDispatchForSubmit(t, gc); code != 1 {
		t.Fatalf("exit = %d, want 1 (infra): the session was never created", code)
	}
	if gc.SubmitAttemptsSeen != 0 {
		t.Fatalf("submit attempted %d times, want 0 -- there is no session to submit to",
			gc.SubmitAttemptsSeen)
	}
}

// A create whose success event has not arrived yet is the NORMAL case, not a
// failure: upstream emits it only after the pod is commandable (measured live
// as not within 45s), while a create that genuinely failed says so immediately.
// So dispatch must carry on and let the delivery retry loop wait out the pod
// start -- treating silence as failure would fail every healthy slow start.
func TestDispatchProceedsWhenTheCreateHasNotReportedYet(t *testing.T) {
	gc := gcapitest.New(t)
	gc.CreateSilent = true

	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}}, Args: baseDispatchArgs(),
		SubmitAttempts: 3, SubmitBackoff: zeroBackoff,
		CreateAwaitTimeout: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0: a pod still starting is not a failed create", code)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %d, want 1 -- delivery must proceed", len(gc.Submitted))
	}
}

// An outcome that never arrives is UNKNOWN, and unknown is not success: the run
// must fail rather than record StateRunning for a session that may be idling.
//
// It must also not RESUBMIT. The message may well have landed -- the terminal
// event is what is missing, not necessarily the delivery -- so a retry here
// risks handing the agent its prompt twice while still learning nothing. Asserting
// the attempt count is what pins that half.
func TestDispatchFailsWhenTheOutcomeIsNeverReported(t *testing.T) {
	gc := gcapitest.New(t)
	gc.SubmitSilent = true

	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}}, Args: baseDispatchArgs(),
		SubmitAttempts: 3, SubmitBackoff: zeroBackoff,
		SubmitAwaitTimeout: 50 * time.Millisecond,
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (infra): an unobserved outcome is not a delivered prompt", code)
	}
	if gc.SubmitAttemptsSeen != 1 {
		t.Fatalf("submit attempted %d times, want exactly 1 -- an unknown outcome must not be resubmitted",
			gc.SubmitAttemptsSeen)
	}
}

// A transport-level rejection is terminal, NOT retryable. Only the async-create
// window is a race; retrying a 502 or a 401 just burns the order's 120s timeout
// budget and turns a fast, legible failure into a killed order. Asserting the
// ATTEMPT COUNT (not merely the exit code) is what pins this: without the guard
// the loop would spend every attempt and still exit 1, passing an outcome-only
// test.
func TestDispatchDoesNotRetryTransportRejections(t *testing.T) {
	gc := gcapitest.New(t)
	gc.SubmitFail = 99 // a persistently broken supervisor, not a missing session

	if code := runDispatchForSubmit(t, gc); code != 1 {
		t.Fatalf("exit = %d, want 1 (infra)", code)
	}
	if gc.SubmitAttemptsSeen != 1 {
		t.Fatalf("submit attempted %d times, want exactly 1 -- a hard rejection must not be retried",
			gc.SubmitAttemptsSeen)
	}
}
