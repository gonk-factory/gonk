package main

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func readerWith(objs ...*corev1.Secret) *secretReader {
	cs := fake.NewSimpleClientset()
	for _, o := range objs {
		_, _ = cs.CoreV1().Secrets(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{})
	}
	return &secretReader{cs: cs, namespace: "gonk"}
}

func secret(name, key, val string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gonk"},
		Data:       map[string][]byte{key: []byte(val)},
	}
}

func TestResolveReturnsTheProjectKey(t *testing.T) {
	r := readerWith(secret("gonk-key-proj", "LITELLM_API_KEY", "sk-project-key"))
	got, err := resolveLiteLLMKey(context.Background(), r, "gonk-key-proj", "LITELLM_API_KEY")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "sk-project-key" {
		t.Fatalf("got %q, want the project key", got)
	}
}

// THE PROPERTY THAT MAKES THIS SAFE. Every failure must stop the dispatch, never
// fall back to the controller's own key. The fallback is what caused gonk-8gb:
// an agent authenticating as the proxy admin looks exactly like a working
// session, so the failure would be silent and permanent, and the budget ceiling
// would go on not applying with nothing to show for it.
func TestEveryFailureFailsClosed(t *testing.T) {
	full := readerWith(secret("gonk-key-proj", "LITELLM_API_KEY", "sk-project-key"))
	cases := []struct {
		name, secretName, secretKey string
		r                           *secretReader
	}{
		{"no key reference at all", "", "", full},
		{"reference missing the key name", "gonk-key-proj", "", full},
		{"reference missing the secret name", "", "LITELLM_API_KEY", full},
		{"secret does not exist", "gonk-key-absent", "LITELLM_API_KEY", full},
		{"secret lacks the named key", "gonk-key-proj", "WRONG_KEY", full},
		{"secret value is empty", "gonk-key-empty", "LITELLM_API_KEY",
			readerWith(secret("gonk-key-empty", "LITELLM_API_KEY", "   "))},
		{"no cluster access at all", "gonk-key-proj", "LITELLM_API_KEY", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveLiteLLMKey(context.Background(), c.r, c.secretName, c.secretKey)
			if err == nil {
				t.Fatalf("returned a key (%q) instead of failing closed", got)
			}
			if got != "" {
				t.Fatalf("returned %q alongside an error; a partial result here would be used", got)
			}
		})
	}
}

// A key written with a trailing newline is a common and silent cause of 401s
// that read as a wrong credential rather than a wrong byte.
func TestTrailingWhitespaceIsTrimmed(t *testing.T) {
	r := readerWith(secret("gonk-key-nl", "LITELLM_API_KEY", "sk-key-with-newline\n"))
	got, err := resolveLiteLLMKey(context.Background(), r, "gonk-key-nl", "LITELLM_API_KEY")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "sk-key-with-newline" || strings.ContainsAny(got, "\n\r ") {
		t.Fatalf("got %q, want it trimmed", got)
	}
}

// The error must name what is wrong without ever echoing the credential.
func TestErrorsNeverContainAKey(t *testing.T) {
	r := readerWith(secret("gonk-key-proj", "LITELLM_API_KEY", "sk-super-secret-value"))
	_, err := resolveLiteLLMKey(context.Background(), r, "gonk-key-proj", "NOPE")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sk-super-secret-value") {
		t.Fatalf("error leaked the key: %v", err)
	}
}
