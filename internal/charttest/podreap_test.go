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

	"github.com/gonk-factory/gonk/internal/envname"
)

// THE POD REAPER'S CHART SURFACE (gonk-mfsr).
//
// This is the only feature in gonk that DELETES a Kubernetes object, so the
// chart's job here is almost entirely about what does NOT happen: with
// podReap.enabled false -- which is what values.yaml ships and what merging the
// chart therefore deploys -- nothing renders, no env reaches the controller,
// and gonk-gate builds no pod reaper at all.

// RenderNamespace is the namespace Render templates into (charttest.go passes
// --namespace gonk), and therefore what .Release.Namespace expands to.
const RenderNamespace = "gonk"

// WithPodReap turns the pod reaper ON for a render. It is OFF in values.yaml,
// so every test of its rendered shape has to say so explicitly -- the same
// convention WithWorkStore follows, and for the same reason.
func WithPodReap(args []string) []string {
	return append(append([]string{}, args...), "--set", "podReap.enabled=true")
}

// THE DEFAULT IS OFF, AND OFF MUST BE COMPLETE. A merge of this chart cannot
// arm pod deletion: not the env that switches gonk-gate's reaper on, not the
// Role, not the RoleBinding.
func TestPodReaperIsOffByDefault(t *testing.T) {
	out := Render(t, Minimum()...)

	NoObject(t, out, "Role", "gonk-gate-podreap")
	NoObject(t, out, "RoleBinding", "gonk-gate-podreap")

	c := controllerContainers(t, MustObject(t, out, "Deployment", "gonk-controller"))["controller"]
	if c == nil {
		t.Fatal("controller Deployment has no controller container")
	}
	if v, ok := containerEnv(t, c)[envname.PodReapNamespace]; ok {
		t.Fatalf("%s = %q with podReap.enabled false. The namespace IS "+
			"the on switch: set, gonk-gate will list and DELETE agent pods.",
			envname.PodReapNamespace, v)
	}
}

// AND ON MEANS ALL THREE, TOGETHER. An env without the Role is a reaper that
// 403s on every tick; a Role without the env is a grant nothing uses.
func TestPodReaperEnabledRendersTheEnvAndTheGrant(t *testing.T) {
	out := Render(t, WithPodReap(Minimum())...)

	c := controllerContainers(t, MustObject(t, out, "Deployment", "gonk-controller"))["controller"]
	env := containerEnv(t, c)
	ns := env[envname.PodReapNamespace]
	if ns == "" {
		t.Fatalf("podReap.enabled did not set %s; gonk-gate builds no "+
			"pod reaper without it and the feature is silently off",
			envname.PodReapNamespace)
	}
	if ns != RenderNamespace {
		t.Fatalf("%s = %q, want the release namespace %q. Any other value "+
			"points the list and the DELETE at someone else's pods.",
			envname.PodReapNamespace, ns, RenderNamespace)
	}

	MustObject(t, out, "Role", "gonk-gate-podreap")
	MustObject(t, out, "RoleBinding", "gonk-gate-podreap")
}

// THE JOIN ITSELF (gonk-b6pb). Everything above asserts the chart against
// envname.PodReapNamespace; this asserts, in one place and with one message,
// that the constant is also what the BINARY reads -- so the chart and
// cmd/gonk-gate cannot be renamed apart.
//
// The hole this closes was demonstrated, not imagined. A reviewer renamed ONLY
// the Go constant to GONK_PODREAP_NS, left the template emitting the old name,
// and ./cmd/gonk-gate, ./internal/charttest and ./internal/buildgate were ALL
// green. In production that is gonk-gate reading a variable nobody sets: no
// namespace, no reaper, no log line, and a leaked pod that looks exactly like a
// cluster with no leaks.
//
// THIS IS THE CHART HALF: the rendered controller carries
// envname.PodReapNamespace. The BINARY half -- that cmd/gonk-gate's constant is
// still that same value, and that it is still read -- lives in
// cmd/gonk-gate/envname_join_test.go, because only a test inside that package
// can compare the two constants by VALUE. That split matters: the first version
// of this test checked the binary half with a regexp over the source, and
// verification of 0d5e6a7 walked through it by assembling the name from two
// literals (`"GONK_POD_REAP" + "_NAMESPACE"`), which no text search for the
// name can see.
func TestPodReapEnvNameIsTheOneGonkGateReads(t *testing.T) {
	c := controllerContainers(t, MustObject(t, Render(t, WithPodReap(Minimum())...),
		"Deployment", "gonk-controller"))["controller"]
	if c == nil {
		t.Fatal("controller Deployment has no controller container")
	}
	if _, ok := containerEnv(t, c)[envname.PodReapNamespace]; !ok {
		var names []string
		for n := range containerEnv(t, c) {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("the controller sets no %s.\n"+
			"cmd/gonk-gate reads THAT name and builds no pod reaper without it, so this is a "+
			"chart and binary renamed apart -- the feature is off and nothing says so (gonk-b6pb).\n"+
			"env names rendered: %s", envname.PodReapNamespace, strings.Join(names, ", "))
	}
}

// A NAMESPACED Role, NEVER a ClusterRole. A ClusterRole here would let a
// compromised controller delete pods in every namespace on the cluster.
func TestPodReapRoleIsNamespacedAndMinimal(t *testing.T) {
	out := Render(t, WithPodReap(Minimum())...)

	NoObject(t, out, "ClusterRole", "gonk-gate-podreap")
	NoObject(t, out, "ClusterRoleBinding", "gonk-gate-podreap")

	role := MustObject(t, out, "Role", "gonk-gate-podreap")
	rules := toSliceOfMaps(t, role.Raw["rules"])
	if len(rules) != 1 {
		t.Fatalf("Role gonk-gate-podreap has %d rules, want exactly 1 (pods)", len(rules))
	}
	if g := toStringSlice(rules[0]["apiGroups"]); len(g) != 1 || g[0] != "" {
		t.Fatalf("rule is not scoped to the core API group: apiGroups = %v", g)
	}
	if r := toStringSlice(rules[0]["resources"]); len(r) != 1 || r[0] != "pods" {
		t.Fatalf("Role gonk-gate-podreap reaches beyond pods: resources = %v", r)
	}
}

// THE BINDING'S SUBJECT IS THE CONTROLLER, AND NOT THE AGENT. gonk-gate's sweep
// is an exec order inside the controller pod, so it authenticates as
// gc-controller. Binding gc-agent instead would hand every agent session the
// ability to delete a sibling agent's pod.
func TestPodReapRoleBindingNamesOnlyTheControllerServiceAccount(t *testing.T) {
	rb := MustObject(t, Render(t, WithPodReap(Minimum())...), "RoleBinding", "gonk-gate-podreap")

	ref, _ := rb.Raw["roleRef"].(map[string]any)
	if ref["kind"] != "Role" || ref["name"] != "gonk-gate-podreap" {
		t.Fatalf("roleRef = %v, want the namespaced Role gonk-gate-podreap", ref)
	}
	subs := toSliceOfMaps(t, rb.Raw["subjects"])
	if len(subs) != 1 {
		t.Fatalf("RoleBinding has %d subjects, want exactly 1", len(subs))
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
func TestPodReapGrantsNeedTheBundledController(t *testing.T) {
	args := append(WithPodReap(Minimum()), "--set", "gascity.enabled=false",
		"--set", "gascity.supervisorURL=http://gc.example:9443")
	out, err := RenderErr(t, args...)
	if err != nil {
		t.Skipf("BYO-gascity render is refused by a guard unrelated to this test: %v", err)
	}
	NoObject(t, out, "Role", "gonk-gate-podreap")
	NoObject(t, out, "RoleBinding", "gonk-gate-podreap")
}

// THE TEST THAT TIES THE ROLE TO REALITY, exactly as
// TestKeySinkRoleVerbsMatchK8sSink does for the meter: the verbs granted on
// pods must be EXACTLY the client-go calls cmd/gonk-gate/reap_pods.go makes,
// derived from the source rather than copied by hand. A future reaper that
// starts calling Get or Watch fails HERE, instead of failing as a 403 on a live
// cluster -- and a Role that keeps granting a verb the reaper stopped using
// fails here too.
func TestPodReapRoleVerbsMatchTheReaperSource(t *testing.T) {
	role := MustObject(t, Render(t, WithPodReap(Minimum())...), "Role", "gonk-gate-podreap")

	var granted []string
	for _, rule := range toSliceOfMaps(t, role.Raw["rules"]) {
		if !containsStr(toStringSlice(rule["resources"]), "pods") {
			continue
		}
		granted = append(granted, toStringSlice(rule["verbs"])...)
	}
	sort.Strings(granted)

	want := podReaperPodVerbs(t)
	if !reflect.DeepEqual(granted, want) {
		t.Fatalf("Role gonk-gate-podreap grants verbs %v on pods, but "+
			"cmd/gonk-gate/reap_pods.go actually calls %v -- update "+
			"role-gonk-gate-podreap.yaml to match (add a verb it needs, or drop one "+
			"it does not)", granted, want)
	}
}

// podsVerbCall matches a client-go call of the shape
// `<something>.Pods(<ns>).<Verb>(ctx, ...)`.
var podsVerbCall = regexp.MustCompile(`\.Pods\([^)]*\)\.(Get|List|Watch|Create|Update|UpdateStatus|Patch|Delete|DeleteCollection)\(`)

// podReaperPodVerbs reads cmd/gonk-gate/reap_pods.go directly and returns the
// sorted, de-duplicated RBAC verbs its Pods calls require. A source-level check,
// not a copy of today's verb list, so it keeps tracking the reaper as it changes.
func podReaperPodVerbs(t *testing.T) []string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(self), "..", "..", "cmd", "gonk-gate", "reap_pods.go")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read pod reaper source %s: %v", src, err)
	}

	verbSet := map[string]bool{}
	for _, m := range podsVerbCall.FindAllStringSubmatch(string(b), -1) {
		switch m[1] {
		case "Get":
			verbSet["get"] = true
		case "List":
			verbSet["list"] = true
		case "Watch":
			verbSet["watch"] = true
		case "Create":
			verbSet["create"] = true
		case "Update", "UpdateStatus":
			verbSet["update"] = true
		case "Patch":
			verbSet["patch"] = true
		case "Delete":
			verbSet["delete"] = true
		case "DeleteCollection":
			verbSet["deletecollection"] = true
		}
	}
	if len(verbSet) == 0 {
		t.Fatalf("found no client-go Pods() calls in %s; the regex or the file moved, and "+
			"this test would then happily accept any Role at all", src)
	}
	var out []string
	for v := range verbSet {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// THE ENV NAME MUST SURVIVE THE EXEC-ORDER STRIP. Gas City's
// execenv.FilterInherited drops every key execenv.IsSensitiveKey flags from an
// exec order's inherited environment, and gonk-sweep IS an exec order. A pod
// reaper whose switch was stripped would be silently off, which is
// indistinguishable from a cluster with no leaks.
//
// IT CHECKS envname.PodReapNamespace, NOT A COPY OF IT. This test used to
// declare `const name = "GONK_POD_REAP_NAMESPACE"` in its own body and inspect
// that -- so it asserted a property of a string literal nothing else in the
// system used, and renaming the real variable on BOTH sides left it passing
// (gonk-3nir). A test of a name must read the name.
//
// execSensitiveMarkers below is execenv.IsSensitiveKey's OWN list, transcribed
// from internal/execenv/execenv.go at GASCITY_REF 4fda5a28 -- the SHA
// images/versions.env pins -- and read there on 2026-09-23, not remembered.
// IsSensitiveKey uppercases the key and asks strings.Contains for each marker,
// which is what this does.
//
// gonkExtraMarkers is what this repo adds on top, and it is an ADDITION, not a
// correction: execenv does NOT flag a bare KEY or APIKEY. Adding them keeps the
// guard a strict superset, because the cost of refusing a slightly-too-scary
// new name is a rename and the cost of shipping a stripped one is a
// pod-deleting feature that never runs.
//
// THE EARLIER VERSION OF THIS COMMENT WAS WRONG AND THE LIST WAS NOT A
// SUPERSET. It claimed CREDENTIAL was an addition (execenv has had it all
// along) and it omitted OAUTH, AUTH_JSON and AUTH-JSON entirely -- so a name
// renamed on both sides to GONK_POD_REAP_OAUTH_NS passed this guard while the
// real IsSensitiveKey returns true for it, i.e. it would have been stripped in
// production and disarmed the reaper silently. Measured, not reasoned about.
//
// envname.KeysinkNamespace is deliberately NOT run through this list. It
// contains KEY -- one of OUR two additions, not an execenv marker -- so execenv
// leaves it alone, which is why gonk-gate has been reading it in the sweep exec
// order all along.
var execSensitiveMarkers = []string{
	"PASSWORD",
	"TOKEN",
	"SECRET",
	"PRIVATE_KEY",
	"PRIVATE-KEY",
	"API_KEY",
	"API-KEY",
	"ACCESS_KEY",
	"ACCESS-KEY",
	"CREDENTIAL",
	"OAUTH",
	"AUTH_JSON",
	"AUTH-JSON",
}

var gonkExtraMarkers = []string{"KEY", "APIKEY"}

func TestPodReapEnvNameCarriesNoSensitivityMarker(t *testing.T) {
	name := strings.ToUpper(strings.TrimSpace(envname.PodReapNamespace))
	for _, marker := range append(append([]string{}, execSensitiveMarkers...), gonkExtraMarkers...) {
		if strings.Contains(name, marker) {
			t.Fatalf("%s contains %q, so Gas City strips it from the gonk-sweep exec order's "+
				"environment and the pod reaper never arms", envname.PodReapNamespace, marker)
		}
	}
}
