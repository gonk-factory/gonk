package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/effects"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// traceServer serves GET /v1/trace/{key} with a fixed view, or fails.
func traceServer(t *testing.T, view *meterapi.TraceView) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if view == nil {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// packWithTriagePolicy writes a baked-pack layout carrying the shipped rule.
func packWithTriagePolicy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	agent := filepath.Join(dir, "agents", "triage")
	if err := os.MkdirAll(agent, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[comment]\nmin = 1\nmax = 1\n\n[trajectory]\nread_tools = [\"read\"]\nrequire_any_read_for = [\"reply-only\", \"close\"]\n"
	if err := os.WriteFile(filepath.Join(agent, "effect-shape.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func trajDeps(t *testing.T, view *meterapi.TraceView, enforce bool) sweepDeps {
	t.Helper()
	return sweepDeps{
		Meter:             meterClient(traceServer(t, view).URL),
		PackDir:           packWithTriagePolicy(t),
		EnforceTrajectory: enforce,
		Log:               slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	}
}

func trajRec() beadstore.Record {
	return beadstore.Record{BeadAnchor: "gonk:7:issue:3", SessionKey: "gonk-7-issue-3", Attempt: 1}
}

var replyOnly = effects.Batch{Verdict: effects.VerdictReplyOnly}

// THE PROPERTY EVERYTHING ELSE DEPENDS ON RIGHT NOW: while enforcement is off,
// the gate cannot reject anything, however damning the evidence. A predicate
// enabled on unmeasured evidence rejects honest batches, and a rejected batch
// on the triage path re-slings the bead onto a pricier rung.
func TestObservingOnlyNeverRejects(t *testing.T) {
	readNothing := &meterapi.TraceView{
		SessionKey: "gonk-7-issue-3", Attempt: 1, Completeness: "complete",
		Calls: []meterapi.TraceCall{{Tool: "glob"}}, Turns: 1,
	}
	d := trajDeps(t, readNothing, false)
	if v := checkTrajectory(context.Background(), d, trajRec(), "triage", replyOnly); v != "" {
		t.Fatalf("observing-only gate rejected: %q", v)
	}
}

// And with enforcement on, the same evidence DOES reject -- otherwise the
// switch would be decorative.
func TestEnforcingRejectsAVerdictReachedWithoutReading(t *testing.T) {
	readNothing := &meterapi.TraceView{
		SessionKey: "gonk-7-issue-3", Attempt: 1, Completeness: "complete",
		Calls: []meterapi.TraceCall{{Tool: "glob"}}, Turns: 1,
	}
	d := trajDeps(t, readNothing, true)
	v := checkTrajectory(context.Background(), d, trajRec(), "triage", replyOnly)
	if v == "" {
		t.Fatal("enforcing gate accepted a no-artifact verdict reached without reading anything")
	}
}

// A session that read something passes even under enforcement.
func TestEnforcingAcceptsASessionThatRead(t *testing.T) {
	didRead := &meterapi.TraceView{
		SessionKey: "gonk-7-issue-3", Attempt: 1, Completeness: "complete",
		Calls: []meterapi.TraceCall{{Tool: "read", Target: "/workspace/a.go"}}, Turns: 1,
	}
	d := trajDeps(t, didRead, true)
	if v := checkTrajectory(context.Background(), d, trajRec(), "triage", replyOnly); v != "" {
		t.Fatalf("a session that read was rejected: %q", v)
	}
}

// NOT BEING ABLE TO ASK IS NOT A VERDICT. A meter we cannot reach must never
// look like an agent that misbehaved, even with enforcement on.
func TestAnUnreachableMeterNeverRejects(t *testing.T) {
	d := trajDeps(t, nil, true)
	if v := checkTrajectory(context.Background(), d, trajRec(), "triage", replyOnly); v != "" {
		t.Fatalf("an unreachable meter produced a rejection: %q", v)
	}
}

// An absent trace is evidence we did not collect, not evidence of idleness.
func TestAnAbsentTraceNeverRejects(t *testing.T) {
	absent := &meterapi.TraceView{SessionKey: "gonk-7-issue-3", Attempt: 1, Completeness: "absent"}
	d := trajDeps(t, absent, true)
	if v := checkTrajectory(context.Background(), d, trajRec(), "triage", replyOnly); v != "" {
		t.Fatalf("an absent trace produced a rejection: %q", v)
	}
}

// An agent with no declared predicates is not judged at all, so adding
// trajectory to triage cannot start rejecting scaffold batches.
func TestAnAgentWithNoPolicyIsNotJudged(t *testing.T) {
	readNothing := &meterapi.TraceView{
		SessionKey: "gonk-7-issue-3", Attempt: 1, Completeness: "complete", Turns: 1,
	}
	d := trajDeps(t, readNothing, true)
	if v := checkTrajectory(context.Background(), d, trajRec(), "scaffold", replyOnly); v != "" {
		t.Fatalf("an agent with no trajectory policy was judged: %q", v)
	}
}

// code-change produces a diff for the verify pipeline to judge on outcome
// evidence; it must not be gated on process evidence here.
func TestCodeChangeIsNotGatedByTheFifthGate(t *testing.T) {
	readNothing := &meterapi.TraceView{
		SessionKey: "gonk-7-issue-3", Attempt: 1, Completeness: "complete",
		Calls: []meterapi.TraceCall{{Tool: "glob"}}, Turns: 1,
	}
	d := trajDeps(t, readNothing, true)
	batch := effects.Batch{Verdict: effects.VerdictCodeChange}
	if v := checkTrajectory(context.Background(), d, trajRec(), "triage", batch); v != "" {
		t.Fatalf("code-change was rejected on process evidence: %q", v)
	}
}
