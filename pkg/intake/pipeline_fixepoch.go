// The fix epoch's reads (gonk-2usw plan section 3.3).
//
// pipelinectx.FixEpoch is the pure rule; this file gathers what it needs:
// which gonk pipeline-fix MRs have MERGED into the target's branch, and the
// failing pipeline's commit and creation time. Every read is a GET and every
// fact is GitLab's -- none comes from a session, a batch or the repository.
//
// The cost on a target no fix has merged into is ONE list call (the bot's
// merged MRs into that branch, normally empty). Nothing else is read until
// there is a merged fix to place the pipeline against.
package intake

import (
	"context"
	"fmt"

	"github.com/gonk-factory/gonk/pkg/ghook"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// mrStateMerged is GitLab's state of a merged MR.
const mrStateMerged = "merged"

// MergedFixSource answers which gonk pipeline-fix MRs have merged in a
// project. targetBranch restricts the answer to fixes merged INTO that
// branch; "" means every branch (each MergedFix names its own).
//
// *GitLabMergedFixes is the real one. The interface exists so the identity
// rule can be tested without a GitLab, and so a later part that holds the
// same facts elsewhere (the fix record, once part APPLY writes one) can
// supply them without this package changing.
type MergedFixSource interface {
	MergedFixes(ctx context.Context, projectID int64, targetBranch string) ([]pipelinectx.MergedFix, error)
}

// MergedFixGitLab is the API slice GitLabMergedFixes reads. *glab.Client
// satisfies it.
type MergedFixGitLab interface {
	ListMergeRequests(ctx context.Context, projectID int64, o glab.MRListOptions) ([]glab.MergeRequest, error)
}

// GitLabMergedFixes reads merged fix MRs from GitLab. BotUserID is REQUIRED.
//
// An MR is a merged gonk pipeline fix when ALL hold, each re-checked on the
// object GitLab returned (the server-side filters only bound the listing):
//
//   - state is "merged";
//   - its AUTHOR is the bot (BotUserID) -- gonk opened it;
//   - its source branch is a fix branch (pipelinectx.IsFixBranch), in THIS
//     project (not a fork);
//   - it targets this project.
//
// Labels are deliberately NOT read: `gonk::pipeline-fix` can be removed by any
// member, and a fix that lost its label has still merged.
type GitLabMergedFixes struct {
	GL        MergedFixGitLab
	BotUserID int64
}

// MergedFixes implements MergedFixSource. A merged fix MR GitLab reports
// without a merged_at is an error, never skipped: skipping it would silently
// file the next failure under the run that fix ended.
func (g *GitLabMergedFixes) MergedFixes(ctx context.Context, projectID int64, targetBranch string) ([]pipelinectx.MergedFix, error) {
	if g.BotUserID <= 0 {
		return nil, fmt.Errorf("merged fixes: BotUserID must be a positive GitLab user id, got %d", g.BotUserID)
	}
	mrs, err := g.GL.ListMergeRequests(ctx, projectID, glab.MRListOptions{
		State: mrStateMerged, AuthorID: g.BotUserID, TargetBranch: targetBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("merged fixes: bot-authored merged merge requests: %w", err)
	}
	var out []pipelinectx.MergedFix
	for _, mr := range mrs {
		if mr.State != mrStateMerged || mr.Author.ID != g.BotUserID {
			continue
		}
		if !pipelinectx.IsFixBranch(mr.SourceBranch) {
			continue
		}
		if mr.SourceProjectID != projectID || mr.TargetProjectID != projectID {
			continue
		}
		if targetBranch != "" && mr.TargetBranch != targetBranch {
			continue
		}
		if mr.IID < 1 {
			return nil, fmt.Errorf("merged fixes: a merged fix merge request from %s carries no iid", mr.SourceBranch)
		}
		if mr.MergedAt == nil || mr.MergedAt.IsZero() {
			return nil, fmt.Errorf("merged fixes: merged fix !%d carries no merged_at", mr.IID)
		}
		out = append(out, pipelinectx.MergedFix{
			IID: mr.IID, TargetBranch: mr.TargetBranch, MergedAt: *mr.MergedAt,
			LandedSHAs: []string{mr.MergeCommitSHA, mr.SquashCommitSHA, mr.SHA},
		})
	}
	return out, nil
}

// fixEpoch is the fix epoch of failing pipeline p on target t (plan 3.3),
// given the target's last green pipeline (the zero Pipeline when none).
//
// The branch a fix for the target merges INTO is the ref itself for a ref
// target and the MR's SOURCE branch for an MR target. A branch pipeline that
// landed on an MR target ran on that very branch (D1). An MR-event pipeline's
// ref is refs/merge-requests/<iid>/head, so its branch is read from the MR --
// but only once the project is known to have a merged fix at all.
func (g *GitLabPipelineFacts) fixEpoch(ctx context.Context, pid int64, p *ghook.Pipeline, t pipelinectx.Target, last glab.Pipeline) (int64, error) {
	src := g.Fixes
	if src == nil {
		src = &GitLabMergedFixes{GL: g.GL, BotUserID: g.BotUserID}
	}
	branch := ""
	switch {
	case t.Kind == pipelinectx.TargetKindRef:
		branch = t.Ref
	case p.Source != sourceMergeRequest:
		branch = p.Ref
	}
	fixes, err := src.MergedFixes(ctx, pid, branch)
	if err != nil {
		return 0, err
	}
	if len(fixes) == 0 {
		return 0, nil
	}
	if branch == "" {
		mr, err := g.GL.GetMergeRequest(ctx, pid, t.MRIID)
		if err != nil {
			return 0, fmt.Errorf("merge request !%d: %w", t.MRIID, err)
		}
		if mr.SourceBranch == "" {
			return 0, fmt.Errorf("merge request !%d carries no source branch", t.MRIID)
		}
		branch = mr.SourceBranch
	}
	// Re-check the branch whatever the source was asked: a fix merged into
	// another branch says nothing about this target.
	var here []pipelinectx.MergedFix
	for _, x := range fixes {
		if x.TargetBranch == branch {
			here = append(here, x)
		}
	}
	if len(here) == 0 {
		return 0, nil
	}
	// The webhook payload's own timestamps are not parsed anywhere; the API's
	// pipeline object is the one source of the creation time and the commit.
	full, err := g.GL.GetPipeline(ctx, pid, p.ID)
	if err != nil {
		return 0, fmt.Errorf("pipeline %d: %w", p.ID, err)
	}
	return pipelinectx.FixEpoch(here, pipelinectx.FixEpochFacts{
		PipelineSHA: full.SHA, PipelineCreatedAt: full.CreatedAt,
		LastSuccess: last.ID, LastSuccessSHA: last.SHA, LastSuccessCreatedAt: last.CreatedAt,
	})
}
