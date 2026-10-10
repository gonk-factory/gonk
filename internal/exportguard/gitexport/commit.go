package gitexport

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gonk-factory/gonk/internal/exportguard"
)

// CommitOptions are the inputs of `commit` besides the source.
type CommitOptions struct {
	Version string // vX.Y.Z
	Parent  string // the public commit the snapshot goes on top of, 40 hex
	// Root declares the snapshot the FIRST commit of a new public history:
	// it has no parent. It is never inferred from a missing Parent.
	Root bool
	// Mirror, when not empty, is fetched (read-only) so that Parent is
	// available. A parent that is already in origin's history needs none,
	// and a root needs no mirror at all.
	Mirror string
	Check  CheckOptions
	// OutDir, when not empty, receives export-report.json and the bundle.
	OutDir string
}

// CommitResult is the report of `commit`.
type CommitResult struct {
	*CheckResult
	Parent     string `json:"parent"`
	Root       bool   `json:"root"`
	Commit     string `json:"commit"`
	Tag        string `json:"tag"`
	OriginTag  string `json:"origin_tag"`
	Message    string `json:"commit_message"`
	TagMessage string `json:"tag_message"`
	Bundle     string `json:"bundle,omitempty"`
}

// NotesPath is where the release notes of a version live in the source tree.
func NotesPath(version string) string { return "docs/releases/" + version + ".md" }

// Commit builds the snapshot commit and the public tag for (src, parent,
// version). It refuses unless the tree passes Check, scanner included, and
// the commit and tag messages pass the guard.
//
// Nothing leaves the clone: the commit and tag are objects in it. Their ids
// are a pure function of (src, parent-or-root, version) -- same identity,
// same dates (the source commit's committer date, in UTC), no signature.
func (r *Repo) Commit(src string, opt CommitOptions) (*CommitResult, error) {
	if !exportguard.ValidVersion(opt.Version) {
		return nil, refuse("%q is not a vX.Y.Z version", opt.Version)
	}
	if opt.Root && opt.Parent != "" {
		return nil, refuse("a root snapshot has no parent; declare a root export or name a parent, not both")
	}
	if !opt.Root && !validID(opt.Parent) {
		return nil, refuse("the parent must be a full 40-hex commit id, got %q (the first commit of a new public history has none: declare a root export)", opt.Parent)
	}
	if opt.Check.SkipScanner {
		return nil, refuse("a snapshot commit is never built from a guard-only check")
	}
	tr, err := r.Tree(src, opt.Version)
	if err != nil {
		return nil, err
	}
	chk, err := r.Check(tr, opt.Check)
	if err != nil {
		return nil, err
	}
	res := &CommitResult{CheckResult: chk, Parent: opt.Parent, Root: opt.Root}
	if !chk.OK {
		return res, refuse("the public tree of %s does not pass the check:\n%s", src, chk.Refusal())
	}

	if !opt.Root {
		if opt.Mirror != "" {
			if err := r.fetchMirror(opt.Mirror); err != nil {
				return res, err
			}
		}
		if typ, err := r.git("cat-file", "-t", opt.Parent); err != nil || typ != "commit" {
			return res, refuse("parent %s is not a commit available here (name the mirror to fetch it from)", opt.Parent)
		}
	}

	notes, ok, err := r.show(src, NotesPath(opt.Version))
	if err != nil {
		return res, err
	}
	if !ok {
		return res, refuse("%s does not exist at %s; a release needs its notes", NotesPath(opt.Version), src)
	}
	para, err := FirstParagraph(notes)
	if err != nil {
		return res, refuse("%s: %v", NotesPath(opt.Version), err)
	}
	res.Message = CommitMessage(opt.Version, para, src, tr.Tree, CoAuthors(notes))
	res.TagMessage = TagMessage(opt.Version, src, tr.Tree)

	// The messages are published text. They pass the same guard as the
	// tree, excused only what the notes file itself is excused.
	var bad []exportguard.Finding
	bad = append(bad, chk.guard.Message("commit-message", []byte(res.Message), NotesPath(opt.Version))...)
	bad = append(bad, chk.guard.Message("tag-message", []byte(res.TagMessage), NotesPath(opt.Version))...)
	if len(bad) > 0 {
		lines := make([]string, len(bad))
		for i, f := range bad {
			lines[i] = "deny: " + f.String()
		}
		res.Findings = append(res.Findings, bad...)
		res.OK = false
		return res, refuse("the release messages do not pass the guard:\n%s", strings.Join(lines, "\n"))
	}

	date, err := r.git("show", "-s", "--format=%ct", src)
	if err != nil {
		return res, err
	}
	if _, err := strconv.ParseInt(date, 10, 64); err != nil {
		return res, fmt.Errorf("unexpected committer date %q", date)
	}
	stamp := date + " +0000"

	commitTree := []string{"commit-tree", tr.Tree}
	if !opt.Root {
		commitTree = append(commitTree, "-p", opt.Parent)
	}
	res.Commit, err = r.gitWith(gitCmd{stdin: []byte(res.Message), env: identityEnv(stamp)}, commitTree...)
	if err != nil {
		return res, err
	}
	res.Tag, err = r.mktag(res.Commit, opt.Version, stamp, res.TagMessage)
	if err != nil {
		return res, err
	}
	// The INTERNAL tag of the same name points at the source commit and
	// records the public side. It is built here because the public commit id
	// is known before anything is pushed; pushing it is not this tool's job.
	res.OriginTag, err = r.mktag(src, opt.Version, stamp,
		"gonk "+opt.Version+"\n\nGonk-Export-Tree: "+tr.Tree+"\nGonk-Public-Commit: "+res.Commit+"\n")
	if err != nil {
		return res, err
	}

	if err := r.assertShape(res.Commit, opt.Parent, tr.Tree, opt.Root); err != nil {
		return res, err
	}

	if opt.OutDir != "" {
		if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
			return res, err
		}
		exportRef := "refs/export/" + opt.Version
		tagRef := "refs/public-tags/" + opt.Version
		if _, err := r.git("update-ref", exportRef, res.Commit); err != nil {
			return res, err
		}
		if _, err := r.git("update-ref", tagRef, res.Tag); err != nil {
			return res, err
		}
		bundle, err := filepath.Abs(filepath.Join(opt.OutDir, "gonk-"+opt.Version+"-export.bundle"))
		if err != nil {
			return res, err
		}
		// Thin against the parent; a root snapshot has no prerequisite.
		create := []string{"bundle", "create", "--quiet", bundle, exportRef, tagRef}
		if !opt.Root {
			create = append(create, "^"+opt.Parent)
		}
		if _, err := r.git(create...); err != nil {
			return res, err
		}
		res.Bundle = bundle
	}
	return res, nil
}

func identityEnv(stamp string) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + IdentityName, "GIT_AUTHOR_EMAIL=" + IdentityEmail, "GIT_AUTHOR_DATE=" + stamp,
		"GIT_COMMITTER_NAME=" + IdentityName, "GIT_COMMITTER_EMAIL=" + IdentityEmail, "GIT_COMMITTER_DATE=" + stamp,
	}
}

// mktag writes an annotated, UNSIGNED tag object. Unsigned because a
// signature is not reproducible.
func (r *Repo) mktag(object, name, stamp, message string) (string, error) {
	body := "object " + object + "\ntype commit\ntag " + name + "\ntagger " + IdentityName + " <" + IdentityEmail + "> " + stamp + "\n\n" + message
	return r.gitWith(gitCmd{stdin: []byte(body)}, "mktag")
}

// assertSnapshot holds what must be true of a snapshot commit before
// anything leaves the machine: it carries the checked tree, and it is
// exactly one new commit on top of parent.
func (r *Repo) assertSnapshot(commit, parent, tree string) error {
	got, err := r.git("rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return err
	}
	if got != tree {
		return refuse("commit %s carries tree %s, not the checked tree %s", commit, got, tree)
	}
	if err := r.assertOnlyNewCommit(commit, parent); err != nil {
		return err
	}
	if err := r.assertOneMoreThanParent(commit, parent); err != nil {
		return err
	}
	return r.assertParentIsAncestor(commit, parent)
}

// assertShape holds the shape of a snapshot, root or child.
func (r *Repo) assertShape(commit, parent, tree string, root bool) error {
	if root {
		return r.assertRootSnapshot(commit, tree)
	}
	return r.assertSnapshot(commit, parent, tree)
}

// assertRootSnapshot holds what must be true of the FIRST snapshot of a new
// public history before anything leaves the machine: it carries the checked
// tree, it has no parent, and its whole history is itself -- so pushing it
// publishes one commit and nothing an internal commit ever held.
func (r *Repo) assertRootSnapshot(commit, tree string) error {
	have, err := r.git("rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return err
	}
	if have != tree {
		return refuse("commit %s carries tree %s, not the checked tree %s", commit, have, tree)
	}
	if err := r.assertHasNoParent(commit); err != nil {
		return err
	}
	return r.assertHistoryIsOnlyItself(commit)
}

// assertHasNoParent: the commit object names no parent.
func (r *Repo) assertHasNoParent(commit string) error {
	out, err := r.git("rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return err
	}
	if f := strings.Fields(out); len(f) != 1 || f[0] != commit {
		return refuse("root snapshot %s has a parent (%s); the first commit of a new history has none", commit, strings.Join(f[1:], " "))
	}
	return nil
}

// assertHistoryIsOnlyItself: `git rev-list C` is exactly C.
func (r *Repo) assertHistoryIsOnlyItself(commit string) error {
	history, err := r.git("rev-list", commit)
	if err != nil {
		return err
	}
	if history != commit {
		return refuse("root snapshot %s would publish %d commits, not exactly itself", commit, len(strings.Fields(history)))
	}
	return nil
}

// assertOnlyNewCommit: `git rev-list C --not PARENT` is exactly C.
func (r *Repo) assertOnlyNewCommit(commit, parent string) error {
	out, err := r.git("rev-list", commit, "--not", parent)
	if err != nil {
		return err
	}
	if out != commit {
		return refuse("%s would add %d commits to the mirror, not exactly itself", commit, len(strings.Fields(out)))
	}
	return nil
}

// assertOneMoreThanParent: the history of C is the history of PARENT plus
// one commit.
func (r *Repo) assertOneMoreThanParent(commit, parent string) error {
	c, err := r.git("rev-list", "--count", commit)
	if err != nil {
		return err
	}
	p, err := r.git("rev-list", "--count", parent)
	if err != nil {
		return err
	}
	cn, err1 := strconv.Atoi(c)
	pn, err2 := strconv.Atoi(p)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("unexpected rev-list count %q / %q", c, p)
	}
	if cn != pn+1 {
		return refuse("%s has %d commits in its history; the parent has %d, so it must have %d", commit, cn, pn, pn+1)
	}
	return nil
}

// assertParentIsAncestor: PARENT is an ancestor of C, so the push is a
// fast-forward.
func (r *Repo) assertParentIsAncestor(commit, parent string) error {
	ok, err := r.gitOK("merge-base", "--is-ancestor", parent, commit)
	if err != nil {
		return err
	}
	if !ok {
		return refuse("parent %s is not an ancestor of %s; the push would not be a fast-forward", parent, commit)
	}
	return nil
}

// fetchMirror fetches the mirror's main branch and tags into private refs.
// Read-only. Public tags never land in refs/tags, where they would collide
// with the internal tags of the same name.
func (r *Repo) fetchMirror(mirror string) error {
	_, err := netGit(r.dir, "fetch", "--quiet", "--no-tags", "--", mirror,
		"+refs/heads/main:refs/mirror/main", "+refs/tags/*:refs/public-tags/*")
	return err
}

// FirstParagraph returns the first paragraph of release notes: the first
// block of non-blank lines that is not a heading.
func FirstParagraph(notes []byte) (string, error) {
	var para []string
	for _, line := range strings.Split(strings.ReplaceAll(string(notes), "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "" && len(para) > 0:
			return strings.Join(para, "\n"), nil
		case t == "" || (len(para) == 0 && strings.HasPrefix(t, "#")):
			continue
		}
		para = append(para, strings.TrimRight(line, " \t"))
	}
	if len(para) == 0 {
		return "", fmt.Errorf("the notes have no paragraph")
	}
	return strings.Join(para, "\n"), nil
}

var reCoAuthor = regexp.MustCompile(`^Co-authored-by: \S.* <[^<>\s]+@[^<>\s]+>$`)

// CoAuthors returns the Co-authored-by lines of the release notes, in file
// order, without duplicates. One is written per contributor whose public
// pull request is in the release.
func CoAuthors(notes []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(string(notes), "\r\n", "\n"), "\n") {
		if reCoAuthor.MatchString(line) && !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}

// CommitMessage is the fixed template of a snapshot commit.
func CommitMessage(version, paragraph, src, tree string, coAuthors []string) string {
	var b strings.Builder
	b.WriteString("gonk " + version + "\n\n")
	b.WriteString(paragraph + "\n\n")
	b.WriteString("This is a release snapshot. It is the exported tree of the source commit\n")
	b.WriteString("named below; see CONTRIBUTING.md for how changes reach this repository.\n\n")
	b.WriteString("Gonk-Source-Commit: " + src + "\n")
	b.WriteString("Gonk-Export-Tree: " + tree + "\n")
	for _, c := range coAuthors {
		b.WriteString(c + "\n")
	}
	return b.String()
}

// TagMessage is the message of the public tag.
func TagMessage(version, src, tree string) string {
	return "gonk " + version + "\n\nGonk-Source-Commit: " + src + "\nGonk-Export-Tree: " + tree + "\n"
}

// trailer returns the value of a trailer line in a commit or tag message.
func trailer(message, key string) string {
	for _, line := range strings.Split(message, "\n") {
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return v
		}
	}
	return ""
}
