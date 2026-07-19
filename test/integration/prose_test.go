package integration_test

import (
	"context"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// rowSnap is one ledger row, reduced to the facts that MUST be prose-independent:
// attribution, tokens, and the two currencies. Reservation ids (random) and wall
// timestamps are excluded on purpose.
type rowSnap struct {
	Project, Rig, Bead, Session, Rung, Trigger string
	Attempt                                    int
	Prompt, Completion                         int64
	Cost                                       float64
	Synthetic                                  bool
}

// decSnap is one decision the scenario observed.
type decSnap struct {
	Bead, Decision, Rung, Reason string
	Attempt                      int
}

// scenarioResult is the whole L1 money path's OBSERVABLE outcome, with nothing
// from the model's prose in it.
type scenarioResult struct {
	Rows      []rowSnap
	Decisions []decSnap
}

// runAllScenarios drives the L1 money path (a local $0 path and a cloud
// escalation path) with a fixed canned prose, and returns a prose-free snapshot
// of the ledger and every decision. The whole point: prose changes here must not
// change one byte of the result.
func runAllScenarios(t *testing.T, prose string) scenarioResult {
	t.Helper()
	w := NewWorld(t)
	var decisions []decSnap
	record := func(bead string, d meterapi.DecideResponse) {
		decisions = append(decisions, decSnap{Bead: bead, Decision: d.Decision, Rung: d.Rung, Reason: d.Reason, Attempt: d.Attempt})
	}

	// --- the LOCAL $0-budget path (the brick scenario's currency): two successes.
	onboard(t, w, "acme/local", withLadder("qwen-local"), withBudget(0))
	w.Model.SetScript(successScript(prose, 1000, 250))
	for _, bead := range []string{"loc-1", "loc-2"} {
		s := w.SessionFor("acme/local", bead, bead+"-s", atags.TriggerScaffold)
		record(bead, s.Run(t, 2, meterapi.OutcomeSuccess))
	}

	// --- the CLOUD escalation path: a gate failure climbs qwen-local -> glm.
	onboard(t, w, "acme/cloud", withLadder("qwen-local", "glm"), withBudget(10))
	s1 := w.SessionFor("acme/cloud", "cl-1", "cl-1-s1", atags.TriggerIssueTriage)
	record("cl-1", s1.Run(t, 1, meterapi.OutcomeGateFailed)) // qwen-local, attempt 1
	s2 := w.SessionFor("acme/cloud", "cl-1", "cl-1-s2", atags.TriggerIssueTriage)
	record("cl-1", s2.Run(t, 1, meterapi.OutcomeSuccess)) // glm, attempt 2

	w.SyncSpend(t)

	rows, err := w.LLM.Rows(context.Background())
	if err != nil {
		t.Fatalf("read spend rows: %v", err)
	}
	snaps := make([]rowSnap, 0, len(rows))
	for _, r := range rows {
		snaps = append(snaps, rowSnap{
			Project: r.Tags.Project, Rig: r.Tags.Rig, Bead: r.Tags.BeadID, Session: r.Tags.SessionKey,
			Rung: r.Tags.Rung, Trigger: r.Tags.Trigger, Attempt: r.Tags.Attempt,
			Prompt: r.PromptTokens, Completion: r.CompletionTokens, Cost: r.CostUSD, Synthetic: r.Synthetic,
		})
	}
	sort.Slice(snaps, func(i, j int) bool {
		a, b := snaps[i], snaps[j]
		if a.Bead != b.Bead {
			return a.Bead < b.Bead
		}
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Attempt != b.Attempt {
			return a.Attempt < b.Attempt
		}
		return a.Rung < b.Rung
	})
	return scenarioResult{Rows: snaps, Decisions: decisions}
}

// TestNoAssertionDependsOnModelProse is AD-4, proven mechanically rather than
// promised: run the whole L1 scenario set twice with DIFFERENT canned prose
// (identical tool calls, identical usage) and require the ledger and every
// outcome to be byte-identical. The local $0 money path is included, so this
// also pins that synthetic-priced local spend is prose-independent.
func TestNoAssertionDependsOnModelProse(t *testing.T) {
	a := runAllScenarios(t, "I have triaged this issue.")
	b := runAllScenarios(t, "znork blimp 42 ¡¡ <script>alert(1)</script>")
	if diff := cmp.Diff(a, b); diff != "" {
		t.Fatalf("an assertion depends on model prose:\n%s", diff)
	}
	// Non-vacuity: the scenarios actually produced ledger rows and decisions.
	if len(a.Rows) == 0 || len(a.Decisions) == 0 {
		t.Fatalf("scenario produced nothing to compare (rows=%d decisions=%d)", len(a.Rows), len(a.Decisions))
	}
}
