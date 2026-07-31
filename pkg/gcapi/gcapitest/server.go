// Package gcapitest is an in-memory Gas City supervisor: enough of the order-run
// route to drive gonk-gate's tests with no city, no cluster, no containers.
package gcapitest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
)

// Server records every order poured against it, so a test can assert exactly
// what was fired and with which vars -- which is how Task 3 proves the
// re-sling path.
type Server struct {
	// Poured is append-only, in request order. Only SUCCESSFUL pours are
	// recorded: a request answered with an injected failure (see Fail) was not
	// actually accepted by the supervisor, so it must not appear here.
	Poured []Pour
	// Created is append-only, in request order: every accepted CreateSession
	// (POST /v0/city/{city}/sessions). The triage broker (C2) creates sessions
	// directly instead of pouring the gonk-triage formula, so dispatch tests
	// assert against this rather than Poured.
	Created []CreatedSession
	// Fail maps an order name to a count of remaining 5xx responses: each
	// matching request decrements the count and answers 502 instead of being
	// recorded, until the count reaches zero. The key "sessions" injects
	// failures on the CreateSession route.
	Fail map[string]int
	// SessionOutputs maps a session id/alias to the last_output the fake returns
	// for a peek GET (GetSessionOutput). A key that is absent answers 404, so a
	// caller exercises the IsNotFound path.
	SessionOutputs map[string]string
	// Submitted is append-only, in request order: every accepted SubmitSession
	// (POST /v0/city/{city}/session/{id}/submit). gonk delivers the agent's
	// prompt here rather than as create-time initial_message, because Gas City's
	// k8s runtime provider never composes PromptSuffix onto the launch command
	// (gonk-u1p.1 / upstream gonk-drf), so dispatch tests assert the prompt
	// against this rather than against Created.
	Submitted []SubmittedMessage
	// SubmitNotFoundUntil maps an id/alias to a count of remaining 404s: each
	// matching submit decrements the count and answers 404 instead of being
	// recorded, until it reaches zero. Create is async upstream, so the session
	// legitimately may not exist for the first few submits -- this models that
	// window so a caller's retry loop is exercised rather than assumed.
	SubmitNotFoundUntil map[string]int
	// SubmitAttemptsSeen counts EVERY submit request reaching the fake, including
	// ones answered with a 404 or an injected failure. A caller that treats a
	// non-404 as retryable would burn its whole budget here, so a test can assert
	// the attempt count rather than only the outcome.
	SubmitAttemptsSeen int
	// SubmitFail is a count of remaining 401s on the submit route, decremented
	// per request. Unlike SubmitNotFoundUntil these are NOT the async-create
	// window -- they model a hard rejection that must be terminal, not retried.
	// 401 specifically (not 5xx) because gcapi.Client retries 5xx internally, so
	// a 5xx count would measure the CLIENT's retries; a 4xx is terminal there,
	// which makes SubmitAttemptsSeen isolate the CALLER's own retry loop.
	SubmitFail int

	srv  *httptest.Server
	mu   sync.Mutex
	next int
}

// Pour is one accepted order-run request.
type Pour struct {
	Order string
	Vars  map[string]string
}

// CreatedSession is one accepted CreateSession request, recorded verbatim so a
// test can assert the kind/name/alias/message dispatch sent.
type CreatedSession struct {
	Kind    string
	Name    string
	Alias   string
	Message string
	Async   bool
}

// SubmittedMessage is one accepted SubmitSession request, recorded verbatim so
// a test can assert which session got which prompt, with which intent.
type SubmittedMessage struct {
	ID      string
	Message string
	Intent  string
}

// New starts an httptest.Server backing a fresh, empty fake supervisor.
// t.Cleanup closes it.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the fake's base URL, suitable for gcapi.New.
func (s *Server) URL() string { return s.srv.URL }

// Client builds a *gcapi.Client wired to this fake for city, with retries
// unthrottled (RetryBackoff returns 0) so a test exercising Fail does not
// sleep through the real client's backoff.
func (s *Server) Client(city string) *gcapi.Client {
	c := gcapi.New(s.URL(), city)
	c.RetryBackoff = func(int) time.Duration { return 0 }
	return c
}

// PouredNames is the order name of each recorded Pour, in order -- a
// convenience for assertions that only care what ran, not with which vars.
func (s *Server) PouredNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, len(s.Poured))
	for i, p := range s.Poured {
		names[i] = p.Order
	}
	return names
}

// parseRunPath extracts (city, order) from "/v0/city/{city}/order/{name}/run",
// mirroring exactly the route gcapi.Client.RunOrder builds. Anything else 404s,
// same as the real supervisor answering an unknown route.
func parseRunPath(p string) (city, order string, ok bool) {
	const prefix = "/v0/city/"
	const mid = "/order/"
	const suffix = "/run"
	rest, found := strings.CutPrefix(p, prefix)
	if !found {
		return "", "", false
	}
	city, rest, found = strings.Cut(rest, mid)
	if !found || city == "" {
		return "", "", false
	}
	order, found = strings.CutSuffix(rest, suffix)
	if !found || order == "" {
		return "", "", false
	}
	return city, order, true
}

// parseSessionsPath extracts city from "/v0/city/{city}/sessions", mirroring
// the route gcapi.Client.CreateSession builds. Anything else returns ok=false.
func parseSessionsPath(p string) (city string, ok bool) {
	const prefix = "/v0/city/"
	const suffix = "/sessions"
	rest, found := strings.CutPrefix(p, prefix)
	if !found {
		return "", false
	}
	city, found = strings.CutSuffix(rest, suffix)
	if !found || city == "" || strings.Contains(city, "/") {
		return "", false
	}
	return city, true
}

// parseSessionGetPath extracts (city, id) from "/v0/city/{city}/session/{id}",
// mirroring the route gcapi.Client.GetSessionOutput builds.
func parseSessionGetPath(p string) (city, id string, ok bool) {
	const prefix = "/v0/city/"
	const mid = "/session/"
	rest, found := strings.CutPrefix(p, prefix)
	if !found {
		return "", "", false
	}
	city, id, found = strings.Cut(rest, mid)
	if !found || city == "" || id == "" || strings.Contains(city, "/") {
		return "", "", false
	}
	return city, id, true
}

// parseSessionSubmitPath extracts (city, id) from
// "/v0/city/{city}/session/{id}/submit", mirroring the route
// gcapi.Client.SubmitSession builds. Checked BEFORE the bare session path so a
// submit is not mistaken for a read of a session literally named "{id}/submit".
func parseSessionSubmitPath(p string) (city, id string, ok bool) {
	const suffix = "/submit"
	rest, found := strings.CutSuffix(p, suffix)
	if !found {
		return "", "", false
	}
	city, id, ok = parseSessionGetPath(rest)
	if !ok || strings.Contains(id, "/") {
		return "", "", false
	}
	return city, id, true
}

// handle answers the routes gcapi.Client speaks:
// POST /v0/city/{cityName}/order/{name}/run, POST /v0/city/{cityName}/sessions,
// POST /v0/city/{cityName}/session/{id}/submit,
// and GET /v0/city/{cityName}/session/{id}.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, id, ok := parseSessionGetPath(r.URL.Path); ok {
			s.handleGetSession(w, id)
			return
		}
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if _, id, ok := parseSessionSubmitPath(r.URL.Path); ok {
		s.handleSubmitSession(w, r, id)
		return
	}
	if _, ok := parseSessionsPath(r.URL.Path); ok {
		s.handleCreateSession(w, r)
		return
	}
	city, order, ok := parseRunPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	var body struct {
		Vars map[string]string `json:"vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.Fail[order] > 0 {
		s.Fail[order]--
		s.mu.Unlock()
		http.Error(w, "injected failure", http.StatusBadGateway)
		return
	}
	s.next++
	trackingID := fmt.Sprintf("trk-%d", s.next)
	s.Poured = append(s.Poured, Pour{Order: order, Vars: body.Vars})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gcapi.RunResult{
		Status:     "queued",
		ScopedName: city + "/" + order,
		TrackingID: trackingID,
	})
}

// handleCreateSession mirrors gascity's always-async agent create: it records
// the request and answers 202 with {status, request_id, event_cursor}. Fail
// keyed on "sessions" injects a 502 (not recorded), exercising the client's
// retry path.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Alias   string `json:"alias"`
		Message string `json:"message"`
		Async   bool   `json:"async"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.Fail["sessions"] > 0 {
		s.Fail["sessions"]--
		s.mu.Unlock()
		http.Error(w, "injected failure", http.StatusBadGateway)
		return
	}
	s.next++
	reqID := fmt.Sprintf("req-%d", s.next)
	s.Created = append(s.Created, CreatedSession{
		Kind: body.Kind, Name: body.Name, Alias: body.Alias, Message: body.Message, Async: body.Async,
	})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(gcapi.CreateSessionResult{
		Status:      "accepted",
		RequestID:   reqID,
		EventCursor: "0",
	})
}

// handleSubmitSession mirrors gascity's async submit: it records the message
// and answers 202. SubmitNotFoundUntil[id] answers 404 first (not recorded),
// modelling the window between an async create being accepted and the session
// actually existing.
func (s *Server) handleSubmitSession(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Message string `json:"message"`
		Intent  string `json:"intent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.SubmitAttemptsSeen++
	if s.SubmitFail > 0 {
		s.SubmitFail--
		s.mu.Unlock()
		http.Error(w, "injected rejection", http.StatusUnauthorized)
		return
	}
	if s.SubmitNotFoundUntil[id] > 0 {
		s.SubmitNotFoundUntil[id]--
		s.mu.Unlock()
		http.Error(w, `{"detail":"session not found"}`, http.StatusNotFound)
		return
	}
	s.next++
	reqID := fmt.Sprintf("req-%d", s.next)
	s.Submitted = append(s.Submitted, SubmittedMessage{ID: id, Message: body.Message, Intent: body.Intent})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "request_id": reqID})
}

// handleGetSession answers a peek read: it returns a SessionView carrying the
// configured last_output for id, or 404 when no output is registered (so a
// caller exercises GetSessionOutput's IsNotFound path).
func (s *Server) handleGetSession(w http.ResponseWriter, id string) {
	s.mu.Lock()
	out, ok := s.SessionOutputs[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"detail":"session not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gcapi.SessionView{
		ID: id, State: "idle", LastOutput: out,
	})
}
