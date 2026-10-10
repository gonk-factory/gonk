// Pipeline intake (gonk-tios P3): a failed GitLab pipeline becomes ONE
// pipeline-diagnosis order per RUN, never one per pipeline.
//
// The split is the same one the issue path has: a PURE decision
// (decidePipeline: entry + event + facts -> Decision, no IO, no clock) and an
// IO step that gathers the GitLab facts the decision needs
// (GitLabPipelineFacts.Gather). Decide calls decidePipeline with NO facts
// first, so every drop that needs no API call -- the action is off, the
// pipeline did not fail, it is a child or a tag or a fork -- is made before a
// single GitLab request; only a pipeline that survives those is worth reading.
//
// Intake writes NOTHING to GitLab on this path (D9): not on a drop, not on a
// Gate-1 deny. The run note is gonk-gate's.
package intake

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/ghook"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// Pipeline drop reasons. Decision-record and metric values: keep them stable.
const (
	ReasonPipelineNotFailed        = "pipeline_not_failed"
	ReasonRefNotConfigured         = "ref_not_configured"
	ReasonForkMRUnsupported        = "fork_mr_unsupported"
	ReasonChildPipelineUnsupported = "child_pipeline_unsupported"
	ReasonRunAlreadyHandled        = "run_already_handled"
	ReasonLandingUnreadable        = "landing_unreadable"
	// ReasonMRPipelineWithoutMR: source merge_request_event but the payload
	// names no MR (iid < 1). Nothing to land on.
	ReasonMRPipelineWithoutMR = "mr_pipeline_without_mr"
	// ReasonPipelineIdentityInvalid: the facts did not form a valid anchor.
	ReasonPipelineIdentityInvalid = "pipeline_identity_invalid"
	// ReasonPipelineFactsNeeded is decidePipeline's answer when every
	// IO-free rule passed and it was given no facts. Handle reads it as "go
	// and gather"; it is never a final decision.
	ReasonPipelineFactsNeeded = "pipeline_facts_needed"
	// ReasonPipelineFactsError: a GitLab read the run identity depends on
	// failed. Fail closed; the backfill sweep retries.
	ReasonPipelineFactsError = "pipeline_facts_error"
	// ReasonSweepSuppressed: the backfill sweep already fired this anchor
	// inside PipelineSweepSuppressWindow.
	ReasonSweepSuppressed = "sweep_suppressed"
	// ReasonMalformedEvent: an event whose kind-specific object is missing
	// (never produced by ghook.ParseEvent; a guard for synthesized events).
	ReasonMalformedEvent = "malformed_event"
)

// GitLab pipeline values intake branches on.
const (
	pipelineStatusFailed  = "failed"
	pipelineStatusSuccess = "success"
	sourceMergeRequest    = "merge_request_event"
	sourceParentPipeline  = "parent_pipeline"
	mrStateOpened         = "opened"
)

// PipelineFacts is everything decidePipeline needs from GitLab, gathered by
// PipelineFactSource.Gather. It is plain data so the decision stays pure and
// table-testable.
type PipelineFacts struct {
	// CommitMRsLooked: CommitMRs was fetched (only done for a branch pipeline,
	// where the target depends on it). CommitMRs is every MR containing the
	// pipeline's SHA, any state; decidePipeline filters.
	CommitMRsLooked bool
	CommitMRs       []glab.MergeRequest
	// FailedJobs: the latest attempt of each failed job. FailureReason: the
	// pipeline-level failure_reason (read only when no job counts).
	FailedJobs    []pipelinectx.FailedJob
	FailureReason string
	// LastSuccess: id of the newest green pipeline on the target, 0 if none.
	LastSuccess int64
	// FixEpoch: iid of the merged gonk pipeline-fix MR this pipeline comes
	// after (pipelinectx.FixEpoch, gonk-2usw 3.3), 0 if none. Part of the run
	// identity: a merged fix ends the run it was written for.
	FixEpoch int64
	// RunStates: the state of every BOT-AUTHORED run marker for this run's
	// anchor found on the landing (normally zero or one).
	RunStates []string
	// LandingErr: the landing could not be read. Fail closed (D3).
	LandingErr error
}

// PipelineFactSource gathers PipelineFacts. *GitLabPipelineFacts is the real
// one; nil on a Dispatch means pipeline events are never dispatched.
type PipelineFactSource interface {
	Gather(ctx context.Context, e Entry, ev *ghook.Event) (*PipelineFacts, error)
}

// defaultBranchOf prefers the reconciled project (read from the API) over the
// payload copy.
func defaultBranchOf(e Entry, ev *ghook.Event) string {
	if e.Project.DefaultBranch != "" {
		return e.Project.DefaultBranch
	}
	return ev.Project.DefaultBranch
}

// pipelineTarget is D1's target rule, pure. With looked == false it stops
// (needLookup) at the one step that needs the commit's MRs; everything before
// that step is decided from the payload alone.
//
// Order: not failed; child; MR pipeline (fork refused); tag; a branch
// pipeline whose SHA has EXACTLY ONE open same-project MR from that branch;
// the default branch; everything else.
func pipelineTarget(ev *ghook.Event, defaultBranch string, mrs []glab.MergeRequest, looked bool) (t pipelinectx.Target, reason string, needLookup bool) {
	p := ev.Pipeline
	if p.Status != pipelineStatusFailed {
		return t, ReasonPipelineNotFailed, false
	}
	if p.Source == sourceParentPipeline {
		return t, ReasonChildPipelineUnsupported, false
	}
	if p.Source == sourceMergeRequest {
		if p.MRIID < 1 {
			return t, ReasonMRPipelineWithoutMR, false
		}
		if p.MRSourceProjectID != ev.Project.ID {
			return t, ReasonForkMRUnsupported, false
		}
		return pipelinectx.MRTarget(p.MRIID), "", false
	}
	if p.Tag {
		return t, ReasonRefNotConfigured, false
	}
	if !looked {
		return t, "", true
	}
	var open []int64
	for _, mr := range mrs {
		if mr.State == mrStateOpened && mr.SourceBranch == p.Ref && mr.SourceProjectID == ev.Project.ID {
			open = append(open, mr.IID)
		}
	}
	if len(open) == 1 {
		return pipelinectx.MRTarget(open[0]), "", false
	}
	if defaultBranch != "" && p.Ref == defaultBranch {
		return pipelinectx.RefTarget(p.Ref), "", false
	}
	return t, ReasonRefNotConfigured, false
}

// decidePipeline is the pure pipeline decision. f == nil means "no facts yet":
// it then applies every IO-free rule and answers ReasonPipelineFactsNeeded if
// they all pass. With facts it computes the run identity (D2) and applies the
// already-handled check (D3).
func decidePipeline(e Entry, ev *ghook.Event, f *PipelineFacts) Decision {
	if d, ok := gate(e, atags.TriggerPipelineDiagnosis); !ok {
		return d
	}
	if ev.Pipeline == nil {
		return Decision{Reason: ReasonMalformedEvent}
	}
	var mrs []glab.MergeRequest
	looked := false
	if f != nil {
		mrs, looked = f.CommitMRs, f.CommitMRsLooked
	}
	t, reason, needLookup := pipelineTarget(ev, defaultBranchOf(e, ev), mrs, looked)
	if reason != "" {
		return Decision{Reason: reason}
	}
	if f == nil || needLookup {
		return Decision{Reason: ReasonPipelineFactsNeeded}
	}

	fp := pipelinectx.Fingerprint(f.FailedJobs, f.FailureReason)
	anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, f.LastSuccess, f.FixEpoch)
	if err != nil {
		return Decision{Reason: ReasonPipelineIdentityInvalid}
	}
	session, err := pipelinectx.SessionKey(anchor)
	if err != nil {
		return Decision{Reason: ReasonPipelineIdentityInvalid}
	}
	// Everything below carries the identity, so a drop's record still names
	// the run it was about.
	d := Decision{
		Trigger: atags.TriggerPipelineDiagnosis, SessionKey: session, BeadAnchor: anchor,
		PipelineID: ev.Pipeline.ID, PipelineTarget: t.String(),
		PipelineFingerprint: fp, PipelineLastSuccess: f.LastSuccess, PipelineFixEpoch: f.FixEpoch,
	}

	// D3, fail closed: an unreadable landing, or a marker in a state this
	// binary does not know (a newer writer), is NOT "absent".
	if f.LandingErr != nil {
		d.Reason = ReasonLandingUnreadable
		return d
	}
	unknown := false
	for _, st := range f.RunStates {
		if pipelinectx.RunHandled(st) {
			d.Reason = ReasonRunAlreadyHandled
			return d
		}
		if !pipelinectx.KnownRunState(st) {
			unknown = true
		}
	}
	if unknown {
		d.Reason = ReasonLandingUnreadable
		return d
	}
	d.Dispatch = true
	return d
}

// PipelineGitLab is the API slice GitLabPipelineFacts reads. *glab.Client
// satisfies it. Every call is a GET.
type PipelineGitLab interface {
	ListMRsForCommit(ctx context.Context, projectID int64, sha string) ([]glab.MergeRequest, error)
	ListPipelineJobs(ctx context.Context, projectID, pipelineID int64, scope string, includeRetried bool) ([]glab.Job, error)
	GetPipeline(ctx context.Context, projectID, pipelineID int64) (*glab.Pipeline, error)
	ListPipelines(ctx context.Context, projectID int64, ref, status string, perPage int) ([]glab.Pipeline, error)
	ListMRPipelines(ctx context.Context, projectID, mrIID int64) ([]glab.Pipeline, error)
	ListMRNotes(ctx context.Context, projectID, mrIID int64) ([]glab.Note, error)
	// The fix epoch's reads (gonk-2usw 3.3): the bot's merged fix MRs, and the
	// source branch of an MR target.
	ListMergeRequests(ctx context.Context, projectID int64, o glab.MRListOptions) ([]glab.MergeRequest, error)
	GetMergeRequest(ctx context.Context, projectID, mrIID int64) (*glab.MergeRequest, error)
	ListIssues(ctx context.Context, projectID int64, o glab.IssueListOptions) ([]glab.Issue, error)
	ListIssueNotes(ctx context.Context, projectID, issueIID int64) ([]glab.Note, error)
}

// GitLabPipelineFacts is the real PipelineFactSource. BotUserID is REQUIRED:
// only bot-authored landing notes and issues count (D3), and a zero id would
// make that filter match nothing -- every run would read as unhandled.
//
// Fixes answers which gonk pipeline-fix MRs have merged (the fix epoch,
// gonk-2usw 3.3). Nil means the real one over GL and BotUserID -- so a caller
// that wires nothing still gets the epoch, and cannot silently run without it.
type GitLabPipelineFacts struct {
	GL        PipelineGitLab
	BotUserID int64
	Fixes     MergedFixSource
}

// Gather reads, in order and only as far as needed: the commit's MRs (branch
// pipelines only), the failed jobs, the pipeline's own failure reason (only
// when no job counts), the target's last green pipeline, the merged fixes
// that set the fix epoch, and the landing's run markers. A read the identity
// depends on failing -- the fix epoch's included -- is an error; the landing
// read failing is recorded in LandingErr (decidePipeline fails closed on it).
func (g *GitLabPipelineFacts) Gather(ctx context.Context, e Entry, ev *ghook.Event) (*PipelineFacts, error) {
	if g.BotUserID <= 0 {
		return nil, fmt.Errorf("pipeline facts: BotUserID must be a positive GitLab user id, got %d", g.BotUserID)
	}
	if ev.Pipeline == nil {
		return nil, errors.New("pipeline facts: event has no pipeline")
	}
	p, pid := ev.Pipeline, ev.Project.ID
	f := &PipelineFacts{}

	t, reason, need := pipelineTarget(ev, defaultBranchOf(e, ev), nil, false)
	if need {
		if !glab.IsCommitSHA(p.SHA) {
			return nil, fmt.Errorf("pipeline facts: pipeline %d has no full commit SHA (%q)", p.ID, p.SHA)
		}
		mrs, err := g.GL.ListMRsForCommit(ctx, pid, p.SHA)
		if err != nil {
			return nil, fmt.Errorf("pipeline facts: merge requests for %s: %w", p.SHA, err)
		}
		f.CommitMRs, f.CommitMRsLooked = mrs, true
		t, reason, _ = pipelineTarget(ev, defaultBranchOf(e, ev), mrs, true)
	}
	if reason != "" {
		return f, nil // decidePipeline re-derives the same drop
	}

	jobs, err := g.GL.ListPipelineJobs(ctx, pid, p.ID, pipelineStatusFailed, false)
	if err != nil {
		return nil, fmt.Errorf("pipeline facts: failed jobs of %d: %w", p.ID, err)
	}
	counted := 0
	for _, j := range jobs {
		if j.Status != pipelineStatusFailed { // re-check the server-side scope
			continue
		}
		f.FailedJobs = append(f.FailedJobs, pipelinectx.FailedJob{
			Stage: j.Stage, Name: j.Name, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure,
		})
		if !j.AllowFailure {
			counted++
		}
	}
	if counted == 0 {
		full, err := g.GL.GetPipeline(ctx, pid, p.ID)
		if err != nil {
			return nil, fmt.Errorf("pipeline facts: pipeline %d: %w", p.ID, err)
		}
		f.FailureReason = full.FailureReason
	}

	last, err := g.lastSuccess(ctx, pid, t)
	if err != nil {
		return nil, fmt.Errorf("pipeline facts: last success on %s: %w", t, err)
	}
	f.LastSuccess = last.ID

	// An unreadable fix epoch is NOT "no fix": with it wrong, a failure a fix
	// revealed would be filed under the run the fix ended. Fail closed; the
	// backfill sweep retries.
	f.FixEpoch, err = g.fixEpoch(ctx, pid, p, t, last)
	if err != nil {
		return nil, fmt.Errorf("pipeline facts: fix epoch on %s: %w", t, err)
	}

	anchor, err := pipelinectx.Anchor(pid, t, pipelinectx.Fingerprint(f.FailedJobs, f.FailureReason), f.LastSuccess, f.FixEpoch)
	if err != nil {
		return f, nil // decidePipeline reports ReasonPipelineIdentityInvalid
	}
	f.RunStates, f.LandingErr = g.runStates(ctx, pid, t, anchor)
	return f, nil
}

// lastSuccessPage is how many of a ref's newest green pipelines are read to
// find the newest non-child one (one page; GitLab's maximum).
const lastSuccessPage = 100

// lastSuccess is D2's one-call rule: the newest status=success pipeline on
// the target, or the zero Pipeline (id 0). The max id is taken rather than
// trusting list order. The whole list object is returned because the fix
// epoch needs the green pipeline's commit and creation time.
//
// A CHILD pipeline (source parent_pipeline) is never a green of the target
// (verify report R3): GitLab's project pipeline list includes children, and a
// green child after a red parent would otherwise move last_success, so every
// later failure of the same run would get a new anchor and a new paid
// diagnosis. Run maintenance (gonk-gate observePipelineTarget) and the
// backfill (sweepPipelines) pass children over the same way. One page is read:
// a ref whose 100 newest greens are ALL children reads as "no green", which
// starts at most one new run -- it never hides a failure.
func (g *GitLabPipelineFacts) lastSuccess(ctx context.Context, pid int64, t pipelinectx.Target) (glab.Pipeline, error) {
	var ps []glab.Pipeline
	var err error
	if t.Kind == pipelinectx.TargetKindMR {
		ps, err = g.GL.ListMRPipelines(ctx, pid, t.MRIID)
	} else {
		ps, err = g.GL.ListPipelines(ctx, pid, t.Ref, pipelineStatusSuccess, lastSuccessPage)
	}
	if err != nil {
		return glab.Pipeline{}, err
	}
	var best glab.Pipeline
	for _, pl := range ps {
		if pl.Status == pipelineStatusSuccess && pl.Source != sourceParentPipeline && pl.ID > best.ID {
			best = pl
		}
	}
	return best, nil
}

// runStates reads the landing for this anchor's run markers (D3/D9), counting
// only BOT-AUTHORED, non-system notes. MR target: the MR's notes. Ref target:
// the notes of every bot-authored issue (ANY state: a human closing the
// tracking issue does not reset a run, D2) whose description carries the
// ref's marker.
func (g *GitLabPipelineFacts) runStates(ctx context.Context, pid int64, t pipelinectx.Target, anchor string) ([]string, error) {
	var states []string
	collect := func(notes []glab.Note) {
		for _, n := range notes {
			if n.System || n.Author.ID != g.BotUserID {
				continue
			}
			states = append(states, pipelinectx.RunMarkerStates(n.Body, anchor)...)
		}
	}
	if t.Kind == pipelinectx.TargetKindMR {
		notes, err := g.GL.ListMRNotes(ctx, pid, t.MRIID)
		if err != nil {
			return nil, fmt.Errorf("notes of MR !%d: %w", t.MRIID, err)
		}
		collect(notes)
		return states, nil
	}
	issues, err := g.GL.ListIssues(ctx, pid, glab.IssueListOptions{State: "all", AuthorID: g.BotUserID})
	if err != nil {
		return nil, fmt.Errorf("bot-authored issues: %w", err)
	}
	marker := pipelinectx.RefIssueMarker(t.Ref)
	for _, is := range issues {
		// Re-check the author: a proxy or an older GitLab ignoring author_id
		// would otherwise let a human-written issue speak for the bot.
		if is.Author.ID != g.BotUserID || !strings.Contains(is.Description, marker) {
			continue
		}
		notes, err := g.GL.ListIssueNotes(ctx, pid, is.IID)
		if err != nil {
			return nil, fmt.Errorf("notes of issue #%d: %w", is.IID, err)
		}
		collect(notes)
	}
	return states, nil
}
