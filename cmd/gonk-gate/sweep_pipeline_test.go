package main

// gonk-tios P7: run maintenance (sweepPipelineRuns). Synthetic fixtures only.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// landed returns plRun with its diagnosis applied and the record as the sweep
// leaves it (done, run open), in a store.
func landed(t *testing.T, o plOpts) (*plRun, beadstore.Store) {
	t.Helper()
	r := newPLRun(t, o)
	if applied, v, err := r.apply(t, plDiagnosis(nil), io.Discard); !applied {
		t.Fatalf("refused: %q %v", v, err)
	}
	r.rec.State = beadstore.StateDone
	store := beadstore.NewMemory()
	if err := store.Put(context.Background(), r.rec); err != nil {
		t.Fatal(err)
	}
	return r, store
}

func (r *plRun) maintain(t *testing.T, store beadstore.Store, now time.Time) beadstore.Record {
	t.Helper()
	sweepPipelineRuns(context.Background(), sweepDeps{Store: store, Pipelines: r.gl.Client(),
		Log: testLogger(io.Discard), Now: func() time.Time { return now }})
	rec, _, _ := store.Get(context.Background(), r.rec.BeadAnchor)
	return rec
}

func (r *plRun) runNote(t *testing.T) (string, string) {
	t.Helper()
	var notes []glab.Note
	if r.target.Kind == pipelinectx.TargetKindMR {
		notes = r.gl.MRNotesOf(r.pid, 5)
	} else {
		notes = r.gl.IssueNotesOf(r.pid, 1)
	}
	if len(notes) != 1 {
		t.Fatalf("run notes = %d, want 1", len(notes))
	}
	st := pipelinectx.RunMarkerStates(notes[0].Body, r.rec.BeadAnchor)
	if len(st) != 1 {
		t.Fatalf("marker states %v", st)
	}
	return st[0], notes[0].Body
}

// GREEN: the note says resolved (keeping the diagnosis), the tracking issue is
// closed and loses gonk::pipeline-failing, and the run is closed.
func TestAGreenPipelineResolvesTheRunAndClosesTheIssue(t *testing.T) {
	r, store := landed(t, plOpts{})
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 950, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
	rec := r.maintain(t, store, time.Now())
	st, body := r.runNote(t)
	if st != pipelinectx.RunStateResolved || !strings.Contains(body, plLink(r.proj.WebURL, 950)+" passed") ||
		!strings.Contains(firstLine(body), "gonk's diagnosis of "+plLink(r.proj.WebURL, 900)+" is kept below.") || !strings.Contains(body, plGoodLine) {
		t.Fatalf("note state %s, body:\n%s", st, body)
	}
	is := r.gl.IssuesOf(r.pid)[0]
	if is.State != "closed" {
		t.Fatalf("tracking issue state = %s, want closed", is.State)
	}
	for _, l := range is.Labels {
		if l == labelPipelineFailing {
			t.Fatalf("a resolved issue still says failing: %v", is.Labels)
		}
	}
	if rec.PipelineRunOpen {
		t.Fatal("run still open after a green pipeline")
	}
	// Idempotent: a later look at a closed run does nothing.
	before := len(writes(r.gl))
	r.maintain(t, store, time.Now().Add(time.Hour))
	if after := len(writes(r.gl)); after != before {
		t.Fatalf("a closed run was written again: %v", writes(r.gl)[before:])
	}
}

// MR target, green: resolved, and the MR loses its class label.
func TestAGreenMRPipelineResolvesAndRemovesTheClassLabel(t *testing.T) {
	r, store := landed(t, plOpts{mr: true})
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 950, Ref: "feature/sum", SHA: plSHAGreen, Status: "success", Source: "merge_request_event"})
	r.gl.AddMRPipeline(r.pid, 5, 950)
	r.maintain(t, store, time.Now())
	if st, _ := r.runNote(t); st != pipelinectx.RunStateResolved {
		t.Fatalf("state %s", st)
	}
	if labels := r.gl.MRsOf(r.pid)[0].Labels; len(labels) != 0 {
		t.Fatalf("MR labels = %v, want none", labels)
	}
}

// STILL FAILING with the same failing jobs: the note's latest-failing line
// moves on; the run stays open; no new note.
func TestTheSameFailureAgainUpdatesTheLatestFailingLine(t *testing.T) {
	r, store := landed(t, plOpts{})
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 970, Ref: "main", SHA: plSHA, Status: "failed", Source: "push"})
	r.gl.AddJob(r.pid, 970, glab.Job{ID: 9702, Name: "unit-tests", Stage: "test", Status: "failed", FailureReason: "script_failure"})
	rec := r.maintain(t, store, time.Now())
	st, body := r.runNote(t)
	if st != pipelinectx.RunStateDiagnosed || !strings.Contains(body, "Latest failing pipeline in this run: "+plLink(r.proj.WebURL, 970)) ||
		!strings.Contains(body, plGoodLine) {
		t.Fatalf("state %s body:\n%s", st, body)
	}
	if !rec.PipelineRunOpen || rec.PipelineLatestFailed != 970 {
		t.Fatalf("record = open %v latest %d", rec.PipelineRunOpen, rec.PipelineLatestFailed)
	}
	if !strings.Contains(r.gl.IssuesOf(r.pid)[0].Description, "| Latest failing pipeline | "+plLink(r.proj.WebURL, 970)+" |") {
		t.Fatalf("description not updated:\n%s", r.gl.IssuesOf(r.pid)[0].Description)
	}
}

// SUPERSEDED: a different failing-job set closes this run; the issue stays
// open for the new run.
func TestADifferentFailureSupersedesTheRun(t *testing.T) {
	r, store := landed(t, plOpts{})
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 960, Ref: "main", SHA: plSHA, Status: "failed", Source: "push"})
	r.gl.AddJob(r.pid, 960, glab.Job{ID: 9601, Name: "build", Stage: "build", Status: "failed", FailureReason: "script_failure"})
	rec := r.maintain(t, store, time.Now())
	if st, body := r.runNote(t); st != pipelinectx.RunStateSuperseded || !strings.Contains(body, plLink(r.proj.WebURL, 960)+" failed with a different set") ||
		!strings.Contains(firstLine(body), "about the earlier failure, "+plLink(r.proj.WebURL, 900)+".") {
		t.Fatalf("state %s body:\n%s", st, body)
	}
	if rec.PipelineRunOpen {
		t.Fatal("a superseded run is still open")
	}
	if r.gl.IssuesOf(r.pid)[0].State != "opened" {
		t.Fatal("superseding must not close the tracking issue")
	}
}

// Each open run is looked at no more than once per five minutes.
func TestRunMaintenanceLooksAtMostEveryFiveMinutes(t *testing.T) {
	r, store := landed(t, plOpts{})
	now := time.Now()
	r.maintain(t, store, now)
	reads := len(r.gl.Requests())
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 950, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
	rec := r.maintain(t, store, now.Add(4*time.Minute))
	if len(r.gl.Requests()) != reads || !rec.PipelineRunOpen {
		t.Fatal("looked again inside five minutes")
	}
	if rec = r.maintain(t, store, now.Add(6*time.Minute)); rec.PipelineRunOpen {
		t.Fatal("did not look again after five minutes")
	}
}

// A PARKED run that has resolved is retired, not unparked into a diagnosis
// nobody needs; a running one is left to its session.
func TestAResolvedParkedRunIsRetiredAndARunningOneIsLeftAlone(t *testing.T) {
	r, store := landed(t, plOpts{})
	parked := r.rec
	parked.State = beadstore.StateParked
	if err := store.Put(context.Background(), parked); err != nil {
		t.Fatal(err)
	}
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 950, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
	if rec := r.maintain(t, store, time.Now()); rec.State != beadstore.StateDone || rec.PipelineRunOpen {
		t.Fatalf("parked resolved run = %s open %v, want done and closed", rec.State, rec.PipelineRunOpen)
	}

	r2, store2 := landed(t, plOpts{})
	running := r2.rec
	running.State = beadstore.StateRunning
	_ = store2.Put(context.Background(), running)
	r2.gl.AddPipeline(r2.pid, glab.Pipeline{ID: 950, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
	if rec := r2.maintain(t, store2, time.Now()); !rec.PipelineRunOpen || rec.State != beadstore.StateRunning {
		t.Fatal("a running run was maintained")
	}
}

// An MR that is merged or closed ends the run without a write.
func TestAClosedMRClosesTheRunSilently(t *testing.T) {
	r, store := landed(t, plOpts{mr: true})
	r.gl.SetMRState(r.pid, 5, "merged", time.Now())
	before := len(writes(r.gl))
	if rec := r.maintain(t, store, time.Now()); rec.PipelineRunOpen {
		t.Fatal("run still open on a merged MR")
	}
	if len(writes(r.gl)) != before {
		t.Fatal("closing a run on a merged MR wrote to GitLab")
	}
}

// The maintenance is wired into runSweep itself, once per tick.
func TestRunSweepMaintainsPipelineRuns(t *testing.T) {
	r, store := landed(t, plOpts{})
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 950, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
	if code := runSweep(context.Background(), sweepDeps{Store: store, Runtime: newTestRuntime(), Orders: &fakeOrders{},
		Pipelines: r.gl.Client(), Log: testLogger(io.Discard)}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if st, _ := r.runNote(t); st != pipelinectx.RunStateResolved {
		t.Fatalf("state %s after a sweep tick, want resolved", st)
	}
}

// R5 (verify report), D9: a run resolves only when the NEWEST terminal
// pipeline on the target is green -- not on any green newer than the run's
// last success. Pipelines finish out of order; "newest" is by id (creation),
// so the order they FINISH in must not matter. Both orders:
//
//	older green (905) finishing after a newer red (910): the target is still
//	red -- the run stays open and its tracking issue stays open;
//	newer green (910) after an older red (905): resolved.
func TestOnlyTheNewestTerminalPipelineResolvesARun(t *testing.T) {
	t.Run("older green, newer red: still failing", func(t *testing.T) {
		r, store := landed(t, plOpts{})
		r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 910, Ref: "main", SHA: plSHA, Status: "failed", Source: "push"})
		r.gl.AddJob(r.pid, 910, glab.Job{ID: 9102, Name: "unit-tests", Stage: "test", Status: "failed", FailureReason: "script_failure"})
		r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 905, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
		rec := r.maintain(t, store, time.Now())
		if st, body := r.runNote(t); st != pipelinectx.RunStateDiagnosed {
			t.Fatalf("an older green resolved the run while the newest pipeline (910) is red: state %s\n%s", st, body)
		}
		if !rec.PipelineRunOpen || rec.PipelineLatestFailed != 910 {
			t.Fatalf("record = open %v latest %d, want open with latest failing 910", rec.PipelineRunOpen, rec.PipelineLatestFailed)
		}
		if is := r.gl.IssuesOf(r.pid)[0]; is.State != "opened" {
			t.Fatalf("the tracking issue was closed while the target is red: %s", is.State)
		}
	})
	t.Run("older red, newer green: resolved", func(t *testing.T) {
		r, store := landed(t, plOpts{})
		r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 910, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
		r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 905, Ref: "main", SHA: plSHA, Status: "failed", Source: "push"})
		r.gl.AddJob(r.pid, 905, glab.Job{ID: 9052, Name: "unit-tests", Stage: "test", Status: "failed", FailureReason: "script_failure"})
		rec := r.maintain(t, store, time.Now())
		if st, body := r.runNote(t); st != pipelinectx.RunStateResolved || !strings.Contains(body, plLink(r.proj.WebURL, 910)+" passed") {
			t.Fatalf("the newest pipeline (910) is green but the run is %s:\n%s", st, body)
		}
		if rec.PipelineRunOpen {
			t.Fatal("run still open with the newest pipeline green")
		}
	})
}

// B27 (verify report): a green CHILD pipeline is not a green of the target
// and never resolves a run (D2: the run is the parent's).
func TestAGreenChildPipelineDoesNotResolveTheRun(t *testing.T) {
	r, store := landed(t, plOpts{})
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 950, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "parent_pipeline"})
	rec := r.maintain(t, store, time.Now())
	if st, body := r.runNote(t); st != pipelinectx.RunStateDiagnosed {
		t.Fatalf("a green child pipeline moved the run to %s:\n%s", st, body)
	}
	if !rec.PipelineRunOpen {
		t.Fatal("a green child pipeline closed the run")
	}
	if is := r.gl.IssuesOf(r.pid)[0]; is.State != "opened" {
		t.Fatalf("a green child pipeline closed the tracking issue: %s", is.State)
	}
}

// readCountingStore is Memory, recording every record a read hands the
// sweep: List (by state) and ListOpenPipelineRuns.
type readCountingStore struct {
	*beadstore.Memory
	listed []beadstore.State
	read   map[string]int // anchor -> times handed out by a list
}

func (s *readCountingStore) List(ctx context.Context, st beadstore.State) ([]beadstore.Record, error) {
	s.listed = append(s.listed, st)
	recs, err := s.Memory.List(ctx, st)
	for _, r := range recs {
		s.read[r.BeadAnchor]++
	}
	return recs, err
}

func (s *readCountingStore) ListOpenPipelineRuns(ctx context.Context) ([]beadstore.Record, error) {
	recs, err := s.Memory.ListOpenPipelineRuns(ctx)
	for _, r := range recs {
		s.read[r.BeadAnchor]++
	}
	return recs, err
}

// R6 (verify report): run maintenance reads ONLY open runs. It used to List
// every done, needs-human and parked record on every 30 s tick and filter in
// Go -- a full scan and decode of a set that only grows. A closed (resolved)
// run, and any number of finished records, are never handed to it again.
func TestRunMaintenanceNeverReadsAClosedRun(t *testing.T) {
	r, mem := landed(t, plOpts{})
	store := &readCountingStore{Memory: mem.(*beadstore.Memory), read: map[string]int{}}
	ctx := context.Background()
	for i := 0; i < 50; i++ { // finished work of every kind, none of it an open run
		old := r.rec
		old.BeadAnchor = fmt.Sprintf("gonk:%d:pipeline:ref-old:%04d:s1", r.pid, i)
		old.State, old.PipelineRunOpen = []beadstore.State{beadstore.StateDone, beadstore.StateNeedsHuman, beadstore.StateParked}[i%3], false
		if i%5 == 0 {
			old.Trigger = "triage"
		}
		if err := store.Put(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 950, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
	now := time.Now()
	if rec := r.maintain(t, store, now); rec.PipelineRunOpen {
		t.Fatal("the run was not resolved")
	}
	if len(store.listed) != 0 {
		t.Fatalf("run maintenance listed whole states %v: every finished record is read on every tick", store.listed)
	}
	for a, n := range store.read {
		if a != r.rec.BeadAnchor {
			t.Fatalf("run maintenance read %s (%d times), which is not an open run", a, n)
		}
	}
	store.read = map[string]int{}
	reqs := len(r.gl.Requests())
	r.maintain(t, store, now.Add(time.Hour))
	if len(store.read) != 0 || len(store.listed) != 0 {
		t.Fatalf("after it closed, the run was read again: read %v listed %v", store.read, store.listed)
	}
	if len(r.gl.Requests()) != reqs {
		t.Fatalf("a closed run cost GitLab reads: %v", r.gl.Requests()[reqs:])
	}
}

// The run's OWN last success is never its resolution. If the failing
// pipeline is retried (it is running again), the newest terminal pipeline on
// the target is the green the run started after: nothing has passed since,
// so the run stays open and nothing is written.
func TestTheRunsOwnLastSuccessDoesNotResolveIt(t *testing.T) {
	r, store := landed(t, plOpts{})
	r.gl.SetPipelineStatus(r.pid, 900, "running")
	before := len(writes(r.gl))
	rec := r.maintain(t, store, time.Now())
	if st, _ := r.runNote(t); st != pipelinectx.RunStateDiagnosed || !rec.PipelineRunOpen {
		t.Fatalf("the run's own last success (880) resolved it: note %s, open %v", st, rec.PipelineRunOpen)
	}
	if w := writes(r.gl); len(w) != before {
		t.Fatalf("nothing passed, but maintenance wrote: %v", w[before:])
	}
}
