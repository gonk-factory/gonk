package litellm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
)

// A row with a good gonk_project but a malformed secondary field
// (gonk_attempt: "many", not a number) must NOT be dropped wholesale -- that
// would undercount the project's real spend and could let it slip past
// monthly_cost_usd. It counts under PartiallyAttributed, with Tags reduced to
// just the project.
func TestSinceCountsPartiallyAttributedRowsAgainstTheProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"request_id":"c1","spend":0.30,"startTime":"2026-07-05T10:00:00Z",
		   "metadata":{"spend_logs_metadata":{
		               "gonk_project":"group/repo","gonk_rig":"repo","gonk_bead_id":"gk-1",
		               "gonk_session_key":"s1","gonk_rung":"glm","gonk_attempt":"many",
		               "gonk_trigger":"issue-triage"}}}
		]`))
	}))
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"request_id":"local1","spend":0.02,"startTime":"2026-07-05T10:00:00Z",
		   "metadata":{"spend_logs_metadata":{
		               "gonk_project":"group/repo","gonk_rig":"repo","gonk_bead_id":"gk-1",
		               "gonk_session_key":"s1","gonk_rung":"qwen-local","gonk_attempt":"1",
		               "gonk_trigger":"issue-triage"}}},
		  {"request_id":"cloud1","spend":0.40,"startTime":"2026-07-05T10:00:00Z",
		   "metadata":{"spend_logs_metadata":{
		               "gonk_project":"group/repo","gonk_rig":"repo","gonk_bead_id":"gk-2",
		               "gonk_session_key":"s2","gonk_rung":"glm","gonk_attempt":"1",
		               "gonk_trigger":"issue-triage"}}},
		  {"request_id":"unknown1","spend":0.10,"startTime":"2026-07-05T10:00:00Z",
		   "metadata":{"spend_logs_metadata":{
		               "gonk_project":"group/repo","gonk_rig":"repo","gonk_bead_id":"gk-3",
		               "gonk_session_key":"s3","gonk_rung":"mystery-rung","gonk_attempt":"1",
		               "gonk_trigger":"issue-triage"}}}
		]`))
	}))
	defer srv.Close()

	cat := map[string]opercfg.RungSpec{
		"qwen-local": {Name: "qwen-local", Kind: opercfg.KindLocal},
		"glm":        {Name: "glm", Kind: opercfg.KindCloud},
	}
	s := NewHTTPSpendSource(srv.URL, "k", srv.Client(), WithRungCatalog(cat))
	rows, _, err := s.Since(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]bool{}
	for _, r := range rows {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"request_id":"c1","spend":0.02,"startTime":"2026-07-05T10:00:00Z",
		   "metadata":{"spend_logs_metadata":{
		               "gonk_project":"group/repo","gonk_rig":"repo","gonk_bead_id":"gk-1",
		               "gonk_session_key":"s1","gonk_rung":"qwen-local","gonk_attempt":"1",
		               "gonk_trigger":"issue-triage"}}}
		]`))
	}))
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
