package changejudge

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
)

// JudgeFacts is everything plan 2.5 reads. Change was RECORDED by the
// controller at dispatch and apply; the rest is READ from GitLab at judge
// time. Nothing here comes from the session's batch.
type JudgeFacts struct {
	Change     ChangeFacts
	MR         MRFacts
	Pipelines  PipelineFacts
	Definition DefinitionFacts
}

// ChangeFacts is the change record's part of 2.5. Every field is required
// unless it says otherwise: Judge answers DecisionCannotJudge, naming the
// field, when one is missing.
type ChangeFacts struct {
	// Recorded: these facts were read from a change record.
	Recorded bool
	// BaseSHA, ChangeCommitSHA: the commit the change was written against and
	// the commit gonk authored. ChangeCommitSHA moves on a later attempt.
	BaseSHA         string
	ChangeCommitSHA string
	// BasePipelineID: the base pipeline whose report BaseReport is.
	BasePipelineID int64
	// MRIID is the change merge request.
	MRIID int64
	// DiffClasses: the path class (pkg/scope Class values) of EVERY path the
	// change touches, as the controller computed them at apply. K4 is skipped
	// only for a diff that is entirely `docs`.
	DiffClasses []string
	// BaseReport is TB: the base pipeline's test report as recorded at
	// dispatch. Read with Total 0 is the plan's "none".
	BaseReport Report
	// GoProfile: the `go` scope profile is selected, so K4's "new tests ran"
	// is decided by test-function name.
	GoProfile bool
	// AddedGoTests: the Go test functions the change ADDS, from the static
	// gate (tier B). May be empty. Not empty without GoProfile is malformed.
	AddedGoTests []GoTest
	// RequireTestReport: `change.require_test_report` as folded at dispatch.
	RequireTestReport bool
	// VerifyDeadline: when the exit check stops waiting.
	VerifyDeadline time.Time
}

// GoTest is one added Go test function.
type GoTest struct {
	// Dir: the directory of the test file, slash-separated, relative to the
	// repository root; "" for the root (and then any package matches).
	Dir string
	// Name: the function, e.g. TestTally.
	Name string
}

// The path classes Judge distinguishes (scope.Class values; a test pins them).
const (
	ClassSource = "source"
	ClassTest   = "test"
	ClassDocs   = "docs"
)

// MRFacts is the change merge request as read now.
type MRFacts struct {
	Read  bool
	IID   int64
	State string // GitLab's: opened, closed, locked, merged
	SHA   string // the MR's head SHA
}

// GitLab merge request states.
const (
	MRStateOpened = "opened"
	MRStateClosed = "closed"
	MRStateMerged = "merged"
)

// PipelineFacts is every pipeline of the merge request, as listed now. Judge
// selects the candidates itself (K1); the caller does not filter.
type PipelineFacts struct {
	Read      bool
	Pipelines []Pipeline
}

// Pipeline is one pipeline of the merge request.
type Pipeline struct {
	ID     int64
	SHA    string
	Source string // GitLab's pipeline source, e.g. merge_request_event, push, parent_pipeline
	Status string
	// JobsRead: the pipeline's jobs were listed, completely (every page).
	JobsRead bool
	// IncludeRetried: the listing was made with include_retried=true, so
	// Jobs holds every attempt. A listing without it hides exactly the
	// attempts a retry is meant to hide, and is refused.
	IncludeRetried bool
	// Jobs: every job object of the pipeline, retried attempts included.
	Jobs []Attempt
	// Report is TH for this pipeline: its test report. Needed only for a
	// candidate that finished `success`, and only when K4 applies.
	Report Report
}

// SourceParentPipeline is the pipeline source of a child pipeline, which K1
// (and I4) exclude from the candidates.
const SourceParentPipeline = "parent_pipeline"

// GitLab pipeline and job statuses Judge distinguishes.
const (
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusSkipped  = "skipped"
	StatusManual   = "manual"
)

// Attempt is one job object: one attempt of one job in one pipeline.
type Attempt struct {
	ID           int64
	Name         string
	Status       string
	AllowFailure bool
}

// DefinitionFacts is K2's input: the base-vs-head delta of the effective CI
// definition (pkg/cidef) -- or, by the shortcut SC-CIDEF (no `ci`-class path
// in the diff and equal `ci` blob SHAs at base and head),
// cidef.Delta{Kind: cidef.KindNone} without calling the resolver.
type DefinitionFacts struct {
	Read  bool
	Delta cidef.Delta
}

// Decision says what kind of answer a Result is.
type Decision string

const (
	// DecisionVerdict: K5 answered; Result.Verdict is set.
	DecisionVerdict Decision = "verdict"
	// DecisionWait: K1 -- no candidate pipeline yet, or one has not
	// finished, and the deadline has not passed. Look again.
	DecisionWait Decision = "wait"
	// DecisionCannotJudge: a fact is missing or was not read. NO verdict;
	// Result.Missing names what. Look again.
	DecisionCannotJudge Decision = "cannot-judge"
	// DecisionStopped: K0 -- the merge request is no longer gonk's open
	// change (closed, merged, or somebody else pushed). Result.State says
	// which. gonk stops judging.
	DecisionStopped Decision = "stopped"
)

// Rule names the row of plan 2.5 that produced a finding or decided a Result.
type Rule string

// Rule codes. "<step>.<row>".
const (
	RuleFacts Rule = "K0.FACTS" // a recorded or read fact is missing: cannot judge

	RuleK0Closed    Rule = "K0.CLOSED"
	RuleK0Merged    Rule = "K0.MERGED"
	RuleK0TakenOver Rule = "K0.TAKEN_OVER"

	RuleK1Wait    Rule = "K1.WAIT"
	RuleK1Timeout Rule = "K1.UNVERIFIABLE(timeout)"
	RuleK1Unread  Rule = "K1.UNVERIFIABLE(unread)" // the pipeline list stayed unreadable past the deadline

	RuleK2Changed      Rule = "K2.UNVERIFIABLE(definition)"              // the CI definition differs at all
	RuleK2Violation    Rule = "K2.UNVERIFIABLE(definition-violation)"    // and breaks a rule of cidef
	RuleK2Unresolvable Rule = "K2.UNVERIFIABLE(definition-unresolvable)" // or cannot be compared
	RuleK2Unread       Rule = "K2.UNVERIFIABLE(unread)"                  // the delta stayed unread past the deadline

	RuleK3Flaky          Rule = "K3.FLAKY"
	RuleK3Red            Rule = "K3.RED"
	RuleK3AllowedFailure Rule = "K3.ALLOWED_FAILURE"        // failed on every attempt, every attempt allowed to fail: a finding, not a verdict
	RuleK3Attempts       Rule = "K3.UNVERIFIABLE(attempts)" // failed, never succeeded, and not on every attempt
	RuleK3Status         Rule = "K3.UNVERIFIABLE(status)"   // a candidate pipeline is not `success`
	RuleK3Unread         Rule = "K3.UNVERIFIABLE(unread)"   // a job list stayed unread past the deadline

	RuleK4NotApplicable Rule = "K4.NOT_APPLICABLE"                 // the diff is docs only
	RuleK4Unknown       Rule = "K4.UNKNOWN"                        // a report is unread: no verdict this pass
	RuleK4Unread        Rule = "K4.UNVERIFIABLE(report-unread)"    // ... and stayed unread past the deadline
	RuleK4Unobserved    Rule = "K4.TESTS_UNOBSERVED"               // no report at the head, and none with a pass at the base
	RuleK4Vanished      Rule = "K4.TESTS_REDUCED(report-vanished)" // the base had passing cases, the head reports nothing
	RuleK4Reduced       Rule = "K4.TESTS_REDUCED"
	RuleK4NewNotRunGo   Rule = "K4.NEW_NOT_RUN(go)"
	RuleK4NewNotRunNone Rule = "K4.NEW_NOT_RUN(no-new-case)"
	RuleK4Failed        Rule = "K4.TESTS_FAILED" // a case fails at the head that did not fail at the base, on a green pipeline
	RuleK4Observed      Rule = "K4.TESTS_OBSERVED"

	RuleK5ReportRequired Rule = "K5.UNVERIFIABLE(report-required)" // TESTS_UNOBSERVED under require_test_report
	RuleK5Unexplained    Rule = "K5.UNVERIFIABLE(unexplained)"     // nothing above matched and the test finding is not one GREEN accepts
	RuleK5Unobserved     Rule = "K5.GREEN_UNOBSERVED"
	RuleK5Green          Rule = "K5.GREEN"
)

// JobFinding is K3's result for one job that has a failed attempt. A job with
// no failed attempt has no finding.
type JobFinding struct {
	Job  string
	Rule Rule
	// Attempts, Failed, Succeeded: what A(j) held, across every candidate.
	Attempts  int
	Failed    int
	Succeeded int
}

// Finding is K4's answer.
type Finding string

// The K4 findings. The zero value is "K4 did not answer": no candidate
// pipeline finished `success`, so no report is evidence of anything.
const (
	TestsNotApplicable Finding = "NOT_APPLICABLE" // the diff is docs only; nothing about tests was observed or needed
	TestsUnknown       Finding = "UNKNOWN"        // a report is unread
	TestsUnobserved    Finding = "TESTS_UNOBSERVED"
	TestsReduced       Finding = "TESTS_REDUCED"
	TestsNewNotRun     Finding = "NEW_NOT_RUN"
	TestsFailed        Finding = "TESTS_FAILED"
	TestsObserved      Finding = "TESTS_OBSERVED"
)

// Tests is K4's result.
type Tests struct {
	Finding Finding
	Rule    Rule
	// Lost: TESTS_REDUCED -- the cases that passed at the base and do not at
	// the head, as "suite / classname / name", sorted.
	Lost []string
	// NotRun: NEW_NOT_RUN(go) -- the added test functions with no new passing
	// case, as "dir.Name", sorted.
	NotRun []string
	// Failing: TESTS_FAILED -- the cases that fail at the head and did not at
	// the base, sorted.
	Failing []string
}

// Result is Judge's answer.
type Result struct {
	Decision Decision
	// Verdict: set iff Decision is DecisionVerdict.
	Verdict Verdict
	// State: the change state the decision selects. `verifying` for
	// DecisionWait; "" for DecisionCannotJudge (the state does not change).
	State State
	// Rule is the row that decided: the first finding, in K5's order, of the
	// kind that matched.
	Rule Rule
	// Detail says it in words, for the log and the note.
	Detail string
	// Missing: for DecisionCannotJudge, the facts that are missing or unread.
	Missing []string
	// Candidates is C: the ids of the pipelines that were judged.
	Candidates []int64
	// Definition is cidef.KindNone when K2 compared the definition and found
	// it unchanged; "" otherwise.
	Definition cidef.Kind
	// Jobs: K3's findings, sorted by job name.
	Jobs []JobFinding
	// Tests: K4's finding. It selects the tests-observed / tests-unobserved
	// label (Labels); a GREEN on a docs-only diff carries TestsNotApplicable.
	Tests Tests
}

// Judge is the exit check of plan 2.5 at the instant `now`. The steps are the
// plan's K0-K5 and each finding carries the Rule of the row that produced it.
//
// It gives NO verdict before every fact is in: an unsettled pipeline is
// DecisionWait, a missing or unread fact is DecisionCannotJudge. Once `now`
// has reached Change.VerifyDeadline, what is still unsettled or unread at
// JUDGE TIME (the pipeline list, a job list, the definition delta, a test
// report) is UNVERIFIABLE. Three things are never bounded by the deadline and
// stay DecisionCannotJudge for as long as they last: a missing RECORDED fact
// (a malformed base report included), an unread merge request (without it
// there is no telling whose commit would be judged), and a malformed fact (a
// pipeline or job object without its id).
//
// The readings taken where the plan left a choice, in short (the evidence log
// has each one): K4 reads the report of a candidate pipeline that finished
// `success` and of no other; cases are compared as multisets; monotonicity
// must hold in EVERY reporting pipeline while "the new test ran" needs ONE; a
// head that reports nothing where the base had passing cases is
// TESTS_REDUCED, not TESTS_UNOBSERVED; outside Go the head must hold a passing
// case the base did not, with or without a base report; a case that newly
// fails on a green pipeline is UNVERIFIABLE; K4 is skipped only for an
// all-`docs` diff.
func Judge(f JudgeFacts, now time.Time) Result {
	// The recorded facts and the merge request are needed for ANY answer,
	// before or after the deadline.
	if missing := missingFacts(f, now); len(missing) > 0 {
		return cannotJudge(RuleFacts, missing, Result{})
	}

	// K0 identity.
	switch {
	case f.MR.State == MRStateMerged:
		return Result{Decision: DecisionStopped, State: StateMerged, Rule: RuleK0Merged,
			Detail: "the merge request was merged"}
	case f.MR.State == MRStateClosed:
		return Result{Decision: DecisionStopped, State: StateClosedUnmerged, Rule: RuleK0Closed,
			Detail: "the merge request was closed without being merged"}
	case f.MR.State != MRStateOpened:
		return cannotJudge(RuleFacts, []string{fmt.Sprintf("mr.state: %q is neither opened, closed nor merged", f.MR.State)}, Result{})
	case f.MR.SHA != f.Change.ChangeCommitSHA:
		return Result{Decision: DecisionStopped, State: StateTakenOver, Rule: RuleK0TakenOver,
			Detail: "the merge request's head is not gonk's commit: somebody else pushed; gonk stops judging"}
	}

	past := !now.Before(f.Change.VerifyDeadline)
	var res Result
	// unverifiable: what K1, K2 and K3 could not establish, in step order.
	// unread: facts to look for again -- before the deadline any of them
	// means no verdict this pass.
	var unverifiable []cause
	var unread []string

	// K1 settle. C = the MR's pipelines of the change commit that are not
	// child pipelines.
	var candidates []Pipeline
	if !f.Pipelines.Read {
		unread = append(unread, "pipelines: the merge request's pipelines were not read")
		unverifiable = append(unverifiable, cause{RuleK1Unread, "the merge request's pipelines could not be read before the verify deadline"})
	} else {
		for _, p := range f.Pipelines.Pipelines {
			if p.ID == 0 || p.SHA == "" || p.Source == "" || p.Status == "" {
				return cannotJudge(RuleFacts, []string{"pipelines: a pipeline without id, sha, source or status"}, Result{})
			}
			if p.SHA != f.Change.ChangeCommitSHA {
				continue
			}
			if p.Source == SourceParentPipeline {
				continue
			}
			candidates = append(candidates, p)
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		settled := len(candidates) > 0
		for _, c := range candidates {
			res.Candidates = append(res.Candidates, c.ID)
			if !settledStatus(c.Status) {
				settled = false
			}
		}
		if !settled && !past {
			res.Decision, res.State, res.Rule = DecisionWait, StateVerifying, RuleK1Wait
			res.Detail = "no pipeline of the change commit exists yet"
			if len(candidates) > 0 {
				res.Detail = "a pipeline of the change commit has not finished"
			}
			return res
		}
		if !settled {
			unverifiable = append(unverifiable, cause{RuleK1Timeout, "the pipelines of the change commit had not all finished (or none existed) at the verify deadline"})
		}
	}

	// K2 definition integrity.
	if !f.Definition.Read || !knownKind(f.Definition.Delta.Kind) {
		unread = append(unread, "definition: the base-vs-head CI definition delta was not computed")
		unverifiable = append(unverifiable, cause{RuleK2Unread, "the CI definition could not be compared before the verify deadline"})
	} else if c := judgeDefinition(f.Definition.Delta); c.rule != "" {
		unverifiable = append(unverifiable, c)
	} else {
		res.Definition = cidef.KindNone
	}

	// K3 jobs: every attempt of every job across C.
	complete := f.Pipelines.Read
	for _, c := range candidates {
		if !c.JobsRead {
			unread = append(unread, fmt.Sprintf("jobs: the jobs of pipeline %d were not listed", c.ID))
			complete = false
		}
		if !c.IncludeRetried {
			unread = append(unread, fmt.Sprintf("jobs: pipeline %d was listed without include_retried, so retried attempts may be missing", c.ID))
			complete = false
		}
		for _, a := range c.Jobs {
			if a.ID == 0 || a.Name == "" || a.Status == "" {
				return cannotJudge(RuleFacts, []string{fmt.Sprintf("jobs: a job object of pipeline %d without id, name or status", c.ID)}, res)
			}
		}
	}
	if complete {
		// With every attempt known, each job with a failed attempt is judged.
		res.Jobs = judgeJobs(candidates)
		if j, found := firstJob(res.Jobs, RuleK3Attempts); found {
			unverifiable = append(unverifiable, cause{j.Rule, fmt.Sprintf(
				"job %q failed, never succeeded, and did not fail on every one of its %d attempts", j.Job, j.Attempts)})
		}
	} else if f.Pipelines.Read {
		// Attempts may be missing: an unseen one could turn any finding into
		// another, so nothing is said about any job.
		unverifiable = append(unverifiable, cause{RuleK3Unread, "the jobs of a pipeline of the change commit could not be listed, with every attempt, before the verify deadline"})
	}
	for _, c := range candidates {
		if c.Status != StatusSuccess {
			unverifiable = append(unverifiable, cause{RuleK3Status, fmt.Sprintf("pipeline %d is %q and no failed job explains it", c.ID, c.Status)})
			break
		}
	}

	// K4 tests.
	res.Tests = judgeTests(f.Change, candidates)

	// Before the deadline nothing is decided on partial facts.
	if !past && len(unread) > 0 {
		if res.Tests.Finding == TestsUnknown {
			unread = append(unread, unreadReports(candidates)...)
		}
		return cannotJudge(RuleFacts, unread, res)
	}
	if !past && res.Tests.Finding == TestsUnknown {
		return cannotJudge(RuleK4Unknown, unreadReports(candidates), res)
	}
	// Past it, what is still unknown was not observed.
	if res.Tests.Finding == TestsUnknown {
		res.Tests.Rule = RuleK4Unread
	}

	// K5 verdict, first match.
	v, rule, detail := verdict(res.Jobs, unverifiable, res.Tests, f.Change.RequireTestReport)
	res.Decision, res.Verdict, res.Rule, res.Detail = DecisionVerdict, v, rule, detail
	res.State, _ = StateFor(v)
	return res
}

// verdict is K5: the first match, in the plan's order.
func verdict(jobs []JobFinding, unverifiable []cause, tests Tests, requireReport bool) (Verdict, Rule, string) {
	// NOT_GREEN: any RED.
	if j, found := firstJob(jobs, RuleK3Red); found {
		return VerdictNotGreen, j.Rule, fmt.Sprintf("job %q failed on every one of its %d attempts", j.Job, j.Attempts)
	}
	// TESTS_NOT_RUN: NEW_NOT_RUN.
	if tests.Finding == TestsNewNotRun {
		if len(tests.NotRun) > 0 {
			return VerdictTestsNotRun, tests.Rule, "an added test function has no new passing case in the test report: " + strings.Join(tests.NotRun, ", ")
		}
		return VerdictTestsNotRun, tests.Rule, "the test report holds no passing case that the base did not already have: the new tests were not collected"
	}
	// FLAKY: any FLAKY.
	if j, found := firstJob(jobs, RuleK3Flaky); found {
		return VerdictFlaky, j.Rule, fmt.Sprintf("job %q both failed and succeeded across its %d attempts", j.Job, j.Attempts)
	}
	// UNVERIFIABLE: from K1-K3.
	if len(unverifiable) > 0 {
		return VerdictUnverifiable, unverifiable[0].rule, unverifiable[0].detail
	}
	// UNVERIFIABLE: a report that stayed unread past the deadline.
	if tests.Finding == TestsUnknown {
		return VerdictUnverifiable, tests.Rule, "the test report of a green pipeline could not be read before the verify deadline"
	}
	// UNVERIFIABLE(tests-reduced): TESTS_REDUCED.
	if tests.Finding == TestsReduced {
		if tests.Rule == RuleK4Vanished {
			return VerdictUnverifiable, tests.Rule, "the base pipeline reported passing test cases and the change's pipeline reports none"
		}
		return VerdictUnverifiable, tests.Rule, "a test case that passed on the base does not pass in the change's pipeline: " + strings.Join(tests.Lost, "; ")
	}
	// UNVERIFIABLE: a case newly failing on a green pipeline.
	if tests.Finding == TestsFailed {
		return VerdictUnverifiable, tests.Rule, "a test case fails in the change's green pipeline that did not fail on the base: " + strings.Join(tests.Failing, "; ")
	}
	// GREEN_UNOBSERVED: TESTS_UNOBSERVED -- UNVERIFIABLE when a report is required.
	if tests.Finding == TestsUnobserved {
		if requireReport {
			return VerdictUnverifiable, RuleK5ReportRequired, "change.require_test_report is set and the change's pipeline published no test report"
		}
		return VerdictGreenUnobserved, RuleK5Unobserved, "every pipeline of the change commit succeeded; no test report was published, so whether the tests ran was not observed"
	}
	// GREEN: only on a finding GREEN accepts. Anything this ladder has no
	// rung for can never fall through to it.
	if tests.Finding == TestsObserved {
		return VerdictGreen, RuleK5Green, "every pipeline of the change commit succeeded with no failed attempt, the suite did not shrink, and the new tests ran and passed"
	}
	if tests.Finding == TestsNotApplicable {
		return VerdictGreen, RuleK5Green, "every pipeline of the change commit succeeded with no failed attempt; the change touches documentation only, so no test was looked for"
	}
	return VerdictUnverifiable, RuleK5Unexplained, fmt.Sprintf("the test finding %q is not one a green verdict accepts", tests.Finding)
}

// cause is a finding of K1, K2 or K3: the rule and its words.
type cause struct {
	rule   Rule
	detail string
}

// cannotJudge is the no-verdict answer. partial carries what was established
// before the missing fact was met, for the log.
func cannotJudge(rule Rule, missing []string, partial Result) Result {
	return Result{
		Decision:   DecisionCannotJudge,
		Rule:       rule,
		Detail:     "no verdict: " + strings.Join(missing, "; "),
		Missing:    missing,
		Candidates: partial.Candidates,
		Jobs:       partial.Jobs,
		Tests:      partial.Tests,
	}
}

// missingFacts names every recorded fact, and every fact of the merge
// request, that is missing. These are required whatever the time is.
func missingFacts(f JudgeFacts, now time.Time) []string {
	var missing []string
	miss := func(bad bool, what string) {
		if bad {
			missing = append(missing, what)
		}
	}
	miss(now.IsZero(), "now: no time was given")
	if !f.Change.Recorded {
		return append(missing, "change.recorded: no change record was read")
	}
	ch := f.Change
	miss(ch.BaseSHA == "", "change.base_sha: no base SHA")
	miss(ch.ChangeCommitSHA == "", "change.change_commit_sha: no change commit")
	miss(ch.BasePipelineID == 0, "change.base_pipeline_id: no base pipeline")
	miss(ch.MRIID == 0, "change.mr_iid: no merge request")
	miss(len(ch.DiffClasses) == 0, "change.diff_classes: the change touches no path")
	for _, c := range ch.DiffClasses {
		miss(c == "", "change.diff_classes: a path without a class")
	}
	miss(!ch.BaseReport.wellFormed(), "change.base_report: the base test report was not recorded, or its cases do not match its total")
	miss(len(ch.AddedGoTests) > 0 && !ch.GoProfile, "change.added_go_tests: added Go test functions without the go profile")
	for _, g := range ch.AddedGoTests {
		miss(g.Name == "", "change.added_go_tests: a test function without a name")
	}
	miss(ch.VerifyDeadline.IsZero(), "change.verify_deadline: no deadline")

	if !f.MR.Read {
		return append(missing, "mr: the merge request was not read")
	}
	miss(f.MR.IID != ch.MRIID, fmt.Sprintf("mr.iid: read !%d, the change is !%d", f.MR.IID, ch.MRIID))
	miss(f.MR.SHA == "", "mr.sha: the merge request has no head SHA")
	return missing
}

// settledStatus: K1's (and I4's) finished pipeline statuses. `manual` -- a
// pipeline blocked on a manual job -- is finished here: plan 2.5 names
// manual-blocked under K3 (UNVERIFIABLE(status)), not under K1's wait.
func settledStatus(s string) bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped, StatusManual:
		return true
	}
	return false
}

// knownKind: a Delta somebody computed. The zero Kind is a delta nobody
// computed and is treated as unread.
func knownKind(k cidef.Kind) bool {
	switch k {
	case cidef.KindNone, cidef.KindPreserving, cidef.KindExecChanging, cidef.KindViolation, cidef.KindUnresolvable:
		return true
	}
	return false
}

// judgeDefinition is K2 over a computed delta. The zero cause means the
// definition is the one the base ran. There is no `ci` lift in this workflow:
// ANY difference is UNVERIFIABLE(definition).
//
// The delta's lists are read as well as its Kind, and the worse of the two
// decides: a change that is listed is a change whatever Kind says.
func judgeDefinition(d cidef.Delta) cause {
	if d.Kind == cidef.KindViolation {
		return cause{RuleK2Violation, "the CI definition the change is judged by was altered: " + violationText(d)}
	}
	if len(d.Violations) > 0 {
		return cause{RuleK2Violation, "the CI definition the change is judged by was altered: " + violationText(d)}
	}
	if d.Kind == cidef.KindUnresolvable {
		return cause{RuleK2Unresolvable, "the CI definition cannot be compared: " + unresolvableText(d)}
	}
	if len(d.Unresolvable) > 0 {
		return cause{RuleK2Unresolvable, "the CI definition cannot be compared: " + unresolvableText(d)}
	}
	if d.Kind != cidef.KindNone {
		return cause{RuleK2Changed, fmt.Sprintf("the CI definition at the change commit is not the one the base ran (%s)", d.Kind)}
	}
	if len(d.Jobs)+len(d.Removed)+len(d.Added)+len(d.Renamed)+len(d.Globals) > 0 {
		return cause{RuleK2Changed, "the CI definition at the change commit is not the one the base ran (the delta lists a change)"}
	}
	return cause{}
}

func violationText(d cidef.Delta) string {
	if len(d.Violations) == 0 {
		return "a rule of the definition comparison is broken"
	}
	v := d.Violations[0]
	return fmt.Sprintf("%s %s: %s", v.Rule, where(v.Job, v.Key), v.Detail)
}

func unresolvableText(d cidef.Delta) string {
	if len(d.Unresolvable) == 0 {
		return "it could not be resolved"
	}
	u := d.Unresolvable[0]
	return fmt.Sprintf("%s (%s side): %s", where(u.Job, u.Key), u.Side, u.Reason)
}

func where(job, key string) string {
	switch {
	case job == "" && key == "":
		return "the document"
	case job == "":
		return "global `" + key + "`"
	case key == "":
		return "job `" + job + "`"
	}
	return "job `" + job + "` key `" + key + "`"
}

// judgeJobs is K3 per job: A(j) is every job object of that name in any
// candidate pipeline, retried attempts included. One finding per job that has
// a failed attempt, sorted by name.
func judgeJobs(candidates []Pipeline) []JobFinding {
	byName := map[string]*JobFinding{}
	required := map[string]bool{}
	var names []string
	for _, c := range candidates {
		for _, a := range c.Jobs {
			j, has := byName[a.Name]
			if !has {
				j = &JobFinding{Job: a.Name}
				byName[a.Name] = j
				names = append(names, a.Name)
			}
			j.Attempts++
			if a.Status == StatusFailed {
				j.Failed++
			}
			if a.Status == StatusSuccess {
				j.Succeeded++
			}
			// One attempt that is not allowed to fail makes the job one
			// that must pass.
			if !a.AllowFailure {
				required[a.Name] = true
			}
		}
	}
	sort.Strings(names)
	var out []JobFinding
	for _, name := range names {
		j := *byName[name]
		if j.Failed == 0 {
			continue
		}
		j.Rule = jobRule(j, required[name])
		out = append(out, j)
	}
	return out
}

// jobRule is K3's ladder for a job with at least one failed attempt.
func jobRule(j JobFinding, required bool) Rule {
	// Failed and succeeded: FLAKY, allowed to fail or not, whoever retried.
	if j.Succeeded > 0 {
		return RuleK3Flaky
	}
	if !required {
		return RuleK3AllowedFailure
	}
	if j.Failed == j.Attempts {
		return RuleK3Red
	}
	return RuleK3Attempts
}

func firstJob(jobs []JobFinding, rules ...Rule) (JobFinding, bool) {
	for _, j := range jobs {
		for _, r := range rules {
			if j.Rule == r {
				return j, true
			}
		}
	}
	return JobFinding{}, false
}
