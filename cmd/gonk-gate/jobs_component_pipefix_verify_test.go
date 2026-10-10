//go:build component

package main

// gonk-2usw part VERIFY, the COMPONENT half: fix sessions that really ran --
// Jobs of the real agent image on a real cluster, answered by the stub model
// -- applied AND THEN JUDGED by the real sweep, with the fix merge request's
// pipeline played by the fake GitLab (pkg/glab/glabtest).
//
//	green      the (fake) merge request pipeline goes green: the fix ends
//	           `fixed` with ONE success outcome, the merge request says
//	           verified, nothing merges, the Job is gone
//	recurs     the pipeline fails the way it was diagnosed: gate-failed and
//	           a re-sling; the SECOND session -- another real Job, whose
//	           prompt carries the exit check's feedback -- is applied on top
//	           of the first commit, on the SAME merge request; its pipeline
//	           recurs too: gate-failed again, and at max_attempts (2) the fix
//	           ends `denied` (plan 7.1: not-fixed, then denied), with no
//	           further order
//
// What only this can show, beyond sweep_pipefix_verify_test.go: the batch the
// exit check's attempt came from was read out of a real pod's log through the
// real Jobs runtime; the second attempt's session is a second real Job under
// the attempt-suffixed name; and the sweep Stops each Job it judged.
//
// Same harness and rules as jobs_component_test.go: context k3d-*/kind-* only,
// never skipped, bounded polls. Synthetic fixture (plan section 0).

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// newFVComponentWorld is an fvWorld whose sweep judges REAL sessions: the fix
// session was dispatched in-process (so the record and its FixContext are the
// dispatch's own), and nothing has answered it yet.
func newFVComponentWorld(t *testing.T, e *componentEnv, o faOpts) *fvWorld {
	t.Helper()
	w := &fvWorld{faWorld: newFAWorld(t, o), next: 2000, clock: time.Now().Add(time.Minute), realRT: e.rt, realPack: componentPackDir}
	w.om = &fakeOutcomeMeter{outcomeNext: "done", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 1200, AsOf: time.Now().Add(240 * time.Hour)}}
	w.r.gl.SetCILintFromFile(w.r.pid, faCI)
	return w
}

// componentFixSession turns a fix session gonk-dispatch started on the
// in-process runtime into a Spec for the REAL one: the same name, the same
// prompt, the attempt's env.
func componentFixSession(t *testing.T, e *componentEnv, started runtime.Spec, attempt int, sc sessionStub) pfSession {
	t.Helper()
	if !strings.Contains(string(started.Prompt), plGoodLine) {
		t.Fatalf("fixture: the dispatched prompt does not carry %q", plGoodLine)
	}
	packEnv, err := packAgentEnv(componentPackDir, pipefixAgent)
	if err != nil {
		t.Fatalf("packAgentEnv(pipefix): %v", err)
	}
	liteLLM := e.stub.register(started.Name, sc)
	key := "sk-component-" + randHex(t, 12)
	md, err := json.Marshal(map[string]string{"gonk_project": "example/calc", "gonk_bead": "gonk-2usw-verify-component"})
	if err != nil {
		t.Fatal(err)
	}
	return pfSession{ss: sessionSpec{
		token:  plGoodLine,
		key:    key,
		rigURL: liteLLM + "/rig.tar.gz",
		spec: runtime.Spec{
			Name:  started.Name,
			Agent: pipefixAgent,
			Env: buildSessionEnv(packEnv, sessionEnvInputs{
				Attempt: attempt, Model: componentModel, LiteLLMURL: liteLLM, RigBaseURL: e.stub.base,
			}),
			RigURL:     liteLLM + "/rig.tar.gz",
			Prompt:     started.Prompt,
			LiteLLMKey: []byte(key),
			Metadata:   md,
			Deadline:   time.Now().Add(20 * time.Minute),
		},
	}}
}

// requireJobGone waits for the session's Job to be gone: the sweep stopped
// the session it judged.
func requireJobGone(t *testing.T, e *componentEnv, name string) {
	t.Helper()
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		if st, err := e.rt.Status(context.Background(), runtime.Handle(name)); err == nil && st.Phase == runtime.PhaseNotFound {
			return
		}
		<-time.After(500 * time.Millisecond)
	}
	t.Fatalf("the judged session's Job %s still exists 90s after the sweep stopped it", name)
}

// A FIX WHOSE MERGE REQUEST PIPELINE GOES GREEN ENDS `fixed` WITH ONE SUCCESS.
func TestComponentJobsPipefixGreenPipelineEndsFixedWithOneSuccess(t *testing.T) {
	e := newComponentEnv(t)
	w := newFVComponentWorld(t, e, faOpts{})
	want := batchJSON(t, faGood())
	ps := componentFixSession(t, e, w.rt.Starts()[0], 1, sessionStub{scenario: scenarioAnswer, answer: answerText(want)})
	runPipefixSession(t, e, ps, want)

	// The sweep judges the real session: branch, commit, merge request.
	log := w.tick(0)
	rec := w.rec()
	if rec.PipelineFix.State != string(pipefix.StateVerifying) || len(w.om.requests()) != 0 || len(w.fixMRs()) != 1 {
		t.Fatalf("after the apply: fix state %q, %d outcome(s), %d merge request(s)\n%s", rec.PipelineFix.State, len(w.om.requests()), len(w.fixMRs()), log)
	}
	requireJobGone(t, e, ps.ss.spec.Name)

	// The (fake) merge request pipeline: still running, then green.
	id := w.mrPipeline("running", fvPass("build"))
	log = w.look()
	if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateVerifying) || len(w.om.requests()) != 0 {
		t.Fatalf("a running pipeline: fix state %q outcomes %+v\n%s", rec.PipelineFix.State, w.om.requests(), log)
	}
	w.r.gl.SetPipelineStatus(w.r.pid, id, "success")
	w.r.gl.AddJob(w.r.pid, id, glabJob(id*100+9, "unit-tests", "test", "success", ""))
	log = w.look()
	rec = w.rec()
	out := w.om.requests()
	if rec.PipelineFix.State != string(pipefix.StateFixed) || rec.State != beadstore.StateDone || rec.PipelineFix.VerdictRule != "J5.FIXED" ||
		len(out) != 1 || out[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("after the green pipeline: fix state %q record %q rule %q outcomes %+v\n%s", rec.PipelineFix.State, rec.State, rec.PipelineFix.VerdictRule, out, log)
	}
	mr := w.mr()
	if got := stateLabels(mr); !reflect.DeepEqual(got, []string{labelFixVerified}) || mr.State != "opened" {
		t.Fatalf("merge request: state labels %v, state %q", got, mr.State)
	}
	if st, _, _ := w.fixNote(); st != "fixed" {
		t.Fatalf("fix note %q", st)
	}
	requireNoMergeCall(t, w.r.gl.Writes())
	// More ticks: one outcome, still.
	w.look()
	w.look()
	if n := len(w.om.requests()); n != 1 {
		t.Fatalf("%d outcomes after two more looks", n)
	}
	t.Logf("FIXED: mr=!%d labels=%v fix_state=%s verdict=%s rule=%s outcome=%s (attempt %d, reservation %s)", mr.IID, mr.Labels,
		rec.PipelineFix.State, rec.PipelineFix.Verdict, rec.PipelineFix.VerdictRule, out[0].Outcome, out[0].Attempt, out[0].ReservationID)
	for _, l := range faLines(w.r.gl.Writes()[w.mark:]) {
		t.Log("WRITE " + l)
	}
}

// A FIX WHOSE PIPELINE RECURS IS RE-SLUNG ONCE, AND AT max_attempts ENDS
// `denied`: two real sessions, two commits, ONE merge request, two
// gate-failed outcomes, no third order.
func TestComponentJobsPipefixRecurringPipelineIsReslungOnceThenDenied(t *testing.T) {
	e := newComponentEnv(t)
	w := newFVComponentWorld(t, e, faOpts{cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.MaxAttempts = 2 }})
	w.om.outcomeNext = "retry"

	// ---- attempt 1: a real session, applied; its pipeline recurs ----
	want1 := batchJSON(t, faGood())
	ps1 := componentFixSession(t, e, w.rt.Starts()[0], 1, sessionStub{scenario: scenarioAnswer, answer: answerText(want1)})
	runPipefixSession(t, e, ps1, want1)
	log := w.tick(0)
	first := w.rec().PipelineFix
	if first.State != string(pipefix.StateVerifying) || first.FixCommitSHA == "" {
		t.Fatalf("after the first apply: %+v\n%s", first, log)
	}
	requireJobGone(t, e, ps1.ss.spec.Name)
	fired := len(w.orders.orders())
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	log = w.look()
	rec := w.rec()
	out := w.om.requests()
	if len(out) != 1 || out[0].Outcome != meterapi.OutcomeGateFailed || rec.PipelineFix.State != "" || rec.ReslungAt.IsZero() ||
		len(w.orders.orders()) != fired+1 {
		t.Fatalf("after the first recurrence: outcomes %+v fix state %q reslung %v orders +%d\n%s", out, rec.PipelineFix.State,
			rec.ReslungAt, len(w.orders.orders())-fired, log)
	}
	if st, _, _ := w.fixNote(); st != "not-fixed" {
		t.Fatalf("fix note %q, want not-fixed (trying again)", st)
	}

	// ---- attempt 2: gonk-dispatch prepares it (the production preparer);
	// a SECOND real Job runs it ----
	code, dlog, session := w.dispatchAttempt(2)
	if code != 0 || session == "" {
		t.Fatalf("the second attempt was not dispatched (exit %d)\n%s", code, dlog)
	}
	starts := w.rt.Starts()
	started := starts[len(starts)-1]
	if !strings.Contains(string(started.Prompt), "== Previous attempt ==") || !strings.Contains(string(started.Prompt), "NOT_FIXED by rule J3.RECURS") {
		t.Fatalf("the second session's prompt carries no feedback:\n%s", started.Prompt)
	}
	second := faFix(faEdit(pfSource, pfOld, "range xs[0:] {"))
	want2 := batchJSON(t, second)
	// Its own namespace and stub: the harness checks that a stub saw ONLY
	// its session's requests, and the first session's are on e's. The sweep
	// reads the second session from the runtime it ran in.
	e2 := newComponentEnv(t)
	w.realRT = e2.rt
	ps2 := componentFixSession(t, e2, started, 2, sessionStub{scenario: scenarioAnswer, answer: answerText(want2)})
	if ps2.ss.spec.Name == ps1.ss.spec.Name {
		t.Fatalf("the second session reuses the first's name %q", ps2.ss.spec.Name)
	}
	runPipefixSession(t, e2, ps2, want2)
	log = w.tick(time.Minute)
	rec = w.rec()
	pf := rec.PipelineFix
	if pf.State != string(pipefix.StateVerifying) || pf.AppliedAttempt != 2 || pf.FixCommitSHA == first.FixCommitSHA || pf.MRIID != first.MRIID || len(w.fixMRs()) != 1 {
		t.Fatalf("after the second apply: fix state %q applied %d commit %q (first %q) mr %d (first %d), %d merge request(s)\n%s",
			pf.State, pf.AppliedAttempt, pf.FixCommitSHA, first.FixCommitSHA, pf.MRIID, first.MRIID, len(w.fixMRs()), log)
	}
	c, err := w.r.gl.Client().GetCommit(context.Background(), w.r.pid, pf.FixCommitSHA)
	if err != nil || len(c.ParentIDs) != 1 || c.ParentIDs[0] != first.FixCommitSHA {
		t.Fatalf("the second commit's parents %v (%v), want the first commit", c.ParentIDs, err)
	}
	requireJobGone(t, e2, ps2.ss.spec.Name)

	// ---- its pipeline recurs too: gate-failed, and at max_attempts, denied ----
	fired = len(w.orders.orders())
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	log = w.look()
	rec = w.rec()
	out = w.om.requests()
	if len(out) != 2 || out[1].Outcome != meterapi.OutcomeGateFailed || out[1].Attempt != 2 {
		t.Fatalf("after the second recurrence: outcomes %+v\n%s", out, log)
	}
	if rec.PipelineFix.State != string(pipefix.StateDenied) || rec.PipelineFix.Verdict != string(pipefix.VerdictNotFixed) ||
		rec.State != beadstore.StateNeedsHuman || len(w.orders.orders()) != fired {
		t.Fatalf("at max_attempts: fix state %q verdict %q record %q orders +%d\n%s", rec.PipelineFix.State, rec.PipelineFix.Verdict,
			rec.State, len(w.orders.orders())-fired, log)
	}
	mr := w.mr()
	if got := stateLabels(mr); !reflect.DeepEqual(got, []string{labelFixFailed}) || mr.State != "opened" {
		t.Fatalf("merge request: state labels %v, state %q", got, mr.State)
	}
	if st, _, _ := w.fixNote(); st != "denied" {
		t.Fatalf("fix note %q", st)
	}
	requireNoMergeCall(t, w.r.gl.Writes())
	w.tick(10 * time.Minute)
	if len(w.om.requests()) != 2 || len(w.orders.orders()) != fired {
		t.Fatal("a denied fix was reported or dispatched again")
	}
	t.Logf("DENIED after 2 attempts: mr=!%d labels=%v commits=%s <- %s fix_state=%s last_verdict=%s", mr.IID, mr.Labels,
		pf.FixCommitSHA, first.FixCommitSHA, rec.PipelineFix.State, rec.PipelineFix.Verdict)
	for _, o := range out {
		t.Logf("LEDGER attempt=%d rung=%s reservation=%s outcome=%s", o.Attempt, o.Rung, o.ReservationID, o.Outcome)
	}
}
