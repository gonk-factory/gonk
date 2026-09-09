// Shared fake-PATH harness for running images/agent/entrypoint.sh as a real
// process against controlled stand-ins for the external tools it shells out
// to (opencode, curl -- see testdata/fakebin/). Lives in the `entrypoint`
// package alongside extract_test.go, since running the real script this way
// is that package's pattern (unlike lifecycle_log_test.go's entrypoint_test
// package, which only inspects the source as text).
//
// T-02 (GC_ALIAS-unset guard) is the first consumer; T-03 (provider check /
// prompt-fetch loop) is the next, which is why this stays generic -- no
// scenario-specific assumptions baked in here.
package entrypoint

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakebinDir resolves test/entrypoint/testdata/fakebin, the directory of
// fake external-tool scripts (opencode, curl) this package ships.
func fakebinDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(testdataDir(t), "fakebin")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("fake PATH dir missing or not a directory: %s (%v)", dir, err)
	}
	return dir
}

// testdataDir resolves test/entrypoint/testdata from this file's own
// location, independent of the caller's working directory (go test runs
// each package with its own directory as cwd, but resolving via
// entrypointPath's sibling keeps this consistent with extract_test.go).
func testdataDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(entrypointPath(t)), "..", "..", "test", "entrypoint", "testdata")
}

// fakePATH returns a PATH value with fakebin (the fake opencode and fake
// curl) prepended ahead of the real PATH, so the entrypoint's actual system
// tools (mkdir, jq, tar, sed, date, tmux, sh's own builtins, ...) still
// resolve normally while opencode and curl are the deterministic stand-ins.
func fakePATH(t *testing.T) string {
	t.Helper()
	return fakebinDir(t) + string(os.PathListSeparator) + os.Getenv("PATH")
}

// runEntrypoint runs entrypoint.sh under `sh -eu` (matching the script's own
// `set -eu`, so a test failure mode matches what production actually hits)
// with env as the CHILD PROCESS'S ENTIRE ENVIRONMENT -- nothing is inherited
// beyond what the caller passes in, so a test asserting on "GC_ALIAS unset"
// is not at the mercy of whatever happens to be in the test runner's own
// environment. args are passed through as the script's positional argv
// (e.g. --prompt <text>).
//
// log() writes to stderr unconditionally and to /proc/1/fd/1 best-effort
// (see entrypoint.sh's log()), so stdout and stderr are combined here: which
// channel a line lands on depends on whether the test process happens to be
// pid 1, and a harness that only watched one channel would be flaky across
// environments for a reason that has nothing to do with what is being
// tested.
func runEntrypoint(t *testing.T, env []string, args ...string) (output string, err error) {
	t.Helper()
	sh, lookErr := exec.LookPath("sh")
	if lookErr != nil {
		t.Fatalf("sh not found on the test runner's own PATH: %v", lookErr)
	}
	cmdArgs := append([]string{"-eu", entrypointPath(t)}, args...)
	cmd := exec.Command(sh, cmdArgs...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	return buf.String(), err
}
