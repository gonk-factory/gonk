package gcapi

// Correlating an async 202 with its real outcome.
//
// gascity's session routes are ASYNC IN THE HANDLER, not merely async in the
// runtime: humaHandleSessionSubmit captures a request id and an event cursor,
// returns 202 "accepted", and only THEN -- in a goroutine -- resolves the
// session and delivers the message (GASCITY_REF
// internal/api/huma_handlers_sessions_command.go). Nothing about the outcome is
// in the HTTP response, and a submit against a session that does not exist is
// answered 202 exactly like one that lands.
//
// PROVEN LIVE 2026-08-01 against ns gonk: submitting to a nonexistent alias
// returned 202 and then emitted
//     type=request.failed
//     payload={request_id, operation:"session.submit",
//              error_code:"resolve_failed", error_message:"session not found: ..."}
// while a submit that landed emitted
//     type=request.result.session.submit
//     payload={request_id, session_id, queued, intent}
//
// That is why the 202 body carries request_id AND event_cursor: they are the
// correlation handle. The city event log is the ONLY place the outcome exists,
// so a caller that ignores them cannot distinguish "delivered" from "dropped" --
// which is precisely how gonk shipped a prompt-delivery path that reported
// success while every agent sat at an idle splash (gonk-u1p.7).
//
// The read is UNSIGNED (cityGet), like GetSessionOutput.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Terminal event types for an async request. A request emits exactly one.
// Upstream's own SessionCreateOutput documents this as the contract: "Watch the
// city event stream for request.result.session.create,
// request.result.session.message, request.result.session.submit, or
// request.failed with this request_id."
const (
	// EventRequestFailed is the failure terminal for EVERY async operation;
	// payload.operation says which one, so a caller must match on request_id.
	EventRequestFailed = "request.failed"
	// EventSessionCreateResult is the success terminal for POST .../sessions.
	EventSessionCreateResult = "request.result.session.create"
	// EventSessionSubmitResult is the success terminal for POST .../submit.
	EventSessionSubmitResult = "request.result.session.submit"
)

// Async failure codes gonk must tell apart. The set is small and stable
// (emitSessionSubmitFailed has exactly two call sites upstream).
const (
	// ErrorCodeResolveFailed means the id/alias did not resolve to a session.
	// For gonk this is the NORMAL async-create window -- create returns 202 with
	// no session id and a live run took ~31s to actually start the pod -- so it
	// is retryable, not fatal.
	ErrorCodeResolveFailed = "resolve_failed"
	// ErrorCodeSubmitFailed means the session resolved but delivery failed. It
	// is retryable only when the reason is that the session is not live YET;
	// see RequestOutcome.Retryable.
	ErrorCodeSubmitFailed = "submit_failed"
)

// RequestOutcome is the terminal result of one async request, read off the city
// event log. OK is the ONLY proof of delivery gonk can obtain.
type RequestOutcome struct {
	RequestID string
	OK        bool
	// SessionID is the id the target resolved to (success only). The alias gonk
	// stamped is the handle it submits against; this is what upstream resolved
	// it to, which is worth logging once.
	SessionID string
	// Queued reports that the message was accepted but parked rather than
	// delivered into the live runtime this instant. Not a failure -- but not the
	// same as landing in the pane either, so it is surfaced rather than folded
	// into OK.
	Queued bool
	// ErrorCode / ErrorMessage are set when OK is false.
	ErrorCode    string
	ErrorMessage string
}

// Retryable reports whether re-submitting could plausibly succeed.
//
// resolve_failed is the async-create window: the session genuinely does not
// exist yet and will shortly. submit_failed is retryable ONLY when the session
// exists but is not live yet (gascity's session.ErrSessionInactive, "session is
// not active") -- the pod is still starting. Anything else is a real rejection
// and retrying it only burns the order's timeout budget.
func (o RequestOutcome) Retryable() bool {
	if o.OK {
		return false
	}
	switch o.ErrorCode {
	case ErrorCodeResolveFailed:
		return true
	case ErrorCodeSubmitFailed:
		// Matched on the message because upstream collapses every delivery
		// failure into the one code. Both spellings are checked: the sentinel's
		// own text and the "not found" phrasing the resolver uses.
		msg := strings.ToLower(o.ErrorMessage)
		return strings.Contains(msg, "not active") || strings.Contains(msg, "not found")
	default:
		return false
	}
}

func (o RequestOutcome) String() string {
	if o.OK {
		return fmt.Sprintf("ok (session=%s queued=%t)", o.SessionID, o.Queued)
	}
	return fmt.Sprintf("failed (code=%s: %s)", o.ErrorCode, o.ErrorMessage)
}

// wireEvent is the subset of gascity's WireEvent gonk reads.
type wireEvent struct {
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	Subject string          `json:"subject,omitempty"`
	Payload requestPayload  `json:"payload,omitempty"`
	Ts      json.RawMessage `json:"ts,omitempty"`
}

// requestPayload is the union of the two terminal payloads. Both are keyed by
// request_id, which is the only field gonk matches on.
type requestPayload struct {
	RequestID    string `json:"request_id"`
	Operation    string `json:"operation,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Queued       bool   `json:"queued,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type eventListResponse struct {
	Items []wireEvent `json:"items"`
	Total int         `json:"total"`
}

// defaultOutcomePollInterval paces AwaitRequestOutcome. Measured live (ns gonk,
// 2026-08-01): a resolve_failed terminal event lands ~262ms after the 202, and a
// successful delivery ~10.8s after it (the message is exec'd into the pod's tmux
// first). So this is a short poll against a bounded deadline, not a backoff --
// tight enough that the fast-failure retry loop is not paced by it, loose enough
// that a slow delivery costs ~20 requests rather than hundreds.
const defaultOutcomePollInterval = 500 * time.Millisecond

// AwaitRequestOutcome polls the city event log for the terminal event of one
// async request and returns what actually happened.
//
// successType is the operation's own success terminal (EventSessionCreateResult,
// EventSessionSubmitResult); failure is always EventRequestFailed, so both are
// watched and whichever names this request id wins.
//
// sinceCursor is the event_cursor from the 202 body: the city event sequence
// number as of the moment the request was accepted, so the terminal event is
// necessarily above it. It bounds the scan and, just as importantly, makes the
// scan immune to a stale event with a recycled request id.
//
// A deadline that expires WITHOUT a terminal event is an error, not a silent
// success: "I could not observe the outcome" and "it worked" are different
// answers, and conflating them is the bug this function exists to close.
func (c *Client) AwaitRequestOutcome(ctx context.Context, requestID, sinceCursor, successType string, timeout time.Duration) (*RequestOutcome, error) {
	if c.City == "" {
		return nil, errEmptyCity
	}
	if requestID == "" {
		return nil, fmt.Errorf("gascity: AwaitRequestOutcome: request id is required")
	}
	if successType == "" {
		return nil, fmt.Errorf("gascity: AwaitRequestOutcome: success event type is required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	var sinceSeq uint64
	if sinceCursor != "" {
		// A cursor gonk cannot parse must not silently become "scan everything":
		// treat it as no lower bound but keep going, since the request id match
		// is still exact.
		if n, err := strconv.ParseUint(strings.TrimSpace(sinceCursor), 10, 64); err == nil {
			sinceSeq = n
		}
	}

	interval := c.OutcomePollInterval
	if interval <= 0 {
		interval = defaultOutcomePollInterval
	}

	deadline := time.Now().Add(timeout)
	for {
		outcome, err := c.findRequestOutcome(ctx, requestID, successType, sinceSeq)
		if err != nil {
			return nil, err
		}
		if outcome != nil {
			return outcome, nil
		}
		if !time.Now().Add(interval).Before(deadline) {
			return nil, fmt.Errorf("gascity: no terminal event for request %s within %s: the outcome is UNKNOWN, not successful", requestID, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// findRequestOutcome does one sweep of both terminal event types. Each is read
// type-filtered rather than scanning the whole log, so a busy city cannot push
// the answer off the first page.
func (c *Client) findRequestOutcome(ctx context.Context, requestID, successType string, sinceSeq uint64) (*RequestOutcome, error) {
	for _, eventType := range []string{successType, EventRequestFailed} {
		events, err := c.listEvents(ctx, eventType, outcomeScanLimit)
		if err != nil {
			return nil, err
		}
		for _, ev := range events {
			if ev.Payload.RequestID != requestID || ev.Seq < sinceSeq {
				continue
			}
			out := &RequestOutcome{
				RequestID:    requestID,
				OK:           ev.Type == successType,
				SessionID:    ev.Payload.SessionID,
				Queued:       ev.Payload.Queued,
				ErrorCode:    ev.Payload.ErrorCode,
				ErrorMessage: ev.Payload.ErrorMessage,
			}
			if out.SessionID == "" {
				out.SessionID = ev.Subject
			}
			return out, nil
		}
	}
	return nil, nil
}

// outcomeScanLimit is the page size for one terminal-event sweep. Both types are
// low-volume (one row per async API request) and the answer is always near the
// head, so this only has to outrun concurrent requests, not city traffic.
const outcomeScanLimit = 50

// listEvents reads one page of the city event log, newest first. Unsigned.
func (c *Client) listEvents(ctx context.Context, eventType string, limit int) ([]wireEvent, error) {
	path := fmt.Sprintf("/v0/city/%s/events", url.PathEscape(c.City))
	q := url.Values{}
	if eventType != "" {
		q.Set("type", eventType)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	body, err := c.doRequest(ctx, http.MethodGet, path, q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out eventListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gascity: GET %s: decode: %w", path, err)
	}
	return out.Items, nil
}
