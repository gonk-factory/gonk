//go:build component

package main

// THE JOBS RUNTIME, ON A REAL CLUSTER, WITH THE REAL AGENT IMAGE (gonk-t55
// plan docs/plans/2026-09-25-t55-session-runtime.md, part P6 / "component").
//
// Every unit test of pkg/runtime/jobs runs against client-go's fake
// clientset, and every entrypoint test runs the script under a fake PATH.
// Neither can say whether the two agree: whether a Job the runtime builds
// actually schedules, mounts its Secret readably for uid 65532, runs the
// image's own entrypoint in jobs mode, reaches a model with the mounted key,
// and leaves a pod log that gonk-gate's own extractBatch reads the batch out
// of. This file is where that is observed.
//
// WHY IT LIVES IN cmd/gonk-gate AND NOT pkg/runtime/jobs (the plan said the
// latter). The assertion that matters most is that gonk-gate's OWN fence
// parser, extractBatch, reads the batch out of a real pod log -- and
// extractBatch is unexported in package main. Living here, the test also
// builds its Spec with dispatch's own pieces (packAgentEnv over the real
// pack, buildSessionEnv, renderTriagePrompt, runtime.Name) and checks the
// DeadlineExceeded reason against the sweep's own reasonDeadlineExceeded,
// rather than against copies of them. The runtime under test is still the
// real one: jobs.New over a real clientset.
//
// WHY THE STUB RUNS IN THIS PROCESS. The model endpoint and the checkout
// listener are served from the test binary (jobs_component_stub_test.go) on an
// address the cluster's pods can reach (for k3d under podman: the network's
// gateway, e.g. 192.0.2.1). That makes "the stub saw no prompt fetch" a
// direct observation instead of a log scrape, and needs no second image.
//
// HOW TO RUN IT: `make component-jobs` (see README-component-jobs.md next to
// this file). It needs three settings and refuses to run without them --
// it does NOT skip, because a skipped component test reads as green:
//
//	GONK_COMPONENT_KUBE_CONTEXT  kubectl context; must start with k3d- or kind-
//	GONK_COMPONENT_AGENT_IMAGE   agent image ref already present on the node
//	GONK_COMPONENT_STUB_LISTEN   host:port the stub listens on AND pods dial
//
// Each test creates its own namespace (gonk-t55c-<random>) with a gc-agent
// ServiceAccount, and deletes it at the end.
//
// TIME (docs/testing-time.md). Nothing here sleeps: every wait is a bounded
// poll on the state it is waiting for, and each bound is a safety net whose
// failure names what was last observed. No assertion is about how long
// anything took.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gonk-factory/gonk/internal/keydomain"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/runtime"
	"github.com/gonk-factory/gonk/pkg/runtime/jobs"
)

// The test's own settings. These are TEST env, read by nothing in production.
const (
	componentKubeContextEnv = "GONK_COMPONENT_KUBE_CONTEXT"
	componentAgentImageEnv  = "GONK_COMPONENT_AGENT_IMAGE"
	// componentSkewImageEnv names an agent image VARIANT whose supported
	// session contracts exclude runtime.SessionContract (built by `make
	// component-jobs-image`: the agent image with its
	// GONK_SUPPORTED_SESSION_CONTRACTS line rewritten to "999"). Only the
	// contract-mismatch test reads it.
	componentSkewImageEnv  = "GONK_COMPONENT_SKEW_IMAGE"
	componentStubListenEnv = "GONK_COMPONENT_STUB_LISTEN"
	componentKubectlEnv    = "GONK_COMPONENT_KUBECTL" // optional, default kubectl
)

const (
	componentModel   = "component-stub-model"
	componentPackDir = "../../pack"
	componentProject = "t55c/component"
	componentProjID  = int64(4242)
	componentIID     = int64(17)

	// Safety nets (docs/testing-time.md rule 5): generous bounds that stop a
	// hung run, never the assertion. A session on this box goes from Start
	// to Succeeded in well under a minute.
	finishBound = 5 * time.Minute
	goneBound   = 3 * time.Minute
	pollEvery   = time.Second
)

// forbiddenPodEnv is D5's list (gonk-426v): Gas City plumbing a Jobs pod must
// carry none of. Written out here, not borrowed from pkg/runtime/jobs, so a
// narrowing of the runtime's own refusal cannot also narrow this check.
var forbiddenPodEnv = regexp.MustCompile(`^(GC_DOLT|BEADS_|GC_INSTANCE|GC_CITY_WRITE)`)

// componentEnv is one test's cluster, namespace, runtime and stub.
type componentEnv struct {
	cs    kubernetes.Interface
	ns    string
	image string
	rt    runtime.SessionRuntime
	stub  *componentStub
}

func mustEnv(t *testing.T, name, what string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Fatalf("%s is not set (%s). This is a component test: it needs a real cluster and refuses to "+
			"skip, because a skipped component test reads as green. Run it with `make component-jobs` -- "+
			"see cmd/gonk-gate/README-component-jobs.md", name, what)
	}
	return v
}

// kubeConfigFor builds a rest.Config for exactly one named kubectl context,
// from `kubectl config view --raw --minify --context <ctx>`. The context is
// always explicit: the default context on this box has been the live dev
// cluster before, and a test that follows the default is one `kubectl config
// use-context` away from creating Jobs there.
func kubeConfigFor(t *testing.T, kubeContext string) *rest.Config {
	t.Helper()
	if !strings.HasPrefix(kubeContext, "k3d-") && !strings.HasPrefix(kubeContext, "kind-") {
		t.Fatalf("REFUSING context %q: this test creates Jobs and namespaces, and runs only against a "+
			"disposable k3d-* or kind-* cluster", kubeContext)
	}
	kubectl := os.Getenv(componentKubectlEnv)
	if kubectl == "" {
		kubectl = "kubectl"
	}
	out, err := exec.Command(kubectl, "config", "view", "--raw", "--minify", "--context", kubeContext, "-o", "json").Output()
	if err != nil {
		t.Fatalf("kubectl config view --context %s: %v", kubeContext, err)
	}
	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CAData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				CertData string `json:"client-certificate-data"`
				KeyData  string `json:"client-key-data"`
				Token    string `json:"token"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := json.Unmarshal(out, &kc); err != nil {
		t.Fatalf("parse kubeconfig for %s: %v", kubeContext, err)
	}
	if len(kc.Clusters) != 1 || len(kc.Users) != 1 || kc.Clusters[0].Cluster.Server == "" {
		t.Fatalf("kubeconfig for %s: want exactly one cluster with a server and one user, got %d/%d", kubeContext, len(kc.Clusters), len(kc.Users))
	}
	dec := func(what, s string) []byte {
		if s == "" {
			return nil
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("kubeconfig for %s: %s is not base64: %v", kubeContext, what, err)
		}
		return b
	}
	c, u := kc.Clusters[0].Cluster, kc.Users[0].User
	cfg := &rest.Config{
		Host:        c.Server,
		BearerToken: u.Token,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   dec("certificate-authority-data", c.CAData),
			CertData: dec("client-certificate-data", u.CertData),
			KeyData:  dec("client-key-data", u.KeyData),
		},
		Timeout: 60 * time.Second,
	}
	t.Logf("cluster: context=%s server=%s", kubeContext, c.Server)
	return cfg
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// newComponentEnv connects, creates a fresh namespace with the gc-agent
// ServiceAccount the runtime's pods run as, builds the REAL jobs runtime over
// it, and starts the stub. The namespace is deleted when the test ends.
func newComponentEnv(t *testing.T) *componentEnv {
	t.Helper()
	return newComponentEnvWithImage(t, mustEnv(t, componentAgentImageEnv, "the agent image built from images/Dockerfile.agent and imported into the cluster"))
}

// newComponentEnvWithImage is newComponentEnv with the runtime's configured
// agent image chosen by the caller.
func newComponentEnvWithImage(t *testing.T, image string) *componentEnv {
	t.Helper()
	kubeContext := mustEnv(t, componentKubeContextEnv, "the kubectl context of a disposable k3d/kind cluster")
	listen := mustEnv(t, componentStubListenEnv, "host:port for the in-test stub, reachable from the cluster's pods")

	cs, err := kubernetes.NewForConfig(kubeConfigFor(t, kubeContext))
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	ctx := context.Background()
	ns := "gonk-t55c-" + randHex(t, 3)
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: ns, Labels: map[string]string{keydomain.Prefix + "test": "t55-component"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Logf("namespace: %s", ns)
	t.Cleanup(func() { deleteNamespace(t, cs, ns) })
	if _, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: jobs.DefaultServiceAccount},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ServiceAccount %s/%s: %v", ns, jobs.DefaultServiceAccount, err)
	}

	cfg := jobs.Config{
		Namespace:      ns,
		Image:          image,
		ServiceAccount: jobs.DefaultServiceAccount,
		// No pull secret: the image is imported into the node, not pulled.
		PullSecret:              "",
		TTLSecondsAfterFinished: jobs.DefaultTTLSecondsAfterFinished,
	}
	cfg.CPURequest = resource.MustParse(jobs.DefaultCPURequest)
	cfg.MemoryRequest = resource.MustParse(jobs.DefaultMemoryRequest)
	cfg.CPULimit = resource.MustParse(jobs.DefaultCPULimit)
	cfg.MemoryLimit = resource.MustParse(jobs.DefaultMemoryLimit)
	rt, err := jobs.New(cs, cfg)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	t.Logf("agent image: %s", image)
	return &componentEnv{cs: cs, ns: ns, image: image, rt: rt, stub: startComponentStub(t, listen)}
}

// deleteNamespace deletes ns and waits, bounded, until it is gone. A
// namespace that outlives its test is reported, not ignored: it holds Jobs.
func deleteNamespace(t *testing.T, cs kubernetes.Interface, ns string) {
	ctx := context.Background()
	if err := cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("cleanup: delete namespace %s: %v", ns, err)
		return
	}
	pollUntil(t, goneBound, "namespace "+ns+" deleted", func() (bool, string) {
		_, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, "gone"
		}
		return false, fmt.Sprintf("still present (err=%v)", err)
	})
}

// pollUntil polls f every pollEvery until it reports done, or fails the test
// when the safety bound passes, naming the last observation. It never sleeps
// past a bound, and the bound is never the assertion: f's observation is.
func pollUntil(t *testing.T, bound time.Duration, what string, f func() (bool, string)) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		done, obs := f()
		if done {
			return obs
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: not observed within the %s safety bound; last observed: %s", what, bound, obs)
			return obs
		case <-tick.C:
		}
	}
}

// sessionSpec is a triage session Spec built the way runBrokerDispatch builds
// one: the pack's [env] plus the named GONK_* variables, the real triage
// prompt (with its EXAMPLE fence), a mounted key and metadata.
type sessionSpec struct {
	spec  runtime.Spec
	token string // unique string planted in the issue text, i.e. in the prompt
	key   string
	// rigURL is the session's checkout URL: mounted as rig-url, never env.
	rigURL string
}

func buildTriageSpec(t *testing.T, e *componentEnv, attempt int, sc sessionStub, deadline time.Time) sessionSpec {
	t.Helper()
	name := runtime.Name("triage", componentProjID, runtime.KindIssue, componentIID, attempt)
	if err := runtime.ValidName(name); err != nil {
		t.Fatalf("runtime.Name gave an invalid name %q: %v", name, err)
	}
	packEnv, err := packAgentEnv(componentPackDir, "triage")
	if err != nil {
		t.Fatalf("packAgentEnv: %v", err)
	}
	liteLLM := e.stub.register(name, sc)
	token := "COMPONENT-TOKEN-" + randHex(t, 6)
	key := "sk-component-" + randHex(t, 12)
	issue := "Title: the agent image will not start under a Kubernetes Job\n" +
		"Body: planted marker " + token + " -- the model endpoint checks it arrives.\n"
	prompt := renderTriagePrompt(componentProject, componentIID, "", issue, liteLLM+"/rig.tar.gz", "")
	md, err := json.Marshal(map[string]string{"gonk_project": componentProject, "gonk_bead": "gonk-t55-component", "token": token})
	if err != nil {
		t.Fatal(err)
	}
	return sessionSpec{
		token:  token,
		key:    key,
		rigURL: liteLLM + "/rig.tar.gz",
		spec: runtime.Spec{
			Name:  name,
			Agent: "triage",
			Env: buildSessionEnv(packEnv, sessionEnvInputs{
				Attempt: attempt, Model: componentModel,
				LiteLLMURL: liteLLM, RigBaseURL: e.stub.base,
			}),
			// The checkout URL embeds the rig grant's key: MOUNTED, never
			// env (plan 3.3, handshake item 4).
			RigURL:     liteLLM + "/rig.tar.gz",
			Prompt:     []byte(prompt),
			LiteLLMKey: []byte(key),
			Metadata:   md,
			Deadline:   deadline,
		},
	}
}

// answerText is what the stub's "model" says: some prose, then the batch
// fenced exactly as the triage prompt asks.
func answerText(batch string) string {
	return "Triage complete.\n\n" + batchStartSentinel + "\n" + batch + "\n" + batchEndSentinel + "\n"
}

// waitFinished polls Status until the session is Succeeded or Failed and
// returns that Status, logging each phase change it saw on the way.
func waitFinished(t *testing.T, e *componentEnv, h runtime.Handle) runtime.Status {
	t.Helper()
	var st runtime.Status
	var seen []string
	pollUntil(t, finishBound, "session "+string(h)+" finished", func() (bool, string) {
		s, err := e.rt.Status(context.Background(), h)
		if err != nil {
			return false, "Status error: " + err.Error()
		}
		obs := s.Phase.String()
		if s.Reason != "" {
			obs += "/" + s.Reason
		}
		if len(seen) == 0 || seen[len(seen)-1] != obs {
			seen = append(seen, obs)
		}
		st = s
		return s.Phase.Finished(), obs
	})
	t.Logf("session %s phases observed: %s", h, strings.Join(seen, " -> "))
	return st
}

// sessionPods lists the pods carrying the session label.
func sessionPods(t *testing.T, e *componentEnv, name string) []corev1.Pod {
	t.Helper()
	pods, err := e.cs.CoreV1().Pods(e.ns).List(context.Background(), metav1.ListOptions{LabelSelector: jobs.SessionLabel + "=" + name})
	if err != nil {
		t.Fatalf("list pods of %s: %v", name, err)
	}
	return pods.Items
}

// agentTerminated returns the agent container's terminated state, or nil.
func agentTerminated(p corev1.Pod) *corev1.ContainerStateTerminated {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == "agent" {
			return cs.State.Terminated
		}
	}
	return nil
}

// TestComponentJobsSessionRunsTheRealAgentImage is P6 item 2, end to end, on
// one session: Start creates the Job and its owned Secret; a second Start is
// the same Job; the pod's env is D5's and nothing else; the entrypoint read
// the MOUNTED prompt and key (the stub saw the prompt text and the key, and
// no prompt fetch); the session reaches Succeeded; Output holds a fence that
// extractBatch parses into exactly the batch the stub's model emitted; Stop
// removes the Job, its pod and its Secret.
func TestComponentJobsSessionRunsTheRealAgentImage(t *testing.T) {
	e := newComponentEnv(t)
	ctx := context.Background()

	batch := `{"verdict":"reply-only","effects":[{"kind":"comment","body":"component run: the Job ran and the batch came back"},{"kind":"label","add":["gonk::question"]}]}`
	ss := buildTriageSpec(t, e, 1, sessionStub{scenario: scenarioAnswer, answer: answerText(batch)}, time.Now().Add(20*time.Minute))
	name := ss.spec.Name

	// --- Start creates a Job (and, after it, the Secret it owns) ----------
	h, created, err := e.rt.Start(ctx, ss.spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !created {
		t.Errorf("Start reported created=false for a Job it created (plan 3.4 C2)")
	}
	if string(h) != name {
		t.Errorf("Start returned handle %q, want the session name %q", h, name)
	}
	job, err := e.cs.BatchV1().Jobs(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("after Start, Job %s/%s: %v", e.ns, name, err)
	}
	if job.UID == "" || job.Labels[jobs.ComponentLabel] != jobs.ComponentValue || job.Labels[jobs.SessionLabel] != name {
		t.Errorf("Job %s: uid=%q labels=%v, want a uid and %s=%s, %s=%s", name, job.UID, job.Labels,
			jobs.ComponentLabel, jobs.ComponentValue, jobs.SessionLabel, name)
	}
	if got := job.Spec.Template.Labels["app"]; got != "gc-agent" {
		t.Errorf("pod template label app=%q, want gc-agent (the NetworkPolicy join key)", got)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 {
		t.Fatalf("Job %s: backoffLimit=%v activeDeadlineSeconds=%v, want 0 and > 0", name, job.Spec.BackoffLimit, job.Spec.ActiveDeadlineSeconds)
	}
	t.Logf("Start created Job %s uid=%s activeDeadlineSeconds=%d", name, job.UID, *job.Spec.ActiveDeadlineSeconds)
	sec, err := e.cs.CoreV1().Secrets(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("after Start, Secret %s/%s: %v", e.ns, name, err)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != job.UID {
		t.Errorf("Secret %s ownerReferences=%v, want exactly the Job uid %s", name, sec.OwnerReferences, job.UID)
	}
	if !bytes.Equal(sec.Data[jobs.KeyPrompt], ss.spec.Prompt) || !bytes.Equal(sec.Data[jobs.KeyLiteLLMKey], ss.spec.LiteLLMKey) {
		t.Errorf("Secret %s does not hold the Spec's prompt and key", name)
	}
	if string(sec.Data[jobs.KeyRigURL]) != ss.rigURL {
		t.Errorf("Secret %s %s = %q, want the checkout URL %q (mounted, not env)", name, jobs.KeyRigURL, sec.Data[jobs.KeyRigURL], ss.rigURL)
	}

	// --- A second Start of the same name returns the SAME Job -------------
	again := ss.spec
	again.Prompt = []byte("a second dispatch's prompt, which must NOT replace the first")
	h2, created2, err := e.rt.Start(ctx, again)
	if err != nil {
		t.Errorf("second Start of %s: %v, want the existing session and no error", name, err)
	}
	if created2 {
		t.Errorf("second Start of %s reported created=true; it ADOPTED the existing Job (plan 3.4 C2)", name)
	}
	if h2 != h {
		t.Errorf("second Start returned handle %q, want %q", h2, h)
	}
	job2, err := e.cs.BatchV1().Jobs(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("after second Start, Job %s: %v", name, err)
	}
	if job2.UID != job.UID {
		t.Errorf("after second Start the Job uid is %s, want the first Start's %s -- the session was replaced", job2.UID, job.UID)
	}
	secs, err := e.cs.CoreV1().Secrets(e.ns).List(ctx, metav1.ListOptions{LabelSelector: jobs.SessionLabel + "=" + name})
	if err != nil {
		t.Fatalf("list Secrets: %v", err)
	}
	if len(secs.Items) != 1 || secs.Items[0].UID != sec.UID || !bytes.Equal(secs.Items[0].Data[jobs.KeyPrompt], ss.spec.Prompt) {
		t.Errorf("after second Start: %d Secret(s) for %s; want exactly the first one (uid %s) with the first prompt", len(secs.Items), name, sec.UID)
	}
	t.Logf("second Start: handle %s, Job uid %s (unchanged), 1 Secret uid %s (unchanged)", h2, job2.UID, secs.Items[0].UID)

	// --- Status reaches Succeeded -----------------------------------------
	st := waitFinished(t, e, h)
	if st.Phase != runtime.PhaseSucceeded {
		t.Errorf("session %s finished %s/%s, want Succeeded", name, st.Phase, st.Reason)
	}
	t.Logf("final Status: phase=%s reason=%q startedAt=%s finishedAt=%s", st.Phase, st.Reason,
		st.StartedAt.Format(time.RFC3339), st.FinishedAt.Format(time.RFC3339))

	// --- The pod's env is D5's, and none of the forbidden names -----------
	pods := sessionPods(t, e, name)
	if len(pods) != 1 {
		t.Fatalf("session %s has %d pods, want 1", name, len(pods))
	}
	pod := pods[0]
	checkPodEnv(t, pod, ss)
	if term := agentTerminated(pod); term == nil || term.ExitCode != 0 {
		t.Errorf("agent container terminated state %+v, want exit 0", term)
	}

	// --- The entrypoint read the mount: no prompt fetch, key + prompt arrived
	checkStubSawTheMountedSession(t, e, ss)

	// --- Output holds a fence extractBatch parses into the stub's batch ---
	out, err := e.rt.Output(ctx, h)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	t.Logf("pod log (Output, %d bytes):\n%s", len(out), out)
	text := string(out)
	raw, ok := extractBatch(text)
	if !ok {
		t.Errorf("extractBatch found no batch in the session's Output")
	} else if string(raw) != batch {
		t.Errorf("extractBatch returned\n  %s\nwant the batch the stub's model emitted\n  %s", raw, batch)
	}
	if ok {
		parsed, perr := effects.ParseBatch(raw)
		if perr != nil {
			t.Errorf("effects.ParseBatch: %v", perr)
		} else {
			shape, serr := effects.LoadShape(componentPackDir, "triage")
			if serr != nil {
				t.Fatalf("LoadShape: %v", serr)
			}
			if verr := effects.Validate(parsed, shape); verr != nil {
				t.Errorf("the extracted batch fails the triage shape: %v", verr)
			}
			t.Logf("extractBatch -> ParseBatch -> Validate(triage shape): verdict=%s effects=%d", parsed.Verdict, len(parsed.Effects))
		}
	}
	// Plan note (a): does the pod log echo the prompt, whose EXAMPLE fence
	// extractBatch could then read as if the model had said it? The prompt
	// carries one START sentinel; the model's answer another. Exactly one in
	// the log means the prompt is NOT echoed.
	if n := strings.Count(text, batchStartSentinel); n != 1 {
		t.Errorf("the pod log holds %d %s sentinels, want exactly 1 (the model's); more means the prompt's example is echoed", n, batchStartSentinel)
	}
	if strings.Contains(text, ss.token) || strings.Contains(text, "<one of: reply-only") {
		t.Errorf("the pod log contains prompt text (the planted token or the example batch): the prompt is echoed into Output")
	}
	// D10: each entrypoint line once, now the entrypoint is PID 1.
	if n := strings.Count(text, "session end: runtime=jobs"); n != 1 {
		t.Errorf("the session-end line appears %d times in the pod log, want 1 (D10: log() writes once as PID 1)", n)
	}

	// --- Stop removes Job, pod and Secret ---------------------------------
	if err := e.rt.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	obs := pollUntil(t, goneBound, "Job, pod and Secret of "+name+" all gone after Stop", func() (bool, string) {
		_, jerr := e.cs.BatchV1().Jobs(e.ns).Get(ctx, name, metav1.GetOptions{})
		_, serr := e.cs.CoreV1().Secrets(e.ns).Get(ctx, name, metav1.GetOptions{})
		np := len(sessionPods(t, e, name))
		obs := fmt.Sprintf("job notfound=%v secret notfound=%v pods=%d", apierrors.IsNotFound(jerr), apierrors.IsNotFound(serr), np)
		return apierrors.IsNotFound(jerr) && apierrors.IsNotFound(serr) && np == 0, obs
	})
	t.Logf("after Stop: %s", obs)
	if err := e.rt.Stop(ctx, h); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("a second Stop returned %v, want runtime.ErrNotFound", err)
	}
	if st, err := e.rt.Status(ctx, h); err != nil || st.Phase != runtime.PhaseNotFound {
		t.Errorf("Status after Stop = %v, %v; want NotFound, nil", st.Phase, err)
	}
}

// checkPodEnv asserts the pod's env NAMES are exactly D5's -- the pack's
// [env], the named GONK_* variables dispatch sets, and the four the runtime
// sets -- that none matches the Gas City plumbing, and that no value carries
// the key or the prompt. It reads the POD, not the Job template.
func checkPodEnv(t *testing.T, pod corev1.Pod, ss sessionSpec) {
	t.Helper()
	var ctr *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "agent" {
			ctr = &pod.Spec.Containers[i]
		}
	}
	if ctr == nil || len(pod.Spec.Containers) != 1 {
		t.Fatalf("pod %s: want exactly one container named agent, got %d", pod.Name, len(pod.Spec.Containers))
	}
	got := map[string]string{}
	for _, ev := range ctr.Env {
		if ev.ValueFrom != nil {
			t.Errorf("pod env %s is valueFrom %+v; a session's env is literal values only", ev.Name, ev.ValueFrom)
		}
		got[ev.Name] = ev.Value
	}
	if len(ctr.EnvFrom) != 0 {
		t.Errorf("pod container has envFrom %+v; want none", ctr.EnvFrom)
	}
	want := maps.Clone(ss.spec.Env)
	for _, k := range runtimeOwnedEnv {
		want[k] = ""
	}
	gotNames, wantNames := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want))
	if !slices.Equal(gotNames, wantNames) {
		t.Errorf("pod env names\n  got  %v\n  want %v (pack [env] + dispatch's GONK_* + the runtime-owned names)", gotNames, wantNames)
	}
	for _, k := range []string{envAttempt, envModel, envLiteLLMURL, envRigBaseURL, "OPENCODE_PERMISSION", "HOME"} {
		if _, ok := got[k]; !ok {
			t.Errorf("pod env lacks %s (D5)", k)
		}
	}
	for k, v := range map[string]string{
		"GONK_SESSION_RUNTIME": "jobs", "GONK_SESSION_DIR": jobs.SessionDir,
		"GONK_SESSION_NAME": ss.spec.Name, "GONK_AGENT": ss.spec.Agent,
		// The session contract the controller speaks (plan 3.3): the
		// entrypoint refuses to run on any version it does not support.
		runtime.EnvSessionContract: strconv.Itoa(runtime.SessionContract),
	} {
		if got[k] != v {
			t.Errorf("pod env %s=%q, want %q", k, got[k], v)
		}
	}
	for _, k := range gotNames {
		if forbiddenPodEnv.MatchString(k) {
			t.Errorf("pod env carries %s: D5 forbids GC_DOLT*/BEADS_*/GC_INSTANCE*/GC_CITY_WRITE* in a Jobs pod (gonk-426v)", k)
		}
		if strings.Contains(got[k], ss.key) || strings.Contains(got[k], ss.token) {
			t.Errorf("pod env %s carries the LiteLLM key or prompt text; both are mounted, never env", k)
		}
		// The checkout URL embeds the rig grant's key; anyone who can `get`
		// the pod could fetch the tree with it (plan 3.3, handshake item 4).
		if strings.Contains(got[k], ss.rigURL) {
			t.Errorf("pod env %s=%q carries the rig checkout URL; it is mounted as %s, never env", k, got[k], jobs.KeyRigURL)
		}
	}
	t.Logf("pod %s env names (%d): %s", pod.Name, len(gotNames), strings.Join(gotNames, " "))
}

// checkStubSawTheMountedSession asserts, from the model endpoint's side, that
// the session used what was MOUNTED: every chat call carries the mounted key
// and metadata, the prompt text reached the model, and there was no request
// but the model and the checkout -- in particular no prompt fetch.
func checkStubSawTheMountedSession(t *testing.T, e *componentEnv, ss sessionSpec) {
	t.Helper()
	all := e.stub.requests("")
	var chats, rigs int
	var promptSeen bool
	for _, r := range all {
		t.Logf("stub saw: session=%s %s %s auth=%t metadata=%t", r.Session, r.Method, r.Path, r.Auth != "", r.Metadata != "")
		if r.Session != ss.spec.Name {
			t.Errorf("stub saw a request outside this session's routes: %s %s (a prompt fetch would land here)", r.Method, r.Path)
			continue
		}
		switch r.Path {
		case "/v1/chat/completions":
			chats++
			if r.Auth != "Bearer "+ss.key {
				t.Errorf("chat call authenticated with %q, want the MOUNTED key", redactAuth(r.Auth))
			}
			if r.Metadata != string(ss.spec.Metadata) {
				t.Errorf("chat call metadata header %q, want the mounted metadata.json %q", r.Metadata, ss.spec.Metadata)
			}
			if strings.Contains(r.Body, ss.token) {
				promptSeen = true
			}
		case "/rig.tar.gz":
			rigs++
		default:
			t.Errorf("stub saw %s %s for this session; want only the model and the checkout (a prompt fetch is the Gas City path)", r.Method, r.Path)
		}
		if strings.Contains(r.Path, "prompt") {
			t.Errorf("stub saw a PROMPT FETCH: %s %s -- the entrypoint must read the mounted prompt", r.Method, r.Path)
		}
	}
	if chats == 0 {
		t.Errorf("the stub model saw no chat completion: opencode never reached it")
	}
	if !promptSeen {
		t.Errorf("no chat call carried the planted prompt token %s: the mounted prompt did not reach the model", ss.token)
	}
	if rigs != 1 {
		t.Errorf("the checkout was fetched %d times, want 1", rigs)
	}
	t.Logf("stub: %d chat calls (all with the mounted key and metadata; prompt token seen=%t), %d checkout fetch, 0 prompt fetches", chats, promptSeen, rigs)
}

func redactAuth(s string) string {
	if len(s) > 14 {
		return s[:14] + "..."
	}
	return s
}

// TestComponentJobsAModelThatEmitsNoBatchYieldsNoBatch answers plan note (a)
// as an assertion rather than a reading: the triage prompt contains an
// EXAMPLE fence, and if the pod log echoed the prompt, extractBatch would
// find that example in the log of a session whose model said nothing useful
// -- and the sweep would judge the example. Here the model answers prose with
// no fence, and extractBatch must find nothing.
func TestComponentJobsAModelThatEmitsNoBatchYieldsNoBatch(t *testing.T) {
	e := newComponentEnv(t)
	ctx := context.Background()
	ss := buildTriageSpec(t, e, 2, sessionStub{scenario: scenarioAnswer,
		answer: "I looked at the issue and I have no batch to offer.\n"}, time.Now().Add(20*time.Minute))
	h, _, err := e.rt.Start(ctx, ss.spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitFinished(t, e, h)
	if st.Phase != runtime.PhaseSucceeded {
		t.Errorf("session finished %s/%s, want Succeeded (opencode exits 0 on a batchless answer)", st.Phase, st.Reason)
	}
	out, err := e.rt.Output(ctx, h)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	t.Logf("pod log (Output, %d bytes):\n%s", len(out), out)
	if !bytes.Contains(out, []byte("I looked at the issue")) {
		t.Errorf("the model's answer is not in the pod log; this run proves nothing about echoing")
	}
	if raw, ok := extractBatch(string(out)); ok {
		t.Errorf("extractBatch found a batch in the log of a session whose model emitted none: %s -- "+
			"the prompt's example fence is being read as the model's answer", raw)
	}
	if n := bytes.Count(out, []byte(batchStartSentinel)); n != 0 {
		t.Errorf("the pod log holds %d %s sentinels; the model emitted none, so the prompt is echoed", n, batchStartSentinel)
	}
}

// TestComponentJobsDeadlineExceededReachesFailedWithTheSweepsReason: a
// session with a 5s deadline whose endpoints never answer is killed by the
// Job's activeDeadlineSeconds, and Status reports Failed with EXACTLY the
// reason string the sweep's infra-failed classification matches
// (reasonDeadlineExceeded, sweep.go). The literal string is logged.
func TestComponentJobsDeadlineExceededReachesFailedWithTheSweepsReason(t *testing.T) {
	e := newComponentEnv(t)
	ctx := context.Background()
	ss := buildTriageSpec(t, e, 3, sessionStub{scenario: scenarioStall}, time.Now().Add(5*time.Second))
	h, _, err := e.rt.Start(ctx, ss.spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job, err := e.cs.BatchV1().Jobs(e.ns).Get(ctx, ss.spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	// The deadline is the runtime's to set from Spec.Deadline; without it
	// nothing below can happen (and the session would run until the stub
	// gave up).
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 || *job.Spec.ActiveDeadlineSeconds > 6 {
		t.Fatalf("Job %s activeDeadlineSeconds=%v, want the 5s Spec.Deadline rounded up (5 or 6)", job.Name, job.Spec.ActiveDeadlineSeconds)
	}
	t.Logf("Job %s activeDeadlineSeconds=%d", job.Name, *job.Spec.ActiveDeadlineSeconds)

	st := waitFinished(t, e, h)
	if st.Phase != runtime.PhaseFailed {
		t.Errorf("session finished %s, want Failed", st.Phase)
	}
	if st.Reason != reasonDeadlineExceeded {
		t.Errorf("Status.Reason = %q, want %q (the string sweep.go's infra-failed case matches)", st.Reason, reasonDeadlineExceeded)
	}
	job, err = e.cs.BatchV1().Jobs(e.ns).Get(ctx, ss.spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	for _, c := range job.Status.Conditions {
		t.Logf("Job condition: type=%s status=%s reason=%q message=%q", c.Type, c.Status, c.Reason, c.Message)
	}
	t.Logf("OBSERVED Status: phase=%s reason=%q", st.Phase, st.Reason)

	var stalled []string
	for _, r := range e.stub.requests(ss.spec.Name) {
		if r.Stalled {
			stalled = append(stalled, r.Method+" "+r.Path)
		}
	}
	t.Logf("stub requests held open (stalled) before the deadline: %v", stalled)
	pods := sessionPods(t, e, ss.spec.Name)
	for _, p := range pods {
		t.Logf("pod %s after the deadline: phase=%s deletionTimestamp=%v terminated=%+v", p.Name, p.Status.Phase, p.DeletionTimestamp, agentTerminated(p))
	}
	out, err := e.rt.Output(ctx, h)
	if err != nil {
		t.Logf("Output after DeadlineExceeded: error %v", err)
	} else {
		t.Logf("Output after DeadlineExceeded: %d bytes:\n%s", len(out), out)
	}
}

// TestComponentJobsASessionContractMismatchExitsEightBeforeTouchingAnything
// is the real image-skew shape (plan 3.3, handshake item 5): the REAL
// jobs.Runtime.Start (which stamps the pod with runtime.SessionContract)
// against an agent image VARIANT that supports only contract 999 -- an
// "older or newer" image. The entrypoint must refuse with its dedicated exit
// code 8 before reading its inputs, fetching the checkout or reaching the
// model, and the runtime must surface that code as Status.ExitCode on a
// Failed session, which is what the sweep's skew classification reads.
func TestComponentJobsASessionContractMismatchExitsEightBeforeTouchingAnything(t *testing.T) {
	skew := mustEnv(t, componentSkewImageEnv, "an agent image variant whose GONK_SUPPORTED_SESSION_CONTRACTS excludes the controller's contract")
	e := newComponentEnvWithImage(t, skew)
	ctx := context.Background()
	ss := buildTriageSpec(t, e, 4, sessionStub{scenario: scenarioAnswer,
		answer: answerText(`{"verdict":"reply-only","effects":[{"kind":"comment","body":"must never run"}]}`)}, time.Now().Add(20*time.Minute))
	h, _, err := e.rt.Start(ctx, ss.spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitFinished(t, e, h)
	if st.Phase != runtime.PhaseFailed {
		t.Errorf("session finished %s/%s, want Failed", st.Phase, st.Reason)
	}
	if st.ExitCode == nil || *st.ExitCode != runtime.ExitSessionContractMismatch {
		t.Errorf("Status.ExitCode = %v, want %d (runtime.ExitSessionContractMismatch)", derefInt(st.ExitCode), runtime.ExitSessionContractMismatch)
	}
	t.Logf("OBSERVED Status: phase=%s reason=%q exitCode=%v", st.Phase, st.Reason, derefInt(st.ExitCode))

	pods := sessionPods(t, e, ss.spec.Name)
	if len(pods) != 1 {
		t.Fatalf("session %s has %d pods, want 1", ss.spec.Name, len(pods))
	}
	term := agentTerminated(pods[0])
	if term == nil || term.ExitCode != int32(runtime.ExitSessionContractMismatch) {
		t.Errorf("agent container terminated state %+v, want exit %d", term, runtime.ExitSessionContractMismatch)
	} else {
		t.Logf("pod %s agent container terminated: exitCode=%d reason=%s", pods[0].Name, term.ExitCode, term.Reason)
	}
	checkPodEnv(t, pods[0], ss) // the pod WAS stamped with contract 1

	out, err := e.rt.Output(ctx, h)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	t.Logf("pod log (Output, %d bytes):\n%s", len(out), out)
	want := fmt.Sprintf("session contract mismatch: sent=%d supported=[999]", runtime.SessionContract)
	if !bytes.Contains(out, []byte(want)) {
		t.Errorf("the pod log lacks %q", want)
	}
	if !bytes.Contains(out, []byte("session end: refused (session contract mismatch)")) {
		t.Errorf("the pod log lacks the refusal's session-end line (the sweep's skew check reads it)")
	}
	for _, not := range []string{"session files present", "fetched session checkout", "starting opencode run"} {
		if bytes.Contains(out, []byte(not)) {
			t.Errorf("the pod log contains %q: the refusal came AFTER the entrypoint touched its inputs", not)
		}
	}
	if reqs := e.stub.requests(ss.spec.Name); len(reqs) != 0 {
		t.Errorf("the stub saw %d request(s) from a session that must refuse before touching anything: %+v", len(reqs), reqs)
	}
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestComponentJobsALoopingModelIsStoppedByTheWatchdog pins the loop
// watchdog (gonk-un7z) to the REAL opencode output format, in the real agent
// image: the stub model (scenarioLoop) makes real opencode `read` a file that
// does not exist, again and again, exactly the rom#1 shape. The entrypoint's
// filter must recognise opencode's own failed-call line, stop the session at
// the second identical failure, and exit runtime.ExitLoopWatchdog; the runtime
// must surface that as a Failed Status with that ExitCode; and the sweep's own
// helpers must read the repeated call out of the pod log.
//
// THIS is the test that goes red when an opencode upgrade (images/versions.env
// OPENCODE_VERSION) changes the failed-call line: test/entrypoint's fixtures
// are a copy of the old format and cannot notice.
func TestComponentJobsALoopingModelIsStoppedByTheWatchdog(t *testing.T) {
	e := newComponentEnv(t)
	ctx := context.Background()
	ss := buildTriageSpec(t, e, 5, sessionStub{scenario: scenarioLoop,
		answer: "Title: looping component session\n"}, time.Now().Add(20*time.Minute))
	h, _, err := e.rt.Start(ctx, ss.spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitFinished(t, e, h)
	out, oerr := e.rt.Output(ctx, h)
	if oerr != nil {
		t.Fatalf("Output: %v", oerr)
	}
	t.Logf("pod log (Output, %d bytes):\n%s", len(out), out)
	calls := e.stub.loopToolCalls(ss.spec.Name)
	t.Logf("OBSERVED Status: phase=%s reason=%q exitCode=%v; the stub answered %d looping tool call(s) (gives up after %d)",
		st.Phase, st.Reason, derefInt(st.ExitCode), calls, loopGiveUp)

	if st.Phase != runtime.PhaseFailed || st.ExitCode == nil || *st.ExitCode != runtime.ExitLoopWatchdog {
		t.Fatalf("session finished %s/%s exitCode=%v, want Failed with exit %d (runtime.ExitLoopWatchdog): the watchdog did "+
			"not recognise REAL opencode's failed tool call -- check the pod log above against the signature rule in entrypoint.sh",
			st.Phase, st.Reason, derefInt(st.ExitCode), runtime.ExitLoopWatchdog)
	}
	if !isLoopWatchdogExit(&st) {
		t.Errorf("the sweep's isLoopWatchdogExit does not recognise this Status")
	}
	cause, ok := loopWatchdogCause(out)
	if !ok {
		t.Fatalf("the sweep's loopWatchdogCause found no watchdog line in the pod log")
	}
	t.Logf("sweep cause: %s", cause)
	if !strings.Contains(cause, "failed 2 times (threshold 2)") || !strings.Contains(cause, "adapter.go") {
		t.Errorf("cause %q does not name the repeated read of %s at the default threshold", cause, loopMissingFile)
	}
	// STOPPED, i.e. tokens saved: the stub would have answered up to
	// loopGiveUp calls. Two failing calls fire the watchdog; at most one more
	// may already be in flight when the kill lands.
	if calls < 2 || calls > 3 {
		t.Errorf("the stub answered %d looping tool calls, want 2 (or 3 with one in flight at the kill)", calls)
	}
	if !bytes.Contains(out, []byte("session end: opencode run exited rc=137")) {
		t.Errorf("opencode was not ended by the watchdog's SIGKILL (no rc=137 line)")
	}
	if raw, ok := extractBatch(string(out)); ok {
		t.Errorf("extractBatch found a batch in a stopped session's log: %s", raw)
	}
	pods := sessionPods(t, e, ss.spec.Name)
	if len(pods) != 1 {
		t.Fatalf("session %s has %d pods, want 1", ss.spec.Name, len(pods))
	}
	if term := agentTerminated(pods[0]); term == nil || term.ExitCode != int32(runtime.ExitLoopWatchdog) {
		t.Errorf("agent container terminated state %+v, want exit %d", term, runtime.ExitLoopWatchdog)
	}
}

// The two malformed `glob` calls of the first live pipeline diagnosis (project
// 75 pipeline 3246, attempt 1, 2026-10-04; bead gonk-mf3v): each argument
// string has one closing brace too many, so it is not JSON.
const (
	malformedGlobCI     = `{"pattern": "**/.gitlab-ci*"}}`
	malformedGlobReadme = `{"pattern": "README*"}}`
)

// TestComponentJobsMalformedToolCallsAreToldApartByTheWatchdog pins, against
// REAL opencode, how the loop watchdog reads a tool call whose arguments are
// not JSON (gonk-mf3v). opencode prints EVERY such call as the same bare line
// "Invalid Tool" and puts what was actually called on the line after it, so a
// signature made of the first line alone counts two DIFFERENT calls as one
// call failing twice -- which stopped a live session that had not looped.
//
//   - two different malformed calls, then an answer: NOT stopped;
//   - the identical malformed call twice: stopped, exit ExitLoopWatchdog, and
//     the cause names the call.
//
// As with the looping test above, THIS is what goes red when an opencode
// upgrade changes those lines; test/entrypoint's fixtures are copies.
func TestComponentJobsMalformedToolCallsAreToldApartByTheWatchdog(t *testing.T) {
	t.Run("two different malformed calls are not a loop", func(t *testing.T) {
		e := newComponentEnv(t)
		ctx := context.Background()
		const answer = "Both of my tool calls were malformed and I have no batch to offer.\n"
		ss := buildTriageSpec(t, e, 6, sessionStub{scenario: scenarioAnswer, answer: answer,
			malformedArgs: []string{malformedGlobCI, malformedGlobReadme}}, time.Now().Add(20*time.Minute))
		h, _, err := e.rt.Start(ctx, ss.spec)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		st := waitFinished(t, e, h)
		out, oerr := e.rt.Output(ctx, h)
		if oerr != nil {
			t.Fatalf("Output: %v", oerr)
		}
		t.Logf("pod log (Output, %d bytes):\n%s", len(out), out)
		t.Logf("pod log, quoted:\n%q", out)
		calls := e.stub.loopToolCalls(ss.spec.Name)
		t.Logf("OBSERVED Status: phase=%s reason=%q exitCode=%v; %d model call(s) offered tools", st.Phase, st.Reason, derefInt(st.ExitCode), calls)
		// "Invalid Tool\n": opencode's own lines end there; the watchdog's
		// line about a stopped session goes on after it.
		if n := bytes.Count(out, []byte("Invalid Tool\n")); n != 2 {
			t.Fatalf("the pod log holds %d \"Invalid Tool\" lines, want 2: real opencode did not print the malformed calls "+
				"the way the watchdog's signature rule assumes, so this run proves nothing", n)
		}
		if cause, fired := loopWatchdogCause(out); fired {
			t.Errorf("the watchdog stopped a session whose two failing calls were DIFFERENT: %s", cause)
		}
		if st.Phase != runtime.PhaseSucceeded {
			t.Errorf("session finished %s/%s exitCode=%v, want Succeeded", st.Phase, st.Reason, derefInt(st.ExitCode))
		}
		if !bytes.Contains(out, []byte("Both of my tool calls were malformed")) {
			t.Errorf("the model's final answer is not in the pod log: the session did not run to its end")
		}
		if calls != 3 {
			t.Errorf("%d model calls offered tools, want 3 (two malformed calls, then the answer)", calls)
		}
	})
	t.Run("the identical malformed call twice is a loop", func(t *testing.T) {
		e := newComponentEnv(t)
		ctx := context.Background()
		same := make([]string, loopGiveUp)
		for i := range same {
			same[i] = malformedGlobReadme
		}
		ss := buildTriageSpec(t, e, 7, sessionStub{scenario: scenarioAnswer,
			answer: "I am giving up without a batch.\n", malformedArgs: same}, time.Now().Add(20*time.Minute))
		h, _, err := e.rt.Start(ctx, ss.spec)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		st := waitFinished(t, e, h)
		out, oerr := e.rt.Output(ctx, h)
		if oerr != nil {
			t.Fatalf("Output: %v", oerr)
		}
		t.Logf("pod log (Output, %d bytes):\n%s", len(out), out)
		t.Logf("pod log, quoted:\n%q", out)
		calls := e.stub.loopToolCalls(ss.spec.Name)
		t.Logf("OBSERVED Status: phase=%s reason=%q exitCode=%v; %d model call(s) offered tools (gives up after %d)",
			st.Phase, st.Reason, derefInt(st.ExitCode), calls, loopGiveUp)
		if st.Phase != runtime.PhaseFailed || st.ExitCode == nil || *st.ExitCode != runtime.ExitLoopWatchdog {
			t.Fatalf("session finished %s/%s exitCode=%v, want Failed with exit %d: the same malformed call twice must still stop the session",
				st.Phase, st.Reason, derefInt(st.ExitCode), runtime.ExitLoopWatchdog)
		}
		cause, ok := loopWatchdogCause(out)
		if !ok {
			t.Fatalf("the sweep's loopWatchdogCause found no watchdog line in the pod log")
		}
		t.Logf("sweep cause: %s", cause)
		for _, want := range []string{"failed 2 times (threshold 2)", "Invalid Tool", "glob", "README*"} {
			if !strings.Contains(cause, want) {
				t.Errorf("cause %q does not hold %q: it must name WHICH call was malformed", cause, want)
			}
		}
		if calls < 2 || calls > 3 {
			t.Errorf("%d model calls offered tools, want 2 (or 3 with one in flight at the kill)", calls)
		}
	})
}
