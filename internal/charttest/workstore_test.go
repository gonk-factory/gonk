//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// gonk-t41 RELEASE 2 (expand then contract): the chart mounts the work store's
// DSN -- Secret gonk-workstore, role gonk_workstore, NOT the meter's
// gonk-ledger -- into BOTH controller containers as a FILE, and runs the bd
// import in bootstrap-city before gc starts.

func controllerContainers(t *testing.T, o Object) map[string]map[string]any {
	t.Helper()
	spec, _ := o.Raw["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pspec, _ := tmpl["spec"].(map[string]any)
	out := map[string]map[string]any{}
	for _, group := range []string{"containers", "initContainers"} {
		list, _ := pspec[group].([]any)
		for _, c := range list {
			cm, _ := c.(map[string]any)
			name, _ := cm["name"].(string)
			out[name] = cm
		}
	}
	out["__pod"] = pspec
	return out
}

func containerEnv(t *testing.T, c map[string]any) map[string]string {
	t.Helper()
	env := map[string]string{}
	envs, _ := c["env"].([]any)
	for _, e := range envs {
		em, _ := e.(map[string]any)
		if _, ref := em["valueFrom"]; ref {
			t.Errorf("env %v uses valueFrom; secrets are file mounts only", em["name"])
		}
		n, _ := em["name"].(string)
		v, _ := em["value"].(string)
		env[n] = v
	}
	return env
}

func mountsSecret(c map[string]any, name, path string) bool {
	mounts, _ := c["volumeMounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm["name"] == name && mm["mountPath"] == path && mm["readOnly"] == true {
			return true
		}
	}
	return false
}

func script(c map[string]any) string {
	var b strings.Builder
	for _, key := range []string{"command", "args"} {
		parts, _ := c[key].([]any)
		for _, p := range parts {
			s, _ := p.(string)
			b.WriteString(s + "\n")
		}
	}
	return b.String()
}

func TestControllerMountsTheWorkStoreDSN(t *testing.T) {
	ctrl := MustObject(t, Render(t, WithWorkStore(Minimum())...), "Deployment", "gonk-controller")
	cs := controllerContainers(t, ctrl)
	const path = "/etc/gonk/secrets/workstore/dsn"
	for _, name := range []string{"controller", "bootstrap-city"} {
		c, ok := cs[name]
		if !ok {
			t.Fatalf("no container %s", name)
		}
		env := containerEnv(t, c)
		if env["GONK_WORKSTORE_DSN_FILE"] != path {
			t.Errorf("%s: GONK_WORKSTORE_DSN_FILE = %q, want %q", name, env["GONK_WORKSTORE_DSN_FILE"], path)
		}
		if _, on := env["GONK_WORKSTORE_IMPORT_BREAK_GLASS"]; on {
			t.Errorf("%s: break-glass is on by default", name)
		}
		if !mountsSecret(c, "secret-workstore", "/etc/gonk/secrets/workstore") {
			t.Errorf("%s: secret-workstore is not mounted read-only at /etc/gonk/secrets/workstore", name)
		}
		// Isolation: the controller must never hold the meter's role.
		if mountsSecret(c, "secret-ledger", "/etc/gonk/secrets/ledger") || strings.Contains(env["GONK_WORKSTORE_DSN_FILE"], "ledger") {
			t.Errorf("%s: the controller mounts the meter's ledger Secret", name)
		}
	}
	vols, _ := cs["__pod"]["volumes"].([]any)
	projected := false
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if vm["name"] == "secret-ledger" {
			t.Error("the controller pod has a secret-ledger volume: it must not hold the meter's role")
		}
		if vm["name"] != "secret-workstore" {
			continue
		}
		proj, _ := vm["projected"].(map[string]any)
		sources, _ := proj["sources"].([]any)
		for _, src := range sources {
			sm, _ := src.(map[string]any)
			sec, _ := sm["secret"].(map[string]any)
			items, _ := sec["items"].([]any)
			if sec["name"] == "gonk-workstore" && len(items) == 1 {
				it, _ := items[0].(map[string]any)
				projected = it["key"] == "dsn" && it["path"] == "dsn"
			}
		}
	}
	if !projected {
		t.Error("volume secret-workstore does not project exactly key dsn of Secret gonk-workstore")
	}
}

// The import runs in bootstrap-city (before gc starts), only when the image's
// gonk-gate has it, and its failure is not swallowed.
func TestBootstrapImportsBdRecordsBeforeGcStarts(t *testing.T) {
	ctrl := MustObject(t, Render(t, WithWorkStore(Minimum())...), "Deployment", "gonk-controller")
	cs := controllerContainers(t, ctrl)
	boot, sup := script(cs["bootstrap-city"]), script(cs["controller"])
	for _, want := range []string{
		"if gonk-gate capabilities 2>/dev/null | grep -qx workstore-import; then",
		"gonk-gate workstore-import\n",
		`BEADS_DOLT_PASSWORD="$GC_DOLT_PASSWORD"`,
		`BEADS_DOLT_SERVER_USER="$GC_DOLT_USER"`,
		"BEADS_DIR=/city/.beads",
		"GONK_BEAD_REPO_DIR=/city \\\n",
	} {
		if !strings.Contains(boot, want) {
			t.Errorf("bootstrap-city is missing %q", want)
		}
	}
	if strings.Contains(boot, "gonk-gate workstore-import ||") || strings.Contains(boot, "gonk-gate workstore-import;") {
		t.Error("the import's failure is swallowed; it must fail the initContainer")
	}
	if !strings.Contains(boot, "set -eu") {
		t.Error("bootstrap-city does not run under set -eu")
	}
	if strings.Contains(sup, "workstore-import") {
		t.Error("the supervisor container runs the import; it must run before gc starts")
	}
	if strings.Index(boot, "gc init --preserve-existing") > strings.Index(boot, "gonk-gate workstore-import") {
		t.Error("the import runs before gc init has written .beads/metadata.json")
	}
}

// The import's stamp must be found by the supervisor: same directory, the
// pod-scoped /city emptyDir, in both containers.
func TestImportStampDirIsSharedBetweenBootstrapAndSupervisor(t *testing.T) {
	ctrl := MustObject(t, Render(t, WithWorkStore(Minimum())...), "Deployment", "gonk-controller")
	cs := controllerContainers(t, ctrl)
	for _, name := range []string{"bootstrap-city", "controller"} {
		if !mountsCity(cs[name]) {
			t.Errorf("%s does not mount the city volume at /city", name)
		}
	}
	if containerEnv(t, cs["controller"])["GONK_BEAD_REPO_DIR"] != "/city" {
		t.Error("supervisor GONK_BEAD_REPO_DIR is not /city")
	}
	vols, _ := cs["__pod"]["volumes"].([]any)
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if vm["name"] == "city" {
			if _, ok := vm["emptyDir"]; !ok {
				t.Error("volume city is not an emptyDir: a stamp would outlive the pod")
			}
		}
	}
}

func mountsCity(c map[string]any) bool {
	mounts, _ := c["volumeMounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm["name"] == "city" && mm["mountPath"] == "/city" {
			return true
		}
	}
	return false
}

// workstore.importBreakGlass sets the env on the bootstrap (where the import
// runs) and nowhere else.
func TestWorkStoreBreakGlassValue(t *testing.T) {
	ctrl := MustObject(t, Render(t, append(WithWorkStore(Minimum()), "--set", "workstore.importBreakGlass=true")...), "Deployment", "gonk-controller")
	cs := controllerContainers(t, ctrl)
	if containerEnv(t, cs["bootstrap-city"])["GONK_WORKSTORE_IMPORT_BREAK_GLASS"] != "1" {
		t.Error("importBreakGlass=true does not set GONK_WORKSTORE_IMPORT_BREAK_GLASS=1 on bootstrap-city")
	}
	if _, on := containerEnv(t, cs["controller"])["GONK_WORKSTORE_IMPORT_BREAK_GLASS"]; on {
		t.Error("break-glass env set on the supervisor; only the import reads it")
	}
}

// workstore.enabled=false renders the chart as it was before the work store:
// no mount, no DSN, no import -- a release-1-or-later gonk-gate then runs on bd.
func TestWorkStoreDisabledRendersThePreviousShape(t *testing.T) {
	out := Render(t, append(Minimum(), "--set", "workstore.enabled=false")...)
	ctrl := MustObject(t, out, "Deployment", "gonk-controller")
	for _, bad := range []string{"GONK_WORKSTORE_DSN_FILE", "secret-workstore", "workstore-import"} {
		if strings.Contains(ctrl.Doc, bad) {
			t.Errorf("workstore.enabled=false still renders %q", bad)
		}
	}
	if !strings.Contains(ctrl.Doc, "GONK_BEAD_REPO_DIR") {
		t.Error("workstore.enabled=false lost GONK_BEAD_REPO_DIR")
	}
}

// An unnamed work-store Secret must fail the RENDER, and for the right reason:
// the values schema rejects it (secretRef.existingSecret minLength 1). An
// assertion on "some error" would pass on an unrelated failure.
func TestWorkStoreSecretMustBeNamed(t *testing.T) {
	out, err := RenderErr(t, append(Minimum(), "--set", "secrets.workstore.existingSecret=")...)
	if err == nil {
		t.Fatalf("rendering with secrets.workstore.existingSecret=\"\" succeeded; it must fail closed:\n%s", out)
	}
	msg := err.Error() + out
	if !strings.Contains(msg, "secrets/workstore/existingSecret") || !strings.Contains(msg, "minLength") {
		t.Fatalf("render failed, but not on the empty work-store Secret name:\n%s", msg)
	}
}

func TestControllerNetworkPolicyReachesTheWorkStore(t *testing.T) {
	np := MustObject(t, Render(t, WithWorkStore(Minimum())...), "NetworkPolicy", "gonk-controller")
	if !strings.Contains(np.Doc, "port: 5432") {
		t.Errorf("gonk-controller NetworkPolicy has no egress to Postgres (5432):\n%s", np.Doc)
	}
	cnpg := MustObject(t, Render(t, append(WithWorkStore(Minimum()), "--set", "ledger.postgres.mode=cnpg")...), "NetworkPolicy", "gonk-controller")
	if !strings.Contains(cnpg.Doc, "cnpg.io/cluster: gonk-ledger-postgres") {
		t.Errorf("mode=cnpg: gonk-controller NetworkPolicy does not select the bundled CNPG cluster:\n%s", cnpg.Doc)
	}
	off := MustObject(t, Render(t, append(Minimum(), "--set", "workstore.enabled=false")...), "NetworkPolicy", "gonk-controller")
	if strings.Contains(off.Doc, "port: 5432") {
		t.Error("workstore.enabled=false still opens Postgres egress for the controller")
	}
}

// Verification defect 1: bd's "database not found" is an empty import ONLY on a
// positive fresh-install signal. The chart gives that signal to the import (the
// bootstrap) when Helm is INSTALLING the release -- never on an upgrade, which
// is how an existing install is cut over and where a missing database means a
// wrong dolt_database -- or when an operator sets workstore.freshInstall.
func TestFreshInstallSignalOnlyWhenHelmInstalls(t *testing.T) {
	const env = "GONK_WORKSTORE_FRESH_INSTALL"
	for name, c := range map[string]struct {
		args []string
		want bool
	}{
		"install":                  {WithWorkStore(Minimum()), true},
		"upgrade":                  {append(WithWorkStore(Minimum()), "--is-upgrade"), false},
		"upgrade, freshInstall on": {append(WithWorkStore(Minimum()), "--is-upgrade", "--set", "workstore.freshInstall=true"), true},
		"install, work store off":  {append(Minimum(), "--set", "workstore.enabled=false"), false},
	} {
		cs := controllerContainers(t, MustObject(t, Render(t, c.args...), "Deployment", "gonk-controller"))
		v, on := containerEnv(t, cs["bootstrap-city"])[env]
		if on != c.want || (on && v != "1") {
			t.Errorf("%s: bootstrap-city %s = %q (set %v), want set=%v to \"1\"", name, env, v, on, c.want)
		}
		if _, on := containerEnv(t, cs["controller"])[env]; on {
			t.Errorf("%s: %s set on the supervisor; only the import reads it", name, env)
		}
	}
}

// gonk-js99: the work store is OFF by default. Merging release 2 must not start
// the cutover by itself the moment Flux reconciles the chart: the Postgres role,
// the schema and the gonk-workstore Secret are provisioned in gitops, and
// without them the projected volume cannot mount and the controller (strategy
// Recreate) stays down. The deploy that turns it ON is the gitops HelmRelease
// value, landed with those objects.
func TestWorkStoreIsOffByDefaultAndOnWhenAsked(t *testing.T) {
	off := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-controller")
	for _, bad := range []string{"GONK_WORKSTORE_DSN_FILE", "secret-workstore", "workstore-import", "GONK_WORKSTORE_FRESH_INSTALL"} {
		if strings.Contains(off.Doc, bad) {
			t.Errorf("the DEFAULT render (no workstore.enabled) renders %q: the cutover would start on merge", bad)
		}
	}
	if !strings.Contains(off.Doc, "GONK_BEAD_REPO_DIR") {
		t.Error("the default render lost GONK_BEAD_REPO_DIR; it must be bd exactly as before")
	}
	on := MustObject(t, Render(t, WithWorkStore(Minimum())...), "Deployment", "gonk-controller")
	for _, want := range []string{"GONK_WORKSTORE_DSN_FILE", "secret-workstore", "workstore-import"} {
		if !strings.Contains(on.Doc, want) {
			t.Errorf("workstore.enabled=true does not render %q", want)
		}
	}
	// The values.yaml default and the schema must agree with each other.
	if out, err := RenderErr(t, append(Minimum(), "--set", "workstore.enabled=off")...); err == nil {
		t.Errorf("workstore.enabled=off rendered; the schema must reject a non-boolean:\n%s", out)
	}
}
