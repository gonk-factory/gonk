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

// This file replaces broker_submit_test.go. That file tested delivering the
// prompt by a second signed call (POST .../session/{id}/submit) into opencode's
// TUI, and every one of its cases was about surviving the async-create window:
// resolve_failed, then ErrSessionInactive, then give up.
//
// That whole channel is gone (gonk-mzd). It could not be made reliable, because
// it was a race against composer readiness that reported success either way --
// across five live runs one composer received text while the pod carried
// GC_STARTUP_PROMPT_DELIVERED=1 every time. The pod now FETCHES its prompt, so
// the window those tests defended against is the pod's own retry loop, and the
// properties worth asserting here are different ones.

// The prompt must be stored BEFORE the session is created. A pod can be up and
// asking before CreateSession returns, so a prompt written afterwards is a race
// the pod loses -- it would 404 through its whole window and exit.
func TestDispatchStoresThePromptBeforeCreatingTheSession(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}

	fm.createdAtPut = func() int { return len(gc.Created) }

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: promptTestForge(), Args: baseDispatchArgs(),
		SubmitAttempts: 3, SubmitBackoff: zeroBackoff,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("created = %+v, want one", gc.Created)
	}
	alias := gc.Created[0].Alias
	stored := fm.putPrompts()
	if _, ok := stored[alias]; !ok {
		t.Fatalf("no prompt stored for the created alias %q; stored = %+v", alias, stored)
	}
	// The ordering claim, proven: at the moment the prompt was PUT, no session
	// had been created yet.
	if len(fm.createdCounts) != 1 || fm.createdCounts[0] != 0 {
		t.Fatalf("sessions created at PUT time = %+v, want [0] -- the prompt must be "+
			"stored BEFORE the pod can exist to ask for it", fm.createdCounts)
	}
}

// If the pod never fetches, the run has genuinely failed: the session exists but
// will idle forever and never be swept. That must be an infra failure so the
// re-sling retries with a fresh alias -- and the session must be torn down,
// because "idles forever" is not a figure of speech.
func TestDispatchFailsWhenThePromptIsNeverFetched(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{
		resp: meterapi.DecideResponse{
			Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
		},
		neverFetched: true,
	}

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: promptTestForge(), Args: baseDispatchArgs(),
		SubmitAttempts: 2, SubmitBackoff: zeroBackoff,
	})
	if code == 0 {
		t.Fatal("exit = 0, want non-zero -- a prompt nobody fetched means an agent " +
			"that will idle forever, which is exactly the failure this design ends")
	}
	if len(gc.LiveSessions()) != 0 {
		t.Fatalf("live sessions = %+v, want none -- an undelivered session must be "+
			"torn down or the re-sling leaks a pod per rung", gc.LiveSessions())
	}
}

// The attribution metadata travels WITH the prompt, which is what lets the pod
// render its overlay per session and restores per-bead spend (gonk-m6t). It used
// to be marshalled only to be logged.
func TestStoredPromptCarriesModelAndAttributionMetadata(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "qwen3-6-35b",
		Attempt: 1, ReservationID: "rsv-1",
		Metadata: map[string]string{"gonk_project": "group/repo", "gonk_rung": "cheap"},
	}}

	if code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: promptTestForge(), Args: baseDispatchArgs(),
		SubmitAttempts: 3, SubmitBackoff: zeroBackoff,
	}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := fm.putPrompts()[gc.Created[0].Alias]
	if got.Model != "qwen3-6-35b" {
		t.Fatalf("stored model = %q, want the rung's model -- the pod renders its "+
			"overlay from this", got.Model)
	}
	if !strings.Contains(got.Metadata, "gonk_project") {
		t.Fatalf("stored metadata = %q, want the meter's attribution payload "+
			"(gonk-m6t)", got.Metadata)
	}
}

// The alias is the capability on the meter's one unauthenticated route, so it
// must not be reconstructible from a project and issue number.
func TestSessionAliasCarriesANonceAndIsNotReusedAcrossDispatches(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		gc := gcapitest.New(t)
		fm := &fakeMeter{resp: meterapi.DecideResponse{
			Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
		}}
		if code := runDispatch(context.Background(), dispatchDeps{
			Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
			Forge: promptTestForge(), Args: baseDispatchArgs(),
			SubmitAttempts: 3, SubmitBackoff: zeroBackoff,
		}); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		a := gc.Created[0].Alias
		if seen[a] {
			t.Fatalf("alias %q reused across dispatches -- a guessable alias makes "+
				"the unauthenticated prompt route a public read", a)
		}
		seen[a] = true
		if !aliasLooksNonced(a) {
			t.Fatalf("alias %q has no 26-char base32 nonce; the meter refuses such "+
				"an alias by shape", a)
		}
	}
}

// zeroBackoff kept from the deleted broker_submit_test.go: retry timing is not
// what these tests are about.
func zeroBackoff(int) time.Duration { return 0 }

// A real issue, because the triage prompt renders one; an empty stubForge
// returns a nil issue and the renderer dereferences it.
func promptTestForge() stubForge {
	return stubForge{iss: &glab.Issue{
		IID: 3, Title: "Export times out", State: "opened",
		Labels: []string{"needs-triage"}, Description: "It hangs then 504s.",
	}}
}

func aliasLooksNonced(alias string) bool {
	i := strings.LastIndex(alias, ".")
	if i < 0 {
		return false
	}
	n := alias[i+1:]
	if len(n) != 26 {
		return false
	}
	for _, c := range n {
		if !(c >= 'A' && c <= 'Z') && !(c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}
