package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// THE ROUND TRIP (gonk-u1p.4). Dispatch and sweep were only ever tested apart,
// against a fake whose session output was a pre-seeded string -- placed there by
// the test, never produced by anything. Every bug in this slice so far lived in
// the seams that arrangement skips:
//
//	gonk-u1p.1  the prompt was never delivered (pod launched with a bare command)
//	gonk-u1p.2  a still-running session was judged as "produced no batch"
//	gonk-u1p.5  nothing ever stamped SessionEndedAt, so sweep skipped every bead
//
// This drives the whole chain against one fake supervisor: dispatch creates the
// session and delivers the prompt, the session runs and then STOPS with the
// agent's fenced batch, and only then does sweep read, validate and apply it.
func TestBrokerRoundTripDispatchToAppliedComment(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	applier := &recordingApplier{}
	// The alias is derived, not hardcoded: glabtest assigns the project id.
	alias := brokerSessionAlias(p.ID, 3, 1)

	// ---- 1. DISPATCH: decide, create the session, deliver the prompt --------
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	forge := stubForge{iss: &glab.Issue{
		IID: 3, Title: "CSV export times out over 10k rows", State: "opened",
		Labels:      []string{"needs-triage"},
		Description: "Export hangs ~60s then 504s. Smaller reports are fine.",
	}}
	args := baseDispatchArgs()
	args.ProjectID = p.ID
	if code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Forge: forge, Args: args, SubmitAttempts: 3, SubmitBackoff: zeroBackoff,
	}); code != 0 {
		t.Fatalf("dispatch exit = %d, want 0", code)
	}

	if len(gc.Created) != 1 || gc.Created[0].Alias != alias {
		t.Fatalf("created = %+v, want one session aliased %q", gc.Created, alias)
	}
	if len(gc.Submitted) != 1 || gc.Submitted[0].ID != alias {
		t.Fatalf("submitted = %+v, want the prompt delivered to %q", gc.Submitted, alias)
	}
	if !strings.Contains(gc.Submitted[0].Message, "GONK_BATCH_START") {
		t.Fatalf("the delivered prompt does not state the batch contract:\n%s", gc.Submitted[0].Message)
	}
	// Create leaves it RUNNING -- the agent has work to do.
	if got := gc.SessionStateOf(alias); got != gcapitest.SessionRunning {
		t.Fatalf("session state after dispatch = %q, want running", got)
	}

	// ---- 2. SWEEP WHILE RUNNING: must judge nothing -------------------------
	om := &fakeOutcomeMeter{outcomeNext: "done"}
	omSrv := meterClient(om.server(t))
	sweep := func() int {
		return runSweep(context.Background(), sweepDeps{
			Meter: omSrv, GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
			Store: store, BotUsername: "gonk", PackDir: repoPackDir,
			// Default is a 60s poll for meter's spend view to catch up; a fake
			// meter answers instantly, so keep the test off that deadline.
			SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
		})
	}
	if code := sweep(); code != 0 {
		t.Fatalf("sweep(running) exit = %d", code)
	}
	if len(applier.notes) != 0 || len(om.requests()) != 0 || len(gc.Poured) != 0 {
		t.Fatalf("sweep judged a running session: notes=%d outcomes=%d poured=%d",
			len(applier.notes), len(om.requests()), len(gc.Poured))
	}

	// ---- 3. THE AGENT FINISHES ---------------------------------------------
	gc.FinishSession(alias, "reading the issue...\nthinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Looks like an unbounded export query."},{"kind":"label","add":["gonk::bug"]}]}`+
		"\nGONK_BATCH_END\n")

	// ---- 4. SWEEP AFTER STOP: reads, validates, applies ---------------------
	if code := sweep(); code != 0 {
		t.Fatalf("sweep(stopped) exit = %d", code)
	}
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want exactly one triage comment", applier.notes)
	}
	if !strings.Contains(applier.notes[0].Body, "unbounded export query") {
		t.Fatalf("comment lost the agent's text:\n%s", applier.notes[0].Body)
	}
	if !strings.Contains(applier.notes[0].Body, "<!-- gonk:bead:gk-1a2b -->") {
		t.Fatalf("comment missing the bead marker:\n%s", applier.notes[0].Body)
	}
	if len(applier.labels) != 1 || applier.labels[0].Label != "gonk::bug" {
		t.Fatalf("labels = %+v, want gonk::bug", applier.labels)
	}
	if reqs := om.requests(); len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %+v, want exactly one success", reqs)
	}

	// ---- 5. IDEMPOTENCE: a further sweep must not re-apply -------------------
	if code := sweep(); code != 0 {
		t.Fatalf("sweep(after done) exit = %d", code)
	}
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v after a second sweep, want the comment applied exactly once", applier.notes)
	}
}

// A crashed session is TERMINAL, so it must be judged rather than waited on --
// but it produced no batch, so nothing may be applied. Without a lifecycle in
// the fake this case cannot be expressed at all: a crash and a still-running
// agent both look like "no output".
func TestBrokerRoundTripCrashedSessionIsJudgedButAppliesNothing(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.CrashSession(brokerSessionAlias(p.ID, 3, 1))
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	om := &fakeOutcomeMeter{outcomeNext: "retry"}

	if code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(om.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
	}); code != 0 {
		t.Fatal("sweep exit != 0")
	}
	if len(applier.notes) != 0 || len(applier.labels) != 0 {
		t.Fatalf("applied something from a crashed session: %+v %+v", applier.notes, applier.labels)
	}
	// Terminal => it IS judged (an outcome is reported), unlike a running one.
	if reqs := om.requests(); len(reqs) != 1 {
		t.Fatalf("outcomes = %+v, want exactly one -- a crash is terminal and must be classified", reqs)
	}
}
