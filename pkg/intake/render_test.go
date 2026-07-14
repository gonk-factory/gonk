package intake

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// The template must render a valid, enabled config — an onboarding MR that lands
// a config gonk then rejects would be a spectacular own goal. We render with a
// representative operator ladder (this instance's catalog is qwen-local).
func TestDefaultConfigIsValidAndEnabled(t *testing.T) {
	raw, err := RenderDefaultConfig([]string{"qwen-local"})
	if err != nil {
		t.Fatalf("render default config: %v", err)
	}
	cfg, err := gonkcfg.Load(raw)
	if err != nil {
		t.Fatalf("the config we ask projects to merge does not validate: %v", err)
	}
	eff := gonkcfg.Resolve(gonkcfg.Policy{}, gonkcfg.Policy{}, *cfg)
	if !eff.Enabled {
		t.Fatalf("merging the default config would leave the project disabled: %s", eff.DisabledReason)
	}
	if !eff.Actions.Triage || eff.Actions.Pipelines || eff.Actions.Features {
		t.Fatalf("default must be triage-only: %+v", eff.Actions)
	}
	if eff.Budget.MonthlyCostUSD != 0 {
		t.Fatalf("default cloud budget must be 0, got %v", eff.Budget.MonthlyCostUSD)
	}
	if len(eff.Ladder) != 1 || eff.Ladder[0] != "qwen-local" {
		t.Fatalf("default ladder must be local-only: %v", eff.Ladder)
	}
}

// FAIL CLOSED on an empty ladder. Intake does NOT run opercfg, so nothing else on
// its side stops an unset GONK_INSTANCE_LADDER from producing a `.gonk.yml` with
// an empty `ladder:` -- which ADR-002 then uses to disable every onboarded
// project. RenderDefaultConfig must refuse, and must NOT emit an empty ladder.
func TestRenderDefaultConfigRefusesEmptyLadder(t *testing.T) {
	for _, ladder := range [][]string{nil, {}} {
		if _, err := RenderDefaultConfig(ladder); err == nil {
			t.Fatalf("RenderDefaultConfig(%v) returned no error -- an empty ladder must fail closed", ladder)
		}
	}
}

// Determinism: same input, same bytes, every time. This is what makes the
// onboarding MR reviewable and reproducible.
func TestRenderIsDeterministic(t *testing.T) {
	a, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		b, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatal("render is not deterministic")
		}
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	got, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/onboarding-mr.golden.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("MR body drifted from the golden file.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Spec 5.3: the MR must explain "every configurable key". This gate makes that
// enforceable rather than aspirational: it reads the PUBLISHED schema and fails
// if a key exists that the MR body never mentions. Adding a key to .gonk.yml
// without documenting it here fails CI.
func TestMRBodyDocumentsEveryConfigKey(t *testing.T) {
	raw, err := os.ReadFile("../../docs/schemas/gonk-config.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
	if err != nil {
		t.Fatal(err)
	}
	for top, sub := range schema.Properties {
		if top == "version" {
			continue // schema plumbing, not a user-facing knob
		}
		if !strings.Contains(body, top) {
			t.Errorf("MR body never mentions config key %q (spec 5.3: explain every configurable key)", top)
		}
		for k := range sub.Properties {
			if !strings.Contains(body, k) {
				t.Errorf("MR body never mentions config key %q.%q", top, k)
			}
		}
	}
}

// The prose must be generated from the values, not typed alongside them.
func TestRenderedBodyQuotesTheActualValues(t *testing.T) {
	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := RenderDefaultConfig([]string{"qwen-local"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, string(cfg)) {
		t.Fatal("the MR body must embed the exact .gonk.yml it commits, byte for byte")
	}
	for _, want := range []string{"$0", "qwen-local", "@gonk", "group/repo"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not mention %q", want)
		}
	}
}
