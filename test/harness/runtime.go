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
	// HostNetwork forces --network=host. It is set automatically for podman,
	// because the dev box's podman-under-WSL has BROKEN CNI BRIDGE NETWORKING and
	// a container without host networking simply has no route to anything.
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
		return &Runtime{Bin: path, Kind: kind, HostNetwork: kind == Podman}, nil
	}
	return nil, fmt.Errorf("harness: no container runtime found (tried podman, docker)")
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
