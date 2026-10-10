package ghook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	secretA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 36 chars
	secretB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// req builds a webhook request carrying token in X-Gitlab-Token. An empty token
// sets no header at all (the "missing" case), so one helper serves every test.
func req(token string) *http.Request {
	r := httptest.NewRequest("POST", "/hook/gitlab", nil)
	if token != "" {
		r.Header.Set("X-Gitlab-Token", token)
	}
	return r
}

func TestVerifierAcceptsEitherRotationSlot(t *testing.T) {
	v, err := NewVerifier(secretA, secretB)
	if err != nil {
		t.Fatalf("NewVerifier = %v", err)
	}
	for _, tok := range []string{secretA, secretB} {
		if err := v.Verify(req(tok)); err != nil {
			t.Errorf("Verify(%q…) = %v, want nil", tok[:4], err)
		}
	}
}

func TestVerifierRejects(t *testing.T) {
	v, _ := NewVerifier(secretA)
	cases := map[string]string{
		"missing":   "",
		"wrong":     strings.Repeat("c", 36),
		"prefix":    secretA[:35],
		"suffix":    secretA + "x",
		"empty-ish": " ",
	}
	for name, tok := range cases {
		if err := v.Verify(req(tok)); err == nil {
			t.Errorf("%s: Verify accepted", name)
		}
	}
}

func TestNewVerifierRefusesWeakConfig(t *testing.T) {
	if _, err := NewVerifier(); err == nil {
		t.Error("no secrets configured must be an error, not an open door")
	}
	if _, err := NewVerifier(""); err == nil {
		t.Error("empty-only secret must be an error")
	}
	if _, err := NewVerifier("short"); err == nil {
		t.Error("short secret must be rejected")
	}
	// An unset *second* slot is fine: rotation is usually one-slot-populated.
	if _, err := NewVerifier(secretA, ""); err != nil {
		t.Errorf("empty second slot should be allowed: %v", err)
	}
}

// Errors and logs are the classic leak path for a shared secret.
func TestErrorsDoNotContainSecrets(t *testing.T) {
	v, _ := NewVerifier(secretA)
	err := v.Verify(req(secretB))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), secretA) || strings.Contains(err.Error(), secretB) {
		t.Fatalf("error leaks secret material: %v", err)
	}
}
