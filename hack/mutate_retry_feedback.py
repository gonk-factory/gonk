#!/usr/bin/env python3
"""mutate_retry_feedback.py -- "seen red" evidence for gonk-37kg part E.

Two things are covered here; the retry feedback of the fix agent and the
record's path through the store are mutate_pipefix_prompt.py's, and the
diagnosis block's own rendering is mutate_pipeline_prompt.py's.

  SETS   gonk-zxsx / gonk-37kg.1: a refused diagnosis's retry prompt states the
         failed job names and the fact keys, from what the gate reads.
  ISSUE  gonk-hpgy: a refused triage or scaffold answer leaves feedback, and
         the retry's prompt carries it.

Applies one named break at a time, runs the tests that guard it, and requires
them to FAIL; then restores the file byte for byte (try/finally). A mutation
whose text is not found is VACUOUS, one the tests do not notice SURVIVED, and
one that does not compile NOCOMPILE -- each is a failure of this script.

Usage: hack/mutate_retry_feedback.py            (all mutations)
       hack/mutate_retry_feedback.py 3 7        (only these indexes)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
env = dict(os.environ)
RF = 'cmd/gonk-gate/retry_feedback.go'
PP = 'cmd/gonk-gate/broker_pipeline_prompt.go'
BA = 'cmd/gonk-gate/broker_apply.go'
BI = 'cmd/gonk-gate/broker_inject.go'
DI = 'cmd/gonk-gate/dispatch.go'
DG = 'pkg/effects/diagnosis.go'
FB = 'pkg/effects/feedback.go'
GATE, EFF = './cmd/gonk-gate/', './pkg/effects/'
SETS = 'Diagnosis|PipelinePrompt|PipelineRefused'
ISSUE = 'Triage|Scaffold|IssueAgent|IssueGate|WhichAgents'
muts = [
    # ---- SETS: the diagnosis block's closed sets ------------------------------------
    ("sets: the diagnosis block states no closed set (the block is the common one again)", PP,
     "renderRetryFeedback(pc.Req.Refused, pc.Req.Attempt, diagnosisFeedbackSets(pc.Req.Refused, pc.DiagnosisContext())...)",
     "renderRetryFeedback(pc.Req.Refused, pc.Req.Attempt)", GATE, SETS),
    ("sets: the renderer drops the closed-set lines", RF,
     "\tfor _, line := range sets {\n\t\tb.WriteString(line + \"\\n\")\n\t}\n", "\t_ = sets\n", GATE, SETS),
    ("sets: the job names are left out", RF,
     "\tsets := []string{feedbackJobsLine(dc.JobNames())}\n", "\tsets := []string{}\n", GATE, SETS),
    ("sets: the fact keys are left out", RF,
     "\tif facts := effects.FactsText(); fb.Allowed != facts { // feedback:facts-once",
     "\tif facts := effects.FactsText(); false && fb.Allowed != facts { // feedback:facts-once", GATE, SETS),
    ("sets: the fact keys are said twice when the refusal is about them", RF,
     "\tif facts := effects.FactsText(); fb.Allowed != facts { // feedback:facts-once",
     "\tif facts := effects.FactsText(); true || fb.Allowed != facts { // feedback:facts-once", GATE, SETS),
    ("sets: THE GATE READS ANOTHER SET THAN THE BLOCK STATES (the gate drops the first job)", DG,
     "\tfor _, name := range ctx.JobNames() { // rule:evidence-job reads THIS set",
     "\tfor _, name := range ctx.JobNames()[min(1, len(ctx.FailedJobs)):] { // rule:evidence-job reads THIS set", GATE, SETS),
    ("sets: THE GATE READS ANOTHER SET THAN THE BLOCK STATES (the gate folds case)", DG,
     "\tfor _, name := range ctx.JobNames() { // rule:evidence-job reads THIS set\n\t\tjobs[name] = true",
     "\tfor _, name := range ctx.JobNames() { // rule:evidence-job reads THIS set\n\t\tjobs[strings.ToLower(name)] = true", GATE, SETS),
    ("sets: THE BLOCK STATES ANOTHER SET THAN THE GATE READS (the last job is not listed)", RF,
     "\tsets := []string{feedbackJobsLine(dc.JobNames())}\n",
     "\tsets := []string{feedbackJobsLine(dc.JobNames()[:max(0, len(dc.FailedJobs)-1)])}\n", GATE, SETS),
    ("sets: JobNames is not the context's names (one is altered)", DG,
     "\t\tout = append(out, j.Name) // jobs:names", "\t\tout = append(out, strings.TrimSpace(j.Name)+\"\") // jobs:names\n\t\tif len(out) == 1 {\n\t\t\tout[0] += \"x\"\n\t\t}", EFF, "JobNames"),
    ("sets: the gate's fact set loses a key the block lists", DG,
     "\tfor _, f := range factOrder {\n\t\tm[f] = true\n\t}", "\tfor _, f := range factOrder[1:] {\n\t\tm[f] = true\n\t}", GATE, SETS),
    ("sets: the gate's fact set loses a key the block lists (seen by pkg/effects)", DG,
     "\tfor _, f := range factOrder {\n\t\tm[f] = true\n\t}", "\tfor _, f := range factOrder[1:] {\n\t\tm[f] = true\n\t}", EFF, "JobNames|ListedClosedSets"),
    ("sets: EVERY NAME IS SHOWN, whatever it is made of (a job named like a sentence stands above the attestation)", RF,
     "\t\tif !feedbackJobToken(name[i]) { // feedback:job-listable", "\t\tif false && !feedbackJobToken(name[i]) { // feedback:job-listable", GATE, SETS),
    ("sets: a name with a SPACE is shown (a sentence can be made)", RF,
     "strings.IndexByte(\"-_.:/\", c) >= 0 // feedback:job-token", "strings.IndexByte(\"-_.:/ \", c) >= 0 // feedback:job-token", GATE, SETS),
    ("sets: a name with a comma or a semicolon is shown (the list's own separators)", RF,
     "strings.IndexByte(\"-_.:/\", c) >= 0 // feedback:job-token", "strings.IndexByte(\"-_.:/,;\", c) >= 0 // feedback:job-token", GATE, SETS),
    ("sets: a name holding a double quote or a backslash is shown", RF,
     "strings.IndexByte(\"-_.:/\", c) >= 0 // feedback:job-token", "strings.IndexByte(\"-_.:/\\\"\\\\\", c) >= 0 // feedback:job-token", GATE, SETS),
    ("sets: any printable byte is a token byte (the block's filter alone decides)", RF,
     "strings.IndexByte(\"-_.:/\", c) >= 0 // feedback:job-token", "c >= 0x20 && c <= 0x7e && c != '\"' && c != '\\\\' // feedback:job-token", GATE, SETS),
    ("sets: a byte above ASCII is a token byte", RF,
     "strings.IndexByte(\"-_.:/\", c) >= 0 // feedback:job-token", "strings.IndexByte(\"-_.:/\", c) >= 0 || c >= 0x80 // feedback:job-token", GATE, SETS),
    ("sets: a plain separator is not a token byte (`build:linux/amd64` is only counted)", RF,
     "strings.IndexByte(\"-_.:/\", c) >= 0 // feedback:job-token", "strings.IndexByte(\"-_.\", c) >= 0 // feedback:job-token", GATE, SETS),
    ("sets: the empty name is shown", RF,
     "\treturn name != \"\" // feedback:job-nonempty", "\treturn true // feedback:job-nonempty", GATE, SETS),
    ("sets: THE JOB LINE HAS NO CAP (every name is shown, whatever its size)", RF,
     "\t\tif !feedbackJobListable(n) || used+len(q)+len(\", \") > room { // feedback:job-fits",
     "\t\tif !feedbackJobListable(n) || used+len(q)+len(\", \") > room+(1<<20) { // feedback:job-fits", GATE, SETS),
    ("sets: the job line's cap is raised past the prompt's budget", RF,
     "\tretryFeedbackCapJobs    = 320", "\tretryFeedbackCapJobs    = 384", GATE, SETS),
    ("sets: names that are not shown are not counted", RF,
     "\tif more > 0 { // feedback:job-more", "\tif false && more > 0 { // feedback:job-more", GATE, SETS),
    ("sets: a name that does not fit ends the list (a later short one is not shown)", RF,
     "\t\t\tmore++\n\t\t\tcontinue\n", "\t\t\tmore += len(names) - len(shown) - more\n\t\t\tbreak\n", GATE, SETS),
    ("sets: the miscopied job name is replayed into the feedback", FB,
     "\t\tfb.What = evidenceAt() + \" names a job that is not one of the failed jobs, copied exactly from its heading\"",
     "\t\tfb.What = evidenceAt() + \" names a job that is not one of the failed jobs, copied exactly from its heading: \" + d.Evidence[max(item, 0)].Job", GATE, SETS),
    # ---- ISSUE: triage and scaffold ---------------------------------------------------
    ("issue: triage and scaffold get no feedback (the unwired state)", RF,
     "\tcase triageAgent: // TestIssueAgentFeedbackKindsAreTheirShapes ties both to the shape files\n\t\treturn []effects.Kind{effects.KindComment, effects.KindLabel}\n\tcase scaffoldAgent:\n\t\treturn []effects.Kind{effects.KindFile}\n",
     "", GATE, ISSUE),
    ("issue: a parse refusal names kinds the triage shape does not take", RF,
     "\t\treturn []effects.Kind{effects.KindComment, effects.KindLabel}\n", "\t\treturn []effects.Kind{effects.KindComment, effects.KindLabel, effects.KindFile}\n", GATE, ISSUE),
    ("issue: a gate refusal records nothing", BA,
     "\t\tif fbKinds != nil { // feedback:issue-gate\n\t\t\tfb.Code = gate\n\t\t\trecordRetryFeedback(recp, fb)\n\t\t}\n", "\t\t_ = fb\n", GATE, ISSUE),
    ("issue: THE GATE'S ERROR TEXT -- which quotes the batch -- is the feedback", BA,
     "\t\t\tfb.Code = gate\n\t\t\trecordRetryFeedback(recp, fb)\n", "\t\t\trecordRetryFeedback(recp, effects.Feedback{Code: gate, What: detail})\n", GATE, ISSUE),
    ("issue: the feedback's code is not the gate", BA,
     "\t\t\tfb.Code = gate\n\t\t\trecordRetryFeedback(recp, fb)\n", "\t\t\trecordRetryFeedback(recp, fb)\n", GATE, ISSUE),
    ("issue: an applied answer leaves the earlier refusal's feedback on the record", BA,
     "\trecp.RetryFeedback = beadstore.RetryFeedback{} // feedback:issue-clear\n", "", GATE, ISSUE),
    ("issue: the feedback points at a heading the triage prompt does not have", RF,
     "\treturn strings.ReplaceAll(what, \" shown under OUTPUT\", \" this message shows\") // feedback:issue-wording",
     "\treturn what // feedback:issue-wording", GATE, ISSUE),
    ("issue: dispatch does not carry the feedback onto the record it writes", RF,
     "\t\tbase.RetryFeedback = prior.RetryFeedback // dispatch:carry-issue-feedback\n", "", GATE, ISSUE),
    ("issue: dispatch carries the feedback for no issue-shaped agent", DI,
     "\tif ag := agentForTrigger[a.Trigger]; ag == triageAgent || ag == scaffoldAgent {", "\tif ag := agentForTrigger[a.Trigger]; false && (ag == triageAgent || ag == scaffoldAgent) {", GATE, ISSUE),
    ("issue: the triage prompt does not carry the block", BI,
     "prompt = renderTriagePrompt(a.Project, a.IssueIID, agentContext, issueContext, checkout, sha) + retryFeedbackSuffix(base)",
     "prompt = renderTriagePrompt(a.Project, a.IssueIID, agentContext, issueContext, checkout, sha)", GATE, ISSUE),
    ("issue: the scaffold prompt for a granted checkout does not carry the block", BI,
     "renderScaffoldCheckoutPrompt(a.Project, url)+retryFeedbackSuffix(base), url, sha", "renderScaffoldCheckoutPrompt(a.Project, url), url, sha", GATE, ISSUE),
    ("issue: the scaffold dispatch renders the material prompt without the record's feedback", BI,
     "\t\tprompt = scaffoldPromptFor(base, a.Project, repoContext)\n", "\t\tprompt = renderScaffoldPrompt(a.Project, repoContext)\n", GATE, ISSUE),
    ("issue: the block is rendered into a prompt for no attempt", RF,
     "\tif fb := renderRetryFeedback(rec.RetryFeedback, rec.Attempt); fb != \"\" { // prompt:issue-refused",
     "\tif fb := renderRetryFeedback(rec.RetryFeedback, rec.Attempt); false && fb != \"\" { // prompt:issue-refused", GATE, ISSUE),
    ("issue: the scaffold prompt that asks for NO batch carries 'answer again in full'", RF,
     "\tif strings.TrimSpace(repoContext) == \"\" { // prompt:scaffold-no-material", "\tif false && strings.TrimSpace(repoContext) == \"\" { // prompt:scaffold-no-material", GATE, ISSUE),
    ("issue: the scaffold prompt built from material does not carry the block", RF,
     "\treturn p + retryFeedbackSuffix(rec)\n", "\treturn p\n", GATE, ISSUE),
    ("issue: the path gate's sentence is gone (falls to an empty one, which records nothing)", RF,
     "\tissueGatePath:       \"a file effect names a path this task may not write, or holds more than a file may\",\n", "", GATE, ISSUE),
    ("issue: an unknown key in a kind with no key table is followed by an empty 'allowed' heading", FB,
     "\tif len(parts) == 0 { // feedback:no-key-table", "\tif false && len(parts) == 0 { // feedback:no-key-table", GATE, ISSUE),
    ("issue: a triage batch goes down a pipeline agent's chain", RF,
     "func ownChainAgent(agent string) bool { return agent == pipelineAgent || agent == pipefixAgent }",
     "func ownChainAgent(agent string) bool { return agent != scaffoldAgent }", GATE, ISSUE),
]
only = sys.argv[1:]
res = []
for i, (name, f, old, new, pkg, run) in enumerate(muts):
    if only and str(i) not in only:
        continue
    src = open(f).read()
    if old not in src:
        res.append(f"VACUOUS  {i} {name}")
        continue
    open(f, 'w').write(src.replace(old, new, 1))
    try:
        r = subprocess.run([GO, "test", pkg, "-run", run, "-count=1"], capture_output=True, text=True, env=env)
        out = r.stdout + r.stderr
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED {i} {name}")
        elif not fails:
            res.append(f"NOCOMPILE {i} {name}\n{out[:800]}")
        else:
            res.append(f"KILLED   {i} {name}\n           " + "\n           ".join(fails))
    finally:
        open(f, 'w').write(src)
print("\n".join(res))
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)}")
sys.exit(1 if bad else 0)
