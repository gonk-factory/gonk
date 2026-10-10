// Package buildgate asserts properties of the build itself that no unit test
// of the Go code could catch, because the mistake lives in the Makefile.
//
// It runs in the ordinary `go test ./...` gate deliberately -- unlike
// test/images, which is behind the `images` build tag and therefore does not
// run in CI's test job. A guard nobody runs is the same as no guard.
package buildgate

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// pushRecipe returns the body of the Makefile's `push:` target.
func pushRecipe(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "push:") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatal("no `push:` target found in the Makefile")
	}
	var body []string
	for _, l := range lines[start:] {
		// A recipe line is tab-indented; the first non-tab, non-blank line
		// ends the target.
		if strings.HasPrefix(l, "\t") {
			body = append(body, l)
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		break
	}
	if len(body) == 0 {
		t.Fatal("`push:` target has an empty recipe")
	}
	return strings.Join(body, "\n")
}

// pushedTag matches the tag argument of a `podman push` (via $(PODMAN)).
// The reference stops at whitespace or shell punctuation: these lines live
// inside a `for ... ; do ... ; done` loop, so the ref is often followed
// immediately by a `;`.
var pushedTag = regexp.MustCompile(`\$\(PODMAN\)\s+push\s+([^\s;\\]+)`)

// A hand `make push` is single-architecture -- it builds for whatever the box
// running make happens to be. On 2026-08-04 one overwrote four CI-published
// multi-arch indexes with amd64 images, and every gonk pod scheduled onto an
// arm64 node crash-looped with `exec format error` for six days while CI
// stayed green (gonk-n50, gonk-9ub).
//
// So: `push` may only ever write ARCH-SUFFIXED tags. The deployable tag
// belongs to CI, which publishes it as an index over both architectures.
func TestPushNeverWritesADeployableTag(t *testing.T) {
	recipe := pushRecipe(t)

	matches := pushedTag.FindAllStringSubmatch(recipe, -1)
	if len(matches) == 0 {
		t.Fatal("no `$(PODMAN) push` found in the push recipe -- has it been renamed? this guard must be updated with it")
	}

	for _, m := range matches {
		ref := m[1]
		if !strings.HasSuffix(ref, "-$(TARGETARCH)") {
			t.Errorf("push writes %q, which is not arch-suffixed.\n"+
				"A hand build is single-arch; writing a deployable tag from one is what caused gonk-n50 "+
				"(six days of `exec format error` on the arm64 nodes, CI green throughout).\n"+
				"Push to $(GONK_TAG)-$(TARGETARCH) and let CI publish the deployable tag as an index.", ref)
		}
	}
}

// The images the push target tags must be the ones `images` built, so the
// arch-suffixed push cannot silently publish something stale or unrelated.
func TestPushTagsTheImagesItJustBuilt(t *testing.T) {
	recipe := pushRecipe(t)
	if !strings.Contains(recipe, "$(PODMAN) tag") {
		t.Error("push no longer re-tags before pushing; the arch-suffixed refs must come from the images `make images` just built")
	}
}
