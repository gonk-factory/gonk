package gonkcfg

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// Policy is one layer of gonk configuration. All fields are optional
// (pointers/nil slices); Resolve folds layers into an Effective config.
// The same shape serves instance defaults, group overrides, and the
// project's .gonk.yml.
type Policy struct {
	Enabled    *bool            `yaml:"enabled"`
	Actions    ActionsPolicy    `yaml:"actions"`
	Schedule   *Schedule        `yaml:"schedule"`
	Budget     BudgetPolicy     `yaml:"budget"`
	Ladder     []string         `yaml:"ladder"`
	Continuity *string          `yaml:"continuity"`
	Triage     TriagePolicy     `yaml:"triage"`
	Provenance ProvenancePolicy `yaml:"provenance"`
	// Scope is this layer's workflow scope (gonk-066): which path classes a
	// code-writing workflow may touch, its size caps, and extra protected or
	// reclassified paths. It is NOT folded by Resolve and is not part of
	// Effective: pkg/scope.Fold combines it with the agent's pack row, per
	// dispatch, and no layer can widen past that row. The keys each layer may
	// write differ (a project may not set source_paths or deny_lift); the two
	// JSON Schemas and scope.Policy.Validate enforce that. Reference:
	// docs/reference/gonk-scope.md.
	Scope scope.Policy `yaml:"scope"`
	// PipelineFix is this layer's `pipeline_fix:` section (gonk-2usw): how a
	// pipeline-fix merge request may be made once `actions.pipeline_fixes`
	// allows one at all. Resolve folds it into Effective.PipelineFix. A project
	// may write every key; an operator layer may write every key but `paths`.
	// Reference: docs/reference/gonk-pipeline-fix.md.
	PipelineFix PipelineFixPolicy `yaml:"pipeline_fix"`
	// Change is this layer's `change:` section (gonk-cuwn / gonk-3so): how an
	// issue may be turned into a merge request once `actions.features` allows
	// it at all. Resolve folds it into Effective.Change. A project may write
	// every key; an operator layer may write every key but `paths`.
	// Reference: docs/reference/gonk-change.md.
	Change ChangePolicy `yaml:"change"`
	// Review is this layer's `review:` section (gonk-cuwn / gonk-td7): how an
	// independent review of a gonk-authored merge request is bounded once
	// `actions.reviews` allows one at all. Resolve folds it into
	// Effective.Review. `rung` is OPERATOR ONLY. Reference:
	// docs/reference/gonk-review.md.
	Review ReviewPolicy `yaml:"review"`
}

type ActionsPolicy struct {
	Triage    *bool `yaml:"triage"`
	Pipelines *bool `yaml:"pipelines"`
	// PipelineFixes: a diagnosis may be followed by a fix merge request
	// (gonk-2usw). OFF unless a project writes `pipeline_fixes: true`, and a
	// project that does must also write `pipelines: true` (CheckPipelineFix).
	PipelineFixes *bool `yaml:"pipeline_fixes"`
	Features      *bool `yaml:"features"`
	// Scaffold authorizes the metered `.agent/` scaffold session (spec 5.3).
	// It is OFF unless a project writes `scaffold: true`, because the
	// onboarding merge request now ships a deterministic `.agent/` seed and
	// the metered scaffold is an opt-in extra rather than a step every project
	// must pay for.
	Scaffold *bool `yaml:"scaffold"`
	// Reviews: a merge request gonk authored may be read by an independent
	// review session (gonk-cuwn / gonk-td7). OFF unless a project writes
	// `reviews: true`, and a project that does must also switch on a workflow
	// that authors merge requests: `pipeline_fixes` or `features`
	// (CheckReview).
	Reviews *bool `yaml:"reviews"`
}

type Schedule struct {
	QuietHours string `yaml:"quiet_hours"`
	Timezone   string `yaml:"timezone"`
}

type BudgetPolicy struct {
	MonthlyCostUSD *float64       `yaml:"monthly_cost_usd"`
	MonthlyTokens  *TokenQuantity `yaml:"monthly_tokens"`
	PerTaskTokens  *TokenQuantity `yaml:"per_task_tokens"`
}

type TriagePolicy struct {
	LabelPrefix       *string `yaml:"label_prefix"`
	RespondToMentions *bool   `yaml:"respond_to_mentions"`
}

// ProvenancePolicy is accepted and validated for backward compatibility with
// existing .gonk.yml files, but UNIMPLEMENTED: nothing in gonk reads these
// fields. The commit-trailer writer was removed before v1 shipped (the
// install guard it depended on could never fire, since pkg/rig hands a pod a
// GitLab archive tarball rather than a git clone, so RIG_DIR/.git never
// exists), and no onboarding or documentation surface may promise the
// behaviour these fields describe (gonk-92jq). Keep this comment and the
// matching "description" in gonk-config.v1.schema.json in sync with reality
// if a reader is ever wired back up.
type ProvenancePolicy struct {
	CommitTrailers *bool `yaml:"commit_trailers"`
	IncludeUsage   *bool `yaml:"include_usage"`
}

// ProjectConfig is a parsed, schema-valid .gonk.yml.
type ProjectConfig struct {
	Version int `yaml:"version"`
	Policy  `yaml:",inline"`
}

// Load validates raw .gonk.yml bytes against the schema, then decodes them.
func Load(raw []byte) (*ProjectConfig, error) {
	if err := Validate(raw); err != nil {
		return nil, err
	}
	var pc ProjectConfig
	if err := yaml.Unmarshal(raw, &pc); err != nil {
		return nil, err
	}
	// The schema cannot express the reclassification lattice (a `docs_paths`
	// pattern aimed at deploy/ is a well-formed list of strings). Checking it
	// HERE is what makes a bad scope fail at dispatch -- the meter marks the
	// project invalid and denies -- instead of at judge time, where it would
	// look like the agent's failure (gonk-066 section 3.4, invariant 5).
	if err := pc.Scope.Validate(scope.LayerProject); err != nil {
		return nil, fmt.Errorf(".gonk.yml: %w", err)
	}
	// The pipeline-fix opt-in (gonk-2usw 3.1). The schema says all of this for
	// a document; it is repeated here so that the rule does not rest on one
	// keyword of one JSON file.
	if err := CheckPipelineFix(pc.Policy); err != nil {
		return nil, fmt.Errorf(".gonk.yml: %w", err)
	}
	// The issue-change and review opt-ins (gonk-cuwn). As above: the schema
	// says all of this for a document, and it is repeated in Go.
	if err := CheckChange(pc.Policy); err != nil {
		return nil, fmt.Errorf(".gonk.yml: %w", err)
	}
	if err := CheckReview(pc.Policy); err != nil {
		return nil, fmt.Errorf(".gonk.yml: %w", err)
	}
	return &pc, nil
}
