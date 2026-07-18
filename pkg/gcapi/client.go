// Package gcapi is a minimal typed client for the ONE Gas City supervisor route
// gonk uses: firing an order.
//
//	POST /v0/city/{cityName}/order/{name}/run   {"vars": {k: v}}
//	                                        ->  {status, scoped_name, tracking_id}
//
// (Source: gascity's docs/tutorials/07-orders.md and the supervisor's route
// table. gascity is MIT. NOTHING in this package is derived from gascity-packs,
// which is all-rights-reserved -- see the licensing constraint at the top of
// Plan 04.)
//
// SECURITY NOTE, and it is not a small one: the order-run route is guarded by
// TWO independent gates, and network position is no longer the only one.
//
//  1. ed25519 WRITE-AUTH (X-GC-City-Write). When a *Signer is configured on the
//     Client, every mutating request carries a single-use, request-bound
//     ed25519 grant token (see writeauth.go) plus the X-GC-Request CSRF header.
//     The server holds only the public key and verifies the signature and the
//     method+path+query+body binding, so an attacker who can merely reach the
//     port can no longer pour a formula -- they would also need the private key.
//     When NO Signer is configured the client sends no grant headers, exactly
//     as before write-auth existed: that preserves the in-pod loopback path,
//     where admission is by network position, and is the back-compat default.
//  2. NETWORK POSITION still matters. The supervisor MUST NOT be exposed beyond
//     the namespace (spec 9); gonk's Ingress exposes /hook/gitlab and nothing
//     else (Plan 05). Write-auth is defense in depth on top of that, not a
//     license to expose the port.
//
// And, independent of both: pouring a formula is NOT the same as spending
// money. Every pour goes through `gonk-gate dispatch`, which asks gonk-meter
// /v1/policy/decide first. That is Gate 2, and it is the enforcement point
// rather than an optimization.
package gcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultPort is the supervisor's REST/SSE port.
const DefaultPort = 8372

type Client struct {
	BaseURL      string // e.g. http://gascity.gascity.svc:8372
	City         string // OD-1: from GONK_CITY. NO DEFAULT. An empty city is an error.
	HTTP         *http.Client
	MaxRetries   int
	RetryBackoff func(attempt int) time.Duration
	UserAgent    string
	// Signer, when non-nil, mints an ed25519 X-GC-City-Write grant (and sends
	// the X-GC-Request CSRF header) on every mutating request. When nil, no
	// grant headers are sent -- the loopback / network-position path, unchanged.
	Signer *Signer
}

func New(baseURL, city string) *Client {
	return &Client{
		BaseURL:      strings.TrimSuffix(baseURL, "/"),
		City:         city,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		MaxRetries:   3,
		RetryBackoff: func(a int) time.Duration { return time.Duration(1<<a) * 250 * time.Millisecond },
		UserAgent:    "gonk-gate",
	}
}

// RunResult is the supervisor's answer.
type RunResult struct {
	Status     string `json:"status"`
	ScopedName string `json:"scoped_name"`
	TrackingID string `json:"tracking_id"`
}

// APIError is any non-2xx response. It carries the status and a truncated
// RESPONSE body for diagnosis, and never the request's vars: a key_ref var is
// a Secret NAME, never key material, but there is no reason to echo caller
// input into a string that ends up in every log line that captures err.Error().
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	if hint := e.authHint(); hint != "" {
		return fmt.Sprintf("gascity: POST %s: %d: %s [%s]", e.Path, e.Status, e.Body, hint)
	}
	return fmt.Sprintf("gascity: POST %s: %d: %s", e.Path, e.Status, e.Body)
}

// authHint turns a write-auth / CSRF / host rejection into a diagnostic string,
// so a misconfiguration reads as a misconfiguration and NOT as "order not
// found". The distinctions come from status + the server's (deliberately
// generic) problem-JSON detail; see the gascity failure-mode table.
func (e *APIError) authHint() string {
	body := strings.ToLower(e.Body)
	switch {
	case e.Status == http.StatusUnauthorized && strings.Contains(body, "x-gc-city-write"):
		return "write-auth: server requires an X-GC-City-Write grant but none was sent -- configure a Signer on the gcapi.Client"
	case e.Status == http.StatusForbidden && strings.Contains(body, "write grant rejected"):
		return "write-auth: grant rejected -- wrong kid/key/cid/epoch/audience or clock skew; a persistent 403 here is a config error, not a missing order"
	case e.Status == http.StatusForbidden && strings.Contains(body, "csrf"):
		return "write-auth: CSRF gate -- the X-GC-Request header is missing or rejected"
	case e.Status == http.StatusForbidden && strings.Contains(body, "read_only"):
		return "write-auth: server is read-only (non-loopback bind without allow_mutations)"
	case e.Status == http.StatusMisdirectedRequest: // 421
		return "write-auth: supervisor Host not allowed -- add the client's Host to the server's allowed_hosts"
	default:
		return ""
	}
}

// IsNotFound: on the order-run route a 404 nearly always means the CITY NAME is
// wrong (OD-1) or the order is not in the loaded pack. Both are configuration
// errors, and both are loud -- which is the failure mode we chose deliberately
// over guessing a default city name.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// IsWriteAuthRequired reports a 401 "missing X-GC-City-Write grant": the server
// enforces write-auth but the client sent no grant (no Signer configured).
func IsWriteAuthRequired(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized &&
		strings.Contains(strings.ToLower(ae.Body), "x-gc-city-write")
}

// IsWriteGrantRejected reports a 403 "write grant rejected": the grant was
// verified and refused. The server does not disclose which check failed (bad
// sig, unknown kid, expired, wrong audience/city/cid, req mismatch, replay,
// epoch below floor) -- but a persistent one means the key/kid/cid/epoch/aud is
// misconfigured, so surface it rather than retrying forever.
func IsWriteGrantRejected(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(ae.Body), "write grant rejected")
}

// IsCSRFRejected reports a 403 CSRF failure (X-GC-Request missing/rejected).
func IsCSRFRejected(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(ae.Body), "csrf")
}

// IsReadOnly reports a 403 read-only rejection (server bound to a non-loopback
// address without allow_mutations).
func IsReadOnly(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(ae.Body), "read_only")
}

// IsHostNotAllowed reports a 421 host_not_allowed (supervisor-mode Host
// validation): the request Host is neither loopback nor in allowed_hosts.
func IsHostNotAllowed(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusMisdirectedRequest
}

// maxResponseBytes caps the supervisor's response body. gascity is on the
// trust boundary (spec 9): a misbehaving or compromised supervisor answering
// with an unbounded body must not be read into memory whole.
const maxResponseBytes = 1 << 16

type runBody struct {
	// Vars is never nil on the wire: a nil map marshals to `null`, and `{}` and
	// `null` are not the same document.
	Vars map[string]string `json:"vars"`
}

// RunOrder fires one order. Vars become the order's declared [order.params],
// which Gas City namespaces into an exec order's environment as GC_WEBHOOK_ARG_*.
func (c *Client) RunOrder(ctx context.Context, name string, vars map[string]string) (*RunResult, error) {
	if c.City == "" {
		return nil, errors.New("gascity: city name is empty (set GONK_CITY -- there is no default, and an empty one builds a route that silently is not the one you meant)")
	}
	if name == "" {
		return nil, errors.New("gascity: order name is empty")
	}
	if vars == nil {
		vars = map[string]string{}
	}
	path := fmt.Sprintf("/v0/city/%s/order/%s/run", url.PathEscape(c.City), url.PathEscape(name))
	payload, err := json.Marshal(runBody{Vars: vars})
	if err != nil {
		return nil, fmt.Errorf("gascity: encode vars: %w", err)
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", c.UserAgent)

		// Mint a FRESH grant every attempt: new jti/iat/exp, and the req digest
		// recomputed over the exact method/path/query/body on the wire. Doing
		// this inside the loop is required -- the server's replay guard is
		// single-use per jti, so a reused token on a retry would be rejected.
		// We digest req.URL.Path / req.URL.RawQuery (what the server derives
		// from the wire), not the pre-escape strings.
		if c.Signer != nil {
			token, err := c.Signer.mintGrant(req.Method, req.URL.Path, req.URL.RawQuery, c.City, payload, time.Now())
			if err != nil {
				return nil, fmt.Errorf("gascity: POST %s: %w", path, err)
			}
			req.Header.Set("X-GC-Request", "true")
			req.Header.Set("X-GC-City-Write", token)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("gascity: POST %s: %w", path, err)
		} else {
			body, rerr := readCapped(resp.Body, maxResponseBytes)
			_ = resp.Body.Close()
			switch {
			case rerr != nil:
				return nil, fmt.Errorf("gascity: POST %s: %w", path, rerr)
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				var out RunResult
				if err := json.Unmarshal(body, &out); err != nil {
					return nil, fmt.Errorf("gascity: POST %s: decode: %w", path, err)
				}
				return &out, nil
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				lastErr = &APIError{Status: resp.StatusCode, Path: path, Body: truncate(body)}
			default:
				return nil, &APIError{Status: resp.StatusCode, Path: path, Body: truncate(body)}
			}
		}
		if attempt >= c.MaxRetries {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.RetryBackoff(attempt)):
		}
	}
}

// readCapped reads at most limit bytes and errors if the body is longer,
// rather than silently truncating (a truncated response could parse as a
// *different, valid* document). Same semantics as pkg/glab's helper of the
// same name.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response too large (> %d bytes)", limit)
	}
	return b, nil
}

// truncate bounds a RESPONSE body for embedding in an error. It never sees the
// request body, so it cannot leak a request var (see APIError's doc comment).
func truncate(b []byte) string {
	const n = 256
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
