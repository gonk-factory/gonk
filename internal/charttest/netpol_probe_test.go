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

// The legs are the whole design. Two ports on ONE destination pod, so the only
// difference between the permitted and forbidden legs is the port the policy
// names -- legs differing in namespace, subnet or NAT path prove nothing. Plus
// one pod the policy never names, to separate a CNI that enforces WHICH PODS but
// ignores WHICH PORTS from one that enforces neither.
func TestProbeLegsMatchTheAgentPolicy(t *testing.T) {
	out := Render(t, Minimum()...)
	o := MustObject(t, out, "Pod", probePod)

	for _, want := range []string{
		"gonk-intake-internal:9090", // permitted: the policy names intake on the private port
		"gonk-intake:8080",          // forbidden: SAME pod, port not named
		"gonk-controller:9443",      // forbidden: pod not named at all
	} {
		if !strings.Contains(o.Doc, want) {
			t.Errorf("probe does not target %s", want)
		}
	}

	// And the policy must actually say what the legs assume. If someone widens
	// the agent policy to allow intake:8080, the deny leg silently becomes a
	// second allow leg and the probe reports NOT ENFORCED on a healthy cluster.
	pol := MustObject(t, out, "NetworkPolicy", "gonk-agent")
	if !strings.Contains(pol.Doc, "port: 9090") {
		t.Error("the agent policy no longer permits intake:9090, so the probe's ALLOW leg is wrong")
	}
	if strings.Contains(pol.Doc, "port: 8080") && strings.Contains(pol.Doc, "component: intake") {
		// meter:8080 is legitimately allowed; only an intake:8080 rule breaks us.
		t.Log("note: the agent policy mentions 8080 -- confirm it is meter, not intake")
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
