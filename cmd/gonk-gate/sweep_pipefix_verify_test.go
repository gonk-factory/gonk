package main

// gonk-2usw part VERIFY: the exit check of a pipeline fix (plan 6, 7.1-7.3,
// 8.1) -- sweep_pipefix_verify.go.
//
// Every test starts where production does: a diagnosed run, the sweep's look
// firing the fix order, gonk-dispatch starting the fix session through the
// REAL preparer, the session's answer applied by the REAL sweep (branch,
// commit, merge request). From there the test plays GitLab: it adds the
// merge request's pipeline and its jobs to pkg/glab/glabtest, moves the
// clock, and runs whole sweep ticks. What is asserted is the meter's request
// log (fakeOutcomeMeter), the stored record, and GitLab's WRITE LOG.
//
// SYNTHETIC AND NEUTRAL (plan section 0): the made-up `example/calc`.

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// fvOtherLog is a failing unit-test log whose FIRST ERROR LINE is not the
// diagnosed one (plTestLog's is "test_sum ... FAILED"): the same job failing
// differently (plan 6.3 J3 CHANGED).
const fvOtherLog = "$ ./run-tests\n" +
	"collecting tests\n" +
	"test_addition ... ok\n" +
	"test_empty_list ... FAILED\n" +
	"ValueError: total() was handed an empty list of crates\n" +
	"1 failed, 1 passed\n" +
	"ERROR: Job failed: exit code 1\n"

// fvWorld is a faWorld whose fix was applied: the merge request is open and
// the record is `verifying` (or `awaiting-run`).
type fvWorld struct {
	*faWorld
	om    *fakeOutcomeMeter
	clock time.Time
	log   bytes.Buffer
	next  int64 // the next fake pipeline id
	// realRT and realPack, when set, replace the in-process runtime and the
	// fixture's pack copy in the sweep's deps: the component tests judge
	// sessions that ran as Jobs on a real cluster (jobs_component_*_test.go).
	realRT   runtime.SessionRuntime
	realPack string
}

func newFVWorld(t *testing.T, o faOpts) *fvWorld {
	t.Helper()
	return newFVWorldWith(t, o, faGood())
}

func newFVWorldWith(t *testing.T, o faOpts, batch effects.Batch) *fvWorld {
	t.Helper()
	w := &fvWorld{faWorld: newFAWorld(t, o), next: 2000, clock: time.Now().Add(time.Minute)}
	w.om = &fakeOutcomeMeter{outcomeNext: "done", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 1200, AsOf: time.Now().Add(240 * time.Hour)}}
	// The lint of a commit gonk creates answers from that commit's own file.
	w.r.gl.SetCILintFromFile(w.r.pid, faCI)
	h := runtime.Handle(w.session)
	w.rt.SetOutput(h, []byte(plSessionOutput(t, batch)))
	w.rt.SetPhase(h, runtime.PhaseSucceeded, "")
	w.tick(0)
	rec := w.rec()
	if rec.State != beadstore.StateRunning || !fixOpenStates[rec.PipelineFix.State] || rec.PipelineFix.MRIID == 0 {
		t.Fatalf("setup: the fix was not applied: state %q fix state %q mr %d\n%s", rec.State, rec.PipelineFix.State, rec.PipelineFix.MRIID, w.log.String())
	}
	if len(w.om.requests()) != 0 {
		t.Fatalf("setup: an outcome was reported before anything judged the fix: %+v", w.om.requests())
	}
	w.resetWrites()
	return w
}

func (w *fvWorld) deps() sweepDeps {
	d := w.faWorld.deps()
	d.Meter, d.Runtime, d.GL, d.BotUsername = meterClient(w.om.server(w.t)), w.rt, w.r.gl.Client(), "gonk"
	d.Log = testLogger(&w.log)
	d.Now = func() time.Time { return w.clock }
	d.SpendPollInterval, d.SpendDeadline = time.Millisecond, time.Second
	d.FixesLive = false // the fix LOOK is not under test here; the post-merge tests turn it on
	if w.realRT != nil {
		d.Runtime = w.realRT
	}
	if w.realPack != "" {
		d.PackDir = w.realPack
	}
	return d
}

// tick moves the clock and runs one whole sweep; it returns what it logged.
func (w *fvWorld) tick(d time.Duration) string {
	w.t.Helper()
	w.clock = w.clock.Add(d)
	at := w.log.Len()
	if code := runSweep(context.Background(), w.deps()); code != 0 {
		w.t.Fatalf("sweep exit %d\n%s", code, w.log.String())
	}
	return w.log.String()[at:]
}

// look is a tick far enough on for the exit check to look again.
func (w *fvWorld) look() string { return w.tick(pipefixJudgeEvery + time.Second) }

func (w *fvWorld) rec() beadstore.Record {
	w.t.Helper()
	rec, ok := w.fixRecord()
	if !ok {
		w.t.Fatal("no fix record")
	}
	return rec
}

func (w *fvWorld) mr() glab.MergeRequest {
	w.t.Helper()
	mrs := w.fixMRs()
	if len(mrs) != 1 {
		w.t.Fatalf("%d merge request(s) on the fix branch, want exactly one", len(mrs))
	}
	return mrs[0]
}

// fvJob is one job attempt of a fake pipeline. log is its trace when failed.
type fvJob struct {
	name, stage, status string
	allowFailure        bool
	log                 string
}

func fvPass(name string) fvJob { return fvJob{name: name, stage: fvStage(name), status: "success"} }
func fvFail(name, log string) fvJob {
	return fvJob{name: name, stage: fvStage(name), status: "failed", log: log}
}
func fvStage(name string) string {
	if name == "build" {
		return "build"
	}
	return "test"
}

// mrPipeline adds one pipeline of the fix merge request on the CURRENT fix
// commit, with these job attempts in order (a later attempt of a name is a
// retry), and returns its id.
func (w *fvWorld) mrPipeline(status string, jobs ...fvJob) int64 {
	w.t.Helper()
	rec := w.rec()
	return w.pipelineOn(rec.PipelineFix.FixCommitSHA, w.branch(), "merge_request_event", status, rec.PipelineFix.MRIID, jobs...)
}

func (w *fvWorld) pipelineOn(sha, ref, source, status string, mrIID int64, jobs ...fvJob) int64 {
	w.t.Helper()
	w.next++
	id := w.next
	w.r.gl.AddPipeline(w.r.pid, glab.Pipeline{ID: id, Ref: ref, SHA: sha, Status: status, Source: source, CreatedAt: w.clock})
	if mrIID > 0 {
		w.r.gl.AddMRPipeline(w.r.pid, mrIID, id)
	}
	for i, j := range jobs {
		jid := id*100 + int64(i+1)
		job := glab.Job{ID: jid, Name: j.name, Stage: j.stage, Status: j.status, AllowFailure: j.allowFailure}
		if j.status == "failed" {
			job.FailureReason = "script_failure"
		}
		w.r.gl.AddJob(w.r.pid, id, job)
		if j.log != "" {
			w.r.gl.SetTrace(w.r.pid, jid, []byte(j.log))
		}
	}
	return id
}

// stateLabels are the fix merge request's STATE labels (exactly one is the
// invariant), sorted.
func stateLabels(mr glab.MergeRequest) []string {
	var out []string
	for _, l := range mr.Labels {
		if hasString(fixStateLabels, l) {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// statusBlock is the text between the status markers of a description.
func statusBlock(t *testing.T, desc string) string {
	t.Helper()
	a := strings.Index(desc, fixStatusOpen)
	b := strings.Index(desc, fixStatusClose)
	if a < 0 || b < a || strings.Count(desc, fixStatusOpen) != 1 || strings.Count(desc, fixStatusClose) != 1 {
		t.Fatalf("the description does not hold exactly one status block:\n%s", desc)
	}
	return strings.TrimSpace(desc[a+len(fixStatusOpen) : b])
}

// requireNoMergeCall fails if anything in the write log accepts, approves,
// assigns or pushes: the exit check never does.
func requireNoMergeCall(t *testing.T, ws []glabtest.Write) {
	t.Helper()
	for _, wr := range ws {
		if strings.HasSuffix(wr.Path, "/merge") || strings.Contains(wr.Path, "/approve") ||
			strings.Contains(wr.Body, "assignee") || strings.Contains(wr.Body, "reviewer") {
			t.Fatalf("the exit check sent %s", faLine(wr))
		}
	}
}

// ---- the table: every verdict, its state, its meter outcome ----------------------

type fvCase struct {
	name string
	opts faOpts
	// arrange puts GitLab into the state the exit check finds.
	arrange func(w *fvWorld)

	rule      string
	fixState  pipefix.State
	recState  beadstore.State
	verdict   pipefix.Verdict
	outcome   string
	label     string
	noteState string
	// status: a substring of the merge request's status block.
	status string
	// reslung: the record was re-slung for a further attempt.
	reslung    bool
	needsHuman bool
	noAuto     []string
}

func fvCases() []fvCase {
	return []fvCase{
		{name: "FIXED", arrange: func(w *fvWorld) { w.mrPipeline("success", fvPass("build"), fvPass("unit-tests")) },
			rule: "J5.FIXED", fixState: pipefix.StateFixed, recState: beadstore.StateDone, verdict: pipefix.VerdictFixed,
			outcome: meterapi.OutcomeSuccess, label: labelFixVerified, noteState: "fixed", status: "**Verified.**"},
		{name: "FIXED_NEW_FAILURE: the diagnosed job fails differently",
			arrange: func(w *fvWorld) { w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", fvOtherLog)) },
			rule:    "J3.CHANGED", fixState: pipefix.StateFixedNewFailure, recState: beadstore.StateDone, verdict: pipefix.VerdictFixedNewFailure,
			outcome: meterapi.OutcomeSuccess, label: labelFixVerified, noteState: "fixed-new-failure", status: "**Verified, with a different failure.**",
			noAuto: []string{"verdict-fixed-new-failure"}},
		{name: "FLAKY: a human retried the failed job and it passed",
			arrange: func(w *fvWorld) {
				w.mrPipeline("success", fvPass("build"), fvFail("unit-tests", plTestLog), fvPass("unit-tests"))
			},
			rule: "J3.FLAKY(recurs-and-passes)", fixState: pipefix.StateFlaky, recState: beadstore.StateDone, verdict: pipefix.VerdictFlaky,
			outcome: meterapi.OutcomeSuccess, label: labelFixInconclusive, noteState: "flaky", status: "**Inconclusive: flaky.**",
			needsHuman: true, noAuto: []string{"verdict-flaky"}},
		{name: "UNVERIFIABLE: the diagnosed job did not run",
			arrange: func(w *fvWorld) { w.mrPipeline("success", fvPass("build")) },
			rule:    "J3.NOT_OBSERVED(absent)", fixState: pipefix.StateUnverifiable, recState: beadstore.StateDone, verdict: pipefix.VerdictUnverifiable,
			outcome: meterapi.OutcomeSuccess, label: labelFixInconclusive, noteState: "unverifiable", status: "**Inconclusive.**",
			needsHuman: true, noAuto: []string{"verdict-unverifiable"}},
		{name: "REGRESSED: a job that passed on the base fails",
			arrange: func(w *fvWorld) { w.mrPipeline("failed", fvFail("build", plLintLog), fvPass("unit-tests")) },
			rule:    "J4.REGRESSED", fixState: pipefix.StateRegressed, recState: beadstore.StateDone, verdict: pipefix.VerdictRegressed,
			outcome: meterapi.OutcomeSuccess, label: labelFixInconclusive, noteState: "regressed", status: "**Inconclusive: a job that passed before",
			needsHuman: true, noAuto: []string{"verdict-regressed"}},
		{name: "NOT_FIXED: the diagnosed job fails the same way",
			arrange: func(w *fvWorld) { w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog)) },
			rule:    "J3.RECURS", fixState: "", recState: beadstore.StateRunning, verdict: pipefix.VerdictNotFixed,
			outcome: meterapi.OutcomeGateFailed, label: labelFixVerifying, noteState: "not-fixed", status: "**Not fixed yet.**",
			reslung: true, noAuto: []string{"verdict-not-fixed"}},
		{name: "FORGED: the diagnosed job was allowed to fail",
			arrange: func(w *fvWorld) {
				j := fvFail("unit-tests", plTestLog)
				j.allowFailure = true
				w.mrPipeline("success", fvPass("build"), j)
			},
			rule: "J3.WEAKENED", fixState: "", recState: beadstore.StateRunning, verdict: pipefix.VerdictForged,
			outcome: meterapi.OutcomeGateFailed, label: labelFixVerifying, noteState: "not-fixed", status: "**Not fixed yet.**",
			reslung: true, noAuto: []string{"verdict-not-fixed"}},
		{name: "stopped: the merge request was closed",
			arrange: func(w *fvWorld) { w.r.gl.SetMRState(w.r.pid, w.rec().PipelineFix.MRIID, "closed", w.clock) },
			rule:    "J0.CLOSED", fixState: pipefix.StateClosedUnmerged, recState: beadstore.StateDone,
			outcome: meterapi.OutcomeSuccess, label: labelFixInconclusive, noteState: "closed-unmerged", status: "**Closed.**",
			noAuto: []string{"verdict-closed-unmerged"}},
		{name: "stopped: somebody else pushed to the branch",
			arrange: func(w *fvWorld) {
				w.r.gl.SetMRHead(w.r.pid, w.rec().PipelineFix.MRIID, "9999999999999999999999999999999999999999")
			},
			rule: "J0.TAKEN_OVER", fixState: pipefix.StateTakenOver, recState: beadstore.StateDone,
			outcome: meterapi.OutcomeSuccess, label: labelFixInconclusive, noteState: "taken-over", status: "**Taken over.**",
			noAuto: []string{"verdict-taken-over"}},
		{name: "stopped: the merge request was merged before any verdict",
			arrange: func(w *fvWorld) { w.r.gl.SetMRState(w.r.pid, w.rec().PipelineFix.MRIID, "merged", w.clock) },
			rule:    "J0.MERGED", fixState: pipefix.StateMerged, recState: beadstore.StateDone,
			// A merged merge request is not edited: it keeps the label it merged with.
			outcome: meterapi.OutcomeSuccess, label: labelFixVerifying, noteState: "merged", status: "**Verifying.**",
			noAuto: []string{"verdict-merged"}},
	}
}

// EVERY DECISION OF THE EXIT CHECK: the fix state, the record's state, ONE
// meter outcome for the attempt, the fix note, the merge request's status
// block and its one state label -- and nothing that merges. The ledger row of
// each case is printed under -v (the plan's evidence log quotes them).
func TestTheExitCheckSettlesEveryDecision(t *testing.T) {
	for _, c := range fvCases() {
		t.Run(c.name, func(t *testing.T) {
			w := newFVWorld(t, c.opts)
			w.om.outcomeNext = "done"
			if c.outcome != meterapi.OutcomeSuccess {
				w.om.outcomeNext = "retry"
			}
			c.arrange(w)
			log := w.look()
			rec := w.rec()
			pf := rec.PipelineFix

			out := w.om.requests()
			if len(out) != 1 {
				t.Fatalf("%d outcome report(s), want exactly one\n%s", len(out), log)
			}
			o := out[0]
			t.Logf("LEDGER fix_state=%-18s verdict=%-18s rule=%-28s outcome=%-12s attempt=%d rung=%s reservation=%s trigger=%s",
				orDash(string(c.fixState), c.noteState), orDash(string(c.verdict), "-"), c.rule, o.Outcome, o.Attempt, o.Rung, o.ReservationID, o.Trigger)
			if o.Outcome != c.outcome || o.ReservationID != "rsv-fix-1" || o.Attempt != 1 || o.BeadID != w.fixAnchor() || o.Trigger != "pipeline-fix" {
				t.Fatalf("outcome %+v, want %s on the fix's reservation", o, c.outcome)
			}
			if rec.State != c.recState || pf.State != string(c.fixState) {
				t.Fatalf("record state %q fix state %q, want %q / %q\n%s", rec.State, pf.State, c.recState, c.fixState, log)
			}
			if pf.Verdict != string(c.verdict) || pf.VerdictRule != c.rule || pf.VerdictAttempt != 1 || pf.Outcome != c.outcome || pf.OutcomeAttempt != 1 {
				t.Fatalf("recorded verdict %q by %q (attempt %d), outcome %q (attempt %d); want %q by %q, %q",
					pf.Verdict, pf.VerdictRule, pf.VerdictAttempt, pf.Outcome, pf.OutcomeAttempt, c.verdict, c.rule, c.outcome)
			}
			if !strings.Contains(log, `msg="sweep: pipeline fix settled"`) || !strings.Contains(log, "outcome="+c.outcome) {
				t.Fatalf("no settled line with the outcome:\n%s", log)
			}
			// The python-less fixture is Go, decided: the only reason is the verdict.
			if !reflect.DeepEqual(pf.NoAutoMerge, c.noAuto) {
				t.Fatalf("NoAutoMerge %v, want %v", pf.NoAutoMerge, c.noAuto)
			}
			if c.reslung != !rec.ReslungAt.IsZero() {
				t.Fatalf("reslung at %v, want reslung=%v", rec.ReslungAt, c.reslung)
			}
			if c.reslung {
				if pf.Feedback == "" || !strings.Contains(pf.Feedback, c.rule) || pf.FixCommitSHA == "" || pf.MRIID == 0 || len(pf.ChangedFiles) == 0 {
					t.Fatalf("a re-slung fix must keep its commit, merge request and changed files and carry feedback: %+v", pf)
				}
				if st, _, known := fixStateOf(rec, w.clock); st != pipefix.StateProposing || !known {
					t.Fatalf("a re-slung fix reads as %q, want proposing", st)
				}
			}

			// GitLab: the note, the status block, exactly one state label.
			if st, body, ok := w.fixNote(); !ok || st != c.noteState {
				t.Fatalf("fix note state %q (found %v), want %q\n%s", st, ok, c.noteState, body)
			}
			mr := w.mr()
			if got := stateLabels(mr); !reflect.DeepEqual(got, []string{c.label}) {
				t.Fatalf("state labels %v, want exactly [%s]", got, c.label)
			}
			if sb := statusBlock(t, mr.Description); !strings.Contains(sb, c.status) {
				t.Fatalf("status block does not say %q:\n%s", c.status, sb)
			}
			if hasString(mr.Labels, labelFixNewFailure) != (c.fixState == pipefix.StateFixedNewFailure) {
				t.Fatalf("labels %v: gonk::fix-new-failure must be there exactly for fixed-new-failure", mr.Labels)
			}
			if a, cause, ok := pipelinectx.ParseFixMRMarker(mr.Description); !ok || a != w.fixAnchor() || cause != pf.CauseKey {
				t.Fatalf("the status edit damaged the merge request marker (%q %q %v)", a, cause, ok)
			}
			if !strings.Contains(mr.Description, "Related to #1") || closingPattern.MatchString(mr.Description) {
				t.Fatalf("the edited description lost its link or gained a closing pattern:\n%s", mr.Description)
			}
			if hasString(w.landingLabels(), labelNeedsHuman) != c.needsHuman {
				t.Fatalf("landing labels %v, want needs-human=%v", w.landingLabels(), c.needsHuman)
			}
			if mr.State != "merged" && (mr.State != "opened" && c.fixState != pipefix.StateClosedUnmerged) {
				t.Fatalf("the merge request is %q", mr.State)
			}
			requireNoMergeCall(t, w.written())

			// AND ONLY ONCE: later ticks report nothing more and write nothing.
			before := len(w.r.gl.Writes())
			w.tick(30 * time.Second)
			w.tick(30 * time.Second)
			if n := len(w.om.requests()); n != 1 {
				t.Fatalf("%d outcome reports after two more ticks, want 1: %+v", n, w.om.requests())
			}
			if n := len(w.r.gl.Writes()) - before; n != 0 {
				t.Fatalf("%d GitLab write(s) after the settlement: %v", n, faLines(w.r.gl.Writes()[before:]))
			}
		})
	}
}

func orDash(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// ---- the four tests the plan's row names ------------------------------------------

// NOT_FIXED IS NOT SUCCESS: gate-failed (the session spent tokens), a
// re-sling with feedback, the order fired once.
func TestNotFixedIsGateFailedAndReslungWithFeedback(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	w.om.outcomeNext = "escalate"
	fired := len(w.orders.orders())
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	log := w.look()
	out := w.om.requests()
	if len(out) != 1 || out[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcomes %+v, want one gate-failed\n%s", out, log)
	}
	rec := w.rec()
	orders := w.orders.orders()
	if len(orders) != fired+1 || orders[len(orders)-1].Vars["trigger"] != "pipeline-fix" || orders[len(orders)-1].Vars["bead_anchor"] != w.fixAnchor() {
		t.Fatalf("the re-sling fired %d order(s): %+v", len(orders)-fired, orders[fired:])
	}
	if rec.State != beadstore.StateRunning || rec.PipelineFix.State != "" || rec.ReslungAt.IsZero() || rec.ReslingFireBy.IsZero() || !rec.SessionEndedAt.IsZero() {
		t.Fatalf("record after not-fixed: state %q fix state %q reslung %v fire_by %v ended %v", rec.State, rec.PipelineFix.State,
			rec.ReslungAt, rec.ReslingFireBy, rec.SessionEndedAt)
	}
	want := `NOT_FIXED by rule J3.RECURS: a diagnosed job still failed the way it was diagnosed in the fix's pipeline ("unit-tests" J3.RECURS)`
	if rec.PipelineFix.Feedback != want {
		t.Fatalf("feedback %q\nwant     %q", rec.PipelineFix.Feedback, want)
	}
	// A not-fixed attempt whose tokens meter cannot show is NOT gate-failed:
	// an escalation must be paid for.
	t.Run("without proven tokens it is infra-failed", func(t *testing.T) {
		w := newFVWorld(t, faOpts{})
		w.om.sessionCost.TotalTokens = 0
		w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
		w.look()
		if out := w.om.requests(); len(out) != 1 || out[0].Outcome != meterapi.OutcomeInfraFailed {
			t.Fatalf("outcomes %+v, want one infra-failed", out)
		}
	})
}

// REGRESSED DOES NOT ESCALATE (plan 7.3, said plainly there): success, no
// re-sling, a person.
func TestRegressedIsNotAnEscalation(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	fired := len(w.orders.orders())
	w.mrPipeline("failed", fvFail("build", plLintLog), fvPass("unit-tests"))
	w.look()
	out := w.om.requests()
	if len(out) != 1 || out[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcomes %+v, want one success", out)
	}
	rec := w.rec()
	if rec.PipelineFix.State != string(pipefix.StateRegressed) || rec.State != beadstore.StateDone || !rec.ReslungAt.IsZero() ||
		len(w.orders.orders()) != fired {
		t.Fatalf("regressed: fix state %q record %q reslung %v orders +%d", rec.PipelineFix.State, rec.State, rec.ReslungAt, len(w.orders.orders())-fired)
	}
}

// A PIPELINE THAT IS STILL RUNNING YIELDS NO VERDICT: nothing reported, the
// record untouched but for the look's stamp, and after the first look nothing
// written to GitLab at all. So does a `failed` pipeline whose retry has not
// run (gonk-5b2f).
func TestAPipelineStillRunningYieldsNoVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(w *fvWorld)
	}{
		{"no pipeline yet", func(*fvWorld) {}},
		{"the pipeline is running", func(w *fvWorld) {
			w.mrPipeline("running", fvPass("build"), fvJob{name: "unit-tests", stage: "test", status: "running"})
		}},
		{"the pipeline is failed and its retry is still created (gonk-5b2f)", func(w *fvWorld) {
			w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog), fvJob{name: "unit-tests", stage: "test", status: "created"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFVWorld(t, faOpts{})
			tc.arrange(w)
			log := w.look()
			rec := w.rec()
			if len(w.om.requests()) != 0 || rec.PipelineFix.State != string(pipefix.StateVerifying) || rec.State != beadstore.StateRunning ||
				rec.PipelineFix.Verdict != "" || rec.PipelineFix.JudgedAt.IsZero() {
				t.Fatalf("outcomes %+v, fix state %q, verdict %q, judged at %v\n%s", w.om.requests(), rec.PipelineFix.State,
					rec.PipelineFix.Verdict, rec.PipelineFix.JudgedAt, log)
			}
			if !strings.Contains(log, "decision=wait") || !strings.Contains(log, "rule=J1.WAIT") {
				t.Fatalf("the look did not log the wait:\n%s", log)
			}
			if got := stateLabels(w.mr()); !reflect.DeepEqual(got, []string{labelFixVerifying}) {
				t.Fatalf("state labels %v", got)
			}
			// The first look put the fix line on the run note; later looks
			// write nothing -- and READ nothing of the landing: no issue, no
			// note, not who the bot is. Only the merge request and its
			// pipelines. (Run maintenance, five minutes apart, is another
			// pass and is not due inside these two looks.)
			w.resetWrites()
			reads := len(w.r.gl.Requests())
			w.tick(pipefixJudgeEvery + time.Second)
			if ws := w.written(); len(ws) != 0 || len(w.om.requests()) != 0 {
				t.Fatalf("a waiting fix wrote %v and reported %+v", faLines(ws), w.om.requests())
			}
			for _, r := range w.r.gl.Requests()[reads:] {
				if strings.Contains(r, "/issues") || strings.Contains(r, "/notes") || strings.HasSuffix(r, "/user") {
					t.Fatalf("a waiting look that changes nothing read the landing: %s\nall: %v", r, w.r.gl.Requests()[reads:])
				}
			}
		})
	}
}

// THE LOOK IS CHEAP: at most once per pipefixJudgeEvery, whatever the tick
// rate, and a look that is skipped sends GitLab nothing.
func TestTheExitCheckLooksAtMostEveryTwoMinutes(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	// The exit check's reads are the ones of the fix merge request; run
	// maintenance (five minutes apart, another pass) reads the target.
	mrPath := fmt.Sprintf("/merge_requests/%d", w.rec().PipelineFix.MRIID)
	looks := func() int {
		n := 0
		for _, r := range w.r.gl.Requests() {
			if strings.HasSuffix(r, mrPath) {
				n++
			}
		}
		return n
	}
	w.look()
	first := looks()
	if first == 0 {
		t.Fatal("the first look did not read the merge request")
	}
	w.tick(30 * time.Second)
	w.tick(30 * time.Second)
	w.tick(30 * time.Second)
	if n := looks() - first; n != 0 {
		t.Fatalf("%d look(s) within two minutes of the last one", n)
	}
	w.tick(30*time.Second + time.Second)
	if looks() == first {
		t.Fatal("no look two minutes on")
	}
}

// A HUMAN RETRY IS NOT A PASS: the failed attempt is still an attempt.
func TestAHumanRetryDoesNotYieldFixed(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	w.mrPipeline("success", fvPass("build"), fvFail("unit-tests", plTestLog), fvPass("unit-tests"))
	w.look()
	rec := w.rec()
	if rec.PipelineFix.State != string(pipefix.StateFlaky) || rec.PipelineFix.Verdict != string(pipefix.VerdictFlaky) {
		t.Fatalf("fix state %q verdict %q, want flaky / FLAKY", rec.PipelineFix.State, rec.PipelineFix.Verdict)
	}
	if got := stateLabels(w.mr()); !reflect.DeepEqual(got, []string{labelFixInconclusive}) {
		t.Fatalf("state labels %v, want inconclusive -- never verified", got)
	}
}

// ---- bounds ---------------------------------------------------------------------

// A VERIFYING RECORD IS NOT SWEPT AS STALE. Its session is long over and the
// default reservation TTL has passed, and the merge request's pipeline is
// still running inside the verify timeout: nothing is reported and nothing is
// stopped. (The break: judging it by the base reservation TTL.)
func TestAVerifyingRecordIsNotSweptAsStale(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	w.mrPipeline("running", fvPass("build"), fvJob{name: "unit-tests", stage: "test", status: "running"})
	stops := w.rt.stopCount()
	// Past the default reservation TTL, inside verify_timeout (2h).
	log := w.tick(opercfg.DefaultReservationTTL + 20*time.Minute)
	rec := w.rec()
	if len(w.om.requests()) != 0 || rec.State != beadstore.StateRunning || rec.PipelineFix.State != string(pipefix.StateVerifying) {
		t.Fatalf("a verifying fix past the base TTL: outcomes %+v record %q fix %q\n%s", w.om.requests(), rec.State, rec.PipelineFix.State, log)
	}
	if w.rt.stopCount() != stops {
		t.Fatalf("a session was stopped (%d -> %d)", stops, w.rt.stopCount())
	}
	if strings.Contains(log, "infra-failed") || strings.Contains(log, "STALE") {
		t.Fatalf("the sweep treated the verifying fix as stale:\n%s", log)
	}
}

// The fallback deadline of a record that lost its reservation deadline knows
// the fix trigger's longer reservation.
func TestTheFallbackDeadlineLearnsVerifyTimeout(t *testing.T) {
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	fix := beadstore.Record{Trigger: "pipeline-fix", UpdatedAt: at,
		PipelineFix: beadstore.PipelineFix{RunAnchor: "x", VerifyTimeoutSeconds: 7200}}
	if got, want := runningDeadline(fix), at.Add(opercfg.DefaultReservationTTL+2*time.Hour); !got.Equal(want) {
		t.Fatalf("fix fallback deadline %s, want %s", got, want)
	}
	// Clamped as meter clamps it; and no other trigger moves.
	fix.PipelineFix.VerifyTimeoutSeconds = 100 * 3600
	if got, want := runningDeadline(fix), at.Add(opercfg.DefaultReservationTTL+6*time.Hour); !got.Equal(want) {
		t.Fatalf("clamped fix fallback deadline %s, want %s", got, want)
	}
	other := fix
	other.Trigger = "pipeline-diagnosis"
	if got, want := runningDeadline(other), at.Add(opercfg.DefaultReservationTTL); !got.Equal(want) {
		t.Fatalf("a diagnosis fallback deadline moved: %s, want %s", got, want)
	}
	// A deadline meter minted is used as it is.
	minted := fix
	minted.ReservationExpiresAt = at.Add(time.Minute)
	if got := runningDeadline(minted); !got.Equal(at.Add(time.Minute)) {
		t.Fatalf("a minted deadline was replaced: %s", got)
	}
}

// An UNCLAIMED pipefix session is not "stale" at the age every other session
// is: its Job's own deadline is the fix reservation's.
func TestAnUnclaimedPipefixSessionIsNotStaleBeforeItsOwnDeadline(t *testing.T) {
	rt := newTestRuntime()
	store := beadstore.NewMemory()
	fix := rt.startNamed(t, runtime.Name(pipefixAgent, 7, runtime.KindPipeline, 900, 1))
	triage := rt.startNamed(t, runtime.Name("triage", 7, runtime.KindIssue, 3, 1))
	var log bytes.Buffer
	started := time.Now()
	for _, h := range []runtime.Handle{fix, triage} {
		rt.SetStatus(h, runtime.Status{Phase: runtime.PhaseRunning, StartedAt: started})
	}
	d := (&sweepDeps{Runtime: rt, Store: store, Log: testLogger(&log)}).withDefaults()
	at := func(age time.Duration) []runtime.Handle {
		d.Now = func() time.Time { return started.Add(age) }
		reapStaleSessions(context.Background(), d)
		return rt.live(t)
	}
	if live := at(staleSessionAfter + time.Minute); !reflect.DeepEqual(live, []runtime.Handle{fix}) {
		t.Fatalf("past staleSessionAfter: live %v, want only the pipefix session %s (triage %s reaped)\n%s", live, fix, triage, log.String())
	}
	if live := at(staleFixSessionAfter + time.Minute); len(live) != 0 {
		t.Fatalf("past staleFixSessionAfter the pipefix session is still alive: %v", live)
	}
}

// A RECORD THAT CANNOT BE JUDGED DOES NOT LIVE FOR EVER. The merge request
// cannot be read (Judge has no deadline for that): before the verify deadline
// plus the grace nothing happens; past it the fix is `unverifiable` by the
// check's own named rule, reported once, and done.
func TestACannotJudgeRecordEndsAtItsBound(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	rec := w.rec()
	w.r.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d", w.r.pid, rec.PipelineFix.MRIID), 500)
	log := w.look()
	if !strings.Contains(log, "decision=cannot-judge") || len(w.om.requests()) != 0 {
		t.Fatalf("an unreadable merge request: outcomes %+v\n%s", w.om.requests(), log)
	}
	// At the verify deadline itself: still cannot-judge, still waiting.
	w.clock = rec.PipelineFix.VerifyDeadline
	w.look()
	if got := w.rec(); len(w.om.requests()) != 0 || got.PipelineFix.State != string(pipefix.StateVerifying) {
		t.Fatalf("at the deadline: outcomes %+v fix state %q", w.om.requests(), got.PipelineFix.State)
	}
	// Short of the bound: nothing. At it: settled.
	w.clock = rec.PipelineFix.VerifyDeadline.Add(pipefixJudgeGrace - 3*pipefixJudgeEvery)
	w.look()
	if len(w.om.requests()) != 0 {
		t.Fatalf("settled before the bound: %+v", w.om.requests())
	}
	w.clock = rec.PipelineFix.VerifyDeadline.Add(pipefixJudgeGrace)
	log = w.tick(0)
	got := w.rec()
	out := w.om.requests()
	if len(out) != 1 || out[0].Outcome != meterapi.OutcomeSuccess || got.State != beadstore.StateDone ||
		got.PipelineFix.State != string(pipefix.StateUnverifiable) || got.PipelineFix.VerdictRule != ruleVerifyBoundCannotJudge {
		t.Fatalf("at the bound: outcomes %+v record %q fix %q rule %q\n%s", out, got.State, got.PipelineFix.State, got.PipelineFix.VerdictRule, log)
	}
	if !strings.Contains(got.PipelineFix.VerdictDetail, "no verdict could be given") {
		t.Fatalf("the bound's reason is not named: %q", got.PipelineFix.VerdictDetail)
	}
	w.tick(time.Minute)
	if len(w.om.requests()) != 1 {
		t.Fatalf("reported again after the bound: %+v", w.om.requests())
	}
}

// `awaiting-run`: a CI change committed with [skip ci]. While nobody starts
// the pipeline the fix waits; once a maintainer starts one it is `verifying`
// (note, status block and label follow, in one request); past the deadline
// with none started it is `unverifiable` and a success for the meter.
func TestAwaitingRunBecomesVerifyingOrTimesOut(t *testing.T) {
	// The diagnosed job's own script: an execution-changing edit, which the
	// `ci` lift permits for a job of T and for no other.
	ciEdit := faFix(faEdit(faCI, "  - ./run-tests\n", "  - ./run-tests --all\n"))
	newWorld := func(t *testing.T) *fvWorld {
		w := newFVWorldWith(t, faOpts{class: pipefix.ClassConfiguration, cfg: func(c *meterapi.ProjectResponse) {
			c.Effective.PipelineFix.CIExecChanges = "manual-run"
		}}, ciEdit)
		if st := w.rec().PipelineFix.State; st != string(pipefix.StateAwaitingRun) {
			t.Fatalf("setup: fix state %q, want awaiting-run\n%s", st, w.log.String())
		}
		return w
	}
	t.Run("a maintainer starts the pipeline", func(t *testing.T) {
		w := newWorld(t)
		// What GitLab made of the [skip ci] push: skipped, no jobs.
		w.mrPipeline("skipped")
		w.look()
		if st := w.rec().PipelineFix.State; st != string(pipefix.StateAwaitingRun) || len(w.om.requests()) != 0 {
			t.Fatalf("a skipped pipeline moved the fix to %q (outcomes %+v)", st, w.om.requests())
		}
		w.resetWrites()
		w.mrPipeline("running", fvJob{name: "build", stage: "build", status: "running"})
		w.look()
		rec := w.rec()
		if !hasString(w.mr().Labels, labelCIExecChanged) {
			t.Fatalf("labels %v lack %s", w.mr().Labels, labelCIExecChanged)
		}
		if rec.PipelineFix.State != string(pipefix.StateVerifying) || len(w.om.requests()) != 0 {
			t.Fatalf("after the pipeline was started: fix state %q outcomes %+v\n%s", rec.PipelineFix.State, w.om.requests(), w.log.String())
		}
		mr := w.mr()
		if got := stateLabels(mr); !reflect.DeepEqual(got, []string{labelFixVerifying}) {
			t.Fatalf("state labels %v, want exactly verifying", got)
		}
		if st, _, _ := w.fixNote(); st != string(pipefix.StateVerifying) {
			t.Fatalf("fix note %q", st)
		}
		// ONE request swapped the label: never two state labels, never none.
		swaps := 0
		for _, wr := range w.written() {
			if wr.Method == "PUT" && strings.HasSuffix(wr.Path, fmt.Sprintf("/merge_requests/%d", mr.IID)) {
				swaps++
				if !strings.Contains(wr.Body, labelFixVerifying) || !strings.Contains(wr.Body, labelFixAwaitingRun) ||
					!strings.Contains(wr.Body, "add_labels") || !strings.Contains(wr.Body, "remove_labels") {
					t.Fatalf("the label swap is not one request adding and removing: %s", faLine(wr))
				}
			}
		}
		if swaps != 1 {
			t.Fatalf("%d merge request updates, want 1: %v", swaps, faLines(w.written()))
		}
	})
	t.Run("nobody starts it before the deadline", func(t *testing.T) {
		w := newWorld(t)
		w.mrPipeline("skipped")
		w.clock = w.rec().PipelineFix.VerifyDeadline
		w.look()
		rec := w.rec()
		out := w.om.requests()
		if len(out) != 1 || out[0].Outcome != meterapi.OutcomeSuccess || rec.PipelineFix.State != string(pipefix.StateUnverifiable) ||
			rec.PipelineFix.VerdictRule != "J1.TIMEOUT" || rec.State != beadstore.StateDone {
			t.Fatalf("outcomes %+v fix state %q rule %q", out, rec.PipelineFix.State, rec.PipelineFix.VerdictRule)
		}
		if got := stateLabels(w.mr()); !reflect.DeepEqual(got, []string{labelFixInconclusive}) {
			t.Fatalf("state labels %v", got)
		}
		// A CI change of this kind is never merged automatically, whatever
		// the verdict: both reasons are on the record.
		if !hasString(rec.PipelineFix.NoAutoMerge, noAutoMergeExec) {
			t.Fatalf("NoAutoMerge %v lacks %s", rec.PipelineFix.NoAutoMerge, noAutoMergeExec)
		}
	})
}

// ---- exactly once ---------------------------------------------------------------

// THE OUTCOME IS REPORTED ONCE PER ATTEMPT -- also when the settlement has to
// be repeated: a meter that fails the first report is asked again on the next
// tick, and a record write that is lost repeats the settlement without a
// second accepted report mattering (the record ends the repetition).
func TestTheOutcomeIsReportedOncePerAttempt(t *testing.T) {
	t.Run("a verdict is reported once across many ticks", func(t *testing.T) {
		w := newFVWorld(t, faOpts{})
		w.mrPipeline("success", fvPass("build"), fvPass("unit-tests"))
		for i := 0; i < 6; i++ {
			w.look()
		}
		if out := w.om.requests(); len(out) != 1 || out[0].Outcome != meterapi.OutcomeSuccess {
			t.Fatalf("outcomes after six looks: %+v", out)
		}
	})
	t.Run("a re-slung fix is not reported again inside its window", func(t *testing.T) {
		w := newFVWorld(t, faOpts{})
		w.om.outcomeNext = "retry"
		w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
		w.look()
		fired := len(w.orders.orders())
		w.tick(30 * time.Second)
		w.tick(30 * time.Second)
		w.tick(time.Minute)
		if out := w.om.requests(); len(out) != 1 || len(w.orders.orders()) != fired {
			t.Fatalf("inside the re-sling window: %d outcome(s), %d further order(s)", len(out), len(w.orders.orders())-fired)
		}
	})
	t.Run("a failed report leaves the fix verifying and is repeated", func(t *testing.T) {
		w := newFVWorld(t, faOpts{})
		w.mrPipeline("success", fvPass("build"), fvPass("unit-tests"))
		d := w.deps()
		d.Meter = meterClient("http://127.0.0.1:1") // nothing listens
		w.clock = w.clock.Add(pipefixJudgeEvery + time.Second)
		if code := runSweep(context.Background(), d); code != 0 {
			t.Fatalf("sweep exit %d", code)
		}
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateVerifying) || rec.State != beadstore.StateRunning || rec.PipelineFix.Outcome != "" {
			t.Fatalf("after a failed report: fix state %q record %q outcome %q", rec.PipelineFix.State, rec.State, rec.PipelineFix.Outcome)
		}
		w.tick(30 * time.Second) // no throttle stamp was written: the next tick settles
		if out, rec := w.om.requests(), w.rec(); len(out) != 1 || rec.PipelineFix.State != string(pipefix.StateFixed) {
			t.Fatalf("after the retry: outcomes %+v fix state %q", out, rec.PipelineFix.State)
		}
	})
	t.Run("GitLab first: a verdict that cannot be shown is not reported yet", func(t *testing.T) {
		w := newFVWorld(t, faOpts{})
		w.mrPipeline("success", fvPass("build"), fvPass("unit-tests"))
		w.r.gl.FailAlways("PUT", fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d", w.r.pid, w.rec().PipelineFix.MRIID), 500)
		w.look()
		if rec := w.rec(); len(w.om.requests()) != 0 || rec.PipelineFix.State != string(pipefix.StateVerifying) {
			t.Fatalf("reported %+v with the merge request not updated (fix state %q)", w.om.requests(), rec.PipelineFix.State)
		}
		w.r.gl.ClearFailures()
		w.look()
		if out, rec := w.om.requests(), w.rec(); len(out) != 1 || rec.PipelineFix.State != string(pipefix.StateFixed) {
			t.Fatalf("after GitLab came back: outcomes %+v fix state %q", out, rec.PipelineFix.State)
		}
	})
}

// ---- the note, the labels -------------------------------------------------------

// THE FIX NOTE NEVER MOVES BACKWARDS, whoever writes: the rule as a table.
func TestFixNoteMayReplaceNeverMovesANoteBackwards(t *testing.T) {
	verify := func(s string) fixNote { return fixNote{State: s, Verify: true} }
	apply := func(s string) fixNote { return fixNote{State: s, Apply: true} }
	dispatch := func(s string) fixNote { return fixNote{State: s} }
	for _, c := range []struct {
		name     string
		n        fixNote
		existing string
		want     bool
	}{
		// forward
		{"verify: verifying -> fixed", verify("fixed"), "verifying", true},
		{"verify: awaiting-run -> verifying", verify("verifying"), "awaiting-run", true},
		{"verify: verifying -> not-fixed", verify("not-fixed"), "verifying", true},
		{"verify: not-fixed -> denied", verify("denied"), "not-fixed", true},
		{"verify: verifying -> denied", verify("denied"), "verifying", true},
		{"verify: proposing -> unverifiable", verify("unverifiable"), "proposing", true},
		{"verify: fixed -> merged", verify("merged"), "fixed", true},
		{"verify: unverifiable -> merged", verify("merged"), "unverifiable", true},
		{"verify: merged -> confirmed", verify("confirmed"), "merged", true},
		{"verify: merged -> recurred", verify("recurred"), "merged", true},
		{"verify: fixed -> confirmed", verify("confirmed"), "fixed", true},
		{"verify: the same state said again", verify("merged"), "merged", true},
		{"verify: fixed -> closed-unmerged is a verdict over a verdict", verify("closed-unmerged"), "fixed", false},
		// backwards
		{"verify: fixed -> verifying", verify("verifying"), "fixed", false},
		{"verify: fixed -> not-fixed", verify("not-fixed"), "fixed", false},
		{"verify: fixed -> flaky", verify("flaky"), "fixed", false},
		{"verify: merged -> fixed", verify("fixed"), "merged", false},
		{"verify: confirmed -> merged", verify("merged"), "confirmed", false},
		{"verify: confirmed -> recurred", verify("recurred"), "confirmed", false},
		{"verify: recurred -> confirmed", verify("confirmed"), "recurred", false},
		{"verify: fixed -> denied", verify("denied"), "fixed", false},
		{"verify: merged -> denied", verify("denied"), "merged", false},
		{"verify: a state this build does not know", verify("fixed"), "from-a-newer-build", false},
		{"verify: writing a state this build does not know", verify("from-a-newer-build"), "verifying", false},
		// the apply and the dispatch, against what the exit check wrote
		{"apply: fixed -> verifying", apply("verifying"), "fixed", false},
		{"apply: merged -> verifying", apply("verifying"), "merged", false},
		{"apply: confirmed -> verifying", apply("verifying"), "confirmed", false},
		{"apply: not-fixed -> verifying (the next attempt)", apply("verifying"), "not-fixed", true},
		{"dispatch: fixed -> proposing", dispatch("proposing"), "fixed", false},
		{"dispatch: not-fixed -> proposing", dispatch("proposing"), "not-fixed", false},
		{"dispatch: not-fixed -> denied (the next attempt was refused)", dispatch("denied"), "not-fixed", true},
		{"dispatch: fixed -> denied", dispatch("denied"), "fixed", false},
		{"dispatch: verifying -> denied", dispatch("denied"), "verifying", false},
	} {
		if got := fixNoteMayReplace(c.n, c.existing); got != c.want {
			t.Errorf("%s: fixNoteMayReplace = %v, want %v", c.name, got, c.want)
		}
	}
	// Every state has a rank, so no state silently falls out of the rule.
	for _, s := range pipelinectx.FixNoteStates() {
		if _, ok := fixNoteRank(s); !ok {
			t.Errorf("fix note state %q has no rank", s)
		}
	}
}

// On GitLab: a note the exit check moved to `fixed` is not moved back by a
// late write of an earlier state, and a PERSON's note quoting the marker is
// never edited.
func TestTheVerdictNoteIsNotMovedBackAndAHumansNoteIsNeverEdited(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	// A person quotes the fix marker, as a reply would.
	marker, _ := pipelinectx.FixMarker(w.fixAnchor(), "verifying")
	human := "I think this is wrong.\n\n> " + marker
	w.r.gl.AddNote(w.r.pid, 1, glab.User{ID: 77, Username: "a-person"}, human, false)
	w.mrPipeline("success", fvPass("build"), fvPass("unit-tests"))
	w.look()
	if st, _, ok := w.fixNote(); !ok || st != "fixed" {
		t.Fatalf("fix note %q", st)
	}
	for _, n := range w.landingNotes() {
		if n.Author.ID == 77 && n.Body != human {
			t.Fatalf("the person's note was edited:\n%s", n.Body)
		}
	}
	// A late write of an earlier state, through the same function the apply
	// and the dispatch use.
	run, _, _ := w.store.Get(context.Background(), w.r.rec.BeadAnchor)
	l, err := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(&w.log))
	if err != nil {
		t.Fatal(err)
	}
	for _, late := range []fixNote{
		{State: "verifying", Text: "late", Apply: true},
		{State: "proposing", Text: "late"},
		{State: "verifying", Text: "late", Verify: true},
		{State: "not-fixed", Text: "late", Verify: true},
	} {
		if changed, _, err := l.writeFixNote(context.Background(), run, w.fixAnchor(), late); changed || err == nil {
			t.Fatalf("a late %q note (apply=%v verify=%v) replaced `fixed`: changed=%v err=%v", late.State, late.Apply, late.Verify, changed, err)
		}
	}
	if st, _, _ := w.fixNote(); st != "fixed" {
		t.Fatalf("the fix note moved back to %q", st)
	}
}

// EXACTLY ONE STATE LABEL AT A TIME, at every instant: every request that
// adds a state label removes the one it replaces in the same request.
func TestExactlyOneStateLabelAtATime(t *testing.T) {
	// The mapping covers every fix state, and names one label or none.
	for _, s := range pipefix.States() {
		if l := fixStateLabel(s); l != "" && !hasString(fixStateLabels, l) {
			t.Errorf("state %q maps to %q, which is not a state label", s, l)
		}
	}
	w := newFVWorld(t, faOpts{})
	w.om.outcomeNext = "retry"
	check := func(when string) {
		t.Helper()
		if got := stateLabels(w.mr()); len(got) != 1 {
			t.Fatalf("%s: state labels %v, want exactly one", when, got)
		}
	}
	check("after the apply")
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	w.look()
	check("after not-fixed")
	// A person adds a second state label by hand; the next write that sets
	// a state removes every other one.
	if _, err := w.r.gl.Client().UpdateMergeRequest(context.Background(), w.r.pid, w.mr().IID,
		glab.MRUpdate{AddLabels: []string{labelFixFailed, labelFixAwaitingRun}}); err != nil {
		t.Fatal(err)
	}
	rec := w.rec()
	l, _ := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(&w.log))
	if err := updateFixMR(context.Background(), w.r.gl.Client(), l.me, rec, pipefix.StateFixed, "**Verified.**"); err != nil {
		t.Fatal(err)
	}
	if got := stateLabels(w.mr()); !reflect.DeepEqual(got, []string{labelFixVerified}) {
		t.Fatalf("state labels %v, want exactly [%s]", got, labelFixVerified)
	}
	// Every label-changing request in the log that ADDS a state label also
	// REMOVES one (the swap is atomic) -- except the hand edit above.
	for _, wr := range w.r.gl.Writes() {
		if wr.Method != "PUT" || !strings.Contains(wr.Path, "/merge_requests/") || !strings.Contains(wr.Body, "add_labels") {
			continue
		}
		if strings.Contains(wr.Body, labelFixAwaitingRun) && strings.Contains(wr.Body, labelFixFailed) && !strings.Contains(wr.Body, "remove_labels") {
			continue // the person's edit
		}
		adds := false
		for _, sl := range fixStateLabels {
			adds = adds || strings.Contains(wr.Body, `"add_labels":"`+sl) || strings.Contains(wr.Body, ","+sl+`"`)
		}
		if adds && !strings.Contains(wr.Body, "remove_labels") {
			t.Fatalf("a request added a state label without removing the old one: %s", faLine(wr))
		}
	}
	// Idempotent: saying it again sends nothing.
	before := len(w.r.gl.Writes())
	if err := updateFixMR(context.Background(), w.r.gl.Client(), l.me, rec, pipefix.StateFixed, "**Verified.**"); err != nil {
		t.Fatal(err)
	}
	if n := len(w.r.gl.Writes()) - before; n != 0 {
		t.Fatalf("%d write(s) for a merge request that already says it", n)
	}
}

// updateFixMR writes only to a merge request that is still this fix's, edits
// nothing but the status block, and refuses a description that would close
// something.
func TestUpdateFixMRTouchesOnlyItsOwnStatusBlock(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	rec := w.rec()
	l, _ := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(&w.log))
	c := w.r.gl.Client()
	ctx := context.Background()
	before := w.mr().Description
	if err := updateFixMR(ctx, c, l.me, rec, pipefix.StateFixed, "**Verified.** all good"); err != nil {
		t.Fatal(err)
	}
	after := w.mr().Description
	if statusBlock(t, after) != "**Verified.** all good" {
		t.Fatalf("status block:\n%s", statusBlock(t, after))
	}
	strip := func(d string) string {
		return d[:strings.Index(d, fixStatusOpen)] + d[strings.Index(d, fixStatusClose):]
	}
	if strip(before) != strip(after) {
		t.Fatal("the edit changed the description outside the status block")
	}
	t.Run("a description that lost its block gets it back at the top", func(t *testing.T) {
		got := setStatusBlock("Related to #1\n\nbody", "S")
		if got != fixStatusOpen+"\nS\n"+fixStatusClose+"\n\nRelated to #1\n\nbody" {
			t.Fatalf("%q", got)
		}
		if got := setStatusBlock("a\n"+fixStatusOpen+"\nold\n"+fixStatusClose+"\nz", "new"); got != "a\n"+fixStatusOpen+"\nnew\n"+fixStatusClose+"\nz" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("a closing pattern is refused", func(t *testing.T) {
		err := updateFixMR(ctx, c, l.me, rec, pipefix.StateFixed, "this closes #1 for good")
		if err == nil || !strings.Contains(err.Error(), "closing pattern") {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if strings.Contains(w.mr().Description, "closes #1") {
			t.Fatal("the closing text reached the merge request")
		}
	})
	t.Run("a merge request that is not the bot's is not edited", func(t *testing.T) {
		writes := len(w.r.gl.Writes())
		err := updateFixMR(ctx, c, glab.User{ID: 4242}, rec, pipefix.StateFlaky, "**Inconclusive: flaky.**")
		if err == nil || len(w.r.gl.Writes()) != writes {
			t.Fatalf("err = %v, writes +%d: a merge request another user authored was edited", err, len(w.r.gl.Writes())-writes)
		}
	})
	t.Run("a merged merge request is not edited", func(t *testing.T) {
		w.r.gl.SetMRState(w.r.pid, rec.PipelineFix.MRIID, "merged", time.Now())
		writes := len(w.r.gl.Writes())
		if err := updateFixMR(ctx, c, l.me, rec, pipefix.StateFlaky, "**Inconclusive: flaky.**"); err != nil || len(w.r.gl.Writes()) != writes {
			t.Fatalf("err = %v, writes +%d", err, len(w.r.gl.Writes())-writes)
		}
	})
}

// THE RUN NOTE'S FIX LINE (plan 5.4 step 4, which the apply left unwritten):
// the diagnosed run's note says which merge request and where it stands, the
// run's record carries both, and a later "still failing" re-render keeps it.
func TestTheRunNoteCarriesTheFixLine(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	w.look()
	runNote := func() string {
		for _, n := range w.landingNotes() {
			if len(pipelinectx.RunMarkerStates(n.Body, w.r.rec.BeadAnchor)) > 0 {
				return n.Body
			}
		}
		t.Fatal("no run note")
		return ""
	}
	mr := w.rec().PipelineFix.MRIID
	want := fmt.Sprintf("- Fix: merge request !%d -- gonk is waiting for its pipeline\n", mr)
	if body := runNote(); !strings.Contains(body, want) || strings.Count(body, "- Fix: merge request") != 1 {
		t.Fatalf("the run note lacks the fix line %q:\n%s", want, body)
	}
	run, _, _ := w.store.Get(context.Background(), w.r.rec.BeadAnchor)
	if run.PipelineRunFixMR != mr || run.PipelineRunFixState != "verifying" {
		t.Fatalf("run record fix line: !%d %q", run.PipelineRunFixMR, run.PipelineRunFixState)
	}
	// A look that changes nothing does not write the run again (run
	// maintenance, five minutes apart, is not due inside this one).
	w.look()
	if again, _, _ := w.store.Get(context.Background(), w.r.rec.BeadAnchor); again.Revision != run.Revision {
		t.Fatalf("the run record was rewritten by a look that changed nothing (%s -> %s)", run.Revision, again.Revision)
	}
	// Nor does writing the line it already carries: no store write, no
	// GitLab request (the post-merge pass says `merged` more than once).
	l, err := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(&w.log))
	if err != nil {
		t.Fatal(err)
	}
	reads := len(w.r.gl.Requests())
	if _, err := writeRunFixLine(context.Background(), w.deps(), l, run, mr, pipefix.StateVerifying); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := w.store.Get(context.Background(), w.r.rec.BeadAnchor); again.Revision != run.Revision || len(w.r.gl.Requests()) != reads {
		t.Fatalf("writing the fix line the run already carries wrote the store (%s -> %s) or read GitLab (%d request(s))",
			run.Revision, again.Revision, len(w.r.gl.Requests())-reads)
	}
	w.mrPipeline("success", fvPass("build"), fvPass("unit-tests"))
	w.look()
	want = fmt.Sprintf("- Fix: merge request !%d -- the diagnosed failure no longer occurs in its pipeline\n", mr)
	body := runNote()
	if !strings.Contains(body, want) || strings.Count(body, "- Fix: merge request") != 1 {
		t.Fatalf("the run note's fix line did not move to fixed:\n%s", body)
	}
	if st := pipelinectx.RunMarkerStates(body, w.r.rec.BeadAnchor); len(st) != 1 || st[0] != pipelinectx.RunStateDiagnosed {
		t.Fatalf("the fix line changed the run note's state: %v", st)
	}
	// Every state has words; none renders the raw fallback for a known state.
	for _, s := range pipefix.States() {
		if strings.Contains(fixLineText(1, string(s)), "state `") && s != pipefix.StateProposing && s != pipefix.StateNeedsScope {
			t.Errorf("fix state %q has no words on the run note", s)
		}
	}
}

// ---- the next attempt -----------------------------------------------------------

// dispatchAttempt runs gonk-dispatch for the newest fired fix order with the
// PRODUCTION preparer, on the world's runtime, as meter attempt n.
func (w *fvWorld) dispatchAttempt(n int) (code int, logs string, session string) {
	w.t.Helper()
	orders := w.orders.orders()
	vars := orders[len(orders)-1].Vars
	dec := pfRunDecision()
	dec.Attempt, dec.ReservationID = n, fmt.Sprintf("rsv-fix-%d", n)
	fm := &fakeMeter{resp: dec}
	var buf bytes.Buffer
	before := len(w.rt.Starts())
	code = runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(w.t)), Runtime: w.rt, Store: w.store,
		Forge: promptTestForge(), Pipelines: w.r.gl.Client(), BotUsername: "gonk", PackDir: w.pack,
		Args:  dispatchArgsFrom(func(k string) string { return vars[k] }),
		Clock: &fakeClock{now: w.clock.Add(time.Second)}, Log: testLogger(&buf),
		PrepareFix: pipefixPrepare,
		FixConfig:  func(context.Context, string) (*meterapi.ProjectResponse, error) { return w.cfg, w.cfgErr },
	})
	if starts := w.rt.Starts(); len(starts) > before {
		session = starts[len(starts)-1].Name
	}
	return code, buf.String(), session
}

// THE SECOND ATTEMPT ADOPTS THE MERGE REQUEST (the path the apply wrote and
// left untested): after not-fixed the fix is dispatched again, its session is
// told what the exit check found and shown the change that failed, and its
// answer becomes a SECOND COMMIT ON TOP of the first, on the SAME branch and
// the SAME merge request. Then that commit's pipeline is judged like any.
func TestTheSecondAttemptCommitsOnTopAndAdoptsTheMergeRequest(t *testing.T) {
	w := newFVWorld(t, faOpts{cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.MaxAttempts = 3 }})
	w.om.outcomeNext = "retry"
	first := w.rec().PipelineFix
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	w.look()

	code, logs, session := w.dispatchAttempt(2)
	if code != 0 || session == "" {
		t.Fatalf("the second attempt was not dispatched (exit %d)\n%s", code, logs)
	}
	if want := runtime.Name(pipefixAgent, w.r.pid, runtime.KindPipeline, 900, 2); session != want {
		t.Fatalf("second session %q, want %q", session, want)
	}
	starts := w.rt.Starts()
	prompt := string(starts[len(starts)-1].Prompt)
	for _, want := range []string{
		"== Previous attempt ==",
		"NOT_FIXED by rule J3.RECURS",
		"-\tfor _, x := range xs[1:] {", // the line the first attempt replaced
		"+\tfor _, x := range xs {",     // with this
		"attempt: 2",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the second attempt's prompt lacks %q:\n%s", want, prompt)
		}
	}
	rec := w.rec()
	if rec.Attempt != 2 || rec.PipelineFix.State != "" || rec.PipelineFix.FixCommitSHA != first.FixCommitSHA || rec.PipelineFix.MRIID != first.MRIID {
		t.Fatalf("record at the second dispatch: attempt %d fix state %q commit %q mr %d", rec.Attempt, rec.PipelineFix.State,
			rec.PipelineFix.FixCommitSHA, rec.PipelineFix.MRIID)
	}

	// The second answer: a different change to the same file (against the
	// BASE, as every attempt's edits are).
	second := faFix(faEdit(pfSource, pfOld, "range xs[0:] {"))
	h := runtime.Handle(session)
	w.rt.SetOutput(h, []byte(plSessionOutput(t, second)))
	w.rt.SetPhase(h, runtime.PhaseSucceeded, "")
	w.resetWrites()
	log := w.tick(time.Minute)
	rec = w.rec()
	pf := rec.PipelineFix
	if pf.State != string(pipefix.StateVerifying) || pf.AppliedAttempt != 2 || pf.FixCommitSHA == first.FixCommitSHA || pf.FixCommitSHA == "" {
		t.Fatalf("after the second apply: fix state %q applied attempt %d commit %q (first %q)\n%s", pf.State, pf.AppliedAttempt,
			pf.FixCommitSHA, first.FixCommitSHA, log)
	}
	// ONE merge request, the same one; ONE new commit whose parent is the
	// first attempt's commit; no second branch.
	mr := w.mr()
	if mr.IID != first.MRIID || pf.MRIID != first.MRIID {
		t.Fatalf("the second attempt is on merge request !%d, the first was !%d", pf.MRIID, first.MRIID)
	}
	c, err := w.r.gl.Client().GetCommit(context.Background(), w.r.pid, pf.FixCommitSHA)
	if err != nil || len(c.ParentIDs) != 1 || c.ParentIDs[0] != first.FixCommitSHA {
		t.Fatalf("the second commit's parents %v (err %v), want [%s]", c.ParentIDs, err, first.FixCommitSHA)
	}
	if !strings.Contains(c.Message, "attempt=2") {
		t.Fatalf("second commit message:\n%s", c.Message)
	}
	routes := faRoutes(w.written())
	for _, r := range routes {
		if strings.HasSuffix(r, "/repository/branches") || (strings.HasPrefix(r, "POST ") && strings.HasSuffix(r, "/merge_requests")) {
			t.Fatalf("the second attempt created a branch or a merge request: %v", routes)
		}
	}
	if n := strings.Count(strings.Join(routes, "\n"), "/repository/commits"); n != 1 {
		t.Fatalf("%d commits by the second attempt, want 1: %v", n, routes)
	}
	if raw, _ := w.r.gl.Client().GetRawFile(context.Background(), w.r.pid, pfSource, pf.FixCommitSHA, 1<<20); !strings.Contains(string(raw), "range xs[0:] {") {
		t.Fatalf("the second commit does not hold the second attempt's content:\n%s", raw)
	}
	if st, _, _ := w.fixNote(); st != "verifying" {
		t.Fatalf("fix note %q after the second apply, want verifying", st)
	}
	if got := stateLabels(mr); !reflect.DeepEqual(got, []string{labelFixVerifying}) {
		t.Fatalf("state labels %v", got)
	}

	// The old commit's pipeline no longer counts; the new commit's decides.
	// The exit check's first look at the new attempt is the very next tick
	// (the re-sling cleared the look's stamp), and it makes the merge
	// request's status block name the second commit.
	w.tick(30 * time.Second)
	if sb := statusBlock(t, w.mr().Description); !strings.Contains(sb, "**Verifying.**") || !strings.Contains(sb, shortSHA(pf.FixCommitSHA)) {
		t.Fatalf("the status block does not name the second commit:\n%s", sb)
	}
	if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateVerifying) || len(w.om.requests()) != 1 {
		t.Fatalf("the first commit's failed pipeline judged the second attempt: fix state %q outcomes %+v", rec.PipelineFix.State, w.om.requests())
	}
	w.om.outcomeNext = "done"
	w.mrPipeline("success", fvPass("build"), fvPass("unit-tests"))
	w.look()
	out := w.om.requests()
	rec = w.rec()
	if len(out) != 2 || out[1].Outcome != meterapi.OutcomeSuccess || out[1].Attempt != 2 || out[1].ReservationID != "rsv-fix-2" ||
		rec.PipelineFix.State != string(pipefix.StateFixed) || rec.PipelineFix.VerdictAttempt != 2 {
		t.Fatalf("after the second attempt's green pipeline: outcomes %+v fix state %q", out, rec.PipelineFix.State)
	}
	t.Logf("LEDGER attempt 1: %s (%s)  attempt 2: %s (%s)", out[0].Outcome, out[0].ReservationID, out[1].Outcome, out[1].ReservationID)
}

// BOUNDED BY max_attempts: the attempt that reaches it is still reported
// (gate-failed), and then nothing is re-slung -- the fix is `denied`, a
// person is asked, and the merge request says it failed.
func TestNotFixedAtMaxAttemptsEndsDenied(t *testing.T) {
	w := newFVWorld(t, faOpts{cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.MaxAttempts = 1 }})
	w.om.outcomeNext = "escalate"
	fired := len(w.orders.orders())
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	log := w.look()
	rec := w.rec()
	out := w.om.requests()
	if len(out) != 1 || out[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcomes %+v\n%s", out, log)
	}
	if rec.PipelineFix.State != string(pipefix.StateDenied) || rec.State != beadstore.StateNeedsHuman || !rec.ReslungAt.IsZero() ||
		rec.PipelineFix.Verdict != string(pipefix.VerdictNotFixed) || len(w.orders.orders()) != fired {
		t.Fatalf("at max_attempts: fix state %q record %q reslung %v verdict %q orders +%d", rec.PipelineFix.State, rec.State,
			rec.ReslungAt, rec.PipelineFix.Verdict, len(w.orders.orders())-fired)
	}
	if st, body, _ := w.fixNote(); st != "denied" || !strings.Contains(body, "pipeline_fix.max_attempts") {
		t.Fatalf("fix note %q:\n%s", st, body)
	}
	if got := stateLabels(w.mr()); !reflect.DeepEqual(got, []string{labelFixFailed}) {
		t.Fatalf("state labels %v, want failed", got)
	}
	if !hasString(w.landingLabels(), labelNeedsHuman) {
		t.Fatalf("landing labels %v lack needs-human", w.landingLabels())
	}
	if st, _, known := fixStateOf(rec, w.clock); st != pipefix.StateDenied || !known {
		t.Fatalf("the denied fix reads as %q", st)
	}
	w.tick(10 * time.Minute)
	if len(w.om.requests()) != 1 || len(w.orders.orders()) != fired {
		t.Fatal("a denied fix was reported or dispatched again")
	}
}

// A fix whose RUN has moved is not re-slung either: gonk-dispatch would
// refuse the order, and a re-sling it refuses would be re-fired for ever.
func TestNotFixedIsNotReslungAgainstARunThatMoved(t *testing.T) {
	w := newFVWorld(t, faOpts{cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.MaxAttempts = 3 }})
	w.setRun(func(r *beadstore.Record) { r.PipelineLatestFailed = 905 })
	fired := len(w.orders.orders())
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	w.look()
	rec := w.rec()
	if rec.PipelineFix.State != string(pipefix.StateDenied) || len(w.orders.orders()) != fired {
		t.Fatalf("fix state %q, orders +%d", rec.PipelineFix.State, len(w.orders.orders())-fired)
	}
	if _, body, _ := w.fixNote(); !strings.Contains(body, "newer failing pipeline") {
		t.Fatalf("the note does not say why no further attempt follows:\n%s", body)
	}
}

// When the attempt that was to follow is refused at DISPATCH (meter denies,
// or max_attempts by meter's count), the landing and the open merge request
// stop saying "trying again".
func TestADenyAfterNotFixedUpdatesTheNoteAndTheMergeRequest(t *testing.T) {
	w := newFVWorld(t, faOpts{cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.MaxAttempts = 2 }})
	w.om.outcomeNext = "escalate"
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	w.look()
	if st, _, _ := w.fixNote(); st != "not-fixed" {
		t.Fatalf("fix note %q", st)
	}
	// Meter counts an infrastructure failure in between: the next attempt
	// is number 3, past max_attempts.
	code, logs, session := w.dispatchAttempt(3)
	if code != 0 || session != "" {
		t.Fatalf("exit %d session %q\n%s", code, session, logs)
	}
	if st, body, _ := w.fixNote(); st != "denied" {
		t.Fatalf("fix note %q after the capped dispatch:\n%s\n%s", st, body, logs)
	}
	if got := stateLabels(w.mr()); !reflect.DeepEqual(got, []string{labelFixFailed}) {
		t.Fatalf("state labels %v, want failed", got)
	}
}

// ---- what is recorded so nothing later merges it ----------------------------------

func TestNoAutoMergeReasonsAreRecordedOnTheResult(t *testing.T) {
	t.Run("weakening not decidable (no predicate for the language)", func(t *testing.T) {
		w := newFVWorldWith(t, faOpts{python: true}, effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{
			{Kind: effects.KindEdit, Path: "calc/sum.py", Old: "    return 0\n", New: "    return sum(xs)\n"},
			faMeta(faRationale),
		}})
		// This fixture's repository holds no pipeline definition file; the
		// commit's merged definition is the base's.
		w.r.gl.SetCILint(w.r.pid, w.rec().PipelineFix.FixCommitSHA, glab.CILint{Valid: true, MergedYAML: plMergedYAML})
		if weak := w.rec().PipelineFix.Weakening; weak != beadstore.FixWeakeningNotDecidable {
			t.Fatalf("setup: weakening %q", weak)
		}
		w.mrPipeline("success", fvPass("build"), fvPass("unit-tests"))
		log := w.look()
		rec := w.rec()
		if rec.PipelineFix.State != string(pipefix.StateFixed) {
			t.Fatalf("fix state %q\n%s", rec.PipelineFix.State, log)
		}
		if !reflect.DeepEqual(rec.PipelineFix.NoAutoMerge, []string{noAutoMergeWeakening}) {
			t.Fatalf("NoAutoMerge %v, want [%s]: a fixed fix whose weakening is not decidable", rec.PipelineFix.NoAutoMerge, noAutoMergeWeakening)
		}
		if _, body, _ := w.fixNote(); !strings.Contains(body, "cannot tell whether this change weakens a test") {
			t.Fatalf("the note does not say so:\n%s", body)
		}
		if sb := statusBlock(t, w.mr().Description); !strings.Contains(sb, "never merges such a change on its own") {
			t.Fatalf("the status block does not say so:\n%s", sb)
		}
	})
	t.Run("the pure function", func(t *testing.T) {
		decided := beadstore.Record{PipelineFix: beadstore.PipelineFix{Weakening: beadstore.FixWeakeningDecided, CIDelta: "none"}}
		if got := noAutoMergeReasons(decided, fixSettlement{}, pipefix.StateFixed); len(got) != 0 {
			t.Fatalf("a decided, unchanged-definition, fixed fix has reasons %v", got)
		}
		exec := decided
		exec.PipelineFix.CIDelta = "exec-changing"
		if got := noAutoMergeReasons(exec, fixSettlement{}, pipefix.StateFixed); !reflect.DeepEqual(got, []string{noAutoMergeExec}) {
			t.Fatalf("exec-changing: %v", got)
		}
		judged := fixSettlement{res: pipefix.Result{Definition: "exec-changing"}}
		if got := noAutoMergeReasons(decided, judged, pipefix.StateFixed); !reflect.DeepEqual(got, []string{noAutoMergeExec}) {
			t.Fatalf("exec-changing as judged: %v", got)
		}
		unset := beadstore.Record{}
		if got := noAutoMergeReasons(unset, fixSettlement{}, pipefix.StateFlaky); !reflect.DeepEqual(got, []string{"verdict-flaky", noAutoMergeWeakening}) {
			t.Fatalf("unset weakening, flaky: %v", got)
		}
	})
}

// ---- it cannot merge ------------------------------------------------------------

// The exit check's forge has no call that merges, approves or pushes, and the
// record facts are filled from the record alone.
func TestTheExitCheckCannotMerge(t *testing.T) {
	typ := reflect.TypeOf((*pipefixVerifyForge)(nil)).Elem()
	writes := map[string]bool{
		"UpdateMergeRequest": true, "UpdateMRLabels": true, "CreateMRNote": true, "UpdateMRNote": true,
		"CreateIssue": true, "UpdateIssue": true, "CreateIssueNote": true, "UpdateIssueNote": true, "CloseIssue": true,
	}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		read := strings.HasPrefix(name, "Get") || strings.HasPrefix(name, "List") || name == "CurrentUser" || name == "CILintStatic"
		if !read && !writes[name] {
			t.Errorf("pipefixVerifyForge has %s: not a read, and not one of the exit check's writes", name)
		}
		for _, banned := range []string{"Accept", "Approve", "Commit", "Branch", "Retry", "Play", "Cancel", "Assign", "Review"} {
			if !read && strings.Contains(name, banned) {
				t.Errorf("pipefixVerifyForge has %s", name)
			}
		}
	}
	if _, has := typ.MethodByName("AcceptMR"); has {
		t.Fatal("pipefixVerifyForge can merge")
	}
}

func TestFixFactsOfCopiesTheRecord(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	rec := w.rec()
	f := fixFactsOf(rec)
	pf := rec.PipelineFix
	if !f.Recorded || f.MRIID != pf.MRIID || f.FixCommitSHA != pf.FixCommitSHA || f.FixBaseSHA != plSHA || f.BasePipelineID != 900 ||
		!f.VerifyDeadline.Equal(pf.VerifyDeadline) || f.CIExecChanges != "manual-run" || len(f.Lift) != 0 ||
		f.ExcerptVersion != pipelinectx.ExcerptVersion || f.SignatureVersion != pipelinectx.SignatureVersion {
		t.Fatalf("fix facts %+v", f)
	}
	if len(f.Targets) != 1 || f.Targets[0].Name() != "unit-tests" || f.Targets[0].Key.Stage != "test" ||
		f.Targets[0].Key.FailureReason != "script_failure" || f.Targets[0].Key.ErrorSignature != pf.Targets[0].ErrorSignature || f.Targets[0].Generic {
		t.Fatalf("targets %+v", f.Targets)
	}
	want := []pipefix.BaseJob{{Name: "build", Status: "success"}, {Name: "unit-tests", Status: "failed"}}
	got := append([]pipefix.BaseJob(nil), f.BaseJobs...)
	sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("base jobs %+v, want %+v", got, want)
	}
	// Not recorded: a record the apply never finished, and one that is not a fix.
	unapplied := rec
	unapplied.PipelineFix.AppliedAttempt = 0
	other := rec
	other.Trigger = "pipeline-diagnosis"
	if fixFactsOf(unapplied).Recorded || fixFactsOf(other).Recorded {
		t.Fatal("a record without an applied attempt, or of another trigger, reads as recorded")
	}
	// A target with no signature is generic, as TargetSet made it at dispatch.
	nosig := rec
	nosig.PipelineFix.Targets = []beadstore.PipelineFixTarget{{Name: "unit-tests", Stage: "test", FailureReason: "script_failure"}}
	if !fixFactsOf(nosig).Targets[0].Generic {
		t.Fatal("a target without a signature is not generic")
	}
	// The bound: the verify deadline plus the grace; the reservation's when
	// no verify deadline was written.
	if got := fixVerifyBound(rec); !got.Equal(pf.VerifyDeadline.Add(pipefixJudgeGrace)) {
		t.Fatalf("bound %s", got)
	}
	nodeadline := rec
	nodeadline.PipelineFix.VerifyDeadline = time.Time{}
	if got := fixVerifyBound(nodeadline); !got.Equal(runningDeadline(nodeadline).Add(pipefixJudgeGrace)) {
		t.Fatalf("bound without a verify deadline %s", got)
	}
}

// A running fix record in a state the exit check does not own is left alone.
func TestTheExitCheckLeavesStatesItDoesNotOwn(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	rec := w.rec()
	rec.PipelineFix.State = string(pipefix.StateFixed) // not a state a RUNNING record carries
	if err := w.store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	reads := len(w.r.gl.Requests())
	log := w.look()
	if len(w.om.requests()) != 0 || !strings.Contains(log, "does not own") {
		t.Fatalf("outcomes %+v\n%s", w.om.requests(), log)
	}
	// Nothing of the merge request was read for it (run maintenance's own
	// reads of the target are another pass's).
	for _, r := range w.r.gl.Requests()[reads:] {
		if strings.Contains(r, "/merge_requests/") {
			t.Fatalf("the exit check read %s for a record it does not own", r)
		}
	}
}

// A fix note that cannot be written holds the settlement like any other
// GitLab write: nothing is reported until it lands.
func TestAFailedFixNoteHoldsTheSettlement(t *testing.T) {
	w := newFVWorld(t, faOpts{})
	w.look() // the run note's fix line is written now, so only the fix note changes below
	var noteID int64
	for _, n := range w.landingNotes() {
		if len(pipelinectx.FixMarkerStates(n.Body, w.fixAnchor())) > 0 {
			noteID = n.ID
		}
	}
	if noteID == 0 {
		t.Fatal("no fix note")
	}
	w.mrPipeline("running", fvPass("build"))
	w.look()
	// The fix note, and only it, cannot be edited. (A waiting look does not
	// rewrite it; the verdict does.)
	w.r.gl.FailAlways("PUT", fmt.Sprintf("/api/v4/projects/%d/issues/1/notes/%d", w.r.pid, noteID), 500)
	rec := w.rec()
	v := &fixVerify{d: w.deps(), f: w.r.gl.Client(), rec: rec, readRevision: rec.Revision, now: w.clock}
	if v.show(context.Background(), rec, pipefix.StateFixed, "**fixed**", "**Verified.**", false) {
		t.Fatal("show reported success although the fix note could not be written")
	}
	w.r.gl.ClearFailures()
	if !v.show(context.Background(), rec, pipefix.StateFixed, "**fixed**", "**Verified.**", false) {
		t.Fatalf("show failed with GitLab back:\n%s", w.log.String())
	}
}

// staleFixStore loses every conditional write of one anchor while armed: the
// record "changed after this look read it".
type staleFixStore struct {
	beadstore.Store
	anchor string
	armed  bool
}

func (s *staleFixStore) PutIf(ctx context.Context, rec beadstore.Record, ifRevision string) (string, error) {
	if s.armed && rec.BeadAnchor == s.anchor {
		return "", fmt.Errorf("%w (test)", beadstore.ErrStale)
	}
	return s.Store.PutIf(ctx, rec, ifRevision)
}

// NO FIRE AFTER A LOST WRITE (gonk-p8w8): a not-fixed verdict whose re-sling
// record did not land fires no order -- another writer owns the record -- and
// the next look settles again.
func TestALostReslingWriteFiresNoOrder(t *testing.T) {
	w := newFVWorld(t, faOpts{cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.MaxAttempts = 3 }})
	w.om.outcomeNext = "retry"
	st := &staleFixStore{Store: w.store, anchor: w.fixAnchor(), armed: true}
	w.store = st
	fired := len(w.orders.orders())
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	log := w.look()
	if n := len(w.orders.orders()) - fired; n != 0 {
		t.Fatalf("%d order(s) fired although the re-sling record was not written\n%s", n, log)
	}
	if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateVerifying) || !rec.ReslungAt.IsZero() {
		t.Fatalf("the record changed: fix state %q reslung %v", rec.PipelineFix.State, rec.ReslungAt)
	}
	st.armed = false
	w.look()
	if rec := w.rec(); rec.PipelineFix.State != "" || rec.ReslungAt.IsZero() || len(w.orders.orders()) != fired+1 {
		t.Fatalf("after the write could land: fix state %q reslung %v orders +%d", rec.PipelineFix.State, rec.ReslungAt, len(w.orders.orders())-fired)
	}
}

// The previous attempt's change, as the next attempt is shown it.
func TestChangedLines(t *testing.T) {
	for _, c := range []struct {
		name, a, b     string
		removed, added []string
	}{
		{"one line in the middle", "a\nb\nc\n", "a\nB\nc\n", []string{"b"}, []string{"B"}},
		{"a line added", "a\nc\n", "a\nb\nc\n", nil, []string{"b"}},
		{"a line removed", "a\nb\nc\n", "a\nc\n", []string{"b"}, nil},
		{"the first line", "a\nb\n", "A\nb\n", []string{"a"}, []string{"A"}},
		{"the last line", "a\nb", "a\nB", []string{"b"}, []string{"B"}},
		{"no change", "a\nb\n", "a\nb\n", nil, nil},
		{"everything", "a\n", "b\n", []string{"a"}, []string{"b"}},
	} {
		removed, added := changedLines(c.a, c.b)
		if len(removed)+len(c.removed) != 0 && !reflect.DeepEqual(removed, c.removed) || len(added)+len(c.added) != 0 && !reflect.DeepEqual(added, c.added) {
			t.Errorf("%s: removed %q added %q, want %q / %q", c.name, removed, added, c.removed, c.added)
		}
	}
	// A file that cannot be read is named as unavailable, never guessed.
	w := newFVWorld(t, faOpts{})
	rec := w.rec()
	rec.PipelineFix.ChangedFiles = append(rec.PipelineFix.ChangedFiles, beadstore.PipelineFixFile{Path: "calc/gone.go"})
	got := previousFixChange(context.Background(), w.r.gl.Client(), rec, testLogger(&w.log))
	want := "--- calc/total.go\n-\tfor _, x := range xs[1:] {\n+\tfor _, x := range xs {\n--- calc/gone.go\n(unavailable)\n"
	if got != want {
		t.Fatalf("previous change:\n%q\nwant\n%q", got, want)
	}
}

// One state label per fix state (plan 8.1), every state named.
func TestFixStateLabelPerState(t *testing.T) {
	want := map[pipefix.State]string{
		pipefix.StateProposing: "", pipefix.StateNeedsScope: "",
		pipefix.StateAwaitingRun:     labelFixAwaitingRun,
		pipefix.StateVerifying:       labelFixVerifying,
		pipefix.StateNotFixed:        labelFixVerifying,
		pipefix.StateFixed:           labelFixVerified,
		pipefix.StateFixedNewFailure: labelFixVerified,
		pipefix.StateFlaky:           labelFixInconclusive,
		pipefix.StateUnverifiable:    labelFixInconclusive,
		pipefix.StateRegressed:       labelFixInconclusive,
		pipefix.StateTakenOver:       labelFixInconclusive,
		pipefix.StateClosedUnmerged:  labelFixInconclusive,
		pipefix.StateDenied:          labelFixFailed,
		pipefix.StateMerged:          "", pipefix.StateConfirmed: "", pipefix.StateRecurred: "",
	}
	for _, s := range pipefix.States() {
		got, known := want[s]
		if !known {
			t.Errorf("state %q is not in this test's table", s)
			continue
		}
		if l := fixStateLabel(s); l != got {
			t.Errorf("fixStateLabel(%s) = %q, want %q", s, l, got)
		}
	}
	// The state labels are a subset of the reserved fix labels, and the two
	// qualifiers are not state labels.
	for _, l := range fixStateLabels {
		if !hasString(fixMRLabels, l) {
			t.Errorf("state label %q is not a reserved fix label", l)
		}
	}
	if hasString(fixStateLabels, labelFixNewFailure) || hasString(fixStateLabels, labelCIExecChanged) {
		t.Error("a qualifier label is in the state label set")
	}
}
