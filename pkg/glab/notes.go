package glab

import (
	"context"
	"fmt"
)

// Note is one comment on an issue (GET .../issues/:iid/notes). gonk-gate's
// deterministic gate (check/sweep) reads these to find the bot's
// marker-carrying comment -- see pkg/gate.BeadMarker. It is a fact query, not
// a judgement: the gate asks "did the bot post the marker", never "does this
// comment look right".
type Note struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	Author User   `json:"author"`
	// System notes are GitLab's own audit trail (label changes, state
	// transitions, etc.), never authored text. A gate must not mistake one
	// for the bot's comment.
	System bool `json:"system"`
}

// ListIssueNotes returns every note on the issue, oldest first (GitLab's
// default order), for the deterministic gate to scan for a marker.
func (c *Client) ListIssueNotes(ctx context.Context, projectID, issueIID int64) ([]Note, error) {
	return paginate[Note](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d/notes", projectID, issueIID),
	})
}

// GetIssue fetches one issue by IID -- used by the sweeper's Aborted signal
// (a human closed the issue).
func (c *Client) GetIssue(ctx context.Context, projectID, issueIID int64) (*Issue, error) {
	var is Issue
	if err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d", projectID, issueIID),
	}, &is); err != nil {
		return nil, err
	}
	return &is, nil
}
