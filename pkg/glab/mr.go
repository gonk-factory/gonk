package glab

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Merge-request reads and writes for pipeline triage (gonk-tios): the MR is
// where an author looks, so the diagnosis lands as ONE note edited in place and
// a class label. Like CreateIssueNote these send JSON bodies, never
// query-string bodies (see AddIssueLabel for why).

// GetMergeRequest fetches one MR. The returned iid must equal the one asked for.
func (c *Client) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (*MergeRequest, error) {
	var mr MergeRequest
	if err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d", projectID, mrIID),
	}, &mr); err != nil {
		return nil, err
	}
	if mr.IID != mrIID {
		return nil, fmt.Errorf("gitlab: GET merge request %d: reply carries iid %d", mrIID, mr.IID)
	}
	return &mr, nil
}

// ListMRNotes returns every note on the MR, oldest first, system notes
// included (check Note.System).
func (c *Client) ListMRNotes(ctx context.Context, projectID, mrIID int64) ([]Note, error) {
	return paginate[Note](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/notes", projectID, mrIID),
		query:  map[string]string{"order_by": "created_at", "sort": "asc"},
	})
}

// CreateMRNote posts one note on an MR. The reply must carry a note id.
func (c *Client) CreateMRNote(ctx context.Context, projectID, mrIID int64, body string) (*Note, error) {
	var n Note
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/notes", projectID, mrIID),
		body:   map[string]string{"body": body},
	}, &n); err != nil {
		return nil, err
	}
	if n.ID == 0 {
		return nil, fmt.Errorf("gitlab: POST merge request %d note: reply carries no note id", mrIID)
	}
	return &n, nil
}

// UpdateMRNote rewrites the body of a note the bot already posted. The reply's
// id must equal noteID.
func (c *Client) UpdateMRNote(ctx context.Context, projectID, mrIID, noteID int64, body string) (*Note, error) {
	var n Note
	if err := c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/notes/%d", projectID, mrIID, noteID),
		body:   map[string]string{"body": body},
	}, &n); err != nil {
		return nil, err
	}
	if n.ID != noteID {
		return nil, fmt.Errorf("gitlab: PUT merge request %d note %d: reply carries note id %d", mrIID, noteID, n.ID)
	}
	return &n, nil
}

// UpdateMRLabels adds and removes labels in one edit (add_labels /
// remove_labels, JSON arrays so a label containing a comma is one label).
// With nothing to add or remove it sends NO request and returns (nil, nil);
// callers that need the MR then use GetMergeRequest. Otherwise the reply must
// carry iid == mrIID, every added label, and none of the removed ones.
func (c *Client) UpdateMRLabels(ctx context.Context, projectID, mrIID int64, add, remove []string) (*MergeRequest, error) {
	return c.UpdateMergeRequest(ctx, projectID, mrIID, MRUpdate{AddLabels: add, RemoveLabels: remove})
}

// UpdateMergeRequest applies a partial edit to an MR as a JSON body
// (description only when set; labels via add_labels/remove_labels arrays). It
// is how a gonk-owned fix MR's description and labels are kept current. With
// nothing to change it sends no request and returns (nil, nil).
//
// The reply is validated by the fields it was asked to change, never by being
// well-formed JSON: iid must equal mrIID; a set Description must come back
// (modulo CRLF and surrounding whitespace, which GitLab normalises); every
// added label must be in Labels and no removed label may be.
func (c *Client) UpdateMergeRequest(ctx context.Context, projectID, mrIID int64, u MRUpdate) (*MergeRequest, error) {
	body := map[string]any{}
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
	var mr MergeRequest
	if err := c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d", projectID, mrIID),
		body:   body,
	}, &mr); err != nil {
		return nil, err
	}
	if mr.IID != mrIID {
		return nil, fmt.Errorf("gitlab: PUT merge request %d: reply carries iid %d", mrIID, mr.IID)
	}
	if u.Description != nil && normText(mr.Description) != normText(*u.Description) {
		return nil, fmt.Errorf("gitlab: PUT merge request %d: reply description does not match the one sent", mrIID)
	}
	if missing := missingLabels(mr.Labels, u.AddLabels); len(missing) > 0 {
		return nil, fmt.Errorf("gitlab: PUT merge request %d: reply lacks added label(s) %q", mrIID, missing)
	}
	if kept := presentLabels(mr.Labels, u.RemoveLabels); len(kept) > 0 {
		return nil, fmt.Errorf("gitlab: PUT merge request %d: reply still carries removed label(s) %q", mrIID, kept)
	}
	return &mr, nil
}

// AcceptMR merges the MR ONLY if its head is still sha (PUT .../merge with
// sha=<head>; GitLab refuses with 409 when the head moved). sha must be a full
// commit id: an empty or abbreviated one would send no pin at all, and a merge
// without the pin is exactly what this call exists to prevent. It never sets
// merge_when_pipeline_succeeds and never retries a 5xx (the caller re-reads the
// MR to learn whether the merge happened).
//
// A 2xx is not a merge. The reply must carry iid == mrIID, state "merged" and
// sha == the sha asked for; anything else is an error, however well-formed.
// A refusal is an *APIError: 405/406 (not mergeable now), 409 (head moved, see
// IsShaMismatch), 401/403 (no permission).
func (c *Client) AcceptMR(ctx context.Context, projectID, mrIID int64, sha string) (*MergeRequest, error) {
	if !IsCommitSHA(sha) {
		return nil, fmt.Errorf("gitlab: accept merge request %d: %q is not a full commit SHA", mrIID, sha)
	}
	var mr MergeRequest
	if err := c.getJSON(ctx, request{
		method:  "PUT",
		path:    fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/merge", projectID, mrIID),
		body:    map[string]string{"sha": sha},
		noRetry: true,
	}, &mr); err != nil {
		return nil, err
	}
	if mr.IID != mrIID {
		return nil, fmt.Errorf("gitlab: PUT merge request %d merge: reply carries iid %d", mrIID, mr.IID)
	}
	if mr.State != "merged" {
		return nil, fmt.Errorf("gitlab: PUT merge request %d merge: reply state is %q, not merged", mrIID, mr.State)
	}
	if mr.SHA != sha {
		return nil, fmt.Errorf("gitlab: PUT merge request %d merge: reply head sha %q is not the %q asked for", mrIID, mr.SHA, sha)
	}
	return &mr, nil
}

// IsShaMismatch reports that GitLab refused a merge because the MR head is no
// longer the sha the merge pinned (409).
func IsShaMismatch(err error) bool {
	ae := asAPIError(err)
	return ae != nil && ae.Status == http.StatusConflict
}

// GetMRApprovals reads who approved the MR (GET .../approvals). gonk only
// READS approvals; it never approves (no approve call exists here).
//
// The reply is validated by the presence of `approved` and `approved_by`: a
// 200 that carries neither (an error object that parses as JSON) is an error,
// not "nobody approved". This instance's reply has no iid to compare, which is
// why the check is on those two fields.
func (c *Client) GetMRApprovals(ctx context.Context, projectID, mrIID int64) (*MRApprovals, error) {
	var raw struct {
		Approved   *bool         `json:"approved"`
		ApprovedBy *[]MRApprover `json:"approved_by"`
	}
	if err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/approvals", projectID, mrIID),
	}, &raw); err != nil {
		return nil, err
	}
	if raw.Approved == nil || raw.ApprovedBy == nil {
		return nil, fmt.Errorf("gitlab: GET merge request %d approvals: reply lacks approved/approved_by", mrIID)
	}
	return &MRApprovals{Approved: *raw.Approved, ApprovedBy: *raw.ApprovedBy}, nil
}

// ListMRDiffs returns the MR's CURRENT diff, one entry per file (GET
// .../merge_requests/:iid/diffs), for re-running the scope gates against what
// is actually on the MR rather than what gonk once wrote (plan 9.1 A5). An
// entry with neither path is an error. A file GitLab withheld (TooLarge) is
// returned flagged; the caller must treat it as unreadable, not as unchanged.
func (c *Client) ListMRDiffs(ctx context.Context, projectID, mrIID int64) ([]MRDiff, error) {
	ds, err := paginate[MRDiff](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/diffs", projectID, mrIID),
	})
	if err != nil {
		return nil, err
	}
	for i, d := range ds {
		if d.NewPath == "" && d.OldPath == "" {
			return nil, fmt.Errorf("gitlab: GET merge request %d diffs: entry %d has no path", mrIID, i)
		}
	}
	return ds, nil
}

// normText makes a description comparable: CRLF -> LF and trimmed.
func normText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

func missingLabels(have, want []string) []string {
	var out []string
	for _, w := range want {
		if !slices.Contains(have, w) {
			out = append(out, w)
		}
	}
	return out
}

func presentLabels(have, unwanted []string) []string {
	var out []string
	for _, u := range unwanted {
		if slices.Contains(have, u) {
			out = append(out, u)
		}
	}
	return out
}
