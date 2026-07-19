package ledger

import (
	"context"
	"strconv"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

// Rung kinds, as the operator catalog (pkg/opercfg) classifies them.
const (
	KindLocal = "local"
	KindCloud = "cloud"
)

// StubLog is the first of the three views: the stub's own request log, the
// INDEPENDENT ground truth for how many model calls happened and what usage each
// reported. *stubmodel.Log satisfies it directly; the library's own tests supply
// a fake. (An interface rather than a bare *stubmodel.Log so a corrupted view is
// constructible in assert_test.go -- the plan mandates that self-check, and a
// populated *stubmodel.Log cannot be built outside its package.)
type StubLog interface {
	Calls() []stubmodel.Call
	ScriptExhausted() bool
}

// SpendSource is the second view: LiteLLM's spend log, one row per call carrying
// attribution tags. At L1 a fake seeded from the stub; at L2/L3 a wrapper over
// GET /spend/logs/v2 (the date-bounded, paginated endpoint -- never the legacy
// /spend/logs that OOM-killed the live pod).
type SpendSource interface {
	Rows(ctx context.Context) ([]spend.Row, error)
}

// CostSource is the third view: meter's cost API, the join of spend -> bead ->
// GitLab artifact (spec 6.2.1). At L1 the in-proc service; at L2/L3 a client
// over HTTP against GET /v1/cost/bead/{id} and /v1/cost/project/{p}.
type CostSource interface {
	BeadCost(ctx context.Context, beadID string) (meterapi.BeadCostResponse, error)
	ProjectCost(ctx context.Context, project string) (meterapi.ProjectCostResponse, error)
}

// Views bundles the three sources plus the rung catalog. Each layer supplies
// them differently (L1 fakes, L2/L3 real containers), but the ASSERTION IS THE
// SAME at every layer -- that is what makes a scenario portable up the stack.
type Views struct {
	Stub    StubLog
	Spend   SpendSource
	Cost    CostSource        // optional: nil skips the meter-agreement leg (step 6)
	Catalog map[string]string // rung -> KindLocal | KindCloud, from pkg/opercfg
}

// Want is what a scenario claims the ledger should say for one (bead, attempt)
// slice. Every identity field is required: an assertion that omits Trigger is an
// assertion that would pass if attribution were broken, and pkg/atags exists
// precisely so that it cannot be.
type Want struct {
	Project    string
	Rig        string
	BeadID     string
	SessionKey string
	Rung       string
	Attempt    int
	Trigger    string // atags.Trigger* value

	Calls            int // model calls attributable to THIS (bead, attempt)
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64 // REAL dollars. Cloud rungs only; must be 0 on a local rung.
	SyntheticUSD     float64 // local rungs. NOT SPEND (Plan 03 Decision 9); must be 0 on a cloud rung.
}

// Attempt is one rung of the ladder, as meter's own attempt history records it.
type Attempt struct {
	N       int
	Rung    string
	Outcome string // success | gate-failed | infra-failed | aborted
}

// Assert is the whole point of this package: it checks that the three views
// agree, in order. Any single mismatch is fatal.
//
//  1. STUB <-> LITELLM: the number of spend rows equals the number of successful
//     stub calls, and the token totals match. A mismatch means a lost or
//     double-counted row -- the single worst thing a billing ledger can do.
//  2. NO UNATTRIBUTED ROWS: every spend row's tags validate (pkg/atags). A row
//     LiteLLM recorded that gonk cannot attribute is spend nobody is accountable
//     for.
//  3. CURRENCY FIREWALL: a row on a LOCAL rung is Synthetic (its dollars are an
//     accounting fiction); a row on a CLOUD rung is not. If these mix, the
//     onboarding default (monthly_cost_usd: 0 + a local-only ladder) can no
//     longer afford its own only rung.
//  4. EXACT ATTRIBUTION: for each Want, EXACTLY the rows with that
//     (project, rig, bead, session, rung, attempt, trigger) tuple exist, with
//     exactly those tokens and that cost. Not "at least" -- so a double-charge
//     fails.
//  5. NO EXTRA ROWS: the union of the Wants accounts for EVERY row. An
//     unaccounted-for row is unmetered spend.
//  6. METER AGREES: meter's cost API returns the same totals as the raw spend
//     log, per bead and per project (skipped when Views.Cost is nil).
//  7. SCRIPT NOT EXHAUSTED: the stub served no unscripted call. An unscripted
//     call is an uncontrolled cost.
func Assert(t TB, v Views, want []Want) {
	t.Helper()
	if v.Stub == nil {
		t.Fatalf("ledger.Assert: Views.Stub is nil -- the stub log is the independent ground truth and is required")
	}
	if v.Spend == nil {
		t.Fatalf("ledger.Assert: Views.Spend is nil")
	}
	if v.Catalog == nil {
		t.Fatalf("ledger.Assert: Views.Catalog is nil -- a row's currency cannot be judged without it")
	}
	ctx := context.Background()

	rows, err := v.Spend.Rows(ctx)
	if err != nil {
		t.Fatalf("ledger.Assert: read spend rows: %v", err)
	}

	// (1) STUB <-> LITELLM.
	var stubOK int
	var stubPrompt, stubCompletion int64
	for _, c := range v.Stub.Calls() {
		if c.Status >= 200 && c.Status < 300 {
			stubOK++
			stubPrompt += int64(c.Usage.PromptTokens)
			stubCompletion += int64(c.Usage.CompletionTokens)
		}
	}
	if len(rows) != stubOK {
		t.Fatalf("STUB<->LITELLM: %d spend rows but %d successful stub calls -- a row was lost or double-counted", len(rows), stubOK)
	}
	var rowPrompt, rowCompletion int64
	for _, r := range rows {
		rowPrompt += r.PromptTokens
		rowCompletion += r.CompletionTokens
	}
	if rowPrompt != stubPrompt || rowCompletion != stubCompletion {
		t.Fatalf("STUB<->LITELLM: tokens disagree: spend(prompt=%d,completion=%d) stub(prompt=%d,completion=%d)",
			rowPrompt, rowCompletion, stubPrompt, stubCompletion)
	}

	// (2) NO UNATTRIBUTED ROWS.
	for i, r := range rows {
		if err := r.Tags.Validate(); err != nil {
			t.Fatalf("UNATTRIBUTED: spend row %d (call %q) has no valid attribution: %v", i, r.CallID, err)
		}
	}

	// (3) CURRENCY FIREWALL (per row).
	for i, r := range rows {
		kind, ok := v.Catalog[r.Tags.Rung]
		if !ok {
			t.Fatalf("CURRENCY: spend row %d is on rung %q, which is not in the catalog -- fail closed rather than mis-currency it", i, r.Tags.Rung)
		}
		switch kind {
		case KindLocal:
			if !r.Synthetic {
				t.Fatalf("CURRENCY: row %d on LOCAL rung %q carries REAL dollars (Synthetic=false) -- synthetic dollars must never mix with spend", i, r.Tags.Rung)
			}
		case KindCloud:
			if r.Synthetic {
				t.Fatalf("CURRENCY: row %d on CLOUD rung %q is marked Synthetic -- real spend must not be hidden as fiction", i, r.Tags.Rung)
			}
		default:
			t.Fatalf("CURRENCY: catalog classifies rung %q as %q, not %q|%q", r.Tags.Rung, kind, KindLocal, KindCloud)
		}
	}

	// (4) EXACT ATTRIBUTION + (5) NO EXTRA ROWS.
	matched := make([]bool, len(rows))
	for wi, w := range want {
		kind, ok := v.Catalog[w.Rung]
		if !ok {
			t.Fatalf("ATTRIBUTION: want[%d] names rung %q, not in the catalog", wi, w.Rung)
		}
		var calls int
		var p, c int64
		var cost float64
		for i, r := range rows {
			if !matchWant(r, w) {
				continue
			}
			matched[i] = true
			calls++
			p += r.PromptTokens
			c += r.CompletionTokens
			cost += r.CostUSD
		}
		if calls != w.Calls {
			t.Fatalf("ATTRIBUTION: want[%d] %s: %d rows match the tuple, want exactly %d", wi, describe(w), calls, w.Calls)
		}
		if p != int64(w.PromptTokens) || c != int64(w.CompletionTokens) {
			t.Fatalf("ATTRIBUTION: want[%d] %s: tokens prompt=%d completion=%d, want prompt=%d completion=%d",
				wi, describe(w), p, c, w.PromptTokens, w.CompletionTokens)
		}
		switch kind {
		case KindLocal:
			if w.CostUSD != 0 {
				t.Fatalf("ATTRIBUTION: want[%d] %s is a LOCAL rung but sets CostUSD=%v -- local dollars belong in SyntheticUSD", wi, describe(w), w.CostUSD)
			}
			AssertUSD(t, "want["+describe(w)+"] synthetic cost", cost, w.SyntheticUSD)
		case KindCloud:
			if w.SyntheticUSD != 0 {
				t.Fatalf("ATTRIBUTION: want[%d] %s is a CLOUD rung but sets SyntheticUSD=%v -- cloud spend is real", wi, describe(w), w.SyntheticUSD)
			}
			AssertUSD(t, "want["+describe(w)+"] real cost", cost, w.CostUSD)
		}
	}
	for i := range rows {
		if !matched[i] {
			t.Fatalf("EXTRA ROW: spend row %d (call %q, bead %q, rung %q, attempt %d) is accounted for by no Want -- unmetered spend",
				i, rows[i].CallID, rows[i].Tags.BeadID, rows[i].Tags.Rung, rows[i].Tags.Attempt)
		}
	}

	// (6) METER AGREES.
	if v.Cost != nil {
		type pb struct{ project, bead string }
		beads := map[pb]bool{}
		projects := map[string]bool{}
		for _, w := range want {
			beads[pb{w.Project, w.BeadID}] = true
			projects[w.Project] = true
		}
		for k := range beads {
			bc, err := v.Cost.BeadCost(ctx, k.bead)
			if err != nil {
				t.Fatalf("METER: bead cost %q: %v", k.bead, err)
			}
			tot := spend.BeadTotals(rows, k.project, k.bead)
			AssertUSD(t, "meter bead "+k.bead+" cost_usd", bc.CostUSD, tot.CostUSD)
			AssertUSD(t, "meter bead "+k.bead+" synthetic_usd", bc.SyntheticCostUSD, tot.SyntheticCostUSD)
			if bc.PromptTokens != tot.PromptTokens || bc.CompletionTokens != tot.CompletionTokens {
				t.Fatalf("METER: bead %q tokens: meter(prompt=%d,completion=%d) spend(prompt=%d,completion=%d)",
					k.bead, bc.PromptTokens, bc.CompletionTokens, tot.PromptTokens, tot.CompletionTokens)
			}
		}
		for p := range projects {
			pc, err := v.Cost.ProjectCost(ctx, p)
			if err != nil {
				t.Fatalf("METER: project cost %q: %v", p, err)
			}
			tot := spend.Sum(rows, func(r spend.Row) bool { return r.Tags.Project == p })
			AssertUSD(t, "meter project "+p+" cost_usd", pc.CostUSD, tot.CostUSD)
			AssertUSD(t, "meter project "+p+" synthetic_usd", pc.SyntheticCostUSD, tot.SyntheticCostUSD)
			if pc.PromptTokens != tot.PromptTokens || pc.CompletionTokens != tot.CompletionTokens {
				t.Fatalf("METER: project %q tokens: meter(prompt=%d,completion=%d) spend(prompt=%d,completion=%d)",
					p, pc.PromptTokens, pc.CompletionTokens, tot.PromptTokens, tot.CompletionTokens)
			}
		}
	}

	// (7) SCRIPT NOT EXHAUSTED.
	if v.Stub.ScriptExhausted() {
		t.Fatalf("SCRIPT EXHAUSTED: the stub served a call the scenario did not script -- an uncontrolled cost")
	}
}

// AssertNoSpend is the fail-CLOSED assertion: after a refusal, a defer, a deny,
// or a killed component, there must be ZERO spend rows. Not "few". Zero.
func AssertNoSpend(t TB, v Views) {
	t.Helper()
	if v.Spend == nil {
		t.Fatalf("ledger.AssertNoSpend: Views.Spend is nil")
	}
	rows, err := v.Spend.Rows(context.Background())
	if err != nil {
		t.Fatalf("ledger.AssertNoSpend: read spend rows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("NO SPEND: expected ZERO rows on a fail-closed path, got %d (first: bead %q rung %q)",
			len(rows), rows[0].Tags.BeadID, rows[0].Tags.Rung)
	}
	if v.Stub != nil {
		for i, c := range v.Stub.Calls() {
			if c.Status >= 200 && c.Status < 300 && c.Usage.PromptTokens+c.Usage.CompletionTokens > 0 {
				t.Fatalf("NO SPEND: stub served a billable call (#%d, %d tokens) on a path that must not spend",
					i, c.Usage.PromptTokens+c.Usage.CompletionTokens)
			}
		}
		if v.Stub.ScriptExhausted() {
			t.Fatalf("NO SPEND: stub script exhausted on a path that must not call the model")
		}
	}
}

// AssertAttempts checks the LADDER from meter's own attempt history: the rungs,
// in order, with their outcomes. This is how "an infra failure never escalates"
// is asserted -- as data, not as a log line.
func AssertAttempts(t TB, v Views, beadID string, want []Attempt) {
	t.Helper()
	if v.Cost == nil {
		t.Fatalf("ledger.AssertAttempts: Views.Cost is nil -- attempt history comes from meter's bead cost API")
	}
	bc, err := v.Cost.BeadCost(context.Background(), beadID)
	if err != nil {
		t.Fatalf("ledger.AssertAttempts: bead cost %q: %v", beadID, err)
	}
	if len(bc.Attempts) != len(want) {
		t.Fatalf("ATTEMPTS: bead %q has %d attempts, want %d (%v)", beadID, len(bc.Attempts), len(want), bc.Attempts)
	}
	for i, w := range want {
		got := bc.Attempts[i]
		if got.Attempt != w.N || got.Rung != w.Rung || got.Outcome != w.Outcome {
			t.Fatalf("ATTEMPTS: bead %q attempt %d: got {n=%d rung=%q outcome=%q}, want {n=%d rung=%q outcome=%q}",
				beadID, i, got.Attempt, got.Rung, got.Outcome, w.N, w.Rung, w.Outcome)
		}
	}
}

func matchWant(r spend.Row, w Want) bool {
	return r.Tags.Project == w.Project &&
		r.Tags.Rig == w.Rig &&
		r.Tags.BeadID == w.BeadID &&
		r.Tags.SessionKey == w.SessionKey &&
		r.Tags.Rung == w.Rung &&
		r.Tags.Attempt == w.Attempt &&
		r.Tags.Trigger == w.Trigger
}

func describe(w Want) string {
	return w.Project + "/" + w.BeadID + "@" + w.Rung + "#" + strconv.Itoa(w.Attempt)
}
