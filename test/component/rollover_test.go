//go:build component

package component_test

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// lagCapPerCall bounds how long a single lag sample waits before we call the
// spend-log write "too late to measure honestly here". It is intentionally a
// large fraction of max_spend_staleness (5m) so a genuinely severe lag is caught,
// while keeping the whole test well under the suite timeout.
const lagCapPerCall = 100 * time.Second

// maxSpendStaleness mirrors l2OperatorYAML's meter.max_spend_staleness. The lag
// test fails if p95 exceeds it: a spend-log lag above the staleness budget stalls
// the factory permanently while it believes it is being careful (P3-3).
const maxSpendStaleness = 5 * time.Minute

// TestMeasureLiteLLMSpendLogLag (P3-3) MEASURES the real spend-log lag: the delay
// between a completion and its row becoming visible in /spend/logs/v2 (which
// meter polls). Plan 03's max_spend_staleness is a GUESS; if it is below the real
// lag the factory stalls. The measured numbers are WRITTEN into
// docs/spikes/litellm-verified.md -- a number nobody wrote down is a number
// nobody will believe in six months.
func TestMeasureLiteLLMSpendLogLag(t *testing.T) {
	w := newWorld(t)
	w.Stub.SetScript(alwaysAnswer(1000, 0))
	key := w.LiteLLM.ProvisionKey(t, "gonk-lag-"+harnessShortID(), nil, []string{"stub-glm"})
	ctx := context.Background()

	const samples = 6
	lags := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		project := "acme/lag-" + harnessShortID()
		start := time.Now().Add(-2 * time.Hour)
		t0 := time.Now()
		if code := w.LiteLLM.CallDirect(t, key, "stub-glm", map[string]string{atags.KeyProject: project}); code != 200 {
			t.Fatalf("lag call %d = %d, want 200", i, code)
		}
		var lag time.Duration
		deadline := time.Now().Add(lagCapPerCall)
		for {
			rows, _, err := w.LiteLLM.Spend.Since(ctx, start)
			if err != nil {
				t.Fatalf("spend poll: %v", err)
			}
			found := false
			for _, r := range rows {
				if r.Tags.Project == project {
					found = true
					break
				}
			}
			if found {
				lag = time.Since(t0)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("spend-log row for %s did NOT appear within %s -- the spend-log write lag "+
					"exceeds a large fraction of max_spend_staleness (%s). This is a real P3-3 config risk; "+
					"record it in litellm-verified.md.", project, lagCapPerCall, maxSpendStaleness)
			}
			time.Sleep(200 * time.Millisecond)
		}
		lags = append(lags, lag)
		t.Logf("lag sample %d: %s", i+1, lag.Round(time.Millisecond))
	}

	sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
	n := len(lags)
	min, median := lags[0], lags[n/2]
	p95, max := lags[min95Idx(n)], lags[n-1]
	t.Logf("SPEND-LOG LAG: min=%s median=%s p95=%s max=%s (record these in litellm-verified.md)",
		min.Round(time.Millisecond), median.Round(time.Millisecond), p95.Round(time.Millisecond), max.Round(time.Millisecond))
	if p95 > maxSpendStaleness {
		t.Fatalf("spend-log p95 lag %s EXCEEDS max_spend_staleness %s -- the factory would stall. "+
			"This is a real config finding, not a harness bug.", p95, maxSpendStaleness)
	}
}

func min95Idx(n int) int {
	idx := int(0.95 * float64(n-1))
	if idx >= n {
		idx = n - 1
	}
	return idx
}

// TestLiteLLMBudgetDurationIsACalendarMonth (P3-2). Meter's budget month is the
// UTC CALENDAR month (Decision 6). LiteLLM's virtual key uses budget_duration
// "1mo" (AD-9). ARE THOSE THE SAME BOUNDARY? If "1mo" were a ROLLING 30 DAYS, the
// soft and hard doors would reset on different days -- a window where LiteLLM
// permits work meter has already stopped counting. The spike found it IS a
// calendar month; this locks it.
func TestLiteLLMBudgetDurationIsACalendarMonth(t *testing.T) {
	w := newWorld(t)
	key := w.LiteLLM.ProvisionKey(t, "gonk-cal-"+harnessShortID(), ptr(5.00), []string{"stub-glm"})
	ki := w.LiteLLM.KeyInfo(t, key)
	if ki.BudgetResetAt.IsZero() {
		t.Fatal("no budget_reset_at returned -- cannot verify the boundary")
	}
	now := time.Now().UTC()
	wantReset := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	delta := ki.BudgetResetAt.Sub(wantReset)
	if delta < -time.Minute || delta > time.Minute {
		t.Fatalf("budget_reset_at = %s, want first-of-next-month UTC %s (delta %s). If this is ~30 days "+
			"from creation instead, budget_duration is a ROLLING window, not a calendar month -- a PRODUCT "+
			"DEFECT (report it; do not adjust the harness).", ki.BudgetResetAt, wantReset, delta)
	}
	t.Logf("budget_duration \"1mo\" resets at %s (UTC calendar-month boundary, delta %s)", ki.BudgetResetAt, delta.Round(time.Second))
}

// TestBackwardsClockDoesNotResetSpend (Decision 6). A backwards clock jump (NTP
// correction, VM restore) must NEVER move the budget window backward, because an
// early rollover is free budget. Advance the meter's window forward, then jump
// the clock back, and require the persisted window start to STAY put -- the
// monotone guarantee of spend.Advance, through the real binary and real store.
func TestBackwardsClockDoesNotResetSpend(t *testing.T) {
	w := newWorld(t)
	m := w.startMeter(meterOpts{})

	base := parseGauge(t, m.Metrics(), "gonk_meter_budget_window_start_seconds")

	// Move the clock ~70 days forward (across at least one month boundary) and
	// sync: the window MUST advance.
	m.AdvanceClock(70 * 24 * time.Hour)
	m.SyncSpend()
	forward := parseGauge(t, m.Metrics(), "gonk_meter_budget_window_start_seconds")
	if !(forward > base) {
		t.Fatalf("window start did not advance on a +70d clock move (base=%v forward=%v) -- test is vacuous", base, forward)
	}

	// Now jump the clock all the way back to the origin and sync. The window MUST
	// NOT retreat: a backwards clock does not reset spend.
	m.SetClockOffset(0)
	m.SyncSpend()
	after := parseGauge(t, m.Metrics(), "gonk_meter_budget_window_start_seconds")
	if after != forward {
		t.Fatalf("backwards clock RESET the budget window: was %v, became %v. An early rollover is free "+
			"budget (Decision 6). spend.Advance must be monotone.", forward, after)
	}
	t.Logf("window start held at %v across a backwards clock jump", after)
}

// TestClockSkewMakesEveryBudgetedProjectDefer (Decision 7). Skew is a HEALTH
// problem, not a math problem -- injected by the Date-rewriting SkewProxy, never
// by touching the system clock. Past max_clock_skew: /readyz is not ready, every
// FINITE-ceiling project defers spend-data-stale, but an UNLIMITED project still
// runs (it has no budget to protect).
func TestClockSkewMakesEveryBudgetedProjectDefer(t *testing.T) {
	w := newWorld(t)
	skewURL, stop := w.LiteLLM.SkewProxy(t, 10*time.Minute) // > max_clock_skew (5m)
	defer stop()

	m := w.startMeter(meterOpts{LiteLLMURL: skewURL, SkipInitialSync: true})
	m.SyncSpend() // reads the skewed Date header -> skewOK false

	if code := m.Readyz(); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d under +10m skew, want 503", code)
	}

	// A budgeted project defers spend-data-stale.
	budgeted := "acme/skew-budgeted-" + harnessShortID()
	m.MustRegister(budgeted, 8001, gonkYMLBudgeted(1.00, "glm"))
	d, _ := m.Decide(decideReq(budgeted, "gk-skew-1", "s1"))
	if d.Decision != meterapi.DecisionDefer || d.Reason != meterapi.ReasonSpendStale {
		t.Fatalf("budgeted project under skew: decision %q reason %q, want defer/%s", d.Decision, d.Reason, meterapi.ReasonSpendStale)
	}

	// An unlimited project has nothing stale to protect and still runs.
	unlimited := "acme/skew-unlimited-" + harnessShortID()
	m.MustRegister(unlimited, 8002, gonkYMLUnlimited("qwen-local"))
	du, _ := m.Decide(decideReq(unlimited, "gk-skew-2", "s2"))
	if du.Decision != meterapi.DecisionRun {
		t.Fatalf("unlimited project under skew: decision %q reason %q, want run (no budget to protect)", du.Decision, du.Reason)
	}
	t.Logf("under +10m skew: /readyz 503, budgeted defers spend-data-stale, unlimited runs")
}
