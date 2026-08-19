// Package entrypoint tests the shell in images/agent/entrypoint.sh against a
// REAL tar, in the DEFAULT test run.
//
// WHY THIS PACKAGE EXISTS AT ALL (gonk-j9z, 2026-08-18). The rig checkout
// shipped with `--no-absolute-names` in its extract, and that flag DOES NOT
// EXIST -- GNU tar spells the opt-out `-P/--absolute-names` and strips leading
// "/" by default. tar exited 64 on every session. Because the fetch is
// non-fatal by design, the only symptom was a WARNING written into a tmux pane
// that opencode redrew a moment later, so five commits of rig work sat in
// production for a week with every agent silently running on an EMPTY working
// copy. The grant logged success on both sides the whole time.
//
// Nothing could have caught it. Unit and chart tests never execute the script,
// and test/images/ -- which does, in the real container -- is behind
// `//go:build images` while CI runs a bare `go test ./...`, so it has never run
// in CI. This file therefore carries NO build tag on purpose: a guard that only
// runs when someone remembers to ask for it is how this bug reached production.
package entrypoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// entrypointPath resolves images/agent/entrypoint.sh from this file's own
// location, so the test does not care about the caller's working directory.
func entrypointPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "images", "agent", "entrypoint.sh")
}

// tarFlags pulls the long flags off the entrypoint's real tar invocation, so
// the test tracks the script rather than a copy of it that can drift.
func tarFlags(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(entrypointPath(t))
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	// The invocation spans a line continuation; join then match.
	joined := strings.ReplaceAll(string(b), "\\\n", " ")
	re := regexp.MustCompile(`tar -xzf [^\n]*`)
	line := re.FindString(joined)
	if line == "" {
		t.Fatal("no `tar -xzf` invocation found in entrypoint.sh -- did the extract move?")
	}
	var flags []string
	for _, f := range regexp.MustCompile(`--[a-z-]+(=('[^']*'|[^\s]*))?`).FindAllString(line, -1) {
		flags = append(flags, strings.ReplaceAll(f, "'", ""))
	}
	if len(flags) == 0 {
		t.Fatalf("no long flags parsed from %q", line)
	}
	return flags
}

// THE REGRESSION. Every long flag the entrypoint hands tar must be one this
// tar actually accepts. `--no-absolute-names` passed review, passed every unit
// test, and failed 100% of the time in production.
func TestEveryTarFlagInTheEntrypointIsRealAndTheTreeLands(t *testing.T) {
	flags := tarFlags(t)

	// A GitLab archive: ONE top-level <project>-<sha>/ wrapper, which is the
	// whole reason --strip-components=1 is there.
	root := t.TempDir()
	src := filepath.Join(root, "src", "proj-main-abc123", ".agent")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("repo content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgz := filepath.Join(root, "rig.tar.gz")
	mk := exec.Command("tar", "-czf", tgz, "-C", filepath.Join(root, "src"), "proj-main-abc123")
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("building the fixture archive failed: %v\n%s", err, out)
	}

	dest := filepath.Join(root, "workspace")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-xzf", tgz, "-C", dest}, flags...)
	out, err := exec.Command("tar", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("the entrypoint's own tar flags were REJECTED by tar: %v\nflags: %v\n%s\n\n"+
			"This is the gonk-j9z failure: the extract fails, the fetch is non-fatal, "+
			"and the agent runs on an empty working copy with only a WARNING nobody reads.",
			err, flags, out)
	}

	// strip-components must actually land the tree at dest, not one level down.
	landed := filepath.Join(dest, ".agent", "README.md")
	if _, err := os.Stat(landed); err != nil {
		got, _ := os.ReadDir(dest)
		var names []string
		for _, e := range got {
			names = append(names, e.Name())
		}
		t.Fatalf("tree did not land at the checkout root: %v missing; dest contains %v\n"+
			"--strip-components is wrong, so the agent would see a <project>-<sha>/ wrapper "+
			"instead of the repository.", landed, names)
	}
}

// The guard the invalid flag was reaching for must still hold: a member with
// ".." in its path must not escape the destination.
func TestExtractRefusesToEscapeTheCheckoutDirectory(t *testing.T) {
	flags := tarFlags(t)
	root := t.TempDir()

	victim := filepath.Join(root, "ESCAPED.md")
	src := filepath.Join(root, "src", "proj-main-abc123")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "ok.md"), []byte("fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgz := filepath.Join(root, "rig.tar.gz")
	// -P is what lets us BUILD a hostile archive; the entrypoint must not need
	// it to extract one safely.
	mk := exec.Command("tar", "-czf", tgz, "-C", filepath.Join(root, "src"), "-P",
		"--transform", "s|ok.md|../../ESCAPED.md|", "proj-main-abc123")
	if out, err := mk.CombinedOutput(); err != nil {
		t.Skipf("this tar cannot build the hostile fixture (%v): %s", err, out)
	}

	dest := filepath.Join(root, "workspace")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-xzf", tgz, "-C", dest}, flags...)
	_ = exec.Command("tar", args...).Run() // may fail; what matters is the escape

	if _, err := os.Stat(victim); err == nil {
		t.Fatalf("a member escaped the checkout directory and wrote %s -- "+
			"the archive comes from the forge and is untrusted", victim)
	}
}
