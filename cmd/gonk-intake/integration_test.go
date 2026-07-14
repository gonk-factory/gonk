package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// newFakeMeterServer is a minimal stand-in for gonk-meter (Plan 03): it always
// resolves a project to "active" with triage enabled, and always answers
// /policy/decide with "run". That is enough to prove the WIRING in this
// package works end to end; Plan 03's own table of meter behaviour is not
// this test's concern.
func newFakeMeterServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(meterapi.HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(meterapi.DecidePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meterapi.DecideResponse{
			Decision: meterapi.DecisionRun,
			Rung:     "qwen-local",
			Metadata: map[string]string{},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meterapi.ProjectResponse{
			Project: "group/repo",
			State:   meterapi.StateActive,
			Effective: &meterapi.Effective{
				Enabled: true,
				Actions: meterapi.Actions{Triage: true},
				Ladder:  []string{"qwen-local"},
				Triage:  meterapi.Triage{LabelPrefix: "gonk::", RespondToMentions: true},
			},
			KeyRef: meterapi.KeyRef{SecretName: "gonk-key", SecretKey: "LITELLM_API_KEY"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// recordingOrderServer stands in for the Gas City supervisor's order API
// (OD-A): it records every POST so the test can assert exactly one order was
// fired.
type recordingOrderServer struct {
	mu    sync.Mutex
	posts []map[string]any
}

func (s *recordingOrderServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts)
}

func newRecordingOrderServer(t *testing.T) (*httptest.Server, *recordingOrderServer) {
	t.Helper()
	rec := &recordingOrderServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.posts = append(rec.posts, body)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestServiceEndToEnd is the DoD scenario: "cmd/gonk-intake builds and starts
// against a fake GitLab; a forged webhook is rejected 401; a valid issue
// webhook produces one order." It builds the whole dependency graph via
// newService (the same function run() uses) against glabtest (fake GitLab), a
// fake gonk-meter, and a fake Gas City order endpoint -- no real network, no
// live GitLab, no model.
func TestServiceEndToEnd(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 99, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("this repo does a thing"))

	meterSrv := newFakeMeterServer(t)
	orderSrv, orders := newRecordingOrderServer(t)

	dir := t.TempDir()
	gitlabTokenFile := writeFile(t, dir, "gitlab-token", "glabtest-token")
	hookSecret := strings.Repeat("b", 40)
	webhookSecretFile := writeFile(t, dir, "webhook-secret", hookSecret)
	meterTokenFile := writeFile(t, dir, "meter-token", "meter-bearer")

	cfg := Config{
		GitLabURL:         gl.URL(),
		GitLabTokenFile:   gitlabTokenFile,
		WebhookSecretFile: webhookSecretFile,
		WebhookPublicURL:  "https://gonk.example.invalid/hook/gitlab",
		WebhookTokenGen:   "1",
		HookSSLVerify:     true,
		BotUsername:       "gonk",
		MeterURL:          meterSrv.URL,
		MeterTokenFile:    meterTokenFile,
		SupervisorURL:     orderSrv.URL,
		InstanceLadder:    []string{"qwen-local"},
		ReconcileInterval: time.Hour,
		ListenAddr:        ":0",
		PrivateAddr:       ":0",
		Version:           "test",
	}

	ctx := context.Background()
	svc, err := newService(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newService = %v", err)
	}

	// A forged webhook (wrong token) must be rejected 401, and must never reach
	// the event queue.
	forged := httptest.NewRequest(http.MethodPost, "/hook/gitlab", strings.NewReader(`{}`))
	forged.Header.Set("X-Gitlab-Event", "Issue Hook")
	forged.Header.Set("X-Gitlab-Token", "not-the-right-secret-not-the-right-secret")
	forged.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	svc.Server.Public().ServeHTTP(w, forged)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged webhook = %d, want 401", w.Code)
	}
	select {
	case <-svc.Events:
		t.Fatal("a forged webhook must never reach the event queue")
	default:
	}

	// Reconcile once so the project enters the cache as `valid` (config +
	// .agent/ present, meter says active+triage) -- dispatch refuses to fire
	// for a project it has never reconciled.
	if _, err := svc.Reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce = %v", err)
	}
	if e, ok := svc.Cache.Get(p.ID); !ok || e.State() != "valid" {
		t.Fatalf("project state = %+v, want valid", e)
	}

	// A valid, correctly-signed issue-open webhook must be accepted and, once
	// handed to Dispatch, must fire exactly one order.
	payload := fmt.Sprintf(`{
		"object_kind": "issue",
		"project": {"id": %d, "path_with_namespace": %q, "default_branch": "main", "web_url": "x"},
		"user": {"id": 12345, "username": "someone-else"},
		"object_attributes": {"iid": 1, "action": "open", "title": "t", "description": "d"}
	}`, p.ID, p.PathWithNamespace)

	valid := httptest.NewRequest(http.MethodPost, "/hook/gitlab", strings.NewReader(payload))
	valid.Header.Set("X-Gitlab-Event", "Issue Hook")
	valid.Header.Set("X-Gitlab-Token", hookSecret)
	valid.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	svc.Server.Public().ServeHTTP(w, valid)
	if w.Code != http.StatusOK {
		t.Fatalf("valid webhook = %d, want 200: %s", w.Code, w.Body.String())
	}

	select {
	case ev := <-svc.Events:
		if ev.Kind != ghook.KindIssue {
			t.Fatalf("event kind = %v, want issue", ev.Kind)
		}
		svc.Dispatch.Handle(ctx, ev)
	case <-time.After(2 * time.Second):
		t.Fatal("the valid webhook's event never reached the queue")
	}

	if got := orders.count(); got != 1 {
		t.Fatalf("orders fired = %d, want exactly 1", got)
	}
}
