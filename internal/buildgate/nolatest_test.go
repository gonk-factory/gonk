// The no-latest / digest-pin gate (originally Task 7 Step 3, moved here by
// T-04 so it actually runs).
//
// docs/environment.md is unambiguous: "Pin exact tags -- never `latest`."
// Renovate autodiscovers and bumps PINNED tags. It cannot bump `latest`, and
// `latest` cannot be rolled back, cannot be reasoned about, and does not tell
// you what is running.
//
// This used to live at test/images/nolatest_test.go behind the `images`
// build tag, alongside tests that genuinely need a built container. These
// two tests are pure static analysis over files already in this checkout --
// no podman required -- so gating them behind `images` only meant they never
// ran in the default `go test ./...` gate (nor in CI's test job, which does
// not pass -tags images). A guard nobody runs is the same as no guard. They
// live in package buildgate now for the same reason push_test.go does: this
// package runs in the ordinary gate on purpose.
package buildgate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// noLatestBannedTagPart1/2: the same $(empty)-style split the Makefile's own
// no-latest target uses (see Makefile's BANNED_TAG comment) -- so THIS FILE,
// which necessarily talks about the substring, never contains it as one
// contiguous token and does not trip its own scan (or the Makefile's).
const noLatestBannedTagPart1 = ":late"
const noLatestBannedTagPart2 = "st"

var noLatestBannedTag = noLatestBannedTagPart1 + noLatestBannedTagPart2

// noLatestScanTargets are exactly what the Makefile's no-latest target scans:
// images/**, Makefile, chart/**, test/**. A missing path (e.g. chart/ before
// Plan 05 landed) is skipped, not a failure.
func noLatestScanTargets(root string) []string {
	return []string{
		filepath.Join(root, "images"),
		filepath.Join(root, "Makefile"),
		filepath.Join(root, "chart"),
		filepath.Join(root, "test"),
	}
}

var (
	noLatestFromRe  = regexp.MustCompile(`(?i)^\s*FROM\s+(\S+)`)
	noLatestAsRe    = regexp.MustCompile(`(?i)\bAS\s+(\S+)`)
	noLatestImageRe = regexp.MustCompile(`(?i)^\s*image:\s*(\S+)`)
	// noLatestWholeVarRe matches a FROM/image ref that is ENTIRELY a single
	// ${VAR} substitution (e.g. `FROM ${DEBIAN_BASE}`) -- images/Dockerfile.controller
	// and Dockerfile.intake/.meter's runtime stages all do this. The var's
	// resolved value is checked for a tag/digest by
	// TestBaseImagesArePinnedByDigest instead; this test only needs to not
	// misfire on the unresolved placeholder having no literal colon.
	noLatestWholeVarRe = regexp.MustCompile(`^\$\{\w+\}$`)
)

// noLatestStageNames collects every Dockerfile multi-stage name
// ("... AS <name>") so a later `FROM <name>` (an internal reference to an
// earlier stage, not a registry pull) is not mistaken for an untagged base
// image.
func noLatestStageNames(lines []string) map[string]bool {
	names := map[string]bool{}
	for _, l := range lines {
		if m := noLatestAsRe.FindStringSubmatch(l); m != nil {
			names[m[1]] = true
		}
	}
	return names
}

// noLatestHasTag reports whether ref carries an explicit tag or digest. A
// digest (`@sha256:...`) always counts. Otherwise the LAST colon must fall
// after the last slash -- `registry.orac.local:5000/x` has a colon that is a
// port, not a tag, and must not be mistaken for one.
func noLatestHasTag(ref string) bool {
	if strings.Contains(ref, "@") {
		return true
	}
	lastColon := strings.LastIndex(ref, ":")
	lastSlash := strings.LastIndex(ref, "/")
	return lastColon > lastSlash
}

// noLatestSkipDir reports whether a directory must never be descended into:
//
//   - chart/gonk/tests/crd-schemas: vendored upstream CRD schemas (CNPG,
//     prometheus-operator) whose JSON legitimately contains the literal
//     string ":latest" inside Kubernetes' own imagePullPolicy documentation
//     ("Defaults to Always if :latest tag is specified..."). That is
//     upstream API prose describing k8s's default pull-policy behavior, not
//     one of OUR image tags.
//   - test/harness: Go test-support code, not a build input; scanned
//     source there (e.g. a struct field literally named `image`) has tripped
//     the `image:`-shaped heuristic before.
func noLatestSkipDir(root, path string) bool {
	if filepath.Base(path) == "crd-schemas" {
		return true
	}
	if path == filepath.Join(root, "test", "harness") {
		return true
	}
	return false
}

// noLatestScanFile runs the no-latest checks over one file's lines, skipping
// Helm template directives (which legitimately embed `{{ ... }}` in place of
// a literal tag -- that is resolved at install time, not something this
// static scan can or should evaluate) and Go source (which is scanned by the
// normal Go build/vet/test gate, not this Dockerfile/Helm-values heuristic,
// and whose own identifiers -- e.g. a struct field named `image`, or this
// file's own doc comments about the scan -- are not image references).
func noLatestScanFile(t *testing.T, path string) {
	t.Helper()
	if filepath.Ext(path) == ".go" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// The FROM heuristic only makes sense for actual Dockerfiles: a line
	// beginning "from ..." is a real English word too, and Helm block
	// comments (chart/gonk/templates/test-netpol-probe.yaml's NetworkPolicy
	// prose, e.g.) contain sentences like "...ingress from app=gc-agent on
	// 8080." that are not FROM instructions. Every FROM instruction in this
	// repo lives in a file named Dockerfile* (images/Dockerfile.*,
	// images/stubmodel/Dockerfile); restricting the check to those loses no
	// real coverage.
	isDockerfile := strings.HasPrefix(filepath.Base(path), "Dockerfile")

	lines := strings.Split(string(b), "\n")
	stages := noLatestStageNames(lines)
	for i, line := range lines {
		if strings.Contains(line, "{{") {
			continue
		}
		if strings.Contains(line, noLatestBannedTag) {
			t.Errorf("%s:%d: contains a floating %q tag -- pin an exact tag (docs/environment.md)", path, i+1, noLatestBannedTag)
		}
		if isDockerfile {
			if m := noLatestFromRe.FindStringSubmatch(line); m != nil {
				ref := m[1]
				if ref == "scratch" || stages[ref] || noLatestWholeVarRe.MatchString(ref) {
					// "scratch" has no tag by definition; an internal stage
					// reference is not a base image; a bare ${VAR} defers to
					// TestBaseImagesArePinnedByDigest, which resolves it.
					continue
				}
				if !noLatestHasTag(ref) {
					t.Errorf("%s:%d: FROM %q has no tag -- pin an exact tag or digest (docs/environment.md)", path, i+1, ref)
				}
			}
		}
		if m := noLatestImageRe.FindStringSubmatch(line); m != nil {
			ref := strings.Trim(m[1], `"'`)
			if !noLatestHasTag(ref) {
				t.Errorf("%s:%d: image: %q has no tag -- pin an exact tag or digest (docs/environment.md)", path, i+1, ref)
			}
		}
	}
}

// TestNothingSaysLatest walks images/**, Makefile, chart/** and test/**,
// failing on the floating tag this file deliberately never spells out
// contiguously (see noLatestBannedTag above), an untagged `FROM`, or an
// untagged `image:` reference (the k8s/Helm-values shape). Deliberately
// blunt string/line matching, not a YAML or Dockerfile parser -- the whole
// point of this gate is that it is simple enough to trust.
func TestNothingSaysLatest(t *testing.T) {
	root := repoRoot(t)

	for _, target := range noLatestScanTargets(root) {
		info, err := os.Stat(target)
		if err != nil {
			continue // not present yet -- nothing to scan
		}
		if !info.IsDir() {
			noLatestScanFile(t, target)
			continue
		}
		walkErr := filepath.Walk(target, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.IsDir() {
				if path != target && noLatestSkipDir(root, path) {
					return filepath.SkipDir
				}
				return nil
			}
			noLatestScanFile(t, path)
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", target, walkErr)
		}
	}
}

// noLatestVarRe matches a Dockerfile ARG substitution like ${GO_VERSION}.
var noLatestVarRe = regexp.MustCompile(`\$\{(\w+)\}`)

// noLatestVersionPins reads images/versions.env the same way the Makefile
// does: a flat KEY=value file, `#`-comments allowed after a value. This is
// the SAME file `make images` builds from -- if this parse and the
// Makefile's `include` ever disagree, that is a bug in this file, not in
// versions.env.
func noLatestVersionPins(t *testing.T, root string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "images", "versions.env"))
	if err != nil {
		t.Fatalf("read images/versions.env: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		val := kv[1]
		if i := strings.Index(val, "#"); i >= 0 {
			val = val[:i]
		}
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(val)
	}
	return out
}

// TestBaseImagesArePinnedByDigest walks every images/Dockerfile.* and, for
// every FROM that is NOT an internal multi-stage reference, resolves any
// ${VAR} against images/versions.env and asserts the final image reference
// carries a `@sha256:` digest. A base image without a digest is a base image
// that changed under you -- a tag alone can be re-pointed by the upstream
// registry at any time; a digest cannot.
func TestBaseImagesArePinnedByDigest(t *testing.T) {
	root := repoRoot(t)
	pins := noLatestVersionPins(t, root)

	matches, err := filepath.Glob(filepath.Join(root, "images", "Dockerfile.*"))
	if err != nil {
		t.Fatalf("glob images/Dockerfile.*: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no images/Dockerfile.* found -- nothing to check (this test should never silently pass on zero Dockerfiles)")
	}

	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(string(b), "\n")
		stages := noLatestStageNames(lines)

		for i, line := range lines {
			m := noLatestFromRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref := m[1]
			if ref == "scratch" || stages[ref] {
				continue
			}

			resolved := ref
			unresolved := false
			for _, vm := range noLatestVarRe.FindAllStringSubmatch(ref, -1) {
				val, ok := pins[vm[1]]
				if !ok {
					t.Errorf("%s:%d: FROM %q references undefined ${%s} (images/versions.env)", path, i+1, ref, vm[1])
					unresolved = true
					continue
				}
				resolved = strings.ReplaceAll(resolved, "${"+vm[1]+"}", val)
			}
			if unresolved {
				continue
			}

			if !strings.Contains(resolved, "@sha256:") {
				t.Errorf("%s:%d: base image %q (resolved: %q) is not pinned by digest -- "+
					"a base image without a digest is a base image that changed under you", path, i+1, ref, resolved)
			}
		}
	}
}
