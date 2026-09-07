package intake

import (
	"context"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// countingSlowGL is a GitLab fake whose ListMemberProjects call reports its
// own call number over entered, then blocks until the test sends on release.
// Every other method panics -- these tests never have any projects
// (ListMemberProjects always returns an empty list), so nothing else in
// reconcileProject is ever reached.
type countingSlowGL struct {
	entered chan int
	release chan struct{}
	calls   int
}

func newCountingSlowGL() *countingSlowGL {
	return &countingSlowGL{entered: make(chan int, 8), release: make(chan struct{})}
}

func (g *countingSlowGL) ListMemberProjects(ctx context.Context) ([]glab.Project, error) {
	g.calls++
	n := g.calls
	g.entered <- n
	<-g.release
	return nil, nil
}

func (g *countingSlowGL) GetRawFile(context.Context, int64, string, string, int64) ([]byte, error) {
	panic("not used: ListMemberProjects always returns no projects in this test")
}
func (g *countingSlowGL) DirExists(context.Context, int64, string, string) (bool, error) {
	panic("not used")
}
func (g *countingSlowGL) ListHooks(context.Context, int64) ([]glab.Hook, error) { panic("not used") }
func (g *countingSlowGL) CreateHook(context.Context, int64, glab.HookOptions) (*glab.Hook, error) {
	panic("not used")
}
func (g *countingSlowGL) EditHook(context.Context, int64, int64, glab.HookOptions) (*glab.Hook, error) {
	panic("not used")
}
func (g *countingSlowGL) ListMergeRequests(context.Context, int64, glab.MRListOptions) ([]glab.MergeRequest, error) {
	panic("not used")
}
func (g *countingSlowGL) ListMembers(context.Context, int64) ([]glab.Member, error) {
	panic("not used")
}
func (g *countingSlowGL) ListIssues(context.Context, int64, glab.IssueListOptions) ([]glab.Issue, error) {
	panic("not used")
}

func newLoopTestReconciler(gl GitLab) *Reconciler {
	return &Reconciler{
		GL:    gl,
		Meter: NewMeterClient("http://127.0.0.1:1", "unused-token", nil),
		Cache: NewCache(),
		Obs:   NopObserver{},
		// NO STARTUP LADDER. Every test in this file is about Kick/
		// WaitForNextPass sequencing and counts passes by hand, so a boot pass
		// they did not ask for shifts every number by one and competes for the
		// same release channel. An EMPTY (non-nil) ladder means "no rungs",
		// which is distinct from nil ("use the default") -- see runStartupLadder.
		// The startup behaviour itself is covered in reconcile_startup_test.go.
		StartupLadder: []time.Duration{},
	}
}

// Concurrent kicks while a pass is in flight must coalesce into exactly ONE
// extra pass -- not zero (lost work) and not five (an amplifier on an
// unauthenticated endpoint).
func TestKickCoalesces(t *testing.T) {
	gl := newCountingSlowGL()
	r := newLoopTestReconciler(gl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, time.Hour) // the ticker must never fire during this test

	r.Kick()
	if n := <-gl.entered; n != 1 {
		t.Fatalf("first pass call number = %d, want 1", n)
	}

	// Pass #1 is now blocked inside ListMemberProjects. Kick several more
	// times while it is in flight.
	for i := 0; i < 5; i++ {
		r.Kick()
	}
	gl.release <- struct{}{} // let pass #1 finish

	if n := <-gl.entered; n != 2 {
		t.Fatalf("second pass call number = %d, want exactly one coalesced extra pass", n)
	}
	gl.release <- struct{}{} // let pass #2 finish

	select {
	case n := <-gl.entered:
		t.Fatalf("unexpected third pass (call #%d): 5 kicks during pass #1 must coalesce into ONE, not five", n)
	case <-time.After(150 * time.Millisecond):
	}
}

// HB-1's core guarantee: WaitForNextPass must not return based on a pass that
// was already running when it was called -- only on one that starts at or
// after the call.
func TestWaitForNextPassIgnoresAnAlreadyInFlightPass(t *testing.T) {
	gl := newCountingSlowGL()
	r := newLoopTestReconciler(gl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Loop(ctx, time.Hour)

	r.Kick()
	if n := <-gl.entered; n != 1 {
		t.Fatalf("first pass call number = %d, want 1", n)
	}

	// Pass #1 is now in flight (blocked). Call WaitForNextPass while it is
	// running: it must wait for pass #2, not accept pass #1's completion.
	done := make(chan ReconcileSummary, 1)
	go func() {
		rs, err := r.WaitForNextPass(context.Background())
		if err != nil {
			t.Errorf("WaitForNextPass: %v", err)
		}
		done <- rs
	}()

	gl.release <- struct{}{} // let pass #1 finish

	// The wait must still be pending: pass #1 started before the call.
	select {
	case <-done:
		t.Fatal("WaitForNextPass returned on the pre-existing in-flight pass, not a fresh one")
	case <-time.After(100 * time.Millisecond):
	}

	// Pass #2 (queued by WaitForNextPass's own Kick) must now be starting --
	// and it too is blocked until released, so the wait must still be pending.
	if n := <-gl.entered; n != 2 {
		t.Fatalf("second pass call number = %d, want 2", n)
	}
	select {
	case <-done:
		t.Fatal("WaitForNextPass returned before the qualifying pass (#2) actually completed")
	case <-time.After(50 * time.Millisecond):
	}

	gl.release <- struct{}{} // let pass #2 finish

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForNextPass never returned after the pass that started after the call completed")
	}
}

// LastSummary reports no pass until one has actually completed, then the most
// recent one. This backs readiness (main.go): "first reconcile done" is
// exactly this boolean.
func TestLastSummaryBeforeAndAfterAPass(t *testing.T) {
	gl := newCountingSlowGL()
	go func() {
		<-gl.entered
		gl.release <- struct{}{}
	}()
	r := newLoopTestReconciler(gl)

	if _, ok := r.LastSummary(); ok {
		t.Fatal("LastSummary reported ok before any pass ran")
	}

	rs := r.runPass(context.Background())
	if rs.Result != "ok" {
		t.Fatalf("first pass result = %q, want ok (no projects, no errors)", rs.Result)
	}

	got, ok := r.LastSummary()
	if !ok {
		t.Fatal("LastSummary reported !ok after a pass completed")
	}
	if got.Result != "ok" {
		t.Errorf("LastSummary().Result = %q, want ok", got.Result)
	}
}
