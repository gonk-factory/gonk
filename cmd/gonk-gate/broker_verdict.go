package main

import (
	"context"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/effects"
)

// Labels the broker applies to record what a verdict MEANT. They are written by
// the controller, not proposed by the agent, so they are trustworthy in a way an
// agent-proposed label is not: a human filtering on gonk::needs-maintainer is
// reading gonk's own decision, not a model's suggestion.
const (
	labelNeedsMaintainer = "gonk::needs-maintainer"
	labelFixQueued       = "gonk::fix-queued"
)

// verdictLabel is the audit trail for what gonk concluded. The closed set is
// mirrored here deliberately rather than interpolated, so a new verdict cannot
// silently produce a new label nobody is filtering on.
func verdictLabel(v effects.Verdict) string {
	switch v {
	case effects.VerdictCodeChange:
		return "gonk::verdict-code-change"
	case effects.VerdictClose:
		return "gonk::verdict-close"
	default:
		return "gonk::verdict-reply-only"
	}
}

// applyVerdict performs the deterministic action a validated verdict names.
//
// THE SPLIT MATTERS: the model states a CONCLUSION and this function decides
// what that conclusion is allowed to DO (ADR-007). No model closes an issue or
// opens a merge request; it says "close" or "code-change" and the broker, which
// holds the credentials and the policy, acts. That is why the verdict is
// validated against a closed set before it ever reaches here.
//
// Called AFTER comments and labels are applied, so the reporter always has the
// explanation in hand before the issue changes state underneath them.
//
// Errors are returned only for things worth retrying. A verdict action that
// fails after the comment is already posted must not re-run the whole batch --
// that would double-post -- so those are logged and swallowed, and the comment,
// which is the part the human reads, stands.
func applyVerdict(ctx context.Context, d sweepDeps, rec beadstore.Record, batch effects.Batch) {
	v := batch.EffectiveVerdict()
	// RECORD THE ROUTING DECISION WHERE HUMANS LOOK (gonk-kxg: "the
	// classification must be recorded ... so the routing decision is auditable
	// after the fact"). A label is the cheapest durable record that survives log
	// rotation and is filterable in the GitLab UI, and it is written by the
	// BROKER, so it states what gonk actually did rather than what a model
	// suggested.
	label(ctx, d, rec, verdictLabel(v))
	switch v {
	case effects.VerdictReplyOnly:
		// The comment IS the action. Nothing further, and nothing to log beyond
		// the batch-applied line the caller already writes.
		return

	case effects.VerdictClose:
		if err := d.Apply.CloseIssue(ctx, rec.ProjectID, rec.IssueIID); err != nil {
			// The reporter has the reasoning; the issue merely stayed open. Worth
			// a warning, not worth re-running the batch and double-posting.
			d.Log.Warn("sweep: verdict close, but the issue would not close (comment already posted)",
				"bead", rec.BeadAnchor, "err", err)
			return
		}
		d.Log.Info("sweep: verdict close applied", "bead", rec.BeadAnchor)

	case effects.VerdictCodeChange:
		applyCodeChangeVerdict(ctx, d, rec)

	default:
		// Unreachable: ParseBatch rejects unknown verdicts. Kept so that adding a
		// verdict without teaching the broker what it means is loud rather than
		// silent.
		d.Log.Error("sweep: no action defined for verdict -- the batch was applied but its conclusion was ignored",
			"bead", rec.BeadAnchor, "verdict", string(v))
	}
}

// applyCodeChangeVerdict routes an identified defect according to the PROJECT'S
// POLICY, not the model's preference.
//
// actions.features is the existing knob for "may gonk propose code here". When
// it is off, the agent's finding must still reach a human: an identified fix
// that nobody is told about is worse than no triage at all, because the project
// now believes it was triaged. So the finding is labelled for a maintainer
// queue rather than dropped.
func applyCodeChangeVerdict(ctx context.Context, d sweepDeps, rec beadstore.Record) {
	allowed, known := codeChangesAllowed(ctx, d, rec)
	if !known {
		// FAIL CLOSED. Not knowing the policy is not permission to act on the
		// repository; route it to a human exactly as a "no" would.
		d.Log.Warn("sweep: could not read actions.features; treating code-change as maintainer-only",
			"bead", rec.BeadAnchor)
		label(ctx, d, rec, labelNeedsMaintainer)
		return
	}
	if !allowed {
		d.Log.Info("sweep: verdict code-change, but actions.features is off -- routed to a maintainer",
			"bead", rec.BeadAnchor)
		label(ctx, d, rec, labelNeedsMaintainer)
		return
	}
	// features=true: the project has opted in to gonk proposing code. The fix
	// run is a SEPARATE metered dispatch -- it needs its own prompt, its own
	// rung and its own budget decision, and doing it inline here would spend
	// tokens the meter never authorised.
	d.Log.Info("sweep: verdict code-change with actions.features on -- queued for a fix run",
		"bead", rec.BeadAnchor)
	label(ctx, d, rec, labelFixQueued)
}

// codeChangesAllowed asks the meter for the project's resolved policy. The
// second return distinguishes "policy says no" from "we could not find out",
// which must not be conflated: one is a decision, the other is ignorance, and
// only the first is safe to treat as final.
func codeChangesAllowed(ctx context.Context, d sweepDeps, rec beadstore.Record) (allowed, known bool) {
	if d.Meter == nil || rec.Project == "" {
		return false, false
	}
	resp, err := d.Meter.Project(ctx, rec.Project)
	if err != nil || resp == nil || resp.Effective == nil {
		return false, false
	}
	return resp.Effective.Actions.Features, true
}

// label applies one broker-authored label, logging rather than failing: by the
// time verdict actions run the comment is already posted, and a missing label
// is not worth re-running a batch over.
func label(ctx context.Context, d sweepDeps, rec beadstore.Record, name string) {
	if err := d.Apply.AddIssueLabel(ctx, rec.ProjectID, rec.IssueIID, name); err != nil {
		d.Log.Warn("sweep: verdict label apply failed", "bead", rec.BeadAnchor, "label", name, "err", err)
	}
}

// labelPrefix is the project's configured label namespace, or the shipped
// default when the meter cannot tell us.
//
// Falling back to the default rather than to "" matters: an empty prefix
// disables normalisation entirely, so a meter blip would silently let a run's
// labels escape the namespace -- the exact defect being fixed.
func labelPrefix(ctx context.Context, d sweepDeps, rec beadstore.Record) string {
	const shippedDefault = "gonk::"
	if d.Meter == nil || rec.Project == "" {
		return shippedDefault
	}
	resp, err := d.Meter.Project(ctx, rec.Project)
	if err != nil || resp == nil || resp.Effective == nil || resp.Effective.Triage.LabelPrefix == "" {
		return shippedDefault
	}
	return resp.Effective.Triage.LabelPrefix
}
