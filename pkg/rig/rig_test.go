package rig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubFetcher struct {
	body []byte
	err  error
	// gotID/gotRef record what the fetcher was asked for, so a test can prove
	// delivery keyed off the GRANT and not off anything the caller supplied.
	gotID  int64
	gotRef string
}

func (s *stubFetcher) RepoArchive(_ context.Context, projectID int64, ref string, _ int64) ([]byte, error) {
	s.gotID, s.gotRef = projectID, ref
	if s.err != nil {
		return nil, s.err
	}
	return s.body, nil
}

func newHandler(t *testing.T, f ArchiveFetcher) (*Handler, *http.ServeMux) {
	t.Helper()
	h := &Handler{Store: NewStore(nil), Fetch: f}
	mux := http.NewServeMux()
	h.Register(mux)
	return h, mux
}

func grant(t *testing.T, mux *http.ServeMux, alias, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rig/"+alias, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// The happy path, end to end through the mux: the controller registers a grant,
// the pod fetches its tree, and the bytes come back.
func TestGrantThenFetchDeliversTheTree(t *testing.T) {
	f := &stubFetcher{body: []byte("tarball-bytes")}
	_, mux := newHandler(t, f)

	if rec := grant(t, mux, "gonk.triage.p7.i1.a1", `{"project":"acme/widget","project_id":7,"ref":"abc123"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("register = %d, want 204: %s", rec.Code, rec.Body)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rig/gonk.triage.p7.i1.a1.tar.gz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != "tarball-bytes" {
		t.Errorf("body = %q, want the archive bytes", got)
	}
	// Delivery must use the GRANT's project and ref -- the pod supplies only an
	// alias, and must not be able to steer either.
	if f.gotID != 7 || f.gotRef != "abc123" {
		t.Errorf("fetched project %d ref %q, want 7/abc123 from the grant", f.gotID, f.gotRef)
	}
	if got := rec.Header().Get("X-Gonk-Rig-Ref"); got != "abc123" {
		t.Errorf("ref header = %q, want the pinned ref so a session can prove what it read", got)
	}
}

// THE AUTHORIZATION PROPERTY. Without a grant there is no checkout: this is the
// only thing standing between "the pod fetches its own tree" and "any pod in the
// namespace fetches any project", because the private listener has no per-caller
// auth of its own.
func TestFetchWithoutAGrantIsRefused(t *testing.T) {
	_, mux := newHandler(t, &stubFetcher{body: []byte("nope")})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rig/gonk.triage.p9.i9.a1.tar.gz", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("fetch without a grant = %d, want 404", rec.Code)
	}
}

// An expired grant must be indistinguishable from an unknown one, so a prober
// cannot enumerate which sessions existed.
func TestExpiredGrantLooksExactlyLikeAnUnknownOne(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	h := &Handler{Store: NewStore(clock), Fetch: &stubFetcher{body: []byte("x")}, TTL: time.Minute}
	mux := http.NewServeMux()
	h.Register(mux)

	if rec := grant(t, mux, "alias-a", `{"project":"p","project_id":1,"ref":"r"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("register = %d", rec.Code)
	}
	now = now.Add(2 * time.Hour) // past the TTL

	expired := httptest.NewRecorder()
	mux.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, "/rig/alias-a.tar.gz", nil))
	unknown := httptest.NewRecorder()
	mux.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/rig/alias-b.tar.gz", nil))

	if expired.Code != unknown.Code || expired.Body.String() != unknown.Body.String() {
		t.Errorf("expired (%d %q) and unknown (%d %q) responses differ; that difference tells a prober which aliases were real",
			expired.Code, expired.Body.String(), unknown.Code, unknown.Body.String())
	}
}

// A crafted alias must never reach the forge call. These are the shapes that
// would turn an alias into a path traversal or a query smuggle.
func TestHostileAliasesAreRejected(t *testing.T) {
	for _, alias := range []string{
		"../../etc/passwd",
		"a/b",
		"",
		strings.Repeat("x", 200),
		"a?sha=main",
		".hidden",
	} {
		t.Run(alias, func(t *testing.T) {
			if ValidAlias(alias) {
				t.Fatalf("ValidAlias(%q) = true; it must not be usable as a path segment", alias)
			}
		})
	}
}

// A grant with no ref is refused: an unpinned checkout could change under an
// in-flight session, so what the agent read would stop being reproducible.
func TestGrantRequiresAPinnedRef(t *testing.T) {
	s := NewStore(nil)
	if err := s.Register("alias-a", Grant{Project: "p", ProjectID: 1}); err == nil {
		t.Fatal("expected a grant with no ref to be refused")
	}
	if err := s.Register("alias-a", Grant{Project: "p", Ref: "r"}); err == nil {
		t.Fatal("expected a grant with no project id to be refused")
	}
}

// A forge failure must be a 502 the pod can distinguish from "you have no
// grant", and must not be mistaken for an empty tree.
func TestForgeFailureIsNotAnEmptyTree(t *testing.T) {
	_, mux := newHandler(t, &stubFetcher{err: errors.New("gitlab exploded")})
	if rec := grant(t, mux, "alias-a", `{"project":"p","project_id":1,"ref":"r"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("register = %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rig/alias-a.tar.gz", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("fetch on forge failure = %d, want 502 (an empty 200 would look like an empty repository)", rec.Code)
	}
}

// Revoke ends delivery, so a pod outliving its work cannot keep pulling.
func TestRevokeStopsDelivery(t *testing.T) {
	h, mux := newHandler(t, &stubFetcher{body: []byte("x")})
	grant(t, mux, "alias-a", `{"project":"p","project_id":1,"ref":"r"}`)
	h.Store.Revoke("alias-a")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rig/alias-a.tar.gz", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("fetch after revoke = %d, want 404", rec.Code)
	}
}

// The Client and the Handler must agree on the URL shape; if they drift, the
// prompt advertises an address that 404s.
func TestClientAndHandlerAgreeOnTheURL(t *testing.T) {
	f := &stubFetcher{body: []byte("bytes")}
	h := &Handler{Store: NewStore(nil), Fetch: f}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	if err := c.Grant(context.Background(), "alias-a", "acme/widget", 7, "abc123"); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	resp, err := http.Get(FetchURL(srv.URL, "alias-a"))
	if err != nil {
		t.Fatalf("fetch %s: %v", FetchURL(srv.URL, "alias-a"), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("FetchURL round trip = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "bytes" {
		t.Errorf("body = %q", got)
	}
}

// Registering many sessions must not grow the map without bound once they age
// out; the sweep runs on Register so there is no goroutine to leak.
func TestExpiredGrantsAreSweptOnRegister(t *testing.T) {
	now := time.Now()
	s := NewStore(func() time.Time { return now })
	for i := range 20 {
		if err := s.Register(fmt.Sprintf("alias-%d", i), Grant{
			Project: "p", ProjectID: 1, Ref: "r", ExpiresAt: now.Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour)
	if err := s.Register("fresh", Grant{Project: "p", ProjectID: 1, Ref: "r", ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got := s.Len(); got != 1 {
		t.Errorf("held %d grants after the sweep, want only the fresh one", got)
	}
}
