package gonkcfg

import "testing"

func TestLoadSpecExample(t *testing.T) {
	pc, err := Load([]byte(validYAML))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if pc.Version != 1 || pc.Enabled == nil || !*pc.Enabled {
		t.Fatalf("version/enabled wrong: %+v", pc)
	}
	if pc.Actions.Triage == nil || !*pc.Actions.Triage {
		t.Fatalf("actions.triage wrong: %+v", pc.Actions)
	}
	if pc.Budget.MonthlyTokens == nil || *pc.Budget.MonthlyTokens != 50_000_000 {
		t.Fatalf("monthly_tokens wrong: %+v", pc.Budget)
	}
	if got := pc.Ladder; len(got) != 1 || got[0] != "qwen-local" {
		t.Fatalf("ladder wrong: %v", got)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	if _, err := Load([]byte("version: 1")); err == nil {
		t.Fatal("Load accepted schema-invalid doc")
	}
}

func TestLoadRejectsFractionalTokens(t *testing.T) {
	// The schema accepts 2.0 (zero-fraction numbers are JSON Schema integers);
	// TokenQuantity's tag dispatch is what rejects it. Load must run both, so
	// a float budget can never reach an Effective config. See Task 3 review.
	for _, doc := range []string{
		"version: 1\nenabled: true\nbudget: { monthly_tokens: 2.0 }",
		"version: 1\nenabled: true\nbudget: { monthly_tokens: 1.5 }",
		"version: 1\nenabled: true\nbudget: { per_task_tokens: 2.0 }",
	} {
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("Load accepted fractional token budget: %q", doc)
		}
	}
}
