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
	"fmt"
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

// noLatestFromProseFiles: the ONLY two files in this repo where a real
// English sentence happens to match the case-insensitive `^\s*FROM\s+`
// heuristic without being a Dockerfile instruction --
// chart/gonk/templates/test-netpol-probe.yaml's Helm block-comment prose
// ("...ingress from app=gc-agent on 8080.") and
// chart/gonk/smoke/gc-controller-smoke.md's runbook prose ("From a
// **separate** pod in the ns, ..."). This is an EXCLUDE list, not an
// allow-list of "files that look like Dockerfiles": an earlier version of
// this check only ran the FROM heuristic when the basename started with
// "Dockerfile", which is exactly backwards for a security gate -- an
// untagged `FROM debian` (Docker resolves that to :latest) in a
// differently-named build file such as images/Containerfile or
// images/agent.dockerfile would have escaped silently. Docker does not care
// what the file is called, so this check does not get to either. New prose
// that trips this in the future gets added here; a new build file never
// needs to be.
var noLatestFromProseFiles = []string{
	"chart/gonk/templates/test-netpol-probe.yaml",
	"chart/gonk/smoke/gc-controller-smoke.md",
}

func noLatestIsKnownFromProseFile(path string) bool {
	p := filepath.ToSlash(path)
	for _, suffix := range noLatestFromProseFiles {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// noLatestLineProblems scans already-read file content and returns one
// human-readable problem string per no-latest violation found -- it does not
// touch a *testing.T, so it can be exercised directly by
// TestTheNoLatestGateActuallyFails without needing fixture files on disk.
// Skips Helm template directive lines (`{{ ... }}` is resolved at install
// time, not something this static scan can or should evaluate) and Go
// source entirely (scanned by the normal Go build/vet/test gate instead;
// its own identifiers -- e.g. a struct field named `image` -- are not image
// references).
func noLatestLineProblems(path string, content string) []string {
	if filepath.Ext(path) == ".go" {
		return nil
	}
	skipFrom := noLatestIsKnownFromProseFile(path)

	var problems []string
	lines := strings.Split(content, "\n")
	stages := noLatestStageNames(lines)
	for i, line := range lines {
		if strings.Contains(line, "{{") {
			continue
		}
		if strings.Contains(line, noLatestBannedTag) {
			problems = append(problems, fmt.Sprintf("%s:%d: contains a floating %q tag -- pin an exact tag (docs/environment.md)", path, i+1, noLatestBannedTag))
		}
		if !skipFrom {
			if m := noLatestFromRe.FindStringSubmatch(line); m != nil {
				ref := m[1]
				if ref == "scratch" || stages[ref] || noLatestWholeVarRe.MatchString(ref) {
					// "scratch" has no tag by definition; an internal stage
					// reference is not a base image; a bare ${VAR} defers to
					// TestBaseImagesArePinnedByDigest, which resolves it.
					continue
				}
				if !noLatestHasTag(ref) {
					problems = append(problems, fmt.Sprintf("%s:%d: FROM %q has no tag -- pin an exact tag or digest (docs/environment.md)", path, i+1, ref))
				}
			}
		}
		if m := noLatestImageRe.FindStringSubmatch(line); m != nil {
			ref := strings.Trim(m[1], `"'`)
			if !noLatestHasTag(ref) {
				problems = append(problems, fmt.Sprintf("%s:%d: image: %q has no tag -- pin an exact tag or digest (docs/environment.md)", path, i+1, ref))
			}
		}
	}
	return problems
}

// noLatestScanFile reads path off disk and reports every problem
// noLatestLineProblems finds in it.
func noLatestScanFile(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, p := range noLatestLineProblems(path, string(b)) {
		t.Error(p)
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

// TestTheNoLatestGateActuallyFails proves each violation shape
// noLatestLineProblems exists to catch actually produces a problem. A check
// whose failure path is never exercised is a check that can quietly stop
// checking -- see the Makefile's own no-latest recipe, whose `!` idiom did
// exactly that for a grep error until T-04.
//
// The "untagged FROM in a file NOT named Dockerfile*" case pins a regression:
// an earlier version of this file only ran the FROM check when the file's
// basename started with "Dockerfile", which let an untagged `FROM debian`
// (Docker resolves that to :latest) in e.g. images/Containerfile or
// images/agent.dockerfile escape both this test and `make no-latest` (whose
// grep only matches the literal banned substring, not an absent tag)
// entirely. Docker does not care what the build file is named, so this test
// does not get to either.
func TestTheNoLatestGateActuallyFails(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		content  string
		wantSaid string
	}{
		{
			name:     "explicit banned tag, in a file that is NOT a Dockerfile",
			path:     "chart/gonk/values.yaml",
			content:  "repository: registry.example/gonk-agent" + noLatestBannedTag + "\n",
			wantSaid: "floating",
		},
		{
			name:     "untagged FROM in a file named Dockerfile.*",
			path:     "images/Dockerfile.agent",
			content:  "FROM debian\n",
			wantSaid: "has no tag",
		},
		{
			name:     "untagged FROM in a build file NOT named Dockerfile* -- the exact hole an earlier isDockerfile allow-list left open",
			path:     "images/Containerfile",
			content:  "FROM debian\n",
			wantSaid: "has no tag",
		},
		{
			name:     "untagged FROM in a .dockerfile-suffixed file, also not matching Dockerfile*",
			path:     "images/agent.dockerfile",
			content:  "FROM debian\n",
			wantSaid: "has no tag",
		},
		{
			name:     "untagged image: reference",
			path:     "chart/gonk/templates/workload-gonk-controller.yaml",
			content:  "image: registry.example/gonk-agent\n",
			wantSaid: "has no tag",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := noLatestLineProblems(tc.path, tc.content)
			if len(problems) == 0 {
				t.Fatalf("noLatestLineProblems(%q, %q) found nothing -- this is exactly the shape the gate exists to catch", tc.path, tc.content)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.wantSaid) {
				t.Errorf("wanted %q in:\n%s", tc.wantSaid, strings.Join(problems, "\n"))
			}
		})
	}

	// And the passing cases, so the above is not vacuously true of every
	// input: a correctly digest-pinned FROM in a non-Dockerfile-named file,
	// and the two known prose files that must NOT be flagged for saying
	// "from" in an English sentence.
	if p := noLatestLineProblems("images/Containerfile", "FROM debian:12@sha256:"+strings.Repeat("a", 64)+"\n"); len(p) != 0 {
		t.Errorf("noLatestLineProblems rejected a correctly pinned FROM: %v", p)
	}
	if p := noLatestLineProblems("chart/gonk/templates/test-netpol-probe.yaml", "  from app=gc-agent on 8080.\n"); len(p) != 0 {
		t.Errorf("noLatestLineProblems flagged known NetworkPolicy prose as an untagged FROM: %v", p)
	}
	if p := noLatestLineProblems("chart/gonk/smoke/gc-controller-smoke.md", "From a **separate** pod in the ns\n"); len(p) != 0 {
		t.Errorf("noLatestLineProblems flagged known runbook prose as an untagged FROM: %v", p)
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
