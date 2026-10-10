package buildgate

import (
	"regexp"
	"strings"
	"testing"
)

// THE JOBS SESSION RUNTIME'S CHART TESTS MUST BE IN `make gate` (gonk-t55 P5).
//
// Same edge as TestGateRunsEveryPodReapChartTest: `make gate` runs the chart
// package through a -run filter, and the GitHub chart job that runs the rest is
// gated on an image that does not exist, so a chart test the filter misses runs
// NOWHERE. internal/charttest/jobs_rbac_test.go guards Role gonk-gate-jobs --
// Job create/delete and Secret create/get/delete for the controller -- and the
// env that names the agent image and the session namespace. Add a test there
// whose name the pattern does not cover and this goes red, in the gate.
func TestGateRunsEveryJobsRuntimeChartTest(t *testing.T) {
	pattern := goldensRunPattern(t)
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("the goldens -run pattern %q does not compile as a regexp: %v", pattern, err)
	}

	var missing []string
	for _, n := range chartTestFuncNames(t, "jobs_rbac_test.go") {
		if !re.MatchString(n) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("`make gate` does not run these tests from internal/charttest/jobs_rbac_test.go:\n"+
			"  %s\n"+
			"goldens -run pattern: %s\n"+
			"They guard the Jobs runtime's RBAC and configuration; outside the pattern they run "+
			"nowhere, because the GitHub chart job never runs. Widen the pattern in the "+
			"Makefile's `goldens:` recipe, or name the test TestJobsRuntime*.",
			strings.Join(missing, "\n  "), pattern)
	}
}
