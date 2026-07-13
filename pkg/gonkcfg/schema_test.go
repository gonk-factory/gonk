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
	}
	for name, doc := range cases {
		if err := Validate([]byte(doc)); err == nil {
			t.Errorf("%s: Validate accepted %q, want error", name, strings.TrimSpace(doc))
		}
	}
}
