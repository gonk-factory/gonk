//go:build images

// The no-latest / digest-pin gate (Task 7 Step 3).
//
// docs/environment.md is unambiguous: "Pin exact tags -- never `latest`."
// Renovate autodiscovers and bumps PINNED tags. It cannot bump `latest`, and
// `latest` cannot be rolled back, cannot be reasoned about, and does not tell
// you what is running.
//
// Unlike every other test in this package, these two are PURE STATIC ANALYSIS
// over files already in this checkout -- no `images` build tag skip-if-absent
// dance, no podman required to run them. They are still gated behind the
// `images` build tag (matching Task 5/6's pattern) because they are about
// image-build inputs, not because they need a container.
package images

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bannedTagPart1/2: the same $(empty)-style split the Makefile's own
// no-latest target uses (see Makefile's BANNED_TAG comment) -- so THIS FILE,
// which necessarily talks about the substring, never contains it as one
// contiguous token and does not trip its own scan.
const bannedTagPart1 = ":late"
const bannedTagPart2 = "st"

var bannedTag = bannedTagPart1 + bannedTagPart2

// scanTargets are exactly what Task 7 Step 3 names: "images/**, Makefile,
// chart/** (Plan 05), test/**". chart/ does not exist yet -- a missing path
// is skipped, not a failure, so this test starts covering it automatically
// the moment Plan 05 adds it, with no edit here.
func scanTargets(root string) []string {
	return []string{
		filepath.Join(root, "images"),
		filepath.Join(root, "Makefile"),
		filepath.Join(root, "chart"),
		filepath.Join(root, "test"),
	}
}

var (
	fromRe  = regexp.MustCompile(`(?i)^\s*FROM\s+(\S+)`)
	asRe    = regexp.MustCompile(`(?i)\bAS\s+(\S+)`)
	imageRe = regexp.MustCompile(`(?i)^\s*image:\s*(\S+)`)
	// wholeVarRe matches a FROM/image ref that is ENTIRELY a single
	// ${VAR} substitution (e.g. `FROM ${DEBIAN_BASE}`) -- images/Dockerfile.controller
	// and this task's own Dockerfile.intake/.meter both do this for their
	// runtime stage. The var's resolved value is checked for a tag/digest by
	// TestBaseImagesArePinnedByDigest instead; this test only needs to not
	// misfire on the unresolved placeholder having no literal colon.
	wholeVarRe = regexp.MustCompile(`^\$\{\w+\}$`)
)

// stageNames collects every Dockerfile multi-stage name ("... AS <name>") so
// a later `FROM <name>` (an internal reference to an earlier stage, not a
// registry pull) is not mistaken for an untagged base image.
func stageNames(lines []string) map[string]bool {
	names := map[string]bool{}
	for _, l := range lines {
		if m := asRe.FindStringSubmatch(l); m != nil {
			names[m[1]] = true
		}
	}
	return names
}

// hasTag reports whether ref carries an explicit tag or digest. A digest
// (`@sha256:...`) always counts. Otherwise the LAST colon must fall after the
// last slash -- `registry.orac.local:5000/x` has a colon that is a port, not
// a tag, and must not be mistaken for one.
func hasTag(ref string) bool {
	if strings.Contains(ref, "@") {
		return true
	}
	lastColon := strings.LastIndex(ref, ":")
	lastSlash := strings.LastIndex(ref, "/")
	return lastColon > lastSlash
}

// TestNothingSaysLatest walks images/**, Makefile, chart/** (Plan 05) and
// test/**, failing on the floating tag this file deliberately never spells
// out contiguously (see bannedTag above), an untagged `FROM`, or an untagged
// `image:` reference (the k8s/Helm-values shape Plan 05 introduces).
// Deliberately blunt string/line matching, not a YAML or Dockerfile parser --
// the whole point of this gate is that it is simple enough to trust.
func TestNothingSaysLatest(t *testing.T) {
	root := repoRoot(t)

	scanFile := func(path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(string(b), "\n")
		stages := stageNames(lines)
		for i, line := range lines {
			if strings.Contains(line, bannedTag) {
				t.Errorf("%s:%d: contains a floating %q tag -- pin an exact tag (docs/environment.md)", path, i+1, bannedTag)
			}
			if m := fromRe.FindStringSubmatch(line); m != nil {
				ref := m[1]
				if ref == "scratch" || stages[ref] || wholeVarRe.MatchString(ref) {
					// "scratch" has no tag by definition; an internal stage
					// reference is not a base image; a bare ${VAR} defers to
					// TestBaseImagesArePinnedByDigest, which resolves it.
					continue
				}
				if !hasTag(ref) {
					t.Errorf("%s:%d: FROM %q has no tag -- pin an exact tag or digest (docs/environment.md)", path, i+1, ref)
				}
			}
			if m := imageRe.FindStringSubmatch(line); m != nil {
				ref := strings.Trim(m[1], `"'`)
				if !hasTag(ref) {
					t.Errorf("%s:%d: image: %q has no tag -- pin an exact tag or digest (docs/environment.md)", path, i+1, ref)
				}
			}
		}
	}

	for _, target := range scanTargets(root) {
		info, err := os.Stat(target)
		if err != nil {
			continue // not present yet (e.g. chart/, Plan 05) -- nothing to scan
		}
		if !info.IsDir() {
			scanFile(target)
			continue
		}
		walkErr := filepath.Walk(target, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.IsDir() {
				return nil
			}
			scanFile(path)
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", target, walkErr)
		}
	}
}

// varRe matches a Dockerfile ARG substitution like ${GO_VERSION}.
var varRe = regexp.MustCompile(`\$\{(\w+)\}`)

// TestBaseImagesArePinnedByDigest walks every images/Dockerfile.* and, for
// every FROM that is NOT an internal multi-stage reference, resolves any
// ${VAR} against images/versions.env and asserts the final image reference
// carries a `@sha256:` digest. A base image without a digest is a base image
// that changed under you -- a tag alone can be re-pointed by the upstream
// registry at any time; a digest cannot.
func TestBaseImagesArePinnedByDigest(t *testing.T) {
	root := repoRoot(t)
	pins := versionPins(t, root)

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
		stages := stageNames(lines)

		for i, line := range lines {
			m := fromRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref := m[1]
			if ref == "scratch" || stages[ref] {
				continue
			}

			resolved := ref
			unresolved := false
			for _, vm := range varRe.FindAllStringSubmatch(ref, -1) {
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
