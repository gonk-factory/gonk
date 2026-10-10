package glab

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// Part GLAB of gonk-2usw. Every call here is validated by a NAMED FIELD of the
// reply, never by the reply being well-formed JSON: GitLab answers many errors
// with an object that parses perfectly ({"message":"404 ..."}), and on
// 2026-09-07 a check that accepted any parseable reply reported four image tags
// present when none existed.

const (
	mrOKBody     = `{"iid":7,"title":"T","description":"D","state":"opened","source_branch":"gonk/fix-1","target_branch":"main","labels":["gonk::pipeline-fix","gonk::fix-verifying"],"sha":"` + sha1a + `"}`
	notFoundJSON = `{"message":"404 Project Not Found"}`
)

func TestCreateMergeRequest(t *testing.T) {
	opts := MROptions{
		SourceBranch: "gonk/fix-1", TargetBranch: "main", Title: "T", Description: "D",
		Labels: []string{"gonk::pipeline-fix", "gonk::fix-verifying"},
	}
	t.Run("ok asserts iid, branches and labels of the reply", func(t *testing.T) {
		c, rc := setup(t, 201, mrOKBody)
		mr, err := c.CreateMergeRequest(context.Background(), 9, opts)
		if err != nil {
			t.Fatal(err)
		}
		r := rc.only(t)
		wantReq(t, r, "POST", "/api/v4/projects/9/merge_requests", nil)
		b := jsonBody(t, r)
		if b["source_branch"] != "gonk/fix-1" || b["target_branch"] != "main" {
			t.Errorf("body = %v", b)
		}
		if l := strs(t, b["labels"]); len(l) != 2 || l[0] != "gonk::pipeline-fix" {
			t.Errorf("labels must go as a JSON array, got %v", b["labels"])
		}
		if _, has := b["assignee_id"]; has {
			t.Errorf("an assignee was sent: %v", b)
		}
		if mr.IID != 7 || mr.SourceBranch != "gonk/fix-1" || mr.TargetBranch != "main" || len(mr.Labels) != 2 {
			t.Fatalf("mr = %+v", mr)
		}
	})
	for _, tc := range []struct {
		name   string
		status int
		body   string
		notFnd bool
	}{
		{"404 status is an error", 404, notFoundJSON, true},
		{"200 with an error object that parses as JSON is an error", 200, notFoundJSON, false},
		{"reply iid 0", 201, `{"source_branch":"gonk/fix-1","target_branch":"main","labels":["gonk::pipeline-fix","gonk::fix-verifying"]}`, false},
		{"reply is another source branch", 201, `{"iid":7,"source_branch":"other","target_branch":"main","labels":["gonk::pipeline-fix","gonk::fix-verifying"]}`, false},
		{"reply is another target branch", 201, `{"iid":7,"source_branch":"gonk/fix-1","target_branch":"release","labels":["gonk::pipeline-fix","gonk::fix-verifying"]}`, false},
		{"reply lacks one label", 201, `{"iid":7,"source_branch":"gonk/fix-1","target_branch":"main","labels":["gonk::pipeline-fix"]}`, false},
		{"409 an MR already exists", 409, `{"message":["Another open merge request already exists for this source branch: !3"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setup(t, tc.status, tc.body)
			mr, err := c.CreateMergeRequest(context.Background(), 9, opts)
			if err == nil || mr != nil {
				t.Fatalf("got %+v, %v; want an error and no MR", mr, err)
			}
			if tc.notFnd && !IsNotFound(err) {
				t.Fatalf("err = %v, want IsNotFound", err)
			}
		})
	}
	t.Run("a label with a comma is refused before any request", func(t *testing.T) {
		c, rc := setup(t, 201, mrOKBody)
		o := opts
		o.Labels = []string{"a,b"}
		if mr, err := c.CreateMergeRequest(context.Background(), 9, o); err == nil || mr != nil {
			t.Fatalf("got %+v, %v", mr, err)
		}
		rc.none(t)
	})
	t.Run("no labels asked, none required", func(t *testing.T) {
		c, _ := setup(t, 201, `{"iid":7,"source_branch":"gonk/fix-1","target_branch":"main"}`)
		o := opts
		o.Labels = nil
		if _, err := c.CreateMergeRequest(context.Background(), 9, o); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateMergeRequest(t *testing.T) {
	ctx := context.Background()
	t.Run("ok sends description and label arrays and asserts them back", func(t *testing.T) {
		c, rc := setup(t, 200, `{"iid":7,"description":"new body","labels":["gonk::fixed"]}`)
		mr, err := c.UpdateMergeRequest(ctx, 9, 7, MRUpdate{
			Description: sp("new body"), AddLabels: []string{"gonk::fixed"}, RemoveLabels: []string{"gonk::fix-verifying"},
		})
		if err != nil {
			t.Fatal(err)
		}
		r := rc.only(t)
		wantReq(t, r, "PUT", "/api/v4/projects/9/merge_requests/7", nil)
		b := jsonBody(t, r)
		if b["description"] != "new body" || strs(t, b["add_labels"])[0] != "gonk::fixed" || strs(t, b["remove_labels"])[0] != "gonk::fix-verifying" {
			t.Errorf("body = %v", b)
		}
		if _, has := b["assignee_id"]; has {
			t.Errorf("an assignee was sent: %v", b)
		}
		if mr.IID != 7 || mr.Description != "new body" {
			t.Fatalf("mr = %+v", mr)
		}
	})
	t.Run("description only sends no label keys, and CRLF/trailing newline is normalised", func(t *testing.T) {
		c, rc := setup(t, 200, `{"iid":7,"description":"a\r\nb\n"}`)
		if _, err := c.UpdateMergeRequest(ctx, 9, 7, MRUpdate{Description: sp("a\nb")}); err != nil {
			t.Fatal(err)
		}
		if b := jsonBody(t, rc.only(t)); b["add_labels"] != nil || b["remove_labels"] != nil {
			t.Fatalf("body = %v", b)
		}
	})
	t.Run("an empty description clears", func(t *testing.T) {
		c, rc := setup(t, 200, `{"iid":7,"description":""}`)
		if _, err := c.UpdateMergeRequest(ctx, 9, 7, MRUpdate{Description: sp("")}); err != nil {
			t.Fatal(err)
		}
		if b := jsonBody(t, rc.only(t)); b["description"] != "" {
			t.Fatalf("body = %v", b)
		}
	})
	t.Run("nothing to change sends nothing", func(t *testing.T) {
		c, rc := setup(t, 200, `{"iid":7}`)
		if mr, err := c.UpdateMergeRequest(ctx, 9, 7, MRUpdate{}); err != nil || mr != nil {
			t.Fatalf("got %+v, %v", mr, err)
		}
		rc.none(t)
	})
	for _, tc := range []struct {
		name   string
		status int
		body   string
		u      MRUpdate
		notFnd bool
	}{
		{"404", 404, `{"message":"404 Merge Request Not Found"}`, MRUpdate{Description: sp("x")}, true},
		{"200 error object that parses", 200, `{"message":"404 Merge Request Not Found"}`, MRUpdate{Description: sp("x")}, false},
		{"reply is another MR", 200, `{"iid":8,"description":"x"}`, MRUpdate{Description: sp("x")}, false},
		{"description not applied", 200, `{"iid":7,"description":"old"}`, MRUpdate{Description: sp("x")}, false},
		{"added label not in reply", 200, `{"iid":7,"labels":["other"]}`, MRUpdate{AddLabels: []string{"gonk::fixed"}}, false},
		{"removed label still in reply", 200, `{"iid":7,"labels":["gonk::fix-verifying"]}`, MRUpdate{RemoveLabels: []string{"gonk::fix-verifying"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setup(t, tc.status, tc.body)
			mr, err := c.UpdateMergeRequest(ctx, 9, 7, tc.u)
			if err == nil || mr != nil {
				t.Fatalf("got %+v, %v; want an error and no MR", mr, err)
			}
			if tc.notFnd && !IsNotFound(err) {
				t.Fatalf("err = %v, want IsNotFound", err)
			}
		})
	}
}

func TestAcceptMR(t *testing.T) {
	ctx := context.Background()
	merged := `{"iid":7,"state":"merged","sha":"` + sha1a + `","merge_commit_sha":"` + sha1b + `"}`
	t.Run("ok pins the head sha and asserts iid, state and sha of the reply", func(t *testing.T) {
		c, rc := setup(t, 200, merged)
		mr, err := c.AcceptMR(ctx, 9, 7, sha1a)
		if err != nil {
			t.Fatal(err)
		}
		r := rc.only(t)
		wantReq(t, r, "PUT", "/api/v4/projects/9/merge_requests/7/merge", nil)
		if b := jsonBody(t, r); b["sha"] != sha1a || len(b) != 1 {
			t.Fatalf("body must be exactly {sha}, got %v", b)
		}
		if mr.State != "merged" || mr.IID != 7 || mr.SHA != sha1a || mr.MergeCommitSHA != sha1b {
			t.Fatalf("mr = %+v", mr)
		}
	})
	for _, bad := range []string{"", "abc123", strings.ToUpper(sha1a), "main"} {
		t.Run(fmt.Sprintf("sha %q is refused before any request", bad), func(t *testing.T) {
			c, rc := setup(t, 200, merged)
			if mr, err := c.AcceptMR(ctx, 9, 7, bad); err == nil || mr != nil {
				t.Fatalf("got %+v, %v", mr, err)
			}
			rc.none(t)
		})
	}
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		mismatch bool
	}{
		{"409 head moved is a sha mismatch", 409, `{"message":"SHA does not match HEAD of source branch"}`, true},
		{"405 not mergeable", 405, `{"message":"405 Method Not Allowed"}`, false},
		{"406 cannot be merged", 406, `{"message":"Branch cannot be merged"}`, false},
		{"404", 404, `{"message":"404 Merge Request Not Found"}`, false},
		{"200 error object that parses", 200, `{"message":"404 Merge Request Not Found"}`, false},
		{"200 but state is opened", 200, `{"iid":7,"state":"opened","sha":"` + sha1a + `"}`, false},
		{"200 but another MR", 200, `{"iid":8,"state":"merged","sha":"` + sha1a + `"}`, false},
		{"200 merged but a different head", 200, `{"iid":7,"state":"merged","sha":"` + sha1b + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setup(t, tc.status, tc.body)
			mr, err := c.AcceptMR(ctx, 9, 7, sha1a)
			if err == nil || mr != nil {
				t.Fatalf("got %+v, %v; want an error and no MR", mr, err)
			}
			if IsShaMismatch(err) != tc.mismatch {
				t.Fatalf("IsShaMismatch(%v) = %v, want %v", err, IsShaMismatch(err), tc.mismatch)
			}
			if ae := asAPIError(err); tc.status >= 400 && (ae == nil || ae.Status != tc.status) {
				t.Fatalf("err = %v, want an *APIError with status %d", err, tc.status)
			}
		})
	}
	t.Run("a 5xx is not retried: the merge may have happened", func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		}))
		if _, err := c.AcceptMR(ctx, 9, 7, sha1a); err == nil {
			t.Fatal("want error")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("calls = %d, want exactly 1 (a repeated merge reads as 'already merged')", n)
		}
	})
}

func TestGetMRApprovals(t *testing.T) {
	ctx := context.Background()
	t.Run("approved by a named user", func(t *testing.T) {
		c, rc := setup(t, 200, `{"approved":true,"approved_by":[{"user":{"id":3,"username":"dev"}}],"user_has_approved":false,"user_can_approve":true}`)
		a, err := c.GetMRApprovals(ctx, 9, 7)
		if err != nil {
			t.Fatal(err)
		}
		wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/merge_requests/7/approvals", nil)
		if !a.Approved || len(a.ApprovedBy) != 1 || a.ApprovedBy[0].User.Username != "dev" || a.ApprovedBy[0].User.ID != 3 {
			t.Fatalf("approvals = %+v", a)
		}
	})
	t.Run("not approved is a real answer, not an error", func(t *testing.T) {
		c, _ := setup(t, 200, `{"approved":false,"approved_by":[]}`)
		a, err := c.GetMRApprovals(ctx, 9, 7)
		if err != nil || a.Approved || len(a.ApprovedBy) != 0 {
			t.Fatalf("got %+v, %v", a, err)
		}
	})
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"404", 404, `{"message":"404 Not Found"}`},
		{"200 error object that parses must not read as 'nobody approved'", 200, `{"message":"404 Not Found"}`},
		{"approved without approved_by", 200, `{"approved":true}`},
		{"approved_by without approved", 200, `{"approved_by":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setup(t, tc.status, tc.body)
			if a, err := c.GetMRApprovals(ctx, 9, 7); err == nil || a != nil {
				t.Fatalf("got %+v, %v; want an error", a, err)
			}
		})
	}
}

func TestListMRDiffs(t *testing.T) {
	ctx := context.Background()
	c, rc := setup(t, 200, `[{"old_path":"a.md","new_path":"a.md","new_file":false,"renamed_file":false,"deleted_file":false,"diff":"@@ x","too_large":false},{"old_path":"b","new_path":"c","renamed_file":true,"too_large":true}]`)
	ds, err := c.ListMRDiffs(ctx, 9, 7)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/merge_requests/7/diffs", nil)
	if len(ds) != 2 || ds[0].NewPath != "a.md" || ds[0].Diff != "@@ x" || !ds[1].RenamedFile || ds[1].OldPath != "b" || !ds[1].TooLarge {
		t.Fatalf("diffs = %+v", ds)
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"404", 404, notFoundBody},
		{"an entry with no path", 200, `[{"diff":"x"}]`},
		{"an error object where a list belongs", 200, `{"message":"404 Not Found"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setup(t, tc.status, tc.body)
			if ds, err := c.ListMRDiffs(ctx, 9, 7); err == nil || ds != nil {
				t.Fatalf("got %+v, %v; want an error", ds, err)
			}
		})
	}
}

func TestDeleteBranch(t *testing.T) {
	ctx := context.Background()
	t.Run("204, and the slash in the name is escaped", func(t *testing.T) {
		c, rc := setup(t, 204, "")
		if err := c.DeleteBranch(ctx, 9, "gonk/fix-1"); err != nil {
			t.Fatal(err)
		}
		wantReq(t, rc.only(t), "DELETE", "/api/v4/projects/9/repository/branches/gonk%2Ffix-1", nil)
	})
	t.Run("empty name is refused before any request", func(t *testing.T) {
		c, rc := setup(t, 204, "")
		if err := c.DeleteBranch(ctx, 9, ""); err == nil {
			t.Fatal("want error")
		}
		rc.none(t)
	})
	for _, tc := range []struct {
		name   string
		status int
		body   string
		notFnd bool
	}{
		{"404 branch gone", 404, `{"message":"404 Branch Not Found"}`, true},
		{"403", 403, `{"message":"403 Forbidden"}`, false},
		{"200 with a JSON body is not a delete", 200, `{"message":"404 Branch Not Found"}`, false},
		{"200 empty is not a delete either", 200, ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setup(t, tc.status, tc.body)
			err := c.DeleteBranch(ctx, 9, "gonk/fix-1")
			if err == nil {
				t.Fatal("want error")
			}
			if IsNotFound(err) != tc.notFnd {
				t.Fatalf("IsNotFound(%v) = %v, want %v", err, IsNotFound(err), tc.notFnd)
			}
		})
	}
}

func TestMergeRequestDecodesMergeFacts(t *testing.T) {
	c, _ := setup(t, 200, `{"iid":4,"draft":true,"detailed_merge_status":"discussions_not_resolved","blocking_discussions_resolved":false,"merge_commit_sha":null}`)
	mr, err := c.GetMergeRequest(context.Background(), 9, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !mr.Draft || mr.DetailedMergeStatus != "discussions_not_resolved" || mr.BlockingDiscussionsResolved || mr.MergeCommitSHA != "" {
		t.Fatalf("mr = %+v", mr)
	}
	// A reply that omits the fields must read as the CONSERVATIVE value: not
	// mergeable, threads unresolved.
	c, _ = setup(t, 200, `{"iid":4}`)
	if mr, err = c.GetMergeRequest(context.Background(), 9, 4); err != nil {
		t.Fatal(err)
	}
	if mr.DetailedMergeStatus == "mergeable" || mr.BlockingDiscussionsResolved {
		t.Fatalf("absent fields read permissively: %+v", mr)
	}
}

func TestCreateBranchAndCommitRepliesAreValidated(t *testing.T) {
	ctx := context.Background()
	c, _ := setup(t, 201, `{"name":"gonk/fix-1"}`)
	if b, err := c.CreateBranch(ctx, 9, "gonk/fix-1", "main"); err != nil || b.Name != "gonk/fix-1" {
		t.Fatalf("got %+v, %v", b, err)
	}
	c, _ = setup(t, 201, `{"message":"ok"}`)
	if b, err := c.CreateBranch(ctx, 9, "gonk/fix-1", "main"); err == nil || b != nil {
		t.Fatalf("a branch reply with another name must be an error, got %+v, %v", b, err)
	}
	c, _ = setup(t, 201, `{"id":"`+sha1a+`"}`)
	if cm, err := c.CreateCommit(ctx, 9, CommitOptions{Branch: "gonk/fix-1"}); err != nil || cm.ID != sha1a {
		t.Fatalf("got %+v, %v", cm, err)
	}
	for _, body := range []string{`{"message":"404 Not Found"}`, `{"id":"abc"}`} {
		c, _ = setup(t, 201, body)
		if cm, err := c.CreateCommit(ctx, 9, CommitOptions{Branch: "gonk/fix-1"}); err == nil || cm != nil {
			t.Fatalf("a commit reply %s must be an error, got %+v, %v", body, cm, err)
		}
	}
}
