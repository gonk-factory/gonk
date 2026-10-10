package opercfg

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/gonkcfg"
)

// The operator layer's `actions.reviews`, `change:` and `review:` (gonk-cuwn
// plan 2.7, 3.1, owner decisions 8.1), on `instance` and on each group.
// Documented in docs/reference/gonk-change.md, gonk-review.md and the schema's
// own descriptions.

func TestLoadDecodesOperatorChangeAndReview(t *testing.T) {
	oc, err := Load([]byte(withFix(
		"  actions: { reviews: false }\n"+
			"  change:\n    triggers: [label]\n    create_classes: [test]\n    require_docs: always\n    docs_labels: [api]\n    require_test_report: true\n"+
			"    max_attempts: 1\n    max_open: 1\n    verify_timeout: 45m\n    max_files: 2\n    max_changed_lines: 20\n    auto_merge: { mode: \"off\", classes: [docs], max_per_day: 1 }\n"+
			"  review: { rung: glm, targets: [gonk], max_notes: 5, max_reviews_per_mr: 1 }\n"+
			"  pipeline_fix: { auto_merge: { max_per_day: 2 } }\n",
		"    change: { max_open: 2, auto_merge: { mode: after-human-approval } }\n    review: { rung: glm, max_notes: 3 }\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	in := oc.Instance
	if in.Actions.Reviews == nil || *in.Actions.Reviews {
		t.Errorf("instance actions.reviews = %v, want false", in.Actions.Reviews)
	}
	one, two, five, twenty := int64(1), int64(2), int64(5), int64(20)
	always, off, glm := "always", "off", "glm"
	yes := true
	d := gonkcfg.Duration(45 * time.Minute)
	wantChange := gonkcfg.ChangePolicy{Triggers: []string{"label"}, CreateClasses: []string{"test"}, RequireDocs: &always, DocsLabels: []string{"api"},
		RequireTestReport: &yes, MaxAttempts: &one, MaxOpen: &one, VerifyTimeout: &d, MaxFiles: &two, MaxChangedLines: &twenty,
		AutoMerge: gonkcfg.AutoMergePolicy{Mode: &off, Classes: []string{"docs"}, MaxPerDay: &one}}
	if !reflect.DeepEqual(in.Change, wantChange) {
		t.Errorf("instance change = %+v\nwant %+v", in.Change, wantChange)
	}
	wantReview := gonkcfg.ReviewPolicy{Targets: []string{"gonk"}, Rung: &glm, MaxNotes: &five, MaxReviewsPerMR: &one}
	if !reflect.DeepEqual(in.Review, wantReview) {
		t.Errorf("instance review = %+v\nwant %+v", in.Review, wantReview)
	}
	if in.PipelineFix.AutoMerge.MaxPerDay == nil || *in.PipelineFix.AutoMerge.MaxPerDay != 2 {
		t.Errorf("instance pipeline_fix.auto_merge.max_per_day = %v, want 2", in.PipelineFix.AutoMerge.MaxPerDay)
	}
	g := oc.Groups["agentic"]
	if g.Change.MaxOpen == nil || *g.Change.MaxOpen != 2 || g.Change.AutoMerge.Mode == nil || *g.Change.AutoMerge.Mode != "after-human-approval" ||
		g.Review.Rung == nil || *g.Review.Rung != "glm" || g.Review.MaxNotes == nil || *g.Review.MaxNotes != 3 {
		t.Errorf("group change/review = %+v / %+v", g.Change, g.Review)
	}
}

func TestLoadWithoutChangeOrReviewIsTheZeroPolicy(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	g := oc.GroupFor("agentic/x/repo")
	if !oc.Instance.Change.IsZero() || !oc.Instance.Review.IsZero() || oc.Instance.Actions.Reviews != nil ||
		!g.Change.IsZero() || !g.Review.IsZero() || g.Actions.Reviews != nil {
		t.Error("an operator config with no change/review keys has a non-zero change/review policy")
	}
}

// The meter refuses to START on an operator change/review section it cannot
// make sense of, at every operator layer. PLAN, must be seen red: "unknown key
// accepted", "a ceiling exceeded is accepted", "triage-verdict accepted".
func TestLoadRejectsInvalidOperatorChange(t *testing.T) {
	cases := map[string]struct{ instance, group, want string }{
		"instance unknown change key":      {"  change: { banana: 1 }\n", "", "change"},
		"group unknown change key":         {"", "    change: { banana: 1 }\n", "change"},
		"instance unknown merge key":       {"  change: { auto_merge: { banana: 1 } }\n", "", "auto_merge"},
		"instance paths":                   {"  change: { paths: [\"internal/**\"] }\n", "", "change"},
		"group paths":                      {"", "    change: { paths: [\"internal/**\"] }\n", "change"},
		"instance trigger triage-verdict":  {"  change: { triggers: [triage-verdict] }\n", "", "triggers"},
		"group trigger triage-verdict":     {"", "    change: { triggers: [label, triage-verdict] }\n", "triggers"},
		"instance singular trigger":        {"  change: { trigger: label }\n", "", "change"},
		"instance create class deps":       {"  change: { create_classes: [deps] }\n", "", "create_classes"},
		"group require_docs sometimes":     {"", "    change: { require_docs: sometimes }\n", "require_docs"},
		"instance attempts over":           {"  change: { max_attempts: 4 }\n", "", "max_attempts"},
		"group open zero":                  {"", "    change: { max_open: 0 }\n", "max_open"},
		"instance files over":              {"  change: { max_files: 9 }\n", "", "max_files"},
		"group lines over":                 {"", "    change: { max_changed_lines: 301 }\n", "max_changed_lines"},
		"instance mode always":             {"  change: { auto_merge: { mode: always } }\n", "", "mode"},
		"instance merge class ci":          {"  change: { auto_merge: { classes: [ci] } }\n", "", "classes"},
		"group merges per day over":        {"", "    change: { auto_merge: { max_per_day: 4 } }\n", "max_per_day"},
		"instance fix merges per day over": {"  pipeline_fix: { auto_merge: { max_per_day: 4 } }\n", "", "max_per_day"},
		"group fix merges per day zero":    {"", "    pipeline_fix: { auto_merge: { max_per_day: 0 } }\n", "max_per_day"},
		"instance timeout over (Go)":       {"  change: { verify_timeout: 7h }\n", "", "instance: change: verify_timeout"},
		"group timeout zero (Go)":          {"", "    change: { verify_timeout: 0s }\n", "group agentic: change: verify_timeout"},
		"instance docs label blank":        {"  change: { docs_labels: [\" \"] }\n", "", "docs_labels"},
		"instance unknown review key":      {"  review: { banana: 1 }\n", "", "review"},
		"group unknown review key":         {"", "    review: { banana: 1 }\n", "review"},
		"instance target human":            {"  review: { targets: [human] }\n", "", "targets"},
		"group notes over":                 {"", "    review: { max_notes: 21 }\n", "max_notes"},
		"instance reviews per mr over":     {"  review: { max_reviews_per_mr: 4 }\n", "", "max_reviews_per_mr"},
		"instance rung not a name":         {"  review: { rung: \"Big Model\" }\n", "", "rung"},
		"instance rung not in the catalog": {"  review: { rung: opus }\n", "", "instance review.rung names rung \"opus\", which is not in the rung catalog"},
		"group rung not in the catalog":    {"", "    review: { rung: opus }\n", "group agentic review.rung names rung \"opus\", which is not in the rung catalog"},
		"instance reviews not a bool":      {"  actions: { reviews: \"no\" }\n", "", "reviews"},
		"instance unknown action key":      {"  actions: { review: false }\n", "", "actions"},
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
func TestCheckPolicyValidatesChangeAndReview(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	n := int64(99)
	opus, bad := "opus", "Not A Rung"
	for _, who := range []string{"instance", "group agentic"} {
		for name, c := range map[string]struct {
			p    gonkcfg.Policy
			want string
		}{
			"change paths":           {gonkcfg.Policy{Change: gonkcfg.ChangePolicy{Paths: []string{"internal/**"}}}, ": change: paths is project-only"},
			"change over ceiling":    {gonkcfg.Policy{Change: gonkcfg.ChangePolicy{MaxFiles: &n}}, ": change: max_files"},
			"change triage-verdict":  {gonkcfg.Policy{Change: gonkcfg.ChangePolicy{Triggers: []string{"triage-verdict"}}}, ": change: triggers"},
			"review over ceiling":    {gonkcfg.Policy{Review: gonkcfg.ReviewPolicy{MaxNotes: &n}}, ": review: max_notes"},
			"review target human":    {gonkcfg.Policy{Review: gonkcfg.ReviewPolicy{Targets: []string{"human"}}}, ": review: targets"},
			"review rung malformed":  {gonkcfg.Policy{Review: gonkcfg.ReviewPolicy{Rung: &bad}}, ": review: rung"},
			"review rung off-ladder": {gonkcfg.Policy{Review: gonkcfg.ReviewPolicy{Rung: &opus}}, " review.rung names rung \"opus\""},
		} {
			err := oc.checkPolicy(who, c.p)
			if err == nil || !strings.HasPrefix(err.Error(), "operator config: "+who+c.want) {
				t.Errorf("%s / %s: err = %v, want operator config: %s%s ...", who, name, err, who, c.want)
			}
		}
		// A reviewer rung that IS in the catalog passes, whether or not it is
		// on the instance ladder.
		glm := "glm"
		if err := oc.checkPolicy(who, gonkcfg.Policy{Review: gonkcfg.ReviewPolicy{Rung: &glm}}); err != nil {
			t.Errorf("%s: a catalog rung was refused as the reviewer: %v", who, err)
		}
	}
}

// GroupFor folds nested groups so that a child can only tighten its parent --
// and cannot swap the reviewer its parent named.
func TestGroupForFoldsChangeAndReviewTightenOnly(t *testing.T) {
	oc, err := Load([]byte(`
version: 1
instance: { ladder: [a, b], review: { rung: b } }
groups:
  top:
    actions: { reviews: false }
    change: { triggers: [label, command], create_classes: [test, docs], require_docs: always, docs_labels: [api], require_test_report: true, max_attempts: 2, max_open: 2, verify_timeout: 1h, auto_merge: { mode: "off", max_per_day: 2 } }
    review: { rung: b, max_notes: 10 }
  top/sub:
    actions: { reviews: true }
    change: { triggers: [command, mention], create_classes: [source, test], require_docs: never, docs_labels: [ux], require_test_report: false, max_attempts: 3, max_open: 1, verify_timeout: 4h, auto_merge: { mode: after-human-approval, max_per_day: 3 } }
    review: { rung: a, max_notes: 20 }
  other:
    change: { max_files: 1 }
    review: { max_reviews_per_mr: 1 }
rungs:
  - { name: a, kind: local, model: m, est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.2 }
  - { name: b, kind: local, model: n, est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.2 }
`))
	if err != nil {
		t.Fatal(err)
	}
	g := oc.GroupFor("top/sub/repo")
	if g.Actions.Reviews == nil || *g.Actions.Reviews {
		t.Errorf("a nested group re-enabled reviews its parent vetoed: %v", g.Actions.Reviews)
	}
	c := g.Change
	if !reflect.DeepEqual(c.Triggers, []string{"command"}) || !reflect.DeepEqual(c.CreateClasses, []string{"test"}) || *c.RequireDocs != "always" ||
		!reflect.DeepEqual(c.DocsLabels, []string{"api", "ux"}) || !*c.RequireTestReport || *c.MaxAttempts != 2 || *c.MaxOpen != 1 ||
		time.Duration(*c.VerifyTimeout) != time.Hour || *c.AutoMerge.Mode != "off" || *c.AutoMerge.MaxPerDay != 2 {
		t.Errorf("nested change fold = %+v", c)
	}
	r := g.Review
	if r.Rung == nil || *r.Rung != "" || *r.MaxNotes != 10 {
		t.Errorf("nested review fold = %+v, want NO rung (the two groups name different ones), 10 notes", r)
	}
	// A single matching group is carried, not dropped.
	o := oc.GroupFor("other/repo")
	if o.Change.MaxFiles == nil || *o.Change.MaxFiles != 1 || o.Review.MaxReviewsPerMR == nil || *o.Review.MaxReviewsPerMR != 1 {
		t.Errorf("a single group's change/review did not reach GroupFor: %+v / %+v", o.Change, o.Review)
	}
	// And the whole chain reaches an Effective: the project asks for the
	// widest of everything and gets the operator's bounds.
	pc, err := gonkcfg.Load([]byte("version: 1\nenabled: true\nladder: [a, b]\nactions: { features: true, reviews: true }\n" +
		"change: { triggers: [label, command, mention], create_classes: [source, test, docs], require_docs: never, docs_labels: [], max_attempts: 3, max_open: 3, verify_timeout: 6h, auto_merge: { mode: after-human-approval } }\n" +
		"review: { max_notes: 20 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	eff := gonkcfg.Resolve(oc.Instance, g, *pc)
	e := eff.Change
	if eff.Actions.Reviews || !reflect.DeepEqual(e.Triggers, []string{"command"}) || !reflect.DeepEqual(e.CreateClasses, []string{"test"}) ||
		e.RequireDocs != "always" || !reflect.DeepEqual(e.DocsLabels, []string{"api", "ux"}) || !e.RequireTestReport || e.MaxAttempts != 2 || e.MaxOpen != 1 ||
		e.VerifyTimeout != time.Hour || e.AutoMerge.Mode != "off" || e.AutoMerge.MaxPerDay != 2 {
		t.Errorf("effective = reviews %v, %+v", eff.Actions.Reviews, e)
	}
	// The instance says b, the folded group says "none": none wins.
	if eff.Review.Rung != "" || eff.Review.MaxNotes != 10 {
		t.Errorf("effective review = %+v, want no rung, 10 notes", eff.Review)
	}
	// Under a group that does not name a rung, the instance's stands.
	if got := gonkcfg.Resolve(oc.Instance, o, *pc).Review; got.Rung != "b" || got.MaxReviewsPerMR != 1 {
		t.Errorf("effective review under `other` = %+v, want rung b, 1 review per MR", got)
	}
}

// The operator SCHEMA on its own (Load also runs the Go check, which would
// hide a schema that stopped refusing these).
func TestOperatorSchemaAloneRefusesBadChangeAndReview(t *testing.T) {
	doc := func(layer string, p map[string]any) map[string]any {
		d := map[string]any{"version": 1, "rungs": []any{map[string]any{"name": "a", "kind": "local", "model": "m", "est_tokens": "200K"}}}
		if layer == "instance" {
			d["instance"] = p
		} else {
			d["groups"] = map[string]any{"agentic": p}
		}
		return d
	}
	sec := func(key string, body map[string]any) map[string]any { return map[string]any{key: body} }
	bad := map[string]map[string]any{
		"unknown change key":       sec("change", map[string]any{"banana": 1}),
		"unknown change merge key": sec("change", map[string]any{"auto_merge": map[string]any{"banana": 1}}),
		"change paths":             sec("change", map[string]any{"paths": []any{"internal/**"}}),
		"trigger triage-verdict":   sec("change", map[string]any{"triggers": []any{"triage-verdict"}}),
		"singular trigger":         sec("change", map[string]any{"trigger": "label"}),
		"create class deps":        sec("change", map[string]any{"create_classes": []any{"deps"}}),
		"require_docs sometimes":   sec("change", map[string]any{"require_docs": "sometimes"}),
		"docs label blank":         sec("change", map[string]any{"docs_labels": []any{" "}}),
		"report a string":          sec("change", map[string]any{"require_test_report": "yes"}),
		"attempts over":            sec("change", map[string]any{"max_attempts": 4}),
		"open over":                sec("change", map[string]any{"max_open": 4}),
		"files over":               sec("change", map[string]any{"max_files": 9}),
		"lines over":               sec("change", map[string]any{"max_changed_lines": 301}),
		"timeout a number":         sec("change", map[string]any{"verify_timeout": 3600}),
		"mode always":              sec("change", map[string]any{"auto_merge": map[string]any{"mode": "always"}}),
		"merge class ci":           sec("change", map[string]any{"auto_merge": map[string]any{"classes": []any{"ci"}}}),
		"merges per day over":      sec("change", map[string]any{"auto_merge": map[string]any{"max_per_day": 4}}),
		"fix merges per day over":  sec("pipeline_fix", map[string]any{"auto_merge": map[string]any{"max_per_day": 4}}),
		"unknown review key":       sec("review", map[string]any{"banana": 1}),
		"target human":             sec("review", map[string]any{"targets": []any{"human"}}),
		"rung not a name":          sec("review", map[string]any{"rung": "Big Model"}),
		"rung a list":              sec("review", map[string]any{"rung": []any{"a", "b"}}),
		// Owner decision: no operator key waives (or restates) the distinct model.
		"distinct model false":  sec("review", map[string]any{"require_distinct_model": false}),
		"distinct model true":   sec("review", map[string]any{"require_distinct_model": true}),
		"notes over":            sec("review", map[string]any{"max_notes": 21}),
		"reviews per mr over":   sec("review", map[string]any{"max_reviews_per_mr": 4}),
		"reviews not a boolean": sec("actions", map[string]any{"reviews": "no"}),
		"unknown action":        sec("actions", map[string]any{"review": false}),
	}
	for name, p := range bad {
		for _, layer := range []string{"instance", "group"} {
			if err := compiled.Validate(doc(layer, p)); err == nil {
				t.Errorf("%s (%s): the operator schema accepted %v", name, layer, p)
			}
		}
	}
	good := map[string]any{
		"actions": map[string]any{"reviews": false},
		"change": map[string]any{"triggers": []any{"label", "command", "mention"}, "create_classes": []any{"source", "test", "docs"}, "require_docs": "always",
			"docs_labels": []any{"api"}, "require_test_report": true, "max_attempts": 3, "max_open": 3, "verify_timeout": "6h", "max_files": 8, "max_changed_lines": 300,
			"auto_merge": map[string]any{"mode": "off", "classes": []any{"docs"}, "max_per_day": 3}},
		"review":       map[string]any{"rung": "a", "targets": []any{"gonk"}, "max_notes": 20, "max_reviews_per_mr": 3},
		"pipeline_fix": map[string]any{"auto_merge": map[string]any{"max_per_day": 3}},
	}
	for _, layer := range []string{"instance", "group"} {
		if err := compiled.Validate(doc(layer, good)); err != nil {
			t.Errorf("%s: the operator schema refused a valid change/review: %v", layer, err)
		}
	}
}
