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

// sha1 / sha2 are two distinct full-length commit ids (gonk-qdr4).
const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
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

	if rec := grant(t, mux, "gonk.triage.p7.i1.a1", `{"project":"acme/widget","project_id":7,"sha":"`+sha1+`"}`); rec.Code != http.StatusNoContent {
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
	if f.gotID != 7 || f.gotRef != sha1 {
		t.Errorf("fetched project %d ref %q, want 7/%s from the grant", f.gotID, f.gotRef, sha1)
	}
	if got := rec.Header().Get("X-Gonk-Rig-Ref"); got != sha1 {
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

	if rec := grant(t, mux, "alias-a", `{"project":"p","project_id":1,"sha":"`+sha1+`"}`); rec.Code != http.StatusNoContent {
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
	if err := s.Register("alias-a", Grant{Project: "p", SHA: sha1}); err == nil {
		t.Fatal("expected a grant with no project id to be refused")
	}
}

// A forge failure must be a 502 the pod can distinguish from "you have no
// grant", and must not be mistaken for an empty tree.
func TestForgeFailureIsNotAnEmptyTree(t *testing.T) {
	_, mux := newHandler(t, &stubFetcher{err: errors.New("gitlab exploded")})
	if rec := grant(t, mux, "alias-a", `{"project":"p","project_id":1,"sha":"`+sha1+`"}`); rec.Code != http.StatusNoContent {
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
	grant(t, mux, "alias-a", `{"project":"p","project_id":1,"sha":"`+sha1+`"}`)
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
	if err := c.Grant(context.Background(), "alias-a", "acme/widget", 7, sha1); err != nil {
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
			Project: "p", ProjectID: 1, SHA: sha1, ExpiresAt: now.Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour)
	if err := s.Register("fresh", Grant{Project: "p", ProjectID: 1, SHA: sha1, ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got := s.Len(); got != 1 {
		t.Errorf("held %d grants after the sweep, want only the fresh one", got)
	}
}

// movingForge is a fake forge whose branch head MOVES, like a real one. Like
// GitLab's archive endpoint, RepoArchive accepts a branch name OR a sha and
// resolves a name to whatever the head is at the moment of the call -- which is
// exactly the defect gonk-qdr4 is about.
type movingForge struct {
	heads    map[string]string // branch -> sha
	archives map[string]string // sha -> tree
	asked    []string
}

func (m *movingForge) push(branch, sha string) { m.heads[branch] = sha }

func (m *movingForge) RepoArchive(_ context.Context, _ int64, ref string, _ int64) ([]byte, error) {
	m.asked = append(m.asked, ref)
	if sha, ok := m.heads[ref]; ok {
		ref = sha
	}
	tree, ok := m.archives[ref]
	if !ok {
		return nil, fmt.Errorf("404 no such ref %q", ref)
	}
	return []byte(tree), nil
}

// gonk-qdr4: a push landing between the grant and the pod's fetch must NOT change
// the tree the pod receives. The grant pins sha1; main then moves to sha2; the
// pod still gets sha1's tree, and the forge was asked for the sha, not the name.
func TestAPushAfterGrantStillServesTheGrantedCommit(t *testing.T) {
	f := &movingForge{
		heads:    map[string]string{"main": sha1},
		archives: map[string]string{sha1: "tree-at-grant", sha2: "tree-after-push"},
	}
	_, mux := newHandler(t, f)
	if rec := grant(t, mux, "alias-a", `{"project":"p","project_id":1,"sha":"`+sha1+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body)
	}
	f.push("main", sha2) // the push between grant and fetch

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rig/alias-a.tar.gz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != "tree-at-grant" {
		t.Errorf("served %q after a push, want the tree at the granted commit", got)
	}
	if len(f.asked) != 1 || f.asked[0] != sha1 {
		t.Errorf("forge asked for %v, want exactly the granted sha %s", f.asked, sha1)
	}
	if got := rec.Header().Get("X-Gonk-Rig-Ref"); got != sha1 {
		t.Errorf("header = %q, want the granted sha", got)
	}
}

// A branch NAME (or an abbreviated sha) is not a pin and must be refused at
// registration, over the wire and in the store, so the defect cannot return by a
// caller that forgets to resolve.
func TestAGrantMustBeAFullCommitSHA(t *testing.T) {
	for _, bad := range []string{"main", "abc123", sha1[:39], strings.Repeat("A", 40), sha1 + "0", ""} {
		_, mux := newHandler(t, &stubFetcher{})
		if rec := grant(t, mux, "alias-a", `{"project":"p","project_id":1,"sha":"`+bad+`"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("sha %q: register = %d, want 400", bad, rec.Code)
		}
		if err := NewStore(nil).Register("alias-a", Grant{Project: "p", ProjectID: 1, SHA: bad}); err == nil {
			t.Errorf("sha %q: Store.Register accepted a non-SHA", bad)
		}
	}
	// The pre-qdr4 wire shape (a branch name under "ref") is refused too.
	_, mux := newHandler(t, &stubFetcher{})
	if rec := grant(t, mux, "alias-a", `{"project":"p","project_id":1,"ref":"main"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("legacy ref body: register = %d, want 400", rec.Code)
	}
}
