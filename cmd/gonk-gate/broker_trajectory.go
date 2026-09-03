package main

import (
	"context"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/effects"
	"gitlab.orac.local/agentic/gonk-project/pkg/trace"
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
	verdict := trace.Classify(trace.Input{
		Trace: trace.Trace{
			SessionKey:   view.SessionKey,
			Attempt:      view.Attempt,
			Completeness: trace.Completeness(view.Completeness),
			Calls:        calls,
			Turns:        view.Turns,
		},
		Verdict: string(batch.EffectiveVerdict()),
	}, policy)

	d.Log.Info("trajectory: judged",
		"bead", rec.BeadAnchor, "agent", agent,
		"outcome", string(verdict.Outcome), "reason", verdict.Reason,
		"completeness", view.Completeness, "calls", len(view.Calls), "turns", view.Turns,
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
