package main

import (
	"context"
	"log/slog"
)

// checkArgs is what GC_WEBHOOK_ARG_* handed main.go for a `[steps.check]`
// invocation.
type checkArgs struct {
	ProjectID   int64
	IssueIID    int64
	BeadID      string
	Trigger     string
	BotUsername string
}

type checkDeps struct {
	GL   gitlabQuerier
	Log  *slog.Logger
	Args checkArgs
}

// runCheck is what a formula's [steps.check] executes. It is PURE and IDEMPOTENT:
// it asks GitLab one question -- "is the bot's comment carrying this bead's
// marker on the issue?" -- and answers with an exit code. NOTHING ELSE.
//
// It is a VERIFICATION LOOP body (formula-spec-v2: [steps.check]'s only mode is
// exec, and it is re-run until it passes or times out). So it MUST NOT POST an
// outcome, MUST NOT reserve budget, MUST NOT write to the bead store, and MUST
// NOT decide anything. Every one of those belongs to `gonk-gate sweep`, which
// runs once per bead.
//
// Exit: 0 = the artifact is there. 3 = not yet (keep polling). 1 = we could not
// ask GitLab (infra) -- and note that this is NOT the same as "not there", which
// is exactly the distinction the classifier is built on.
func runCheck(ctx context.Context, d checkDeps) int {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	a := d.Args

	kind, ok := artifactKindForTrigger[a.Trigger]
	if !ok {
		d.Log.Error("unknown trigger; refusing to guess an artifact kind", "trigger", a.Trigger)
		return 2
	}

	present, unknown, err := artifactPresent(ctx, d.GL, a.BotUsername, kind, a.ProjectID, a.IssueIID, a.BeadID)
	if unknown {
		d.Log.Error("could not ask GitLab whether the artifact landed", "err", err, "bead", a.BeadID)
		return 1
	}
	if !present {
		return 3
	}
	return 0
}
