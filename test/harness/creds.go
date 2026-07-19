package harness

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
)

// Creds owns a per-run directory OUTSIDE the repo (under os.TempDir()) holding
// every generated test credential: the GitLab root PAT, the bot PAT, the webhook
// token, the meter bearer token, the LiteLLM admin key, and the ed25519
// write-auth keypair (D1). All are generated at runtime and removed at teardown.
//
// NOTHING UNDER test/ MAY EVER CONTAIN A CREDENTIAL. `make secrets-scan` (Task 9)
// greps the tree and fails the build if one appears, so the dir must live under
// os.TempDir(), never inside the repo.
type Creds struct{ dir string }

// NewCreds makes a 0700 per-run directory under baseDir (pass "" for os.TempDir).
// The runID keeps concurrent runs from colliding and makes a leftover dir
// traceable to the run that made it.
func NewCreds(baseDir, runID string) (*Creds, error) {
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("harness: NewCreds needs a non-empty runID")
	}
	dir := filepath.Join(baseDir, "gonk-e2e-"+runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("harness: create creds dir: %w", err)
	}
	// MkdirAll does not tighten an existing dir's mode; enforce 0700 explicitly so
	// a pre-existing world-readable dir cannot leak a secret we are about to write.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("harness: chmod creds dir: %w", err)
	}
	return &Creds{dir: dir}, nil
}

// Dir is the per-run credential directory.
func (c *Creds) Dir() string { return c.dir }

// WriteSecret writes value to a 0600 file named name inside the creds dir and
// returns its path. Every gonk service reads secrets from FILES (two rotation
// slots, Plan 02/03 Decision 12), so the harness passes PATHS, never env values:
// GONK_WEBHOOK_SECRET_FILE, GONK_METER_TOKEN_FILE, LITELLM_ADMIN_KEY_FILE,
// GONK_GITLAB_TOKEN_FILE. A harness that passed a secret by env would be testing
// a shape we do not ship.
func (c *Creds) WriteSecret(name, value string) (string, error) {
	p := c.Path(name)
	if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
		return "", fmt.Errorf("harness: write secret %q: %w", name, err)
	}
	// WriteFile respects umask, so a permissive umask could widen the mode past
	// 0600. Force it back down.
	if err := os.Chmod(p, 0o600); err != nil {
		return "", fmt.Errorf("harness: chmod secret %q: %w", name, err)
	}
	return p, nil
}

// Path is where a named secret lives (whether or not it has been written yet).
func (c *Creds) Path(name string) string { return filepath.Join(c.dir, name) }

// Cleanup removes the whole per-run directory and every secret in it.
func (c *Creds) Cleanup() error { return os.RemoveAll(c.dir) }

// RandomToken returns n bytes of crypto/rand, base64url-encoded (no padding).
// Used for the webhook secret (>= ghook.MinSecretLen = 32) and the meter bearer
// token. The encoded string is longer than n, so RandomToken(32) comfortably
// clears a 32-char minimum.
func RandomToken(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("harness: RandomToken needs n > 0")
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("harness: RandomToken: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ---------------------------------------------------------------- write-auth (D1)

// WriteAuthCreds is a per-run ed25519 write-auth keypair. Dispatch is grant-gated
// (RECONCILIATION-06 D1): the controller runs `gc start --foreground` with an
// 0.0.0.0 any-host [api] listener and rejects every order-run POST that does not
// carry a valid single-use X-GC-City-Write grant. This keypair is what lets the
// harness dispatch through that gate.
//
//   - The PRIVATE half is written as a PKCS#8 PEM file (0600) that gcapi.LoadSigner
//     reads; the same bytes are the payload of the k8s Secret referenced by the
//     chart's secrets.gcWriteKey (L3), pointed at by GONK_GC_WRITE_KEY_FILE for
//     both gonk-intake and the in-pod gonk-gate.
//   - The PUBLIC half is the "kid:base64(std)" string the controller verifies
//     with, passed as GC_CITY_WRITE_PUBKEY / chart gascity.writeAuth.verifyKey.
//
// The keypair is MINTED PER RUN and NEVER committed (secrets-scan would catch a
// leaked ed25519 private key). The three layers consume it differently: L1 builds
// an in-process gcapi.Signer via Signer(); L2/L3 mount the PEM file and set the
// three GONK_GC_WRITE_* env vars; L3 additionally ships the public VerifyKey().
type WriteAuthCreds struct {
	// KID selects the controller's verifying key. It is the prefix of VerifyKey()
	// and of the chart's gascity.writeAuth.verifyKey ("kid:base64"), and the value
	// intake/gate pass as GONK_GC_WRITE_KEY_ID.
	KID string

	priv    ed25519.PrivateKey
	pub     ed25519.PublicKey
	keyPath string // 0600 PKCS#8 PEM private key, under the creds dir
	pem     []byte // the PKCS#8 PEM bytes (== k8s Secret payload)
}

// NewWriteAuth mints a fresh ed25519 keypair, writes the PKCS#8 PEM private key
// into the creds dir under fileName (default "gc-write-key.pem"), and returns the
// credential. kid must be non-empty (it selects the controller's verifying key);
// use a short stable label like "gonk-e2e".
func (c *Creds) NewWriteAuth(kid, fileName string) (*WriteAuthCreds, error) {
	if strings.TrimSpace(kid) == "" {
		return nil, fmt.Errorf("harness: NewWriteAuth needs a non-empty kid")
	}
	if fileName == "" {
		fileName = "gc-write-key.pem"
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("harness: generate write-auth key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("harness: marshal write-auth key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	p := c.Path(fileName)
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("harness: write write-auth key: %w", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return nil, fmt.Errorf("harness: chmod write-auth key: %w", err)
	}
	return &WriteAuthCreds{
		KID:     kid,
		priv:    priv,
		pub:     pub,
		keyPath: p,
		pem:     pemBytes,
	}, nil
}

// KeyFile is the path to the 0600 PKCS#8 PEM private key. This is the value for
// GONK_GC_WRITE_KEY_FILE (intake + gate) and the file gcapi.LoadSigner reads.
func (w *WriteAuthCreds) KeyFile() string { return w.keyPath }

// PrivateKeyPEM is the PKCS#8 PEM private key bytes, i.e. the payload of the k8s
// Secret the chart references as secrets.gcWriteKey (L3). NEVER log or commit it.
func (w *WriteAuthCreds) PrivateKeyPEM() []byte {
	out := make([]byte, len(w.pem))
	copy(out, w.pem)
	return out
}

// VerifyKey is the PUBLIC verify key as "kid:base64(std)" -- STANDARD base64 of
// the raw 32-byte ed25519 public key, the exact format the controller expects for
// GC_CITY_WRITE_PUBKEY / chart gascity.writeAuth.verifyKey (values.schema.json
// G22). The kid derives from this string by splitting on the first ':', which is
// how the chart derives GONK_GC_WRITE_KEY_ID -- so signer kid and verify kid
// cannot drift.
func (w *WriteAuthCreds) VerifyKey() string {
	return w.KID + ":" + base64.StdEncoding.EncodeToString(w.pub)
}

// PublicKey is the raw ed25519 public key (for a stub verifier in tests).
func (w *WriteAuthCreds) PublicKey() ed25519.PublicKey { return w.pub }

// Signer builds an in-process gcapi.Signer over the minted private key -- the L1
// path, where intake's dispatcher signs without touching the file. It is exactly
// what gcapi.LoadSigner(KeyFile(), KID, opts...) yields at L2/L3, minus the file
// round-trip.
func (w *WriteAuthCreds) Signer(opts ...gcapi.SignerOption) (*gcapi.Signer, error) {
	return gcapi.NewSigner(w.priv, w.KID, opts...)
}
