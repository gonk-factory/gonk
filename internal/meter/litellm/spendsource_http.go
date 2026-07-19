package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// *** THE POLL MUST ALWAYS BE BOUNDED, PAGINATED, AND NEVER NAKED. ***
//
// An unbounded GET /spend/logs (no start_date) OOM-killed the live LiteLLM pod
// during research (2Gi limit, exit 137, ~90s outage; docs/environment.md,
// "OPERATIONAL HAZARD"). gonk-meter POLLS this endpoint on a timer, so a
// regression here does not just break gonk -- it takes down the inference
// gateway for the whole cluster. HTTPSpendSource.Since sets BOTH start_date
// and end_date on EVERY request, and pages until the source reports no more
// pages. TestHTTPSpendSourceParsesTagsAndClock enforces both properties: it
// fails the test if either date is missing, and it serves two pages so a
// regression to a single-page fetch is caught by a missing row, not by
// intuition.
//
// spendLogsPath is LiteLLM's bounded, paginated spend-log endpoint (Decision
// 11: the HTTP API, not LiteLLM's Postgres -- a supported surface that
// survives upgrades). We prefer the v2 shape (mandatory dates, paginated, 10k
// row cap) over the legacy /spend/logs, which is the one that OOM-killed the
// pod when called without a date bound.
//
// VERIFIED against real LiteLLM v1.92.0 (gonk-huy live harness, ghcr.io/
// berriai/litellm-database:v1.92.0). The exact wire contract this adapter now
// speaks, and which cost a Plan 03 shipping bug to learn:
//   - Dates MUST be "YYYY-MM-DD" or "YYYY-MM-DD HH:MM:SS". An RFC3339 value
//     (2026-07-01T00:00:00Z) is rejected HTTP 400 "Invalid date format".
//   - The body is an OBJECT, not a bare array:
//     {"data":[...], "total":N, "page":P, "page_size":S, "total_pages":T}.
//   - Pagination is by the body fields page/total_pages. There is NO
//     X-Next-Page header (the only response headers are Date and content-type).
const spendLogsPath = "/spend/logs/v2"

// litellmDateFormat is the ONLY date format real v1.92.0 /spend/logs/v2
// accepts alongside bare "YYYY-MM-DD"; RFC3339 is a 400. We send the full
// timestamp so the start bound is exact (Since must return rows at or after t,
// not merely at or after t's calendar day).
const litellmDateFormat = "2006-01-02 15:04:05"

// HTTPSpendSource reads LiteLLM's spend log over HTTP.
type HTTPSpendSource struct {
	baseURL  string
	adminKey string
	client   *http.Client
	catalog  map[string]opercfg.RungSpec

	unattributed        atomic.Int64
	partiallyAttributed atomic.Int64
}

// SpendSourceOption configures an HTTPSpendSource at construction.
type SpendSourceOption func(*HTTPSpendSource)

// WithRungCatalog gives the source the rung catalog it needs to set
// Row.Synthetic correctly (Decision 9: LiteLLM has one `spend` column and
// cannot tell real dollars from synthetic ones -- only the catalog knows which
// rungs are local). Without this option the catalog is empty, so every rung
// looks "unknown" and every row is treated as REAL -- fail closed, never the
// other way around.
func WithRungCatalog(catalog map[string]opercfg.RungSpec) SpendSourceOption {
	return func(s *HTTPSpendSource) { s.catalog = catalog }
}

func NewHTTPSpendSource(baseURL, adminKey string, c *http.Client, opts ...SpendSourceOption) *HTTPSpendSource {
	s := &HTTPSpendSource{baseURL: strings.TrimRight(baseURL, "/"), adminKey: adminKey, client: c}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Unattributed is the running count of rows skipped because no project could
// be resolved from their metadata at all (not even the fast path). It is a
// counter, not a sample: Task 9 exposes it as
// gonk_meter_spend_rows_unattributed_total.
func (s *HTTPSpendSource) Unattributed() int { return int(s.unattributed.Load()) }

// PartiallyAttributed is the running count of rows that resolved a project but
// had a malformed SECONDARY field (bad gonk_attempt/gonk_rung/gonk_session_key
// etc): they still count against the project's monthly ceiling, but their
// per-bead/per-session breakdown is lost. Distinct from Unattributed so an
// operator can tell "we are undercounting a project's breakdowns" from "we are
// undercounting a project's total spend" -- the former is a papercut, the
// latter is a budget-safety bug.
func (s *HTTPSpendSource) PartiallyAttributed() int { return int(s.partiallyAttributed.Load()) }

// logEntry is one row of LiteLLM's spend log, as the HTTP API returns it.
type logEntry struct {
	RequestID        string    `json:"request_id"`
	Spend            float64   `json:"spend"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	StartTime        time.Time `json:"startTime"`
	Metadata         struct {
		// VERIFIED against live LiteLLM v1.92.0 (docs/environment.md): this is
		// where opencode's x-litellm-spend-logs-metadata header lands.
		SpendLogsMetadata map[string]string `json:"spend_logs_metadata"`
	} `json:"metadata"`
}

// spendLogPage is the v2 response envelope. VERIFIED against real v1.92.0:
// /spend/logs/v2 returns this object, never a bare array. Pagination is driven
// by Page/TotalPages -- there is no cursor header.
type spendLogPage struct {
	Data       []logEntry `json:"data"`
	Total      int        `json:"total"`
	Page       int        `json:"page"`
	PageSize   int        `json:"page_size"`
	TotalPages int        `json:"total_pages"`
}

// Since implements SpendSource. See the package-level comment above for the
// OOM-guard invariant this method exists to uphold: every page of every
// request carries both start_date and end_date.
func (s *HTTPSpendSource) Since(ctx context.Context, t time.Time) ([]spend.Row, time.Time, error) {
	end := time.Now().UTC()
	var (
		rows        []spend.Row
		sourceClock time.Time
	)
	page := 1
	for {
		body, status, header, err := s.fetchPage(ctx, t, end, page)
		if err != nil {
			return nil, time.Time{}, err
		}
		if status >= 400 {
			return nil, time.Time{}, fmt.Errorf("litellm: %s: status %d: %s", spendLogsPath, status, string(body))
		}
		if sourceClock.IsZero() {
			if dt := header.Get("Date"); dt != "" {
				if parsed, perr := http.ParseTime(dt); perr == nil {
					sourceClock = parsed
				}
			}
		}
		var pg spendLogPage
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, time.Time{}, fmt.Errorf("litellm: %s: decoding page: %w", spendLogsPath, err)
		}
		for _, e := range pg.Data {
			if row, ok := s.toRow(e); ok {
				rows = append(rows, row)
			}
		}
		// Paginate by the body's page/total_pages -- v2 sends no cursor header.
		// Stop when we have read the last page, and defensively when a page came
		// back empty (a total_pages that never catches up must not loop forever).
		if page >= pg.TotalPages || len(pg.Data) == 0 {
			break
		}
		page++
	}
	return rows, sourceClock, nil
}

func (s *HTTPSpendSource) fetchPage(ctx context.Context, start, end time.Time, page int) ([]byte, int, http.Header, error) {
	q := url.Values{}
	// BOTH bounds, on EVERY page -- this is the line the OOM guard lives on.
	// litellmDateFormat, NOT RFC3339: real v1.92.0 rejects RFC3339 with HTTP 400.
	q.Set("start_date", start.UTC().Format(litellmDateFormat))
	q.Set("end_date", end.UTC().Format(litellmDateFormat))
	q.Set("page", strconv.Itoa(page))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+spendLogsPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("litellm: building spend-log request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.adminKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("litellm: spend-log request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, nil, fmt.Errorf("litellm: reading spend-log page: %w", err)
	}
	return body, resp.StatusCode, resp.Header, nil
}

// toRow joins one log entry to its attribution tags and reports whether it
// could be attributed at all.
//
// A row whose metadata is missing or malformed is counted, never silently
// dropped in a way that under-reports a project's real spend: a fully
// resolvable row counts normally; a row with a good gonk_project but a bad
// secondary field (gonk_attempt/gonk_rung/gonk_session_key/...) STILL counts
// against that project's monthly ceiling via the ProjectFromMetadata fast
// path, tallied under PartiallyAttributed; only a row with no resolvable
// project at all is dropped, tallied under Unattributed.
func (s *HTTPSpendSource) toRow(e logEntry) (spend.Row, bool) {
	md := e.Metadata.SpendLogsMetadata
	tags, err := atags.FromMetadata(md)
	if err != nil {
		project, ok := ProjectFromMetadata(md)
		if !ok {
			s.unattributed.Add(1)
			return spend.Row{}, false
		}
		s.partiallyAttributed.Add(1)
		tags = atags.Tags{Project: project}
	}
	return spend.Row{
		CallID:           e.RequestID,
		Tags:             tags,
		CostUSD:          e.Spend,
		PromptTokens:     e.PromptTokens,
		CompletionTokens: e.CompletionTokens,
		At:               e.StartTime.UTC(),
		Synthetic:        s.catalog[tags.Rung].Kind == opercfg.KindLocal,
	}, true
}

// ProjectFromMetadata is the fast path that validates ONLY the project field,
// so a row with a good gonk_project but a malformed secondary field (a
// non-numeric gonk_attempt, say) still resolves to a project instead of being
// dropped wholesale. It deliberately does not validate anything else: that is
// what atags.FromMetadata is for, and this function only runs after that has
// already failed.
func ProjectFromMetadata(m map[string]string) (string, bool) {
	p := m[atags.KeyProject]
	if p == "" {
		return "", false
	}
	return p, true
}

var _ SpendSource = (*HTTPSpendSource)(nil)
