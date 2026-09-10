//go:build component

package component_test

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// maxSpendStaleness mirrors l2OperatorYAML's meter.max_spend_staleness: a
// spend-log lag at or above this is the real failure -- it stalls the factory
// permanently while it believes it is being careful (P3-3). It is the true
// invariant and the reason this test exists. It is deliberately NOT this test's
// failure line: a per-sample poll deadline of 5m, times a sample count large
// enough for the statistics to mean anything, would budget the whole package's
// 10m timeout to this one test. spendLagBudget below is the line, and it is
// strictly tighter, so the invariant is still enforced -- see there.
const maxSpendStaleness = 5 * time.Minute

// spendLagBudget is BOTH the per-sample poll deadline and the ONLY failure line
// of this test. Because it is strictly below maxSpendStaleness, any lag that
// would actually stall the factory (>= 5m) trips this budget first: a second
// assertion comparing the observed max against maxSpendStaleness could never
// fire, so there isn't one.
//
// Where 45s comes from -- measurement of this path, not the meter's loop
// intervals. (An earlier version of this comment derived a 2m margin from
// cmd/gonk-meter's 30s default GONK_SYNC_SPEND_INTERVAL, calling it "4 full
// spend-sync cycles of buffer". That derivation was void here: the component
// suite starts the meter with GONK_SYNC_SPEND_INTERVAL=1h and drives SyncSpend
// explicitly for determinism -- see meterProc.spawn in meter_test.go -- so no
// spend-sync cycle of any length elapses during this test, and this test does
// not start a meter at all.)
//
//   - docs/spikes/2026-09-10-spend-log-lag.md measured 0.24s-2.1s into
//     Postgres over two samples, with /spend/logs/v2 -- the endpoint meter
//     polls, and the one sampled here -- trailing the table by ~17ms.
//   - The structural ceiling is LiteLLM's spend-log queue monitor: it polls
//     every SPEND_LOG_QUEUE_POLL_INTERVAL (2s) and backs off 1.5x, capped at
//     30s, while the queue stays under its size threshold. On an otherwise idle
//     proxy a flush can therefore be up to ~30s late.
//   - Twenty samples show that backoff happening, which two could not: a local
//     run measured min=845ms median=5.916s p95=6.142s max=6.213s -- an order of
//     magnitude above the spike's two-sample floor, and settled well below the
//     30s ceiling.
//   - 45s is 1.5x that structural ceiling and ~7x the largest lag yet observed,
//     while still sitting 6.7x inside maxSpendStaleness.
//
// The 100s that preceded this was an arbitrary "large fraction of
// max_spend_staleness"; the 3m that replaced it was chosen when the lag was
// believed to be minutes. It is not: the apparent failures were test/stubmodel
// rewinding its completion-id counter, so LiteLLM's ON CONFLICT DO NOTHING
// silently discarded rows that were never written (same spike). That is fixed.
const spendLagBudget = 45 * time.Second

// lagSamples is the sample count docs/superpowers/plans/2026-07-13-plan-06-e2e-harness.md
// asked for ("Repeat 20x; report min/median/p95/max"). It was cut to 2 while a
// sample was believed to cost minutes; a sample costs single-digit seconds, so
// the whole loop measured ~95s locally and 20 is affordable again.
//
// 20 is also the smallest n at which a nearest-rank p95 is not simply the
// maximum (ceil(0.95*20) = 19, the 19th of 20), i.e. the smallest n at which
// every statistic printed below carries information the others do not.
const lagSamples = 20

// lagMeasureBudget bounds the measurement loop's wall clock, so this test's
// worst case is lagMeasureBudget + spendLagBudget (4m45s) rather than
// lagSamples * spendLagBudget (15m) -- the latter would blow the package's
// default 10m timeout, and a timeout panic reports no measurement at all. The
// loop starts a new sample only while the budget is unspent, so on any run that
// passes at least floor(4m/45s) = 5 samples completed; median is meaningful
// from n=3 up, and p95 is suppressed below n=20 rather than printed as a
// synonym for max. The budget is deliberately not raised to protect p95 against
// a slower rig: losing p95 costs information, whereas overrunning the package
// timeout costs the whole run, output included.
const lagMeasureBudget = 4 * time.Minute

// nearestRank returns the nearest-rank percentile of an ASCENDING-sorted slice:
// the ceil(p*n)-th smallest sample. No interpolation, so the value returned is
// always one that was actually observed.
func nearestRank(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// medianDuration returns the median of an ASCENDING-sorted slice, averaging the
// two middle samples when n is even.
func medianDuration(sorted []time.Duration) time.Duration {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// TestMeasureLiteLLMSpendLogLag (P3-3) MEASURES the real spend-log lag: the
// delay between a completion and its row becoming visible in /spend/logs/v2,
// which is the endpoint meter polls. The measurement, tracked over time, is the
// value this test provides regardless of pass/fail, so the summary is emitted
// from a defer -- t.Fatalf unwinds through it, and the run that fails is
// precisely the run whose numbers matter most.
//
// It has exactly one assertion of its own: no single sample may take longer
// than spendLagBudget for its row to appear. See spendLagBudget for why that is
// the line and why there is no separate assertion against maxSpendStaleness.
func TestMeasureLiteLLMSpendLogLag(t *testing.T) {
	w := newWorld(t)
	w.Stub.SetScript(alwaysAnswer(1000, 0))
	key := w.LiteLLM.ProvisionKey(t, "gonk-lag-"+harnessShortID(), nil, []string{"stub-glm"})
	ctx := context.Background()

	lags := make([]time.Duration, 0, lagSamples)
	defer func() {
		if len(lags) == 0 {
			t.Logf("SPEND-LOG LAG: no sample completed -- nothing was measured")
			return
		}
		s := append([]time.Duration(nil), lags...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		n := len(s)
		parts := []string{fmt.Sprintf("min=%s", s[0].Round(time.Millisecond))}
		if n >= 3 {
			parts = append(parts, "median="+medianDuration(s).Round(time.Millisecond).String())
		} else {
			parts = append(parts, "median=n/a (n<3)")
		}
		if n >= lagSamples {
			parts = append(parts, "p95="+nearestRank(s, 0.95).Round(time.Millisecond).String())
		} else {
			parts = append(parts, fmt.Sprintf("p95=n/a (n<%d: nearest-rank p95 would just be max)", lagSamples))
		}
		parts = append(parts, "max="+s[n-1].Round(time.Millisecond).String())
		t.Logf("SPEND-LOG LAG (n=%d): %s -- track this over time in docs/spikes/litellm-verified.md",
			n, strings.Join(parts, " "))
	}()

	loopStart := time.Now()
	for i := 0; i < lagSamples; i++ {
		if elapsed := time.Since(loopStart); elapsed >= lagMeasureBudget {
			t.Logf("stopping at %d samples: the %s measurement budget is spent (%s elapsed)",
				len(lags), lagMeasureBudget, elapsed.Round(time.Second))
			break
		}
		project := "acme/lag-" + harnessShortID()
		start := time.Now().Add(-2 * time.Hour)
		t0 := time.Now()
		if code := w.LiteLLM.CallDirect(t, key, "stub-glm", map[string]string{atags.KeyProject: project}); code != 200 {
			t.Fatalf("lag call %d = %d, want 200", i+1, code)
		}
		var lag time.Duration
		deadline := t0.Add(spendLagBudget)
		for {
			rows, _, err := w.LiteLLM.Spend.Since(ctx, start)
			if err != nil {
				t.Fatalf("spend poll (sample %d): %v", i+1, err)
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
				t.Fatalf("sample %d: the spend-log row for %s did NOT appear in /spend/logs/v2 within %s "+
					"(spendLagBudget). Measured lag on this rig is under 7s and LiteLLM's own queue "+
					"monitor cannot back off past ~30s, so this is a real change in the spend-log write "+
					"path, not a harness cutoff. meter polls this endpoint; at max_spend_staleness (%s) "+
					"every budgeted project defers spend-data-stale and the factory stalls (P3-3).",
					i+1, project, spendLagBudget, maxSpendStaleness)
			}
			time.Sleep(200 * time.Millisecond)
		}
		lags = append(lags, lag)
		t.Logf("lag sample %d/%d: %s", i+1, lagSamples, lag.Round(time.Millisecond))
	}
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
