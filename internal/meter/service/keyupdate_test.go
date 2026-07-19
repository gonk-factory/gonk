package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/keysink"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
)

// tokenOnceAdmin models the REAL litellm.HTTPAdmin contract that gonk-huy
// uncovered: a create returns the plaintext token exactly once; an
// update-in-place (an alias it has already created) returns an EMPTY Token,
// because a real proxy never reveals a key's plaintext again. RotateKey mints a
// fresh plaintext. The in-memory litellm.Fake cannot express this (it
// "remembers" the plaintext and returns it on every call), so the service's
// empty-token handling needs this faithful stub to be exercised at all.
type tokenOnceAdmin struct {
	mu          sync.Mutex
	created     map[string]bool
	createToken string
	rotateToken string
	rotateCalls int
}

func (a *tokenOnceAdmin) EnsureKey(_ context.Context, spec litellm.KeySpec) (litellm.KeyInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.created[spec.Alias] {
		return litellm.KeyInfo{Alias: spec.Alias, Token: ""}, nil // update path: plaintext unrecoverable
	}
	a.created[spec.Alias] = true
	return litellm.KeyInfo{Alias: spec.Alias, Token: a.createToken}, nil
}

func (a *tokenOnceAdmin) RotateKey(_ context.Context, spec litellm.KeySpec) (litellm.KeyInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rotateCalls++
	a.created[spec.Alias] = true
	return litellm.KeyInfo{Alias: spec.Alias, Token: a.rotateToken}, nil
}

func (a *tokenOnceAdmin) DeleteKey(_ context.Context, alias string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.created, alias)
	return nil
}

var _ litellm.Admin = (*tokenOnceAdmin)(nil)

func regReq(project, rig, yaml string) meterapi.ProjectRequest {
	return meterapi.ProjectRequest{Project: project, ProjectID: 1, Rig: rig, GonkYML: yaml}
}

func newKeyUpdateService(t *testing.T, admin litellm.Admin, ks keysink.KeySink, st store.Store) *Service {
	t.Helper()
	cfg, err := opercfg.Load([]byte(`
version: 1
instance:
  enabled: true
  ladder: [qwen-local, glm]
  budget: { monthly_cost_usd: 200 }
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0, est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
  - { name: glm,        kind: cloud, model: glm-5,           est_cost_usd: 0.40, est_tokens: "200K" }
meter: { max_spend_staleness: 5m, reservation_ttl: 60m }
`))
	if err != nil {
		t.Fatalf("opercfg.Load: %v", err)
	}
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	return New(cfg, st, admin, litellm.NewFake(), ks, func() time.Time { return now })
}

// A re-register that raises the budget hits EnsureKey's update path (empty
// token). The service MUST preserve the plaintext the KeySink already holds --
// overwriting it with the empty token would hand the project a dead credential,
// the exact end-to-end failure gonk-huy's Bug B causes. No rotate should happen:
// there was a stored token to keep.
func TestReRegisterPreservesTokenOnEmptyUpdate(t *testing.T) {
	admin := &tokenOnceAdmin{created: map[string]bool{}, createToken: "sk-create-1", rotateToken: "sk-rotate-1"}
	ks := keysink.NewMemory()
	svc := newKeyUpdateService(t, admin, ks, store.NewMemory())

	if _, status, err := svc.Register(context.Background(), regReq("group/repo", "group-repo", simpleYAML("qwen-local, glm", 10))); err != nil || status != 200 {
		t.Fatalf("first register: status=%d err=%v", status, err)
	}
	if got := ks.Token("group/repo"); got != "sk-create-1" {
		t.Fatalf("after create, keysink token = %q, want sk-create-1", got)
	}

	// Re-register with a higher ceiling -> EnsureKey update path -> empty token.
	if _, status, err := svc.Register(context.Background(), regReq("group/repo", "group-repo", simpleYAML("qwen-local, glm", 25))); err != nil || status != 200 {
		t.Fatalf("re-register: status=%d err=%v", status, err)
	}
	if got := ks.Token("group/repo"); got != "sk-create-1" {
		t.Fatalf("after update, keysink token = %q, want the PRESERVED sk-create-1 (empty must not clobber it)", got)
	}
	if admin.rotateCalls != 0 {
		t.Fatalf("rotateCalls = %d, want 0 -- a stored token was available to preserve", admin.rotateCalls)
	}
}

// If EnsureKey takes the update path but nothing is stored to preserve (the
// alias exists in LiteLLM but the plaintext was lost -- recovering state), the
// service rotates to mint a fresh usable token rather than storing nothing.
func TestReRegisterRotatesWhenNoStoredTokenToPreserve(t *testing.T) {
	// Pre-seed the alias as already-created so the FIRST EnsureKey call returns
	// the empty update-path token, with an empty KeySink and no prior KeyRef.
	admin := &tokenOnceAdmin{created: map[string]bool{"gonk-group-repo": true}, createToken: "sk-create-1", rotateToken: "sk-rotate-1"}
	ks := keysink.NewMemory()
	svc := newKeyUpdateService(t, admin, ks, store.NewMemory())

	if _, status, err := svc.Register(context.Background(), regReq("group/repo", "group-repo", simpleYAML("qwen-local, glm", 10))); err != nil || status != 200 {
		t.Fatalf("register: status=%d err=%v", status, err)
	}
	if got := ks.Token("group/repo"); got != "sk-rotate-1" {
		t.Fatalf("keysink token = %q, want sk-rotate-1 (rotate self-heals a lost plaintext)", got)
	}
	if admin.rotateCalls != 1 {
		t.Fatalf("rotateCalls = %d, want 1", admin.rotateCalls)
	}
}
