package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setRequiredEnv sets every env var loadConfig treats as required, so a test
// that wants to flip exactly one knob can do so without repeating the rest.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "gitlab-token", "gitlab-pat-value")
	writeFile(t, dir, "webhook-secret", strings.Repeat("a", 32))
	writeFile(t, dir, "meter-token", "meter-bearer-value")

	env := map[string]string{
		"GONK_GITLAB_URL":          "https://gitlab.orac.local",
		"GONK_GITLAB_TOKEN_FILE":   filepath.Join(dir, "gitlab-token"),
		"GONK_WEBHOOK_SECRET_FILE": filepath.Join(dir, "webhook-secret"),
		"GONK_WEBHOOK_PUBLIC_URL":  "https://gonk.orac.local/hook/gitlab",
		"GONK_METER_URL":           "https://gonk-meter.internal",
		"GONK_METER_TOKEN_FILE":    filepath.Join(dir, "meter-token"),
		"GONK_INSTANCE_LADDER":     "qwen-local,gpt-4o",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigRejectsEmptyInstanceLadder(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("GONK_INSTANCE_LADDER", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig with an empty GONK_INSTANCE_LADDER = nil error, want a fail-closed refusal")
	}
}

func TestLoadConfigRejectsUnsetInstanceLadder(t *testing.T) {
	setRequiredEnv(t)
	_ = os.Unsetenv("GONK_INSTANCE_LADDER")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig with GONK_INSTANCE_LADDER unset = nil error, want a fail-closed refusal")
	}
}

func TestLoadConfigParsesInstanceLadder(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("GONK_INSTANCE_LADDER", "qwen-local, gpt-4o ,claude-haiku")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig = %v", err)
	}
	want := []string{"qwen-local", "gpt-4o", "claude-haiku"}
	if len(cfg.InstanceLadder) != len(want) {
		t.Fatalf("ladder = %v, want %v", cfg.InstanceLadder, want)
	}
	for i, v := range want {
		if cfg.InstanceLadder[i] != v {
			t.Errorf("ladder[%d] = %q, want %q (whitespace around commas must be trimmed)", i, cfg.InstanceLadder[i], v)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig = %v", err)
	}
	if cfg.BotUsername != "gonk" {
		t.Errorf("BotUsername default = %q, want gonk", cfg.BotUsername)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr default = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.PrivateAddr != ":9090" {
		t.Errorf("PrivateAddr default = %q, want :9090", cfg.PrivateAddr)
	}
	if cfg.ReconcileInterval != 10*time.Minute {
		t.Errorf("ReconcileInterval default = %v, want 10m", cfg.ReconcileInterval)
	}
	if !cfg.HookSSLVerify {
		t.Error("HookSSLVerify default = false, want true (this is what GitLab does, not what we do)")
	}
}

func TestLoadConfigRequiresGitLabURL(t *testing.T) {
	setRequiredEnv(t)
	_ = os.Unsetenv("GONK_GITLAB_URL")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig with no GONK_GITLAB_URL = nil error")
	}
}

func TestLoadConfigRequiresMeterURL(t *testing.T) {
	setRequiredEnv(t)
	_ = os.Unsetenv("GONK_METER_URL")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig with no GONK_METER_URL = nil error")
	}
}

func TestLoadConfigRequiresWebhookPublicURL(t *testing.T) {
	setRequiredEnv(t)
	_ = os.Unsetenv("GONK_WEBHOOK_PUBLIC_URL")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig with no GONK_WEBHOOK_PUBLIC_URL = nil error")
	}
}

// --------------------------------------------------------------- readSecretFile

func TestReadSecretFileTrimsExactlyOneTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "s", "s3cr3t\n")
	got, err := readSecretFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cr3t" {
		t.Errorf("got %q, want s3cr3t (exactly one trailing newline trimmed)", got)
	}
}

func TestReadSecretFileRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "s", "")
	if _, err := readSecretFile(p); err == nil {
		t.Fatal("readSecretFile on an empty file = nil error; an empty webhook secret would accept every request")
	}
}

func TestReadSecretFileEmptyPathIsOptionalAndEmpty(t *testing.T) {
	got, err := readSecretFile("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (an unset optional secret path)", got)
	}
}

func TestReadSecretFileMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := readSecretFile(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("readSecretFile on a missing file = nil error")
	}
}
