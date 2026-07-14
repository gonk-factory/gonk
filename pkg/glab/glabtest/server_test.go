package glabtest_test

import (
	"context"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

func TestFakeDrivesTheRealClient(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\n"))

	c := gl.Client()
	ctx := context.Background()

	if u, err := c.CurrentUser(ctx); err != nil || u.Username != "gonk" {
		t.Fatalf("CurrentUser = %+v, %v", u, err)
	}
	ps, err := c.ListMemberProjects(ctx)
	if err != nil || len(ps) != 1 || ps[0].EffectiveAccess() != glab.AccessMaintainer {
		t.Fatalf("ListMemberProjects = %+v, %v", ps, err)
	}
	raw, err := c.GetRawFile(ctx, ps[0].ID, ".gonk.yml", "main", 65536)
	if err != nil || string(raw) != "version: 1\nenabled: true\n" {
		t.Fatalf("GetRawFile = %q, %v", raw, err)
	}
	if _, err := c.GetRawFile(ctx, ps[0].ID, "nope.yml", "main", 65536); !glab.IsNotFound(err) {
		t.Fatalf("missing file err = %v, want IsNotFound", err)
	}
}

func TestFakeRejectsBadToken(t *testing.T) {
	gl := glabtest.New(t)
	c := glab.New(gl.URL(), "wrong-token")
	if _, err := c.CurrentUser(context.Background()); !glab.IsForbidden(err) {
		t.Fatalf("err = %v, want 401/403", err)
	}
}

func TestFakeHookLifecycle(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	c, ctx := gl.Client(), context.Background()

	h, err := c.CreateHook(ctx, p.ID, glab.HookOptions{URL: "https://gonk/hook/gitlab?gen=1", Token: "t", IssuesEvents: true})
	if err != nil {
		t.Fatalf("CreateHook = %v", err)
	}
	hooks, err := c.ListHooks(ctx, p.ID)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("ListHooks = %+v, %v", hooks, err)
	}
	if hooks[0].URL == "" || hooks[0].ID != h.ID {
		t.Fatalf("hook = %+v", hooks[0])
	}
	// GitLab never returns the token. The fake must not either, or a test could
	// pass against a behaviour production does not have.
	if got := gl.HookToken(p.ID, h.ID); got != "t" {
		t.Fatalf("stored token = %q", got)
	}
	if _, err := c.EditHook(ctx, p.ID, h.ID, glab.HookOptions{URL: "https://gonk/hook/gitlab?gen=2", Token: "t2", IssuesEvents: true}); err != nil {
		t.Fatalf("EditHook = %v", err)
	}
	if got := gl.HookToken(p.ID, h.ID); got != "t2" {
		t.Fatalf("token after edit = %q", got)
	}
}

func TestFakeBranchCommitMR(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	c, ctx := gl.Client(), context.Background()

	if _, err := c.CreateBranch(ctx, p.ID, "gonk/onboard", "main"); err != nil {
		t.Fatalf("CreateBranch = %v", err)
	}
	if _, err := c.CreateBranch(ctx, p.ID, "gonk/onboard", "main"); err == nil {
		t.Fatal("second CreateBranch must fail (branch exists)")
	}
	if _, err := c.CreateCommit(ctx, p.ID, glab.CommitOptions{
		Branch:        "gonk/onboard",
		CommitMessage: "chore: add .gonk.yml",
		Actions:       []glab.CommitAction{{Action: "create", FilePath: ".gonk.yml", Content: "version: 1\n"}},
	}); err != nil {
		t.Fatalf("CreateCommit = %v", err)
	}
	mr, err := c.CreateMergeRequest(ctx, p.ID, glab.MROptions{
		SourceBranch: "gonk/onboard", TargetBranch: "main", Title: "gonk onboarding",
	})
	if err != nil {
		t.Fatalf("CreateMergeRequest = %v", err)
	}
	mrs, err := c.ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: "gonk/onboard", State: "all"})
	if err != nil || len(mrs) != 1 || mrs[0].IID != mr.IID || mrs[0].State != "opened" {
		t.Fatalf("ListMergeRequests = %+v, %v", mrs, err)
	}
	// Branch content is visible on the branch, not on main: the onboarding MR
	// must not make the project look already-onboarded.
	if _, err := c.GetRawFile(ctx, p.ID, ".gonk.yml", "main", 4096); !glab.IsNotFound(err) {
		t.Fatalf("main must not have .gonk.yml yet: %v", err)
	}
	if _, err := c.GetRawFile(ctx, p.ID, ".gonk.yml", "gonk/onboard", 4096); err != nil {
		t.Fatalf("branch must have .gonk.yml: %v", err)
	}
}

func TestFakeCanInjectFailures(t *testing.T) {
	gl := glabtest.New(t)
	gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.FailNext("GET", "/api/v4/projects", 500, 2) // fail twice, then succeed
	c := gl.Client()
	if _, err := c.ListMemberProjects(context.Background()); err != nil {
		t.Fatalf("client should have retried through the 500s: %v", err)
	}
}

func TestGetRawFileEscapesNestedPath(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".agent/context.md", []byte("hi"))
	b, err := gl.Client().GetRawFile(context.Background(), p.ID, ".agent/context.md", "main", 4096)
	if err != nil || string(b) != "hi" {
		t.Fatalf("GetRawFile = %q, %v", b, err)
	}
}
