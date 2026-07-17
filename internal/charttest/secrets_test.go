//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// G14: the chart renders NO Secret, ever. Secrets come from Vault/1Password via
// an ExternalSecret, outside this chart.
func TestChartRendersNoSecretObjects(t *testing.T) {
	for _, o := range Objects(t, Render(t, Minimum()...)) {
		if o.Kind == "Secret" {
			t.Fatalf("the chart rendered a Secret (%s). Secrets are provisioned OUTSIDE the chart.", o.Metadata.Name)
		}
	}
}

// G14: and no credential-shaped string leaks into a manifest.
func TestNoSecretMaterialInRender(t *testing.T) {
	out := Render(t, Minimum()...)
	for _, bad := range []string{"glpat-", "sk-", "-----BEGIN", "PRIVATE KEY"} {
		if strings.Contains(out, bad) {
			t.Fatalf("the render contains %q", bad)
		}
	}
}

// THE RULE: secrets are FILES. Never envFrom, never secretKeyRef.
func TestSecretsAreNeverEnvironmentVariables(t *testing.T) {
	out := Render(t, Minimum()...)
	for _, o := range Objects(t, out) {
		if o.Kind != "Deployment" && o.Kind != "StatefulSet" {
			continue
		}
		for _, bad := range []string{"secretKeyRef", "envFrom"} {
			if strings.Contains(o.Doc, bad) {
				t.Fatalf("%s/%s uses %s. Secrets are FILE MOUNTS: the environment leaks into "+
					"/proc/<pid>/environ, into crash dumps, and into every child process.",
					o.Kind, o.Metadata.Name, bad)
			}
		}
	}
}

// The provisioning contract, asserted: what key, in what Secret, at what path,
// in what env var. If Plans 02/03 rename one of these, THIS is what catches it.
func TestMeterSecretContract(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-meter")
	for _, want := range []string{
		"LITELLM_ADMIN_KEY_FILE",
		"/etc/gonk/secrets/litellm/admin-key",
		"GONK_METER_TOKEN_FILE",
		"/etc/gonk/secrets/meter-api/token",
		"GONK_METER_STORE_DSN_FILE",
		"/etc/gonk/secrets/ledger/dsn",
		"GONK_METER_STORE_BACKEND",
		"--operator-config",
		"defaultMode: 256", // 0400
	} {
		if !strings.Contains(d.Doc, want) {
			t.Errorf("gonk-meter deployment is missing %q", want)
		}
	}
}

// The design table (this plan, "Private CA everywhere") mounts trust-bundle
// for "intake (and meter, for symmetry)". Meter's own env contract does not
// name SSL_CERT_FILE (the image's own ENV already sets it -- Task 0
// reconciliation), but the file that path names must actually exist, or a
// future https LITELLM_URL / DSN sslmode=verify-full would fail closed for
// the wrong reason (a missing trust bundle, not a policy decision).
func TestMeterTrustBundleMountedForSymmetry(t *testing.T) {
	out := Render(t, append(Minimum(), "--set", "gitlab.caCert.existingConfigMap=gitlab-ca")...)
	d := MustObject(t, out, "Deployment", "gonk-meter")
	for _, want := range []string{"SSL_CERT_FILE", "gitlab-ca", "/etc/ssl/orac/ca.crt"} {
		if !strings.Contains(d.Doc, want) {
			t.Errorf("gonk-meter's trust bundle is not wired: missing %q", want)
		}
	}
}

func TestIntakeSecretContract(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-intake")
	for _, want := range []string{
		"GONK_GITLAB_TOKEN_FILE", "/etc/gonk/secrets/gitlab/token",
		"GONK_WEBHOOK_SECRET_FILE", "/etc/gonk/secrets/webhook/token",
		"GONK_METER_TOKEN_FILE", "/etc/gonk/secrets/meter-api/token",
	} {
		if !strings.Contains(d.Doc, want) {
			t.Errorf("intake deployment is missing %q", want)
		}
	}
	// The admin token is only mounted in split-credential mode.
	if strings.Contains(d.Doc, "GONK_GITLAB_ADMIN_TOKEN_FILE") {
		t.Error("the admin token is mounted in bot-does-everything mode; that Secret key does not exist there and the pod would not start")
	}
}

func TestSplitCredentialMountsTheAdminToken(t *testing.T) {
	out := Render(t, append(Minimum(), "--set", "gitlab.mode=split-credential")...)
	d := MustObject(t, out, "Deployment", "gonk-intake")
	if !strings.Contains(d.Doc, "GONK_GITLAB_ADMIN_TOKEN_FILE") {
		t.Fatal("split-credential mode did not mount the admin token")
	}
}

// Rotation slot 2 is OPT-IN: a projected volume with a key that does not exist
// in the Secret makes the pod fail to start.
func TestRotationSlotTwoIsOptIn(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-intake")
	if strings.Contains(d.Doc, "GONK_WEBHOOK_SECRET_PREVIOUS_FILE") {
		t.Fatal("slot 2 is mounted by default; the key usually does not exist and the pod would not start")
	}
	out := Render(t, append(Minimum(), "--set", "secrets.rotation.webhookPrevious=true")...)
	d2 := MustObject(t, out, "Deployment", "gonk-intake")
	if !strings.Contains(d2.Doc, "GONK_WEBHOOK_SECRET_PREVIOUS_FILE") {
		t.Fatal("opting in to slot 2 did not mount it")
	}
}

// AD-5, RECONCILED (Task 0): replicas: 1 + Recreate are the chart's DEFAULT
// for the modest footprint of the initial deployment -- NOT a correctness
// requirement any more. ADR-004 records that meter's ReserveIfFits atomicity
// is enforced in Postgres, across replicas (TestReserveIfFitsRace /
// TestReserveIsIdempotentAcrossReplicas, internal/meter/store), so this test
// asserts the DEFAULT profile's shape, not a guard the chart enforces --
// raising meter.replicaCount renders cleanly (see
// TestMeterMultipleReplicasRendersWithNoGuard, Task 2).
func TestMeterDefaultsToSingleReplicaAndRecreate(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-meter")
	if !strings.Contains(d.Doc, "replicas: 1") {
		t.Error("gonk-meter's default replicaCount is not 1")
	}
	if !strings.Contains(d.Doc, "type: Recreate") {
		t.Error("gonk-meter's default strategy is not Recreate")
	}
}
