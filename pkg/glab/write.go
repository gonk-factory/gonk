package glab

import (
	"context"
	"fmt"
)

// Each method here is getJSON (or paginate) over a request{...}, exactly like
// CurrentUser / ListMemberProjects in projects.go. No retries beyond the
// shared policy; no logging.

func (c *Client) CreateBranch(ctx context.Context, projectID int64, branch, ref string) (*Branch, error) {
	var b Branch
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/repository/branches", projectID),
		query:  map[string]string{"branch": branch, "ref": ref},
	}, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *Client) CreateCommit(ctx context.Context, projectID int64, opts CommitOptions) (*Commit, error) {
	var cm Commit
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/repository/commits", projectID),
		body:   opts,
	}, &cm); err != nil {
		return nil, err
	}
	return &cm, nil
}

func (c *Client) ListMergeRequests(ctx context.Context, projectID int64, opts MRListOptions) ([]MergeRequest, error) {
	q := map[string]string{}
	if opts.SourceBranch != "" {
		q["source_branch"] = opts.SourceBranch
	}
	if opts.State != "" {
		q["state"] = opts.State
	}
	return paginate[MergeRequest](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests", projectID),
		query:  q,
	})
}

func (c *Client) CreateMergeRequest(ctx context.Context, projectID int64, opts MROptions) (*MergeRequest, error) {
	var mr MergeRequest
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests", projectID),
		body:   opts,
	}, &mr); err != nil {
		return nil, err
	}
	return &mr, nil
}

func (c *Client) ListIssues(ctx context.Context, projectID int64, opts IssueListOptions) ([]Issue, error) {
	q := map[string]string{}
	if opts.State != "" {
		q["state"] = opts.State
	}
	if opts.Labels != "" {
		q["labels"] = opts.Labels
	}
	return paginate[Issue](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues", projectID),
		query:  q,
	})
}

func (c *Client) CreateIssue(ctx context.Context, projectID int64, opts IssueOptions) (*Issue, error) {
	var is Issue
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues", projectID),
		body:   opts,
	}, &is); err != nil {
		return nil, err
	}
	return &is, nil
}

// AddIssueLabel idempotently adds one label to an issue via the `add_labels`
// parameter of the issue-edit endpoint: GitLab treats re-adding a label
// already present as a no-op, so no read-before-write is needed. Used by
// intake's Gate-1 deny path (its ONLY GitLab write on the dispatch path).
func (c *Client) AddIssueLabel(ctx context.Context, projectID, issueIID int64, label string) error {
	return c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d", projectID, issueIID),
		query:  map[string]string{"add_labels": label},
	}, nil)
}

// CreateIssueNote posts one comment (a "note") to an issue and returns the note
// GitLab created, so the caller can read back its id. It is the broker's
// comment-apply write: the read side (ListIssueNotes, notes.go) scans notes for
// the bot's marker; this is how the bot posts one. Like AddIssueLabel it takes
// the numeric project id and the issue IID, not a path.
func (c *Client) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (*Note, error) {
	var n Note
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d/notes", projectID, issueIID),
		query:  map[string]string{"body": body},
	}, &n); err != nil {
		return nil, err
	}
	return &n, nil
}
