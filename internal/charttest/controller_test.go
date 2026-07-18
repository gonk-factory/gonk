//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// The bundled Gas City controller renders a SINGLE-REPLICA workload, a Service on
// the (Task 0.5-settled) supervisor port 9443, and NAMESPACED RBAC. No leader
// election exists, so there must be exactly one controller.
//
// SETTLED by Task 0.5 + the gonk-fsl re-smoke (chart/gonk/smoke/gc-controller-smoke.md):
//   - port 9443 (the per-city [api] listener), NOT 8372 (loopback admin API);
//   - command `gc start --foreground /city` as PID 1, NOT `gc supervisor run`
//     (which logs `[api] ... ignored under supervisor mode` and never binds 9443);
//   - city NAME gonk (`gc init --name gonk`), so /v0/city/gonk/... resolves;
//   - grant-gated: GC_CITY_WRITE_PUBKEY on, private signing key mounted;
//   - delivery prebaked + a writable-/city initContainer, NOT a ConfigMap at /city;
//   - Deployment, replicas 1, strategy Recreate.
func TestControllerRendersWorkloadServiceAndRBAC(t *testing.T) {
	out := Render(t, Minimum()...)

	d := MustObject(t, out, "Deployment", "gonk-controller")
	if !strings.Contains(d.Doc, "replicas: 1") {
		t.Errorf("the controller is not single-replica; with no leader election, two reconcile loops race the Dolt beads store:\n%s", d.Doc)
	}
	// Recreate, never RollingUpdate: maxSurge could briefly run two controllers
	// (smoke U3). This is the exactly-one guarantee in the absence of leader election.
	if !strings.Contains(d.Doc, "Recreate") {
		t.Errorf("the controller Deployment does not use strategy Recreate; a RollingUpdate can run two controllers at once:\n%s", d.Doc)
	}
	// `gc start --foreground /city` as PID 1 (gonk-fsl re-smoke): the ONLY mode that
	// binds the per-city [api] 9443 order-run route. NEVER `gc supervisor run` (it
	// ignores 9443) and NEVER `gc register` (it daemonizes a second supervisor and
	// self-crashes PID 1).
	if !strings.Contains(d.Doc, "gc start --foreground") {
		t.Errorf("the controller does not run `gc start --foreground`:\n%s", d.Doc)
	}
	if strings.Contains(d.Doc, "gc supervisor run") {
		t.Errorf("the controller runs `gc supervisor run`, which ignores the [api] 9443 order-run route (gonk-fsl re-smoke):\n%s", d.Doc)
	}
	if strings.Contains(d.Doc, "gc register") {
		t.Errorf("the controller calls `gc register`, which daemonizes a second supervisor and crashes PID 1 (smoke U3):\n%s", d.Doc)
	}
	// City name is load-bearing: intake dials /v0/city/gonk/... (hardcoded), so the
	// served name must be gonk or every dispatch 404s city-not-found.
	if !strings.Contains(d.Doc, "--name gonk") {
		t.Errorf("the bootstrap does not `gc init --name gonk`; the served city name would default to \"city\" and dispatch 404s (gonk-fsl re-smoke):\n%s", d.Doc)
	}
	// Grant-gating on: the [api] plane is 0.0.0.0 + allow_mutations, unauthenticated
	// without a verify key. GC_CITY_WRITE_PUBKEY turns on ed25519 verification.
	if !strings.Contains(d.Doc, "GC_CITY_WRITE_PUBKEY") {
		t.Errorf("the controller does not set GC_CITY_WRITE_PUBKEY; the mutation plane would be unauthenticated (G22):\n%s", d.Doc)
	}
	// The in-pod gonk-gate signs its own order-run POSTs: the PRIVATE key is a file
	// mount (a PATH env, never a value), same seam intake uses.
	if !strings.Contains(d.Doc, "GONK_GC_WRITE_KEY_FILE") || !strings.Contains(d.Doc, "secret-gc-write-key") {
		t.Errorf("the controller does not mount the ed25519 signing key for the in-pod gonk-gate:\n%s", d.Doc)
	}
	// The prebaked-delivery bootstrap: an initContainer copies the baked pack into
	// a writable /city and runs `gc init` there (a read-only ConfigMap at /city is
	// not viable -- gc init must write into it; smoke U2).
	if !strings.Contains(d.Doc, "initContainers") {
		t.Errorf("the controller has no initContainer to prep a writable /city from the baked pack (smoke U2):\n%s", d.Doc)
	}
	if !strings.Contains(d.Doc, "/opt/gonk/pack") {
		t.Errorf("the bootstrap does not copy the baked pack from /opt/gonk/pack (smoke U2):\n%s", d.Doc)
	}
	if !strings.Contains(d.Doc, "gc init") {
		t.Errorf("the bootstrap does not run `gc init` to write the k8s-cell profile (smoke U2):\n%s", d.Doc)
	}

	svc := MustObject(t, out, "Service", "gonk-controller")
	if !strings.Contains(svc.Doc, "9443") {
		t.Errorf("the supervisor Service is not on 9443 (the per-city [api] listener, smoke U1):\n%s", svc.Doc)
	}

	MustObject(t, out, "ServiceAccount", "gc-controller")
	MustObject(t, out, "ServiceAccount", "gc-agent")

	role := MustObject(t, out, "Role", "gc-controller")
	for _, want := range []string{"pods/exec", "pods/log", "configmaps"} {
		if !strings.Contains(role.Doc, want) {
			t.Errorf("gc-controller Role is missing %q (from docs/environment.md)", want)
		}
	}
	// The agent Role is pods:[get] ONLY -- least privilege.
	agentRole := MustObject(t, out, "Role", "gc-agent")
	if strings.Contains(agentRole.Doc, "pods/exec") || strings.Contains(agentRole.Doc, "configmaps") {
		t.Errorf("gc-agent Role is over-privileged; it must be pods:[get] only:\n%s", agentRole.Doc)
	}
	MustObject(t, out, "RoleBinding", "gc-controller")
	MustObject(t, out, "RoleBinding", "gc-agent")
}

// Port 8372 is the MACHINE-WIDE supervisor admin API and binds 127.0.0.1 LOOPBACK
// ONLY (smoke U1). It must NEVER appear in a Service, Ingress, or NetworkPolicy --
// a kubelet or a peer pod cannot reach it, and exposing it would be a lie in the
// object graph. The reachable port is 9443, the per-city [api] listener.
func TestSupervisorPortIs9443And8372IsNeverExposed(t *testing.T) {
	out := Render(t, Minimum()...)
	svc := MustObject(t, out, "Service", "gonk-controller")
	if !strings.Contains(svc.Doc, "9443") {
		t.Errorf("controller Service is not on 9443:\n%s", svc.Doc)
	}
	for _, o := range Objects(t, out) {
		switch o.Kind {
		case "Service", "Ingress", "NetworkPolicy":
			if strings.Contains(o.Doc, "8372") {
				t.Errorf("%s/%s exposes 8372, the loopback-only supervisor admin API (smoke U1):\n%s", o.Kind, o.Metadata.Name, o.Doc)
			}
		}
	}
}

// The controller RBAC is NAMESPACED. A ClusterRole would let a compromised
// controller reach every namespace -- exactly what the blast radius must not be.
func TestControllerRBACIsNamespacedNeverCluster(t *testing.T) {
	for _, o := range Objects(t, Render(t, Minimum()...)) {
		if o.Kind == "ClusterRole" || o.Kind == "ClusterRoleBinding" {
			t.Fatalf("the chart rendered a %s/%s; controller RBAC must be namespaced", o.Kind, o.Metadata.Name)
		}
	}
}

// gascity.enabled=false: BYO controller. NOTHING controller-shaped renders, and
// intake still learns where to dispatch (the external supervisorURL).
func TestBYOGasCityRendersNoController(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "gascity.enabled=false",
		"--set", "gascity.supervisorURL=http://gc.external.svc:9443")...)
	for _, kind := range []string{"Deployment", "StatefulSet", "Pod"} {
		NoObject(t, out, kind, "gonk-controller")
	}
	NoObject(t, out, "ServiceAccount", "gc-controller")
	NoObject(t, out, "ServiceAccount", "gc-agent")
	NoObject(t, out, "Role", "gc-controller")
	NoObject(t, out, "Service", "gonk-controller")
	// intake still learns where to dispatch.
	d := MustObject(t, out, "Deployment", "gonk-intake")
	if !strings.Contains(d.Doc, "gc.external.svc") {
		t.Errorf("intake was not pointed at the external supervisor:\n%s", d.Doc)
	}
}

// Intake's GONK_SUPERVISOR_URL is DERIVED from the in-chart controller Service on
// 9443 when the controller is bundled (the default). This is the gonk.supervisorURL
// helper's bundled branch.
func TestIntakeSupervisorURLDerivedFromBundledControllerOn9443(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-intake")
	if !strings.Contains(d.Doc, "http://gonk-controller.gonk.svc:9443") {
		t.Errorf("intake's GONK_SUPERVISOR_URL is not the bundled controller Service on 9443:\n%s", d.Doc)
	}
}

// DELIVERY is prebaked ONLY (smoke U2). A read-only ConfigMap mounted at /city is
// not viable because `gc init` must WRITE .gc/, city.toml and .beads/ there. So the
// draft's delivery=configmap case is REMOVED: no gonk-city ConfigMap is ever
// rendered, and the pack reaches a WRITABLE /city via an initContainer.
func TestPackDeliveryIsPrebakedWithWritableCityInit(t *testing.T) {
	out := Render(t, Minimum()...)
	NoObject(t, out, "ConfigMap", "gonk-city")
	d := MustObject(t, out, "Deployment", "gonk-controller")
	// /city is an emptyDir (writable), shared with the bootstrap initContainer --
	// never a read-only configMap volume.
	if !strings.Contains(d.Doc, "initContainers") || !strings.Contains(d.Doc, "/opt/gonk/pack") {
		t.Errorf("prebaked delivery does not copy the baked pack into a writable /city via an initContainer:\n%s", d.Doc)
	}
	if strings.Contains(d.Doc, "configMap:\n") && strings.Contains(d.Doc, "name: gonk-city") {
		t.Errorf("the controller mounts a gonk-city ConfigMap at /city; delivery=configmap was removed (smoke U2):\n%s", d.Doc)
	}
}

// WRITE-AUTH end-to-end wiring (gonk-fsl / gonk-5we). The bundled controller is
// grant-gated, so BOTH the dispatcher (gonk-intake, cross-pod) and the in-pod
// gonk-gate must sign order-run POSTs with the SAME ed25519 private key, and the
// controller must verify with its public half. The private key is a 0400 file
// mount from a pre-provisioned Secret (never an env value, never chart-created);
// the public key is GC_CITY_WRITE_PUBKEY on the controller.
func TestWriteAuthWiredIntoIntakeAndController(t *testing.T) {
	out := Render(t, Minimum()...)

	ctrl := MustObject(t, out, "Deployment", "gonk-controller")
	intake := MustObject(t, out, "Deployment", "gonk-intake")

	// Controller: public verify key + gate signing env + key mount.
	for _, want := range []string{
		"GC_CITY_WRITE_PUBKEY",
		"k1:1hcioE4eYD4PsM66wVJ8oBErEfCTyNPt9Q/+ZT0drmk=",
		"GONK_GC_WRITE_KEY_FILE",
		"name: GONK_GC_WRITE_KEY_ID",
		"secret-gc-write-key",
	} {
		if !strings.Contains(ctrl.Doc, want) {
			t.Errorf("controller is missing write-auth wiring %q:\n%s", want, ctrl.Doc)
		}
	}
	// The kid must be derived from the public verifyKey ("k1:...") so it cannot drift.
	// Asserted against the RAW render (quotes preserved): `value: "k1"` matches the
	// kid but not the pubkey `value: "k1:..."`.
	if !strings.Contains(out, `value: "k1"`) {
		t.Errorf("GONK_GC_WRITE_KEY_ID is not the kid \"k1\" derived from verifyKey:\n%s", ctrl.Doc)
	}

	// Intake: SAME signing key mount + env, so its cross-pod dispatch is signed.
	for _, want := range []string{
		"GONK_GC_WRITE_KEY_FILE",
		"name: GONK_GC_WRITE_KEY_ID",
		"secret-gc-write-key",
		"gonk-gc-write-key", // the pre-provisioned Secret NAME both pods reference
	} {
		if !strings.Contains(intake.Doc, want) {
			t.Errorf("intake is missing write-auth signing wiring %q:\n%s", want, intake.Doc)
		}
	}

	// The KEY FILE is a PATH under the secret mount root, never an env VALUE, and the
	// controller must NOT carry the private signing key inline anywhere.
	if !strings.Contains(ctrl.Doc, "/etc/gonk/secrets/gc-write-key/key") {
		t.Errorf("controller GONK_GC_WRITE_KEY_FILE is not the mounted key path:\n%s", ctrl.Doc)
	}
	// defaultMode 0400 on the projected key (secret-safety).
	if !strings.Contains(ctrl.Doc, "defaultMode: 256") {
		t.Errorf("the gc-write-key volume is not mode 0400 (256 decimal):\n%s", ctrl.Doc)
	}
}

// The workload type is selectable between the two single-instance shapes the smoke
// leaves open: the settled default Deployment (strategy Recreate) and a StatefulSet
// (OrderedReady). Both are exactly one instance. The upstream bare-Pod hack
// (restartPolicy: Never) is dropped -- the smoke proved a Deployment survives a pod
// restart cleanly, so a never-restarting Pod is strictly worse and unjustified.
func TestWorkloadTypeIsSelectable(t *testing.T) {
	ss := Render(t, append(Minimum(), "--set", "gascity.workload=statefulset")...)
	sts := MustObject(t, ss, "StatefulSet", "gonk-controller")
	if !strings.Contains(sts.Doc, "replicas: 1") {
		t.Errorf("the StatefulSet controller is not single-replica:\n%s", sts.Doc)
	}
	NoObject(t, ss, "Deployment", "gonk-controller")
}
