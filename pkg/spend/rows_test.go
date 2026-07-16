package spend

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

// row mimics what the ingest path does: it sets Synthetic from the rung's kind.
// Here, "qwen-local" is the local rung, so its dollars are an accounting fiction
// (Decision 9) and must land in SyntheticCostUSD, never in CostUSD.
func row(callID, bead, session, rung string, cost float64, prompt, completion int64, ts string) Row {
	return Row{
		CallID: callID,
		Tags: atags.Tags{
			Project: "group/repo", Rig: "group-repo", BeadID: bead, SessionKey: session,
			Rung: rung, Attempt: 1, Trigger: atags.TriggerIssueTriage,
		},
		CostUSD: cost, PromptTokens: prompt, CompletionTokens: completion, At: at(ts),
		Synthetic: rung == "qwen-local",
	}
}

func rows() []Row {
	return []Row{
		row("c1", "gk-1", "s1", "qwen-local", 0, 10_000, 2_000, "2026-07-02T10:00:00Z"),
		row("c2", "gk-1", "s1", "qwen-local", 0, 5_000, 1_000, "2026-07-02T10:01:00Z"),
		row("c3", "gk-1", "s2", "glm", 0.40, 8_000, 1_500, "2026-07-05T10:00:00Z"),
		row("c4", "gk-2", "s3", "glm", 0.25, 4_000, 500, "2026-07-06T10:00:00Z"),
		// Last month: inside the bead's lifetime, OUTSIDE the budget window.
		row("c5", "gk-1", "s0", "qwen-local", 0, 90_000, 9_000, "2026-06-28T10:00:00Z"),
	}
}

func TestProjectTotalsRespectsTheWindow(t *testing.T) {
	w := MonthWindow(at("2026-07-13T00:00:00Z"))
	got := ProjectTotals(rows(), w, "group/repo")
	// c1..c4 only: c5 is June.
	if got.CostUSD != 0.65 {
		t.Errorf("cost = %v, want 0.65", got.CostUSD)
	}
	// c1 12k + c2 6k + c3 9.5k + c4 4.5k = 32k. (c5 is June: out of the window.)
	if got.TotalTokens() != 32_000 {
		t.Errorf("tokens = %d, want 32000", got.TotalTokens())
	}
	if got.Calls != 4 {
		t.Errorf("calls = %d, want 4", got.Calls)
	}
}

// The per-task ceiling is a property of the WORK ITEM, not the month: a bead
// that straddles a month boundary keeps spending against the same per-task
// budget. So bead totals are lifetime, not windowed.
func TestBeadTotalsAreLifetime(t *testing.T) {
	got := BeadTotals(rows(), "group/repo", "gk-1")
	if got.TotalTokens() != 126_500 { // includes June's c5
		t.Fatalf("bead tokens = %d, want 126500 (June's row must count)", got.TotalTokens())
	}
	if got.CostUSD != 0.40 {
		t.Fatalf("bead cost = %v, want 0.40", got.CostUSD)
	}
}

func TestSessionTotals(t *testing.T) {
	got := SessionTotals(rows(), "s1")
	if got.Calls != 2 || got.TotalTokens() != 18_000 {
		t.Fatalf("session s1 = %+v", got)
	}
}

// Spend logs are polled with overlapping windows, so the same call WILL be
// read twice. Double-counting is a wrong budget.
func TestDedupeByCallID(t *testing.T) {
	dup := append(rows(), row("c3", "gk-1", "s2", "glm", 0.40, 8_000, 1_500, "2026-07-05T10:00:00Z"))
	deduped, dropped := Dedupe(dup)
	if dropped != 1 || len(deduped) != len(rows()) {
		t.Fatalf("Dedupe dropped %d, len %d", dropped, len(deduped))
	}
	w := MonthWindow(at("2026-07-13T00:00:00Z"))
	if got := ProjectTotals(deduped, w, "group/repo"); got.CostUSD != 0.65 {
		t.Fatalf("cost after dedupe = %v, want 0.65", got.CostUSD)
	}
}

func TestByRungAndByTrigger(t *testing.T) {
	w := MonthWindow(at("2026-07-13T00:00:00Z"))
	byRung := ByRung(rows(), w, "group/repo")
	if byRung["glm"].CostUSD != 0.65 || byRung["qwen-local"].CostUSD != 0 {
		t.Fatalf("by rung = %+v", byRung)
	}
	if byRung["qwen-local"].TotalTokens() != 18_000 {
		t.Fatalf("local rungs cost no REAL money but MUST still be metered in tokens (spec 6.3): %+v", byRung["qwen-local"])
	}
	if got := ByTrigger(rows(), w, "group/repo")[atags.TriggerIssueTriage].Calls; got != 4 {
		t.Fatalf("by trigger calls = %d, want 4", got)
	}
}

// DECISION 9. A local rung's LiteLLM `spend` is a SYNTHETIC dollar -- priced only
// so that the USD virtual-key ceiling is a hard door for tokens. It must land in
// SyntheticCostUSD and NEVER in CostUSD, or it flows into rung.Decide's cost gate
// and bricks every project whose monthly_cost_usd is 0 (i.e. every freshly
// onboarded one).
func TestSyntheticSpendIsNeverCountedAsRealSpend(t *testing.T) {
	w := MonthWindow(at("2026-07-13T00:00:00Z"))
	// A local row that LiteLLM billed at a synthetic $2.50.
	rs := append(rows(), row("c6", "gk-3", "s4", "qwen-local", 2.50, 50_000, 5_000, "2026-07-07T10:00:00Z"))

	got := ProjectTotals(rs, w, "group/repo")
	if got.CostUSD != 0.65 {
		t.Fatalf("real cost = %v, want 0.65: a synthetic local-rung dollar was counted as real spend", got.CostUSD)
	}
	if got.SyntheticCostUSD != 2.50 {
		t.Fatalf("synthetic cost = %v, want 2.50 (it must be reported, just never as spend)", got.SyntheticCostUSD)
	}
	if byRung := ByRung(rs, w, "group/repo"); byRung["qwen-local"].CostUSD != 0 ||
		byRung["qwen-local"].SyntheticCostUSD != 2.50 {
		t.Fatalf("qwen-local = %+v; local dollars are synthetic, and the split must survive grouping", byRung["qwen-local"])
	}
}

// THE MONTH-ROLLOVER MONEY BUG. Spend rows are POLLED (Decision 11), so a row can
// land days after the call it describes -- including after the budget window has
// rolled. Rows must be windowed by the ROW'S OWN TIMESTAMP, never by ingest time.
//
// Get this wrong and, on the 1st of every month, July's late-arriving rows are
// charged to August -- or worse, July's spend is never charged at all and every
// project silently gets a free budget.
func TestALateRowIsChargedToTheMonthItHappenedIn(t *testing.T) {
	// It is now 1 August. This row is for a call made on 31 July, and it has only
	// just landed.
	late := row("c9", "gk-9", "s9", "glm", 5.00, 10_000, 1_000, "2026-07-31T23:59:00Z")

	august := MonthWindow(at("2026-08-01T00:30:00Z"))
	if got := ProjectTotals([]Row{late}, august, "group/repo"); got.CostUSD != 0 {
		t.Fatalf("a July call was charged $%.2f against AUGUST's budget", got.CostUSD)
	}
	july := MonthWindow(at("2026-07-15T00:00:00Z"))
	if got := ProjectTotals([]Row{late}, july, "group/repo"); got.CostUSD != 5.00 {
		t.Fatalf("a late-landing July call was charged $%.2f against July, want $5.00; July's spend has vanished", got.CostUSD)
	}
}
