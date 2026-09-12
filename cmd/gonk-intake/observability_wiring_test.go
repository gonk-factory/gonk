package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

// THE WIRING CHECK for gonk-pop3 item 1.
//
// pkg/ghook and pkg/intake both prove the records are emitted when a logger is
// wired in. This proves cmd/gonk-intake ACTUALLY WIRES ONE -- which is a
// different claim, and the one that decides whether `kubectl logs` shows
// anything. A nil hook.Log falls back to slog.Default(), so a missing wire-up
// would still emit SOMETHING in a unit test while writing to the wrong place in
// the binary; asserting on the logger newService was handed is what closes that.
//
// It is the same path TestServiceEndToEnd drives, with a capturing logger.
func TestNewServiceWiresTheDecisionRecordsIntoItsLogger(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 99, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("this repo does a thing"))

	meterSrv := newFakeMeterServer(t)
	orderSrv, _ := newRecordingOrderServer(t)

	dir := t.TempDir()
	hookSecret := strings.Repeat("b", 40)
	cfg := Config{
		GitLabURL:         gl.URL(),
		GitLabTokenFile:   writeFile(t, dir, "gitlab-token", "glabtest-token"),
		WebhookSecretFile: writeFile(t, dir, "webhook-secret", hookSecret),
		WebhookPublicURL:  "https://gonk.example.invalid/hook/gitlab",
		WebhookTokenGen:   "1",
		HookSSLVerify:     true,
		BotUsername:       "gonk",
		MeterURL:          meterSrv.URL,
		MeterTokenFile:    writeFile(t, dir, "meter-token", "meter-bearer"),
		SupervisorURL:     orderSrv.URL,
		InstanceLadder:    []string{"qwen-local"},
		ReconcileInterval: time.Hour,
		ListenAddr:        ":0",
		PrivateAddr:       ":0",
		Version:           "test",
	}

	var logged strings.Builder
	log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx := context.Background()
	svc, err := newService(ctx, cfg, log)
	if err != nil {
		t.Fatalf("newService = %v", err)
	}
	if _, err := svc.Reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce = %v", err)
	}

	payload := fmt.Sprintf(`{
		"object_kind": "note",
		"project": {"id": %d, "path_with_namespace": %q, "default_branch": "main", "web_url": "x"},
		"user": {"id": 12345, "username": "someone-else"},
		"object_attributes": {"id": 4471, "note": "just talking amongst ourselves", "noteable_type": "Issue", "discussion_id": "d1"},
		"issue": {"iid": 71}
	}`, p.ID, p.PathWithNamespace)

	r := httptest.NewRequest(http.MethodPost, "/hook/gitlab", strings.NewReader(payload))
	r.Header.Set("X-Gitlab-Event", "Note Hook")
	r.Header.Set("X-Gitlab-Token", hookSecret)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gitlab-Event-UUID", "wiring-check-1")
	w := httptest.NewRecorder()
	svc.Server.Public().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook = %d, want 200: %s", w.Code, w.Body.String())
	}

	select {
	case ev := <-svc.Events:
		svc.Dispatch.Handle(ctx, ev)
	case <-time.After(2 * time.Second):
		t.Fatal("the event never reached the queue")
	}

	// Both records, from the logger newService was handed -- not from
	// slog.Default(), and not from a logger a test set by hand afterwards.
	var sawDelivery, sawDecision bool
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue // intake writes other things too; only the records matter here
		}
		switch m["msg"] {
		case "webhook delivery":
			sawDelivery = true
			if m["delivery"] != "wiring-check-1" {
				t.Errorf("delivery record carries delivery=%v, want the header value", m["delivery"])
			}
		case "intake decision":
			sawDecision = true
			// This comment does not mention the bot, so the answer is a
			// REASONED ignore -- which is the whole point of the record.
			if m["decision"] != "ignored" || m["reason"] != "no_mention" {
				t.Errorf("decision record = decision:%v reason:%v, want ignored/no_mention", m["decision"], m["reason"])
			}
			if m["delivery"] != "wiring-check-1" {
				t.Errorf("decision record carries delivery=%v; it cannot be joined to the receiver's record", m["delivery"])
			}
		}
	}
	if !sawDelivery {
		t.Errorf("cmd/gonk-intake did not wire a logger into the webhook receiver:\n%s", logged.String())
	}
	if !sawDecision {
		t.Errorf("cmd/gonk-intake did not wire a logger into the dispatcher:\n%s", logged.String())
	}
	// And the untrusted comment body must not be anywhere in it.
	if strings.Contains(logged.String(), "just talking amongst ourselves") {
		t.Errorf("the note body reached the log:\n%s", logged.String())
	}
}
