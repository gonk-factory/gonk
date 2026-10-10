package glab

import (
	"context"
	"fmt"
	"time"
)

// Issue label events (gonk-cuwn X-GLAB2, plan hypothesis H5). A GET.

// LabelEventUser is who made a label change.
type LabelEventUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// LabelRef is the label an event concerns. GitLab answers label: null for a
// label that has since been deleted, so LabelEvent.Label may be nil.
type LabelRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// LabelEvent is one add or remove of a label on an issue.
type LabelEvent struct {
	ID        int64          `json:"id"`
	User      LabelEventUser `json:"user"`
	CreatedAt time.Time      `json:"created_at"`
	// Action is "add" or "remove".
	Action string    `json:"action"`
	Label  *LabelRef `json:"label"`
}

// ListIssueLabelEvents returns EVERY resource label event of an issue (all
// pages), in the order GitLab returns them (the caller must not assume it, and
// should pick "newest" by CreatedAt then ID).
//
// Each event is asserted on: a positive id, a user.id, action add or remove,
// resource_type Issue, and a created_at. GitLab answers a missing issue as
// {"message":"404 ..."}; that cannot decode as an array and is an error, and an
// array of anything that is not an event fails the checks below.
func (c *Client) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error) {
	path := fmt.Sprintf("/api/v4/projects/%d/issues/%d/resource_label_events", projectID, issueIID)
	raw, err := paginate[struct {
		LabelEvent
		ResourceType string `json:"resource_type"`
	}](ctx, c, request{method: "GET", path: path})
	if err != nil {
		return nil, err
	}
	out := make([]LabelEvent, 0, len(raw))
	for _, r := range raw {
		e := r.LabelEvent
		switch {
		case e.ID <= 0:
			return nil, fmt.Errorf("gitlab: GET %s: event with id %d", path, e.ID)
		case e.User.ID <= 0:
			return nil, fmt.Errorf("gitlab: GET %s: event %d has no user.id", path, e.ID)
		case e.Action != "add" && e.Action != "remove":
			return nil, fmt.Errorf("gitlab: GET %s: event %d has action %q", path, e.ID, e.Action)
		case r.ResourceType != "Issue":
			return nil, fmt.Errorf("gitlab: GET %s: event %d has resource_type %q", path, e.ID, r.ResourceType)
		case e.CreatedAt.IsZero():
			return nil, fmt.Errorf("gitlab: GET %s: event %d has no created_at", path, e.ID)
		}
		out = append(out, e)
	}
	return out, nil
}
