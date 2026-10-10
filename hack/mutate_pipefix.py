#!/usr/bin/env python3
"""mutate_pipefix.py -- "seen red" evidence for gonk-2usw part JUDGE (pkg/pipefix).

Disables one predicate at a time in pkg/pipefix, runs ONLY the test that guards
it (each mutation names its own -run pattern, so a kill means that predicate's
own fixture went red, not that some unrelated test noticed), and requires it to
FAIL; then restores every file byte for byte (try/finally). A mutation whose
text is not found is VACUOUS, one the named test does not notice SURVIVED, and
one that does not compile NOCOMPILE -- each is a failure of this script. Before
anything is mutated the unmutated package must be green, or a kill would mean
nothing.

The rows are the plan's: E1-E12 of section 3.2, J0-J5 of section 6.3, the
mappings of 7.1 / 7.3. The three tests section 11 names for this part each
have a NAMED break, marked [NAMED] in the list:
  conservative-across-pipelines     "only the newest candidate pipeline is judged"
  a read failure yields no verdict  "an unread trace is read as an empty log"
  a human retry is not PASSED       "only the latest attempt of a job is judged"

Usage: hack/mutate_pipefix.py            (all mutations)
       hack/mutate_pipefix.py 3 7        (only these indexes)
       hack/mutate_pipefix.py --list     (names and indexes, nothing run)
       hack/mutate_pipefix.py --show 3   (also print the full `go test` output)
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
P = 'pkg/pipefix/'
E = P + 'eligible.go'
J = P + 'judge.go'
O = P + 'outcome.go'
T = P + 'target.go'
ET = P + 'eligible_test.go'
CT = P + 'cheats_test.go'


def run(test, *subs):
    """-run pattern: the test, then each subtest level, anchored and escaped
    the way `go test` names subtests (spaces become underscores)."""
    parts = ['^%s$' % test]
    for s in subs:
        parts.append('^%s$' % re.escape(s.replace(' ', '_')))
    return '/'.join(parts)


ROWS = 'TestEligibleRows'
UNREAD = 'TestAnUnreadFactIsNeverEligibility'
REC = 'TestEveryRecordedFactIsRequired'
J0 = 'TestJ0Identity'
J1 = 'TestJ1Settle'
J2 = 'TestJ2Definition'
J3 = 'TestJ3PerDiagnosedJob'
J4 = 'TestJ4OtherJobs'
J5 = 'TestJ5FirstMatchOrder'
CONS = 'TestConservativeAcrossPipelines'
RETRY = 'TestAHumanRetryIsNotAPass'
READF = 'TestAReadFailureIsNotAVerdict'
GREEN = 'TestPipelineGreenIsNotTheCriterion'
LATE = 'TestPastTheDeadlineTheOrderStillHolds'
NOPASS = 'TestNoUnreadFactIsEverAPass'
V2S = 'TestVerdictToStateToMeterOutcome'
STATES = 'TestStateOutcomes'
TSET = 'TestTargetSet'
CHEATS = 'TestCheats'

# (name, [(file, old, new), ...], -run pattern of the guarding test)
muts = []


def m(name, edits, pattern):
    muts.append((name, edits, pattern))


def off(src, line):
    """Disable an `if <cond> {` line: the condition becomes false."""
    assert line.lstrip('\t').startswith('if ') and line.endswith(' {'), line
    indent = line[:len(line) - len(line.lstrip('\t'))]
    return (src, line + '\n', indent + 'if false {\n')


# =========================== Eligible: section 3.2 ===========================
m("E1: an unloaded config is not checked", [off(E, "\tif !f.Config.Loaded {")], run(UNREAD, "the config was not loaded"))
m("E1: an unloaded config is not checked (the zero value)", [off(E, "\tif !f.Config.Loaded {")], run(UNREAD, "the zero value"))
m("E1: actions.pipeline_fixes is not required", [off(E, "\tif !f.Config.PipelineFixes {")], run(ROWS, "E1 pipeline_fixes is off"))
m("E1: actions.pipelines is not required", [off(E, "\tif !f.Config.Pipelines {")], run(ROWS, "E1 pipelines is off"))
m("E2: an unread run is not checked", [off(E, "\tif !f.Run.Read {")], run(UNREAD, "the run was not read"))
m("E2: the target kind is not checked", [off(E, "\tif !contains(f.Config.Targets, f.Run.TargetKind) {")],
  run(ROWS, "E2 an mr target on a project that enabled ref only"))
m("E2: an empty pipeline_fix.targets enables every kind",
  [(E, "\tif !contains(f.Config.Targets, f.Run.TargetKind) {", "\tif len(f.Config.Targets) > 0 && !contains(f.Config.Targets, f.Run.TargetKind) {")],
  run(ROWS, "E2 no target kind enabled"))
m("E3: the run note state is not checked", [off(E, "\tif f.Run.NoteState != pipelinectx.RunStateDiagnosed {")],
  run(ROWS, "E3 the run note is still diagnosing"))
m("E3: any note state past diagnosing passes",
  [(E, "\tif f.Run.NoteState != pipelinectx.RunStateDiagnosed {", "\tif f.Run.NoteState == pipelinectx.RunStateDiagnosing {")],
  run(ROWS, "E3 the run note is resolved"))
m("E3: a closed run is still fixed", [off(E, "\tif !f.Run.Open {")], run(ROWS, "E3 the run is not open"))
m("E3: a run with no recorded diagnosis is not refused at E3", [off(E, '\tif f.Run.Class == "" {')],
  run(ROWS, "E3 the note says diagnosed and the record holds no diagnosis"))
for cls in ("flaky", "infrastructure", "unknown"):
    m("E4: the closed class set is not checked (`%s`)" % cls, [off(E, "\tif !contains(FixableClasses(), f.Run.Class) {")],
      run(ROWS, "E4 " + cls))
    m("E4: config can make `%s` eligible (the closed set is not checked)" % cls,
      [off(E, "\tif !contains(FixableClasses(), f.Run.Class) {")],
      run('TestNoConfigMakesAnUnfixableClassEligible', cls))
    m("E4: `%s` is in the closed set" % cls,
      [(E, "func FixableClasses() []string { return []string{ClassCode, ClassTest, ClassConfiguration} }",
           'func FixableClasses() []string { return []string{ClassCode, ClassTest, ClassConfiguration, "%s"} }' % cls)],
      run('TestNoConfigMakesAnUnfixableClassEligible', cls))
m("E4: a class nobody defined is fixable when the project lists it",
  [(E, "\tif !contains(FixableClasses(), f.Run.Class) {", "\tif !contains(FixableClasses(), f.Run.Class) && !contains(f.Config.Classes, f.Run.Class) {")],
  run('TestC17AFlakyDiagnosisIsNeverDispatched'))
m("E4: pipeline_fix.classes is not checked", [off(E, "\tif !contains(f.Config.Classes, f.Run.Class) {")],
  run(ROWS, "E4 configuration, which this project did not enable"))
m("E4: the refusal is silent, not noted",
  [(E, "\t\treturn noted(E4NeverEligible, classNote)", "\t\treturn silent(E4NeverEligible, classNote)")], run(ROWS, "E4 flaky"))
m("E4: the note does not name the class",
  [(E, 'classNote := fmt.Sprintf("no fix attempted: classified `%s`", f.Run.Class)', 'classNote := fmt.Sprintf("no fix attempted: classified `%s`", "other")')],
  run(ROWS, "E4 infrastructure"))
m("E5: an empty T is not refused", [off(E, "\tif len(f.Run.Targets) == 0 {")], run(ROWS, "E5 the diagnosis cites no job"))
m("E5: a T with problems is not refused", [off(E, "\tif len(f.Run.TargetProblems) > 0 {")], run(ROWS, "E5 a cited job cannot be judged"))
m("E6: an unfolded envelope is not checked", [off(E, "\tif !f.Scope.Folded {")], run(UNREAD, "the envelope was not folded"))
m("E6: an unclassified cited file is not `unread`", [off(E, '\t\tif c.Class == "" {')], run(UNREAD, "a cited file was not classified"))
m("E6: a diagnosis that cites no file is eligible", [off(E, "\tif len(f.Scope.Cited) == 0 {")], run(ROWS, "E6 the diagnosis cites no file"))
m("E6: the envelope is not checked", [off(E, "\t\tif !contains(f.Scope.Envelope, c.Class) {")],
  run(ROWS, "E6 a cited file is a test, and the class is code"))
m("E6: only the FIRST cited file is checked",
  [(E, "\tfor _, c := range f.Scope.Cited {\n\t\tif !contains(f.Scope.Envelope, c.Class) {",
       "\tfor _, c := range f.Scope.Cited[:1] {\n\t\tif !contains(f.Scope.Envelope, c.Class) {")],
  run(ROWS, "E6 a cited file is a test, and the class is code"))
m("E6: an empty envelope permits everything",
  [(E, "\t\tif !contains(f.Scope.Envelope, c.Class) {", "\t\tif len(f.Scope.Envelope) > 0 && !contains(f.Scope.Envelope, c.Class) {")],
  run(ROWS, "E6 the envelope is empty"))
m("E6: the refusal is not needs-scope",
  [(E, "Text: text, Note: true, NeedsHuman: true, NeedsScope: true}", "Text: text, Note: true, NeedsHuman: true}")],
  run(ROWS, "E6 the only cited file is the CI definition"))
m("E6: the refusal does not ask for a human",
  [(E, "Text: text, Note: true, NeedsHuman: true, NeedsScope: true}", "Text: text, Note: true, NeedsScope: true}")],
  run(ROWS, "E6 the only cited file is the CI definition"))
E7 = '\tif !f.Head.Read || f.Head.TargetSHA == "" || f.Head.NewestFailingSHA == "" {'
m("E7: an unread head is not checked", [(E, E7, '\tif f.Head.TargetSHA == "" || f.Head.NewestFailingSHA == "" {')], run(UNREAD, "the head was not read"))
m("E7: an empty target SHA is a SHA", [(E, E7, '\tif !f.Head.Read || f.Head.NewestFailingSHA == "" {')], run(UNREAD, "the head SHA is empty"))
m("E7: an empty newest-failing SHA is a SHA", [(E, E7, '\tif !f.Head.Read || f.Head.TargetSHA == "" {')], run(UNREAD, "the newest failing SHA is empty"))
m("E7: two empty SHAs are equal", [(E, E7, '\tif !f.Head.Read {')], run(UNREAD, "both SHAs are empty (equal, and not a fact)"))
m("E7: a moved head is still fixed", [off(E, "\tif f.Head.TargetSHA != f.Head.NewestFailingSHA {")],
  run(ROWS, "E7 the target moved past the newest failing pipeline"))
m("E7: the refusal is noted (it must be silent)",
  [(E, "\t\treturn silent(E7HeadMoved,", "\t\treturn noted(E7HeadMoved,")], run(ROWS, "E7 the target moved past the newest failing pipeline"))
m("E8: uncomputed current keys are not checked", [off(E, "\tif !f.Current.Read {")], run(UNREAD, "the current failure keys were not computed"))
m("E8: drift is not checked",
  [(E, "\t\tif !containsKey(f.Current.FailedKeys, t.Key) {", "\t\tif false && !containsKey(f.Current.FailedKeys, t.Key) {")],
  run(ROWS, "E8 the cited job now fails with another error"))
m("E8: drift compares the job name only",
  [(E, "\t\tif x == k {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n",
       "\t\tif x.Name == k.Name {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n")],
  run(ROWS, "E8 the cited job fails for another failure_reason"))
m("E8: only the FIRST target is checked for drift",
  [(E, "\tfor _, t := range f.Run.Targets {\n\t\tif !containsKey(", "\tfor _, t := range f.Run.Targets[:1] {\n\t\tif !containsKey(")],
  run('TestEveryTargetMustStillFailTheSameWay'))
m("E8: the refusal is silent, not noted",
  [(E, "\t\t\treturn noted(E8Drifted,", "\t\t\treturn silent(E8Drifted,")], run(ROWS, "E8 the cited job no longer fails; another one does"))
E9 = '\tif !f.Dedup.Read || (f.Dedup.FixState != "" && !KnownState(f.Dedup.FixState)) {'
m("E9: unread dedup facts are not checked", [(E, E9, '\tif f.Dedup.FixState != "" && !KnownState(f.Dedup.FixState) {')],
  run(UNREAD, "the fix record and MRs were not read"))
m("E9: a fix state nobody defined is a state", [(E, E9, '\tif !f.Dedup.Read {')], run(UNREAD, "the fix record holds a state nobody defined"))
m("E9: a fix that ended is proposed again", [off(E, '\tif f.Dedup.FixState != "" && f.Dedup.FixState.Terminal() {')],
  run(ROWS, "E9 the fix already ended (flaky)"))
m("E9: a fix that ended is proposed again (and no later row stops it)",
  [off(E, '\tif f.Dedup.FixState != "" && f.Dedup.FixState.Terminal() {'),
   (E, '\tif f.Dedup.FixState != "" {\n\t\treturn silent(E9FixInFlight', '\tif false {\n\t\treturn silent(E9FixInFlight')],
  run(ROWS, "E9 the fix already ended (closed-unmerged)"))
m("E9: a fix in flight is proposed again",
  [(E, '\tif f.Dedup.FixState != "" {\n\t\treturn silent(E9FixInFlight', '\tif false {\n\t\treturn silent(E9FixInFlight')],
  run(ROWS, "E9 a fix is in flight (proposing)"))
m("E9: a fix a human closed is proposed again", [off(E, "\tif f.Dedup.ClosedUnmergedCauseMRs != 0 {")],
  run(ROWS, "E9 a human closed a fix for this cause"))
m("E9: the closed-unmerged refusal is silent, not noted",
  [(E, "\t\treturn noted(E9ClosedUnmerged,", "\t\treturn silent(E9ClosedUnmerged,")], run(ROWS, "E9 a human closed a fix for this cause"))
m("E9: an open fix MR for this cause does not stop a second", [off(E, "\tif f.Dedup.OpenCauseMRs != 0 {")],
  run(ROWS, "E9 a fix MR for this cause is open"))
m("E10: unread serial facts are not checked", [off(E, "\tif !f.Serial.Read {")], run(UNREAD, "the target's fix MRs were not read"))
m("E10: a second open fix MR on the target is allowed", [off(E, "\tif f.Serial.OpenFixMRs != 0 {")],
  run(ROWS, "E10 another fix MR is open on this target"))
E11 = "\tif f.Serial.MergedSinceGreen >= f.Config.MaxChain {"
m("E11: the chain cap is not checked", [off(E, E11)], run(ROWS, "E11 the chain is past max_chain"))
m("E11: max_chain is inclusive (one fix too many)", [(E, E11, "\tif f.Serial.MergedSinceGreen > f.Config.MaxChain {")],
  run(ROWS, "E11 the chain is at max_chain"))
m("E11: max_chain is inclusive (the bound test)", [(E, E11, "\tif f.Serial.MergedSinceGreen > f.Config.MaxChain {")], run('TestMaxChainIsAStrictBound'))
m("E11: a max_chain of zero means no cap", [(E, E11, "\tif f.Config.MaxChain > 0 && f.Serial.MergedSinceGreen >= f.Config.MaxChain {")],
  run(ROWS, "E11 max_chain is zero"))
m("E11: the refusal does not ask for a human",
  [(E, "\t\t\tCode: E11ChainExhausted, Note: true, NeedsHuman: true,", "\t\t\tCode: E11ChainExhausted, Note: true,")],
  run(ROWS, "E11 the chain is at max_chain"))
m("E12: the correlation hold is not checked", [off(E, "\tif f.CorrelationHold {")], run(ROWS, "E12 the run is held by a cross-project roll-up"))
m("order: E12 is evaluated first",
  [(E, "\t// E1: opted in.\n", '\t// E1: opted in.\n\tif f.CorrelationHold {\n\t\treturn noted(E12CorrelationHold, "held")\n\t}\n')],
  run('TestTheFirstFailingRowIsTheReason'))
m("order: E7 (the head) is evaluated before E1",
  [(E, "\t// E1: opted in.\n", '\t// E1: opted in.\n\tif !f.Head.Read {\n\t\treturn unread(E7Unread, "head")\n\t}\n')],
  run('TestAnUnreadFactDoesNotHideAnEarlierDecision'))
m("order: E10 is evaluated before E7",
  [(E, "\t// E7: the fix is written against a tree whose pipeline is known red.\n",
       '\t// E7: the fix is written against a tree whose pipeline is known red.\n\tif f.Serial.Read && f.Serial.OpenFixMRs != 0 {\n\t\treturn silent(E10OpenFixMR, "open")\n\t}\n')],
  run('TestAnUnreadFactDoesNotHideAnEarlierDecision'))
m("an unread fact is reported as a decision (Unknown is not set)",
  [(E, "\t\treturn false, Reason{Code: code, Text: text, Unknown: true}", "\t\treturn false, Reason{Code: code, Text: text}")],
  run(UNREAD, "the head was not read"))
m("Reason.Row is the whole code", [(E, '\trow, _, _ := strings.Cut(string(r.Code), ".")\n\treturn row', "\treturn string(r.Code)")],
  run(ROWS, "E10 another fix MR is open on this target"))
m("everything is eligible", [(E, "\t// E1: opted in.\n", "\t// E1: opted in.\n\tif f.Config.Loaded {\n\t\treturn true, Reason{}\n\t}\n")],
  run('TestTheFirstFailingRowIsTheReason'))
m("test table: an E row loses its fixtures (the plan-row test must notice)",
  [(ET, 'row: "E12", code: E12CorrelationHold', 'row: "E11", code: E12CorrelationHold')], run('TestEveryPlanERowHasAFixture'))

# ================================ T: section 3.2 ================================
m("T: a job that was allowed to fail is in T", [(T, "\t\tcase found[0].AllowFailure:\n", "\t\tcase false:\n")],
  run(TSET, "a job that was allowed to fail is not in T"))
m("T: a cited job that is not recorded is skipped silently",
  [(T, "\t\tcase len(found) == 0:\n\t\t\tproblems = append(problems, ", "\t\tcase len(found) == 0:\n\t\t\t_ = append(problems, ")],
  run(TSET, "a cited job that is not recorded is a problem"))
m("T: of a job recorded twice the first is taken", [(T, "\t\tcase len(found) > 1:\n", "\t\tcase false:\n")], run(TSET, "a job recorded twice is a problem"))
m("T: a job without recorded failure facts is in T", [(T, "\t\tcase !found[0].FactsRecorded:\n", "\t\tcase false:\n")],
  run(TSET, "a job recorded before the failure facts existed is a problem"))
m("T: an empty signature is not generic",
  [(T, '\t\t\t\tGeneric: j.Generic || j.ErrorSignature == "",', "\t\t\t\tGeneric: j.Generic,")],
  run(TSET, "a job with no signature is generic whatever the flag says"))
m("T: the generic flag is dropped",
  [(T, '\t\t\t\tGeneric: j.Generic || j.ErrorSignature == "",', '\t\t\t\tGeneric: j.ErrorSignature == "",')],
  run(TSET, "the cited jobs, in first-cited order, uncited ones left out"))
m("T: a job cited twice is in T twice", [(T, "\t\tif seen[name] {\n\t\t\tcontinue\n\t\t}\n", "")],
  run(TSET, "the cited jobs, in first-cited order, uncited ones left out"))
m("T: every recorded failed job is in T, cited or not",
  [(T, "\tfor _, name := range evidenceJobs {\n", "\tevidenceJobs = nil\n\tfor _, j := range jobs {\n\t\tevidenceJobs = append(evidenceJobs, j.Name)\n\t}\n\tfor _, name := range evidenceJobs {\n")],
  run(TSET, "the cited jobs, in first-cited order, uncited ones left out"))
m("T: the cause key ignores all but the first target",
  [(T, "\tfor _, t := range targets {\n\t\tkeys = append(keys, t.Key)", "\tfor _, t := range targets[:min(1, len(targets))] {\n\t\tkeys = append(keys, t.Key)")],
  run('TestCauseKeyIsPipelinectxsOverTheTargetKeys'))

# ======================= Judge: the recorded facts (6.1) =======================
def miss(what, sub, name=None, test=REC, when="before the deadline"):
    old = "\tmiss(" + what + ","
    new = "\tmiss(false && " + what + ","
    pattern = run(test, sub, when) if test == REC else run(test, sub)
    m(name or ("6.1: `%s` is not required" % what), [(J, old, new)], pattern)


m("6.1: a clock is not required", [(J, "\tmiss(now.IsZero(),", "\tmiss(false,")], run(REC, "no clock"))
m("6.1: an unrecorded fix is judged", [off(J, "\tif !f.Fix.Recorded {")], run(REC, "nothing recorded", "before the deadline"))
m("6.1: an unrecorded fix is judged once the deadline passed", [off(J, "\tif !f.Fix.Recorded {")], run(REC, "nothing recorded", "past the deadline"))
miss("len(fix.Targets) == 0", "T is empty")
m("6.1: a target without a name is accepted", [(J, '\t\tmiss(t.Name() == "",', "\t\tmiss(false,")], run(REC, "a target has no name", "before the deadline"))
m("6.1: a target listed twice is accepted", [(J, "\t\tmiss(names[t.Name()],", "\t\tmiss(false,")], run(REC, "a target is listed twice", "before the deadline"))
miss("fix.BasePipelineID == 0", "no base pipeline")
m("6.1: a base job without a status is accepted", [(J, '\t\tmiss(b.Status == "",', "\t\tmiss(false,")], run(REC, "a base job has no status", "before the deadline"))
m("6.1: a base job listed twice is accepted (the last wins)",
  [(J, "\t\tif _, dup := base[b.Name]; dup {", "\t\tif _, dup := base[b.Name]; dup && false {")], run(REC, "a base job is listed twice", "before the deadline"))
m("6.1: a target missing from the base jobs is accepted", [(J, "\t\tmiss(!has,", "\t\tmiss(false,")], run(REC, "a target is not among the base jobs", "before the deadline"))
m("6.1: empty base jobs are accepted", [(J, "\t\tmiss(!has,", "\t\tmiss(false,")], run(REC, "no base jobs", "before the deadline"))
m("6.1: a target that did not fail on the base is accepted",
  [(J, "\t\tmiss(has && b.Status != StatusFailed,", "\t\tmiss(false,")], run(REC, "a target did not fail on the base", "before the deadline"))
m("6.1: a target that was allowed to fail on the base is accepted",
  [(J, "\t\tmiss(has && b.AllowFailure,", "\t\tmiss(false,")], run(REC, "a target was allowed to fail on the base", "before the deadline"))
miss('fix.FixBaseSHA == ""', "no base SHA")
miss('fix.FixCommitSHA == ""', "no fix commit")
m("6.1: two empty SHAs are an identity", [(J, '\tmiss(fix.FixCommitSHA == "",', "\tmiss(false,"), (J, '\tmiss(f.MR.SHA == "",', "\tmiss(false,"),
                                          (J, '\t\t\tif p.ID == 0 || p.SHA == "" || p.Source == "" {', '\t\t\tif p.ID == 0 || p.Source == "" {')],
  run('TestTwoEmptySHAsAreNotAnIdentity'))
miss("fix.MRIID == 0", "no MR iid")
miss("fix.ExcerptVersion == 0", "no excerpt version")
miss("fix.SignatureVersion == 0", "no signature version")
m("6.1: ci_exec_changes is not required",
  [(J, "\tmiss(!contains([]string{CIExecDeny, CIExecManualRun, CIExecAuto}, fix.CIExecChanges),", "\tmiss(false,")],
  run(REC, "no ci_exec_changes", "before the deadline"))
m("6.1: any non-empty ci_exec_changes is accepted",
  [(J, "\tmiss(!contains([]string{CIExecDeny, CIExecManualRun, CIExecAuto}, fix.CIExecChanges),", '\tmiss(fix.CIExecChanges == "",')],
  run(REC, "a ci_exec_changes nobody defined", "before the deadline"))
miss("fix.VerifyDeadline.IsZero()", "no verify deadline")
m("6.1: a missing recorded fact becomes a verdict once the deadline passed",
  [(J, "\tif missing := missingFacts(f, now); len(missing) > 0 {", "\tif missing := missingFacts(f, now); len(missing) > 0 && now.Before(f.Fix.VerifyDeadline) {")],
  run(REC, "no base SHA", "past the deadline"))

# ================================ J0 identity ================================
m("J0: an unread MR is judged", [off(J, "\tif !f.MR.Read {")], run(J0, "the MR was not read"))
m("J0: an unread MR is judged once the deadline passed", [off(J, "\tif !f.MR.Read {")], run(J0, "the MR was not read, past the deadline: still no verdict"))
m("J0: facts of another MR are judged", [(J, "\tmiss(f.MR.IID != fix.MRIID,", "\tmiss(false,")], run(J0, "the facts are of another MR"))
m("J0: an MR without a head SHA is judged", [(J, '\tmiss(f.MR.SHA == "",', "\tmiss(false,")], run(J0, "the MR has no head SHA"))
m("J0: a merged MR is not `merged`", [(J, "\tcase f.MR.State == MRStateMerged:\n", "\tcase false:\n")], run(J0, "the MR was merged"))
m("J0: a closed MR is not `closed-unmerged`", [(J, "\tcase f.MR.State == MRStateClosed:\n", "\tcase false:\n")], run(J0, "the MR was closed"))
m("J0: a closed MR keeps being judged",
  [(J, "\tcase f.MR.State == MRStateClosed:\n", "\tcase false:\n"), (J, "\tcase f.MR.State != MRStateOpened:\n", "\tcase false:\n")],
  run(J0, "the MR was closed"))
m("J0: an MR in any other state is judged as open", [(J, "\tcase f.MR.State != MRStateOpened:\n", "\tcase false:\n")], run(J0, "the MR is locked (mid-merge)"))
m("J0: a foreign head commit is judged as gonk's", [(J, "\tcase f.MR.SHA != f.Fix.FixCommitSHA:\n", "\tcase false:\n")], run(J0, "somebody else pushed"))
m("J0: taken-over is decided before merged",
  [(J, "\tswitch {\n\tcase f.MR.State == MRStateMerged:\n", "\tswitch {\n\tcase f.MR.SHA != f.Fix.FixCommitSHA:\n\t\treturn Result{Decision: DecisionStopped, State: StateTakenOver, Rule: RuleJ0TakenOver, Detail: \"x\"}\n\tcase f.MR.State == MRStateMerged:\n")],
  run(J0, "merged, and its head is not gonk's commit: still merged"))
m("J0: taken-over is decided before closed",
  [(J, "\tswitch {\n\tcase f.MR.State == MRStateMerged:\n", "\tswitch {\n\tcase f.MR.State != MRStateMerged && f.MR.SHA != f.Fix.FixCommitSHA:\n\t\treturn Result{Decision: DecisionStopped, State: StateTakenOver, Rule: RuleJ0TakenOver, Detail: \"x\"}\n\tcase f.MR.State == MRStateMerged:\n")],
  run(J0, "closed, and its head is not gonk's commit: still closed"))
m("J0: taken-over is a verdict past the deadline",
  [(J, "\tcase f.MR.SHA != f.Fix.FixCommitSHA:\n", "\tcase f.MR.SHA != f.Fix.FixCommitSHA && now.Before(f.Fix.VerifyDeadline):\n")],
  run(J0, "somebody else pushed, past the deadline"))

# ================================= J1 settle =================================
PAST = "\tpast := !now.Before(f.Fix.VerifyDeadline)\n"
m("J1: the deadline instant is still before the deadline", [(J, PAST, "\tpast := now.After(f.Fix.VerifyDeadline)\n")], run('TestTheDeadlineItselfIsPastTheDeadline'))
m("J1: the deadline never passes (it waits forever)", [(J, PAST, "\tpast := false\n")], run(J1, "no pipeline, past the deadline"))
m("J1: the deadline has always passed (it never waits)", [(J, PAST, "\tpast := true\n")], run(J1, "no pipeline yet"))
m("J1: an unread pipeline list is an empty one", [off(J, "\tif !f.Pipelines.Read {")], run(J1, "the pipeline list was not read"))
m("J1: an unread pipeline list is not UNVERIFIABLE(J1) past the deadline",
  [(J, '\t\tunverifiable = append(unverifiable, cause{RuleJ1Unread, "the merge request\'s pipelines could not be read before the verify deadline"})\n', "")],
  run(J1, "the pipeline list was not read, past the deadline"))
m("J1: with the pipeline list unread, a diagnosed job reads as absent rather than unread",
  [(J, '\t\tunverifiable = append(unverifiable, cause{RuleJ1Unread, "the merge request\'s pipelines could not be read before the verify deadline"})\n\t\tcomplete = false\n',
       '\t\tunverifiable = append(unverifiable, cause{RuleJ1Unread, "the merge request\'s pipelines could not be read before the verify deadline"})\n')],
  run(J1, "the pipeline list was not read, past the deadline"))
PMAL = '\t\t\tif p.ID == 0 || p.SHA == "" || p.Source == "" {'
m("J1: a pipeline without an id is accepted", [(J, PMAL, '\t\t\tif p.SHA == "" || p.Source == "" {')], run(J1, "a pipeline has no id"))
m("J1: a pipeline without a sha is skipped silently", [(J, PMAL, '\t\t\tif p.ID == 0 || p.Source == "" {')], run(J1, "a pipeline has no sha"))
m("J1: a pipeline without a source is a candidate", [(J, PMAL, '\t\t\tif p.ID == 0 || p.SHA == "" {')], run(J1, "a pipeline has no source"))
m("J1: a pipeline of another commit is a candidate", [off(J, "\t\t\tif p.SHA != f.Fix.FixCommitSHA {")], run(J1, "the only pipeline is of another commit"))
m("J1: a pipeline of another commit holds the verdict", [off(J, "\t\t\tif p.SHA != f.Fix.FixCommitSHA {")],
  run(J1, "a pipeline of another commit still running does not hold the verdict"))
m("J1: a child pipeline is a candidate", [off(J, "\t\t\tif p.Source == SourceParentPipeline {")], run(J1, "the only pipeline is a child pipeline"))
m("J1: a child pipeline holds the verdict", [off(J, "\t\t\tif p.Source == SourceParentPipeline {")],
  run(J1, "a child pipeline still running does not hold the verdict"))
# ---- gonk-vi9k: a `[skip ci]` pipeline (skipped, no jobs) is not a candidate ----
SK = 'TestJ1ASkippedPipelineWithoutJobsIsNotACandidate'
SKREC = 'TestTheRecordedSkipCIFactsWaitThenTimeOut'
SKIF = "\t\t\tif skippedWithoutJobs(p) {"
SKFN = "\treturn p.Status == StatusSkipped && p.JobsRead && p.IncludeRetried && len(p.Jobs) == 0\n"
m("J1 (gonk-vi9k): a skipped pipeline with no jobs is a candidate (the defect: UNVERIFIABLE at once)", [off(J, SKIF)],
  run(SK, "skipped, no jobs: wait"))
m("J1 (gonk-vi9k): the same, on the facts recorded from the real GitLab", [off(J, SKIF)], run(SKREC))
m("J1 (gonk-vi9k): past the deadline a skipped-only fix is J3's absent job, not J1.TIMEOUT", [off(J, SKIF)],
  run(SK, "skipped, no jobs, past the deadline: J1.TIMEOUT"))
m("J1 (gonk-vi9k): the skipped pipeline still has to be `success` for a later pipeline to be FIXED", [off(J, SKIF)],
  run(SK, "a pipeline started later on the same commit is a normal candidate: it passes"))
m("J1 (gonk-vi9k): ANY pipeline with no jobs is not a candidate",
  [(J, SKFN, "\treturn p.JobsRead && p.IncludeRetried && len(p.Jobs) == 0\n")], run(SK, "status canceled with no jobs is a candidate"))
m("J1 (gonk-vi9k): a skipped pipeline that HAS jobs is not a candidate",
  [(J, SKFN, "\treturn p.Status == StatusSkipped && p.JobsRead && p.IncludeRetried\n")],
  run(SK, "skipped WITH jobs is a candidate (the diagnosed job itself skipped)"))
m("J1 (gonk-vi9k): a skipped pipeline with ONE job is not a candidate",
  [(J, SKFN, "\treturn p.Status == StatusSkipped && p.JobsRead && p.IncludeRetried && len(p.Jobs) <= 1\n")],
  run(SK, "skipped with ONE job that is not the diagnosed one is a candidate"))
m("J1 (gonk-vi9k): a skipped pipeline whose jobs were NOT listed is taken to have none",
  [(J, SKFN, "\treturn p.Status == StatusSkipped && p.IncludeRetried && len(p.Jobs) == 0\n")], run(SK, "skipped, jobs NOT listed: nobody knows it ran nothing"))
m("J1 (gonk-vi9k): a skipped pipeline listed without retried attempts is taken to have none",
  [(J, SKFN, "\treturn p.Status == StatusSkipped && p.JobsRead && len(p.Jobs) == 0\n")],
  run(SK, "skipped, jobs listed without include_retried: nobody knows it ran nothing"))
# ---- gonk-5b2f: a terminal pipeline with an ACTIVE job attempt is not settled ----
AC = 'TestJ1AnActiveAttemptHoldsASettledPipeline'
ACREC = 'TestTheRecordedRetryPendingFactsAreNotSettled'
ACOFF = ("\t\t\tif a, found := activeAttempt(c); found {\n\t\t\t\tsettled = false\n", "\t\t\tif a, found := activeAttempt(c); found {\n")
ACLIST = '\tvar activeJobStatuses = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"}\n'.lstrip('\t')
m("J1 (gonk-5b2f): a failed pipeline with a pending retry is settled (the defect), on the facts recorded from the real GitLab",
  [(J,) + ACOFF], run(ACREC, "as recorded: the fix added the retry key"))
m("J1 (gonk-5b2f): the same when the retry is the base definition's own: NOT_FIXED before the retry ran",
  [(J,) + ACOFF], run(ACREC, "the retry is the base definition's own"))
m("J1 (gonk-5b2f): a pending human retry does not hold the verdict", [(J,) + ACOFF], run('TestAPendingHumanRetryIsWaitedFor'))
m("J1 (gonk-5b2f): an active attempt holds the verdict but past the deadline the pipeline counts as settled (a pass)",
  [(J, "\t\t\tif a, found := activeAttempt(c); found {\n\t\t\t\tsettled = false\n", "\t\t\tif a, found := activeAttempt(c); found {\n\t\t\t\tsettled = settled && past\n")],
  run(AC, "pipeline success, a job created"))
m("J1 (gonk-5b2f): the wait does not say which job it is waiting for",
  [(J, '\t\t\tif pendingDetail != "" {\n\t\t\t\tres.Detail = pendingDetail\n\t\t\t}\n', "")], run(AC, "pipeline failed, a job pending"))
for _st in ["created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"]:
    m("J1 (gonk-5b2f): a job attempt that is `%s` is not active" % _st,
      [(J, ACLIST, ACLIST.replace('"%s", ' % _st, '').replace(', "%s"' % _st, ''))], run(AC, "pipeline failed, a job %s" % _st))
m("J1 (gonk-5b2f): a `manual` job nobody started holds the verdict for ever",
  [(J, ACLIST, ACLIST.replace('"created"', '"manual", "created"'))], run(AC, "a job manual does not hold it"))
m("J1 (gonk-5b2f): ANY job that did not succeed holds the verdict",
  [(J, "\t\tif !activeJobStatus(a.Status) {\n", "\t\tif a.Status == StatusSuccess || a.Status == StatusFailed {\n")], run(AC, "a job skipped does not hold it"))
m("J1 (gonk-5b2f): the listed statuses are not the bead's decision",
  [(J, ACLIST, ACLIST.replace('"scheduled"', '"scheduled", "canceling"'))], run('TestActiveJobStatusesAreTheDecision'))
m("J1 (gonk-vi9k): a skipped-only fix is settled (no candidate is enough once something was skipped)",
  [(J, "\t\tsettled := len(candidates) > 0\n", "\t\tsettled := len(candidates) > 0 || len(res.Skipped) > 0\n")],
  run(SK, "one per source, both skipped with no jobs (the observed shape): wait"))
m("J1 (gonk-vi9k): a skipped-only fix is a PASS past the deadline (the timeout is not recorded)",
  [(J, "\t\t\tunverifiable = append(unverifiable, cause{RuleJ1Timeout, detail})\n", "\t\t\t_ = detail\n")],
  run(SK, "skipped, no jobs, past the deadline: J1.TIMEOUT"))
m("J1 (gonk-vi9k): the pipelines that were set aside are not named",
  [(J, "\t\t\t\tres.Skipped = append(res.Skipped, p.ID)\n", "")], run(SK, "what is not a candidate is named, and is not in C"))
m("J1 (gonk-vi9k): the wait does not say the pipelines were skipped",
  [off(J, "\t\t\tif len(res.Skipped) > 0 {")], run(SK, "what is not a candidate is named, and is not in C"))
m("J1 (gonk-vi9k): the timeout does not say the pipelines were skipped",
  [off(J, "\t\t\tif len(candidates) == 0 && len(res.Skipped) > 0 {")], run(SK, "what is not a candidate is named, and is not in C"))
m("J1 (gonk-vi9k): a started pipeline still running does not hold the verdict once one was skipped",
  [(J, "\t\t\tif !settledStatus(c.Status) {", "\t\t\tif len(res.Skipped) == 0 && !settledStatus(c.Status) {")],
  run(SK, "a pipeline started later is still running: wait"))
m("J1: no candidate pipeline counts as settled", [(J, "\t\tsettled := len(candidates) > 0\n", "\t\tsettled := true\n")], run(J1, "no pipeline yet"))
m("J1: a running pipeline counts as settled", [off(J, "\t\t\tif !settledStatus(c.Status) {")], run(J1, "status running is not settled"))
m("J1: only the FIRST candidate has to settle",
  [(J, "\t\t\tif !settledStatus(c.Status) {", "\t\t\tif c.ID == candidates[0].ID && !settledStatus(c.Status) {")], run(J1, "a second candidate is still running"))
for st in ("StatusSuccess", "StatusFailed", "StatusCanceled", "StatusSkipped"):
    lst = ["StatusSuccess", "StatusFailed", "StatusCanceled", "StatusSkipped"]
    lst.remove(st)
    name = {"StatusSuccess": "success", "StatusFailed": "failed", "StatusCanceled": "canceled", "StatusSkipped": "skipped"}[st]
    m("J1: `%s` is not a settled status" % name,
      [(J, "\tcase StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped:\n\t\treturn true", "\tcase %s:\n\t\treturn true" % ", ".join(lst))],
      run(J1, "status %s is settled" % name))
for st in ("manual", "scheduled", "created", "pending", "waiting_for_resource", "preparing", "canceling"):
    m("J1: `%s` is a settled status" % st,
      [(J, "\tcase StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped:\n\t\treturn true",
           '\tcase StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped, "%s":\n\t\treturn true' % st)],
      run(J1, "status %s is not settled" % st))
m("J1: an empty status is settled",
  [(J, "\tcase StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped:\n\t\treturn true",
       '\tcase StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped, "":\n\t\treturn true')], run(J1, "status  is not settled"))
m("J1: it still waits past the deadline", [(J, "\t\tif !settled && !past {", "\t\tif !settled {")], run(J1, "a second candidate is still running, past the deadline"))
m("J1: an unsettled pipeline past the deadline is not UNVERIFIABLE(timeout)",
  [(J, "\t\tif !settled {\n\t\t\tdetail := ", "\t\tif false {\n\t\t\tdetail := ")], run(J1, "status running past the deadline"))
m("J1: no pipeline at all past the deadline is not UNVERIFIABLE(timeout)",
  [(J, "\t\tif !settled {\n\t\t\tdetail := ", "\t\tif false {\n\t\t\tdetail := ")], run(J1, "no pipeline, past the deadline"))
m("J1: candidates are taken in listing order, not by id",
  [(J, "\t\tsort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })\n", "")],
  run(CONS, "recurs in pipeline 501", "listed newest first"))
m("J1: the wait does not report state `verifying`",
  [(J, "\t\t\tres.Decision, res.State, res.Rule = DecisionWait, StateVerifying, RuleJ1Wait", '\t\t\tres.Decision, res.State, res.Rule = DecisionWait, "", RuleJ1Wait')],
  run(J1, "no pipeline yet"))

# ========================= J2 definition integrity =========================
DEFUN = "\tif !f.Definition.Read || !knownKind(f.Definition.Delta.Kind) {"
m("J2: an unread delta is judged", [(J, DEFUN, "\tif !knownKind(f.Definition.Delta.Kind) {")], run(J2, "the delta was not read"))
m("J2: a delta nobody computed is judged", [(J, DEFUN, "\tif !f.Definition.Read {")], run(J2, "a delta nobody computed (the zero Kind)"))
m("J2: a Kind nobody defined is judged", [(J, DEFUN, "\tif !f.Definition.Read {")], run(J2, "a Kind nobody defined"))
m("J2: an unread delta past the deadline is no finding at all (a pass)",
  [(J, '\t\tunverifiable = append(unverifiable, cause{RuleJ2Unread, "the CI definition could not be compared before the verify deadline"})\n', "")],
  run(J2, "the delta was not read, past the deadline"))
m("J2: an unread delta past the deadline is a pass (the property test)",
  [(J, '\t\tunverifiable = append(unverifiable, cause{RuleJ2Unread, "the CI definition could not be compared before the verify deadline"})\n', "")],
  run(NOPASS))
m("J2: an unread delta does not hold the verdict before the deadline",
  [(J, '\t\tunread = append(unread, "definition: the base-vs-head CI definition delta was not computed")\n', "")], run(J2, "the delta was not read"))
KK = "\tcase cidef.KindNone, cidef.KindPreserving, cidef.KindExecChanging, cidef.KindViolation, cidef.KindUnresolvable:"
for kind, pattern in (("KindNone", run('TestTheBaselineFixIsFixed')), ("KindPreserving", run(J2, "preserving, with the ci lift")),
                      ("KindExecChanging", run(J2, "exec-changing, lifted, auto")), ("KindViolation", run(J2, "a violation")),
                      ("KindUnresolvable", run(J2, "unresolvable on both sides"))):
    kinds = ["cidef.KindNone", "cidef.KindPreserving", "cidef.KindExecChanging", "cidef.KindViolation", "cidef.KindUnresolvable"]
    kinds.remove("cidef." + kind)
    m("J2: `%s` is not a known Kind" % kind, [(J, KK, "\tcase %s:" % ", ".join(kinds))], pattern)
m("J2: Kind `violation` with an empty list is not a violation", [off(J, "\tif d.Kind == cidef.KindViolation {")],
  run(J2, "Kind says violation and the list is empty"))
m("J2: a listed violation is ignored when Kind says otherwise", [off(J, "\tif len(d.Violations) > 0 {")],
  run(J2, "a violation is listed and Kind says preserving"))
m("J2: a violation is not FORGED",
  [off(J, "\tif d.Kind == cidef.KindViolation {"), off(J, "\tif len(d.Violations) > 0 {")], run(J2, "a violation"))
m("J2: an unresolvable value outranks a violation in the same delta",
  [(J, "\tif d.Kind == cidef.KindViolation {", "\tif d.Kind == cidef.KindViolation && len(d.Unresolvable) == 0 {"),
   (J, "\tif len(d.Violations) > 0 {", "\tif len(d.Violations) > 0 && len(d.Unresolvable) == 0 {")],
  run(J2, "a violation and an unresolvable value: the violation decides"))
m("J2 [DECISION]: a head-only unresolvable construct is merely UNVERIFIABLE", [off(J, "\t\tif u.Side == cidef.SideHead {")],
  run(J2, "unresolvable at head only: the change introduced it"))
m("J2 [DECISION]: only the FIRST unresolvable entry's side is read",
  [(J, "\tfor _, u := range d.Unresolvable {\n\t\tif u.Side == cidef.SideHead {", "\tfor _, u := range d.Unresolvable[:min(1, len(d.Unresolvable))] {\n\t\tif u.Side == cidef.SideHead {")],
  run(J2, "one head-only unresolvable among base-side ones still decides"))
m("J2: a base-side unresolvable is FORGED too", [(J, "\t\tif u.Side == cidef.SideHead {", "\t\tif u.Side == cidef.SideHead || u.Side == cidef.SideBase {")],
  run(J2, "unresolvable on the base only"))
m("J2: a both-sides unresolvable is FORGED too", [(J, "\t\tif u.Side == cidef.SideHead {", "\t\tif u.Side != cidef.SideBase {")],
  run(J2, "unresolvable on both sides"))
m("J2: Kind `unresolvable` with an empty list is resolvable", [off(J, "\tif d.Kind == cidef.KindUnresolvable {")],
  run(J2, "Kind says unresolvable and the list is empty"))
m("J2: a listed unresolvable is ignored when Kind says otherwise", [off(J, "\tif len(d.Unresolvable) > 0 {")],
  run(J2, "an unresolvable value is listed and Kind says none"))
m("J2: unresolvable is a pass (`!reference` reads as equal)",
  [off(J, "\tif d.Kind == cidef.KindUnresolvable {"), off(J, "\tif len(d.Unresolvable) > 0 {")], run(J2, "unresolvable on both sides"))
LIFT = "\tif d.Kind != cidef.KindNone && !contains(fix.Lift, LiftCI) {"
m("J2: a definition change needs no lift", [off(J, LIFT)], run(J2, "preserving, WITHOUT the ci lift"))
m("J2: an exec-changing definition needs no lift", [off(J, LIFT)], run(J2, "exec-changing, WITHOUT the ci lift"))
m("J2: any lift counts as the ci lift", [(J, LIFT, "\tif d.Kind != cidef.KindNone && len(fix.Lift) == 0 {")],
  run(J2, "a lift of some other class is not the ci lift"))
m("J2: an untouched definition needs the lift", [(J, LIFT, "\tif !contains(fix.Lift, LiftCI) {")], run('TestTheBaselineFixIsFixed'))
DENY = "\tif d.Kind == cidef.KindExecChanging && fix.CIExecChanges == CIExecDeny {"
m("J2: ci_exec_changes: deny is not enforced", [off(J, DENY)], run(J2, "exec-changing, lifted, ci_exec_changes: deny"))
m("J2: deny refuses a preserving change too", [(J, DENY, "\tif fix.CIExecChanges == CIExecDeny {")], run(J2, "preserving under ci_exec_changes: deny is fine"))
m("J2: manual-run refuses an exec-changing fix", [(J, DENY, "\tif d.Kind == cidef.KindExecChanging && fix.CIExecChanges != CIExecAuto {")],
  run(J2, "exec-changing, lifted, manual-run: flagged, and the verdict stands"))
m("J2: the delta is not recorded on the result",
  [(J, "\t\t\tres.Definition = f.Definition.Delta.Kind\n", "\t\t\tres.Definition = \"\"\n")], run('TestTheBaselineFixIsFixed'))
m("J2: an exec-changing fix is recorded as `none` (not flagged)",
  [(J, "\t\t\tres.Definition = f.Definition.Delta.Kind\n", "\t\t\tres.Definition = cidef.KindNone\n")],
  run(CHEATS, "C11 static: an or-true appended to the script: flagged exec-changing"))
m("J2: unresolvable is FORGED",
  [(J, "\t\tcase RuleJ2Unresolvable:\n\t\t\tunverifiable = append(unverifiable, c)", "\t\tcase RuleJ2Unresolvable:\n\t\t\tforged = append(forged, c)")],
  run(J2, "unresolvable on both sides"))
m("J2: a violation is merely UNVERIFIABLE",
  [(J, "\t\tdefault:\n\t\t\tforged = append(forged, c)", "\t\tdefault:\n\t\t\tunverifiable = append(unverifiable, c)")], run(J2, "a violation"))

# ============================ the attempts (J3's A) ============================
m("A: an unlisted job list is an empty one", [off(J, "\t\tif !c.JobsRead {")], run(J3, "jobs: a candidate's jobs were not listed"))
m("A: a listing without retried attempts is accepted", [off(J, "\t\tif !c.IncludeRetried {")], run(J3, "jobs: listed without the retried attempts"))
m("A: an unlisted job list past the deadline: a visible pass is PASSED",
  [(J, '\t\t\tunread = append(unread, fmt.Sprintf("jobs: the jobs of pipeline %d were not listed", c.ID))\n\t\t\tcomplete = false\n',
       '\t\t\tunread = append(unread, fmt.Sprintf("jobs: the jobs of pipeline %d were not listed", c.ID))\n')],
  run(J3, "jobs: not listed, past the deadline: a visible pass is not PASSED"))
m("A: a listing without retried attempts past the deadline: a visible pass is PASSED",
  [(J, "so retried attempts may be missing\", c.ID))\n\t\t\tcomplete = false\n", "so retried attempts may be missing\", c.ID))\n")],
  run(J3, "jobs: listed without retried attempts, past the deadline"))
AMAL = '\t\t\tif a.ID == 0 || a.Name == "" || a.Status == "" {'
m("A: a job object without an id is accepted", [(J, AMAL, '\t\t\tif a.Name == "" || a.Status == "" {')], run(J3, "jobs: a job object has no id"))
m("A: a job object without a name is accepted", [(J, AMAL, '\t\t\tif a.ID == 0 || a.Status == "" {')], run(J3, "jobs: a job object has no name"))
m("A: a job object without a status is accepted", [(J, AMAL, '\t\t\tif a.ID == 0 || a.Name == "" {')], run(J3, "jobs: a job object has no status"))
ONLY_NEWEST = [(J, "\t\tfor _, c := range candidates {\n\t\t\tfor _, a := range c.Jobs {\n\t\t\t\tif a.Name == t.Name() {",
                   "\t\tfor _, c := range candidates[max(0, len(candidates)-1):] {\n\t\t\tfor _, a := range c.Jobs {\n\t\t\t\tif a.Name == t.Name() {")]
m("[NAMED] conservative-across-pipelines: only the NEWEST candidate pipeline is judged (a pass there outweighs a recurrence in an older one)",
  ONLY_NEWEST, run(CONS, "recurs in pipeline 501", "listed in id order"))
for kind in ("changed", "weakened", "skipped"):
    m("conservative-across-pipelines: only the newest candidate pipeline is judged (%s in the older one)" % kind,
      ONLY_NEWEST, run(CONS, "%s in pipeline 501" % kind, "listed in id order"))
ONLY_OLDEST = [(J, "\t\tfor _, c := range candidates {\n\t\t\tfor _, a := range c.Jobs {\n\t\t\t\tif a.Name == t.Name() {",
                   "\t\tfor _, c := range candidates[:min(1, len(candidates))] {\n\t\t\tfor _, a := range c.Jobs {\n\t\t\t\tif a.Name == t.Name() {")]
m("conservative-across-pipelines: only the OLDEST candidate pipeline is judged", ONLY_OLDEST, run(CONS, "recurs in pipeline 502", "listed in id order"))
ANY_PASS = [(J, "\t\tj := judgeJob(t, attempts, complete, sameVersions)\n",
                "\t\tfor _, a := range attempts {\n\t\t\tif a.Status == StatusSuccess && !a.AllowFailure {\n\t\t\t\tattempts = []Attempt{a}\n\t\t\t}\n\t\t}\n\t\tj := judgeJob(t, attempts, complete, sameVersions)\n")]
m("conservative-across-pipelines: a pass in ANY pipeline is a pass", ANY_PASS, run(CONS, "recurs in pipeline 502", "listed newest first"))
LATEST_ONLY = [(J, "\t\tj := judgeJob(t, attempts, complete, sameVersions)\n",
                   "\t\tsort.Slice(attempts, func(i, k int) bool { return attempts[i].ID < attempts[k].ID })\n\t\tif len(attempts) > 1 {\n\t\t\tattempts = attempts[len(attempts)-1:]\n\t\t}\n\t\tj := judgeJob(t, attempts, complete, sameVersions)\n")]
m("[NAMED] a human retry is not PASSED: only the LATEST attempt of a diagnosed job is judged (a retry replaces the failure)",
  LATEST_ONLY, run(RETRY))
m("a human retry: only the latest attempt is judged (C5, retried until green)", LATEST_ONLY,
  run(CHEATS, "C5 runtime: retried until green: two failures, then a pass"))
m("a human retry: only the latest attempt is judged (J3 flaky row)", LATEST_ONLY, run(J3, "flaky: failed the same way, then passed on a retry"))
m("Judge writes into the caller's facts",
  [(J, "\t\tinT[t.Name()] = true\n", "\t\tinT[t.Name()] = true\n\t\tf.Fix.Targets[0].Generic = true\n")], run('TestJudgeDoesNotModifyItsFacts'))

# ============================ J3 per diagnosed job ============================
SV = "\tsameVersions := f.Fix.SignatureVersion == pipelinectx.SignatureVersion &&\n\t\tf.Fix.ExcerptVersion == pipelinectx.ExcerptVersion\n"
m("J3: a signature of another SignatureVersion is compared anyway",
  [(J, SV, "\tsameVersions := f.Fix.ExcerptVersion == pipelinectx.ExcerptVersion\n")],
  run(J3, "versions: a failed attempt cannot be compared with a signature of another version"))
m("J3: a signature of another ExcerptVersion is compared anyway",
  [(J, SV, "\tsameVersions := f.Fix.SignatureVersion == pipelinectx.SignatureVersion\n")], run(J3, "versions: nor with an excerpt of another version"))
m("J3: incomparable versions are not a finding", [off(J, "\t\tif incomparable {")],
  run(J3, "versions: a failed attempt cannot be compared with a signature of another version"))
m("J3: incomparable versions refuse a pass too",
  [(J, "\tif f.Failed > 0 {\n\t\tif incomparable {", "\tif !sv {\n\t\treturn RuleJ3Versions\n\t}\n\tif f.Failed > 0 {\n\t\tif incomparable {"),
   (J, "func jobRule(f JobFinding, weakened, same, unknown, incomparable, complete bool) Rule {", "func jobRule(f JobFinding, weakened, same, unknown, incomparable, complete, sv bool) Rule {"),
   (J, "\tf.Rule = jobRule(f, weakened, same, unknown, incomparable, complete)", "\tf.Rule = jobRule(f, weakened, same, unknown, incomparable, complete, sameVersions)")],
  run(J3, "versions: a pass needs no signature"))
m("J3 absent: a job in no pipeline is PASSED (zero attempts, all of them green)", [off(J, "\tif f.Attempts == 0 {")],
  run(J3, "absent: the job is in no candidate pipeline"))
m("J3 absent (C1): a deleted job is PASSED", [off(J, "\tif f.Attempts == 0 {")], run(CHEATS, "C1 runtime: the job is in no pipeline of the fix"))
WK = "\t\tif a.AllowFailure {\n\t\t\tweakened = true\n\t\t}\n"
m("J3 weakened: allow_failure on the job object is ignored", [(J, WK, "")], run(J3, "weakened: the job passed and was allowed to fail"))
m("J3 weakened (C2): a failed, allowed-to-fail job on a green pipeline is not FORGED", [(J, WK, "")],
  run(CHEATS, "C2 runtime: the job object says allow_failure"))
m("J3 weakened: only a FAILED allowed-to-fail attempt counts",
  [(J, "\t\tif a.AllowFailure {\n\t\t\tweakened = true", "\t\tif a.AllowFailure && a.Status == StatusFailed {\n\t\t\tweakened = true")],
  run(J3, "weakened: the job passed and was allowed to fail"))
m("J3 weakened: only the LATEST attempt's allow_failure counts",
  [(J, "\t\tif a.AllowFailure {\n\t\t\tweakened = true", "\t\tweakened = false\n\t\tif a.AllowFailure {\n\t\t\tweakened = true")],
  run(J3, "weakened: one retried attempt was allowed to fail"))
m("J3 weakened: WEAKENED is not a finding", [off(J, "\tif weakened {")], run(J3, "weakened: the job failed and was allowed to fail (the pipeline is green)"))
m("J3 weakened: an incomplete listing hides a weakened job", [(J, "\tif weakened {", "\tif weakened && complete {")],
  run(J3, "jobs: an incomplete listing past the deadline still shows a weakened job"))
m("J3: an incomplete listing is judged as if complete", [off(J, "\tif !complete {")],
  run(J3, "jobs: not listed, past the deadline: a visible pass is not PASSED"))
m("J3: an incomplete listing still proves a recurrence",
  [(J, "\tif !complete {\n\t\treturn RuleJ3Unread", "\tif !complete && !same {\n\t\treturn RuleJ3Unread")],
  run(LATE, "jobs unread, a recurrence in the part that was read: not provable, UNVERIFIABLE"))
TR = "\t\tif !a.Trace.Read {\n\t\t\tunknown = true\n\t\t\tcontinue\n\t\t}\n"
m("[NAMED] a read failure yields no verdict: an unread trace is read as an empty log", [(J, TR, "")], run(READF))
m("a read failure: an unread trace is read as an empty log (J3 row)", [(J, TR, "")], run(J3, "unknown: a failed attempt's log was not read"))
m("a read failure: an unread trace is a DIFFERENT failure (CHANGED)",
  [(J, TR, "\t\tif !a.Trace.Read {\n\t\t\tcontinue\n\t\t}\n")], run(READF))
m("a read failure: UNKNOWN is not a finding", [off(J, "\t\tif unknown {")], run(J3, "unknown: the unread attempt was retried into a pass -- still no verdict"))
m("a read failure: UNKNOWN does not hold the verdict", [off(J, "\t\tif j.Rule == RuleJ3Unknown {")], run(READF))
m("a read failure: unread traces do not stop a verdict before the deadline", [off(J, "\tif !past && len(unreadTraces) > 0 {")], run(READF))
m("a read failure: unread traces do not stop a verdict (the property test)", [off(J, "\tif !past && len(unreadTraces) > 0 {")], run(NOPASS))
m("a read failure: no fact stops a verdict before the deadline", [off(J, "\tif !past && len(unread) > 0 {")], run(NOPASS))
m("a read failure: an unread definition does not stop a verdict before the deadline", [off(J, "\tif !past && len(unread) > 0 {")],
  run(J2, "the delta was not read"))
m("a read failure: it is still no verdict past the deadline (never bounded)",
  [(J, "\tif !past && len(unreadTraces) > 0 {", "\tif len(unreadTraces) > 0 {")], run(J3, "unknown past the deadline is not observed"))
m("a read failure: past the deadline UNKNOWN is not renamed NOT_OBSERVED(unread)",
  [(J, "\t\tif res.Jobs[i].Rule == RuleJ3Unknown {\n\t\t\tres.Jobs[i].Rule = RuleJ3Unread", "\t\tif false {\n\t\t\tres.Jobs[i].Rule = RuleJ3Unread")],
  run(J3, "unknown past the deadline is not observed"))
m("J3: a generic TARGET is compared by signature",
  [(J, "\t\tif t.Generic {\n\t\t\tsame = true\n\t\t\tcontinue\n\t\t}\n", "")],
  run(J3, "recurs: the RECORDED signature is generic, the new error is specific (conservative)"))
m("J3: a generic target needs a trace",
  [(J, "\t\tif t.Generic {\n\t\t\tsame = true\n\t\t\tcontinue\n\t\t}\n", "\t\tif t.Generic && a.Trace.Read {\n\t\t\tsame = true\n\t\t\tcontinue\n\t\t}\n")],
  run(J3, "recurs: a generic target needs no trace"))
GEN = "\t\tcase pipelinectx.GenericFirstError(a.Trace.FirstError):\n\t\t\tsame = true\n"
m("J3: an attempt that leaves only the runner's summary is a DIFFERENT failure", [(J, GEN, "")],
  run(J3, "recurs: the attempt's only error is the runner's summary (conservative)"))
m("J3: an attempt with no error line is a DIFFERENT failure", [(J, GEN, "")], run(J3, "recurs: the attempt's log has no error line (conservative)"))
m("J3 (C13): breaking the job earlier turns RECURS into CHANGED", [(J, GEN, "")],
  run(CHEATS, "C13 runtime: the job is broken earlier, so only the runner's summary is left: RECURS"))
m("J3: the same key is not `same`", [(J, "\t\tcase key == t.Key:\n\t\t\tsame = true\n", "")], run(J3, "recurs: the same failure"))
m("J3: the key ignores failure_reason", [(J, "FailureReason: a.FailureReason,", "FailureReason: t.Key.FailureReason,")],
  run(J3, "changed: the same error text under another failure_reason"))
m("J3: the key ignores the stage", [(J, "\t\t\tStage: a.Stage, Name: t.Name(),", "\t\t\tStage: t.Key.Stage, Name: t.Name(),")],
  run(J3, "changed: the same error text in another stage"))
m("J3: the signature is the raw line, not the canonical one",
  [(J, "\t\t\tErrorSignature: pipelinectx.ErrorSignature(a.Trace.FirstError),", "\t\t\tErrorSignature: a.Trace.FirstError,")],
  run(J3, "recurs: the same failure with other numbers in it"))
m("J3 flaky: failed-the-same-way-then-passed is RECURS", [off(J, "\t\tif same && f.Succeeded > 0 {")],
  run(J3, "flaky: passed, then failed the same way on a later attempt"))
m("J3 recurs: a recurrence is CHANGED", [(J, "\t\tif same {\n\t\t\treturn RuleJ3Recurs\n\t\t}\n", "")], run(J3, "recurs: failed twice the same way"))
m("J3 recurs: one new failure among recurrences makes it CHANGED",
  [(J, "\t\tcase f.NewSignature == \"\":\n\t\t\tf.NewSignature = key.ErrorSignature", "\t\tdefault:\n\t\t\tsame = false\n\t\t\tf.NewSignature = key.ErrorSignature")],
  run(J3, "recurs: one attempt the old way, then one a new way"))
m("J3 flaky: failed-differently-then-passed is CHANGED",
  [(J, "\t\tif f.Succeeded > 0 {\n\t\t\treturn RuleJ3FlakyChanged\n\t\t}\n", "")], run(J3, "flaky: failed a different way, then passed on a retry"))
m("J3 changed (C13): a different failure of the same job is PASSED",
  [(J, "\t\treturn RuleJ3Changed\n\t}\n", "\t\treturn RuleJ3Passed\n\t}\n")], run(CHEATS, "C13 runtime: the job fails with another message: CHANGED, never PASSED"))
m("J3 passed: one success among other statuses is PASSED", [(J, "\tif f.Succeeded == f.Attempts {", "\tif f.Succeeded > 0 {")],
  run(J3, "status: one attempt passed and one was canceled -- not all succeeded"))
# `created` and `scheduled` are ACTIVE statuses since gonk-5b2f: before the
# deadline J1 waits for them whatever J3 would say, so their J3 row is the one
# judged past the deadline.
for st in ("manual", "skipped", "canceled", "created, past the deadline", "scheduled (start_in), past the deadline"):
    m("J3 status: a job that is `%s` is PASSED" % st,
      [(J, "\tif f.Succeeded == f.Attempts {\n\t\treturn RuleJ3Passed\n\t}\n\treturn RuleJ3Status", "\treturn RuleJ3Passed")], run(J3, "status: " + st))
m("J3 status (C3): a manual job nobody ran is PASSED",
  [(J, "\tif f.Succeeded == f.Attempts {\n\t\treturn RuleJ3Passed\n\t}\n\treturn RuleJ3Status", "\treturn RuleJ3Passed")],
  run(CHEATS, "C3 runtime: the job is manual and nobody ran it"))
m("J3: a failed attempt is not counted when another was canceled",
  [(J, "\tif f.Failed > 0 {\n\t\tif incomparable {", "\tif f.Failed > 0 && f.Failed+f.Succeeded == f.Attempts {\n\t\tif incomparable {")],
  run(J3, "a failed attempt outranks a canceled one"))
m("J3: the new signature of a changed failure is not carried",
  [(J, "\t\tcase f.NewSignature == \"\":\n\t\t\tf.NewSignature = key.ErrorSignature\n", "")], run('TestAChangedFailureCarriesItsNewSignature'))
m("J3: a recurrence among other failures offers a `new` signature",
  [off(J, "\tif f.Rule != RuleJ3Changed && f.Rule != RuleJ3FlakyChanged {")], run('TestAChangedFailureCarriesItsNewSignature'))
FT = 'TestFailedTrace'
FTT = 'TestFailedTraceHonoursTheTruncationFlags'
EXC = "\tex := pipelinectx.ExcerptWith(l.Data, pipelinectx.Options{HeadTruncated: l.HeadTruncated})\n"
m("FailedTrace: a log that was read is not marked read",
  [(J, "\treturn Trace{Read: true, FirstError: ex.FirstError}", "\treturn Trace{FirstError: ex.FirstError}")], run(FT))
m("FailedTrace: the first error is the first LINE",
  [(J, "\treturn Trace{Read: true, FirstError: ex.FirstError}",
       "\t_ = ex\n\treturn Trace{Read: true, FirstError: strings.SplitN(string(l.Data), \"\\n\", 2)[0]}")], run(FT))
# ---- gonk-etvm: the truncation flags are honoured INSIDE the function ----
m("FailedTrace (gonk-etvm): the head-truncation flag is ignored (the defect: a cut line is the first error)",
  [(J, EXC, "\tex := pipelinectx.ExcerptWith(l.Data, pipelinectx.Options{})\n")],
  run(FTT, "head truncated: the cut first line is never the first error"))
m("FailedTrace (gonk-etvm): the head-truncation flag is ignored -- through Judge a recurrence reads as CHANGED",
  [(J, EXC, "\tex := pipelinectx.ExcerptWith(l.Data, pipelinectx.Options{})\n")],
  run(FTT, "head truncated: through Judge it is the recorded failure, not a new one"))
m("FailedTrace: the first line of EVERY log is dropped",
  [(J, EXC, "\tex := pipelinectx.ExcerptWith(l.Data, pipelinectx.Options{HeadTruncated: true})\n")], run(FTT, "head truncated: the cut first line is never the first error"))
m("FailedTrace (gonk-etvm): a read that stopped before the end of the log is a log that was read",
  [off(J, "\tif l.TailTruncated {")], run(FTT, "tail truncated: the end of the log was not read, so the log was not read"))
m("FailedTrace: EVERY log is not read", [(J, "\tif l.TailTruncated {\n", "\tif true || l.TailTruncated {\n")], run(FT))
FRAG = "\tif l.HeadTruncated && len(l.Data) > 0 && bytes.IndexByte(l.Data, '\\n') < 0 {"
m("FailedTrace (gonk-etvm): a lone cut line is a first error", [(J, FRAG, FRAG.replace("< 0 {", "< -1 {"))],
  run(FTT, "head truncated, one line and no newline: only a fragment was read"))
m("FailedTrace: a whole one-line log with no newline is not read",
  [(J, FRAG, "\tif len(l.Data) > 0 && bytes.IndexByte(l.Data, '\\n') < 0 {")], run(FTT, "a whole log of one line with no newline is read"))
m("FailedTrace: an EMPTY head-truncated read is not read", [(J, FRAG, "\tif l.HeadTruncated && bytes.IndexByte(l.Data, '\\n') < 0 {")],
  run(FTT, "an empty read is an empty log, whatever the flags"))
m("FailedTrace: a head-truncated log with a cut line and nothing after it is not read",
  [(J, FRAG, "\tif l.HeadTruncated && len(l.Data) > 0 && bytes.Count(l.Data, []byte(\"\\n\")) < 2 {")],
  run(FTT, "head truncated, nothing after the cut line"))

# ============================== J4 other jobs ==============================
m("J4: a diagnosed job is also reported as an `other` job", [off(J, "\t\t\tif inT[a.Name] {")],
  run(J3, "changed: the same job fails with a different error"))
LAT = "\t\t\tif l, has := latest[a.Name]; !has || a.ID > l.ID {"
m("J4: the EARLIEST attempt of another job is read", [(J, LAT, "\t\t\tif l, has := latest[a.Name]; !has || a.ID < l.ID {")],
  run(J4, "latest attempt: another job passed and then failed on a later attempt"))
m("J4: a retried-into-a-pass other job still counts (earliest attempt)", [(J, LAT, "\t\t\tif l, has := latest[a.Name]; !has || a.ID < l.ID {")],
  run(J4, "latest attempt: another job failed and was retried into a pass -- nothing found"))
m("J4: the latest attempt is the last one LISTED", [(J, LAT, "\t\t\tif l, has := latest[a.Name]; !has || a.ID != l.ID {")],
  run(J4, "latest attempt is by job id, not by position in the listing"))
m("J4: a job that did not fail is a finding", [off(J, "\t\t\tif a.Status != StatusFailed {")], run('TestTheBaselineFixIsFixed'))
m("J4: a job that is skipped now is a finding",
  [(J, "\t\t\tif a.Status != StatusFailed {", "\t\t\tif a.Status != StatusFailed && a.Status != StatusSkipped {")],
  run(J4, "another job that is merely skipped now is nothing"))
m("J4 regressed: an allowed-to-fail failure is REGRESSED",
  [(J, "\t\t\tif !a.AllowFailure && required[name] == 0 {", "\t\t\tif required[name] == 0 {")],
  run(J4, "regressed, but allowed to fail: not REGRESSED, and not FIXED"))
m("J4 regressed: REGRESSED is not a finding (it reads as allowed)",
  [(J, "\t\tcase has && b.Status == StatusSuccess && required[name] != 0:\n", "\t\tcase false:\n")], run(J4, "regressed: passed on the base, fails now"))
m("J4 regressed: a regression that is allowed to fail is nothing new (REVEALED)",
  [(J, "\t\tcase has && b.Status == StatusSuccess:\n", "\t\tcase false:\n")], run(J4, "regressed, but allowed to fail: not REGRESSED, and not FIXED"))
m("J4 remaining: a job that failed before is REVEALED", [(J, "\t\tcase has && b.Status == StatusFailed:\n", "\t\tcase false:\n")],
  run(J4, "remaining: failed on the base (uncited), fails now"))
m("J4 remaining: a job that failed before is REGRESSED",
  [(J, "\t\tcase has && b.Status == StatusSuccess && required[name] != 0:\n", "\t\tcase has && required[name] != 0:\n")],
  run(J4, "remaining: failed on the base (uncited), fails now"))
for base_status in ("skipped on the base", "manual on the base", "canceled on the base"):
    m("J4 revealed: a job that was not success on the base is REGRESSED (%s)" % base_status,
      [(J, "\t\tcase has && b.Status == StatusSuccess && required[name] != 0:\n", "\t\tcase has && b.Status != StatusFailed && required[name] != 0:\n")],
      run(J4, "revealed: " + base_status))
m("J4 revealed: a job absent from the base is REGRESSED",
  [(J, "\t\tcase has && b.Status == StatusSuccess && required[name] != 0:\n", "\t\tcase (!has || b.Status == StatusSuccess) && required[name] != 0:\n")],
  run(J4, "revealed: absent from the base"))
m("J4: only the FIRST candidate pipeline is read for other jobs",
  [(J, "\tfor _, c := range candidates {\n\t\tlatest := map[string]Attempt{}", "\tfor _, c := range candidates[:min(1, len(candidates))] {\n\t\tlatest := map[string]Attempt{}")],
  run(J4, "a regression in ONE of two candidate pipelines decides"))
m("J4: the latest attempt is taken ACROSS pipelines (the newest pipeline's pass hides an older failure)",
  [(J, "\tfor _, c := range candidates {\n\t\tlatest := map[string]Attempt{}", "\tlatest := map[string]Attempt{}\n\tfor _, c := range candidates[max(0, len(candidates)-1):] {\n\t\tfor _, cc := range candidates {\n\t\t\tfor _, a := range cc.Jobs {\n\t\t\t\tif l, has := latest[a.Name]; !inT[a.Name] && (!has || a.ID > l.ID) {\n\t\t\t\t\tlatest[a.Name] = a\n\t\t\t\t}\n\t\t\t}\n\t\t}"),
   (J, "\t\tfor _, a := range c.Jobs {\n\t\t\tif inT[a.Name] {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tif l, has := latest[a.Name]; !has || a.ID > l.ID {\n\t\t\t\tlatest[a.Name] = a\n\t\t\t}\n\t\t}\n", "")],
  run('TestJ4FindingsAreSortedAndNameTheirPipeline'))
m("J4: a required failure in a later pipeline does not outrank an allowed one in an earlier",
  [(J, "\t\t\tif !a.AllowFailure && required[name] == 0 {", "\t\t\tif !a.AllowFailure && required[name] == 0 && failed[name] == c.ID {")],
  run(J4, "allowed to fail in one candidate, required in the other: REGRESSED"))
m("J4: findings are not in name order", [(J, "\tsort.Strings(names)\n", "\tsort.Sort(sort.Reverse(sort.StringSlice(names)))\n")],
  run('TestJ4FindingsAreSortedAndNameTheirPipeline'))
m("J4: REGRESSED names the first failing pipeline, not the first REQUIRED failure",
  [(J, "\t\t\to.Rule, o.PipelineID = RuleJ4Regressed, required[name]", "\t\t\to.Rule = RuleJ4Regressed")], run('TestJ4FindingsAreSortedAndNameTheirPipeline'))
m("J4: the base status is not carried", [(J, "PipelineID: failed[name], BaseStatus: b.Status}", "PipelineID: failed[name]}")],
  run('TestJ4FindingsAreSortedAndNameTheirPipeline'))

# ================================ J5 verdict ================================
m("J5 FORGED: a J2 finding is not a verdict", [off(J, "\tif len(forged) > 0 {")], run(J5, "FORGED (definition) over NOT_FIXED"))
m("J5 FORGED: WEAKENED is not FORGED",
  [(J, "\tif j, found := firstJob(res.Jobs, RuleJ3Weakened); found {", "\tif j, found := firstJob(res.Jobs); found {")], run(J5, "FORGED (weakened) over NOT_FIXED"))
m("J5 NOT_FIXED: RECURS is not NOT_FIXED",
  [(J, "\tif j, found := firstJob(res.Jobs, RuleJ3Recurs); found {", "\tif j, found := firstJob(res.Jobs); found {")], run(J5, "NOT_FIXED over FLAKY"))
m("J5 FLAKY: FLAKY(recurs-and-passes) is not FLAKY",
  [(J, "firstJob(res.Jobs, RuleJ3FlakySame, RuleJ3FlakyChanged); found {", "firstJob(res.Jobs, RuleJ3FlakyChanged); found {")], run(J5, "FLAKY over REGRESSED"))
m("J5 FLAKY: FLAKY(fails-and-passes) is not FLAKY",
  [(J, "firstJob(res.Jobs, RuleJ3FlakySame, RuleJ3FlakyChanged); found {", "firstJob(res.Jobs, RuleJ3FlakySame); found {")],
  run(J3, "flaky: failed a different way, then passed on a retry"))
m("J5 UNVERIFIABLE: a J1 / J2 cause is not a verdict", [off(J, "\tif len(unverifiable) > 0 {")], run(J5, "UNVERIFIABLE (the definition) over REGRESSED"))
m("J5 UNVERIFIABLE: an unresolvable definition is a pass", [off(J, "\tif len(unverifiable) > 0 {")], run(J2, "unresolvable on both sides"))
NOTOBS = "\tif j, found := firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3Changed); found {"
m("J5 UNVERIFIABLE: a job that was not observed is a pass", [(J, NOTOBS, "\tif j, found := firstJob(res.Jobs); found {")],
  run(J5, "UNVERIFIABLE (a job not observed) over REGRESSED"))
m("J5 UNVERIFIABLE (C4): a skipped job is a pass", [(J, NOTOBS, "\tif j, found := firstJob(res.Jobs); found {")], run(CHEATS, "C4 runtime: the job was skipped"))
m("J5: CHANGED is not observed (owner decision Q3 reversed)", [(J, NOTOBS, "\tif j, found := firstJobNot(res.Jobs, RuleJ3Passed); found {")],
  run(J5, "FIXED_NEW_FAILURE (changed) over FIXED"))
m("J5 REGRESSED: REGRESSED is not a verdict",
  [(J, "\tif o, found := firstOther(res.Others, RuleJ4Regressed); found {", "\tif o, found := firstOther(res.Others); found {")],
  run(J5, "REGRESSED over FIXED_NEW_FAILURE (revealed)"))
m("J5 FIXED_NEW_FAILURE: CHANGED is not a verdict (a different failure of the job is FIXED or unexplained)",
  [(J, "\tif j, found := firstJob(res.Jobs, RuleJ3Changed); found {", "\tif j, found := firstJob(res.Jobs); found {")],
  run(J5, "FIXED_NEW_FAILURE (changed) over FIXED"))
OTH = "firstOther(res.Others, RuleJ4Remaining, RuleJ4Revealed, RuleJ4RegressedAllowed); found {"
m("J5 FIXED_NEW_FAILURE: REVEALED is not a verdict", [(J, OTH, "firstOther(res.Others, RuleJ4Remaining, RuleJ4RegressedAllowed); found {")],
  run(J5, "FIXED_NEW_FAILURE (revealed) over FIXED"))
m("J5 FIXED_NEW_FAILURE: REMAINING is not a verdict", [(J, OTH, "firstOther(res.Others, RuleJ4Revealed, RuleJ4RegressedAllowed); found {")],
  run(J4, "remaining: failed on the base (uncited), fails now"))
m("J5 FIXED_NEW_FAILURE: an allowed-to-fail regression is FIXED", [(J, OTH, "firstOther(res.Others, RuleJ4Remaining, RuleJ4Revealed); found {")],
  run(J4, "regressed, but allowed to fail: not REGRESSED, and not FIXED"))
m("J5 FIXED: the candidate pipelines need not be `success`", [off(J, "\t\tif c.Status != StatusSuccess {")],
  run(GREEN, "FIXED needs every candidate to be success: canceled"))
m("J5 FIXED: only the first candidate pipeline needs to be `success`",
  [(J, "\t\tif c.Status != StatusSuccess {", "\t\tif c.ID == candidates[0].ID && c.Status != StatusSuccess {")], run(GREEN, "one of two candidates not success"))
m("J5: pipeline green IS the criterion (a green pipeline is FIXED whatever the jobs say)",
  [(J, "\t// J5 verdict, first match.\n", "\t// J5 verdict, first match.\n\tif len(candidates) > 0 && candidates[0].Status == StatusSuccess {\n\t\tres.Decision, res.Verdict, res.State, res.Rule, res.Detail = DecisionVerdict, VerdictFixed, StateFixed, RuleJ5Fixed, \"green\"\n\t\treturn res\n\t}\n")],
  run(GREEN, "a green pipeline is not the criterion met"))
m("J5: a red pipeline is never the criterion met",
  [(J, "\t// J5 verdict, first match.\n", "\t// J5 verdict, first match.\n\tfor _, c := range candidates {\n\t\tif c.Status == StatusFailed {\n\t\t\tres.Decision, res.Verdict, res.State, res.Rule, res.Detail = DecisionVerdict, VerdictNotFixed, StateNotFixed, RuleJ3Recurs, \"red\"\n\t\t\treturn res\n\t\t}\n\t}\n")],
  run(GREEN, "the criterion is met on a red pipeline"))
m("J5: the verdict does not select its state", [(J, "\t\tres.State, _ = StateFor(v)\n", "")], run('TestTheBaselineFixIsFixed'))


def swap_tiers(a, b):
    """Swap two adjacent rungs of the J5 ladder, found by their comments."""
    src = open(J).read()
    marks = ["\t// FORGED: a J2 violation, or any WEAKENED.\n", "\t// NOT_FIXED: any RECURS.\n", "\t// FLAKY.\n",
             "\t// UNVERIFIABLE: from J1 / J2, or any NOT_OBSERVED.\n", "\t// REGRESSED.\n",
             "\t// FIXED_NEW_FAILURE: any CHANGED, REMAINING or REVEALED.\n", "\t// FIXED: every t PASSED"]
    pos = [src.find(x) for x in marks]
    if min(pos) < 0 or pos != sorted(pos):
        return (J, "\x00 the J5 ladder's comments were not found \x00", "")
    first, second = src[pos[a]:pos[a + 1]], src[pos[b]:pos[b + 1]]
    return (J, first + second, second + first)


for (a, b, an, bn, sub) in ((0, 1, "FORGED", "NOT_FIXED", "FORGED (weakened) over NOT_FIXED"),
                            (1, 2, "NOT_FIXED", "FLAKY", "NOT_FIXED over FLAKY"),
                            (2, 3, "FLAKY", "UNVERIFIABLE", "FLAKY over UNVERIFIABLE (the definition)"),
                            (3, 4, "UNVERIFIABLE", "REGRESSED", "UNVERIFIABLE (a job not observed) over REGRESSED"),
                            (4, 5, "REGRESSED", "FIXED_NEW_FAILURE", "REGRESSED over FIXED_NEW_FAILURE (changed)")):
    m("J5 order: %s is matched before %s" % (bn, an), [swap_tiers(a, b)], run(J5, sub))
m("J5 order: past the deadline a timeout outranks a visible definition violation",
  [(J, "\tif len(forged) > 0 {", "\tif len(forged) > 0 && len(unverifiable) == 0 {")], run(LATE, "unsettled, a definition violation"))
m("J5 order: past the deadline a timeout outranks a visible recurrence",
  [(J, "\tif j, found := firstJob(res.Jobs, RuleJ3Recurs); found {", "\tif j, found := firstJob(res.Jobs, RuleJ3Recurs); found && len(unverifiable) == 0 {")],
  run(LATE, "unsettled, a recurrence already visible"))
m("J5 order: past the deadline a regression is reported although the pipelines never settled",
  [(J, "\tif len(unverifiable) > 0 {", "\tif len(unverifiable) > 0 && len(res.Others) == 0 {")],
  run(LATE, "unsettled, another job already regressed: not REGRESSED, the pipelines never settled"))

# ====================== 7.1 / 7.3: verdict -> state -> outcome ======================
m("7.3: NOT_FIXED is reported as success (an artifact is present)",
  [(O, "\tcase StateNotFixed:\n\t\treturn false, true", "\tcase StateNotFixed:\n\t\treturn true, true")], run(V2S, "NOT_FIXED"))
m("7.3: FORGED is reported as success", [(O, "\tcase StateNotFixed:\n\t\treturn false, true", "\tcase StateNotFixed:\n\t\treturn true, true")], run(V2S, "FORGED"))
m("7.1: FORGED has no state", [(O, "\tcase VerdictForged, VerdictNotFixed:\n", "\tcase VerdictNotFixed:\n")], run(V2S, "FORGED"))
for v, st in (("Regressed", "REGRESSED"), ("Flaky", "FLAKY"), ("Unverifiable", "UNVERIFIABLE")):
    m("7.3: %s escalates (its state is not-fixed)" % st,
      [(O, "\tcase Verdict%s:\n\t\treturn State%s, true" % (v, v), "\tcase Verdict%s:\n\t\treturn StateNotFixed, true" % v)], run(V2S, st))
m("7.1: FIXED_NEW_FAILURE is the state `fixed`",
  [(O, "\tcase VerdictFixedNewFailure:\n\t\treturn StateFixedNewFailure, true", "\tcase VerdictFixedNewFailure:\n\t\treturn StateFixed, true")],
  run(V2S, "FIXED_NEW_FAILURE"))
m("7.1: FIXED is the state `fixed-new-failure`",
  [(O, "\tcase VerdictFixed:\n\t\treturn StateFixed, true", "\tcase VerdictFixed:\n\t\treturn StateFixedNewFailure, true")], run(V2S, "FIXED"))
PRES = "\tcase StateNeedsScope, StateFixed, StateFixedNewFailure, StateFlaky, StateUnverifiable,\n\t\tStateRegressed, StateTakenOver, StateClosedUnmerged, StateMerged:"
for st, sub in (("StateNeedsScope, ", "needs-scope"), ("StateFixed, ", "fixed"), ("StateFixedNewFailure, ", "fixed-new-failure"),
                ("StateFlaky, ", "flaky"), ("StateRegressed, ", "regressed"), ("StateTakenOver, ", "taken-over"),
                ("StateClosedUnmerged, ", "closed-unmerged"), (", StateMerged", "merged")):
    m("7.3: `%s` reports no outcome" % sub, [(O, PRES, PRES.replace(st, "", 1))], run(STATES, sub))
m("7.3: `unverifiable` reports no outcome", [(O, PRES, PRES.replace(" StateUnverifiable,", "", 1))], run(STATES, "unverifiable"))
for st, sub in (("StateVerifying", "verifying"), ("StateProposing", "proposing"), ("StateAwaitingRun", "awaiting-run"),
                ("StateDenied", "denied"), ("StateConfirmed", "confirmed"), ("StateRecurred", "recurred")):
    m("7.3: `%s` reports success" % sub, [(O, PRES, PRES.replace("StateMerged:", "StateMerged, %s:" % st, 1))], run(STATES, sub))
m("7.3: an attempt nobody paid for escalates (gate.Classify bypassed)",
  [(O, "gate.Signals{ArtifactPresent: present, ModelTokens: modelTokens}", "gate.Signals{ArtifactPresent: present, ModelTokens: 1}")], run(V2S, "NOT_FIXED"))
m("7.3: more than not-fixed escalates (unverifiable is absent)",
  [(O, PRES, PRES.replace(" StateUnverifiable,", "", 1)), (O, "\tcase StateNotFixed:\n", "\tcase StateNotFixed, StateUnverifiable:\n")],
  run('TestOnlyNotFixedEscalates'))
m("7.3: something that is not a verdict has an outcome",
  [(O, "\ts, known := StateFor(v)\n\tif !known {\n\t\treturn \"\", false\n\t}", "\ts, known := StateFor(v)\n\tif !known {\n\t\ts = StateFixed\n\t}")],
  run('TestWhatIsNotAVerdictOrAStateHasNoOutcome'))
m("7.1: every string is a known state", [(O, "\t\tif k == s {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false", "\t\tif k == s {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn true")],
  run('TestWhatIsNotAVerdictOrAStateHasNoOutcome'))
TERM = "\tcase StateProposing, StateAwaitingRun, StateVerifying, StateNotFixed:"
for st, sub in (("StateProposing, ", "proposing"), ("StateAwaitingRun, ", "awaiting-run"), ("StateVerifying, ", "verifying"), (", StateNotFixed", "not-fixed")):
    m("7.1: `%s` is terminal" % sub, [(O, TERM, TERM.replace(st, "", 1))], run(STATES, sub))
m("7.1: `flaky` is not terminal", [(O, TERM, TERM.replace("StateNotFixed:", "StateNotFixed, StateFlaky:"))], run(STATES, "flaky"))
m("7.1: `fixed` is not terminal (it would be proposed again as in flight)", [(O, TERM, TERM.replace("StateNotFixed:", "StateNotFixed, StateFixed:"))],
  run(STATES, "fixed"))
m("7.1: J5's order is not the order of Verdicts()",
  [(O, "\treturn []Verdict{VerdictForged, VerdictNotFixed, VerdictFlaky, VerdictUnverifiable,", "\treturn []Verdict{VerdictNotFixed, VerdictForged, VerdictFlaky, VerdictUnverifiable,")],
  run(V2S, "FORGED"))
m("constants: the ci lift is not scope's `ci` class", [(J, 'const LiftCI = "ci"', 'const LiftCI = "cicd"')], run('TestFixableClassesAreThePlansThree'))
m("constants: FixableClasses is not the plan's three, in order",
  [(E, "return []string{ClassCode, ClassTest, ClassConfiguration} }", "return []string{ClassTest, ClassCode, ClassConfiguration} }")],
  run('TestFixableClassesAreThePlansThree'))
m("test table: a cheat row is dropped (the plan-row test must notice)",
  [(CT, '\t"C9":  "static, part SCOPE: the floor is not liftable",\n', "")], run('TestEveryPlanCheatIsAccountedFor'))
m("test table: a both-layer cheat loses its runtime fixture (the plan-row test must notice)",
  [(CT, '{cheat: "C2", name: "the job object says allow_failure", layer: "runtime",', '{cheat: "C17", name: "the job object says allow_failure", layer: "runtime",')],
  run('TestEveryPlanCheatIsAccountedFor'))

# ------------------------------------------------------------------ the runner
if '--list' in sys.argv[1:]:
    for i, (name, _, pattern) in enumerate(muts):
        print(f"[{i}] {name}    -run {pattern}")
    sys.exit(0)

show = '--show' in sys.argv[1:]
only = [a for a in sys.argv[1:] if a != '--show']

base = subprocess.run([GO, "test", "./pkg/pipefix/", "-count=1"], capture_output=True, text=True)
if base.returncode != 0:
    print("the UNMUTATED package is not green; nothing was mutated:\n" + base.stdout + base.stderr)
    sys.exit(2)

res = []
for i, (name, edits, pattern) in enumerate(muts):
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
        r = subprocess.run([GO, "test", "./pkg/pipefix/", "-run", pattern, "-count=1"],
                           capture_output=True, text=True)
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
