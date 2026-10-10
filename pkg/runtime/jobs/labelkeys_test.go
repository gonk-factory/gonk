package jobs

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// THE WIRE CONTRACT, AS LITERALS ON PURPOSE.
//
// Every other test in this package names the labels through ComponentLabel
// and SessionLabel, so it stays green whatever those constants hold -- and it
// would stay green if the WRITER (jobSpec) and the SELECTOR (List, sessionPod)
// ever stopped using the same one. These three tests spell the keys out, once
// for each side:
//
//   - the writer must put exactly these keys on the Job, its pod and its Secret;
//   - the selector must find an object that carries exactly these keys, must
//     ask the API for exactly this selector, and must NOT find a session
//     labelled under any other domain.
//
// A gate that writes one key and selects another creates sessions it can never
// list, reap or resolve, and nothing errors: the list is empty. That is a
// failed deploy, so it has to be a failed test first.
//
// Changing these strings is a wire change on every cluster that has a session
// running: see "Upgrading" in chart/gonk/README.md.
const (
	wireComponentKey = "gonk-factory.github.io/component"
	wireSessionKey   = "gonk-factory.github.io/session"
	wireComponentVal = "agent-session"
)

func TestWriterLabelsEveryObjectWithTheWireKeys(t *testing.T) {
	s := testSpec()
	job, sec := jobSpec(s, testConfig(), t0)
	for what, got := range map[string]map[string]string{
		"Job":          job.Labels,
		"pod template": job.Spec.Template.Labels,
		"Secret":       sec.Labels,
	} {
		if got[wireComponentKey] != wireComponentVal {
			t.Errorf("%s label %s = %q, want %q; labels: %v", what, wireComponentKey, got[wireComponentKey], wireComponentVal, got)
		}
		if got[wireSessionKey] != s.Name {
			t.Errorf("%s label %s = %q, want %q; labels: %v", what, wireSessionKey, got[wireSessionKey], s.Name, got)
		}
	}
}

// labelSelectorsAsked records the label selector of every list of `resource`.
func labelSelectorsAsked(cs interface {
	PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
}, resource string) *[]string {
	var asked []string
	cs.PrependReactor("list", resource, func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		asked = append(asked, a.(k8stesting.ListAction).GetListRestrictions().Labels.String())
		return false, nil, nil
	})
	return &asked
}

func TestListSelectsByTheWireKey(t *testing.T) {
	const mine, foreign, legacy = "gonk-triage-p42-i8-a1", "gonk-triage-p42-i9-a1", "gonk-triage-p42-i7-a1"
	job := func(name string, labels map[string]string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gonk", Labels: labels}}
	}
	r, cs := newTestRuntime(t,
		job(mine, map[string]string{wireComponentKey: wireComponentVal, wireSessionKey: mine}),
		// The same NAME part under another domain: what a gate built before a
		// domain change left behind. It is not this runtime's object.
		job(legacy, map[string]string{"sessions.example.test/component": wireComponentVal, "sessions.example.test/session": legacy}),
		// The right key with another value is not a session either.
		job(foreign, map[string]string{wireComponentKey: "something-else"}),
	)
	asked := labelSelectorsAsked(cs, "jobs")
	got, err := r.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []runtime.Handle{mine}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("List = %v, want %v: a Job carrying the wire keys must be listed, and only that one", got, want)
	}
	if want := wireComponentKey + "=" + wireComponentVal; len(*asked) != 1 || (*asked)[0] != want {
		t.Errorf("List asked the API for selector(s) %q, want exactly [%q]", *asked, want)
	}
}

func TestSessionPodIsFoundByTheWireKey(t *testing.T) {
	const n = "gonk-triage-p42-i7-a1"
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: n, Namespace: "gonk", UID: "u1",
		Labels: map[string]string{wireComponentKey: wireComponentVal, wireSessionKey: n},
	}}
	pod := func(name string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "gonk", Labels: labels, CreationTimestamp: metav1.NewTime(t0),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: n, UID: "u1", Controller: ptr.To(true),
			}},
		}}
	}
	r, cs := newTestRuntime(t, j,
		pod(n+"-wire", map[string]string{wireSessionKey: n}),
		// Owned by the same Job, but labelled under another domain: the
		// selector must not reach it.
		pod(n+"-zzzzz-other-domain", map[string]string{"sessions.example.test/session": n}),
	)
	asked := labelSelectorsAsked(cs, "pods")
	var readPod string
	r.readLog = func(_ context.Context, _, pod, _ string) (io.ReadCloser, error) {
		readPod = pod
		return io.NopCloser(strings.NewReader("x\n")), nil
	}
	if _, err := r.Output(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if readPod != n+"-wire" {
		t.Errorf("Output read pod %q, want %q (the pod carrying %s)", readPod, n+"-wire", wireSessionKey)
	}
	if want := wireSessionKey + "=" + n; len(*asked) == 0 || (*asked)[0] != want {
		t.Errorf("the pod lookup asked the API for selector(s) %q, want %q", *asked, want)
	}
}
