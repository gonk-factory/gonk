package ledger_test

import (
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

func ranDecision() meterapi.DecideResponse {
	return meterapi.DecideResponse{
		Decision:      meterapi.DecisionRun,
		Rung:          "qwen-local",
		Attempt:       1,
		ReservationID: "resv-1",
		Metadata:      goodTags().Metadata(),
	}
}

// A run, a defer and a deny that are all well-formed must PASS -- a defer/deny is
// a first-class success (AD-3), not an error.
func TestDecisionAssertionsPassOnWellFormedDecisions(t *testing.T) {
	ledger.AssertRan(t, ranDecision())
	ledger.AssertDeferred(t, meterapi.DecideResponse{
		Decision:   meterapi.DecisionDefer,
		Reason:     meterapi.ReasonQuietHours,
		RetryAfter: time.Now().Add(time.Hour),
	}, meterapi.ReasonQuietHours)
	ledger.AssertDenied(t, meterapi.DecideResponse{
		Decision: meterapi.DecisionDeny,
		Reason:   meterapi.ReasonDisabled,
	}, meterapi.ReasonDisabled)
}

func TestAssertRanCatchesALeakedRunWithoutReservation(t *testing.T) {
	d := ranDecision()
	d.ReservationID = "" // a run that hands out no reservation to settle
	mustFail(t, "run-without-reservation", func(tb ledger.TB) { ledger.AssertRan(tb, d) })
}

func TestAssertDeferredCatchesAnInfinitePark(t *testing.T) {
	d := meterapi.DecideResponse{Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours}
	// RetryAfter is zero -> an infinite park.
	mustFail(t, "defer-no-retry-after", func(tb ledger.TB) { ledger.AssertDeferred(tb, d, "") })
}

func TestAssertDeferredCatchesALeakedGrant(t *testing.T) {
	d := meterapi.DecideResponse{
		Decision:      meterapi.DecisionDefer,
		RetryAfter:    time.Now().Add(time.Hour),
		ReservationID: "resv-leaked", // a refusal must hand out no reservation
	}
	mustFail(t, "defer-leaked-reservation", func(tb ledger.TB) { ledger.AssertDeferred(tb, d, "") })
}

func TestAssertDeniedCatchesALeakedIdentity(t *testing.T) {
	d := meterapi.DecideResponse{
		Decision: meterapi.DecisionDeny,
		Metadata: goodTags().Metadata(), // a deny must hand out no attribution identity
	}
	mustFail(t, "deny-leaked-metadata", func(tb ledger.TB) { ledger.AssertDenied(tb, d, "") })
}

func TestAssertDeniedCatchesAMislabelledRun(t *testing.T) {
	// A `run` masquerading through AssertDenied must be caught.
	mustFail(t, "run-not-deny", func(tb ledger.TB) { ledger.AssertDenied(tb, ranDecision(), "") })
}

// ---- money conservation ----

func conservedViews(spentUSD float64) ledger.Views {
	// One cloud row that spent spentUSD, against a $10 ceiling.
	tags := goodTags()
	tags.Rung = "glm"
	rows := []spend.Row{{CallID: "c1", Tags: tags, CostUSD: spentUSD, PromptTokens: 100, CompletionTokens: 200, Synthetic: false, At: at}}
	ceiling := 10.0
	remaining := ceiling - spentUSD
	tokCeiling := int64(1_000_000)
	tokRemaining := tokCeiling - 300
	cost := &fakeCost{
		rows:      rows,
		budget:    meterapi.Budget{MonthlyCostUSD: &ceiling, MonthlyTokens: &tokCeiling},
		remaining: meterapi.Budget{MonthlyCostUSD: &remaining, MonthlyTokens: &tokRemaining},
	}
	return ledger.Views{Stub: goodStub(), Spend: &fakeSpend{rows: rows}, Cost: cost, Catalog: catalog()}
}

func TestAssertProjectConservedPassesWhenTheLedgerCloses(t *testing.T) {
	ledger.AssertProjectConserved(t, conservedViews(3.50), "acme/widget")
}

func TestAssertProjectConservedCatchesOverspend(t *testing.T) {
	// Spent $12 against a $10 ceiling: overspend. (Remaining would go negative;
	// the ceiling check fires first.)
	mustFail(t, "overspend", func(tb ledger.TB) {
		ledger.AssertProjectConserved(tb, conservedViews(12.0), "acme/widget")
	})
}

func TestAssertProjectConservedCatchesAnUnreconciledRemaining(t *testing.T) {
	// Spend $3, but meter claims $9 remaining against a $10 ceiling: the ledger's
	// own accounting does not close ($10 - $3 != $9).
	ceiling, remaining := 10.0, 9.0
	tags := goodTags()
	tags.Rung = "glm"
	rows := []spend.Row{{CallID: "c1", Tags: tags, CostUSD: 3.0, PromptTokens: 1, CompletionTokens: 1, At: at}}
	cost := &fakeCost{rows: rows, budget: meterapi.Budget{MonthlyCostUSD: &ceiling}, remaining: meterapi.Budget{MonthlyCostUSD: &remaining}}
	v := ledger.Views{Stub: goodStub(), Spend: &fakeSpend{rows: rows}, Cost: cost, Catalog: catalog()}
	mustFail(t, "unreconciled-remaining", func(tb ledger.TB) {
		ledger.AssertProjectConserved(tb, v, "acme/widget")
	})
}
