//go:build chart

package charttest

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE CLASS (gonk-zjk7)
// ---------------------------------------------------------------------------
//
// gonk-zjk7 was not "the controller policy forgot the meter". The class is: a
// pod can be TOLD to call an in-cluster Service -- a `*_URL` in its own env,
// put there by this chart -- while the NetworkPolicy that governs it does not
// permit the call, and nothing in the build notices. The two facts live in
// different files and, until this test, only ever met in a running cluster
// that enforces policy. On a cluster whose CNI enforces no NetworkPolicy there
// is no runtime signal at all, so the build is the only place the mismatch can
// be caught.
//
// Three instances existed when this was written, all found by this test:
//   - controller -> meter:     GONK_METER_URL, no egress rule. gonk-zjk7.
//   - controller -> intake:    GONK_RIG_BASE_URL, no egress rule.
//   - intake     -> controller: GONK_SUPERVISOR_URL; the only rule named the
//     `gascity` NAMESPACE on port 8080, while the bundled controller is in the
//     release namespace on 9443 -- a rule that permitted nothing the default
//     profile dials.
//
// Nothing below is a list of expected rules, and nothing hardcodes 8080, 9090
// or 9443. The URL yields host+port; the host resolves to a Service THIS CHART
// renders; the Service's selector yields the destination component; the egress
// of the policy governing the SOURCE pod must name that component on that
// port. A new URL env var is covered the day it is added, without editing this
// file.
//
// SCOPE -- stated so nobody over-trusts the green:
//   - EGRESS ONLY, which is what gonk-zjk7 asked for. Reachability also needs
//     the destination's INGRESS half. That half is held by
//     TestEveryInNamespaceRuleHasItsOtherHalf (netpol_pairs_test.go), which
//     reads the policies alone and pairs every same-namespace rule with its
//     mirror; it found the two dead rules towards gonk-intake's private
//     listener (gonk-nax7).
//   - IN-CHART SERVICES ONLY. A URL outside the release (LiteLLM, GitLab)
//     resolves to no Service in the render and is skipped: those rules are
//     namespaceSelector-shaped and cannot be derived from a URL.
//   - RENDERED SHAPE ONLY. This proves the RULE exists. It does not prove a
//     packet arrives: that depends on the cluster's CNI. The enforcement
//     probe (`helm test`) and the netpol-enforcement workflow are what do.

const componentLabel = "app.kubernetes.io/component"

// dialExemption records a `*_URL` that names an in-chart Service the reading
// pod is NOT required to have an egress rule for. The default is the opposite:
// an unlisted URL MUST have a rule, so a new env var fails closed.
//
// `self` exemptions are machine-checked (see checkExemption): the destination
// component must equal the source component AND the source workload must be a
// single replica, so the exemption stops holding the moment either changes.
type dialExemption struct {
	Workload string // source workload name
	Env      string // env var on that workload
	Self     bool   // the pod dials its own Service
	Why      string
}

var dialExemptions = []dialExemption{
	{
		Workload: "gonk-controller",
		Env:      "GONK_SUPERVISOR_URL",
		Self:     true,
		Why: "The in-pod gonk-gate POSTs /v0/city/gonk/sessions to the city [api] -- " +
			"which is THIS pod, via its own Service. Kubernetes NetworkPolicy does not " +
			"govern traffic from a pod to itself, and the controller workload is pinned " +
			"to replicas: 1 by the template, so there is no sibling to reach either. A " +
			"rule here would grant nothing. The `Self` check below re-proves both halves " +
			"on every render, so raising the replica count turns this exemption red " +
			"rather than leaving it silently wrong.",
	},
}

// chartService is one Service the chart renders, keyed by the DNS names a URL
// in this chart may legitimately use for it.
type chartService struct {
	Name      string
	Component string
	Ports     map[int]bool
}

// dial is one resolved `*_URL` -> in-chart Service edge.
type dial struct {
	Workload    string
	SrcComp     string
	SrcReplicas int
	Env         string
	RawURL      string
	Svc         chartService
	Port        int
}

// ---------------------------------------------------------------------------
// THE TESTS
// ---------------------------------------------------------------------------

// Every in-chart Service a pod is configured to call must appear in that pod's
// own egress rules, on the port the URL names.
func TestInClusterDialsHaveEgress(t *testing.T) {
	for _, profile := range goldenProfiles {
		t.Run(profile, func(t *testing.T) {
			objs := Objects(t, Render(t, Profile(profile)...))
			if !hasDefaultDeny(objs) {
				t.Skipf("%s renders no default-deny policy (networkPolicy disabled), so there is nothing to permit", profile)
			}
			dials := resolveDials(t, objs)
			if len(dials) == 0 {
				t.Fatalf("%s: resolved NO in-chart *_URL dials at all. The chart sets several "+
					"(GONK_METER_URL, GONK_RIG_BASE_URL, GONK_SUPERVISOR_URL); resolving none "+
					"means the resolver is broken and this test is asserting nothing.", profile)
			}
			for _, d := range dials {
				if ex, ok := exemptionFor(d); ok {
					checkExemption(t, d, ex)
					continue
				}
				np, ok := policyFor(objs, d.SrcComp)
				if !ok {
					t.Errorf("%s: %s is configured to call %s (%s=%s) but NO NetworkPolicy "+
						"governs component %q, so default-deny drops it with nothing to override.",
						profile, d.Workload, d.Svc.Name, d.Env, d.RawURL, d.SrcComp)
					continue
				}
				if !egressPermits(np, d.Svc.Component, d.Port) {
					t.Errorf("%s: %s is configured to call %s (%s=%s -> component %q, port %d), "+
						"but NetworkPolicy/%s has no egress rule permitting it. With default-deny "+
						"covering Egress, every one of those calls is dropped on the way OUT.\n"+
						"This is gonk-zjk7's class: the destination's own policy may well admit "+
						"the caller -- a one-sided permission is not a permission.\n"+
						"Egress rules present:\n%s",
						profile, d.Workload, d.Svc.Name, d.Env, d.RawURL, d.Svc.Component, d.Port,
						npName(np), describeEgress(np))
				}
			}
		})
	}
}

// The URL must also name a port the destination Service actually serves. The
// intake -> controller rule was wrong in exactly this shape before gonk-zjk7:
// it named a port nothing listened on, so it read as a permission and was not
// one.
func TestInClusterDialsNameAPortTheServiceServes(t *testing.T) {
	for _, profile := range goldenProfiles {
		t.Run(profile, func(t *testing.T) {
			objs := Objects(t, Render(t, Profile(profile)...))
			dials := resolveDials(t, objs)
			if len(dials) == 0 {
				t.Fatalf("%s: resolved NO in-chart *_URL dials at all, so this test asserted nothing", profile)
			}
			for _, d := range dials {
				if !d.Svc.Ports[d.Port] {
					t.Errorf("%s: %s sets %s=%s, but Service/%s serves no port %d (it serves %v). "+
						"The URL points at a door that is not there.",
						profile, d.Workload, d.Env, d.RawURL, d.Svc.Name, d.Port, sortedPorts(d.Svc.Ports))
				}
			}
		})
	}
}

// An exemption that matches nothing is an exemption that will silently cover
// the next env var to take that name. Orphans are a build failure.
func TestEveryDialExemptionStillMatchesARenderedEnvVar(t *testing.T) {
	used := map[string]bool{}
	for _, profile := range goldenProfiles {
		for _, d := range resolveDials(t, Objects(t, Render(t, Profile(profile)...))) {
			used[d.Workload+"/"+d.Env] = true
		}
	}
	for _, ex := range dialExemptions {
		if !used[ex.Workload+"/"+ex.Env] {
			t.Errorf("dialExemptions has an orphan: %s/%s resolves to no in-chart Service in any "+
				"golden profile. Delete it -- a stale exemption silently excuses whatever takes "+
				"that name next.", ex.Workload, ex.Env)
		}
	}
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// resolveDials returns every (workload, *_URL env) pair whose URL names a
// Service this render contains.
func resolveDials(t *testing.T, objs []Object) []dial {
	t.Helper()
	svcs := servicesByHost(objs)
	var out []dial
	for _, o := range objs {
		if o.Kind != "Deployment" && o.Kind != "StatefulSet" {
			continue
		}
		comp := o.Metadata.Labels[componentLabel]
		if comp == "" {
			continue
		}
		replicas := intAt(o.Raw, "spec", "replicas")
		for _, env := range podEnv(o) {
			if !strings.HasSuffix(env.Name, "_URL") || env.Value == "" {
				continue
			}
			host, port, ok := splitURL(env.Value)
			if !ok {
				continue
			}
			svc, ok := svcs[host]
			if !ok {
				continue // outside the release: LiteLLM, GitLab, a BYO controller
			}
			out = append(out, dial{
				Workload: o.Metadata.Name, SrcComp: comp, SrcReplicas: replicas,
				Env: env.Name, RawURL: env.Value, Svc: svc, Port: port,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Workload != out[j].Workload {
			return out[i].Workload < out[j].Workload
		}
		return out[i].Env < out[j].Env
	})
	return out
}

// servicesByHost indexes every rendered Service under the DNS names a URL may
// use for it: bare name, name.ns, name.ns.svc, name.ns.svc.cluster.local.
func servicesByHost(objs []Object) map[string]chartService {
	out := map[string]chartService{}
	for _, o := range objs {
		if o.Kind != "Service" {
			continue
		}
		spec, _ := o.Raw["spec"].(map[string]any)
		sel, _ := spec["selector"].(map[string]any)
		comp, _ := sel[componentLabel].(string)
		if comp == "" {
			continue // headless/selectorless Services name no component to permit
		}
		svc := chartService{Name: o.Metadata.Name, Component: comp, Ports: map[int]bool{}}
		ports, _ := spec["ports"].([]any)
		for _, p := range ports {
			pm, _ := p.(map[string]any)
			if n, ok := asInt(pm["port"]); ok {
				svc.Ports[n] = true
			}
		}
		out[o.Metadata.Name] = svc
	}
	return out
}

// splitURL reduces a URL to the Service name it names and the port it dials.
// The name is the first DNS label, and everything after it must be a
// cluster-DNS tail (<ns>, <ns>.svc, <ns>.svc.cluster.local) -- so an external
// host such as gitlab.example.internal yields a name that matches no Service and is
// skipped rather than mistaken for one.
func splitURL(raw string) (host string, port int, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", 0, false
	}
	labels := strings.Split(u.Hostname(), ".")
	switch len(labels) {
	case 1:
	case 2, 3, 5:
		tail := strings.Join(labels[2:], ".")
		if tail != "" && tail != "svc" && tail != "svc.cluster.local" {
			return "", 0, false
		}
	default:
		return "", 0, false
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, false
		}
		return labels[0], n, true
	}
	switch u.Scheme {
	case "http":
		return labels[0], 80, true
	case "https":
		return labels[0], 443, true
	}
	return "", 0, false
}

type envVar struct{ Name, Value string }

// podEnv returns the plain-valued env of every container and initContainer.
// valueFrom entries carry no URL and are skipped.
func podEnv(o Object) []envVar {
	var out []envVar
	spec, _ := o.Raw["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pod, _ := tmpl["spec"].(map[string]any)
	for _, key := range []string{"initContainers", "containers"} {
		cs, _ := pod[key].([]any)
		for _, c := range cs {
			cm, _ := c.(map[string]any)
			envs, _ := cm["env"].([]any)
			for _, e := range envs {
				em, _ := e.(map[string]any)
				name, _ := em["name"].(string)
				val, _ := em["value"].(string)
				out = append(out, envVar{Name: name, Value: val})
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Policy inspection
// ---------------------------------------------------------------------------

func hasDefaultDeny(objs []Object) bool {
	for _, o := range objs {
		if o.Kind == "NetworkPolicy" && o.Metadata.Name == "gonk-default-deny" {
			return true
		}
	}
	return false
}

// policyFor finds the NetworkPolicy whose podSelector selects a component.
func policyFor(objs []Object, component string) (map[string]any, bool) {
	for _, o := range objs {
		if o.Kind != "NetworkPolicy" {
			continue
		}
		spec, _ := o.Raw["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		if c, _ := ml[componentLabel].(string); c == component {
			return o.Raw, true
		}
	}
	return nil, false
}

func npName(np map[string]any) string {
	md, _ := np["metadata"].(map[string]any)
	n, _ := md["name"].(string)
	return n
}

// egressPermits reports whether any egress rule names component on port, via a
// same-namespace podSelector. A `to` entry that also carries a
// namespaceSelector is cross-namespace and is not counted: it cannot be shown
// to select the in-release pod.
func egressPermits(np map[string]any, component string, port int) bool {
	spec, _ := np["spec"].(map[string]any)
	rules, _ := spec["egress"].([]any)
	for _, r := range rules {
		rm, _ := r.(map[string]any)
		if !ruleSelectsComponent(rm, component) {
			continue
		}
		if ruleAllowsTCPPort(rm, port) {
			return true
		}
	}
	return false
}

func ruleSelectsComponent(rule map[string]any, component string) bool {
	tos, _ := rule["to"].([]any)
	for _, to := range tos {
		tm, _ := to.(map[string]any)
		if _, cross := tm["namespaceSelector"]; cross {
			continue
		}
		ps, _ := tm["podSelector"].(map[string]any)
		ml, _ := ps["matchLabels"].(map[string]any)
		if c, _ := ml[componentLabel].(string); c == component {
			return true
		}
	}
	return false
}

// ruleAllowsTCPPort reports whether a rule permits TCP on port. A rule with no
// `ports` key permits every port, which is the NetworkPolicy semantic.
func ruleAllowsTCPPort(rule map[string]any, port int) bool {
	ports, ok := rule["ports"].([]any)
	if !ok || len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		pm, _ := p.(map[string]any)
		proto, _ := pm["protocol"].(string)
		if proto == "" {
			proto = "TCP"
		}
		n, ok := asInt(pm["port"])
		if proto == "TCP" && ok && n == port {
			return true
		}
	}
	return false
}

func describeEgress(np map[string]any) string {
	spec, _ := np["spec"].(map[string]any)
	rules, _ := spec["egress"].([]any)
	var b strings.Builder
	for i, r := range rules {
		fmt.Fprintf(&b, "  [%d] %v\n", i, r)
	}
	if b.Len() == 0 {
		return "  (none)\n"
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Exemptions
// ---------------------------------------------------------------------------

func exemptionFor(d dial) (dialExemption, bool) {
	for _, ex := range dialExemptions {
		if ex.Workload == d.Workload && ex.Env == d.Env {
			return ex, true
		}
	}
	return dialExemption{}, false
}

// checkExemption re-proves the reason the exemption was granted, so it cannot
// outlive it.
func checkExemption(t *testing.T, d dial, ex dialExemption) {
	t.Helper()
	if !ex.Self {
		return
	}
	if d.Svc.Component != d.SrcComp {
		t.Errorf("%s/%s is exempted as a SELF dial, but %s selects component %q while the "+
			"pod is %q -- it is not talking to itself, so it needs a real egress rule.",
			d.Workload, d.Env, d.Svc.Name, d.Svc.Component, d.SrcComp)
	}
	if d.SrcReplicas != 1 {
		t.Errorf("%s/%s is exempted as a SELF dial on the grounds that the workload is a "+
			"single replica, but it renders replicas: %d. With a sibling, the call leaves the "+
			"pod and default-deny drops it: the exemption no longer holds and the policy needs "+
			"an egress rule (and a matching ingress rule) for component %q.",
			d.Workload, d.Env, d.SrcReplicas, d.SrcComp)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func intAt(m map[string]any, path ...string) int {
	var cur any = m
	for _, p := range path {
		cm, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = cm[p]
	}
	n, _ := asInt(cur)
	return n
}

func sortedPorts(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}
