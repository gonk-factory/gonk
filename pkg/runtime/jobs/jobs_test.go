package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// newTestRuntime returns a runtime over a fake clientset that behaves like an
// API server in the one way these tests depend on: a created object gets a
// fresh UID (the fake tracker leaves it empty, and the Secret's ownerReference
// is only meaningful with one).
func newTestRuntime(t *testing.T, objs ...k8sruntime.Object) (*Runtime, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	var n atomic.Int64
	cs.PrependReactor("create", "*", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		obj := a.(k8stesting.CreateAction).GetObject()
		if m, ok := obj.(metav1.Object); ok && m.GetUID() == "" {
			m.SetUID(types.UID(fmt.Sprintf("uid-%d", n.Add(1))))
		}
		return false, nil, nil // fall through to the tracker, which stores the mutated object
	})
	r, err := newRuntime(cs, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return t0 }
	return r, cs
}

func actions(cs *fake.Clientset, verb, resource string) []k8stesting.Action {
	var out []k8stesting.Action
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource && a.GetSubresource() == "" {
			out = append(out, a)
		}
	}
	return out
}

func TestNewRefusesABadConfig(t *testing.T) {
	cs := fake.NewClientset()
	for name, mut := range map[string]func(*Config){
		"empty namespace": func(c *Config) { c.Namespace = "" },
		"empty image":     func(c *Config) { c.Image = "" },
		"zero ttl":        func(c *Config) { c.TTLSecondsAfterFinished = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mut(&cfg)
			if _, err := New(cs, cfg); err == nil {
				t.Fatal("New accepted it")
			}
		})
	}
	if _, err := New(nil, testConfig()); err == nil {
		t.Fatal("New accepted a nil client")
	}
	if _, err := New(cs, testConfig()); err != nil {
		t.Fatalf("New refused a good config: %v", err)
	}
}

// TestStartCreatesJobThenOwnedSecret: Job first, then the Secret, owned by
// that Job's UID so it is garbage-collected with it (D3).
func TestStartCreatesJobThenOwnedSecret(t *testing.T) {
	r, cs := newTestRuntime(t)
	s := testSpec()
	h, _, err := r.Start(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if string(h) != s.Name {
		t.Errorf("handle %q, want the name %q", h, s.Name)
	}

	var order []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			order = append(order, a.GetResource().Resource)
		}
	}
	if strings.Join(order, ",") != "jobs,secrets" {
		t.Fatalf("create order = %v, want [jobs secrets]", order)
	}

	job, err := cs.BatchV1().Jobs("gonk").Get(context.Background(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := cs.CoreV1().Secrets("gonk").Get(context.Background(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.UID == "" {
		t.Fatal("test harness gave the Job no UID")
	}
	if len(sec.OwnerReferences) != 1 {
		t.Fatalf("secret ownerReferences = %v, want exactly the Job", sec.OwnerReferences)
	}
	o := sec.OwnerReferences[0]
	if o.Kind != "Job" || o.APIVersion != "batch/v1" || o.Name != s.Name || o.UID != job.UID ||
		o.Controller == nil || !*o.Controller {
		t.Errorf("ownerReference = %+v, want controller ref to Job %s uid %s", o, s.Name, job.UID)
	}
	if o.BlockOwnerDeletion != nil {
		t.Errorf("blockOwnerDeletion set; Role gonk-gate-jobs cannot grant jobs/finalizers")
	}
	if string(sec.Data["litellm-key"]) != testKey {
		t.Error("secret does not carry the key")
	}
}

// TestStartTwiceReturnsTheSameJobAndNoSecondSecret is bead exit criterion 1:
// a re-dispatch of the same bead+attempt gets the EXISTING session -- the
// same Job object, untouched -- and creates no second Secret.
func TestStartTwiceReturnsTheSameJobAndNoSecondSecret(t *testing.T) {
	r, cs := newTestRuntime(t)
	ctx := context.Background()
	s := testSpec()
	h1, created1, err := r.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if !created1 {
		t.Errorf("first Start reported created=false; it created the Job")
	}
	before, err := cs.BatchV1().Jobs("gonk").Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// The second dispatch renders a different prompt: if Start replaced or
	// updated anything, it would show.
	s2 := testSpec()
	s2.Prompt = []byte("A DIFFERENT PROMPT")
	s2.Deadline = t0.Add(5 * time.Minute)
	h2, created2, err := r.Start(ctx, s2)
	if err != nil {
		t.Fatalf("second Start: %v (AlreadyExists must be the existing session, not an error)", err)
	}
	if created2 {
		t.Errorf("second Start reported created=true; it ADOPTED the existing Job (plan 3.4 C2)")
	}
	if h1 != h2 {
		t.Fatalf("handles differ: %q then %q", h1, h2)
	}

	jobs, err := cs.BatchV1().Jobs("gonk").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("%d Jobs, want 1", len(jobs.Items))
	}
	after := jobs.Items[0]
	if after.UID != before.UID || after.ResourceVersion != before.ResourceVersion ||
		*after.Spec.ActiveDeadlineSeconds != *before.Spec.ActiveDeadlineSeconds {
		t.Errorf("the Job changed: uid %s->%s rv %s->%s deadline %d->%d",
			before.UID, after.UID, before.ResourceVersion, after.ResourceVersion,
			*before.Spec.ActiveDeadlineSeconds, *after.Spec.ActiveDeadlineSeconds)
	}

	if n := len(actions(cs, "create", "secrets")); n != 1 {
		t.Errorf("%d Secret creates, want exactly 1", n)
	}
	for _, v := range []string{"update", "patch", "delete"} {
		for _, res := range []string{"jobs", "secrets"} {
			if n := len(actions(cs, v, res)); n != 0 {
				t.Errorf("%d %s %s calls; an idempotent Start touches nothing", n, v, res)
			}
		}
	}
	sec, err := cs.CoreV1().Secrets("gonk").Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(sec.Data["prompt"]) != testPrompt {
		t.Errorf("secret prompt = %q, want the FIRST dispatch's", sec.Data["prompt"])
	}
}

// TestStartRefusesToAdoptAForeignJob: AlreadyExists on a Job that is not a
// gonk session is an error, never a handle to someone else's workload.
func TestStartRefusesToAdoptAForeignJob(t *testing.T) {
	s := testSpec()
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: "gonk", Labels: map[string]string{"app": "other"}}}
	r, cs := newTestRuntime(t, foreign)
	if _, _, err := r.Start(context.Background(), s); err == nil {
		t.Fatal("Start adopted a foreign Job")
	}
	if n := len(actions(cs, "create", "secrets")); n != 0 {
		t.Errorf("%d Secret creates for a foreign Job", n)
	}
}

// TestStartSecretFailureDeletesTheJob: no half-session (D3), and the delete is
// Foreground. The fake clientset ignores contexts, so this test CANNOT show
// the delete surviving the caller's cancelled context; the next test does.
func TestStartSecretFailureDeletesTheJob(t *testing.T) {
	r, cs := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		cancel() // the caller gives up at the worst moment
		return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), "x", errors.New("no"))
	})
	s := testSpec()
	if _, _, err := r.Start(ctx, s); err == nil {
		t.Fatal("Start succeeded without its Secret")
	}
	dels := actions(cs, "delete", "jobs")
	if len(dels) != 1 {
		t.Fatalf("%d Job deletes, want 1", len(dels))
	}
	opts := dels[0].(k8stesting.DeleteAction).GetDeleteOptions()
	if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Errorf("delete propagation = %v, want Foreground", opts.PropagationPolicy)
	}
	if _, err := cs.BatchV1().Jobs("gonk").Get(context.Background(), s.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Job still present after the Secret failed: %v", err)
	}
}

// TestStartSecretFailureDeletesTheJobUnderACancelledCallerContext is plan 3.4
// C4. The rollback delete must run on a context DETACHED from the caller's,
// because the commonest way the Secret create fails is the caller's context
// ending. The fake clientset ignores contexts entirely, so the test above
// cannot tell a detached delete from one on the dead caller context; this one
// wraps the clientset so every Job delete made with a done context FAILS, as
// the real client does.
func TestStartSecretFailureDeletesTheJobUnderACancelledCallerContext(t *testing.T) {
	r, cs := newTestRuntime(t)
	r.client = ctxEnforcingClientset{cs}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		cancel() // the caller's context ends while the Secret create is in flight
		return true, nil, context.Canceled
	})
	s := testSpec()
	_, _, err := r.Start(ctx, s)
	if err == nil {
		t.Fatal("Start succeeded without its Secret")
	}
	if strings.Contains(err.Error(), "AND deleting its Job failed") {
		t.Fatalf("the rollback delete ran on the caller's cancelled context: %v", err)
	}
	if _, gerr := cs.BatchV1().Jobs("gonk").Get(context.Background(), s.Name, metav1.GetOptions{}); !apierrors.IsNotFound(gerr) {
		t.Fatalf("Job still present after the Secret failed under a cancelled caller context (half-session): get err=%v; Start err=%v", gerr, err)
	}
}

// ctxEnforcingClientset makes Job deletes honour their context, which the fake
// clientset does not: a delete made with a done context fails with its error
// and deletes nothing -- what client-go does against a real API server.
type ctxEnforcingClientset struct{ *fake.Clientset }

func (c ctxEnforcingClientset) BatchV1() batchv1client.BatchV1Interface {
	return ctxEnforcingBatch{c.Clientset.BatchV1()}
}

type ctxEnforcingBatch struct{ batchv1client.BatchV1Interface }

func (b ctxEnforcingBatch) Jobs(ns string) batchv1client.JobInterface {
	return ctxEnforcingJobs{b.BatchV1Interface.Jobs(ns)}
}

type ctxEnforcingJobs struct{ batchv1client.JobInterface }

func (j ctxEnforcingJobs) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.JobInterface.Delete(ctx, name, opts)
}

// TestStartRefusesForbiddenEnvBeforeCreatingAnything: a Spec carrying a Gas
// City name never reaches the API server.
func TestStartRefusesForbiddenEnvBeforeCreatingAnything(t *testing.T) {
	r, cs := newTestRuntime(t)
	s := testSpec()
	s.Env["GC_DOLT_PASSWORD"] = "x"
	if _, _, err := r.Start(context.Background(), s); err == nil {
		t.Fatal("Start accepted GC_DOLT_PASSWORD")
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("%d API calls for a refused Spec", n)
	}
}

// --- Status ---

func jobWith(name string, uid types.UID, conds ...batchv1.JobCondition) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "gonk", UID: uid,
		Labels: map[string]string{ComponentLabel: ComponentValue, SessionLabel: name},
	}}
	j.Status.Conditions = conds
	j.Status.StartTime = ptr.To(metav1.NewTime(t0))
	return j
}

func podFor(job *batchv1.Job, name string, created time.Time, cs ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "gonk",
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{SessionLabel: job.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true),
			}},
		},
		Status: corev1.PodStatus{ContainerStatuses: cs},
	}
}

func cond(t batchv1.JobConditionType, reason string, at time.Time) batchv1.JobCondition {
	return batchv1.JobCondition{Type: t, Status: corev1.ConditionTrue, Reason: reason, LastTransitionTime: metav1.NewTime(at)}
}

func waiting(reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
}

func running(at time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at)}}}
}

func TestStatusMapsJobAndPod(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	fin := t0.Add(3 * time.Minute)
	type want struct {
		phase    runtime.Phase
		reason   string
		started  time.Time
		finished time.Time
	}
	cases := []struct {
		name string
		objs func() []k8sruntime.Object
		want want
	}{
		{"no job", func() []k8sruntime.Object { return nil }, want{phase: runtime.PhaseNotFound}},
		{"complete", func() []k8sruntime.Object {
			j := jobWith(n, "u1", cond(batchv1.JobSuccessCriteriaMet, "CompletionsReached", fin), cond(batchv1.JobComplete, "CompletionsReached", fin))
			j.Status.CompletionTime = ptr.To(metav1.NewTime(fin))
			return []k8sruntime.Object{j}
		}, want{runtime.PhaseSucceeded, "CompletionsReached", t0, fin}},
		{"deadline exceeded", func() []k8sruntime.Object {
			return []k8sruntime.Object{jobWith(n, "u1", cond(batchv1.JobFailureTarget, "DeadlineExceeded", fin), cond(batchv1.JobFailed, "DeadlineExceeded", fin))}
		}, want{runtime.PhaseFailed, "DeadlineExceeded", t0, fin}},
		{"backoff limit exceeded", func() []k8sruntime.Object {
			return []k8sruntime.Object{jobWith(n, "u1", cond(batchv1.JobFailed, "BackoffLimitExceeded", fin))}
		}, want{runtime.PhaseFailed, "BackoffLimitExceeded", t0, fin}},
		{"no pod yet", func() []k8sruntime.Object {
			return []k8sruntime.Object{jobWith(n, "u1")}
		}, want{phase: runtime.PhasePending}},
		{"image pull backoff", func() []k8sruntime.Object {
			j := jobWith(n, "u1")
			return []k8sruntime.Object{j, podFor(j, n+"-abcde", t0, waiting("ImagePullBackOff"))}
		}, want{phase: runtime.PhasePending, reason: "ImagePullBackOff"}},
		{"unschedulable", func() []k8sruntime.Object {
			j := jobWith(n, "u1")
			p := podFor(j, n+"-abcde", t0)
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}}
			return []k8sruntime.Object{j, p}
		}, want{phase: runtime.PhasePending, reason: "Unschedulable"}},
		{"running", func() []k8sruntime.Object {
			j := jobWith(n, "u1")
			return []k8sruntime.Object{j, podFor(j, n+"-abcde", t0, running(t0.Add(time.Minute)))}
		}, want{phase: runtime.PhaseRunning, started: t0.Add(time.Minute)}},
		{"failure target, pod still terminating", func() []k8sruntime.Object {
			j := jobWith(n, "u1", cond(batchv1.JobFailureTarget, "DeadlineExceeded", fin))
			return []k8sruntime.Object{j, podFor(j, n+"-abcde", t0, running(t0.Add(time.Minute)))}
		}, want{runtime.PhaseRunning, "DeadlineExceeded", t0.Add(time.Minute), time.Time{}}},
		{"pod of an earlier Job with the same name is ignored", func() []k8sruntime.Object {
			j := jobWith(n, "u2")
			old := podFor(jobWith(n, "u1"), n+"-old00", t0, running(t0))
			return []k8sruntime.Object{j, old}
		}, want{phase: runtime.PhasePending}},
		{"newest pod wins", func() []k8sruntime.Object {
			j := jobWith(n, "u1")
			return []k8sruntime.Object{j,
				podFor(j, n+"-aaaaa", t0, waiting("ErrImagePull")),
				podFor(j, n+"-bbbbb", t0.Add(time.Second), running(t0.Add(2*time.Second)))}
		}, want{phase: runtime.PhaseRunning, started: t0.Add(2 * time.Second)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newTestRuntime(t, c.objs()...)
			st, err := r.Status(context.Background(), runtime.Handle(n))
			if err != nil {
				t.Fatal(err)
			}
			got := want{st.Phase, st.Reason, st.StartedAt, st.FinishedAt}
			if !got.started.Equal(c.want.started) || !got.finished.Equal(c.want.finished) ||
				got.phase != c.want.phase || got.reason != c.want.reason {
				t.Errorf("status = %s/%q started %v finished %v; want %s/%q started %v finished %v",
					got.phase, got.reason, got.started, got.finished,
					c.want.phase, c.want.reason, c.want.started, c.want.finished)
			}
			if st.Phase == runtime.PhaseUnknown {
				t.Error("PhaseUnknown with a nil error")
			}
		})
	}
}

func TestStatusPropagatesAPIErrors(t *testing.T) {
	r, cs := newTestRuntime(t)
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	st, err := r.Status(context.Background(), "gonk-triage-p42-i7-a1")
	if err == nil {
		t.Fatalf("Status = %v with a nil error; an unreadable Job is not NotFound", st.Phase)
	}
}

// --- Output ---

func TestOutputReadsTheSessionPodsLog(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	j := jobWith(n, "u1", cond(batchv1.JobComplete, "", t0))
	other := jobWith("gonk-triage-p42-i8-a1", "u9")
	r, _ := newTestRuntime(t, j,
		podFor(j, n+"-aaaaa", t0),
		podFor(other, "gonk-triage-p42-i8-a1-zzzzz", t0.Add(time.Hour)))
	var readPod, readContainer string
	r.readLog = func(_ context.Context, ns, pod, container string) (io.ReadCloser, error) {
		readPod, readContainer = pod, container
		return io.NopCloser(strings.NewReader("line\nGONK_BATCH_START\n{}\nGONK_BATCH_END\n")), nil
	}
	out, err := r.Output(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if readPod != n+"-aaaaa" || readContainer != "agent" {
		t.Errorf("read %s/%s, want %s-aaaaa/agent", readPod, readContainer, n)
	}
	if !strings.Contains(string(out), "GONK_BATCH_START") {
		t.Errorf("output = %q", out)
	}
}

// TestOutputThroughTheClientset exercises the production log reader: the
// fake clientset answers every pod log with "fake logs".
func TestOutputThroughTheClientset(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	j := jobWith(n, "u1")
	r, cs := newTestRuntime(t, j, podFor(j, n+"-aaaaa", t0))
	out, err := r.Output(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "fake logs" {
		t.Errorf("output = %q", out)
	}
	var sawLog bool
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "pods" && a.GetSubresource() == "log" {
			sawLog = true
			if o, ok := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions); !ok || o.Container != "agent" {
				t.Errorf("log options = %#v, want container agent", a.(k8stesting.GenericAction).GetValue())
			}
		}
	}
	if !sawLog {
		t.Error("no pods/log call")
	}
}

func TestOutputUnknownIsErrNotFound(t *testing.T) {
	r, _ := newTestRuntime(t)
	if _, err := r.Output(context.Background(), "gonk-triage-p42-i7-a1"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestOutputNoPodIsEmpty(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	r, _ := newTestRuntime(t, jobWith(n, "u1", cond(batchv1.JobFailed, "DeadlineExceeded", t0)))
	r.readLog = func(context.Context, string, string, string) (io.ReadCloser, error) {
		t.Fatal("read a log with no pod")
		return nil, nil
	}
	out, err := r.Output(context.Background(), n)
	if err != nil || len(out) != 0 {
		t.Fatalf("Output = %q, %v; want empty, nil", out, err)
	}
}

func TestOutputLogErrorIsAnError(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	j := jobWith(n, "u1")
	r, _ := newTestRuntime(t, j, podFor(j, n+"-aaaaa", t0))
	r.readLog = func(context.Context, string, string, string) (io.ReadCloser, error) {
		return nil, errors.New("container is waiting to start")
	}
	if _, err := r.Output(context.Background(), n); err == nil || errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v; want a non-NotFound error", err)
	}
}

// --- Stop ---

func TestStopDeletesForegroundAndSecondStopIsNotFound(t *testing.T) {
	r, cs := newTestRuntime(t)
	ctx := context.Background()
	s := testSpec()
	h, _, err := r.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(ctx, h); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	dels := actions(cs, "delete", "jobs")
	if len(dels) != 1 {
		t.Fatalf("%d Job deletes", len(dels))
	}
	opts := dels[0].(k8stesting.DeleteAction).GetDeleteOptions()
	if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Errorf("propagation = %v, want Foreground", opts.PropagationPolicy)
	}
	if _, err := cs.BatchV1().Jobs("gonk").Get(ctx, s.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Job survived Stop: %v", err)
	}
	if _, err := cs.CoreV1().Secrets("gonk").Get(ctx, s.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Secret survived Stop: %v", err)
	}
	if err := r.Stop(ctx, h); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("second Stop = %v, want ErrNotFound (not \"destroyed\")", err)
	}
}

func TestStopLeavesASecretItDoesNotOwn(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	foreignSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "gonk",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: n, UID: "someone-else"}}}}
	r, cs := newTestRuntime(t, jobWith(n, "u1"), foreignSecret)
	if err := r.Stop(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Secrets("gonk").Get(context.Background(), n, metav1.GetOptions{}); err != nil {
		t.Errorf("deleted a Secret another Job owns: %v", err)
	}
}

func TestStopDeleteNotFoundIsErrNotFound(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	r, cs := newTestRuntime(t, jobWith(n, "u1"))
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewNotFound(batchv1.Resource("jobs"), n) // reaped by TTL between Get and Delete
	})
	if err := r.Stop(context.Background(), n); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// --- List ---

func TestListSelectsSessionJobsOnly(t *testing.T) {
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: "gonk"}}
	elsewhere := jobWith("gonk-triage-p1-i1-a1", "u5")
	elsewhere.Namespace = "other"
	r, _ := newTestRuntime(t, jobWith("gonk-triage-p42-i8-a1", "u2"), jobWith("gonk-mention-p42-m3-a2", "u3"), foreign, elsewhere)
	got, err := r.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []runtime.Handle{"gonk-mention-p42-m3-a2", "gonk-triage-p42-i8-a1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

// TestStartThenListStatusRoundTrip: what Start creates, List and Status see.
func TestStartThenListStatusRoundTrip(t *testing.T) {
	r, _ := newTestRuntime(t)
	ctx := context.Background()
	h, _, err := r.Start(ctx, testSpec())
	if err != nil {
		t.Fatal(err)
	}
	hs, err := r.List(ctx)
	if err != nil || len(hs) != 1 || hs[0] != h {
		t.Fatalf("List = %v, %v; want [%s]", hs, err, h)
	}
	st, err := r.Status(ctx, h)
	if err != nil || st.Phase != runtime.PhasePending {
		t.Fatalf("Status = %v, %v; want Pending", st.Phase, err)
	}
}
