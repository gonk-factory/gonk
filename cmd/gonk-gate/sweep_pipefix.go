package main

// PIPELINE-FIX DISPATCH (gonk-2usw plan 3.2, part DISPATCH): for each open,
// diagnosed pipeline run, decide whether a fix may be dispatched -- and when
// it may, record the fix and fire ONE gonk-dispatch order with trigger
// pipeline-fix. Zero tokens: GitLab reads, the meter's project read, the work
// store. Meter's /decide (Gate 2) still decides whether the order runs.
//
// WHEN. On every run-maintenance look (sweepPipelineRuns, at most once per
// pipelineRunCheckEvery per run), after the run itself was maintained and
// only while it is still open. The first look after a diagnosis lands is the
// next sweep tick.
//
// THE DECISION IS pipefix.Eligible, AND THIS FILE ONLY FILLS ITS FACTS. Each
// group of facts is read from ONE source and marked read; a group that could
// not be read stays "not read", and Eligible then answers "unknown" at that
// row -- never eligible. The groups are read LAZILY, in row order: a group is
// read only when Eligible has passed every earlier row and asks for it
// (pipefixLook.decide). So a project that has not opted in costs ONE read of
// its meter registration per look, and not one GitLab request
// (TestAnOptedOutProjectIsInert).
//
//	row       fact group   source
//	E1        Config       gonk-meter GET /v1/projects/{project}: the
//	                       EFFECTIVE config, operator layers folded in
//	E2..E5    Run          the run's record; the run note's state on the
//	                       landing (bot-authored notes only)
//	E6        Scope        scope.Fold of (pack row, operator layers from
//	                       meter, the project's `scope:` read from .gonk.yml
//	                       AT THE DEFAULT BRANCH'S HEAD COMMIT), narrowed by
//	                       plan 4.2's row for the diagnosis class; each cited
//	                       file classified with base facts read at the commit
//	                       of the run's newest failing pipeline
//	E7        Head         the target's head commit; that pipeline's commit
//	E8        Current      the job failure keys of that pipeline, recomputed
//	                       from its jobs and logs exactly as dispatch records
//	                       them (currentFailureKeys)
//	E9        Dedup        the fix record (work store); the bot's merge
//	                       requests on fix branches and their cause markers
//	E10, E11  Serial       the same merge requests, on this target's branch
//
// DEDUP IS A COMPARE-AND-SWAP ON THE FIX RECORD. Dispatching means: write the
// fix record under the fix anchor with PutIf (create-only, or over the one
// stale revision read), and fire the order only if that write WON. Two sweeps,
// or two looks, therefore fire one order: the loser's write is stale and it
// fires nothing; a later look finds the record and stops at E9. The record
// carries the same fire_by deadline a re-sling does, so an order Gas City
// dropped is re-fired when its window has ended, never beside a live one.
//
// EVERYTHING IS OFF unless sweepDeps.FixesLive is set, which main.go does only
// when a fix session can actually be prepared (pipefixPrepare, part AGENT).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/intake"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// pipefixGeneratedProbeBytes caps the read of one cited file that decides
// whether it is generated (scope.IsGenerated looks at line 1). It is the
// largest file a fix may edit (plan 4.3): a file over the cap fails the read
// and is then "not classified" -- which is right, since it could not be
// edited in any case.
const pipefixGeneratedProbeBytes = 256 << 10

// pipefixCIFile is the pipeline definition whose local includes are CI files
// too (scope.BaseFacts.CILocalIncludes).
const pipefixCIFile = ".gitlab-ci.yml"

// labelNeedsHuman is put on the diagnosis landing when a fix needs a person
// (plan 8.1: needs-scope at dispatch, the chain cap). Controller-written, so
// reserved (broker_label.go).
const labelNeedsHuman = "gonk::needs-human"

// labelFixClassConfirmed is the label a HUMAN puts on the diagnosis landing to
// confirm a `configuration` diagnosis before gonk writes a CI-definition fix
// (owner decision Q2, plan 14.1). gonk never writes it, and no agent can
// propose it: it is reserved (broker_label.go).
const labelFixClassConfirmed = "gonk::fix-class-confirmed"

// Controller-side reasons that are not rows of plan 3.2. They are logged in
// the same field as a pipefix.ReasonCode so one grep finds every look.
const (
	// reasonOutsidePaths: every cited file is inside the class envelope (E6
	// passed) but none is inside pipeline_fix.paths. needs-scope, like E6.
	reasonOutsidePaths pipefix.ReasonCode = "E6.outside-paths"
	// reasonClassUnconfirmed: a `configuration` fix waits for a human to
	// confirm the class (labelFixClassConfirmed).
	reasonClassUnconfirmed pipefix.ReasonCode = "Q2.class-not-confirmed"
)

// pipefixForge is what the dispatch pass reads from GitLab beyond the pipeline
// path's own surface. Satisfied by *glab.Client.
type pipefixForge interface {
	pipelineForge
	GetProject(ctx context.Context, projectID int64) (*glab.Project, error)
	ResolveRef(ctx context.Context, projectID int64, ref string) (string, error)
	ListMergeRequests(ctx context.Context, projectID int64, o glab.MRListOptions) ([]glab.MergeRequest, error)
}

var _ pipefixForge = (*glab.Client)(nil)

// pipefixConfigSource answers a project's effective configuration: gonk-meter
// in production (meterAPI.Project), a fake in tests.
type pipefixConfigSource func(ctx context.Context, project string) (*meterapi.ProjectResponse, error)

// pipefixSweep is the per-tick state of the dispatch pass: the forge, and the
// config answers already fetched this tick (one read per project per tick).
type pipefixSweep struct {
	forge  pipefixForge
	config pipefixConfigSource
	cfgs   map[string]pipefixConfigAnswer
}

type pipefixConfigAnswer struct {
	resp *meterapi.ProjectResponse
	err  error
}

// newPipefixSweep returns nil when the pass is off: FixesLive is not set, or
// the forge cannot serve the pass's reads, or there is no config source.
func newPipefixSweep(d sweepDeps) *pipefixSweep {
	if !d.FixesLive || d.Pipelines == nil {
		return nil
	}
	f, ok := d.Pipelines.(pipefixForge)
	if !ok {
		d.Log.Warn("sweep: pipeline-fix dispatch is on but the pipeline forge cannot serve its reads; the pass is off")
		return nil
	}
	src := d.FixConfig
	if src == nil {
		if d.Meter == nil {
			return nil
		}
		src = d.Meter.Project
	}
	return &pipefixSweep{forge: f, config: src, cfgs: map[string]pipefixConfigAnswer{}}
}

func (s *pipefixSweep) projectConfig(ctx context.Context, project string) (*meterapi.ProjectResponse, error) {
	if a, ok := s.cfgs[project]; ok {
		return a.resp, a.err
	}
	resp, err := s.config(ctx, project)
	s.cfgs[project] = pipefixConfigAnswer{resp: resp, err: err}
	return resp, err
}

// pipefixLook is one look at one diagnosed run: the facts read so far and
// what was learned on the way that the dispatch itself needs.
type pipefixLook struct {
	d   sweepDeps
	s   *pipefixSweep
	l   *pipelineLander
	run beadstore.Record
	now time.Time

	target    pipelinectx.Target
	fixAnchor string
	facts     pipefix.EligibleFacts

	cfg      *meterapi.ProjectResponse
	fixCfg   gonkcfg.EffectivePipelineFix
	landing  runLanding
	landed   bool
	mr       *glab.MergeRequest // the MR target, once read
	latest   *glab.Pipeline     // the run's newest failing pipeline
	branch   string             // the branch a fix MR targets
	eff      scope.Effective    // the fold, narrowed to pipeline_fix.paths
	env      pipefixEnvelope
	cfgSHA   string
	fixMRs   []glab.MergeRequest // the bot's merge requests on fix branches, any state
	listed   bool
	baseJobs []beadstore.PipelineFixBaseJob // every job of that pipeline
	fixRec   beadstore.Record               // the stored fix record, when found
	hasFix   bool
	staleFix bool // found, fired, never picked up, and its fire window has ended
}

// considerPipelineFix is the look. Best effort and never fatal: a read that
// fails leaves its facts unread, the answer is "unknown", and the next look
// repeats it.
func considerPipelineFix(ctx context.Context, d sweepDeps, s *pipefixSweep, l *pipelineLander, run beadstore.Record, now time.Time) {
	if s == nil || run.Trigger != atags.TriggerPipelineDiagnosis || !run.PipelineRunOpen ||
		run.State != beadstore.StateDone || !run.PipelineDiagnosis.Recorded() {
		return
	}
	t, err := pipelineRunOf(run)
	if err != nil {
		return // maintenance already said so
	}
	fixAnchor, err := pipelinectx.FixAnchor(run.BeadAnchor)
	if err != nil {
		d.Log.Warn("sweep: pipeline fix: the run's anchor has no fix anchor", "bead", run.BeadAnchor, "err", err)
		return
	}
	lk := &pipefixLook{d: d, s: s, l: l, run: run, now: now, target: t, fixAnchor: fixAnchor}
	// E12 is the one fact with no "read" marker: nothing sets it until
	// gonk-lreg exists, and its zero value deliberately holds nothing.
	lk.facts.CorrelationHold = d.FixHold != nil && d.FixHold(ctx, run)
	ok, reason := lk.decide(ctx)

	// THE ONE LINE PER LOOK. `reason` is the code of the first row that
	// refused ("" when eligible); `unknown` says its facts could not be read.
	d.Log.Info("sweep: pipeline fix eligibility", "bead", run.BeadAnchor, "fix", fixAnchor,
		"eligible", ok, "row", reason.Row(), "reason", string(reason.Code), "unknown", reason.Unknown,
		"detail", reason.Text)

	switch {
	case ok:
		lk.dispatch(ctx)
	case reason.Unknown || !reason.Note:
		// Nothing is decided, or the row is silent: no write of any kind.
	default:
		lk.refuse(ctx, reason)
	}
}

// decide fills the facts as far as Eligible asks for them and returns its
// answer, then applies the two controller-side holds that are not rows of the
// plan's table.
func (lk *pipefixLook) decide(ctx context.Context) (bool, pipefix.Reason) {
	readers := map[pipefix.ReasonCode]func(context.Context){
		pipefix.E1Unread:  lk.readConfig,
		pipefix.E2Unread:  lk.readRun,
		pipefix.E6Unread:  lk.readScope,
		pipefix.E7Unread:  lk.readHead,
		pipefix.E8Unread:  lk.readCurrent,
		pipefix.E9Unread:  lk.readDedup,
		pipefix.E10Unread: lk.readSerial,
	}
	tried := map[pipefix.ReasonCode]bool{}
	for {
		ok, reason := pipefix.Eligible(lk.facts)
		if ok {
			return lk.hold()
		}
		read := readers[reason.Code]
		if !reason.Unknown || read == nil || tried[reason.Code] {
			return false, reason
		}
		tried[reason.Code] = true
		read(ctx)
	}
}

// hold applies, to a run every row of the table passed, the two checks the
// table does not have.
func (lk *pipefixLook) hold() (bool, pipefix.Reason) {
	if len(lk.editableFiles()) == 0 {
		return false, pipefix.Reason{Code: reasonOutsidePaths, Note: true, NeedsHuman: true, NeedsScope: true,
			Text: "needs scope: none of the files the diagnosis cites is inside this project's `pipeline_fix.paths`"}
	}
	if lk.facts.Run.Class == pipefix.ClassConfiguration && !lk.classConfirmed() {
		return false, pipefix.Reason{Code: reasonClassUnconfirmed,
			Text: "a configuration fix waits for a person to confirm the diagnosis's class with the label " + labelFixClassConfirmed}
	}
	return true, pipefix.Reason{}
}

// editableFiles are the cited files inside pipeline_fix.paths (all of them
// when the project set none): the only paths an edit may touch.
func (lk *pipefixLook) editableFiles() []string {
	var out []string
	for _, f := range lk.run.PipelineDiagnosis.Files {
		if lk.eff.InBead(f) {
			out = append(out, f)
		}
	}
	return out
}

// classConfirmed reports whether a human confirmed the diagnosis's class on
// the landing. Read from the landing as this look already read it.
func (lk *pipefixLook) classConfirmed() bool {
	var labels []string
	switch {
	case lk.target.Kind == pipelinectx.TargetKindMR && lk.mr != nil:
		labels = lk.mr.Labels
	case lk.landing.issue != nil:
		labels = lk.landing.issue.Labels
	}
	for _, l := range labels {
		if l == labelFixClassConfirmed {
			return true
		}
	}
	return false
}

func (lk *pipefixLook) warn(what string, err error) {
	lk.d.Log.Warn("sweep: pipeline fix: "+what+"; the facts stay unread and nothing is dispatched",
		"bead", lk.run.BeadAnchor, "err", err)
}

// ---- E1: the effective configuration ---------------------------------------

func (lk *pipefixLook) readConfig(ctx context.Context) {
	resp, err := lk.s.projectConfig(ctx, lk.run.Project)
	if err != nil || resp == nil {
		lk.warn("could not read the project's effective configuration", err)
		return
	}
	lk.cfg = resp
	if resp.Effective == nil {
		// No effective config (the project's .gonk.yml does not load): it has
		// opted in to nothing. Loaded, and everything off.
		lk.facts.Config = pipefix.ConfigFacts{Loaded: true}
		return
	}
	e := resp.Effective
	lk.fixCfg = e.PipelineFix.EffectivePipelineFix()
	lk.facts.Config = pipefix.ConfigFacts{
		Loaded:        true,
		PipelineFixes: e.Actions.PipelineFixes,
		Pipelines:     e.Actions.Pipelines,
		Targets:       lk.fixCfg.Targets,
		Classes:       lk.fixCfg.Classes,
		MaxChain:      lk.fixCfg.MaxChain,
	}
}

// ---- E2..E5: the diagnosed run ---------------------------------------------

// recordedJobs is the record's failed jobs as pipefix.TargetSet reads them.
func recordedJobs(rec beadstore.Record) []pipefix.RecordedJob {
	out := make([]pipefix.RecordedJob, 0, len(rec.PipelineJobs))
	for _, j := range rec.PipelineJobs {
		out = append(out, pipefix.RecordedJob{
			Name: j.Name, Stage: j.Stage, FailureReason: j.FailureReason, ErrorSignature: j.ErrorSignature,
			AllowFailure: j.AllowFailure, Generic: j.GenericFailure(), FactsRecorded: j.FailureFactsRecorded(),
		})
	}
	return out
}

func (lk *pipefixLook) readRun(ctx context.Context) {
	run := pipefix.RunFacts{
		Read: true, TargetKind: lk.target.Kind, Open: lk.run.PipelineRunOpen, Class: lk.run.PipelineDiagnosis.Class,
	}
	run.Targets, run.TargetProblems = pipefix.TargetSet(recordedJobs(lk.run), lk.run.PipelineDiagnosis.EvidenceJobs)
	// The run note is read only when the target kind is one this project
	// fixes: otherwise E2 answers before the note's state is looked at, and
	// the landing is not worth a request.
	if lk.fixCfg.AllowsTarget(lk.target.Kind) {
		ld, err := lk.l.findRunLanding(ctx, lk.run, lk.target)
		if err != nil {
			lk.warn("could not read the run's landing", err)
			return
		}
		lk.landing, lk.landed = ld, true
		run.NoteState = ld.state
	}
	lk.facts.Run = run
}

// ---- E6: the scope envelope ------------------------------------------------

// latestFailing is the run's newest failing pipeline, read once per look.
func (lk *pipefixLook) latestFailing(ctx context.Context) (*glab.Pipeline, error) {
	if lk.latest != nil {
		return lk.latest, nil
	}
	id := max(lk.run.PipelineLatestFailed, lk.run.PipelineID)
	p, err := lk.s.forge.GetPipeline(ctx, lk.run.ProjectID, id)
	if err != nil {
		return nil, fmt.Errorf("read pipeline %d: %w", id, err)
	}
	if p == nil || p.ID != id {
		return nil, fmt.Errorf("read pipeline %d: GitLab answered with another pipeline", id)
	}
	if !glab.IsCommitSHA(p.SHA) {
		return nil, fmt.Errorf("pipeline %d carries no full commit SHA (%q)", id, p.SHA)
	}
	if p.Status != "failed" {
		return nil, fmt.Errorf("pipeline %d is %q, not failed: the run's newest failing pipeline is not what the record says", id, p.Status)
	}
	lk.latest = p
	return p, nil
}

func (lk *pipefixLook) readScope(ctx context.Context) {
	f, pid := lk.s.forge, lk.run.ProjectID
	if lk.cfg == nil || lk.cfg.OperatorScope == nil {
		lk.warn("the meter's answer carries no operator scope layers", errors.New("operator_scope is absent"))
		return
	}
	row, err := scope.LoadRow(lk.d.PackDir, pipefixAgent)
	if err != nil {
		lk.warn("the pack's scope row for the fix agent does not load", err)
		return
	}
	if len(row.Classes)+len(row.Liftable) == 0 {
		lk.warn("the pack has no scope row for the fix agent", fmt.Errorf("agents/%s/effect-shape.toml has no [scope] table", pipefixAgent))
		return
	}
	latest, err := lk.latestFailing(ctx)
	if err != nil {
		lk.warn("could not read the run's newest failing pipeline", err)
		return
	}

	// The project's `scope:` section is read at a commit THIS look pins: the
	// default branch's head. Never the target branch of an MR, and never the
	// fix branch (066 invariant 3).
	proj, err := f.GetProject(ctx, pid)
	if err != nil || proj == nil || proj.DefaultBranch == "" {
		lk.warn("could not read the project's default branch", err)
		return
	}
	cfgSHA, err := f.ResolveRef(ctx, pid, proj.DefaultBranch)
	if err != nil {
		lk.warn("could not pin the default branch to a commit", err)
		return
	}
	raw, err := f.GetRawFile(ctx, pid, intake.ConfigPath, cfgSHA, intake.MaxConfigBytes)
	if err != nil {
		lk.warn("could not read .gonk.yml at the pinned commit", err)
		return
	}
	pc, err := gonkcfg.Load(raw)
	if err != nil {
		lk.warn(".gonk.yml at the pinned commit does not load", err)
		return
	}
	eff, err := scope.Fold(scope.Input{
		Row:      row,
		Instance: lk.cfg.OperatorScope.Instance.Policy(),
		Group:    lk.cfg.OperatorScope.Group.Policy(),
		Project:  lk.fixCfg.TightenScope(pc.Scope),
		BaseSHA:  cfgSHA,
		Enabled:  lk.facts.Config.PipelineFixes && lk.facts.Config.Pipelines,
		// The project's opt-in to the ci lift, and nothing else's: the batch,
		// the diagnosis and the order have no say (066 section 9 a/b).
		LiftOptIn: lk.fixCfg.LiftOptIn(),
	})
	if err != nil {
		lk.warn("the scope does not fold", err)
		return
	}
	if eff, err = eff.WithBead(lk.fixCfg.Paths); err != nil {
		lk.warn("pipeline_fix.paths does not validate", err)
		return
	}
	env, err := envelopeFor(eff, lk.run.PipelineDiagnosis.Class)
	if err != nil {
		lk.warn("no envelope for the diagnosis class", err)
		return
	}

	// Base facts, at the commit the fix would be written against.
	base := scope.BaseFacts{Generated: map[string]bool{}}
	switch ci, cerr := f.GetRawFile(ctx, pid, pipefixCIFile, latest.SHA, intake.MaxConfigBytes*16); {
	case cerr == nil:
		if base.CILocalIncludes, err = scope.CILocalIncludes(ci); err != nil {
			lk.warn("the pipeline definition's includes cannot be read", err)
			return
		}
	case !glab.IsNotFound(cerr):
		lk.warn("could not read the pipeline definition at the pipeline's commit", cerr)
		return
	}
	cited := make([]pipefix.CitedFile, 0, len(lk.run.PipelineDiagnosis.Files))
	cls := eff.Classifier(base, lk.d.Log)
	for _, path := range lk.run.PipelineDiagnosis.Files {
		c := pipefix.CitedFile{Path: path}
		content, rerr := f.GetRawFile(ctx, pid, path, latest.SHA, pipefixGeneratedProbeBytes)
		if rerr != nil {
			// Not classified: Eligible answers "unknown" for the row. A file
			// that is gone, or too large to edit, cannot be fixed in any case.
			lk.d.Log.Warn("sweep: pipeline fix: a cited file could not be read at the pipeline's commit; it is not classified",
				"bead", lk.run.BeadAnchor, "sha", latest.SHA, "err", rerr)
		} else {
			if scope.IsGenerated(content) {
				base.Generated[path] = true
				cls = eff.Classifier(base, lk.d.Log)
			}
			c.Class = string(cls.Classify(path).Class)
		}
		cited = append(cited, c)
	}

	lk.eff, lk.env, lk.cfgSHA = eff, env, cfgSHA
	lk.facts.Scope = pipefix.ScopeFacts{Folded: true, Envelope: env.classes, Cited: cited}
}

// ---- E7: the head ------------------------------------------------------------

func (lk *pipefixLook) readHead(ctx context.Context) {
	f, pid := lk.s.forge, lk.run.ProjectID
	latest, err := lk.latestFailing(ctx)
	if err != nil {
		lk.warn("could not read the run's newest failing pipeline", err)
		return
	}
	var head string
	if lk.target.Kind == pipelinectx.TargetKindMR {
		mr, err := f.GetMergeRequest(ctx, pid, lk.target.MRIID)
		if err != nil || mr == nil {
			lk.warn("could not read the target merge request", err)
			return
		}
		if mr.IID != lk.target.MRIID {
			lk.warn("could not read the target merge request", fmt.Errorf("GitLab answered with !%d", mr.IID))
			return
		}
		if mr.SourceProjectID != pid || mr.SourceBranch == "" {
			// A fork's branch is not one gonk can open a fix MR against.
			lk.warn("the target merge request's source branch is not in this project",
				fmt.Errorf("source project %d", mr.SourceProjectID))
			return
		}
		lk.mr, lk.branch, head = mr, mr.SourceBranch, mr.SHA
	} else {
		sha, err := f.ResolveRef(ctx, pid, lk.target.Ref)
		if err != nil {
			lk.warn("could not read the target branch's head", err)
			return
		}
		lk.branch, head = lk.target.Ref, sha
	}
	lk.facts.Head = pipefix.HeadFacts{Read: true, TargetSHA: head, NewestFailingSHA: latest.SHA}
}

// ---- E8: the cause, now ------------------------------------------------------

// currentFailureKeys recomputes, from GitLab's job objects and logs, the job
// failure key of every job that failed pipelineID -- the SAME computation
// dispatch records (buildPipelineContext + JobRecords): the same failed-job
// split and order, an excerpt for the first MaxExcerptJobs only, the first
// error line found before any cap is applied, the canonical job name. The two
// are held together by TestCurrentFailureKeysAreWhatDispatchRecords.
//
// An error means the keys are NOT KNOWN: the job list could not be read, or
// the log of a job that would have an excerpt could not be.
func currentFailureKeys(ctx context.Context, r pipelineReader, projectID, pipelineID int64) ([]pipelinectx.JobFailureKey, error) {
	keys, _, err := currentPipelineFacts(ctx, r, projectID, pipelineID)
	return keys, err
}

// currentPipelineFacts is currentFailureKeys plus every job of the pipeline
// (latest attempts, canonical names), from the one job listing both need.
func currentPipelineFacts(ctx context.Context, r pipelineReader, projectID, pipelineID int64) ([]pipelinectx.JobFailureKey, []beadstore.PipelineFixBaseJob, error) {
	keys, jobs, err := failureKeysOf(ctx, r, projectID, pipelineID)
	if err != nil {
		return nil, nil, err
	}
	all := make([]beadstore.PipelineFixBaseJob, 0, len(jobs))
	for _, j := range jobs {
		all = append(all, beadstore.PipelineFixBaseJob{Name: jobNameText(j.Name), Status: j.Status, AllowFailure: j.AllowFailure})
	}
	return keys, all, nil
}

// failureKeysOf is the computation; see currentFailureKeys.
func failureKeysOf(ctx context.Context, r pipelineReader, projectID, pipelineID int64) ([]pipelinectx.JobFailureKey, []glab.Job, error) {
	jobs, err := r.ListPipelineJobs(ctx, projectID, pipelineID, "", false)
	if err != nil {
		return nil, nil, fmt.Errorf("list jobs of pipeline %d: %w", pipelineID, err)
	}
	failed, _ := splitPipelineJobs(jobs)
	keys := make([]pipelinectx.JobFailureKey, 0, len(failed))
	for i, j := range failed {
		k := pipelinectx.JobFailureKey{Stage: j.Stage, Name: jobNameText(j.Name), FailureReason: j.FailureReason}
		if i < pipelinectx.MaxExcerptJobs {
			tt, err := fetchPipelineTrace(ctx, r, projectID, j.ID)
			if err != nil {
				return nil, nil, fmt.Errorf("read the log of job %d: %w", j.ID, err)
			}
			ex := pipelinectx.ExcerptWith(tt.Data, pipelineExcerptOptions(tt, pipelinectx.JobBudget(1)))
			k.ErrorSignature = ex.ErrorSignature
		}
		keys = append(keys, k)
	}
	return keys, jobs, nil
}

func (lk *pipefixLook) readCurrent(ctx context.Context) {
	latest, err := lk.latestFailing(ctx)
	if err != nil {
		lk.warn("could not read the run's newest failing pipeline", err)
		return
	}
	keys, jobs, err := currentPipelineFacts(ctx, lk.s.forge, lk.run.ProjectID, latest.ID)
	if err != nil {
		lk.warn("could not recompute the newest failing pipeline's job failure keys", err)
		return
	}
	lk.baseJobs = jobs
	lk.facts.Current = pipefix.CurrentFacts{Read: true, FailedKeys: keys}
}

// ---- E9: dedup -----------------------------------------------------------------

// fixStateOf maps a stored fix record to the fix state of plan 7.1.
//
//	PipelineFix.State set            that state (needs-scope, and everything
//	                                 parts APPLY and VERIFY write)
//	running, pending-prompt, parked  proposing: the fix is being dispatched
//	needs-human                      denied: meter stopped it
//	State "" inside its fire window  proposing: the order is on its way
//	State "" past its fire window    none (stale=true): no dispatch this
//	                                 record's order fired can still be alive,
//	                                 so the look may dispatch again over it
//	anything else                    unknown (known=false): fail closed
func fixStateOf(rec beadstore.Record, now time.Time) (st pipefix.State, stale, known bool) {
	if s := pipefix.State(rec.PipelineFix.State); s != "" {
		return s, false, pipefix.KnownState(s)
	}
	switch rec.State {
	case beadstore.StateRunning, beadstore.StatePendingPrompt, beadstore.StateParked:
		return pipefix.StateProposing, false, true
	case beadstore.StateNeedsHuman:
		return pipefix.StateDenied, false, true
	case "":
		if end, ok := reslingWindowEnd(rec); ok && now.Before(end) {
			return pipefix.StateProposing, false, true
		}
		return "", true, true
	}
	return "", false, false
}

// pipefixUnknownState is what E9 is given for a fix record whose state cannot
// be told: it is outside pipefix.States, so Eligible answers E9.unread.
const pipefixUnknownState pipefix.State = "unknown"

// botFixMRs lists, once per look, the bot's merge requests on fix branches in
// this project, in any state. Each returned MR is re-checked: the bot is its
// author, its source branch is a fix branch, and both ends are this project.
func (lk *pipefixLook) botFixMRs(ctx context.Context) ([]glab.MergeRequest, error) {
	if lk.listed {
		return lk.fixMRs, nil
	}
	all, err := lk.s.forge.ListMergeRequests(ctx, lk.run.ProjectID, glab.MRListOptions{State: "all", AuthorID: lk.l.me.ID})
	if err != nil {
		return nil, err
	}
	pid := lk.run.ProjectID
	var out []glab.MergeRequest
	for _, mr := range all {
		if mr.Author.ID != lk.l.me.ID || !pipelinectx.IsFixBranch(mr.SourceBranch) ||
			mr.SourceProjectID != pid || mr.TargetProjectID != pid {
			continue
		}
		out = append(out, mr)
	}
	lk.fixMRs, lk.listed = out, true
	return out, nil
}

func (lk *pipefixLook) readDedup(ctx context.Context) {
	dd := pipefix.DedupFacts{}
	rec, found, err := lk.d.Store.Get(ctx, lk.fixAnchor)
	if err != nil {
		lk.warn("could not read the fix record", err)
		return
	}
	if found {
		st, stale, known := fixStateOf(rec, lk.now)
		lk.fixRec, lk.hasFix, lk.staleFix = rec, true, stale
		switch {
		case !known:
			dd.FixState = pipefixUnknownState
			lk.d.Log.Warn("sweep: pipeline fix: the fix record is in a state this build cannot read; nothing is dispatched",
				"fix", lk.fixAnchor, "state", string(rec.State), "fix_state", rec.PipelineFix.State)
		default:
			dd.FixState = st
		}
	}
	mrs, err := lk.botFixMRs(ctx)
	if err != nil {
		lk.warn("could not list the bot's fix merge requests", err)
		return
	}
	branch, err := pipelinectx.FixBranch(lk.fixAnchor)
	if err != nil {
		lk.warn("the fix anchor has no branch", err)
		return
	}
	cause := pipefix.CauseKey(lk.facts.Run.Targets)
	for _, mr := range mrs {
		if mr.State != "opened" && mr.State != "closed" {
			continue // merged (the fix epoch's business) or locked mid-merge
		}
		mine := mr.SourceBranch == branch
		if !mine {
			_, mrCause, ok := pipelinectx.ParseFixMRMarker(mr.Description)
			if !ok {
				// Its cause cannot be told, so "no merge request for this cause
				// exists" cannot be said either.
				lk.warn("a fix merge request carries no readable cause marker",
					fmt.Errorf("merge request !%d from %s", mr.IID, mr.SourceBranch))
				return
			}
			mine = mrCause == cause
		}
		if !mine {
			continue
		}
		if mr.State == "opened" {
			dd.OpenCauseMRs++
		} else {
			dd.ClosedUnmergedCauseMRs++
		}
	}
	dd.Read = true
	lk.facts.Dedup = dd
}

// ---- E10, E11: serial and chain ------------------------------------------------

func (lk *pipefixLook) readSerial(ctx context.Context) {
	if lk.branch == "" {
		lk.warn("the branch a fix would target is not known", errors.New("the head was not read"))
		return
	}
	mrs, err := lk.botFixMRs(ctx)
	if err != nil {
		lk.warn("could not list the bot's fix merge requests", err)
		return
	}
	var sf pipefix.SerialFacts
	var merged []glab.MergeRequest
	for _, mr := range mrs {
		if mr.TargetBranch != lk.branch {
			continue
		}
		switch mr.State {
		case "opened", "locked":
			sf.OpenFixMRs++
		case "merged":
			if mr.MergedAt == nil || mr.MergedAt.IsZero() {
				lk.warn("a merged fix merge request carries no merged_at", fmt.Errorf("merge request !%d", mr.IID))
				return
			}
			merged = append(merged, mr)
		}
	}
	// "Since its last green pipeline": the run's own last success is the
	// target's newest green one (a newer green would have resolved the run).
	// It is read only when there is a merged fix to place against it.
	var since time.Time
	if len(merged) > 0 && lk.run.PipelineLastSuccess != 0 {
		green, err := lk.s.forge.GetPipeline(ctx, lk.run.ProjectID, lk.run.PipelineLastSuccess)
		if err != nil || green == nil || green.ID != lk.run.PipelineLastSuccess || green.CreatedAt.IsZero() {
			lk.warn("could not read when the target's last green pipeline was created", err)
			return
		}
		since = green.CreatedAt
	}
	for _, mr := range merged {
		if mr.MergedAt.After(since) {
			sf.MergedSinceGreen++
		}
	}
	sf.Read = true
	lk.facts.Serial = sf
}

// ---- the dispatch ------------------------------------------------------------

// fixRecord is the record of a fix this look decided to dispatch: the run's
// identity, and everything the controller decided about the fix. State is ""
// until gonk-dispatch picks the order up.
func (lk *pipefixLook) fixRecord() (beadstore.Record, error) {
	key, err := pipelinectx.FixSessionKey(lk.fixAnchor)
	if err != nil {
		return beadstore.Record{}, err
	}
	run, latest := lk.run, lk.latest
	if latest == nil || lk.cfg == nil {
		return beadstore.Record{}, errors.New("the look holds no pipeline or no configuration")
	}
	targets := make([]beadstore.PipelineFixTarget, 0, len(lk.facts.Run.Targets))
	inT := map[string]bool{}
	for _, t := range lk.facts.Run.Targets {
		inT[t.Key.Name] = true
		targets = append(targets, beadstore.PipelineFixTarget{Stage: t.Key.Stage, Name: t.Key.Name,
			FailureReason: t.Key.FailureReason, ErrorSignature: t.Key.ErrorSignature, Generic: t.Generic})
	}
	var also []string
	for _, j := range run.PipelineJobs {
		if !inT[j.Name] {
			also = append(also, j.Name)
		}
	}
	// The version the target signatures were recorded under (the same for
	// every job of one dispatch).
	sigVersion := 0
	for _, j := range run.PipelineJobs {
		if inT[j.Name] {
			sigVersion = j.SignatureVersion
		}
	}
	fireAt := lk.now
	return beadstore.Record{
		BeadAnchor: lk.fixAnchor, BeadID: lk.fixAnchor, Project: run.Project, ProjectID: run.ProjectID, Rig: run.Rig,
		SessionKey: key, Trigger: atags.TriggerPipelineFix, ConfigHash: lk.cfg.ConfigHash,
		// The run identity the fix anchor was built from, and the pipeline the
		// fix is written against.
		PipelineID: latest.ID, PipelineTarget: run.PipelineTarget, PipelineFingerprint: run.PipelineFingerprint,
		PipelineLastSuccess: run.PipelineLastSuccess, PipelineFixEpoch: run.PipelineFixEpoch,
		PipelineSHA: latest.SHA, PipelineProjectURL: run.PipelineProjectURL,
		PipelineFix: beadstore.PipelineFix{
			RunAnchor: run.BeadAnchor, CauseKey: pipefix.CauseKey(lk.facts.Run.Targets), Class: run.PipelineDiagnosis.Class,
			BaseSHA: latest.SHA, BasePipelineID: latest.ID, TargetBranch: lk.branch, ConfigSHA: lk.cfgSHA,
			Envelope: lk.env.classes, Lift: lk.env.liftNames(),
			Files: lk.editableFiles(), Paths: lk.fixCfg.Paths,
			MaxFiles: lk.eff.Caps.MaxFiles, MaxLines: lk.eff.Caps.MaxLines,
			MaxFileBytes: lk.eff.Caps.MaxFileBytes, MaxPatchBytes: lk.eff.Caps.MaxPatchBytes,
			CIExecChanges: lk.fixCfg.CIExecChanges, MaxAttempts: lk.fixCfg.MaxAttempts,
			VerifyTimeoutSeconds: int64(lk.fixCfg.VerifyTimeout / time.Second),
			Targets:              targets, AlsoFailing: also, FiredAt: fireAt,
			BaseJobs:         lk.baseJobs,
			ExcerptVersion:   run.PipelineExcerptVersion,
			SignatureVersion: sigVersion,
		},
		// The order's start deadline, exactly as a re-sling's (reslingWindowEnd):
		// a dispatch that starts later does nothing, so once the window has
		// ended no dispatch this record's order fired can be alive.
		ReslungAt: fireAt, ReslingFireBy: reslingFireBy(fireAt),
	}, nil
}

// dispatch records the fix and fires the order -- in that order, and the
// order only if the record write won.
func (lk *pipefixLook) dispatch(ctx context.Context) {
	rec, err := lk.fixRecord()
	if err != nil {
		lk.d.Log.Error("sweep: pipeline fix: cannot build the fix record; dispatching nothing", "fix", lk.fixAnchor, "err", err)
		return
	}
	if lk.d.Orders == nil {
		lk.d.Log.Error("sweep: pipeline fix: no order runner is configured; dispatching nothing", "fix", lk.fixAnchor)
		return
	}
	// Create-only -- or over exactly the stale record this look read. Any
	// other writer's record makes this stale, and then THAT writer dispatches.
	ifRevision := ""
	if lk.hasFix && lk.staleFix {
		ifRevision = lk.fixRec.Revision
	}
	if _, err := lk.d.Store.PutIf(ctx, rec, ifRevision); errors.Is(err, beadstore.ErrStale) {
		lk.d.Log.Info("sweep: pipeline fix: another look recorded this fix first; firing no order", "fix", lk.fixAnchor)
		return
	} else if err != nil {
		lk.d.Log.Warn("sweep: pipeline fix: the fix record could not be written; firing no order", "fix", lk.fixAnchor, "err", err)
		return
	}
	fired := refire(ctx, lk.d, rec)
	lk.d.Log.Info("sweep: pipeline fix dispatched", "bead", lk.run.BeadAnchor, "fix", lk.fixAnchor,
		"order_accepted", fired, "class", rec.PipelineFix.Class, "cause", rec.PipelineFix.CauseKey,
		"base_sha", rec.PipelineFix.BaseSHA, "base_pipeline", rec.PipelineFix.BasePipelineID,
		"envelope", strings.Join(rec.PipelineFix.Envelope, ","), "lift", lk.env.lift.String(),
		"files", len(rec.PipelineFix.Files), "config_sha", rec.PipelineFix.ConfigSHA,
		"fire_by", rec.ReslingFireBy)
}

// ---- a refusal a person should see (the rows marked N) -------------------------

// refuse writes why no fix is attempted on the diagnosis landing -- once: the
// note is edited only when its text changes -- and, for E6, records the fix
// as needs-scope so the decision is a stored fact (plan 4.4: decided at
// dispatch, zero tokens).
func (lk *pipefixLook) refuse(ctx context.Context, reason pipefix.Reason) {
	state := pipelinectx.FixNoteNotEligible
	if reason.NeedsScope {
		state = string(pipefix.StateNeedsScope)
	}
	changed, ld, err := lk.l.writeFixNote(ctx, lk.run, lk.fixAnchor, fixNote{State: state, Text: lk.refusalText(reason)})
	if err != nil {
		lk.d.Log.Warn("sweep: pipeline fix: could not write why no fix is attempted", "bead", lk.run.BeadAnchor,
			"reason", string(reason.Code), "err", err)
		return
	}
	if changed && reason.NeedsHuman {
		lk.l.addLandingLabel(ctx, lk.run, ld, labelNeedsHuman)
	}
	if !reason.NeedsScope {
		return
	}
	if _, found, err := lk.d.Store.Get(ctx, lk.fixAnchor); err != nil || found {
		return // already recorded (or unreadable: the next look repeats this)
	}
	key, err := pipelinectx.FixSessionKey(lk.fixAnchor)
	if err != nil {
		return
	}
	run := lk.run
	rec := beadstore.Record{
		BeadAnchor: lk.fixAnchor, BeadID: lk.fixAnchor, Project: run.Project, ProjectID: run.ProjectID, Rig: run.Rig,
		SessionKey: key, Trigger: atags.TriggerPipelineFix, State: beadstore.StateDone,
		PipelineID: max(run.PipelineLatestFailed, run.PipelineID), PipelineTarget: run.PipelineTarget,
		PipelineFingerprint: run.PipelineFingerprint, PipelineLastSuccess: run.PipelineLastSuccess,
		PipelineFixEpoch: run.PipelineFixEpoch, PipelineProjectURL: run.PipelineProjectURL,
		PipelineFix: beadstore.PipelineFix{
			RunAnchor: run.BeadAnchor, State: string(pipefix.StateNeedsScope), Class: run.PipelineDiagnosis.Class,
			CauseKey: pipefix.CauseKey(lk.facts.Run.Targets), Envelope: lk.env.classes,
		},
	}
	if _, err := lk.d.Store.PutIf(ctx, rec, ""); err != nil && !errors.Is(err, beadstore.ErrStale) {
		lk.d.Log.Warn("sweep: pipeline fix: the needs-scope record could not be written; the next look repeats it",
			"fix", lk.fixAnchor, "err", err)
	}
}

// refusalText is the note's sentence for a refusal. It is built HERE from the
// facts, not copied from the reason's log text: every piece of project-chosen
// text (a path, a class) goes through codeSpan, as the diagnosis note does.
func (lk *pipefixLook) refusalText(reason pipefix.Reason) string {
	class := codeSpan(lk.facts.Run.Class)
	about := pipelineRef(lk.run, lk.run.PipelineID)
	var why string
	switch reason.Code {
	case pipefix.E4NeverEligible:
		why = fmt.Sprintf("the failure is classified %s, and gonk only proposes fixes for failures classified `code`, `test` or `configuration`", class)
	case pipefix.E4NotEnabled:
		why = fmt.Sprintf("the failure is classified %s, which this project's `pipeline_fix.classes` does not include", class)
	case pipefix.E5NoTargets:
		why = "the diagnosis names no failed job, so there is no job a fix could be judged on"
	case pipefix.E5Unjudgeable:
		why = "the failed jobs the diagnosis names cannot be matched to the jobs gonk recorded for this run"
	case pipefix.E6NoCitedFiles:
		why = "the diagnosis cites no file, so a fix has nothing it may change"
	case pipefix.E6OutsideEnvelope:
		why = "a file the diagnosis cites is outside what a fix of this class may change here"
		for _, c := range lk.facts.Scope.Cited {
			if !containsString(lk.facts.Scope.Envelope, c.Class) {
				why = fmt.Sprintf("the cited file %s is of path class %s, which a %s fix may not change in this project",
					codeSpan(c.Path), codeSpan(c.Class), class)
				break
			}
		}
	case reasonOutsidePaths:
		why = "none of the files the diagnosis cites is inside this project's `pipeline_fix.paths`"
	case pipefix.E8Drifted:
		why = "the same jobs now fail differently, and gonk does not fix a stale diagnosis"
	case pipefix.E9ClosedUnmerged:
		why = "a fix merge request for this cause was closed without being merged"
	case pipefix.E11ChainExhausted:
		why = fmt.Sprintf("%d fix merge requests have already merged on this target since its last green pipeline, the most this project allows (`pipeline_fix.max_chain`)",
			lk.facts.Serial.MergedSinceGreen)
	case pipefix.E12CorrelationHold:
		why = "this run is part of a failure whose common cause is outside this project"
	default:
		why = "it is not eligible for one"
	}
	var b strings.Builder
	if reason.NeedsScope {
		fmt.Fprintf(&b, "**No fix merge request: more scope is needed.** gonk will not open one for the failure diagnosed in %s: %s.", about, why)
		b.WriteString("\n\nA person can make the change, or widen what gonk may change in `.gonk.yml`. gonk will not retry this on its own.")
	} else {
		fmt.Fprintf(&b, "**No fix merge request.** gonk will not open one for the failure diagnosed in %s: %s.", about, why)
	}
	b.WriteString("\n\n" + fixNoteFooter)
	return b.String()
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- a fix whose merge request is open ------------------------------------------

// holdFixForVerification is what the sweep does with a fix the apply has just
// opened a merge request for: write the record -- still `running`, still
// holding its reservation, now carrying the fix state, the commit and the
// merge request -- with a PutIf over the revision this pass read, and close
// the finished session. NO OUTCOME IS REPORTED: the exit check reports it.
//
// A lost write is not repaired here: the record is then still a running fix
// with no fix state, so the next tick judges the same finished session again,
// and the apply converges on the branch, commit and merge request it finds.
func holdFixForVerification(ctx context.Context, d sweepDeps, rec beadstore.Record, readRevision string) {
	if _, err := d.Store.PutIf(ctx, rec, readRevision); errors.Is(err, beadstore.ErrStale) {
		d.Log.Warn("sweep: pipeline fix: the record changed after this pass read it; leaving the newer record in place",
			"bead", rec.BeadAnchor, "mr", rec.PipelineFix.MRIID)
	} else if err != nil {
		d.Log.Error("sweep: pipeline fix: the merge request is open but the record could not be written; "+
			"the next tick runs the apply again and adopts it", "bead", rec.BeadAnchor, "mr", rec.PipelineFix.MRIID, "err", err)
		return // the session's output is still needed for that
	}
	d.Log.Info("sweep: pipeline fix merge request open; waiting for the exit check", "bead", rec.BeadAnchor,
		"fix_state", rec.PipelineFix.State, "mr", rec.PipelineFix.MRIID, "commit", rec.PipelineFix.FixCommitSHA,
		"verify_deadline", rec.PipelineFix.VerifyDeadline)
	closeSession(ctx, d, rec)
}

// pipefixJudge judges a fix whose merge request is open: the exit check on
// its pipeline (plan 6), the outcome report (7.3), the next attempt. It is
// part VERIFY's judgeOpenFix (sweep_pipefix_verify.go); a variable so a test
// can run the sweep as a build without it.
var pipefixJudge func(ctx context.Context, d sweepDeps, rec beadstore.Record) = judgeOpenFix

// sweepRunningFix is sweepRunning for a pipeline-fix record that carries a
// fix state: the apply opened its merge request (awaiting-run, verifying) and
// the exit check now owns the record until it has a verdict. With no judge
// (a test's build without part VERIFY) it touches NOTHING: no outcome is
// reported, nothing is re-slung, and the reservation runs out at meter on its
// own (booked infra-failed, which does not escalate).
func sweepRunningFix(ctx context.Context, d sweepDeps, rec beadstore.Record) {
	if pipefixJudge != nil {
		pipefixJudge(ctx, d, rec)
		return
	}
	if reservationExpired(rec, d.Now()) {
		d.Log.Warn("sweep: a pipeline fix's merge request is open past its reservation and nothing in this build judges it; "+
			"the record is left as it is (gonk-2usw part VERIFY)",
			"bead", rec.BeadAnchor, "session", rec.SessionID, "deadline", runningDeadline(rec))
	}
}
