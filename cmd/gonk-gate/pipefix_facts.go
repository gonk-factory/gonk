package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// THE READ SIDE OF THE PIPELINE-FIX EXIT CHECK (gonk-2usw, plan 6.1 / 6.3).
//
// pipefix.Judge is pure: it decides from fact structs. This file fills the
// judge-time half of those structs -- the merge request, its pipelines, every
// attempt of their jobs, the failed attempts' logs, and the base-vs-head CI
// definition delta -- from GitLab, and (for a caller with no fix record yet:
// the `pipefix-judge` subcommand and its probe) the base pipeline's facts a
// fix dispatch would have recorded.
//
// THE ONE RULE: A READ THAT FAILED LEAVES ITS FACT GROUP "NOT READ". Never a
// zero value that reads as a fact: an unread job list is not "the pipeline has
// no jobs", an unread log is not "an empty log", an unread definition is not
// "no change". Judge turns every "not read" into no verdict (plan 6.3: "a read
// failure is never a verdict"). A reply that parses but is not the object
// asked for -- GitLab answers errors as JSON too -- is a failed read (the
// 2026-09-07 rule): each object is checked by a NAMED field here, in addition
// to what pkg/glab asserts, because this code is written against an interface.
//
// JOB NAMES are put through jobNameText -- the function that produced the
// names on the record (FACTS deviation 5) -- before anything compares them.

// pipefixReader is everything the exit check reads from GitLab. EVERY METHOD
// IS A GET; pipefixReaderMethods pins the set and a test fails when a method
// that is not on that list is added. *glab.Client satisfies it; so does the
// read-only client of the `pipefix-judge` subcommand.
type pipefixReader interface {
	GetMergeRequest(ctx context.Context, projectID, mrIID int64) (*glab.MergeRequest, error)
	ListMRPipelines(ctx context.Context, projectID, mrIID int64) ([]glab.Pipeline, error)
	GetPipeline(ctx context.Context, projectID, pipelineID int64) (*glab.Pipeline, error)
	ListPipelineAttempts(ctx context.Context, projectID, pipelineID int64) ([]glab.Job, error)
	GetJobTraceTail(ctx context.Context, projectID, jobID, maxRead, tail int64) (*glab.TraceTail, error)
	CILintStatic(ctx context.Context, projectID int64, sha string) (*glab.CILint, error)
}

var _ pipefixReader = (*glab.Client)(nil)

// pipefixReaderMethods is the closed list of pipefixReader's methods, each of
// which is known to send only GETs.
var pipefixReaderMethods = []string{
	"CILintStatic", "GetJobTraceTail", "GetMergeRequest", "GetPipeline", "ListMRPipelines", "ListPipelineAttempts",
}

// pipefixRead is one GitLab read the facts rest on -- for the log, and for a
// reader of the subcommand's output who wants to know WHY a group is unread.
type pipefixRead struct {
	// What: mr | mr-pipelines | jobs | trace | ci-lint | base-pipeline |
	// base-jobs | base-trace.
	What string `json:"what"`
	// Ref says which object: "!12", "pipeline 3301", "job 29001", a SHA.
	Ref string `json:"ref"`
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

// pipefixLint is one side of the definition comparison as GitLab answered it.
type pipefixLint struct {
	SHA      string               `json:"sha"`
	Valid    bool                 `json:"valid"`
	Errors   []string             `json:"errors,omitempty"`
	Includes []glab.CILintInclude `json:"includes,omitempty"`
	// MergedYAMLBytes is len(merged_yaml); the text itself is MergedYAML.
	MergedYAMLBytes int    `json:"merged_yaml_bytes"`
	MergedYAML      string `json:"merged_yaml,omitempty"`
}

// pipefixEvidence is what the live reads returned beside the facts.
type pipefixEvidence struct {
	Reads []pipefixRead `json:"reads"`
	// BaseLint, HeadLint: nil when that lint could not be read.
	BaseLint *pipefixLint `json:"base_lint,omitempty"`
	HeadLint *pipefixLint `json:"head_lint,omitempty"`
}

// pipefixBase is the base (diagnosed) pipeline as a fix dispatch records it.
type pipefixBase struct {
	// Read: every field below was read. False when ANY read failed.
	Read       bool   `json:"read"`
	PipelineID int64  `json:"pipeline_id"`
	SHA        string `json:"sha"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	// Jobs: every job of the pipeline, one entry per canonical name, with
	// its LATEST attempt's status (FixFacts.BaseJobs).
	Jobs []pipefix.BaseJob `json:"jobs"`
	// Recorded: for each target name whose latest attempt FAILED, the failure
	// facts the diagnosis dispatch records (beadstore.PipelineJob): the input
	// of pipefix.TargetSet.
	Recorded []pipefix.RecordedJob `json:"recorded"`
	Reads    []pipefixRead         `json:"reads"`
}

// readPipefixJudgeFacts fills the judge-time groups of JudgeFacts for the fix
// described by fix (which the caller RECORDED, or synthesised): the merge
// request, its pipelines with every attempt and the failed attempts' logs, and
// the definition delta between fix.FixBaseSHA and fix.FixCommitSHA. fix itself
// is copied through untouched. It never returns an error: what could not be
// read is "not read" in the facts and an entry in the evidence.
func readPipefixJudgeFacts(ctx context.Context, r pipefixReader, projectID int64, fix pipefix.FixFacts) (pipefix.JudgeFacts, pipefixEvidence) {
	var ev pipefixEvidence
	facts := pipefix.JudgeFacts{Fix: fix}
	targets := make([]string, 0, len(fix.Targets))
	for _, t := range fix.Targets {
		targets = append(targets, t.Name())
	}

	var rd pipefixRead
	facts.MR, rd = readPipefixMR(ctx, r, projectID, fix.MRIID)
	ev.Reads = append(ev.Reads, rd)

	var reads []pipefixRead
	facts.Pipelines, reads = readPipefixPipelines(ctx, r, projectID, fix.MRIID, fix.FixCommitSHA, targets)
	ev.Reads = append(ev.Reads, reads...)

	facts.Definition, ev.BaseLint, ev.HeadLint, reads = readPipefixDefinition(ctx, r, projectID, fix.FixBaseSHA, fix.FixCommitSHA, targets)
	ev.Reads = append(ev.Reads, reads...)
	return facts, ev
}

func pipefixFailed(what, ref string, err error) pipefixRead {
	return pipefixRead{What: what, Ref: ref, Err: err.Error()}
}

// readPipefixMR reads the fix merge request. The reply must be THAT merge
// request (iid) and carry a state; anything else is "not read".
func readPipefixMR(ctx context.Context, r pipefixReader, projectID, mrIID int64) (pipefix.MRFacts, pipefixRead) {
	ref := fmt.Sprintf("!%d", mrIID)
	mr, err := r.GetMergeRequest(ctx, projectID, mrIID)
	switch {
	case err != nil:
		return pipefix.MRFacts{}, pipefixFailed("mr", ref, err)
	case mr == nil:
		return pipefix.MRFacts{}, pipefixFailed("mr", ref, fmt.Errorf("no merge request in the reply"))
	case mr.IID != mrIID:
		return pipefix.MRFacts{}, pipefixFailed("mr", ref, fmt.Errorf("the reply carries iid %d", mr.IID))
	case mr.State == "":
		return pipefix.MRFacts{}, pipefixFailed("mr", ref, fmt.Errorf("the reply carries no state"))
	}
	return pipefix.MRFacts{Read: true, IID: mr.IID, State: mr.State, SHA: mr.SHA}, pipefixRead{What: "mr", Ref: ref, OK: true}
}

// pipefixPipelineProblem says why a listed object is not a pipeline of this
// project: "" when it is. GitLab's error object decodes into a Pipeline with
// every field zero, so each field the exit check reasons from is required.
func pipefixPipelineProblem(p glab.Pipeline, projectID int64) string {
	switch {
	case p.ID == 0:
		return "an entry carries no id"
	case !glab.IsCommitSHA(p.SHA):
		return fmt.Sprintf("pipeline %d carries sha %q, not a commit", p.ID, p.SHA)
	case p.Source == "":
		return fmt.Sprintf("pipeline %d carries no source", p.ID)
	case p.Status == "":
		return fmt.Sprintf("pipeline %d carries no status", p.ID)
	case p.ProjectID != projectID:
		return fmt.Sprintf("pipeline %d carries project_id %d", p.ID, p.ProjectID)
	}
	return ""
}

// readPipefixPipelines lists EVERY pipeline of the merge request (Judge picks
// the candidates, J1) and, for those of fixCommitSHA that are not child
// pipelines, every attempt of every job; a FAILED attempt of a job named in
// targets also has its log read.
func readPipefixPipelines(ctx context.Context, r pipefixReader, projectID, mrIID int64, fixCommitSHA string, targets []string) (pipefix.PipelineFacts, []pipefixRead) {
	ref := fmt.Sprintf("!%d", mrIID)
	listed, err := r.ListMRPipelines(ctx, projectID, mrIID)
	if err != nil {
		return pipefix.PipelineFacts{}, []pipefixRead{pipefixFailed("mr-pipelines", ref, err)}
	}
	for _, p := range listed {
		if problem := pipefixPipelineProblem(p, projectID); problem != "" {
			return pipefix.PipelineFacts{}, []pipefixRead{pipefixFailed("mr-pipelines", ref, fmt.Errorf("not a pipeline list: %s", problem))}
		}
	}
	reads := []pipefixRead{{What: "mr-pipelines", Ref: ref, OK: true}}
	// targets are T's names: canonical already (they come from the record).
	inT := map[string]bool{}
	for _, t := range targets {
		inT[t] = true
	}
	facts := pipefix.PipelineFacts{Read: true}
	for _, p := range listed {
		out := pipefix.Pipeline{ID: p.ID, SHA: p.SHA, Source: p.Source, Status: p.Status}
		if p.SHA == fixCommitSHA && p.Source != pipefix.SourceParentPipeline {
			var jr []pipefixRead
			out.Jobs, out.JobsRead, jr = readPipefixAttempts(ctx, r, projectID, p.ID, inT)
			// ListPipelineAttempts IS the include_retried=true listing.
			out.IncludeRetried = out.JobsRead
			reads = append(reads, jr...)
		}
		facts.Pipelines = append(facts.Pipelines, out)
	}
	return facts, reads
}

// readPipefixAttempts reads every job object of one pipeline.
func readPipefixAttempts(ctx context.Context, r pipefixReader, projectID, pipelineID int64, inT map[string]bool) ([]pipefix.Attempt, bool, []pipefixRead) {
	ref := fmt.Sprintf("pipeline %d", pipelineID)
	jobs, err := r.ListPipelineAttempts(ctx, projectID, pipelineID)
	if err != nil {
		return nil, false, []pipefixRead{pipefixFailed("jobs", ref, err)}
	}
	for _, j := range jobs {
		if j.ID == 0 || j.Name == "" || j.Status == "" {
			return nil, false, []pipefixRead{pipefixFailed("jobs", ref,
				fmt.Errorf("not a job list: an entry carries id %d, name %q, status %q", j.ID, j.Name, j.Status))}
		}
	}
	reads := []pipefixRead{{What: "jobs", Ref: ref, OK: true}}
	attempts := make([]pipefix.Attempt, 0, len(jobs))
	for _, j := range jobs {
		a := pipefix.Attempt{
			ID: j.ID, Name: jobNameText(j.Name), Stage: j.Stage, Status: j.Status,
			FailureReason: j.FailureReason, AllowFailure: j.AllowFailure,
		}
		if a.Status == pipefix.StatusFailed && inT[a.Name] {
			var rd pipefixRead
			a.Trace, rd = readPipefixTrace(ctx, r, projectID, j.ID, "trace")
			reads = append(reads, rd)
		}
		attempts = append(attempts, a)
	}
	return attempts, true, reads
}

// readPipefixTrace reads the END of one failed job's log, with the bounds the
// diagnosis dispatch uses (fetchPipelineTrace), and reduces it to the first
// error line through pipefix.FailedTrace.
//
// What a read that is not the whole log means is FailedTrace's rule, not this
// function's (gonk-etvm): it is handed the two truncation flags and answers
// "not read" when the first error line cannot be established. This function
// only reports that as the failed read it is.
func readPipefixTrace(ctx context.Context, r pipefixReader, projectID, jobID int64, what string) (pipefix.Trace, pipefixRead) {
	ref := fmt.Sprintf("job %d", jobID)
	tt, err := fetchPipefixTrace(ctx, r, projectID, jobID)
	switch {
	case err != nil:
		return pipefix.Trace{}, pipefixFailed(what, ref, err)
	case tt == nil:
		return pipefix.Trace{}, pipefixFailed(what, ref, fmt.Errorf("no log in the reply"))
	}
	tr := pipefix.FailedTrace(pipefix.LogRead{Data: tt.Data, HeadTruncated: tt.HeadTruncated, TailTruncated: tt.TailTruncated})
	if !tr.Read {
		return pipefix.Trace{}, pipefixFailed(what, ref, fmt.Errorf(
			"the log was not read whole (start dropped: %v, stopped before the end: %v) and its first error line cannot be established",
			tt.HeadTruncated, tt.TailTruncated))
	}
	return tr, pipefixRead{What: what, Ref: ref, OK: true}
}

func fetchPipefixTrace(ctx context.Context, r pipefixReader, projectID, jobID int64) (*glab.TraceTail, error) {
	return r.GetJobTraceTail(ctx, projectID, jobID, pipelineTraceMaxRead, pipelineTraceTail)
}

// readPipefixDefinition computes J2's delta: cidef.Diff over the STATIC lint's
// merged YAML at the base and at the head commit, with targets as the
// diagnosed set. Either lint failing leaves the definition unread.
//
// An INVALID configuration is a lint that was read: its (empty) merged YAML
// goes to cidef, which reports that side unresolvable -- GitLab cannot build
// the definition, so there is none to compare.
//
// One thing only this reader can know is added to the delta: cidef sees the
// merged YAML, from which GitLab has already REMOVED every `include:`. An
// include that is not a `local` file of the commit itself (remote, template,
// another project's file, a component) has content no commit SHA of this
// project pins, so the two documents are not comparable (plan 6.2) -- it is
// reported as an unresolvable finding on the side(s) that have it.
func readPipefixDefinition(ctx context.Context, r pipefixReader, projectID int64, baseSHA, headSHA string, targets []string) (pipefix.DefinitionFacts, *pipefixLint, *pipefixLint, []pipefixRead) {
	var reads []pipefixRead
	lint := func(sha string) (*glab.CILint, *pipefixLint) {
		l, err := r.CILintStatic(ctx, projectID, sha)
		switch {
		case err != nil:
			reads = append(reads, pipefixFailed("ci-lint", sha, err))
			return nil, nil
		case l == nil:
			reads = append(reads, pipefixFailed("ci-lint", sha, fmt.Errorf("no lint result in the reply")))
			return nil, nil
		}
		reads = append(reads, pipefixRead{What: "ci-lint", Ref: sha, OK: true})
		return l, &pipefixLint{SHA: sha, Valid: l.Valid, Errors: l.Errors, Includes: l.Includes,
			MergedYAMLBytes: len(l.MergedYAML), MergedYAML: l.MergedYAML}
	}
	base, baseEv := lint(baseSHA)
	head, headEv := lint(headSHA)
	if base == nil || head == nil {
		return pipefix.DefinitionFacts{}, baseEv, headEv, reads
	}
	delta := cidef.Diff([]byte(base.MergedYAML), []byte(head.MergedYAML), targets)
	delta = withUnpinnedIncludes(delta, base.Includes, head.Includes)
	return pipefix.DefinitionFacts{Read: true, Delta: delta}, baseEv, headEv, reads
}

// withUnpinnedIncludes adds one unresolvable finding per include that is not
// a local file of the linted commit. A violation still outranks it (cidef's
// order, and J5's): Kind only becomes `unresolvable` when it was not already
// `violation`.
func withUnpinnedIncludes(d cidef.Delta, base, head []glab.CILintInclude) cidef.Delta {
	unpinned := func(incs []glab.CILintInclude) map[string]bool {
		out := map[string]bool{}
		for _, inc := range incs {
			if inc.Type != glab.IncludeTypeLocal {
				out[inc.Type+" "+inc.Location] = true
			}
		}
		return out
	}
	b, h := unpinned(base), unpinned(head)
	sides := map[string]cidef.Side{}
	for k := range b {
		sides[k] = cidef.SideBase
		if h[k] {
			sides[k] = cidef.SideBoth
		}
	}
	for k := range h {
		if !b[k] {
			sides[k] = cidef.SideHead
		}
	}
	if len(sides) == 0 {
		return d
	}
	keys := make([]string, 0, len(sides))
	for k := range sides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.Unresolvable = append(d.Unresolvable, cidef.Unresolvable{
			Side: sides[k], Key: "include",
			Reason: fmt.Sprintf("an include that is not a local file of the commit (%s): no commit of this project pins its content", k),
		})
	}
	if d.Kind != cidef.KindViolation {
		d.Kind = cidef.KindUnresolvable
	}
	return d
}

// readPipefixBase reads the base (diagnosed) pipeline the way a fix dispatch
// records it: every job with its LATEST attempt's status (FixFacts.BaseJobs),
// and, for each target whose latest attempt failed, the failure facts the
// diagnosis dispatch records -- stage, failure_reason, allow_failure and the
// signature of the log's first error line (pipelineContext.JobRecords). Any
// failed read leaves the whole of it unread.
func readPipefixBase(ctx context.Context, r pipefixReader, projectID, pipelineID int64, targets []string) pipefixBase {
	var reads []pipefixRead
	unread := func(rd pipefixRead) pipefixBase {
		return pipefixBase{PipelineID: pipelineID, Reads: append(reads, rd)}
	}
	ref := fmt.Sprintf("pipeline %d", pipelineID)
	p, err := r.GetPipeline(ctx, projectID, pipelineID)
	switch {
	case err != nil:
		return unread(pipefixFailed("base-pipeline", ref, err))
	case p == nil:
		return unread(pipefixFailed("base-pipeline", ref, fmt.Errorf("no pipeline in the reply")))
	case p.ID != pipelineID:
		return unread(pipefixFailed("base-pipeline", ref, fmt.Errorf("the reply carries id %d", p.ID)))
	}
	if problem := pipefixPipelineProblem(*p, projectID); problem != "" {
		return unread(pipefixFailed("base-pipeline", ref, fmt.Errorf("not a pipeline: %s", problem)))
	}
	reads = append(reads, pipefixRead{What: "base-pipeline", Ref: ref, OK: true})

	jobs, err := r.ListPipelineAttempts(ctx, projectID, pipelineID)
	if err != nil {
		return unread(pipefixFailed("base-jobs", ref, err))
	}
	// The latest attempt of a name is the job object with the highest id.
	latest := map[string]glab.Job{}
	for _, j := range jobs {
		if j.ID == 0 || j.Name == "" || j.Status == "" {
			return unread(pipefixFailed("base-jobs", ref,
				fmt.Errorf("not a job list: an entry carries id %d, name %q, status %q", j.ID, j.Name, j.Status)))
		}
		name := jobNameText(j.Name)
		if cur, seen := latest[name]; !seen || j.ID > cur.ID {
			latest[name] = j
		}
	}
	reads = append(reads, pipefixRead{What: "base-jobs", Ref: ref, OK: true})
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	sort.Strings(names)
	base := pipefixBase{PipelineID: p.ID, SHA: p.SHA, Status: p.Status, Source: p.Source}
	for _, name := range names {
		j := latest[name]
		base.Jobs = append(base.Jobs, pipefix.BaseJob{Name: name, Status: j.Status, AllowFailure: j.AllowFailure})
	}

	seen := map[string]bool{}
	for _, t := range targets {
		name := jobNameText(t)
		j, has := latest[name]
		if seen[name] || !has || j.Status != pipefix.StatusFailed {
			// Not a failed job of the run: nothing is recorded for it, and
			// pipefix.TargetSet says so.
			continue
		}
		seen[name] = true
		trace, rd := readPipefixTrace(ctx, r, projectID, j.ID, "base-trace")
		if !trace.Read {
			return unread(rd)
		}
		reads = append(reads, rd)
		base.Recorded = append(base.Recorded, pipefix.RecordedJob{
			Name: name, Stage: j.Stage, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure,
			ErrorSignature: pipelinectx.ErrorSignature(trace.FirstError),
			Generic:        pipelinectx.GenericFirstError(trace.FirstError),
			FactsRecorded:  true,
		})
	}
	base.Read, base.Reads = true, reads
	return base
}
