package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/trace"
)

// checkTrajectory is the FIFTH GATE (gonk-hsb), joining ParseBatch, Validate,
// ValidateTargets and ValidatePaths. It asks the one question the other four
// cannot: not "is this batch well-formed" but "did the session do the work it
// is claiming to report".
//
// IT IS OBSERVING-ONLY UNTIL enforceTrajectory IS TURNED ON, and that is the
// bead's own sequencing rule, not caution for its own sake: a predicate enabled
// on unmeasured evidence rejects honest batches, and a rejected batch on the
// triage path re-slings the bead onto a pricier rung. We have exactly one real
// trace so far. Collect, watch the verdicts in the log, and switch this on when
// the false-positive rate is known rather than assumed.
//
// Returns a violation string when it would reject, "" otherwise. The caller
// decides whether to act on it.
func checkTrajectory(ctx context.Context, d sweepDeps, rec beadstore.Record, agent string, batch effects.Batch) string {
	if d.Meter == nil || rec.SessionKey == "" {
		return ""
	}
	policy, err := trace.LoadPolicy(d.PackDir, agent)
	if err != nil {
		// OUR misconfiguration, not the agent's. Never let a broken policy file
		// reject somebody's work.
		d.Log.Warn("trajectory: could not load policy; not judging this session",
			"bead", rec.BeadAnchor, "agent", agent, "err", err)
		return ""
	}
	if policy.Empty() {
		return ""
	}

	view, err := d.Meter.GetTrace(ctx, rec.SessionKey, rec.Attempt)
	if err != nil || view == nil {
		// We could not ASK. That is not the same as observing nothing, and it
		// must never look like a verdict.
		d.Log.Warn("trajectory: could not read evidence; not judging this session",
			"bead", rec.BeadAnchor, "err", err)
		return ""
	}

	calls := make([]trace.Call, 0, len(view.Calls))
	for _, c := range view.Calls {
		calls = append(calls, trace.Call{Tool: c.Tool, Target: c.Target})
	}
	tr := trace.Trace{
		SessionKey:   view.SessionKey,
		Attempt:      view.Attempt,
		Completeness: trace.Completeness(view.Completeness),
		Calls:        calls,
		Turns:        view.Turns,
	}
	verdict := trace.Classify(trace.Input{
		Trace:   tr,
		Verdict: trajectoryClaim(agent, batch),
	}, policy)

	d.Log.Info("trajectory: judged",
		"bead", rec.BeadAnchor, "agent", agent,
		"outcome", string(verdict.Outcome), "reason", verdict.Reason,
		"completeness", view.Completeness, "calls", len(view.Calls),
		"tools", toolNamesAttr(tr), "turns", view.Turns,
		"enforcing", d.EnforceTrajectory)

	if !verdict.Rejects() {
		return ""
	}
	if !d.EnforceTrajectory {
		// The whole point of the observing phase: say loudly what WOULD have
		// been rejected, and apply the batch anyway.
		d.Log.Warn("trajectory: WOULD REJECT this batch, but enforcement is off",
			"bead", rec.BeadAnchor, "reason", verdict.Reason)
		return ""
	}
	return "trajectory: " + verdict.Reason
}

// trajectoryClaim is what the trajectory policy's require_any_read_for is
// matched against: the batch's verdict for triage, and for the pipeline agent
// its diagnosis's CLASSIFICATION (gonk-tios D8 step 9) -- the classes code,
// test and configuration claim something about repository content, so a
// session asserting one without reading a file has not looked. A pipeline
// batch has no verdict, so matching the verdict would make the check inert.
func trajectoryClaim(agent string, batch effects.Batch) string {
	if agent == pipelineAgent {
		for _, e := range batch.Effects {
			if e.Kind == effects.KindDiagnosis {
				return string(e.Classification)
			}
		}
	}
	return string(batch.EffectiveVerdict())
}

// maxLoggedToolNames bounds the `tools` attribute of the judged verdict.
//
// A tool name is what the MODEL called the tool (pkg/trace.Call.Tool), so the
// set of distinct names is not closed by anything gonk owns -- a session that
// invents a new name per call would otherwise write an unbounded log line. That
// matters here specifically: this record goes out through defaultLogSink, a
// write to PID 1's stdout pipe, where only writes under PIPE_BUF (4096) are
// atomic and a longer record can TEAR (logsink.go). 24 names of at most 24
// bytes each is ~600 bytes, which keeps the whole record comfortably inside
// that, and 24 distinct tools is already far more than any agent pack declares.
const maxLoggedToolNames = 24

// toolNamesAttr is the DISTINCT, sorted tool names of a trace as one greppable
// log field -- "bash,edit,read" (gonk-u0z0).
//
// WHY THE NAMES AND NOTHING ELSE. The verdict line used to carry only
// len(view.Calls), so a 14-day Loki sweep for which tools agents actually call
// returned zero named invocations and gonk-7b79's permission narrowing had to
// rest on design evidence instead of measurement. The names answer that; the
// TARGETS would not, and they are exactly what must not be here. Call.Target is
// a normalised path or issue reference derived from model output over untrusted
// issue text, and the rule this follows is sessionViewAttrs' (broker_apply.go):
// a log attribute carries values whose domain we control, never a field that
// can hold somebody else's prose.
//
// Distinct rather than the observed sequence: the sequence is unbounded in a
// long run and nothing greps for it, while the SET is what every question asked
// of this data so far has been -- "does anything call grep", "which tools does
// triage use". Sorted because an unordered set rendered in observation order
// makes two identical sessions log two different lines.
//
// The names are still model-supplied strings, so they go through the package's
// existing log-field sanitiser (sanitizeRole, transcript_archive.go) rather
// than a second one: a tool name is the same kind of value as a transcript
// role -- upstream-named, destined for a log field -- and must not be able to
// put a control byte, a quote or a comma into it.
func toolNamesAttr(tr trace.Trace) string {
	names := tr.Tools()
	extra := 0
	if len(names) > maxLoggedToolNames {
		extra = len(names) - maxLoggedToolNames
		names = names[:maxLoggedToolNames]
	}
	parts := make([]string, 0, len(names)+1)
	for _, n := range names {
		parts = append(parts, sanitizeRole(n))
	}
	if extra > 0 {
		parts = append(parts, fmt.Sprintf("+%d-more", extra))
	}
	return strings.Join(parts, ",")
}
