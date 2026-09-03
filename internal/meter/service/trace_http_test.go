package service

import (
	"encoding/json"
	"net/http"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

func decodeTrace(t *testing.T, body []byte) meterapi.TraceResponse {
	t.Helper()
	var got meterapi.TraceResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode trace response: %v (%s)", err, body)
	}
	return got
}

// Reports are incremental: a long session produces several, and each must ADD
// to what was seen rather than replace it.
func TestTraceReportsAccumulate(t *testing.T) {
	h := newHTTPFixture(t)

	resp, body := h.do("POST", meterapi.TracePath, h.token, meterapi.TraceRequest{
		SessionKey: "s1", Attempt: 1, BeadID: "gonk:1:issue:2", Project: "g/p",
		Completeness: "complete", Turns: 1,
		Calls: []meterapi.TraceCall{{Tool: "read", Target: "README.md"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first report = %d, want 200 (%s)", resp.StatusCode, body)
	}
	resp, body = h.do("POST", meterapi.TracePath, h.token, meterapi.TraceRequest{
		SessionKey: "s1", Attempt: 1, Completeness: "complete", Turns: 2,
		Calls: []meterapi.TraceCall{{Tool: "grep"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second report = %d, want 200 (%s)", resp.StatusCode, body)
	}
	got := decodeTrace(t, body)
	if got.Calls != 2 || got.Turns != 3 {
		t.Fatalf("got calls=%d turns=%d, want 2 and 3 accumulated", got.Calls, got.Turns)
	}
	if got.Completeness != "complete" {
		t.Fatalf("completeness = %q, want complete", got.Completeness)
	}
}

// THE RESPONSE REPORTS WHAT IS STORED, NOT WHAT WAS SENT. A collector that
// reports "complete" against a trace already known to have a gap must be told
// "partial" -- echoing its own input back would let it believe the evidence is
// cleaner than it is.
func TestTraceResponseReportsStoredCompletenessNotSubmitted(t *testing.T) {
	h := newHTTPFixture(t)

	_, _ = h.do("POST", meterapi.TracePath, h.token, meterapi.TraceRequest{
		SessionKey: "s-degrade", Attempt: 1, Completeness: "partial",
	})
	_, body := h.do("POST", meterapi.TracePath, h.token, meterapi.TraceRequest{
		SessionKey: "s-degrade", Attempt: 1, Completeness: "complete",
	})
	if got := decodeTrace(t, body).Completeness; got != "partial" {
		t.Fatalf("completeness = %q, want partial -- a gap already observed cannot be undone", got)
	}
}

// A report with no completeness must be REJECTED, not defaulted. Defaulting
// would make an unobserved session indistinguishable from an idle one, which is
// the confusion the field exists to prevent.
func TestTraceRequiresAValidCompleteness(t *testing.T) {
	h := newHTTPFixture(t)
	for _, c := range []string{"", "unknown", "COMPLETE", "done"} {
		resp, _ := h.do("POST", meterapi.TracePath, h.token, meterapi.TraceRequest{
			SessionKey: "s2", Attempt: 1, Completeness: c,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("completeness %q = %d, want 400", c, resp.StatusCode)
		}
	}
}

// Attempt is part of the identity. A trace filed against attempt 0 would merge
// evidence from runs that must stay separate.
func TestTraceRequiresSessionKeyAndPositiveAttempt(t *testing.T) {
	h := newHTTPFixture(t)
	for _, req := range []meterapi.TraceRequest{
		{SessionKey: "", Attempt: 1, Completeness: "complete"},
		{SessionKey: "s3", Attempt: 0, Completeness: "complete"},
		{SessionKey: "s3", Attempt: -1, Completeness: "complete"},
	} {
		resp, _ := h.do("POST", meterapi.TracePath, h.token, req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%+v = %d, want 400", req, resp.StatusCode)
		}
	}
}

// A re-slung attempt is a different run and must not inherit the first
// attempt's reads, or a predicate would credit it with work it never did.
func TestTraceIsSeparatedByAttempt(t *testing.T) {
	h := newHTTPFixture(t)

	_, _ = h.do("POST", meterapi.TracePath, h.token, meterapi.TraceRequest{
		SessionKey: "s4", Attempt: 1, Completeness: "complete", Turns: 1,
		Calls: []meterapi.TraceCall{{Tool: "read", Target: "a.go"}},
	})
	_, body := h.do("POST", meterapi.TracePath, h.token, meterapi.TraceRequest{
		SessionKey: "s4", Attempt: 2, Completeness: "complete", Turns: 1,
	})
	if got := decodeTrace(t, body); got.Calls != 0 {
		t.Fatalf("attempt 2 reports %d calls; it must not inherit attempt 1's", got.Calls)
	}
}

// A call with no tool name is not evidence of anything, and counting it would
// inflate the counts a predicate later reasons over.
func TestNamelessCallsAreDropped(t *testing.T) {
	h := newHTTPFixture(t)
	_, body := h.do("POST", meterapi.TracePath, h.token, meterapi.TraceRequest{
		SessionKey: "s5", Attempt: 1, Completeness: "complete",
		Calls: []meterapi.TraceCall{{Tool: ""}, {Tool: "read"}, {Tool: "", Target: "x"}},
	})
	if got := decodeTrace(t, body).Calls; got != 1 {
		t.Fatalf("calls = %d, want 1 -- nameless calls must not be counted", got)
	}
}
