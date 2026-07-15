// Package litellm is gonk-meter's seam onto the LiteLLM proxy: the admin API
// that provisions per-project virtual keys, and the spend log that is the raw
// ledger (spec 6.1, 6.2).
//
// Meter is NEVER in the request path. Agent pods talk to LiteLLM directly with
// their project's virtual key; LiteLLM performs the hard budget refusal at the
// only door. Meter provisions the key, reads the log, and decides policy.
//
// NOTE (Decision 9): a LiteLLM virtual key enforces a USD max_budget and
// nothing else -- there is no token counter. Left alone, that means local rungs
// (priced at $0) never move the dollar counter, so the hard door never fires for
// them, and monthly_tokens has no hard enforcement anywhere.
//
// So we PRICE LOCAL MODELS SYNTHETICALLY. Every local rung carries a nonzero
// synthetic per-token price (opercfg's rung catalog), configured identically in
// LiteLLM's model list, and meter folds the project's TOKEN ceiling into the USD
// max_budget it provisions on the virtual key:
//
//	max_budget = monthly_cost_usd + monthly_tokens * max(pricePerToken(ladder))
//
// The USD door is now a genuine hard door for every rung. It is deliberately
// LOOSE (an upper bound, so it can never cut off legitimate work early); meter's
// per-rung reservation gate remains the tight, primary control. Defence in depth.
//
// The dollars this produces for local models ARE SYNTHETIC and must never be
// presented as real spend -- see spend.Row.Synthetic and the cost API's
// cost_synthetic flag.
package litellm

import (
	"context"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
)

// KeySpec is the virtual key gonk wants LiteLLM to have for a project. Alias is
// the idempotency key: EnsureKey creates or updates by alias.
type KeySpec struct {
	Alias string
	// MaxBudgetUSD is THE HARD DOOR. nil = unlimited (and then there IS no hard
	// door -- say so, do not pretend). Build it with MaxBudgetFor, never by hand.
	MaxBudgetUSD   *float64
	BudgetDuration string // must be the UTC calendar month; see AD-9
	Models         []string
	Metadata       map[string]string
	TPMLimit       *int
	RPMLimit       *int
}

// MaxBudgetFor converts a project's resolved budget into the single USD ceiling
// LiteLLM can actually enforce. This is the mechanical heart of Decision 9.
//
// LiteLLM's virtual key has ONE counter and it is denominated in dollars. It has
// no token counter. So to give `monthly_tokens` a hard door at all, we fold it
// into the dollar ceiling, using the per-token price each rung will actually be
// charged at (real for cloud, synthetic for local):
//
//	max_budget = monthly_cost_usd + monthly_tokens * max(pricePerToken(ladder))
//
// Pricing the token term at the MOST EXPENSIVE rung in the ladder makes the
// ceiling an UPPER BOUND: it can never close early on legitimate work, which
// would be worse than having no door. It is correspondingly LOOSE -- a project
// that runs entirely on its cheapest rung can exceed monthly_tokens before the
// dollar counter runs out. That is fine and it is the design: meter's per-rung
// reservation gate is the tight, primary control, and this is the backstop that
// catches meter being wrong or being bypassed.
//
// If EITHER ceiling is unlimited, there is no finite dollar ceiling to compute
// and we return nil (LiteLLM then omits `max_budget` -- no hard door at all).
//
// READ THIS BEFORE YOU "SIMPLIFY" IT: returning nil when only ONE ceiling is
// unlimited does NOT mean "the project has no hard door because it wants none".
// A project with a finite `monthly_cost_usd` but an unlimited `monthly_tokens`
// DOES have a cost ceiling it expects to be enforced -- and it gets NO LiteLLM
// backstop, only meter's soft reservation-estimate gate, which a runaway session
// can overshoot. We still return nil deliberately (folding cost alone into
// `max_budget` would let synthetic local dollars slam the door on legitimate
// local work -- see Decision 9), but the money consequence is real and is
// recorded as a Known limitation, not waved away as "no door wanted". The only
// case that is genuinely door-free is BOTH ceilings unlimited.
func MaxBudgetFor(b gonkcfg.EffectiveBudget, ladder []string, catalog map[string]opercfg.RungSpec) *float64 {
	cost := budget.CostLimit(b.MonthlyCostUSD)
	tokens := budget.TokenLimit(b.MonthlyTokens)
	if cost.Unlimited() || tokens.Unlimited() {
		// No finite dollar ceiling to provision. A finite cost + unlimited tokens
		// lands here too: it keeps its soft meter gate but has NO LiteLLM backstop.
		// ADR-004 records this as a Known limitation.
		return nil
	}
	var maxPrice float64
	for _, name := range ladder {
		if p := catalog[name].PricePerToken(); p > maxPrice {
			maxPrice = p
		}
	}
	v := float64(cost) + float64(tokens)*maxPrice
	return &v
}

type KeyInfo struct {
	Alias     string
	Token     string // the secret. Handed straight to a KeySink; never logged, never returned by the API.
	ExpiresAt time.Time
}

// Admin is LiteLLM's key-management API.
type Admin interface {
	EnsureKey(ctx context.Context, spec KeySpec) (KeyInfo, error)
	RotateKey(ctx context.Context, spec KeySpec) (KeyInfo, error)
	DeleteKey(ctx context.Context, alias string) error
}
