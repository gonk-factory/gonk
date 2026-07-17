//go:build images

package images

import (
	"os/exec"
	"strings"
	"testing"
)

// controllerImage mirrors agentImage (agent_smoke_test.go) for the
// gonk-controller image `make controller-image` builds (Task 6).
func controllerImage(t *testing.T) (image string, pins map[string]string) {
	t.Helper()
	root := repoRoot(t)
	pins = versionPins(t, root)
	registry := pins["REGISTRY"]
	if registry == "" {
		t.Fatal("REGISTRY missing from images/versions.env")
	}
	tag := gonkTag(t, root, pins["GONK_VERSION"])
	image = registry + "/gonk-controller:" + tag
	if err := exec.Command("podman", "image", "exists", image).Run(); err != nil {
		t.Skipf("image %s not present locally -- run `make controller-image` first (skip, not fail: this is a container smoke test, not a unit test)", image)
	}
	return image, pins
}

// TestControllerImagePinsMatchVersionsEnv: `gc version --long` and
// `bd --version` both report the pins images/versions.env declares -- a
// floating controller is not a controller.
//
// REAL FINDING: gascity's CLI has no `gc --version` flag -- confirmed against
// the running image (`gc: unknown flag: --version`, then it printed full
// help). The real invocation is the `version` SUBCOMMAND, and only
// `gc version --long` includes the git commit
// (cmd/gc/cmd_version.go: `debug.ReadBuildInfo()`'s `vcs.revision`, embedded
// automatically because images/Dockerfile.controller's `gc` build stage
// compiles from an actual git checkout at GASCITY_REF) -- bare `gc version`
// prints only gascity's own semver ("1.1.1" at this pin), which tells us
// nothing about whether the image was built at the SHA we pinned.
func TestControllerImagePinsMatchVersionsEnv(t *testing.T) {
	image, pins := controllerImage(t)

	t.Run("gc", func(t *testing.T) {
		want := pins["GASCITY_REF"]
		if want == "" {
			t.Fatal("GASCITY_REF missing from images/versions.env")
		}
		out, code := runIn(t, image, "/usr/local/bin/gc", "version", "--long")
		if code != 0 {
			t.Fatalf("gc version --long exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, want) {
			t.Fatalf("gc version --long = %q, want it to contain the pinned GASCITY_REF %q", out, want)
		}
	})

	t.Run("bd", func(t *testing.T) {
		want := pins["BD_VERSION"]
		if want == "" {
			t.Fatal("BD_VERSION missing from images/versions.env")
		}
		out, code := runIn(t, image, "/usr/local/bin/bd", "--version")
		if code != 0 {
			t.Fatalf("bd --version exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, want) {
			t.Fatalf("bd --version = %q, want it to contain pinned version %q", out, want)
		}
	})
}

// TestControllerImageGonkGateVersionMatchesTag closes the gap Task 5 and this
// task's own smoke-test wishlist both flagged: cmd/gonk-gate now has a
// `--version` flag (cmd/gonk-gate/main.go), stamped at build time via
// `-ldflags -X main.version=$(GONK_TAG)` (images/Dockerfile.controller).
func TestControllerImageGonkGateVersionMatchesTag(t *testing.T) {
	image, _ := controllerImage(t)
	root := repoRoot(t)
	pins := versionPins(t, root)
	tag := gonkTag(t, root, pins["GONK_VERSION"])

	out, code := runIn(t, image, "/usr/local/bin/gonk-gate", "--version")
	if code != 0 {
		t.Fatalf("gonk-gate --version exited %d:\n%s", code, out)
	}
	if got := strings.TrimSpace(out); got != tag {
		t.Fatalf("gonk-gate --version = %q, want %q (GONK_TAG)", got, tag)
	}
}

func TestControllerImagePackIsBaked(t *testing.T) {
	image, _ := controllerImage(t)
	out, code := runIn(t, image, "/usr/bin/test", "-f", "/opt/gonk/pack/pack.toml")
	if code != 0 {
		t.Fatalf("/opt/gonk/pack/pack.toml missing in the controller image (exit %d):\n%s", code, out)
	}
}

func TestControllerImageRunsAsNonRoot(t *testing.T) {
	image, _ := controllerImage(t)
	out, code := runIn(t, image, "/usr/bin/id", "-u")
	if code != 0 {
		t.Fatalf("id -u exited %d:\n%s", code, out)
	}
	if got := strings.TrimSpace(out); got != "65532" {
		t.Fatalf("id -u = %q, want 65532 (not root)", got)
	}
}
