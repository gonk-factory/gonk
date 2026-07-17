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
// SETTLED by Task 0.5 (chart/gonk/smoke/gc-controller-smoke.md), overriding the
// plan's provisional draft:
//   - port 9443 (the per-city [api] listener), NOT 8372 (loopback admin API);
//   - command `gc supervisor run` as PID 1, NOT `gc start --foreground /city`;
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
	// `gc supervisor run` as PID 1 (smoke U1/U3). Never `--foreground` (that flag
	// does not exist) and never `gc register` (it daemonizes a second supervisor
	// and self-crashes PID 1).
	if !strings.Contains(d.Doc, "gc supervisor run") {
		t.Errorf("the controller does not run `gc supervisor run`:\n%s", d.Doc)
	}
	if strings.Contains(d.Doc, "--foreground") {
		t.Errorf("the controller uses `--foreground`, which is the plan's provisional draft; that flag does not exist (smoke U1):\n%s", d.Doc)
	}
	if strings.Contains(d.Doc, "gc register") {
		t.Errorf("the controller calls `gc register`, which daemonizes a second supervisor and crashes PID 1 (smoke U3):\n%s", d.Doc)
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
