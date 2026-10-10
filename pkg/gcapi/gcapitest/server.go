// Package gcapitest is an in-memory Gas City supervisor: enough of the order-run
// route to drive gonk-gate's tests with no city, no cluster, no containers.
package gcapitest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/gcapi"
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
	// sessions is the session LIFECYCLE, keyed by id/alias. A session is a state
	// machine, not a string: three separate production bugs (gonk-u1p.1/.2/.5)
	// lived in the create -> prompt -> finish -> read chain and NONE was visible
	// to the flat output map this replaces, because a map cannot express "not
	// finished yet" -- the exact state the broker read path was getting wrong.
	// Drive it with RunSession/FinishSession/CrashSession; an unknown key 404s,
	// so a caller still exercises the IsNotFound path.
	sessions map[string]*fakeSession
	// Submitted is append-only, in request order: every accepted SubmitSession
	// (POST /v0/city/{city}/session/{id}/submit). gonk delivers the agent's
	// prompt here rather than as create-time initial_message, because Gas City's
	// k8s runtime provider never composes PromptSuffix onto the launch command
	// (gonk-u1p.1 / upstream gonk-drf), so dispatch tests assert the prompt
	// against this rather than against Created.
	Submitted []SubmittedMessage
	// SubmitUnresolvedUntil maps an id/alias to a count of remaining
	// resolve_failed outcomes: each matching submit decrements the count and is
	// answered 202 (like every submit) but emits request.failed with
	// error_code=resolve_failed instead of being recorded, until it reaches zero.
	//
	// This models the async-create window AS IT ACTUALLY IS. An earlier version
	// of this fake answered 404 there, which upstream never does -- the route
	// resolves the session in a goroutine AFTER answering 202 -- and that
	// fiction is exactly why the round-trip test could not see gonk-u1p.7.
	SubmitUnresolvedUntil map[string]int
	// SubmitInactiveUntil is the same shape for the OTHER retryable outcome:
	// the session resolved but its runtime is not live yet, which upstream
	// reports as error_code=submit_failed with session.ErrSessionInactive's
	// text. Retryable for the same reason -- the pod is still starting.
	SubmitInactiveUntil map[string]int
	// SubmitSilent accepts submits with a 202 and emits NO terminal event, so a
	// caller has to face the one answer it cannot get: the outcome is unknown.
	// That must never be read as success -- and must not be resubmitted either,
	// since the message may well have landed.
	SubmitSilent bool
	// CreateFailWith, when non-empty, makes every create answer 202 and then
	// emit request.failed with that error_message and error_code=create_failed.
	CreateFailWith string
	// NotRunningUntil maps an id/alias to a count of remaining reads that report
	// running=false: each GetSession decrements it. Agent create is async, so a
	// freshly created session is start-pending for a while -- and a default
	// submit into a start-pending session is PARKED on the nudge queue rather
	// than delivered (proven live: the parked copy never arrived). Modelling the
	// window is what forces a caller to wait for the runtime instead of firing a
	// prompt into a session that cannot receive one.
	NotRunningUntil map[string]int
	// StartPendingReads and CreatingReads model Gas City's REAL start lifecycle
	// for every created session, as observed live on project 75 issue 73
	// (2026-09-13, GET /session/<alias> polled every 5s): first
	// state=start-pending running=false while the controller has not attempted a
	// start -- which under FS pressure lasts until a reconciler tick is allowed to
	// run -- then state=creating running=false while the pod is being made (the
	// pod fetched its prompt DURING this phase), then commandable. Each read of a
	// session consumes one: its first StartPendingReads reads report
	// start-pending, the next CreatingReads report creating, and every read after
	// that reports the session's own state. Per session rather than per alias,
	// because dispatch aliases carry a nonce a test cannot know in advance.
	StartPendingReads int
	CreatingReads     int
	// OnClose, when set, is called after a successful close of id, before the
	// response is written. A test uses it to charge a close its measured
	// duration on a fake clock.
	OnClose func(id string)
	// CloseHangs makes every close block until the caller gives up, without
	// closing anything -- a close slower than any budget.
	CloseHangs bool
	// CloseAttempts records every close REQUEST, whether it succeeded, failed
	// or hung -- so a test can prove the close it is about was reached.
	CloseAttempts []string
	// transcriptReads counts every transcript GET, answered or not, so a test
	// can bound how often a caller pays for the (up to 4 MiB) output of record
	// -- see TranscriptReadCount.
	transcriptReads int
	// peekReads records the peek_lines of every session GET that asked for a
	// preview, so a test can bound how often a caller takes a DEEP capture
	// (up to ~8.8 MB of cap) -- see PeekReadsDeeperThan.
	peekReads []int
	// transcriptTurns holds explicit conversation-format transcripts; see
	// SetTranscriptTurns.
	transcriptTurns map[string][]gcapi.TranscriptTurn
	// CreateSilent accepts creates with a 202 and emits no terminal event,
	// modelling the NORMAL case for a real pod: upstream emits the create's
	// success event only after WaitForSessionCommandable, so a session whose pod
	// is still starting has said nothing yet. That must not read as a failure.
	CreateSilent bool
	// SubmitAttemptsSeen counts EVERY submit request reaching the fake, including
	// ones whose outcome event is a failure and ones answered with an injected
	// rejection. A caller that retries something terminal would burn its whole
	// budget here, so a test can assert the attempt count, not only the outcome.
	SubmitAttemptsSeen int
	// SubmitFail is a count of remaining 401s on the submit route, decremented
	// per request. Unlike SubmitUnresolvedUntil these are NOT the async-create
	// window -- they model a hard rejection that must be terminal, not retried.
	// 401 specifically (not 5xx) because gcapi.Client retries 5xx internally, so
	// a 5xx count would measure the CLIENT's retries; a 4xx is terminal there,
	// which makes SubmitAttemptsSeen isolate the CALLER's own retry loop.
	SubmitFail int
	// Closed is append-only, in request order: the id/alias of every ACCEPTED
	// CloseSession (POST /v0/city/{city}/session/{id}/close). A test asserts
	// teardown against LiveSessions rather than this -- what matters is that no
	// session survived, not that a call was made -- but the order is here for a
	// test that needs to prove close ran exactly once.
	Closed []string
	// ListPageSize, when > 0, makes the session list PAGINATE at that size --
	// the behaviour of the real route, which is keyset-paginated with a server
	// cap. A client that reads page one and stops sees an arbitrary prefix, and
	// for a reaper that is a silent under-reap: success reported having missed
	// most of what it exists to find.
	ListPageSize int
	// ListPartial makes every session-list page report partial=true, modelling
	// "one or more backends failed and this list is incomplete". A caller that
	// destroys things based on absence must not read that as "these are all the
	// sessions that exist".
	ListPartial bool
	// CloseFail is a count of remaining 401s on the close route, decremented per
	// request. 401 (not 5xx) for the same reason as SubmitFail: gcapi.Client
	// retries 5xx internally, so only a 4xx isolates the caller's own handling.
	// The point of injecting it is that a failed teardown must be LOUD -- a
	// silently-swallowed close is how the leak got here in the first place.
	CloseFail int

	// AmbiguousAliases makes a session read answer 409 session-conflict, the way
	// upstream does when an alias resolves to MORE THAN ONE session (gonk-u6p:
	// "gonk.triage.p75.i24.a1" matched both go-s4ug and go-93gk).
	//
	// The property that matters, and why this is a set rather than a countdown
	// like SubmitFail/CloseFail: THIS ERROR NEVER CLEARS. Both sessions go on
	// existing, so every retry gets the same 409 forever. A fake that decremented
	// would model a transient blip and quietly hide the only thing worth testing
	// here -- that gonk stops waiting on a read that can never succeed.
	AmbiguousAliases map[string]bool

	srv    *httptest.Server
	mu     sync.Mutex
	next   int
	seq    uint64
	events []wireEvent
}

// wireEvent is one row of the fake's city event log, in gascity's list shape.
// The log exists because the ONLY place an async request's outcome is reported
// is this stream -- the 202 says nothing (see gcapi.SubmitResult).
type wireEvent struct {
	Seq     uint64       `json:"seq"`
	Type    string       `json:"type"`
	Subject string       `json:"subject,omitempty"`
	Payload eventPayload `json:"payload"`
}

type eventPayload struct {
	RequestID    string `json:"request_id"`
	Operation    string `json:"operation,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Queued       bool   `json:"queued,omitempty"`
	Intent       string `json:"intent,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// recordEventLocked appends one terminal event and returns its seq. Callers
// hold s.mu.
func (s *Server) recordEventLocked(eventType, subject string, payload eventPayload) {
	s.seq++
	s.events = append(s.events, wireEvent{Seq: s.seq, Type: eventType, Subject: subject, Payload: payload})
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

// parseEventsPath extracts city from "/v0/city/{city}/events", mirroring the
// route gcapi.Client.listEvents builds.
func parseEventsPath(p string) (city string, ok bool) {
	const prefix = "/v0/city/"
	const suffix = "/events"
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

// parseSessionClosePath extracts (city, id) from
// "/v0/city/{city}/session/{id}/close", mirroring the route
// gcapi.Client.CloseSession builds. Checked alongside the submit path, before
// the bare session path, for the same reason.
func parseSessionClosePath(p string) (city, id string, ok bool) {
	const suffix = "/close"
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
// POST /v0/city/{cityName}/session/{id}/close,
// and GET /v0/city/{cityName}/session/{id}.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := parseEventsPath(r.URL.Path); ok {
			s.handleEventList(w, r)
			return
		}
		if _, id, ok := parseSessionTranscriptPath(r.URL.Path); ok {
			s.handleGetTranscript(w, id, r.URL.Query().Get("tail") == "0")
			return
		}
		if _, ok := parseSessionsPath(r.URL.Path); ok {
			s.handleListSessions(w, r)
			return
		}
		if _, id, ok := parseSessionGetPath(r.URL.Path); ok {
			// peek_lines, snake_case, EXACTLY as upstream binds it
			// (GASCITY_REF internal/api/huma_types_sessions.go:43). The fake
			// carried gonk's camelCase typo, so it agreed with the client
			// about a name the real server has never read and no test in this
			// tree could see the mismatch -- which is how gonk-lv3y's
			// brokerPeekLines=400 stayed inert and unnoticed. A fake that
			// mirrors the client's spelling instead of the server's is not a
			// fake, it is the same bug written twice.
			n, _ := strconv.Atoi(r.URL.Query().Get("peek_lines"))
			s.handleGetSession(w, id, r.URL.Query().Get("peek") == "true", n)
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
	if _, id, ok := parseSessionClosePath(r.URL.Path); ok {
		s.handleCloseSession(w, r, id)
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
	cursor := s.seq

	// Create is async in the HANDLER too: it answers 202 and only then validates
	// and spawns, so its real outcome is a terminal event exactly like submit's.
	// CreateFailWith models that half -- observed live as a taken alias answering
	// 202 and then emitting create_failed while dispatch logged success.
	if s.CreateFailWith != "" {
		s.recordEventLocked("request.failed", "", eventPayload{
			RequestID:    reqID,
			Operation:    "session.create",
			ErrorCode:    "create_failed",
			ErrorMessage: s.CreateFailWith,
		})
		s.mu.Unlock()
		writeAccepted(w, reqID, cursor)
		return
	}

	s.Created = append(s.Created, CreatedSession{
		Kind: body.Kind, Name: body.Name, Alias: body.Alias, Message: body.Message, Async: body.Async,
	})
	// An accepted agent create spawns the session in the background, so it comes
	// into existence RUNNING with no output. A test moves it on with
	// FinishSession/CrashSession -- nothing here ever produces a finished
	// session implicitly, which is what makes the lifecycle assertable.
	if body.Alias != "" {
		s.putSessionLocked(body.Alias, SessionRunning, "")
	}
	if !s.CreateSilent {
		s.recordEventLocked("request.result.session.create", body.Alias, eventPayload{
			RequestID: reqID,
			SessionID: body.Alias,
		})
	}
	s.mu.Unlock()

	writeAccepted(w, reqID, cursor)
}

// handleListSessions mirrors GET /v0/city/{city}/sessions, INCLUDING ITS
// PAGINATION, which is the part worth modelling: the real route is keyset-
// paginated with a server cap, so a client that reads one page and stops sees an
// arbitrary prefix. For the orphan reaper that is a silent under-reap. A fake
// that returned everything in one page could not show that, so this one pages at
// ListPageSize whenever a test sets it.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	items := make([]gcapi.SessionSummary, 0, len(ids))
	for _, id := range ids {
		sess := s.sessions[id]
		// Closed sessions are gone; upstream's list is of live ones and a reaper
		// must not be handed corpses to re-close.
		if sess.State == SessionClosed {
			continue
		}
		// THE LIST IS THE OPEN-BEAD LIST, NOT THE LIVE-POD LIST, and the
		// difference is a leak the reaper cannot see (gonk-wmb7). Upstream's
		// humaHandleSessionList reads the session read model with IncludeClosed
		// unset, so a session whose bead gascity closed is absent here even
		// though its pod is running. See BeadClosedPodAlive.
		if sess.beadClosed {
			continue
		}
		created := sess.createdAt
		if created == "" {
			created = "2020-01-01T00:00:00Z"
		}
		items = append(items, gcapi.SessionSummary{
			ID: id, Alias: id, State: string(sess.State), CreatedAt: created,
		})
	}
	partial := s.ListPartial
	pageSize := s.ListPageSize
	s.mu.Unlock()

	start := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		start, _ = strconv.Atoi(c)
	}
	if start > len(items) {
		start = len(items)
	}
	end := len(items)
	next := ""
	if pageSize > 0 && start+pageSize < len(items) {
		end = start + pageSize
		next = strconv.Itoa(end)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gcapi.SessionList{
		Items: items[start:end], Total: len(items), NextCursor: next, Partial: partial,
	})
}

// handleCloseSession mirrors gascity's close, and the thing worth modelling is
// that it is NOT like create and submit: upstream's humaHandleSessionClose calls
// handle.CloseDetailed INLINE and only then answers, so its 200 {"status":"ok"}
// is a REAL RECEIPT, not a 202 that means "we will get to it". A close is
// therefore the one session mutation a caller may believe. An unknown id 404s,
// so a caller still exercises the IsNotFound path -- which matters, because
// closing a session that is already gone must not read as a failure.
func (s *Server) handleCloseSession(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	s.CloseAttempts = append(s.CloseAttempts, id)
	if s.CloseHangs {
		s.mu.Unlock()
		// A close that never answers: only the CALLER's timeout ends it. The
		// session is not closed and nothing is recorded.
		<-r.Context().Done()
		return
	}
	if s.CloseFail > 0 {
		s.CloseFail--
		s.mu.Unlock()
		http.Error(w, `{"detail":"injected close rejection"}`, http.StatusUnauthorized)
		return
	}
	sess, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, `{"detail":"session not found"}`, http.StatusNotFound)
		return
	}
	// A CLOSED BEAD MAKES CLOSE UNREACHABLE (gonk-wmb7). Upstream resolves the
	// close target with allowClosed=false, so once the bead is closed this route
	// answers 404 no matter how alive the pod is -- and nothing changes here:
	// the session stays in the map, still live, still holding its pod.
	if sess.beadClosed {
		s.mu.Unlock()
		http.Error(w, `{"detail":"session not found"}`, http.StatusNotFound)
		return
	}
	// Close is terminal and idempotent upstream: the runtime is stopped and the
	// bead closed. Output is preserved -- closing does not erase what the agent
	// said, and sweep may legitimately have read it moments earlier.
	sess.State = SessionClosed
	s.Closed = append(s.Closed, id)
	hook := s.OnClose
	s.mu.Unlock()
	if hook != nil {
		// Outside the lock: a hook may advance a test clock or read the fake.
		hook(id)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

// writeAccepted answers the 202 every async session route returns: an
// acknowledgement plus the correlation handle, and nothing about the outcome.
func writeAccepted(w http.ResponseWriter, reqID string, cursor uint64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(gcapi.CreateSessionResult{
		Status:      "accepted",
		RequestID:   reqID,
		EventCursor: strconv.FormatUint(cursor, 10),
	})
}

// handleSubmitSession mirrors gascity's async submit EXACTLY: it answers 202
// unconditionally -- resolution and delivery happen after the response -- and
// reports the real outcome only as a terminal event on the city log.
//
// So a submit for a session that does not exist yet looks IDENTICAL on the wire
// to one that lands. That is not a quirk of this fake; it is the observed
// upstream behaviour (proven live 2026-08-01, gonk-u1p.7), and modelling it any
// other way lets a caller that ignores the event stream pass its tests and drop
// every prompt in production.
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
	// SubmitFail is a TRANSPORT-level rejection (no grant, wrong host), which
	// really is answered synchronously upstream. It is the one submit failure
	// that never reaches the event log.
	if s.SubmitFail > 0 {
		s.SubmitFail--
		s.mu.Unlock()
		http.Error(w, "injected rejection", http.StatusUnauthorized)
		return
	}
	s.next++
	reqID := fmt.Sprintf("req-%d", s.next)
	cursor := s.seq

	// A default-intent submit into a session that is not running yet is PARKED,
	// not delivered: upstream answers ok with queued=true and hands the message
	// to the nudge queue. Modelled here because "accepted" and "delivered" part
	// company exactly there.
	parked := false
	if sess, ok := s.sessions[id]; ok && sess.State == SessionRunning && s.NotRunningUntil[id] > 0 {
		parked = true
	}

	switch {
	case parked:
		s.recordEventLocked("request.result.session.submit", id, eventPayload{
			RequestID: reqID,
			SessionID: id,
			Queued:    true,
			Intent:    body.Intent,
		})
	case s.SubmitSilent:
		// Accepted, and then nothing is ever said about it.
	case s.SubmitUnresolvedUntil[id] > 0:
		s.SubmitUnresolvedUntil[id]--
		s.recordEventLocked("request.failed", "", eventPayload{
			RequestID:    reqID,
			Operation:    "session.submit",
			ErrorCode:    "resolve_failed",
			ErrorMessage: fmt.Sprintf("session not found: %q", id),
		})
	case s.SubmitInactiveUntil[id] > 0:
		s.SubmitInactiveUntil[id]--
		s.recordEventLocked("request.failed", "", eventPayload{
			RequestID:    reqID,
			Operation:    "session.submit",
			ErrorCode:    "submit_failed",
			ErrorMessage: fmt.Sprintf("session is not active: %s", id),
		})
	default:
		s.Submitted = append(s.Submitted, SubmittedMessage{ID: id, Message: body.Message, Intent: body.Intent})
		s.recordEventLocked("request.result.session.submit", id, eventPayload{
			RequestID: reqID,
			SessionID: id,
			Intent:    body.Intent,
		})
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":       "accepted",
		"request_id":   reqID,
		"event_cursor": strconv.FormatUint(cursor, 10),
	})
}

// handleEventList answers GET /v0/city/{city}/events, newest first, optionally
// filtered by type -- the read gcapi.AwaitRequestOutcome uses to learn what an
// async request actually did.
func (s *Server) handleEventList(w http.ResponseWriter, r *http.Request) {
	eventType := r.URL.Query().Get("type")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	s.mu.Lock()
	items := make([]wireEvent, 0, len(s.events))
	for i := len(s.events) - 1; i >= 0; i-- { // newest first, as upstream orders it
		if eventType != "" && s.events[i].Type != eventType {
			continue
		}
		items = append(items, s.events[i])
		if limit > 0 && len(items) >= limit {
			break
		}
	}
	total := len(items)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": total})
}

// handleGetSession answers a session read: it returns a SessionView carrying
// the configured last_output for id, or 404 when no output is registered (so a
// caller exercises GetSessionOutput's IsNotFound path).
//
// peek is honoured rather than assumed (gonk-lv3y): upstream returns
// last_output ONLY when peek=true, and GetSessionState depends on that -- a
// fake that always sent the preview could not show the difference between the
// read that carries an unbounded terminal capture and the read that does not,
// which is the entire distinction the fix rests on.
func (s *Server) handleGetSession(w http.ResponseWriter, id string, peek bool, peekLines int) {
	s.mu.Lock()
	if peek {
		s.peekReads = append(s.peekReads, peekLines)
	}
	ambiguous := s.AmbiguousAliases[id]
	s.mu.Unlock()
	if ambiguous {
		// Upstream's shape, kept verbatim from the live 409 on gonk-u6p so the
		// error string a caller logs in a test is the one it logs in production.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"urn:gascity:error:session-conflict",` +
			`"title":"Session State Conflict","status":409,` +
			`"detail":"ambiguous: ambiguous session identifier: \"` + id + `\" matches 2 sessions"}`))
		return
	}

	s.mu.Lock()
	sess, ok := s.sessions[id]
	var state, out string
	var running bool
	if ok {
		state, out, running = string(sess.State), sess.output, sess.State == SessionRunning
		sess.reads++
		if running {
			switch {
			case sess.reads <= s.StartPendingReads:
				state, running = string(SessionStartPending), false
			case sess.reads <= s.StartPendingReads+s.CreatingReads:
				state, running = string(SessionCreating), false
			}
		}
		if running && s.NotRunningUntil[id] > 0 {
			s.NotRunningUntil[id]--
			state, running = string(SessionStartPending), false
		}
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"detail":"session not found"}`, http.StatusNotFound)
		return
	}
	// peek is a PREVIEW WINDOW, not the transcript. Modelling the truncation is
	// the point -- a batch pushed past the window by a chatty agent is silently
	// unreadable (gonk-u1p.3), and a fake that always returned everything could
	// not show that.
	//
	// Upstream's window is `capture-pane -S -<peek_lines>` with no -E, i.e.
	// peek_lines rows of SCROLLBACK plus the whole visible pane, and it is
	// served only while the session is Running (handler_sessions.go:604). The
	// line budget modelled here is therefore peek_lines + a pane height, not
	// peek_lines; zero means the server's own default of 5.
	view := gcapi.SessionView{ID: id, State: state, Running: running}
	if peek && running {
		view.LastOutput = capturePane(out, peekLines)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}

// --- session lifecycle -------------------------------------------------------

// SessionState is the slice of gascity's session lifecycle gonk reacts to. The
// values match gascity's SessionView.State strings, because production reads
// them (broker_apply.go compares against "running").
type SessionState string

const (
	// SessionRunning is a session that is still working. Its output is not the
	// final word and must not be judged -- see gonk-u1p.2.
	SessionRunning SessionState = "running"
	// SessionStopped is a session that finished normally. Only now is its
	// output the output of record.
	SessionStopped SessionState = "stopped"
	// SessionCrashed is a session that died. Upstream emits session.crashed
	// alongside session.stopped; it is terminal too, but carries no batch.
	SessionCrashed SessionState = "crashed"
	// SessionClosed is a session gonk has torn down: the runtime is stopped and
	// the pod is gone. This is the ONLY state that returns the cluster's CPU and
	// memory. A "stopped" session still holds its pod -- that distinction is
	// exactly what nobody was making, and it cost 13 leaked pods and a wedged
	// scheduler (gonk-xkm).
	SessionClosed SessionState = "closed"
	// SessionStartPending and SessionCreating are the two states a freshly
	// accepted agent session passes through before it is commandable. They are
	// only ever REPORTED (see StartPendingReads/CreatingReads), never stored.
	// The strings are upstream's (GASCITY_REF internal/session/manager.go).
	SessionStartPending SessionState = "start-pending"
	SessionCreating     SessionState = "creating"

	// SessionActive and SessionAwake are gascity's LIVE runtime states: the
	// controller has confirmed a runtime process is up. gonk's startPhaseOf
	// (cmd/gonk-gate/prompt_wait.go) treats them, with "running", as the live
	// set, from upstream's own definitions.
	//
	// The fake reports `running` only for SessionRunning, and that is not an
	// omission -- it is the shape gonk observed live and had to be fixed for
	// (gonk-7k73): a session whose runtime is up and working while the
	// `running` flag still reads false. A test that wants a live agent the
	// completion check mistakes for a finished one asks for this state.
	SessionActive SessionState = "active"
	SessionAwake  SessionState = "awake"
)

type fakeSession struct {
	State  SessionState
	output string
	// createdAt is RFC3339 and drives the reaper's grace window. Empty means
	// "long ago", so a test that does not care about the window gets the
	// reapable case by default and must opt IN to the young-session case.
	createdAt string
	// reads counts GetSession reads of this session, for StartPendingReads.
	reads int
	// beadClosed marks a session whose SESSION BEAD gascity closed while its
	// runtime kept running. See BeadClosedPodAlive: it is a state gonk cannot
	// reach by asking, only observe, and the one the reaper is blind to.
	beadClosed bool
}

// CreateSessionAt declares a session that already exists, created at the given
// RFC3339 time. It is how a test expresses the one case the orphan reaper must
// never get wrong: a session created seconds ago by a dispatch that has not yet
// written its bead record, which must be left alone rather than reaped out from
// under a live agent.
func (s *Server) CreateSessionAt(id string, state SessionState, createdAt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putSessionLocked(id, state, "")
	s.sessions[id].createdAt = createdAt
}

// putSessionLocked upserts a session. Caller holds s.mu.
func (s *Server) putSessionLocked(id string, state SessionState, output string) {
	if s.sessions == nil {
		s.sessions = map[string]*fakeSession{}
	}
	sess, ok := s.sessions[id]
	if !ok {
		sess = &fakeSession{}
		s.sessions[id] = sess
	}
	sess.State = state
	if output != "" {
		sess.output = output
	}
}

// RunSession puts a session in the RUNNING state with the partial output it has
// produced so far. Use it to assert that a caller declines to judge mid-flight;
// a test that wants a judgeable session wants FinishSession instead.
func (s *Server) RunSession(id, partialOutput string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putSessionLocked(id, SessionRunning, partialOutput)
}

// FinishSession transitions a session to STOPPED with its final output -- the
// moment its output becomes the output of record and may be judged. It
// upserts, so a test may declare an already-finished session without creating
// one first.
func (s *Server) FinishSession(id, output string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putSessionLocked(id, SessionStopped, output)
}

// AmbiguousAlias makes every read of alias answer 409 session-conflict, and keep
// answering it. Models the state gonk-u6p wedged on: two sessions minted under
// one alias, so the name no longer identifies anything and never will again.
// Deliberately has no "un-ambiguous" counterpart -- nothing in production clears
// it either.
func (s *Server) AmbiguousAlias(alias string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AmbiguousAliases == nil {
		s.AmbiguousAliases = map[string]bool{}
	}
	s.AmbiguousAliases[alias] = true
}

// BeadClosedPodAlive puts a session into the state gonk-wmb7 was found in:
// gascity has CLOSED THE SESSION BEAD while the pod kept running.
//
// It is not a state gonk asks for. gascity's own start path can roll a pending
// create back, close the bead with
// close_reason "session create failed: aborted before creation_complete", and
// then still bring the runtime up -- observed on 2026-09-21, session go-whg1
// (bead closed 06:32:34, creation_complete_at 06:32:35, pod Running 25 minutes
// later and still holding its 500m/1Gi).
//
// Three behaviours follow from the closed bead, and all three are upstream's,
// verified against the live supervisor at GASCITY_REF:
//
//   - IT IS ABSENT FROM THE SESSION LIST. humaHandleSessionList reads the
//     session read model with IncludeClosed unset, so a closed bead is not a
//     row. Live: GET /v0/city/gonk/sessions returned {"items":[],"total":0}
//     while GET /v0/city/gonk/beads?type=session&all=true returned 832 rows,
//     this session among them. THE ORPHAN REAPER THEREFORE CANNOT SEE IT.
//   - CLOSE 404s, FOREVER. humaHandleSessionClose resolves with
//     allowClosed=false, so there is no open bead to resolve and the only call
//     that returns a pod answers "not found". Kill is resolved the same way.
//     Live: POST .../close -> 404 at 06:32:38, eight seconds after a 200 on
//     that same alias's transcript.
//   - IT STILL READS. The query routes resolve with allowClosed=true, so
//     GetSessionOutput and the transcript keep answering 200 -- which is why a
//     caller cannot tell this apart from a session that was torn down cleanly.
//
// A test uses it to hold gonk to the only honest thing it can do here: say so.
func (s *Server) BeadClosedPodAlive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		s.putSessionLocked(id, SessionRunning, "")
	}
	s.sessions[id].beadClosed = true
}

// CrashSession transitions a session to CRASHED: terminal, so it is judgeable,
// but it produced no batch.
func (s *Server) CrashSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putSessionLocked(id, SessionCrashed, "")
}

// CloseAttemptCount is how many close requests have arrived, safely.
func (s *Server) CloseAttemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.CloseAttempts)
}

// TranscriptReadCount is how many transcript reads have arrived, safely. It is
// what a cost bound asserts on: the sweep consults the transcript for a fence
// only when the preview cannot answer (gonk-lv3y, gonk-xflt), and "only when"
// is a claim about this number.
func (s *Server) TranscriptReadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transcriptReads
}

// PeekReadsDeeperThan is how many preview reads asked for more than lines rows
// of scrollback, safely. It is what bounds the deep capture's frequency the way
// TranscriptReadCount bounds the transcript's (gonk-xflt NEW-2).
func (s *Server) PeekReadsDeeperThan(lines int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.peekReads {
		if l > lines {
			n++
		}
	}
	return n
}

// LiveSessions is every session this supervisor still holds a pod for, sorted:
// one that exists and has not been closed. THIS is what a teardown test asserts
// on -- not the sweep's exit code, not whether a close call was made.
//
// The distinction is the whole point of gonk-xkm. A session that stopped, or
// crashed, or was judged and reported and marked done, is still a running pod
// holding 500m CPU and 1Gi of memory. Every one of those looked like a success
// from inside gonk, and eleven of them wedged the cluster's scheduler.
func (s *Server) LiveSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var live []string
	for id, sess := range s.sessions {
		if sess.State != SessionClosed {
			live = append(live, id)
		}
	}
	sort.Strings(live)
	return live
}

// SessionStateOf reports a session's current state ("" when unknown), so a
// round-trip test can assert the lifecycle it drove actually happened.
func (s *Server) SessionStateOf(id string) SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		return sess.State
	}
	return ""
}

// handleGetTranscript answers GET /v0/city/{city}/session/{id}/transcript the
// way gascity does for THIS DEPLOYMENT (gonk-xflt).
//
// A RUNNING session with no session log -- the deployed agent's case, since
// gascity resolves no log for opencode -- is served gascity's FALLBACK: a pane
// capture of gcapi.GascityTranscriptPeekLines rows of scrollback plus the whole
// pane, as format "text" with one role "output" turn (GASCITY_REF 4fda5a28
// handler_session_transcript.go:140). The fake used to serve the WHOLE output
// here, which let a fix that relies on "the transcript sees everything" pass
// against the fake while failing against production: every live transcript
// read is roles "output=1", 1.5-2.5 KB.
//
// A session given explicit turns (SetTranscriptTurns) is served those, as
// format "conversation" -- gascity's primary path when it does resolve a
// session log, and the one that carries the prompt as a role "user" turn.
//
// A NON-running session keeps the old shape, the whole output as one
// assistant turn. That is NOT what the pinned gascity does (its fallback Peek
// fails on an inactive session and it serves no turns); it models the
// conversation path for tests of a session that finished. The deployed agent
// never stops (images/agent/entrypoint.sh holds), so it is not a deployed state.
func (s *Server) handleGetTranscript(w http.ResponseWriter, id string, allSegments bool) {
	s.mu.Lock()
	s.transcriptReads++
	sess, ok := s.sessions[id]
	var out string
	var running bool
	turnsSet, hasTurns := s.transcriptTurns[id]
	if ok {
		out, running = sess.output, sess.State == SessionRunning
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"detail":"session not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"id": id, "template": "triage", "provider": "open-code"}
	switch {
	case hasTurns:
		resp["format"], resp["turns"] = gcapi.TranscriptFormatConvo, turnsSet
	case running:
		turns := []map[string]any{}
		if out != "" {
			turns = append(turns, map[string]any{"role": gcapi.TurnRoleOutput,
				"text": capturePane(out, gcapi.GascityTranscriptPeekLines)})
		}
		resp["format"], resp["turns"] = gcapi.TranscriptFormatText, turns
	default:
		// tail: "0 returns all segments"; omitting it returns only the most
		// recent. The fake honours the distinction so a caller that forgets
		// tail=0 sees a truncated transcript rather than silently getting
		// everything.
		turns := []map[string]any{}
		if out != "" {
			text := out
			if !allSegments {
				text = tailLines(out, 1)
			}
			turns = append(turns, map[string]any{"role": gcapi.TurnRoleAssistant, "text": text})
		}
		resp["format"], resp["turns"] = gcapi.TranscriptFormatConvo, turns
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// SetTranscriptTurns makes id's transcript a conversation-format session log
// with exactly these turns, as gascity serves when it resolves one.
func (s *Server) SetTranscriptTurns(id string, turns []gcapi.TranscriptTurn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transcriptTurns == nil {
		s.transcriptTurns = map[string][]gcapi.TranscriptTurn{}
	}
	s.transcriptTurns[id] = turns
}

// parseSessionTranscriptPath extracts (city, id) from
// "/v0/city/{city}/session/{id}/transcript".
func parseSessionTranscriptPath(p string) (city, id string, ok bool) {
	rest, found := strings.CutSuffix(p, "/transcript")
	if !found {
		return "", "", false
	}
	city, id, ok = parseSessionGetPath(rest)
	if !ok || strings.Contains(id, "/") {
		return "", "", false
	}
	return city, id, true
}

// fakePaneRows and fakeDefaultPeekLines are the agent pane height and
// gascity's defaultSessionPeekLines, so the fake's window is the same shape as
// the real one. They are DERIVED, not restated (gonk-z61b): gcapi's pair is the
// one copy TestPeekCapIsDerivedFromTheAgentPaneGeometry holds against
// images/agent/entrypoint.sh, so a pane resize either moves this fake with it
// or turns that test red. A literal 200 here would keep modelling the old
// window while the production cap moved.
const (
	fakePaneRows         = gcapi.AgentPaneRows
	fakeDefaultPeekLines = gcapi.GascityDefaultPeekLines
)

// clampScrollback is the scrollback a peek of peekLines rows asks tmux for:
// gascity floors it at its default.
func clampScrollback(peekLines int) int {
	if peekLines < fakeDefaultPeekLines {
		return fakeDefaultPeekLines
	}
	return peekLines
}

// historyKept is how many rows of scrollback tmux still holds after scrolled
// rows have gone off the top of the pane. It is NOT min(scrolled, limit):
// tmux's grid_collect_history, on reaching history-limit, drops a TENTH of
// the limit at once, so the history saws between limit-limit/10+1 and limit
// depending on how much has been printed (gonk-xflt NEW-1). Measured on tmux
// 3.2a, 4000x200 pane, default limit 2000: `seq 1 N` then an idle cursor row
// captures (capture-pane -S -10000, wc -l) 1501 rows at N=1500, 2001 at 2200,
// 2003 at 2202, 2101 at 2300, 2200 at 2399, 2002 at 2401, 2001 at 3000 and
// 5000 -- exactly this function (TestFakeCaptureMatchesMeasuredTmux).
// A fake that kept a steady 2000 served the BEST phase and let a test pass
// that real tmux fails.
func historyKept(scrolled int) int {
	limit := gcapi.AgentPaneHistoryLimit
	if scrolled <= limit {
		return scrolled
	}
	tenth := max(limit/10, 1)
	return limit - tenth + 1 + (scrolled-limit-1)%tenth
}

// capturePane is what `capture-pane -p -S -<peekLines>` returns for a pane
// that has printed out: the rows tmux still holds, the requested scrollback of
// them, plus the whole visible pane. out's trailing newline leaves the cursor
// on an empty last row, as a real pane does.
func capturePane(out string, peekLines int) string {
	rows := strings.Count(out, "\n") + 1
	kept := historyKept(max(rows-fakePaneRows, 0))
	return tailLines(out, fakePaneRows+min(clampScrollback(peekLines), kept))
}

// PeekWindowLines is how many rows of output a peek asking for peekLines rows
// of scrollback is GUARANTEED to carry, whatever phase tmux's history is in:
// the pane plus the scrollback asked for, capped at the least history tmux
// ever holds once it is full (gcapi.AgentPaneHistoryGuaranteed). A test that
// means to put something inside or outside a capture must compute against
// THIS, not against its own copy of the geometry (gonk-z61b), and a test that
// claims coverage must claim the worst case (gonk-xflt NEW-1).
func PeekWindowLines(peekLines int) int {
	return fakePaneRows + min(clampScrollback(peekLines), gcapi.AgentPaneHistoryGuaranteed)
}

// PeekWindowLinesMax is the most such a peek can ever carry: the pane plus
// a full history-limit of scrollback. Past it, nothing is reachable in any
// phase.
func PeekWindowLinesMax(peekLines int) int {
	return fakePaneRows + min(clampScrollback(peekLines), gcapi.AgentPaneHistoryLimit)
}

// tailLines returns at most the last n lines of s, modelling peek's preview
// window. n <= 0 means "no limit".
func tailLines(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// GetJSON does a raw GET against the fake and decodes the JSON body. It exists
// for routes gonk does not yet have a client method for (the transcript --
// gonk-u1p.3), so the fake's own behaviour stays testable without pulling an
// unused method into pkg/gcapi.
func (s *Server) GetJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	resp, err := http.Get(s.srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
	return out
}
