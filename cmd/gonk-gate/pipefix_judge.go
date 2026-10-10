package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// `gonk-gate pipefix-judge` -- plan section 10, proof rung L2 (gonk-2usw).
//
// Runs the exit check of plan 6.3 (pipefix.Judge) over LIVE GitLab facts for
// one base pipeline and one merge request, and prints the Result together
// with every fact it rested on, as one JSON object on stdout.
//
// IT IS READ-ONLY, BY CONSTRUCTION. It writes nothing to GitLab and opens no
// store: its GitLab client refuses, in its transport, every request that is
// not a GET (readOnlyGitLab), and the code that fills the facts is written
// against pipefixReader, an interface of six reads. There is no meter, no
// work store and no session runtime here -- it is answered before
// loadGateConfig and reads only the GitLab URL and the token FILE.
//
// What a fix RECORD would supply and this command has to SYNTHESISE instead
// (there is no fix record yet: APPLY and VERIFY are not built). Each is named
// in the output's "synthesised" list:
//
//   - T and its failure keys: from the BASE pipeline, read now -- the failed
//     job of each --job name, with the signature of its log's first error
//     line, as the diagnosis dispatch records it. (A real T comes from the
//     landed diagnosis's evidence; --job stands in for it.)
//   - BaseJobs: the base pipeline's jobs, read now.
//   - FixBaseSHA: the base pipeline's SHA.
//   - FixCommitSHA: --fix-commit, or the merge request's head when absent
//     (in which case J0's "somebody else pushed" can never be observed).
//   - ExcerptVersion / SignatureVersion: this build's.
//   - Lift, CIExecChanges: --lift, --ci-exec-changes (default: no lift, deny).
//   - VerifyDeadline: --deadline, default one hour from now.
//
// Exit codes: 0 a decision was printed (verdict, wait or stopped); 1 the
// result was printed and it is "cannot-judge" (a read failed or a fact is
// missing -- look again); 2 usage or misconfiguration, nothing was read.
const (
	pipefixJudgeExitOK          = 0
	pipefixJudgeExitCannotJudge = 1
	pipefixJudgeExitUsage       = 2
)

// errReadOnlyGitLab is what the read-only client answers to anything but a GET.
var errReadOnlyGitLab = errors.New("this GitLab client is read-only: only GET is sent")

// getOnlyTransport is the construction that makes `pipefix-judge` unable to
// write: a request whose method is not GET never leaves the process.
type getOnlyTransport struct {
	next http.RoundTripper
	// refused counts the requests that were not sent.
	refused atomic.Int64
}

func (t *getOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		t.refused.Add(1)
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, errReadOnlyGitLab)
	}
	return t.next.RoundTrip(req)
}

// readOnlyGitLab builds a GitLab client that cannot write. Like every client
// here it has NO TLSClientConfig: the private CA is trusted through the
// system pool / SSL_CERT_DIR (see glab.New).
func readOnlyGitLab(baseURL, token string) (*glab.Client, *getOnlyTransport) {
	c := glab.New(baseURL, token)
	tr := &getOnlyTransport{next: http.DefaultTransport}
	c.HTTP = &http.Client{Timeout: c.HTTP.Timeout, Transport: tr}
	c.UserAgent = "gonk-gate/pipefix-judge"
	return c, tr
}

// pipefixJudgeClient builds the subcommand's GitLab client from the
// environment: GONK_GITLAB_URL and the token FILE (GONK_GITLAB_TOKEN_FILE, or
// the marker-free GONK_BOT_FILE the controller exports). It is the read-only
// client and nothing else.
func pipefixJudgeClient(getenv func(string) string) (*glab.Client, error) {
	baseURL := getenv("GONK_GITLAB_URL")
	tokenFile := firstNonEmpty(getenv("GONK_GITLAB_TOKEN_FILE"), getenv("GONK_BOT_FILE"))
	if baseURL == "" || tokenFile == "" {
		return nil, errors.New("GONK_GITLAB_URL and GONK_GITLAB_TOKEN_FILE (a file path, never the token) must be set")
	}
	token, err := readSecretFile(tokenFile)
	if err != nil {
		return nil, err
	}
	c, _ := readOnlyGitLab(baseURL, token)
	return c, nil
}

// pipefixJudgeOutput is the JSON the subcommand prints. The fact structs are
// pkg/pipefix's own and carry Go field names.
type pipefixJudgeOutput struct {
	Project int64          `json:"project"`
	Now     time.Time      `json:"now"`
	Result  pipefix.Result `json:"result"`
	// Facts is exactly what Judge was given.
	Facts pipefix.JudgeFacts `json:"facts"`
	// Base is the base pipeline as read; Facts.Fix was built from it.
	Base pipefixBase `json:"base"`
	// TargetProblems: why T could not be built from the base pipeline
	// (pipefix.TargetSet), e.g. a --job that did not fail there.
	TargetProblems []string        `json:"target_problems,omitempty"`
	Evidence       pipefixEvidence `json:"evidence"`
	// Synthesised names every input a fix record would have supplied.
	Synthesised []string `json:"synthesised"`
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

// jobList is --job: repeatable, and NOT split on commas (a job name may hold
// one, e.g. a parallel matrix job "build: [a, b]").
type jobList []string

func (l *jobList) String() string { return strings.Join(*l, "; ") }
func (l *jobList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("an empty job name")
	}
	*l = append(*l, v)
	return nil
}

// runPipefixJudge is the subcommand. r is the reader; nil means "build the
// read-only client from the environment" (the only non-test path). now is the
// clock.
func runPipefixJudge(ctx context.Context, args []string, r pipefixReader, now func() time.Time, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipefix-judge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.Int64("project", 0, "GitLab project id")
	basePipeline := fs.Int64("base-pipeline", 0, "id of the diagnosed (base) pipeline")
	mrIID := fs.Int64("mr", 0, "iid of the fix merge request")
	var jobs jobList
	fs.Var(&jobs, "job", "name of a diagnosed job (repeatable): the target set T")
	fixCommit := fs.String("fix-commit", "", "full SHA of the fix commit (default: the merge request's head, read now)")
	var lift stringList
	fs.Var(&lift, "lift", "path classes the dispatch lifted, e.g. ci (default: none)")
	ciExec := fs.String("ci-exec-changes", pipefix.CIExecDeny, "pipeline_fix.ci_exec_changes: deny, manual-run or auto")
	deadline := fs.String("deadline", "", "verify deadline, RFC 3339 (default: one hour from now)")
	withYAML := fs.Bool("merged-yaml", false, "include the merged CI YAML of both commits in the output")
	usage := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "pipefix-judge: "+format+". Nothing was read.\n", a...)
		return pipefixJudgeExitUsage
	}
	if err := fs.Parse(args); err != nil {
		return pipefixJudgeExitUsage
	}
	switch {
	case fs.NArg() != 0:
		return usage("unexpected argument %q", fs.Arg(0))
	case *project <= 0 || *basePipeline <= 0 || *mrIID <= 0:
		return usage("--project, --base-pipeline and --mr are required positive ids")
	case len(jobs) == 0:
		return usage("at least one --job is required: the diagnosed job(s)")
	case *fixCommit != "" && !glab.IsCommitSHA(*fixCommit):
		return usage("--fix-commit %q is not a full commit SHA", *fixCommit)
	case *ciExec != pipefix.CIExecDeny && *ciExec != pipefix.CIExecManualRun && *ciExec != pipefix.CIExecAuto:
		return usage("--ci-exec-changes %q is not deny, manual-run or auto", *ciExec)
	}
	for _, l := range lift {
		if l != pipefix.LiftCI {
			return usage("--lift %q: only %q can be lifted", l, pipefix.LiftCI)
		}
	}
	at := now()
	synth := []string{
		"fix.targets: from the base pipeline's failed job(s) named by --job, read now (a record holds T from the landed diagnosis)",
		"fix.base_jobs: the base pipeline's jobs, read now",
		"fix.fix_base_sha: the base pipeline's sha",
		"fix.excerpt_version, fix.signature_version: this build's",
	}
	due := at.Add(time.Hour)
	if *deadline != "" {
		d, err := time.Parse(time.RFC3339, *deadline)
		if err != nil {
			return usage("--deadline %q is not RFC 3339", *deadline)
		}
		due = d
		synth = append(synth, "fix.verify_deadline: --deadline")
	} else {
		synth = append(synth, "fix.verify_deadline: one hour from now (no --deadline)")
	}
	if len(lift) == 0 {
		synth = append(synth, "fix.lift: none (no --lift)")
	} else {
		synth = append(synth, "fix.lift: --lift")
	}
	synth = append(synth, "fix.ci_exec_changes: --ci-exec-changes ("+*ciExec+")")

	if r == nil {
		c, err := pipefixJudgeClient(getenv)
		if err != nil {
			return usage("%v", err)
		}
		r = c
	}

	out := pipefixJudgeOutput{Project: *project, Now: at}
	out.Base = readPipefixBase(ctx, r, *project, *basePipeline, jobs)
	fix := pipefix.FixFacts{
		BasePipelineID: *basePipeline, MRIID: *mrIID, FixCommitSHA: *fixCommit,
		ExcerptVersion: pipelinectx.ExcerptVersion, SignatureVersion: pipelinectx.SignatureVersion,
		Lift: lift, CIExecChanges: *ciExec, VerifyDeadline: due,
	}
	if out.Base.Read {
		canon := make([]string, 0, len(jobs))
		for _, j := range jobs {
			canon = append(canon, jobNameText(j))
		}
		fix.Recorded = true
		fix.Targets, out.TargetProblems = pipefix.TargetSet(out.Base.Recorded, canon)
		if len(out.TargetProblems) > 0 {
			// A T that cannot be judged is not a T (Eligible refuses it, E5).
			fix.Targets = nil
		}
		fix.BaseJobs, fix.FixBaseSHA = out.Base.Jobs, out.Base.SHA
	}
	if fix.FixCommitSHA == "" {
		// One extra read, for the one input that has no flag.
		if mr, rd := readPipefixMR(ctx, r, *project, *mrIID); mr.Read {
			fix.FixCommitSHA = mr.SHA
		} else {
			out.Evidence.Reads = append(out.Evidence.Reads, rd)
		}
		synth = append(synth, "fix.fix_commit_sha: the merge request's head as read now (no --fix-commit): J0's taken-over cannot be observed")
	} else {
		synth = append(synth, "fix.fix_commit_sha: --fix-commit")
	}
	var ev pipefixEvidence
	out.Facts, ev = readPipefixJudgeFacts(ctx, r, *project, fix)
	out.Evidence.Reads = append(out.Evidence.Reads, ev.Reads...)
	out.Evidence.BaseLint, out.Evidence.HeadLint = ev.BaseLint, ev.HeadLint
	if !*withYAML {
		for _, l := range []*pipefixLint{out.Evidence.BaseLint, out.Evidence.HeadLint} {
			if l != nil {
				l.MergedYAML = ""
			}
		}
	}
	out.Synthesised = synth
	out.Result = pipefix.Judge(out.Facts, at)

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		_, _ = fmt.Fprintf(stderr, "pipefix-judge: cannot print the result: %v\n", err)
		return pipefixJudgeExitCannotJudge
	}
	if out.Result.Decision == pipefix.DecisionCannotJudge {
		_, _ = fmt.Fprintf(stderr, "pipefix-judge: %s\n", out.Result.Detail)
		return pipefixJudgeExitCannotJudge
	}
	return pipefixJudgeExitOK
}

// pipefixJudgeMain is main's entry: the real clock, environment and streams.
func pipefixJudgeMain(args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return runPipefixJudge(ctx, args, nil, time.Now, os.Getenv, os.Stdout, os.Stderr)
}
