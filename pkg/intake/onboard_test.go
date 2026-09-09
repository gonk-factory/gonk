package intake

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
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

// *** CRITERION 2 OF T-08. ***
// The onboarding merge request carries the `.agent/` seed in the SAME commit as
// `.gonk.yml`. The seed used to come from a metered scaffold session after the
// merge, which is what made a project's first triage wait on a model call.
func TestOnboardCommitsTheAgentSeed(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()

	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatalf("Onboard = %v", err)
	}

	seed, err := RenderAgentSeed(OnboardingContext{
		Project: "group/repo", BotUsername: o.BotUsername, Version: o.Version, Ladder: o.InstanceLadder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seed) != len(AgentSeedPaths) {
		t.Fatalf("rendered %d seed files, want %d", len(seed), len(AgentSeedPaths))
	}

	// .gonk.yml is still there, and every seed file landed with EXACTLY the
	// rendered bytes -- not merely "a file exists at that path".
	if raw, err := gl.Client().GetRawFile(ctx, p.ID, ConfigPath, OnboardBranch, 65536); err != nil {
		t.Fatalf("%s missing from the onboarding branch: %v", ConfigPath, err)
	} else if want, _ := RenderDefaultConfig(o.InstanceLadder); string(raw) != string(want) {
		t.Fatalf("%s is not the rendered config", ConfigPath)
	}
	for _, f := range seed {
		raw, err := gl.Client().GetRawFile(ctx, p.ID, f.Path, OnboardBranch, 1<<20)
		if err != nil {
			t.Fatalf("%s missing from the onboarding branch: %v", f.Path, err)
		}
		if string(raw) != f.Content {
			t.Errorf("%s content differs from the render", f.Path)
		}
		// Nothing may reach the default branch. Only a human merging can do that.
		if _, err := gl.Client().GetRawFile(ctx, p.ID, f.Path, "main", 1<<20); !glab.IsNotFound(err) {
			t.Errorf("%s was written to the default branch", f.Path)
		}
	}
}

// The seed's prose is RENDERED FROM THE CONFIG, not typed alongside it. This is
// the assertion that distinguishes the two: it renders from a config whose
// values are deliberately NOT the defaults, so a README carrying a hardcoded
// "gonk::" or "qwen-local" fails here even though it would look right against
// the real one.
func TestAgentSeedQuotesTheConfigsOwnValues(t *testing.T) {
	prefix, rung := "acme::", "zebra-local"
	cfg := &gonkcfg.ProjectConfig{Version: 1, Policy: gonkcfg.Policy{
		Ladder: []string{rung},
		Triage: gonkcfg.TriagePolicy{LabelPrefix: &prefix},
	}}
	seed, err := renderAgentSeedFrom(OnboardingContext{
		Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{rung},
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	readme := seed[0].Content
	if seed[0].Path != AgentDir+"/README.md" {
		t.Fatalf("first seed file is %q, want the README", seed[0].Path)
	}
	for _, want := range []string{prefix, rung} {
		if !strings.Contains(readme, want) {
			t.Errorf("README does not quote %q from the config:\n%s", want, readme)
		}
	}
	for _, notWant := range []string{"gonk::", "qwen-local"} {
		if strings.Contains(readme, notWant) {
			t.Errorf("README hardcodes the default %q instead of reading the config", notWant)
		}
	}
}

// And on the production path the value it quotes is the one the merge request
// actually commits: parse the committed .gonk.yml and look for ITS label prefix
// in the committed README.
func TestCommittedReadmeQuotesTheCommittedLabelPrefix(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}

	rawCfg, err := gl.Client().GetRawFile(ctx, p.ID, ConfigPath, OnboardBranch, 65536)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := gonkcfg.Load(rawCfg)
	if err != nil {
		t.Fatalf("the committed config does not load: %v", err)
	}
	if cfg.Triage.LabelPrefix == nil {
		t.Fatal("the committed config sets no triage.label_prefix")
	}
	readme, err := gl.Client().GetRawFile(ctx, p.ID, AgentDir+"/README.md", OnboardBranch, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), *cfg.Triage.LabelPrefix) {
		t.Fatalf("the committed README never states the committed label prefix %q", *cfg.Triage.LabelPrefix)
	}
	// It must also say what .agent/ is for and name both ways to fill it in.
	for _, want := range []string{"Navigator", "actions.scaffold: true", SeedNotFilledIn} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("the committed README never mentions %q", want)
		}
	}
}

// strictCommitGitLab enforces GitLab's real commit-action semantics, which the
// fake forge deliberately does not: "create" fails when the path is present and
// "update" fails when it is absent. Without it, a commit that mixes the two
// cases looks fine in tests and 400s in production.
type strictCommitGitLab struct {
	OnboardGitLab
	present map[string]bool
}

func (g *strictCommitGitLab) CreateCommit(ctx context.Context, projectID int64, o glab.CommitOptions) (*glab.Commit, error) {
	for _, a := range o.Actions {
		switch {
		case a.Action == "create" && g.present[a.FilePath]:
			return nil, fmt.Errorf("400: A file with this name already exists: %s", a.FilePath)
		case a.Action == "update" && !g.present[a.FilePath]:
			return nil, fmt.Errorf("400: A file with this name doesn't exist: %s", a.FilePath)
		}
	}
	for _, a := range o.Actions {
		g.present[a.FilePath] = true
	}
	return g.OnboardGitLab.CreateCommit(ctx, projectID, o)
}

// A crashed earlier attempt can leave `gonk/onboard` carrying .gonk.yml and no
// seed -- so ONE commit has to create some paths and update others. An
// all-create-then-all-update retry cannot express that mixture and wedges
// onboarding for the project until somebody deletes the branch by hand.
func TestOnboardRecoversFromABranchCarryingOnlyTheConfig(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	cfgBytes, err := RenderDefaultConfig([]string{"qwen-local"})
	if err != nil {
		t.Fatal(err)
	}
	p.PutFileOn(OnboardBranch, ConfigPath, cfgBytes)

	o := newOnboarder(gl)
	o.GL = &strictCommitGitLab{OnboardGitLab: gl.Client(), present: map[string]bool{ConfigPath: true}}
	ctx := context.Background()

	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatalf("Onboard = %v; a half-written onboarding branch must be recoverable", err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 {
		t.Fatalf("opened %d merge requests, want 1", len(mrs))
	}
	for _, path := range AgentSeedPaths {
		if _, err := gl.Client().GetRawFile(ctx, p.ID, path, OnboardBranch, 1<<20); err != nil {
			t.Errorf("%s did not land on the recovered branch: %v", path, err)
		}
	}
}
