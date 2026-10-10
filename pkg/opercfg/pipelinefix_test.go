package opercfg

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/gonkcfg"
)

// The operator layer's `actions.pipeline_fixes` and `pipeline_fix:` (gonk-2usw
// plan 3.1), on `instance` and on each group. Documented in
// docs/reference/gonk-pipeline-fix.md and in the schema's own descriptions.

func withFix(instanceBody, groupBody string) string {
	doc := validOperatorYAML
	if instanceBody != "" {
		doc = strings.Replace(doc, "instance:\n  enabled: true\n", "instance:\n  enabled: true\n"+instanceBody, 1)
	}
	if groupBody != "" {
		doc = strings.Replace(doc, "  agentic:\n", "  agentic:\n"+groupBody, 1)
	}
	return doc
}

func TestLoadDecodesOperatorPipelineFix(t *testing.T) {
	oc, err := Load([]byte(withFix(
		"  actions: { pipeline_fixes: false }\n  pipeline_fix:\n    classes: [code]\n    targets: [ref]\n    allow_ci: false\n    ci_exec_changes: deny\n"+
			"    max_attempts: 1\n    max_chain: 3\n    verify_timeout: 45m\n    max_files: 2\n    max_changed_lines: 20\n    auto_merge: { mode: \"off\", classes: [docs] }\n",
		"    pipeline_fix: { max_chain: 2, auto_merge: { mode: after-human-approval } }\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	in := oc.Instance
	if in.Actions.PipelineFixes == nil || *in.Actions.PipelineFixes {
		t.Errorf("instance actions.pipeline_fixes = %v, want false", in.Actions.PipelineFixes)
	}
	f := in.PipelineFix
	if !reflect.DeepEqual(f.Classes, []string{"code"}) || !reflect.DeepEqual(f.Targets, []string{"ref"}) || f.AllowCI == nil || *f.AllowCI ||
		f.CIExecChanges == nil || *f.CIExecChanges != "deny" || f.MaxAttempts == nil || *f.MaxAttempts != 1 || f.MaxChain == nil || *f.MaxChain != 3 ||
		f.VerifyTimeout == nil || time.Duration(*f.VerifyTimeout) != 45*time.Minute || f.MaxFiles == nil || *f.MaxFiles != 2 ||
		f.MaxChangedLines == nil || *f.MaxChangedLines != 20 || f.AutoMerge.Mode == nil || *f.AutoMerge.Mode != "off" ||
		!reflect.DeepEqual(f.AutoMerge.Classes, []string{"docs"}) {
		t.Errorf("instance pipeline_fix = %+v", f)
	}
	g := oc.Groups["agentic"].PipelineFix
	if g.MaxChain == nil || *g.MaxChain != 2 || g.AutoMerge.Mode == nil || *g.AutoMerge.Mode != "after-human-approval" {
		t.Errorf("group pipeline_fix = %+v", g)
	}
}

func TestLoadWithoutPipelineFixIsTheZeroPolicy(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !oc.Instance.PipelineFix.IsZero() || !oc.Groups["agentic"].PipelineFix.IsZero() || oc.Instance.Actions.PipelineFixes != nil {
		t.Error("an operator config with no pipeline-fix keys has a non-zero pipeline-fix policy")
	}
	if !oc.GroupFor("agentic/x/repo").PipelineFix.IsZero() || oc.GroupFor("agentic/x/repo").Actions.PipelineFixes != nil {
		t.Error("GroupFor invented a pipeline-fix policy")
	}
}

// The meter refuses to START on an operator pipeline_fix it cannot make sense
// of, at every operator layer. PLAN, must be seen red: "unknown key accepted".
func TestLoadRejectsInvalidOperatorPipelineFix(t *testing.T) {
	cases := map[string]struct{ instance, group, want string }{
		"instance unknown key":         {"  pipeline_fix: { banana: 1 }\n", "", "pipeline_fix"},
		"group unknown key":            {"", "    pipeline_fix: { banana: 1 }\n", "pipeline_fix"},
		"instance unknown merge key":   {"  pipeline_fix: { auto_merge: { banana: 1 } }\n", "", "auto_merge"},
		"instance paths":               {"  pipeline_fix: { paths: [\"internal/**\"] }\n", "", "pipeline_fix"},
		"group paths":                  {"", "    pipeline_fix: { paths: [\"internal/**\"] }\n", "pipeline_fix"},
		"instance class flaky":         {"  pipeline_fix: { classes: [flaky] }\n", "", "classes"},
		"group target tag":             {"", "    pipeline_fix: { targets: [tag] }\n", "targets"},
		"instance attempts over":       {"  pipeline_fix: { max_attempts: 4 }\n", "", "max_attempts"},
		"group chain zero":             {"", "    pipeline_fix: { max_chain: 0 }\n", "max_chain"},
		"instance exec always":         {"  pipeline_fix: { ci_exec_changes: always }\n", "", "ci_exec_changes"},
		"group mode always":            {"", "    pipeline_fix: { auto_merge: { mode: always } }\n", "mode"},
		"instance merge class source":  {"  pipeline_fix: { auto_merge: { classes: [source] } }\n", "", "classes"},
		"instance timeout over (Go)":   {"  pipeline_fix: { verify_timeout: 7h }\n", "", "instance: pipeline_fix: verify_timeout"},
		"group timeout zero (Go)":      {"", "    pipeline_fix: { verify_timeout: 0s }\n", "group agentic: pipeline_fix: verify_timeout"},
		"instance unknown action key":  {"  actions: { pipeline_fix: false }\n", "", "actions"},
		"instance action not a bool":   {"  actions: { pipeline_fixes: \"no\" }\n", "", "pipeline_fixes"},
		"group timeout a bare integer": {"", "    pipeline_fix: { verify_timeout: 3600 }\n", "verify_timeout"},
	}
	for name, c := range cases {
		_, err := Load([]byte(withFix(c.instance, c.group)))
		if err == nil {
			t.Errorf("%s: Load accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "operator config: ") {
			t.Errorf("%s: err = %v, want an operator config error mentioning %q", name, err, c.want)
		}
	}
}

// checkPolicy is the Go half: it refuses what a config built without the
// schema could carry, on every operator layer, and names the layer.
func TestCheckPolicyValidatesPipelineFix(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	n := int64(99)
	for _, who := range []string{"instance", "group agentic"} {
		for name, p := range map[string]gonkcfg.PipelineFixPolicy{
			"paths":        {Paths: []string{"internal/**"}},
			"over ceiling": {MaxFiles: &n},
			"bad class":    {Classes: []string{"flaky"}},
		} {
			err := oc.checkPolicy(who, gonkcfg.Policy{PipelineFix: p})
			if err == nil || !strings.HasPrefix(err.Error(), "operator config: "+who+": pipeline_fix: ") {
				t.Errorf("%s / %s: err = %v, want operator config: %s: pipeline_fix: ...", who, name, err, who)
			}
		}
	}
}

// GroupFor folds nested groups so that a child can only tighten its parent:
// the veto sticks, lists intersect, numbers take the minimum.
func TestGroupForFoldsPipelineFixTightenOnly(t *testing.T) {
	oc, err := Load([]byte(`
version: 1
instance: { ladder: [a] }
groups:
  top:
    actions: { pipeline_fixes: false }
    pipeline_fix: { classes: [code, test], allow_ci: false, max_attempts: 2, max_chain: 4, verify_timeout: 1h, ci_exec_changes: manual-run, auto_merge: { mode: "off" } }
  top/sub:
    actions: { pipeline_fixes: true }
    pipeline_fix: { classes: [test, configuration], allow_ci: true, max_attempts: 3, max_chain: 2, verify_timeout: 4h, ci_exec_changes: auto, auto_merge: { mode: after-human-approval } }
  other:
    pipeline_fix: { max_files: 1 }
rungs:
  - { name: a, kind: local, model: m, est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.2 }
`))
	if err != nil {
		t.Fatal(err)
	}
	g := oc.GroupFor("top/sub/repo")
	if g.Actions.PipelineFixes == nil || *g.Actions.PipelineFixes {
		t.Errorf("a nested group re-enabled pipeline_fixes its parent vetoed: %v", g.Actions.PipelineFixes)
	}
	f := g.PipelineFix
	if !reflect.DeepEqual(f.Classes, []string{"test"}) || f.AllowCI == nil || *f.AllowCI || *f.MaxAttempts != 2 || *f.MaxChain != 2 ||
		time.Duration(*f.VerifyTimeout) != time.Hour || *f.CIExecChanges != "manual-run" || *f.AutoMerge.Mode != "off" {
		t.Errorf("nested fold = %+v, want classes [test], allow_ci false, attempts 2, chain 2, 1h, manual-run, mode off", f)
	}
	// A single matching group is carried, not dropped.
	if o := oc.GroupFor("other/repo").PipelineFix; o.MaxFiles == nil || *o.MaxFiles != 1 {
		t.Errorf("a single group's pipeline_fix did not reach GroupFor: %+v", o)
	}
	// And the whole chain reaches an Effective: the project asks for the
	// widest of everything and gets the operator's bounds.
	pc, err := gonkcfg.Load([]byte("version: 1\nenabled: true\nladder: [a]\nactions: { pipelines: true, pipeline_fixes: true }\n" +
		"pipeline_fix: { classes: [code, test, configuration], allow_ci: true, max_attempts: 3, max_chain: 10, verify_timeout: 6h, ci_exec_changes: auto, auto_merge: { mode: after-human-approval } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	eff := gonkcfg.Resolve(oc.Instance, g, *pc)
	e := eff.PipelineFix
	if eff.Actions.PipelineFixes || !reflect.DeepEqual(e.Classes, []string{"test"}) || e.AllowCI || e.MaxAttempts != 2 || e.MaxChain != 2 ||
		e.VerifyTimeout != time.Hour || e.CIExecChanges != "manual-run" || e.AutoMerge.Mode != "off" {
		t.Errorf("effective = fixes %v, %+v", eff.Actions.PipelineFixes, e)
	}
}

// The operator SCHEMA on its own (Load also runs the Go check, which would
// hide a schema that stopped refusing these): unknown keys and the
// project-only `paths` are refused by the published contract itself.
func TestOperatorSchemaAloneRefusesBadPipelineFix(t *testing.T) {
	policy := func(fix map[string]any, actions map[string]any) map[string]any {
		p := map[string]any{}
		if fix != nil {
			p["pipeline_fix"] = fix
		}
		if actions != nil {
			p["actions"] = actions
		}
		return p
	}
	doc := func(layer string, p map[string]any) map[string]any {
		d := map[string]any{"version": 1, "rungs": []any{map[string]any{"name": "a", "kind": "local", "model": "m", "est_tokens": "200K"}}}
		if layer == "instance" {
			d["instance"] = p
		} else {
			d["groups"] = map[string]any{"agentic": p}
		}
		return d
	}
	bad := map[string]map[string]any{
		"unknown key":           policy(map[string]any{"banana": 1}, nil),
		"unknown merge key":     policy(map[string]any{"auto_merge": map[string]any{"banana": 1}}, nil),
		"paths":                 policy(map[string]any{"paths": []any{"internal/**"}}, nil),
		"class flaky":           policy(map[string]any{"classes": []any{"flaky"}}, nil),
		"target tag":            policy(map[string]any{"targets": []any{"tag"}}, nil),
		"exec always":           policy(map[string]any{"ci_exec_changes": "always"}, nil),
		"mode always":           policy(map[string]any{"auto_merge": map[string]any{"mode": "always"}}, nil),
		"merge class source":    policy(map[string]any{"auto_merge": map[string]any{"classes": []any{"source"}}}, nil),
		"attempts over":         policy(map[string]any{"max_attempts": 4}, nil),
		"chain over":            policy(map[string]any{"max_chain": 11}, nil),
		"files over":            policy(map[string]any{"max_files": 4}, nil),
		"lines over":            policy(map[string]any{"max_changed_lines": 41}, nil),
		"attempts zero":         policy(map[string]any{"max_attempts": 0}, nil),
		"timeout days":          policy(map[string]any{"verify_timeout": "1d"}, nil),
		"timeout a number":      policy(map[string]any{"verify_timeout": 3600}, nil),
		"allow_ci a string":     policy(map[string]any{"allow_ci": "no"}, nil),
		"unknown action":        policy(nil, map[string]any{"pipeline_fix": false}),
		"action not a boolean":  policy(nil, map[string]any{"pipeline_fixes": "no"}),
		"scaffold stays absent": policy(nil, map[string]any{"scaffold": true}),
	}
	for name, p := range bad {
		for _, layer := range []string{"instance", "group"} {
			if err := compiled.Validate(doc(layer, p)); err == nil {
				t.Errorf("%s (%s): the operator schema accepted %v", name, layer, p)
			}
		}
	}
	good := policy(map[string]any{"classes": []any{"code", "test", "configuration"}, "targets": []any{"ref", "mr"}, "allow_ci": false,
		"ci_exec_changes": "deny", "max_attempts": 3, "max_chain": 10, "verify_timeout": "6h", "max_files": 3, "max_changed_lines": 40,
		"auto_merge": map[string]any{"mode": "off", "classes": []any{"docs", "ci"}}}, map[string]any{"pipeline_fixes": false})
	for _, layer := range []string{"instance", "group"} {
		if err := compiled.Validate(doc(layer, good)); err != nil {
			t.Errorf("%s: the operator schema refused a valid pipeline_fix: %v", layer, err)
		}
	}
}
