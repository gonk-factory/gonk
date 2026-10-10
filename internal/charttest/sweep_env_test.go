//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// The sweep's spend controls must actually reach the pod. They were added to the
// binary first and to the chart later; in between, an operator watching an
// unexpected drain could not slow it without a rebuild (gonk-vrf review).
func TestIssueSweepControlsReachTheIntakePod(t *testing.T) {
	t.Run("unset by default", func(t *testing.T) {
		o := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-intake")
		if strings.Contains(o.Doc, "GONK_ISSUE_SWEEP") {
			t.Error("sweep env vars are set when the values are empty; empty must mean 'use the default'")
		}
	})
	t.Run("set when configured", func(t *testing.T) {
		o := MustObject(t, Render(t, append(Minimum(),
			"--set", "intake.issueSweep.limit=2",
			"--set", "intake.issueSweep.maxAge=1s")...), "Deployment", "gonk-intake")
		// Read the env structurally. Object.Doc is re-marshalled YAML, so
		// asserting on quoting would test the serialiser, not the chart.
		got := workloadEnv(t, o)
		for k, want := range map[string]string{
			"GONK_ISSUE_SWEEP_LIMIT":   "2",
			"GONK_ISSUE_SWEEP_MAX_AGE": "1s",
		} {
			if got[k] != want {
				t.Errorf("intake env %s = %q, want %q; the operator control does not reach the pod", k, got[k], want)
			}
		}
	})
}
