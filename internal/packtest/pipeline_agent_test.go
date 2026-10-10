package packtest

// The `pipeline` agent (gonk-tios D6/P6). Every agent-wide rule in pack_test.go
// already ranges over agents/ and so covers it the moment the directory exists
// (start_command + env, narrowed permissions, the external_directory map, no
// model name, no `name` field). What is pinned HERE is what is specific to it:
//
//   - its harness and permission set are TRIAGE'S, BYTE FOR BYTE. The plan
//     says so (D6: "[env] and OPENCODE_PERMISSION byte-identical to triage's"),
//     and the agent-wide tests would still pass a copy that drifted in a way
//     they do not model -- a whitelist entry dropped, a seventh deny added
//     here but not there, an env var changed. Permissions are the source of
//     truth (gonk-blxu), so a second, slightly different source is the defect.
//   - its effect shape is EXACTLY one diagnosis and nothing else, and its
//     trajectory predicate names only real diagnosis classes.

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/trace"
)

// harnessKeys are the agent.toml top-level keys that make up the opencode
// harness. All of them must equal triage's.
var harnessKeys = []string{"start_command", "prompt_mode", "prompt_flag", "min_active_sessions"}

func TestPipelineAgentHarnessIsTriagesByteForByte(t *testing.T) {
	agents := filepath.Join(packRoot(t), "agents")
	triage := decodeTOMLFile(t, filepath.Join(agents, "triage", "agent.toml"))
	pipeline := decodeTOMLFile(t, filepath.Join(agents, "pipeline", "agent.toml"))

	for _, k := range harnessKeys {
		if !reflect.DeepEqual(pipeline[k], triage[k]) {
			t.Errorf("agents/pipeline/agent.toml %s = %#v, triage has %#v -- D6 makes the pipeline "+
				"harness triage's, byte for byte", k, pipeline[k], triage[k])
		}
	}
	if d, _ := pipeline["description"].(string); d == "" {
		t.Error("agents/pipeline/agent.toml has no description")
	}

	tEnv, _ := triage["env"].(map[string]any)
	pEnv, _ := pipeline["env"].(map[string]any)
	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if !reflect.DeepEqual(keys(pEnv), keys(tEnv)) {
		t.Errorf("agents/pipeline/agent.toml [env] keys %v, triage's %v -- must be identical", keys(pEnv), keys(tEnv))
	}
	for k, tv := range tEnv {
		if pv, ok := pEnv[k]; ok && pv != tv {
			t.Errorf("agents/pipeline/agent.toml [env] %s differs from triage's:\n pipeline: %v\n triage:   %v\n"+
				"OPENCODE_PERMISSION is the source of truth for what this agent can do (gonk-blxu); "+
				"a drifted copy is a second, different truth", k, pv, tv)
		}
	}
}

// The effective permission for the tools D6 names, evaluated the way opencode
// does (last match wins), so the assertion is on the outcome rather than on
// the spelling: read/glob/grep run, the six denied tools are removed.
func TestPipelineAgentCanReadAndNothingElse(t *testing.T) {
	cfg := decodeTOMLFile(t, filepath.Join(packRoot(t), "agents", "pipeline", "agent.toml"))
	env, _ := cfg["env"].(map[string]any)
	raw, _ := env["OPENCODE_PERMISSION"].(string)
	entries, err := orderedJSONObject(raw)
	if err != nil {
		t.Fatalf("OPENCODE_PERMISSION: %v", err)
	}
	rules := flattenPermissionConfig(entries)
	for _, p := range []string{"read", "glob", "grep"} {
		if got := evaluatePermissionRules(rules, p, "*"); got != "allow" {
			t.Errorf("pipeline agent: %s resolves to %q, want allow -- the agent must be able to read the checkout", p, got)
		}
	}
	for _, p := range deniedPermissions {
		if got := evaluatePermissionRules(rules, p, "*"); got != "deny" {
			t.Errorf("pipeline agent: %s resolves to %q, want deny", p, got)
		}
	}
	if got := evaluatePermissionRules(rules, "external_directory", "/tmp/gonk/*"); got != "ask" {
		t.Errorf("pipeline agent: external_directory for /tmp/gonk/* resolves to %q, want ask (auto-rejected under opencode run, gonk-blxu)", got)
	}
}

func TestPipelineEffectShapeIsExactlyOneDiagnosis(t *testing.T) {
	root := packRoot(t)
	shape, err := effects.LoadShape(root, "pipeline")
	if err != nil {
		t.Fatalf("LoadShape(pipeline): %v", err)
	}
	want := map[effects.Kind]effects.Card{effects.KindDiagnosis: {Min: 1, Max: 1}}
	if !reflect.DeepEqual(shape.Kinds, want) {
		t.Errorf("pipeline effect shape = %v, want exactly %v (every other kind forbidden)", shape.Kinds, want)
	}

	// And the shape actually refuses what it must: a batch with a comment, or
	// two diagnoses, or none.
	diag := effects.Effect{Kind: effects.KindDiagnosis}
	for name, b := range map[string]effects.Batch{
		"none":            {},
		"two diagnoses":   {Effects: []effects.Effect{diag, diag}},
		"plus a comment":  {Effects: []effects.Effect{diag, {Kind: effects.KindComment, Body: "x"}}},
		"plus a label":    {Effects: []effects.Effect{diag, {Kind: effects.KindLabel, Add: []string{"x"}}}},
		"plus a file":     {Effects: []effects.Effect{diag, {Kind: effects.KindFile, Path: ".agent/x", Content: "x"}}},
		"comment instead": {Effects: []effects.Effect{{Kind: effects.KindComment, Body: "x"}}},
	} {
		if err := effects.Validate(b, shape); err == nil {
			t.Errorf("pipeline shape accepted a batch with %s", name)
		}
	}
	if err := effects.Validate(effects.Batch{Effects: []effects.Effect{diag}}, shape); err != nil {
		t.Errorf("pipeline shape refused exactly one diagnosis: %v", err)
	}

	pol, err := trace.LoadPolicy(root, "pipeline")
	if err != nil {
		t.Fatalf("LoadPolicy(pipeline): %v", err)
	}
	if !reflect.DeepEqual(pol.ReadTools, []string{"read", "Read"}) {
		t.Errorf("pipeline trajectory read_tools = %v, want [read Read]", pol.ReadTools)
	}
	if want := []string{"code", "test", "configuration"}; !reflect.DeepEqual(pol.RequireAnyReadFor, want) {
		t.Errorf("pipeline trajectory require_any_read_for = %v, want %v (the classes that claim something about repository content)",
			pol.RequireAnyReadFor, want)
	}
	known := map[string]bool{}
	for _, c := range effects.Classifications() {
		known[string(c)] = true
	}
	for _, c := range pol.RequireAnyReadFor {
		if !known[c] {
			t.Errorf("pipeline trajectory names %q, which is not a diagnosis classification -- a dead predicate", c)
		}
	}
}
