#!/usr/bin/env python3
"""mutate_pipeline_roundtrip.py -- "seen red" evidence for gonk-tios P8.

Applies one named break at a time ANYWHERE on the pipeline-triage path --
the webhook receiver, intake's gate and run identity, the order on the wire,
gonk-gate dispatch, the sweep's validation chain, the landing and run
maintenance -- runs ONLY the P8 end-to-end tests
(cmd/gonk-gate/pipeline_roundtrip_test.go), and requires them to FAIL; then
restores every file byte for byte (try/finally). The point is the converse of
P3's and P7's scripts: those prove each part's own tests notice a break in
that part; this proves the end-to-end test notices a break in ANY part, i.e.
that it is not passing on the strength of fakes standing in for the parts.

A mutation whose text is not found is VACUOUS, one the tests do not notice
SURVIVED, and one that does not compile NOCOMPILE -- each is a failure of this
script.

Usage: hack/mutate_pipeline_roundtrip.py            (all mutations)
       hack/mutate_pipeline_roundtrip.py 3 7        (only these indexes)
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
I = 'pkg/intake/'
LK = G + 'pipeline_links.go'
RUN = '^TestPipelineRoundTrip'

# (name, [(file, old, new), ...])
muts = [
 # ---- the hook and intake (P3) ----
 ("receiver: Pipeline Hook not handled",
  [('pkg/ghook/receiver.go', '\t"Pipeline Hook": true,', '')]),
 ("intake: the opt-in gate is triage's (a project without actions.pipelines is diagnosed)",
  [(I + 'dispatch.go', '\t\treturn cls.MayDiagnosePipelines()', '\t\treturn cls.MayTriage()')]),
 ("intake: dedupe broken -- the pipeline id feeds the fingerprint (every failure a new run)",
  [(I + 'pipeline.go', 'fp := pipelinectx.Fingerprint(f.FailedJobs, f.FailureReason)',
    'fp := pipelinectx.Fingerprint(append(f.FailedJobs, pipelinectx.FailedJob{Name: fmt.Sprint(ev.Pipeline.ID)}), f.FailureReason)')]),
 ("intake: a green does not end the run (last success left out of the anchor)",
  [(I + 'pipeline.go', 'anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, f.LastSuccess, f.FixEpoch)',
    'anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, 0, f.FixEpoch)')]),
 ("intake: the landing is never consulted (handled runs re-diagnosed)",
  [(I + 'pipeline.go', 'if pipelinectx.RunHandled(st) {', 'if false {')]),
 ("intake: the order drops the run's last success",
  [(I + 'dispatch.go', '\t\t\tPipelineLastSuccess: dec.PipelineLastSuccess,\n', '')]),
 # ---- the wire -> gonk-gate ----
 ("gate: dispatchArgsFrom does not read pipeline_last_success",
  [(DIS, '\t\tPipelineLastSuccess: i64("pipeline_last_success"),', '\t\tPipelineLastSuccess: 0,')]),
 ("gate: a second session for the same attempt is not refused (dedupe guard gone)",
  [(INJ, 'if priorFound && prior.SessionID != "" && prior.Attempt == pending.Attempt {',
         'if false && priorFound && prior.SessionID != "" && prior.Attempt == pending.Attempt {')]),
 ("gate: the session takes the order's pipeline, not the recorded one",
  [(DP, "\t\tbase.PipelineID = prior.PipelineID\n", "\t\t_ = prior.PipelineID\n")]),
 ("gate: checkout granted at the default branch head, not the pipeline's commit",
  [(DP, "grantCheckoutAt(ctx, d, pipelineAgent, session, pc.Pipeline.SHA)", "grantCheckout(ctx, d, pipelineAgent, session)")]),
 # ---- the sweep's validation chain (D8) ----
 ("sweep: grounding disabled (a hallucinated line is accepted)",
  [(AP, "\t\tif !grounded {\n", "\t\tif false && !grounded {\n")]),
 ("sweep: forged gonk markers are not refused",
  [(AP, "\t\tif gonkMarkerRe.MatchString(s) {", "\t\tif false && gonkMarkerRe.MatchString(s) {")]),
 ("sweep: the rendered note is never redact-checked",
  [(AP, "if _, kinds := pipelinectx.Redact(body); len(kinds) > 0 {", "if kinds := []string(nil); len(kinds) > 0 {")]),
 ("sweep: cited files are never checked at the pipeline's commit",
  [(AP, "\tfor fi, f := range diag.Files {\n\t\t// A 1-byte read", "\tfor fi, f := range diag.Files[:0] {\n\t\t// A 1-byte read")]),
 # ---- landing (D9) ----
 ("landing: the marker is written in a format intake does not read",
  [(LD, "\tmarker, err := pipelinectx.RunMarker(anchor, n.State)\n",
        "\tmarker, err := \"<!-- gonk:pipeline-run:\"+anchor+\" state: \"+n.State+\" -->\", error(nil)\n")]),
 ("landing: an open tracking issue is never reused (a second issue)",
  [(LD, "\t\tif issues[i].State == \"opened\" {", "\t\tif false {")]),
 ("landing: other class labels are not removed",
  [(LD, "add, remove = []string{pipelineClassLabel(class)}, classLabelsExcept(class)", "add, remove = []string{pipelineClassLabel(class)}, nil")]),
 # ---- run maintenance (D9) ----
 ("maintenance: not called from runSweep",
  [(SWP, "\tsweepPipelineRuns(ctx, dd)\n", "")]),
 ("maintenance: resolution skipped (a green pipeline is never seen)",
  [(SW, "\t\tif newest.ID > rec.PipelineLastSuccess {", "\t\tif false {")]),
 ("maintenance: resolution does not close the tracking issue",
  [(SW, 'state == pipelinectx.RunStateResolved && t.Kind == pipelinectx.TargetKindRef && ld.issue != nil && ld.issue.State == "opened"', "false")]),
 ("maintenance: a different failure does not supersede the run",
  [(SW, "\t\tif fp != rec.PipelineFingerprint {", "\t\tif false && fp != rec.PipelineFingerprint {")]),
 ("maintenance: the latest failing pipeline is not recorded",
  [(SW, "\t\trec.PipelineLatestFailed = obs.failed.ID\n", "\t\t_ = obs.failed.ID\n")]),
 # ---- links and the diagnosed heading (gonk-385g) ----
 ("links: a pipeline is named in plain text, never linked",
  [(LK, 'base != "" {\n\t\treturn fmt.Sprintf("[%s](%s/-/pipelines/%d)", text, base, id)',
        'base != "" && false {\n\t\treturn fmt.Sprintf("[%s](%s/-/pipelines/%d)", text, base, id)')]),
 ("links: the project URL is not recorded at dispatch",
  [(DP, "\tout.projectURL = projectWebURLOf(pc.Pipeline)\n", "\tout.projectURL = \"\"\n")]),
 ("heading: a diagnosed note does not lead with its subject",
  [(LD, '\tb.WriteString(diagnosedHeading(rec) + "\\n\\n")\n', '\tb.WriteString("gonk diagnosed this failure.\\n\\n")\n')]),
 ("evidence: a job is never linked",
  [(LK, "\treturn jobLink(rec, found.ID, codeSpan(found.Name))\n", "\treturn codeSpan(found.Name)\n")]),
 # ---- gonk-2usw FACTS: what a pipeline-fix is judged against is on the STORED record ----
 ("facts: the error signature never reaches the stored record",
  [(G + 'broker_pipeline_context.go', "firstError, r.ErrorSignature = fj.Excerpt.FirstError, fj.Excerpt.ErrorSignature", "firstError = fj.Excerpt.FirstError")]),
 ("facts: stage/failure_reason/allow_failure never reach the stored record",
  [(DP, "\t\t\tStage: j.Stage, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure,\n", "")]),
 ("facts: the generic flag never reaches the stored record",
  [(DP, "ErrorSignature: j.ErrorSignature, ErrorGeneric: j.ErrorGeneric,", "ErrorSignature: j.ErrorSignature,")]),
 ("diagnosis: the landed diagnosis is not recorded",
  [(AP, "\trecp.PipelineDiagnosis = landedDiagnosis(diag)\n", "")]),
 ("diagnosis: the sweep applies on a copy, so the stored record never carries it",
  [(SWP, "applyBrokerBatch(ctx, d, agent, &rec, *judged)", "applyBrokerBatch(ctx, d, agent, func() *beadstore.Record { c := rec; return &c }(), *judged)")]),
 ("diagnosis: every job of the pipeline is recorded as an evidence job",
  [(AP, "\tseen := map[string]bool{}\n\tfor _, ev := range e.Evidence {", "\tseen := map[string]bool{}\n\tfor _, j := range rec.PipelineJobs {\n\t\td.EvidenceJobs = append(d.EvidenceJobs, j.Name)\n\t}\n\tfor _, ev := range e.Evidence[:0] {"),
   (AP, "func landedDiagnosis(e effects.Effect) beadstore.PipelineDiagnosis {", "func landedDiagnosis(e effects.Effect, rec beadstore.Record) beadstore.PipelineDiagnosis {"),
   (AP, "landedDiagnosis(diag)", "landedDiagnosis(diag, rec)")]),
 ("maintenance: rewriting the record drops the diagnosis",
  [(SW, "\t\trec.PipelineLatestFailed = obs.failed.ID\n", "\t\trec.PipelineLatestFailed = obs.failed.ID\n\t\trec.PipelineDiagnosis = beadstore.PipelineDiagnosis{}\n")]),
 ("maintenance: rewriting the record drops the job facts",
  [(SW, "\t\trec.PipelineLatestFailed = obs.failed.ID\n", "\t\trec.PipelineLatestFailed = obs.failed.ID\n\t\trec.PipelineJobs = nil\n")]),
 # ---- the fix epoch (gonk-2usw EPOCH, plan 3.3): A -> fix merged -> A' ----
 ("epoch: Anchor ignores the fix epoch (the bug: after a merged fix the same fingerprint is the same run)",
  [('pkg/pipelinectx/anchor.go', 'if fixEpoch != 0 {', 'if false {')]),
 ("epoch intake: the decision ignores the fix epoch",
  [(I + 'pipeline.go', 'anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, f.LastSuccess, f.FixEpoch)',
    'anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, f.LastSuccess, 0)')]),
 ("epoch intake: the landing is read under the fix-less anchor (run A's marker answers for A')",
  [(I + 'pipeline.go', 'pipelinectx.Fingerprint(f.FailedJobs, f.FailureReason), f.LastSuccess, f.FixEpoch)',
    'pipelinectx.Fingerprint(f.FailedJobs, f.FailureReason), f.LastSuccess, 0)')]),
 ("epoch intake: merged fixes are never looked up",
  [(I + 'pipeline.go', 'f.FixEpoch, err = g.fixEpoch(ctx, pid, p, t, last)', 'f.FixEpoch, err = 0, error(nil)')]),
 ("epoch intake: run A's own pipeline joins the new run after the merge (no upper bound)",
  [('pkg/pipelinectx/fixepoch.go', 'after := landedOn(x, f.PipelineSHA) || !f.PipelineCreatedAt.Before(x.MergedAt)', 'after := true')]),
 ("epoch intake: the order drops pipeline_fix_epoch",
  [(I + 'dispatch.go', '\t\t\tPipelineFixEpoch:    dec.PipelineFixEpoch,\n', '')]),
 ("epoch gate: dispatchArgsFrom does not read pipeline_fix_epoch",
  [(DIS, '\t\tPipelineFixEpoch:    i64("pipeline_fix_epoch"),', '\t\tPipelineFixEpoch:    0,')]),
 ("epoch gate: the identity check ignores the fix epoch",
  [(DP, 'a.PipelineFingerprint, a.PipelineLastSuccess, a.PipelineFixEpoch)', 'a.PipelineFingerprint, a.PipelineLastSuccess, 0)')]),
 ("epoch gate: the record drops the fix epoch",
  [(DP, '\tbase.PipelineFixEpoch = a.PipelineFixEpoch\n', '')]),
 ("epoch: a run marker answers for any anchor it is a prefix of (A's note found for A')",
  [('pkg/pipelinectx/runmarker.go', 'if m[1] == anchor {', 'if strings.HasPrefix(m[1], anchor) {')]),
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
        r = subprocess.run([GO, "test", "./cmd/gonk-gate/", "-run", RUN, "-count=1"],
                           capture_output=True, text=True)
        out = r.stdout + r.stderr
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        why = [l.strip() for l in out.splitlines() if "_test.go:" in l][:1]
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}")
        elif not fails:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:800]}")
        else:
            res.append(f"KILLED   [{i}] {name}\n           " + "\n           ".join(fails + why))
    finally:
        for f, s in originals.items():
            open(f, 'w').write(s)
    print(res[-1], flush=True)
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)}")
sys.exit(1 if bad else 0)
