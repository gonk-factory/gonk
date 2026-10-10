package fake_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/runtime"
	"github.com/gonk-factory/gonk/pkg/runtime/fake"
)

func spec(agent string, iid int64, attempt int) runtime.Spec {
	return runtime.Spec{
		Name:       runtime.Name(agent, 42, runtime.KindIssue, iid, attempt),
		Agent:      agent,
		Image:      "registry.example/agent:1",
		Env:        map[string]string{"GONK_MODEL": "m1"},
		Prompt:     []byte("prompt"),
		LiteLLMKey: []byte("sk-1"),
		Metadata:   []byte(`{"bead":"b1"}`),
		Labels:     map[string]string{"x": "1"},
	}
}

// Re-dispatching the same bead+attempt must land on the SAME session and
// change nothing about it -- not its spec, not its state, not its output.
func TestStartIsIdempotentOnName(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	first := spec("triage", 7, 1)
	h1, created1, err := r.Start(ctx, first)
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	if !created1 {
		t.Errorf("first Start reported created=false; it made the session")
	}
	if h1 != runtime.Handle(first.Name) {
		t.Fatalf("handle = %q, want the name %q", h1, first.Name)
	}
	r.SetPhase(h1, runtime.PhaseRunning, "")
	r.SetOutput(h1, []byte("partial"))

	second := spec("triage", 7, 1)
	second.Prompt = []byte("a different prompt")
	second.Env = map[string]string{"GONK_MODEL": "m2"}
	h2, created2, err := r.Start(ctx, second)
	if err != nil {
		t.Fatalf("second Start = %v, want nil (AlreadyExists is not an error)", err)
	}
	if created2 {
		t.Errorf("second Start reported created=true; it ADOPTED the first session (the Jobs runtime reports false here)")
	}
	if h2 != h1 {
		t.Fatalf("second Start handle = %q, want %q", h2, h1)
	}
	st, _ := r.Status(ctx, h1)
	if st.Phase != runtime.PhaseRunning {
		t.Errorf("after second Start, phase = %v, want Running (reset to Pending?)", st.Phase)
	}
	out, _ := r.Output(ctx, h1)
	if string(out) != "partial" {
		t.Errorf("after second Start, output = %q, want %q", out, "partial")
	}
	got, _ := r.Spec(h1)
	if string(got.Prompt) != "prompt" || got.Env["GONK_MODEL"] != "m1" {
		t.Errorf("after second Start, spec = %+v, want the FIRST spec", got)
	}
	if hs, _ := r.List(ctx); len(hs) != 1 {
		t.Errorf("List = %v, want exactly one session", hs)
	}
	if n := len(r.Starts()); n != 2 {
		t.Errorf("Starts = %d, want 2 recorded calls", n)
	}
}

// Concurrent starts of one name must still produce one session.
func TestConcurrentStartOfOneNameMakesOneSession(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := r.Start(ctx, spec("triage", 7, 1)); err != nil {
				t.Errorf("Start = %v", err)
			}
		}()
	}
	wg.Wait()
	if hs, _ := r.List(ctx); len(hs) != 1 {
		t.Fatalf("List = %v, want one session", hs)
	}
}

func TestStopThenStopReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	h, _, err := r.Start(ctx, spec("triage", 7, 1))
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	if err := r.Stop(ctx, h); err != nil {
		t.Fatalf("first Stop = %v, want nil", err)
	}
	if err := r.Stop(ctx, h); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("second Stop = %v, want ErrNotFound", err)
	}
	if st, err := r.Status(ctx, h); err != nil || st.Phase != runtime.PhaseNotFound {
		t.Fatalf("Status after Stop = %+v, %v; want NotFound, nil", st, err)
	}
	if _, err := r.Output(ctx, h); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Output after Stop = %v, want ErrNotFound", err)
	}
	if err := r.Stop(ctx, "gonk-never-started"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Stop of a never-started handle = %v, want ErrNotFound", err)
	}
	// A stopped name can be started again: a fresh session, back at Pending.
	h2, _, err := r.Start(ctx, spec("triage", 7, 1))
	if err != nil || h2 != h {
		t.Fatalf("restart = %q, %v", h2, err)
	}
	if st, _ := r.Status(ctx, h2); st.Phase != runtime.PhasePending {
		t.Fatalf("restarted phase = %v, want Pending", st.Phase)
	}
}

func TestListReturnsEveryLiveSessionSorted(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	if hs, err := r.List(ctx); err != nil || len(hs) != 0 {
		t.Fatalf("empty List = %v, %v", hs, err)
	}
	var want []runtime.Handle
	for _, s := range []runtime.Spec{spec("triage", 9, 1), spec("mention", 3, 2), spec("triage", 7, 1)} {
		h, _, err := r.Start(ctx, s)
		if err != nil {
			t.Fatalf("Start = %v", err)
		}
		want = append(want, h)
	}
	slices.Sort(want)
	got, err := r.List(ctx)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("List = %v, %v; want %v", got, err, want)
	}
	if err := r.Stop(ctx, want[1]); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	got, _ = r.List(ctx)
	if want = slices.Delete(want, 1, 2); !slices.Equal(got, want) {
		t.Fatalf("List after Stop = %v, want %v", got, want)
	}
}

func TestNewSessionIsPendingWithEmptyOutput(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	h, _, _ := r.Start(ctx, spec("triage", 7, 1))
	st, err := r.Status(ctx, h)
	if err != nil || st != (runtime.Status{Phase: runtime.PhasePending}) {
		t.Fatalf("Status = %+v, %v; want bare Pending", st, err)
	}
	out, err := r.Output(ctx, h)
	if err != nil || len(out) != 0 {
		t.Fatalf("Output = %q, %v; want empty", out, err)
	}
}

func TestSetPhaseStampsTimesOnceAndCarriesReason(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	now := t0
	r.Now = func() time.Time { return now }
	h, _, _ := r.Start(ctx, spec("triage", 7, 1))

	r.SetPhase(h, runtime.PhaseRunning, "")
	now = t0.Add(time.Minute)
	r.SetPhase(h, runtime.PhaseFailed, "DeadlineExceeded")
	now = t0.Add(2 * time.Minute)
	r.SetPhase(h, runtime.PhaseFailed, "DeadlineExceeded") // no restamp

	st, _ := r.Status(ctx, h)
	want := runtime.Status{Phase: runtime.PhaseFailed, Reason: "DeadlineExceeded", StartedAt: t0, FinishedAt: t0.Add(time.Minute)}
	if st != want {
		t.Fatalf("Status = %+v, want %+v", st, want)
	}

	full := runtime.Status{Phase: runtime.PhaseSucceeded, StartedAt: t0, FinishedAt: t0.Add(time.Hour)}
	r.SetStatus(h, full)
	if st, _ := r.Status(ctx, h); st != full {
		t.Fatalf("after SetStatus, Status = %+v, want %+v", st, full)
	}

	r.SetPhase(h, runtime.PhaseNotFound, "")
	if st, _ := r.Status(ctx, h); st.Phase != runtime.PhaseNotFound {
		t.Fatalf("after SetPhase(NotFound), phase = %v", st.Phase)
	}
	if hs, _ := r.List(ctx); len(hs) != 0 {
		t.Fatalf("after SetPhase(NotFound), List = %v, want empty", hs)
	}
}

func TestSetOutputAndOutputAreCopies(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	h, _, _ := r.Start(ctx, spec("triage", 7, 1))
	buf := []byte("batch")
	r.SetOutput(h, buf)
	buf[0] = 'X'
	out, _ := r.Output(ctx, h)
	if string(out) != "batch" {
		t.Fatalf("Output = %q; SetOutput aliased the caller's slice", out)
	}
	out[0] = 'Y'
	if again, _ := r.Output(ctx, h); !bytes.Equal(again, []byte("batch")) {
		t.Fatalf("Output = %q; Output returned the fake's own slice", again)
	}
}

func TestSpecIsCopiedInAndOut(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	s := spec("triage", 7, 1)
	h, _, _ := r.Start(ctx, s)
	s.Env["GONK_MODEL"] = "mutated"
	s.Prompt[0] = 'X'
	got, ok := r.Spec(h)
	if !ok || got.Env["GONK_MODEL"] != "m1" || string(got.Prompt) != "prompt" {
		t.Fatalf("Spec = %+v; Start aliased the caller's Spec", got)
	}
	got.Labels["x"] = "mutated"
	if again, _ := r.Spec(h); again.Labels["x"] != "1" {
		t.Fatalf("Spec returned the fake's own map")
	}
	if _, ok := r.Spec("gonk-nope"); ok {
		t.Fatalf("Spec of an unknown handle reported ok")
	}
}

func TestInjectedStartErrorAdmitsNothing(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	boom := errors.New("admission webhook said no")
	r.SetStartErr(boom)
	if _, _, err := r.Start(ctx, spec("triage", 7, 1)); !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want the injected error", err)
	}
	if hs, _ := r.List(ctx); len(hs) != 0 {
		t.Fatalf("a failed Start admitted %v", hs)
	}
	if n := len(r.Starts()); n != 1 {
		t.Fatalf("Starts = %d, want the failed call recorded", n)
	}
	r.SetStartErr(nil)
	if _, _, err := r.Start(ctx, spec("triage", 7, 1)); err != nil {
		t.Fatalf("Start after clearing = %v", err)
	}
}

func TestInjectedOutputErrorIsPerSession(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	h1, _, _ := r.Start(ctx, spec("triage", 7, 1))
	h2, _, _ := r.Start(ctx, spec("triage", 8, 1))
	r.SetOutput(h1, []byte("one"))
	r.SetOutput(h2, []byte("two"))
	boom := errors.New("container log rotated away")
	r.SetOutputErr(h1, boom)
	if _, err := r.Output(ctx, h1); !errors.Is(err, boom) {
		t.Fatalf("Output(h1) = %v, want the injected error", err)
	}
	if out, err := r.Output(ctx, h2); err != nil || string(out) != "two" {
		t.Fatalf("Output(h2) = %q, %v; the injection leaked", out, err)
	}
	r.SetOutputErr(h1, nil)
	if out, err := r.Output(ctx, h1); err != nil || string(out) != "one" {
		t.Fatalf("Output(h1) after clearing = %q, %v", out, err)
	}
}

func TestStartRefusesAnInvalidName(t *testing.T) {
	r := fake.New()
	for _, name := range []string{"", "Gonk_Triage", fmt.Sprintf("gonk-%060d", 0)} {
		s := spec("triage", 7, 1)
		s.Name = name
		if _, _, err := r.Start(context.Background(), s); err == nil {
			t.Errorf("Start(Name=%q) = nil error, want a refusal", name)
		}
	}
	if hs, _ := r.List(context.Background()); len(hs) != 0 {
		t.Fatalf("invalid names admitted: %v", hs)
	}
}

func TestCancelledContextIsHonoured(t *testing.T) {
	r := fake.New()
	h, _, _ := r.Start(context.Background(), spec("triage", 7, 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Start(ctx, spec("triage", 8, 1)); !errors.Is(err, context.Canceled) {
		t.Errorf("Start = %v", err)
	}
	if _, err := r.Status(ctx, h); !errors.Is(err, context.Canceled) {
		t.Errorf("Status = %v", err)
	}
	if _, err := r.Output(ctx, h); !errors.Is(err, context.Canceled) {
		t.Errorf("Output = %v", err)
	}
	if err := r.Stop(ctx, h); !errors.Is(err, context.Canceled) {
		t.Errorf("Stop = %v", err)
	}
	if _, err := r.List(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("List = %v", err)
	}
	if hs, _ := r.List(context.Background()); len(hs) != 1 {
		t.Errorf("cancelled calls changed state: %v", hs)
	}
}

func TestHookOnUnknownHandlePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("SetPhase on an unknown handle did not panic")
		}
	}()
	fake.New().SetPhase("gonk-nope", runtime.PhaseRunning, "")
}

// Every method under -race at once: the fake is shared by dispatch and sweep
// goroutines in gonk-gate's tests.
func TestConcurrentUseIsRaceFree(t *testing.T) {
	ctx := context.Background()
	r := fake.New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, _, err := r.Start(ctx, spec("triage", int64(i%4)+1, 1))
			if err != nil {
				t.Errorf("Start = %v", err)
				return
			}
			_, _ = r.Status(ctx, h)
			_, _ = r.Output(ctx, h)
			_, _ = r.List(ctx)
			_, _ = r.Spec(h)
			_ = r.Starts()
			_ = r.Stop(ctx, h) // may be ErrNotFound: another goroutine got there
		}(i)
	}
	wg.Wait()
}
