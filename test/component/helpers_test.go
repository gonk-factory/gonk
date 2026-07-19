//go:build component

package component_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// gonkYMLBudgeted renders a .gonk.yml with a finite monthly USD ceiling and a
// single-rung ladder. triage is enabled so an issue-triage decide is allowed.
func gonkYMLBudgeted(usd float64, rung string) string {
	return "version: 1\nenabled: true\n" +
		"actions: { triage: true }\n" +
		"ladder: [" + rung + "]\n" +
		"budget: { monthly_cost_usd: " + strconv.FormatFloat(usd, 'f', -1, 64) + " }\n"
}

// gonkYMLUnlimited renders a .gonk.yml with NO budget block -- unlimited (the
// gonkcfg sentinels). Used for the skew test's "unlimited project still runs".
func gonkYMLUnlimited(rung string) string {
	return "version: 1\nenabled: true\n" +
		"actions: { triage: true }\n" +
		"ladder: [" + rung + "]\n"
}

// decideReq builds an issue-triage DecideRequest for a project.
func decideReq(project, bead, session string) meterapi.DecideRequest {
	return meterapi.DecideRequest{
		Project:    project,
		Rig:        rigFor(project),
		BeadID:     bead,
		SessionKey: session,
		Trigger:    atags.TriggerIssueTriage,
	}
}

// parseGauge extracts a single unlabeled gauge value from Prometheus exposition
// text. Fails the test if the series is absent.
func parseGauge(t *testing.T, metrics, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		rest := strings.TrimPrefix(line, name)
		// Accept "name value" and "name{...} value"; reject "name_other value".
		if rest != "" && rest[0] != ' ' && rest[0] != '{' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		return v
	}
	t.Fatalf("metric %q not found in /metrics", name)
	return 0
}

var _ = fmt.Sprintf
