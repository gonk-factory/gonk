package main

// gonk-tios P7: the dispatch half -- routing, the session name, the checkout
// at the pipeline's commit, what the running record keeps, the run note's
// lifecycle and its ordering against the record write, the refire round trip,
// and the guarantee that a pipeline bead never reaches the issue status note.
// Synthetic fixtures only (plan section 0).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/rig"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// grantRecorder is a rig endpoint that records every grant body.
type grantRecorder struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
}

func newGrantRecorder(t *testing.T) *grantRecorder {
	g := &grantRecorder{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.bodies = append(g.bodies, string(b))
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *grantRecorder) shas(t *testing.T) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, b := range g.bodies {
		var got struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal([]byte(b), &got); err != nil {
			t.Fatal(err)
		}
		out = append(out, got.SHA)
	}
	return out
}

// plArgs is the order intake would fire for plRun's run.
func (r *plRun) args() dispatchArgs {
	return dispatchArgs{
		Project: "example/calc", ProjectID: r.pid, Rig: "example-calc",
		BeadAnchor: r.rec.BeadAnchor, BeadID: r.rec.BeadAnchor, SessionKey: r.rec.SessionKey,
		Trigger: "pipeline-diagnosis", PipelineID: 900, PipelineTarget: r.target.String(),
		PipelineFingerprint: r.rec.PipelineFingerprint, PipelineLastSuccess: 880,
	}
}

type plDispatch struct {
	rt     *testRuntime
	fm     *fakeMeter
	store  beadstore.Store
	grants *grantRecorder
	issue  *recordingApplier // the ISSUE status note's write side: must see nothing
	issueG *glabtest.Server  // ...and its read side
	logs   *bytes.Buffer
	deps   dispatchDeps
}

func (r *plRun) dispatch(t *testing.T, dec meterapi.DecideResponse, store beadstore.Store, args dispatchArgs) (*plDispatch, int) {
	t.Helper()
	return r.dispatchWithHook(t, dec, store, args, nil)
}

// requireNoIssueStatusNote: the issue status-note lifecycle (gonk-l900) is for
// ISSUE triage; a pipeline bead must never reach it.
func (pd *plDispatch) requireNoIssueStatusNote(t *testing.T) {
	t.Helper()
	if len(pd.issue.notes)+len(pd.issue.updated)+len(pd.issue.labels)+len(pd.issue.closed) != 0 {
		t.Fatalf("a pipeline bead reached the issue status note: notes=%v updated=%v labels=%v closed=%v",
			pd.issue.notes, pd.issue.updated, pd.issue.labels, pd.issue.closed)
	}
	if reqs := pd.issueG.Requests(); len(reqs) != 0 {
		t.Fatalf("a pipeline bead read the issue status note's thread: %v", reqs)
	}
}

func plRunDecision() meterapi.DecideResponse {
	d := runDecision("m")
	d.Rung = "r2"
	return d
}

// The run path end to end: the pipeline agent, named for the pipeline, a
// checkout at the PIPELINE'S commit (not the default branch head), the record
// carrying what the sweep checks against, and the diagnosing run note written
// only after the session started and its record landed.
func TestPipelineDispatchStartsTheAgentAtThePipelineCommit(t *testing.T) {
	r := newPLRun(t, plOpts{})
	writesAtStart := -1
	store := beadstore.NewMemory()
	pdx, code := r.dispatchWithHook(t, plRunDecision(), store, r.args(),
		func(runtime.Handle) { writesAtStart = len(writes(r.gl)) })
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, pdx.logs)
	}
	want := runtime.Name("pipeline", r.pid, runtime.KindPipeline, 900, 1)
	starts := pdx.rt.Starts()
	if len(starts) != 1 || starts[0].Name != want || starts[0].Agent != "pipeline" {
		t.Fatalf("starts = %+v, want one %s for agent pipeline", starts, want)
	}
	if shas := pdx.grants.shas(t); len(shas) != 1 || shas[0] != plSHA {
		t.Fatalf("checkout granted at %v, want the pipeline's commit %s (not the branch head %s)", shas, plSHA, plSHAOld)
	}
	if !strings.Contains(string(starts[0].Prompt), "pipeline 900") || !strings.Contains(string(starts[0].Prompt), plSHA) {
		t.Fatalf("prompt does not name the pipeline and the checkout commit:\n%.600s", starts[0].Prompt)
	}
	rec, _, _ := store.Get(context.Background(), r.rec.BeadAnchor)
	if rec.State != beadstore.StateRunning || rec.SessionID != want || rec.CheckoutSHA != plSHA || rec.PipelineSHA != plSHA {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.PipelineJobs) != 1 || !rec.PipelineJobs[0].Excerpted || len(rec.PipelineJobs[0].ExcerptSHA256) != 64 ||
		rec.PipelineExcerptVersion != pipelinectx.ExcerptVersion || rec.PipelineExcerptMaxBytes != pipelinectx.JobBudget(1) ||
		!rec.PipelineRunOpen || rec.PipelineFirstFailed != 900 {
		t.Fatalf("record does not carry what the sweep checks against: %+v", rec)
	}
	if writesAtStart != 0 {
		t.Fatalf("%d GitLab write(s) before the session started; the run note must follow the record", writesAtStart)
	}
	notes := r.gl.IssueNotesOf(r.pid, 1)
	if len(notes) != 1 || len(pipelinectx.RunMarkerStates(notes[0].Body, r.rec.BeadAnchor)) != 1 ||
		pipelinectx.RunMarkerStates(notes[0].Body, r.rec.BeadAnchor)[0] != pipelinectx.RunStateDiagnosing {
		t.Fatalf("no diagnosing run note: %+v", notes)
	}
	pdx.requireNoIssueStatusNote(t)
}

func (r *plRun) dispatchWithHook(t *testing.T, dec meterapi.DecideResponse, store beadstore.Store, args dispatchArgs, after func(runtime.Handle)) (*plDispatch, int) {
	t.Helper()
	if store == nil {
		store = beadstore.NewMemory()
	}
	pd := &plDispatch{rt: newTestRuntime(), fm: &fakeMeter{resp: dec}, store: store,
		grants: newGrantRecorder(t), issue: &recordingApplier{}, issueG: glabtest.New(t), logs: &bytes.Buffer{}}
	pd.rt.afterStart = after
	forge := promptTestForge()
	forge.resolve = func(string) (string, error) { return plSHAOld, nil }
	pd.deps = dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(pd.fm.server(t)), Runtime: pd.rt, Store: store,
		Forge: forge, Pipelines: r.gl.Client(),
		Apply: pd.issue, GL: pd.issueG.Client(), BotUsername: "gonk",
		Args: args, Clock: newFakeClock(), Log: testLogger(pd.logs),
		Rig: &rig.Client{BaseURL: pd.grants.srv.URL}, RigBaseURL: pd.grants.srv.URL,
	}
	return pd, runDispatch(context.Background(), pd.deps)
}

// PROJECT ISOLATION / RUN IDENTITY: an order whose identity does not rebuild
// its own anchor -- another fingerprint, another project id -- is refused
// before meter is asked, and nothing starts.
func TestPipelineDispatchRefusesAnOrderWhoseIdentityIsNotItsAnchor(t *testing.T) {
	r := newPLRun(t, plOpts{})
	for name, mutate := range map[string]func(*dispatchArgs){
		"another fingerprint": func(a *dispatchArgs) { a.PipelineFingerprint = "0123456789abcdef" },
		"another project":     func(a *dispatchArgs) { a.ProjectID = r.pid + 1 },
		"another target":      func(a *dispatchArgs) { a.PipelineTarget = "mr:5" },
		"no pipeline":         func(a *dispatchArgs) { a.PipelineID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			a := r.args()
			mutate(&a)
			pd, code := r.dispatch(t, plRunDecision(), nil, a)
			if code != 2 || pd.fm.calls != 0 || len(pd.rt.Starts()) != 0 {
				t.Fatalf("exit=%d meter calls=%d starts=%d, want a refusal before meter", code, pd.fm.calls, len(pd.rt.Starts()))
			}
		})
	}
}

// Defer and deny: the record is written, THEN the run note says so (and only
// the run note -- never the issue status note).
func TestPipelineDeferAndDenyWriteTheRunNoteAfterTheRecord(t *testing.T) {
	for _, tc := range []struct {
		dec   meterapi.DecideResponse
		state beadstore.State
		note  string
	}{
		{meterapi.DecideResponse{Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours, RetryAfter: time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)},
			beadstore.StateParked, pipelinectx.RunStateDeferred},
		{meterapi.DecideResponse{Decision: meterapi.DecisionDeny, Reason: meterapi.ReasonLadderExhausted},
			beadstore.StateNeedsHuman, pipelinectx.RunStateDenied},
	} {
		t.Run(tc.note, func(t *testing.T) {
			r := newPLRun(t, plOpts{})
			store := beadstore.NewMemory()
			pd, code := r.dispatch(t, tc.dec, store, r.args())
			if code != 0 {
				t.Fatalf("exit = %d", code)
			}
			rec, _, _ := store.Get(context.Background(), r.rec.BeadAnchor)
			if rec.State != tc.state || rec.PipelineID != 900 || !rec.PipelineRunOpen || rec.PipelineTarget != "ref:main" {
				t.Fatalf("record = %+v", rec)
			}
			notes := r.gl.IssueNotesOf(r.pid, 1)
			if len(notes) != 1 {
				t.Fatalf("notes = %+v", notes)
			}
			if st := pipelinectx.RunMarkerStates(notes[0].Body, r.rec.BeadAnchor); len(st) != 1 || st[0] != tc.note {
				t.Fatalf("marker states = %v, want [%s]", st, tc.note)
			}
			pd.requireNoIssueStatusNote(t)

			// The SAME decision with a record write that fails: no note at all.
			r2 := newPLRun(t, plOpts{})
			pd2, _ := r2.dispatch(t, tc.dec, &plFailWritesStore{Store: beadstore.NewMemory(), failPutIf: true}, r2.args())
			if w := writes(r2.gl); len(w) != 0 {
				t.Fatalf("the run note was written although the record write failed: %v", w)
			}
			pd2.requireNoIssueStatusNote(t)
		})
	}
}

// The running record's write fails: the session is stopped and NO diagnosing
// note is written -- a note must never report a record that did not land.
func TestNoDiagnosingNoteWhenTheRunningRecordFails(t *testing.T) {
	r := newPLRun(t, plOpts{})
	pd, code := r.dispatch(t, plRunDecision(), &plFailWritesStore{Store: beadstore.NewMemory(), failPut: true}, r.args())
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if w := writes(r.gl); len(w) != 0 {
		t.Fatalf("a run note was written for a record that did not land: %v", w)
	}
	pd.requireNoIssueStatusNote(t)
}

// Without the pipeline (or its jobs) there is nothing to diagnose: no session.
func TestNoSessionWhenThePipelineCannotBeRead(t *testing.T) {
	r := newPLRun(t, plOpts{})
	r.gl.FailAlways("GET", "/api/v4/projects/1/pipelines/900", 500)
	store := beadstore.NewMemory()
	pd, code := r.dispatch(t, plRunDecision(), store, r.args())
	if code != 1 || len(pd.rt.Starts()) != 0 {
		t.Fatalf("exit=%d starts=%d, want 1 and none", code, len(pd.rt.Starts()))
	}
	if !strings.Contains(pd.logs.String(), "cannot build the pipeline's context") {
		t.Fatalf("not logged:\n%s", pd.logs)
	}
}

// The run identity survives a park and a refire: the sweep's order vars,
// read back exactly as main.go reads them (dispatchArgsFrom), rebuild the
// anchor, and the session keeps the RECORDED pipeline even when a later
// failure of the same run is what fired it.
func TestPipelineRunIdentityRoundTripsThroughRefire(t *testing.T) {
	r := newPLRun(t, plOpts{})
	store := beadstore.NewMemory()
	if _, code := r.dispatch(t, meterapi.DecideResponse{Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours}, store, r.args()); code != 0 {
		t.Fatalf("defer exit = %d", code)
	}
	parked, _, _ := store.Get(context.Background(), r.rec.BeadAnchor)
	orders := &fakeOrders{}
	if !refire(context.Background(), sweepDeps{Orders: orders, Log: testLogger(io.Discard)}, parked) {
		t.Fatal("refire refused")
	}
	vars := orders.orders()[0].Vars
	args := dispatchArgsFrom(func(k string) string { return vars[k] })
	if err := checkPipelineIdentity(args); err != nil {
		t.Fatalf("refired vars do not rebuild the anchor: %v (%v)", err, vars)
	}
	args.PipelineID = 950 // a later failure of the same run fired this
	pd, code := r.dispatch(t, plRunDecision(), store, args)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, pd.logs)
	}
	want := runtime.Name("pipeline", r.pid, runtime.KindPipeline, 900, 1)
	if starts := pd.rt.Starts(); len(starts) != 1 || starts[0].Name != want {
		t.Fatalf("starts = %+v, want %s (the recorded pipeline)", starts, want)
	}
	rec, _, _ := store.Get(context.Background(), r.rec.BeadAnchor)
	if rec.BeadAnchor != r.rec.BeadAnchor || rec.PipelineID != 900 || rec.PipelineLatestFailed != 950 ||
		rec.PipelineFingerprint != r.rec.PipelineFingerprint || rec.PipelineLastSuccess != 880 {
		t.Fatalf("record after the refire = %+v", rec)
	}
}

// The sweep's re-sling of a pipeline bead: still never the issue status note.
func TestPipelineReslingNeverReachesTheIssueStatusNote(t *testing.T) {
	r := newPLRun(t, plOpts{})
	applier := &recordingApplier{}
	issueG := glabtest.New(t)
	rt := newTestRuntime()
	rt.finishSession(t, r.rec.SessionID, "no fence at all\n")
	store := beadstore.NewMemory()
	_ = store.Put(context.Background(), r.rec)
	fm := &fakeOutcomeMeter{outcomeNext: "escalate",
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 500, AsOf: time.Now().Add(time.Hour)}}
	orders := &fakeOrders{}
	runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, Store: store,
		GL: issueG.Client(), Apply: applier, Pipelines: r.gl.Client(), BotUsername: "gonk", PackDir: repoPackDir,
		Log: testLogger(io.Discard), SpendPollInterval: time.Millisecond, SpendDeadline: time.Second,
	})
	if len(orders.orders()) != 1 {
		t.Fatalf("orders = %+v, want the re-sling", orders.orders())
	}
	if len(applier.notes)+len(applier.updated)+len(applier.labels)+len(applier.closed) != 0 || len(issueG.Requests()) != 0 {
		t.Fatalf("the re-sling of a pipeline bead touched the issue status note: %+v %v", applier, issueG.Requests())
	}
}

// plFailWritesStore fails Put or PutIf.
type plFailWritesStore struct {
	beadstore.Store
	failPut, failPutIf bool
}

var errPLWrite = errors.New("store unwritable (test)")

func (s *plFailWritesStore) Put(ctx context.Context, r beadstore.Record) error {
	if s.failPut && r.State == beadstore.StateRunning {
		return errPLWrite
	}
	return s.Store.Put(ctx, r)
}

func (s *plFailWritesStore) PutIf(ctx context.Context, r beadstore.Record, rev string) (string, error) {
	if s.failPutIf {
		return "", errPLWrite
	}
	return s.Store.PutIf(ctx, r, rev)
}

// grantCheckoutAt pins only a FULL commit SHA: a branch name or a short sha
// is refused, and nothing is registered.
func TestGrantCheckoutAtRefusesAnythingButAFullSHA(t *testing.T) {
	g := newGrantRecorder(t)
	d := dispatchDeps{Rig: &rig.Client{BaseURL: g.srv.URL}, RigBaseURL: g.srv.URL, Log: testLogger(io.Discard)}
	for _, bad := range []string{"main", "111111111111", ""} {
		if url, sha, err := grantCheckoutAt(context.Background(), d, pipelineAgent, "gonk-pipeline-p1-pl900-a1", bad); err == nil || url != "" || sha != "" {
			t.Errorf("grantCheckoutAt(%q) = %q, %q, %v; want a refusal", bad, url, sha, err)
		}
	}
	if shas := g.shas(t); len(shas) != 0 {
		t.Fatalf("grants registered for non-SHA commits: %v", shas)
	}
	if _, sha, err := grantCheckoutAt(context.Background(), d, pipelineAgent, "gonk-pipeline-p1-pl900-a1", plSHA); err != nil || sha != plSHA {
		t.Fatalf("a full SHA: %q %v", sha, err)
	}
}
