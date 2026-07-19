package stubmodel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

// AD-2: record mode proxies ONE turn to a real upstream and commits the result.
// A committed cassette must never carry a live credential. This drives the
// recorder against a fake upstream with a request that contains a secret in both
// the Authorization header and an api_key body field, then asserts the written
// cassette has redacted it.
func TestCassetteScrubsCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-x","object":"chat.completion","created":1752400000,`+
			`"model":"stub-qwen","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	rec, err := stubmodel.NewRecorder(upstream.URL, "recorded", dir)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	proxy := httptest.NewServer(rec)
	defer proxy.Close()

	body := `{"model":"stub-qwen","api_key":"sk-live-DEADBEEF","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequestWithContext(context.Background(), "POST", proxy.URL+"/v1/chat/completions",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-live-DEADBEEF")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	_ = resp.Body.Close()

	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "recorded.json"))
	if err != nil {
		t.Fatalf("read cassette: %v", err)
	}
	if bytes.Contains(raw, []byte("sk-live-DEADBEEF")) {
		t.Fatalf("cassette leaked a live credential:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("REDACTED")) {
		t.Fatalf("cassette did not redact the api_key field:\n%s", raw)
	}
}

// A committed cassette replays into a script whose tool_calls and usage exactly
// reproduce the recorded turn -- offline, forever.
func TestReplayCommittedCassette(t *testing.T) {
	steps, err := stubmodel.LoadCassette("triage")
	if err != nil {
		t.Fatalf("LoadCassette: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("cassette produced %d steps, want 1", len(steps))
	}

	s, base := newStub(t)
	s.SetScript(steps)
	_, out := chat(t, base, "stub-qwen", nil)

	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	tc := msg["tool_calls"].([]any)
	if len(tc) != 1 {
		t.Fatalf("replayed tool_calls = %+v", tc)
	}
	fn := tc[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "gitlab_comment" {
		t.Fatalf("replayed tool name = %v, want gitlab_comment", fn["name"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
	u := out["usage"].(map[string]any)
	if u["prompt_tokens"].(float64) != 812 || u["completion_tokens"].(float64) != 41 ||
		u["total_tokens"].(float64) != 853 {
		t.Fatalf("replayed usage = %+v, want the recorded 812/41/853", u)
	}
}

// The committed fixture itself must never carry a live-looking credential.
func TestCommittedCassetteHasNoSecret(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "cassettes", "triage.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if bytes.Contains(raw, []byte("sk-live")) || bytes.Contains(raw, []byte("Bearer ")) {
		t.Fatalf("committed cassette carries a secret-looking value:\n%s", raw)
	}
}
