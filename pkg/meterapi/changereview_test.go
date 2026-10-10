package meterapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/gonkcfg"
)

// gonk-cuwn part X-CONFIG: the folded change and review configuration crosses
// the wire field for field. Every value here differs from its default AND from
// its zero, so a field EffectiveFrom forgot shows up as a mismatch.
func crEffective() gonkcfg.Effective {
	return gonkcfg.Effective{
		Actions: gonkcfg.Actions{Features: true, Reviews: true},
		PipelineFix: gonkcfg.EffectivePipelineFix{
			AutoMerge: gonkcfg.EffectiveAutoMerge{Mode: "after-human-approval", Classes: []string{"docs"}, MaxPerDay: 2},
		},
		Change: gonkcfg.EffectiveChange{
			Triggers: []string{"label", "command"}, CreateClasses: []string{"source", "test"}, RequireDocs: "always",
			DocsLabels: []string{"api", "needs docs"}, RequireTestReport: true, MaxAttempts: 3, MaxOpen: 2,
			VerifyTimeout: 45 * time.Minute, MaxFiles: 4, MaxChangedLines: 120, Paths: []string{"internal/**"},
			AutoMerge: gonkcfg.EffectiveAutoMerge{Mode: "after-agentic-review", Classes: []string{"docs"}, MaxPerDay: 1},
		},
		Review: gonkcfg.EffectiveReview{
			Targets: []string{"gonk"}, Rung: "big", MaxNotes: 7, MaxReviewsPerMR: 2,
			PipelineFixMerge: gonkcfg.ReviewMergeStatus{Available: true},
			ChangeMerge:      gonkcfg.ReviewMergeStatus{Reason: "no-distinct-model"},
		},
	}
}

func TestEffectiveFromCarriesChangeAndReview(t *testing.T) {
	in := crEffective()
	got := EffectiveFrom(in)
	wantChange := Change{
		Triggers: []string{"label", "command"}, CreateClasses: []string{"source", "test"}, RequireDocs: "always",
		DocsLabels: []string{"api", "needs docs"}, RequireTestReport: true, MaxAttempts: 3, MaxOpen: 2,
		VerifyTimeoutSeconds: 2700, MaxFiles: 4, MaxChangedLines: 120, Paths: []string{"internal/**"},
		AutoMerge: AutoMerge{Mode: "after-agentic-review", Classes: []string{"docs"}, MaxPerDay: 1},
	}
	if !reflect.DeepEqual(got.Change, wantChange) {
		t.Errorf("Change = %+v\nwant     %+v", got.Change, wantChange)
	}
	wantReview := Review{
		Targets: []string{"gonk"}, Rung: "big", MaxNotes: 7, MaxReviewsPerMR: 2,
		AgenticMerge: ReviewMerge{PipelineFix: ReviewMergeStatus{Available: true}, Change: ReviewMergeStatus{Reason: "no-distinct-model"}},
	}
	if !reflect.DeepEqual(got.Review, wantReview) {
		t.Errorf("Review = %+v\nwant     %+v", got.Review, wantReview)
	}
	if !got.Actions.Reviews || !got.Actions.Features || got.PipelineFix.AutoMerge.MaxPerDay != 2 {
		t.Errorf("actions %+v / fix merges per day %d, want features and reviews true, 2", got.Actions, got.PipelineFix.AutoMerge.MaxPerDay)
	}
	// The wire copy does not alias the resolved config.
	got.Change.Triggers[0], got.Change.DocsLabels[0], got.Review.Targets[0] = "mention", "x", "human"
	if in.Change.Triggers[0] != "label" || in.Change.DocsLabels[0] != "api" || in.Review.Targets[0] != "gonk" {
		t.Error("EffectiveFrom aliased a resolved slice")
	}
}

// The reader's conversions give back exactly what meter resolved.
func TestChangeAndReviewConvertBackToEffective(t *testing.T) {
	{
		in := crEffective()
		w := EffectiveFrom(in)
		if got := w.Change.EffectiveChange(); !reflect.DeepEqual(got, in.Change) {
			t.Errorf("EffectiveChange() = %+v\nwant %+v", got, in.Change)
		}
		if got := w.Review.EffectiveReview(); !reflect.DeepEqual(got, in.Review) {
			t.Errorf("EffectiveReview() = %+v\nwant %+v", got, in.Review)
		}
		if got := w.PipelineFix.EffectivePipelineFix().AutoMerge.MaxPerDay; got != 2 {
			t.Errorf("EffectivePipelineFix().AutoMerge.MaxPerDay = %d, want 2", got)
		}
	}
	// The conversions do not alias the wire value.
	w := EffectiveFrom(crEffective())
	c, r := w.Change.EffectiveChange(), w.Review.EffectiveReview()
	c.Triggers[0], c.AutoMerge.Classes[0], r.Targets[0] = "x", "x", "x"
	if w.Change.Triggers[0] != "label" || w.Change.AutoMerge.Classes[0] != "docs" || w.Review.Targets[0] != "gonk" {
		t.Error("a reader conversion aliased the wire slices")
	}
}

// The action is OFF on the wire unless resolved on, and independent of the
// neighbouring actions.
func TestEffectiveFromReviewsIsItsOwnAction(t *testing.T) {
	for _, c := range []struct {
		in   gonkcfg.Actions
		want bool
	}{
		{gonkcfg.Actions{Triage: true, Pipelines: true, PipelineFixes: true, Features: true, Scaffold: true}, false},
		{gonkcfg.Actions{Features: true, Reviews: true}, true},
		{gonkcfg.Actions{}, false},
	} {
		if got := EffectiveFrom(gonkcfg.Effective{Actions: c.in}).Actions.Reviews; got != c.want {
			t.Errorf("%+v: wire reviews = %v, want %v", c.in, got, c.want)
		}
	}
}

// The literal key names, "none" as [] rather than null, and the fail-closed
// reading of a response from a meter that predates the fields.
func TestChangeAndReviewWireLiterals(t *testing.T) {
	raw, err := json.Marshal(EffectiveFrom(gonkcfg.Effective{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"reviews":false`,
		`"change":{"triggers":[],"create_classes":[],"require_docs":"","docs_labels":[],"require_test_report":false,"max_attempts":0,"max_open":0,` +
			`"verify_timeout_seconds":0,"max_files":0,"max_changed_lines":0,"paths":[],"auto_merge":{"mode":"","classes":[],"max_per_day":0}}`,
		`"review":{"targets":[],"rung":"","max_notes":0,"max_reviews_per_mr":0,` +
			`"agentic_merge":{"pipeline_fix":{"available":false,"reason":""},"change":{"available":false,"reason":""}}}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("wire JSON lacks %s\ngot %s", want, raw)
		}
	}
	// Owner decision: nothing on the wire can say "the same model may
	// authorise a merge".
	if strings.Contains(string(raw), "same_model") || strings.Contains(string(raw), "distinct") {
		t.Errorf("the wire carries a distinct-model switch: %s", raw)
	}
	// A response from a meter that predates the fields decodes to "nothing
	// allowed, agentic merge unavailable".
	var old Effective
	if err := json.Unmarshal([]byte(`{"enabled":true,"actions":{"triage":true,"pipelines":true,"pipeline_fixes":true,"features":true},`+
		`"pipeline_fix":{"auto_merge":{"mode":"after-agentic-review","classes":["docs"]}}}`), &old); err != nil {
		t.Fatal(err)
	}
	c, r := old.Change.EffectiveChange(), old.Review.EffectiveReview()
	if old.Actions.Reviews || len(c.Triggers) != 0 || c.AllowsTrigger("label") || len(c.CreateClasses) != 0 || c.MaxAttempts != 0 || c.MaxOpen != 0 ||
		c.AutoMerge.Mode != "" || c.AutoMerge.MaxPerDay != 0 || old.PipelineFix.AutoMerge.MaxPerDay != 0 {
		t.Errorf("an older meter's response decoded to something permissive: %+v / %+v", old.Actions, c)
	}
	if r.Rung != "" || r.AllowsTarget("gonk") || r.PipelineFixMerge.Available || r.ChangeMerge.Available {
		t.Errorf("an older meter's response decoded to a permissive review: %+v", r)
	}
	// ...and the agentic merge mode it DID state is not usable.
	mode, why := gonkcfg.UsableAutoMergeMode(old.Actions.PipelineFixes, old.PipelineFix.EffectivePipelineFix().AutoMerge, r.PipelineFixMerge)
	if mode != gonkcfg.AutoMergeOff || why == "" {
		t.Errorf("an older meter's after-agentic-review is usable: (%q, %q)", mode, why)
	}
}
