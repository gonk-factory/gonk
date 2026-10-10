package main

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gonk-factory/gonk/pkg/runtime"
	"github.com/gonk-factory/gonk/pkg/runtime/jobs"
)

// newSessionRuntime builds the production SessionRuntime that dispatch and
// sweep run agent sessions on (gonk-t55): Kubernetes Jobs, the only production
// choice (plan section 0, cutover option A -- there is no Gas City adapter
// behind the seam).
//
// Its configuration is the controller's env, read by jobs.ConfigFromEnv
// through internal/envname (GONK_SESSION_NAMESPACE and GONK_AGENT_IMAGE
// required); the cluster client is the pod's own service account. Either
// failing is a misconfiguration: main refuses to dispatch or sweep (exit 2)
// rather than guess.
//
// It is a variable, and the ONE place gonk-gate chooses a runtime, so a test
// that drives the real main() (main_signal_test.go) can hand it a fake --
// what those tests check is main's wiring, not a cluster.
var newSessionRuntime = func() (runtime.SessionRuntime, error) {
	cfg, err := jobs.ConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("session runtime: %w", err)
	}
	cs, err := inClusterClientset()
	if err != nil {
		return nil, fmt.Errorf("session runtime: no in-cluster Kubernetes client: %w", err)
	}
	return jobs.New(cs, cfg)
}

// inClusterClientset builds a clientset from the pod's own service account,
// exactly as newSecretReader (keyread.go) does.
func inClusterClientset() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
