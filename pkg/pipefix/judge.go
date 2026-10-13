package pipefix

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// JudgeFacts is everything plan 6.3 reads (6.1). Fix was RECORDED by the
// controller at fix dispatch and apply; the rest is READ from GitLab at judge
// time. Nothing here comes from the fix batch.
type JudgeFacts struct {
	Fix        FixFacts
	MR         MRFacts
	Pipelines  PipelineFacts
	Definition DefinitionFacts
}

// FixFacts is the fix record's part of 6.1. Every field is required: Judge
// answers DecisionCannotJudge, naming the field, when one is missing.
type FixFacts struct {
	// Recorded: these facts were read from a fix record.
	Recorded bool
	// Targets is T, from TargetSet at dispatch.
	Targets []Target
	// BasePipelineID is the diagnosed (base) pipeline.
	BasePipelineID int64
	// BaseJobs: EVERY job of the base pipeline, one entry per name, with its
	// latest attempt's status. Every job of T must be in it as failed and not
	// allowed to fail.
	BaseJobs []BaseJob
	// FixBaseSHA, FixCommitSHA: the commit the fix was written against and
	// the commit gonk authored. FixCommitSHA moves on a later attempt.
	FixBaseSHA   string
	FixCommitSHA string
	// MRIID is the fix merge request.
	MRIID int64
	// ExcerptVersion, SignatureVersion: pipelinectx's versions when T's
	// signatures were recorded. A failed attempt can only be compared with a
	// recorded signature by a build of the same versions.
	ExcerptVersion   int
	SignatureVersion int
	// Lift: the path classes the DISPATCH lifted (plan 4.2); only LiftCI
	// means anything here.
	Lift []string
	// CIExecChanges: `pipeline_fix.ci_exec_changes` as folded at dispatch.
	CIExecChanges string
	// VerifyDeadline: when the exit check stops waiting.
	VerifyDeadline time.Time
}

// LiftCI is the one liftable path class (scope.ClassCI; pinned by a test).
const LiftCI = "ci"

// `pipeline_fix.ci_exec_changes` values (plan 3.1).
const (
	CIExecDeny      = "deny"
	CIExecManualRun = "manual-run"
	CIExecAuto      = "auto"
)

// BaseJob is one job of the base pipeline as recorded at fix dispatch.
type BaseJob struct {
	Name         string
	Status       string
	AllowFailure bool
}

// MRFacts is the fix merge request as read now.
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
// selects the candidates itself (J1); the caller does not filter.
type PipelineFacts struct {
	Read      bool
	Pipelines []Pipeline
}

// Pipeline is one pipeline of the merge request.
type Pipeline struct {
	ID     int64
	SHA    string
	Source string // GitLab's pipeline source, e.g. merge_request_event, push, parent_pipeline
	// Ref is GitLab's pipeline ref: a branch name, or for a merge request
	// pipeline refs/merge-requests/<iid>/head, .../merge (merged results) or
	// .../train (a merge train). Read only by mergedResultPipeline; "" is a
	// pipeline whose ref was not recorded, and is none of those.
	Ref    string
	Status string
	// JobsRead: the pipeline's jobs were listed, completely (every page).
	JobsRead bool
	// IncludeRetried: the listing was made with include_retried=true, so
	// Jobs holds every attempt. A listing without it hides exactly the
	// attempts a retry is meant to hide, and is refused.
	IncludeRetried bool
	// Jobs: every job object of the pipeline, retried attempts included.
	Jobs []Attempt
}

// SourceParentPipeline is the pipeline source of a child pipeline, which J1
// excludes from the candidates.
const SourceParentPipeline = "parent_pipeline"

// GitLab pipeline and job statuses Judge distinguishes.
const (
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusSkipped  = "skipped"
)

// Attempt is one job object: one attempt of one job in one pipeline.
type Attempt struct {
	// ID is GitLab's job id. Within one pipeline the attempt with the
	// highest id of a name is that job's latest attempt.
	ID            int64
	Name          string
	Stage         string
	Status        string
	FailureReason string
	AllowFailure  bool
	// Trace is needed only for a FAILED attempt of a job in T.
	Trace Trace
}

// Trace is what Judge needs of a failed attempt's log: its first error line
// under pipelinectx's excerpt (FailedTrace computes it).
type Trace struct {
	Read       bool
	FirstError string
}

// LogRead is a failed job's log AS IT WAS READ: the bytes, and whether they
// are the whole log. A caller that read a bounded tail (glab.TraceTail) copies
// its two flags here; they are part of the fact, not an option.
type LogRead struct {
	Data []byte
	// HeadTruncated: bytes precede Data in the log, so Data begins in the
	// middle of a line.
	HeadTruncated bool
	// TailTruncated: the read stopped before the END of the log.
	TailTruncated bool
}

// FailedTrace is how a caller turns a job log it READ into a Trace: the first
// error line of pipelinectx's excerpt, computed with the SAME truncation flags
// the diagnosis dispatch computed the recorded signature with
// (pipelinectx.ExcerptWith). A log that could not be read at all is the zero
// Trace, never FailedTrace(LogRead{}).
//
// A truncated read whose first error line cannot be established is NOT READ
// (the zero Trace: J3.UNKNOWN, look again) -- never a signature computed from
// a partial log, which would read as "it fails differently now" (gonk-etvm):
//
//   - TailTruncated: the end of the log, where the failure is, was not read.
//   - HeadTruncated with no newline in Data: everything read is the tail of
//     ONE line whose beginning was never read.
//
// HeadTruncated otherwise: the cut first line is dropped, as the record's
// excerpt drops it, and the first error line is looked for in the rest.
func FailedTrace(l LogRead) Trace {
	if l.TailTruncated {
		return Trace{}
	}
	if l.HeadTruncated && len(l.Data) > 0 && bytes.IndexByte(l.Data, '\n') < 0 {
		return Trace{}
	}
	ex := pipelinectx.ExcerptWith(l.Data, pipelinectx.Options{HeadTruncated: l.HeadTruncated})
	return Trace{Read: true, FirstError: ex.FirstError}
}

// DefinitionFacts is J2's input: the base-vs-head delta of the effective CI
// definition (pkg/cidef), computed by the caller from the merged YAML at
// FixBaseSHA and FixCommitSHA with T as the diagnosed set -- or, by plan 6.2's
// shortcut SC-CIDEF (no `ci`-class path in the diff and equal blob SHAs),
// cidef.Delta{Kind: cidef.KindNone} without calling the resolver.
type DefinitionFacts struct {
	Read  bool
	Delta cidef.Delta
}

// Decision says what kind of answer a Result is.
type Decision string

const (
	// DecisionVerdict: J5 answered; Result.Verdict is set.
	DecisionVerdict Decision = "verdict"
	// DecisionWait: J1 -- no candidate pipeline yet (none, or only pipelines
	// a `[skip ci]` commit left skipped with no job: plan 5.5's
	// `awaiting-run`), or one has not finished, and the deadline has not
	// passed. Look again.
	DecisionWait Decision = "wait"
	// DecisionCannotJudge: a fact is missing or was not read. NO verdict;
	// Result.Missing names what. Look again.
	DecisionCannotJudge Decision = "cannot-judge"
	// DecisionStopped: J0 -- the merge request is no longer gonk's open fix
	// (closed, merged, or somebody else pushed). Result.State says which.
	// gonk stops judging and never merges.
	DecisionStopped Decision = "stopped"
)

// Rule names the row of plan 6.3 that produced a finding or decided a Result.
type Rule string

// Rule codes. "<step>.<row>".
const (
	RuleFacts Rule = "J0.FACTS" // a recorded or read fact is missing: cannot judge

	RuleJ0Closed    Rule = "J0.CLOSED"
	RuleJ0Merged    Rule = "J0.MERGED"
	RuleJ0TakenOver Rule = "J0.TAKEN_OVER"

	RuleJ1Wait    Rule = "J1.WAIT"
	RuleJ1Timeout Rule = "J1.TIMEOUT"
	RuleJ1Unread  Rule = "J1.UNREAD" // the pipeline list stayed unreadable past the deadline
	// RuleJ1MergedResult: at the deadline the merge request's only pipelines
	// ran on GitLab's merged-results (or merge train) commit, which cannot be
	// attributed to the fix commit (gonk-ow8u; mergedResultPipeline).
	RuleJ1MergedResult Rule = "J1.MERGED_RESULT"

	RuleJ2Violation        Rule = "J2.VIOLATION"         // D1 / D2 / D3 broken
	RuleJ2HeadUnresolvable Rule = "J2.HEAD_UNRESOLVABLE" // the change introduced what cannot be compared
	RuleJ2Unlifted         Rule = "J2.UNLIFTED"          // the CI definition changed without the `ci` lift
	RuleJ2ExecDenied       Rule = "J2.EXEC_DENIED"       // exec-changing under ci_exec_changes: deny
	RuleJ2Unresolvable     Rule = "J2.UNRESOLVABLE"
	RuleJ2Unread           Rule = "J2.UNREAD" // the delta stayed unread past the deadline

	RuleJ3Absent       Rule = "J3.NOT_OBSERVED(absent)"
	RuleJ3Weakened     Rule = "J3.WEAKENED"
	RuleJ3Unknown      Rule = "J3.UNKNOWN" // a trace is unread: no verdict this pass
	RuleJ3FlakySame    Rule = "J3.FLAKY(recurs-and-passes)"
	RuleJ3Recurs       Rule = "J3.RECURS"
	RuleJ3FlakyChanged Rule = "J3.FLAKY(fails-and-passes)"
	RuleJ3Changed      Rule = "J3.CHANGED"
	RuleJ3Passed       Rule = "J3.PASSED"
	RuleJ3Status       Rule = "J3.NOT_OBSERVED(status)"
	RuleJ3Unread       Rule = "J3.NOT_OBSERVED(unread)"   // jobs or a trace stayed unread past the deadline
	RuleJ3Versions     Rule = "J3.NOT_OBSERVED(versions)" // recorded signature versions are not this build's

	RuleJ4Regressed        Rule = "J4.REGRESSED"
	RuleJ4RegressedAllowed Rule = "J4.REGRESSED(allowed-to-fail)"
	RuleJ4Remaining        Rule = "J4.REMAINING"
	RuleJ4Revealed         Rule = "J4.REVEALED"

	RuleJ5NotSuccess Rule = "J5.PIPELINE_NOT_SUCCESS" // nothing failed that explains a candidate that is not `success`
	RuleJ5Fixed      Rule = "J5.FIXED"
)

// JobFinding is J3's result for one job of T.
type JobFinding struct {
	Job  string
	Rule Rule
	// Attempts, Failed, Succeeded: what A held.
	Attempts  int
	Failed    int
	Succeeded int
	// NewSignature is the error signature of the first failed attempt whose
	// failure differs from the recorded one (CHANGED, FLAKY); "" otherwise.
	NewSignature string
}

// OtherFinding is J4's result for one job outside T that failed.
type OtherFinding struct {
	Job        string
	Rule       Rule
	PipelineID int64
	BaseStatus string // "" when the base pipeline had no such job
}

// Result is Judge's answer.
type Result struct {
	Decision Decision
	// Verdict: set iff Decision is DecisionVerdict.
	Verdict Verdict
	// State: the fix state the decision selects (plan 7.1). `verifying` for
	// DecisionWait; "" for DecisionCannotJudge (the state does not change).
	State State
	// Rule is the row that decided: the first finding, in J5's order, of the
	// kind that matched.
	Rule Rule
	// Detail says it in words, for the log and the note.
	Detail string
	// Missing: for DecisionCannotJudge, the facts that are missing or
	// unread.
	Missing []string
	// Candidates is C: the ids of the pipelines that were judged.
	Candidates []int64
	// Skipped: the pipelines of the fix commit that are NOT candidates
	// because they were `skipped` and ran no job (a `[skip ci]` commit).
	Skipped []int64
	// MergedResult: the pipelines of this merge request that ran on GitLab's
	// merged-results or merge-train commit and so are NOT candidates
	// (mergedResultPipeline). Listed so the outcome can say what was there.
	MergedResult []int64
	// Definition is J2's delta when the definition could be compared:
	// none, preserving or exec-changing. A recorded fact that the merge
	// policy consumes (plan 9.1 A4); "" otherwise.
	Definition cidef.Kind
	Jobs       []JobFinding
	Others     []OtherFinding
}

// Judge is the exit check of plan 6.3 at the instant `now`. See the package
// comment for what it guarantees; the steps are the plan's J0-J5 and each
// finding carries the Rule of the row that produced it.
//
// It gives NO verdict before every fact is in: an unsettled pipeline is
// DecisionWait, a missing or unread fact is DecisionCannotJudge. A pipeline
// that was `skipped` without running a job (a `[skip ci]` commit) is not a
// candidate at all (skippedWithoutJobs): with nothing else it is DecisionWait
// until the deadline, then UNVERIFIABLE by J1.TIMEOUT. Once `now`
// has reached Fix.VerifyDeadline, what is still unsettled or unread at JUDGE
// TIME (the pipeline list, a job list, a trace, the definition delta) is
// UNVERIFIABLE in J5's order, so a forgery that IS visible still outranks it.
// Three things are never bounded by the deadline and stay DecisionCannotJudge
// for as long as they last: a missing RECORDED fact, an unread merge request
// (without it there is no telling whose commit would be judged), and a
// malformed fact (a pipeline or job object without its id).
//
// Where the plan left a choice, the reading taken is recorded in the plan's
// evidence log (section 17, "JUDGE"). In short: a definition construct only
// the HEAD cannot resolve is FORGED, not UNVERIFIABLE; any definition change
// without the dispatch's `ci` lift is FORGED; a job list that is incomplete
// past the deadline proves nothing but WEAKENED; and J4 reads one LATEST
// attempt per candidate pipeline, a failure in any one of them counting.
func Judge(f JudgeFacts, now time.Time) Result {
	// The recorded facts and the merge request are needed for ANY answer,
	// before or after the deadline: without them there is not even a fix to
	// call unverifiable.
	if missing := missingFacts(f, now); len(missing) > 0 {
		return cannotJudge(RuleFacts, missing, Result{})
	}

	// J0 identity.
	switch {
	case f.MR.State == MRStateMerged:
		return Result{Decision: DecisionStopped, State: StateMerged, Rule: RuleJ0Merged,
			Detail: "the merge request was merged; confirmation on the target takes over (plan 7.4)"}
	case f.MR.State == MRStateClosed:
		return Result{Decision: DecisionStopped, State: StateClosedUnmerged, Rule: RuleJ0Closed,
			Detail: "the merge request was closed without being merged"}
	case f.MR.State != MRStateOpened:
		return cannotJudge(RuleFacts, []string{fmt.Sprintf("mr.state: %q is neither opened, closed nor merged", f.MR.State)}, Result{})
	case f.MR.SHA != f.Fix.FixCommitSHA:
		return Result{Decision: DecisionStopped, State: StateTakenOver, Rule: RuleJ0TakenOver,
			Detail: "the merge request's head is not gonk's commit: somebody else pushed; gonk stops judging and never merges"}
	}

	past := !now.Before(f.Fix.VerifyDeadline)
	var res Result
	// unverifiable: what J1 and J2 could not establish, in step order.
	// forged: what J2 found. unread: facts to look for again -- before the
	// deadline any of them means no verdict this pass.
	var unverifiable, forged []cause
	var unread, unreadTraces []string

	// J1 settle. C = the MR's pipelines of the fix commit that are not child
	// pipelines.
	var candidates []Pipeline
	// complete: every attempt of every candidate pipeline is known.
	complete := true
	if !f.Pipelines.Read {
		unread = append(unread, "pipelines: the merge request's pipelines were not read")
		unverifiable = append(unverifiable, cause{RuleJ1Unread, "the merge request's pipelines could not be read before the verify deadline"})
		complete = false
	} else {
		for _, p := range f.Pipelines.Pipelines {
			if p.ID == 0 || p.SHA == "" || p.Source == "" {
				return cannotJudge(RuleFacts, []string{"pipelines: a pipeline without id, sha or source"}, Result{})
			}
			if p.SHA != f.Fix.FixCommitSHA {
				// Not a candidate, whatever it is. One shape is remembered so
				// the outcome can name it (gonk-ow8u).
				if mergedResultPipeline(p, f.Fix.MRIID) { // j1:merged-result
					res.MergedResult = append(res.MergedResult, p.ID)
				}
				continue
			}
			if p.Source == SourceParentPipeline {
				continue
			}
			if skippedWithoutJobs(p) {
				res.Skipped = append(res.Skipped, p.ID)
				continue
			}
			candidates = append(candidates, p)
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		sort.Slice(res.Skipped, func(i, j int) bool { return res.Skipped[i] < res.Skipped[j] })
		sort.Slice(res.MergedResult, func(i, j int) bool { return res.MergedResult[i] < res.MergedResult[j] })
		settled := len(candidates) > 0
		// pendingDetail: the first candidate that has a terminal status while
		// one of its job attempts has not finished (gonk-5b2f).
		pendingDetail := ""
		for _, c := range candidates {
			res.Candidates = append(res.Candidates, c.ID)
			if !settledStatus(c.Status) {
				settled = false
			}
			if a, found := activeAttempt(c); found {
				settled = false
				if pendingDetail == "" {
					pendingDetail = fmt.Sprintf("pipeline %d is %q but its job %q (attempt %d) is still %q", c.ID, c.Status, a.Name, a.ID, a.Status)
				}
			}
		}
		if !settled && !past {
			res.Decision, res.State, res.Rule = DecisionWait, StateVerifying, RuleJ1Wait
			res.Detail = "no pipeline of the fix commit exists yet"
			if len(res.Skipped) > 0 {
				res.Detail = skippedText(res.Skipped) + "; waiting for one to be started"
			}
			if len(candidates) == 0 && len(res.MergedResult) > 0 { // j1:merged-wait
				res.Detail = mergedResultText(res.MergedResult) + "; waiting for a pipeline of the fix commit itself"
			}
			if len(candidates) > 0 {
				res.Detail = "a pipeline of the fix commit has not finished"
			}
			if pendingDetail != "" {
				res.Detail = pendingDetail
			}
			return res
		}
		if !settled {
			detail := "the pipelines of the fix commit had not all finished (or none existed) at the verify deadline"
			if len(candidates) == 0 && len(res.Skipped) > 0 {
				detail = skippedText(res.Skipped) + ", and none was started before the verify deadline"
			}
			if pendingDetail != "" {
				detail = pendingDetail + " at the verify deadline"
			}
			rule := RuleJ1Timeout
			if len(candidates) == 0 && len(res.MergedResult) > 0 { // j1:merged-timeout
				// Nothing of the fix commit ever ran, and something of the
				// merge request did -- on a commit that is not the fix's.
				rule, detail = RuleJ1MergedResult, mergedResultText(res.MergedResult)+", and no pipeline of the fix commit itself finished before the verify deadline"
			}
			unverifiable = append(unverifiable, cause{rule, detail})
		}
	}

	// J2 definition integrity.
	if !f.Definition.Read || !knownKind(f.Definition.Delta.Kind) {
		unread = append(unread, "definition: the base-vs-head CI definition delta was not computed")
		unverifiable = append(unverifiable, cause{RuleJ2Unread, "the CI definition could not be compared before the verify deadline"})
	} else {
		c := judgeDefinition(f.Definition.Delta, f.Fix)
		switch c.rule {
		case "":
			res.Definition = f.Definition.Delta.Kind
		case RuleJ2Unresolvable:
			unverifiable = append(unverifiable, c)
		default:
			forged = append(forged, c)
		}
	}

	// The attempts: every job object of every candidate pipeline.
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

	// J3 per diagnosed job, over all attempts in all candidate pipelines.
	sameVersions := f.Fix.SignatureVersion == pipelinectx.SignatureVersion &&
		f.Fix.ExcerptVersion == pipelinectx.ExcerptVersion
	inT := map[string]bool{}
	for _, t := range f.Fix.Targets {
		inT[t.Name()] = true
		var attempts []Attempt
		for _, c := range candidates {
			for _, a := range c.Jobs {
				if a.Name == t.Name() {
					attempts = append(attempts, a)
				}
			}
		}
		j := judgeJob(t, attempts, complete, sameVersions)
		if j.Rule == RuleJ3Unknown {
			unreadTraces = append(unreadTraces, fmt.Sprintf("trace: the log of a failed attempt of job %q was not read", j.Job))
		}
		res.Jobs = append(res.Jobs, j)
	}

	// Before the deadline nothing is decided on partial facts.
	if !past && len(unread) > 0 {
		return cannotJudge(RuleFacts, append(unread, unreadTraces...), res)
	}
	if !past && len(unreadTraces) > 0 {
		return cannotJudge(RuleJ3Unknown, unreadTraces, res)
	}
	// Past it, what is still unknown was not observed.
	for i := range res.Jobs {
		if res.Jobs[i].Rule == RuleJ3Unknown {
			res.Jobs[i].Rule = RuleJ3Unread
		}
	}

	// J4 every other job, latest attempt per candidate pipeline, against the
	// base pipeline.
	res.Others = judgeOthers(f.Fix.BaseJobs, inT, candidates)

	// J5 verdict, first match.
	decide := func(v Verdict, rule Rule, detail string) Result {
		res.Decision, res.Verdict, res.Rule, res.Detail = DecisionVerdict, v, rule, detail
		res.State, _ = StateFor(v)
		return res
	}
	// FORGED: a J2 violation, or any WEAKENED.
	if len(forged) > 0 {
		return decide(VerdictForged, forged[0].rule, forged[0].detail)
	}
	if j, found := firstJob(res.Jobs, RuleJ3Weakened); found {
		return decide(VerdictForged, j.Rule, fmt.Sprintf("job %q was allowed to fail in the fix's pipeline", j.Job))
	}
	// NOT_FIXED: any RECURS.
	if j, found := firstJob(res.Jobs, RuleJ3Recurs); found {
		return decide(VerdictNotFixed, j.Rule, fmt.Sprintf("job %q still fails the way it was diagnosed", j.Job))
	}
	// FLAKY.
	if j, found := firstJob(res.Jobs, RuleJ3FlakySame, RuleJ3FlakyChanged); found {
		return decide(VerdictFlaky, j.Rule, fmt.Sprintf("job %q both failed and succeeded across its %d attempts", j.Job, j.Attempts))
	}
	// UNVERIFIABLE: from J1 / J2, or any NOT_OBSERVED.
	if len(unverifiable) > 0 {
		return decide(VerdictUnverifiable, unverifiable[0].rule, unverifiable[0].detail)
	}
	// Written as "any finding left that is neither PASSED nor CHANGED", so
	// a finding this ladder has no rung for can never fall through to FIXED.
	if j, found := firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3Changed); found {
		return decide(VerdictUnverifiable, j.Rule, fmt.Sprintf("job %q was not observed passing or failing in the fix's pipeline (%s)", j.Job, j.Rule))
	}
	// REGRESSED.
	if o, found := firstOther(res.Others, RuleJ4Regressed); found {
		return decide(VerdictRegressed, o.Rule, fmt.Sprintf("job %q passed on the base pipeline and fails in pipeline %d", o.Job, o.PipelineID))
	}
	// FIXED_NEW_FAILURE: any CHANGED, REMAINING or REVEALED.
	if j, found := firstJob(res.Jobs, RuleJ3Changed); found {
		return decide(VerdictFixedNewFailure, j.Rule, fmt.Sprintf("job %q no longer fails the way it was diagnosed; it fails differently", j.Job))
	}
	if o, found := firstOther(res.Others, RuleJ4Remaining, RuleJ4Revealed, RuleJ4RegressedAllowed); found {
		return decide(VerdictFixedNewFailure, o.Rule, fmt.Sprintf("the diagnosed failure is gone; job %q fails in pipeline %d", o.Job, o.PipelineID))
	}
	// FIXED: every t PASSED (nothing else is left: see the UNVERIFIABLE rung)
	// and every c in C success -- asserted, not assumed.
	for _, c := range candidates {
		if c.Status != StatusSuccess {
			return decide(VerdictUnverifiable, RuleJ5NotSuccess,
				fmt.Sprintf("pipeline %d is %q and no failed job explains it", c.ID, c.Status))
		}
	}
	return decide(VerdictFixed, RuleJ5Fixed, "every diagnosed job passed on every attempt and every pipeline of the fix succeeded")
}

// cause is a finding of J1 or J2: the rule and its words.
type cause struct {
	rule   Rule
	detail string
}

// cannotJudge is the no-verdict answer. partial carries what was established
// before the missing fact was met (candidates, job findings), for the log.
func cannotJudge(rule Rule, missing []string, partial Result) Result {
	return Result{
		Decision:     DecisionCannotJudge,
		Rule:         rule,
		Detail:       "no verdict: " + strings.Join(missing, "; "),
		Missing:      missing,
		Candidates:   partial.Candidates,
		Skipped:      partial.Skipped,
		MergedResult: partial.MergedResult,
		Jobs:         partial.Jobs,
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
	if !f.Fix.Recorded {
		return append(missing, "fix.recorded: no fix record was read")
	}
	fix := f.Fix
	miss(len(fix.Targets) == 0, "fix.targets: the target set T is empty")
	names := map[string]bool{}
	for _, t := range fix.Targets {
		miss(t.Name() == "", "fix.targets: a target without a job name")
		miss(names[t.Name()], fmt.Sprintf("fix.targets: job %q is listed twice", t.Name()))
		names[t.Name()] = true
	}
	miss(fix.BasePipelineID == 0, "fix.base_pipeline_id: no base pipeline")
	base := map[string]BaseJob{}
	for _, b := range fix.BaseJobs {
		miss(b.Status == "", fmt.Sprintf("fix.base_jobs: job %q has no status", b.Name))
		if _, dup := base[b.Name]; dup {
			miss(true, fmt.Sprintf("fix.base_jobs: job %q is listed twice", b.Name))
		}
		base[b.Name] = b
	}
	for _, t := range fix.Targets {
		b, has := base[t.Name()]
		miss(!has, fmt.Sprintf("fix.base_jobs: the diagnosed job %q is not among the base pipeline's jobs", t.Name()))
		miss(has && b.Status != StatusFailed, fmt.Sprintf("fix.base_jobs: the diagnosed job %q did not fail on the base pipeline", t.Name()))
		miss(has && b.AllowFailure, fmt.Sprintf("fix.base_jobs: the diagnosed job %q was allowed to fail on the base pipeline", t.Name()))
	}
	miss(fix.FixBaseSHA == "", "fix.fix_base_sha: no base SHA")
	miss(fix.FixCommitSHA == "", "fix.fix_commit_sha: no fix commit")
	miss(fix.MRIID == 0, "fix.mr_iid: no merge request")
	miss(fix.ExcerptVersion == 0, "fix.excerpt_version: not recorded")
	miss(fix.SignatureVersion == 0, "fix.signature_version: not recorded")
	miss(!contains([]string{CIExecDeny, CIExecManualRun, CIExecAuto}, fix.CIExecChanges),
		fmt.Sprintf("fix.ci_exec_changes: %q is not deny, manual-run or auto", fix.CIExecChanges))
	miss(fix.VerifyDeadline.IsZero(), "fix.verify_deadline: no deadline")

	if !f.MR.Read {
		return append(missing, "mr: the merge request was not read")
	}
	miss(f.MR.IID != fix.MRIID, fmt.Sprintf("mr.iid: read !%d, the fix is !%d", f.MR.IID, fix.MRIID))
	miss(f.MR.SHA == "", "mr.sha: the merge request has no head SHA")
	return missing
}

// activeJobStatuses are the job statuses of an attempt that has not finished
// and will, or may, still run by itself (bead gonk-5b2f). `manual` is NOT one:
// nothing runs it until a human does, and a pipeline does not wait for it.
var activeJobStatuses = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"}

// ActiveJobStatuses is the list J1 holds a verdict for (a copy).
func ActiveJobStatuses() []string { return append([]string(nil), activeJobStatuses...) }

// SourceMergeRequestEvent is the pipeline source of a merge request pipeline
// of any kind (detached, merged results, merge train).
const SourceMergeRequestEvent = "merge_request_event"

// mergedResultPipeline reports whether p -- a pipeline of the merge request
// whose sha is NOT the fix commit's -- is a merged-results or merge-train
// pipeline of merge request mrIID: source `merge_request_event`, and ref
// exactly refs/merge-requests/<mrIID>/merge or .../train.
//
// WHY SUCH A PIPELINE IS NEVER A CANDIDATE (gonk-ow8u). GitLab runs it on "a
// temporary merged commit that combines code from the source and target
// branches", which "doesn't exist in either branch"
// (docs.gitlab.com/ci/pipelines/merged_results_pipelines): the pipeline's
// `sha` is that commit's. The REST pipeline object documents no field naming
// the source commit it was merged from (docs.gitlab.com/api/pipelines: id,
// iid, project_id, sha, ref, status, source, before_sha, ...); GitLab states
// it only to the jobs themselves, as CI_MERGE_REQUEST_SOURCE_BRANCH_SHA
// (docs.gitlab.com/ci/variables/predefined_variables). The merge request's
// own `sha`, `diff_refs.head_sha` and `head_pipeline` describe its CURRENT
// head, not the commit a given past pipeline was built from. So nothing
// GitLab asserts through the API ties such a pipeline to FixCommitSHA, and a
// verdict that needs "this ran the fix commit" cannot be given from it:
// neither FIXED when it is green, nor NOT_FIXED when it is red (its tree
// also holds whatever the target branch gained since the fix was written).
//
// What the shape IS used for: when it is all there is at the deadline, the
// outcome is UNVERIFIABLE by J1.MERGED_RESULT -- naming what happened --
// instead of J1.TIMEOUT's "no pipeline finished". Misreading the shape can
// therefore only change that wording, never a verdict.
//
// The `/merge` ref is in GitLab's documentation (the list-pipelines example
// of docs.gitlab.com/api/pipelines shows `refs/merge-requests/<iid>/merge`).
// The `/train` ref is NOT documented there; it is what GitLab's source uses
// for a merge train, taken on that basis alone. Neither was observed: the
// instance this was built against is Community Edition, where a merge
// request pipeline's ref is refs/merge-requests/<iid>/head and its sha the
// source head (observed, project 75, GitLab 19.4.1).
func mergedResultPipeline(p Pipeline, mrIID int64) bool {
	if p.Source != SourceMergeRequestEvent || mrIID <= 0 {
		return false
	}
	base := fmt.Sprintf("refs/merge-requests/%d/", mrIID)
	return p.Ref == base+"merge" || p.Ref == base+"train"
}

// mergedResultText names the merged-results pipelines in a detail.
func mergedResultText(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return fmt.Sprintf("this merge request's pipeline(s) %s ran on GitLab's merged-results commit, not on the fix commit, "+
		"and cannot be attributed to it", strings.Join(parts, ", "))
}

// activeAttempt is the lowest-id job attempt of a candidate pipeline that is
// still active. GitLab can report a pipeline `failed` while an automatic
// retry of the failed job is still `created` and has not run (observed on
// project 75, GitLab 19.4.1, pipeline 3346; testdata/retry_pending_facts.json):
// a pipeline status alone does not say that every attempt was made. A
// candidate with an active attempt is NOT settled, whatever its status --
// wait, then J1.TIMEOUT. This can only withhold a verdict, never produce one:
// it is a pass for nothing.
//
// A pipeline whose jobs were not listed is not looked at here: an unread job
// list is already "no verdict" (cannot-judge) further down.
func activeAttempt(p Pipeline) (Attempt, bool) {
	var first Attempt
	found := false
	for _, a := range p.Jobs {
		if !activeJobStatus(a.Status) {
			continue
		}
		if !found || a.ID < first.ID {
			first, found = a, true
		}
	}
	return first, found
}

func activeJobStatus(s string) bool {
	for _, active := range activeJobStatuses {
		if s == active {
			return true
		}
	}
	return false
}

// settledStatus: J1's terminal pipeline statuses. `skipped` is one of them for
// a pipeline that has jobs; one that has none never gets here
// (skippedWithoutJobs).
func settledStatus(s string) bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped:
		return true
	}
	return false
}

// skippedWithoutJobs: J1's one exclusion by status. A pipeline GitLab created
// for a `[skip ci]` commit is `skipped` and has NO job objects (observed on
// project 75, GitLab 19.4.1: one such pipeline per source). It ran nothing and
// never will, so it is neither evidence nor the end of the wait: plan 5.5's
// `awaiting-run` is exactly the time between it and the pipeline a maintainer
// starts (gonk-vi9k). "No jobs" must be KNOWN -- listed, with retried attempts
// -- or the pipeline stays a candidate and its unread jobs hold the verdict.
// A skipped pipeline that HAS jobs is a candidate like any other.
func skippedWithoutJobs(p Pipeline) bool {
	return p.Status == StatusSkipped && p.JobsRead && p.IncludeRetried && len(p.Jobs) == 0
}

func skippedText(ids []int64) string {
	return fmt.Sprintf("the %d pipeline(s) of the fix commit so far were skipped without running a job (%v; a `[skip ci]` commit)", len(ids), ids)
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

// judgeDefinition is J2 over a computed delta. The zero cause means the
// definition holds (none, preserving, or a permitted exec-changing).
//
// The delta's lists are read as well as its Kind, and the worse of the two
// decides: a violation that is listed is a violation whatever Kind says.
func judgeDefinition(d cidef.Delta, fix FixFacts) cause {
	// D1 / D2 / D3 broken: FORGED. No lift changes this.
	if d.Kind == cidef.KindViolation {
		return cause{RuleJ2Violation, "the fix changed the CI definition it is judged by: " + violationText(d)}
	}
	if len(d.Violations) > 0 {
		return cause{RuleJ2Violation, "the fix changed the CI definition it is judged by: " + violationText(d)}
	}
	// Something only the HEAD cannot resolve was introduced by the change
	// being judged. The base resolved; the fix made its own definition
	// uncheckable. That is the fix's doing, not ours: FORGED.
	for _, u := range d.Unresolvable {
		if u.Side == cidef.SideHead {
			return cause{RuleJ2HeadUnresolvable, fmt.Sprintf(
				"the fix introduced a CI construct that cannot be compared (%s): %s", where(u.Job, u.Key), u.Reason)}
		}
	}
	// Unresolvable on the base, or on both sides: ours. UNVERIFIABLE.
	if d.Kind == cidef.KindUnresolvable {
		return cause{RuleJ2Unresolvable, "the CI definition cannot be compared: " + unresolvableText(d)}
	}
	if len(d.Unresolvable) > 0 {
		return cause{RuleJ2Unresolvable, "the CI definition cannot be compared: " + unresolvableText(d)}
	}
	// Any change at all needs the `ci` lift of the DISPATCH.
	if d.Kind != cidef.KindNone && !contains(fix.Lift, LiftCI) {
		return cause{RuleJ2Unlifted, fmt.Sprintf("the CI definition changed (%s) and the dispatch did not lift the `ci` class", d.Kind)}
	}
	// An execution change additionally needs ci_exec_changes != deny.
	if d.Kind == cidef.KindExecChanging && fix.CIExecChanges == CIExecDeny {
		return cause{RuleJ2ExecDenied, "the fix changes what the diagnosed job executes and ci_exec_changes is deny"}
	}
	return cause{}
}

func violationText(d cidef.Delta) string {
	if len(d.Violations) == 0 {
		return "a rule of plan 6.2 is broken"
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

// judgeJob is J3 for one job of T. attempts is A: every job object of that
// name in any candidate pipeline, retried attempts included. complete is
// false when A may be missing attempts (a job list unread, or read without
// retried attempts) -- only reached past the deadline. sameVersions is false
// when this build cannot recompute a signature under the recorded versions.
func judgeJob(t Target, attempts []Attempt, complete, sameVersions bool) JobFinding {
	f := JobFinding{Job: t.Name(), Attempts: len(attempts)}
	weakened, same, unknown, incomparable := false, false, false, false
	for _, a := range attempts {
		if a.AllowFailure {
			weakened = true
		}
		if a.Status == StatusSuccess {
			f.Succeeded++
		}
		if a.Status != StatusFailed {
			continue
		}
		f.Failed++
		// same(a) = key(a) == t.key OR t.generic OR signature(a) is "" or
		// generic. A generic target needs no trace: every failure is "the
		// same".
		if t.Generic {
			same = true
			continue
		}
		if !sameVersions {
			incomparable = true
			continue
		}
		if !a.Trace.Read {
			unknown = true
			continue
		}
		key := pipelinectx.JobFailureKey{
			Stage: a.Stage, Name: t.Name(), FailureReason: a.FailureReason,
			ErrorSignature: pipelinectx.ErrorSignature(a.Trace.FirstError),
		}
		switch {
		case key == t.Key:
			same = true
		case pipelinectx.GenericFirstError(a.Trace.FirstError):
			same = true
		case f.NewSignature == "":
			f.NewSignature = key.ErrorSignature
		}
	}
	f.Rule = jobRule(f, weakened, same, unknown, incomparable, complete)
	if f.Rule != RuleJ3Changed && f.Rule != RuleJ3FlakyChanged {
		f.NewSignature = ""
	}
	return f
}

// jobRule is J3's ladder, in the plan's order.
func jobRule(f JobFinding, weakened, same, unknown, incomparable, complete bool) Rule {
	// WEAKENED holds whatever else is or is not known: one attempt that was
	// allowed to fail is enough, and no further attempt can undo it.
	if weakened {
		return RuleJ3Weakened
	}
	// Past the deadline with attempts possibly missing, nothing else can be
	// said: an unseen attempt could turn any other finding into another.
	if !complete {
		return RuleJ3Unread
	}
	if f.Attempts == 0 {
		return RuleJ3Absent
	}
	if f.Failed > 0 {
		if incomparable {
			return RuleJ3Versions
		}
		if unknown {
			return RuleJ3Unknown
		}
		if same && f.Succeeded > 0 {
			return RuleJ3FlakySame
		}
		if same {
			return RuleJ3Recurs
		}
		if f.Succeeded > 0 {
			return RuleJ3FlakyChanged
		}
		return RuleJ3Changed
	}
	if f.Succeeded == f.Attempts {
		return RuleJ3Passed
	}
	return RuleJ3Status
}

// judgeOthers is J4: for every job outside T, its LATEST attempt in each
// candidate pipeline against the base pipeline's job of that name. A failure
// in any one candidate pipeline counts. One finding per job, sorted by name.
func judgeOthers(baseJobs []BaseJob, inT map[string]bool, candidates []Pipeline) []OtherFinding {
	base := map[string]BaseJob{}
	for _, b := range baseJobs {
		base[b.Name] = b
	}
	// failed: the first candidate pipeline (by id) in which the job's latest
	// attempt failed. required: the first in which it failed and was NOT
	// allowed to fail.
	failed, required := map[string]int64{}, map[string]int64{}
	var names []string
	for _, c := range candidates {
		latest := map[string]Attempt{}
		for _, a := range c.Jobs {
			if inT[a.Name] {
				continue
			}
			if l, has := latest[a.Name]; !has || a.ID > l.ID {
				latest[a.Name] = a
			}
		}
		for name, a := range latest {
			if a.Status != StatusFailed {
				continue
			}
			if failed[name] == 0 {
				failed[name] = c.ID
				names = append(names, name)
			}
			if !a.AllowFailure && required[name] == 0 {
				required[name] = c.ID
			}
		}
	}
	sort.Strings(names)
	var out []OtherFinding
	for _, name := range names {
		b, has := base[name]
		o := OtherFinding{Job: name, PipelineID: failed[name], BaseStatus: b.Status}
		switch {
		case has && b.Status == StatusSuccess && required[name] != 0:
			o.Rule, o.PipelineID = RuleJ4Regressed, required[name]
		case has && b.Status == StatusSuccess:
			o.Rule = RuleJ4RegressedAllowed
		case has && b.Status == StatusFailed:
			o.Rule = RuleJ4Remaining
		default:
			o.Rule = RuleJ4Revealed
		}
		out = append(out, o)
	}
	return out
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

// firstJobNot is the first finding whose rule is none of the given ones.
func firstJobNot(jobs []JobFinding, rules ...Rule) (JobFinding, bool) {
	for _, j := range jobs {
		listed := false
		for _, r := range rules {
			listed = listed || j.Rule == r
		}
		if !listed {
			return j, true
		}
	}
	return JobFinding{}, false
}

func firstOther(others []OtherFinding, rules ...Rule) (OtherFinding, bool) {
	for _, o := range others {
		for _, r := range rules {
			if o.Rule == r {
				return o, true
			}
		}
	}
	return OtherFinding{}, false
}
