package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// TestBudgetRemainingGaugeEmitsInfForUnlimited: Prometheus's text format DOES
// support +Inf, so unlimited stays +Inf here -- unlike the JSON API, where it
// must be null (pkg/budget). This is the carry-forward's other half: the SAME
// value has two correct encodings, and confusing them is how an unlimited
// project ends up with a 9.22e18 gauge in Grafana.
func TestBudgetRemainingGaugeEmitsInfForUnlimited(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.SetBudgetRemaining("group/repo", budget.Unlimited, budget.TokenLimit(1000))

	expected := `
# HELP gonk_meter_budget_remaining_usd Remaining monthly USD headroom by project. +Inf for an unlimited ceiling -- Prometheus's text format supports it; the JSON cost API uses null for the same fact (pkg/budget).
# TYPE gonk_meter_budget_remaining_usd gauge
gonk_meter_budget_remaining_usd{project="group/repo"} +Inf
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "gonk_meter_budget_remaining_usd"); err != nil {
		t.Fatal(err)
	}
}

// TestBudgetRemainingTokensGaugeEmitsInfForUnlimited: the token sentinel is
// MaxInt64, NOT +Inf -- so this gauge needs an explicit conversion that the
// USD one does not, and it is the one that actually produces the 9.22e18 in
// Grafana if you forget.
func TestBudgetRemainingTokensGaugeEmitsInfForUnlimited(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.SetBudgetRemaining("group/repo", budget.CostLimit(5), budget.UnlimitedTokens)

	expected := `
# HELP gonk_meter_budget_remaining_tokens Remaining monthly token headroom by project. +Inf for an unlimited ceiling -- NOT the raw MaxInt64 sentinel, which would render as ~9.22e18 in Grafana.
# TYPE gonk_meter_budget_remaining_tokens gauge
gonk_meter_budget_remaining_tokens{project="group/repo"} +Inf
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "gonk_meter_budget_remaining_tokens"); err != nil {
		t.Fatal(err)
	}
}

// TestLocalRungsCostNoRealMoneyButAreMeteredInTokens: spec 6.3: "Local rungs
// are cost-0 dollars but metered in tokens for fairness visibility."
func TestLocalRungsCostNoRealMoneyButAreMeteredInTokens(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	// A local rung: real cost is 0 (per spec 6.3, its LiteLLM `spend` figure
	// is reported separately as synthetic -- see the next test), but it still
	// burns tokens.
	m.RecordSpendRow("group/repo", "qwen-local", "triage", true, 0.02, 100, 50)

	if got := testutil.ToFloat64(m.tokens.WithLabelValues("group/repo", "qwen-local", "triage", "prompt")); got != 100 {
		t.Fatalf("prompt tokens = %v, want 100", got)
	}
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("group/repo", "qwen-local", "triage", "completion")); got != 50 {
		t.Fatalf("completion tokens = %v, want 50", got)
	}
	if got := testutil.ToFloat64(m.spendUSD.WithLabelValues("group/repo", "qwen-local", "triage", "false")); got != 0 {
		t.Fatalf("real (synthetic=false) spend for a local rung = %v, want 0", got)
	}
}

// TestSyntheticSpendIsLabelledAndNeverMixedWithReal: DECISION 9. A local
// rung's dollars are an accounting fiction. The label is what lets the Cost
// dashboard show REAL spend by default. Without it, every dashboard silently
// reports fiction as money.
func TestSyntheticSpendIsLabelledAndNeverMixedWithReal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.RecordSpendRow("group/repo", "qwen-local", "triage", true, 0.02, 100, 50) // synthetic
	m.RecordSpendRow("group/repo", "glm", "triage", false, 1.50, 200, 100)      // real

	if got := testutil.ToFloat64(m.spendUSD.WithLabelValues("group/repo", "qwen-local", "triage", "true")); got != 0.02 {
		t.Fatalf("synthetic spend for qwen-local = %v, want 0.02", got)
	}
	if got := testutil.ToFloat64(m.spendUSD.WithLabelValues("group/repo", "qwen-local", "triage", "false")); got != 0 {
		t.Fatalf("real spend for qwen-local = %v, want 0", got)
	}
	if got := testutil.ToFloat64(m.spendUSD.WithLabelValues("group/repo", "glm", "triage", "true")); got != 0 {
		t.Fatalf("synthetic spend for glm = %v, want 0", got)
	}
	if got := testutil.ToFloat64(m.spendUSD.WithLabelValues("group/repo", "glm", "triage", "false")); got != 1.50 {
		t.Fatalf("real spend for glm = %v, want 1.50", got)
	}
}

// TestNoUnboundedLabels: gather every metric and assert no label named
// bead_id or session_key exists. This test is the guardrail: it will fail
// the day someone adds one.
func TestNoUnboundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)
	touchEverySeries(m)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) == 0 {
		t.Fatal("no metric families registered -- test is vacuous")
	}
	forbidden := map[string]bool{"bead_id": true, "session_key": true}
	found := false
	for _, fam := range families {
		for _, metric := range fam.Metric {
			for _, lbl := range metric.Label {
				found = true
				if forbidden[lbl.GetName()] {
					t.Fatalf("metric %s has forbidden unbounded label %q", fam.GetName(), lbl.GetName())
				}
			}
		}
	}
	if !found {
		t.Fatal("no labelled series observed -- test is vacuous")
	}
}

// TestDecisionCounterUsesTheBoundedReasonSet: every meterapi.Reason* constant
// is a legal label value and nothing else is.
func TestDecisionCounterUsesTheBoundedReasonSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	legal := []string{
		"", // a Run decision carries no reason
		meterapi.ReasonNotRegistered, meterapi.ReasonDisabled, meterapi.ReasonInvalidConfig,
		meterapi.ReasonActionNotAllowed, meterapi.ReasonKeyMissing, meterapi.ReasonQuietHours,
		meterapi.ReasonSpendStale, meterapi.ReasonLadderExhausted, meterapi.ReasonInfraRetriesExhausted,
		meterapi.ReasonPerTaskTokensExhausted, meterapi.ReasonMonthlyTokensExhausted, meterapi.ReasonMonthlyCostExhausted,
	}
	for _, r := range legal {
		m.RecordDecision("group/repo", meterapi.DecisionDefer, r)
	}
	// A hostile/buggy reason string must NOT reach the label verbatim.
	const hostile = "some-free-text-a-litellm-error-produced"
	m.RecordDecision("group/repo", meterapi.DecisionDefer, hostile)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var reasonsSeen []string
	for _, fam := range families {
		if fam.GetName() != "gonk_meter_policy_decisions_total" {
			continue
		}
		for _, metric := range fam.Metric {
			for _, lbl := range metric.Label {
				if lbl.GetName() == "reason" {
					reasonsSeen = append(reasonsSeen, lbl.GetValue())
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, r := range reasonsSeen {
		seen[r] = true
	}
	for _, r := range legal {
		if !seen[r] {
			t.Errorf("legal reason %q never appeared as a label value", r)
		}
	}
	if seen[hostile] {
		t.Fatalf("hostile reason %q reached the label verbatim -- must collapse to %q", hostile, unknownLabel)
	}
	if !seen[unknownLabel] {
		t.Fatalf("hostile reason did not collapse to the bounded fallback %q", unknownLabel)
	}
}

// touchEverySeries exercises every recording/setting method at least once so
// Gather() has something to report for every series (a *Vec with no child
// yet emits nothing).
func touchEverySeries(m *Metrics) {
	m.RecordSpendRow("group/repo", "glm", "triage", false, 1.0, 10, 5)
	m.SetBudgetRemaining("group/repo", budget.CostLimit(5), budget.TokenLimit(100))
	m.RecordDecision("group/repo", meterapi.DecisionRun, "")
	m.RecordGateOutcome("group/repo", "glm", meterapi.OutcomeSuccess)
	m.RecordEscalation("group/repo", "qwen-local", "glm")
	m.RecordReservationRaceLost("group/repo")
	m.SetDeferredBeads("group/repo", 1)
	m.SetProjectState("group/repo", "active")
	m.SetVirtualKeys(1, 0)
	m.SetReservationsOpen("group/repo", 1)
	m.RecordReservationsExpired("group/repo", 1)
	m.SetSpendSyncAge(0)
	m.SetSpendSyncedAt(time.Now())
	m.RecordSpendSyncFailure()
	m.AddSpendRowsUnattributed(1)
	m.SetClockSkew(0)
	m.RecordClockSkewUnknown()
	m.SetBudgetWindowStart(time.Now())
	m.RecordColdStart()
	m.RecordCatalogDrift()
}
