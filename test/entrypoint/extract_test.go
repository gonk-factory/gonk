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
	"syscall"
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

// ADR-008 section 12 carry-forward (gonk-t55, plan P4 item 1): the extract
// must not take ownership or permission bits from the archive. A GitLab
// repository-archive names uid 0 and the forge's modes; the agent is not root.
//
// Run as ROOT this is a real test: root's tar defaults to --same-owner and
// --same-permissions, so without the two flags the tree lands owned by the
// archive's uid with its exact modes, and this goes red. As a non-root user
// tar already implies both, so it passes whatever the flags say -- which is
// logged, so a green run there is not mistaken for evidence.
func TestExtractTakesNeitherOwnerNorPermissionsFromTheArchive(t *testing.T) {
	flags := tarFlags(t)
	for _, want := range []string{"--no-same-owner", "--no-same-permissions"} {
		var found bool
		for _, f := range flags {
			found = found || f == want
		}
		if !found {
			t.Errorf("entrypoint's tar extract does not pass %s (flags: %v)", want, flags)
		}
	}
	if os.Geteuid() != 0 {
		t.Logf("not root (euid %d): tar implies both flags for this user, so the extraction below "+
			"cannot tell whether they were passed; only the flag check above is evidence here", os.Geteuid())
	}

	root := t.TempDir()
	src := filepath.Join(root, "src", "proj-main-abc123")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "tool.sh"), []byte("#!/bin/sh\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "tool.sh"), 0o777); err != nil { // defeat the process umask
		t.Fatal(err)
	}
	tgz := filepath.Join(root, "rig.tar.gz")
	const archiveUID = 4242
	mk := exec.Command("tar", "-czf", tgz, "-C", filepath.Join(root, "src"),
		"--owner=4242", "--group=4242", "--numeric-owner", "proj-main-abc123")
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("building the fixture archive failed: %v\n%s", err, out)
	}
	dest := filepath.Join(root, "workspace")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// umask 022 in the extracting shell only: --no-same-permissions means
	// "apply the umask", so the mode that lands is 0777 &^ 022.
	script := `umask 022; exec tar "$@"`
	args := append([]string{"-c", script, "sh", "-xzf", tgz, "-C", dest}, flags...)
	if out, err := exec.Command("sh", args...).CombinedOutput(); err != nil {
		t.Fatalf("extract failed: %v\n%s", err, out)
	}
	info, err := os.Stat(filepath.Join(dest, "tool.sh"))
	if err != nil {
		t.Fatalf("tree did not land: %v", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no Stat_t on this platform")
	}
	if int(st.Uid) == archiveUID {
		t.Errorf("extracted file is owned by the ARCHIVE's uid %d, not the extracting user %d: "+
			"--no-same-owner is not in effect", archiveUID, os.Geteuid())
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("extracted mode %v, want 0755 (the archive's 0777 with the umask applied): "+
			"--no-same-permissions is not in effect", got)
	}
}
