package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/trace"
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
	return trajDepsLogging(t, view, enforce, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
}

// trajDepsLogging is trajDeps with the verdict log routed somewhere readable,
// for the tests that assert on the record itself rather than on the return.
func trajDepsLogging(t *testing.T, view *meterapi.TraceView, enforce bool, log *slog.Logger) sweepDeps {
	t.Helper()
	return sweepDeps{
		Meter:             meterClient(traceServer(t, view).URL),
		PackDir:           packWithTriagePolicy(t),
		EnforceTrajectory: enforce,
		Log:               log,
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

// gonk-u0z0. THE VERDICT LINE MUST NAME THE TOOLS, NOT JUST COUNT THEM.
//
// A 14-day Loki sweep for which tools agents actually call came back with zero
// named invocations, because "calls=3" is all this record ever said. That is
// why gonk-7b79's permission narrowing rested on design evidence rather than
// measurement. This pins the attribute that fixes it AND its exact shape --
// distinct, sorted, comma-joined -- because the shape is what a Loki query
// binds to, and a shape that drifts silently is the same outage again.
func TestJudgedVerdictLogsTheDistinctSortedToolNames(t *testing.T) {
	view := &meterapi.TraceView{
		SessionKey: "gonk-7-issue-3", Attempt: 1, Completeness: "complete",
		Calls: []meterapi.TraceCall{
			{Tool: "read", Target: "/workspace/a.go"},
			{Tool: "bash", Target: "/workspace/b.go"},
			{Tool: "read", Target: "/workspace/a.go"},
		},
		Turns: 2,
	}
	var buf bytes.Buffer
	d := trajDepsLogging(t, view, false, slog.New(slog.NewJSONHandler(&buf, nil)))
	if v := checkTrajectory(context.Background(), d, trajRec(), "triage", replyOnly); v != "" {
		t.Fatalf("observing-only gate rejected: %q", v)
	}

	rec := findJudgedRecord(t, buf.Bytes())
	if got, want := rec["tools"], "bash,read"; got != want {
		t.Fatalf("judged record tools = %v, want %q -- distinct, sorted, comma-joined "+
			"(three calls of two tools, in observation order read,bash,read)\nrecord: %v", got, want, rec)
	}
	// The count stays: "how many calls" and "which tools" are different
	// questions and the sweep that failed needed both.
	if got, want := rec["calls"], float64(3); got != want {
		t.Fatalf("judged record calls = %v, want %v", got, want)
	}
}

// AND IT MUST NOT NAME ANYTHING ELSE. Call.Target is a normalised path or issue
// reference derived from model output over untrusted issue text; this record is
// the one place it could leak into a log field, and sessionViewAttrs'
// rule (broker_apply.go) is that a log attribute carries only values whose
// domain we control.
func TestJudgedVerdictNeverLogsCallTargets(t *testing.T) {
	const secret = "/workspace/leaked-target-path.go"
	view := &meterapi.TraceView{
		SessionKey: "gonk-7-issue-3", Attempt: 1, Completeness: "complete",
		Calls: []meterapi.TraceCall{{Tool: "read", Target: secret}}, Turns: 1,
	}
	var buf bytes.Buffer
	d := trajDepsLogging(t, view, false, slog.New(slog.NewJSONHandler(&buf, nil)))
	if v := checkTrajectory(context.Background(), d, trajRec(), "triage", replyOnly); v != "" {
		t.Fatalf("observing-only gate rejected: %q", v)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("the trajectory log carried a call TARGET; only tool NAMES may appear here:\n%s", buf.String())
	}
	// Proves the fixture reached the log at all, so the assertion above cannot
	// pass by the record never having been written.
	if rec := findJudgedRecord(t, buf.Bytes()); rec["tools"] != "read" {
		t.Fatalf("judged record tools = %v, want %q", rec["tools"], "read")
	}
}

// A model names its own tools, so the distinct set is not closed by anything
// gonk owns: the attribute has to stay bounded and free of separators however
// the session behaves. This record goes out through defaultLogSink, where a
// write over PIPE_BUF can tear (logsink.go), so an unbounded line is not a
// cosmetic problem.
func TestToolNamesAttrIsBoundedAndSanitized(t *testing.T) {
	t.Run("no calls yields an empty attribute", func(t *testing.T) {
		if got := toolNamesAttr(trace.Trace{}); got != "" {
			t.Fatalf("toolNamesAttr(empty) = %q, want \"\"", got)
		}
	})

	t.Run("separators and control bytes cannot escape the field", func(t *testing.T) {
		tr := trace.Trace{Calls: []trace.Call{{Tool: "ba,d\ntool=\"x\""}}}
		got := toolNamesAttr(tr)
		for _, bad := range []string{",", "\n", "=", "\""} {
			if strings.Contains(got, bad) {
				t.Fatalf("toolNamesAttr kept %q in %q -- a model-named tool must not be able to forge a log field", bad, got)
			}
		}
	})

	t.Run("a runaway set of names is capped", func(t *testing.T) {
		var calls []trace.Call
		for i := 0; i < maxLoggedToolNames+7; i++ {
			calls = append(calls, trace.Call{Tool: fmt.Sprintf("tool%03d", i)})
		}
		got := toolNamesAttr(trace.Trace{Calls: calls})
		parts := strings.Split(got, ",")
		if len(parts) != maxLoggedToolNames+1 {
			t.Fatalf("toolNamesAttr returned %d parts, want %d names + 1 overflow marker:\n%s",
				len(parts), maxLoggedToolNames+1, got)
		}
		if want := "+7-more"; parts[len(parts)-1] != want {
			t.Fatalf("overflow marker = %q, want %q", parts[len(parts)-1], want)
		}
	})
}

// findJudgedRecord returns the one "trajectory: judged" JSON record in the
// captured log, failing if there is not exactly one -- an assertion made
// against a record that was never written proves nothing.
func findJudgedRecord(t *testing.T, logged []byte) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", ln, err)
		}
		if rec["msg"] == "trajectory: judged" {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one \"trajectory: judged\" record, got %d:\n%s", len(found), logged)
	}
	return found[0]
}
