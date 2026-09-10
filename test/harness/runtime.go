package harness

import (
	"fmt"
	"os/exec"
	"strconv"
)

type RuntimeKind string

const (
	Podman RuntimeKind = "podman"
	Docker RuntimeKind = "docker"
)

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
// reality. If `docker` is actually a podman shim (very common under WSL), we must
// still treat it as podman.
func DetectRuntime() (*Runtime, error) {
	for _, bin := range []string{"podman", "docker"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		out, err := exec.Command(path, "version", "--format", "{{.Client.Version}}").CombinedOutput()
		if err != nil {
			continue
		}
		kind := Docker
		if bin == "podman" || looksLikePodman(out) {
			kind = Podman
		}
		return &Runtime{Bin: path, Kind: kind, HostNetwork: hostNetworkFor(kind)}, nil
	}
	return nil, fmt.Errorf("harness: no container runtime found (tried podman, docker)")
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
