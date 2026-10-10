package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
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

	rt := newTestRuntime()
	orders := &fakeOrders{}
	// The agent is mid-flight: output so far, no fence yet.
	rt.runSession(t, testSession, "reading the issue...\nthinking...\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
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
	if len(orders.orders()) != 0 {
		t.Fatalf("re-slung a still-running session: %+v -- that escalates the rung mid-flight", orders.names())
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

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.finishSession(t, testSession, "GONK_BATCH_START\n"+
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
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
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

// gonk-u1p.3 / gonk-xflt. The batch is read from the session's WHOLE output,
// wherever it sits. Under Gas City the preview was a pane-sized window, and a
// harness that kept printing after the fence pushed it out of view -- first of
// the peek, then of the ~300-row transcript capture -- until the sweep read
// "no batch" and re-slung a session whose work was done. A pod log has no
// window (plan D1), so a fence followed by thousands of rows of trailing
// output -- far past any pane Gas City ever gave gonk (2000 rows of tmux
// history) -- is still found.
func TestSweepBrokerFindsABatchFollowedByThousandsOfTrailingLines(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	const trailing = 5000
	var out strings.Builder
	out.WriteString("GONK_BATCH_START\n" +
		`{"effects":[{"kind":"comment","body":"Unbounded export query."},{"kind":"label","add":["gonk::bug"]}]}` +
		"\nGONK_BATCH_END\n")
	// ...and then the harness keeps talking.
	for i := 0; i < trailing; i++ {
		fmt.Fprintf(&out, "post-batch harness output line %d\n", i)
	}

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.finishSession(t, testSession, out.String())
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want the batch found despite %d lines of trailing output",
			applier.notes, trailing)
	}
	if !strings.Contains(applier.notes[0].Body, "Unbounded export query") {
		t.Fatalf("wrong comment body:\n%s", applier.notes[0].Body)
	}
}

// A session that never finishes must still be STOPPED and judged. Declining to judge a
// running session (gonk-u1p.2) is right, but it cannot mean waiting forever:
// a wedged agent -- one that never received its prompt, say -- would hold its
// reservation until TTL and leave the bead in StateRunning permanently, with
// no outcome ever reported. The reservation IS the deadline.
func TestSweepBrokerReapsARunningSessionPastItsReservation(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	orders := &fakeOrders{}
	// Still running, and it has produced nothing -- a wedged agent.
	rt.runSession(t, testSession, "")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	rec.ReservationExpiresAt = time.Now().Add(-time.Minute) // expired a minute ago
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "retry"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if reqs := fm.requests(); len(reqs) != 1 {
		t.Fatalf("outcomes = %+v, want exactly one -- a session past its reservation must be judged, not waited on", reqs)
	}
	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("live = %v, want the wedged session stopped", live)
	}
	// Nothing to apply: it produced no batch.
	if len(applier.batchNotes()) != 0 {
		t.Fatalf("applied something from a wedged session: %+v", applier.notes)
	}
}
