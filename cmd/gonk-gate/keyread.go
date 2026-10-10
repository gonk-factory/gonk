package main

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// secretReader reads one key out of one Kubernetes Secret.
//
// IT EXISTS TO STOP AGENTS HOLDING THE PROXY ADMIN KEY (gonk-8gb). The meter
// provisions a per-project LiteLLM virtual key with a real max_budget and
// keysinks it to a Secret; dispatch already carries a POINTER to that Secret in
// key_secret_name/key_secret_key. What was missing was anything that followed
// the pointer, so `litellm_key` forwarded the controller's own mounted key
// instead -- and that file is the ADMIN key. Every session therefore
// authenticated to LiteLLM as the proxy administrator, on a key with no alias
// and no budget, which is why the per-project ceiling was never consulted.
//
// Reading here rather than in the pod is deliberate: the agent has no
// Kubernetes access and must not get any. The controller already holds a
// strictly MORE powerful credential, so letting it read the project key is a
// reduction in total privilege, not an increase -- it is what lets the admin key
// stop leaving this pod.
type secretReader struct {
	cs        kubernetes.Interface
	namespace string
}

// newSecretReader builds a reader from the in-cluster service account. It
// returns nil (not an error) when there is no cluster config, so a local or test
// invocation of gonk-gate keeps working -- callers must handle a nil reader.
func newSecretReader(namespace string) *secretReader {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil
	}
	return &secretReader{cs: cs, namespace: namespace}
}

// read returns the value at key in the named Secret.
func (r *secretReader) read(ctx context.Context, name, key string) (string, error) {
	if r == nil || r.cs == nil {
		return "", fmt.Errorf("keyread: no cluster access")
	}
	if name == "" || key == "" {
		return "", fmt.Errorf("keyread: incomplete secret reference")
	}
	sec, err := r.cs.CoreV1().Secrets(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("keyread: get secret %s/%s: %w", r.namespace, name, err)
	}
	raw, ok := sec.Data[key]
	if !ok {
		return "", fmt.Errorf("keyread: secret %s/%s has no key %q", r.namespace, name, key)
	}
	// Trim: a key written with a trailing newline is a common and silent cause of
	// 401s that look like a wrong credential rather than a wrong byte.
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", fmt.Errorf("keyread: secret %s/%s key %q is empty", r.namespace, name, key)
	}
	return v, nil
}

// resolveLiteLLMKey returns the key an agent session should authenticate with.
//
// THE PROJECT'S VIRTUAL KEY, OR NOTHING. It follows the KeyRef the meter already
// returns, and it FAILS CLOSED: if the pointer is present but the Secret cannot
// be read, dispatch stops rather than falling back to the controller's mounted
// key. The fallback is what caused gonk-8gb -- an agent authenticating as the
// proxy admin on a key with no budget looks exactly like a working session, so
// the failure would be silent and permanent, and the budget ceiling would go on
// not applying with nothing to show for it.
//
// A run that cannot be metered must not run. That is the same fail-closed rule
// the rest of the meter path already follows.
func resolveLiteLLMKey(ctx context.Context, r *secretReader, secretName, secretKey string) (string, error) {
	if secretName == "" || secretKey == "" {
		// The meter returned no key reference at all. That is a meter-side
		// problem (an unregistered or key-missing project), and the decision
		// gate upstream should already have refused; say so plainly rather than
		// substituting a credential of our own.
		return "", fmt.Errorf("no LiteLLM key reference for this project; refusing to dispatch unmetered")
	}
	key, err := r.read(ctx, secretName, secretKey)
	if err != nil {
		return "", fmt.Errorf("could not read this project's LiteLLM key: %w", err)
	}
	return key, nil
}
