package spend

import (
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

// Row is one LiteLLM spend-log entry, joined to its attribution tags. The tags
// ARE the join key: pkg/atags is the contract that connects a model call to a
// bead, a session, and a GitLab artifact (spec 6.1).
type Row struct {
	CallID           string     // LiteLLM request id; the dedupe key
	Tags             atags.Tags // gonk_project, gonk_bead_id, ... (spec 6.1)
	CostUSD          float64    // as LiteLLM billed it -- REAL for cloud, SYNTHETIC for local
	PromptTokens     int64
	CompletionTokens int64
	At               time.Time // UTC

	// Synthetic marks a row whose CostUSD is an accounting fiction: a local
	// model, priced in LiteLLM only so that its USD virtual-key ceiling is a hard
	// door for TOKENS (Decision 9).
	//
	// It is set AT INGEST, from the rung catalog: catalog[Tags.Rung].Kind ==
	// KindLocal. A row whose rung is NOT IN THE CATALOG is treated as REAL --
	// fail closed, so an unknown rung counts against the money ceiling rather
	// than being waved through as fiction.
	//
	// LiteLLM has one `spend` column and does not know the difference. This flag
	// is the ONLY thing that keeps synthetic dollars out of the cost gate and off
	// the "real spend" panel.
	Synthetic bool
}

// Totals is an aggregate over rows. CostUSD and SyntheticCostUSD are DIFFERENT
// CURRENCIES: never add them together, and never show them in one number.
type Totals struct {
	CostUSD          float64 `json:"cost_usd"`           // real money (cloud rungs)
	SyntheticCostUSD float64 `json:"synthetic_cost_usd"` // accounting fiction (local rungs)
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Calls            int     `json:"calls"`
}

func (t Totals) TotalTokens() int64 { return t.PromptTokens + t.CompletionTokens }

func (t *Totals) add(r Row) {
	if r.Synthetic {
		t.SyntheticCostUSD += r.CostUSD
	} else {
		t.CostUSD += r.CostUSD
	}
	t.PromptTokens += r.PromptTokens
	t.CompletionTokens += r.CompletionTokens
	t.Calls++
}

// Sum aggregates the rows that match.
func Sum(rows []Row, match func(Row) bool) Totals {
	var out Totals
	for _, r := range rows {
		if match(r) {
			out.add(r)
		}
	}
	return out
}

// ProjectTotals is spend against the MONTHLY ceilings: windowed.
func ProjectTotals(rows []Row, w Window, project string) Totals {
	return Sum(rows, func(r Row) bool {
		return r.Tags.Project == project && w.Contains(r.At)
	})
}

// BeadTotals is spend against the PER-TASK ceiling: lifetime, not windowed. A
// bead that straddles a month boundary keeps the same per-task budget -- the
// ceiling is a property of the work item, not of the calendar.
func BeadTotals(rows []Row, project, beadID string) Totals {
	return Sum(rows, func(r Row) bool {
		return r.Tags.Project == project && r.Tags.BeadID == beadID
	})
}

// SessionTotals is what commit provenance trailers report (spec 6.1).
func SessionTotals(rows []Row, sessionKey string) Totals {
	return Sum(rows, func(r Row) bool { return r.Tags.SessionKey == sessionKey })
}

func ByRung(rows []Row, w Window, project string) map[string]Totals {
	return groupBy(rows, w, project, func(r Row) string { return r.Tags.Rung })
}

func ByTrigger(rows []Row, w Window, project string) map[string]Totals {
	return groupBy(rows, w, project, func(r Row) string { return r.Tags.Trigger })
}

func groupBy(rows []Row, w Window, project string, key func(Row) string) map[string]Totals {
	out := map[string]Totals{}
	for _, r := range rows {
		if r.Tags.Project != project || !w.Contains(r.At) {
			continue
		}
		t := out[key(r)]
		t.add(r)
		out[key(r)] = t
	}
	return out
}

// Dedupe removes rows whose CallID has already been seen, keeping the first.
// Spend logs are polled with overlapping time windows, so the same call WILL
// be read more than once; counting it twice is a wrong budget, which is a
// wrong decision.
func Dedupe(rows []Row) ([]Row, int) {
	seen := make(map[string]struct{}, len(rows))
	out := rows[:0:0]
	dropped := 0
	for _, r := range rows {
		if _, dup := seen[r.CallID]; dup {
			dropped++
			continue
		}
		seen[r.CallID] = struct{}{}
		out = append(out, r)
	}
	return out, dropped
}
