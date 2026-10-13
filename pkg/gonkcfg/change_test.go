package gonkcfg

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// The issue -> merge request configuration (gonk-cuwn plan 2.1, 2.7, owner
// decisions 8.1): the `change:` section. Documented in
// docs/reference/gonk-change.md and in the schema's own descriptions.

// changeExampleYAML is plan 2.7's example with owner decision Q2 applied
// (`triggers`, a list of request forms, replaces `trigger`): every value is
// the documented default.
const changeExampleYAML = `
version: 1
enabled: true
actions:
  features: true
change:
  triggers: [label, command, mention]
  create_classes: [test, docs]
  require_docs: when-labelled
  docs_labels: [feature, enhancement, "type::feature"]
  require_test_report: false
  max_attempts: 2
  max_open: 1
  verify_timeout: 2h
  max_files: 6
  max_changed_lines: 200
  paths: []
  auto_merge:
    mode: off
    classes: [docs]
    max_per_day: 3
`

const changeHead = "version: 1\nenabled: true\nactions: { features: true }\n"

// defaultChange is what a project with `actions.features: true` and no
// `change:` section gets, spelled out as literals so a changed constant fails
// here.
func defaultChange() EffectiveChange {
	return EffectiveChange{
		Triggers: []string{"label"}, CreateClasses: []string{"test", "docs"},
		RequireDocs: "when-labelled", DocsLabels: []string{"feature", "enhancement", "type::feature"},
		RequireTestReport: false, MaxAttempts: 2, MaxOpen: 1, VerifyTimeout: 2 * time.Hour,
		MaxFiles: 6, MaxChangedLines: 200, Paths: []string{},
		AutoMerge: EffectiveAutoMerge{Mode: "off", Classes: []string{"docs"}, MaxPerDay: 3},
	}
}

func TestLoadDecodesTheChangeExample(t *testing.T) {
	pc, err := Load([]byte(changeExampleYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := ChangePolicy{
		Triggers: []string{"label", "command", "mention"}, CreateClasses: []string{"test", "docs"},
		RequireDocs: sp("when-labelled"), DocsLabels: []string{"feature", "enhancement", "type::feature"},
		RequireTestReport: bp(false), MaxAttempts: ip(2), MaxOpen: ip(1), VerifyTimeout: dp(2 * time.Hour),
		MaxFiles: ip(6), MaxChangedLines: ip(200), Paths: []string{},
		AutoMerge: AutoMergePolicy{Mode: sp("off"), Classes: []string{"docs"}, MaxPerDay: ip(3)},
	}
	if !reflect.DeepEqual(pc.Change, want) {
		t.Errorf("Change = %+v\nwant     %+v", pc.Change, want)
	}
	eff := Resolve(Policy{Ladder: []string{"qwen-local"}}, Policy{}, withLadder(*pc))
	// The example spells out every key at its default except `triggers`, where
	// it opts in to all three forms (the default is label only).
	wantEff := defaultChange()
	wantEff.Triggers = []string{"label", "command", "mention"}
	if !eff.Actions.Features || !reflect.DeepEqual(eff.Change, wantEff) {
		t.Errorf("effective = features %v, %+v\nwant true, %+v", eff.Actions.Features, eff.Change, wantEff)
	}
}

// Default OFF: a config that predates the feature decodes to the zero policy
// and resolves to the conservative defaults with nothing switched on.
func TestExistingConfigsHaveNoChangeOrReview(t *testing.T) {
	for name, doc := range map[string]string{
		"spec example":     validYAML,
		"pipelines only":   "version: 1\nenabled: true\nladder: [qwen-local]\nactions: { triage: true, pipelines: true }\n",
		"fixes on":         planExampleYAML + "ladder: [qwen-local]\n",
		"features on":      "version: 1\nenabled: true\nladder: [qwen-local]\nactions: { features: true }\n",
		"scope section":    scopeYAML + "ladder: [qwen-local]\n",
		"nothing opted in": "version: 1\nenabled: true\nladder: [qwen-local]\n",
	} {
		pc, err := Load([]byte(doc))
		if err != nil {
			t.Errorf("%s: Load: %v", name, err)
			continue
		}
		if !pc.Change.IsZero() || !pc.Review.IsZero() || pc.Actions.Reviews != nil {
			t.Errorf("%s: a config with no change/review keys decoded to %+v / %+v / %v", name, pc.Change, pc.Review, pc.Actions.Reviews)
		}
		eff := Resolve(Policy{Ladder: []string{"qwen-local"}}, Policy{}, *pc)
		if eff.Actions.Reviews {
			t.Errorf("%s: reviews are ON for a project that never asked", name)
		}
		if !reflect.DeepEqual(eff.Change, defaultChange()) {
			t.Errorf("%s: Change = %+v, want the defaults", name, eff.Change)
		}
		if !reflect.DeepEqual(eff.Review, defaultReview("reviews-off")) {
			t.Errorf("%s: Review = %+v, want the defaults with reviews off", name, eff.Review)
		}
		if eff.Change.AutoMerge.Mode != "off" || eff.Change.AllowsCreateClass("source") || eff.Change.RequireDocs == "never" ||
			eff.Review.PipelineFixMerge.Available || eff.Review.ChangeMerge.Available || eff.Review.Rung != "" {
			t.Errorf("%s: a default is not the conservative one: %+v / %+v", name, eff.Change, eff.Review)
		}
	}
}

// PLAN, must be seen red: "unknown key accepted", "a ceiling exceeded is
// accepted", "`trigger: triage-verdict` is the default or accepted". These go
// through Validate -- the SCHEMA alone.
func TestSchemaRefusesBadChange(t *testing.T) {
	cases := map[string]string{
		"unknown key in change":               "change: { banana: true }\n",
		"unknown key in change.auto_merge":    "change: { auto_merge: { banana: true } }\n",
		"misspelt section":                    "changes: { max_files: 1 }\n",
		"the plan's old singular trigger key": "change: { trigger: label }\n",
		"trigger triage-verdict":              "change: { triggers: [triage-verdict] }\n",
		"trigger triage-verdict beside label": "change: { triggers: [label, triage-verdict] }\n",
		"trigger unknown":                     "change: { triggers: [webhook] }\n",
		"duplicate trigger":                   "change: { triggers: [label, label] }\n",
		"triggers not a list":                 "change: { triggers: label }\n",
		"create class deps":                   "change: { create_classes: [deps] }\n",
		"create class ci":                     "change: { create_classes: [test, ci] }\n",
		"create class other":                  "change: { create_classes: [other] }\n",
		"require_docs sometimes":              "change: { require_docs: sometimes }\n",
		"require_docs a boolean":              "change: { require_docs: true }\n",
		"docs label empty":                    "change: { docs_labels: [\"\"] }\n",
		"docs label with a leading space":     "change: { docs_labels: [\" feature\"] }\n",
		"docs label with a newline":           "change: { docs_labels: [\"a\\nb\"] }\n",
		"docs label a number":                 "change: { docs_labels: [7] }\n",
		"docs label duplicated":               "change: { docs_labels: [feature, feature] }\n",
		"seventeen docs labels":               "change: { docs_labels: [a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q] }\n",
		"docs label 256 characters":           "change: { docs_labels: [" + strings.Repeat("x", 256) + "] }\n",
		"require_test_report a string":        "change: { require_test_report: \"yes\" }\n",
		"max_attempts above the ceiling":      "change: { max_attempts: 4 }\n",
		"max_attempts zero":                   "change: { max_attempts: 0 }\n",
		"max_open above the ceiling":          "change: { max_open: 4 }\n",
		"max_open zero":                       "change: { max_open: 0 }\n",
		"max_files above the ceiling":         "change: { max_files: 9 }\n",
		"max_files zero":                      "change: { max_files: 0 }\n",
		"max_changed_lines above the ceiling": "change: { max_changed_lines: 301 }\n",
		"max_changed_lines fractional":        "change: { max_changed_lines: 1.5 }\n",
		"verify_timeout a bare number":        "change: { verify_timeout: 7200 }\n",
		"verify_timeout days":                 "change: { verify_timeout: 1d }\n",
		"paths absolute":                      "change: { paths: [\"/etc/**\"] }\n",
		"paths not a list":                    "change: { paths: \"internal/**\" }\n",
		"mode always":                         "change: { auto_merge: { mode: always } }\n",
		"merge class ci":                      "change: { auto_merge: { classes: [ci] } }\n",
		"merge class source":                  "change: { auto_merge: { classes: [docs, source] } }\n",
		"merge class test":                    "change: { auto_merge: { classes: [test] } }\n",
		"max_per_day above the ceiling":       "change: { auto_merge: { max_per_day: 4 } }\n",
		"max_per_day zero":                    "change: { auto_merge: { max_per_day: 0 } }\n",
		"fix max_per_day above the ceiling":   "pipeline_fix: { auto_merge: { max_per_day: 4 } }\n",
		"fix max_per_day zero":                "pipeline_fix: { auto_merge: { max_per_day: 0 } }\n",
		"fix max_per_day a string":            "pipeline_fix: { auto_merge: { max_per_day: \"3\" } }\n",
		"auto_merge a string":                 "change: { auto_merge: after-human-approval }\n",
		"change a list":                       "change: [label]\n",
	}
	for name, body := range cases {
		if err := Validate([]byte(changeHead + body)); err == nil {
			t.Errorf("%s: the schema accepted\n%s", name, body)
		}
	}
}

func TestSchemaAcceptsEveryDocumentedChangeValue(t *testing.T) {
	for name, body := range map[string]string{
		"one trigger form":       "change: { triggers: [command] }\n",
		"no trigger form at all": "change: { triggers: [] }\n",
		"create source too":      "change: { create_classes: [source, test, docs] }\n",
		"create nothing":         "change: { create_classes: [] }\n",
		"docs never":             "change: { require_docs: never }\n",
		"docs always":            "change: { require_docs: always }\n",
		"labels with spaces":     "change: { docs_labels: [\"needs docs\", \"kind::feature\"] }\n",
		"no docs labels":         "change: { docs_labels: [] }\n",
		"sixteen docs labels":    "change: { docs_labels: [a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p] }\n",
		"a 255-character label":  "change: { docs_labels: [" + strings.Repeat("x", 255) + "] }\n",
		"require_test_report":    "change: { require_test_report: true }\n",
		"the ceilings":           "change: { max_attempts: 3, max_open: 3, max_files: 8, max_changed_lines: 300, verify_timeout: 6h, auto_merge: { max_per_day: 3 } }\n",
		"the floors":             "change: { max_attempts: 1, max_open: 1, max_files: 1, max_changed_lines: 1, verify_timeout: 1s, auto_merge: { max_per_day: 1 } }\n",
		"paths":                  "change: { paths: [\"internal/**\", \"docs/*.md\"] }\n",
		"after-human-approval":   "change: { auto_merge: { mode: after-human-approval, classes: [docs] } }\n",
		"mode off, quoted":       "change: { auto_merge: { mode: \"off\", classes: [] } }\n",
		"fix max_per_day":        "pipeline_fix: { auto_merge: { max_per_day: 2 } }\n",
		"section without action": "",
	} {
		if _, err := Load([]byte(changeHead + body)); err != nil {
			t.Errorf("%s: Load refused\n%s%v", name, body, err)
		}
	}
	// A staged section: the block is written, the action is not switched on.
	if _, err := Load([]byte("version: 1\nenabled: true\nchange: { triggers: [label], max_files: 1, auto_merge: { mode: \"off\", max_per_day: 1 } }\n")); err != nil {
		t.Errorf("a change section without the action was refused: %v", err)
	}
}

// PLAN, must be seen red: a requires-relation is enforced. `change.auto_merge`
// may not be switched on without the workflow, and its agentic-review mode not
// without reviews. Two guards, tested apart: the schema (Validate) and
// CheckChange (a decoded Policy).
func TestChangeAutoMergeRequiresItsActions(t *testing.T) {
	head := "version: 1\nenabled: true\n"
	cases := []struct {
		name    string
		doc     string
		policy  Policy
		invalid string // "" = valid; else a substring of the Go error
	}{
		{"human-approval merge without features", "change: { auto_merge: { mode: after-human-approval } }\n",
			Policy{Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}}, "actions.features"},
		{"human-approval merge with features false", "actions: { features: false }\nchange: { auto_merge: { mode: after-human-approval } }\n",
			Policy{Actions: ActionsPolicy{Features: bp(false)}, Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}}, "actions.features"},
		{"human-approval merge with other actions only", "actions: { triage: true, pipelines: true, pipeline_fixes: true }\nchange: { auto_merge: { mode: after-human-approval } }\n",
			Policy{Actions: ActionsPolicy{Triage: bp(true), Pipelines: bp(true), PipelineFixes: bp(true)}, Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}}, "actions.features"},
		{"agentic-review merge without features", "actions: { pipelines: true, pipeline_fixes: true, reviews: true }\nchange: { auto_merge: { mode: after-agentic-review } }\n",
			Policy{Actions: ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true), Reviews: bp(true)}, Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review")}}}, "actions.features"},
		{"agentic-review merge without reviews", "actions: { features: true }\nchange: { auto_merge: { mode: after-agentic-review } }\n",
			Policy{Actions: ActionsPolicy{Features: bp(true)}, Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review")}}}, "actions.reviews"},
		{"agentic-review merge with reviews false", "actions: { features: true, reviews: false }\nchange: { auto_merge: { mode: after-agentic-review } }\n",
			Policy{Actions: ActionsPolicy{Features: bp(true), Reviews: bp(false)}, Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review")}}}, "actions.reviews"},
		{"human-approval merge with features", "actions: { features: true }\nchange: { auto_merge: { mode: after-human-approval } }\n",
			Policy{Actions: ActionsPolicy{Features: bp(true)}, Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval")}}}, ""},
		{"agentic-review merge with features and reviews", "actions: { features: true, reviews: true }\nchange: { auto_merge: { mode: after-agentic-review } }\n",
			Policy{Actions: ActionsPolicy{Features: bp(true), Reviews: bp(true)}, Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review")}}}, ""},
		{"mode off without features", "change: { auto_merge: { mode: \"off\" } }\n",
			Policy{Change: ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("off")}}}, ""},
		{"merge classes without a mode, without features", "change: { auto_merge: { classes: [docs], max_per_day: 1 } }\n",
			Policy{Change: ChangePolicy{AutoMerge: AutoMergePolicy{Classes: []string{"docs"}, MaxPerDay: ip(1)}}}, ""},
		// The pipeline-fix block predates reviews: a file that names the
		// agentic mode there stays VALID (it is reported unavailable instead).
		{"an existing fix config naming the agentic mode", "actions: { pipelines: true, pipeline_fixes: true }\npipeline_fix: { auto_merge: { mode: after-agentic-review } }\n",
			Policy{Actions: ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true)}, PipelineFix: PipelineFixPolicy{AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review")}}}, ""},
	}
	for _, c := range cases {
		if err := Validate([]byte(head + c.doc)); (err != nil) != (c.invalid != "") {
			t.Errorf("%s: schema: err = %v, want invalid=%v", c.name, err, c.invalid != "")
		}
		err := CheckChange(c.policy)
		if (err != nil) != (c.invalid != "") {
			t.Errorf("%s: CheckChange: err = %v, want invalid=%v", c.name, err, c.invalid != "")
		}
		if c.invalid != "" && err != nil && (!strings.Contains(err.Error(), c.invalid) || !strings.Contains(err.Error(), "auto_merge.mode")) {
			t.Errorf("%s: err = %v, want it to name auto_merge.mode and %s", c.name, err, c.invalid)
		}
		if _, err := Load([]byte(head + c.doc)); (err != nil) != (c.invalid != "") {
			t.Errorf("%s: Load: err = %v, want invalid=%v", c.name, err, c.invalid != "")
		} else if err != nil && !strings.HasPrefix(err.Error(), ".gonk.yml: ") {
			t.Errorf("%s: Load err = %v, want a .gonk.yml error", c.name, err)
		}
	}
}

// Validate on a decoded layer: the Go half of every schema rule.
func TestChangePolicyValidate(t *testing.T) {
	many := make([]string, scope.MaxPatterns+1)
	for i := range many {
		many[i] = "a/**"
	}
	// 17 labels: one more than the limit of 16, as LITERALS, so that a raised
	// constant is noticed.
	labels := make([]string, 17)
	for i := range labels {
		labels[i] = "l" + strings.Repeat("x", i)
	}
	cases := []struct {
		name  string
		p     ChangePolicy
		layer scope.Layer
		want  string // "" = valid
	}{
		{"zero", ChangePolicy{}, scope.LayerProject, ""},
		{"zero, operator", ChangePolicy{}, scope.LayerOperator, ""},
		{"everything at its ceiling", ChangePolicy{Triggers: []string{"label", "command", "mention"}, CreateClasses: []string{"source", "test", "docs"},
			RequireDocs: sp("never"), DocsLabels: labels[:16], RequireTestReport: bp(true), MaxAttempts: ip(3), MaxOpen: ip(3),
			VerifyTimeout: dp(6 * time.Hour), MaxFiles: ip(8), MaxChangedLines: ip(300), Paths: []string{"internal/**"},
			AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval"), Classes: []string{"docs"}, MaxPerDay: ip(3)}}, scope.LayerProject, ""},
		{"unknown layer", ChangePolicy{}, scope.Layer("batch"), "unknown layer"},
		{"trigger triage-verdict", ChangePolicy{Triggers: []string{"triage-verdict"}}, scope.LayerProject, "triggers"},
		{"trigger triage-verdict, operator", ChangePolicy{Triggers: []string{"label", "triage-verdict"}}, scope.LayerOperator, "triggers"},
		{"create class deps", ChangePolicy{CreateClasses: []string{"deps"}}, scope.LayerProject, "create_classes"},
		{"create class ci", ChangePolicy{CreateClasses: []string{"ci"}}, scope.LayerOperator, "create_classes"},
		{"merge class ci", ChangePolicy{AutoMerge: AutoMergePolicy{Classes: []string{"ci"}}}, scope.LayerProject, "auto_merge.classes"},
		{"merge class source", ChangePolicy{AutoMerge: AutoMergePolicy{Classes: []string{"source"}}}, scope.LayerProject, "auto_merge.classes"},
		{"require_docs sometimes", ChangePolicy{RequireDocs: sp("sometimes")}, scope.LayerProject, "require_docs"},
		{"docs label empty", ChangePolicy{DocsLabels: []string{""}}, scope.LayerProject, "docs_labels"},
		{"docs label trailing space", ChangePolicy{DocsLabels: []string{"feature "}}, scope.LayerProject, "docs_labels"},
		{"docs label two lines", ChangePolicy{DocsLabels: []string{"a\nb"}}, scope.LayerProject, "docs_labels"},
		{"docs label too long", ChangePolicy{DocsLabels: []string{strings.Repeat("x", 256)}}, scope.LayerProject, "docs_labels"},
		{"docs label twice", ChangePolicy{DocsLabels: []string{"feature", "bug", "feature"}}, scope.LayerProject, "listed twice"},
		{"too many docs labels", ChangePolicy{DocsLabels: labels}, scope.LayerProject, "docs_labels"},
		{"mode always", ChangePolicy{AutoMerge: AutoMergePolicy{Mode: sp("always")}}, scope.LayerProject, "auto_merge.mode"},
		{"max_attempts 4", ChangePolicy{MaxAttempts: ip(4)}, scope.LayerProject, "max_attempts"},
		{"max_attempts 0", ChangePolicy{MaxAttempts: ip(0)}, scope.LayerProject, "max_attempts"},
		{"max_open 4", ChangePolicy{MaxOpen: ip(4)}, scope.LayerOperator, "max_open"},
		{"max_open 0", ChangePolicy{MaxOpen: ip(0)}, scope.LayerProject, "max_open"},
		{"max_files 9", ChangePolicy{MaxFiles: ip(9)}, scope.LayerProject, "max_files"},
		{"max_files -1", ChangePolicy{MaxFiles: ip(-1)}, scope.LayerProject, "max_files"},
		{"max_changed_lines 301", ChangePolicy{MaxChangedLines: ip(301)}, scope.LayerProject, "max_changed_lines"},
		{"max_changed_lines 0", ChangePolicy{MaxChangedLines: ip(0)}, scope.LayerProject, "max_changed_lines"},
		{"max_per_day 4", ChangePolicy{AutoMerge: AutoMergePolicy{MaxPerDay: ip(4)}}, scope.LayerProject, "auto_merge.max_per_day"},
		{"max_per_day 0", ChangePolicy{AutoMerge: AutoMergePolicy{MaxPerDay: ip(0)}}, scope.LayerOperator, "auto_merge.max_per_day"},
		{"verify_timeout 0", ChangePolicy{VerifyTimeout: dp(0)}, scope.LayerProject, "verify_timeout"},
		{"verify_timeout negative", ChangePolicy{VerifyTimeout: dp(-time.Second)}, scope.LayerProject, "verify_timeout"},
		{"verify_timeout past the ceiling", ChangePolicy{VerifyTimeout: dp(6*time.Hour + time.Second)}, scope.LayerProject, "verify_timeout"},
		{"paths from an operator", ChangePolicy{Paths: []string{"internal/**"}}, scope.LayerOperator, "project-only"},
		{"paths with ..", ChangePolicy{Paths: []string{"a/../b"}}, scope.LayerProject, "paths"},
		{"too many paths", ChangePolicy{Paths: many}, scope.LayerProject, "paths"},
	}
	for _, c := range cases {
		err := c.p.Validate(c.layer)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: Validate = %v, want nil", c.name, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: Validate accepted %+v", c.name, c.p)
		case c.want != "" && (!strings.HasPrefix(err.Error(), "change: ") || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want a change error mentioning %q", c.name, err, c.want)
		}
	}
	// The same bound on the pipeline-fix block's new key.
	for _, v := range []int64{0, 4, -1} {
		err := PipelineFixPolicy{AutoMerge: AutoMergePolicy{MaxPerDay: ip(v)}}.Validate(scope.LayerProject)
		if err == nil || !strings.Contains(err.Error(), "auto_merge.max_per_day") {
			t.Errorf("pipeline_fix.auto_merge.max_per_day %d: err = %v", v, err)
		}
	}
}

// Load runs the Go check, not only the schema.
func TestLoadRunsTheChangeGoCheck(t *testing.T) {
	for name, body := range map[string]string{
		"verify_timeout past the ceiling": "change: { verify_timeout: 7h }\n",
		"verify_timeout zero":             "change: { verify_timeout: 0s }\n",
		"path with a .. segment":          "change: { paths: [\"a/../b\"] }\n",
	} {
		if err := Validate([]byte(changeHead + body)); err != nil {
			t.Fatalf("%s: precondition: the schema is meant to accept this, got %v", name, err)
		}
		_, err := Load([]byte(changeHead + body))
		if err == nil || !strings.HasPrefix(err.Error(), ".gonk.yml: change: ") {
			t.Errorf("%s: Load err = %v, want a .gonk.yml: change: error", name, err)
		}
	}
}

// The fold: the project's value (or the default) is the base, and an operator
// layer can only tighten it.
func TestResolveChange(t *testing.T) {
	def := defaultChange()
	with := func(mut func(*EffectiveChange)) EffectiveChange {
		e := defaultChange()
		mut(&e)
		return e
	}
	z := ChangePolicy{}
	merge := func(m AutoMergePolicy) ChangePolicy { return ChangePolicy{AutoMerge: m} }
	cases := []struct {
		name                     string
		instance, group, project ChangePolicy
		want                     EffectiveChange
	}{
		{"nothing set anywhere: the defaults", z, z, z, def},

		// ---- triggers: intersection, canonical order
		{"the project keeps one form", z, z, ChangePolicy{Triggers: []string{"command"}},
			with(func(e *EffectiveChange) { e.Triggers = []string{"command"} })},
		{"canonical order", z, z, ChangePolicy{Triggers: []string{"mention", "label"}},
			with(func(e *EffectiveChange) { e.Triggers = []string{"label", "mention"} })},
		{"the default is label only (owner, 2026-10-10)", z, z, z,
			with(func(e *EffectiveChange) { e.Triggers = []string{"label"} })},
		{"a project opts in to all three forms", z, z, ChangePolicy{Triggers: []string{"label", "command", "mention"}},
			with(func(e *EffectiveChange) { e.Triggers = []string{"label", "command", "mention"} })},
		{"an operator list cannot add command to the default", ChangePolicy{Triggers: []string{"label", "command"}}, z, z,
			with(func(e *EffectiveChange) { e.Triggers = []string{"label"} })},
		{"an operator removes mention from a project's three", ChangePolicy{Triggers: []string{"label", "command"}}, z, ChangePolicy{Triggers: []string{"label", "command", "mention"}},
			with(func(e *EffectiveChange) { e.Triggers = []string{"label", "command"} })},
		{"an operator list cannot add a form the project dropped", ChangePolicy{Triggers: []string{"label", "command", "mention"}}, z,
			ChangePolicy{Triggers: []string{"label"}}, with(func(e *EffectiveChange) { e.Triggers = []string{"label"} })},
		{"instance and group lists both apply", ChangePolicy{Triggers: []string{"label", "command"}}, ChangePolicy{Triggers: []string{"command", "mention"}}, ChangePolicy{Triggers: []string{"label", "command", "mention"}},
			with(func(e *EffectiveChange) { e.Triggers = []string{"command"} })},
		{"disjoint lists allow nothing", ChangePolicy{Triggers: []string{"mention"}}, z, ChangePolicy{Triggers: []string{"label"}},
			with(func(e *EffectiveChange) { e.Triggers = []string{} })},
		{"a project's empty list allows nothing", z, z, ChangePolicy{Triggers: []string{}},
			with(func(e *EffectiveChange) { e.Triggers = []string{} })},
		{"an unvalidated triage-verdict is dropped", z, z, ChangePolicy{Triggers: []string{"triage-verdict", "label"}},
			with(func(e *EffectiveChange) { e.Triggers = []string{"label"} })},
		{"an operator's unvalidated triage-verdict adds nothing", ChangePolicy{Triggers: []string{"triage-verdict"}}, z, z,
			with(func(e *EffectiveChange) { e.Triggers = []string{} })},

		// ---- create_classes
		{"source is a project opt-in", z, z, ChangePolicy{CreateClasses: []string{"docs", "source", "test"}},
			with(func(e *EffectiveChange) { e.CreateClasses = []string{"source", "test", "docs"} })},
		{"an operator cannot opt a project in to source", ChangePolicy{CreateClasses: []string{"source", "test", "docs"}}, ChangePolicy{CreateClasses: []string{"source", "test", "docs"}}, z, def},
		{"a group removes source", z, ChangePolicy{CreateClasses: []string{"test", "docs"}}, ChangePolicy{CreateClasses: []string{"source", "test", "docs"}}, def},
		{"an operator removes docs from the default", ChangePolicy{CreateClasses: []string{"test"}}, z, z,
			with(func(e *EffectiveChange) { e.CreateClasses = []string{"test"} })},
		{"an unknown create class is dropped", z, z, ChangePolicy{CreateClasses: []string{"deps", "test"}},
			with(func(e *EffectiveChange) { e.CreateClasses = []string{"test"} })},

		// ---- require_docs: strictest wins
		{"docs: the project loosens to never", z, z, ChangePolicy{RequireDocs: sp("never")},
			with(func(e *EffectiveChange) { e.RequireDocs = "never" })},
		{"docs: the project tightens to always", z, z, ChangePolicy{RequireDocs: sp("always")},
			with(func(e *EffectiveChange) { e.RequireDocs = "always" })},
		{"docs: an operator never does not loosen the default", ChangePolicy{RequireDocs: sp("never")}, ChangePolicy{RequireDocs: sp("never")}, z, def},
		{"docs: an operator when-labelled tightens a project's never", ChangePolicy{RequireDocs: sp("when-labelled")}, z, ChangePolicy{RequireDocs: sp("never")}, def},
		{"docs: a group always beats everything", ChangePolicy{RequireDocs: sp("never")}, ChangePolicy{RequireDocs: sp("always")}, ChangePolicy{RequireDocs: sp("never")},
			with(func(e *EffectiveChange) { e.RequireDocs = "always" })},
		{"docs: an unknown value reads as always", z, z, ChangePolicy{RequireDocs: sp("sometimes")},
			with(func(e *EffectiveChange) { e.RequireDocs = "always" })},
		{"docs: an operator's unknown value reads as always", z, ChangePolicy{RequireDocs: sp("sometimes")}, ChangePolicy{RequireDocs: sp("never")},
			with(func(e *EffectiveChange) { e.RequireDocs = "always" })},

		// ---- docs_labels: the project replaces the default, an operator adds
		{"labels: the project's list replaces the default", z, z, ChangePolicy{DocsLabels: []string{"needs docs"}},
			with(func(e *EffectiveChange) { e.DocsLabels = []string{"needs docs"} })},
		{"labels: a project's empty list", z, z, ChangePolicy{DocsLabels: []string{}},
			with(func(e *EffectiveChange) { e.DocsLabels = []string{} })},
		{"labels: an operator adds to the default", ChangePolicy{DocsLabels: []string{"api"}}, ChangePolicy{DocsLabels: []string{"feature", "ux"}}, z,
			with(func(e *EffectiveChange) {
				e.DocsLabels = []string{"feature", "enhancement", "type::feature", "api", "ux"}
			})},
		{"labels: an operator adds to the project's empty list", ChangePolicy{DocsLabels: []string{"api"}}, z, ChangePolicy{DocsLabels: []string{}},
			with(func(e *EffectiveChange) { e.DocsLabels = []string{"api"} })},
		{"labels: an unvalidated bad label is dropped", z, z, ChangePolicy{DocsLabels: []string{" x", "ok", "", "ok"}},
			with(func(e *EffectiveChange) { e.DocsLabels = []string{"ok"} })},

		// ---- require_test_report: true anywhere applies
		{"report: the project asks", z, z, ChangePolicy{RequireTestReport: bp(true)},
			with(func(e *EffectiveChange) { e.RequireTestReport = true })},
		{"report: the instance forces it", ChangePolicy{RequireTestReport: bp(true)}, z, ChangePolicy{RequireTestReport: bp(false)},
			with(func(e *EffectiveChange) { e.RequireTestReport = true })},
		{"report: the group forces it", z, ChangePolicy{RequireTestReport: bp(true)}, z,
			with(func(e *EffectiveChange) { e.RequireTestReport = true })},
		{"report: a group false does not undo an instance true", ChangePolicy{RequireTestReport: bp(true)}, ChangePolicy{RequireTestReport: bp(false)}, z,
			with(func(e *EffectiveChange) { e.RequireTestReport = true })},
		{"report: an operator false does not undo the project's true", ChangePolicy{RequireTestReport: bp(false)}, ChangePolicy{RequireTestReport: bp(false)}, ChangePolicy{RequireTestReport: bp(true)},
			with(func(e *EffectiveChange) { e.RequireTestReport = true })},

		// ---- numbers: minimum, never above the ceiling
		{"numbers: the project tightens", z, z,
			ChangePolicy{MaxAttempts: ip(1), MaxFiles: ip(2), MaxChangedLines: ip(50), VerifyTimeout: dp(30 * time.Minute), AutoMerge: AutoMergePolicy{MaxPerDay: ip(1)}},
			with(func(e *EffectiveChange) {
				e.MaxAttempts, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout, e.AutoMerge.MaxPerDay = 1, 2, 50, 30*time.Minute, 1
			})},
		{"numbers: the project raises to the ceilings", z, z,
			ChangePolicy{MaxAttempts: ip(3), MaxOpen: ip(3), MaxFiles: ip(8), MaxChangedLines: ip(300), VerifyTimeout: dp(6 * time.Hour)},
			with(func(e *EffectiveChange) {
				e.MaxAttempts, e.MaxOpen, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout = 3, 3, 8, 300, 6*time.Hour
			})},
		{"numbers: an operator value above the default does not raise it",
			ChangePolicy{MaxAttempts: ip(3), MaxOpen: ip(3), MaxFiles: ip(8), MaxChangedLines: ip(300), VerifyTimeout: dp(6 * time.Hour)}, z, z, def},
		{"numbers: the instance is the lowest for every key",
			ChangePolicy{MaxAttempts: ip(1), MaxOpen: ip(1), MaxFiles: ip(2), MaxChangedLines: ip(7), VerifyTimeout: dp(10 * time.Minute), AutoMerge: AutoMergePolicy{MaxPerDay: ip(1)}},
			ChangePolicy{MaxAttempts: ip(2), MaxOpen: ip(2), MaxFiles: ip(5), MaxChangedLines: ip(250), VerifyTimeout: dp(4 * time.Hour), AutoMerge: AutoMergePolicy{MaxPerDay: ip(2)}},
			ChangePolicy{MaxAttempts: ip(3), MaxOpen: ip(3), MaxFiles: ip(8), MaxChangedLines: ip(300), VerifyTimeout: dp(6 * time.Hour), AutoMerge: AutoMergePolicy{MaxPerDay: ip(3)}},
			with(func(e *EffectiveChange) {
				e.MaxAttempts, e.MaxOpen, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout, e.AutoMerge.MaxPerDay = 1, 1, 2, 7, 10*time.Minute, 1
			})},
		{"numbers: the group is the lowest for every key",
			ChangePolicy{MaxAttempts: ip(2), MaxOpen: ip(2), MaxFiles: ip(5), MaxChangedLines: ip(250), VerifyTimeout: dp(4 * time.Hour), AutoMerge: AutoMergePolicy{MaxPerDay: ip(2)}},
			ChangePolicy{MaxAttempts: ip(1), MaxOpen: ip(1), MaxFiles: ip(2), MaxChangedLines: ip(7), VerifyTimeout: dp(10 * time.Minute), AutoMerge: AutoMergePolicy{MaxPerDay: ip(1)}},
			ChangePolicy{MaxAttempts: ip(3), MaxOpen: ip(3), MaxFiles: ip(8), MaxChangedLines: ip(300), VerifyTimeout: dp(6 * time.Hour), AutoMerge: AutoMergePolicy{MaxPerDay: ip(3)}},
			with(func(e *EffectiveChange) {
				e.MaxAttempts, e.MaxOpen, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout, e.AutoMerge.MaxPerDay = 1, 1, 2, 7, 10*time.Minute, 1
			})},
		{"numbers: an unvalidated value past the ceiling is clamped", z, z,
			ChangePolicy{MaxAttempts: ip(99), MaxOpen: ip(99), MaxFiles: ip(99), MaxChangedLines: ip(9999), VerifyTimeout: dp(99 * time.Hour), AutoMerge: AutoMergePolicy{MaxPerDay: ip(99)}},
			with(func(e *EffectiveChange) {
				e.MaxAttempts, e.MaxOpen, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout = 3, 3, 8, 300, 6*time.Hour
			})},
		{"numbers: an unvalidated negative is clamped to zero", z, z,
			ChangePolicy{MaxAttempts: ip(-1), MaxOpen: ip(-1), MaxFiles: ip(-1), MaxChangedLines: ip(-1), VerifyTimeout: dp(-time.Hour), AutoMerge: AutoMergePolicy{MaxPerDay: ip(-1)}},
			with(func(e *EffectiveChange) {
				e.MaxAttempts, e.MaxOpen, e.MaxFiles, e.MaxChangedLines, e.VerifyTimeout, e.AutoMerge.MaxPerDay = 0, 0, 0, 0, 0, 0
			})},

		// ---- paths: the project's
		{"paths: the project's list", z, z, ChangePolicy{Paths: []string{"internal/**", "docs/**"}},
			with(func(e *EffectiveChange) { e.Paths = []string{"internal/**", "docs/**"} })},
		{"paths: an operator's unvalidated list is not carried", ChangePolicy{Paths: []string{"x/**"}}, ChangePolicy{Paths: []string{"y/**"}}, z, def},

		// ---- auto-merge: off unless the project asks and no operator disagrees
		{"merge: the project asks", z, z, merge(AutoMergePolicy{Mode: sp("after-human-approval")}),
			with(func(e *EffectiveChange) { e.AutoMerge.Mode = "after-human-approval" })},
		{"merge: an operator mode does not switch it on", merge(AutoMergePolicy{Mode: sp("after-human-approval")}), merge(AutoMergePolicy{Mode: sp("after-human-approval")}), z, def},
		{"merge: an instance off vetoes", merge(AutoMergePolicy{Mode: sp("off")}), z, merge(AutoMergePolicy{Mode: sp("after-human-approval")}), def},
		{"merge: a group off vetoes", z, merge(AutoMergePolicy{Mode: sp("off")}), merge(AutoMergePolicy{Mode: sp("after-agentic-review")}), def},
		{"merge: an operator naming the same mode allows it", merge(AutoMergePolicy{Mode: sp("after-agentic-review")}), z, merge(AutoMergePolicy{Mode: sp("after-agentic-review")}),
			with(func(e *EffectiveChange) { e.AutoMerge.Mode = "after-agentic-review" })},
		{"merge: an operator naming a different mode turns it off", merge(AutoMergePolicy{Mode: sp("after-agentic-review")}), z, merge(AutoMergePolicy{Mode: sp("after-human-approval")}), def},
		{"merge: instance and group disagreeing turn it off", merge(AutoMergePolicy{Mode: sp("after-agentic-review")}), merge(AutoMergePolicy{Mode: sp("after-human-approval")}),
			merge(AutoMergePolicy{Mode: sp("after-human-approval")}), def},
		{"merge: an unknown mode reads as off", z, z, merge(AutoMergePolicy{Mode: sp("always")}), def},
		{"merge classes: the project allows none", z, z, merge(AutoMergePolicy{Classes: []string{}}),
			with(func(e *EffectiveChange) { e.AutoMerge.Classes = []string{} })},
		{"merge classes: an operator allows none", z, merge(AutoMergePolicy{Classes: []string{}}), z,
			with(func(e *EffectiveChange) { e.AutoMerge.Classes = []string{} })},
		{"merge classes: ci and source can never be reached", merge(AutoMergePolicy{Classes: []string{"docs", "ci", "source"}}), z,
			merge(AutoMergePolicy{Classes: []string{"ci", "source", "test", "docs"}}), def},
	}
	for _, c := range cases {
		got := ResolveChange(c.instance, c.group, c.project)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, got, c.want)
		}
		// Resolve carries exactly this fold.
		eff := Resolve(Policy{Change: c.instance}, Policy{Change: c.group}, ProjectConfig{Policy: Policy{Change: c.project}})
		if !reflect.DeepEqual(eff.Change, c.want) {
			t.Errorf("%s: Resolve().Change = %+v, want %+v", c.name, eff.Change, c.want)
		}
	}
}

// The property behind "every key can only TIGHTEN": whatever the layers say,
// the result is inside the ceilings, and adding an operator layer never makes
// it wider than the same project without that layer.
func TestNoChangeFoldIsWiderThanTheCeiling(t *testing.T) {
	trigs := [][]string{nil, {}, {"label"}, {"command", "mention"}, {"label", "command", "mention"}, {"triage-verdict", "label"}}
	creates := [][]string{nil, {}, {"test"}, {"docs", "source"}, {"source", "test", "docs"}, {"deps", "test"}}
	mclasses := [][]string{nil, {}, {"docs"}, {"ci"}, {"docs", "source"}}
	labels := [][]string{nil, {}, {"feature"}, {"api", "ux"}, {" bad", "ok"}}
	bools := []*bool{nil, bp(true), bp(false)}
	docs := []*string{nil, sp("never"), sp("when-labelled"), sp("always"), sp("bogus")}
	modes := []*string{nil, sp("off"), sp("after-human-approval"), sp("after-agentic-review"), sp("bogus")}
	nums := []*int64{nil, ip(-5), ip(0), ip(1), ip(2), ip(3), ip(7), ip(1000)}
	durs := []*Duration{nil, dp(-time.Hour), dp(0), dp(time.Minute), dp(2 * time.Hour), dp(6 * time.Hour), dp(100 * time.Hour)}
	gen := func(i int) ChangePolicy {
		return ChangePolicy{
			Triggers: trigs[i%len(trigs)], CreateClasses: creates[(i/2)%len(creates)], RequireTestReport: bools[(i/3)%len(bools)],
			RequireDocs: docs[(i/5)%len(docs)], MaxAttempts: nums[(i/7)%len(nums)], MaxOpen: nums[(i/11)%len(nums)],
			VerifyTimeout: durs[(i/13)%len(durs)], MaxFiles: nums[(i/17)%len(nums)], MaxChangedLines: nums[(i/19)%len(nums)],
			DocsLabels: labels[(i/31)%len(labels)],
			AutoMerge:  AutoMergePolicy{Mode: modes[(i/23)%len(modes)], Classes: mclasses[(i/29)%len(mclasses)], MaxPerDay: nums[(i/37)%len(nums)]},
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
	for i := 0; i < 6000; i += 7 {
		for j := 1; j < 6000; j += 311 {
			inst, grp, proj := gen(i), gen(j), gen(i*31+j)
			e := ResolveChange(inst, grp, proj)
			alone := ResolveChange(ChangePolicy{}, ChangePolicy{}, proj)
			n++
			id := func() string {
				return "instance " + showAny(inst) + " group " + showAny(grp) + " project " + showAny(proj)
			}
			if !subset(e.Triggers, changeTriggerOrder) || !subset(e.CreateClasses, createClassOrder) || !subset(e.AutoMerge.Classes, []string{"docs"}) {
				t.Fatalf("a list left its set: %+v\n%s", e, id())
			}
			if e.MaxAttempts < 0 || e.MaxAttempts > 3 || e.MaxOpen < 0 || e.MaxOpen > 3 || e.MaxFiles < 0 || e.MaxFiles > 8 ||
				e.MaxChangedLines < 0 || e.MaxChangedLines > 300 || e.VerifyTimeout < 0 || e.VerifyTimeout > 6*time.Hour ||
				e.AutoMerge.MaxPerDay < 0 || e.AutoMerge.MaxPerDay > 3 {
				t.Fatalf("a number left its ceiling: %+v\n%s", e, id())
			}
			if !subset(e.Triggers, alone.Triggers) || !subset(e.CreateClasses, alone.CreateClasses) || !subset(e.AutoMerge.Classes, alone.AutoMerge.Classes) ||
				e.MaxAttempts > alone.MaxAttempts || e.MaxOpen > alone.MaxOpen || e.MaxFiles > alone.MaxFiles ||
				e.MaxChangedLines > alone.MaxChangedLines || e.VerifyTimeout > alone.VerifyTimeout || e.AutoMerge.MaxPerDay > alone.AutoMerge.MaxPerDay ||
				requireDocsRank[e.RequireDocs] < requireDocsRank[alone.RequireDocs] || (alone.RequireTestReport && !e.RequireTestReport) ||
				!subset(alone.DocsLabels, e.DocsLabels) ||
				(e.AutoMerge.Mode != "off" && e.AutoMerge.Mode != alone.AutoMerge.Mode) {
				t.Fatalf("an operator layer WIDENED the project's own result:\n with    %+v\n without %+v\n%s", e, alone, id())
			}
			if _, ok := requireDocsRank[e.RequireDocs]; !ok || !autoMergeModes[e.AutoMerge.Mode] {
				t.Fatalf("an enum left its set: %+v", e)
			}
			if e.AllowsTrigger("triage-verdict") || e.AllowsCreateClass("deps") {
				t.Fatalf("a value outside the menu survived: %+v", e)
			}
			for _, l := range e.DocsLabels {
				if !validDocsLabel(l) {
					t.Fatalf("a malformed docs label survived: %q", l)
				}
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d combinations were tried", n)
	}
}

// FoldChange is what a nested group hierarchy goes through (opercfg.GroupFor):
// at least as tight as either layer, in either order.
func TestFoldChangeOnlyTightens(t *testing.T) {
	a := ChangePolicy{Triggers: []string{"label", "command"}, CreateClasses: []string{"source", "test"}, RequireDocs: sp("never"),
		DocsLabels: []string{"api"}, RequireTestReport: bp(false), MaxAttempts: ip(3), MaxOpen: ip(1), MaxFiles: ip(2), MaxChangedLines: ip(90),
		VerifyTimeout: dp(time.Hour), AutoMerge: AutoMergePolicy{Mode: sp("after-human-approval"), Classes: []string{"docs"}, MaxPerDay: ip(3)}}
	b := ChangePolicy{Triggers: []string{"command", "mention"}, CreateClasses: []string{"test", "docs"}, RequireDocs: sp("always"),
		DocsLabels: []string{"ux"}, RequireTestReport: bp(true), MaxAttempts: ip(1), MaxOpen: ip(2), MaxFiles: ip(8), MaxChangedLines: ip(30),
		VerifyTimeout: dp(3 * time.Hour), AutoMerge: AutoMergePolicy{Mode: sp("after-agentic-review"), Classes: []string{}, MaxPerDay: ip(1)}}
	for name, got := range map[string]ChangePolicy{"a then b": FoldChange(a, b), "b then a": FoldChange(b, a)} {
		wantLabels := []string{"api", "ux"}
		if name == "b then a" {
			wantLabels = []string{"ux", "api"}
		}
		want := ChangePolicy{Triggers: []string{"command"}, CreateClasses: []string{"test"}, RequireDocs: sp("always"), DocsLabels: wantLabels,
			RequireTestReport: bp(true), MaxAttempts: ip(1), MaxOpen: ip(1), MaxFiles: ip(2), MaxChangedLines: ip(30), VerifyTimeout: dp(time.Hour),
			AutoMerge: AutoMergePolicy{Mode: sp("off"), Classes: []string{}, MaxPerDay: ip(1)}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %s\n want %s", name, showAny(got), showAny(want))
		}
	}
	if got := FoldChange(ChangePolicy{}, ChangePolicy{}); !got.IsZero() {
		t.Errorf("two silent layers folded to %s", showAny(got))
	}
	if got := FoldChange(a, ChangePolicy{}); !reflect.DeepEqual(got, a) {
		t.Errorf("folding a silent layer changed the policy:\n got  %s\n want %s", showAny(got), showAny(a))
	}
	if got := FoldChange(ChangePolicy{Triggers: []string{"label"}}, ChangePolicy{Triggers: []string{"mention"}}); got.Triggers == nil || len(got.Triggers) != 0 {
		t.Errorf("disjoint trigger lists folded to %#v, want a non-nil empty list", got.Triggers)
	}
	// paths is project-only and is never carried between operator layers.
	if got := FoldChange(ChangePolicy{Paths: []string{"a/**"}}, ChangePolicy{Paths: []string{"b/**"}}); got.Paths != nil {
		t.Errorf("FoldChange carried paths: %v", got.Paths)
	}
	// ResolvePipelineFix: the project's max_per_day (or the default 3) is the
	// base, an operator layer lowers it and cannot raise it.
	for _, c := range []struct {
		name            string
		inst, grp, proj *int64
		want            int
	}{
		{"default", nil, nil, nil, 3},
		{"the project lowers it", nil, nil, ip(1), 1},
		{"the instance lowers it", ip(2), nil, nil, 2},
		{"the group lowers the project's", nil, ip(1), ip(3), 1},
		{"an operator value above the project's raises nothing", ip(3), ip(3), ip(1), 1},
		{"an unvalidated value is clamped", nil, nil, ip(99), 3},
	} {
		got := ResolvePipelineFix(PipelineFixPolicy{AutoMerge: AutoMergePolicy{MaxPerDay: c.inst}}, PipelineFixPolicy{AutoMerge: AutoMergePolicy{MaxPerDay: c.grp}},
			PipelineFixPolicy{AutoMerge: AutoMergePolicy{MaxPerDay: c.proj}})
		if got.AutoMerge.MaxPerDay != c.want {
			t.Errorf("pipeline_fix max_per_day, %s: %d, want %d", c.name, got.AutoMerge.MaxPerDay, c.want)
		}
	}
	// The pipeline-fix fold carries the new key the same way.
	f := FoldPipelineFix(PipelineFixPolicy{AutoMerge: AutoMergePolicy{MaxPerDay: ip(3)}}, PipelineFixPolicy{AutoMerge: AutoMergePolicy{MaxPerDay: ip(2)}})
	g := FoldPipelineFix(PipelineFixPolicy{AutoMerge: AutoMergePolicy{MaxPerDay: ip(1)}}, PipelineFixPolicy{})
	if *f.AutoMerge.MaxPerDay != 2 || *g.AutoMerge.MaxPerDay != 1 {
		t.Errorf("FoldPipelineFix max_per_day = %d, %d; want 2, 1", *f.AutoMerge.MaxPerDay, *g.AutoMerge.MaxPerDay)
	}
}

func TestChangeHelpers(t *testing.T) {
	e := defaultChange()
	for form, want := range map[string]bool{"label": true, "command": false, "mention": false, "triage-verdict": false, "": false} {
		if got := e.AllowsTrigger(form); got != want {
			t.Errorf("default: AllowsTrigger(%q) = %v, want %v", form, got, want)
		}
	}
	all := defaultChange()
	all.Triggers = []string{"label", "command", "mention"}
	for form, want := range map[string]bool{"label": true, "command": true, "mention": true, "triage-verdict": false, "": false} {
		if got := all.AllowsTrigger(form); got != want {
			t.Errorf("all three: AllowsTrigger(%q) = %v, want %v", form, got, want)
		}
	}
	for class, want := range map[string]bool{"test": true, "docs": true, "source": false, "deps": false, "ci": false} {
		if got := e.AllowsCreateClass(class); got != want {
			t.Errorf("AllowsCreateClass(%q) = %v, want %v", class, got, want)
		}
	}
	// It reads create_classes and nothing else: a project that allows only
	// new source files may not create a test, and a trigger name is no class.
	only := defaultChange()
	only.CreateClasses = []string{"source"}
	for class, want := range map[string]bool{"source": true, "test": false, "docs": false, "label": false} {
		if got := only.AllowsCreateClass(class); got != want {
			t.Errorf("create_classes [source]: AllowsCreateClass(%q) = %v, want %v", class, got, want)
		}
	}
	for _, c := range []struct {
		mode   string
		labels []string
		want   bool
	}{
		{"never", []string{"feature"}, false},
		{"always", nil, true},
		{"when-labelled", nil, false},
		{"when-labelled", []string{"bug", "priority::1"}, false},
		{"when-labelled", []string{"bug", "type::feature"}, true},
		{"when-labelled", []string{"Feature"}, false}, // exact match: GitLab label names are case-preserving
		{"bogus", nil, true},
		{"", nil, true},
	} {
		e.RequireDocs = c.mode
		if got := e.DocsRequired(c.labels); got != c.want {
			t.Errorf("DocsRequired(%s, %v) = %v, want %v", c.mode, c.labels, got, c.want)
		}
	}
	// The owner's level, as a literal: GitLab's Maintainer.
	if ChangeMinAccessLevel != 40 {
		t.Errorf("ChangeMinAccessLevel = %d, want 40 (Maintainer); Developer is 30 and must not be enough", ChangeMinAccessLevel)
	}
}

// max_files / max_changed_lines reach pkg/scope as lowered caps.
func TestChangeTightenScope(t *testing.T) {
	row := scope.Row{Classes: []scope.Class{scope.ClassSource, scope.ClassTest, scope.ClassDocs},
		Caps: scope.Caps{MaxFiles: 8, MaxLines: 300, MaxFileBytes: 65536, MaxPatchBytes: 65536}}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct {
		name, yaml           string
		wantFiles, wantLines int64
	}{
		{"defaults", "", 6, 200},
		{"tightened", "change: { max_files: 2, max_changed_lines: 12 }\n", 2, 12},
		{"raised to the ceiling", "change: { max_files: 8, max_changed_lines: 300 }\n", 8, 300},
		{"a lower scope.caps stands", "change: { max_files: 4, max_changed_lines: 100 }\nscope: { caps: { max_files: 1, max_lines: 5 } }\n", 1, 5},
		{"a higher scope.caps does not undo change", "change: { max_files: 4, max_changed_lines: 100 }\nscope: { caps: { max_files: 8, max_lines: 300 } }\n", 4, 100},
	} {
		pc, err := Load([]byte(changeHead + c.yaml))
		if err != nil {
			t.Errorf("%s: Load: %v", c.name, err)
			continue
		}
		eff := Resolve(Policy{}, Policy{}, withLadder(*pc))
		folded, err := scope.Fold(scope.Input{Row: row, Project: eff.Change.TightenScope(pc.Scope), BaseSHA: sha, Enabled: eff.Actions.Features})
		if err != nil {
			t.Errorf("%s: Fold: %v", c.name, err)
			continue
		}
		if folded.Caps.MaxFiles != c.wantFiles || folded.Caps.MaxLines != c.wantLines {
			t.Errorf("%s: caps = files %d lines %d, want %d / %d", c.name, folded.Caps.MaxFiles, folded.Caps.MaxLines, c.wantFiles, c.wantLines)
		}
	}
	orig := scope.Policy{Caps: scope.CapsPolicy{MaxFiles: ip(9)}}
	_ = defaultChange().TightenScope(orig)
	if *orig.Caps.MaxFiles != 9 {
		t.Error("TightenScope wrote through to the caller's policy")
	}
}

// The coverage test that reads the PLAN: section 2.7's own YAML example.
// Every key the plan lists under `change:` must be a key of the schema with
// the plan's value as its default -- except `trigger`, which owner decision Q2
// (section 8.1, overriding 2.7) replaced with the list `triggers` and which
// must now be REFUSED.
func TestPlan27ExampleIsTheDefaults(t *testing.T) {
	// The example is the product's rule and lives in the reference, which is
	// published with the code (it used to be section 2.7 of a build plan,
	// which is not).
	raw, err := os.ReadFile("../../docs/reference/gonk-change-rules.md")
	if err != nil {
		t.Fatal(err)
	}
	plan := string(raw)
	start := strings.Index(plan, "\n## 4. The `change:` example\n")
	if start < 0 {
		t.Fatal("the rules page has no section 4 (the `change:` example)")
	}
	block := plan[start+1:]
	if next := strings.Index(block, "\n## "); next >= 0 {
		block = block[:next]
	} else {
		t.Fatal("no heading follows the `change:` example")
	}
	open := strings.Index(block, "```yaml\n")
	if open < 0 {
		t.Fatal("section 4 has no yaml block")
	}
	end := strings.Index(block[open+8:], "```")
	if end < 0 {
		t.Fatal("section 4's yaml block is not closed")
	}
	example := block[open+8 : open+8+end]
	var doc struct {
		Change map[string]any `yaml:"change"`
	}
	if err := yaml.Unmarshal([]byte(example), &doc); err != nil {
		t.Fatalf("the rules page's example is not YAML: %v", err)
	}
	if len(doc.Change) != 12 {
		t.Fatalf("the rules page's example lists %d change keys, this test was written against 12: %v", len(doc.Change), doc.Change)
	}
	// As written, with `trigger:`, the example is refused...
	if _, err := Load([]byte("version: 1\nenabled: true\n" + example)); err == nil || !strings.Contains(err.Error(), "trigger") {
		t.Errorf("the rules page's example with the superseded `trigger:` key: err = %v, want a refusal naming it", err)
	}
	if doc.Change["trigger"] != "label" {
		t.Fatalf("the rules page's example no longer has `trigger: label`: %v", doc.Change["trigger"])
	}
	// ...and with that one line removed, every remaining key loads and
	// resolves to exactly the defaults.
	accepted := strings.Replace(example, "  trigger: label          # label | triage-verdict\n", "", 1)
	if accepted == example {
		t.Fatal("could not remove the `trigger:` line from the rules page's example")
	}
	pc, err := Load([]byte("version: 1\nenabled: true\n" + accepted))
	if err != nil {
		t.Fatalf("the rules page's example without `trigger:` is refused: %v", err)
	}
	if pc.Change.RequireDocs == nil || pc.Change.MaxOpen == nil || pc.Change.AutoMerge.Mode == nil || pc.Change.DocsLabels == nil {
		t.Fatalf("the rules page's example did not decode into the section: %+v", pc.Change)
	}
	eff := Resolve(Policy{}, Policy{}, withLadder(*pc))
	if !eff.Actions.Features || !reflect.DeepEqual(eff.Change, defaultChange()) {
		t.Errorf("the rules page's example resolves to\n %+v\nwant the defaults\n %+v", eff.Change, defaultChange())
	}
	// The ceilings the example's comments state.
	for _, want := range []string{"max_attempts: 2         # ceiling 3", "max_open: 1             # ceiling 3", "verify_timeout: 2h      # ceiling 6h",
		"create_classes: [test, docs]     # ceiling [source, test, docs]", "classes: [docs]       # ceiling [docs] for this workflow"} {
		if !strings.Contains(example, want) {
			t.Errorf("the rules page's example no longer says %q; re-check the ceilings in pkg/gonkcfg/change.go", want)
		}
	}
	if ChangeMaxAttemptsCeiling != 3 || ChangeMaxOpenCeiling != 3 || ChangeVerifyTimeoutCeiling != 6*time.Hour {
		t.Error("a ceiling constant differs from the rules page's example")
	}
}
