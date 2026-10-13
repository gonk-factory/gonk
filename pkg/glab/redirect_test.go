package glab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// gonk-467e. PRIVATE-TOKEN is a custom header, and Go's http.Client copies
// custom headers onto EVERY redirect, to any host (it drops only
// Authorization, WWW-Authenticate, Cookie and Cookie2 when the host changes).
// Until 2026-10-12 this client had no redirect policy, so a 302 from GitLab
// -- object storage with proxy_download off, a reverse proxy, a hostile
// Location -- carried the bot's token to the host it named.

// seen records what one test server received.
type seen struct {
	mu     sync.Mutex
	tokens []string
	paths  []string
	bodies []string
}

func (s *seen) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, r.Header.Get("PRIVATE-TOKEN"))
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	buf := make([]byte, 1<<10)
	n, _ := r.Body.Read(buf)
	s.bodies = append(s.bodies, string(buf[:n]))
}

func (s *seen) snapshot() (tokens, paths, bodies []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens...), append([]string(nil), s.paths...), append([]string(nil), s.bodies...)
}

// otherHost is a second server: another origin (same address, another port).
func otherHost(t *testing.T, h http.HandlerFunc) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

func redirectClient(t *testing.T, h http.HandlerFunc) (*Client, *seen) {
	t.Helper()
	s := &seen{}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		h(w, r)
	}))
	c.SetPreviousToken("prev-s3cret")
	c.AdminToken = "admin-s3cret"
	return c, s
}

func noToken(t *testing.T, what string, s *seen) {
	t.Helper()
	tokens, paths, _ := s.snapshot()
	for i, tok := range tokens {
		if tok != "" {
			t.Errorf("%s: %s was sent PRIVATE-TOKEN %q", what, paths[i], tok)
		}
	}
}

// A read redirected to ANOTHER origin is followed -- that is how GitLab serves
// a raw file, a trace or an archive from object storage, with a pre-signed
// URL -- and the token does not go with it.
func TestACrossOriginRedirectIsFollowedWithoutTheToken(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			other, got := otherHost(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, "the file") })
			c, origin := redirectClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+"/bucket/object?X-Signature=abc", status)
			})
			body, err := c.GetRawFile(context.Background(), 75, "a.txt", "main", 1<<10)
			if err != nil || string(body) != "the file" {
				t.Fatalf("GetRawFile = %q, %v; want the redirected body", body, err)
			}
			_, paths, _ := got.snapshot()
			if len(paths) != 1 || paths[0] != "GET /bucket/object" {
				t.Fatalf("the other origin saw %v, want one GET", paths)
			}
			noToken(t, "the other origin", got)
			if tokens, _, _ := origin.snapshot(); len(tokens) != 1 || tokens[0] != "s3cret" {
				t.Errorf("the origin saw tokens %q, want the one request with the token", tokens)
			}
		})
	}
}

// Neither the rotation slot nor the admin token crosses either.
func TestNoTokenSlotCrossesAnOrigin(t *testing.T) {
	other, got := otherHost(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `[]`) })
	c, _ := redirectClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") == "s3cret" {
			w.WriteHeader(http.StatusUnauthorized) // forces the previous-token retry
			return
		}
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
	})
	if _, _, err := c.do(context.Background(), request{method: http.MethodGet, path: "/api/v4/user"}); err != nil {
		t.Fatalf("previous-slot request: %v", err)
	}
	if _, _, err := c.do(context.Background(), request{method: http.MethodGet, path: "/api/v4/hooks", admin: true}); err != nil {
		t.Fatalf("admin request: %v", err)
	}
	if _, paths, _ := got.snapshot(); len(paths) != 2 {
		t.Fatalf("the other origin saw %v, want both redirected requests", paths)
	}
	noToken(t, "the other origin", got)
}

// THE NEAR MISS: a redirect that stays on the origin is an ordinary one (a
// renamed project path, a trailing slash) and keeps the token, hop after hop.
func TestASameOriginRedirectKeepsTheToken(t *testing.T) {
	c, origin := redirectClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/user":
			http.Redirect(w, r, "/api/v4/moved/1", http.StatusFound) // relative
		case "/api/v4/moved/1":
			http.Redirect(w, r, "http://"+r.Host+"/api/v4/moved/2", http.StatusMovedPermanently) // absolute, same origin
		default:
			_, _ = fmt.Fprint(w, `{"id":7,"username":"gonk"}`)
		}
	})
	u, err := c.CurrentUser(context.Background())
	if err != nil || u.ID != 7 {
		t.Fatalf("CurrentUser = %+v, %v", u, err)
	}
	tokens, paths, _ := origin.snapshot()
	if len(paths) != 3 {
		t.Fatalf("the origin saw %v, want three hops", paths)
	}
	for i, tok := range tokens {
		if tok != "s3cret" {
			t.Errorf("hop %s: PRIVATE-TOKEN = %q, want the token (same origin)", paths[i], tok)
		}
	}
}

// Once a chain has left the origin the token is gone for the rest of it,
// even when a later hop points BACK at the origin: the other host chose that
// URL, and gonk's credentials are not its to aim.
func TestTheTokenDoesNotComeBackAfterAChainLeftTheOrigin(t *testing.T) {
	var originURL string
	other, got := otherHost(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, originURL+"/api/v4/projects/1/chosen-by-the-other-host", http.StatusFound)
	})
	c, origin := redirectClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/user" {
			http.Redirect(w, r, other.URL+"/bounce", http.StatusFound)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":7}`)
	})
	originURL = c.BaseURL
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	noToken(t, "the other origin", got)
	tokens, paths, _ := origin.snapshot()
	if len(paths) != 2 || tokens[0] != "s3cret" || tokens[1] != "" {
		t.Errorf("the origin saw %v with tokens %q; want the token on the first request only", paths, tokens)
	}
}

// A WRITE is never redirected to another origin, token or not: a 307/308
// would carry the body (a note's text, a commit's content) to the host the
// Location names, and a 301/302/303 would turn it into a GET there and report
// the answer as the write's result.
func TestAWriteIsNotRedirectedToAnotherOrigin(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			other, got := otherHost(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `{"id":1}`) })
			c, origin := redirectClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+"/collect?X-Signature=presigned", status)
			})
			_, err := c.CreateIssueNote(context.Background(), 75, 1, "the note's text")
			if !errors.Is(err, ErrCrossOriginRedirect) {
				t.Fatalf("a POST redirected to another origin: %v, want %v", err, ErrCrossOriginRedirect)
			}
			// In our words: not Go's url.Error, which prints the Location --
			// the other host's text, and a pre-signed URL when it is storage.
			for _, secret := range []string{"s3cret", "the note's text", "collect", "presigned", other.URL} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error carries %q: %v", secret, err)
				}
			}
			if _, paths, _ := got.snapshot(); len(paths) != 0 {
				t.Errorf("the other origin was sent %v", paths)
			}
			// And not retried: the write went to the origin once.
			if _, paths, _ := origin.snapshot(); len(paths) != 1 {
				t.Errorf("the origin saw %v, want the one POST", paths)
			}
		})
	}
}

// The other near misses of "same origin": another port, another scheme and
// another host name are each ANOTHER origin; the same host in another letter
// case is the same one.
func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://gitlab.example/api/v4/x", "https://gitlab.example/y", true},
		{"https://gitlab.example/api/v4/x", "https://GitLab.Example/y", true},
		{"https://gitlab.example/x", "https://gitlab.example:443/y", true},
		{"http://gitlab.example/x", "http://gitlab.example:80/y", true},
		{"https://gitlab.example:8443/x", "https://gitlab.example:8443/y", true},
		{"https://gitlab.example/x", "https://gitlab.example:8443/y", false},
		{"https://gitlab.example/x", "http://gitlab.example/y", false},
		{"http://gitlab.example/x", "https://gitlab.example/y", false},
		{"https://gitlab.example:8443/x", "http://gitlab.example:8443/y", false},
		{"http://gitlab.example:8443/x", "https://gitlab.example:8443/y", false},
		{"HTTPS://gitlab.example/x", "https://gitlab.example/y", true},
		{"https://gitlab.example/x", "https://gitlab.example.evil.test/y", false},
		{"https://gitlab.example/x", "https://evil.test/gitlab.example", false},
		{"https://gitlab.example/x", "https://user@evil.test/y", false},
		{"https://gitlab.example/x", "https://storage.gitlab.example/y", false},
		{"http://127.0.0.1:8080/x", "http://127.0.0.1:8081/y", false},
		{"http://127.0.0.1:8080/x", "http://localhost:8080/y", false},
	} {
		ra, _ := http.NewRequest(http.MethodGet, tc.a, nil)
		rb, _ := http.NewRequest(http.MethodGet, tc.b, nil)
		if got := sameOrigin(ra.URL, rb.URL); got != tc.want {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// The policy is the CLIENT's, not the http.Client's: a caller that swaps
// c.HTTP (a test transport, a timeout) keeps it, and its own CheckRedirect
// still runs.
func TestTheRedirectPolicySurvivesAReplacedHTTPClient(t *testing.T) {
	other, got := otherHost(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `{"id":7}`) })
	c, _ := redirectClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	})
	called := 0
	c.HTTP = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { called++; return nil }}
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	noToken(t, "the other origin", got)
	if called != 1 {
		t.Errorf("the caller's own CheckRedirect ran %d times, want 1", called)
	}
	// And a redirect loop still ends (Go's default limit of 10 is kept when
	// the caller set no policy of its own).
	loop, _ := redirectClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, r.URL.Path, http.StatusFound) })
	loop.MaxRetries = 0
	if _, err := loop.CurrentUser(context.Background()); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("a redirect loop: %v, want Go's stopped-after-10-redirects error", err)
	}
}

// Nor does it reappear on the OTHER host's own second hop: "left the origin"
// is measured from the first request, not from the hop before.
func TestTheTokenStaysOffWhileAChainStaysAway(t *testing.T) {
	other, got := otherHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":7}`)
	})
	c, _ := redirectClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/first", http.StatusFound)
	})
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if _, paths, _ := got.snapshot(); len(paths) != 2 {
		t.Fatalf("the other origin saw %v, want both hops", paths)
	}
	noToken(t, "the other origin", got)
}
