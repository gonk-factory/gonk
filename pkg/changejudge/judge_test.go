package changejudge

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/gate"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

var (
	tDispatch = time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	tDeadline = tDispatch.Add(2 * time.Hour)
	tEarly    = tDispatch.Add(10 * time.Minute) // before the deadline
	tLate     = tDeadline.Add(time.Minute)      // past it
)

func ok(id int64, name string) Attempt {
	return Attempt{ID: id, Name: name, Status: StatusSuccess}
}

func failed(id int64, name string) Attempt {
	return Attempt{ID: id, Name: name, Status: StatusFailed}
}

func withStatus(id int64, name, status string) Attempt {
	return Attempt{ID: id, Name: name, Status: status}
}

var newCase = Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_two_crates", Status: CaseSuccess}

// headCases: the base's cases and the one the change adds, all passing.
func headCases() []Case { return append(baseCases(), newCase) }

// goodFacts is a change that is GREEN: a `source` and a `test` path changed
// on a project that is not Go; one merge request pipeline of gonk's commit,
// green on its only attempts; the report holds every base case and a new one.
func goodFacts() JudgeFacts {
	return JudgeFacts{
		Change: ChangeFacts{
			Recorded:        true,
			BaseSHA:         shaBase,
			ChangeCommitSHA: shaChange,
			BasePipelineID:  300,
			MRIID:           41,
			DiffClasses:     []string{ClassSource, ClassTest},
			BaseReport:      report(baseCases()...),
			VerifyDeadline:  tDeadline,
		},
		MR: MRFacts{Read: true, IID: 41, State: MRStateOpened, SHA: shaChange},
		Pipelines: PipelineFacts{Read: true, Pipelines: []Pipeline{{
			ID: 501, SHA: shaChange, Source: "merge_request_event", Status: StatusSuccess,
			JobsRead: true, IncludeRetried: true,
			Jobs:   []Attempt{ok(9001, "compile"), ok(9002, "unit-tests"), ok(9003, "lint")},
			Report: report(headCases()...),
		}}},
		Definition: DefinitionFacts{Read: true, Delta: cidef.Delta{Kind: cidef.KindNone}},
	}
}

// goFacts is goodFacts on a project with the `go` profile: the change adds
// TestTallyTwoCrates in crates/, and the report (as gotestsum writes it:
// classname = the package's import path) shows it passing.
func goFacts() JudgeFacts {
	f := goodFacts()
	base := []Case{
		{Suite: "go", Classname: "example.test/depot/crates", Name: "TestTallyEmpty", Status: CaseSuccess},
		{Suite: "go", Classname: "example.test/depot/crates", Name: "TestTallyOneCrate", Status: CaseSuccess},
		{Suite: "go", Classname: "example.test/depot/labels", Name: "TestWidth", Status: CaseSuccess},
	}
	f.Change.GoProfile = true
	f.Change.AddedGoTests = []GoTest{{Dir: "crates", Name: "TestTallyTwoCrates"}}
	f.Change.BaseReport = report(base...)
	pipe(&f, 501).Report = report(append(append([]Case(nil), base...),
		Case{Suite: "go", Classname: "example.test/depot/crates", Name: "TestTallyTwoCrates", Status: CaseSuccess})...)
	return f
}

func pipe(f *JudgeFacts, id int64) *Pipeline {
	for i := range f.Pipelines.Pipelines {
		if f.Pipelines.Pipelines[i].ID == id {
			return &f.Pipelines.Pipelines[i]
		}
	}
	panic("no such pipeline in the fixture")
}

// setJob replaces the attempts of one job in pipeline 501.
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

func redPipeline(f *JudgeFacts) { pipe(f, 501).Status = StatusFailed }

// secondPipeline adds a second candidate (a branch pipeline of the same
// commit), green, with the same report, and returns it.
func secondPipeline(f *JudgeFacts) *Pipeline {
	first := pipe(f, 501)
	f.Pipelines.Pipelines = append(f.Pipelines.Pipelines, Pipeline{
		ID: 502, SHA: shaChange, Source: "push", Status: StatusSuccess,
		JobsRead: true, IncludeRetried: true,
		Jobs:   []Attempt{ok(9101, "compile"), ok(9102, "unit-tests"), ok(9103, "lint")},
		Report: report(first.Report.Cases...),
	})
	return pipe(f, 502)
}

// setHead replaces pipeline 501's report.
func setHead(f *JudgeFacts, cases ...Case) { pipe(f, 501).Report = report(cases...) }

// headWith is the base's cases with one of them changed, plus the new case.
func headWith(i int, change func(*Case)) []Case {
	cases := headCases()
	change(&cases[i])
	return cases
}

func definitionChanged(f *JudgeFacts) { f.Definition.Delta = cidef.Delta{Kind: cidef.KindPreserving} }
func retriedGreen(f *JudgeFacts) {
	setJob(f, "unit-tests", failed(9002, "unit-tests"), ok(9012, "unit-tests"))
}
func noNewCase(f *JudgeFacts) { setHead(f, baseCases()...) }

type judgeCase struct {
	name string
	// plan: the tokens of the plan's K block this fixture is the evidence
	// for, as "<step>:<token>" (TestEveryPlanRowHasAFixture reads them).
	plan []string
	base func() JudgeFacts // goodFacts when nil
	mut  func(*JudgeFacts)
	late bool // judged past the verify deadline

	decision Decision
	verdict  Verdict
	state    State
	rule     Rule
	// job, jobRule: the finding for one job ("" = not asserted; job defaults
	// to unit-tests).
	job     string
	jobRule Rule
	// tests: Result.Tests.Rule ("" = not asserted).
	tests Rule
	// missing: a substring one entry of Result.Missing must hold.
	missing string
}

func runJudgeCases(t *testing.T, cases []judgeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := goodFacts
			if c.base != nil {
				base = c.base
			}
			f := base()
			if c.mut != nil {
				c.mut(&f)
			}
			now := tEarly
			if c.late {
				now = tLate
			}
			checkResult(t, Judge(f, now), c)
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
	if c.jobRule != "" {
		name := c.job
		if name == "" {
			name = "unit-tests"
		}
		var got Rule
		for _, j := range r.Jobs {
			if j.Job == name {
				got = j.Rule
			}
		}
		if got != c.jobRule {
			t.Errorf("finding for %s = %q, want %q (all: %+v)", name, got, c.jobRule, r.Jobs)
		}
	}
	if c.tests != "" && r.Tests.Rule != c.tests {
		t.Errorf("tests = %+v, want rule %q", r.Tests, c.tests)
	}
	if c.decision == DecisionVerdict {
		// Result.Definition says "compared and unchanged", and nothing else.
		wantDef := cidef.KindNone
		if strings.HasPrefix(string(c.rule), "K2.") {
			wantDef = ""
		}
		if c.verdict == VerdictGreen || c.verdict == VerdictGreenUnobserved || strings.HasPrefix(string(c.rule), "K2.") {
			if r.Definition != wantDef {
				t.Errorf("definition = %q, want %q", r.Definition, wantDef)
			}
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

func TestTheBaselineChangeIsGreen(t *testing.T) {
	for name, base := range map[string]func() JudgeFacts{"not go": goodFacts, "go": goFacts} {
		t.Run(name, func(t *testing.T) {
			r := Judge(base(), tEarly)
			checkResult(t, r, judgeCase{decision: DecisionVerdict, verdict: VerdictGreen, state: StateVerified,
				rule: RuleK5Green, tests: RuleK4Observed})
			if !reflect.DeepEqual(r.Candidates, []int64{501}) {
				t.Errorf("candidates = %v, want [501]", r.Candidates)
			}
			if len(r.Jobs) != 0 || r.Tests.Finding != TestsObserved {
				t.Errorf("jobs = %+v tests = %+v; want no job finding and TESTS_OBSERVED", r.Jobs, r.Tests)
			}
			// Time is not a fact once every fact is in.
			if late := Judge(base(), tLate); late.Verdict != VerdictGreen {
				t.Errorf("past the deadline: %q by %q", late.Verdict, late.Rule)
			}
		})
	}
}

// ---- K0: identity ----

var k0Cases = []judgeCase{
	{name: "the MR was closed", plan: []string{"K0:closed-unmerged"},
		mut:      func(f *JudgeFacts) { f.MR.State = MRStateClosed },
		decision: DecisionStopped, state: StateClosedUnmerged, rule: RuleK0Closed},
	{name: "the MR was merged", plan: []string{"K0:merged"},
		mut:      func(f *JudgeFacts) { f.MR.State = MRStateMerged },
		decision: DecisionStopped, state: StateMerged, rule: RuleK0Merged},
	{name: "somebody else pushed", plan: []string{"K0:taken-over"},
		mut:      func(f *JudgeFacts) { f.MR.SHA = shaOther },
		decision: DecisionStopped, state: StateTakenOver, rule: RuleK0TakenOver},
	{name: "somebody else pushed, past the deadline", late: true,
		mut:      func(f *JudgeFacts) { f.MR.SHA = shaOther },
		decision: DecisionStopped, state: StateTakenOver, rule: RuleK0TakenOver},
	{name: "merged after somebody else pushed: merged",
		mut:      func(f *JudgeFacts) { f.MR.State = MRStateMerged; f.MR.SHA = shaOther },
		decision: DecisionStopped, state: StateMerged, rule: RuleK0Merged},
	{name: "closed after somebody else pushed: closed",
		mut:      func(f *JudgeFacts) { f.MR.State = MRStateClosed; f.MR.SHA = shaOther },
		decision: DecisionStopped, state: StateClosedUnmerged, rule: RuleK0Closed},
	{name: "a state GitLab has and K0 does not name (locked)",
		mut:      func(f *JudgeFacts) { f.MR.State = "locked" },
		decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr.state"},
	{name: "the MR was not read",
		mut:      func(f *JudgeFacts) { f.MR.Read = false },
		decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr"},
	{name: "the MR was not read, past the deadline", late: true,
		mut:      func(f *JudgeFacts) { f.MR.Read = false },
		decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr"},
	{name: "another MR was read",
		mut:      func(f *JudgeFacts) { f.MR.IID = 42 },
		decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr.iid"},
	{name: "the MR has no head SHA",
		mut:      func(f *JudgeFacts) { f.MR.SHA = "" },
		decision: DecisionCannotJudge, rule: RuleFacts, missing: "mr.sha"},
}

func TestK0Identity(t *testing.T) { runJudgeCases(t, k0Cases) }

// Two empty SHAs are equal and are not an identity.
func TestTwoEmptySHAsAreNotAnIdentity(t *testing.T) {
	f := goodFacts()
	f.Change.ChangeCommitSHA, f.MR.SHA = "", ""
	for i := range f.Pipelines.Pipelines {
		f.Pipelines.Pipelines[i].SHA = ""
	}
	r := Judge(f, tEarly)
	if r.Decision != DecisionCannotJudge || r.Verdict != "" {
		t.Fatalf("Judge = %+v; want cannot-judge", r)
	}
}

// ---- the recorded facts: each one missing means "cannot judge" ----

func TestEveryRecordedFactIsRequired(t *testing.T) {
	cases := []struct {
		name    string
		missing string
		mut     func(*JudgeFacts)
	}{
		{"nothing recorded", "change.recorded", func(f *JudgeFacts) { f.Change.Recorded = false }},
		{"no base SHA", "change.base_sha", func(f *JudgeFacts) { f.Change.BaseSHA = "" }},
		{"no change commit", "change.change_commit_sha", func(f *JudgeFacts) { f.Change.ChangeCommitSHA = "" }},
		{"no base pipeline", "change.base_pipeline_id", func(f *JudgeFacts) { f.Change.BasePipelineID = 0 }},
		{"no MR iid", "change.mr_iid", func(f *JudgeFacts) { f.Change.MRIID = 0; f.MR.IID = 0 }},
		{"no diff classes", "change.diff_classes", func(f *JudgeFacts) { f.Change.DiffClasses = nil }},
		{"a path without a class", "change.diff_classes", func(f *JudgeFacts) { f.Change.DiffClasses = []string{ClassDocs, ""} }},
		{"the base report was not recorded", "change.base_report", func(f *JudgeFacts) { f.Change.BaseReport = Report{} }},
		{"the base report's cases do not add up to its total", "change.base_report", func(f *JudgeFacts) { f.Change.BaseReport.Total = 9 }},
		{"the base report has fewer cases than its total says", "change.base_report", func(f *JudgeFacts) {
			f.Change.BaseReport.Cases = f.Change.BaseReport.Cases[:1]
		}},
		{"a base case has no name", "change.base_report", func(f *JudgeFacts) { f.Change.BaseReport.Cases[0].Name = "" }},
		{"a base case has a status nobody defined", "change.base_report", func(f *JudgeFacts) { f.Change.BaseReport.Cases[0].Status = "passed" }},
		{"a negative total", "change.base_report", func(f *JudgeFacts) { f.Change.BaseReport = Report{Read: true, Total: -1} }},
		{"added Go tests without the go profile", "change.added_go_tests", func(f *JudgeFacts) {
			f.Change.AddedGoTests = []GoTest{{Dir: "crates", Name: "TestTallyTwoCrates"}}
		}},
		{"an added Go test without a name", "change.added_go_tests", func(f *JudgeFacts) {
			f.Change.GoProfile = true
			f.Change.AddedGoTests = []GoTest{{Dir: "crates"}}
		}},
		{"no verify deadline", "change.verify_deadline", func(f *JudgeFacts) { f.Change.VerifyDeadline = time.Time{} }},
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

// ---- K1: settle ----

func unverifiable(rule Rule) judgeCase {
	return judgeCase{decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateInconclusive, rule: rule}
}

func (c judgeCase) named(name string, plan ...string) judgeCase {
	c.name, c.plan = name, plan
	return c
}

func (c judgeCase) with(mut func(*JudgeFacts)) judgeCase { c.mut = mut; return c }
func (c judgeCase) on(base func() JudgeFacts) judgeCase  { c.base = base; return c }
func (c judgeCase) past() judgeCase                      { c.late = true; return c }
func (c judgeCase) testsRule(r Rule) judgeCase           { c.tests = r; return c }
func (c judgeCase) finding(job string, r Rule) judgeCase { c.job, c.jobRule = job, r; return c }

var (
	wait   = judgeCase{decision: DecisionWait, state: StateVerifying, rule: RuleK1Wait}
	green  = judgeCase{decision: DecisionVerdict, verdict: VerdictGreen, state: StateVerified, rule: RuleK5Green}
	noFact = judgeCase{decision: DecisionCannotJudge, rule: RuleFacts}
)

func notGreen(rule Rule) judgeCase {
	return judgeCase{decision: DecisionVerdict, verdict: VerdictNotGreen, state: StateNotVerified, rule: rule}
}

func testsNotRun(rule Rule) judgeCase {
	return judgeCase{decision: DecisionVerdict, verdict: VerdictTestsNotRun, state: StateNotVerified, rule: rule, tests: rule}
}

var flaky = judgeCase{decision: DecisionVerdict, verdict: VerdictFlaky, state: StateInconclusive, rule: RuleK3Flaky}

func missing(what string) judgeCase { c := noFact; c.missing = what; return c }

// unknownReport: K4's UNKNOWN -- a report is unread, so no verdict this pass.
var unknownReport = judgeCase{decision: DecisionCannotJudge, rule: RuleK4Unknown, tests: RuleK4Unknown, missing: "test report"}

var k1Cases = []judgeCase{
	wait.named("no pipeline of the MR yet", "K1:wait").with(func(f *JudgeFacts) { f.Pipelines.Pipelines = nil }),
	wait.named("the pipeline is running").with(func(f *JudgeFacts) { pipe(f, 501).Status = "running" }),
	wait.named("the pipeline is pending").with(func(f *JudgeFacts) { pipe(f, 501).Status = "pending" }),
	wait.named("the only pipelines are of another commit").with(func(f *JudgeFacts) { pipe(f, 501).SHA = shaOther }),
	wait.named("the only pipeline of the commit is a child pipeline").with(func(f *JudgeFacts) { pipe(f, 501).Source = SourceParentPipeline }),
	wait.named("one candidate finished, one is running").with(func(f *JudgeFacts) { secondPipeline(f).Status = "running" }),
	wait.named("a job already failed and the pipeline is still running: no verdict on a pipeline that has not finished").with(func(f *JudgeFacts) {
		setJob(f, "unit-tests", failed(9002, "unit-tests"))
		pipe(f, 501).Status = "running"
	}),
	unverifiable(RuleK1Timeout).named("no pipeline at the deadline", "K1:UNVERIFIABLE(timeout)").past().
		with(func(f *JudgeFacts) { f.Pipelines.Pipelines = nil }),
	unverifiable(RuleK1Timeout).named("still running at the deadline").past().
		with(func(f *JudgeFacts) { pipe(f, 501).Status = "running" }),
	unverifiable(RuleK1Timeout).named("one candidate finished, one still running at the deadline").past().
		with(func(f *JudgeFacts) { secondPipeline(f).Status = "running" }),
	unverifiable(RuleK1Timeout).named("only a child pipeline at the deadline").past().
		with(func(f *JudgeFacts) { pipe(f, 501).Source = SourceParentPipeline }),
	missing("pipelines").named("the pipelines were not read").with(func(f *JudgeFacts) { f.Pipelines.Read = false }),
	unverifiable(RuleK1Unread).named("the pipelines stayed unread past the deadline").past().
		with(func(f *JudgeFacts) { f.Pipelines.Read = false }),
	missing("pipelines").named("a pipeline without an id").with(func(f *JudgeFacts) { pipe(f, 501).ID = 0 }),
	missing("pipelines").named("a pipeline without a source, past the deadline").past().with(func(f *JudgeFacts) { pipe(f, 501).Source = "" }),
	missing("pipelines").named("a pipeline without a status").with(func(f *JudgeFacts) { pipe(f, 501).Status = "" }),
	missing("pipelines").named("a pipeline without a SHA is not a pipeline of another commit").with(func(f *JudgeFacts) { secondPipeline(f).SHA = "" }),
	green.named("a failed pipeline of ANOTHER commit is not a candidate").with(func(f *JudgeFacts) {
		p := secondPipeline(f)
		p.SHA, p.Status = shaOther, StatusFailed
		p.Jobs = []Attempt{failed(9102, "unit-tests")}
	}),
	green.named("a failed CHILD pipeline is not a candidate").with(func(f *JudgeFacts) {
		p := secondPipeline(f)
		p.Source, p.Status = SourceParentPipeline, StatusFailed
		p.Jobs = []Attempt{failed(9102, "unit-tests")}
	}),
}

func TestK1Settle(t *testing.T) { runJudgeCases(t, k1Cases) }

func TestTheDeadlineItselfIsPastTheDeadline(t *testing.T) {
	f := goodFacts()
	pipe(&f, 501).Status = "running"
	if r := Judge(f, tDeadline.Add(-time.Nanosecond)); r.Decision != DecisionWait {
		t.Errorf("one nanosecond before the deadline: %q, want wait", r.Decision)
	}
	if r := Judge(f, tDeadline); r.Verdict != VerdictUnverifiable || r.Rule != RuleK1Timeout {
		t.Errorf("at the deadline: %q by %q, want UNVERIFIABLE by %q", r.Verdict, r.Rule, RuleK1Timeout)
	}
}

func TestCandidatesAreSortedAndOnlyTheCommits(t *testing.T) {
	f := goodFacts()
	f.Pipelines.Pipelines = append([]Pipeline{
		{ID: 777, SHA: shaOther, Source: "push", Status: StatusSuccess},
		{ID: 600, SHA: shaChange, Source: SourceParentPipeline, Status: StatusSuccess},
	}, f.Pipelines.Pipelines...)
	second := secondPipeline(&f)
	second.ID = 480
	r := Judge(f, tEarly)
	if !reflect.DeepEqual(r.Candidates, []int64{480, 501}) {
		t.Fatalf("candidates = %v, want [480 501]", r.Candidates)
	}
}

// ---- K2: definition integrity ----

func delta(d cidef.Delta) func(*JudgeFacts) {
	return func(f *JudgeFacts) { f.Definition.Delta = d }
}

var k2Cases = []judgeCase{
	unverifiable(RuleK2Changed).named("the definition changed, execution preserved", "K2:UNVERIFIABLE(definition)").
		with(delta(cidef.Delta{Kind: cidef.KindPreserving})),
	unverifiable(RuleK2Changed).named("the definition changed what a job executes").
		with(delta(cidef.Delta{Kind: cidef.KindExecChanging})),
	unverifiable(RuleK2Changed).named("Kind says none and the delta lists an added job").
		with(delta(cidef.Delta{Kind: cidef.KindNone, Added: []string{"deploy-preview"}})),
	unverifiable(RuleK2Changed).named("Kind says none and the delta lists a removed job").
		with(delta(cidef.Delta{Kind: cidef.KindNone, Removed: []string{"lint"}})),
	unverifiable(RuleK2Changed).named("Kind says none and the delta lists a changed job").
		with(delta(cidef.Delta{Kind: cidef.KindNone, Jobs: []cidef.JobDelta{{}}})),
	unverifiable(RuleK2Changed).named("Kind says none and the delta lists a rename").
		with(delta(cidef.Delta{Kind: cidef.KindNone, Renamed: []cidef.Rename{{From: "lint", To: "lint-all"}}})),
	unverifiable(RuleK2Changed).named("Kind says none and the delta lists a global change").
		with(delta(cidef.Delta{Kind: cidef.KindNone, Globals: []cidef.GlobalChange{{}}})),
	unverifiable(RuleK2Violation).named("the delta is a violation").
		with(delta(cidef.Delta{Kind: cidef.KindViolation, Violations: []cidef.Violation{{Rule: "D2", Job: "unit-tests", Key: "allow_failure", Detail: "false -> true"}}})),
	unverifiable(RuleK2Violation).named("Kind says none and a violation is listed").
		with(delta(cidef.Delta{Kind: cidef.KindNone, Violations: []cidef.Violation{{Rule: "D1", Job: "lint"}}})),
	unverifiable(RuleK2Unresolvable).named("the delta is unresolvable").
		with(delta(cidef.Delta{Kind: cidef.KindUnresolvable, Unresolvable: []cidef.Unresolvable{{Side: cidef.SideBoth, Reason: "an include that is not local"}}})),
	unverifiable(RuleK2Unresolvable).named("Kind says none and an unresolvable construct is listed").
		with(delta(cidef.Delta{Kind: cidef.KindNone, Unresolvable: []cidef.Unresolvable{{Side: cidef.SideHead, Job: "compile", Key: "script"}}})),
	unverifiable(RuleK2Violation).named("a violation outranks an unresolvable construct").
		with(delta(cidef.Delta{Kind: cidef.KindUnresolvable,
			Violations:   []cidef.Violation{{Rule: "D1", Job: "lint"}},
			Unresolvable: []cidef.Unresolvable{{Side: cidef.SideBoth}}})),
	missing("definition").named("the delta was not computed").with(func(f *JudgeFacts) { f.Definition.Read = false }),
	missing("definition").named("a delta nobody computed (the zero Kind)").with(delta(cidef.Delta{})),
	missing("definition").named("a Kind nobody defined").with(delta(cidef.Delta{Kind: "same"})),
	unverifiable(RuleK2Unread).named("the delta stayed unread past the deadline").past().
		with(func(f *JudgeFacts) { f.Definition.Read = false }),
	unverifiable(RuleK2Unread).named("the zero Kind past the deadline").past().with(delta(cidef.Delta{})),
}

func TestK2Definition(t *testing.T) { runJudgeCases(t, k2Cases) }

// ---- K3: jobs, every attempt ----

var k3Cases = []judgeCase{
	notGreen(RuleK3Red).named("a job failed on its only attempt", "K3:RED(j)", "K5:NOT_GREEN").finding("", RuleK3Red).
		with(func(f *JudgeFacts) { setJob(f, "unit-tests", failed(9002, "unit-tests")); redPipeline(f) }),
	notGreen(RuleK3Red).named("a job was retried and failed again").finding("", RuleK3Red).
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"), failed(9012, "unit-tests"))
			redPipeline(f)
		}),
	notGreen(RuleK3Red).named("another job than the tests failed").finding("lint", RuleK3Red).
		with(func(f *JudgeFacts) { setJob(f, "lint", failed(9003, "lint")); redPipeline(f) }),
	notGreen(RuleK3Red).named("one attempt was allowed to fail and another was not: the job must pass").finding("", RuleK3Red).
		with(func(f *JudgeFacts) {
			allowed := failed(9012, "unit-tests")
			allowed.AllowFailure = true
			setJob(f, "unit-tests", failed(9002, "unit-tests"), allowed)
			redPipeline(f)
		}),
	flaky.named("a job failed and was retried into green", "K3:FLAKY", "K5:FLAKY").finding("", RuleK3Flaky).with(retriedGreen),
	flaky.named("a job passed, was re-run and failed").finding("", RuleK3Flaky).
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", ok(9002, "unit-tests"), failed(9012, "unit-tests"))
			redPipeline(f)
		}),
	flaky.named("a job that is allowed to fail failed and was retried into green").finding("lint", RuleK3Flaky).
		with(func(f *JudgeFacts) {
			a, b := failed(9003, "lint"), ok(9013, "lint")
			a.AllowFailure, b.AllowFailure = true, true
			setJob(f, "lint", a, b)
		}),
	flaky.named("a job failed in one candidate pipeline and passed in the other").finding("", RuleK3Flaky).
		with(func(f *JudgeFacts) {
			p := secondPipeline(f)
			p.Status = StatusFailed
			p.Jobs = []Attempt{ok(9101, "compile"), failed(9102, "unit-tests"), ok(9103, "lint")}
		}),
	green.named("a job that is allowed to fail failed on every attempt: a finding, and the pipeline is what GitLab says").
		finding("lint", RuleK3AllowedFailure).
		with(func(f *JudgeFacts) {
			a := failed(9003, "lint")
			a.AllowFailure = true
			setJob(f, "lint", a)
		}),
	unverifiable(RuleK3Attempts).named("a job failed, was retried and the retry was canceled").finding("", RuleK3Attempts).
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"), withStatus(9012, "unit-tests", StatusCanceled))
			pipe(f, 501).Status = StatusCanceled
		}),
	unverifiable(RuleK3Attempts).named("a job failed, never passed, and the pipeline says success all the same").finding("", RuleK3Attempts).
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"), withStatus(9012, "unit-tests", StatusSkipped))
		}),
	unverifiable(RuleK3Status).named("the pipeline was canceled and nothing failed", "K3:UNVERIFIABLE(status)", "K5:UNVERIFIABLE").
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", withStatus(9002, "unit-tests", StatusCanceled))
			pipe(f, 501).Status = StatusCanceled
		}),
	unverifiable(RuleK3Status).named("the pipeline was skipped").
		with(func(f *JudgeFacts) { pipe(f, 501).Jobs = nil; pipe(f, 501).Status = StatusSkipped }),
	unverifiable(RuleK3Status).named("the pipeline is blocked on a manual job").
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", withStatus(9002, "unit-tests", StatusManual))
			pipe(f, 501).Status = StatusManual
		}),
	unverifiable(RuleK3Status).named("the pipeline failed and no job did").with(redPipeline),
	unverifiable(RuleK3Status).named("one of two candidates was canceled").
		with(func(f *JudgeFacts) { secondPipeline(f).Status = StatusCanceled }),
	missing("jobs").named("the jobs were not listed").with(func(f *JudgeFacts) { pipe(f, 501).JobsRead = false }),
	missing("include_retried").named("the jobs were listed without retried attempts").
		with(func(f *JudgeFacts) { pipe(f, 501).IncludeRetried = false }),
	missing("jobs").named("the jobs of the second candidate were not listed").
		with(func(f *JudgeFacts) { secondPipeline(f).JobsRead = false }),
	unverifiable(RuleK3Unread).named("the jobs stayed unlisted past the deadline").past().
		with(func(f *JudgeFacts) { pipe(f, 501).JobsRead = false }),
	unverifiable(RuleK3Unread).named("listed without retried attempts past the deadline: a failure seen is not RED, a pass unseen may exist").past().
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"))
			redPipeline(f)
			pipe(f, 501).IncludeRetried = false
		}),
	unverifiable(RuleK3Unread).named("listed without retried attempts past the deadline: a green seen is not GREEN").past().
		with(func(f *JudgeFacts) { pipe(f, 501).IncludeRetried = false }),
	missing("jobs").named("a job object without an id").with(func(f *JudgeFacts) { pipe(f, 501).Jobs[1].ID = 0 }),
	missing("jobs").named("a job object without a name, past the deadline").past().with(func(f *JudgeFacts) { pipe(f, 501).Jobs[1].Name = "" }),
	missing("jobs").named("a job object without a status").with(func(f *JudgeFacts) { pipe(f, 501).Jobs[1].Status = "" }),
}

func TestK3Jobs(t *testing.T) { runJudgeCases(t, k3Cases) }

// The plan's test for this part: "a retried-green job yields GREEN" must be
// seen red. Whoever pressed retry, a job that failed and then passed is not
// evidence the change is green.
func TestARetriedGreenJobIsNotGreen(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*JudgeFacts)
	}{
		{"failed, retried, passed", retriedGreen},
		{"failed twice, then passed", func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"), failed(9012, "unit-tests"), ok(9022, "unit-tests"))
		}},
		{"the job objects are listed newest first", func(f *JudgeFacts) {
			setJob(f, "unit-tests", ok(9012, "unit-tests"), failed(9002, "unit-tests"))
		}},
		{"a job that is allowed to fail", func(f *JudgeFacts) {
			a, b := failed(9003, "lint"), ok(9013, "lint")
			a.AllowFailure, b.AllowFailure = true, true
			setJob(f, "lint", a, b)
		}},
		{"the failure is in an older pipeline of the same commit", func(f *JudgeFacts) {
			p := secondPipeline(f)
			p.ID, p.Status = 480, StatusFailed
			p.Jobs = []Attempt{ok(8101, "compile"), failed(8102, "unit-tests"), ok(8103, "lint")}
		}},
	}
	for _, base := range []struct {
		name string
		f    func() JudgeFacts
	}{{"not go", goodFacts}, {"go", goFacts}} {
		for _, c := range cases {
			t.Run(base.name+"/"+c.name, func(t *testing.T) {
				f := base.f()
				c.mut(&f)
				for _, now := range []time.Time{tEarly, tLate} {
					r := Judge(f, now)
					if r.Verdict == VerdictGreen || r.Verdict == VerdictGreenUnobserved || r.State == StateVerified || r.State == StateVerifiedUnobserved {
						t.Fatalf("a retried job read as %q / %q by %q", r.Verdict, r.State, r.Rule)
					}
					if r.Verdict != VerdictFlaky || r.Rule != RuleK3Flaky || r.State != StateInconclusive {
						t.Fatalf("verdict %q by %q, state %q; want FLAKY by %q, inconclusive", r.Verdict, r.Rule, r.State, RuleK3Flaky)
					}
					if out, _ := VerdictOutcome(r.Verdict, 5000); gate.Escalates(out) {
						t.Errorf("a flaky job bought a rung (%q)", out)
					}
				}
			})
		}
	}
}

// ---- K4: the tests ran ----

func goHead(f *JudgeFacts, change func(cases []Case) []Case) {
	p := pipe(f, 501)
	p.Report = report(change(append([]Case(nil), p.Report.Cases...))...)
}

var k4Cases = []judgeCase{
	// TH unreadable.
	unknownReport.named("the head report was not read: no verdict this pass", "K4:UNKNOWN").
		with(func(f *JudgeFacts) { pipe(f, 501).Report = Report{} }),
	unknownReport.named("the head report's cases do not add up to its total").
		with(func(f *JudgeFacts) { pipe(f, 501).Report.Total = 40 }),
	unknownReport.named("a head case has a status nobody defined").
		with(func(f *JudgeFacts) { pipe(f, 501).Report.Cases[0].Status = "ok" }),
	unknownReport.named("the report of the second green candidate was not read").
		with(func(f *JudgeFacts) { secondPipeline(f).Report = Report{} }),
	unverifiable(RuleK4Unread).named("the head report stayed unread past the deadline").past().testsRule(RuleK4Unread).
		with(func(f *JudgeFacts) { pipe(f, 501).Report = Report{} }),

	// TH.total == 0.
	{name: "no report at the head and none at the base", plan: []string{"K4:TESTS_UNOBSERVED", "K5:GREEN_UNOBSERVED"},
		mut:      func(f *JudgeFacts) { f.Change.BaseReport = report(); setHead(f) },
		decision: DecisionVerdict, verdict: VerdictGreenUnobserved, state: StateVerifiedUnobserved,
		rule: RuleK5Unobserved, tests: RuleK4Unobserved},
	{name: "no report at the head; the base reported only a skipped case",
		mut: func(f *JudgeFacts) {
			f.Change.BaseReport = report(Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_empty", Status: CaseSkipped})
			setHead(f)
		},
		decision: DecisionVerdict, verdict: VerdictGreenUnobserved, state: StateVerifiedUnobserved,
		rule: RuleK5Unobserved, tests: RuleK4Unobserved},
	unverifiable(RuleK5ReportRequired).named("no report anywhere, and the project requires one").testsRule(RuleK4Unobserved).
		with(func(f *JudgeFacts) {
			f.Change.BaseReport = report()
			f.Change.RequireTestReport = true
			setHead(f)
		}),
	unverifiable(RuleK4Vanished).named("the base had passing cases and the head reports nothing").testsRule(RuleK4Vanished).
		with(func(f *JudgeFacts) { setHead(f) }),

	// P(B) not a subset of P(H).
	unverifiable(RuleK4Reduced).named("a base case is gone from the head report", "K4:TESTS_REDUCED", "K5:UNVERIFIABLE(tests-reduced)").
		testsRule(RuleK4Reduced).with(func(f *JudgeFacts) { setHead(f, headCases()[1:]...) }),
	unverifiable(RuleK4Reduced).named("a base case fails at the head on a green pipeline").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) { setHead(f, headWith(1, func(c *Case) { c.Status = CaseFailed })...) }),
	unverifiable(RuleK4Reduced).named("a base case is skipped at the head").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) { setHead(f, headWith(0, func(c *Case) { c.Status = CaseSkipped })...) }),
	unverifiable(RuleK4Reduced).named("a base case errors at the head").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) { setHead(f, headWith(2, func(c *Case) { c.Status = CaseError })...) }),
	unverifiable(RuleK4Reduced).named("a base case was renamed (an unstable name reads as reduced)").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) { setHead(f, headWith(2, func(c *Case) { c.Name = "test_label_width[80]" })...) }),
	unverifiable(RuleK4Reduced).named("a base case moved to another classname").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) { setHead(f, headWith(2, func(c *Case) { c.Classname = "crates.test_labels" })...) }),
	unverifiable(RuleK4Reduced).named("a base case moved to another suite").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) { setHead(f, headWith(2, func(c *Case) { c.Suite = "integration" })...) }),
	unverifiable(RuleK4Reduced).named("two base passes share one identity and the head has one of them").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) {
			dup := baseCases()[0]
			f.Change.BaseReport = report(append(baseCases(), dup)...)
		}),
	unverifiable(RuleK4Reduced).named("the suite shrank in one of two reporting pipelines").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) { secondPipeline(f).Report = report(headCases()[1:]...) }),

	// The new tests ran: not Go.
	testsNotRun(RuleK4NewNotRunNone).named("the head report is the base's: no new passing case", "K4:NEW_NOT_RUN", "K5:TESTS_NOT_RUN").with(noNewCase),
	testsNotRun(RuleK4NewNotRunNone).named("the only new case failed").
		with(func(f *JudgeFacts) { setHead(f, headWith(3, func(c *Case) { c.Status = CaseFailed })...) }),
	testsNotRun(RuleK4NewNotRunNone).named("the only new case was skipped").
		with(func(f *JudgeFacts) { setHead(f, headWith(3, func(c *Case) { c.Status = CaseSkipped })...) }),
	testsNotRun(RuleK4NewNotRunNone).named("no new case in either of two reporting pipelines").
		with(func(f *JudgeFacts) { noNewCase(f); secondPipeline(f) }),
	green.named("the new case ran in one of two reporting pipelines").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) { secondPipeline(f).Report = report(baseCases()...) }),
	green.named("one of two green pipelines published no report: the one that did is the evidence").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) { secondPipeline(f).Report = report() }),
	green.named("no report at the base, cases at the head").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) { f.Change.BaseReport = report() }),
	testsNotRun(RuleK4NewNotRunNone).named("no report at the base, and the head's only case is skipped: nothing ran").
		with(func(f *JudgeFacts) {
			f.Change.BaseReport = report()
			setHead(f, Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_two_crates", Status: CaseSkipped})
		}),
	green.named("a second pass of an identity the base had once is a new pass").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) { setHead(f, append(baseCases(), baseCases()[0])...) }),

	// A case newly failing on a green pipeline.
	unverifiable(RuleK4Failed).named("a new case passes and another new case fails, on a green pipeline").testsRule(RuleK4Failed).
		with(func(f *JudgeFacts) {
			setHead(f, append(headCases(), Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_negative", Status: CaseFailed})...)
		}),
	unverifiable(RuleK4Failed).named("a new case errors on a green pipeline").testsRule(RuleK4Failed).
		with(func(f *JudgeFacts) {
			setHead(f, append(headCases(), Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_negative", Status: CaseError})...)
		}),
	unverifiable(RuleK4Failed).named("no report at the base and a failing case at the head").testsRule(RuleK4Failed).
		with(func(f *JudgeFacts) {
			f.Change.BaseReport = report()
			setHead(f, headWith(0, func(c *Case) { c.Status = CaseFailed })...)
		}),
	green.named("a case that failed at the base still fails: not the change's").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) {
			known := Case{Suite: "unit", Classname: "crates.test_label", Name: "test_known_broken", Status: CaseFailed}
			f.Change.BaseReport = report(append(baseCases(), known)...)
			setHead(f, append(headCases(), known)...)
		}),
	green.named("a skipped new case beside a passing one").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) {
			setHead(f, append(headCases(), Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_slow", Status: CaseSkipped})...)
		}),

	// Which diffs K4 looks at.
	green.named("a docs-only diff: no test is looked for, the reports are not even read").testsRule(RuleK4NotApplicable).
		with(func(f *JudgeFacts) {
			f.Change.DiffClasses = []string{ClassDocs, ClassDocs}
			f.Change.RequireTestReport = true
			pipe(f, 501).Report = Report{}
		}),
	testsNotRun(RuleK4NewNotRunNone).named("a docs and test diff is not docs only").
		with(func(f *JudgeFacts) { f.Change.DiffClasses = []string{ClassDocs, ClassTest}; noNewCase(f) }),
	testsNotRun(RuleK4NewNotRunNone).named("a source-only diff").
		with(func(f *JudgeFacts) { f.Change.DiffClasses = []string{ClassSource}; noNewCase(f) }),
	testsNotRun(RuleK4NewNotRunNone).named("a class this package does not know is not a reason to skip the tests").
		with(func(f *JudgeFacts) { f.Change.DiffClasses = []string{ClassDocs, "Docs"}; noNewCase(f) }),

	// The new tests ran: Go, by test-function name.
	green.on(goFacts).named("go: the added function has a passing case", "K4:TESTS_OBSERVED", "K5:GREEN").testsRule(RuleK4Observed),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: the added function has no case", "K4:NEW_NOT_RUN").
		with(func(f *JudgeFacts) { goHead(f, func(c []Case) []Case { return c[:3] }) }),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: the added function's case failed").
		with(func(f *JudgeFacts) { goHead(f, func(c []Case) []Case { c[3].Status = CaseFailed; return c }) }),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: the added function's case was skipped").
		with(func(f *JudgeFacts) { goHead(f, func(c []Case) []Case { c[3].Status = CaseSkipped; return c }) }),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: only a subtest of the added function has a case").
		with(func(f *JudgeFacts) {
			goHead(f, func(c []Case) []Case { c[3].Name = "TestTallyTwoCrates/empty_depot"; return c })
		}),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: a function of that name passes in ANOTHER package").
		with(func(f *JudgeFacts) {
			goHead(f, func(c []Case) []Case { c[3].Classname = "example.test/depot/labels"; return c })
		}),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: a package whose path merely ends like the directory").
		with(func(f *JudgeFacts) {
			goHead(f, func(c []Case) []Case { c[3].Classname = "example.test/depot/othercrates"; return c })
		}),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: the name passed at the base already and passes no more often now").
		with(func(f *JudgeFacts) {
			f.Change.AddedGoTests = []GoTest{{Dir: "crates", Name: "TestTallyEmpty"}}
		}),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: one of two added functions has no case").
		with(func(f *JudgeFacts) {
			f.Change.AddedGoTests = append(f.Change.AddedGoTests, GoTest{Dir: "labels", Name: "TestWidthOfEmpty"})
		}),
	testsNotRun(RuleK4NewNotRunGo).on(goFacts).named("go: the added function ran in neither of two reporting pipelines").
		with(func(f *JudgeFacts) {
			goHead(f, func(c []Case) []Case { return c[:3] })
			secondPipeline(f)
		}),
	green.on(goFacts).named("go: the added function ran in one of two reporting pipelines").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) {
			p := secondPipeline(f)
			p.Report = report(p.Report.Cases[:3]...)
		}),
	green.on(goFacts).named("go: the classname is the directory itself").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) { goHead(f, func(c []Case) []Case { c[3].Classname = "crates"; return c }) }),
	green.on(goFacts).named("go: a function in the repository root matches any package").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) { f.Change.AddedGoTests = []GoTest{{Name: "TestTallyTwoCrates"}} }),
	green.on(goFacts).named("go: no function was added (a test body changed): the name rule has nothing to look for").testsRule(RuleK4Observed).
		with(func(f *JudgeFacts) {
			f.Change.AddedGoTests = nil
			goHead(f, func(c []Case) []Case { return c[:3] })
		}),
	unverifiable(RuleK4Reduced).on(goFacts).named("go: the suite shrank, whatever the added function did").testsRule(RuleK4Reduced).
		with(func(f *JudgeFacts) { goHead(f, func(c []Case) []Case { return c[1:] }) }),

	// A report is evidence only of a pipeline that ran to green.
	unverifiable(RuleK3Status).on(goFacts).named("go: a canceled pipeline's truncated report is not evidence that a test was not collected").
		with(func(f *JudgeFacts) {
			pipe(f, 501).Status = StatusCanceled
			goHead(f, func(c []Case) []Case { return c[:3] })
		}),
	unverifiable(RuleK1Timeout).on(goFacts).named("go: a pipeline still running at the deadline, its report partial").past().
		with(func(f *JudgeFacts) {
			pipe(f, 501).Status = "running"
			goHead(f, func(c []Case) []Case { return c[:3] })
		}),
	unverifiable(RuleK3Status).named("a canceled pipeline's report is not read at all").
		with(func(f *JudgeFacts) {
			pipe(f, 501).Status = StatusCanceled
			pipe(f, 501).Report = Report{}
		}),
	notGreen(RuleK3Red).named("a red pipeline's report is not read at all").
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"))
			redPipeline(f)
			pipe(f, 501).Report = Report{}
		}),
}

func TestK4Tests(t *testing.T) { runJudgeCases(t, k4Cases) }

// The plan's test for this part: "an unreadable report yields a verdict" must
// be seen red. A report that was not read is not an empty report.
func TestAnUnreadableReportIsNotAVerdict(t *testing.T) {
	unreadable := map[string]func(*JudgeFacts){
		"never read":                func(f *JudgeFacts) { pipe(f, 501).Report = Report{} },
		"read flag without content": func(f *JudgeFacts) { pipe(f, 501).Report = Report{Read: false, Total: 4, Cases: headCases()} },
		"fewer cases than the total": func(f *JudgeFacts) {
			pipe(f, 501).Report.Cases = pipe(f, 501).Report.Cases[:2]
		},
	}
	bases := map[string]func() JudgeFacts{
		"would be GREEN":      goodFacts,
		"would be GREEN (go)": goFacts,
		"would be GREEN_UNOBSERVED": func() JudgeFacts {
			f := goodFacts()
			f.Change.BaseReport = report()
			return f
		},
	}
	for bn, base := range bases {
		for un, mut := range unreadable {
			t.Run(bn+"/"+un, func(t *testing.T) {
				f := base()
				mut(&f)
				r := Judge(f, tEarly)
				if r.Decision != DecisionCannotJudge || r.Verdict != "" || r.State != "" {
					t.Fatalf("before the deadline: decision %q verdict %q state %q by %q; an unread report gave an answer",
						r.Decision, r.Verdict, r.State, r.Rule)
				}
				if r.Rule != RuleK4Unknown || r.Tests.Finding != TestsUnknown {
					t.Errorf("rule %q tests %+v; want %q and UNKNOWN", r.Rule, r.Tests, RuleK4Unknown)
				}
				if len(r.Missing) != 1 || !strings.Contains(r.Missing[0], "pipeline 501") {
					t.Errorf("missing = %v; want the report of pipeline 501 named", r.Missing)
				}
				late := Judge(f, tLate)
				if late.Verdict != VerdictUnverifiable || late.Rule != RuleK4Unread || late.State != StateInconclusive {
					t.Fatalf("past the deadline: %q by %q, state %q; want UNVERIFIABLE by %q", late.Verdict, late.Rule, late.State, RuleK4Unread)
				}
			})
		}
	}
}

// The plan's test for this part: "TESTS_REDUCED yields GREEN" must be seen
// red. A suite that shrank is not verified, however green the pipeline.
func TestTestsReducedIsNotGreen(t *testing.T) {
	lost := "unit / crates.test_tally / test_one_crate"
	cases := map[string]func(*JudgeFacts){
		"a base pass is absent":  func(f *JudgeFacts) { setHead(f, append(headCases()[:1], headCases()[2:]...)...) },
		"a base pass now fails":  func(f *JudgeFacts) { setHead(f, headWith(1, func(c *Case) { c.Status = CaseFailed })...) },
		"a base pass is skipped": func(f *JudgeFacts) { setHead(f, headWith(1, func(c *Case) { c.Status = CaseSkipped })...) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := goodFacts()
			mut(&f)
			for _, now := range []time.Time{tEarly, tLate} {
				r := Judge(f, now)
				if r.Verdict == VerdictGreen || r.Verdict == VerdictGreenUnobserved {
					t.Fatalf("a reduced suite read as %q by %q", r.Verdict, r.Rule)
				}
				if r.Verdict != VerdictUnverifiable || r.Rule != RuleK4Reduced || r.State != StateInconclusive {
					t.Fatalf("verdict %q by %q, state %q; want UNVERIFIABLE by %q, inconclusive", r.Verdict, r.Rule, r.State, RuleK4Reduced)
				}
				if r.Tests.Finding != TestsReduced || !reflect.DeepEqual(r.Tests.Lost, []string{lost}) {
					t.Errorf("tests = %+v; want TESTS_REDUCED naming %q", r.Tests, lost)
				}
				if !strings.Contains(r.Detail, lost) {
					t.Errorf("detail %q does not name the lost case", r.Detail)
				}
				labels := LabelsFor(r.State, r.Tests.Finding)
				if !reflect.DeepEqual(labels.MR, []string{LabelInconclusive}) || !labels.IssueNeedsHuman {
					t.Errorf("labels = %+v", labels)
				}
			}
		})
	}
	t.Run("the whole report vanished", func(t *testing.T) {
		f := goodFacts()
		setHead(&f)
		r := Judge(f, tEarly)
		if r.Verdict != VerdictUnverifiable || r.Rule != RuleK4Vanished || r.Tests.Finding != TestsReduced {
			t.Fatalf("verdict %q by %q tests %+v; a head with no report where the base had passes is a reduced suite, not an unobserved one",
				r.Verdict, r.Rule, r.Tests)
		}
		if !strings.Contains(r.Detail, "reports none") {
			t.Errorf("detail %q does not say the report vanished", r.Detail)
		}
	})
}

func TestTheFindingsNameWhatTheyFound(t *testing.T) {
	t.Run("NEW_NOT_RUN(go) names the functions, sorted", func(t *testing.T) {
		f := goFacts()
		f.Change.AddedGoTests = []GoTest{{Dir: "labels", Name: "TestWidthOfEmpty"}, {Name: "TestRoot"}, {Dir: "crates", Name: "TestTallyTwoCrates"}}
		r := Judge(f, tEarly)
		want := []string{"TestRoot", "labels.TestWidthOfEmpty"}
		if !reflect.DeepEqual(r.Tests.NotRun, want) {
			t.Fatalf("not run = %v, want %v", r.Tests.NotRun, want)
		}
		if !strings.Contains(r.Detail, "labels.TestWidthOfEmpty") {
			t.Errorf("detail %q", r.Detail)
		}
	})
	t.Run("TESTS_FAILED names the cases, sorted", func(t *testing.T) {
		f := goodFacts()
		setHead(&f, append(headCases(),
			Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_zzz", Status: CaseError},
			Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_negative", Status: CaseFailed})...)
		r := Judge(f, tEarly)
		want := []string{"unit / crates.test_tally / test_negative", "unit / crates.test_tally / test_zzz"}
		if !reflect.DeepEqual(r.Tests.Failing, want) {
			t.Fatalf("failing = %v, want %v", r.Tests.Failing, want)
		}
	})
	t.Run("a pipeline that did not run to green carries no test finding", func(t *testing.T) {
		f := goodFacts()
		pipe(&f, 501).Status = StatusCanceled
		if r := Judge(f, tEarly); r.Verdict != VerdictUnverifiable || !reflect.DeepEqual(r.Tests, Tests{}) {
			t.Errorf("verdict %q, tests = %+v; a canceled pipeline's report was read", r.Verdict, r.Tests)
		}
	})
	t.Run("a wait and a stop carry no test finding", func(t *testing.T) {
		f := goodFacts()
		pipe(&f, 501).Status = "running"
		if r := Judge(f, tEarly); !reflect.DeepEqual(r.Tests, Tests{}) {
			t.Errorf("tests on a wait = %+v", r.Tests)
		}
	})
}

func TestReportOf(t *testing.T) {
	if r := ReportOf(nil); r.Read || r.wellFormed() {
		t.Fatalf("ReportOf(nil) = %+v; a report that was not read must stay not read", r)
	}
	empty := ReportOf(&glab.TestReport{})
	if !empty.Read || empty.Total != 0 || !empty.wellFormed() {
		t.Fatalf("ReportOf(empty) = %+v; want read, total 0", empty)
	}
	tr := &glab.TestReport{TotalCount: 3, SuccessCount: 2, FailedCount: 1, TestSuites: []glab.TestSuite{
		{Name: "unit", TestCases: []glab.TestCase{
			{Name: "test_empty", Classname: "crates.test_tally", Status: "success"},
			{Name: "test_one_crate", Classname: "crates.test_tally", Status: "failed"},
		}},
		{Name: "lint", TestCases: []glab.TestCase{{Name: "style", Classname: "crates", Status: "success"}}},
	}}
	want := Report{Read: true, Total: 3, Cases: []Case{
		{Suite: "unit", Classname: "crates.test_tally", Name: "test_empty", Status: CaseSuccess},
		{Suite: "unit", Classname: "crates.test_tally", Name: "test_one_crate", Status: CaseFailed},
		{Suite: "lint", Classname: "crates", Name: "style", Status: CaseSuccess},
	}}
	got := ReportOf(tr)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReportOf = %+v\nwant       %+v", got, want)
	}
	if !got.wellFormed() {
		t.Errorf("a report GitLab returned is not well formed")
	}
	// A report whose listed cases are fewer than its total is not a fact
	// about the suite.
	tr.TotalCount = 5
	if ReportOf(tr).wellFormed() {
		t.Errorf("a report with 3 cases and total 5 is well formed")
	}
}

// ---- K5: first match ----

var k5Cases = []judgeCase{
	notGreen(RuleK3Red).named("NOT_GREEN over TESTS_NOT_RUN").
		with(func(f *JudgeFacts) {
			p := secondPipeline(f)
			p.Status = StatusFailed
			p.Jobs = []Attempt{failed(9104, "package")}
			noNewCase(f)
		}),
	notGreen(RuleK3Red).named("NOT_GREEN over FLAKY").
		with(func(f *JudgeFacts) {
			setJob(f, "compile", failed(9001, "compile"), ok(9011, "compile"))
			setJob(f, "unit-tests", failed(9002, "unit-tests"))
			redPipeline(f)
		}),
	notGreen(RuleK3Red).named("NOT_GREEN over UNVERIFIABLE (the definition)").
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"))
			redPipeline(f)
			definitionChanged(f)
		}),
	testsNotRun(RuleK4NewNotRunNone).named("TESTS_NOT_RUN over FLAKY").with(func(f *JudgeFacts) { retriedGreen(f); noNewCase(f) }),
	testsNotRun(RuleK4NewNotRunNone).named("TESTS_NOT_RUN over UNVERIFIABLE (the definition)").
		with(func(f *JudgeFacts) { definitionChanged(f); noNewCase(f) }),
	flaky.named("FLAKY over UNVERIFIABLE (the definition)").with(func(f *JudgeFacts) { retriedGreen(f); definitionChanged(f) }),
	flaky.named("FLAKY over UNVERIFIABLE (tests reduced)").
		with(func(f *JudgeFacts) { retriedGreen(f); setHead(f, headCases()[1:]...) }),
	flaky.named("FLAKY over GREEN_UNOBSERVED").
		with(func(f *JudgeFacts) { retriedGreen(f); f.Change.BaseReport = report(); setHead(f) }),
	unverifiable(RuleK2Changed).named("UNVERIFIABLE (the definition) over UNVERIFIABLE (tests reduced)").
		with(func(f *JudgeFacts) { definitionChanged(f); setHead(f, headCases()[1:]...) }),
	unverifiable(RuleK2Changed).named("UNVERIFIABLE (the definition) over UNVERIFIABLE (a case newly fails)").
		with(func(f *JudgeFacts) {
			definitionChanged(f)
			setHead(f, append(headCases(), Case{Suite: "unit", Classname: "crates.test_tally", Name: "test_negative", Status: CaseFailed})...)
		}),
	unverifiable(RuleK2Changed).named("UNVERIFIABLE (the definition) over GREEN_UNOBSERVED").
		with(func(f *JudgeFacts) { definitionChanged(f); f.Change.BaseReport = report(); setHead(f) }),
	unverifiable(RuleK2Changed).named("UNVERIFIABLE (the definition) over GREEN on a docs-only diff").
		with(func(f *JudgeFacts) { definitionChanged(f); f.Change.DiffClasses = []string{ClassDocs} }),
	unverifiable(RuleK2Changed).named("the definition (K2) is named before a pipeline status (K3)").
		with(func(f *JudgeFacts) { definitionChanged(f); pipe(f, 501).Status = StatusCanceled }),
	unverifiable(RuleK3Attempts).named("a job's attempts (K3) are named before the pipeline's status (K3)").
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"), withStatus(9012, "unit-tests", StatusCanceled))
			pipe(f, 501).Status = StatusCanceled
		}),
	notGreen(RuleK3Red).named("past the deadline, a visible RED outranks the timeout").past().
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"))
			p := secondPipeline(f)
			p.Status = "running"
			p.Jobs = []Attempt{withStatus(9101, "compile", "running")}
			redPipeline(f)
		}),
	unverifiable(RuleK1Timeout).named("past the deadline, a failure whose retry is still running is not RED: the retry may pass").past().
		with(func(f *JudgeFacts) {
			setJob(f, "unit-tests", failed(9002, "unit-tests"), withStatus(9012, "unit-tests", "running"))
			pipe(f, 501).Status = "running"
		}),
	unverifiable(RuleK1Timeout).named("past the deadline, the timeout (K1) is named before the definition (K2)").past().
		with(func(f *JudgeFacts) { pipe(f, 501).Status = "running"; definitionChanged(f) }),
	unverifiable(RuleK2Unread).named("past the deadline, an unread definition is named before an unread report").past().
		with(func(f *JudgeFacts) { f.Definition.Read = false; pipe(f, 501).Report = Report{} }),
	missing("definition").named("before the deadline, an unread definition and an unread report are both named").
		with(func(f *JudgeFacts) { f.Definition.Read = false; pipe(f, 501).Report = Report{} }),
}

func TestK5FirstMatchOrder(t *testing.T) { runJudgeCases(t, k5Cases) }

func TestBeforeTheDeadlineEveryUnreadFactIsNamed(t *testing.T) {
	f := goodFacts()
	f.Definition.Read = false
	pipe(&f, 501).Report = Report{}
	r := Judge(f, tEarly)
	if len(r.Missing) != 2 || !strings.Contains(r.Missing[0], "definition") || !strings.Contains(r.Missing[1], "test report") {
		t.Fatalf("missing = %v; want the definition and the test report", r.Missing)
	}
}

// K5's last rung, asserted and not assumed: a test finding this ladder has
// no rung for is never GREEN.
func TestOnlyAnObservedOrInapplicableFindingIsGreen(t *testing.T) {
	want := map[Finding]Verdict{
		TestsObserved:      VerdictGreen,
		TestsNotApplicable: VerdictGreen,
		TestsUnobserved:    VerdictGreenUnobserved,
		TestsReduced:       VerdictUnverifiable,
		TestsFailed:        VerdictUnverifiable,
		TestsUnknown:       VerdictUnverifiable,
		TestsNewNotRun:     VerdictTestsNotRun,
		"":                 VerdictUnverifiable,
		"SOMETHING_NEW":    VerdictUnverifiable,
	}
	for finding, v := range want {
		got, rule, detail := verdict(nil, nil, Tests{Finding: finding}, false)
		if got != v || detail == "" {
			t.Errorf("verdict(tests %q) = %q by %q (%q), want %q", finding, got, rule, detail, v)
		}
	}
	for _, finding := range []Finding{"", "SOMETHING_NEW"} {
		if _, rule, _ := verdict(nil, nil, Tests{Finding: finding}, false); rule != RuleK5Unexplained {
			t.Errorf("verdict(tests %q) by %q, want %q", finding, rule, RuleK5Unexplained)
		}
	}
}

// Every combination of the "not read" switches, on a change that would
// otherwise be GREEN and on one that would be GREEN_UNOBSERVED: before the
// deadline there is no verdict at all; past it, never a green one.
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
		{"record", func(f *JudgeFacts) { f.Change.Recorded = false }},
		{"report", func(f *JudgeFacts) { f.Pipelines.Pipelines[0].Report = Report{} }},
		{"base report", func(f *JudgeFacts) { f.Change.BaseReport = Report{} }},
	}
	bases := map[string]func() JudgeFacts{
		"GREEN":      goodFacts,
		"GREEN (go)": goFacts,
		"GREEN_UNOBSERVED": func() JudgeFacts {
			f := goodFacts()
			f.Change.BaseReport = report()
			setHead(&f)
			return f
		},
	}
	for baseName, base := range bases {
		if v := Judge(base(), tEarly).Verdict; v != VerdictGreen && v != VerdictGreenUnobserved {
			t.Fatalf("%s: the base is %q", baseName, v)
		}
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
			if r := Judge(f, tLate); r.Verdict == VerdictGreen || r.Verdict == VerdictGreenUnobserved ||
				r.State == StateVerified || r.State == StateVerifiedUnobserved {
				t.Errorf("%s, past the deadline: verdict %q by %q; an unread fact became a pass", label, r.Verdict, r.Rule)
			}
		}
	}
}

// Nothing but a green pipeline is ever green: every pipeline status GitLab
// has, on an otherwise perfect change.
func TestOnlyASuccessPipelineIsGreen(t *testing.T) {
	for _, status := range []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled",
		StatusFailed, StatusCanceled, "canceling", StatusSkipped, StatusManual, "Success", "passed"} {
		for _, now := range []time.Time{tEarly, tLate} {
			f := goodFacts()
			pipe(&f, 501).Status = status
			r := Judge(f, now)
			if r.Verdict == VerdictGreen || r.Verdict == VerdictGreenUnobserved {
				t.Errorf("pipeline status %q: %q by %q", status, r.Verdict, r.Rule)
			}
			if out, reported := VerdictOutcome(r.Verdict, 9000); reported && gate.Escalates(out) {
				t.Errorf("pipeline status %q with no failed job bought a rung: %q by %q", status, r.Verdict, r.Rule)
			}
		}
	}
}

func TestJudgeDoesNotModifyItsFacts(t *testing.T) {
	build := func() JudgeFacts {
		f := goFacts()
		p := secondPipeline(&f)
		p.ID = 480
		p.Jobs = []Attempt{ok(8103, "lint"), failed(8102, "unit-tests"), ok(8101, "compile")}
		p.Status = StatusFailed
		return f
	}
	f, g := build(), build()
	first := Judge(f, tEarly)
	if !reflect.DeepEqual(f, g) {
		t.Fatalf("Judge modified its facts")
	}
	if second := Judge(f, tEarly); !reflect.DeepEqual(first, second) {
		t.Errorf("two calls on the same facts differ:\n%+v\n%+v", first, second)
	}
}

func TestJobFindingsAreSortedAndCounted(t *testing.T) {
	f := goodFacts()
	setJob(&f, "unit-tests", failed(9002, "unit-tests"), ok(9012, "unit-tests"), ok(9022, "unit-tests"))
	a := failed(9003, "lint")
	a.AllowFailure = true
	setJob(&f, "lint", a)
	r := Judge(f, tEarly)
	want := []JobFinding{
		{Job: "lint", Rule: RuleK3AllowedFailure, Attempts: 1, Failed: 1},
		{Job: "unit-tests", Rule: RuleK3Flaky, Attempts: 3, Failed: 1, Succeeded: 2},
	}
	if !reflect.DeepEqual(r.Jobs, want) {
		t.Fatalf("jobs = %+v\nwant   %+v", r.Jobs, want)
	}
}

// NOT_GREEN and TESTS_NOT_RUN are the only verdicts that buy a rung, and both
// need a pipeline that RAN: nothing unread, unsettled or canceled escalates.
func TestOnlyAPipelineThatRanEscalates(t *testing.T) {
	var all []judgeCase
	for _, cs := range [][]judgeCase{k0Cases, k1Cases, k2Cases, k3Cases, k4Cases, k5Cases} {
		all = append(all, cs...)
	}
	for _, c := range all {
		out, reported := VerdictOutcome(c.verdict, 9000)
		escalates := reported && gate.Escalates(out)
		wantEscalates := c.verdict == VerdictNotGreen || c.verdict == VerdictTestsNotRun
		if escalates != wantEscalates {
			t.Errorf("%s: verdict %q outcome %q escalates=%v", c.name, c.verdict, out, escalates)
		}
		if escalates && out != meterapi.OutcomeGateFailed {
			t.Errorf("%s: outcome %q", c.name, out)
		}
	}
}

// The detail of a K2 finding says what was found, and where.
func TestK2DetailsNameTheFinding(t *testing.T) {
	cases := []struct {
		name  string
		delta cidef.Delta
		want  string
	}{
		{"a violation of a job key", cidef.Delta{Kind: cidef.KindViolation, Violations: []cidef.Violation{{Rule: "D2", Job: "unit-tests", Key: "allow_failure", Detail: "false became true"}}},
			"D2 job `unit-tests` key `allow_failure`: false became true"},
		{"a violation of a whole job", cidef.Delta{Kind: cidef.KindViolation, Violations: []cidef.Violation{{Rule: "D1", Job: "lint", Detail: "removed"}}},
			"D1 job `lint`: removed"},
		{"a violation of a global key", cidef.Delta{Kind: cidef.KindViolation, Violations: []cidef.Violation{{Rule: "D3", Key: "workflow", Detail: "changed"}}},
			"D3 global `workflow`: changed"},
		{"a violation Kind with nothing listed", cidef.Delta{Kind: cidef.KindViolation}, "a rule of the definition comparison is broken"},
		{"an unresolvable document", cidef.Delta{Kind: cidef.KindUnresolvable, Unresolvable: []cidef.Unresolvable{{Side: cidef.SideBoth, Reason: "a remote include"}}},
			"the document (both side): a remote include"},
		{"an unresolvable Kind with nothing listed", cidef.Delta{Kind: cidef.KindUnresolvable}, "it could not be resolved"},
		{"a change", cidef.Delta{Kind: cidef.KindExecChanging}, "(exec-changing)"},
		{"a change only the lists show", cidef.Delta{Kind: cidef.KindNone, Added: []string{"deploy-preview"}}, "the delta lists a change"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := goodFacts()
			f.Definition.Delta = c.delta
			r := Judge(f, tEarly)
			if r.Verdict != VerdictUnverifiable || !strings.Contains(r.Detail, c.want) {
				t.Fatalf("verdict %q, detail %q; want UNVERIFIABLE saying %q", r.Verdict, r.Detail, c.want)
			}
		})
	}
}
