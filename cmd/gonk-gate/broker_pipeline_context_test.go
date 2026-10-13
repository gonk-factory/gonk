package main

// Tests for the pipeline agent's controller-side context (gonk-tios P6).
//
// EVERY FIXTURE HERE IS SYNTHETIC AND GENERIC (plan section 0, the held-out
// rule): one failing unit test in a made-up project, made-up runner and job
// names, made-up YAML. Nothing is copied from a real job log, and no fixture
// hints at any particular failure cause. The owner reviews this set for
// leakage before merge.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

const (
	plSHA      = "1111111111111111111111111111111111111111"
	plSHAOld   = "2222222222222222222222222222222222222222"
	plSHAGreen = "3333333333333333333333333333333333333333"
)

// plTestLog is the synthetic failing-unit-test log every scenario uses.
const plTestLog = "$ ./run-tests\n" +
	"collecting tests\n" +
	"test_addition ... ok\n" +
	"test_sum ... FAILED\n" +
	"AssertionError: sum([1, 2]) returned 3, expected 4\n" +
	"1 failed, 1 passed\n" +
	"ERROR: Job failed: exit code 1\n"

const plMergedYAML = `stages:
- build
- test
default:
  image: example/base:1
.test-template:
  stage: test
  before_script:
  - ./prepare
build:
  stage: build
  script:
  - ./build
unit-tests:
  extends: .test-template
  script:
  - ./run-tests
`

func plTime(min int) *time.Time {
	t := time.Date(2026, 3, 1, 12, min, 0, 0, time.UTC)
	return &t
}

// fenceOpenRe matches a real untrusted-block opening line (not the prompt's
// own description of one, which has "<id>" where the hex goes).
var fenceOpenRe = regexp.MustCompile(`(?m)^<<<UNTRUSTED [0-9a-f]{12} .*$`)

func plDur(s float64) *float64 { return &s }

// plScenario is a fake GitLab seeded with one failed pipeline (id 900) on
// main of project "example/calc", its predecessors 880 (success), 890
// (failed), 895 (canceled), and the commit, lint reply and log of each job.
type plScenario struct {
	gl   *glabtest.Server
	proj *glabtest.Project
	pid  int64
	req  pipelineContextRequest
}

func newPLScenario(t *testing.T) *plScenario {
	t.Helper()
	gl := glabtest.New(t)
	p := gl.AddProject("example/calc", glab.AccessMaintainer)
	runnerA := &glab.Runner{ID: 12, Description: "runner-a", RunnerType: "instance_type", IsShared: true}
	runnerB := &glab.Runner{ID: 13, Description: "runner-b", RunnerType: "instance_type", IsShared: true}

	gl.AddPipeline(p.ID, glab.Pipeline{ID: 880, CreatedAt: *plTime(0), IID: 38, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
	gl.AddPipeline(p.ID, glab.Pipeline{ID: 890, CreatedAt: *plTime(9), IID: 39, Ref: "main", SHA: plSHAOld, Status: "failed", Source: "push"})
	gl.AddPipeline(p.ID, glab.Pipeline{ID: 895, CreatedAt: *plTime(15), IID: 40, Ref: "main", SHA: plSHAOld, Status: "canceled", Source: "push"})
	gl.AddPipeline(p.ID, glab.Pipeline{ID: 900, CreatedAt: *plTime(19), IID: 41, Ref: "main", SHA: plSHA, Status: "failed", Source: "push",
		FinishedAt: plTime(30)})

	gl.AddJob(p.ID, 880, glab.Job{ID: 8801, Name: "build", Stage: "build", Status: "success", Runner: runnerB})
	gl.AddJob(p.ID, 880, glab.Job{ID: 8802, Name: "unit-tests", Stage: "test", Status: "success", Runner: runnerB, FinishedAt: plTime(1)})
	gl.AddJob(p.ID, 890, glab.Job{ID: 8901, Name: "build", Stage: "build", Status: "success", Runner: runnerA})
	gl.AddJob(p.ID, 890, glab.Job{ID: 8902, Name: "unit-tests", Stage: "test", Status: "failed", Runner: runnerA, FinishedAt: plTime(10)})
	gl.AddJob(p.ID, 900, glab.Job{ID: 9001, Name: "build", Stage: "build", Status: "success", Runner: runnerA})
	gl.AddJob(p.ID, 900, glab.Job{ID: 9002, Name: "unit-tests", Stage: "test", Status: "failed", FailureReason: "script_failure",
		Runner: runnerA, TagList: []string{"small"}, Duration: plDur(42), StartedAt: plTime(20), FinishedAt: plTime(21)})
	gl.AddJob(p.ID, 900, glab.Job{ID: 9003, Name: "lint-docs", Stage: "test", Status: "failed", AllowFailure: true,
		FailureReason: "script_failure", Runner: runnerA})
	gl.SetTrace(p.ID, 9002, []byte(plTestLog))

	gl.AddCommit(p.ID, glab.Commit{ID: plSHA, Title: "Add sum helper", AuthorName: "Example Author",
		AuthoredDate: plTime(0), Message: "Add sum helper\n\nAdds sum() for lists of numbers.\n"})
	gl.SetCILint(p.ID, plSHA, glab.CILint{Valid: true, MergedYAML: plMergedYAML})

	return &plScenario{gl: gl, proj: p, pid: p.ID, req: pipelineContextRequest{
		ProjectID: p.ID, Project: "example/calc", PipelineID: 900,
		Target: pipelinectx.RefTarget("main"), LastSuccess: 880,
	}}
}

func (s *plScenario) build(t *testing.T) *pipelineContext {
	t.Helper()
	pc, err := buildPipelineContext(context.Background(), s.gl.Client(), s.req)
	if err != nil {
		t.Fatalf("buildPipelineContext: %v", err)
	}
	return pc
}

func mustFit(t *testing.T, pc *pipelineContext, sha string) string {
	t.Helper()
	p, err := fitPipelinePrompt(pc, sha)
	if err != nil {
		t.Fatalf("fitPipelinePrompt: %v", err)
	}
	return p
}

// Every fact block D6 names reaches the prompt, from the forge, unaltered.
func TestPipelineContextCarriesEveryFactBlock(t *testing.T) {
	s := newPLScenario(t)
	pc := s.build(t)
	p := mustFit(t, pc, plSHA)

	if len(pc.Unavailable) != 0 {
		t.Errorf("Unavailable = %v, want none in the full scenario", pc.Unavailable)
	}
	for _, want := range []string{
		// pipeline
		"pipeline 900 in project `example/calc`", "id: 900 (number 41 in this project)", "target: branch main",
		"ref: main", "commit: " + plSHA, "source: push", "status: failed", "finished: 2026-03-01T12:30:00Z",
		"pipeline_error: none",
		// commit
		"title: Add sum helper", "author: Example Author", "Adds sum() for lists of numbers.",
		// run facts
		"first failed pipeline since the last success: 890 at commit 222222222222, created 2026-03-01T12:09:00Z",
		"failed pipelines since the last success: 2", "last success: 880 at commit 333333333333",
		// failed job facts, incl. runner identity and tags from the JOB object
		`--- failed job 1 of 1: "unit-tests" ---`, "stage: test", "failure_reason: script_failure",
		"allow_failure: false", "duration: 42s (started 2026-03-01T12:20:00Z, finished 2026-03-01T12:21:00Z)",
		"tags: small", `runner: id 12, description "runner-a", type instance_type, shared`,
		// history: 895 (canceled) is passed over
		"history (the same job name in earlier finished pipelines on this target, newest first, up to 5):",
		`pipeline 890: failed, runner "runner-a", finished 2026-03-01T12:10:00Z`,
		`pipeline 880: success, runner "runner-b", finished 2026-03-01T12:01:00Z`,
		// definition incl. the inherited template and the default image
		"unit-tests:\n  extends: .test-template", ".test-template:\n  stage: test", "default:\n  image: example/base:1",
		// excerpt
		"AssertionError: sum([1, 2]) returned 3, expected 4",
		// other jobs, incl. the allowed-to-fail one
		`- "build" (stage build): success`, `- "lint-docs" (stage test): failed (allowed to fail)`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	if strings.Contains(p, "pipeline 895") {
		t.Error("history lists canceled pipeline 895 -- only success/failed say anything about health")
	}
	if got := pc.JobRecords(); len(got) != 1 || got[0].Name != "unit-tests" || got[0].ID != 9002 || !got[0].Excerpted || !got[0].Groundable {
		t.Errorf("JobRecords = %+v, want one excerpted, groundable unit-tests (9002); lint-docs is allowed to fail", got)
	}
}

// A line the model copies from the prompt's excerpt is grounded against the
// excerpt the record keeps, and the dispatch-side DiagnosisContext accepts a
// batch citing it.
func TestPipelineContextExcerptGroundsAQuotedLine(t *testing.T) {
	pc := newPLScenario(t).build(t)
	mustFit(t, pc, plSHA)
	line := "AssertionError: sum([1, 2]) returned 3, expected 4"
	fj := pc.Failed[0]
	if !pipelinectx.Grounded(fj.Excerpt.Text, line) {
		t.Fatalf("a line shown in the prompt is not grounded in the recorded excerpt:\n%s", fj.Excerpt.Text)
	}
	b := effects.Batch{Effects: []effects.Effect{{Kind: effects.KindDiagnosis, Classification: effects.ClassTest,
		Summary: "s", Remedy: "r", Files: []string{"tests/test_sum.py"},
		Evidence: []effects.EvidenceItem{{Job: "unit-tests", Line: line}, {Job: "unit-tests", Fact: effects.FactRunner}}}}}
	if err := effects.ValidateDiagnosis(b, pc.DiagnosisContext()); err != nil {
		t.Errorf("ValidateDiagnosis against the dispatch context: %v", err)
	}
}

// Each job's excerpt is built from ITS OWN log, and the hash recorded for it
// is that excerpt's.
func TestPipelineExcerptComesFromItsOwnJob(t *testing.T) {
	s := newPLScenario(t)
	s.gl.AddJob(s.pid, 900, glab.Job{ID: 9004, Name: "unit-tests-b", Stage: "test", Status: "failed", FailureReason: "script_failure"})
	s.gl.SetTrace(s.pid, 9004, []byte("$ ./run-tests --part b\ntest_other ... FAILED\nMARKER-ONLY-IN-JOB-B\n"))
	pc := s.build(t)
	p := mustFit(t, pc, plSHA)

	byName := map[string]*pipelineFailedJob{}
	for _, fj := range pc.Failed {
		byName[fj.Job.Name] = fj
	}
	a, b := byName["unit-tests"], byName["unit-tests-b"]
	if a == nil || b == nil {
		t.Fatalf("failed jobs = %v", pc.Failed)
	}
	if strings.Contains(a.Excerpt.Text, "MARKER-ONLY-IN-JOB-B") || !strings.Contains(a.Excerpt.Text, "test_sum ... FAILED") {
		t.Errorf("unit-tests excerpt is not its own log:\n%s", a.Excerpt.Text)
	}
	if !strings.Contains(b.Excerpt.Text, "MARKER-ONLY-IN-JOB-B") || strings.Contains(b.Excerpt.Text, "test_sum") {
		t.Errorf("unit-tests-b excerpt is not its own log:\n%s", b.Excerpt.Text)
	}
	// In the prompt, B's marker sits inside B's fenced block.
	blk := fencedBlock(t, p, `log of "unit-tests-b"`)
	if !strings.Contains(blk, "MARKER-ONLY-IN-JOB-B") {
		t.Errorf("job B's marker is not inside job B's log block:\n%s", blk)
	}
	if strings.Contains(fencedBlock(t, p, `log of "unit-tests"`), "MARKER-ONLY-IN-JOB-B") {
		t.Error("job B's log line appears inside job A's log block")
	}
	for _, r := range pc.JobRecords() {
		want := byName[r.Name].Excerpt.SHA256
		if r.ExcerptSHA256 != want {
			t.Errorf("record for %s carries hash %s, its excerpt hashes to %s", r.Name, r.ExcerptSHA256, want)
		}
	}
	// Ordering: stage order (build's stage first, by lowest job id), then name.
	if pc.Failed[0].Job.Name != "unit-tests" || pc.Failed[1].Job.Name != "unit-tests-b" {
		t.Errorf("failed job order = %s, %s", pc.Failed[0].Job.Name, pc.Failed[1].Job.Name)
	}
}

// fencedBlock returns the body of the untrusted block whose label is label.
func fencedBlock(t *testing.T, prompt, label string) string {
	t.Helper()
	for _, line := range strings.Split(prompt, "\n") {
		if !strings.HasPrefix(line, untrustedOpen) || !strings.HasSuffix(line, " "+label) {
			continue
		}
		id := strings.Fields(strings.TrimPrefix(line, untrustedOpen))[0]
		open := line + "\n"
		start := strings.Index(prompt, open) + len(open)
		end := strings.Index(prompt[start:], "\n"+untrustedClose+id+untrustedEnd+"\n")
		if end < 0 {
			t.Fatalf("block %q (id %s) is never closed", label, id)
		}
		return prompt[start : start+end+1]
	}
	t.Fatalf("no untrusted block labelled %q in the prompt", label)
	return ""
}

// The per-job cap is honoured, recorded, and enough for the sweep to
// re-derive the identical excerpt from a fresh fetch (D8 step 5).
func TestPipelineExcerptCapIsHonouredAndReproducible(t *testing.T) {
	s := newPLScenario(t)
	var big strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&big, "step %04d of the synthetic suite completed normally\n", i)
	}
	big.WriteString(plTestLog)
	s.gl.SetTrace(s.pid, 9002, []byte(big.String()))
	for _, id := range []int64{9004, 9005} {
		s.gl.AddJob(s.pid, 900, glab.Job{ID: id, Name: fmt.Sprintf("unit-tests-%d", id), Stage: "test", Status: "failed"})
		s.gl.SetTrace(s.pid, id, []byte(big.String()))
	}
	s.gl.TraceIgnoresRange = true

	pc := s.build(t)
	mustFit(t, pc, plSHA)
	// Three jobs share the 12 KiB: 4 KiB each, below the 6 KiB single-job cap.
	if want := pipelinectx.JobBudget(3); pc.ExcerptMaxBytes != want || want >= pipelinectx.MaxJobExcerptBytes {
		t.Errorf("ExcerptMaxBytes = %d, want JobBudget(3) = %d (the prompt fits without shrinking)", pc.ExcerptMaxBytes, want)
	}
	for _, fj := range pc.Failed {
		if n := len(fj.Excerpt.Text); n > pc.ExcerptMaxBytes {
			t.Errorf("%s excerpt is %d bytes, over the recorded cap %d", fj.Job.Name, n, pc.ExcerptMaxBytes)
		}
	}
	// The sweep's path: fetch again with the same constants, rebuild at the
	// RECORDED cap, compare with the RECORDED hash.
	for _, r := range pc.JobRecords() {
		tt, err := fetchPipelineTrace(context.Background(), s.gl.Client(), s.pid, r.ID)
		if err != nil {
			t.Fatalf("refetch %s: %v", r.Name, err)
		}
		if _, err := pipelinectx.VerifyExcerpt(tt.Data, pipelineExcerptOptions(tt, pc.ExcerptMaxBytes),
			pipelinectx.ExcerptVersion, r.ExcerptSHA256); err != nil {
			t.Errorf("re-derivation of %s from a fresh fetch at the recorded cap failed: %v", r.Name, err)
		}
	}
}

// GitLab ignores Range on the job-trace endpoint (verified live 2026-10-04),
// so the read starts at byte 0. The END of a log longer than the tail buffer
// must still be what the excerpt shows.
func TestPipelineTraceKeepsTheEndOfALongLogWhenRangeIsIgnored(t *testing.T) {
	if pipelineTraceMaxRead < 100<<20 {
		t.Errorf("pipelineTraceMaxRead = %d: below GitLab's default 100 MB job-log limit, the end of a long log is lost "+
			"when the server ignores Range", pipelineTraceMaxRead)
	}
	s := newPLScenario(t)
	s.gl.TraceIgnoresRange = true
	filler := strings.Repeat("progress line of a long synthetic job, nothing to see here\n", 160_000) // ~9.4 MiB
	s.gl.SetTrace(s.pid, 9002, []byte(filler+plTestLog))
	pc := s.build(t)
	mustFit(t, pc, plSHA)
	ex := pc.Failed[0].Excerpt
	if !strings.Contains(ex.Text, "AssertionError: sum([1, 2]) returned 3, expected 4") {
		t.Errorf("the end of a %d-byte log is not in the excerpt (Range ignored):\n%.400s", len(filler), ex.Text)
	}
	if !ex.HeadTruncated || ex.TailTruncated {
		t.Errorf("HeadTruncated=%v TailTruncated=%v, want true/false", ex.HeadTruncated, ex.TailTruncated)
	}
}

// A block whose fetch failed says so, and nothing is guessed in its place.
func TestPipelineContextUnavailableBlocksAreSaidNotGuessed(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("example/calc", glab.AccessMaintainer)
	gl.AddPipeline(p.ID, glab.Pipeline{ID: 900, CreatedAt: *plTime(19), IID: 41, Ref: "feature", SHA: plSHA, Status: "failed", Source: "merge_request_event"})
	gl.AddJob(p.ID, 900, glab.Job{ID: 9002, Name: "unit-tests", Stage: "test", Status: "failed", FailureReason: "script_failure"})
	// No commit, no lint reply, no trace, no MR: every one 404s.
	req := pipelineContextRequest{ProjectID: p.ID, Project: "example/calc", PipelineID: 900,
		Target: pipelinectx.MRTarget(7), LastSuccess: 0}
	pc, err := buildPipelineContext(context.Background(), gl.Client(), req)
	if err != nil {
		t.Fatalf("buildPipelineContext: %v", err)
	}
	prompt := mustFit(t, pc, "")

	for _, block := range []string{"commit", "merge request", "ci definition", "target pipelines", `log of job "unit-tests"`} {
		found := false
		for _, u := range pc.Unavailable {
			found = found || strings.HasPrefix(u, block+": ")
		}
		if !found {
			t.Errorf("Unavailable %v does not name %q", pc.Unavailable, block)
		}
	}
	for _, want := range []string{
		"== Commit ==\n(unavailable)\n",
		"== Merge request ==\n!7: (unavailable)\n",
		"== Run ==\n(unavailable)\n",
		"history: (unavailable)\n",
		"definition:\n(unavailable)\n",
		"when that is earlier):\n(unavailable)\n",
		// The lint reply is missing too; that is not an error of the pipeline.
		"\npipeline_error: none\n",
		"No checkout of the repository is available",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	for _, guess := range []string{"title: ", "author: ", "first failed pipeline"} {
		if strings.Contains(prompt, guess) {
			t.Errorf("prompt contains %q although nothing it could introduce was fetched", guess)
		}
	}
	if m := fenceOpenRe.FindString(prompt); m != "" {
		t.Errorf("prompt opens an untrusted block (%q) although no forge text was fetched", m)
	}
	if r := pc.JobRecords(); len(r) != 1 || r[0].Excerpted || r[0].ExcerptSHA256 != "" {
		t.Errorf("JobRecords = %+v, want the job recorded with NO excerpt", r)
	}
	for _, u := range pc.Unavailable {
		if strings.Contains(prompt, u) {
			t.Errorf("an error text reached the prompt: %q", u)
		}
	}
}

// lintDidNotRun is what GitLab's CI lint answered on the first live pipeline
// diagnosis (project 75, 2026-10-04) for a configuration whose workflow:rules
// run it on merge requests only: the lint is a dry run of ANOTHER pipeline
// source, and for that source no pipeline would be created.
const lintDidNotRun = "The pipeline did not run. Review the workflow:rules configuration for the pipeline."

// pipeline_error is the pipeline's OWN error and nothing else (gonk-mf3v).
// gonk's CI-lint dry run is used only to obtain the merged YAML; what it says
// about validity is not a fact about the diagnosed pipeline and reaches the
// prompt nowhere -- not on the pipeline_error line, not as a definition. The
// job definition is "(unavailable)" and the reason is in the log.
func TestPipelineErrorIsNeverTheLintResult(t *testing.T) {
	for name, lint := range map[string]glab.CILint{
		"invalid, no YAML":            {Valid: false, Errors: []string{lintDidNotRun}},
		"invalid, with a merged YAML": {Valid: false, Errors: []string{lintDidNotRun}, MergedYAML: plMergedYAML},
		"invalid, with warnings":      {Valid: false, Errors: []string{lintDidNotRun}, Warnings: []string{"LINT-WARNING-TEXT"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newPLScenario(t)
			s.gl.SetCILint(s.pid, plSHA, lint)
			pc := s.build(t)
			if pc.Pipeline.YamlErrors != "" || pc.Pipeline.FailureReason != "" {
				t.Fatalf("the scenario's pipeline has an error of its own (%q, %q); this test needs one without",
					pc.Pipeline.YamlErrors, pc.Pipeline.FailureReason)
			}
			prompt := mustFit(t, pc, plSHA)
			if !strings.Contains(prompt, "\npipeline_error: none\n") {
				t.Errorf("the prompt does not say `pipeline_error: none` for a pipeline with no error of its own:\n%s",
					section(prompt, "== Pipeline ==", "== Commit =="))
			}
			for _, leak := range []string{lintDidNotRun, "did not run", "workflow:rules", "CI definition check", "LINT-WARNING-TEXT", "dry run"} {
				if strings.Contains(prompt, leak) {
					t.Errorf("the lint result reached the prompt (%q)", leak)
				}
			}
			if !strings.Contains(prompt, "definition:\n(unavailable)\n") {
				t.Errorf("the job definition is not `(unavailable)` although the lint gave no usable YAML")
			}
			if fenceBlockExists(prompt, `CI definition of "unit-tests"`) {
				t.Errorf("a job definition was rendered from an INVALID lint reply")
			}
			logged := false
			for _, u := range pc.Unavailable {
				logged = logged || (strings.HasPrefix(u, "ci definition: ") && strings.Contains(u, lintDidNotRun))
				if strings.Contains(prompt, u) {
					t.Errorf("the logged reason reached the prompt: %q", u)
				}
			}
			if !logged {
				t.Errorf("Unavailable = %q: the reason the definition is unavailable (the lint's own words) is not there for the log", pc.Unavailable)
			}
		})
	}
}

// ...and the pipeline's own failure_reason and yaml_errors ARE that line,
// whatever the lint said.
func TestPipelineErrorIsThePipelinesOwnFailureReasonAndYamlErrors(t *testing.T) {
	const yamlErr = "jobs:unit-tests config contains unknown keys: frobnicate"
	for name, c := range map[string]struct {
		pl   glab.Pipeline
		want string
	}{
		"both":           {glab.Pipeline{FailureReason: "config_error", YamlErrors: yamlErr}, "pipeline_error: failure_reason config_error; yaml_errors " + yamlErr + "\n"},
		"failure_reason": {glab.Pipeline{FailureReason: "config_error"}, "pipeline_error: failure_reason config_error\n"},
		"yaml_errors":    {glab.Pipeline{YamlErrors: yamlErr}, "pipeline_error: yaml_errors " + yamlErr + "\n"},
		"neither":        {glab.Pipeline{}, "pipeline_error: none\n"},
	} {
		for lintName, lint := range map[string]*glab.CILint{
			"lint valid":       {Valid: true, MergedYAML: plMergedYAML},
			"lint invalid":     {Valid: false, Errors: []string{lintDidNotRun}},
			"lint unavailable": nil,
		} {
			t.Run(name+"/"+lintName, func(t *testing.T) {
				pl := c.pl
				pl.ID, pl.Status = 900, "failed"
				got := pipelineBlock(&pipelineContext{
					Req:      pipelineContextRequest{PipelineID: 900, Target: pipelinectx.RefTarget("main")},
					Pipeline: pl, Lint: lint,
				})
				if !strings.HasSuffix(got, "\n"+c.want) {
					t.Errorf("pipeline block ends\n%s\nwant its last line to be %q", got, c.want)
				}
				if strings.Contains(got, "did not run") || strings.Contains(got, "CI definition") {
					t.Errorf("the lint result is in the pipeline block:\n%s", got)
				}
			})
		}
	}
}

// section is the part of s from the first `from` up to the next `to`.
func section(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return s
	}
	if j := strings.Index(s[i:], to); j >= 0 {
		return s[i : i+j]
	}
	return s[i:]
}

// fenceBlockExists reports whether the prompt opens an untrusted block with
// this label.
func fenceBlockExists(prompt, label string) bool {
	for _, m := range fenceOpenRe.FindAllString(prompt, -1) {
		if strings.Contains(m, label) {
			return true
		}
	}
	return false
}

// Without the pipeline or its job list there is nothing to diagnose or to
// validate evidence against: the build fails instead of guessing.
func TestPipelineContextFailsWithoutThePipelineOrItsJobs(t *testing.T) {
	s := newPLScenario(t)
	s.req.PipelineID = 999 // unknown
	if _, err := buildPipelineContext(context.Background(), s.gl.Client(), s.req); err == nil {
		t.Error("an unknown pipeline produced a context")
	}
	s = newPLScenario(t)
	s.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/pipelines/900/jobs", s.pid), 404)
	if _, err := buildPipelineContext(context.Background(), s.gl.Client(), s.req); err == nil {
		t.Error("an unreadable job list produced a context")
	}
	if _, err := buildPipelineContext(context.Background(), nil, s.req); err == nil {
		t.Error("a nil reader produced a context")
	}
}

// The job definition comes from the MERGED YAML: a job defined in an included
// file (absent from the project's own .gitlab-ci.yml) is still shown, with
// the image it inherits.
func TestPipelineJobDefinitionFromMergedYAMLWithInclude(t *testing.T) {
	s := newPLScenario(t)
	// The repository's own file only includes another; the job lives in the
	// included file, and GitLab's lint reply has it merged.
	s.proj.PutFile(".gitlab-ci.yml", []byte("include:\n- local: ci/tests.yml\n"))
	merged := "stages:\n- test\ndefault:\n  image: example/base:2\nincluded-tests:\n  stage: test\n  script:\n  - ./run-included\n"
	s.gl.SetCILint(s.pid, plSHA, glab.CILint{Valid: true, MergedYAML: merged})
	s.gl.AddJob(s.pid, 900, glab.Job{ID: 9005, Name: "included-tests", Stage: "test", Status: "failed"})
	pc := s.build(t)
	prompt := mustFit(t, pc, plSHA)
	def := fencedBlock(t, prompt, `CI definition of "included-tests"`)
	for _, want := range []string{"included-tests:\n  stage: test\n  script:\n    - ./run-included", "default:\n  image: example/base:2"} {
		if !strings.Contains(def, want) {
			t.Errorf("definition is missing %q:\n%s", want, def)
		}
	}
	// A job the merged YAML does not hold is "unavailable", never another job's block.
	if d, ok := pipelineJobDefinition(&glab.CILint{Valid: true, MergedYAML: merged}, "unit-tests"); ok || d != "" {
		t.Errorf("definition of a job absent from the merged YAML = %q, %v", d, ok)
	}
	if _, ok := pipelineJobDefinition(&glab.CILint{Valid: false, Errors: []string{"bad"}}, "x"); ok {
		t.Error("an invalid lint reply produced a definition")
	}
}

// recordedLint loads one RAW ci/lint reply recorded by the maintainers'
// pipefix probe (pipefix_probe.py --keep-json, not published) on the synthetic
// project 75 (GitLab CE 19.4.1, run
// 20261007T145957Z, the `include` case: a commit that changed ONLY its locally
// included CI file). Kept under pkg/glab/testdata, where the client's own test
// reads the same two files.
func recordedLint(t *testing.T, name string) glab.CILint {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "pkg", "glab", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var l glab.CILint
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// recordedIncludeSHA is the commit both recorded replies were asked about.
const recordedIncludeSHA = "4a774003b2e8f5c8c4497827273c1aaf6d28710b"

// TestPipelineJobDefinitionIsTheCommitsOwn (gonk-z6gb). The definition shown
// for a failed job must be the CI configuration AT THE PIPELINE'S COMMIT. A
// lint with dry_run=true resolves `include: local:` on the project's DEFAULT
// BRANCH instead; the fake answers a dry run differently from a static lint,
// as the real GitLab does, and each row is one way that went wrong.
func TestPipelineJobDefinitionIsTheCommitsOwn(t *testing.T) {
	const ownYAML = "unit-tests:\n  stage: test\n  script:\n  - ./run-tests --from-this-commit\n"
	const defaultBranchYAML = "unit-tests:\n  stage: test\n  script:\n  - ./run-tests --from-the-default-branch\n"
	own := glab.CILint{Valid: true, MergedYAML: ownYAML, Includes: []glab.CILintInclude{
		{Type: glab.IncludeTypeLocal, Location: "ci/tests.yml", ContextSHA: plSHA}}}

	t.Run("a dry run would show the default branch's included file", func(t *testing.T) {
		s := newPLScenario(t)
		s.gl.SetCILint(s.pid, plSHA, own)
		s.gl.SetCILintDryRun(s.pid, plSHA, glab.CILint{Valid: true, MergedYAML: defaultBranchYAML, Includes: []glab.CILintInclude{
			{Type: glab.IncludeTypeLocal, Location: "ci/tests.yml", ContextSHA: plSHAGreen}}})
		pc := s.build(t)
		prompt := mustFit(t, pc, plSHA)
		def := fencedBlock(t, prompt, `CI definition of "unit-tests"`)
		if !strings.Contains(def, "--from-this-commit") || strings.Contains(prompt, "--from-the-default-branch") {
			t.Errorf("the definition is not the commit's own:\n%s", def)
		}
		if pc.Lint == nil || len(pc.Lint.Includes) != 1 || pc.Lint.Includes[0].ContextSHA != plSHA {
			t.Errorf("lint = %+v, want the include resolved at %s", pc.Lint, plSHA)
		}
	})

	t.Run("a dry run would show nothing: the recorded replies", func(t *testing.T) {
		static, dry := recordedLint(t, "ci_lint_include_static.json"), recordedLint(t, "ci_lint_include_dry_run.json")
		// The fixtures are what the test says they are, by named field.
		if !static.Valid || len(static.Includes) != 1 || static.Includes[0].Type != glab.IncludeTypeLocal ||
			static.Includes[0].ContextSHA != recordedIncludeSHA || !strings.Contains(static.MergedYAML, `echo "one more line"`) {
			t.Fatalf("the recorded static reply is not the include case: %+v", static)
		}
		if dry.Valid || dry.MergedYAML != "" || len(dry.Errors) != 1 || !strings.Contains(dry.Errors[0], "ci/probe-common.yml` does not exist") {
			t.Fatalf("the recorded dry-run reply is not the include case: %+v", dry)
		}
		s := newPLScenario(t)
		// The diagnosed pipeline is of the recorded commit, and its failed
		// job is one the recorded configuration defines.
		s.gl.AddPipeline(s.pid, glab.Pipeline{ID: 950, CreatedAt: *plTime(40), Ref: "main", SHA: recordedIncludeSHA, Status: "failed", Source: "push"})
		s.gl.AddJob(s.pid, 950, glab.Job{ID: 9501, Name: "probe-status", Stage: "check", Status: "failed", FailureReason: "script_failure"})
		s.gl.SetTrace(s.pid, 9501, []byte(plTestLog))
		s.gl.AddCommit(s.pid, glab.Commit{ID: recordedIncludeSHA, Title: "pipefix probe: include", AuthorName: "Example Author"})
		s.gl.SetCILint(s.pid, recordedIncludeSHA, static)
		s.gl.SetCILintDryRun(s.pid, recordedIncludeSHA, dry)
		s.req.PipelineID = 950
		pc := s.build(t)
		prompt := mustFit(t, pc, recordedIncludeSHA)
		def := fencedBlock(t, prompt, `CI definition of "probe-status"`)
		// The line the commit added to its INCLUDED file, in the job that
		// inherits it.
		for _, want := range []string{"probe-status:", `echo "one more line"`, "sh status.sh"} {
			if !strings.Contains(def, want) {
				t.Errorf("the definition is missing %q:\n%s", want, def)
			}
		}
		for _, u := range pc.Unavailable {
			if strings.HasPrefix(u, "ci definition") {
				t.Errorf("the CI definition was reported unavailable: %s", u)
			}
		}
	})

	t.Run("the lint request is not a dry run", func(t *testing.T) {
		s := newPLScenario(t)
		s.build(t)
		qs := s.gl.CILintQueries(s.pid)
		if len(qs) != 1 || qs[0].Get("content_ref") != plSHA || qs[0].Get("include_merged_yaml") != "true" {
			t.Fatalf("lint requests = %v, want one, at content_ref=%s, with the merged YAML", qs, plSHA)
		}
		if _, sent := qs[0]["dry_run"]; sent {
			t.Errorf("the lint is a DRY RUN (%v): its local includes are the default branch's", qs[0])
		}
	})

	// An include GitLab resolved at ANOTHER commit is not this commit's
	// definition: no definition is shown, and the log says why.
	t.Run("a local include resolved at another commit yields no definition", func(t *testing.T) {
		s := newPLScenario(t)
		other := own
		other.Includes = []glab.CILintInclude{{Type: glab.IncludeTypeLocal, Location: "ci/tests.yml", ContextSHA: plSHAGreen}}
		s.gl.SetCILint(s.pid, plSHA, other)
		pc := s.build(t)
		prompt := mustFit(t, pc, plSHA)
		if fenceBlockExists(prompt, `CI definition of "unit-tests"`) || !strings.Contains(prompt, "definition:\n(unavailable)\n") {
			t.Errorf("a definition was shown although its include was resolved at another commit")
		}
		logged := false
		for _, u := range pc.Unavailable {
			logged = logged || (strings.HasPrefix(u, "ci definition: ") && strings.Contains(u, "not at the commit asked for"))
		}
		if !logged {
			t.Errorf("the reason is not in the log: %q", pc.Unavailable)
		}
	})
}

// A definition longer than its cap is cut with a marker, at the cap.
func TestPipelineJobDefinitionIsCapped(t *testing.T) {
	var y strings.Builder
	y.WriteString("long-job:\n  script:\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&y, "  - ./step-%03d --with-a-fairly-long-argument\n", i)
	}
	d, ok := pipelineJobDefinition(&glab.CILint{Valid: true, MergedYAML: y.String()}, "long-job")
	if !ok || len(d) > pipelinectx.CapJobDefinitionPerJob || !strings.HasSuffix(d, pipelineBlockTruncated) {
		t.Errorf("definition: ok=%v len=%d (cap %d), truncated-suffix=%v", ok, len(d), pipelinectx.CapJobDefinitionPerJob,
			strings.HasSuffix(d, pipelineBlockTruncated))
	}
}
