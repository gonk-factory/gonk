package main

// gonk-tios P8: PIPELINE TRIAGE, END TO END, BELOW THE LIVE CLUSTER.
//
// Every earlier part tested its own seam: P3 intake against a fact stub, P7
// dispatch from a hand-built order, the sweep from a hand-built record. Each
// of those hand-built things is an assumption about what the previous hop
// produces, and nothing checked the assumptions agree. This file drives the
// whole path in one process, each hop fed ONLY by what the previous hop
// actually emitted:
//
//	GitLab Pipeline Hook (raw JSON, real ghook.Handler: token, parse, dedupe,
//	  loop guard)
//	-> gonk-intake Dispatch.Handle (real: cache gate, GitLabPipelineFacts over
//	  the fake GitLab, decidePipeline, Gate 1)
//	-> the order on the WIRE (real HTTPDispatcher -> Gas City order-run POST,
//	  captured as the vars map)
//	-> gonk-gate dispatch reading those vars exactly as main.go does
//	  (GC_WEBHOOK_ARG_* env -> envArg -> dispatchArgsFrom)
//	-> runDispatch (real: identity check, Gate 2, context + prompt, checkout,
//	  pkg/runtime/fake session, running record, run note)
//	-> the "agent": it reads the prompt the session was started with and
//	  answers with a batch quoting a line it found THERE
//	-> runSweep (real: judge, D8 chain, landing, run maintenance)
//	-> the landing, read back from the fake GitLab -- and by intake's own fact
//	  source on the next delivery.
//
// Fakes are only at the edges: GitLab (pkg/glab/glabtest), meter (two small
// HTTP/DecideClient fakes), Gas City's order route (an httptest capture), the
// rig grant endpoint, and the session runtime (pkg/runtime/fake).
//
// SYNTHETIC AND NEUTRAL (plan section 0, the held-out rule): a made-up
// project, a failing unit test, a lint complaint, a failing build. Nothing
// here encodes or hints at any infrastructure cause.
//
// TIME (docs/testing-time.md): no sleeps. The sweep's clock is injected and
// moved forward explicitly where run maintenance's five-minute spacing would
// otherwise hide a step.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/ghook"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/intake"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/rig"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

const (
	rtHookSecret = "p8-roundtrip-webhook-secret-0123456789"
	rtHumanID    = 99
	rtMRIID      = 5
	rtMRBranch   = "feature/sum"

	// rtBuildLog is the third synthetic failure: the build step itself fails.
	rtBuildLog = "$ ./build\n" +
		"compiling calc\n" +
		"calc/sum.py:2: syntax error near 'retrun'\n" +
		"ERROR: Job failed: exit code 2\n"
	rtBuildLine = "calc/sum.py:2: syntax error near 'retrun'"
)

// rtOrderVarNames is every order var either side of the wire knows. Before each
// dispatch every one is (re)set, absent ones to "", so a var left over from an
// earlier order can never leak into a later one.
var rtOrderVarNames = []string{
	"trigger", "project", "project_id", "rig", "issue_iid", "note_id", "discussion_id", "mr_iid",
	"session_key", "bead_anchor", "bead_id", "config_hash", "rung", "model", "metadata_json",
	"key_ref", "reservation_id", "pipeline_id", "pipeline_target", "pipeline_fingerprint",
	"pipeline_last_success", "pipeline_fix_epoch", fireByVar,
}

// rtGate1 is meter's /decide as intake sees it: always run, attempt 1 (meter
// hands back the open reservation for an anchor it already reserved).
type rtGate1 struct {
	mu   sync.Mutex
	reqs []meterapi.DecideRequest
}

func (m *rtGate1) Decide(_ context.Context, req meterapi.DecideRequest) (*meterapi.DecideResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	return &meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "r2", Model: "m", Attempt: 1, ReservationID: "rsv-" + req.BeadID,
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Metadata: map[string]string{"gonk_project": req.Project, "gonk_attempt": "1"},
	}, nil
}

func (m *rtGate1) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reqs)
}

// rtCity is Gas City's order-run route: it records the vars of every POST.
type rtCity struct {
	mu     sync.Mutex
	orders []map[string]string
}

func (c *rtCity) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/order/gonk-dispatch/run") {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		var body struct {
			Vars map[string]string `json:"vars"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.orders = append(c.orders, body.Vars)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"accepted"}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (c *rtCity) all() []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]string(nil), c.orders...)
}

// rtWorld is one fake GitLab, intake, gonk-gate and a session runtime, wired
// as production wires them.
type rtWorld struct {
	t      *testing.T
	mr     bool
	ref    string // the ref the target's pipelines run on
	source string // their Pipeline Hook source
	gl     *glabtest.Server
	proj   *glabtest.Project // actions.pipelines: true
	other  *glabtest.Project // actions.triage only: NOT opted in
	target pipelinectx.Target

	hook      *ghook.Handler
	sunk      []*ghook.Event
	intake    *intake.Dispatch
	intakeLog *bytes.Buffer
	gate1     *rtGate1
	city      *rtCity

	rt        *testRuntime
	store     beadstore.Store
	gate2     *fakeMeter
	grants    *grantRecorder
	issueSide *recordingApplier // the ISSUE triage write side: a pipeline must never reach it
	om        *fakeOutcomeMeter
	resling   *fakeOrders
	gateLog   *bytes.Buffer

	delivery int
	skew     time.Duration

	// gonk-2usw part DISPATCH (pipeline_roundtrip_pipefix_test.go). All zero
	// for every other test, which is production's wiring today: no fix pass,
	// no preparer, the repository's own pack.
	fixLive   bool
	fixConfig pipefixConfigSource
	fixPrep   pipefixPreparer
	packDir   string
}

// pack is the pack directory this world's gonk-gate reads.
func (w *rtWorld) pack() string {
	if w.packDir != "" {
		return w.packDir
	}
	return repoPackDir
}

func newRTWorld(t *testing.T, mr bool) *rtWorld {
	t.Helper()
	w := &rtWorld{t: t, mr: mr, ref: "main", source: "push",
		intakeLog: &bytes.Buffer{}, gateLog: &bytes.Buffer{}, gate1: &rtGate1{}, city: &rtCity{}}
	w.gl = glabtest.New(t)
	w.gl.Me = glab.User{ID: plBotID, Username: "gonk"}
	w.proj = w.gl.AddProject("example/calc", glab.AccessMaintainer)
	w.other = w.gl.AddProject("example/other", glab.AccessMaintainer)
	w.target = pipelinectx.RefTarget("main")
	if mr {
		w.ref, w.source, w.target = rtMRBranch, "merge_request_event", pipelinectx.MRTarget(rtMRIID)
		w.gl.AddMR(w.proj.ID, glab.MergeRequest{IID: rtMRIID, Title: "Add sum helper", SourceBranch: rtMRBranch, TargetBranch: "main"})
	}
	// The run's last green pipeline, and the file the diagnosis cites, at the
	// commit every failing pipeline below runs on.
	w.addPipeline(880, plSHAGreen, "success")
	w.gl.AddCommit(w.proj.ID, glab.Commit{ID: plSHA, Title: "Add sum helper", AuthorName: "Example Author",
		AuthoredDate: plTime(0), Message: "Add sum helper\n"})
	w.gl.SetCILint(w.proj.ID, plSHA, glab.CILint{Valid: true, MergedYAML: plMergedYAML})
	w.proj.PutFileOn(plSHA, "calc/sum.py", []byte("def total(xs):\n    return 0\n"))

	// ---- gonk-intake, wired as cmd/gonk-intake wires it ----
	v, err := ghook.NewVerifier(rtHookSecret)
	if err != nil {
		t.Fatal(err)
	}
	w.hook, err = ghook.NewHandler(v, ghook.NewDeduper(time.Hour, 1000), plBotID,
		func(ev *ghook.Event) bool { w.sunk = append(w.sunk, ev); return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.hook.Log = slog.New(slog.NewTextHandler(w.intakeLog, nil))
	cache := intake.NewCache()
	cache.Put(w.proj.ID, rtEntry(w.proj, true))
	cache.Put(w.other.ID, rtEntry(w.other, false))
	w.intake = &intake.Dispatch{
		Dispatcher:  intake.NewHTTPDispatcher(w.city.server(t), nil, nil),
		Meter:       w.gate1,
		Cache:       cache,
		BotUsername: "gonk",
		Obs:         intake.NopObserver{},
		Log:         slog.New(slog.NewTextHandler(w.intakeLog, nil)),
		Pipelines:   &intake.GitLabPipelineFacts{GL: w.gl.Client(), BotUserID: plBotID},
		KickReconcile: func() {
			t.Error("intake kicked a reconcile: a pipeline event for a cached project must not need one")
		},
	}

	// ---- gonk-gate ----
	w.rt = newTestRuntime()
	w.store = beadstore.NewMemory()
	w.gate2 = &fakeMeter{resp: plRunDecision()}
	w.grants = newGrantRecorder(t)
	w.issueSide = &recordingApplier{}
	w.om = &fakeOutcomeMeter{outcomeNext: "done",
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 500, AsOf: time.Now().Add(240 * time.Hour)}}
	w.resling = &fakeOrders{}
	return w
}

// rtEntry is the reconciled cache entry of a valid, onboarded project. Only
// actions.pipelines differs between the two projects.
func rtEntry(p *glabtest.Project, pipelines bool) intake.Entry {
	cfg := "version: 1\nenabled: true\nactions: { triage: true, pipelines: " + strconv.FormatBool(pipelines) + " }\nladder: [local-1, local-2]\n"
	mr := &meterapi.ProjectResponse{
		Project: p.PathWithNamespace, Rig: intake.RigName(p.PathWithNamespace), State: meterapi.StateActive,
		Effective: &meterapi.Effective{Enabled: true,
			Actions: meterapi.Actions{Triage: true, Pipelines: pipelines},
			Ladder:  []string{"local-1", "local-2"},
			Triage:  meterapi.Triage{LabelPrefix: "gonk::"}},
		KeyRef:     meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		ConfigHash: "sha256:p8",
	}
	return intake.Entry{
		Project:        p.Project,
		Classification: intake.Classify(intake.Observation{Member: true, ConfigBytes: []byte(cfg), AgentDirPresent: true}, mr),
		LastReconcile:  time.Now(),
	}
}

// addPipeline adds a pipeline on the world's target (linked to the MR for an
// MR target).
func (w *rtWorld) addPipeline(id int64, sha, status string) {
	w.addPipelineOn(w.proj, id, w.ref, sha, status)
	if w.mr {
		w.gl.AddMRPipeline(w.proj.ID, rtMRIID, id)
	}
}

func (w *rtWorld) addPipelineOn(p *glabtest.Project, id int64, ref, sha, status string) {
	w.gl.AddPipeline(p.ID, glab.Pipeline{ID: id, Ref: ref, SHA: sha, Status: status, Source: w.sourceOf(p), FinishedAt: plTime(int(id % 60))})
}

// sourceOf is the pipeline source of p's pipelines: the target's for the
// opted-in project, a plain push on the default branch for the other one.
func (w *rtWorld) sourceOf(p *glabtest.Project) string {
	if p == w.proj {
		return w.source
	}
	return "push"
}

// failPipeline adds a failed pipeline whose failed jobs are exactly `failing`
// (name -> log); every other job of {build, unit-tests, lint} succeeded. Job
// ids are pipeline*10 + slot, so each pipeline's jobs are its own.
func (w *rtWorld) failPipeline(id int64, failing map[string]string) {
	w.t.Helper()
	w.addPipeline(id, plSHA, "failed")
	for slot, j := range []struct{ name, stage string }{{"build", "build"}, {"unit-tests", "test"}, {"lint", "test"}} {
		jid := id*10 + int64(slot+1)
		log, failed := failing[j.name]
		job := glab.Job{ID: jid, Name: j.name, Stage: j.stage, Status: "success"}
		if failed {
			job.Status, job.FailureReason = "failed", "script_failure"
		}
		w.gl.AddJob(w.proj.ID, id, job)
		if failed {
			w.gl.SetTrace(w.proj.ID, jid, []byte(log))
		}
	}
}

// hookBody is a GitLab Pipeline Hook payload for pipeline id of project p.
func (w *rtWorld) hookBody(p *glabtest.Project, id int64, ref, sha, status string) []byte {
	w.t.Helper()
	body := map[string]any{
		"object_kind": "pipeline",
		"object_attributes": map[string]any{
			"id": id, "iid": id % 1000, "ref": ref, "tag": false, "sha": sha,
			"source": w.sourceOf(p), "status": status, "detailed_status": status,
		},
		"merge_request": nil,
		"user":          map[string]any{"id": rtHumanID, "name": "A Human", "username": "human"},
		"project": map[string]any{"id": p.ID, "name": "calc", "path_with_namespace": p.PathWithNamespace,
			"default_branch": "main", "web_url": p.WebURL},
		"builds": []any{},
	}
	if w.mr && p == w.proj {
		body["merge_request"] = map[string]any{"id": 1000 + rtMRIID, "iid": rtMRIID, "source_branch": rtMRBranch,
			"source_project_id": p.ID, "target_branch": "main", "target_project_id": p.ID, "state": "opened"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		w.t.Fatal(err)
	}
	return raw
}

// delivery is what one webhook delivery led to on the intake side.
type delivery struct {
	status int
	orders []map[string]string // the orders it put on the wire
	log    string              // intake's records for this delivery
}

// deliver POSTs the Pipeline Hook for pipeline id (of the opted-in project,
// unless p says otherwise) to the real receiver, then hands what it accepted to
// intake's Dispatch.Handle, exactly once each.
func (w *rtWorld) deliver(id int64, status string) delivery {
	w.t.Helper()
	return w.deliverFor(w.proj, id, w.ref, status)
}

func (w *rtWorld) deliverFor(p *glabtest.Project, id int64, ref, status string) delivery {
	w.t.Helper()
	w.delivery++
	logAt, ordersAt, sunkAt := w.intakeLog.Len(), len(w.city.all()), len(w.sunk)
	req := httptest.NewRequest(http.MethodPost, "/hook/gitlab", bytes.NewReader(w.hookBody(p, id, ref, plSHA, status)))
	req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
	req.Header.Set("X-Gitlab-Token", rtHookSecret)
	req.Header.Set("X-Gitlab-Event-UUID", fmt.Sprintf("p8-delivery-%04d", w.delivery))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.hook.ServeHTTP(rec, req)
	for _, ev := range w.sunk[sunkAt:] {
		w.intake.Handle(context.Background(), ev)
	}
	return delivery{status: rec.Code, orders: w.city.all()[ordersAt:], log: w.intakeLog.String()[logAt:]}
}

// requireDropped asserts the delivery was accepted by the receiver and dropped
// by intake for exactly this reason, with no order on the wire.
func (d delivery) requireDropped(t *testing.T, reason string) {
	t.Helper()
	if d.status != http.StatusOK {
		t.Fatalf("receiver answered %d, want 200", d.status)
	}
	if len(d.orders) != 0 {
		t.Fatalf("intake fired %d order(s), want none (drop %s): %v", len(d.orders), reason, d.orders)
	}
	if !strings.Contains(d.log, "reason="+reason) {
		t.Fatalf("intake did not record the drop reason %s:\n%s", reason, d.log)
	}
}

// requireOrder asserts the delivery put exactly one pipeline order on the wire
// and returns its vars.
func (d delivery) requireOrder(t *testing.T) map[string]string {
	t.Helper()
	if d.status != http.StatusOK || len(d.orders) != 1 {
		t.Fatalf("receiver %d, %d order(s), want 200 and exactly one order\nintake log:\n%s", d.status, len(d.orders), d.log)
	}
	o := d.orders[0]
	if o["trigger"] != "pipeline-diagnosis" || o["issue_iid"] != "" {
		t.Fatalf("order vars = %v, want a pipeline-diagnosis order with no issue", o)
	}
	return o
}

// dispatch runs gonk-gate's dispatch for an order, reading its params exactly
// as main.go does: from GC_WEBHOOK_ARG_* through envArg.
func (w *rtWorld) dispatch(vars map[string]string) int {
	w.t.Helper()
	for _, k := range rtOrderVarNames {
		w.t.Setenv("GC_WEBHOOK_ARG_"+strings.ToUpper(k), vars[k])
	}
	forge := promptTestForge()
	forge.resolve = func(string) (string, error) { return plSHAOld, nil } // the branch head: NOT the pipeline's commit
	return runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(w.gate2.server(w.t)), Runtime: w.rt, Store: w.store,
		Forge: forge, Pipelines: w.gl.Client(),
		Apply: w.issueSide, GL: w.gl.Client(), BotUsername: "gonk",
		Args: dispatchArgsFrom(envArg), Clock: newFakeClock(), Log: testLogger(w.gateLog),
		Rig: &rig.Client{BaseURL: w.grants.srv.URL}, RigBaseURL: w.grants.srv.URL,
		PackDir: w.pack(), PrepareFix: w.fixPrep, FixConfig: w.fixConfig,
	})
}

// sessions is every session start the runtime has seen.
func (w *rtWorld) sessions() []runtime.Spec { return w.rt.Starts() }

// answer finishes the session as the agent would: it reads the prompt the
// session was STARTED with, finds the quoted line there (so a line that the
// prompt did not carry fails the test, not the gate), and emits the batch.
func (w *rtWorld) answer(session string, b effects.Batch, mustQuote ...string) {
	w.t.Helper()
	var spec *runtime.Spec
	for _, s := range w.sessions() {
		if s.Name == session {
			spec = &s
		}
	}
	if spec == nil {
		w.t.Fatalf("no session %s was started; starts: %v", session, w.sessionNames())
	}
	for _, q := range mustQuote {
		if !strings.Contains(string(spec.Prompt), q) {
			w.t.Fatalf("the prompt of %s does not carry %q, so the agent could not have quoted it", session, q)
		}
	}
	h := runtime.Handle(session)
	w.rt.SetOutput(h, []byte(plSessionOutput(w.t, b)))
	w.rt.SetPhase(h, runtime.PhaseSucceeded, "")
}

func (w *rtWorld) sessionNames() []string {
	var out []string
	for _, s := range w.sessions() {
		out = append(out, s.Name)
	}
	return out
}

// sweep runs one gonk-sweep tick at the world's clock.
func (w *rtWorld) sweep() {
	w.t.Helper()
	skew := w.skew
	if code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(w.om.server(w.t)), Runtime: w.rt, Orders: w.resling, Store: w.store,
		GL: w.gl.Client(), Apply: w.issueSide, Pipelines: w.gl.Client(),
		BotUsername: "gonk", PackDir: w.pack(), Log: testLogger(w.gateLog),
		FixesLive: w.fixLive, FixConfig: w.fixConfig,
		Now:               func() time.Time { return time.Now().Add(skew) },
		SpendPollInterval: time.Millisecond, SpendDeadline: time.Second,
	}); code != 0 {
		w.t.Fatalf("sweep exit = %d\n%s", code, w.gateLog)
	}
}

// tick moves the sweep's clock past run maintenance's five-minute spacing and
// sweeps, so every open run is looked at.
func (w *rtWorld) tick() {
	w.t.Helper()
	w.skew += pipelineRunCheckEvery + time.Minute
	w.sweep()
}

// runNotes is every bot note on the world's landing: the MR's notes, or the
// notes of every tracking issue, oldest first.
func (w *rtWorld) runNotes() []glab.Note {
	if w.mr {
		return w.gl.MRNotesOf(w.proj.ID, rtMRIID)
	}
	var out []glab.Note
	for _, is := range w.gl.IssuesOf(w.proj.ID) {
		out = append(out, w.gl.IssueNotesOf(w.proj.ID, is.IID)...)
	}
	return out
}

// runState is the state the landing's marker for this anchor carries; it
// fails unless exactly one bot note carries exactly one marker for it.
func (w *rtWorld) runState(anchor string) (string, string) {
	w.t.Helper()
	var found []string
	var body string
	for _, n := range w.runNotes() {
		if st := pipelinectx.RunMarkerStates(n.Body, anchor); len(st) > 0 {
			if n.Author.ID != plBotID {
				w.t.Fatalf("a run marker for %s in a note by %+v, want the bot", anchor, n.Author)
			}
			found = append(found, st...)
			body = n.Body
		}
	}
	if len(found) != 1 {
		w.t.Fatalf("anchor %s: %d run marker(s) on the landing (%v), want exactly one", anchor, len(found), found)
	}
	return found[0], body
}

func (w *rtWorld) record(anchor string) beadstore.Record {
	w.t.Helper()
	rec, ok, err := w.store.Get(context.Background(), anchor)
	if err != nil || !ok {
		w.t.Fatalf("record %s: found=%v err=%v", anchor, ok, err)
	}
	return rec
}

func (w *rtWorld) writes() []string { return writes(w.gl) }

// classLabels is the gonk::pipeline-* labels on the MR, or on the open
// tracking issue(s).
func (w *rtWorld) labels() []string {
	if w.mr {
		return w.gl.MRsOf(w.proj.ID)[0].Labels
	}
	var out []string
	for _, is := range w.gl.IssuesOf(w.proj.ID) {
		if is.State == "opened" {
			out = append(out, is.Labels...)
		}
	}
	return out
}

func (w *rtWorld) requireNoIssueTriage() {
	w.t.Helper()
	a := w.issueSide
	if len(a.notes)+len(a.updated)+len(a.labels)+len(a.closed) != 0 {
		w.t.Fatalf("the pipeline path reached the ISSUE triage write side: %+v", a)
	}
}

func sessionName(pid, pipelineID int64) string {
	return runtime.Name(pipelineAgent, pid, runtime.KindPipeline, pipelineID, 1)
}

// rtDiagnosis is a valid diagnosis quoting `line` from `job`'s log.
func rtDiagnosis(class effects.Classification, job, line string, mutate func(*effects.Effect)) effects.Batch {
	b := plDiagnosis(func(e *effects.Effect) {
		e.Classification = class
		e.Evidence = []effects.EvidenceItem{{Job: job, Line: line}, {Job: job, Fact: effects.FactFailureReason}}
	})
	if mutate != nil {
		mutate(&b.Effects[0])
	}
	return b
}

// TestPipelineRoundTrip is the whole feature on both targets, in order:
//
//  1. a failed pipeline's hook is accepted for the opted-in project and dropped
//     (action_disabled, no GitLab read, no meter call) for one that is not;
//  2. intake's order, read back through the wire and the env, dispatches ONE
//     pipeline session named for the pipeline, at the pipeline's commit, whose
//     prompt carries the failing line; the run note says diagnosing;
//  3. a second identical failure WHILE the session runs: same anchor, no
//     second session;
//  4. the agent answers; the sweep validates and lands it: the note says
//     diagnosed with the RunMarker, the class label is set;
//  5. a third identical failure AFTER the diagnosis: intake reads the landing
//     and drops it run_already_handled -- meter is not even asked; run
//     maintenance moves the note's latest-failing line to it;
//  6. a green pipeline: the hook is dropped pipeline_not_failed; maintenance
//     resolves the run (note resolved, MR label removed / issue closed);
//  7. the same failure AFTER the green is a NEW run: new anchor, new session;
//  8. a DIFFERENT failure supersedes that run, and is itself a new run whose
//     diagnosis lands beside it.
func TestPipelineRoundTrip(t *testing.T) {
	for _, mr := range []bool{false, true} {
		name := "default-branch"
		if mr {
			name = "merge-request"
		}
		t.Run(name, func(t *testing.T) { roundTrip(t, mr) })
	}
}

func roundTrip(t *testing.T, mr bool) {
	w := newRTWorld(t, mr)
	pid := w.proj.ID
	unitFails := map[string]string{"unit-tests": plTestLog}

	// ---- 1. the hook: opted in -> order; not opted in -> dropped --------------
	w.addPipelineOn(w.other, 7001, "main", plSHA, "failed")
	readsBefore := len(w.gl.Requests())
	w.deliverFor(w.other, 7001, "main", "failed").requireDropped(t, "action_disabled")
	if r := w.gl.Requests()[readsBefore:]; len(r) != 0 {
		t.Fatalf("intake read GitLab for a project that has not opted in: %v", r)
	}
	if w.gate1.calls() != 0 {
		t.Fatalf("meter was asked about a project that has not opted in")
	}

	w.failPipeline(900, unitFails)
	o1 := w.deliver(900, "failed").requireOrder(t)
	anchor1 := o1["bead_anchor"]
	wantFP := pipelinectx.Fingerprint([]pipelinectx.FailedJob{{Stage: "test", Name: "unit-tests", FailureReason: "script_failure"}}, "")
	if want, _ := pipelinectx.Anchor(pid, w.target, wantFP, 880, 0); anchor1 != want || o1["pipeline_id"] != "900" ||
		o1["pipeline_target"] != w.target.String() || o1["pipeline_last_success"] != "880" {
		t.Fatalf("order = %v, want anchor %s for pipeline 900 on %s since 880", o1, want, w.target)
	}

	// ---- 2. dispatch: one session, at the pipeline's commit -------------------
	if code := w.dispatch(o1); code != 0 {
		t.Fatalf("dispatch exit = %d\n%s", code, w.gateLog)
	}
	s1 := sessionName(pid, 900)
	if got := w.sessionNames(); len(got) != 1 || got[0] != s1 || w.sessions()[0].Agent != pipelineAgent {
		t.Fatalf("sessions = %v, want exactly %s for agent %s", got, s1, pipelineAgent)
	}
	if shas := w.grants.shas(t); len(shas) != 1 || shas[0] != plSHA {
		t.Fatalf("checkout granted at %v, want the pipeline's commit %s", shas, plSHA)
	}
	if st, _ := w.runState(anchor1); st != pipelinectx.RunStateDiagnosing {
		t.Fatalf("run note state = %s after dispatch, want diagnosing", st)
	}
	w.rt.SetPhase(runtime.Handle(s1), runtime.PhaseRunning, "")
	w.sweep() // a running session is judged by nobody
	if len(w.om.requests()) != 0 {
		t.Fatalf("the sweep judged a running session: %+v", w.om.requests())
	}

	// ---- 3. the same failure again, while the session runs --------------------
	w.failPipeline(910, unitFails)
	o2 := w.deliver(910, "failed").requireOrder(t) // diagnosing is not "handled": Gate 1 sees it, meter dedupes
	if o2["bead_anchor"] != anchor1 || o2["pipeline_id"] != "910" {
		t.Fatalf("the second failure of the run has anchor %s (pipeline %s), want %s", o2["bead_anchor"], o2["pipeline_id"], anchor1)
	}
	if code := w.dispatch(o2); code != 0 {
		t.Fatalf("second dispatch exit = %d", code)
	}
	if got := w.sessionNames(); len(got) != 1 {
		t.Fatalf("a second failure of the SAME run started another session: %v", got)
	}

	// ---- 4. the agent answers; the sweep lands it -----------------------------
	w.answer(s1, rtDiagnosis(effects.ClassCode, "unit-tests", plGoodLine, nil), plGoodLine)
	w.sweep()
	if oc := w.om.requests(); len(oc) != 1 || oc[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcomes = %+v, want one success\n%s", oc, w.gateLog)
	}
	st, body := w.runState(anchor1)
	if st != pipelinectx.RunStateDiagnosed || !strings.Contains(body, plGoodLine) || !strings.Contains(body, "`code`") {
		t.Fatalf("run note = %s, want diagnosed with the quoted line and the class:\n%s", st, body)
	}
	// gonk-385g: the note LEADS with its subject -- the pipeline (linked), the
	// commit, the failing job (linked) -- and its evidence links the job too.
	// The URL is the one dispatch recorded from GitLab's own pipeline object.
	u := w.proj.WebURL
	unitJob := w.record(anchor1).PipelineJobs[0]
	if want := "### Diagnosis of " + plLink(u, 900) + " -- commit `111111111111` -- failed job: " + plJobLink(u, "unit-tests", unitJob.ID); firstLine(body) != want {
		t.Fatalf("the diagnosed note's first line:\n got %q\nwant %q", firstLine(body), want)
	}
	if want := "- " + plJobLink(u, "unit-tests", unitJob.ID) + ": `" + plGoodLine + "`"; !strings.Contains(body, want) {
		t.Fatalf("the evidence does not link its job (%q):\n%s", want, body)
	}
	if !equalSet(w.labels(), rtWantLabels(mr, "gonk::pipeline-code")) {
		t.Fatalf("labels = %v, want %v", w.labels(), rtWantLabels(mr, "gonk::pipeline-code"))
	}
	if n := len(w.gl.IssuesOf(pid)); (mr && n != 0) || (!mr && n != 1) {
		t.Fatalf("issues = %d (mr=%v): an MR target opens none, a ref target exactly one", n, mr)
	}

	// ---- 5. the same failure after the diagnosis: handled, not re-asked -------
	w.failPipeline(920, unitFails)
	calls := w.gate1.calls()
	w.deliver(920, "failed").requireDropped(t, intake.ReasonRunAlreadyHandled)
	if w.gate1.calls() != calls {
		t.Fatal("meter was asked about a run whose landing says it is diagnosed")
	}
	w.tick()
	if st, body := w.runState(anchor1); st != pipelinectx.RunStateDiagnosed ||
		!strings.Contains(body, "Latest failing pipeline in this run: "+plLink(u, 920)) {
		t.Fatalf("after a later failure of the run: %s\n%s", st, body)
	}
	if got := w.sessionNames(); len(got) != 1 {
		t.Fatalf("sessions after three failures of one run = %v, want one", got)
	}

	// ---- 6. green: dropped at intake, resolved by maintenance -----------------
	w.addPipeline(930, plSHAGreen, "success")
	w.deliver(930, "success").requireDropped(t, intake.ReasonPipelineNotFailed)
	w.tick()
	if st, body := w.runState(anchor1); st != pipelinectx.RunStateResolved || !strings.Contains(body, plLink(u, 930)+" passed") {
		t.Fatalf("after a green pipeline: %s\n%s", st, body)
	}
	if rec := w.record(anchor1); rec.PipelineRunOpen {
		t.Fatal("the run is still open after a green pipeline")
	}
	if l := w.labels(); len(l) != 0 {
		t.Fatalf("labels after resolution = %v, want none (MR label removed / tracking issue closed)", l)
	}
	if !mr {
		if is := w.gl.IssuesOf(pid)[0]; is.State != "closed" {
			t.Fatalf("tracking issue is %s after the run resolved, want closed", is.State)
		}
	}

	// ---- 7. the same failure after the green: a NEW run -----------------------
	w.failPipeline(940, unitFails)
	o3 := w.deliver(940, "failed").requireOrder(t)
	anchor2 := o3["bead_anchor"]
	if anchor2 == anchor1 || o3["pipeline_last_success"] != "930" || o3["pipeline_fingerprint"] != o1["pipeline_fingerprint"] {
		t.Fatalf("after a green, the same failure must be a new run (same fingerprint, last success 930): %v", o3)
	}
	if code := w.dispatch(o3); code != 0 {
		t.Fatalf("dispatch exit = %d", code)
	}
	s2 := sessionName(pid, 940)
	if got := w.sessionNames(); len(got) != 2 || got[1] != s2 {
		t.Fatalf("sessions = %v, want a second session %s for the new run", got, s2)
	}
	w.answer(s2, rtDiagnosis(effects.ClassTest, "unit-tests", plGoodLine, nil), plGoodLine)
	w.sweep()
	if st, _ := w.runState(anchor2); st != pipelinectx.RunStateDiagnosed {
		t.Fatalf("new run state = %s, want diagnosed", st)
	}
	if st, _ := w.runState(anchor1); st != pipelinectx.RunStateResolved {
		t.Fatalf("the first run's note moved to %s, want it left resolved", st)
	}

	// ---- 8. a different failure supersedes, and is its own run ----------------
	w.failPipeline(950, map[string]string{"build": rtBuildLog})
	w.tick()
	if st, body := w.runState(anchor2); st != pipelinectx.RunStateSuperseded || !strings.Contains(body, plLink(u, 950)+" failed with a different set") {
		t.Fatalf("after a different failure: %s\n%s", st, body)
	}
	if w.record(anchor2).PipelineRunOpen {
		t.Fatal("a superseded run is still open")
	}
	o4 := w.deliver(950, "failed").requireOrder(t)
	anchor3 := o4["bead_anchor"]
	if anchor3 == anchor2 || anchor3 == anchor1 || o4["pipeline_last_success"] != "930" {
		t.Fatalf("a different failure must be a new run on the same last success: %v", o4)
	}
	if code := w.dispatch(o4); code != 0 {
		t.Fatalf("dispatch exit = %d", code)
	}
	s3 := sessionName(pid, 950)
	w.answer(s3, rtDiagnosis(effects.ClassCode, "build", rtBuildLine, nil), rtBuildLine)
	w.sweep()
	if st, _ := w.runState(anchor3); st != pipelinectx.RunStateDiagnosed {
		t.Fatalf("third run state = %s, want diagnosed", st)
	}
	if got := w.sessionNames(); len(got) != 3 {
		t.Fatalf("sessions = %v, want exactly three (one per run)", got)
	}
	if n := len(w.runNotes()); n != 3 {
		t.Fatalf("run notes = %d, want one per run (3)", n)
	}
	if !mr {
		// Run 1's issue was closed on green; runs 2 and 3 share the next one.
		issues := w.gl.IssuesOf(pid)
		if len(issues) != 2 || issues[1].State != "opened" || len(w.gl.IssueNotesOf(pid, issues[1].IID)) != 2 {
			t.Fatalf("tracking issues = %+v, want the closed first and one open issue holding runs 2 and 3", issues)
		}
	}
	if !equalSet(w.labels(), rtWantLabels(mr, "gonk::pipeline-code")) {
		t.Fatalf("labels = %v, want the third run's class", w.labels())
	}
	w.requireNoIssueTriage()
	for _, r := range w.gl.Requests() {
		if strings.Contains(r, fmt.Sprintf("/projects/%d/", w.other.ID)) {
			t.Fatalf("something touched the project that never opted in: %s", r)
		}
	}
}

func rtWantLabels(mr bool, class string) []string {
	if mr {
		return []string{class}
	}
	return []string{labelPipelineFailing, class}
}

// TestPipelineRoundTripRefusals: the batches gonk must NEVER post, each through
// the whole path (hook -> intake -> order -> dispatch -> session -> sweep). For
// each: the sweep writes NOTHING to GitLab, logs the cause, reports gate-failed
// (the model's fault, so the ladder escalates with the run identity), and the
// landing still says diagnosing -- which intake does not treat as handled.
func TestPipelineRoundTripRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  string
		mutate func(anchor string) func(*effects.Effect)
	}{
		{"an ungrounded quoted line", causeUngrounded, func(string) func(*effects.Effect) {
			return func(e *effects.Effect) { e.Evidence[0].Line = "AssertionError: sum([1, 2]) returned 5, expected 4" }
		}},
		{"a forged gonk marker", causeForgedMarker, func(anchor string) func(*effects.Effect) {
			forged, _ := pipelinectx.RunMarker(anchor, pipelinectx.RunStateResolved)
			return func(e *effects.Effect) { e.Summary += " " + forged }
		}},
		{"a secret in the remedy", causeSecret, func(string) func(*effects.Effect) {
			return func(e *effects.Effect) { e.Remedy += "\nSet password=hunter22hunter in the job." }
		}},
		{"a cited file missing at the pipeline's commit", causeFileMissing, func(string) func(*effects.Effect) {
			return func(e *effects.Effect) { e.Files = []string{"calc/missing.py"} }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newRTWorld(t, false)
			w.om.outcomeNext = "escalate"
			w.failPipeline(900, map[string]string{"unit-tests": plTestLog})
			o := w.deliver(900, "failed").requireOrder(t)
			anchor := o["bead_anchor"]
			if code := w.dispatch(o); code != 0 {
				t.Fatalf("dispatch exit = %d", code)
			}
			s := sessionName(w.proj.ID, 900)
			w.answer(s, rtDiagnosis(effects.ClassCode, "unit-tests", plGoodLine, tc.mutate(anchor)))

			before := w.writes()
			w.sweep()
			if after := w.writes(); len(after) != len(before) {
				t.Fatalf("a refused batch wrote to GitLab: %v", after[len(before):])
			}
			if !strings.Contains(w.gateLog.String(), "pipeline diagnosis refused, applied nothing") ||
				!strings.Contains(w.gateLog.String(), "cause="+tc.cause) {
				t.Fatalf("refusal not logged with cause %s:\n%s", tc.cause, w.gateLog)
			}
			if oc := w.om.requests(); len(oc) != 1 || oc[0].Outcome != meterapi.OutcomeGateFailed {
				t.Fatalf("outcomes = %+v, want one gate-failed", oc)
			}
			if ro := w.resling.orders(); len(ro) != 1 || ro[0].Vars["bead_anchor"] != anchor || ro[0].Vars["pipeline_id"] != "900" {
				t.Fatalf("re-sling = %+v, want one escalation carrying the run identity", ro)
			}
			if st, _ := w.runState(anchor); st != pipelinectx.RunStateDiagnosing {
				t.Fatalf("landing state = %s after a refusal, want still diagnosing", st)
			}
			for _, l := range w.labels() {
				if strings.HasPrefix(l, "gonk::pipeline-") && l != labelPipelineFailing {
					t.Fatalf("a refused diagnosis left a class label: %v", w.labels())
				}
			}
			// The next failure of the run is NOT dropped as handled: a refusal
			// diagnosed nothing.
			w.failPipeline(910, map[string]string{"unit-tests": plTestLog})
			o2 := w.deliver(910, "failed").requireOrder(t)
			if o2["bead_anchor"] != anchor {
				t.Fatalf("the next failure of the run has anchor %s, want %s", o2["bead_anchor"], anchor)
			}

			// THE LADDER CLIMBS, ON THE SAME RUN. Meter answers attempt 2 (the
			// escalation); whichever order arrives first -- intake's for pipeline
			// 910 or the sweep's re-sling -- starts ONE session, and it diagnoses
			// the RECORDED pipeline (900), whose excerpts the record's hashes are
			// of, not the order's. The second order starts nothing.
			esc := plRunDecision()
			esc.Attempt, esc.Rung = 2, "r3"
			w.gate2.resp = esc
			if code := w.dispatch(o2); code != 0 {
				t.Fatalf("escalated dispatch exit = %d\n%s", code, w.gateLog)
			}
			if code := w.dispatch(w.resling.orders()[0].Vars); code != 0 {
				t.Fatalf("re-sling dispatch exit = %d\n%s", code, w.gateLog)
			}
			s2 := runtime.Name(pipelineAgent, w.proj.ID, runtime.KindPipeline, 900, 2)
			if got := w.sessionNames(); len(got) != 2 || got[1] != s2 {
				t.Fatalf("sessions = %v, want %s then exactly one escalated %s", got, s, s2)
			}
			w.om.outcomeNext = "done"
			w.answer(s2, rtDiagnosis(effects.ClassCode, "unit-tests", plGoodLine, nil), plGoodLine)
			w.sweep()
			if st, _ := w.runState(anchor); st != pipelinectx.RunStateDiagnosed {
				t.Fatalf("after the escalated attempt answered correctly: %s, want diagnosed\n%s", st, w.gateLog)
			}
			w.requireNoIssueTriage()
		})
	}
}

// TestPipelineRoundTripRecordsFailureFacts (gonk-2usw FACTS): through the
// REAL hook, intake, order, dispatch, sweep and store -- nothing prepared by
// hand -- the stored record of a diagnosed run carries what a pipeline-fix is
// judged against: per failed job its stage, failure_reason, allow_failure and
// error signature, and the landed diagnosis. And it KEEPS them while run
// maintenance rewrites the record (a later failure of the run, then a green).
func TestPipelineRoundTripRecordsFailureFacts(t *testing.T) {
	for _, mr := range []bool{false, true} {
		name := "default-branch"
		if mr {
			name = "merge-request"
		}
		t.Run(name, func(t *testing.T) { roundTripFailureFacts(t, mr) })
	}
}

func roundTripFailureFacts(t *testing.T, mr bool) {
	w := newRTWorld(t, mr)
	pid := w.proj.ID
	bothFail := map[string]string{"unit-tests": plTestLog, "lint": plLintLog}
	w.failPipeline(900, bothFail)
	o := w.deliver(900, "failed").requireOrder(t)
	anchor := o["bead_anchor"]
	if code := w.dispatch(o); code != 0 {
		t.Fatalf("dispatch exit = %d\n%s", code, w.gateLog)
	}

	wantJobs := map[string]beadstore.PipelineJob{
		"unit-tests": {Stage: "test", FailureReason: "script_failure",
			ErrorSignature: pipelinectx.ErrorSignature(plTestFirstError), SignatureVersion: pipelinectx.SignatureVersion},
		"lint": {Stage: "test", FailureReason: "script_failure",
			ErrorSignature: pipelinectx.ErrorSignature(plLintFirstError), ErrorGeneric: true, SignatureVersion: pipelinectx.SignatureVersion},
	}
	requireJobFacts := func(when string) beadstore.Record {
		t.Helper()
		rec := w.record(anchor)
		if len(rec.PipelineJobs) != len(wantJobs) {
			t.Fatalf("%s: PipelineJobs = %+v, want %d jobs", when, rec.PipelineJobs, len(wantJobs))
		}
		for _, j := range rec.PipelineJobs {
			want := wantJobs[j.Name]
			got := beadstore.PipelineJob{Stage: j.Stage, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure,
				ErrorSignature: j.ErrorSignature, ErrorGeneric: j.ErrorGeneric, SignatureVersion: j.SignatureVersion}
			if got != want {
				t.Fatalf("%s: job %s failure facts\n got  %+v\n want %+v", when, j.Name, got, want)
			}
		}
		return rec
	}

	// The RUNNING record dispatch wrote: the job facts are there before any
	// answer exists, and no diagnosis is.
	if rec := requireJobFacts("after dispatch"); rec.State != beadstore.StateRunning || rec.PipelineDiagnosis.Recorded() {
		t.Fatalf("after dispatch: state %s, diagnosis %+v; want running with none", rec.State, rec.PipelineDiagnosis)
	}

	// The agent blames unit-tests only; lint failed too but is not cited.
	s := sessionName(pid, 900)
	answer := rtDiagnosis(effects.ClassCode, "unit-tests", plGoodLine, nil)
	w.answer(s, answer, plGoodLine)
	w.sweep()
	if oc := w.om.requests(); len(oc) != 1 || oc[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcomes = %+v, want one success\n%s", oc, w.gateLog)
	}
	wantDiag := beadstore.PipelineDiagnosis{
		Class: "code", Files: []string{"calc/sum.py"}, EvidenceJobs: []string{"unit-tests"},
		Summary: answer.Effects[0].Summary, Remedy: answer.Effects[0].Remedy,
	}
	requireDiagnosed := func(when string) beadstore.Record {
		t.Helper()
		rec := requireJobFacts(when)
		if !reflect.DeepEqual(rec.PipelineDiagnosis, wantDiag) {
			t.Fatalf("%s: stored diagnosis\n got  %+v\n want %+v", when, rec.PipelineDiagnosis, wantDiag)
		}
		return rec
	}
	rec := requireDiagnosed("after the sweep landed the diagnosis")
	if rec.State != beadstore.StateDone {
		t.Fatalf("stored state = %s, want done", rec.State)
	}
	// What the record says is what the note says: one source, two renderings.
	if st, body := w.runState(anchor); st != pipelinectx.RunStateDiagnosed || !strings.Contains(body, "`"+rec.PipelineDiagnosis.Class+"`") ||
		!strings.Contains(body, rec.PipelineDiagnosis.Remedy) || !strings.Contains(body, "`"+rec.PipelineDiagnosis.Files[0]+"`") {
		t.Fatalf("the landed note (%s) and the recorded diagnosis disagree:\n%s\n%+v", st, body, rec.PipelineDiagnosis)
	}

	// The target set T (plan 3.2) and its cause key, from the STORED record
	// alone: the cited, failed, required jobs.
	var keys []pipelinectx.JobFailureKey
	for _, j := range rec.PipelineJobs {
		for _, name := range rec.PipelineDiagnosis.EvidenceJobs {
			if j.Name == name && !j.AllowFailure {
				keys = append(keys, pipelinectx.JobFailureKey{Stage: j.Stage, Name: j.Name, FailureReason: j.FailureReason, ErrorSignature: j.ErrorSignature})
			}
		}
	}
	wantKey := pipelinectx.JobFailureKey{Stage: "test", Name: "unit-tests", FailureReason: "script_failure",
		ErrorSignature: pipelinectx.ErrorSignature(plTestFirstError)}
	if len(keys) != 1 || keys[0] != wantKey {
		t.Fatalf("T from the stored record = %v, want exactly [%v] (lint failed but was not cited)", keys, wantKey)
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("STORED RECORD of the diagnosed run (job failure key %s, cause key %s):\n%s", keys[0], pipelinectx.CauseKey(keys), raw)

	// Run maintenance REWRITES the record: a later failure of the same run
	// advances PipelineLatestFailed...
	w.failPipeline(920, bothFail)
	w.deliver(920, "failed").requireDropped(t, intake.ReasonRunAlreadyHandled)
	w.tick()
	if rec := requireDiagnosed("after maintenance saw a later failure"); rec.PipelineLatestFailed != 920 {
		t.Fatalf("PipelineLatestFailed = %d, want 920 (maintenance did not rewrite the record, so this proves nothing)", rec.PipelineLatestFailed)
	}
	// ...and a green closes the run. The facts survive both writes.
	w.addPipeline(930, plSHAGreen, "success")
	w.tick()
	if rec := requireDiagnosed("after the run resolved"); rec.PipelineRunOpen {
		t.Fatal("the run is still open after a green pipeline")
	}
}
