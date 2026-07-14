package budget

import "math"

// Spend is consumption within one budget window: what LiteLLM's spend log has
// already reported, plus what open reservations have committed us to but not
// yet billed. Reservations are what make the check survive spend-log lag and
// concurrent sessions (ADR-004).
//
// CostUSD IS REAL MONEY AND ONLY REAL MONEY. Local rungs are priced
// synthetically in LiteLLM so that its USD virtual-key ceiling is a hard door
// for TOKENS too (Decision 9) -- but those synthetic dollars are an accounting
// unit, not spend, and they are tracked SEPARATELY here. They must never reach
// the cost gate.
//
// The reason is not fastidiousness, it is the onboarding default. The .gonk.yml
// the onboarding MR ships carries `monthly_cost_usd: 0` and `ladder:
// [qwen-local]` (spec 5.3). If a local rung's synthetic dollars counted against
// monthly_cost_usd, that project could not afford its own only rung and would be
// DEAD ON ARRIVAL. So: budget.Remain computes remaining cost from CostUSD alone,
// rung.Decide's cost gate reads it, and `monthly_cost_usd: 0` keeps meaning
// exactly what it has always meant -- "local rungs run freely, a cloud rung is
// never affordable".
type Spend struct {
	// Real money. Cloud rungs only.
	CostUSD         float64 `json:"cost_usd"`
	ReservedCostUSD float64 `json:"reserved_cost_usd"`

	// Synthetic money. Local rungs only. REPORTING AND THE LITELLM KEY CEILING
	// ONLY -- never a policy input. Nothing in Remain() or Fits*() reads these.
	SyntheticCostUSD         float64 `json:"synthetic_cost_usd"`
	ReservedSyntheticCostUSD float64 `json:"reserved_synthetic_cost_usd"`

	Tokens             int64 `json:"tokens"`
	TaskTokens         int64 `json:"task_tokens"` // this bead only, all attempts, all time
	ReservedTokens     int64 `json:"reserved_tokens"`
	ReservedTaskTokens int64 `json:"reserved_task_tokens"`
}

// Remaining is ceiling - (observed + reserved), clamped at zero. Unlimited
// ceilings stay unlimited: +Inf - x is +Inf for free, but MaxInt64 - x is NOT
// MaxInt64, so the token fields must special-case it or an "unlimited" project
// would slowly acquire a limit.
type Remaining struct {
	MonthlyCostUSD CostLimit  `json:"monthly_cost_usd"`
	MonthlyTokens  TokenLimit `json:"monthly_tokens"`
	PerTaskTokens  TokenLimit `json:"per_task_tokens"`
}

func Remain(b Budget, s Spend) Remaining {
	return Remaining{
		MonthlyCostUSD: remainCost(b.MonthlyCostUSD, s.CostUSD, s.ReservedCostUSD),
		MonthlyTokens:  remainTokens(b.MonthlyTokens, s.Tokens, s.ReservedTokens),
		PerTaskTokens:  remainTokens(b.PerTaskTokens, s.TaskTokens, s.ReservedTaskTokens),
	}
}

func remainCost(ceiling CostLimit, observed, reserved float64) CostLimit {
	if ceiling.Unlimited() {
		return Unlimited
	}
	used := observed + reserved
	// Fail closed on garbage: a NaN loses every comparison, so a NaN remaining
	// would read as "affordable" at every call site.
	if math.IsNaN(used) || math.IsInf(used, 1) || math.IsNaN(float64(ceiling)) {
		return 0
	}
	// A NEGATIVE spend must not RAISE the ceiling. LiteLLM rows are external,
	// unvalidated input, and a credit/refund/adjustment row with a negative
	// `spend` would otherwise hand the project extra headroom -- the one place
	// this arithmetic could fail open.
	//
	// Note this clamps to "nothing spent" while remainTokens (via addSat) clamps
	// a negative token count to "fully spent". That asymmetry is deliberate: a
	// negative dollar amount is a MEANINGFUL row (a credit), so we neither grant
	// the headroom nor punish the project for it; a negative TOKEN COUNT is
	// nonsense, so it can only be corruption, and corruption fails closed.
	if used < 0 {
		used = 0
	}
	if r := float64(ceiling) - used; r > 0 {
		return CostLimit(r)
	}
	return 0
}

func remainTokens(ceiling TokenLimit, observed, reserved int64) TokenLimit {
	if ceiling.Unlimited() {
		return UnlimitedTokens
	}
	used := addSat(observed, reserved)
	if r := int64(ceiling) - used; r > 0 {
		return TokenLimit(r)
	}
	return 0
}

// addSat saturates at MaxInt64 instead of wrapping to a negative number, which
// would read as "nothing has been spent".
func addSat(a, b int64) int64 {
	if a < 0 || b < 0 {
		return math.MaxInt64 // nonsense input: treat as fully spent
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// FitsCost reports whether a rung whose estimated cost is est can be started.
// est is 0 only for local rungs, which are free by definition and skip this
// check entirely (rung.Decide never calls FitsCost for them) -- so a caller
// reaching here with est == 0 is asking about a PRICED rung with no price, and
// the honest answer against a zero budget is no.
func (r Remaining) FitsCost(est float64) bool {
	if r.MonthlyCostUSD.Unlimited() {
		return true
	}
	if float64(r.MonthlyCostUSD) <= 0 {
		return false
	}
	return float64(r.MonthlyCostUSD) >= est
}

func (r Remaining) FitsMonthTokens(est int64) bool { return fits(r.MonthlyTokens, est) }
func (r Remaining) FitsTaskTokens(est int64) bool  { return fits(r.PerTaskTokens, est) }

func fits(rem TokenLimit, est int64) bool {
	if rem.Unlimited() {
		return true
	}
	if rem <= 0 {
		return false
	}
	return int64(rem) >= est
}
