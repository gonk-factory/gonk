package pipefix

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
)

// gonk-5b2f. GitLab can report a pipeline `failed` while a job attempt of it
// has not run yet: an automatic retry that is still `created`, and the later
// stage behind it. J1 used to settle on the pipeline's status alone, so Judge
// answered before the retry ran. A candidate is settled only when its status
// is terminal AND none of its job attempts is still active.

// loadRetryPending reads the facts `gonk-gate pipefix-judge` recorded on
// project 75 (GitLab 19.4.1, 2026-10-07, MR !55, pipeline 3346): the pipeline
// is `failed`, attempt 29204 of probe-status failed, its automatic retry
// 29212 and probe-report 29205 are `created`.
func loadRetryPending(t *testing.T) (JudgeFacts, time.Time) {
	t.Helper()
	raw, err := os.ReadFile("testdata/retry_pending_facts.json")
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
	if !f.Fix.Recorded || f.Fix.MRIID != 55 || !f.MR.Read || f.MR.State != MRStateOpened || f.MR.SHA != f.Fix.FixCommitSHA ||
		!f.Pipelines.Read || len(f.Pipelines.Pipelines) != 1 || !f.Definition.Read {
		t.Fatalf("the recorded facts are not the retry case: %+v", f)
	}
	p := f.Pipelines.Pipelines[0]
	status := map[int64]string{}
	for _, a := range p.Jobs {
		status[a.ID] = a.Name + "/" + a.Status
	}
	want := map[int64]string{
		29203: "probe-lint/success", 29204: "probe-status/failed",
		29205: "probe-report/created", 29212: "probe-status/created",
	}
	if p.ID != 3346 || p.Status != StatusFailed || p.SHA != f.Fix.FixCommitSHA || !p.JobsRead || !p.IncludeRetried || !reflect.DeepEqual(status, want) {
		t.Fatalf("recorded pipeline %+v is not `failed` with a failed attempt and a created retry", p)
	}
	if rec.Now.IsZero() || !rec.Now.Before(f.Fix.VerifyDeadline) {
		t.Fatalf("recorded at %s with deadline %s: not before the deadline", rec.Now, f.Fix.VerifyDeadline)
	}
	return f, rec.Now
}

func TestTheRecordedRetryPendingFactsAreNotSettled(t *testing.T) {
	f, now := loadRetryPending(t)

	t.Run("as recorded: the fix added the retry key", func(t *testing.T) {
		if f.Definition.Delta.Kind != cidef.KindViolation {
			t.Fatalf("recorded delta %q, want a violation", f.Definition.Delta.Kind)
		}
		r := Judge(f, now)
		if r.Decision != DecisionWait || r.Rule != RuleJ1Wait || r.State != StateVerifying || r.Verdict != "" {
			t.Fatalf("decision %q verdict %q by %q (%s); want wait by %s: the retry has not run", r.Decision, r.Verdict, r.Rule, r.Detail, RuleJ1Wait)
		}
		if !reflect.DeepEqual(r.Candidates, []int64{3346}) {
			t.Errorf("candidates %v, want [3346]", r.Candidates)
		}
		// Past the deadline the forgery that IS visible still decides.
		if late := Judge(f, f.Fix.VerifyDeadline); late.Verdict != VerdictForged || late.Rule != RuleJ2Violation {
			t.Errorf("at the deadline: verdict %q by %q, want FORGED by %s", late.Verdict, late.Rule, RuleJ2Violation)
		}
	})

	// The case the bead is about: the retry was NOT added by the fix (it is
	// in the base definition, or a human pressed retry). One failed attempt
	// and a pending one used to read J3.RECURS -> NOT_FIXED, a rung bought,
	// where the finished pipeline may be FLAKY.
	t.Run("the retry is the base definition's own", func(t *testing.T) {
		g := f
		g.Definition = DefinitionFacts{Read: true, Delta: cidef.Delta{Kind: cidef.KindNone}}
		r := Judge(g, now)
		if r.Decision != DecisionWait || r.Rule != RuleJ1Wait || r.Verdict != "" {
			t.Fatalf("decision %q verdict %q by %q (%s); want wait by %s", r.Decision, r.Verdict, r.Rule, r.Detail, RuleJ1Wait)
		}
		// The deadline bounds the wait, and what it yields is never a pass.
		late := Judge(g, g.Fix.VerifyDeadline)
		if late.Decision != DecisionVerdict || late.Verdict == VerdictFixed || late.Verdict == VerdictFixedNewFailure {
			t.Fatalf("at the deadline: decision %q verdict %q by %q; want a verdict that is not a pass", late.Decision, late.Verdict, late.Rule)
		}
		// The retry then runs and passes: FLAKY, which the early verdict hid.
		done := g
		p := g.Pipelines.Pipelines[0]
		p.Jobs = append([]Attempt{}, p.Jobs...)
		for i := range p.Jobs {
			if p.Jobs[i].Status == "created" {
				p.Jobs[i].Status = StatusSuccess
			}
		}
		p.Status = StatusSuccess
		done.Pipelines.Pipelines = []Pipeline{p}
		if r := Judge(done, now); r.Verdict != VerdictFlaky {
			t.Errorf("once the retry passed: verdict %q by %q, want FLAKY", r.Verdict, r.Rule)
		}
	})
}

// decidedActive is bead gonk-5b2f's decision, written out: the test below
// does not read the list from the code it checks.
var decidedActive = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"}

func TestActiveJobStatusesAreTheDecision(t *testing.T) {
	if !reflect.DeepEqual(decidedActive, ActiveJobStatuses()) {
		t.Fatalf("ActiveJobStatuses() = %v, want %v (bead gonk-5b2f's decision)", ActiveJobStatuses(), decidedActive)
	}
}

// Every active job status holds J1, under every terminal pipeline status;
// `manual` and the terminal job statuses do not.
func TestJ1AnActiveAttemptHoldsASettledPipeline(t *testing.T) {
	active := decidedActive
	for _, ps := range []string{StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped} {
		for _, js := range active {
			pending := func(f *JudgeFacts) {
				p := pipe(f, 501)
				p.Status = ps
				p.Jobs = append(p.Jobs, Attempt{ID: 9050, Name: "package", Stage: "package", Status: js})
			}
			t.Run("pipeline "+ps+", a job "+js, func(t *testing.T) {
				f := goodFacts()
				pending(&f)
				r := Judge(f, tEarly)
				if r.Decision != DecisionWait || r.Rule != RuleJ1Wait || r.State != StateVerifying || r.Verdict != "" {
					t.Fatalf("decision %q verdict %q by %q (%s); want wait by %s", r.Decision, r.Verdict, r.Rule, r.Detail, RuleJ1Wait)
				}
				if !bytes.Contains([]byte(r.Detail), []byte(js)) || !bytes.Contains([]byte(r.Detail), []byte("package")) {
					t.Errorf("detail %q does not name the job and its status", r.Detail)
				}
				late := Judge(f, tLate)
				if late.Decision != DecisionVerdict || late.Verdict == VerdictFixed || late.Verdict == VerdictFixedNewFailure {
					t.Fatalf("past the deadline: decision %q verdict %q by %q; want a verdict that is not a pass", late.Decision, late.Verdict, late.Rule)
				}
				if ps == StatusSuccess && (late.Verdict != VerdictUnverifiable || late.Rule != RuleJ1Timeout) {
					t.Errorf("past the deadline on a green pipeline: verdict %q by %q, want UNVERIFIABLE by %s", late.Verdict, late.Rule, RuleJ1Timeout)
				}
			})
		}
	}
	// What does NOT hold it: a manual job nobody started, and finished ones.
	for _, js := range []string{"manual", StatusSkipped, StatusCanceled, StatusSuccess} {
		t.Run("a job "+js+" does not hold it", func(t *testing.T) {
			f := goodFacts()
			p := pipe(&f, 501)
			p.Jobs = append(p.Jobs, Attempt{ID: 9050, Name: "deploy", Stage: "deploy", Status: js})
			if r := Judge(f, tEarly); r.Decision != DecisionVerdict || r.Verdict != VerdictFixed {
				t.Fatalf("decision %q verdict %q by %q (%s); want FIXED", r.Decision, r.Verdict, r.Rule, r.Detail)
			}
		})
	}
	// An active attempt in a pipeline that is NOT a candidate holds nothing.
	t.Run("an active job of another commit's pipeline does not hold it", func(t *testing.T) {
		f := goodFacts()
		p := secondPipeline(&f)
		p.SHA = shaOther
		p.Jobs[0].Status = "running"
		if r := Judge(f, tEarly); r.Verdict != VerdictFixed {
			t.Fatalf("verdict %q by %q, want FIXED", r.Verdict, r.Rule)
		}
	})
}

// A human pressing retry on the failed diagnosed job: while the new attempt
// is pending there is no verdict; it never launders the failure into a pass.
func TestAPendingHumanRetryIsWaitedFor(t *testing.T) {
	f := goodFacts()
	redPipeline(&f)
	setJob(&f, "unit-tests", failedWith(9002, "unit-tests", lineOld),
		Attempt{ID: 9010, Name: "unit-tests", Stage: "test", Status: "pending"})
	if r := Judge(f, tEarly); r.Decision != DecisionWait || r.Rule != RuleJ1Wait {
		t.Fatalf("retry pending: decision %q verdict %q by %q; want wait", r.Decision, r.Verdict, r.Rule)
	}
	setJob(&f, "unit-tests", failedWith(9002, "unit-tests", lineOld), ok(9010, "unit-tests", "test"))
	pipe(&f, 501).Status = StatusSuccess
	if r := Judge(f, tEarly); r.Verdict != VerdictFlaky || r.Rule != RuleJ3FlakySame {
		t.Fatalf("retry passed: verdict %q by %q; want FLAKY by %s", r.Verdict, r.Rule, RuleJ3FlakySame)
	}
}
