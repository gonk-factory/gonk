// The golangci-lint version is pinned in THREE places -- Makefile's
// LINT_VERSION (the local gate and its podman fallback), .gitlab-ci.yml's
// `lint:` job image, and .github/workflows/ci.yml's golangci-lint-action
// `version:` -- because there are three places that run it. A bump to one
// without the other two is exactly the version skew Makefile's `lint`
// target's own host-binary check (T-14, Change 4) exists to catch on a dev
// box; this test catches the same drift between the pins themselves.
package buildgate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// makefileLintVersionRe matches Makefile's `LINT_VERSION_NUM := X.Y.Z` --
// the bare number, because LINT_VERSION itself is composed from it
// (`v$(LINT_VERSION_NUM)`) rather than spelled out literally, and a regex
// over Makefile text cannot resolve make's own variable substitution.
var makefileLintVersionRe = regexp.MustCompile(`(?m)^LINT_VERSION_NUM\s*:=\s*(\d+\.\d+\.\d+)\s*$`)

// gitlabLintImageRe matches .gitlab-ci.yml's `image: golangci/golangci-lint:vX.Y.Z`.
var gitlabLintImageRe = regexp.MustCompile(`image:\s*golangci/golangci-lint:(v\d+\.\d+\.\d+)`)

// ciActionVersionRe matches .github/workflows/ci.yml's golangci-lint-action
// step: the `uses: golangci/golangci-lint-action@vN` line, then (allowing
// any lines between, including the `with:` key and comments) the actual
// `version: vX.Y.Z` argument passed to it. Scoped to the action block (not
// just "the first vX.Y.Z anywhere in the file") so this does not silently
// start matching kubeconform's or helm's OWN pinned versions instead if
// they ever move earlier in the file.
var ciActionVersionRe = regexp.MustCompile(`(?s)golangci-lint-action@v\d+.*?version:\s*(v\d+\.\d+\.\d+)`)

// extractLintPin runs re against path's content and returns the captured
// version, failing loudly (not silently returning "") if either the file
// cannot be read or the pattern cannot be found -- a pin this test cannot
// locate is a pin this test is not actually checking.
func extractLintPin(t *testing.T, path string, re *regexp.Regexp) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("%s: golangci-lint version pin not found with pattern %s -- has the file's shape changed?", path, re)
	}
	return m[1]
}

// TestLintVersionPinsAgree pins the property Makefile's own lint target
// relies on: the three files that each invoke golangci-lint name the SAME
// release. If they drift, a host binary matching one pin could still lint
// with different rules than CI's pinned image runs, silently.
func TestLintVersionPinsAgree(t *testing.T) {
	root := repoRoot(t)

	// makefileVer is the bare number (no "v"); strip the "v" the other two
	// files spell so all three compare as the same shape.
	makefileVer := extractLintPin(t, filepath.Join(root, "Makefile"), makefileLintVersionRe)
	gitlabVer := strings.TrimPrefix(extractLintPin(t, filepath.Join(root, ".gitlab-ci.yml"), gitlabLintImageRe), "v")
	ciVer := strings.TrimPrefix(extractLintPin(t, filepath.Join(root, ".github", "workflows", "ci.yml"), ciActionVersionRe), "v")

	if makefileVer != gitlabVer {
		t.Errorf("Makefile LINT_VERSION_NUM=%s but .gitlab-ci.yml lint image=v%s -- these must match", makefileVer, gitlabVer)
	}
	if makefileVer != ciVer {
		t.Errorf("Makefile LINT_VERSION_NUM=%s but .github/workflows/ci.yml golangci-lint-action version=v%s -- these must match", makefileVer, ciVer)
	}
}

// TestTheLintPinGateActuallyFails proves extractLintPin's patterns can both
// find a real pin and report failure when one is absent -- a regex that
// silently matches nothing would make TestLintVersionPinsAgree pass
// vacuously (every "" == "" comparison agreeing with every other).
func TestTheLintPinGateActuallyFails(t *testing.T) {
	if m := makefileLintVersionRe.FindStringSubmatch("LINT_VERSION_NUM := 2.12.2\n"); m == nil || m[1] != "2.12.2" {
		t.Fatalf("makefileLintVersionRe did not match a real Makefile line: %v", m)
	}
	if m := makefileLintVersionRe.FindStringSubmatch("LINT_IMAGE ?= golangci/golangci-lint:v2.12.2\n"); m != nil {
		t.Fatalf("makefileLintVersionRe matched a line that does not declare LINT_VERSION_NUM: %v", m)
	}

	if m := gitlabLintImageRe.FindStringSubmatch("  image: golangci/golangci-lint:v2.12.2\n"); m == nil || m[1] != "v2.12.2" {
		t.Fatalf("gitlabLintImageRe did not match a real .gitlab-ci.yml line: %v", m)
	}
	if m := gitlabLintImageRe.FindStringSubmatch("  image: golang:1.26\n"); m != nil {
		t.Fatalf("gitlabLintImageRe matched an unrelated image line: %v", m)
	}

	ciFixture := "      - uses: golangci/golangci-lint-action@v7\n        with:\n          version: v2.12.2\n"
	if m := ciActionVersionRe.FindStringSubmatch(ciFixture); m == nil || m[1] != "v2.12.2" {
		t.Fatalf("ciActionVersionRe did not match a real ci.yml golangci-lint-action block: %v", m)
	}
	// Must NOT reach past the golangci-lint-action block to a LATER step's
	// own version (e.g. helm's `version: v3.19.0`), the exact failure mode
	// an unscoped "first version: line in the file" regex would have.
	otherStepFixture := "      - uses: azure/setup-helm@v4\n        with:\n          version: v3.19.0\n"
	if m := ciActionVersionRe.FindStringSubmatch(otherStepFixture); m != nil {
		t.Fatalf("ciActionVersionRe matched a version: line outside the golangci-lint-action step: %v", m)
	}
}
