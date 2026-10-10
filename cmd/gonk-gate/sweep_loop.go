package main

import (
	"context"
	"fmt"
	"regexp"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// A SESSION THE LOOP WATCHDOG STOPPED (gonk-un7z).
//
// In jobs mode the agent's entrypoint passes opencode's output through a line
// filter that kills opencode the moment the SAME tool call fails for the
// second time (images/agent/entrypoint.sh, gonk_run_opencode_watched), and
// then exits runtime.ExitLoopWatchdog after logging the repeated call. The
// tokens a looping model would have burned are therefore never spent; this
// file only makes the verdict SAY why.
//
// THE VERDICT ITSELF IS NOT SPECIAL-CASED. A stopped session is Failed, has no
// batch (it was killed before emitting one) and has model spend (it made the
// calls that failed), so gate.Classify already answers gate-failed through the
// ordinary path: the model's behaviour, not infra, and the ladder escalates as
// for any bad answer. Deliberately so -- forcing gate-failed from the exit
// code would let a session reach an escalation WITHOUT the model-spend proof
// that pkg/gate requires of every escalation ("an escalation must be paid
// for"). If meter's spend view is stale, it stays infra-failed, as for any
// other session. What this file adds is the cause on the classification line.

// loopWatchdogLine is the entrypoint's line naming the repeated call. The
// exit code alone is not proof -- jobs mode passes opencode's own rc through,
// and opencode could exit 9 -- the line is written only by the watchdog.
var loopWatchdogLine = regexp.MustCompile(`(?m)^gonk-agent-entrypoint: loop watchdog: stopped opencode -- the same tool call failed (\d+) times \(threshold (\d+)\): (.*?)\r?$`)

// isLoopWatchdogExit: did the session end with the watchdog's exit code?
func isLoopWatchdogExit(st *runtime.Status) bool {
	return st != nil && st.Phase == runtime.PhaseFailed && st.ExitCode != nil && *st.ExitCode == runtime.ExitLoopWatchdog
}

// loopWatchdogCause names the repeated call from the session's output, or
// reports false when the output has no watchdog line (the last one wins).
func loopWatchdogCause(out []byte) (string, bool) {
	all := loopWatchdogLine.FindAllSubmatch(out, -1)
	if len(all) == 0 {
		return "", false
	}
	m := all[len(all)-1]
	return fmt.Sprintf("loop_watchdog: the same tool call failed %s times (threshold %s): %s", m[1], m[2], m[3]), true
}

// gateFailedCause names what made gate.Classify answer gate-failed for this
// session: the loop watchdog's repeated call when it stopped the session,
// otherwise the general case (the model answered and nothing valid was
// applied -- the violation, when there was one, is on the "batch rejected"
// line just before).
func gateFailedCause(ctx context.Context, d sweepDeps, rec beadstore.Record, judged *runtime.Status) string {
	const general = "no_valid_batch: the model answered (meter recorded its spend) and no valid batch was applied"
	if !isLoopWatchdogExit(judged) {
		return general
	}
	out, err := d.Runtime.Output(ctx, runtime.Handle(rec.SessionID))
	if err != nil {
		return fmt.Sprintf("loop_watchdog: exit %d, but the output naming the repeated call is unreadable: %v",
			runtime.ExitLoopWatchdog, err)
	}
	if cause, ok := loopWatchdogCause(out); ok {
		return cause
	}
	return fmt.Sprintf("%s (exit %d WITHOUT the entrypoint's loop-watchdog line: opencode's own rc, not the watchdog)",
		general, runtime.ExitLoopWatchdog)
}
