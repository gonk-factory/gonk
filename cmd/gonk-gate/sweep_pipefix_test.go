package main

// gonk-2usw part DISPATCH: the eligibility wiring in the sweep. Every test
// here drives sweepPipelineRuns over a diagnosed run on the fake GitLab and
// asserts what the sweep LOGGED, what it WROTE and what it FIRED. Synthetic
// fixtures only (plan section 0).

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

const pfDriftLog = "$ ./run-tests\n" +
	"ImportError: cannot import name 'total' from 'calc.sum'\n" +
	"ERROR: Job failed: exit code 1\n"

// outcome is what one look may have done besides logging.
type pfOutcome struct {
	orders   int    // orders fired
	note     string // the fix note's state ("" = no fix note)
	fixState string // the fix record's PipelineFix.State ("-" = no fix record)
	human    bool   // gonk::needs-human on the landing
}

func (w *pfWorld) outcome() pfOutcome {
	w.t.Helper()
	o := pfOutcome{orders: len(w.orders.orders()), fixState: "-", human: hasString(w.landingLabels(), labelNeedsHuman)}
	if st, _, ok := w.fixNote(); ok {
		o.note = st
	}
	if rec, ok := w.fixRecord(); ok {
		o.fixState = rec.PipelineFix.State
	}
	return o
}

// ONE TEST PER ROW OF PLAN 3.2 (and per reason code the sweep can reach): the
// fixture is eligible, `breakIt` breaks exactly one thing, and the look must
// log that row's reason code, fire no order, and write exactly what the row's
// "On failure" column says -- nothing for a silent or unknown row, the note
// for a row marked N.
func TestEachEligibilityRowIsLoggedByItsReasonCode(t *testing.T) {
	silent := pfOutcome{fixState: "-"}
	noted := pfOutcome{note: pipelinectx.FixNoteNotEligible, fixState: "-"}
	needsScope := pfOutcome{note: "needs-scope", fixState: "needs-scope", human: true}
	for _, tc := range []struct {
		name    string
		opts    plOpts
		breakIt func(w *pfWorld)
		want    pipefix.ReasonCode
		out     pfOutcome
		// inNote must appear in the fix note when the row writes one.
		inNote string
		// again is the reason the SECOND look logs when it differs from the
		// first ("" = the same).
		again pipefix.ReasonCode
	}{
		// ---- E1: opted in ----
		{name: "E1 the configuration cannot be read", want: pipefix.E1Unread, out: silent,
			breakIt: func(w *pfWorld) { w.cfgErr = errors.New("meter unreachable") }},
		{name: "E1 pipeline_fixes is off", want: pipefix.E1FixesOff, out: silent,
			breakIt: func(w *pfWorld) { w.cfg.Effective.Actions.PipelineFixes = false }},
		{name: "E1 pipelines is off", want: pipefix.E1PipelinesOff, out: silent,
			breakIt: func(w *pfWorld) { w.cfg.Effective.Actions.Pipelines = false }},
		{name: "E1 the project's config does not load: opted in to nothing", want: pipefix.E1FixesOff, out: silent,
			breakIt: func(w *pfWorld) { w.cfg.Effective, w.cfg.State = nil, meterapi.StateInvalid }},

		// ---- E2: the target kind ----
		{name: "E2 an MR target while only ref is enabled (the default)", opts: plOpts{mr: true},
			want: pipefix.E2TargetKind, out: silent,
			breakIt: func(w *pfWorld) { w.cfg.Effective.PipelineFix.Targets = []string{"ref"} }},
		{name: "E2 the landing cannot be read", want: pipefix.E2Unread, out: silent,
			breakIt: func(w *pfWorld) {
				w.r.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/issues", w.r.pid), 500)
			}},

		// ---- E3: a diagnosed, open run ----
		{name: "E3 the run note is not in state diagnosed", want: pipefix.E3NoteState, out: silent,
			breakIt: func(w *pfWorld) {
				n := w.landingNotes()[0]
				denied, _ := pipelinectx.RunMarker(w.r.rec.BeadAnchor, pipelinectx.RunStateDenied)
				diagnosed, _ := pipelinectx.RunMarker(w.r.rec.BeadAnchor, pipelinectx.RunStateDiagnosed)
				if !strings.Contains(n.Body, diagnosed) {
					w.t.Fatalf("the fixture's run note is not diagnosed:\n%s", n.Body)
				}
				if _, err := w.r.gl.Client().UpdateIssueNote(context.Background(), w.r.pid, 1, n.ID,
					strings.Replace(n.Body, diagnosed, denied, 1)); err != nil {
					w.t.Fatal(err)
				}
			}},

		// ---- E4: the class ----
		{name: "E4 a flaky diagnosis is never eligible", want: pipefix.E4NeverEligible, out: noted, inNote: "`flaky`",
			breakIt: func(w *pfWorld) {
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Class = "flaky" })
			}},
		{name: "E4 a flaky diagnosis is not eligible even when the config lists it", want: pipefix.E4NeverEligible, out: noted,
			breakIt: func(w *pfWorld) {
				w.cfg.Effective.PipelineFix.Classes = []string{"code", "test", "flaky", "infrastructure", "unknown"}
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Class = "flaky" })
			}},
		{name: "E4 infrastructure", want: pipefix.E4NeverEligible, out: noted,
			breakIt: func(w *pfWorld) {
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Class = "infrastructure" })
			}},
		{name: "E4 unknown", want: pipefix.E4NeverEligible, out: noted,
			breakIt: func(w *pfWorld) {
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Class = "unknown" })
			}},
		{name: "E4 a class the project did not enable", want: pipefix.E4NotEnabled, out: noted, inNote: "pipeline_fix.classes",
			breakIt: func(w *pfWorld) { w.cfg.Effective.PipelineFix.Classes = []string{"test"} }},
		{name: "E4 configuration is not enabled by default", want: pipefix.E4NotEnabled, out: noted,
			breakIt: func(w *pfWorld) {
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Class = "configuration" })
			}},

		// ---- E5: a job to judge ----
		{name: "E5 the diagnosis names no job", want: pipefix.E5NoTargets, out: noted, inNote: "no failed job",
			breakIt: func(w *pfWorld) {
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.EvidenceJobs = nil })
			}},
		{name: "E5 the diagnosis names a job that is not recorded", want: pipefix.E5Unjudgeable, out: noted,
			breakIt: func(w *pfWorld) {
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.EvidenceJobs = []string{"unit-tests", "no-such-job"} })
			}},

		// ---- E6: the scope envelope ----
		{name: "E6 the cited file is outside the envelope: needs-scope, decided at dispatch", want: pipefix.E6OutsideEnvelope,
			out: needsScope, inNote: "`calc/sum.py`",
			breakIt: func(w *pfWorld) {
				// Without the python profile calc/sum.py is `other`.
				w.r.proj.PutFileOn(plSHA, ".gonk.yml", []byte(strings.Replace(pfGonkYML, "scope:\n  profiles: [python, docs-md]\n", "", 1)))
			}},
		{name: "E6 a code diagnosis citing a test file: needs-scope", want: pipefix.E6OutsideEnvelope, out: needsScope, inNote: "`test`",
			breakIt: func(w *pfWorld) {
				w.r.proj.PutFileOn(plSHA, "tests/test_sum.py", []byte("def test_total():\n    assert True\n"))
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Files = []string{"calc/sum.py", "tests/test_sum.py"} })
			}},
		{name: "E6 the diagnosis cites no file: needs-scope", want: pipefix.E6NoCitedFiles, out: needsScope,
			breakIt: func(w *pfWorld) { w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Files = nil }) }},
		// The paths check runs after every row of the table, so once it has
		// recorded needs-scope the next look stops at E9 on that record.
		{name: "E6 every cited file is outside pipeline_fix.paths: needs-scope", want: reasonOutsidePaths, out: needsScope,
			inNote: "pipeline_fix.paths", again: pipefix.E9FixTerminal,
			breakIt: func(w *pfWorld) { w.cfg.Effective.PipelineFix.Paths = []string{"docs/**"} }},
		{name: "E6 .gonk.yml cannot be read at the pinned commit", want: pipefix.E6Unread, out: silent,
			breakIt: func(w *pfWorld) { w.r.gl.SetBranchHead(w.r.pid, "main", plSHAGreen) }},
		{name: "E6 the meter states no operator scope", want: pipefix.E6Unread, out: silent,
			breakIt: func(w *pfWorld) { w.cfg.OperatorScope = nil }},
		{name: "E6 the pack has no scope row for the fix agent", want: pipefix.E6Unread, out: silent,
			breakIt: func(w *pfWorld) { w.pack = pfPackDir(w.t, "") }},
		{name: "E6 a cited file cannot be read at the pipeline's commit", want: pipefix.E6Unread, out: silent,
			breakIt: func(w *pfWorld) {
				w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Files = []string{"calc/gone.py"} })
			}},

		// ---- E7: the head ----
		{name: "E7 the target's head has moved past the failing pipeline's commit", want: pipefix.E7HeadMoved, out: silent,
			breakIt: func(w *pfWorld) {
				w.r.gl.SetBranchHead(w.r.pid, "main", plSHAOld)
				w.r.proj.PutFileOn(plSHAOld, ".gonk.yml", []byte(pfGonkYML))
			}},
		{name: "E7 the MR's head has moved", opts: plOpts{mr: true}, want: pipefix.E7HeadMoved, out: silent,
			breakIt: func(w *pfWorld) { w.r.gl.SetMRHead(w.r.pid, 5, plSHAOld) }},
		{name: "E7 the MR's source branch is in another project", opts: plOpts{mr: true}, want: pipefix.E7Unread, out: silent,
			breakIt: func(w *pfWorld) {
				mrs := w.r.proj.MRs
				mrs[0].SourceProjectID = w.r.pid + 1000
			}},

		// ---- E8: the cause, now ----
		{name: "E8 the same job now fails differently", want: pipefix.E8Drifted, out: noted, inNote: "stale diagnosis",
			breakIt: func(w *pfWorld) { w.r.gl.SetTrace(w.r.pid, 9002, []byte(pfDriftLog)) }},
		{name: "E8 the job's log cannot be read", want: pipefix.E8Unread, out: silent,
			breakIt: func(w *pfWorld) {
				w.r.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/jobs/9002/trace", w.r.pid), 500)
			}},

		// ---- E9: dedup ----
		{name: "E9 a fix for this run already ended", want: pipefix.E9FixTerminal, out: pfOutcome{fixState: "fixed"},
			breakIt: func(w *pfWorld) {
				w.putFix(func(r *beadstore.Record) { r.State, r.PipelineFix.State = beadstore.StateDone, "fixed" })
			}},
		{name: "E9 a fix for this run is being dispatched", want: pipefix.E9FixInFlight, out: pfOutcome{fixState: ""},
			breakIt: func(w *pfWorld) { w.putFix(func(r *beadstore.Record) { r.State = beadstore.StateRunning }) }},
		{name: "E9 a fix for this run is parked", want: pipefix.E9FixInFlight, out: pfOutcome{fixState: ""},
			breakIt: func(w *pfWorld) { w.putFix(func(r *beadstore.Record) { r.State = beadstore.StateParked }) }},
		{name: "E9 meter denied the fix", want: pipefix.E9FixTerminal, out: pfOutcome{fixState: ""},
			breakIt: func(w *pfWorld) { w.putFix(func(r *beadstore.Record) { r.State = beadstore.StateNeedsHuman }) }},
		{name: "E9 a fix record in a state this build cannot read", want: pipefix.E9Unread, out: pfOutcome{fixState: ""},
			breakIt: func(w *pfWorld) { w.putFix(func(r *beadstore.Record) { r.State = beadstore.StateDone }) }},
		{name: "E9 a fix MR for this cause was closed unmerged", want: pipefix.E9ClosedUnmerged, out: noted, inNote: "closed without being merged",
			breakIt: func(w *pfWorld) {
				m, _ := pipelinectx.FixMRMarker("gonk:9:pipefix:mr1:00112233aabbccdd:s0", w.causeKey())
				w.fixMR(40, "closed", "gonk/pipefix-aaaaaaaaaaaa", "main", "an earlier run\n\n"+m, nil)
			}},
		{name: "E9 this fix's own branch has a closed-unmerged MR, whatever its description says", want: pipefix.E9ClosedUnmerged, out: noted,
			breakIt: func(w *pfWorld) {
				b, _ := pipelinectx.FixBranch(w.fixAnchor())
				w.fixMR(41, "closed", b, "main", "a human rewrote this description", nil)
			}},
		{name: "E9 a fix MR for this cause is open", want: pipefix.E9OpenCauseMR, out: silent,
			breakIt: func(w *pfWorld) {
				m, _ := pipelinectx.FixMRMarker("gonk:9:pipefix:mr1:00112233aabbccdd:s0", w.causeKey())
				w.fixMR(42, "opened", "gonk/pipefix-bbbbbbbbbbbb", "release", m, nil)
			}},
		{name: "E9 a fix MR whose cause cannot be read", want: pipefix.E9Unread, out: silent,
			breakIt: func(w *pfWorld) { w.fixMR(43, "closed", "gonk/pipefix-cccccccccccc", "main", "no marker", nil) }},

		{name: "E9 the fix record cannot be read", want: pipefix.E9Unread, out: silent,
			breakIt: func(w *pfWorld) { w.store = &failFixGets{Store: w.store} }},

		// ---- E10: serial ----
		{name: "E10 another fix MR is open on this target", want: pipefix.E10OpenFixMR, out: silent,
			breakIt: func(w *pfWorld) {
				m, _ := pipelinectx.FixMRMarker("gonk:9:pipefix:mr1:00112233aabbccdd:s0", "ffffffffffffffff")
				w.fixMR(44, "opened", "gonk/pipefix-dddddddddddd", "main", m, nil)
			}},

		{name: "E10 a merged fix MR on this target carries no merged_at", want: pipefix.E10Unread, out: silent,
			breakIt: func(w *pfWorld) { w.fixMR(47, "merged", "gonk/pipefix-ffffffffffff", "main", "", nil) }},
		{name: "E10 the last green pipeline cannot be read", want: pipefix.E10Unread, out: silent,
			breakIt: func(w *pfWorld) {
				at := rtPipelineCreated(880).Add(time.Hour)
				w.fixMR(48, "merged", "gonk/pipefix-ffffffffffff", "main", "", &at)
				w.r.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/pipelines/880", w.r.pid), 500)
			}},

		// ---- E11: chain ----
		{name: "E11 max_chain fix MRs already merged since the last green pipeline", want: pipefix.E11ChainExhausted,
			out: pfOutcome{note: pipelinectx.FixNoteNotEligible, fixState: "-", human: true}, inNote: "pipeline_fix.max_chain",
			breakIt: func(w *pfWorld) {
				w.cfg.Effective.PipelineFix.MaxChain = 1
				at := rtPipelineCreated(880).Add(time.Hour)
				w.fixMR(45, "merged", "gonk/pipefix-eeeeeeeeeeee", "main", "", &at)
			}},

		// ---- E12: correlation hold ----
		{name: "E12 the run is held by a roll-up", want: pipefix.E12CorrelationHold, out: noted, inNote: "outside this project",
			breakIt: func(w *pfWorld) { w.hold = func(context.Context, beadstore.Record) bool { return true } }},

		// ---- Q2: a configuration fix waits for a person ----
		{name: "Q2 a configuration diagnosis nobody confirmed", want: reasonClassUnconfirmed, out: silent,
			breakIt: func(w *pfWorld) { w.configurationFix() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPFWorld(t, tc.opts)
			tc.breakIt(w)
			writesBefore := len(writes(w.r.gl))
			log := w.look()
			requireReason(t, log, tc.want)
			if got := w.outcome(); got != tc.out {
				t.Fatalf("outcome %+v, want %+v\n%s", got, tc.out, log)
			}
			if tc.out.note == "" {
				if n := len(writes(w.r.gl)) - writesBefore; n != 0 {
					t.Fatalf("a silent row wrote to GitLab %d time(s): %v", n, writes(w.r.gl)[writesBefore:])
				}
			}
			if tc.inNote != "" {
				if _, body, _ := w.fixNote(); !strings.Contains(body, tc.inNote) {
					t.Fatalf("the fix note does not say %q:\n%s", tc.inNote, body)
				}
			}

			// THE SAME LOOK AGAIN writes nothing more: a refusal is noted once.
			writesBefore = len(writes(w.r.gl))
			again := tc.want
			if tc.again != "" {
				again = tc.again
			}
			requireReason(t, w.look(), again)
			if n := len(writes(w.r.gl)) - writesBefore; n != 0 {
				t.Fatalf("the second look wrote to GitLab again: %v", writes(w.r.gl)[writesBefore:])
			}
			if got := w.outcome(); got != tc.out {
				t.Fatalf("after the second look: outcome %+v, want %+v", got, tc.out)
			}
		})
	}
}

// failFixGets is a store whose reads of a FIX record fail.
type failFixGets struct{ beadstore.Store }

func (s *failFixGets) Get(ctx context.Context, anchor string) (beadstore.Record, bool, error) {
	if pipelinectx.IsFixAnchor(anchor) {
		return beadstore.Record{}, false, errors.New("store unreachable")
	}
	return s.Store.Get(ctx, anchor)
}

// putFix stores a fix record for the fixture's run, as an earlier look and its
// dispatch would have left one.
func (w *pfWorld) putFix(mutate func(*beadstore.Record)) {
	w.t.Helper()
	rec := beadstore.Record{BeadAnchor: w.fixAnchor(), BeadID: w.fixAnchor(), Project: "example/calc", ProjectID: w.r.pid,
		Trigger: atags.TriggerPipelineFix, PipelineID: 900, PipelineTarget: w.r.target.String(),
		PipelineFix: beadstore.PipelineFix{RunAnchor: w.r.rec.BeadAnchor, Class: "code", BasePipelineID: 900, BaseSHA: plSHA}}
	mutate(&rec)
	if err := w.store.Put(context.Background(), rec); err != nil {
		w.t.Fatal(err)
	}
}

// configurationFix turns the fixture into a `configuration` diagnosis citing
// the pipeline definition, on a project that enabled the class and the lift.
func (w *pfWorld) configurationFix() {
	w.cfg.Effective.PipelineFix.Classes = []string{"code", "test", "configuration"}
	w.cfg.Effective.PipelineFix.AllowCI = true
	w.r.proj.PutFileOn(plSHA, ".gitlab-ci.yml", []byte("unit-tests:\n  script:\n  - ./run-tests\n"))
	w.setRun(func(r *beadstore.Record) {
		r.PipelineDiagnosis.Class = "configuration"
		r.PipelineDiagnosis.Files = []string{".gitlab-ci.yml"}
	})
}

// THE ELIGIBLE RUN: exactly one order, through gonk-dispatch, carrying the fix
// identity -- and a fix record holding every decision the controller made,
// which is where dispatch reads them from.
func TestAnEligibleDiagnosisFiresOneFixOrder(t *testing.T) {
	for _, mr := range []bool{false, true} {
		name := "default-branch"
		if mr {
			name = "merge-request"
		}
		t.Run(name, func(t *testing.T) {
			w := newPFWorld(t, plOpts{mr: mr})
			writesBefore := len(writes(w.r.gl))
			log := w.look()
			if !strings.Contains(log, "eligible=true") || !strings.Contains(log, "pipeline fix dispatched") {
				t.Fatalf("the look did not dispatch:\n%s", log)
			}
			if n := len(writes(w.r.gl)) - writesBefore; n != 0 {
				t.Fatalf("the sweep wrote to GitLab while dispatching (the proposing note is dispatch's): %v", writes(w.r.gl)[writesBefore:])
			}
			fired := w.orders.orders()
			if len(fired) != 1 || fired[0].Order != dispatchOrderName {
				t.Fatalf("orders = %+v, want exactly one %s", fired, dispatchOrderName)
			}
			run := w.r.rec
			key, _ := pipelinectx.FixSessionKey(w.fixAnchor())
			rec, ok := w.fixRecord()
			if !ok {
				t.Fatal("no fix record")
			}
			wantVars := map[string]string{
				"trigger": "pipeline-fix", "bead_anchor": w.fixAnchor(), "bead_id": w.fixAnchor(), "session_key": key,
				"project": "example/calc", "project_id": fmt.Sprint(w.r.pid), "rig": "example-calc", "issue_iid": "0",
				"config_hash": "sha256:pf", "pipeline_id": "900", "pipeline_target": run.PipelineTarget,
				"pipeline_fingerprint": run.PipelineFingerprint, "pipeline_last_success": "880",
				fireByVar: rec.ReslingFireBy.UTC().Format(time.RFC3339Nano),
			}
			if !reflect.DeepEqual(fired[0].Vars, wantVars) {
				t.Fatalf("order vars:\n got %v\nwant %v", fired[0].Vars, wantVars)
			}
			// The order's vars are an identity dispatch re-derives.
			if err := checkPipelineFixIdentity(dispatchArgsFrom(func(k string) string { return fired[0].Vars[k] })); err != nil {
				t.Fatalf("the fired order does not carry its own identity: %v", err)
			}

			branch, cfgSHA := "main", plSHA
			if mr {
				branch, cfgSHA = "feature/sum", plSHAOld
			}
			want := beadstore.PipelineFix{
				RunAnchor: run.BeadAnchor, CauseKey: w.causeKey(), Class: "code",
				BaseSHA: plSHA, BasePipelineID: 900, TargetBranch: branch, ConfigSHA: cfgSHA,
				Envelope: []string{"docs", "source"}, Files: []string{"calc/sum.py"}, Paths: []string{},
				MaxFiles: 3, MaxLines: 40, MaxFileBytes: 262144, MaxPatchBytes: 16384,
				CIExecChanges: "manual-run", MaxAttempts: 2, VerifyTimeoutSeconds: 7200,
				Targets: []beadstore.PipelineFixTarget{{Stage: "test", Name: "unit-tests", FailureReason: "script_failure",
					ErrorSignature: run.PipelineJobs[0].ErrorSignature}},
				FiredAt:        w.now,
				BaseJobs:       []beadstore.PipelineFixBaseJob{{Name: "build", Status: "success"}, {Name: "unit-tests", Status: "failed"}},
				ExcerptVersion: pipelinectx.ExcerptVersion, SignatureVersion: pipelinectx.SignatureVersion,
			}
			if run.PipelineJobs[0].ErrorSignature == "" {
				t.Fatal("the fixture's job has no recorded signature; the target comparison would be vacuous")
			}
			if !reflect.DeepEqual(rec.PipelineFix, want) {
				t.Fatalf("fix record:\n got %+v\nwant %+v", rec.PipelineFix, want)
			}
			if rec.State != "" || rec.Trigger != atags.TriggerPipelineFix || rec.PipelineRunOpen || rec.SessionID != "" ||
				rec.PipelineID != 900 || rec.PipelineSHA != plSHA || rec.SessionKey != key {
				t.Fatalf("fix record envelope: %+v", rec)
			}
			if !rec.ReslingFireBy.Equal(reslingFireBy(w.now)) {
				t.Fatalf("fire_by %v, want %v", rec.ReslingFireBy, reslingFireBy(w.now))
			}
			// A fix record is never a run: maintenance must not be handed it.
			open, err := w.store.ListOpenPipelineRuns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range open {
				if o.BeadAnchor == w.fixAnchor() {
					t.Fatal("the fix record is listed as an open pipeline run")
				}
			}
		})
	}
}

// PLAN, must be seen red: "two looks fire two orders". The second look finds
// the fix record the first one wrote and stops at E9; a third, and a tenth,
// likewise -- for as long as a dispatch the order fired could still be alive.
func TestTwoLooksFireOneOrder(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	w.look()
	if n := len(w.orders.orders()); n != 1 {
		t.Fatalf("the first look fired %d order(s), want 1", n)
	}
	first, _ := w.fixRecord()
	// Inside the fire window: move the clock by less than the spacing allows
	// a look, then force one, twice.
	for i := 0; i < 2; i++ {
		w.setRun(func(r *beadstore.Record) { r.PipelineCheckedAt = time.Time{} })
		at := w.log.Len()
		w.now = w.now.Add(time.Minute)
		sweepPipelineRuns(context.Background(), w.deps())
		requireReason(t, w.log.String()[at:], pipefix.E9FixInFlight)
	}
	if n := len(w.orders.orders()); n != 1 {
		t.Fatalf("%d orders after three looks, want exactly 1", n)
	}
	again, _ := w.fixRecord()
	if again.Revision != first.Revision {
		t.Fatal("a later look rewrote the fix record")
	}
}

// casStore lets a test put another writer's record in between a look's read
// of the fix record and its write.
type casStore struct {
	beadstore.Store
	beforePutIf func(r beadstore.Record)
}

func (s *casStore) PutIf(ctx context.Context, r beadstore.Record, rev string) (string, error) {
	if s.beforePutIf != nil && pipelinectx.IsFixAnchor(r.BeadAnchor) {
		hook := s.beforePutIf
		s.beforePutIf = nil
		hook(r)
	}
	return s.Store.PutIf(ctx, r, rev)
}

// TWO SWEEPS, ONE ORDER: both read "no fix record", both decide to dispatch;
// the one whose record write loses fires nothing.
func TestTwoSweepsRacingFireOneOrder(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	inner := w.store
	raced := false
	w.store = &casStore{Store: inner, beforePutIf: func(r beadstore.Record) {
		if r.BeadAnchor != w.fixAnchor() {
			return
		}
		// The other sweep's write lands first.
		other := r
		if _, err := inner.PutIf(context.Background(), other, ""); err != nil {
			t.Fatalf("the competing write: %v", err)
		}
		raced = true
	}}
	log := w.look()
	if !raced {
		t.Fatalf("the look never tried to write the fix record:\n%s", log)
	}
	if !strings.Contains(log, "another look recorded this fix first") {
		t.Fatalf("the losing look did not say so:\n%s", log)
	}
	if n := len(w.orders.orders()); n != 0 {
		t.Fatalf("the look that lost the record write fired %d order(s)", n)
	}
}

// AN ORDER GAS CITY DROPPED IS FIRED AGAIN -- but only once no dispatch the
// first one fired can still be alive (the re-sling window, enforced by
// fire_by), and over exactly the record the look read.
func TestAFixOrderThatNeverArrivedIsFiredAgainAfterItsWindow(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	w.look()
	first, _ := w.fixRecord()
	end, ok := reslingWindowEnd(first)
	if !ok {
		t.Fatal("the fix record carries no fire window")
	}
	// Just inside the window: still in flight.
	w.setRun(func(r *beadstore.Record) { r.PipelineCheckedAt = time.Time{} })
	w.now = end.Add(-time.Second)
	at := w.log.Len()
	sweepPipelineRuns(context.Background(), w.deps())
	requireReason(t, w.log.String()[at:], pipefix.E9FixInFlight)
	if n := len(w.orders.orders()); n != 1 {
		t.Fatalf("%d orders inside the window, want 1", n)
	}
	// Past it, with the record still where the sweep left it: fire again.
	w.setRun(func(r *beadstore.Record) { r.PipelineCheckedAt = time.Time{} })
	w.now = end.Add(time.Second)
	sweepPipelineRuns(context.Background(), w.deps())
	fired := w.orders.orders()
	if len(fired) != 2 {
		t.Fatalf("%d orders after the window ended, want 2", len(fired))
	}
	second, _ := w.fixRecord()
	if second.Revision == first.Revision || !second.PipelineFix.FiredAt.Equal(w.now) {
		t.Fatalf("the re-fire did not rewrite the record: %+v", second.PipelineFix)
	}
	if fired[1].Vars[fireByVar] == fired[0].Vars[fireByVar] {
		t.Fatal("the second order carries the first one's fire_by deadline")
	}
}

// A refused order (Gas City said no) leaves the record, and the window then
// decides -- never an immediate second order.
func TestARefusedFixOrderIsNotRetriedInsideItsWindow(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	w.orders.fail = 1
	log := w.look()
	if !strings.Contains(log, "order_accepted=false") {
		t.Fatalf("the refused order was not reported:\n%s", log)
	}
	w.setRun(func(r *beadstore.Record) { r.PipelineCheckedAt = time.Time{} })
	w.now = w.now.Add(time.Minute)
	sweepPipelineRuns(context.Background(), w.deps())
	if n := len(w.orders.orders()); n != 0 {
		t.Fatalf("%d order(s) accepted inside the window of a refused one, want 0", n)
	}
}

// PLAN, must be seen red: "a closed-unmerged MR is re-proposed". A human who
// closed a fix said no: no order, on this look or any later one, and the
// landing says why -- once.
func TestAClosedUnmergedFixIsNotReproposed(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	m, err := pipelinectx.FixMRMarker(w.fixAnchor(), w.causeKey())
	if err != nil {
		t.Fatal(err)
	}
	branch, _ := pipelinectx.FixBranch(w.fixAnchor())
	w.fixMR(40, "closed", branch, "main", "gonk's earlier fix\n\n"+m, nil)
	for i := 0; i < 3; i++ {
		requireReason(t, w.look(), pipefix.E9ClosedUnmerged)
	}
	if n := len(w.orders.orders()); n != 0 {
		t.Fatalf("a fix a human closed was re-proposed: %d order(s)", n)
	}
	if _, ok := w.fixRecord(); ok {
		t.Fatal("a fix record was written for a fix that is not attempted")
	}
	st, body, ok := w.fixNote()
	if !ok || st != pipelinectx.FixNoteNotEligible || !strings.Contains(body, "closed without being merged") {
		t.Fatalf("fix note: %q ok=%v\n%s", st, ok, body)
	}
	// A closed-unmerged fix for ANOTHER cause on another branch blocks nothing.
	w2 := newPFWorld(t, plOpts{})
	other, _ := pipelinectx.FixMRMarker("gonk:9:pipefix:mr1:00112233aabbccdd:s0", "ffffffffffffffff")
	w2.fixMR(40, "closed", "gonk/pipefix-aaaaaaaaaaaa", "main", other, nil)
	if log := w2.look(); !strings.Contains(log, "eligible=true") || len(w2.orders.orders()) != 1 {
		t.Fatalf("a closed fix for another cause blocked this one:\n%s", log)
	}
}

// PLAN, must be seen red: "a `flaky` diagnosis dispatches". Whatever the
// project's configuration says.
func TestAFlakyDiagnosisNeverDispatches(t *testing.T) {
	for _, class := range []string{"flaky", "infrastructure", "unknown"} {
		t.Run(class, func(t *testing.T) {
			w := newPFWorld(t, plOpts{})
			w.cfg.Effective.PipelineFix.Classes = []string{"code", "test", "configuration", class}
			w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Class = class })
			for i := 0; i < 2; i++ {
				requireReason(t, w.look(), pipefix.E4NeverEligible)
			}
			if n := len(w.orders.orders()); n != 0 {
				t.Fatalf("a %s diagnosis fired %d fix order(s)", class, n)
			}
			if _, ok := w.fixRecord(); ok {
				t.Fatalf("a %s diagnosis got a fix record", class)
			}
			st, body, ok := w.fixNote()
			if !ok || st != pipelinectx.FixNoteNotEligible || !strings.Contains(body, "`"+class+"`") {
				t.Fatalf("the landing does not say why: %q ok=%v\n%s", st, ok, body)
			}
		})
	}
}

// SERIAL (E10): one open fix MR per target. The same open MR on ANOTHER
// branch does not block this target.
func TestOneOpenFixMRPerTarget(t *testing.T) {
	other, _ := pipelinectx.FixMRMarker("gonk:9:pipefix:mr1:00112233aabbccdd:s0", "ffffffffffffffff")
	w := newPFWorld(t, plOpts{})
	w.fixMR(44, "opened", "gonk/pipefix-dddddddddddd", "release", other, nil)
	if log := w.look(); len(w.orders.orders()) != 1 {
		t.Fatalf("an open fix MR on another branch blocked this target:\n%s", log)
	}
	// A HUMAN's MR from a branch named like a fix branch is not a fix MR.
	w = newPFWorld(t, plOpts{})
	w.r.gl.AddMR(w.r.pid, glabMR(46, "opened", "gonk/pipefix-dddddddddddd", "main", rtHumanID))
	if log := w.look(); len(w.orders.orders()) != 1 {
		t.Fatalf("a human's merge request was counted as an open fix MR:\n%s", log)
	}
}

// WHAT COUNTS AS A FIX MR is re-checked on every object GitLab returns -- the
// server-side filters only bound the listing. With a server that ignores them,
// none of these is an open fix MR on the target: a human's MR from a branch
// named like a fix branch, the bot's MR from another kind of branch, and a
// fix-branch MR from another project (a fork).
func TestWhatCountsAsAFixMRIsRecheckedClientSide(t *testing.T) {
	for _, tc := range []struct {
		name string
		mr   func(pid int64) glab.MergeRequest
	}{
		{"a human's MR from a fix-shaped branch", func(int64) glab.MergeRequest {
			return glabMR(46, "opened", "gonk/pipefix-dddddddddddd", "main", rtHumanID)
		}},
		{"the bot's MR from another kind of branch", func(int64) glab.MergeRequest {
			return glabMR(46, "opened", "gonk/scaffold", "main", plBotID)
		}},
		{"a fix-branch MR whose source is another project", func(pid int64) glab.MergeRequest {
			m := glabMR(46, "opened", "gonk/pipefix-dddddddddddd", "main", plBotID)
			m.SourceProjectID = pid + 1000
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPFWorld(t, plOpts{})
			w.r.gl.IgnoreMRListFilters = true
			w.r.gl.AddMR(w.r.pid, tc.mr(w.r.pid))
			if log := w.look(); len(w.orders.orders()) != 1 {
				t.Fatalf("it was counted as a fix MR and blocked the dispatch:\n%s", log)
			}
		})
	}
}

// ONLY A DIAGNOSED RUN IS LOOKED AT, and before anything is read: a run with
// no landed diagnosis, or one that is not done, costs the fix pass nothing --
// not even the read of its project's configuration.
func TestOnlyADiagnosedRunIsLookedAt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(r *beadstore.Record)
	}{
		{"no diagnosis on the record", func(r *beadstore.Record) { r.PipelineDiagnosis = beadstore.PipelineDiagnosis{} }},
		{"a denied run (needs-human)", func(r *beadstore.Record) { r.State = beadstore.StateNeedsHuman }},
		{"a parked run", func(r *beadstore.Record) { r.State = beadstore.StateParked; r.RetryAfter = time.Now().Add(time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPFWorld(t, plOpts{})
			w.setRun(tc.mutate)
			log := w.look()
			if strings.Contains(log, "pipeline fix eligibility") || w.cfgCalls != 0 || len(w.orders.orders()) != 0 {
				t.Fatalf("the run was looked at (config reads %d, orders %d):\n%s", w.cfgCalls, len(w.orders.orders()), log)
			}
		})
	}
	// The control: the untouched fixture IS looked at.
	w := newPFWorld(t, plOpts{})
	if log := w.look(); !strings.Contains(log, "pipeline fix eligibility") || w.cfgCalls != 1 {
		t.Fatalf("the diagnosed fixture was not looked at (config reads %d):\n%s", w.cfgCalls, log)
	}
}

// ONE configuration read per project per tick, however many of its runs are
// looked at.
func TestTheConfigurationIsReadOncePerProjectPerTick(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	w.cfg.Effective.Actions.PipelineFixes = false
	// A second diagnosed run of the same project (another failure set).
	second := w.r.rec
	anchor, err := pipelinectx.Anchor(w.r.pid, w.r.target, "ffffffffffffffff", 880, 0)
	if err != nil {
		t.Fatal(err)
	}
	second.BeadAnchor, second.BeadID, second.PipelineFingerprint = anchor, anchor, "ffffffffffffffff"
	if err := w.store.Put(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	log := w.look()
	if n := strings.Count(log, "pipeline fix eligibility"); n != 2 {
		t.Fatalf("%d looks, want 2 (one per run):\n%s", n, log)
	}
	if w.cfgCalls != 1 {
		t.Fatalf("the project's configuration was read %d times in one tick, want once", w.cfgCalls)
	}
}

// CHAIN (E11): fix MRs merged on this target SINCE ITS LAST GREEN pipeline
// count; older ones, and ones merged into another branch, do not.
func TestTheChainCountsOnlyFixesMergedOnThisTargetSinceItsLastGreen(t *testing.T) {
	green := rtPipelineCreated(880)
	for _, tc := range []struct {
		name     string
		maxChain int
		merged   []struct {
			at     time.Time
			branch string
		}
		want pipefix.ReasonCode // "" = dispatches
	}{
		{name: "one merged since green, max_chain 2: dispatches", maxChain: 2,
			merged: []struct {
				at     time.Time
				branch string
			}{{green.Add(time.Hour), "main"}}},
		{name: "two merged since green, max_chain 2: exhausted", maxChain: 2, want: pipefix.E11ChainExhausted,
			merged: []struct {
				at     time.Time
				branch string
			}{{green.Add(time.Hour), "main"}, {green.Add(2 * time.Hour), "main"}}},
		{name: "two merged, one BEFORE the last green: dispatches", maxChain: 2,
			merged: []struct {
				at     time.Time
				branch string
			}{{green.Add(-time.Hour), "main"}, {green.Add(time.Hour), "main"}}},
		{name: "two merged, one into another branch: dispatches", maxChain: 2,
			merged: []struct {
				at     time.Time
				branch string
			}{{green.Add(time.Hour), "release"}, {green.Add(time.Hour), "main"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPFWorld(t, plOpts{})
			w.cfg.Effective.PipelineFix.MaxChain = tc.maxChain
			for i, m := range tc.merged {
				at := m.at
				w.fixMR(int64(50+i), "merged", fmt.Sprintf("gonk/pipefix-%012x", i), m.branch, "", &at)
			}
			log := w.look()
			if tc.want == "" {
				if len(w.orders.orders()) != 1 {
					t.Fatalf("no order:\n%s", log)
				}
				return
			}
			requireReason(t, log, tc.want)
			if len(w.orders.orders()) != 0 {
				t.Fatal("an order was fired past the chain cap")
			}
		})
	}
}

// A CONFIGURATION FIX (plan 4.2 row 3, owner decision Q2): dispatched only on
// a project that enabled the class AND the lift, and only after a person
// confirmed the class on the landing. The lift is recorded on the fix; it
// comes from the project's allow_ci.
func TestAConfigurationFixNeedsTheLiftAndAConfirmedClass(t *testing.T) {
	t.Run("confirmed, allow_ci on: dispatched with the ci lift", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.configurationFix()
		w.humanLabel(labelFixClassConfirmed)
		log := w.look()
		rec, ok := w.fixRecord()
		if !ok || len(w.orders.orders()) != 1 {
			t.Fatalf("not dispatched:\n%s", log)
		}
		if !reflect.DeepEqual(rec.PipelineFix.Envelope, []string{"ci"}) || !reflect.DeepEqual(rec.PipelineFix.Lift, []string{"ci"}) ||
			rec.PipelineFix.Class != "configuration" {
			t.Fatalf("envelope %v lift %v class %q, want [ci] [ci] configuration",
				rec.PipelineFix.Envelope, rec.PipelineFix.Lift, rec.PipelineFix.Class)
		}
		if !strings.Contains(log, "lift=\"ci (trigger=pipeline-fix workflow=configuration)\"") {
			t.Fatalf("the lift was not logged with its dispatch:\n%s", log)
		}
	})
	t.Run("confirmed, allow_ci OFF: needs-scope, no lift", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.configurationFix()
		w.cfg.Effective.PipelineFix.AllowCI = false
		w.humanLabel(labelFixClassConfirmed)
		requireReason(t, w.look(), pipefix.E6OutsideEnvelope)
		if len(w.orders.orders()) != 0 {
			t.Fatal("a configuration fix was dispatched without allow_ci")
		}
		rec, _ := w.fixRecord()
		if rec.PipelineFix.State != "needs-scope" || len(rec.PipelineFix.Envelope) != 0 || len(rec.PipelineFix.Lift) != 0 {
			t.Fatalf("fix record %+v, want needs-scope with an empty envelope and no lift", rec.PipelineFix)
		}
	})
	t.Run("allow_ci on, the operator vetoes the lift: needs-scope", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.configurationFix()
		w.cfg.OperatorScope.Instance.DenyLift = []string{"ci"}
		w.humanLabel(labelFixClassConfirmed)
		requireReason(t, w.look(), pipefix.E6OutsideEnvelope)
		if len(w.orders.orders()) != 0 {
			t.Fatal("a configuration fix was dispatched over the operator's veto")
		}
	})
	t.Run("a code fix on a project with allow_ci gets no lift", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.cfg.Effective.PipelineFix.AllowCI = true
		w.look()
		rec, ok := w.fixRecord()
		if !ok || len(rec.PipelineFix.Lift) != 0 || !reflect.DeepEqual(rec.PipelineFix.Envelope, []string{"docs", "source"}) {
			t.Fatalf("fix record %+v ok=%v, want envelope [docs source] and no lift", rec.PipelineFix, ok)
		}
	})
}

// The envelope recorded on a fix is the FOLD's: the operator and the project
// narrow it, and their caps reach the record.
func TestTheRecordedEnvelopeIsTheFold(t *testing.T) {
	two, twenty := int64(2), int64(20)
	t.Run("the operator allows only docs: a code fix citing source is needs-scope", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.cfg.OperatorScope.Instance.Classes = []string{"docs"}
		requireReason(t, w.look(), pipefix.E6OutsideEnvelope)
	})
	t.Run("the group's caps and the project's max_changed_lines reach the record", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.cfg.OperatorScope.Group.Caps.MaxFiles = &two
		w.cfg.Effective.PipelineFix.MaxChangedLines = int(twenty)
		w.look()
		rec, ok := w.fixRecord()
		if !ok || rec.PipelineFix.MaxFiles != 2 || rec.PipelineFix.MaxLines != 20 {
			t.Fatalf("caps on the record: files %d lines %d ok=%v, want 2 and 20", rec.PipelineFix.MaxFiles, rec.PipelineFix.MaxLines, ok)
		}
	})
	t.Run("an operator protected path makes the cited file control: needs-scope", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.cfg.OperatorScope.Instance.ProtectedPaths = []string{"calc/**"}
		requireReason(t, w.look(), pipefix.E6OutsideEnvelope)
		_, body, _ := w.fixNote()
		if !strings.Contains(body, "`control`") {
			t.Fatalf("the note does not name the class:\n%s", body)
		}
	})
	t.Run("a generated file is not source", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.r.proj.PutFileOn(plSHA, "calc/sum.py", []byte("// Code generated by a tool. DO NOT EDIT.\n"))
		requireReason(t, w.look(), pipefix.E6OutsideEnvelope)
	})
	t.Run("a file the pipeline definition includes is ci, not whatever its name suggests", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.r.proj.PutFileOn(plSHA, ".gitlab-ci.yml", []byte("include:\n  - local: build/steps.py\n"))
		w.r.proj.PutFileOn(plSHA, "build/steps.py", []byte("x = 1\n"))
		w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Files = []string{"build/steps.py"} })
		requireReason(t, w.look(), pipefix.E6OutsideEnvelope)
		if _, body, _ := w.fixNote(); !strings.Contains(body, "path class `ci`") {
			t.Fatalf("the included file was not classified ci:\n%s", body)
		}
	})
	t.Run("pipeline_fix.paths keeps only the cited files inside it", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.r.proj.PutFileOn(plSHA, "lib/util.py", []byte("x = 1\n"))
		w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Files = []string{"calc/sum.py", "lib/util.py"} })
		w.cfg.Effective.PipelineFix.Paths = []string{"calc/**"}
		w.look()
		rec, ok := w.fixRecord()
		if !ok || !reflect.DeepEqual(rec.PipelineFix.Files, []string{"calc/sum.py"}) || !reflect.DeepEqual(rec.PipelineFix.Paths, []string{"calc/**"}) {
			t.Fatalf("files %v paths %v ok=%v", rec.PipelineFix.Files, rec.PipelineFix.Paths, ok)
		}
	})
}

// THE FACTS ARE THE NEWEST FAILING PIPELINE'S. After the run's same failure
// recurs on a newer pipeline at a newer commit, the fix is decided against
// THAT pipeline and commit.
func TestTheFixIsDecidedAgainstTheNewestFailingPipeline(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	gl, pid := w.r.gl, w.r.pid
	// The head moves to a new commit whose pipeline fails the same way.
	gl.AddPipeline(pid, glabPipeline(901, "main", plSHAOld, "failed"))
	gl.AddJob(pid, 901, glabJob(9011, "build", "build", "success", ""))
	gl.AddJob(pid, 901, glabJob(9012, "unit-tests", "test", "failed", "script_failure"))
	gl.SetTrace(pid, 9012, []byte(plTestLog))
	gl.SetBranchHead(pid, "main", plSHAOld)
	w.r.proj.PutFileOn(plSHAOld, ".gonk.yml", []byte(pfGonkYML))
	w.r.proj.PutFileOn(plSHAOld, "calc/sum.py", []byte("def total(xs):\n    return 0\n"))
	log := w.look()
	rec, ok := w.fixRecord()
	if !ok {
		t.Fatalf("not dispatched:\n%s", log)
	}
	if rec.PipelineFix.BasePipelineID != 901 || rec.PipelineFix.BaseSHA != plSHAOld || rec.PipelineID != 901 {
		t.Fatalf("the fix is against pipeline %d at %s, want 901 at %s", rec.PipelineFix.BasePipelineID, rec.PipelineFix.BaseSHA, plSHAOld)
	}
	if v := w.orders.orders()[0].Vars["pipeline_id"]; v != "901" {
		t.Fatalf("order pipeline_id = %s, want 901", v)
	}
}

// currentFailureKeys must compute what dispatch RECORDS -- the same split,
// order, name and signature -- or E8 would call every run drifted (or none).
func TestCurrentFailureKeysAreWhatDispatchRecords(t *testing.T) {
	// lintJob + bigContext is FIVE failed jobs: one more than gets an excerpt,
	// so the fifth is recorded with no signature and must be recomputed so.
	for _, o := range []plOpts{{}, {lintJob: true}, {lintJob: true, lintName: "lint  check"}, {longLog: true}, {bigContext: true},
		{lintJob: true, bigContext: true}} {
		r := newPLRun(t, o)
		keys, err := currentFailureKeys(context.Background(), r.gl.Client(), r.pid, 900)
		if err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		if len(keys) != len(r.rec.PipelineJobs) || len(keys) == 0 {
			t.Fatalf("%+v: %d keys for %d recorded jobs", o, len(keys), len(r.rec.PipelineJobs))
		}
		signed := 0
		for i, j := range r.rec.PipelineJobs {
			want := pipelinectx.JobFailureKey{Stage: j.Stage, Name: j.Name, FailureReason: j.FailureReason, ErrorSignature: j.ErrorSignature}
			if keys[i] != want {
				t.Errorf("%+v: job %d: recomputed %+v, recorded %+v", o, i, keys[i], want)
			}
			if j.ErrorSignature != "" {
				signed++
			}
		}
		if signed == 0 {
			t.Fatalf("%+v: no recorded job has a signature; the comparison would be vacuous", o)
		}
		if o.lintJob && o.bigContext && signed == len(keys) {
			t.Fatalf("%+v: every one of %d jobs has a signature; the past-the-excerpt-slots case is not exercised", o, len(keys))
		}
	}
	// A log that cannot be read is an error, never a key with no signature.
	r := newPLRun(t, plOpts{})
	r.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/jobs/9002/trace", r.pid), 500)
	if keys, err := currentFailureKeys(context.Background(), r.gl.Client(), r.pid, 900); err == nil {
		t.Fatalf("an unreadable log yielded keys %+v", keys)
	}
}

// fixStateOf is the whole record -> fix state mapping E9 stands on.
func TestFixStateOf(t *testing.T) {
	now := time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC)
	fired := func(at time.Time) beadstore.Record {
		return beadstore.Record{ReslungAt: at, ReslingFireBy: reslingFireBy(at)}
	}
	windowEnd, _ := reslingWindowEnd(fired(now))
	for _, tc := range []struct {
		name  string
		rec   beadstore.Record
		at    time.Time
		want  pipefix.State
		stale bool
		known bool
	}{
		{"an explicit fix state wins over the record state", beadstore.Record{State: beadstore.StateRunning,
			PipelineFix: beadstore.PipelineFix{State: "verifying"}}, now, pipefix.StateVerifying, false, true},
		{"an explicit state outside the closed set is unknown", beadstore.Record{PipelineFix: beadstore.PipelineFix{State: "banana"}},
			now, "banana", false, false},
		{"running", beadstore.Record{State: beadstore.StateRunning}, now, pipefix.StateProposing, false, true},
		{"pending-prompt", beadstore.Record{State: beadstore.StatePendingPrompt}, now, pipefix.StateProposing, false, true},
		{"parked", beadstore.Record{State: beadstore.StateParked}, now, pipefix.StateProposing, false, true},
		{"needs-human", beadstore.Record{State: beadstore.StateNeedsHuman}, now, pipefix.StateDenied, false, true},
		{"fired, inside the window", fired(now), windowEnd.Add(-time.Second), pipefix.StateProposing, false, true},
		{"fired, at the window's end", fired(now), windowEnd, "", true, true},
		{"fired, past the window", fired(now), windowEnd.Add(time.Hour), "", true, true},
		{"released (no state, no window)", beadstore.Record{}, now, "", true, true},
		{"done with no fix state", beadstore.Record{State: beadstore.StateDone}, now, "", false, false},
	} {
		st, stale, known := fixStateOf(tc.rec, tc.at)
		if st != tc.want || stale != tc.stale || known != tc.known {
			t.Errorf("%s: (%q, stale=%v, known=%v), want (%q, %v, %v)", tc.name, st, stale, known, tc.want, tc.stale, tc.known)
		}
	}
	// Every state fixStateOf can return is one Eligible reads without "unknown".
	for _, s := range []pipefix.State{pipefix.StateProposing, pipefix.StateDenied} {
		if !pipefix.KnownState(s) {
			t.Errorf("fixStateOf returns %q, which pkg/pipefix does not know", s)
		}
	}
	if pipefix.KnownState(pipefixUnknownState) {
		t.Fatal("pipefixUnknownState is a known state; an unreadable fix record would read as one")
	}
}

// The fix note's states are a superset of pkg/pipefix's: a later part can
// write any fix state through pipelinectx.FixMarker.
func TestFixNoteStatesCoverEveryFixState(t *testing.T) {
	for _, s := range pipefix.States() {
		if !pipelinectx.KnownFixNoteState(string(s)) {
			t.Errorf("fix state %q cannot be written as a fix note state", s)
		}
	}
	if len(pipelinectx.FixNoteStates()) != len(pipefix.States())+1 {
		t.Errorf("FixNoteStates has %d entries, want pipefix.States (%d) plus not-eligible",
			len(pipelinectx.FixNoteStates()), len(pipefix.States()))
	}
	if !pipelinectx.KnownFixNoteState(pipelinectx.FixNoteNotEligible) || pipefix.KnownState(pipefix.State(pipelinectx.FixNoteNotEligible)) {
		t.Error("not-eligible must be a fix NOTE state and must not be a fix state")
	}
	if string(pipefix.StateProposing) != pipelinectx.FixNoteProposing {
		t.Error("the proposing state has two spellings")
	}
}

// INERT WHEN NOT OPTED IN (the proof the plan's row asks for). Three identical
// worlds, each given the same two looks:
//
//	A  the fix pass does not exist (FixesLive false: production until a
//	   preparer is wired in)
//	B  the pass is on, the project has NOT opted in
//	C  the pass is on and the project has opted in (the control: it shows the
//	   comparison can see a read when there is one)
//
// B must make EXACTLY the GitLab requests A makes -- the same requests, in the
// same order -- write nothing, fire nothing and store nothing. Its one cost is
// one read of the project's meter registration per look.
func TestAnOptedOutProjectIsInert(t *testing.T) {
	type result struct {
		requests []string
		cfgCalls int
		orders   int
		records  int
	}
	run := func(arrange func(w *pfWorld)) result {
		w := newPFWorld(t, plOpts{})
		arrange(w)
		before := len(w.r.gl.Requests())
		w.look()
		w.look()
		all, err := w.store.List(context.Background(), beadstore.StateDone)
		if err != nil {
			t.Fatal(err)
		}
		_, hasFix := w.fixRecord()
		n := len(all)
		if hasFix {
			n++
		}
		return result{requests: w.r.gl.Requests()[before:], cfgCalls: w.cfgCalls, orders: len(w.orders.orders()), records: n}
	}
	a := run(func(w *pfWorld) { w.live = false })
	b := run(func(w *pfWorld) { w.cfg.Effective.Actions.PipelineFixes = false })
	c := run(func(w *pfWorld) {})

	if len(a.requests) == 0 {
		t.Fatal("run maintenance made no GitLab request at all; the comparison would be vacuous")
	}
	if !reflect.DeepEqual(b.requests, a.requests) {
		t.Fatalf("an opted-out project sees different GitLab requests with the fix pass on:\n off: %v\n on:  %v", a.requests, b.requests)
	}
	for _, r := range b.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Fatalf("a write for an opted-out project: %s", r)
		}
	}
	if a.cfgCalls != 0 {
		t.Fatalf("with the pass off the project's configuration was read %d time(s)", a.cfgCalls)
	}
	if b.cfgCalls != 2 {
		t.Fatalf("an opted-out project's configuration was read %d time(s) over two looks, want one per look", b.cfgCalls)
	}
	if b.orders != 0 || a.orders != 0 {
		t.Fatalf("orders: off %d, opted out %d; want none", a.orders, b.orders)
	}
	if b.records != a.records {
		t.Fatalf("the store holds %d record(s) for an opted-out project, %d with the pass off", b.records, a.records)
	}
	t.Logf("GitLab requests over two looks: pass off %d, opted out %d, opted in %d", len(a.requests), len(b.requests), len(c.requests))
	// The control.
	if len(c.requests) <= len(a.requests) || c.orders != 1 || c.records != a.records+1 {
		t.Fatalf("the opted-in control made %d requests (off: %d), fired %d order(s), stored %d record(s): "+
			"the comparison above cannot be trusted", len(c.requests), len(a.requests), c.orders, c.records)
	}
}

// With no fix pass there is no pipefixSweep at all, whatever else is set.
func TestTheFixPassIsOffUnlessItIsTurnedOn(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	d := w.deps()
	if newPipefixSweep(d) == nil {
		t.Fatal("setup: the fixture's pass is off")
	}
	d.FixesLive = false
	if newPipefixSweep(d) != nil {
		t.Fatal("a sweep with FixesLive false has a fix pass")
	}
	d.FixesLive, d.Pipelines = true, nil
	if newPipefixSweep(d) != nil {
		t.Fatal("a sweep with no pipeline forge has a fix pass")
	}
	d = w.deps()
	d.FixConfig, d.Meter = nil, nil
	if newPipefixSweep(d) != nil {
		t.Fatal("a sweep with no configuration source has a fix pass")
	}
}
