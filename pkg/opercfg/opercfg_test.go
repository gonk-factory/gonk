package opercfg

import (
	"strings"
	"testing"
	"time"

	_ "time/tzdata" // checkPolicy calls time.LoadLocation; CI images have no zoneinfo
)

const validOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local, glm, sonnet]
  budget: { monthly_cost_usd: 200, monthly_tokens: "5G" }
groups:
  agentic:
    budget: { monthly_cost_usd: 50 }
  agentic/experiments:
    enabled: false
rungs:
  - { name: qwen-local, kind: local,  model: qwen3-coder-30b, est_cost_usd: 0,    est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.20 }
  - { name: glm,        kind: cloud,  model: glm-5,           est_cost_usd: 0.40, est_tokens: "200K" }
  - { name: sonnet,     kind: cloud,  model: claude-sonnet,   est_cost_usd: 1.20, est_tokens: "200K" }
meter:
  max_spend_staleness: 5m
  reservation_ttl: 60m
  max_infra_retries: 5
`

func TestLoadOperatorConfig(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	// Assert NON-DEFAULT values, so a Load that returned a zero struct fails.
	if oc.Instance.Budget.MonthlyCostUSD == nil || *oc.Instance.Budget.MonthlyCostUSD != 200 {
		t.Fatalf("instance cost ceiling: %+v", oc.Instance.Budget)
	}
	if oc.Instance.Budget.MonthlyTokens == nil || *oc.Instance.Budget.MonthlyTokens != 5_000_000_000 {
		t.Fatalf("instance token ceiling: %+v", oc.Instance.Budget)
	}
	if got := oc.Instance.Ladder; len(got) != 3 || got[2] != "sonnet" {
		t.Fatalf("instance ladder: %v", got)
	}
	glm, ok := oc.Catalog["glm"]
	if !ok || glm.Kind != KindCloud || glm.Model != "glm-5" || glm.EstCostUSD != 0.40 || glm.EstTokens != 200_000 {
		t.Fatalf("catalog[glm] = %+v", glm)
	}
	if oc.Meter.MaxSpendStaleness != 5*time.Minute || oc.Meter.ReservationTTL != 60*time.Minute {
		t.Fatalf("meter knobs: %+v", oc.Meter)
	}
	// Defaults applied where silent.
	if oc.Meter.MaxClockSkew != 5*time.Minute || !oc.Meter.EnforceLadderOrder {
		t.Fatalf("meter defaults: %+v", oc.Meter)
	}
}

// GroupFor must FOLD every matching ancestor, not just return the longest
// prefix. Returning only the deepest match would silently drop an ancestor's
// ceiling -- a budget escape, and a direct contradiction of ADR-002's
// "ceilings only tighten downward".
func TestGroupFor(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	for project, wantEnabledSet := range map[string]bool{
		"agentic/gonk-project":      false, // agentic sets budget only, not enabled
		"agentic/experiments/spike": true,  // agentic/experiments sets enabled: false
		"unrelated/thing":           false, // no group matches -> zero Policy
	} {
		p := oc.GroupFor(project)
		if (p.Enabled != nil) != wantEnabledSet {
			t.Errorf("GroupFor(%q).Enabled set = %v, want %v", project, p.Enabled != nil, wantEnabledSet)
		}
	}
	if p := oc.GroupFor("agentic/gonk-project"); p.Budget.MonthlyCostUSD == nil || *p.Budget.MonthlyCostUSD != 50 {
		t.Errorf("GroupFor(agentic/...) lost the group budget: %+v", p.Budget)
	}
	// THE money case: the nested group sets no budget, so the PARENT's $50
	// ceiling must survive the fold. A longest-prefix-only GroupFor returns the
	// experiments policy alone and this project silently gets the instance's $200.
	p := oc.GroupFor("agentic/experiments/spike")
	if p.Budget.MonthlyCostUSD == nil || *p.Budget.MonthlyCostUSD != 50 {
		t.Errorf("a nested group dropped its ancestor's ceiling: %+v", p.Budget)
	}
	if p.Enabled == nil || *p.Enabled {
		t.Errorf("the nested group's enabled:false was lost in the fold: %+v", p.Enabled)
	}
	// A prefix must match on a path SEGMENT boundary, not a string prefix:
	// group "agentic" must not capture project "agentic-other/repo".
	if p := oc.GroupFor("agentic-other/repo"); p.Budget.MonthlyCostUSD != nil {
		t.Error("group prefix matched across a segment boundary")
	}
}

// A nested group may TIGHTEN its parent's ceiling and may never LOOSEN it.
func TestGroupForCeilingsOnlyTighten(t *testing.T) {
	oc, err := Load([]byte(`
version: 1
instance: { ladder: [a, b] }
groups:
  top:            { budget: { monthly_cost_usd: 50, monthly_tokens: "1G" } }
  top/tighter:    { budget: { monthly_cost_usd: 5 } }
  top/looser:     { budget: { monthly_cost_usd: 500, monthly_tokens: "9G" } }
rungs:
  - { name: a, kind: local, model: m, est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.2 }
  - { name: b, kind: cloud, model: n, est_cost_usd: 0.4, est_tokens: "200K" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := oc.GroupFor("top/tighter/repo").Budget.MonthlyCostUSD; got == nil || *got != 5 {
		t.Errorf("a tightening nested group = %v, want 5", got)
	}
	// A group chain whose ladders do not overlap allows NOTHING, and must hand
	// Resolve a non-nil empty slice to say so. A nil would mean "this layer is
	// silent" (ADR-002), Resolve would skip the group allow-list entirely, and
	// the project would run a rung the group hierarchy forbade.
	oc2, err := Load([]byte(`
version: 1
instance: { ladder: [a, b] }
groups:
  top:     { ladder: [b] }
  top/sub: { ladder: [a] }
rungs:
  - { name: a, kind: local, model: m, est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.2 }
  - { name: b, kind: cloud, model: n, est_cost_usd: 0.4, est_tokens: "200K" }
`))
	if err != nil {
		t.Fatal(err)
	}
	l := oc2.GroupFor("top/sub/repo").Ladder
	if l == nil {
		t.Fatal("a group chain that allows no rung produced a NIL ladder; Resolve reads that as 'no constraint' and the project escapes the group allow-list")
	}
	if len(l) != 0 {
		t.Fatalf("group ladders [b] and [a] do not intersect; got %v", l)
	}
	loose := oc.GroupFor("top/looser/repo").Budget
	if loose.MonthlyCostUSD == nil || *loose.MonthlyCostUSD != 50 {
		t.Errorf("a nested group LOOSENED its parent's cost ceiling to %v; ceilings only tighten (ADR-002)", loose.MonthlyCostUSD)
	}
	if loose.MonthlyTokens == nil || *loose.MonthlyTokens != 1_000_000_000 {
		t.Errorf("a nested group LOOSENED its parent's token ceiling to %v", loose.MonthlyTokens)
	}
}

// This is the whole point of the package: the operator layers must not be able
// to smuggle in what .gonk.yml cannot.
//
// NOTE: every fixture below must be otherwise-VALID, or it is rejected for the
// wrong reason and the case proves nothing. `base` is the minimum valid
// document; each case perturbs exactly one thing. TestLoadRejectsBaseIsValid
// guards that.
func base(extra string) string {
	return "version: 1\n" +
		"instance: { ladder: [a] }\n" +
		"rungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\n" + extra
}

func TestLoadRejectsBaseIsValid(t *testing.T) {
	if _, err := Load([]byte(base(""))); err != nil {
		t.Fatalf("the reject-table's base document is itself invalid (%v); every case below would pass vacuously", err)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"NaN cost ceiling":             "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], budget: { monthly_cost_usd: .nan } }",
		"Inf cost ceiling":             "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], budget: { monthly_cost_usd: .inf } }",
		"negative cost ceiling":        "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], budget: { monthly_cost_usd: -1 } }",
		"fractional tokens":            "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], budget: { monthly_tokens: 1.5 } }",
		"unknown key":                  "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], bananas: true }",
		"empty ladder":                 "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [] }",
		"duplicate rung in ladder":     "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a, a] }",
		"no rungs":                     "version: 1\nrungs: []",
		"ladder names an unknown rung": "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [ghost] }",
		"duplicate catalog entry":      "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}, {name: a, kind: cloud, model: n, est_cost_usd: 1, est_tokens: \"200K\"}]",
		"cloud rung with no price":     "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: cloud, model: m, est_tokens: \"200K\"}]",
		"local rung with a REAL price": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_cost_usd: 0.5, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]",
		// DECISION 9. A local rung with no SYNTHETIC price costs LiteLLM's USD
		// counter nothing, so the virtual-key ceiling never moves for it and
		// monthly_tokens has no hard door anywhere. This is the hole the synthetic
		// price exists to close; refusing to start is how it stays closed.
		"local rung with no SYNTHETIC price": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\"}]",
		// ...and the mirror: a cloud rung's price is real and lives in LiteLLM's
		// model config. Declaring a synthetic one for it is a lie that
		// PricePerToken would silently believe.
		"cloud rung with a synthetic price": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: cloud, model: m, est_cost_usd: 0.4, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]",
		// A rung that reserves no tokens holds no token budget. With Decision 9
		// the token reservation is what the whole hard door is built on, so this
		// is a real hole, not a schema formality.
		"rung with no est_tokens":   "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, synthetic_usd_per_1m_tokens: 0.2}]",
		"rung with zero est_tokens": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: 0, synthetic_usd_per_1m_tokens: 0.2}]",
		// enforce_ladder_order defaults on, and it is toothless with no instance
		// ladder to order against.
		"ladder order enforced with no instance ladder": "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]",
		"group ladder reorders the instance ladder": "version: 1\n" +
			"instance: { ladder: [a, b] }\n" +
			"groups: { g: { ladder: [b, a] } }\n" +
			"rungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}, {name: b, kind: cloud, model: n, est_cost_usd: 0.4, est_tokens: \"200K\"}]",
		"bad duration":     "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\nmeter: { reservation_ttl: soon }",
		"unknown timezone": "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], schedule: { quiet_hours: \"22:00-07:00\", timezone: Mars/Olympus } }",
		"wrong version":    "version: 2\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]",
		"not yaml":         "{{{{",
		// max_infra_retries: 0 denies a FRESH bead (trailing_infra_failures 0 >=
		// max_infra_retries 0) with a misleading infra-retries-exhausted reason;
		// every decision on the instance bricks. 1 is the minimum sane value.
		"zero max_infra_retries":     base("meter: { max_infra_retries: 0 }"),
		"negative max_infra_retries": base("meter: { max_infra_retries: -1 }"),
	}
	for name, doc := range cases {
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("%s: Load accepted %q, want error", name, strings.TrimSpace(doc))
		}
	}
}

// max_infra_retries defaults to 5 when unset (the rung policy needs a sane
// deny-after-N floor even when the operator says nothing). 0 or negative bricks
// the instance and is rejected; see TestLoadRejects.
func TestMaxInfraRetriesDefault(t *testing.T) {
	oc, err := Load([]byte(base("")))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if oc.Meter.MaxInfraRetries != 5 {
		t.Fatalf("unset max_infra_retries default = %d, want 5", oc.Meter.MaxInfraRetries)
	}
}

// A cloud rung MUST be priced, because rung.Decide only applies the cost gate
// to priced rungs -- an unpriced cloud rung would be free money.
func TestCloudRungsMustBePriced(t *testing.T) {
	_, err := Load([]byte("version: 1\ninstance: { ladder: [glm] }\n" +
		"rungs: [{name: glm, kind: cloud, model: glm-5, est_cost_usd: 0, est_tokens: \"200K\"}]"))
	if err == nil {
		t.Fatal("a cloud rung with est_cost_usd: 0 was accepted; it would bypass the cost gate")
	}
}

func TestCheckLadderOrder(t *testing.T) {
	instance := []string{"qwen-local", "glm", "sonnet"}
	if err := CheckLadderOrder(instance, []string{"qwen-local", "sonnet"}); err != nil {
		t.Errorf("a subsequence was rejected: %v", err)
	}
	if err := CheckLadderOrder(instance, []string{"sonnet", "qwen-local"}); err == nil {
		t.Error("a project reordered cloud ahead of local and was accepted (spec 6.3: start at the cheapest rung)")
	}
	if err := CheckLadderOrder(nil, []string{"sonnet", "qwen-local"}); err != nil {
		t.Errorf("no instance ladder means no ordering constraint: %v", err)
	}
}
