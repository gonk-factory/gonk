package glab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	if b.Name != branch {
		return nil, fmt.Errorf("gitlab: POST branch %q: reply carries name %q", branch, b.Name)
	}
	return &b, nil
}

// DeleteBranch deletes a branch (DELETE .../repository/branches/:branch). The
// caller passes only a branch gonk created; this method does not know which
// those are. A 2xx other than 204 is an error: GitLab answers a delete with
// 204 and no body, so a 200 carrying JSON is something else answering. A branch
// that is already gone is an *APIError satisfying IsNotFound; a caller that
// wants delete-if-present checks for it.
func (c *Client) DeleteBranch(ctx context.Context, projectID int64, branch string) error {
	if branch == "" {
		return fmt.Errorf("gitlab: delete branch: empty branch name")
	}
	var status int
	_, _, err := c.do(ctx, request{
		method: "DELETE",
		path:   fmt.Sprintf("/api/v4/projects/%d/repository/branches/%s", projectID, url.PathEscape(branch)),
		consume: func(resp *http.Response) error {
			status = resp.StatusCode
			return nil
		},
	})
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("gitlab: DELETE branch %q: status %d, want 204", branch, status)
	}
	return nil
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
	// The id is recorded as the fix commit (FixCommitSHA), so a reply that is
	// not a full commit id must not become one.
	if !IsCommitSHA(cm.ID) {
		return nil, fmt.Errorf("gitlab: POST commit on %q: reply id %q is not a commit SHA", opts.Branch, cm.ID)
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
	if !opts.UpdatedAfter.IsZero() {
		q["updated_after"] = opts.UpdatedAfter.UTC().Format(time.RFC3339)
	}
	if opts.TargetBranch != "" {
		q["target_branch"] = opts.TargetBranch
	}
	if opts.AuthorID != 0 {
		q["author_id"] = strconv.FormatInt(opts.AuthorID, 10)
	}
	return paginate[MergeRequest](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests", projectID),
		query:  q,
	})
}

// CreateMergeRequest opens one MR. The reply must carry a nonzero iid, the
// source and target branch asked for, and every label asked for; a 2xx that
// does not is an error, not an MR. Labels containing a comma are refused before
// any request: GitLab may split a label on a comma, which would apply labels
// the caller never named.
func (c *Client) CreateMergeRequest(ctx context.Context, projectID int64, opts MROptions) (*MergeRequest, error) {
	for _, l := range opts.Labels {
		if l == "" || strings.Contains(l, ",") {
			return nil, fmt.Errorf("gitlab: create merge request: label %q is empty or contains a comma", l)
		}
	}
	var mr MergeRequest
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests", projectID),
		body:   opts,
	}, &mr); err != nil {
		return nil, err
	}
	if mr.IID == 0 {
		return nil, fmt.Errorf("gitlab: POST merge request %q: reply carries no iid", opts.SourceBranch)
	}
	if mr.SourceBranch != opts.SourceBranch || mr.TargetBranch != opts.TargetBranch {
		return nil, fmt.Errorf("gitlab: POST merge request %q -> %q: reply carries %q -> %q",
			opts.SourceBranch, opts.TargetBranch, mr.SourceBranch, mr.TargetBranch)
	}
	if missing := missingLabels(mr.Labels, opts.Labels); len(missing) > 0 {
		return nil, fmt.Errorf("gitlab: POST merge request %q: reply lacks label(s) %q", opts.SourceBranch, missing)
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
	if opts.AuthorID != 0 {
		q["author_id"] = strconv.FormatInt(opts.AuthorID, 10)
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
//
// Sent as a JSON body carrying add_labels as a ONE-ELEMENT ARRAY, not the
// query-string form (`?add_labels=<value>`) this used to send. GitLab treats
// a query-string add_labels as a comma-separated list, so a label value that
// happened to contain a comma silently applied more labels than the caller
// named -- one label effect from an agent-authored batch could apply two
// labels. GitLab's array form has no such splitting: each array element is
// applied verbatim as one label (T-05, closes R-02).
func (c *Client) AddIssueLabel(ctx context.Context, projectID, issueIID int64, label string) error {
	return c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d", projectID, issueIID),
		body:   map[string][]string{"add_labels": {label}},
	}, nil)
}

// CreateIssueNote posts one comment (a "note") to an issue and returns the note
// GitLab created, so the caller can read back its id. It is the broker's
// comment-apply write: the read side (ListIssueNotes, notes.go) scans notes for
// the bot's marker; this is how the bot posts one. Like AddIssueLabel it takes
// the numeric project id and the issue IID, not a path.
//
// Sent as a JSON body, not the query-string form (`?body=<value>`) this used
// to send (T-06, closes R-16): a comment body is unbounded model output, and
// the query-string form has no room for one anywhere near the 64 KiB the
// broker now allows -- a real comment routinely exceeds what a URL can carry
// before hitting a proxy's/GitLab's own request-line limit (414), which is a
// silent apply failure with no relation to whether the comment was valid.
// JSON has no such ambient limit. This also keeps the comment text out of
// access/proxy logs that record request lines but not bodies.
func (c *Client) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (*Note, error) {
	var n Note
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d/notes", projectID, issueIID),
		body:   map[string]string{"body": body},
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
//
// Sent as a JSON body, not the query-string form this used to send (T-06,
// closes R-16), for the same reason CreateIssueNote is: the edited body is the
// same unbounded, up-to-64-KiB model output, and query-string encoding has no
// room for it.
func (c *Client) UpdateIssueNote(ctx context.Context, projectID, issueIID, noteID int64, body string) (*Note, error) {
	var n Note
	if err := c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d/notes/%d", projectID, issueIID, noteID),
		body:   map[string]string{"body": body},
	}, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// UpdateIssue applies a partial edit to an issue (PUT .../issues/:iid) as a JSON
// body: title/description only when set, labels via add_labels/remove_labels
// arrays. It is how a gonk-owned tracking issue's description and labels are
// kept current in place. With nothing to change it sends no request and
// returns (nil, nil). The reply's iid must equal issueIID.
func (c *Client) UpdateIssue(ctx context.Context, projectID, issueIID int64, u IssueUpdate) (*Issue, error) {
	body := map[string]any{}
	if u.Title != nil {
		body["title"] = *u.Title
	}
	if u.Description != nil {
		body["description"] = *u.Description
	}
	if len(u.AddLabels) > 0 {
		body["add_labels"] = u.AddLabels
	}
	if len(u.RemoveLabels) > 0 {
		body["remove_labels"] = u.RemoveLabels
	}
	if len(body) == 0 {
		return nil, nil
	}
	var is Issue
	if err := c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/issues/%d", projectID, issueIID),
		body:   body,
	}, &is); err != nil {
		return nil, err
	}
	if is.IID != issueIID {
		return nil, fmt.Errorf("gitlab: PUT issue %d: reply carries iid %d", issueIID, is.IID)
	}
	return &is, nil
}
