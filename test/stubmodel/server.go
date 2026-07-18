package stubmodel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fixedCreated keeps every response byte-identical across runs. A wall-clock
// timestamp in the body would make "deterministic" a lie (see
// TestResponsesAreByteStable).
const fixedCreated int64 = 1752400000

// maxBody caps a request. The stub is only ever reachable from the test network,
// but a cap here is free and an unbounded read is never right.
const maxBody = 4 << 20

type Server struct {
	mu     sync.Mutex
	script []Step
	seq    int
	log    Log
}

func New() *Server { return &Server{} }

func (s *Server) SetScript(steps []Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = make([]Step, len(steps))
	copy(s.script, steps)
	s.seq = 0
}

func (s *Server) Log() *Log { return &s.log }

func (s *Server) Reset() {
	s.mu.Lock()
	s.script, s.seq = nil, 0
	s.mu.Unlock()
	s.log.reset()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/chat/completions":
		s.chat(w, r)
	case r.Method == "GET" && r.URL.Path == "/v1/models":
		writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "stub-qwen", "object": "model", "owned_by": "gonk-stub"},
			{"id": "stub-glm", "object": "model", "owned_by": "gonk-stub"},
			{"id": "stub-sonnet", "object": "model", "owned_by": "gonk-stub"},
		}})
	case r.Method == "POST" && r.URL.Path == "/_control/script":
		var steps []Step
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&steps); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.SetScript(steps)
		w.WriteHeader(204)
	case r.Method == "POST" && r.URL.Path == "/_control/reset":
		s.Reset()
		w.WriteHeader(204)
	case r.Method == "GET" && r.URL.Path == "/_control/requests":
		writeJSON(w, 200, map[string]any{
			"calls":            s.log.Calls(),
			"script_exhausted": s.log.ScriptExhausted(),
			"total_tokens":     s.log.TotalTokens(),
		})
	case r.URL.Path == "/_control/health" || r.URL.Path == "/health":
		w.WriteHeader(200)
	default:
		http.NotFound(w, r)
	}
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	// LiteLLM MAY forward this. Whether it does is version-dependent and is one of
	// the things Task 5 discovers empirically (P3-1). We record it either way.
	Metadata map[string]string `json:"metadata"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", 400)
		return
	}
	var req chatRequest
	_ = json.Unmarshal(raw, &req) // a malformed request is still a call; log it

	step := s.pick(req)
	if step == nil {
		s.log.markExhausted()
		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata, RawBody: raw, Status: 500})
		writeJSON(w, 500, map[string]any{"error": map[string]any{
			"message": "stubmodel: script exhausted — the test made a model call it did not script",
			"type":    "stub_error",
		}})
		return
	}

	if step.Delay > 0 {
		time.Sleep(step.Delay)
	}
	if step.Hang {
		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata, RawBody: raw, Status: 0})
		<-r.Context().Done() // the client's deadline is the control
		return
	}

	switch {
	case step.Status != 0 && (step.Status < 200 || step.Status >= 300):
		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata, RawBody: raw, Status: step.Status})
		writeJSON(w, step.Status, map[string]any{"error": map[string]any{
			"message": "stubmodel: induced failure", "type": "stub_induced", "code": strconv.Itoa(step.Status),
		}})
	case step.RawBody != "":
		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata, RawBody: raw, Status: 200})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, step.RawBody)
	default:
		s.mu.Lock()
		s.seq++
		id := fmt.Sprintf("chatcmpl-stub-%04d", s.seq)
		s.mu.Unlock()

		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata,
			RawBody: raw, Status: 200, Usage: step.Usage})
		writeJSON(w, 200, completion(id, req.Model, step.Response, step.Usage))
	}
}

// pick returns the first non-exhausted step that matches, and charges it a use.
func (s *Server) pick(req chatRequest) *Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.script {
		st := &s.script[i]
		if st.exhausted() || !matches(st.Match, req) {
			continue
		}
		st.served++
		cp := *st
		return &cp
	}
	return nil
}

func matches(m Match, req chatRequest) bool {
	if m.Model != "" && m.Model != req.Model {
		return false
	}
	if m.Contains != "" {
		var last string
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				last = msg.Content
			}
		}
		if !strings.Contains(last, m.Contains) {
			return false
		}
	}
	if m.Tag != "" {
		k, v, ok := strings.Cut(m.Tag, "=")
		if !ok || req.Metadata[k] != v {
			return false
		}
	}
	return true
}

func completion(id, model string, resp Response, u Usage) map[string]any {
	msg := map[string]any{"role": "assistant"}
	finish := "stop"
	if len(resp.ToolCalls) > 0 {
		finish = "tool_calls"
		tcs := make([]map[string]any, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			tcs = append(tcs, map[string]any{
				"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
			})
		}
		msg["tool_calls"] = tcs
		msg["content"] = nil
	} else {
		msg["content"] = resp.Content
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "created": fixedCreated, "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
			"total_tokens": u.PromptTokens + u.CompletionTokens,
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
