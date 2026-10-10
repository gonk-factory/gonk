package gitexport

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard/gitexport/exporttest"
)

// A root snapshot is the first commit of a new public history: it has no
// parent, and its history is itself.
func TestRootSnapshotAssertions(t *testing.T) {
	w := newWorld(t)
	r := w.clone()
	tree, err := r.git("rev-parse", w.src+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(parents ...string) string {
		args := []string{"commit-tree", tree}
		for _, p := range parents {
			args = append(args, "-p", p)
		}
		id, err := r.gitWith(gitCmd{stdin: []byte("constructed\n"), env: identityEnv("1700000000 +0000")}, args...)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	root := mk()
	if err := r.assertRootSnapshot(root, tree); err != nil {
		t.Fatalf("a correct root snapshot was refused: %v", err)
	}
	t.Run("the commit must carry the checked tree", func(t *testing.T) {
		mustRefuse(t, r.assertRootSnapshot(root, strings.Repeat("0", 40)), "not the checked tree")
	})
	t.Run("a root has no parent", func(t *testing.T) {
		child := mk(w.baseline)
		mustRefuse(t, r.assertHasNoParent(child), "has a parent", w.baseline)
		mustRefuse(t, r.assertRootSnapshot(child, tree), "has a parent")
		if err := r.assertHasNoParent(root); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the history of a root is itself", func(t *testing.T) {
		// A child of the internal history: it would carry that history to the
		// mirror. The count sees it without being told about parents.
		child := mk(w.src)
		mustRefuse(t, r.assertHistoryIsOnlyItself(child), "would publish 3 commits")
		mustRefuse(t, r.assertHistoryIsOnlyItself(mk(root)), "would publish 2 commits")
		if err := r.assertHistoryIsOnlyItself(root); err != nil {
			t.Fatal(err)
		}
	})
}

// Ids of the ROOT snapshot of the base fixture as v0.2.0. The tree is the
// same tree a child snapshot carries (a tree does not depend on its parent).
const (
	pinnedChildCommit = "0ee57cd7e55842a64ed5eb59a9175df6ac735862"
	pinnedChildTag    = "b6bcb858c5124ffded301f5763a115dfabe39f78"
)

func TestARootAndAChildOfTheSameSourceDifferOnlyInTheCommit(t *testing.T) {
	w := newWorld(t)
	r := w.clone()
	root, err := r.Commit(w.src, w.commitOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !root.Root || root.Parent != "" {
		t.Fatalf("root result: root=%v parent=%q", root.Root, root.Parent)
	}
	child, err := r.Commit(w.src, CommitOptions{Version: "v0.2.0", Parent: w.baseline, Check: withScanner})
	if err != nil {
		t.Fatal(err)
	}
	if child.Root || child.Parent != w.baseline {
		t.Fatalf("child result: root=%v parent=%q", child.Root, child.Parent)
	}
	if root.Tree != child.Tree || root.Tree != pinnedTree {
		t.Fatalf("trees: root %s child %s pinned %s", root.Tree, child.Tree, pinnedTree)
	}
	if root.Commit == child.Commit || root.Tag == child.Tag {
		t.Fatal("a root and a child must be different commits and tags")
	}
	// The child path still produces what it produced before root mode
	// existed: the stacking rule for the next release did not move.
	if child.Commit != pinnedChildCommit || child.Tag != pinnedChildTag {
		t.Fatalf("child ids changed:\n commit %s\n tag    %s", child.Commit, child.Tag)
	}
	if parents, _ := r.git("rev-list", "--parents", "-n", "1", root.Commit); parents != root.Commit {
		t.Fatalf("the root snapshot has parents: %s", parents)
	}
	if parents, _ := r.git("rev-list", "--parents", "-n", "1", child.Commit); parents != child.Commit+" "+w.baseline {
		t.Fatalf("the child snapshot's parents: %s", parents)
	}
}

// listFiles returns the regular files under dir, slash-separated, sorted.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestWriteTreeWritesExactlyThePublicTree(t *testing.T) {
	w := newWorld(t)
	w.origin.Commit("an executable, an ignore rule and an export-ignore attribute", map[string]string{
		"hack/run.sh":    "#!/bin/sh\necho hi\n",
		".gitignore":     "ignored.txt\n",
		"ignored.txt":    "tracked although ignored\n",
		".gitattributes": "docs/guide.md export-ignore\n",
	})
	if err := os.Chmod(filepath.Join(w.origin.Dir, "hack", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := w.origin.Commit("mode", nil)
	if mode := w.origin.Git("ls-tree", src, "hack/run.sh"); !strings.HasPrefix(mode, "100755 ") {
		t.Fatalf("precondition: the fixture's script is not executable in the source: %s", mode)
	}

	r := w.clone()
	tr, err := r.Tree(src, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "public")
	if err := r.WriteTree(tr, dir); err != nil {
		t.Fatal(err)
	}

	got := listFiles(t, dir)
	want := paths(tr)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("files written:\n%s\nwant the tree's:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, p := range []string{"EXPORT-SOURCE", "docs/guide.md", "ignored.txt", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s was not written: %v", p, err)
		}
	}
	for _, p := range []string{"release", ".beads", "CLAUDE.md", "docs/plans", ".git"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s was written into the public tree", p)
		}
	}
	es, err := os.ReadFile(filepath.Join(dir, "EXPORT-SOURCE"))
	if err != nil || !strings.Contains(string(es), "source-commit: "+src+"\n") || !strings.Contains(string(es), "version: v0.2.0\n") {
		t.Fatalf("EXPORT-SOURCE: %v\n%s", err, es)
	}
	if st, err := os.Stat(filepath.Join(dir, "hack/run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the executable bit was lost: %v %v", err, st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(dir, "README.md")); st.Mode().Perm()&0o111 != 0 {
		t.Fatalf("README.md became executable: %v", st.Mode())
	}

	// Independent recomputation, with plain git and none of the tool: the
	// directory, committed as it is, has the tree id the tool printed.
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "--all", "--force"}} {
		if out, err := exporttest.GitIn(dir, 0, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	id, err := exporttest.GitIn(dir, 0, "write-tree")
	if err != nil || id != tr.Tree {
		t.Fatalf("the written directory hashes to %s (%v); the tree is %s", id, err, tr.Tree)
	}
}

func TestWriteTreeRefusals(t *testing.T) {
	w := newWorld(t)
	r := w.clone()
	tr, err := r.Tree(w.src, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("a directory that is not empty", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, r.WriteTree(tr, dir), "is not empty")
		if got := listFiles(t, dir); len(got) != 1 {
			t.Fatalf("a refused write left files: %v", got)
		}
	})
	t.Run("a path that is a file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, r.WriteTree(tr, f), "is not a directory")
	})
	t.Run("a relative path", func(t *testing.T) {
		mustRefuse(t, r.WriteTree(tr, "public-tree"), "absolute")
	})
	t.Run("an existing empty directory is accepted", func(t *testing.T) {
		dir := t.TempDir()
		if err := r.WriteTree(tr, dir); err != nil {
			t.Fatal(err)
		}
		if len(listFiles(t, dir)) != tr.FilesInTree {
			t.Fatal("not every file was written")
		}
	})
	// The check that the directory IS the tree, with a constructed violation:
	// a tree other than the one written.
	t.Run("a directory that does not hash to the tree", func(t *testing.T) {
		dir := t.TempDir()
		id, err := r.git("rev-parse", w.baseline+"^{tree}")
		if err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, r.verifyWritten(dir, id), "hashes to", "not the public tree")
	})
}

// Record pushes the snapshot commit to ORIGIN as refs/export/<version>: the
// reviewed object becomes addressable on the internal side. It is fenced like
// publish and writes nothing to any mirror.
func TestRecordPushesTheExportRefToOriginOnly(t *testing.T) {
	w := newWorld(t)
	c, err := w.clone().Commit(w.src, w.commitOpts())
	if err != nil {
		t.Fatal(err)
	}
	opts := func() RecordOptions {
		return RecordOptions{Version: "v0.2.0", Root: true, Check: withScanner}
	}
	originRefs := func() string {
		return w.origin.Git("for-each-ref", "--format=%(refname) %(objectname)", "refs/export", "refs/tags", "refs/public-tags")
	}

	t.Run("without the confirmation nothing is pushed", func(t *testing.T) {
		for _, confirm := range []string{"", "yes", c.Tree, c.Tag, c.Commit[:39]} {
			opt := opts()
			opt.Confirm = confirm
			res, err := w.clone().Record(w.src, opt)
			var nc *NotConfirmed
			if !errors.As(err, &nc) || res.Pushed {
				t.Fatalf("confirm %q: want NotConfirmed, got %v", confirm, err)
			}
			if nc.Object != c.Commit || nc.Target != "refs/export/v0.2.0" || nc.Mirror != w.origin.Dir {
				t.Fatalf("dry run: %+v", nc)
			}
		}
		if got := originRefs(); got != "" {
			t.Fatalf("a dry run wrote refs to origin:\n%s", got)
		}
	})
	t.Run("with the exact commit id the ref is pushed, and only it", func(t *testing.T) {
		opt := opts()
		opt.Confirm = c.Commit
		res, err := w.clone().Record(w.src, opt)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Pushed || res.Already || res.Object != c.Commit || res.Ref != "refs/export/v0.2.0" {
			t.Fatalf("%+v", res)
		}
		if got := originRefs(); got != "refs/export/v0.2.0 "+c.Commit {
			t.Fatalf("origin refs:\n%s", got)
		}
		if got := w.origin.Git("rev-parse", "refs/export/v0.2.0^{tree}"); got != c.Tree {
			t.Fatalf("the export ref carries tree %s", got)
		}
		if got := w.origin.Git("rev-parse", "refs/heads/main"); got != w.src {
			t.Fatalf("origin main moved to %s", got)
		}
		// Nothing reached the mirror.
		refs, _ := exporttest.GitIn(w.mirror, 0, "for-each-ref")
		if refs != "" {
			t.Fatalf("record wrote to the mirror:\n%s", refs)
		}
	})
	t.Run("recording the same commit again is a no-op that says so", func(t *testing.T) {
		opt := opts() // no confirmation needed: nothing would be pushed
		res, err := w.clone().Record(w.src, opt)
		if err != nil || res.Pushed || !res.Already {
			t.Fatalf("%v %+v", err, res)
		}
	})
	t.Run("an export ref is never moved", func(t *testing.T) {
		src2 := w.origin.Commit("one byte later", map[string]string{"README.md": "# gonk.\n"})
		c2, err := w.clone().Commit(src2, w.commitOpts())
		if err != nil {
			t.Fatal(err)
		}
		opt := opts()
		opt.Confirm = c2.Commit
		_, err = w.clone().Record(src2, opt)
		mustRefuse(t, err, "origin already has refs/export/v0.2.0", c.Commit, "never moved")
		if got := originRefs(); got != "refs/export/v0.2.0 "+c.Commit {
			t.Fatalf("origin refs:\n%s", got)
		}
	})
	t.Run("a tree that fails the check is not recorded", func(t *testing.T) {
		src := w.origin.Commit("leak", map[string]string{
			"docs/guide.md":           cat("gitlab.", "zork", ".lan\n"),
			"docs/releases/v0.3.0.md": "# v0.3.0\n\nThird.\n",
		})
		opt := RecordOptions{Version: "v0.3.0", Root: true, Check: withScanner, Confirm: strings.Repeat("ab", 20)}
		_, err := w.clone().Record(src, opt)
		mustRefuse(t, err, "does not pass the check", "deny: I1")
		if got := originRefs(); got != "refs/export/v0.2.0 "+c.Commit {
			t.Fatalf("origin refs:\n%s", got)
		}
	})
	t.Run("a guard-only check is refused", func(t *testing.T) {
		opt := opts()
		opt.Check = CheckOptions{SkipScanner: true}
		_, err := w.clone().Record(w.src, opt)
		mustRefuse(t, err, "guard-only")
	})
}

// The export ref goes to the INTERNAL remote. A clone whose origin is the
// public host is refused before any work: publishing is publish's job.
func TestRecordRefusesAPublicOrigin(t *testing.T) {
	for _, origin := range []string{
		// Assembled, so that this file holds nothing shaped like an address.
		"https://github.com/example/repo.git", cat("git", "@", "github.com:example/repo.git"),
		cat("ssh://git", "@", "GitHub.com/example/repo"), cat("https://user", "@", "github.com/example/repo"),
		"https://www.github.com/example/repo",
	} {
		if !isPublicHost(origin) {
			t.Errorf("%s is not recognised as the public host", origin)
		}
		r := &Repo{origin: origin}
		res, err := r.Record(strings.Repeat("ab", 20), RecordOptions{Version: "v0.2.0", Root: true, Check: withScanner})
		mustRefuse(t, err, "record pushes to the internal origin only")
		if res != nil {
			t.Fatalf("the fence let work begin for %s", origin)
		}
	}
	for _, origin := range []string{
		"https://gitlab.example.internal/group/repo.git", "/srv/git/repo.git",
		"https://notgithub.com.example.internal/x", "https://example.internal/github.com-mirror/repo",
		"https://notgithub.com/x", cat("git", "@", "notgithub.com:x/y.git"), "./github.com/x", "/srv/github.com:x",
		"https://github.com.example.internal/x",
	} {
		if isPublicHost(origin) {
			t.Errorf("%s is taken for the public host", origin)
		}
	}
}

// A later release is recorded with its parent, fetched from the mirror.
func TestRecordAChildSnapshot(t *testing.T) {
	w := newWorld(t)
	c, err := w.clone().Commit(w.src, w.commitOpts())
	if err != nil {
		t.Fatal(err)
	}
	opt := w.publishOpts(c.Tree)
	opt.Confirm = c.Commit
	if _, err := w.clone().Publish(w.src, opt); err != nil {
		t.Fatal(err)
	}
	src2 := w.origin.Commit("work for v0.3.0", map[string]string{"docs/releases/v0.3.0.md": "# v0.3.0\n\nThe third release.\n"})
	c2, err := w.clone().Commit(src2, CommitOptions{Version: "v0.3.0", Parent: c.Commit, Mirror: w.mirror, Check: withScanner})
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.clone().Record(src2, RecordOptions{Version: "v0.3.0", Parent: c.Commit, Mirror: w.mirror, Confirm: c2.Commit, Check: withScanner})
	if err != nil || !res.Pushed {
		t.Fatalf("%v %+v", err, res)
	}
	if got := w.origin.Git("rev-list", "--parents", "refs/export/v0.3.0"); got != c2.Commit+" "+c.Commit+"\n"+c.Commit {
		t.Fatalf("the recorded history:\n%s", got)
	}
	if got := w.mirrorMain(); got != c.Commit {
		t.Fatalf("record moved the mirror to %s", got)
	}
}
