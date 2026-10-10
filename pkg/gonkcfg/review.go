package gonkcfg

import (
	"fmt"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// Independent review's configuration (gonk-cuwn / gonk-td7, plan
// docs/plans/2026-10-05-gonk-cuwn-epic-remainder.md section 3.1 and the owner
// decisions of 8.1): `actions.reviews` and the `review:` section. Everything is
// OFF unless a project writes `actions.reviews: true`.
//
// NOTHING DISPATCHES A REVIEW YET. This file is configuration only.
//
// Reference: docs/reference/gonk-review.md.

// ReviewKey is the configuration key the section sits under.
const ReviewKey = "review"

// ReviewTargetGonk: merge requests gonk itself authored. It is the ONLY target
// in v1 (owner decision Q5): `human` is not a value any layer may write.
const ReviewTargetGonk = "gonk"

// Defaults and ceilings (plan 3.3: at most 20 notes; 3.4: at most 3 reviews of
// one merge request). The plan states one number for each, so the default is
// the ceiling: a layer can lower it and nothing can raise it.
const (
	DefaultReviewMaxNotes        = 20
	ReviewMaxNotesCeiling        = 20
	DefaultReviewMaxReviewsPerMR = 3
	ReviewMaxReviewsPerMRCeiling = 3
)

// The author workflows a review can follow, as UsableAutoMergeMode and
// rung.ResolveReviewMerge name them.
const (
	WorkflowPipelineFix = "pipeline-fix"
	WorkflowChange      = "change"
)

// Why `auto_merge.mode: after-agentic-review` cannot be satisfied for a
// workflow (ReviewMergeStatus.Reason). Named so that the mode is never
// silently satisfied and never silently ignored (owner decision Q4).
const (
	// ReviewMergeReviewsOff: `actions.reviews` did not resolve to true.
	ReviewMergeReviewsOff = "reviews-off"
	// ReviewMergeNoRung: no operator layer names `review.rung`, or two layers
	// name different ones. No model has been declared adequate for review.
	ReviewMergeNoRung = "no-review-rung"
	// ReviewMergeModelsNotCompared: gonkcfg.Resolve alone cannot see the rung
	// catalog. rung.ResolveReviewMerge replaces this; until it has run the
	// mode is unavailable.
	ReviewMergeModelsNotCompared = "models-not-compared"
	// ReviewMergeRungUnknown: `review.rung` is not in the rung catalog.
	ReviewMergeRungUnknown = "review-rung-not-in-catalog"
	// ReviewMergeNoAuthorRung: the project's ladder has no rung the author
	// workflow could run on.
	ReviewMergeNoAuthorRung = "no-author-rung"
	// ReviewMergeNoDistinctModel: every rung the author workflow can run on
	// uses the reviewer's model. There is no setting that waives this.
	ReviewMergeNoDistinctModel = "no-distinct-model"
)

var reviewTargetOrder = []string{ReviewTargetGonk}

// ReviewPolicy is ONE layer's `review:` section. nil means "this layer says
// nothing".
type ReviewPolicy struct {
	// Targets: which merge requests are reviewed. Only `gonk` exists.
	Targets []string `yaml:"targets"`
	// Rung names the ONE rung of the operator's catalog a review runs on: the
	// operator's statement of which model is adequate for review. OPERATOR
	// ONLY: a project may not choose its reviewer.
	Rung *string `yaml:"rung"`
	// MaxNotes caps findings in one review; MaxReviewsPerMR caps reviews of
	// one merge request (one per head commit).
	MaxNotes        *int64 `yaml:"max_notes"`
	MaxReviewsPerMR *int64 `yaml:"max_reviews_per_mr"`
}

// IsZero reports whether the layer says nothing at all.
func (p ReviewPolicy) IsZero() bool {
	return p.Targets == nil && p.Rung == nil && p.MaxNotes == nil && p.MaxReviewsPerMR == nil
}

func (p ReviewPolicy) ints() []intBound {
	return []intBound{
		{"max_notes", p.MaxNotes, ReviewMaxNotesCeiling},
		{"max_reviews_per_mr", p.MaxReviewsPerMR, ReviewMaxReviewsPerMRCeiling},
	}
}

// Validate checks one layer. It refuses an unknown target (so `human`), a
// number below 1 or above its ceiling, a malformed rung name, and `rung` from
// a project layer. Whether the rung exists in the catalog is checked where the
// catalog is: pkg/opercfg.
func (p ReviewPolicy) Validate(layer scope.Layer) error {
	if layer != scope.LayerOperator && layer != scope.LayerProject {
		return fmt.Errorf("review: unknown layer %q", layer)
	}
	for _, v := range p.Targets {
		if !contains(reviewTargetOrder, v) {
			return fmt.Errorf("review: targets: %q is not one of %v", v, reviewTargetOrder)
		}
	}
	if p.Rung != nil && layer == scope.LayerProject {
		return fmt.Errorf("review: rung is operator-only; which model reviews is the operator's decision and a project may not set it")
	}
	if p.Rung != nil && !reviewRungRe.MatchString(*p.Rung) {
		return fmt.Errorf("review: rung: %q is not a rung name", *p.Rung)
	}
	for _, n := range p.ints() {
		if n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {
			return fmt.Errorf("review: %s is %d; it must be between 1 and %d (the ceiling can only be tightened)", n.key, *n.v, n.ceiling)
		}
	}
	return nil
}

// CheckReview is the project-layer check Load runs after the schema: the
// `review:` section validates, and `actions.reviews: true` is written together
// with a workflow whose merge requests can be reviewed.
//
// v1 reviews gonk-authored merge requests only, and only two workflows author
// one: pipeline fixes (`actions.pipeline_fixes`) and issue changes
// (`actions.features`). Reviews with neither is a configuration that can never
// do anything -- refused at registration rather than left to look like "gonk
// is not reviewing".
func CheckReview(p Policy) error {
	if on(p.Actions.Reviews) && !on(p.Actions.PipelineFixes) && !on(p.Actions.Features) {
		return fmt.Errorf("actions.reviews: true requires actions.pipeline_fixes: true or actions.features: true (a review follows a merge request gonk opened; with neither there is none)")
	}
	return p.Review.Validate(scope.LayerProject)
}

// ReviewMergeStatus says whether `auto_merge.mode: after-agentic-review` can be
// satisfied for one author workflow under the resolved configuration. The ZERO
// value is "unavailable": a reader given no status must not merge.
//
// Available is about CONFIGURATION only. Whether one particular merge request
// may merge is still decided per merge request, on the recorded models of its
// author and its reviewer (plan 5.1 M9).
type ReviewMergeStatus struct {
	Available bool
	// Reason is one of the ReviewMerge* constants when Available is false, and
	// "" when it is true.
	Reason string
}

// THERE IS NO `require_distinct_model` KEY (owner decision, 2026-10-07): the
// same model can never satisfy the agentic-review merge mode, and no project
// or operator setting turns that off. A review itself may run on the author's
// model and says so; it then authorises no merge.
//
// EffectiveReview is the folded `review:` section. WHETHER a review may run at
// all is Effective.Actions.Reviews AND a non-empty Rung; these values are
// populated (with the defaults) for a project that never opted in.
type EffectiveReview struct {
	Targets []string // never nil
	// Rung is the operator's reviewer rung, "" when no operator layer names
	// one or two layers disagree. With "" no review can run.
	Rung            string
	MaxNotes        int
	MaxReviewsPerMR int
	// PipelineFixMerge and ChangeMerge: whether after-agentic-review can be
	// satisfied for that workflow's merge requests. gonkcfg.Resolve fills in
	// what the configuration alone decides; rung.ResolveReviewMerge completes
	// them against the rung catalog.
	PipelineFixMerge ReviewMergeStatus
	ChangeMerge      ReviewMergeStatus
}

// AllowsTarget reports whether merge requests of this kind are reviewed.
func (e EffectiveReview) AllowsTarget(target string) bool { return contains(e.Targets, target) }

// MergeStatus returns the status for a workflow (WorkflowPipelineFix,
// WorkflowChange); an unknown workflow is unavailable.
func (e EffectiveReview) MergeStatus(workflow string) ReviewMergeStatus {
	switch workflow {
	case WorkflowPipelineFix:
		return e.PipelineFixMerge
	case WorkflowChange:
		return e.ChangeMerge
	}
	return ReviewMergeStatus{}
}

// FoldReview folds `next` onto `base`, two OPERATOR layers: targets intersect,
// numbers take the minimum, and two
// layers naming DIFFERENT rungs fold to the empty rung -- no reviewer -- rather
// than letting the more specific layer swap the reviewer for another model.
func FoldReview(base, next ReviewPolicy) ReviewPolicy {
	return ReviewPolicy{
		Targets:         intersectLists(base.Targets, next.Targets),
		Rung:            foldRung(base.Rung, next.Rung),
		MaxNotes:        minInt(base.MaxNotes, next.MaxNotes),
		MaxReviewsPerMR: minInt(base.MaxReviewsPerMR, next.MaxReviewsPerMR),
	}
}

// foldRung: two layers naming the same rung keep it; naming different rungs
// folds to "" (none).
func foldRung(a, b *string) *string {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a == *b:
		return a
	}
	none := ""
	return &none
}

// ResolveReview folds the operator layers and the project's section into the
// effective one. The rung is the OPERATOR'S alone: a project value is ignored
// here (and refused by Validate).
//
// The merge statuses are left zero (unavailable); Resolve sets them.
func ResolveReview(instance, group, project ReviewPolicy) EffectiveReview {
	op := FoldReview(instance, group)
	eff := EffectiveReview{
		Targets:         resolveList(reviewTargetOrder, defaultReviewTargetsList, project.Targets, op.Targets),
		MaxNotes:        resolveNum(DefaultReviewMaxNotes, ReviewMaxNotesCeiling, project.MaxNotes, op.MaxNotes),
		MaxReviewsPerMR: resolveNum(DefaultReviewMaxReviewsPerMR, ReviewMaxReviewsPerMRCeiling, project.MaxReviewsPerMR, op.MaxReviewsPerMR),
	}
	if op.Rung != nil && reviewRungRe.MatchString(*op.Rung) {
		eff.Rung = *op.Rung
	}
	return eff
}

// reviewMergeStatus is the part of a ReviewMergeStatus the configuration alone
// decides. It never answers "available": comparing models needs the rung
// catalog, which is rung.ResolveReviewMerge's.
func reviewMergeStatus(reviewsOn bool, r EffectiveReview) ReviewMergeStatus {
	switch {
	case !reviewsOn:
		return ReviewMergeStatus{Reason: ReviewMergeReviewsOff}
	case r.Rung == "":
		return ReviewMergeStatus{Reason: ReviewMergeNoRung}
	}
	return ReviewMergeStatus{Reason: ReviewMergeModelsNotCompared}
}

// UsableAutoMergeMode is the merge mode that may actually be acted on for one
// workflow: the resolved `auto_merge.mode`, or AutoMergeOff with the reason
// when it cannot be used.
//
//   - actionOn false (the workflow itself is off): off, "action-off".
//   - mode after-agentic-review while the workflow's ReviewMergeStatus is not
//     available: off, and the status's reason. This is the owner's rule that
//     the mode is UNAVAILABLE, never silently satisfied, when no distinct
//     adequate reviewer model is configured.
//   - a mode this build does not know: off, "unknown-mode".
//
// A consumer (the merge policy) must use this and not read Mode directly.
func UsableAutoMergeMode(actionOn bool, am EffectiveAutoMerge, st ReviewMergeStatus) (mode, reason string) {
	switch {
	case am.Mode == AutoMergeOff:
		return AutoMergeOff, ""
	case !autoMergeModes[am.Mode]:
		return AutoMergeOff, "unknown-mode"
	case !actionOn:
		return AutoMergeOff, "action-off"
	case am.Mode == AutoMergeAfterAgenticReview && !st.Available:
		if st.Reason == "" {
			return AutoMergeOff, ReviewMergeModelsNotCompared
		}
		return AutoMergeOff, st.Reason
	}
	return am.Mode, ""
}
