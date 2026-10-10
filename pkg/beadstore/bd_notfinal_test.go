package beadstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// THIS TEST PINS A KNOWN DEFECT, NOT A FEATURE. It is why BdCLI is not run
// against storetest.RunCompareAndSwap and why gonk-gate no longer selects it
// for its state (gonk-t41). The day BdCLI gets a real compare-and-swap -- or is
// deleted -- this test should fail or go, and RunCompareAndSwap should be wired
// to whatever replaced it.
//
// gonk-vred found one way two conditional writes on one revision can both be
// told "applied": bd sorts comments by a client timestamp, not commit order.
// Running storetest's PutIfSuccessIsFinal against BdCLI (gonk-t41) found a
// second, needing no clock at all, on the very FIRST write to an anchor: PutIf
// with ifRevision "" resolves the anchor to a bead with `bd list`, and when
// there is none it runs `bd create`. Two writers that both list before either
// creates get TWO beads for the one anchor, each appends its conditional
// comment to its own bead, each re-reads its own bead, and each is told it
// applied. That is dispatch's pending-prompt reservation when two dispatches
// race on a bead with no record yet.
//
// Deterministic here: writer A is held after its `bd list` found nothing until
// writer B's whole PutIf has returned.
func TestBdCLIPutIfOnAnAbsentAnchorIsNotACompareAndSwap(t *testing.T) {
	fake := newFakeBd()
	type writerKey struct{}
	listed := make(chan struct{})
	bDone := make(chan struct{})
	var once sync.Once
	run := func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		out, err := fake.run(ctx, dir, bin, args...)
		if ctx.Value(writerKey{}) == "A" && len(args) > 0 && args[0] == "list" {
			once.Do(func() {
				close(listed)
				<-bDone
			})
		}
		return out, err
	}
	s := &beadstore.BdCLI{Run: run}
	rec := beadstore.Record{BeadAnchor: "gonk:42:issue:3", State: beadstore.StatePendingPrompt, Attempt: 1}

	var revA string
	var errA error
	aReturned := make(chan struct{})
	go func() {
		defer close(aReturned)
		a := rec
		a.SessionID = "A"
		revA, errA = s.PutIf(context.WithValue(context.Background(), writerKey{}, "A"), a, "")
	}()
	<-listed
	b := rec
	b.SessionID = "B"
	revB, errB := s.PutIf(context.Background(), b, "")
	close(bDone)
	<-aReturned

	if errB != nil {
		t.Fatalf("writer B PutIf = %v, want applied", errB)
	}
	if errors.Is(errA, beadstore.ErrStale) {
		t.Fatalf("writer A was refused (ErrStale): BdCLI's first write to an anchor now behaves as a " +
			"compare-and-swap. The defect this test pins is gone -- wire BdCLI to storetest.RunCompareAndSwap " +
			"and delete this test.")
	}
	if errA != nil {
		t.Fatalf("writer A PutIf = %v", errA)
	}
	if revA == "" || revB == "" || revA == revB {
		t.Fatalf("revisions A=%q B=%q", revA, revB)
	}
	// Both were told "applied" on the one precondition "" -- the defect.
	got, ok, err := s.Get(context.Background(), rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("Get = %+v %v %v", got, ok, err)
	}
	t.Logf("KNOWN DEFECT reproduced: both writers applied on ifRevision \"\" (A=%s, B=%s); a reader sees %s",
		revA, revB, got.SessionID)
}
