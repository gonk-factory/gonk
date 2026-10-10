package main

// gonk-2usw part FACTS: what a pipeline-fix is later judged against is ON THE
// RECORD -- per failed job its stage, failure_reason, allow_failure and error
// signature (written by dispatch from GitLab's job object and the excerpt),
// and the landed diagnosis's class, files, evidence jobs, summary and remedy
// (written by the sweep's apply). Before this, both existed only as functions
// and as a rendered note (plan F1, F2).
//
// EVERY FIXTURE HERE IS SYNTHETIC (plan section 0): plRun's made-up project.

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// The first error line of each synthetic log, written out here rather than
// read back from the excerpt: the test's oracle must not be the code under
// test.
const (
	plTestFirstError  = "test_sum ... FAILED"
	plLintFirstError  = "ERROR: Job failed: exit code 2" // plLintLog has no other error line
	plSlashFirstError = "/usr/local/bin/run-tests: line 4: assertion failed in test_total"
)

func plJob(t *testing.T, rec beadstore.Record, name string) beadstore.PipelineJob {
	t.Helper()
	for _, j := range rec.PipelineJobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("the record names no job %q: %+v", name, rec.PipelineJobs)
	return beadstore.PipelineJob{}
}

// wantFacts is one recorded job's expected failure facts.
type wantFacts struct {
	job        string
	stage      string
	reason     string
	excerpted  bool
	firstError string // "" = no signature
	generic    bool
}

// TestDispatchRecordsJobFailureFacts: the record the REAL dispatch preparation
// writes carries, per failed job, the facts GitLab reported and the signature
// of the job's first error line.
func TestDispatchRecordsJobFailureFacts(t *testing.T) {
	cases := []struct {
		name string
		opts plOpts
		want []wantFacts
	}{
		{"one failed job, a specific first error", plOpts{},
			[]wantFacts{{"unit-tests", "test", "script_failure", true, plTestFirstError, false}}},
		{"each job its own stage and failure_reason; the runner's summary is flagged generic",
			plOpts{lintJob: true, lintStage: "check", lintReason: "runner_system_failure"},
			[]wantFacts{
				{"unit-tests", "test", "script_failure", true, plTestFirstError, false},
				{"lint", "check", "runner_system_failure", true, plLintFirstError, true},
			}},
		{"a job whose log could not be read has no signature and is generic",
			plOpts{lintJob: true, lintNoTrace: true},
			[]wantFacts{
				{"unit-tests", "test", "script_failure", true, plTestFirstError, false},
				{"lint", "test", "script_failure", false, "", true},
			}},
		{"the signature is of the FIRST error line, not the last",
			plOpts{slashLog: true},
			[]wantFacts{{"unit-tests", "test", "script_failure", true, plSlashFirstError, false}}},
		{"a long log: the excerpt cap does not change the signature",
			plOpts{longLog: true},
			[]wantFacts{{"unit-tests", "test", "script_failure", true, plTestFirstError, false}}},
		{"a job beyond the excerpt slots still has its GitLab facts",
			plOpts{lintJob: true, bigContext: true}, // five failed jobs; unit-tests sorts fifth
			[]wantFacts{
				{"check-a", "test", "script_failure", true, plTestFirstError, false},
				{"lint", "test", "script_failure", true, plLintFirstError, true},
				{"unit-tests", "test", "script_failure", false, "", true},
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newPLRun(t, c.opts)
			for _, w := range c.want {
				j := plJob(t, r.rec, w.job)
				if j.Excerpted != w.excerpted {
					t.Fatalf("fixture: job %s Excerpted = %v, want %v", w.job, j.Excerpted, w.excerpted)
				}
				wantSig := pipelinectx.ErrorSignature(w.firstError)
				if (w.firstError == "") != (wantSig == "") {
					t.Fatalf("fixture: signature of %q is %q", w.firstError, wantSig)
				}
				got := wantFacts{j.Name, j.Stage, j.FailureReason, j.Excerpted, w.firstError, j.ErrorGeneric}
				if got != w || j.ErrorSignature != wantSig {
					t.Errorf("job %s recorded\n got  stage=%q failure_reason=%q signature=%q generic=%v\n want stage=%q failure_reason=%q signature=%q (of %q) generic=%v",
						w.job, j.Stage, j.FailureReason, j.ErrorSignature, j.ErrorGeneric,
						w.stage, w.reason, wantSig, w.firstError, w.generic)
				}
				if j.SignatureVersion != pipelinectx.SignatureVersion || !j.FailureFactsRecorded() {
					t.Errorf("job %s SignatureVersion = %d, want %d", w.job, j.SignatureVersion, pipelinectx.SignatureVersion)
				}
				if j.AllowFailure {
					t.Errorf("job %s recorded allow_failure true: it failed the pipeline", w.job)
				}
				if j.GenericFailure() != w.generic {
					t.Errorf("job %s GenericFailure() = %v, want %v", w.job, j.GenericFailure(), w.generic)
				}
			}
			// EVERY recorded job has the facts, not only the ones named above.
			for _, j := range r.rec.PipelineJobs {
				if !j.FailureFactsRecorded() || j.Stage == "" || j.FailureReason == "" {
					t.Errorf("job %s recorded without failure facts: %+v", j.Name, j)
				}
				if !j.Excerpted && j.ErrorSignature != "" {
					t.Errorf("job %s has a signature (%s) but no excerpt it could come from", j.Name, j.ErrorSignature)
				}
			}
		})
	}
}

// The recorded signature is what the controller computes again, with zero
// tokens, from the same job's log at the RECORDED cap (plan 3.2 E8, 6.3 J3):
// if dispatch's value and a later recomputation could differ for an unchanged
// log, every fix would look like a changed failure.
func TestRecordedSignatureIsRecomputableFromTheJobLog(t *testing.T) {
	for _, o := range []plOpts{{}, {lintJob: true}, {longLog: true}, {lintJob: true, bigContext: true}, {liveP75: true}} {
		r := newPLRun(t, o)
		n := 0
		for _, j := range r.rec.PipelineJobs {
			if !j.Excerpted {
				continue
			}
			n++
			tt, err := fetchPipelineTrace(context.Background(), r.gl.Client(), r.pid, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			again := pipelinectx.ExcerptWith(tt.Data, pipelineExcerptOptions(tt, r.rec.PipelineExcerptMaxBytes))
			if again.ErrorSignature != j.ErrorSignature || pipelinectx.GenericFirstError(again.FirstError) != j.ErrorGeneric {
				t.Errorf("%+v job %s: recorded signature %q generic=%v, recomputed %q generic=%v (first error %q)",
					o, j.Name, j.ErrorSignature, j.ErrorGeneric, again.ErrorSignature,
					pipelinectx.GenericFirstError(again.FirstError), again.FirstError)
			}
			// ...and at any other cap: the signature must not depend on how
			// much of the log the prompt had room for.
			small := pipelinectx.ExcerptWith(tt.Data, pipelineExcerptOptions(tt, pipelineExcerptFloorBytes))
			if small.ErrorSignature != j.ErrorSignature {
				t.Errorf("%+v job %s: signature %q at the recorded cap, %q at the floor cap", o, j.Name, j.ErrorSignature, small.ErrorSignature)
			}
		}
		if n == 0 {
			t.Fatalf("%+v: fixture has no excerpted job", o)
		}
	}
}

// A failed job that was ALLOWED to fail did not fail the pipeline: it is not
// in the record's job list at all, so it can never be a fix's target.
func TestAnAllowedFailureIsNotARecordedFailedJob(t *testing.T) {
	_, rec := plRunWithExtraJob(t, glab.Job{ID: 9005, Name: "optional-check", Stage: "test", Status: "failed",
		FailureReason: "script_failure", AllowFailure: true})
	for _, j := range rec.PipelineJobs {
		if j.Name == "optional-check" {
			t.Fatalf("an allow_failure job is recorded as a failed job: %+v", j)
		}
	}
	if len(rec.PipelineJobs) != 1 || rec.PipelineJobs[0].Name != "unit-tests" || rec.PipelineJobs[0].AllowFailure {
		t.Fatalf("PipelineJobs = %+v, want only the required failed job, allow_failure false", rec.PipelineJobs)
	}
}

// plRunWithExtraJob is newPLRun's default world with one more job on the
// failed pipeline, dispatched again so the record reflects it.
func plRunWithExtraJob(t *testing.T, job glab.Job) (*plRun, beadstore.Record) {
	t.Helper()
	r := newPLRun(t, plOpts{})
	r.gl.AddJob(r.pid, 900, job)
	r.gl.SetTrace(r.pid, job.ID, []byte(plLintLog))
	pp, err := preparePipelineDispatch(context.Background(),
		dispatchDeps{Pipelines: r.gl.Client(), Log: testLogger(io.Discard)}, r.rec.SessionID, r.rec)
	if err != nil {
		t.Fatalf("preparePipelineDispatch: %v", err)
	}
	return r, pp.apply(r.rec)
}

// TestApplyRecordsTheLandedDiagnosis: an accepted diagnosis is ON THE RECORD
// the caller goes on to write -- class, files, evidence jobs, summary, remedy
// -- exactly as validated.
func TestApplyRecordsTheLandedDiagnosis(t *testing.T) {
	base := plDiagnosis(nil).Effects[0]
	cases := []struct {
		name   string
		opts   plOpts
		mutate func(*effects.Effect)
		want   beadstore.PipelineDiagnosis
	}{
		{"the plain case", plOpts{}, nil, beadstore.PipelineDiagnosis{
			Class: "code", Files: []string{"calc/sum.py"}, EvidenceJobs: []string{"unit-tests"},
			Summary: base.Summary, Remedy: base.Remedy}},
		{"evidence jobs are distinct, in first-cited order", plOpts{lintJob: true}, func(e *effects.Effect) {
			e.Evidence = []effects.EvidenceItem{
				{Job: "lint", Line: plLintLine},
				{Job: "unit-tests", Line: plGoodLine},
				{Job: "lint", Fact: effects.FactStage},
				{Job: "unit-tests", Fact: effects.FactFailureReason},
			}
		}, beadstore.PipelineDiagnosis{
			Class: "code", Files: []string{"calc/sum.py"}, EvidenceJobs: []string{"lint", "unit-tests"},
			Summary: base.Summary, Remedy: base.Remedy}},
		{"a failed job the evidence does not name is not an evidence job", plOpts{lintJob: true}, nil,
			beadstore.PipelineDiagnosis{
				Class: "code", Files: []string{"calc/sum.py"}, EvidenceJobs: []string{"unit-tests"},
				Summary: base.Summary, Remedy: base.Remedy}},
		{"a pipeline-level evidence item names no job", plOpts{}, func(e *effects.Effect) {
			e.Evidence = append(e.Evidence, effects.EvidenceItem{Fact: effects.FactPipelineError})
		}, beadstore.PipelineDiagnosis{
			Class: "code", Files: []string{"calc/sum.py"}, EvidenceJobs: []string{"unit-tests"},
			Summary: base.Summary, Remedy: base.Remedy}},
		{"the class is the diagnosis's, and a class without files records none", plOpts{}, func(e *effects.Effect) {
			e.Classification, e.Files = effects.ClassInfrastructure, nil
		}, beadstore.PipelineDiagnosis{
			Class: "infrastructure", EvidenceJobs: []string{"unit-tests"},
			Summary: base.Summary, Remedy: base.Remedy}},
		{"summary and remedy are the diagnosis's own text", plOpts{}, func(e *effects.Effect) {
			e.Summary, e.Remedy = "  A different summary.\n", "A different remedy."
		}, beadstore.PipelineDiagnosis{
			Class: "code", Files: []string{"calc/sum.py"}, EvidenceJobs: []string{"unit-tests"},
			Summary: "  A different summary.\n", Remedy: "A different remedy."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newPLRun(t, c.opts)
			rec := r.rec
			if rec.PipelineDiagnosis.Recorded() {
				t.Fatalf("fixture: a record fresh from dispatch already carries a diagnosis: %+v", rec.PipelineDiagnosis)
			}
			applied, v, err := applyPipelineBatch(context.Background(), r.deps(t, io.Discard), &rec, plDiagnosis(c.mutate))
			if !applied || v != "" || err != nil {
				t.Fatalf("applied=%v violation=%q err=%v", applied, v, err)
			}
			if !reflect.DeepEqual(rec.PipelineDiagnosis, c.want) {
				t.Fatalf("recorded diagnosis\n got  %+v\n want %+v", rec.PipelineDiagnosis, c.want)
			}
			if !rec.PipelineDiagnosis.Recorded() {
				t.Fatal("Recorded() is false for a landed diagnosis")
			}
			// Every evidence job is one of the record's own failed jobs: T
			// (plan 3.2) is a join on the record, never on the batch.
			for _, name := range rec.PipelineDiagnosis.EvidenceJobs {
				plJob(t, rec, name)
			}
			// Nothing else on the record moved.
			rec.PipelineDiagnosis = beadstore.PipelineDiagnosis{}
			if !reflect.DeepEqual(rec, r.rec) {
				t.Fatalf("the apply changed more of the record than the diagnosis:\n got  %+v\n want %+v", rec, r.rec)
			}
		})
	}
}

// The recorded slices are the record's own: the batch the session emitted can
// be reused or dropped without changing what was recorded.
func TestTheRecordedDiagnosisDoesNotAliasTheBatch(t *testing.T) {
	r := newPLRun(t, plOpts{})
	rec, b := r.rec, plDiagnosis(nil)
	if applied, v, err := applyPipelineBatch(context.Background(), r.deps(t, io.Discard), &rec, b); !applied {
		t.Fatalf("applied=%v violation=%q err=%v", applied, v, err)
	}
	b.Effects[0].Files[0] = "elsewhere.py"
	if len(rec.PipelineDiagnosis.Files) != 1 || rec.PipelineDiagnosis.Files[0] != "calc/sum.py" {
		t.Fatalf("the recorded files alias the batch: %v", rec.PipelineDiagnosis.Files)
	}
}

// A diagnosis that did NOT land records nothing: not when it is refused
// (gate-failed), not when it could not be checked (UNKNOWN), and not when
// every check passed but the run note could not be written.
func TestADiagnosisThatDidNotLandIsNotRecorded(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*plRun)
		mutate  func(*effects.Effect)
		refused string // the violation's cause; "" = an error (UNKNOWN)
	}{
		{"refused: a hallucinated line", nil, func(e *effects.Effect) { e.Evidence[0].Line = "AssertionError: this line is in no log at all" }, causeUngrounded},
		{"refused: a cited file that does not exist", nil, func(e *effects.Effect) { e.Files = []string{"calc/missing.py"} }, causeFileMissing},
		{"refused: a forged marker", nil, func(e *effects.Effect) { e.Remedy += " <!-- gonk:pipeline-run:x state=resolved -->" }, causeForgedMarker},
		{"unknown: the log cannot be re-read", func(r *plRun) {
			r.gl.FailAlways("GET", "/api/v4/projects/1/jobs/9002/trace", 500)
		}, nil, ""},
		{"unknown: the run note cannot be written", func(r *plRun) {
			r.gl.FailAlways("POST", "/api/v4/projects/1/issues", 500)
		}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newPLRun(t, plOpts{})
			if c.prepare != nil {
				c.prepare(r)
			}
			rec := r.rec
			applied, v, err := applyPipelineBatch(context.Background(), r.deps(t, io.Discard), &rec, plDiagnosis(c.mutate))
			if applied {
				t.Fatal("fixture: the batch was applied")
			}
			if c.refused != "" && (err != nil || !strings.HasPrefix(v, c.refused+":")) {
				t.Fatalf("violation=%q err=%v, want a refusal with cause %s", v, err, c.refused)
			}
			if c.refused == "" && (err == nil || v != "") {
				t.Fatalf("violation=%q err=%v, want an error (UNKNOWN)", v, err)
			}
			// The ONE thing a refusal leaves on the record is the word for the
			// next attempt (retry_feedback.go), under the refusal's own cause;
			// an UNKNOWN leaves not even that -- it is ours, not the answer's.
			fb := rec.RetryFeedback
			if (c.refused != "") != fb.Recorded() || fb.Code != c.refused {
				t.Fatalf("retry feedback = %+v after a refusal with cause %q", fb, c.refused)
			}
			rec.RetryFeedback = beadstore.RetryFeedback{}
			if rec.PipelineDiagnosis.Recorded() || !reflect.DeepEqual(rec, r.rec) {
				t.Fatalf("a diagnosis that did not land changed the record: %+v", rec.PipelineDiagnosis)
			}
		})
	}
}

// A NUL in the prose would make the whole record unwritable in Postgres
// (JSONB cannot hold one) -- the run note would have landed and the record
// could never say so. It is dropped from the recorded copy.
func TestTheRecordedDiagnosisHoldsNoNUL(t *testing.T) {
	r := newPLRun(t, plOpts{})
	rec := r.rec
	applied, v, err := applyPipelineBatch(context.Background(), r.deps(t, io.Discard), &rec, plDiagnosis(func(e *effects.Effect) {
		e.Summary = "The unit test of sum() fails\x00: wrong total."
		e.Remedy = "\x00Fix sum()."
	}))
	if !applied {
		// If the chain ever refuses a NUL outright, that is safe too -- then
		// assert THAT here instead. What must never happen is a landed note
		// with an unwritable record.
		t.Fatalf("applied=%v violation=%q err=%v: the chain no longer lands a diagnosis whose prose carries a NUL", applied, v, err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `\u0000`) {
		t.Fatalf("the record carries a NUL character:\n%s", raw)
	}
	if got, want := rec.PipelineDiagnosis.Summary, "The unit test of sum() fails: wrong total."; got != want {
		t.Fatalf("recorded summary %q, want %q", got, want)
	}
	if got, want := rec.PipelineDiagnosis.Remedy, "Fix sum()."; got != want {
		t.Fatalf("recorded remedy %q, want %q", got, want)
	}
}

// THROUGH THE SWEEP AND THE STORE: the record gonk-sweep WRITES after it
// lands a diagnosis carries it, with the job facts dispatch recorded, and the
// job failure keys and cause key a fix dispatch will read are computable from
// that stored record alone.
func TestTheSweepPersistsTheLandedDiagnosisAndTheJobFailureFacts(t *testing.T) {
	r := newPLRun(t, plOpts{lintJob: true, lintStage: "check"})
	res := r.sweep(t, plSessionOutput(t, plDiagnosis(nil)), "done")
	if len(res.outcomes) != 1 || res.outcomes[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcomes = %+v, want one success\n%s", res.outcomes, res.logs)
	}
	rec := res.rec
	if rec.State != beadstore.StateDone {
		t.Fatalf("stored state = %s, want done", rec.State)
	}
	want := beadstore.PipelineDiagnosis{
		Class: "code", Files: []string{"calc/sum.py"}, EvidenceJobs: []string{"unit-tests"},
		Summary: plDiagnosis(nil).Effects[0].Summary, Remedy: plDiagnosis(nil).Effects[0].Remedy,
	}
	if !reflect.DeepEqual(rec.PipelineDiagnosis, want) {
		t.Fatalf("stored diagnosis\n got  %+v\n want %+v", rec.PipelineDiagnosis, want)
	}
	unit, lint := plJob(t, rec, "unit-tests"), plJob(t, rec, "lint")
	if unit.ErrorSignature != pipelinectx.ErrorSignature(plTestFirstError) || unit.Stage != "test" || unit.GenericFailure() ||
		lint.ErrorSignature != pipelinectx.ErrorSignature(plLintFirstError) || lint.Stage != "check" || !lint.GenericFailure() {
		t.Fatalf("stored job facts: unit-tests %+v, lint %+v", unit, lint)
	}
	// The target set T and its cause key, from the stored record alone.
	var keys []pipelinectx.JobFailureKey
	for _, name := range rec.PipelineDiagnosis.EvidenceJobs {
		j := plJob(t, rec, name)
		keys = append(keys, pipelinectx.JobFailureKey{Stage: j.Stage, Name: j.Name, FailureReason: j.FailureReason, ErrorSignature: j.ErrorSignature})
	}
	wantKey := pipelinectx.JobFailureKey{Stage: "test", Name: "unit-tests", FailureReason: "script_failure",
		ErrorSignature: pipelinectx.ErrorSignature(plTestFirstError)}
	if len(keys) != 1 || keys[0] != wantKey || pipelinectx.CauseKey(keys) != pipelinectx.CauseKey([]pipelinectx.JobFailureKey{wantKey}) {
		t.Fatalf("job failure keys from the stored record = %v, want [%v]", keys, wantKey)
	}
	raw, err := json.MarshalIndent(struct {
		BeadAnchor        string
		State             beadstore.State
		PipelineID        int64
		PipelineSHA       string
		PipelineJobs      []beadstore.PipelineJob
		PipelineDiagnosis beadstore.PipelineDiagnosis
	}{rec.BeadAnchor, rec.State, rec.PipelineID, rec.PipelineSHA, rec.PipelineJobs, rec.PipelineDiagnosis}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the stored record after the sweep (cause key %s):\n%s", pipelinectx.CauseKey(keys), raw)
}

// A REFUSED diagnosis through the sweep leaves the stored record without one,
// whatever the ladder then does with the bead.
func TestTheSweepDoesNotPersistARefusedDiagnosis(t *testing.T) {
	r := newPLRun(t, plOpts{})
	res := r.sweep(t, plSessionOutput(t, plDiagnosis(func(e *effects.Effect) {
		e.Evidence[0].Line = "AssertionError: this line is in no log at all"
	})), "escalate")
	if len(res.outcomes) != 1 || res.outcomes[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcomes = %+v, want one gate-failed\n%s", res.outcomes, res.logs)
	}
	if res.rec.PipelineDiagnosis.Recorded() {
		t.Fatalf("a refused diagnosis is on the stored record: %+v", res.rec.PipelineDiagnosis)
	}
}
