package gonkcfg

import (
	"strings"
	"testing"
)

const validYAML = `
version: 1
enabled: true
actions: { triage: true, pipelines: false, features: false }
schedule: { quiet_hours: "22:00-07:00", timezone: "America/New_York" }
budget: { monthly_cost_usd: 0, monthly_tokens: "50M", per_task_tokens: "2M" }
ladder: [qwen-local]
continuity: resume
triage: { label_prefix: "gonk::", respond_to_mentions: true }
provenance: { commit_trailers: true, include_usage: false }
`

func TestValidateAcceptsSpecExample(t *testing.T) {
	if err := Validate([]byte(validYAML)); err != nil {
		t.Fatalf("Validate(spec example) = %v, want nil", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"missing version":  "enabled: true",
		"wrong version":    "version: 2\nenabled: true",
		"missing enabled":  "version: 1",
		"unknown key":      "version: 1\nenabled: true\nbanana: true",
		"bad continuity":   "version: 1\nenabled: true\ncontinuity: sometimes",
		"bad quiet_hours":  "version: 1\nenabled: true\nschedule: { quiet_hours: \"25:00-07:00\" }",
		"bad token suffix": "version: 1\nenabled: true\nbudget: { monthly_tokens: \"50MB\" }",
		"negative cost":    "version: 1\nenabled: true\nbudget: { monthly_cost_usd: -1 }",
		"empty ladder":     "version: 1\nenabled: true\nladder: []",
		"uppercase rung":   "version: 1\nenabled: true\nladder: [Qwen]",
		"not yaml":         "{{{{",
		"float tokens":     "version: 1\nenabled: true\nbudget: { monthly_tokens: 1.5 }",
		"duplicate rung":   "version: 1\nenabled: true\nladder: [glm, glm]",
	}
	for name, doc := range cases {
		if err := Validate([]byte(doc)); err == nil {
			t.Errorf("%s: Validate accepted %q, want error", name, strings.TrimSpace(doc))
		}
	}
}

// Non-finite floats crash jsonschema/v6 (v6.0.2 validator.go:515-524 builds a
// big.Rat from the value, gets nil back for NaN/Inf, discards the ok, and
// dereferences it). .gonk.yml is untrusted project-authored content, so
// Validate must reject non-finite floats itself, cleanly, before the document
// reaches the validator. A panic here would let any project crash the process
// that enforces every project's budget.
func TestValidateRejectsNonFiniteFloats(t *testing.T) {
	cases := map[string]string{
		"nan cost":        "version: 1\nenabled: true\nbudget: { monthly_cost_usd: .nan }",
		"inf cost":        "version: 1\nenabled: true\nbudget: { monthly_cost_usd: .inf }",
		"neg inf cost":    "version: 1\nenabled: true\nbudget: { monthly_cost_usd: -.inf }",
		"nan nested":      "version: 1\nenabled: true\nschedule: { timezone: .nan }",
		"nan in ladder":   "version: 1\nenabled: true\nladder: [.nan]",
		"nan at root":     "version: 1\nenabled: true\nbanana: .nan",
		"nan uppercase":   "version: 1\nenabled: true\nbudget: { monthly_cost_usd: .NaN }",
		"inf capitalized": "version: 1\nenabled: true\nbudget: { monthly_cost_usd: .Inf }",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			// Must return an error, not panic. A panic fails the test.
			err := Validate([]byte(doc))
			if err == nil {
				t.Fatalf("Validate accepted %q, want error", strings.TrimSpace(doc))
			}
			if !strings.Contains(err.Error(), "finite") {
				t.Fatalf("error %q should explain the non-finite number", err)
			}
		})
	}
}

// Load must surface the same clean error rather than panicking.
func TestLoadRejectsNonFiniteFloats(t *testing.T) {
	_, err := Load([]byte("version: 1\nenabled: true\nbudget: { monthly_cost_usd: .nan }"))
	if err == nil {
		t.Fatal("Load accepted a NaN budget, want error")
	}
	if !strings.Contains(err.Error(), "finite") {
		t.Fatalf("error %q should explain the non-finite number", err)
	}
}
