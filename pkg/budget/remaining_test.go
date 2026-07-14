package budget

import (
	"math"
	"testing"
)

// Every case here uses NON-DEFAULT values. Plan 01's review found a resolver
// that ignored all three config layers and still passed, because the expected
// values coincided with the defaults. A test that would pass against
// `return Remaining{}` is not a test.
type remainCase struct {
	name string
	b    Budget
	s    Spend
	want Remaining
}

func remainCases() []remainCase {
	return []remainCase{
		{
			name: "observed and reserved both subtract",
			b:    Budget{MonthlyCostUSD: 10, MonthlyTokens: 50_000_000, PerTaskTokens: 2_000_000},
			s: Spend{
				CostUSD: 4, Tokens: 10_000_000, TaskTokens: 500_000,
				ReservedCostUSD: 1.5, ReservedTokens: 2_000_000, ReservedTaskTokens: 100_000,
			},
			want: Remaining{MonthlyCostUSD: 4.5, MonthlyTokens: 38_000_000, PerTaskTokens: 1_400_000},
		},
		{
			name: "overspend clamps to zero, never negative",
			b:    Budget{MonthlyCostUSD: 5, MonthlyTokens: 1_000, PerTaskTokens: 100},
			s:    Spend{CostUSD: 9.75, Tokens: 4_000, TaskTokens: 900},
			want: Remaining{MonthlyCostUSD: 0, MonthlyTokens: 0, PerTaskTokens: 0},
		},
		{
			name: "unlimited stays unlimited after spending",
			b:    Budget{MonthlyCostUSD: Unlimited, MonthlyTokens: UnlimitedTokens, PerTaskTokens: UnlimitedTokens},
			s:    Spend{CostUSD: 1234.56, Tokens: 9_000_000_000, TaskTokens: 8_000_000},
			want: Remaining{MonthlyCostUSD: Unlimited, MonthlyTokens: UnlimitedTokens, PerTaskTokens: UnlimitedTokens},
		},
		{
			name: "a zero ceiling is a real ceiling, not 'unset' (ADR-002)",
			b:    Budget{MonthlyCostUSD: 0, MonthlyTokens: 50_000_000, PerTaskTokens: 2_000_000},
			s:    Spend{Tokens: 1_000_000, TaskTokens: 1_000},
			want: Remaining{MonthlyCostUSD: 0, MonthlyTokens: 49_000_000, PerTaskTokens: 1_999_000},
		},
		{
			name: "mixed: unlimited cost, finite tokens",
			b:    Budget{MonthlyCostUSD: Unlimited, MonthlyTokens: 8_000_000, PerTaskTokens: 3_000_000},
			s:    Spend{CostUSD: 40, Tokens: 7_500_000, TaskTokens: 2_999_999, ReservedTokens: 100_000},
			want: Remaining{MonthlyCostUSD: Unlimited, MonthlyTokens: 400_000, PerTaskTokens: 1},
		},
		{
			// LiteLLM rows are external, unvalidated input. A credit or refund
			// row carrying a negative spend must NOT hand the project extra
			// headroom -- that is this package's only fail-open path.
			name: "a negative spend does not raise the ceiling",
			b:    Budget{MonthlyCostUSD: 10, MonthlyTokens: 50_000_000, PerTaskTokens: 2_000_000},
			s:    Spend{CostUSD: -100, Tokens: -5, TaskTokens: -5},
			want: Remaining{MonthlyCostUSD: 10, MonthlyTokens: 0, PerTaskTokens: 0},
		},
		{
			// THE SYNTHETIC-DOLLAR SEPARATION (Decision 9). Local rungs carry a
			// synthetic price so LiteLLM's USD key ceiling can act as a hard door
			// for TOKENS. Those dollars are an accounting unit, not spend, and
			// they must never touch monthly_cost_usd.
			//
			// $100 of synthetic spend and $50 of synthetic reservations against a
			// $10 REAL ceiling with $4 of real spend: remaining is $6. If the
			// synthetic dollars leaked in, it would be $0 -- and the onboarding
			// default (monthly_cost_usd: 0, ladder: [qwen-local]) could not afford
			// its own only rung.
			name: "synthetic local-rung dollars do NOT consume the real cost ceiling",
			b:    Budget{MonthlyCostUSD: 10, MonthlyTokens: 50_000_000, PerTaskTokens: 2_000_000},
			s: Spend{
				CostUSD: 4, SyntheticCostUSD: 100, ReservedSyntheticCostUSD: 50,
				Tokens: 1_000_000, TaskTokens: 300_000,
			},
			want: Remaining{MonthlyCostUSD: 6, MonthlyTokens: 49_000_000, PerTaskTokens: 1_700_000},
		},
		{
			// ...and the tokens a local rung burns DO count, against both token
			// ceilings. That is the whole reason it is metered at all (spec 6.3:
			// "local rungs are cost-0 dollars but metered in tokens").
			name: "a local rung's TOKENS still consume both token ceilings",
			b:    Budget{MonthlyCostUSD: 0, MonthlyTokens: 1_000_000, PerTaskTokens: 400_000},
			s:    Spend{SyntheticCostUSD: 3, Tokens: 750_000, TaskTokens: 150_000, ReservedTokens: 50_000},
			want: Remaining{MonthlyCostUSD: 0, MonthlyTokens: 200_000, PerTaskTokens: 250_000},
		},
	}
}

func TestRemain(t *testing.T) {
	for _, c := range remainCases() {
		t.Run(c.name, func(t *testing.T) {
			if got := Remain(c.b, c.s); got != c.want {
				t.Fatalf("Remain(%+v, %+v)\n got %+v\nwant %+v", c.b, c.s, got, c.want)
			}
		})
	}
}

// The sentinel is a package var because Go has no constant +Inf. Guard it.
func TestUnlimitedSentinelIsInf(t *testing.T) {
	if !math.IsInf(float64(Unlimited), 1) || int64(UnlimitedTokens) != math.MaxInt64 {
		t.Fatal("the unlimited sentinels have been reassigned; every budget check downstream is now wrong")
	}
}

// A NaN spend must not produce a NaN remaining (which loses every comparison
// and would read as "affordable"). Fail closed: treat it as fully spent.
func TestRemainFailsClosedOnNaNSpend(t *testing.T) {
	got := Remain(
		Budget{MonthlyCostUSD: 10, MonthlyTokens: 1_000, PerTaskTokens: 100},
		Spend{CostUSD: math.NaN()},
	)
	if got.MonthlyCostUSD != 0 {
		t.Fatalf("NaN spend gave remaining %v, want 0", got.MonthlyCostUSD)
	}
}

// Saturating arithmetic: a hostile or corrupt reservation total must not wrap.
func TestRemainDoesNotOverflow(t *testing.T) {
	got := Remain(
		Budget{MonthlyCostUSD: 10, MonthlyTokens: 1_000, PerTaskTokens: 100},
		Spend{Tokens: math.MaxInt64 - 1, ReservedTokens: math.MaxInt64 - 1},
	)
	if got.MonthlyTokens != 0 {
		t.Fatalf("overflowing spend gave remaining %d, want 0", int64(got.MonthlyTokens))
	}
}

func TestFits(t *testing.T) {
	r := Remaining{MonthlyCostUSD: 0.30, MonthlyTokens: 500_000, PerTaskTokens: 250_000}
	if !r.FitsCost(0.30) {
		t.Error("exactly-affordable cost rejected")
	}
	if r.FitsCost(0.31) {
		t.Error("unaffordable cost accepted")
	}
	// A zero remaining never fits a priced rung, even at a zero estimate:
	// an unpriced cloud rung must not slip through a $0 budget.
	zero := Remaining{MonthlyCostUSD: 0}
	if zero.FitsCost(0) {
		t.Error("priced rung admitted against a zero cost budget")
	}
	if !r.FitsMonthTokens(500_000) || r.FitsMonthTokens(500_001) {
		t.Error("month token fit wrong")
	}
	if !r.FitsTaskTokens(250_000) || r.FitsTaskTokens(250_001) {
		t.Error("task token fit wrong")
	}
	unl := Remaining{MonthlyCostUSD: Unlimited, MonthlyTokens: UnlimitedTokens, PerTaskTokens: UnlimitedTokens}
	if !unl.FitsCost(1e9) || !unl.FitsMonthTokens(math.MaxInt64-1) || !unl.FitsTaskTokens(math.MaxInt64-1) {
		t.Error("unlimited failed to fit")
	}
}
