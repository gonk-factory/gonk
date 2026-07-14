package budget

import (
	"encoding/json"
	"math"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// The whole point of this package: gonkcfg's unlimited sentinels are not
// JSON-serializable (+Inf errors) or not JSON-safe (MaxInt64 rounds in a
// float64 parser). Unlimited must come out as null, both ways.
func TestUnlimitedMarshalsAsNull(t *testing.T) {
	b := FromEffective(gonkcfg.EffectiveBudget{
		MonthlyCostUSD: math.Inf(1),
		MonthlyTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
		PerTaskTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
	})
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal(unlimited) = %v; the +Inf sentinel escaped", err)
	}
	want := `{"monthly_cost_usd":null,"monthly_tokens":null,"per_task_tokens":null}`
	if string(got) != want {
		t.Fatalf("Marshal = %s, want %s", got, want)
	}
}

func TestFiniteMarshalsAsNumbers(t *testing.T) {
	b := FromEffective(gonkcfg.EffectiveBudget{
		MonthlyCostUSD: 10.5,
		MonthlyTokens:  50_000_000,
		PerTaskTokens:  2_000_000,
	})
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"monthly_cost_usd":10.5,"monthly_tokens":50000000,"per_task_tokens":2000000}`
	if string(got) != want {
		t.Fatalf("Marshal = %s, want %s", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, in := range []Budget{
		{Unlimited, UnlimitedTokens, UnlimitedTokens},
		{0, 0, 0},
		{10.5, 50_000_000, 2_000_000},
	} {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal(%+v) = %v", in, err)
		}
		var out Budget
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("Unmarshal(%s) = %v", raw, err)
		}
		if out != in {
			t.Fatalf("round trip: %s -> %+v, want %+v", raw, out, in)
		}
	}
}

// A NaN or -Inf ceiling must never be silently emitted as a number. Resolve
// already fails closed on one (ADR-002), but this type is the last line before
// the wire.
func TestNonFiniteRefusesToMarshal(t *testing.T) {
	for name, c := range map[string]CostLimit{
		"NaN":  CostLimit(math.NaN()),
		"-Inf": CostLimit(math.Inf(-1)),
	} {
		if _, err := json.Marshal(c); err == nil {
			t.Errorf("%s: Marshal succeeded, want error", name)
		}
	}
	if _, err := json.Marshal(TokenLimit(-1)); err == nil {
		t.Error("negative TokenLimit marshaled, want error")
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	for _, raw := range []string{`-1`, `"lots"`, `1e400`} {
		var c CostLimit
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			t.Errorf("CostLimit accepted %s", raw)
		}
	}
}
