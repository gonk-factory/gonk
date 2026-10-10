package buildgate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE POD REAPER'S CHART TESTS MUST BE IN `make gate` (gonk-md3v).
//
// `make gate` runs the chart package with a -run FILTER, not whole. That is a
// deliberate trade -- the full suite costs 46s against 2.3s and hard-fatals
// without kubeconform -- but it has a sharp edge: a chart test that does not
// match the filter is not in the gate at all, and nothing says so. That is how
// the seven podreap tests came to exist while running only in the GitHub chart
// job, i.e. only after the commit was already on main. On the one feature in
// gonk that DELETES a Kubernetes object.
//
// So the filter is checked against the file. Add a test to podreap_test.go
// whose name the pattern does not cover and this goes red, here, in the gate,
// with the pattern to fix.

// goldensRunPattern is the -run '<...>' argument of the Makefile's `goldens`
// recipe, with make's `$$` unescaped back to the `$` the shell (and Go) sees.
func goldensRunPattern(t *testing.T) string {
	t.Helper()
	_, recipes := makeTargets(t)
	recipe, ok := recipes["goldens"]
	if !ok {
		t.Fatal("the Makefile has no `goldens:` target. It is the gate's only chart step; " +
			"if it was renamed, this test must follow it, and if it was deleted then no chart " +
			"test runs in `make gate` at all.")
	}
	m := regexp.MustCompile(`-run\s+'([^']*)'`).FindStringSubmatch(recipe)
	if m == nil {
		t.Fatalf("no -run '<pattern>' in the goldens recipe:\n%s\n"+
			"Either it now runs the whole package (in which case delete this test and say so) "+
			"or the quoting changed and this check silently stopped reading anything.", recipe)
	}
	return strings.ReplaceAll(m[1], "$$", "$")
}

// chartTestFuncNames returns every `func TestXxx(t *testing.T)` declared in one
// file of internal/charttest.
func chartTestFuncNames(t *testing.T, file string) []string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "internal", "charttest", file)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
			continue
		}
		names = append(names, fn.Name.Name)
	}
	if len(names) == 0 {
		t.Fatalf("found no Test functions in %s; the parse is broken and this check would "+
			"then pass against an empty gate", path)
	}
	return names
}

func TestGateRunsEveryPodReapChartTest(t *testing.T) {
	pattern := goldensRunPattern(t)
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("the goldens -run pattern %q does not compile as a regexp: %v.\n"+
			"`go test -run` would then match nothing and the gate's chart step would be "+
			"silently empty.", pattern, err)
	}

	names := chartTestFuncNames(t, "podreap_test.go")
	var missing []string
	for _, n := range names {
		if !re.MatchString(n) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("`make gate` does not run these tests from internal/charttest/podreap_test.go:\n"+
			"  %s\n"+
			"goldens -run pattern: %s\n"+
			"They guard the chart surface of the only feature in gonk that DELETES a Kubernetes "+
			"pod, so being reachable only from the GitHub chart job means being checked only "+
			"after the commit is on main (gonk-md3v). Widen the pattern in the Makefile's "+
			"`goldens:` recipe, or rename the test so the existing pattern covers it.",
			strings.Join(missing, "\n  "), pattern)
	}
}

// AND THE STEP MUST STILL BE IN THE GATE. The pattern above is worthless if
// nothing runs it.
func TestGoldensIsAGateStep(t *testing.T) {
	deps, _ := makeTargets(t)
	for _, d := range deps["gate"] {
		if d == "goldens" {
			return
		}
	}
	t.Fatalf("`goldens` is not a prerequisite of `gate` (gate: %s).\n"+
		"Without it `make gate` never renders the chart: template edits, stale goldens, "+
		"moved image floors and the pod reaper's RBAC are all unchecked until CI.",
		strings.Join(deps["gate"], " "))
}
