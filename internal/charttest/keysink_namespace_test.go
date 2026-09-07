//go:build chart

package charttest

import (
	"sort"
	"strings"
	"testing"
)

// workloadEnv returns every container env name->value on a rendered Deployment
// or StatefulSet. Values that come from a valueFrom (fieldRef, secretKeyRef)
// are reported as "" -- this helper is for plain values, which is what the
// keysink namespace is.
func workloadEnv(t *testing.T, o Object) map[string]string {
	t.Helper()
	out := map[string]string{}

	spec, _ := o.Raw["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pspec, _ := tmpl["spec"].(map[string]any)
	for _, group := range []string{"containers", "initContainers"} {
		list, _ := pspec[group].([]any)
		for _, c := range list {
			cm, _ := c.(map[string]any)
			envs, _ := cm["env"].([]any)
			for _, e := range envs {
				em, _ := e.(map[string]any)
				name, _ := em["name"].(string)
				val, _ := em["value"].(string)
				if name != "" {
					out[name] = val
				}
			}
		}
	}
	return out
}

const keysinkNS = "GONK_KEYSINK_NAMESPACE"

// The meter's KeySink writes one Secret per project holding that project's
// LiteLLM virtual key, and gonk-gate (in the controller) reads it back so a
// session authenticates as the PROJECT rather than as the proxy admin
// (gonk-8gb). Both ends read the namespace from the same env var, and both
// ends must be told.
//
// UNSET IS NOT A DEFAULT. client-go issues a Get with an empty namespace as a
// CLUSTER-SCOPED read, and Role gc-controller grants `secrets: [get]` in the
// release namespace only -- so the read is denied and every dispatch fails
// closed with
//
//	cannot get resource "secrets" in API group "" at the cluster scope
//
// which reads like missing RBAC and is not. That is not hypothetical: it took
// triage down on 2026-09-07 (project 75, issue 64) with the Role already
// correct and only the controller's env line missing, because the var had been
// wired to the meter alone.
func TestKeysinkNamespaceIsWiredToEveryReaderAndWriter(t *testing.T) {
	out := Render(t, Minimum()...)

	found := map[string]string{}
	var workloads []string
	for _, o := range Objects(t, out) {
		if o.Kind != "Deployment" && o.Kind != "StatefulSet" {
			continue
		}
		workloads = append(workloads, o.Metadata.Name)
		env := workloadEnv(t, o)
		if v, ok := env[keysinkNS]; ok {
			found[o.Metadata.Name] = v
		}
	}
	sort.Strings(workloads)

	// The writer (meter) and the reader (controller) both need it. Named
	// explicitly: a rename that drops one of them should fail here rather than
	// pass because the remaining one still agrees with itself.
	for _, want := range []string{"gonk-meter", "gonk-controller"} {
		if _, ok := found[want]; !ok {
			t.Errorf("workload %q does not set %s.\n"+
				"Without it the Secret read is cluster-scoped and Role gc-controller cannot authorize it, "+
				"so every dispatch fails closed (gonk-8gb).\nWorkloads rendered: %s",
				want, keysinkNS, strings.Join(workloads, ", "))
		}
	}

	// And they must agree, or the controller reads a namespace the meter never
	// writes to -- a fail-closed that looks like a missing key.
	var values []string
	for name, v := range found {
		if strings.TrimSpace(v) == "" {
			t.Errorf("workload %q sets %s to an empty value, which Kubernetes treats as cluster scope", name, keysinkNS)
		}
		values = append(values, v)
	}
	for _, v := range values {
		if v != values[0] {
			names := make([]string, 0, len(found))
			for n := range found {
				names = append(names, n+"="+found[n])
			}
			sort.Strings(names)
			t.Errorf("%s disagrees across workloads: %s.\nThe reader would look in a namespace the writer never writes to.",
				keysinkNS, strings.Join(names, ", "))
			break
		}
	}
}
