package buildgate

import (
	"regexp"
	"strings"
	"testing"
)

// THE RELEASE-TAG CHART TESTS MUST BE IN `make gate` (release-unit, 2026-09-26).
//
// internal/charttest/release_tag_test.go asserts that every gonk image renders
// under ONE release tag -- controller, intake, meter and the agent image the
// controller hands to session pods (GC_K8S_IMAGE) -- and that an empty resolved
// agent tag fails the render. `make gate` runs the chart package through a -run
// FILTER (see podreap_gate_test.go for why), so a test the filter does not match
// is not in the gate at all, and nothing would say so. This reads the filter out
// of the Makefile and requires it to match every test in that file.
func TestGateRunsEveryReleaseTagChartTest(t *testing.T) {
	pattern := goldensRunPattern(t)
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("the goldens -run pattern %q does not compile as a regexp: %v", pattern, err)
	}
	names := chartTestFuncNames(t, "release_tag_test.go")
	var missing []string
	for _, n := range names {
		if !re.MatchString(n) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("`make gate` does not run these tests from internal/charttest/release_tag_test.go:\n"+
			"  %s\n"+
			"goldens -run pattern: %s\n"+
			"They hold the controller and agent images to one release tag; outside the gate they "+
			"are checked only after the commit is on main. Widen the Makefile's `goldens:` "+
			"pattern (it carries ^TestReleaseTag), or rename the test to match it.",
			strings.Join(missing, "\n  "), pattern)
	}
}
