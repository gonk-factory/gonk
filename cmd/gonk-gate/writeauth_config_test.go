package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// writeTestKeyPEM writes a PKCS#8 PEM ed25519 private key (the format
// `openssl genpkey -algorithm ed25519` emits, and what gcapi.LoadSigner reads)
// to a fresh temp file and returns its path.
func writeTestKeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "gc-write.ed25519")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// setGateRequiredEnv sets the two env vars loadGateConfig treats as required
// (GONK_CITY and GONK_METER_TOKEN_FILE), so a write-auth test can flip only the
// write-auth knobs.
func setGateRequiredEnv(t *testing.T) {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "meter-token")
	if err := os.WriteFile(tok, []byte("meter-bearer"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GONK_CITY", "gonk")
	t.Setenv("GONK_METER_TOKEN_FILE", tok)
	t.Setenv("GONK_SUPERVISOR_URL", "http://gascity.gascity.svc:8372")
}

// A configured key file builds a signer, and gc() carries it onto the client.
func TestLoadGateConfigBuildsSignerWhenKeyFileSet(t *testing.T) {
	setGateRequiredEnv(t)
	t.Setenv("GONK_GC_WRITE_KEY_FILE", writeTestKeyPEM(t))
	t.Setenv("GONK_GC_WRITE_KEY_ID", "gonk-write-1")

	cfg, err := loadGateConfig()
	if err != nil {
		t.Fatalf("loadGateConfig = %v", err)
	}
	if cfg.signer == nil {
		t.Fatal("cfg.signer is nil, want a signer built from GONK_GC_WRITE_KEY_FILE")
	}
	if cfg.gc().Signer == nil {
		t.Fatal("gc().Signer is nil, want the loaded signer attached to the client")
	}
}

// No key file -> no signer -> the client sends no grant (the loopback default).
func TestLoadGateConfigNoSignerWhenKeyFileUnset(t *testing.T) {
	setGateRequiredEnv(t)
	_ = os.Unsetenv("GONK_GC_WRITE_KEY_FILE")

	cfg, err := loadGateConfig()
	if err != nil {
		t.Fatalf("loadGateConfig = %v", err)
	}
	if cfg.signer != nil {
		t.Error("cfg.signer is non-nil with no key file configured")
	}
	if cfg.gc().Signer != nil {
		t.Error("gc().Signer is non-nil with no key file configured")
	}
}

// A key file that is set but unreadable/invalid is a loud misconfiguration
// (loadGateConfig fails, which main turns into exit 2), not a per-pour surprise.
func TestLoadGateConfigRejectsBadKeyFile(t *testing.T) {
	setGateRequiredEnv(t)
	t.Setenv("GONK_GC_WRITE_KEY_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("GONK_GC_WRITE_KEY_ID", "gonk-write-1")

	if _, err := loadGateConfig(); err == nil {
		t.Fatal("loadGateConfig with an unreadable key file = nil error, want a misconfiguration refusal")
	}
}
