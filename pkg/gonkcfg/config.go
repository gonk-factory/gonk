package gonkcfg

import "gopkg.in/yaml.v3"

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
}

type ActionsPolicy struct {
	Triage    *bool `yaml:"triage"`
	Pipelines *bool `yaml:"pipelines"`
	Features  *bool `yaml:"features"`
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
	return &pc, nil
}
