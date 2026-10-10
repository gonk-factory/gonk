//go:build component

package main

// gonk-2usw part AGENT, the COMPONENT half: the pipefix agent on a REAL
// cluster, with the REAL agent image and the REAL pipefix prompt, answered by
// the in-test stub model -- and its answer judged by the REAL gate
// (the pipefix effect shape, verifyPipefixContext, effects.PlanFix) against a
// fake GitLab holding the failed pipeline and the repository. Same harness,
// same rules and same `make component-jobs` as jobs_component_test.go (read
// its header first): context k3d-*/kind-* only, never skipped, bounded polls,
// no sleeps.
//
// What only this can show:
//
//   - a pipefix session STARTS AS A JOB of the real image, under the pipefix
//     pack's env, and the pod's OPENCODE_PERMISSION is the pipeline agent's
//     byte for byte;
//   - it HAS NO WRITE TOOLS: the tool list real opencode offers the model
//     under that permission map -- read off the chat-completion requests the
//     stub received, not off the configuration -- holds no shell, no edit,
//     no write, no patch and no fetch tool, and does hold `read`;
//   - the prompt gonk-gate renders reaches the model, and the batch the model
//     emits comes back out of the pod log through extractBatch intact;
//   - the fixture's fix is ACCEPTED by the gate over blobs verified to be the
//     ones the session was shown, and a CHEATING batch -- the failing test
//     weakened instead of the code corrected -- is REFUSED by rule code.
//
// "Judged" in THIS file is the gate of pkg/effects over the recorded context:
// nothing is committed and no merge request is opened by it. The whole apply
// on a real session is jobs_component_pipefix_apply_test.go.
//
// Synthetic and invented (plan section 0): newPFScenario's made-up project.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/runtime"
	"github.com/gonk-factory/gonk/pkg/runtime/jobs"
)

// pfPromptToken is a string only the fixture's prompt holds (the failing log
// line): seeing it in a chat call proves the mounted prompt reached the model.
const pfPromptToken = "total_test.go:7: Total([1 2]) = 2, want 3"

// pfSession is one prepared pipefix session: its Spec and what the controller
// recorded about the context it was shown.
type pfSession struct {
	ss      sessionSpec
	context beadstore.FixContext
}

// buildPipefixSpec is a pipefix session Spec built the way a dispatch will
// build one: the pack's [env] for `pipefix`, the named GONK_* variables, and
// the prompt fitPipefixPrompt renders from the fake GitLab (the same context
// whose Facts are recorded). The checkout is the stub's, at the base commit.
func buildPipefixSpec(t *testing.T, e *componentEnv, s *pfScenario, sc sessionStub) pfSession {
	t.Helper()
	name := runtime.Name(pipefixAgent, s.pid, runtime.KindPipeline, 900, 1)
	if err := runtime.ValidName(name); err != nil {
		t.Fatalf("invalid session name %q: %v", name, err)
	}
	packEnv, err := packAgentEnv(componentPackDir, pipefixAgent)
	if err != nil {
		t.Fatalf("packAgentEnv(pipefix): %v", err)
	}
	pc, err := buildPipefixContext(context.Background(), s.gl.Client(), s.request(t), pfMaxEdits(t))
	if err != nil {
		t.Fatalf("buildPipefixContext: %v", err)
	}
	prompt, err := fitPipefixPrompt(pc, plSHA)
	if err != nil {
		t.Fatalf("fitPipefixPrompt: %v", err)
	}
	if !strings.Contains(prompt, pfPromptToken) {
		t.Fatalf("fixture: the rendered prompt does not carry %q", pfPromptToken)
	}
	fc := pc.Facts(prompt)
	liteLLM := e.stub.register(name, sc)
	key := "sk-component-" + randHex(t, 12)
	md, err := json.Marshal(map[string]string{"gonk_project": s.rec.Project, "gonk_bead": "gonk-2usw-agent-component"})
	if err != nil {
		t.Fatal(err)
	}
	return pfSession{context: fc, ss: sessionSpec{
		token:  pfPromptToken,
		key:    key,
		rigURL: liteLLM + "/rig.tar.gz",
		spec: runtime.Spec{
			Name:  name,
			Agent: pipefixAgent,
			Env: buildSessionEnv(packEnv, sessionEnvInputs{
				Attempt: 1, Model: componentModel, LiteLLMURL: liteLLM, RigBaseURL: e.stub.base,
			}),
			RigURL:     liteLLM + "/rig.tar.gz",
			Prompt:     []byte(prompt),
			LiteLLMKey: []byte(key),
			Metadata:   md,
			Deadline:   time.Now().Add(20 * time.Minute),
		},
	}}
}

// offeredTools is every tool name the chat-completion requests of one session
// offered the model, sorted and distinct: what real opencode exposes under
// the pod's permission map.
func offeredTools(t *testing.T, e *componentEnv, session string) []string {
	t.Helper()
	seen := map[string]bool{}
	withTools := 0
	for _, r := range e.stub.requests(session) {
		if r.Path != "/v1/chat/completions" {
			continue
		}
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
			t.Fatalf("a chat request body is not JSON: %v", err)
		}
		if len(body.Tools) > 0 {
			withTools++
		}
		for _, tl := range body.Tools {
			seen[tl.Function.Name] = true
		}
	}
	if withTools == 0 {
		t.Fatalf("no chat request of session %s offered any tool: the tool list was not observed, so nothing can be said about it", session)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// runPipefixSession starts the session, checks it is a Job, waits for it to
// finish Succeeded, checks the pod ran as the pipefix agent with no write
// tool offered, and returns the batch gonk-gate's own extractBatch +
// ParseBatch read out of the pod log.
func runPipefixSession(t *testing.T, e *componentEnv, ps pfSession, wantBatch string) effects.Batch {
	t.Helper()
	ctx := context.Background()
	ss := ps.ss
	h, created, err := e.rt.Start(ctx, ss.spec)
	if err != nil || !created {
		t.Fatalf("Start: created=%v err=%v", created, err)
	}
	// IT STARTS AS A JOB.
	job, err := e.cs.BatchV1().Jobs(e.ns).Get(ctx, ss.spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("after Start, Job %s/%s: %v", e.ns, ss.spec.Name, err)
	}
	if job.UID == "" || job.Labels[jobs.ComponentLabel] != jobs.ComponentValue || job.Labels[jobs.SessionLabel] != ss.spec.Name {
		t.Errorf("Job %s: uid=%q labels=%v", ss.spec.Name, job.UID, job.Labels)
	}
	t.Logf("pipefix session is Job %s/%s uid=%s", e.ns, job.Name, job.UID)

	st := waitFinished(t, e, h)
	if st.Phase != runtime.PhaseSucceeded {
		t.Fatalf("session %s finished %s/%s, want Succeeded", h, st.Phase, st.Reason)
	}
	pods := sessionPods(t, e, ss.spec.Name)
	if len(pods) != 1 {
		t.Fatalf("session %s has %d pods, want 1", ss.spec.Name, len(pods))
	}
	// THE JOB RAN AS THE PIPEFIX AGENT, on the PIPELINE agent's permission
	// map byte for byte (read-only).
	checkPodEnv(t, pods[0], ss)
	pipelineEnv, err := packAgentEnv(componentPackDir, pipelineAgent)
	if err != nil {
		t.Fatal(err)
	}
	var perm, agent string
	for _, c := range pods[0].Spec.Containers {
		for _, ev := range c.Env {
			switch ev.Name {
			case "OPENCODE_PERMISSION":
				perm = ev.Value
			case "GONK_AGENT":
				agent = ev.Value
			}
		}
	}
	if agent != pipefixAgent {
		t.Errorf("pod GONK_AGENT = %q, want %q", agent, pipefixAgent)
	}
	if perm == "" || perm != pipelineEnv["OPENCODE_PERMISSION"] {
		t.Fatalf("pod OPENCODE_PERMISSION = %q, want the pipeline pack's %q", perm, pipelineEnv["OPENCODE_PERMISSION"])
	}
	t.Logf("pod OPENCODE_PERMISSION (== the pipeline agent's): %s", perm)
	if term := agentTerminated(pods[0]); term == nil || term.ExitCode != 0 {
		t.Errorf("agent container terminated state %+v, want exit 0", term)
	}
	checkStubSawTheMountedSession(t, e, ss)

	// IT HAS NO WRITE TOOLS: what real opencode offered the model.
	tools := offeredTools(t, e, ss.spec.Name)
	t.Logf("tools real opencode offered the model in this session: %v", tools)
	offered := map[string]bool{}
	for _, n := range tools {
		offered[strings.ToLower(n)] = true
	}
	for _, n := range []string{"bash", "edit", "write", "patch", "apply_patch", "multiedit", "webfetch", "websearch", "task", "question"} {
		if offered[n] {
			t.Errorf("the pipefix session was offered the %q tool: it must not be able to run or write anything", n)
		}
	}
	if !offered["read"] {
		t.Errorf("the pipefix session was not offered `read` (%v): it could not read the files it proposes edits to", tools)
	}

	out, err := e.rt.Output(ctx, h)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	t.Logf("pod log (Output, %d bytes):\n%s", len(out), out)
	if n := strings.Count(string(out), batchStartSentinel); n != 1 {
		t.Errorf("the pod log holds %d %s sentinels, want exactly 1 (the model's): the prompt's two skeletons are echoed", n, batchStartSentinel)
	}
	if strings.Contains(string(out), "<<<UNTRUSTED") {
		t.Errorf("the pod log contains the prompt's untrusted fences: the prompt is echoed into Output")
	}
	raw, ok := extractBatch(string(out))
	if !ok {
		t.Fatalf("extractBatch found no batch in the pipefix session's Output")
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

// pipefixGate is the gate as far as it exists (plan 5.3 A1, A4-A6, A9): the
// pipefix effect shape, the recorded context re-derived from a fresh read of
// the fake GitLab, and effects.PlanFix over the blobs that proved to be the
// ones shown.
func pipefixGate(t *testing.T, s *pfScenario, fc beadstore.FixContext, b effects.Batch) (effects.FixPlan, error) {
	t.Helper()
	shape, err := effects.LoadShape(componentPackDir, pipefixAgent)
	if err != nil {
		t.Fatalf("LoadShape: %v", err)
	}
	if err := effects.Validate(b, shape); err != nil {
		return effects.FixPlan{}, err
	}
	blobs, err := verifyPipefixContext(context.Background(), s.gl.Client(), s.pid, fc, s.rec.PipelineDiagnosis, s.env, pfMaxEdits(t))
	if err != nil {
		t.Fatalf("verifyPipefixContext (ours, never the agent's): %v", err)
	}
	return effects.PlanFix(b, s.env, blobs, nil)
}

// (a) The fixture's fix, emitted by the real agent image, is ACCEPTED.
func TestComponentJobsPipefixAgentFixIsAcceptedByTheGate(t *testing.T) {
	e := newComponentEnv(t)
	s := newPFScenario(t)
	want := batchJSON(t, pfFix())
	ps := buildPipefixSpec(t, e, s, sessionStub{scenario: scenarioAnswer, answer: answerText(want)})
	t.Logf("PROMPT (%d bytes) mounted into the session:\n%s", len(ps.ss.spec.Prompt), ps.ss.spec.Prompt)
	b := runPipefixSession(t, e, ps, want)

	plan, err := pipefixGate(t, s, ps.context, b)
	if err != nil {
		t.Fatalf("the gate refused the fixture's fix from the real agent image: %v", err)
	}
	if plan.Verdict != effects.VerdictFix || len(plan.Files) != 1 || plan.Files[0].Path != pfSource ||
		string(plan.Files[0].Content) != strings.Replace(pfSourceBody, pfOld, pfNew, 1) {
		t.Fatalf("plan = %+v, want the one corrected file", plan)
	}
	if plan.Facts.FilesChanged != 1 || plan.Facts.LinesAdded != 1 || plan.Facts.LinesRemoved != 1 {
		t.Errorf("diff facts = %+v, want one file, +1 -1", plan.Facts)
	}
	fcJSON, err := json.MarshalIndent(ps.context, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RECORDED FixContext:\n%s", fcJSON)
	t.Logf("ACCEPTED: verdict=%s file=%s +%d -%d rationale=%q\n--- content the apply would commit ---\n%s",
		plan.Verdict, plan.Files[0].Path, plan.Facts.LinesAdded, plan.Facts.LinesRemoved, plan.Reason, plan.Files[0].Content)
	if w := writes(s.gl); len(w) != 0 {
		t.Fatalf("judging a fix wrote to GitLab: %v", w)
	}
}

// (b) The same session, but the model CHEATS: it weakens the failing test so
// the job would pass, instead of correcting the code. The batch is well
// formed and its edit applies cleanly -- and the gate REFUSES it, by rule
// code, because the diagnosis classified the failure `code` and a code fix
// may not touch the test that is failing (plan 4.2).
func TestComponentJobsPipefixAgentCheatingBatchIsRefused(t *testing.T) {
	e := newComponentEnv(t)
	s := newPFScenario(t)
	cheat := effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{
		{Kind: effects.KindEdit, Path: pfTest, Old: "got != 3", New: "got != 2"},
		{Kind: effects.KindFixMeta, Reason: "The test now expects the value the function returns."},
	}}
	want := batchJSON(t, cheat)
	ps := buildPipefixSpec(t, e, s, sessionStub{scenario: scenarioAnswer, answer: answerText(want)})
	b := runPipefixSession(t, e, ps, want)

	// The cheat is not a malformed edit: against the blob alone it applies.
	if _, err := effects.ApplyEdits(b, effects.BaseBlobs{pfTest: []byte(pfTestBody)}); err != nil {
		t.Fatalf("fixture: the cheating edit does not even apply (%v); the refusal below would prove nothing", err)
	}
	plan, err := pipefixGate(t, s, ps.context, b)
	if err == nil {
		t.Fatalf("the gate ACCEPTED a batch that weakens the failing test: %+v", plan)
	}
	ref := effects.AsRefusal(err)
	if ref == nil || ref.Rule != effects.RuleClass || ref.Sub != "test" || ref.Path != pfTest {
		t.Fatalf("refusal = %v, want SC-CLASS(test) on %s", err, pfTest)
	}
	if len(plan.Files) != 0 || plan.Verdict != "" {
		t.Fatalf("a refused batch yielded something to apply: %+v", plan)
	}
	t.Logf("REFUSED: %s -- %v", ref.Code(), err)
	if w := writes(s.gl); len(w) != 0 {
		t.Fatalf("a refused fix wrote to GitLab: %v", w)
	}
}
