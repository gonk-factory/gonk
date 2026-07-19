package integration_test

import (
	"fmt"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

// TestInfraFailuresNeverEscalate is the single most important behavioural claim
// after "budgets are hard": the rung index is the count of prior GATE failures.
// An infra failure retries the SAME rung, up to max_infra_retries, then DENIES --
// it must never silently promote a project to its most expensive model.
func TestInfraFailuresNeverEscalate(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local", "glm", "sonnet")) // unlimited budget

	for i := 1; i <= 3; i++ { // three consecutive infra failures (max_infra_retries = 3)
		s := w.Session("gk-1", fmt.Sprintf("s%d", i), atags.TriggerIssueTriage)
		d := s.Decide(t)
		if d.Decision != "run" || d.Rung != "qwen-local" {
			t.Fatalf("attempt %d ESCALATED/refused on infra: %+v", i, d)
		}
		s.Report(t, meterapi.OutcomeInfraFailed)
	}
	// Every attempt at the cheapest rung. Not one climb.
	ledger.AssertAttempts(t, w.Views, "gk-1", []ledger.Attempt{
		{N: 1, Rung: "qwen-local", Outcome: "infra-failed"},
		{N: 2, Rung: "qwen-local", Outcome: "infra-failed"},
		{N: 3, Rung: "qwen-local", Outcome: "infra-failed"},
	})
	// After max_infra_retries: DENY, not a silent climb to sonnet.
	d := w.Session("gk-1", "s4", atags.TriggerIssueTriage).Decide(t)
	ledger.AssertDenied(t, d, meterapi.ReasonInfraRetriesExhausted)
}

// TestJanitorInfraFailureDoesNotEscalate is the meter-side infra path: a session
// that dies silently (its reservation TTL expires) gets an infra-failed attempt
// from the janitor and retries the SAME rung -- not a climb.
func TestJanitorInfraFailureDoesNotEscalate(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local", "glm", "sonnet"))

	for i := 1; i <= 3; i++ {
		s := w.Session("gk-1", fmt.Sprintf("s%d", i), atags.TriggerIssueTriage)
		d := s.Decide(t)
		if d.Decision != "run" || d.Rung != "qwen-local" {
			t.Fatalf("attempt %d ESCALATED/refused on a dead session: %+v", i, d)
		}
		w.Clock.Advance(61 * time.Minute) // past reservation_ttl (60m); no Report
		w.Meter.RunJanitor(t)
	}
	ledger.AssertAttempts(t, w.Views, "gk-1", []ledger.Attempt{
		{N: 1, Rung: "qwen-local", Outcome: "infra-failed"},
		{N: 2, Rung: "qwen-local", Outcome: "infra-failed"},
		{N: 3, Rung: "qwen-local", Outcome: "infra-failed"},
	})
	d := w.Session("gk-1", "s4", atags.TriggerIssueTriage).Decide(t)
	ledger.AssertDenied(t, d, meterapi.ReasonInfraRetriesExhausted)
}

// TestGateFailureEscalatesExactlyOneRung: a GENUINE outcome-gate failure (the
// model completed and produced no artifact) escalates EXACTLY ONE rung. No LLM
// judged anything; the gate's verdict is a pure function of the reported outcome.
func TestGateFailureEscalatesExactlyOneRung(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local", "glm", "sonnet"))
	w.Model.SetScript(successScript("prose only, no artifact.", 1000, 250))

	w.Session("gk-1", "s1", atags.TriggerIssueTriage).Run(t, 1, meterapi.OutcomeGateFailed)
	w.Session("gk-1", "s2", atags.TriggerIssueTriage).Run(t, 1, meterapi.OutcomeGateFailed)
	d := w.Session("gk-1", "s3", atags.TriggerIssueTriage).Decide(t)
	if d.Rung != "sonnet" || d.Attempt != 3 {
		t.Fatalf("decision = %+v, want sonnet at attempt 3", d)
	}
}

// TestInterleavedFailuresCountOnlyGateFailures is the one that catches the subtle
// bug: gate, infra, gate. The rung index must be 2, not 3. An implementation that
// counted `attempt` instead of `gate failures` passes every test above and fails
// this one.
func TestInterleavedFailuresCountOnlyGateFailures(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local", "glm", "sonnet"))
	w.Model.SetScript(successScript("interleaved.", 1000, 250))

	w.Session("gk-1", "s1", atags.TriggerIssueTriage).Run(t, 1, meterapi.OutcomeGateFailed) // -> glm next

	s2 := w.Session("gk-1", "s2", atags.TriggerIssueTriage)
	if d := s2.Decide(t); d.Rung != "glm" {
		t.Fatalf("attempt 2 rung = %q, want glm", d.Rung)
	}
	s2.Report(t, meterapi.OutcomeInfraFailed) // -> still glm

	s3 := w.Session("gk-1", "s3", atags.TriggerIssueTriage)
	if d := s3.Decide(t); d.Rung != "glm" {
		t.Fatalf("rung = %q after gate+infra, want glm (an infra failure must not advance the index)", d.Rung)
	}
	s3.Call(t, 1)
	s3.Report(t, meterapi.OutcomeGateFailed) // -> sonnet

	if d := w.Session("gk-1", "s4", atags.TriggerIssueTriage).Decide(t); d.Rung != "sonnet" {
		t.Fatalf("rung = %q, want sonnet", d.Rung)
	}
	ledger.AssertAttempts(t, w.Views, "gk-1", []ledger.Attempt{
		{N: 1, Rung: "qwen-local", Outcome: "gate-failed"},
		{N: 2, Rung: "glm", Outcome: "infra-failed"},
		{N: 3, Rung: "glm", Outcome: "gate-failed"},
	})
}
