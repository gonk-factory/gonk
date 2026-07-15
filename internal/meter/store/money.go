package store

import (
	"fmt"
	"math"
)

// financeSafe reports whether f is safe to persist as money: not NaN, not
// +/-Inf. LiteLLM's spend log is external, unvalidated input, and a
// Postgres double precision column either rejects Infinity outright or
// silently stores NaN depending on configuration -- either way a non-finite
// dollar amount must never reach the budget arithmetic.
func financeSafe(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// validateFiniteMoney rejects a reservation whose held amounts are not
// finite, before they reach the store. A reservation's CostUSD/SyntheticCostUSD
// are meter's own computed values (rung.Reserve), not external input, but the
// guard is cheap and this is money-safety-critical code: never trust a float
// crossing a persistence boundary without checking it.
func validateFiniteMoney(vals ...float64) error {
	for _, v := range vals {
		if !financeSafe(v) {
			return fmt.Errorf("store: refusing to persist non-finite money value %v", v)
		}
	}
	return nil
}
