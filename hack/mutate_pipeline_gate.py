#!/usr/bin/env python3
"""mutate_pipeline_gate.py -- "seen red" evidence for gonk-tios P7.

Applies one named break at a time to gonk-gate's pipeline path (dispatch,
validation chain, landing, run maintenance), runs the tests that guard it, and
requires them to FAIL; then restores every file byte for byte (try/finally).
A mutation whose text is not found is VACUOUS, one the tests do not notice
SURVIVED, and one that does not compile NOCOMPILE -- each is a failure of this
script. A mutation may edit several places (a list of edits), all applied.

Usage: hack/mutate_pipeline_gate.py            (all mutations)
       hack/mutate_pipeline_gate.py 3 7        (only these indexes)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate
(gonk-5ed1); its output is pasted into the plan's evidence log.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
G = 'cmd/gonk-gate/'
AP = G + 'broker_pipeline_apply.go'
DP = G + 'broker_pipeline_dispatch.go'
LD = G + 'pipeline_landing.go'
SW = G + 'sweep_pipeline.go'
INJ = G + 'broker_inject.go'
DIS = G + 'dispatch.go'
SWP = G + 'sweep.go'
BA = G + 'broker_apply.go'
TR = G + 'broker_trajectory.go'
RUN = ('Pipeline|D8|Landing|Lands|Target|RunNote|Hallucinated|ExcerptMismatch|ThroughTheSweep|ClosedRun|Slash|'
       'Green|SameFailure|Supersedes|FiveMinutes|ParkedRun|ClosedMR|NoDiagnosing|NoSession|RunSweepMaintains|CannotWrite|GrantCheckoutAt|LabelFailure|'
       'OddWhitespace|FittedExcerptCap|OwnLastSuccess|Diagnosed|EvidenceJob|GonkMarker|'
       # gonk-2usw FACTS (cmd/gonk-gate/pipeline_facts_test.go, pkg/beadstore/pipeline_facts_test.go)
       'FailureFacts|LandedDiagnosis|RecordedSignature|AllowedFailure|RecordedDiagnosis|DidNotLand|RefusedDiagnosis|GenericFailure|'
       # gonk-2usw EPOCH (cmd/gonk-gate/broker_pipeline_fixepoch_test.go, pkg/beadstore/pipeline_fixepoch_test.go)
       'FixEpoch|WithoutAFix')
CTX = G + 'broker_pipeline_context.go'
MEM = 'pkg/beadstore/memory.go'
LK = G + 'pipeline_links.go'
ST = 'pkg/beadstore/store.go'
REC = '\trecp.PipelineDiagnosis = landedDiagnosis(diag)\n'

# (name, [(file, old, new), ...])
muts = [
 # ---- the hallucination check (D8 step 6) ----
 ("grounding: an ungrounded line is accepted",
  [(AP, "\t\tif !grounded {\n", "\t\tif false && !grounded {\n")]),
 ("grounding: a line may come from ANY job's excerpt",
  [(AP, "\t\texcerpts[j.Name] = append(excerpts[j.Name], ex.Text)",
        "\t\tfor _, jj := range rec.PipelineJobs {\n\t\t\texcerpts[jj.Name] = append(excerpts[jj.Name], ex.Text)\n\t\t}")]),
 # ---- context integrity (D8 step 5) ----
 ("integrity: the recorded hash is never compared",
  [(AP, "ex, verr := pipelinectx.VerifyExcerpt(tt.Data, pipelineExcerptOptions(tt, rec.PipelineExcerptMaxBytes),\n\t\t\trec.PipelineExcerptVersion, j.ExcerptSHA256)",
        "ex, verr := pipelinectx.ExcerptWith(tt.Data, pipelineExcerptOptions(tt, rec.PipelineExcerptMaxBytes)), error(nil)")]),
 ("integrity: a changed context is gate-failed, not UNKNOWN",
  [(AP, 'return unknown(5, causeContextChanged, fmt.Errorf("job %q: %w", j.Name, verr))',
        'return refuse(5, causeContextChanged, verr.Error(), effects.Feedback{What: "context changed"})')]),
 ("integrity: the cap is re-derived from the job list, not the recorded one",
  [(AP, "pipelineExcerptOptions(tt, rec.PipelineExcerptMaxBytes),", "pipelineExcerptOptions(tt, pipelinectx.JobBudget(len(rec.PipelineJobs))),")]),
 ("integrity: the trace is re-read with another fetch (small maxRead)",
  [(AP, "tt, ferr := fetchPipelineTrace(ctx, d.Pipelines, rec.ProjectID, j.ID)",
        "tt, ferr := d.Pipelines.GetJobTraceTail(ctx, rec.ProjectID, j.ID, 1<<10, pipelineTraceTail)")]),
 # ---- citations (D8 step 7, P5) ----
 ("citations: existence never checked",
  [(AP, "\tfor fi, f := range diag.Files {\n\t\t// A 1-byte read", "\tfor fi, f := range diag.Files[:0] {\n\t\t// A 1-byte read")]),
 ("citations: checked on the default branch, not the pipeline's commit",
  [(AP, "GetRawFile(ctx, rec.ProjectID, f, rec.PipelineSHA, 1)", 'GetRawFile(ctx, rec.ProjectID, f, "main", 1)')]),
 ("citations: the pipeline forge gains a repository write",
  [(LD, "\tCloseIssue(ctx context.Context, projectID, issueIID int64) error\n}",
        "\tCloseIssue(ctx context.Context, projectID, issueIID int64) error\n\tCreateCommit(ctx context.Context, projectID int64, opts glab.CommitOptions) (*glab.Commit, error)\n}")]),
 ("routing: the pipeline batch takes the issue path (routing line removed)",
  [(BA, "\tif fbKinds != nil { // apply:own-chain", "\tif fbKinds != nil && agent != pipelineAgent { // apply:own-chain")]),
 # ---- what is posted (D8 step 8) ----
 ("secret: the rendered note is never redact-checked",
  [(AP, "if _, kinds := pipelinectx.Redact(body); len(kinds) > 0 {", "if kinds := []string(nil); len(kinds) > 0 {")]),
 ("marker: forged gonk markers are not refused",
  [(AP, "\t\tif gonkMarkerRe.MatchString(s) {", "\t\tif false && gonkMarkerRe.MatchString(s) {")]),
 ("marker B14: quoted evidence lines are not marker-checked",
  [(AP, "\t\tout = append(out, ev.Job, ev.Line, string(ev.Fact))", "\t\tout = append(out, ev.Job, string(ev.Fact))")]),
 ("marker: only run markers refused (a body fence passes)",
  [(AP, "regexp.MustCompile(`(?i)<!--\\s*/?\\s*gonk`)", "regexp.MustCompile(`<!-- gonk:pipeline-run:`)")]),
 ("quick action: the comment gate is not run",
  [(AP, "effects.ValidateComments(effects.Batch{Effects: []effects.Effect{{Kind: effects.KindComment, Body: body}}})",
        "effects.ValidateComments(effects.Batch{})")]),
 ("rendering: evidence lines are not bullets (a log line may begin with /)",
  [(LD, 'fmt.Fprintf(&b, "- %s: %s\\n", job, codeSpan(pipelinectx.Norm(ev.Line)))',
        'fmt.Fprintf(&b, "%s\\n%s\\n", job, pipelinectx.Norm(ev.Line))')]),
 # ---- target / identity / project isolation ----
 ("target: the allowed pipeline set is taken from the batch",
  [(AP, "effects.Targets{Pipelines: map[int64]bool{rec.PipelineID: true}}",
        "effects.Targets{Pipelines: map[int64]bool{rec.PipelineID: true, batch.Effects[0].TargetIID: true}}")]),
 ("identity: a pipeline order is never checked against its anchor",
  [(DIS, "\t\tif err := checkPipelineIdentity(a); err != nil {", "\t\tif err := error(nil); err != nil {")]),
 ("identity: the anchor comparison is skipped",
  [(DP, "\tif want != a.BeadAnchor {", "\tif false && want != a.BeadAnchor {")]),
 ("identity: the session takes the order's pipeline, not the recorded one",
  [(DP, "\t\tbase.PipelineID = prior.PipelineID\n", "\t\t_ = prior.PipelineID\n")]),
 ("refire: the run identity is not carried",
  [(SWP, '\t\tvars["pipeline_id"] = fmt.Sprint(rec.PipelineID)\n', '\t\t_ = rec.PipelineID\n')]),
 # ---- dispatch ----
 ("checkout: granted at the default branch head, not the pipeline's commit",
  [(DP, "grantCheckoutAt(ctx, d, pipelineAgent, session, pc.Pipeline.SHA)", "grantCheckout(ctx, d, pipelineAgent, session)")]),
 ("checkout: a non-SHA commit is accepted",
  [(INJ, "\tif !glab.IsCommitSHA(sha) {\n\t\treturn \"\", \"\", fmt.Errorf(\"pin checkout: %q is not a full commit SHA\", sha)",
         "\tif false {\n\t\treturn \"\", \"\", fmt.Errorf(\"pin checkout: %q is not a full commit SHA\", sha)")]),
 ("record: excerpt hashes not recorded",
  [(DP, "\trec.PipelineJobs = p.jobs\n", "\t_ = p.jobs\n")]),
 ("record B8: the budget recorded instead of the cap the fit actually used",
  [(DP, "\tout.maxBytes = pc.ExcerptMaxBytes\n", "\tout.maxBytes = pipelinectx.JobBudget(pc.excerptedCount())\n")]),
 ("record R9: the raw job name recorded, not the one the prompt shows",
  [(CTX, "Name: jobNameText(fj.Job.Name),", "Name: fj.Job.Name,")]),
 ("record: the excerpt cap not recorded",
  [(DP, "\trec.PipelineExcerptMaxBytes = p.maxBytes\n", "\trec.PipelineExcerptMaxBytes = 0\n")]),
 ("session: named as an issue session",
  [(INJ, "name = runtime.Name(agent, a.ProjectID, runtime.KindPipeline, base.PipelineID, dec.Attempt)",
         "name = runtime.Name(agent, a.ProjectID, runtime.KindIssue, base.PipelineID, dec.Attempt)")]),
 ("lifecycle: diagnosing note written before the session and its record",
  [(INJ, "\t\tbase = pp.apply(base)\n",
         "\t\tbase = pp.apply(base)\n\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDiagnosing, Status: \"early\"})\n")]),
 ("lifecycle: deferred note written before the park record",
  [(DIS, "\t\tbase.State = beadstore.StateParked\n\t\tbase.RetryAfter = dec.RetryAfter\n",
         "\t\tbase.State = beadstore.StateParked\n\t\tbase.RetryAfter = dec.RetryAfter\n\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDeferred, Status: \"early\"})\n")]),
 ("lifecycle: a pipeline defer goes to the issue status note",
  [(DIS, "\t\tif base.PipelineID != 0 { // the run note, never the issue status note\n\t\t\tbase = d.withPipelineProjectURL(ctx, base) // for the note's link only (gonk-385g)\n\t\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDeferred,",
         "\t\tif false { // the run note, never the issue status note\n\t\t\tbase = d.withPipelineProjectURL(ctx, base) // for the note's link only (gonk-385g)\n\t\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDeferred,")]),
 ("lifecycle: a pipeline deny goes to the issue status note",
  [(DIS, "\t\tif base.PipelineID != 0 { // the run note, never the issue status note\n\t\t\tbase = d.withPipelineProjectURL(ctx, base) // for the note's link only (gonk-385g)\n\t\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDenied,",
         "\t\tif false { // the run note, never the issue status note\n\t\t\tbase = d.withPipelineProjectURL(ctx, base) // for the note's link only (gonk-385g)\n\t\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDenied,")]),
 ("lifecycle: the re-sling notice is not limited to triage",
  [(SWP, 'agentForTrigger[rec.Trigger] != "triage" || rec.ProjectID == 0 || rec.IssueIID == 0',
         'rec.ProjectID == 0')]),
 # ---- landing (D9) ----
 ("marker: written in a format intake does not read",
  [(LD, "\tmarker, err := pipelinectx.RunMarker(anchor, n.State)\n",
        "\tmarker, err := \"<!-- gonk:pipeline-run:\"+anchor+\" state: \"+n.State+\" -->\", error(nil)\n")]),
 ("landing: notes by anyone count as the bot's",
  [(LD, "\treturn !n.System && n.Author.ID == l.me.ID\n", "\treturn !n.System\n")]),
 ("landing: a human-authored issue may be adopted as the tracking issue",
  [(LD, "glab.IssueListOptions{State: \"all\", AuthorID: l.me.ID}", "glab.IssueListOptions{State: \"all\"}"),
   (LD, "\t\tif is.Author.ID == l.me.ID && strings.Contains(is.Description, marker) {", "\t\tif strings.Contains(is.Description, marker) {")]),
 ("landing: the run note is never found again (a second note per apply)",
  [(LD, "\t\tld.note, ld.state = l.runNoteIn(notes, rec.BeadAnchor)\n\t\treturn ld, nil", "\t\t_ = notes\n\t\treturn ld, nil")]),
 ("landing: an open tracking issue is never reused (a second issue)",
  [(LD, "\t\tif issues[i].State == \"opened\" {", "\t\tif false {")]),
 ("landing: other class labels are not removed",
  [(LD, "add, remove = []string{pipelineClassLabel(class)}, classLabelsExcept(class)", "add, remove = []string{pipelineClassLabel(class)}, nil")]),
 ("landing: a closed run's note can be reopened",
  [(LD, "\tif ld.note != nil && terminalRunState(ld.state) && !terminalRunState(n.State) {", "\tif false {")]),
 ("trajectory: keyed on the verdict, not the classification",
  [(TR, "\tif agent == pipelineAgent {\n\t\tfor _, e := range batch.Effects {", "\tif false {\n\t\tfor _, e := range batch.Effects {")]),
 # ---- run maintenance ----
 ("maintenance: not called from runSweep",
  [(SWP, "\tsweepPipelineRuns(ctx, dd)\n", "")]),
 ("maintenance: resolution does not close the tracking issue",
  [(SW, 'state == pipelinectx.RunStateResolved && t.Kind == pipelinectx.TargetKindRef && ld.issue != nil && ld.issue.State == "opened"', "false")]),
 ("maintenance: the run's own last success counts as green",
  [(SW, "\t\tif newest.ID > rec.PipelineLastSuccess {", "\t\tif true {")]),
 # verify report R5 / B27 / R6
 ("maintenance R5: any green resolves the run, not only the newest terminal pipeline",
  [(SW, "\t\tif newest == nil || p.ID > newest.ID {", "\t\tif newest == nil || p.ID > newest.ID || p.Status == \"success\" {")]),
 ("maintenance R5: the OLDEST terminal pipeline decides",
  [(SW, "\t\tif newest == nil || p.ID > newest.ID {", "\t\tif newest == nil || p.ID < newest.ID {")]),
 ("maintenance B27: child pipelines are not passed over",
  [(SW, ' || p.Source == "parent_pipeline" {', ' {')]),
 ("maintenance R6: every done record is listed and decoded every tick",
  [(SW, "\trecs, err := d.Store.ListOpenPipelineRuns(ctx)", "\trecs, err := d.Store.List(ctx, beadstore.StateDone)")]),
 ("maintenance R6: the store hands back closed runs too",
  [(MEM, "\t\tif r.PipelineRunOpen {\n\t\t\tout = append(out, r)", "\t\tif true {\n\t\t\tout = append(out, r)")]),
 ("maintenance: a different failure is not a new run",
  [(SW, "\t\tif fp != rec.PipelineFingerprint {", "\t\tif false && fp != rec.PipelineFingerprint {")]),
 ("maintenance: looks every tick",
  [(SW, "if !rec.PipelineCheckedAt.IsZero() && now.Sub(rec.PipelineCheckedAt) < pipelineRunCheckEvery {", "if false {")]),
 ("maintenance: a resolved parked run is unparked later",
  [(SW, "\t\t\trec.State = beadstore.StateDone // never unpark", "\t\t\t_ = 0 // never unpark")]),
 ("maintenance: running runs are maintained too",
  [(SW, "beadstore.StateDone: true, beadstore.StateNeedsHuman: true, beadstore.StateParked: true,",
        "beadstore.StateDone: true, beadstore.StateNeedsHuman: true, beadstore.StateParked: true, beadstore.StateRunning: true,")]),
 ("maintenance: the latest failing pipeline is not recorded",
  [(SW, "\t\trec.PipelineLatestFailed = obs.failed.ID\n", "\t\t_ = obs.failed.ID\n")]),
 # ---- links and the diagnosed heading (gonk-385g) ----
 ("links: a pipeline is named in plain text, never linked",
  [(LK, 'base != "" {\n\t\treturn fmt.Sprintf("[%s](%s/-/pipelines/%d)", text, base, id)',
        'base != "" && false {\n\t\treturn fmt.Sprintf("[%s](%s/-/pipelines/%d)", text, base, id)')]),
 ("links: the record's project URL is linked without being checked at render",
  [(LK, 'if base := safeProjectURL(rec.PipelineProjectURL); base != "" {\n\t\treturn fmt.Sprintf("[%s](%s/-/pipelines/%d)"',
        'if base := rec.PipelineProjectURL; base != "" {\n\t\treturn fmt.Sprintf("[%s](%s/-/pipelines/%d)"')]),
 ("links: a project URL may hold any character",
  [(LK, '\t\tdefault:\n\t\t\treturn ""\n', '\t\tdefault:\n')]),
 ("links: a project URL may have any scheme",
  [(LK, '(p.Scheme != "http" && p.Scheme != "https") || ', '')]),
 ("links: a project URL needs no host",
  [(LK, ' || p.Host == "" {', ' {')]),
 ("links: a project URL may end in a slash",
  [(LK, 'if u == "" || strings.HasSuffix(u, "/") {', 'if u == "" {')]),
 ("links: the project URL is cut from ANY pipeline's URL (the id is not matched)",
  [(LK, 'base, ok := strings.CutSuffix(p.WebURL, fmt.Sprintf("/-/pipelines/%d", p.ID))',
        'base, _, ok := strings.Cut(p.WebURL, "/-/pipelines/")')]),
 ("links: the derived project URL is not safety-checked",
  [(LK, '\treturn safeProjectURL(base)\n', '\treturn base\n')]),
 ("links: a job link is put on a line of its own (a rendered line begins with /)",
  [(LK, 'return fmt.Sprintf("[%s](%s/-/jobs/%d)", text, base, id)', 'return fmt.Sprintf("%[1]s\\n/-/jobs/%[3]d %[2]s", text, base, id)')]),
 ("record: the project URL is not recorded at dispatch",
  [(DP, "\tout.projectURL = projectWebURLOf(pc.Pipeline)\n", "\tout.projectURL = \"\"\n")]),
 ("record: the project URL is not carried to a later dispatch of the run",
  [(DP, "\t\tbase.PipelineProjectURL = prior.PipelineProjectURL\n", "\t\t_ = prior.PipelineProjectURL\n")]),
 ("lifecycle: a first deferred note does not link (the pipeline is not read)",
  [(DIS, "\t\t\tbase = d.withPipelineProjectURL(ctx, base) // for the note's link only (gonk-385g)\n\t\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDeferred,",
         "\t\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDeferred,")]),
 ("lifecycle: a first denied note does not link (the pipeline is not read)",
  [(DIS, "\t\t\tbase = d.withPipelineProjectURL(ctx, base) // for the note's link only (gonk-385g)\n\t\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDenied,",
         "\t\t\td.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDenied,")]),
 ("lifecycle: the pipeline is read for the link BEFORE the park record is written",
  [(DIS, "\t\tbase.State = beadstore.StateParked\n\t\tbase.RetryAfter = dec.RetryAfter\n",
         "\t\tbase.State = beadstore.StateParked\n\t\tbase.RetryAfter = dec.RetryAfter\n\t\tbase = d.withPipelineProjectURL(ctx, base)\n")]),
 ("lifecycle: the pipeline is read for the link BEFORE the deny record is written",
  [(DIS, "\t\tbase.State = beadstore.StateNeedsHuman\n", "\t\tbase.State = beadstore.StateNeedsHuman\n\t\tbase = d.withPipelineProjectURL(ctx, base)\n")]),
 ("heading: a diagnosed note does not lead with its subject",
  [(LD, '\tb.WriteString(diagnosedHeading(rec) + "\\n\\n")\n', '\tb.WriteString("gonk diagnosed this failure.\\n\\n")\n')]),
 ("heading: the commit is left out",
  [(LK, 'pipelineRef(rec, rec.PipelineID), codeSpan(shortSHA(rec.PipelineSHA)))\n\tjobs := rec.PipelineJobs',
        'pipelineRef(rec, rec.PipelineID), codeSpan("commit"))\n\tjobs := rec.PipelineJobs')]),
 ("heading: every failed job is listed (unbounded)",
  [(LK, "\tshown := min(len(jobs), headingJobs)\n", "\tshown := len(jobs)\n")]),
 ("heading: the jobs not named are not counted",
  [(LK, "\tif more := len(jobs) - shown; more > 0 {", "\tif more := len(jobs) - shown; false {")]),
 ("heading: a long job name is not cut",
  [(LK, "len(r) > headingJobNameRunes {", "false && len(r) > headingJobNameRunes {")]),
 ("heading: a job name that reads as a gonk marker is written",
  [(LK, ' || gonkMarkerRe.MatchString(j.Name) || len(secret) > 0 {', ' || len(secret) > 0 {')]),
 ("heading: a job name the redactor flags is written",
  [(LK, ' || gonkMarkerRe.MatchString(j.Name) || len(secret) > 0 {', ' || gonkMarkerRe.MatchString(j.Name) {\n\t\t_ = secret')]),
 ("evidence: a job is never linked",
  [(LK, "\treturn jobLink(rec, found.ID, codeSpan(found.Name))\n", "\treturn codeSpan(found.Name)\n")]),
 ("evidence: an ambiguous job name links to one of the jobs",
  [(LK, "\t\tif found != nil {\n\t\t\treturn codeSpan(name)\n\t\t}\n", "\t\tif found != nil {\n\t\t\tbreak\n\t\t}\n")]),
 ("evidence: a job with no id is linked",
  [(LK, 'base != "" && id > 0 {', 'base != "" {')]),
 ("evidence: the job link's project URL is not checked at render",
  [(LK, 'if base := safeProjectURL(rec.PipelineProjectURL); base != "" && id > 0 {', 'if base := rec.PipelineProjectURL; base != "" && id > 0 {')]),
 ("resolved: the note no longer says which pipeline it is about",
  [(LD, "at, pipelineRef(rec, rec.PipelineID))", 'at, "this run")')]),
 ("superseded: the note no longer says which pipeline it is about",
  [(LD, "pipelineRef(rec, next), pipelineRef(rec, rec.PipelineID))", 'pipelineRef(rec, next), "the earlier one")')]),
 ("description: the latest failing pipeline is named in plain text",
  [(LD, '"| Latest failing pipeline | %s |\\n", pipelineRef(rec, latest))', '"| Latest failing pipeline | pipeline %d |\\n", latest)')]),
 ("maintenance: an old record never learns its project URL",
  [(SW, '\tif rec.PipelineProjectURL == "" {\n\t\tfor _, p := range', '\tif false {\n\t\tfor _, p := range')]),
 # ---- gonk-2usw FACTS: the per-job failure facts dispatch records ----
 ("facts: the job's stage is not recorded",
  [(CTX, "Stage: fj.Job.Stage, FailureReason: fj.Job.FailureReason,", "FailureReason: fj.Job.FailureReason,")]),
 ("facts: the job's failure_reason is not recorded",
  [(CTX, "Stage: fj.Job.Stage, FailureReason: fj.Job.FailureReason,", "Stage: fj.Job.Stage,")]),
 ("facts: stage and failure_reason swapped",
  [(CTX, "Stage: fj.Job.Stage, FailureReason: fj.Job.FailureReason,", "Stage: fj.Job.FailureReason, FailureReason: fj.Job.Stage,")]),
 ("facts: allow_failure recorded inverted",
  [(CTX, "AllowFailure: fj.Job.AllowFailure}", "AllowFailure: !fj.Job.AllowFailure}")]),
 ("facts: the error signature is not recorded (the plan's F1)",
  [(CTX, "firstError, r.ErrorSignature = fj.Excerpt.FirstError, fj.Excerpt.ErrorSignature", "firstError = fj.Excerpt.FirstError")]),
 ("facts: the signature is of the excerpt's hash, not its first error line",
  [(CTX, "firstError, r.ErrorSignature = fj.Excerpt.FirstError, fj.Excerpt.ErrorSignature",
         "firstError, r.ErrorSignature = fj.Excerpt.FirstError, pipelinectx.ErrorSignature(fj.Excerpt.SHA256)")]),
 ("facts: the runner's summary is never flagged generic",
  [(CTX, "r.ErrorGeneric = pipelinectx.GenericFirstError(firstError)", "r.ErrorGeneric = false && pipelinectx.GenericFirstError(firstError)")]),
 ("facts: a job with no signature is not flagged generic",
  [(CTX, "r.ErrorGeneric = pipelinectx.GenericFirstError(firstError)", "r.ErrorGeneric = pipelinectx.GenericErrorLine(firstError)")]),
 ("facts: every job is flagged generic",
  [(CTX, "r.ErrorGeneric = pipelinectx.GenericFirstError(firstError)", "r.ErrorGeneric = true || pipelinectx.GenericFirstError(firstError)")]),
 ("facts: dispatch drops stage/failure_reason/allow_failure on the way to the record",
  [(DP, "\t\t\tStage: j.Stage, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure,\n", "")]),
 ("facts: dispatch drops the signature on the way to the record",
  [(DP, "ErrorSignature: j.ErrorSignature, ErrorGeneric: j.ErrorGeneric,", "ErrorGeneric: j.ErrorGeneric,")]),
 ("facts: dispatch drops the generic flag on the way to the record",
  [(DP, "ErrorSignature: j.ErrorSignature, ErrorGeneric: j.ErrorGeneric,", "ErrorSignature: j.ErrorSignature,")]),
 ("facts: SignatureVersion is not recorded",
  [(DP, "\t\t\tSignatureVersion: pipelinectx.SignatureVersion,\n", "")]),
 ("facts: GenericFailure trusts the flag alone (an unsigned old job reads as specific)",
  [(ST, 'return j.ErrorSignature == "" || j.ErrorGeneric', "return j.ErrorGeneric")]),
 ("facts: FailureFactsRecorded is always true",
  [(ST, "return j.SignatureVersion != 0", "return true")]),
 # ---- gonk-2usw FACTS: the landed diagnosis on the record ----
 ("diagnosis: not recorded at apply (the plan's F2)",
  [(AP, REC, "")]),
 ("diagnosis: recorded before validation (a refused batch records)",
  [(AP, "\t// 4. The diagnosis's own rules", REC + "\t// 4. The diagnosis's own rules")]),
 ("diagnosis: recorded before the landing (an unwritten note still records)",
  [(AP, REC, ""), (AP, "\tlander, lerr := newPipelineLander(", REC + "\tlander, lerr := newPipelineLander(")]),
 ("diagnosis: Recorded() is always true",
  [(ST, 'return d.Class != ""', "return true")]),
 ("diagnosis: the class is not recorded",
  [(AP, "Class:   string(e.Classification),", 'Class:   "",')]),
 ("diagnosis: files are not recorded",
  [(AP, "Files:   append([]string(nil), e.Files...),", "Files:   nil,")]),
 ("diagnosis: the recorded files alias the batch",
  [(AP, "Files:   append([]string(nil), e.Files...),", "Files:   e.Files,")]),
 ("diagnosis: evidence jobs are not de-duplicated",
  [(AP, 'if ev.Job == "" || seen[ev.Job] {', 'if ev.Job == "" {')]),
 ("diagnosis: a pipeline-level evidence item is recorded as a job",
  [(AP, 'if ev.Job == "" || seen[ev.Job] {', "if seen[ev.Job] {")]),
 ("diagnosis: evidence jobs are not recorded",
  [(AP, "\t\td.EvidenceJobs = append(d.EvidenceJobs, ev.Job)\n", "")]),
 ("diagnosis: the summary recorded is the remedy",
  [(AP, 'Summary: strings.ReplaceAll(e.Summary, "\\x00", ""),', 'Summary: strings.ReplaceAll(e.Remedy, "\\x00", ""),')]),
 ("diagnosis: a NUL is kept in the summary (the record is unwritable in JSONB)",
  [(AP, 'Summary: strings.ReplaceAll(e.Summary, "\\x00", ""),', "Summary: e.Summary,")]),
 ("diagnosis: a NUL is kept in the remedy",
  [(AP, 'Remedy:  strings.ReplaceAll(e.Remedy, "\\x00", ""),', "Remedy:  e.Remedy,")]),
 ("diagnosis: the sweep applies on a copy, so its record write never carries it",
  [(SWP, "applyBrokerBatch(ctx, d, agent, &rec, *judged)", "applyBrokerBatch(ctx, d, agent, func() *beadstore.Record { c := rec; return &c }(), *judged)")]),
 ("diagnosis: the broker hands the pipeline path a copy",
  [(BA, "\t\tapplied, violation, err = apply(ctx, d, recp, batch)\n", "\t\tapplied, violation, err = apply(ctx, d, &rec, batch)\n")]),
 # ---- the fix epoch (gonk-2usw EPOCH, plan 3.3) ----
 ("epoch: the identity check ignores the fix epoch",
  [(DP, 'a.PipelineFingerprint, a.PipelineLastSuccess, a.PipelineFixEpoch)', 'a.PipelineFingerprint, a.PipelineLastSuccess, 0)')]),
 ("epoch: dispatchArgsFrom does not read pipeline_fix_epoch",
  [(DIS, '\t\tPipelineFixEpoch:    i64("pipeline_fix_epoch"),', '\t\tPipelineFixEpoch:    0,')]),
 ("epoch: the record drops the fix epoch",
  [(DP, '\tbase.PipelineFixEpoch = a.PipelineFixEpoch\n', '')]),
 ("epoch: a re-fire drops pipeline_fix_epoch",
  [(SWP, '\t\tif rec.PipelineFixEpoch != 0 {', '\t\tif false {')]),
 ("epoch: a re-fire of a run with no fix sends pipeline_fix_epoch=0",
  [(SWP, '\t\tif rec.PipelineFixEpoch != 0 {', '\t\tif true {')]),
 ("epoch: PipelineFixEpoch not omitempty (the record of a run with no fix changes)",
  [(ST, '`json:"PipelineFixEpoch,omitempty"`', '`json:"PipelineFixEpoch"`')]),
 ("epoch: the order does not declare pipeline_fix_epoch",
  [('pack/orders/gonk-dispatch.toml', 'pipeline_fix_epoch = { required = false }\n', '')]),
 ("epoch: the order declares pipeline_fix_epoch required",
  [('pack/orders/gonk-dispatch.toml', 'pipeline_fix_epoch = { required = false }', 'pipeline_fix_epoch = { required = true }')]),
]

only = sys.argv[1:]
res = []
for i, (name, edits) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals = {}
    vacuous = False
    for f, _, _ in edits:
        if f not in originals:
            originals[f] = open(f).read()
    cur = dict(originals)
    for f, old, new in edits:
        if old not in cur[f]:
            vacuous = True
            break
        cur[f] = cur[f].replace(old, new, 1)
    if vacuous:
        res.append(f"VACUOUS  [{i}] {name}")
        print(res[-1], flush=True)
        continue
    try:
        for f, s in cur.items():
            open(f, 'w').write(s)
        # ./pkg/beadstore/ too: the record type's own methods are mutated above.
        r = subprocess.run([GO, "test", "./cmd/gonk-gate/", "./pkg/beadstore/", "-run", RUN, "-count=1"],
                           capture_output=True, text=True)
        out = r.stdout + r.stderr
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}")
        elif not fails:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:800]}")
        else:
            res.append(f"KILLED   [{i}] {name}\n           " + "\n           ".join(fails))
    finally:
        for f, s in originals.items():
            open(f, 'w').write(s)
    print(res[-1], flush=True)
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)}")
sys.exit(1 if bad else 0)
