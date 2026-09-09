package integration_test

import (
	"context"
	"slices"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
)

// TestScaffoldDispatchCarriesAValidGrant is D1 at L1: intake's dispatcher now
// needs an ed25519 write-auth Signer, and the controller rejects any order-run
// POST without a valid, request-bound grant. Here a project in the one state
// that still auto-fires a scaffold order has the reconciler fire it through the
// REAL pkg/gcapi signing path; the stub controller records the pour ONLY after
// verifying the signature AND the method+path+query+body binding -- so a recorded
// gonk-dispatch pour proves the real signer was attached and produced a valid grant.
//
// THAT STATE IS NARROWER SINCE T-08 and the config below is what puts the
// project in it: `.agent/` absent (so `pending`) AND `actions.scaffold: true`
// (the opt-in). A merged onboarding MR no longer produces it -- that merge lands
// the `.agent/` seed and leaves the project `valid` with scaffold off -- so this
// test writes the config directly rather than merging one.
func TestScaffoldDispatchCarriesAValidGrant(t *testing.T) {
	w := NewWorld(t)
	p := w.GitLab.AddProject("acme/widget", glab.AccessMaintainer)

	w.Reconcile(t)
	mr := requireOneMR(t, w, p.ID, intake.OnboardBranch)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\n"+
		"actions: { triage: true, scaffold: true }\nladder: [qwen-local]\n"))
	w.GitLab.SetMRState(p.ID, mr.IID, "merged", baseClock)
	w.Reconcile(t) // pending + opted in -> FireScaffold -> grant-gated order-run POST

	requireIntakeState(t, w, "acme/widget", intake.StatePending)

	names := w.Controller.pouredNames()
	if !slices.Contains(names, "gonk-dispatch") {
		t.Fatalf("no grant-verified gonk-dispatch order was poured: %v "+
			"(a rejected grant would 403 and record nothing)", names)
	}
}

// The other side of the opt-in, and the one T-08 actually changes for every
// project: a NORMALLY onboarded project -- merged onboarding MR, `.agent/` seed
// and all -- fires NO scaffold order. Without this, "scaffold is opt-in" is
// asserted only at the unit level, where the reconciler is not in the picture.
func TestNormalOnboardingFiresNoScaffoldOrder(t *testing.T) {
	w := NewWorld(t)
	p := w.GitLab.AddProject("acme/widget", glab.AccessMaintainer)

	w.Reconcile(t)
	mr := requireOneMR(t, w, p.ID, intake.OnboardBranch)
	mergeOnboardingMR(t, w, p, mr, onboardingLadder)
	w.Reconcile(t)

	// The seed rode the merge, so there is nothing to scaffold.
	requireIntakeState(t, w, "acme/widget", intake.StateValid)
	if names := w.Controller.pouredNames(); slices.Contains(names, "gonk-dispatch") {
		t.Fatalf("a normally onboarded project paid for a scaffold session: %v", names)
	}
}

// TestUnsignedDispatchIsRejected is the other half of D1: a dispatcher with NO
// Signer sends no grant, and the grant-gated controller answers 401 -- proving
// the gate is real, not decorative. Without it, "the signer is attached" would
// prove nothing.
func TestUnsignedDispatchIsRejected(t *testing.T) {
	w := NewWorld(t)
	disp := intake.NewHTTPDispatcher(w.Controller.URL(), nil, nil) // NO signer
	err := disp.FireOrder(context.Background(), intake.OrderRequest{
		Trigger: atags.TriggerScaffold, Project: "acme/widget", ProjectID: 1, Rig: "acme-widget",
		SessionKey: "test-session-not-a-secret", BeadAnchor: "gonk:1:scaffold",
	})
	if err == nil {
		t.Fatal("a grant-gated controller must reject an unsigned dispatch")
	}
	if !gcapi.IsWriteAuthRequired(err) {
		t.Fatalf("want a write-auth-required rejection, got: %v", err)
	}
}
