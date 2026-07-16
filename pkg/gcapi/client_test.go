package gcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "gonk-city")
	c.RetryBackoff = func(int) time.Duration { return 0 }
	return c
}

func TestRunOrderPostsToTheRealRoute(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = fmt.Fprint(w, `{"status":"queued","scoped_name":"gonk-city/gonk-dispatch","tracking_id":"trk-1"}`)
	}))

	res, err := c.RunOrder(context.Background(), "gonk-dispatch", map[string]string{
		"bead_anchor": "gonk:42:issue:3",
		"trigger":     "issue-triage",
	})
	if err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	if gotPath != "/v0/city/gonk-city/order/gonk-dispatch/run" {
		t.Fatalf("path = %q", gotPath)
	}
	vars, _ := gotBody["vars"].(map[string]any)
	if vars["bead_anchor"] != "gonk:42:issue:3" || vars["trigger"] != "issue-triage" {
		t.Fatalf("vars = %+v", vars)
	}
	if res.TrackingID != "trk-1" || res.ScopedName != "gonk-city/gonk-dispatch" {
		t.Fatalf("res = %+v", res)
	}
}

// The city name is part of the ROUTE. A wrong one is a 404 on every dispatch, and
// an EMPTY one silently builds "/v0/city//order/x/run", which is a different route
// that may 404 or may match something else. Refuse at construction.
func TestEmptyCityIsRefused(t *testing.T) {
	if _, err := New("http://x", "").RunOrder(context.Background(), "o", nil); err == nil {
		t.Fatal("an empty city name must be an error, not a URL with an empty path segment")
	}
}

// A name with a slash would escape the route. Escape it, do not concatenate.
func TestOrderNameIsEscaped(t *testing.T) {
	var gotPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = fmt.Fprint(w, `{"status":"queued"}`)
	}))
	_, _ = c.RunOrder(context.Background(), "a/b", nil)
	if strings.Contains(gotPath, "/a/b/") {
		t.Fatalf("order name was not escaped: %q", gotPath)
	}
}

func TestRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"queued","tracking_id":"t"}`)
	}))
	if _, err := c.RunOrder(context.Background(), "gonk-dispatch", nil); err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestDoesNotRetry4xx(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	_, err := c.RunOrder(context.Background(), "nope", nil)
	if err == nil {
		t.Fatal("want error")
	}
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want IsNotFound (a 404 here almost always means a wrong GONK_CITY -- OD-1)", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

// Vars are strings on the wire. A nil map must send `{"vars":{}}`, not
// `{"vars":null}` -- a null could deserialize differently upstream.
func TestNilVarsSendsEmptyObject(t *testing.T) {
	var raw string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		raw = string(b[:n])
		_, _ = fmt.Fprint(w, `{"status":"queued"}`)
	}))
	_, _ = c.RunOrder(context.Background(), "o", nil)
	if !strings.Contains(raw, `"vars":{}`) {
		t.Fatalf("body = %q, want vars to be an empty object", raw)
	}
}

// ---------------------------------------------------------------------------
// The tests below are not in Plan 04 Task 2's text; they verify properties the
// task calls out explicitly (verbatim decide-response carry, bounded retry,
// size cap, no secret leak) with real assertions rather than by inspection.

// A dropped field here is a session that runs with the wrong (or no) rung,
// key, or reservation -- a silent budget-context bug, not a crash. Simulate
// the exact set of fields intake's OrderRequest carries from a meter
// DecideResponse (rung, model, key_ref's secret name, reservation_id, and the
// attempt folded into metadata_json) and confirm every single one survives
// the trip through RunOrder to the wire, unmodified.
func TestRunOrderCarriesDecideResponseFieldsVerbatim(t *testing.T) {
	vars := map[string]string{
		"rung":           "qwen-local",
		"model":          "qwen2.5-coder-32b",
		"key_ref":        "litellm-key-group-repo", // a Secret NAME, never key material
		"reservation_id": "rsv-abc123",
		"metadata_json":  `{"gonk_attempt":"2","gonk_bead":"gonk:42:issue:3"}`,
	}
	var gotBody map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = fmt.Fprint(w, `{"status":"queued","tracking_id":"t"}`)
	}))
	if _, err := c.RunOrder(context.Background(), "gonk-dispatch", vars); err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	gotVars, _ := gotBody["vars"].(map[string]any)
	for k, want := range vars {
		if got, _ := gotVars[k].(string); got != want {
			t.Fatalf("var %q = %q, want %q (a dropped decide-response field is wrong budget context downstream)", k, got, want)
		}
	}
	if len(gotVars) != len(vars) {
		t.Fatalf("vars = %+v, want exactly %+v (no extra, no missing)", gotVars, vars)
	}
}

// Bounded means bounded: a supervisor stuck returning 502 forever must not
// retry forever. MaxRetries=2 means 3 total attempts (1 + 2 retries), then
// give up and return the error.
func TestRetriesAreBounded(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	c.MaxRetries = 2
	_, err := c.RunOrder(context.Background(), "gonk-dispatch", nil)
	if err == nil {
		t.Fatal("want error: the supervisor never recovered")
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (1 initial + MaxRetries=2, then stop)", calls)
	}
}

// gascity is semi-trusted at best (it is on the trust boundary, spec 9): a
// misbehaving or compromised supervisor that answers with an unbounded body
// must not be read into memory whole.
func TestRunOrderRefusesOversizeBody(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 100 {
			_, _ = fmt.Fprint(w, strings.Repeat("x", 1024)) // 100 KiB, over the 64 KiB cap
		}
	}))
	_, err := c.RunOrder(context.Background(), "gonk-dispatch", nil)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want 'too large'", err)
	}
}

// key_ref is a Secret NAME (meterapi.KeyRef), never key material -- but a
// client that echoes the request body into its error text would leak
// whatever the caller put in vars, secret name or not, into every log line
// that captures err.Error(). Confirm a failing RunOrder's error carries the
// response status/path/body (diagnosable) but never the request vars.
func TestErrorDoesNotLeakRequestVars(t *testing.T) {
	const secretLookingValue = "litellm-key-group-repo-do-not-log-me"
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	c.MaxRetries = 0
	_, err := c.RunOrder(context.Background(), "gonk-dispatch", map[string]string{
		"key_ref": secretLookingValue,
	})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), secretLookingValue) {
		t.Fatalf("err = %v: leaked a request var into the error text", err)
	}
}
