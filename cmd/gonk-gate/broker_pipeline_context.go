package main

// The pipeline agent's context, fetched CONTROLLER-SIDE at dispatch (gonk-tios
// D6). The pod holds no forge credential, so every fact the agent may reason
// from -- and every log line it may quote -- is fetched here with the bot PAT,
// bounded by the caps in pkg/pipelinectx, and handed to renderPipelinePrompt.
//
// TWO PROPERTIES THE REST OF THE CHAIN DEPENDS ON:
//
//  1. NOTHING IS GUESSED. A block whose fetch failed is recorded as
//     unavailable and rendered as an explicit "unavailable" line; it is never
//     filled from a default, a neighbour, or an earlier pipeline. Only the
//     pipeline itself and its job list are essential: without them there is
//     no failed-job set to record or to validate evidence against
//     (effects.DiagnosisContext), so their failure fails the build.
//
//  2. THE EXCERPT IS REPRODUCIBLE. The sweep re-fetches each excerpted job's
//     trace and re-derives the excerpt (plan D8 step 5) and the hash must
//     match. The excerpt is a function of (trace bytes, Options), and Options
//     is (HeadTruncated, TailTruncated, MaxBytes). The first two follow from
//     the trace and the two fetch constants below, which the sweep uses too
//     (fetchPipelineTrace). MaxBytes is NOT re-derivable from the record's job
//     list once the 28 KiB prompt cap has shrunk the excerpts (the shrink
//     depends on the size of every other block -- a commit message, an MR
//     description -- which can change between dispatch and judgement), so it
//     is RECORDED: pipelineContext.ExcerptMaxBytes is the ONE per-job cap every
//     excerpt of this dispatch was built with, and P7 stores it on the bead
//     (Record.PipelineExcerptMaxBytes) and passes it back through
//     pipelineExcerptOptions. Decision recorded in the plan, P6 as built.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// pipelineReader is the forge surface the context builder reads. Satisfied by
// *glab.Client; every call is a GET.
type pipelineReader interface {
	GetPipeline(ctx context.Context, projectID, pipelineID int64) (*glab.Pipeline, error)
	ListPipelines(ctx context.Context, projectID int64, ref, status string, perPage int) ([]glab.Pipeline, error)
	ListMRPipelines(ctx context.Context, projectID, mrIID int64) ([]glab.Pipeline, error)
	ListPipelineJobs(ctx context.Context, projectID, pipelineID int64, scope string, includeRetried bool) ([]glab.Job, error)
	GetJobTraceTail(ctx context.Context, projectID, jobID, maxRead, tail int64) (*glab.TraceTail, error)
	// CILintStatic, never a dry run: a dry run resolves `include: local:` on
	// the DEFAULT BRANCH, so the definition shown for a job could be another
	// commit's (gonk-z6gb).
	CILintStatic(ctx context.Context, projectID int64, sha string) (*glab.CILint, error)
	GetCommit(ctx context.Context, projectID int64, sha string) (*glab.Commit, error)
	GetMergeRequest(ctx context.Context, projectID, mrIID int64) (*glab.MergeRequest, error)
}

var _ pipelineReader = (*glab.Client)(nil)

// traceTailReader is the one call fetchPipelineTrace makes.
type traceTailReader interface {
	GetJobTraceTail(ctx context.Context, projectID, jobID, maxRead, tail int64) (*glab.TraceTail, error)
}

const (
	// pipelineTraceMaxRead bounds how many bytes of ONE job log are streamed.
	// It is deliberately LARGE: GitLab's job-trace endpoint ignores Range
	// (verified live 2026-10-04: 200 and the full body), so the read starts at
	// byte 0 and the END of the log -- where a failure usually is -- is reached
	// only by reading through. A small value here would silently keep the
	// HEAD of a long log. 128 MiB is above GitLab's default job-log size limit
	// (100 MB); memory stays bounded by the tail buffer, not by this.
	pipelineTraceMaxRead = 128 << 20
	// pipelineTraceTail is how much of the end of the log is kept: the most
	// pipelinectx ever considers (D7: "read at most 8 MiB, keep the tail").
	pipelineTraceTail = pipelinectx.MaxTraceBytes
	// pipelineHistoryRuns: terminal pipelines on the target shown per job (D6).
	pipelineHistoryRuns = 5
	// pipelineRunWindow: how many of the target's pipelines are scanned for the
	// run facts and the history. One page, never paginated.
	pipelineRunWindow = 100
	// pipelineExcerptFloorBytes: the per-job excerpt cap never shrinks below
	// this while fitting the prompt. Reaching it with the prompt still over
	// MaxPromptBytes is an error (TestWorstCasePipelineContextFits proves the
	// block caps leave room above it).
	pipelineExcerptFloorBytes = 512
)

// pipelineContextRequest names what to diagnose. Everything in it comes from
// the order (D12), not from the agent.
type pipelineContextRequest struct {
	ProjectID   int64
	Project     string // path_with_namespace, for the prompt's first line
	PipelineID  int64  // the pipeline this session diagnoses
	Target      pipelinectx.Target
	LastSuccess int64 // the run's last green pipeline on the target, 0 = none
	// Attempt is the attempt this session is, from 1 (0 = unknown: no
	// feedback is rendered). Refused is the record's feedback about an
	// earlier attempt's refused answer (retry_feedback.go); the prompt
	// renders it only when it is about an attempt before this one.
	Attempt int
	Refused beadstore.RetryFeedback
}

// pipelineContext is everything renderPipelinePrompt shows. Exported-style
// field names are for the reader; the type is package-private.
type pipelineContext struct {
	Req      pipelineContextRequest
	Pipeline glab.Pipeline

	Commit *glab.Commit       // nil = unavailable
	MR     *glab.MergeRequest // MR target only; nil = unavailable (or not an MR target)
	// Lint is gonk's own STATIC CI lint AT THE PIPELINE'S COMMIT
	// (glab.CILintStatic: includes resolved at that commit, gonk-z6gb), kept
	// ONLY for its merged YAML (the job definitions). nil = unavailable. It is
	// a question gonk asked, not a fact about the pipeline being diagnosed, so
	// neither its validity nor its errors are ever rendered into the prompt
	// (gonk-mf3v).
	Lint *glab.CILint
	Run  pipelineRunFacts

	// Failed are the jobs that failed the pipeline (status failed,
	// allow_failure false, latest attempt), in stage order then name. The
	// first pipelinectx.MaxExcerptJobs carry Slot and get an excerpt.
	Failed []*pipelineFailedJob
	// Others are every other job of the pipeline (incl. failed jobs that were
	// allowed to fail), in stage order then name.
	Others []glab.Job

	// ExcerptMaxBytes is the per-job byte cap every excerpt was built with
	// (pipelinectx.Options.MaxBytes). Final after fitPipelinePrompt. RECORD IT.
	ExcerptMaxBytes int

	// Unavailable lists "<block>: <error>" for every fetch that failed, for
	// the caller to log. Never rendered: an error text can carry hosts and
	// paths, and the prompt says only that the block is unavailable.
	Unavailable []string
}

// pipelineRunFacts are the run facts block (D6): the run's first failing
// pipeline, how many failed since the last success, and the last success.
type pipelineRunFacts struct {
	Available     bool
	First         *glab.Pipeline // oldest failed pipeline after LastSuccess in the window
	FailuresSince int            // failed pipelines after LastSuccess in the window
	WindowFull    bool           // the window was full: FailuresSince is a lower bound
	LastSuccess   *glab.Pipeline // nil when Req.LastSuccess == 0 or it could not be read
}

// pipelineFailedJob is one failed job and what was fetched about it.
type pipelineFailedJob struct {
	Job  glab.Job
	Slot bool // among the first MaxExcerptJobs: gets definition, history, excerpt

	Definition          string // the job's block from the merged CI YAML, capped
	DefinitionAvailable bool   // false: lint unavailable/invalid, or the job is not in it

	History          []pipelineJobRun
	HistoryAvailable bool

	TraceAvailable bool
	trace          *glab.TraceTail // held only until fitPipelinePrompt finishes
	Excerpt        pipelinectx.JobExcerpt
}

// pipelineJobRun is the same job name in one earlier terminal pipeline.
type pipelineJobRun struct {
	PipelineID int64
	Status     string // the job's status there, or "absent"
	Runner     string // the runner description, "" when none
	FinishedAt *time.Time
}

// errPipelineContext wraps the failures that make a context impossible.
var errPipelineContext = errors.New("pipeline context")

// buildPipelineContext fetches the context for one failed pipeline. It
// returns an error only when the pipeline or its job list cannot be read;
// every other failure becomes an "unavailable" block. Excerpts are built at
// pipelinectx.JobBudget(n); fitPipelinePrompt may shrink them.
func buildPipelineContext(ctx context.Context, r pipelineReader, req pipelineContextRequest) (*pipelineContext, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: no forge reader configured", errPipelineContext)
	}
	if err := req.Target.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", errPipelineContext, err)
	}
	pl, err := r.GetPipeline(ctx, req.ProjectID, req.PipelineID)
	if err != nil {
		return nil, fmt.Errorf("%w: get pipeline %d: %v", errPipelineContext, req.PipelineID, err)
	}
	jobs, err := r.ListPipelineJobs(ctx, req.ProjectID, req.PipelineID, "", false)
	if err != nil {
		return nil, fmt.Errorf("%w: list jobs of pipeline %d: %v", errPipelineContext, req.PipelineID, err)
	}

	pc := &pipelineContext{Req: req, Pipeline: *pl}
	unavailable := func(block string, err error) {
		pc.Unavailable = append(pc.Unavailable, block+": "+err.Error())
	}

	failed, others := splitPipelineJobs(jobs)
	for i, j := range failed {
		pc.Failed = append(pc.Failed, &pipelineFailedJob{Job: j, Slot: i < pipelinectx.MaxExcerptJobs})
	}
	pc.Others = others

	if c, err := r.GetCommit(ctx, req.ProjectID, pl.SHA); err != nil {
		unavailable("commit", err)
	} else {
		pc.Commit = c
	}
	if req.Target.Kind == pipelinectx.TargetKindMR {
		if mr, err := r.GetMergeRequest(ctx, req.ProjectID, req.Target.MRIID); err != nil {
			unavailable("merge request", err)
		} else {
			pc.MR = mr
		}
	}
	if l, err := r.CILintStatic(ctx, req.ProjectID, pl.SHA); err != nil {
		unavailable("ci definition", err)
	} else {
		pc.Lint = l
		// A lint that answers "invalid", or with no YAML, yields no job
		// definitions. WHY goes to the log -- never to the prompt, where it
		// would read as an error of the pipeline (gonk-mf3v).
		if reason := lintUnusable(l); reason != "" {
			unavailable("ci definition", errors.New(reason))
		}
	}

	// The target's recent pipelines: run facts and job history both come from
	// this one page.
	var window []glab.Pipeline
	var werr error
	if req.Target.Kind == pipelinectx.TargetKindMR {
		window, werr = r.ListMRPipelines(ctx, req.ProjectID, req.Target.MRIID)
	} else {
		window, werr = r.ListPipelines(ctx, req.ProjectID, req.Target.Ref, "", pipelineRunWindow)
	}
	if werr != nil {
		unavailable("target pipelines", werr)
	} else {
		pc.Run = runFacts(ctx, r, req, window, unavailable)
	}

	slots := pc.slotJobs()
	if werr == nil {
		fillJobHistory(ctx, r, req, window, slots, unavailable)
	}
	for _, fj := range slots {
		if pc.Lint != nil {
			fj.Definition, fj.DefinitionAvailable = pipelineJobDefinition(pc.Lint, fj.Job.Name)
		}
		tt, err := fetchPipelineTrace(ctx, r, req.ProjectID, fj.Job.ID)
		if err != nil {
			unavailable(fmt.Sprintf("log of job %q", fj.Job.Name), err)
			continue
		}
		fj.trace, fj.TraceAvailable = tt, true
	}
	pc.setExcerptCap(pipelinectx.JobBudget(pc.excerptedCount()))
	return pc, nil
}

// fetchPipelineTrace reads the end of one job's log with the fixed constants
// above. The sweep's integrity check (D8 step 5) MUST fetch through this same
// function, or HeadTruncated/TailTruncated -- and so the excerpt hash -- can
// differ.
//
// It asks for the one call it makes, so the fix context (pipefixContextReader,
// gonk-2usw) reads a job log through this function too.
func fetchPipelineTrace(ctx context.Context, r traceTailReader, projectID, jobID int64) (*glab.TraceTail, error) {
	return r.GetJobTraceTail(ctx, projectID, jobID, pipelineTraceMaxRead, pipelineTraceTail)
}

// pipelineExcerptOptions are the excerpt options for a fetched trace at the
// recorded per-job cap. Dispatch and the sweep's re-derivation both use it.
func pipelineExcerptOptions(tt *glab.TraceTail, maxBytes int) pipelinectx.Options {
	return pipelinectx.Options{HeadTruncated: tt.HeadTruncated, TailTruncated: tt.TailTruncated, MaxBytes: maxBytes}
}

// splitPipelineJobs separates the jobs that failed the pipeline from the rest
// and orders both: stage order, then name. Stage order is the order GitLab
// created the stages' jobs in -- each stage ranked by the lowest job id in it
// -- which needs no YAML and is the same for every reader.
func splitPipelineJobs(jobs []glab.Job) (failed, others []glab.Job) {
	rank := map[string]int64{}
	for _, j := range jobs {
		if r, ok := rank[j.Stage]; !ok || j.ID < r {
			rank[j.Stage] = j.ID
		}
	}
	for _, j := range jobs {
		if j.Status == "failed" && !j.AllowFailure {
			failed = append(failed, j)
		} else {
			others = append(others, j)
		}
	}
	less := func(s []glab.Job) func(a, b int) bool {
		return func(a, b int) bool {
			if ra, rb := rank[s[a].Stage], rank[s[b].Stage]; ra != rb {
				return ra < rb
			}
			if s[a].Name != s[b].Name {
				return s[a].Name < s[b].Name
			}
			return s[a].ID < s[b].ID
		}
	}
	sort.SliceStable(failed, less(failed))
	sort.SliceStable(others, less(others))
	return failed, others
}

func (pc *pipelineContext) slotJobs() []*pipelineFailedJob {
	var out []*pipelineFailedJob
	for _, fj := range pc.Failed {
		if fj.Slot {
			out = append(out, fj)
		}
	}
	return out
}

// excerptedCount is how many jobs share the excerpt budget: slot jobs whose
// log was read.
func (pc *pipelineContext) excerptedCount() int {
	n := 0
	for _, fj := range pc.Failed {
		if fj.Slot && fj.TraceAvailable {
			n++
		}
	}
	return n
}

// setExcerptCap (re)builds every excerpt at one per-job cap. The same cap for
// every job is what "shrink evenly" means, and it is one number to record.
func (pc *pipelineContext) setExcerptCap(maxBytes int) {
	pc.ExcerptMaxBytes = maxBytes
	for _, fj := range pc.Failed {
		if fj.Slot && fj.TraceAvailable && fj.trace != nil {
			fj.Excerpt = pipelinectx.ExcerptWith(fj.trace.Data, pipelineExcerptOptions(fj.trace, maxBytes))
		}
	}
}

// releaseTraces drops the raw logs once the excerpts are final.
func (pc *pipelineContext) releaseTraces() {
	for _, fj := range pc.Failed {
		fj.trace = nil
	}
}

// pipelineJobRecord is what dispatch records per failed job (D12
// Record.PipelineJobs) and what the sweep re-derives against.
type pipelineJobRecord struct {
	ID            int64
	Name          string
	Excerpted     bool   // an excerpt was built and shown
	ExcerptSHA256 string // "" unless Excerpted
	Groundable    bool   // the excerpt holds at least one quotable log line

	// The job's failure facts (gonk-2usw): GitLab's own stage, failure_reason
	// and allow_failure, and the signature of the excerpt's first error line.
	// Stage and FailureReason are GitLab's strings as returned -- they are
	// compared against GitLab's strings again later, never rendered from here.
	Stage          string
	FailureReason  string
	AllowFailure   bool
	ErrorSignature string // "" unless Excerpted and the log has an error line
	ErrorGeneric   bool   // pipelinectx.GenericFirstError of that line (true when there is none)
}

// JobRecords lists every failed job (excerpted or not), in prompt order.
// ExcerptVersion is pipelinectx.ExcerptVersion and the cap ExcerptMaxBytes.
func (pc *pipelineContext) JobRecords() []pipelineJobRecord {
	out := make([]pipelineJobRecord, 0, len(pc.Failed))
	for _, fj := range pc.Failed {
		// The canonical name, the one the prompt shows (jobNameText, R9).
		r := pipelineJobRecord{ID: fj.Job.ID, Name: jobNameText(fj.Job.Name),
			Stage: fj.Job.Stage, FailureReason: fj.Job.FailureReason, AllowFailure: fj.Job.AllowFailure}
		// The first error line is found before the caps select what is shown,
		// so the signature does not depend on ExcerptMaxBytes
		// (TestRecordedSignatureIsRecomputableFromTheJobLog).
		firstError := ""
		if fj.Slot && fj.TraceAvailable {
			r.Excerpted, r.ExcerptSHA256, r.Groundable = true, fj.Excerpt.SHA256, fj.Excerpt.Groundable()
			firstError, r.ErrorSignature = fj.Excerpt.FirstError, fj.Excerpt.ErrorSignature
		}
		r.ErrorGeneric = pipelinectx.GenericFirstError(firstError)
		out = append(out, r)
	}
	return out
}

// DiagnosisContext is what effects.ValidateDiagnosis checks a batch against.
// The sweep builds it from the RECORD; this is the dispatch-side twin, so the
// two can be compared in a test.
func (pc *pipelineContext) DiagnosisContext() effects.DiagnosisContext {
	var dc effects.DiagnosisContext
	for _, r := range pc.JobRecords() {
		dc.FailedJobs = append(dc.FailedJobs, effects.FailedJob{Name: r.Name, HasExcerpt: r.Groundable})
	}
	return dc
}

// runFacts derives the run block from the target's pipeline window.
func runFacts(ctx context.Context, r pipelineReader, req pipelineContextRequest, window []glab.Pipeline,
	unavailable func(string, error)) pipelineRunFacts {
	rf := pipelineRunFacts{Available: true, WindowFull: len(window) >= pipelineRunWindow}
	for i := range window {
		p := window[i]
		if p.ID == req.LastSuccess {
			rf.LastSuccess = &window[i]
		}
		if p.Status != "failed" || p.ID <= req.LastSuccess || p.ID > req.PipelineID {
			continue
		}
		rf.FailuresSince++
		if rf.First == nil || p.ID < rf.First.ID {
			rf.First = &window[i]
		}
	}
	if req.LastSuccess > 0 && rf.LastSuccess == nil {
		if p, err := r.GetPipeline(ctx, req.ProjectID, req.LastSuccess); err != nil {
			unavailable("last success", err)
		} else {
			rf.LastSuccess = p
		}
	}
	return rf
}

// fillJobHistory reads the jobs of the last pipelineHistoryRuns terminal
// pipelines on the target older than the diagnosed one, and records, per slot
// job, how the job of the same name ended there.
func fillJobHistory(ctx context.Context, r pipelineReader, req pipelineContextRequest, window []glab.Pipeline,
	slots []*pipelineFailedJob, unavailable func(string, error)) {
	if len(slots) == 0 {
		return
	}
	var prior []glab.Pipeline
	for _, p := range window {
		if p.ID < req.PipelineID && pipelinectx.TerminalForHealth(p.Status) {
			prior = append(prior, p)
		}
	}
	sort.Slice(prior, func(a, b int) bool { return prior[a].ID > prior[b].ID })
	if len(prior) > pipelineHistoryRuns {
		prior = prior[:pipelineHistoryRuns]
	}
	for _, fj := range slots {
		fj.HistoryAvailable = true
	}
	for _, p := range prior {
		jobs, err := r.ListPipelineJobs(ctx, req.ProjectID, p.ID, "", false)
		if err != nil {
			unavailable(fmt.Sprintf("jobs of pipeline %d", p.ID), err)
			for _, fj := range slots {
				fj.HistoryAvailable = false
			}
			return
		}
		for _, fj := range slots {
			run := pipelineJobRun{PipelineID: p.ID, Status: "absent"}
			for _, j := range jobs {
				if j.Name != fj.Job.Name {
					continue
				}
				run.Status, run.FinishedAt = j.Status, j.FinishedAt
				if j.Runner != nil {
					run.Runner = j.Runner.Description
				}
				break
			}
			fj.History = append(fj.History, run)
		}
	}
}

// lintUnusable says why a lint reply yields no job definitions, "" when it
// does. For the log only.
func lintUnusable(l *glab.CILint) string {
	switch {
	case l == nil:
		return "no CI lint reply"
	case !l.Valid:
		return "the CI lint at this commit reports the configuration invalid " +
			"(gonk's own lint call, not this pipeline's own error): " + strings.Join(l.Errors, "; ")
	case strings.TrimSpace(l.MergedYAML) == "":
		return "the CI lint reply carries no merged YAML"
	}
	return ""
}

// pipelineJobDefinition extracts one job's block from the merged CI YAML,
// followed by what it inherits that the merged YAML leaves unresolved: the
// templates it `extends` (GitLab's merged_yaml keeps `extends:` -- seen on a
// real reply, 2026-10-04) and the `default:` block, which is where an image
// set for every job lives. Redacted and capped at CapJobDefinitionPerJob.
// false when the lint reply carries no usable YAML or the job is not in it.
func pipelineJobDefinition(l *glab.CILint, jobName string) (string, bool) {
	if lintUnusable(l) != "" {
		return "", false
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(l.MergedYAML), &doc); err != nil || len(doc.Content) == 0 {
		return "", false
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return "", false
	}
	lookup := func(key string) *yaml.Node {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				return root.Content[i+1]
			}
		}
		return nil
	}
	job := lookup(jobName)
	if job == nil {
		return "", false
	}

	var b strings.Builder
	b.WriteString(yamlBlock(jobName, job))
	// extends: one name or a list; followed transitively, each once.
	seen := map[string]bool{jobName: true}
	queue := extendsOf(job)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		if tmpl := lookup(name); tmpl != nil {
			b.WriteString("# extended by the job above:\n")
			b.WriteString(yamlBlock(name, tmpl))
			queue = append(queue, extendsOf(tmpl)...)
		}
	}
	for _, key := range []string{"default", "image", "services", "before_script", "after_script"} {
		if n := lookup(key); n != nil {
			b.WriteString("# top-level, applies where the job does not override it:\n")
			b.WriteString(yamlBlock(key, n))
		}
	}
	red, _ := pipelinectx.Redact(b.String())
	return capPipelineBlock(red, pipelinectx.CapJobDefinitionPerJob), true
}

// extendsOf lists the templates a job extends. The reading of `extends` is
// pkg/cidef's (the package that resolves effective definitions for the
// pipeline-fix exit check, gonk-2usw), so the prompt and the check cannot
// come to read the key differently.
func extendsOf(job *yaml.Node) []string {
	return cidef.ExtendsOf(job)
}

// yamlBlock renders `key: <value>` as YAML (2-space indent).
func yamlBlock(key string, value *yaml.Node) string {
	m := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: key}, value,
	}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return key + ": (could not be rendered)\n"
	}
	_ = enc.Close()
	return buf.String()
}

// pipelineBlockTruncated marks a block cut by capPipelineBlock.
const pipelineBlockTruncated = "\n" + pipelinectx.MarkerPrefix + "block truncated]\n"

// capPipelineBlock bounds s to max bytes INCLUDING the truncation marker,
// cutting at a rune boundary.
func capPipelineBlock(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max-len(pipelineBlockTruncated)]
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1] // a partial rune at the cut point
			continue
		}
		break
	}
	return cut + pipelineBlockTruncated
}
