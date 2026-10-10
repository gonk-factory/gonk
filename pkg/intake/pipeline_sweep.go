package intake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/ghook"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// PipelineSweeper is what the pipeline backfill needs from *Dispatch: the
// SAME entry point the webhook uses (Handle), plus the run anchor it decided
// on and a hook to suppress an anchor the sweep fired recently. Nil disables
// the backfill.
type PipelineSweeper interface {
	HandlePipeline(ctx context.Context, ev *ghook.Event, suppressed func(anchor string) bool) (bool, string)
}

// PipelineSweepGitLab is the API slice the backfill reads. *glab.Client
// satisfies it. Kept apart from GitLab so the reconciler's existing fakes need
// not grow pipeline methods.
type PipelineSweepGitLab interface {
	ListPipelines(ctx context.Context, projectID int64, ref, status string, perPage int) ([]glab.Pipeline, error)
	ListMRPipelines(ctx context.Context, projectID, mrIID int64) ([]glab.Pipeline, error)
	GetPipeline(ctx context.Context, projectID, pipelineID int64) (*glab.Pipeline, error)
	ListMergeRequests(ctx context.Context, projectID int64, o glab.MRListOptions) ([]glab.MergeRequest, error)
}

// Backfill bounds (D4). Spend guards as much as API-call guards: each one is
// a cap on how many orders one pass can ask meter for.
const (
	// PipelineSweepRefDepth: how many of the default branch's newest
	// pipelines are searched for the newest terminal-for-health one.
	PipelineSweepRefDepth = 20
	// PipelineSweepMRLimit / PipelineSweepMRMaxAge: at most this many open
	// MRs, updated within this window, are looked at per pass.
	PipelineSweepMRLimit  = 5
	PipelineSweepMRMaxAge = 24 * time.Hour
	// PipelineSweepSuppressWindow: how long the sweep remembers a run anchor
	// it got a real fire for before it will ask meter about it again.
	PipelineSweepSuppressWindow = time.Hour
)

// newestHealthSignal is D4's selection rule over a newest-first list: the
// first pipeline whose status says something about health (success or
// failed). skipped, canceled, manual, created and running states are passed
// over, and so are child pipelines (source parent_pipeline): a child's
// failure surfaces as its parent's failed bridge, and a child is never a
// target (D1).
func newestHealthSignal(ps []glab.Pipeline) *glab.Pipeline {
	sorted := append([]glab.Pipeline(nil), ps...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID > sorted[j].ID })
	for i := range sorted {
		if sorted[i].Source == sourceParentPipeline {
			continue
		}
		if pipelinectx.TerminalForHealth(sorted[i].Status) {
			return &sorted[i]
		}
	}
	return nil
}

// sweepPipelines is the pipeline half of "reconciliation is the correctness
// path" (D4): a failed pipeline whose webhook was lost, sent before the
// project opted in, or dropped landing_unreadable, is found here. It is also
// how an ALREADY-red default branch is picked up on the first pass after a
// project opts in.
//
// Per opted-in project, per pass: (a) the default branch's newest
// terminal-for-health pipeline among its last PipelineSweepRefDepth; (b) the
// same rule over each of up to PipelineSweepMRLimit open same-project MRs
// updated in the last PipelineSweepMRMaxAge. Each that is `failed` and not
// bot-triggered is handed to Dispatch as a synthesized Pipeline Hook event --
// the same Decide rules, Gate 1 and already-handled check the webhook takes.
//
// It returns (swept, dispatched, err): attempts handed to Dispatch, real
// fires, and every read error joined (one target failing does not stop the
// others).
func (r *Reconciler) sweepPipelines(ctx context.Context, p glab.Project) (int, int, error) {
	if r.Pipelines == nil || r.PipelineGL == nil {
		return 0, 0, nil
	}
	entry, ok := r.Cache.Get(p.ID)
	if !ok {
		return 0, 0, nil
	}
	// The real rule, not a re-derivation: a project that has not opted in is
	// not even listed (and its hook has no pipeline_events either).
	if _, allowed := gate(entry, atags.TriggerPipelineDiagnosis); !allowed {
		return 0, 0, nil
	}
	gl := r.PipelineGL
	now := time.Now()

	fired := make(map[string]time.Time, len(entry.PipelineDispatchedAt))
	for a, at := range entry.PipelineDispatchedAt {
		if now.Sub(at) < PipelineSweepSuppressWindow {
			fired[a] = at
		}
	}
	suppressed := func(anchor string) bool { _, ok := fired[anchor]; return ok }

	type candidate struct {
		id int64
		mr *glab.MergeRequest
	}
	var cands []candidate
	var errs []error

	// (a) the default branch.
	if db := entry.Project.DefaultBranch; db != "" {
		ps, err := gl.ListPipelines(ctx, p.ID, db, "", PipelineSweepRefDepth)
		if err != nil {
			errs = append(errs, fmt.Errorf("pipelines on %s: %w", db, err))
		} else if pl := newestHealthSignal(ps); pl != nil && pl.Status == pipelineStatusFailed {
			cands = append(cands, candidate{id: pl.ID})
		}
	}

	// (b) recently updated open MRs from this project.
	cutoff := now.Add(-PipelineSweepMRMaxAge)
	mrs, err := gl.ListMergeRequests(ctx, p.ID, glab.MRListOptions{State: mrStateOpened, UpdatedAfter: cutoff})
	if err != nil {
		errs = append(errs, fmt.Errorf("open merge requests: %w", err))
	}
	var recent []glab.MergeRequest
	for _, mr := range mrs {
		// Re-check the server-side bounds: a filter that only works when the
		// server cooperates is not a spend guard.
		if mr.State != mrStateOpened || mr.UpdatedAt == nil || mr.UpdatedAt.Before(cutoff) {
			continue
		}
		if mr.SourceProjectID != p.ID { // a fork MR is dropped by Decide anyway
			continue
		}
		recent = append(recent, mr)
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].UpdatedAt.After(*recent[j].UpdatedAt) })
	if len(recent) > PipelineSweepMRLimit {
		recent = recent[:PipelineSweepMRLimit]
	}
	for i := range recent {
		mr := recent[i]
		ps, err := gl.ListMRPipelines(ctx, p.ID, mr.IID)
		if err != nil {
			errs = append(errs, fmt.Errorf("pipelines of MR !%d: %w", mr.IID, err))
			continue
		}
		if pl := newestHealthSignal(ps); pl != nil && pl.Status == pipelineStatusFailed {
			cands = append(cands, candidate{id: pl.ID, mr: &mr})
		}
	}

	swept, dispatched := 0, 0
	for _, c := range cands {
		// The list shape carries no user and no tag; the detail does.
		full, err := gl.GetPipeline(ctx, p.ID, c.id)
		if err != nil {
			errs = append(errs, fmt.Errorf("pipeline %d: %w", c.id, err))
			continue
		}
		// The loop guard the webhook gets from the receiver (bot_authored).
		if full.User.ID == r.BotUserID {
			continue
		}
		pl := &ghook.Pipeline{
			ID: full.ID, IID: full.IID, Ref: full.Ref, Tag: full.Tag, SHA: full.SHA,
			Source: full.Source, Status: full.Status,
		}
		if c.mr != nil && full.Source == sourceMergeRequest {
			pl.MRIID, pl.MRSourceProjectID = c.mr.IID, c.mr.SourceProjectID
		}
		did, anchor := r.Pipelines.HandlePipeline(ctx, &ghook.Event{
			Kind: ghook.KindPipeline,
			Project: ghook.Project{
				ID: entry.Project.ID, PathWithNamespace: entry.Project.PathWithNamespace,
				DefaultBranch: entry.Project.DefaultBranch, WebURL: entry.Project.WebURL,
			},
			User:     ghook.User{ID: full.User.ID, Username: full.User.Username},
			Pipeline: pl,
		}, suppressed)
		swept++
		if did {
			fired[anchor] = now
			dispatched++
		}
	}

	entry.PipelineDispatchedAt = fired
	r.Cache.Put(p.ID, entry)
	if swept > 0 {
		r.log().Info("pipeline sweep handed failed pipelines to dispatch",
			"project", p.PathWithNamespace, "count", swept, "dispatched", dispatched)
	}
	return swept, dispatched, errors.Join(errs...)
}
