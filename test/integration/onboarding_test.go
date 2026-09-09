package integration_test

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

// TestOnboardingToTriage walks spec 11.2 with fakes, end to end through the real
// intake->meter wire (P3-10) and the real money path: onboarding MR (zero
// tokens, `.gonk.yml` AND the `.agent/` seed), merge, resolve to active/valid,
// and TRIAGE -- the first metered work -- landing an EXACT ledger row.
//
// The shape of that walk is what T-08 changed. It used to run
// onboarding -> pending -> a metered SCAFFOLD session -> only then triage, so
// the first thing every project spent money on was a model writing `.agent/`.
// The seed is deterministic and rides the onboarding merge request now, so the
// first spend is the work somebody actually asked for.
func TestOnboardingToTriage(t *testing.T) {
	w := NewWorld(t)
	p := w.GitLab.AddProject("acme/widget", glab.AccessMaintainer) // bot invited, no .gonk.yml

	w.Reconcile(t)

	// 1. Bot is a member with no .gonk.yml -> a deterministic onboarding MR, ZERO tokens.
	mr := requireOneMR(t, w, p.ID, intake.OnboardBranch)
	ledger.AssertNoSpend(t, w.Views) // spec 5.3: the onboarding MR is deterministic

	// 2. Merging it puts .gonk.yml on the default branch.
	mergeOnboardingMR(t, w, p, mr, onboardingLadder)
	w.Reconcile(t)

	// 3. Intake PUT the RAW bytes; meter resolved them. Intake never resolved.
	requireMeterState(t, w, "acme/widget", meterapi.StateActive)
	// VALID, not pending: the merge landed the `.agent/` seed along with the
	// config (T-08), so the project is triageable the moment it is merged. It
	// used to sit at `pending` here until a metered scaffold session wrote
	// `.agent/` for it.
	requireIntakeState(t, w, "acme/widget", intake.StateValid)

	pr := w.Meter.GetProject(t, "acme/widget")
	if pr.Effective == nil || !pr.Effective.Actions.Triage {
		t.Fatalf("resolved project = %+v", pr)
	}
	// And the default config does NOT buy a scaffold session: that is opt-in.
	if pr.Effective.Actions.Scaffold {
		t.Fatalf("the onboarding config opted into the metered scaffold: %+v", pr.Effective.Actions)
	}

	// 4. TRIAGE is the first metered work. Nothing was spent to get here.
	w.project, w.rig = "acme/widget", rigFor("acme/widget")
	w.Model.SetScript(successScript("triaged the issue.", 1000, 250))
	sess := w.Session("gk-1", "sess-1", atags.TriggerIssueTriage)
	d := sess.Run(t, 2, "success")
	if d.Decision != "run" || d.Rung != "qwen-local" {
		t.Fatalf("triage decision = %+v", d)
	}
	w.SyncSpend(t)

	// 5. THE LEDGER SAYS EXACTLY THIS. Not "roughly". Exactly.
	ledger.Assert(t, w.Views, []ledger.Want{{
		Project: "acme/widget", Rig: "acme-widget", BeadID: "gk-1", SessionKey: "sess-1",
		Rung: "qwen-local", Attempt: 1, Trigger: atags.TriggerIssueTriage,
		Calls: 2, PromptTokens: 2000, CompletionTokens: 500,
		CostUSD:      0,        // local rung: ZERO real dollars, forever
		SyntheticUSD: 0.000625, // 2500 tokens x $0.25/1M
	}})
}
