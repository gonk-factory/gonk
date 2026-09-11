package stubmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// AD-2: nobody can author opencode's real tool-call schema from imagination. So
// the stub has a record mode: it proxies ONE real turn to a live upstream (e.g.
// bailey), captures the exact {request, response} pair, scrubs every credential,
// and writes a committed cassette. Replay serves that cassette forever, offline.
//
// Record mode is run by a human, out of band, and NEVER inside a test. Tests
// only ever LOAD a committed, already-scrubbed cassette (LoadCassette). See
// `make refresh-cassette` (Makefile) for the documented, credentialed way to
// run it.
//
// cassettes/triage.json (T-19, 2026-09-10): the committed fixture is HAND
// AUTHORED, not recorded -- it was written by hand in the same commit that
// introduced this package (8a72f5b), and its response is an OpenAI tool_call
// naming a "gitlab_comment" function. That shape was never wired to anything:
// no code in this repo converts a tool_call into a pkg/effects.Batch, and the
// REAL triage prompt (cmd/gonk-gate/broker_inject.go's renderTriagePrompt)
// explicitly forbids tool use ("Do NOT post anything yourself... you hold no
// credentials") and instead has the agent emit its batch as PLAIN TEXT, fenced
// GONK_BATCH_START / {json} / GONK_BATCH_END, inside its own message content.
// A fresh recording aimed at replacing cassettes/triage.json must send THAT
// prompt (or a faithful shape of it) and capture a `content` field containing
// the fence -- not a tool_calls array -- or it will be exactly as unusable for
// pkg/effects.ParseBatch as the current fixture is.

// redacted is the placeholder written in place of any secret value. It must not
// look like a real key, so a committed cassette can never carry live material.
const redacted = "REDACTED"

// secretKeys are JSON object keys whose values are credentials. Matching is
// case-insensitive. This is deliberately broad: a false redaction is harmless,
// a leaked key is not.
var secretKeys = map[string]bool{
	"authorization": true,
	"api_key":       true,
	"apikey":        true,
	"api-key":       true,
	"x-api-key":     true,
	"access_token":  true,
	"token":         true,
	"bearer":        true,
	"password":      true,
	"secret":        true,
}

// scrubJSON walks a JSON document and replaces every secret-keyed value with
// redacted, recursively. A body that is not JSON is returned unchanged (record
// mode only ever proxies JSON chat-completions traffic).
func scrubJSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	scrubbed := scrubValue(v)
	out, err := json.Marshal(scrubbed)
	if err != nil {
		return raw
	}
	return out
}

func scrubValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if secretKeys[strings.ToLower(k)] {
				t[k] = redacted
			} else {
				t[k] = scrubValue(val)
			}
		}
		return t
	case []any:
		for i := range t {
			t[i] = scrubValue(t[i])
		}
		return t
	default:
		return v
	}
}

// cassetteEntry is one recorded turn: the request the stub received and the
// response the real upstream returned, both scrubbed of credentials.
type cassetteEntry struct {
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
}

type cassetteFile struct {
	Name    string          `json:"name"`
	Entries []cassetteEntry `json:"entries"`
}

// Recorder is an http.Handler that proxies chat-completions traffic to a real
// upstream, capturing a scrubbed cassette. It is used ONLY by the -record flag
// on cmd/gonk-stubmodel, never by a test.
type Recorder struct {
	upstream string
	name     string
	dir      string
	client   *http.Client

	mu      sync.Mutex
	entries []cassetteEntry
}

// NewRecorder returns a Recorder that proxies to upstream (a base URL like
// https://bailey.example/v1) and, on Save, writes dir/name.json.
func NewRecorder(upstream, name, dir string) (*Recorder, error) {
	if upstream == "" {
		return nil, fmt.Errorf("stubmodel: record mode needs an upstream base URL")
	}
	return &Recorder{
		upstream: strings.TrimRight(upstream, "/"),
		name:     name,
		dir:      dir,
		client:   &http.Client{},
	}, nil
}

func (rec *Recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqBody, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", 400)
		return
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, rec.upstream+r.URL.Path, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	upReq.Header = r.Header.Clone()
	resp, err := rec.client.Do(upReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	// Capture the turn, scrubbed. Credentials live in the request (auth header,
	// api_key field) and MUST NOT reach a committed file.
	rec.mu.Lock()
	rec.entries = append(rec.entries, cassetteEntry{
		Request:  scrubJSON(reqBody),
		Response: scrubJSON(respBody),
	})
	rec.mu.Unlock()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// Save writes the captured cassette to dir/name.json.
func (rec *Recorder) Save() error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if err := os.MkdirAll(rec.dir, 0o755); err != nil {
		return err
	}
	cf := cassetteFile{Name: rec.name, Entries: rec.entries}
	out, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rec.dir, rec.name+".json"), append(out, '\n'), 0o644)
}

// LoadCassette reads a committed cassette by name and turns each recorded turn
// into a scripted Step, so replay serves exactly what the real upstream said.
// The name resolves against test/stubmodel/cassettes/ regardless of the caller's
// working directory.
func LoadCassette(name string) ([]Step, error) {
	return LoadCassetteFile(filepath.Join(cassetteDir(), name+".json"))
}

// LoadCassetteFile reads a cassette from an explicit path.
func LoadCassetteFile(path string) ([]Step, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf cassetteFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, fmt.Errorf("stubmodel: cassette %s: %w", path, err)
	}
	steps := make([]Step, 0, len(cf.Entries))
	for i, e := range cf.Entries {
		step, err := entryToStep(e)
		if err != nil {
			return nil, fmt.Errorf("stubmodel: cassette %s entry %d: %w", path, i, err)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// entryToStep converts one recorded OpenAI chat-completion turn into a Step that
// reproduces its tool_calls, content, and usage exactly. Prose (content) is
// preserved for fidelity but is NEVER what a test asserts on (AD-4): only the
// tool_calls and usage are load-bearing.
func entryToStep(e cassetteEntry) (Step, error) {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(e.Request, &req)

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(e.Response, &resp); err != nil {
		return Step{}, fmt.Errorf("decode response: %w", err)
	}

	step := Step{
		Match:  Match{Model: req.Model},
		Usage:  Usage{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens},
		Repeat: 1,
	}
	if len(resp.Choices) > 0 {
		m := resp.Choices[0].Message
		step.Response.Content = m.Content
		for _, tc := range m.ToolCalls {
			step.Response.ToolCalls = append(step.Response.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	return step, nil
}

// cassetteDir resolves test/stubmodel/cassettes relative to this source file, so
// LoadCassette works from any working directory (go test runs in the package
// dir, but the compiled binary may not).
func cassetteDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "cassettes"
	}
	return filepath.Join(filepath.Dir(thisFile), "cassettes")
}
