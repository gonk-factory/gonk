#!/usr/bin/env python3
"""mutate_pipefix_dispatch.py -- "seen red" evidence for gonk-2usw part DISPATCH.

Breaks one thing at a time in the pipeline-fix dispatch -- the sweep's
eligibility wiring (cmd/gonk-gate/sweep_pipefix.go), the class -> envelope
table (pipefix_envelope.go), the dispatch half (broker_pipefix_dispatch.go,
dispatch.go, broker_inject.go), the fix note (pipefix_note.go), the identities
and markers (pkg/pipelinectx/fixanchor.go), the fix record (pkg/beadstore) and
the operator scope on the meter wire (pkg/meterapi, internal/meter/service) --
runs ONLY the test that guards it (each mutation names its package and its own
-run pattern, so a kill means that row's own fixture went red, not that some
unrelated test noticed), and requires it to FAIL; then restores every file byte
for byte (try/finally). A mutation whose text is not found exactly once is
VACUOUS, one the named test does not notice SURVIVED, and one that does not
compile NOCOMPILE -- each is a failure of this script. Before anything is
mutated the unmutated packages must be green, or a kill would mean nothing.

The three tests plan section 11 names for this part each have a NAMED break,
marked [NAMED] in the list:
  two looks fire two orders          "a fired fix is not in flight until dispatch picks it up"
                                     "the look that lost the record write fires anyway"
  a closed-unmerged MR is re-proposed "dedup looks at open merge requests only"
  a `flaky` diagnosis dispatches     "E4's closed set is gone and the table has a row for flaky"

Usage: hack/mutate_pipefix_dispatch.py            (all mutations)
       hack/mutate_pipefix_dispatch.py 3 7        (only these indexes)
       hack/mutate_pipefix_dispatch.py --list     (names and indexes, nothing run)
       hack/mutate_pipefix_dispatch.py --show 3   (also print the full `go test` output)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate
(gonk-5ed1); its output is pasted into the plan's evidence log.
"""
import os
import re
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')

G = './cmd/gonk-gate/'
S = 'cmd/gonk-gate/sweep_pipefix.go'
EN = 'cmd/gonk-gate/pipefix_envelope.go'
BD = 'cmd/gonk-gate/broker_pipefix_dispatch.go'
DI = 'cmd/gonk-gate/dispatch.go'
BI = 'cmd/gonk-gate/broker_inject.go'
SW = 'cmd/gonk-gate/sweep.go'
SP = 'cmd/gonk-gate/sweep_pipeline.go'
NO = 'cmd/gonk-gate/pipefix_note.go'
PC = './pkg/pipelinectx/'
FA = 'pkg/pipelinectx/fixanchor.go'
PF = 'pkg/pipefix/eligible.go'
MA = './pkg/meterapi/'
MF = 'pkg/meterapi/meterapi.go'
MS = './internal/meter/service/'
SV = 'internal/meter/service/service.go'
BS = './pkg/beadstore/'
ST = 'pkg/beadstore/store.go'


def run(test, *subs):
    """-run pattern: the test, then each subtest level, anchored and escaped
    the way `go test` names subtests (spaces become underscores)."""
    parts = ['^%s$' % test]
    for s in subs:
        parts.append('^%s$' % re.escape(s.replace(' ', '_')))
    return '/'.join(parts)


ROWS = 'TestEachEligibilityRowIsLoggedByItsReasonCode'
ELIG = 'TestAnEligibleDiagnosisFiresOneFixOrder'
LOOKS = 'TestTwoLooksFireOneOrder'
RACE = 'TestTwoSweepsRacingFireOneOrder'
REFIRE = 'TestAFixOrderThatNeverArrivedIsFiredAgainAfterItsWindow'
CLOSED = 'TestAClosedUnmergedFixIsNotReproposed'
FLAKY = 'TestAFlakyDiagnosisNeverDispatches'
SERIAL = 'TestOneOpenFixMRPerTarget'
RECHECK = 'TestWhatCountsAsAFixMRIsRecheckedClientSide'
CHAIN = 'TestTheChainCountsOnlyFixesMergedOnThisTargetSinceItsLastGreen'
CONF = 'TestAConfigurationFixNeedsTheLiftAndAConfirmedClass'
FOLD = 'TestTheRecordedEnvelopeIsTheFold'
NEWEST = 'TestTheFixIsDecidedAgainstTheNewestFailingPipeline'
KEYS = 'TestCurrentFailureKeysAreWhatDispatchRecords'
FSTATE = 'TestFixStateOf'
INERT = 'TestAnOptedOutProjectIsInert'
ONLY = 'TestOnlyADiagnosedRunIsLookedAt'
ONCE = 'TestTheConfigurationIsReadOncePerProjectPerTick'
TABLE = 'TestEnvelopeTable'
NEVER = 'TestEnvelopeNeverColumn'
LIFT = 'TestTheCILiftIsNamedAndComesFromTheProjectOptIn'
RUNP = 'TestFixDispatchStartsOneSessionThroughThePreparer'
REFUSE = 'TestFixDispatchRefusalsHappenBeforeMeter'
NOREL = 'TestAStaleFixDispatchDoesNotReleaseARunningRecord'
DENY = 'TestAFixMeterDeniesIsNotedOnTheLanding'
PREPF = 'TestAFailedPrepareStartsNothingAndRestoresTheRecord'
CAP = 'TestAFixPastTheProjectsMaxAttemptsStartsNoSession'
NOTE = 'TestTheFixNoteIsEditedInPlaceAndNeverBackwards'
NOJUDGE = 'TestTheSweepDoesNotJudgeARunningFixWithoutAJudge'
RT = 'TestPipelineFixRoundTrip'

# (name, [(file, old, new), ...], package, -run pattern of the guarding test)
muts = []


def m(name, edits, pkg, pattern):
    muts.append((name, edits, pkg, pattern))


def g(name, edits, pattern):
    m(name, edits, G, pattern)


# ============================ the three named breaks ============================
g("[NAMED] two looks fire two orders: a fired fix is not in flight until dispatch picks it up",
  [(S, "\t\tif end, ok := reslingWindowEnd(rec); ok && now.Before(end) {\n\t\t\treturn pipefix.StateProposing, false, true\n\t\t}",
    "\t\tif end, ok := reslingWindowEnd(rec); ok && now.Before(end) && false {\n\t\t\treturn pipefix.StateProposing, false, true\n\t\t}")],
  run(LOOKS))
g("[NAMED] two sweeps fire two orders: the look that lost the record write fires anyway",
  [(S, '\t\tlk.d.Log.Info("sweep: pipeline fix: another look recorded this fix first; firing no order", "fix", lk.fixAnchor)\n\t\treturn\n',
    '\t\tlk.d.Log.Info("sweep: pipeline fix: another look recorded this fix first; firing no order", "fix", lk.fixAnchor)\n')],
  run(RACE))
g("[NAMED] a closed-unmerged MR is re-proposed: dedup looks at open merge requests only",
  [(S, '\t\tif mr.State != "opened" && mr.State != "closed" {', '\t\tif mr.State != "opened" {')], run(CLOSED))
g("[NAMED] a `flaky` diagnosis dispatches: E4's closed set is gone and the table has a row for flaky",
  [(PF, "\tif !contains(FixableClasses(), f.Run.Class) {", "\tif false {"),
   (EN, "\tpipefix.ClassTest:          {writable: []scope.Class{scope.ClassTest}},",
    '\tpipefix.ClassTest:          {writable: []scope.Class{scope.ClassTest}},\n\t"flaky":                    {writable: []scope.Class{scope.ClassSource}},')],
  run(FLAKY, "flaky"))
g("a `flaky` diagnosis is reported under another row: the sweep reports the class as `code`",
  [(S, "\t\tRead: true, TargetKind: lk.target.Kind, Open: lk.run.PipelineRunOpen, Class: lk.run.PipelineDiagnosis.Class,",
    "\t\tRead: true, TargetKind: lk.target.Kind, Open: lk.run.PipelineRunOpen, Class: pipefix.ClassCode,")],
  run(FLAKY, "flaky"))

# ================================ E1: the config ================================
g("E1: pipeline_fixes is read as on", [(S, "\t\tPipelineFixes: e.Actions.PipelineFixes,", "\t\tPipelineFixes: true,")],
  run(ROWS, "E1 pipeline_fixes is off"))
g("E1: pipelines is read as on", [(S, "\t\tPipelines:     e.Actions.Pipelines,", "\t\tPipelines:     true,")],
  run(ROWS, "E1 pipelines is off"))
g("E1: a failed configuration read is used anyway", [(S, "\tif err != nil || resp == nil {\n\t\tlk.warn(\"could not read the project's effective configuration\", err)",
                                                      "\tif resp == nil {\n\t\tlk.warn(\"could not read the project's effective configuration\", err)")],
  run(ROWS, "E1 the configuration cannot be read"))
g("E1: a project whose config does not load reads as opted in",
  [(S, "\t\tlk.facts.Config = pipefix.ConfigFacts{Loaded: true}\n", "\t\tlk.facts.Config = pipefix.ConfigFacts{Loaded: true, PipelineFixes: true, Pipelines: true}\n")],
  run(ROWS, "E1 the project's config does not load: opted in to nothing"))
g("INERT: the pass runs without FixesLive", [(S, "\tif !d.FixesLive || d.Pipelines == nil {", "\tif d.Pipelines == nil {")], run(INERT))
g("INERT: the pass runs without FixesLive (the constructor's own test)", [(S, "\tif !d.FixesLive || d.Pipelines == nil {", "\tif d.Pipelines == nil {")],
  run('TestTheFixPassIsOffUnlessItIsTurnedOn'))
g("INERT: an opted-out project's landing is read before E1 is answered",
  [(S, "\tif lk.fixCfg.AllowsTarget(lk.target.Kind) {", "\tif true {"),
   (S, "\t\tpipefix.E1Unread:  lk.readConfig,", "\t\tpipefix.E1Unread:  func(ctx context.Context) { lk.readConfig(ctx); lk.readRun(ctx) },")],
  run(INERT))
g("the config is read once per run, not once per project per tick",
  [(S, "\tif a, ok := s.cfgs[project]; ok {", "\tif a, ok := s.cfgs[project]; ok && false {")], run(ONCE))
g("a run with no landed diagnosis is looked at",
  [(S, "\t\trun.State != beadstore.StateDone || !run.PipelineDiagnosis.Recorded() {", "\t\trun.State != beadstore.StateDone {")],
  run(ONLY, "no diagnosis on the record"))
g("a run that is not done is looked at",
  [(S, "\t\trun.State != beadstore.StateDone || !run.PipelineDiagnosis.Recorded() {", "\t\t!run.PipelineDiagnosis.Recorded() {")],
  run(ONLY, "a denied run (needs-human)"))

# ============================== E2..E5: the run ================================
g("E2: every target kind is read as enabled", [(S, "\t\tTargets:       lk.fixCfg.Targets,", '\t\tTargets:       []string{"ref", "mr"},')],
  run(ROWS, "E2 an MR target while only ref is enabled (the default)"))
g("E2: an unreadable landing is read as a run with no note",
  [(S, "\t\t\tlk.warn(\"could not read the run's landing\", err)\n\t\t\treturn\n", "\t\t\tlk.warn(\"could not read the run's landing\", err)\n")],
  run(ROWS, "E2 the landing cannot be read"))
g("E3: the run note is assumed diagnosed", [(S, "\t\trun.NoteState = ld.state", "\t\trun.NoteState = pipelinectx.RunStateDiagnosed")],
  run(ROWS, "E3 the run note is not in state diagnosed"))
g("E5: the target set's problems are dropped",
  [(S, "\trun.Targets, run.TargetProblems = pipefix.TargetSet(", "\trun.Targets, _ = pipefix.TargetSet(")],
  run(ROWS, "E5 the diagnosis names a job that is not recorded"))
g("E5: the target set is built from every recorded job, not the evidence",
  [(S, "pipefix.TargetSet(recordedJobs(lk.run), lk.run.PipelineDiagnosis.EvidenceJobs)", 'pipefix.TargetSet(recordedJobs(lk.run), []string{"unit-tests"})')],
  run(ROWS, "E5 the diagnosis names no job"))

# ================================ E6: the scope ================================
g("E6: the envelope given to Eligible is wider than the table's row",
  [(S, "\tlk.facts.Scope = pipefix.ScopeFacts{Folded: true, Envelope: env.classes, Cited: cited}",
    '\tlk.facts.Scope = pipefix.ScopeFacts{Folded: true, Envelope: append(env.classes, "other", "test", "ci", "control"), Cited: cited}')],
  run(ROWS, "E6 the cited file is outside the envelope: needs-scope, decided at dispatch"))
g("E6: only the first cited file is classified",
  [(S, "\tfor _, path := range lk.run.PipelineDiagnosis.Files {\n\t\tc := pipefix.CitedFile{Path: path}",
    "\tfor _, path := range lk.run.PipelineDiagnosis.Files[:min(1, len(lk.run.PipelineDiagnosis.Files))] {\n\t\tc := pipefix.CitedFile{Path: path}")],
  run(ROWS, "E6 a code diagnosis citing a test file: needs-scope"))
g("E6: the project's scope is read at the pipeline's commit, not the default branch's pinned head",
  [(S, "\traw, err := f.GetRawFile(ctx, pid, intake.ConfigPath, cfgSHA, intake.MaxConfigBytes)",
    "\traw, err := f.GetRawFile(ctx, pid, intake.ConfigPath, latest.SHA, intake.MaxConfigBytes)")],
  run(ROWS, "E6 .gonk.yml cannot be read at the pinned commit"))
g("E6: an absent operator scope folds as 'the operator restricts nothing'",
  [(S, "\tif lk.cfg == nil || lk.cfg.OperatorScope == nil {", "\tif lk.cfg.OperatorScope == nil {\n\t\tlk.cfg.OperatorScope = &meterapi.OperatorScope{}\n\t}\n\tif lk.cfg == nil {")],
  run(ROWS, "E6 the meter states no operator scope"))
g("E6: a pack with no scope row folds (to an empty envelope: a needs-scope note for OUR misconfiguration)",
  [(S, "\tif len(row.Classes)+len(row.Liftable) == 0 {", "\tif false {")],
  run(ROWS, "E6 the pack has no scope row for the fix agent"))
g("E6: a cited file that cannot be read is classified anyway",
  [(S, "\t\tcontent, rerr := f.GetRawFile(ctx, pid, path, latest.SHA, pipefixGeneratedProbeBytes)\n",
    "\t\tcontent, rerr := f.GetRawFile(ctx, pid, path, latest.SHA, pipefixGeneratedProbeBytes)\n\t\trerr = nil\n")],
  run(ROWS, "E6 a cited file cannot be read at the pipeline's commit"))
g("E6: the instance layer is dropped from the fold",
  [(S, "\t\tInstance: lk.cfg.OperatorScope.Instance.Policy(),", "\t\tInstance: scope.Policy{},")],
  run(FOLD, "the operator allows only docs: a code fix citing source is needs-scope"))
g("E6: the group layer is dropped from the fold",
  [(S, "\t\tGroup:    lk.cfg.OperatorScope.Group.Policy(),", "\t\tGroup:    scope.Policy{},")],
  run(FOLD, "the group's caps and the project's max_changed_lines reach the record"))
g("E6: the project's scope section is dropped from the fold",
  [(S, "\t\tProject:  lk.fixCfg.TightenScope(pc.Scope),", "\t\tProject:  lk.fixCfg.TightenScope(scope.Policy{Classes: pc.Scope.Classes}),")], run(ELIG, "default-branch"))
g("E6: pipeline_fix.max_files / max_changed_lines do not tighten the fold",
  [(S, "\t\tProject:  lk.fixCfg.TightenScope(pc.Scope),", "\t\tProject:  pc.Scope,")],
  run(FOLD, "the group's caps and the project's max_changed_lines reach the record"))
g("E6: the workflow folds as disabled", [(S, "\t\tEnabled:  lk.facts.Config.PipelineFixes && lk.facts.Config.Pipelines,", "\t\tEnabled:  false,")],
  run(ELIG, "default-branch"))
g("4.2: the ci lift is opted in for every project", [(S, "\t\tLiftOptIn: lk.fixCfg.LiftOptIn(),", "\t\tLiftOptIn: []scope.Class{scope.ClassCI},")],
  run(CONF, "confirmed, allow_ci OFF: needs-scope, no lift"))
g("4.2: allow_ci never reaches the fold", [(S, "\t\tLiftOptIn: lk.fixCfg.LiftOptIn(),", "\t\tLiftOptIn: nil,")],
  run(CONF, "confirmed, allow_ci on: dispatched with the ci lift"))
g("E6: pipeline_fix.paths is not applied", [(S, "\tif eff, err = eff.WithBead(lk.fixCfg.Paths); err != nil {", "\tif eff, err = eff.WithBead(nil); err != nil {")],
  run(ROWS, "E6 every cited file is outside pipeline_fix.paths: needs-scope"))
g("E6: the paths hold is off", [(S, "\tif len(lk.editableFiles()) == 0 {", "\tif false {")],
  run(ROWS, "E6 every cited file is outside pipeline_fix.paths: needs-scope"))
g("E6: a generated file is classified by its name", [(S, "\t\t\tif scope.IsGenerated(content) {", "\t\t\tif scope.IsGenerated(content) && false {")],
  run(FOLD, "a generated file is not source"))
g("E6: the pipeline definition's local includes are not ci",
  [(S, "\t\tif base.CILocalIncludes, err = scope.CILocalIncludes(ci); err != nil {", "\t\tif _, err = scope.CILocalIncludes(ci); err != nil {")],
  run(FOLD, "a file the pipeline definition includes is ci, not whatever its name suggests"))
g("E6: base facts are read at the default branch's commit, not the pipeline's",
  [(S, "f.GetRawFile(ctx, pid, pipefixCIFile, latest.SHA, intake.MaxConfigBytes*16)", "f.GetRawFile(ctx, pid, pipefixCIFile, cfgSHA, intake.MaxConfigBytes*16)"),
   (S, "f.GetRawFile(ctx, pid, path, latest.SHA, pipefixGeneratedProbeBytes)", "f.GetRawFile(ctx, pid, path, cfgSHA, pipefixGeneratedProbeBytes)")],
  run(ELIG, "merge-request"))
g("Q2: a configuration fix does not wait for a confirmed class",
  [(S, "\tif lk.facts.Run.Class == pipefix.ClassConfiguration && !lk.classConfirmed() {", "\tif false {")],
  run(ROWS, "Q2 a configuration diagnosis nobody confirmed"))
g("Q2: any label on the landing confirms the class", [(S, "\t\tif l == labelFixClassConfirmed {", '\t\tif l != "" {')],
  run(ROWS, "Q2 a configuration diagnosis nobody confirmed"))

# ================================ E7: the head =================================
g("E7: the head is assumed to be the failing pipeline's commit",
  [(S, "\tlk.facts.Head = pipefix.HeadFacts{Read: true, TargetSHA: head, NewestFailingSHA: latest.SHA}",
    "\tlk.facts.Head = pipefix.HeadFacts{Read: head != \"\", TargetSHA: latest.SHA, NewestFailingSHA: latest.SHA}")],
  run(ROWS, "E7 the target's head has moved past the failing pipeline's commit"))
g("E7: an MR's head is assumed to be the failing pipeline's commit",
  [(S, "\t\tlk.mr, lk.branch, head = mr, mr.SourceBranch, mr.SHA", "\t\tlk.mr, lk.branch, head = mr, mr.SourceBranch, latest.SHA")],
  run(ROWS, "E7 the MR's head has moved"))
g("E7: a fork's merge request is a target", [(S, '\t\tif mr.SourceProjectID != pid || mr.SourceBranch == "" {', '\t\tif mr.SourceBranch == "" {')],
  run(ROWS, "E7 the MR's source branch is in another project"))
g("the newest failing pipeline is taken to be the diagnosed one",
  [(S, "\tid := max(lk.run.PipelineLatestFailed, lk.run.PipelineID)", "\tid := lk.run.PipelineID")], run(NEWEST))

# ================================ E8: the cause ================================
g("E8: the recorded keys are trusted (a drifted cause is not seen)",
  [(S, "\tlk.facts.Current = pipefix.CurrentFacts{Read: true, FailedKeys: keys}",
    "\tfor _, t := range lk.facts.Run.Targets {\n\t\tkeys = append(keys, t.Key)\n\t}\n\tlk.facts.Current = pipefix.CurrentFacts{Read: true, FailedKeys: keys}")],
  run(ROWS, "E8 the same job now fails differently"))
g("E8: an unreadable log yields a key with no signature (drift, not unknown)",
  [(S, '\t\t\t\treturn nil, nil, fmt.Errorf("read the log of job %d: %w", j.ID, err)', "\t\t\t\tkeys = append(keys, k)\n\t\t\t\tcontinue")],
  run(ROWS, "E8 the job's log cannot be read"))
g("E8: the recomputed key drops the signature", [(S, "\t\t\tk.ErrorSignature = ex.ErrorSignature", '\t\t\tk.ErrorSignature = ex.ErrorSignature[:0]')], run(KEYS))
g("E8: the recomputed key uses GitLab's raw job name, not the canonical one",
  [(S, "pipelinectx.JobFailureKey{Stage: j.Stage, Name: jobNameText(j.Name), FailureReason: j.FailureReason}",
    "pipelinectx.JobFailureKey{Stage: j.Stage, Name: j.Name, FailureReason: j.FailureReason}")], run(KEYS))
g("E8: every failed job gets an excerpt (dispatch records only the first four)",
  [(S, "\t\tif i < pipelinectx.MaxExcerptJobs {\n\t\t\ttt, err := fetchPipelineTrace", "\t\tif i < 99 {\n\t\t\ttt, err := fetchPipelineTrace")], run(KEYS))

# ================================= E9: dedup ==================================
g("E9: an explicit fix state on the record is ignored",
  [(S, '\tif s := pipefix.State(rec.PipelineFix.State); s != "" {', '\tif s := pipefix.State(rec.PipelineFix.State); s != "" && false {')], run(FSTATE))
g("E9: a parked fix is not in flight",
  [(S, "\tcase beadstore.StateRunning, beadstore.StatePendingPrompt, beadstore.StateParked:", "\tcase beadstore.StateRunning, beadstore.StatePendingPrompt:")], run(FSTATE))
g("E9: a denied fix is in flight forever", [(S, "\t\treturn pipefix.StateDenied, false, true", "\t\treturn pipefix.StateProposing, false, true")], run(FSTATE))
g("E9: a done fix record with no state reads as 'no fix' (it would be dispatched over)",
  [(S, '\t}\n\treturn "", false, false\n}', '\t}\n\treturn "", true, true\n}')], run(FSTATE))
g("E9: the fire window is one instant longer", [(S, "ok && now.Before(end) {\n\t\t\treturn pipefix.StateProposing", "ok && !now.After(end) {\n\t\t\treturn pipefix.StateProposing")],
  run(FSTATE))
g("E9: an unreadable fix record is treated as no record",
  [(S, '\t\tlk.warn("could not read the fix record", err)\n\t\treturn\n', '\t\tlk.warn("could not read the fix record", err)\n')],
  run(ROWS, "E9 the fix record cannot be read"))
g("E9: a fix record in an unknown state is treated as no record",
  [(S, "\t\t\tdd.FixState = pipefixUnknownState", '\t\t\tdd.FixState = ""')], run(ROWS, "E9 a fix record in a state this build cannot read"))
g("E9: this fix's own branch is not recognised without its marker", [(S, "\t\tmine := mr.SourceBranch == branch", "\t\tmine := mr.SourceBranch == branch && false")],
  run(ROWS, "E9 this fix's own branch has a closed-unmerged MR, whatever its description says"))
g("E9: a fix MR whose cause cannot be read is skipped",
  [(S, '\t\t\t\t\tfmt.Errorf("merge request !%d from %s", mr.IID, mr.SourceBranch))\n\t\t\t\treturn\n',
    '\t\t\t\t\tfmt.Errorf("merge request !%d from %s", mr.IID, mr.SourceBranch))\n\t\t\t\tcontinue\n')],
  run(ROWS, "E9 a fix MR whose cause cannot be read"))
g("E9: the cause key is not compared (no MR is for this cause)", [(S, "\t\t\tmine = mrCause == cause", "\t\t\tmine = mrCause == cause && false")],
  run(ROWS, "E9 a fix MR for this cause was closed unmerged"))
g("E9: the cause key is not compared (every fix MR is for this cause)", [(S, "\t\t\tmine = mrCause == cause", "\t\t\tmine = mrCause == cause || true")], run(CLOSED))
g("E9: an open cause MR is counted as closed-unmerged", [(S, '\t\tif mr.State == "opened" {\n\t\t\tdd.OpenCauseMRs++', '\t\tif mr.State == "closed" {\n\t\t\tdd.OpenCauseMRs++')],
  run(ROWS, "E9 a fix MR for this cause is open"))
g("E9: a human's merge request counts as a fix MR",
  [(S, "\t\tif mr.Author.ID != lk.l.me.ID || !pipelinectx.IsFixBranch(mr.SourceBranch) ||", "\t\tif !pipelinectx.IsFixBranch(mr.SourceBranch) ||")],
  run(RECHECK, "a human's MR from a fix-shaped branch"))
g("E9: any of the bot's merge requests counts as a fix MR",
  [(S, "\t\tif mr.Author.ID != lk.l.me.ID || !pipelinectx.IsFixBranch(mr.SourceBranch) ||", "\t\tif mr.Author.ID != lk.l.me.ID ||")],
  run(RECHECK, "the bot's MR from another kind of branch"))
g("E9: a fork's merge request counts as a fix MR",
  [(S, "\t\t\tmr.SourceProjectID != pid || mr.TargetProjectID != pid {", "\t\t\tmr.TargetProjectID != pid {")],
  run(RECHECK, "a fix-branch MR whose source is another project"))

# ============================ E10, E11, E12: the target ===========================
g("E10: an open fix MR on any branch blocks this target", [(S, "\t\tif mr.TargetBranch != lk.branch {\n\t\t\tcontinue\n\t\t}", "\t\tif false {\n\t\t\tcontinue\n\t\t}")], run(SERIAL))
g("E10: open fix MRs are not counted", [(S, '\t\tcase "opened", "locked":\n\t\t\tsf.OpenFixMRs++', '\t\tcase "opened", "locked":')],
  run(ROWS, "E10 another fix MR is open on this target"))
g("E10: a merged fix MR without merged_at is skipped",
  [(S, '\t\t\t\tlk.warn("a merged fix merge request carries no merged_at", fmt.Errorf("merge request !%d", mr.IID))\n\t\t\t\treturn\n',
    '\t\t\t\tlk.warn("a merged fix merge request carries no merged_at", fmt.Errorf("merge request !%d", mr.IID))\n\t\t\t\tcontinue\n')],
  run(ROWS, "E10 a merged fix MR on this target carries no merged_at"))
g("E11: an unreadable last green pipeline counts every merged fix",
  [(S, "\t\t\tlk.warn(\"could not read when the target's last green pipeline was created\", err)\n\t\t\treturn\n",
    "\t\t\tlk.warn(\"could not read when the target's last green pipeline was created\", err)\n")],
  run(ROWS, "E10 the last green pipeline cannot be read"))
g("E11: fixes merged before the last green pipeline count", [(S, "\t\tif mr.MergedAt.After(since) {", "\t\tif mr.MergedAt.After(since) || true {")],
  run(CHAIN, "two merged, one BEFORE the last green: dispatches"))
g("E11: fixes merged into another branch count",
  [(S, "\t\tif mr.TargetBranch != lk.branch {\n\t\t\tcontinue\n\t\t}", "\t\tif false {\n\t\t\tcontinue\n\t\t}")],
  run(CHAIN, "two merged, one into another branch: dispatches"))
g("E11: max_chain is not the project's", [(S, "\t\tMaxChain:      lk.fixCfg.MaxChain,", "\t\tMaxChain:      99,")],
  run(ROWS, "E11 max_chain fix MRs already merged since the last green pipeline"))
g("E12: the correlation hold is never asked",
  [(S, "\tlk.facts.CorrelationHold = d.FixHold != nil && d.FixHold(ctx, run)", "\tlk.facts.CorrelationHold = false")],
  run(ROWS, "E12 the run is held by a roll-up"))

# ============================ the decision's consequences ===========================
g("a silent row writes a note", [(S, "\tcase reason.Unknown || !reason.Note:", "\tcase reason.Unknown:")],
  run(ROWS, "E7 the target's head has moved past the failing pipeline's commit"))
g("a noted row is not noted", [(S, "\tcase reason.Unknown || !reason.Note:", "\tcase reason.Unknown || !reason.Note || true:")],
  run(ROWS, "E4 a flaky diagnosis is never eligible"))
g("needs-human is never put on the landing", [(S, "\tif changed && reason.NeedsHuman {", "\tif changed && reason.NeedsHuman && false {")],
  run(ROWS, "E11 max_chain fix MRs already merged since the last green pipeline"))
g("needs-human is put on every noted refusal", [(S, "\tif changed && reason.NeedsHuman {", "\tif changed {")],
  run(ROWS, "E4 a flaky diagnosis is never eligible"))
g("needs-scope at dispatch is not recorded", [(S, "\tif !reason.NeedsScope {\n\t\treturn\n\t}", "\tif true {\n\t\treturn\n\t}")],
  run(ROWS, "E6 the cited file is outside the envelope: needs-scope, decided at dispatch"))
g("needs-scope is noted as not-eligible", [(S, "\t\tstate = string(pipefix.StateNeedsScope)", "\t\tstate = pipelinectx.FixNoteNotEligible")],
  run(ROWS, "E6 the diagnosis cites no file: needs-scope"))
g("the needs-scope record is not terminal", [(S, "RunAnchor: run.BeadAnchor, State: string(pipefix.StateNeedsScope), Class:", "RunAnchor: run.BeadAnchor, Class:")],
  run(ROWS, "E6 the cited file is outside the envelope: needs-scope, decided at dispatch"))
g("a re-fire does not write over the stale record it read", [(S, "\t\tifRevision = lk.fixRec.Revision", '\t\tifRevision = ""')], run(REFIRE))
g("the fix order carries no fire_by deadline", [(S, "\t\tReslungAt: fireAt, ReslingFireBy: reslingFireBy(fireAt),", "\t\tReslungAt: fireAt,")], run(ELIG, "default-branch"))
g("the fix record is an open pipeline run", [(S, "\t\tPipelineSHA: latest.SHA, PipelineProjectURL: run.PipelineProjectURL,\n\t\tPipelineFix: beadstore.PipelineFix{\n\t\t\tRunAnchor: run.BeadAnchor, CauseKey:",
                                              "\t\tPipelineSHA: latest.SHA, PipelineProjectURL: run.PipelineProjectURL, PipelineRunOpen: true,\n\t\tPipelineFix: beadstore.PipelineFix{\n\t\t\tRunAnchor: run.BeadAnchor, CauseKey:")],
  run(ELIG, "default-branch"))
g("the fix record does not keep the lift", [(S, "\t\t\tEnvelope: lk.env.classes, Lift: lk.env.liftNames(),", "\t\t\tEnvelope: lk.env.classes,")],
  run(CONF, "confirmed, allow_ci on: dispatched with the ci lift"))
g("the fix record keeps every cited file, not those inside paths",
  [(S, "\t\t\tFiles: lk.editableFiles(), Paths: lk.fixCfg.Paths,", "\t\t\tFiles: lk.run.PipelineDiagnosis.Files, Paths: lk.fixCfg.Paths,")],
  run(FOLD, "pipeline_fix.paths keeps only the cited files inside it"))
g("the fix is recorded against the diagnosed pipeline, not the newest failing one",
  [(S, "\t\t\tBaseSHA: latest.SHA, BasePipelineID: latest.ID, TargetBranch: lk.branch, ConfigSHA: lk.cfgSHA,",
    "\t\t\tBaseSHA: run.PipelineSHA, BasePipelineID: run.PipelineID, TargetBranch: lk.branch, ConfigSHA: lk.cfgSHA,")], run(NEWEST))
g("the fix MR's target branch is always the default branch",
  [(S, "BasePipelineID: latest.ID, TargetBranch: lk.branch, ConfigSHA: lk.cfgSHA,", 'BasePipelineID: latest.ID, TargetBranch: "main", ConfigSHA: lk.cfgSHA,')],
  run(ELIG, "merge-request"))
g("the recorded config commit is the pipeline's", [(S, "TargetBranch: lk.branch, ConfigSHA: lk.cfgSHA,", "TargetBranch: lk.branch, ConfigSHA: latest.SHA,")],
  run(ELIG, "merge-request"))
g("the order names the diagnosed pipeline, not the fix's base", [(S, "\t\tPipelineID: latest.ID, PipelineTarget: run.PipelineTarget,", "\t\tPipelineID: run.PipelineID, PipelineTarget: run.PipelineTarget,")],
  run(NEWEST))
g("the fix record does not keep the base pipeline's jobs", [(S, "\t\t\tBaseJobs:         lk.baseJobs,\n", "")], run(ELIG, "default-branch"))
g("the fix record does not keep the signature version", [(S, "\t\t\tSignatureVersion: sigVersion,\n", "\t\t\tSignatureVersion: 0 * sigVersion,\n")], run(ELIG, "default-branch"))
g("the fix record does not keep the excerpt version", [(S, "\t\t\tExcerptVersion:   run.PipelineExcerptVersion,\n", "")], run(ELIG, "default-branch"))
g("the base jobs are recorded under GitLab's raw names", [(S, "beadstore.PipelineFixBaseJob{Name: jobNameText(j.Name), Status: j.Status,", "beadstore.PipelineFixBaseJob{Name: j.Name + \" \", Status: j.Status,")], run(ELIG, "default-branch"))
g("the look after maintenance is dropped (nothing is ever dispatched)",
  [(SP, "\t\tconsiderPipelineFix(ctx, d, fixes, lander, maintainPipelineRun(ctx, d, lander, rec, now), now)", "\t\t_, _ = fixes, maintainPipelineRun(ctx, d, lander, rec, now)")],
  run(RT, "default-branch"))

# ============================== 4.2: the envelope table =============================
CODE_ROW = "\tpipefix.ClassCode:          {writable: []scope.Class{scope.ClassSource, scope.ClassDocs}},"
TEST_ROW = "\tpipefix.ClassTest:          {writable: []scope.Class{scope.ClassTest}},"
CONF_ROW = "\tpipefix.ClassConfiguration: {lift: []scope.Class{scope.ClassCI}},"
g("4.2: a code fix may write test", [(EN, CODE_ROW, CODE_ROW.replace("scope.ClassDocs}", "scope.ClassDocs, scope.ClassTest}"))],
  run(TABLE, "code: source and docs, nothing lifted"))
g("4.2: a code fix may not write docs", [(EN, CODE_ROW, CODE_ROW.replace("scope.ClassSource, scope.ClassDocs", "scope.ClassSource"))],
  run(TABLE, "code: source and docs, nothing lifted"))
g("4.2: a test fix may write source", [(EN, TEST_ROW, TEST_ROW.replace("scope.ClassTest}", "scope.ClassTest, scope.ClassSource}"))],
  run(TABLE, "test: test only"))
g("4.2: a configuration fix may write source", [(EN, CONF_ROW, CONF_ROW.replace("{lift:", "{writable: []scope.Class{scope.ClassSource}, lift:"))],
  run(TABLE, "configuration without allow_ci: nothing at all"))
g("4.2: a configuration fix asks for no lift", [(EN, CONF_ROW, "\tpipefix.ClassConfiguration: {},")],
  run(TABLE, "configuration with allow_ci: ci, lifted, and nothing else"))
g("4.2: a code fix asks for the ci lift too", [(EN, CODE_ROW, CODE_ROW.replace("scope.ClassDocs}}", "scope.ClassDocs}, lift: []scope.Class{scope.ClassCI}}"))],
  run(TABLE, "code with allow_ci: still no ci"))
g("4.2: the row is not applied inside the fold (Narrow dropped)",
  [(EN, "\tenv := pipefixEnvelope{scope: eff.Narrow(row.writable...)}", "\tenv := pipefixEnvelope{scope: eff}")], run(TABLE, "test: test only"))
g("4.2: a lift the fold would refuse is asked for anyway (an error, not needs-scope)",
  [(EN, "\t\t\tif grantable[c] {", "\t\t\tif true {")], run(TABLE, "configuration without allow_ci: nothing at all"))
g("4.2: the recorded envelope leaves out the lifted class",
  [(EN, "\t\tif env.allows(c) {", "\t\tif env.scope.Allows(c, scope.Lift{}) {")], run(TABLE, "configuration with allow_ci: ci, lifted, and nothing else"))
g("4.2: the lift is not named for its dispatch",
  [(EN, "scope.Grant{Trigger: atags.TriggerPipelineFix, Workflow: class, Classes: ask}", 'scope.Grant{Trigger: atags.TriggerPipelineFix, Workflow: "any", Classes: ask}')], run(LIFT))
g("4.2: a class outside the table gets the whole fold",
  [(EN, '\tif !ok {\n\t\treturn pipefixEnvelope{}, fmt.Errorf("pipeline fix: diagnosis class %q has no scope envelope", class)\n\t}\n', '\t_ = ok\n')],
  run('TestEveryFixableClassHasAnEnvelopeRow'))
g("4.2: a code fix may write test under the widest configuration (the Never column)",
  [(EN, CODE_ROW, CODE_ROW.replace("scope.ClassDocs}", "scope.ClassDocs, scope.ClassTest}"))], run(NEVER))

# ================================ the dispatch half ================================
g("a pipeline-fix order is routed with no preparer wired", [(DI, "\t\tagent, ok = pipefixAgent, d.PrepareFix != nil", "\t\tagent, ok = pipefixAgent, true")],
  run(REFUSE, "no preparer is wired (production until part AGENT)"))
g("the order's identity is not checked", [(DI, "\t\tif err := checkPipelineFixIdentity(a); err != nil {", "\t\tif err := checkPipelineFixIdentity(a); err != nil && false {")],
  run(REFUSE, "the anchor is not the fix anchor of the order's run identity"))
g("identity: any anchor is accepted for the run identity", [(BD, "\tif want != a.BeadAnchor {", "\tif false {")],
  run(REFUSE, "a RUN anchor under the pipeline-fix trigger"))
g("identity: any session key is accepted", [(BD, "\tif key != a.SessionKey {", "\tif false {")], run(REFUSE, "another fix's session key"))
g("identity: an order with no pipeline id is accepted", [(BD, "\tif a.PipelineID < 1 {", "\tif false {")], run('TestFixOrderIdentity'))
g("the pack is not checked before meter is asked",
  [(DI, "\t\tif _, err := packAgentEnv(d.packDir(), agent); err != nil {", "\t\tif _, err := packAgentEnv(d.packDir(), agent); err != nil && false {")],
  run(REFUSE, "the pack has no fix agent"))
g("preflight: an order with no fix record is dispatched",
  [(BD, "\tif !decided.found || !fix.PipelineFix.Recorded() || fix.Trigger != atags.TriggerPipelineFix {", "\tif fix.Trigger == atags.TriggerPipelineFix && false {")],
  run(REFUSE, "no fix record: an order nobody decided"))
g("preflight: a record with no fix facts is dispatched",
  [(BD, "\tif !decided.found || !fix.PipelineFix.Recorded() || fix.Trigger != atags.TriggerPipelineFix {", "\tif !decided.found || fix.Trigger != atags.TriggerPipelineFix {")],
  run(REFUSE, "a record under the fix anchor that is not a fix record"))
g("preflight: a fix with a recorded state is dispatched again", [(BD, '\tif fix.PipelineFix.State != "" {', "\tif false {")],
  run(REFUSE, "the fix already has a recorded state"))
g("preflight: the order's pipeline is not compared with the fix's base", [(BD, "\tif fix.PipelineFix.BasePipelineID != a.PipelineID {", "\tif false {")],
  run(REFUSE, "the order names another pipeline than the fix was decided against"))
g("preflight: a closed run's fix is dispatched", [(BD, "\tcase !run.PipelineRunOpen:", "\tcase false:")], run(REFUSE, "the run has closed since"))
g("preflight: a fix is dispatched against a run that has moved on",
  [(BD, "\tcase max(run.PipelineLatestFailed, run.PipelineID) != fix.PipelineFix.BasePipelineID:", "\tcase false:")],
  run(REFUSE, "the run has a newer failing pipeline since"))
g("preflight: a missing run record is not noticed", [(BD, "\tcase !found:", "\tcase !found && false:")], run(REFUSE, "the run's record is gone"))
g("preflight: a stale fix is not released (it stays 'in flight' until its window ends)",
  [(BD, "\t\tif _, err := d.Store.PutIf(ctx, released, fix.Revision); err != nil && !errors.Is(err, beadstore.ErrStale) {",
    "\t\tif _, err := error(nil), error(nil); err != nil && !errors.Is(err, beadstore.ErrStale) {")],
  run(REFUSE, "the run has closed since"))
g("preflight: the release keeps the fire window", [(BD, "\t\treleased.ReslungAt, released.ReslingFireBy = time.Time{}, time.Time{}\n", "")],
  run(REFUSE, "the run has closed since"))
g("preflight: the release takes a running record", [(BD, '\tif fix.State == "" || fix.State == beadstore.StateParked {', "\tif true {")], run(NOREL))
g("dispatch does not carry the controller's decisions onto the record", [(BD, "\tbase.PipelineFix = stored.PipelineFix\n", "")], run(RUNP, "default-branch"))
g("dispatch takes the fix's pipeline from nowhere", [(BD, "\tbase.PipelineSHA = stored.PipelineSHA\n", "")], run(RUNP, "default-branch"))
g("the fix session is named like an issue session", [(BI, "\tif agent == pipefixAgent {\n\t\t// Named for the pipeline the fix was decided against", "\tif false {\n\t\t// Named for the pipeline the fix was decided against")],
  run(RUNP, "default-branch"))
g("a failed prepare starts a session anyway", [(BI, "\t\tif perr != nil {\n\t\t\td.Log.Error(\"cannot prepare the fix session; starting no session\",", "\t\tif false {\n\t\t\td.Log.Error(\"cannot prepare the fix session; starting no session\",")],
  run(PREPF))
g("the session is not started with the preparer's prompt", [(BI, "\t\tprompt, checkoutURL, checkoutSHA = fs.Prompt, fs.CheckoutURL, fs.CheckoutSHA", "\t\tprompt, checkoutURL, checkoutSHA = \"\", fs.CheckoutURL, fs.CheckoutSHA")],
  run(RUNP, "default-branch"))
g("the context hash is not recorded", [(BI, "\t\tbase.PipelineFix.ContextSHA256 = fs.ContextSHA256\n", "")], run(RUNP, "default-branch"))
g("the preparer is not handed the diagnosed run", [(BI, "pipefixPrepareInput{Session: name, Fix: base, Run: d.fixRun, Rung: dec.Rung}", "pipefixPrepareInput{Session: name, Fix: base, Rung: dec.Rung}")],
  run(RUNP, "default-branch"))
g("no proposing note is written", [(BI, "\t\td.postFixNote(ctx, base, d.fixRun, fixProposing(base, dec.Attempt, dec.Rung), false)\n", "")], run(RUNP, "default-branch"))
g("a proposing fix asks for a human", [(BI, "fixProposing(base, dec.Attempt, dec.Rung), false)", "fixProposing(base, dec.Attempt, dec.Rung), true)")], run(RUNP, "default-branch"))
g("a denied fix writes the RUN note (the diagnosis's), not the fix note",
  [(DI, "\t\tif isFix { // the FIX note on the diagnosis landing (8.1 `denied`), never a run note\n\t\t\td.postFixNote(ctx, base, d.fixRun, fixDenied(base, dec.Reason, dec.Detail), true)\n\t\t\treturn 0\n\t\t}\n", "")],
  run(DENY))
g("a denied fix does not ask for a human", [(DI, "fixDenied(base, dec.Reason, dec.Detail), true)", "fixDenied(base, dec.Reason, dec.Detail), false)")], run(DENY))
g("max_attempts: the cap is one attempt too tight", [(DI, "dec.Attempt > base.PipelineFix.MaxAttempts {", "dec.Attempt >= base.PipelineFix.MaxAttempts {")], run(CAP, "attempt 2 of 2: runs"))
g("max_attempts: not enforced", [(DI, "\tif isFix && base.PipelineFix.MaxAttempts > 0 && dec.Attempt > base.PipelineFix.MaxAttempts {", "\tif false {")], run(CAP, "attempt 3 of 2: stopped"))
g("max_attempts: the capped attempt's reservation is left to expire", [(BD, '\tif dec.ReservationID != "" {', "\tif false {")], run(CAP, "attempt 3 of 2: stopped"))
g("max_attempts: the capped attempt is reported gate-failed (it would escalate)", [(BD, "Outcome: meterapi.OutcomeAborted, Trigger: a.Trigger,", "Outcome: meterapi.OutcomeGateFailed, Trigger: a.Trigger,")],
  run(CAP, "attempt 3 of 2: stopped"))
g("the sweep judges a fix whose merge request is open as if it were a session",
  [(SW, '\tif rec.Trigger == atags.TriggerPipelineFix && rec.PipelineFix.State != "" {', '\tif rec.Trigger == atags.TriggerPipelineFix && rec.PipelineFix.State != "" && false {')],
  run(NOJUDGE))
g("an unjudged fix past its reservation says nothing", [(S, "\tif reservationExpired(rec, d.Now()) {\n\t\td.Log.Warn(\"sweep: a pipeline fix's merge request is open past its reservation", "\tif false {\n\t\td.Log.Warn(\"sweep: a pipeline fix's merge request is open past its reservation")],
  run(NOJUDGE))

# ================================== the fix note ==================================
g("note: a dispatch-time state overwrites a later part's", [(NO, "\tcase existing != nil && !fixNoteMayReplace(n, state):", "\tcase existing != nil && !fixNoteMayReplace(n, state) && false:")], run(NOTE))
g("note: a human's note quoting the marker is the fix note", [(NO, "\t\tif !l.botNote(n) {\n\t\t\tcontinue\n\t\t}\n\t\tif states := pipelinectx.FixMarkerStates", "\t\tif false {\n\t\t\tcontinue\n\t\t}\n\t\tif states := pipelinectx.FixMarkerStates")],
  run(NOTE))
g("note: an identical note is written again", [(NO, "\tcase existing != nil && existing.Body == body:", "\tcase false:")], run(NOTE))
g("note: written on an MR with no diagnosis note", [(NO, "\tif ld.note == nil || (t.Kind == pipelinectx.TargetKindRef && ld.issue == nil) {", "\tif t.Kind == pipelinectx.TargetKindRef && ld.issue == nil {")],
  run(NOTE))
g("note: text holding an HTML comment is written", [(NO, '\tif strings.Contains(text, "<!--") || strings.Contains(text, "gonk:pipeline-") {', '\tif strings.Contains(text, "gonk:pipeline-") {')], run(NOTE))
g("note: text holding a gonk marker name is written", [(NO, '\tif strings.Contains(text, "<!--") || strings.Contains(text, "gonk:pipeline-") {', '\tif strings.Contains(text, "<!--") {')], run(NOTE))
g("note: the proposing note does not link its pipeline", [(NO, "pipelineRef(fix, fix.PipelineFix.BasePipelineID),\n\t\tcodeSpan(shortSHA(fix.PipelineFix.BaseSHA)))", "fmt.Sprint(fix.PipelineFix.BasePipelineID),\n\t\tcodeSpan(shortSHA(fix.PipelineFix.BaseSHA)))")],
  run(RUNP, "default-branch"))
g("note: the proposing note leaves out the attempt", [(NO, "max(attempt, 1), rungText(rung))", "0, rungText(rung))")], run(RUNP, "default-branch"))
g("note: the refusal names the cited path outside a code span",
  [(S, "\t\t\t\t\tcodeSpan(c.Path), codeSpan(c.Class), class)", "\t\t\t\t\tc.Path, codeSpan(c.Class), class)")],
  run(ROWS, "E6 the cited file is outside the envelope: needs-scope, decided at dispatch"))

# ============================= identities and markers =============================
def c(name, edits, pattern):
    m(name, edits, PC, pattern)


c("the fix anchor is the run anchor", [(FA, "\treturn strings.Replace(runAnchor, anchorRunSegment, anchorFixSegment, 1), nil", "\treturn runAnchor, nil")], run('TestFixAnchorRoundTrips'))
c("FixAnchor accepts anything", [(FA, '\tif !anchorRe.MatchString(runAnchor) {\n\t\treturn "", fmt.Errorf("pipelinectx: %q is not a pipeline anchor", runAnchor)\n\t}\n\treturn strings.Replace', '\treturn strings.Replace')],
  run('TestFixAnchorRefusesWhatIsNotARunAnchor'))
c("RunAnchorOfFix accepts a run anchor", [(FA, '\tif !fixAnchorRe.MatchString(fixAnchor) {\n\t\treturn "", fmt.Errorf("pipelinectx: %q is not a pipeline-fix anchor", fixAnchor)\n\t}\n\treturn strings.Replace', '\treturn strings.Replace')],
  run('TestFixAnchorRefusesWhatIsNotARunAnchor'))
c("the fix branch is 10 hex, not 12", [(FA, "\treturn m[1], hex.EncodeToString(sum[:])[:12], nil", "\treturn m[1], hex.EncodeToString(sum[:])[:10], nil")], run('TestFixBranchAndSessionKeyGolden'))
c("the fix session key has the diagnosis's infix", [(FA, '\treturn "gonk-" + pid + "-pf-" + h, nil', '\treturn "gonk-" + pid + "-pl-" + h, nil')], run('TestFixBranchAndSessionKeyGolden'))
c("the fix branch has another prefix", [(FA, "\treturn FixBranchPrefix + h, nil", '\treturn "gonk/fix-" + h, nil')], run('TestFixBranchAndSessionKeyGolden'))
c("FixMarker accepts any state", [(FA, "\tif !KnownFixNoteState(state) {", "\tif false {")], run('TestFixMarkerRefusesABadAnchorOrState'))
c("FixMarker accepts a run anchor", [(FA, '\tif !fixAnchorRe.MatchString(fixAnchor) {\n\t\treturn "", fmt.Errorf("pipelinectx: %q is not a pipeline-fix anchor", fixAnchor)\n\t}\n\tif !KnownFixNoteState(state) {', '\tif !KnownFixNoteState(state) {')],
  run('TestFixMarkerRefusesABadAnchorOrState'))
c("a fix marker for another fix is read", [(FA, "\t\tif m[1] == fixAnchor {\n\t\t\tout = append(out, m[2])", "\t\tif true {\n\t\t\tout = append(out, m[2])")], run('TestFixMarkerRoundTrips'))
c("the fix marker is the run marker", [(FA, 'const fixMarkerPrefix = "<!-- gonk:pipeline-fix:"', 'const fixMarkerPrefix = "<!-- gonk:pipeline-run:"')], run('TestFixMarkerRoundTrips'))
c("FixMRMarker accepts any cause key", [(FA, "\tif !causeKeyRe.MatchString(causeKey) {", "\tif false {")], run('TestFixMRMarkerFailsClosed'))
c("two disagreeing MR markers: the last one wins", [(FA, "\t\tif ok && (m[1] != fixAnchor || m[2] != causeKey) {", "\t\tif false {")], run('TestFixMRMarkerFailsClosed'))
c("an MR marker naming something that is not a fix anchor is read", [(FA, "\t\tif !fixAnchorRe.MatchString(m[1]) {\n\t\t\treturn \"\", \"\", false\n\t\t}", "\t\tif false {\n\t\t\treturn \"\", \"\", false\n\t\t}")],
  run('TestFixMRMarkerFailsClosed'))
g("not-eligible is not a fix note state", [(FA, "\tFixNoteNotEligible,\n\tFixNoteProposing, ", "\tFixNoteProposing, ")], run('TestFixNoteStatesCoverEveryFixState'))

# ============================== the record and the wire ==============================
m("record: IsZero forgets the context hash (a record with only it would be dropped)", [(ST, ' &&\n\t\tf.ContextSHA256 == ""', '')], BS, run('TestPipelineFixIsZeroCoversEveryField'))
m("record: IsZero forgets the fix state", [(ST, 'f.RunAnchor == "" && f.State == "" && f.CauseKey == ""', 'f.RunAnchor == "" && f.CauseKey == ""')], BS, run('TestPipelineFixIsZeroCoversEveryField'))
m("record: PipelineFix is stored on every record", [(ST, 'PipelineFix PipelineFix `json:"PipelineFix,omitzero"`', 'PipelineFix PipelineFix `json:"PipelineFix"`')], BS, run('TestARecordThatIsNotAFixStoresNoPipelineFixKey'))
m("record: PipelineFix is stored on every record (the fix-less golden)", [(ST, 'PipelineFix PipelineFix `json:"PipelineFix,omitzero"`', 'PipelineFix PipelineFix `json:"PipelineFix"`')], BS, run('TestPipelineRecordWithoutAFixUnchanged'))
m("wire: deny_lift is not carried", [(MF, "SourcePaths: cloneStrings(p.SourcePaths), DenyLift: classNames(p.DenyLift),", "SourcePaths: cloneStrings(p.SourcePaths),")], MA, run('TestScopePolicyRoundTripsEveryField'))
m("wire: classes are not read back", [(MF, "\t\tClasses: classValues(p.Classes),", "\t\tClasses: nil,")], MA, run('TestScopePolicyRoundTripsEveryField'))
m("wire: protected_paths are not read back", [(MF, "\t\tProtectedPaths: cloneStrings(p.ProtectedPaths), CIPaths: cloneStrings(p.CIPaths),\n\t\tTestPaths: cloneStrings(p.TestPaths), DocsPaths: cloneStrings(p.DocsPaths),\n\t\tSourcePaths: cloneStrings(p.SourcePaths), DenyLift: classValues(p.DenyLift),",
                                               "\t\tCIPaths: cloneStrings(p.CIPaths),\n\t\tTestPaths: cloneStrings(p.TestPaths), DocsPaths: cloneStrings(p.DocsPaths),\n\t\tSourcePaths: cloneStrings(p.SourcePaths), DenyLift: classValues(p.DenyLift),")],
  MA, run('TestScopePolicyRoundTripsEveryField'))
m("wire: 'says nothing' becomes 'allows no class'", [(MF, "func classNames(in []scope.Class) []string {\n\tif in == nil {\n\t\treturn nil\n\t}\n", "func classNames(in []scope.Class) []string {\n")], MA, run('TestScopePolicyRoundTripsNilAndEmpty'))
m("wire: 'allows no class' becomes 'says nothing' on the way back", [(MF, "func classValues(in []string) []scope.Class {\n\tif in == nil {\n\t\treturn nil\n\t}\n\tout := make([]scope.Class, 0, len(in))", "func classValues(in []string) []scope.Class {\n\tif len(in) == 0 {\n\t\treturn nil\n\t}\n\tout := make([]scope.Class, 0, len(in))")],
  MA, run('TestScopePolicyRoundTripsNilAndEmpty'))
m("wire: an unset profile menu becomes an empty one", [(MF, "func cloneStrings(in []string) []string {\n\tif in == nil {\n\t\treturn nil\n\t}\n", "func cloneStrings(in []string) []string {\n")], MA, run('TestScopePolicyRoundTripsNilAndEmpty'))
m("wire: allow_ci is not carried back to the gonkcfg type", [(MF, "\t\tAllowCI: p.AllowCI, CIExecChanges: p.CIExecChanges,", "\t\tCIExecChanges: p.CIExecChanges,")], MA, run('TestPipelineFixConvertsBackToEffective'))
m("meter: operator_scope is left off the response", [(SV, "\tresp.OperatorScope = &meterapi.OperatorScope{", "\t_ = &meterapi.OperatorScope{")], MS, run('TestProjectResponseCarriesTheOperatorScopeLayers'))
m("meter: the group layer is the instance's", [(SV, "\t\tGroup:    meterapi.ScopePolicyFrom(cfg.GroupFor(reg.Project).Scope),", "\t\tGroup:    meterapi.ScopePolicyFrom(cfg.Instance.Scope),")], MS, run('TestProjectResponseCarriesTheOperatorScopeLayers'))
m("meter: the instance layer is left out", [(SV, "\t\tInstance: meterapi.ScopePolicyFrom(cfg.Instance.Scope),\n", "")], MS, run('TestProjectResponseCarriesTheOperatorScopeLayers'))
m("meter: an invalid project carries an operator scope", [(SV, "\tif err != nil || resp.Effective == nil {\n\t\treturn resp, err\n\t}\n\tcfg := s.Config()", "\tif err != nil {\n\t\treturn resp, err\n\t}\n\tcfg := s.Config()")], MS, run('TestAnInvalidProjectCarriesNoOperatorScope'))

# ------------------------------------------------------------------ the runner
if '--list' in sys.argv[1:]:
    for i, (name, _, pkg, pattern) in enumerate(muts):
        print(f"[{i}] {name}    {pkg} -run {pattern}")
    sys.exit(0)

show = '--show' in sys.argv[1:]
only = [a for a in sys.argv[1:] if a != '--show']

pkgs = sorted({pkg for _, _, pkg, _ in muts})
base = subprocess.run([GO, "test", "-count=1"] + pkgs, capture_output=True, text=True)
if base.returncode != 0:
    print("the UNMUTATED packages are not green; nothing was mutated:\n" + base.stdout + base.stderr)
    sys.exit(2)

res = []
for i, (name, edits, pkg, pattern) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals = {}
    vacuous = False
    for f, _, _ in edits:
        if f not in originals:
            originals[f] = open(f).read()
    cur = dict(originals)
    for f, old, new in edits:
        if cur[f].count(old) != 1:  # not found, or ambiguous: either way not the mutation it claims to be
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
            # the test went red, but not in the fixture this mutation names
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
