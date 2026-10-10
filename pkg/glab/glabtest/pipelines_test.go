package glabtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestFakePipelinesNewestFirstAndFiltered(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	ctx, c := context.Background(), gl.Client()
	a := gl.AddPipeline(p.ID, glab.Pipeline{Ref: "main", Status: "success", SHA: shaA})
	b := gl.AddPipeline(p.ID, glab.Pipeline{Ref: "main", Status: "failed", SHA: shaB})
	gl.AddPipeline(p.ID, glab.Pipeline{Ref: "other", Status: "success", SHA: shaB})

	all, err := c.ListPipelines(ctx, p.ID, "main", "", 10)
	if err != nil || len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Fatalf("main pipelines = %+v, %v; want newest first [%d %d]", all, err, b.ID, a.ID)
	}
	ok, err := c.ListPipelines(ctx, p.ID, "main", "success", 1)
	if err != nil || len(ok) != 1 || ok[0].ID != a.ID {
		t.Fatalf("success = %+v, %v", ok, err)
	}
	// The list endpoint omits detail-only fields; Get returns them.
	gl.SetPipelineStatus(p.ID, b.ID, "failed")
	got, err := c.GetPipeline(ctx, p.ID, b.ID)
	if err != nil || got.SHA != shaB || got.ProjectID != p.ID {
		t.Fatalf("GetPipeline = %+v, %v", got, err)
	}
	if _, err := c.GetPipeline(ctx, p.ID, 99999); !glab.IsNotFound(err) {
		t.Fatalf("unknown pipeline err = %v, want IsNotFound", err)
	}
}

func TestFakeJobsCarryRunnerAndTagsAndScopeFilters(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	ctx, c := context.Background(), gl.Client()
	pl := gl.AddPipeline(p.ID, glab.Pipeline{Ref: "main", Status: "failed", SHA: shaA})
	gl.AddJob(p.ID, pl.ID, glab.Job{
		Name: "unit", Stage: "test", Status: "failed", FailureReason: "script_failure",
		TagList:       []string{"docker"},
		Runner:        &glab.Runner{ID: 17, Description: "shared-a", RunnerType: "instance_type", IsShared: true, Active: true, Online: true, Status: "online"},
		RunnerManager: &glab.RunnerManager{ID: 3, SystemID: "r_x", Version: "18.10.0"},
	})
	gl.AddJob(p.ID, pl.ID, glab.Job{Name: "build", Stage: "build", Status: "success"})

	failed, err := c.ListPipelineJobs(ctx, p.ID, pl.ID, "failed", false)
	if err != nil || len(failed) != 1 {
		t.Fatalf("failed jobs = %+v, %v", failed, err)
	}
	j := failed[0]
	if j.Name != "unit" || j.FailureReason != "script_failure" || len(j.TagList) != 1 || j.TagList[0] != "docker" ||
		j.Runner == nil || j.Runner.ID != 17 || j.Runner.Description != "shared-a" || j.Runner.RunnerType != "instance_type" || !j.Runner.IsShared ||
		j.RunnerManager == nil || j.RunnerManager.SystemID != "r_x" ||
		j.Pipeline.ID != pl.ID || j.Pipeline.SHA != shaA || j.Ref != "main" {
		t.Fatalf("job = %+v runner=%+v", j, j.Runner)
	}
	all, err := c.ListPipelineJobs(ctx, p.ID, pl.ID, "", false)
	if err != nil || len(all) != 2 {
		t.Fatalf("all jobs = %+v, %v", all, err)
	}
	if _, err := c.ListPipelineJobs(ctx, p.ID, 99999, "failed", false); !glab.IsNotFound(err) {
		t.Fatalf("unknown pipeline err = %v", err)
	}
}

func TestFakeTraceTailBothServerBehaviours(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	ctx, c := context.Background(), gl.Client()
	trace := strings.Repeat("noise\n", 2000) + "FINAL ERROR LINE\n"
	gl.SetTrace(p.ID, 5, []byte(trace))

	tt, err := c.GetJobTraceTail(ctx, p.ID, 5, 1000, 64)
	if err != nil || !strings.HasSuffix(string(tt.Data), "FINAL ERROR LINE\n") || tt.TailTruncated || !tt.HeadTruncated || tt.BytesRead != 1000 {
		t.Fatalf("range honoured: %+v err=%v", tt, err)
	}

	gl.TraceIgnoresRange = true
	tt, err = c.GetJobTraceTail(ctx, p.ID, 5, 1000, 64)
	if err != nil || tt.BytesRead != 1000 || !tt.TailTruncated || strings.Contains(string(tt.Data), "FINAL") {
		t.Fatalf("range ignored: %+v err=%v", tt, err)
	}
	if _, err := c.GetJobTraceTail(ctx, p.ID, 404, 1000, 64); !glab.IsNotFound(err) {
		t.Fatalf("unknown job err = %v", err)
	}
}

func TestFakeCILintAndCommits(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	ctx, c := context.Background(), gl.Client()
	gl.SetCILint(p.ID, shaA, glab.CILint{Valid: true, MergedYAML: "unit:\n  script: [x]\n"})
	gl.AddCommit(p.ID, glab.Commit{ID: shaA, Title: "t", AuthorName: "A"})
	gl.AddMR(p.ID, glab.MergeRequest{IID: 4, SHA: shaA, SourceBranch: "feat/x", Labels: []string{"keep"}})
	gl.SetCommitMRs(p.ID, shaA, 4)

	l, err := c.CILintStatic(ctx, p.ID, shaA)
	if err != nil || !strings.Contains(l.MergedYAML, "unit:") {
		t.Fatalf("lint = %+v, %v", l, err)
	}
	if _, err := c.CILintStatic(ctx, p.ID, shaB); !glab.IsNotFound(err) {
		t.Fatalf("unseeded sha err = %v", err)
	}
	cm, err := c.GetCommit(ctx, p.ID, shaA)
	if err != nil || cm.Title != "t" || cm.AuthorName != "A" {
		t.Fatalf("commit = %+v, %v", cm, err)
	}
	mrs, err := c.ListMRsForCommit(ctx, p.ID, shaA)
	if err != nil || len(mrs) != 1 || mrs[0].IID != 4 || mrs[0].SourceProjectID != p.ID {
		t.Fatalf("mrs = %+v, %v", mrs, err)
	}
	if _, err := c.ListMRsForCommit(ctx, p.ID, shaB); !glab.IsNotFound(err) {
		t.Fatalf("unknown commit err = %v", err)
	}
}

func TestFakeMRNotesEditInPlaceAndLabels(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	ctx, c := context.Background(), gl.Client()
	gl.AddMR(p.ID, glab.MergeRequest{IID: 4, Labels: []string{"keep", "gonk::pipeline-code"}})
	pl := gl.AddPipeline(p.ID, glab.Pipeline{Ref: "feat/x", Status: "failed"})
	gl.AddMRPipeline(p.ID, 4, pl.ID)

	n, err := c.CreateMRNote(ctx, p.ID, 4, "v1")
	if err != nil || n.Author.ID != 7 {
		t.Fatalf("create = %+v, %v", n, err)
	}
	if _, err := c.UpdateMRNote(ctx, p.ID, 4, n.ID, "v2"); err != nil {
		t.Fatal(err)
	}
	notes, err := c.ListMRNotes(ctx, p.ID, 4)
	if err != nil || len(notes) != 1 || notes[0].Body != "v2" || notes[0].ID != n.ID {
		t.Fatalf("notes = %+v, %v; want ONE note edited in place", notes, err)
	}
	if _, err := c.UpdateMRNote(ctx, p.ID, 4, 99999, "x"); !glab.IsNotFound(err) {
		t.Fatalf("unknown note err = %v", err)
	}

	mr, err := c.UpdateMRLabels(ctx, p.ID, 4, []string{"gonk::pipeline-test"}, []string{"gonk::pipeline-code"})
	if err != nil || strings.Join(mr.Labels, ",") != "keep,gonk::pipeline-test" {
		t.Fatalf("labels = %+v, %v", mr, err)
	}
	got, err := c.GetMergeRequest(ctx, p.ID, 4)
	if err != nil || strings.Join(got.Labels, ",") != "keep,gonk::pipeline-test" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	ps, err := c.ListMRPipelines(ctx, p.ID, 4)
	if err != nil || len(ps) != 1 || ps[0].ID != pl.ID {
		t.Fatalf("mr pipelines = %+v, %v", ps, err)
	}
	if _, err := c.GetMergeRequest(ctx, p.ID, 99); !glab.IsNotFound(err) {
		t.Fatalf("unknown MR err = %v", err)
	}
}

func TestFakeUpdateIssueAndAuthorFilter(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	ctx, c := context.Background(), gl.Client()
	gl.AddIssue(p.ID, 1, "opened", "keep")
	p.Issues[0].Author = glab.User{ID: 7}
	gl.AddIssue(p.ID, 2, "opened")
	p.Issues[1].Author = glab.User{ID: 8}

	mine, err := c.ListIssues(ctx, p.ID, glab.IssueListOptions{AuthorID: 7})
	if err != nil || len(mine) != 1 || mine[0].IID != 1 {
		t.Fatalf("author filter = %+v, %v", mine, err)
	}
	title, desc := "T", "D"
	is, err := c.UpdateIssue(ctx, p.ID, 1, glab.IssueUpdate{Title: &title, Description: &desc, AddLabels: []string{"new"}, RemoveLabels: []string{"keep"}})
	if err != nil || is.Title != "T" || is.Description != "D" || strings.Join(is.Labels, ",") != "new" {
		t.Fatalf("update = %+v, %v", is, err)
	}
	if err := c.CloseIssue(ctx, p.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetIssue(ctx, p.ID, 1)
	if err != nil || got.State != "closed" || got.Title != "T" {
		t.Fatalf("after close = %+v, %v", got, err)
	}
	if _, err := c.UpdateIssue(ctx, p.ID, 99, glab.IssueUpdate{Title: &title}); !glab.IsNotFound(err) {
		t.Fatalf("unknown issue err = %v", err)
	}
}

func TestFakeHookPipelineEvents(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	ctx, c := context.Background(), gl.Client()
	h, err := c.CreateHook(ctx, p.ID, glab.HookOptions{URL: "u", PipelineEvents: true})
	if err != nil || !h.PipelineEvents {
		t.Fatalf("create = %+v, %v", h, err)
	}
	if h, err = c.EditHook(ctx, p.ID, h.ID, glab.HookOptions{URL: "u"}); err != nil || h.PipelineEvents {
		t.Fatalf("edit off = %+v, %v", h, err)
	}
	hs, _ := c.ListHooks(ctx, p.ID)
	if len(hs) != 1 || hs[0].PipelineEvents {
		t.Fatalf("hooks = %+v", hs)
	}
	if h, err = c.EditHook(ctx, p.ID, h.ID, glab.HookOptions{URL: "u", PipelineEvents: true}); err != nil || !h.PipelineEvents {
		t.Fatalf("edit on = %+v, %v", h, err)
	}
	if hs, _ = c.ListHooks(ctx, p.ID); len(hs) != 1 || !hs[0].PipelineEvents {
		t.Fatalf("hooks after edit on = %+v", hs)
	}
}

// The issue-note routes gonk-tios's ref landing writes through: a created
// issue keeps its description and is authored by the token's user, and a note
// is created and then edited in place, both authored by the token's user.
func TestFakeIssueNotesCreateAndEditInPlace(t *testing.T) {
	s := glabtest.New(t)
	s.Me = glab.User{ID: 7, Username: "gonk"}
	p := s.AddProject("g/r", glab.AccessMaintainer)
	c := s.Client()
	ctx := context.Background()

	is, err := c.CreateIssue(ctx, p.ID, glab.IssueOptions{Title: "t", Description: "d <!-- m -->"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.IssuesOf(p.ID)
	if len(got) != 1 || got[0].Description != "d <!-- m -->" || got[0].Author.ID != 7 {
		t.Fatalf("issues = %+v, want one with the description, authored by the token user", got)
	}
	n, err := c.CreateIssueNote(ctx, p.ID, is.IID, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateIssueNote(ctx, p.ID, is.IID, n.ID, "second"); err != nil {
		t.Fatal(err)
	}
	notes := s.IssueNotesOf(p.ID, is.IID)
	if len(notes) != 1 || notes[0].Body != "second" || notes[0].Author.ID != 7 {
		t.Fatalf("notes = %+v, want one edited note authored by the token user", notes)
	}
	if _, err := c.UpdateIssueNote(ctx, p.ID, is.IID, n.ID+100, "x"); !glab.IsNotFound(err) {
		t.Fatalf("editing an absent note: err = %v, want a 404", err)
	}
	if _, err := c.CreateIssueNote(ctx, p.ID, 99, "x"); !glab.IsNotFound(err) {
		t.Fatalf("a note on an absent issue: err = %v, want a 404", err)
	}
}

// GetProject and a branch name resolved to its head commit: what a caller
// needs to pin "the default branch, now" to a SHA.
func TestFakeGetProjectAndBranchHead(t *testing.T) {
	s := glabtest.New(t)
	p := s.AddProject("example/calc", glab.AccessMaintainer)
	const head = "2222222222222222222222222222222222222222"
	s.SetBranchHead(p.ID, "main", head)
	s.SetBranchHead(p.ID, "feature/sum", "3333333333333333333333333333333333333333")
	c := s.Client()
	ctx := context.Background()

	got, err := c.GetProject(ctx, p.ID)
	if err != nil || got.DefaultBranch != "main" || got.ID != p.ID || got.PathWithNamespace != "example/calc" {
		t.Fatalf("GetProject = %+v, %v", got, err)
	}
	if sha, err := c.ResolveRef(ctx, p.ID, "main"); err != nil || sha != head {
		t.Fatalf("ResolveRef(main) = %q, %v; want %s", sha, err, head)
	}
	if sha, err := c.ResolveRef(ctx, p.ID, "feature/sum"); err != nil || sha != "3333333333333333333333333333333333333333" {
		t.Fatalf("ResolveRef(feature/sum) = %q, %v", sha, err)
	}
	if _, err := c.ResolveRef(ctx, p.ID, "no-such-branch"); !glab.IsNotFound(err) {
		t.Fatalf("ResolveRef of an unknown branch: %v, want not found", err)
	}
	if _, err := c.GetProject(ctx, 999999); !glab.IsNotFound(err) {
		t.Fatalf("GetProject of an unknown project: %v, want not found", err)
	}
}

// GitLab lists only the LATEST attempt of each job unless include_retried=true
// (observed on project 75); a fake that always listed every attempt would let a
// reader that forgets the parameter pass its tests.
func TestJobsListHidesRetriedAttemptsUnlessAsked(t *testing.T) {
	s := glabtest.New(t)
	p := s.AddProject("g/p", 40)
	pl := s.AddPipeline(p.ID, glab.Pipeline{Status: "success", SHA: "s"})
	first := s.AddJob(p.ID, pl.ID, glab.Job{Name: "unit", Status: "failed"})
	s.AddJob(p.ID, pl.ID, glab.Job{Name: "lint", Status: "success"})
	second := s.AddJob(p.ID, pl.ID, glab.Job{Name: "unit", Status: "success"})
	c := s.Client()

	latest, err := c.ListPipelineJobs(context.Background(), p.ID, pl.ID, "", false)
	if err != nil || len(latest) != 2 {
		t.Fatalf("default listing = %+v, %v; want the 2 latest attempts", latest, err)
	}
	for _, j := range latest {
		if j.ID == first.ID {
			t.Fatalf("the default listing holds the retried attempt %d", first.ID)
		}
	}
	all, err := c.ListPipelineAttempts(context.Background(), p.ID, pl.ID)
	if err != nil || len(all) != 3 {
		t.Fatalf("include_retried listing = %+v, %v; want all 3 job objects", all, err)
	}
	seen := map[int64]bool{}
	for _, j := range all {
		seen[j.ID] = true
	}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatalf("include_retried listing lacks an attempt: %+v", all)
	}
}

func TestDryRunLintCanDifferFromTheStaticOne(t *testing.T) {
	s := glabtest.New(t)
	p := s.AddProject("g/p", 40)
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	s.SetCILint(p.ID, sha, glab.CILint{Valid: true, MergedYAML: "a:\n  script: [own]\n"})
	s.SetCILintDryRun(p.ID, sha, glab.CILint{Valid: true, MergedYAML: "a:\n  script: [default-branch]\n"})
	c := s.Client()
	st, err := c.CILintStatic(context.Background(), p.ID, sha)
	if err != nil || st.MergedYAML != "a:\n  script: [own]\n" {
		t.Fatalf("static = %+v, %v", st, err)
	}
	// The client has no dry-run call (gonk-z6gb); the fake still answers one
	// the way GitLab does, so a test can show what a dry run WOULD have read.
	rq, err := http.NewRequest("GET", fmt.Sprintf("%s/api/v4/projects/%d/ci/lint?content_ref=%s&dry_run=true&include_merged_yaml=true", s.URL(), p.ID, sha), nil)
	if err != nil {
		t.Fatal(err)
	}
	rq.Header.Set("PRIVATE-TOKEN", "glabtest-token")
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var dry glab.CILint
	if err := json.NewDecoder(resp.Body).Decode(&dry); err != nil || resp.StatusCode != 200 || dry.MergedYAML != "a:\n  script: [default-branch]\n" {
		t.Fatalf("dry run = %d %+v, %v", resp.StatusCode, dry, err)
	}
	qs := s.CILintQueries(p.ID)
	if len(qs) != 2 || qs[0].Get("dry_run") != "" || qs[1].Get("dry_run") != "true" || qs[0].Get("content_ref") != sha {
		t.Fatalf("lint queries = %v, want the static one then the dry run", qs)
	}
}
