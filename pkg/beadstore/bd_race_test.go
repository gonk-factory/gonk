package beadstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// These tests are about what only BdCLI can get wrong: its writes are
// several separate `bd` processes, so another writer can land between them
// and any one of them can be the last that runs. Memory holds a lock across a
// write and has neither failure, so they are not in the shared suite.

type runFunc func(ctx context.Context, dir, bin string, args ...string) ([]byte, error)

// beforeFirst runs hook once, immediately before the first invocation whose
// verb is verb, then lets that invocation through.
func beforeFirst(run runFunc, verb string, hook func()) runFunc {
	fired := false
	return func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		if !fired && len(args) > 0 && args[0] == verb {
			fired = true
			hook()
		}
		return run(ctx, dir, bin, args...)
	}
}

// killedAfter lets the first n invocations through and fails every one after:
// the writer process was killed (or lost the database) part-way through.
func killedAfter(run runFunc, n int) runFunc {
	calls := 0
	return func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		calls++
		if calls > n {
			return nil, fmt.Errorf("killed before bd invocation %d %v", calls, args)
		}
		return run(ctx, dir, bin, args...)
	}
}

func counted(run runFunc, n *int) runFunc {
	return func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		*n++
		return run(ctx, dir, bin, args...)
	}
}

func record(st beadstore.State, attempt int, session string) beadstore.Record {
	return beadstore.Record{
		BeadAnchor: "gonk:75:issue:73", Project: "group/repo", ProjectID: 75, IssueIID: 73,
		Trigger: "issue-triage", SessionKey: "gonk-75-issue-73",
		State: st, Attempt: attempt, SessionID: session,
	}
}

// THE CASE A CHECK-THEN-WRITE CANNOT CLOSE. The conditional write's own check
// passes, and THEN the dispatch's reservation lands, before the sweep's
// comment does -- under the Dolt slowness gonk-p8w8 happened in, seconds
// separate those two bd processes. If the comment log were resolved "last
// comment wins", the sweep's stale copy would now be current: the reservation
// erased exactly as at 15:50:36 in go-3tbq. The log must ignore it instead,
// and the writer must be told it lost.
func TestBdCLIConditionalWriteOvertakenAfterItsCheckTakesNoEffect(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBd()
	dispatch := &beadstore.BdCLI{Run: fake.run}

	if err := dispatch.Put(ctx, record(beadstore.StateRunning, 1, "a1")); err != nil {
		t.Fatalf("seed a1: %v", err)
	}
	listed, err := (&beadstore.BdCLI{Run: fake.run}).List(ctx, beadstore.StateRunning)
	if err != nil || len(listed) != 1 {
		t.Fatalf("List = %+v, %v", listed, err)
	}

	// The sweep's PutIf reads the current revision (still a1 -- its check
	// passes), and just before it appends its comment the reservation lands.
	sweep := &beadstore.BdCLI{Run: beforeFirst(fake.run, "comment", func() {
		if err := dispatch.Put(ctx, record(beadstore.StatePendingPrompt, 2, "a2.MPKO")); err != nil {
			t.Errorf("reservation: %v", err)
		}
	})}
	_, err = sweep.PutIf(ctx, listed[0], listed[0].Revision)
	if !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("PutIf overtaken after its check = %v, want ErrStale", err)
	}

	reader := &beadstore.BdCLI{Run: fake.run}
	got, ok, err := reader.Get(ctx, "gonk:75:issue:73")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if got.State != beadstore.StatePendingPrompt || got.Attempt != 2 || got.SessionID != "a2.MPKO" {
		t.Fatalf("current record = %s Attempt %d %s, want the reservation (pending-prompt, 2, a2.MPKO).\n"+
			"The stale conditional comment is LAST in the log and was allowed to win.",
			got.State, got.Attempt, got.SessionID)
	}
	if recs, _ := reader.List(ctx, beadstore.StatePendingPrompt); len(recs) != 1 {
		t.Fatalf("List(pending-prompt) = %+v, want the reservation findable", recs)
	}
	if recs, _ := reader.List(ctx, beadstore.StateRunning); len(recs) != 0 {
		t.Fatalf("List(running) = %+v, want nothing", recs)
	}
	// The loser must not leave its label behind either: a stale gonk::running
	// on the reservation would cost a comment read on every pass until the
	// next write, and says the wrong thing to a human reading the beads UI.
	for _, ls := range fake.labels {
		if hasLabel(ls, "gonk::running") || !hasLabel(ls, "gonk::pending-prompt") {
			t.Fatalf("labels after the lost write = %v, want gonk::pending-prompt and not gonk::running", ls)
		}
	}
}

// A record written before revisions existed has no Revision in its JSON. It
// must read back with Revision "", and a sweep that listed it must still be able
// to write it conditionally -- otherwise the first sweep after the deploy would
// refuse every in-flight bead as stale.
func TestBdCLIAPreRevisionRecordCanBeWrittenConditionally(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBd()
	b := &beadstore.BdCLI{Run: fake.run}
	if _, err := fake.run(ctx, "", "", "create", "--title", "gonk:75:issue:73", "--labels", "gonk-anchor:gonk:75:issue:73", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.run(ctx, "", "", "label", "add", "bd-1", "gonk::running"); err != nil {
		t.Fatal(err)
	}
	legacy := `<!-- gonk-state {"BeadAnchor":"gonk:75:issue:73","State":"running","Attempt":1,"SessionID":"a1"} -->`
	if _, err := fake.run(ctx, "", "", "comment", "bd-1", legacy); err != nil {
		t.Fatal(err)
	}
	recs, err := b.List(ctx, beadstore.StateRunning)
	if err != nil || len(recs) != 1 || recs[0].Revision != "" {
		t.Fatalf("List = %+v, %v; want the legacy record with Revision \"\"", recs, err)
	}
	next := recs[0]
	next.State = beadstore.StateDone
	if _, err := b.PutIf(ctx, next, recs[0].Revision); err != nil {
		t.Fatalf("PutIf on a pre-revision record with the revision it was listed at = %v", err)
	}
	if got, _, _ := b.Get(ctx, "gonk:75:issue:73"); got.State != beadstore.StateDone || got.Revision == "" {
		t.Fatalf("Get = %s rev %q, want done with a fresh revision", got.State, got.Revision)
	}
}

// A TORN WRITE MUST NOT RETIRE A BEAD (gonk-p7qh's go-ffoe). Kill the writer
// after each of its bd invocations in turn; after that, one ordinary sweep
// pass (List of each state it scans) must leave the bead findable under the
// state of whatever record is now current. Before gonk-p8w8, a kill between
// the comment and the final label add left the old label on a record that
// contradicted it -- matched by no List, forever.
func TestBdCLITornWriteLeavesTheBeadFindable(t *testing.T) {
	swept := []beadstore.State{beadstore.StateRunning, beadstore.StateParked, beadstore.StatePendingPrompt}
	isSwept := func(st beadstore.State) bool {
		for _, s := range swept {
			if s == st {
				return true
			}
		}
		return false
	}
	transitions := []struct{ from, to beadstore.State }{
		{beadstore.StateRunning, beadstore.StatePendingPrompt}, // a dispatch reserving over a re-sling
		{beadstore.StatePendingPrompt, beadstore.StateRunning}, // a dispatch confirming its session
		{beadstore.StateNeedsHuman, beadstore.StateRunning},    // from a state no pass scans
		{beadstore.StateRunning, beadstore.StateDone},          // into one
	}
	for _, tr := range transitions {
		for _, conditional := range []bool{false, true} {
			// How many invocations does an uninterrupted write make?
			n := 0
			{
				fake := newFakeBd()
				seed := &beadstore.BdCLI{Run: fake.run}
				if err := seed.Put(context.Background(), record(tr.from, 1, "old")); err != nil {
					t.Fatal(err)
				}
				w := &beadstore.BdCLI{Run: counted(fake.run, &n)}
				if err := write(w, record(tr.to, 2, "new"), conditional, seed); err != nil {
					t.Fatal(err)
				}
			}
			for k := 0; k < n; k++ {
				name := fmt.Sprintf("%s->%s/conditional=%v/killed-after-%d-of-%d", tr.from, tr.to, conditional, k, n)
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					fake := newFakeBd()
					healthy := &beadstore.BdCLI{Run: fake.run}
					if err := healthy.Put(ctx, record(tr.from, 1, "old")); err != nil {
						t.Fatal(err)
					}
					torn := &beadstore.BdCLI{Run: killedAfter(fake.run, k)}
					_ = write(torn, record(tr.to, 2, "new"), conditional, healthy) // it fails; that is the scenario

					cur, ok, err := healthy.Get(ctx, "gonk:75:issue:73")
					if err != nil || !ok {
						t.Fatalf("Get after the torn write = %v, %v", ok, err)
					}
					if cur.State != tr.from && cur.State != tr.to {
						t.Fatalf("current state %q is neither side of the transition", cur.State)
					}
					for _, st := range swept { // one sweep pass, which may repair
						if _, err := healthy.List(ctx, st); err != nil {
							t.Fatalf("List(%s) = %v", st, err)
						}
					}
					if !isSwept(cur.State) {
						return // nothing scans that state; nothing to strand
					}
					recs, err := healthy.List(ctx, cur.State)
					if err != nil {
						t.Fatalf("List(%s) = %v", cur.State, err)
					}
					if len(recs) != 1 {
						t.Fatalf("the current record is %s but List(%s) after a full pass returns %d records: "+
							"the bead is stranded (bd labels %v)", cur.State, cur.State, len(recs), fake.labels)
					}
				})
			}
		}
	}
}

func write(w *beadstore.BdCLI, r beadstore.Record, conditional bool, reader *beadstore.BdCLI) error {
	ctx := context.Background()
	if !conditional {
		return w.Put(ctx, r)
	}
	cur, _, err := reader.Get(ctx, r.BeadAnchor)
	if err != nil {
		return err
	}
	_, err = w.PutIf(ctx, r, cur.Revision)
	return err
}

// A bead the OLD write order already tore -- label `gonk::running`, record
// pending-prompt -- is in production stores today, and a new write order does
// nothing for it. List's repair does: the pass that lists the stale label adds
// the record's own, and the pass for that state then finds it.
func TestBdCLIListRepairsABeadTornByTheOldWriteOrder(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBd()
	b := &beadstore.BdCLI{Run: fake.run}
	if err := b.Put(ctx, record(beadstore.StateRunning, 1, "a1")); err != nil {
		t.Fatal(err)
	}
	// Hand-tear it the way the old order did when killed after its comment:
	// the new record is in the log, the labels were never touched.
	var id string
	for bead := range fake.labels {
		id = bead
	}
	torn := record(beadstore.StatePendingPrompt, 2, "a2")
	torn.Revision = "torn"
	if _, err := fake.run(ctx, "", "", "comment", id,
		"<!-- gonk-state "+mustJSON(t, torn)+" -->"); err != nil {
		t.Fatal(err)
	}

	if recs, err := b.List(ctx, beadstore.StateRunning); err != nil || len(recs) != 0 {
		t.Fatalf("List(running) = %+v, %v, want nothing (its record is pending-prompt)", recs, err)
	}
	recs, err := b.List(ctx, beadstore.StatePendingPrompt)
	if err != nil || len(recs) != 1 || recs[0].SessionID != "a2" {
		t.Fatalf("List(pending-prompt) after the running pass = %+v, %v: the torn bead is still stranded (labels %v)",
			recs, err, fake.labels[id])
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// pauseBefore runs hook once, immediately before this writer's k-th bd
// invocation (1-based): the writer is preempted there while another runs.
func pauseBefore(run runFunc, k int, hook func()) runFunc {
	calls := 0
	return func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		calls++
		if calls == k {
			hook()
		}
		return run(ctx, dir, bin, args...)
	}
}

// TWO UNKILLED WRITERS MUST NOT LEAVE THE CURRENT RECORD UNLABELLED. Found by
// the independent review of gonk-p8w8's first cut, in the incident's own
// interleaving: the dispatch's Put adds `pending-prompt` and is preempted; the
// sweep's whole PutIf runs, sees that label in its label list and removes it as
// stale; the dispatch then appends its reservation and removes `running`. The
// reservation is current and the bead carries no gonk:: label, so no List ever
// finds it again -- and if that dispatch is then killed, the reclaim that exists
// for exactly that case cannot see it.
//
// Every single-preemption interleaving of the two writers, in both roles.
func TestBdCLITwoWritersNeverLeaveTheCurrentRecordUnlabelled(t *testing.T) {
	type writer struct {
		name string
		rec  beadstore.Record
		cond bool
	}
	sweepRunning := writer{"sweep-PutIf-running", record(beadstore.StateRunning, 1, "a1"), true}
	sweepDone := writer{"sweep-PutIf-done", record(beadstore.StateDone, 1, "a1"), true}
	dispatchRes := writer{"dispatch-Put-pending", record(beadstore.StatePendingPrompt, 2, "a2"), false}
	dispatchRun := writer{"dispatch-Put-running", record(beadstore.StateRunning, 2, "a2"), false}
	pairs := [][2]writer{
		{dispatchRes, sweepRunning}, {sweepRunning, dispatchRes},
		{dispatchRes, sweepDone}, {sweepDone, dispatchRes},
		{dispatchRes, dispatchRun}, {dispatchRun, dispatchRes},
	}
	do := func(b *beadstore.BdCLI, w writer, rev string) {
		ctx := context.Background()
		if w.cond {
			_, _ = b.PutIf(ctx, w.rec, rev) // may lose; that is allowed
			return
		}
		_ = b.Put(ctx, w.rec)
	}
	for _, p := range pairs {
		first, second := p[0], p[1]
		n := 0
		{
			fake := newFakeBd()
			seed := &beadstore.BdCLI{Run: fake.run}
			_ = seed.Put(context.Background(), record(beadstore.StateRunning, 1, "a1"))
			cur, _, _ := seed.Get(context.Background(), "gonk:75:issue:73")
			do(&beadstore.BdCLI{Run: counted(fake.run, &n)}, first, cur.Revision)
		}
		for k := 1; k <= n; k++ {
			t.Run(fmt.Sprintf("%s-preempted-before-call-%d-of-%d-by-%s", first.name, k, n, second.name), func(t *testing.T) {
				ctx := context.Background()
				fake := newFakeBd()
				reader := &beadstore.BdCLI{Run: fake.run}
				if err := reader.Put(ctx, record(beadstore.StateRunning, 1, "a1")); err != nil {
					t.Fatal(err)
				}
				// Both writers read before either writes (the sweep listed early).
				seed, _, _ := reader.Get(ctx, "gonk:75:issue:73")
				w1 := &beadstore.BdCLI{Run: pauseBefore(fake.run, k, func() {
					do(&beadstore.BdCLI{Run: fake.run}, second, seed.Revision)
				})}
				do(w1, first, seed.Revision)

				cur, ok, err := reader.Get(ctx, "gonk:75:issue:73")
				if err != nil || !ok {
					t.Fatalf("Get = %v, %v", ok, err)
				}
				var labels []string
				for _, ls := range fake.labels {
					labels = ls
				}
				if !hasLabel(labels, "gonk::"+string(cur.State)) {
					t.Fatalf("current record is %s (%s) but the bead's labels are %v: no List(%s) will ever find it",
						cur.State, cur.SessionID, labels, cur.State)
				}
			})
		}
	}
}

// The loser's label undo, exercised: the overtaken write is to a state the bead
// did NOT already carry (a done verdict), so PutIf added that label itself and
// must take it back when its comment turns out to be inert.
func TestBdCLIOvertakenConditionalWriteTakesBackTheLabelItAdded(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBd()
	dispatch := &beadstore.BdCLI{Run: fake.run}
	if err := dispatch.Put(ctx, record(beadstore.StateRunning, 1, "a1")); err != nil {
		t.Fatal(err)
	}
	listed, _, _ := dispatch.Get(ctx, "gonk:75:issue:73")
	done := listed
	done.State = beadstore.StateDone
	// Overtaken after its revision check and BEFORE its label list: the
	// dispatch's own cleanup has already run, so nothing but PutIf itself can
	// remove the done label it is about to add.
	sweep := &beadstore.BdCLI{Run: beforeFirst(fake.run, "label", func() {
		if err := dispatch.Put(ctx, record(beadstore.StatePendingPrompt, 2, "a2")); err != nil {
			t.Errorf("reservation: %v", err)
		}
	})}
	if _, err := sweep.PutIf(ctx, done, listed.Revision); !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("PutIf = %v, want ErrStale", err)
	}
	for _, ls := range fake.labels {
		if hasLabel(ls, "gonk::done") {
			t.Fatalf("labels = %v: the lost done write left its label on the reservation", ls)
		}
	}
}

// beforeFirstMatch runs hook once, immediately before the first invocation
// match accepts.
func beforeFirstMatch(run runFunc, match func(args []string) bool, hook func()) runFunc {
	fired := false
	return func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		if !fired && match(args) {
			fired = true
			hook()
		}
		return run(ctx, dir, bin, args...)
	}
}

func isLabelRemove(args []string) bool {
	return len(args) >= 2 && args[0] == "label" && args[1] == "remove"
}

// PutIf's OWN settle, on the path where its write applied. Only a second
// preemption reaches it (the independent re-check found both PutIf settles
// could be deleted with every other test green). The sweep's done write
// applies; before it removes the labels it saw, a dispatch writes `running`
// over it, finding `running` already present, removing `done` and settling.
// The sweep then removes `running` as one of the labels it saw. Without the
// sweep's own settle the current running record is left with no label.
func TestBdCLIAppliedConditionalWriteSettlesAfterItsRemovals(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBd()
	reader := &beadstore.BdCLI{Run: fake.run}
	if err := reader.Put(ctx, record(beadstore.StateRunning, 1, "a1")); err != nil {
		t.Fatal(err)
	}
	seed, _, _ := reader.Get(ctx, "gonk:75:issue:73")
	done := seed
	done.State = beadstore.StateDone

	sweep := &beadstore.BdCLI{Run: beforeFirstMatch(fake.run, isLabelRemove, func() {
		if err := reader.Put(ctx, record(beadstore.StateRunning, 2, "a2")); err != nil {
			t.Errorf("dispatch: %v", err)
		}
	})}
	if _, err := sweep.PutIf(ctx, done, seed.Revision); err != nil {
		t.Fatalf("PutIf = %v, want applied (the dispatch came after its re-read)", err)
	}
	assertCurrentLabelled(t, fake, reader)
}

// ...and on the path where it lost. The sweep's done write is overtaken by a
// reservation after its check, so its comment is inert and it goes to take back
// the done label it added. Before it does, another writer makes `done` current
// again, finds the label already there and settles. The sweep's undo then
// removes the label the current record needs.
func TestBdCLILostConditionalWriteSettlesAfterItsUndo(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBd()
	reader := &beadstore.BdCLI{Run: fake.run}
	if err := reader.Put(ctx, record(beadstore.StateRunning, 1, "a1")); err != nil {
		t.Fatal(err)
	}
	seed, _, _ := reader.Get(ctx, "gonk:75:issue:73")
	done := seed
	done.State = beadstore.StateDone

	run := beforeFirst(fake.run, "comment", func() {
		if err := reader.Put(ctx, record(beadstore.StatePendingPrompt, 2, "a2")); err != nil {
			t.Errorf("reservation: %v", err)
		}
	})
	run = beforeFirstMatch(run, isLabelRemove, func() {
		if err := reader.Put(ctx, record(beadstore.StateDone, 2, "a2")); err != nil {
			t.Errorf("third writer: %v", err)
		}
	})
	if _, err := (&beadstore.BdCLI{Run: run}).PutIf(ctx, done, seed.Revision); !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("PutIf = %v, want ErrStale", err)
	}
	assertCurrentLabelled(t, fake, reader)
}

func assertCurrentLabelled(t *testing.T, fake *fakeBd, reader *beadstore.BdCLI) {
	t.Helper()
	cur, ok, err := reader.Get(context.Background(), "gonk:75:issue:73")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	for _, ls := range fake.labels {
		if !hasLabel(ls, "gonk::"+string(cur.State)) {
			t.Fatalf("current record is %s but the bead's labels are %v: no List(%s) will find it",
				cur.State, ls, cur.State)
		}
	}
}
