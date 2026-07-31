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

// Agent-kind create is always-async upstream: it returns 202 with no session id
// and spawns in the background. The session therefore does NOT exist for the
// first submits, so a 404 must be retried rather than treated as a failure.
func TestDispatchRetriesSubmitUntilSessionMaterializes(t *testing.T) {
	gc := gcapitest.New(t)
	gc.SubmitNotFoundUntil = map[string]int{brokerSessionAlias(42, 3, 1): 2}

	if code := runDispatchForSubmit(t, gc); code != 0 {
		t.Fatalf("exit = %d, want 0 (the 404s are the async-create window, not a failure)", code)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %d, want exactly one accepted prompt after the retries", len(gc.Submitted))
	}
}

// If the prompt never lands, the run has genuinely failed: the session would sit
// idle forever and never be swept. Return infra-failure so the existing re-sling
// machinery retries with a fresh attempt-suffixed alias, rather than silently
// reporting success and stranding an idle session (which is exactly the bug this
// whole change exists to fix).
func TestDispatchFailsWhenPromptNeverDelivers(t *testing.T) {
	gc := gcapitest.New(t)
	gc.SubmitNotFoundUntil = map[string]int{brokerSessionAlias(42, 3, 1): 99}

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

// A non-404 is terminal, NOT retryable. Only the 404 window is the async-create
// race; retrying a 502 or a 401 just burns the order's 120s timeout budget and
// turns a fast, legible failure into a killed order. Asserting the ATTEMPT COUNT
// (not merely the exit code) is what pins this: without the IsNotFound guard the
// loop would spend every attempt and still exit 1, passing an outcome-only test.
func TestDispatchDoesNotRetryNonNotFoundSubmitErrors(t *testing.T) {
	gc := gcapitest.New(t)
	gc.SubmitFail = 99 // a persistently broken supervisor, not a missing session

	if code := runDispatchForSubmit(t, gc); code != 1 {
		t.Fatalf("exit = %d, want 1 (infra)", code)
	}
	if gc.SubmitAttemptsSeen != 1 {
		t.Fatalf("submit attempted %d times, want exactly 1 -- a non-404 must not be retried",
			gc.SubmitAttemptsSeen)
	}
}
