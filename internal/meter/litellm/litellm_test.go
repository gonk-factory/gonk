package litellm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPAdminEnsureKey(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"sk-real-abc","key_alias":"gonk-group-repo"}`))
	}))
	defer srv.Close()

	a := NewHTTPAdmin(srv.URL, "sk-master-SECRET", srv.Client())
	limit := 10.0
	info, err := a.EnsureKey(context.Background(), KeySpec{
		Alias: "gonk-group-repo", MaxBudgetUSD: &limit, BudgetDuration: "1mo",
		Models: []string{"qwen3-coder-30b", "glm-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Token != "sk-real-abc" {
		t.Fatalf("token = %q", info.Token)
	}
	if gotAuth != "Bearer sk-master-SECRET" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"max_budget":10`) {
		t.Fatalf("body did not carry the budget: %s", gotBody)
	}
}

// An unlimited budget must OMIT max_budget. Sending +Inf is a marshal error and
// sending null may be read as "no limit" or as "zero" depending on the version
// -- omission is the only unambiguous encoding.
func TestHTTPAdminUnlimitedOmitsBudget(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"key":"sk-x","key_alias":"a"}`))
	}))
	defer srv.Close()
	if _, err := NewHTTPAdmin(srv.URL, "k", srv.Client()).
		EnsureKey(context.Background(), KeySpec{Alias: "a"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotBody, "max_budget") {
		t.Fatalf("unlimited budget still sent a max_budget field: %s", gotBody)
	}
}

// A 500 from LiteLLM must not put the admin key in the error -- errors get
// logged, and logs get shipped.
func TestHTTPAdminErrorDoesNotLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	_, err := NewHTTPAdmin(srv.URL, "sk-master-SECRET", srv.Client()).
		EnsureKey(context.Background(), KeySpec{Alias: "a"})
	if err == nil {
		t.Fatal("a 500 was not an error")
	}
	if strings.Contains(err.Error(), "sk-master-SECRET") {
		t.Fatalf("the admin key leaked into an error string: %v", err)
	}
}

func TestHTTPSpendSourceParsesTagsAndClock(t *testing.T) {
	var reqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		q := r.URL.Query()
		// THE BOUND IS NOT OPTIONAL, ON EVERY PAGE. An unbounded /spend/logs
		// OOM-killed the live LiteLLM (docs/environment.md, OPERATIONAL HAZARD).
		// Every request MUST carry BOTH a start_date and an end bound; a request
		// missing either is the bug these two assertions exist to catch.
		if q.Get("start_date") == "" {
			t.Fatalf("spend poll had no start_date -- an unbounded /spend/logs takes down the cluster's gateway")
		}
		if q.Get("end_date") == "" {
			t.Fatalf("spend poll had no end_date -- the window must be bounded at BOTH ends, not just the start")
		}
		w.Header().Set("Date", "Mon, 13 Jul 2026 10:00:00 GMT")
		w.Header().Set("Content-Type", "application/json")
		// TWO PAGES of the REAL v1.92.0 envelope {data,total,page,page_size,
		// total_pages}, paginated by page/total_pages -- there is NO X-Next-Page
		// header on the real proxy. A regression to a single-page fetch FAILS: c3
		// lives on page 2. LiteLLM persists the header-supplied tags at
		// metadata.spend_logs_metadata (VERIFIED, docs/environment.md).
		switch q.Get("page") {
		case "", "1":
			_, _ = w.Write([]byte(`{"total":3,"page":1,"page_size":2,"total_pages":2,"data":[
			  {"request_id":"c1","spend":0.40,"prompt_tokens":8000,"completion_tokens":1500,
			   "startTime":"2026-07-05T10:00:00Z",
			   "metadata":{"spend_logs_metadata":{
			               "gonk_project":"group/repo","gonk_rig":"repo","gonk_bead_id":"gk-1",
			               "gonk_session_key":"s1","gonk_rung":"glm","gonk_attempt":"1",
			               "gonk_trigger":"issue-triage"}}},
			  {"request_id":"c2","spend":0.10,"startTime":"2026-07-05T11:00:00Z","metadata":{}}
			]}`))
		default:
			_, _ = w.Write([]byte(`{"total":3,"page":2,"page_size":2,"total_pages":2,"data":[
			  {"request_id":"c3","spend":0.25,"prompt_tokens":4000,"completion_tokens":500,
			   "startTime":"2026-07-06T10:00:00Z",
			   "metadata":{"spend_logs_metadata":{
			               "gonk_project":"group/repo","gonk_rig":"repo","gonk_bead_id":"gk-2",
			               "gonk_session_key":"s3","gonk_rung":"glm","gonk_attempt":"1",
			               "gonk_trigger":"issue-triage"}}}
			]}`))
		}
	}))
	defer srv.Close()

	s := NewHTTPSpendSource(srv.URL, "k", srv.Client())
	rows, clock, err := s.Since(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// The source MUST page: c1 is on page 1, c3 on page 2. A single-page fetch
	// would silently miss c3 (and undercount spend), which is exactly the
	// regression this guards against.
	if reqs < 2 {
		t.Fatalf("spend source issued %d request(s) -- it did not page; a single unbounded fetch is the bug", reqs)
	}
	// c2 has no attribution tags: skipped, not fatal, but COUNTED (Task 9's
	// gonk_meter_spend_rows_unattributed_total). The two attributable rows -- one
	// per page -- must both come back.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (c1 on page 1 + c3 on page 2; the untagged c2 is skipped)", len(rows))
	}
	if rows[0].Tags.BeadID != "gk-1" || rows[0].CostUSD != 0.40 || rows[0].CallID != "c1" {
		t.Fatalf("row[0] = %+v", rows[0])
	}
	if rows[1].Tags.BeadID != "gk-2" || rows[1].CallID != "c3" {
		t.Fatalf("row[1] = %+v (page 2 must be collected too)", rows[1])
	}
	if !clock.Equal(time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("source clock = %v (the Date header is what guards month rollover)", clock)
	}
	if n := s.Unattributed(); n != 1 {
		t.Fatalf("unattributed counter = %d, want 1", n)
	}
}
