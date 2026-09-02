package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

// gonk-u1p.2. gonk-sweep is a 30s cooldown order and a triage session takes
// MINUTES, so sweep routinely reads a session that is still mid-inference. Such
// a session has not "produced no batch" -- it has produced NOTHING YET. Treating
// the two the same reports a gate failure and re-slings the bead onto a more
// expensive rung while the original agent is still working: it burns budget and
// corrupts ladder state, on essentially every run.
//
// An unfinished session is ArtifactUnknown. Uncertainty must never escalate
// (AD-6).
func TestSweepBrokerDoesNotJudgeAStillRunningSession(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	// The agent is mid-flight: output so far, no fence yet.
	gc.RunSession("gonk.triage.p42.i3.a1", "reading the issue...\nthinking...\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	// Nothing may be applied from a session that has not finished.
	if len(applier.notes) != 0 || len(applier.labels) != 0 {
		t.Fatalf("applied from an unfinished session: notes=%+v labels=%+v", applier.notes, applier.labels)
	}
	// No outcome may be reported AT ALL for a session that has not finished.
	// Asserting on the final bead state would test the fake meter (it answers
	// whatever outcomeNext says); asserting that meter was never ASKED is what
	// actually pins the behaviour.
	if reqs := fm.requests(); len(reqs) != 0 {
		t.Fatalf("reported %d outcome(s) for a still-running session: %+v", len(reqs), reqs)
	}
	if len(gc.Poured) != 0 {
		t.Fatalf("re-slung a still-running session: %+v -- that escalates the rung mid-flight", gc.PouredNames())
	}
	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StateRunning {
		t.Fatalf("state = %q, want the bead left running for a later sweep", got.State)
	}
}

// gonk-u1p.5. NOTHING in the tree ever stamps SessionEndedAt -- the only
// assignment anywhere clears it on a re-sling. So a broker bead as DISPATCH
// actually writes it has a zero SessionEndedAt, and the old
// `if rec.SessionEndedAt.IsZero() { return }` guard skipped it on every 30s
// tick, forever: the batch was never read and no comment could ever post.
//
// Every existing broker test passed only because its fixture hand-stamped the
// field -- the one thing production never does. This test deliberately does not.
func TestSweepBrokerJudgesABeadThatWasNeverStampedEnded(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.FinishSession("gonk.triage.p42.i3.a1", "GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"CSV export times out over 10k rows."},{"kind":"label","add":["gonk::bug"]}]}`+
		"\nGONK_BATCH_END\n")
	// Session is NOT running -> finished -> judgeable.
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	rec.SessionEndedAt = time.Time{} // as dispatch really leaves it
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want the triage comment -- an unstamped bead must still be swept", applier.notes)
	}
	// The agent's label plus the broker's verdict audit label (gonk-kxg).
	if len(applier.labels) != 2 ||
		applier.labels[0].Label != "gonk::bug" ||
		applier.labels[1].Label != "gonk::verdict-reply-only" {
		t.Fatalf("labels = %+v, want gonk::bug then gonk::verdict-reply-only", applier.labels)
	}
}

// gonk-u1p.3. The batch must be read from the session's TRANSCRIPT, not from
// the bounded peek preview. An agent that keeps talking after the fence pushes
// it out of a brokerPeekLines window; reading from the window then reports "no
// batch" and re-slings the bead onto a more expensive rung, having thrown away
// work the agent actually did.
//
// The chatter here is deliberately longer than brokerPeekLines, so this test
// fails against a peek-based read and passes against a transcript-based one.
func TestSweepBrokerFindsABatchBuriedBeyondThePeekWindow(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	var out strings.Builder
	out.WriteString("GONK_BATCH_START\n" +
		`{"effects":[{"kind":"comment","body":"Unbounded export query."},{"kind":"label","add":["gonk::bug"]}]}` +
		"\nGONK_BATCH_END\n")
	// ...and then the agent rambles well past the preview window.
	for i := 0; i < brokerPeekLines+50; i++ {
		fmt.Fprintf(&out, "post-batch chatter line %d\n", i)
	}

	gc := gcapitest.New(t)
	gc.FinishSession("gonk.triage.p42.i3.a1", out.String())
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want the batch found despite %d lines of trailing chatter",
			applier.notes, brokerPeekLines+50)
	}
	if !strings.Contains(applier.notes[0].Body, "Unbounded export query") {
		t.Fatalf("wrong comment body:\n%s", applier.notes[0].Body)
	}
}

// A session that never finishes must still be REAPED. Declining to judge a
// running session (gonk-u1p.2) is right, but it cannot mean waiting forever:
// a wedged agent -- one that never received its prompt, say -- would hold its
// reservation until TTL and leave the bead in StateRunning permanently, with
// no outcome ever reported. The reservation IS the deadline.
func TestSweepBrokerReapsARunningSessionPastItsReservation(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	// Still running, and it has produced nothing -- a wedged agent.
	gc.RunSession("gonk.triage.p42.i3.a1", "")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	rec.ReservationExpiresAt = time.Now().Add(-time.Minute) // expired a minute ago
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "retry"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if reqs := fm.requests(); len(reqs) != 1 {
		t.Fatalf("outcomes = %+v, want exactly one -- a session past its reservation must be reaped, not waited on", reqs)
	}
	// Nothing to apply: it produced no batch.
	if len(applier.notes) != 0 {
		t.Fatalf("applied something from a wedged session: %+v", applier.notes)
	}
}
