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
// SECURITY NOTE, and it is not a small one: THE ORDER-RUN ROUTE HAS NO
// PER-ROUTE AUTH SCHEME. Admission to it is by NETWORK POSITION. Anything that
// can reach the supervisor's port (:8372) can pour a formula. Two consequences,
// both of which the rest of gonk is built around:
//
//  1. The supervisor MUST NOT be exposed beyond the namespace (spec 9). gonk's
//     Ingress exposes /hook/gitlab and nothing else (Plan 05).
//  2. Pouring a formula is NOT the same as spending money. Every pour goes
//     through `gonk-gate dispatch`, which asks gonk-meter /v1/policy/decide
//     first. That is Gate 2, and this is precisely why it is the enforcement
//     point rather than an optimization.
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
	return fmt.Sprintf("gascity: POST %s: %d: %s", e.Path, e.Status, e.Body)
}

// IsNotFound: on the order-run route a 404 nearly always means the CITY NAME is
// wrong (OD-1) or the order is not in the loaded pack. Both are configuration
// errors, and both are loud -- which is the failure mode we chose deliberately
// over guessing a default city name.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
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
