package integration_test

import (
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

// TestInstanceKillSwitchDisablesAProjectWithoutAConfigChange is P2-9's nastiest
// half: an operator kill switch must reach a project whose OWN .gonk.yml never
// changed -- exactly what meter's config_hash short-circuit could silently
// swallow. The instance config bytes are identical; only the operator layer flips.
func TestInstanceKillSwitchDisablesAProjectWithoutAConfigChange(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local"), withBudget(0))
	requireMeterState(t, w, "acme/widget", meterapi.StateActive)
	requireIntakeState(t, w, "acme/widget", intake.StateValid)

	// SAME .gonk.yml bytes: only the operator instance kill switch flips.
	w.Meter.SetOperatorConfig(t, operatorYAMLInstanceEnabled(false))
	w.Reconcile(t)

	requireMeterState(t, w, "acme/widget", meterapi.StateDisabled)
	requireIntakeState(t, w, "acme/widget", intake.StateDisabled)

	d := w.Session("gk-2", "sess-2", atags.TriggerIssueTriage).Decide(t)
	ledger.AssertDenied(t, d, meterapi.ReasonDisabled)
	ledger.AssertNoSpend(t, w.Views)
}

// TestDeleteDeonboardsAndDeletesTheKey: removing the bot from a project (it
// vanishes from the membership list) drives intake to DELETE the meter
// registration, which disables the project and deletes its LiteLLM virtual key.
func TestDeleteDeonboardsAndDeletesTheKey(t *testing.T) {
	w := NewWorld(t)
	p := onboard(t, w, "acme/widget", withLadder("qwen-local", "glm"), withBudget(5))
	requireMeterState(t, w, "acme/widget", meterapi.StateActive)
	if !w.LLM.HasKeyFor("acme/widget") {
		t.Fatal("an active project must hold a live virtual key")
	}

	w.GitLab.RemoveProject(p.ID) // the bot was removed / the project deleted
	w.Reconcile(t)

	// Meter no longer knows the project, and its key is gone.
	if s := w.Meter.State(t, "acme/widget"); s != "" {
		t.Fatalf("de-onboarded project still registered with meter: state %q", s)
	}
	if w.LLM.HasKeyFor("acme/widget") {
		t.Fatal("de-onboard must delete the LiteLLM virtual key (a disabled project must not keep spending)")
	}
	// A decision for a de-onboarded project denies (not-registered), no spend.
	d := w.SessionFor("acme/widget", "gk-x", "sess-x", atags.TriggerIssueTriage).Decide(t)
	ledger.AssertDenied(t, d, meterapi.ReasonNotRegistered)
	ledger.AssertNoSpend(t, w.Views)
}

// TestQuietHoursDefer is P2-10: with the injected clock inside a project's quiet
// window, meter answers `defer`/quiet-hours (a first-class success), spending
// nothing; advancing the clock past the window turns the SAME session into a run.
func TestQuietHoursDefer(t *testing.T) {
	// 23:00 UTC is inside 22:00-07:00.
	inWindow := time.Date(2026, 7, 13, 23, 0, 0, 0, time.UTC)
	w := NewWorld(t, WithClock(inWindow))
	onboard(t, w, "acme/widget", withLadder("qwen-local"), withQuietHours("22:00-07:00", "UTC"))

	d := w.Session("gk-1", "s1", atags.TriggerIssueTriage).Decide(t)
	ledger.AssertDeferred(t, d, meterapi.ReasonQuietHours)
	ledger.AssertNoSpend(t, w.Views)

	// Cross out of the window (to 08:00 the next morning) and re-sync the spend
	// snapshot; the same work now runs.
	w.Clock.Advance(9 * time.Hour) // 23:00 -> 08:00 next day, past 07:00
	w.SyncSpend(t)
	w.Model.SetScript(successScript("triaged after quiet hours.", 1000, 250))
	d = w.Session("gk-1", "s1", atags.TriggerIssueTriage).Decide(t)
	ledger.AssertRan(t, d)
}

// TestMeterWireIsTheRealClient is a direct statement of P3-10: intake's REAL
// meterapi client (via the reconciler's PUT) and a SyntheticSession's REAL
// decide/outcome both speak to the REAL meter service over HTTP -- no fakeMeter
// anywhere. A round-trip that resolves an active project proves the wire.
func TestMeterWireIsTheRealClient(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local", "glm"), withBudget(10))
	pr := w.Meter.GetProject(t, "acme/widget")
	if pr.State != meterapi.StateActive || pr.Effective == nil {
		t.Fatalf("real meter wire did not resolve an active project: %+v", pr)
	}
	if pr.KeyRef.SecretName == "" {
		t.Fatal("an active project's ProjectResponse must carry a KeyRef")
	}
}
