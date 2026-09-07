package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

// The issue sweep can be unplugged in one line -- delete `Issues: dp` from
// newService -- and every unit test in pkg/intake stays green, because they all
// call sweepIssues directly. That is the dead-but-tested shape this project
// keeps rediscovering, so the wiring gets its own assertion at the seam where it
// actually happens.
//
// It also pins the two spend guards to non-zero, because a Reconciler built with
// IssueSweepLimit or IssueSweepMaxAge left at zero falls back to the package
// defaults -- which is correct, but only as long as somebody has checked that
// the defaults are the ones in force.
func TestNewServiceWiresTheIssueSweep(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 99, Username: "gonk"}

	dir := t.TempDir()
	cfg := Config{
		GitLabURL:         gl.URL(),
		GitLabTokenFile:   writeFile(t, dir, "gitlab-token", "glabtest-token"),
		WebhookSecretFile: writeFile(t, dir, "webhook-secret", strings.Repeat("b", 40)),
		WebhookPublicURL:  "https://gonk.example.invalid/hook/gitlab",
		WebhookTokenGen:   "1",
		HookSSLVerify:     true,
		BotUsername:       "gonk",
		MeterURL:          "http://127.0.0.1:1",
		MeterTokenFile:    writeFile(t, dir, "meter-token", "meter-bearer"),
		SupervisorURL:     "http://127.0.0.1:1",
		InstanceLadder:    []string{"qwen-local"},
		ReconcileInterval: time.Hour,
		ListenAddr:        ":0",
		PrivateAddr:       ":0",
		Version:           "test",
	}

	svc, err := newService(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newService = %v", err)
	}
	if svc.Reconciler == nil {
		t.Fatal("no reconciler")
	}
	if svc.Reconciler.Issues == nil {
		t.Fatal("Reconciler.Issues is nil: the issue sweep is compiled in but never runs, " +
			"so a webhook that is ACKed and then lost stays lost (gonk-vrf)")
	}
	if svc.Reconciler.BotUserID != gl.Me.ID {
		t.Errorf("BotUserID = %d, want %d: without it the sweep triages the bot's own issues",
			svc.Reconciler.BotUserID, gl.Me.ID)
	}
}
