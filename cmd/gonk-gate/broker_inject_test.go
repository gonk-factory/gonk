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

// stubForge is a minimal issueReader: it returns a canned issue (or error) so
// the broker's context-fetch can be exercised without a live GitLab.
type stubForge struct {
	iss *glab.Issue
	err error
}

func (s stubForge) GetIssue(_ context.Context, _, _ int64) (*glab.Issue, error) {
	return s.iss, s.err
}

// With a forge configured, the broker fetches the issue controller-side and
// splices its title/labels/body into the prompt -- the agent pod cannot fetch
// it (no creds), so the context MUST come pre-fetched in the prompt.
func TestDispatchInjectsIssueContext(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	forge := stubForge{iss: &glab.Issue{
		IID: 3, Title: "Login button does nothing on Safari", State: "opened",
		Labels:      []string{"needs-triage"},
		Description: "Steps: click login in Safari 17. Nothing happens. Works in Chrome.",
	}}

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: forge, Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("created = %+v, want one", gc.Created)
	}
	// The prompt rides the SUBMIT, not the create: k8s-backed sessions never
	// receive template_overrides.initial_message (gonk-u1p.1 / gonk-drf).
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %+v, want one", gc.Submitted)
	}
	msg := gc.Submitted[0].Message
	for _, want := range []string{
		"fetched for you",
		"Login button does nothing on Safari", // title
		"needs-triage",                        // current label
		"Nothing happens. Works in Chrome.",   // body
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("prompt missing %q:\n%s", want, msg)
		}
	}
}

// A forge fetch failure must NOT fail the dispatch: the session is still
// created (a re-sling can retry context), with the degraded reference-only
// prompt marker.
func TestDispatchProceedsWhenContextFetchFails(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	forge := stubForge{err: context.DeadlineExceeded}

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: forge, Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (a context-fetch failure must not fail the run)", code)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("created = %+v, want one even on a context miss", gc.Created)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %+v, want one even on a context miss", gc.Submitted)
	}
	if !strings.Contains(gc.Submitted[0].Message, "issue context unavailable") {
		t.Fatalf("prompt should carry the degraded marker:\n%s", gc.Submitted[0].Message)
	}
}

func TestCapBody(t *testing.T) {
	// Under cap: unchanged.
	if got := capBody("short", 100); got != "short" {
		t.Fatalf("under-cap body changed: %q", got)
	}
	// Over cap: truncated to <= max bytes of original + a visible marker.
	big := strings.Repeat("a", 50)
	got := capBody(big, 10)
	if !strings.Contains(got, "truncated by gonk") {
		t.Fatalf("over-cap body lacks truncation marker: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 10)) {
		t.Fatalf("kept the wrong prefix: %q", got)
	}
	// Multibyte safety: cutting mid-rune must not leave an invalid trailing byte.
	multi := strings.Repeat("é", 20) // 2 bytes each
	if got := capBody(multi, 5); strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncation split a rune: %q", got)
	}
}
