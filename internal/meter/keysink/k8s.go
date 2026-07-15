package keysink

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
)

// SecretKey is the key inside the per-project Secret. The agent image reads it as
// its LiteLLM virtual key; the pack (Plan 04) finds it through the KeyRef meter
// returns. *** CHANGING THIS STRING BREAKS THE AGENT IMAGE. *** It is also the
// literal the chart hard-codes (Plan 05) and Plan 06 asserts.
const SecretKey = "LITELLM_API_KEY"

// K8s writes each project's LiteLLM virtual key into its own Kubernetes Secret.
//
// This is the delivery mechanism for the whole attribution chain: an agent pod
// gets a key scoped to ONE project with ONE budget, so LiteLLM's hard refusal
// lands on the right project. The key material NEVER travels through meter's HTTP
// responses, the Gas City event bus, or a log line -- meter hands out only a
// KeyRef, and this is what the ref points at.
//
// One Secret per project (not one Secret with N keys) so that RBAC can later
// scope an agent pod to exactly its own key.
type K8s struct {
	cs        kubernetes.Interface
	namespace string
	prefix    string
}

func NewK8s(cs kubernetes.Interface, namespace, prefix string) *K8s {
	return &K8s{cs: cs, namespace: namespace, prefix: prefix}
}

func (k *K8s) name(project string) string { return k.prefix + Slug(project) }

// Put is idempotent: intake re-registers every project on every reconcile pass
// (every 10 minutes), and a rotation must overwrite rather than duplicate.
func (k *K8s) Put(ctx context.Context, project, token string) (store.KeyRef, error) {
	name := k.name(project)
	ref := store.KeyRef{SecretName: name, SecretKey: SecretKey}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gonk-meter",
				"app.kubernetes.io/part-of":    "gonk",
				"gonk.orac.local/project":      Slug(project),
			},
			Annotations: map[string]string{
				// Slug is lossy; keep the real path for a human and for meter's own
				// reconcile. The PATH is not a secret; the token is.
				"gonk.orac.local/project-path": project,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{SecretKey: []byte(token)},
	}

	_, err := k.cs.CoreV1().Secrets(k.namespace).Create(ctx, sec, metav1.CreateOptions{})
	if err == nil {
		return ref, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		// NEVER wrap the token into an error: errors get logged.
		return store.KeyRef{}, fmt.Errorf("keysink: create secret %s/%s: %w", k.namespace, name, err)
	}
	if _, err := k.cs.CoreV1().Secrets(k.namespace).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return store.KeyRef{}, fmt.Errorf("keysink: update secret %s/%s: %w", k.namespace, name, err)
	}
	return ref, nil
}

// Delete removes a project's key. De-onboarding must leave no live credential
// behind. Deleting an absent key is a no-op.
func (k *K8s) Delete(ctx context.Context, project string) error {
	err := k.cs.CoreV1().Secrets(k.namespace).Delete(ctx, k.name(project), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("keysink: delete secret %s/%s: %w", k.namespace, k.name(project), err)
	}
	return nil
}

var _ KeySink = (*K8s)(nil)
