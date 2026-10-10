package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard"
	"github.com/gonk-factory/gonk/internal/exportguard/gitexport"
	"github.com/gonk-factory/gonk/internal/exportguard/gitexport/exporttest"
)

func cleanScanner(string) ([]gitexport.ScanFinding, error) { return nil, nil }

// fakeScanner replaces the pinned scanner for one test.
func fakeScanner(t *testing.T, s gitexport.Scanner, err error) {
	t.Helper()
	old := resolveScanner
	resolveScanner = func(string) (gitexport.Scanner, error) { return s, err }
	t.Cleanup(func() { resolveScanner = old })
}

type fixture struct {
	origin   *exporttest.Origin
	mirror   string
	baseline string
	src      string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	o := exporttest.NewOrigin(t)
	baseline := o.Commit("public history", map[string]string{"README.md": "# gonk\n"})
	mirror := o.EmptyMirror()
	src := o.Commit("internal work", exporttest.BaseFiles())
	return &fixture{origin: o, mirror: mirror, baseline: baseline, src: src}
}

func (f *fixture) relexport(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	args = append([]string{args[0], "--origin", f.origin.Dir}, args[1:]...)
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func field(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, key+" "); ok {
			return v
		}
	}
	return ""
}

func TestUsageErrorsExitTwo(t *testing.T) {
	fakeScanner(t, cleanScanner, nil)
	f := newFixture(t)
	var out, errb bytes.Buffer
	for _, args := range [][]string{
		{},
		{"export"},
		{"tree"},
		{"tree", "--src", f.src, "extra"},
		{"tree", "--no-such-flag"},
		{"commit", "--origin", f.origin.Dir, "--src", f.src},
		{"commit", "--origin", f.origin.Dir, "--src", f.src, "--root"},
		// A snapshot is a declared root or has a parent: never neither, never both.
		{"commit", "--origin", f.origin.Dir, "--src", f.src, "--version", "v0.2.0"},
		{"commit", "--origin", f.origin.Dir, "--src", f.src, "--version", "v0.2.0", "--root", "--parent", f.baseline},
		{"record", "--origin", f.origin.Dir, "--src", f.src, "--root"},
		{"record", "--origin", f.origin.Dir, "--src", f.src, "--version", "v0.2.0"},
		{"record", "--origin", f.origin.Dir, "--src", f.src, "--version", "v0.2.0", "--root", "--parent", f.baseline},
		{"verify", "--origin", f.origin.Dir, "--src", f.src},
		{"publish", "--origin", f.origin.Dir, "--src", f.src, "--version", "v0.2.0", "--root", "--reviewed-tree", strings.Repeat("a", 40)},
		{"publish", "--origin", f.origin.Dir, "--src", f.src, "--version", "v0.2.0", "--root", "--mirror", f.mirror},
		{"publish", "--origin", f.origin.Dir, "--src", f.src, "--version", "v0.2.0", "--mirror", f.mirror, "--reviewed-tree", strings.Repeat("a", 40)},
		{"publish", "--origin", f.origin.Dir, "--src", f.src, "--version", "v0.2.0", "--mirror", f.mirror, "--reviewed-tree", strings.Repeat("a", 40), "--root", "--parent", f.baseline},
	} {
		if code := run(args, &out, &errb); code != exitUsage {
			t.Errorf("relexport %v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestTreePrintsTheTreeAndWritesTheReport(t *testing.T) {
	f := newFixture(t)
	code, out, errOut := f.relexport("tree", "--src", f.src)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	tree := field(out, "tree")
	if !exportguard.ValidCommitID(tree) {
		t.Fatalf("stdout: %q", out)
	}
	if !strings.Contains(errOut, "removed     3  release/") || !strings.Contains(errOut, "14 files; public tree: 8 files") {
		t.Fatalf("stderr: %s", errOut)
	}
	dir := t.TempDir()
	code, out, _ = f.relexport("tree", "--src", f.src, "--json", "--out", dir)
	if code != exitOK {
		t.Fatal(code)
	}
	var rep struct {
		Tree   string `json:"tree"`
		Source string `json:"source_commit"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Tree != tree || rep.Source != f.src {
		t.Fatalf("json: %v %+v", err, rep)
	}
	file, err := os.ReadFile(filepath.Join(dir, "export-report.json"))
	if err != nil || string(file) != out {
		t.Fatalf("report file differs from stdout: %v", err)
	}
	// With a version the tree differs.
	_, out, _ = f.relexport("tree", "--src", f.src, "--version", "v0.2.0")
	if v := field(out, "tree"); v == tree || v == "" {
		t.Fatalf("tree with a version: %q", v)
	}
	if code, _, errOut := f.relexport("tree", "--src", f.src[:10]); code != exitRefused || !strings.Contains(errOut, "full 40-hex") {
		t.Fatalf("short id: exit %d %s", code, errOut)
	}
}

func TestCheckExitStatusAndOutput(t *testing.T) {
	fakeScanner(t, cleanScanner, nil)
	f := newFixture(t)
	code, _, errOut := f.relexport("check", "--src", f.src)
	if code != exitOK || !strings.Contains(errOut, "check: PASS (guard and scanner)") {
		t.Fatalf("clean tree: exit %d\n%s", code, errOut)
	}

	leak := fmt.Sprintf("%d.%d.%d.%d", 10, 20, 30, 40)
	bad := f.origin.Commit("leak", map[string]string{"docs/guide.md": "one\nhost " + leak + "\n"})
	code, out, errOut := f.relexport("check", "--src", bad)
	if code != exitRefused {
		t.Fatalf("leaking tree: exit %d", code)
	}
	for _, want := range []string{"deny: G1 rfc1918-10 content docs/guide.md:2:6", "  G1       1 / 1", "  I1       0 / 0", "check: REFUSED"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(out+errOut, leak) {
		t.Fatal("the output repeats the matched text")
	}
	code, _, errOut = f.relexport("check", "--src", bad, "--quiet")
	if code != exitRefused || strings.Contains(errOut, "deny: ") || !strings.Contains(errOut, "  G1       1 / 1") {
		t.Fatalf("--quiet: exit %d\n%s", code, errOut)
	}

	// Guard-only: exits by the guard alone, and always says what it skipped.
	code, _, errOut = f.relexport("check", "--src", f.src, "--guard-only")
	if code != exitOK || !strings.Contains(errOut, "secret scanner (X1): NOT RUN") || !strings.Contains(errOut, "NOT cleared for export") {
		t.Fatalf("guard-only clean: exit %d\n%s", code, errOut)
	}
	code, _, _ = f.relexport("check", "--src", bad, "--guard-only")
	if code != exitRefused {
		t.Fatalf("guard-only leaking: exit %d", code)
	}

	// Scanner findings refuse; a missing scanner refuses.
	fakeScanner(t, func(string) ([]gitexport.ScanFinding, error) {
		return []gitexport.ScanFinding{{Rule: "generic-api-key", File: "README.md", Line: 1}}, nil
	}, nil)
	code, _, errOut = f.relexport("check", "--src", f.src)
	if code != exitRefused || !strings.Contains(errOut, "scanner: X1 generic-api-key README.md:1") || !strings.Contains(errOut, "secret scanner (X1): 1 findings") {
		t.Fatalf("scanner finding: exit %d\n%s", code, errOut)
	}
	fakeScanner(t, nil, errors.New("no gitleaks binary"))
	code, _, errOut = f.relexport("check", "--src", f.src)
	if code != exitRefused || !strings.Contains(errOut, "no gitleaks binary") {
		t.Fatalf("missing scanner: exit %d\n%s", code, errOut)
	}
}

func TestThePinnedScannerRefusesAnyOtherBinary(t *testing.T) {
	t.Setenv("RELEXPORT_GITLEAKS", "")
	fake := filepath.Join(t.TempDir(), "gitleaks")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedScanner(fake); err == nil || !strings.Contains(err.Error(), "not the pinned gitleaks") {
		t.Fatalf("a binary that always says clean was accepted as the scanner: %v", err)
	}
	t.Setenv("RELEXPORT_GITLEAKS", fake)
	if _, err := pinnedScanner(""); err == nil || !strings.Contains(err.Error(), "not the pinned gitleaks") {
		t.Fatalf("the environment variable bypassed the pin: %v", err)
	}
	t.Setenv("RELEXPORT_GITLEAKS", "")
	t.Setenv("PATH", t.TempDir())
	if _, err := pinnedScanner(""); err == nil || !strings.Contains(err.Error(), "no gitleaks binary") {
		t.Fatalf("no binary anywhere: %v", err)
	}
}

// The whole chain through the command, against a local EMPTY stand-in: the
// first release is a root snapshot, the next one a fast-forward child.
func TestCommitVerifyAndPublishEndToEnd(t *testing.T) {
	fakeScanner(t, cleanScanner, nil)
	f := newFixture(t)
	mirrorMain := func() string { return exporttest.RemoteRef(t, f.mirror, "refs/heads/main") }
	mirrorRefs := func() string {
		out, _ := exporttest.GitIn(f.mirror, 0, "for-each-ref", "--format=%(refname)")
		return out
	}

	out := t.TempDir()
	code, stdout, errOut := f.relexport("commit", "--src", f.src, "--root", "--version", "v0.2.0", "--out", out)
	if code != exitOK {
		t.Fatalf("commit: exit %d\n%s", code, errOut)
	}
	tree, commit, tag := field(stdout, "tree"), field(stdout, "commit"), field(stdout, "tag")
	if !exportguard.ValidCommitID(tree) || !exportguard.ValidCommitID(commit) || !exportguard.ValidCommitID(tag) || field(stdout, "origin-tag") == "" {
		t.Fatalf("commit stdout: %q", stdout)
	}
	if !strings.Contains(errOut, "NOTHING was pushed") || mirrorRefs() != "" {
		t.Fatalf("commit must push nothing; the mirror has %q\n%s", mirrorRefs(), errOut)
	}
	for _, name := range []string{"export-report.json", "gonk-v0.2.0-export.bundle"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("commit output: %v", err)
		}
	}
	// A second run in a second clone gives the same ids.
	_, again, _ := f.relexport("commit", "--src", f.src, "--root", "--version", "v0.2.0")
	if again != stdout {
		t.Fatalf("not deterministic:\n%s\n%s", stdout, again)
	}
	// The same source as a CHILD of some commit is another commit with the
	// same tree: --root is what makes a root, not a missing parent.
	_, child, _ := f.relexport("commit", "--src", f.src, "--parent", f.baseline, "--version", "v0.2.0")
	if field(child, "tree") != tree || field(child, "commit") == commit || field(child, "commit") == "" {
		t.Fatalf("child commit: %q", child)
	}

	root := []string{"publish", "--src", f.src, "--root", "--version", "v0.2.0", "--mirror", f.mirror}

	// A remote mirror is refused before any work without the variable.
	t.Setenv(gitexport.PublishEnv, "")
	code, _, errOut = f.relexport("publish", "--src", f.src, "--root", "--version", "v0.2.0",
		"--reviewed-tree", tree, "--mirror", "https://mirror.invalid/org/repo.git", "--confirm", commit)
	if code != exitRefused || !strings.Contains(errOut, "requires "+gitexport.PublishEnv+"=v0.2.0") {
		t.Fatalf("remote mirror without the variable: exit %d\n%s", code, errOut)
	}
	// The retired baseline: a first snapshot on a named parent, with no
	// previous source, is refused whatever the parent is.
	code, _, errOut = f.relexport("publish", "--src", f.src, "--parent", f.baseline, "--version", "v0.2.0",
		"--reviewed-tree", tree, "--mirror", f.mirror, "--confirm", commit)
	if code != exitRefused || !strings.Contains(errOut, "needs the previous release's source commit") || mirrorRefs() != "" {
		t.Fatalf("a parent without a previous source: exit %d, mirror %q\n%s", code, mirrorRefs(), errOut)
	}

	// The root: dry run (exit 3), wrong confirmation, unreviewed tree, then
	// confirmed.
	code, stdout, errOut = f.relexport(append(root, "--reviewed-tree", tree)...)
	if code != exitDryRun || field(stdout, "would-push") != commit || field(stdout, "to") != "refs/heads/main" || !strings.Contains(errOut, "NOT PUBLISHED (dry run)") {
		t.Fatalf("root dry run: exit %d\n%s\n%s", code, stdout, errOut)
	}
	code, _, _ = f.relexport(append(root, "--reviewed-tree", tree, "--confirm", tree)...)
	if code != exitDryRun || mirrorRefs() != "" {
		t.Fatalf("wrong confirmation: exit %d, mirror %q", code, mirrorRefs())
	}
	code, _, errOut = f.relexport(append(root, "--reviewed-tree", strings.Repeat("ab", 20), "--confirm", commit)...)
	if code != exitRefused || !strings.Contains(errOut, "is not the reviewed tree") || mirrorRefs() != "" {
		t.Fatalf("unreviewed tree: exit %d, mirror %q\n%s", code, mirrorRefs(), errOut)
	}
	code, stdout, errOut = f.relexport(append(root, "--reviewed-tree", tree, "--confirm", commit)...)
	if code != exitOK || field(stdout, "pushed") != commit || mirrorMain() != commit || mirrorRefs() != "refs/heads/main" {
		t.Fatalf("root publish: exit %d, mirror %q\n%s\n%s", code, mirrorRefs(), stdout, errOut)
	}
	if history, _ := exporttest.GitIn(f.mirror, 0, "rev-list", "--parents", "refs/heads/main"); history != commit {
		t.Fatalf("the mirror's history is not one parentless commit:\n%s", history)
	}
	// A second root is refused: a previous export exists there.
	code, _, errOut = f.relexport(append(root, "--reviewed-tree", tree, "--confirm", commit)...)
	if code != exitRefused || !strings.Contains(errOut, "a previous export exists there") || mirrorMain() != commit {
		t.Fatalf("second root: exit %d\n%s", code, errOut)
	}
	// The tag, after the commit.
	code, stdout, _ = f.relexport(append(root, "--reviewed-tree", tree, "--ref", "tag")...)
	if code != exitDryRun || field(stdout, "would-push") != tag || field(stdout, "to") != "refs/tags/v0.2.0" {
		t.Fatalf("root tag dry run: exit %d\n%s", code, stdout)
	}
	code, _, errOut = f.relexport(append(root, "--reviewed-tree", tree, "--ref", "tag", "--confirm", tag)...)
	if code != exitOK || exporttest.RemoteRef(t, f.mirror, "refs/tags/v0.2.0") != tag {
		t.Fatalf("root tag publish: exit %d\n%s", code, errOut)
	}

	code, stdout, errOut = f.relexport("verify", "--src", f.src, "--mirror", f.mirror, "--public", "refs/heads/main")
	if code != exitOK || field(stdout, "public-tree") != tree || field(stdout, "recomputed-tree") != tree || !strings.Contains(errOut, "verify: EQUAL") {
		t.Fatalf("verify: exit %d\n%s\n%s", code, stdout, errOut)
	}

	src2 := f.origin.Commit("work for v0.3.0", map[string]string{"docs/releases/v0.3.0.md": "# v0.3.0\n\nThe third release.\n"})
	code, _, errOut = f.relexport("verify", "--src", src2, "--mirror", f.mirror, "--public", commit)
	if code != exitRefused || !strings.Contains(errOut, "says it was exported from "+f.src) {
		t.Fatalf("verify against another source: exit %d\n%s", code, errOut)
	}

	_, stdout, _ = f.relexport("commit", "--src", src2, "--parent", commit, "--version", "v0.3.0", "--mirror", f.mirror)
	tree2, commit2, tag2 := field(stdout, "tree"), field(stdout, "commit"), field(stdout, "tag")
	if tree2 == "" || commit2 == "" {
		t.Fatalf("commit v0.3.0: %q", stdout)
	}
	next := []string{"publish", "--src", src2, "--parent", commit, "--version", "v0.3.0", "--prev-src", f.src, "--mirror", f.mirror}

	// A root for the next release is refused; so is a parent with --root.
	code, _, errOut = f.relexport("publish", "--src", src2, "--root", "--version", "v0.3.0", "--mirror", f.mirror, "--reviewed-tree", tree2)
	if code != exitRefused || !strings.Contains(errOut, "a previous export exists there") {
		t.Fatalf("root for the next release: exit %d\n%s", code, errOut)
	}
	// Dry run: exit 3, says what it would push, pushes nothing.
	code, stdout, errOut = f.relexport(append(next, "--reviewed-tree", tree2)...)
	if code != exitDryRun || field(stdout, "would-push") != commit2 || field(stdout, "to") != "refs/heads/main" || !strings.Contains(errOut, "NOT PUBLISHED (dry run)") {
		t.Fatalf("dry run: exit %d\n%s\n%s", code, stdout, errOut)
	}
	if mirrorMain() != commit {
		t.Fatalf("a dry run moved the mirror to %s", mirrorMain())
	}
	// A wrong confirmation is still a dry run.
	code, _, _ = f.relexport(append(next, "--reviewed-tree", tree2, "--confirm", tree2)...)
	if code != exitDryRun || mirrorMain() != commit {
		t.Fatalf("wrong confirmation: exit %d, mirror %s", code, mirrorMain())
	}
	// A tree other than the reviewed one is refused even when confirmed.
	code, _, errOut = f.relexport(append(next, "--reviewed-tree", tree, "--confirm", commit2)...)
	if code != exitRefused || !strings.Contains(errOut, "is not the reviewed tree") || mirrorMain() != commit {
		t.Fatalf("unreviewed tree: exit %d, mirror %s\n%s", code, mirrorMain(), errOut)
	}
	// Confirmed.
	code, stdout, errOut = f.relexport(append(next, "--reviewed-tree", tree2, "--confirm", commit2)...)
	if code != exitOK || field(stdout, "pushed") != commit2 || mirrorMain() != commit2 {
		t.Fatalf("publish: exit %d, mirror %s\n%s\n%s", code, mirrorMain(), stdout, errOut)
	}
	if history, _ := exporttest.GitIn(f.mirror, 0, "rev-list", "--parents", "refs/heads/main"); history != commit2+" "+commit+"\n"+commit {
		t.Fatalf("the mirror's history after v0.3.0:\n%s", history)
	}
	// The tag, after the commit.
	code, stdout, _ = f.relexport(append(next, "--reviewed-tree", tree2, "--ref", "tag")...)
	if code != exitDryRun || field(stdout, "would-push") != tag2 || field(stdout, "to") != "refs/tags/v0.3.0" {
		t.Fatalf("tag dry run: exit %d\n%s", code, stdout)
	}
	code, _, errOut = f.relexport(append(next, "--reviewed-tree", tree2, "--ref", "tag", "--confirm", tag2)...)
	if code != exitOK || exporttest.RemoteRef(t, f.mirror, "refs/tags/v0.3.0") != tag2 {
		t.Fatalf("tag publish: exit %d\n%s", code, errOut)
	}
	code, _, _ = f.relexport("verify", "--src", src2, "--mirror", f.mirror, "--public", "refs/tags/v0.3.0")
	if code != exitOK {
		t.Fatalf("verify the tag: exit %d", code)
	}
}

// write puts the public tree into a directory; the directory, hashed with
// plain git, has the id `tree` prints.
func TestWriteWritesThePublicTreeIntoADirectory(t *testing.T) {
	f := newFixture(t)
	_, stdout, _ := f.relexport("tree", "--src", f.src, "--version", "v0.2.0")
	want := field(stdout, "tree")

	dir := filepath.Join(t.TempDir(), "public")
	code, stdout, errOut := f.relexport("write", "--src", f.src, "--version", "v0.2.0", "--dir", dir)
	if code != exitOK || field(stdout, "tree") != want || field(stdout, "dir") != dir {
		t.Fatalf("write: exit %d\n%s\n%s", code, stdout, errOut)
	}
	if !strings.Contains(errOut, "NOT checked by the guard") {
		t.Fatalf("write must say it does not check:\n%s", errOut)
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "--all", "--force"}} {
		if out, err := exporttest.GitIn(dir, 0, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if id, err := exporttest.GitIn(dir, 0, "write-tree"); err != nil || id != want {
		t.Fatalf("the directory hashes to %s (%v), want %s", id, err, want)
	}
	// Nothing the fixture's internal-only list names was written.
	list, err := exportguard.ParseList([]byte(exporttest.List))
	if err != nil || len(list.Entries) < 4 {
		t.Fatalf("the fixture's list: %v %v", err, list)
	}
	for _, p := range list.Entries {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s is in the written tree", p)
		}
	}
	// Into the same, now non-empty, directory: refused. Without --dir: usage.
	code, _, errOut = f.relexport("write", "--src", f.src, "--dir", dir)
	if code != exitRefused || !strings.Contains(errOut, "is not empty") {
		t.Fatalf("write into a non-empty directory: exit %d\n%s", code, errOut)
	}
	if code, _, _ = f.relexport("write", "--src", f.src); code != exitUsage {
		t.Fatalf("write without --dir: exit %d", code)
	}
}

// record pushes refs/export/<version> to ORIGIN and nothing to the mirror.
func TestRecordThroughTheCommand(t *testing.T) {
	fakeScanner(t, cleanScanner, nil)
	f := newFixture(t)
	_, stdout, _ := f.relexport("commit", "--src", f.src, "--root", "--version", "v0.2.0")
	commit := field(stdout, "commit")
	exportRef := func() string { return exporttest.RemoteRef(t, f.origin.Dir, "refs/export/v0.2.0") }

	code, stdout, errOut := f.relexport("record", "--src", f.src, "--root", "--version", "v0.2.0")
	if code != exitDryRun || field(stdout, "would-push") != commit || field(stdout, "to") != "refs/export/v0.2.0" || field(stdout, "on") != f.origin.Dir {
		t.Fatalf("record dry run: exit %d\n%s\n%s", code, stdout, errOut)
	}
	if exportRef() != "" {
		t.Fatal("a dry run recorded the ref")
	}
	code, stdout, errOut = f.relexport("record", "--src", f.src, "--root", "--version", "v0.2.0", "--confirm", commit)
	if code != exitOK || field(stdout, "pushed") != commit || exportRef() != commit {
		t.Fatalf("record: exit %d, ref %q\n%s\n%s", code, exportRef(), stdout, errOut)
	}
	code, stdout, _ = f.relexport("record", "--src", f.src, "--root", "--version", "v0.2.0")
	if code != exitOK || field(stdout, "already-recorded") != commit {
		t.Fatalf("record again: exit %d\n%s", code, stdout)
	}
	if refs, _ := exporttest.GitIn(f.mirror, 0, "for-each-ref"); refs != "" {
		t.Fatalf("record wrote to the mirror:\n%s", refs)
	}
}

// This command is published; its source must pass the generic rules.
func TestOwnSourcePassesTheGuard(t *testing.T) {
	g, err := exportguard.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("*.go")
	if len(files) < 2 {
		t.Fatalf("found %d source files", len(files))
	}
	// The mutation script is published too, and it talks about every rule.
	files = append(files, "../mutate_relexport.py")
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range g.Content(f, data) {
			t.Errorf("own source refused: %s", fd)
		}
	}
}
