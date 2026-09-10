package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFakeRuntimeBin drops an executable shell stub named `name` into dir
// that answers `<name> version --format ...` with a fake version, without
// needing a real container runtime installed. It fails (nonzero exit, no
// output) on an `info` subcommand -- matching genuine Docker's behavior for
// looksLikePodman's `docker info --format {{.Host.BuildahVersion}}` probe,
// which errors because Docker's info struct has no such field -- so a fake
// "docker" on PATH is classified as Docker, not misread as a podman shim.
func writeFakeRuntimeBin(t *testing.T, dir, name string) {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = \"info\" ]; then exit 1; fi\necho 1.2.3\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestHostNetworkForIsUnconditional pins hostNetworkFor's decision for both
// runtime kinds. StartPostgres and StartLiteLLM never publish a container
// port -- they always dial the container they just started on
// 127.0.0.1:<fixed port> -- so host networking is the only way either
// function's container is ever reachable, for EITHER runtime. A regression
// back to "Podman only" (the original bug: true only when kind == Podman)
// would leave a genuine Docker daemon -- e.g. a GitHub Actions runner --
// booting containers with no route back to them at all, and this test would
// catch it: it fails today if hostNetworkFor(Docker) is changed to false.
func TestHostNetworkForIsUnconditional(t *testing.T) {
	cases := []struct {
		kind RuntimeKind
		want bool
	}{
		{Podman, true},
		{Docker, true},
	}
	for _, tc := range cases {
		if got := hostNetworkFor(tc.kind); got != tc.want {
			t.Errorf("hostNetworkFor(%s) = %v, want %v -- StartPostgres/StartLiteLLM have no other way to reach a container they start under this runtime", tc.kind, got, tc.want)
		}
	}
}

// TestDetectRuntimeEnvOverrideRefusesSilentFallback reproduces the T-16 CI
// bug class directly: podman is on PATH (as GitHub Actions runners ship
// it), but GONK_CONTAINER_RUNTIME asks for docker, which is absent. The old,
// unguided PATH scan would have happily returned podman -- exactly the
// accident-of-PATH-order substitution that made the `images` job probe a
// store `make images PODMAN=docker` never wrote to. DetectRuntime must fail
// instead of silently handing back the runtime it happened to find.
func TestDetectRuntimeEnvOverrideRefusesSilentFallback(t *testing.T) {
	dir := t.TempDir()
	writeFakeRuntimeBin(t, dir, "podman") // only podman present
	t.Setenv("PATH", dir)
	t.Setenv(RuntimeEnvVar, "docker") // but docker is what's requested

	rt, err := DetectRuntime()
	if err == nil {
		t.Fatalf("DetectRuntime silently fell back to podman (bin=%s) instead of failing on the requested, absent docker", rt.Bin)
	}
}

// TestDetectRuntimeEnvOverrideBogusValueFailsLoudly pins the "bogus binary"
// contract: a GONK_CONTAINER_RUNTIME value that names neither supported
// runtime must be rejected outright, never quietly ignored in favor of
// whatever a PATH scan would have found.
func TestDetectRuntimeEnvOverrideBogusValueFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	writeFakeRuntimeBin(t, dir, "podman")
	writeFakeRuntimeBin(t, dir, "docker")
	t.Setenv("PATH", dir)
	t.Setenv(RuntimeEnvVar, "not-a-real-runtime")

	rt, err := DetectRuntime()
	if err == nil {
		t.Fatalf("DetectRuntime accepted a bogus GONK_CONTAINER_RUNTIME value and returned %+v instead of failing loudly", rt)
	}
}

// TestDetectRuntimeEnvOverrideSelectsNamedRuntime is the positive case:
// with both runtimes present, the override picks the one it names, not
// whichever the PATH-scan order would prefer (podman, first in the
// unset-var scan).
func TestDetectRuntimeEnvOverrideSelectsNamedRuntime(t *testing.T) {
	dir := t.TempDir()
	writeFakeRuntimeBin(t, dir, "podman")
	writeFakeRuntimeBin(t, dir, "docker")
	t.Setenv("PATH", dir)
	t.Setenv(RuntimeEnvVar, "docker")

	rt, err := DetectRuntime()
	if err != nil {
		t.Fatalf("DetectRuntime(%s=docker) returned error with both runtimes present: %v", RuntimeEnvVar, err)
	}
	if rt.Kind != Docker {
		t.Errorf("Kind = %s, want %s", rt.Kind, Docker)
	}
	if rt.Bin != filepath.Join(dir, "docker") {
		t.Errorf("Bin = %s, want the docker stub at %s", rt.Bin, filepath.Join(dir, "docker"))
	}
}

// TestDetectRuntimeUnsetEnvStillScansPath is the unchanged-local-behavior
// guarantee: with GONK_CONTAINER_RUNTIME unset, DetectRuntime must still
// prefer podman on PATH exactly as it did before this override existed.
func TestDetectRuntimeUnsetEnvStillScansPath(t *testing.T) {
	dir := t.TempDir()
	writeFakeRuntimeBin(t, dir, "podman")
	writeFakeRuntimeBin(t, dir, "docker")
	t.Setenv("PATH", dir)
	t.Setenv(RuntimeEnvVar, "")

	rt, err := DetectRuntime()
	if err != nil {
		t.Fatalf("DetectRuntime() with %s unset returned error: %v", RuntimeEnvVar, err)
	}
	if rt.Kind != Podman {
		t.Errorf("Kind = %s, want %s (podman must still win the PATH scan when the override is unset)", rt.Kind, Podman)
	}
}
