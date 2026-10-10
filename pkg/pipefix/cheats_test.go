package pipefix

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/effects"
)

// The cheats of plan 6.4. For each row that is caught at JUDGE's layers --
// the definition delta (J2, through the real pkg/cidef on hand-written YAML)
// and the runtime check (J1, J3) -- there is a fixture per layer: the fixed
// baseline with the cheat applied at THAT layer only, so exactly one
// predicate refuses it, asserted by rule code.

// ciBase is the invented project's CI definition at the fix base.
const ciBase = `
stages: [build, test, package]
compile:
  stage: build
  script: [make compile]
unit-tests:
  stage: test
  script: [make check]
lint:
  stage: test
  script: [make lint]
package:
  stage: package
  script: [make dist]
`

// edit returns ciBase with one exact replacement.
func edit(t *testing.T, old, new string) string {
	t.Helper()
	if strings.Count(ciBase, old) != 1 {
		t.Fatalf("fixture: %q occurs %d times in the base definition", old, strings.Count(ciBase, old))
	}
	return strings.Replace(ciBase, old, new, 1)
}

const unitTestsJob = "unit-tests:\n  stage: test\n  script: [make check]\n"

type cheatCase struct {
	cheat string // the plan's row
	name  string
	// layer: "static" (J2 over a real cidef delta) or "runtime" (J1/J3).
	layer string
	// head: for a static fixture, the CI definition at the fix commit.
	head func(t *testing.T) string
	// wantKind: what cidef must make of it (asserted, so the fixture is
	// proven to be the cheat it claims to be).
	wantKind cidef.Kind
	// unlifted: judge it WITHOUT the ci lift.
	unlifted bool
	execDeny bool
	// mut: for a runtime fixture, the cheat as GitLab would show it.
	mut  func(*JudgeFacts)
	late bool

	verdict Verdict
	rule    Rule
	def     cidef.Kind
}

var cheatCases = []cheatCase{
	// C1 delete the failing job (or rename it)
	{cheat: "C1", name: "the job is deleted from the definition", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, "") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C1", name: "the job is renamed in the definition", layer: "static",
		head:     func(t *testing.T) string { return edit(t, "unit-tests:", "unit-tests-v2:") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C1", name: "the job is in no pipeline of the fix", layer: "runtime",
		mut:     func(f *JudgeFacts) { setJob(f, "unit-tests") },
		verdict: VerdictUnverifiable, rule: RuleJ3Absent},
	{cheat: "C1", name: "a job of another name ran in its place", layer: "runtime",
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests")
			setJob(f, "unit-tests-v2", ok(9002, "unit-tests-v2", "test"))
		},
		verdict: VerdictUnverifiable, rule: RuleJ3Absent},
	// C2 allow_failure: true
	{cheat: "C2", name: "allow_failure: true in the definition", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  allow_failure: true\n") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C2", name: "the job object says allow_failure", layer: "runtime",
		mut: func(f *JudgeFacts) {
			a := failedWith(9002, "unit-tests", lineOld)
			a.AllowFailure = true
			setJob(f, "unit-tests", a)
		},
		verdict: VerdictForged, rule: RuleJ3Weakened},
	// C3 when: manual / when: never / start_in
	{cheat: "C3", name: "when: manual in the definition", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  when: manual\n") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C3", name: "when: never in the definition", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  when: never\n") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C3", name: "start_in in the definition", layer: "static",
		head: func(t *testing.T) string {
			return edit(t, unitTestsJob, unitTestsJob+"  when: delayed\n  start_in: 1 week\n")
		},
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C3", name: "the job is manual and nobody ran it", layer: "runtime",
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests", Attempt{ID: 9002, Name: "unit-tests", Stage: "test", Status: "manual"})
		},
		verdict: VerdictUnverifiable, rule: RuleJ3Status},
	{cheat: "C3", name: "the pipeline is blocked on the manual job until the deadline", layer: "runtime", late: true,
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests", Attempt{ID: 9002, Name: "unit-tests", Stage: "test", Status: "manual"})
			pipe(f, 501).Status = "manual"
		},
		verdict: VerdictUnverifiable, rule: RuleJ1Timeout},
	{cheat: "C3", name: "the job is scheduled for next week until the deadline", layer: "runtime", late: true,
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests", Attempt{ID: 9002, Name: "unit-tests", Stage: "test", Status: "scheduled"})
			pipe(f, 501).Status = "scheduled"
		},
		verdict: VerdictUnverifiable, rule: RuleJ1Timeout},
	// C4 narrow rules / only / except / workflow:rules
	{cheat: "C4", name: "rules added to the job", layer: "static",
		head: func(t *testing.T) string {
			return edit(t, unitTestsJob, unitTestsJob+"  rules:\n    - if: '$CI_COMMIT_BRANCH == \"main\"'\n")
		},
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C4", name: "only added to the job", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  only: [tags]\n") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C4", name: "except added to the job", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  except: [merge_requests]\n") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C4", name: "workflow:rules added", layer: "static",
		head: func(t *testing.T) string {
			return edit(t, "stages: [build, test, package]\n", "stages: [build, test, package]\nworkflow:\n  rules:\n    - when: never\n")
		},
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C4", name: "the job was skipped", layer: "runtime",
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests", Attempt{ID: 9002, Name: "unit-tests", Stage: "test", Status: StatusSkipped})
		},
		verdict: VerdictUnverifiable, rule: RuleJ3Status},
	{cheat: "C4", name: "the job is not in the pipeline", layer: "runtime",
		mut:     func(f *JudgeFacts) { setJob(f, "unit-tests") },
		verdict: VerdictUnverifiable, rule: RuleJ3Absent},
	{cheat: "C4", name: "no pipeline ever ran for the fix commit", layer: "runtime", late: true,
		mut:     func(f *JudgeFacts) { f.Pipelines.Pipelines = nil },
		verdict: VerdictUnverifiable, rule: RuleJ1Timeout},
	// C5 raise retry: or press retry until green
	{cheat: "C5", name: "retry raised in the definition", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  retry: 2\n") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C5", name: "default retry raised", layer: "static",
		head: func(t *testing.T) string {
			return edit(t, "stages: [build, test, package]\n", "stages: [build, test, package]\ndefault:\n  retry: 2\n")
		},
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	{cheat: "C5", name: "retried until green: two failures, then a pass", layer: "runtime",
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests",
				failedWith(9002, "unit-tests", lineOld), failedWith(9020, "unit-tests", lineOld), ok(9030, "unit-tests", "test"))
		},
		verdict: VerdictFlaky, rule: RuleJ3FlakySame},
	// C6 move the failing command into a new allowed-to-fail job
	{cheat: "C6", name: "the command moves to a new allowed-to-fail job: flagged exec-changing", layer: "static",
		head: func(t *testing.T) string {
			return edit(t, unitTestsJob,
				"unit-tests:\n  stage: test\n  script: [\"true\"]\nunit-tests-real:\n  stage: test\n  script: [make check]\n  allow_failure: true\n")
		},
		wantKind: cidef.KindExecChanging, verdict: VerdictFixed, rule: RuleJ5Fixed, def: cidef.KindExecChanging},
	{cheat: "C6", name: "the same, on a dispatch that lifted nothing", layer: "static", unlifted: true,
		head: func(t *testing.T) string {
			return edit(t, unitTestsJob,
				"unit-tests:\n  stage: test\n  script: [\"true\"]\nunit-tests-real:\n  stage: test\n  script: [make check]\n  allow_failure: true\n")
		},
		wantKind: cidef.KindExecChanging, verdict: VerdictForged, rule: RuleJ2Unlifted},
	{cheat: "C6", name: "the same, under ci_exec_changes: deny", layer: "static", execDeny: true,
		head: func(t *testing.T) string {
			return edit(t, unitTestsJob,
				"unit-tests:\n  stage: test\n  script: [\"true\"]\nunit-tests-real:\n  stage: test\n  script: [make check]\n  allow_failure: true\n")
		},
		wantKind: cidef.KindExecChanging, verdict: VerdictForged, rule: RuleJ2ExecDenied},
	// C11 swallow the exit code in the script
	{cheat: "C11", name: "an or-true appended to the script: flagged exec-changing", layer: "static",
		head:     func(t *testing.T) string { return edit(t, "script: [make check]", "script: [make check || true]") },
		wantKind: cidef.KindExecChanging, verdict: VerdictFixed, rule: RuleJ5Fixed, def: cidef.KindExecChanging},
	{cheat: "C11", name: "set +e in a before_script: flagged exec-changing", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  before_script: [set +e]\n") },
		wantKind: cidef.KindExecChanging, verdict: VerdictFixed, rule: RuleJ5Fixed, def: cidef.KindExecChanging},
	{cheat: "C11", name: "the exit code swallowed on a job that was NOT diagnosed", layer: "static",
		head:     func(t *testing.T) string { return edit(t, "script: [make lint]", "script: [make lint || true]") },
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	// C12 change image / services / variables
	{cheat: "C12", name: "image changed on the diagnosed job: flagged exec-changing", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  image: busybox\n") },
		wantKind: cidef.KindExecChanging, verdict: VerdictFixed, rule: RuleJ5Fixed, def: cidef.KindExecChanging},
	{cheat: "C12", name: "services changed on the diagnosed job: flagged exec-changing", layer: "static",
		head:     func(t *testing.T) string { return edit(t, unitTestsJob, unitTestsJob+"  services: [redis]\n") },
		wantKind: cidef.KindExecChanging, verdict: VerdictFixed, rule: RuleJ5Fixed, def: cidef.KindExecChanging},
	{cheat: "C12", name: "a dry-run variable on the diagnosed job: flagged exec-changing", layer: "static",
		head: func(t *testing.T) string {
			return edit(t, unitTestsJob, unitTestsJob+"  variables:\n    MAKEFLAGS: --dry-run\n")
		},
		wantKind: cidef.KindExecChanging, verdict: VerdictFixed, rule: RuleJ5Fixed, def: cidef.KindExecChanging},
	{cheat: "C12", name: "a GLOBAL dry-run variable reaches jobs that were not diagnosed", layer: "static",
		head: func(t *testing.T) string {
			return edit(t, "stages: [build, test, package]\n", "stages: [build, test, package]\nvariables:\n  MAKEFLAGS: --dry-run\n")
		},
		wantKind: cidef.KindViolation, verdict: VerdictForged, rule: RuleJ2Violation},
	// C13 make the job fail differently
	{cheat: "C13", name: "the job fails with another message: CHANGED, never PASSED", layer: "runtime",
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineNew))
			redPipeline(f)
		},
		verdict: VerdictFixedNewFailure, rule: RuleJ3Changed},
	{cheat: "C13", name: "the job is broken earlier, so only the runner's summary is left: RECURS", layer: "runtime",
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineGeneric))
			redPipeline(f)
		},
		verdict: VerdictNotFixed, rule: RuleJ3Recurs},
	{cheat: "C13", name: "a lowered timeout makes it fail as a timeout: CHANGED, never PASSED", layer: "runtime",
		mut: func(f *JudgeFacts) {
			a := failedWith(9002, "unit-tests", "ERROR: Job failed: execution took longer than 1m0s seconds")
			a.FailureReason = "job_execution_timeout"
			setJob(f, "unit-tests", a)
			redPipeline(f)
		},
		verdict: VerdictFixedNewFailure, rule: RuleJ3Changed},
	{cheat: "C13", name: "the diagnosed failure was generic to begin with: any failure RECURS", layer: "runtime",
		mut: func(f *JudgeFacts) {
			f.Fix.Targets[0] = target("unit-tests", lineGeneric)
			setJob(f, "unit-tests", failedWith(9002, "unit-tests", lineNew))
			redPipeline(f)
		},
		verdict: VerdictNotFixed, rule: RuleJ3Recurs},
	// C17 "fix" a failure that was never deterministic
	{cheat: "C17", name: "the attempts disagree: FLAKY", layer: "runtime",
		mut: func(f *JudgeFacts) {
			setJob(f, "unit-tests", ok(9002, "unit-tests", "test"), failedWith(9040, "unit-tests", lineOld))
			redPipeline(f)
		},
		verdict: VerdictFlaky, rule: RuleJ3FlakySame},
}

// cheatsNotJudges: the rows of 6.4 with no fixture here, and why. Nothing in
// this package claims to catch them.
var cheatsNotJudges = map[string]string{
	"C7":  "static, part EFFECTS / SCOPE: `test-config` and `other` are in no envelope (SC-CLASS)",
	"C8":  "static, part EFFECTS / SCOPE: `test` is outside the code envelope; SC-WEAKEN",
	"C9":  "static, part SCOPE: the floor is not liftable",
	"C10": "static, part EFFECTS: the cited-files bound; SC-SIZE",
	"C14": "NOT decidable (U3): special-casing the test in product code",
	"C15": "NOT decidable in v1 (U4): a weakened assertion",
	"C16": "NOT decidable in v1 (U5): the job passes having run nothing",
}

func TestCheats(t *testing.T) {
	for _, c := range cheatCases {
		t.Run(c.cheat+" "+c.layer+": "+c.name, func(t *testing.T) {
			f := goodFacts()
			switch c.layer {
			case "static":
				d := cidef.Diff([]byte(ciBase), []byte(c.head(t)), []string{"unit-tests"})
				if d.Kind != c.wantKind {
					t.Fatalf("fixture: cidef makes this %q (violations %+v, unresolvable %+v), the fixture claims %q",
						d.Kind, d.Violations, d.Unresolvable, c.wantKind)
				}
				f.Definition.Delta = d
				if !c.unlifted {
					withLift(&f)
				}
				if c.execDeny {
					f.Fix.CIExecChanges = CIExecDeny
				}
			case "runtime":
				c.mut(&f)
			default:
				t.Fatalf("fixture: layer %q", c.layer)
			}
			now := tEarly
			if c.late {
				now = tLate
			}
			r := Judge(f, now)
			if r.Decision != DecisionVerdict || r.Verdict != c.verdict || r.Rule != c.rule {
				t.Fatalf("decision %q verdict %q by %q (%s); want %q by %q",
					r.Decision, r.Verdict, r.Rule, r.Detail, c.verdict, c.rule)
			}
			if r.Definition != c.def && c.def != "" {
				t.Errorf("definition = %q, want %q", r.Definition, c.def)
			}
			// The flagged-only rows (C6, C11, C12 on the diagnosed job) are
			// the ones the plan says are NOT decidable (U2): the verdict
			// stands, and the flag is the fact that keeps them from anything
			// automatic.
			if c.def == cidef.KindExecChanging && r.Definition != cidef.KindExecChanging {
				t.Errorf("an execution-changing fix is not flagged as one")
			}
		})
	}
}

// C17's other half: a `flaky` diagnosis never dispatches (E4).
func TestC17AFlakyDiagnosisIsNeverDispatched(t *testing.T) {
	f := eligibleFacts()
	f.Run.Class = string(effects.ClassFlaky)
	f.Config.Classes = append(f.Config.Classes, string(effects.ClassFlaky))
	if ok, why := Eligible(f); ok || why.Code != E4NeverEligible {
		t.Fatalf("eligible=%v by %q, want refused by %s", ok, why.Code, E4NeverEligible)
	}
}

// The untouched definition is cidef's `none` -- so the static fixtures above
// differ from the baseline in the definition only.
func TestTheBaseDefinitionComparesAsNone(t *testing.T) {
	d := cidef.Diff([]byte(ciBase), []byte(ciBase), []string{"unit-tests"})
	if d.Kind != cidef.KindNone {
		t.Fatalf("Kind = %q (%+v), want none", d.Kind, d)
	}
}

func TestEveryPlanCheatIsAccountedFor(t *testing.T) {
	want := planRows(t, "## 3. The cheats, and what catches each (C rows)", "C")
	if len(want) != 17 {
		t.Fatalf("the rules page lists %d cheats (%v), expected 17: its section 3 changed, re-read it", len(want), want)
	}
	layers := map[string]map[string]bool{}
	for _, c := range cheatCases {
		if layers[c.cheat] == nil {
			layers[c.cheat] = map[string]bool{}
		}
		layers[c.cheat][c.layer] = true
	}
	var got []string
	for c := range layers {
		got = append(got, c)
		if _, both := cheatsNotJudges[c]; both {
			t.Errorf("%s has a fixture AND is listed as not JUDGE's", c)
		}
	}
	for c := range cheatsNotJudges {
		got = append(got, c)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Errorf("accounted for: %v\non the page:   %v", got, sorted)
	}
	// The rows the plan catches at BOTH layers have a fixture at both.
	for _, c := range []string{"C1", "C2", "C3", "C4", "C5"} {
		if !layers[c]["static"] || !layers[c]["runtime"] {
			t.Errorf("%s: layers with a fixture %v, want static and runtime", c, layers[c])
		}
	}
}
