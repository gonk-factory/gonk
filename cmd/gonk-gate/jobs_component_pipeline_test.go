//go:build component

package main

// gonk-tios P8, the COMPONENT half: the pipeline agent on a REAL cluster, with
// the REAL agent image and the REAL pipeline prompt, answered by the in-test
// stub model -- and its answer judged by the REAL sweep-side chain
// (applyPipelineBatch: D8 steps 1-9, then the landing) against a fake GitLab
// holding the failed pipeline. Same harness, same rules and same `make
// component-jobs` as jobs_component_test.go (read its header first): context
// k3d-*/kind-* only, never skipped, bounded polls, no sleeps.
//
// What only this can show: that the prompt gonk-gate renders for a pipeline,
// mounted into a real Job running the real image under the pipeline agent's
// env and permission set, reaches a model; that what the model says comes
// back out of the pod log through extractBatch intact; and that a GROUNDED
// answer -- a line the model could only have taken from the excerpt it was
// given -- is accepted and lands, while an answer quoting a line that is NOT
// in the excerpt is refused with nothing written.
//
// (c), the loop watchdog (gonk-un7z), is NOT here: it is not on this branch
// (fix/gonk-un7z-loop-watchdog is unmerged). When it merges, add the case the
// plan names: the watchdog's exit is classified for the pipeline agent exactly
// as for triage.
//
// Synthetic and neutral (plan section 0): plRun's fake GitLab -- one failing
// unit test in a made-up project.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// buildPipelineSpec is a pipeline session Spec built the way runBrokerDispatch
// builds one for the pipeline agent: the pack's [env] for `pipeline`, the named
// GONK_* variables, and the prompt fitPipelinePrompt renders from the fake
// GitLab's failed pipeline (the same context the record r.rec was prepared
// from, so its excerpt hashes are this prompt's excerpts). The checkout is the
// stub's, at the pipeline's commit.
func buildPipelineSpec(t *testing.T, e *componentEnv, r *plRun, sc sessionStub) sessionSpec {
	t.Helper()
	name := r.rec.SessionID
	if name != runtime.Name(pipelineAgent, r.pid, runtime.KindPipeline, 900, 1) {
		t.Fatalf("fixture: session %q is not the pipeline session name", name)
	}
	if err := runtime.ValidName(name); err != nil {
		t.Fatalf("invalid session name %q: %v", name, err)
	}
	packEnv, err := packAgentEnv(componentPackDir, pipelineAgent)
	if err != nil {
		t.Fatalf("packAgentEnv(pipeline): %v", err)
	}
	pc, err := buildPipelineContext(context.Background(), r.gl.Client(), pipelineContextRequest{
		ProjectID: r.pid, Project: r.rec.Project, PipelineID: r.rec.PipelineID,
		Target: r.target, LastSuccess: r.rec.PipelineLastSuccess,
	})
	if err != nil {
		t.Fatalf("buildPipelineContext: %v", err)
	}
	prompt, err := fitPipelinePrompt(pc, plSHA)
	if err != nil {
		t.Fatalf("fitPipelinePrompt: %v", err)
	}
	// The prompt's excerpts must be the record's: the sweep re-derives them
	// from the trace and compares against exactly these hashes.
	for i, j := range pc.JobRecords() {
		if r.rec.PipelineJobs[i].ExcerptSHA256 != j.ExcerptSHA256 {
			t.Fatalf("fixture: the prompt's excerpt of %s is not the recorded one", j.Name)
		}
	}
	if !strings.Contains(prompt, plGoodLine) {
		t.Fatalf("fixture: the rendered prompt does not carry the failing line %q", plGoodLine)
	}
	liteLLM := e.stub.register(name, sc)
	key := "sk-component-" + randHex(t, 12)
	md, err := json.Marshal(map[string]string{"gonk_project": r.rec.Project, "gonk_bead": r.rec.BeadAnchor})
	if err != nil {
		t.Fatal(err)
	}
	return sessionSpec{
		// The failing line is the prompt's own text: seeing it in a chat call
		// proves the mounted prompt reached the model.
		token:  plGoodLine,
		key:    key,
		rigURL: liteLLM + "/rig.tar.gz",
		spec: runtime.Spec{
			Name:  name,
			Agent: pipelineAgent,
			Env: buildSessionEnv(packEnv, sessionEnvInputs{
				Attempt: 1, Model: componentModel, LiteLLMURL: liteLLM, RigBaseURL: e.stub.base,
			}),
			RigURL:     liteLLM + "/rig.tar.gz",
			Prompt:     []byte(prompt),
			LiteLLMKey: []byte(key),
			Metadata:   md,
			Deadline:   time.Now().Add(20 * time.Minute),
		},
	}
}

// runPipelineSession starts the session, waits for it to finish Succeeded,
// checks the pod ran as the pipeline agent, and returns the batch gonk-gate's
// own extractBatch + ParseBatch read out of the pod log.
func runPipelineSession(t *testing.T, e *componentEnv, ss sessionSpec, wantBatch string) effects.Batch {
	t.Helper()
	ctx := context.Background()
	h, created, err := e.rt.Start(ctx, ss.spec)
	if err != nil || !created {
		t.Fatalf("Start: created=%v err=%v", created, err)
	}
	st := waitFinished(t, e, h)
	if st.Phase != runtime.PhaseSucceeded {
		t.Fatalf("session %s finished %s/%s, want Succeeded", h, st.Phase, st.Reason)
	}
	pods := sessionPods(t, e, ss.spec.Name)
	if len(pods) != 1 {
		t.Fatalf("session %s has %d pods, want 1", ss.spec.Name, len(pods))
	}
	// THE JOB RAN AS THE PIPELINE AGENT: D5's env names exactly, GONK_AGENT
	// pipeline, and the pack's own OPENCODE_PERMISSION (read-only: bash, edit,
	// webfetch ... denied) byte for byte.
	checkPodEnv(t, pods[0], ss)
	packEnv, err := packAgentEnv(componentPackDir, pipelineAgent)
	if err != nil {
		t.Fatal(err)
	}
	var perm string
	for _, c := range pods[0].Spec.Containers {
		for _, ev := range c.Env {
			if ev.Name == "OPENCODE_PERMISSION" {
				perm = ev.Value
			}
		}
	}
	if perm == "" || perm != packEnv["OPENCODE_PERMISSION"] {
		t.Fatalf("pod OPENCODE_PERMISSION = %q, want the pipeline pack's %q", perm, packEnv["OPENCODE_PERMISSION"])
	}
	if term := agentTerminated(pods[0]); term == nil || term.ExitCode != 0 {
		t.Errorf("agent container terminated state %+v, want exit 0", term)
	}
	checkStubSawTheMountedSession(t, e, ss)

	out, err := e.rt.Output(ctx, h)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	t.Logf("pod log (Output, %d bytes):\n%s", len(out), out)
	if n := strings.Count(string(out), batchStartSentinel); n != 1 {
		t.Errorf("the pod log holds %d %s sentinels, want exactly 1 (the model's): the prompt is echoed", n, batchStartSentinel)
	}
	if strings.Contains(string(out), "<<<UNTRUSTED") {
		t.Errorf("the pod log contains the prompt's untrusted fences: the prompt is echoed into Output")
	}
	raw, ok := extractBatch(string(out))
	if !ok {
		t.Fatalf("extractBatch found no batch in the pipeline session's Output")
	}
	if string(raw) != wantBatch {
		t.Fatalf("extractBatch returned\n  %s\nwant the stub model's batch\n  %s", raw, wantBatch)
	}
	b, err := effects.ParseBatch(raw)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	return b
}

func batchJSON(t *testing.T, b effects.Batch) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// (a) A GROUNDED diagnosis from the real agent image is ACCEPTED by the real
// chain and lands: the run note in state diagnosed, carrying the quoted line,
// and the class label.
func TestComponentJobsPipelineAgentGroundedDiagnosisIsAccepted(t *testing.T) {
	e := newComponentEnv(t)
	r := newPLRun(t, plOpts{})
	want := batchJSON(t, plDiagnosis(nil)) // quotes plGoodLine, which is in the excerpt
	ss := buildPipelineSpec(t, e, r, sessionStub{scenario: scenarioAnswer, answer: answerText(want)})
	b := runPipelineSession(t, e, ss, want)

	var logs strings.Builder
	rec := r.rec
	applied, violation, err := applyPipelineBatch(context.Background(), r.deps(t, &logs), &rec, b)
	t.Logf("sweep chain: applied=%v violation=%q err=%v\n%s", applied, violation, err, logs.String())
	if !applied || violation != "" || err != nil {
		t.Fatalf("the real chain did not accept a grounded diagnosis from the real agent image: applied=%v violation=%q err=%v", applied, violation, err)
	}
	// gonk-2usw FACTS: the record now carries what a pipeline-fix is judged
	// against -- the job's failure facts (from dispatch) and the diagnosis the
	// real agent image produced (from this apply). Printed whole: it is the
	// evidence the plan's verifier asks for.
	recJSON, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RECORD after the apply:\n%s", recJSON)
	emitted := b.Effects[0]
	if d := rec.PipelineDiagnosis; d.Class != string(emitted.Classification) || d.Summary != emitted.Summary || d.Remedy != emitted.Remedy ||
		!equalSet(d.Files, emitted.Files) || !equalSet(d.EvidenceJobs, []string{"unit-tests"}) {
		t.Fatalf("the record's diagnosis is not the one the agent emitted and the chain accepted: %+v", d)
	}
	if j := rec.PipelineJobs[0]; j.Name != "unit-tests" || j.Stage != "test" || j.FailureReason != "script_failure" || j.AllowFailure ||
		j.ErrorSignature != pipelinectx.ErrorSignature(plTestFirstError) || j.GenericFailure() ||
		j.SignatureVersion != pipelinectx.SignatureVersion {
		t.Fatalf("the record's job failure facts: %+v", j)
	}
	notes := r.gl.IssueNotesOf(r.pid, 1)
	if len(notes) != 1 {
		t.Fatalf("run notes on the tracking issue = %d, want 1", len(notes))
	}
	if st := pipelinectx.RunMarkerStates(notes[0].Body, r.rec.BeadAnchor); len(st) != 1 || st[0] != pipelinectx.RunStateDiagnosed {
		t.Fatalf("run note marker states = %v, want [diagnosed]:\n%s", st, notes[0].Body)
	}
	if !strings.Contains(notes[0].Body, plGoodLine) {
		t.Fatalf("the landed note does not quote the grounded line:\n%s", notes[0].Body)
	}
	if is := r.gl.IssuesOf(r.pid); len(is) != 1 || !equalSet(is[0].Labels, []string{labelPipelineFailing, "gonk::pipeline-code"}) {
		t.Fatalf("tracking issue labels = %+v", is)
	}
	t.Logf("LANDED run note:\n%s", notes[0].Body)
}

// (b) The same session, but the model quotes a line that is NOT in the
// excerpt (it never appeared in the job's log): the real chain REFUSES it at
// D8 step 6, gate-failed, and writes nothing to GitLab.
func TestComponentJobsPipelineAgentUngroundedLineIsRefused(t *testing.T) {
	e := newComponentEnv(t)
	r := newPLRun(t, plOpts{})
	const invented = "AssertionError: sum([1, 2]) returned 5, expected 4"
	if strings.Contains(plTestLog, invented) {
		t.Fatal("fixture: the invented line must not be in the log")
	}
	want := batchJSON(t, plDiagnosis(func(e *effects.Effect) { e.Evidence[0].Line = invented }))
	ss := buildPipelineSpec(t, e, r, sessionStub{scenario: scenarioAnswer, answer: answerText(want)})
	b := runPipelineSession(t, e, ss, want)

	var logs strings.Builder
	rec := r.rec
	applied, violation, err := applyPipelineBatch(context.Background(), r.deps(t, &logs), &rec, b)
	t.Logf("sweep chain: applied=%v violation=%q err=%v\n%s", applied, violation, err, logs.String())
	if applied || err != nil || !strings.HasPrefix(violation, causeUngrounded+":") {
		t.Fatalf("applied=%v violation=%q err=%v, want refused gate-failed with cause %s", applied, violation, err, causeUngrounded)
	}
	if rec.PipelineDiagnosis.Recorded() {
		t.Fatalf("a refused diagnosis is on the record: %+v", rec.PipelineDiagnosis)
	}
	if w := writes(r.gl); len(w) != 0 {
		t.Fatalf("a refused diagnosis wrote to GitLab: %v", w)
	}
	if !strings.Contains(logs.String(), "cause="+causeUngrounded) {
		t.Fatalf("the refusal's cause is not logged:\n%s", logs.String())
	}
}
