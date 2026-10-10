package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/gcapi/gcapitest"
)

// POD-LEVEL RECOVERY (gonk-mfsr).
//
// EVERY TEST HERE BUT ONE IS ABOUT WHAT MUST NOT BE DELETED. That asymmetry is
// the same one reap_test.go opens with, and it is stronger here: runReap closes
// a session and lets gascity tear the pod down in order; this DELETES A POD. A
// leaked pod costs money. A wrongly deleted pod kills a live agent mid-turn,
// destroys work it has not written and burns a ladder attempt.
//
// The Kubernetes side is an httptest server speaking the real API shapes to a
// real client-go clientset -- not a stub of our own interface. The interface is
// the thing under test at the seam; the wire is where a wrong label selector, a
// wrong namespace or a mishandled error actually bites.

const podReapNS = "gonk"

// fakeAPIServer is a Kubernetes apiserver with exactly the two routes the pod
// reaper uses: LIST pods in a namespace and DELETE one by name.
type fakeAPIServer struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// pods is the namespace's contents, keyed by pod name.
	pods map[string]*corev1.Pod
	// Deleted is append-only, in request order.
	Deleted []string
	// Selectors is every labelSelector a list request carried.
	Selectors []string
	// Paths is every request path, so a test can assert the NAMESPACE the
	// reaper used -- an empty one would make these cluster-scoped.
	Paths []string
	// ListFails makes every list answer 500.
	ListFails bool
	// DeleteFails names pods whose delete answers 500.
	DeleteFails map[string]bool
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{t: t, pods: map[string]*corev1.Pod{}, DeleteFails: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// AddPod puts an agent pod in the namespace. alias goes into the `gc-agent`
// label, which is where GASCITY_REF internal/runtime/k8s/pod.go puts
// SanitizeLabel(GC_ALIAS).
func (f *fakeAPIServer) AddPod(name, alias string, createdAt time.Time) *corev1.Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         podReapNS,
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels: map[string]string{
				"app":        "gc-agent",
				"gc-session": "s-" + name,
				"gc-agent":   alias,
			},
			Annotations: map[string]string{"gc-session-name": "s-" + name},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	f.pods[name] = p
	return p
}

// Names is every pod still in the namespace, sorted -- THIS is what a leak test
// asserts on, not whether a delete call was made.
func (f *fakeAPIServer) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.pods {
		out = append(out, n)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (f *fakeAPIServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Paths = append(f.Paths, r.URL.Path)
	f.mu.Unlock()

	const prefix = "/api/v1/namespaces/" + podReapNS + "/pods"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == prefix:
		f.mu.Lock()
		f.Selectors = append(f.Selectors, r.URL.Query().Get("labelSelector"))
		fails := f.ListFails
		var items []corev1.Pod
		for _, p := range f.pods {
			items = append(items, *p)
		}
		f.mu.Unlock()
		if fails {
			writeStatus(w, http.StatusInternalServerError, "injected list failure")
			return
		}
		sortPods(items)
		writeJSON(w, http.StatusOK, &corev1.PodList{
			TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
			Items:    items,
		})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, prefix+"/"):
		name := strings.TrimPrefix(r.URL.Path, prefix+"/")
		f.mu.Lock()
		if f.DeleteFails[name] {
			f.mu.Unlock()
			writeStatus(w, http.StatusInternalServerError, "injected delete failure for "+name)
			return
		}
		if _, ok := f.pods[name]; !ok {
			f.mu.Unlock()
			writeStatus(w, http.StatusNotFound, "pods \""+name+"\" not found")
			return
		}
		delete(f.pods, name)
		f.Deleted = append(f.Deleted, name)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, &metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   "Success",
		})
	default:
		writeStatus(w, http.StatusNotFound, "no route for "+r.Method+" "+r.URL.Path)
	}
}

func sortPods(p []corev1.Pod) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].Name < p[j-1].Name; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, &metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   "Failure",
		Message:  msg,
		Code:     int32(code),
	})
}

// Reaper builds the PRODUCTION k8sPodReaper against this fake, so the
// label selector, the namespace in the path and the error handling are all the
// real ones.
func (f *fakeAPIServer) Reaper(t *testing.T) *k8sPodReaper {
	t.Helper()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: f.srv.URL})
	if err != nil {
		t.Fatalf("build clientset against the fake apiserver: %v", err)
	}
	return &k8sPodReaper{cs: cs, namespace: podReapNS}
}

// podAge returns a creation timestamp `d` before the frozen reap clock.
func podAge(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, reapNow)
	if err != nil {
		t.Fatalf("bad test clock: %v", err)
	}
	return now.Add(-d)
}

// podReapDeps is reapDeps plus a pod reaper pointed at the fake apiserver.
func podReapDeps(t *testing.T, gc *gcapitest.Server, store beadstore.Store, f *fakeAPIServer, log *strings.Builder) reaperDeps {
	t.Helper()
	d := reapDeps(t, gc, store, log)
	d.Pods = f.Reaper(t)
	return d
}

// ---------------------------------------------------------------------------
// THE LEAK, AND ITS RECOVERY
// ---------------------------------------------------------------------------

// THE CASE THIS WHOLE FILE EXISTS FOR (gonk-mfsr, from gonk-wmb7).
//
// gascity rolled a pending session create back -- closing the session bead --
// and then started the runtime anyway one second later. Observed 2026-09-21:
// pod s-go-whg1, bead closed 06:32:34, creation_complete_at 06:32:35, Running
// and holding 500m/1Gi half an hour later.
//
// Every bead-level path is blind to it: the session list is the OPEN-bead list
// so the reaper never hears of it, and close/stop/kill resolve with
// allowClosed=false so they 404 forever. The pod list is the only thing that
// can see it, and deleting the pod is the only thing that can recover it.
func TestPodReaperDeletesALeakedPodWhoseSessionBeadWasAlreadyClosed(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	// The state gonk-wmb7 was found in: absent from the session list, 404 on
	// close, pod very much alive.
	gc.BeadClosedPodAlive(alias)

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))

	var log strings.Builder
	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, &log))

	if got := f.Names(); len(got) != 0 {
		t.Fatalf("THE LEAKED POD SURVIVED: %v still in the namespace.\nlog:\n%s", got, log.String())
	}
	if len(f.Deleted) != 1 || f.Deleted[0] != "s-go-whg1" {
		t.Fatalf("Deleted = %v, want [s-go-whg1]", f.Deleted)
	}
	if !strings.Contains(log.String(), "DELETED a LEAKED agent pod") {
		t.Fatalf("a pod deletion must be audible. log:\n%s", log.String())
	}
}

// ---------------------------------------------------------------------------
// THE BOUNDS
// ---------------------------------------------------------------------------

// THE LOAD-BEARING ASSERTION, and the one worth more than every leak this
// recovers. A StateRunning record claiming this alias means a live agent is
// working in that pod right now.
func TestPodReaperNeverDeletesAPodARunningBeadClaims(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	gc.BeadClosedPodAlive(alias) // even here: bead closed, close 404s, pod old

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(42)
	rec.SessionID = alias
	rec.State = beadstore.StateRunning
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	runReap(context.Background(), podReapDeps(t, gc, store, f, nil))

	if got := f.Names(); len(got) != 1 || got[0] != "s-go-whg1" {
		t.Fatalf("THE REAPER DELETED A LIVE AGENT'S POD. remaining = %v", got)
	}
}

// The grace window, which is the same one runReap uses and for the same reason:
// a dispatch creates the session FIRST and writes its record only after prompt
// delivery, so for that whole window a perfectly healthy pod has no record and
// is indistinguishable from a leak.
func TestPodReaperLeavesYoungPodsAlone(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	gc.BeadClosedPodAlive(alias)

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, time.Minute))

	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, nil))

	if got := f.Names(); len(got) != 1 {
		t.Fatal("the pod reaper deleted a pod a dispatch was still setting up: " +
			"the pod precedes the record by the whole prompt-delivery window")
	}
	if n := gc.CloseAttemptCount(); n != 0 {
		t.Fatalf("close attempts = %d, want 0: the grace check must come BEFORE we "+
			"start closing things on a pod's say-so", n)
	}
}

// Match on OUR alias scheme only. The namespace holds agent pods gonk did not
// create -- Gas City's own control-dispatcher, a pool session, a human's ad-hoc
// probe -- and every one of them carries app=gc-agent too. Deleting one of
// those is not a leak fix, it is an outage.
func TestPodReaperNeverDeletesPodsGonkDidNotCreate(t *testing.T) {
	gc := gcapitest.New(t)
	f := newFakeAPIServer(t)
	for name, alias := range map[string]string{
		"s-control-dispatcher": "control-dispatcher",
		"s-pool-1":             "scaffold-1",
		"s-adhoc":              "triage-adhoc-58f5c50f2a",
		"s-nearmiss":           "gonkish.triage.p1.a1." + testNonce,
		"s-prenonce":           "gonk.triage.p1.a1", // pre-nonce: nothing mints it now
		"s-nolabel":            "",                  // no gc-agent label at all
	} {
		f.AddPod(name, alias, podAge(t, 30*time.Minute))
	}

	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, nil))

	if got := f.Names(); len(got) != 6 {
		t.Fatalf("the pod reaper deleted pods gonk did not create. survivors = %v", got)
	}
	if n := gc.CloseAttemptCount(); n != 0 {
		t.Fatalf("close attempts = %d, want 0: a pod that is not ours must not even be "+
			"mentioned to gascity", n)
	}
}

// IF WE CANNOT READ THE BEAD STORE WE KNOW NOTHING ABOUT WHAT IS LIVE, and
// every pod in the namespace looks unclaimed. runReap already refuses to reap a
// SESSION on that read; the pod path must be behind the same refusal, because
// it is strictly more destructive.
func TestPodReaperDeletesNothingWhenTheBeadStoreCannotBeRead(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	gc.BeadClosedPodAlive(alias)

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))

	var log strings.Builder
	runReap(context.Background(), podReapDeps(t, gc, failingStore{}, f, &log))

	if got := f.Names(); len(got) != 1 {
		t.Fatal("THE POD REAPER DELETED PODS WITHOUT KNOWING WHICH WERE LIVE. " +
			"An unreadable bead store makes every pod look unclaimed.")
	}
	if len(f.Paths) != 0 {
		t.Fatalf("the pod list was read anyway (%v); the store guard must come first", f.Paths)
	}
	if !strings.Contains(log.String(), "reap: cannot read the bead store") {
		t.Fatalf("failing closed must be reported. log:\n%s", log.String())
	}
}

// AND THE SAME FOR THE POD LIST. A list that errored is not a namespace that is
// empty. There is no partial mode here: one error, zero deletions.
//
// This half is the WIRE half: a real 500 from the apiserver, through real
// client-go, must come back to runPodReap as an error rather than as an empty
// namespace. It cannot, on its own, prove the `return` that follows the error is
// there -- see TestPodReaperDeletesNothingWhenThePodListErrsWithPodsAnyway for
// that -- so it asserts what it CAN see: nothing was deleted, gascity was never
// asked about anything, and the refusal was said out loud.
func TestPodReaperDeletesNothingWhenThePodListFails(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	gc.BeadClosedPodAlive(alias)

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))
	f.ListFails = true

	var log strings.Builder
	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, &log))

	if got := f.Names(); len(got) != 1 {
		t.Fatalf("pods were deleted on a failed list: remaining = %v", got)
	}
	if len(f.Deleted) != 0 {
		t.Fatalf("Deleted = %v, want none", f.Deleted)
	}
	if n := gc.CloseAttemptCount(); n != 0 {
		t.Fatalf("gascity was asked to close %d session(s) after the pod list failed; "+
			"a tick that could not read the pod list must not act on pods at all", n)
	}
	if !strings.Contains(log.String(), "cannot list agent pods; deleting NOTHING this tick") {
		t.Fatalf("failing closed must be reported. log:\n%s", log.String())
	}
}

// poisonedLister hands back BOTH pods and an error.
//
// It exists because the wire test above CANNOT be armed (gonk-dsb8): a real 500
// makes client-go return nil pods, so the loop after the error is empty whether
// or not the `return` is there, and deleting that `return` left the test GREEN.
// Its store-read twin does not have that weakness -- it asserts that the pod
// list was never even requested -- and this restores the same shape here.
//
// Returning both is not a fiction about client-go; it is the CONTRACT
// podLister's doc states ("An error means the reaper deletes NOTHING this
// tick"). A future lister that retries, or that pages and fails on page two,
// would return exactly this, and the guard must hold for it.
//
// DeletePod is delegated to the real reaper, so a pod this tick decides to
// delete really does leave the fake namespace and really does show up as a
// request path.
type poisonedLister struct {
	podLister
	pods []agentPod
	err  error
}

func (p poisonedLister) ListAgentPods(context.Context) ([]agentPod, error) {
	return p.pods, p.err
}

// THE GUARD ITSELF: the error MUST stop the tick, not merely happen during it.
//
// Same shape as TestPodReaperDeletesNothingWhenTheBeadStoreCannotBeRead --
// nothing deleted AND nothing downstream reached. Downstream of the list is the
// apiserver (any request path at all would be a DELETE here, since the list is
// stubbed out) and gascity (CloseSession is the reaper's last gate).
func TestPodReaperDeletesNothingWhenThePodListErrsWithPodsAnyway(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	// The leak shape: no open bead, so close 404s and the pod would be DELETED
	// if this tick were allowed to proceed.
	gc.BeadClosedPodAlive(alias)

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))

	var log strings.Builder
	d := podReapDeps(t, gc, beadstore.NewMemory(), f, &log)
	d.Pods = poisonedLister{
		podLister: d.Pods,
		pods: []agentPod{{
			Name:      "s-go-whg1",
			Alias:     alias,
			CreatedAt: podAge(t, 30*time.Minute),
			Phase:     "Running",
		}},
		err: errors.New("injected list failure with a partial page"),
	}
	runReap(context.Background(), d)

	if got := f.Names(); len(got) != 1 {
		t.Fatalf("THE POD REAPER ACTED ON A LIST IT KNEW HAD FAILED: remaining = %v.\nlog:\n%s",
			got, log.String())
	}
	if len(f.Deleted) != 0 {
		t.Fatalf("Deleted = %v, want none", f.Deleted)
	}
	if len(f.Paths) != 0 {
		t.Fatalf("the apiserver was called anyway (%v); a failed list must end the tick "+
			"before anything reaches the cluster", f.Paths)
	}
	if n := gc.CloseAttemptCount(); n != 0 {
		t.Fatalf("gascity was asked to close %d session(s) from a failed pod list; "+
			"the pods it named are not a list, they are debris of one", n)
	}
	if !strings.Contains(log.String(), "cannot list agent pods; deleting NOTHING this tick") {
		t.Fatalf("failing closed must be reported. log:\n%s", log.String())
	}
}

// PREFER GASCITY'S OWN CLOSE. Pod deletion is the LAST resort and only for a pod
// gascity will not own. When close SUCCEEDS, gascity's provider stops the
// runtime and reclaims the pod AND the per-session ConfigMaps; deleting the pod
// under it would strand the rest.
//
// The session here is one the list omits but close still accepts -- the shape a
// stale or partial list produces, which is exactly when this gate earns its
// keep.
func TestPodReaperDoesNotDeleteAPodGasCityStillOwns(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	gc.CreateSessionAt(alias, gcapitest.SessionClosed, "2026-08-01T00:00:00Z")

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))

	var log strings.Builder
	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, &log))

	if len(gc.Closed) != 1 || gc.Closed[0] != alias {
		t.Fatalf("gascity was not asked to close first: Closed = %v", gc.Closed)
	}
	if got := f.Names(); len(got) != 1 {
		t.Fatalf("the pod was deleted although gascity accepted the close: remaining = %v", got)
	}
	if !strings.Contains(log.String(), "gascity still owned it") {
		t.Fatalf("the close-instead-of-delete path must say so. log:\n%s", log.String())
	}
}

// A pod whose alias the session list STILL RETURNS is open-bead and belongs to
// runReap's session loop, which has already judged it against the same claim set
// and the same grace. The pod path must not second-guess that -- in particular
// it must not delete a session the loop deliberately left alone.
func TestPodReaperLeavesAPodWhoseSessionGasCityStillLists(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	// Listed and YOUNG: the session loop leaves it alone on the grace window.
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-03T11:59:00Z")

	f := newFakeAPIServer(t)
	// The POD, however, is old -- a shape the pod path would otherwise act on.
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))

	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, nil))

	if got := f.Names(); len(got) != 1 {
		t.Fatalf("the pod path overrode the session loop's decision to wait: remaining = %v", got)
	}
	if n := gc.CloseAttemptCount(); n != 0 {
		t.Fatalf("close attempts = %d, want 0", n)
	}
}

// An ambiguous answer from gascity -- a 5xx, a timeout, a rejected grant -- is
// not a 404. We do not know whether gascity owns this pod, and "do not know"
// means do nothing.
func TestPodReaperDoesNotDeleteWhenGasCityAnswersAmbiguously(t *testing.T) {
	gc := gcapitest.New(t)
	gc.CloseFail = 1 // 401, not 404
	alias := brokerSessionAlias("triage", 42, 79, 1)
	gc.CreateSessionAt(alias, gcapitest.SessionClosed, "2026-08-01T00:00:00Z")

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))

	var log strings.Builder
	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, &log))

	if got := f.Names(); len(got) != 1 {
		t.Fatalf("a pod was deleted on an ambiguous close error: remaining = %v", got)
	}
	if !strings.Contains(log.String(), "NOT deleting it") {
		t.Fatalf("an ambiguous answer must be reported. log:\n%s", log.String())
	}
}

// A DELETE THAT FAILS IS A LEAK THAT PERSISTS, and it must be as loud as
// gonk-wmb7 made the close 404. Silence here is the failure mode that cost
// twenty minutes of a running pod with nothing in the log.
func TestPodReaperReportsADeleteItCouldNotPerform(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	gc.BeadClosedPodAlive(alias)

	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))
	f.DeleteFails["s-go-whg1"] = true

	var log strings.Builder
	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, &log))

	if got := f.Names(); len(got) != 1 {
		t.Fatalf("the fake reported the delete as failed but the pod is gone: %v", got)
	}
	got := log.String()
	if !strings.Contains(got, "could not delete a LEAKED agent pod -- IT IS STILL RUNNING") {
		t.Fatalf("a failed delete must name the leak. log:\n%s", got)
	}
	if !strings.Contains(got, "s-go-whg1") || !strings.Contains(got, alias) {
		t.Fatalf("a failed delete must name the pod AND the session. log:\n%s", got)
	}
}

// A pod already terminating needs no second delete, and a pod in a TERMINAL
// phase holds no agent at all -- its logs are the only surviving record of what
// the session did, and reclaiming those is gascity's teardown's job.
func TestPodReaperSkipsTerminatingAndTerminalPods(t *testing.T) {
	gc := gcapitest.New(t)
	f := newFakeAPIServer(t)

	terminating := brokerSessionAlias("triage", 42, 1, 1)
	gc.BeadClosedPodAlive(terminating)
	p := f.AddPod("s-terminating", terminating, podAge(t, 30*time.Minute))
	now := metav1.NewTime(podAge(t, time.Minute))
	p.DeletionTimestamp = &now

	for name, alias := range map[string]corev1.PodPhase{
		"s-succeeded": corev1.PodSucceeded,
		"s-failed":    corev1.PodFailed,
	} {
		a := brokerSessionAlias("triage", 42, 2, 1)
		gc.BeadClosedPodAlive(a)
		q := f.AddPod(name, a, podAge(t, 30*time.Minute))
		q.Status.Phase = alias
	}

	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, nil))

	if got := f.Names(); len(got) != 3 {
		t.Fatalf("a terminating or terminal pod was deleted: remaining = %v", got)
	}
}

// Unknown age fails safe, exactly as it does for a session's unparseable
// created_at: a zero creation timestamp must read as "too young to touch", never
// as "year 1, therefore ancient".
func TestPodReaperTreatsAPodWithNoCreationTimestampAsTooYoung(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	gc.BeadClosedPodAlive(alias)

	f := newFakeAPIServer(t)
	p := f.AddPod("s-go-whg1", alias, podAge(t, 30*time.Minute))
	p.CreationTimestamp = metav1.Time{}

	var log strings.Builder
	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, &log))

	if got := f.Names(); len(got) != 1 {
		t.Fatal("a pod whose age could not be read was deleted; unknown age must fail safe")
	}
	if !strings.Contains(log.String(), "no readable creation timestamp") {
		t.Fatalf("an unreadable age must be reported. log:\n%s", log.String())
	}
}

// ---------------------------------------------------------------------------
// THE CONFIGURATION, WHICH IS THE ARMING MECHANISM
// ---------------------------------------------------------------------------

// OFF IS THE DEFAULT AND OFF MEANS SILENT. With no pod reaper configured the
// sweep must behave exactly as it did before gonk-mfsr: no pod read, no pod
// delete, and -- just as important -- nothing in the log. A permanent ERROR
// line about a feature nobody enabled teaches operators to ignore the reaper's
// log, which is the one place a real leak announces itself.
func TestPodReaperDoesNothingAndSaysNothingWhenNotConfigured(t *testing.T) {
	gc := gcapitest.New(t)
	gc.BeadClosedPodAlive(brokerSessionAlias("triage", 42, 79, 1))

	var log strings.Builder
	d := reapDeps(t, gc, beadstore.NewMemory(), &log)
	if d.Pods != nil {
		t.Fatal("sweepDeps.Pods must default to nil")
	}
	runReap(context.Background(), d)

	if got := log.String(); strings.Contains(got, "pod") || strings.Contains(got, "Pod") {
		t.Fatalf("an unconfigured pod reaper said something. log:\n%s", got)
	}
}

// THE TYPED-NIL TRAP, PINNED. newPodReaper returns a *k8sPodReaper, and a nil
// one assigned straight into an interface field is a NON-nil interface holding
// a nil pointer. That would make every tick on every install with the feature
// off log `cannot list agent pods; deleting NOTHING this tick` forever.
func TestPodReaperOrNilYieldsATrulyNilInterface(t *testing.T) {
	if got := podReaperOrNil(nil); got != nil {
		t.Fatal("podReaperOrNil(nil) is not nil: sweepDeps.Pods would be a non-nil " +
			"interface holding a nil pointer, and every sweep tick would log a " +
			"pod-list failure for a feature nobody turned on")
	}
	if got := podReaperOrNil(&k8sPodReaper{namespace: podReapNS}); got == nil {
		t.Fatal("podReaperOrNil dropped a real reaper")
	}
}

// AN EMPTY NAMESPACE IS NOT "THE CURRENT ONE", IT IS CLUSTER-SCOPED -- the
// gonk-8gb lesson, where an unset GONK_KEYSINK_NAMESPACE turned a namespaced
// Secret read into a cluster-scoped one. A cluster-scoped pod LIST plus DELETE
// is the exact blast radius this feature must never have, so the namespace and
// the on-switch are deliberately the same variable.
// The builder is injected because OFF A CLUSTER inClusterClientset fails for
// every namespace, so calling newPodReaper("") here would return nil whether or
// not the refusal exists -- a green test asserting nothing about the guard that
// stands between this code and a cluster-scoped list and delete.
func TestPodReaperRefusesToExistWithoutANamespace(t *testing.T) {
	built := 0
	ok := func() (kubernetes.Interface, error) {
		built++
		return fakeClientset(t), nil
	}

	if got := newPodReaperWith("", ok); got != nil {
		t.Fatal("newPodReaperWith(\"\", ...) returned a reaper: an empty namespace makes " +
			"client-go issue CLUSTER-SCOPED pod list and delete calls")
	}
	if built != 0 {
		t.Fatal("an empty namespace still built a clientset; the refusal must come first")
	}
	if got := newPodReaperWith(podReapNS, ok); got == nil || got.namespace != podReapNS {
		t.Fatalf("newPodReaperWith(%q, ok) = %+v, want a reaper in that namespace", podReapNS, got)
	}
}

// AND THE SAME REFUSAL AT THE CALL SITES, because a reaper can also be built by
// hand (this file does it, and so could a future caller). A cluster-scoped list
// or delete must be unreachable, not merely unreached.
//
// The assertion that matters is NOT that the calls error -- they would error
// against any unreachable apiserver, guard or no guard. It is that NO REQUEST
// LEFT THE PROCESS: a cluster-scoped `/api/v1/pods` list must be unreachable,
// not merely unreached.
func TestPodReaperRefusesToActWithoutANamespace(t *testing.T) {
	f := newFakeAPIServer(t)
	k := &k8sPodReaper{cs: f.Reaper(t).cs, namespace: ""}

	if _, err := k.ListAgentPods(context.Background()); err == nil {
		t.Fatal("ListAgentPods with an empty namespace succeeded; that is a CLUSTER-SCOPED list")
	}
	if err := k.DeletePod(context.Background(), "s-go-whg1"); err == nil {
		t.Fatal("DeletePod with an empty namespace succeeded; that is a CLUSTER-SCOPED delete")
	}
	if len(f.Paths) != 0 {
		t.Fatalf("a namespace-less request reached the apiserver: %v. An empty namespace "+
			"does not mean the current one -- it makes the request CLUSTER-SCOPED.", f.Paths)
	}
}

func fakeClientset(t *testing.T) kubernetes.Interface {
	t.Helper()
	return newFakeAPIServer(t).Reaper(t).cs
}

// THE READ IS NAMESPACED AND SELECTED. A wrong selector would walk pods that
// are not agent sessions; a wrong (or empty) namespace would walk the cluster.
// Both render fine and both are only visible on the wire.
func TestPodReaperListsOnlyAgentPodsInItsOwnNamespace(t *testing.T) {
	gc := gcapitest.New(t)
	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", brokerSessionAlias("triage", 42, 79, 1), podAge(t, 30*time.Minute))

	runReap(context.Background(), podReapDeps(t, gc, beadstore.NewMemory(), f, nil))

	if len(f.Selectors) != 1 || f.Selectors[0] != "app=gc-agent" {
		t.Fatalf("labelSelector = %v, want [app=gc-agent]", f.Selectors)
	}
	for _, p := range f.Paths {
		if !strings.HasPrefix(p, "/api/v1/namespaces/"+podReapNS+"/pods") {
			t.Fatalf("request path %q is not namespaced to %s", p, podReapNS)
		}
	}
}

// ---------------------------------------------------------------------------
// THE JOIN'S PREMISE
// ---------------------------------------------------------------------------

// k8sLabelValue is Kubernetes' own label-value grammar: at most 63 characters,
// alphanumerics plus '-', '_' and '.', starting and ending alphanumeric.
var k8sLabelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// THE PREMISE THE WHOLE JOIN RESTS ON. The pod's `gc-agent` label is
// SanitizeLabel(GC_ALIAS) (GASCITY_REF internal/runtime/k8s/pod.go), and
// SanitizeLabel is the IDENTITY on any string that is already a valid label
// value of at most 63 characters. So for every alias brokerSessionAlias mints,
// the label equals the alias, and claimedSessionIDs -- which keys on
// rec.SessionID, the same alias -- joins against it unchanged.
//
// THE BUDGET IS 63 CHARACTERS AND IT IS NOT GENEROUS. A 26-character nonce plus
// `gonk.` plus the p/i/a segments leaves 25 characters for the agent name and
// all three numbers together. Today's agents are `triage` and `scaffold`, so a
// 7-digit project and a 5-digit issue still fit comfortably -- but a 27-character
// agent name blows the limit outright (measured: 79 characters), and the pod
// reaper would then skip every pod that agent ever creates, silently.
//
// So this iterates agentForTrigger -- the real map -- rather than a list a test
// author chose. Adding an agent whose name does not fit goes RED HERE, at the
// moment the name is chosen, instead of showing up as a leak nobody recovers.
func TestBrokerAliasesSurviveKubernetesLabelSanitisationUnchanged(t *testing.T) {
	// Generously large but realistic ids: a 7-digit project, a 5-digit issue,
	// a 2-digit attempt. GitLab's own instance-wide project ids and per-project
	// issue iids are far below these on this forge.
	const bigProject, bigIssue, bigAttempt = 9999999, 99999, 99

	for trigger, agent := range agentForTrigger {
		for _, alias := range []string{
			brokerSessionAlias(agent, bigProject, bigIssue, bigAttempt),
			brokerSessionAlias(agent, bigProject, 0, bigAttempt), // project-scoped form
		} {
			if !gonkSessionAlias.MatchString(alias) {
				t.Fatalf("brokerSessionAlias minted %q for trigger %q, which gonkSessionAlias "+
					"does not match", alias, trigger)
			}
			if n := len(alias); n > 63 {
				t.Errorf("agent %q (trigger %q) mints a %d-character alias %q. Kubernetes "+
					"truncates a label value at 63, which cuts the nonce, so the gc-agent "+
					"label stops equalling rec.SessionID and the pod reaper skips every pod "+
					"this agent creates -- silently. Shorten the agent name.",
					agent, trigger, n, alias)
			}
			if !k8sLabelValue.MatchString(alias) {
				t.Errorf("alias %q is not a valid Kubernetes label value, so SanitizeLabel "+
					"would rewrite it and the gc-agent label would no longer equal "+
					"rec.SessionID", alias)
			}
		}
	}
}

// And the budget itself, stated as arithmetic rather than left implicit in the
// test above: 63 - len("gonk.") - len(".pN") - len(".iN") - len(".aN") - 26.
// A reader who wants to know how much room a new agent name has reads this.
func TestTheAliasLabelBudgetIsWhatThePodReaperThinksItIs(t *testing.T) {
	alias := brokerSessionAlias("x", 1, 1, 1)
	// "gonk.x.p1.i1.a1." + 26 = 16 + 26 = 42, so one-character agent, all
	// one-digit numbers.
	if len(alias) != 42 {
		t.Fatalf("minimal alias %q is %d characters, want 42; the budget arithmetic in "+
			"reap_pods.go is stale", alias, len(alias))
	}
	// 63 - (42 - 1 /* the agent's own character */) = 22 characters to share
	// between the agent name and the extra digits of the three numbers.
	const budget = 63 - 41
	if budget != 22 {
		t.Fatalf("budget arithmetic wrong: %d", budget)
	}
}

// A guard against the fake drifting from the real API: the production reaper
// must read back exactly the fields the decision rests on.
func TestListAgentPodsReadsTheFieldsTheDecisionRestsOn(t *testing.T) {
	f := newFakeAPIServer(t)
	alias := brokerSessionAlias("triage", 42, 79, 1)
	created := podAge(t, 30*time.Minute)
	p := f.AddPod("s-go-whg1", alias, created)
	p.Status.Phase = corev1.PodPending

	pods, err := f.Reaper(t).ListAgentPods(context.Background())
	if err != nil {
		t.Fatalf("ListAgentPods: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("got %d pods, want 1", len(pods))
	}
	got := pods[0]
	want := agentPod{Name: "s-go-whg1", Alias: alias, CreatedAt: created.UTC(), Phase: "Pending"}
	if got.Name != want.Name || got.Alias != want.Alias || got.Phase != want.Phase ||
		!got.CreatedAt.Equal(want.CreatedAt) || got.Terminating {
		t.Fatalf("ListAgentPods = %+v, want %+v (Terminating false)", got, want)
	}
}

// A delete that the apiserver refuses must come back as an error naming the
// namespace and the pod, not as a silent success.
func TestDeletePodSurfacesTheApiserverError(t *testing.T) {
	f := newFakeAPIServer(t)
	f.AddPod("s-go-whg1", brokerSessionAlias("triage", 42, 79, 1), podAge(t, time.Hour))
	f.DeleteFails["s-go-whg1"] = true

	err := f.Reaper(t).DeletePod(context.Background(), "s-go-whg1")
	if err == nil {
		t.Fatal("DeletePod reported success on a 500")
	}
	if !strings.Contains(err.Error(), podReapNS+"/s-go-whg1") {
		t.Fatalf("delete error %q does not name the namespace/pod", err)
	}
	if got := f.Names(); len(got) != 1 {
		t.Fatalf("the pod is gone after a refused delete: %v", got)
	}
}

func TestDeletePodRefusesAnEmptyName(t *testing.T) {
	f := newFakeAPIServer(t)
	if err := f.Reaper(t).DeletePod(context.Background(), ""); err == nil {
		t.Fatal("DeletePod(\"\") succeeded; an empty name is a request shape we must never send")
	}
	if len(f.Paths) != 0 {
		t.Fatalf("an empty-name delete reached the apiserver: %v", f.Paths)
	}
}
