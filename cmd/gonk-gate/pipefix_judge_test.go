package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipefix"
)

const pfFakeToken = "glabtest-token"

// pfRun runs the subcommand against the seeded fake THROUGH THE READ-ONLY
// CLIENT the real command builds, and returns the exit code, the decoded
// output, both streams, and the transport (for its refused count).
func pfRun(t *testing.T, w *pfxWorld, extra ...string) (int, pipefixJudgeOutput, string, string, *getOnlyTransport) {
	t.Helper()
	c, tr := readOnlyGitLab(w.gl.URL(), pfFakeToken)
	c.RetryBackoff = func(int) time.Duration { return 0 }
	args := append([]string{
		"--project", fmt.Sprint(w.pid), "--base-pipeline", fmt.Sprint(w.base.ID), "--mr", fmt.Sprint(pfMRIID),
		"--job", "unit-tests",
	}, extra...)
	var stdout, stderr bytes.Buffer
	code := runPipefixJudge(context.Background(), args, c, func() time.Time { return pfNow }, func(string) string { return "" }, &stdout, &stderr)
	var out pipefixJudgeOutput
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("stdout is not the output JSON: %v\n%s", err, stdout.String())
		}
	}
	return code, out, stdout.String(), stderr.String(), tr
}

// pfxScenario is one seeded world and what the exit check must say of it.
type pfxScenario struct {
	name    string
	spec    func(*pfSpec)
	args    []string
	exit    int
	decide  pipefix.Decision
	verdict pipefix.Verdict
	rule    pipefix.Rule
	// jobRule: the J3 finding for unit-tests, "" when not asserted.
	jobRule pipefix.Rule
}

func pfFailWith(line string) pfJob {
	return pfJob{name: "unit-tests", stage: "test", status: "failed", reason: "script_failure", trace: pfTrace(line, pfGenericLine)}
}

func pfScenarios() []pfxScenario {
	red := func(job pfJob) func(*pfSpec) {
		return func(s *pfSpec) {
			s.headStatus = "failed"
			s.headJobs[1] = job
			s.headJobs[2].status = "skipped"
		}
	}
	lifted := []string{"--lift", "ci", "--ci-exec-changes", "auto"}
	return []pfxScenario{
		{name: "an honest fix", exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictFixed, rule: pipefix.RuleJ5Fixed, jobRule: pipefix.RuleJ3Passed},
		{name: "not fixed: the same failure", spec: red(pfFailWith(pfDiagnosedLine)),
			exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictNotFixed, rule: pipefix.RuleJ3Recurs, jobRule: pipefix.RuleJ3Recurs},
		{name: "a different failure in the same job", spec: red(pfFailWith(pfOtherLine)),
			exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictFixedNewFailure, rule: pipefix.RuleJ3Changed, jobRule: pipefix.RuleJ3Changed},
		{name: "C1 the job is gone from the pipeline", spec: func(s *pfSpec) { s.headJobs = []pfJob{s.headJobs[0], s.headJobs[2]} },
			exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictUnverifiable, rule: pipefix.RuleJ3Absent, jobRule: pipefix.RuleJ3Absent},
		{name: "C2 the job object says allow_failure", spec: func(s *pfSpec) {
			s.headJobs[1] = pfJob{name: "unit-tests", stage: "test", status: "failed", reason: "script_failure", allow: true, trace: pfTrace(pfDiagnosedLine)}
		}, exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictForged, rule: pipefix.RuleJ3Weakened, jobRule: pipefix.RuleJ3Weakened},
		{name: "C3 the job is manual and never ran", spec: func(s *pfSpec) { s.headJobs[1].status = "manual" },
			exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictUnverifiable, rule: pipefix.RuleJ3Status, jobRule: pipefix.RuleJ3Status},
		{name: "C5 retried until green", spec: func(s *pfSpec) {
			s.headJobs = []pfJob{s.headJobs[0], pfFailWith(pfDiagnosedLine), s.headJobs[1], s.headJobs[2]}
		}, exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictFlaky, rule: pipefix.RuleJ3FlakySame, jobRule: pipefix.RuleJ3FlakySame},
		{name: "a CI change without the ci lift", spec: func(s *pfSpec) { s.headLint.MergedYAML += "label:\n  stage: package\n  script: [make label]\n" },
			exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictForged, rule: pipefix.RuleJ2Unlifted, jobRule: pipefix.RuleJ3Passed},
		{name: "the same CI change WITH the ci lift", spec: func(s *pfSpec) { s.headLint.MergedYAML += "label:\n  stage: package\n  script: [make label]\n" },
			args: lifted, exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictFixed, rule: pipefix.RuleJ5Fixed, jobRule: pipefix.RuleJ3Passed},
		{name: "C11 an or-true on the diagnosed job, exec changes denied", spec: func(s *pfSpec) {
			s.headLint.MergedYAML = strings.Replace(s.headLint.MergedYAML, "[make count]", "[make count || true]", 1)
		}, args: []string{"--lift", "ci"}, exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictForged, rule: pipefix.RuleJ2ExecDenied},
		{name: "the pipeline is still running", spec: func(s *pfSpec) { s.headStatus = "running"; s.headJobs[1].status = "running" },
			exit: 0, decide: pipefix.DecisionWait, rule: pipefix.RuleJ1Wait},
		{name: "no pipeline yet", spec: func(s *pfSpec) { s.noHead = true }, exit: 0, decide: pipefix.DecisionWait, rule: pipefix.RuleJ1Wait},
		{name: "no pipeline and the deadline has passed", spec: func(s *pfSpec) { s.noHead = true },
			args: []string{"--deadline", "2026-10-05T11:00:00Z"},
			exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictUnverifiable, rule: pipefix.RuleJ1Timeout},
		{name: "the merge request was closed", spec: func(s *pfSpec) { s.mrState = "closed" }, exit: 0, decide: pipefix.DecisionStopped, rule: pipefix.RuleJ0Closed},
		{name: "somebody else pushed: the head is not the fix commit", args: []string{"--fix-commit", pfOldSHA},
			exit: 0, decide: pipefix.DecisionStopped, rule: pipefix.RuleJ0TakenOver},
		{name: "the fix commit given is the head", args: []string{"--fix-commit", pfHeadSHA},
			exit: 0, decide: pipefix.DecisionVerdict, verdict: pipefix.VerdictFixed, rule: pipefix.RuleJ5Fixed},
	}
}

func TestPipefixJudgeDecisions(t *testing.T) {
	for _, sc := range pfScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			spec := pfDefault()
			if sc.spec != nil {
				sc.spec(&spec)
			}
			w := pfSeed(t, spec)
			code, out, _, stderr, _ := pfRun(t, w, sc.args...)
			if code != sc.exit {
				t.Fatalf("exit %d, want %d; stderr: %s", code, sc.exit, stderr)
			}
			pfWant(t, out.Result, sc.decide, sc.verdict, sc.rule)
			if sc.jobRule != "" && (len(out.Result.Jobs) != 1 || out.Result.Jobs[0].Rule != sc.jobRule) {
				t.Errorf("job findings = %+v, want unit-tests by %q", out.Result.Jobs, sc.jobRule)
			}
		})
	}
}

// TestPipefixJudgeWritesNothing: whatever it is asked to judge, the command
// sends only GETs and its client never had to refuse a write. Named break:
// "the subcommand performs a write".
func TestPipefixJudgeWritesNothing(t *testing.T) {
	for _, sc := range pfScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			spec := pfDefault()
			if sc.spec != nil {
				sc.spec(&spec)
			}
			w := pfSeed(t, spec)
			before := len(w.gl.Requests())
			_, _, _, _, tr := pfRun(t, w, sc.args...)
			sent := w.gl.Requests()[before:]
			if len(sent) < 4 {
				t.Fatalf("only %d requests were sent: the command did not read GitLab: %v", len(sent), sent)
			}
			for _, rq := range sent {
				if !strings.HasPrefix(rq, "GET ") {
					t.Errorf("the command sent %q", rq)
				}
			}
			if n := tr.refused.Load(); n != 0 {
				t.Errorf("the command ATTEMPTED %d write(s); its client refused them", n)
			}
			if len(w.gl.MRNotesOf(w.pid, pfMRIID)) != 0 {
				t.Errorf("a note was written on the merge request")
			}
		})
	}
}

// TestTheReadOnlyClientCannotWrite: every write pkg/glab can send, sent
// through the subcommand's client, is refused before it leaves the process.
func TestTheReadOnlyClientCannotWrite(t *testing.T) {
	w := pfSeed(t, pfDefault())
	c, tr := readOnlyGitLab(w.gl.URL(), pfFakeToken)
	c.RetryBackoff = func(int) time.Duration { return 0 }
	ctx := context.Background()
	desc := "x"
	writes := map[string]func() error{
		"CreateMRNote":   func() error { _, err := c.CreateMRNote(ctx, w.pid, pfMRIID, "x"); return err },
		"UpdateMRNote":   func() error { _, err := c.UpdateMRNote(ctx, w.pid, pfMRIID, 1, "x"); return err },
		"UpdateMRLabels": func() error { _, err := c.UpdateMRLabels(ctx, w.pid, pfMRIID, []string{"a"}, nil); return err },
		"UpdateMergeRequest": func() error {
			_, err := c.UpdateMergeRequest(ctx, w.pid, pfMRIID, glab.MRUpdate{Description: &desc})
			return err
		},
		"AcceptMR":     func() error { _, err := c.AcceptMR(ctx, w.pid, pfMRIID, pfHeadSHA); return err },
		"DeleteBranch": func() error { return c.DeleteBranch(ctx, w.pid, "gonk/pipefix-0123456789ab") },
		"CreateBranch": func() error { _, err := c.CreateBranch(ctx, w.pid, "x", "main"); return err },
		"CreateCommit": func() error {
			_, err := c.CreateCommit(ctx, w.pid, glab.CommitOptions{Branch: "x", CommitMessage: "m",
				Actions: []glab.CommitAction{{Action: "create", FilePath: "f", Content: "c"}}})
			return err
		},
		"CreateMergeRequest": func() error {
			_, err := c.CreateMergeRequest(ctx, w.pid, glab.MROptions{SourceBranch: "x", TargetBranch: "main", Title: "t"})
			return err
		},
		"CreateIssueNote": func() error { _, err := c.CreateIssueNote(ctx, w.pid, 1, "x"); return err },
	}
	before := len(w.gl.Requests())
	for name, write := range writes {
		if err := write(); !errors.Is(err, errReadOnlyGitLab) {
			t.Errorf("%s: err = %v, want the read-only refusal", name, err)
		}
	}
	if got := w.gl.Requests()[before:]; len(got) != 0 {
		t.Errorf("requests reached GitLab: %v", got)
	}
	// At least once each: the client retries a transport error, and every
	// retry is refused again.
	if n := tr.refused.Load(); n < int64(len(writes)) {
		t.Errorf("%d refusals for %d writes", n, len(writes))
	}
	// And it still reads.
	if _, err := c.GetMergeRequest(ctx, w.pid, pfMRIID); err != nil {
		t.Errorf("a read through the read-only client failed: %v", err)
	}
}

func TestPipefixJudgeOutputCarriesTheFactsItRestedOn(t *testing.T) {
	spec := pfDefault()
	spec.headStatus = "failed"
	spec.headJobs[1] = pfFailWith(pfOtherLine)
	spec.headJobs[2].status = "skipped"
	w := pfSeed(t, spec)
	code, out, raw, _, _ := pfRun(t, w)
	if code != 0 || out.Result.Verdict != pipefix.VerdictFixedNewFailure {
		t.Fatalf("exit %d result %+v", code, out.Result)
	}
	if out.Project != w.pid || !out.Now.Equal(pfNow) {
		t.Errorf("project %d now %v", out.Project, out.Now)
	}
	f := out.Facts
	if !f.Fix.Recorded || f.Fix.BasePipelineID != w.base.ID || f.Fix.FixBaseSHA != pfBaseSHA || f.Fix.FixCommitSHA != pfHeadSHA ||
		f.Fix.MRIID != pfMRIID || len(f.Fix.Targets) != 1 || f.Fix.Targets[0].Key.Name != "unit-tests" ||
		f.Fix.Targets[0].Key.ErrorSignature == "" || f.Fix.Targets[0].Generic || len(f.Fix.BaseJobs) != 3 ||
		f.Fix.CIExecChanges != pipefix.CIExecDeny || len(f.Fix.Lift) != 0 || !f.Fix.VerifyDeadline.Equal(pfNow.Add(time.Hour)) {
		t.Errorf("fix = %+v", f.Fix)
	}
	if !f.MR.Read || !f.Pipelines.Read || !f.Definition.Read {
		t.Errorf("a group is unread: %+v", f)
	}
	if !out.Base.Read || out.Base.SHA != pfBaseSHA || len(out.Base.Recorded) != 1 {
		t.Errorf("base = %+v", out.Base)
	}
	if len(out.Result.Jobs) != 1 || out.Result.Jobs[0].NewSignature == "" ||
		out.Result.Jobs[0].NewSignature == f.Fix.Targets[0].Key.ErrorSignature {
		t.Errorf("job finding = %+v: the new failure's signature must be carried and differ", out.Result.Jobs)
	}
	// Every synthesised input is named, by the field it stands in for.
	for _, field := range []string{"fix.targets", "fix.base_jobs", "fix.fix_base_sha", "fix.fix_commit_sha", "fix.excerpt_version",
		"fix.signature_version", "fix.lift", "fix.ci_exec_changes", "fix.verify_deadline"} {
		named := false
		for _, s := range out.Synthesised {
			named = named || strings.HasPrefix(s, field) || strings.Contains(strings.SplitN(s, ":", 2)[0], field)
		}
		if !named {
			t.Errorf("synthesised does not name %s: %v", field, out.Synthesised)
		}
	}
	// The field names the probe reads, pinned as text.
	for _, key := range []string{`"result"`, `"Verdict": "FIXED_NEW_FAILURE"`, `"Rule": "J3.CHANGED"`, `"facts"`, `"AllowFailure": false`,
		`"FailureReason": "script_failure"`, `"IncludeRetried": true`, `"FirstError": "` + pfOtherLine + `"`, `"evidence"`, `"reads"`,
		`"head_lint"`, `"merged_yaml_bytes"`, `"synthesised"`, `"base"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("the output lacks %s", key)
		}
	}
	if strings.Contains(raw, `"merged_yaml":`) {
		t.Errorf("the merged YAML is printed without --merged-yaml")
	}
	if _, _, raw, _, _ := pfRun(t, w, "--merged-yaml"); !strings.Contains(raw, `"merged_yaml": "stages: [build, test, package]`) {
		t.Errorf("--merged-yaml did not print the merged YAML")
	}
	if strings.Contains(raw, pfFakeToken) {
		t.Errorf("the token is in the output")
	}
}

func TestPipefixJudgeWithoutAFixCommitSaysItUsedTheHead(t *testing.T) {
	w := pfSeed(t, pfDefault())
	_, out, _, _, _ := pfRun(t, w)
	if out.Facts.Fix.FixCommitSHA != pfHeadSHA {
		t.Fatalf("fix commit = %q, want the merge request's head", out.Facts.Fix.FixCommitSHA)
	}
	said := false
	for _, s := range out.Synthesised {
		said = said || (strings.HasPrefix(s, "fix.fix_commit_sha") && strings.Contains(s, "head") && strings.Contains(s, "taken-over"))
	}
	if !said {
		t.Errorf("synthesised does not say the head stood in for the fix commit: %v", out.Synthesised)
	}
}

// TestPipefixJudgeCannotJudgeIsExit1: a read that fails is not a verdict and
// not exit 0 -- and the result is still printed, naming what is missing.
func TestPipefixJudgeCannotJudgeIsExit1(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fail    func(w *pfxWorld) string
		args    []string
		missing string
	}{
		{"the base pipeline cannot be read", func(w *pfxWorld) string { return fmt.Sprintf("/pipelines/%d", w.base.ID) }, nil, "fix.recorded"},
		{"the merge request cannot be read", func(w *pfxWorld) string { return "/merge_requests/12" }, nil, "fix.fix_commit_sha"},
		{"the CI definition cannot be read", func(w *pfxWorld) string { return "/ci/lint" }, nil, "definition"},
		{"a job that did not fail on the base pipeline is not a target", nil, []string{"--job", "compile"}, "fix.targets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := pfSeed(t, pfDefault())
			if tc.fail != nil {
				w.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d%s", w.pid, tc.fail(w)), 403)
			}
			code, out, _, stderr, _ := pfRun(t, w, tc.args...)
			if code != pipefixJudgeExitCannotJudge || out.Result.Decision != pipefix.DecisionCannotJudge || out.Result.Verdict != "" {
				t.Fatalf("exit %d result %+v; want exit 1 and cannot-judge", code, out.Result)
			}
			if !strings.Contains(strings.Join(out.Result.Missing, "\n"), tc.missing) {
				t.Errorf("missing = %v, want something about %s", out.Result.Missing, tc.missing)
			}
			if !strings.Contains(stderr, "no verdict") {
				t.Errorf("stderr does not say there is no verdict: %q", stderr)
			}
		})
	}
	t.Run("the target problem is printed", func(t *testing.T) {
		w := pfSeed(t, pfDefault())
		_, out, _, _, _ := pfRun(t, w, "--job", "compile")
		if len(out.TargetProblems) != 1 || !strings.Contains(out.TargetProblems[0], "compile") {
			t.Fatalf("target problems = %v", out.TargetProblems)
		}
	})
}

func TestPipefixJudgeUsage(t *testing.T) {
	w := pfSeed(t, pfDefault())
	ids := []string{"--project", fmt.Sprint(w.pid), "--base-pipeline", fmt.Sprint(w.base.ID), "--mr", "12"}
	for name, args := range map[string][]string{
		"no arguments":           nil,
		"no job":                 ids,
		"no project":             {"--base-pipeline", "1", "--mr", "12", "--job", "unit-tests"},
		"no base pipeline":       {"--project", "1", "--mr", "12", "--job", "unit-tests"},
		"no merge request":       {"--project", "1", "--base-pipeline", "1", "--job", "unit-tests"},
		"a short fix commit":     append(append([]string{}, ids...), "--job", "unit-tests", "--fix-commit", "2222222"),
		"an unknown exec policy": append(append([]string{}, ids...), "--job", "unit-tests", "--ci-exec-changes", "yes"),
		"a lift that is not ci":  append(append([]string{}, ids...), "--job", "unit-tests", "--lift", "control"),
		"a deadline that is not": append(append([]string{}, ids...), "--job", "unit-tests", "--deadline", "tomorrow"),
		"a positional argument":  append(append([]string{}, ids...), "--job", "unit-tests", "extra"),
		"an unknown flag":        append(append([]string{}, ids...), "--job", "unit-tests", "--write"),
		"an empty job name":      append(append([]string{}, ids...), "--job", " "),
	} {
		t.Run(name, func(t *testing.T) {
			before := len(w.gl.Requests())
			var stdout, stderr bytes.Buffer
			code := runPipefixJudge(context.Background(), args, w.gl.Client(), func() time.Time { return pfNow },
				func(string) string { return "" }, &stdout, &stderr)
			if code != pipefixJudgeExitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("exit %d stdout %q stderr %q; want exit 2, nothing printed, a reason", code, stdout.String(), stderr.String())
			}
			if n := len(w.gl.Requests()) - before; n != 0 {
				t.Errorf("%d request(s) were sent on a usage error", n)
			}
		})
	}
}

// TestPipefixJudgeBuildsItsOwnReadOnlyClient: with no reader given, the
// command reads the GitLab URL and the token FILE from the environment, and
// what it builds is the read-only client.
func TestPipefixJudgeBuildsItsOwnReadOnlyClient(t *testing.T) {
	w := pfSeed(t, pfDefault())
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(pfFakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--project", fmt.Sprint(w.pid), "--base-pipeline", fmt.Sprint(w.base.ID), "--mr", "12", "--job", "unit-tests"}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	run := func(m map[string]string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := runPipefixJudge(context.Background(), args, nil, func() time.Time { return pfNow }, env(m), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	before := len(w.gl.Requests())
	code, stdout, stderr := run(map[string]string{"GONK_GITLAB_URL": w.gl.URL(), "GONK_GITLAB_TOKEN_FILE": tokenFile})
	if code != 0 || !strings.Contains(stdout, `"Verdict": "FIXED"`) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, rq := range w.gl.Requests()[before:] {
		if !strings.HasPrefix(rq, "GET ") {
			t.Errorf("sent %q", rq)
		}
	}
	if strings.Contains(stdout+stderr, pfFakeToken) {
		t.Errorf("the token is printed")
	}
	// What it built is the client that cannot write.
	built, err := pipefixJudgeClient(env(map[string]string{"GONK_GITLAB_URL": w.gl.URL(), "GONK_GITLAB_TOKEN_FILE": tokenFile}))
	if err != nil {
		t.Fatal(err)
	}
	built.RetryBackoff = func(int) time.Duration { return 0 }
	n := len(w.gl.Requests())
	if _, err := built.CreateMRNote(context.Background(), w.pid, pfMRIID, "x"); !errors.Is(err, errReadOnlyGitLab) {
		t.Errorf("a write through the command's own client: err = %v, want the read-only refusal", err)
	}
	if len(w.gl.Requests()) != n || len(w.gl.MRNotesOf(w.pid, pfMRIID)) != 0 {
		t.Errorf("the command's own client wrote to GitLab")
	}
	// The marker-free name the controller exports works too.
	if code, _, _ := run(map[string]string{"GONK_GITLAB_URL": w.gl.URL(), "GONK_BOT_FILE": tokenFile}); code != 0 {
		t.Errorf("GONK_BOT_FILE: exit %d", code)
	}
	for name, m := range map[string]map[string]string{
		"no URL":         {"GONK_GITLAB_TOKEN_FILE": tokenFile},
		"no token file":  {"GONK_GITLAB_URL": w.gl.URL()},
		"a missing file": {"GONK_GITLAB_URL": w.gl.URL(), "GONK_GITLAB_TOKEN_FILE": tokenFile + ".absent"},
		"nothing set":    {},
	} {
		n := len(w.gl.Requests())
		if code, stdout, _ := run(m); code != pipefixJudgeExitUsage || stdout != "" || len(w.gl.Requests()) != n {
			t.Errorf("%s: exit %d stdout %q; want exit 2 and no request", name, code, stdout)
		}
	}
}
