package main

// gonk-2usw part VERIFY: after the fix merge request merges (plan 7.4) --
// sweep_pipefix_confirm.go. The world is sweep_pipefix_verify_test.go's, with
// the fix pass ON (the confirmation hangs off run maintenance and exists only
// then). A person "merges" by setting the fake merge request's state.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

const fcTargetSHA = "abababababababababababababababababababab"

// fcWorld is a fix the exit check has already settled as `fixed`, in a world
// whose sweep runs the fix pass.
type fcWorld struct {
	*fvWorld
	live bool
}

func newFCWorld(t *testing.T, verdict func(w *fvWorld)) *fcWorld {
	t.Helper()
	w := &fcWorld{fvWorld: newFVWorld(t, faOpts{}), live: true}
	if verdict == nil {
		verdict = func(w *fvWorld) { w.mrPipeline("success", fvPass("build"), fvPass("unit-tests")) }
	}
	verdict(w.fvWorld)
	w.fvWorld.look()
	if rec := w.rec(); rec.State == beadstore.StateRunning {
		t.Fatalf("setup: the fix was not settled (fix state %q)\n%s", rec.PipelineFix.State, w.log.String())
	}
	w.resetWrites()
	return w
}

// look is one sweep far enough on for run maintenance to visit the run.
func (w *fcWorld) look() string {
	w.t.Helper()
	w.clock = w.clock.Add(pipelineRunCheckEvery + time.Minute)
	at := w.log.Len()
	d := w.deps()
	d.FixesLive = w.live
	if code := runSweep(context.Background(), d); code != 0 {
		w.t.Fatalf("sweep exit %d\n%s", code, w.log.String())
	}
	return w.log.String()[at:]
}

// merge is a person merging the fix merge request, now.
func (w *fcWorld) merge() {
	w.t.Helper()
	w.clock = w.clock.Add(time.Minute)
	w.r.gl.SetMRState(w.r.pid, w.rec().PipelineFix.MRIID, "merged", w.clock)
	w.clock = w.clock.Add(time.Minute)
}

// targetPipeline adds a pipeline on the target branch, created now.
func (w *fcWorld) targetPipeline(status string, jobs ...fvJob) int64 {
	w.t.Helper()
	return w.pipelineOn(fcTargetSHA, "main", "push", status, 0, jobs...)
}

func (w *fcWorld) run() beadstore.Record {
	w.t.Helper()
	run, ok, err := w.store.Get(context.Background(), w.r.rec.BeadAnchor)
	if err != nil || !ok {
		w.t.Fatalf("run record: %v %v", ok, err)
	}
	return run
}

func (w *fcWorld) runNote() (state, body string) {
	w.t.Helper()
	for _, n := range w.landingNotes() {
		if st := pipelinectx.RunMarkerStates(n.Body, w.r.rec.BeadAnchor); len(st) > 0 {
			return st[len(st)-1], n.Body
		}
	}
	w.t.Fatal("no run note")
	return "", ""
}

func (w *fcWorld) issueState() string { return w.r.gl.IssuesOf(w.r.pid)[0].State }

// THE ROWS OF PLAN 7.4, on GitLab and on both records.
func TestAfterTheMergeTheTargetsNextPipelineDecides(t *testing.T) {
	t.Run("merged: the note says so and the watch starts", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.merge()
		log := w.look()
		rec := w.rec()
		pf := rec.PipelineFix
		if pf.State != string(pipefix.StateMerged) || pf.MergedAt.IsZero() || !reflect.DeepEqual(pf.MergedSHAs, []string{pf.FixCommitSHA}) || rec.State != beadstore.StateDone {
			t.Fatalf("after the merge: fix state %q merged at %v shas %v record %q\n%s", pf.State, pf.MergedAt, pf.MergedSHAs, rec.State, log)
		}
		if st, body, _ := w.fixNote(); st != "merged" || !strings.Contains(body, fmt.Sprintf("!%d merged", pf.MRIID)) {
			t.Fatalf("fix note %q:\n%s", st, body)
		}
		if run := w.run(); !run.PipelineRunOpen || run.PipelineRunFixState != "merged" {
			t.Fatalf("run open %v fix line %q", run.PipelineRunOpen, run.PipelineRunFixState)
		}
		if n := len(w.om.requests()); n != 1 {
			t.Fatalf("%d outcome reports: the merge must not report another", n)
		}
		// A merged merge request is not edited.
		for _, wr := range w.written() {
			if strings.Contains(wr.Path, "/merge_requests/") && wr.Method == "PUT" && !strings.Contains(wr.Path, "/notes/") {
				t.Fatalf("the merged merge request was edited: %s", faLine(wr))
			}
		}
	})

	t.Run("green, the diagnosed job passed: confirmed, and the run resolves", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.merge()
		id := w.targetPipeline("success", fvPass("build"), fvPass("unit-tests"))
		log := w.look()
		rec := w.rec()
		if rec.PipelineFix.State != string(pipefix.StateConfirmed) || rec.PipelineFix.ConfirmPipelineID != id {
			t.Fatalf("fix state %q confirm pipeline %d, want confirmed on %d\n%s", rec.PipelineFix.State, rec.PipelineFix.ConfirmPipelineID, id, log)
		}
		if st, body, _ := w.fixNote(); st != "confirmed" || !strings.Contains(body, "is confirmed on") {
			t.Fatalf("fix note %q:\n%s", st, body)
		}
		if st, _ := w.runNote(); st != pipelinectx.RunStateResolved || w.run().PipelineRunOpen || w.issueState() != "closed" {
			t.Fatalf("run note %q open %v issue %q: a green target resolves the run as before", st, w.run().PipelineRunOpen, w.issueState())
		}
		if hasString(w.landingLabels(), labelNeedsHuman) {
			t.Fatal("a confirmed fix asked for a person")
		}
	})

	t.Run("red for a different reason: confirmed, and THE RUN THE FIX ENDED IS CLOSED", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.merge()
		id := w.targetPipeline("failed", fvPass("build"), fvFail("unit-tests", fvOtherLog))
		log := w.look()
		rec := w.rec()
		if rec.PipelineFix.State != string(pipefix.StateConfirmed) || rec.PipelineFix.ConfirmPipelineID != id {
			t.Fatalf("fix state %q\n%s", rec.PipelineFix.State, log)
		}
		run := w.run()
		st, body := w.runNote()
		if run.PipelineRunOpen || st != pipelinectx.RunStateSuperseded {
			t.Fatalf("the ended run: open %v, note %q -- it must be closed, or run maintenance keeps marking it still failing\n%s",
				run.PipelineRunOpen, st, log)
		}
		if !strings.Contains(body, fmt.Sprintf("the fix merge request !%d merged", rec.PipelineFix.MRIID)) || !strings.Contains(body, fmt.Sprintf("/-/pipelines/%d)", id)) {
			t.Fatalf("the closed run's note does not say why:\n%s", body)
		}
		// NOT booked as "still failing" on the old run, and the tracking issue
		// stays open for the new one.
		if run.PipelineLatestFailed == id || strings.Contains(log, "still failing") {
			t.Fatalf("the post-merge pipeline was booked on the ended run (latest failed %d)\n%s", run.PipelineLatestFailed, log)
		}
		if w.issueState() != "opened" {
			t.Fatalf("the tracking issue is %q; the failure that remains is a new run and lands on it", w.issueState())
		}
		// Later looks: the run is closed, nothing more happens to either.
		w.resetWrites()
		w.look()
		if ws := w.written(); len(ws) != 0 {
			t.Fatalf("writes after the run was closed: %v", faLines(ws))
		}
	})

	t.Run("the diagnosed job fails the same way: recurred, a person, the run stays open", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.merge()
		id := w.targetPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
		log := w.look()
		rec := w.rec()
		if rec.PipelineFix.State != string(pipefix.StateRecurred) || rec.PipelineFix.ConfirmPipelineID != id {
			t.Fatalf("fix state %q\n%s", rec.PipelineFix.State, log)
		}
		if st, body, _ := w.fixNote(); st != "recurred" || !strings.Contains(body, "proposes no revert") {
			t.Fatalf("fix note %q:\n%s", st, body)
		}
		if !hasString(w.landingLabels(), labelNeedsHuman) || !w.run().PipelineRunOpen {
			t.Fatalf("labels %v, run open %v", w.landingLabels(), w.run().PipelineRunOpen)
		}
		if st, _ := w.runNote(); st != pipelinectx.RunStateDiagnosed {
			t.Fatalf("run note %q, want diagnosed (the run stays open)", st)
		}
	})

	// THE ROW THE TASK NAMES.
	t.Run("green, but the diagnosed job DID NOT RUN: not confirmed", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.merge()
		first := w.targetPipeline("success", fvPass("build"))
		log := w.look()
		rec := w.rec()
		if rec.PipelineFix.State != string(pipefix.StateMerged) || rec.PipelineFix.ConfirmPipelineID != first {
			t.Fatalf("fix state %q (seen pipeline %d): a green pipeline without the diagnosed job confirms nothing\n%s",
				rec.PipelineFix.State, rec.PipelineFix.ConfirmPipelineID, log)
		}
		if st, body, _ := w.fixNote(); st != "merged" || !strings.Contains(body, "the fix is NOT confirmed") {
			t.Fatalf("fix note %q:\n%s", st, body)
		}
		if !strings.Contains(log, "confirmation=not-observed") {
			t.Fatalf("the look did not log not-observed:\n%s", log)
		}
		// The target IS green: the run resolves as it always did, which ends
		// the watch -- and the note says the fix was not confirmed.
		if st, _ := w.runNote(); st != pipelinectx.RunStateResolved || w.run().PipelineRunOpen {
			t.Fatalf("run note %q open %v", st, w.run().PipelineRunOpen)
		}
	})

	t.Run("a pipeline that decided nothing is skipped; the next one decides", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.merge()
		// Red (so the run stays open) and the diagnosed job absent.
		first := w.targetPipeline("failed", fvFail("build", plLintLog))
		w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateMerged) || rec.PipelineFix.ConfirmPipelineID != first {
			t.Fatalf("fix state %q after a pipeline without the job", rec.PipelineFix.State)
		}
		second := w.targetPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
		w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateRecurred) || rec.PipelineFix.ConfirmPipelineID != second {
			t.Fatalf("fix state %q on pipeline %d, want recurred on %d", rec.PipelineFix.State, rec.PipelineFix.ConfirmPipelineID, second)
		}
	})

	t.Run("only a pipeline AFTER the merge counts, and the first one first", func(t *testing.T) {
		w := newFCWorld(t, nil)
		// Before the merge: a green target pipeline with the job passing.
		// It says nothing about the fix.
		w.targetPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
		w.merge()
		log := w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateMerged) || rec.PipelineFix.ConfirmPipelineID != 0 {
			t.Fatalf("a pipeline created BEFORE the merge decided: fix state %q\n%s", rec.PipelineFix.State, log)
		}
		// After it: the first is still running, a later one already failed
		// the same way. The first must be allowed to decide first.
		running := w.targetPipeline("running", fvPass("build"))
		w.clock = w.clock.Add(time.Second)
		later := w.targetPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
		log = w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateMerged) {
			t.Fatalf("a later pipeline decided while the first post-merge one is running: %q", rec.PipelineFix.State)
		}
		// And the run is HELD: maintenance did not book the later failure on it.
		if run := w.run(); !run.PipelineRunOpen || run.PipelineLatestFailed == later || strings.Contains(log, "still failing") {
			t.Fatalf("run maintenance booked a post-merge pipeline on a run whose merged fix is unconfirmed (latest failed %d)\n%s",
				run.PipelineLatestFailed, log)
		}
		w.r.gl.SetPipelineStatus(w.r.pid, running, "success")
		w.r.gl.AddJob(w.r.pid, running, glab.Job{ID: running*100 + 9, Name: "unit-tests", Stage: "test", Status: "success"})
		w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateConfirmed) || rec.PipelineFix.ConfirmPipelineID != running {
			t.Fatalf("fix state %q on %d, want confirmed on the first post-merge pipeline %d", rec.PipelineFix.State, rec.PipelineFix.ConfirmPipelineID, running)
		}
	})

	t.Run("a pipeline on the merged commit counts even if created before merged_at", func(t *testing.T) {
		w := newFCWorld(t, nil)
		sha := w.rec().PipelineFix.FixCommitSHA
		// GitLab stamps merged_at AFTER it creates the merge commit's pipeline.
		id := w.pipelineOn(sha, "main", "push", "success", 0, fvPass("build"), fvPass("unit-tests"))
		w.merge()
		w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateConfirmed) || rec.PipelineFix.ConfirmPipelineID != id {
			t.Fatalf("fix state %q: the landed commit's own pipeline was not taken", rec.PipelineFix.State)
		}
	})

	t.Run("closed after its verdict: closed-unmerged", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.r.gl.SetMRState(w.r.pid, w.rec().PipelineFix.MRIID, "closed", w.clock)
		w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateClosedUnmerged) {
			t.Fatalf("fix state %q", rec.PipelineFix.State)
		}
		// fixed -> closed-unmerged is a verdict over a verdict on the note:
		// the note keeps `fixed`; the record says what happened.
		if st, _, _ := w.fixNote(); st != "fixed" {
			t.Fatalf("fix note %q", st)
		}
	})

	t.Run("an inconclusive fix a person merged anyway is still confirmed (7.1: unverifiable)", func(t *testing.T) {
		w := newFCWorld(t, func(w *fvWorld) { w.mrPipeline("success", fvPass("build")) })
		if st := w.rec().PipelineFix.State; st != string(pipefix.StateUnverifiable) {
			t.Fatalf("setup: %q", st)
		}
		w.merge()
		w.targetPipeline("success", fvPass("build"), fvPass("unit-tests"))
		w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateConfirmed) {
			t.Fatalf("fix state %q", rec.PipelineFix.State)
		}
		if st, _, _ := w.fixNote(); st != "confirmed" {
			t.Fatalf("fix note %q", st)
		}
	})

	t.Run("merged before any verdict: the exit check says merged, the confirmation takes over", func(t *testing.T) {
		w := &fcWorld{fvWorld: newFVWorld(t, faOpts{}), live: true}
		w.merge()
		w.fvWorld.look() // the exit check: J0.MERGED
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateMerged) || !rec.PipelineFix.MergedAt.IsZero() {
			t.Fatalf("after the exit check: fix state %q merged at %v", rec.PipelineFix.State, rec.PipelineFix.MergedAt)
		}
		id := w.targetPipeline("failed", fvPass("build"), fvFail("unit-tests", fvOtherLog))
		w.look()
		rec := w.rec()
		if rec.PipelineFix.State != string(pipefix.StateConfirmed) || rec.PipelineFix.MergedAt.IsZero() || rec.PipelineFix.ConfirmPipelineID != id || w.run().PipelineRunOpen {
			t.Fatalf("fix state %q merged at %v run open %v", rec.PipelineFix.State, rec.PipelineFix.MergedAt, w.run().PipelineRunOpen)
		}
	})
}

// A run whose close could not be written is closed by the next look: the fix
// is already `confirmed`, and the run must not stay open for ever.
func TestAConfirmedFixWhoseRunStayedOpenIsClosedByTheNextLook(t *testing.T) {
	w := newFCWorld(t, nil)
	w.merge()
	id := w.targetPipeline("failed", fvPass("build"), fvFail("unit-tests", fvOtherLog))
	// The RUN note, and only it, cannot be edited during the first look: the
	// fix note still moves to `confirmed`.
	var runNoteID int64
	for _, n := range w.landingNotes() {
		if len(pipelinectx.RunMarkerStates(n.Body, w.r.rec.BeadAnchor)) > 0 {
			runNoteID = n.ID
		}
	}
	if runNoteID == 0 {
		t.Fatal("no run note")
	}
	w.r.gl.FailAlways("PUT", fmt.Sprintf("/api/v4/projects/%d/issues/1/notes/%d", w.r.pid, runNoteID), 500)
	w.look()
	if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateConfirmed) {
		t.Fatalf("setup: the fix is %q, want confirmed with its run still open", rec.PipelineFix.State)
	}
	if st, _ := w.runNote(); !w.run().PipelineRunOpen || st != pipelinectx.RunStateDiagnosed {
		t.Fatalf("the run was closed (open %v, note %q) although its note could not be written", w.run().PipelineRunOpen, st)
	}
	w.r.gl.ClearFailures()
	w.look()
	rec := w.rec()
	if rec.PipelineFix.State != string(pipefix.StateConfirmed) || rec.PipelineFix.ConfirmPipelineID != id || w.run().PipelineRunOpen {
		t.Fatalf("after GitLab came back: fix state %q run open %v", rec.PipelineFix.State, w.run().PipelineRunOpen)
	}
	if st, _ := w.runNote(); st != pipelinectx.RunStateSuperseded {
		t.Fatalf("run note %q", st)
	}
}

// What the confirmation does NOT touch, and what it costs when there is
// nothing to do.
func TestThePostMergeLookIsInertWhereItShouldBe(t *testing.T) {
	t.Run("the fix pass is off: no look at all", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.live = false
		w.merge()
		w.targetPipeline("failed", fvPass("build"), fvFail("unit-tests", fvOtherLog))
		w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateFixed) {
			t.Fatalf("fix state %q with the pass off", rec.PipelineFix.State)
		}
	})
	t.Run("a fix still being verified is the exit check's", func(t *testing.T) {
		w := &fcWorld{fvWorld: newFVWorld(t, faOpts{}), live: true}
		before := w.rec()
		l, _ := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(&w.log))
		reads := len(w.r.gl.Requests())
		got, hold := confirmMergedFix(context.Background(), w.deps(), l, w.run())
		if n := len(w.r.gl.Requests()) - reads; n != 0 || hold || !got.PipelineRunOpen || w.rec().Revision != before.Revision {
			t.Fatalf("%d GitLab request(s) for a fix the exit check owns", n)
		}
	})
	t.Run("a run with no fix costs no GitLab request", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		l, _ := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(w.log))
		run, _, _ := w.store.Get(context.Background(), w.r.rec.BeadAnchor)
		reads := len(w.r.gl.Requests())
		confirmMergedFix(context.Background(), w.deps(), l, run)
		if n := len(w.r.gl.Requests()) - reads; n != 0 {
			t.Fatalf("%d GitLab request(s) for a run without a fix", n)
		}
	})
	t.Run("a merge request that is not the bot's is not watched", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.merge()
		w.targetPipeline("success", fvPass("build"), fvPass("unit-tests"))
		l, _ := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(&w.log))
		l.me = glab.User{ID: 4242}
		confirmMergedFix(context.Background(), w.deps(), l, w.run())
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateFixed) {
			t.Fatalf("fix state %q: a merge request another user authored was taken for the fix's", rec.PipelineFix.State)
		}
	})
	t.Run("a merged merge request without merged_at is not believed", func(t *testing.T) {
		w := newFCWorld(t, nil)
		w.r.gl.SetMRState(w.r.pid, w.rec().PipelineFix.MRIID, "merged", time.Time{})
		w.look()
		if rec := w.rec(); rec.PipelineFix.State != string(pipefix.StateFixed) || !rec.PipelineFix.MergedAt.IsZero() {
			t.Fatalf("fix state %q merged at %v", rec.PipelineFix.State, rec.PipelineFix.MergedAt)
		}
	})
}

// A held run is looked at once per maintenance interval, like any other run:
// the hold stamps it.
func TestAHeldRunIsLookedAtOncePerMaintenanceInterval(t *testing.T) {
	w := newFCWorld(t, nil)
	w.merge()
	w.look() // merged, no pipeline after the merge yet: held
	if st := w.rec().PipelineFix.State; st != string(pipefix.StateMerged) {
		t.Fatalf("setup: %q", st)
	}
	reads := len(w.r.gl.Requests())
	for i := 0; i < 3; i++ {
		w.clock = w.clock.Add(30 * time.Second)
		d := w.deps()
		d.FixesLive = true
		if code := runSweep(context.Background(), d); code != 0 {
			t.Fatalf("sweep exit %d", code)
		}
	}
	if n := len(w.r.gl.Requests()) - reads; n != 0 {
		t.Fatalf("%d GitLab request(s) for a held run inside its maintenance interval: %v", n, w.r.gl.Requests()[reads:])
	}
	w.look()
	if len(w.r.gl.Requests()) == reads {
		t.Fatal("a held run was not looked at after its interval")
	}
}

// A fix that has ENDED costs the post-merge pass no GitLab request, however
// long its run stays open.
func TestAnEndedFixCostsNoGitLabRequest(t *testing.T) {
	for _, st := range []pipefix.State{pipefix.StateRecurred, pipefix.StateClosedUnmerged, pipefix.StateNeedsScope, pipefix.StateNotFixed} {
		t.Run(string(st), func(t *testing.T) {
			w := newFCWorld(t, nil)
			rec := w.rec()
			rec.PipelineFix.State = string(st)
			if err := w.store.Put(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			l, _ := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(&w.log))
			reads := len(w.r.gl.Requests())
			run, hold := confirmMergedFix(context.Background(), w.deps(), l, w.run())
			if n := len(w.r.gl.Requests()) - reads; n != 0 || hold || !run.PipelineRunOpen {
				t.Fatalf("%d GitLab request(s), hold=%v for a fix in state %s", n, hold, st)
			}
		})
	}
}
