package keysink

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestK8sPutCreatesASecret(t *testing.T) {
	cs := fakeclient.NewSimpleClientset()
	k := NewK8s(cs, "gonk", "gonk-key-")
	ctx := context.Background()

	ref, err := k.Put(ctx, "group/repo", "sk-token")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := cs.CoreV1().Secrets("gonk").Get(ctx, ref.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Fatalf("secret type = %v, want Opaque", sec.Type)
	}
	if string(sec.Data[SecretKey]) != "sk-token" {
		t.Fatalf("secret data[%s] = %q, want sk-token", SecretKey, sec.Data[SecretKey])
	}
	if ref.SecretKey != SecretKey {
		t.Fatalf("KeyRef.SecretKey = %q, want %q", ref.SecretKey, SecretKey)
	}
	if sec.Labels["gonk.orac.local/project"] == "" {
		t.Fatal("secret has no gonk.orac.local/project label -- a human could not find it")
	}
}

// Intake re-registers every project every 10 minutes; a non-idempotent Put is
// a Secret storm, and a rotation must overwrite rather than duplicate.
func TestK8sPutIsIdempotentAndUpdates(t *testing.T) {
	cs := fakeclient.NewSimpleClientset()
	k := NewK8s(cs, "gonk", "gonk-key-")
	ctx := context.Background()

	ref1, err := k.Put(ctx, "group/repo", "sk-old")
	if err != nil {
		t.Fatal(err)
	}
	ref2, err := k.Put(ctx, "group/repo", "sk-new")
	if err != nil {
		t.Fatal(err)
	}
	if ref1 != ref2 {
		t.Fatalf("SecretName changed across Put calls: %+v vs %+v", ref1, ref2)
	}

	list, err := cs.CoreV1().Secrets("gonk").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d secrets, want exactly 1 (a non-idempotent Put is a Secret storm)", len(list.Items))
	}
	if string(list.Items[0].Data[SecretKey]) != "sk-new" {
		t.Fatalf("secret data = %q, want the new token sk-new", list.Items[0].Data[SecretKey])
	}
}

func TestK8sDeleteIsIdempotent(t *testing.T) {
	cs := fakeclient.NewSimpleClientset()
	k := NewK8s(cs, "gonk", "gonk-key-")
	ctx := context.Background()

	if err := k.Delete(ctx, "group/repo"); err != nil {
		t.Fatalf("deleting an absent key errored: %v", err)
	}
	if _, err := k.Put(ctx, "group/repo", "sk-token"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(ctx, "group/repo"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(ctx, "group/repo"); err != nil {
		t.Fatalf("second delete of an absent key errored: %v", err)
	}
}

func TestK8sHandlesHostileProjectPaths(t *testing.T) {
	cs := fakeclient.NewSimpleClientset()
	k := NewK8s(cs, "gonk", "gonk-key-")
	ctx := context.Background()

	for _, project := range []string{
		"Group/Repo",
		strings.Repeat("seg/", 60) + "leaf",
		"group/repo.with.dots",
		"group/_leading-underscore",
	} {
		ref, err := k.Put(ctx, project, "sk-token")
		if err != nil {
			t.Fatalf("Put(%q): %v", project, err)
		}
		if len(ref.SecretName) > 63 {
			t.Fatalf("Put(%q) secret name = %q, len %d > 63", project, ref.SecretName, len(ref.SecretName))
		}
		if !dns1123Subdomain.MatchString(ref.SecretName) {
			t.Fatalf("Put(%q) secret name = %q, not a legal DNS-1123 subdomain", project, ref.SecretName)
		}
		if _, err := cs.CoreV1().Secrets("gonk").Get(ctx, ref.SecretName, metav1.GetOptions{}); err != nil {
			t.Fatalf("secret for %q was not stored under %q: %v", project, ref.SecretName, err)
		}
	}
}

func TestK8sPutSurfacesErrorsWithoutLeakingTheToken(t *testing.T) {
	cs := fakeclient.NewSimpleClientset()
	// Force Get to fail is hard with the fake clientset's basic reactor chain;
	// instead assert the create/update error-wrapping never formats the token
	// by checking a duplicate-namespace-mismatch style failure surfaces the
	// underlying API error text (not the token) when Create fails for a reason
	// other than AlreadyExists. A prepended reactor injects that failure.
	cs.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "secrets"}, "gonk-key-x", errors.New("no"))
	})
	k := NewK8s(cs, "gonk", "gonk-key-")
	_, err := k.Put(context.Background(), "group/repo", "sk-super-secret-token")
	if err == nil {
		t.Fatal("expected an error from a forbidden create")
	}
	if strings.Contains(err.Error(), "sk-super-secret-token") {
		t.Fatalf("the token leaked into the error: %v", err)
	}
}
