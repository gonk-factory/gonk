package entrypoint

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestEntrypointSurvivesUnsetAlias is the regression test for R-06/R-37.
//
// Under `set -eu`, entrypoint.sh used to expand ${GC_ALIAS} unguarded to
// compute the logged alias prefix -- the FIRST thing the script does after
// defining log(). An unset GC_ALIAS made that expansion an unbound-variable
// error, which `set -e` turns into an immediate abort, before "session
// start:" is ever printed. Gas City launches the pod inside a detached tmux
// pane (see entrypoint.sh's log() comment), so that abort went nowhere a
// reader could see: `kubectl logs` on the pod showed nothing at all, and the
// pod just looked dead.
//
// The fix computes the prefix with ${GC_ALIAS:-} semantics so the log always
// fires, and then refuses -- loudly, with a distinct exit code -- if the
// alias turns out to be empty, since a session with no alias has no identity
// to fetch a checkout or a prompt for.
func TestEntrypointSurvivesUnsetAlias(t *testing.T) {
	env := []string{"PATH=" + fakePATH(t)}
	out, err := runEntrypoint(t, env)

	startIdx := strings.Index(out, "session start:")
	if startIdx < 0 {
		t.Fatalf("no %q line in output; the entrypoint aborted before logging identity "+
			"(this is exactly the R-06/R-37 regression):\n%s", "session start:", out)
	}

	endIdx := strings.Index(out, "session end:")
	if endIdx < 0 {
		t.Fatalf("no %q line in output:\n%s", "session end:", out)
	}
	if endIdx < startIdx {
		t.Fatalf("\"session end:\" appeared before \"session start:\":\n%s", out)
	}

	// "naming the missing alias": the session-end line (and everything
	// through end of output) must say what refused it, in plain terms a
	// reader can grep for -- not just "exit 1" with no explanation.
	tail := out[endIdx:]
	if !strings.Contains(tail, "alias") {
		t.Errorf("\"session end:\" does not name the missing alias:\n%s", tail)
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected the script to exit non-zero with GC_ALIAS unset, got err=%v\noutput:\n%s", err, out)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("expected a non-zero exit code, got 0\noutput:\n%s", out)
	}
	t.Logf("exit code %d; output:\n%s", exitErr.ExitCode(), out)
}
