package budget

import "testing"

// A saboteur is a deliberately-wrong Remain. Each one MUST be caught by at
// least one row of TestRemain's table. If a saboteur survives the whole table,
// the table does not actually constrain the money math and this test fails,
// naming the bug we would have shipped.
type saboteur struct {
	name string
	bug  func(Budget, Spend) Remaining
}

func saboteurs() []saboteur {
	return []saboteur{
		{"ignores reserved spend", func(b Budget, s Spend) Remaining {
			s.ReservedCostUSD, s.ReservedTokens, s.ReservedTaskTokens = 0, 0, 0
			return Remain(b, s)
		}},
		{"ignores observed spend", func(b Budget, s Spend) Remaining {
			s.CostUSD, s.Tokens, s.TaskTokens = 0, 0, 0
			return Remain(b, s)
		}},
		{"treats a zero ceiling as unset", func(b Budget, s Spend) Remaining {
			if b.MonthlyCostUSD == 0 {
				b.MonthlyCostUSD = Unlimited
			}
			return Remain(b, s)
		}},
		{"lets an unlimited ceiling decay", func(b Budget, s Spend) Remaining {
			if b.MonthlyTokens.Unlimited() {
				b.MonthlyTokens = TokenLimit(int64(UnlimitedTokens) - 1)
			}
			return Remain(b, s)
		}},
		{"ignores the per-task ceiling", func(b Budget, s Spend) Remaining {
			b.PerTaskTokens = UnlimitedTokens
			return Remain(b, s)
		}},
		{"confuses task tokens with month tokens", func(b Budget, s Spend) Remaining {
			s.TaskTokens = s.Tokens
			return Remain(b, s)
		}},
		{"lets a negative spend raise the ceiling", func(b Budget, s Spend) Remaining {
			// The bug: no `if used < 0 { used = 0 }` guard in remainCost.
			r := Remain(b, s)
			if s.CostUSD+s.ReservedCostUSD < 0 && !b.MonthlyCostUSD.Unlimited() {
				r.MonthlyCostUSD = CostLimit(float64(b.MonthlyCostUSD) - (s.CostUSD + s.ReservedCostUSD))
			}
			return r
		}},
		{"counts SYNTHETIC dollars against the real cost ceiling", func(b Budget, s Spend) Remaining {
			// The bug someone WILL write: "cost is cost, add it all up." It kills
			// the onboarding default -- a monthly_cost_usd: 0 project can no
			// longer afford its own local rung -- and it silently reports
			// accounting fiction as real money on the Cost dashboard.
			s.CostUSD += s.SyntheticCostUSD
			s.ReservedCostUSD += s.ReservedSyntheticCostUSD
			return Remain(b, s)
		}},
		{"drops a local rung's TOKENS because it is 'free'", func(b Budget, s Spend) Remaining {
			// The mirror-image bug: "local is free, so don't meter it." Spec 6.3
			// says local rungs are cost-0 dollars but METERED IN TOKENS, and with
			// Decision 9 the token reservation is what the hard door is built on.
			if s.SyntheticCostUSD > 0 || s.ReservedSyntheticCostUSD > 0 {
				s.Tokens, s.TaskTokens, s.ReservedTokens, s.ReservedTaskTokens = 0, 0, 0, 0
			}
			return Remain(b, s)
		}},
	}
}

func TestSaboteursAreCaught(t *testing.T) {
	// Reuse TestRemain's table by re-declaring it through the same helper.
	for _, sab := range saboteurs() {
		caught := false
		for _, c := range remainCases() {
			if sab.bug(c.b, c.s) != c.want {
				caught = true
				break
			}
		}
		if !caught {
			t.Errorf("saboteur %q survived every table row: the budget table is vacuous and would ship this bug", sab.name)
		}
	}
}
