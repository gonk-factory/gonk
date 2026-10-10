//go:build component

package main

// The model endpoint and the checkout listener a Jobs session talks to,
// stubbed IN THIS TEST PROCESS and reached from the cluster over the network
// (see jobs_component_test.go for why in-process, and README-component-jobs.md
// for the address). Everything it receives is recorded, so the test asserts
// on what the session actually sent -- the key it authenticated with, the
// metadata header, the prompt text -- rather than on what the pod says it did.
//
// Routes, all under /s/<session>/ so one server serves every session in a
// run and each session gets its own scenario:
//
//	POST /s/<session>/v1/chat/completions  LiteLLM (opencode streams: SSE)
//	GET  /s/<session>/v1/models            LiteLLM (not expected: the
//	                                       entrypoint sets OPENCODE_DISABLE_MODELS_FETCH)
//	GET  /s/<session>/rig.tar.gz           gonk-intake's checkout listener
//
// ANY other path is recorded too, and that is the point of recording it: a
// prompt fetch (/v1/prompt/<alias>, the Gas City delivery path) must never
// happen under Jobs, and "the stub saw none" is only evidence if the stub
// would have seen one.

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubScenario is what the stub does for one session.
type stubScenario int

const (
	// scenarioAnswer: every chat completion answers with the scenario's text
	// (opencode's title call and the real turn alike, as the T-55 gate stub
	// did). The rig route serves a small checkout.
	scenarioAnswer stubScenario = iota + 1
	// scenarioStall: EVERY route for the session -- rig and model alike --
	// accepts the request and never answers, until the client goes away or
	// the stub is closed. Whichever the session reaches first is where it
	// stalls, so the stall does not depend on how far the entrypoint got
	// before the deadline.
	scenarioStall
	// scenarioLoop (gonk-un7z): every chat completion that offers TOOLS is
	// answered with the same tool call -- opencode's `read` of loopMissingFile,
	// a path the stub checkout does not contain -- so REAL opencode fails the
	// same call again and again, as qwen3-14b did on rom#1. A completion with
	// no tools (opencode's title call) gets plain text. After loopGiveUp tool
	// calls the stub gives up and answers text with no batch: a watchdog that
	// never fired then shows up as a Succeeded session, not a hang.
	scenarioLoop
)

// loopMissingFile is the file scenarioLoop's "model" keeps reading. It is not
// in writeStubCheckout's tree.
const loopMissingFile = "/workspace/internal/widget/adapter.go"

// loopGiveUp bounds scenarioLoop's tool calls per session.
const loopGiveUp = 8

// stubRequest is one request the stub received.
type stubRequest struct {
	Session  string
	Method   string
	Path     string // path AFTER /s/<session>, or the full path when unrouted
	Auth     string // Authorization header, verbatim
	Metadata string // x-litellm-spend-logs-metadata header, verbatim
	Body     string
	Stalled  bool
}

type sessionStub struct {
	scenario stubScenario
	answer   string // scenarioAnswer: the assistant text
	// malformedArgs (gonk-mf3v): the i-th model call that offers tools is
	// answered with a `glob` tool call whose arguments are this RAW string --
	// not valid JSON, which is what a small model produced live on project 75
	// (2026-10-04). After the last one the stub answers with `answer`.
	malformedArgs []string
	// ifPrompt / answerThen (gonk-nf9i): a chat completion whose REQUEST BODY
	// contains ifPrompt is answered with answerThen instead of answer. One
	// stub "model" can then answer differently by what its prompt says --
	// which proves, on a real session, that the text reached the model.
	ifPrompt   string
	answerThen string
}

type componentStub struct {
	t   *testing.T
	srv *http.Server
	ln  net.Listener
	// base is the URL a pod uses to reach the stub, without a trailing slash.
	base string
	done chan struct{}

	mu       sync.Mutex
	sessions map[string]sessionStub
	reqs     []stubRequest
	// loopCalls counts the tool calls scenarioLoop has answered, per session.
	loopCalls map[string]int
}

// loopToolCalls is how many tool calls scenarioLoop answered for session.
func (s *componentStub) loopToolCalls(session string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loopCalls[session]
}

// startComponentStub listens on listen (host:port; port 0 picks one) and
// serves until the test ends. The host must be an address the cluster's pods
// can reach, because it is also the address handed to them.
func startComponentStub(t *testing.T, listen string) *componentStub {
	t.Helper()
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatalf("component stub: listen on %s: %v", listen, err)
	}
	s := &componentStub{
		t:         t,
		ln:        ln,
		base:      "http://" + ln.Addr().String(),
		done:      make(chan struct{}),
		sessions:  map[string]sessionStub{},
		loopCalls: map[string]int{},
	}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	t.Cleanup(func() {
		close(s.done) // releases every stalled handler
		_ = s.srv.Close()
	})
	return s
}

// register gives session its scenario and returns the per-session base URL
// (what GONK_LITELLM_URL is set to; opencode appends /v1).
func (s *componentStub) register(session string, sc sessionStub) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session] = sc
	return s.base + "/s/" + session
}

// requests returns a copy of every request recorded for session ("" = all).
func (s *componentStub) requests(session string) []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stubRequest
	for _, r := range s.reqs {
		if session == "" || r.Session == session {
			out = append(out, r)
		}
	}
	return out
}

func (s *componentStub) record(r stubRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
}

func (s *componentStub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	req := stubRequest{
		Method:   r.Method,
		Path:     r.URL.Path,
		Auth:     r.Header.Get("Authorization"),
		Metadata: r.Header.Get("x-litellm-spend-logs-metadata"),
		Body:     string(body),
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/s/")
	if !ok {
		s.record(req)
		http.NotFound(w, r)
		return
	}
	session, sub, _ := strings.Cut(rest, "/")
	req.Session = session
	req.Path = "/" + sub
	s.mu.Lock()
	sc, known := s.sessions[session]
	s.mu.Unlock()
	if !known {
		s.record(req)
		http.NotFound(w, r)
		return
	}

	if sc.scenario == scenarioStall {
		req.Stalled = true
		s.record(req)
		select {
		case <-r.Context().Done():
		case <-s.done:
		}
		return
	}
	s.record(req)

	switch {
	case r.Method == http.MethodPost && req.Path == "/v1/chat/completions":
		stream := strings.Contains(req.Body, `"stream":true`) || strings.Contains(req.Body, `"stream": true`)
		if sc.scenario == scenarioLoop && strings.Contains(req.Body, `"tools"`) {
			s.mu.Lock()
			s.loopCalls[session]++
			n := s.loopCalls[session]
			s.mu.Unlock()
			if n <= loopGiveUp {
				writeStubToolCall(w, stream, fmt.Sprintf("call_loop_%d", n), "read",
					fmt.Sprintf(`{"filePath":%q}`, loopMissingFile))
				return
			}
			writeStubText(w, stream, "I could not find the file, so I am giving up without a batch.\n")
			return
		}
		if len(sc.malformedArgs) > 0 && strings.Contains(req.Body, `"tools"`) {
			s.mu.Lock()
			s.loopCalls[session]++
			n := s.loopCalls[session]
			s.mu.Unlock()
			if n <= len(sc.malformedArgs) {
				writeStubToolCall(w, stream, fmt.Sprintf("call_malformed_%d", n), "glob", sc.malformedArgs[n-1])
				return
			}
		}
		if sc.ifPrompt != "" && strings.Contains(req.Body, sc.ifPrompt) {
			writeStubText(w, stream, sc.answerThen)
			return
		}
		writeStubText(w, stream, sc.answer)
	case r.Method == http.MethodGet && req.Path == "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
			map[string]any{"id": componentModel, "object": "model", "owned_by": "gonk-component-stub"},
		}})
	case r.Method == http.MethodGet && req.Path == "/rig.tar.gz":
		writeStubCheckout(w)
	default:
		http.NotFound(w, r)
	}
}

// writeStubCheckout serves a GitLab-archive-shaped tarball: one wrapping
// directory, members owned by uid 0 (as a forge archive is), which the
// entrypoint extracts as uid 65532 with --no-same-owner.
func writeStubCheckout(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/gzip")
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{"component-checkout/README.md", "# synthetic checkout for the t55 component test\n"},
		{"component-checkout/NOTES.md", "nothing to see here\n"},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)),
			Uid: 0, Gid: 0, Uname: "root", Gname: "root", ModTime: time.Unix(1752400000, 0), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f.body))
	}
	_ = tw.Close()
	_ = gz.Close()
}

// writeStubText answers a chat completion with text, streamed as SSE when the
// client asked for a stream (opencode does), else as one JSON body.
func writeStubText(w http.ResponseWriter, stream bool, text string) {
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-component", "object": "chat.completion", "created": 1752400000, "model": componentModel,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": text}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	send := func(delta map[string]any, finish string) {
		ch := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			ch["finish_reason"] = finish
		}
		b, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-component", "object": "chat.completion.chunk",
			"created": 1752400000, "model": componentModel, "choices": []any{ch},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	send(map[string]any{"role": "assistant", "content": ""}, "")
	send(map[string]any{"content": text}, "")
	send(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// writeStubToolCall answers a chat completion with ONE tool call, streamed as
// SSE when the client asked for a stream (opencode does), else as one JSON
// body -- the OpenAI wire shape @ai-sdk/openai-compatible parses.
func writeStubToolCall(w http.ResponseWriter, stream bool, id, name, args string) {
	call := map[string]any{"index": 0, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args}}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-component", "object": "chat.completion", "created": 1752400000, "model": componentModel,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	send := func(delta map[string]any, finish string) {
		ch := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			ch["finish_reason"] = finish
		}
		b, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-component", "object": "chat.completion.chunk",
			"created": 1752400000, "model": componentModel, "choices": []any{ch},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	send(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}}, "")
	send(map[string]any{}, "tool_calls")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}
