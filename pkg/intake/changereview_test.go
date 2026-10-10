package intake

import (
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// gonk-cuwn part X-CONFIG: intake's pre-filters for the issue-change and
// mr-review triggers. Each must be a SUBSET of meter's rule and must not be
// opened by any other action. Nothing fires either trigger yet.
func TestMayChangeIssuesAndMayReview(t *testing.T) {
	with := func(a meterapi.Actions, rung string) func(*meterapi.ProjectResponse) {
		return func(r *meterapi.ProjectResponse) { r.Effective.Actions, r.Effective.Review.Rung = a, rung }
	}
	everyOther := meterapi.Actions{Triage: true, Pipelines: true, PipelineFixes: true, Scaffold: true}
	for _, c := range []struct {
		name               string
		cls                Classification
		wantChange, wantRv bool
	}{
		{"features on", Classify(obs(nil), active(with(meterapi.Actions{Features: true}, "big"))), true, false},
		{"features on, no .agent/ yet (pending)", Classify(obs(func(o *Observation) { o.AgentDirPresent = false }),
			active(with(meterapi.Actions{Features: true}, "big"))), true, false},
		{"reviews with features, reviewer named", Classify(obs(nil), active(with(meterapi.Actions{Features: true, Reviews: true}, "big"))), true, true},
		{"reviews with fixes, reviewer named", Classify(obs(nil), active(with(meterapi.Actions{Pipelines: true, PipelineFixes: true, Reviews: true}, "big"))), false, true},
		{"reviews on, NO reviewer rung", Classify(obs(nil), active(with(meterapi.Actions{Features: true, Reviews: true}, ""))), true, false},
		{"reviews without an author workflow (a meter that predates the conjunction)", Classify(obs(nil), active(with(meterapi.Actions{Triage: true, Pipelines: true, Reviews: true}, "big"))), false, false},
		{"every OTHER action on", Classify(obs(nil), active(with(everyOther, "big"))), false, false},
		{"triage only (the onboarding default)", Classify(obs(nil), active(nil)), false, false},
		{"meter unreachable", Classify(obs(nil), nil), false, false},
		{"not a member", Classify(obs(func(o *Observation) { o.Member = false }), active(with(meterapi.Actions{Features: true, Reviews: true}, "big"))), false, false},
		{"disabled by meter, actions still reported on", Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
			r.State, r.DisabledReason = meterapi.StateDisabled, "disabled by group policy"
			r.Effective.Actions, r.Effective.Review.Rung = meterapi.Actions{Features: true, Reviews: true}, "big"
		})), false, false},
		{"key missing, actions on", Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
			r.State = meterapi.StateKeyMissing
			r.Effective.Actions, r.Effective.Review.Rung = meterapi.Actions{Features: true, Reviews: true}, "big"
		})), false, false},
		{"no config", Classify(obs(func(o *Observation) { o.ConfigBytes = nil }), nil), false, false},
	} {
		if got := c.cls.MayChangeIssues(); got != c.wantChange {
			t.Errorf("%s: MayChangeIssues = %v, want %v (state %q)", c.name, got, c.wantChange, c.cls.State)
		}
		if got := c.cls.MayReview(); got != c.wantRv {
			t.Errorf("%s: MayReview = %v, want %v (state %q)", c.name, got, c.wantRv, c.cls.State)
		}
		// mayRun is the per-trigger switch gate() uses; it must agree.
		if got := mayRun(c.cls, atags.TriggerIssueChange); got != c.wantChange {
			t.Errorf("%s: mayRun(issue-change) = %v, want %v", c.name, got, c.wantChange)
		}
		if got := mayRun(c.cls, atags.TriggerMRReview); got != c.wantRv {
			t.Errorf("%s: mayRun(mr-review) = %v, want %v", c.name, got, c.wantRv)
		}
	}
}

// The gate, end to end: a triage-only project is refused both triggers by name.
func TestGateRefusesChangeAndReviewWithoutTheirActions(t *testing.T) {
	for _, trig := range []string{atags.TriggerIssueChange, atags.TriggerMRReview} {
		if d, ok := gate(triageOnlyEntry(42), trig); ok || d.Reason != "action_disabled" {
			t.Errorf("a triage-only project passed the %s gate: ok=%v %+v", trig, ok, d)
		}
		if d, ok := gate(pipelinesEntry(42), trig); ok || d.Reason != "action_disabled" {
			t.Errorf("a diagnose-only project passed the %s gate: ok=%v %+v", trig, ok, d)
		}
	}
	optedIn := pipelinesEntry(42)
	optedIn.Classification = Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Actions, r.Effective.Review.Rung = meterapi.Actions{Features: true, Reviews: true}, "big"
	}))
	for _, trig := range []string{atags.TriggerIssueChange, atags.TriggerMRReview} {
		if d, ok := gate(optedIn, trig); !ok {
			t.Errorf("an opted-in project was refused the %s gate: %+v", trig, d)
		}
	}
}

// The onboarding .gonk.yml leaves reviews OFF, writes no change or review
// section, and the merge request explains every new key AND says plainly that
// none of it is active yet.
func TestOnboardingLeavesChangeAndReviewOffAndSaysNotActive(t *testing.T) {
	raw, err := RenderDefaultConfig([]string{"qwen-local"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n  reviews: false ") || !strings.Contains(string(raw), "reviews: false           # NOT ACTIVE YET") {
		t.Errorf("the onboarding .gonk.yml does not state `reviews: false` with NOT ACTIVE YET beside it:\n%s", raw)
	}
	if strings.Contains(string(raw), "\nchange:") || strings.Contains(string(raw), "\nreview:") {
		t.Errorf("the onboarding .gonk.yml writes a change: or review: section:\n%s", raw)
	}
	cfg, err := gonkcfg.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Actions.Reviews == nil || *cfg.Actions.Reviews || !cfg.Change.IsZero() || !cfg.Review.IsZero() {
		t.Errorf("onboarding config: reviews = %v, change = %+v, review = %+v; want explicit false and no sections", cfg.Actions.Reviews, cfg.Change, cfg.Review)
	}
	eff := gonkcfg.Resolve(gonkcfg.Policy{}, gonkcfg.Policy{}, *cfg)
	if eff.Actions.Reviews || eff.Actions.Features || eff.Change.AutoMerge.Mode != gonkcfg.AutoMergeOff || eff.Review.ChangeMerge.Available {
		t.Errorf("merging the onboarding config would authorize a change or a review: %+v / %+v / %+v", eff.Actions, eff.Change, eff.Review)
	}

	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
	if err != nil {
		t.Fatal(err)
	}
	const notActive = "**Not active yet: nothing in this version of gonk starts this workflow, so this key changes nothing today.**"
	// One table row per key, each saying it is not active.
	for _, key := range []string{"change.triggers", "change.create_classes", "change.require_docs", "change.docs_labels", "change.require_test_report",
		"change.max_attempts", "change.max_open", "change.verify_timeout", "change.max_files", "change.max_changed_lines", "change.paths", "change.auto_merge",
		"review.targets", "review.max_notes", "review.max_reviews_per_mr"} {
		row := ""
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "| `"+key+"` | unset | ") {
				row = line
			}
		}
		switch {
		case row == "":
			t.Errorf("the onboarding merge request has no row for `%s`", key)
		case !strings.Contains(row, "Not active yet"):
			t.Errorf("the row for `%s` does not say it is not active yet:\n%s", key, row)
		}
	}
	for _, want := range []string{
		"| `actions.reviews` | `false` | gonk will not review merge requests. " + notActive,
		"| `change.triggers` | unset | " + notActive,
		"| `review.targets` | unset | " + notActive,
		"only on an explicit request from a project **Maintainer or Owner**",
		"The same request from anyone below Maintainer does nothing",
		"gonk never decides on its own that an issue should be implemented",
		"never approves, never assigns itself, never opens a discussion thread and never merges",
		"Merge requests written by people are not reviewed",
		"writing it with neither is refused as a configuration error",
		"writing `rung` here is refused",
		"only when the reviewer ran on a different model than the author; no setting changes that",
		"that mode is unavailable and merges nothing",
		"meaning prose files only",
		"Unset means `[test, docs]`",
		"gonk runs nothing itself: your pipeline is the only thing that tests the change",
		"`max_per_day` (default and maximum 3) caps automatic merges in 24 hours",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the onboarding merge request does not say %q", want)
		}
	}
	// The pipeline-fix block's new key is explained on ITS row.
	fixRow := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "| `pipeline_fix.auto_merge` | unset | ") {
			fixRow = line
		}
	}
	if !strings.Contains(fixRow, "`max_per_day` (default and maximum 3) caps automatic merges in 24 hours") {
		t.Errorf("the `pipeline_fix.auto_merge` row does not explain max_per_day:\n%s", fixRow)
	}
	// It must not suggest a triage verdict or a Developer can start the workflow.
	for _, never := range []string{"triage-verdict", "Developer"} {
		if strings.Contains(body, never) {
			t.Errorf("the onboarding merge request mentions %q", never)
		}
	}
}
