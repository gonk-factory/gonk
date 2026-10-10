//go:build chart

package charttest

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/envname"
)

// THE JOBS SESSION RUNTIME'S CHART SURFACE (gonk-t55, plan P5).
//
// gonk-gate creates each agent session as a Kubernetes Job (pkg/runtime/jobs).
// The chart owes it two things: an API grant that is exactly what the runtime
// calls, and the configuration it cannot guess -- which image, which namespace,
// which ServiceAccount, how long a finished Job lives, how big a session is.
//
// UNLIKE THE POD REAPER, THIS IS ON BY DEFAULT. Owner decision 2026-09-25
// (cutover option A): the Jobs runtime is the only session runtime behind
// dispatch and sweep, so a controller without this grant is a controller whose
// every dispatch 403s. There is no flag to forget.

// jobsRules is the Role's rule list, exactly: apiGroup/resource -> verbs.
// Checked by hand against every client call in pkg/runtime/jobs/jobs.go (t55
// verify round 1, which found `watch` and pods `get` granted and never used,
// and dropped them). It is still a literal: a source-derived check (the way
// TestPodReapRoleVerbsMatchTheReaperSource reads reap_pods.go) would catch a
// new call the literal misses, and is not built.
var jobsRules = map[string][]string{
	"batch/jobs": {"create", "delete", "get", "list"},
	"/secrets":   {"create", "delete", "get"},
	"/pods":      {"list"},
	"/pods/log":  {"get"},
}

// THE GRANT RENDERS IN THE DEFAULT RENDER. A merge that deploys the Jobs
// runtime without it is a dispatch that fails on every issue.
func TestJobsRuntimeGrantRendersByDefault(t *testing.T) {
	out := Render(t, Minimum()...)
	MustObject(t, out, "Role", "gonk-gate-jobs")
	MustObject(t, out, "RoleBinding", "gonk-gate-jobs")

	// And in the dev-shaped profile, which is what the goldens pin.
	out = Render(t, Profile("values-default")...)
	MustObject(t, out, "Role", "gonk-gate-jobs")
	MustObject(t, out, "RoleBinding", "gonk-gate-jobs")
}

// A NAMESPACED Role, NEVER a ClusterRole: a ClusterRole here would let a
// compromised controller create Jobs, and read and delete Secrets, in every
// namespace on the cluster.
func TestJobsRuntimeRoleIsNamespaced(t *testing.T) {
	out := Render(t, Minimum()...)
	NoObject(t, out, "ClusterRole", "gonk-gate-jobs")
	NoObject(t, out, "ClusterRoleBinding", "gonk-gate-jobs")
}

// EXACTLY THE RULES THE RUNTIME NEEDS, and nothing else: no update or patch, no
// pods create (the Job controller creates the pod), no pods/exec, no
// jobs/status. A rule added "while we are here" fails; so does one dropped.
func TestJobsRuntimeRoleGrantsExactlyTheRuntimeCalls(t *testing.T) {
	role := MustObject(t, Render(t, Minimum()...), "Role", "gonk-gate-jobs")

	got := map[string][]string{}
	for _, rule := range toSliceOfMaps(t, role.Raw["rules"]) {
		for _, g := range toStringSlice(rule["apiGroups"]) {
			for _, r := range toStringSlice(rule["resources"]) {
				k := g + "/" + r
				got[k] = append(got[k], toStringSlice(rule["verbs"])...)
			}
		}
	}
	for k := range got {
		sort.Strings(got[k])
	}
	if !reflect.DeepEqual(got, jobsRules) {
		t.Fatalf("Role gonk-gate-jobs grants\n  %v\nwant exactly\n  %v\n"+
			"(apiGroup/resource -> verbs; the core group is the empty string). Update "+
			"role-gonk-gate-jobs.yaml and its per-call table together, and only for a "+
			"call pkg/runtime/jobs actually makes.", got, jobsRules)
	}
}

// THE BINDING'S SUBJECT IS THE CONTROLLER, AND NOT THE AGENT. gonk-gate's
// dispatch and sweep are exec orders inside the controller pod, so they run as
// gc-controller. Binding gc-agent would let a session read a sibling session's
// Secret -- which holds that sibling's LiteLLM key.
func TestJobsRuntimeRoleBindingNamesOnlyTheControllerServiceAccount(t *testing.T) {
	rb := MustObject(t, Render(t, Minimum()...), "RoleBinding", "gonk-gate-jobs")

	ref, _ := rb.Raw["roleRef"].(map[string]any)
	if ref["kind"] != "Role" || ref["name"] != "gonk-gate-jobs" ||
		ref["apiGroup"] != "rbac.authorization.k8s.io" {
		t.Fatalf("roleRef = %v, want the namespaced Role gonk-gate-jobs", ref)
	}
	subs := toSliceOfMaps(t, rb.Raw["subjects"])
	if len(subs) != 1 {
		t.Fatalf("RoleBinding gonk-gate-jobs has %d subjects, want exactly 1", len(subs))
	}
	if subs[0]["kind"] != "ServiceAccount" || subs[0]["name"] != "gc-controller" {
		t.Fatalf("subject = %v, want ServiceAccount gc-controller", subs[0])
	}
	if subs[0]["namespace"] != RenderNamespace {
		t.Fatalf("subject namespace = %v, want %q", subs[0]["namespace"], RenderNamespace)
	}
}

// Nothing renders for a BYO Gas City: with gascity.enabled false the chart
// deploys no controller, so there is no gonk-gate in the cluster to grant.
func TestJobsRuntimeGrantNeedsTheBundledController(t *testing.T) {
	args := append(Minimum(), "--set", "gascity.enabled=false",
		"--set", "gascity.supervisorURL=http://gc.example:9443")
	out, err := RenderErr(t, args...)
	if err != nil {
		t.Skipf("BYO-gascity render is refused by a guard unrelated to this test: %v", err)
	}
	NoObject(t, out, "Role", "gonk-gate-jobs")
	NoObject(t, out, "RoleBinding", "gonk-gate-jobs")
}

// controllerEnv renders and returns the controller container's env.
func controllerEnv(t *testing.T, args ...string) map[string]string {
	t.Helper()
	c := controllerContainers(t, MustObject(t, Render(t, args...),
		"Deployment", "gonk-controller"))["controller"]
	if c == nil {
		t.Fatal("controller Deployment has no controller container")
	}
	return containerEnv(t, c)
}

// EVERY NAME THE RUNTIME READS IS SET, THROUGH envname. A name renamed on one
// side only is gonk-gate reading a variable nobody sets (gonk-b6pb).
func TestJobsRuntimeEnvIsSetOnTheController(t *testing.T) {
	env := controllerEnv(t, Minimum()...)
	var missing []string
	for _, n := range envname.SessionRuntime {
		if strings.TrimSpace(env[n]) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		var names []string
		for n := range env {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("the controller does not set %v (or sets it empty).\n"+
			"pkg/runtime/jobs reads these through internal/envname; unset, the runtime "+
			"has no image, no namespace (client-go reads \"\" as CLUSTER scope) or no "+
			"sizing.\nenv names rendered: %s",
			missing, strings.Join(names, ", "))
	}
}

// ONE AGENT IMAGE PER RELEASE. GONK_AGENT_IMAGE is the same string Gas City's
// GC_K8S_IMAGE gets, from componentImages.agent: the Jobs path and the Gas City
// path must never run different agent images from one render.
func TestJobsRuntimeAgentImageIsComponentImagesAgent(t *testing.T) {
	const tag = "v0.1.0-999.0123456789ab"
	env := controllerEnv(t, append(Minimum(), "--set", "componentImages.agent.tag="+tag)...)

	img := env[envname.AgentImage]
	want := publicRegistry + "/gonk-agent:" + tag
	if img != want {
		t.Fatalf("%s = %q, want %q (image.registry / componentImages.agent)",
			envname.AgentImage, img, want)
	}
	if gc := env["GC_K8S_IMAGE"]; gc != img {
		t.Fatalf("%s = %q but GC_K8S_IMAGE = %q; one release must run ONE agent image",
			envname.AgentImage, img, gc)
	}
}

// THE NAMESPACE IS THE RELEASE NAMESPACE and the ServiceAccount is the one the
// chart renders for agents -- which also carries the registry pull secret, so a
// Job pod running as it can pull the agent image (D6).
func TestJobsRuntimeSessionNamespaceAndServiceAccount(t *testing.T) {
	// The pull secret has no default (a public registry needs none), so this
	// render names one, as an installation with a private registry does.
	const pullSecret = "registry-pull-creds"
	out := Render(t, append(Minimum(), "--set", "image.pullSecrets[0].name="+pullSecret)...)
	c := controllerContainers(t, MustObject(t, out, "Deployment", "gonk-controller"))["controller"]
	env := containerEnv(t, c)

	if ns := env[envname.SessionNamespace]; ns != RenderNamespace {
		t.Fatalf("%s = %q, want the release namespace %q. Any other value creates "+
			"sessions where Role gonk-gate-jobs grants nothing.",
			envname.SessionNamespace, ns, RenderNamespace)
	}
	sa := env[envname.SessionServiceAccount]
	if sa != "gc-agent" {
		t.Fatalf("%s = %q, want gc-agent (D6)", envname.SessionServiceAccount, sa)
	}
	acct := MustObject(t, out, "ServiceAccount", sa)
	var pulls []string
	for _, p := range toSliceOfMaps(t, acct.Raw["imagePullSecrets"]) {
		s, _ := p["name"].(string)
		pulls = append(pulls, s)
	}
	if !containsStr(pulls, pullSecret) {
		t.Fatalf("ServiceAccount %s carries imagePullSecrets %v; a session pod "+
			"running as it cannot pull the agent image from the private registry", sa, pulls)
	}
}

// THE DEFAULTS ARE D6: the sizing a live Gas City agent pod ran with, and a
// TTL long enough for the sweep to read a finished session's log.
func TestJobsRuntimeDefaultsAreD6(t *testing.T) {
	env := controllerEnv(t, Minimum()...)
	want := map[string]string{
		envname.SessionTTLSeconds:    "3600",
		envname.SessionCPURequest:    "500m",
		envname.SessionMemoryRequest: "1Gi",
		envname.SessionCPULimit:      "2",
		envname.SessionMemoryLimit:   "4Gi",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want the D6 default %q", k, env[k], v)
		}
	}
}

// AND THEY ARE VALUES, NOT CONSTANTS: an operator's setting reaches gonk-gate.
func TestJobsRuntimeValuesReachTheController(t *testing.T) {
	env := controllerEnv(t, append(Minimum(),
		"--set", "sessionRuntime.ttlSecondsAfterFinished=7200",
		"--set", "sessionRuntime.resources.requests.cpu=250m",
		"--set", "sessionRuntime.resources.requests.memory=512Mi",
		"--set", "sessionRuntime.resources.limits.cpu=3",
		"--set", "sessionRuntime.resources.limits.memory=6Gi",
	)...)
	want := map[string]string{
		envname.SessionTTLSeconds:    "7200",
		envname.SessionCPURequest:    "250m",
		envname.SessionMemoryRequest: "512Mi",
		envname.SessionCPULimit:      "3",
		envname.SessionMemoryLimit:   "6Gi",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q from sessionRuntime", k, env[k], v)
		}
	}
}

// THE SCHEMA REFUSES WHAT THE RUNTIME CANNOT USE, at render time, instead of
// gonk-gate failing every dispatch on a quantity it cannot parse or a TTL that
// deletes a finished Job before the sweep has read it.
func TestJobsRuntimeSchemaRefusesBadValues(t *testing.T) {
	for name, set := range map[string]string{
		"ttl below a few sweep intervals": "sessionRuntime.ttlSecondsAfterFinished=60",
		"ttl zero":                        "sessionRuntime.ttlSecondsAfterFinished=0",
		"cpu not a quantity":              "sessionRuntime.resources.requests.cpu=lots",
		"memory empty":                    "sessionRuntime.resources.limits.memory=",
		"unknown key":                     "sessionRuntime.namespace=elsewhere",
	} {
		t.Run(name, func(t *testing.T) {
			out, err := RenderErr(t, append(Minimum(), "--set", set)...)
			if err == nil {
				t.Fatalf("--set %s rendered; values.schema.json must refuse it", set)
			}
			// Refused for THIS value, not for something unrelated in the render.
			if !strings.Contains(out, "sessionRuntime") {
				t.Fatalf("--set %s was refused, but not by the sessionRuntime schema: %v\n%s", set, err, out)
			}
		})
	}
}

// THE NAMES MUST SURVIVE THE EXEC-ORDER STRIP. gonk-dispatch and gonk-sweep are
// Gas City exec orders, and execenv.FilterInherited drops any inherited env
// name carrying a sensitivity marker. A stripped GONK_AGENT_IMAGE is a dispatch
// with no image. Same marker lists as TestPodReapEnvNameCarriesNoSensitivityMarker.
func TestJobsRuntimeEnvNamesCarryNoSensitivityMarker(t *testing.T) {
	markers := append(append([]string{}, execSensitiveMarkers...), gonkExtraMarkers...)
	for _, n := range envname.SessionRuntime {
		name := strings.ToUpper(strings.TrimSpace(n))
		for _, m := range markers {
			if strings.Contains(name, m) {
				t.Errorf("%s contains %q, so Gas City strips it from the gonk-dispatch / "+
					"gonk-sweep exec orders and the Jobs runtime never sees it", n, m)
			}
		}
	}
}
