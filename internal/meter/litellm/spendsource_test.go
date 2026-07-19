package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
)

// v2Server is an httptest.Server that speaks the SAME /spend/logs/v2 contract as
// real LiteLLM v1.92.0, verified against ghcr.io/berriai/litellm-database:v1.92.0
// by the gonk-huy live harness. It exists because the original mock lied -- it
// returned a bare array and ignored the query dates -- which is exactly why the
// Plan 03 adapter shipped unable to read a real proxy. Every rule the real proxy
// enforces, this mock enforces, so a regression to the broken shapes fails here
// in the standing gate instead of only in production:
//
//   - start_date/end_date MUST be "YYYY-MM-DD" or "YYYY-MM-DD HH:MM:SS"; an
//     RFC3339 value is rejected HTTP 400 (real message reproduced).
//   - start_date is MANDATORY on every page (the OOM guard); a request without
//     one is rejected, so a naked-query regression cannot pass.
//   - the body is the object envelope {data,total,page,page_size,total_pages},
//     never a bare array.
//   - pagination is by the page query param against total_pages; there is no
//     X-Next-Page header.
//
// rows are served page_size at a time so a >1-page fetch is genuinely exercised.
type v2Server struct {
	rows     []logEntry
	pageSize int
	requests []url.Values // captured per request, for date-format assertions
}

// litellmDateRE matches the two formats real v1.92.0 accepts and nothing else
// (notably not RFC3339, which carries a 'T' and a zone).
var litellmDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}( \d{2}:\d{2}:\d{2})?$`)

func newV2Server(t *testing.T, pageSize int, rows []logEntry) *httptest.Server {
	t.Helper()
	srv := &v2Server{rows: rows, pageSize: pageSize}
	return httptest.NewServer(http.HandlerFunc(srv.handle(t)))
}

func (v *v2Server) handle(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spend/logs/v2" {
			t.Fatalf("unexpected path %q, want /spend/logs/v2", r.URL.Path)
		}
		q := r.URL.Query()
		v.requests = append(v.requests, q)

		start := q.Get("start_date")
		if start == "" {
			// The OOM guard: real v2 requires a bound, and so must we.
			writeV2Error(w, http.StatusBadRequest, "start_date is required")
			return
		}
		for _, name := range []string{"start_date", "end_date"} {
			val := q.Get(name)
			if val != "" && !litellmDateRE.MatchString(val) {
				writeV2Error(w, http.StatusBadRequest,
					fmt.Sprintf("Invalid date format: %s. Expected: 'YYYY-MM-DD' or 'YYYY-MM-DD HH:MM:SS'", val))
				return
			}
		}

		page := 1
		if p := q.Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		totalPages := (len(v.rows) + v.pageSize - 1) / v.pageSize
		if totalPages == 0 {
			totalPages = 1
		}
		lo := (page - 1) * v.pageSize
		hi := lo + v.pageSize
		if lo > len(v.rows) {
			lo = len(v.rows)
		}
		if hi > len(v.rows) {
			hi = len(v.rows)
		}
		body := spendLogPage{
			Data:       v.rows[lo:hi],
			Total:      len(v.rows),
			Page:       page,
			PageSize:   v.pageSize,
			TotalPages: totalPages,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeV2Error(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"internal_server_error","code":"%d"}}`, msg, code)
}

func entry(id, project, rung string, spendUSD float64) logEntry {
	var e logEntry
	e.RequestID = id
	e.Spend = spendUSD
	e.StartTime = time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	e.Metadata.SpendLogsMetadata = map[string]string{
		"gonk_project":     project,
		"gonk_rig":         "repo",
		"gonk_bead_id":     "gk-1",
		"gonk_session_key": "s1",
		"gonk_rung":        rung,
		"gonk_attempt":     "1",
		"gonk_trigger":     "issue-triage",
	}
	return e
}

// Since must walk EVERY page of the v2 envelope. The mock serves one row per
// page across three pages; a regression to a single-page fetch (the original
// X-Next-Page bug) drops rows 2 and 3 and fails here.
func TestSincePaginatesAcrossAllPages(t *testing.T) {
	rows := []logEntry{
		entry("r1", "group/repo", "glm", 0.10),
		entry("r2", "group/repo", "glm", 0.20),
		entry("r3", "group/repo", "glm", 0.30),
	}
	srv := newV2Server(t, 1, rows)
	defer srv.Close()

	s := NewHTTPSpendSource(srv.URL, "k", srv.Client())
	got, _, err := s.Since(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows across pages, want 3 -- pagination must read every page", len(got))
	}
	var sum float64
	for _, r := range got {
		sum += r.CostUSD
	}
	if math.Abs(sum-0.60) > 1e-9 {
		t.Fatalf("summed spend = %v, want 0.60 (all three pages)", sum)
	}
}

// Every page of every request must carry start_date (the OOM guard) formatted
// the way real v1.92.0 demands -- YYYY-MM-DD[ HH:MM:SS], NEVER RFC3339. This is
// the test that would have caught the shipped bug.
func TestSinceSendsBoundedNonRFC3339Dates(t *testing.T) {
	rows := []logEntry{entry("r1", "group/repo", "glm", 0.10), entry("r2", "group/repo", "glm", 0.20)}
	srvImpl := &v2Server{rows: rows, pageSize: 1}
	srv := httptest.NewServer(http.HandlerFunc(srvImpl.handle(t)))
	defer srv.Close()

	s := NewHTTPSpendSource(srv.URL, "k", srv.Client())
	if _, _, err := s.Since(context.Background(), time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if len(srvImpl.requests) < 2 {
		t.Fatalf("expected >=2 paged requests, got %d", len(srvImpl.requests))
	}
	for i, q := range srvImpl.requests {
		start := q.Get("start_date")
		end := q.Get("end_date")
		if start == "" || end == "" {
			t.Fatalf("request %d missing a date bound: start=%q end=%q (OOM guard)", i, start, end)
		}
		if !litellmDateRE.MatchString(start) || !litellmDateRE.MatchString(end) {
			t.Fatalf("request %d dates not in YYYY-MM-DD[ HH:MM:SS] form: start=%q end=%q", i, start, end)
		}
		if _, err := time.Parse(time.RFC3339, start); err == nil {
			t.Fatalf("request %d start_date %q parses as RFC3339 -- real v1.92.0 rejects that with HTTP 400", i, start)
		}
	}
}

// If the adapter regresses to sending RFC3339 dates, the faithful mock rejects
// them HTTP 400 exactly as the real proxy does, and Since surfaces the error
// rather than silently returning zero rows.
func TestSinceSurfacesDateFormatRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stand-in that ONLY accepts RFC3339's inverse: reject anything the real
		// proxy would reject. Here we assert the adapter does NOT send RFC3339 by
		// having the server 400 on a 'T' in the date.
		q := r.URL.Query()
		if d := q.Get("start_date"); d != "" && litellmDateRE.MatchString(d) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(spendLogPage{Data: nil, Total: 0, Page: 1, PageSize: 50, TotalPages: 1})
			return
		}
		writeV2Error(w, http.StatusBadRequest, "Invalid date format")
	}))
	defer srv.Close()

	s := NewHTTPSpendSource(srv.URL, "k", srv.Client())
	if _, _, err := s.Since(context.Background(), time.Now()); err != nil {
		t.Fatalf("Since returned error against a faithful date-checking mock: %v", err)
	}
}

// A row with a good gonk_project but a malformed secondary field
// (gonk_attempt: "many", not a number) must NOT be dropped wholesale -- that
// would undercount the project's real spend and could let it slip past
// monthly_cost_usd. It counts under PartiallyAttributed, with Tags reduced to
// just the project.
func TestSinceCountsPartiallyAttributedRowsAgainstTheProject(t *testing.T) {
	bad := entry("c1", "group/repo", "glm", 0.30)
	bad.Metadata.SpendLogsMetadata["gonk_attempt"] = "many"
	srv := newV2Server(t, 50, []logEntry{bad})
	defer srv.Close()

	s := NewHTTPSpendSource(srv.URL, "k", srv.Client())
	rows, _, err := s.Since(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 -- a bad gonk_attempt must not drop the row wholesale (spend undercount)", len(rows))
	}
	if rows[0].Tags.Project != "group/repo" {
		t.Fatalf("row project = %q, want group/repo", rows[0].Tags.Project)
	}
	if rows[0].CostUSD != 0.30 {
		t.Fatalf("row cost = %v, want 0.30 -- the project's real spend must still be counted", rows[0].CostUSD)
	}
	if got := s.PartiallyAttributed(); got != 1 {
		t.Fatalf("PartiallyAttributed = %d, want 1", got)
	}
	if got := s.Unattributed(); got != 0 {
		t.Fatalf("Unattributed = %d, want 0 -- a project-resolvable row is NOT the same failure as an unresolvable one", got)
	}
}

// A row whose metadata has no gonk_project at all cannot be attributed to
// anything, even by the fast path: it is truly unattributable.
func TestProjectFromMetadataRequiresProject(t *testing.T) {
	if p, ok := ProjectFromMetadata(map[string]string{"gonk_rig": "repo"}); ok {
		t.Fatalf("ProjectFromMetadata resolved %q from metadata with no project", p)
	}
	if p, ok := ProjectFromMetadata(nil); ok {
		t.Fatalf("ProjectFromMetadata resolved %q from nil metadata", p)
	}
	p, ok := ProjectFromMetadata(map[string]string{"gonk_project": "group/repo"})
	if !ok || p != "group/repo" {
		t.Fatalf("ProjectFromMetadata(project only) = %q, %v, want group/repo, true", p, ok)
	}
}

// Since must set Row.Synthetic from the rung catalog given at construction: a
// local rung is synthetic, a cloud rung is not, and a rung the catalog has
// never heard of is treated as REAL money -- fail closed, so an unknown rung's
// dollars count against the real ceiling instead of being waved through as
// accounting fiction.
func TestSinceSetsSyntheticFromCatalog(t *testing.T) {
	rows := []logEntry{
		entry("local1", "group/repo", "qwen-local", 0.02),
		entry("cloud1", "group/repo", "glm", 0.40),
		entry("unknown1", "group/repo", "mystery-rung", 0.10),
	}
	srv := newV2Server(t, 50, rows)
	defer srv.Close()

	cat := map[string]opercfg.RungSpec{
		"qwen-local": {Name: "qwen-local", Kind: opercfg.KindLocal},
		"glm":        {Name: "glm", Kind: opercfg.KindCloud},
	}
	s := NewHTTPSpendSource(srv.URL, "k", srv.Client(), WithRungCatalog(cat))
	got, _, err := s.Since(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]bool{}
	for _, r := range got {
		byID[r.CallID] = r.Synthetic
	}
	if !byID["local1"] {
		t.Fatal("local1 (qwen-local, KindLocal) should be Synthetic")
	}
	if byID["cloud1"] {
		t.Fatal("cloud1 (glm, KindCloud) should NOT be Synthetic")
	}
	if byID["unknown1"] {
		t.Fatal("unknown1 (mystery-rung, not in catalog) should NOT be Synthetic -- fail closed to REAL money")
	}
}

// Without a catalog at all, every rung is "unknown" -- every row must still
// fail closed to REAL, never silently treated as synthetic accounting
// fiction.
func TestSinceWithoutCatalogFailsClosedToReal(t *testing.T) {
	srv := newV2Server(t, 50, []logEntry{entry("c1", "group/repo", "qwen-local", 0.02)})
	defer srv.Close()

	s := NewHTTPSpendSource(srv.URL, "k", srv.Client())
	rows, _, err := s.Since(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Synthetic {
		t.Fatalf("rows = %+v, want one non-synthetic row (no catalog => fail closed to real)", rows)
	}
}
