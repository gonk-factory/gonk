package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// THE LOOP WATCHDOG (gonk-un7z): a session the entrypoint stopped for
// repeating a failing tool call is judged like any other batch-less session
// that spent tokens -- gate-failed, escalated -- and the classification line
// names the repeated call. See sweep_loop.go.

// loopStoppedOutput is the pod log of a stopped session as the entrypoint
// writes it (test/entrypoint TestLoopWatchdogStopsTheRom1Loop pins the
// script's side of this shape, and the k3d component test pins both against
// the real image): rom#1's real failing lines, then the watchdog's line.
const loopStoppedOutput = "gonk-agent-entrypoint: starting opencode run (non-interactive)\n" +
	"gonk-agent-entrypoint: loop watchdog: armed (stops the session when the same tool call fails 2 times)\n" +
	"\x1b[0m\u2717 \x1b[0mRead internal/widget/adapter.go failed\x1b[90m [limit=2000]\x1b[0m\n" +
	"\x1b[91m\x1b[1mError: \x1b[0mFile not found: /workspace/internal/widget/adapter.go\n" +
	"\x1b[0m\u2717 \x1b[0mRead internal/widget/adapter.go failed\x1b[90m [limit=2000]\x1b[0m\n" +
	"gonk-agent-entrypoint: session end: opencode run exited rc=137\n" +
	"gonk-agent-entrypoint: loop watchdog: stopped opencode -- the same tool call failed 2 times (threshold 2): Read internal/widget/adapter.go [limit=2000]\n" +
	"gonk-agent-entrypoint: session end: runtime=jobs -- exiting 9 (loop watchdog) so the Job reports a terminal status\n"

const wantLoopCause = `cause="loop_watchdog: the same tool call failed 2 times (threshold 2): Read internal/widget/adapter.go [limit=2000]"`

// loopStoppedSession admits testSession as Failed with exit code `code` and
// the given output.
func loopStoppedSession(t *testing.T, rt *testRuntime, code int, output string) {
	t.Helper()
	h := rt.startNamed(t, testSession)
	rt.SetOutput(h, []byte(output))
	now := time.Now()
	rt.SetStatus(h, runtime.Status{
		Phase: runtime.PhaseFailed, Reason: "BackoffLimitExceeded",
		StartedAt: now.Add(-time.Minute), FinishedAt: now, ExitCode: &code,
	})
}

// classificationLine returns the one "sweep: classified" line in log, failing
// t unless there is exactly one.
func classificationLine(t *testing.T, log string) string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "sweep: classified ") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want exactly one classification line, got %d; log:\n%s", len(lines), log)
	}
	return lines[0]
}

// The bead's case, on the fake runtime: exit 9 with the watchdog's line and
// model spend -> gate-failed (reported to meter exactly like any gate-failed,
// so the ladder escalates), with the repeated call as the cause on the
// classification line.
func TestALoopWatchdogStopIsGateFailedAndNamesTheRepeatedCall(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{
		outcomeNext: "escalate", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 193983, AsOf: time.Now().Add(time.Hour)},
	})
	loopStoppedSession(t, rt, runtime.ExitLoopWatchdog, loopStoppedOutput)

	f.sweep(t)
	reqs := f.fm.requests()
	if len(reqs) != 1 {
		t.Fatalf("outcomes = %+v, want exactly one; log:\n%s", reqs, f.log.String())
	}
	got := reqs[0]
	if got.Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %q, want gate-failed: the loop is the model's behaviour, not infra; log:\n%s", got.Outcome, f.log.String())
	}
	// Not charged differently: the same request any gate-failed sends.
	if got.Attempt != f.rec.Attempt || got.Rung != f.rec.Rung || got.ReservationID != f.rec.ReservationID || got.BeadID != f.rec.BeadAnchor {
		t.Errorf("outcome request %+v does not carry the record's attempt/rung/reservation like any other verdict", got)
	}
	line := classificationLine(t, f.log.String())
	for _, want := range []string{"sweep: classified gate-failed", "session=" + testSession, wantLoopCause} {
		if !strings.Contains(line, want) {
			t.Errorf("classification line lacks %s:\n%s", want, line)
		}
	}
	if names := f.orders.names(); len(names) != 1 {
		t.Errorf("re-slings = %v, want one (escalate)", names)
	}
	// The re-sling status note (gonk-l900) is the broker's own text and is
	// expected; what must NOT exist is a note carrying a batch.
	if notes := f.applier.batchNotes(); len(notes) != 0 {
		t.Errorf("batch notes = %+v: a stopped session has no batch to apply", notes)
	}
}

// Exit 9 WITHOUT the watchdog's line is opencode's own rc, not the watchdog:
// still judged as usual, and the cause says the line was missing rather than
// inventing a repeated call.
func TestExitNineWithoutTheWatchdogLineIsNotBlamedOnALoop(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{
		outcomeNext: "escalate", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 500, AsOf: time.Now().Add(time.Hour)},
	})
	loopStoppedSession(t, rt, runtime.ExitLoopWatchdog, "some opencode output\nno batch\n")

	f.sweep(t)
	if got := f.oneOutcome(t); got != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %q, want gate-failed", got)
	}
	line := classificationLine(t, f.log.String())
	if strings.Contains(line, "loop_watchdog:") || !strings.Contains(line, "WITHOUT the entrypoint's loop-watchdog line") {
		t.Errorf("classification line should name the missing watchdog line, not a loop:\n%s", line)
	}
}

// The watchdog's exit does NOT bypass the proof every escalation needs: when
// meter's spend view is stale, a stopped session is infra-failed like any
// other (pkg/gate: an escalation must be paid for).
func TestALoopWatchdogStopWithStaleSpendIsStillInfraFailed(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry", spendSynced: true}) // AsOf zero: stale
	loopStoppedSession(t, rt, runtime.ExitLoopWatchdog, loopStoppedOutput)

	f.sweep(t)
	if got := f.oneOutcome(t); got != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %q, want infra-failed (spend not proven); log:\n%s", got, f.log.String())
	}
	if line := classificationLine(t, f.log.String()); !strings.Contains(line, `cause="spend_stale`) {
		t.Errorf("classification line: %s", line)
	}
}

// An ordinary gate-failed (not stopped by the watchdog) names the general
// cause, and never a loop.
func TestAnOrdinaryGateFailedNamesTheGeneralCause(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{
		outcomeNext: "escalate", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 900, AsOf: time.Now().Add(time.Hour)},
	})
	// Even with the watchdog's words in the log: exit 0 is not the watchdog.
	rt.finishSession(t, testSession, loopStoppedOutput)

	f.sweep(t)
	if got := f.oneOutcome(t); got != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %q, want gate-failed", got)
	}
	line := classificationLine(t, f.log.String())
	if !strings.Contains(line, `cause="no_valid_batch:`) || strings.Contains(line, "loop_watchdog") {
		t.Errorf("classification line: %s", line)
	}
}

func TestLoopWatchdogCauseReadsTheLastWatchdogLine(t *testing.T) {
	if _, ok := loopWatchdogCause([]byte("nothing here\n")); ok {
		t.Fatal("found a cause in output with no watchdog line")
	}
	// A line the MODEL printed is not at the start of a line with the
	// entrypoint's prefix, so it is not read.
	if _, ok := loopWatchdogCause([]byte("I say: gonk-agent-entrypoint: loop watchdog: stopped opencode -- the same tool call failed 9 times (threshold 2): x\n")); ok {
		t.Fatal("read a watchdog line that does not start the line")
	}
	cause, ok := loopWatchdogCause([]byte(strings.ReplaceAll(loopStoppedOutput, "\n", "\r\n")))
	if !ok || cause != "loop_watchdog: the same tool call failed 2 times (threshold 2): Read internal/widget/adapter.go [limit=2000]" {
		t.Fatalf("cause = %q, %v (CRLF line endings)", cause, ok)
	}
}
