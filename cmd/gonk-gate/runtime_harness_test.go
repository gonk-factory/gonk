package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gonk-factory/gonk/pkg/runtime"
	"github.com/gonk-factory/gonk/pkg/runtime/fake"
)

// TestMain points dispatch's default pack at the repository's own pack/ --
// the directory images/Dockerfile.controller bakes into /opt/gonk/pack -- so
// every dispatch test builds its session env from the real agent.toml files
// (plan D5) rather than from a fixture that could drift from them.
func TestMain(m *testing.M) {
	defaultPackDir = repoPackDir
	os.Exit(m.Run())
}

// testSession is the session name dispatch mints for baseDispatchArgs' bead at
// attempt 1, and the one the sweep fixtures record: gonk-triage-p42-i3-a1.
var testSession = runtime.Name("triage", 42, runtime.KindIssue, 3, 1)

// testRuntime is pkg/runtime/fake plus the fault injection and bookkeeping the
// gonk-gate tests need and the fake deliberately does not carry: a Stop that
// fails or hangs, a Status or List that errors, a count of Stop requests, and
// a hook at the start of Start. Everything it does not override is the fake's,
// with the fake's contract (idempotent Start by name, ErrNotFound on an
// unknown Stop/Output, PhaseNotFound on an unknown Status).
type testRuntime struct {
	*fake.Runtime

	mu sync.Mutex
	// stopErr fails every Stop (after recording it) without stopping anything.
	stopErr error
	// stopHangs makes every Stop block until its context ends, stopping
	// nothing -- a teardown slower than any budget.
	stopHangs bool
	// statusErr fails every Status.
	statusErr error
	// listErr fails every List.
	listErr error
	// stops records every Stop REQUEST, whatever its outcome.
	stops []runtime.Handle
	// starts records every Start that reached this wrapper past onStart --
	// i.e. every Start the code under test made -- whatever its outcome.
	// Sessions a test seeds with startNamed & co. go to the fake directly and
	// are NOT here; see Starts.
	starts []runtime.Spec
	// onStart, when set, runs at the start of every Start, without the lock. A
	// non-nil error fails that Start before the fake sees it (so it is not in
	// Starts()).
	onStart func(spec runtime.Spec) error
	// afterStart, when set, runs after every SUCCESSFUL Start, without the
	// lock: a point where the session is admitted and dispatch has not yet
	// written its running record.
	afterStart func(h runtime.Handle)
	// onStop, when set, runs at the start of every Stop, without the lock.
	onStop func(h runtime.Handle)
}

var _ runtime.SessionRuntime = (*testRuntime)(nil)

func newTestRuntime() *testRuntime { return &testRuntime{Runtime: fake.New()} }

func (r *testRuntime) Start(ctx context.Context, spec runtime.Spec) (runtime.Handle, bool, error) {
	r.mu.Lock()
	hook := r.onStart
	r.mu.Unlock()
	if hook != nil {
		if err := hook(spec); err != nil {
			return "", false, err
		}
	}
	r.mu.Lock()
	r.starts = append(r.starts, spec)
	r.mu.Unlock()
	if err := jobsSpecRefusal(spec); err != nil {
		return "", false, err
	}
	h, created, err := r.Runtime.Start(ctx, spec)
	r.mu.Lock()
	after := r.afterStart
	r.mu.Unlock()
	if err == nil && after != nil {
		after(h)
	}
	return h, created, err
}

func (r *testRuntime) Status(ctx context.Context, h runtime.Handle) (runtime.Status, error) {
	r.mu.Lock()
	err := r.statusErr
	r.mu.Unlock()
	if err != nil {
		return runtime.Status{}, err
	}
	return r.Runtime.Status(ctx, h)
}

func (r *testRuntime) List(ctx context.Context) ([]runtime.Handle, error) {
	r.mu.Lock()
	err := r.listErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.Runtime.List(ctx)
}

func (r *testRuntime) Stop(ctx context.Context, h runtime.Handle) error {
	r.mu.Lock()
	r.stops = append(r.stops, h)
	err, hangs, hook := r.stopErr, r.stopHangs, r.onStop
	r.mu.Unlock()
	if hook != nil {
		hook(h)
	}
	if hangs {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return r.Runtime.Stop(ctx, h)
}

// jobsSpecRefusal is what the Kubernetes Jobs runtime refuses in a Spec
// (pkg/runtime/jobs), which the in-memory fake does not check: an Env that
// sets one of the names the runtime owns, carries Gas City plumbing
// (gonk-426v), or holds the LiteLLM key's or the rig URL's value anywhere.
// Checking it here is what makes every dispatch test fail on a Spec
// production would refuse.
func jobsSpecRefusal(spec runtime.Spec) error {
	for k, v := range spec.Env {
		if slices.Contains(runtimeOwnedEnv, k) {
			return fmt.Errorf("jobs runtime would refuse: Env sets runtime-owned %s", k)
		}
		if strings.HasPrefix(k, "GC_DOLT") || strings.HasPrefix(k, "BEADS_") ||
			strings.HasPrefix(k, "GC_INSTANCE") || strings.HasPrefix(k, "GC_CITY_WRITE") {
			return fmt.Errorf("jobs runtime would refuse: Env carries %s (gonk-426v)", k)
		}
		if len(spec.LiteLLMKey) > 0 && strings.Contains(v, string(spec.LiteLLMKey)) {
			return fmt.Errorf("jobs runtime would refuse: Env %s carries the LiteLLM key", k)
		}
		if spec.RigURL != "" && strings.Contains(v, spec.RigURL) {
			return fmt.Errorf("jobs runtime would refuse: Env %s carries the rig checkout URL", k)
		}
	}
	return nil
}

// Starts shadows the fake's: every Start the CODE UNDER TEST made (see the
// starts field), in call order -- not the sessions the test seeded.
func (r *testRuntime) Starts() []runtime.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.Spec(nil), r.starts...)
}

// stopCount is how many Stop requests reached the runtime.
func (r *testRuntime) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stops)
}

// live is every session the runtime still holds, sorted.
func (r *testRuntime) live(t *testing.T) []runtime.Handle {
	t.Helper()
	hs, err := r.Runtime.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return hs
}

// startNamed admits a session under name, as a dispatch would have.
func (r *testRuntime) startNamed(t *testing.T, name string) runtime.Handle {
	t.Helper()
	h, _, err := r.Runtime.Start(context.Background(), runtime.Spec{Name: name, Agent: "triage"})
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	return h
}

// finishSession admits a session that has already Succeeded with output.
func (r *testRuntime) finishSession(t *testing.T, name, output string) runtime.Handle {
	t.Helper()
	h := r.startNamed(t, name)
	r.SetOutput(h, []byte(output))
	r.SetPhase(h, runtime.PhaseSucceeded, "")
	return h
}

// failSession admits a session that has already Failed, for reason, with output.
func (r *testRuntime) failSession(t *testing.T, name, reason, output string) runtime.Handle {
	t.Helper()
	h := r.startNamed(t, name)
	r.SetOutput(h, []byte(output))
	r.SetPhase(h, runtime.PhaseFailed, reason)
	return h
}

// runSession admits a session that is still Running, with output so far.
func (r *testRuntime) runSession(t *testing.T, name, output string) runtime.Handle {
	t.Helper()
	h := r.startNamed(t, name)
	r.SetOutput(h, []byte(output))
	r.SetPhase(h, runtime.PhaseRunning, "")
	return h
}

// firedOrder is one RunOrder call a fakeOrders accepted.
type firedOrder struct {
	Order string
	Vars  map[string]string
}

// fakeOrders is the sweep's orderRunner (plan D8): it records every order the
// sweep fired, and can refuse the next few.
type fakeOrders struct {
	mu sync.Mutex
	// fired is append-only, in call order, and holds only ACCEPTED orders.
	fired []firedOrder
	// fail is a count of remaining refusals.
	fail int
	// onRun, when set, runs at the start of every RunOrder, without the lock.
	onRun func(name string, vars map[string]string)
}

var errOrderRefused = &orderRefusedError{}

type orderRefusedError struct{}

func (*orderRefusedError) Error() string { return "fake order route: refused" }

func (f *fakeOrders) RunOrder(_ context.Context, name string, vars map[string]string) error {
	f.mu.Lock()
	hook := f.onRun
	f.mu.Unlock()
	if hook != nil {
		hook(name, vars)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errOrderRefused
	}
	cp := make(map[string]string, len(vars))
	for k, v := range vars {
		cp[k] = v
	}
	f.fired = append(f.fired, firedOrder{Order: name, Vars: cp})
	return nil
}

// orders returns a copy of every accepted order, in call order.
func (f *fakeOrders) orders() []firedOrder {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]firedOrder(nil), f.fired...)
}

// names returns the accepted orders' names, in call order.
func (f *fakeOrders) names() []string {
	out := []string{}
	for _, o := range f.orders() {
		out = append(out, o.Order)
	}
	return out
}
