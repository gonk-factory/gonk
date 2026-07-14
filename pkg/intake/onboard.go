package intake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// OnboardingIssueLabel marks the "I need Developer" issue so the bot can find it
// again and not open a second one.
const OnboardingIssueLabel = "gonk::onboarding"

// GitLabOnboarder opens the deterministic onboarding MR (spec 5.3).
//
// DETERMINISTIC. NO MODEL CALL. Every byte it writes comes from render.go.
type GitLabOnboarder struct {
	GL             OnboardGitLab
	BotUserID      int64
	BotUsername    string
	Version        string
	InstanceLadder []string // operator instance ladder seeded into the template (OD-B; from chart/operator config, Plan 05)
	Obs            Observer
}

// OnboardGitLab is the API slice onboarding needs.
type OnboardGitLab interface {
	ListMergeRequests(ctx context.Context, projectID int64, o glab.MRListOptions) ([]glab.MergeRequest, error)
	CreateMergeRequest(ctx context.Context, projectID int64, o glab.MROptions) (*glab.MergeRequest, error)
	CreateBranch(ctx context.Context, projectID int64, branch, ref string) (*glab.Branch, error)
	CreateCommit(ctx context.Context, projectID int64, o glab.CommitOptions) (*glab.Commit, error)
	ListMembers(ctx context.Context, projectID int64) ([]glab.Member, error)
	ListIssues(ctx context.Context, projectID int64, o glab.IssueListOptions) ([]glab.Issue, error)
	CreateIssue(ctx context.Context, projectID int64, o glab.IssueOptions) (*glab.Issue, error)
}

func (o *GitLabOnboarder) Onboard(ctx context.Context, p glab.Project) error {
	mrs, err := o.GL.ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if err != nil {
		return fmt.Errorf("onboard: list MRs: %w", err)
	}
	if len(mrs) > 0 {
		// Open: the ball is in the maintainer's court. Closed: declined (and
		// Declined() decides whether a re-invite reopens it). Merged: the project
		// has a config and is not our problem. In no case do we open a second MR.
		return nil
	}

	// Spec 5.3: no Developer, no branch. Say what is needed instead of failing.
	if p.EffectiveAccess() < glab.AccessDeveloper {
		return o.requestAccess(ctx, p)
	}

	target := p.DefaultBranch
	if target == "" {
		target = "main"
	}

	// Render the committed .gonk.yml once, with the operator's instance ladder
	// (OD-B) -- the SAME bytes the MR body embeds, so they cannot disagree.
	cfgBytes, err := RenderDefaultConfig(o.InstanceLadder)
	if err != nil {
		return fmt.Errorf("onboard: render config: %w", err)
	}

	// A branch may survive a crashed earlier attempt; that is not an error.
	if _, err := o.GL.CreateBranch(ctx, p.ID, OnboardBranch, target); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("onboard: create branch: %w", err)
	}

	action := "create"
	if _, err := o.GL.CreateCommit(ctx, p.ID, glab.CommitOptions{
		Branch:        OnboardBranch,
		CommitMessage: o.commitMessage(),
		Actions: []glab.CommitAction{{
			Action: action, FilePath: ConfigPath, Content: string(cfgBytes),
		}},
	}); err != nil {
		// The file may already exist on a leftover branch: retry as an update.
		if _, uerr := o.GL.CreateCommit(ctx, p.ID, glab.CommitOptions{
			Branch:        OnboardBranch,
			CommitMessage: o.commitMessage(),
			Actions: []glab.CommitAction{{
				Action: "update", FilePath: ConfigPath, Content: string(cfgBytes),
			}},
		}); uerr != nil {
			return fmt.Errorf("onboard: commit: %w", err)
		}
	}

	body, err := RenderOnboardingMR(OnboardingContext{
		Project: p.PathWithNamespace, BotUsername: o.BotUsername, Version: o.Version,
		Ladder: o.InstanceLadder,
	})
	if err != nil {
		return err
	}

	if _, err := o.GL.CreateMergeRequest(ctx, p.ID, glab.MROptions{
		SourceBranch:       OnboardBranch,
		TargetBranch:       target,
		Title:              "gonk: enable automated issue triage",
		Description:        body,
		AssigneeID:         o.pickAssignee(ctx, p),
		RemoveSourceBranch: true,
	}); err != nil {
		return fmt.Errorf("onboard: create MR: %w", err)
	}
	o.Obs.OnboardingResult("mr_opened")
	return nil
}

// commitMessage carries a provenance trailer (spec 6.1). This commit had no
// model in it at all, and says so.
func (o *GitLabOnboarder) commitMessage() string {
	return "chore: add .gonk.yml (gonk onboarding)\n\n" +
		"Generated-By: gonk/" + o.Version + " (deterministic onboarding; no model)\n"
}

// pickAssignee: the lowest-numbered Maintainer/Owner, deterministically. GitLab
// CE supports a single assignee (multiple assignees is a paid feature), so the
// choice must be stable rather than arbitrary -- two reconcile passes must not
// disagree. 0 means "leave unassigned", which is legal.
func (o *GitLabOnboarder) pickAssignee(ctx context.Context, p glab.Project) int64 {
	members, err := o.GL.ListMembers(ctx, p.ID)
	if err != nil {
		return 0
	}
	var ids []int64
	for _, m := range members {
		if m.ID != o.BotUserID && m.AccessLevel >= glab.AccessMaintainer {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return 0
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids[0]
}

// requestAccess opens exactly one issue naming the role gonk needs (spec 5.3).
// Idempotent via the label: no repeat nagging every reconcile pass.
func (o *GitLabOnboarder) requestAccess(ctx context.Context, p glab.Project) error {
	existing, err := o.GL.ListIssues(ctx, p.ID, glab.IssueListOptions{State: "opened", Labels: OnboardingIssueLabel})
	if err != nil {
		return fmt.Errorf("onboard: list issues: %w", err)
	}
	if len(existing) > 0 {
		return nil
	}
	_, err = o.GL.CreateIssue(ctx, p.ID, glab.IssueOptions{
		Title: "gonk needs the Developer role to open its onboarding merge request",
		Description: fmt.Sprintf(
			"`@%s` was invited to this project, but with a role below Developer, so it "+
				"cannot push the `%s` branch that carries its onboarding merge request.\n\n"+
				"Grant `@%s` the **Developer** role and it will open the merge request on its "+
				"next reconciliation pass (within ten minutes). Nothing else will happen until "+
				"a human merges that request.\n\n"+
				"If gonk was invited by mistake, remove it from the project and close this issue.",
			o.BotUsername, OnboardBranch, o.BotUsername),
		Labels: OnboardingIssueLabel,
	})
	if err != nil {
		return fmt.Errorf("onboard: create access issue: %w", err)
	}
	o.Obs.OnboardingResult("access_requested")
	return nil
}

// Declined: an onboarding MR was closed without merging, and the bot has not
// been re-invited since (spec 5.3, AD-3). Both facts are read from GitLab --
// nothing about a decline is persisted on gonk's side.
func (o *GitLabOnboarder) Declined(ctx context.Context, p glab.Project) (bool, error) {
	mrs, err := o.GL.ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "closed"})
	if err != nil {
		return false, err
	}
	var closedAt time.Time
	for _, mr := range mrs {
		at := mr.ClosedAt
		if at == nil {
			at = mr.UpdatedAt
		}
		if at != nil && at.After(closedAt) {
			closedAt = *at
		}
	}
	if closedAt.IsZero() {
		return false, nil
	}
	// Re-invited after the decline? Then the question is open again.
	members, err := o.GL.ListMembers(ctx, p.ID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.ID == o.BotUserID && m.CreatedAt != nil && m.CreatedAt.After(closedAt) {
			return false, nil
		}
	}
	return true, nil
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}
