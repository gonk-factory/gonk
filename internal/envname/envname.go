// Package envname holds the NAMES of the environment variables that the Helm
// chart sets and a gonk binary reads back.
//
// WHY A PACKAGE FOR A HANDFUL OF STRINGS. Every one of these names is a join between
// two artefacts that no compiler checks against each other: a YAML template in
// chart/gonk/templates and an os.Getenv somewhere under cmd/. Nothing in Go
// notices when one side is renamed, and the failure that produces is always the
// same and always silent -- the binary reads a variable nobody sets, gets "",
// and takes the unconfigured path. Nothing logs, nothing 500s, and the feature
// is simply not there.
//
// That has already happened once, expensively: GONK_KEYSINK_NAMESPACE was wired
// to the meter and not to the controller, and because client-go reads an empty
// namespace as CLUSTER SCOPE, every dispatch failed closed with a message that
// read like missing RBAC (gonk-8gb, 2026-09-07). And it was shown to be
// reachable again in review: renaming ONLY the Go constant behind
// GONK_POD_REAP_NAMESPACE left ./cmd/gonk-gate, ./internal/charttest and
// ./internal/buildgate all green while the pod reaper silently never armed
// (gonk-b6pb).
//
// So the name exists exactly once in Go, here, and the chart tests in
// internal/charttest assert the RENDERED manifests against these constants.
// That makes the join real in both directions: rename the constant and the
// chart assertion fails; rename the template and it fails too. Neither side can
// move alone.
//
// TestNoGoSourceRepeatsAnEnvNameLiteral keeps that property from decaying by
// re-inlining: no .go file outside this one may contain these strings as
// literals.
package envname

// PodReapNamespace names the namespace whose agent pods gonk-gate may list and
// delete. IT IS ALSO THE ON SWITCH: empty means no pod reaper at all.
//
// The two are one variable on purpose. An empty namespace does not mean "the
// current one" to client-go -- it makes the request CLUSTER-SCOPED, which is
// the exact blast radius this feature must never have, and the exact shape of
// the gonk-8gb bug (see KeysinkNamespace). Tying the switch to the namespace
// makes the dangerous value unreachable: there is no configuration in which
// cmd/gonk-gate's pod reaper runs without a namespace to run in.
//
// The name carries no Gas City sensitivity marker (TOKEN/SECRET/PASSWORD/
// API_KEY), or internal/execenv.FilterInherited would strip it from the
// environment of the `gonk-sweep` EXEC ORDER and the reaper would silently
// never arm. TestPodReapEnvNameCarriesNoSensitivityMarker (internal/charttest)
// asserts that against THIS constant. See the GONK_*_FILE note in
// chart/gonk/templates/workload-gonk-controller.yaml.
//
// Set by chart/gonk/templates/workload-gonk-controller.yaml under
// `podReap.enabled`; read by cmd/gonk-gate/main.go.
const PodReapNamespace = "GONK_POD_REAP_NAMESPACE"

// KeysinkNamespace names the namespace holding the per-project LiteLLM virtual
// key Secrets: the meter WRITES them there and gonk-gate READS them back, so a
// session authenticates as the project rather than as the proxy admin.
//
// UNSET IS NOT A DEFAULT. client-go issues a Get with an empty namespace as a
// CLUSTER-SCOPED read, which Role gc-controller cannot authorize, so every
// dispatch fails closed with
//
//	cannot get resource "secrets" in API group "" at the cluster scope
//
// which reads like missing RBAC and is not (gonk-8gb).
//
// Set by chart/gonk/templates/deployment-gonk-meter.yaml and
// chart/gonk/templates/workload-gonk-controller.yaml; read by
// cmd/gonk-meter/main.go and cmd/gonk-gate/main.go.
const KeysinkNamespace = "GONK_KEYSINK_NAMESPACE"

// THE SESSION RUNTIME'S CONFIGURATION (gonk-t55, plan D5/D6/P5).
//
// Under the Jobs runtime gonk-gate creates each agent session as a Kubernetes
// Job itself, so everything Gas City's k8s session provider used to be told
// through GC_K8S_* is now told to gonk-gate. All of these are set by
// chart/gonk/templates/workload-gonk-controller.yaml on the controller
// container, which is where the gonk-dispatch and gonk-sweep exec orders run;
// read by pkg/runtime/jobs's configuration in cmd/gonk-gate (P2/P3).
//
// They are CONTROLLER env, not agent env: none of them is copied onto the Job's
// pod (D5 names the only agent env). All are marker-free so Gas City's
// execenv.FilterInherited does not strip them from the exec orders
// (TestJobsRuntimeEnvNamesCarryNoSensitivityMarker, internal/charttest).

// AgentImage names the full image reference (registry/repo:tag) every agent
// session Job runs. The chart renders it from componentImages.agent -- the SAME
// value GC_K8S_IMAGE gets -- so the Gas City path and the Jobs path cannot run
// different agent images from one release.
const AgentImage = "GONK_AGENT_IMAGE"

// SessionNamespace names the namespace gonk-gate creates session Jobs and their
// Secrets in, and lists them from. Always the release namespace. UNSET IS NOT
// A DEFAULT: an empty namespace makes client-go's List CLUSTER-SCOPED (the
// gonk-8gb shape), which Role gonk-gate-jobs cannot authorize.
const SessionNamespace = "GONK_SESSION_NAMESPACE"

// SessionServiceAccount names the ServiceAccount a session pod runs as (D6:
// gc-agent, with automountServiceAccountToken false). The chart owns that
// ServiceAccount's name, so the chart states it.
const SessionServiceAccount = "GONK_SESSION_SERVICE_ACCOUNT"

// SessionTTLSeconds names the Job's ttlSecondsAfterFinished (D6). Long enough
// that the sweep, which runs about every 60s, can still read a finished
// session's log; sessionRuntime.ttlSecondsAfterFinished in values.yaml.
const SessionTTLSeconds = "GONK_SESSION_TTL_SECONDS"

// SessionCPURequest, SessionMemoryRequest, SessionCPULimit and
// SessionMemoryLimit name the session container's resources (D6), as
// Kubernetes quantity strings; sessionRuntime.resources in values.yaml.
const (
	SessionCPURequest    = "GONK_SESSION_CPU_REQUEST"
	SessionMemoryRequest = "GONK_SESSION_MEMORY_REQUEST"
	SessionCPULimit      = "GONK_SESSION_CPU_LIMIT"
	SessionMemoryLimit   = "GONK_SESSION_MEMORY_LIMIT"
)

// SessionRuntime is every name above, for the tests that walk the session
// runtime's configuration as a set.
var SessionRuntime = []string{
	AgentImage,
	SessionNamespace,
	SessionServiceAccount,
	SessionTTLSeconds,
	SessionCPURequest,
	SessionMemoryRequest,
	SessionCPULimit,
	SessionMemoryLimit,
}

// All is every env name this package declares, in declaration order.
//
// It exists so a test can walk the set rather than naming its members -- a new
// constant added above without a line here is the one case those tests would
// miss, which is why TestAllListsEveryDeclaredEnvName reads the source of this
// file rather than trusting the slice.
var All = []string{
	PodReapNamespace,
	KeysinkNamespace,
	AgentImage,
	SessionNamespace,
	SessionServiceAccount,
	SessionTTLSeconds,
	SessionCPURequest,
	SessionMemoryRequest,
	SessionCPULimit,
	SessionMemoryLimit,
}
