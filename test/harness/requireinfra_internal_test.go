package harness

import "testing"

// requireInfraAction is pure (no *testing.T), so the CI=true / t.Fatal
// branch can be pinned directly without a real subtest failure cascading up
// and failing the package's own `go test` result -- see the note in
// harness_test.go next to the other RequireInfra tests.
func TestRequireInfraActionMatchesTheCITaskContract(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
		ci   string
		want infraAction
	}{
		{"dependency present, CI unset", true, "", infraActionOK},
		{"dependency present, CI=true", true, "true", infraActionOK},
		{"dependency missing, CI unset -> skip", false, "", infraActionSkip},
		{"dependency missing, CI empty string explicitly -> skip", false, "", infraActionSkip},
		{"dependency missing, CI=false -> skip (only the literal \"true\" counts)", false, "false", infraActionSkip},
		{"dependency missing, CI=true -> fatal", false, "true", infraActionFatal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requireInfraAction(tc.ok, tc.ci); got != tc.want {
				t.Errorf("requireInfraAction(ok=%v, CI=%q) = %v, want %v", tc.ok, tc.ci, got, tc.want)
			}
		})
	}
}
