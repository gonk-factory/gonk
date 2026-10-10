package main

// gonk-2usw part DISPATCH: the fixture every pipeline-fix dispatch test stands
// on. SYNTHETIC AND NEUTRAL (plan section 0): plRun's made-up project, its
// failing unit test, and a diagnosis citing calc/sum.py.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// pfScopeTOML names THE REPOSITORY'S OWN pack row for the pipefix agent
// (pack/agents/pipefix/effect-shape.toml, part AGENT): pfPackDir(t,
// pfScopeTOML) is a copy of the pack exactly as production bakes it. The text
// below is what that row must say (plan 4.2, 4.3) -- it is not written
// anywhere; TestTheFixtureUsesTheRepositorysPackRow compares the real row
// with it, so a test on this fixture can never pass against a row the pack
// does not ship.
const pfScopeTOML = `
[scope]
classes = ["source", "docs", "test"]
liftable = ["ci"]
max_files = 3
max_lines = 40
max_deletes = 0
max_file_bytes = 262144
max_patch_bytes = 16384
`

// pfPackDir is a copy of the repository's pack. With shape == pfScopeTOML it
// is that copy and nothing else: the real pipefix agent, its real effect
// shape, its real [scope] row. Any other shape REPLACES the fix agent's
// effect-shape.toml with it ("" for a pack whose fix agent has no scope row
// at all), for the tests that need a pack production does not ship.
func pfPackDir(t *testing.T, shape string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(repoPackDir)); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "agents", pipefixAgent)
	if _, err := os.Stat(filepath.Join(agent, "effect-shape.toml")); err != nil {
		t.Fatalf("the repository's pack has no pipefix agent: %v", err)
	}
	if shape == pfScopeTOML {
		return dir
	}
	if err := os.WriteFile(filepath.Join(agent, "effect-shape.toml"), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The fixture's pack row IS the repository's: the row the real pack ships is
// the one pfScopeTOML describes.
func TestTheFixtureUsesTheRepositorysPackRow(t *testing.T) {
	want := t.TempDir()
	if err := os.MkdirAll(filepath.Join(want, "agents", pipefixAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(want, "agents", pipefixAgent, "effect-shape.toml"), []byte(pfScopeTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	wantRow, err := scope.LoadRow(want, pipefixAgent)
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"the repository's pack": repoPackDir, "pfPackDir": pfPackDir(t, pfScopeTOML)} {
		got, err := scope.LoadRow(dir, pipefixAgent)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, wantRow) {
			t.Errorf("%s: the pipefix [scope] row is\n %+v\nwant (plan 4.2, 4.3)\n %+v", name, got, wantRow)
		}
	}
	raw, err := os.ReadFile(filepath.Join(pfPackDir(t, pfScopeTOML), "agents", pipefixAgent, "effect-shape.toml"))
	if err != nil {
		t.Fatal(err)
	}
	real, err := os.ReadFile(filepath.Join(repoPackDir, "agents", pipefixAgent, "effect-shape.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, real) {
		t.Error("pfPackDir(pfScopeTOML) does not carry the repository's effect-shape.toml byte for byte")
	}
}

// pfGonkYML is the project's .gonk.yml at the pinned commit: opted in, and
// selecting the python profile, under which calc/sum.py is `source`.
const pfGonkYML = "version: 1\nenabled: true\nactions: { pipelines: true, pipeline_fixes: true }\n" +
	"ladder: [local-1]\nscope:\n  profiles: [python, docs-md]\n"

// pfConfig is meter's answer for an opted-in project with the defaults of
// plan 3.1, and an operator who restricts nothing.
func pfConfig(targets ...string) *meterapi.ProjectResponse {
	if len(targets) == 0 {
		targets = []string{"ref"}
	}
	return &meterapi.ProjectResponse{
		Project: "example/calc", Rig: "example-calc", State: meterapi.StateActive, ConfigHash: "sha256:pf",
		Effective: &meterapi.Effective{
			Enabled: true,
			Actions: meterapi.Actions{Pipelines: true, PipelineFixes: true},
			PipelineFix: meterapi.PipelineFix{
				Classes: []string{"code", "test"}, Targets: targets, CIExecChanges: "manual-run",
				MaxAttempts: 2, MaxChain: 5, VerifyTimeoutSeconds: 7200, MaxFiles: 3, MaxChangedLines: 40,
				Paths: []string{}, AutoMerge: meterapi.AutoMerge{Mode: "off", Classes: []string{"docs"}},
			},
		},
		OperatorScope: &meterapi.OperatorScope{},
	}
}

// pfWorld is one diagnosed run on a fake GitLab, a work store, the config
// source and the order route -- everything the sweep's fix pass touches.
type pfWorld struct {
	t      *testing.T
	r      *plRun
	store  beadstore.Store
	orders *fakeOrders
	log    *bytes.Buffer
	pack   string
	now    time.Time

	live     bool // sweepDeps.FixesLive
	cfg      *meterapi.ProjectResponse
	cfgErr   error
	cfgCalls int
	hold     func(context.Context, beadstore.Record) bool
}

// newPFWorld returns a world in which the run IS eligible: opted in, a `code`
// diagnosis citing one source file, the target's head at the failing
// pipeline's commit, no fix yet. Each E-row test breaks exactly one thing.
func newPFWorld(t *testing.T, o plOpts) *pfWorld {
	t.Helper()
	// The diagnosis is landed by the REAL apply, so the record carries exactly
	// what the sweep records (PipelineDiagnosis), and the run note is the one
	// production writes.
	r := newPLRun(t, o)
	rec := r.rec
	if applied, v, err := applyPipelineBatch(context.Background(), r.deps(t, io.Discard), &rec, plDiagnosis(nil)); !applied {
		t.Fatalf("the fixture's diagnosis was refused: %q %v", v, err)
	}
	if !rec.PipelineDiagnosis.Recorded() {
		t.Fatal("the apply recorded no diagnosis; every test on this fixture would be vacuous")
	}
	rec.State = beadstore.StateDone
	store := beadstore.NewMemory()
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	r.rec = rec
	w := &pfWorld{t: t, r: r, store: store, orders: &fakeOrders{}, log: &bytes.Buffer{},
		pack: pfPackDir(t, pfScopeTOML), now: time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC), live: true}
	if o.mr {
		// The default branch's head is NOT the pipeline's commit: the config is
		// pinned there, the fix is written against the MR's head.
		w.cfg = pfConfig("ref", "mr")
		r.gl.SetBranchHead(r.pid, "main", plSHAOld)
		r.proj.PutFileOn(plSHAOld, ".gonk.yml", []byte(pfGonkYML))
		r.gl.SetMRHead(r.pid, 5, plSHA)
	} else {
		w.cfg = pfConfig()
		r.gl.SetBranchHead(r.pid, "main", plSHA)
		r.proj.PutFileOn(plSHA, ".gonk.yml", []byte(pfGonkYML))
	}
	return w
}

func (w *pfWorld) deps() sweepDeps {
	d := sweepDeps{
		Store: w.store, Pipelines: w.r.gl.Client(), Orders: w.orders, PackDir: w.pack,
		Log: testLogger(w.log), Now: func() time.Time { return w.now },
		FixesLive: w.live, FixHold: w.hold,
		FixConfig: func(context.Context, string) (*meterapi.ProjectResponse, error) {
			w.cfgCalls++
			return w.cfg, w.cfgErr
		},
	}
	return d.withDefaults()
}

// look runs one run-maintenance pass (and so one fix look) past the
// five-minute spacing, and returns what it logged.
func (w *pfWorld) look() string {
	w.t.Helper()
	w.now = w.now.Add(pipelineRunCheckEvery + time.Minute)
	at := w.log.Len()
	sweepPipelineRuns(context.Background(), w.deps())
	return w.log.String()[at:]
}

// requireReason asserts the look logged exactly one eligibility line and that
// it carries this reason code.
func requireReason(t *testing.T, log string, code pipefix.ReasonCode) {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "pipeline fix eligibility") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d eligibility line(s), want exactly one:\n%s", len(lines), log)
	}
	// The line itself, for `go test -v` (the plan's evidence log quotes these).
	if i := strings.Index(lines[0], "eligible="); i >= 0 {
		t.Log(lines[0][i:])
	}
	if !strings.Contains(lines[0], " reason="+string(code)+" ") {
		t.Fatalf("the sweep did not log reason=%s:\n%s", code, lines[0])
	}
	if !strings.Contains(lines[0], "eligible=false") {
		t.Fatalf("a refusal logged as eligible:\n%s", lines[0])
	}
}

func (w *pfWorld) fixAnchor() string {
	w.t.Helper()
	a, err := pipelinectx.FixAnchor(w.r.rec.BeadAnchor)
	if err != nil {
		w.t.Fatal(err)
	}
	return a
}

func (w *pfWorld) fixRecord() (beadstore.Record, bool) {
	w.t.Helper()
	store := w.store
	if f, ok := store.(*failFixGets); ok {
		store = f.Store // the test's own read is not the one being failed
	}
	rec, ok, err := store.Get(context.Background(), w.fixAnchor())
	if err != nil {
		w.t.Fatal(err)
	}
	return rec, ok
}

// setRun rewrites the stored run record (the diagnosis the look reads).
func (w *pfWorld) setRun(mutate func(*beadstore.Record)) {
	w.t.Helper()
	rec, ok, err := w.store.Get(context.Background(), w.r.rec.BeadAnchor)
	if err != nil || !ok {
		w.t.Fatalf("run record: %v %v", ok, err)
	}
	mutate(&rec)
	if err := w.store.Put(context.Background(), rec); err != nil {
		w.t.Fatal(err)
	}
}

// landingNotes is every note on the run's landing, oldest first.
func (w *pfWorld) landingNotes() []glab.Note {
	if w.r.target.Kind == pipelinectx.TargetKindMR {
		return w.r.gl.MRNotesOf(w.r.pid, 5)
	}
	return w.r.gl.IssueNotesOf(w.r.pid, 1)
}

// fixNote is the one BOT note carrying a fix marker for this fix: its state
// and body. ok is false when there is none; more than one fails the test.
func (w *pfWorld) fixNote() (state, body string, ok bool) {
	w.t.Helper()
	n := 0
	for _, note := range w.landingNotes() {
		if note.Author.ID != plBotID || note.System {
			continue // a human quoting a marker is not the fix note
		}
		if st := pipelinectx.FixMarkerStates(note.Body, w.fixAnchor()); len(st) > 0 {
			n++
			state, body, ok = st[len(st)-1], note.Body, true
		}
	}
	if n > 1 {
		w.t.Fatalf("%d fix notes on the landing, want at most one", n)
	}
	return state, body, ok
}

func (w *pfWorld) landingLabels() []string {
	if w.r.target.Kind == pipelinectx.TargetKindMR {
		return w.r.gl.MRsOf(w.r.pid)[0].Labels
	}
	return w.r.gl.IssuesOf(w.r.pid)[0].Labels
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// fixMR seeds a merge request on a fix branch, authored by the bot.
func (w *pfWorld) fixMR(iid int64, state, sourceBranch, targetBranch, description string, mergedAt *time.Time) {
	w.r.gl.AddMR(w.r.pid, glab.MergeRequest{
		IID: iid, Title: "gonk: fix", State: state, SourceBranch: sourceBranch, TargetBranch: targetBranch,
		Description: description, Author: glab.User{ID: plBotID, Username: "gonk"}, MergedAt: mergedAt,
	})
}

// causeKey is the cause key of the fixture's diagnosed run.
func (w *pfWorld) causeKey() string {
	w.t.Helper()
	rec, _, _ := w.store.Get(context.Background(), w.r.rec.BeadAnchor)
	targets, problems := pipefix.TargetSet(recordedJobs(rec), rec.PipelineDiagnosis.EvidenceJobs)
	if len(problems) != 0 || len(targets) == 0 {
		w.t.Fatalf("the fixture's target set: %v %v", targets, problems)
	}
	return pipefix.CauseKey(targets)
}

// humanLabel puts a label on the tracking issue as a project member would.
func (w *pfWorld) humanLabel(label string) {
	w.t.Helper()
	if _, err := w.r.gl.Client().UpdateIssue(context.Background(), w.r.pid, 1, glab.IssueUpdate{AddLabels: []string{label}}); err != nil {
		w.t.Fatal(err)
	}
}

func glabMR(iid int64, state, source, target string, authorID int64) glab.MergeRequest {
	return glab.MergeRequest{IID: iid, Title: "a merge request", State: state, SourceBranch: source, TargetBranch: target,
		Author: glab.User{ID: authorID}}
}

func glabPipeline(id int64, ref, sha, status string) glab.Pipeline {
	return glab.Pipeline{ID: id, Ref: ref, SHA: sha, Status: status, Source: "push", FinishedAt: plTime(int(id % 60))}
}

func glabJob(id int64, name, stage, status, reason string) glab.Job {
	return glab.Job{ID: id, Name: name, Stage: stage, Status: status, FailureReason: reason}
}
