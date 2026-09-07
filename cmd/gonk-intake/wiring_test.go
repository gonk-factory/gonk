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

// wiringConfig is the minimum Config newService accepts, pointed at glabtest.
func wiringConfig(t *testing.T, gl *glabtest.Server) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
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
}

func wiringService(t *testing.T, mutate func(*Config)) (*service, *glabtest.Server) {
	t.Helper()
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 99, Username: "gonk"}
	cfg := wiringConfig(t, gl)
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := newService(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newService = %v", err)
	}
	return svc, gl
}

// The issue sweep can be unplugged in one line -- delete `Issues: dp` from
// newService -- and every unit test in pkg/intake stays green, because they all
// call sweepIssues directly. That is the dead-but-tested shape this project
// keeps rediscovering, so the wiring gets its own assertion at the seam where it
// actually happens.
func TestNewServiceWiresTheIssueSweep(t *testing.T) {
	svc, gl := wiringService(t, nil)
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

// The blocklist is the only thing between a GitLab membership change and a
// code-writing agent with a path to its own broker and gate (gonk-jn5). Same
// unplug-in-one-line risk, same seam.
func TestNewServiceWiresTheBlocklist(t *testing.T) {
	svc, _ := wiringService(t, func(c *Config) {
		c.BlockedProjects = []string{"agentic/gonk-project"}
	})
	if svc.Reconciler.Blocked.Empty() {
		t.Fatal("Reconciler.Blocked is empty: the blocklist is parsed and then discarded, " +
			"so a project on it would be reconciled and dispatched to anyway (gonk-jn5)")
	}
	if !svc.Reconciler.Blocked.Blocked(glab.Project{ID: 69, PathWithNamespace: "agentic/gonk-project"}) {
		t.Error("the configured entry does not actually block the project it names")
	}
}

// FAIL CLOSED AT STARTUP. An entry that will not parse is one that would block
// less than the operator wrote, so the process must refuse to come up rather
// than run with a list that silently protects nothing.
func TestNewServiceRefusesToStartOnAnUnparseableBlocklist(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 99, Username: "gonk"}
	cfg := wiringConfig(t, gl)
	// The dangerous typo: looks deliberate, parses as a name, matches nothing.
	cfg.BlockedProjects = []string{"gonk-project"}

	if _, err := newService(context.Background(), cfg, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("started with a blocklist entry that blocks nothing; it must fail closed")
	}
}
