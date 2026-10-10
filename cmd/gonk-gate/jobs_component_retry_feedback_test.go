//go:build component

package main

// gonk-nf9i, the COMPONENT half: the retry feedback reaches a REAL session.
//
// One stub "model" serves both attempts of one fix. It misspells the fix_meta
// key -- the mistake 5 of 8 live sessions made -- UNLESS the prompt it is sent
// carries gonk's refused-answer block, in which case it spells the key
// correctly. Nothing else tells the two attempts apart:
//
//	attempt 1   a real Job; the prompt has no block; the model misspells the
//	            key; the real sweep refuses the batch (parse), reports
//	            gate-failed, records the feedback, fires the re-sling and
//	            writes NOTHING to the fake GitLab
//	attempt 2   gonk-dispatch's production preparer builds the prompt from the
//	            record; a SECOND real Job runs it; the model -- because the
//	            block is in the request real opencode sent it -- answers
//	            correctly; the real sweep applies it: one merge request
//
// What only this can show, beyond retry_feedback_test.go: the block survives
// the real delivery path (Secret mount, entrypoint, real opencode) into the
// request the model receives, on the attempt it is meant for and not before.
//
// Same harness and rules as jobs_component_test.go: context k3d-*/kind-* only,
// never skipped, bounded polls. Synthetic fixture (plan section 0).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// chatBodiesWith counts the session's chat requests whose body holds text.
func chatBodiesWith(e *componentEnv, session, text string) (with, total int) {
	for _, r := range e.stub.requests(session) {
		if r.Path != "/v1/chat/completions" {
			continue
		}
		total++
		// The body is JSON (and the prompt may be encoded more than once on
		// its way through opencode): compare with every backslash removed.
		if strings.Contains(strings.ReplaceAll(r.Body, `\`, ""), text) {
			with++
		}
	}
	return with, total
}

func TestComponentJobsPipefixRefusedAnswerIsCorrectedWhenTheFeedbackReachesTheSession(t *testing.T) {
	e := newComponentEnv(t)
	w := newFVComponentWorld(t, e, faOpts{})
	w.om.outcomeNext = "retry"

	good := batchJSON(t, faGood())
	bad := strings.Replace(good, `"reason"`, `"reasn"`, 1)
	if bad == good {
		t.Fatal("fixture: the good batch has no reason key to misspell")
	}
	if _, err := effects.ParseBatch([]byte(bad)); err == nil {
		t.Fatal("fixture: the misspelled batch parses")
	}
	// THE ONE MODEL of both attempts.
	model := sessionStub{scenario: scenarioAnswer, answer: answerText(bad),
		ifPrompt: retryFeedbackHeading, answerThen: answerText(good)}

	// ---- attempt 1: no block in the prompt; the model misspells the key ----
	started1 := w.rt.Starts()[0]
	if strings.Contains(string(started1.Prompt), retryFeedbackHeading) {
		t.Fatal("the FIRST attempt's prompt carries a refused-answer block")
	}
	ps1 := componentFixSession(t, e, started1, 1, model)
	h1, created, err := e.rt.Start(context.Background(), ps1.ss.spec)
	if err != nil || !created {
		t.Fatalf("Start: created=%v err=%v", created, err)
	}
	if st := waitFinished(t, e, h1); st.Phase != runtime.PhaseSucceeded {
		t.Fatalf("session %s finished %s/%s, want Succeeded", h1, st.Phase, st.Reason)
	}
	out1, err := e.rt.Output(context.Background(), h1)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	raw1, ok := extractBatch(string(out1))
	if !ok || string(raw1) != bad {
		t.Fatalf("the first session's batch is\n  %s\nwant the misspelled one\n  %s", raw1, bad)
	}
	if with, total := chatBodiesWith(e, ps1.ss.spec.Name, retryFeedbackHeading); with != 0 || total == 0 {
		t.Fatalf("the first session's model saw the block in %d of %d chat requests, want 0 of at least 1", with, total)
	}
	log := w.tick(0)
	rec := w.rec()
	outcomes := w.om.requests()
	if len(outcomes) != 1 || outcomes[0].Outcome != meterapi.OutcomeGateFailed || outcomes[0].Attempt != 1 {
		t.Fatalf("after the refused answer: outcomes %+v\n%s", outcomes, log)
	}
	want := beadstore.RetryFeedback{Attempt: 1, Code: "parse",
		What:    `effect 2 of 2 (fix_meta) has a key that does not exist: "reasn"`,
		Allowed: "the keys of an effect of kind fix_meta are: kind, reason"}
	if rec.RetryFeedback != want {
		t.Fatalf("recorded feedback\n got  %+v\n want %+v\n%s", rec.RetryFeedback, want, log)
	}
	if ws := w.written(); len(ws) != 0 || len(w.fixMRs()) != 0 {
		t.Fatalf("a refused answer wrote to the fake GitLab: %v", faLines(ws))
	}
	if rec.ReslungAt.IsZero() || len(w.orders.orders()) < 2 {
		t.Fatalf("no re-sling was fired for the refused answer (%d orders)\n%s", len(w.orders.orders()), log)
	}
	requireJobGone(t, e, ps1.ss.spec.Name)
	t.Logf("ATTEMPT 1 REFUSED: outcome=%s feedback=%+v", outcomes[0].Outcome, rec.RetryFeedback)

	// ---- attempt 2: the production preparer; a SECOND real Job; the same model ----
	code, dlog, session := w.dispatchAttempt(2)
	if code != 0 || session == "" {
		t.Fatalf("the second attempt was not dispatched (exit %d)\n%s", code, dlog)
	}
	starts := w.rt.Starts()
	started2 := starts[len(starts)-1]
	block := renderRetryFeedback(want, 2)
	if !strings.HasSuffix(string(started2.Prompt), "\n"+block) || strings.Count(string(started2.Prompt), retryFeedbackHeading) != 1 {
		t.Fatalf("the second session's prompt does not end with the refused-answer block:\n%s", started2.Prompt)
	}
	t.Logf("PROMPT BYTES: attempt 1 %d, attempt 2 %d; the block:\n%s", len(started1.Prompt), len(started2.Prompt), block)
	e2 := newComponentEnv(t)
	w.realRT = e2.rt
	ps2 := componentFixSession(t, e2, started2, 2, model)
	if ps2.ss.spec.Name == ps1.ss.spec.Name {
		t.Fatalf("the second session reuses the first's name %q", ps2.ss.spec.Name)
	}
	// runPipefixSession requires the batch read out of the pod log to be the
	// CORRECT one: the model gives it only when the block is in its request.
	b := runPipefixSession(t, e2, ps2, good)
	if b.Verdict != effects.VerdictFix {
		t.Fatalf("the second session's batch: %+v", b)
	}
	with, total := chatBodiesWith(e2, ps2.ss.spec.Name, retryFeedbackHeading)
	named, _ := chatBodiesWith(e2, ps2.ss.spec.Name, `what was wrong: effect 2 of 2 (fix_meta) has a key that does not exist: "reasn"`)
	if with == 0 || named == 0 {
		t.Fatalf("the second session's model saw the block in %d of %d chat requests, and the misspelled key named in %d", with, total, named)
	}
	t.Logf("ATTEMPT 2: the model saw the block in %d of %d chat requests (the key named in %d) and answered with the correct batch", with, total, named)

	w.om.outcomeNext = "done"
	log = w.tick(time.Minute)
	rec = w.rec()
	pf := rec.PipelineFix
	if pf.State != string(pipefix.StateVerifying) || pf.AppliedAttempt != 2 || pf.MRIID == 0 || len(w.fixMRs()) != 1 {
		t.Fatalf("after the corrected answer: fix state %q applied attempt %d mr %d, %d merge request(s)\n%s",
			pf.State, pf.AppliedAttempt, pf.MRIID, len(w.fixMRs()), log)
	}
	if rec.RetryFeedback != (beadstore.RetryFeedback{}) {
		t.Errorf("an applied answer left the feedback of the earlier refusal: %+v", rec.RetryFeedback)
	}
	if out := w.om.requests(); len(out) != 1 {
		t.Errorf("outcomes after the apply: %+v, want only attempt 1's gate-failed (a fix nothing has judged reports none)", out)
	}
	requireJobGone(t, e2, ps2.ss.spec.Name)
	t.Logf("APPLIED on attempt 2: mr=!%d fix_state=%s", pf.MRIID, pf.State)
}
