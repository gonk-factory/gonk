package main

// gonk-2usw part APPLY: the static gates and the controller-authored apply of
// a pipeline fix (plan 5.3, 5.4, 8.1).
//
// Every test starts where production does: a diagnosed run, the SWEEP's look
// deciding a fix and firing its order, gonk-dispatch starting the fix session
// through the REAL preparer (so the record carries the real FixContext), and
// then a session answer handed to applyPipefixBatch -- or to the whole sweep.
// GitLab is pkg/glab/glabtest, and what is asserted about writes is asserted
// on ITS WRITE LOG: every non-GET request it received.
//
// SYNTHETIC AND NEUTRAL (plan section 0): a made-up `example/calc`, a Go
// function that skips an element, its unit test.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// faGonkYML is the project's .gonk.yml at the pinned commit for the Go
// fixture: opted in, and selecting the go profile, under which calc/total.go
// is `source` and calc/total_test.go is `test` -- and for which a test
// weakening predicate exists.
const faGonkYML = "version: 1\nenabled: true\nactions: { pipelines: true, pipeline_fixes: true }\n" +
	"ladder: [local-1]\nscope:\n  profiles: [go, docs-md]\n"

const (
	faCI     = ".gitlab-ci.yml"
	faCIBody = "stages:\n- build\n- test\nbuild:\n  stage: build\n  script:\n  - ./build\n" +
		"unit-tests:\n  stage: test\n  script:\n  - ./run-tests\n"
	faRationale = "Total skipped the first element of its argument; it now adds every element."
	faBotToken  = fxBotToken // assembled in fixture_secrets_test.go
)

// faOpts arranges one world. The zero value is a `code` diagnosis citing
// calc/total.go in a Go project.
type faOpts struct {
	class  string            // diagnosis class: code (default), test, configuration
	cited  []string          // default: the class's natural file
	files  map[string]string // extra files at the base commit
	modes  map[string]string // path -> git mode at the base commit
	python bool              // keep DISPATCH's python fixture (calc/sum.py): no weakening predicate
	cfg    func(*meterapi.ProjectResponse)
}

// faWorld is a pfWorld whose fix session has been dispatched by the real
// preparer and is waiting to be judged.
type faWorld struct {
	*pfWorld
	rt      *testRuntime
	session string
	mark    int // len(write log) when the session started
}

func newFAWorld(t *testing.T, o faOpts) *faWorld {
	t.Helper()
	w := newPFWorld(t, plOpts{})
	p := w.r.proj
	if !o.python {
		class := o.class
		if class == "" {
			class = pipefix.ClassCode
		}
		cited := o.cited
		if cited == nil {
			switch class {
			case pipefix.ClassTest:
				cited = []string{pfTest}
			case pipefix.ClassConfiguration:
				cited = []string{faCI}
			default:
				cited = []string{pfSource}
			}
		}
		p.PutFileOn(plSHA, pfSource, []byte(pfSourceBody))
		p.PutFileOn(plSHA, pfTest, []byte(pfTestBody))
		p.PutFileOn(plSHA, faCI, []byte(faCIBody))
		p.PutFileOn(plSHA, ".gonk.yml", []byte(faGonkYML))
		// The merged definition at the base IS the file (no include).
		w.r.gl.SetCILint(w.r.pid, plSHA, glab.CILint{Valid: true, MergedYAML: faCIBody})
		w.setRun(func(r *beadstore.Record) {
			r.PipelineDiagnosis.Class = class
			r.PipelineDiagnosis.Files = cited
		})
		if class == pipefix.ClassConfiguration {
			// The project's opt-in to the ci lift, and a person's confirmation
			// of the class (owner decision Q2).
			w.cfg.Effective.PipelineFix.Classes = []string{"code", "test", "configuration"}
			w.cfg.Effective.PipelineFix.AllowCI = true
			w.humanLabel(labelFixClassConfirmed)
			w.r.gl.SetCILintFromFile(w.r.pid, faCI)
		}
	}
	for path, body := range o.files {
		p.PutFileOn(plSHA, path, []byte(body))
	}
	for path, mode := range o.modes {
		p.SetFileMode(plSHA, path, mode)
	}
	if o.cfg != nil {
		o.cfg(w.cfg)
	}
	vars := w.firedFix()
	pd := w.dispatchFixReal(vars, pfRunDecision())
	if pd.code != 0 || len(pd.rt.Starts()) != 1 {
		t.Fatalf("setup: the fix session was not dispatched (exit %d)\n%s", pd.code, pd.logs)
	}
	rec, _ := w.fixRecord()
	if rec.State != beadstore.StateRunning || !rec.PipelineFix.Context.Recorded() || rec.PipelineFix.State != "" {
		t.Fatalf("setup: fix record state %q fix state %q context %v", rec.State, rec.PipelineFix.State, rec.PipelineFix.Context.Recorded())
	}
	return &faWorld{pfWorld: w, rt: pd.rt, session: pd.rt.Starts()[0].Name, mark: len(w.r.gl.Writes())}
}

func (w *faWorld) deps() sweepDeps {
	d := w.pfWorld.deps()
	d.FixSecrets = []effects.Secret{effects.NewSecret("gitlab_bot_token", faBotToken)}
	return d
}

// apply hands one batch to the apply, as applyBrokerBatch does, on a copy of
// the stored fix record. It does NOT write the store: that is the sweep's.
func (w *faWorld) apply(b effects.Batch) (applied bool, violation string, rec beadstore.Record, err error) {
	w.t.Helper()
	rec, ok := w.fixRecord()
	if !ok {
		w.t.Fatal("no fix record")
	}
	applied, violation, err = applyPipefixBatch(context.Background(), w.deps(), &rec, b)
	return applied, violation, rec, err
}

// written is the write log since the session started (or since reset).
func (w *faWorld) written() []glabtest.Write { return w.r.gl.Writes()[w.mark:] }

func (w *faWorld) resetWrites() { w.mark = len(w.r.gl.Writes()) }

// faLine renders one write for the evidence log and for assertions: the
// method, the path with its query, and the body's top-level keys in order of
// appearance with long values cut. It never drops a key.
func faLine(wr glabtest.Write) string {
	out := wr.Method + " " + wr.Path
	if wr.Query != "" {
		q, _ := url.QueryUnescape(wr.Query)
		out += "?" + q
	}
	if wr.Body == "" {
		return out
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(wr.Body), &m); err != nil {
		return out + " " + wr.Body
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := string(m[k])
		if len(v) > 96 {
			v = v[:93] + "..."
		}
		parts = append(parts, k+"="+v)
	}
	return out + " {" + strings.Join(parts, " ") + "}"
}

func faLines(ws []glabtest.Write) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, faLine(w))
	}
	return out
}

// faRoutes is the write log as "METHOD path-with-ids-replaced", for comparing
// the SEQUENCE of calls without pinning ids.
func faRoutes(ws []glabtest.Write) []string {
	re := regexp.MustCompile(`/notes/\d+`)
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Method+" "+re.ReplaceAllString(w.Path, "/notes/N"))
	}
	return out
}

func faEdit(path, old, new string) effects.Effect {
	return effects.Effect{Kind: effects.KindEdit, Path: path, Old: old, New: new}
}

func faMeta(rationale string) effects.Effect {
	return effects.Effect{Kind: effects.KindFixMeta, Reason: rationale}
}

// faFix is a `fix` batch: the edits and one fix_meta.
func faFix(edits ...effects.Effect) effects.Batch {
	return effects.Batch{Verdict: effects.VerdictFix, Effects: append(edits, faMeta(faRationale))}
}

// faGood is the one edit that fixes the synthetic defect.
func faGood() effects.Batch { return faFix(faEdit(pfSource, pfOld, pfNew)) }

func faNeedsScope(reason string, paths ...string) effects.Batch {
	return effects.Batch{Verdict: effects.VerdictNeedsScope, Effects: []effects.Effect{
		{Kind: effects.KindScopeRequest, Paths: paths, Reason: reason}}}
}

func (w *faWorld) branch() string {
	w.t.Helper()
	b, err := pipelinectx.FixBranch(w.fixAnchor())
	if err != nil {
		w.t.Fatal(err)
	}
	return b
}

func (w *faWorld) fixMRs() []glab.MergeRequest {
	var out []glab.MergeRequest
	for _, m := range w.r.gl.MRsOf(w.r.pid) {
		if m.SourceBranch == w.branch() {
			out = append(out, m)
		}
	}
	return out
}

// ---- the accepted batch: exactly the expected writes, in order ------------------

// AN ACCEPTED BATCH WRITES EXACTLY: the branch at the pinned base, one commit,
// one merge request, the fix note -- in that order and nothing else. The
// write log is printed under -v for the plan's evidence section.
func TestFixApplyAcceptedBatchWritesExactlyTheExpectedCalls(t *testing.T) {
	w := newFAWorld(t, faOpts{})
	applied, violation, rec, err := w.apply(faGood())
	if !applied || violation != "" || err != nil {
		t.Fatalf("applied=%v violation=%q err=%v\n%s", applied, violation, err, w.log)
	}
	ws := w.written()
	for _, l := range faLines(ws) {
		t.Log("WRITE " + l)
	}
	pid := w.r.pid
	want := []string{
		fmt.Sprintf("POST /api/v4/projects/%d/repository/branches", pid),
		fmt.Sprintf("POST /api/v4/projects/%d/repository/commits", pid),
		fmt.Sprintf("POST /api/v4/projects/%d/merge_requests", pid),
		fmt.Sprintf("PUT /api/v4/projects/%d/issues/1/notes/N", pid), // the `proposing` fix note, edited in place
	}
	if got := faRoutes(ws); !reflect.DeepEqual(got, want) {
		t.Fatalf("the write log is\n  %s\nwant exactly\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// 1. the branch: the fix branch, FROM THE PINNED SHA (never a branch name).
	q, _ := url.ParseQuery(ws[0].Query)
	if q.Get("branch") != w.branch() || q.Get("ref") != plSHA {
		t.Errorf("branch write %q: want branch %s at %s", ws[0].Query, w.branch(), plSHA)
	}
	// 2. the commit: on that branch, `update` actions only, the controller's message.
	var commit glab.CommitOptions
	if err := json.Unmarshal([]byte(ws[1].Body), &commit); err != nil {
		t.Fatal(err)
	}
	wantMsg := "gonk: fix job unit-tests on main\n\ngonk-fix: " + w.fixAnchor() + " attempt=1"
	if commit.Branch != w.branch() || commit.StartBranch != "" || commit.CommitMessage != wantMsg {
		t.Errorf("commit: branch %q start_branch %q message %q\nwant message %q", commit.Branch, commit.StartBranch, commit.CommitMessage, wantMsg)
	}
	wantContent := strings.Replace(pfSourceBody, pfOld, pfNew, 1)
	if len(commit.Actions) != 1 || commit.Actions[0].Action != "update" || commit.Actions[0].FilePath != pfSource ||
		commit.Actions[0].Content != wantContent {
		t.Errorf("commit actions %+v, want one update of %s with the patched content", commit.Actions, pfSource)
	}
	if strings.Contains(commit.CommitMessage, faRationale) {
		t.Error("the commit message carries the agent's text")
	}
	// 3. the merge request.
	mrs := w.fixMRs()
	if len(mrs) != 1 {
		t.Fatalf("%d merge requests on the fix branch, want one", len(mrs))
	}
	mr := mrs[0]
	if mr.TargetBranch != "main" || mr.Title != "gonk: fix job unit-tests on main" || mr.State != "opened" {
		t.Errorf("mr target %q title %q state %q", mr.TargetBranch, mr.Title, mr.State)
	}
	if !reflect.DeepEqual(mr.Labels, []string{"gonk::pipeline-fix", "gonk::fix-verifying"}) {
		t.Errorf("mr labels %v", mr.Labels)
	}
	marker, _ := pipelinectx.FixMRMarker(w.fixAnchor(), rec.PipelineFix.CauseKey)
	if a, c, ok := pipelinectx.ParseFixMRMarker(mr.Description); !ok || a != w.fixAnchor() || c != rec.PipelineFix.CauseKey ||
		!strings.Contains(mr.Description, marker) {
		t.Errorf("the description's fix marker reads (%q, %q, %v)", a, c, ok)
	}
	for _, wantText := range []string{"Related to #1", fixStatusOpen, fixStatusClose, ident("unit-tests"), ident("calc/total.go") + " (source): +1 -1 lines",
		faRationale, "gonk assigns no one", "The pipeline definition is unchanged."} {
		if !strings.Contains(mr.Description, wantText) {
			t.Errorf("the description lacks %q:\n%s", wantText, mr.Description)
		}
	}
	// 4. the fix note: ONE, the bot's, now `verifying`, naming the merge request.
	st, body, ok := w.fixNote()
	if !ok || st != "verifying" || !strings.Contains(body, fmt.Sprintf("!%d", mr.IID)) {
		t.Errorf("fix note state %q ok=%v:\n%s", st, ok, body)
	}

	// The record, as the caller will write it.
	f := rec.PipelineFix
	head, rerr := w.r.gl.Client().ResolveRef(context.Background(), pid, w.branch())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if f.State != "verifying" || f.MRIID != mr.IID || f.FixCommitSHA != head || !glab.IsCommitSHA(f.FixCommitSHA) || f.AppliedAttempt != 1 {
		t.Errorf("record: state %q mr %d commit %q applied attempt %d; branch head %q mr iid %d", f.State, f.MRIID, f.FixCommitSHA, f.AppliedAttempt, head, mr.IID)
	}
	if f.Weakening != beadstore.FixWeakeningDecided || len(f.WeakeningUndecided) != 0 || f.CIDelta != "none" || f.Refusal != "" {
		t.Errorf("record: weakening %q undecided %v ci delta %q refusal %q", f.Weakening, f.WeakeningUndecided, f.CIDelta, f.Refusal)
	}
	if want := []beadstore.PipelineFixFile{{Path: pfSource, Class: "source", LinesAdded: 1, LinesRemoved: 1}}; !reflect.DeepEqual(f.ChangedFiles, want) {
		t.Errorf("record: changed files %+v", f.ChangedFiles)
	}
	if want := w.now.Add(7200 * time.Second); !f.VerifyDeadline.Equal(want) {
		t.Errorf("record: verify deadline %v, want %v", f.VerifyDeadline, want)
	}
	// The commit is one commit, directly on the pinned base, by the bot.
	c, cerr := w.r.gl.Client().GetCommit(context.Background(), pid, head)
	if cerr != nil || len(c.ParentIDs) != 1 || c.ParentIDs[0] != plSHA || c.AuthorName != "gonk" {
		t.Errorf("commit %+v (%v): want one parent %s, authored by the bot", c, cerr, plSHA)
	}
	// The base is untouched.
	if raw, _ := w.r.gl.Client().GetRawFile(context.Background(), pid, pfSource, plSHA, 1<<20); string(raw) != pfSourceBody {
		t.Error("the apply changed the base commit's tree")
	}
}

// ---- refusals: by rule code, and NOTHING is written ------------------------------

type faRefusal struct {
	name  string
	opts  faOpts
	batch func(w *faWorld) effects.Batch
	code  string
	gate  string
}

func faRefusals() []faRefusal {
	big := strings.Repeat("\tsum += 0\n", 41)
	return []faRefusal{
		// A1: the pack shape alone accepts an empty batch; ValidateFix must run too.
		{name: "an empty batch", batch: func(*faWorld) effects.Batch { return effects.Batch{Verdict: effects.VerdictFix} },
			code: "FX-SHAPE", gate: "A1"},
		// This one the pack shape AND effects.Validate both accept (every
		// minimum is 0 and there is no fix verdict to tie kinds to): only
		// ValidateFix refuses it at A1.
		{name: "an empty batch with no verdict at all", batch: func(*faWorld) effects.Batch { return effects.Batch{} },
			code: "FX-SHAPE", gate: "A1"},
		{name: "a fix with no fix_meta", batch: func(*faWorld) effects.Batch {
			return effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{faEdit(pfSource, pfOld, pfNew)}}
		}, code: "FX-SHAPE", gate: "A1"},
		{name: "eleven edits", batch: func(*faWorld) effects.Batch {
			var e []effects.Effect
			for i := 0; i < 11; i++ {
				e = append(e, faEdit(pfSource, pfOld, pfNew))
			}
			return faFix(e...)
		}, code: "FX-PACK", gate: "A1"},
		{name: "an edit and a scope_request together", batch: func(*faWorld) effects.Batch {
			b := faGood()
			b.Effects = append(b.Effects, effects.Effect{Kind: effects.KindScopeRequest, Reason: "x"})
			return b
		}, code: "FX-SHAPE", gate: "A1"},

		// A4: paths.
		{name: "an edit to a file the diagnosis did not cite", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfTest, "want 3", "want 2"))
		}, code: "SC-SHOWN(SC-CITED)", gate: "A4"},
		{name: "an edit to a cited file the session was told it may not change",
			opts: faOpts{cited: []string{pfSource, "calc/empty.go"}, files: map[string]string{"calc/empty.go": ""}},
			batch: func(*faWorld) effects.Batch {
				return faFix(faEdit("calc/empty.go", "a", "b"))
			}, code: "SC-SHOWN(ED-NOMATCH)", gate: "A4"},
		{name: "a path that climbs out", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit("calc/../.gonk.yml", "enabled", "disabled"))
		}, code: "SC-FORM(path)", gate: "A1"},
		{name: "the project's own gonk configuration", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(".gonk.yml", "enabled: true", "enabled: false"))
		}, code: "SC-SHOWN(SC-CITED)", gate: "A4"},

		// A5: only regular files.
		{name: "a symlink at the edit path", opts: faOpts{modes: map[string]string{pfSource: glab.ModeSymlink}},
			batch: func(*faWorld) effects.Batch { return faGood() }, code: "SC-FORM(symlink)", gate: "A5"},
		{name: "a submodule at the edit path", opts: faOpts{modes: map[string]string{pfSource: glab.ModeSubmodule}},
			batch: func(*faWorld) effects.Batch { return faGood() }, code: "SC-FORM(submodule)", gate: "A5"},

		// A5/A6: pkg/effects' own predicates, reached with the real inputs.
		{name: "old text that is not in the file", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfSource, "range ys {", "range xs {"))
		}, code: "ED-NOMATCH", gate: "A6"},
		{name: "old text that occurs twice", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfSource, "sum", "total"))
		}, code: "ED-AMBIGUOUS", gate: "A6"},
		{name: "more lines than the cap", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfSource, "\tsum := 0\n", "\tsum := 0\n"+big))
		}, code: "SC-SIZE(max_lines)", gate: "A6"},
		{name: "a private key in an added line", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfSource, "\tsum := 0\n", "\tsum := 0 // -----BEGIN RSA "+"PRIVATE KEY-----\n"))
		}, code: "SC-SECRET(private_key)", gate: "A6"},
		{name: "the bot's own token in an added line", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfSource, "\tsum := 0\n", "\tsum := 0 // "+faBotToken+"\n"))
		}, code: "SC-SECRET(gitlab_bot_token)", gate: "A6"},

		// A6: SC-WEAKEN, Go.
		{name: "a test deleted", opts: faOpts{class: pipefix.ClassTest}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfTest, "func TestTotal(t *testing.T) {\n\tif got := Total([]int{1, 2}); got != 3 {\n\t\tt.Errorf(\"Total([1 2]) = %d, want 3\", got)\n\t}\n}\n",
				"var _ = testing.Short\n"))
		}, code: "SC-WEAKEN(removed-test)", gate: "A6"},
		{name: "a test renamed out of go test's sight", opts: faOpts{class: pipefix.ClassTest}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfTest, "func TestTotal(", "func checkTotal("))
		}, code: "SC-WEAKEN(removed-test)", gate: "A6"},
		{name: "a skip added", opts: faOpts{class: pipefix.ClassTest}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfTest, "\tif got := Total", "\tt.Skip(\"until fixed\")\n\tif got := Total"))
		}, code: "SC-WEAKEN(added-skip)", gate: "A6"},
		{name: "a test file built out with a tag", opts: faOpts{class: pipefix.ClassTest}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfTest, "package calc\n", "//go:build never\n\npackage calc\n"))
		}, code: "SC-WEAKEN(build-constraint)", gate: "A6"},
		{name: "a source file built out with a tag", batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfSource, "package calc\n", "//go:build never\n\npackage calc\n"))
		}, code: "SC-WEAKEN(build-constraint)", gate: "A6"},
		{name: "a test file left unparseable", opts: faOpts{class: pipefix.ClassTest}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(pfTest, "func TestTotal(t *testing.T) {", "func TestTotal(t *testing.T) {{"))
		}, code: "SC-WEAKEN(unparseable)", gate: "A6"},

		// A7: the CI pre-check (the `configuration` row, with the lift).
		{name: "allow_failure added to the job", opts: faOpts{class: pipefix.ClassConfiguration}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(faCI, "unit-tests:\n  stage: test\n", "unit-tests:\n  stage: test\n  allow_failure: true\n"))
		}, code: "CI-PRE(allow_failure)", gate: "A7"},
		{name: "the job made manual", opts: faOpts{class: pipefix.ClassConfiguration}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(faCI, "unit-tests:\n  stage: test\n", "unit-tests:\n  stage: test\n  when: manual\n"))
		}, code: "CI-PRE(when)", gate: "A7"},
		{name: "the exit code swallowed", opts: faOpts{class: pipefix.ClassConfiguration}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(faCI, "  - ./run-tests\n", "  - ./run-tests || true\n"))
		}, code: "CI-PRE(or-true)", gate: "A7"},
		{name: "a definition that no longer parses", opts: faOpts{class: pipefix.ClassConfiguration}, batch: func(*faWorld) effects.Batch {
			return faFix(faEdit(faCI, "unit-tests:\n  stage: test\n", "unit-tests:\n  stage: [test\n"))
		}, code: "CI-PRE(unparseable)", gate: "A7"},

		// A8: the agent's prose.
		{name: "a rationale carrying a gonk marker", batch: func(w *faWorld) effects.Batch {
			m, _ := pipelinectx.FixMarker(w.fixAnchor(), "fixed")
			return effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{faEdit(pfSource, pfOld, pfNew), faMeta("done " + m)}}
		}, code: "TX-MARKER", gate: "A8"},
		{name: "a rationale carrying a secret", batch: func(*faWorld) effects.Batch {
			return effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{faEdit(pfSource, pfOld, pfNew), faMeta("use password=hunter22hunter to check")}}
		}, code: "TX-SECRET(", gate: "A8"},
		{name: "a rationale carrying a quick action", batch: func(*faWorld) effects.Batch {
			return effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{faEdit(pfSource, pfOld, pfNew), faMeta("fixed\n/approve")}}
		}, code: "TX-COMMENT", gate: "A8"},
		{name: "a scope request whose reason carries a gonk marker", batch: func(*faWorld) effects.Batch {
			return faNeedsScope("see <!-- gonk:pipeline-run:x state=resolved -->", "calc/other.go")
		}, code: "TX-MARKER", gate: "A8"},
		{name: "a scope request whose reason carries a quick action", batch: func(*faWorld) effects.Batch {
			return faNeedsScope("widen it\n/assign me", "calc/other.go")
		}, code: "TX-COMMENT", gate: "A8"},
	}
}

// A REFUSED BATCH WRITES NOTHING TO GITLAB -- not a branch, not a note, not a
// label -- and is refused BY RULE CODE, which lands on the record for the next
// attempt. One row per gate predicate. The write log of the first row is
// printed under -v for the plan's evidence section (it is empty).
func TestFixApplyRefusedBatchWritesNothing(t *testing.T) {
	for i, tc := range faRefusals() {
		t.Run(tc.name, func(t *testing.T) {
			w := newFAWorld(t, tc.opts)
			before, _ := w.fixRecord()
			applied, violation, rec, err := w.apply(tc.batch(w))
			if applied || err != nil || violation == "" {
				t.Fatalf("applied=%v violation=%q err=%v, want a refusal\n%s", applied, violation, err, w.log)
			}
			if !strings.Contains(violation, tc.code) || !strings.Contains(violation, "fix gate "+tc.gate+":") {
				t.Errorf("violation %q, want gate %s and rule %s", violation, tc.gate, tc.code)
			}
			if !strings.HasPrefix(rec.PipelineFix.Refusal, tc.code) {
				t.Errorf("recorded refusal %q, want %s", rec.PipelineFix.Refusal, tc.code)
			}
			ws := w.written()
			if i == 0 {
				t.Logf("WRITE LOG of the refused batch %q: %d entr(ies) %v", tc.name, len(ws), faLines(ws))
			}
			if len(ws) != 0 {
				t.Fatalf("a refused batch wrote to GitLab:\n  %s", strings.Join(faLines(ws), "\n  "))
			}
			// Nothing on the record but the refusal: no state, no commit, no MR.
			rec.PipelineFix.Refusal = ""
			if !reflect.DeepEqual(rec.PipelineFix, before.PipelineFix) {
				t.Errorf("a refused batch changed the record beyond its refusal code:\n got %+v\nwant %+v", rec.PipelineFix, before.PipelineFix)
			}
			if len(w.fixMRs()) != 0 {
				t.Error("a merge request exists")
			}
			if st, _, _ := w.fixNote(); st != "proposing" {
				t.Errorf("the fix note moved to %q", st)
			}
		})
	}
}

// Every refusal the table produces is distinct evidence only if the SAME
// worlds accept a good batch: a fixture that refuses everything would pass
// the table above.
func TestFixApplyAcceptsAGoodBatchInEveryRefusalWorld(t *testing.T) {
	goodFor := map[string]effects.Batch{
		pipefix.ClassCode: faGood(),
		pipefix.ClassTest: faFix(faEdit(pfTest, "want 3", "want three")),
		pipefix.ClassConfiguration: faFix(faEdit(faCI, "unit-tests:\n  stage: test\n",
			"unit-tests:\n  stage: test\n  timeout: 10m\n")),
	}
	seen := map[string]bool{}
	for _, tc := range faRefusals() {
		class := tc.opts.class
		if class == "" {
			class = pipefix.ClassCode
		}
		if seen[class] || tc.opts.modes != nil {
			continue
		}
		seen[class] = true
		t.Run(class, func(t *testing.T) {
			w := newFAWorld(t, faOpts{class: class})
			applied, violation, rec, err := w.apply(goodFor[class])
			if !applied || violation != "" || err != nil {
				t.Fatalf("a good %s fix: applied=%v violation=%q err=%v\n%s", class, applied, violation, err, w.log)
			}
			if len(w.fixMRs()) != 1 || rec.PipelineFix.MRIID == 0 {
				t.Fatal("no merge request was opened")
			}
		})
	}
	if len(seen) != 3 {
		t.Fatalf("classes covered: %v", seen)
	}
}

// ---- our own failures: unknown, never the agent's, and nothing is written -------

// A FAILURE OF OURS IS AN ERROR, NOT A REFUSAL (plan 5.3: A3, A5 read, A9;
// 7.3 infra-failed): no refusal code is recorded, nothing is written, and the
// next sweep tries again.
func TestFixApplyOurFailuresAreUnknownAndWriteNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(w *faWorld)
		wantIs  error
		want    string
	}{
		{name: "the project opted out after the session started", arrange: func(w *faWorld) {
			w.cfg.Effective.Actions.PipelineFixes = false
		}, wantIs: errPipefixNotOptedIn},
		{name: "the configuration cannot be read", arrange: func(w *faWorld) { w.cfgErr = errors.New("meter is down") }, want: "meter is down"},
		{name: "an operator tightened a cap after the session started", arrange: func(w *faWorld) {
			w.cfg.Effective.PipelineFix.MaxChangedLines = 5
		}, wantIs: errPipefixEnvelopeChanged},
		// THE LIFT IS NEVER TAKEN FROM THE RECORD: a record that names a ci
		// lift and a ci envelope is not believed -- the fold runs again, the
		// project never set allow_ci, GrantLift is not even asked.
		{name: "a lift written onto the record", arrange: func(w *faWorld) {
			rec, _ := w.fixRecord()
			rec.PipelineFix.Lift = []string{"ci"}
			rec.PipelineFix.Envelope = []string{"ci", "docs", "source"}
			if err := w.store.Put(context.Background(), rec); err != nil {
				w.t.Fatal(err)
			}
		}, wantIs: errPipefixEnvelopeChanged},
		{name: "the recorded context was altered", arrange: func(w *faWorld) {
			rec, _ := w.fixRecord()
			// A field no re-read of GitLab would notice: only the record's
			// own hash can.
			rec.PipelineFix.Context.PromptSHA256 = strings.Repeat("0", 64)
			if err := w.store.Put(context.Background(), rec); err != nil {
				w.t.Fatal(err)
			}
		}, wantIs: errPipefixContextChanged},
		{name: "the context names another commit than the fix is written against", arrange: func(w *faWorld) {
			rec, _ := w.fixRecord()
			rec.PipelineFix.Context.BaseSHA = plSHAOld
			rec.PipelineFix.ContextSHA256 = pipefixContextHash(rec.PipelineFix.Context)
			if err := w.store.Put(context.Background(), rec); err != nil {
				w.t.Fatal(err)
			}
		}, wantIs: errPipefixContextChanged},
		{name: "no context was recorded", arrange: func(w *faWorld) {
			rec, _ := w.fixRecord()
			rec.PipelineFix.Context, rec.PipelineFix.ContextSHA256 = beadstore.FixContext{}, ""
			if err := w.store.Put(context.Background(), rec); err != nil {
				w.t.Fatal(err)
			}
		}, wantIs: errPipefixContextChanged},
		{name: "the diagnosis on the run record changed", arrange: func(w *faWorld) {
			w.setRun(func(r *beadstore.Record) { r.PipelineDiagnosis.Remedy = "Delete the test." })
		}, wantIs: errPipefixContextChanged},
		{name: "the file at the base is not the one the session was shown", arrange: func(w *faWorld) {
			w.r.proj.PutFileOn(plSHA, pfSource, []byte(strings.ReplaceAll(pfSourceBody, "sum", "acc")))
		}, wantIs: errPipefixContextChanged},
		{name: "the job log changed", arrange: func(w *faWorld) {
			w.r.gl.SetTrace(w.r.pid, 9002, []byte("$ ./run-tests\nsomething else entirely\nERROR: Job failed: exit code 1\n"))
		}, wantIs: errPipefixContextChanged},
		{name: "the base tree cannot be listed", arrange: func(w *faWorld) {
			w.r.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/repository/tree", w.r.pid), 500)
		}, want: "basefacts"},
		{name: "the diagnosed run's record is gone", arrange: func(w *faWorld) {
			rec, _ := w.fixRecord()
			rec.PipelineFix.RunAnchor = "gonk:1:pipeline:ref-00000000:0000000000000000:s1"
			if err := w.store.Put(context.Background(), rec); err != nil {
				w.t.Fatal(err)
			}
		}, want: "diagnosed run's record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFAWorld(t, faOpts{})
			tc.arrange(w)
			w.resetWrites()
			applied, violation, rec, err := w.apply(faGood())
			if applied || violation != "" || err == nil {
				t.Fatalf("applied=%v violation=%q err=%v, want an error of ours", applied, violation, err)
			}
			if effects.AsRefusal(err) != nil {
				t.Fatalf("our failure is reported as the agent's refusal: %v", err)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want %v", err, tc.wantIs)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			if rec.PipelineFix.Refusal != "" || rec.PipelineFix.State != "" {
				t.Errorf("record: refusal %q state %q", rec.PipelineFix.Refusal, rec.PipelineFix.State)
			}
			if ws := w.written(); len(ws) != 0 {
				t.Fatalf("an apply that failed on our side wrote to GitLab:\n  %s", strings.Join(faLines(ws), "\n  "))
			}
		})
	}
}

// ---- no assignee, no reviewer, no approval, no closing keyword -------------------

var faForbiddenWrite = regexp.MustCompile(`(?i)assignee|reviewer|approv|/merge\b|merge_when|auto_merge|squash`)

// THE MERGE REQUEST GETS NO ASSIGNEE, NO REVIEWER AND NO APPROVAL, and nothing
// is merged: read off the requests the fake received, across the whole apply.
func TestFixMRHasNoAssigneeReviewerOrApproval(t *testing.T) {
	w := newFAWorld(t, faOpts{})
	if applied, v, _, err := w.apply(faGood()); !applied {
		t.Fatalf("setup: %q %v", v, err)
	}
	var mrWrite *glabtest.Write
	for _, wr := range w.written() {
		wr := wr
		if strings.HasSuffix(wr.Path, "/merge_requests") && wr.Method == "POST" {
			mrWrite = &wr
			continue // its description is checked separately: it says "assigns no one" in prose
		}
		if faForbiddenWrite.MatchString(wr.Path+"?"+wr.Query) || regexp.MustCompile(`"(assignee|reviewer)[a-z_]*"`).MatchString(wr.Body) {
			t.Errorf("a write that assigns, reviews, approves or merges: %s", faLine(wr))
		}
	}
	if mrWrite == nil {
		t.Fatal("no merge request was created")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(mrWrite.Body), &body); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// EXACTLY these keys: a new one (assignee_id, reviewer_ids, ...) fails here.
	want := []string{"description", "labels", "remove_source_branch", "source_branch", "target_branch", "title"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("the merge request was created with keys %v, want exactly %v", keys, want)
	}
	if string(body["remove_source_branch"]) != "false" {
		t.Errorf("remove_source_branch = %s", body["remove_source_branch"])
	}
}

// faClosing is GitLab's default issue closing pattern, loosely: a closing verb
// followed by a reference.
var faClosing = regexp.MustCompile(`(?i)\b(?:clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\b:?\s+(?:issues?\s+)?(?:[\w./-]*[#!]\d+|https?://|[A-Z][A-Z0-9_]+-\d+)`)

// faOutsideFences returns md with every fenced block removed: GitLab resolves
// no reference and no closing keyword inside one.
func faOutsideFences(md string) string {
	var out []string
	fence := ""
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimLeft(line, " ")
		n := 0
		for n < len(t) && t[n] == '`' {
			n++
		}
		switch {
		case fence == "" && n >= 3:
			fence = t[:n]
		case fence != "" && n >= len(fence) && strings.TrimSpace(t[n:]) == "":
			fence = ""
		case fence == "":
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// THE FIX NEVER CLOSES THE DIAGNOSIS: `Related to`, never a closing keyword --
// not in the description, not in the title, not in the commit message -- even
// when the agent's rationale, the job name and the branch name all try.
func TestFixMRNeverCloses(t *testing.T) {
	if !faClosing.MatchString("Closes #1") || !faClosing.MatchString("this fixes #12") || faClosing.MatchString("Related to #1") {
		t.Fatal("the closing pattern of this test is wrong; the assertions below would be vacuous")
	}
	w := newFAWorld(t, faOpts{})
	// A job and a branch named to smuggle a closing keyword.
	rec, _ := w.fixRecord()
	rec.PipelineFix.Targets[0].Name = "TRACK-7 closes #1"
	rec.PipelineFix.TargetBranch = "main"
	if err := w.store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	hostile := "Closes #1 and fixes #1.\n```\nCloses #1\n```\n````\nResolves #1\nImplements TRACK-7. Closes https://gitlab.example.invalid/g/p/-/issues/1"
	b := effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{faEdit(pfSource, pfOld, pfNew), faMeta(hostile)}}
	if applied, v, _, err := w.apply(b); !applied {
		t.Fatalf("setup: %q %v\n%s", v, err, w.log)
	}
	mr := w.fixMRs()[0]
	if !strings.Contains(mr.Description, "Related to #1") {
		t.Fatalf("the description does not link the diagnosis:\n%s", mr.Description)
	}
	if !strings.Contains(mr.Description, "Resolves # 1") || !strings.Contains(mr.Description, "TRACK- 7") {
		t.Fatalf("the rationale is not in the description (made inert); the checks below would be vacuous:\n%s", mr.Description)
	}
	// GitLab scans the RAW text for closing patterns: a fence hides nothing.
	// So the pattern must match NOWHERE -- this test's own, and production's.
	for name, re := range map[string]*regexp.Regexp{"the test's pattern": faClosing, "closingPattern": closingPattern} {
		if m := re.FindString(mr.Description); m != "" {
			t.Fatalf("%s matches in the description: %q\n%s", name, m, mr.Description)
		}
		if m := re.FindString(mr.Title); m != "" {
			t.Fatalf("%s matches in the title: %q", name, mr.Title)
		}
	}
	if strings.ContainsAny(mr.Title, "#!") {
		t.Fatalf("the title can reference: %q", mr.Title)
	}
	// The rationale is still there, in a fence, merely unable to reference.
	if strings.Contains(faOutsideFences(mr.Description), "Resolves") {
		t.Fatalf("the agent's text is rendered outside a fence:\n%s", mr.Description)
	}
	for _, wr := range w.written() {
		if !strings.HasSuffix(wr.Path, "/repository/commits") {
			continue
		}
		var c glab.CommitOptions
		_ = json.Unmarshal([]byte(wr.Body), &c)
		if faClosing.MatchString(c.CommitMessage) || closingPattern.MatchString(c.CommitMessage) || strings.Contains(c.CommitMessage, "#") {
			t.Fatalf("the commit message can close an issue: %q", c.CommitMessage)
		}
	}
	// An MR target is linked with `Related to !<iid>`.
	if line, err := relatedLine(runLanding{target: pipelinectx.Target{Kind: pipelinectx.TargetKindMR, MRIID: 5}}); err != nil || line != "Related to !5" {
		t.Fatalf("relatedLine for an MR target = %q, %v", line, err)
	}
	if _, err := relatedLine(runLanding{}); err == nil {
		t.Fatal("a fix with no landing to link is rendered anyway")
	}
}

// ---- notes: never a human's, never backwards ------------------------------------

// A HUMAN'S NOTE THAT QUOTES A FIX MARKER IS NEVER EDITED, wherever it sits in
// the thread; and a note that a later part moved on is never moved back.
func TestFixApplyNeverEditsAHumansNoteAndNeverMovesANoteBackwards(t *testing.T) {
	t.Run("a human quoting the marker", func(t *testing.T) {
		w := newFAWorld(t, faOpts{})
		// The human quotes the fix's own marker AFTER the bot's note, so theirs
		// is the newest note carrying it.
		_, botBody, _ := w.fixNote()
		quoted, _ := pipelinectx.FixMarker(w.fixAnchor(), "proposing")
		human := "gonk said this, and I disagree:\n\n" + quoted
		w.r.gl.AddNote(w.r.pid, 1, glab.User{ID: rtHumanID, Username: "human"}, human, false)
		if applied, v, _, err := w.apply(faGood()); !applied {
			t.Fatalf("setup: %q %v", v, err)
		}
		var humans, bots int
		for _, n := range w.landingNotes() {
			switch {
			case n.Author.ID == rtHumanID:
				humans++
				if n.Body != human {
					t.Fatalf("the human's note was edited:\n%s", n.Body)
				}
			case len(pipelinectx.FixMarkerStates(n.Body, w.fixAnchor())) > 0:
				bots++
				if n.Body == botBody {
					t.Fatal("the bot's own fix note was not edited")
				}
			}
		}
		if humans != 1 || bots != 1 {
			t.Fatalf("%d human note(s), %d bot fix note(s); want 1 and 1", humans, bots)
		}
		if st, _, _ := w.fixNote(); st != "verifying" {
			t.Fatalf("the bot's fix note is %q", st)
		}
	})
	for _, later := range []string{"fixed", "fixed-new-failure", "merged", "confirmed", "taken-over", "regressed"} {
		t.Run("a note already "+later, func(t *testing.T) {
			w := newFAWorld(t, faOpts{})
			l, err := newPipelineLander(context.Background(), w.r.gl.Client(), testLogger(&bytes.Buffer{}))
			if err != nil {
				t.Fatal(err)
			}
			// What part VERIFY will write, written here directly into the note.
			marker, _ := pipelinectx.FixMarker(w.fixAnchor(), later)
			for _, n := range w.landingNotes() {
				if len(pipelinectx.FixMarkerStates(n.Body, w.fixAnchor())) > 0 {
					if _, err := w.r.gl.Client().UpdateIssueNote(context.Background(), w.r.pid, 1, n.ID, "a later part's words\n\n"+marker); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, state := range []string{"verifying", "awaiting-run", "needs-scope"} {
				changed, _, err := l.writeFixNote(context.Background(), w.r.rec, w.fixAnchor(), fixNote{State: state, Text: "late", Apply: true})
				if !errors.Is(err, errFixNoteAdvanced) || changed {
					t.Fatalf("%s -> %s by the apply: changed=%v err=%v, want errFixNoteAdvanced", later, state, changed, err)
				}
			}
			if st, body, _ := w.fixNote(); st != later || !strings.Contains(body, "a later part's words") {
				t.Fatalf("the note went backwards to %q", st)
			}
		})
	}
	// What the apply MAY write over: every dispatch-time state, an earlier
	// open-merge-request note, and not-fixed.
	for _, earlier := range []string{pipelinectx.FixNoteNotEligible, "proposing", "denied", "needs-scope", "awaiting-run", "verifying", "not-fixed"} {
		if !fixNoteMayReplace(fixNote{State: "verifying", Apply: true}, earlier) {
			t.Errorf("the apply may not write `verifying` over %q", earlier)
		}
	}
	for _, state := range pipelinectx.FixNoteStates() {
		if fixNoteDispatchStates[state] || fixNoteApplyReplaces[state] {
			continue
		}
		if fixNoteMayReplace(fixNote{State: "verifying", Apply: true}, state) {
			t.Errorf("the apply may write `verifying` over %q", state)
		}
	}
	if fixNoteMayReplace(fixNote{State: "verifying", Apply: true}, "a-state-of-a-newer-build") {
		t.Error("the apply may write over a state this build does not know")
	}
}

// ---- needs-scope ----------------------------------------------------------------

// NEEDS-SCOPE IS A SUCCESS, LABELLED FOR A HUMAN: the note lands with the
// session's request as fenced text, the landing gets gonk::needs-human, the
// fix state is needs-scope -- and no branch, commit or merge request exists.
func TestFixApplyNeedsScopeLandsTheRequestAndNothingElse(t *testing.T) {
	w := newFAWorld(t, faOpts{})
	reason := "The defect is in calc/other.go, which this fix was not given. Closes #1"
	applied, violation, rec, err := w.apply(faNeedsScope(reason, "calc/other.go", "calc/`odd`.go"))
	if !applied || violation != "" || err != nil {
		t.Fatalf("applied=%v violation=%q err=%v\n%s", applied, violation, err, w.log)
	}
	pid := w.r.pid
	want := []string{
		fmt.Sprintf("PUT /api/v4/projects/%d/issues/1/notes/N", pid),
		fmt.Sprintf("PUT /api/v4/projects/%d/issues/1", pid),
	}
	if got := faRoutes(w.written()); !reflect.DeepEqual(got, want) {
		t.Fatalf("the write log is %v, want exactly %v", got, want)
	}
	if rec.PipelineFix.State != "needs-scope" || rec.PipelineFix.MRIID != 0 || rec.PipelineFix.FixCommitSHA != "" {
		t.Errorf("record %+v", rec.PipelineFix)
	}
	st, body, _ := w.fixNote()
	if st != "needs-scope" || !strings.Contains(body, "`calc/other.go`") || !strings.Contains(body, "The defect is in calc/other.go") {
		t.Errorf("needs-scope note (%q):\n%s", st, body)
	}
	if m := faClosing.FindString(faOutsideFences(body)); m != "" {
		t.Errorf("the agent's reason is rendered outside a fence: %q\n%s", m, body)
	}
	if !hasString(w.landingLabels(), "gonk::needs-human") {
		t.Errorf("landing labels %v", w.landingLabels())
	}
	if len(w.fixMRs()) != 0 {
		t.Error("a merge request exists")
	}
	// The label IS the needs-scope verdict's (verdictLabels), and both fix
	// verdict labels are reserved: no agent can propose them.
	if verdictLabel(effects.VerdictNeedsScope) != "gonk::needs-human" || verdictLabel(effects.VerdictFix) != "gonk::pipeline-fix" {
		t.Errorf("verdict labels: needs-scope %q fix %q", verdictLabel(effects.VerdictNeedsScope), verdictLabel(effects.VerdictFix))
	}
	for _, l := range append([]string{"gonk::pipeline-fix", "gonk::needs-human"}, fixMRLabels...) {
		if _, err := normaliseLabel(l, gonkLabelPrefix); err == nil {
			t.Errorf("an agent may propose the label %s", l)
		}
	}
}

// ---- the weakening fact ----------------------------------------------------------

// WHERE NO PREDICATE EXISTS, THE FACT IS RECORDED, NOT PRETENDED: a python
// fix is applied, the record says "not-decidable" and names the path, and the
// merge request says so in words. A Go fix says "decided".
func TestFixApplyRecordsWhetherWeakeningWasDecidable(t *testing.T) {
	t.Run("python: not decidable", func(t *testing.T) {
		w := newFAWorld(t, faOpts{python: true})
		b := faFix(faEdit("calc/sum.py", "    return 0\n", "    return sum(xs)\n"))
		applied, v, rec, err := w.apply(b)
		if !applied {
			t.Fatalf("%q %v\n%s", v, err, w.log)
		}
		if rec.PipelineFix.Weakening != beadstore.FixWeakeningNotDecidable || !reflect.DeepEqual(rec.PipelineFix.WeakeningUndecided, []string{"calc/sum.py"}) {
			t.Fatalf("weakening %q undecided %v", rec.PipelineFix.Weakening, rec.PipelineFix.WeakeningUndecided)
		}
		desc := w.fixMRs()[0].Description
		if !strings.Contains(desc, "gonk cannot tell whether this change weakens a test") || !strings.Contains(desc, ident("calc/sum.py")) {
			t.Fatalf("the description does not say the weakening check was not possible:\n%s", desc)
		}
		if strings.Contains(desc, "No test was removed") {
			t.Fatal("the description claims a check that did not happen")
		}
	})
	t.Run("go test file: decided", func(t *testing.T) {
		w := newFAWorld(t, faOpts{class: pipefix.ClassTest})
		applied, v, rec, err := w.apply(faFix(faEdit(pfTest, "want 3", "want three")))
		if !applied {
			t.Fatalf("%q %v\n%s", v, err, w.log)
		}
		if rec.PipelineFix.Weakening != beadstore.FixWeakeningDecided || len(rec.PipelineFix.WeakeningUndecided) != 0 {
			t.Fatalf("weakening %q undecided %v", rec.PipelineFix.Weakening, rec.PipelineFix.WeakeningUndecided)
		}
		if !strings.Contains(w.fixMRs()[0].Description, "No test was removed, skipped or built out") {
			t.Fatal("the description does not state the check")
		}
	})
	t.Run("a non-Go test file under the go profile: not decidable", func(t *testing.T) {
		golden := "calc/testdata/total.golden"
		w := newFAWorld(t, faOpts{class: pipefix.ClassTest, cited: []string{golden}, files: map[string]string{golden: "3\n"}})
		applied, v, rec, err := w.apply(faFix(faEdit(golden, "3\n", "2\n")))
		if !applied {
			t.Fatalf("%q %v\n%s", v, err, w.log)
		}
		if rec.PipelineFix.Weakening != beadstore.FixWeakeningNotDecidable || !reflect.DeepEqual(rec.PipelineFix.WeakeningUndecided, []string{golden}) {
			t.Fatalf("a golden file's weakening is recorded as %q %v", rec.PipelineFix.Weakening, rec.PipelineFix.WeakeningUndecided)
		}
	})
}

// ---- crash and resume ------------------------------------------------------------

// THE APPLY IS IDEMPOTENT ACROSS A CRASH BETWEEN ANY TWO WRITES. Each row
// stops the first run at one point (the write after it fails, or the record
// write is "lost": the second run starts from the ORIGINAL record) and runs
// the apply again. The second run writes only what is missing, and both ways
// end in the same place: one branch, one commit on the base, one merge
// request, one fix note.
func TestFixApplyResumesAfterACrash(t *testing.T) {
	pidPath := func(w *faWorld, suffix string) string { return fmt.Sprintf("/api/v4/projects/%d%s", w.r.pid, suffix) }
	for _, tc := range []struct {
		name string
		// crash arranges the failure of the FIRST run; firstErr says whether
		// that run reports an error (ours) or success.
		crash    func(w *faWorld)
		firstErr bool
		// second is the exact write sequence of the second run.
		second func(w *faWorld) []string
	}{
		{name: "after the branch, before the commit",
			crash:    func(w *faWorld) { w.r.gl.FailNext("POST", pidPath(w, "/repository/commits"), 502, 10) },
			firstErr: true,
			second: func(w *faWorld) []string {
				return []string{"POST " + pidPath(w, "/repository/commits"), "POST " + pidPath(w, "/merge_requests"), "PUT " + pidPath(w, "/issues/1/notes/N")}
			}},
		{name: "after the commit, before the merge request",
			crash:    func(w *faWorld) { w.r.gl.FailNext("POST", pidPath(w, "/merge_requests"), 502, 10) },
			firstErr: true,
			second: func(w *faWorld) []string {
				return []string{"POST " + pidPath(w, "/merge_requests"), "PUT " + pidPath(w, "/issues/1/notes/N")}
			}},
		{name: "after the merge request, before the note",
			crash: func(w *faWorld) { w.r.gl.FailNext("PUT", pidPath(w, "/issues/1/notes"), 502, 10) },
			second: func(w *faWorld) []string {
				return []string{"PUT " + pidPath(w, "/issues/1/notes/N")}
			}},
		{name: "after everything, before the record write",
			crash:  func(w *faWorld) {},
			second: func(w *faWorld) []string { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFAWorld(t, faOpts{})
			tc.crash(w)
			applied, v, _, err := w.apply(faGood())
			if tc.firstErr && (applied || err == nil || v != "") {
				t.Fatalf("first run: applied=%v violation=%q err=%v, want an error of ours", applied, v, err)
			}
			if !tc.firstErr && !applied {
				t.Fatalf("first run: violation=%q err=%v", v, err)
			}
			firstWrites := len(w.written())
			// The crash is over. The record was never written: the second run
			// reads the same record the first did.
			clearFailures(w.r.gl)
			w.resetWrites()
			applied, v, rec, err := w.apply(faGood())
			if !applied || v != "" || err != nil {
				t.Fatalf("second run: applied=%v violation=%q err=%v\n%s", applied, v, err, w.log)
			}
			if got, want := faRoutes(w.written()), tc.second(w); len(got)+len(want) != 0 && !reflect.DeepEqual(got, want) {
				t.Fatalf("the second run wrote\n  %v\nwant exactly\n  %v\n(the first run made %d write(s))", got, want, firstWrites)
			}
			// The same end state as an uninterrupted run.
			mrs := w.fixMRs()
			if len(mrs) != 1 || mrs[0].State != "opened" {
				t.Fatalf("%d merge request(s) on the fix branch", len(mrs))
			}
			head, _ := w.r.gl.Client().ResolveRef(context.Background(), w.r.pid, w.branch())
			c, cerr := w.r.gl.Client().GetCommit(context.Background(), w.r.pid, head)
			if cerr != nil || len(c.ParentIDs) != 1 || c.ParentIDs[0] != plSHA {
				t.Fatalf("the branch head %s is not ONE commit on the base: %+v %v", head, c, cerr)
			}
			if rec.PipelineFix.FixCommitSHA != head || rec.PipelineFix.MRIID != mrs[0].IID || rec.PipelineFix.State != "verifying" {
				t.Fatalf("record commit %q mr %d state %q; head %s mr %d", rec.PipelineFix.FixCommitSHA, rec.PipelineFix.MRIID,
					rec.PipelineFix.State, head, mrs[0].IID)
			}
			if mrs[0].SHA != head && mrs[0].SHA != "" {
				t.Fatalf("the merge request's head %s is not the commit %s", mrs[0].SHA, head)
			}
			if st, _, _ := w.fixNote(); st != "verifying" {
				t.Fatalf("fix note %q", st)
			}
			// A THIRD run changes nothing at all.
			w.resetWrites()
			if applied, v, again, err := w.apply(faGood()); !applied || len(w.written()) != 0 || again.PipelineFix.FixCommitSHA != head {
				t.Fatalf("a third run: applied=%v %q %v, %d write(s)", applied, v, err, len(w.written()))
			}
		})
	}
}

// clearFailures drops every injected failure of the fake.
func clearFailures(gl *glabtest.Server) { gl.ClearFailures() }

// WHAT IS ON THE BRANCH MUST BE GONK'S OWN COMMIT OF THIS ATTEMPT, or nothing
// is written and the fix is `taken-over`. Each row plants something else.
func TestFixApplyAdoptsOnlyItsOwnCommit(t *testing.T) {
	other := strings.Repeat("c", 40)
	for _, tc := range []struct {
		name  string
		plant func(w *faWorld, msg, patched string)
	}{
		{name: "a commit with another message", plant: func(w *faWorld, msg, patched string) {
			w.plantCommit(other, "someone else's change", plSHA, map[string]string{pfSource: patched})
		}},
		{name: "our message on another parent", plant: func(w *faWorld, msg, patched string) {
			w.plantCommit(other, msg, plSHAOld, map[string]string{pfSource: patched})
		}},
		{name: "our message and parent, other content", plant: func(w *faWorld, msg, patched string) {
			w.plantCommit(other, msg, plSHA, map[string]string{pfSource: patched + "// and more\n"})
		}},
		{name: "our message with two parents", plant: func(w *faWorld, msg, patched string) {
			w.plantCommit(other, msg, plSHA, map[string]string{pfSource: patched})
			w.r.gl.AddCommit(w.r.pid, glab.Commit{ID: other, Message: msg, ParentIDs: []string{plSHA, plSHAOld}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFAWorld(t, faOpts{})
			msg := "gonk: fix job unit-tests on main\n\ngonk-fix: " + w.fixAnchor() + " attempt=1"
			tc.plant(w, msg, strings.Replace(pfSourceBody, pfOld, pfNew, 1))
			w.resetWrites()
			applied, v, rec, err := w.apply(faGood())
			if !applied || v != "" || err != nil {
				t.Fatalf("applied=%v violation=%q err=%v", applied, v, err)
			}
			if rec.PipelineFix.State != "taken-over" || rec.PipelineFix.FixCommitSHA != "" || rec.PipelineFix.MRIID != 0 {
				t.Fatalf("record state %q commit %q mr %d, want taken-over and nothing else", rec.PipelineFix.State, rec.PipelineFix.FixCommitSHA, rec.PipelineFix.MRIID)
			}
			if ws := w.written(); len(ws) != 0 {
				t.Fatalf("a taken-over branch was written to:\n  %s", strings.Join(faLines(ws), "\n  "))
			}
		})
	}
	// The control: exactly our commit IS adopted.
	t.Run("exactly our commit", func(t *testing.T) {
		w := newFAWorld(t, faOpts{})
		msg := "gonk: fix job unit-tests on main\n\ngonk-fix: " + w.fixAnchor() + " attempt=1"
		w.plantCommit(other, msg, plSHA, map[string]string{pfSource: strings.Replace(pfSourceBody, pfOld, pfNew, 1)})
		w.resetWrites()
		applied, _, rec, _ := w.apply(faGood())
		if !applied || rec.PipelineFix.FixCommitSHA != other || rec.PipelineFix.State != "verifying" {
			t.Fatalf("our own commit was not adopted: state %q commit %q", rec.PipelineFix.State, rec.PipelineFix.FixCommitSHA)
		}
		for _, r := range faRoutes(w.written()) {
			if strings.Contains(r, "/repository/") {
				t.Fatalf("adopting a commit wrote to the repository: %s", r)
			}
		}
	})
}

// plantCommit puts a commit on the fix branch as if someone had pushed it.
func (w *faWorld) plantCommit(sha, message, parent string, files map[string]string) {
	w.t.Helper()
	p := w.r.proj
	base := map[string][]byte{}
	for path, body := range p.Files[plSHA] { // the whole base tree
		base[path] = body
	}
	for path, body := range base {
		p.PutFileOn(sha, path, body)
	}
	for path, body := range files {
		p.PutFileOn(sha, path, []byte(body))
	}
	p.PutFileOn(w.branch(), pfSource, []byte(files[pfSource]))
	w.r.gl.AddCommit(w.r.pid, glab.Commit{ID: sha, Message: message, ParentIDs: []string{parent}})
	w.r.gl.SetBranchHead(w.r.pid, w.branch(), sha)
}

// A MERGE REQUEST ALREADY ON THE FIX BRANCH decides before anything is written.
func TestFixApplyWithAMergeRequestAlreadyOnTheBranch(t *testing.T) {
	closedAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		mr        func(w *faWorld) glab.MergeRequest
		wantState string
	}{
		{"closed by a person", func(w *faWorld) glab.MergeRequest {
			return glab.MergeRequest{IID: 40, State: "closed", SourceBranch: w.branch(), TargetBranch: "main",
				Author: glab.User{ID: plBotID}, ClosedAt: &closedAt}
		}, "closed-unmerged"},
		{"already merged", func(w *faWorld) glab.MergeRequest {
			return glab.MergeRequest{IID: 40, State: "merged", SourceBranch: w.branch(), TargetBranch: "main",
				Author: glab.User{ID: plBotID}, MergedAt: &closedAt}
		}, "merged"},
		{"open, and not the bot's", func(w *faWorld) glab.MergeRequest {
			return glab.MergeRequest{IID: 40, State: "opened", SourceBranch: w.branch(), TargetBranch: "main",
				Author: glab.User{ID: rtHumanID}}
		}, "taken-over"},
		{"open, from another project", func(w *faWorld) glab.MergeRequest {
			return glab.MergeRequest{IID: 40, State: "opened", SourceBranch: w.branch(), TargetBranch: "main",
				Author: glab.User{ID: plBotID}, SourceProjectID: 999}
		}, "taken-over"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFAWorld(t, faOpts{})
			w.r.gl.AddMR(w.r.pid, tc.mr(w))
			w.resetWrites()
			applied, v, rec, err := w.apply(faGood())
			if !applied || v != "" || err != nil {
				t.Fatalf("applied=%v violation=%q err=%v", applied, v, err)
			}
			if rec.PipelineFix.State != tc.wantState {
				t.Fatalf("fix state %q, want %q", rec.PipelineFix.State, tc.wantState)
			}
			if ws := w.written(); len(ws) != 0 {
				t.Fatalf("wrote to GitLab:\n  %s", strings.Join(faLines(ws), "\n  "))
			}
		})
	}
}

// ---- a CI change ------------------------------------------------------------------

// A CI CHANGE DOES NOT RUN ON PUSH unless the project accepted that: the
// commit carries [skip ci], the fix is `awaiting-run`, and the merge request
// says a maintainer must start the pipeline. And the definition check runs
// AFTER the commit and BEFORE the merge request: a violation removes the
// branch and opens nothing.
func TestFixApplyOfACIChange(t *testing.T) {
	free := faFix(faEdit(faCI, "unit-tests:\n  stage: test\n", "unit-tests:\n  stage: test\n  timeout: 10m\n"))
	exec := faFix(faEdit(faCI, "  - ./run-tests\n", "  - ./run-tests --verbose\n"))
	// A gating change with a spelling the pre-check (A7) does not know: only
	// the comparison of the merged definitions can refuse it.
	gating := faFix(faEdit(faCI, "unit-tests:\n  stage: test\n", "unit-tests:\n  stage: test\n  rules:\n  - if: $NEVER\n"))
	commitOf := func(w *faWorld) glab.CommitOptions {
		var c glab.CommitOptions
		for _, wr := range w.written() {
			if strings.HasSuffix(wr.Path, "/repository/commits") {
				_ = json.Unmarshal([]byte(wr.Body), &c)
			}
		}
		return c
	}
	t.Run("manual-run (the default): skip ci, awaiting-run", func(t *testing.T) {
		w := newFAWorld(t, faOpts{class: pipefix.ClassConfiguration})
		applied, v, rec, err := w.apply(free)
		if !applied {
			t.Fatalf("%q %v\n%s", v, err, w.log)
		}
		if !strings.Contains(commitOf(w).CommitMessage, "[skip ci]") {
			t.Fatalf("the commit runs on push: %q", commitOf(w).CommitMessage)
		}
		mr := w.fixMRs()[0]
		if rec.PipelineFix.State != "awaiting-run" || rec.PipelineFix.CIDelta != "preserving" ||
			!reflect.DeepEqual(mr.Labels, []string{"gonk::pipeline-fix", "gonk::fix-awaiting-run"}) {
			t.Fatalf("state %q delta %q labels %v", rec.PipelineFix.State, rec.PipelineFix.CIDelta, mr.Labels)
		}
		if !strings.Contains(mr.Description, "start this merge request's pipeline") {
			t.Fatalf("the description does not ask a maintainer to start the pipeline:\n%s", mr.Description)
		}
		if st, _, _ := w.fixNote(); st != "awaiting-run" {
			t.Fatalf("fix note %q", st)
		}
	})
	t.Run("an execution-changing edit is labelled", func(t *testing.T) {
		w := newFAWorld(t, faOpts{class: pipefix.ClassConfiguration})
		applied, v, rec, err := w.apply(exec)
		if !applied {
			t.Fatalf("%q %v\n%s", v, err, w.log)
		}
		mr := w.fixMRs()[0]
		if rec.PipelineFix.CIDelta != "exec-changing" || rec.PipelineFix.State != "awaiting-run" || !hasString(mr.Labels, "gonk::ci-exec-changed") {
			t.Fatalf("delta %q state %q labels %v", rec.PipelineFix.CIDelta, rec.PipelineFix.State, mr.Labels)
		}
	})
	t.Run("auto: no skip ci, verifying", func(t *testing.T) {
		w := newFAWorld(t, faOpts{class: pipefix.ClassConfiguration, cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.CIExecChanges = "auto" }})
		applied, v, rec, err := w.apply(free)
		if !applied {
			t.Fatalf("%q %v\n%s", v, err, w.log)
		}
		if strings.Contains(commitOf(w).CommitMessage, "skip ci") || rec.PipelineFix.State != "verifying" {
			t.Fatalf("message %q state %q", commitOf(w).CommitMessage, rec.PipelineFix.State)
		}
	})
	t.Run("deny: an execution-changing edit leaves nothing and needs scope", func(t *testing.T) {
		w := newFAWorld(t, faOpts{class: pipefix.ClassConfiguration, cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.CIExecChanges = "deny" }})
		applied, v, rec, err := w.apply(exec)
		if !applied || v != "" || err != nil {
			t.Fatalf("applied=%v %q %v\n%s", applied, v, err, w.log)
		}
		if rec.PipelineFix.State != "needs-scope" || len(w.fixMRs()) != 0 {
			t.Fatalf("state %q, %d merge request(s)", rec.PipelineFix.State, len(w.fixMRs()))
		}
		if _, err := w.r.gl.Client().ResolveRef(context.Background(), w.r.pid, w.branch()); !glab.IsNotFound(err) {
			t.Fatalf("the fix branch still exists: %v", err)
		}
	})
	t.Run("a gating change is refused after the commit and the branch removed", func(t *testing.T) {
		w := newFAWorld(t, faOpts{class: pipefix.ClassConfiguration})
		applied, v, rec, err := w.apply(gating)
		if applied || err != nil || !strings.Contains(v, "CI-DEF(violation)") || rec.PipelineFix.Refusal != "CI-DEF(violation)" {
			t.Fatalf("applied=%v violation=%q err=%v refusal %q\n%s", applied, v, err, rec.PipelineFix.Refusal, w.log)
		}
		pid := w.r.pid
		want := []string{
			fmt.Sprintf("POST /api/v4/projects/%d/repository/branches", pid),
			fmt.Sprintf("POST /api/v4/projects/%d/repository/commits", pid),
			fmt.Sprintf("DELETE /api/v4/projects/%d/repository/branches/%s", pid, url.PathEscape(w.branch())),
		}
		if got := faRoutes(w.written()); !reflect.DeepEqual(got, want) {
			t.Fatalf("writes %v, want exactly %v", got, want)
		}
		if len(w.fixMRs()) != 0 || rec.PipelineFix.State != "" || rec.PipelineFix.FixCommitSHA != "" {
			t.Fatalf("after a refused commit: %d MR(s), state %q, commit %q", len(w.fixMRs()), rec.PipelineFix.State, rec.PipelineFix.FixCommitSHA)
		}
	})
	t.Run("a non-CI change in a project whose definition moved is not applied", func(t *testing.T) {
		// The shortcut: no ci path in the diff, so the definition must be
		// byte-identical at the base and at the commit. Here the fake makes it
		// differ (the branch carries another definition), which a gonk commit
		// cannot do: unresolvable, ours, no merge request.
		w := newFAWorld(t, faOpts{})
		w.r.proj.PutFileOn(w.branch(), faCI, []byte(faCIBody+"extra:\n  stage: test\n  script:\n  - ./x\n"))
		w.r.proj.PutFileOn(w.branch(), pfSource, []byte(pfSourceBody))
		w.r.gl.SetBranchHead(w.r.pid, w.branch(), plSHA)
		applied, v, _, err := w.apply(faGood())
		if applied || v != "" || err == nil || len(w.fixMRs()) != 0 {
			t.Fatalf("applied=%v %q %v, %d MR(s)", applied, v, err, len(w.fixMRs()))
		}
	})
}

// The pre-check's patterns, one by one: what each refuses and what it lets by.
func TestCIPreCheckPatterns(t *testing.T) {
	sub := func(line string) string {
		for _, c := range ciPreChecks {
			if c.re.MatchString(line) {
				return c.sub
			}
		}
		return ""
	}
	for line, want := range map[string]string{
		"  allow_failure: true":           "allow_failure",
		"  allow_failure:":                "allow_failure",
		"  when: manual":                  "when",
		"  when: \"never\"":               "when",
		"    - when: never":               "when",
		"  retry: 2":                      "retry",
		"  retry:":                        "retry",
		"  - ./run-tests || true":         "or-true",
		"  - ./run-tests || :":            "or-true",
		"  - ./run-tests || exit 0":       "or-true",
		"  - set +e":                      "set-e",
		"  - set +o errexit":              "set-e",
		"  - ./run-tests; exit 0":         "exit-0",
		"  when: on_success":              "",
		"  - ./run-tests --retry-flaky=0": "",
		"  timeout: 10m":                  "",
		"  - test -e file || exit 1":      "",
		"  - set -e":                      "",
		"  - echo exit 01":                "",
	} {
		if got := sub(line); got != want {
			t.Errorf("%q: refused as %q, want %q", line, got, want)
		}
	}
}

// ---- through the sweep: the meter outcome of plan 7.3 ----------------------------

type faSweep struct {
	outcomes []meterapi.OutcomeRequest
	rec      beadstore.Record
	log      string
	stops    int
}

// sweepWith finishes the fix session with output and runs ONE sweep tick with
// a meter that has seen the session spend tokens.
func (w *faWorld) sweepWith(output string, next string) faSweep {
	w.t.Helper()
	h := runtime.Handle(w.session)
	w.rt.SetOutput(h, []byte(output))
	w.rt.SetPhase(h, runtime.PhaseSucceeded, "")
	return w.sweepAgain(next)
}

func (w *faWorld) sweepAgain(next string) faSweep {
	w.t.Helper()
	om := &fakeOutcomeMeter{outcomeNext: next, spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 1200, AsOf: time.Now().Add(time.Hour)}}
	var log bytes.Buffer
	d := w.deps()
	d.Meter, d.Runtime, d.GL, d.BotUsername = meterClient(om.server(w.t)), w.rt, w.r.gl.Client(), "gonk"
	d.Log = testLogger(&log)
	d.Now = func() time.Time { return time.Now().Add(time.Minute) }
	d.SpendPollInterval, d.SpendDeadline = time.Millisecond, time.Second
	d.FixesLive = false // this tick judges the session; the look is not under test
	if code := runSweep(context.Background(), d); code != 0 {
		w.t.Fatalf("sweep exit %d\n%s", code, log.String())
	}
	rec, _ := w.fixRecord()
	return faSweep{outcomes: om.requests(), rec: rec, log: log.String(), stops: w.rt.stopCount()}
}

// A FINISHED FIX SESSION, THROUGH THE WHOLE SWEEP, reports what plan 7.3 says:
//
//	a refused batch          gate-failed (the session spent tokens), re-slung,
//	                         NOTHING written to GitLab
//	a failure of ours        infra-failed, re-slung, nothing written
//	needs-scope              success, the record done with the fix state
//	an accepted fix          NO OUTCOME: the record stays running with the
//	                         fix state `verifying`, the session is closed, and
//	                         later ticks neither judge it again nor write
func TestSweepJudgesAFixSessionAndReportsThePlansOutcome(t *testing.T) {
	t.Run("a refused batch is gate-failed and writes nothing", func(t *testing.T) {
		w := newFAWorld(t, faOpts{})
		weak := faFix(faEdit(pfSource, "package calc\n", "//go:build never\n\npackage calc\n"))
		got := w.sweepWith(plSessionOutput(t, weak), "retry")
		if len(got.outcomes) != 1 || got.outcomes[0].Outcome != meterapi.OutcomeGateFailed || got.outcomes[0].Trigger != "pipeline-fix" {
			t.Fatalf("outcomes %+v\n%s", got.outcomes, got.log)
		}
		if ws := w.written(); len(ws) != 0 {
			t.Fatalf("a refused batch wrote to GitLab through the sweep:\n  %s", strings.Join(faLines(ws), "\n  "))
		}
		if got.rec.PipelineFix.State != "" || got.rec.PipelineFix.Refusal != "SC-WEAKEN(build-constraint)" || got.rec.ReslungAt.IsZero() {
			t.Fatalf("record: fix state %q refusal %q reslung %v", got.rec.PipelineFix.State, got.rec.PipelineFix.Refusal, got.rec.ReslungAt)
		}
		if st, _, known := fixStateOf(got.rec, time.Now()); st != pipefix.StateProposing || !known {
			t.Fatalf("a refused, re-slung fix reads as %q (known %v), want proposing", st, known)
		}
		if got.stops != 1 {
			t.Fatalf("the judged session was stopped %d time(s)", got.stops)
		}
	})
	t.Run("a session with no batch is gate-failed and writes nothing", func(t *testing.T) {
		w := newFAWorld(t, faOpts{})
		got := w.sweepWith("session start\nI could not decide.\n", "retry")
		if len(got.outcomes) != 1 || got.outcomes[0].Outcome != meterapi.OutcomeGateFailed || len(w.written()) != 0 {
			t.Fatalf("outcomes %+v, %d write(s)", got.outcomes, len(w.written()))
		}
	})
	t.Run("a failure of ours is infra-failed and writes nothing", func(t *testing.T) {
		w := newFAWorld(t, faOpts{})
		w.cfg.Effective.Actions.PipelineFixes = false // opted out since
		got := w.sweepWith(plSessionOutput(t, faGood()), "retry")
		if len(got.outcomes) != 1 || got.outcomes[0].Outcome != meterapi.OutcomeInfraFailed {
			t.Fatalf("outcomes %+v\n%s", got.outcomes, got.log)
		}
		if len(w.written()) != 0 || got.rec.PipelineFix.Refusal != "" || got.rec.PipelineFix.State != "" {
			t.Fatalf("%d write(s), refusal %q, state %q", len(w.written()), got.rec.PipelineFix.Refusal, got.rec.PipelineFix.State)
		}
	})
	t.Run("needs-scope is success and terminal", func(t *testing.T) {
		w := newFAWorld(t, faOpts{})
		got := w.sweepWith(plSessionOutput(t, faNeedsScope("The fix is in another file.", "calc/other.go")), "done")
		if len(got.outcomes) != 1 || got.outcomes[0].Outcome != meterapi.OutcomeSuccess {
			t.Fatalf("outcomes %+v\n%s", got.outcomes, got.log)
		}
		if got.rec.State != beadstore.StateDone || got.rec.PipelineFix.State != "needs-scope" {
			t.Fatalf("record state %q fix state %q", got.rec.State, got.rec.PipelineFix.State)
		}
		if st, _, known := fixStateOf(got.rec, time.Now()); st != pipefix.StateNeedsScope || !known {
			t.Fatalf("fixStateOf = %q known %v", st, known)
		}
	})
	t.Run("an accepted fix reports no outcome and waits for the exit check", func(t *testing.T) {
		// The state the APPLY leaves, and that a build WITHOUT the exit
		// check then does nothing with it. What the exit check does with it
		// is sweep_pipefix_verify_test.go.
		judge := pipefixJudge
		pipefixJudge = nil
		t.Cleanup(func() { pipefixJudge = judge })
		w := newFAWorld(t, faOpts{})
		got := w.sweepWith(plSessionOutput(t, faGood()), "done")
		if len(got.outcomes) != 0 {
			t.Fatalf("an outcome was reported for a fix nothing has judged: %+v", got.outcomes)
		}
		f := got.rec.PipelineFix
		if got.rec.State != beadstore.StateRunning || f.State != "verifying" || f.MRIID == 0 || !glab.IsCommitSHA(f.FixCommitSHA) ||
			f.AppliedAttempt != 1 || f.VerifyDeadline.IsZero() {
			t.Fatalf("record state %q, fix %+v\n%s", got.rec.State, f, got.log)
		}
		if got.rec.ReservationID == "" || !got.rec.ReslungAt.IsZero() {
			t.Fatalf("the reservation was given up or the fix re-slung: %q %v", got.rec.ReservationID, got.rec.ReslungAt)
		}
		if st, _, known := fixStateOf(got.rec, time.Now()); st != pipefix.StateVerifying || !known {
			t.Fatalf("fixStateOf = %q known %v", st, known)
		}
		if got.stops != 1 {
			t.Fatalf("the finished session was stopped %d time(s), want once", got.stops)
		}
		if len(w.fixMRs()) != 1 {
			t.Fatalf("%d merge request(s)", len(w.fixMRs()))
		}
		// Later ticks: nothing judges it, nothing is written, nothing reported.
		w.resetWrites()
		for i := 0; i < 2; i++ {
			again := w.sweepAgain("done")
			if len(again.outcomes) != 0 || len(w.written()) != 0 || again.rec.Revision != got.rec.Revision || again.stops != 1 {
				t.Fatalf("tick %d after the merge request opened: %d outcome(s), %d write(s), revision changed %v, stops %d\n%s",
					i, len(again.outcomes), len(w.written()), again.rec.Revision != got.rec.Revision, again.stops, again.log)
			}
		}
		if len(w.fixMRs()) != 1 {
			t.Fatal("a second merge request was opened")
		}
	})
}

// A fix verdict never reaches the issue-shaped verdict path: applyVerdict does
// nothing with one, and the other agents' chains refuse the vocabulary.
func TestApplyVerdictDoesNothingWithAFixVerdict(t *testing.T) {
	for _, v := range []effects.Verdict{effects.VerdictFix, effects.VerdictNeedsScope} {
		w := newFAWorld(t, faOpts{})
		var log bytes.Buffer
		d := w.deps()
		d.Log = testLogger(&log)
		rec, _ := w.fixRecord()
		rec.IssueIID = 1
		w.resetWrites()
		applyVerdict(context.Background(), d, rec, effects.Batch{Verdict: v})
		if len(w.written()) != 0 {
			t.Fatalf("applyVerdict(%s) wrote to GitLab: %v", v, faLines(w.written()))
		}
		if !strings.Contains(log.String(), "pipeline-fix verdict reached the issue verdict path") {
			t.Fatalf("applyVerdict(%s) did not say it was misrouted:\n%s", v, log.String())
		}
	}
}

// ---- needs-scope at dispatch -----------------------------------------------------

// A FIX WHOSE CITED FILES CANNOT BE EDITED AT THE BASE (here: the file is
// empty) starts no session: gonk-dispatch lands needs-scope itself, for zero
// tokens, and the fix is terminal.
func TestDispatchLandsNeedsScopeWhenNothingIsEditable(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	w.r.proj.PutFileOn(plSHA, "calc/sum.py", []byte(""))
	vars := w.firedFix()
	pd := w.dispatchFixReal(vars, pfRunDecision())
	if pd.code == 0 || len(pd.rt.Starts()) != 0 {
		t.Fatalf("exit %d, %d session(s)\n%s", pd.code, len(pd.rt.Starts()), pd.logs)
	}
	rec, _ := w.fixRecord()
	if rec.State != beadstore.StateDone || rec.PipelineFix.State != "needs-scope" {
		t.Fatalf("record state %q fix state %q\n%s", rec.State, rec.PipelineFix.State, pd.logs)
	}
	if st, _, known := fixStateOf(rec, w.now); st != pipefix.StateNeedsScope || !known {
		t.Fatalf("fixStateOf = %q known %v", st, known)
	}
	st, body, ok := w.fixNote()
	if !ok || st != "needs-scope" || !strings.Contains(body, "more scope is needed") {
		t.Fatalf("fix note %q:\n%s", st, body)
	}
	if !hasString(w.landingLabels(), labelNeedsHuman) {
		t.Fatalf("landing labels %v", w.landingLabels())
	}
	// The next look does not dispatch it again.
	before := len(w.orders.orders())
	w.look()
	if n := len(w.orders.orders()) - before; n != 0 {
		t.Fatalf("%d more order(s) for a fix that needs scope", n)
	}
}

// ---- the schema is documented where it lives -------------------------------------

func TestEveryFixRefusalCodeIsDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/reference/effects-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, r := range append(effects.Rules(), pipefixApplyRules()...) {
		if !strings.Contains(doc, "`"+string(r)+"`") {
			t.Errorf("rule code %s is not in docs/reference/effects-contract.md", r)
		}
	}
	// Every refusal the table produces carries a documented rule.
	known := map[string]bool{}
	for _, r := range append(effects.Rules(), pipefixApplyRules()...) {
		known[string(r)] = true
	}
	for _, tc := range faRefusals() {
		rule, _, _ := strings.Cut(tc.code, "(")
		if !known[rule] {
			t.Errorf("refusal %q uses rule %q, which is in no rule list", tc.name, rule)
		}
	}
}

func TestEveryFixRecordFieldLabelAndMarkerIsDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/reference/gonk-state-record.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, typ := range []reflect.Type{reflect.TypeOf(beadstore.PipelineFix{}), reflect.TypeOf(beadstore.PipelineFixFile{}),
		reflect.TypeOf(beadstore.FixContext{}), reflect.TypeOf(beadstore.FixContextFile{})} {
		for i := 0; i < typ.NumField(); i++ {
			if name := typ.Field(i).Name; !strings.Contains(doc, "`"+name+"`") {
				t.Errorf("%s.%s is not documented in docs/reference/gonk-state-record.md", typ.Name(), name)
			}
		}
	}
	for _, s := range append([]string{"gonk::pipeline-fix", "gonk::needs-human", fixStatusOpen, fixStatusClose, "gonk-fix:",
		beadstore.FixWeakeningDecided, beadstore.FixWeakeningNotDecidable, "[skip ci]"}, fixMRLabels...) {
		if !strings.Contains(doc, s) {
			t.Errorf("%q is not documented in docs/reference/gonk-state-record.md", s)
		}
	}
}

// The CI pre-check reads CI files only: a source line that happens to spell
// one of its patterns is none of its business.
func TestCIPreCheckOnlyReadsCIFiles(t *testing.T) {
	w := newFAWorld(t, faOpts{})
	b := faFix(faEdit(pfSource, "\tsum := 0\n", "\tsum := 0 // when: manual, allow_failure, exit 0\n"), faEdit(pfSource, pfOld, pfNew))
	if applied, v, _, err := w.apply(b); !applied {
		t.Fatalf("a source change was refused: %q %v", v, err)
	}
}
