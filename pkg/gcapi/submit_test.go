package gcapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// SubmitSession is the prompt-delivery path. Gas City's k8s runtime provider
// never composes runtime.Config.PromptSuffix onto the launch command (only
// tmux/acp/herdr/t3bridge do), so template_overrides.initial_message set at
// create is silently dropped for pod-backed sessions and the agent boots idle.
// Delivering the prompt as a second signed call is the supported way in --
// see gonk-u1p.1 and the upstream report gonk-drf.

func TestSubmitSessionPostsMessageToSubmitRoute(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody struct {
		Message string `json:"message"`
		Intent  string `json:"intent"`
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.Unmarshal(readAll(t, r), &gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"status":"accepted","request_id":"r","event_cursor":"0"}`)
	}))

	if _, err := c.SubmitSession(context.Background(), "gonk.triage.p75.i16.a1", "do the thing", SubmitIntentDefault); err != nil {
		t.Fatalf("SubmitSession = %v", err)
	}
	if want := "/v0/city/gonk-city/session/gonk.triage.p75.i16.a1/submit"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotBody.Message != "do the thing" {
		t.Errorf("message = %q, want %q", gotBody.Message, "do the thing")
	}
	if gotBody.Intent != "default" {
		t.Errorf("intent = %q, want %q", gotBody.Intent, "default")
	}
}

// The alias is the correlation handle we stamp at create; the upstream route
// documents {id} as "Session ID, alias, or runtime session_name", so an alias
// containing dots must survive path escaping intact.
func TestSubmitSessionEscapesTheAlias(t *testing.T) {
	var gotPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{}`)
	}))
	if _, err := c.SubmitSession(context.Background(), "gonk.triage.p75.i16.a1", "m", SubmitIntentDefault); err != nil {
		t.Fatalf("SubmitSession = %v", err)
	}
	if want := "/v0/city/gonk-city/session/gonk.triage.p75.i16.a1/submit"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// A 404 must stay distinguishable as IsNotFound. NOTE this is NOT the
// async-create window: upstream resolves the session after answering 202, so a
// missing session is never a 404 here (proven live -- see gonk-u1p.7). A 404 on
// this route means a wrong city or an unrouted path.
func TestSubmitSessionNotFoundIsDistinguishable(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"detail":"no such session"}`)
	}))
	_, err := c.SubmitSession(context.Background(), "missing-alias", "m", SubmitIntentDefault)
	if err == nil {
		t.Fatal("SubmitSession = nil, want error")
	}
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false, want true", err)
	}
}

// It is a mutation, so it must carry the same ed25519 write grant as RunOrder
// and CreateSession. An unsigned submit would 401 against a write-auth city.
func TestSubmitSessionIsSigned(t *testing.T) {
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
		_, _ = fmt.Fprint(w, `{}`)
	}))
	c.Signer = signer

	if _, err := c.SubmitSession(context.Background(), "alias", "hi", SubmitIntentDefault); err != nil {
		t.Fatalf("SubmitSession = %v", err)
	}
	if !seen {
		t.Fatal("server handler never ran")
	}
}

func TestSubmitSessionRejectsEmptyInputs(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be called on invalid input")
		w.WriteHeader(http.StatusAccepted)
	}))
	if _, err := c.SubmitSession(context.Background(), "", "m", SubmitIntentDefault); err == nil {
		t.Error("empty id: err = nil, want error")
	}
	// Upstream validates message with minLength:1 + pattern \S, so a
	// whitespace-only prompt is a 422 there. Reject locally instead of
	// spending a signed round-trip to learn it.
	if _, err := c.SubmitSession(context.Background(), "alias", "   ", SubmitIntentDefault); err == nil {
		t.Error("blank message: err = nil, want error")
	}
}
