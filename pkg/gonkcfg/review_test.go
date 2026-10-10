package gonkcfg

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// Independent review's configuration (gonk-cuwn plan 3.1, owner decisions
// 8.1): `actions.reviews` and the `review:` section. Documented in
// docs/reference/gonk-review.md and in the schema's own descriptions.

const reviewHead = "version: 1\nenabled: true\nactions: { pipelines: true, pipeline_fixes: true, reviews: true }\n"

func showAny(v any) string {
	b, _ := yaml.Marshal(v)
	return "{" + strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", ", ") + "}"
}

// defaultReview is the resolved section of a project that writes no `review:`
// keys, under an operator that names no reviewer. reason is what both merge
// statuses say.
func defaultReview(reason string) EffectiveReview {
	return EffectiveReview{
		Targets: []string{"gonk"}, Rung: "", MaxNotes: 20, MaxReviewsPerMR: 3,
		PipelineFixMerge: ReviewMergeStatus{Reason: reason}, ChangeMerge: ReviewMergeStatus{Reason: reason},
	}
}

func TestLoadDecodesTheReviewSection(t *testing.T) {
	pc, err := Load([]byte(reviewHead + "review:\n  targets: [gonk]\n  max_notes: 20\n  max_reviews_per_mr: 3\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := ReviewPolicy{Targets: []string{"gonk"}, MaxNotes: ip(20), MaxReviewsPerMR: ip(3)}
	if !reflect.DeepEqual(pc.Review, want) {
		t.Errorf("Review = %+v\nwant     %+v", pc.Review, want)
	}
	if pc.Actions.Reviews == nil || !*pc.Actions.Reviews {
		t.Errorf("actions.reviews not decoded: %+v", pc.Actions)
	}
	eff := Resolve(Policy{Ladder: []string{"qwen-local"}}, Policy{}, withLadder(*pc))
	if !eff.Actions.Reviews || !reflect.DeepEqual(eff.Review, defaultReview("no-review-rung")) {
		t.Errorf("effective = reviews %v, %+v\nwant true, %+v", eff.Actions.Reviews, eff.Review, defaultReview("no-review-rung"))
	}
}

// PLAN, must be seen red: "unknown key accepted", "a ceiling exceeded is
// accepted", "a project sets `review.rung`". Through Validate -- the SCHEMA
// alone.
func TestSchemaRefusesBadReview(t *testing.T) {
	cases := map[string]string{
		"unknown key in review":             "review: { banana: true }\n",
		"a project sets review.rung":        "review: { rung: qwen-local }\n",
		"a project sets rung beside others": "review: { targets: [gonk], rung: glm, max_notes: 5 }\n",
		"misspelt section":                  "reviews: { max_notes: 5 }\n",
		"target human":                      "review: { targets: [human] }\n",
		"target human beside gonk":          "review: { targets: [gonk, human] }\n",
		"target all":                        "review: { targets: [all] }\n",
		"targets not a list":                "review: { targets: gonk }\n",
		"duplicate target":                  "review: { targets: [gonk, gonk] }\n",
		// Owner decision: there is no key that waives the distinct model.
		"require_distinct_model: false": "review: { require_distinct_model: false }\n",
		"require_distinct_model: true":  "review: { require_distinct_model: true }\n",
		"same_model_allowed":            "review: { same_model_allowed: true }\n",
		"max_notes above the ceiling":   "review: { max_notes: 21 }\n",
		"max_notes zero":                "review: { max_notes: 0 }\n",
		"max_notes fractional":          "review: { max_notes: 2.5 }\n",
		"max_reviews_per_mr above":      "review: { max_reviews_per_mr: 4 }\n",
		"max_reviews_per_mr zero":       "review: { max_reviews_per_mr: 0 }\n",
		"review a list":                 "review: [gonk]\n",
	}
	for name, body := range cases {
		if err := Validate([]byte(reviewHead + body)); err == nil {
			t.Errorf("%s: the schema accepted\n%s", name, body)
		}
	}
	for name, doc := range map[string]string{
		"the action a string":        "version: 1\nenabled: true\nactions: { features: true, reviews: \"yes\" }\n",
		"the action a number":        "version: 1\nenabled: true\nactions: { features: true, reviews: 1 }\n",
		"misspelt action":            "version: 1\nenabled: true\nactions: { features: true, review: true }\n",
		"thread-style review action": "version: 1\nenabled: true\nactions: { features: true, reviews: true, approve: true }\n",
	} {
		if err := Validate([]byte(doc)); err == nil {
			t.Errorf("%s: the schema accepted\n%s", name, doc)
		}
	}
}

func TestSchemaAcceptsEveryDocumentedReviewValue(t *testing.T) {
	for name, body := range map[string]string{
		"targets":                "review: { targets: [gonk] }\n",
		"no target":              "review: { targets: [] }\n",
		"the ceilings":           "review: { max_notes: 20, max_reviews_per_mr: 3 }\n",
		"the floors":             "review: { max_notes: 1, max_reviews_per_mr: 1 }\n",
		"section without a body": "",
	} {
		if _, err := Load([]byte(reviewHead + body)); err != nil {
			t.Errorf("%s: Load refused\n%s%v", name, body, err)
		}
	}
	// A staged section: the block is written, the action is not switched on.
	if _, err := Load([]byte("version: 1\nenabled: true\nreview: { max_notes: 5 }\n")); err != nil {
		t.Errorf("a review section without the action was refused: %v", err)
	}
}

// PLAN, must be seen red: "`reviews` on without its opt-in dependency
// accepted". Two guards, tested apart so that neither can hide the other's
// absence: the schema's if/then (through Validate), and CheckReview (on a
// decoded Policy).
func TestReviewsRequiresAnAuthorWorkflow(t *testing.T) {
	head := "version: 1\nenabled: true\n"
	cases := []struct {
		name    string
		actions string
		policy  ActionsPolicy
		invalid bool
	}{
		{"reviews alone", "actions: { reviews: true }\n", ActionsPolicy{Reviews: bp(true)}, true},
		{"reviews with triage and pipelines only", "actions: { triage: true, pipelines: true, reviews: true }\n", ActionsPolicy{Triage: bp(true), Pipelines: bp(true), Reviews: bp(true)}, true},
		{"reviews with both author workflows false", "actions: { pipelines: true, pipeline_fixes: false, features: false, reviews: true }\n",
			ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(false), Features: bp(false), Reviews: bp(true)}, true},
		{"reviews with scaffold only", "actions: { scaffold: true, reviews: true }\n", ActionsPolicy{Scaffold: bp(true), Reviews: bp(true)}, true},
		{"reviews with fixes", "actions: { pipelines: true, pipeline_fixes: true, reviews: true }\n", ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true), Reviews: bp(true)}, false},
		{"reviews with features", "actions: { features: true, reviews: true }\n", ActionsPolicy{Features: bp(true), Reviews: bp(true)}, false},
		{"reviews with features, fixes false", "actions: { pipeline_fixes: false, features: true, reviews: true }\n", ActionsPolicy{PipelineFixes: bp(false), Features: bp(true), Reviews: bp(true)}, false},
		{"reviews with both", "actions: { pipelines: true, pipeline_fixes: true, features: true, reviews: true }\n",
			ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true), Features: bp(true), Reviews: bp(true)}, false},
		{"reviews false, nothing else", "actions: { reviews: false }\n", ActionsPolicy{Reviews: bp(false)}, false},
		{"features alone", "actions: { features: true }\n", ActionsPolicy{Features: bp(true)}, false},
		{"neither", "actions: { triage: true }\n", ActionsPolicy{Triage: bp(true)}, false},
	}
	for _, c := range cases {
		if err := Validate([]byte(head + c.actions)); (err != nil) != c.invalid {
			t.Errorf("%s: schema: err = %v, want invalid=%v", c.name, err, c.invalid)
		}
		err := CheckReview(Policy{Actions: c.policy})
		if (err != nil) != c.invalid {
			t.Errorf("%s: CheckReview: err = %v, want invalid=%v", c.name, err, c.invalid)
		}
		if c.invalid && err != nil && (!strings.Contains(err.Error(), "actions.reviews") || !strings.Contains(err.Error(), "actions.pipeline_fixes") || !strings.Contains(err.Error(), "actions.features")) {
			t.Errorf("%s: err = %v, want it to name all three actions", c.name, err)
		}
		if _, err := Load([]byte(head + c.actions)); (err != nil) != c.invalid {
			t.Errorf("%s: Load: err = %v, want invalid=%v", c.name, err, c.invalid)
		} else if err != nil && !strings.HasPrefix(err.Error(), ".gonk.yml: ") {
			t.Errorf("%s: Load err = %v, want a .gonk.yml error", c.name, err)
		}
	}
}

func TestReviewPolicyValidate(t *testing.T) {
	cases := []struct {
		name  string
		p     ReviewPolicy
		layer scope.Layer
		want  string // "" = valid
	}{
		{"zero", ReviewPolicy{}, scope.LayerProject, ""},
		{"zero, operator", ReviewPolicy{}, scope.LayerOperator, ""},
		{"everything a project may write", ReviewPolicy{Targets: []string{"gonk"}, MaxNotes: ip(20), MaxReviewsPerMR: ip(3)}, scope.LayerProject, ""},
		{"an operator names the rung", ReviewPolicy{Rung: sp("qwen3-6-35b"), MaxNotes: ip(1)}, scope.LayerOperator, ""},
		{"unknown layer", ReviewPolicy{}, scope.Layer("batch"), "unknown layer"},
		{"a project names the rung", ReviewPolicy{Rung: sp("qwen-local")}, scope.LayerProject, "operator-only"},
		{"a project names an empty rung", ReviewPolicy{Rung: sp("")}, scope.LayerProject, "operator-only"},
		{"an operator rung that is not a rung name", ReviewPolicy{Rung: sp("Qwen Local")}, scope.LayerOperator, "rung"},
		{"an operator empty rung", ReviewPolicy{Rung: sp("")}, scope.LayerOperator, "rung"},
		{"target human", ReviewPolicy{Targets: []string{"human"}}, scope.LayerProject, "targets"},
		{"target human, operator", ReviewPolicy{Targets: []string{"gonk", "human"}}, scope.LayerOperator, "targets"},
		{"max_notes 21", ReviewPolicy{MaxNotes: ip(21)}, scope.LayerProject, "max_notes"},
		{"max_notes 0", ReviewPolicy{MaxNotes: ip(0)}, scope.LayerOperator, "max_notes"},
		{"max_reviews_per_mr 4", ReviewPolicy{MaxReviewsPerMR: ip(4)}, scope.LayerProject, "max_reviews_per_mr"},
		{"max_reviews_per_mr -1", ReviewPolicy{MaxReviewsPerMR: ip(-1)}, scope.LayerProject, "max_reviews_per_mr"},
	}
	for _, c := range cases {
		err := c.p.Validate(c.layer)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: Validate = %v, want nil", c.name, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: Validate accepted %+v", c.name, c.p)
		case c.want != "" && (!strings.HasPrefix(err.Error(), "review: ") || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want a review error mentioning %q", c.name, err, c.want)
		}
	}
	// CheckReview is what Load runs: a Go-built project policy with a rung.
	if err := CheckReview(Policy{Review: ReviewPolicy{Rung: sp("glm")}}); err == nil || !strings.Contains(err.Error(), "operator-only") {
		t.Errorf("CheckReview let a project name the reviewer rung: %v", err)
	}
}

func TestResolveReview(t *testing.T) {
	def := defaultReview("")
	with := func(mut func(*EffectiveReview)) EffectiveReview {
		e := defaultReview("")
		mut(&e)
		return e
	}
	z := ReviewPolicy{}
	cases := []struct {
		name                     string
		instance, group, project ReviewPolicy
		want                     EffectiveReview
	}{
		{"nothing set anywhere: the defaults, and no reviewer", z, z, z, def},

		// ---- the rung is the operator's alone
		{"the instance names the reviewer", ReviewPolicy{Rung: sp("big")}, z, z, with(func(e *EffectiveReview) { e.Rung = "big" })},
		{"a group names the reviewer", z, ReviewPolicy{Rung: sp("big")}, z, with(func(e *EffectiveReview) { e.Rung = "big" })},
		{"instance and group agree", ReviewPolicy{Rung: sp("big")}, ReviewPolicy{Rung: sp("big")}, z, with(func(e *EffectiveReview) { e.Rung = "big" })},
		{"instance and group DISAGREE: no reviewer", ReviewPolicy{Rung: sp("big")}, ReviewPolicy{Rung: sp("small")}, z, def},
		{"a project's rung is ignored", z, z, ReviewPolicy{Rung: sp("small")}, def},
		{"a project's rung does not replace the operator's", ReviewPolicy{Rung: sp("big")}, z, ReviewPolicy{Rung: sp("small")}, with(func(e *EffectiveReview) { e.Rung = "big" })},
		{"an unvalidated operator rung that is not a name is dropped", ReviewPolicy{Rung: sp("Not A Rung")}, z, z, def},

		// ---- targets
		{"a project's empty target list reviews nothing", z, z, ReviewPolicy{Targets: []string{}}, with(func(e *EffectiveReview) { e.Targets = []string{} })},
		{"an operator's empty target list reviews nothing", ReviewPolicy{Targets: []string{}}, z, z, with(func(e *EffectiveReview) { e.Targets = []string{} })},
		{"an unvalidated human target is dropped", z, z, ReviewPolicy{Targets: []string{"human", "gonk"}}, def},
		{"an operator's unvalidated human target adds nothing", z, ReviewPolicy{Targets: []string{"human"}}, z, with(func(e *EffectiveReview) { e.Targets = []string{} })},

		// ---- numbers
		{"numbers: the project tightens", z, z, ReviewPolicy{MaxNotes: ip(5), MaxReviewsPerMR: ip(1)},
			with(func(e *EffectiveReview) { e.MaxNotes, e.MaxReviewsPerMR = 5, 1 })},
		{"numbers: the instance is lowest", ReviewPolicy{MaxNotes: ip(3), MaxReviewsPerMR: ip(1)}, ReviewPolicy{MaxNotes: ip(9), MaxReviewsPerMR: ip(2)}, ReviewPolicy{MaxNotes: ip(20), MaxReviewsPerMR: ip(3)},
			with(func(e *EffectiveReview) { e.MaxNotes, e.MaxReviewsPerMR = 3, 1 })},
		{"numbers: the group is lowest", ReviewPolicy{MaxNotes: ip(9), MaxReviewsPerMR: ip(2)}, ReviewPolicy{MaxNotes: ip(3), MaxReviewsPerMR: ip(1)}, ReviewPolicy{MaxNotes: ip(20), MaxReviewsPerMR: ip(3)},
			with(func(e *EffectiveReview) { e.MaxNotes, e.MaxReviewsPerMR = 3, 1 })},
		{"numbers: an unvalidated value past the ceiling is clamped", z, z, ReviewPolicy{MaxNotes: ip(999), MaxReviewsPerMR: ip(999)}, def},
		{"numbers: an operator value past the default raises nothing", ReviewPolicy{MaxNotes: ip(999), MaxReviewsPerMR: ip(999)}, z, ReviewPolicy{MaxNotes: ip(4), MaxReviewsPerMR: ip(2)},
			with(func(e *EffectiveReview) { e.MaxNotes, e.MaxReviewsPerMR = 4, 2 })},
		{"numbers: an unvalidated negative is clamped to zero", z, z, ReviewPolicy{MaxNotes: ip(-1), MaxReviewsPerMR: ip(-1)},
			with(func(e *EffectiveReview) { e.MaxNotes, e.MaxReviewsPerMR = 0, 0 })},
	}
	for _, c := range cases {
		got := ResolveReview(c.instance, c.group, c.project)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, got, c.want)
		}
		// Resolve carries exactly this fold; with reviews off both merge
		// statuses say so, whatever the section holds.
		eff := Resolve(Policy{Review: c.instance}, Policy{Review: c.group}, ProjectConfig{Policy: Policy{Review: c.project}})
		want := c.want
		want.PipelineFixMerge, want.ChangeMerge = ReviewMergeStatus{Reason: "reviews-off"}, ReviewMergeStatus{Reason: "reviews-off"}
		if !reflect.DeepEqual(eff.Review, want) {
			t.Errorf("%s: Resolve().Review = %+v, want %+v", c.name, eff.Review, want)
		}
	}
}

func TestFoldReviewOnlyTightens(t *testing.T) {
	a := ReviewPolicy{Targets: []string{"gonk"}, Rung: sp("big"), MaxNotes: ip(20), MaxReviewsPerMR: ip(1)}
	b := ReviewPolicy{Targets: []string{}, Rung: sp("big"), MaxNotes: ip(7), MaxReviewsPerMR: ip(3)}
	want := ReviewPolicy{Targets: []string{}, Rung: sp("big"), MaxNotes: ip(7), MaxReviewsPerMR: ip(1)}
	for name, got := range map[string]ReviewPolicy{"a then b": FoldReview(a, b), "b then a": FoldReview(b, a)} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %s\n want %s", name, showAny(got), showAny(want))
		}
	}
	if got := FoldReview(ReviewPolicy{}, ReviewPolicy{}); !got.IsZero() {
		t.Errorf("two silent layers folded to %s", showAny(got))
	}
	if got := FoldReview(a, ReviewPolicy{}); !reflect.DeepEqual(got, a) {
		t.Errorf("folding a silent layer changed the policy: %s", showAny(got))
	}
	// Different rungs: none -- and "none" is not "silent", so a third layer
	// cannot bring a reviewer back.
	conflict := FoldReview(ReviewPolicy{Rung: sp("big")}, ReviewPolicy{Rung: sp("small")})
	if conflict.Rung == nil || *conflict.Rung != "" {
		t.Fatalf("two different rungs folded to %v, want a non-nil empty rung", conflict.Rung)
	}
	if again := FoldReview(conflict, ReviewPolicy{Rung: sp("big")}); again.Rung == nil || *again.Rung != "" {
		t.Errorf("a third layer restored a reviewer after a conflict: %v", again.Rung)
	}
}

// Effective.Actions.Reviews: the project opts in, any operator layer may veto,
// and it is never true without an effective author workflow.
func TestResolveReviewsAction(t *testing.T) {
	both := ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true), Features: bp(true), Reviews: bp(true)}
	cases := []struct {
		name                     string
		instance, group, project ActionsPolicy
		want                     bool
	}{
		{"project opts in with fixes", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true), Reviews: bp(true)}, true},
		{"project opts in with features", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{Features: bp(true), Reviews: bp(true)}, true},
		{"project silent", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{Pipelines: bp(true), PipelineFixes: bp(true), Features: bp(true)}, false},
		{"project says false", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{Features: bp(true), Reviews: bp(false)}, false},
		{"an operator true opts nobody in", ActionsPolicy{Reviews: bp(true)}, ActionsPolicy{Reviews: bp(true)}, ActionsPolicy{Features: bp(true)}, false},
		{"instance vetoes reviews", ActionsPolicy{Reviews: bp(false)}, ActionsPolicy{}, both, false},
		{"group vetoes reviews", ActionsPolicy{}, ActionsPolicy{Reviews: bp(false)}, both, false},
		{"instance vetoes ONE author workflow: reviews stay", ActionsPolicy{Features: bp(false)}, ActionsPolicy{}, both, true},
		{"operators veto BOTH author workflows: reviews go with them", ActionsPolicy{Features: bp(false)}, ActionsPolicy{PipelineFixes: bp(false)}, both, false},
		{"an operator vetoes diagnoses and features", ActionsPolicy{Pipelines: bp(false), Features: bp(false)}, ActionsPolicy{}, both, false},
		{"a Go-built project with reviews and no author workflow", ActionsPolicy{}, ActionsPolicy{}, ActionsPolicy{Triage: bp(true), Pipelines: bp(true), Reviews: bp(true)}, false},
	}
	for _, c := range cases {
		eff := Resolve(Policy{Actions: c.instance, Ladder: []string{"qwen-local"}}, Policy{Actions: c.group},
			ProjectConfig{Policy: Policy{Enabled: bp(true), Actions: c.project, Ladder: []string{"qwen-local"}}})
		if eff.Actions.Reviews != c.want {
			t.Errorf("%s: Actions.Reviews = %v, want %v", c.name, eff.Actions.Reviews, c.want)
		}
		if eff.Actions.Reviews && !eff.Actions.PipelineFixes && !eff.Actions.Features {
			t.Errorf("%s: reviews are on while no workflow authors a merge request", c.name)
		}
	}
}

// What Resolve alone says about an agentic-review merge: the reason it knows,
// and NEVER "available" -- comparing models needs the rung catalog.
func TestResolveNeverCallsAgenticReviewAvailable(t *testing.T) {
	on := ActionsPolicy{Features: bp(true), Reviews: bp(true)}
	for _, c := range []struct {
		name     string
		instance Policy
		project  Policy
		want     string
	}{
		{"reviews off", Policy{Review: ReviewPolicy{Rung: sp("big")}}, Policy{Actions: ActionsPolicy{Features: bp(true)}}, "reviews-off"},
		{"reviews vetoed", Policy{Actions: ActionsPolicy{Reviews: bp(false)}, Review: ReviewPolicy{Rung: sp("big")}}, Policy{Actions: on}, "reviews-off"},
		{"reviews on, no reviewer rung", Policy{}, Policy{Actions: on}, "no-review-rung"},
		{"reviews on, reviewer named", Policy{Review: ReviewPolicy{Rung: sp("big")}}, Policy{Actions: on}, "models-not-compared"},
	} {
		c.instance.Ladder = []string{"small", "big"}
		c.project.Enabled, c.project.Ladder = bp(true), []string{"small", "big"}
		eff := Resolve(c.instance, Policy{}, ProjectConfig{Policy: c.project})
		for wf, st := range map[string]ReviewMergeStatus{WorkflowPipelineFix: eff.Review.PipelineFixMerge, WorkflowChange: eff.Review.ChangeMerge} {
			if st.Available || st.Reason != c.want {
				t.Errorf("%s / %s: status = %+v, want unavailable/%s", c.name, wf, st, c.want)
			}
			if got := eff.Review.MergeStatus(wf); got != st {
				t.Errorf("%s: MergeStatus(%s) = %+v, want %+v", c.name, wf, got, st)
			}
		}
		if got := eff.Review.MergeStatus("triage"); got.Available || got.Reason != "" {
			t.Errorf("MergeStatus of an unknown workflow = %+v, want the zero (unavailable) status", got)
		}
	}
	// Each workflow gets ITS status, not the other's.
	two := EffectiveReview{PipelineFixMerge: ReviewMergeStatus{Available: true}, ChangeMerge: ReviewMergeStatus{Reason: "no-distinct-model"}}
	if got := two.MergeStatus(WorkflowPipelineFix); !got.Available || got.Reason != "" {
		t.Errorf("MergeStatus(pipeline-fix) = %+v, want the pipeline-fix status (available)", got)
	}
	if got := two.MergeStatus(WorkflowChange); got.Available || got.Reason != "no-distinct-model" {
		t.Errorf("MergeStatus(change) = %+v, want the change status (no-distinct-model)", got)
	}
	if WorkflowPipelineFix != "pipeline-fix" || WorkflowChange != "change" {
		t.Errorf("workflow names = %q, %q", WorkflowPipelineFix, WorkflowChange)
	}
}

// Owner decision Q4: the agentic-review merge mode is UNAVAILABLE -- off, with
// the reason -- when the review cannot authorise it. Never silently satisfied.
func TestUsableAutoMergeMode(t *testing.T) {
	am := func(mode string) EffectiveAutoMerge {
		return EffectiveAutoMerge{Mode: mode, Classes: []string{"docs"}, MaxPerDay: 3}
	}
	ok := ReviewMergeStatus{Available: true}
	no := func(reason string) ReviewMergeStatus { return ReviewMergeStatus{Reason: reason} }
	for _, c := range []struct {
		name      string
		action    bool
		am        EffectiveAutoMerge
		st        ReviewMergeStatus
		mode, why string
	}{
		{"off stays off", true, am("off"), ok, "off", ""},
		{"human approval does not need a review", true, am("after-human-approval"), no("reviews-off"), "after-human-approval", ""},
		{"human approval, workflow off", false, am("after-human-approval"), ok, "off", "action-off"},
		{"agentic review, available", true, am("after-agentic-review"), ok, "after-agentic-review", ""},
		{"agentic review, reviews off", true, am("after-agentic-review"), no("reviews-off"), "off", "reviews-off"},
		{"agentic review, no reviewer", true, am("after-agentic-review"), no("no-review-rung"), "off", "no-review-rung"},
		{"agentic review, same model only", true, am("after-agentic-review"), no("no-distinct-model"), "off", "no-distinct-model"},
		{"agentic review, a status nobody filled in", true, am("after-agentic-review"), ReviewMergeStatus{}, "off", "models-not-compared"},
		{"agentic review, workflow off", false, am("after-agentic-review"), ok, "off", "action-off"},
		{"an unknown mode", true, am("always"), ok, "off", "unknown-mode"},
		{"an empty mode (a meter that predates the field)", true, am(""), ok, "off", "unknown-mode"},
	} {
		mode, why := UsableAutoMergeMode(c.action, c.am, c.st)
		if mode != c.mode || why != c.why {
			t.Errorf("%s: (%q, %q), want (%q, %q)", c.name, mode, why, c.mode, c.why)
		}
	}
}

// Owner rule: schema changes are documented where the schema lives. Every key
// of the change and review surface carries a description in the published
// schema, says it is not active yet where it switches something on, and is
// named in its reference document.
func TestEveryChangeAndReviewKeyIsDocumented(t *testing.T) {
	type props = map[string]map[string]any
	var doc struct {
		Properties struct {
			Actions struct {
				Properties props `json:"properties"`
			} `json:"actions"`
			Change struct {
				Description string `json:"description"`
				Properties  props  `json:"properties"`
			} `json:"change"`
			Review struct {
				Description string `json:"description"`
				Properties  props  `json:"properties"`
			} `json:"review"`
			PipelineFix struct {
				Properties props `json:"properties"`
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
	desc := func(p map[string]any) string { d, _ := p["description"].(string); return d }
	sub := func(p map[string]any, k string) map[string]any {
		m, _ := p["properties"].(map[string]any)
		s, _ := m[k].(map[string]any)
		return s
	}
	for name, d := range map[string]string{"actions.reviews": desc(doc.Properties.Actions.Properties["reviews"]),
		"change": doc.Properties.Change.Description, "review": doc.Properties.Review.Description} {
		if len(d) < 40 || !strings.Contains(d, "NOT ACTIVE YET") {
			t.Errorf("%s: the schema description must exist and say NOT ACTIVE YET: %q", name, d)
		}
	}
	for file, c := range map[string]struct {
		section string
		have    props
		want    []string
		merge   bool
	}{
		"gonk-change.md": {"change", doc.Properties.Change.Properties, []string{"triggers", "create_classes", "require_docs", "docs_labels", "require_test_report",
			"max_attempts", "max_open", "verify_timeout", "max_files", "max_changed_lines", "paths", "auto_merge"}, true},
		"gonk-review.md": {"review", doc.Properties.Review.Properties, []string{"targets", "max_notes", "max_reviews_per_mr"}, false},
	} {
		ref, err := os.ReadFile("../../docs/reference/" + file)
		if err != nil {
			t.Errorf("the reference document is missing: %v", err)
			continue
		}
		if !strings.Contains(string(ref), "NOT ACTIVE YET") {
			t.Errorf("docs/reference/%s does not say NOT ACTIVE YET", file)
		}
		if len(c.have) != len(c.want) {
			t.Errorf("the schema has %d %s keys, this test knows %d: add the new key here and to the reference", len(c.have), c.section, len(c.want))
		}
		keys := append([]string{}, c.want...)
		for _, k := range c.want {
			p, ok := c.have[k]
			if !ok {
				t.Errorf("%s.%s is not in the schema", c.section, k)
				continue
			}
			if len(desc(p)) < 40 {
				t.Errorf("%s.%s has no real description in the schema: %q", c.section, k, desc(p))
			}
			if k == "auto_merge" && c.merge {
				for _, sk := range []string{"mode", "classes", "max_per_day"} {
					if len(desc(sub(p, sk))) < 40 {
						t.Errorf("%s.auto_merge.%s has no real description in the schema", c.section, sk)
					}
					keys = append(keys, "auto_merge."+sk)
				}
			}
		}
		for _, k := range keys {
			if !strings.Contains(string(ref), "`"+k+"`") {
				t.Errorf("docs/reference/%s never names `%s`", file, k)
			}
		}
	}
	// The operator-only key, and the key added to the pipeline-fix block.
	review, err := os.ReadFile("../../docs/reference/gonk-review.md")
	if err == nil && (!strings.Contains(string(review), "`rung`") || !strings.Contains(string(review), "`reviews`")) {
		t.Error("docs/reference/gonk-review.md never names `rung` or `reviews`")
	}
	if d := desc(sub(doc.Properties.PipelineFix.Properties["auto_merge"], "max_per_day")); len(d) < 40 {
		t.Errorf("pipeline_fix.auto_merge.max_per_day has no real description in the schema: %q", d)
	}
	fix, err := os.ReadFile("../../docs/reference/gonk-pipeline-fix.md")
	if err != nil || !strings.Contains(string(fix), "`auto_merge.max_per_day`") {
		t.Errorf("docs/reference/gonk-pipeline-fix.md never names `auto_merge.max_per_day` (%v)", err)
	}
	// Owner decision: no layer has a key that lets the same model satisfy the
	// agentic-review merge mode.
	for _, file := range []string{"../../docs/schemas/gonk-config.v1.schema.json", "../../docs/schemas/gonk-operator.v1.schema.json"} {
		b, err := os.ReadFile(file)
		if err != nil || strings.Contains(string(b), "\"require_distinct_model\"") || strings.Contains(string(b), "same_model") {
			t.Errorf("%s offers a distinct-model opt-out (%v)", file, err)
		}
	}
	// A project schema that offered `rung` would let a project choose its
	// reviewer.
	if _, ok := doc.Properties.Review.Properties["rung"]; ok {
		t.Error("the PROJECT schema lists review.rung; it is operator-only")
	}
}
