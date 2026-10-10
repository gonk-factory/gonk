package main

import (
	"context"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
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
// That is why these assert on what the runtime still holds (testRuntime.live)
// and not on an exit code or a recorded call. "runSweep returned 0" was true the whole time it was leaking. The only
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

	rt := newTestRuntime()
	orders := &fakeOrders{}
	store := beadstore.NewMemory()
	applier := &recordingApplier{}

	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	args := baseDispatchArgs()
	args.ProjectID = p.ID
	if code := runDispatch(context.Background(), dispatchDeps{
		// The session's project key rides the prompt row, and dispatch fails
		// closed without it (gonk-8gb).
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Forge: stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}},
		Args:  args, Clock: newFakeClock(),
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
			Meter: omSrv, Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
			Store: store, BotUsername: "gonk", PackDir: repoPackDir,
			SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
		})
	}
	if code := sweep(); code != 0 {
		t.Fatalf("sweep(running) exit = %d", code)
	}
	alias := runtime.Handle(rt.Starts()[0].Name)
	if live := rt.live(t); len(live) != 1 || live[0] != alias {
		t.Fatalf("live sessions while the agent is still working = %v, want exactly [%s]: "+
			"tearing a session down mid-turn destroys the batch it has not written yet", live, alias)
	}

	// The agent finishes and writes its batch.
	rt.SetOutput(alias, []byte("GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"unbounded export query"}]}`+
		"\nGONK_BATCH_END\n"))
	rt.SetPhase(alias, runtime.PhaseSucceeded, "")

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

	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("LEAKED: %v still holding a pod after a completed, judged, applied dispatch. "+
			"This is gonk-xkm: the comment posted, the outcome was reported, and the pod stayed. "+
			"Thirteen of these wedged the cluster's scheduler.", live)
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

			rt := newTestRuntime()
			orders := &fakeOrders{}
			store := beadstore.NewMemory()
			rec := brokerRunningRecord(p.ID)
			// A finished session with no batch: enough to be judged, and the
			// outcome the fake meter answers with is what varies here.
			rt.finishSession(t, rec.SessionID, "the agent said nothing useful")
			_ = store.Put(context.Background(), rec)

			om := &fakeOutcomeMeter{outcomeNext: next}
			if code := runSweep(context.Background(), sweepDeps{
				Meter: meterClient(om.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(),
				Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
				SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
			}); code != 0 {
				t.Fatalf("sweep exit != 0")
			}
			if reqs := om.requests(); len(reqs) != 1 {
				t.Fatalf("outcomes = %+v, want exactly one -- the bead must be judged", reqs)
			}
			if live := rt.live(t); len(live) != 0 {
				t.Fatalf("LEAKED on next=%q: %v still holding a pod. A re-sling opens a NEW "+
					"attempt-suffixed session, so this leaks once per ladder rung.", next, live)
			}
		})
	}
}

// The session nothing will ever judge is the one most likely to be leaked: it
// was admitted, but its running record never landed, so no sweep will look at
// it. Under Gas City this was gonk-pev (the create succeeded, delivery failed,
// the session stayed forever, once per attempt). Dispatch must stop it.
func TestDispatchStopsTheSessionWhenItsRecordFails(t *testing.T) {
	rt := newTestRuntime()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	clk := newFakeClock()
	code := runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt,
		// Write 1 is the reservation; write 2, the running record, fails.
		Store: &slowStore{Store: beadstore.NewMemory(), clk: clk, free: 99, failAt: map[int]bool{2: true}},
		Forge: stubForge{iss: &glab.Issue{IID: 3, Title: "t", State: "opened"}},
		Args:  baseDispatchArgs(), Clock: clk,
	})
	if code != 1 {
		t.Fatalf("dispatch exit = %d, want 1 -- a session with no record is an infra failure", code)
	}
	if n := len(rt.Starts()); n != 1 {
		t.Fatalf("starts = %d, want 1 (the premise: a session WAS admitted)", n)
	}
	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("LEAKED: %v left running with no record naming it. No sweep will ever judge it, "+
			"and the re-sling will start another.", live)
	}
}
