package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
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
// This drives the whole chain against one fake SessionRuntime: dispatch starts
// the session with the prompt mounted, the session runs and then FINISHES with
// the agent's fenced batch, and only then does sweep read, validate and apply
// it.
func TestBrokerRoundTripDispatchToAppliedComment(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	orders := &fakeOrders{}
	store := beadstore.NewMemory()
	applier := &recordingApplier{}

	// ---- 1. DISPATCH: decide, create the session, deliver the prompt --------
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
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
		// The session's project key rides the prompt row, and dispatch fails
		// closed without it (gonk-8gb).
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Forge: forge, Args: args, Clock: newFakeClock(),
	}); code != 0 {
		t.Fatalf("dispatch exit = %d, want 0", code)
	}

	starts := rt.Starts()
	if len(starts) != 1 {
		t.Fatalf("starts = %+v, want exactly one session", starts)
	}
	h := runtime.Handle(starts[0].Name)
	if !strings.Contains(string(starts[0].Prompt), "GONK_BATCH_START") {
		t.Fatalf("the mounted prompt does not state the batch contract:\n%s", starts[0].Prompt)
	}
	// The pod comes up and the agent works.
	rt.SetPhase(h, runtime.PhaseRunning, "")

	// ---- 2. SWEEP WHILE RUNNING: must judge nothing -------------------------
	om := &fakeOutcomeMeter{outcomeNext: "done"}
	omSrv := meterClient(om.server(t))
	sweep := func() int {
		return runSweep(context.Background(), sweepDeps{
			Meter: omSrv, Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
			Store: store, BotUsername: "gonk", PackDir: repoPackDir,
			// Default is a 60s poll for meter's spend view to catch up; a fake
			// meter answers instantly, so keep the test off that deadline.
			SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
		})
	}
	if code := sweep(); code != 0 {
		t.Fatalf("sweep(running) exit = %d", code)
	}
	if len(applier.notes) != 0 || len(om.requests()) != 0 || len(orders.orders()) != 0 {
		t.Fatalf("sweep judged a running session: notes=%d outcomes=%d orders=%d",
			len(applier.notes), len(om.requests()), len(orders.orders()))
	}

	// ---- 3. THE AGENT FINISHES ---------------------------------------------
	rt.SetOutput(h, []byte("reading the issue...\nthinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Looks like an unbounded export query."},{"kind":"label","add":["gonk::bug"]}]}`+
		"\nGONK_BATCH_END\n"))
	rt.SetPhase(h, runtime.PhaseSucceeded, "")

	// ---- 4. SWEEP AFTER THE FINISH: reads, validates, applies ---------------
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
	// The agent's label, plus the BROKER'S OWN audit label recording which
	// verdict it acted on (gonk-kxg). The second is written by gonk, not
	// proposed by the model, which is what makes it trustworthy as an audit
	// trail -- so it is asserted here rather than tolerated.
	if len(applier.labels) != 2 ||
		applier.labels[0].Label != "gonk::bug" ||
		applier.labels[1].Label != "gonk::verdict-reply-only" {
		t.Fatalf("labels = %+v, want gonk::bug then gonk::verdict-reply-only", applier.labels)
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
	// ...and the judged session is gone: nothing left running behind a done bead.
	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("live sessions after the round trip = %v, want none", live)
	}
}

// A crashed session (Failed, no batch) is TERMINAL, so it must be judged
// rather than waited on -- but it produced no batch, so nothing may be applied.
func TestBrokerRoundTripCrashedSessionIsJudgedButAppliesNothing(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	rt.failSession(t, brokerRunningRecord(p.ID).SessionID, "BackoffLimitExceeded",
		"session start\nopencode: fatal: segmentation fault\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	om := &fakeOutcomeMeter{outcomeNext: "retry"}

	if code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(om.server(t)), Runtime: rt, Orders: &fakeOrders{}, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
	}); code != 0 {
		t.Fatal("sweep exit != 0")
	}
	if len(applier.batchNotes()) != 0 || len(applier.labels) != 0 {
		t.Fatalf("applied something from a crashed session: %+v %+v", applier.notes, applier.labels)
	}
	// Terminal => it IS judged (an outcome is reported), unlike a running one.
	if reqs := om.requests(); len(reqs) != 1 {
		t.Fatalf("outcomes = %+v, want exactly one -- a crash is terminal and must be classified", reqs)
	}
}
