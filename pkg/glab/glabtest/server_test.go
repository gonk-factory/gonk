package glabtest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
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

// An OPEN merge request's `sha` is its source branch's head: a second commit
// to the branch moves it (what the pipeline-fix exit check's J0 reads to tell
// gonk's own next attempt from somebody else's push). A merged one keeps the
// head it merged with.
func TestAnOpenMergeRequestsHeadFollowsItsSourceBranch(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	c, ctx := gl.Client(), context.Background()
	commit := func(content string) string {
		t.Helper()
		got, err := c.CreateCommit(ctx, p.ID, glab.CommitOptions{Branch: "gonk/x", CommitMessage: "change",
			Actions: []glab.CommitAction{{Action: "create", FilePath: "f", Content: content}}})
		if err != nil {
			t.Fatal(err)
		}
		return got.ID
	}
	if _, err := c.CreateBranch(ctx, p.ID, "gonk/x", "main"); err != nil {
		t.Fatal(err)
	}
	first := commit("one")
	mr, err := c.CreateMergeRequest(ctx, p.ID, glab.MROptions{SourceBranch: "gonk/x", TargetBranch: "main", Title: "t"})
	if err != nil || mr.SHA != first {
		t.Fatalf("created merge request sha %q, %v; want %s", mr.SHA, err, first)
	}
	second := commit("two")
	if got, err := c.GetMergeRequest(ctx, p.ID, mr.IID); err != nil || got.SHA != second || second == first {
		t.Fatalf("after a second commit: sha %q, %v; want %s", got.SHA, err, second)
	}
	gl.SetMRState(p.ID, mr.IID, "merged", time.Now())
	commit("three")
	if got, err := c.GetMergeRequest(ctx, p.ID, mr.IID); err != nil || got.SHA != second {
		t.Fatalf("a merged merge request's head moved: %q, %v; want %s", got.SHA, err, second)
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

// The write log holds every non-GET request, in order, with its query and
// body -- including one that was refused -- and no GET.
func TestWriteLogRecordsEveryWriteAndNoRead(t *testing.T) {
	s := glabtest.New(t)
	p := s.AddProject("g/p", glab.AccessMaintainer)
	p.PutFile("a.txt", []byte("one\n"))
	c := s.Client()
	ctx := context.Background()
	if _, err := c.GetRawFile(ctx, p.ID, "a.txt", p.DefaultBranch, 100); err != nil {
		t.Fatal(err)
	}
	if n := len(s.Writes()); n != 0 {
		t.Fatalf("%d write(s) logged for a read", n)
	}
	s.FailNext("POST", fmt.Sprintf("/api/v4/projects/%d/repository/branches", p.ID), 500, 1)
	if _, err := c.CreateBranch(ctx, p.ID, "b1", p.DefaultBranch); err == nil {
		t.Fatal("the injected failure did not fail the call")
	}
	if _, err := c.CreateBranch(ctx, p.ID, "b1", p.DefaultBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateCommit(ctx, p.ID, glab.CommitOptions{Branch: "b1", CommitMessage: "m",
		Actions: []glab.CommitAction{{Action: "update", FilePath: "a.txt", Content: "two\n"}}}); err != nil {
		t.Fatal(err)
	}
	ws := s.Writes()
	if len(ws) < 3 {
		t.Fatalf("writes: %v", ws)
	}
	last := ws[len(ws)-1]
	if last.Method != "POST" || !strings.HasSuffix(last.Path, "/repository/commits") || !strings.Contains(last.Body, `"file_path":"a.txt"`) {
		t.Fatalf("the commit is not the last logged write: %s", last)
	}
	branches := 0
	for _, w := range ws {
		if strings.HasSuffix(w.Path, "/repository/branches") {
			branches++
			if !strings.Contains(w.Query, "branch=b1") {
				t.Fatalf("a branch write without its query: %s", w)
			}
		}
	}
	if branches < 2 {
		t.Fatalf("the REFUSED branch write is not in the log (%d branch writes): %v", branches, ws)
	}
}

// A commit made through the API is an object: the branch head moves to it, it
// names its parent, and the tree it leaves is readable at its SHA, with modes.
func TestCommitsMoveTheBranchHeadAndTreesCarryModes(t *testing.T) {
	s := glabtest.New(t)
	p := s.AddProject("g/p", glab.AccessMaintainer)
	base := strings.Repeat("a", 40)
	p.PutFileOn(base, "dir/a.txt", []byte("one\n"))
	p.PutFileOn(base, "link", []byte("dir/a.txt"))
	p.SetFileMode(base, "link", glab.ModeSymlink)
	c := s.Client()
	ctx := context.Background()
	if _, err := c.CreateBranch(ctx, p.ID, "b1", base); err != nil {
		t.Fatal(err)
	}
	if head, err := c.ResolveRef(ctx, p.ID, "b1"); err != nil || head != base {
		t.Fatalf("a branch created at a SHA resolves to %q (%v), want %s", head, err, base)
	}
	cm, err := c.CreateCommit(ctx, p.ID, glab.CommitOptions{Branch: "b1", CommitMessage: "title\n\nbody",
		Actions: []glab.CommitAction{{Action: "update", FilePath: "dir/a.txt", Content: "two\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	if head, _ := c.ResolveRef(ctx, p.ID, "b1"); head != cm.ID {
		t.Fatalf("the branch head is %q after the commit, want %s", head, cm.ID)
	}
	got, err := c.GetCommit(ctx, p.ID, cm.ID)
	if err != nil || got.Message != "title\n\nbody" || len(got.ParentIDs) != 1 || got.ParentIDs[0] != base {
		t.Fatalf("commit %+v (%v), want the message and the one parent %s", got, err, base)
	}
	if raw, err := c.GetRawFile(ctx, p.ID, "dir/a.txt", cm.ID, 100); err != nil || string(raw) != "two\n" {
		t.Fatalf("the file at the commit: %q %v", raw, err)
	}
	if raw, _ := c.GetRawFile(ctx, p.ID, "dir/a.txt", base, 100); string(raw) != "one\n" {
		t.Fatalf("the commit changed the base tree: %q", raw)
	}
	root, err := c.ListTree(ctx, p.ID, cm.ID, "")
	if err != nil || len(root) != 2 {
		t.Fatalf("root tree %+v %v", root, err)
	}
	modes := map[string]string{}
	for _, e := range root {
		modes[e.Path] = e.Mode
	}
	if modes["dir"] != glab.ModeTree || modes["link"] != glab.ModeSymlink {
		t.Fatalf("root modes %v", modes)
	}
	if _, err := c.ListTree(ctx, p.ID, cm.ID, "nope"); !glab.IsNotFound(err) {
		t.Fatalf("a missing directory: %v, want a 404", err)
	}
}

// gonk-ltep. The fake's URL is read back by the code under test as a project's
// web_url (gonk-gate links a pipeline only when that URL has no "[" in it), so
// it must not depend on which ports the kernel has to spare.
// httptest.NewServer listens on [::1] when it cannot bind an IPv4 port;
// internal/testnet.NewServer does not. Syntactic, like buildgate's checks: no
// non-test file of this package may start a server any other way.
func TestTheFakeIsServedOnlyThroughTestnet(t *testing.T) {
	gl := glabtest.New(t)
	if !strings.HasPrefix(gl.URL(), "http://127.0.0.1:") {
		t.Fatalf("the fake's URL = %q, want http://127.0.0.1:PORT", gl.URL())
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files found (%v)", err)
	}
	starts := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"httptest.NewServer(", "httptest.NewUnstartedServer(", "httptest.NewTLSServer("} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s calls %s: use testnet.NewServer, whose URL is always IPv4 loopback", f, banned)
			}
		}
		starts += strings.Count(string(src), "testnet.NewServer(")
	}
	if starts != 1 {
		t.Errorf("testnet.NewServer is called %d times in this package, want exactly 1 (New)", starts)
	}
}
