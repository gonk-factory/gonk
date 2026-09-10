package gcapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// These tests pin the contract that gonk-u1p.7 was lost in: an async 202 is
// an ACKNOWLEDGEMENT, not a receipt, and the only statement of what actually
// happened is a terminal event on the city log keyed by request id.
// AwaitRequestOutcome is what CreateSession's caller uses to learn the real
// outcome (cmd/gonk-gate/broker_inject.go); these tests exercise it directly
// against synthetic events rather than through any particular async call.

func eventsHandler(t *testing.T, byType map[string]string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/v0/city/gonk-city/events"; r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		body, ok := byType[r.URL.Query().Get("type")]
		if !ok {
			body = `{"items":[],"total":0}`
		}
		_, _ = fmt.Fprint(w, body)
	})
}

func TestAwaitRequestOutcomeReadsTheSuccessEvent(t *testing.T) {
	c := newTestClient(t, eventsHandler(t, map[string]string{
		EventSessionSubmitResult: `{"items":[{"seq":35,"type":"request.result.session.submit","subject":"go-d3y",
			"payload":{"request_id":"req-1","session_id":"go-d3y","queued":false,"intent":"default"}}],"total":1}`,
	}))
	out, err := c.AwaitRequestOutcome(context.Background(), "req-1", "10", EventSessionSubmitResult, time.Second)
	if err != nil {
		t.Fatalf("AwaitRequestOutcome = %v", err)
	}
	if !out.OK {
		t.Fatalf("OK = false, want true (%s)", out)
	}
	if out.SessionID != "go-d3y" {
		t.Errorf("SessionID = %q, want go-d3y", out.SessionID)
	}
	if out.Retryable() {
		t.Error("a delivered prompt must not be retryable")
	}
}

// The failure event is where the truth lives. resolve_failed is the
// async-create window (the session is not there YET), so it must read as
// retryable rather than fatal.
func TestAwaitRequestOutcomeReadsTheFailureEventAndClassifiesIt(t *testing.T) {
	c := newTestClient(t, eventsHandler(t, map[string]string{
		EventRequestFailed: `{"items":[{"seq":38,"type":"request.failed",
			"payload":{"request_id":"req-2","operation":"session.submit","error_code":"resolve_failed",
			"error_message":"session not found: \"gonk.triage.p99.i999.a1\""}}],"total":1}`,
	}))
	out, err := c.AwaitRequestOutcome(context.Background(), "req-2", "0", EventSessionSubmitResult, time.Second)
	if err != nil {
		t.Fatalf("AwaitRequestOutcome = %v", err)
	}
	if out.OK {
		t.Fatal("OK = true, want false")
	}
	if out.ErrorCode != ErrorCodeResolveFailed {
		t.Errorf("ErrorCode = %q, want %q", out.ErrorCode, ErrorCodeResolveFailed)
	}
	if !out.Retryable() {
		t.Error("resolve_failed is the async-create window and MUST be retryable")
	}
}

// Only the not-live-yet flavour of submit_failed is the pod still starting.
// Anything else is a real rejection: retrying it burns the order's timeout.
func TestSubmitFailedIsRetryableOnlyWhenTheSessionIsNotLiveYet(t *testing.T) {
	notLive := RequestOutcome{ErrorCode: ErrorCodeSubmitFailed, ErrorMessage: "session is not active: s-go-d3y"}
	if !notLive.Retryable() {
		t.Error("an inactive session is the pod still starting; want retryable")
	}
	rejected := RequestOutcome{ErrorCode: ErrorCodeSubmitFailed, ErrorMessage: "message text is required"}
	if rejected.Retryable() {
		t.Error("a real rejection must be terminal, not retried")
	}
	unknown := RequestOutcome{ErrorCode: "something_else", ErrorMessage: "session is not active"}
	if unknown.Retryable() {
		t.Error("an unrecognised code must be terminal")
	}
}

// A terminal event that never arrives means the outcome is UNKNOWN. Reporting
// that as success is the exact shape of the bug this whole path exists to fix,
// so it must be an error.
func TestAwaitRequestOutcomeTimesOutRatherThanAssumingSuccess(t *testing.T) {
	c := newTestClient(t, eventsHandler(t, nil))
	c.OutcomePollInterval = time.Millisecond
	out, err := c.AwaitRequestOutcome(context.Background(), "req-missing", "0", EventSessionSubmitResult, 20*time.Millisecond)
	if err == nil {
		t.Fatalf("AwaitRequestOutcome = %+v, want an error: an unobserved outcome is not a success", out)
	}
}

// The cursor from the 202 bounds the scan. An OLDER event carrying the same
// request id (a recycled id, a replayed log) must not be mistaken for this
// request's answer.
func TestAwaitRequestOutcomeIgnoresEventsBelowTheCursor(t *testing.T) {
	c := newTestClient(t, eventsHandler(t, map[string]string{
		EventSessionSubmitResult: `{"items":[{"seq":5,"type":"request.result.session.submit",
			"payload":{"request_id":"req-3","session_id":"stale"}}],"total":1}`,
	}))
	c.OutcomePollInterval = time.Millisecond
	if _, err := c.AwaitRequestOutcome(context.Background(), "req-3", "40", EventSessionSubmitResult, 20*time.Millisecond); err == nil {
		t.Fatal("an event below the submit cursor must not answer for this request")
	}
}
