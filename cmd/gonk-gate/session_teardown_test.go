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

// TEARDOWN (gonk-xkm). Every test in this file asserts the same single fact from
// a different direction: WHEN GONK IS FINISHED WITH A SESSION, NO POD SURVIVES.
//
// Nothing in this tree closed a session before 2026-08-03. Not the happy path,
// not the re-sling, not a failed prompt delivery. Each of those reported success
// -- the comment posted, the outcome was recorded, the suite stayed green -- and
// left a 500m/1Gi pod behind. Thirteen accumulated, the three schedulable nodes
// reached 98%/91%/96% of requests, and then the failure surfaced somewhere else
// entirely: a scaffold dispatch three days later whose pod could not be
// SCHEDULED, reported as a prompt-delivery timeout (gonk-pev).
//
// That is why these assert on LiveSessions and not on an exit code or a recorded
// call. "runSweep returned 0" was true the whole time it was leaking. The only
// question that would have caught this is what is left standing afterwards.
//
// See also TestSweepLeavesNoLiveSessionOnEveryTerminalOutcome, which walks the
// same assertion across done/escalate/retry so no single ladder branch can be
// the one that leaks.

// The headline case: a dispatch that succeeds end-to-end, is swept, is judged,
// applies its comment -- and leaves nothing running.
func TestRoundTripLeavesNoLiveSessionBehind(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	applier := &recordingApplier{}
	var alias string

	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	args := baseDispatchArgs()
	args.ProjectID = p.ID
	if code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Forge: stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}},
		Args:  args, SubmitAttempts: 3, SubmitBackoff: zeroBackoff,
	}); code != 0 {
		t.Fatalf("dispatch exit = %d, want 0", code)
	}

	// While the agent is working the session MUST still be live. A teardown that
	// fires early is not a fix, it is a different bug -- it would kill the agent
	// mid-turn and the batch would never be written.
	om := &fakeOutcomeMeter{outcomeNext: "done"}
	omSrv := meterClient(om.server(t))
	sweep := func() int {
		return runSweep(context.Background(), sweepDeps{
			Meter: omSrv, GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
			Store: store, BotUsername: "gonk", PackDir: repoPackDir,
			SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
		})
	}
	if code := sweep(); code != 0 {
		t.Fatalf("sweep(running) exit = %d", code)
	}
	alias = gc.Created[0].Alias // captured, not recomputed: the alias is nonced
	if live := gc.LiveSessions(); len(live) != 1 || live[0] != alias {
		t.Fatalf("live sessions while the agent is still working = %v, want exactly [%s]: "+
			"tearing a session down mid-turn destroys the batch it has not written yet", live, alias)
	}

	// The agent finishes and writes its batch.
	gc.FinishSession(alias, "GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"unbounded export query"}]}`+
		"\nGONK_BATCH_END\n")

	if code := sweep(); code != 0 {
		t.Fatalf("sweep(stopped) exit = %d", code)
	}
	// The work really did complete -- otherwise "no live sessions" would pass for
	// the boring reason that nothing ever ran.
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want the triage comment applied", applier.notes)
	}
	if reqs := om.requests(); len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %+v, want exactly one success", reqs)
	}

	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("LEAKED: %v still holding a pod after a completed, judged, applied dispatch. "+
			"This is gonk-xkm: the comment posted, the outcome was reported, and the pod stayed. "+
			"Thirteen of these wedged the cluster's scheduler.", live)
	}
	if got := gc.SessionStateOf(alias); got != gcapitest.SessionClosed {
		t.Fatalf("session state = %q, want %q", got, gcapitest.SessionClosed)
	}
}

// Every terminal ladder branch, not just the happy one. A re-sling creates a
// FRESH attempt-suffixed session, so an unclosed session on retry/escalate leaks
// once PER ATTEMPT -- strictly worse than the success path, and the reason the
// live cluster held two aliases for the same issue (i16.a1 AND i16.a2, i17.a1
// AND i17.a2, i18, i20, i21 -- five issues, both attempts, ten pods).
func TestSweepLeavesNoLiveSessionOnEveryTerminalOutcome(t *testing.T) {
	for _, next := range []string{"done", "escalate", "retry"} {
		t.Run(next, func(t *testing.T) {
			gl := glabtest.New(t)
			gl.Me = glab.User{ID: 1, Username: "gonk"}
			p := gl.AddProject("group/repo", glab.AccessMaintainer)
			gl.AddIssue(p.ID, 3, "opened")

			gc := gcapitest.New(t)
			store := beadstore.NewMemory()
			rec := brokerRunningRecord(p.ID)
			// A finished session with no batch: enough to be judged, and the
			// outcome the fake meter answers with is what varies here.
			gc.FinishSession(rec.SessionID, "the agent said nothing useful")
			_ = store.Put(context.Background(), rec)

			om := &fakeOutcomeMeter{outcomeNext: next}
			if code := runSweep(context.Background(), sweepDeps{
				Meter: meterClient(om.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(),
				Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
				SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
			}); code != 0 {
				t.Fatalf("sweep exit != 0")
			}
			if reqs := om.requests(); len(reqs) != 1 {
				t.Fatalf("outcomes = %+v, want exactly one -- the bead must be judged", reqs)
			}
			if live := gc.LiveSessions(); len(live) != 0 {
				t.Fatalf("LEAKED on next=%q: %v still holding a pod. A re-sling opens a NEW "+
					"attempt-suffixed session, so this leaks once per ladder rung.", next, live)
			}
		})
	}
}

// The session gonk could not talk to is the one most likely to be leaked, and it
// is exactly what happened in gonk-pev: the create succeeded, delivery failed,
// dispatch returned 1, and the session stayed forever. Worse, the failure is
// retried -- so this leaks once per attempt while producing nothing at all.
func TestDispatchClosesTheSessionWhenPromptDeliveryFails(t *testing.T) {
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	// The pod never fetches its prompt, so delivery exhausts its attempts. This
	// used to be modelled as every submit reporting resolve_failed; the submit
	// channel is gone (gonk-mzd) and never-fetched is its successor.
	fm := &fakeMeter{
		resp: meterapi.DecideResponse{
			Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
		},
		neverFetched: true,
	}
	args := baseDispatchArgs()
	args.ProjectID = 42
	args.IssueIID = 7

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Forge: stubForge{iss: &glab.Issue{IID: 7, Title: "t", State: "opened"}},
		Args:  args, SubmitAttempts: 2, SubmitBackoff: zeroBackoff,
	})
	if code != 1 {
		t.Fatalf("dispatch exit = %d, want 1 -- an undelivered prompt is an infra failure", code)
	}
	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("LEAKED: %v left running after delivery failed. The agent never got a prompt, "+
			"so it will idle forever holding a pod, and the re-sling will create another one.", live)
	}
}

// Teardown is best-effort by design -- the outcome is already reported and the
// bead already moved on, so a close failure must not fail the sweep or re-judge
// the bead. What it must NOT do is pass quietly. A swallowed teardown error is
// the same silent success that produced gonk-xkm, one level up.
func TestSweepSurvivesACloseFailureButSaysSo(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.CloseFail = 99
	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	gc.FinishSession(rec.SessionID, "no batch here")
	_ = store.Put(context.Background(), rec)

	var logged strings.Builder
	om := &fakeOutcomeMeter{outcomeNext: "done"}
	if code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(om.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		Log:               testLogger(&logged),
		SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
	}); code != 0 {
		t.Fatalf("sweep exit = %d, want 0 -- a teardown failure must not undo a reported outcome", code)
	}
	// The bead still reached its terminal state: teardown is downstream of the
	// verdict, not a precondition for it.
	got, ok, err := store.Get(context.Background(), rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("store.Get: ok=%v err=%v", ok, err)
	}
	if got.State != beadstore.StateDone {
		t.Fatalf("bead state = %q, want %q -- a close failure must not strand the bead",
			got.State, beadstore.StateDone)
	}
	if !strings.Contains(logged.String(), "session close failed") {
		t.Fatalf("a failed teardown was swallowed. Log said:\n%s", logged.String())
	}
}
