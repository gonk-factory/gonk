package main

import (
	"context"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// gonk-6n8. TWO dispatches for ONE bead+attempt must not create two sessions.
//
// MEASURED IN PRODUCTION 2026-08-18: go-93gk at 23:11:07Z and go-s4ug at
// 23:15:36Z, 4m29s apart, both carrying the alias `gonk.triage.p75.i24.a1`.
// Not a race -- a re-dispatch, at an interval no lock would have covered.
//
// Why it happened: runDispatch builds `base` FRESH from the webhook args, so it
// carries an empty SessionID even when a session for that exact bead+attempt
// already exists, and nothing else looked. The meter is idempotent here BY
// DESIGN (an open, unsettled reservation returns the same attempt number), so
// the second dispatch computed the same alias and created a second session
// under it.
//
// Why it is severe rather than untidy: Gas City's create-time alias uniqueness
// considers only ACTIVE sessions, while alias RESOLUTION considers all of them.
// Once the first session ended, the duplicate create succeeded and the alias
// permanently resolved to two sessions -- 409 on every read, forever
// (gonk-u6p wedged gonk:75:issue:24 for eight days). Teardown resolves by alias
// too, so an ambiguous alias cannot be reliably closed either.
func TestASecondDispatchForTheSameAttemptCreatesNoSecondSession(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	forge := stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}}
	store := beadstore.NewMemory()
	meter := meterClient(fm.server(t))

	deps := func() dispatchDeps {
		return dispatchDeps{
			// The session's project key rides the prompt row, and dispatch fails
			// closed without it (gonk-8gb).
			Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
			Meter: meter, GC: gc.Client("gonk-city"), Store: store,
			Forge: forge, Args: baseDispatchArgs(),
		}
	}

	if code := runDispatch(context.Background(), deps()); code != 0 {
		t.Fatalf("first dispatch exit = %d, want 0", code)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("first dispatch created %d sessions, want 1", len(gc.Created))
	}
	first := gc.Created[0]

	// The re-dispatch. Same bead, same attempt -- exactly what the meter returns
	// while the reservation is open and unsettled.
	if code := runDispatch(context.Background(), deps()); code != 0 {
		t.Fatalf("second dispatch exit = %d, want 0 (a duplicate is a no-op, not a failure)", code)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("second dispatch created another session: %+v\n"+
			"One alias now resolves to %d sessions, so every read 409s forever and "+
			"teardown cannot address either one (gonk-u6p, gonk-xkm).",
			gc.Created, len(gc.Created))
	}
	if gc.Created[0] != first {
		t.Fatalf("the recorded session changed identity: %v -> %v", first, gc.Created[0])
	}
}

// The guard must key on the ATTEMPT, not merely on "a session exists". A
// re-sling is a NEW attempt and legitimately needs its own session -- refusing
// there would strand every retried bead, which is a worse failure than the one
// being fixed.
func TestARealReSlingOnTheNextAttemptStillGetsItsOwnSession(t *testing.T) {
	gc := gcapitest.New(t)
	forge := stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}}
	store := beadstore.NewMemory()

	fm1 := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	if code := runDispatch(context.Background(), dispatchDeps{
		// The session's project key rides the prompt row, and dispatch fails
		// closed without it (gonk-8gb).
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm1.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Forge: forge, Args: baseDispatchArgs(),
	}); code != 0 {
		t.Fatalf("attempt 1 exit = %d", code)
	}

	fm2 := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 2, ReservationID: "rsv-2",
	}}
	if code := runDispatch(context.Background(), dispatchDeps{
		// The session's project key rides the prompt row, and dispatch fails
		// closed without it (gonk-8gb).
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm2.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Forge: forge, Args: baseDispatchArgs(),
	}); code != 0 {
		t.Fatalf("attempt 2 exit = %d", code)
	}

	if len(gc.Created) != 2 {
		t.Fatalf("created %d sessions across two attempts, want 2 -- a re-sling that "+
			"cannot open a session leaves the bead unable to progress: %+v", len(gc.Created), gc.Created)
	}
	a, b := gc.Created[0].Alias, gc.Created[1].Alias
	if a == b {
		t.Fatalf("both attempts reused alias %q; a re-sling must not collide with the "+
			"prior attempt's session", a)
	}
	// The attempt segment is no longer the SUFFIX -- the nonce is (gonk-mzd) --
	// but it must still be present and correct, because it is what keeps a
	// re-sling legible in logs next to the prior attempt.
	if !strings.Contains(a, ".a1.") || !strings.Contains(b, ".a2.") {
		t.Fatalf("aliases do not carry their attempt: %q, %q", a, b)
	}
}
