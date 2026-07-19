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
// POST without a valid, request-bound grant. Here a freshly-merged project goes
// `pending` and the reconciler auto-fires the scaffold order through the REAL
// pkg/gcapi signing path; the stub controller records the pour ONLY after
// verifying the signature AND the method+path+query+body binding -- so a recorded
// gonk-dispatch pour proves the real signer was attached and produced a valid grant.
func TestScaffoldDispatchCarriesAValidGrant(t *testing.T) {
	w := NewWorld(t)
	p := w.GitLab.AddProject("acme/widget", glab.AccessMaintainer)

	w.Reconcile(t)
	mr := requireOneMR(t, w, p.ID, intake.OnboardBranch)
	mergeOnboardingMR(t, w, p, mr, onboardingLadder)
	w.Reconcile(t) // pending -> FireScaffold -> grant-gated order-run POST

	names := w.Controller.pouredNames()
	if !slices.Contains(names, "gonk-dispatch") {
		t.Fatalf("no grant-verified gonk-dispatch order was poured: %v "+
			"(a rejected grant would 403 and record nothing)", names)
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
		SessionKey: "gonk-1-scaffold", BeadAnchor: "gonk:1:scaffold",
	})
	if err == nil {
		t.Fatal("a grant-gated controller must reject an unsigned dispatch")
	}
	if !gcapi.IsWriteAuthRequired(err) {
		t.Fatalf("want a write-auth-required rejection, got: %v", err)
	}
}
