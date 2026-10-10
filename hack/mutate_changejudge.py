#!/usr/bin/env python3
"""mutate_changejudge.py -- "seen red" evidence for gonk-cuwn part CH-JUDGE (pkg/changejudge).

Disables one predicate at a time in pkg/changejudge, runs ONLY the test that
guards it (each mutation names its own -run pattern, so a kill means that
predicate's own fixture went red, not that some unrelated test noticed), and
requires it to FAIL; then restores every file byte for byte (try/finally). A
mutation whose text is not found EXACTLY ONCE is VACUOUS, one the named test
does not notice SURVIVED, and one that does not compile NOCOMPILE -- each is a
failure of this script. Before anything is mutated the unmutated package must
be green, or a kill would mean nothing.

The rows are those of docs/reference/gonk-change-rules.md: I1-I8 with the
owner's trigger decision, K0-K5 and the outcome table (once sections 2.1, 8.1
and 2.5 of the epic plan). The four tests section 6.2 names for this
part each have a NAMED break, marked [NAMED] in the list:
  a retried-green job yields GREEN     "only the latest attempt of a job is judged"
  an unreadable report yields a verdict "an unread report is read as an empty one"
                                        "UNKNOWN is not held back before the deadline"
  TESTS_REDUCED yields GREEN           "the subset check is skipped"
  a red base dispatches                "the base pipeline's status is not checked"

Usage: hack/mutate_changejudge.py            (all mutations)
       hack/mutate_changejudge.py 3 7        (only these indexes)
       hack/mutate_changejudge.py --list     (names and indexes, nothing run)
       hack/mutate_changejudge.py --show 3   (also print the full `go test` output)
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
P = 'pkg/changejudge/'
E = P + 'eligible.go'
J = P + 'judge.go'
R = P + 'report.go'
O = P + 'outcome.go'
ET = P + 'eligible_test.go'
JT = P + 'judge_test.go'
# The rules page: the tables the plan-row tests read (they were the plan's own
# sections until they moved to the reference; the mutations below still say
# "plan:" in their names).
PLAN = 'docs/reference/gonk-change-rules.md'


def run(test, *subs):
    """-run pattern: the test, then each subtest level, anchored and escaped
    the way `go test` names subtests (spaces become underscores)."""
    parts = ['^%s$' % test]
    for s in subs:
        parts.append('^%s$' % re.escape(s.replace(' ', '_')))
    return '/'.join(parts)


ROWS = 'TestEligibleRows'
UNREAD = 'TestAnUnreadFactIsNeverEligibility'
FORMS = 'TestTheThreeTriggerFormsFromAMaintainerOrOwner'
SEVERAL = 'TestSeveralRequests'
REDBASE = 'TestARedBaseIsNotDispatched'
FIRST = 'TestTheFirstFailingRowIsTheReason'
BASELINE = 'TestTheBaselineChangeIsGreen'
REC = 'TestEveryRecordedFactIsRequired'
K0 = 'TestK0Identity'
K1 = 'TestK1Settle'
K2 = 'TestK2Definition'
K2D = 'TestK2DetailsNameTheFinding'
K3 = 'TestK3Jobs'
K4 = 'TestK4Tests'
K5 = 'TestK5FirstMatchOrder'
RETRY = 'TestARetriedGreenJobIsNotGreen'
NOREPORT = 'TestAnUnreadableReportIsNotAVerdict'
REDUCED = 'TestTestsReducedIsNotGreen'
FOUND = 'TestTheFindingsNameWhatTheyFound'
LAST = 'TestOnlyAnObservedOrInapplicableFindingIsGreen'
V2S = 'TestVerdictToStateToMeterOutcome'
STATES = 'TestStateOutcomes'
OBSLABEL = 'TestTheTestsObservedLabelNeedsObservedTests'
PLANT = 'TestEveryPlanRowHasAFixture'
EARLY = 'before the deadline'

# (name, [(file, old, new), ...], -run pattern of the guarding test)
muts = []


def m(name, edits, pattern):
    muts.append((name, edits, pattern))


def off(src, line):
    """Disable an `if <cond> {` line: the condition becomes false. The
    original condition stays in the text (unevaluated), so the mutant still
    compiles when it holds the only use of a variable or an import."""
    assert line.lstrip('\t').startswith('if ') and line.endswith(' {'), line
    indent = line[:len(line) - len(line.lstrip('\t'))]
    cond = line.lstrip('\t')[3:-2]
    assert ';' not in cond, line
    return (src, line + '\n', indent + 'if false && (' + cond + ') {\n')


def drop(src, text):
    return (src, text, '')


# =========================== Eligible: section 2.1 ===========================
m("I1: an unloaded config is not checked", [off(E, "\tif !f.Config.Loaded {")], run(UNREAD, "the config was not loaded"))
m("I1: an unloaded config is not checked (the zero value)", [off(E, "\tif !f.Config.Loaded {")], run(UNREAD, "the zero value"))
m("I1: actions.features is not required", [off(E, "\tif !f.Config.Features {")], run(ROWS, "I1 actions.features is off"))
m("an unread fact is a decision, not Unknown", [(E, "Reason{Code: code, Text: text, Unknown: true}", "Reason{Code: code, Text: text}")],
  run(UNREAD, "the config was not loaded"))
m("a noted row is silent", [(E, "Reason{Code: code, Text: text, Note: true}", "Reason{Code: code, Text: text}")], run(ROWS, "I3 the issue is confidential"))

m("I2: unread label events and notes are not checked", [off(E, "\tif !f.Trigger.Read {")], run(UNREAD, "the label events and notes were not read"))
m("I2: nobody asking is a request",
  [off(E, "\tif len(f.Trigger.Requests) == 0 {"), (E, "\trequested := false\n", "\trequested := len(f.Trigger.Requests) == 0\n")],
  run(ROWS, "I2 nobody asked"))
m("I2: any request at all is enough (who asked is not checked)", [(E, "\trequested := false\n", "\trequested := true\n")],
  run(ROWS, "I2 the label was added by a developer"))
m("I2: the form is not checked", [off(E, "\tif !knownForm(rq.Form) {")], run(ROWS, "I2 the triage verdict is not a request form"))
m("I2: the triage verdict is a request form (8.1 Q2 reversed)",
  [(E, "return []Form{FormLabel, FormCommand, FormMention} }", 'return []Form{FormLabel, FormCommand, FormMention, "triage-verdict"} }')],
  run(ROWS, "I2 the triage verdict is not a request form"))
m("I2: a request with no form is accepted", [off(E, "\tif !knownForm(rq.Form) {")], run(ROWS, "I2 a request with no form"))
m("I2: a label that was removed is still a request", [off(E, "\tif rq.Form == FormLabel && !rq.LabelOnIssue {")],
  run(ROWS, "I2 the label was added by a maintainer and has since been removed"))
ACTOR = "\tif !rq.Actor.Identified || rq.Actor.UserID == 0 {"
m("I2: the label alone is a request (no event read)", [off(E, ACTOR), off(E, "\tif !rq.Actor.AccessRead {"), off(E, "\tif rq.Actor.AccessLevel < MinRequesterAccess {")],
  run(UNREAD, "the label is on the issue and no event was read (the label alone)"))
m("I2: an unidentified actor is not checked (a user id is enough)", [(E, ACTOR, "\tif rq.Actor.UserID == 0 {")],
  run(UNREAD, "no event was read, though a user and an access level are filled in"))
m("I2: an actor without a user id is accepted", [(E, ACTOR, "\tif !rq.Actor.Identified {")], run(UNREAD, "the event names no user"))
m("I2: the bot's own label is a request", [off(E, "\tif rq.Actor.IsBot {")], run(ROWS, "I2 the label was added by the bot"))
m("I2: the bot's own command is a request", [off(E, "\tif rq.Actor.IsBot {")], run(ROWS, "I2 a slash command by the bot"))
m("I2: an unread access level is not checked", [off(E, "\tif !rq.Actor.AccessRead {")], run(UNREAD, "the actor's access was not read"))
LEVEL = "\tif rq.Actor.AccessLevel < MinRequesterAccess {"
m("I2: the actor's access level is not checked (label)", [off(E, LEVEL)], run(ROWS, "I2 the label was added by somebody who is not a member"))
m("I2: the actor's access level is not checked (command)", [off(E, LEVEL)], run(ROWS, "I2 a slash command by a developer"))
m("I2: the actor's access level is not checked (mention)", [off(E, LEVEL)], run(ROWS, "I2 an at-mention by somebody who is not a member"))
m("I2: a Developer may request (the plan's 2.1 floor, not the owner's)", [(E, "const MinRequesterAccess = 40", "const MinRequesterAccess = 30")],
  run(ROWS, "I2 the label was added by a developer"))
m("I2: a Developer may request, per form", [(E, "const MinRequesterAccess = 40", "const MinRequesterAccess = 30")], run(FORMS, "mention", "developer"))
m("I2: one below Maintainer may request", [(E, "const MinRequesterAccess = 40", "const MinRequesterAccess = 39")], run(FORMS, "command", "one below maintainer"))
m("I2: a Maintainer may not request (only an Owner)", [(E, LEVEL, "\tif rq.Actor.AccessLevel <= MinRequesterAccess {")], run(FORMS, "label", "maintainer"))
m("I2: a refused actor is not told", [(E, "Reason{Code: I2ActorCannotRequest, Note: true,", "Reason{Code: I2ActorCannotRequest,")],
  run(ROWS, "I2 the label was added by a developer"))
m("I2: an unread request does not outrank a refused one (a note is written while a valid request may exist)",
  [off(E, "\tif !requested && unknown != nil {")], run(SEVERAL, "a stranger's label and an unread mention: nothing is decided yet"))
m("I2: the LAST refused request is the reason", [(E, "\t\tcase !why.Unknown && refused == nil:", "\t\tcase !why.Unknown:")],
  run(SEVERAL, "a stranger's label and the bot's: the first is the reason"))
m("I2: EVERY request must be valid", [(E, "\tif !requested {\n\t\treturn false, *refused", "\tif !requested || refused != nil {\n\t\treturn false, *refused")],
  run(SEVERAL, "a stranger's label, then a maintainer's command"))

ISSUE = '\tif !f.Issue.Read || f.Issue.State == "" {'
m("I3: an unread issue is not checked", [(E, ISSUE, '\tif f.Issue.State == "" {')], run(UNREAD, "the issue was not read"))
m("I3: an issue without a state is decided on", [(E, ISSUE, "\tif !f.Issue.Read {")], run(UNREAD, "the issue has no state"))
m("I3: a closed issue is implemented", [off(E, "\tif f.Issue.State != IssueStateOpened {")], run(ROWS, "I3 the issue is closed"))
m("I3: a confidential issue is implemented", [off(E, "\tif f.Issue.Confidential {")], run(ROWS, "I3 the issue is confidential"))
m("I3: the bot's own issue is implemented", [off(E, "\tif f.Issue.AuthorIsBot {")], run(ROWS, "I3 the bot filed the issue"))

BASE = '\tif !f.Base.Read || f.Base.SHA == "" {'
m("I4: an unread base is not checked", [(E, BASE, '\tif f.Base.SHA == "" {')], run(UNREAD, "the base was not read"))
m("I4: an empty base SHA is decided on", [(E, BASE, "\tif !f.Base.Read {")], run(UNREAD, "the base SHA is empty"))
BP = '\t\tif p.ID == 0 || p.SHA == "" || p.Source == "" || p.Status == "" {'
m("I4: a base pipeline without an id is accepted", [(E, BP, '\t\tif p.SHA == "" || p.Source == "" || p.Status == "" {')], run(UNREAD, "a base pipeline has no id"))
m("I4: a base pipeline without a SHA is another commit's", [(E, BP, '\t\tif p.ID == 0 || p.Source == "" || p.Status == "" {')], run(UNREAD, "a base pipeline has no SHA"))
m("I4: a base pipeline without a source is accepted", [(E, BP, '\t\tif p.ID == 0 || p.SHA == "" || p.Status == "" {')], run(UNREAD, "a base pipeline has no source"))
m("I4: a base pipeline without a status is decided on", [(E, BP, '\t\tif p.ID == 0 || p.SHA == "" || p.Source == "" {')], run(UNREAD, "a base pipeline has no status"))
m("I4: a pipeline of an older commit is the base's", [off(E, "\t\tif p.SHA != f.Base.SHA {")], run(REDBASE, "a green pipeline of an OLDER commit is not the head's"))
m("I4: a child pipeline is the base's", [off(E, "\t\tif p.Source == SourceParentPipeline {")], run(ROWS, "I4 the head's only pipeline is a child pipeline"))
m("I4: a base with no pipeline is dispatched", [off(E, "\tif len(base) == 0 {")], run(ROWS, "I4 the default branch has no pipeline at all"))
m("I4: no pipeline is not `no-verifier-path`", [(E, "Code: I4NoPipeline, Note: true, NoVerifierPath: true,", "Code: I4NoPipeline, Note: true,")],
  run(ROWS, "I4 the only pipelines are of older commits"))
m("I4: a running base is called red", [off(E, "\t\tif !settledStatus(p.Status) {")], run(ROWS, "I4 the base pipeline is still running"))
RED = "\t\tif p.Status != StatusSuccess {"
m("I4 [NAMED]: the base pipeline's status is not checked -- a red base dispatches (failed)", [off(E, RED)], run(REDBASE, "failed"))
m("I4 [NAMED]: the base pipeline's status is not checked -- a red base dispatches (canceled)", [off(E, RED)], run(REDBASE, "canceled"))
m("I4: only a `failed` base is red (canceled, skipped, manual dispatch)", [(E, RED, "\t\tif p.Status == StatusFailed {")], run(REDBASE, "manual"))
m("I4: only the first listed pipeline of the head is checked",
  [(E, "\tfor _, p := range base {\n" + RED, "\tfor _, p := range base[:1] {\n" + RED)],
  run(REDBASE, "the newest pipeline of the head is green and an older one of the same commit is red"))

DEDUP = '\tif !f.Dedup.Read || (f.Dedup.ChangeState != "" && !KnownState(f.Dedup.ChangeState)) {'
m("I5: an unread record is not checked", [(E, DEDUP, '\tif f.Dedup.ChangeState != "" && !KnownState(f.Dedup.ChangeState) {')],
  run(UNREAD, "the change record and MRs were not read"))
m("I5: a state nobody defined is decided on", [(E, DEDUP, "\tif !f.Dedup.Read {")], run(UNREAD, "the change record holds a state nobody defined"))
m("I5: a closed-unmerged record is silent", [off(E, "\tif f.Dedup.ChangeState == StateClosedUnmerged {")],
  run(ROWS, "I5 the record says a maintainer closed the change MR"))
m("I5: a terminal record is called in flight", [off(E, '\tif f.Dedup.ChangeState != "" && f.Dedup.ChangeState.Terminal() {')],
  run(ROWS, "I5 a change for this issue ended verified"))
m("I5: a change in flight is dispatched again", [off(E, '\tif f.Dedup.ChangeState != "" {')], run(ROWS, "I5 a change for this issue is in flight"))
m("I5: a not-verified change is dispatched again from outside", [off(E, '\tif f.Dedup.ChangeState != "" {')],
  run(ROWS, "I5 a change for this issue was not verified and waits for its next attempt"))
m("I5: a closed-unmerged MR is proposed again", [off(E, "\tif f.Dedup.ClosedUnmergedAnchorMRs != 0 {")], run(ROWS, "I5 a closed, unmerged MR carries this anchor"))
m("I5: a second MR is opened beside an open one", [off(E, "\tif f.Dedup.OpenAnchorMRs != 0 {")], run(ROWS, "I5 an open MR carries this anchor"))
m("I5: a merged change is proposed again", [off(E, "\tif f.Dedup.MergedAnchorMRs != 0 {")], run(ROWS, "I5 a merged MR carries this anchor"))

m("I6: unread open MRs are not checked", [off(E, "\tif !f.Serial.Read {")], run(UNREAD, "the open change MRs were not read"))
SERIAL = "\tif f.Serial.OpenChangeMRs >= f.Config.MaxOpen {"
m("I6: max_open is not checked", [off(E, SERIAL)], run(ROWS, "I6 max_open change MRs are open"))
m("I6: max_open is exceeded by one", [(E, SERIAL, "\tif f.Serial.OpenChangeMRs > f.Config.MaxOpen {")], run(ROWS, "I6 max_open change MRs are open"))
m("I6: max_open 0 means one", [(E, SERIAL, "\tif f.Serial.OpenChangeMRs > f.Config.MaxOpen {")], run(ROWS, "I6 max_open is zero"))

m("I7: an unread pinned config is not checked", [off(E, "\tif !f.Pinned.Read {")], run(UNREAD, "the pinned config was not read"))
m("I7: an invalid pinned config is dispatched on", [off(E, "\tif !f.Pinned.ConfigValid {")], run(ROWS, "I7 the config at the pinned base is not valid"))
m("I7: a failed scope fold is dispatched on", [off(E, "\tif !f.Pinned.ScopeFolded {")], run(ROWS, "I7 the scope fold failed"))
m("I7: an invalid config is not OURS", [(E, "Reason{Code: I7ConfigInvalid, Ours: true,", "Reason{Code: I7ConfigInvalid,")],
  run(ROWS, "I7 the config at the pinned base is not valid"))
m("I7: a failed fold is not OURS", [(E, "Reason{Code: I7ScopeFoldFailed, Ours: true,", "Reason{Code: I7ScopeFoldFailed,")], run(ROWS, "I7 the scope fold failed"))

m("I8: require_test_report is ignored", [off(E, "\tif f.Config.RequireTestReport {")], run(ROWS, "I8 a test report is required and the base published none"))
m("I8: a test report is always required", [(E, "\tif f.Config.RequireTestReport {", "\tif true {")], run('TestTheBaseReportIsOnlyRequiredWhenConfigured'))
m("I8: an unread base report is `no report`", [off(E, "\t\tif !f.BaseReport.wellFormed() {")], run(UNREAD, "a report is required and the base report was not read"))
m("I8: a base with no report is dispatched", [off(E, "\t\tif f.BaseReport.Total == 0 {")], run(ROWS, "I8 a test report is required and the base published none"))
m("I8: no report is not `no-verifier-path`", [(E, "Code: I8NoTestReport, Note: true, NoVerifierPath: true,", "Code: I8NoTestReport, Note: true,")],
  run(ROWS, "I8 a test report is required and the base published none"))


def swap_rows(a):
    """Swap two adjacent rows of Eligible, found by their comments."""
    src = open(E).read()
    marks = ["\t// I1: opted in.\n", "\t// I2: a request by somebody who may make it.\n", "\t// I3: an issue an MR may be opened for.\n",
             "\t// I4: step 0 -- the base is green.\n", "\t// I5: dedup.\n", "\t// I6: serial.\n",
             "\t// I7: the configuration at the pinned base. OURS.\n", "\t// I8: a test report, where the project requires one.\n",
             "\treturn true, Reason{}\n"]
    pos = [src.find(x) for x in marks]
    if min(pos) < 0 or pos != sorted(pos):
        return (E, "\x00 the rows' comments were not found \x00", "")
    first, second = src[pos[a]:pos[a + 1]], src[pos[a + 1]:pos[a + 2]]
    return (E, first + second, second + first)


for a in range(7):
    m("order: I%d is evaluated before I%d" % (a + 2, a + 1), [swap_rows(a)], run(FIRST))

# ============================ Judge: section 2.5 ============================
# ---- the recorded facts
m("facts: no clock is accepted", [drop(J, '\tmiss(now.IsZero(), "now: no time was given")\n')], run(REC, "no clock"))
m("facts: no record is accepted", [off(J, "\tif !f.Change.Recorded {")], run(REC, "nothing recorded", EARLY))
for field, sub in (('ch.BaseSHA == ""', "no base SHA"), ('ch.ChangeCommitSHA == ""', "no change commit"),
                   ("ch.BasePipelineID == 0", "no base pipeline"), ("ch.MRIID == 0", "no MR iid"),
                   ("len(ch.DiffClasses) == 0", "no diff classes"), ("!ch.BaseReport.wellFormed()", "the base report was not recorded"),
                   ("len(ch.AddedGoTests) > 0 && !ch.GoProfile", "added Go tests without the go profile"),
                   ("ch.VerifyDeadline.IsZero()", "no verify deadline")):
    m("facts: `%s` is not required" % field, [(J, "\tmiss(" + field + ", ", "\tmiss(false && " + field + ", ")], run(REC, sub, EARLY))
m("facts: a path without a class is accepted", [(J, '\t\tmiss(c == "", ', '\t\tmiss(false && c == "", ')], run(REC, "a path without a class", EARLY))
m("facts: an added Go test without a name is accepted", [(J, '\t\tmiss(g.Name == "", ', '\t\tmiss(false && g.Name == "", ')],
  run(REC, "an added Go test without a name", EARLY))
m("facts: an unread MR is judged", [off(J, "\tif !f.MR.Read {")], run(K0, "the MR was not read"))
m("facts: another MR is judged", [(J, "\tmiss(f.MR.IID != ch.MRIID, ", "\tmiss(false && f.MR.IID != ch.MRIID, ")], run(K0, "another MR was read"))
m("facts: an MR without a head SHA is judged", [(J, '\tmiss(f.MR.SHA == "", ', '\tmiss(false && f.MR.SHA == "", ')], run(K0, "the MR has no head SHA"))
m("report [NAMED]: an unread report is read as an empty one (the Read marker is not checked)", [off(R, "\tif !r.Read {")],
  run(NOREPORT, "would be GREEN", "never read"))
m("report [NAMED]: an unread report is read as an empty one (GREEN_UNOBSERVED)", [off(R, "\tif !r.Read {")],
  run(NOREPORT, "would be GREEN_UNOBSERVED", "never read"))
m("report: cases that do not add up to the total are a report", [off(R, "\tif len(r.Cases) != r.Total {")],
  run(REC, "the base report's cases do not add up to its total", EARLY))
m("report: a truncated head report is a report", [off(R, "\tif len(r.Cases) != r.Total {")], run(NOREPORT, "would be GREEN", "fewer cases than the total"))
m("report: a case without a name is a case", [off(R, '\t\tif c.Name == "" {')], run(REC, "a base case has no name", EARLY))
m("report: a status nobody defined is a status", [(R, "\t\tdefault:\n\t\t\treturn false\n", "\t\tdefault:\n")],
  run(REC, "a base case has a status nobody defined", EARLY))
m("report: ReportOf(nil) is a read report", [(R, "\tif tr == nil {\n\t\treturn Report{}", "\tif tr == nil {\n\t\treturn Report{Read: true}")], run('TestReportOf'))
m("report: ReportOf drops the suite name", [(R, "Case{Suite: s.Name, Classname:", "Case{Classname:")], run('TestReportOf'))

# ---- K0
m("K0: merged is not `merged`", [(J, "\tcase f.MR.State == MRStateMerged:", "\tcase false:")], run(K0, "the MR was merged"))
m("K0: closed is not `closed-unmerged`", [(J, "\tcase f.MR.State == MRStateClosed:", "\tcase false:")], run(K0, "the MR was closed"))
m("K0: a state K0 does not name is judged", [(J, "\tcase f.MR.State != MRStateOpened:", "\tcase false:")], run(K0, "a state GitLab has and K0 does not name (locked)"))
m("K0: somebody else's commit is judged", [(J, "\tcase f.MR.SHA != f.Change.ChangeCommitSHA:", "\tcase false:")], run(K0, "somebody else pushed"))
m("K0: taken-over outranks merged",
  [(J, "\tcase f.MR.State == MRStateMerged:", "\tcase f.MR.State == MRStateMerged && f.MR.SHA == f.Change.ChangeCommitSHA:")],
  run(K0, "merged after somebody else pushed: merged"))
m("K0: taken-over outranks closed",
  [(J, "\tcase f.MR.State == MRStateClosed:", "\tcase f.MR.State == MRStateClosed && f.MR.SHA == f.Change.ChangeCommitSHA:")],
  run(K0, "closed after somebody else pushed: closed"))
m("K0: merged selects the state closed-unmerged", [(J, "State: StateMerged, Rule: RuleK0Merged", "State: StateClosedUnmerged, Rule: RuleK0Merged")],
  run(K0, "the MR was merged"))
m("K0: taken-over selects the state verifying", [(J, "State: StateTakenOver, Rule: RuleK0TakenOver", "State: StateVerifying, Rule: RuleK0TakenOver")],
  run(K0, "somebody else pushed"))

# ---- K1
m("K1: unread pipelines are judged", [off(J, "\tif !f.Pipelines.Read {")], run(K1, "the pipelines were not read"))
m("K1: pipelines unread past the deadline are not a cause", [drop(J, "\t\tunverifiable = append(unverifiable, cause{RuleK1Unread, \"the merge request's pipelines could not be read before the verify deadline\"})\n")],
  run(K1, "the pipelines stayed unread past the deadline"))
JP = '\t\t\tif p.ID == 0 || p.SHA == "" || p.Source == "" || p.Status == "" {'
m("K1: a pipeline without an id is judged", [(J, JP, '\t\t\tif p.SHA == "" || p.Source == "" || p.Status == "" {')], run(K1, "a pipeline without an id"))
m("K1: a pipeline without a SHA is another commit's", [(J, JP, '\t\t\tif p.ID == 0 || p.Source == "" || p.Status == "" {')],
  run(K1, "a pipeline without a SHA is not a pipeline of another commit"))
m("K1: a pipeline without a source is judged", [(J, JP, '\t\t\tif p.ID == 0 || p.SHA == "" || p.Status == "" {')], run(K1, "a pipeline without a source, past the deadline"))
m("K1: a pipeline without a status is waited for", [(J, JP, '\t\t\tif p.ID == 0 || p.SHA == "" || p.Source == "" {')], run(K1, "a pipeline without a status"))
m("K1: a pipeline of another commit is a candidate", [off(J, "\t\t\tif p.SHA != f.Change.ChangeCommitSHA {")], run(K1, "a failed pipeline of ANOTHER commit is not a candidate"))
m("K1: only pipelines of another commit: judged, not waited for", [off(J, "\t\t\tif p.SHA != f.Change.ChangeCommitSHA {")], run(K1, "the only pipelines are of another commit"))
m("K1: a child pipeline is a candidate", [off(J, "\t\t\tif p.Source == SourceParentPipeline {")], run(K1, "a failed CHILD pipeline is not a candidate"))
m("K1: the candidates are not sorted", [drop(J, "\t\tsort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })\n")],
  run('TestCandidatesAreSortedAndOnlyTheCommits'))
m("K1: no candidate at all is settled", [(J, "\t\tsettled := len(candidates) > 0\n", "\t\tsettled := true\n")], run(K1, "no pipeline of the MR yet"))
m("K1: a running pipeline is settled", [off(J, "\t\t\tif !settledStatus(c.Status) {")], run(K1, "the pipeline is running"))
m("K1: one finished candidate is enough", [(J, "\t\t\tif !settledStatus(c.Status) {\n\t\t\t\tsettled = false", "\t\t\tif !settledStatus(c.Status) && c.ID == candidates[0].ID {\n\t\t\t\tsettled = false")],
  run(K1, "one candidate finished, one is running"))
m("K1: a running pipeline with a failed job is judged before it finishes", [off(J, "\t\t\tif !settledStatus(c.Status) {")],
  run(K1, "a job already failed and the pipeline is still running: no verdict on a pipeline that has not finished"))
m("K1: the deadline is ignored (an unsettled pipeline is a timeout at once)", [(J, "\t\tif !settled && !past {", "\t\tif !settled && false {")], run(K1, "the pipeline is pending"))
m("K1: the wait never ends", [(J, "\t\tif !settled && !past {", "\t\tif !settled {")], run(K1, "still running at the deadline"))
m("K1: a timeout is not a cause", [(J, "\t\tif !settled {\n\t\t\tunverifiable = append(", "\t\tif false {\n\t\t\tunverifiable = append(")], run(K1, "no pipeline at the deadline"))
m("K1: the deadline itself is before the deadline", [(J, "\tpast := !now.Before(f.Change.VerifyDeadline)", "\tpast := now.After(f.Change.VerifyDeadline)")],
  run('TestTheDeadlineItselfIsPastTheDeadline'))
SET = "\tcase StatusSuccess, StatusFailed, StatusCanceled, StatusSkipped, StatusManual:"
for st, sub in ((", StatusManual", "the pipeline is blocked on a manual job"), (" StatusCanceled,", "the pipeline was canceled and nothing failed"),
                (" StatusSkipped,", "the pipeline was skipped"), (" StatusFailed,", "the pipeline failed and no job did")):
    m("K1: `%s` is not a finished pipeline" % st.strip(", "), [(J, SET, SET.replace(st, "", 1))], run(K3, sub))

# ---- K2
DEF = "\tif !f.Definition.Read || !knownKind(f.Definition.Delta.Kind) {"
m("K2: an uncomputed delta is compared", [(J, DEF, "\tif !knownKind(f.Definition.Delta.Kind) {")], run(K2, "the delta was not computed"))
m("K2: the zero Kind is a change", [(J, DEF, "\tif !f.Definition.Read {")], run(K2, "a delta nobody computed (the zero Kind)"))
m("K2: an unread delta past the deadline is not a cause", [drop(J, '\t\tunverifiable = append(unverifiable, cause{RuleK2Unread, "the CI definition could not be compared before the verify deadline"})\n')],
  run(K2, "the delta stayed unread past the deadline"))
m("K2: a definition finding is not a cause", [drop(J, "\t\tunverifiable = append(unverifiable, c)\n")], run(K2, "the delta is a violation"))
m("K2: Result.Definition is not recorded", [drop(J, "\t\tres.Definition = cidef.KindNone\n")], run(BASELINE, "not go"))
m("K2: a violation Kind with no list is merely a change", [off(J, "\tif d.Kind == cidef.KindViolation {")], run(K2D, "a violation Kind with nothing listed"))
m("K2: a listed violation under Kind none is not read", [off(J, "\tif len(d.Violations) > 0 {")], run(K2, "Kind says none and a violation is listed"))
m("K2: a listed violation does not outrank an unresolvable Kind", [off(J, "\tif len(d.Violations) > 0 {")], run(K2, "a violation outranks an unresolvable construct"))
m("K2: an unresolvable Kind with no list is merely a change", [off(J, "\tif d.Kind == cidef.KindUnresolvable {")], run(K2D, "an unresolvable Kind with nothing listed"))
m("K2: a listed unresolvable construct under Kind none is not read", [off(J, "\tif len(d.Unresolvable) > 0 {")],
  run(K2, "Kind says none and an unresolvable construct is listed"))
m("K2: a preserving change is accepted (there is no ci lift here)", [off(J, "\tif d.Kind != cidef.KindNone {")], run(K2, "the definition changed, execution preserved"))
m("K2: only an exec-changing change counts", [(J, "\tif d.Kind != cidef.KindNone {", "\tif d.Kind == cidef.KindExecChanging {")], run(K2, "the definition changed, execution preserved"))
m("K2: an exec-changing change is accepted", [(J, "\tif d.Kind != cidef.KindNone {", "\tif d.Kind == cidef.KindPreserving {")], run(K2, "the definition changed what a job executes"))
LISTS = "\tif len(d.Jobs)+len(d.Removed)+len(d.Added)+len(d.Renamed)+len(d.Globals) > 0 {"
for term, sub in (("len(d.Jobs)+", "a changed job"), ("len(d.Removed)+", "a removed job"), ("len(d.Added)+", "an added job"),
                  ("len(d.Renamed)+", "a rename"), ("+len(d.Globals)", "a global change")):
    m("K2: Kind none with %s listed is unchanged" % sub, [(J, LISTS, LISTS.replace(term, "", 1))], run(K2, "Kind says none and the delta lists " + sub))
KINDS = "\tcase cidef.KindNone, cidef.KindPreserving, cidef.KindExecChanging, cidef.KindViolation, cidef.KindUnresolvable:"
for kind, test in (("cidef.KindNone, ", run(BASELINE, "not go")), ("cidef.KindPreserving, ", run(K2, "the definition changed, execution preserved")),
                   ("cidef.KindExecChanging, ", run(K2, "the definition changed what a job executes")),
                   ("cidef.KindViolation, ", run(K2, "the delta is a violation")), (", cidef.KindUnresolvable", run(K2, "the delta is unresolvable"))):
    m("K2: `%s` is a Kind nobody computed" % kind.strip(", "), [(J, KINDS, KINDS.replace(kind, "", 1))], test)
m("K2: any string is a Kind", [(J, KINDS + "\n\t\treturn true\n\t}\n\treturn false", KINDS + "\n\t\treturn true\n\t}\n\treturn true")], run(K2, "a Kind nobody defined"))

# ---- K3
m("K3: unlisted jobs are judged", [off(J, "\t\tif !c.JobsRead {")], run(K3, "the jobs were not listed"))
m("K3: unlisted jobs of the second candidate are judged", [off(J, "\t\tif !c.JobsRead {")], run(K3, "the jobs of the second candidate were not listed"))
m("K3: a listing without retried attempts is accepted", [off(J, "\t\tif !c.IncludeRetried {")], run(K3, "the jobs were listed without retried attempts"))
JA = '\t\t\tif a.ID == 0 || a.Name == "" || a.Status == "" {'
m("K3: a job without an id is judged", [(J, JA, '\t\t\tif a.Name == "" || a.Status == "" {')], run(K3, "a job object without an id"))
m("K3: a job without a name is judged", [(J, JA, '\t\t\tif a.ID == 0 || a.Status == "" {')], run(K3, "a job object without a name, past the deadline"))
m("K3: a job without a status is judged", [(J, JA, '\t\t\tif a.ID == 0 || a.Name == "" {')], run(K3, "a job object without a status"))
m("K3: jobs are judged on an incomplete listing (a failure seen is RED)", [(J, "\tif complete {\n", "\tif complete || f.Pipelines.Read {\n")],
  run(K3, "listed without retried attempts past the deadline: a failure seen is not RED, a pass unseen may exist"))
m("K3: an incomplete listing past the deadline is not a cause (a green seen is GREEN)", [(J, "\t} else if f.Pipelines.Read {", "\t} else if false {")],
  run(K3, "listed without retried attempts past the deadline: a green seen is not GREEN"))
m("K3: unlisted jobs past the deadline are not a cause", [(J, "\t} else if f.Pipelines.Read {", "\t} else if false {")], run(K3, "the jobs stayed unlisted past the deadline"))
m("K3: a job that failed and never passed is not a cause unless RED",
  [(J, "\t\tif j, found := firstJob(res.Jobs, RuleK3Attempts); found {", "\t\tif j, found := firstJob(res.Jobs); found {")],
  run(K3, "a job failed, never passed, and the pipeline says success all the same"))
STATUS = "\t\tif c.Status != StatusSuccess {"
m("K3: a pipeline that is not `success` is not a cause (canceled)", [off(J, STATUS)], run(K3, "the pipeline was canceled and nothing failed"))
m("K3: a pipeline that is not `success` is not a cause (manual)", [off(J, STATUS)], run(K3, "the pipeline is blocked on a manual job"))
m("K3: only the first candidate needs to be `success`", [(J, STATUS, "\t\tif c.ID == candidates[0].ID && c.Status != StatusSuccess {")], run(K3, "one of two candidates was canceled"))
m("K3: only a `failed` pipeline is not green", [(J, STATUS, "\t\tif c.Status == StatusFailed {")], run(K3, "the pipeline was skipped"))
LATEST = "\t\tfor _, a := range c.Jobs {\n\t\t\tj, has := byName[a.Name]\n"
ONLY_LATEST = ("\t\tfor _, a := range c.Jobs {\n\t\t\tsuperseded := false\n\t\t\tfor _, b := range c.Jobs {\n"
               "\t\t\t\tsuperseded = superseded || (b.Name == a.Name && b.ID > a.ID)\n\t\t\t}\n"
               "\t\t\tif superseded {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tj, has := byName[a.Name]\n")
m("K3 [NAMED]: only the latest attempt of a job is judged -- a retried-green job yields GREEN", [(J, LATEST, ONLY_LATEST)],
  run(RETRY, "not go", "failed, retried, passed"))
m("K3 [NAMED]: only the latest attempt of a job is judged (go; listed newest first)", [(J, LATEST, ONLY_LATEST)],
  run(RETRY, "go", "the job objects are listed newest first"))
m("K3 [NAMED]: only the latest attempt of a job is judged (an allowed-to-fail job)", [(J, LATEST, ONLY_LATEST)],
  run(RETRY, "not go", "a job that is allowed to fail"))
m("K3: only the newest candidate pipeline is judged",
  [(J, "func judgeJobs(candidates []Pipeline) []JobFinding {\n", "func judgeJobs(candidates []Pipeline) []JobFinding {\n\tcandidates = candidates[len(candidates)-1:]\n")],
  run(RETRY, "not go", "the failure is in an older pipeline of the same commit"))
m("K3: a failed attempt is not counted", [off(J, "\t\t\tif a.Status == StatusFailed {")], run(K3, "a job failed on its only attempt"))
m("K3: a successful attempt is not counted", [off(J, "\t\t\tif a.Status == StatusSuccess {")], run(K3, "a job failed and was retried into green"))
m("K3: allow_failure is ignored (every job must pass)", [(J, "\t\t\tif !a.AllowFailure {", "\t\t\tif true {")],
  run(K3, "a job that is allowed to fail failed on every attempt: a finding, and the pipeline is what GitLab says"))
m("K3: no job must pass", [off(J, "\t\t\tif !a.AllowFailure {")], run(K3, "a job failed on its only attempt"))
m("K3: the LAST attempt says whether the job must pass",
  [(J, "\t\t\tif !a.AllowFailure {\n\t\t\t\trequired[a.Name] = true\n\t\t\t}\n", "\t\t\trequired[a.Name] = !a.AllowFailure\n")],
  run(K3, "one attempt was allowed to fail and another was not: the job must pass"))
m("K3: a job that never failed has a finding", [off(J, "\t\tif j.Failed == 0 {")], run(BASELINE, "go"))
m("K3: the findings are not sorted", [(J, "\tsort.Strings(names)\n\tvar out []JobFinding", "\tvar out []JobFinding")], run('TestJobFindingsAreSortedAndCounted'))
m("K3: failed-and-passed is not FLAKY", [off(J, "\tif j.Succeeded > 0 {")], run(K3, "a job passed, was re-run and failed"))
m("K3: an allowed-to-fail job retried into green is not FLAKY", [(J, "\tif j.Succeeded > 0 {", "\tif j.Succeeded > 0 && required {")],
  run(K3, "a job that is allowed to fail failed and was retried into green"))
m("K3: an allowed failure is RED", [off(J, "\tif !required {")], run(K3, "a job that is allowed to fail failed on every attempt: a finding, and the pipeline is what GitLab says"))
m("K3: failing every attempt is not RED", [off(J, "\tif j.Failed == j.Attempts {")], run(K3, "a job was retried and failed again"))
m("K3: one failure of a job that never passed is RED (a canceled retry included)", [(J, "\tif j.Failed == j.Attempts {", "\tif j.Failed > 0 {")],
  run(K3, "a job failed, was retried and the retry was canceled"))

# ---- K4
m("K4: only `source` and `test` are looked at (an unknown class skips the tests)", [(R, "\t\tif c != ClassDocs {", "\t\tif c == ClassSource || c == ClassTest {")],
  run(K4, "a class this package does not know is not a reason to skip the tests"))
m("K4: only the first path's class is looked at", [(R, "\tfor _, c := range classes {\n", "\tfor _, c := range classes[:1] {\n")], run(K4, "a docs and test diff is not docs only"))
m("K4: a docs-only diff needs its test report", [(R, "\t\tif c != ClassDocs {", '\t\tif c != "" {')], run(K4, "a docs-only diff: no test is looked for, the reports are not even read"))
m("K4: no diff is looked at", [(R, "\tif !testsApply(ch.DiffClasses) {", "\tif true {")], run(K4, "a source-only diff"))
GREENC = "\t\tif c.Status == StatusSuccess {"
m("K4: the report of a pipeline that did not run to green is evidence (a canceled pipeline escalates)", [(R, GREENC, "\t\tif true {")],
  run(K4, "go: a canceled pipeline's truncated report is not evidence that a test was not collected"))
m("K4: the report of a pipeline still running at the deadline is evidence", [(R, GREENC, "\t\tif true {")],
  run(K4, "go: a pipeline still running at the deadline, its report partial"))
m("K4: a red pipeline's report is waited for", [(R, GREENC, "\t\tif c.Status == StatusSuccess || c.Status == StatusFailed {")], run(K4, "a red pipeline's report is not read at all"))
m("K4: no green candidate still yields a test finding", [off(R, "\tif len(green) == 0 {")], run(FOUND, "a pipeline that did not run to green carries no test finding"))
UNREADR = "\tif len(unreadReports(candidates)) > 0 {"
m("K4: an unread head report is not UNKNOWN", [off(R, UNREADR)], run(K4, "the head report was not read: no verdict this pass"))
m("K4: a head report with a status nobody defined is read", [off(R, UNREADR)], run(K4, "a head case has a status nobody defined"))
m("K4: only the first green candidate's report must be read",
  [(R, "\tfor _, c := range greenCandidates(candidates) {\n", "\tfor _, c := range greenCandidates(candidates)[:1] {\n")],
  run(K4, "the report of the second green candidate was not read"))
m("K4: an unread report is not named", [off(R, "\t\tif !c.Report.wellFormed() {")], run(K4, "the head report's cases do not add up to its total"))
m("K4: an empty report is evidence against the suite", [(R, "\t\tif g.Report.Total > 0 {", "\t\tif true {")],
  run(K4, "one of two green pipelines published no report: the one that did is the evidence"))
m("K4: no report at the head is TESTS_OBSERVED", [off(R, "\tif len(heads) == 0 {")], run(K4, "no report at the head and none at the base"))
m("K4: a report that vanished is merely unobserved", [off(R, "\t\tif len(base.passed) > 0 {")], run(K4, "the base had passing cases and the head reports nothing"))
m("K4: a report that vanished is merely unobserved (the named test)", [off(R, "\t\tif len(base.passed) > 0 {")], run(REDUCED, "the whole report vanished"))
m("K4: no report at the head is always a reduced suite", [(R, "\t\tif len(base.passed) > 0 {", "\t\tif true {")],
  run(K4, "no report at the head; the base reported only a skipped case"))
LOST = "\tif len(lost) > 0 {"
m("K4 [NAMED]: the subset check is skipped -- TESTS_REDUCED yields GREEN (a pass absent)", [off(R, LOST)], run(REDUCED, "a base pass is absent"))
m("K4 [NAMED]: the subset check is skipped -- TESTS_REDUCED yields GREEN (a pass now fails)", [off(R, LOST)], run(REDUCED, "a base pass now fails"))
m("K4 [NAMED]: the subset check is skipped -- TESTS_REDUCED yields GREEN (a pass skipped)", [off(R, LOST)], run(REDUCED, "a base pass is skipped"))
m("K4: cases are a set, not a multiset (a duplicate hides a lost pass)", [(R, "\t\t\tif h.passed[id] < n {", "\t\t\tif h.passed[id] < n && h.passed[id] == 0 {")],
  run(K4, "two base passes share one identity and the head has one of them"))
m("K4: the suite must hold in the first reporting pipeline only",
  [(R, "\tfor _, h := range heads {\n\t\tfor id, n := range base.passed {", "\tfor _, h := range heads[:1] {\n\t\tfor id, n := range base.passed {")],
  run(K4, "the suite shrank in one of two reporting pipelines"))
ID = "\t\tid := caseID{c.Suite, c.Classname, c.Name}"
m("K4: the suite is not part of a case's identity", [(R, ID, '\t\tid := caseID{"", c.Classname, c.Name}')], run(K4, "a base case moved to another suite"))
m("K4: the classname is not part of a case's identity", [(R, ID, '\t\tid := caseID{c.Suite, "", c.Name}')], run(K4, "a base case moved to another classname"))
m("K4: the name is not part of a case's identity", [(R, ID, '\t\tid := caseID{c.Suite, c.Classname, ""}')], run(K4, "a base case was renamed (an unstable name reads as reduced)"))
m("K4: a skipped case counts as passed", [(R, "\t\tcase CaseSuccess:\n\t\t\tt.passed[id]++", "\t\tcase CaseSuccess, CaseSkipped:\n\t\t\tt.passed[id]++")],
  run(K4, "a base case is skipped at the head"))
m("K4: an errored case counts as passed", [(R, "\t\tcase CaseSuccess:\n\t\t\tt.passed[id]++", "\t\tcase CaseSuccess, CaseError:\n\t\t\tt.passed[id]++"),
                                           (R, "\t\tcase CaseFailed, CaseError:", "\t\tcase CaseFailed:")],
  run(K4, "a base case errors at the head"))
m("K4 go: the go profile uses the set difference", [off(R, "\tif ch.GoProfile {")], run(K4, "go: the added function has no case"))
GOP = "\t\t\t\tif h.goPasses(g) > base.goPasses(g) {"
m("K4 go: a passing case of that name is enough (not a NEW pass)", [(R, GOP, "\t\t\t\tif h.goPasses(g) > 0 {")],
  run(K4, "go: the name passed at the base already and passes no more often now"))
m("K4 go: an added function with no passing case is not reported", [off(R, "\t\t\tif !ran {")], run(K4, "go: the added function has no case"))
m("K4 go: NEW_NOT_RUN(go) is not a finding", [off(R, "\t\tif len(notRun) > 0 {")], run(K4, "go: one of two added functions has no case"))
m("K4 go: the function must have run in the LAST reporting pipeline",
  [(R, GOP + "\n\t\t\t\t\tran = true\n\t\t\t\t}\n", "\t\t\t\tran = h.goPasses(g) > base.goPasses(g)\n")],
  run(K4, "go: the added function ran in one of two reporting pipelines"))
GON = "\t\tif c.Status != CaseSuccess || c.Name != g.Name {"
m("K4 go: a skipped case of the function is the function passing", [(R, GON, "\t\tif c.Name != g.Name {")], run(K4, "go: the added function's case was skipped"))
m("K4 go: a failed case of the function is the function passing", [(R, GON, "\t\tif c.Name != g.Name {")], run(K4, "go: the added function's case failed"))
m("K4 go: a subtest passing is the function passing", [(R, GON, "\t\tif c.Status != CaseSuccess || !strings.HasPrefix(c.Name, g.Name) {")],
  run(K4, "go: only a subtest of the added function has a case"))
GOD = '\t\tif g.Dir != "" && c.Classname != g.Dir && !strings.HasSuffix(c.Classname, "/"+g.Dir) {'
m("K4 go: the package is not checked", [off(R, GOD)], run(K4, "go: a function of that name passes in ANOTHER package"))
m("K4 go: a package path that merely ends like the directory matches", [(R, GOD, GOD.replace('"/"+g.Dir', 'g.Dir'))],
  run(K4, "go: a package whose path merely ends like the directory"))
m("K4 go: a classname equal to the directory does not match", [(R, GOD, GOD.replace('c.Classname != g.Dir && ', ''))], run(K4, "go: the classname is the directory itself"))
m("K4 go: a function in the repository root matches no package", [(R, GOD, GOD.replace('g.Dir != "" && ', ''))],
  run(K4, "go: a function in the repository root matches any package"))
NEW = "\t\t\t\tif n > base.passed[id] {"
m("K4: any passing case counts as a new one", [(R, NEW, "\t\t\t\tif n > base.passed[id]-n {")], run(K4, "the head report is the base's: no new passing case"))
m("K4: no new passing case is not NEW_NOT_RUN", [off(R, "\t\tif !anyNew {")], run(K4, "the only new case was skipped"))
m("K4: without a base report nothing needs to have passed", [(R, "\t\tif !anyNew {", "\t\tif !anyNew && ch.BaseReport.Total > 0 {")],
  run(K4, "no report at the base, and the head's only case is skipped: nothing ran"))
m("K4: a second pass of a known identity is not a new pass", [(R, NEW, "\t\t\t\tif n > 0 && base.passed[id] == 0 {")],
  run(K4, "a second pass of an identity the base had once is a new pass"))
m("K4: a case newly failing on a green pipeline is TESTS_OBSERVED", [off(R, "\tif len(failing) > 0 {")],
  run(K4, "a new case passes and another new case fails, on a green pipeline"))
m("K4: an errored case is not a failing one", [(R, "\t\tcase CaseFailed, CaseError:", "\t\tcase CaseFailed:")], run(K4, "a new case errors on a green pipeline"))
m("K4: a failure the base already had is the change's", [(R, "\t\t\tif n > base.failed[id] {", "\t\t\tif n > 0 {")],
  run(K4, "a case that failed at the base still fails: not the change's"))

# ---- before the deadline; K5
m("no verdict on partial facts is not enforced (an unread delta is UNVERIFIABLE at once)", [off(J, "\tif !past && len(unread) > 0 {")], run(K2, "the delta was not computed"))
UNK = "\tif !past && res.Tests.Finding == TestsUnknown {"
m("K4 [NAMED]: UNKNOWN is not held back before the deadline -- an unreadable report yields a verdict", [off(J, UNK)],
  run(NOREPORT, "would be GREEN", "never read"))
m("K4 [NAMED]: UNKNOWN is not held back before the deadline (go)", [off(J, UNK)], run(NOREPORT, "would be GREEN (go)", "read flag without content"))
m("an unread report is not named beside another unread fact",
  [(J, "\t\tif res.Tests.Finding == TestsUnknown {\n\t\t\tunread = append(unread, unreadReports(candidates)...)", "\t\tif false {\n\t\t\tunread = append(unread, unreadReports(candidates)...)")],
  run('TestBeforeTheDeadlineEveryUnreadFactIsNamed'))
m("past the deadline an unread report keeps the rule UNKNOWN",
  [(J, "\tif res.Tests.Finding == TestsUnknown {\n\t\tres.Tests.Rule = RuleK4Unread", "\tif false {\n\t\tres.Tests.Rule = RuleK4Unread")],
  run(K4, "the head report stayed unread past the deadline"))
m("the verdict does not select its state", [drop(J, "\tres.State, _ = StateFor(v)\n")], run(BASELINE, "not go"))
m("K5 NOT_GREEN: RED is not a verdict", [(J, "\tif j, found := firstJob(jobs, RuleK3Red); found {", "\tif j, found := firstJob(jobs); found {")],
  run(K3, "a job failed on its only attempt"))
m("K5 TESTS_NOT_RUN: NEW_NOT_RUN is not a verdict", [off(J, "\tif tests.Finding == TestsNewNotRun {")], run(K4, "the head report is the base's: no new passing case"))
m("K5 FLAKY: FLAKY is not a verdict (a retried job is green)", [(J, "\tif j, found := firstJob(jobs, RuleK3Flaky); found {", "\tif j, found := firstJob(jobs); found {")],
  run(K3, "a job failed and was retried into green"))
m("K5 UNVERIFIABLE: a K1-K3 cause is not a verdict", [off(J, "\tif len(unverifiable) > 0 {")], run(K2, "the definition changed what a job executes"))
m("K5 UNVERIFIABLE: the LAST cause is named", [(J, "unverifiable[0].rule, unverifiable[0].detail", "unverifiable[len(unverifiable)-1].rule, unverifiable[len(unverifiable)-1].detail")],
  run(K5, "the definition (K2) is named before a pipeline status (K3)"))
m("K5 UNVERIFIABLE: a job's attempts are named after the pipeline's status",
  [(J, "unverifiable[0].rule, unverifiable[0].detail", "unverifiable[len(unverifiable)-1].rule, unverifiable[len(unverifiable)-1].detail")],
  run(K5, "a job's attempts (K3) are named before the pipeline's status (K3)"))
m("K5 UNVERIFIABLE: a report unread past the deadline has no rung", [off(J, "\tif tests.Finding == TestsUnknown {")],
  run(K4, "the head report stayed unread past the deadline"))
REDUCEDRUNG = "\tif tests.Finding == TestsReduced {"
OBSERVED = "\tif tests.Finding == TestsObserved {"
m("K5: TESTS_REDUCED has no rung", [off(J, REDUCEDRUNG)], run(K4, "a base case is gone from the head report"))
m("K5: TESTS_REDUCED is a finding GREEN accepts", [off(J, REDUCEDRUNG), (J, OBSERVED, "\tif tests.Finding == TestsObserved || tests.Finding == TestsReduced {")],
  run(REDUCED, "a base pass now fails"))
m("K5: a vanished report is described as a lost case", [off(J, "\t\tif tests.Rule == RuleK4Vanished {")], run(REDUCED, "the whole report vanished"))
m("K5: TESTS_FAILED has no rung", [off(J, "\tif tests.Finding == TestsFailed {")], run(K4, "a new case errors on a green pipeline"))
m("K5: TESTS_FAILED is a finding GREEN accepts",
  [off(J, "\tif tests.Finding == TestsFailed {"), (J, OBSERVED, "\tif tests.Finding == TestsObserved || tests.Finding == TestsFailed {")],
  run(K4, "no report at the base and a failing case at the head"))
m("K5: TESTS_UNOBSERVED has no rung", [off(J, "\tif tests.Finding == TestsUnobserved {")], run(K4, "no report at the head and none at the base"))
m("K5: TESTS_UNOBSERVED is GREEN (tests observed)",
  [off(J, "\tif tests.Finding == TestsUnobserved {"), (J, OBSERVED, "\tif tests.Finding == TestsObserved || tests.Finding == TestsUnobserved {")],
  run(K4, "no report at the head and none at the base"))
m("K5: require_test_report is ignored", [off(J, "\t\tif requireReport {")], run(K4, "no report anywhere, and the project requires one"))
m("K5: a report is always required", [(J, "\t\tif requireReport {", "\t\tif true {")], run(K4, "no report at the head and none at the base"))
m("K5: TESTS_OBSERVED is not GREEN", [off(J, OBSERVED)], run(BASELINE, "go"))
m("K5: a docs-only diff is not GREEN", [off(J, "\tif tests.Finding == TestsNotApplicable {")],
  run(K4, "a docs-only diff: no test is looked for, the reports are not even read"))
m("K5: a finding with no rung is GREEN",
  [(J, "\treturn VerdictUnverifiable, RuleK5Unexplained, ", "\treturn VerdictGreen, RuleK5Green, ")], run(LAST))


def move_rung(a, b):
    """Move rung `a` of the K5 ladder to just after rung `b` (a < b), found by
    their comments. Adjacent a, b is a swap."""
    src = open(J).read()
    marks = ["\t// NOT_GREEN: any RED.\n", "\t// TESTS_NOT_RUN: NEW_NOT_RUN.\n", "\t// FLAKY: any FLAKY.\n",
             "\t// UNVERIFIABLE: from K1-K3.\n", "\t// UNVERIFIABLE: a report that stayed unread past the deadline.\n",
             "\t// UNVERIFIABLE(tests-reduced): TESTS_REDUCED.\n", "\t// UNVERIFIABLE: a case newly failing on a green pipeline.\n",
             "\t// GREEN_UNOBSERVED: TESTS_UNOBSERVED", "\t// GREEN: only on a finding GREEN accepts."]
    pos = [src.find(x) for x in marks]
    if min(pos) < 0 or pos != sorted(pos):
        return (J, "\x00 the K5 ladder's comments were not found \x00", "")
    moved, rest = src[pos[a]:pos[a + 1]], src[pos[a + 1]:pos[b + 1]]
    return (J, moved + rest, rest + moved)


for (a, b, what, sub) in ((0, 1, "TESTS_NOT_RUN is matched before NOT_GREEN", "NOT_GREEN over TESTS_NOT_RUN"),
                          (0, 2, "FLAKY is matched before NOT_GREEN", "NOT_GREEN over FLAKY"),
                          (0, 3, "UNVERIFIABLE is matched before NOT_GREEN", "NOT_GREEN over UNVERIFIABLE (the definition)"),
                          (0, 3, "past the deadline the timeout is matched before a visible RED", "past the deadline, a visible RED outranks the timeout"),
                          (1, 2, "FLAKY is matched before TESTS_NOT_RUN", "TESTS_NOT_RUN over FLAKY"),
                          (1, 3, "UNVERIFIABLE is matched before TESTS_NOT_RUN", "TESTS_NOT_RUN over UNVERIFIABLE (the definition)"),
                          (2, 3, "UNVERIFIABLE is matched before FLAKY", "FLAKY over UNVERIFIABLE (the definition)"),
                          (2, 5, "TESTS_REDUCED is matched before FLAKY", "FLAKY over UNVERIFIABLE (tests reduced)"),
                          (2, 7, "GREEN_UNOBSERVED is matched before FLAKY", "FLAKY over GREEN_UNOBSERVED"),
                          (3, 4, "an unread report is matched before a K1-K3 cause", "past the deadline, an unread definition is named before an unread report"),
                          (3, 5, "TESTS_REDUCED is matched before a K1-K3 cause", "UNVERIFIABLE (the definition) over UNVERIFIABLE (tests reduced)"),
                          (3, 6, "TESTS_FAILED is matched before a K1-K3 cause", "UNVERIFIABLE (the definition) over UNVERIFIABLE (a case newly fails)"),
                          (3, 7, "GREEN_UNOBSERVED is matched before a K1-K3 cause", "UNVERIFIABLE (the definition) over GREEN_UNOBSERVED")):
    m("K5 order: " + what, [move_rung(a, b)], run(K5, sub))
m("K5 order: GREEN on a docs-only diff is matched before a K1-K3 cause",
  [(J, "\tif len(unverifiable) > 0 {", "\tif len(unverifiable) > 0 && tests.Finding != TestsNotApplicable {")],
  run(K5, "UNVERIFIABLE (the definition) over GREEN on a docs-only diff"))

# ================== 2.5 outcome table: verdict -> state -> outcome -> labels ==================
NV = "\tcase VerdictNotGreen, VerdictTestsNotRun:\n\t\treturn StateNotVerified, true"
m("outcome: NOT_GREEN is reported as success (inconclusive)", [(O, NV, NV.replace("StateNotVerified", "StateInconclusive"))], run(V2S, "NOT_GREEN"))
m("outcome: TESTS_NOT_RUN is labelled verified", [(O, NV, NV.replace("StateNotVerified", "StateVerified"))], run(V2S, "TESTS_NOT_RUN"))
m("outcome: TESTS_NOT_RUN has no state", [(O, "\tcase VerdictNotGreen, VerdictTestsNotRun:", "\tcase VerdictNotGreen:")], run(V2S, "TESTS_NOT_RUN"))
INC = "\tcase VerdictFlaky, VerdictUnverifiable:\n\t\treturn StateInconclusive, true"
m("outcome: FLAKY escalates (its state is not-verified)", [(O, INC, INC.replace("StateInconclusive", "StateNotVerified"))], run(V2S, "FLAKY"))
m("outcome: UNVERIFIABLE escalates (its state is not-verified)", [(O, INC, INC.replace("StateInconclusive", "StateNotVerified"))], run(V2S, "UNVERIFIABLE"))
m("outcome: UNVERIFIABLE is verified", [(O, INC, INC.replace("StateInconclusive", "StateVerified"))], run(V2S, "UNVERIFIABLE"))
m("outcome: FLAKY is verified-unobserved", [(O, "\tcase VerdictFlaky, VerdictUnverifiable:", "\tcase VerdictUnverifiable:"),
                                            (O, "\tcase VerdictGreenUnobserved:", "\tcase VerdictGreenUnobserved, VerdictFlaky:")], run(V2S, "FLAKY"))
m("outcome: GREEN_UNOBSERVED is the state `verified`",
  [(O, "\tcase VerdictGreenUnobserved:\n\t\treturn StateVerifiedUnobserved, true", "\tcase VerdictGreenUnobserved:\n\t\treturn StateVerified, true")],
  run(V2S, "GREEN_UNOBSERVED"))
m("outcome: GREEN is the state `verified-unobserved`",
  [(O, "\tcase VerdictGreen:\n\t\treturn StateVerified, true", "\tcase VerdictGreen:\n\t\treturn StateVerifiedUnobserved, true")], run(V2S, "GREEN"))
m("outcome: not-verified is reported as success (an artifact is present)",
  [(O, "\tcase StateNotVerified:\n\t\treturn false, true", "\tcase StateNotVerified:\n\t\treturn true, true")], run(V2S, "NOT_GREEN"))
PRES = "\tcase StateNeedsScope, StateVerified, StateVerifiedUnobserved, StateInconclusive,\n\t\tStateTakenOver, StateClosedUnmerged, StateMerged:"
for st, sub in (("StateNeedsScope, ", "needs-scope"), ("StateVerified, ", "verified"), ("StateVerifiedUnobserved, ", "verified-unobserved"),
                (" StateInconclusive,", "inconclusive"), ("StateTakenOver, ", "taken-over"), ("StateClosedUnmerged, ", "closed-unmerged"),
                (", StateMerged", "merged")):
    m("outcome: `%s` reports no outcome" % sub, [(O, PRES, PRES.replace(st, "", 1))], run(STATES, sub))
for st, sub in (("StateVerifying", "verifying"), ("StateProposing", "proposing"), ("StateDenied", "denied")):
    m("outcome: `%s` reports success" % sub, [(O, PRES, PRES.replace("StateMerged:", "StateMerged, %s:" % st, 1))], run(STATES, sub))
m("outcome: an attempt nobody paid for escalates (gate.Classify bypassed)",
  [(O, "gate.Signals{ArtifactPresent: present, ModelTokens: modelTokens}", "gate.Signals{ArtifactPresent: present, ModelTokens: 1}")], run(V2S, "TESTS_NOT_RUN"))
m("outcome: more than not-verified escalates (inconclusive is absent)",
  [(O, PRES, PRES.replace(" StateInconclusive,", "", 1)), (O, "\tcase StateNotVerified:\n", "\tcase StateNotVerified, StateInconclusive:\n")],
  run('TestOnlyNotVerifiedEscalates'))
m("outcome: something that is not a verdict has an outcome",
  [(O, "\ts, known := StateFor(v)\n\tif !known {\n\t\treturn \"\", false\n\t}", "\ts, known := StateFor(v)\n\tif !known {\n\t\ts = StateVerified\n\t}")],
  run('TestWhatIsNotAVerdictOrAStateHasNoOutcome'))
m("states: every string is a known state", [(O, "\t\tif k == s {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false", "\t\tif k == s {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn true")],
  run('TestWhatIsNotAVerdictOrAStateHasNoOutcome'))
TERM = "\tcase StateProposing, StateVerifying, StateNotVerified:"
for st, sub in (("StateProposing, ", "proposing"), ("StateVerifying, ", "verifying"), (", StateNotVerified", "not-verified")):
    m("states: `%s` is terminal" % sub, [(O, TERM, TERM.replace(st, "", 1))], run(STATES, sub))
m("states: `inconclusive` is not terminal", [(O, TERM, TERM.replace("StateNotVerified:", "StateNotVerified, StateInconclusive:"))], run(STATES, "inconclusive"))
m("states: `verified` is not terminal (I5 would call it in flight)", [(O, TERM, TERM.replace("StateNotVerified:", "StateNotVerified, StateVerified:"))],
  run(ROWS, "I5 a change for this issue ended verified"))
m("states: K5's order is not the order of Verdicts()",
  [(O, "\treturn []Verdict{VerdictNotGreen, VerdictTestsNotRun, VerdictFlaky, VerdictUnverifiable,", "\treturn []Verdict{VerdictTestsNotRun, VerdictNotGreen, VerdictFlaky, VerdictUnverifiable,")],
  run(V2S, "NOT_GREEN"))
m("labels: needs-scope does not ask for a human", [(O, "\t\treturn Labels{IssueNeedsHuman: true}", "\t\treturn Labels{}")], run(STATES, "needs-scope"))
VER = "\tcase StateVerifying, StateNotVerified:"
m("labels: not-verified shows nothing", [(O, VER, "\tcase StateVerifying:")], run(STATES, "not-verified"))
m("labels: verifying shows nothing", [(O, VER, "\tcase StateNotVerified:")], run(STATES, "verifying"))
m("labels: not-verified is labelled verified", [(O, VER + "\n\t\treturn Labels{MR: []string{LabelVerifying}}", VER + "\n\t\treturn Labels{MR: []string{LabelVerified}}")],
  run(STATES, "not-verified"))
m("labels: verified says `tests observed` whatever was observed", [(O, "\t\tif tests == TestsObserved {", "\t\tif true {")], run(OBSLABEL))
m("labels: verified says `tests observed` unless the finding is unobserved", [(O, "\t\tif tests == TestsObserved {", "\t\tif tests != TestsUnobserved {")], run(OBSLABEL))
m("labels: verified never says `tests observed`", [off(O, "\t\tif tests == TestsObserved {")], run(STATES, "verified"))
m("labels: verified-unobserved says `tests observed`",
  [(O, "Labels{MR: []string{LabelVerified, LabelTestsUnobserved}}", "Labels{MR: []string{LabelVerified, LabelTestsObserved}}")], run(STATES, "verified-unobserved"))
m("labels: inconclusive is labelled verified",
  [(O, "Labels{MR: []string{LabelInconclusive}, IssueNeedsHuman: true}", "Labels{MR: []string{LabelVerified}, IssueNeedsHuman: true}")], run(STATES, "inconclusive"))
m("labels: inconclusive does not ask for a human",
  [(O, "Labels{MR: []string{LabelInconclusive}, IssueNeedsHuman: true}", "Labels{MR: []string{LabelInconclusive}}")], run(STATES, "inconclusive"))
m("labels: a label a state shows is not in MRLabels()", [(O, "LabelTestsUnobserved, LabelInconclusive}\n", "LabelTestsUnobserved}\n")], run('TestTheLabels'))
m("labels: the implement label is not gonk::implement", [(O, 'LabelImplement       = "gonk::implement"', 'LabelImplement       = "gonk::change-implement"')],
  run('TestTheLabels'))

# ---- constants, and the tests that read the plan
m("constants: the requester floor is not GitLab's Maintainer", [(E, "const MinRequesterAccess = 40", "const MinRequesterAccess = 50")],
  run('TestTheRequesterFloorIsGitLabsMaintainer'))
m("constants: `docs` is not scope's class", [(J, 'ClassDocs   = "docs"', 'ClassDocs   = "doc"')], run('TestTheClassesAreScopes'))
m("constants: Forms() is not the owner's three, in order", [(E, "return []Form{FormLabel, FormCommand, FormMention} }", "return []Form{FormLabel, FormCommand} }")],
  run(PLANT, "2.1 the trigger is the owner's decision 8.1 Q2"))
m("constants: Forms() is not the owner's three, in order (the forms test)", [(E, "return []Form{FormLabel, FormCommand, FormMention} }", "return []Form{FormCommand, FormLabel, FormMention} }")],
  run(FORMS))
m("test table: the only I8 fixture is dropped (the plan-row test must notice)",
  [(ET, '{name: "I8 a test report is required and the base published none", row: "I8",', '{name: "I8 a test report is required and the base published none", row: "I7",'),
   (ET, "code: I8NoTestReport, note: true, noVerifier: true,", "code: I7ConfigInvalid, note: true, noVerifier: true,")],
  run(PLANT, "2.1 eligibility rows I1-I8"))
m("test table: a K token loses its fixture (K2's only tag)",
  [(JT, '.named("the definition changed, execution preserved", "K2:UNVERIFIABLE(definition)").', '.named("the definition changed, execution preserved").')],
  run(PLANT, "2.5 the K block K0-K5"))
m("test table: NEW_NOT_RUN, named twice in the plan, keeps one fixture",
  [(JT, '.named("go: the added function has no case", "K4:NEW_NOT_RUN").', '.named("go: the added function has no case").')],
  run(PLANT, "2.5 the K block K0-K5"))
m("test table: a tag sits on a fixture that asserts something else",
  [(JT, 'named("a job failed and was retried into green", "K3:FLAKY", "K5:FLAKY")', 'named("a job failed and was retried into green", "K3:RED(j)", "K5:FLAKY")'),
   (JT, '.named("a job failed on its only attempt", "K3:RED(j)", "K5:NOT_GREEN")', '.named("a job failed on its only attempt", "K3:FLAKY", "K5:NOT_GREEN")')],
  run(PLANT, "2.5 the K block K0-K5"))
m("plan: a row I9 is added to section 2.1 (the test must read the plan)",
  [(PLAN, "| I8 | when `change.require_test_report`:", "| I9 | a new condition | N |\n| I8 | when `change.require_test_report`:")],
  run(PLANT, "2.1 eligibility rows I1-I8"))
m("plan: K4 gains an outcome (the test must read the plan)",
  [(PLAN, "                  else                                          -> TESTS_OBSERVED\n",
    "                  the report is older than the commit           -> TESTS_STALE\n                  else                                          -> TESTS_OBSERVED\n")],
  run(PLANT, "2.5 the K block K0-K5"))
m("plan: NOT_GREEN no longer escalates in the outcome table (the test must read the plan)",
  [(PLAN, "| NOT_GREEN, TESTS_NOT_RUN | `not-verified` | gate-failed | yes;", "| NOT_GREEN, TESTS_NOT_RUN | `not-verified` | success | no;")],
  run(PLANT, "2.5 the outcome table"))
m("plan: GREEN_UNOBSERVED is labelled tests-observed in the outcome table",
  [(PLAN, "`gonk::change-verified`, `gonk::change-tests-unobserved` |", "`gonk::change-verified`, `gonk::change-tests-observed` |")],
  run(PLANT, "2.5 the outcome table"))
m("plan: the owner's Q2 decision is reworded (the test must read 8.1)",
  [(PLAN, "THREE trigger forms, each valid only when the actor is a project OWNER or MAINTAINER", "TWO trigger forms, each valid only when the actor is a project DEVELOPER")],
  run(PLANT, "2.1 the trigger is the owner's decision 8.1 Q2"))

# ------------------------------------------------------------------ the runner
if '--list' in sys.argv[1:]:
    for i, (name, _, pattern) in enumerate(muts):
        print(f"[{i}] {name}    -run {pattern}")
    sys.exit(0)

show = '--show' in sys.argv[1:]
only = [a for a in sys.argv[1:] if a != '--show']

base = subprocess.run([GO, "test", "./pkg/changejudge/", "-count=1"], capture_output=True, text=True)
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
        r = subprocess.run([GO, "test", "./pkg/changejudge/", "-run", pattern, "-count=1"],
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
