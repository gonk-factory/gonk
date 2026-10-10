package gonkcfg

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// The pipeline-fix opt-in (gonk-2usw plan section 3.1): `actions.pipeline_fixes`
// and the `pipeline_fix:` section. Documented in docs/reference/gonk-pipeline-fix.md
// and in the schema's own descriptions.

// planExampleYAML is section 3.1's example, key for key. Every value except
// the two lists that show an opt-in is the documented default.
const planExampleYAML = `
version: 1
enabled: true
actions:
  pipelines: true
  pipeline_fixes: true
pipeline_fix:
  classes: [code, test]
  targets: [ref]
  allow_ci: false
  ci_exec_changes: manual-run
  max_attempts: 2
  max_chain: 5
  verify_timeout: 2h
  max_files: 3
  max_changed_lines: 40
  paths: []
  auto_merge:
    mode: off
    classes: [docs]
`

const fixHead = "version: 1\nenabled: true\nactions: { pipelines: true, pipeline_fixes: true }\n"

func sp(s string) *string { return &s }
func ip(v int64) *int64   { return &v }
func bp(v bool) *bool     { return &v }
func dp(v time.Duration) *Duration {
	d := Duration(v)
	return &d
}

// defaultFix is what a project that opts in and writes nothing else gets:
// plan 3.1's defaults, spelled out as literals so a changed constant fails here.
func defaultFix() EffectivePipelineFix {
	return EffectivePipelineFix{
		Classes: []string{"code", "test"}, Targets: []string{"ref"},
		AllowCI: false, CIExecChanges: "manual-run",
		MaxAttempts: 2, MaxChain: 5, VerifyTimeout: 2 * time.Hour,
		MaxFiles: 3, MaxChangedLines: 40, Paths: []string{},
		AutoMerge: EffectiveAutoMerge{Mode: "off", Classes: []string{"docs"}, MaxPerDay: 3},
	}
}

func TestLoadDecodesThePlanExample(t *testing.T) {
	pc, err := Load([]byte(planExampleYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := PipelineFixPolicy{
		Classes: []string{"code", "test"}, Targets: []string{"ref"},
		AllowCI: bp(false), CIExecChanges: sp("manual-run"),
		MaxAttempts: ip(2), MaxChain: ip(5), VerifyTimeout: dp(2 * time.Hour),
		MaxFiles: ip(3), MaxChangedLines: ip(40), Paths: []string{},
		AutoMerge: AutoMergePolicy{Mode: sp("off"), Classes: []string{"docs"}},
	}
	if !reflect.DeepEqual(pc.PipelineFix, want) {
		t.Errorf("PipelineFix = %+v\nwant          %+v", pc.PipelineFix, want)
	}
	if pc.Actions.PipelineFixes == nil || !*pc.Actions.PipelineFixes {
		t.Errorf("actions.pipeline_fixes not decoded: %+v", pc.Actions)
	}
	eff := Resolve(Policy{Ladder: []string{"qwen-local"}}, Policy{}, withLadder(*pc))
	if !eff.Actions.PipelineFixes || !reflect.DeepEqual(eff.PipelineFix, defaultFix()) {
		t.Errorf("effective = fixes %v, %+v\nwant true, %+v", eff.Actions.PipelineFixes, eff.PipelineFix, defaultFix())
	}
}

func withLadder(pc ProjectConfig) ProjectConfig {
	pc.Ladder = []string{"qwen-local"}
	return pc
}

// Default OFF: a config that predates the feature decodes to the zero policy,
// resolves to "no fixes", and its Effective differs from before ONLY in the
// new, defaulted PipelineFix field.
func TestExistingConfigsAreUnchanged(t *testing.T) {
	for name, doc := range map[string]string{
		"spec example":     validYAML,
		"pipelines only":   "version: 1\nenabled: true\nladder: [qwen-local]\nactions: { triage: true, pipelines: true }\n",
		"scope section":    scopeYAML + "ladder: [qwen-local]\n",
		"nothing opted in": "version: 1\nenabled: true\nladder: [qwen-local]\n",
	} {
		pc, err := Load([]byte(doc))
		if err != nil {
			t.Errorf("%s: Load: %v", name, err)
			continue
		}
		if !pc.PipelineFix.IsZero() || pc.Actions.PipelineFixes != nil {
			t.Errorf("%s: a config with no pipeline-fix keys decoded to %+v / %v", name, pc.PipelineFix, pc.Actions.PipelineFixes)
		}
		eff := Resolve(Policy{Ladder: []string{"qwen-local"}}, Policy{}, *pc)
		if eff.Actions.PipelineFixes {
			t.Errorf("%s: pipeline fixes are ON for a project that never asked", name)
		}
		if !reflect.DeepEqual(eff.PipelineFix, defaultFix()) {
			t.Errorf("%s: PipelineFix = %+v, want the defaults", name, eff.PipelineFix)
		}
		if eff.PipelineFix.AllowCI || eff.PipelineFix.LiftOptIn() != nil || eff.PipelineFix.AutoMerge.Mode != "off" ||
			eff.PipelineFix.AllowsTarget("mr") || eff.PipelineFix.AllowsClass("configuration") {
			t.Errorf("%s: a default is not the conservative one: %+v", name, eff.PipelineFix)
		}
	}
}

// PLAN, must be seen red: "unknown key accepted". Every level of the new
// surface refuses a key it does not know, and every value outside its set.
// These go through Validate -- the SCHEMA alone -- so the schema is what is
// being tested (TestCheckPipelineFix covers the Go half on its own).
func TestSchemaRefusesBadPipelineFix(t *testing.T) {
	cases := map[string]string{
		"unknown key in pipeline_fix":     "pipeline_fix: { banana: true }\n",
		"unknown key in auto_merge":       "pipeline_fix: { auto_merge: { banana: true } }\n",
		"misspelt section":                "pipeline_fixes: { classes: [code] }\n",
		"misspelt action":                 "actions: { pipelines: true, pipeline_fix: true }\n",
		"class flaky":                     "pipeline_fix: { classes: [flaky] }\n",
		"class infrastructure":            "pipeline_fix: { classes: [code, infrastructure] }\n",
		"class unknown":                   "pipeline_fix: { classes: [unknown] }\n",
		"a path class as diagnosis class": "pipeline_fix: { classes: [source] }\n",
		"duplicate class":                 "pipeline_fix: { classes: [code, code] }\n",
		"classes not a list":              "pipeline_fix: { classes: code }\n",
		"target tag":                      "pipeline_fix: { targets: [tag] }\n",
		"allow_ci a string":               "pipeline_fix: { allow_ci: \"yes\" }\n",
		"the action a string":             "actions: { pipelines: true, pipeline_fixes: \"yes\" }\n",
		"the action a number":             "actions: { pipelines: true, pipeline_fixes: 1 }\n",
		"ci_exec_changes always":          "pipeline_fix: { ci_exec_changes: always }\n",
		"max_attempts above the ceiling":  "pipeline_fix: { max_attempts: 4 }\n",
		"max_attempts zero":               "pipeline_fix: { max_attempts: 0 }\n",
		"max_attempts fractional":         "pipeline_fix: { max_attempts: 1.5 }\n",
		"max_chain above the ceiling":     "pipeline_fix: { max_chain: 11 }\n",
		"max_chain zero":                  "pipeline_fix: { max_chain: 0 }\n",
		"max_files above the ceiling":     "pipeline_fix: { max_files: 4 }\n",
		"max_files zero":                  "pipeline_fix: { max_files: 0 }\n",
		"max_changed_lines above":         "pipeline_fix: { max_changed_lines: 41 }\n",
		"max_changed_lines negative":      "pipeline_fix: { max_changed_lines: -1 }\n",
		"verify_timeout a bare number":    "pipeline_fix: { verify_timeout: 7200 }\n",
		"verify_timeout days":             "pipeline_fix: { verify_timeout: 1d }\n",
		"verify_timeout compound":         "pipeline_fix: { verify_timeout: 1h30m }\n",
		"paths absolute":                  "pipeline_fix: { paths: [\"/etc/**\"] }\n",
		"paths character class":           "pipeline_fix: { paths: [\"a[bc]/**\"] }\n",
		"paths not a list":                "pipeline_fix: { paths: \"internal/**\" }\n",
		"mode always":                     "pipeline_fix: { auto_merge: { mode: always } }\n",
		"mode true":                       "pipeline_fix: { auto_merge: { mode: true } }\n",
		"auto-merge class source":         "pipeline_fix: { auto_merge: { classes: [source] } }\n",
		"auto-merge class test":           "pipeline_fix: { auto_merge: { classes: [docs, test] } }\n",
		"auto_merge a string":             "pipeline_fix: { auto_merge: after-human-approval }\n",
	}
	for name, body := range cases {
		head := fixHead
		if strings.HasPrefix(body, "actions:") {
			head = "version: 1\nenabled: true\n"
		}
		if err := Validate([]byte(head + body)); err == nil {
			t.Errorf("%s: the schema accepted\n%s", name, body)
		}
	}
}

// And the same surface accepts everything the plan says it should.
func TestSchemaAcceptsEveryDocumentedValue(t *testing.T) {
	for name, body := range map[string]string{
		"all three classes":      "pipeline_fix: { classes: [code, test, configuration] }\n",
		"no class at all":        "pipeline_fix: { classes: [] }\n",
		"both targets":           "pipeline_fix: { targets: [ref, mr] }\n",
		"allow_ci":               "pipeline_fix: { allow_ci: true }\n",
		"deny":                   "pipeline_fix: { ci_exec_changes: deny }\n",
		"auto":                   "pipeline_fix: { ci_exec_changes: auto }\n",
		"the ceilings":           "pipeline_fix: { max_attempts: 3, max_chain: 10, max_files: 3, max_changed_lines: 40, verify_timeout: 6h }\n",
		"the floors":             "pipeline_fix: { max_attempts: 1, max_chain: 1, max_files: 1, max_changed_lines: 1, verify_timeout: 1s }\n",
		"minutes":                "pipeline_fix: { verify_timeout: 90m }\n",
		"paths":                  "pipeline_fix: { paths: [\"internal/**\", \"cmd/*/main.go\"] }\n",
		"after-human-approval":   "pipeline_fix: { auto_merge: { mode: after-human-approval, classes: [docs, ci] } }\n",
		"after-agentic-review":   "pipeline_fix: { auto_merge: { mode: after-agentic-review } }\n",
		"mode off, bare":         "pipeline_fix: { auto_merge: { mode: off } }\n",
		"mode off, quoted":       "pipeline_fix: { auto_merge: { mode: \"off\" } }\n",
		"section without action": "",
	} {
		if _, err := Load([]byte(fixHead + body)); err != nil {
			t.Errorf("%s: Load refused\n%s%v", name, body, err)
		}
	}
	// A staged section: the block is written, the action is not switched on.
	if _, err := Load([]byte("version: 1\nenabled: true\npipeline_fix: { classes: [code], max_files: 1 }\n")); err != nil {
		t.Errorf("a pipeline_fix section without the action was refused: %v", err)
	}
}

// PLAN, must be seen red: "`pipeline_fixes` without `pipelines` accepted".
// Two guards, tested apart so that neither can hide the other's absence: the
// schema's if/then (through Validate), and CheckPipelineFix (on a decoded
// Policy, which is what a config built in Go would reach).
func TestPipelineFixesRequiresPipelines(t *testing.T) {
	head := "version: 1\nenabled: true\n"
	cases := []struct {
		name    string
		actions string
		policy  ActionsPolicy
		invalid bool
	}{
		{"fixes alone", "actions: { pipeline_fixes: true }\n", ActionsPolicy{PipelineFixes: bp(true)}, true},
		{"fixes with pipelines false", "actions: { pipelines: false, pipeline_fixes: true }\n", ActionsPolicy{Pipelines: bp(false), PipelineFixes: bp(true)}, true},
		{"fixes with triage only", "actions: { triage: true, pipeline_fixes: true }\n", ActionsPolicy{Triage: bp(true), PipelineFixes: bp(true)}, true},
		{"both on", "actions: { pipelines: true, pipeline_fixes: true }\n", ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true)}, false},
		{"fixes false, pipelines absent", "actions: { pipeline_fixes: false }\n", ActionsPolicy{PipelineFixes: bp(false)}, false},
		{"fixes false, pipelines on", "actions: { pipelines: true, pipeline_fixes: false }\n", ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(false)}, false},
		{"pipelines alone", "actions: { pipelines: true }\n", ActionsPolicy{Pipelines: bp(true)}, false},
		{"neither", "actions: { triage: true }\n", ActionsPolicy{Triage: bp(true)}, false},
	}
	for _, c := range cases {
		if err := Validate([]byte(head + c.actions)); (err != nil) != c.invalid {
			t.Errorf("%s: schema: err = %v, want invalid=%v", c.name, err, c.invalid)
		}
		err := CheckPipelineFix(Policy{Actions: c.policy})
		if (err != nil) != c.invalid {
			t.Errorf("%s: CheckPipelineFix: err = %v, want invalid=%v", c.name, err, c.invalid)
		}
		if c.invalid && err != nil && (!strings.Contains(err.Error(), "actions.pipeline_fixes") || !strings.Contains(err.Error(), "actions.pipelines")) {
			t.Errorf("%s: err = %v, want it to name both actions", c.name, err)
		}
		if _, err := Load([]byte(head + c.actions)); (err != nil) != c.invalid {
			t.Errorf("%s: Load: err = %v, want invalid=%v", c.name, err, c.invalid)
		} else if err != nil && !strings.HasPrefix(err.Error(), ".gonk.yml: ") {
			t.Errorf("%s: Load err = %v, want a .gonk.yml error", c.name, err)
		}
	}
}

// Validate on a decoded layer: the Go half of every schema rule, for a Policy
// that never saw a schema.
func TestPipelineFixPolicyValidate(t *testing.T) {
	many := make([]string, scope.MaxPatterns+1)
	for i := range many {
		many[i] = "a/**"
	}
	cases := []struct {
		name  string
		p     PipelineFixPolicy
		layer scope.Layer
		want  string // "" = valid
	}{
		{"zero", PipelineFixPolicy{}, scope.LayerProject, ""},
		{"zero, operator", PipelineFixPolicy{}, scope.LayerOperator, ""},
		{"everything at its ceiling", PipelineFixPolicy{Classes: []string{"code", "test", "configuration"}, Targets: []string{"ref", "mr"},
			AllowCI: bp(true), CIExecChanges: sp("auto"), MaxAttempts: ip(3), MaxChain: ip(10), VerifyTimeout: dp(6 * time.Hour),
			MaxFiles: ip(3), MaxChangedLines: ip(40), Paths: []string{"internal/**"},
			AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval"), Classes: []string{"docs", "ci"}}}, scope.LayerProject, ""},
		{"unknown layer", PipelineFixPolicy{}, scope.Layer("batch"), "unknown layer"},
		{"class flaky", PipelineFixPolicy{Classes: []string{"flaky"}}, scope.LayerProject, "classes"},
		{"class unknown", PipelineFixPolicy{Classes: []string{"code", "unknown"}}, scope.LayerProject, "classes"},
		{"target tag", PipelineFixPolicy{Targets: []string{"tag"}}, scope.LayerProject, "targets"},
		{"auto-merge class source", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Classes: []string{"source"}}}, scope.LayerProject, "auto_merge.classes"},
		{"auto-merge class test", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Classes: []string{"test"}}}, scope.LayerOperator, "auto_merge.classes"},
		{"ci_exec_changes always", PipelineFixPolicy{CIExecChanges: sp("always")}, scope.LayerProject, "ci_exec_changes"},
		{"mode always", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("always")}}, scope.LayerProject, "auto_merge.mode"},
		{"max_attempts 4", PipelineFixPolicy{MaxAttempts: ip(4)}, scope.LayerProject, "max_attempts"},
		{"max_attempts 0", PipelineFixPolicy{MaxAttempts: ip(0)}, scope.LayerProject, "max_attempts"},
		{"max_chain 11", PipelineFixPolicy{MaxChain: ip(11)}, scope.LayerProject, "max_chain"},
		{"max_chain 0", PipelineFixPolicy{MaxChain: ip(0)}, scope.LayerOperator, "max_chain"},
		{"max_files 4", PipelineFixPolicy{MaxFiles: ip(4)}, scope.LayerProject, "max_files"},
		{"max_files -1", PipelineFixPolicy{MaxFiles: ip(-1)}, scope.LayerProject, "max_files"},
		{"max_changed_lines 41", PipelineFixPolicy{MaxChangedLines: ip(41)}, scope.LayerProject, "max_changed_lines"},
		{"max_changed_lines 0", PipelineFixPolicy{MaxChangedLines: ip(0)}, scope.LayerProject, "max_changed_lines"},
		{"verify_timeout 0", PipelineFixPolicy{VerifyTimeout: dp(0)}, scope.LayerProject, "verify_timeout"},
		{"verify_timeout negative", PipelineFixPolicy{VerifyTimeout: dp(-time.Second)}, scope.LayerProject, "verify_timeout"},
		{"verify_timeout past the ceiling", PipelineFixPolicy{VerifyTimeout: dp(6*time.Hour + time.Second)}, scope.LayerProject, "verify_timeout"},
		{"paths from an operator", PipelineFixPolicy{Paths: []string{"internal/**"}}, scope.LayerOperator, "project-only"},
		{"paths with ..", PipelineFixPolicy{Paths: []string{"a/../b"}}, scope.LayerProject, "paths"},
		{"too many paths", PipelineFixPolicy{Paths: many}, scope.LayerProject, "paths"},
	}
	for _, c := range cases {
		err := c.p.Validate(c.layer)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: Validate = %v, want nil", c.name, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: Validate accepted %+v", c.name, c.p)
		case c.want != "" && (!strings.HasPrefix(err.Error(), "pipeline_fix: ") || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want a pipeline_fix error mentioning %q", c.name, err, c.want)
		}
	}
}

// Load runs the Go check, not only the schema: a value the schema's pattern
// lets through (6h is the ceiling; "7h" matches the pattern) is still refused.
func TestLoadRunsTheGoCheck(t *testing.T) {
	for name, body := range map[string]string{
		"verify_timeout past the ceiling": "pipeline_fix: { verify_timeout: 7h }\n",
		"verify_timeout zero":             "pipeline_fix: { verify_timeout: 0s }\n",
		"verify_timeout in seconds, past": "pipeline_fix: { verify_timeout: 21601s }\n",
		"path with a .. segment":          "pipeline_fix: { paths: [\"a/../b\"] }\n",
	} {
		if err := Validate([]byte(fixHead + body)); err != nil {
			t.Fatalf("%s: precondition: the schema is meant to accept this, got %v", name, err)
		}
		_, err := Load([]byte(fixHead + body))
		if err == nil || !strings.HasPrefix(err.Error(), ".gonk.yml: pipeline_fix: ") {
			t.Errorf("%s: Load err = %v, want a .gonk.yml: pipeline_fix: error", name, err)
		}
	}
}

func TestDurationRefusesAUnitlessNumber(t *testing.T) {
	for doc, want := range map[string]time.Duration{"2h": 2 * time.Hour, "90m": 90 * time.Minute, "\"45s\"": 45 * time.Second} {
		var d Duration
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil || time.Duration(d) != want {
			t.Errorf("%s -> %v, %v; want %v", doc, time.Duration(d), err, want)
		}
	}
	for _, doc := range []string{"7200", "2.5", "true", "[2h]", "\"soon\"", "\"\""} {
		var d Duration
		if err := yaml.Unmarshal([]byte(doc), &d); err == nil {
			t.Errorf("%s decoded to %v, want an error", doc, time.Duration(d))
		}
	}
}

// The fold: the project's value (or the default) is the base, and an operator
// layer can only tighten it.
func TestResolvePipelineFix(t *testing.T) {
	def := defaultFix()
	with := func(mut func(*EffectivePipelineFix)) EffectivePipelineFix {
		e := defaultFix()
		mut(&e)
		return e
	}
	cases := []struct {
		name                     string
		instance, group, project PipelineFixPolicy
		want                     EffectivePipelineFix
	}{
		{"nothing set anywhere: the defaults", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{}, def},

		// ---- lists: intersection, canonical order, never widened by an operator
		{"project widens classes to all three", PipelineFixPolicy{}, PipelineFixPolicy{},
			PipelineFixPolicy{Classes: []string{"configuration", "code", "test"}},
			with(func(e *EffectivePipelineFix) { e.Classes = []string{"code", "test", "configuration"} })},
		{"an operator class list cannot add to the default", PipelineFixPolicy{Classes: []string{"code", "test", "configuration"}}, PipelineFixPolicy{}, PipelineFixPolicy{}, def},
		{"an operator class list removes from the project's", PipelineFixPolicy{Classes: []string{"code"}}, PipelineFixPolicy{},
			PipelineFixPolicy{Classes: []string{"code", "test", "configuration"}},
			with(func(e *EffectivePipelineFix) { e.Classes = []string{"code"} })},
		{"instance and group lists both apply", PipelineFixPolicy{Classes: []string{"code", "test"}}, PipelineFixPolicy{Classes: []string{"test", "configuration"}},
			PipelineFixPolicy{Classes: []string{"code", "test", "configuration"}},
			with(func(e *EffectivePipelineFix) { e.Classes = []string{"test"} })},
		{"disjoint lists allow nothing", PipelineFixPolicy{Classes: []string{"configuration"}}, PipelineFixPolicy{}, PipelineFixPolicy{},
			with(func(e *EffectivePipelineFix) { e.Classes = []string{} })},
		{"a project's empty class list allows nothing", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{Classes: []string{}},
			with(func(e *EffectivePipelineFix) { e.Classes = []string{} })},
		{"mr targets are a project opt-in", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{Targets: []string{"mr", "ref"}},
			with(func(e *EffectivePipelineFix) { e.Targets = []string{"ref", "mr"} })},
		{"an operator cannot opt a project in to mr targets", PipelineFixPolicy{Targets: []string{"ref", "mr"}}, PipelineFixPolicy{Targets: []string{"mr", "ref"}}, PipelineFixPolicy{}, def},
		{"a group removes mr targets", PipelineFixPolicy{}, PipelineFixPolicy{Targets: []string{"ref"}}, PipelineFixPolicy{Targets: []string{"ref", "mr"}}, def},

		// ---- allow_ci: project opts in, operator may veto, operator cannot grant
		{"allow_ci: the project opts in", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{AllowCI: bp(true)},
			with(func(e *EffectivePipelineFix) { e.AllowCI = true })},
		{"allow_ci: an operator true grants nothing", PipelineFixPolicy{AllowCI: bp(true)}, PipelineFixPolicy{AllowCI: bp(true)}, PipelineFixPolicy{}, def},
		{"allow_ci: an instance veto wins", PipelineFixPolicy{AllowCI: bp(false)}, PipelineFixPolicy{}, PipelineFixPolicy{AllowCI: bp(true)}, def},
		{"allow_ci: a group veto wins", PipelineFixPolicy{}, PipelineFixPolicy{AllowCI: bp(false)}, PipelineFixPolicy{AllowCI: bp(true)}, def},
		{"allow_ci: a group true does not undo an instance veto", PipelineFixPolicy{AllowCI: bp(false)}, PipelineFixPolicy{AllowCI: bp(true)}, PipelineFixPolicy{AllowCI: bp(true)}, def},

		// ---- ci_exec_changes: strictest wins
		{"exec: the project asks for auto", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{CIExecChanges: sp("auto")},
			with(func(e *EffectivePipelineFix) { e.CIExecChanges = "auto" })},
		{"exec: an operator auto does not loosen the default", PipelineFixPolicy{CIExecChanges: sp("auto")}, PipelineFixPolicy{CIExecChanges: sp("auto")}, PipelineFixPolicy{}, def},
		{"exec: an operator manual-run tightens a project's auto", PipelineFixPolicy{CIExecChanges: sp("manual-run")}, PipelineFixPolicy{}, PipelineFixPolicy{CIExecChanges: sp("auto")}, def},
		{"exec: a group deny beats everything", PipelineFixPolicy{CIExecChanges: sp("auto")}, PipelineFixPolicy{CIExecChanges: sp("deny")}, PipelineFixPolicy{CIExecChanges: sp("auto")},
			with(func(e *EffectivePipelineFix) { e.CIExecChanges = "deny" })},
		{"exec: the project tightens to deny", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{CIExecChanges: sp("deny")},
			with(func(e *EffectivePipelineFix) { e.CIExecChanges = "deny" })},
		{"exec: an unknown value reads as deny", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{CIExecChanges: sp("always")},
			with(func(e *EffectivePipelineFix) { e.CIExecChanges = "deny" })},

		// ---- numbers: minimum, never above the ceiling
		{"numbers: the project tightens", PipelineFixPolicy{}, PipelineFixPolicy{},
			PipelineFixPolicy{MaxAttempts: ip(1), MaxChain: ip(2), MaxFiles: ip(1), MaxChangedLines: ip(10), VerifyTimeout: dp(30 * time.Minute)},
			with(func(e *EffectivePipelineFix) {
				e.MaxAttempts, e.MaxChain, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout = 1, 2, 1, 10, 30*time.Minute
			})},
		{"numbers: the project raises to the ceilings", PipelineFixPolicy{}, PipelineFixPolicy{},
			PipelineFixPolicy{MaxAttempts: ip(3), MaxChain: ip(10), VerifyTimeout: dp(6 * time.Hour)},
			with(func(e *EffectivePipelineFix) { e.MaxAttempts, e.MaxChain, e.VerifyTimeout = 3, 10, 6*time.Hour })},
		{"numbers: an operator value above the default does not raise it",
			PipelineFixPolicy{MaxAttempts: ip(3), MaxChain: ip(10), VerifyTimeout: dp(6 * time.Hour)}, PipelineFixPolicy{}, PipelineFixPolicy{}, def},
		{"numbers: the lowest layer wins",
			PipelineFixPolicy{MaxAttempts: ip(2), MaxChain: ip(4), MaxFiles: ip(2), MaxChangedLines: ip(30), VerifyTimeout: dp(time.Hour)},
			PipelineFixPolicy{MaxAttempts: ip(1), MaxChain: ip(7), MaxFiles: ip(3), MaxChangedLines: ip(20), VerifyTimeout: dp(3 * time.Hour)},
			PipelineFixPolicy{MaxAttempts: ip(3), MaxChain: ip(10), MaxFiles: ip(3), MaxChangedLines: ip(40), VerifyTimeout: dp(6 * time.Hour)},
			with(func(e *EffectivePipelineFix) {
				e.MaxAttempts, e.MaxChain, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout = 1, 4, 2, 20, time.Hour
			})},
		{"numbers: the instance is the lowest for every key",
			PipelineFixPolicy{MaxAttempts: ip(1), MaxChain: ip(2), MaxFiles: ip(1), MaxChangedLines: ip(7), VerifyTimeout: dp(10 * time.Minute)},
			PipelineFixPolicy{MaxAttempts: ip(2), MaxChain: ip(6), MaxFiles: ip(2), MaxChangedLines: ip(25), VerifyTimeout: dp(4 * time.Hour)},
			PipelineFixPolicy{MaxAttempts: ip(3), MaxChain: ip(10), MaxFiles: ip(3), MaxChangedLines: ip(40), VerifyTimeout: dp(6 * time.Hour)},
			with(func(e *EffectivePipelineFix) {
				e.MaxAttempts, e.MaxChain, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout = 1, 2, 1, 7, 10*time.Minute
			})},
		{"numbers: the group is the lowest for every key",
			PipelineFixPolicy{MaxAttempts: ip(2), MaxChain: ip(6), MaxFiles: ip(2), MaxChangedLines: ip(25), VerifyTimeout: dp(4 * time.Hour)},
			PipelineFixPolicy{MaxAttempts: ip(1), MaxChain: ip(2), MaxFiles: ip(1), MaxChangedLines: ip(7), VerifyTimeout: dp(10 * time.Minute)},
			PipelineFixPolicy{MaxAttempts: ip(3), MaxChain: ip(10), MaxFiles: ip(3), MaxChangedLines: ip(40), VerifyTimeout: dp(6 * time.Hour)},
			with(func(e *EffectivePipelineFix) {
				e.MaxAttempts, e.MaxChain, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout = 1, 2, 1, 7, 10*time.Minute
			})},
		{"numbers: an unvalidated value past the ceiling is clamped", PipelineFixPolicy{}, PipelineFixPolicy{},
			PipelineFixPolicy{MaxAttempts: ip(99), MaxChain: ip(99), MaxFiles: ip(99), MaxChangedLines: ip(9999), VerifyTimeout: dp(99 * time.Hour)},
			with(func(e *EffectivePipelineFix) { e.MaxAttempts, e.MaxChain, e.VerifyTimeout = 3, 10, 6*time.Hour })},
		{"numbers: an unvalidated negative is clamped to zero", PipelineFixPolicy{}, PipelineFixPolicy{},
			PipelineFixPolicy{MaxAttempts: ip(-1), MaxChain: ip(-1), MaxFiles: ip(-1), MaxChangedLines: ip(-1), VerifyTimeout: dp(-time.Hour)},
			with(func(e *EffectivePipelineFix) {
				e.MaxAttempts, e.MaxChain, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout = 0, 0, 0, 0, 0
			})},

		// ---- paths: the project's
		{"paths: the project's list", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{Paths: []string{"internal/**", "cmd/**"}},
			with(func(e *EffectivePipelineFix) { e.Paths = []string{"internal/**", "cmd/**"} })},

		// ---- auto-merge: off unless the project asks and no operator disagrees
		{"merge: the project asks", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}},
			with(func(e *EffectivePipelineFix) { e.AutoMerge.Mode = "after-human-approval" })},
		{"merge: an operator mode does not switch it on", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}, PipelineFixPolicy{}, def},
		{"merge: an instance off vetoes", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("off")}}, PipelineFixPolicy{},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}, def},
		{"merge: a group off vetoes", PipelineFixPolicy{}, PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("off")}},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review")}}, def},
		{"merge: an operator naming the same mode allows it", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}, PipelineFixPolicy{},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}},
			with(func(e *EffectivePipelineFix) { e.AutoMerge.Mode = "after-human-approval" })},
		{"merge: an operator naming a different mode turns it off", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review")}}, PipelineFixPolicy{},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}, def},
		{"merge: instance and group disagreeing turn it off", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review")}},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}, def},
		{"merge: an unknown mode reads as off", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("always")}}, def},
		{"merge classes: the project widens to the ceiling", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{AutoMerge: AutoMergePolicy{Classes: []string{"ci", "docs"}}},
			with(func(e *EffectivePipelineFix) { e.AutoMerge.Classes = []string{"docs", "ci"} })},
		{"merge classes: an operator cannot add ci", PipelineFixPolicy{AutoMerge: AutoMergePolicy{Classes: []string{"docs", "ci"}}}, PipelineFixPolicy{}, PipelineFixPolicy{}, def},
		{"merge classes: an operator removes docs", PipelineFixPolicy{}, PipelineFixPolicy{AutoMerge: AutoMergePolicy{Classes: []string{"ci"}}},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{Classes: []string{"docs", "ci"}}},
			with(func(e *EffectivePipelineFix) { e.AutoMerge.Classes = []string{"ci"} })},
		{"merge classes: an unknown one is dropped", PipelineFixPolicy{}, PipelineFixPolicy{}, PipelineFixPolicy{AutoMerge: AutoMergePolicy{Classes: []string{"source", "docs"}}}, def},
	}
	for _, c := range cases {
		got := ResolvePipelineFix(c.instance, c.group, c.project)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, got, c.want)
		}
		// Resolve carries exactly this fold.
		eff := Resolve(Policy{PipelineFix: c.instance}, Policy{PipelineFix: c.group}, ProjectConfig{Policy: Policy{PipelineFix: c.project}})
		if !reflect.DeepEqual(eff.PipelineFix, c.want) {
			t.Errorf("%s: Resolve().PipelineFix = %+v, want %+v", c.name, eff.PipelineFix, c.want)
		}
	}
}

// The property behind "every key can only TIGHTEN": whatever the layers say,
// the result is inside the pack ceilings, and adding an operator layer never
// makes it wider than the same project without that layer.
func TestNoPipelineFixFoldIsWiderThanTheCeiling(t *testing.T) {
	lists := [][]string{nil, {}, {"code"}, {"test", "configuration"}, {"code", "test", "configuration"}, {"flaky", "code"}}
	targets := [][]string{nil, {}, {"ref"}, {"mr"}, {"ref", "mr"}}
	mclasses := [][]string{nil, {}, {"docs"}, {"ci"}, {"docs", "ci"}, {"source"}}
	bools := []*bool{nil, bp(true), bp(false)}
	execs := []*string{nil, sp("deny"), sp("manual-run"), sp("auto"), sp("bogus")}
	modes := []*string{nil, sp("off"), sp("after-human-approval"), sp("after-agentic-review"), sp("bogus")}
	nums := []*int64{nil, ip(-5), ip(0), ip(1), ip(2), ip(3), ip(7), ip(1000)}
	durs := []*Duration{nil, dp(-time.Hour), dp(0), dp(time.Minute), dp(2 * time.Hour), dp(6 * time.Hour), dp(100 * time.Hour)}
	gen := func(i int) PipelineFixPolicy {
		return PipelineFixPolicy{
			Classes: lists[i%len(lists)], Targets: targets[(i/2)%len(targets)], AllowCI: bools[(i/3)%len(bools)],
			CIExecChanges: execs[(i/5)%len(execs)], MaxAttempts: nums[(i/7)%len(nums)], MaxChain: nums[(i/11)%len(nums)],
			VerifyTimeout: durs[(i/13)%len(durs)], MaxFiles: nums[(i/17)%len(nums)], MaxChangedLines: nums[(i/19)%len(nums)],
			AutoMerge: AutoMergePolicy{Mode: modes[(i/23)%len(modes)], Classes: mclasses[(i/29)%len(mclasses)]},
		}
	}
	subset := func(a, b []string) bool {
		for _, v := range a {
			if !contains(b, v) {
				return false
			}
		}
		return true
	}
	n := 0
	for i := 0; i < 4000; i += 7 {
		for j := 1; j < 4000; j += 211 {
			inst, grp, proj := gen(i), gen(j), gen(i*31+j)
			e := ResolvePipelineFix(inst, grp, proj)
			alone := ResolvePipelineFix(PipelineFixPolicy{}, PipelineFixPolicy{}, proj)
			n++
			id := func() string { return "instance " + show(inst) + " group " + show(grp) + " project " + show(proj) }
			if !subset(e.Classes, fixClassOrder) || !subset(e.Targets, fixTargetOrder) || !subset(e.AutoMerge.Classes, autoMergeClassOrder) {
				t.Fatalf("a list left its set: %+v\n%s", e, id())
			}
			if e.MaxAttempts < 0 || e.MaxAttempts > 3 || e.MaxChain < 0 || e.MaxChain > 10 || e.MaxFiles < 0 || e.MaxFiles > 3 ||
				e.MaxChangedLines < 0 || e.MaxChangedLines > 40 || e.VerifyTimeout < 0 || e.VerifyTimeout > 6*time.Hour {
				t.Fatalf("a number left its ceiling: %+v\n%s", e, id())
			}
			if !subset(e.Classes, alone.Classes) || !subset(e.Targets, alone.Targets) || !subset(e.AutoMerge.Classes, alone.AutoMerge.Classes) ||
				e.MaxAttempts > alone.MaxAttempts || e.MaxChain > alone.MaxChain || e.MaxFiles > alone.MaxFiles ||
				e.MaxChangedLines > alone.MaxChangedLines || e.VerifyTimeout > alone.VerifyTimeout ||
				(e.AllowCI && !alone.AllowCI) || ciExecRank[e.CIExecChanges] > ciExecRank[alone.CIExecChanges] ||
				(e.AutoMerge.Mode != "off" && e.AutoMerge.Mode != alone.AutoMerge.Mode) {
				t.Fatalf("an operator layer WIDENED the project's own result:\n with    %+v\n without %+v\n%s", e, alone, id())
			}
			if e.AllowCI && !on(proj.AllowCI) {
				t.Fatalf("allow_ci is on without the project's opt-in: %s", id())
			}
			if _, ok := ciExecRank[e.CIExecChanges]; !ok || !autoMergeModes[e.AutoMerge.Mode] {
				t.Fatalf("an enum left its set: %+v", e)
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d combinations were tried", n)
	}
}

func show(p PipelineFixPolicy) string {
	b, _ := yaml.Marshal(p)
	return "{" + strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", ", ") + "}"
}

// FoldPipelineFix is what a nested group hierarchy goes through
// (opercfg.GroupFor): at least as tight as either layer, in either order.
func TestFoldPipelineFixOnlyTightens(t *testing.T) {
	a := PipelineFixPolicy{Classes: []string{"code", "test"}, AllowCI: bp(false), CIExecChanges: sp("auto"), MaxAttempts: ip(3),
		MaxFiles: ip(1), MaxChangedLines: ip(9), Targets: []string{"ref", "mr"},
		VerifyTimeout: dp(time.Hour), AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval"), Classes: []string{"docs", "ci"}}}
	b := PipelineFixPolicy{Classes: []string{"test", "configuration"}, Targets: []string{"ref"}, AllowCI: bp(true), CIExecChanges: sp("deny"),
		MaxAttempts: ip(1), MaxChain: ip(4), MaxFiles: ip(3), MaxChangedLines: ip(30),
		VerifyTimeout: dp(3 * time.Hour), AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review"), Classes: []string{"ci"}}}
	want := PipelineFixPolicy{Classes: []string{"test"}, Targets: []string{"ref"}, AllowCI: bp(false), CIExecChanges: sp("deny"),
		MaxAttempts: ip(1), MaxChain: ip(4), MaxFiles: ip(1), MaxChangedLines: ip(9), VerifyTimeout: dp(time.Hour), AutoMerge: AutoMergePolicy{Mode: sp("off"), Classes: []string{"ci"}}}
	for name, got := range map[string]PipelineFixPolicy{"a then b": FoldPipelineFix(a, b), "b then a": FoldPipelineFix(b, a)} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %s\n want %s", name, show(got), show(want))
		}
	}
	if got := FoldPipelineFix(PipelineFixPolicy{}, PipelineFixPolicy{}); !got.IsZero() {
		t.Errorf("two silent layers folded to %s", show(got))
	}
	if got := FoldPipelineFix(a, PipelineFixPolicy{}); !reflect.DeepEqual(got, a) {
		t.Errorf("folding a silent layer changed the policy:\n got  %s\n want %s", show(got), show(a))
	}
	// Disjoint lists are "none", which must not read as "silent".
	got := FoldPipelineFix(PipelineFixPolicy{Classes: []string{"code"}}, PipelineFixPolicy{Classes: []string{"test"}})
	if got.Classes == nil || len(got.Classes) != 0 {
		t.Errorf("disjoint class lists folded to %#v, want a non-nil empty list", got.Classes)
	}
}

// Effective.Actions.PipelineFixes: the project opts in, any operator layer may
// veto, and it is never true without effective diagnoses.
func TestResolvePipelineFixesAction(t *testing.T) {
	cases := []struct {
		name                     string
		instance, group, project ActionsPolicy
		want                     bool
	}{
		{"project opts in to both", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true)}, true},
		{"project silent", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{Pipelines: bp(true)}, false},
		{"project says false", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(false)}, false},
		{"an operator true opts nobody in", ActionsPolicy{PipelineFixes: bp(true)}, ActionsPolicy{PipelineFixes: bp(true)}, ActionsPolicy{Pipelines: bp(true)}, false},
		{"instance vetoes fixes", ActionsPolicy{PipelineFixes: bp(false)}, ActionsPolicy{}, ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true)}, false},
		{"group vetoes fixes", ActionsPolicy{}, ActionsPolicy{PipelineFixes: bp(false)}, ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true)}, false},
		{"instance vetoes DIAGNOSES: fixes go with them", ActionsPolicy{Pipelines: bp(false)}, ActionsPolicy{}, ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true)}, false},
		{"group vetoes diagnoses", ActionsPolicy{}, ActionsPolicy{Pipelines: bp(false)}, ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true)}, false},
		{"a Go-built project with fixes and no diagnoses", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{PipelineFixes: bp(true)}, false},
	}
	for _, c := range cases {
		eff := Resolve(Policy{Actions: c.instance, Ladder: []string{"qwen-local"}}, Policy{Actions: c.group},
			ProjectConfig{Policy: Policy{Enabled: bp(true), Actions: c.project, Ladder: []string{"qwen-local"}}})
		if eff.Actions.PipelineFixes != c.want {
			t.Errorf("%s: Actions.PipelineFixes = %v, want %v", c.name, eff.Actions.PipelineFixes, c.want)
		}
		if eff.Actions.PipelineFixes && !eff.Actions.Pipelines {
			t.Errorf("%s: fixes are on while diagnoses are off", c.name)
		}
	}
}

// `allow_ci` reaches pkg/scope as scope.Input.LiftOptIn, and nothing else
// about the section can produce a lift.
func TestAllowCIMapsOntoTheScopeLiftOptIn(t *testing.T) {
	row := scope.Row{Classes: []scope.Class{scope.ClassSource, scope.ClassDocs}, Liftable: []scope.Class{scope.ClassCI},
		Caps: scope.Caps{MaxFiles: 3, MaxLines: 40, MaxFileBytes: 262144, MaxPatchBytes: 16384}}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name      string
		yaml      string
		operator  scope.Policy
		wantLift  []scope.Class
		wantFiles int64
		wantLines int64
	}{
		{"default: no lift", "", scope.Policy{}, []scope.Class{}, 3, 40},
		{"allow_ci false: no lift", "pipeline_fix: { allow_ci: false }\n", scope.Policy{}, []scope.Class{}, 3, 40},
		{"allow_ci true: the ci lift", "pipeline_fix: { allow_ci: true }\n", scope.Policy{}, []scope.Class{scope.ClassCI}, 3, 40},
		{"allow_ci true, operator deny_lift: no lift", "pipeline_fix: { allow_ci: true }\n", scope.Policy{DenyLift: []scope.Class{scope.ClassCI}}, []scope.Class{}, 3, 40},
		{"configuration listed without allow_ci: no lift", "pipeline_fix: { classes: [configuration], ci_exec_changes: auto }\n", scope.Policy{}, []scope.Class{}, 3, 40},
		{"max_files and max_changed_lines tighten the scope caps", "pipeline_fix: { max_files: 1, max_changed_lines: 12 }\n", scope.Policy{}, []scope.Class{}, 1, 12},
		{"a lower scope.caps stands", "pipeline_fix: { max_files: 2, max_changed_lines: 30 }\nscope: { caps: { max_files: 1, max_lines: 5 } }\n", scope.Policy{}, []scope.Class{}, 1, 5},
		{"a higher scope.caps does not undo pipeline_fix", "pipeline_fix: { max_files: 2, max_changed_lines: 30 }\nscope: { caps: { max_files: 3, max_lines: 40 } }\n", scope.Policy{}, []scope.Class{}, 2, 30},
	}
	for _, c := range cases {
		pc, err := Load([]byte(fixHead + c.yaml))
		if err != nil {
			t.Errorf("%s: Load: %v", c.name, err)
			continue
		}
		eff := Resolve(Policy{}, Policy{}, withLadder(*pc))
		folded, err := scope.Fold(scope.Input{
			Row: row, Instance: c.operator, Project: eff.PipelineFix.TightenScope(pc.Scope), BaseSHA: sha,
			Enabled: eff.Actions.PipelineFixes, LiftOptIn: eff.PipelineFix.LiftOptIn(),
		})
		if err != nil {
			t.Errorf("%s: Fold: %v", c.name, err)
			continue
		}
		if got := folded.Liftable(); !reflect.DeepEqual(got, c.wantLift) {
			t.Errorf("%s: liftable = %v, want %v", c.name, got, c.wantLift)
		}
		_, gerr := folded.GrantLift(scope.Grant{Trigger: "pipeline-fix", Workflow: "configuration", Classes: []scope.Class{scope.ClassCI}})
		if granted := gerr == nil; granted != (len(c.wantLift) == 1) {
			t.Errorf("%s: GrantLift(ci) err = %v, want granted=%v", c.name, gerr, len(c.wantLift) == 1)
		}
		if folded.Caps.MaxFiles != c.wantFiles || folded.Caps.MaxLines != c.wantLines {
			t.Errorf("%s: caps = files %d lines %d, want %d / %d", c.name, folded.Caps.MaxFiles, folded.Caps.MaxLines, c.wantFiles, c.wantLines)
		}
	}
	// TightenScope does not modify the policy it was given.
	orig := scope.Policy{Caps: scope.CapsPolicy{MaxFiles: ip(9)}}
	_ = defaultFix().TightenScope(orig)
	if *orig.Caps.MaxFiles != 9 {
		t.Error("TightenScope wrote through to the caller's policy")
	}
}

// Owner rule: schema changes are documented where the schema lives. Every key
// of the new surface carries a description in the published schema, and is
// named in the reference document.
func TestEveryPipelineFixKeyIsDocumented(t *testing.T) {
	var doc struct {
		Properties struct {
			Actions struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"actions"`
			PipelineFix struct {
				Description string                    `json:"description"`
				Properties  map[string]map[string]any `json:"properties"`
			} `json:"pipeline_fix"`
		} `json:"properties"`
	}
	raw, err := os.ReadFile("../../docs/schemas/gonk-config.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile("../../docs/reference/gonk-pipeline-fix.md")
	if err != nil {
		t.Fatalf("the reference document is missing: %v", err)
	}
	if len(doc.Properties.Actions.Properties["pipeline_fixes"].Description) < 40 || doc.Properties.PipelineFix.Description == "" {
		t.Error("actions.pipeline_fixes or pipeline_fix has no description in the schema")
	}
	want := []string{"classes", "targets", "allow_ci", "ci_exec_changes", "max_attempts", "max_chain", "verify_timeout",
		"max_files", "max_changed_lines", "paths", "auto_merge"}
	if len(doc.Properties.PipelineFix.Properties) != len(want) {
		t.Errorf("the schema has %d pipeline_fix keys, this test knows %d: add the new key here and to the reference", len(doc.Properties.PipelineFix.Properties), len(want))
	}
	keys := append([]string{"pipeline_fixes"}, want...)
	for _, k := range want {
		p, ok := doc.Properties.PipelineFix.Properties[k]
		if !ok {
			t.Errorf("pipeline_fix.%s is not in the schema", k)
			continue
		}
		if d, _ := p["description"].(string); len(d) < 40 {
			t.Errorf("pipeline_fix.%s has no real description in the schema: %q", k, d)
		}
		if k == "auto_merge" {
			sub, _ := p["properties"].(map[string]any)
			for _, sk := range []string{"mode", "classes", "max_per_day"} {
				m, _ := sub[sk].(map[string]any)
				if d, _ := m["description"].(string); len(d) < 40 {
					t.Errorf("pipeline_fix.auto_merge.%s has no real description in the schema", sk)
				}
				keys = append(keys, "auto_merge."+sk)
			}
		}
	}
	for _, k := range keys {
		if !strings.Contains(string(ref), "`"+k+"`") {
			t.Errorf("docs/reference/gonk-pipeline-fix.md never names `%s`", k)
		}
	}
}
