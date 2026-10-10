package harness

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type RuntimeKind string

const (
	Podman RuntimeKind = "podman"
	Docker RuntimeKind = "docker"
)

// RuntimeEnvVar, when set to "podman" or "docker", pins DetectRuntime to
// exactly that runtime and disables its PATH scan -- see DetectRuntime.
const RuntimeEnvVar = "GONK_CONTAINER_RUNTIME"

type Runtime struct {
	Bin  string
	Kind RuntimeKind
	// HostNetwork forces --network=host. Unconditional (see hostNetworkFor):
	// this dev box's podman-under-WSL has BROKEN CNI BRIDGE NETWORKING, but
	// even on a genuine (non-podman-shimmed) Docker daemon -- e.g. a GitHub
	// Actions runner -- StartPostgres and StartLiteLLM never publish a
	// container port, so without host networking they would have no route
	// back to what they just started either.
	HostNetwork bool
}

// DetectRuntime finds a container runtime and configures it for THIS box's
// reality.
//
// If RuntimeEnvVar (GONK_CONTAINER_RUNTIME) is set, it is authoritative:
// DetectRuntime probes ONLY the named binary and returns an error if it is
// missing or unusable -- it never silently falls back to the other runtime.
// That silent fallback is what broke the `images` CI job (T-16/gonk-ak0):
// the build step ran `make images PODMAN=docker`, so every image landed in
// Docker's image store, but the test step's unguided PATH scan found
// `podman` first (GitHub Actions runners ship both) and probed a store none
// of those images were ever written to. Every presence check came back
// false, and RequireInfra correctly -- but pointlessly -- FATALed the whole
// job. Setting GONK_CONTAINER_RUNTIME=docker on a job that builds and then
// probes images makes the probe use the SAME store the build wrote to, by
// name, not by accident of PATH order.
//
// If RuntimeEnvVar is unset, behavior is unchanged from before it existed:
// scan PATH, podman first, then docker.
func DetectRuntime() (*Runtime, error) {
	if override := os.Getenv(RuntimeEnvVar); override != "" {
		return detectNamedRuntime(override)
	}
	for _, bin := range []string{"podman", "docker"} {
		if rt, err := probeRuntime(bin); err == nil {
			return rt, nil
		}
	}
	return nil, fmt.Errorf("harness: no container runtime found (tried podman, docker)")
}

// detectNamedRuntime probes exactly the binary RuntimeEnvVar names. It never
// tries the other runtime: a caller that set the override wants a specific
// store probed, and falling back to whatever else is on PATH is precisely
// the bug this override exists to close.
func detectNamedRuntime(bin string) (*Runtime, error) {
	if bin != string(Podman) && bin != string(Docker) {
		return nil, fmt.Errorf("harness: %s=%q is not a supported runtime (want %q or %q)", RuntimeEnvVar, bin, Podman, Docker)
	}
	rt, err := probeRuntime(bin)
	if err != nil {
		return nil, fmt.Errorf("harness: %s=%q but %q is not a usable container runtime (must be on PATH and answer `%s version`): %w", RuntimeEnvVar, bin, bin, bin, err)
	}
	return rt, nil
}

// probeRuntime resolves bin on PATH, confirms it actually runs, and
// classifies it as Podman or Docker. It returns an error -- never a
// different runtime -- when bin is absent or unresponsive; callers decide
// whether that error is fatal (detectNamedRuntime) or a reason to try the
// next candidate (DetectRuntime's PATH scan).
func probeRuntime(bin string) (*Runtime, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(path, "version", "--format", "{{.Client.Version}}").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s version: %w (%s)", bin, err, strings.TrimSpace(string(out)))
	}
	kind := Docker
	if bin == string(Podman) || looksLikePodman(out) {
		kind = Podman
	}
	return &Runtime{Bin: path, Kind: kind, HostNetwork: hostNetworkFor(kind)}, nil
}

// hostNetworkFor decides whether a detected runtime must run containers with
// --network=host. It used to be Podman-only, because THIS dev box's
// podman-under-WSL has a broken CNI bridge. But StartPostgres and
// StartLiteLLM (ledgerdb.go, litellm.go) always dial the container they just
// started on 127.0.0.1:<fixed port> and never pass a `-p host:container`
// publish flag -- host networking is the ONLY way either function's
// container is ever reachable, for EITHER runtime. Podman-only host
// networking left a real gap: on a genuine Docker daemon (Kind == Docker,
// not a podman shim) -- exactly what a GitHub Actions ubuntu-24.04 runner
// gives you -- HostNetwork came back false and the harness silently booted
// an unreachable container. gonk's container-backed suites only ever run on
// Linux (this file's own WSL notes, docs/environment.md), where
// --network=host is available and safe for both runtimes, so this is
// unconditional rather than Podman-only.
func hostNetworkFor(kind RuntimeKind) bool {
	_ = kind
	return true
}

func looksLikePodman(version []byte) bool {
	// `docker` on this box may be a podman shim. Ask a podman-specific field: only
	// podman's info carries a Buildah version.
	_ = version
	out, err := exec.Command("docker", "info", "--format", "{{.Host.BuildahVersion}}").CombinedOutput()
	return err == nil && len(out) > 1
}

// RunArgs builds a detached `run` invocation. Under host networking, port
// publishing is meaningless (and podman errors on it), so ports are IGNORED and
// the caller must simply use the container's own listen port on 127.0.0.1.
func (r Runtime) RunArgs(name, image string, ports map[int]int, cmd []string) []string {
	args := []string{"run", "-d", "--name", name}
	if r.HostNetwork {
		args = append(args, "--network=host")
	} else {
		for host, ctr := range ports {
			args = append(args, "-p", strconv.Itoa(host)+":"+strconv.Itoa(ctr))
		}
	}
	args = append(args, image)
	return append(args, cmd...)
}
