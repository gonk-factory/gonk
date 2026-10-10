#!/usr/bin/env python3
"""mutate_pipefix_apply.py -- "seen red" evidence for gonk-2usw part APPLY and
for the wiring of parts AGENT and DISPATCH.

Breaks one thing at a time in the fix apply -- the static gates
(cmd/gonk-gate/broker_pipefix_apply.go, broker_pipefix_gates.go,
pkg/weaken), the envelope folded again from the record (pipefix_refold.go),
the controller-authored writes and their resume rules, the rendering
(pipefix_render.go), the fix note's ordering rule (pipefix_note.go), the
sweep's handling of an open merge request (sweep.go, sweep_pipefix.go), the
verdict labels and the dispatch-side wiring -- runs ONLY the test that guards
it, and requires it to FAIL; then restores every file byte for byte
(try/finally). A mutation whose text is not found exactly once is VACUOUS, one
the named test does not notice SURVIVED, one that does not compile NOCOMPILE:
each is a failure of this script. Before anything is mutated the unmutated
packages must be green.

The four tests plan section 11 names for this part each have a NAMED break:
  a refused batch writes anything      "a branch is created before the gates run"
                                       "the weakening gate's refusal is ignored"
  the MR gets an assignee              "the merge request is assigned to the bot"
  `Closes` appears                     "the diagnosis is linked with Closes"
  a human's quoted marker gets edited  "a human's note quoting the marker is the fix note"

NOT MUTATED, because no test can tell them from the code beside them (each is
a second check behind a first one, on purpose):
  - A2 (ValidateTargets): ValidateFix refuses a target field first (FX-SHAPE).
  - the A4 call of ValidateEditPaths: PlanFix runs the same predicates again.
  - the A5 comparison of the base blob with the blob A9 proved, alone: with
    A9 intact the two reads cannot differ in a test. It is mutated TOGETHER
    with A9's call.
  - the closing-pattern check on the finished description, alone: `inert`
    already makes a match impossible. It is mutated TOGETHER with inert.
  - an invalid CI definition at the new commit (the fake lints every
    definition as valid).

Usage: hack/mutate_pipefix_apply.py            (all mutations)
       hack/mutate_pipefix_apply.py 3 7        (only these indexes)
       hack/mutate_pipefix_apply.py --list     (names and indexes, nothing run)
       hack/mutate_pipefix_apply.py --show 3   (also print the full `go test` output)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout, and after an interrupted run check `git status`: a killed
process leaves its mutated file behind. Needs `go` on PATH (or GO=/path/to/go).
Not run by any gate (gonk-5ed1); its output is pasted into the plan's evidence log.
"""
import os
import re
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')

G = './cmd/gonk-gate/'
A = 'cmd/gonk-gate/broker_pipefix_apply.go'
GA = 'cmd/gonk-gate/broker_pipefix_gates.go'
RF = 'cmd/gonk-gate/pipefix_refold.go'
RE = 'cmd/gonk-gate/pipefix_render.go'
NO = 'cmd/gonk-gate/pipefix_note.go'
SW = 'cmd/gonk-gate/sweep.go'
S = 'cmd/gonk-gate/sweep_pipefix.go'
BD = 'cmd/gonk-gate/broker_pipefix_dispatch.go'
BI = 'cmd/gonk-gate/broker_inject.go'
BV = 'cmd/gonk-gate/broker_verdict.go'
BL = 'cmd/gonk-gate/broker_label.go'
BA = 'cmd/gonk-gate/broker_apply.go'
WK = './pkg/weaken/'
WF = 'pkg/weaken/weaken.go'


def run(test, *subs):
    parts = ['^%s$' % test]
    for s in subs:
        parts.append('^%s$' % re.escape(s.replace(' ', '_')))
    return '/'.join(parts)


ACC = 'TestFixApplyAcceptedBatchWritesExactlyTheExpectedCalls'
REF = 'TestFixApplyRefusedBatchWritesNothing'
OURS = 'TestFixApplyOurFailuresAreUnknownAndWriteNothing'
NOASG = 'TestFixMRHasNoAssigneeReviewerOrApproval'
CLOSE = 'TestFixMRNeverCloses'
NOTES = 'TestFixApplyNeverEditsAHumansNoteAndNeverMovesANoteBackwards'
SCOPE = 'TestFixApplyNeedsScopeLandsTheRequestAndNothingElse'
WEAK = 'TestFixApplyRecordsWhetherWeakeningWasDecidable'
RESUME = 'TestFixApplyResumesAfterACrash'
ADOPT = 'TestFixApplyAdoptsOnlyItsOwnCommit'
MRX = 'TestFixApplyWithAMergeRequestAlreadyOnTheBranch'
CI = 'TestFixApplyOfACIChange'
SWEEP = 'TestSweepJudgesAFixSessionAndReportsThePlansOutcome'
VERD = 'TestApplyVerdictDoesNothingWithAFixVerdict'
DNS = 'TestDispatchLandsNeedsScopeWhenNothingIsEditable'
REAL = 'TestFixDispatchThroughTheRealPreparer'
OPT = 'TestAPipelineFixOrderForAnOptedOutProjectIsRefused'
FOLD = 'TestTheEnvelopeIsFoldedAgainAndMustAgreeWithTheRecord'
WIRED = 'TestTheProductionPreparerIsWired'
E2E = 'TestPipelineFixRoundTripToAnOpenMergeRequest'
TGO = 'TestGo'

muts = []


def m(name, edits, pkg, pattern):
    muts.append((name, edits, pkg, pattern))


def g(name, edits, pattern):
    m(name, edits, G, pattern)


def off(marker_line):
    """`if <cond> { // marker` -> the same line with the condition false."""
    head, tail = marker_line.rsplit(' {', 1)
    return head + ' && false {' + tail


# ============================ the four named breaks ============================
g("[NAMED] a refused batch writes anything: a branch is created before the gates run",
  [(A, "\t// ---- A1: shape. The pack's shape AND the fix rules",
    "\t_, _ = forge.CreateBranch(ctx, rec.ProjectID, \"gonk/early\", pf.BaseSHA)\n\t// ---- A1: shape. The pack's shape AND the fix rules")],
  run(REF, "an empty batch"))
g("[NAMED] a refused batch writes anything: the weakening gate's refusal is ignored",
  [(A, "\tif wr != nil {\n\t\treturn refuse(\"A6\", wr)\n\t}", "\tif wr != nil && false {\n\t\treturn refuse(\"A6\", wr)\n\t}")],
  run(REF, "a test deleted"))
g("[NAMED] the MR gets an assignee: the merge request is assigned to the bot",
  [(A, "\t\t\tTitle: a.title(), Description: desc, Labels: labels,", "\t\t\tTitle: a.title(), Description: desc, Labels: labels, AssigneeID: lander.me.ID,")],
  run(NOASG))
g("[NAMED] `Closes` appears: the diagnosis is linked with Closes",
  [(RE, 'return fmt.Sprintf("Related to #%d", ld.issue.IID), nil // render:related-issue', 'return fmt.Sprintf("Closes #%d", ld.issue.IID), nil // render:related-issue')],
  run(CLOSE))
g("[NAMED] a human's quoted marker gets edited: a human's note quoting the marker is the fix note",
  [(NO, "\t\tif !l.botNote(n) {\n\t\t\tcontinue\n\t\t}\n\t\tif states := pipelinectx.FixMarkerStates", "\t\tif false {\n\t\t\tcontinue\n\t\t}\n\t\tif states := pipelinectx.FixMarkerStates")],
  run(NOTES, "a human quoting the marker"))

# ================================ the static gates ================================
L = "\tif verr := effects.Validate(batch, shape); verr != nil { // gate:A1-shape"
g("A1: the pack's effect shape is not applied", [(A, L, off(L))], run(REF, "eleven edits"))
L = "\tif verr := effects.ValidateFix(batch); verr != nil { // gate:A1-fix"
g("A1: ValidateFix is not run (the shape alone accepts an empty batch)", [(A, L, off(L))], run(REF, "an empty batch with no verdict at all"))

L = "\tif cfg.Effective == nil || !cfg.Effective.Actions.PipelineFixes || !cfg.Effective.Actions.Pipelines {"
g("A3: a project that opted out is still applied to", [(RF, L, "\tif cfg.Effective == nil || !cfg.Effective.Actions.Pipelines {")],
  run(OURS, "the project opted out after the session started"))
g("A3 at dispatch: a project with pipeline_fixes off gets a session", [(RF, L, "\tif cfg.Effective == nil || !cfg.Effective.Actions.Pipelines {")],
  run(OPT, "a record exists, pipeline_fixes off"))
g("A3 at dispatch: a project with pipelines off gets a session", [(RF, L, "\tif cfg.Effective == nil || !cfg.Effective.Actions.PipelineFixes {")],
  run(OPT, "a record exists, pipelines off"))
L = "\tif !slices.Equal(env.classes, pf.Envelope) { // refold:classes"
g("A3: the recorded envelope is believed over the fold", [(RF, L, off(L))], run(FOLD))
L = "\tif !slices.Equal(env.liftNames(), pf.Lift) { // refold:lift"
g("A3: a lift named on the record is not compared with the lift minted now", [(RF, L, off(L))], run(FOLD))
g("A3: the caps on the record are believed over the fold",
  [(RF, "\tif c := eff.Caps; c.MaxFiles != pf.MaxFiles || c.MaxLines != pf.MaxLines || // refold:caps", "\tif c := eff.Caps; false && c.MaxFiles != pf.MaxFiles || false && c.MaxLines != pf.MaxLines || // refold:caps")],
  run(OURS, "an operator tightened a cap after the session started"))
g("A3: a recorded file outside pipeline_fix.paths is still a file the fix may touch",
  [(RF, "\t\tif !eff.InBead(f) {", "\t\tif false {")], run(FOLD))
g("A3: the lift's opt-in is taken as given (allow_ci not read)",
  [(RF, "\t\tLiftOptIn: fixCfg.LiftOptIn(),", "\t\tLiftOptIn: []scope.Class{scope.ClassCI},")], run(FOLD))

L = "\tif !pf.Context.Recorded() || pipefixContextHash(pf.Context) != pf.ContextSHA256 { // gate:A9-record"
g("A9: the recorded context is not checked against its own hash", [(A, L, off(L))], run(OURS, "the recorded context was altered"))
L = "\tif pf.Context.BaseSHA != pf.BaseSHA || !glab.IsCommitSHA(pf.BaseSHA) { // gate:A9-base"
g("A9: a context for another commit is accepted", [(A, L, "\tif false { // gate:A9-base")], run(OURS, "the context names another commit than the fix is written against"))
g("A9: the context is not re-derived, and the base blob is not compared with what was shown",
  [(A, "\tshown, verr := verifyPipefixContext(ctx, forge, rec.ProjectID, pf.Context, run.PipelineDiagnosis, env, maxEdits) // gate:A9",
    "\tshown, verr := effects.BaseBlobs(nil), error(nil) // gate:A9\n\t_ = maxEdits"),
   (A, "\t\tif !bytes.Equal(blob, shown[p]) { // gate:A5-same", "\t\tif !bytes.Equal(blob, shown[p]) && false { // gate:A5-same")],
  run(OURS, "the diagnosis on the run record changed"))

L = "\tif r := shownRefusal(batch, pf.Context); r != nil { // gate:A4-shown"
g("A4: an edit to a file the session was not given is left to the later gates", [(A, L, off(L))],
  run(REF, "an edit to a file the diagnosis did not cite"))
g("A4: a file the prompt said may NOT be changed is editable", [(GA, "\t\tcase !f.Editable: // shown:editable", "\t\tcase false: // shown:editable")],
  run(REF, "an edit to a cited file the session was told it may not change"))
g("A4: a path the context does not hold is editable", [(GA, "\t\tcase !cited:", "\t\tcase !cited && false:")],
  run(REF, "the project's own gonk configuration"))

L = "\tif r := modeRefusal(facts.Refusals); r != nil { // gate:A5-mode"
g("A5: a symlink at the edit path is not refused by its mode", [(A, L, off(L))], run(REF, "a symlink at the edit path"))
g("A5: a submodule at the edit path is not refused by its mode", [(A, L, off(L))], run(REF, "a submodule at the edit path"))
g("A5: the base is read as plain blobs (modes never looked at)",
  [(A, "\tfacts, berr := basefacts.Read(ctx, forge, rec.ProjectID, pf.BaseSHA, paths, nil) // gate:A5-read",
    "\tfacts, berr := &basefacts.Facts{Blobs: shown}, error(nil) // gate:A5-read")],
  run(REF, "a symlink at the edit path"))
g("A6: the controller's own secrets are not checked",
  [(A, "\tplan, perr := effects.PlanFix(batch, env, base, d.FixSecrets) // gate:A6-plan", "\tplan, perr := effects.PlanFix(batch, env, base, nil) // gate:A6-plan")],
  run(REF, "the bot's own token in an added line"))

g("SC-WEAKEN: a skip added to a Go test is not refused",
  [(A, "\tif wr != nil {\n\t\treturn refuse(\"A6\", wr)\n\t}", "\tif wr != nil && false {\n\t\treturn refuse(\"A6\", wr)\n\t}")], run(REF, "a skip added"))
g("SC-WEAKEN: only test files are judged (a source file's build tag passes)",
  [(GA, "\t\tif dec.Class == scope.ClassTest || finding.Sub == weaken.BuildConstraint { // weaken:refuse", "\t\tif dec.Class == scope.ClassTest { // weaken:refuse")],
  run(REF, "a source file built out with a tag"))
g("SC-WEAKEN: only build tags are judged (a skip in a test passes)",
  [(GA, "\t\tif dec.Class == scope.ClassTest || finding.Sub == weaken.BuildConstraint { // weaken:refuse", "\t\tif finding.Sub == weaken.BuildConstraint { // weaken:refuse")],
  run(REF, "a skip added"))
g("SC-WEAKEN: a non-Go file under the go profile is 'decided' (the Go predicate is asked about a golden file)",
  [(GA, "\t\tif !dec.WeakeningDecidable() || !isGo { // weaken:decidable", "\t\tif !dec.WeakeningDecidable() || !isGo && false { // weaken:decidable")],
  run(WEAK, "a non-Go test file under the go profile: not decidable"))
g("SC-WEAKEN: an undecidable path is not recorded (python reads as decided)",
  [(GA, "\t\t\tw.undecided = append(w.undecided, f.Path)\n", "")], run(WEAK, "python: not decidable"))
g("SC-WEAKEN: the description claims the check for an undecidable change",
  [(RE, "\tcase len(f.weak.undecided) > 0:", "\tcase false:")], run(WEAK, "python: not decidable"))


def w(name, edits, sub):
    m(name, edits, WK, run(TGO, sub))


w("weaken: a result that does not parse is not a finding",
  [(WF, "\tif err != nil {\n\t\treturn Finding{Sub: Unparseable, Detail: \"the file does not parse as Go after the change\"} // weaken:unparseable\n\t}",
    "\tif err != nil {\n\t\treturn Finding{}\n\t}")], "left unparseable")
w("weaken: build constraints are not compared", [(WF, "\tif bc, ac := constraints(base), constraints(patched); bc != ac { // weaken:constraint", "\tif bc, ac := constraints(base), constraints(patched); bc != ac && false { // weaken:constraint")],
  "a go:build tag added")
w("weaken: a +build line is not a constraint", [(WF, "\t\tif constraint.IsGoBuild(t) || constraint.IsPlusBuild(t) {", "\t\tif constraint.IsGoBuild(t) {")], "a +build tag added")
w("weaken: a removed test is not a finding", [(WF, "\t\tif _, ok := af[name]; !ok { // weaken:removed", "\t\tif _, ok := af[name]; !ok && false { // weaken:removed")], "a test deleted")
w("weaken: a test turned into a method still counts as present", [(WF, "\t\tif !ok || fn.Recv != nil {\n\t\t\tcontinue\n\t\t}\n\t\tfor _, prefix", "\t\tif !ok {\n\t\t\tcontinue\n\t\t}\n\t\tfor _, prefix")],
  "a test turned into a method")
w("weaken: benchmarks are not tests", [(WF, '[]string{"Test", "Benchmark", "Fuzz", "Example"}', '[]string{"Test", "Fuzz", "Example"}')], "a benchmark deleted")
w("weaken: an added skip is not a finding", [(WF, "\t\tif n > bs[name] { // weaken:skip", "\t\tif n > bs[name] && false { // weaken:skip")], "t.Skip added")
w("weaken: Skipf is not a skip", [(WF, '\t\t\t\tcase "Skip", "Skipf", "SkipNow":', '\t\t\t\tcase "Skip", "SkipNow":')], "t.Skipf added")
w("weaken: SkipNow is not a skip", [(WF, '\t\t\t\tcase "Skip", "Skipf", "SkipNow":', '\t\t\t\tcase "Skip", "Skipf":')], "t.SkipNow added")
w("weaken: any skip at all means 'no new skip' (presence, not count)", [(WF, "\t\tif n > bs[name] { // weaken:skip", "\t\tif n > 0 && bs[name] == 0 { // weaken:skip")],
  "a second skip added beside an existing one")
w("weaken: only test functions are searched for skips", [(WF, "\tbs, as := skipCounts(before), skipCounts(after)", "\tbs, as := skipCounts(before), skipCounts(after)\n\tfor k := range as {\n\t\tif _, ok := af[k]; !ok {\n\t\t\tdelete(as, k)\n\t\t}\n\t}")],
  "a skip added in a helper every test calls")
w("weaken: a bare return added first is not a finding", [(WF, "leadingReturn(fn) && !leadingReturn(old) { // weaken:return", "leadingReturn(fn) && !leadingReturn(old) && false { // weaken:return")],
  "a bare return added first")

L = "\tif r := ciPreCheck(plan); r != nil { // gate:A7"
g("A7: the CI pre-check is not run", [(A, L, off(L))], run(REF, "allow_failure added to the job"))
g("A7: the pre-check runs on every file, not only ci files", [(GA, "\t\tif fd.Class != scope.ClassCI { // cipre:class", "\t\tif false { // cipre:class")], run('TestCIPreCheckOnlyReadsCIFiles'))
g("A7: a definition that no longer parses is not refused",
  [(GA, "\t\tif err := yaml.Unmarshal(patched[fd.Path], &doc); err != nil { // cipre:parse", "\t\tif err := yaml.Unmarshal(patched[fd.Path], &doc); err != nil && false { // cipre:parse")],
  run(REF, "a definition that no longer parses"))
g("A7: the added lines are not scanned", [(GA, "\t\t\t\tif c.re.MatchString(line) { // cipre:line", "\t\t\t\tif c.re.MatchString(line) && false { // cipre:line")],
  run(REF, "the job made manual"))
g("A7: an appended or-true is not a pattern", [(GA, '\t{"or-true", regexp.MustCompile(', '\t{"or-true-off", regexp.MustCompile(`^$`)},\n\t{"or-true", regexp.MustCompile(`^$`)}, {"x", regexp.MustCompile(')],
  run(REF, "the exit code swallowed"))

L = "\tif r := fixTextGates(plan); r != nil { // gate:A8\n"
g("A8: the rationale is not gated", [(A, L, "\tif r := fixTextGates(plan); r != nil && false { // gate:A8\n")], run(REF, "a rationale carrying a quick action"))
L = "\t\tif r := fixTextGates(plan); r != nil { // gate:A8-scope"
g("A8: a scope request's text is not gated", [(A, L, off(L))], run(REF, "a scope request whose reason carries a gonk marker"))
L = "\tif gonkMarkerRe.MatchString(text) { // text:marker"
g("A8: a gonk marker in the rationale is rendered", [(GA, L, off(L))], run(REF, "a rationale carrying a gonk marker"))
L = "\tif _, kinds := pipelinectx.Redact(text); len(kinds) > 0 { // text:secret"
g("A8: a secret in the rationale is rendered", [(GA, L, off(L))], run(REF, "a rationale carrying a secret"))
g("A8: a quick action in the rationale is rendered",
  [(GA, "err != nil { // text:comment", "err != nil && false { // text:comment")], run(REF, "a rationale carrying a quick action"))
g("A8: only a scope request's paths are gated, not its reason",
  [(GA, "\tif r := textGate(\"scope request's reason\", plan.Request.Reason); r != nil {", "\tif r := textGate(\"scope request's reason\", plan.Request.Reason); r != nil && false {")],
  run(REF, "a scope request whose reason carries a quick action"))

# ================================ the writes ================================
g("write: the branch is created from the target BRANCH, not the pinned SHA",
  [(A, "a.f.CreateBranch(ctx, pid, a.branch, pf.BaseSHA); cerr != nil { // apply:branch", "a.f.CreateBranch(ctx, pid, a.branch, pf.TargetBranch); cerr != nil { // apply:branch")], run(ACC))
g("write: the commit creates files instead of updating them",
  [(A, 'actions = append(actions, glab.CommitAction{Action: "update", FilePath: f.Path, Content: string(f.Content)})', 'actions = append(actions, glab.CommitAction{Action: "create", FilePath: f.Path, Content: string(f.Content)})')], run(ACC))
g("write: the commit message carries the agent's rationale",
  [(A, '\tmsg := a.title() + "\\n\\n" + fmt.Sprintf("gonk-fix: %s attempt=%d", a.rec.BeadAnchor, max(a.rec.Attempt, 1))', '\tmsg := a.title() + "\\n\\n" + a.plan.Reason + "\\n\\n" + fmt.Sprintf("gonk-fix: %s attempt=%d", a.rec.BeadAnchor, max(a.rec.Attempt, 1))')],
  run(ACC))
g("write: the commit lands on the target branch", [(A, "\t\tBranch: a.branch, CommitMessage: a.commitMessage(), Actions: actions,", "\t\tBranch: a.rec.PipelineFix.TargetBranch, CommitMessage: a.commitMessage(), Actions: actions,")], run(ACC))
g("write: a CI change runs on push under the default (no [skip ci])",
  [(A, '\ta.skip = changesCI(a.plan) && pf.CIExecChanges != "auto" // apply:skip-ci', '\ta.skip = false // apply:skip-ci')], run(CI, "manual-run (the default): skip ci, awaiting-run"))
g("write: [skip ci] even when the project chose auto",
  [(A, '\ta.skip = changesCI(a.plan) && pf.CIExecChanges != "auto" // apply:skip-ci', '\ta.skip = changesCI(a.plan) // apply:skip-ci')], run(CI, "auto: no skip ci, verifying"))
g("write: every fix is committed with [skip ci]",
  [(A, '\ta.skip = changesCI(a.plan) && pf.CIExecChanges != "auto" // apply:skip-ci', '\ta.skip = true // apply:skip-ci')], run(ACC))
g("5.4 step 2: a definition violation still gets a merge request",
  [(A, "\tcase delta.Kind == cidef.KindViolation: // apply:cidef-violation", "\tcase false: // apply:cidef-violation")],
  run(CI, "a gating change is refused after the commit and the branch removed"))
g("5.4 step 2: the refused commit's branch is left behind",
  [(A, "\tcase delta.Kind == cidef.KindViolation: // apply:cidef-violation\n\t\ta.dropBranch(ctx, mr)\n", "\tcase delta.Kind == cidef.KindViolation: // apply:cidef-violation\n")],
  run(CI, "a gating change is refused after the commit and the branch removed"))
g("5.4 step 2: ci_exec_changes: deny is not acted on",
  [(A, '\tcase delta.Kind == cidef.KindExecChanging && pf.CIExecChanges == "deny": // apply:cidef-deny', "\tcase false: // apply:cidef-deny")],
  run(CI, "deny: an execution-changing edit leaves nothing and needs scope"))
g("5.4 step 2: the definition is not compared when no ci path changed",
  [(A, "\t\t\tif (berr == nil) != (herr == nil) || !bytes.Equal(b, h) { // delta:same", "\t\t\tif (berr == nil) != (herr == nil) || !bytes.Equal(b, h) && false { // delta:same")],
  run(CI, "a non-CI change in a project whose definition moved is not applied"))
g("5.4 step 2: the delta of a CI change is not computed (reported as none)",
  [(A, "\tif !changesCI(a.plan) { // delta:shortcut", "\tif true { // delta:shortcut")], run(CI, "an execution-changing edit is labelled"))
g("write: an execution-changing fix is not labelled",
  [(A, "\tif delta.Kind == cidef.KindExecChanging {\n\t\tlabels = append(labels, labelCIExecChanged)\n\t}", "\tif false {\n\t\tlabels = append(labels, labelCIExecChanged)\n\t}")],
  run(CI, "an execution-changing edit is labelled"))
g("write: the merge request targets the default branch, not the fix's target",
  [(A, "\t\t\tSourceBranch: a.branch, TargetBranch: pf.TargetBranch,", '\t\t\tSourceBranch: a.branch, TargetBranch: "main",')], run(E2E, "merge-request"))
g("write: the merge request carries no labels", [(A, "\t\t\tTitle: a.title(), Description: desc, Labels: labels,", "\t\t\tTitle: a.title(), Description: desc,")], run(ACC))
g("write: the merge request is created with remove_source_branch",
  [(A, "\t\t\tTitle: a.title(), Description: desc, Labels: labels,", "\t\t\tTitle: a.title(), Description: desc, Labels: labels, RemoveSourceBranch: true,")], run(NOASG))
g("write: the description lacks the fix MR marker", [(RE, "\tb.WriteString(marker)\n\tdesc := b.String()", "\t_ = marker\n\tdesc := b.String()")], run(ACC))
g("write: the description lacks the status block",
  [(RE, '\tb.WriteString(fixStatusOpen + "\\n" + fixStatusLine(f.state, f.commit) + "\\n" + fixStatusClose + "\\n\\n")', '\tb.WriteString(fixStatusLine(f.state, f.commit) + "\\n\\n")')], run(ACC))
g("write: an MR target's diagnosis is linked as an issue",
  [(RE, 'return fmt.Sprintf("Related to !%d", ld.target.MRIID), nil // render:related-mr', 'return fmt.Sprintf("Related to #%d", ld.target.MRIID), nil // render:related-mr')], run(CLOSE))
g("write: agent and project text reaches the description able to reference",
  [(RE, '\ts = inertHash.ReplaceAllString(s, "# $1")\n\ts = strings.ReplaceAll(s, "://", ": //")\n\treturn inertKey.ReplaceAllString(s, "$1- $2")', "\treturn s")], run(CLOSE))
g("write: ... and the finished description is not checked either",
  [(RE, '\ts = inertHash.ReplaceAllString(s, "# $1")\n\ts = strings.ReplaceAll(s, "://", ": //")\n\treturn inertKey.ReplaceAllString(s, "$1- $2")', "\treturn s"),
   (RE, '\tif m := closingPattern.FindString(desc); m != "" { // render:closing', '\tif m := closingPattern.FindString(desc); m != "" && false { // render:closing')], run(CLOSE))
g("write: a tracker key in agent text is left able to close", [(RE, '\treturn inertKey.ReplaceAllString(s, "$1- $2")', "\treturn s"),
   (RE, '\tif m := closingPattern.FindString(desc); m != "" { // render:closing', '\tif m := closingPattern.FindString(desc); m != "" && false { // render:closing')], run(CLOSE))
# gonk-htl3: a NAME is shown exactly and GitLab can read nothing into it
NAMES = "TestFixMRDescriptionShowsNamesExactly"
IDENT = "TestIdentShowsANameExactlyAndGitLabCanReadNothingIntoIt"
VT = 'cmd/gonk-gate/pipefix_verify_text.go'
g("names: THE DEFECT -- the criterion table's job, stage and signature go through inert again",
  [(RE, 'fmt.Fprintf(&b, "| %s | %s | %s |\\n", ident(t.Name), ident(t.Stage), ident(sig))',
    'fmt.Fprintf(&b, "| %s | %s | %s |\\n", codeSpan(inert(t.Name)), codeSpan(inert(t.Stage)), codeSpan(inert(sig)))')], run(NAMES))
g("names: the branch goes through inert again",
  [(RE, "pipelineRef(a.rec, pf.BasePipelineID), ident(pf.TargetBranch))", "pipelineRef(a.rec, pf.BasePipelineID), codeSpan(inert(pf.TargetBranch)))")], run(NAMES))
g("names: the rung goes through inert again",
  [(RE, '\treturn "model " + ident(rung)', "\treturn inert(rungText(rung))")], run(NAMES))
g("names: the session goes through inert again",
  [(RE, "fixRungText(a.rec.Rung), ident(a.rec.SessionID))", "fixRungText(a.rec.Rung), codeSpan(inert(a.rec.SessionID)))")], run(NAMES))
g("names: the other failing jobs go through inert again",
  [(RE, "\t\t\tnames = append(names, ident(n))", "\t\t\tnames = append(names, codeSpan(inert(n)))")], run(NAMES))
g("names: a changed path goes through inert again",
  [(RE, "ident(fd.Path), fd.Class, fd.Added, fd.Removed)", "codeSpan(inert(fd.Path)), fd.Class, fd.Added, fd.Removed)")], run("TestFixApplyAcceptedBatchWritesExactlyTheExpectedCalls"))
g("names: the status block's job lines go through inert again",
  [(VT, "jobFindingLines(s.res, ident)", "jobFindingLines(s.res, func(n string) string { return codeSpan(inert(n)) })")], run(NAMES))
g("names: a name is shown RAW in a code span (a job called 'closes #1' can close)",
  [(RE, 'fmt.Fprintf(&b, "| %s | %s | %s |\\n", ident(t.Name), ident(t.Stage), ident(sig))',
    'fmt.Fprintf(&b, "| %s | %s | %s |\\n", codeSpan(t.Name), codeSpan(t.Stage), codeSpan(sig))')], run(CLOSE))
g("names: ... and raw other failing jobs, with the finished description not checked either",
  [(RE, "\t\t\tnames = append(names, ident(n))", "\t\t\tnames = append(names, codeSpan(n))"),
   (RE, '\tif m := closingPattern.FindString(desc); m != "" { // render:closing', '\tif m := closingPattern.FindString(desc); m != "" && false { // render:closing')], run(NAMES))
g("names: ident leaves a hyphen in the raw text (a tracker key can form)",
  [(RE, "|| r == ' ' { // ident:plain", "|| r == ' ' || r == '-' { // ident:plain")], run(IDENT))
g("names: ident leaves a '#' in the raw text (an issue reference can form)",
  [(RE, "|| r == ' ' { // ident:plain", "|| r == ' ' || r == '#' { // ident:plain")], run(IDENT))
g("names: ident leaves markup characters in the raw text",
  [(RE, "|| r == ' ' { // ident:plain", "|| r == ' ' || r == '<' || r == '|' || r == '_' { // ident:plain")], run(IDENT))
g("names: ident ALTERS a name (a character it encodes is shown as another)",
  [(RE, '\t\tfmt.Fprintf(&b, "&#x%x;", r)', '\t\tfmt.Fprintf(&b, "&#x%x;", r|0x20)')], run(IDENT))
g("names: ident drops what it does not show plainly",
  [(RE, '\t\tfmt.Fprintf(&b, "&#x%x;", r)', '\t\t_ = r')], run(NAMES))
g("names: ident is not a code element (the name is prose)",
  [(RE, '\tb.WriteString("<code>")\n\tfor _, r := range s {', '\tfor _, r := range s {'), (RE, '\tb.WriteString("</code>")\n\treturn b.String()', '\treturn b.String()')], run(NAMES))
g("prose: A CLOSING PATTERN IN MODEL TEXT SURVIVES (the rationale is not made inert, and the description is not checked)",
  [(RE, "\tb.WriteString(fence(inert(a.plan.Reason)) + \"\\n\")", "\tb.WriteString(fence(a.plan.Reason) + \"\\n\")"),
   (RE, '\tif m := closingPattern.FindString(desc); m != "" { // render:closing', '\tif m := closingPattern.FindString(desc); m != "" && false { // render:closing')], run(NAMES))
g("prose: the rationale is not made inert (the final check alone must stop it)",
  [(RE, "\tb.WriteString(fence(inert(a.plan.Reason)) + \"\\n\")", "\tb.WriteString(fence(a.plan.Reason) + \"\\n\")")], run(NAMES))
g("prose: inert's tracker-key rule is upper-case only (a lower-case key in the rationale is left alone)",
  [(RE, "inertKey  = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_]+)-(\\d)`)", "inertKey  = regexp.MustCompile(`([A-Z][A-Z0-9_]+)-(\\d)`)")], run("TestInertStillDefusesProse"))
g("write: the rationale's fence can be closed by the rationale", [(RE, '\tf := strings.Repeat("`", longest+1)', '\tf := strings.Repeat("`", 3)')], run(CLOSE))
g("write: the job name reaches the title raw", [(A, "\t\tjob = refSafe(pf.Targets[0].Name)", "\t\tjob = pf.Targets[0].Name")], run(CLOSE))
g("write: the title reads 'fix <name>' (a job named like a tracker key closes it)",
  [(A, '\treturn fmt.Sprintf("gonk: fix job %s on %s", job, refSafe(pf.TargetBranch))', '\treturn fmt.Sprintf("gonk: fix %s on %s", job, refSafe(pf.TargetBranch))')], run(CLOSE))

# ---- the record
for field, line in [
    ("State", "\tfix.State = string(state)\n"),
    ("AppliedAttempt", "\tfix.AppliedAttempt = a.rec.Attempt\n"),
    ("FixCommitSHA", "\tfix.FixCommitSHA = commit\n"),
    ("MRIID", "\tfix.MRIID = mr.IID\n"),
    ("ChangedFiles", "\tfix.ChangedFiles = changedFiles(a.plan)\n"),
    ("Weakening", "\tfix.Weakening, fix.WeakeningUndecided = a.weak.state(), a.weak.undecided\n"),
    ("CIDelta", "\tfix.CIDelta = string(delta.Kind)\n"),
    ("VerifyDeadline", "\tfix.VerifyDeadline = a.now.Add(time.Duration(pf.VerifyTimeoutSeconds) * time.Second)\n"),
]:
    g("record: %s is not set by the apply" % field, [(A, line, "")], run(ACC))
g("record: a refusal's rule code is not recorded", [(A, "\t\trecp.PipelineFix.Refusal = r.Code()\n", "")], run(SWEEP, "a refused batch is gate-failed and writes nothing"))

# ---- the notes
g("note: the apply's note is written as a dispatch-time note (it cannot replace `proposing`)",
  [(A, "\tnote := fixNote{State: string(state), Text: fixOpenedText(a.rec, mr.IID, commit, state), Apply: true}", "\tnote := fixNote{State: string(state), Text: fixOpenedText(a.rec, mr.IID, commit, state)}")], run(ACC))
g("note: the apply moves a later part's note backwards",
  [(NO, "\treturn n.Apply && fixNoteApplyReplaces[existing] // note:backwards", "\treturn n.Apply // note:backwards")], run(NOTES, "a note already fixed"))
g("note: needs-scope does not label the landing for a person",
  [(A, "\t\tlander.addLandingLabel(ctx, run, ld, verdictLabel(effects.VerdictNeedsScope))\n", "\t\t_ = ld\n")], run(SCOPE))
g("note: needs-scope sets no fix state", [(A, "\trecp.PipelineFix.State = string(pipefix.StateNeedsScope)\n\trecp.PipelineFix.Refusal = \"\"\n\td.Log.Info(\"sweep: pipeline fix needs scope", "\trecp.PipelineFix.Refusal = \"\"\n\td.Log.Info(\"sweep: pipeline fix needs scope")], run(SCOPE))
g("note: the scope request's reason is rendered outside a fence",
  [(RE, 'gonk has not checked that it is true):\\n\\n" + fence(req.Reason))', 'gonk has not checked that it is true):\\n\\n" + req.Reason)')], run(SCOPE))

# ---- resume and adoption
g("resume: an existing commit is committed over", [(A, "\tif head != parent { // apply:adopt", "\tif false { // apply:adopt")],
  run(RESUME, "after the commit, before the merge request"))
g("resume: an existing merge request is not adopted (a second one is opened)", [(A, "\t\t\topen = m\n", "")],
  run(RESUME, "after the merge request, before the note"))
L = "\tif strings.TrimSpace(c.Message) != strings.TrimSpace(a.commitMessage()) { // adopt:message"
g("adopt: any commit on the right parent with the right content is ours", [(A, L, off(L))], run(ADOPT, "a commit with another message"))
g("adopt: the parent is not checked", [(A, "\tif len(c.ParentIDs) != 1 || c.ParentIDs[0] != parent { // adopt:parent", "\tif false { // adopt:parent")], run(ADOPT, "our message on another parent"))
g("adopt: a merge commit is adopted", [(A, "\tif len(c.ParentIDs) != 1 || c.ParentIDs[0] != parent { // adopt:parent", "\tif len(c.ParentIDs) < 1 || c.ParentIDs[0] != parent { // adopt:parent")], run(ADOPT, "our message with two parents"))
g("adopt: the content is not checked", [(A, "\t\tif !bytes.Equal(got, f.Content) { // adopt:content", "\t\tif !bytes.Equal(got, f.Content) && false { // adopt:content")], run(ADOPT, "our message and parent, other content"))
g("mr: someone else's open merge request on the fix branch is adopted", [(A, "\t\t\tif !ours { // apply:mr-foreign", "\t\t\tif false { // apply:mr-foreign")], run(MRX, "open, and not the bot's"))
g("mr: a merge request from another project counts as ours",
  [(A, "\t\tours := m.Author.ID == me.ID && m.SourceProjectID == pid && m.TargetProjectID == pid", "\t\tours := m.Author.ID == me.ID && m.TargetProjectID == pid")], run(MRX, "open, from another project"))
g("mr: a closed fix merge request is re-proposed", [(A, "\tcase closed != nil: // apply:mr-closed", "\tcase false: // apply:mr-closed")], run(MRX, "closed by a person"))
g("mr: a merged fix merge request is re-proposed", [(A, "\tcase merged != nil:\n\t\treturn merged, \"merged\", nil", "\tcase false:\n\t\treturn merged, \"merged\", nil")], run(MRX, "already merged"))

# ================================ the sweep ================================
g("sweep: an accepted fix is reported as success before anything judged it",
  [(SW, "\t\t\tcase applied && agent == pipefixAgent && fixOpenStates[rec.PipelineFix.State]:", "\t\t\tcase false:")],
  run(SWEEP, "an accepted fix reports no outcome and waits for the exit check"))
g("sweep: a fix whose merge request is open is judged again as a session",
  [(SW, '\tif rec.Trigger == atags.TriggerPipelineFix && rec.PipelineFix.State != "" {', '\tif rec.Trigger == atags.TriggerPipelineFix && rec.PipelineFix.State != "" && false {')],
  run(SWEEP, "an accepted fix reports no outcome and waits for the exit check"))
g("sweep: the finished fix session is left running", [(S, "\t\t\"verify_deadline\", rec.PipelineFix.VerifyDeadline)\n\tcloseSession(ctx, d, rec)\n", "\t\t\"verify_deadline\", rec.PipelineFix.VerifyDeadline)\n")],
  run(SWEEP, "an accepted fix reports no outcome and waits for the exit check"))
g("sweep: the open fix is not written to the store", [(S, "\tif _, err := d.Store.PutIf(ctx, rec, readRevision); errors.Is(err, beadstore.ErrStale) {\n\t\td.Log.Warn(\"sweep: pipeline fix: the record changed after this pass read it",
                                                        "\tif _, err := error(nil), error(nil); errors.Is(err, beadstore.ErrStale) {\n\t\td.Log.Warn(\"sweep: pipeline fix: the record changed after this pass read it")],
  run(SWEEP, "an accepted fix reports no outcome and waits for the exit check"))
g("sweep: a fix batch goes down the issue-shaped chain", [(BA, "\tif fbKinds != nil { // apply:own-chain", "\tif fbKinds != nil && agent != pipefixAgent { // apply:own-chain")],
  run(SWEEP, "needs-scope is success and terminal"))
g("sweep: a fix batch goes down the DIAGNOSIS chain", [(BA, "\t\tif agent == pipefixAgent {\n\t\t\tapply = applyPipefixBatch\n\t\t}\n", "")],
  run(SWEEP, "needs-scope is success and terminal"))
g("verdict: a fix verdict labels and acts on an issue",
  [(BV, "\tif v == effects.VerdictFix || v == effects.VerdictNeedsScope {", "\tif false {")], run(VERD))
g("verdict: the fix label is not the reserved one", [(BV, '\teffects.VerdictFix:        "gonk::pipeline-fix",', '\teffects.VerdictFix:        "gonk::fix",')], run(SCOPE))
g("verdict: needs-scope has no label of its own", [(BV, "\teffects.VerdictNeedsScope: labelNeedsHuman,\n", "")], run(SCOPE))
g("labels: the fix merge request's labels are not reserved", [(BL, "\tfor _, l := range fixMRLabels {\n\t\treserve(l)\n\t}\n", "")], run(SCOPE))

# ================================ the wiring ================================
g("wiring: the production preparer is nil again", [(BD, "var pipefixPrepare pipefixPreparer = prepareFixSession", "var pipefixPrepare pipefixPreparer")], run(WIRED))
g("wiring: pipeline-fix is not in agentForTrigger", [(BI, '\t"pipeline-fix": pipefixAgent,\n', "")], run(WIRED))
g("wiring: the context the session was shown is not recorded", [(BI, "\t\tbase.PipelineFix.Context = fs.Context\n", "")], run(REAL))
g("wiring: the recorded hash is not the context's", [(BD, "\t\tContextSHA256: pipefixContextHash(out.context), Context: out.context}, nil", '\t\tContextSHA256: "c0ffee", Context: out.context}, nil')], run(REAL))
g("wiring: the preparer is handed the fix record as the run", [(BD, "\treq, err := pipefixRequestFromRecord(in.Run, pf.BaseSHA, pf.BasePipelineID, env, in.Fix.Attempt)", "\treq, err := pipefixRequestFromRecord(in.Fix, pf.BaseSHA, pf.BasePipelineID, env, in.Fix.Attempt)")], run(REAL))
g("wiring: nothing editable at the base is an ordinary failed prepare (it re-fires forever)",
  [(BI, "\t\tif errors.Is(perr, errPipefixNothingEditable) {", "\t\tif false {")], run(DNS))
g("wiring: needs-scope at dispatch records no fix state", [(BD, "\trec.State = beadstore.StateDone\n\trec.PipelineFix.State = string(pipefix.StateNeedsScope)\n", "\trec.State = beadstore.StateDone\n")], run(DNS))
g("wiring: needs-scope at dispatch writes no note",
  [(BD, "\td.postFixNote(ctx, base, d.fixRun, fixNote{State: string(pipefix.StateNeedsScope), Text: fixNothingEditableText(base)}, true)\n", "")], run(DNS))

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
print(f"killed={len(res)-len(bad)} not-killed={len(bad)} (of {len(res)} run, {len(muts)} defined)")
sys.exit(1 if bad else 0)
