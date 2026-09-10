package stubmodel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

func newStub(t *testing.T) (*stubmodel.Server, string) {
	t.Helper()
	s := stubmodel.New()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func chat(t *testing.T, base, model string, extra map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	body := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "triage this issue"}},
	}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// The whole point: the harness dictates the token counts, so it dictates the cost.
func TestReportsScriptedUsageExactly(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{
		Response: stubmodel.Response{Content: "ok"},
		Usage:    stubmodel.Usage{PromptTokens: 1000, CompletionTokens: 250},
		Repeat:   stubmodel.Forever,
	}})

	for range 3 {
		resp, out := chat(t, base, "stub-qwen", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		u := out["usage"].(map[string]any)
		if u["prompt_tokens"].(float64) != 1000 || u["completion_tokens"].(float64) != 250 ||
			u["total_tokens"].(float64) != 1250 {
			t.Fatalf("usage = %+v", u)
		}
	}
	if got := s.Log().Calls(); len(got) != 3 {
		t.Fatalf("log has %d calls, want 3", len(got))
	}
	if got := s.Log().TotalTokens(); got != 3750 {
		t.Fatalf("total tokens = %d, want 3750", got)
	}
}

// Two runs must produce byte-identical responses, or "deterministic" is a lie.
func TestResponsesAreByteStable(t *testing.T) {
	script := []stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 10, CompletionTokens: 2}, Repeat: stubmodel.Forever}}

	var bodies []string
	for range 2 {
		s, base := newStub(t)
		s.SetScript(script)
		_, out := chat(t, base, "stub-qwen", nil)
		b, _ := json.Marshal(out)
		bodies = append(bodies, string(b))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("responses differ across runs:\n%s\n%s", bodies[0], bodies[1])
	}
	if strings.Contains(bodies[0], time.Now().Format("2006")) {
		t.Fatal("response embeds the current time; it cannot be byte-stable")
	}
}

// A canned tool call is how a test forces a PASSING outcome gate.
func TestCannedToolCall(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{
		Response: stubmodel.Response{ToolCalls: []stubmodel.ToolCall{{
			ID: "call_1", Name: "gitlab_comment",
			Arguments: `{"body":"triaged: bug"}`,
		}}},
		Usage:  stubmodel.Usage{PromptTokens: 100, CompletionTokens: 20},
		Repeat: stubmodel.Forever,
	}})
	_, out := chat(t, base, "stub-qwen", nil)
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	tc := msg["tool_calls"].([]any)
	if len(tc) != 1 {
		t.Fatalf("tool_calls = %+v", tc)
	}
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
}

// The infra-failure half of the ladder. A 500 must NOT become a gate failure.
func TestInducedFailures(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{
		{Status: 500, Repeat: 2},
		{Status: 429, Repeat: 1},
		{Response: stubmodel.Response{Content: "ok"}, Usage: stubmodel.Usage{PromptTokens: 1, CompletionTokens: 1}, Repeat: stubmodel.Forever},
	})
	for _, want := range []int{500, 500, 429, 200} {
		resp, _ := chat(t, base, "stub-qwen", nil)
		if resp.StatusCode != want {
			t.Fatalf("status = %d, want %d", resp.StatusCode, want)
		}
	}
	// A failed call still appears in the log: it happened, even though it cost nothing.
	if got := len(s.Log().Calls()); got != 4 {
		t.Fatalf("log has %d calls, want 4", got)
	}
	// ...and it reported NO usage, so it must contribute NO tokens.
	if got := s.Log().TotalTokens(); got != 2 {
		t.Fatalf("failed calls contributed tokens: total = %d, want 2", got)
	}
}

func TestHangProducesClientTimeout(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{Hang: true, Repeat: stubmodel.Forever}})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions",
		strings.NewReader(`{"model":"stub-qwen","messages":[]}`))
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("want a timeout, got a response")
	}
	_ = s
}

// Steps can target a specific rung's model, so one script drives a whole ladder run.
func TestMatchOnModel(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{
		{Match: stubmodel.Match{Model: "stub-qwen"}, Status: 500, Repeat: stubmodel.Forever},
		{Match: stubmodel.Match{Model: "stub-glm"},
			Response: stubmodel.Response{Content: "ok"},
			Usage:    stubmodel.Usage{PromptTokens: 5, CompletionTokens: 5}, Repeat: stubmodel.Forever},
	})
	if r, _ := chat(t, base, "stub-qwen", nil); r.StatusCode != 500 {
		t.Fatalf("qwen status = %d", r.StatusCode)
	}
	if r, _ := chat(t, base, "stub-glm", nil); r.StatusCode != 200 {
		t.Fatalf("glm status = %d", r.StatusCode)
	}
}

// Running off the end of the script is a LOUD failure, not a silent default: a test
// that makes more calls than it scripted has stopped being deterministic.
func TestExhaustedScriptIs500AndRecorded(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 1, CompletionTokens: 1}, Repeat: 1}})
	_, _ = chat(t, base, "stub-qwen", nil)
	resp, out := chat(t, base, "stub-qwen", nil)
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(out["error"].(map[string]any)["message"].(string), "script exhausted") {
		t.Fatalf("error = %+v", out["error"])
	}
	if !s.Log().ScriptExhausted() {
		t.Fatal("Log().ScriptExhausted() must flag this — a suite must be able to fail on it")
	}
}

// LiteLLM may or may not forward `metadata` to its upstream (version-dependent).
// The stub records the WHOLE body so Task 5 can find out empirically rather than
// by reading release notes.
func TestLogCapturesFullRequestBody(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 1, CompletionTokens: 1}, Repeat: stubmodel.Forever}})
	chat(t, base, "stub-qwen", map[string]any{"metadata": map[string]string{"gonk_rung": "qwen-local"}})

	c := s.Log().Calls()[0]
	if !strings.Contains(string(c.RawBody), "gonk_rung") {
		t.Fatalf("raw body did not capture metadata: %s", c.RawBody)
	}
	if c.Metadata["gonk_rung"] != "qwen-local" {
		t.Fatalf("metadata = %+v (if this is empty against a REAL LiteLLM, it strips metadata — record that in Task 5)", c.Metadata)
	}
}

func TestModelsEndpoint(t *testing.T) {
	_, base := newStub(t)
	resp, err := http.Get(base + "/v1/models")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /v1/models = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestControlPlaneResetsScriptAndLog(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 1, CompletionTokens: 1}, Repeat: stubmodel.Forever}})
	chat(t, base, "stub-qwen", nil)

	req, _ := http.NewRequest("POST", base+"/_control/reset", nil)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 204 {
		t.Fatalf("reset = %v, %v", r, err)
	}
	if len(s.Log().Calls()) != 0 {
		t.Fatal("reset must clear the log")
	}
}

// AD-4: no test asserts on model prose. The load-bearing fields -- tool_calls and
// usage -- must be identical no matter what prose the assistant emits. Here two
// scripts differ ONLY in Content; their responses must carry byte-identical
// tool_calls and usage. If prose ever leaked into those fields, this fails.
func TestToolCallsAndUsageAreProseIndependent(t *testing.T) {
	mk := func(prose string) []stubmodel.Step {
		return []stubmodel.Step{{
			Response: stubmodel.Response{
				Content: prose,
				ToolCalls: []stubmodel.ToolCall{{
					ID: "call_1", Name: "gitlab_comment", Arguments: `{"body":"triaged: bug"}`,
				}},
			},
			Usage:  stubmodel.Usage{PromptTokens: 100, CompletionTokens: 20},
			Repeat: stubmodel.Forever,
		}}
	}
	loadBearing := func(out map[string]any) string {
		msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		b, _ := json.Marshal(map[string]any{"tool_calls": msg["tool_calls"], "usage": out["usage"]})
		return string(b)
	}

	proses := []string{
		"Let me look into this issue for you.",
		"Sure, I'll triage it right away with a completely different explanation.",
	}
	var got []string
	for _, p := range proses {
		s, base := newStub(t)
		s.SetScript(mk(p))
		_, out := chat(t, base, "stub-qwen", nil)
		got = append(got, loadBearing(out))
	}
	if got[0] != got[1] {
		t.Fatalf("tool_calls/usage depend on prose:\n%s\n%s", got[0], got[1])
	}
}

// A COMPLETION ID IS AN IDENTITY, NOT A COUNTER. The stub shares one Server
// across every test in a package (test/component's shStub), and each test opens
// with Reset + SetScript. If either rewound the id counter, the stub would hand
// out chatcmpl-stub-0001 again -- and LiteLLM, whose LiteLLM_SpendLogs table has
// request_id as its PRIMARY KEY and whose batch writer inserts with
// create_many(..., skip_duplicates=True), would DISCARD the second call's spend
// row in silence: no error, no retry, no row, ever.
//
// That is gonk-ij2e. TestMeasureLiteLLMSpendLogLag looked like "LiteLLM's
// background spend-log writer is unreliable under load" (1-2s alone, timing out
// at 100s / 3m / a measured 20m after the other tests had run) when the truth
// was that its row was never written, because the id had already been claimed.
// See docs/spikes/2026-09-10-spend-log-lag.md.
//
// This test fails against the pre-fix stub: every id there was chatcmpl-stub-0001.
func TestCompletionIDsAreNeverReusedAcrossResetOrSetScript(t *testing.T) {
	s, base := newStub(t)
	script := []stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 10, CompletionTokens: 2}, Repeat: stubmodel.Forever}}

	seen := map[string]int{}
	// Three "tests", each opening the way newWorld does: Reset, then SetScript.
	for round := range 3 {
		s.Reset()
		s.SetScript(script)
		for call := range 2 {
			_, out := chat(t, base, "stub-qwen", nil)
			id, _ := out["id"].(string)
			if id == "" {
				t.Fatalf("round %d call %d: response carried no id: %+v", round, call, out)
			}
			if prev, dup := seen[id]; dup {
				t.Fatalf("completion id %q reused: first issued in round %d, again in round %d. "+
					"LiteLLM's spend-log writer inserts ON CONFLICT DO NOTHING against a request_id "+
					"PRIMARY KEY, so the second call's spend row is silently discarded and the meter "+
					"under-counts forever. Reset/SetScript must not rewind the id counter.",
					id, prev, round)
			}
			seen[id] = round
		}
	}
	if len(seen) != 6 {
		t.Fatalf("saw %d distinct ids across 6 calls, want 6: %v", len(seen), seen)
	}
}
