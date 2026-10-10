package main

// gonk-2usw part DISPATCH, in the tios P8 in-process world: a failed
// pipeline's hook -> intake -> the diagnosis order -> the diagnosis session ->
// the sweep lands the diagnosis -> THE SAME SWEEP decides a fix may be
// dispatched and fires exactly one pipeline-fix order -> gonk-dispatch reads
// that order's vars as main.go does -> the seam to part AGENT (a fake here)
// is asked ONCE to prepare the fix session, with the envelope the controller
// chose -> one fix session starts -> the landing says so.
//
// And the same world with a project that has NOT opted in: nothing at all.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// armFixes makes the world's gonk-gate a build in which a fix session can be
// prepared: the fix pass is on, the pack has the pipefix agent, the project's
// pinned .gonk.yml selects the python profile, and the target's head is the
// failing pipeline's commit.
func (w *rtWorld) armFixes(cfg *meterapi.ProjectResponse) *fakePreparer {
	w.t.Helper()
	prep := &fakePreparer{}
	w.fixLive, w.fixPrep, w.packDir = true, prep.prepare, pfPackDir(w.t, pfScopeTOML)
	w.fixConfig = func(context.Context, string) (*meterapi.ProjectResponse, error) { return cfg, nil }
	if w.mr {
		w.gl.SetBranchHead(w.proj.ID, "main", plSHAOld)
		w.proj.PutFileOn(plSHAOld, ".gonk.yml", []byte(pfGonkYML))
		w.gl.SetMRHead(w.proj.ID, rtMRIID, plSHA)
	} else {
		w.gl.SetBranchHead(w.proj.ID, "main", plSHA)
		w.proj.PutFileOn(plSHA, ".gonk.yml", []byte(pfGonkYML))
	}
	return prep
}

// fixOrders is every pipeline-fix order the sweep has fired.
func (w *rtWorld) fixOrders() []map[string]string {
	var out []map[string]string
	for _, o := range w.resling.orders() {
		if o.Order == dispatchOrderName && o.Vars["trigger"] == "pipeline-fix" {
			out = append(out, o.Vars)
		}
	}
	return out
}

// diagnose drives pipeline 900's failure to a landed `code` diagnosis and
// returns the run's anchor.
func (w *rtWorld) diagnose() string {
	w.t.Helper()
	w.failPipeline(900, map[string]string{"unit-tests": plTestLog})
	o := w.deliver(900, "failed").requireOrder(w.t)
	if code := w.dispatch(o); code != 0 {
		w.t.Fatalf("diagnosis dispatch exit %d\n%s", code, w.gateLog)
	}
	w.answer(sessionName(w.proj.ID, 900), rtDiagnosis(effects.ClassCode, "unit-tests", plGoodLine, nil), plGoodLine)
	w.sweep()
	if st, _ := w.runState(o["bead_anchor"]); st != pipelinectx.RunStateDiagnosed {
		w.t.Fatalf("the diagnosis did not land (run note %q)\n%s", st, w.gateLog)
	}
	return o["bead_anchor"]
}

func TestPipelineFixRoundTrip(t *testing.T) {
	for _, mr := range []bool{false, true} {
		name := "default-branch"
		if mr {
			name = "merge-request"
		}
		t.Run(name, func(t *testing.T) { roundTripFix(t, mr) })
	}
}

func roundTripFix(t *testing.T, mr bool) {
	w := newRTWorld(t, mr)
	targets := []string{"ref"}
	if mr {
		targets = []string{"ref", "mr"} // the per-project opt-in for MR targets (owner decision Q7)
	}
	prep := w.armFixes(pfConfig(targets...))
	pid := w.proj.ID

	// ---- 1. the diagnosis lands; the SAME sweep fires exactly one fix order ----
	anchor := w.diagnose()
	fixAnchor, err := pipelinectx.FixAnchor(anchor)
	if err != nil {
		t.Fatal(err)
	}
	orders := w.fixOrders()
	if len(orders) != 1 {
		t.Fatalf("%d pipeline-fix order(s) after the diagnosis landed, want exactly one\n%s", len(orders), w.gateLog)
	}
	fix := orders[0]
	if fix["bead_anchor"] != fixAnchor || fix["pipeline_id"] != "900" || fix["pipeline_target"] != w.target.String() {
		t.Fatalf("fix order vars %v", fix)
	}
	if !strings.Contains(w.gateLog.String(), `msg="sweep: pipeline fix eligibility"`) || !strings.Contains(w.gateLog.String(), "eligible=true") {
		t.Fatalf("the sweep did not log the eligibility decision:\n%s", w.gateLog)
	}
	if len(prep.calls) != 0 || len(w.sessions()) != 1 {
		t.Fatalf("something started before the order was dispatched: prepares %d sessions %v", len(prep.calls), w.sessionNames())
	}

	// ---- 2. more sweeps before the order is picked up: still one order ----
	w.sweep()
	w.sweep()
	if n := len(w.fixOrders()); n != 1 {
		t.Fatalf("%d fix orders after three sweeps, want 1", n)
	}

	// ---- 3. gonk-dispatch, reading the order from the env as main.go does ----
	w.gate2.resp = pfRunDecision()
	if code := w.dispatch(fix); code != 0 {
		t.Fatalf("fix dispatch exit %d\n%s", code, w.gateLog)
	}
	if len(prep.calls) != 1 {
		t.Fatalf("the fix session's preparer was called %d times, want exactly once", len(prep.calls))
	}
	in := prep.calls[0]
	wantSession := runtime.Name(pipefixAgent, pid, runtime.KindPipeline, 900, 1)
	wantBranch, wantCfgSHA := "main", plSHA
	if mr {
		wantBranch, wantCfgSHA = rtMRBranch, plSHAOld
	}
	f := in.Fix.PipelineFix
	if in.Session != wantSession || in.Fix.BeadAnchor != fixAnchor || in.Fix.Trigger != "pipeline-fix" ||
		f.RunAnchor != anchor || f.Class != "code" || f.BaseSHA != plSHA || f.BasePipelineID != 900 ||
		f.TargetBranch != wantBranch || f.ConfigSHA != wantCfgSHA ||
		!reflect.DeepEqual(f.Envelope, []string{"docs", "source"}) || len(f.Lift) != 0 ||
		!reflect.DeepEqual(f.Files, []string{"calc/sum.py"}) || f.MaxFiles != 3 || f.MaxLines != 40 ||
		len(f.Targets) != 1 || f.Targets[0].Name != "unit-tests" || f.Targets[0].ErrorSignature == "" {
		t.Fatalf("the preparer was not handed the expected envelope:\nsession %q\nfix %+v", in.Session, in.Fix)
	}
	if in.Run.BeadAnchor != anchor || in.Run.PipelineDiagnosis.Class != "code" {
		t.Fatalf("the preparer was not handed the diagnosed run: %+v", in.Run.PipelineDiagnosis)
	}
	got := w.sessionNames()
	if len(got) != 2 || got[1] != wantSession || w.sessions()[1].Agent != pipefixAgent || string(w.sessions()[1].Prompt) != pfPrompt {
		t.Fatalf("sessions %v, want the diagnosis session and then %s", got, wantSession)
	}
	rec := w.record(fixAnchor)
	if rec.State != beadstore.StateRunning || rec.SessionID != wantSession || rec.PipelineFix.ContextSHA256 != pfContextSHA {
		t.Fatalf("fix record %+v", rec)
	}

	// ---- 4. the landing: the run note untouched, ONE fix note, proposing ----
	if st, _ := w.runState(anchor); st != pipelinectx.RunStateDiagnosed {
		t.Fatalf("the run note is %q after the fix was dispatched, want diagnosed", st)
	}
	var fixNotes []string
	for _, n := range w.runNotes() {
		if st := pipelinectx.FixMarkerStates(n.Body, fixAnchor); len(st) > 0 {
			if n.Author.ID != plBotID {
				t.Fatalf("a fix marker in a note by %+v", n.Author)
			}
			fixNotes = append(fixNotes, st...)
			if !strings.Contains(n.Body, "/-/pipelines/900)") {
				t.Fatalf("the fix note does not link its pipeline:\n%s", n.Body)
			}
		}
	}
	if len(fixNotes) != 1 || fixNotes[0] != string(pipefix.StateProposing) {
		t.Fatalf("fix notes %v, want exactly one, proposing", fixNotes)
	}
	w.requireNoIssueTriage()

	// ---- 5. later sweeps: a running fix is in flight; nothing more is fired,
	// and the unjudged fix session is neither reported nor stopped ----
	outcomes := len(w.om.requests())
	w.tick()
	w.tick()
	if n := len(w.fixOrders()); n != 1 {
		t.Fatalf("%d fix orders once the fix session runs, want 1", n)
	}
	if n := len(w.om.requests()); n != outcomes {
		t.Fatalf("the sweep reported an outcome for the fix session: %+v", w.om.requests()[outcomes:])
	}
	if !strings.Contains(w.gateLog.String(), "reason=E9.fix-in-flight") {
		t.Fatalf("later looks did not log E9.fix-in-flight:\n%s", w.gateLog)
	}

	// ---- 6. intake still treats the run as handled: no second diagnosis ----
	w.failPipeline(910, map[string]string{"unit-tests": plTestLog})
	w.deliver(910, "failed").requireDropped(t, "run_already_handled")
}

// THE SAME WORLD, NOT OPTED IN: the diagnosis lands exactly as before and
// nothing else happens -- no order, no fix record, no fix note, no label --
// across the first sweep and every later look.
func TestPipelineFixRoundTripIsInertWithoutTheOptIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func() *meterapi.ProjectResponse
	}{
		{"pipeline_fixes false (the default)", func() *meterapi.ProjectResponse {
			c := pfConfig()
			c.Effective.Actions.PipelineFixes = false
			return c
		}},
		{"an operator veto folded the action off", func() *meterapi.ProjectResponse {
			c := pfConfig()
			c.Effective.Actions.PipelineFixes = false
			c.Effective.PipelineFix.AllowCI = true // whatever else the project asked for
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newRTWorld(t, false)
			prep := w.armFixes(tc.cfg())
			anchor := w.diagnose()
			fixAnchor, _ := pipelinectx.FixAnchor(anchor)
			writesAfterDiagnosis := len(w.writes())
			for i := 0; i < 3; i++ {
				w.tick()
			}
			if n := len(w.fixOrders()); n != 0 {
				t.Fatalf("%d fix order(s) for a project that has not opted in", n)
			}
			if _, ok, _ := w.store.Get(context.Background(), fixAnchor); ok {
				t.Fatal("a fix record exists for a project that has not opted in")
			}
			if w := w.writes()[writesAfterDiagnosis:]; len(w) != 0 {
				t.Fatalf("GitLab was written after the diagnosis landed: %v", w)
			}
			for _, n := range w.runNotes() {
				if strings.Contains(n.Body, "gonk:pipeline-fix") {
					t.Fatalf("a fix note on the landing of a project that has not opted in:\n%s", n.Body)
				}
			}
			if hasString(w.labels(), labelNeedsHuman) {
				t.Fatal("gonk::needs-human on the landing of a project that has not opted in")
			}
			if len(prep.calls) != 0 || len(w.sessions()) != 1 {
				t.Fatalf("prepares %d sessions %v", len(prep.calls), w.sessionNames())
			}
			if !strings.Contains(w.gateLog.String(), "reason=E1.pipeline-fixes-off") {
				t.Fatalf("the look did not log E1.pipeline-fixes-off:\n%s", w.gateLog)
			}
		})
	}
}

// THE WHOLE WAY TO AN OPEN MERGE REQUEST, with nothing faked but GitLab, the
// meter, the order route and the runtime (gonk-2usw parts AGENT + DISPATCH +
// APPLY together): hook -> intake -> diagnosis -> the sweep fires the fix
// order -> gonk-dispatch prepares the session through the PRODUCTION preparer
// -> the session answers with an edit -> the sweep gates it, commits it, opens
// the merge request and moves the fix note -> the fix record is `verifying`
// and no outcome was reported for it -> later sweeps change nothing -> intake
// still treats the run as handled.
func TestPipelineFixRoundTripToAnOpenMergeRequest(t *testing.T) {
	for _, mr := range []bool{false, true} {
		name := "default-branch"
		if mr {
			name = "merge-request"
		}
		t.Run(name, func(t *testing.T) {
			// This test stops at "merge request open" and asserts that a build
			// WITHOUT the exit check then does nothing more. With it, the story
			// goes on: TestPipelineFixRoundTripToAConfirmedFixAndASequencedFollowUp.
			judge := pipefixJudge
			pipefixJudge = nil
			t.Cleanup(func() { pipefixJudge = judge })
			w := newRTWorld(t, mr)
			targets := []string{"ref"}
			if mr {
				targets = []string{"ref", "mr"}
			}
			w.armFixes(pfConfig(targets...))
			w.fixPrep = pipefixPrepare // the production preparer, not the double
			pid := w.proj.ID

			anchor := w.diagnose()
			fixAnchor, _ := pipelinectx.FixAnchor(anchor)
			orders := w.fixOrders()
			if len(orders) != 1 {
				t.Fatalf("%d fix order(s)\n%s", len(orders), w.gateLog)
			}
			w.gate2.resp = pfRunDecision()
			if code := w.dispatch(orders[0]); code != 0 {
				t.Fatalf("fix dispatch exit %d\n%s", code, w.gateLog)
			}
			session := runtime.Name(pipefixAgent, pid, runtime.KindPipeline, 900, 1)
			rec := w.record(fixAnchor)
			if rec.State != beadstore.StateRunning || rec.SessionID != session || !rec.PipelineFix.Context.Recorded() {
				t.Fatalf("fix record after dispatch: state %q session %q context %v", rec.State, rec.SessionID, rec.PipelineFix.Context.Recorded())
			}

			// The session answers. The text it edits must be in the prompt it
			// was started with (answer checks that).
			fix := effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{
				{Kind: effects.KindEdit, Path: "calc/sum.py", Old: "    return 0\n", New: "    return sum(xs)\n"},
				{Kind: effects.KindFixMeta, Reason: "total returned a constant; it now adds its argument."},
			}}
			w.answer(session, fix, "    return 0")
			outcomes, before := len(w.om.requests()), len(w.gl.Writes())
			w.sweep()

			branch, _ := pipelinectx.FixBranch(fixAnchor)
			var fixMR *glab.MergeRequest
			for _, m := range w.gl.MRsOf(pid) {
				if m.SourceBranch == branch {
					m := m
					fixMR = &m
				}
			}
			if fixMR == nil {
				t.Fatalf("no merge request on %s\n%s", branch, w.gateLog)
			}
			wantTarget, wantRelated := "main", "Related to #"
			if mr {
				wantTarget, wantRelated = rtMRBranch, fmt.Sprintf("Related to !%d", rtMRIID)
			}
			if fixMR.TargetBranch != wantTarget || !strings.Contains(fixMR.Description, wantRelated) {
				t.Fatalf("fix MR targets %q; description:\n%s", fixMR.TargetBranch, fixMR.Description)
			}
			if a, c, ok := pipelinectx.ParseFixMRMarker(fixMR.Description); !ok || a != fixAnchor || c != rec.PipelineFix.CauseKey {
				t.Fatalf("fix MR marker (%q, %q, %v)", a, c, ok)
			}
			var routes []string
			for _, wr := range w.gl.Writes()[before:] {
				routes = append(routes, wr.Method+" "+wr.Path)
			}
			if len(routes) != 4 || !strings.HasSuffix(routes[0], "/repository/branches") || !strings.HasSuffix(routes[1], "/repository/commits") ||
				!strings.HasSuffix(routes[2], "/merge_requests") || !strings.Contains(routes[3], "/notes/") || !strings.HasPrefix(routes[3], "PUT ") {
				t.Fatalf("the sweep's writes: %v", routes)
			}
			got := w.record(fixAnchor)
			f := got.PipelineFix
			if got.State != beadstore.StateRunning || f.State != string(pipefix.StateVerifying) || f.MRIID != fixMR.IID ||
				f.FixCommitSHA == "" || f.Weakening != beadstore.FixWeakeningNotDecidable {
				t.Fatalf("fix record: state %q fix %+v\n%s", got.State, f, w.gateLog)
			}
			if n := len(w.om.requests()); n != outcomes {
				t.Fatalf("an outcome was reported for a fix nothing has judged: %+v", w.om.requests()[outcomes:])
			}
			var states []string
			for _, n := range w.runNotes() {
				states = append(states, pipelinectx.FixMarkerStates(n.Body, fixAnchor)...)
			}
			if len(states) != 1 || states[0] != string(pipefix.StateVerifying) {
				t.Fatalf("fix notes %v, want exactly one, verifying", states)
			}
			if st, _ := w.runState(anchor); st != pipelinectx.RunStateDiagnosed {
				t.Fatalf("the run note is %q", st)
			}
			w.requireNoIssueTriage()

			// Later sweeps and looks: nothing more. One MR, no order, no write,
			// no outcome; the look sees the fix in flight.
			before = len(w.gl.Writes())
			w.tick()
			w.tick()
			if n := len(w.gl.Writes()) - before; n != 0 {
				t.Fatalf("%d GitLab write(s) after the merge request opened: %v", n, w.gl.Writes()[before:])
			}
			if len(w.fixOrders()) != 1 || len(w.om.requests()) != outcomes {
				t.Fatalf("orders %d outcomes %d after the merge request opened", len(w.fixOrders()), len(w.om.requests()))
			}
			if !strings.Contains(w.gateLog.String(), "reason=E9.fix-in-flight") {
				t.Fatalf("later looks did not log E9.fix-in-flight:\n%s", w.gateLog)
			}
		})
	}
}

// THE WHOLE STORY, FOR BOTH TARGETS, with nothing faked but GitLab, the
// meter, the order route and the runtime (gonk-2usw part VERIFY on top of
// AGENT + DISPATCH + APPLY):
//
//	a failed pipeline's hook -> intake -> diagnosis -> the sweep fires the
//	fix order -> the fix session answers -> the sweep commits and opens the
//	merge request -> ITS PIPELINE GOES GREEN -> the exit check says `fixed`
//	and reports ONE success -> a PERSON merges it -> the target's next
//	pipeline fails in the same job, differently -> `confirmed`, and THE RUN
//	THE FIX ENDED IS CLOSED -> that same pipeline's hook is now A NEW RUN
//	(the fix epoch), though its fingerprint is the old one -> it is diagnosed
//	-> and gets ITS OWN fix merge request: the second, sequenced one, which
//	could not exist before the first merged.
//
// gonk merges nothing anywhere in it. Under -v it prints the two artifacts
// the plan's evidence log quotes: the meter's outcome requests and the fake
// GitLab's write log from the fix dispatch to `fixed`.
func TestPipelineFixRoundTripToAConfirmedFixAndASequencedFollowUp(t *testing.T) {
	for _, mr := range []bool{false, true} {
		name := "default-branch"
		if mr {
			name = "merge-request"
		}
		t.Run(name, func(t *testing.T) { roundTripVerifiedFix(t, mr) })
	}
}

// rtOtherLog fails the SAME job with a different first error line.
const rtOtherLine = "ValueError: total() was handed an empty list of crates"
const rtOtherLog = "$ ./run-tests\n" +
	"collecting tests\n" +
	"test_addition ... ok\n" +
	"test_empty_list ... FAILED\n" +
	rtOtherLine + "\n" +
	"1 failed, 1 passed\n" +
	"ERROR: Job failed: exit code 1\n"

// fixMRsOf is every merge request on a gonk fix branch, in creation order.
func (w *rtWorld) fixMRsOf() []glab.MergeRequest {
	var out []glab.MergeRequest
	for _, m := range w.gl.MRsOf(w.proj.ID) {
		if pipelinectx.IsFixBranch(m.SourceBranch) {
			out = append(out, m)
		}
	}
	return out
}

// now is the sweep's clock.
func (w *rtWorld) now() time.Time { return time.Now().Add(w.skew) }

func roundTripVerifiedFix(t *testing.T, mr bool) {
	w := newRTWorld(t, mr)
	targets, targetBranch := []string{"ref"}, "main"
	if mr {
		targets, targetBranch = []string{"ref", "mr"}, rtMRBranch
	}
	w.armFixes(pfConfig(targets...))
	w.fixPrep = pipefixPrepare
	pid := w.proj.ID
	// The pipeline definition is a file of the repository, so the lint of a
	// commit gonk creates answers from that commit.
	w.proj.PutFileOn(plSHA, ".gitlab-ci.yml", []byte(plMergedYAML))
	w.gl.SetCILintFromFile(pid, ".gitlab-ci.yml")
	fixBatch := effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{
		{Kind: effects.KindEdit, Path: "calc/sum.py", Old: "    return 0\n", New: "    return sum(xs)\n"},
		{Kind: effects.KindFixMeta, Reason: "total returned a constant; it now adds its argument."},
	}}

	// ---- 1. diagnosis -> fix order -> fix session -> merge request open ----
	anchor := w.diagnose()
	fixAnchor, _ := pipelinectx.FixAnchor(anchor)
	orders := w.fixOrders()
	if len(orders) != 1 {
		t.Fatalf("%d fix order(s)\n%s", len(orders), w.gateLog)
	}
	mark := len(w.gl.Writes())
	w.gate2.resp = pfRunDecision()
	if code := w.dispatch(orders[0]); code != 0 {
		t.Fatalf("fix dispatch exit %d\n%s", code, w.gateLog)
	}
	session := runtime.Name(pipefixAgent, pid, runtime.KindPipeline, 900, 1)
	w.answer(session, fixBatch, "    return 0")
	outcomesBefore := len(w.om.requests())
	w.sweep()
	rec := w.record(fixAnchor)
	if rec.PipelineFix.State != string(pipefix.StateVerifying) || len(w.fixMRsOf()) != 1 {
		t.Fatalf("after the apply: fix state %q, %d fix merge request(s)\n%s", rec.PipelineFix.State, len(w.fixMRsOf()), w.gateLog)
	}
	first := w.fixMRsOf()[0]
	if first.TargetBranch != targetBranch || first.State != "opened" {
		t.Fatalf("first fix merge request: target %q state %q", first.TargetBranch, first.State)
	}

	// ---- 2. its pipeline goes green -> fixed, ONE success ----
	w.gl.AddPipeline(pid, glab.Pipeline{ID: 2001, Ref: first.SourceBranch, SHA: rec.PipelineFix.FixCommitSHA, Status: "success",
		Source: "merge_request_event", CreatedAt: w.now()})
	w.gl.AddMRPipeline(pid, first.IID, 2001)
	for i, j := range []string{"build", "unit-tests", "lint"} {
		stage := "test"
		if j == "build" {
			stage = "build"
		}
		w.gl.AddJob(pid, 2001, glab.Job{ID: 200100 + int64(i), Name: j, Stage: stage, Status: "success"})
	}
	w.tick()
	rec = w.record(fixAnchor)
	out := w.om.requests()[outcomesBefore:]
	if rec.PipelineFix.State != string(pipefix.StateFixed) || rec.State != beadstore.StateDone || rec.PipelineFix.VerdictRule != "J5.FIXED" ||
		len(out) != 1 || out[0].Outcome != meterapi.OutcomeSuccess || out[0].BeadID != fixAnchor {
		t.Fatalf("after the green pipeline: fix state %q record %q rule %q outcomes %+v\n%s", rec.PipelineFix.State, rec.State,
			rec.PipelineFix.VerdictRule, out, w.gateLog)
	}
	// A python change: gonk has no weakening predicate for it, and says so.
	if !reflect.DeepEqual(rec.PipelineFix.NoAutoMerge, []string{noAutoMergeWeakening}) {
		t.Fatalf("NoAutoMerge %v", rec.PipelineFix.NoAutoMerge)
	}
	first = w.fixMRsOf()[0]
	if got := stateLabels(first); !reflect.DeepEqual(got, []string{labelFixVerified}) || first.State != "opened" {
		t.Fatalf("first fix merge request: state labels %v, state %q -- gonk merges nothing", got, first.State)
	}
	if !strings.Contains(statusBlock(t, first.Description), "**Verified.**") {
		t.Fatalf("status block:\n%s", statusBlock(t, first.Description))
	}
	var states []string
	for _, n := range w.runNotes() {
		states = append(states, pipelinectx.FixMarkerStates(n.Body, fixAnchor)...)
	}
	if len(states) != 1 || states[0] != "fixed" {
		t.Fatalf("fix notes %v, want exactly one, fixed", states)
	}
	if st, body := w.runState(anchor); st != pipelinectx.RunStateDiagnosed || !strings.Contains(body, fmt.Sprintf("- Fix: merge request !%d -- the diagnosed failure no longer occurs", first.IID)) {
		t.Fatalf("run note %q:\n%s", st, body)
	}
	fixedPath := w.gl.Writes()[mark:]
	requireNoMergeCall(t, fixedPath)
	t.Logf("WRITE LOG (%s): fix dispatch -> merge request -> pipeline green -> fixed\n  %s", w.target, strings.Join(faLines(fixedPath), "\n  "))
	for _, o := range w.om.requests() {
		t.Logf("LEDGER trigger=%-18s bead=%s attempt=%d rung=%s outcome=%s", o.Trigger, o.BeadID, o.Attempt, o.Rung, o.Outcome)
	}

	// Still fixed, still one merge request, nothing more reported or written
	// while a person takes their time.
	writes := len(w.gl.Writes())
	w.tick()
	w.tick()
	if n := len(w.gl.Writes()) - writes; n != 0 || len(w.om.requests()) != outcomesBefore+1 || len(w.fixOrders()) != 1 {
		t.Fatalf("while the fixed merge request waits: %d write(s), %d outcome(s), %d order(s): %v", n,
			len(w.om.requests())-outcomesBefore, len(w.fixOrders()), faLines(w.gl.Writes()[writes:]))
	}

	// ---- 3. a PERSON merges it; the target's next pipeline fails in the
	// same job, differently ----
	mergedAt := w.now()
	w.gl.SetMRState(pid, first.IID, "merged", mergedAt)
	w.addPipelineOnAt(920, plSHA, "failed", mergedAt.Add(time.Minute))
	w.gl.AddJob(pid, 920, glab.Job{ID: 9201, Name: "build", Stage: "build", Status: "success"})
	w.gl.AddJob(pid, 920, glab.Job{ID: 9202, Name: "unit-tests", Stage: "test", Status: "failed", FailureReason: "script_failure"})
	w.gl.AddJob(pid, 920, glab.Job{ID: 9203, Name: "lint", Stage: "test", Status: "success"})
	w.gl.SetTrace(pid, 9202, []byte(rtOtherLog))
	w.tick()
	rec = w.record(fixAnchor)
	if rec.PipelineFix.State != string(pipefix.StateConfirmed) || rec.PipelineFix.ConfirmPipelineID != 920 {
		t.Fatalf("after the merge and pipeline 920: fix state %q on %d\n%s", rec.PipelineFix.State, rec.PipelineFix.ConfirmPipelineID, w.gateLog)
	}
	// THE RUN THE FIX ENDED IS CLOSED.
	if run := w.record(anchor); run.PipelineRunOpen || run.PipelineLatestFailed == 920 {
		t.Fatalf("the run the fix ended: open %v, latest failed %d", run.PipelineRunOpen, run.PipelineLatestFailed)
	}
	if st, body := w.runState(anchor); st != pipelinectx.RunStateSuperseded || !strings.Contains(body, fmt.Sprintf("the fix merge request !%d merged", first.IID)) {
		t.Fatalf("the ended run's note %q:\n%s", st, body)
	}
	if n := len(w.om.requests()); n != outcomesBefore+1 {
		t.Fatalf("the confirmation reported an outcome: %+v", w.om.requests()[outcomesBefore+1:])
	}

	// ---- 4. the SAME fingerprint again is a NEW run ----
	o2 := w.deliver(920, "failed").requireOrder(t)
	anchor2 := o2["bead_anchor"]
	if anchor2 == anchor || o2["pipeline_fingerprint"] != orders[0]["pipeline_fingerprint"] || o2["pipeline_fix_epoch"] != fmt.Sprint(first.IID) ||
		!strings.HasSuffix(anchor2, fmt.Sprintf(":f%d", first.IID)) {
		t.Fatalf("pipeline 920 after the merged fix: anchor %q (first run %q), fingerprint %q, epoch %q; want a NEW run with the same fingerprint and epoch %d",
			anchor2, anchor, o2["pipeline_fingerprint"], o2["pipeline_fix_epoch"], first.IID)
	}
	w.gate2.resp = plRunDecision()
	if code := w.dispatch(o2); code != 0 {
		t.Fatalf("second diagnosis dispatch exit %d\n%s", code, w.gateLog)
	}
	w.answer(sessionName(pid, 920), rtDiagnosis(effects.ClassCode, "unit-tests", rtOtherLine, nil), rtOtherLine)
	w.sweep()
	if st, _ := w.runState(anchor2); st != pipelinectx.RunStateDiagnosed {
		t.Fatalf("the new run was not diagnosed (%q)\n%s", st, w.gateLog)
	}
	if st, _ := w.runState(anchor); st != pipelinectx.RunStateSuperseded {
		t.Fatalf("the old run's note changed to %q", st)
	}

	// ---- 5. ...and gets the SECOND, SEQUENCED fix ----
	fixAnchor2, _ := pipelinectx.FixAnchor(anchor2)
	orders = w.fixOrders()
	if len(orders) != 2 || orders[1]["bead_anchor"] != fixAnchor2 || orders[1]["pipeline_id"] != "920" {
		t.Fatalf("fix orders after the new run was diagnosed: %v\n%s", orders, w.gateLog)
	}
	// One open fix merge request per target, always: none is open now.
	for _, m := range w.fixMRsOf() {
		if m.State == "opened" {
			t.Fatalf("a fix merge request is open before the follow-up was dispatched: !%d", m.IID)
		}
	}
	w.gate2.resp = pfRunDecision()
	if code := w.dispatch(orders[1]); code != 0 {
		t.Fatalf("second fix dispatch exit %d\n%s", code, w.gateLog)
	}
	session2 := runtime.Name(pipefixAgent, pid, runtime.KindPipeline, 920, 1)
	w.answer(session2, fixBatch, "    return 0", rtOtherLine)
	w.sweep()
	mrs := w.fixMRsOf()
	if len(mrs) != 2 {
		t.Fatalf("%d fix merge requests, want the merged first and the follow-up\n%s", len(mrs), w.gateLog)
	}
	second := mrs[1]
	branch2, _ := pipelinectx.FixBranch(fixAnchor2)
	if second.SourceBranch != branch2 || second.SourceBranch == first.SourceBranch || second.TargetBranch != targetBranch || second.State != "opened" ||
		w.fixMRsOf()[0].State != "merged" {
		t.Fatalf("the follow-up: branch %q (want %q) target %q state %q; the first is %q", second.SourceBranch, branch2,
			second.TargetBranch, second.State, w.fixMRsOf()[0].State)
	}
	if a, _, ok := pipelinectx.ParseFixMRMarker(second.Description); !ok || a != fixAnchor2 {
		t.Fatalf("the follow-up's marker names %q, want %q", a, fixAnchor2)
	}
	rec2 := w.record(fixAnchor2)
	if rec2.PipelineFix.State != string(pipefix.StateVerifying) || rec2.PipelineFix.BasePipelineID != 920 || rec2.PipelineFixEpoch != first.IID {
		t.Fatalf("the follow-up's record: fix state %q base pipeline %d epoch %d", rec2.PipelineFix.State, rec2.PipelineFix.BasePipelineID, rec2.PipelineFixEpoch)
	}
	// The first fix is untouched by all of it.
	if got := w.record(fixAnchor); got.PipelineFix.State != string(pipefix.StateConfirmed) {
		t.Fatalf("the first fix is now %q", got.PipelineFix.State)
	}
	requireNoMergeCall(t, w.gl.Writes())
	w.requireNoIssueTriage()
}

// addPipelineOnAt is addPipeline with a creation time: the fix epoch and the
// post-merge confirmation both ask whether a pipeline came AFTER a merge.
func (w *rtWorld) addPipelineOnAt(id int64, sha, status string, created time.Time) {
	w.gl.AddPipeline(w.proj.ID, glab.Pipeline{ID: id, Ref: w.ref, SHA: sha, Status: status, Source: w.source,
		CreatedAt: created, FinishedAt: plTime(int(id % 60))})
	if w.mr {
		w.gl.AddMRPipeline(w.proj.ID, rtMRIID, id)
	}
}
