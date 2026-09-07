package glab

import (
	"context"
	"fmt"
	"time"
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
	if !opts.UpdatedAfter.IsZero() {
		q["updated_after"] = opts.UpdatedAfter.UTC().Format(time.RFC3339)
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

// CloseIssue sets an issue's state to closed. It is the write behind the
// `close` triage verdict (gonk-aib), and it is deliberately the ONLY
// state-changing issue write the broker has: a model states the verdict, the
// broker decides what that verdict means and performs it, so no model-authored
// artifact ever closes anything by itself (ADR-007).
//
// state_event=close is idempotent in GitLab: closing an already-closed issue
// succeeds and changes nothing, so a re-sling cannot fail here.
func (c *Client) CloseIssue(ctx context.Context, projectID, issueIID int64) error {
	return c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d", projectID, issueIID),
		query:  map[string]string{"state_event": "close"},
	}, nil)
}

// UpdateIssueNote rewrites the body of a note the bot already posted. It exists
// for the canned-status comment (gonk-yrs): a deferred notice must be EDITED IN
// PLACE as the situation changes, and replaced by the real answer when one
// arrives, so a thread ends with the answer rather than a pile of stale
// apologies.
//
// Only ever called with a note id the bot itself created and recognised by its
// own marker -- GitLab will reject an edit of somebody else's note, but the
// caller should not rely on that as the guard.
func (c *Client) UpdateIssueNote(ctx context.Context, projectID, issueIID, noteID int64, body string) (*Note, error) {
	var n Note
	if err := c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d/notes/%d", projectID, issueIID, noteID),
		query:  map[string]string{"body": body},
	}, &n); err != nil {
		return nil, err
	}
	return &n, nil
}
