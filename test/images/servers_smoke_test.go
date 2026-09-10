//go:build images

// Container smoke tests for the two Plan 02/03 service binaries baked as
// images (Task 7): gonk-intake and gonk-meter. Mirrors agent_smoke_test.go /
// controller_smoke_test.go's own pattern (repoRoot/versionPins/gonkTag/runIn
// are shared with those files in this package).
//
// Run: go test ./test/images/ -tags images -run 'Intake|Meter' -v
package images

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/test/harness"
)

func intakeImage(t *testing.T) (image string, pins map[string]string) {
	t.Helper()
	root := repoRoot(t)
	pins = versionPins(t, root)
	registry := pins["REGISTRY"]
	if registry == "" {
		t.Fatal("REGISTRY missing from images/versions.env")
	}
	tag := gonkTag(t, root, pins["GONK_VERSION"])
	image = registry + "/gonk-intake:" + tag
	present := exec.Command(containerBin(t), "image", "inspect", image).Run() == nil
	harness.RequireInfra(t, "image "+image+" (run `make intake-image` first)", present)
	return image, pins
}

// meterImage returns the PRODUCTION meter image (no BUILD_TAGS -- see
// meterTestclockImage for the e2e-only sibling).
func meterImage(t *testing.T) (image string, pins map[string]string) {
	t.Helper()
	root := repoRoot(t)
	pins = versionPins(t, root)
	registry := pins["REGISTRY"]
	if registry == "" {
		t.Fatal("REGISTRY missing from images/versions.env")
	}
	tag := gonkTag(t, root, pins["GONK_VERSION"])
	image = registry + "/gonk-meter:" + tag
	present := exec.Command(containerBin(t), "image", "inspect", image).Run() == nil
	harness.RequireInfra(t, "image "+image+" (run `make meter-image` first)", present)
	return image, pins
}

// meterTestclockImage returns the e2e-only variant built with
// --build-arg BUILD_TAGS=testclock (`make meter-testclock-image`), tagged
// $(GONK_TAG)-testclock -- NEVER the production tag.
func meterTestclockImage(t *testing.T) (image string, pins map[string]string) {
	t.Helper()
	root := repoRoot(t)
	pins = versionPins(t, root)
	registry := pins["REGISTRY"]
	if registry == "" {
		t.Fatal("REGISTRY missing from images/versions.env")
	}
	tag := gonkTag(t, root, pins["GONK_VERSION"]) + "-testclock"
	image = registry + "/gonk-meter:" + tag
	present := exec.Command(containerBin(t), "image", "inspect", image).Run() == nil
	harness.RequireInfra(t, "image "+image+" (run `make meter-testclock-image` first)", present)
	return image, pins
}

// extractFromImage copies a file out of an image without ever running the
// image's own ENTRYPOINT (both images here are distroless -- there is no
// shell to `cat` with). `podman create` materializes a (stopped) container
// from the image layers; `podman cp` reads a path out of it directly.
func extractFromImage(t *testing.T, image, pathInImage string) []byte {
	t.Helper()
	bin := containerBin(t)
	var createOut bytes.Buffer
	create := exec.Command(bin, "create", "--network=host", image)
	create.Stdout = &createOut
	var createErr bytes.Buffer
	create.Stderr = &createErr
	if err := create.Run(); err != nil {
		t.Fatalf("%s create %s: %v\nstderr:\n%s", bin, image, err, createErr.String())
	}
	cid := strings.TrimSpace(createOut.String())
	t.Cleanup(func() { _ = exec.Command(bin, "rm", "-f", cid).Run() })

	dir := t.TempDir()
	dst := dir + "/extracted"
	var cpErr bytes.Buffer
	cp := exec.Command(bin, "cp", cid+":"+pathInImage, dst)
	cp.Stderr = &cpErr
	if err := cp.Run(); err != nil {
		t.Fatalf("%s cp %s:%s: %v\nstderr:\n%s", bin, cid, pathInImage, err, cpErr.String())
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	return b
}

// TestIntakeImagePinsMatchVersionsEnv proves the binary that ships is the one
// built at GONK_TAG -- runs with no config at all (a required env var is
// deliberately absent) and asserts main.go's own fail-closed error surfaces
// on stderr, which only happens if the real gonk-intake binary is present and
// executable on this distroless base.
func TestIntakeImagePinsMatchVersionsEnv(t *testing.T) {
	image, _ := intakeImage(t)
	out, code := runIn(t, image, "/usr/local/bin/gonk-intake")
	if code != 1 {
		t.Fatalf("gonk-intake with no env set: exit %d, want 1 (its own fail-closed startup error):\n%s", code, out)
	}
	if !strings.Contains(out, "GONK_") {
		t.Fatalf("gonk-intake's fatal startup error does not mention a GONK_* env var (want a fail-closed config error):\n%s", out)
	}
}

func TestIntakeImageRunsAsNonRoot(t *testing.T) {
	image, _ := intakeImage(t)
	uid := imageUser(t, image)
	if uid != "65532:65532" {
		t.Fatalf("image User = %q, want 65532:65532 (not root)", uid)
	}
}

// TestMeterImagePinsMatchVersionsEnv is the same fail-closed proof as intake's
// above, for the production (non-testclock) meter image.
//
// NOTE: cmd/gonk-meter/main.go's loadConfig checks its five required env vars
// via a `for name, v := range map[string]string{...}` loop -- Go map
// iteration order is randomized, so WHICH one is reported first is not
// deterministic across runs (confirmed: a first version of this test that
// asserted the message named LITELLM_URL specifically flaked). This asserts
// only what is actually guaranteed: exit 1, and the generic "is required"
// shape every branch of that loop shares.
func TestMeterImagePinsMatchVersionsEnv(t *testing.T) {
	image, _ := meterImage(t)
	out, code := runIn(t, image, "/usr/local/bin/gonk-meter", "--operator-config=/nonexistent")
	if code != 1 {
		t.Fatalf("gonk-meter with only --operator-config set: exit %d, want 1 (its own fail-closed startup error):\n%s", code, out)
	}
	if !strings.Contains(out, "is required") {
		t.Fatalf("gonk-meter's fatal startup error does not have the required-env-var shape:\n%s", out)
	}
}

func TestMeterImageRunsAsNonRoot(t *testing.T) {
	image, _ := meterImage(t)
	uid := imageUser(t, image)
	if uid != "65532:65532" {
		t.Fatalf("image User = %q, want 65532:65532 (not root)", uid)
	}
}

// *** THE testclock guard. ***
//
// Plan 06's Task 9 Step 2 asserts "the production binary contains neither the
// symbol nor the literal." This is that assertion, made HERE, at image-build
// time, where it is cheap (Task 7 Step 2's own instruction): a meter whose
// clock can be moved by a file is a meter whose MONTH BOUNDARY can be moved by
// a file. In production that is a budget bypass -- reset the window, reset
// the spend. The testclock build exists ONLY for the e2e harness, and the
// production image must not contain a trace of it.
func TestProductionMeterImageHasNoTestClock(t *testing.T) {
	image, _ := meterImage(t)
	bin := extractFromImage(t, image, "/usr/local/bin/gonk-meter")
	for _, needle := range []string{"GONK_TESTCLOCK_FILE", "testclock"} {
		if bytes.Contains(bin, []byte(needle)) {
			t.Fatalf("the PRODUCTION gonk-meter image contains %q.\n"+
				"A clock that can be moved by a file is a budget window that can be "+
				"reset by a file.", needle)
		}
	}
}

// And the converse, or the harness is testing a binary it cannot drive: the
// e2e-only testclock image must actually carry the seam it is built for.
func TestTestclockMeterImageHasTheSeam(t *testing.T) {
	image, _ := meterTestclockImage(t)
	bin := extractFromImage(t, image, "/usr/local/bin/gonk-meter")
	if !bytes.Contains(bin, []byte("GONK_TESTCLOCK_FILE")) {
		t.Fatal("the testclock gonk-meter image does NOT contain the GONK_TESTCLOCK_FILE literal -- " +
			"the e2e harness has no way to move this image's clock, so it cannot drive a month-boundary test")
	}
}

// imageUser reads the image's configured USER (podman inspect), the same
// non-shell-dependent way extractFromImage reads a file -- neither
// gonk-intake nor gonk-meter's distroless base has an `id` binary to shell
// out to.
func imageUser(t *testing.T, image string) string {
	t.Helper()
	bin := containerBin(t)
	var out, errBuf bytes.Buffer
	cmd := exec.Command(bin, "inspect", image, "--format", "{{.Config.User}}")
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s inspect %s: %v\nstderr:\n%s", bin, image, err, errBuf.String())
	}
	return strings.TrimSpace(out.String())
}
