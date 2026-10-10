package glabtest_test

import (
	"context"
	"testing"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
)

// The fix-MR lifecycle (gonk-2usw part GLAB) through the real client against
// the fake: branch, commit, MR with labels, edit, approvals, merge pinned on a
// sha, branch delete.
func TestFakeFixMRLifecycle(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	p.PutFile("README.md", []byte("x"))
	ctx, c := context.Background(), gl.Client()

	if _, err := c.CreateBranch(ctx, p.ID, "gonk/fix-1", "main"); err != nil {
		t.Fatal(err)
	}
	cm, err := c.CreateCommit(ctx, p.ID, glab.CommitOptions{Branch: "gonk/fix-1", CommitMessage: "m",
		Actions: []glab.CommitAction{{Action: "update", FilePath: "README.md", Content: "y"}}})
	if err != nil {
		t.Fatal(err)
	}
	mr, err := c.CreateMergeRequest(ctx, p.ID, glab.MROptions{SourceBranch: "gonk/fix-1", TargetBranch: "main",
		Title: "t", Description: "d", Labels: []string{"gonk::pipeline-fix"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mr.Labels) != 1 || mr.Labels[0] != "gonk::pipeline-fix" {
		t.Fatalf("labels = %v", mr.Labels)
	}
	if mr, err = c.UpdateMergeRequest(ctx, p.ID, mr.IID, glab.MRUpdate{Description: sp("d2"), AddLabels: []string{"gonk::fixed"}, RemoveLabels: []string{"gonk::pipeline-fix"}}); err != nil {
		t.Fatal(err)
	}
	if mr.Description != "d2" || len(mr.Labels) != 1 || mr.Labels[0] != "gonk::fixed" {
		t.Fatalf("mr = %+v", mr)
	}

	// Not approved yet; then approved by a human.
	if a, err := c.GetMRApprovals(ctx, p.ID, mr.IID); err != nil || a.Approved {
		t.Fatalf("approvals = %+v, %v", a, err)
	}
	gl.SetMRApprovals(p.ID, mr.IID, glab.User{ID: 3, Username: "dev"})
	if a, err := c.GetMRApprovals(ctx, p.ID, mr.IID); err != nil || !a.Approved || a.ApprovedBy[0].User.Username != "dev" {
		t.Fatalf("approvals = %+v, %v", a, err)
	}

	// The head the MR was judged at is cm.ID. A push moves it: the pinned
	// merge is refused and nothing merges.
	gl.SetMRHead(p.ID, mr.IID, cm.ID)
	gl.SetMRHead(p.ID, mr.IID, "f"+cm.ID[1:])
	if _, err := c.AcceptMR(ctx, p.ID, mr.IID, cm.ID); !glab.IsShaMismatch(err) {
		t.Fatalf("merge after a push: err = %v, want a sha mismatch", err)
	}
	if got, _ := c.GetMergeRequest(ctx, p.ID, mr.IID); got.State != "opened" {
		t.Fatalf("state = %q after a refused merge", got.State)
	}
	gl.SetMRHead(p.ID, mr.IID, cm.ID)
	merged, err := c.AcceptMR(ctx, p.ID, mr.IID, cm.ID)
	if err != nil || merged.State != "merged" {
		t.Fatalf("merged = %+v, %v", merged, err)
	}

	if err := c.DeleteBranch(ctx, p.ID, "gonk/fix-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBranch(ctx, p.ID, "gonk/fix-1"); !glab.IsNotFound(err) {
		t.Fatalf("second delete err = %v, want IsNotFound", err)
	}
}

func TestFakeRefusesToMergeADraftOrNotMergeable(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	ctx, c := context.Background(), gl.Client()
	gl.AddMR(p.ID, glab.MergeRequest{IID: 1, SHA: shaA, Draft: true})
	gl.AddMR(p.ID, glab.MergeRequest{IID: 2, SHA: shaA, DetailedMergeStatus: "not_approved"})
	for _, iid := range []int64{1, 2} {
		if mr, err := c.AcceptMR(ctx, p.ID, iid, shaA); err == nil || mr != nil {
			t.Fatalf("iid %d: got %+v, %v; want a refusal", iid, mr, err)
		}
	}
}

func TestFakeMRDiffs(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("g/r", glab.AccessMaintainer)
	gl.AddMR(p.ID, glab.MergeRequest{IID: 1})
	gl.SetMRDiffs(p.ID, 1, glab.MRDiff{OldPath: "a.md", NewPath: "a.md", Diff: "@@"})
	ds, err := gl.Client().ListMRDiffs(context.Background(), p.ID, 1)
	if err != nil || len(ds) != 1 || ds[0].NewPath != "a.md" {
		t.Fatalf("diffs = %+v, %v", ds, err)
	}
	if _, err := gl.Client().ListMRDiffs(context.Background(), p.ID, 99); !glab.IsNotFound(err) {
		t.Fatalf("unknown MR err = %v, want IsNotFound", err)
	}
}

func sp(s string) *string { return &s }
