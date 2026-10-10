package gitexport

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard"
	"github.com/gonk-factory/gonk/internal/exportguard/gitexport/exporttest"
)

func (w *world) commitOpts() CommitOptions {
	return CommitOptions{Version: "v0.2.0", Root: true, Check: withScanner}
}

func (w *world) publishOpts(tree string) PublishOptions {
	return PublishOptions{
		Version: "v0.2.0", Root: true, Mirror: w.mirror, ReviewedTree: tree,
		Ref: "main", Check: withScanner,
	}
}

func TestCommitBuildsTheSnapshotFromTheFixedTemplate(t *testing.T) {
	w := newWorld(t)
	r := w.clone()
	out := t.TempDir()
	opt := w.commitOpts()
	opt.OutDir = out
	res, err := r.Commit(w.src, opt)
	if err != nil {
		t.Fatal(err)
	}

	wantMsg := "gonk v0.2.0\n\n" +
		"The second release: change requests and reviews,\nwrapped over two lines.\n\n" +
		"This is a release snapshot. It is the exported tree of the source commit\n" +
		"named below; see CONTRIBUTING.md for how changes reach this repository.\n\n" +
		"Gonk-Source-Commit: " + w.src + "\n" +
		"Gonk-Export-Tree: " + res.Tree + "\n"
	if res.Message != wantMsg {
		t.Fatalf("message:\n%s\nwant:\n%s", res.Message, wantMsg)
	}

	raw, err := r.git("cat-file", "commit", res.Commit)
	if err != nil {
		t.Fatal(err)
	}
	// The source commit's committer date is 1700000000 + 2 minutes, written
	// by the fixture at +0200; the snapshot carries the same instant in UTC.
	who := IdentityName + " <" + IdentityEmail + "> 1700000120 +0000"
	wantRaw := "tree " + res.Tree + "\nauthor " + who + "\ncommitter " + who + "\n\n" + strings.TrimRight(wantMsg, "\n")
	if raw != wantRaw {
		t.Fatalf("commit object:\n%s\nwant:\n%s", raw, wantRaw)
	}

	tag, err := r.git("cat-file", "tag", res.Tag)
	if err != nil {
		t.Fatal(err)
	}
	wantTag := "object " + res.Commit + "\ntype commit\ntag v0.2.0\ntagger " + who + "\n\n" +
		"gonk v0.2.0\n\nGonk-Source-Commit: " + w.src + "\nGonk-Export-Tree: " + res.Tree
	if tag != wantTag {
		t.Fatalf("tag object:\n%s\nwant:\n%s", tag, wantTag)
	}
	if strings.Contains(tag, "PGP") || strings.Contains(raw, "gpgsig") {
		t.Fatal("the snapshot must be unsigned")
	}

	otag, err := r.git("cat-file", "tag", res.OriginTag)
	if err != nil {
		t.Fatal(err)
	}
	wantOTag := "object " + w.src + "\ntype commit\ntag v0.2.0\ntagger " + who + "\n\n" +
		"gonk v0.2.0\n\nGonk-Export-Tree: " + res.Tree + "\nGonk-Public-Commit: " + res.Commit
	if otag != wantOTag {
		t.Fatalf("internal tag object:\n%s\nwant:\n%s", otag, wantOTag)
	}

	// Nothing left the clone.
	if got := w.mirrorMain(); got != "" {
		t.Fatalf("commit moved the mirror to %s", got)
	}
	if got := w.origin.Git("for-each-ref", "refs/export", "refs/tags"); got != "" {
		t.Fatalf("commit wrote refs to origin: %s", got)
	}

	// The bundle carries the commit and the tag; a root snapshot's bundle has
	// no prerequisite, so it applies on the empty mirror.
	if res.Bundle != filepath.Join(out, "gonk-v0.2.0-export.bundle") {
		t.Fatalf("bundle path %q", res.Bundle)
	}
	heads, err := exporttest.GitIn(w.mirror, 0, "bundle", "list-heads", res.Bundle)
	if err != nil {
		t.Fatalf("bundle list-heads: %v\n%s", err, heads)
	}
	if !strings.Contains(heads, res.Commit+" refs/export/v0.2.0") || !strings.Contains(heads, res.Tag+" refs/public-tags/v0.2.0") {
		t.Fatalf("bundle heads:\n%s", heads)
	}
	if out, err := exporttest.GitIn(w.mirror, 0, "bundle", "verify", res.Bundle); err != nil {
		t.Fatalf("the bundle does not apply on the mirror: %v\n%s", err, out)
	}
}

func TestCommitCreditsContributorsFromTheNotes(t *testing.T) {
	w := newWorld(t)
	a := "Co-authored-by: Ada Example <ada@example.com>"
	b := "Co-authored-by: Bo Example <bo@users.noreply.example.org>"
	src := w.origin.Commit("notes with credits", map[string]string{
		"docs/releases/v0.2.0.md": exporttest.Notes + "\n" + a + "\n" + b + "\n" + a + "\nCo-authored-by: no address\n  " + a + "\n",
	})
	res, err := w.clone().Commit(src, w.commitOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Message, "Gonk-Export-Tree: "+res.Tree+"\n"+a+"\n"+b+"\n") {
		t.Fatalf("message:\n%s", res.Message)
	}
}

func TestFirstParagraph(t *testing.T) {
	cases := map[string]string{
		"# Title\n\nOne.\nTwo.\n\nThree.\n":     "One.\nTwo.",
		"\n\n# Title\n## Sub\n\nOnly.  \n":      "Only.",
		"No heading.\n\nSecond.\n":              "No heading.",
		"# T\r\n\r\nWindows line.\r\n\r\nx\r\n": "Windows line.",
		"# T\n\nText with\n# a hash line\n\nx":  "Text with\n# a hash line",
	}
	for in, want := range cases {
		got, err := FirstParagraph([]byte(in))
		if err != nil || got != want {
			t.Errorf("FirstParagraph(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "# only a heading\n", "\n\n", "# a\n## b\n"} {
		if got, err := FirstParagraph([]byte(in)); err == nil {
			t.Errorf("FirstParagraph(%q) = %q; want an error", in, got)
		}
	}
}

func TestCommitRefusals(t *testing.T) {
	t.Run("a tree that fails the check", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("leak", map[string]string{"docs/guide.md": "host " + ip4(10, 1, 1, 1) + "\n"})
		res, err := w.clone().Commit(src, w.commitOpts())
		mustRefuse(t, err, "does not pass the check", "deny: G1", "docs/guide.md:1:")
		if res == nil || res.Commit != "" || res.Tag != "" {
			t.Fatalf("a refused commit produced objects: %+v", res)
		}
	})
	t.Run("scanner findings", func(t *testing.T) {
		w := newWorld(t)
		opt := w.commitOpts()
		opt.Check = CheckOptions{Scanner: func(string) ([]ScanFinding, error) {
			return []ScanFinding{{Rule: "gitlab-pat", File: "README.md", Line: 1}}, nil
		}}
		res, err := w.clone().Commit(w.src, opt)
		mustRefuse(t, err, "scanner: X1 gitlab-pat README.md:1")
		if res.Commit != "" {
			t.Fatal("a commit was built")
		}
	})
	t.Run("a guard-only check", func(t *testing.T) {
		w := newWorld(t)
		opt := w.commitOpts()
		opt.Check = CheckOptions{SkipScanner: true}
		_, err := w.clone().Commit(w.src, opt)
		mustRefuse(t, err, "never built from a guard-only check")
	})
	t.Run("no release notes", func(t *testing.T) {
		w := newWorld(t)
		opt := w.commitOpts()
		opt.Version = "v0.3.0"
		_, err := w.clone().Commit(w.src, opt)
		mustRefuse(t, err, "docs/releases/v0.3.0.md does not exist")
	})
	t.Run("notes without a paragraph", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("empty notes", map[string]string{"docs/releases/v0.2.0.md": "# gonk v0.2.0\n"})
		_, err := w.clone().Commit(src, w.commitOpts())
		mustRefuse(t, err, "no paragraph")
	})
	t.Run("a bad version, parent", func(t *testing.T) {
		w := newWorld(t)
		r := w.clone()
		opt := w.commitOpts()
		opt.Version = "v0.2"
		_, err := r.Commit(w.src, opt)
		mustRefuse(t, err, "vX.Y.Z")
		opt = w.commitOpts()
		opt.Root, opt.Parent = false, w.baseline[:10]
		_, err = r.Commit(w.src, opt)
		mustRefuse(t, err, "full 40-hex")
		opt = w.commitOpts()
		opt.Root, opt.Parent = false, strings.Repeat("cd", 20)
		_, err = r.Commit(w.src, opt)
		mustRefuse(t, err, "is not a commit available here")
		// A root is DECLARED: no parent and no declaration is refused, and so
		// is a declaration with a parent.
		opt = w.commitOpts()
		opt.Root = false
		_, err = r.Commit(w.src, opt)
		mustRefuse(t, err, "full 40-hex", "declare a root export")
		opt = w.commitOpts()
		opt.Parent = w.baseline
		_, err = r.Commit(w.src, opt)
		mustRefuse(t, err, "a root snapshot has no parent")
	})
	// The notes can be internal-only (not in the tree at all); the commit
	// message is still derived from them, and it is published. The message
	// scan is then the only thing between the notes and the mirror.
	t.Run("a message that fails the guard although the tree passes", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("internal notes", map[string]string{
			"docs/releases/v0.2.0.md":   "# gonk v0.2.0\n\nDeployed first on " + cat("zork", "02") + ".\n",
			"release/internal-only.txt": exporttest.List + "docs/releases/v0.2.0.md # internal for this test\n",
		})
		res, err := w.clone().Commit(src, w.commitOpts())
		mustRefuse(t, err, "release messages do not pass the guard", "deny: I2 cluster message commit-message:3:19")
		if res.Commit != "" || res.Tag != "" {
			t.Fatal("a commit was built from a refused message")
		}
	})
}

// hostile is a git configuration that changes object ids if any plumbing
// command reads it: an identity, an encoding header, a signing request.
const hostile = `[user]
	name = Somebody Else
	email = else@example.com
	signingkey = ABCDEF
[i18n]
	commitEncoding = ISO-8859-1
[commit]
	gpgsign = true
[tag]
	gpgsign = true
[core]
	autocrlf = true
`

// Pinned ids of the base fixture's export. They are literals on purpose:
// "the same source gives the same ids" must hold across runs, machines and
// git versions, not only between two calls in one process. A change to the
// fixture, the EXPORT-SOURCE format, the message template or the identity
// changes them, and must. They are the ids of a ROOT snapshot (no parent):
// the first commit of a new public history. The ids of a child snapshot of
// the same source are pinned in root_test.go.
const (
	pinnedTree      = "395b7f87397b59bf0877a98f2abcf87745fdcf85"
	pinnedCommit    = "8b04c742a8df29975f9d05cbf13e760eafecc32a"
	pinnedTag       = "742f77f3a91cc69f6f2914322806fd55b2f6f3a8"
	pinnedOriginTag = "32ee910d88b928e3a752ebda7cf2d54f3f254639"
)

func TestTheExportIsDeterministic(t *testing.T) {
	// World A: built and exported in a plain environment.
	a := newWorld(t)
	ra, err := a.clone().Commit(a.src, a.commitOpts())
	if err != nil {
		t.Fatal(err)
	}

	// World B: a second, separately built origin in another directory, and
	// an environment made as different as a process can make it -- another
	// HOME with a hostile git configuration, another time zone and locale,
	// another identity and clock in the environment, another TMPDIR (so the
	// clone lives at another path).
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("TZ", "Pacific/Kiritimati")
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("USER", "someone-else")
	t.Setenv("LOGNAME", "someone-else")
	t.Setenv("GIT_AUTHOR_NAME", "Env Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "env@example.com")
	t.Setenv("GIT_AUTHOR_DATE", "2001-01-01T00:00:00+0900")
	t.Setenv("GIT_COMMITTER_NAME", "Env Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "env@example.com")
	t.Setenv("GIT_COMMITTER_DATE", "2001-01-01T00:00:00+0900")
	// What a git hook exports: with these, an unguarded git command acts on
	// some other repository and index.
	t.Setenv("GIT_DIR", filepath.Join(home, "no-such-repo.git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(home, "no-such-index"))
	t.Setenv("GIT_WORK_TREE", home)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	b := newWorld(t)
	if b.src != a.src || b.baseline != a.baseline {
		t.Fatalf("the two fixtures differ (%s vs %s); the comparison below would mean nothing", a.src, b.src)
	}
	clone := b.clone()
	if !strings.HasPrefix(clone.dir, tmp) {
		t.Fatalf("the second clone is not under the second TMPDIR: %s", clone.dir)
	}
	rb, err := clone.Commit(b.src, b.commitOpts())
	if err != nil {
		t.Fatal(err)
	}

	if ra.Tree != rb.Tree || ra.Commit != rb.Commit || ra.Tag != rb.Tag || ra.OriginTag != rb.OriginTag {
		t.Fatalf("not deterministic:\n A tree %s commit %s tag %s otag %s\n B tree %s commit %s tag %s otag %s",
			ra.Tree, ra.Commit, ra.Tag, ra.OriginTag, rb.Tree, rb.Commit, rb.Tag, rb.OriginTag)
	}
	// Across runs: the ids are the pinned literals.
	if ra.Tree != pinnedTree || ra.Commit != pinnedCommit || ra.Tag != pinnedTag || ra.OriginTag != pinnedOriginTag {
		t.Fatalf("ids differ from the pinned ones:\n tree      %s\n commit    %s\n tag       %s\n originTag %s", ra.Tree, ra.Commit, ra.Tag, ra.OriginTag)
	}

	// A third run in the same clone gives the same ids again.
	rc, err := clone.Commit(b.src, b.commitOpts())
	if err != nil {
		t.Fatal(err)
	}
	if rc.Tree != ra.Tree || rc.Commit != ra.Commit || rc.Tag != ra.Tag {
		t.Fatal("a repeated run in one clone gave different ids")
	}

	// And the inputs DO matter. One byte of the source changes the tree.
	src2 := b.origin.Commit("one byte", map[string]string{"README.md": "# gonk!\n"})
	r2, err := b.clone().Commit(src2, b.commitOpts())
	if err != nil {
		t.Fatal(err)
	}
	if r2.Tree == ra.Tree || r2.Commit == ra.Commit || r2.Tag == ra.Tag {
		t.Fatal("a changed source did not change the ids")
	}
	// A change to an INTERNAL-ONLY file changes the source commit, so
	// EXPORT-SOURCE and the tree change too -- the public files do not.
	src3 := b.origin.Commit("internal only", map[string]string{"README.md": "# gonk\n", "CLAUDE.md": "other instructions\n"})
	r3, err := b.clone().Commit(src3, b.commitOpts())
	if err != nil {
		t.Fatal(err)
	}
	if r3.Tree == ra.Tree {
		t.Fatal("EXPORT-SOURCE names the source commit; another source must give another tree")
	}
	same := 0
	for _, e := range r3.entries {
		for _, f := range ra.entries {
			if e == f {
				same++
			}
		}
	}
	if same != len(ra.entries)-1 {
		t.Fatalf("an internal-only change altered %d public files besides EXPORT-SOURCE", len(ra.entries)-1-same)
	}
}

func TestCommitIdDependsOnParentAndVersion(t *testing.T) {
	w := newWorld(t)
	src := w.origin.Commit("notes for the next one", map[string]string{"docs/releases/v0.3.0.md": "# v0.3.0\n\nThird.\n"})
	r := w.clone()
	base, err := r.Commit(src, w.commitOpts())
	if err != nil {
		t.Fatal(err)
	}
	opt := w.commitOpts()
	opt.Version = "v0.3.0"
	other, err := r.Commit(src, opt)
	if err != nil {
		t.Fatal(err)
	}
	if other.Tree == base.Tree || other.Commit == base.Commit || other.Tag == base.Tag {
		t.Fatal("another version must give another tree, commit and tag")
	}
	// Another parent: same tree, another commit.
	opt = w.commitOpts()
	opt.Root, opt.Parent = false, w.src
	onOther, err := r.Commit(src, opt)
	if err != nil {
		t.Fatal(err)
	}
	if onOther.Tree != base.Tree || onOther.Commit == base.Commit {
		t.Fatalf("another parent must keep the tree and change the commit: %s/%s vs %s/%s", onOther.Tree, onOther.Commit, base.Tree, base.Commit)
	}
}

// Each assertion of the snapshot shape, with a constructed violation that
// the other assertions are not asked about.
func TestSnapshotAssertions(t *testing.T) {
	w := newWorld(t)
	p0 := w.baseline
	p1 := w.src
	p2 := w.origin.Commit("third", map[string]string{"x.txt": "x\n"})
	r := w.clone()
	tree, err := r.git("rev-parse", p2+"^{tree}")
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
	good := mk(p1)
	if err := r.assertSnapshot(good, p1, tree); err != nil {
		t.Fatalf("a correct snapshot was refused: %v", err)
	}

	t.Run("the commit must carry the checked tree", func(t *testing.T) {
		mustRefuse(t, r.assertSnapshot(good, p1, strings.Repeat("0", 40)), "not the checked tree")
	})
	t.Run("rev-list C --not PARENT must be exactly C", func(t *testing.T) {
		two := mk(mk(p1)) // two new commits on the parent
		mustRefuse(t, r.assertOnlyNewCommit(two, p1), "would add 2 commits")
		mustRefuse(t, r.assertSnapshot(two, p1, tree), "would add 2 commits")
		if err := r.assertOnlyNewCommit(good, p1); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the history must be the parent's plus one", func(t *testing.T) {
		// Built on the parent's own parent: one new commit, but a history
		// one commit short. Only the count sees it among these two.
		short := mk(p0)
		if err := r.assertOnlyNewCommit(short, p1); err != nil {
			t.Fatalf("precondition: this violation must pass the first assertion, got %v", err)
		}
		mustRefuse(t, r.assertOneMoreThanParent(short, p1), "has 2 commits", "must have 3")
		mustRefuse(t, r.assertSnapshot(short, p1, tree), "must have 3")
		if err := r.assertOneMoreThanParent(good, p1); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the parent must be an ancestor", func(t *testing.T) {
		root := mk() // no parent at all
		mustRefuse(t, r.assertParentIsAncestor(root, p1), "not an ancestor", "fast-forward")
		mustRefuse(t, r.assertParentIsAncestor(mk(p0), p1), "not an ancestor")
		if err := r.assertParentIsAncestor(good, p1); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPublishIsFencedAndPushesExactlyOneObject(t *testing.T) {
	w := newWorld(t)
	c, err := w.clone().Commit(w.src, w.commitOpts())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("without the confirmation nothing is pushed", func(t *testing.T) {
		res, err := w.clone().Publish(w.src, w.publishOpts(c.Tree))
		var nc *NotConfirmed
		if !errors.As(err, &nc) {
			t.Fatalf("want NotConfirmed, got %v", err)
		}
		if nc.Object != c.Commit || nc.Target != "refs/heads/main" || nc.Mirror != w.mirror || res.Pushed {
			t.Fatalf("dry run: %+v pushed=%v", nc, res.Pushed)
		}
		if got := w.mirrorMain(); got != "" {
			t.Fatalf("a dry run moved the mirror to %s", got)
		}
	})
	t.Run("a wrong confirmation pushes nothing", func(t *testing.T) {
		for _, confirm := range []string{"yes", c.Tree, c.Tag, c.Commit[:39], w.baseline} {
			opt := w.publishOpts(c.Tree)
			opt.Confirm = confirm
			res, err := w.clone().Publish(w.src, opt)
			var nc *NotConfirmed
			if !errors.As(err, &nc) || res.Pushed {
				t.Fatalf("confirm %q: want NotConfirmed, got %v", confirm, err)
			}
		}
		if got := w.mirrorMain(); got != "" {
			t.Fatalf("the mirror moved to %s", got)
		}
	})
	t.Run("the tag is refused before the commit is on the mirror", func(t *testing.T) {
		opt := w.publishOpts(c.Tree)
		opt.Ref, opt.Confirm = "tag", c.Tag
		_, err := w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "the tag is pushed only after the commit")
		if exporttest.RemoteRef(t, w.mirror, "refs/tags/v0.2.0") != "" {
			t.Fatal("a tag was pushed")
		}
	})
	t.Run("with the exact commit id the snapshot is pushed, and only it", func(t *testing.T) {
		opt := w.publishOpts(c.Tree)
		opt.Confirm = c.Commit
		res, err := w.clone().Publish(w.src, opt)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Pushed || res.Object != c.Commit {
			t.Fatalf("%+v", res)
		}
		if got := w.mirrorMain(); got != c.Commit {
			t.Fatalf("mirror main is %s, want %s", got, c.Commit)
		}
		refs, _ := exporttest.GitIn(w.mirror, 0, "for-each-ref", "--format=%(refname)")
		if refs != "refs/heads/main" {
			t.Fatalf("the mirror has refs other than main:\n%s", refs)
		}
		// Exactly one commit on the mirror, a ROOT, and it has the public tree.
		log, _ := exporttest.GitIn(w.mirror, 0, "rev-list", "--parents", "refs/heads/main")
		if log != c.Commit {
			t.Fatalf("the mirror's history is not the one parentless snapshot:\n%s", log)
		}
		if got := exporttest.RemoteRef(t, w.mirror, "refs/heads/main^{tree}"); got != c.Tree {
			t.Fatalf("the mirror's main carries tree %s, want %s", got, c.Tree)
		}
		if out, err := exporttest.GitIn(w.mirror, 0, "cat-file", "-e", w.baseline); err == nil {
			t.Fatalf("an internal commit is in the mirror's object store: %s", out)
		}
		files, _ := exporttest.GitIn(w.mirror, 0, "ls-tree", "-r", "--name-only", "refs/heads/main")
		for _, f := range strings.Split(files, "\n") {
			if strings.HasPrefix(f, "release/") || strings.HasPrefix(f, ".beads/") || strings.HasPrefix(f, "docs/plans/") || f == "CLAUDE.md" {
				t.Fatalf("internal-only path on the mirror: %s", f)
			}
		}
		// No object of an internal-only file reached the mirror.
		for _, p := range []string{"release/deny-internal.txt", "CLAUDE.md", ".beads/issues.jsonl", "docs/plans/plan.md"} {
			id := w.origin.Git("rev-parse", w.src+":"+p)
			if out, err := exporttest.GitIn(w.mirror, 0, "cat-file", "-e", id); err == nil {
				t.Fatalf("the blob of %s is in the mirror's object store: %s", p, out)
			}
		}
		if out, err := exporttest.GitIn(w.mirror, 0, "cat-file", "-e", w.src); err == nil {
			t.Fatalf("the internal source commit is in the mirror's object store: %s", out)
		}
	})
	t.Run("a second root is refused: the mirror holds a previous export", func(t *testing.T) {
		opt := w.publishOpts(c.Tree)
		opt.Confirm = c.Commit
		_, err := w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "is not empty", "a previous export exists there", c.Commit, "exported from "+w.src)
	})
	t.Run("the tag: dry run, then the exact tag id", func(t *testing.T) {
		opt := w.publishOpts(c.Tree)
		opt.Ref = "tag"
		_, err := w.clone().Publish(w.src, opt)
		var nc *NotConfirmed
		if !errors.As(err, &nc) || nc.Object != c.Tag || nc.Target != "refs/tags/v0.2.0" {
			t.Fatalf("want NotConfirmed for the tag, got %v", err)
		}
		opt.Confirm = c.Commit // the commit id does not confirm a tag push
		if _, err := w.clone().Publish(w.src, opt); !errors.As(err, &nc) {
			t.Fatalf("want NotConfirmed, got %v", err)
		}
		if exporttest.RemoteRef(t, w.mirror, "refs/tags/v0.2.0") != "" {
			t.Fatal("a tag was pushed without confirmation")
		}
		opt.Confirm = c.Tag
		res, err := w.clone().Publish(w.src, opt)
		if err != nil || !res.Pushed {
			t.Fatalf("%v %+v", err, res)
		}
		// The ref holds the TAG OBJECT, which points at the commit.
		if got := exporttest.RemoteRef(t, w.mirror, "refs/tags/v0.2.0"); got != c.Tag {
			t.Fatalf("mirror tag is %s, want the tag object %s", got, c.Tag)
		}
		if got := exporttest.RemoteRef(t, w.mirror, "refs/tags/v0.2.0^{commit}"); got != c.Commit {
			t.Fatalf("the tag points at %s", got)
		}
		_, err = w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "the mirror already has refs/tags/v0.2.0")
	})
	t.Run("verify: the published commit and tag are the export of the source", func(t *testing.T) {
		for _, public := range []string{c.Commit, "refs/heads/main", "refs/tags/v0.2.0"} {
			v, err := w.clone().Verify(w.src, w.mirror, public)
			if err != nil {
				t.Fatalf("verify %s: %v", public, err)
			}
			if !v.Equal || v.PublicCommit != c.Commit || v.PublicTree != c.Tree || v.Recomputed != c.Tree || v.Version != "v0.2.0" {
				t.Fatalf("verify %s: %+v", public, v)
			}
		}
	})
	t.Run("the next release goes on top of the previous export", func(t *testing.T) {
		notes := map[string]string{"docs/releases/v0.3.0.md": "# v0.3.0\n\nThe third release.\n", "README.md": "# gonk 0.3\n"}
		src2 := w.origin.Commit("work for v0.3.0", notes)
		next := func() PublishOptions {
			return PublishOptions{
				Version: "v0.3.0", Parent: c.Commit, Mirror: w.mirror, PrevSource: w.src,
				Ref: "main", Check: withScanner,
			}
		}
		c2, err := w.clone().Commit(src2, CommitOptions{Version: "v0.3.0", Parent: c.Commit, Mirror: w.mirror, Check: withScanner})
		if err != nil {
			t.Fatal(err)
		}

		opt := next()
		opt.ReviewedTree = c2.Tree
		opt.PrevSource = ""
		_, err = w.clone().Publish(src2, opt)
		mustRefuse(t, err, "needs the previous release's source commit")

		// A root for the NEXT release: the mirror is not empty, and what is
		// there is an export.
		opt = next()
		opt.ReviewedTree = c2.Tree
		opt.Root, opt.Parent, opt.PrevSource = true, "", ""
		_, err = w.clone().Publish(src2, opt)
		mustRefuse(t, err, "is not empty", "a previous export exists there")
		if got := w.mirrorMain(); got != c.Commit {
			t.Fatalf("a refused root moved the mirror to %s", got)
		}

		opt = next()
		opt.ReviewedTree = c2.Tree
		opt.PrevSource = src2
		_, err = w.clone().Publish(src2, opt)
		mustRefuse(t, err, "the same commit")

		// A previous source that is not what the mirror head was exported
		// from.
		opt = next()
		opt.ReviewedTree = c2.Tree
		opt.PrevSource = w.baseline
		_, err = w.clone().Publish(src2, opt)
		mustRefuse(t, err, "is not the export of the previous source")

		opt = next()
		opt.ReviewedTree = c2.Tree
		opt.Confirm = c2.Commit
		res, err := w.clone().Publish(src2, opt)
		if err != nil || !res.Pushed {
			t.Fatalf("%v", err)
		}
		if got := w.mirrorMain(); got != c2.Commit {
			t.Fatalf("mirror main %s, want %s", got, c2.Commit)
		}
		// The mirror's whole history: the root, then one fast-forward child.
		history, _ := exporttest.GitIn(w.mirror, 0, "rev-list", "--parents", "refs/heads/main")
		if history != c2.Commit+" "+c.Commit+"\n"+c.Commit {
			t.Fatalf("the mirror's history:\n%s", history)
		}
		// The child's parent is, tree for tree, the export of the PREVIOUS
		// source -- recomputed here, not read from a report.
		prev, err := w.clone().Tree(w.src, "v0.2.0")
		if err != nil {
			t.Fatal(err)
		}
		if got := exporttest.RemoteRef(t, w.mirror, "refs/heads/main^^{tree}"); got != prev.Tree {
			t.Fatalf("the parent of the v0.3.0 snapshot carries tree %s; the export of the previous source is %s", got, prev.Tree)
		}
		v, err := w.clone().Verify(src2, w.mirror, "refs/heads/main")
		if err != nil || !v.Equal || v.PublicTree != c2.Tree {
			t.Fatalf("verify the v0.3.0 snapshot: %v %+v", err, v)
		}
		// Releases move forward: the older source cannot follow the newer.
		opt = PublishOptions{
			Version: "v0.2.0", Parent: c2.Commit, Mirror: w.mirror, PrevSource: src2, ReviewedTree: c.Tree,
			Ref: "main", Check: withScanner,
		}
		_, err = w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "releases move forward")
	})
}

func TestPublishRefusals(t *testing.T) {
	setup := func(t *testing.T) (*world, *CommitResult) {
		w := newWorld(t)
		c, err := w.clone().Commit(w.src, w.commitOpts())
		if err != nil {
			t.Fatal(err)
		}
		return w, c
	}
	confirmed := func(w *world, c *CommitResult) PublishOptions {
		opt := w.publishOpts(c.Tree)
		opt.Confirm = c.Commit
		return opt
	}
	unmoved := func(t *testing.T, w *world) {
		t.Helper()
		if got := w.mirrorMain(); got != "" {
			t.Fatalf("the mirror moved to %s", got)
		}
	}

	t.Run("no mirror named", func(t *testing.T) {
		w, c := setup(t)
		opt := confirmed(w, c)
		opt.Mirror = " "
		_, err := w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "explicit mirror")
	})
	t.Run("a remote mirror without the release variable, before any network", func(t *testing.T) {
		w, c := setup(t)
		notADir := filepath.Join(t.TempDir(), "mirror-file")
		if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, mirror := range []string{
			"https://mirror.invalid/org/repo.git", "git@mirror.invalid:org/repo.git", "file://" + w.mirror,
			"relative/dir", ".", // a relative path, even one that exists
			filepath.Join(t.TempDir(), "no-such-dir"), notADir, // absolute, but not a directory
		} {
			for _, env := range []string{"", "v0.3.0", "yes"} {
				t.Setenv(PublishEnv, env)
				opt := confirmed(w, c)
				opt.Mirror = mirror
				res, err := w.clone().Publish(w.src, opt)
				mustRefuse(t, err, "requires "+PublishEnv+"=v0.2.0")
				if res != nil {
					t.Fatalf("the fence let work begin for %s", mirror)
				}
			}
		}
		unmoved(t, w)
	})
	t.Run("a remote mirror WITH the variable passes that fence (and is then confirmed like any other)", func(t *testing.T) {
		w, c := setup(t)
		t.Setenv(PublishEnv, "v0.2.0")
		opt := w.publishOpts(c.Tree)
		opt.Mirror = "file://" + w.mirror
		_, err := w.clone().Publish(w.src, opt)
		var nc *NotConfirmed
		if !errors.As(err, &nc) {
			t.Fatalf("want NotConfirmed, got %v", err)
		}
		unmoved(t, w)
	})
	t.Run("a tree other than the reviewed one", func(t *testing.T) {
		w, c := setup(t)
		// The review recorded the tree of w.src; one byte later it is
		// another tree.
		src2 := w.origin.Commit("one byte after the review", map[string]string{"README.md": "# gonk.\n"})
		opt := confirmed(w, c)
		_, err := w.clone().Publish(src2, opt)
		mustRefuse(t, err, "is not the reviewed tree "+c.Tree)
		opt.ReviewedTree = ""
		_, err = w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "needs the reviewed tree id")
		unmoved(t, w)
	})
	t.Run("a root on a mirror that is not empty: someone pushed there", func(t *testing.T) {
		w, c := setup(t)
		w.origin.Git("checkout", "--quiet", "-b", "theirs", w.baseline)
		theirs := w.origin.Commit("pushed to the mirror", map[string]string{"their.txt": "x\n"})
		w.origin.Git("push", "--quiet", w.mirror, theirs+":refs/heads/main")
		w.origin.Git("checkout", "--quiet", "main")
		_, err := w.clone().Publish(w.src, confirmed(w, c))
		mustRefuse(t, err, "is not empty", "refs/heads/main", "a root snapshot goes to an empty mirror only")
		if got := w.mirrorMain(); got != theirs {
			t.Fatalf("the mirror was overwritten: %s", got)
		}
	})
	t.Run("a root on a mirror that has no main but is not empty", func(t *testing.T) {
		for _, ref := range []string{"refs/heads/other", "refs/tags/v0.0.1", "refs/pull/1/head"} {
			w, c := setup(t)
			w.origin.Git("push", "--quiet", w.mirror, w.baseline+":"+ref)
			_, err := w.clone().Publish(w.src, confirmed(w, c))
			mustRefuse(t, err, "is not empty", ref)
			unmoved(t, w)
		}
	})
	t.Run("a root is declared, with no parent and no previous source", func(t *testing.T) {
		w, c := setup(t)
		opt := confirmed(w, c)
		opt.Parent = w.baseline
		res, err := w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "a root snapshot has no parent")
		if res != nil {
			t.Fatal("the fence let work begin for a root with a parent")
		}
		opt = confirmed(w, c)
		opt.PrevSource = w.baseline
		res, err = w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "a root snapshot has no previous release")
		if res != nil {
			t.Fatal("the fence let work begin for a root with a previous source")
		}
		unmoved(t, w)
	})
	// The retired rule: there is no baseline any more. A snapshot that is not
	// a declared root must name the previous release's source, whatever
	// parent it is given -- the old mirror head included.
	t.Run("no parent is trusted without a previous source", func(t *testing.T) {
		w, c := setup(t)
		for _, parent := range []string{w.baseline, "0b86bb94fad4a453fcc030eadc1af66508689dfb"} {
			opt := confirmed(w, c)
			opt.Root, opt.Parent = false, parent
			res, err := w.clone().Publish(w.src, opt)
			mustRefuse(t, err, "needs the previous release's source commit")
			if res != nil {
				t.Fatalf("the fence let work begin for parent %s", parent)
			}
		}
		unmoved(t, w)
	})
	t.Run("a later release on a mirror that is empty", func(t *testing.T) {
		w, c := setup(t)
		src2 := w.origin.Commit("next", map[string]string{"docs/releases/v0.3.0.md": "# v0.3.0\n\nThird.\n"})
		_, err := w.clone().Publish(src2, PublishOptions{
			Version: "v0.3.0", Parent: c.Commit, PrevSource: w.src, Mirror: w.mirror,
			ReviewedTree: c.Tree, Ref: "main", Confirm: c.Commit, Check: withScanner,
		})
		mustRefuse(t, err, "the mirror has no main", "root export")
		unmoved(t, w)
	})
	t.Run("a later release on a mirror that moved: not a fast-forward", func(t *testing.T) {
		w, c := setup(t)
		if _, err := w.clone().Publish(w.src, confirmed(w, c)); err != nil {
			t.Fatal(err)
		}
		// Someone merges on the mirror, on top of the v0.2.0 snapshot.
		work := filepath.Join(t.TempDir(), "theirs")
		if out, err := exporttest.GitIn(t.TempDir(), 0, "clone", "--quiet", "--", w.mirror, work); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if err := os.WriteFile(filepath.Join(work, "their.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "--all"}, {"commit", "--quiet", "-m", "merged on the mirror"}, {"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
			if out, err := exporttest.GitIn(work, 7, args...); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		theirs := w.mirrorMain()
		src2 := w.origin.Commit("next", map[string]string{"docs/releases/v0.3.0.md": "# v0.3.0\n\nThird.\n"})
		c2, err := w.clone().Commit(src2, CommitOptions{Version: "v0.3.0", Parent: c.Commit, Mirror: w.mirror, Check: withScanner})
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.clone().Publish(src2, PublishOptions{
			Version: "v0.3.0", Parent: c.Commit, PrevSource: w.src, Mirror: w.mirror,
			ReviewedTree: c2.Tree, Ref: "main", Confirm: c2.Commit, Check: withScanner,
		})
		mustRefuse(t, err, "the mirror's main is "+theirs, "would not be a fast-forward")
		if got := w.mirrorMain(); got != theirs {
			t.Fatalf("the mirror was overwritten: %s", got)
		}
	})
	t.Run("a tree that fails the check", func(t *testing.T) {
		w, _ := setup(t)
		src := w.origin.Commit("leak", map[string]string{"docs/guide.md": cat("gitlab.", "zork", ".lan\n")})
		opt := w.publishOpts(strings.Repeat("34", 20))
		opt.Confirm = strings.Repeat("34", 20)
		_, err := w.clone().Publish(src, opt)
		mustRefuse(t, err, "does not pass the check", "deny: I1")
		unmoved(t, w)
	})
	t.Run("a guard-only check, an unknown ref, a bad previous source", func(t *testing.T) {
		w, c := setup(t)
		opt := confirmed(w, c)
		opt.Check = CheckOptions{SkipScanner: true}
		_, err := w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "guard-only")
		opt = confirmed(w, c)
		opt.Ref = "refs/heads/release"
		_, err = w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "pushes \"main\" or \"tag\"")
		opt = confirmed(w, c)
		opt.Root, opt.Parent, opt.PrevSource = false, w.baseline, "abc"
		_, err = w.clone().Publish(w.src, opt)
		mustRefuse(t, err, "previous source must be a full")
		unmoved(t, w)
	})
}

func TestVerifyRefusesWhatIsNotTheExport(t *testing.T) {
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

	t.Run("another source", func(t *testing.T) {
		src2 := w.origin.Commit("later", map[string]string{"README.md": "# later\n"})
		_, err := w.clone().Verify(src2, w.mirror, c.Commit)
		mustRefuse(t, err, "says it was exported from "+w.src)
	})
	t.Run("a commit that is not an export", func(t *testing.T) {
		_, err := w.clone().Verify(w.src, w.mirror, w.baseline)
		mustRefuse(t, err, "has no EXPORT-SOURCE")
	})
	t.Run("a commit the mirror does not have, and a ref it does not have", func(t *testing.T) {
		_, err := w.clone().Verify(w.src, w.mirror, strings.Repeat("ef", 20))
		mustRefuse(t, err, "is not a commit on the mirror")
		_, err = w.clone().Verify(w.src, w.mirror, "refs/tags/v9.9.9")
		mustRefuse(t, err, "is not a commit on the mirror")
		_, err = w.clone().Verify(w.src, w.mirror, "main")
		mustRefuse(t, err, "must be a commit id")
		_, err = w.clone().Verify(w.src, "", c.Commit)
		mustRefuse(t, err, "needs the mirror")
	})
	t.Run("a published tree that was altered after the export", func(t *testing.T) {
		// A commit on the mirror that keeps EXPORT-SOURCE and the trailers
		// but changes one public file: it still CLAIMS to be the export.
		work := filepath.Join(t.TempDir(), "tamper")
		if out, err := exporttest.GitIn(t.TempDir(), 0, "clone", "--quiet", "--", w.mirror, work); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# tampered\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"add", "--all"},
			{"commit", "--quiet", "-m", "gonk v0.2.0\n\nGonk-Source-Commit: " + w.src + "\nGonk-Export-Tree: " + c.Tree},
			{"push", "--quiet", "origin", "HEAD:refs/heads/main"},
		} {
			if out, err := exporttest.GitIn(work, 5, args...); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		_, err := w.clone().Verify(w.src, w.mirror, "refs/heads/main")
		mustRefuse(t, err, "tree mismatch")
	})
	t.Run("a published commit whose message names another tree", func(t *testing.T) {
		r := w.clone()
		if err := r.fetchMirror(w.mirror); err != nil {
			t.Fatal(err)
		}
		forged, err := r.gitWith(gitCmd{stdin: []byte("gonk v0.2.0\n\nGonk-Source-Commit: " + w.src + "\nGonk-Export-Tree: " + strings.Repeat("0", 40) + "\n"), env: identityEnv("1700000000 +0000")},
			"commit-tree", c.Tree, "-p", w.baseline)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.verifyCommit(w.src, forged)
		mustRefuse(t, err, "names tree")
		forged, err = r.gitWith(gitCmd{stdin: []byte("gonk v0.2.0\n\nGonk-Export-Tree: " + c.Tree + "\n"), env: identityEnv("1700000000 +0000")},
			"commit-tree", c.Tree, "-p", w.baseline)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.verifyCommit(w.src, forged)
		mustRefuse(t, err, "names source")
	})
}

// TestOnlyPublishPushes is structural: in this package the word "push" as a
// git argument appears once in publish.go (to the mirror) and once in
// record.go (to ORIGIN, refs/export/* only), and nowhere else, so no other
// command can write to a remote.
func TestOnlyPublishPushes(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n := strings.Count(string(data), `"push"`)
		if f == "publish.go" || f == "record.go" {
			if n != 1 {
				t.Errorf("%s must contain exactly one push, found %d", f, n)
			}
			seen += n
			continue
		}
		if n != 0 {
			t.Errorf("%s runs git push; only publish.go and record.go may", f)
		}
	}
	if seen != 2 {
		t.Fatalf("want exactly two pushes in the package (publish.go, record.go), found %d", seen)
	}
}

// The package is published; its source must pass the generic rules. The one
// exemption is the snapshot identity in identity.go, which is the address
// already on every public commit.
func TestOwnSourcePassesTheGuard(t *testing.T) {
	g, err := exportguard.New(nil, []byte("path\tidentity.go\t"+IdentityEmail+"\tthe public commit identity\n"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, pattern := range []string{"*.go", "exporttest/*.go"} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 9 {
		t.Fatalf("found only %d source files", len(files))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range g.Content(filepath.ToSlash(f), data) {
			t.Errorf("own source refused: %s", fd)
		}
	}
	if st := g.Stale(); len(st) != 0 {
		t.Fatalf("the identity exemption is stale: %v", st)
	}
}
