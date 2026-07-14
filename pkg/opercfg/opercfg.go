// Package opercfg is the contract for gonk's OPERATOR configuration: instance
// defaults, group overrides, the rung catalog, and gonk-meter's knobs. It lives
// in the private gonk-city repo and the chart's values, and it is mounted into
// gonk-meter as a single YAML document.
//
// Why this package exists: pkg/gonkcfg validates the project's .gonk.yml, but
// NOTHING validated the instance and group Policy values fed into
// gonkcfg.Resolve -- they bypassed the JSON Schema and the non-finite-float
// check (ADR-002, "Known gap"). Resolve fails closed on the one case that would
// otherwise be a silent budget escape (a NaN ceiling reading as "unlimited"),
// but the resolver should not be the only guard. Load is the missing gate:
// gonk-meter refuses to start on an operator config it cannot validate.
package opercfg

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// SchemaVersion is the operator-config schema major version this package speaks.
const SchemaVersion = 1

//go:embed gonk-operator.v1.schema.json
var schemaJSON []byte

var compiled = mustCompile()

func mustCompile() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("opercfg: embedded schema unreadable: %v", err))
	}
	var meta struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(schemaJSON, &meta); err != nil || meta.ID == "" {
		panic("opercfg: embedded schema has no $id")
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(meta.ID, doc); err != nil {
		panic(err)
	}
	s, err := c.Compile(meta.ID)
	if err != nil {
		panic(fmt.Sprintf("opercfg: embedded schema invalid: %v", err))
	}
	return s
}

// Kind classifies a rung. It is the difference between "costs tokens" and
// "costs money", which is the difference between a defer and a run.
type Kind string

const (
	KindLocal Kind = "local"
	KindCloud Kind = "cloud"
)

// RungSpec is one rung of the ladder: the LiteLLM model it dispatches to, and
// what one attempt at it is estimated to consume. The estimates are what a
// reservation reserves (ADR-004).
type RungSpec struct {
	Name  string `yaml:"name"`
	Kind  Kind   `yaml:"kind"`
	Model string `yaml:"model"`

	// EstCostUSD is REAL MONEY: what one attempt at this rung actually costs.
	// Cloud rungs: > 0, required. Local rungs: exactly 0 -- local inference is
	// free, and this is what makes `monthly_cost_usd: 0` mean "local rungs only,
	// forever" fall straight out of the arithmetic in rung.Decide.
	EstCostUSD float64 `yaml:"est_cost_usd"`

	// EstTokens is what one attempt is estimated to consume. REQUIRED ON EVERY
	// RUNG, local ones included: rung.Reserve reserves it, and a rung that
	// reserves no tokens holds no token budget.
	EstTokens gonkcfg.TokenQuantity `yaml:"est_tokens"`

	// SyntheticUSDPer1MTokens is the price we CONFIGURE IN LITELLM for a local
	// model, so that LiteLLM's USD virtual-key ceiling is a hard door for TOKENS
	// too (Decision 9). Without it a local rung costs $0, the dollar counter
	// never moves, and monthly_tokens has no hard enforcement anywhere.
	//
	// REQUIRED on local rungs (> 0). FORBIDDEN on cloud rungs: a cloud rung's
	// price is real and lives in LiteLLM's own model config, and declaring a
	// synthetic price for it would be a lie.
	//
	// The operator MUST configure the same number in LiteLLM's model list. That
	// agreement is unverifiable from here; Plan 06 checks it.
	SyntheticUSDPer1MTokens float64 `yaml:"synthetic_usd_per_1m_tokens"`
}

// PricePerToken is what LiteLLM's USD counter will be charged per token for this
// rung: the real price for a cloud rung (derived from its estimates), the
// synthetic price for a local one. It is what Service uses to convert a
// project's TOKEN ceiling into the USD max_budget it provisions on the virtual
// key (Decision 9).
//
// Both terms are operator-supplied estimates and both are approximations. That
// is acceptable: the resulting max_budget is a deliberately LOOSE backstop, not
// the primary control. Meter's reservation gate is the primary control.
func (r RungSpec) PricePerToken() float64 {
	if r.Kind == KindLocal {
		return r.SyntheticUSDPer1MTokens / 1e6
	}
	if r.EstTokens <= 0 {
		return 0 // unreachable: Load rejects a rung with est_tokens <= 0
	}
	return r.EstCostUSD / float64(r.EstTokens)
}

// MeterConfig holds gonk-meter's operational knobs, all with fail-closed
// defaults.
type MeterConfig struct {
	MaxSpendStaleness  time.Duration
	ReservationTTL     time.Duration
	MaxClockSkew       time.Duration
	KeyRetryBackoff    time.Duration
	MaxInfraRetries    int
	EnforceLadderOrder bool
}

// OperatorConfig is the validated operator layer.
type OperatorConfig struct {
	Instance gonkcfg.Policy
	Groups   map[string]gonkcfg.Policy
	Catalog  map[string]RungSpec
	Meter    MeterConfig
}

// rawConfig is the on-disk shape. Durations are strings in YAML ("5m") and
// time.Duration in the parsed struct, so the two shapes are separate types.
type rawConfig struct {
	Version  int                       `yaml:"version"`
	Instance gonkcfg.Policy            `yaml:"instance"`
	Groups   map[string]gonkcfg.Policy `yaml:"groups"`
	Rungs    []RungSpec                `yaml:"rungs"`
	Meter    struct {
		MaxSpendStaleness  *string `yaml:"max_spend_staleness"`
		ReservationTTL     *string `yaml:"reservation_ttl"`
		MaxClockSkew       *string `yaml:"max_clock_skew"`
		KeyRetryBackoff    *string `yaml:"key_retry_backoff"`
		MaxInfraRetries    *int    `yaml:"max_infra_retries"`
		EnforceLadderOrder *bool   `yaml:"enforce_ladder_order"`
	} `yaml:"meter"`
}

// Load validates raw operator-config bytes and decodes them. Every error is
// fatal: gonk-meter must not start on a config it cannot make sense of,
// because every budget decision it makes flows from this file.
func Load(raw []byte) (*OperatorConfig, error) {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("operator config: not valid YAML: %w", err)
	}
	// Same guard as gonkcfg.Validate: jsonschema/v6 v6.0.2 SIGSEGVs on a
	// non-finite float reaching a "minimum" keyword. Operator config is
	// trusted-ish, but a typo must not be a crash.
	if err := rejectNonFinite(doc); err != nil {
		return nil, fmt.Errorf("operator config: %w", err)
	}
	if err := compiled.Validate(doc); err != nil {
		return nil, fmt.Errorf("operator config: %w", err)
	}

	var rc rawConfig
	if err := yaml.Unmarshal(raw, &rc); err != nil {
		return nil, fmt.Errorf("operator config: %w", err)
	}

	oc := &OperatorConfig{
		Instance: rc.Instance,
		Groups:   rc.Groups,
		Catalog:  make(map[string]RungSpec, len(rc.Rungs)),
		Meter: MeterConfig{ // fail-closed defaults
			MaxSpendStaleness:  5 * time.Minute,
			ReservationTTL:     60 * time.Minute,
			MaxClockSkew:       5 * time.Minute,
			KeyRetryBackoff:    5 * time.Minute,
			MaxInfraRetries:    5,
			EnforceLadderOrder: true,
		},
	}

	// Parse the meter knobs FIRST: the checks below consult
	// Meter.EnforceLadderOrder, so it must already hold the operator's value
	// rather than the default.
	m := &oc.Meter
	for _, d := range []struct {
		raw *string
		dst *time.Duration
		key string
	}{
		{rc.Meter.MaxSpendStaleness, &m.MaxSpendStaleness, "max_spend_staleness"},
		{rc.Meter.ReservationTTL, &m.ReservationTTL, "reservation_ttl"},
		{rc.Meter.MaxClockSkew, &m.MaxClockSkew, "max_clock_skew"},
		{rc.Meter.KeyRetryBackoff, &m.KeyRetryBackoff, "key_retry_backoff"},
	} {
		if d.raw == nil {
			continue
		}
		v, err := time.ParseDuration(*d.raw)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("operator config: meter.%s: want a positive duration like \"5m\", got %q", d.key, *d.raw)
		}
		*d.dst = v
	}
	if rc.Meter.MaxInfraRetries != nil {
		m.MaxInfraRetries = *rc.Meter.MaxInfraRetries
	}
	if rc.Meter.EnforceLadderOrder != nil {
		m.EnforceLadderOrder = *rc.Meter.EnforceLadderOrder
	}

	// enforce_ladder_order only has teeth if there is an instance ladder to be
	// ordered against: CheckLadderOrder imposes nothing when the instance is
	// silent, so a project could still put a cloud rung first. Refuse to start
	// rather than pretend the guard is on. (This also discharges PLAN.md's
	// Plan-05 carry-forward: the chart's default values must ship a non-empty
	// instance ladder.)
	if m.EnforceLadderOrder && len(rc.Instance.Ladder) == 0 {
		return nil, fmt.Errorf("operator config: meter.enforce_ladder_order is on but instance.ladder is empty, so nothing constrains a project's rung order; set an instance ladder or turn the guard off deliberately")
	}

	for _, r := range rc.Rungs {
		if _, dup := oc.Catalog[r.Name]; dup {
			return nil, fmt.Errorf("operator config: rung %q defined twice", r.Name)
		}
		// EVERY rung must estimate its token consumption, local ones included.
		// Two reasons, both load-bearing:
		//   1. rung.Reserve reserves EstTokens. A rung estimating 0 tokens holds
		//      NO token budget, so N concurrent sessions can blow monthly_tokens
		//      and per_task_tokens freely. And under Decision 9 the token ceiling
		//      is converted into LiteLLM's USD key ceiling VIA est_tokens, so a
		//      rung with no token estimate gets NO HARD DOOR either -- it breaks
		//      both layers of the defence at once.
		//   2. Local rungs are cost-0 dollars but are metered in tokens for
		//      fairness visibility (spec 6.3). A local rung with no token
		//      estimate is invisible to exactly the accounting it exists for.
		if r.EstTokens <= 0 {
			return nil, fmt.Errorf("operator config: rung %q needs est_tokens > 0; a rung that reserves no tokens cannot be held against monthly_tokens or per_task_tokens, and LiteLLM does not enforce token budgets", r.Name)
		}
		switch r.Kind {
		case KindCloud:
			// rung.Decide applies the cost gate ONLY to rungs with a positive
			// REAL estimate. An unpriced cloud rung would sail past a $0 budget.
			if !(r.EstCostUSD > 0) {
				return nil, fmt.Errorf("operator config: cloud rung %q needs est_cost_usd > 0, else it bypasses the cost budget", r.Name)
			}
			// A cloud rung's price is REAL and lives in LiteLLM's model config.
			// A synthetic price for it would be a lie, and PricePerToken would
			// silently use the wrong one.
			if r.SyntheticUSDPer1MTokens != 0 {
				return nil, fmt.Errorf("operator config: cloud rung %q must not set synthetic_usd_per_1m_tokens; its price is real and comes from LiteLLM's model config", r.Name)
			}
		case KindLocal:
			if r.EstCostUSD != 0 {
				return nil, fmt.Errorf("operator config: local rung %q must have est_cost_usd 0 (local inference costs no real money; price it as a cloud rung if it does)", r.Name)
			}
			// DECISION 9. Without a synthetic price, a local rung costs LiteLLM's
			// USD counter nothing, the virtual-key ceiling never moves for it, and
			// monthly_tokens has NO HARD DOOR ANYWHERE. Meter's reservation would
			// be the only protection -- which is exactly the hole the synthetic
			// price exists to close. Refuse to start.
			if !(r.SyntheticUSDPer1MTokens > 0) {
				return nil, fmt.Errorf("operator config: local rung %q needs synthetic_usd_per_1m_tokens > 0; without a synthetic price LiteLLM's USD key ceiling never moves for this rung and monthly_tokens has no hard enforcement (see ADR-004, Decision 9)", r.Name)
			}
		}
		oc.Catalog[r.Name] = r
	}

	if err := oc.checkPolicy("instance", rc.Instance); err != nil {
		return nil, err
	}
	for g, p := range rc.Groups {
		if err := oc.checkPolicy("group "+g, p); err != nil {
			return nil, err
		}
		// A group ladder may narrow the instance's, but may not REORDER it --
		// the same rule the project ladder obeys, for the same reason (spec 6.3:
		// start at the cheapest rung).
		if oc.Meter.EnforceLadderOrder {
			if err := CheckLadderOrder(rc.Instance.Ladder, p.Ladder); err != nil {
				return nil, fmt.Errorf("operator config: group %s: %w", g, err)
			}
		}
	}

	return oc, nil
}

// checkPolicy enforces what the JSON Schema cannot: cross-references into the
// rung catalog, and timezone validity.
func (oc *OperatorConfig) checkPolicy(who string, p gonkcfg.Policy) error {
	for _, r := range p.Ladder {
		if _, ok := oc.Catalog[r]; !ok {
			return fmt.Errorf("operator config: %s ladder names rung %q, which is not in the rung catalog", who, r)
		}
	}
	if p.Schedule != nil && p.Schedule.Timezone != "" {
		if _, err := time.LoadLocation(p.Schedule.Timezone); err != nil {
			return fmt.Errorf("operator config: %s schedule.timezone %q: %w", who, p.Schedule.Timezone, err)
		}
	}
	return nil
}

// GroupFor returns the single group Policy that applies to a project path, by
// FOLDING every matching ancestor group coarsest-first.
//
// Returning only the longest-prefix match would be a budget escape: with groups
// "agentic" (ceiling $50) and "agentic/experiments" (which sets no budget),
// project agentic/experiments/spike would inherit NO group ceiling at all and be
// bounded only by the instance's $200. Spec 5.4 and ADR-002 say ceilings only
// tighten downward, so a nested group must never be able to LOOSEN its parent's.
//
// The fold uses exactly ADR-002's semantics, one layer at a time: budgets take
// the minimum, `enabled` and `actions` are vetoable (an explicit false anywhere
// in the chain wins), ladders intersect, and other scalars are most-specific-wins.
//
// Prefixes match on SEGMENT boundaries only: group "agentic" does not capture
// project "agentic-other/repo".
func (oc *OperatorConfig) GroupFor(project string) gonkcfg.Policy {
	var matches []string
	for g := range oc.Groups {
		if project == g || strings.HasPrefix(project, g+"/") {
			matches = append(matches, g)
		}
	}
	// Coarsest first, so the most specific layer is folded last.
	sort.Slice(matches, func(i, j int) bool { return len(matches[i]) < len(matches[j]) })

	var out gonkcfg.Policy
	for _, g := range matches {
		out = foldPolicy(out, oc.Groups[g])
	}
	return out
}

// foldPolicy folds `next` (more specific) onto `base` (coarser) using ADR-002's
// precedence rules. It exists because gonkcfg.Resolve takes exactly ONE group
// layer, and a nested group hierarchy has several.
func foldPolicy(base, next gonkcfg.Policy) gonkcfg.Policy {
	out := base

	// Vetoes: an explicit false at ANY level wins, so once false, stays false.
	if next.Enabled != nil {
		if base.Enabled == nil || *base.Enabled {
			out.Enabled = next.Enabled
		}
	}
	out.Actions = gonkcfg.ActionsPolicy{
		Triage:    foldVeto(base.Actions.Triage, next.Actions.Triage),
		Pipelines: foldVeto(base.Actions.Pipelines, next.Actions.Pipelines),
		Features:  foldVeto(base.Actions.Features, next.Actions.Features),
	}

	// Ceilings only tighten: minimum over the layers that set one.
	out.Budget = gonkcfg.BudgetPolicy{
		MonthlyCostUSD: minFloat(base.Budget.MonthlyCostUSD, next.Budget.MonthlyCostUSD),
		MonthlyTokens:  minTokens(base.Budget.MonthlyTokens, next.Budget.MonthlyTokens),
		PerTaskTokens:  minTokens(base.Budget.PerTaskTokens, next.Budget.PerTaskTokens),
	}

	// Ladder: the more specific list is the base ordering, narrowed by the
	// coarser allow-list. Same rule Resolve applies.
	switch {
	case next.Ladder != nil && base.Ladder != nil:
		allowed := make(map[string]struct{}, len(base.Ladder))
		for _, r := range base.Ladder {
			allowed[r] = struct{}{}
		}
		// NOT `var l []string`. An empty intersection must be a NON-NIL
		// zero-length slice, because Resolve's allow-list fold is gated on
		// `Ladder != nil` -- ADR-002 says so in as many words ("a naive
		// `Ladder == nil` check would miss the non-nil zero-length slice an
		// empty intersection produces"). A nil here means "this layer is
		// silent", so a group hierarchy that allows NO rung would be read as
		// imposing no constraint, and the project's cloud rung would run.
		l := next.Ladder[:0:0]
		for _, r := range next.Ladder {
			if _, ok := allowed[r]; ok {
				l = append(l, r)
			}
		}
		out.Ladder = l
	case next.Ladder != nil:
		out.Ladder = append([]string(nil), next.Ladder...)
	}

	// Most-specific-wins scalars.
	if next.Schedule != nil {
		sc := *next.Schedule
		out.Schedule = &sc
	}
	if next.Continuity != nil {
		out.Continuity = next.Continuity
	}
	if next.Triage.LabelPrefix != nil {
		out.Triage.LabelPrefix = next.Triage.LabelPrefix
	}
	if next.Triage.RespondToMentions != nil {
		out.Triage.RespondToMentions = next.Triage.RespondToMentions
	}
	if next.Provenance.CommitTrailers != nil {
		out.Provenance.CommitTrailers = next.Provenance.CommitTrailers
	}
	if next.Provenance.IncludeUsage != nil {
		out.Provenance.IncludeUsage = next.Provenance.IncludeUsage
	}
	return out
}

// foldVeto: nil means "silent". An explicit false anywhere in the chain sticks.
func foldVeto(base, next *bool) *bool {
	if base != nil && !*base {
		return base
	}
	if next != nil {
		return next
	}
	return base
}

func minFloat(a, b *float64) *float64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b < *a:
		return b
	}
	return a
}

func minTokens(a, b *gonkcfg.TokenQuantity) *gonkcfg.TokenQuantity {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b < *a:
		return b
	}
	return a
}

// CheckLadderOrder reports whether a project's ladder respects the instance's
// escalation ordering. gonkcfg.Resolve preserves the PROJECT's order when it
// intersects (ADR-002), so without this a project could list [sonnet,
// qwen-local] and make a cloud rung its first attempt -- while spec 6.3 says
// "everything starts at the cheapest rung its project config allows". A
// project's ladder must be a SUBSEQUENCE of the instance's. If the instance
// sets no ladder, it imposes no ordering.
func CheckLadderOrder(instance, project []string) error {
	if len(instance) == 0 || len(project) == 0 {
		return nil
	}
	pos := make(map[string]int, len(instance))
	for i, r := range instance {
		pos[r] = i
	}
	last := -1
	for _, r := range project {
		i, ok := pos[r]
		if !ok {
			continue // Resolve's intersection will drop it anyway
		}
		if i < last {
			return fmt.Errorf("ladder order: %v reorders the instance ladder %v; rungs must escalate cheapest-first (spec 6.3)", project, instance)
		}
		last = i
	}
	return nil
}

func rejectNonFinite(v any) error {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return fmt.Errorf("not a finite number")
		}
	case map[string]any:
		for _, val := range t {
			if err := rejectNonFinite(val); err != nil {
				return err
			}
		}
	case map[any]any:
		for _, val := range t {
			if err := rejectNonFinite(val); err != nil {
				return err
			}
		}
	case []any:
		for _, val := range t {
			if err := rejectNonFinite(val); err != nil {
				return err
			}
		}
	}
	return nil
}
