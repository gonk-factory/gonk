//go:build component

package main

// gonk-2usw part APPLY, the COMPONENT half: a pipefix session that really ran
// -- a Job of the real agent image on a real cluster, answered by the stub
// model -- judged by the REAL SWEEP, whose apply writes to the fake GitLab the
// component harness uses (pkg/glab/glabtest).
//
// What only this can show, beyond the in-process tests of
// broker_pipefix_apply_test.go: the sweep reads the batch out of a real pod's
// log through the real Jobs runtime (Status, Output), applies it, and Stops
// the real Job -- with the prompt and the recorded FixContext being the ones
// gonk-dispatch's production preparer built.
//
//	accepted   branch + ONE commit + ONE merge request in the fake GitLab,
//	           the fix note moved, the record `verifying`, no outcome
//	           reported, the Job gone
//	refused    a test-weakening Go edit: SC-WEAKEN, gate-failed, and the fake
//	           GitLab's write log is EMPTY
//
// Same harness and rules as jobs_component_test.go: context k3d-*/kind-* only,
// never skipped, bounded polls. Synthetic fixture (plan section 0).

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// faComponentSession turns the fix session gonk-dispatch prepared in w (on the
// in-process runtime, so the record and its FixContext are the dispatch's
// own) into a Spec for the REAL runtime: the same name, the same prompt.
func faComponentSession(t *testing.T, e *componentEnv, w *faWorld, sc sessionStub) pfSession {
	t.Helper()
	started := w.rt.Starts()[0]
	prompt := string(started.Prompt)
	if !strings.Contains(prompt, plGoodLine) {
		t.Fatalf("fixture: the dispatched prompt does not carry %q", plGoodLine)
	}
	packEnv, err := packAgentEnv(componentPackDir, pipefixAgent)
	if err != nil {
		t.Fatalf("packAgentEnv(pipefix): %v", err)
	}
	liteLLM := e.stub.register(started.Name, sc)
	key := "sk-component-" + randHex(t, 12)
	md, err := json.Marshal(map[string]string{"gonk_project": "example/calc", "gonk_bead": "gonk-2usw-apply-component"})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := w.fixRecord()
	return pfSession{context: rec.PipelineFix.Context, ss: sessionSpec{
		token:  plGoodLine,
		key:    key,
		rigURL: liteLLM + "/rig.tar.gz",
		spec: runtime.Spec{
			Name:  started.Name,
			Agent: pipefixAgent,
			Env: buildSessionEnv(packEnv, sessionEnvInputs{
				Attempt: 1, Model: componentModel, LiteLLMURL: liteLLM, RigBaseURL: e.stub.base,
			}),
			RigURL:     liteLLM + "/rig.tar.gz",
			Prompt:     started.Prompt,
			LiteLLMKey: []byte(key),
			Metadata:   md,
			Deadline:   time.Now().Add(20 * time.Minute),
		},
	}}
}

// faComponentSweep runs ONE real sweep tick over w's store with the REAL
// runtime, and returns the outcomes it reported and its log.
func faComponentSweep(t *testing.T, e *componentEnv, w *faWorld, next string) ([]meterapi.OutcomeRequest, string) {
	t.Helper()
	om := &fakeOutcomeMeter{outcomeNext: next, spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 1200, AsOf: time.Now().Add(time.Hour)}}
	var log bytes.Buffer
	d := w.deps()
	d.Meter, d.Runtime, d.GL, d.BotUsername = meterClient(om.server(t)), e.rt, w.r.gl.Client(), "gonk"
	d.Log = testLogger(&log)
	d.PackDir = componentPackDir
	d.Now = func() time.Time { return time.Now().Add(time.Minute) }
	d.SpendPollInterval, d.SpendDeadline = time.Millisecond, time.Second
	d.FixesLive = false
	if code := runSweep(context.Background(), d); code != 0 {
		t.Fatalf("sweep exit %d\n%s", code, log.String())
	}
	return om.requests(), log.String()
}

// AN ACCEPTED BATCH FROM A REAL SESSION BECOMES A BRANCH, ONE COMMIT AND ONE
// MERGE REQUEST in the fake GitLab.
func TestComponentJobsPipefixAcceptedBatchOpensAMergeRequest(t *testing.T) {
	// This test's end state is "merge request open, unjudged": it runs the
	// sweep as a build without the exit check. What the exit check then does
	// with a real session's merge request is jobs_component_pipefix_verify_test.go.
	judge := pipefixJudge
	pipefixJudge = nil
	t.Cleanup(func() { pipefixJudge = judge })
	e := newComponentEnv(t)
	w := newFAWorld(t, faOpts{})
	want := batchJSON(t, faGood())
	ps := faComponentSession(t, e, w, sessionStub{scenario: scenarioAnswer, answer: answerText(want)})
	b := runPipefixSession(t, e, ps, want)
	if b.Verdict != effects.VerdictFix {
		t.Fatalf("the session's batch: %+v", b)
	}

	outcomes, log := faComponentSweep(t, e, w, "done")
	ws := w.written()
	for _, l := range faLines(ws) {
		t.Log("WRITE " + l)
	}
	routes := faRoutes(ws)
	if len(routes) != 4 || !strings.HasSuffix(routes[0], "/repository/branches") || !strings.HasSuffix(routes[1], "/repository/commits") ||
		!strings.HasSuffix(routes[2], "/merge_requests") || !strings.HasSuffix(routes[3], "/issues/1/notes/N") {
		t.Fatalf("the sweep's writes to the fake GitLab: %v\n%s", routes, log)
	}
	mrs := w.fixMRs()
	if len(mrs) != 1 {
		t.Fatalf("%d merge request(s) on %s\n%s", len(mrs), w.branch(), log)
	}
	rec, _ := w.fixRecord()
	f := rec.PipelineFix
	head, err := w.r.gl.Client().ResolveRef(context.Background(), w.r.pid, w.branch())
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != beadstore.StateRunning || f.State != string(pipefix.StateVerifying) || f.MRIID != mrs[0].IID || f.FixCommitSHA != head ||
		f.Weakening != beadstore.FixWeakeningDecided || f.CIDelta != "none" {
		t.Fatalf("fix record: state %q fix %+v (branch head %s)\n%s", rec.State, f, head, log)
	}
	if raw, _ := w.r.gl.Client().GetRawFile(context.Background(), w.r.pid, pfSource, head, 1<<20); string(raw) != strings.Replace(pfSourceBody, pfOld, pfNew, 1) {
		t.Fatalf("the committed file is not the patched content:\n%s", raw)
	}
	if len(outcomes) != 0 {
		t.Fatalf("an outcome was reported for a fix nothing has judged: %+v", outcomes)
	}
	if st, _, _ := w.fixNote(); st != "verifying" {
		t.Fatalf("fix note %q", st)
	}
	// The real Job is gone: the sweep stopped the session it judged.
	gone := false
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline) && !gone; {
		st, serr := e.rt.Status(context.Background(), runtime.Handle(ps.ss.spec.Name))
		gone = serr == nil && st.Phase == runtime.PhaseNotFound
		if !gone {
			<-time.After(500 * time.Millisecond)
		}
	}
	if !gone {
		t.Fatalf("the judged session's Job still exists 90s after the sweep stopped it")
	}
	t.Logf("APPLIED: branch=%s commit=%s mr=!%d title=%q labels=%v state=%s", w.branch(), head, mrs[0].IID, mrs[0].Title, mrs[0].Labels, f.State)
	t.Logf("MR DESCRIPTION:\n%s", mrs[0].Description)
}

// A TEST-WEAKENING BATCH FROM A REAL SESSION IS REFUSED BY SC-WEAKEN, reported
// gate-failed, and writes NOTHING to the fake GitLab.
func TestComponentJobsPipefixWeakeningBatchIsRefusedAndWritesNothing(t *testing.T) {
	e := newComponentEnv(t)
	w := newFAWorld(t, faOpts{class: pipefix.ClassTest})
	weak := faFix(faEdit(pfTest, "\tif got := Total", "\tt.Skip(\"until the function is fixed\")\n\tif got := Total"))
	want := batchJSON(t, weak)
	ps := faComponentSession(t, e, w, sessionStub{scenario: scenarioAnswer, answer: answerText(want)})
	runPipefixSession(t, e, ps, want)

	outcomes, log := faComponentSweep(t, e, w, "retry")
	if ws := w.written(); len(ws) != 0 {
		t.Fatalf("a refused batch wrote to the fake GitLab:\n  %s", strings.Join(faLines(ws), "\n  "))
	}
	t.Logf("WRITE LOG of the refused batch: %d entr(ies)", len(w.written()))
	if len(outcomes) != 1 || outcomes[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcomes %+v\n%s", outcomes, log)
	}
	rec, _ := w.fixRecord()
	if rec.PipelineFix.Refusal != "SC-WEAKEN(added-skip)" || rec.PipelineFix.State != "" || len(w.fixMRs()) != 0 {
		t.Fatalf("refusal %q state %q, %d MR(s)\n%s", rec.PipelineFix.Refusal, rec.PipelineFix.State, len(w.fixMRs()), log)
	}
	t.Logf("REFUSED: %s, outcome %s", rec.PipelineFix.Refusal, outcomes[0].Outcome)
}
