#!/usr/bin/env python3
"""mutate_pipefix_verify.py -- "seen red" evidence for gonk-2usw part VERIFY:
the exit check of a pipeline fix (cmd/gonk-gate/sweep_pipefix_verify.go), the
post-merge confirmation (sweep_pipefix_confirm.go, pkg/pipefix/confirm.go),
the fix note's forward-only rule, the deadlines that learned verify_timeout,
and the next attempt's feedback.

Same contract as hack/mutate_pipefix.py: one named break at a time, ONLY the
test that guards it is run (in the package named with it) and must FAIL; every
file is restored byte for byte. A search text that does not occur EXACTLY once
is VACUOUS, a break the named test does not notice SURVIVED, one that does not
compile NOCOMPILE, and a break that names a subtest is only KILLED when a
subtest is among the failures -- each of those fails this script. The
unmutated packages must be green first.

The breaks the task names are marked [NAMED]:
  NOT_FIXED reported as success
  REGRESSED escalates
  a pipeline still running yields a verdict
  a human retry yields PASSED
  the outcome is reported twice
  a verifying record is swept as stale
  a cannot-judge record lives past its bound
  the note moves backwards
  two state labels at once
  the second attempt opens a second MR instead of adopting
  `confirmed` when the diagnosed job did not run
  the ended run is not closed

Usage: hack/mutate_pipefix_verify.py            (all)
       hack/mutate_pipefix_verify.py 3 7        (only these indexes)
       hack/mutate_pipefix_verify.py --list
       hack/mutate_pipefix_verify.py --show 3   (also the full `go test` output)
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
PIPEFIX = './pkg/pipefix/'
GLABTEST = './pkg/glab/glabtest/'
V = 'cmd/gonk-gate/sweep_pipefix_verify.go'
C = 'cmd/gonk-gate/sweep_pipefix_confirm.go'
T = 'cmd/gonk-gate/pipefix_verify_text.go'
N = 'cmd/gonk-gate/pipefix_note.go'
D = 'cmd/gonk-gate/broker_pipefix_dispatch.go'
A = 'cmd/gonk-gate/broker_pipefix_apply.go'
SW = 'cmd/gonk-gate/sweep.go'
SP = 'cmd/gonk-gate/sweep_pipeline.go'
SF = 'cmd/gonk-gate/sweep_pipefix.go'
L = 'cmd/gonk-gate/pipeline_landing.go'
J = 'pkg/pipefix/judge.go'
CF = 'pkg/pipefix/confirm.go'
GL = 'pkg/glab/glabtest/server.go'


def run(test, *subs):
    parts = ['^%s$' % test]
    for s in subs:
        parts.append('^%s$' % re.escape(s.replace(' ', '_')))
    return '/'.join(parts)


muts = []


def m(name, edits, pattern, pkg=GATE):
    muts.append((name, edits, pattern, pkg))


def off(src, line):
    """Disable an `if [init; ]<cond> {` line: the condition is ANDed with
    false, so every name the line declares or uses stays used (a plain
    `if false {` does not compile when the body needs the init's variable)."""
    body = line.lstrip('\t')
    assert body.startswith('if ') and ' {' in body, line
    indent = line[:len(line) - len(body)]
    head, tail = body[3:body.rindex(' {')], body[body.rindex(' {') + 2:]
    init, cond = ('', head)
    if '; ' in head:
        init, cond = head.rsplit('; ', 1)
        init += '; '
    return (src, line + '\n', indent + 'if ' + init + '(' + cond + ') && false {' + tail + '\n')


TABLE = 'TestTheExitCheckSettlesEveryDecision'
NOTFIXED = 'TestNotFixedIsGateFailedAndReslungWithFeedback'
REGRESSED = 'TestRegressedIsNotAnEscalation'
RUNNING = 'TestAPipelineStillRunningYieldsNoVerdict'
THROTTLE = 'TestTheExitCheckLooksAtMostEveryTwoMinutes'
RETRY = 'TestAHumanRetryDoesNotYieldFixed'
STALE = 'TestAVerifyingRecordIsNotSweptAsStale'
FALLBACK = 'TestTheFallbackDeadlineLearnsVerifyTimeout'
UNCLAIMED = 'TestAnUnclaimedPipefixSessionIsNotStaleBeforeItsOwnDeadline'
BOUND = 'TestACannotJudgeRecordEndsAtItsBound'
AWAIT = 'TestAwaitingRunBecomesVerifyingOrTimesOut'
ONCE = 'TestTheOutcomeIsReportedOncePerAttempt'
NOTERULE = 'TestFixNoteMayReplaceNeverMovesANoteBackwards'
NOTEGL = 'TestTheVerdictNoteIsNotMovedBackAndAHumansNoteIsNeverEdited'
LABEL = 'TestExactlyOneStateLabelAtATime'
UPD = 'TestUpdateFixMRTouchesOnlyItsOwnStatusBlock'
RUNLINE = 'TestTheRunNoteCarriesTheFixLine'
SECOND = 'TestTheSecondAttemptCommitsOnTopAndAdoptsTheMergeRequest'
MAXATT = 'TestNotFixedAtMaxAttemptsEndsDenied'
MOVED = 'TestNotFixedIsNotReslungAgainstARunThatMoved'
DENY = 'TestADenyAfterNotFixedUpdatesTheNoteAndTheMergeRequest'
NOAUTO = 'TestNoAutoMergeReasonsAreRecordedOnTheResult'
FACTS = 'TestFixFactsOfCopiesTheRecord'
OWN = 'TestTheExitCheckLeavesStatesItDoesNotOwn'
MERGE = 'TestAfterTheMergeTheTargetsNextPipelineDecides'
REOPEN = 'TestAConfirmedFixWhoseRunStayedOpenIsClosedByTheNextLook'
INERT = 'TestThePostMergeLookIsInertWhereItShouldBe'
E2E = 'TestPipelineFixRoundTripToAConfirmedFixAndASequencedFollowUp'
LEDGER = 'TestTheRealMetersLedgerHoldsOneRowPerAttemptWithThePlansOutcome'
CONFIRM = 'TestConfirmRows'
UNREADC = 'TestNoUnreadFactIsEverConfirmed'
MRHEAD = 'TestAnOpenMergeRequestsHeadFollowsItsSourceBranch'

# =============================== the named breaks ===============================
SIG = "\tsignals := gate.Signals{ArtifactPresent: present}\n"
m("[NAMED] NOT_FIXED reported as success", [(V, SIG, "\tsignals := gate.Signals{ArtifactPresent: true}\n")], run(NOTFIXED))
m("[NAMED] NOT_FIXED reported as success, on the real meter's ledger",
  [(V, SIG, "\tsignals := gate.Signals{ArtifactPresent: true}\n")], run(LEDGER, "denied (not-fixed at max_attempts)"))
m("[NAMED] REGRESSED escalates: it is reported as a failed attempt, not success",
  [(V, SIG, "\tsignals := gate.Signals{ArtifactPresent: present && s.state != pipefix.StateRegressed}\n")], run(REGRESSED))
m("[NAMED] REGRESSED escalates: it is re-slung like a not-fixed attempt",
  [(V, "\tfinal, retry, whyNot := s.state, false, \"\"\n\tif s.state == pipefix.StateNotFixed {",
       "\tfinal, retry, whyNot := s.state, false, \"\"\n\tif s.state == pipefix.StateNotFixed || s.state == pipefix.StateRegressed {")],
  run(REGRESSED))
m("[NAMED] a pipeline still running yields a verdict", [(J, "\t\tif !settled && !past {\n", "\t\tif false {\n")],
  run(RUNNING, "the pipeline is running"))
m("[NAMED] a pipeline still running yields a verdict: `failed` with its retry not yet run (gonk-5b2f)",
  [(J, "\t\t\tif a, found := activeAttempt(c); found {\n\t\t\t\tsettled = false\n", "\t\t\tif a, found := activeAttempt(c); found {\n")],
  run(RUNNING, "the pipeline is failed and its retry is still created (gonk-5b2f)"))
m("[NAMED] a human retry yields PASSED",
  [(J, "\t\tif same && f.Succeeded > 0 {\n\t\t\treturn RuleJ3FlakySame\n", "\t\tif same && f.Succeeded > 0 {\n\t\t\treturn RuleJ3Passed\n")], run(RETRY))
m("[NAMED] the outcome is reported twice: the settled record is not written, so every look settles again",
  [(V, "\t\trec.State = beadstore.StateDone\n\t\tv.put(ctx, rec, string(final))\n", "\t\trec.State = beadstore.StateDone\n")],
  run(ONCE, "a verdict is reported once across many ticks"))
m("[NAMED] the outcome is reported twice: on the real meter the ledger must still hold one row, and the record must be settled",
  [(V, "\t\trec.State = beadstore.StateDone\n\t\tv.put(ctx, rec, string(final))\n", "\t\trec.State = beadstore.StateDone\n")],
  run(LEDGER, "fixed"))
m("[NAMED] a verifying record is swept as stale: the exit check's bound is the base reservation TTL",
  [(V, "\treturn deadline.Add(pipefixJudgeGrace)\n", "\treturn rec.UpdatedAt.Add(opercfg.DefaultReservationTTL)\n")], run(STALE))
m("[NAMED] a cannot-judge record lives past its bound", [off(V, "\t\tif !now.Before(fixVerifyBound(rec)) { // verify:bound")], run(BOUND))
m("[NAMED] the note moves backwards (the rule)", [(N, "\t\treturn want > have // note:verify-forward", "\t\treturn true // note:verify-forward")],
  run(NOTERULE))
m("[NAMED] the note moves backwards (on GitLab)", [(N, "\t\treturn want > have // note:verify-forward", "\t\treturn true // note:verify-forward")],
  run(NOTEGL))
m("[NAMED] two state labels at once: the old one is not removed",
  [off(V, "\t\t\tif l != want && slices.Contains(mr.Labels, l) { // mr:one-label")], run(LABEL))
m("[NAMED] two state labels at once, seen by the table", [off(V, "\t\t\tif l != want && slices.Contains(mr.Labels, l) { // mr:one-label")],
  run(TABLE, "FIXED"))
m("[NAMED] the second attempt opens a second MR instead of adopting",
  [(A, "\tcase open != nil:\n\t\treturn open, \"open\", nil\n", "\tcase open != nil:\n\t\treturn nil, \"\", nil\n")], run(SECOND))
m("[NAMED] `confirmed` when the diagnosed job did not run (the rule)",
  [(CF, "firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3Changed, RuleJ3FlakyChanged)", "firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3Changed, RuleJ3FlakyChanged, RuleJ3Absent)")],
  run(CONFIRM, "GREEN, and the diagnosed job did not run at all"), PIPEFIX)
m("[NAMED] `confirmed` when the diagnosed job did not run (on the target)",
  [(CF, "firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3Changed, RuleJ3FlakyChanged)", "firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3Changed, RuleJ3FlakyChanged, RuleJ3Absent)")],
  run(MERGE, "green, but the diagnosed job DID NOT RUN: not confirmed"))
m("[NAMED] the ended run is not closed", [off(C, "\t\tif !green { // confirm:close-run")],
  run(MERGE, "red for a different reason: confirmed, and THE RUN THE FIX ENDED IS CLOSED"))
m("[NAMED] the ended run is not closed, end to end (the same fingerprint is then not a new run's alone)",
  [off(C, "\t\tif !green { // confirm:close-run")], run(E2E, "default-branch"))

# ============================ the exit check: facts ============================
m("facts: a target without a signature is not generic", [(V, "\t\t\tGeneric: t.Generic || t.ErrorSignature == \"\",\n", "\t\t\tGeneric: t.Generic,\n")], run(FACTS))
m("facts: the target's stage is dropped", [(V, "Key: pipelinectx.JobFailureKey{Stage: t.Stage, Name: t.Name,", "Key: pipelinectx.JobFailureKey{Name: t.Name,")], run(FACTS))
m("facts: the target's signature is dropped",
  [(V, "FailureReason: t.FailureReason, ErrorSignature: t.ErrorSignature},", "FailureReason: t.FailureReason},")], run(FACTS))
m("facts: the base jobs are dropped", [(V, "\t\tBaseJobs:         base,\n", "")], run(FACTS))
m("facts: a base job's status is dropped", [(V, "pipefix.BaseJob{Name: j.Name, Status: j.Status, AllowFailure: j.AllowFailure}", "pipefix.BaseJob{Name: j.Name, AllowFailure: j.AllowFailure}")], run(FACTS))
m("facts: a record the apply never finished reads as recorded",
  [(V, "pf.Recorded() && rec.Trigger == atags.TriggerPipelineFix && pf.AppliedAttempt > 0,", "pf.Recorded() && rec.Trigger == atags.TriggerPipelineFix,")], run(FACTS))
m("facts: a record of another trigger reads as recorded",
  [(V, "pf.Recorded() && rec.Trigger == atags.TriggerPipelineFix && pf.AppliedAttempt > 0,", "pf.Recorded() && pf.AppliedAttempt > 0,")], run(FACTS))
m("facts: the verify deadline is dropped", [(V, "\t\tVerifyDeadline:   pf.VerifyDeadline,\n", "")], run(FACTS))
m("facts: ci_exec_changes is dropped", [(V, "\t\tCIExecChanges:    pf.CIExecChanges,\n", "")], run(FACTS))
m("facts: the base commit is the fix commit", [(V, "\t\tFixBaseSHA:       pf.BaseSHA,\n", "\t\tFixBaseSHA:       pf.FixCommitSHA,\n")], run(FACTS))
m("facts: the signature versions are dropped", [(V, "\t\tSignatureVersion: pf.SignatureVersion,\n", "")], run(FACTS))

# ============================ the exit check: looks ============================
m("look: no throttle, every tick reads GitLab", [off(V, "\tif !pf.JudgedAt.IsZero() && now.Sub(pf.JudgedAt) < pipefixJudgeEvery { // verify:throttle")], run(THROTTLE))
m("look: the throttle never ends", [(V, "now.Sub(pf.JudgedAt) < pipefixJudgeEvery { // verify:throttle", "now.Sub(pf.JudgedAt) < 240*pipefixJudgeEvery { // verify:throttle")], run(THROTTLE))
m("look: a state the exit check does not own is judged", [off(V, "\tif !fixOpenStates[pf.State] {")], run(OWN))
m("look: the wait is not stamped", [(V, "\trec := v.rec\n\trec.PipelineFix.JudgedAt = v.now\n", "\trec := v.rec\n")], run(RUNNING, "no pipeline yet"))
m("bound: without a verify deadline the bound is the zero time's", [(V, "\t\tdeadline = runningDeadline(rec) // verify:bound-fallback", "\t\tdeadline = time.Time{} // verify:bound-fallback")], run(FACTS))
m("bound: no grace", [(V, "\treturn deadline.Add(pipefixJudgeGrace)\n", "\treturn deadline\n")], run(BOUND))
m("bound: the bound's rule is not named",
  [(V, "\t\t\tif res.Decision == pipefix.DecisionCannotJudge {\n\t\t\t\trule = ruleVerifyBoundCannotJudge\n\t\t\t}\n", "")], run(BOUND))
m("bound: past the bound a GitLab that cannot be written still holds the settlement",
  [(V, "\tif !shown && v.now.Before(fixVerifyBound(rec)) { // verify:gitlab-first", "\tif !shown { // verify:gitlab-first")], run(BOUND))
m("look: every waiting look reads and rewrites the landing", [(V, "\tif v.landingBehind(ctx, rec, state) { // verify:landing-behind", "\tif v.landingBehind(ctx, rec, state) || true { // verify:landing-behind")],
  run(RUNNING, "the pipeline is running"))
m("look: the landing is never brought up to date by a waiting look", [off(V, "\tif v.landingBehind(ctx, rec, state) { // verify:landing-behind")], run(RUNLINE))
m("look: only the merge request is compared, not the state shown",
  [(V, "\treturn run.PipelineRunFixMR != rec.PipelineFix.MRIID || run.PipelineRunFixState != string(state)\n", "\treturn run.PipelineRunFixMR != rec.PipelineFix.MRIID\n")],
  run(AWAIT, "a maintainer starts the pipeline"))
m("awaiting-run: a started pipeline does not make it verifying",
  [off(V, "\tif state == pipefix.StateAwaitingRun && res.Decision == pipefix.DecisionWait && len(res.Candidates) > 0 { // verify:started")],
  run(AWAIT, "a maintainer starts the pipeline"))
m("awaiting-run: a skipped pipeline that ran nothing makes it verifying",
  [(V, "res.Decision == pipefix.DecisionWait && len(res.Candidates) > 0 { // verify:started", "res.Decision == pipefix.DecisionWait { // verify:started")],
  run(AWAIT, "a maintainer starts the pipeline"))

# ============================ the exit check: writes ============================
m("write: the fix note is not written",
  [(V, "\tchanged, ld, nerr := lander.writeFixNote(ctx, run, rec.BeadAnchor, note) // verify:note",
       "\tchanged, ld, nerr := false, runLanding{}, error(nil) // verify:note\n\t_ = note")], run(TABLE, "FIXED"))
# (No mutant for `Verify: !open` on the settlement's note: in THIS path the
# existing note is always rank 0 or 1 -- the record is still verifying -- and
# an apply-flagged note may replace exactly those too, so flipping the flag
# changes nothing a test could see. The flag matters for the post-merge
# writes, which have their own mutants below.)
m("write: gonk::needs-human is not put on the landing", [(V, "\tcase changed && fixStateNeedsHuman(state): // verify:needs-human", "\tcase changed && false: // verify:needs-human")],
  run(TABLE, "FLAKY: a human retried the failed job and it passed"))
m("write: gonk::needs-human is put on the landing of a fixed fix",
  [(V, "\tcase changed && fixStateNeedsHuman(state): // verify:needs-human", "\tcase changed: // verify:needs-human")], run(TABLE, "FIXED"))
m("write: the merge request is not updated",
  [(V, "\tif merr := updateFixMR(ctx, v.f, lander.me, rec, state, statusText); merr != nil { // verify:mr", "\tif merr := error(nil); merr != nil { // verify:mr")],
  run(TABLE, "FIXED"))
m("write: the run note's fix line is not written",
  [(V, "\tif _, lerr := writeRunFixLine(ctx, v.d, lander, run, pf.MRIID, state); lerr != nil { // verify:run-line", "\tif lerr := error(nil); lerr != nil { // verify:run-line")],
  run(RUNLINE))
m("write: the verdict is reported although GitLab could not be told",
  [off(V, "\tif !shown && v.now.Before(fixVerifyBound(rec)) { // verify:gitlab-first")], run(ONCE, "GitLab first: a verdict that cannot be shown is not reported yet"))
m("write: a failed fix note does not fail the show", [(V, "\tcase nerr != nil:\n\t\tv.d.Log.Warn(\"sweep: pipeline fix: the fix note could not be written\", \"bead\", rec.BeadAnchor, \"state\", string(state), \"err\", nerr)\n\t\tok = false\n",
                                                        "\tcase nerr != nil:\n\t\tv.d.Log.Warn(\"sweep: pipeline fix: the fix note could not be written\", \"bead\", rec.BeadAnchor, \"state\", string(state), \"err\", nerr)\n")],
  run('TestAFailedFixNoteHoldsTheSettlement'))
m("write: a failed meter report still settles the record",
  [(V, "\t\td.Log.Error(\"sweep: POST /v1/policy/outcome failed for a judged pipeline fix; the next look repeats the settlement\",\n\t\t\t\"err\", err, \"bead\", rec.BeadAnchor, \"outcome\", outcome)\n\t\treturn\n",
       "\t\td.Log.Error(\"sweep: POST /v1/policy/outcome failed for a judged pipeline fix; the next look repeats the settlement\",\n\t\t\t\"err\", err, \"bead\", rec.BeadAnchor, \"outcome\", outcome)\n\t\tresp = &meterapi.OutcomeResponse{}\n")],
  run(ONCE, "a failed report leaves the fix verifying and is repeated"))
m("record: the outcome and its attempt are not recorded", [(V, "\tpf.Outcome, pf.OutcomeAttempt = outcome, attempt\n", "")], run(TABLE, "FIXED"))
m("record: the verdict is not recorded",
  [(V, "\tpf.Verdict, pf.VerdictRule, pf.VerdictDetail, pf.VerdictAttempt = string(s.verdict), s.rule, oneLine(s.detail, 600), attempt\n",
       "\tpf.VerdictDetail = oneLine(s.detail, 600)\n")], run(TABLE, "FIXED"))
m("record: a settled fix stays running", [(V, "\t\tpf.State = string(final)\n\t\trec.State = beadstore.StateDone\n", "\t\tpf.State = string(final)\n")], run(TABLE, "FIXED"))
m("record: a settled fix keeps the state `verifying`", [(V, "\t\tpf.State = string(final)\n\t\trec.State = beadstore.StateDone\n", "\t\trec.State = beadstore.StateDone\n")],
  run(TABLE, "FIXED"))
m("outcome: a not-fixed attempt whose tokens cannot be shown is gate-failed (an unpaid escalation)",
  [(V, "\t\tsignals.ModelTokens, signals.SpendStale = tokens, stale\n", "\t\tsignals.ModelTokens, signals.SpendStale = max(tokens, 1), stale && false\n")],
  run(NOTFIXED, "without proven tokens it is infra-failed"))
m("outcome: the report carries no reservation", [(V, "Attempt: rec.Attempt, Rung: rec.Rung, ReservationID: rec.ReservationID, Outcome: outcome,", "Attempt: rec.Attempt, Rung: rec.Rung, Outcome: outcome,")],
  run(TABLE, "FIXED"))
m("outcome: the report carries no trigger", [(V, "\t\tTrigger: rec.Trigger,\n\t})\n\tif err != nil {\n\t\td.Log.Error(\"sweep: POST /v1/policy/outcome failed for a judged",
                                               "\t})\n\tif err != nil {\n\t\td.Log.Error(\"sweep: POST /v1/policy/outcome failed for a judged")], run(TABLE, "FIXED"))

# ============================ the next attempt ============================
m("retry: max_attempts is not enforced", [off(V, "\tif pf.MaxAttempts > 0 && rec.Attempt >= pf.MaxAttempts { // verify:max-attempts")], run(MAXATT))
m("retry: one attempt more than max_attempts", [(V, "rec.Attempt >= pf.MaxAttempts { // verify:max-attempts", "rec.Attempt > pf.MaxAttempts { // verify:max-attempts")], run(MAXATT))
m("retry: a fix whose run moved is re-slung", [(V, "\tif stale := fixRunStale(run, found, rec); stale != \"\" { // verify:run-stale", "\tif stale := fixRunStale(run, found, rec); stale != \"\" && false { // verify:run-stale")],
  run(MOVED))
m("retry: the fix state is not cleared (gonk-dispatch then refuses the next attempt)",
  [(V, "\t\tpf.State = \"\" // verify:clear-state", "\t\tpf.State = string(pipefix.StateNotFixed) // verify:clear-state")], run(SECOND))
m("retry: the first commit is forgotten (the next apply cannot build on it)",
  [(V, "\t\tpf.State = \"\" // verify:clear-state", "\t\tpf.State, pf.FixCommitSHA = \"\", \"\" // verify:clear-state")], run(SECOND))
m("retry: the merge request is forgotten", [(V, "\t\tpf.State = \"\" // verify:clear-state", "\t\tpf.State, pf.MRIID = \"\", 0 // verify:clear-state")], run(TABLE, "NOT_FIXED: the diagnosed job fails the same way"))
m("retry: the re-sling is not stamped (the next tick would judge and fire again)",
  [(V, "\t\trec.ReslungAt = d.Now()\n\t\trec.ReslingFireBy = reslingFireBy(rec.ReslungAt)\n\t\tif !v.put(ctx, rec, \"re-sling\") {", "\t\tif !v.put(ctx, rec, \"re-sling\") {")], run(NOTFIXED))
m("retry: the session's end is not cleared", [(V, "\t\trec.SessionEndedAt = time.Time{}\n\t\trec.ReslungAt = d.Now()\n", "\t\trec.ReslungAt = d.Now()\n")], run(NOTFIXED))
m("retry: no order is fired", [(V, "\t\tif refire(ctx, d, rec) {\n\t\t\td.Log.Info(\"sweep: pipeline fix re-sling fired\"", "\t\tif false {\n\t\t\td.Log.Info(\"sweep: pipeline fix re-sling fired\"")], run(NOTFIXED))
m("retry: the order is fired although the record write was lost",
  [(V, "\t\tif !v.put(ctx, rec, \"re-sling\") {\n\t\t\treturn // no fire after a lost or failed write (gonk-p8w8, gonk-aavs)\n\t\t}\n", "\t\tv.put(ctx, rec, \"re-sling\")\n")],
  run('TestALostReslingWriteFiresNoOrder'))
m("retry: no feedback is recorded", [(V, "\t\tpf.Feedback = fixFeedback(s)\n", "")], run(NOTFIXED))
m("retry: the feedback does not carry the rule code", [(V, "out := fmt.Sprintf(\"%s by rule %s: %s\", s.verdict, s.rule, why)", "out := fmt.Sprintf(\"%s: %s\", s.verdict, why)")], run(NOTFIXED))
m("retry: the feedback does not carry the job findings", [(V, "\tif len(jobs) > 0 {\n\t\tout += \" (\" + strings.Join(jobs, \"; \") + \")\"\n\t}\n", "")], run(NOTFIXED))
m("retry: the look's stamp is kept, so the next attempt's first look is late", [(V, "\t\tpf.JudgedAt = time.Time{}\n", "")], run(SECOND))
m("retry: the next attempt is not told what happened", [off(D, "\tif pf.Feedback != \"\" && pf.FixCommitSHA != \"\" { // dispatch:previous")], run(SECOND))
m("retry: the previous change is shown with its lines swapped",
  [(D, "\t\t\tb.WriteString(\"-\" + l + \"\\n\")\n", "\t\t\tb.WriteString(\"+\" + l + \"\\n\")\n")], run(SECOND))
m("retry: the previous change is shown as the whole file", [(D, "\tfor len(al) > 0 && len(bl) > 0 && al[0] == bl[0] {\n", "\tfor false {\n")], run('TestChangedLines'))
m("denied: the record is `done`, not needs-human", [(V, "\t\trec.State = beadstore.StateNeedsHuman\n\t\tv.put(ctx, rec, \"denied\")", "\t\trec.State = beadstore.StateDone\n\t\tv.put(ctx, rec, \"denied\")")], run(MAXATT))
m("denied: the fix state stays not-fixed", [(V, "\t\tif !retry {\n\t\t\tfinal = pipefix.StateDenied\n\t\t}\n", "")], run(MAXATT))
m("denied at dispatch: the landing still says trying again", [off(N, "\tif existing == string(pipefix.StateNotFixed) && n.State == string(pipefix.StateDenied) { // note:denied-after-not-fixed")], run(DENY))
m("denied at dispatch: the merge request still says verifying", [off(D, "\tif n.State == string(pipefix.StateDenied) && fix.PipelineFix.MRIID > 0 { // dispatch:denied-label")], run(DENY))

# ============================ what must never auto-merge ============================
m("auto-merge: weakening not-decidable is not recorded", [off(V, "\tif rec.PipelineFix.Weakening != beadstore.FixWeakeningDecided { // automerge:weakening")], run(NOAUTO))
m("auto-merge: an unset weakening fact counts as decided",
  [(V, "rec.PipelineFix.Weakening != beadstore.FixWeakeningDecided { // automerge:weakening", "rec.PipelineFix.Weakening == beadstore.FixWeakeningNotDecidable { // automerge:weakening")],
  run(NOAUTO, "the pure function"))
m("auto-merge: an exec-changing definition is not recorded", [off(V, "\tif rec.PipelineFix.CIDelta == string(cidef.KindExecChanging) || s.res.Definition == cidef.KindExecChanging { // automerge:exec")], run(NOAUTO, "the pure function"))
m("auto-merge: only the RECORDED delta counts, not the judged one",
  [(V, "rec.PipelineFix.CIDelta == string(cidef.KindExecChanging) || s.res.Definition == cidef.KindExecChanging { // automerge:exec", "rec.PipelineFix.CIDelta == string(cidef.KindExecChanging) { // automerge:exec")],
  run(NOAUTO, "the pure function"))
m("auto-merge: a verdict other than fixed is not recorded", [off(V, "\tif final != pipefix.StateFixed { // automerge:verdict")], run(TABLE, "FLAKY: a human retried the failed job and it passed"))
m("auto-merge: the caution is not on the note", [(T, "\tif pf.Weakening == beadstore.FixWeakeningNotDecidable {\n", "\tif false {\n")], run(NOAUTO, "weakening not decidable (no predicate for the language)"))

# ============================ the merge request ============================
m("mr: a merge request another user authored is edited", [off(V, "\tif mr.Author.ID != me.ID || mr.SourceBranch != branch || mr.SourceProjectID != rec.ProjectID || mr.TargetProjectID != rec.ProjectID { // mr:ours")],
  run(UPD, "a merge request that is not the bot's is not edited"))
m("mr: a merged merge request is edited", [off(V, "\tif mr.State == pipefix.MRStateMerged || state == pipefix.StateMerged { // mr:merged")], run(UPD, "a merged merge request is not edited"))
m("mr: a closing pattern reaches the description", [off(V, "\t\tif m := closingPattern.FindString(desc); m != \"\" { // mr:closing")], run(UPD, "a closing pattern is refused"))
m("mr: the status block is appended instead of replaced", [(V, "\treturn description[:open] + block + rest[end+len(fixStatusClose):]\n", "\treturn description + block\n")], run(UPD))
m("mr: a description without the block is left without one", [(V, "\tif open < 0 {\n\t\treturn block + \"\\n\\n\" + description\n\t}\n", "\tif open < 0 {\n\t\treturn description\n\t}\n")],
  run(UPD, "a description that lost its block gets it back at the top"))
m("mr: it is rewritten on every look", [(V, "\tif u.Description == nil && len(u.AddLabels) == 0 && len(u.RemoveLabels) == 0 {\n\t\treturn nil\n\t}\n", "")], run(LABEL))
m("label: flaky is shown as verified", [(V, "pipefix.StateTakenOver, pipefix.StateClosedUnmerged:\n\t\treturn labelFixInconclusive\n", "pipefix.StateTakenOver, pipefix.StateClosedUnmerged:\n\t\treturn labelFixVerified\n")],
  run(TABLE, "FLAKY: a human retried the failed job and it passed"))
m("label: denied is shown as inconclusive", [(V, "\tcase pipefix.StateDenied:\n\t\treturn labelFixFailed\n", "\tcase pipefix.StateDenied:\n\t\treturn labelFixInconclusive\n")], run(MAXATT))
m("label: fixed keeps the verifying label", [(V, "\tcase pipefix.StateFixed, pipefix.StateFixedNewFailure:\n\t\treturn labelFixVerified\n", "\tcase pipefix.StateFixed, pipefix.StateFixedNewFailure:\n\t\treturn labelFixVerifying\n")],
  run(TABLE, "FIXED"))
m("label: gonk::fix-new-failure is not added", [(V, "\tcase state == pipefix.StateFixedNewFailure && !has:\n", "\tcase false:\n")], run(TABLE, "FIXED_NEW_FAILURE: the diagnosed job fails differently"))
m("label: the awaiting-run label is kept when verifying starts", [(V, "\tcase pipefix.StateAwaitingRun:\n\t\treturn labelFixAwaitingRun\n", "\tcase pipefix.StateAwaitingRun:\n\t\treturn labelFixVerifying\n")],
  run('TestFixStateLabelPerState'))

# ============================ the run note's fix line ============================
m("run line: it is on the record but not on the note",
  [(V, "\tif ld.note != nil && ld.state == pipelinectx.RunStateDiagnosed && run.PipelineRunOpen {\n", "\tif false {\n")], run(RUNLINE))
m("run line: it is on the note but not on the record", [(V, "\trun.PipelineRunFixMR, run.PipelineRunFixState = mrIID, string(state)\n\tt, err := pipelineRunOf(run)", "\tt, err := pipelineRunOf(run)")], run(RUNLINE))
m("run line: the note's status does not render it", [off(L, "\tif rec.PipelineRunFixMR > 0 {")], run(RUNLINE))
m("run line: it is rewritten on every look", [(V, "\tif run.PipelineRunFixMR == mrIID && run.PipelineRunFixState == string(state) {\n\t\treturn run, nil\n\t}\n", "")], run(RUNLINE))

# ============================ the note's rule ============================
m("note: the exit check's `denied` replaces a verdict", [(N, "\t\t\treturn have <= 1 // note:verify-denied", "\t\t\treturn true // note:verify-denied")], run(NOTERULE))
m("note: the same state cannot be said again", [(N, "\t\tcase n.State == existing:\n\t\t\treturn true\n", "\t\tcase n.State == existing:\n\t\t\treturn false\n")], run(NOTERULE))
m("note: verifying cannot become not-fixed", [(N, "\t\tcase want == 1 && have == 1:\n\t\t\treturn true\n", "\t\tcase want == 1 && have == 1:\n\t\t\treturn false\n")], run(NOTERULE))
m("note: a state this build does not know is rewritten", [(N, "\t\tcase !known || !ok:\n\t\t\treturn false\n", "\t\tcase !known && !ok:\n\t\t\treturn false\n")], run(NOTERULE))
m("note: merged ranks with the verdicts", [(N, "\tcase pipefix.StateMerged:\n\t\treturn 3, true\n", "\tcase pipefix.StateMerged:\n\t\treturn 2, true\n")], run(NOTERULE))
m("note: confirmed ranks with merged", [(N, "\tcase pipefix.StateConfirmed, pipefix.StateRecurred:\n\t\treturn 4, true\n", "\tcase pipefix.StateConfirmed, pipefix.StateRecurred:\n\t\treturn 3, true\n")], run(NOTERULE))
m("note: not-fixed ranks with the verdicts (an apply could not follow it... and a verdict could not replace it)",
  [(N, "\tcase pipefix.StateAwaitingRun, pipefix.StateVerifying, pipefix.StateNotFixed:\n\t\treturn 1, true\n", "\tcase pipefix.StateAwaitingRun, pipefix.StateVerifying, pipefix.StateNotFixed:\n\t\treturn 2, true\n")],
  run(NOTERULE))
m("note: a dispatch-time `denied` replaces anything that is not a dispatch state",
  [(N, "\tif existing == string(pipefix.StateNotFixed) && n.State == string(pipefix.StateDenied) { // note:denied-after-not-fixed", "\tif n.State == string(pipefix.StateDenied) { // note:denied-after-not-fixed")],
  run(NOTERULE))

# ============================ deadlines ============================
m("deadline: the fallback does not learn verify_timeout",
  [(SW, "\treturn rec.UpdatedAt.Add(opercfg.DefaultReservationTTL + fixReservationSlack(rec))\n", "\treturn rec.UpdatedAt.Add(opercfg.DefaultReservationTTL)\n")], run(FALLBACK))
m("deadline: every trigger gets the fix's slack", [off(V, "\tif rec.Trigger != atags.TriggerPipelineFix {")], run(FALLBACK))
m("deadline: the slack is not clamped", [(V, "\treturn min(max(slack, 0), gonkcfg.FixVerifyTimeoutCeiling)\n", "\treturn max(slack, 0)\n")], run(FALLBACK))
m("stale: an unclaimed pipefix session is stale at the base age", [off(SW, "\t\tif strings.HasPrefix(string(h), \"gonk-\"+pipefixAgent+\"-\") && age < staleFixSessionAfter { // stale:pipefix")], run(UNCLAIMED))
m("stale: an unclaimed pipefix session is never stale",
  [(SW, "strings.HasPrefix(string(h), \"gonk-\"+pipefixAgent+\"-\") && age < staleFixSessionAfter { // stale:pipefix", "strings.HasPrefix(string(h), \"gonk-\"+pipefixAgent+\"-\") { // stale:pipefix")], run(UNCLAIMED))
m("stale: every unclaimed session gets the fix's age", [(SW, "strings.HasPrefix(string(h), \"gonk-\"+pipefixAgent+\"-\") && age < staleFixSessionAfter { // stale:pipefix", "age < staleFixSessionAfter { // stale:pipefix")],
  run(UNCLAIMED))
m("wiring: the exit check is not wired in", [(SF, "var pipefixJudge func(ctx context.Context, d sweepDeps, rec beadstore.Record) = judgeOpenFix", "var pipefixJudge func(ctx context.Context, d sweepDeps, rec beadstore.Record)")],
  run(TABLE, "FIXED"))

# ============================ after the merge: the rule (pkg/pipefix) ============================
m("confirm: a recurrence a retry covered is confirmed",
  [(CF, "firstJob(res.Jobs, RuleJ3Recurs, RuleJ3FlakySame); found {", "firstJob(res.Jobs, RuleJ3Recurs); found {")],
  run(CONFIRM, "green only because a human retried the recurring failure: a retry does not launder it"), PIPEFIX)
m("confirm: a recurrence is not recurred", [(CF, "firstJob(res.Jobs, RuleJ3Recurs, RuleJ3FlakySame); found {", "firstJob(res.Jobs, RuleJ3FlakySame); found {")],
  run(CONFIRM, "red, the diagnosed job fails the way it was diagnosed"), PIPEFIX)
m("confirm: a different failure is not confirmed",
  [(CF, "firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3Changed, RuleJ3FlakyChanged)", "firstJobNot(res.Jobs, RuleJ3Passed, RuleJ3FlakyChanged)")],
  run(CONFIRM, "red, the diagnosed job fails DIFFERENTLY (the specific failure is gone)"), PIPEFIX)
m("confirm: an attempt that has not run yet is not waited for", [(CF, "\tif a, active := activeAttempt(p); active {\n\t\tres.Confirmation = ConfirmCannot\n", "\tif a, active := activeAttempt(p); active && false {\n\t\tres.Confirmation = ConfirmCannot\n")],
  run(CONFIRM, "the pipeline is failed and the diagnosed job's retry has not run (gonk-5b2f)"), PIPEFIX)
m("confirm: an unread log is taken for a different failure",
  [(CF, "\tif j, found := firstJob(res.Jobs, RuleJ3Unknown); found {\n\t\tres.Confirmation = ConfirmCannot\n", "\tif j, found := firstJob(res.Jobs, RuleJ3Unknown); found && false {\n\t\tres.Confirmation = ConfirmCannot\n")],
  run(CONFIRM, "the failed attempt's log was not read"), PIPEFIX)
m("confirm: a pipeline still running decides", [(CF, "\tif p.ID == 0 || !pipelinectx.TerminalForHealth(p.Status) {\n", "\tif p.ID == 0 {\n")], run(CONFIRM, "the pipeline is still running"), PIPEFIX)
m("confirm: unlisted jobs are no jobs", [(CF, "\tif !p.JobsRead || !p.IncludeRetried {\n", "\tif !p.IncludeRetried {\n")], run(CONFIRM, "the jobs were not listed"), PIPEFIX)
m("confirm: a listing without retried attempts is enough", [(CF, "\tif !p.JobsRead || !p.IncludeRetried {\n", "\tif !p.JobsRead {\n")],
  run(CONFIRM, "the jobs were listed without retried attempts"), PIPEFIX)
m("confirm: no recorded target is confirmed", [(CF, "\tif len(f.Targets) == 0 {\n", "\tif false {\n")], run(UNREADC), PIPEFIX)
m("confirm: a malformed job object is accepted", [(CF, "\t\tif a.ID == 0 || a.Name == \"\" || a.Status == \"\" {\n\t\t\treturn ConfirmResult{Confirmation: ConfirmCannot,", "\t\tif a.ID == 0 && false {\n\t\t\treturn ConfirmResult{Confirmation: ConfirmCannot,")],
  run(CONFIRM, "a job object has no status"), PIPEFIX)
m("confirm: the recorded signature versions are ignored",
  [(CF, "sameVersions := f.SignatureVersion == pipelinectx.SignatureVersion && f.ExcerptVersion == pipelinectx.ExcerptVersion", "sameVersions := true")],
  run(CONFIRM, "recorded under other signature versions: cannot be compared"), PIPEFIX)
m("confirm: recurred does not select its state", [(CF, "res.Confirmation, res.State = ConfirmRecurred, StateRecurred", "res.Confirmation = ConfirmRecurred")],
  run(CONFIRM, "red, the diagnosed job fails the way it was diagnosed"), PIPEFIX)
m("confirm: a recurrence waits for a job that has not run",
  [(CF, "\t// recurred: any attempt that failed the way it was diagnosed. A retry\n", "\tif _, active := activeAttempt(p); active {\n\t\treturn ConfirmResult{Confirmation: ConfirmCannot, Detail: \"x\"}\n\t}\n\t// recurred: any attempt that failed the way it was diagnosed. A retry\n")],
  run(CONFIRM, "a recurrence is visible although a later job has not run: recurred, at once"), PIPEFIX)

# ============================ after the merge: the pass ============================
HOOK = "\t\tif fixes != nil {\n\t\t\tvar hold bool\n"
m("merge: the pass is not called", [(SP, HOOK, "\t\tif false {\n\t\t\tvar hold bool\n")], run(MERGE, "merged: the note says so and the watch starts"))
m("merge: the pass runs although the fix pass is off", [(SP, HOOK, "\t\tif true {\n\t\t\tvar hold bool\n")], run(INERT, "the fix pass is off: no look at all"))
m("merge: a held run is maintained anyway", [(SP, "\t\t\t\tholdRun(ctx, d, rec, now)\n\t\t\t\tcontinue\n", "\t\t\t\tholdRun(ctx, d, rec, now)\n")], run(MERGE, "only a pipeline AFTER the merge counts, and the first one first"))
m("merge: a run with an unconfirmed merged fix is not held", [(C, "\treturn c.run, c.run.PipelineRunOpen && c.fix.PipelineFix.State == string(pipefix.StateMerged) && !green // confirm:hold", "\treturn c.run, false && green // confirm:hold")],
  run(MERGE, "only a pipeline AFTER the merge counts, and the first one first"))
m("merge: a run is held although the target went green", [(C, "c.fix.PipelineFix.State == string(pipefix.StateMerged) && !green // confirm:hold", "c.fix.PipelineFix.State == string(pipefix.StateMerged) && (green || !green) // confirm:hold")],
  run(MERGE, "green, but the diagnosed job DID NOT RUN: not confirmed"))
m("merge: a held run is looked at on every tick", [(C, "\trun.PipelineCheckedAt = now\n\tif _, err := d.Store.PutIf(ctx, run, readRevision); err != nil", "\tif _, err := d.Store.PutIf(ctx, run, readRevision); err != nil")],
  run('TestAHeldRunIsLookedAtOncePerMaintenanceInterval'))
m("merge: an ended fix is read on every look", [off(C, "\tif !fixWatchedForMerge[st] { // confirm:watched")], run('TestAnEndedFixCostsNoGitLabRequest'))
m("merge: a merge request another user authored is taken for the fix's",
  [off(C, "\tif mr.Author.ID != c.l.me.ID || mr.SourceBranch != branch || mr.SourceProjectID != c.fix.ProjectID || mr.TargetProjectID != c.fix.ProjectID { // confirm:ours")],
  run(INERT, "a merge request that is not the bot's is not watched"))
m("merge: a merged merge request without merged_at is believed", [off(C, "\t\tif mr.MergedAt == nil || mr.MergedAt.IsZero() { // confirm:merged-at")],
  run(INERT, "a merged merge request without merged_at is not believed"))
m("merge: merged_at is not recorded", [(C, "\t\tpf.MergedAt = *mr.MergedAt\n", "")], run(MERGE, "merged: the note says so and the watch starts"))
m("merge: the landed commits are not recorded", [(C, "\t\t\tif sha != \"\" && !slices.Contains(pf.MergedSHAs, sha) {\n", "\t\t\tif false {\n")], run(MERGE, "merged: the note says so and the watch starts"))
m("merge: the merged note is not written", [(C, "\t\t\tc.note(ctx, pipefix.StateMerged, fixMergedText(c.fix, \"\"))\n", "")], run(MERGE, "merged: the note says so and the watch starts"))
m("merge: a closed merge request is not noticed", [(C, "\t\tpf.State = string(pipefix.StateClosedUnmerged)\n\t\tif c.note(", "\t\tif c.note(")], run(MERGE, "closed after its verdict: closed-unmerged"))
m("merge: a pipeline from before the merge decides", [off(C, "\t\tif !landed && (w.CreatedAt.IsZero() || w.CreatedAt.Before(pf.MergedAt)) { // confirm:after-merge")],
  run(MERGE, "only a pipeline AFTER the merge counts, and the first one first"))
m("merge: the landed commit's own pipeline is not taken", [(C, "\t\tlanded := w.SHA != \"\" && slices.Contains(pf.MergedSHAs, w.SHA)\n", "\t\tlanded := false\n")],
  run(MERGE, "a pipeline on the merged commit counts even if created before merged_at"))
m("merge: a later pipeline decides while the first is running", [(C, "\t\tcase slices.Contains(activePipelineStatuses, after[i].Status): // confirm:first", "\t\tcase false: // confirm:first")],
  run(MERGE, "only a pipeline AFTER the merge counts, and the first one first"))
m("merge: a pipeline that decided nothing is asked again for ever", [(C, " || w.ID <= pf.ConfirmPipelineID { // confirm:skip-seen", " { // confirm:skip-seen")],
  run(MERGE, "a pipeline that decided nothing is skipped; the next one decides"))
m("merge: a pipeline that decided nothing is not remembered", [(C, "\t\tpf.ConfirmPipelineID = p.ID\n\t\twaitingOn :=", "\t\twaitingOn :=")], run(MERGE, "a pipeline that decided nothing is skipped; the next one decides"))
m("merge: the confirmation's note is not written",
  [(C, "\tchanged, ld, err := c.l.writeFixNote(ctx, c.run, c.fix.BeadAnchor, fixNote{State: string(state), Text: text, Verify: true}) // confirm:note",
       "\tchanged, ld, err := false, runLanding{}, error(nil) // confirm:note\n\t_ = text")], run(MERGE, "green, the diagnosed job passed: confirmed, and the run resolves"))
m("merge: a recurrence does not ask for a person", [(C, "\tif changed && fixStateNeedsHuman(state) { // confirm:needs-human", "\tif changed && false { // confirm:needs-human")],
  run(MERGE, "the diagnosed job fails the same way: recurred, a person, the run stays open"))
m("merge: recurred is recorded as confirmed", [(C, "\t\tpf.State, pf.ConfirmPipelineID = string(pipefix.StateRecurred), p.ID\n", "\t\tpf.State, pf.ConfirmPipelineID = string(pipefix.StateConfirmed), p.ID\n")],
  run(MERGE, "the diagnosed job fails the same way: recurred, a person, the run stays open"))
m("merge: the ended run's record stays open", [(C, "\trun.PipelineRunOpen = false\n\trev, err := c.d.Store.PutIf(ctx, run, readRevision)", "\trev, err := c.d.Store.PutIf(ctx, run, readRevision)")],
  run(MERGE, "red for a different reason: confirmed, and THE RUN THE FIX ENDED IS CLOSED"))
m("merge: a green target's run is closed here, as superseded", [(C, "\t\tif !green { // confirm:close-run", "\t\tif true { // confirm:close-run")],
  run(MERGE, "green, the diagnosed job passed: confirmed, and the run resolves"))
m("merge: a run whose close was lost is never closed", [(C, "\tif p.Status == pipefix.StatusFailed {\n\t\tc.closeEndedRun(ctx, id)\n", "\tif false {\n\t\tc.closeEndedRun(ctx, id)\n")], run(REOPEN))
m("merge: the run is closed although its note could not be written",
  [(C, "\t\treturn // the next look closes it (closeIfEnded): the fix is already `confirmed`\n", "")], run(REOPEN))

# ============================ the fake ============================
m("fake: an open merge request's head does not follow its branch",
  [(GL, "\t\tif p.MRs[i].SourceBranch == opts.Branch && (p.MRs[i].State == \"opened\" || p.MRs[i].State == \"\") {\n", "\t\tif false {\n")], run(MRHEAD), GLABTEST)
m("fake: a merged merge request's head follows its branch",
  [(GL, "\t\tif p.MRs[i].SourceBranch == opts.Branch && (p.MRs[i].State == \"opened\" || p.MRs[i].State == \"\") {\n", "\t\tif p.MRs[i].SourceBranch == opts.Branch {\n")], run(MRHEAD), GLABTEST)

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
        print(f"the UNMUTATED package {pkg} is not green; nothing was mutated:\n" + base.stdout[-4000:] + base.stderr[-2000:])
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
print(f"killed={len(res)-len(bad)} not-killed={len(bad)} (of {len(res)} run, {len(muts)} defined)")
sys.exit(1 if bad else 0)
