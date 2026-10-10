package glab

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func jsonBody(t *testing.T, r recorded) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("request body %q is not JSON: %v", r.body, err)
	}
	return m
}

func strs(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%v (%T) is not an array", v, v)
	}
	var out []string
	for _, e := range arr {
		out = append(out, e.(string))
	}
	return out
}

func TestGetMergeRequest(t *testing.T) {
	c, rc := setup(t, 200, `{"iid":4,"title":"T","description":"D","state":"opened","source_branch":"feat/x","target_branch":"main","source_project_id":9,"target_project_id":9,"project_id":9,"labels":["gonk::pipeline-code"],"sha":"`+sha1a+`","author":{"id":3,"username":"dev"}}`)
	mr, err := c.GetMergeRequest(context.Background(), 9, 4)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/merge_requests/4", nil)
	if mr.IID != 4 || mr.Title != "T" || mr.Description != "D" || mr.SourceBranch != "feat/x" || mr.TargetBranch != "main" ||
		mr.SourceProjectID != 9 || len(mr.Labels) != 1 || mr.Labels[0] != "gonk::pipeline-code" || mr.SHA != sha1a || mr.Author.Username != "dev" {
		t.Fatalf("mr = %+v", mr)
	}
}

func TestGetMergeRequestNotFoundAndWrongIID(t *testing.T) {
	c, _ := setup(t, 404, `{"message":"404 Merge Request Not Found"}`)
	if mr, err := c.GetMergeRequest(context.Background(), 9, 4); !IsNotFound(err) || mr != nil {
		t.Fatalf("got %+v, %v", mr, err)
	}
	c, _ = setup(t, 200, `{"iid":5}`)
	if mr, err := c.GetMergeRequest(context.Background(), 9, 4); err == nil {
		t.Fatalf("got %+v, nil; want error for another iid", mr)
	}
}

func TestListMRNotes(t *testing.T) {
	c, rc := setup(t, 200, `[{"id":1,"body":"hi","author":{"id":2,"username":"gonk"},"system":false},{"id":2,"body":"x","system":true}]`)
	ns, err := c.ListMRNotes(context.Background(), 9, 4)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/merge_requests/4/notes", map[string]string{"sort": "asc", "order_by": "created_at"})
	if len(ns) != 2 || ns[0].Body != "hi" || ns[0].Author.Username != "gonk" || ns[0].System || !ns[1].System {
		t.Fatalf("notes = %+v", ns)
	}
}

func TestListMRNotes404IsAnError(t *testing.T) {
	c, _ := setup(t, 404, notFoundBody)
	if ns, err := c.ListMRNotes(context.Background(), 9, 4); !IsNotFound(err) || ns != nil {
		t.Fatalf("got %+v, %v", ns, err)
	}
}

func TestCreateMRNote(t *testing.T) {
	c, rc := setup(t, 201, `{"id":77,"body":"hello <!-- m -->","author":{"id":2}}`)
	n, err := c.CreateMRNote(context.Background(), 9, 4, "hello <!-- m -->")
	if err != nil {
		t.Fatal(err)
	}
	r := rc.only(t)
	wantReq(t, r, "POST", "/api/v4/projects/9/merge_requests/4/notes", nil)
	if b := jsonBody(t, r); b["body"] != "hello <!-- m -->" || len(b) != 1 {
		t.Fatalf("body = %v", b)
	}
	if r.header.Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q", r.header.Get("Content-Type"))
	}
	if n.ID != 77 {
		t.Fatalf("note = %+v", n)
	}
}

func TestCreateMRNoteReplyWithoutIDIsAnError(t *testing.T) {
	c, _ := setup(t, 201, `{"message":"ok"}`)
	if n, err := c.CreateMRNote(context.Background(), 9, 4, "x"); err == nil {
		t.Fatalf("got %+v, nil; want error", n)
	}
}

func TestCreateMRNote404IsAnError(t *testing.T) {
	c, _ := setup(t, 404, notFoundBody)
	if n, err := c.CreateMRNote(context.Background(), 9, 4, "x"); !IsNotFound(err) || n != nil {
		t.Fatalf("got %+v, %v", n, err)
	}
}

func TestUpdateMRNote(t *testing.T) {
	c, rc := setup(t, 200, `{"id":77,"body":"v2"}`)
	n, err := c.UpdateMRNote(context.Background(), 9, 4, 77, "v2")
	if err != nil {
		t.Fatal(err)
	}
	r := rc.only(t)
	wantReq(t, r, "PUT", "/api/v4/projects/9/merge_requests/4/notes/77", nil)
	if b := jsonBody(t, r); b["body"] != "v2" || len(b) != 1 {
		t.Fatalf("body = %v", b)
	}
	if n.ID != 77 || n.Body != "v2" {
		t.Fatalf("note = %+v", n)
	}
}

func TestUpdateMRNoteWrongIDAnd404(t *testing.T) {
	c, _ := setup(t, 200, `{"id":78,"body":"v2"}`)
	if n, err := c.UpdateMRNote(context.Background(), 9, 4, 77, "v2"); err == nil {
		t.Fatalf("got %+v, nil; want error for another note id", n)
	}
	c, _ = setup(t, 404, `{"message":"404 Note Not Found"}`)
	if n, err := c.UpdateMRNote(context.Background(), 9, 4, 77, "v2"); !IsNotFound(err) || n != nil {
		t.Fatalf("got %+v, %v", n, err)
	}
}

func TestUpdateMRLabels(t *testing.T) {
	c, rc := setup(t, 200, `{"iid":4,"labels":["gonk::pipeline-test"]}`)
	mr, err := c.UpdateMRLabels(context.Background(), 9, 4, []string{"gonk::pipeline-test"}, []string{"gonk::pipeline-code", "b,c"})
	if err != nil {
		t.Fatal(err)
	}
	r := rc.only(t)
	wantReq(t, r, "PUT", "/api/v4/projects/9/merge_requests/4", nil)
	b := jsonBody(t, r)
	if add := strs(t, b["add_labels"]); len(add) != 1 || add[0] != "gonk::pipeline-test" {
		t.Errorf("add_labels = %v", add)
	}
	// A comma inside one label stays one array element.
	if rm := strs(t, b["remove_labels"]); len(rm) != 2 || rm[0] != "gonk::pipeline-code" || rm[1] != "b,c" {
		t.Errorf("remove_labels = %v", rm)
	}
	if mr.IID != 4 || len(mr.Labels) != 1 || mr.Labels[0] != "gonk::pipeline-test" {
		t.Fatalf("mr = %+v", mr)
	}
}

func TestUpdateMRLabelsOnlyRemoveOmitsAdd(t *testing.T) {
	c, rc := setup(t, 200, `{"iid":4}`)
	if _, err := c.UpdateMRLabels(context.Background(), 9, 4, nil, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if b := jsonBody(t, rc.only(t)); b["add_labels"] != nil || b["remove_labels"] == nil {
		t.Fatalf("body = %v", b)
	}
}

func TestUpdateMRLabelsNothingToDoSendsNothing(t *testing.T) {
	c, rc := setup(t, 200, `{"iid":4}`)
	mr, err := c.UpdateMRLabels(context.Background(), 9, 4, nil, nil)
	if err != nil || mr != nil {
		t.Fatalf("got %+v, %v", mr, err)
	}
	rc.none(t)
}

func TestUpdateMRLabelsWrongIIDAnd404(t *testing.T) {
	c, _ := setup(t, 200, `{"iid":5}`)
	if mr, err := c.UpdateMRLabels(context.Background(), 9, 4, []string{"a"}, nil); err == nil {
		t.Fatalf("got %+v, nil; want error", mr)
	}
	c, _ = setup(t, 404, notFoundBody)
	if mr, err := c.UpdateMRLabels(context.Background(), 9, 4, []string{"a"}, nil); !IsNotFound(err) || mr != nil {
		t.Fatalf("got %+v, %v", mr, err)
	}
}

// ---- issues ----

func sp(s string) *string { return &s }

func TestUpdateIssueSendsOnlyWhatIsSet(t *testing.T) {
	c, rc := setup(t, 200, `{"iid":3,"title":"T2","description":"D2","labels":["l1"]}`)
	is, err := c.UpdateIssue(context.Background(), 9, 3, IssueUpdate{Title: sp("T2"), Description: sp("D2"), AddLabels: []string{"l1"}, RemoveLabels: []string{"l0"}})
	if err != nil {
		t.Fatal(err)
	}
	r := rc.only(t)
	wantReq(t, r, "PUT", "/api/v4/projects/9/issues/3", nil)
	b := jsonBody(t, r)
	if b["title"] != "T2" || b["description"] != "D2" || len(b) != 4 {
		t.Fatalf("body = %v", b)
	}
	if a := strs(t, b["add_labels"]); len(a) != 1 || a[0] != "l1" {
		t.Errorf("add_labels = %v", a)
	}
	if a := strs(t, b["remove_labels"]); len(a) != 1 || a[0] != "l0" {
		t.Errorf("remove_labels = %v", a)
	}
	if is.IID != 3 || is.Title != "T2" || is.Description != "D2" {
		t.Fatalf("issue = %+v", is)
	}
}

func TestUpdateIssueEmptyDescriptionClearsButNilLeavesAlone(t *testing.T) {
	c, rc := setup(t, 200, `{"iid":3}`)
	if _, err := c.UpdateIssue(context.Background(), 9, 3, IssueUpdate{Description: sp("")}); err != nil {
		t.Fatal(err)
	}
	b := jsonBody(t, rc.only(t))
	if v, ok := b["description"]; !ok || v != "" {
		t.Fatalf("body = %v, want description:\"\" present", b)
	}
	if _, ok := b["title"]; ok {
		t.Fatalf("title sent although nil: %v", b)
	}
}

func TestUpdateIssueNothingToDoSendsNothing(t *testing.T) {
	c, rc := setup(t, 200, `{"iid":3}`)
	is, err := c.UpdateIssue(context.Background(), 9, 3, IssueUpdate{})
	if err != nil || is != nil {
		t.Fatalf("got %+v, %v", is, err)
	}
	rc.none(t)
}

func TestUpdateIssueWrongIIDAnd404(t *testing.T) {
	c, _ := setup(t, 200, `{"iid":4}`)
	if is, err := c.UpdateIssue(context.Background(), 9, 3, IssueUpdate{Title: sp("x")}); err == nil {
		t.Fatalf("got %+v, nil; want error", is)
	}
	c, _ = setup(t, 404, `{"message":"404 Issue Not Found"}`)
	if is, err := c.UpdateIssue(context.Background(), 9, 3, IssueUpdate{Title: sp("x")}); !IsNotFound(err) || is != nil {
		t.Fatalf("got %+v, %v", is, err)
	}
}

func TestListIssuesAuthorIDAndLabelsQuery(t *testing.T) {
	c, rc := setup(t, 200, `[{"iid":1,"author":{"id":42}}]`)
	is, err := c.ListIssues(context.Background(), 9, IssueListOptions{State: "opened", Labels: "gonk::pipeline-failing", AuthorID: 42, UpdatedAfter: time.Unix(0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/issues",
		map[string]string{"state": "opened", "labels": "gonk::pipeline-failing", "author_id": "42"})
	if len(is) != 1 || is[0].Author.ID != 42 {
		t.Fatalf("issues = %+v", is)
	}
}

func TestListIssuesNoAuthorIDWhenZero(t *testing.T) {
	c, rc := setup(t, 200, `[]`)
	if _, err := c.ListIssues(context.Background(), 9, IssueListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, has := rc.only(t).query["author_id"]; has {
		t.Fatal("author_id sent for AuthorID 0")
	}
}

// ---- hooks ----

func TestHookPipelineEventsRoundTrip(t *testing.T) {
	c, rc := setup(t, 201, `{"id":5,"url":"u","pipeline_events":true,"issues_events":true}`)
	h, err := c.CreateHook(context.Background(), 9, HookOptions{URL: "u", IssuesEvents: true, PipelineEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	if b := jsonBody(t, rc.only(t)); b["pipeline_events"] != true {
		t.Fatalf("create body pipeline_events = %v", b["pipeline_events"])
	}
	if !h.PipelineEvents {
		t.Fatalf("hook = %+v, want PipelineEvents decoded", h)
	}

	c, rc = setup(t, 200, `{"id":5,"pipeline_events":false}`)
	h, err = c.EditHook(context.Background(), 9, 5, HookOptions{URL: "u"})
	if err != nil {
		t.Fatal(err)
	}
	// Off must be sent explicitly: edit-back-to-false depends on it.
	if b := jsonBody(t, rc.only(t)); b["pipeline_events"] != false {
		t.Fatalf("edit body pipeline_events = %v, want explicit false", b["pipeline_events"])
	}
	if h.PipelineEvents {
		t.Fatalf("hook = %+v", h)
	}
}

func TestListHooksDecodesPipelineEvents(t *testing.T) {
	c, _ := setup(t, 200, `[{"id":1,"pipeline_events":true},{"id":2,"pipeline_events":false}]`)
	hs, err := c.ListHooks(context.Background(), 9)
	if err != nil || len(hs) != 2 || !hs[0].PipelineEvents || hs[1].PipelineEvents {
		t.Fatalf("hooks = %+v, %v", hs, err)
	}
}

// gonk-2usw EPOCH: the merged-fix lookup bounds its listing by state, target
// branch and author. Each is sent under GitLab's own parameter name, the
// squash commit is decoded, and an unbounded listing sends none of them.
func TestListMergeRequestsTargetBranchAndAuthorQuery(t *testing.T) {
	c, rc := setup(t, 200, `[{"iid":12,"state":"merged","target_branch":"main","author":{"id":42},`+
		`"merge_commit_sha":"aa","squash_commit_sha":"bb","sha":"cc","merged_at":"2026-03-01T12:00:00Z"}]`)
	mrs, err := c.ListMergeRequests(context.Background(), 9, MRListOptions{State: "merged", TargetBranch: "main", AuthorID: 42})
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/merge_requests",
		map[string]string{"state": "merged", "target_branch": "main", "author_id": "42"})
	if len(mrs) != 1 || mrs[0].IID != 12 || mrs[0].Author.ID != 42 || mrs[0].MergeCommitSHA != "aa" ||
		mrs[0].SquashCommitSHA != "bb" || mrs[0].SHA != "cc" || mrs[0].MergedAt == nil {
		t.Fatalf("merge requests = %+v", mrs)
	}

	c, rc = setup(t, 200, `[]`)
	if _, err := c.ListMergeRequests(context.Background(), 9, MRListOptions{}); err != nil {
		t.Fatal(err)
	}
	q := rc.only(t).query
	for _, k := range []string{"target_branch", "author_id", "state"} {
		if _, ok := q[k]; ok {
			t.Fatalf("an unbounded listing sent %s: %v", k, q)
		}
	}
}
