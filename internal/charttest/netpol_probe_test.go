//go:build chart

package charttest

import (
	"strings"
	"testing"
)

const probePod = "gonk-netpol-probe"

// The probe must carry EXACTLY the labels the gonk-agent policy selects on, or
// it is probing a policy that does not apply to it and every leg is meaningless.
func TestProbeCarriesTheAgentSelectorLabels(t *testing.T) {
	o := MustObject(t, Render(t, Minimum()...), "Pod", probePod)
	sel := o.Metadata.Labels["app"]
	if sel != "gc-agent" {
		t.Fatalf("probe pod has app=%q, want gc-agent (networkPolicy.agentPodSelector). "+
			"Without the selector labels the gonk-agent policy does not apply to this pod "+
			"and the probe measures nothing.", sel)
	}
}

// AND NOTHING SESSION-SHAPED. Gas City's k8s provider lists
// app=gc-agent,status.phase=Running and skips only pods that have neither a
// `gc-session` label nor a `gc-session-name` annotation. This pod is inside that
// query while it runs, and is excluded by a single `continue` in a pinned
// upstream dependency. A session marker here would make the orchestrator treat a
// test pod as a live agent session.
func TestProbeIsNotMistakableForAnAgentSession(t *testing.T) {
	o := MustObject(t, Render(t, Minimum()...), "Pod", probePod)
	for k, v := range o.Metadata.Labels {
		if strings.Contains(strings.ToLower(k), "gc-session") {
			t.Errorf("probe pod carries session label %s=%s; Gas City would list it as a live agent", k, v)
		}
	}
	for k := range o.Metadata.Annotations {
		if strings.Contains(strings.ToLower(k), "gc-session") {
			t.Errorf("probe pod carries session annotation %q; Gas City would list it as a live agent", k)
		}
	}
	// It must also hold no Kubernetes credential: the probe opens sockets and
	// needs no API access, and gc-agent holds pods:[get].
	if !strings.Contains(o.Doc, "automountServiceAccountToken: false") {
		t.Error("probe pod does not disable the service account token")
	}
	if !strings.Contains(o.Doc, "serviceAccountName: default") {
		t.Error("probe pod does not use the default service account")
	}
}

// The legs are the whole design, and the ALLOW leg has a constraint that is
// easy to miss: NetworkPolicy is enforced on EGRESS AT THE SOURCE and on INGRESS
// AT THE DESTINATION, so a leg is reachable only if BOTH say yes.
//
// The probe originally used gonk-intake:9090 because the agent's egress rule
// names it -- and that rule is DEAD: intake's own policy admits only the traefik
// and monitoring namespaces. On orac, which enforces nothing, the leg connected
// and the probe looked fine. On a cluster that enforces, it would have failed
// and the probe would have reported INCONCLUSIVE on a perfectly good cluster.
//
// So this test asserts the end-to-end property, not the egress half.
func TestProbeAllowLegIsPermittedAtBothEnds(t *testing.T) {
	out := Render(t, Minimum()...)
	probe := MustObject(t, out, "Pod", probePod)
	agent := MustObject(t, out, "NetworkPolicy", "gonk-agent")
	meter := MustObject(t, out, "NetworkPolicy", "gonk-meter")

	if !strings.Contains(probe.Doc, "gonk-meter:8080") {
		t.Fatalf("the allow leg does not target gonk-meter:8080.\n"+
			"It must be a destination BOTH policies permit; gonk-intake:9090 is not one, "+
			"because intake's ingress admits only traefik and monitoring.\nprobe args:\n%s", probe.Doc)
	}
	// Source side: the agent may egress to the meter on 8080.
	if !strings.Contains(agent.Doc, "component: meter") {
		t.Error("the agent egress policy no longer names the meter, so the allow leg is not permitted at the source")
	}
	// Destination side: the meter admits the agent. THIS is the half that was
	// missing, and the half that makes the leg actually work.
	if !strings.Contains(meter.Doc, "app: gc-agent") {
		t.Error("the meter ingress policy no longer admits app=gc-agent, so the allow leg " +
			"would be blocked at the destination and the probe would report INCONCLUSIVE " +
			"on a cluster that enforces correctly")
	}
}

// The denied legs must be destinations the agent's egress does NOT name -- one
// differing by port, one by pod, so a CNI that enforces pods but ignores ports
// is distinguishable from one that enforces neither.
func TestProbeDenyLegsAreNotPermitted(t *testing.T) {
	out := Render(t, Minimum()...)
	probe := MustObject(t, out, "Pod", probePod)
	agent := MustObject(t, out, "NetworkPolicy", "gonk-agent")

	for _, want := range []string{"gonk-intake:8080", "gonk-controller:9443"} {
		if !strings.Contains(probe.Doc, want) {
			t.Errorf("probe does not target the denied destination %s", want)
		}
	}
	// The agent policy must not name the controller at all, or the L3 leg is
	// testing an allowed destination and would always look "not enforced".
	if strings.Contains(agent.Doc, "component: controller") {
		t.Error("the agent egress policy now names the controller, so the L3 deny leg is no longer denied")
	}
}

// A bare TCP connect, never an HTTP request: gonk-intake's private listener
// serves POST /admin/reconcile UNAUTHENTICATED, so a probe that spoke HTTP could
// trigger real work as a side effect of asking a question about the network.
func TestProbeSpeaksNoHTTP(t *testing.T) {
	o := MustObject(t, Render(t, Minimum()...), "Pod", probePod)
	for _, forbidden := range []string{"http://", "https://", "/admin/", "curl", "wget"} {
		if strings.Contains(o.Doc, forbidden) {
			t.Errorf("probe pod references %q; it must open a socket and close it, nothing more", forbidden)
		}
	}
}

// It is a TEST hook. An install hook that failed would block the release -- and
// on a cluster with no policy controller this SHOULD fail, while the chart must
// still install there.
func TestProbeIsATestHookAndCannotBlockARelease(t *testing.T) {
	o := MustObject(t, Render(t, Minimum()...), "Pod", probePod)
	if got := o.Metadata.Annotations["helm.sh/hook"]; got != "test" {
		t.Fatalf("helm.sh/hook = %q, want \"test\". Any install/upgrade hook would block the "+
			"release on exactly the clusters this probe exists to diagnose.", got)
	}
}

// Turning the policies off must not produce a CNI accusation, and neither must
// the absence of the destination the probe dials.
func TestProbeIsAbsentWhenItCouldOnlyMislead(t *testing.T) {
	t.Run("policies disabled", func(t *testing.T) {
		out := Render(t, append(Minimum(), "--set", "networkPolicy.enabled=false")...)
		NoObject(t, out, "Pod", probePod)
	})
	t.Run("probe disabled by value", func(t *testing.T) {
		out := Render(t, append(Minimum(), "--set", "networkPolicy.enforcementProbe.enabled=false")...)
		NoObject(t, out, "Pod", probePod)
	})
	t.Run("no intake to dial", func(t *testing.T) {
		out := Render(t, append(Minimum(), "--set", "intake.enabled=false")...)
		NoObject(t, out, "Pod", probePod)
	})
}
