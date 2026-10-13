package effects

// The `diagnosis` effect: a failed pipeline's typed answer (gonk-tios D7a/D8).
//
// WHAT IS HERE AND WHAT IS NOT. ValidateDiagnosis is D8 step 4: the SHAPE of
// the answer -- the class is one of a closed set, the prose fields are present
// and bounded, every evidence item names a failed job this session was handed
// and either quotes a log line or cites a fact from a closed key set, and the
// files are clean relative paths. It is pure and needs only the names of the
// failed jobs and whether each had a log excerpt (DiagnosisContext).
//
// It deliberately does NOT check that a quoted `line` actually occurs in the
// job's log. That is the hallucination check (D8 step 6), and it lives in
// pkg/pipelinectx as `Grounded(excerpt, line) bool`, because it needs the
// stored excerpt text and its normalisation rules, which belong to the package
// that builds the excerpt. The seam is this: after ValidateDiagnosis passes,
// the caller (gonk-gate's applyPipelineBatch) walks DiagnosisLines(b) and asks
// pipelinectx.Grounded of each (job, line) against THAT job's stored excerpt.
// Nothing here duplicates that rule -- not even the 12-rune minimum, which is
// only meaningful against an excerpt ("or equals a whole excerpt line").
//
// The classification is a LABEL, never a policy input: nothing retries,
// cancels or skips on it (gonk-066 5.1). There is no retry/cancel effect kind,
// and adding one is not a matter of adding a class here.

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// Classification is a diagnosis's class: where the remedy belongs.
type Classification string

const (
	// ClassCode: the product code at this commit is wrong.
	ClassCode Classification = "code"
	// ClassTest: tests or fixtures are wrong while the product behaves as intended.
	ClassTest Classification = "test"
	// ClassFlaky: nondeterministic -- the same job/commit both passes and fails.
	ClassFlaky Classification = "flaky"
	// ClassInfrastructure: the execution environment the job ran in failed or
	// lacked something.
	ClassInfrastructure Classification = "infrastructure"
	// ClassConfiguration: the pipeline definition or the project's CI settings
	// are wrong.
	ClassConfiguration Classification = "configuration"
	// ClassUnknown: could not determine; the summary says what was checked. A
	// correct outcome, not a failure.
	ClassUnknown Classification = "unknown"
)

// classificationOrder is the closed set in its documented order. It is the
// ONE enumeration: knownClassifications and Classifications() are built from
// it, so a label set or prompt derived from Classifications() cannot miss a
// class.
var classificationOrder = []Classification{
	ClassCode, ClassTest, ClassFlaky, ClassInfrastructure, ClassConfiguration, ClassUnknown,
}

var knownClassifications = func() map[Classification]bool {
	m := make(map[Classification]bool, len(classificationOrder))
	for _, c := range classificationOrder {
		m[c] = true
	}
	return m
}()

// Classifications returns the closed class set in documented order (a copy).
func Classifications() []Classification {
	return append([]Classification(nil), classificationOrder...)
}

// classesRequiringFiles are the classes that make a claim about repository
// content, so they must say where (D8 step 4).
var classesRequiringFiles = map[Classification]bool{
	ClassCode: true, ClassTest: true, ClassConfiguration: true,
}

// EvidenceFact is the closed key set an evidence item may cite instead of
// quoting a log line: each names a fact the controller itself injected.
type EvidenceFact string

const (
	FactFailureReason EvidenceFact = "failure_reason"
	FactRunner        EvidenceFact = "runner"
	FactTags          EvidenceFact = "tags"
	FactStage         EvidenceFact = "stage"
	FactAllowFailure  EvidenceFact = "allow_failure"
	FactDuration      EvidenceFact = "duration"
	FactHistory       EvidenceFact = "history"
	FactDefinition    EvidenceFact = "definition"
	// FactPipelineError is pipeline-level (YAML error, a pipeline that failed
	// with zero jobs), so it is the one fact that may omit `job`.
	FactPipelineError EvidenceFact = "pipeline_error"
)

// factOrder is the closed evidence-fact key set in its documented order. It
// is the ONE enumeration: knownFacts (what validateDiagnosis accepts) and
// EvidenceFacts() (what a prompt and a refusal's feedback list) are both
// built from it, so the two cannot drift.
var factOrder = []EvidenceFact{
	FactFailureReason, FactRunner, FactTags, FactStage, FactAllowFailure,
	FactDuration, FactHistory, FactDefinition, FactPipelineError,
}

var knownFacts = func() map[EvidenceFact]bool {
	m := make(map[EvidenceFact]bool, len(factOrder))
	for _, f := range factOrder {
		m[f] = true
	}
	return m
}()

// EvidenceFacts returns the closed evidence-fact key set in documented order
// (a copy).
func EvidenceFacts() []EvidenceFact { return append([]EvidenceFact(nil), factOrder...) }

// ClassesRequiringFiles returns the classes whose diagnosis must cite at
// least one file, in Classifications() order: the set rule:files-required
// reads.
func ClassesRequiringFiles() []Classification {
	var out []Classification
	for _, c := range classificationOrder {
		if classesRequiringFiles[c] {
			out = append(out, c)
		}
	}
	return out
}

// itemError marks WHICH evidence item or file a diagnosis rule refused, for
// the retry feedback (feedback.go). Its text and its chain are those of the
// error it wraps, so errors.Is and every message are unchanged.
type itemError struct {
	err  error
	item int
}

func (e *itemError) Error() string { return e.err.Error() }
func (e *itemError) Unwrap() error { return e.err }

func atItem(k int, err error) error { return &itemError{err: err, item: k} }

// EvidenceItem is one piece of support for a diagnosis: EXACTLY ONE of Line (a
// verbatim quote from Job's log excerpt, grounded by pkg/pipelinectx) or Fact
// (a key from the closed set naming an injected fact about Job).
type EvidenceItem struct {
	Job  string       `json:"job,omitempty"`
	Line string       `json:"line,omitempty"`
	Fact EvidenceFact `json:"fact,omitempty"`
}

// Caps (D8 step 4). Summary/remedy are measured in characters (runes), as the
// plan states them, not bytes; the rendered landing note is separately bounded
// by ValidateComments.
const (
	MaxDiagnosisSummaryChars = 1500
	MaxDiagnosisRemedyChars  = 1500
	MinDiagnosisEvidence     = 1
	MaxDiagnosisEvidence     = 8
	MaxDiagnosisFiles        = 10
)

// Rule errors. ValidateDiagnosis wraps exactly one of these, so a caller (and
// a test) can tell WHICH rule refused the batch with errors.Is.
var (
	ErrDiagnosisForeignField   = errors.New("diagnosis carries another kind's field")
	ErrDiagnosisClassification = errors.New("classification not in the closed set")
	ErrDiagnosisSummaryEmpty   = errors.New("summary is empty")
	ErrDiagnosisSummaryTooLong = errors.New("summary over cap")
	ErrDiagnosisRemedyEmpty    = errors.New("remedy is empty")
	ErrDiagnosisRemedyTooLong  = errors.New("remedy over cap")
	ErrDiagnosisEvidenceCount  = errors.New("evidence count out of range")
	ErrDiagnosisEvidenceShape  = errors.New("evidence item must carry exactly one non-blank line or fact")
	ErrDiagnosisEvidenceFact   = errors.New("evidence fact not in the closed key set")
	ErrDiagnosisEvidenceJob    = errors.New("evidence job is not a failed job of this pipeline")
	ErrDiagnosisNoLineEvidence = errors.New("no line evidence although a failed job has a log excerpt")
	ErrDiagnosisFileCount      = errors.New("too many files")
	ErrDiagnosisFilePath       = errors.New("file is not a clean repository-relative path")
	ErrDiagnosisFilesRequired  = errors.New("classification requires at least one file")
)

// FailedJob is what the controller recorded at dispatch about one failed job
// of the diagnosed pipeline (rec.PipelineJobs): its name, and whether its log
// excerpt was non-empty.
type FailedJob struct {
	Name       string
	HasExcerpt bool
}

// DiagnosisContext is the injected context ValidateDiagnosis checks against.
// Build it ONLY from the record written at dispatch, never from the batch.
type DiagnosisContext struct {
	FailedJobs []FailedJob
}

// JobNames is the closed set of names an evidence item's `job` may hold, in
// the context's order (a copy). It is the ONE enumeration: ValidateDiagnosis
// builds the set rule:evidence-job reads from it, and a refusal's feedback
// lists it (gonk-37kg.1), so what the gate accepts and what the next attempt
// is told cannot drift.
func (c DiagnosisContext) JobNames() []string {
	out := make([]string, 0, len(c.FailedJobs))
	for _, j := range c.FailedJobs {
		out = append(out, j.Name) // jobs:names
	}
	return out
}

// ValidateDiagnosis enforces the diagnosis shape rules (D8 step 4) on every
// diagnosis effect in b. It is a no-op for a batch with none -- cardinality
// ("exactly one") is the shape gate's job (Validate), as for every kind.
func ValidateDiagnosis(b Batch, ctx DiagnosisContext) error {
	jobs := make(map[string]bool, len(ctx.FailedJobs))
	anyExcerpt := false
	for _, name := range ctx.JobNames() { // rule:evidence-job reads THIS set
		jobs[name] = true
	}
	for _, j := range ctx.FailedJobs {
		anyExcerpt = anyExcerpt || j.HasExcerpt
	}
	for i, e := range b.Effects {
		if e.Kind != KindDiagnosis {
			continue
		}
		if err := validateDiagnosis(e, jobs, anyExcerpt); err != nil {
			return fmt.Errorf("effects: effect %d (diagnosis): %w", i, err)
		}
	}
	return nil
}

func validateDiagnosis(e Effect, jobs map[string]bool, anyExcerpt bool) error {
	// A diagnosis carries only its own fields. Above all no path/content: its
	// files are citations the denylist deliberately does not check
	// (citationOnlyPathFields), so a write shape must not ride along on it.
	//
	// By REFLECTION over diagnosisFields (gonk-8yux), as the fix kinds' rule
	// is over fixKindFields: until 2026-10-12 this named six fields by hand,
	// and the fix vocabulary added to Effect afterwards (old, new, paths,
	// reason) rode on a diagnosis unread. A field added to Effect later is
	// now foreign here until someone lists it.
	if f := foreignTo(e, diagnosisFields); f != "" { // rule:foreign-field
		return fmt.Errorf("%w: %s", ErrDiagnosisForeignField, f)
	}
	if !knownClassifications[e.Classification] { // rule:classification
		return fmt.Errorf("%w: %q", ErrDiagnosisClassification, e.Classification)
	}
	if strings.TrimSpace(e.Summary) == "" { // rule:summary-empty
		return ErrDiagnosisSummaryEmpty
	}
	if n := utf8.RuneCountInString(e.Summary); n > MaxDiagnosisSummaryChars { // rule:summary-cap
		return fmt.Errorf("%w: %d chars, max %d", ErrDiagnosisSummaryTooLong, n, MaxDiagnosisSummaryChars)
	}
	if strings.TrimSpace(e.Remedy) == "" { // rule:remedy-empty
		return ErrDiagnosisRemedyEmpty
	}
	if n := utf8.RuneCountInString(e.Remedy); n > MaxDiagnosisRemedyChars { // rule:remedy-cap
		return fmt.Errorf("%w: %d chars, max %d", ErrDiagnosisRemedyTooLong, n, MaxDiagnosisRemedyChars)
	}
	if n := len(e.Evidence); n < MinDiagnosisEvidence || n > MaxDiagnosisEvidence { // rule:evidence-count
		return fmt.Errorf("%w: %d items, want %d..%d", ErrDiagnosisEvidenceCount, n, MinDiagnosisEvidence, MaxDiagnosisEvidence)
	}
	sawLine := false
	for k, ev := range e.Evidence {
		hasLine, hasFact := strings.TrimSpace(ev.Line) != "", ev.Fact != ""
		blankLine := ev.Line != "" && !hasLine
		if blankLine || hasLine == hasFact { // rule:evidence-shape
			return atItem(k, fmt.Errorf("%w (item %d)", ErrDiagnosisEvidenceShape, k))
		}
		if hasFact && !knownFacts[ev.Fact] { // rule:evidence-fact
			return atItem(k, fmt.Errorf("%w (item %d): %q", ErrDiagnosisEvidenceFact, k, ev.Fact))
		}
		pipelineLevel := hasFact && ev.Fact == FactPipelineError && ev.Job == ""
		if !pipelineLevel && !jobs[ev.Job] { // rule:evidence-job
			return atItem(k, fmt.Errorf("%w (item %d): %q", ErrDiagnosisEvidenceJob, k, ev.Job))
		}
		sawLine = sawLine || hasLine
	}
	if anyExcerpt && !sawLine { // rule:line-required
		return ErrDiagnosisNoLineEvidence
	}
	if n := len(e.Files); n > MaxDiagnosisFiles { // rule:file-count
		return fmt.Errorf("%w: %d, max %d", ErrDiagnosisFileCount, n, MaxDiagnosisFiles)
	}
	for k, f := range e.Files {
		if !cleanRelativePath(f) { // rule:file-path
			return atItem(k, fmt.Errorf("%w (file %d): %q", ErrDiagnosisFilePath, k, f))
		}
	}
	if classesRequiringFiles[e.Classification] && len(e.Files) == 0 { // rule:files-required
		return fmt.Errorf("%w: %q", ErrDiagnosisFilesRequired, e.Classification)
	}
	return nil
}

// cleanRelativePath: non-empty, no NUL, not absolute, no `..` segment, and
// already in canonical form (so the path checked is the path a human reads
// and the controller fetches: "a/./b" and "a//b" are refused, not repaired).
func cleanRelativePath(p string) bool {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return path.Clean(p) == p && p != "."
}

// DiagnosisLine is one quoted log line of a diagnosis, for the grounding step.
type DiagnosisLine struct {
	Effect, Item int
	Job, Line    string
}

// DiagnosisLines returns every `line` evidence item of every diagnosis effect
// in b, in order. It is the seam to pkg/pipelinectx: the caller checks each
// with pipelinectx.Grounded against Job's stored excerpt (D8 step 6). Call it
// only after ValidateDiagnosis has passed.
func DiagnosisLines(b Batch) []DiagnosisLine {
	var out []DiagnosisLine
	for i, e := range b.Effects {
		if e.Kind != KindDiagnosis {
			continue
		}
		for k, ev := range e.Evidence {
			if ev.Line != "" {
				out = append(out, DiagnosisLine{Effect: i, Item: k, Job: ev.Job, Line: ev.Line})
			}
		}
	}
	return out
}
