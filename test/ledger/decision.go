package ledger

import (
	"context"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// AssertRan asserts a /decide answered `run` and handed out a WELL-FORMED
// execution grant: a reservation to settle and a complete attribution identity.
// A `run` with no reservation is a leaked headroom; a `run` with unparseable
// metadata is spend that lands unattributed.
func AssertRan(t TB, d meterapi.DecideResponse) {
	t.Helper()
	if d.Decision != meterapi.DecisionRun {
		t.Fatalf("AssertRan: decision = %q, want %q (reason %q: %s)", d.Decision, meterapi.DecisionRun, d.Reason, d.Detail)
	}
	if d.ReservationID == "" {
		t.Fatalf("AssertRan: a `run` decision handed out no reservation_id -- nothing to settle")
	}
	if len(d.Metadata) == 0 {
		t.Fatalf("AssertRan: a `run` decision carried no attribution metadata")
	}
	if _, err := atags.FromMetadata(d.Metadata); err != nil {
		t.Fatalf("AssertRan: decision metadata does not attribute: %v", err)
	}
}

// AssertDeferred asserts a /decide answered `defer` -- a FIRST-CLASS SUCCESS
// (AD-3): quiet hours, an exhausted budget, or a full reservation table are all
// correct refusals, not errors. It fails closed: a defer must carry a
// retry_after (a defer with none is an infinite park), and must hand out neither
// a reservation nor an attribution identity. If reason is non-empty it must
// match d.Reason (a meterapi.Reason* value).
func AssertDeferred(t TB, d meterapi.DecideResponse, reason string) {
	t.Helper()
	if d.Decision != meterapi.DecisionDefer {
		t.Fatalf("AssertDeferred: decision = %q, want %q (reason %q)", d.Decision, meterapi.DecisionDefer, d.Reason)
	}
	if reason != "" && d.Reason != reason {
		t.Fatalf("AssertDeferred: reason = %q, want %q", d.Reason, reason)
	}
	if d.RetryAfter.IsZero() {
		t.Fatalf("AssertDeferred: defer carries no retry_after -- an infinite park")
	}
	assertNoGrant(t, "AssertDeferred", d)
}

// AssertDenied asserts a /decide answered `deny` -- also a first-class success
// (a disabled project, an invalid config, a disallowed action). It fails closed:
// no reservation, no attribution identity. If reason is non-empty it must match.
func AssertDenied(t TB, d meterapi.DecideResponse, reason string) {
	t.Helper()
	if d.Decision != meterapi.DecisionDeny {
		t.Fatalf("AssertDenied: decision = %q, want %q (reason %q)", d.Decision, meterapi.DecisionDeny, d.Reason)
	}
	if reason != "" && d.Reason != reason {
		t.Fatalf("AssertDenied: reason = %q, want %q", d.Reason, reason)
	}
	assertNoGrant(t, "AssertDenied", d)
}

// assertNoGrant is the fail-closed core: a decision that is not going to run
// hands out neither a reservation nor an attribution identity (DecideResponse's
// own contract). Either one leaking is a route to unmetered spend.
func assertNoGrant(t TB, who string, d meterapi.DecideResponse) {
	t.Helper()
	if d.ReservationID != "" {
		t.Fatalf("%s: a non-run decision leaked reservation_id %q", who, d.ReservationID)
	}
	if len(d.Metadata) != 0 {
		t.Fatalf("%s: a non-run decision leaked attribution metadata %v", who, d.Metadata)
	}
}

// AssertProjectConserved is the money-conservation check: real spend never
// exceeds the ceiling, and remaining reconciles to ceiling minus spend. It reads
// meter's project cost (which carries both Budget and Remaining) and proves the
// ledger's own accounting closes. Unlimited ceilings (nil in the wire Budget)
// are skipped: there is nothing to conserve against +Inf.
func AssertProjectConserved(t TB, v Views, project string) {
	t.Helper()
	if v.Cost == nil {
		t.Fatalf("AssertProjectConserved: Views.Cost is nil")
	}
	pc, err := v.Cost.ProjectCost(context.Background(), project)
	if err != nil {
		t.Fatalf("AssertProjectConserved: project cost %q: %v", project, err)
	}
	if pc.CostUSD < 0 {
		t.Fatalf("AssertProjectConserved: project %q has NEGATIVE real spend %.12f", project, pc.CostUSD)
	}
	if pc.Budget.MonthlyCostUSD != nil {
		ceiling := *pc.Budget.MonthlyCostUSD
		if pc.CostUSD > ceiling+USDEpsilon {
			t.Fatalf("OVERSPEND: project %q spent $%.12f against a $%.12f ceiling", project, pc.CostUSD, ceiling)
		}
		if pc.Remaining.MonthlyCostUSD != nil {
			AssertUSD(t, "project "+project+" remaining reconciles (ceiling - spend)",
				*pc.Remaining.MonthlyCostUSD, ceiling-pc.CostUSD)
		}
	}
	if pc.Budget.MonthlyTokens != nil {
		ceiling := *pc.Budget.MonthlyTokens
		if pc.TotalTokens > ceiling {
			t.Fatalf("OVERSPEND: project %q used %d tokens against a %d ceiling", project, pc.TotalTokens, ceiling)
		}
		if pc.Remaining.MonthlyTokens != nil && *pc.Remaining.MonthlyTokens != ceiling-pc.TotalTokens {
			t.Fatalf("project %q token remaining %d does not reconcile to ceiling(%d) - used(%d)",
				project, *pc.Remaining.MonthlyTokens, ceiling, pc.TotalTokens)
		}
	}
}
