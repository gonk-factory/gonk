package gonkcfg

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// The `.gonk.yml` `scope:` section (gonk-066 S3). Documented in
// docs/reference/gonk-scope.md and in the schema's own descriptions.

const scopeYAML = `
version: 1
enabled: true
scope:
  classes: [source, test]
  caps: { max_files: 2, max_lines: 30, max_deletes: 0, max_file_bytes: 65536, max_patch_bytes: 8192 }
  protected_paths: ["internal/auth/**"]
  ci_paths: ["ci/**"]
  test_paths: ["internal/testutil/**"]
  docs_paths: ["notes/**"]
`

func TestLoadDecodesTheScopeSection(t *testing.T) {
	pc, err := Load([]byte(scopeYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	i := func(v int64) *int64 { return &v }
	want := scope.Policy{
		Classes:        []scope.Class{scope.ClassSource, scope.ClassTest},
		Caps:           scope.CapsPolicy{MaxFiles: i(2), MaxLines: i(30), MaxDeletes: i(0), MaxFileBytes: i(65536), MaxPatchBytes: i(8192)},
		ProtectedPaths: []string{"internal/auth/**"}, CIPaths: []string{"ci/**"},
		TestPaths: []string{"internal/testutil/**"}, DocsPaths: []string{"notes/**"},
	}
	if !reflect.DeepEqual(pc.Scope, want) {
		t.Errorf("Scope = %+v\nwant    %+v", pc.Scope, want)
	}
}

// A config with no scope section is exactly what it was: the zero policy.
func TestLoadWithoutScopeIsTheZeroPolicy(t *testing.T) {
	pc, err := Load([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !pc.Scope.IsZero() {
		t.Errorf("Scope = %+v, want zero", pc.Scope)
	}
}

// "invalid .gonk.yml scope denies at dispatch": Load is what the meter calls at
// registration, so a refusal here is StateInvalid -- no key, no decision, no
// spend (internal/meter/service has the end-to-end half).
func TestLoadRejectsInvalidScope(t *testing.T) {
	head := "version: 1\nenabled: true\nscope:\n"
	cases := map[string]struct{ body, want string }{
		// the schema's half
		"unknown key":              {"  banana: true\n", "scope"},
		"source_paths is operator": {"  source_paths: [\"**/*.py\"]\n", "scope"},
		"deny_lift is operator":    {"  deny_lift: [ci]\n", "scope"},
		"protected class listed":   {"  classes: [source, ci]\n", "scope"},
		"control listed":           {"  classes: [control]\n", "scope"},
		"unknown class":            {"  classes: [sauce]\n", "scope"},
		"negative cap":             {"  caps: { max_files: -1 }\n", "scope"},
		"fractional cap":           {"  caps: { max_files: 1.5 }\n", "scope"},
		"unknown cap":              {"  caps: { max_reviewers: 1 }\n", "scope"},
		"absolute pattern":         {"  protected_paths: [\"/etc/**\"]\n", "scope"},
		"character class":          {"  test_paths: [\"a[bc]/**\"]\n", "scope"},
		"empty pattern":            {"  docs_paths: [\"\"]\n", "scope"},
		"patterns not a list":      {"  ci_paths: \"ci/**\"\n", "scope"},
		// profiles (gonk-wj14)
		"unknown profile":          {"  profiles: [go, rust]\n", "scope"},
		"profile wrong case":       {"  profiles: [Python]\n", "scope"},
		"profile listed twice":     {"  profiles: [go, go]\n", "scope"},
		"profiles not a list":      {"  profiles: python\n", "scope"},
		"allowed_profiles is oper": {"  allowed_profiles: [go]\n", "scope"},
		"profile row as a pattern": {"  profiles: [\"**/*.py\"]\n", "scope"},
		// the lattice's half (the schema cannot say these)
		"deploy -> docs":       {"  docs_paths: [\"deploy/**\"]\n", "lattice"},
		"chart -> test":        {"  test_paths: [\"chart/**\"]\n", "lattice"},
		"Dockerfile -> docs":   {"  docs_paths: [\"**/Dockerfile*\"]\n", "lattice"},
		"go.mod -> test":       {"  test_paths: [\"go.mod\"]\n", "lattice"},
		".agent -> docs":       {"  docs_paths: [\".agent/**\"]\n", "lattice"},
		"Makefile -> test":     {"  test_paths: [\"Makefile\"]\n", "lattice"},
		"secret -> ci":         {"  ci_paths: [\"**/*.pem\"]\n", "lattice"},
		"deploy -> ci":         {"  ci_paths: [\"k8s/**\"]\n", "lattice"},
		"test -> docs":         {"  docs_paths: [\"**/*_test.go\"]\n", "lattice"},
		"dot-dot segment":      {"  test_paths: [\"a/../chart/**\"]\n", "scope"},
		"double star in a seg": {"  test_paths: [\"a**/b\"]\n", "scope"},
		// A sensitive row of a profile nobody selected is still not a target.
		"package.json -> docs": {"  docs_paths: [\"**/package.json\"]\n", "lattice"},
		"conftest.py -> test":  {"  test_paths: [\"**/conftest.py\"]\n", "lattice"},
		"own docs -> test":     {"  profiles: [docs-txt]\n  test_paths: [\"notes/*.txt\"]\n", "lattice"},
	}
	for name, c := range cases {
		_, err := Load([]byte(head + c.body))
		if err == nil {
			t.Errorf("%s: Load accepted\n%s", name, c.body)
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), ".gonk.yml: ") {
			t.Errorf("%s: err = %v, want a .gonk.yml error mentioning %q", name, err, c.want)
		}
	}
}

// Resolve does not read the scope section: Effective is unchanged by it, so
// nothing an existing agent is given moves because a project wrote `scope:`.
func TestResolveIgnoresScope(t *testing.T) {
	with, err := Load([]byte(scopeYAML + "ladder: [qwen-local]\nactions: { triage: true }\n"))
	if err != nil {
		t.Fatal(err)
	}
	without, err := Load([]byte("version: 1\nenabled: true\nladder: [qwen-local]\nactions: { triage: true }\n"))
	if err != nil {
		t.Fatal(err)
	}
	inst := Policy{Ladder: []string{"qwen-local"}}
	if a, b := Resolve(inst, Policy{}, *with), Resolve(inst, Policy{}, *without); !reflect.DeepEqual(a, b) {
		t.Errorf("Resolve differs with a scope section:\n%+v\n%+v", a, b)
	}
}

// scope.profiles (gonk-wj14): a project selects ecosystem profiles from the
// shipped menu. Absent is nil -- the default selection -- and an empty list is
// a real, different value (select none).
func TestLoadDecodesScopeProfiles(t *testing.T) {
	head := "version: 1\nenabled: true\n"
	cases := []struct {
		body string
		want []string
		sel  []string
	}{
		{"", nil, []string{"go", "docs-md"}},
		{"scope:\n  caps: { max_files: 1 }\n", nil, []string{"go", "docs-md"}},
		{"scope:\n  profiles: [python, docs-rst]\n", []string{"python", "docs-rst"}, []string{"python", "docs-rst"}},
		{"scope:\n  profiles: [docs-md, node, go]\n", []string{"docs-md", "node", "go"}, []string{"go", "node", "docs-md"}},
		{"scope:\n  profiles: []\n", []string{}, []string{}},
	}
	for _, c := range cases {
		pc, err := Load([]byte(head + c.body))
		if err != nil {
			t.Errorf("Load(%q): %v", c.body, err)
			continue
		}
		if !reflect.DeepEqual(pc.Scope.Profiles, c.want) {
			t.Errorf("Load(%q): Profiles = %#v, want %#v", c.body, pc.Scope.Profiles, c.want)
		}
		sel, err := scope.SelectProfiles(pc.Scope)
		if err != nil || !reflect.DeepEqual(sel, c.sel) {
			t.Errorf("Load(%q): selected %v, %v; want %v", c.body, sel, err, c.sel)
		}
	}
	// Every shipped profile is selectable, and what the schema accepts is
	// exactly what pkg/scope ships.
	all := "scope:\n  profiles: [" + strings.Join(scope.ProfileNames(), ", ") + "]\n"
	if _, err := Load([]byte(head + all)); err != nil {
		t.Errorf("selecting the whole menu refused: %v", err)
	}
	var doc struct {
		Properties struct {
			Scope struct {
				Properties map[string]struct {
					Items struct {
						Enum []string `json:"enum"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"scope"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		t.Fatal(err)
	}
	if got, want := doc.Properties.Scope.Properties["profiles"].Items.Enum, scope.ProfileNames(); !reflect.DeepEqual(got, want) {
		t.Errorf(".gonk.yml schema scope.profiles enum = %v, pkg/scope ships %v", got, want)
	}
	if _, ok := doc.Properties.Scope.Properties["allowed_profiles"]; ok {
		t.Error("the .gonk.yml schema offers scope.allowed_profiles; it is operator-only")
	}
	if _, ok := doc.Properties.Scope.Properties["source_paths"]; ok {
		t.Error("the .gonk.yml schema offers scope.source_paths; free-form source paths stay operator-only (066 Q3)")
	}
}

// TightenScope hands the project layer on to Fold: the selection must survive it.
func TestTightenScopeKeepsTheProfileSelection(t *testing.T) {
	pc, err := Load([]byte("version: 1\nenabled: true\nscope:\n  profiles: [python]\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := ResolvePipelineFix(PipelineFixPolicy{}, PipelineFixPolicy{}, pc.PipelineFix).TightenScope(pc.Scope)
	if !reflect.DeepEqual(got.Profiles, []string{"python"}) {
		t.Errorf("TightenScope dropped scope.profiles: %#v", got.Profiles)
	}
}
