package gcapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// CreateSession is the C2 create+correlate+inject call: ONE signed POST to the
// city-scoped sessions route that creates the agent session, stamps a unique
// alias (the correlation marker), and delivers the rendered prompt as the
// initial message. It mirrors RunOrder's write-auth exactly.
func TestCreateSessionPostsToCityScopedRoute(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"status":"accepted","request_id":"req-9","event_cursor":"42"}`)
	}))

	res, err := c.CreateSession(context.Background(), CreateSessionRequest{
		Kind:    "agent",
		Name:    "triage",
		Alias:   "gonk.triage.p75.i12.a0",
		Message: "triage this issue; emit your batch fenced GONK_BATCH_START/END",
		Async:   true,
	})
	if err != nil {
		t.Fatalf("CreateSession = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v0/city/gonk-city/sessions" {
		t.Fatalf("path = %q, want /v0/city/gonk-city/sessions", gotPath)
	}
	// The wire body must use gascity's sessionCreateBody json keys exactly, or
	// the supervisor silently ignores fields (e.g. a dropped alias => no
	// correlation handle; a dropped message => no inject).
	if gotBody["kind"] != "agent" || gotBody["name"] != "triage" ||
		gotBody["alias"] != "gonk.triage.p75.i12.a0" ||
		gotBody["message"] != "triage this issue; emit your batch fenced GONK_BATCH_START/END" ||
		gotBody["async"] != true {
		t.Fatalf("body = %+v", gotBody)
	}
	if res.Status != "accepted" || res.RequestID != "req-9" || res.EventCursor != "42" {
		t.Fatalf("res = %+v", res)
	}
}

// The create route is a mutation: with a Signer configured it MUST carry a
// valid X-GC-City-Write grant whose req digest covers the exact method/path/body
// on the wire -- identical to RunOrder. Verify against the reference server algo.
func TestCreateSessionIsSigned(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewSigner(priv, "k1", WithCID("city_gonk"), WithEpoch(1))
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		if r.Header.Get("X-GC-Request") != "true" {
			t.Fatal("missing X-GC-Request CSRF header")
		}
		_ = verifyGrantAsServer(t, pub, r, body) // fails the test on any mismatch
		seen = true
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"status":"accepted","request_id":"r","event_cursor":"0"}`)
	}))
	c.Signer = signer

	if _, err := c.CreateSession(context.Background(), CreateSessionRequest{
		Kind: "agent", Name: "triage", Alias: "gonk.triage.p1.i1.a0", Message: "hi", Async: true,
	}); err != nil {
		t.Fatalf("CreateSession = %v", err)
	}
	if !seen {
		t.Fatal("server handler never ran")
	}
}

// GetSessionOutput is the return-read: a single GET that asks the supervisor to
// include the last-output preview. It resolves by id OR alias, so the alias we
// stamped at create is a durable read handle.
func TestGetSessionOutputReadsPeekPreview(t *testing.T) {
	var gotPath, gotQuery, gotMethod string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotMethod = r.Method
		_, _ = fmt.Fprint(w, `{"id":"gc-7","state":"idle","last_output":"GONK_BATCH_START\n{\"effects\":[]}\nGONK_BATCH_END"}`)
	}))

	sv, err := c.GetSessionOutput(context.Background(), "gonk.triage.p75.i12.a0", 200)
	if err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/v0/city/gonk-city/session/gonk.triage.p75.i12.a0" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotQuery, "peek=true") || !strings.Contains(gotQuery, "peekLines=200") {
		t.Fatalf("query = %q, want peek=true&peekLines=200", gotQuery)
	}
	if sv.ID != "gc-7" || sv.State != "idle" || !strings.Contains(sv.LastOutput, "GONK_BATCH_START") {
		t.Fatalf("view = %+v", sv)
	}
}

// Reads admit by network position, not write-auth: the city GET routes declare
// no 401/403. A GET must NEVER carry a grant, even when a Signer is configured
// (signing a read would be a category error and could trip the mutation gate).
func TestGetSessionOutputIsUnsigned(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewSigner(priv, "k1")
	if err != nil {
		t.Fatal(err)
	}
	var sawGrant, sawCSRF bool
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawGrant = r.Header.Get("X-GC-City-Write") != ""
		sawCSRF = r.Header.Get("X-GC-Request") != ""
		_, _ = fmt.Fprint(w, `{"id":"gc-1","state":"idle","last_output":""}`)
	}))
	c.Signer = signer

	if _, err := c.GetSessionOutput(context.Background(), "gonk.triage.p1.i1.a0", 0); err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if sawGrant || sawCSRF {
		t.Fatalf("a read must send no write-auth headers (grant=%v csrf=%v)", sawGrant, sawCSRF)
	}
}

// peekLines=0 means "use the server default": the client omits the param
// rather than sending peekLines=0 (which the server would read as an explicit 0).
func TestGetSessionOutputOmitsZeroPeekLines(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = fmt.Fprint(w, `{"id":"gc-1","state":"idle","last_output":""}`)
	}))
	if _, err := c.GetSessionOutput(context.Background(), "a", 0); err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if strings.Contains(gotQuery, "peekLines") {
		t.Fatalf("query = %q, want no peekLines when 0", gotQuery)
	}
	if !strings.Contains(gotQuery, "peek=true") {
		t.Fatalf("query = %q, want peek=true", gotQuery)
	}
}

// Sweep must distinguish "no session for this alias" (a create that failed
// async, or a not-yet-materialized session) from other errors: a 404 read is
// IsNotFound, which sweep maps to the no-output re-sling path.
func TestGetSessionOutputNotFoundIsAPIError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"detail":"session not found"}`)
	}))
	_, err := c.GetSessionOutput(context.Background(), "missing-alias", 0)
	if err == nil {
		t.Fatal("want error")
	}
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want IsNotFound so sweep can treat it as no-output", err)
	}
}

// An id/alias with a '/' (aliases permit segmented names) must be path-escaped,
// never concatenated into the route.
func TestGetSessionOutputEscapesID(t *testing.T) {
	var gotPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = fmt.Fprint(w, `{"id":"gc-1"}`)
	}))
	_, _ = c.GetSessionOutput(context.Background(), "rig/triage", 0)
	if strings.Contains(gotPath, "/rig/triage") {
		t.Fatalf("id was not escaped: %q", gotPath)
	}
}

// City is part of the route for both calls; an empty one is refused at the door
// exactly as RunOrder refuses it.
func TestSessionCallsRefuseEmptyCity(t *testing.T) {
	if _, err := New("http://x", "").CreateSession(context.Background(), CreateSessionRequest{Kind: "agent", Name: "triage"}); err == nil {
		t.Fatal("CreateSession with empty city must error")
	}
	if _, err := New("http://x", "").GetSessionOutput(context.Background(), "a", 0); err == nil {
		t.Fatal("GetSessionOutput with empty city must error")
	}
}

// Kind and name are required; a create with neither is a client-side error
// (fail before the wire, since the supervisor would 400 anyway).
func TestCreateSessionRequiresKindAndName(t *testing.T) {
	c := New("http://x", "gonk-city")
	if _, err := c.CreateSession(context.Background(), CreateSessionRequest{Name: "triage"}); err == nil {
		t.Fatal("missing kind must error")
	}
	if _, err := c.CreateSession(context.Background(), CreateSessionRequest{Kind: "agent"}); err == nil {
		t.Fatal("missing name must error")
	}
}

// The default retry policy also guards CreateSession (a 5xx supervisor blip
// must not fail a dispatch on the first try). Uses the same bounded loop.
func TestCreateSessionRetriesOn5xx(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"status":"accepted","request_id":"r","event_cursor":"0"}`)
	}))
	if _, err := c.CreateSession(context.Background(), CreateSessionRequest{Kind: "agent", Name: "triage", Alias: "a", Async: true}); err != nil {
		t.Fatalf("CreateSession = %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (retried once)", calls)
	}
}
