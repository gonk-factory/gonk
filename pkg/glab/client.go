// Package glab is a minimal typed client for the subset of the GitLab REST API
// that gonk-intake uses. It is deliberately not a full SDK: the client is on
// the trust boundary, so it owns its own response size caps, retry policy, and
// error type, and it never logs or embeds the private token.
package glab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxBytes caps any single response body. GitLab is semi-trusted, but a
// project's repository content is not.
const DefaultMaxBytes int64 = 1 << 20

type Client struct {
	BaseURL string // e.g. https://gitlab.example.internal  (PRIVATE CA -- see below)
	token   string
	// prevToken is rotation slot 2 (AD-4b). The PAT is a credential we PRESENT,
	// not one we verify, so the second slot is a FALLBACK: on a 401 with the
	// current token we retry the request ONCE with this one, loudly. Empty means
	// no fallback, which is the normal steady state.
	//
	// 401 ONLY, deliberately not 403. 401 is "this token is not recognised",
	// which is exactly what mid-rotation looks like. 403 is "this token is
	// recognised but not allowed" -- a permissions problem the previous slot
	// cannot fix, so retrying with it would just spend a second round trip
	// confirming the same no.
	prevToken string
	HTTP      *http.Client
	// AdminToken, when non-empty, is used ONLY for webhook management
	// (split-credential mode, spec 5.1). Empty means bot-does-everything.
	AdminToken string
	MaxBytes   int64
	MaxRetries int
	// MaxPages bounds a single paginate walk. GitLab is semi-trusted: a
	// misbehaving or MITM'd server that always advertises X-Next-Page one
	// greater than requested would otherwise drive paginate to unbounded
	// requests (the self-reference / non-numeric guards don't catch a
	// monotonic hostile sequence). 1000 pages * per_page=100 = 100k items is
	// far past any real bot membership (a group with thousands of projects),
	// while still finite. Exceeding it is an error, not a silent truncation: a
	// short project list would make the reconciler act on a partial view.
	MaxPages     int
	RetryBackoff func(attempt int) time.Duration
	UserAgent    string
	// AuthFallback is called when slot 1 was rejected and slot 2 was used
	// instead. cmd/gonk-intake wires it to a Prometheus counter
	// (gonk_intake_gitlab_auth_fallback_total): a rotation that quietly
	// half-completed must be visible, not merely survivable.
	AuthFallback func()
}

// SetPreviousToken installs rotation slot 2. It is a setter, not an exported
// field, for the same reason `token` is unexported: nothing may read the
// material back out, and nothing may print the struct.
func (c *Client) SetPreviousToken(tok string) { c.prevToken = tok }

// New builds a client for baseURL.
//
// *** THE http.Client HAS NO TLSClientConfig, AND MUST NEVER GET ONE. ***
// A self-hosted GitLab often serves a certificate signed by a PRIVATE CA
// (docs/environment.md, "Optional: a private certificate authority"). The
// fix is to TRUST the CA, not to skip verification: the chart mounts the CA
// ConfigMap and adds its directory to SSL_CERT_DIR beside the system roots,
// which crypto/x509 honours with no code here at all. A single InsecureSkipVerify in
// this package would let the component that guards the money path talk to
// anything that answers on port 443.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:      strings.TrimSuffix(baseURL, "/"),
		token:        token,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		MaxBytes:     DefaultMaxBytes,
		MaxRetries:   3,
		MaxPages:     1000,
		RetryBackoff: func(a int) time.Duration { return time.Duration(1<<a) * 250 * time.Millisecond },
		UserAgent:    "gonk-intake",
	}
}

// APIError is any non-2xx response. It carries the status and a truncated body
// for diagnosis, and never the token.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitlab: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func IsForbidden(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusUnauthorized)
}

type request struct {
	method   string
	path     string // begins with /api/v4
	query    map[string]string
	body     any   // JSON-encoded when non-nil
	admin    bool  // use AdminToken when set (hook management)
	maxBytes int64 // 0 -> Client.MaxBytes
	usedPrev bool  // AD-4b: this attempt is the one-shot retry on rotation slot 2
	// noRetry surfaces a 5xx immediately, like a POST, for a non-POST write that
	// is not safe to repeat blind (the merge: a 5xx does not say whether GitLab
	// merged before failing to answer, and a second PUT would then be refused
	// as "already merged" and read as a failure).
	noRetry bool
	// headers are extra request headers (e.g. Range). Never the token.
	headers map[string]string
	// consume, when non-nil, takes over reading a 2xx response body instead of
	// the readCapped-into-memory path: the caller streams and bounds it itself
	// (GetJobTraceTail). Non-2xx bodies are still read capped, so errors keep
	// their truncated body. do returns a nil body in this mode.
	consume func(resp *http.Response) error
}

// do sends one request with retries on 429 and 5xx, EXCEPT a 5xx on a POST:
// every POST this client sends creates something, so a 5xx there is surfaced
// immediately rather than retried blind (see the StatusCode >= 500 case
// below). It returns the raw body and the response headers (pagination lives
// in X-Next-Page).
func (c *Client) do(ctx context.Context, rq request) ([]byte, http.Header, error) {
	limit := rq.maxBytes
	if limit == 0 {
		limit = c.MaxBytes
	}
	var payload []byte
	if rq.body != nil {
		var err error
		if payload, err = json.Marshal(rq.body); err != nil {
			return nil, nil, fmt.Errorf("gitlab: encode %s %s: %w", rq.method, rq.path, err)
		}
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, rq.method, c.BaseURL+rq.path, bytes.NewReader(payload))
		if err != nil {
			return nil, nil, err
		}
		if len(rq.query) > 0 {
			q := req.URL.Query()
			for k, v := range rq.query {
				q.Set(k, v)
			}
			req.URL.RawQuery = q.Encode()
		}
		tok := c.token
		if rq.usedPrev {
			tok = c.prevToken // AD-4b: the one-shot rotation fallback
		}
		if rq.admin && c.AdminToken != "" {
			tok = c.AdminToken
		}
		req.Header.Set(tokenHeader, tok)
		req.Header.Set("User-Agent", c.UserAgent)
		if rq.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range rq.headers {
			req.Header.Set(k, v)
		}

		resp, err := c.httpClient().Do(req)
		if errors.Is(err, ErrCrossOriginRedirect) {
			// Not retried, and reported in our words: the url.Error Go wraps
			// it in prints the Location, which is the other host's text.
			return nil, nil, fmt.Errorf("gitlab: %s %s: %w", rq.method, rq.path, ErrCrossOriginRedirect)
		}
		if err != nil {
			lastErr = fmt.Errorf("gitlab: %s %s: %w", rq.method, rq.path, err)
		} else if rq.consume != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			cerr := rq.consume(resp)
			_ = resp.Body.Close()
			if cerr != nil {
				return nil, nil, fmt.Errorf("gitlab: %s %s: %w", rq.method, rq.path, cerr)
			}
			return nil, resp.Header, nil
		} else {
			// An ERROR body is read under its own floor, not the caller's cap:
			// the cap bounds the RESOURCE, and a caller asking for 1 byte (a
			// presence check) must still get a 404 as a 404, not as "too large".
			capFor := limit
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				capFor = max(limit, errorBodyCap)
			}
			body, rerr := readCapped(resp.Body, capFor)
			_ = resp.Body.Close()
			switch {
			case rerr != nil:
				return nil, nil, fmt.Errorf("gitlab: %s %s: %w", rq.method, rq.path, rerr)
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return body, resp.Header, nil
			case resp.StatusCode == http.StatusTooManyRequests:
				lastErr = &APIError{Status: resp.StatusCode, Method: rq.method, Path: rq.path, Body: truncate(body)}
			case resp.StatusCode >= 500:
				apiErr := &APIError{Status: resp.StatusCode, Method: rq.method, Path: rq.path, Body: truncate(body)}
				if rq.method == http.MethodPost || rq.noRetry {
					// POST is not idempotent here: every POST this client sends
					// CREATES something (an issue, a note/comment, a branch, a
					// commit, an MR, a hook), and a 5xx does not tell us whether
					// GitLab applied it before failing to answer. Retrying blind
					// risks a duplicate a human can see -- most visibly a second
					// bot comment from CreateIssueNote, which is exactly the
					// failure mode E3 forbids. Surface the error instead.
					return nil, nil, apiErr
				}
				lastErr = apiErr
			case resp.StatusCode == http.StatusUnauthorized &&
				c.prevToken != "" && !rq.usedPrev && !rq.admin:
				// AD-4b: rotation slot 2. Slot 1 was rejected -- we are mid-rotation
				// and the pod still holds the old value, or holds the new one before
				// GitLab activated it. Retry ONCE with the previous slot, count it,
				// and let the next reconcile pass proceed. Never loop: usedPrev makes
				// this exactly one extra attempt.
				if c.AuthFallback != nil {
					c.AuthFallback()
				}
				rq.usedPrev = true
				continue
			default:
				// 4xx other than 429: retrying cannot help.
				return nil, nil, &APIError{Status: resp.StatusCode, Method: rq.method, Path: rq.path, Body: truncate(body)}
			}
		}
		if attempt >= c.MaxRetries {
			return nil, nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(c.RetryBackoff(attempt)):
		}
	}
}

// errorBodyCap is the least an error response's body is read up to (its
// message is all that is kept, truncated).
const errorBodyCap = 64 << 10

// ErrResponseTooLarge is wrapped into the error a capped read returns when the
// body is longer than its cap. A 200 that was too large still proves the
// resource EXISTS, which a presence check (a 1-byte GetRawFile) can rely on.
var ErrResponseTooLarge = errors.New("response too large")

// readCapped reads at most limit bytes and errors if the body is longer, rather
// than silently truncating (a truncated .gonk.yml could parse as a *different,
// valid* config).
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w (> %d bytes)", ErrResponseTooLarge, limit)
	}
	return b, nil
}

func truncate(b []byte) string {
	const n = 256
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (c *Client) getJSON(ctx context.Context, rq request, out any) error {
	body, _, err := c.do(ctx, rq)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("gitlab: %s %s: decode: %w", rq.method, rq.path, err)
	}
	return nil
}

// paginate walks X-Next-Page, appending each page's decoded items.
func paginate[T any](ctx context.Context, c *Client, rq request) ([]T, error) {
	var all []T
	page := "1"
	for pages := 0; ; pages++ {
		// Bound the walk like MaxBytes/MaxRetries: a hostile monotonic
		// X-Next-Page sequence must error, not loop or truncate.
		if c.MaxPages > 0 && pages >= c.MaxPages {
			return nil, fmt.Errorf("glab: pagination exceeded %d pages for %s", c.MaxPages, rq.path)
		}
		q := map[string]string{"per_page": "100", "page": page}
		for k, v := range rq.query {
			q[k] = v
		}
		body, hdr, err := c.do(ctx, request{method: rq.method, path: rq.path, query: q, admin: rq.admin})
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("gitlab: %s %s: decode: %w", rq.method, rq.path, err)
		}
		all = append(all, items...)
		next := hdr.Get("X-Next-Page")
		if next == "" || next == page {
			return all, nil
		}
		if _, err := strconv.Atoi(next); err != nil {
			return all, nil
		}
		page = next
	}
}

// asAPIError returns err's *APIError, or nil.
func asAPIError(err error) *APIError {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// ---- redirects (gonk-467e) -------------------------------------------------------

// tokenHeader carries the PAT. It is a CUSTOM header, so Go's http.Client
// copies it onto every redirect, to any host: only Authorization,
// WWW-Authenticate, Cookie and Cookie2 are dropped when the host changes.
const tokenHeader = "PRIVATE-TOKEN"

// ErrCrossOriginRedirect: a write was answered with a redirect to another
// origin, and was not followed.
var ErrCrossOriginRedirect = errors.New("gitlab: a write was redirected to another origin; not followed")

// maxRedirects is Go's own default limit, kept when the caller's http.Client
// has no CheckRedirect of its own (setting ours replaces the default).
const maxRedirects = 10

// httpClient is c.HTTP with THIS client's redirect policy in front of
// whatever policy c.HTTP has. It is built per request from a shallow copy
// (the transport and its connections are shared), so the policy holds
// whatever c.HTTP was replaced with.
//
// The policy, for a redirect chain that has LEFT the origin of the first
// request (scheme, host and port; sameOrigin):
//
//   - a read (GET, HEAD) is followed WITHOUT the token. GitLab answers a raw
//     file, a job trace or an archive with a 302 to object storage when
//     direct download is on, and that URL is pre-signed: it needs no token
//     and must not be given one.
//   - the token stays off for the rest of the chain, even on a hop that
//     points back at the origin: the other host chose that URL.
//   - a write (anything else) is NOT followed at all (ErrCrossOriginRedirect):
//     a 307/308 would carry the body to the named host, and a 301/302/303
//     would turn the write into a GET there and report its answer as the
//     write's.
//
// A chain that stays on the origin is untouched: token and all, as before.
func (c *Client) httpClient() *http.Client {
	hc := *c.HTTP
	inner := hc.CheckRedirect
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := redirectPolicy(req, via); err != nil {
			return err
		}
		if inner != nil {
			return inner(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
	return &hc
}

// redirectPolicy is the rule httpClient documents. req is the request about
// to be sent; via holds the ones already made, the first being ours.
func redirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	first := via[0]
	left := !sameOrigin(first.URL, req.URL) // redirect:leaves
	for _, v := range via[1:] {
		if !sameOrigin(first.URL, v.URL) { // redirect:left-earlier
			left = true
		}
	}
	if !left {
		return nil
	}
	if first.Method != http.MethodGet && first.Method != http.MethodHead { // redirect:write
		return ErrCrossOriginRedirect
	}
	req.Header.Del(tokenHeader) // redirect:strip
	return nil
}

// sameOrigin reports whether two URLs have the same scheme, host name and
// port (the scheme's default port when none is written). Host names compare
// without regard to letter case and in no other way: a subdomain, a suffix
// and another name for the same address are all other origins. http and
// https are different origins in both directions.
func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		switch strings.ToLower(u.Scheme) {
		case "http":
			return "80"
		case "https":
			return "443"
		}
		return ""
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
