package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "time/tzdata" // opercfg resolves IANA zones; the image carries no /usr/share/zoneinfo

	"gitlab.orac.local/agentic/gonk-project/internal/meter/keysink"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/service"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// goodOperatorYAML is the config in force before the bad edit: an instance
// ladder, a real budget, and NO schedule at all.
const goodOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local]
  budget: { monthly_cost_usd: 200 }
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0, est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
`

// badOperatorYAML is goodOperatorYAML plus ONE edit: an instance quiet-hours
// window with no timezone. Everything else is byte-identical, so if the
// reload were to go through, the only thing that could break is the schedule --
// which is precisely R-20's failure.
const badOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local]
  budget: { monthly_cost_usd: 200 }
  schedule:
    quiet_hours: "22:00-07:00"
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0, est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
`

// fixedOperatorYAML is badOperatorYAML with the missing timezone supplied: the
// edit the operator MEANT to make. It proves the guard rejects the defect
// rather than the feature.
const fixedOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local]
  budget: { monthly_cost_usd: 200 }
  schedule:
    quiet_hours: "22:00-07:00"
    timezone: "America/New_York"
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0, est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
`

const projectYAML = "version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\n"

// TestRejectedReloadKeepsThePreviousConfigAndTheProjectsKey is R-20's
// regression test, and it asserts the OPERATIONAL claim, not the loader one.
//
// "opercfg.Load returns an error" and "the running instance is unharmed" are
// two different statements. The one that matters is the second: the reresolve
// ticker re-reads the ConfigMap and then re-resolves every registered project
// on the SAME tick, so a bad config that reached SetConfig would mark every
// project invalid and delete every project's LiteLLM virtual key. This test
// therefore drives the real reload path against a real Service, and then runs
// the Reresolve that follows it, and asserts the project is still active and
// its key is still there.
func TestRejectedReloadKeepsThePreviousConfigAndTheProjectsKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "operator.yaml")
	writeFile(t, path, goodOperatorYAML)

	good, err := loadOperatorConfig(path)
	if err != nil {
		t.Fatalf("setup: the good operator config must load: %v", err)
	}

	st := store.NewMemory()
	admin := litellm.NewFake()
	keys := keysink.NewMemory()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	admin.Now = now
	svc := service.New(good, st, admin, admin, keys, func() time.Time { return now })

	resp, status, err := svc.Register(ctx, meterapi.ProjectRequest{
		Project: "agentic/widget", ProjectID: 1, Rig: "agentic-widget", GonkYML: projectYAML,
	})
	if err != nil || status != 200 {
		t.Fatalf("setup: Register = %+v %d %v", resp, status, err)
	}
	tokenBefore := keys.Token("agentic/widget")
	if tokenBefore == "" {
		t.Fatal("setup: the project has no virtual key, so this test could not observe one being deleted")
	}

	// The operator edits the ConfigMap and gets it wrong.
	writeFile(t, path, badOperatorYAML)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reloadOperatorConfig(log, svc, path)

	// 1. The previous config is STILL the config in force -- not merely
	//    "Load returned an error somewhere".
	inForce := svc.Config()
	if inForce != good {
		t.Fatalf("a rejected reload swapped the operator config: in force = %p, previous = %p", inForce, good)
	}
	if inForce.Instance.Schedule != nil {
		t.Fatalf("the bad schedule reached the running config: %+v", inForce.Instance.Schedule)
	}
	if inForce.Instance.Budget.MonthlyCostUSD == nil || *inForce.Instance.Budget.MonthlyCostUSD != 200 {
		t.Fatalf("the previous instance ceiling did not survive the rejected reload: %+v", inForce.Instance.Budget)
	}

	// 2. And the tick completes: Reresolve runs against the config in force.
	//    THIS is where the key would have been deleted.
	if err := svc.Reresolve(ctx); err != nil {
		t.Fatalf("Reresolve after a rejected reload: %v", err)
	}
	reg, ok, err := st.GetRegistration(ctx, "agentic/widget")
	if err != nil || !ok {
		t.Fatalf("registration gone after a rejected reload: ok=%v err=%v", ok, err)
	}
	if reg.State != store.StateActive {
		t.Fatalf("project state = %q after a rejected reload, want %q (invalid_detail: %q)",
			reg.State, store.StateActive, reg.InvalidDetail)
	}
	if got := keys.Token("agentic/widget"); got != tokenBefore {
		t.Fatalf("the project's virtual key changed across a rejected reload: %q -> %q", tokenBefore, got)
	}
}

// The mirror: a GOOD reload must still take effect. Without this, "never swap
// the config" would pass the test above, and the hot reload the instance kill
// switch depends on would be dead.
func TestAcceptedReloadSwapsTheConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.yaml")
	writeFile(t, path, goodOperatorYAML)

	good, err := loadOperatorConfig(path)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	svc := service.New(good, store.NewMemory(), litellm.NewFake(), litellm.NewFake(),
		keysink.NewMemory(), func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) })

	// The same edit as badOperatorYAML, done RIGHT: quiet hours with the
	// timezone they need.
	writeFile(t, path, fixedOperatorYAML)

	reloadOperatorConfig(slog.New(slog.NewTextHandler(io.Discard, nil)), svc, path)

	inForce := svc.Config()
	if inForce == good {
		t.Fatal("a valid reload did NOT swap the config; the instance kill switch would never take effect")
	}
	if inForce.Instance.Schedule == nil || inForce.Instance.Schedule.Timezone != "America/New_York" {
		t.Fatalf("the reloaded config does not carry the new schedule: %+v", inForce.Instance.Schedule)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
