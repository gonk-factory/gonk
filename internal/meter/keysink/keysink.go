// Package keysink is where a provisioned LiteLLM virtual key is put so that an
// agent pod can read it.
//
// The key is a live credential. It must never travel through the /decide
// response, the Gas City event bus, or a log line -- meter returns only a
// KeyRef (a pointer to where the key lives), never the key itself.
package keysink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
)

type KeySink interface {
	// Put stores the token and returns a reference to it. It must be idempotent.
	Put(ctx context.Context, project, token string) (store.KeyRef, error)
	Delete(ctx context.Context, project string) error
}

// Memory is a KeySink for tests. Tokens live only in the process.
type Memory struct {
	mu     sync.Mutex
	tokens map[string]string
}

func NewMemory() *Memory { return &Memory{tokens: map[string]string{}} }

func (m *Memory) Put(_ context.Context, project, token string) (store.KeyRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[project] = token
	return store.KeyRef{SecretName: "gonk-key-" + Slug(project), SecretKey: "LITELLM_API_KEY"}, nil
}

func (m *Memory) Delete(_ context.Context, project string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, project)
	return nil
}

// Token is for tests only: it is the assertion that a key was actually stored.
func (m *Memory) Token(project string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens[project]
}

// Slug turns a GitLab path into a DNS-1123 name fragment, with a hash suffix.
//
// The suffix is NOT decoration. Flattening `/`, `.` and `_` to `-` collides:
// projects "a/b" and "a-b" both flatten to "a-b". They would then share a Secret
// name AND a LiteLLM key alias -- and since EnsureKey is idempotent BY ALIAS,
// the second project would silently adopt the first project's virtual key, and
// with it the first project's max_budget. Two projects sharing one hard USD
// ceiling is the exact failure this whole package exists to prevent.
//
// tagmint has already guaranteed the input is [A-Za-z0-9._/-] and <= 200 bytes.
func Slug(project string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(project) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default: // /, ., _, - all collapse to a single -
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	sum := sha256.Sum256([]byte(project)) // the ORIGINAL path, so a/b != a-b
	return name + "-" + hex.EncodeToString(sum[:4])
}

var _ KeySink = (*Memory)(nil)
