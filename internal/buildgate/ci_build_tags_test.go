// The build-tag CI gate: every `//go:build` tag this repo declares must be
// exercised by a CI job, and this test says so mechanically.
//
// T-14 introduced it as a DELIBERATELY RED test -- four tagged suites
// (component, integration, images, live) had never run in any CI job, and
// their `t.Skip`s on missing infrastructure read as green while being
// executed nowhere. T-15/T-16/T-17 landed those jobs. This file is what
// remains once they have: a live tripwire, not a to-do list, and it is
// GREEN -- `make test` and ci.yml no longer carry a `-skip` exclusion for
// it, because a red gate is never acceptable (delivery plan rule 4b).
//
// WHAT CHANGED IN THE ASSERTION ITSELF (gonk-0bvc). The original rule was
// "every tag appears in a `go test … -tags <tag>` line". That rule is wrong
// for `testclock`, and demonstrably unsatisfiable: `testclock` guards no
// `_test.go` file anywhere. It swaps a clock implementation into the meter
// binary (cmd/gonk-meter/clock_testclock.go, "THIS FILE MUST NEVER BE IN A
// PRODUCTION IMAGE") so an e2e run can move time. There is no suite to run
// under it, so demanding a `go test -tags testclock` job asks CI to run
// zero tests and call that coverage -- exactly the silent-skip failure this
// gate exists to prevent.
//
// So tags are now classified FROM THE TREE, never from a hardcoded list of
// names (a hardcoded list would let the next modifier tag reintroduce this
// same argument):
//
//   - SUITE tag: guards at least one `_test.go` file. There are tests behind
//     it, so CI must RUN them -- a `go test … -tags <tag>` line.
//   - MODIFIER tag: guards only non-test files. There is nothing to run, so
//     the honest requirement is that the tagged build still TYPE-CHECKS --
//     a `go vet … -tags <tag>` line. Untagged `go vet ./...` never compiles
//     those files at all, so without this they are checked by nothing.
//
// The scan covers EVERY workflow under .github/workflows/, not one named
// file: the question is "does CI run this suite", not "does ci.yml mention
// it". T-17's nightly `live` workflow is a separate file precisely because
// it is scheduled rather than per-push.
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
// negative controls below can exercise it directly.
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

// tagKind is the whole point of gonk-0bvc: what a build tag GUARDS decides
// what CI owes it.
type tagKind int

const (
	// tagSuite guards at least one _test.go file: CI must RUN those tests.
	tagSuite tagKind = iota
	// tagModifier guards only non-test files: there is nothing to run, so
	// CI must at least TYPE-CHECK the tagged build.
	tagModifier
)

func (k tagKind) String() string {
	if k == tagSuite {
		return "suite"
	}
	return "modifier"
}

// classifyBuildTags maps every custom build tag to its kind, given the
// //go:build expressions declared by each file path. Derived entirely from
// the tree: a tag is a suite if ANY file that names it is a _test.go file.
//
// Pure, so the negative controls below can prove BOTH directions without
// planting files in the real repo.
func classifyBuildTags(decls map[string][]string) map[string]tagKind {
	kinds := map[string]tagKind{}
	for path, exprs := range decls {
		isTestFile := strings.HasSuffix(path, "_test.go")
		for _, expr := range exprs {
			for _, tag := range buildConstraintTags(expr) {
				if isTestFile {
					kinds[tag] = tagSuite
					continue
				}
				if _, seen := kinds[tag]; !seen {
					kinds[tag] = tagModifier
				}
			}
		}
	}
	return kinds
}

// repoBuildTagDecls walks buildTagTrees and returns, per .go file that has
// one, the //go:build expressions it declares.
func repoBuildTagDecls(t *testing.T, root string) map[string][]string {
	t.Helper()
	decls := map[string][]string{}
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
			if fi.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, m := range goBuildLineRe.FindAllStringSubmatch(string(b), -1) {
				decls[path] = append(decls[path], m[1])
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}
	return decls
}

// sortedTagNames is a stable rendering helper for failure messages.
func sortedTagNames(kinds map[string]tagKind) []string {
	names := make([]string, 0, len(kinds))
	for tag := range kinds {
		names = append(names, tag)
	}
	sort.Strings(names)
	return names
}

// ignoredCoverageLineRe drops lines that only TALK about a command: YAML and
// shell comments, and a step's `name:`. This is load-bearing, not tidiness.
// Before it, ci.yml's `- name: go test -tags component (test/component)`
// step titles and a prose comment ("go test -tags component still covers
// it") were what satisfied this gate -- the actual `run:` lines invoke the
// wrapper script, not `go test`. Coverage claimed by a job TITLE is coverage
// that survives deleting the command underneath it.
var ignoredCoverageLineRe = regexp.MustCompile(`^\s*(#|-\s*name:\s|name:\s)`)

// goTestInvocationRe recognizes a line that actually runs the Go test
// binary. hack/require_nonzero_go_tests.sh counts as one because it IS one:
// it execs `go test -v "$@"` and additionally fails the job if zero tests
// ran, which is strictly stronger than a bare `go test`.
var goTestInvocationRe = regexp.MustCompile(`\bgo test\b|require_nonzero_go_tests\.sh\b`)

// goVetInvocationRe recognizes a line that type-checks a tagged build.
var goVetInvocationRe = regexp.MustCompile(`\bgo vet\b`)

// tagsFlagRe captures the argument of a `-tags` flag in either `-tags foo`
// or `-tags=foo` form. The argument may itself be a comma-separated list
// (`-tags a,b,c`), the same syntax the go tool accepts.
var tagsFlagRe = regexp.MustCompile(`[ =]-tags[= ]+(\S+)`)

// taggedInvocations returns the set of build tags named by a `-tags` flag on
// any non-comment line matching invocation. Pure, so the negative controls
// below can exercise it without fixture files on disk.
func taggedInvocations(text string, invocation *regexp.Regexp) map[string]bool {
	found := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if ignoredCoverageLineRe.MatchString(line) || !invocation.MatchString(line) {
			continue
		}
		for _, m := range tagsFlagRe.FindAllStringSubmatch(line, -1) {
			for _, tag := range strings.Split(m[1], ",") {
				tag = strings.Trim(tag, `"'`)
				if tag != "" {
					found[tag] = true
				}
			}
		}
	}
	return found
}

// workflowText concatenates every workflow under .github/workflows/. The
// gate asks "does CI run this suite ANYWHERE", so a suite covered by a
// scheduled workflow of its own (T-17's nightly `live` run) counts exactly
// as much as one covered by ci.yml.
func workflowText(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var parts []string
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", e.Name(), readErr)
		}
		names = append(names, e.Name())
		parts = append(parts, string(b))
	}
	if len(parts) == 0 {
		t.Fatalf("no workflow files found under %s -- the scan is broken, not the repo", dir)
	}
	sort.Strings(names)
	t.Logf("workflows scanned: %s", strings.Join(names, ", "))
	return strings.Join(parts, "\n")
}

// missingTagCoverage is the rule itself, as a pure function of (what the
// tree declares, what CI runs). Suites owe a `go test … -tags`; modifiers
// owe a `go vet … -tags`.
func missingTagCoverage(kinds map[string]tagKind, testTags, vetTags map[string]bool) (missingSuites, missingModifiers []string) {
	for _, tag := range sortedTagNames(kinds) {
		switch kinds[tag] {
		case tagSuite:
			if !testTags[tag] {
				missingSuites = append(missingSuites, tag)
			}
		case tagModifier:
			if !vetTags[tag] {
				missingModifiers = append(missingModifiers, tag)
			}
		}
	}
	return missingSuites, missingModifiers
}

// TestEveryBuildTagRunsInCI is the enumerate-and-assert gate: every custom
// //go:build tag under cmd/, pkg/, internal/ and test/ must be exercised by
// a CI job under .github/workflows/ -- suites run, modifiers type-check --
// or CI has never touched that code and a `t.Skip` inside it reads as green
// for a job that does not exist.
//
// This is deliberately a MECHANICAL check (does the tag appear on a real
// `go test`/`go vet` command line), not a semantic one (does that job also
// pass, carry the right services, run on every push). Its purpose is to
// make "this tag has zero coverage" impossible to add silently.
func TestEveryBuildTagRunsInCI(t *testing.T) {
	root := repoRoot(t)
	kinds := classifyBuildTags(repoBuildTagDecls(t, root))

	// Guard the guard: a scan that silently found nothing would make every
	// assertion below vacuously true.
	if len(kinds) == 0 {
		t.Fatal("found zero //go:build tags under cmd/ pkg/ internal/ test/ -- the scan is broken, not the repo")
	}

	yml := workflowText(t, root)
	testTags := taggedInvocations(yml, goTestInvocationRe)
	vetTags := taggedInvocations(yml, goVetInvocationRe)

	for _, tag := range sortedTagNames(kinds) {
		t.Logf("build tag %-12s %s", tag, kinds[tag])
	}

	missingSuites, missingModifiers := missingTagCoverage(kinds, testTags, vetTags)
	if len(missingSuites) > 0 {
		t.Errorf("SUITE build tag(s) with no `go test … -tags <tag>` line in any .github/workflows/ file: %s -- "+
			"these tags guard _test.go files, so there ARE tests behind them and CI runs none of them; every "+
			"t.Skip inside reads as green for a job that does not exist (R-40, R-43, R-44)",
			strings.Join(missingSuites, ", "))
	}
	if len(missingModifiers) > 0 {
		t.Errorf("MODIFIER build tag(s) with no `go vet … -tags <tag>` line in any .github/workflows/ file: %s -- "+
			"these tags guard only non-test files, so there is no suite to run, but an untagged `go vet ./...` "+
			"never compiles them either: without a tagged vet they are type-checked by nothing (gonk-0bvc)",
			strings.Join(missingModifiers, ", "))
	}
}

// TestBuildTagKindDecidesWhatCIOwes is the two-directional negative control
// the suite/modifier split lives or dies on. A gate that only proved the
// modifier direction would let a real suite tag silently lose its job.
func TestBuildTagKindDecidesWhatCIOwes(t *testing.T) {
	decls := map[string][]string{
		"test/newsuite/thing_test.go":  {"newsuite"},
		"cmd/gonk-meter/clock_fake.go": {"fakeclock"},
		"cmd/gonk-meter/clock.go":      {"!fakeclock"},
	}
	kinds := classifyBuildTags(decls)
	if kinds["newsuite"] != tagSuite {
		t.Fatalf("a tag guarding a _test.go file classified as %v, want suite", kinds["newsuite"])
	}
	if kinds["fakeclock"] != tagModifier {
		t.Fatalf("a tag guarding only non-test files classified as %v, want modifier", kinds["fakeclock"])
	}

	// DIRECTION 1: a suite tag with only a vet line is NOT covered. This is
	// the direction a modifier-only gate would lose.
	vetOnly := taggedInvocations("        run: go vet -tags newsuite ./...\n", goVetInvocationRe)
	testOnly := taggedInvocations("        run: go vet -tags newsuite ./...\n", goTestInvocationRe)
	missingSuites, missingModifiers := missingTagCoverage(kinds, testOnly, vetOnly)
	if len(missingSuites) != 1 || missingSuites[0] != "newsuite" {
		t.Fatalf("a SUITE tag with only a `go vet -tags` line was reported covered: missing suites %v", missingSuites)
	}
	if len(missingModifiers) != 1 || missingModifiers[0] != "fakeclock" {
		t.Fatalf("a MODIFIER tag with no `go vet -tags` line was reported covered: missing modifiers %v", missingModifiers)
	}

	// DIRECTION 2: the modifier is satisfied by a vet line and does NOT
	// additionally demand a test job; the suite is satisfied by a test line.
	yml := "" +
		"        run: go test -tags newsuite ./test/newsuite/... -count=1\n" +
		"        run: go vet -tags fakeclock ./...\n"
	missingSuites, missingModifiers = missingTagCoverage(kinds,
		taggedInvocations(yml, goTestInvocationRe),
		taggedInvocations(yml, goVetInvocationRe))
	if len(missingSuites) != 0 || len(missingModifiers) != 0 {
		t.Fatalf("correctly covered tags reported missing: suites %v, modifiers %v", missingSuites, missingModifiers)
	}

	// And a modifier tag must NOT be satisfiable by a `go test -tags` line
	// alone: running zero tests under a tag is not coverage, it is the
	// silent-skip failure this gate exists to catch.
	_, missingModifiers = missingTagCoverage(kinds,
		taggedInvocations("        run: go test -tags fakeclock ./...\n", goTestInvocationRe),
		map[string]bool{})
	if len(missingModifiers) != 1 || missingModifiers[0] != "fakeclock" {
		t.Fatalf("a MODIFIER tag was satisfied by a `go test -tags` line that runs nothing: %v", missingModifiers)
	}
}

// TestTaggedInvocationsIgnoresProseAndRecognizesTheWrapper pins the parser's
// two sharp edges: coverage must come from a COMMAND, and the command may be
// hack/require_nonzero_go_tests.sh (which is `go test` plus a zero-test
// guard), which is how ci.yml's component/integration/images jobs actually
// invoke their suites.
func TestTaggedInvocationsIgnoresProseAndRecognizesTheWrapper(t *testing.T) {
	prose := "" +
		"      # Tracked as follow-up work; go test -tags component still covers it\n" +
		"      - name: go test -tags images (test/images)\n"
	if got := taggedInvocations(prose, goTestInvocationRe); len(got) != 0 {
		t.Fatalf("a comment and a step name were counted as coverage: %v", got)
	}

	wrapper := "        run: hack/require_nonzero_go_tests.sh -tags component ./test/component/... -race -count=1\n"
	if got := taggedInvocations(wrapper, goTestInvocationRe); !got["component"] {
		t.Fatalf("the require_nonzero_go_tests.sh wrapper was not recognized as a go test invocation: %v", got)
	}

	// A bare `go test ./...` names no tag and must contribute nothing.
	if got := taggedInvocations("        run: go test ./... -race -count=1\n", goTestInvocationRe); len(got) != 0 {
		t.Fatalf("an untagged go test line contributed tags: %v", got)
	}

	// The comma-separated and `-tags=` forms must both parse, or a future CI
	// line written either way silently fails to register as coverage.
	multi := taggedInvocations("        run: go test -tags=live,integration ./... -count=1\n", goTestInvocationRe)
	if !multi["live"] || !multi["integration"] {
		t.Fatalf("a comma-separated -tags= line did not parse: %v", multi)
	}
}

// TestBuildConstraintTagsIgnoresStandardTagsAndNegation pins the two shapes
// this repo's own go:build lines actually use --
// cmd/gonk-meter/clock.go's "!testclock" and clock_testclock.go's
// "testclock" -- to the SAME identifier, and confirms a bare GOOS/GOARCH tag
// (which is not one of our opt-in suites) is filtered out rather than
// demanded of CI.
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

// TestRepoBuildTagsAndKindsMatchTheKnownSix pins the exact set AND the
// classification against a real scan of this checkout, so a future change to
// any tagged file's build line is caught here even before the CI comparison
// above. In particular: if somebody adds a `testclock`-tagged _test.go file,
// testclock becomes a SUITE and this test says so -- loudly -- rather than
// letting the vet-only requirement quietly under-serve a real suite.
func TestRepoBuildTagsAndKindsMatchTheKnownSix(t *testing.T) {
	want := map[string]tagKind{
		"chart":       tagSuite,
		"component":   tagSuite,
		"images":      tagSuite,
		"integration": tagSuite,
		"live":        tagSuite,
		"testclock":   tagModifier,
	}
	got := classifyBuildTags(repoBuildTagDecls(t, repoRoot(t)))

	render := func(m map[string]tagKind) string {
		var parts []string
		for _, tag := range sortedTagNames(m) {
			parts = append(parts, tag+"="+m[tag].String())
		}
		return strings.Join(parts, " ")
	}
	if render(got) != render(want) {
		t.Fatalf("build tags = %q, want %q -- either a tag was added/removed, or a tag changed KIND "+
			"(a modifier grew a _test.go file, or a suite lost its last one). Either way this needs a "+
			"human to look, not a silent pass", render(got), render(want))
	}
}
