package glab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "s3cret")
	c.RetryBackoff = func(int) time.Duration { return 0 } // no sleeping in tests
	return c
}

func TestSendsPrivateToken(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "s3cret" {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		_, _ = fmt.Fprint(w, `{"id":7,"username":"gonk"}`)
	}))
	u, err := c.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser = %v", err)
	}
	if u.ID != 7 || u.Username != "gonk" {
		t.Fatalf("user = %+v", u)
	}
}

func TestNotFoundIsTyped(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404 File Not Found"}`, http.StatusNotFound)
	}))
	_, err := c.GetRawFile(context.Background(), 1, ".gonk.yml", "main", 1024)
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want IsNotFound", err)
	}
}

// An error must never carry the token: errors get logged.
func TestErrorDoesNotLeakToken(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	c.MaxRetries = 0
	_, err := c.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks token: %v", err)
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
		_, _ = fmt.Fprint(w, `{"id":7,"username":"gonk"}`)
	}))
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatalf("CurrentUser = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestDoesNotRetry4xx(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	if _, err := c.CurrentUser(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (4xx must not retry)", calls)
	}
}

// AD-4b: rotation slot 2 for a credential we PRESENT. Mid-rotation, GitLab still
// only accepts the old PAT (or only the new one). One retry with the other slot
// turns an outage into a metric. It must be exactly ONE retry, and it must never
// happen when no previous slot is configured.
func TestFallsBackToPreviousTokenOnce(t *testing.T) {
	var seen []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("PRIVATE-TOKEN")
		seen = append(seen, tok)
		if tok != "old" {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":7,"username":"gonk"}`)
	}))
	c.SetPreviousToken("old")
	var fallbacks int
	c.AuthFallback = func() { fallbacks++ }

	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatalf("CurrentUser = %v", err)
	}
	if len(seen) != 2 || seen[0] != "s3cret" || seen[1] != "old" {
		t.Fatalf("tokens presented = %v, want [s3cret old]", seen)
	}
	if fallbacks != 1 {
		t.Fatalf("AuthFallback fired %d times, want 1 (a silent half-rotation is the bug)", fallbacks)
	}
}

func TestNoFallbackWhenNoPreviousSlot(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	if _, err := c.CurrentUser(context.Background()); !IsForbidden(err) {
		t.Fatalf("err = %v, want 401", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no previous slot means no retry)", calls)
	}
}

func TestPaginatesMemberProjects(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("X-Next-Page", "2")
			_, _ = fmt.Fprint(w, `[{"id":1,"path_with_namespace":"a/b","default_branch":"main"}]`)
		default:
			w.Header().Set("X-Next-Page", "")
			_, _ = fmt.Fprint(w, `[{"id":2,"path_with_namespace":"c/d","default_branch":"main"}]`)
		}
	}))
	ps, err := c.ListMemberProjects(context.Background())
	if err != nil {
		t.Fatalf("ListMemberProjects = %v", err)
	}
	if len(ps) != 2 || ps[1].ID != 2 {
		t.Fatalf("projects = %+v", ps)
	}
}

// .gonk.yml is attacker-controlled: a project can commit a 2 GiB file. The
// client must refuse to read past the cap rather than OOM the budget enforcer.
func TestGetRawFileRefusesOversizeBody(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 100 {
			_, _ = fmt.Fprint(w, strings.Repeat("x", 1024))
		}
	}))
	_, err := c.GetRawFile(context.Background(), 1, ".gonk.yml", "main", 4096)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want 'too large'", err)
	}
}

// A misbehaving or MITM'd GitLab that always advertises a next page one greater
// than the one requested drives paginate to unbounded requests. The self-
// reference and non-numeric guards don't catch a monotonically-increasing
// hostile sequence, so pagination must be bounded like MaxBytes/MaxRetries.
// Reconciliation calls ListMemberProjects (-> paginate) as its entry point, so
// an uncapped loop stalls the whole reconciler.
func TestPaginateCapsHostilePages(t *testing.T) {
	var hits int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		page := r.URL.Query().Get("page")
		n, _ := strconv.Atoi(page)
		if n == 0 {
			n = 1
		}
		w.Header().Set("X-Next-Page", strconv.Itoa(n+1)) // always one more, forever
		_, _ = fmt.Fprint(w, `[{"id":1,"path_with_namespace":"a/b","default_branch":"main"}]`)
	}))
	c.MaxPages = 5
	// Backstop so a broken cap can't hang the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.ListMemberProjects(ctx)
	if err == nil || !strings.Contains(err.Error(), "pagination exceeded") {
		t.Fatalf("err = %v, want 'pagination exceeded'", err)
	}
	if hits > c.MaxPages+2 {
		t.Fatalf("server hit %d times, want <= %d (cap not enforced)", hits, c.MaxPages+2)
	}
}

// AddIssueLabel is intake's ONLY GitLab write on the dispatch path (the Gate-1
// deny label). It must PUT the issue with add_labels, and rely on GitLab's own
// idempotency (re-adding a present label is a no-op) rather than doing a
// read-before-write.
//
// The body must be JSON carrying add_labels as an ARRAY, not a query string
// (T-05, closes R-02): GitLab splits a query-string add_labels on commas, so
// the old form let a label value containing a comma apply more than one
// label. This asserts the request is JSON and decodes to exactly the one
// label passed in.
func TestAddIssueLabel(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody struct {
		AddLabels []string `json:"add_labels"`
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body as JSON: %v", err)
		}
		_, _ = fmt.Fprint(w, `{}`)
	}))
	if err := c.AddIssueLabel(context.Background(), 42, 7, "gonk::denied"); err != nil {
		t.Fatalf("AddIssueLabel = %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/v4/projects/42/issues/7" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if want := []string{"gonk::denied"}; len(gotBody.AddLabels) != 1 || gotBody.AddLabels[0] != want[0] {
		t.Errorf("add_labels = %v, want %v", gotBody.AddLabels, want)
	}
}

// CreateIssueNote is the broker's comment-apply write: it POSTs one note to the
// issue's notes collection with the given body, and returns the created note so
// the caller can read back its id. Mirrors AddIssueLabel's param style (numeric
// project id + issue iid + a plain string), and CreateIssue's created-object
// return.
//
// The body must be JSON, not the query-string form (`?body=<value>`) this used
// to send (T-06, closes R-16): a comment body is unbounded model output up to
// 64 KiB, and a query string that long risks a 414 from GitLab or an
// intervening proxy long before it risks anything about the CONTENT of the
// comment. This asserts the request carries Content-Type: application/json
// and that the body decodes to exactly the string passed in.
func TestCreateIssueNote(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody struct {
		Body string `json:"body"`
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body as JSON: %v", err)
		}
		_, _ = fmt.Fprint(w, `{"id":99,"body":"hello from the broker"}`)
	}))
	n, err := c.CreateIssueNote(context.Background(), 42, 7, "hello from the broker")
	if err != nil {
		t.Fatalf("CreateIssueNote = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v4/projects/42/issues/7/notes" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.Body != "hello from the broker" {
		t.Errorf("body = %q, want %q", gotBody.Body, "hello from the broker")
	}
	if n == nil || n.ID != 99 {
		t.Fatalf("note = %+v, want id 99", n)
	}
}

// UpdateIssueNote rewrites the canned-status note in place (gonk-yrs). Same
// JSON-body requirement as CreateIssueNote and for the same reason (T-06,
// closes R-16): the edited body is the same unbounded, up-to-64-KiB model
// output.
func TestUpdateIssueNote(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody struct {
		Body string `json:"body"`
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body as JSON: %v", err)
		}
		_, _ = fmt.Fprint(w, `{"id":99,"body":"the real answer"}`)
	}))
	n, err := c.UpdateIssueNote(context.Background(), 42, 7, 99, "the real answer")
	if err != nil {
		t.Fatalf("UpdateIssueNote = %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/v4/projects/42/issues/7/notes/99" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.Body != "the real answer" {
		t.Errorf("body = %q, want %q", gotBody.Body, "the real answer")
	}
	if n == nil || n.ID != 99 {
		t.Fatalf("note = %+v, want id 99", n)
	}
}
