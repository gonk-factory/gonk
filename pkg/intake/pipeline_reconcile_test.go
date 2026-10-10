package intake

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

const plConfig = "version: 1\nenabled: true\nactions: {triage: true, pipelines: true}\nladder: [qwen-local]\n"

func ourHook(t *testing.T, gl *glabtest.Server, pid int64) glab.Hook {
	t.Helper()
	hooks, err := gl.Client().ListHooks(context.Background(), pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Fatalf("project %d has %d hooks, want exactly 1: %+v", pid, len(hooks), hooks)
	}
	return hooks[0]
}

// D4: pipeline_events ON only for a project that may diagnose pipelines, OFF
// for one that has not opted in -- on the FIRST pass (hook creation is not
// delayed by the move after Classify) -- and edited back OFF when the project
// opts out. Red if ensureHook enables pipeline events for every project.
func TestEnsureHookPipelineEventsOnlyForOptedInProjects(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = botUser
	opted := gl.AddProject("group/opted", glab.AccessMaintainer)
	opted.PutFile(".gonk.yml", []byte(plConfig))
	opted.PutFile(".agent/context.md", []byte("hi"))
	plain := gl.AddProject("group/plain", glab.AccessMaintainer)
	plain.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	plain.PutFile(".agent/context.md", []byte("hi"))
	absent := gl.AddProject("group/absent", glab.AccessMaintainer) // no .gonk.yml at all

	m := newFakeMeter(t)
	m.setPipelines("group/opted", true)
	r := newReconciler(t, gl, m)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if h := ourHook(t, gl, opted.ID); !h.PipelineEvents || !h.IssuesEvents || !h.NoteEvents || !h.MergeRequestsEvents || h.PushEvents {
		t.Fatalf("opted-in project hook = %+v, want pipeline_events on (and the rest unchanged)", h)
	}
	for _, p := range []*glabtest.Project{plain, absent} {
		if h := ourHook(t, gl, p.ID); h.PipelineEvents || !h.IssuesEvents {
			t.Fatalf("%s hook = %+v: pipeline events enabled for a project that never opted in", p.PathWithNamespace, h)
		}
	}

	// Idempotent: a second pass with nothing changed writes nothing.
	before := len(gl.Requests())
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, req := range gl.Requests()[before:] {
		if strings.HasPrefix(req, "PUT") || strings.HasPrefix(req, "POST") {
			t.Fatalf("an unchanged pass wrote to GitLab: %s", req)
		}
	}

	// The project opts out (its .gonk.yml changes, meter answers pipelines
	// off): the next pass edits the SAME hook back to pipeline_events=false.
	opted.PutFile(".gonk.yml", []byte(strings.Replace(plConfig, "pipelines: true", "pipelines: false", 1)))
	m.setPipelines("group/opted", false)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := ourHook(t, gl, opted.ID); h.PipelineEvents {
		t.Fatalf("hook after opting out = %+v, want pipeline_events off", h)
	}
}

// R10 (verify report): a TRANSIENT meter registration failure (5xx,
// unreachable) leaves the project unsynced -- "we do not know its policy" --
// and must not be read as "opted out". The hook keeps the pipeline_events
// setting it already has, across consecutive failures and across a restart
// (a fresh Reconciler with no cache), until meter answers again.
func TestATransientMeterFailureDoesNotFlipPipelineEventsOff(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = botUser
	opted := gl.AddProject("group/opted", glab.AccessMaintainer)
	opted.PutFile(".gonk.yml", []byte(plConfig))
	opted.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	m.setPipelines("group/opted", true)
	r := newReconciler(t, gl, m)
	r.MeterResyncInterval = time.Nanosecond // every pass re-registers
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := ourHook(t, gl, opted.ID); !h.PipelineEvents {
		t.Fatalf("opted-in hook = %+v, want pipeline_events on", h)
	}

	m.failNow.Store(true)
	for pass := 1; pass <= 2; pass++ {
		_, _ = r.ReconcileOnce(context.Background())
		if e, _ := r.Cache.Get(opted.ID); e.Classification.State != StateUnsynced {
			t.Fatalf("pass %d with meter down: state %s, want unsynced (the test is not exercising a transient failure)",
				pass, e.Classification.State)
		}
		if h := ourHook(t, gl, opted.ID); !h.PipelineEvents {
			t.Fatalf("pass %d with meter down turned pipeline_events OFF for an opted-in project: %+v", pass, h)
		}
	}
	restarted := newReconciler(t, gl, m)
	_, _ = restarted.ReconcileOnce(context.Background())
	if h := ourHook(t, gl, opted.ID); !h.PipelineEvents {
		t.Fatalf("a restart with meter down turned pipeline_events OFF: %+v", h)
	}

	// Meter answers again, now with pipelines off: THAT is an answer, and the
	// hook follows it.
	m.failNow.Store(false)
	m.setPipelines("group/opted", false)
	opted.PutFile(".gonk.yml", []byte(strings.Replace(plConfig, "pipelines: true", "pipelines: false", 1)))
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := ourHook(t, gl, opted.ID); h.PipelineEvents {
		t.Fatalf("hook after meter answered pipelines off = %+v, want pipeline_events off", h)
	}
}

// ---------------------------------------------------------------- backfill

type sweepRig struct {
	gl    *glabtest.Server
	proj  *glabtest.Project
	meter *fakeDecide
	disp  *recordingDispatcher
	r     *Reconciler
}

// newSweepRig: one opted-in project, the REAL Dispatch (Decide, facts,
// Gate 1) behind the backfill, meter's Gate-1 answer "run".
func newSweepRig(t *testing.T, optedIn bool) *sweepRig {
	t.Helper()
	gl := glabtest.New(t)
	gl.Me = botUser
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte(plConfig))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	m.setPipelines("group/repo", optedIn)

	r := newReconciler(t, gl, m)
	dec := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Metadata: map[string]string{}}}
	disp := &recordingDispatcher{}
	dp := &Dispatch{
		Dispatcher: disp, Meter: dec, Cache: r.Cache, BotUsername: "gonk", Obs: NopObserver{},
		Log: slog.New(slog.DiscardHandler), KickReconcile: func() {},
		Pipelines: &GitLabPipelineFacts{GL: gl.Client(), BotUserID: plBotID},
	}
	r.Pipelines, r.PipelineGL = dp, gl.Client()
	return &sweepRig{gl: gl, proj: p, meter: dec, disp: disp, r: r}
}

func (s *sweepRig) pass(t *testing.T) Summary {
	t.Helper()
	sum, err := s.r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Errors != 0 {
		t.Fatalf("pass errors: %v", sum.ErrorMsgs)
	}
	return sum
}

// pipeline seeds a pipeline (and its failed job and commit when failed).
func (s *sweepRig) pipeline(status, ref, source string, user glab.User) glab.Pipeline {
	sha := fmt.Sprintf("%040x", len(s.proj.Pipelines)+1)
	s.gl.AddCommit(s.proj.ID, glab.Commit{ID: sha})
	pl := s.gl.AddPipeline(s.proj.ID, glab.Pipeline{Status: status, Ref: ref, SHA: sha, Source: source, User: user})
	if status == "failed" {
		s.gl.AddJob(s.proj.ID, pl.ID, glab.Job{Name: "unit", Stage: "test", Status: "failed", FailureReason: "script_failure"})
	}
	return pl
}

// D4 (a): the NEWEST terminal-for-health pipeline on the default branch, with
// skipped / canceled / manual / running / child pipelines passed over; it is
// failed, so it is dispatched -- and only it.
func TestBackfillDispatchesNewestTerminalFailedOnDefaultBranch(t *testing.T) {
	s := newSweepRig(t, true)
	s.pipeline("success", "main", "push", humanUser)
	s.pipeline("failed", "main", "push", humanUser) // older failure: not the newest signal
	newest := s.pipeline("failed", "main", "push", humanUser)
	for _, st := range []string{"skipped", "canceled", "manual", "running", "created"} {
		s.pipeline(st, "main", "push", humanUser)
	}
	s.pipeline("failed", "main", "parent_pipeline", humanUser) // a child: never the signal

	sum := s.pass(t)
	if len(s.disp.orders) != 1 || sum.PipelinesSwept != 1 || sum.Dispatched != 1 {
		t.Fatalf("orders=%d swept=%d dispatched=%d, want 1/1/1", len(s.disp.orders), sum.PipelinesSwept, sum.Dispatched)
	}
	o := s.disp.orders[0]
	if o.PipelineID != newest.ID || o.Trigger != atags.TriggerPipelineDiagnosis || o.PipelineTarget != "ref:main" {
		t.Fatalf("order = %+v, want pipeline %d on ref:main", o, newest.ID)
	}
}

// The newest health signal is GREEN: an older failure is history, not work.
func TestBackfillIgnoresFailureOlderThanAGreen(t *testing.T) {
	s := newSweepRig(t, true)
	s.pipeline("failed", "main", "push", humanUser)
	s.pipeline("success", "main", "push", humanUser)
	s.pipeline("canceled", "main", "push", humanUser)
	s.pass(t)
	if len(s.disp.orders) != 0 || len(s.meter.reqs) != 0 {
		t.Fatalf("orders=%d decides=%d, want none", len(s.disp.orders), len(s.meter.reqs))
	}
}

// A failed pipeline the BOT triggered is passed over (the receiver's loop
// guard, re-applied on the sweep path).
func TestBackfillSkipsBotTriggeredPipeline(t *testing.T) {
	s := newSweepRig(t, true)
	s.pipeline("failed", "main", "push", botUser)
	s.pass(t)
	if len(s.disp.orders) != 0 || len(s.meter.reqs) != 0 {
		t.Fatalf("orders=%d decides=%d for a bot-triggered pipeline", len(s.disp.orders), len(s.meter.reqs))
	}
}

// Per-anchor suppression holds for the window: the next pass does not even
// ask meter about a run it fired; once the window has passed, it asks again.
func TestBackfillSuppressionWindowHoldsOneHour(t *testing.T) {
	s := newSweepRig(t, true)
	s.pipeline("failed", "main", "push", humanUser)

	s.pass(t)
	if len(s.disp.orders) != 1 || len(s.meter.reqs) != 1 {
		t.Fatalf("pass 1: orders=%d decides=%d", len(s.disp.orders), len(s.meter.reqs))
	}
	anchor := s.disp.orders[0].BeadAnchor
	e, _ := s.r.Cache.Get(s.proj.ID)
	if _, ok := e.PipelineDispatchedAt[anchor]; !ok {
		t.Fatalf("the fire was not remembered by anchor: %+v", e.PipelineDispatchedAt)
	}

	s.pass(t)
	if len(s.disp.orders) != 1 || len(s.meter.reqs) != 1 {
		t.Fatalf("pass 2 (inside the window): orders=%d decides=%d, want still 1/1", len(s.disp.orders), len(s.meter.reqs))
	}

	e, _ = s.r.Cache.Get(s.proj.ID)
	e.PipelineDispatchedAt[anchor] = time.Now().Add(-PipelineSweepSuppressWindow - time.Minute)
	s.r.Cache.Put(s.proj.ID, e)
	s.pass(t)
	if len(s.disp.orders) != 2 || s.disp.orders[1].BeadAnchor != anchor {
		t.Fatalf("pass 3 (window expired): orders=%d, want a second fire of %s", len(s.disp.orders), anchor)
	}
}

// D4 (b): a recently updated open MR whose newest health signal is a failed
// MR pipeline is dispatched on mr:<iid>; an MR not updated within the window
// is not even looked at.
func TestBackfillMergeRequests(t *testing.T) {
	s := newSweepRig(t, true)
	recent, stale := time.Now().Add(-time.Hour), time.Now().Add(-48*time.Hour)
	s.gl.AddMR(s.proj.ID, glab.MergeRequest{IID: 8, SourceBranch: "feature", UpdatedAt: &recent})
	s.gl.AddMR(s.proj.ID, glab.MergeRequest{IID: 9, SourceBranch: "old", UpdatedAt: &stale})
	pl := s.pipeline("failed", "refs/merge-requests/8/head", "merge_request_event", humanUser)
	s.gl.AddMRPipeline(s.proj.ID, 8, pl.ID)
	old := s.pipeline("failed", "refs/merge-requests/9/head", "merge_request_event", humanUser)
	s.gl.AddMRPipeline(s.proj.ID, 9, old.ID)

	s.pass(t)
	if len(s.disp.orders) != 1 || s.disp.orders[0].PipelineTarget != "mr:8" || s.disp.orders[0].PipelineID != pl.ID {
		t.Fatalf("orders = %+v, want one on mr:8 for pipeline %d", s.disp.orders, pl.ID)
	}
	for _, req := range s.gl.Requests() {
		if strings.Contains(req, "/merge_requests/9/") {
			t.Fatalf("an MR outside the recency window was read: %s", req)
		}
	}
}

// A project that has not opted in costs the backfill ZERO pipeline reads.
func TestBackfillSkipsProjectNotOptedIn(t *testing.T) {
	s := newSweepRig(t, false)
	s.pipeline("failed", "main", "push", humanUser)
	s.pass(t)
	for _, req := range s.gl.Requests() {
		if strings.Contains(req, "/pipelines") || strings.Contains(req, "/merge_requests") {
			t.Fatalf("backfill read pipelines of a project that never opted in: %s", req)
		}
	}
	if len(s.disp.orders) != 0 || len(s.meter.reqs) != 0 {
		t.Fatalf("orders=%d decides=%d", len(s.disp.orders), len(s.meter.reqs))
	}
}

// An already-diagnosed run found by the backfill is dropped
// run_already_handled -- the backfill is not a way around D3.
func TestBackfillHonoursAlreadyHandled(t *testing.T) {
	s := newSweepRig(t, true)
	pl := s.pipeline("failed", "main", "push", humanUser)
	s.pass(t)
	if len(s.disp.orders) != 1 {
		t.Fatalf("pass 1 orders = %d", len(s.disp.orders))
	}
	anchor := s.disp.orders[0].BeadAnchor
	// gonk-gate landed the diagnosis on the ref's tracking issue.
	s.gl.AddIssue(s.proj.ID, 30, "opened", "gonk::pipeline-failing")
	for i := range s.proj.Issues {
		if s.proj.Issues[i].IID == 30 {
			s.proj.Issues[i].Author = botUser
			s.proj.Issues[i].Description = pipelinectx.RefIssueMarker("main")
		}
	}
	m, _ := pipelinectx.RunMarker(anchor, pipelinectx.RunStateDiagnosed)
	s.gl.AddNote(s.proj.ID, 30, botUser, m, false)
	// Expire the sweep's own memory so only D3 can stop it.
	e, _ := s.r.Cache.Get(s.proj.ID)
	e.PipelineDispatchedAt = nil
	s.r.Cache.Put(s.proj.ID, e)

	s.pass(t)
	if len(s.disp.orders) != 1 || len(s.meter.reqs) != 1 {
		t.Fatalf("a diagnosed run was asked about again: orders=%d decides=%d (pipeline %d)", len(s.disp.orders), len(s.meter.reqs), pl.ID)
	}
}

// D9: the default-branch landing is a BOT-AUTHORED issue labelled
// gonk::pipeline-failing. The issue sweep must never triage it. Shown with the
// label-prefix optimisation neutralised, so the author guard alone holds it.
func TestIssueSweepNeverTriagesThePipelineTrackingIssue(t *testing.T) {
	now := time.Now()
	gl := &sweepGL{issues: []glab.Issue{{
		IID: 21, Title: "gonk: pipeline failing on main", Author: glab.User{ID: 7},
		Labels: []string{"gonk::pipeline-failing"}, UpdatedAt: now,
		Description: pipelinectx.RefIssueMarker("main"),
	}}}
	sw := &countingSweeper{fire: true}
	r, p := sweepReconciler(gl, sw)
	r.IssueLabelPrefix = "not-gonk::" // only the bot-author guard is left
	n, _, err := r.sweepIssues(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(sw.got) != 0 {
		t.Fatalf("the bot's own pipeline tracking issue was handed to triage (%d)", n)
	}
}
