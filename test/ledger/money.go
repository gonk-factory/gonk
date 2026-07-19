// Package ledger is the three-way ledger-assertion library for the gonk e2e
// harness. It proves that three INDEPENDENT views of the same model spend agree:
// the stub's own request log (how many calls actually happened), LiteLLM's spend
// log (what the ledger recorded), and meter's cost API (what gonk reports). If
// any two disagree, a row was lost, double-counted, or misattributed -- each a
// distinct, named billing bug this library is built to surface.
//
// The ledger is Postgres-only (Plan 06 reconciliation D2): there is no Dolt
// store and no GONK_LEDGER switch. Meter runs with GONK_METER_STORE_BACKEND=
// postgres. Nothing in this package touches Dolt or a second backend.
//
// The SAME assertions run at every layer: L1 wires in-process fakes (the stub
// directly, a spend source seeded from it, meter's in-proc cost API); L2/L3
// point the SpendSource at real LiteLLM (/spend/logs/v2) and the CostSource at a
// real meter over HTTP. That portability is the whole point -- a scenario
// asserted at L1 asserts the identical invariant against real containers at L3.
package ledger

import "math"

// USDEpsilon bounds dollar comparisons. Cost is tokens x a configured per-token
// price, and neither the price nor the product is exactly representable in binary
// floating point. NEVER compare dollars with ==.
const USDEpsilon = 1e-9

// ApproxUSD reports whether two dollar amounts are equal within USDEpsilon.
func ApproxUSD(got, want float64) bool { return math.Abs(got-want) <= USDEpsilon }

// TB is the slice of *testing.T / *testing.B / *testing.F that this library's
// assertions need. It is deliberately NOT testing.TB: testing.TB is a sealed
// interface (it has an unexported method) that cannot be implemented outside the
// testing package, and this library's OWN tests (assert_test.go) must feed the
// assertions a recorder for which a FAILURE is the PASS -- the same saboteur
// idiom Plan 03 uses. Every *testing.T satisfies TB, so callers pass `t`
// unchanged.
type TB interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// AssertUSD fails with BOTH numbers and the delta. "expected 0.025, got 0.025"
// is the worst possible test failure.
func AssertUSD(t TB, what string, got, want float64) {
	t.Helper()
	if !ApproxUSD(got, want) {
		t.Fatalf("%s = %.12f, want %.12f (delta %.2e)", what, got, want, got-want)
	}
}
