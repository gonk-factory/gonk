#!/usr/bin/env python3
"""mutate_pipefix_reader.py -- "seen red" evidence for gonk-2usw part PROBE:
the read side of the pipeline-fix exit check (cmd/gonk-gate/pipefix_facts.go),
the read-only `pipefix-judge` subcommand (pipefix_judge.go) and the two GitLab
reads it added (pkg/glab/pipefix_reads.go).

Same contract as hack/mutate_pipefix.py: one named break at a time, ONLY the
test that guards it is run (in the package named with it) and must FAIL; every
file is restored byte for byte. A search text that does not occur EXACTLY once
is VACUOUS, a break the named test does not notice SURVIVED, one that does not
compile NOCOMPILE, and a break that names a subtest is only KILLED when a
subtest is among the failures -- each of those fails this script. The
unmutated packages must be green first.

The four breaks the task names are marked [NAMED]:
  a failed read becomes a zero-value fact
  the subcommand performs a write
  the latest attempt only is read
  a 404 JSON body accepted as a pipeline

Usage: hack/mutate_pipefix_reader.py            (all)
       hack/mutate_pipefix_reader.py 3 7        (only these indexes)
       hack/mutate_pipefix_reader.py --list
       hack/mutate_pipefix_reader.py --show 3   (also the full `go test` output)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree. Needs `go`
on PATH (or GO=/path/to/go). Not run by any gate (gonk-5ed1).
"""
import os
import re
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
GATE = './cmd/gonk-gate/'
GLAB = './pkg/glab/'
F = 'cmd/gonk-gate/pipefix_facts.go'
JG = 'cmd/gonk-gate/pipefix_judge.go'
G = 'pkg/glab/pipefix_reads.go'


def run(test, *subs):
    parts = ['^%s$' % test]
    for s in subs:
        parts.append('^%s$' % re.escape(s.replace(' ', '_')))
    return '/'.join(parts)


muts = []


def m(name, edits, pattern, pkg=GATE):
    muts.append((name, edits, pattern, pkg))


READ = 'TestAFailedReadIsNotAFact'
BASEREAD = 'TestAFailedBaseReadIsNotAFact'
NOTPL = 'TestWhatIsNotAPipelineIsNotRead'
NOTBASE = 'TestWhatIsNotTheBasePipelineIsNotRead'
NOTMR = 'TestWhatIsNotTheMergeRequestIsNotRead'
DELTA = 'TestDefinitionDelta'
TRACE = 'TestATraceThatIsNotTheWholeEndOfTheLogIsHandled'
WRITES = 'TestPipefixJudgeWritesNothing'
RO = 'TestTheReadOnlyClientCannotWrite'
OUT = 'TestPipefixJudgeOutputCarriesTheFactsItRestedOn'
USAGE = 'TestPipefixJudgeUsage'
EXIT1 = 'TestPipefixJudgeCannotJudgeIsExit1'
ATT = 'TestListPipelineAttemptsRejectsWhatIsNotAJobOfThatPipeline'
LINT = 'TestCILintStaticAssertsItsFields'

# ------------------------------------------- a failed read is never a fact
m("[NAMED] a failed read becomes a zero-value fact: an unread merge request is an opened one",
  [(F, '\tcase err != nil:\n\t\treturn pipefix.MRFacts{}, pipefixFailed("mr", ref, err)',
       '\tcase err != nil:\n\t\treturn pipefix.MRFacts{Read: true, IID: mrIID, State: "opened"}, pipefixFailed("mr", ref, err)')],
  run(READ, "the merge request"))
m("[NAMED] a failed read becomes a zero-value fact: an unread pipeline list is an empty one",
  [(F, '\tif err != nil {\n\t\treturn pipefix.PipelineFacts{}, []pipefixRead{pipefixFailed("mr-pipelines", ref, err)}',
       '\tif err != nil {\n\t\treturn pipefix.PipelineFacts{Read: true}, []pipefixRead{pipefixFailed("mr-pipelines", ref, err)}')],
  run(READ, "the merge request's pipelines"))
m("[NAMED] a failed read becomes a zero-value fact: an unread job list is a pipeline with no jobs",
  [(F, '\tif err != nil {\n\t\treturn nil, false, []pipefixRead{pipefixFailed("jobs", ref, err)}',
       '\tif err != nil {\n\t\treturn nil, true, []pipefixRead{pipefixFailed("jobs", ref, err)}')],
  run(READ, "a pipeline's jobs"))
m("[NAMED] a failed read becomes a zero-value fact: an unread log is an empty log",
  [(F, '\tcase err != nil:\n\t\treturn pipefix.Trace{}, pipefixFailed(what, ref, err)',
       '\tcase err != nil:\n\t\treturn pipefix.FailedTrace(pipefix.LogRead{}), pipefixFailed(what, ref, err)')],
  run(READ, "a failed attempt's log"))
m("[NAMED] a failed read becomes a zero-value fact: an unread definition is `none`",
  [(F, '\tif base == nil || head == nil {\n\t\treturn pipefix.DefinitionFacts{}, baseEv, headEv, reads',
       '\tif base == nil || head == nil {\n\t\treturn pipefix.DefinitionFacts{Read: true, Delta: cidef.Delta{Kind: cidef.KindNone}}, baseEv, headEv, reads')],
  run(READ, "the CI definition"))
m("a failed read is not reported in the evidence",
  [(F, 'func pipefixFailed(what, ref string, err error) pipefixRead {\n\treturn pipefixRead{What: what, Ref: ref, Err: err.Error()}',
       'func pipefixFailed(what, ref string, err error) pipefixRead {\n\treturn pipefixRead{What: what, Ref: ref, OK: true}')],
  run(READ, "the CI definition"))
m("[NAMED] a failed read becomes a zero-value fact: an unread base pipeline is a read one with nothing in it",
  [(F, '\tcase err != nil:\n\t\treturn unread(pipefixFailed("base-pipeline", ref, err))',
       '\tcase err != nil:\n\t\treturn pipefixBase{Read: true, PipelineID: pipelineID}')],
  run(BASEREAD, "the base pipeline"))
m("[NAMED] a failed read becomes a zero-value fact: an unread base job list is a base pipeline with no jobs",
  [(F, '\tif err != nil {\n\t\treturn unread(pipefixFailed("base-jobs", ref, err))\n\t}', '\tif false {\n\t\treturn unread(pipefixFailed("base-jobs", ref, err))\n\t}')],
  run(BASEREAD, "the base pipeline's jobs"))
m("[NAMED] a failed read becomes a zero-value fact: an unread base log is an empty one (a generic target)",
  [(F, '\t\tif !trace.Read {\n\t\t\treturn unread(rd)\n\t\t}', '\t\tif false {\n\t\t\treturn unread(rd)\n\t\t}')],
  run(BASEREAD, "the diagnosed job's log"))
m("a failed base read keeps what was read before it",
  [(F, '\t\treturn pipefixBase{PipelineID: pipelineID, Reads: append(reads, rd)}',
       '\t\treturn pipefixBase{PipelineID: pipelineID, Jobs: []pipefix.BaseJob{}, Reads: append(reads, rd)}')],
  run(BASEREAD, "the diagnosed job's log"))

# ------------------------------------------- every attempt
m("[NAMED] the latest attempt only is read: the job list is asked for without include_retried",
  [(G, 'c.ListPipelineJobs(ctx, projectID, pipelineID, "", true)', 'c.ListPipelineJobs(ctx, projectID, pipelineID, "", false)')],
  run('TestEveryAttemptIsReadNotOnlyTheLatest'))
m("[NAMED] the latest attempt only is read: ... and pkg/glab's own test of the request notices",
  [(G, 'c.ListPipelineJobs(ctx, projectID, pipelineID, "", true)', 'c.ListPipelineJobs(ctx, projectID, pipelineID, "", false)')],
  run('TestListPipelineAttempts'), GLAB)
m("[NAMED] the latest attempt only is read: the fake lists every attempt whatever is asked (the test would prove nothing)",
  [('pkg/glab/glabtest/pipelines.go', '\tif q.Get("include_retried") != "true" {', '\tif false {')],
  run('TestJobsListHidesRetriedAttemptsUnlessAsked'), './pkg/glab/glabtest/')
m("a job list is not marked as holding every attempt",
  [(F, '\t\t\tout.IncludeRetried = out.JobsRead', '\t\t\tout.IncludeRetried = false')], run('TestPipefixFactsOfAnHonestFix'))

# ------------------------------------------- an object that parses is not the object
m("[NAMED] a 404 JSON body accepted as a pipeline: listed entries are not checked",
  [(F, '\t\tif problem := pipefixPipelineProblem(p, projectID); problem != "" {\n\t\t\treturn pipefix.PipelineFacts{}',
       '\t\tif problem := pipefixPipelineProblem(p, projectID); false {\n\t\t\t_ = problem\n\t\t\treturn pipefix.PipelineFacts{}')],
  run(NOTPL, 'an error object ({"message":"404 Not found"})'))
for case, sub in (("\tcase p.ID == 0:", "no id"), ("\tcase !glab.IsCommitSHA(p.SHA):", "a sha that is not a commit"),
                  ('\tcase p.Source == "":', "no source"), ('\tcase p.Status == "":', "no status"),
                  ("\tcase p.ProjectID != projectID:", "another project's pipeline")):
    m("a listed pipeline's field is not asserted (%s)" % sub, [(F, case, "\tcase false:")], run(NOTPL, sub))
m("a pipeline's sha may be empty", [(F, "\tcase !glab.IsCommitSHA(p.SHA):", '\tcase p.SHA != "" && !glab.IsCommitSHA(p.SHA):')], run(NOTPL, "no sha"))
m("[NAMED] a 404 JSON body accepted as a pipeline: the base pipeline's fields are not checked",
  [(F, '\tif problem := pipefixPipelineProblem(*p, projectID); problem != "" {', '\tif problem := pipefixPipelineProblem(*p, projectID); false {\n\t\t_ = problem')],
  run(NOTBASE, "a sha that is not a commit"))
m("the base pipeline's id is not compared with the one asked for", [(F, "\tcase p.ID != pipelineID:", "\tcase false:")], run(NOTBASE, "another pipeline"))
m("the merge request's iid is not compared with the one asked for", [(F, "\tcase mr.IID != mrIID:", "\tcase false:")], run(NOTMR, "another MR"))
m("a merge request without a state is read", [(F, '\tcase mr.State == "":', "\tcase false:")], run(NOTMR, "no state"))
m("a job list entry that is not a job is accepted",
  [(F, '\t\tif j.ID == 0 || j.Name == "" || j.Status == "" {\n\t\t\treturn nil, false,', '\t\tif false {\n\t\t\treturn nil, false,')],
  run('TestWhatIsNotAJobOfThePipelineLeavesItsJobsUnread', "no status"))

# ------------------------------------------- names
m("FACTS deviation 5: a live attempt keeps its RAW job name", [(F, "ID: j.ID, Name: jobNameText(j.Name), Stage: j.Stage", "ID: j.ID, Name: j.Name, Stage: j.Stage")],
  run('TestJobNamesAreCanonicalisedLikeTheRecord'))
m("FACTS deviation 5: a base job keeps its RAW job name", [(F, "\t\tname := jobNameText(j.Name)\n", "\t\tname := j.Name\n")],
  run('TestJobNamesAreCanonicalisedLikeTheRecord'))
m("FACTS deviation 5: a target name is matched raw against the base jobs", [(F, "\t\tname := jobNameText(t)\n", "\t\tname := t\n")],
  run('TestJobNamesAreCanonicalisedLikeTheRecord'))

# ------------------------------------------- the definition delta
DIFF = "\tdelta := cidef.Diff([]byte(base.MergedYAML), []byte(head.MergedYAML), targets)"
m("J2: base and head are swapped", [(F, DIFF, "\tdelta := cidef.Diff([]byte(head.MergedYAML), []byte(base.MergedYAML), targets)")],
  run(DELTA, "the head removes a job (D1) -- base and head are not swapped"))
m("J2: the head is compared with itself", [(F, DIFF, "\tdelta := cidef.Diff([]byte(head.MergedYAML), []byte(head.MergedYAML), targets)")],
  run(DELTA, "the head lets the diagnosed job fail (GATING, D2)"))
m("J2: T is not passed to cidef (an exec change on the diagnosed job reads as D3)", [(F, DIFF, DIFF.replace("targets)", "nil)"))],
  run(DELTA, "the head changes what the DIAGNOSED job runs (exec-changing; T is passed)"))
m("J2: the definition is read with a DRY RUN (local includes from the default branch)",
  [(G, 'query:  map[string]string{"content_ref": sha, "include_merged_yaml": "true"},',
       'query:  map[string]string{"content_ref": sha, "include_merged_yaml": "true", "dry_run": "true"},')],
  run(DELTA, "the head lets the diagnosed job fail (GATING, D2)"))
m("J2: ... and pkg/glab's own test of the request notices the dry run",
  [(G, 'query:  map[string]string{"content_ref": sha, "include_merged_yaml": "true"},',
       'query:  map[string]string{"content_ref": sha, "include_merged_yaml": "true", "dry_run": "true"},')],
  run('TestCILintStatic'), GLAB)
m("6.2: an include that is not pinned to the commit is not reported",
  [(F, "\tdelta = withUnpinnedIncludes(delta, base.Includes, head.Includes)\n", "")],
  run(DELTA, "the head adds a remote include: only the head cannot be pinned"))
m("6.2: a LOCAL include is treated as unpinned too", [(F, "\t\t\tif inc.Type != glab.IncludeTypeLocal {", "\t\t\tif true {")],
  run(DELTA, "local includes pinned to each commit are comparable"))
m("6.2: an include both sides have is blamed on the base alone",
  [(F, "\t\tif h[k] {\n\t\t\tsides[k] = cidef.SideBoth", "\t\tif h[k] {\n\t\t\tsides[k] = cidef.SideBase")],
  run(DELTA, "both commits use the same template include: neither can be pinned"))
m("6.2: an include only the head has is not the head's doing",
  [(F, "\t\tif !b[k] {\n\t\t\tsides[k] = cidef.SideHead", "\t\tif !b[k] {\n\t\t\tsides[k] = cidef.SideBoth")],
  run(DELTA, "the head adds a remote include: only the head cannot be pinned"))
m("6.2: an include only the base has is dropped",
  [(F, "\tfor k := range b {\n\t\tsides[k] = cidef.SideBase", "\tfor k := range b {\n\t\tif !h[k] {\n\t\t\tcontinue\n\t\t}\n\t\tsides[k] = cidef.SideBase")],
  run(DELTA, "only the base has an include of another project"))
m("6.2: an unpinned include hides a violation (Kind becomes unresolvable)",
  [(F, "\tif d.Kind != cidef.KindViolation {\n\t\td.Kind = cidef.KindUnresolvable", "\tif true {\n\t\td.Kind = cidef.KindUnresolvable")],
  run(DELTA, "a violation outranks an include that cannot be pinned"))
m("6.2: an unpinned include leaves Kind as cidef computed it",
  [(F, "\tif d.Kind != cidef.KindViolation {\n\t\td.Kind = cidef.KindUnresolvable", "\tif false {\n\t\td.Kind = cidef.KindUnresolvable")],
  run(DELTA, "both commits use the same template include: neither can be pinned"))

# ------------------------------------------- which jobs and logs are read
COND = "\t\tif p.SHA == fixCommitSHA && p.Source != pipefix.SourceParentPipeline {"
m("jobs are read for every pipeline of the merge request", [(F, COND, "\t\tif p.Source != pipefix.SourceParentPipeline {")],
  run('TestOnlyTheFixCommitsPipelinesHaveTheirJobsRead'))
m("jobs are read for a child pipeline", [(F, COND, "\t\tif p.SHA == fixCommitSHA {")], run('TestAChildPipelineIsListedButNotRead'))
m("the fix commit's own pipeline has no jobs read", [(F, COND, "\t\tif p.SHA != fixCommitSHA {")], run('TestPipefixFactsOfAnHonestFix'))
LOGS = "\t\tif a.Status == pipefix.StatusFailed && inT[a.Name] {"
m("a log is read for every attempt of a diagnosed job", [(F, LOGS, "\t\tif inT[a.Name] {")], run('TestOnlyFailedAttemptsOfDiagnosedJobsHaveTheirLogRead'))
m("a log is read for every failed job", [(F, LOGS, "\t\tif a.Status == pipefix.StatusFailed {")], run('TestOnlyFailedAttemptsOfDiagnosedJobsHaveTheirLogRead'))
m("no log is read at all", [(F, LOGS, "\t\tif false {")], run('TestOnlyFailedAttemptsOfDiagnosedJobsHaveTheirLogRead'))
# gonk-etvm: what a truncated read means is pipefix.FailedTrace's rule; the
# reader's part is to HAND IT THE FLAGS and to report "not read" as a failed
# read. (The rule's own mutants are in hack/mutate_pipefix.py.)
LR = "pipefix.LogRead{Data: tt.Data, HeadTruncated: tt.HeadTruncated, TailTruncated: tt.TailTruncated}"
m("a log whose read stopped before its end is read (the reader does not pass TailTruncated)",
  [(F, LR, "pipefix.LogRead{Data: tt.Data, HeadTruncated: tt.HeadTruncated}")],
  run(TRACE, "the read stopped before the end of the log"))
m("the fragment that starts a head-truncated log is kept (the reader does not pass HeadTruncated)",
  [(F, LR, "pipefix.LogRead{Data: tt.Data, TailTruncated: tt.TailTruncated}")],
  run(TRACE, "the start of the log was dropped: the first line is a fragment"))
m("the first line of EVERY log is dropped", [(F, LR, "pipefix.LogRead{Data: tt.Data, HeadTruncated: true, TailTruncated: tt.TailTruncated}")],
  run(TRACE, "whole, and its first line is the error"))
m("the two truncation flags are swapped", [(F, LR, "pipefix.LogRead{Data: tt.Data, HeadTruncated: tt.TailTruncated, TailTruncated: tt.HeadTruncated}")],
  run(TRACE, "the start of the log was dropped: the first line is a fragment"))
m("a log that was not read whole is reported as a successful read",
  [(F, "\tif !tr.Read {\n", "\tif false {\n")], run(TRACE, "both ends are missing"))
m("a truncated read the function refuses is read anyway by the reader (it compensates the other way)",
  [(F, "\tif !tr.Read {\n\t\treturn pipefix.Trace{}, pipefixFailed(",
       "\tif !tr.Read {\n\t\treturn pipefix.Trace{Read: true}, pipefixFailed(")],
  run(TRACE, "the start of the log was dropped and only the cut line was read: nothing is established"))
m("EVERY log is reported as not read whole", [(F, "\tif !tr.Read {\n", "\tif true {\n")], run(TRACE, "whole"))

# ------------------------------------------- the base pipeline
m("base: the FIRST attempt of a job is its status, not the latest", [(F, "!seen || j.ID > cur.ID", "!seen || j.ID < cur.ID")], run('TestPipefixBaseFacts'))
m("base: a failure is never generic", [(F, "\t\t\tGeneric:        pipelinectx.GenericFirstError(trace.FirstError),", "\t\t\tGeneric:        false,")],
  run('TestPipefixBaseFactsOfAJobWithNoErrorLineOfItsOwnAreGeneric'))
m("base: a target that did not fail is recorded as a failed job",
  [(F, "\t\tif seen[name] || !has || j.Status != pipefix.StatusFailed {", "\t\tif seen[name] || !has {")],
  run('TestPipefixBaseTargetThatDidNotFailIsNotRecorded'))
m("base: the recorded signature is not the first error line's",
  [(F, "\t\t\tErrorSignature: pipelinectx.ErrorSignature(trace.FirstError),", '\t\t\tErrorSignature: pipelinectx.ErrorSignature("x" + trace.FirstError),')],
  run('TestPipefixBaseFacts'))
m("base: the job's failure_reason is not recorded",
  [(F, "Name: name, Stage: j.Stage, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure,", "Name: name, Stage: j.Stage, AllowFailure: j.AllowFailure,")],
  run('TestPipefixBaseFacts'))

# ------------------------------------------- the interface
m("the GET-only method list is not what the interface has (the test must notice)",
  [(F, '\t"CILintStatic", "GetJobTraceTail", "GetMergeRequest",', '\t"CILintStatic", "GetJobTraceTail", "CreateMRNote", "GetMergeRequest",')],
  run('TestPipefixReaderIsGETOnly'))

# ------------------------------------------- the subcommand is read-only
WRITE = ("\tout.Result = pipefix.Judge(out.Facts, at)\n",
         "\tout.Result = pipefix.Judge(out.Facts, at)\n\tif w, ok := r.(*glab.Client); ok {\n"
         "\t\t_, _ = w.CreateMRNote(ctx, *project, *mrIID, \"verdict: \"+string(out.Result.Verdict))\n\t}\n")
GETONLY = ("\tif req.Method != http.MethodGet {", "\tif false {")
m("[NAMED] the subcommand performs a write (its client refuses it; the attempt is what is caught)", [(JG,) + WRITE],
  run(WRITES, "an honest fix"))
m("[NAMED] the subcommand performs a write AND its client lets writes through (the write reaches GitLab)", [(JG,) + WRITE, (JG,) + GETONLY],
  run(WRITES, "not fixed: the same failure"))
m("the read-only transport lets every method through", [(JG,) + GETONLY], run(RO))
m("the read-only transport lets POST through", [(JG, "\tif req.Method != http.MethodGet {", "\tif req.Method != http.MethodGet && req.Method != http.MethodPost {")], run(RO))
m("the read-only client is built without its transport",
  [(JG, "\tc.HTTP = &http.Client{Timeout: c.HTTP.Timeout, Transport: tr}\n", "")], run(RO))
m("the command's own client (from the environment) is an ordinary, writing one",
  [(JG, "\tc, _ := readOnlyGitLab(baseURL, token)\n\treturn c, nil", "\treturn glab.New(baseURL, token), nil")],
  run('TestPipefixJudgeBuildsItsOwnReadOnlyClient'))

# ------------------------------------------- the subcommand's contract
m("cannot-judge exits 0",
  [(JG, '\t\t_, _ = fmt.Fprintf(stderr, "pipefix-judge: %s\\n", out.Result.Detail)\n\t\treturn pipefixJudgeExitCannotJudge',
        '\t\t_, _ = fmt.Fprintf(stderr, "pipefix-judge: %s\\n", out.Result.Detail)\n\t\treturn pipefixJudgeExitOK')],
  run(EXIT1, "the CI definition cannot be read"))
m("a target set with problems is judged on the jobs that are left", [(JG, "\t\t\tfix.Targets = nil\n", "")],
  run(EXIT1, "a job that did not fail on the base pipeline is not a target"))
m("an unread base pipeline still yields a `recorded` fix", [(JG, "\tif out.Base.Read {\n\t\tcanon", "\tif true {\n\t\tcanon")],
  run(EXIT1, "the base pipeline cannot be read"))
m("the default ci_exec_changes is auto, not deny",
  [(JG, 'fs.String("ci-exec-changes", pipefix.CIExecDeny,', 'fs.String("ci-exec-changes", pipefix.CIExecAuto,')], run(OUT))
m("the default deadline is already past", [(JG, "\tdue := at.Add(time.Hour)", "\tdue := at.Add(-time.Hour)")], run(OUT))
m("the merged YAML is always printed", [(JG, "\tif !*withYAML {", "\tif !*withYAML && false {")], run(OUT))
m("--merged-yaml prints nothing", [(JG, "\tif !*withYAML {", "\tif !*withYAML || true {")], run(OUT))
m("the fix commit is not said to be synthesised from the head",
  [(JG, '\t\tsynth = append(synth, "fix.fix_commit_sha: the merge request\'s head as read now (no --fix-commit): J0\'s taken-over cannot be observed")\n', "")],
  run('TestPipefixJudgeWithoutAFixCommitSaysItUsedTheHead'))
m("--fix-commit is ignored (the head is always the fix commit)",
  [(JG, "BasePipelineID: *basePipeline, MRIID: *mrIID, FixCommitSHA: *fixCommit,", "BasePipelineID: *basePipeline, MRIID: *mrIID,")],
  run('TestPipefixJudgeDecisions', "somebody else pushed: the head is not the fix commit"))
m("--lift is ignored", [(JG, "\t\tLift: lift, CIExecChanges: *ciExec, VerifyDeadline: due,", "\t\tCIExecChanges: *ciExec, VerifyDeadline: due,")],
  run('TestPipefixJudgeDecisions', "the same CI change WITH the ci lift"))
m("--deadline is ignored", [(JG, "\t\tdue = d\n", "\t\tdue, _ = due, d\n")], run('TestPipefixJudgeDecisions', "no pipeline and the deadline has passed"))
m("usage: a short --fix-commit is accepted", [(JG, '\tcase *fixCommit != "" && !glab.IsCommitSHA(*fixCommit):', "\tcase false:")], run(USAGE, "a short fix commit"))
m("usage: any --ci-exec-changes is accepted",
  [(JG, "\tcase *ciExec != pipefix.CIExecDeny && *ciExec != pipefix.CIExecManualRun && *ciExec != pipefix.CIExecAuto:", "\tcase false:")],
  run(USAGE, "an unknown exec policy"))
m("usage: any class can be lifted", [(JG, "\t\tif l != pipefix.LiftCI {", "\t\tif false {")], run(USAGE, "a lift that is not ci"))
m("usage: no --job is accepted", [(JG, "\tcase len(jobs) == 0:", "\tcase false:")], run(USAGE, "no job"))
m("usage: a missing id is accepted", [(JG, "\tcase *project <= 0 || *basePipeline <= 0 || *mrIID <= 0:", "\tcase false:")], run(USAGE, "no merge request"))
m("usage: a positional argument is ignored", [(JG, "\tcase fs.NArg() != 0:", "\tcase false:")], run(USAGE, "a positional argument"))

# ------------------------------------------- pkg/glab: the named fields
m("glab: a job with no id / name / status is a job", [(G, '\t\tcase j.ID == 0 || j.Name == "" || j.Status == "":', "\t\tcase false:")],
  run(ATT, "a job with no status"), GLAB)
m("glab: an error object in a job list is a job (the pipeline check alone is left)",
  [(G, "\t\tcase j.Pipeline.ID != pipelineID:", "\t\tcase false:")], run(ATT, "a job of ANOTHER pipeline"), GLAB)
m("glab: only the FIRST job's pipeline is checked",
  [(G, "\tfor i, j := range jobs {\n\t\tswitch {", "\tfor i, j := range jobs[:min(1, len(jobs))] {\n\t\tswitch {")],
  run(ATT, "the second job is of another pipeline"), GLAB)
m("glab: a lint reply with no `valid` is an invalid configuration", [(G, "\tcase present.Valid == nil:", "\tcase false:")],
  run(LINT, "a reason but no `valid`"), GLAB)
m("glab: a valid lint needs no merged_yaml", [(G, '\tcase l.Valid && strings.TrimSpace(l.MergedYAML) == "":', '\tcase l.Valid && strings.TrimSpace(l.MergedYAML) == "" && false:')],
  run(LINT, "valid with a blank merged_yaml"), GLAB)
m("glab: an invalid lint needs no reason", [(G, "\tcase !l.Valid && len(l.Errors) == 0:", "\tcase false:")], run(LINT, "invalid with no reason"), GLAB)
m("glab: a local include resolved at another commit is accepted",
  [(G, "\t\tcase inc.Type == IncludeTypeLocal && inc.ContextSHA != sha:", "\t\tcase false:")],
  run(LINT, "a local include resolved at ANOTHER commit"), GLAB)
m("glab: a local include with no context_sha is accepted",
  [(G, "\t\tcase inc.Type == IncludeTypeLocal && inc.ContextSHA != sha:", '\t\tcase inc.Type == IncludeTypeLocal && inc.ContextSHA != "" && inc.ContextSHA != sha:')],
  run(LINT, "a local include with no context_sha"), GLAB)
m("glab: an include needs no type", [(G, '\t\tcase inc.Type == "":', "\t\tcase false:")], run(LINT, "an include with no type"), GLAB)
m("glab: EVERY include must be at the commit (a remote include fails the read)",
  [(G, "\t\tcase inc.Type == IncludeTypeLocal && inc.ContextSHA != sha:", "\t\tcase inc.ContextSHA != sha:")],
  run('TestCILintStaticAnIncludeThatIsNotLocalIsReturnedNotJudged'), GLAB)
m("glab: a ref name is linted", [(G, "\tif !IsCommitSHA(sha) {\n\t\treturn nil, fmt.Errorf(\"gitlab: ci lint: %q is not a full commit SHA\", sha)\n\t}\n\trq := request{", "\trq := request{")],
  run('TestCILintStaticRejectsNonSHAWithoutARequest'), GLAB)

# ------------------------------------------------------------------ the runner
if '--list' in sys.argv[1:]:
    for i, (name, _, pattern, pkg) in enumerate(muts):
        print(f"[{i}] {name}    {pkg} -run {pattern}")
    sys.exit(0)

show = '--show' in sys.argv[1:]
only = [a for a in sys.argv[1:] if a != '--show']

for pkg in sorted({p for _, _, _, p in muts}):
    base = subprocess.run([GO, "test", pkg, "-count=1"], capture_output=True, text=True)
    if base.returncode != 0:
        print(f"the UNMUTATED package {pkg} is not green; nothing was mutated:\n" + base.stdout + base.stderr)
        sys.exit(2)

res = []
for i, (name, edits, pattern, pkg) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals = {}
    vacuous = False
    for f, _, _ in edits:
        if f not in originals:
            originals[f] = open(f).read()
    cur = dict(originals)
    for f, old, new in edits:
        if cur[f].count(old) != 1:
            vacuous = True
            break
        cur[f] = cur[f].replace(old, new, 1)
    if vacuous or cur == originals:
        res.append(f"VACUOUS  [{i}] {name}")
        print(res[-1], flush=True)
        continue
    out = ""
    try:
        for f, s in cur.items():
            open(f, 'w').write(s)
        r = subprocess.run([GO, "test", pkg, "-run", pattern, "-count=1"], capture_output=True, text=True)
        out = r.stdout + r.stderr
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}" + ("  (the -run pattern matched NO test)" if "no tests to run" in out else ""))
        elif not fails:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:800]}")
        elif '/' in pattern and not any('/' in l for l in fails):
            res.append(f"SURVIVED [{i}] {name}  (red outside the named subtest)\n           " + "\n           ".join(fails))
        else:
            res.append(f"KILLED   [{i}] {name}\n           " + "\n           ".join(fails))
    finally:
        for f, s in originals.items():
            open(f, 'w').write(s)
    print(res[-1], flush=True)
    if show:
        print(out, flush=True)
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)}")
sys.exit(1 if bad else 0)
