//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// R-48 (T-26, INTERIM until T-56 deletes Dolt entirely at the Gas City
// cutover): the bundled Dolt used to be root@%, no password, no TLS -- any
// pod that could reach gonk-dolt could `mysql -h gonk-dolt -u root` and
// rewrite the beads store. This asserts the three load-bearing pieces of the
// fix are actually rendered, not merely that the StatefulSet exists:
//   - root is pinned to localhost (unreachable cross-pod)
//   - the `gc` user (not root) is what gets created, scoped to ONLY the
//     beads database, with its password sourced from an existingSecret
//   - the controller connects AS that `gc` user, never root
func TestDoltIsNotRootOpen(t *testing.T) {
	out := Render(t, Minimum()...)

	dolt := MustObject(t, out, "StatefulSet", "gonk-dolt")

	// Root is bound to localhost, not the old root-open "%".
	if strings.Contains(dolt.Doc, `DOLT_ROOT_HOST`) && strings.Contains(dolt.Doc, `value: "%"`) {
		t.Error("gonk-dolt StatefulSet still sets DOLT_ROOT_HOST to \"%\" -- root would be reachable cross-pod (R-48)")
	}
	if !strings.Contains(dolt.Doc, "DOLT_ROOT_HOST") || !strings.Contains(dolt.Doc, "localhost") {
		t.Error("gonk-dolt StatefulSet does not pin DOLT_ROOT_HOST to localhost")
	}

	// The gc user, scoped to ONLY the beads database (bd_gonk in Minimum()'s
	// default dolt.database=gonk), reachable cross-pod via DOLT_USER_HOST=%.
	for _, want := range []string{
		`DOLT_USER`,
		`DOLT_USER_HOST`,
		`DOLT_DATABASE`,
		`value: gc`,      // DOLT_USER's value, round-tripped through yaml.v3 (unquoted plain scalar)
		`value: bd_gonk`, // DOLT_DATABASE's value
		`export DOLT_ROOT_PASSWORD="$(cat /etc/gonk/secrets/dolt/root-password)"`,
		`export DOLT_PASSWORD="$(cat /etc/gonk/secrets/dolt/gc-password)"`,
	} {
		if !strings.Contains(dolt.Doc, want) {
			t.Errorf("gonk-dolt StatefulSet is missing %q:\n%s", want, dolt.Doc)
		}
	}

	// The root/gc password Secret is the operator-provisioned existingSecret,
	// never a chart literal -- it must be rendered as a projected Secret
	// volume reference, not a value.
	if !strings.Contains(dolt.Doc, "name: gonk-dolt") {
		t.Errorf("gonk-dolt StatefulSet does not reference the secrets.dolt.existingSecret Secret (gonk-dolt):\n%s", dolt.Doc)
	}
	if !strings.Contains(dolt.Doc, "root-password") || !strings.Contains(dolt.Doc, "gc-password") {
		t.Errorf("gonk-dolt StatefulSet does not project both root-password and gc-password keys:\n%s", dolt.Doc)
	}

	// No secretKeyRef/envFrom -- covered chart-wide by
	// TestSecretsAreNeverEnvironmentVariables, but this is the one place a
	// literal password could sneak in as a plain env `value:` instead, so
	// check directly that no plausible secret literal appears.
	for _, bad := range []string{"DOLT_ROOT_PASSWORD\n  value:", "DOLT_PASSWORD\n  value:", "secretKeyRef"} {
		if strings.Contains(dolt.Doc, bad) {
			t.Errorf("gonk-dolt StatefulSet contains %q -- a password must never be a literal env value", bad)
		}
	}

	// The controller connects to Dolt as gc, not root.
	ctrl := MustObject(t, out, "Deployment", "gonk-controller")
	if !strings.Contains(ctrl.Doc, "GC_DOLT_USER") {
		t.Fatal("gonk-controller does not set GC_DOLT_USER")
	}
	if !strings.Contains(ctrl.Doc, "value: gc") {
		t.Errorf("gonk-controller's GC_DOLT_USER is not \"gc\":\n%s", ctrl.Doc)
	}
	if strings.Contains(ctrl.Doc, `value: root`) {
		t.Error("gonk-controller connects to Dolt as root")
	}
	if !strings.Contains(ctrl.Doc, `export GC_DOLT_PASSWORD="$(cat /etc/gonk/secrets/dolt-gc/gc-password)"`) {
		t.Errorf("gonk-controller does not read GC_DOLT_PASSWORD from the FILE-mounted gc-password Secret key:\n%s", ctrl.Doc)
	}
}

// An empty secrets.dolt.existingSecret must fail closed -- the chart creates
// and defaults NO Dolt password, root or gc.
func TestDoltSecretGuardFailsClosed(t *testing.T) {
	_, err := RenderErr(t, append(Minimum(), "--set", "secrets.dolt.existingSecret=")...)
	if err == nil {
		t.Fatal("rendering with secrets.dolt.existingSecret=\"\" succeeded; it must fail closed")
	}
}
