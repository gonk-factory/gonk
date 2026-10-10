package pipefix

import (
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// confirmFacts is a merged fix whose first post-merge target pipeline (701)
// is green and ran the one diagnosed job, which passed. Every row is this
// with ONE fact changed.
func confirmFacts() ConfirmFacts {
	return ConfirmFacts{
		Targets:          []Target{target("unit-tests", lineOld)},
		ExcerptVersion:   pipelinectx.ExcerptVersion,
		SignatureVersion: pipelinectx.SignatureVersion,
		Pipeline: Pipeline{ID: 701, SHA: shaOther, Source: "push", Status: StatusSuccess, JobsRead: true, IncludeRetried: true,
			Jobs: []Attempt{ok(9701, "compile", "build"), ok(9702, "unit-tests", "test"), ok(9703, "lint", "test")}},
	}
}

func setConfirmJob(f *ConfirmFacts, name string, attempts ...Attempt) {
	var jobs []Attempt
	for _, a := range f.Pipeline.Jobs {
		if a.Name != name {
			jobs = append(jobs, a)
		}
	}
	f.Pipeline.Jobs = append(jobs, attempts...)
}

// PLAN 7.4, ROW BY ROW -- and the row the plan's table does not have: a
// green pipeline in which the diagnosed job did not run confirms nothing.
func TestConfirmRows(t *testing.T) {
	red := func(f *ConfirmFacts) { f.Pipeline.Status = StatusFailed }
	for _, c := range []struct {
		name   string
		mut    func(*ConfirmFacts)
		want   Confirmation
		state  State
		job    Rule
		detail string
	}{
		{name: "green, the diagnosed job passed", want: ConfirmConfirmed, state: StateConfirmed, job: RuleJ3Passed},
		{name: "red for another reason, the diagnosed job passed",
			mut: func(f *ConfirmFacts) {
				red(f)
				setConfirmJob(f, "lint", failedWith(9703, "lint", lineNew))
			},
			want: ConfirmConfirmed, state: StateConfirmed, job: RuleJ3Passed},
		{name: "red, the diagnosed job fails DIFFERENTLY (the specific failure is gone)",
			mut:  func(f *ConfirmFacts) { red(f); setConfirmJob(f, "unit-tests", failedWith(9702, "unit-tests", lineNew)) },
			want: ConfirmConfirmed, state: StateConfirmed, job: RuleJ3Changed},
		{name: "green after a retry of a DIFFERENT failure",
			mut: func(f *ConfirmFacts) {
				setConfirmJob(f, "unit-tests", failedWith(9702, "unit-tests", lineNew), ok(9710, "unit-tests", "test"))
			},
			want: ConfirmConfirmed, state: StateConfirmed, job: RuleJ3FlakyChanged},
		{name: "red, the diagnosed job fails the way it was diagnosed",
			mut:  func(f *ConfirmFacts) { red(f); setConfirmJob(f, "unit-tests", failedWith(9702, "unit-tests", lineOld)) },
			want: ConfirmRecurred, state: StateRecurred, job: RuleJ3Recurs, detail: "unit-tests"},
		{name: "green only because a human retried the recurring failure: a retry does not launder it",
			mut: func(f *ConfirmFacts) {
				setConfirmJob(f, "unit-tests", failedWith(9702, "unit-tests", lineOld), ok(9710, "unit-tests", "test"))
			},
			want: ConfirmRecurred, state: StateRecurred, job: RuleJ3FlakySame},
		{name: "a generic target: any failure of the job is the same failure",
			mut: func(f *ConfirmFacts) {
				red(f)
				f.Targets[0].Generic = true
				setConfirmJob(f, "unit-tests", failedWith(9702, "unit-tests", lineNew))
			},
			want: ConfirmRecurred, state: StateRecurred, job: RuleJ3Recurs},
		// THE ROW THE TASK NAMES: confirmed when the diagnosed job did not run.
		{name: "GREEN, and the diagnosed job did not run at all",
			mut:  func(f *ConfirmFacts) { setConfirmJob(f, "unit-tests") },
			want: ConfirmNotObserved, state: "", job: RuleJ3Absent, detail: "unit-tests"},
		{name: "green, and the diagnosed job was skipped",
			mut: func(f *ConfirmFacts) {
				setConfirmJob(f, "unit-tests", Attempt{ID: 9702, Name: "unit-tests", Stage: "test", Status: StatusSkipped})
			},
			want: ConfirmNotObserved, job: RuleJ3Status},
		{name: "green, and the diagnosed job is manual",
			mut: func(f *ConfirmFacts) {
				setConfirmJob(f, "unit-tests", Attempt{ID: 9702, Name: "unit-tests", Stage: "test", Status: "manual"})
			},
			want: ConfirmNotObserved, job: RuleJ3Status},
		{name: "green because the diagnosed job is now allowed to fail",
			mut: func(f *ConfirmFacts) {
				a := failedWith(9702, "unit-tests", lineOld)
				a.AllowFailure = true
				setConfirmJob(f, "unit-tests", a)
			},
			want: ConfirmNotObserved, job: RuleJ3Weakened},
		{name: "recorded under other signature versions: cannot be compared",
			mut: func(f *ConfirmFacts) {
				red(f)
				f.SignatureVersion++
				setConfirmJob(f, "unit-tests", failedWith(9702, "unit-tests", lineNew))
			},
			want: ConfirmNotObserved, job: RuleJ3Versions},
		{name: "a second diagnosed job did not run, the first passed",
			mut: func(f *ConfirmFacts) {
				f.Targets = append(f.Targets, target("integration", lineNew))
			},
			want: ConfirmNotObserved, job: RuleJ3Passed, detail: "integration"},
		{name: "a second diagnosed job recurs, the first did not run: recurred outranks",
			mut: func(f *ConfirmFacts) {
				red(f)
				f.Targets = append(f.Targets, target("integration", lineNew))
				setConfirmJob(f, "unit-tests")
				f.Pipeline.Jobs = append(f.Pipeline.Jobs, failedWith(9705, "integration", lineNew))
			},
			want: ConfirmRecurred, state: StateRecurred, job: RuleJ3Absent, detail: "integration"},
		// what is not an answer
		{name: "the failed attempt's log was not read",
			mut: func(f *ConfirmFacts) {
				red(f)
				a := failedWith(9702, "unit-tests", lineOld)
				a.Trace = Trace{}
				setConfirmJob(f, "unit-tests", a)
			},
			want: ConfirmCannot, job: RuleJ3Unknown, detail: "log"},
		{name: "the jobs were not listed", mut: func(f *ConfirmFacts) { f.Pipeline.JobsRead = false }, want: ConfirmCannot, detail: "not listed"},
		{name: "the jobs were listed without retried attempts", mut: func(f *ConfirmFacts) { f.Pipeline.IncludeRetried = false },
			want: ConfirmCannot, detail: "not listed"},
		{name: "the pipeline is still running", mut: func(f *ConfirmFacts) { f.Pipeline.Status = "running" }, want: ConfirmCannot, detail: "running"},
		{name: "the pipeline was canceled", mut: func(f *ConfirmFacts) { f.Pipeline.Status = StatusCanceled }, want: ConfirmCannot},
		{name: "the pipeline has no id", mut: func(f *ConfirmFacts) { f.Pipeline.ID = 0 }, want: ConfirmCannot},
		{name: "a job object has no status",
			mut:  func(f *ConfirmFacts) { f.Pipeline.Jobs[0].Status = "" },
			want: ConfirmCannot, detail: "job object"},
		{name: "no diagnosed job is recorded", mut: func(f *ConfirmFacts) { f.Targets = nil }, want: ConfirmCannot, detail: "no diagnosed job"},
		{name: "the pipeline is failed and the diagnosed job's retry has not run (gonk-5b2f)",
			mut: func(f *ConfirmFacts) {
				red(f)
				setConfirmJob(f, "unit-tests", failedWith(9702, "unit-tests", lineNew),
					Attempt{ID: 9710, Name: "unit-tests", Stage: "test", Status: "created"})
			},
			want: ConfirmCannot, job: RuleJ3Changed, detail: "created"},
		{name: "a recurrence is visible although a later job has not run: recurred, at once",
			mut: func(f *ConfirmFacts) {
				red(f)
				setConfirmJob(f, "unit-tests", failedWith(9702, "unit-tests", lineOld))
				setConfirmJob(f, "lint", Attempt{ID: 9703, Name: "lint", Stage: "test", Status: "created"})
			},
			want: ConfirmRecurred, state: StateRecurred, job: RuleJ3Recurs},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := confirmFacts()
			if c.mut != nil {
				c.mut(&f)
			}
			r := Confirm(f)
			if r.Confirmation != c.want || r.State != c.state {
				t.Fatalf("confirmation %q state %q (%s), want %q / %q", r.Confirmation, r.State, r.Detail, c.want, c.state)
			}
			if r.Detail == "" || !strings.Contains(r.Detail, c.detail) {
				t.Errorf("detail %q, want it to name %q", r.Detail, c.detail)
			}
			if c.job != "" {
				var got Rule
				for _, j := range r.Jobs {
					if j.Job == "unit-tests" {
						got = j.Rule
					}
				}
				if got != c.job {
					t.Errorf("finding for unit-tests %q, want %q (%+v)", got, c.job, r.Jobs)
				}
			}
			// The state is set exactly for the two answers that are states.
			if (r.State != "") != (r.Confirmation == ConfirmConfirmed || r.Confirmation == ConfirmRecurred) {
				t.Errorf("state %q with confirmation %q", r.State, r.Confirmation)
			}
		})
	}
}

// Nothing unread is ever `confirmed`: every fact Confirm reads, withheld one
// at a time, never yields confirmed.
func TestNoUnreadFactIsEverConfirmed(t *testing.T) {
	for name, mut := range map[string]func(*ConfirmFacts){
		"jobs unread":             func(f *ConfirmFacts) { f.Pipeline.JobsRead = false },
		"retried attempts unread": func(f *ConfirmFacts) { f.Pipeline.IncludeRetried = false },
		"no jobs":                 func(f *ConfirmFacts) { f.Pipeline.Jobs = nil },
		"no targets":              func(f *ConfirmFacts) { f.Targets = nil },
		"no pipeline":             func(f *ConfirmFacts) { f.Pipeline = Pipeline{} },
		"the zero value":          func(f *ConfirmFacts) { *f = ConfirmFacts{} },
	} {
		f := confirmFacts()
		mut(&f)
		if r := Confirm(f); r.Confirmation == ConfirmConfirmed || r.State == StateConfirmed {
			t.Errorf("%s: confirmed (%s)", name, r.Detail)
		}
	}
}

func TestConfirmDoesNotModifyItsFacts(t *testing.T) {
	f := confirmFacts()
	before := len(f.Pipeline.Jobs)
	first := f.Pipeline.Jobs[0]
	Confirm(f)
	if len(f.Pipeline.Jobs) != before || f.Pipeline.Jobs[0] != first || len(f.Targets) != 1 {
		t.Fatal("Confirm changed its facts")
	}
}
