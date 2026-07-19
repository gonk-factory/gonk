package integration_test

import (
	"fmt"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

// TestOnboardingDefaultCanAffordItsOnlyRung is the permanent regression net for
// gonk-2g4 (the zero-cost reservation brick). The onboarding MR ships
// `budget: {monthly_cost_usd: 0}` with a LOCAL-only ladder; local rungs are
// priced SYNTHETICALLY in LiteLLM, but real dollars and synthetic dollars are TWO
// DIFFERENT CURRENCIES and the cost gate reads REAL dollars ONLY. If anyone ever
// unifies those fields (or drops the zero-cost carve-out from ReserveIfFits),
// every freshly-onboarded project bricks itself on arrival. This test is the wall.
//
// It uses the ACTUAL onboarding template (pkg/intake's render.go) and the ACTUAL
// rung catalog (the operator config) -- not hand-written copies, which could not
// catch the template or catalog drifting.
func TestOnboardingDefaultCanAffordItsOnlyRung(t *testing.T) {
	w := NewWorld(t)
	p := w.GitLab.AddProject("acme/fresh", glab.AccessMaintainer)
	cfg, err := intake.RenderDefaultConfig(onboardingLadder) // the ACTUAL onboarding .gonk.yml
	if err != nil {
		t.Fatalf("render default config: %v", err)
	}
	p.PutFile(".gonk.yml", cfg)
	p.PutFile(".agent/context.md", []byte("# context\n")) // valid: no scaffold auto-fire

	w.Reconcile(t)
	w.project, w.rig = "acme/fresh", rigFor("acme/fresh")
	requireMeterState(t, w, "acme/fresh", meterapi.StateActive)

	w.Model.SetScript(successScript("scaffolding.", 1000, 250))

	d := w.Session("gk-0", "s0", atags.TriggerScaffold).Decide(t)
	if d.Decision != "run" {
		t.Fatalf("A FRESHLY ONBOARDED PROJECT CANNOT RUN: %+v\n"+
			"If reason is monthly-cost-exhausted, someone has let SYNTHETIC dollars "+
			"reach the REAL cost gate, or dropped the zero-cost carve-out from "+
			"ReserveIfFits (gonk-2g4). See Plan 03 Decision 9.", d)
	}

	// ...and it STILL runs after burning an arbitrary pile of synthetic dollars,
	// because synthetic dollars are NOT SPEND. Distinct beads keep the per-task
	// token ceiling out of it -- this is a currency test, not a token test.
	for i := 0; i < 50; i++ {
		w.Session(fmt.Sprintf("gk-%d", i), fmt.Sprintf("s-%d", i), atags.TriggerScaffold).Run(t, 5, meterapi.OutcomeSuccess)
	}
	w.SyncSpend(t)
	if d := w.Session("gk-last", "s-last", atags.TriggerScaffold).Decide(t); d.Decision != "run" {
		t.Fatalf("synthetic spend closed the REAL cost door: %+v", d)
	}

	// The ledger agrees: real spend is exactly zero, synthetic spend is not.
	c := w.projectCost(t, "acme/fresh")
	ledger.AssertUSD(t, "real spend", c.CostUSD, 0)
	if c.SyntheticCostUSD <= 0 {
		t.Fatal("local spend was not recorded as synthetic at all -- the token door has no teeth")
	}
}

// TestBrickTestWouldCatchCurrencyUnification is the saboteur that makes the guard
// above non-vacuous: it proves budget.Remain keeps synthetic dollars INVISIBLE to
// real remaining cost. If the two currencies were ever summed, a heavy pile of
// synthetic spend against a finite real ceiling would erode real headroom to zero
// -- and the brick guard above would go red. Here it must not.
func TestBrickTestWouldCatchCurrencyUnification(t *testing.T) {
	ceiling := budget.Budget{
		MonthlyCostUSD: 5,
		MonthlyTokens:  budget.UnlimitedTokens,
		PerTaskTokens:  budget.UnlimitedTokens,
	}
	// A mountain of synthetic dollars, zero real dollars.
	spent := budget.Spend{
		CostUSD:                  0,
		SyntheticCostUSD:         1000,
		ReservedSyntheticCostUSD: 500,
	}
	rem := budget.Remain(ceiling, spent)
	if float64(rem.MonthlyCostUSD) != 5 {
		t.Fatalf("synthetic dollars eroded real remaining cost to %v (want 5): the currency firewall is breached, "+
			"and the onboarding default would brick", float64(rem.MonthlyCostUSD))
	}
}
