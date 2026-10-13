//go:build chart

package charttest

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A PERMISSION IS TWO RULES (gonk-nax7)
// ---------------------------------------------------------------------------
//
// NetworkPolicy is enforced on EGRESS at the source pod and on INGRESS at the
// destination pod. Inside this namespace the default-deny policy covers both
// directions for every pod, so a connection between two pods needs BOTH an
// egress rule on the source's policy and an ingress rule on the destination's.
// Either one alone permits nothing, and reads as if it did.
//
// Two such dead rules shipped in v0.2.0, both towards gonk-intake's private
// listener: the agent pod's fetch of its checkout (gonk-nax7) and the
// controller's registration of that checkout. Each had its egress half and no
// ingress half. TestInClusterDialsHaveEgress could not see either: it checks
// egress only, and says so.
//
// The tests below hold nothing but the rendered policies. Every same-namespace
// podSelector peer in any rule is followed to the policy that governs the pod
// at the other end, which must carry the mirror rule on the same TCP port. So
// an edit to one side alone fails here, in whichever direction it is made.

// peerEdge is one same-namespace TCP permission read from one side.
type peerEdge struct {
	Policy string // the policy the rule was read from
	Src    string // canonical selector of the dialling pod
	Dst    string // canonical selector of the listening pod
	Port   int    // 0 = every port
}

func (e peerEdge) String() string {
	port := "every port"
	if e.Port != 0 {
		port = fmt.Sprintf("TCP %d", e.Port)
	}
	return fmt.Sprintf("{%s} -> {%s} on %s", e.Src, e.Dst, port)
}

// pairExemption names a one-sided rule that is KNOWN and not fixed here. Each
// one is a real defect on a cluster that enforces NetworkPolicy; the list
// exists so that the defect is written down next to the test that found it
// rather than hidden by narrowing the test. An entry that matches nothing is a
// failure (TestEveryPairExemptionStillMatchesARule).
type pairExemption struct {
	Dst string // canonical selector of the destination
	Why string
}

var pairExemptions = []pairExemption{
	{
		Dst: "cnpg.io/cluster=gonk-ledger-postgres",
		Why: "ledger.postgres.mode=cnpg: gonk-meter and the controller have egress to the " +
			"chart-managed CloudNativePG pods, but those pods are in this namespace, " +
			"default-deny selects them, and the chart renders no policy that admits " +
			"anything to them. On a cluster that enforces NetworkPolicy the meter cannot " +
			"reach its own ledger in this mode, and the operator cannot reach the " +
			"instances either. Found while fixing gonk-nax7 and NOT fixed with it: the " +
			"policy a CloudNativePG cluster needs (the operator's namespace, replication " +
			"between instances, the API server) is its own piece of work and has no " +
			"enforcing-cluster evidence yet. Not the default mode.",
	},
}

func selectorKey(matchLabels map[string]any) string {
	keys := make([]string, 0, len(matchLabels))
	for k := range matchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, matchLabels[k]))
	}
	return strings.Join(parts, ",")
}

// netpol is one rendered NetworkPolicy, reduced to what the pairing needs.
type netpol struct {
	Name     string
	Selector string // canonical podSelector; "" selects every pod
	Types    map[string]bool
	Spec     map[string]any
}

func netpols(objs []Object) []netpol {
	var out []netpol
	for _, o := range objs {
		if o.Kind != "NetworkPolicy" {
			continue
		}
		spec, _ := o.Raw["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		types := map[string]bool{}
		for _, t := range toStringSlice(spec["policyTypes"]) {
			types[t] = true
		}
		out = append(out, netpol{Name: o.Metadata.Name, Selector: selectorKey(ml), Types: types, Spec: spec})
	}
	return out
}

// tcpPorts returns the TCP ports a rule names; {0} when it names none (which
// in NetworkPolicy means every port).
func tcpPorts(rule map[string]any) []int {
	ports, ok := rule["ports"].([]any)
	if !ok || len(ports) == 0 {
		return []int{0}
	}
	var out []int
	for _, p := range ports {
		pm, _ := p.(map[string]any)
		proto, _ := pm["protocol"].(string)
		if proto != "" && proto != "TCP" {
			continue
		}
		if n, ok := asInt(pm["port"]); ok {
			out = append(out, n)
		}
	}
	return out
}

// sameNamespacePeers returns the canonical selectors of the peers in a rule
// that are plain podSelectors: no namespaceSelector, no ipBlock.
func sameNamespacePeers(rule map[string]any, key string) []string {
	var out []string
	peers, _ := rule[key].([]any)
	for _, p := range peers {
		pm, _ := p.(map[string]any)
		if _, cross := pm["namespaceSelector"]; cross {
			continue
		}
		ps, ok := pm["podSelector"].(map[string]any)
		if !ok {
			continue
		}
		ml, _ := ps["matchLabels"].(map[string]any)
		out = append(out, selectorKey(ml))
	}
	return out
}

// edges reads every same-namespace permission out of one direction of every
// policy: direction "egress" yields (policy's pods -> peer), "ingress" yields
// (peer -> policy's pods).
func edges(nps []netpol, direction string) []peerEdge {
	peerKey := map[string]string{"egress": "to", "ingress": "from"}[direction]
	var out []peerEdge
	for _, np := range nps {
		if np.Selector == "" {
			continue // default-deny: it has no rules
		}
		rules, _ := np.Spec[direction].([]any)
		for _, r := range rules {
			rm, _ := r.(map[string]any)
			for _, peer := range sameNamespacePeers(rm, peerKey) {
				for _, port := range tcpPorts(rm) {
					e := peerEdge{Policy: np.Name, Port: port}
					if direction == "egress" {
						e.Src, e.Dst = np.Selector, peer
					} else {
						e.Src, e.Dst = peer, np.Selector
					}
					out = append(out, e)
				}
			}
		}
	}
	return out
}

func hasEdge(es []peerEdge, want peerEdge) bool {
	for _, e := range es {
		if e.Src == want.Src && e.Dst == want.Dst && (e.Port == want.Port || e.Port == 0) {
			return true
		}
	}
	return false
}

func governing(nps []netpol, selector string) (netpol, bool) {
	for _, np := range nps {
		if np.Selector == selector {
			return np, true
		}
	}
	return netpol{}, false
}

func pairExempt(e peerEdge) bool {
	for _, ex := range pairExemptions {
		if ex.Dst == e.Dst {
			return true
		}
	}
	return false
}

// pairingRenders is every render the pairing is checked on: the golden
// profiles, plus the toggles that add or move a same-namespace rule.
func pairingRenders(t *testing.T) map[string][]Object {
	t.Helper()
	out := map[string][]Object{}
	for _, profile := range goldenProfiles {
		out[profile] = Objects(t, Render(t, Profile(profile)...))
	}
	out["minimum"] = Objects(t, Render(t, withNetpol()...))
	out["minimum+workstore"] = Objects(t, Render(t, WithWorkStore(withNetpol())...))
	out["minimum+kubeAPI"] = Objects(t, Render(t, withNetpol(
		"--set", "networkPolicy.kubeAPI.cidrs={192.0.2.10/32}")...))
	return out
}

// Every same-namespace EGRESS rule has the INGRESS rule that completes it, and
// every same-namespace INGRESS rule has the EGRESS rule that completes it.
func TestEveryInNamespaceRuleHasItsOtherHalf(t *testing.T) {
	checked := 0
	for name, objs := range pairingRenders(t) {
		nps := netpols(objs)
		if !hasDefaultDeny(objs) {
			continue
		}
		egress, ingress := edges(nps, "egress"), edges(nps, "ingress")

		for _, e := range egress {
			if pairExempt(e) {
				continue
			}
			checked++
			dst, ok := governing(nps, e.Dst)
			if !ok {
				t.Errorf("%s: NetworkPolicy/%s allows egress %s, but NO policy governs the "+
					"destination pods. gonk-default-deny selects every pod in the namespace, so "+
					"nothing admits this connection on the way IN: the egress rule is dead.",
					name, e.Policy, e)
				continue
			}
			if !dst.Types["Ingress"] {
				t.Errorf("%s: NetworkPolicy/%s allows egress %s, but NetworkPolicy/%s (which "+
					"governs the destination) has no Ingress policyType, so gonk-default-deny "+
					"is the only ingress policy on those pods and it admits nothing.",
					name, e.Policy, e, dst.Name)
				continue
			}
			if !hasEdge(ingress, e) {
				t.Errorf("%s: NetworkPolicy/%s allows egress %s, but NetworkPolicy/%s has no "+
					"ingress rule admitting that source on that port. The connection is "+
					"permitted on the way OUT and dropped on the way IN (gonk-nax7's class): "+
					"add the `from` half, or delete the egress rule.",
					name, e.Policy, e, dst.Name)
			}
		}

		for _, e := range ingress {
			checked++
			src, ok := governing(nps, e.Src)
			if !ok {
				t.Errorf("%s: NetworkPolicy/%s admits ingress %s, but NO policy governs the "+
					"source pods, so gonk-default-deny drops the connection on the way OUT: "+
					"the ingress rule is dead.", name, e.Policy, e)
				continue
			}
			if !src.Types["Egress"] {
				continue // the source's egress is not restricted beyond default-deny
			}
			if !hasEdge(egress, e) {
				t.Errorf("%s: NetworkPolicy/%s admits ingress %s, but NetworkPolicy/%s has no "+
					"egress rule towards that destination on that port. The connection is "+
					"admitted on the way IN and dropped on the way OUT (gonk-zjk7's class): "+
					"add the `to` half, or delete the ingress rule.",
					name, e.Policy, e, src.Name)
			}
		}
	}
	// agent->intake, agent->meter, controller->intake, controller->meter,
	// controller->dolt, intake->meter, intake->controller: seven pairs, both
	// directions, on one render at the very least.
	if checked < 14 {
		t.Fatalf("only %d same-namespace rules were checked across every render; the chart has "+
			"at least seven pairs. The reader is broken and this test asserted too little.", checked)
	}
}

// THE TWO PAIRS gonk-nax7 IS ABOUT, by name, so that the general test above
// cannot go green by ceasing to see them: the agent pod fetches its checkout
// from gonk-intake's private listener, and the controller registers it there.
// Both halves of both, in the default render.
func TestIntakePrivateListenerIsReachableFromTheAgentAndTheController(t *testing.T) {
	objs := Objects(t, Render(t, withNetpol()...))
	nps := netpols(objs)
	egress, ingress := edges(nps, "egress"), edges(nps, "ingress")

	intake, ok := policyFor(objs, "intake")
	if !ok {
		t.Fatal("no NetworkPolicy governs component intake")
	}
	intakeSel := selectorKey(intake["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any))
	controller, ok := policyFor(objs, "controller")
	if !ok {
		t.Fatal("no NetworkPolicy governs component controller")
	}
	controllerSel := selectorKey(controller["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any))

	const privatePort = 9090 // intake.ports.private's default
	for _, src := range []struct{ who, sel string }{
		{"the agent session pod (the checkout fetch)", "app=gc-agent"},
		{"the controller (gonk-gate registers the checkout)", controllerSel},
	} {
		want := peerEdge{Src: src.sel, Dst: intakeSel, Port: privatePort}
		if !hasEdge(egress, want) {
			t.Errorf("%s has no EGRESS rule to gonk-intake on %d", src.who, privatePort)
		}
		if !hasEdge(ingress, want) {
			t.Errorf("gonk-intake has no INGRESS rule admitting %s on %d: the egress rule on "+
				"the other side is dead and no session gets a working copy on a cluster that "+
				"enforces NetworkPolicy (gonk-nax7)", src.who, privatePort)
		}
	}

	// And the public (webhook) listener stays closed to both: the rule is for
	// the private port only.
	for _, sel := range []string{"app=gc-agent", controllerSel} {
		if hasEdge(ingress, peerEdge{Src: sel, Dst: intakeSel, Port: 8080}) {
			t.Errorf("gonk-intake admits {%s} on the PUBLIC listener; only the private one was meant", sel)
		}
	}
}

// The agent's selector is a VALUE. The ingress half must follow it, or an
// operator who changes networkPolicy.agentPodSelector gets a one-sided rule.
func TestIntakeIngressFollowsTheAgentPodSelector(t *testing.T) {
	objs := Objects(t, Render(t, withNetpol(
		"--set", "networkPolicy.agentPodSelector.app=null",
		"--set", "networkPolicy.agentPodSelector.role=session")...))
	nps := netpols(objs)
	intake, _ := policyFor(objs, "intake")
	intakeSel := selectorKey(intake["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any))
	if !hasEdge(edges(nps, "ingress"), peerEdge{Src: "role=session", Dst: intakeSel, Port: 9090}) {
		t.Fatal("gonk-intake's ingress does not follow networkPolicy.agentPodSelector")
	}
}

func TestEveryPairExemptionStillMatchesARule(t *testing.T) {
	used := map[string]bool{}
	all := pairingRenders(t)
	all["cnpg"] = Objects(t, Render(t, withNetpol("--set", "ledger.postgres.mode=cnpg")...))
	for _, objs := range all {
		for _, e := range edges(netpols(objs), "egress") {
			used[e.Dst] = true
		}
	}
	for _, ex := range pairExemptions {
		if !used[ex.Dst] {
			t.Errorf("pairExemptions has an orphan: no egress rule in any render names {%s}. "+
				"Delete it.", ex.Dst)
		}
	}
}

// ---------------------------------------------------------------------------
// DNS (gonk-6xzj)
// ---------------------------------------------------------------------------

// A resolver falls back to TCP when a response does not fit in a UDP datagram.
// The controller's policy allowed UDP 53 only while the other three allowed
// both, so on a cluster that enforces NetworkPolicy the controller alone could
// not complete a truncated lookup. Every policy that allows DNS allows it on
// both protocols.
func TestEveryDNSEgressRuleAllowsUDPAndTCP(t *testing.T) {
	for name, objs := range pairingRenders(t) {
		rules := 0
		for _, np := range netpols(objs) {
			egress, _ := np.Spec["egress"].([]any)
			for _, r := range egress {
				rm, _ := r.(map[string]any)
				protos := map[int]map[string]bool{}
				ports, _ := rm["ports"].([]any)
				for _, p := range ports {
					pm, _ := p.(map[string]any)
					n, ok := asInt(pm["port"])
					if !ok {
						continue
					}
					proto, _ := pm["protocol"].(string)
					if proto == "" {
						proto = "TCP"
					}
					if protos[n] == nil {
						protos[n] = map[string]bool{}
					}
					protos[n][proto] = true
				}
				got, isDNS := protos[53]
				if !isDNS {
					continue
				}
				rules++
				if !got["UDP"] || !got["TCP"] {
					t.Errorf("%s: NetworkPolicy/%s allows DNS on %v only. A truncated response is "+
						"retried over TCP, so the rule needs both UDP 53 and TCP 53 (gonk-6xzj).",
						name, np.Name, keysOf(got))
				}
			}
		}
		if hasDefaultDeny(objs) && rules == 0 {
			t.Errorf("%s: found no DNS egress rule in any policy; the reader is broken", name)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// THE KUBERNETES API SERVER (gonk-vxel)
// ---------------------------------------------------------------------------
//
// The API server is not a pod, so no selector names it; an egress rule can
// only name it by address. The chart cannot know that address, so it is a
// value: networkPolicy.kubeAPI.cidrs, with networkPolicy.kubeAPI.ports.
//
// Which pods get the rule is DERIVED: a workload whose ServiceAccount is the
// subject of a RoleBinding calls the API, and a workload without one does not.

// ipBlockRules returns the egress rules of a policy that name an ipBlock, as
// "cidr,cidr|port,port".
func ipBlockRules(np netpol) []string {
	var out []string
	egress, _ := np.Spec["egress"].([]any)
	for _, r := range egress {
		rm, _ := r.(map[string]any)
		var cidrs []string
		tos, _ := rm["to"].([]any)
		for _, to := range tos {
			tm, _ := to.(map[string]any)
			if ib, ok := tm["ipBlock"].(map[string]any); ok {
				c, _ := ib["cidr"].(string)
				cidrs = append(cidrs, c)
			}
		}
		if len(cidrs) == 0 {
			continue
		}
		var ports []string
		for _, p := range tcpPorts(rm) {
			ports = append(ports, fmt.Sprint(p))
		}
		out = append(out, strings.Join(cidrs, ",")+"|"+strings.Join(ports, ","))
	}
	return out
}

// apiCallers returns the component of every rendered workload whose
// ServiceAccount is bound to a Role.
func apiCallers(objs []Object) map[string]bool {
	bound := map[string]bool{}
	for _, o := range objs {
		if o.Kind != "RoleBinding" {
			continue
		}
		subjects, _ := o.Raw["subjects"].([]any)
		for _, s := range subjects {
			sm, _ := s.(map[string]any)
			if sm["kind"] == "ServiceAccount" {
				name, _ := sm["name"].(string)
				bound[name] = true
			}
		}
	}
	out := map[string]bool{}
	for _, o := range objs {
		if o.Kind != "Deployment" && o.Kind != "StatefulSet" {
			continue
		}
		spec, _ := o.Raw["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		pod, _ := tmpl["spec"].(map[string]any)
		sa, _ := pod["serviceAccountName"].(string)
		if bound[sa] {
			out[o.Metadata.Labels[componentLabel]] = true
		}
	}
	return out
}

const testAPICIDRs = "networkPolicy.kubeAPI.cidrs={192.0.2.10/32,192.0.2.11/32}"

// With the address set, every component that calls the API gets an egress rule
// to exactly those addresses on exactly kubeAPI.ports, and nothing else does.
// The agent session pods above all: a session that can reach the API server
// has one more thing to attack and no reason to reach it (its ServiceAccount
// token is not even mounted, pkg/runtime/jobs/spec.go).
func TestKubeAPIEgressGoesToTheAPICallersAndNobodyElse(t *testing.T) {
	objs := Objects(t, Render(t, withNetpol(
		"--set", testAPICIDRs,
		"--set", "networkPolicy.kubeAPI.ports={6443}")...))
	callers := apiCallers(objs)
	if !callers["controller"] || !callers["meter"] {
		t.Fatalf("derived API callers = %v; the controller (session Jobs, Secrets) and the "+
			"meter (the KeySink) both hold RoleBindings, so the derivation is broken", callers)
	}
	const want = "192.0.2.10/32,192.0.2.11/32|6443"
	seen := 0
	for _, np := range netpols(objs) {
		if np.Selector == "" {
			if len(ipBlockRules(np)) != 0 {
				t.Errorf("gonk-default-deny carries an ipBlock rule; it must stay a pure deny")
			}
			continue
		}
		spec := np.Spec["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
		comp, _ := spec[componentLabel].(string)
		got := ipBlockRules(np)
		if callers[comp] {
			seen++
			if !reflect.DeepEqual(got, []string{want}) {
				t.Errorf("NetworkPolicy/%s governs an API caller (%s) but its ipBlock egress is "+
					"%v, want exactly [%s]. Without the rule default-deny drops every call to "+
					"the API server on a cluster that enforces NetworkPolicy: no session Job is "+
					"created and no key Secret is written (gonk-vxel).", np.Name, comp, got, want)
			}
			continue
		}
		if len(got) != 0 {
			t.Errorf("NetworkPolicy/%s does not govern an API caller, but carries ipBlock egress "+
				"%v. The API server allowance is for the pods that hold a RoleBinding only.",
				np.Name, got)
		}
	}
	if seen != len(callers) {
		t.Errorf("%d API callers (%v) but only %d of them have a NetworkPolicy", len(callers), callers, seen)
	}
}

// THE DEFAULT ADDS NOTHING. An install that does not set the value renders the
// policies it rendered before: no ipBlock anywhere, so nothing is opened by
// upgrading, and nothing changes on a cluster that does not enforce.
func TestKubeAPIEgressIsAbsentUntilTheAddressIsSet(t *testing.T) {
	for _, np := range netpols(Objects(t, Render(t, withNetpol()...))) {
		if got := ipBlockRules(np); len(got) != 0 {
			t.Errorf("NetworkPolicy/%s renders ipBlock egress %v with networkPolicy.kubeAPI.cidrs "+
				"unset. The chart cannot guess the API server's address, and a guess wide "+
				"enough to be right everywhere is an allow-all.", np.Name, got)
		}
	}
}

// ...AND THE INSTALL NOTES SAY SO. With the address unset the policies are
// correct only on a cluster that does not enforce them, and the operator who
// is about to find that out should read it first.
func TestNotesWarnWhenTheAPIServerIsNotAllowed(t *testing.T) {
	notes, _ := notesRender(t, withNetpol()...)
	for _, want := range []string{"networkPolicy.kubeAPI.cidrs", "API server", "no session"} {
		if !strings.Contains(notes, want) {
			t.Errorf("with networkPolicy.kubeAPI.cidrs unset the install notes do not say %q:\n%s", want, notes)
		}
	}
	set, _ := notesRender(t, withNetpol("--set", testAPICIDRs)...)
	if strings.Contains(set, "networkPolicy.kubeAPI.cidrs is empty") {
		t.Errorf("the notes still warn after the address was set:\n%s", set)
	}
	if !strings.Contains(set, "192.0.2.10/32") {
		t.Errorf("the notes do not echo the API server addresses the policies allow:\n%s", set)
	}
	off, _ := notesRender(t, append(Minimum(), "--set", "networkPolicy.enabled=false")...)
	if strings.Contains(off, "networkPolicy.kubeAPI") {
		t.Errorf("networkPolicy.enabled=false renders no policy, so there is nothing to warn about:\n%s", off)
	}
}

// DEFAULT-DENY MUST NOT BECOME ALLOW-ALL THROUGH THIS VALUE. 0.0.0.0/0 on the
// API port is egress to every host on the internet that listens there, from the
// pod that holds the forge token and the LiteLLM admin key.
func TestGuardKubeAPICIDRMustNotBeEverything(t *testing.T) {
	for _, cidr := range []string{"0.0.0.0/0", "::/0", "192.0.2.0/0"} {
		mustFail(t, "networkPolicy.kubeAPI.cidrs",
			"--set", "networkPolicy.kubeAPI.cidrs={"+cidr+"}")
	}
}

// ...AND NOT IN TWO HALVES EITHER. Until chart 0.2.4 the guard refused only a
// literal /0, so `0.0.0.0/1` with `128.0.0.0/1` -- every address, in two
// entries -- rendered. The guard refuses a prefix shorter than /8 (IPv4) or
// /16 (IPv6): no control plane is that wide. It is a floor, not a proof: 256
// entries of /8 still cover everything, and nobody writes those by accident.
func TestGuardKubeAPICIDRMustNotBeNearlyEverything(t *testing.T) {
	for _, cidrs := range []string{
		"0.0.0.0/1,128.0.0.0/1", // the pair that passed
		"128.0.0.0/1",           // the half the old guard's first failure would not have named
		"198.0.0.0/7",
		"::/1,8000::/1",
		"2000::/3",
		"2001::/15",
		"192.0.2.10/32,0.0.0.0/4", // one honest entry does not excuse the other
	} {
		mustFail(t, "networkPolicy.kubeAPI.cidrs",
			"--set", "networkPolicy.kubeAPI.cidrs={"+cidrs+"}")
	}
	// At the floor and narrower it renders: a /8, a control-plane subnet, a
	// host route, and IPv6 at /16 and at /128.
	for _, cidrs := range []string{"198.0.0.0/8", "192.0.2.0/24", "192.0.2.10/32", "2001::/16", "2001:db8::/64", "2001:db8::10/128"} {
		Render(t, withNetpol("--set", "networkPolicy.kubeAPI.cidrs={"+cidrs+"}")...)
	}
}

// A bare address, a hostname or a typo is not a CIDR. The API server would
// reject the NetworkPolicy at install time; the render refuses it first.
func TestKubeAPICIDRsMustBeCIDRs(t *testing.T) {
	for _, bad := range []string{"192.0.2.10", "kubernetes.default.svc", "192.0.2.10/"} {
		if _, err := RenderErr(t, withNetpol("--set", "networkPolicy.kubeAPI.cidrs={"+bad+"}")...); err == nil {
			t.Errorf("networkPolicy.kubeAPI.cidrs={%s} rendered; it is not a CIDR", bad)
		}
	}
	// IPv6 is a CIDR too.
	Render(t, withNetpol("--set", "networkPolicy.kubeAPI.cidrs={2001:db8::10/128}")...)
}

// An ipBlock rule with no ports allows EVERY port on those addresses.
func TestGuardKubeAPINeedsAPort(t *testing.T) {
	mustFail(t, "networkPolicy.kubeAPI.ports",
		"--set", testAPICIDRs, "--set", "networkPolicy.kubeAPI.ports=null")
}

// ---------------------------------------------------------------------------
// RBAC: WHAT THE DOCUMENTS SAY IS WHAT RENDERS (gonk-6by9)
// ---------------------------------------------------------------------------

// unwrap joins wrapped prose into one line and drops YAML comment markers, so
// a phrase is found wherever the line breaks fall.
func unwrap(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "#", " ")), " ")
}

func chartFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(SourceChartDir(), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// values.yaml said gonk-meter's Role was "scoped to this prefix"
// (secrets.keySink.namePrefix). It is not and cannot be: RBAC has no name
// prefixes, `create` cannot be restricted by resourceNames at all, and the
// names are derived from project paths that do not exist at install time. The
// Role is a namespace-wide create/update/delete on Secrets.
//
// This holds the two together. While the Role's secrets rule carries no
// resourceNames, values.yaml may not describe it as scoped by name; if the
// rule ever does gain resourceNames, this test fails and must be rewritten to
// hold whatever the new claim is.
func TestValuesDoNotClaimTheKeySinkRoleIsScopedByName(t *testing.T) {
	role := MustObject(t, Render(t, Minimum()...), "Role", "gonk-meter")
	for _, rule := range toSliceOfMaps(t, role.Raw["rules"]) {
		if !containsStr(toStringSlice(rule["resources"]), "secrets") {
			continue
		}
		if names := toStringSlice(rule["resourceNames"]); len(names) != 0 {
			t.Fatalf("Role gonk-meter's secrets rule now carries resourceNames %v. Rewrite this "+
				"test and the values.yaml comment to describe the new scope.", names)
		}
	}
	values := chartFile(t, "values.yaml")
	_, after, ok := strings.Cut(values, "\n  keySink:\n")
	if !ok {
		t.Fatal("values.yaml has no secrets.keySink block; this test is reading the wrong thing")
	}
	before := values[:len(values)-len(after)]
	start := strings.LastIndex(before, "\n  rotation:")
	if start < 0 {
		t.Fatal("cannot find the comment above secrets.keySink")
	}
	comment := unwrap(strings.ToLower(before[start:]))
	for _, claim := range []string{"scoped to this prefix", "scoped to the prefix", "limited to this prefix", "only secrets named"} {
		if strings.Contains(comment, claim) {
			t.Errorf("values.yaml says the meter's Role is %q, but the rendered Role grants "+
				"create/update/delete on EVERY Secret in the namespace (no resourceNames).", claim)
		}
	}
	for _, want := range []string{"every secret in", "not scoped"} {
		if !strings.Contains(comment, want) {
			t.Errorf("the comment above secrets.keySink does not say %q: an operator reading "+
				"values.yaml must learn the real extent of the grant there.", want)
		}
	}
}

// The chart README describes every Role the chart can render: its name, the
// ServiceAccount it is bound to, and every resource and verb it grants. Read
// from the render, so a Role or a verb added later fails here until the README
// says so.
func TestREADMEDescribesEveryRoleTheChartRenders(t *testing.T) {
	readme := chartFile(t, "README.md")
	_, section, ok := strings.Cut(readme, "## 12. RBAC")
	if !ok {
		t.Fatal("chart/gonk/README.md has no section '## 12. RBAC'")
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	objs := Objects(t, Render(t, append(Minimum(), "--set", "podReap.enabled=true")...))

	boundTo := map[string]string{}
	for _, o := range objs {
		if o.Kind == "ClusterRole" || o.Kind == "ClusterRoleBinding" {
			t.Fatalf("the chart renders %s/%s; the README says it renders none", o.Kind, o.Metadata.Name)
		}
		if o.Kind != "RoleBinding" {
			continue
		}
		ref, _ := o.Raw["roleRef"].(map[string]any)
		role, _ := ref["name"].(string)
		subjects, _ := o.Raw["subjects"].([]any)
		for _, s := range subjects {
			sm, _ := s.(map[string]any)
			sa, _ := sm["name"].(string)
			boundTo[role] = sa
		}
	}

	roles := 0
	for _, o := range objs {
		if o.Kind != "Role" {
			continue
		}
		roles++
		// One table row per Role: "| `name` | `serviceaccount` | grants |".
		var row string
		for _, line := range strings.Split(section, "\n") {
			if strings.HasPrefix(line, "| `"+o.Metadata.Name+"` |") {
				row = line
			}
		}
		if row == "" {
			t.Errorf("README section 12 has no table row for Role %s", o.Metadata.Name)
			continue
		}
		if sa := boundTo[o.Metadata.Name]; !strings.Contains(row, "`"+sa+"`") {
			t.Errorf("README row for Role %s does not name the ServiceAccount it is bound to (%s)", o.Metadata.Name, sa)
		}
		for _, rule := range toSliceOfMaps(t, o.Raw["rules"]) {
			verbs := toStringSlice(rule["verbs"])
			sort.Strings(verbs)
			for _, res := range toStringSlice(rule["resources"]) {
				want := "`" + res + "`: " + strings.Join(verbs, ", ")
				if !strings.Contains(row, want) {
					t.Errorf("README row for Role %s does not say %q (the rendered grant).\nrow: %s",
						o.Metadata.Name, want, row)
				}
			}
		}
	}
	// gc-agent, gc-controller, gonk-gate-jobs, gonk-gate-podreap, gonk-meter.
	if roles < 5 {
		t.Fatalf("only %d Roles rendered, want at least 5: the loop above checked too little", roles)
	}
	// The two grants a reader most needs to see are stated in words as well.
	for _, want := range []string{"every Secret in the namespace", "resourceNames"} {
		if !strings.Contains(unwrap(section), want) {
			t.Errorf("README section 12 does not say %q", want)
		}
	}
}
