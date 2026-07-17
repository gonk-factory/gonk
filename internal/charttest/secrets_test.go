//go:build chart

package charttest

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
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

// --- Task 4: the RBAC that makes the Kubernetes KeySink legal ---------------

// The KeySink's RBAC must be a NAMESPACED Role -- never a ClusterRole. A
// cluster-wide write grant on Secrets is a cluster-wide compromise.
func TestKeySinkRBACIsNamespaced(t *testing.T) {
	out := Render(t, Minimum()...)
	MustObject(t, out, "ServiceAccount", "gonk-meter")
	MustObject(t, out, "Role", "gonk-meter")
	MustObject(t, out, "RoleBinding", "gonk-meter")
	for _, o := range Objects(t, out) {
		if o.Kind == "ClusterRole" || o.Kind == "ClusterRoleBinding" {
			t.Fatalf("the chart grants CLUSTER-wide RBAC (%s/%s)", o.Kind, o.Metadata.Name)
		}
	}
}

// The Role is inert if nothing tells meter WHERE to write. A chart that renders
// perfect RBAC and never sets GONK_KEYSINK_NAMESPACE ships a meter that falls
// back to the MEMORY sink -- provisioning virtual keys that no pod can read, and
// leaving every project in `key-missing` (P03 AD-1).
func TestMeterIsPointedAtAKeySinkNamespace(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-meter")
	if !strings.Contains(d.Doc, "GONK_KEYSINK_NAMESPACE") {
		t.Fatal("meter has RBAC to write key Secrets but no namespace to write them in")
	}
	if !strings.Contains(d.Doc, "gonk-key-") {
		t.Error("GONK_KEYSINK_PREFIX is not set; the Role's Secret names and the sink's would drift")
	}
	if strings.Contains(d.Doc, "automountServiceAccountToken: false") {
		t.Fatal("meter cannot build an in-cluster client without its ServiceAccount token")
	}
}

// No meter -> no keysink RBAC. Rendering the Role/RoleBinding/ServiceAccount
// for a component the operator disabled would be dead weight at best and a
// stale write grant at worst. (intake.enabled must also be false here: a
// separate guard refuses intake without meter, since intake asks meter
// before dispatching every order.)
func TestKeySinkRBACGatedOnMeterEnabled(t *testing.T) {
	out := Render(t, append(Minimum(), "--set", "meter.enabled=false", "--set", "intake.enabled=false")...)
	NoObject(t, out, "ServiceAccount", "gonk-meter")
	NoObject(t, out, "Role", "gonk-meter")
	NoObject(t, out, "RoleBinding", "gonk-meter")
}

// The RoleBinding must bind gonk-meter's own ServiceAccount, in the namespace
// the Release (and GONK_KEYSINK_NAMESPACE) actually installs into.
func TestKeySinkRoleBindingTargetsMeterServiceAccount(t *testing.T) {
	rb := MustObject(t, Render(t, Minimum()...), "RoleBinding", "gonk-meter")
	roleRef, _ := rb.Raw["roleRef"].(map[string]any)
	if roleRef["kind"] != "Role" || roleRef["name"] != "gonk-meter" {
		t.Fatalf("RoleBinding roleRef = %+v, want Role/gonk-meter", roleRef)
	}
	subjects, _ := rb.Raw["subjects"].([]any)
	if len(subjects) != 1 {
		t.Fatalf("RoleBinding has %d subjects, want exactly 1", len(subjects))
	}
	subj, _ := subjects[0].(map[string]any)
	if subj["kind"] != "ServiceAccount" || subj["name"] != "gonk-meter" {
		t.Fatalf("RoleBinding subject = %+v, want ServiceAccount/gonk-meter", subj)
	}
	if subj["namespace"] != "gonk" { // Render() passes --namespace gonk
		t.Fatalf("RoleBinding subject namespace = %v, want the release namespace", subj["namespace"])
	}
}

// The Role must be LEAST PRIVILEGE: no wildcard resource or verb, and its
// secrets rule must live in the core ("") API group.
func TestKeySinkRoleIsLeastPrivilege(t *testing.T) {
	role := MustObject(t, Render(t, Minimum()...), "Role", "gonk-meter")
	rules := toSliceOfMaps(t, role.Raw["rules"])
	if len(rules) == 0 {
		t.Fatal("Role gonk-meter has no rules")
	}
	for _, rule := range rules {
		for _, res := range toStringSlice(rule["resources"]) {
			if res == "*" {
				t.Fatal("Role grants a wildcard resource; least privilege forbids this")
			}
		}
		for _, v := range toStringSlice(rule["verbs"]) {
			if v == "*" {
				t.Fatal("Role grants a wildcard verb; least privilege forbids this")
			}
		}
		groups := toStringSlice(rule["apiGroups"])
		if len(groups) != 1 || groups[0] != "" {
			t.Fatalf("Role rule is not scoped to the core API group: apiGroups = %v", groups)
		}
	}
}

// This is the test that ties the Role to reality: the verbs granted on
// Secrets must be EXACTLY the client-go calls internal/meter/keysink/k8s.go
// makes -- derived from the source itself, not copied by hand. A future
// keysink change that starts calling Get/List/Watch/Patch on Secrets fails
// THIS test until role-gonk-meter.yaml is updated to match, instead of
// failing silently as a 403 on a real cluster (which is what Plan 06 would
// otherwise be left to discover).
func TestKeySinkRoleVerbsMatchK8sSink(t *testing.T) {
	role := MustObject(t, Render(t, Minimum()...), "Role", "gonk-meter")
	rules := toSliceOfMaps(t, role.Raw["rules"])

	var roleVerbs []string
	var sawSecretsRule bool
	for _, rule := range rules {
		if !containsStr(toStringSlice(rule["resources"]), "secrets") {
			continue
		}
		sawSecretsRule = true
		roleVerbs = append(roleVerbs, toStringSlice(rule["verbs"])...)
	}
	if !sawSecretsRule {
		t.Fatal(`Role gonk-meter has no rule for resource "secrets"`)
	}
	sort.Strings(roleVerbs)

	want := keysinkSecretsVerbs(t)
	if !reflect.DeepEqual(roleVerbs, want) {
		t.Fatalf("Role gonk-meter grants verbs %v on secrets, but "+
			"internal/meter/keysink/k8s.go actually calls %v -- update "+
			"role-gonk-meter.yaml to match (add a verb it needs, or drop one "+
			"it does not)", roleVerbs, want)
	}
}

// secretsVerbCall matches a client-go call of the shape
// `<something>.Secrets(<ns>).<Verb>(ctx, ...)` -- the exact pattern k8s.go
// uses for Create/Update/Delete.
var secretsVerbCall = regexp.MustCompile(`\.Secrets\([^)]*\)\.(Get|List|Watch|Create|Update|UpdateStatus|Patch|Delete|DeleteCollection)\(`)

// keysinkSecretsVerbs reads internal/meter/keysink/k8s.go directly and returns
// the sorted, de-duplicated set of RBAC verbs its Secrets calls require. It is
// deliberately a source-level check, not a copy of today's verb list, so it
// keeps tracking the sink even as it changes.
func keysinkSecretsVerbs(t *testing.T) []string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(self), "..", "meter", "keysink", "k8s.go")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read keysink source %s: %v", src, err)
	}

	verbSet := map[string]bool{}
	for _, m := range secretsVerbCall.FindAllStringSubmatch(string(b), -1) {
		switch m[1] {
		case "Create":
			verbSet["create"] = true
		case "Update", "UpdateStatus":
			verbSet["update"] = true
		case "Delete", "DeleteCollection":
			verbSet["delete"] = true
		case "Get":
			verbSet["get"] = true
		case "List":
			verbSet["list"] = true
		case "Watch":
			verbSet["watch"] = true
		case "Patch":
			verbSet["patch"] = true
		}
	}
	if len(verbSet) == 0 {
		t.Fatalf("found no client-go Secrets(...) calls in %s; the regex or the path is stale", src)
	}
	var verbs []string
	for v := range verbSet {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	return verbs
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// toStringSlice converts a []any of strings (as decoded from YAML into
// map[string]any) into a []string.
func toStringSlice(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// toSliceOfMaps converts a []any of map[string]any (as decoded from YAML)
// into a []map[string]any.
func toSliceOfMaps(t *testing.T, v any) []map[string]any {
	t.Helper()
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("expected a mapping, got %T: %+v", it, it)
		}
		out = append(out, m)
	}
	return out
}
