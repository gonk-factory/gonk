package intake

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

func newOnboarder(gl *glabtest.Server) *GitLabOnboarder {
	// This test instance's operator ladder is qwen-local -- a valid operator
	// catalog, seeded into the template like the chart does in production (Plan 05).
	return &GitLabOnboarder{
		GL: gl.Client(), BotUserID: 7, BotUsername: "gonk", Version: "v0",
		InstanceLadder: []string{"qwen-local"}, Obs: NopObserver{},
	}
}

func TestOnboardOpensMRWithTheConfig(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()

	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatalf("Onboard = %v", err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 || mrs[0].TargetBranch != "main" {
		t.Fatalf("mrs = %+v", mrs)
	}
	if !strings.Contains(mrs[0].Description, "monthly_cost_usd") {
		t.Fatal("MR description is not the rendered explanation")
	}
	raw, err := gl.Client().GetRawFile(ctx, p.ID, ConfigPath, OnboardBranch, 65536)
	want, _ := RenderDefaultConfig(o.InstanceLadder)
	if err != nil || string(raw) != string(want) {
		t.Fatalf("branch does not carry the exact default config: %v", err)
	}
	// The commit must not land on the default branch. Only a human merging can
	// do that.
	if _, err := gl.Client().GetRawFile(ctx, p.ID, ConfigPath, "main", 65536); !glab.IsNotFound(err) {
		t.Fatal("onboarding must not write to the default branch")
	}
}

func TestOnboardIsIdempotent(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatalf("second Onboard = %v", err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 {
		t.Fatalf("opened %d MRs; onboarding must never nag", len(mrs))
	}
}

// Spec 5.3: closing the MR unmerged is a decline. Do not re-open it.
func TestDeclinedIsNotReopened(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}
	gl.SetMRState(p.ID, 1, "closed", time.Now())

	declined, err := o.Declined(ctx, p.Project)
	if err != nil || !declined {
		t.Fatalf("Declined = %v, %v; want true", declined, err)
	}
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 {
		t.Fatal("a declined project was pestered with a second onboarding MR")
	}
}

// AD-3: re-inviting the bot (a fresh membership, created after the decline)
// re-opens the question.
func TestReinviteClearsDecline(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}
	closedAt := time.Now().Add(-time.Hour)
	gl.SetMRState(p.ID, 1, "closed", closedAt)
	joined := time.Now()
	gl.AddMember(p.ID, glab.Member{ID: 7, Username: "gonk", AccessLevel: glab.AccessDeveloper, CreatedAt: &joined})

	declined, err := o.Declined(ctx, p.Project)
	if err != nil {
		t.Fatal(err)
	}
	if declined {
		t.Fatal("a re-invite after a decline must re-open onboarding (spec 5.3)")
	}
}

// Spec 5.3: without Developer, the bot cannot push a branch. It says so in an
// issue instead of failing silently — and it says so exactly once.
func TestInsufficientRoleOpensOneIssue(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessReporter)
	o := newOnboarder(gl)
	ctx := context.Background()

	for range 3 {
		if err := o.Onboard(ctx, p.Project); err != nil {
			t.Fatalf("Onboard = %v", err)
		}
	}
	issues, _ := gl.Client().ListIssues(ctx, p.ID, glab.IssueListOptions{State: "opened", Labels: OnboardingIssueLabel})
	if len(issues) != 1 {
		t.Fatalf("opened %d issues, want exactly 1", len(issues))
	}
	if !strings.Contains(issues[0].Title, "Developer") {
		t.Fatalf("issue must name the role it needs: %q", issues[0].Title)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 0 {
		t.Fatal("must not attempt an MR without push rights")
	}
}

// A leftover branch from a crashed run must not wedge onboarding forever.
func TestOnboardRecoversFromOrphanBranch(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	ctx := context.Background()
	if _, err := gl.Client().CreateBranch(ctx, p.ID, OnboardBranch, "main"); err != nil {
		t.Fatal(err)
	}
	if err := newOnboarder(gl).Onboard(ctx, p.Project); err != nil {
		t.Fatalf("Onboard = %v", err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 {
		t.Fatalf("mrs = %+v", mrs)
	}
}
