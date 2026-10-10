package pipefix

import (
	"fmt"

	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// AFTER THE MERGE: CONFIRMATION ON THE TARGET (plan 7.4).
//
// Once the fix merge request has merged, the first terminal-for-health
// pipeline on the target that comes after the merge says whether the
// diagnosed failure is gone -- by J3's rules, applied to that ONE pipeline:
//
//	every job of T PASSED or CHANGED            -> confirmed
//	some job of T fails the way it was          -> recurred
//	  diagnosed, on any attempt
//	some job of T did not run, or ran under     -> not-observed: this pipeline
//	  allow_failure, or cannot be compared         decides nothing; the next one
//	                                               after it is looked at
//	a job list or a failed attempt's log was    -> cannot-confirm: look again
//	  not read; an attempt is still active
//
// A GREEN PIPELINE IS NOT ENOUGH. Plan 7.4's first row reads "green ->
// confirmed"; a target pipeline can be green without the diagnosed job having
// run in it at all (a rule that skips it on the merge commit, a pipeline for
// another path), and that is not evidence the failure is gone. `confirmed` is
// given on the diagnosed jobs passing, exactly as FIXED is.
//
// Like Judge it is pure, and a read failure is never an answer.

// Confirmation says what one target pipeline established.
type Confirmation string

const (
	// ConfirmConfirmed: the diagnosed failure did not occur.
	ConfirmConfirmed Confirmation = "confirmed"
	// ConfirmRecurred: a diagnosed job failed the way it was diagnosed.
	ConfirmRecurred Confirmation = "recurred"
	// ConfirmNotObserved: this pipeline does not decide; look at the next.
	ConfirmNotObserved Confirmation = "not-observed"
	// ConfirmCannot: a fact is unread or the pipeline has not settled; look
	// at THIS pipeline again.
	ConfirmCannot Confirmation = "cannot-confirm"
)

// ConfirmFacts is what Confirm reads: the recorded target set with its
// versions, and ONE pipeline of the target that comes after the merge.
type ConfirmFacts struct {
	Targets          []Target
	ExcerptVersion   int
	SignatureVersion int
	// Pipeline: its jobs listed with include_retried=true, and the log of
	// every failed attempt of a job in T read.
	Pipeline Pipeline
}

// ConfirmResult is Confirm's answer.
type ConfirmResult struct {
	Confirmation Confirmation
	// State is the fix state the answer selects: confirmed, recurred, or ""
	// (the state stays `merged`).
	State  State
	Detail string
	Jobs   []JobFinding
}

// Confirm is plan 7.4 for one pipeline. First match, in this order: recurred
// (a recurrence that IS visible outranks everything), cannot-confirm,
// not-observed, confirmed.
func Confirm(f ConfirmFacts) ConfirmResult {
	p := f.Pipeline
	if len(f.Targets) == 0 {
		return ConfirmResult{Confirmation: ConfirmCannot, Detail: "no diagnosed job is recorded for this fix"}
	}
	if p.ID == 0 || !pipelinectx.TerminalForHealth(p.Status) {
		return ConfirmResult{Confirmation: ConfirmCannot,
			Detail: fmt.Sprintf("pipeline %d is %q: not a finished pipeline that says anything about the target's health", p.ID, p.Status)}
	}
	if !p.JobsRead || !p.IncludeRetried {
		return ConfirmResult{Confirmation: ConfirmCannot,
			Detail: fmt.Sprintf("the jobs of pipeline %d were not listed with every attempt", p.ID)}
	}
	for _, a := range p.Jobs {
		if a.ID == 0 || a.Name == "" || a.Status == "" {
			return ConfirmResult{Confirmation: ConfirmCannot,
				Detail: fmt.Sprintf("a job object of pipeline %d has no id, name or status", p.ID)}
		}
	}
	sameVersions := f.SignatureVersion == pipelinectx.SignatureVersion && f.ExcerptVersion == pipelinectx.ExcerptVersion
	res := ConfirmResult{}
	for _, t := range f.Targets {
		var attempts []Attempt
		for _, a := range p.Jobs {
			if a.Name == t.Name() {
				attempts = append(attempts, a)
			}
		}
		res.Jobs = append(res.Jobs, judgeJob(t, attempts, true, sameVersions))
	}
	// recurred: any attempt that failed the way it was diagnosed. A retry
	// that then passed does not launder it (FLAKY's first form).
	if j, found := firstJob(res.Jobs, RuleJ3Recurs, RuleJ3FlakySame); found {
		res.Confirmation, res.State = ConfirmRecurred, StateRecurred
		res.Detail = fmt.Sprintf("job %q failed the way it was diagnosed in pipeline %d, after the fix merged", j.Job, p.ID)
		return res
	}
	// An attempt that has not run yet (gonk-5b2f): the pipeline's status is
	// not the end of it.
	if a, active := activeAttempt(p); active {
		res.Confirmation = ConfirmCannot
		res.Detail = fmt.Sprintf("pipeline %d is %q but its job %q (attempt %d) is still %q", p.ID, p.Status, a.Name, a.ID, a.Status)
		return res
	}
	if j, found := firstJob(res.Jobs, RuleJ3Unknown); found {
		res.Confirmation = ConfirmCannot
		res.Detail = fmt.Sprintf("the log of a failed attempt of job %q in pipeline %d was not read", j.Job, p.ID)
		return res
	}
	// Anything left that is neither PASSED, CHANGED nor "failed differently
	// and passed" was not observed -- written as a negation, so a finding
	// this ladder has no rung for can never fall through to confirmed.
	if j, found := firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3Changed, RuleJ3FlakyChanged); found {
		res.Confirmation = ConfirmNotObserved
		res.Detail = fmt.Sprintf("job %q was not observed passing or failing in pipeline %d (%s)", j.Job, p.ID, j.Rule)
		return res
	}
	res.Confirmation, res.State = ConfirmConfirmed, StateConfirmed
	res.Detail = fmt.Sprintf("no diagnosed job failed the way it was diagnosed in pipeline %d", p.ID)
	return res
}
