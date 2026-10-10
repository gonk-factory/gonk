package pipefix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

var (
	tDispatch = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	tDeadline = tDispatch.Add(2 * time.Hour)
	tEarly    = tDispatch.Add(10 * time.Minute) // before the deadline
	tLate     = tDeadline.Add(time.Minute)      // past it
)

func ok(id int64, name, stage string) Attempt {
	return Attempt{ID: id, Name: name, Stage: stage, Status: StatusSuccess}
}

// failedWith is a failed attempt whose log was read and whose first error
// line is `line`.
func failedWith(id int64, name, line string) Attempt {
	return Attempt{
		ID: id, Name: name, Stage: "test", Status: StatusFailed, FailureReason: "script_failure",
		Trace: Trace{Read: true, FirstError: line},
	}
}

// goodFacts is a fix that IS fixed: one diagnosed job (unit-tests), which
// passes on its only attempt in the one green pipeline of the merge request,
// an untouched CI definition, and nothing else failing. Every row fixture is
// this with ONE fact changed.
func goodFacts() JudgeFacts {
	return JudgeFacts{
		Fix: FixFacts{
			Recorded:       true,
			Targets:        []Target{target("unit-tests", lineOld)},
			BasePipelineID: 400,
			BaseJobs: []BaseJob{
				{Name: "compile", Status: StatusSuccess},
				{Name: "unit-tests", Status: StatusFailed},
				{Name: "lint", Status: StatusSuccess},
				{Name: "package", Status: StatusSkipped},
			},
			FixBaseSHA:       shaBase,
			FixCommitSHA:     shaFix,
			MRIID:            7,
			ExcerptVersion:   pipelinectx.ExcerptVersion,
			SignatureVersion: pipelinectx.SignatureVersion,
			CIExecChanges:    CIExecManualRun,
			VerifyDeadline:   tDeadline,
		},
		MR: MRFacts{Read: true, IID: 7, State: MRStateOpened, SHA: shaFix},
		Pipelines: PipelineFacts{Read: true, Pipelines: []Pipeline{{
			ID: 501, SHA: shaFix, Source: "merge_request_event", Status: StatusSuccess,
			JobsRead: true, IncludeRetried: true,
			Jobs: []Attempt{
				ok(9001, "compile", "build"),
				ok(9002, "unit-tests", "test"),
				ok(9003, "lint", "test"),
				ok(9004, "package", "package"),
			},
		}}},
		Definition: DefinitionFacts{Read: true, Delta: cidef.Delta{Kind: cidef.KindNone}},
	}
}

// ---- fixture editing ----

func pipe(f *JudgeFacts, id int64) *Pipeline {
	for i := range f.Pipelines.Pipelines {
		if f.Pipelines.Pipelines[i].ID == id {
			return &f.Pipelines.Pipelines[i]
		}
	}
	panic("no such pipeline in the fixture")
}

// setJob replaces every attempt of the named job in pipeline 501.
func setJob(f *JudgeFacts, name string, attempts ...Attempt) {
	p := pipe(f, 501)
	var jobs []Attempt
	for _, a := range p.Jobs {
		if a.Name != name {
			jobs = append(jobs, a)
		}
	}
	p.Jobs = append(jobs, attempts...)
}

// redPipeline marks pipeline 501 failed (what GitLab reports when a required
// job failed).
func redPipeline(f *JudgeFacts) { pipe(f, 501).Status = StatusFailed }

// secondPipeline adds another candidate pipeline of the same commit, green,
// where every job passed.
func secondPipeline(f *JudgeFacts) *Pipeline {
	f.Pipelines.Pipelines = append(f.Pipelines.Pipelines, Pipeline{
		ID: 502, SHA: shaFix, Source: "push", Status: StatusSuccess,
		JobsRead: true, IncludeRetried: true,
		Jobs: []Attempt{
			ok(9101, "compile", "build"),
			ok(9102, "unit-tests", "test"),
			ok(9103, "lint", "test"),
			ok(9104, "package", "package"),
		},
	})
	return pipe(f, 502)
}

// secondTarget adds a second diagnosed job, `integration`, that failed on the
// base and passes at head.
func secondTarget(f *JudgeFacts) {
	f.Fix.Targets = append(f.Fix.Targets, target("integration", lineNew))
	f.Fix.BaseJobs = append(f.Fix.BaseJobs, BaseJob{Name: "integration", Status: StatusFailed})
	p := pipe(f, 501)
	p.Jobs = append(p.Jobs, ok(9005, "integration", "test"))
}

func withLift(f *JudgeFacts) { f.Fix.Lift = []string{LiftCI} }

type judgeCase struct {
	name string
	mut  func(*JudgeFacts)
	late bool // judged past the verify deadline

	decision Decision
	verdict  Verdict
	state    State
	rule     Rule
	// job: the Rule of the finding for unit-tests ("" = not asserted).
	job Rule
	// other: the Rule of the one J4 finding ("" = there must be none).
	other Rule
	// def: Result.Definition.
	def cidef.Kind
	// missing: a substring one entry of Result.Missing must hold.
	missing string
}

func runJudgeCases(t *testing.T, cases []judgeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := goodFacts()
			if c.mut != nil {
				c.mut(&f)
			}
			now := tEarly
			if c.late {
				now = tLate
			}
			r := Judge(f, now)
			checkResult(t, r, c)
		})
	}
}

func checkResult(t *testing.T, r Result, c judgeCase) {
	t.Helper()
	if r.Decision != c.decision || r.Verdict != c.verdict {
		t.Fatalf("decision=%q verdict=%q rule=%q (%s) missing=%v\nwant decision=%q verdict=%q rule=%q",
			r.Decision, r.Verdict, r.Rule, r.Detail, r.Missing, c.decision, c.verdict, c.rule)
	}
	if r.Rule != c.rule {
		t.Errorf("rule = %q (%s), want %q", r.Rule, r.Detail, c.rule)
	}
	if r.State != c.state {
		t.Errorf("state = %q, want %q", r.State, c.state)
	}
	if (r.Verdict != "") != (r.Decision == DecisionVerdict) {
		t.Errorf("verdict %q with decision %q: a verdict is set iff the decision is one", r.Verdict, r.Decision)
	}
	if r.Detail == "" {
		t.Errorf("no detail")
	}
	if c.job != "" {
		var got Rule
		for _, j := range r.Jobs {
			if j.Job == "unit-tests" {
				got = j.Rule
			}
		}
		if got != c.job {
			t.Errorf("finding for unit-tests = %q, want %q (all: %+v)", got, c.job, r.Jobs)
		}
	}
	if c.decision == DecisionVerdict {
		switch {
		case c.other == "" && len(r.Others) != 0:
			t.Errorf("J4 findings %+v, want none", r.Others)
		case c.other != "" && (len(r.Others) != 1 || r.Others[0].Rule != c.other):
			t.Errorf("J4 findings %+v, want exactly one %q", r.Others, c.other)
		}
		// Result.Definition is the delta wherever J2 could compare the
		// definition and found it holding; "" where J2 itself refused.
		wantDef := c.def
		if wantDef == "" && !strings.HasPrefix(string(c.rule), "J2.") {
			wantDef = cidef.KindNone
		}
		if r.Definition != wantDef {
			t.Errorf("definition = %q, want %q", r.Definition, wantDef)
		}
	}
	if c.missing != "" {
		found := false
		for _, m := range r.Missing {
			found = found || strings.Contains(m, c.missing)
		}
		if !found {
			t.Errorf("missing = %v, want an entry naming %q", r.Missing, c.missing)
		}
	}
	if c.decision != DecisionCannotJudge && len(r.Missing) != 0 {
		t.Errorf("missing = %v on a %q result", r.Missing, r.Decision)
	}
}

func TestTheBaselineFixIsFixed(t *testing.T) {
	r := Judge(goodFacts(), tEarly)
	checkResult(t, r, judgeCase{decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed,
		rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone})
	if !reflect.DeepEqual(r.Candidates, []int64{501}) {
		t.Errorf("candidates = %v, want [501]", r.Candidates)
	}
	if len(r.Jobs) != 1 || r.Jobs[0].Attempts != 1 || r.Jobs[0].Succeeded != 1 || r.Jobs[0].Failed != 0 {
		t.Errorf("job findings = %+v, want one job, one attempt, one success", r.Jobs)
	}
	// and it is the same past the deadline: a settled, fully read fix does
	// not go stale.
	if late := Judge(goodFacts(), tLate); late.Verdict != VerdictFixed {
		t.Errorf("past the deadline: %q (%s), want FIXED", late.Verdict, late.Detail)
	}
}

// ---- J0: identity ----

func TestJ0Identity(t *testing.T) {
	runJudgeCases(t, []judgeCase{
		{name: "the MR was closed", mut: func(f *JudgeFacts) { f.MR.State = MRStateClosed },
			decision: DecisionStopped, state: StateClosedUnmerged, rule: RuleJ0Closed},
		{name: "the MR was merged", mut: func(f *JudgeFacts) { f.MR.State = MRStateMerged },
			decision: DecisionStopped, state: StateMerged, rule: RuleJ0Merged},
		{name: "merged, and its head is not gonk's commit: still merged",
			mut:      func(f *JudgeFacts) { f.MR.State = MRStateMerged; f.MR.SHA = shaOther },
			decision: DecisionStopped, state: StateMerged, rule: RuleJ0Merged},
		{name: "closed, and its head is not gonk's commit: still closed",
			mut:      func(f *JudgeFacts) { f.MR.State = MRStateClosed; f.MR.SHA = shaOther },
			decision: DecisionStopped, state: StateClosedUnmerged, rule: RuleJ0Closed},
		{name: "somebody else pushed", mut: func(f *JudgeFacts) { f.MR.SHA = shaOther },
			decision: DecisionStopped, state: StateTakenOver, rule: RuleJ0TakenOver},
		{name: "somebody else pushed, past the deadline", late: true, mut: func(f *JudgeFacts) { f.MR.SHA = shaOther },
			decision: DecisionStopped, state: StateTakenOver, rule: RuleJ0TakenOver},
		{name: "the MR was not read", mut: func(f *JudgeFacts) { f.MR.Read = false },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr"},
		{name: "the MR was not read, past the deadline: still no verdict", late: true,
			mut:      func(f *JudgeFacts) { f.MR.Read = false },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr"},
		{name: "the facts are of another MR", mut: func(f *JudgeFacts) { f.MR.IID = 8 },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr.iid"},
		{name: "the MR is locked (mid-merge)", mut: func(f *JudgeFacts) { f.MR.State = "locked" },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr.state"},
		{name: "the MR has no head SHA", mut: func(f *JudgeFacts) { f.MR.SHA = "" },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr.sha"},
	})
}

// An empty SHA on both sides is equal and is not an identity.
func TestTwoEmptySHAsAreNotAnIdentity(t *testing.T) {
	f := goodFacts()
	f.MR.SHA, f.Fix.FixCommitSHA = "", ""
	for i := range f.Pipelines.Pipelines {
		f.Pipelines.Pipelines[i].SHA = ""
	}
	r := Judge(f, tEarly)
	if r.Decision != DecisionCannotJudge {
		t.Fatalf("decision = %q verdict = %q; want cannot-judge", r.Decision, r.Verdict)
	}
}

// ---- the recorded facts: each one missing means "cannot judge" ----

func TestEveryRecordedFactIsRequired(t *testing.T) {
	cases := []struct {
		name    string
		missing string
		mut     func(*JudgeFacts)
	}{
		{"nothing recorded", "fix.recorded", func(f *JudgeFacts) { f.Fix.Recorded = false }},
		{"T is empty", "fix.targets", func(f *JudgeFacts) { f.Fix.Targets = nil }},
		{"a target has no name", "fix.targets", func(f *JudgeFacts) { f.Fix.Targets[0].Key.Name = "" }},
		{"a target is listed twice", "fix.targets", func(f *JudgeFacts) {
			f.Fix.Targets = append(f.Fix.Targets, f.Fix.Targets[0])
		}},
		{"no base pipeline", "fix.base_pipeline_id", func(f *JudgeFacts) { f.Fix.BasePipelineID = 0 }},
		{"no base jobs", "fix.base_jobs", func(f *JudgeFacts) { f.Fix.BaseJobs = nil }},
		{"a base job is listed twice", "fix.base_jobs", func(f *JudgeFacts) {
			f.Fix.BaseJobs = append(f.Fix.BaseJobs, BaseJob{Name: "lint", Status: StatusFailed})
		}},
		{"a base job has no status", "fix.base_jobs", func(f *JudgeFacts) { f.Fix.BaseJobs[0].Status = "" }},
		{"a target is not among the base jobs", "fix.base_jobs", func(f *JudgeFacts) {
			f.Fix.BaseJobs = []BaseJob{f.Fix.BaseJobs[0], f.Fix.BaseJobs[2], f.Fix.BaseJobs[3]}
		}},
		{"a target did not fail on the base", "fix.base_jobs", func(f *JudgeFacts) { f.Fix.BaseJobs[1].Status = StatusSuccess }},
		{"a target was allowed to fail on the base", "fix.base_jobs", func(f *JudgeFacts) { f.Fix.BaseJobs[1].AllowFailure = true }},
		{"no base SHA", "fix.fix_base_sha", func(f *JudgeFacts) { f.Fix.FixBaseSHA = "" }},
		{"no fix commit", "fix.fix_commit_sha", func(f *JudgeFacts) { f.Fix.FixCommitSHA = "" }},
		{"no MR iid", "fix.mr_iid", func(f *JudgeFacts) { f.Fix.MRIID = 0; f.MR.IID = 0 }},
		{"no excerpt version", "fix.excerpt_version", func(f *JudgeFacts) { f.Fix.ExcerptVersion = 0 }},
		{"no signature version", "fix.signature_version", func(f *JudgeFacts) { f.Fix.SignatureVersion = 0 }},
		{"no ci_exec_changes", "fix.ci_exec_changes", func(f *JudgeFacts) { f.Fix.CIExecChanges = "" }},
		{"a ci_exec_changes nobody defined", "fix.ci_exec_changes", func(f *JudgeFacts) { f.Fix.CIExecChanges = "always" }},
		{"no verify deadline", "fix.verify_deadline", func(f *JudgeFacts) { f.Fix.VerifyDeadline = time.Time{} }},
	}
	for _, c := range cases {
		for _, when := range []struct {
			name string
			now  time.Time
		}{{"before the deadline", tEarly}, {"past the deadline", tLate}} {
			t.Run(c.name+"/"+when.name, func(t *testing.T) {
				f := goodFacts()
				c.mut(&f)
				r := Judge(f, when.now)
				checkResult(t, r, judgeCase{decision: DecisionCannotJudge, rule: RuleFacts, missing: c.missing})
			})
		}
	}
	t.Run("no clock", func(t *testing.T) {
		r := Judge(goodFacts(), time.Time{})
		checkResult(t, r, judgeCase{decision: DecisionCannotJudge, rule: RuleFacts, missing: "now"})
	})
	t.Run("the zero value", func(t *testing.T) {
		r := Judge(JudgeFacts{}, tEarly)
		if r.Decision != DecisionCannotJudge || r.Verdict != "" || r.State != "" {
			t.Fatalf("Judge(zero) = %+v; want cannot-judge, no verdict, no state", r)
		}
	})
}

// ---- J1: settle ----

func TestJ1Settle(t *testing.T) {
	noPipelines := func(f *JudgeFacts) { f.Pipelines.Pipelines = nil }
	status := func(s string) func(*JudgeFacts) {
		return func(f *JudgeFacts) { pipe(f, 501).Status = s }
	}
	cases := []judgeCase{
		{name: "no pipeline yet", mut: noPipelines,
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "no pipeline, past the deadline", late: true, mut: noPipelines,
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout,
			job: RuleJ3Absent},
		{name: "the only pipeline is of another commit", mut: func(f *JudgeFacts) { pipe(f, 501).SHA = shaOther },
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "the only pipeline is of another commit, past the deadline", late: true,
			mut:      func(f *JudgeFacts) { pipe(f, 501).SHA = shaOther },
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout,
			job: RuleJ3Absent},
		{name: "the only pipeline is a child pipeline", mut: func(f *JudgeFacts) { pipe(f, 501).Source = SourceParentPipeline },
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "a child pipeline still running does not hold the verdict",
			mut: func(f *JudgeFacts) {
				p := secondPipeline(f)
				p.Source, p.Status = SourceParentPipeline, "running"
			},
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "a pipeline of another commit still running does not hold the verdict",
			mut: func(f *JudgeFacts) {
				p := secondPipeline(f)
				p.SHA, p.Status = shaOther, "running"
			},
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "a second candidate is still running",
			mut:      func(f *JudgeFacts) { secondPipeline(f).Status = "running" },
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "a second candidate is still running, past the deadline", late: true,
			mut:      func(f *JudgeFacts) { secondPipeline(f).Status = "running" },
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout,
			job: RuleJ3Passed},
		{name: "the pipeline list was not read", mut: func(f *JudgeFacts) { f.Pipelines.Read = false },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "pipelines"},
		{name: "the pipeline list was not read, past the deadline", late: true,
			mut:      func(f *JudgeFacts) { f.Pipelines.Read = false },
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Unread,
			job: RuleJ3Unread},
		{name: "a pipeline has no id", mut: func(f *JudgeFacts) { pipe(f, 501).ID = 0 },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "pipelines"},
		{name: "a pipeline has no source", mut: func(f *JudgeFacts) { pipe(f, 501).Source = "" },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "pipelines"},
		{name: "a pipeline has no sha", mut: func(f *JudgeFacts) { pipe(f, 501).SHA = "" },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "pipelines"},
	}
	for _, s := range []string{"created", "waiting_for_resource", "preparing", "pending", "running", "manual", "scheduled", "canceling", ""} {
		cases = append(cases,
			judgeCase{name: "status " + s + " is not settled", mut: status(s),
				decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
			judgeCase{name: "status " + s + " past the deadline", late: true, mut: status(s),
				decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout,
				job: RuleJ3Passed})
	}
	runJudgeCases(t, cases)

	// The four settled statuses: each is judged before the deadline (what it
	// is judged AS is J3's and J5's business).
	for _, s := range []string{StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped} {
		t.Run("status "+s+" is settled", func(t *testing.T) {
			f := goodFacts()
			pipe(&f, 501).Status = s
			if r := Judge(f, tEarly); r.Decision != DecisionVerdict {
				t.Fatalf("decision = %q (%s), want a verdict", r.Decision, r.Detail)
			}
		})
	}
}

// skippedEmpty turns pipeline 501 into what GitLab creates for a `[skip ci]`
// commit (observed on project 75, GitLab 19.4.1, 2026-10-07; the recorded
// facts are testdata/skipci_facts.json): status `skipped`, its jobs listed
// with include_retried=true, and none there.
func skippedEmpty(f *JudgeFacts) {
	p := pipe(f, 501)
	p.Status, p.Jobs = StatusSkipped, nil
}

// TestJ1ASkippedPipelineWithoutJobsIsNotACandidate: plan 6.3 J1 and 5.5
// (gonk-vi9k). A `[skip ci]` commit gets pipelines that are `skipped` and ran
// nothing. Nothing was observed on them and nothing ever will be: they are not
// candidates. The exit check waits for a pipeline somebody starts, and only
// the deadline ends the wait. Never a pass.
func TestJ1ASkippedPipelineWithoutJobsIsNotACandidate(t *testing.T) {
	recurs := func(p *Pipeline) {
		p.Status = StatusFailed
		for i := range p.Jobs {
			if p.Jobs[i].Name == "unit-tests" {
				p.Jobs[i] = failedWith(p.Jobs[i].ID, "unit-tests", lineOld)
			}
		}
	}
	cases := []judgeCase{
		{name: "skipped, no jobs: wait", mut: skippedEmpty,
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "skipped, no jobs, past the deadline: J1.TIMEOUT", late: true, mut: skippedEmpty,
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout,
			job: RuleJ3Absent},
		{name: "one per source, both skipped with no jobs (the observed shape): wait",
			mut: func(f *JudgeFacts) {
				skippedEmpty(f)
				p := secondPipeline(f)
				p.Status, p.Jobs = StatusSkipped, nil
			},
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "a pipeline started later on the same commit is a normal candidate: it passes",
			mut: func(f *JudgeFacts) {
				skippedEmpty(f)
				secondPipeline(f)
			},
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "a pipeline started later on the same commit is a normal candidate: it recurs",
			mut: func(f *JudgeFacts) {
				skippedEmpty(f)
				recurs(secondPipeline(f))
			},
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "a pipeline started later is still running: wait",
			mut: func(f *JudgeFacts) {
				skippedEmpty(f)
				secondPipeline(f).Status = "running"
			},
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "a pipeline started later is still running, past the deadline", late: true,
			mut: func(f *JudgeFacts) {
				skippedEmpty(f)
				secondPipeline(f).Status = "running"
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout,
			job: RuleJ3Passed},
		// ---- unchanged: everything that is NOT "skipped and ran nothing" ----
		{name: "skipped WITH jobs is a candidate (the diagnosed job itself skipped)",
			mut: func(f *JudgeFacts) {
				pipe(f, 501).Status = StatusSkipped
				a := ok(9002, "unit-tests", "test")
				a.Status = StatusSkipped
				setJob(f, "unit-tests", a)
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Status,
			job: RuleJ3Status},
		{name: "skipped with ONE job that is not the diagnosed one is a candidate",
			mut: func(f *JudgeFacts) {
				p := pipe(f, 501)
				p.Status, p.Jobs = StatusSkipped, []Attempt{ok(9001, "compile", "build")}
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Absent,
			job: RuleJ3Absent},
		{name: "skipped, jobs NOT listed: nobody knows it ran nothing",
			mut: func(f *JudgeFacts) {
				skippedEmpty(f)
				pipe(f, 501).JobsRead = false
			},
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "jobs"},
		{name: "skipped, jobs listed without include_retried: nobody knows it ran nothing",
			mut: func(f *JudgeFacts) {
				skippedEmpty(f)
				pipe(f, 501).IncludeRetried = false
			},
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "include_retried"},
		{name: "skipped, jobs NOT listed, past the deadline", late: true,
			mut: func(f *JudgeFacts) {
				skippedEmpty(f)
				pipe(f, 501).JobsRead = false
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Unread,
			job: RuleJ3Unread},
	}
	// Any OTHER terminal status with no jobs is settled and judged at once.
	for _, s := range []string{StatusSuccess, StatusFailed, StatusCanceled} {
		cases = append(cases, judgeCase{name: "status " + s + " with no jobs is a candidate",
			mut: func(f *JudgeFacts) {
				p := pipe(f, 501)
				p.Status, p.Jobs = s, nil
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Absent,
			job: RuleJ3Absent})
	}
	runJudgeCases(t, cases)

	t.Run("what is not a candidate is named, and is not in C", func(t *testing.T) {
		f := goodFacts()
		skippedEmpty(&f)
		secondPipeline(&f)
		r := Judge(f, tEarly)
		if !reflect.DeepEqual(r.Candidates, []int64{502}) || !reflect.DeepEqual(r.Skipped, []int64{501}) {
			t.Errorf("candidates %v skipped %v, want [502] and [501]", r.Candidates, r.Skipped)
		}
		f = goodFacts()
		skippedEmpty(&f)
		for _, now := range []time.Time{tEarly, tLate} {
			r := Judge(f, now)
			if len(r.Candidates) != 0 || !reflect.DeepEqual(r.Skipped, []int64{501}) {
				t.Errorf("candidates %v skipped %v, want none and [501]", r.Candidates, r.Skipped)
			}
			if !strings.Contains(r.Detail, "skipped") {
				t.Errorf("detail %q does not say the pipelines were skipped", r.Detail)
			}
		}
		if r := Judge(goodFacts(), tEarly); len(r.Skipped) != 0 {
			t.Errorf("skipped %v on a fix whose pipeline ran", r.Skipped)
		}
	})

	// Never a pass: with nothing but skipped pipelines there is no instant at
	// which the answer is FIXED or FIXED_NEW_FAILURE.
	t.Run("never a pass", func(t *testing.T) {
		f := goodFacts()
		skippedEmpty(&f)
		for _, now := range []time.Time{tDispatch, tEarly, tDeadline.Add(-time.Nanosecond), tDeadline, tLate, tLate.Add(1000 * time.Hour)} {
			if r := Judge(f, now); r.Verdict == VerdictFixed || r.Verdict == VerdictFixedNewFailure {
				t.Errorf("at %s: verdict %q by %q", now, r.Verdict, r.Rule)
			}
		}
	})
}

// TestTheRecordedSkipCIFactsWaitThenTimeOut judges the facts `gonk-gate
// pipefix-judge` READ on the real GitLab for a `[skip ci]` commit
// (testdata/skipci_facts.json: the `facts` of its output, verbatim, from
// hack/pipefix_probe.py run 20261007T145957Z on the synthetic project 75,
// merge request !46). The build that recorded them answered UNVERIFIABLE by
// J3.NOT_OBSERVED(absent) at once, with both pipelines as candidates
// (gonk-vi9k).
func TestTheRecordedSkipCIFactsWaitThenTimeOut(t *testing.T) {
	raw, err := os.ReadFile("testdata/skipci_facts.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Now   time.Time  `json:"now"`
		Facts JudgeFacts `json:"facts"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		t.Fatal(err)
	}
	f := rec.Facts
	// The fixture is what the test says it is: named fields, not "it parsed".
	if !f.Fix.Recorded || f.Fix.MRIID != 46 || !f.MR.Read || f.MR.State != MRStateOpened || f.MR.SHA != f.Fix.FixCommitSHA ||
		!f.Pipelines.Read || len(f.Pipelines.Pipelines) != 2 || !f.Definition.Read || f.Definition.Delta.Kind != cidef.KindNone {
		t.Fatalf("the recorded facts are not the [skip ci] case: %+v", f)
	}
	sources := map[string]bool{}
	for _, p := range f.Pipelines.Pipelines {
		sources[p.Source] = true
		if p.Status != StatusSkipped || p.SHA != f.Fix.FixCommitSHA || !p.JobsRead || !p.IncludeRetried || len(p.Jobs) != 0 {
			t.Fatalf("recorded pipeline %+v is not a skipped pipeline of the fix commit with no jobs", p)
		}
	}
	if !sources["push"] || !sources["merge_request_event"] {
		t.Fatalf("recorded pipeline sources %v, want one push and one merge_request_event", sources)
	}
	if rec.Now.IsZero() || !rec.Now.Before(f.Fix.VerifyDeadline) {
		t.Fatalf("recorded at %s with deadline %s: not before the deadline", rec.Now, f.Fix.VerifyDeadline)
	}

	early := Judge(f, rec.Now)
	if early.Decision != DecisionWait || early.Rule != RuleJ1Wait || early.State != StateVerifying || early.Verdict != "" {
		t.Errorf("when it was recorded: decision %q verdict %q by %q (%s); want wait by %s",
			early.Decision, early.Verdict, early.Rule, early.Detail, RuleJ1Wait)
	}
	if len(early.Candidates) != 0 || !reflect.DeepEqual(early.Skipped, []int64{3332, 3333}) {
		t.Errorf("candidates %v skipped %v, want none and [3332 3333]", early.Candidates, early.Skipped)
	}
	late := Judge(f, f.Fix.VerifyDeadline)
	if late.Decision != DecisionVerdict || late.Verdict != VerdictUnverifiable || late.Rule != RuleJ1Timeout || late.State != StateUnverifiable {
		t.Errorf("at the deadline: decision %q verdict %q by %q (%s); want UNVERIFIABLE by %s",
			late.Decision, late.Verdict, late.Rule, late.Detail, RuleJ1Timeout)
	}

	// A maintainer starts the pipeline: one more pipeline of the SAME commit,
	// in which the diagnosed job runs. It is judged like any other.
	started := func(status string, job Attempt) JudgeFacts {
		g := f
		g.Pipelines.Pipelines = append(append([]Pipeline{}, f.Pipelines.Pipelines...), Pipeline{
			ID: 3400, SHA: f.Fix.FixCommitSHA, Source: "merge_request_event", Status: status,
			JobsRead: true, IncludeRetried: true,
			Jobs: []Attempt{ok(30001, "probe-lint", "check"), job, ok(30003, "probe-report", "report")},
		})
		return g
	}
	if r := Judge(started(StatusSuccess, ok(30002, "probe-status", "check")), rec.Now); r.Verdict != VerdictFixed || !reflect.DeepEqual(r.Candidates, []int64{3400}) {
		t.Errorf("a started pipeline that passes: verdict %q by %q, candidates %v; want FIXED on [3400]", r.Verdict, r.Rule, r.Candidates)
	}
	failed := Attempt{ID: 30002, Name: "probe-status", Stage: "check", Status: StatusFailed, FailureReason: "script_failure", Trace: Trace{Read: true}}
	if r := Judge(started(StatusFailed, failed), rec.Now); r.Verdict == VerdictFixed || r.Verdict == VerdictFixedNewFailure || r.Decision != DecisionVerdict {
		t.Errorf("a started pipeline whose diagnosed job fails: decision %q verdict %q by %q", r.Decision, r.Verdict, r.Rule)
	}
}

func TestTheDeadlineItselfIsPastTheDeadline(t *testing.T) {
	f := goodFacts()
	pipe(&f, 501).Status = "running"
	if r := Judge(f, tDeadline.Add(-time.Nanosecond)); r.Decision != DecisionWait {
		t.Errorf("a nanosecond before the deadline: %q, want wait", r.Decision)
	}
	if r := Judge(f, tDeadline); r.Verdict != VerdictUnverifiable || r.Rule != RuleJ1Timeout {
		t.Errorf("at the deadline: decision %q verdict %q rule %q, want UNVERIFIABLE by %s", r.Decision, r.Verdict, r.Rule, RuleJ1Timeout)
	}
}

// ---- J2: definition integrity ----

func TestJ2Definition(t *testing.T) {
	delta := func(d cidef.Delta) func(*JudgeFacts) {
		return func(f *JudgeFacts) { f.Definition.Delta = d }
	}
	lifted := func(d cidef.Delta) func(*JudgeFacts) {
		return func(f *JudgeFacts) { withLift(f); f.Definition.Delta = d }
	}
	violation := cidef.Violation{Rule: cidef.RuleD2, Job: "lint", Key: "rules", Detail: "a GATING key differs from the base"}
	runJudgeCases(t, []judgeCase{
		{name: "a violation",
			mut:      lifted(cidef.Delta{Kind: cidef.KindViolation, Violations: []cidef.Violation{violation}}),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2Violation, job: RuleJ3Passed},
		{name: "Kind says violation and the list is empty",
			mut:      lifted(cidef.Delta{Kind: cidef.KindViolation}),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2Violation, job: RuleJ3Passed},
		{name: "a violation is listed and Kind says preserving",
			mut:      lifted(cidef.Delta{Kind: cidef.KindPreserving, Violations: []cidef.Violation{violation}}),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2Violation, job: RuleJ3Passed},
		{name: "a violation and an unresolvable value: the violation decides",
			mut: lifted(cidef.Delta{Kind: cidef.KindViolation, Violations: []cidef.Violation{violation},
				Unresolvable: []cidef.Unresolvable{{Side: cidef.SideBoth, Job: "package", Key: "script", Reason: "!reference"}}}),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2Violation, job: RuleJ3Passed},
		{name: "unresolvable on both sides",
			mut: lifted(cidef.Delta{Kind: cidef.KindUnresolvable,
				Unresolvable: []cidef.Unresolvable{{Side: cidef.SideBoth, Job: "package", Key: "script", Reason: "!reference"}}}),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ2Unresolvable, job: RuleJ3Passed},
		{name: "unresolvable on the base only",
			mut: lifted(cidef.Delta{Kind: cidef.KindUnresolvable,
				Unresolvable: []cidef.Unresolvable{{Side: cidef.SideBase, Job: "package", Key: "script", Reason: "!reference"}}}),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ2Unresolvable, job: RuleJ3Passed},
		{name: "Kind says unresolvable and the list is empty",
			mut:      lifted(cidef.Delta{Kind: cidef.KindUnresolvable}),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ2Unresolvable, job: RuleJ3Passed},
		{name: "an unresolvable value is listed and Kind says none",
			mut: delta(cidef.Delta{Kind: cidef.KindNone,
				Unresolvable: []cidef.Unresolvable{{Side: cidef.SideBoth, Job: "package", Key: "script", Reason: "!reference"}}}),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ2Unresolvable, job: RuleJ3Passed},
		{name: "unresolvable at head only: the change introduced it",
			mut: lifted(cidef.Delta{Kind: cidef.KindUnresolvable,
				Unresolvable: []cidef.Unresolvable{{Side: cidef.SideHead, Job: "lint", Key: "rules", Reason: "!reference"}}}),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2HeadUnresolvable, job: RuleJ3Passed},
		{name: "one head-only unresolvable among base-side ones still decides",
			mut: lifted(cidef.Delta{Kind: cidef.KindUnresolvable,
				Unresolvable: []cidef.Unresolvable{
					{Side: cidef.SideBoth, Job: "package", Key: "script", Reason: "!reference"},
					{Side: cidef.SideHead, Reason: "the merged CI YAML holds a second document"},
				}}),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2HeadUnresolvable, job: RuleJ3Passed},
		{name: "preserving, with the ci lift",
			mut:      lifted(cidef.Delta{Kind: cidef.KindPreserving}),
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindPreserving},
		{name: "preserving, WITHOUT the ci lift",
			mut:      delta(cidef.Delta{Kind: cidef.KindPreserving}),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2Unlifted, job: RuleJ3Passed},
		{name: "exec-changing, WITHOUT the ci lift",
			mut:      delta(cidef.Delta{Kind: cidef.KindExecChanging}),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2Unlifted, job: RuleJ3Passed},
		{name: "a lift of some other class is not the ci lift",
			mut: func(f *JudgeFacts) {
				f.Fix.Lift = []string{"deploy"}
				f.Definition.Delta = cidef.Delta{Kind: cidef.KindPreserving}
			},
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2Unlifted, job: RuleJ3Passed},
		{name: "exec-changing, lifted, manual-run: flagged, and the verdict stands",
			mut:      lifted(cidef.Delta{Kind: cidef.KindExecChanging}),
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindExecChanging},
		{name: "exec-changing, lifted, auto",
			mut: func(f *JudgeFacts) {
				withLift(f)
				f.Fix.CIExecChanges = CIExecAuto
				f.Definition.Delta = cidef.Delta{Kind: cidef.KindExecChanging}
			},
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindExecChanging},
		{name: "exec-changing, lifted, ci_exec_changes: deny",
			mut: func(f *JudgeFacts) {
				withLift(f)
				f.Fix.CIExecChanges = CIExecDeny
				f.Definition.Delta = cidef.Delta{Kind: cidef.KindExecChanging}
			},
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ2ExecDenied, job: RuleJ3Passed},
		{name: "preserving under ci_exec_changes: deny is fine",
			mut: func(f *JudgeFacts) {
				withLift(f)
				f.Fix.CIExecChanges = CIExecDeny
				f.Definition.Delta = cidef.Delta{Kind: cidef.KindPreserving}
			},
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindPreserving},
		{name: "none, with a lift nobody used",
			mut:      lifted(cidef.Delta{Kind: cidef.KindNone}),
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "the delta was not read", mut: func(f *JudgeFacts) { f.Definition.Read = false },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "definition"},
		{name: "the delta was not read, past the deadline", late: true,
			mut:      func(f *JudgeFacts) { f.Definition.Read = false },
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ2Unread, job: RuleJ3Passed},
		{name: "a delta nobody computed (the zero Kind)", mut: delta(cidef.Delta{}),
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "definition"},
		{name: "a Kind nobody defined", mut: delta(cidef.Delta{Kind: "harmless"}),
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "definition"},
		{name: "a delta nobody computed, past the deadline", late: true, mut: delta(cidef.Delta{}),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ2Unread, job: RuleJ3Passed},
	})
}

// ---- J3: per diagnosed job, over ALL attempts in ALL candidate pipelines ----

func TestJ3PerDiagnosedJob(t *testing.T) {
	job := func(attempts ...Attempt) func(*JudgeFacts) {
		return func(f *JudgeFacts) { setJob(f, "unit-tests", attempts...) }
	}
	red := func(attempts ...Attempt) func(*JudgeFacts) {
		return func(f *JudgeFacts) { setJob(f, "unit-tests", attempts...); redPipeline(f) }
	}
	other := func(status string) Attempt {
		return Attempt{ID: 9002, Name: "unit-tests", Stage: "test", Status: status}
	}
	unread := failedWith(9002, "unit-tests", "")
	unread.Trace = Trace{}
	otherStage := failedWith(9002, "unit-tests", lineOld)
	otherStage.Stage = "verify"
	otherReason := failedWith(9002, "unit-tests", lineOld)
	otherReason.FailureReason = "job_execution_timeout"
	allowed := ok(9002, "unit-tests", "test")
	allowed.AllowFailure = true
	allowedFailed := failedWith(9002, "unit-tests", lineOld)
	allowedFailed.AllowFailure = true

	runJudgeCases(t, []judgeCase{
		// NOT_OBSERVED(absent)
		{name: "absent: the job is in no candidate pipeline", mut: job(),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Absent, job: RuleJ3Absent},
		// WEAKENED
		{name: "weakened: the job passed and was allowed to fail", mut: job(allowed),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ3Weakened, job: RuleJ3Weakened},
		{name: "weakened: the job failed and was allowed to fail (the pipeline is green)", mut: job(allowedFailed),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ3Weakened, job: RuleJ3Weakened},
		{name: "weakened: one retried attempt was allowed to fail",
			mut:      job(allowedFailed, ok(9010, "unit-tests", "test")),
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ3Weakened, job: RuleJ3Weakened},
		// UNKNOWN
		{name: "unknown: a failed attempt's log was not read", mut: red(unread),
			decision: DecisionCannotJudge, rule: RuleJ3Unknown, job: RuleJ3Unknown, missing: "trace"},
		{name: "unknown past the deadline is not observed", late: true, mut: red(unread),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Unread, job: RuleJ3Unread},
		{name: "unknown: the unread attempt was retried into a pass -- still no verdict",
			mut:      job(unread, ok(9010, "unit-tests", "test")),
			decision: DecisionCannotJudge, rule: RuleJ3Unknown, job: RuleJ3Unknown, missing: "trace"},
		// RECURS
		{name: "recurs: the same failure", mut: red(failedWith(9002, "unit-tests", lineOld)),
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "recurs: the same failure with other numbers in it", mut: red(failedWith(9002, "unit-tests", lineOldVar)),
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "recurs: failed twice the same way",
			mut:      red(failedWith(9002, "unit-tests", lineOld), failedWith(9010, "unit-tests", lineOld)),
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "recurs: one attempt the old way, then one a new way",
			mut:      red(failedWith(9002, "unit-tests", lineOld), failedWith(9010, "unit-tests", lineNew)),
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "recurs: one attempt a new way, then one the old way",
			mut:      red(failedWith(9002, "unit-tests", lineNew), failedWith(9010, "unit-tests", lineOld)),
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "recurs: the attempt's only error is the runner's summary (conservative)",
			mut:      red(failedWith(9002, "unit-tests", lineGeneric)),
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "recurs: the attempt's log has no error line (conservative)",
			mut:      red(failedWith(9002, "unit-tests", "")),
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "recurs: the RECORDED signature is generic, the new error is specific (conservative)",
			mut: func(f *JudgeFacts) {
				f.Fix.Targets[0] = target("unit-tests", lineGeneric)
				setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineNew))
				redPipeline(f)
			},
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		{name: "recurs: a generic target needs no trace",
			mut: func(f *JudgeFacts) {
				f.Fix.Targets[0] = target("unit-tests", lineGeneric)
				setJob(f, "unit-tests", unread)
				redPipeline(f)
			},
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		// FLAKY
		{name: "flaky: failed the same way, then passed on a retry",
			mut:      job(failedWith(9002, "unit-tests", lineOld), ok(9010, "unit-tests", "test")),
			decision: DecisionVerdict, verdict: VerdictFlaky, state: StateFlaky, rule: RuleJ3FlakySame, job: RuleJ3FlakySame},
		{name: "flaky: passed, then failed the same way on a later attempt",
			mut:      red(ok(9002, "unit-tests", "test"), failedWith(9010, "unit-tests", lineOld)),
			decision: DecisionVerdict, verdict: VerdictFlaky, state: StateFlaky, rule: RuleJ3FlakySame, job: RuleJ3FlakySame},
		{name: "flaky: failed a different way, then passed on a retry",
			mut:      job(failedWith(9002, "unit-tests", lineNew), ok(9010, "unit-tests", "test")),
			decision: DecisionVerdict, verdict: VerdictFlaky, state: StateFlaky, rule: RuleJ3FlakyChanged, job: RuleJ3FlakyChanged},
		// CHANGED
		{name: "changed: the same job fails with a different error", mut: red(failedWith(9002, "unit-tests", lineNew)),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ3Changed, job: RuleJ3Changed},
		{name: "changed: the same error text under another failure_reason", mut: red(otherReason),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ3Changed, job: RuleJ3Changed},
		{name: "changed: the same error text in another stage", mut: red(otherStage),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ3Changed, job: RuleJ3Changed},
		// PASSED is the baseline; twice is still PASSED
		{name: "passed: two attempts, both green",
			mut:      job(ok(9002, "unit-tests", "test"), ok(9010, "unit-tests", "test")),
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		// NOT_OBSERVED(status)
		{name: "status: manual", mut: job(other("manual")),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Status, job: RuleJ3Status},
		{name: "status: skipped", mut: job(other(StatusSkipped)),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Status, job: RuleJ3Status},
		{name: "status: canceled", mut: job(other(StatusCanceled)),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Status, job: RuleJ3Status},
		// gonk-5b2f: an attempt that has not run yet holds J1 (wait) until
		// the deadline; past it the job was not observed, and J1's timeout
		// is the cause named.
		{name: "status: created", mut: job(other("created")),
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "status: created, past the deadline", late: true, mut: job(other("created")),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout, job: RuleJ3Status},
		{name: "status: scheduled (start_in)", mut: job(other("scheduled")),
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "status: scheduled (start_in), past the deadline", late: true, mut: job(other("scheduled")),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout, job: RuleJ3Status},
		{name: "status: one attempt passed and one was canceled -- not all succeeded",
			mut:      job(ok(9002, "unit-tests", "test"), Attempt{ID: 9010, Name: "unit-tests", Stage: "test", Status: StatusCanceled}),
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Status, job: RuleJ3Status},
		{name: "a failed attempt outranks a canceled one",
			mut:      red(failedWith(9002, "unit-tests", lineOld), Attempt{ID: 9010, Name: "unit-tests", Stage: "test", Status: StatusCanceled}),
			decision: DecisionVerdict, verdict: VerdictNotFixed, state: StateNotFixed, rule: RuleJ3Recurs, job: RuleJ3Recurs},
		// versions
		{name: "versions: a failed attempt cannot be compared with a signature of another version",
			mut: func(f *JudgeFacts) {
				f.Fix.SignatureVersion = pipelinectx.SignatureVersion + 1
				setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineOld))
				redPipeline(f)
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Versions, job: RuleJ3Versions},
		{name: "versions: nor with an excerpt of another version",
			mut: func(f *JudgeFacts) {
				f.Fix.ExcerptVersion = pipelinectx.ExcerptVersion + 1
				setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineNew))
				redPipeline(f)
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Versions, job: RuleJ3Versions},
		{name: "versions: a pass needs no signature",
			mut:      func(f *JudgeFacts) { f.Fix.SignatureVersion = pipelinectx.SignatureVersion + 1 },
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		// the job list
		{name: "jobs: a candidate's jobs were not listed", mut: func(f *JudgeFacts) { pipe(f, 501).JobsRead = false },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "jobs"},
		{name: "jobs: listed without the retried attempts", mut: func(f *JudgeFacts) { pipe(f, 501).IncludeRetried = false },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "include_retried"},
		{name: "jobs: a job object has no id", mut: func(f *JudgeFacts) { pipe(f, 501).Jobs[2].ID = 0 },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "jobs"},
		{name: "jobs: a job object has no name", mut: func(f *JudgeFacts) { pipe(f, 501).Jobs[2].Name = "" },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "jobs"},
		{name: "jobs: a job object has no status", mut: func(f *JudgeFacts) { pipe(f, 501).Jobs[2].Status = "" },
			decision: DecisionCannotJudge, rule: RuleFacts, missing: "jobs"},
		{name: "jobs: not listed, past the deadline: a visible pass is not PASSED", late: true,
			mut:      func(f *JudgeFacts) { pipe(f, 501).JobsRead = false },
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Unread, job: RuleJ3Unread},
		{name: "jobs: listed without retried attempts, past the deadline", late: true,
			mut:      func(f *JudgeFacts) { pipe(f, 501).IncludeRetried = false },
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ3Unread, job: RuleJ3Unread},
		{name: "jobs: an incomplete listing past the deadline still shows a weakened job", late: true,
			mut: func(f *JudgeFacts) {
				pipe(f, 501).IncludeRetried = false
				setJob(f, "unit-tests", allowed)
			},
			decision: DecisionVerdict, verdict: VerdictForged, state: StateNotFixed, rule: RuleJ3Weakened, job: RuleJ3Weakened},
	})
}

// "Conservative across pipelines: one recurring attempt in ANY candidate
// pipeline outweighs a pass in another" (plan 6.3). Whichever pipeline holds
// the bad attempt, in whichever order they are listed.
func TestConservativeAcrossPipelines(t *testing.T) {
	bad := map[string]struct {
		attempt Attempt
		verdict Verdict
		rule    Rule
	}{
		"recurs":   {failedWith(9102, "unit-tests", lineOld), VerdictFlaky, RuleJ3FlakySame},
		"changed":  {failedWith(9102, "unit-tests", lineNew), VerdictFlaky, RuleJ3FlakyChanged},
		"weakened": {Attempt{ID: 9102, Name: "unit-tests", Stage: "test", Status: StatusSuccess, AllowFailure: true}, VerdictForged, RuleJ3Weakened},
		"skipped":  {Attempt{ID: 9102, Name: "unit-tests", Stage: "test", Status: StatusSkipped}, VerdictUnverifiable, RuleJ3Status},
	}
	for name, b := range bad {
		for _, badIn := range []int64{501, 502} { // the older, the newer candidate
			for _, order := range []string{"listed in id order", "listed newest first"} {
				t.Run(fmt.Sprintf("%s in pipeline %d/%s", name, badIn, order), func(t *testing.T) {
					f := goodFacts()
					secondPipeline(&f)
					p := pipe(&f, badIn)
					for i := range p.Jobs {
						if p.Jobs[i].Name == "unit-tests" {
							a := b.attempt
							a.ID = p.Jobs[i].ID
							p.Jobs[i] = a
						}
					}
					if b.attempt.Status == StatusFailed {
						p.Status = StatusFailed
					}
					if order == "listed newest first" {
						ps := f.Pipelines.Pipelines
						ps[0], ps[1] = ps[1], ps[0]
					}
					r := Judge(f, tEarly)
					if r.Verdict != b.verdict || r.Rule != b.rule {
						t.Fatalf("unit-tests passes in one candidate pipeline and is %s in pipeline %d: verdict %q by %q (%s); want %q by %q",
							name, badIn, r.Verdict, r.Rule, r.Detail, b.verdict, b.rule)
					}
					if !reflect.DeepEqual(r.Candidates, []int64{501, 502}) {
						t.Errorf("candidates = %v, want [501 502]", r.Candidates)
					}
				})
			}
		}
	}
	t.Run("recurs in both: NOT_FIXED", func(t *testing.T) {
		f := goodFacts()
		p := secondPipeline(&f)
		p.Status = StatusFailed
		p.Jobs[1] = failedWith(9102, "unit-tests", lineOld)
		setJob(&f, "unit-tests", failedWith(9002, "unit-tests", lineOld))
		redPipeline(&f)
		if r := Judge(f, tEarly); r.Verdict != VerdictNotFixed {
			t.Fatalf("verdict %q, want NOT_FIXED", r.Verdict)
		}
	})
	t.Run("a job that only runs in one of the candidates is judged on that one", func(t *testing.T) {
		f := goodFacts()
		p := secondPipeline(&f)
		p.Jobs = append(p.Jobs[:1], p.Jobs[2:]...) // no unit-tests in 502
		if r := Judge(f, tEarly); r.Verdict != VerdictFixed {
			t.Fatalf("verdict %q (%s), want FIXED", r.Verdict, r.Detail)
		}
	})
}

// "A human retry does not launder a failure: the failed attempt is still in
// A." The MR pipeline ends GREEN, because the retry passed; the job is not
// PASSED and the fix is not FIXED.
func TestAHumanRetryIsNotAPass(t *testing.T) {
	f := goodFacts()
	setJob(&f, "unit-tests",
		failedWith(9002, "unit-tests", lineOld), // the first attempt: the diagnosed failure
		ok(9050, "unit-tests", "test"))          // somebody pressed retry
	r := Judge(f, tEarly)
	if r.Verdict == VerdictFixed || r.Verdict == VerdictFixedNewFailure {
		t.Fatalf("verdict %q: a retried failure was read as a pass", r.Verdict)
	}
	if len(r.Jobs) != 1 {
		t.Fatalf("verdict %q with job findings %+v; want one finding", r.Verdict, r.Jobs)
	}
	if r.Verdict != VerdictFlaky || r.Jobs[0].Rule != RuleJ3FlakySame {
		t.Fatalf("verdict %q, finding %q; want FLAKY by %s", r.Verdict, r.Jobs[0].Rule, RuleJ3FlakySame)
	}
	if r.Jobs[0].Attempts != 2 || r.Jobs[0].Failed != 1 || r.Jobs[0].Succeeded != 1 {
		t.Errorf("finding %+v, want 2 attempts: 1 failed, 1 succeeded", r.Jobs[0])
	}
	if st, _ := StateFor(r.Verdict); st != StateFlaky {
		t.Errorf("state %q, want flaky", st)
	}
}

// "A read failure is never a verdict (UNKNOWN -> look again; bounded by the
// deadline -> UNVERIFIABLE)."
func TestAReadFailureIsNotAVerdict(t *testing.T) {
	unread := failedWith(9002, "unit-tests", "")
	unread.Trace = Trace{}
	f := goodFacts()
	setJob(&f, "unit-tests", unread)
	redPipeline(&f)

	r := Judge(f, tEarly)
	if r.Decision != DecisionCannotJudge || r.Verdict != "" || r.State != "" {
		t.Fatalf("a failed attempt whose log could not be read: decision %q verdict %q state %q; want cannot-judge and nothing else",
			r.Decision, r.Verdict, r.State)
	}
	if r.Rule != RuleJ3Unknown {
		t.Errorf("rule %q, want %s", r.Rule, RuleJ3Unknown)
	}
	if _, reported := VerdictOutcome(r.Verdict, 1000); reported {
		t.Errorf("a result with no verdict reports a meter outcome")
	}
	late := Judge(f, tLate)
	if late.Verdict != VerdictUnverifiable || late.Rule != RuleJ3Unread {
		t.Fatalf("past the deadline: verdict %q by %q; want UNVERIFIABLE by %s", late.Verdict, late.Rule, RuleJ3Unread)
	}
}

func TestFailedTrace(t *testing.T) {
	log := []byte("$ make check\nrunning 14 tests\n" + lineOld + "\nmore output\n" + lineGeneric + "\n")
	tr := FailedTrace(LogRead{Data: log})
	if !tr.Read || tr.FirstError != pipelinectx.Excerpt(log).FirstError || tr.FirstError != lineOld {
		t.Fatalf("FailedTrace = %+v, want read, first error %q", tr, lineOld)
	}
	if e := FailedTrace(LogRead{}); !e.Read || e.FirstError != "" {
		t.Errorf("FailedTrace of no bytes = %+v: an EMPTY log that was read is read", e)
	}
	// and through Judge: the log above is the recorded failure.
	f := goodFacts()
	a := failedWith(9002, "unit-tests", "")
	a.Trace = tr
	setJob(&f, "unit-tests", a)
	redPipeline(&f)
	if r := Judge(f, tEarly); r.Verdict != VerdictNotFixed {
		t.Errorf("verdict %q, want NOT_FIXED", r.Verdict)
	}
}

// TestFailedTraceHonoursTheTruncationFlags (gonk-etvm). A log that was not
// read whole must give the SAME first error the record's signature was
// computed from (pipelinectx.ExcerptWith with the same flags), or no first
// error at all -- never one taken from a cut line.
func TestFailedTraceHonoursTheTruncationFlags(t *testing.T) {
	// The read began mid-line: the first "line" is the END of a line whose
	// beginning was never read, and it happens to look like an error.
	fragment := "rror: 3 widgets failed to render\n"
	rest := "running 14 tests\n" + lineOld + "\nmore output\n"
	cut := []byte(fragment + rest)

	t.Run("head truncated: the cut first line is never the first error", func(t *testing.T) {
		tr := FailedTrace(LogRead{Data: cut, HeadTruncated: true})
		want := pipelinectx.ExcerptWith(cut, pipelinectx.Options{HeadTruncated: true}).FirstError
		if !tr.Read || tr.FirstError != lineOld || tr.FirstError != want {
			t.Fatalf("FailedTrace = %+v, want read with first error %q (the record's: %q)", tr, lineOld, want)
		}
		// The same bytes WITHOUT the flag are a whole log, and its first line
		// is its first error: the flag, not the bytes, is what decides.
		whole := FailedTrace(LogRead{Data: cut})
		if !whole.Read || whole.FirstError == lineOld || whole.FirstError == "" {
			t.Fatalf("FailedTrace without the flag = %+v, want the first line as first error", whole)
		}
	})
	t.Run("head truncated: through Judge it is the recorded failure, not a new one", func(t *testing.T) {
		f := goodFacts()
		a := failedWith(9002, "unit-tests", "")
		a.Trace = FailedTrace(LogRead{Data: cut, HeadTruncated: true})
		setJob(&f, "unit-tests", a)
		redPipeline(&f)
		if r := Judge(f, tEarly); r.Verdict != VerdictNotFixed || r.Rule != RuleJ3Recurs {
			t.Errorf("verdict %q by %q, want NOT_FIXED by %s", r.Verdict, r.Rule, RuleJ3Recurs)
		}
	})
	t.Run("head truncated, one line and no newline: only a fragment was read", func(t *testing.T) {
		// Everything read is the tail of one cut line. pipelinectx keeps it
		// (there is no newline to cut at); it is still not a line anybody
		// read from its start, so it establishes nothing.
		tr := FailedTrace(LogRead{Data: []byte("rror: 3 widgets failed to render"), HeadTruncated: true})
		if tr != (Trace{}) {
			t.Errorf("FailedTrace = %+v, want the zero Trace (not read)", tr)
		}
	})
	t.Run("head truncated, nothing after the cut line", func(t *testing.T) {
		tr := FailedTrace(LogRead{Data: []byte(fragment), HeadTruncated: true})
		if !tr.Read || tr.FirstError != "" {
			t.Errorf("FailedTrace = %+v, want read with no first error (generic: the conservative side)", tr)
		}
	})
	t.Run("a whole log of one line with no newline is read", func(t *testing.T) {
		// Not truncated: the one line was read from its start, and it is the
		// log. Only the FLAG makes a newline-less read a fragment.
		if tr := FailedTrace(LogRead{Data: []byte(lineOld)}); !tr.Read || tr.FirstError != lineOld {
			t.Errorf("FailedTrace = %+v, want read with first error %q", tr, lineOld)
		}
	})
	t.Run("an empty read is an empty log, whatever the flags", func(t *testing.T) {
		// HeadTruncated with no bytes cannot happen (bytes precede nothing);
		// it is still not "a fragment", and an empty log that was read is read.
		if tr := FailedTrace(LogRead{HeadTruncated: true}); !tr.Read || tr.FirstError != "" {
			t.Errorf("FailedTrace = %+v, want read with no first error", tr)
		}
	})
	t.Run("tail truncated: the end of the log was not read, so the log was not read", func(t *testing.T) {
		for _, l := range []LogRead{
			{Data: []byte(rest), TailTruncated: true},
			{Data: cut, HeadTruncated: true, TailTruncated: true},
			{TailTruncated: true},
		} {
			if tr := FailedTrace(l); tr != (Trace{}) {
				t.Errorf("FailedTrace(head=%v tail=%v, %d bytes) = %+v, want the zero Trace", l.HeadTruncated, l.TailTruncated, len(l.Data), tr)
			}
		}
		// and through Judge: no verdict before the deadline.
		f := goodFacts()
		a := failedWith(9002, "unit-tests", "")
		a.Trace = FailedTrace(LogRead{Data: []byte(lineNew + "\n"), TailTruncated: true})
		setJob(&f, "unit-tests", a)
		redPipeline(&f)
		if r := Judge(f, tEarly); r.Decision != DecisionCannotJudge || r.Rule != RuleJ3Unknown {
			t.Errorf("decision %q by %q, want cannot-judge by %s", r.Decision, r.Rule, RuleJ3Unknown)
		}
	})
}

// ---- J4: every other job, latest attempt, against the base pipeline ----

func TestJ4OtherJobs(t *testing.T) {
	failOther := func(name string, base string, allow bool) func(*JudgeFacts) {
		return func(f *JudgeFacts) {
			var jobs []BaseJob
			for _, b := range f.Fix.BaseJobs {
				if b.Name != name {
					jobs = append(jobs, b)
				}
			}
			if base != "" {
				jobs = append(jobs, BaseJob{Name: name, Status: base})
			}
			f.Fix.BaseJobs = jobs
			a := failedWith(9003, name, lineNew)
			a.AllowFailure = allow
			setJob(f, name, a)
			if !allow {
				redPipeline(f)
			}
		}
	}
	runJudgeCases(t, []judgeCase{
		{name: "regressed: passed on the base, fails now", mut: failOther("lint", StatusSuccess, false),
			decision: DecisionVerdict, verdict: VerdictRegressed, state: StateRegressed, rule: RuleJ4Regressed,
			job: RuleJ3Passed, other: RuleJ4Regressed},
		{name: "regressed, but allowed to fail: not REGRESSED, and not FIXED", mut: failOther("lint", StatusSuccess, true),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ4RegressedAllowed,
			job: RuleJ3Passed, other: RuleJ4RegressedAllowed},
		{name: "remaining: failed on the base (uncited), fails now", mut: failOther("lint", StatusFailed, false),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ4Remaining,
			job: RuleJ3Passed, other: RuleJ4Remaining},
		{name: "remaining: and is allowed to fail", mut: failOther("lint", StatusFailed, true),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ4Remaining,
			job: RuleJ3Passed, other: RuleJ4Remaining},
		{name: "revealed: skipped on the base", mut: failOther("package", StatusSkipped, false),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ4Revealed,
			job: RuleJ3Passed, other: RuleJ4Revealed},
		{name: "revealed: manual on the base", mut: failOther("package", "manual", false),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ4Revealed,
			job: RuleJ3Passed, other: RuleJ4Revealed},
		{name: "revealed: canceled on the base", mut: failOther("package", StatusCanceled, false),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ4Revealed,
			job: RuleJ3Passed, other: RuleJ4Revealed},
		{name: "revealed: absent from the base", mut: failOther("package", "", false),
			decision: DecisionVerdict, verdict: VerdictFixedNewFailure, state: StateFixedNewFailure, rule: RuleJ4Revealed,
			job: RuleJ3Passed, other: RuleJ4Revealed},
		{name: "latest attempt: another job failed and was retried into a pass -- nothing found",
			mut: func(f *JudgeFacts) {
				setJob(f, "lint", failedWith(9003, "lint", lineNew), ok(9060, "lint", "test"))
			},
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "latest attempt: another job passed and then failed on a later attempt",
			mut: func(f *JudgeFacts) {
				setJob(f, "lint", ok(9003, "lint", "test"), failedWith(9060, "lint", lineNew))
				redPipeline(f)
			},
			decision: DecisionVerdict, verdict: VerdictRegressed, state: StateRegressed, rule: RuleJ4Regressed,
			job: RuleJ3Passed, other: RuleJ4Regressed},
		{name: "latest attempt is by job id, not by position in the listing",
			mut: func(f *JudgeFacts) {
				setJob(f, "lint", failedWith(9060, "lint", lineNew), ok(9003, "lint", "test"))
				redPipeline(f)
			},
			decision: DecisionVerdict, verdict: VerdictRegressed, state: StateRegressed, rule: RuleJ4Regressed,
			job: RuleJ3Passed, other: RuleJ4Regressed},
		{name: "another job that is merely skipped now is nothing",
			mut: func(f *JudgeFacts) {
				setJob(f, "lint", Attempt{ID: 9003, Name: "lint", Stage: "test", Status: StatusSkipped})
			},
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "a regression in ONE of two candidate pipelines decides",
			mut: func(f *JudgeFacts) {
				p := secondPipeline(f)
				p.Status = StatusFailed
				p.Jobs[2] = failedWith(9103, "lint", lineNew)
			},
			decision: DecisionVerdict, verdict: VerdictRegressed, state: StateRegressed, rule: RuleJ4Regressed,
			job: RuleJ3Passed, other: RuleJ4Regressed},
		{name: "allowed to fail in one candidate, required in the other: REGRESSED",
			mut: func(f *JudgeFacts) {
				a := failedWith(9003, "lint", lineNew)
				a.AllowFailure = true
				setJob(f, "lint", a)
				p := secondPipeline(f)
				p.Status = StatusFailed
				p.Jobs[2] = failedWith(9103, "lint", lineNew)
			},
			decision: DecisionVerdict, verdict: VerdictRegressed, state: StateRegressed, rule: RuleJ4Regressed,
			job: RuleJ3Passed, other: RuleJ4Regressed},
	})
}

func TestJ4FindingsAreSortedAndNameTheirPipeline(t *testing.T) {
	f := goodFacts()
	setJob(&f, "package", failedWith(9004, "package", lineNew))
	setJob(&f, "lint", failedWith(9003, "lint", lineNew))
	redPipeline(&f)
	r := Judge(f, tEarly)
	want := []OtherFinding{
		{Job: "lint", Rule: RuleJ4Regressed, PipelineID: 501, BaseStatus: StatusSuccess},
		{Job: "package", Rule: RuleJ4Revealed, PipelineID: 501, BaseStatus: StatusSkipped},
	}
	if !reflect.DeepEqual(r.Others, want) {
		t.Fatalf("others = %+v\nwant     %+v", r.Others, want)
	}
	if r.Verdict != VerdictRegressed || r.Rule != RuleJ4Regressed {
		t.Errorf("verdict %q by %q, want REGRESSED by %s", r.Verdict, r.Rule, RuleJ4Regressed)
	}

	// A finding names the first candidate pipeline (by id) that shows it:
	// for REGRESSED, the first in which the failure was not allowed.
	g := goodFacts()
	allowed := failedWith(9003, "lint", lineNew)
	allowed.AllowFailure = true
	setJob(&g, "lint", allowed) // 501: fails, allowed
	p := secondPipeline(&g)
	p.Status = StatusFailed
	p.Jobs[2] = failedWith(9103, "lint", lineNew)    // 502: fails, required
	p.Jobs[3] = failedWith(9104, "package", lineNew) // 502 only
	g.Pipelines.Pipelines[0], g.Pipelines.Pipelines[1] = g.Pipelines.Pipelines[1], g.Pipelines.Pipelines[0]
	want = []OtherFinding{
		{Job: "lint", Rule: RuleJ4Regressed, PipelineID: 502, BaseStatus: StatusSuccess},
		{Job: "package", Rule: RuleJ4Revealed, PipelineID: 502, BaseStatus: StatusSkipped},
	}
	if r := Judge(g, tEarly); !reflect.DeepEqual(r.Others, want) {
		t.Errorf("others = %+v\nwant     %+v", r.Others, want)
	}
	h := goodFacts()
	setJob(&h, "lint", allowed)
	q := secondPipeline(&h)
	a2 := failedWith(9103, "lint", lineNew)
	a2.AllowFailure = true
	q.Jobs[2] = a2
	want = []OtherFinding{{Job: "lint", Rule: RuleJ4RegressedAllowed, PipelineID: 501, BaseStatus: StatusSuccess}}
	if r := Judge(h, tEarly); !reflect.DeepEqual(r.Others, want) {
		t.Errorf("others = %+v\nwant     %+v", r.Others, want)
	}
}

// ---- J5: the verdict is the FIRST match, in this order ----

func TestJ5FirstMatchOrder(t *testing.T) {
	// Each ingredient produces one kind of finding without disturbing the
	// others: the first diagnosed job, a second diagnosed job, another job.
	weakened := func(f *JudgeFacts) {
		a := ok(9005, "integration", "test")
		a.AllowFailure = true
		setJob(f, "integration", a)
	}
	forgedDef := func(f *JudgeFacts) {
		withLift(f)
		f.Definition.Delta = cidef.Delta{Kind: cidef.KindViolation,
			Violations: []cidef.Violation{{Rule: cidef.RuleD1, Job: "lint"}}}
	}
	recurs := func(f *JudgeFacts) {
		setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineOld))
		redPipeline(f)
	}
	flaky := func(f *JudgeFacts) {
		setJob(f, "integration", failedWith(9005, "integration", lineNew), ok(9070, "integration", "test"))
	}
	notObserved := func(f *JudgeFacts) {
		setJob(f, "integration", Attempt{ID: 9005, Name: "integration", Stage: "test", Status: StatusSkipped})
	}
	unresolvable := func(f *JudgeFacts) {
		withLift(f)
		f.Definition.Delta = cidef.Delta{Kind: cidef.KindUnresolvable,
			Unresolvable: []cidef.Unresolvable{{Side: cidef.SideBoth, Job: "package", Key: "script", Reason: "!reference"}}}
	}
	regressed := func(f *JudgeFacts) {
		setJob(f, "lint", failedWith(9003, "lint", lineNew))
		redPipeline(f)
	}
	changed := func(f *JudgeFacts) {
		setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineNew))
		redPipeline(f)
	}
	revealed := func(f *JudgeFacts) {
		setJob(f, "package", failedWith(9004, "package", lineNew))
		redPipeline(f)
	}
	cases := []struct {
		name    string
		muts    []func(*JudgeFacts)
		verdict Verdict
		rule    Rule
	}{
		{"FORGED (weakened) over NOT_FIXED", []func(*JudgeFacts){weakened, recurs}, VerdictForged, RuleJ3Weakened},
		{"FORGED (definition) over NOT_FIXED", []func(*JudgeFacts){forgedDef, recurs}, VerdictForged, RuleJ2Violation},
		{"FORGED (definition) over a weakened job: J2 is named first", []func(*JudgeFacts){forgedDef, weakened}, VerdictForged, RuleJ2Violation},
		{"FORGED over everything", []func(*JudgeFacts){weakened, recurs, regressed, revealed}, VerdictForged, RuleJ3Weakened},
		{"NOT_FIXED over FLAKY", []func(*JudgeFacts){recurs, flaky}, VerdictNotFixed, RuleJ3Recurs},
		{"NOT_FIXED over UNVERIFIABLE (a job not observed)", []func(*JudgeFacts){recurs, notObserved}, VerdictNotFixed, RuleJ3Recurs},
		{"NOT_FIXED over UNVERIFIABLE (the definition)", []func(*JudgeFacts){recurs, unresolvable}, VerdictNotFixed, RuleJ3Recurs},
		{"NOT_FIXED over REGRESSED", []func(*JudgeFacts){recurs, regressed}, VerdictNotFixed, RuleJ3Recurs},
		{"FLAKY over UNVERIFIABLE (the definition)", []func(*JudgeFacts){flaky, unresolvable}, VerdictFlaky, RuleJ3FlakySame},
		{"FLAKY over REGRESSED", []func(*JudgeFacts){flaky, regressed}, VerdictFlaky, RuleJ3FlakySame},
		{"FLAKY over FIXED_NEW_FAILURE", []func(*JudgeFacts){flaky, changed}, VerdictFlaky, RuleJ3FlakySame},
		{"UNVERIFIABLE (a job not observed) over REGRESSED", []func(*JudgeFacts){notObserved, regressed}, VerdictUnverifiable, RuleJ3Status},
		{"UNVERIFIABLE (the definition) over REGRESSED", []func(*JudgeFacts){unresolvable, regressed}, VerdictUnverifiable, RuleJ2Unresolvable},
		{"UNVERIFIABLE over FIXED_NEW_FAILURE", []func(*JudgeFacts){notObserved, changed}, VerdictUnverifiable, RuleJ3Status},
		{"REGRESSED over FIXED_NEW_FAILURE (changed)", []func(*JudgeFacts){regressed, changed}, VerdictRegressed, RuleJ4Regressed},
		{"REGRESSED over FIXED_NEW_FAILURE (revealed)", []func(*JudgeFacts){regressed, revealed}, VerdictRegressed, RuleJ4Regressed},
		{"FIXED_NEW_FAILURE (changed) over FIXED", []func(*JudgeFacts){changed}, VerdictFixedNewFailure, RuleJ3Changed},
		{"FIXED_NEW_FAILURE (revealed) over FIXED", []func(*JudgeFacts){revealed}, VerdictFixedNewFailure, RuleJ4Revealed},
		{"FIXED_NEW_FAILURE: a changed diagnosed job is named before another job", []func(*JudgeFacts){revealed, changed}, VerdictFixedNewFailure, RuleJ3Changed},
		{"FIXED", nil, VerdictFixed, RuleJ5Fixed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := goodFacts()
			secondTarget(&f)
			for _, m := range c.muts {
				m(&f)
			}
			r := Judge(f, tEarly)
			if r.Decision != DecisionVerdict || r.Verdict != c.verdict || r.Rule != c.rule {
				t.Fatalf("decision %q verdict %q by %q (%s); want %q by %q", r.Decision, r.Verdict, r.Rule, r.Detail, c.verdict, c.rule)
			}
		})
	}
}

// Past the deadline what is unsettled or unread is UNVERIFIABLE -- and J5's
// order still holds: a forgery or a recurrence that IS visible outranks it.
func TestPastTheDeadlineTheOrderStillHolds(t *testing.T) {
	running := func(f *JudgeFacts) { secondPipeline(f).Status = "running" }
	cases := []struct {
		name    string
		muts    []func(*JudgeFacts)
		verdict Verdict
		rule    Rule
	}{
		{"unsettled, a definition violation", []func(*JudgeFacts){running, func(f *JudgeFacts) {
			withLift(f)
			f.Definition.Delta = cidef.Delta{Kind: cidef.KindViolation}
		}}, VerdictForged, RuleJ2Violation},
		{"no pipeline at all, a definition violation", []func(*JudgeFacts){func(f *JudgeFacts) {
			f.Pipelines.Pipelines = nil
			withLift(f)
			f.Definition.Delta = cidef.Delta{Kind: cidef.KindViolation}
		}}, VerdictForged, RuleJ2Violation},
		{"unsettled, a recurrence already visible", []func(*JudgeFacts){running, func(f *JudgeFacts) {
			pipe(f, 502).Jobs[1].Status = "running" // unit-tests has not finished there
			setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineOld))
			redPipeline(f)
		}}, VerdictNotFixed, RuleJ3Recurs},
		{"unsettled, another job already regressed: not REGRESSED, the pipelines never settled", []func(*JudgeFacts){running, func(f *JudgeFacts) {
			setJob(f, "lint", failedWith(9003, "lint", lineNew))
			redPipeline(f)
		}}, VerdictUnverifiable, RuleJ1Timeout},
		{"the definition unread, a recurrence visible", []func(*JudgeFacts){func(f *JudgeFacts) {
			f.Definition.Read = false
			setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineOld))
			redPipeline(f)
		}}, VerdictNotFixed, RuleJ3Recurs},
		{"jobs unread, a recurrence in the part that was read: not provable, UNVERIFIABLE", []func(*JudgeFacts){func(f *JudgeFacts) {
			secondPipeline(f).JobsRead = false
			setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineOld))
			redPipeline(f)
		}}, VerdictUnverifiable, RuleJ3Unread},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := goodFacts()
			for _, m := range c.muts {
				m(&f)
			}
			early := Judge(f, tEarly)
			if early.Decision == DecisionVerdict {
				t.Fatalf("before the deadline: verdict %q by %q; want none", early.Verdict, early.Rule)
			}
			r := Judge(f, tLate)
			if r.Verdict != c.verdict || r.Rule != c.rule {
				t.Fatalf("verdict %q by %q (%s); want %q by %q", r.Verdict, r.Rule, r.Detail, c.verdict, c.rule)
			}
		})
	}
}

// "Pipeline green appears nowhere above FIXED's last clause."
func TestPipelineGreenIsNotTheCriterion(t *testing.T) {
	t.Run("the criterion is met on a red pipeline", func(t *testing.T) {
		f := goodFacts()
		setJob(&f, "package", failedWith(9004, "package", lineNew))
		redPipeline(&f)
		r := Judge(f, tEarly)
		if r.Verdict != VerdictFixedNewFailure || r.State != StateFixedNewFailure {
			t.Fatalf("verdict %q state %q, want FIXED_NEW_FAILURE", r.Verdict, r.State)
		}
		if out, _ := VerdictOutcome(r.Verdict, 1000); out != "success" {
			t.Errorf("meter outcome %q, want success", out)
		}
	})
	t.Run("a green pipeline is not the criterion met", func(t *testing.T) {
		f := goodFacts()
		a := failedWith(9002, "unit-tests", lineOld)
		a.AllowFailure = true // green, with a warning
		setJob(&f, "unit-tests", a)
		if r := Judge(f, tEarly); r.Verdict != VerdictForged {
			t.Fatalf("verdict %q on a green pipeline whose diagnosed job was allowed to fail, want FORGED", r.Verdict)
		}
	})
	for _, s := range []string{StatusFailed, StatusCanceled, StatusSkipped} {
		t.Run("FIXED needs every candidate to be success: "+s, func(t *testing.T) {
			f := goodFacts()
			pipe(&f, 501).Status = s // nothing in the job list explains it
			r := Judge(f, tEarly)
			if r.Verdict != VerdictUnverifiable || r.Rule != RuleJ5NotSuccess {
				t.Fatalf("verdict %q by %q, want UNVERIFIABLE by %s", r.Verdict, r.Rule, RuleJ5NotSuccess)
			}
		})
	}
	t.Run("one of two candidates not success", func(t *testing.T) {
		f := goodFacts()
		secondPipeline(&f).Status = StatusCanceled
		r := Judge(f, tEarly)
		if r.Verdict != VerdictUnverifiable || r.Rule != RuleJ5NotSuccess {
			t.Fatalf("verdict %q by %q, want UNVERIFIABLE by %s", r.Verdict, r.Rule, RuleJ5NotSuccess)
		}
	})
}

// ---- the property the whole package serves ----

// No fact that was not read, alone or with any other, ever produces a pass.
// Every combination of the "not read" switches, on a fix that would
// otherwise be FIXED and on one that would be FIXED_NEW_FAILURE: before the
// deadline there is no verdict at all; past it, never FIXED or
// FIXED_NEW_FAILURE.
func TestNoUnreadFactIsEverAPass(t *testing.T) {
	switches := []struct {
		name string
		mut  func(*JudgeFacts)
	}{
		{"mr", func(f *JudgeFacts) { f.MR.Read = false }},
		{"pipelines", func(f *JudgeFacts) { f.Pipelines.Read = false }},
		{"jobs", func(f *JudgeFacts) { f.Pipelines.Pipelines[0].JobsRead = false }},
		{"retried", func(f *JudgeFacts) { f.Pipelines.Pipelines[0].IncludeRetried = false }},
		{"definition", func(f *JudgeFacts) { f.Definition.Read = false }},
		{"record", func(f *JudgeFacts) { f.Fix.Recorded = false }},
		{"trace", func(f *JudgeFacts) {
			a := failedWith(9002, "unit-tests", "")
			a.Trace = Trace{}
			setJob(f, "unit-tests", a)
			redPipeline(f)
		}},
	}
	bases := map[string]func() JudgeFacts{
		"FIXED": goodFacts,
		"FIXED_NEW_FAILURE": func() JudgeFacts {
			f := goodFacts()
			setJob(&f, "package", failedWith(9004, "package", lineNew))
			redPipeline(&f)
			return f
		},
	}
	for baseName, base := range bases {
		for mask := 1; mask < 1<<len(switches); mask++ {
			f := base()
			var names []string
			for i, s := range switches {
				if mask&(1<<i) != 0 {
					s.mut(&f)
					names = append(names, s.name)
				}
			}
			label := baseName + " without " + strings.Join(names, "+")
			if r := Judge(f, tEarly); r.Decision == DecisionVerdict || r.Verdict != "" {
				t.Errorf("%s, before the deadline: verdict %q by %q; want none", label, r.Verdict, r.Rule)
			}
			if r := Judge(f, tLate); r.Verdict == VerdictFixed || r.Verdict == VerdictFixedNewFailure {
				t.Errorf("%s, past the deadline: verdict %q by %q; an unread fact became a pass", label, r.Verdict, r.Rule)
			}
		}
	}
}

func TestJudgeDoesNotModifyItsFacts(t *testing.T) {
	f := goodFacts()
	secondTarget(&f)
	p := secondPipeline(&f)
	p.Jobs[1] = failedWith(9102, "unit-tests", lineNew)
	p.Status = StatusFailed
	g := goodFacts()
	secondTarget(&g)
	q := secondPipeline(&g)
	q.Jobs[1] = failedWith(9102, "unit-tests", lineNew)
	q.Status = StatusFailed
	first := Judge(f, tEarly)
	if !reflect.DeepEqual(f, g) {
		t.Fatalf("Judge modified its facts")
	}
	if second := Judge(f, tEarly); !reflect.DeepEqual(first, second) {
		t.Errorf("two calls on the same facts differ:\n%+v\n%+v", first, second)
	}
}

func TestAChangedFailureCarriesItsNewSignature(t *testing.T) {
	f := goodFacts()
	setJob(&f, "unit-tests", failedWith(9002, "unit-tests", lineNew))
	redPipeline(&f)
	r := Judge(f, tEarly)
	if len(r.Jobs) != 1 {
		t.Fatalf("job findings %+v; want one", r.Jobs)
	}
	if want := pipelinectx.ErrorSignature(lineNew); r.Jobs[0].NewSignature != want || want == "" {
		t.Errorf("NewSignature = %q, want %q", r.Jobs[0].NewSignature, want)
	}
	g := goodFacts()
	setJob(&g, "unit-tests", failedWith(9002, "unit-tests", lineOld))
	redPipeline(&g)
	if r := Judge(g, tEarly); len(r.Jobs) != 1 || r.Jobs[0].NewSignature != "" {
		t.Errorf("a recurrence: findings %+v, want one with no new signature", r.Jobs)
	}
	// one attempt a new way, one the old way: RECURS, and no "new" signature
	// is offered for a failure that is not gone.
	setJob(&g, "unit-tests", failedWith(9002, "unit-tests", lineNew), failedWith(9010, "unit-tests", lineOld))
	if r := Judge(g, tEarly); len(r.Jobs) != 1 || r.Jobs[0].Rule != RuleJ3Recurs || r.Jobs[0].NewSignature != "" {
		t.Errorf("a recurrence among other failures: findings %+v, want RECURS with no new signature", r.Jobs)
	}
	// failed a new way, then passed: FLAKY, and the new signature is shown.
	setJob(&g, "unit-tests", failedWith(9002, "unit-tests", lineNew), ok(9010, "unit-tests", "test"))
	if r := Judge(g, tEarly); len(r.Jobs) != 1 || r.Jobs[0].NewSignature != pipelinectx.ErrorSignature(lineNew) {
		t.Errorf("flaky with a new failure: findings %+v, want the new signature", r.Jobs)
	}
}

func TestTheFixtureLinesAreWhatTheTestsAssume(t *testing.T) {
	old, variant, other := pipelinectx.ErrorSignature(lineOld), pipelinectx.ErrorSignature(lineOldVar), pipelinectx.ErrorSignature(lineNew)
	if old == "" || other == "" || old == other || old != variant {
		t.Fatalf("signatures: old %q variant %q new %q; want old == variant != new, none empty", old, variant, other)
	}
	if pipelinectx.GenericFirstError(lineOld) || pipelinectx.GenericFirstError(lineNew) || !pipelinectx.GenericFirstError(lineGeneric) {
		t.Fatalf("generic flags are not what the fixtures assume")
	}
	// Each is a line pipelinectx's excerpt picks as the first error.
	for _, l := range []string{lineOld, lineOldVar, lineNew, lineGeneric} {
		if got := pipelinectx.Excerpt([]byte("$ make check\n" + l + "\n")).FirstError; got != l {
			t.Errorf("first error of a log holding %q = %q", l, got)
		}
	}
}
