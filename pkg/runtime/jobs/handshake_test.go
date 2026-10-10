package jobs

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// The session-contract handshake and the rig URL's move out of env (gonk-t55
// plan section 3.3, "handshake" items 1, 3 and 4).

const testRigURL = "http://gonk-intake:8081/rig/gonk-triage-p42-i7-a1.abcdefghijklmnopqrstuvwxyz.tar.gz"

// HANDSHAKE ITEM 1: every session carries the controller's contract version,
// set by the runtime from the Go constant -- not by the caller.
func TestEverySessionCarriesTheControllersContractVersion(t *testing.T) {
	job, _ := jobSpec(testSpec(), testConfig(), t0)
	env := envMap(t, job.Spec.Template.Spec.Containers[0])
	want := strconv.Itoa(runtime.SessionContract)
	if got, ok := env[runtime.EnvSessionContract]; !ok || got != want {
		t.Fatalf("env %s = %q (set=%v), want %q: the entrypoint refuses a session without it",
			runtime.EnvSessionContract, got, ok, want)
	}
}

// ...and it is RUNTIME-OWNED: a Spec that contradicts it is refused, as for the
// other four fixed names. A caller able to set it could make a skewed image run.
func TestASpecCannotOverrideTheSessionContract(t *testing.T) {
	s := testSpec()
	s.Env[runtime.EnvSessionContract] = strconv.Itoa(runtime.SessionContract + 1)
	if err := validateSpec(s, testConfig(), t0); err == nil || !strings.Contains(err.Error(), runtime.EnvSessionContract) {
		t.Fatalf("validateSpec = %v, want a refusal naming %s", err, runtime.EnvSessionContract)
	}
	// And even past validation, the pod env is the runtime's value: jobSpec
	// lays the fixed env OVER the Spec's.
	job, _ := jobSpec(s, testConfig(), t0)
	if got := envMap(t, job.Spec.Template.Spec.Containers[0])[runtime.EnvSessionContract]; got != strconv.Itoa(runtime.SessionContract) {
		t.Fatalf("pod env %s = %q; the Spec's value beat the runtime's", runtime.EnvSessionContract, got)
	}
	// Repeating the runtime's own value is allowed, like the other fixed names.
	s.Env[runtime.EnvSessionContract] = strconv.Itoa(runtime.SessionContract)
	if err := validateSpec(s, testConfig(), t0); err != nil {
		t.Fatalf("repeating the fixed value was refused: %v", err)
	}
}

// HANDSHAKE ITEM 4: the rig URL is a capability (pkg/rig serves the granted
// tree to anyone presenting its key until the grant's TTL), so it rides the
// session Secret as `rig-url`, and NO env value of the Job contains it -- a
// `kubectl get job -o yaml` must not hand it out.
func TestTheRigURLIsMountedAndNeverInTheJobsEnv(t *testing.T) {
	s := testSpec()
	s.RigURL = testRigURL
	if err := validateSpec(s, testConfig(), t0); err != nil {
		t.Fatalf("a spec carrying its rig URL in the RigURL field was refused: %v", err)
	}
	job, sec := jobSpec(s, testConfig(), t0)
	if got := string(sec.Data[KeyRigURL]); got != testRigURL {
		t.Fatalf("secret %s = %q, want the rig URL", KeyRigURL, got)
	}
	if KeyRigURL != "rig-url" {
		t.Fatalf("KeyRigURL = %q; the entrypoint reads the file rig-url", KeyRigURL)
	}
	pt := job.Spec.Template
	for _, c := range append(append([]corev1.Container{}, pt.Spec.InitContainers...), pt.Spec.Containers...) {
		for _, e := range c.Env {
			if strings.Contains(e.Value, testRigURL) || strings.Contains(e.Value, "abcdefghijklmnopqrstuvwxyz") {
				t.Errorf("container %s env %s carries the rig URL", c.Name, e.Name)
			}
			if e.Name == "GONK_RIG_URL" {
				t.Errorf("container %s still sets GONK_RIG_URL", c.Name)
			}
		}
		for _, a := range append(append([]string{}, c.Command...), c.Args...) {
			if strings.Contains(a, testRigURL) {
				t.Errorf("container %s argv carries the rig URL", c.Name)
			}
		}
	}
	for k, v := range pt.Annotations {
		if strings.Contains(v, testRigURL) {
			t.Errorf("pod annotation %s carries the rig URL", k)
		}
	}
}

// No checkout granted, no `rig-url` file: its ABSENCE is how the entrypoint
// knows it runs without a working copy.
func TestNoRigURLMeansNoRigURLFile(t *testing.T) {
	_, sec := jobSpec(testSpec(), testConfig(), t0)
	if _, ok := sec.Data[KeyRigURL]; ok {
		t.Fatalf("secret has %s with no rig URL in the Spec", KeyRigURL)
	}
	if len(sec.Data) != 3 {
		t.Fatalf("secret keys = %v, want exactly prompt, litellm-key, metadata.json", keys(sec.Data))
	}
}

// ...and a Spec that ALSO puts the URL in env is refused before any API call:
// the same guard the LiteLLM key has.
func TestStartRefusesTheRigURLInEnv(t *testing.T) {
	r, cs := newTestRuntime(t)
	s := testSpec()
	s.RigURL = testRigURL
	s.Env["GONK_RIG_URL"] = testRigURL
	_, _, err := r.Start(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "rig checkout URL") {
		t.Fatalf("Start = %v, want a refusal naming the rig checkout URL", err)
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("%d API calls for a refused Spec", n)
	}
}

func terminated(code int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "agent", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: code, StartedAt: metav1.NewTime(t0)},
	}}
}

// HANDSHAKE ITEM 3's input: Status carries the agent container's exit code for
// a finished session, read from the pod's terminated state -- the only place
// the entrypoint's skew refusal (exit 8) is visible, since the Job's own
// condition says only BackoffLimitExceeded.
func TestStatusCarriesTheContainersExitCode(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	fin := t0.Add(time.Minute)
	cases := []struct {
		name string
		objs func() []k8sruntime.Object
		want *int
	}{
		{"failed with the skew code", func() []k8sruntime.Object {
			j := jobWith(n, "u1", cond(batchv1.JobFailed, "BackoffLimitExceeded", fin))
			return []k8sruntime.Object{j, podFor(j, n+"-abcde", t0, terminated(runtime.ExitSessionContractMismatch))}
		}, ptr.To(runtime.ExitSessionContractMismatch)},
		{"failed with another code", func() []k8sruntime.Object {
			j := jobWith(n, "u1", cond(batchv1.JobFailed, "BackoffLimitExceeded", fin))
			return []k8sruntime.Object{j, podFor(j, n+"-abcde", t0, terminated(1))}
		}, ptr.To(1)},
		{"complete", func() []k8sruntime.Object {
			j := jobWith(n, "u1", cond(batchv1.JobComplete, "CompletionsReached", fin))
			return []k8sruntime.Object{j, podFor(j, n+"-abcde", t0, terminated(0))}
		}, ptr.To(0)},
		{"failed with no pod (deadline before scheduling)", func() []k8sruntime.Object {
			return []k8sruntime.Object{jobWith(n, "u1", cond(batchv1.JobFailed, "DeadlineExceeded", fin))}
		}, nil},
		{"failed, pod of an EARLIER Job with the skew code is not this one's", func() []k8sruntime.Object {
			j := jobWith(n, "u2", cond(batchv1.JobFailed, "BackoffLimitExceeded", fin))
			return []k8sruntime.Object{j, podFor(jobWith(n, "u1"), n+"-old00", t0, terminated(runtime.ExitSessionContractMismatch))}
		}, nil},
		{"running has none", func() []k8sruntime.Object {
			j := jobWith(n, "u1")
			return []k8sruntime.Object{j, podFor(j, n+"-abcde", t0, running(t0))}
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newTestRuntime(t, c.objs()...)
			st, err := r.Status(context.Background(), runtime.Handle(n))
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case c.want == nil && st.ExitCode != nil:
				t.Fatalf("ExitCode = %d, want none", *st.ExitCode)
			case c.want != nil && (st.ExitCode == nil || *st.ExitCode != *c.want):
				t.Fatalf("ExitCode = %v, want %d", st.ExitCode, *c.want)
			}
		})
	}
}

// A failed Job whose pods cannot be listed is an UNREADABLE status, not a
// status without an exit code: reading it as the latter would judge a skewed
// session as an ordinary failure and charge it.
func TestStatusOfAFailedJobWithAnUnreadablePodIsAnError(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	r, cs := newTestRuntime(t, jobWith(n, "u1", cond(batchv1.JobFailed, "BackoffLimitExceeded", t0)))
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	if st, err := r.Status(context.Background(), runtime.Handle(n)); err == nil {
		t.Fatalf("Status = %s with a nil error though its pod could not be read", st.Phase)
	}
}

// A skewed session is stopped and re-dispatched under the SAME name (its
// attempt does not advance). If the Job is still being deleted when the
// re-dispatch arrives, Start must NOT hand it back as the existing session:
// the sweep would then find it absent and charge an attempt.
func TestStartDoesNotAdoptAJobThatIsBeingDeleted(t *testing.T) {
	s := testSpec()
	dying := jobWith(s.Name, "u-old")
	dying.DeletionTimestamp = ptr.To(metav1.NewTime(t0))
	dying.Finalizers = []string{"foregroundDeletion"}
	r, cs := newTestRuntime(t, dying)
	_, _, err := r.Start(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("Start = %v, want a refusal to adopt a Job that is being deleted", err)
	}
	if n := len(actions(cs, "create", "secrets")); n != 0 {
		t.Errorf("%d Secret creates for a dying Job", n)
	}
}
