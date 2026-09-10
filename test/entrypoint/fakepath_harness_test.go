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
// fake external-tool scripts (opencode, curl) this package ships, and
// verifies every entry in it is executable.
//
// THAT CHECK IS LOAD-BEARING, NOT DECORATION. These scripts were once
// committed as mode 100644: this repo runs with core.fileMode=false on
// WSL/drvfs, so the local working tree reports every file rwxrwxrwx
// regardless of what git recorded, and the loss was invisible here. On a
// real Linux checkout (CI), a non-executable file is silently skipped by
// PATH lookup -- so `curl` or `opencode` in the fake dir would fall through
// to the REAL binary. A test that hit the network or a real opencode
// install would be a much worse failure than a red test right here.
func fakebinDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(testdataDir(t), "fakebin")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("fake PATH dir missing or unreadable: %s (%v)", dir, err)
	}
	if len(entries) == 0 {
		t.Fatalf("fake PATH dir is empty: %s", dir)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Join(dir, e.Name()), err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable (mode %s): PATH lookup will silently skip it and the "+
				"entrypoint will fall through to the REAL binary instead of this fake. Fix with: "+
				"git update-index --chmod=+x %s",
				filepath.Join(dir, e.Name()), info.Mode().Perm(), filepath.Join(dir, e.Name()))
		}
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
	requireRealJQ(t)
	return fakebinDir(t) + string(os.PathListSeparator) + os.Getenv("PATH")
}

// requireRealJQ enforces the half of fakePATH's contract that is easiest to
// lose: jq is NOT faked, it is expected to be the real thing on the runner.
//
// entrypoint.sh uses it for work a stand-in cannot honestly do -- parsing
// the fetched prompt document (`jq -r '.prompt // empty'`) and BUILDING the
// webhook JSON with correct escaping (`jq -n --arg ...`, which exists
// exactly because concatenation gets embedded quotes wrong). Without jq the
// script dies mid-run with `jq: not found` and exit 127, which surfaces as
// an unrelated-looking assertion failure about missing output. That is what
// the GitLab `test` job did (job 22185): golang:1.26 ships no jq, so this
// package was red there while green on GitHub, whose runners have it.
//
// Deliberately a hard t.Fatal, not a RequireInfra skip: a skip would hide
// exactly the divergence this check exists to make loud.
func requireRealJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("jq is not on PATH (%v). images/agent/entrypoint.sh parses the prompt document and "+
			"renders the webhook payload with it, so these tests are meaningless without the REAL jq -- "+
			"and a fake one that diverged from it would be worse. Install jq (Debian/Ubuntu: "+
			"apt-get install -y jq; Alpine: apk add jq).", err)
	}
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
