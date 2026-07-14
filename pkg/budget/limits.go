// Package budget is the JSON-safe money layer over gonkcfg.EffectiveBudget.
//
// gonkcfg encodes "unlimited" as concrete sentinels: math.Inf(1) for cost and
// math.MaxInt64 for tokens (ADR-002). Neither survives contact with JSON --
// encoding/json REFUSES to marshal +Inf (it returns an error, not a number),
// and MaxInt64 loses precision in any float64-based parser, which is every
// JavaScript client. This package translates both to JSON null on the way out
// and back on the way in, so "unlimited" means exactly one thing on the wire.
package budget

import (
	"encoding/json"
	"fmt"
	"math"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// Unlimited is the cost sentinel (gonkcfg.EffectiveBudget.MonthlyCostUSD).
//
// It is a var and not a const because Go has no constant expression for +Inf
// (math.Inf is a function call, not a constant). TestUnlimitedSentinelIsInf
// guards it, so a package that reassigned it would be caught.
var Unlimited = CostLimit(math.Inf(1))

// UnlimitedTokens is the token sentinel (gonkcfg.EffectiveBudget token fields).
const UnlimitedTokens = TokenLimit(math.MaxInt64)

// CostLimit is a USD ceiling. +Inf means unlimited and marshals as null.
type CostLimit float64

func (c CostLimit) Unlimited() bool { return math.IsInf(float64(c), 1) }

func (c CostLimit) MarshalJSON() ([]byte, error) {
	f := float64(c)
	switch {
	case math.IsInf(f, 1):
		return []byte("null"), nil
	case math.IsNaN(f) || math.IsInf(f, -1) || f < 0:
		return nil, fmt.Errorf("budget: refusing to marshal cost ceiling %v", f)
	}
	return json.Marshal(f)
}

func (c *CostLimit) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*c = Unlimited
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("budget: cost ceiling: %w", err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return fmt.Errorf("budget: cost ceiling must be a finite number >= 0, got %v", f)
	}
	*c = CostLimit(f)
	return nil
}

// TokenLimit is a token ceiling. MaxInt64 means unlimited and marshals as null.
type TokenLimit int64

func (t TokenLimit) Unlimited() bool { return t == UnlimitedTokens }

func (t TokenLimit) MarshalJSON() ([]byte, error) {
	if t.Unlimited() {
		return []byte("null"), nil
	}
	if t < 0 {
		return nil, fmt.Errorf("budget: refusing to marshal token ceiling %d", int64(t))
	}
	return json.Marshal(int64(t))
}

func (t *TokenLimit) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*t = UnlimitedTokens
		return nil
	}
	var i int64
	if err := json.Unmarshal(raw, &i); err != nil {
		return fmt.Errorf("budget: token ceiling: %w", err)
	}
	if i < 0 {
		return fmt.Errorf("budget: token ceiling must be >= 0, got %d", i)
	}
	*t = TokenLimit(i)
	return nil
}

// Budget is an Effective budget, wire-shaped.
type Budget struct {
	MonthlyCostUSD CostLimit  `json:"monthly_cost_usd"`
	MonthlyTokens  TokenLimit `json:"monthly_tokens"`
	PerTaskTokens  TokenLimit `json:"per_task_tokens"`
}

// FromEffective converts gonkcfg's resolved budget. It is the ONLY legitimate
// way to build a Budget outside tests: per ADR-002, gonkcfg.Resolve is the only
// legitimate way to build an Effective.
func FromEffective(b gonkcfg.EffectiveBudget) Budget {
	return Budget{
		MonthlyCostUSD: CostLimit(b.MonthlyCostUSD),
		MonthlyTokens:  TokenLimit(b.MonthlyTokens),
		PerTaskTokens:  TokenLimit(b.PerTaskTokens),
	}
}

// AllUnlimited reports whether no ceiling constrains this project at all.
// Used to decide whether stale spend data even matters.
func (b Budget) AllUnlimited() bool {
	return b.MonthlyCostUSD.Unlimited() && b.MonthlyTokens.Unlimited() && b.PerTaskTokens.Unlimited()
}
