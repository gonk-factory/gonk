package main

// The pipefix agent's context, fetched CONTROLLER-SIDE at dispatch (gonk-2usw
// plan 5.2, part AGENT). It mirrors the pipeline agent's
// (broker_pipeline_context.go; read that file's header first) and reuses its
// trace fetch, excerpt, job-definition and rendering helpers, so a fix session
// and the diagnosis it follows read a job log through the same function.
//
// WHAT A FIX SESSION IS SHOWN, AND WHERE EACH PART COMES FROM:
//
//   - the landed DIAGNOSIS, from the record (beadstore.PipelineDiagnosis). It
//     is model-authored text, so it is rendered as data to check, fenced;
//   - the JOBS TO FIX: the target set T of plan 3.2 (pipefix.TargetSet over
//     the record), each as it failed in the pipeline named by the request --
//     the newest failing pipeline of the run, whose commit is the fix base;
//   - the CITED FILES, each classified by the SAME GATE that will judge the
//     answer (effects.ValidateEditPaths against the dispatch's envelope), so
//     "you may change this file" in the prompt and "this edit is in scope" at
//     the gate cannot come apart; and the content of each editable file as
//     read at the pinned base SHA, whole or not at all;
//   - the LIMITS, taken from the envelope's caps.
//
// THREE PROPERTIES THE REST OF THE CHAIN DEPENDS ON:
//
//  1. NOTHING IS GUESSED, as for the pipeline agent: a job definition or log
//     that could not be read is "(unavailable)", never filled in.
//  2. THE SESSION IS NOT STARTED WHEN IT COULD NOT SUCCEED. No landed
//     diagnosis, an unusable target set, a diagnosed job that is not failing
//     in the pipeline, a pipeline that did not run on the base commit, a file
//     read that failed for a reason other than "no such file" -- each is an
//     error, and no tokens are spent. A context in which NO cited file may be
//     edited is errPipefixNothingEditable: the answer could only be
//     needs-scope, which the controller can say itself for nothing (plan 3.2
//     E6, 4.4 way 1).
//  3. WHAT WAS SHOWN IS RECORDED AND RE-DERIVABLE (plan 5.3 A9): Facts
//     returns a beadstore.FixContext -- excerpt hashes at one recorded cap,
//     the diagnosis's hash, each editable blob's hash, the limits stated, the
//     prompt's hash -- and verifyPipefixContext re-reads GitLab and compares.
//
// A file's content is shown WHOLE OR NOT AT ALL. An `edit` must match text
// exactly once in the whole file, so a truncated view would invite an `old`
// that is unique in the part shown and ambiguous in the file. And it is shown
// UNALTERED or not at all: a file in which pipelinectx.Redact finds anything
// is left out rather than shown redacted, because redacted text is not what
// the blob holds and could never be matched. A file left out is read by the
// session in its checkout.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// pipefixAgent is the pipeline-fix trigger's broker agent (pack/agents/pipefix).
const pipefixAgent = "pipefix"

// pipefixContextReader is the forge surface the fix context reads. Satisfied
// by *glab.Client and by pipelineForge; every call is a GET. (The exit
// check's own read surface is pipefixReader, pipefix_facts.go.)
//
// The lint is CILintStatic at the pinned commit, never a dry run (gonk-z6gb):
// the definitions shown to the session are the ones the base commit carries.
type pipefixContextReader interface {
	GetPipeline(ctx context.Context, projectID, pipelineID int64) (*glab.Pipeline, error)
	ListPipelineJobs(ctx context.Context, projectID, pipelineID int64, scope string, includeRetried bool) ([]glab.Job, error)
	GetJobTraceTail(ctx context.Context, projectID, jobID, maxRead, tail int64) (*glab.TraceTail, error)
	CILintStatic(ctx context.Context, projectID int64, sha string) (*glab.CILint, error)
	GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error)
}

var (
	_ pipefixContextReader = (*glab.Client)(nil)
	_ pipefixContextReader = pipelineForge(nil)
)

const (
	// pipefixShowFileBytes: a file larger than this is not rendered into the
	// prompt (the session reads it in its checkout). pipefixShowTotalBytes
	// bounds all rendered file content together; files are taken in the
	// diagnosis's order until it is spent.
	pipefixShowFileBytes  = 6 << 10
	pipefixShowTotalBytes = 12 << 10
	// pipefixCapDiagnosisText bounds each of the diagnosis's summary and
	// remedy as rendered, in bytes (they are at most 1500 characters each).
	pipefixCapDiagnosisText = 1536
	// pipefixCapPreviousDiff bounds the previous attempt's diff as rendered.
	// A fix may change 3 files / 40 lines, which is 2-4 KB of this diff
	// (previousFixChange: the lines replaced and the lines put there), so 6 KiB
	// shows the largest allowed fix whole with room. It had been cut to 1024
	// to squeeze under the shared 28 KiB prompt cap (gonk-nf9i); with the
	// pipefix budget below there is no reason to cut it that short. A diff
	// over this cap is still cut, never silently: the prompt carries the
	// controller's own "[gonk: cut: N of M bytes shown]" line after the
	// block (previousDiffBlock).
	pipefixCapPreviousDiff = 6 << 10
	// pipefixMaxPromptBytes is the pipefix agent's OWN whole-prompt budget
	// (gonk-x06m). The owner confirmed 131072 tokens as the v1 context window
	// (gonk-gyj); 64 KiB of prompt is roughly 16-22k tokens, about one sixth
	// of it, which leaves the session its tool calls, its reading of the
	// checkout and its answer. pipelinectx.MaxPromptBytes (28 KiB) is the
	// DIAGNOSIS agent's cap, was sized before that confirmation, and is NOT
	// changed here: its prompt, its goldens and its fit are untouched.
	// Going over this budget is still an ERROR once file content has been left
	// out and the excerpts are at their floor -- never a cut fact or rule.
	pipefixMaxPromptBytes = 64 << 10
	// pipefixMaxPathBytes bounds a rendered path. A cited file whose path is
	// longer cannot be shown exactly and is therefore not editable.
	pipefixMaxPathBytes = 200
)

var (
	// errPipefixContext wraps every failure that makes a fix context impossible.
	errPipefixContext = errors.New("pipefix context")
	// errPipefixNothingEditable: no cited file may be edited under this
	// dispatch's envelope at the base commit. A session could only answer
	// needs-scope; the controller says so itself and starts none.
	errPipefixNothingEditable = errors.New("no cited file may be edited by this dispatch")
	// errPipefixContextChanged: what the session was shown no longer hashes to
	// the recorded context (verifyPipefixContext). Ours, never the agent's.
	errPipefixContextChanged = errors.New("pipefix context changed")
)

// pipefixRequest names what to fix. EVERYTHING in it is the controller's: the
// record's facts and the dispatch's decisions. Nothing comes from a session.
type pipefixRequest struct {
	ProjectID int64
	Project   string // path_with_namespace, for the prompt's first line
	Target    pipelinectx.Target
	// BaseSHA is the full commit the edits are applied against and the
	// checkout is pinned to (plan 3.2 E7: the target's head, which is the
	// commit of the run's newest failing pipeline).
	BaseSHA string
	// PipelineID is the failing pipeline whose jobs are shown: the newest
	// failing pipeline of the run. It must have run on BaseSHA.
	PipelineID int64
	// Diagnosis and Jobs are the DIAGNOSED run's record: its landed diagnosis
	// and every recorded failed job (Record.PipelineDiagnosis, PipelineJobs).
	// The target set T is computed from them here; a caller does not pass T.
	Diagnosis beadstore.PipelineDiagnosis
	Jobs      []beadstore.PipelineJob
	// Envelope is what THIS dispatch may touch (plan 4.2): the folded scope
	// narrowed to the diagnosis class's row and to pipeline_fix.paths, the
	// lift if one was granted, and the cited files. Built by the controller
	// and recorded by it; the same value is later passed to effects.PlanFix.
	Envelope effects.FixEnvelope
	// Attempt is the fix attempt this session is, from 1.
	Attempt int
	// Previous is the earlier attempt's outcome, nil on the first (plan 5.2:
	// "on attempt >= 2, the previous attempt's diff and the exit check's
	// result").
	Previous *pipefixPrevious
	// Refused is the record's feedback about an earlier attempt whose answer
	// a static gate refused (retry_feedback.go): the third source of
	// previous-attempt feedback, and the only one for an answer that never
	// became a commit. Rendered only when it is about an attempt before
	// Attempt.
	Refused beadstore.RetryFeedback
}

// pipefixPrevious is what an earlier attempt of the same fix did.
type pipefixPrevious struct {
	// Diff is the earlier attempt's change, as the controller computed it.
	// Rendered fenced and redacted: it is context, never text to copy.
	Diff string
	// Result is the controller's one-line statement of what the exit check
	// found (rendered through oneLine).
	Result string
}

// pipefixRequestFromRecord fills the record-derived half of a request from the
// DIAGNOSED run's record. The caller supplies the dispatch's own decisions.
func pipefixRequestFromRecord(rec beadstore.Record, baseSHA string, pipelineID int64, env effects.FixEnvelope, attempt int) (pipefixRequest, error) {
	t, err := pipelinectx.ParseTarget(rec.PipelineTarget)
	if err != nil {
		return pipefixRequest{}, fmt.Errorf("%w: %v", errPipefixContext, err)
	}
	return pipefixRequest{
		ProjectID: rec.ProjectID, Project: rec.Project, Target: t,
		BaseSHA: baseSHA, PipelineID: pipelineID,
		Diagnosis: rec.PipelineDiagnosis, Jobs: rec.PipelineJobs,
		Envelope: env, Attempt: attempt,
	}, nil
}

// pipefixContext is everything renderPipefixPrompt shows.
type pipefixContext struct {
	Req      pipefixRequest
	Pipeline glab.Pipeline
	// Jobs are the target set T, in T order, as each failed in Req.PipelineID.
	Jobs []*pipefixJob
	// Files are the diagnosis's cited files, in its order, deduplicated.
	Files []*pipefixFile
	// Limits are the numbers the prompt states.
	Limits beadstore.FixLimits
	// ExcerptMaxBytes is the per-job byte cap every excerpt was built with.
	// Final after fitPipefixPrompt. RECORDED (Facts).
	ExcerptMaxBytes int
	// Unavailable lists "<block>: <error>" for every non-essential fetch that
	// failed, for the caller to log. Never rendered.
	Unavailable []string
}

// pipefixJob is one job of T and what was fetched about it.
type pipefixJob struct {
	Target pipefix.Target // the recorded failure key the fix is judged against
	Job    glab.Job       // the job as it failed in the request's pipeline
	Slot   bool           // among the first MaxExcerptJobs: gets definition and excerpt

	Definition          string
	DefinitionAvailable bool

	TraceAvailable bool
	trace          *glab.TraceTail // held only until fitPipefixPrompt finishes
	Excerpt        pipelinectx.JobExcerpt
}

// pipefixFile is one cited file and what the prompt says about it.
type pipefixFile struct {
	Path     string
	Editable bool
	Rule     string // why not, as a stable code; "" when Editable
	Why      string // why not, in the prompt's words; "" when Editable

	Bytes  int
	SHA256 string
	blob   []byte // the content read at the base SHA (editable files only)

	Shown    bool   // the content is rendered into the prompt
	NotShown string // why it is not, in the prompt's words
}

// pipefixEditRefusal asks the gate whether an edit to path is in scope for
// this envelope: the refusal effects.PlanFix would give a well-formed edit to
// it on path grounds (SC-FORM, SC-PROT, SC-CLASS, SC-CITED, SC-BEAD), or nil.
// It is the gate's own function on a probe edit, not a second implementation.
func pipefixEditRefusal(env effects.FixEnvelope, path string) *effects.Refusal {
	probe := effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{
		{Kind: effects.KindEdit, Path: path, Old: "a", New: "b"},
	}}
	return effects.AsRefusal(effects.ValidateEditPaths(probe, env))
}

// pipefixWhyNot puts a path refusal into the prompt's words. Generic by rule
// code: it never repeats the refusal's detail.
func pipefixWhyNot(r *effects.Refusal) string {
	switch r.Rule {
	case effects.RuleProtected:
		return "protected: this fix may not change it"
	case effects.RuleClass:
		return fmt.Sprintf("its kind of file (%s) is not one this fix may change", oneLine(r.Sub, 40))
	case effects.RuleBead:
		return "outside the paths this project lets a fix change"
	case effects.RuleCited:
		return "not a file this fix was given"
	default:
		return "its path is not one gonk can change"
	}
}

// buildPipefixContext fetches the context for one fix session. maxEdits is the
// `edit` cap the prompt states (the pack shape's, bounded by
// effects.MaxFixEdits). See the file header for when it errors.
func buildPipefixContext(ctx context.Context, r pipefixContextReader, req pipefixRequest, maxEdits int) (*pipefixContext, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", errPipefixContext, fmt.Sprintf(format, args...))
	}
	if r == nil {
		return nil, bad("no forge reader configured")
	}
	if err := req.Target.Validate(); err != nil {
		return nil, bad("%v", err)
	}
	if !glab.IsCommitSHA(req.BaseSHA) {
		return nil, bad("base %q is not a full commit SHA", req.BaseSHA)
	}
	if !req.Diagnosis.Recorded() {
		return nil, bad("the run has no landed diagnosis on its record")
	}
	fixable := false
	for _, c := range pipefix.FixableClasses() {
		fixable = fixable || c == req.Diagnosis.Class
	}
	if !fixable {
		return nil, bad("a diagnosis classified %q is never fixed", req.Diagnosis.Class)
	}
	if maxEdits < 1 || maxEdits > effects.MaxFixEdits {
		return nil, bad("edit cap %d is outside 1..%d", maxEdits, effects.MaxFixEdits)
	}
	caps := req.Envelope.Scope.Caps
	if !req.Envelope.Scope.Enabled || caps.MaxFiles < 1 || caps.MaxLines < 1 || caps.MaxPatchBytes < 1 || caps.MaxFileBytes < 1 {
		return nil, bad("the dispatch's envelope allows no change (enabled %t, caps %+v)", req.Envelope.Scope.Enabled, caps)
	}

	recorded := make([]pipefix.RecordedJob, 0, len(req.Jobs))
	for _, j := range req.Jobs {
		recorded = append(recorded, pipefix.RecordedJob{
			Name: j.Name, Stage: j.Stage, FailureReason: j.FailureReason, ErrorSignature: j.ErrorSignature,
			AllowFailure: j.AllowFailure, Generic: j.GenericFailure(), FactsRecorded: j.FailureFactsRecorded(),
		})
	}
	targets, problems := pipefix.TargetSet(recorded, req.Diagnosis.EvidenceJobs)
	if len(problems) > 0 {
		return nil, bad("the target set is unusable: %s", strings.Join(problems, "; "))
	}
	if len(targets) == 0 {
		return nil, bad("the diagnosis names no failed job to fix")
	}

	pl, err := r.GetPipeline(ctx, req.ProjectID, req.PipelineID)
	if err != nil {
		return nil, bad("get pipeline %d: %v", req.PipelineID, err)
	}
	if pl.SHA != req.BaseSHA {
		return nil, bad("pipeline %d ran on %s, not on the fix base %s", req.PipelineID, pl.SHA, req.BaseSHA)
	}
	jobs, err := r.ListPipelineJobs(ctx, req.ProjectID, req.PipelineID, "", false)
	if err != nil {
		return nil, bad("list jobs of pipeline %d: %v", req.PipelineID, err)
	}

	pc := &pipefixContext{Req: req, Pipeline: *pl, Limits: beadstore.FixLimits{
		Edits: maxEdits, Files: caps.MaxFiles, Lines: caps.MaxLines, NewBytes: caps.MaxPatchBytes,
	}}
	unavailable := func(block string, err error) {
		pc.Unavailable = append(pc.Unavailable, block+": "+err.Error())
	}

	for i, t := range targets {
		var found []glab.Job
		for _, j := range jobs {
			if jobNameText(j.Name) == t.Name() {
				found = append(found, j)
			}
		}
		switch {
		case len(found) == 0:
			return nil, bad("the diagnosed job %q is not in pipeline %d", t.Name(), req.PipelineID)
		case len(found) > 1:
			return nil, bad("pipeline %d has %d jobs named %q", req.PipelineID, len(found), t.Name())
		case found[0].Status != "failed" || found[0].AllowFailure:
			return nil, bad("the diagnosed job %q is not failing pipeline %d (status %q, allow_failure %t)",
				t.Name(), req.PipelineID, found[0].Status, found[0].AllowFailure)
		}
		pc.Jobs = append(pc.Jobs, &pipefixJob{Target: t, Job: found[0], Slot: i < pipelinectx.MaxExcerptJobs})
	}

	var lint *glab.CILint
	if l, err := r.CILintStatic(ctx, req.ProjectID, req.BaseSHA); err != nil {
		unavailable("ci definition", err)
	} else if reason := lintUnusable(l); reason != "" {
		unavailable("ci definition", errors.New(reason))
	} else {
		lint = l
	}
	for _, fj := range pc.Jobs {
		if !fj.Slot {
			continue
		}
		if lint != nil {
			fj.Definition, fj.DefinitionAvailable = pipelineJobDefinition(lint, fj.Job.Name)
		}
		tt, err := fetchPipelineTrace(ctx, r, req.ProjectID, fj.Job.ID)
		if err != nil {
			unavailable(fmt.Sprintf("log of job %q", fj.Job.Name), err)
			continue
		}
		fj.trace, fj.TraceAvailable = tt, true
	}
	pc.setExcerptCap(pipelinectx.JobBudget(pc.excerptedCount()))

	seen := map[string]bool{}
	shownTotal := 0
	for _, path := range req.Diagnosis.Files {
		if seen[path] {
			continue
		}
		seen[path] = true
		f := &pipefixFile{Path: path}
		pc.Files = append(pc.Files, f)
		if oneLine(path, pipefixMaxPathBytes) != path {
			// The path as rendered would not be the path: the session could
			// not name the file it was told about.
			f.Rule, f.Why = string(effects.RuleForm), "its path cannot be shown exactly"
			continue
		}
		if ref := pipefixEditRefusal(req.Envelope, path); ref != nil {
			f.Rule, f.Why = string(ref.Rule), pipefixWhyNot(ref)
			continue
		}
		blob, err := r.GetRawFile(ctx, req.ProjectID, path, req.BaseSHA, caps.MaxFileBytes)
		switch {
		case glab.IsNotFound(err):
			f.Rule, f.Why = string(effects.RuleEditMissing), "it does not exist at this commit"
			continue
		case errors.Is(err, glab.ErrResponseTooLarge):
			f.Rule, f.Why = string(effects.RuleSize), "it is larger than a fix may change"
			continue
		case err != nil:
			return nil, bad("read %q at %s: %v", path, req.BaseSHA, err)
		}
		f.Bytes = len(blob)
		sum := sha256.Sum256(blob)
		f.SHA256 = hex.EncodeToString(sum[:])
		switch {
		case len(blob) == 0:
			f.Rule, f.Why = string(effects.RuleEditNoMatch), "it is empty"
		case !utf8.Valid(blob) || strings.ContainsRune(string(blob), 0):
			f.Rule, f.Why = string(effects.RuleEditEncoding), "it is not a text file"
		case scope.IsGenerated(blob) && !req.Envelope.Scope.Allows(scope.ClassGenerated, req.Envelope.Lift):
			f.Rule, f.Why = string(effects.RuleClass), "it is produced by a tool, not written by hand"
		default:
			f.Editable, f.blob = true, blob
		}
		if !f.Editable {
			continue
		}
		switch _, redacted := pipelinectx.Redact(string(blob)); {
		case len(redacted) > 0:
			f.NotShown = "it holds text gonk does not copy into a message"
		case strings.IndexFunc(string(blob), func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' }) >= 0:
			f.NotShown = "it holds characters that cannot be shown here"
		case len(blob) > pipefixShowFileBytes || shownTotal+len(blob) > pipefixShowTotalBytes:
			f.NotShown = "it is too long to show here"
		default:
			f.Shown = true
			shownTotal += len(blob)
		}
	}
	if len(pc.editable()) == 0 {
		why := make([]string, 0, len(pc.Files))
		for _, f := range pc.Files {
			why = append(why, fmt.Sprintf("%q: %s", f.Path, f.Rule))
		}
		return nil, fmt.Errorf("%w: %w (%s)", errPipefixContext, errPipefixNothingEditable, strings.Join(why, "; "))
	}
	return pc, nil
}

// editable lists the files the session may change, in cited order.
func (pc *pipefixContext) editable() []*pipefixFile {
	var out []*pipefixFile
	for _, f := range pc.Files {
		if f.Editable {
			out = append(out, f)
		}
	}
	return out
}

// excerptedCount is how many jobs share the excerpt budget.
func (pc *pipefixContext) excerptedCount() int {
	n := 0
	for _, fj := range pc.Jobs {
		if fj.Slot && fj.TraceAvailable {
			n++
		}
	}
	return n
}

// setExcerptCap (re)builds every excerpt at one per-job cap, through the
// pipeline agent's own options function.
func (pc *pipefixContext) setExcerptCap(maxBytes int) {
	pc.ExcerptMaxBytes = maxBytes
	for _, fj := range pc.Jobs {
		if fj.Slot && fj.TraceAvailable && fj.trace != nil {
			fj.Excerpt = pipelinectx.ExcerptWith(fj.trace.Data, pipelineExcerptOptions(fj.trace, maxBytes))
		}
	}
}

// release drops the raw logs and blobs once the prompt is final.
func (pc *pipefixContext) release() {
	for _, fj := range pc.Jobs {
		fj.trace = nil
	}
	for _, f := range pc.Files {
		f.blob = nil
	}
}

// pipefixDiagnosisHash is the hash of a landed diagnosis as a fix session is
// shown it: every field, in a fixed encoding.
func pipefixDiagnosisHash(d beadstore.PipelineDiagnosis) string {
	raw, err := json.Marshal(struct {
		Class        string
		Files        []string
		EvidenceJobs []string
		Summary      string
		Remedy       string
	}{d.Class, append([]string{}, d.Files...), append([]string{}, d.EvidenceJobs...), d.Summary, d.Remedy})
	if err != nil {
		panic(err) // strings and string slices always marshal
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Facts is what the controller records about this context (plan 5.3 A9):
// everything verifyPipefixContext needs to re-derive what the session was
// shown. prompt is the final rendered prompt.
func (pc *pipefixContext) Facts(prompt string) beadstore.FixContext {
	psum := sha256.Sum256([]byte(prompt))
	fc := beadstore.FixContext{
		BaseSHA: pc.Req.BaseSHA, PipelineID: pc.Req.PipelineID,
		DiagnosisSHA256: pipefixDiagnosisHash(pc.Req.Diagnosis),
		ExcerptVersion:  pipelinectx.ExcerptVersion, ExcerptMaxBytes: pc.ExcerptMaxBytes,
		Limits: pc.Limits, PromptSHA256: hex.EncodeToString(psum[:]),
	}
	for _, fj := range pc.Jobs {
		j := beadstore.PipelineJob{
			ID: fj.Job.ID, Name: jobNameText(fj.Job.Name),
			Stage: fj.Job.Stage, FailureReason: fj.Job.FailureReason, AllowFailure: fj.Job.AllowFailure,
			SignatureVersion: pipelinectx.SignatureVersion,
		}
		firstError := ""
		if fj.Slot && fj.TraceAvailable {
			j.Excerpted, j.ExcerptSHA256, j.Groundable = true, fj.Excerpt.SHA256, fj.Excerpt.Groundable()
			firstError, j.ErrorSignature = fj.Excerpt.FirstError, fj.Excerpt.ErrorSignature
		}
		j.ErrorGeneric = pipelinectx.GenericFirstError(firstError)
		fc.Jobs = append(fc.Jobs, j)
	}
	for _, f := range pc.Files {
		fc.Files = append(fc.Files, beadstore.FixContextFile{
			Path: f.Path, Editable: f.Editable, Rule: f.Rule, Bytes: f.Bytes, SHA256: f.SHA256, Shown: f.Shown,
		})
	}
	return fc
}

// verifyPipefixContext is plan 5.3 gate A9 for a fix session: what the session
// was shown still hashes to the recorded context. It re-reads the diagnosis
// (from the record the caller passes), each excerpted job's log -- through the
// same fetch function and the recorded cap -- and each editable file's blob
// at the recorded base SHA, and checks that the limits the prompt stated are
// the ones the envelope about to judge the answer carries.
//
// On success it returns the editable files' blobs: the effects.BaseBlobs the
// answer's edits are applied to, PROVEN to be the bytes the session was shown.
// (A blob says nothing of its tree entry's mode; refusing a path that is not
// a regular file stays the apply's obligation -- see effects.BaseBlobs.)
//
// Every error is OURS, never the agent's (the outcome is UNKNOWN, not
// gate-failed): errPipefixContextChanged for a mismatch, a plain error for a
// failed read.
func verifyPipefixContext(ctx context.Context, r pipefixContextReader, projectID int64, fc beadstore.FixContext,
	diag beadstore.PipelineDiagnosis, env effects.FixEnvelope, maxEdits int) (effects.BaseBlobs, error) {
	changed := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", errPipefixContextChanged, fmt.Sprintf(format, args...))
	}
	if r == nil {
		return nil, fmt.Errorf("%w: no forge reader configured", errPipefixContext)
	}
	if !fc.Recorded() {
		return nil, changed("no fix context is recorded")
	}
	if got := pipefixDiagnosisHash(diag); got != fc.DiagnosisSHA256 {
		return nil, changed("the diagnosis on the record is not the one the session was shown")
	}
	caps := env.Scope.Caps
	if want := (beadstore.FixLimits{Edits: maxEdits, Files: caps.MaxFiles, Lines: caps.MaxLines, NewBytes: caps.MaxPatchBytes}); fc.Limits != want {
		return nil, changed("the session was told limits %+v, the envelope judging it has %+v", fc.Limits, want)
	}
	for _, j := range fc.Jobs {
		if !j.Excerpted {
			continue
		}
		tt, err := fetchPipelineTrace(ctx, r, projectID, j.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: re-read the log of job %q: %v", errPipefixContext, j.Name, err)
		}
		if _, err := pipelinectx.VerifyExcerpt(tt.Data, pipelineExcerptOptions(tt, fc.ExcerptMaxBytes), fc.ExcerptVersion, j.ExcerptSHA256); err != nil {
			return nil, changed("job %q: %v", j.Name, err)
		}
	}
	blobs := effects.BaseBlobs{}
	for _, f := range fc.Files {
		if !f.Editable {
			continue
		}
		if ref := pipefixEditRefusal(env, f.Path); ref != nil {
			return nil, changed("%q was shown as editable; the envelope judging the answer refuses it (%s)", f.Path, ref.Code())
		}
		blob, err := r.GetRawFile(ctx, projectID, f.Path, fc.BaseSHA, caps.MaxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: re-read %q at %s: %v", errPipefixContext, f.Path, fc.BaseSHA, err)
		}
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, changed("%q at %s is not the content the session was shown", f.Path, fc.BaseSHA)
		}
		blobs[f.Path] = blob
	}
	return blobs, nil
}
