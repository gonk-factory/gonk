package main

import (
	"context"
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
	gc.SessionOutputs = map[string]string{
		"gonk.triage.p42.i3.a1": "reading the issue...\nthinking...\n",
	}
	gc.SessionRunning = map[string]bool{"gonk.triage.p42.i3.a1": true}
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
	gc.SessionOutputs = map[string]string{
		"gonk.triage.p42.i3.a1": "GONK_BATCH_START\n" +
			`{"effects":[{"kind":"comment","body":"CSV export times out over 10k rows."},{"kind":"label","add":["gonk::bug"]}]}` +
			"\nGONK_BATCH_END\n",
	}
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
	if len(applier.labels) != 1 || applier.labels[0].Label != "gonk::bug" {
		t.Fatalf("labels = %+v, want gonk::bug", applier.labels)
	}
}
