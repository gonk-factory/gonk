#!/usr/bin/env python3
"""mutate_mergepolicy.py -- "seen red" evidence for gonk-cuwn part MP-CORE (pkg/mergepolicy).

Disables one predicate at a time in pkg/mergepolicy, runs ONLY the test that
guards it (each mutation names its own -run pattern, so a kill means that
predicate's own fixture went red, not that some unrelated test noticed), and
requires it to FAIL; then restores every file byte for byte (try/finally). A
mutation whose text is not found is VACUOUS, one the named test does not
notice SURVIVED, and one that does not compile NOCOMPILE -- each is a failure
of this script. Before anything is mutated the unmutated package must be
green, or a kill would mean nothing.

The rows are the plans': M1-M11 of the epic plan's section 5.1 (which carry
A1-A9 of the fix plan's 9.1), the class rows of 5.2 with MP-DOCSPURE, the
modes of 5.3. EVERY rule code of rules.go and every DP-* code of docspure.go
must be named by at least one mutation, or the script refuses to run: a rule
added later with no mutation is not "covered".

The tests plan 6.2 names for this part each have a NAMED break, marked
[NAMED] in the list:
  an unreadable fact merges              "a fact group's read gate is not consulted"
  the bot's own approval counts          "an approver is not compared with the bot"
  docs/tool.py counts as docs-only       "MP-DOCSPURE is not asked" / ".py is on the allow-list"
  same-model merges                      "a same-model review satisfies after-agentic-review"
  a blocking fact merges under
    human-approval mode                  "the veto is read under after-agentic-review only"
  a label without a review fact merges   "a gonk::review-no-blocking label stands in for the fact"

THE M ROWS. One mutation is guarded by TestEveryPlanMRowHasAFixture, which
reads the M table of docs/reference/gonk-change-rules.md (once section 5.1 of
the epic plan). That page is always in the tree and the test cannot skip; if
the page is missing the mutation is reported NOPLAN and the script fails.

Usage: hack/mutate_mergepolicy.py            (all mutations)
       hack/mutate_mergepolicy.py 3 7        (only these indexes)
       hack/mutate_mergepolicy.py --list     (names and indexes, nothing run)
       hack/mutate_mergepolicy.py --show 3   (also print the full `go test` output)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate
(gonk-5ed1); its output is pasted into the part's evidence file.
"""
import os
import re
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
P = 'pkg/mergepolicy/'
D = P + 'decide.go'
DP = P + 'docspure.go'
R = P + 'rate.go'
RU = P + 'rules.go'
F = P + 'facts.go'
DT = P + 'decide_test.go'
PLAN = 'docs/reference/gonk-change-rules.md'


def run(test, *subs):
    """-run pattern: the test, then each subtest level, anchored and escaped
    the way `go test` names subtests (spaces become underscores). A level
    that itself holds a slash is several levels to `go test`."""
    parts = ['^%s$' % test]
    for s in subs:
        for level in s.split('/'):
            parts.append('^%s$' % re.escape(level.replace(' ', '_')))
    return '/'.join(parts)


ALONE = 'TestEachFactFalseAloneBlocksTheMerge'
UNREAD = 'TestAnUnreadFactNeverMerges'
STALE = 'TestAStaleFactNeverMerges'
BASE = 'TestTheBaselineMerges'
BOT = 'TestTheBotsOwnApprovalNeverCounts'
SCRIPT = 'TestAScriptUnderDocsIsNotDocsOnly'
SAME = 'TestTheSameModelNeverSatisfiesAgenticReview'
VETO = 'TestABlockingReviewVetoesUnderEveryMode'
LABEL = 'TestALabelIsNotAReviewFact'
NEVER = 'TestWhatNeverMergesUnderAnySetting'
CLASSES = 'TestNoConfigMakesAnotherClassEligible'
PIN = 'TestTheMergeIsPinnedToTheJudgedSHA'
ORDER = 'TestEveryFailedFactIsListedInPlanOrder'
PURE = 'TestDocsPure'
WINDOW = 'TestMergesInWindow'
INDEP = 'TestIndependence'

# the scenarios of decide_test.go
HC = 'after-human-approval/pipefix/ci'
HD = 'after-human-approval/pipefix/docs'
HX = 'after-human-approval/change/docs'
AC = 'after-agentic-review/pipefix/ci'
AD = 'after-agentic-review/pipefix/docs'

# (name, [(file, old, new), ...], -run pattern of the guarding test, codes)
muts = []


def m(codes, name, edits, pattern):
    """codes: the rule / DP codes this mutation is evidence for ('' for none)."""
    muts.append((name, edits, pattern, codes.split()))


def off(src, line):
    """Disable an `if [init;] <cond> {` line: the condition becomes
    `false && (<cond>)`, so the init statement and every variable the
    condition reads stay in use and the mutant still compiles."""
    assert line.lstrip('\t').startswith('if ') and line.endswith(' {'), line
    head, sep, cond = line[:-2].rpartition('; ')
    if not sep:
        head, cond = line[:len(line) - len(line.lstrip('\t'))] + 'if', line.lstrip('\t')[3:-2]
        return (src, line + '\n', '%s false && (%s) {\n' % (head, cond))
    return (src, line + '\n', '%s; false && (%s) {\n' % (head, cond))


def never(src, line):
    """Disable a `case <cond>:` line of a tagless switch."""
    assert line.lstrip('\t').startswith('case ') and line.endswith(':'), line
    indent = line[:len(line) - len(line.lstrip('\t'))]
    return (src, line + '\n', indent + 'case false:\n')


def alone(scn, case):
    return run(ALONE, scn, case)


# ================= the read gate: one mutation per fact group =================
GATES = [
    ("M1", '\tif !e.read(e.f.Config.ReadAt, M1Unread, M1Stale, "the auto-merge configuration") {', HC),
    ("M2", '\tif !e.read(e.f.MR.ReadAt, M2Unread, M2Stale, "the merge request") {', HC),
    ("M3", '\tif !e.read(e.f.Verdict.ReadAt, M3Unread, M3Stale, "the recomputed exit verdict") {', HC),
    ("M4", '\tif !e.read(e.f.CI.ReadAt, M4Unread, M4Stale, "the CI definition delta") {', HC),
    ("M5", '\tif !e.read(e.f.Diff.ReadAt, M5Unread, M5Stale, "the merge request\'s current diff") {', HC),
    ("M6", '\tif !e.read(e.f.Evidence.ReadAt, M6Unread, M6Stale, "the candidate pipelines") {', HC),
    ("M7", '\tif !e.read(e.f.Target.ReadAt, M7Unread, M7Stale, "the target branch head") {', HC),
    ("M8", '\tif !e.read(e.f.Approvals.ReadAt, M8Unread, M8Stale, "the merge request\'s approvals") {', HC),
    ("M9", '\tif !e.read(e.f.Review.ReadAt, M9Unread, M9Stale, "the review records") {', AC),
    ("M10", '\tif !e.read(e.f.Status.ReadAt, M10Unread, M10Stale, "the merge request\'s merge status") {', HC),
    ("M11", '\tif !e.read(e.f.Rate.ReadAt, M11Unread, M11Stale, "the project\'s automatic-merge ledger") {', HC),
]
for row, gate, scn in GATES:
    m("%s.unread" % row, "[NAMED] %s: an unread fact merges (the group's read gate is not consulted)" % row,
      [off(D, gate)], run(UNREAD, scn, "%s was not read" % row))
    m("%s.stale" % row, "%s: a fact from the judging pass merges (the group's read gate is not consulted)" % row,
      [off(D, gate)], run(STALE, scn, "%s read by the pass that judged, an hour ago" % row))
    m("%s.stale" % row, "%s: the stale fact is reported as unread" % row,
      [(D, gate, gate.replace("%sStale" % row, "%sUnread" % row))], run(STALE, scn, "%s read 60 s and 1 ns ago" % row))
m("", "read gate: a zero ReadAt is a read",
  [(D, "\tif at.IsZero() {\n\t\treturn false, false", "\tif at.IsZero() {\n\t\treturn true, true")], run(UNREAD, "the zero value of everything"))
m("", "read gate: a zero ReadAt is a read (one group)",
  [(D, "\tif at.IsZero() {\n\t\treturn false, false", "\tif at.IsZero() {\n\t\treturn true, true")], run(UNREAD, HC, "M7 was not read"))
m("", "read gate: any age is fresh", [(D, "!at.After(now) && now.Sub(at) <= MaxFactAge", "!at.After(now)")],
  run(STALE, HC, "M3 read 60 s and 1 ns ago"))
m("", "read gate: a fact read after `now` is fresh", [(D, "!at.After(now) && now.Sub(at) <= MaxFactAge", "now.Sub(at) <= MaxFactAge")],
  run(STALE, HC, "M3 read 1 ns in the future"))
m("", "read gate: exactly 60 s is already stale", [(D, "now.Sub(at) <= MaxFactAge", "now.Sub(at) < MaxFactAge")],
  run(STALE, HC, "M3 read exactly 60 s ago"))
m("", "read gate: MaxFactAge is ten minutes", [(F, "MaxFactAge = 60 * time.Second", "MaxFactAge = 600 * time.Second")],
  run(STALE, HC, "M3 read 60 s and 1 ns ago"))
m("", "read gate: an unread failure is not marked Unread",
  [(D, 'Failure{Rule: unread, Unread: true,', 'Failure{Rule: unread,')], run(UNREAD, HC, "M2 was not read"))
m("", "read gate: a stale failure is not marked Unread",
  [(D, 'Failure{Rule: stale, Unread: true,', 'Failure{Rule: stale,')], run(STALE, HC, "M2 read 60 s and 1 ns ago"))
m("", "an ordinary failure is marked Unread",
  [(D, "Failure{Rule: code, Text: fmt.Sprintf(format, args...)}", "Failure{Rule: code, Unread: true, Text: fmt.Sprintf(format, args...)}")],
  alone(HC, "M2 the MR is a draft"))

# ================================== M1 ==================================
m("M1.mode-off", "M1: mode off merges",
  [(D, '\t\te.fail(M1ModeOff, "auto_merge.mode is off for this merge request kind")\n', "")], alone(HC, "M1 the mode is off in every layer"))
m("M1.mode-off", "M1: the zero mode is not off", [(D, '\tcase ModeOff, "":', "\tcase ModeOff:")], alone(HC, "M1 the mode is the zero value"))
m("M1.mode-off", "M1: the zero Config merges (off is not the default)",
  [(D, '\t\te.fail(M1ModeOff, "auto_merge.mode is off for this merge request kind")\n', "")], run('TestOffIsTheDefault'))
m("M1.mode-unknown", "M1: a mode nobody defined is accepted",
  [(D, '\t\te.fail(M1ModeUnknown, "auto_merge.mode %q is not a mode", e.cfg.Mode)\n', "")], alone(HC, "M1 the mode is one nobody defined"))
m("M1.operator-veto", "M1: the operator veto is not read", [off(D, "\tif e.f.Config.OperatorVeto {")], alone(HC, "M1 an operator layer vetoes"))
m("M1.layers-disagree", "M1: no layer need state the mode", [off(D, "\tif len(e.f.Config.LayerModes) == 0 {")], alone(HC, "M1 no layer states a mode"))
m("M1.layers-disagree", "M1: layers may disagree", [off(D, "\t\t\tif l.Mode != e.cfg.Mode {")], alone(HC, "M1 the group layer says off"))
m("M1.layers-disagree", "M1: only the FIRST layer is compared",
  [(D, "for _, l := range e.f.Config.LayerModes {", "for _, l := range e.f.Config.LayerModes[:1] {")], alone(HC, "M1 the group layer says off"))
TIP = "\tif head := e.f.Target.HeadSHA; fullSHA(head) && e.f.Config.AtSHA != head {"
m("M1.config-not-at-target-head", "M1: the config may come from any commit", [off(D, TIP)], alone(HC, "M1 the config was read at another commit"))
m("M1.config-not-at-target-head", "M1: an unknown config commit is accepted",
  [(D, TIP, TIP.replace("e.f.Config.AtSHA != head", 'e.f.Config.AtSHA != "" && e.f.Config.AtSHA != head'))],
  alone(HC, "M1 the config's commit is unknown"))
m("M1.config-not-at-target-head", "M1: the config is held to a target head that is not a SHA (M7's refusal becomes M1's)",
  [(D, TIP, TIP.replace("fullSHA(head) && ", ""))], run(UNREAD, HC, "M7 is marked read and is empty"))

# ================================== M2 ==================================
m("M2.kind-unknown", "M2: an unknown workflow kind is accepted", [off(D, "\tif !known {")], alone(HC, "M2 the workflow kind is unknown"))
m("M2.judged-sha-invalid", "M2: the judged SHA is not validated", [off(D, "\tif !fullSHA(e.f.JudgedSHA) {")], run(PIN))
m("M2.judged-sha-invalid", "M2: the judged SHA is not validated (single fact)", [off(D, "\tif !fullSHA(e.f.JudgedSHA) {")],
  alone(HC, "M2 the judged SHA is abbreviated"))
m("M2.judged-sha-invalid", "M2: an abbreviated SHA is a full SHA", [off(D, "\tif len(s) != 40 {")], run(PIN))
m("M2.judged-sha-invalid", "M2: 40 characters of anything are a SHA",
  [off(D, "\t\tif (c < '0' || c > '9') && (c < 'a' || c > 'f') {")], run(PIN))
m("M2.bot-identity-unknown", "M2: an unknown bot id is not its own refusal", [never(D, "\tcase e.f.BotUserID == 0:")],
  alone(AC, "M2 the bot's user id is unknown"))
m("M2.not-bot-authored", "M2: anybody's merge request is merged", [never(D, "\tcase mr.AuthorID != e.f.BotUserID:")], alone(HC, "M2 a person authored it"))
m("M2.branch-not-workflow", "M2: the branch prefix is not checked",
  [off(D, "\tif known && !strings.HasPrefix(mr.SourceBranch, prefix) {")], alone(HC, "M2 the source branch has no workflow prefix"))
m("M2.branch-not-workflow", "M2: a pipefix MR may sit on any gonk/ branch",
  [(D, "\t\treturn pipelinectx.FixBranchPrefix, true", '\t\treturn pipelinectx.FixBranchPrefix[:5], true')], alone(HC, "M2 a pipefix MR on a change branch"))
m("M2.branch-not-workflow", "M2: a change MR may sit on any gonk/ branch",
  [(D, "\t\treturn ChangeBranchPrefix, true", '\t\treturn "gonk/", true')], alone(HX, "M2 a change MR on a pipefix branch"))
m("M2.branch-not-recorded", "M2: the recorded branch is not compared", [off(D, "\tif mr.SourceBranch != mr.RecordBranch {")],
  alone(HC, "M2 the source branch is not the recorded one"))
m("M2.branch-not-recorded", "M2: a record with no branch is accepted",
  [(D, "\tif mr.SourceBranch != mr.RecordBranch {", '\tif mr.RecordBranch != "" && mr.SourceBranch != mr.RecordBranch {')],
  alone(HC, "M2 the record holds no branch"))
ANCHOR = '\tif mr.RecordAnchor == "" || mr.MetadataAnchor != mr.RecordAnchor {'
m("M2.anchor-mismatch", "M2: the metadata anchor is not compared", [off(D, ANCHOR)], alone(HC, "M2 the metadata anchor is another run's"))
m("M2.anchor-mismatch", "M2: two empty anchors match", [(D, ANCHOR, "\tif mr.MetadataAnchor != mr.RecordAnchor {")],
  alone(HC, "M2 no anchor on either side"))
m("M2.head-not-judged", "M2: a moved head merges", [off(D, "\tif mr.HeadSHA != e.f.JudgedSHA {")],
  alone(HC, "M2 somebody pushed: the head is not the judged commit"))
m("M2.draft", "M2: a draft merges", [off(D, "\tif mr.Draft {")], alone(HC, "M2 the MR is a draft"))
HOLD = "\t\tif strings.EqualFold(strings.TrimSpace(l), HoldLabel) {"
m("M2.hold", "M2: gonk::hold is ignored", [off(D, HOLD)], alone(HC, "M2 a human set gonk::hold"))
m("M2.hold", "M2: gonk::hold is matched byte for byte", [(D, HOLD, "\t\tif l == HoldLabel {")], alone(HC, "M2 gonk::hold in another case"))
m("M2.threads-unresolved", "M2: unresolved threads merge", [off(D, "\tif !mr.BlockingDiscussionsResolved {")], alone(HC, "M2 a thread is unresolved"))

# ================================== M3 ==================================
m("M3.verdict-wrong-sha", "M3: a verdict about another commit is accepted", [off(D, "\tif v.SHA != e.f.JudgedSHA {")],
  alone(HC, "M3 the verdict is about another commit"))
FNF = [(D, "\t\tcase string(pipefix.VerdictFixed):\n", "\t\tcase string(pipefix.VerdictFixed), string(pipefix.VerdictFixedNewFailure):\n"),
       (D, "\t\tcase string(pipefix.VerdictFixedNewFailure):\n", '\t\tcase "never":\n')]
m("M3.fixed-new-failure", "M3: FIXED_NEW_FAILURE merges", FNF, run(NEVER))
m("M3.fixed-new-failure", "M3: FIXED_NEW_FAILURE merges (single fact)", FNF, alone(AD, "M3 FIXED_NEW_FAILURE"))
m("M3.fixed-new-failure", "M3: FIXED_NEW_FAILURE has no code of its own",
  [(D, "\t\tcase string(pipefix.VerdictFixedNewFailure):\n", '\t\tcase "never":\n')], alone(HC, "M3 FIXED_NEW_FAILURE"))
m("M3.not-best-verdict", "M3: any pipefix verdict is accepted",
  [(D, '\t\tdefault:\n\t\t\te.fail(M3NotBestVerdict, "the pipeline-fix verdict is %q, not %s", v.Verdict, pipefix.VerdictFixed)\n\t\t\treturn\n', "\t\tdefault:\n")],
  alone(HC, "M3 UNVERIFIABLE"))
m("M3.not-best-verdict", "M3: any change verdict is accepted", [off(D, "\t\tif v.Verdict != VerdictGreen {")], alone(HX, "M3 verified-unobserved"))
m("M3.no-target-jobs", "M3: FIXED with no diagnosed job is accepted", [off(D, "\t\tif len(v.TargetJobs) == 0 {")],
  alone(HC, "M3 FIXED with no target job"))
m("M3.target-not-passed", "M3: a diagnosed job need not pass", [off(D, "\t\t\tif !j.Passed {")], alone(HC, "M3 a target job did not pass"))
m("M3.target-not-passed", "M3: only the FIRST diagnosed job is checked",
  [(D, "for _, j := range v.TargetJobs {", "for _, j := range v.TargetJobs[:1] {")], alone(HC, "M3 a target job did not pass"))
m("M3.target-retried", "M3: a retried diagnosed job is accepted", [off(D, "\t\t\tif j.Attempts != 1 {")],
  alone(HC, "M3 a target job passed on its second attempt"))
m("M3.target-retried", "M3: a job with no attempt passed on its only attempt",
  [(D, "\t\t\tif j.Attempts != 1 {", "\t\t\tif j.Attempts > 1 {")], alone(HC, "M3 a target job with no attempt"))

# ================================== M4 ==================================
EXEC = [(D, "\tcase cidef.KindNone, cidef.KindPreserving:\n", "\tcase cidef.KindNone, cidef.KindPreserving, cidef.KindExecChanging:\n"),
        (D, "\tcase cidef.KindExecChanging:\n", '\tcase "never":\n')]
m("M4.exec-changing", "M4: exec-changing is an eligible delta", EXEC, run(NEVER))
m("M4.exec-changing", "M4: exec-changing has no code of its own", [(D, "\tcase cidef.KindExecChanging:\n", '\tcase "never":\n')],
  alone(HC, "M4 exec-changing"))
NOTEL = '\tdefault:\n\t\te.fail(M4DeltaNotEligible, "the CI definition delta is %q, not %s or %s", delta, cidef.KindNone, cidef.KindPreserving)\n\t\treturn\n'
for case in ("M4 violation", "M4 unresolvable", "M4 the zero Kind"):
    m("M4.delta-not-eligible", "M4: `%s` is not refused as an ineligible delta" % case[3:], [(D, NOTEL, "\tdefault:\n")], alone(HC, case))
FORCLASS = "\tif want := classDelta(class); delta != want {"
m("M4.delta-not-for-class", "M4: the delta is not held to the class row (docs)", [off(D, FORCLASS)],
  alone(HD, "M4 a docs-only diff with a preserving CI delta"))
m("M4.delta-not-for-class", "M4: the delta is not held to the class row (ci)", [off(D, FORCLASS)],
  alone(HC, "M4 a ci-only diff that changed nothing effective"))
m("M4.delta-not-for-class", "M4: a ci-only diff wants delta none", [off(D, "\tif class == scope.ClassCI {")],
  alone(HC, "M4 a ci-only diff that changed nothing effective"))
m("M4.delta-not-for-class", "M4: a diff of no known class is held to the docs row",
  [off(D, "\tif !classKnown {")], alone(HC, "M5 the diff is empty"))

# ================================== M5 ==================================
m("M5.diff-incomplete", "M5: an incomplete diff is a diff", [off(D, "\tif d.Incomplete {")], alone(HC, "M5 GitLab withheld part of the diff"))
m("M5.empty-diff", "M5: an empty diff merges", [off(D, "\tif len(d.Paths) == 0 {")], alone(HC, "M5 the diff is empty"))
UNCL = '\t\tcase p.Decision.Path == "" || !c.Valid():'
m("M5.unclassified", "M5: an unclassified path is not its own refusal", [never(D, UNCL)], alone(HC, "M5 a path has a class nobody defined"))
m("M5.unclassified", "M5: a path with no name is classified", [(D, UNCL, "\t\tcase !c.Valid():")], alone(HD, "M5 a path with a class and no name"))
m("M5.unclassified", "M5: unclassified paths are skipped, not refused", [off(D, "\tif len(unclassified) > 0 {")],
  alone(HD, "M5 a path was not classified"))
m("M5.static-gates-failed", "M5: the static gates are not re-run", [off(D, "\tif !d.StaticGatesPassed {")],
  alone(HC, "M5 a static gate failed on the current diff"))
m("M5.weakening-undecidable", "M5: undecidable test-weakening is not reported", [off(D, "\tif len(undecidable) > 0 {")],
  alone(HC, "M5 a non-Go test: weakening is not decidable"))
m("M5.weakening-undecidable", "M5: every path's weakening is decidable", [off(D, "\t\tif !p.Decision.WeakeningDecidable() {")],
  alone(HD, "M5 a non-Go test: weakening is not decidable"))
NEV = "\t\tcase !containsClass(EligibleClasses(), c):"
m("M5.class-never-eligible", "M5: auto_merge.classes can make `source` eligible", [never(D, NEV)],
  alone(HC, "M5 a source file the project listed in auto_merge.classes"))
m("M5.class-never-eligible", "M5: a Go test-only diff is not refused by class", [never(D, NEV)], alone(HD, "M5 a Go test only"))
for cls, const in (("test", "ClassTest"), ("source", "ClassSource"), ("deps", "ClassDeps")):
    m("M5.class-never-eligible", "M5: `%s` is in the closed set of eligible classes" % cls,
      [(F, "return []scope.Class{scope.ClassDocs, scope.ClassCI} }", "return []scope.Class{scope.ClassDocs, scope.ClassCI, scope.%s} }" % const)],
      run(CLASSES))
for cls in ("source", "test", "deploy", "other"):
    m("M5.class-never-eligible", "M5: auto_merge.classes can make `%s` eligible (the closed set is not consulted)" % cls, [never(D, NEV)],
      run(CLASSES, HD, cls))
m("M5.class-not-enabled", "M5: auto_merge.classes is not read", [never(D, "\t\tcase !containsClass(e.cfg.Classes, c):")],
  alone(HC, "M5 the project enabled no class"))
m("M5.class-not-enabled", "M5: auto_merge.classes is not read (the other class is enabled)",
  [never(D, "\t\tcase !containsClass(e.cfg.Classes, c):")], alone(HD, "M5 the project did not enable this class"))
MIX = "\tif len(never) == 0 && len(classes) > 1 {"
m("M5.mixed-classes", "M5: docs and ci together merge", [off(D, MIX)], alone(HC, "M5 docs and ci together"))
m("M5.mixed-classes", "M5: the class is that of the FIRST path",
  [off(D, "\t\tif p.Decision.Class != class {")], alone(HC, "M5 docs and ci together"))
m("M5.mixed-classes", "M5: an ineligible class beside an eligible one is also `mixed`", [(D, MIX, "\tif len(classes) > 1 {")],
  alone(HC, "M5 a source file beside the eligible ones"))
m("M5.class-never-eligible", "M5: a diff of an ineligible class has a class row",
  [off(D, "\tif !containsClass(EligibleClasses(), class) {")], alone(HC, "M5 a Go test only"))
m("M5.ci-not-for-kind", "M5: a ci-only change MR merges", [off(D, "\tif classes[scope.ClassCI] && e.f.Kind == KindChange {")],
  alone(HX, "M5 a ci-only change MR"))
PUREIF = "\t\tif findings := DocsPure(docs, d.Profiles); len(findings) > 0 {"
m("M5.MP-DOCSPURE", "[NAMED] M5: docs/tool.py counts as docs-only (MP-DOCSPURE is not asked)", [off(D, PUREIF)],
  run(SCRIPT, HD, "docs/tool.py"))
m("M5.MP-DOCSPURE", "[NAMED] M5: docs/tool.py counts as docs-only (single fact)", [off(D, PUREIF)], alone(HX, "M5 docs/tool.py is not prose"))
m("M5.MP-DOCSPURE", "[NAMED] M5: docs/tool.py counts as docs-only (.py is on the allow-list)",
  [(DP, '\t"docs-md":   {".md"},', '\t"docs-md":   {".md", ".py"},')], run(SCRIPT, HD, "docs/tool.py"))
m("M5.MP-DOCSPURE", "M5: the selected profiles are not passed to MP-DOCSPURE",
  [(D, PUREIF, PUREIF.replace("d.Profiles", "scope.DefaultProfiles()"))], alone(HD, "M5 no docs profile selected"))
m("M5.MP-DOCSPURE", "M5: MP-DOCSPURE is asked about the first docs path only",
  [(D, PUREIF, PUREIF.replace("DocsPure(docs,", "DocsPure(docs[:1],"))], alone(HD, "M5 a new executable Markdown file"))

# ---------------------------- MP-DOCSPURE itself ----------------------------
m("DP-EMPTY", "MP-DOCSPURE: no path is prose-only", [off(DP, "\tif len(paths) == 0 {")], run('TestDocsPureOfNothingIsNotPure'))
m("DP-FORM", "MP-DOCSPURE: an entry with no path is judged by its extension", [off(DP, '\t\tif name == "" {')], run(PURE, "no path"))
FORM = "\t\tif p.Created && (p.Deleted || p.Renamed) {"
m("DP-FORM", "MP-DOCSPURE: created-and-deleted is accepted", [off(DP, FORM)], run(PURE, "created and deleted at once"))
m("DP-FORM", "MP-DOCSPURE: created-and-renamed is accepted", [(DP, FORM, "\t\tif p.Created && p.Deleted {")], run(PURE, "created and renamed at once"))
m("DP-CLASS", "MP-DOCSPURE: the class is not checked", [off(DP, "\t\tif p.Decision.Class != scope.ClassDocs {")],
  run(PURE, "a Markdown file that is not class docs (a chart's README)"))
m("DP-EXT", "MP-DOCSPURE: the extension is not checked", [off(DP, "\t\tif !proseName(name, exts) {")], run(PURE, "docs/tool.py"))
m("DP-EXT", "MP-DOCSPURE: every shipped docs profile's extension counts, selected or not",
  [(DP, "\tfor _, name := range profiles {", "\tfor name := range proseProfiles {")], run(PURE, "a .rst file when only docs-md is selected"))
m("DP-EXT", "MP-DOCSPURE: a file named only `.md` is prose", [(DP, "\treturn ext != base && containsString(exts, ext)", "\treturn containsString(exts, ext)")],
  run(PURE, "a file named only .md"))
m("DP-EXT", "MP-DOCSPURE: the extension is matched without regard to case",
  [(DP, "\tbase := path.Base(name)\n\text := path.Ext(base)", "\tbase := path.Base(name)\n\text := strings.ToLower(path.Ext(base))")],
  run(PURE, "an upper-case extension"))
m("DP-EXT", "MP-DOCSPURE: `.asciidoc` is on the allow-list", [(DP, '\t"docs-adoc": {".adoc"},', '\t"docs-adoc": {".adoc", ".asciidoc"},')],
  run(PURE, ".asciidoc is not on the list"))
m("DP-EXT", "MP-DOCSPURE: the allow-list is not pinned to the plan's four", [(DP, '\t"docs-txt":  {".txt"},', '\t"docs-txt":  {".txt", ".text"},')],
  run('TestProseExtensionsFollowTheShippedDocsProfiles'))
NEWMODE = "\t\tcase !p.Deleted && p.NewMode != FileModeRegular:"
m("DP-MODE", "MP-DOCSPURE: an executable file is prose", [never(DP, NEWMODE)], run(PURE, "an executable Markdown file"))
m("DP-MODE", "MP-DOCSPURE: an executable file is prose (through Decide)", [never(DP, NEWMODE)], alone(HD, "M5 a new executable Markdown file"))
m("DP-MODE", "MP-DOCSPURE: an unread mode is a regular file",
  [(DP, NEWMODE, '\t\tcase !p.Deleted && p.NewMode != FileModeRegular && p.NewMode != "":')], run(PURE, "the new mode was not read"))
m("DP-MODE", "MP-DOCSPURE: only the executable bit is refused (a symlink is prose)",
  [(DP, NEWMODE, "\t\tcase !p.Deleted && p.NewMode == FileModeExecutable:")], run(PURE, "a symlink named like Markdown"))
m("DP-MODE", "MP-DOCSPURE: a deleted file may keep a mode", [never(DP, "\t\tcase p.Deleted && !absentMode(p.NewMode):")],
  run(PURE, "a deleted file that still has a mode"))
OLDMODE = "\t\tcase !p.Created && p.OldMode != FileModeRegular:"
m("DP-OLDMODE", "MP-DOCSPURE: what the file WAS is not checked", [never(DP, OLDMODE)], run(PURE, "it WAS executable (the change drops the bit)"))
m("DP-OLDMODE", "MP-DOCSPURE: deleting an executable is prose-only", [never(DP, OLDMODE)], run(PURE, "deleting an executable"))
m("DP-OLDMODE", "MP-DOCSPURE: a created file may have had a mode", [never(DP, "\t\tcase p.Created && !absentMode(p.OldMode):")],
  run(PURE, "a created file that had a mode"))
m("DP-OLDMODE", "MP-DOCSPURE: `0` is not an absent side", [(DP, 'return mode == "0" || mode == "000000"', 'return mode == "000000"')],
  run(PURE, "a created Markdown file (old mode 0)"))
m("DP-OLDMODE", "MP-DOCSPURE: `000000` is not an absent side", [(DP, 'return mode == "0" || mode == "000000"', 'return mode == "0"')],
  run(PURE, "a created Markdown file (old mode 000000)"))
REN = "\t\tcase p.Renamed && (p.OldClass != scope.ClassDocs || !proseName(p.OldPath, exts)):"
m("DP-RENAME", "MP-DOCSPURE: a rename's old path is not checked", [never(DP, REN)], run(PURE, "docs/tool.py renamed to docs/tool.md"))
m("DP-RENAME", "MP-DOCSPURE: a rename's old CLASS is not checked", [(DP, REN, "\t\tcase p.Renamed && !proseName(p.OldPath, exts):")],
  run(PURE, "a source file renamed into docs"))
m("DP-RENAME", "MP-DOCSPURE: a rename's old EXTENSION is not checked", [(DP, REN, "\t\tcase p.Renamed && p.OldClass != scope.ClassDocs:")],
  run(PURE, "docs/tool.py renamed to docs/tool.md"))
m("DP-RENAME", "MP-DOCSPURE: an old path on a non-rename is ignored",
  [never(DP, '\t\tcase !p.Renamed && (p.OldPath != "" || p.OldClass != ""):')], run(PURE, "an old path on a file that is not a rename"))

# ================================== M6 ==================================
m("M6.no-candidate-pipeline", "M6: no pipeline at all is green", [off(D, "\tif len(ev.Pipelines) == 0 {")], alone(HX, "M6 no candidate pipeline"))
m("M6.pipeline-wrong-sha", "M6: a pipeline of another commit counts", [off(D, "\t\tif p.SHA != e.f.JudgedSHA {")],
  alone(HC, "M6 a pipeline of another commit"))
m("M6.pipeline-not-success", "M6: a running pipeline is green", [off(D, "\t\tif p.Status != PipelineSuccess {")],
  alone(HC, "M6 the pipeline is still running"))
m("M6.pipeline-not-success", "M6: only the FIRST candidate pipeline is judged",
  [(D, "for _, p := range ev.Pipelines {", "for _, p := range ev.Pipelines[:1] {")], alone(HC, "M6 a second candidate pipeline failed"))
m("M6.failed-attempt", "M6: a failed attempt inside a green pipeline is ignored", [off(D, "\t\tif p.FailedAttempts != 0 {")],
  alone(HX, "M6 a green pipeline with a failed attempt in it"))
OBS = "\tif e.f.Kind == KindPipefix && ev.DiagnosedObservations < 2 {"
m("M6.observed-once", "M6: one observation of the failure is enough", [off(D, OBS)], alone(HC, "M6 the diagnosed failure was seen once"))
m("M6.observed-once", "M6: one observation of the failure is enough (< 1)", [(D, OBS, OBS.replace("< 2", "< 1"))],
  alone(HD, "M6 the diagnosed failure was seen once"))

# ================================== M7 ==================================
MOVED = "\tif !fullSHA(t.HeadSHA) || t.HeadSHA != t.JudgedBaseSHA {"
m("M7.target-moved", "M7: a moved target merges", [off(D, MOVED)], alone(HC, "M7 the target moved"))
m("M7.target-moved", "M7: two equal non-SHAs are an unmoved target", [(D, MOVED, "\tif t.HeadSHA != t.JudgedBaseSHA {")],
  alone(HC, "M7 both SHAs abbreviated alike"))

# ================================== M8 ==================================
HMODE = "\tif e.cfg.Mode != ModeAfterHumanApproval {"
m("M8.approvals-insufficient", "M8: after-human-approval asks for no approval", [(D, HMODE, "\tif true {")], alone(HC, "M8 nobody approved"))
m("", "M8: after-agentic-review also asks for a human approval", [off(D, HMODE)], run(BASE, AC))
m("M8.approval-age-unobservable", "M8: an undatable approval on a two-commit MR counts",
  [off(D, "\tif !a.AgeObservable && a.CommitCount != 1 {")], alone(HC, "M8 approval age not observable, two commits"))
m("M8.approval-age-unobservable", "M8: an unknown commit count is one commit",
  [(D, "\tif !a.AgeObservable && a.CommitCount != 1 {", "\tif !a.AgeObservable && a.CommitCount > 1 {")],
  alone(HC, "M8 approval age not observable, commit count unknown"))
m("M8.approvals-insufficient", "M8: RequiredApprovals 0 means zero humans", [off(D, "\tif need < 1 {")], run('TestRequiredApprovalsIsNeverBelowOne'))
m("M8.approvals-insufficient", "M8: the approval count is not checked", [off(D, "\tif len(counted) < need {")], alone(HC, "M8 nobody approved"))
m("M8.approvals-insufficient", "M8: one person approving twice is two approvals",
  [(D, "\tif len(counted) < need {", "\tif len(a.Approvals)-len(excluded) < need {")],
  alone(HC, "M8 two approvals required, one person approved twice"))
m("M8.approvals-insufficient", "[NAMED] M8: the bot's own approval counts", [never(D, "\tcase ap.UserID == bot:")], run(BOT, HC))
m("M8.approvals-insufficient", "[NAMED] M8: the bot's own approval counts (single fact)", [never(D, "\tcase ap.UserID == bot:")],
  alone(HD, "M8 only the bot approved"))
m("M8.approvals-insufficient", "M8: with the bot's id unknown every approver counts", [never(D, "\tcase bot == 0:")],
  alone(HC, "M2 the bot's user id is unknown, so no approver is provably not the bot"))
m("M8.approvals-insufficient", "M8: an approver with no id counts", [never(D, "\tcase ap.UserID == 0:")], alone(HC, "M8 an approver with no user id"))
m("M8.approvals-insufficient", "M8: a non-member's approval counts", [never(D, "\tcase !ap.Member:")], alone(HC, "M8 the approver is not a member"))
m("M8.approvals-insufficient", "M8: a reporter's approval counts", [never(D, "\tcase ap.AccessLevel < AccessDeveloper:")],
  alone(HC, "M8 the approver is a reporter"))
m("M8.approvals-insufficient", "M8: a developer's approval does not count",
  [(D, "\tcase ap.AccessLevel < AccessDeveloper:", "\tcase ap.AccessLevel <= AccessDeveloper:")], run(BASE, HC))
m("M8.approvals-insufficient", "M8: an approval older than the head commit counts",
  [never(D, "\tcase !ap.ApprovedAt.After(a.HeadCommittedAt):")], alone(HC, "M8 the approval is older than the head commit"))
m("M8.approvals-insufficient", "M8: an approval as old as the head commit counts",
  [(D, "\tcase !ap.ApprovedAt.After(a.HeadCommittedAt):", "\tcase ap.ApprovedAt.Before(a.HeadCommittedAt):")],
  alone(HC, "M8 the approval is as old as the head commit"))
m("M8.approvals-insufficient", "M8: with no head commit time every approval is newer", [never(D, "\tcase a.HeadCommittedAt.IsZero():")],
  alone(HC, "M8 the head commit's time is unknown"))
m("M8.approval-age-unobservable", "M8: approval dates are demanded where they cannot be observed", [never(D, "\tcase !a.AgeObservable:")],
  run('TestApprovalAgeNotObservable'))

# ================================== M9 ==================================
m("M9.blocking", "[NAMED] M9: a blocking review merges under after-human-approval (no veto at all)", [off(D, "\tif blocking > 0 {")], run(VETO, HC))
m("M9.blocking", "[NAMED] M9: a blocking review merges under after-human-approval (the veto is read under after-agentic-review only)",
  [(D, "\tif blocking > 0 {", "\tif blocking > 0 && e.cfg.Mode == ModeAfterAgenticReview {")], run(VETO, HC))
m("M9.blocking", "M9: a blocking review beside a clean one merges under after-agentic-review", [off(D, "\tif blocking > 0 {")],
  alone(AC, "M9 a blocking review of this head"))
m("M9.blocking", "M9: a `blocking` verdict is not recognised",
  [(D, "\t\tcase ReviewBlocking:\n\t\t\tblocking++", "\t\tcase \"never\":\n\t\t\tblocking++")], alone(HC, "M9 a blocking review by the author's own model still vetoes"))
m("M9.verdict-unknown", "M9: a review with an undefined verdict is ignored", [off(D, '\tif unknown != "" {')],
  alone(HC, "M9 a review of this head with a verdict nobody defined"))
m("M9.no-review-fact", "M9: a review of an older head counts", [off(D, "\t\tif r.HeadSHA != e.f.JudgedSHA {")],
  alone(AC, "M9 the review is of an older head"))
m("", "M9: a blocking review of an older head vetoes this one", [off(D, "\t\tif r.HeadSHA != e.f.JudgedSHA {")], run(VETO, HC))
AMODE = "\tif e.cfg.Mode != ModeAfterAgenticReview {"
m("M9.no-review-fact", "M9: after-agentic-review asks for no review", [(D, AMODE, "\tif true {")], alone(AC, "M9 no review fact"))
m("", "M9: after-human-approval also asks for a review", [off(D, AMODE)], run(BASE, HC))
m("M9.reviews-not-enabled", "M9: actions.reviews is not required", [off(D, "\tif !e.f.Review.ReviewsEnabled {")], alone(AC, "M9 actions.reviews is off"))
NOFACT = "\tif len(clean) == 0 {\n"
m("M9.no-review-fact", "M9: no review fact is not its own refusal", [off(D, "\tif len(clean) == 0 {")], alone(AC, "M9 no review fact"))
LBL = [(D, NOFACT, '\tif strings.Contains(strings.Join(e.f.MR.Labels, " "), "gonk::review-no-blocking") {\n\t\treturn\n\t}\n' + NOFACT)]
m("M9.no-review-fact", "[NAMED] M9: a label without a review fact merges (gonk::review-no-blocking stands in for the fact)", LBL, run(LABEL, AC))
m("M9.no-review-fact", "[NAMED] M9: a label without a review fact merges (single fact)", LBL, alone(AD, "M9 a label and no review fact"))
SAMEM = [(D, "\t\tcase IndependenceDistinctModel:\n", "\t\tcase IndependenceDistinctModel, IndependenceSameModel:\n"),
         (D, "\t\tcase IndependenceSameModel:\n", '\t\tcase "never":\n')]
m("M9.same-model", "[NAMED] M9: same-model merges (a same-model review satisfies after-agentic-review)", SAMEM, run(SAME, AC))
m("M9.same-model", "[NAMED] M9: same-model merges (single fact)", SAMEM, alone(AD, "M9 the reviewer ran on the author's model"))
m("M9.same-model", "[NAMED] M9: same-model merges (equal model names read as distinct)", [off(R, "\tif strings.EqualFold(a, r) {")], run(SAME, AC))
m("M9.same-model", "M9: same-model is reported as model-unknown", [off(D, '\tif same != "" {')],
  alone(AC, "M9 the reviewer ran on the author's model"))
m("M9.same-model", "M9: the same model in another case is a distinct model", [(R, "\tif strings.EqualFold(a, r) {", "\tif a == r {")],
  alone(AC, "M9 the same model, spelled in another case"))
m("M9.same-model", "M9: the same model with surrounding space is a distinct model",
  [(R, "a, r := strings.TrimSpace(authorModel), strings.TrimSpace(reviewerModel)", "a, r := authorModel, reviewerModel")], run(INDEP))
m("M9.model-unknown", "M9: an unrecorded model is a distinct model", [off(R, '\tif a == "" || r == "" {')],
  alone(AC, "M9 the reviewer's model was not recorded"))
m("M9.model-unknown", "M9: only the reviewer's model need be recorded", [(R, '\tif a == "" || r == "" {', '\tif r == "" {')],
  alone(AC, "M9 the author's model was not recorded"))

# ============================== M10, M11 ==============================
m("M10.not-mergeable", "M10: GitLab's merge status is not read",
  [off(D, "\tif s := e.f.Status.DetailedMergeStatus; s != StatusMergeable {")], alone(HC, "M10 not_approved"))
m("M10.not-mergeable", "M10: an empty merge status is mergeable",
  [(D, "\tif s := e.f.Status.DetailedMergeStatus; s != StatusMergeable {", '\tif s := e.f.Status.DetailedMergeStatus; s != StatusMergeable && s != "" {')],
  alone(HC, "M10 no status"))
RATE = "\tif n := MergesInWindow(e.f.Rate.RecentMerges, e.now); n >= e.cfg.MaxPerDay {"
m("M11.rate-cap-reached", "M11: the rate cap is not applied", [off(D, RATE)], alone(HC, "M11 a third merge in the last 24 h"))
m("M11.rate-cap-reached", "M11: max_per_day merges, then one more", [(D, RATE, RATE.replace("n >= e.cfg", "n > e.cfg"))],
  alone(HC, "M11 a third merge in the last 24 h"))
m("M11.rate-cap-reached", "M11: max_per_day 0 is no cap", [(D, RATE, RATE.replace("n >= e.cfg.MaxPerDay", "e.cfg.MaxPerDay > 0 && n >= e.cfg.MaxPerDay"))],
  alone(HC, "M11 max_per_day is 0"))
WIN = "\t\tif m.IsZero() || !m.Before(start) {"
m("M11.rate-cap-reached", "M11: an undated merge is not counted", [(R, WIN, "\t\tif !m.Before(start) {")], run(WINDOW))
m("M11.rate-cap-reached", "M11: a merge exactly 24 h ago is not counted", [(R, WIN, "\t\tif m.IsZero() || m.After(start) {")], run(WINDOW))
m("M11.rate-cap-reached", "M11: a merge dated after now is not counted", [(R, WIN, "\t\tif m.IsZero() || (!m.Before(start) && !m.After(now)) {")], run(WINDOW))
m("M11.rate-cap-reached", "M11: the window is 12 h", [(F, "RateWindow = 24 * time.Hour", "RateWindow = 12 * time.Hour")], run(WINDOW))
m("M11.rate-cap-reached", "M11: the window is 12 h (through Decide)", [(F, "RateWindow = 24 * time.Hour", "RateWindow = 12 * time.Hour")],
  alone(HC, "M11 max_per_day is 2"))

# ============================ the Decision itself ============================
m("", "Decision: the merge is not pinned to a SHA", [(D, "\td.SHA = f.JudgedSHA\n", '\td.SHA = ""\n')], run(PIN))
m("", "Decision: a merge names no class", [(D, "\td.Class = class\n", "")], run(BASE, HD))
m("", "Decision: a merge has no rule code", [(D, "\td.Rule = RuleMerge\n", "")], run(BASE, HC))
m("", "Decision: the deciding rule is the LAST failure",
  [(D, "d.Rule, d.Text = e.failed[0].Rule, e.failed[0].Text", "d.Rule, d.Text = e.failed[len(e.failed)-1].Rule, e.failed[len(e.failed)-1].Text")], run(ORDER))
m("", "Decision: only the deciding failure is listed", [(D, "\t\td.Failed = e.failed\n", "\t\td.Failed = e.failed[:1]\n")], run(ORDER))
m("", "Decision: M10 is evaluated before M2 (not the plan's order)",
  [(D, "\te.m2()\n", "\te.m10()\n\te.m2()\n"), (D, "\te.m9()\n\te.m10()\n", "\te.m9()\n")], run(ORDER))
m("", "Decision: a refusal carries the mode ''", [(D, "\td := Decision{Mode: cfg.Mode}\n", "\td := Decision{}\n")], run(BASE, HC))

# ============================ constants and tables ============================
m("", "constants: Developer is access level 20", [(F, "AccessDeveloper = 30", "AccessDeveloper = 20")], run('TestConstantsAgreeWithTheirOwners'))
m("", "constants: the hold label is `gonk::holding`", [(F, 'HoldLabel = "gonk::hold"', 'HoldLabel = "gonk::holding"')], alone(HC, "M2 a human set gonk::hold"))
m("", "constants: `can_be_merged` is the yes", [(F, 'StatusMergeable = "mergeable"', 'StatusMergeable = "can_be_merged"')],
  alone(HC, "M10 the old merge_status spelling"))
m("", "constants: M10 carries A8, so A9 has no fixture (the plan-row test must notice)",
  [(RU, '\tcase "M10":\n\t\treturn "A9"', '\tcase "M10":\n\t\treturn "A8"')], run('TestEveryPlanARowHasAFixture'))
m("", "constants: a rule is missing from Rules() (the listing test must notice)",
  [(RU, "\tM7Unread, M7Stale, M7TargetMoved,\n", "\tM7Unread, M7Stale,\n")], run('TestRulesListsEveryCodeOnce'))
m("", "constants: Rules() is not in the plan's order (the listing test must notice)",
  [(RU, "\tM7Unread, M7Stale, M7TargetMoved,\n\tM8Unread, M8Stale, M8AgeUnobservable, M8ApprovalsInsufficient,\n",
        "\tM8Unread, M8Stale, M8AgeUnobservable, M8ApprovalsInsufficient,\n\tM7Unread, M7Stale, M7TargetMoved,\n")],
  run('TestRulesListsEveryCodeOnce'))
M10ROWS = ['\t{name: "M10 the merge status was not read", code: M10Unread, do: func(f *Facts, c *Config) { f.Status.ReadAt = time.Time{} }},\n',
           '\t{name: "M10 the merge status was read 61 s ago", code: M10Stale, do: func(f *Facts, c *Config) { f.Status.ReadAt = stale }},\n',
           '\t{name: "M10 not_approved", code: M10NotMergeable, do: func(f *Facts, c *Config) { f.Status.DetailedMergeStatus = "not_approved" }},\n',
           '\t{name: "M10 no status", code: M10NotMergeable, do: func(f *Facts, c *Config) { f.Status.DetailedMergeStatus = "" }},\n',
           '\t{name: "M10 the old merge_status spelling", code: M10NotMergeable, do: func(f *Facts, c *Config) { f.Status.DetailedMergeStatus = "can_be_merged" }},\n']
DROP_M10 = [(DT, row, "") for row in M10ROWS]
m("PLAN", "test table: row M10 loses every fixture (the epic plan-row test must notice)", DROP_M10, run('TestEveryPlanMRowHasAFixture'))
m("", "test table: row M10 loses every fixture (the eleven-rows test must notice)", DROP_M10, run('TestTheFixturesCoverElevenRows'))
m("", "test table: row M10 loses every fixture (the fix plan-row test must notice: A9)", DROP_M10, run('TestEveryPlanARowHasAFixture'))
m("", "test table: a rule code loses its only fixture (the per-code test must notice)",
  [(DT, M10ROWS[1], "")], run('TestEveryRuleCodeHasAFixtureInSomeScenario'))
m("", "purity: the package reads the clock (the contract test must notice)",
  [(D, "\te := &eval{f: f, cfg: cfg, now: now}\n", "\te := &eval{f: f, cfg: cfg, now: now}\n\t_ = time.Now()\n")], run('TestThePackageIsPure'))
m("", "purity: the package imports the GitLab client (the contract test must notice)",
  [(R, 'import (\n\t"strings"\n\t"time"\n)', 'import (\n\t"strings"\n\t"time"\n\n\t_ "github.com/gonk-factory/gonk/pkg/glab"\n)')],
  run('TestThePackageIsPure'))


# ------------------------------------------------- every code has a mutation
def declared_codes():
    rules = re.findall(r'RuleCode = "(M\d+\.[^"]+)"', open(RU).read())
    dps = re.findall(r'DocsPureCode = "(DP-[^"]+)"', open(DP).read())
    return rules + dps


covered = set(c for _, _, _, codes in muts for c in codes)
missing = [c for c in declared_codes() if c not in covered]
unknown = sorted(covered - set(declared_codes()) - {"PLAN"})
if missing or unknown:
    print("the mutation list does not match the code:\n  codes with NO mutation: %s\n  codes named by a mutation and not declared: %s"
          % (missing, unknown))
    sys.exit(2)

# ------------------------------------------------------------------ the runner
if '--list' in sys.argv[1:]:
    for i, (name, _, pattern, codes) in enumerate(muts):
        print(f"[{i}] {name}    {' '.join(codes)}    -run {pattern}")
    sys.exit(0)

show = '--show' in sys.argv[1:]
only = [a for a in sys.argv[1:] if a != '--show']
have_plan = os.path.exists(PLAN)

base = subprocess.run([GO, "test", "./pkg/mergepolicy/", "-count=1"], capture_output=True, text=True)
if base.returncode != 0:
    print("the UNMUTATED package is not green; nothing was mutated:\n" + base.stdout + base.stderr)
    sys.exit(2)

res = []
for i, (name, edits, pattern, codes) in enumerate(muts):
    if only and str(i) not in only:
        continue
    if "PLAN" in codes and not have_plan:
        res.append(f"NOPLAN   [{i}] {name}  (no {PLAN}: the guarding test has nothing to read)")
        print(res[-1], flush=True)
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
        r = subprocess.run([GO, "test", "./pkg/mergepolicy/", "-run", pattern, "-count=1"],
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
