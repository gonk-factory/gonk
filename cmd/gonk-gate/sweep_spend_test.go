package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// costServer answers GET /v1/cost/session/{key} with a canned view.
func costServer(t *testing.T, resp meterapi.SessionCostResponse, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/cost/session/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// THE gonk-pop3 ITEM 3 CASE. Learning that one run made 6 model calls and
// another 9 previously required port-forwarding gonk-meter and authenticating
// to GET /v1/cost/bead/<anchor> with a bearer token. It must now be in the
// controller's log, which `kubectl logs` reaches.
//
// It PRINTS the record, because the deliverable is the observed output.
func TestSessionSpendIsLoggedWithoutAPortForward(t *testing.T) {
	url := costServer(t, meterapi.SessionCostResponse{
		SessionKey: "gonk-75-issue-71", Project: "group/sortlib", BeadID: "gonk:75:issue:71",
		CostUSD: 0.0412, SyntheticCostUSD: 0.0,
		PromptTokens: 18320, CompletionTokens: 2211, TotalTokens: 20531,
		ByRung: []meterapi.RungCost{
			{Rung: "qwen-local", Kind: "local", Calls: 6, TotalTokens: 12000, CostSynthetic: true},
			{Rung: "sonnet", Kind: "cloud", Calls: 3, TotalTokens: 8531},
		},
		AsOf: time.Date(2026, 9, 12, 6, 15, 0, 0, time.UTC), Complete: true,
	}, http.StatusOK)

	var logged strings.Builder
	dep := sweepDeps{Meter: meterClient(url), Log: testLogger(&logged)}
	d := dep.withDefaults()
	rec := beadstore.Record{
		BeadAnchor: "gonk:75:issue:71", SessionKey: "gonk-75-issue-71",
		Attempt: 2, Rung: "sonnet", Trigger: "issue-triage",
	}

	logSessionSpend(context.Background(), d, rec, "gate-failed")

	t.Logf("\n--- what `kubectl logs deploy/gonk-controller` now shows for this session ---\n%s", logged.String())

	for _, want := range []string{
		`model_calls=9`, // 6 local + 3 cloud, summed across rungs
		`prompt_tokens=18320`,
		`completion_tokens=2211`,
		`total_tokens=20531`,
		`bead=gonk:75:issue:71`,
		`attempt=2`,
		`complete=true`,
		`qwen-local:calls=6`,
		`sonnet:calls=3`,
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("spend record is missing %s:\n%s", want, logged.String())
		}
	}
}

// `complete=false` means an open reservation still exists, so the numbers may
// not be final. A cost record that hid that would be a misleading one.
func TestSpendRecordSaysWhenTheNumbersAreNotFinal(t *testing.T) {
	url := costServer(t, meterapi.SessionCostResponse{
		SessionKey: "s-1", TotalTokens: 10, Complete: false,
		ByRung: []meterapi.RungCost{{Rung: "cheap", Calls: 1}},
	}, http.StatusOK)

	var logged strings.Builder
	dep := sweepDeps{Meter: meterClient(url), Log: testLogger(&logged)}
	d := dep.withDefaults()
	logSessionSpend(context.Background(), d, beadstore.Record{BeadAnchor: "b", SessionKey: "s-1"}, "done")

	if !strings.Contains(logged.String(), "complete=false") {
		t.Errorf("the record does not admit the spend view is incomplete:\n%s", logged.String())
	}
}

// The spend record is best effort. An unreachable meter must say so and change
// nothing -- classification already happened, and this must never be able to
// break a sweep tick.
func TestSpendRecordFailureIsReportedAndHarmless(t *testing.T) {
	url := costServer(t, meterapi.SessionCostResponse{}, http.StatusInternalServerError)

	var logged strings.Builder
	dep := sweepDeps{Meter: meterClient(url), Log: testLogger(&logged)}
	d := dep.withDefaults()
	logSessionSpend(context.Background(), d, beadstore.Record{BeadAnchor: "b", SessionKey: "s-1"}, "done")

	if !strings.Contains(logged.String(), "could not read the meter's cost view") {
		t.Errorf("a failed cost read was silent:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "level=WARN") {
		t.Errorf("a failed cost read must be a WARN, not an ERROR -- it changes no verdict:\n%s", logged.String())
	}
}

// A record with no session key (a pre-broker bead) has nothing to ask about.
// It must not produce a spurious warning every tick.
func TestSpendRecordIsSkippedWhenThereIsNoSession(t *testing.T) {
	var logged strings.Builder
	dep := sweepDeps{Meter: meterClient("http://127.0.0.1:1"), Log: testLogger(&logged)}
	d := dep.withDefaults()
	logSessionSpend(context.Background(), d, beadstore.Record{BeadAnchor: "b"}, "done")
	if logged.String() != "" {
		t.Errorf("a bead with no session key logged something:\n%s", logged.String())
	}
}
