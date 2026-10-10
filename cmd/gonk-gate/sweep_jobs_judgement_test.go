package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// The sweep's judgement under the Jobs runtime (gonk-t55 plan D9): the
// runtime's phase says WHEN a session can be judged, the batch in its output
// says WHAT the outcome is, and a session killed at its deadline is
// infra-failed. Every infra-failed verdict names its cause (gonk-i391).

// jobsSweepFixture is one broker bead on issue 3 of a GitLab project, with the
// meter answering `next` and a sweep wired to rt.
type jobsSweepFixture struct {
	rec     beadstore.Record
	store   *beadstore.Memory
	fm      *fakeOutcomeMeter
	applier *recordingApplier
	orders  *fakeOrders
	log     *syncBuilder
	deps    sweepDeps
}

func newJobsSweepFixture(t *testing.T, rt *testRuntime, fm *fakeOutcomeMeter) *jobsSweepFixture {
	t.Helper()
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	rec := brokerRunningRecord(p.ID)
	// A deadline 30 minutes out, as meter mints one: inside its reservation.
	rec.ReservationExpiresAt = time.Now().Add(30 * time.Minute)
	f := &jobsSweepFixture{
		rec: rec, store: beadstore.NewMemory(), fm: fm,
		applier: &recordingApplier{}, orders: &fakeOrders{}, log: &syncBuilder{},
	}
	if err := f.store.Put(context.Background(), f.rec); err != nil {
		t.Fatal(err)
	}
	f.deps = sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: f.orders, GL: gl.Client(), Apply: f.applier,
		Store: f.store, BotUsername: "gonk", PackDir: repoPackDir, Log: testLogger(f.log),
		SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	}
	return f
}

func (f *jobsSweepFixture) sweep(t *testing.T) {
	t.Helper()
	if code := runSweep(context.Background(), f.deps); code != 0 {
		t.Fatalf("sweep exit = %d; log:\n%s", code, f.log.String())
	}
}

// oneOutcome asserts the sweep reported exactly one outcome, and returns it.
func (f *jobsSweepFixture) oneOutcome(t *testing.T) string {
	t.Helper()
	reqs := f.fm.requests()
	if len(reqs) != 1 {
		t.Fatalf("outcomes = %+v, want exactly one; log:\n%s", reqs, f.log.String())
	}
	return reqs[0].Outcome
}

// rejectedToolThenBatch is `opencode run`'s output for a run that hit a
// REJECTED tool call and went on to answer (the ADR-008 section 13 gate run,
// runB: "Error: The user rejected permission to use this specific tool call",
// then rc=0).
const rejectedToolThenBatch = "session start: runtime=jobs session=gonk-triage-p42-i3-a1 agent=triage attempt=1\n" +
	"| Bash     ls -la\n" +
	"Error: The user rejected permission to use this specific tool call\n" +
	"I will answer from the issue alone.\n" +
	"GONK_BATCH_START\n" +
	`{"verdict":"reply-only","effects":[{"kind":"comment","body":"Could you share the export size?"}]}` +
	"\nGONK_BATCH_END\n" +
	"session end: runtime=jobs -- exiting 0 so the Job reports a terminal status\n"

// PLAN P3 ITEM 4 (bead gonk-t55 note (c)): opencode exits 0 after a REJECTED
// tool call, so a Succeeded session says nothing about the outcome. One whose
// output holds a valid batch after the rejection is judged ON THE BATCH:
// applied, success, done.
func TestASucceededSessionWithABatchAfterARejectedToolCallIsJudgedOnTheBatch(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "done"})
	rt.finishSession(t, testSession, rejectedToolThenBatch)

	f.sweep(t)
	if len(f.applier.notes) != 1 || !strings.Contains(f.applier.notes[0].Body, "Could you share the export size?") {
		t.Fatalf("notes = %+v, want the batch's one comment applied; log:\n%s", f.applier.notes, f.log.String())
	}
	if got := f.oneOutcome(t); got != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %q, want success (the batch is the outcome, not the exit code)", got)
	}
	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("live = %v, want the judged session stopped", live)
	}
}

// ...and the same holds for a Succeeded session with NO batch: exit 0 is not
// success. It falls to the ladder exactly like any batch-less session.
func TestASucceededSessionWithNoBatchIsNotASuccess(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{
		outcomeNext: "escalate", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 900, AsOf: time.Now().Add(time.Hour)},
	})
	rt.finishSession(t, testSession, "session start\nError: The user rejected permission to use this specific tool call\nsession end: exiting 0\n")

	f.sweep(t)
	if len(f.applier.batchNotes()) != 0 {
		t.Fatalf("notes = %+v, want nothing applied", f.applier.notes)
	}
	if got := f.oneOutcome(t); got != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %q, want gate-failed (tokens spent, no batch) -- a Succeeded phase is not the outcome", got)
	}
}

// A session that FAILED (a non-zero exit that is not the deadline) is judged on
// its batch too: a batch fenced before the harness fell over is still the
// agent's answer.
func TestAFailedSessionIsStillJudgedOnItsBatch(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "done"})
	rt.failSession(t, testSession, "BackoffLimitExceeded", rejectedToolThenBatch+"panic: runtime error\n")

	f.sweep(t)
	if len(f.applier.notes) != 1 {
		t.Fatalf("notes = %+v, want the batch applied; log:\n%s", f.applier.notes, f.log.String())
	}
	if got := f.oneOutcome(t); got != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %q, want success", got)
	}
}

// PLAN P3 ITEM 4 (bead gonk-t55 exit criterion 5): a session the runtime KILLED
// AT ITS DEADLINE is infra-failed -- even when its output holds a valid batch,
// and even when the sweep's own clock has not yet passed the reservation (the
// deadline is the reservation's, and the runtime may say so a tick first). Its
// output is not read, nothing is applied, the cause is logged, and it does not
// escalate.
func TestADeadlineExceededSessionIsInfraFailedAndSaysWhy(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
	rt.failSession(t, testSession, reasonDeadlineExceeded, rejectedToolThenBatch)
	if reservationExpired(f.rec, time.Now()) {
		t.Fatal("test setup: the record's reservation has already expired; this test is about the runtime saying so first")
	}

	f.sweep(t)
	if got := f.oneOutcome(t); got != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %q, want infra-failed for a DeadlineExceeded session; log:\n%s", got, f.log.String())
	}
	if len(f.applier.batchNotes()) != 0 {
		t.Fatalf("notes = %+v: a session killed at its deadline must not have its batch applied", f.applier.notes)
	}
	out := f.log.String()
	if !strings.Contains(out, "sweep: classified infra-failed") || !strings.Contains(out, "cause=session_deadline_exceeded") {
		t.Fatalf("the infra-failed verdict does not name its cause; log:\n%s", out)
	}
	if live := rt.live(t); len(live) != 0 {
		t.Fatalf("live = %v, want the dead session's Job removed", live)
	}
}

// A FINISHED session whose output is empty -- the Jobs runtime returns empty
// output for a Job that never got a pod (DeadlineExceeded before scheduling,
// say) -- is judged NOW, as infra-failed with its cause logged. It is not an
// unreadable session to retry forever, and never "no batch", which would
// charge the agent for a log that does not exist.
func TestAFinishedSessionWithEmptyOutputIsJudgedInfraFailedWithItsCause(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
	rt.finishSession(t, testSession, "")

	f.sweep(t)
	if got := f.oneOutcome(t); got != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %q, want infra-failed", got)
	}
	out := f.log.String()
	if !strings.Contains(out, "transcript read EMPTY") {
		t.Fatalf("the empty read was not recorded; log:\n%s", out)
	}
	if !strings.Contains(out, "cause=\"artifact_unknown: batch read/apply error: output of finished session") {
		t.Fatalf("the infra-failed verdict does not name the empty output as its cause; log:\n%s", out)
	}
	if names := f.orders.names(); len(names) != 1 {
		t.Fatalf("re-slings = %v, want one: a judged infra-failure re-slings, it does not wait", names)
	}
}

// EVERY INFRA-FAILED VERDICT NAMES ITS CAUSE (gonk-i391: an infra-failed
// verdict denied issue 82 with nothing in the log to say why). One case per
// signal gate.Classify turns into infra-failed.
func TestEveryInfraFailedVerdictNamesItsCause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, rt *testRuntime, f *jobsSweepFixture)
		wantCause string
	}{
		{"reservation expired", func(t *testing.T, rt *testRuntime, f *jobsSweepFixture) {
			rt.runSession(t, testSession, "still going")
			f.deps.Now = func() time.Time { return f.rec.ReservationExpiresAt.Add(time.Minute) }
		}, "cause=reservation_expired"},
		{"output unreadable", func(t *testing.T, rt *testRuntime, f *jobsSweepFixture) {
			rt.finishSession(t, testSession, "x")
			rt.SetOutputErr(runtime.Handle(testSession), errors.New("pods/log: connection refused"))
		}, "cause=\"artifact_unknown: batch read/apply error: read session output: pods/log: connection refused\""},
		{"issue unreadable", func(t *testing.T, rt *testRuntime, f *jobsSweepFixture) {
			rt.finishSession(t, testSession, rejectedToolThenBatch)
			f.rec.IssueIID = 99 // glabtest has no issue 99
			if err := f.store.Put(context.Background(), f.rec); err != nil {
				t.Fatal(err)
			}
		}, "cause=\"artifact_unknown: issue read failed"},
		{"spend stale", func(t *testing.T, rt *testRuntime, f *jobsSweepFixture) {
			rt.finishSession(t, testSession, "no batch here")
			f.fm.spendSynced = true // but the cost view never catches up (AsOf zero)
		}, "cause=\"spend_stale"},
		{"no model tokens", func(t *testing.T, rt *testRuntime, f *jobsSweepFixture) {
			rt.finishSession(t, testSession, "no batch here")
			f.fm.spendSynced = true
			f.fm.sessionCost = meterapi.SessionCostResponse{TotalTokens: 0, AsOf: time.Now().Add(time.Hour)}
		}, "cause=\"no_model_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newTestRuntime()
			f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
			tc.setup(t, rt, f)
			f.sweep(t)
			if got := f.oneOutcome(t); got != meterapi.OutcomeInfraFailed {
				t.Fatalf("outcome = %q, want infra-failed; log:\n%s", got, f.log.String())
			}
			out := f.log.String()
			if !strings.Contains(out, "sweep: classified infra-failed") || !strings.Contains(out, tc.wantCause) {
				t.Fatalf("no infra-failed line carrying %s; log:\n%s", tc.wantCause, out)
			}
		})
	}
}

// Stopping a judged session whose Job is already gone (its TTL, a human) is
// "already gone" at INFO -- not ERROR "leaked" (ADR-008 section 7, gonk-wmb7
// polarity). Under Gas City a 404 on close could hide a live pod; under Jobs
// the pod goes with its Job.
func TestStoppingAJudgedSessionThatIsAlreadyGoneIsInfoNotAnError(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "done"})
	rt.finishSession(t, testSession, rejectedToolThenBatch)
	// The Job vanishes between the judgement and the Stop.
	rt.onStop = func(h runtime.Handle) { _ = rt.Runtime.Stop(context.Background(), h) }

	f.sweep(t)
	out := f.log.String()
	if !strings.Contains(out, "sweep: session already gone") {
		t.Fatalf("a not-found stop was not logged as already gone; log:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, testSession) {
			t.Fatalf("an already-gone session produced an ERROR: %s", line)
		}
	}
}

// A Stop that genuinely FAILS is loud: the session keeps its pod until its
// deadline. The sweep still completes and the outcome still stands.
func TestSweepSurvivesAStopFailureButSaysSo(t *testing.T) {
	rt := newTestRuntime()
	rt.stopErr = errors.New("jobs.batch is forbidden")
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "done"})
	rt.finishSession(t, testSession, rejectedToolThenBatch)

	f.sweep(t)
	if got := f.oneOutcome(t); got != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %q, want success", got)
	}
	out := f.log.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "session stop failed") ||
		!strings.Contains(out, "jobs.batch is forbidden") {
		t.Fatalf("a failed stop was not reported at ERROR with its cause; log:\n%s", out)
	}
	got, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateDone {
		t.Fatalf("state = %q, want done: a teardown failure must not undo the verdict", got.State)
	}
}

// The runtime's phase and reason ride on the lines an operator reads: the
// held session's "still running" line (so a session stuck Pending on an
// ImagePullBackOff is visible) and the judged session's release line, with
// released_by.
func TestSweepLogsTheRuntimesPhaseAndReason(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "done"})
	h := rt.startNamed(t, testSession)
	rt.SetStatus(h, runtime.Status{Phase: runtime.PhasePending, Reason: "ImagePullBackOff"})

	f.sweep(t)
	out := f.log.String()
	if !strings.Contains(out, "sweep: session still running; nothing to judge yet") ||
		!strings.Contains(out, "session_phase=Pending") || !strings.Contains(out, "session_reason=ImagePullBackOff") {
		t.Fatalf("the held session's line lacks its phase and reason; log:\n%s", out)
	}

	rt.SetOutput(h, []byte(rejectedToolThenBatch))
	rt.SetPhase(h, runtime.PhaseSucceeded, "Completed")
	f.sweep(t)
	out = f.log.String()
	if !strings.Contains(out, "sweep: session finished; judging it") || !strings.Contains(out, "released_by=finished") ||
		!strings.Contains(out, "session_phase=Succeeded") {
		t.Fatalf("the judged session's line lacks released_by/phase; log:\n%s", out)
	}
	if !strings.Contains(out, "transcript read") || !strings.Contains(out, "batch_fence=true") {
		t.Fatalf("no transcript record for the judged output; log:\n%s", out)
	}
}

// The released_by values are WIRE VALUES operator queries are written against
// (gonk-w35m). "absent" keeps its Gas City spelling and meaning.
func TestTheReleaseReasonVocabularyIsStable(t *testing.T) {
	if releasedByFinished != "finished" || releasedByAbsent != "absent" {
		t.Fatalf("released_by vocabulary changed: finished=%q absent=%q", releasedByFinished, releasedByAbsent)
	}
}

// A session the runtime has no record of is judged as having produced
// nothing, released_by=absent -- a re-sling, not a wait.
func TestAnAbsentSessionIsJudgedAsHavingProducedNothing(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
	f.sweep(t)
	out := f.log.String()
	if !strings.Contains(out, "released_by=absent") || !strings.Contains(out, "session output read FAILED: no such session") {
		t.Fatalf("an absent session was not judged as absent; log:\n%s", out)
	}
	if got := f.oneOutcome(t); got != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %q, want infra-failed (no batch, no spend)", got)
	}
	if names := f.orders.names(); len(names) != 1 {
		t.Fatalf("re-slings = %v, want one", names)
	}
}
