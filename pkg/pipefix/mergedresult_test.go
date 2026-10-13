package pipefix

import (
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/cidef"
)

// gonk-ow8u. On a project with merged results pipelines (or merge trains), a
// merge request's pipeline runs on a temporary commit GitLab makes by merging
// the source into the target: its sha is that commit's, not the fix commit's,
// and its ref is refs/merge-requests/<iid>/merge. J1 selects candidates by
// sha == FixCommitSHA, so no such pipeline is ever a candidate. Until
// 2026-10-12 that fix waited to the deadline and was then reported as "no
// pipeline of the fix's commit finished", which is not what happened.
//
// WHAT IS ESTABLISHED, and what is not. GitLab's documentation says the
// commit "doesn't exist in either branch"; no documented field of the REST
// pipeline object names the source commit a merged-results pipeline was
// built from (the job variable CI_MERGE_REQUEST_SOURCE_BRANCH_SHA does, and
// only inside the job). So such a pipeline CANNOT be attributed to the fix
// commit by a fact GitLab asserts, and Judge does not try: it is never a
// candidate, never a pass. What changed is that the outcome is NAMED --
// UNVERIFIABLE by J1.MERGED_RESULT -- and the pipelines are listed.

const shaMerged = "cccccccccccccccccccccccccccccccccccccccc"

// mergedResult turns the fixture's one pipeline into a merged-results
// pipeline of the same merge request: all green, on GitLab's merge commit.
func mergedResult(f *JudgeFacts) {
	p := pipe(f, 501)
	p.SHA, p.Ref = shaMerged, "refs/merge-requests/7/merge"
}

func TestJ1AMergedResultsPipelineIsNeverACandidateAndIsNamed(t *testing.T) {
	if shaMerged == shaFix {
		t.Fatal("fixture: the merged commit is the fix commit")
	}
	cases := []judgeCase{
		// ---- THE SHAPE: green on the merge commit, nothing on the fix commit ----
		{name: "a green merged-results pipeline before the deadline: wait, never a pass", mut: mergedResult,
			decision: DecisionWait, state: StateVerifying, rule: RuleJ1Wait},
		{name: "a green merged-results pipeline past the deadline: UNVERIFIABLE, named", late: true, mut: mergedResult,
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1MergedResult,
			job: RuleJ3Absent},
		{name: "a merge train pipeline past the deadline: the same", late: true,
			mut: func(f *JudgeFacts) {
				mergedResult(f)
				pipe(f, 501).Ref = "refs/merge-requests/7/train"
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1MergedResult,
			job: RuleJ3Absent},
		{name: "a FAILED merged-results pipeline is no verdict against the fix either", late: true,
			mut: func(f *JudgeFacts) {
				mergedResult(f)
				p := pipe(f, 501)
				p.Status = StatusFailed
				p.Jobs[1] = failedWith(p.Jobs[1].ID, "unit-tests", lineOld)
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1MergedResult,
			job: RuleJ3Absent},

		// ---- NEAR MISSES: a pipeline OF THE FIX COMMIT is judged as before ----
		{name: "a plain merge request pipeline of the fix commit: fixed (unchanged)",
			mut:      func(f *JudgeFacts) { pipe(f, 501).Ref = "refs/merge-requests/7/head" },
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "a pipeline with no ref recorded: fixed (unchanged)",
			mut:      func(f *JudgeFacts) { pipe(f, 501).Ref = "" },
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "a merged-results pipeline AND a branch pipeline of the fix commit: the branch pipeline is judged",
			mut: func(f *JudgeFacts) {
				mergedResult(f)
				secondPipeline(f)
			},
			decision: DecisionVerdict, verdict: VerdictFixed, state: StateFixed, rule: RuleJ5Fixed, job: RuleJ3Passed, def: cidef.KindNone},
		{name: "the fix commit's own pipeline still running past the deadline is a TIMEOUT, not this",
			late: true,
			mut: func(f *JudgeFacts) {
				mergedResult(f)
				secondPipeline(f).Status = "running"
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout, job: RuleJ3Passed},

		// ---- NOT the shape: these are other commits' pipelines, as before ----
		{name: "another commit's plain pipeline (an earlier attempt): TIMEOUT as before", late: true,
			mut: func(f *JudgeFacts) {
				p := pipe(f, 501)
				p.SHA, p.Ref = shaMerged, "refs/merge-requests/7/head"
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout, job: RuleJ3Absent},
		{name: "a merge ref of ANOTHER merge request is not this one's", late: true,
			mut: func(f *JudgeFacts) {
				mergedResult(f)
				pipe(f, 501).Ref = "refs/merge-requests/77/merge"
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout, job: RuleJ3Absent},
		{name: "the merge ref with a push source is not a merge request pipeline", late: true,
			mut: func(f *JudgeFacts) {
				mergedResult(f)
				pipe(f, 501).Source = "push"
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout, job: RuleJ3Absent},
		{name: "a ref that only starts like the merge ref", late: true,
			mut: func(f *JudgeFacts) {
				mergedResult(f)
				pipe(f, 501).Ref = "refs/merge-requests/7/merge-queue"
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout, job: RuleJ3Absent},
		{name: "a branch NAMED like the merge ref's tail", late: true,
			mut: func(f *JudgeFacts) {
				mergedResult(f)
				pipe(f, 501).Ref = "merge-requests/7/merge"
			},
			decision: DecisionVerdict, verdict: VerdictUnverifiable, state: StateUnverifiable, rule: RuleJ1Timeout, job: RuleJ3Absent},
	}
	runJudgeCases(t, cases)

	// The result names the pipelines and says why, in words a note can carry.
	f := goodFacts()
	mergedResult(&f)
	f.Pipelines.Pipelines = append(f.Pipelines.Pipelines, Pipeline{ID: 499, SHA: shaMerged, Ref: "refs/merge-requests/7/merge",
		Source: "merge_request_event", Status: StatusFailed})
	for _, now := range []struct {
		late bool
		rule Rule
	}{{false, RuleJ1Wait}, {true, RuleJ1MergedResult}} {
		at := tDeadline.Add(-1)
		if now.late {
			at = tDeadline
		}
		r := Judge(f, at)
		if r.Rule != now.rule || len(r.MergedResult) != 2 || r.MergedResult[0] != 499 || r.MergedResult[1] != 501 || len(r.Candidates) != 0 {
			t.Errorf("late=%v: rule %q merged-result %v candidates %v, want %s and [499 501] and none", now.late, r.Rule, r.MergedResult, r.Candidates, now.rule)
		}
		if !strings.Contains(r.Detail, "merged-results") || !strings.Contains(r.Detail, "499, 501") {
			t.Errorf("late=%v: the detail does not name the pipelines and the cause: %q", now.late, r.Detail)
		}
	}
}
