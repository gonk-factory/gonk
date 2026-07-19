//go:build component

package component_test

import (
	"math"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

// TestVirtualKeyCeilingRefusesEvenWithMeterBypassed is THE MOST IMPORTANT TEST in
// the suite. Meter is the soft door (it refuses to START a session). LiteLLM's
// virtual-key USD ceiling is the HARD door: it refuses a request MID-FLIGHT, at
// the only place agent pods can reach a model. If the hard door does not close, a
// runaway session spends past its ceiling and nothing stops it.
//
// So this test DELIBERATELY BYPASSES METER: it takes a key with a $1.00 ceiling
// and hammers LiteLLM directly, past it. LiteLLM MUST refuse.
func TestVirtualKeyCeilingRefusesEvenWithMeterBypassed(t *testing.T) {
	w := newWorld(t)
	// glm is $2.00/1M; 100k tokens/call => $0.20/call. Five calls = $1.00; the
	// sixth must die.
	w.Stub.SetScript(alwaysAnswer(100_000, 0))
	key := w.LiteLLM.ProvisionKey(t, "gonk-harddoor-"+harnessShortID(), ptr(1.00), []string{"stub-glm"})

	spent := 0.0
	refusedAt := 0
	for i := 1; i <= 12; i++ {
		status := w.LiteLLM.CallDirect(t, key, "stub-glm", nil) // NO meter, NO reservation
		if status == 200 {
			spent += 0.20
			continue
		}
		refusedAt = i
		break
	}
	if refusedAt == 0 {
		t.Fatalf("LITELLM NEVER REFUSED. The hard door does not exist. $%.2f spent against a "+
			"$1.00 ceiling. This is a budget escape and it is fatal.", spent)
	}
	if spent > 1.00+0.20 { // one call's overshoot is the documented, accepted slack
		t.Fatalf("overspent by $%.2f before the door closed", spent-1.00)
	}
	t.Logf("hard door closed at call %d, after $%.2f of a $1.00 ceiling", refusedAt, spent)
}

// TestUSDDoorClosesOnALocalOnlyProjectThatExhaustsItsTokenBudget is P3-5 and
// Decision 9's WHOLE CLAIM: a LOCAL-ONLY project (monthly_cost_usd: 0, ladder
// [qwen-local]) has its TOKEN budget folded into the USD ceiling on its virtual
// key -- via the SYNTHETIC price -- so the dollar door closes on a project that
// never spends a real dollar. If it does NOT close, monthly_tokens has no hard
// enforcement anywhere and Decision 9 is a fiction.
func TestUSDDoorClosesOnALocalOnlyProjectThatExhaustsItsTokenBudget(t *testing.T) {
	w := newWorld(t)
	// The exact fold meter would compute: 0 real + 1,000,000 tokens x $0.25/1M synthetic.
	eff := gonkcfg.EffectiveBudget{
		MonthlyCostUSD: 0,
		MonthlyTokens:  gonkcfg.TokenQuantity(1_000_000),
		PerTaskTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
	}
	maxBudget := litellm.MaxBudgetFor(eff, []string{"qwen-local"}, w.Catalog)
	if maxBudget == nil {
		t.Fatal("MaxBudgetFor returned nil for a finite local-only budget -- the token ceiling has no USD door")
	}
	if !ledger.ApproxUSD(*maxBudget, 0.25) {
		t.Fatalf("folded max_budget = %v, want 0.25 (1M tokens x $0.25/1M synthetic)", *maxBudget)
	}

	// qwen-local is $0.25/1M; 100k tokens/call => $0.025/call. Ten calls = $0.25.
	w.Stub.SetScript(alwaysAnswer(100_000, 0))
	key := w.LiteLLM.ProvisionKey(t, "gonk-localdoor-"+harnessShortID(), maxBudget, []string{"stub-qwen"})
	if got := w.LiteLLM.KeyMaxBudget(t, key); !ledger.ApproxUSD(got, 0.25) {
		t.Fatalf("LiteLLM stored max_budget = %v, want 0.25 (the token ceiling, priced synthetically)", got)
	}

	spent := 0.0
	refusedAt := 0
	for i := 1; i <= 20; i++ {
		status := w.LiteLLM.CallDirect(t, key, "stub-qwen", nil)
		if status == 200 {
			spent += 0.025
			continue
		}
		refusedAt = i
		break
	}
	if refusedAt == 0 {
		t.Fatalf("the SYNTHETIC USD door NEVER closed on a local-only project: $%.3f of synthetic "+
			"spend against a $0.25 ceiling. monthly_tokens has NO hard enforcement -- Decision 9 is a fiction.", spent)
	}
	if spent > 0.25+0.025 { // one call's overshoot slack
		t.Fatalf("local-only project overspent its token ceiling by $%.3f before the door closed", spent-0.25)
	}
	t.Logf("synthetic USD door closed at call %d, after $%.3f of a $0.25 token-derived ceiling", refusedAt, spent)
}

// TestSyntheticPricesAgreeBetweenTheRungCatalogAndLiteLLM (P3-4). The two files
// that must agree -- the operator rung catalog and LiteLLM's model list -- and the
// only place anything reads both. If they drift, the hard door is set to a
// DIFFERENT ceiling than meter thinks.
func TestSyntheticPricesAgreeBetweenTheRungCatalogAndLiteLLM(t *testing.T) {
	w := newWorld(t)
	info := w.LiteLLM.ModelInfo(t)
	for _, r := range w.Catalog {
		if r.Kind != opercfg.KindLocal {
			continue
		}
		want := r.SyntheticUSDPer1MTokens / 1e6
		got, ok := info[r.Model]
		if !ok {
			t.Fatalf("rung %q model %q not in LiteLLM /model/info", r.Name, r.Model)
		}
		if !ledger.ApproxUSD(got.InputPerToken, want) {
			t.Fatalf("rung %q: catalog says $%g/token, LiteLLM says $%g/token. The hard door is set to a "+
				"DIFFERENT ceiling than meter thinks. These live in two different files and NOTHING ELSE CHECKS THEM.",
				r.Name, want, got.InputPerToken)
		}
	}
}
