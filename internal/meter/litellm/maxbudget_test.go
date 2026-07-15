package litellm

import (
	"math"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
)

// testCatalog: qwen-local costs no REAL money but carries a synthetic price
// (Decision 9); glm and sonnet are cloud rungs priced in real money. sonnet is
// deliberately the most expensive per-token rung, so a table row that expects
// "priced at the most expensive rung" is unambiguous about which one that is.
func testCatalog() map[string]opercfg.RungSpec {
	return map[string]opercfg.RungSpec{
		"qwen-local": {Name: "qwen-local", Kind: opercfg.KindLocal, Model: "qwen3-coder-30b",
			EstCostUSD: 0, EstTokens: 200_000, SyntheticUSDPer1MTokens: 0.20}, // $0.0000002/token
		"glm": {Name: "glm", Kind: opercfg.KindCloud, Model: "glm-5",
			EstCostUSD: 0.40, EstTokens: 200_000}, // $0.000002/token
		"sonnet": {Name: "sonnet", Kind: opercfg.KindCloud, Model: "claude-sonnet",
			EstCostUSD: 1.20, EstTokens: 200_000}, // $0.000006/token -- the most expensive
	}
}

func TestMaxBudgetFor(t *testing.T) {
	cat := testCatalog()

	t.Run("cloud+local ladder prices the token term at the most expensive rung", func(t *testing.T) {
		b := gonkcfg.EffectiveBudget{MonthlyCostUSD: 50, MonthlyTokens: 1_000_000}
		got := MaxBudgetFor(b, []string{"qwen-local", "glm", "sonnet"}, cat)
		if got == nil {
			t.Fatal("got nil, want a finite ceiling")
		}
		// sonnet's price/token ($1.20/200_000 = 0.000006) is the most expensive of
		// the three, even though it is last in the ladder -- the max must not just
		// take the last entry.
		want := 50 + 1_000_000*(1.20/200_000)
		if math.Abs(*got-want) > 1e-9 {
			t.Fatalf("MaxBudgetFor = %v, want %v", *got, want)
		}
	})

	t.Run("local-only ladder with monthly_cost_usd 0 still yields a positive ceiling", func(t *testing.T) {
		// This IS Decision 9: before folding tokens into the dollar ceiling, the
		// onboarding default (monthly_cost_usd: 0, local rungs only) had a hard
		// door of $0 -- which also means "no local work at all" if max_budget=0
		// is even honored, or NO door if the field is omitted at 0. Either way
		// wrong. After the fold, the ceiling is > 0 because the synthetic price
		// gives the token ceiling teeth in dollars.
		b := gonkcfg.EffectiveBudget{MonthlyCostUSD: 0, MonthlyTokens: 1_000_000}
		got := MaxBudgetFor(b, []string{"qwen-local"}, cat)
		if got == nil {
			t.Fatal("got nil, want a finite positive ceiling")
		}
		if !(*got > 0) {
			t.Fatalf("MaxBudgetFor = %v, want > 0 -- this is the entire point of Decision 9", *got)
		}
		want := 0 + 1_000_000*(0.20/1_000_000)
		if math.Abs(*got-want) > 1e-9 {
			t.Fatalf("MaxBudgetFor = %v, want %v", *got, want)
		}
	})

	t.Run("unlimited cost ceiling yields no hard door", func(t *testing.T) {
		b := gonkcfg.EffectiveBudget{MonthlyCostUSD: math.Inf(1), MonthlyTokens: 1_000_000}
		if got := MaxBudgetFor(b, []string{"glm"}, cat); got != nil {
			t.Fatalf("MaxBudgetFor = %v, want nil (unlimited cost ceiling)", *got)
		}
	})

	t.Run("unlimited token ceiling yields no hard door", func(t *testing.T) {
		b := gonkcfg.EffectiveBudget{MonthlyCostUSD: 10, MonthlyTokens: math.MaxInt64}
		if got := MaxBudgetFor(b, []string{"glm"}, cat); got != nil {
			t.Fatalf("MaxBudgetFor = %v, want nil (unlimited token ceiling)", *got)
		}
	})

	t.Run("empty ladder makes the token term zero", func(t *testing.T) {
		b := gonkcfg.EffectiveBudget{MonthlyCostUSD: 10, MonthlyTokens: 1_000_000}
		got := MaxBudgetFor(b, nil, cat)
		if got == nil {
			t.Fatal("got nil, want a finite ceiling (cost alone is finite)")
		}
		if *got != 10 {
			t.Fatalf("MaxBudgetFor = %v, want 10 (no rung to price the token term against)", *got)
		}
	})

	// THE MONEY ROW. A project that set a real, finite cost ceiling still gets
	// NO LiteLLM hard door if its token ceiling is unlimited -- do not "simplify"
	// this away as just another unlimited->nil case. See the doc comment on
	// MaxBudgetFor: this is a Known limitation (soft meter gate only on the cost
	// ceiling), not a bug and not "no door wanted".
	t.Run("finite cost + unlimited tokens: unbudgeted key, only meter's soft gate guards the cost ceiling", func(t *testing.T) {
		b := gonkcfg.EffectiveBudget{MonthlyCostUSD: 10, MonthlyTokens: math.MaxInt64}
		got := MaxBudgetFor(b, []string{"glm", "sonnet"}, cat)
		if got != nil {
			t.Fatalf("MaxBudgetFor = %v, want nil: finite cost (10) + unlimited tokens must still yield an unbudgeted key -- LiteLLM provisions no max_budget, and only meter's soft reservation-estimate gate protects the $10 ceiling, which a runaway session can overshoot", *got)
		}
	})
}
