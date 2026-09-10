package store

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
)

// TestMissingLegNamesTheFirstShortLeg is R-25's unit-level proof: missingLeg
// is what both Memory.ReserveIfFits and Postgres.ReserveIfFits call to decide
// which leg a lost race gets blamed on (internal/meter/service then turns
// that into the defer's Reason/Detail -- see
// TestDecideLostRaceReasonNamesTheLegThatLost in the service package for the
// end-to-end proof). Exercised directly here so the three-way branch itself
// -- cost, then monthly tokens, then per-task tokens, in that order -- is
// pinned independently of either backend's plumbing.
func TestMissingLegNamesTheFirstShortLeg(t *testing.T) {
	plenty := budget.Remaining{
		MonthlyCostUSD: budget.CostLimit(100),
		MonthlyTokens:  budget.TokenLimit(1_000_000),
		PerTaskTokens:  budget.TokenLimit(1_000_000),
	}

	cases := []struct {
		name string
		rem  budget.Remaining
		r    Reservation
		want ReserveMiss
	}{
		{
			name: "fits every leg -> no miss",
			rem:  plenty,
			r:    Reservation{CostUSD: 1, Tokens: 100},
			want: "",
		},
		{
			name: "cost leg short",
			rem: budget.Remaining{
				MonthlyCostUSD: budget.CostLimit(0.10), // less than the $1 this reservation wants
				MonthlyTokens:  plenty.MonthlyTokens,
				PerTaskTokens:  plenty.PerTaskTokens,
			},
			r:    Reservation{CostUSD: 1.00, Tokens: 100},
			want: MissCost,
		},
		{
			name: "a ZERO-cost (local) reservation never blames the cost leg, even under a $0 ceiling",
			rem: budget.Remaining{
				MonthlyCostUSD: budget.CostLimit(0), // $0 remaining
				MonthlyTokens:  plenty.MonthlyTokens,
				PerTaskTokens:  plenty.PerTaskTokens,
			},
			r:    Reservation{CostUSD: 0, Tokens: 100}, // local rung: no real dollars
			want: "",
		},
		{
			name: "monthly token leg short (cost fits)",
			rem: budget.Remaining{
				MonthlyCostUSD: plenty.MonthlyCostUSD,
				MonthlyTokens:  budget.TokenLimit(50), // less than the 100 tokens wanted
				PerTaskTokens:  plenty.PerTaskTokens,
			},
			r:    Reservation{CostUSD: 1, Tokens: 100},
			want: MissMonthlyTokens,
		},
		{
			name: "per-task token leg short (cost and monthly tokens fit)",
			rem: budget.Remaining{
				MonthlyCostUSD: plenty.MonthlyCostUSD,
				MonthlyTokens:  plenty.MonthlyTokens,
				PerTaskTokens:  budget.TokenLimit(50), // less than the 100 tokens wanted
			},
			r:    Reservation{CostUSD: 1, Tokens: 100},
			want: MissTaskTokens,
		},
		{
			// Precedence: when MULTIPLE legs are short at once, cost is named
			// first -- matching the order the original `||` fits-check used,
			// so this refactor changes NOTHING about which leg callers were
			// already seeing fail first.
			name: "cost and both token legs all short -> cost named first",
			rem: budget.Remaining{
				MonthlyCostUSD: budget.CostLimit(0.10),
				MonthlyTokens:  budget.TokenLimit(10),
				PerTaskTokens:  budget.TokenLimit(10),
			},
			r:    Reservation{CostUSD: 1, Tokens: 100},
			want: MissCost,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := missingLeg(tc.rem, tc.r); got != tc.want {
				t.Fatalf("missingLeg = %q, want %q", got, tc.want)
			}
		})
	}
}
