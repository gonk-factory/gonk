package main

// gonk-2usw part EPOCH (plan 3.3), END TO END BELOW THE LIVE CLUSTER, on the
// same in-process world as TestPipelineRoundTrip (pipeline_roundtrip_test.go):
// real webhook receiver, real intake over the fake GitLab, the order on the
// wire, real gonk-gate dispatch reading it from the environment, a session,
// the real sweep and the landing read back.
//
// The sequence is the one owner requirement R3 turns on:
//
//	A   the target fails; gonk diagnoses it.
//	fix gonk's fix MR for that run MERGES into the target.
//	A'  the same job fails again, with a DIFFERENT error.
//
// A' has A's fingerprint (stage, name, failure_reason) and A's last green
// pipeline. Without the fix epoch it therefore has A's anchor, the landing
// says "diagnosed", and the failure the fix revealed is never looked at.
//
// SYNTHETIC AND NEUTRAL (plan section 0): a made-up project, a made-up unit
// test, made-up errors. No fix MR is created by gonk here -- no part that
// writes one exists yet -- so the merged MR is seeded into the fake GitLab
// with exactly the facts the lookup reads: author, source branch, target
// branch, state, merged_at, commits.

import (
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/intake"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

const (
	rtFixIID    = 40
	rtFixBranch = "gonk/pipefix-0123456789ab"
	rtFixMerge  = "4444444444444444444444444444444444444444"
	rtFixHead   = "5555555555555555555555555555555555555555"

	// rtAfterFixLog: the same job, the same failure_reason, a different error.
	rtAfterFixLog = "$ ./run-tests\n" +
		"collecting tests\n" +
		"test_addition ... ok\n" +
		"test_sum ... FAILED\n" +
		"TypeError: total() takes 1 positional argument but 2 were given\n" +
		"1 failed, 1 passed\n" +
		"ERROR: Job failed: exit code 1\n"
	rtAfterFixLine = "TypeError: total() takes 1 positional argument but 2 were given"
)

// rtPipelineCreated is when the fake GitLab says pipeline id was created (it
// stamps an unset CreatedAt as a fixed day plus one minute per id).
func rtPipelineCreated(id int64) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Minute)
}

// mergeFix seeds gonk's merged fix MR into the world's target branch, merged
// at the given instant.
func (w *rtWorld) mergeFix(iid int64, at time.Time) {
	w.gl.AddMR(w.proj.ID, glab.MergeRequest{
		IID: iid, Title: "gonk: fix unit-tests on " + w.fixTargetBranch(), State: "merged",
		SourceBranch: rtFixBranch, TargetBranch: w.fixTargetBranch(),
		Author: glab.User{ID: plBotID, Username: "gonk"}, MergedAt: &at,
		MergeCommitSHA: rtFixMerge, SHA: rtFixHead,
	})
}

// fixTargetBranch is the branch a fix for the world's target merges into: the
// default branch, or the MR's source branch.
func (w *rtWorld) fixTargetBranch() string {
	if w.mr {
		return rtMRBranch
	}
	return "main"
}

func TestPipelineRoundTripFixEpoch(t *testing.T) {
	for _, mr := range []bool{false, true} {
		name := "default-branch"
		if mr {
			name = "merge-request"
		}
		t.Run(name, func(t *testing.T) { roundTripFixEpoch(t, mr) })
	}
}

func roundTripFixEpoch(t *testing.T, mr bool) {
	w := newRTWorld(t, mr)
	pid := w.proj.ID
	u := w.proj.WebURL

	// ---- A: the target fails and is diagnosed --------------------------------
	w.failPipeline(900, map[string]string{"unit-tests": plTestLog})
	oA := w.deliver(900, "failed").requireOrder(t)
	anchorA := oA["bead_anchor"]
	// (Read the epoch with AnchorFixEpoch, never by looking for ":f": a
	// fingerprint may itself begin with an f.)
	if e, err := pipelinectx.AnchorFixEpoch(anchorA); err != nil || e != 0 {
		t.Fatalf("run A anchor %s: fix epoch %d, %v; want none", anchorA, e, err)
	}
	if _, has := oA["pipeline_fix_epoch"]; has {
		t.Fatalf("a run that follows no fix carries a fix epoch var: %v", oA)
	}
	fp := pipelinectx.Fingerprint([]pipelinectx.FailedJob{{Stage: "test", Name: "unit-tests", FailureReason: "script_failure"}}, "")
	if want, _ := pipelinectx.Anchor(pid, w.target, fp, 880, 0); anchorA != want {
		t.Fatalf("run A anchor = %s, want %s", anchorA, want)
	}
	if code := w.dispatch(oA); code != 0 {
		t.Fatalf("dispatch A exit = %d\n%s", code, w.gateLog)
	}
	sA := sessionName(pid, 900)
	w.answer(sA, rtDiagnosis(effects.ClassCode, "unit-tests", plGoodLine, nil), plGoodLine)
	w.sweep()
	if st, _ := w.runState(anchorA); st != pipelinectx.RunStateDiagnosed {
		t.Fatalf("run A state = %s, want diagnosed\n%s", st, w.gateLog)
	}
	if got := w.sessionNames(); len(got) != 1 {
		t.Fatalf("sessions after A = %v, want one", got)
	}

	// ---- the fix merges -------------------------------------------------------
	// Between pipeline 900 (run A) and pipeline 910 (the first one after it).
	w.mergeFix(rtFixIID, rtPipelineCreated(905))

	// Run A's own pipeline, delivered again AFTER the merge: still run A, still
	// handled. Meter is not asked and no second session diagnoses the failure
	// the fix was written for.
	calls := w.gate1.calls()
	w.deliver(900, "failed").requireDropped(t, intake.ReasonRunAlreadyHandled)
	if w.gate1.calls() != calls {
		t.Fatal("meter was asked about run A's own pipeline after the fix merged")
	}

	// ---- A': the same job fails again, differently ----------------------------
	w.failPipeline(910, map[string]string{"unit-tests": rtAfterFixLog})
	dB := w.deliver(910, "failed")
	if len(dB.orders) == 0 {
		t.Fatalf("after merged fix !%d, the same fingerprint was NOT a new run: intake dropped pipeline 910 and no second diagnosis can happen.\nintake log:\n%s",
			rtFixIID, dB.log)
	}
	oB := dB.requireOrder(t)
	anchorB := oB["bead_anchor"]
	if oB["pipeline_fingerprint"] != oA["pipeline_fingerprint"] || oB["pipeline_last_success"] != oA["pipeline_last_success"] {
		t.Fatalf("the premise is broken: A' must have A's fingerprint and last success: %v vs %v", oB, oA)
	}
	if anchorB == anchorA {
		t.Fatalf("after merged fix !%d, the same fingerprint yields the SAME anchor %s", rtFixIID, anchorB)
	}
	if want := anchorA + ":f40"; anchorB != want || oB["pipeline_fix_epoch"] != "40" || oB["pipeline_id"] != "910" {
		t.Fatalf("run A' order = %v, want anchor %s, pipeline_fix_epoch 40, pipeline 910", oB, want)
	}
	if oB["session_key"] == oA["session_key"] {
		t.Fatalf("run A' has run A's session key %s", oB["session_key"])
	}
	if code := w.dispatch(oB); code != 0 {
		t.Fatalf("dispatch A' exit = %d: gonk-gate refused the order of the run after the fix\n%s", code, w.gateLog)
	}
	sB := sessionName(pid, 910)
	if got := w.sessionNames(); len(got) != 2 || got[1] != sB {
		t.Fatalf("sessions = %v, want a second session %s for the run after the fix", got, sB)
	}
	if st, _ := w.runState(anchorB); st != pipelinectx.RunStateDiagnosing {
		t.Fatalf("run A' note state = %s after dispatch, want diagnosing", st)
	}

	// The second diagnosis: the agent quotes the NEW error from the prompt it
	// was started with, and the sweep lands it.
	w.answer(sB, rtDiagnosis(effects.ClassCode, "unit-tests", rtAfterFixLine, nil), rtAfterFixLine)
	w.sweep()
	if oc := w.om.requests(); len(oc) != 2 || oc[1].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcomes = %+v, want two successes (one per run)\n%s", oc, w.gateLog)
	}
	stB, bodyB := w.runState(anchorB)
	if stB != pipelinectx.RunStateDiagnosed || !strings.Contains(bodyB, rtAfterFixLine) {
		t.Fatalf("run A' note = %s, want diagnosed quoting the new error:\n%s", stB, bodyB)
	}
	if !strings.Contains(firstLine(bodyB), plLink(u, 910)) {
		t.Fatalf("run A' note does not lead with pipeline 910:\n%s", bodyB)
	}
	recB := w.record(anchorB)
	if recB.PipelineFixEpoch != rtFixIID || recB.PipelineID != 910 || recB.PipelineLastSuccess != 880 || !recB.PipelineRunOpen {
		t.Fatalf("run A' record = %+v, want fix epoch %d on pipeline 910 since 880, open", recB, rtFixIID)
	}
	if recA := w.record(anchorA); recA.PipelineFixEpoch != 0 || recA.PipelineID != 900 {
		t.Fatalf("run A's record changed: %+v", recA)
	}
	// Run A's diagnosis is still on the landing, beside the new one.
	if st, body := w.runState(anchorA); st != pipelinectx.RunStateDiagnosed || !strings.Contains(body, plGoodLine) {
		t.Fatalf("run A's note after A' landed: %s\n%s", st, body)
	}
	if n := len(w.runNotes()); n != 2 {
		t.Fatalf("run notes = %d, want one per run (2)", n)
	}

	// ---- A' again: handled by ITS marker, not asked again ---------------------
	w.failPipeline(920, map[string]string{"unit-tests": rtAfterFixLog})
	calls = w.gate1.calls()
	w.deliver(920, "failed").requireDropped(t, intake.ReasonRunAlreadyHandled)
	if w.gate1.calls() != calls {
		t.Fatal("meter was asked about run A' although its landing says diagnosed")
	}
	if got := w.sessionNames(); len(got) != 2 {
		t.Fatalf("sessions = %v, want exactly two: one diagnosis per run", got)
	}

	// Run maintenance looks at both open runs and neither loses its marker.
	w.tick()
	if st, _ := w.runState(anchorB); st != pipelinectx.RunStateDiagnosed {
		t.Fatalf("run A' after maintenance: %s", st)
	}
	stA, _ := w.runState(anchorA)
	t.Logf("run A after the fix merged and A' was diagnosed: note state %q, record open=%v (closing it is part VERIFY's, plan 7.4)",
		stA, w.record(anchorA).PipelineRunOpen)
	w.requireNoIssueTriage()
}
