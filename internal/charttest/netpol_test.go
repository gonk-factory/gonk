//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// The default assumes GitLab is IN-CLUSTER (namespace `gitlab`, OD-6), so the
// default profile uses a namespaceSelector, NOT an ipBlock: a CIDR for an
// in-cluster Service is a pod IP, and pod IPs move on every reschedule -- the
// policy would work until the first restart and then silently stop allowing
// GitLab. The `gitlab.cidrs` path stays for genuinely-external GitLabs and is
// exercised by withExternalGitLab below.
func withNetpol(extra ...string) []string {
	// Minimum() already sets dolt.image.tag (the bundled factory), so nothing extra
	// is needed for the ledger here.
	return append(append(Minimum(),
		"--set", "networkPolicy.gitlabInCluster.enabled=true",
		"--set", "networkPolicy.gitlabInCluster.namespaceSelector.kubernetes\\.io/metadata\\.name=gitlab"), extra...)
}

func withExternalGitLab(extra ...string) []string {
	// RECONCILED, Task 0: networkPolicy.gitlabInCluster.enabled now DEFAULTS to
	// true (OD-6: the default assumes an in-cluster GitLab), so
	// exercising the external (ipBlock) path requires explicitly turning the
	// in-cluster path back off -- it is no longer off by default.
	return append(append(Minimum(),
		"--set", "networkPolicy.gitlabInCluster.enabled=false",
		"--set", "networkPolicy.gitlab.cidrs={192.0.2.7/32}"), extra...)
}

// EVERY policy must carry the enforcement banner, as an ANNOTATION (object
// data: it survives to `kubectl get -o yaml`, which a comment does not). It is
// the ONLY thing between a reader of the live object and the conclusion that
// the egress guarantee holds on a cluster whose CNI may not enforce it.
// Deleting it is a documentation regression with a security blast radius, so it
// is a TEST, not a convention.
//
// THE BANNER STATES A CONDITION, NOT ONE CLUSTER'S STATE (gonk-z6js). It used
// to say "NOT ENFORCED on this cluster" and name the CNI and the gitops path of
// the maintainers' own deployment -- false on every cluster that does enforce,
// and nobody else's business on the rest. So this also refuses those words.
//
// (Named TestEveryNetworkPolicyDeclaresItIsNotEnforced until gonk-z6js.)
func TestEveryNetworkPolicyDeclaresEnforcementDependsOnTheCNI(t *testing.T) {
	const key = "gonk-factory.github.io/network-policy-enforcement"
	policies := 0
	for _, o := range Objects(t, Render(t, withNetpol()...)) {
		if o.Kind != "NetworkPolicy" {
			continue
		}
		policies++
		banner := o.Metadata.Annotations[key]
		for _, want := range []string{"only where", "CNI", "blocks nothing", "advisory", "helm test"} {
			if !strings.Contains(banner, want) {
				t.Errorf("NetworkPolicy/%s: annotation %s does not say %q.\n"+
					"A NetworkPolicy is enforced only where the CNI implements it, and anyone reading "+
					"the live object must be told so. Got: %q",
					o.Metadata.Name, key, want, banner)
			}
		}
		for _, siteWord := range []string{"this cluster", "Flannel", "flannel", "Cilium", "SUSPENDED", "gitops:", "HelmRelease", "today"} {
			if strings.Contains(o.Doc, siteWord) {
				t.Errorf("NetworkPolicy/%s carries %q: the banner must state the general condition, "+
					"not the state of one cluster.", o.Metadata.Name, siteWord)
			}
		}
	}
	// default-deny, agent, intake, meter, controller, dolt.
	if policies < 6 {
		t.Fatalf("only %d NetworkPolicies rendered, want at least 6: the loop above checked too little", policies)
	}
}

// A default-deny policy is the only thing that makes the ALLOW policies mean
// anything. Without it, "allow egress to GitLab" adds a permission to a pod that
// already had every permission.
func TestDefaultDenyExists(t *testing.T) {
	np := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-default-deny")
	for _, want := range []string{"Ingress", "Egress"} {
		if !strings.Contains(np.Doc, want) {
			t.Errorf("default-deny does not cover %s", want)
		}
	}
	if !strings.Contains(np.Doc, "podSelector: {}") {
		t.Error("default-deny does not select every pod in the namespace")
	}
}

// THE spec-9 policy.
//
// This test proves the policy RENDERS CORRECTLY. It does NOT prove a packet is
// dropped: that depends on the cluster's CNI, and the proof is the enforcement
// probe (`helm test`) and the netpol-enforcement workflow. Do not mistake this
// green for that one.
func TestAgentEgressIsLimitedToGitLabLiteLLMAndDNS(t *testing.T) {
	np := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-agent")
	// GitLab egress is OFF by default (gonk-7oz). This test used to assert the
	// opposite -- that agents CAN reach GitLab -- which encoded the v1 assumption
	// that the agent clones the rig and posts comments itself. Under the broker
	// design the controller does all forge I/O and the checkout is served by
	// gonk-intake, so an agent that can reach the forge is a hole, not a feature.
	//
	// Asserted on the SPEC (the rendered egress rules), not the whole document:
	// the explanatory comment in the template legitimately contains the word
	// "gitlab", so a substring check over np.Doc would pass for the wrong reason.
	if egressMentions(np, "gitlab") {
		t.Errorf("the agent policy still grants egress to GitLab. The pod holds no forge\n"+
			"credentials by design, so this rule's only effect is to remove the second\n"+
			"line of defence if a credential ever reaches it (gonk-7oz):\n%s", np.Doc)
	}
	if !strings.Contains(np.Doc, "litellm") {
		t.Error("agents cannot reach LiteLLM -- which is the ONLY door to a model")
	}
	if !strings.Contains(np.Doc, "port: 53") {
		t.Error("agents cannot resolve DNS, so they can reach nothing at all")
	}
	// The whole point: no blanket egress.
	if strings.Contains(np.Doc, "0.0.0.0/0") {
		t.Fatal("the agent policy allows 0.0.0.0/0. Cloud models would then be reachable OUTSIDE LiteLLM, and budgets could be bypassed -- which is the one thing spec 9 exists to prevent.")
	}
	if !strings.Contains(np.Doc, "agent-session") {
		t.Errorf("the agent policy does not select the agent pods:\n%s", np.Doc)
	}
}

// The public listener may be reached from the ingress controller -- by default
// the namespace `traefik` (networkPolicy.ingressController). The PRIVATE one
// (metrics, health, the checkout routes and the UNAUTHENTICATED
// POST /admin/reconcile) may be reached by Prometheus, the controller and the
// agent session pods (gonk-nax7: the last two use the checkout, and a policy
// cannot admit them to one route only).
func TestIntakeIngressIsLimited(t *testing.T) {
	np := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-intake")
	if !strings.Contains(np.Doc, "traefik") {
		t.Error("the ingress controller (traefik) cannot reach the webhook listener")
	}
	if !strings.Contains(np.Doc, "8080") || !strings.Contains(np.Doc, "9090") {
		t.Errorf("both listeners must be covered:\n%s", np.Doc)
	}
}

// The external-GitLab path must still render, lint and validate -- it is the only
// thing an operator whose GitLab is NOT in-cluster can use.
//
// It now requires networkPolicy.gitlabEgress.enabled, because agent->forge
// egress is off by default (gonk-7oz). The path must keep WORKING for an
// operator who deliberately re-enables it; it must simply not be the default.
func TestExternalGitLabEgressStillRenders(t *testing.T) {
	args := append(withExternalGitLab(), "--set", "networkPolicy.gitlabEgress.enabled=true")
	np := MustObject(t, Render(t, args...), "NetworkPolicy", "gonk-agent")
	if !strings.Contains(np.Doc, "192.0.2.7/32") {
		t.Error("the external-GitLab (ipBlock) path does not render when explicitly enabled")
	}
}

// The escape hatch must be exactly that: OFF unless asked for, and PORT-SCOPED
// when asked for. The in-cluster branch previously emitted no `ports:` at all,
// which is what allowed the whole gitlab namespace on EVERY port (gonk-7oz).
func TestGitLabEgressIsOffByDefaultAndPortScopedWhenEnabled(t *testing.T) {
	off := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-agent")
	if egressMentions(off, "gitlab") {
		t.Error("gitlabEgress must be off by default")
	}

	args := append(withNetpol(), "--set", "networkPolicy.gitlabEgress.enabled=true")
	on := MustObject(t, Render(t, args...), "NetworkPolicy", "gonk-agent")
	if !egressMentions(on, "gitlab") {
		t.Fatalf("the escape hatch does not restore GitLab egress when enabled:\n%s", on.Doc)
	}
	for _, rule := range egressRules(on) {
		if !strings.Contains(rule, "gitlab") {
			continue
		}
		if !strings.Contains(rule, "port:") {
			t.Errorf("the GitLab egress rule opens EVERY port; it must be scoped to\n"+
				"networkPolicy.gitlab.ports even when deliberately enabled:\n%s", rule)
		}
	}
}

// The agent fetches its per-session checkout from gonk-intake's PRIVATE
// listener. That is the replacement for forge egress: the bytes come from gonk,
// so the pod still holds no forge credentials (gonk-msz).
func TestAgentMayReachIntakePrivateListenerForTheCheckout(t *testing.T) {
	np := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-agent")
	var found bool
	for _, rule := range egressRules(np) {
		if strings.Contains(rule, "component: intake") && strings.Contains(rule, "port: 9090") {
			found = true
		}
	}
	if !found {
		t.Errorf("agents cannot reach gonk-intake:9090, so the per-session checkout\n"+
			"can never be fetched and scaffold is back to describing a repo it never saw:\n%s", np.Doc)
	}
}

// egressRules splits a rendered NetworkPolicy's egress list into one string per
// rule, so a test can assert about the rule that matched rather than about the
// whole document (whose COMMENTS mention hosts the policy deliberately denies).
func egressRules(np Object) []string {
	_, after, ok := strings.Cut(np.Doc, "egress:")
	if !ok {
		return nil
	}
	var rules []string
	for _, part := range strings.Split(after, "\n    - to:") {
		if strings.TrimSpace(part) != "" {
			rules = append(rules, part)
		}
	}
	return rules
}

// egressMentions reports whether any egress RULE (not comment) names target.
func egressMentions(np Object, target string) bool {
	for _, rule := range egressRules(np) {
		// Strip comment lines: the template explains at length why certain
		// destinations are denied, and those explanations name them.
		for _, line := range strings.Split(rule, "\n") {
			if t := strings.TrimSpace(line); !strings.HasPrefix(t, "#") && strings.Contains(t, target) {
				return true
			}
		}
	}
	return false
}

// Meter is reachable from intake and from the pack. With the controller BUNDLED
// (the default) the pack is in THIS namespace (component: controller); with a BYO
// controller it is the gascity namespaceSelector. Nothing else may ask for a
// budget decision.
func TestMeterIngressIsLimited(t *testing.T) {
	np := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-meter")
	if !strings.Contains(np.Doc, "component: controller") {
		t.Error("the bundled controller (the pack) cannot reach /policy/decide, so no session could ever spawn")
	}
	if !strings.Contains(np.Doc, "component: intake") && !strings.Contains(np.Doc, "intake") {
		t.Error("intake cannot reach meter")
	}
	// BYO controller: the gascity namespaceSelector path.
	byo := MustObject(t, Render(t, withNetpol(
		"--set", "gascity.enabled=false",
		"--set", "gascity.supervisorURL=http://gc.external.svc:8372")...),
		"NetworkPolicy", "gonk-meter")
	if !strings.Contains(byo.Doc, "gascity") {
		t.Error("BYO controller: the gascity namespaceSelector path is missing")
	}
}

// RECONCILED, Task 0: NEW. The default ledger.postgres.mode is `shared`
// (tenancy on an existing CloudNativePG cluster), and the
// original egress rule only covered mode=cnpg -- meter's DEFAULT profile
// would have shipped a NetworkPolicy with no path to its own ledger at all.
// Assert both the default (shared) and the cnpg mode render a real egress
// target, and that they are NOT the same rule.
func TestMeterEgressReachesTheLedgerInEveryPostgresMode(t *testing.T) {
	shared := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-meter")
	if !strings.Contains(shared.Doc, "databases-app") {
		t.Fatalf("default profile (ledger.postgres.mode=shared): meter has no egress path to its own ledger:\n%s", shared.Doc)
	}
	cnpg := MustObject(t, Render(t, withNetpol("--set", "ledger.postgres.mode=cnpg")...), "NetworkPolicy", "gonk-meter")
	if !strings.Contains(cnpg.Doc, "cnpg.io/cluster: gonk-ledger-postgres") {
		t.Fatalf("ledger.postgres.mode=cnpg: meter has no egress path to the chart-managed Cluster:\n%s", cnpg.Doc)
	}
}

// RECONCILED, Task 0: the bundled Dolt accepts connections from the Gas City
// controller (beads) and NOBODY ELSE -- gonk-meter is explicitly NOT admitted
// any more. Its ledger is Postgres/CNPG, unconditionally (ADR-004 Decision
// 10), so it never needs to reach the Dolt Service at all; admitting it would
// be a NetworkPolicy that grants an access path nothing ever uses.
func TestDoltIngressIsControllerOnlyNotMeter(t *testing.T) {
	np := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-dolt")
	if strings.Contains(np.Doc, "component: meter") {
		t.Fatalf("the Dolt policy admits meter, but meter's ledger is Postgres/CNPG -- it has no legitimate reason to reach Dolt:\n%s", np.Doc)
	}
	if !strings.Contains(np.Doc, "component: controller") {
		t.Fatalf("the Dolt policy does not admit the controller (beads):\n%s", np.Doc)
	}
}

func TestNetworkPolicyCanBeDisabledWholesale(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "networkPolicy.enabled=false")...)
	for _, o := range Objects(t, out) {
		if o.Kind == "NetworkPolicy" {
			t.Fatalf("networkPolicy.enabled=false still rendered %s", o.Metadata.Name)
		}
	}
}

func TestControllerPolicyRendersWhenBundled(t *testing.T) {
	np := MustObject(t, Render(t, withNetpol()...), "NetworkPolicy", "gonk-controller")
	if np.Metadata.Annotations["gonk-factory.github.io/network-policy-enforcement"] == "" {
		t.Error("the controller policy lost its enforcement banner")
	}
	// BYO controller -> no in-namespace controller policy.
	byo := Render(t, withNetpol(
		"--set", "gascity.enabled=false",
		"--set", "gascity.supervisorURL=http://gc.external.svc:8372")...)
	NoObject(t, byo, "NetworkPolicy", "gonk-controller")
}

// THE CHECKOUT WIRING, both halves.
//
// The per-session checkout works only because the URL is split: a per-INSTALL
// base that can ride static config, and a per-SESSION alias that Gas City
// already puts in every agent pod's env (GC_ALIAS). If either half stops being
// rendered, a pooled pod silently runs with no working copy -- and scaffold
// quietly falls back instead of failing, so nothing would go red.
func TestRigBaseURLReachesBothTheControllerAndTheAgent(t *testing.T) {
	objs := Render(t, withNetpol()...)

	// Half one: gonk-gate runs in the controller and REGISTERS the grant.
	ctl := MustObject(t, objs, "Deployment", "gonk-controller")
	if !strings.Contains(ctl.Doc, "GONK_RIG_BASE_URL") {
		t.Errorf("the controller has no GONK_RIG_BASE_URL, so gonk-gate can never register a checkout:\n%s", ctl.Doc)
	}
	if !strings.Contains(ctl.Doc, "gonk-intake-internal") {
		t.Error("GONK_RIG_BASE_URL does not point at intake's private listener")
	}

	// Half two: the bootstrap-city initContainer writes the same base into every
	// agent.toml [env], which is how it reaches the pod at all.
	if !strings.Contains(ctl.Doc, `printf "GONK_RIG_BASE_URL = `) {
		t.Errorf("bootstrap-city does not inject GONK_RIG_BASE_URL into agent.toml, so the\n" +
			"agent pod cannot compose its checkout URL from its own GC_ALIAS")
	}
}
