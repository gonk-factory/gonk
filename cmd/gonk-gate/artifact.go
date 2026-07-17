package main

import (
	"context"
	"fmt"

	"gitlab.orac.local/agentic/gonk-project/pkg/gate"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// gitlabQuerier is the slice of pkg/glab that the deterministic gate needs:
// read the issue's notes to look for the bot's marker comment, read the
// issue's state, and read merge requests from a branch for the scaffold
// trigger's MR-shaped artifact. It is satisfied by *glab.Client directly (its
// method set is a superset) and by a fake in tests -- no adapter needed.
type gitlabQuerier interface {
	ListIssueNotes(ctx context.Context, projectID, issueIID int64) ([]glab.Note, error)
	GetIssue(ctx context.Context, projectID, issueIID int64) (*glab.Issue, error)
	ListMergeRequests(ctx context.Context, projectID int64, opts glab.MRListOptions) ([]glab.MergeRequest, error)
}

// artifactKindForTrigger says which artifact a trigger's gate looks for.
// scaffold's artifact is a merge request (spec 5.3: "a second, post-merge MR");
// every other trigger's artifact is a marker-carrying issue comment.
var artifactKindForTrigger = map[string]string{
	"issue-triage":  "comment",
	"mention-reply": "comment",
	"scaffold":      "mr",
}

const scaffoldSourceBranch = "gonk/scaffold"

// artifactPresent asks GitLab ONE question: is beadID's marker on the
// declared artifact for this trigger? It never guesses: a query failure
// returns unknown=true, never present=false -- absence of evidence is not
// evidence of a gate failure (an outage of OURS must never buy an
// escalation, AD-6).
func artifactPresent(ctx context.Context, gl gitlabQuerier, botUsername, kind string, projectID, issueIID int64, beadID string) (present, unknown bool, err error) {
	switch kind {
	case "mr":
		mrs, qerr := gl.ListMergeRequests(ctx, projectID, glab.MRListOptions{SourceBranch: scaffoldSourceBranch, State: "all"})
		if qerr != nil {
			return false, true, qerr
		}
		for _, mr := range mrs {
			if gate.MarkerPresent(mr.Description, beadID) {
				return true, false, nil
			}
		}
		return false, false, nil
	case "comment":
		notes, qerr := gl.ListIssueNotes(ctx, projectID, issueIID)
		if qerr != nil {
			return false, true, qerr
		}
		for _, n := range notes {
			if n.System {
				continue // GitLab's own audit trail, never authored text
			}
			if n.Author.Username != botUsername {
				continue // a human quoting the marker must not satisfy the gate
			}
			if gate.MarkerPresent(n.Body, beadID) {
				return true, false, nil
			}
		}
		return false, false, nil
	default:
		return false, true, fmt.Errorf("gonk-gate: unknown artifact kind %q", kind)
	}
}
