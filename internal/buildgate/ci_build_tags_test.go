// This file is T-14's own deliverable: a red test that makes absent CI
// coverage visible instead of letting a `//go:build` tag sit unrun forever.
//
// FOUR tagged suites (component, integration, images, live) have never run in
// any CI job -- their `t.Skip`s on missing infrastructure read as green
// locally and are simply never executed on a runner at all. `chart` DOES run
// (the `chart` job in .github/workflows/ci.yml), and `testclock` names no
// tagged _test.go file of its own -- it gates
// cmd/gonk-meter/clock_testclock.go, production code baked into the
// meter-testclock image, verified only by test/images' `images`-tagged
// smoke tests -- but the rule this test enforces does not know any of that
// history. It asserts one thing, mechanically, for every tag it finds: a
// `go test … -tags <tag>` line exists somewhere in ci.yml. That is
// deliberately blunter than "is this suite meaningfully exercised" -- see
// TestEveryBuildTagRunsInCI's own doc comment for why.
//
// TestEveryBuildTagRunsInCI is EXPECTED TO FAIL right now, for `component`,
// `integration`, `images`, `live` and `testclock` -- T-15, T-16 and T-17 add
// the CI jobs that cover the first three of those (T-14 deliberately does
// not: "this task makes their absence a red test, which is the point").
// `testclock` has no CI job named in the delivery plan at all yet, so it
// stays red past T-17 until something adds one.
//
// A test that is meant to be red cannot be allowed to fail `make gate` for
// everyone else, so:
//
//   - ci.yml's own `go test ./... -race -count=1` step excludes this test by
//     name with `-skip`, dated, so CI's hermetic gate job stays green.
//   - the Makefile's `test` target carries the SAME exclusion, for the same
//     reason: it is `go test ./... -race -count=1` too, and this package
//     (buildgate) runs in the ordinary gate on purpose (see
//     nolatest_test.go's package doc). Without a matching exclusion here,
//     `make gate` would go red for every contributor on every branch, which
//     is a worse failure than the absent CI coverage this test exists to
//     surface -- a gate nobody can run is the same as no gate.
//   - the test remains reachable, on purpose, by invoking the package
//     directly: `go test ./internal/buildgate/ -count=1 -v` (T-14's own
//     Verify command) runs with NEITHER exclusion applied, so it shows the
//     real, current red result.
//
// TODO(T-15/T-16/T-17, filed 2026-09-08): once those land the component,
// integration and images CI jobs, narrow (or drop) the `-skip` exclusion in
// ci.yml and in the Makefile's `test` target to match, and confirm this test
// still correctly reports `testclock` (and `live`, until T-17) red rather
// than passing vacuously.
package buildgate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// buildTagTrees are exactly the trees the task names: "every file with
// //go:build under cmd/ pkg/ internal/ test/". A tag declared anywhere else
// (e.g. vendor/, which carries plenty of its own //go:build constraints for
// upstream portability) is out of scope -- those are not OUR suites.
func buildTagTrees(root string) []string {
	return []string{
		filepath.Join(root, "cmd"),
		filepath.Join(root, "pkg"),
		filepath.Join(root, "internal"),
		filepath.Join(root, "test"),
	}
}

// goBuildLineRe matches a //go:build constraint line and captures the whole
// expression after it (e.g. "chart", "!testclock", "a && !b").
var goBuildLineRe = regexp.MustCompile(`(?m)^//go:build (.+)$`)

// buildTagIdentRe pulls bare identifiers out of a build constraint
// expression, discarding the `!`, `&&`, `||`, `(` and `)` operators around
// them -- so "!testclock" and "testclock" both yield the identifier
// "testclock". A tag is a tag whether the file requires its presence or its
// absence; either way `-tags testclock` is what controls it.
var buildTagIdentRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// standardBuildTags are Go's own recognized GOOS/GOARCH/toolchain build tags
// (a non-exhaustive but common set: unix, cgo, and the platforms this repo's
// vendor tree actually uses //go:build for). None of these name a gonk test
// suite that a CI job could sensibly be asked to run with `-tags <goos>` --
// excluding them here keeps this test asking only about OUR opt-in suites,
// not about "does CI run go test on linux/amd64" (it does, trivially, by
// running at all). This is an exclude-list, not an allow-list: a NEW custom
// tag this repo's own code declares is never accidentally filtered out by
// it, only the small fixed set of tags the Go toolchain itself defines.
var standardBuildTags = map[string]bool{
	"linux": true, "darwin": true, "windows": true, "freebsd": true,
	"openbsd": true, "netbsd": true, "plan9": true, "js": true, "wasm": true,
	"amd64": true, "arm64": true, "arm": true, "386": true, "mips": true,
	"mips64": true, "ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true,
	"cgo": true, "unix": true, "purego": true, "go1_1": true,
}

// buildConstraintTags extracts every non-standard identifier named by a
// //go:build expression. Pure (no *testing.T, no filesystem) so the
// negative control below can exercise it directly.
func buildConstraintTags(expr string) []string {
	var tags []string
	for _, tok := range buildTagIdentRe.FindAllString(expr, -1) {
		if standardBuildTags[tok] {
			continue
		}
		tags = append(tags, tok)
	}
	return tags
}

// repoBuildTags walks buildTagTrees and returns the sorted, deduplicated set
// of every custom build tag declared by a //go:build line under them.
func repoBuildTags(t *testing.T, root string) []string {
	t.Helper()
	tagSet := map[string]bool{}
	for _, dir := range buildTagTrees(root) {
		info, err := os.Stat(dir)
		if err != nil {
			continue // e.g. cmd/ or pkg/ absent in some future layout: nothing to scan
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, m := range goBuildLineRe.FindAllStringSubmatch(string(b), -1) {
				for _, tag := range buildConstraintTags(m[1]) {
					tagSet[tag] = true
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}
	tags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// ciGoTestTagLineRe matches a single shell line invoking `go test` with a
// `-tags` flag, in EITHER `-tags foo` or `-tags=foo` form, and captures the
// tag argument -- which may itself be a comma-separated list
// (`-tags a,b,c`), the same syntax `go test` itself accepts.
var ciGoTestTagLineRe = regexp.MustCompile(`(?m)^.*\bgo test\b.*[ =]-tags[= ]+(\S+).*$`)

// ciCoveredTagsFromText returns the set of tags named by any
// `go test … -tags <tag>` line in ciYML's raw text. Pure, so the negative
// control below can exercise it directly without a fixture file on disk.
func ciCoveredTagsFromText(ciYML string) map[string]bool {
	covered := map[string]bool{}
	for _, m := range ciGoTestTagLineRe.FindAllStringSubmatch(ciYML, -1) {
		for _, tag := range strings.Split(m[1], ",") {
			tag = strings.Trim(tag, `"'`)
			if tag != "" {
				covered[tag] = true
			}
		}
	}
	return covered
}

// TestEveryBuildTagRunsInCI is the enumerate-and-assert gate the task calls
// for: every custom //go:build tag under cmd/, pkg/, internal/ and test/
// must appear in a `go test … -tags <tag>` line in
// .github/workflows/ci.yml, or CI has never actually run that suite and a
// `t.Skip` inside it reads as green for a job that does not exist.
//
// This is deliberately a MECHANICAL check (does the tag string appear on a
// `go test -tags` line), not a semantic one (does the job that runs it also
// pass, carry the right services, run on every push). Doing more than that
// is T-15/T-16/T-17's job; this test's whole purpose is to be the thing that
// makes "the tag has zero coverage" visible before those land, and to keep
// being useful afterward as the tripwire for a fifth tag someday added
// without anyone wiring up its CI job.
func TestEveryBuildTagRunsInCI(t *testing.T) {
	root := repoRoot(t)
	tags := repoBuildTags(t, root)

	// Guard the guard: a scan that silently found nothing would make every
	// assertion below vacuously true.
	if len(tags) == 0 {
		t.Fatal("found zero //go:build tags under cmd/ pkg/ internal/ test/ -- the scan is broken, not the repo")
	}

	ciYMLPath := filepath.Join(root, ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(ciYMLPath)
	if err != nil {
		t.Fatalf("read %s: %v", ciYMLPath, err)
	}
	covered := ciCoveredTagsFromText(string(b))

	t.Logf("build tags found under cmd/ pkg/ internal/ test/: %s", strings.Join(tags, ", "))

	var missing []string
	for _, tag := range tags {
		if !covered[tag] {
			missing = append(missing, tag)
		}
	}
	if len(missing) > 0 {
		t.Errorf("build tag(s) with no `go test … -tags <tag>` line in .github/workflows/ci.yml: %s -- "+
			"a suite behind one of these tags has NEVER run in CI, and every t.Skip inside it reads as green "+
			"for a job that does not exist (R-40, R-43, R-44; T-15/T-16/T-17 close these one at a time)",
			strings.Join(missing, ", "))
	}
}

// TestTheBuildTagCIGateActuallyFails proves both halves of
// TestEveryBuildTagRunsInCI's machinery can fail: a tag with no covering
// line is reported, and a tag that IS covered is not. Without this, a typo
// in ciGoTestTagLineRe or buildConstraintTags could silently degrade the
// gate to "always passes" and nothing would notice.
func TestTheBuildTagCIGateActuallyFails(t *testing.T) {
	ciYML := "" +
		"      - name: go test -race\n" +
		"        run: go test ./... -race -count=1\n" +
		"      - name: chart gate\n" +
		"        run: go test -tags chart ./internal/charttest/... -count=1\n"
	covered := ciCoveredTagsFromText(ciYML)

	if covered["images"] {
		t.Fatalf("ciCoveredTagsFromText reported %q covered by a fixture that never mentions it: %v", "images", covered)
	}
	if !covered["chart"] {
		t.Fatalf("ciCoveredTagsFromText did not find the real `-tags chart` line: %v", covered)
	}

	// The comma-separated and `-tags=` forms must both parse, or a future CI
	// line written either way silently fails to register as coverage.
	multi := ciCoveredTagsFromText("run: go test -tags=live,integration ./... -count=1\n")
	if !multi["live"] || !multi["integration"] {
		t.Fatalf("ciCoveredTagsFromText did not parse a comma-separated -tags= line: %v", multi)
	}
}

// TestBuildConstraintTagsIgnoresStandardTagsAndNegation pins the two shapes
// this repo's own go:build lines actually use --
// cmd/gonk-meter/clock.go's "!testclock" and clock_testclock.go's
// "testclock" -- to the SAME identifier, and confirms a bare GOOS/GOARCH tag
// (which is not one of our opt-in suites) is filtered out rather than
// demanded of ci.yml.
func TestBuildConstraintTagsIgnoresStandardTagsAndNegation(t *testing.T) {
	cases := []struct {
		expr string
		want []string
	}{
		{"testclock", []string{"testclock"}},
		{"!testclock", []string{"testclock"}},
		{"chart", []string{"chart"}},
		{"linux && amd64", nil},
		{"linux && custom", []string{"custom"}},
	}
	for _, tc := range cases {
		got := buildConstraintTags(tc.expr)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("buildConstraintTags(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

// TestRepoBuildTagsFindsTheKnownSixMatchesTheTaskList pins the exact set
// T-14's task text names -- component, integration, images, live, chart,
// testclock -- against what a real scan of this checkout finds, so a future
// change to any tagged file's build line is caught here even before it
// reaches TestEveryBuildTagRunsInCI's ci.yml comparison.
func TestRepoBuildTagsFindsTheKnownSixMatchesTheTaskList(t *testing.T) {
	want := []string{"chart", "component", "images", "integration", "live", "testclock"}
	got := repoBuildTags(t, repoRoot(t))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("repoBuildTags = %v, want %v -- either a tag was added/removed, or the scan drifted from the "+
			"task's own enumeration; either way this needs a human to look, not a silent pass", got, want)
	}
}
