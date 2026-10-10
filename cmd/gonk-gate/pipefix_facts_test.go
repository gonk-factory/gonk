package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// Fixtures are INVENTED (a project that counts crates into a box, as in
// pkg/pipefix's tests): nothing here is copied from, or resembles, a real
// project's failure.

const (
	pfBaseSHA = "1111111111111111111111111111111111111111"
	pfHeadSHA = "2222222222222222222222222222222222222222"
	pfOldSHA  = "3333333333333333333333333333333333333333"
	pfMRIID   = int64(12)

	pfDiagnosedLine = "count error: expected the box to hold a dozen crates"
	pfOtherLine     = "label failed: the box has no label"
	pfGenericLine   = "ERROR: Job failed: exit code 1"

	pfYAML = `stages: [build, test, package]
compile:
  stage: build
  script: [make crates]
unit-tests:
  stage: test
  script: [make count]
package:
  stage: package
  script: [make box]
`
)

func pfTrace(lines ...string) string {
	return "Running with gitlab-runner\n$ make count\n" + strings.Join(lines, "\n") + "\n"
}

// pfJob is one job object to seed.
type pfJob struct {
	name, stage, status, reason string
	allow                       bool
	trace                       string
}

// pfSpec describes a seeded GitLab: a failed base pipeline, a merge request
// and its pipelines. The zero changes give an honest, complete fix.
type pfSpec struct {
	baseJobs           []pfJob
	headJobs           []pfJob
	headStatus         string
	headSource         string
	mrState            string
	mrSHA              string
	baseLint, headLint glab.CILint
	// oldPipeline adds a pipeline of an EARLIER commit to the merge request.
	oldPipeline bool
	noHead      bool
}

func pfDefault() pfSpec {
	return pfSpec{
		baseJobs: []pfJob{
			{name: "compile", stage: "build", status: "success"},
			{name: "unit-tests", stage: "test", status: "failed", reason: "script_failure", trace: pfTrace(pfDiagnosedLine, pfGenericLine)},
			{name: "package", stage: "package", status: "skipped"},
		},
		headJobs: []pfJob{
			{name: "compile", stage: "build", status: "success"},
			{name: "unit-tests", stage: "test", status: "success"},
			{name: "package", stage: "package", status: "success"},
		},
		headStatus: "success",
		headSource: "merge_request_event",
		mrState:    "opened",
		mrSHA:      pfHeadSHA,
		baseLint:   glab.CILint{Valid: true, MergedYAML: pfYAML},
		headLint:   glab.CILint{Valid: true, MergedYAML: pfYAML},
	}
}

// pfxWorld is a seeded fake GitLab.
type pfxWorld struct {
	gl      *glabtest.Server
	pid     int64
	base    glab.Pipeline
	head    glab.Pipeline
	old     glab.Pipeline
	headIDs map[string][]int64 // job name -> the ids of its attempts, in seeding order
	baseIDs map[string][]int64
}

func pfSeed(t *testing.T, spec pfSpec) *pfxWorld {
	t.Helper()
	gl := glabtest.New(t)
	p := gl.AddProject("crates/box", 40)
	w := &pfxWorld{gl: gl, pid: p.ID, headIDs: map[string][]int64{}, baseIDs: map[string][]int64{}}
	add := func(pl glab.Pipeline, jobs []pfJob, ids map[string][]int64) {
		for _, j := range jobs {
			made := gl.AddJob(p.ID, pl.ID, glab.Job{Name: j.name, Stage: j.stage, Status: j.status, FailureReason: j.reason, AllowFailure: j.allow})
			ids[j.name] = append(ids[j.name], made.ID)
			if j.trace != "" {
				gl.SetTrace(p.ID, made.ID, []byte(j.trace))
			}
		}
	}
	w.base = gl.AddPipeline(p.ID, glab.Pipeline{Status: "failed", Source: "push", Ref: "main", SHA: pfBaseSHA})
	add(w.base, spec.baseJobs, w.baseIDs)
	gl.AddMR(p.ID, glab.MergeRequest{IID: pfMRIID, State: spec.mrState, SHA: spec.mrSHA, SourceBranch: "gonk/pipefix-0123456789ab", TargetBranch: "main"})
	if spec.oldPipeline {
		w.old = gl.AddPipeline(p.ID, glab.Pipeline{Status: "failed", Source: "merge_request_event", Ref: "refs/merge-requests/12/head", SHA: pfOldSHA})
		gl.AddJob(p.ID, w.old.ID, glab.Job{Name: "unit-tests", Stage: "test", Status: "failed", FailureReason: "script_failure"})
		gl.AddMRPipeline(p.ID, pfMRIID, w.old.ID)
	}
	if !spec.noHead {
		w.head = gl.AddPipeline(p.ID, glab.Pipeline{Status: spec.headStatus, Source: spec.headSource, Ref: "refs/merge-requests/12/head", SHA: pfHeadSHA})
		add(w.head, spec.headJobs, w.headIDs)
		gl.AddMRPipeline(p.ID, pfMRIID, w.head.ID)
	}
	gl.SetCILint(p.ID, pfBaseSHA, spec.baseLint)
	gl.SetCILint(p.ID, pfHeadSHA, spec.headLint)
	return w
}

// pfFix is the fix record a dispatch would hold for the seeded world: T is
// unit-tests with the signature of the diagnosed line.
func (w *pfxWorld) fix(t *testing.T, r pipefixReader) pipefix.FixFacts {
	t.Helper()
	base := readPipefixBase(context.Background(), r, w.pid, w.base.ID, []string{"unit-tests"})
	if !base.Read {
		t.Fatalf("the base pipeline was not read: %+v", base.Reads)
	}
	targets, problems := pipefix.TargetSet(base.Recorded, []string{"unit-tests"})
	if len(problems) > 0 {
		t.Fatalf("target problems: %v", problems)
	}
	return pipefix.FixFacts{
		Recorded: true, Targets: targets, BasePipelineID: w.base.ID, BaseJobs: base.Jobs,
		FixBaseSHA: pfBaseSHA, FixCommitSHA: pfHeadSHA, MRIID: pfMRIID,
		ExcerptVersion: pipelinectx.ExcerptVersion, SignatureVersion: pipelinectx.SignatureVersion,
		Lift: []string{pipefix.LiftCI}, CIExecChanges: pipefix.CIExecAuto,
		VerifyDeadline: pfNow.Add(time.Hour),
	}
}

var pfNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func pfJudge(t *testing.T, w *pfxWorld) (pipefix.Result, pipefix.JudgeFacts, pipefixEvidence) {
	t.Helper()
	c := w.gl.Client()
	fix := w.fix(t, c)
	facts, ev := readPipefixJudgeFacts(context.Background(), c, w.pid, fix)
	return pipefix.Judge(facts, pfNow), facts, ev
}

func pfWant(t *testing.T, res pipefix.Result, decision pipefix.Decision, verdict pipefix.Verdict, rule pipefix.Rule) {
	t.Helper()
	if res.Decision != decision || res.Verdict != verdict || res.Rule != rule {
		t.Fatalf("decision %q verdict %q by %q (%s); want %q %q by %q",
			res.Decision, res.Verdict, res.Rule, res.Detail, decision, verdict, rule)
	}
}

func TestPipefixFactsOfAnHonestFix(t *testing.T) {
	w := pfSeed(t, pfDefault())
	res, facts, ev := pfJudge(t, w)
	pfWant(t, res, pipefix.DecisionVerdict, pipefix.VerdictFixed, pipefix.RuleJ5Fixed)

	if want := (pipefix.MRFacts{Read: true, IID: pfMRIID, State: "opened", SHA: pfHeadSHA}); facts.MR != want {
		t.Errorf("MR = %+v, want %+v", facts.MR, want)
	}
	if !facts.Pipelines.Read || len(facts.Pipelines.Pipelines) != 1 {
		t.Fatalf("pipelines = %+v", facts.Pipelines)
	}
	p := facts.Pipelines.Pipelines[0]
	if p.ID != w.head.ID || p.SHA != pfHeadSHA || p.Source != "merge_request_event" || p.Status != "success" ||
		!p.JobsRead || !p.IncludeRetried || len(p.Jobs) != 3 {
		t.Errorf("pipeline = %+v", p)
	}
	for _, a := range p.Jobs {
		if a.ID == 0 || a.Status != "success" || a.Stage == "" || a.Trace.Read {
			t.Errorf("attempt = %+v (a passing attempt needs no trace)", a)
		}
	}
	if !facts.Definition.Read || facts.Definition.Delta.Kind != cidef.KindNone {
		t.Errorf("definition = %+v", facts.Definition)
	}
	if res.Definition != cidef.KindNone {
		t.Errorf("Result.Definition = %q", res.Definition)
	}
	for _, rd := range ev.Reads {
		if !rd.OK || rd.Err != "" {
			t.Errorf("read %+v was not ok", rd)
		}
	}
	if ev.BaseLint == nil || ev.HeadLint == nil || ev.BaseLint.SHA != pfBaseSHA || ev.HeadLint.SHA != pfHeadSHA ||
		!ev.HeadLint.Valid || ev.HeadLint.MergedYAML != pfYAML || ev.HeadLint.MergedYAMLBytes != len(pfYAML) {
		t.Errorf("lint evidence = %+v / %+v", ev.BaseLint, ev.HeadLint)
	}
}

func TestPipefixBaseFacts(t *testing.T) {
	spec := pfDefault()
	// compile was retried on the base pipeline: failed, then passed. Its
	// LATEST attempt is what the base pipeline says of it.
	spec.baseJobs = append([]pfJob{{name: "compile", stage: "build", status: "failed", reason: "runner_system_failure"}}, spec.baseJobs...)
	w := pfSeed(t, spec)
	base := readPipefixBase(context.Background(), w.gl.Client(), w.pid, w.base.ID, []string{"unit-tests"})
	if !base.Read || base.PipelineID != w.base.ID || base.SHA != pfBaseSHA || base.Status != "failed" || base.Source != "push" {
		t.Fatalf("base = %+v", base)
	}
	want := []pipefix.BaseJob{
		{Name: "compile", Status: "success"},
		{Name: "package", Status: "skipped"},
		{Name: "unit-tests", Status: "failed"},
	}
	if !reflect.DeepEqual(base.Jobs, want) {
		t.Errorf("base jobs = %+v, want %+v (one per name, the latest attempt, sorted)", base.Jobs, want)
	}
	wantRec := []pipefix.RecordedJob{{
		Name: "unit-tests", Stage: "test", FailureReason: "script_failure",
		ErrorSignature: pipelinectx.ErrorSignature(pfDiagnosedLine), FactsRecorded: true,
	}}
	if !reflect.DeepEqual(base.Recorded, wantRec) {
		t.Errorf("recorded = %+v, want %+v", base.Recorded, wantRec)
	}
}

func TestPipefixBaseFactsOfAJobWithNoErrorLineOfItsOwnAreGeneric(t *testing.T) {
	spec := pfDefault()
	spec.baseJobs[1].trace = pfTrace(pfGenericLine)
	w := pfSeed(t, spec)
	base := readPipefixBase(context.Background(), w.gl.Client(), w.pid, w.base.ID, []string{"unit-tests"})
	if !base.Read || len(base.Recorded) != 1 || !base.Recorded[0].Generic ||
		base.Recorded[0].ErrorSignature != pipelinectx.ErrorSignature(pfGenericLine) {
		t.Fatalf("recorded = %+v; the runner's own summary is a GENERIC failure", base.Recorded)
	}
}

func TestPipefixBaseTargetThatDidNotFailIsNotRecorded(t *testing.T) {
	w := pfSeed(t, pfDefault())
	// compile passed on the base pipeline: it is no failed job of the run, so
	// there is nothing to record for it and TargetSet names the problem.
	base := readPipefixBase(context.Background(), w.gl.Client(), w.pid, w.base.ID, []string{"compile", "unit-tests"})
	if !base.Read || len(base.Recorded) != 1 || base.Recorded[0].Name != "unit-tests" {
		t.Fatalf("recorded = %+v", base.Recorded)
	}
	if _, problems := pipefix.TargetSet(base.Recorded, []string{"compile", "unit-tests"}); len(problems) != 1 {
		t.Fatalf("problems = %v, want the one about compile", problems)
	}
}

// TestAFailedReadIsNotAFact: each read, failing ALONE, leaves ITS group "not
// read" -- the exact zero value -- and Judge gives no verdict. Named break
// (section 11, part PROBE): "a failed read becomes a zero-value fact".
func TestAFailedReadIsNotAFact(t *testing.T) {
	recurs := func(s *pfSpec) {
		s.headStatus = "failed"
		s.headJobs[1] = pfJob{name: "unit-tests", stage: "test", status: "failed", reason: "script_failure", trace: pfTrace(pfDiagnosedLine, pfGenericLine)}
		s.headJobs[2].status = "skipped"
	}
	for _, tc := range []struct {
		name string
		spec func(*pfSpec)
		// fail: the path prefix (after /api/v4/projects/<id>) that answers 403.
		fail  func(w *pfxWorld) string
		check func(t *testing.T, w *pfxWorld, f pipefix.JudgeFacts)
		rule  pipefix.Rule
	}{
		{name: "the merge request", fail: func(w *pfxWorld) string { return "/merge_requests/12" },
			check: func(t *testing.T, w *pfxWorld, f pipefix.JudgeFacts) {
				if f.MR != (pipefix.MRFacts{}) {
					t.Errorf("MR = %+v, want the zero value", f.MR)
				}
			}, rule: pipefix.RuleFacts},
		{name: "the merge request's pipelines", fail: func(w *pfxWorld) string { return "/merge_requests/12/pipelines" },
			check: func(t *testing.T, w *pfxWorld, f pipefix.JudgeFacts) {
				if !reflect.DeepEqual(f.Pipelines, pipefix.PipelineFacts{}) {
					t.Errorf("pipelines = %+v, want the zero value", f.Pipelines)
				}
				if !f.MR.Read || !f.Definition.Read {
					t.Errorf("one failed read took another group with it: mr %+v definition read %v", f.MR, f.Definition.Read)
				}
			}, rule: pipefix.RuleFacts},
		{name: "a pipeline's jobs", fail: func(w *pfxWorld) string { return fmt.Sprintf("/pipelines/%d/jobs", w.head.ID) },
			check: func(t *testing.T, w *pfxWorld, f pipefix.JudgeFacts) {
				if !f.Pipelines.Read || len(f.Pipelines.Pipelines) != 1 {
					t.Fatalf("pipelines = %+v: the LIST was read", f.Pipelines)
				}
				p := f.Pipelines.Pipelines[0]
				if p.JobsRead || p.IncludeRetried || p.Jobs != nil {
					t.Errorf("pipeline = %+v, want its jobs unread", p)
				}
			}, rule: pipefix.RuleFacts},
		{name: "a failed attempt's log", spec: recurs, fail: func(w *pfxWorld) string { return fmt.Sprintf("/jobs/%d/trace", w.headIDs["unit-tests"][0]) },
			check: func(t *testing.T, w *pfxWorld, f pipefix.JudgeFacts) {
				for _, a := range f.Pipelines.Pipelines[0].Jobs {
					if a.Name == "unit-tests" && a.Trace != (pipefix.Trace{}) {
						t.Errorf("trace = %+v, want the zero value (an unread log is not an empty log)", a.Trace)
					}
				}
			}, rule: pipefix.RuleJ3Unknown},
		{name: "the CI definition", fail: func(w *pfxWorld) string { return "/ci/lint" },
			check: func(t *testing.T, w *pfxWorld, f pipefix.JudgeFacts) {
				if !reflect.DeepEqual(f.Definition, pipefix.DefinitionFacts{}) {
					t.Errorf("definition = %+v, want the zero value (an unread definition is not `none`)", f.Definition)
				}
			}, rule: pipefix.RuleFacts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := pfDefault()
			if tc.spec != nil {
				tc.spec(&spec)
			}
			w := pfSeed(t, spec)
			c := w.gl.Client()
			fix := w.fix(t, c)
			w.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d%s", w.pid, tc.fail(w)), http.StatusForbidden)
			facts, ev := readPipefixJudgeFacts(context.Background(), c, w.pid, fix)
			tc.check(t, w, facts)
			res := pipefix.Judge(facts, pfNow)
			pfWant(t, res, pipefix.DecisionCannotJudge, "", tc.rule)
			failed := 0
			for _, rd := range ev.Reads {
				if !rd.OK {
					failed++
					if !strings.Contains(rd.Err, "403") {
						t.Errorf("read %+v does not say why it failed", rd)
					}
				}
			}
			if failed == 0 {
				t.Errorf("no read is reported as failed: %+v", ev.Reads)
			}
		})
	}
}

// TestAFailedBaseReadIsNotAFact: the same rule for the base pipeline.
func TestAFailedBaseReadIsNotAFact(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(w *pfxWorld) string
	}{
		{"the base pipeline", func(w *pfxWorld) string { return fmt.Sprintf("/pipelines/%d", w.base.ID) }},
		{"the base pipeline's jobs", func(w *pfxWorld) string { return fmt.Sprintf("/pipelines/%d/jobs", w.base.ID) }},
		{"the diagnosed job's log", func(w *pfxWorld) string { return fmt.Sprintf("/jobs/%d/trace", w.baseIDs["unit-tests"][0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := pfSeed(t, pfDefault())
			w.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d%s", w.pid, tc.fail(w)), http.StatusForbidden)
			base := readPipefixBase(context.Background(), w.gl.Client(), w.pid, w.base.ID, []string{"unit-tests"})
			if base.Read || base.Jobs != nil || base.Recorded != nil {
				t.Fatalf("base = %+v, want nothing read", base)
			}
			failed := false
			for _, rd := range base.Reads {
				failed = failed || (!rd.OK && strings.Contains(rd.Err, "403"))
			}
			if !failed {
				t.Errorf("no read is reported as failed: %+v", base.Reads)
			}
		})
	}
}

// TestEveryAttemptIsReadNotOnlyTheLatest: a diagnosed job that failed the
// diagnosed way and was then RETRIED to green is two job objects, and both are
// evidence. Named break: "the latest attempt only is read".
func TestEveryAttemptIsReadNotOnlyTheLatest(t *testing.T) {
	spec := pfDefault()
	spec.headJobs = []pfJob{
		{name: "compile", stage: "build", status: "success"},
		{name: "unit-tests", stage: "test", status: "failed", reason: "script_failure", trace: pfTrace(pfDiagnosedLine, pfGenericLine)},
		{name: "unit-tests", stage: "test", status: "success"},
		{name: "package", stage: "package", status: "success"},
	}
	w := pfSeed(t, spec)
	res, facts, _ := pfJudge(t, w)
	attempts := 0
	for _, a := range facts.Pipelines.Pipelines[0].Jobs {
		if a.Name == "unit-tests" {
			attempts++
		}
	}
	if attempts != 2 {
		t.Errorf("%d attempt(s) of unit-tests were read, want both", attempts)
	}
	pfWant(t, res, pipefix.DecisionVerdict, pipefix.VerdictFlaky, pipefix.RuleJ3FlakySame)
}

// pfStub is a pipefixReader that answers from fields: what a client would
// hand back if GitLab (or a proxy) answered 200 with something else.
type pfStub struct {
	inner     pipefixReader
	mr        func() (*glab.MergeRequest, error)
	pipelines func() ([]glab.Pipeline, error)
	pipeline  func(id int64) (*glab.Pipeline, error)
	attempts  func(id int64) ([]glab.Job, error)
	trace     func(id int64) (*glab.TraceTail, error)
	lint      func(sha string) (*glab.CILint, error)
}

func (s pfStub) GetMergeRequest(ctx context.Context, p, iid int64) (*glab.MergeRequest, error) {
	if s.mr != nil {
		return s.mr()
	}
	return s.inner.GetMergeRequest(ctx, p, iid)
}
func (s pfStub) ListMRPipelines(ctx context.Context, p, iid int64) ([]glab.Pipeline, error) {
	if s.pipelines != nil {
		return s.pipelines()
	}
	return s.inner.ListMRPipelines(ctx, p, iid)
}
func (s pfStub) GetPipeline(ctx context.Context, p, id int64) (*glab.Pipeline, error) {
	if s.pipeline != nil {
		return s.pipeline(id)
	}
	return s.inner.GetPipeline(ctx, p, id)
}
func (s pfStub) ListPipelineAttempts(ctx context.Context, p, id int64) ([]glab.Job, error) {
	if s.attempts != nil {
		return s.attempts(id)
	}
	return s.inner.ListPipelineAttempts(ctx, p, id)
}
func (s pfStub) GetJobTraceTail(ctx context.Context, p, id, maxRead, tail int64) (*glab.TraceTail, error) {
	if s.trace != nil {
		return s.trace(id)
	}
	return s.inner.GetJobTraceTail(ctx, p, id, maxRead, tail)
}
func (s pfStub) CILintStatic(ctx context.Context, p int64, sha string) (*glab.CILint, error) {
	if s.lint != nil {
		return s.lint(sha)
	}
	return s.inner.CILintStatic(ctx, p, sha)
}

// TestWhatIsNotAPipelineIsNotRead: a list entry that parses but is not a
// pipeline of this project -- GitLab's `{"message":"404 Not found"}` decodes
// into a Pipeline with every field zero -- leaves the pipelines UNREAD. Named
// break: "a 404 JSON body accepted as a pipeline".
func TestWhatIsNotAPipelineIsNotRead(t *testing.T) {
	good := func(w *pfxWorld) glab.Pipeline {
		return glab.Pipeline{ID: w.head.ID, ProjectID: w.pid, SHA: pfHeadSHA, Source: "merge_request_event", Status: "success"}
	}
	for name, breakIt := range map[string]func(p *glab.Pipeline){
		`an error object ({"message":"404 Not found"})`: func(p *glab.Pipeline) { *p = glab.Pipeline{} },
		"no id":                      func(p *glab.Pipeline) { p.ID = 0 },
		"no sha":                     func(p *glab.Pipeline) { p.SHA = "" },
		"a sha that is not a commit": func(p *glab.Pipeline) { p.SHA = "2222222" },
		"no source":                  func(p *glab.Pipeline) { p.Source = "" },
		"no status":                  func(p *glab.Pipeline) { p.Status = "" },
		"another project's pipeline": func(p *glab.Pipeline) { p.ProjectID++ },
	} {
		t.Run(name, func(t *testing.T) {
			w := pfSeed(t, pfDefault())
			c := w.gl.Client()
			fix := w.fix(t, c)
			bad := good(w)
			breakIt(&bad)
			// A good entry FIRST: one bad entry spoils the list, wherever it is.
			stub := pfStub{inner: c, pipelines: func() ([]glab.Pipeline, error) { return []glab.Pipeline{good(w), bad}, nil }}
			facts, ev := readPipefixJudgeFacts(context.Background(), stub, w.pid, fix)
			if !reflect.DeepEqual(facts.Pipelines, pipefix.PipelineFacts{}) {
				t.Errorf("pipelines = %+v, want unread", facts.Pipelines)
			}
			pfWant(t, pipefix.Judge(facts, pfNow), pipefix.DecisionCannotJudge, "", pipefix.RuleFacts)
			named := false
			for _, rd := range ev.Reads {
				named = named || (rd.What == "mr-pipelines" && !rd.OK && rd.Err != "")
			}
			if !named {
				t.Errorf("the refused list is not reported: %+v", ev.Reads)
			}
		})
	}
}

// TestA200ErrorBodyIsNeverAFact: the real client against a server that answers
// EVERYTHING with 200 and GitLab's error object, or a list holding one. Every
// group must come back unread.
func TestA200ErrorBodyIsNeverAFact(t *testing.T) {
	for name, body := range map[string]func(path string) string{
		"an error object": func(string) string { return `{"message":"404 Not found"}` },
		"a list of one error object": func(path string) string {
			if strings.HasSuffix(path, "/pipelines") || strings.HasSuffix(path, "/jobs") {
				return `[{"message":"404 Not found"}]`
			}
			return `{"message":"404 Not found"}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body(r.URL.Path)))
			}))
			defer srv.Close()
			c := glab.New(srv.URL, "t")
			c.RetryBackoff = func(int) time.Duration { return 0 }
			base := readPipefixBase(context.Background(), c, 9, 500, []string{"unit-tests"})
			if base.Read || base.Jobs != nil || base.Recorded != nil || base.SHA != "" {
				t.Errorf("base = %+v, want unread", base)
			}
			fix := pipefix.FixFacts{Recorded: true, FixBaseSHA: pfBaseSHA, FixCommitSHA: pfHeadSHA, MRIID: pfMRIID}
			facts, _ := readPipefixJudgeFacts(context.Background(), c, 9, fix)
			if facts.MR != (pipefix.MRFacts{}) || !reflect.DeepEqual(facts.Pipelines, pipefix.PipelineFacts{}) ||
				!reflect.DeepEqual(facts.Definition, pipefix.DefinitionFacts{}) {
				t.Errorf("facts = %+v, want every group unread", facts)
			}
		})
	}
}

func TestWhatIsNotTheBasePipelineIsNotRead(t *testing.T) {
	for name, breakIt := range map[string]func(p *glab.Pipeline){
		"an error object":            func(p *glab.Pipeline) { *p = glab.Pipeline{} },
		"another pipeline":           func(p *glab.Pipeline) { p.ID++ },
		"a sha that is not a commit": func(p *glab.Pipeline) { p.SHA = "main" },
		"no status":                  func(p *glab.Pipeline) { p.Status = "" },
		"another project's":          func(p *glab.Pipeline) { p.ProjectID++ },
	} {
		t.Run(name, func(t *testing.T) {
			w := pfSeed(t, pfDefault())
			c := w.gl.Client()
			stub := pfStub{inner: c, pipeline: func(id int64) (*glab.Pipeline, error) {
				p := glab.Pipeline{ID: id, ProjectID: w.pid, SHA: pfBaseSHA, Status: "failed", Source: "push"}
				breakIt(&p)
				return &p, nil
			}}
			if base := readPipefixBase(context.Background(), stub, w.pid, w.base.ID, []string{"unit-tests"}); base.Read || base.Jobs != nil {
				t.Fatalf("base = %+v, want unread", base)
			}
		})
	}
}

func TestWhatIsNotTheMergeRequestIsNotRead(t *testing.T) {
	for name, mr := range map[string]*glab.MergeRequest{
		"an error object": {},
		"another MR":      {IID: pfMRIID + 1, State: "opened", SHA: pfHeadSHA},
		"no state":        {IID: pfMRIID, SHA: pfHeadSHA},
		"nil":             nil,
	} {
		t.Run(name, func(t *testing.T) {
			w := pfSeed(t, pfDefault())
			stub := pfStub{inner: w.gl.Client(), mr: func() (*glab.MergeRequest, error) { return mr, nil }}
			facts, rd := readPipefixMR(context.Background(), stub, w.pid, pfMRIID)
			if facts != (pipefix.MRFacts{}) || rd.OK || rd.Err == "" {
				t.Fatalf("facts %+v read %+v, want unread and a reason", facts, rd)
			}
		})
	}
}

// TestJobNamesAreCanonicalisedLikeTheRecord: FACTS deviation 5. The record
// holds jobNameText(name); a live name compared raw would read as ABSENT.
func TestJobNamesAreCanonicalisedLikeTheRecord(t *testing.T) {
	const raw = "unit   tests\t(crates)"
	canon := jobNameText(raw)
	if canon == raw {
		t.Fatalf("the fixture name %q is already canonical; it proves nothing", raw)
	}
	spec := pfDefault()
	spec.baseJobs[1].name = raw
	spec.headJobs[1].name = raw
	w := pfSeed(t, spec)
	c := w.gl.Client()
	// The target is named as the record names it; given raw it must match too.
	for _, target := range []string{canon, raw} {
		base := readPipefixBase(context.Background(), c, w.pid, w.base.ID, []string{target})
		if !base.Read || len(base.Recorded) != 1 || base.Recorded[0].Name != canon {
			t.Fatalf("target %q: recorded = %+v, want the canonical name %q", target, base.Recorded, canon)
		}
		found := false
		for _, b := range base.Jobs {
			found = found || b.Name == canon
			if b.Name == raw {
				t.Errorf("a base job carries the RAW name %q", raw)
			}
		}
		if !found {
			t.Errorf("no base job is named %q: %+v", canon, base.Jobs)
		}
		targets, problems := pipefix.TargetSet(base.Recorded, []string{canon})
		if len(problems) > 0 {
			t.Fatalf("problems: %v", problems)
		}
		fix := pipefix.FixFacts{
			Recorded: true, Targets: targets, BasePipelineID: w.base.ID, BaseJobs: base.Jobs,
			FixBaseSHA: pfBaseSHA, FixCommitSHA: pfHeadSHA, MRIID: pfMRIID,
			ExcerptVersion: pipelinectx.ExcerptVersion, SignatureVersion: pipelinectx.SignatureVersion,
			CIExecChanges: pipefix.CIExecDeny, VerifyDeadline: pfNow.Add(time.Hour),
		}
		facts, _ := readPipefixJudgeFacts(context.Background(), c, w.pid, fix)
		for _, a := range facts.Pipelines.Pipelines[0].Jobs {
			if a.Name == raw {
				t.Errorf("an attempt carries the RAW name %q", raw)
			}
		}
		res := pipefix.Judge(facts, pfNow)
		// The YAML fixture does not define this job, so the definition is not
		// what is under test here; the job finding is.
		if len(res.Jobs) != 1 || res.Jobs[0].Rule != pipefix.RuleJ3Passed {
			t.Fatalf("target %q: job findings %+v (%s); want the job seen PASSED, not absent", target, res.Jobs, res.Detail)
		}
	}
}

// TestDefinitionDelta: the delta is cidef.Diff(BASE, HEAD, T) over the STATIC
// lint of each commit, plus what only the reader can know: an include whose
// content is not pinned to the commit (plan 6.2).
func TestDefinitionDelta(t *testing.T) {
	yaml := func(old, new string) string {
		if !strings.Contains(pfYAML, old) {
			t.Fatalf("fixture has no %q", old)
		}
		return strings.Replace(pfYAML, old, new, 1)
	}
	inc := func(typ, loc, sha string) glab.CILintInclude {
		return glab.CILintInclude{Type: typ, Location: loc, ContextProject: "crates/box", ContextSHA: sha}
	}
	for _, tc := range []struct {
		name       string
		base, head glab.CILint
		kind       cidef.Kind
		// violation: "<rule> <job>/<key>" of the first violation, "" for none.
		violation string
		// unresolvable: the sides of the unresolvable findings, in order.
		unresolvable []cidef.Side
		verdict      pipefix.Verdict
		rule         pipefix.Rule
	}{
		{name: "nothing changed", kind: cidef.KindNone, verdict: pipefix.VerdictFixed, rule: pipefix.RuleJ5Fixed},
		{name: "the head lets the diagnosed job fail (GATING, D2)",
			head: glab.CILint{Valid: true, MergedYAML: yaml("  script: [make count]\n", "  script: [make count]\n  allow_failure: true\n")},
			kind: cidef.KindViolation, violation: "D2 unit-tests/allow_failure", verdict: pipefix.VerdictForged, rule: pipefix.RuleJ2Violation},
		{name: "the head removes a job (D1) -- base and head are not swapped",
			head: glab.CILint{Valid: true, MergedYAML: yaml("package:\n  stage: package\n  script: [make box]\n", "")},
			kind: cidef.KindViolation, violation: "D1 package/", verdict: pipefix.VerdictForged, rule: pipefix.RuleJ2Violation},
		{name: "the head adds a job (free)",
			head: glab.CILint{Valid: true, MergedYAML: pfYAML + "label:\n  stage: package\n  script: [make label]\n"},
			kind: cidef.KindPreserving, verdict: pipefix.VerdictFixed, rule: pipefix.RuleJ5Fixed},
		{name: "the head changes what the DIAGNOSED job runs (exec-changing; T is passed)",
			head: glab.CILint{Valid: true, MergedYAML: yaml("script: [make count]", "script: [make count || true]")},
			kind: cidef.KindExecChanging, verdict: pipefix.VerdictFixed, rule: pipefix.RuleJ5Fixed},
		{name: "the head changes what ANOTHER job runs (D3)",
			head: glab.CILint{Valid: true, MergedYAML: yaml("script: [make crates]", "script: [make crates, make more]")},
			kind: cidef.KindViolation, violation: "D3 compile/script", verdict: pipefix.VerdictForged, rule: pipefix.RuleJ2Violation},
		{name: "local includes pinned to each commit are comparable",
			base: glab.CILint{Valid: true, MergedYAML: pfYAML, Includes: []glab.CILintInclude{inc("local", "ci/common.yml", pfBaseSHA)}},
			head: glab.CILint{Valid: true, MergedYAML: pfYAML, Includes: []glab.CILintInclude{inc("local", "ci/common.yml", pfHeadSHA)}},
			kind: cidef.KindNone, verdict: pipefix.VerdictFixed, rule: pipefix.RuleJ5Fixed},
		{name: "the head adds a remote include: only the head cannot be pinned",
			head: glab.CILint{Valid: true, MergedYAML: pfYAML, Includes: []glab.CILintInclude{inc("remote", "https://example.invalid/ci.yml", pfHeadSHA)}},
			kind: cidef.KindUnresolvable, unresolvable: []cidef.Side{cidef.SideHead}, verdict: pipefix.VerdictForged, rule: pipefix.RuleJ2HeadUnresolvable},
		{name: "both commits use the same template include: neither can be pinned",
			base: glab.CILint{Valid: true, MergedYAML: pfYAML, Includes: []glab.CILintInclude{inc("template", "Jobs/Build.gitlab-ci.yml", pfBaseSHA)}},
			head: glab.CILint{Valid: true, MergedYAML: pfYAML, Includes: []glab.CILintInclude{inc("template", "Jobs/Build.gitlab-ci.yml", pfHeadSHA)}},
			kind: cidef.KindUnresolvable, unresolvable: []cidef.Side{cidef.SideBoth}, verdict: pipefix.VerdictUnverifiable, rule: pipefix.RuleJ2Unresolvable},
		{name: "only the base has an include of another project",
			base: glab.CILint{Valid: true, MergedYAML: pfYAML, Includes: []glab.CILintInclude{inc("file", "ci/shared.yml", pfOldSHA)}},
			kind: cidef.KindUnresolvable, unresolvable: []cidef.Side{cidef.SideBase}, verdict: pipefix.VerdictUnverifiable, rule: pipefix.RuleJ2Unresolvable},
		{name: "a violation outranks an include that cannot be pinned",
			base: glab.CILint{Valid: true, MergedYAML: pfYAML, Includes: []glab.CILintInclude{inc("template", "Jobs/Build.gitlab-ci.yml", pfBaseSHA)}},
			head: glab.CILint{Valid: true, MergedYAML: yaml("  script: [make count]\n", "  script: [make count]\n  allow_failure: true\n"),
				Includes: []glab.CILintInclude{inc("template", "Jobs/Build.gitlab-ci.yml", pfHeadSHA)}},
			kind: cidef.KindViolation, violation: "D2 unit-tests/allow_failure", unresolvable: []cidef.Side{cidef.SideBoth},
			verdict: pipefix.VerdictForged, rule: pipefix.RuleJ2Violation},
		{name: "the head's configuration is INVALID: GitLab answered, and there is no definition to compare",
			head: glab.CILint{Valid: false, Errors: []string{"jobs:unit-tests script can't be blank"}},
			kind: cidef.KindUnresolvable, unresolvable: []cidef.Side{cidef.SideHead}, verdict: pipefix.VerdictForged, rule: pipefix.RuleJ2HeadUnresolvable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := pfDefault()
			if tc.base.Valid || len(tc.base.Errors) > 0 {
				spec.baseLint = tc.base
			}
			if tc.head.Valid || len(tc.head.Errors) > 0 {
				spec.headLint = tc.head
			}
			w := pfSeed(t, spec)
			// What a DRY RUN would answer for the head: the base's definition
			// (local includes resolved on the default branch). A reader that
			// asked for a dry run would see no change at all.
			w.gl.SetCILintDryRun(w.pid, pfHeadSHA, spec.baseLint)
			res, facts, _ := pfJudge(t, w)
			d := facts.Definition.Delta
			if !facts.Definition.Read || d.Kind != tc.kind {
				t.Fatalf("definition read=%v kind=%q (%+v), want %q", facts.Definition.Read, d.Kind, d, tc.kind)
			}
			got := ""
			if len(d.Violations) > 0 {
				got = fmt.Sprintf("%s %s/%s", d.Violations[0].Rule, d.Violations[0].Job, d.Violations[0].Key)
			}
			if got != tc.violation {
				t.Errorf("first violation = %q, want %q (all: %+v)", got, tc.violation, d.Violations)
			}
			var sides []cidef.Side
			for _, u := range d.Unresolvable {
				sides = append(sides, u.Side)
				if u.Reason == "" {
					t.Errorf("an unresolvable finding without a reason: %+v", u)
				}
			}
			if !reflect.DeepEqual(sides, tc.unresolvable) {
				t.Errorf("unresolvable sides = %v, want %v (%+v)", sides, tc.unresolvable, d.Unresolvable)
			}
			pfWant(t, res, pipefix.DecisionVerdict, tc.verdict, tc.rule)
		})
	}
}

// TestOnlyTheFixCommitsPipelinesHaveTheirJobsRead: an earlier push's pipeline
// is listed (Judge picks the candidates itself) but costs no job read.
func TestOnlyTheFixCommitsPipelinesHaveTheirJobsRead(t *testing.T) {
	spec := pfDefault()
	spec.oldPipeline = true
	w := pfSeed(t, spec)
	res, facts, _ := pfJudge(t, w)
	pfWant(t, res, pipefix.DecisionVerdict, pipefix.VerdictFixed, pipefix.RuleJ5Fixed)
	if len(facts.Pipelines.Pipelines) != 2 {
		t.Fatalf("pipelines = %+v, want both listed", facts.Pipelines.Pipelines)
	}
	for _, p := range facts.Pipelines.Pipelines {
		if p.ID == w.old.ID && (p.JobsRead || p.Jobs != nil) {
			t.Errorf("the earlier commit's pipeline had its jobs read: %+v", p)
		}
	}
	for _, rq := range w.gl.Requests() {
		if strings.Contains(rq, fmt.Sprintf("/pipelines/%d/jobs", w.old.ID)) {
			t.Errorf("request %q: the earlier commit's jobs were fetched", rq)
		}
	}
}

// TestAChildPipelineIsListedButNotRead: J1 excludes source parent_pipeline
// from the candidates, so its jobs are no evidence and cost no read.
func TestAChildPipelineIsListedButNotRead(t *testing.T) {
	w := pfSeed(t, pfDefault())
	child := w.gl.AddPipeline(w.pid, glab.Pipeline{Status: "failed", Source: pipefix.SourceParentPipeline, Ref: "refs/merge-requests/12/head", SHA: pfHeadSHA})
	w.gl.AddJob(w.pid, child.ID, glab.Job{Name: "unit-tests", Stage: "test", Status: "failed", FailureReason: "script_failure"})
	w.gl.AddMRPipeline(w.pid, pfMRIID, child.ID)
	res, facts, _ := pfJudge(t, w)
	pfWant(t, res, pipefix.DecisionVerdict, pipefix.VerdictFixed, pipefix.RuleJ5Fixed)
	listed := false
	for _, p := range facts.Pipelines.Pipelines {
		if p.ID == child.ID {
			listed = true
			if p.JobsRead || p.Jobs != nil {
				t.Errorf("the child pipeline had its jobs read: %+v", p)
			}
		}
	}
	if !listed {
		t.Errorf("the child pipeline is not in the facts: Judge, not the reader, drops it")
	}
}

// TestOnlyFailedAttemptsOfDiagnosedJobsHaveTheirLogRead pins the cost and the
// rule: a log is a fact only for a FAILED attempt of a job in T.
func TestOnlyFailedAttemptsOfDiagnosedJobsHaveTheirLogRead(t *testing.T) {
	spec := pfDefault()
	spec.headStatus = "failed"
	spec.headJobs = []pfJob{
		{name: "compile", stage: "build", status: "failed", reason: "script_failure", trace: pfTrace("compile error: no crates")},
		{name: "unit-tests", stage: "test", status: "failed", reason: "script_failure", trace: pfTrace(pfOtherLine, pfGenericLine)},
		{name: "unit-tests", stage: "test", status: "success", trace: pfTrace("ok")},
	}
	w := pfSeed(t, spec)
	c := w.gl.Client()
	fix := w.fix(t, c)
	before := len(w.gl.Requests())
	facts, _ := readPipefixJudgeFacts(context.Background(), c, w.pid, fix)
	var traces []string
	for _, rq := range w.gl.Requests()[before:] {
		if strings.HasSuffix(rq, "/trace") {
			traces = append(traces, rq)
		}
	}
	want := fmt.Sprintf("GET /api/v4/projects/%d/jobs/%d/trace", w.pid, w.headIDs["unit-tests"][0])
	if len(traces) != 1 || traces[0] != want {
		t.Errorf("trace reads = %v, want exactly [%s]", traces, want)
	}
	for _, a := range facts.Pipelines.Pipelines[0].Jobs {
		failedT := a.Name == "unit-tests" && a.Status == "failed"
		if a.Trace.Read != failedT {
			t.Errorf("attempt %+v: trace read = %v, want %v", a, a.Trace.Read, failedT)
		}
		if failedT && a.Trace.FirstError != pfOtherLine {
			t.Errorf("first error = %q, want %q", a.Trace.FirstError, pfOtherLine)
		}
	}
	res := pipefix.Judge(facts, pfNow)
	pfWant(t, res, pipefix.DecisionVerdict, pipefix.VerdictFlaky, pipefix.RuleJ3FlakyChanged)
}

// TestATraceThatIsNotTheWholeEndOfTheLogIsHandled: a log GitLab cut short is
// not read; a log whose START was dropped loses its first, partial line.
func TestATraceThatIsNotTheWholeEndOfTheLogIsHandled(t *testing.T) {
	spec := pfDefault()
	spec.headStatus = "failed"
	spec.headJobs[1] = pfJob{name: "unit-tests", stage: "test", status: "failed", reason: "script_failure"}
	for _, tc := range []struct {
		name string
		tail glab.TraceTail
		want pipefix.Trace
	}{
		{"whole", glab.TraceTail{Data: []byte(pfTrace(pfDiagnosedLine))}, pipefix.Trace{Read: true, FirstError: pfDiagnosedLine}},
		{"whole, and its first line is the error", glab.TraceTail{Data: []byte(pfDiagnosedLine + "\nmore\n")}, pipefix.Trace{Read: true, FirstError: pfDiagnosedLine}},
		{"the read stopped before the end of the log", glab.TraceTail{Data: []byte(pfTrace(pfDiagnosedLine)), TailTruncated: true}, pipefix.Trace{}},
		{"the start of the log was dropped: the first line is a fragment",
			glab.TraceTail{Data: []byte("error: the cut end of an earlier line\n" + pfDiagnosedLine + "\n"), HeadTruncated: true},
			pipefix.Trace{Read: true, FirstError: pfDiagnosedLine}},
		{"the start of the log was dropped and only the cut line was read: nothing is established",
			glab.TraceTail{Data: []byte("error: the cut end of an earlier line"), HeadTruncated: true}, pipefix.Trace{}},
		{"both ends are missing", glab.TraceTail{Data: []byte("cut\n" + pfDiagnosedLine + "\n"), HeadTruncated: true, TailTruncated: true}, pipefix.Trace{}},
		{"an empty log is a log that was read", glab.TraceTail{}, pipefix.Trace{Read: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := pfSeed(t, spec)
			c := w.gl.Client()
			fix := w.fix(t, c)
			tail := tc.tail
			stub := pfStub{inner: c, trace: func(int64) (*glab.TraceTail, error) { return &tail, nil }}
			facts, ev := readPipefixJudgeFacts(context.Background(), stub, w.pid, fix)
			seen := false
			for _, a := range facts.Pipelines.Pipelines[0].Jobs {
				if a.Name != "unit-tests" {
					continue
				}
				seen = true
				if a.Trace != tc.want {
					t.Errorf("trace = %+v, want %+v", a.Trace, tc.want)
				}
				// The reader adds nothing to pipefix.FailedTrace's rule: the
				// fact is exactly what that function makes of the read.
				if direct := pipefix.FailedTrace(pipefix.LogRead{Data: tail.Data, HeadTruncated: tail.HeadTruncated, TailTruncated: tail.TailTruncated}); a.Trace != direct {
					t.Errorf("trace = %+v, pipefix.FailedTrace of the same read = %+v", a.Trace, direct)
				}
			}
			if !seen {
				t.Fatalf("no attempt of unit-tests in the facts")
			}
			// A log that was not read is REPORTED as a failed read, saying why.
			var traces []pipefixRead
			for _, rd := range ev.Reads {
				if rd.What == "trace" {
					traces = append(traces, rd)
				}
			}
			if len(traces) != 1 || traces[0].OK != tc.want.Read || (!tc.want.Read && !strings.Contains(traces[0].Err, "not read whole")) {
				t.Errorf("trace reads = %+v, want one with OK=%v", traces, tc.want.Read)
			}
		})
	}
	t.Run("nil with no error", func(t *testing.T) {
		w := pfSeed(t, spec)
		c := w.gl.Client()
		fix := w.fix(t, c)
		stub := pfStub{inner: c, trace: func(int64) (*glab.TraceTail, error) { return nil, nil }}
		facts, _ := readPipefixJudgeFacts(context.Background(), stub, w.pid, fix)
		for _, a := range facts.Pipelines.Pipelines[0].Jobs {
			if a.Trace.Read {
				t.Errorf("trace = %+v, want unread", a.Trace)
			}
		}
	})
}

func TestWhatIsNotAJobOfThePipelineLeavesItsJobsUnread(t *testing.T) {
	for name, job := range map[string]glab.Job{
		"an error object": {},
		"no status":       {ID: 5, Name: "unit-tests"},
		"no name":         {ID: 5, Status: "success"},
	} {
		t.Run(name, func(t *testing.T) {
			w := pfSeed(t, pfDefault())
			c := w.gl.Client()
			fix := w.fix(t, c)
			stub := pfStub{inner: c, attempts: func(int64) ([]glab.Job, error) {
				return []glab.Job{{ID: 4, Name: "compile", Status: "success"}, job}, nil
			}}
			facts, _ := readPipefixJudgeFacts(context.Background(), stub, w.pid, fix)
			p := facts.Pipelines.Pipelines[0]
			if p.JobsRead || p.IncludeRetried || p.Jobs != nil {
				t.Fatalf("pipeline = %+v, want its jobs unread", p)
			}
		})
	}
}

func TestAReadErrorIsCarriedAsText(t *testing.T) {
	w := pfSeed(t, pfDefault())
	stub := pfStub{inner: w.gl.Client(), mr: func() (*glab.MergeRequest, error) { return nil, errors.New("dial tcp: connection refused") }}
	_, rd := readPipefixMR(context.Background(), stub, w.pid, pfMRIID)
	if rd.OK || rd.What != "mr" || rd.Ref != "!12" || !strings.Contains(rd.Err, "connection refused") {
		t.Fatalf("read = %+v", rd)
	}
}

// TestPipefixReaderIsGETOnly: the interface is the read-only construction. A
// method added to it must be added to pipefixReaderMethods -- by someone who
// has checked that it sends only GETs.
func TestPipefixReaderIsGETOnly(t *testing.T) {
	typ := reflect.TypeOf((*pipefixReader)(nil)).Elem()
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, pipefixReaderMethods) {
		t.Fatalf("pipefixReader's methods are %v; the list known to be GET-only is %v", got, pipefixReaderMethods)
	}
	for _, name := range got {
		for _, verb := range []string{"Create", "Update", "Delete", "Accept", "Add", "Remove", "Set", "Put", "Post", "Merge", "Retry", "Cancel"} {
			if strings.HasPrefix(name, verb) {
				t.Errorf("pipefixReader has %s: that is not a read", name)
			}
		}
	}
}
