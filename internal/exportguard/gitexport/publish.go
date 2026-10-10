package gitexport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gonk-factory/gonk/internal/exportguard"
)

// PublishEnv is the environment variable that must hold the version being
// released before Publish will talk to a mirror that is not a local
// directory.
const PublishEnv = "GONK_RELEASE_PUBLISH"

// PublishOptions are the inputs of `publish`.
type PublishOptions struct {
	Version string
	// Parent is the public commit the snapshot goes on top of. Empty for a
	// root export.
	Parent string
	// Root declares this the FIRST commit of a new public history, published
	// to a mirror that is EMPTY. It is never inferred: a mirror that happens
	// to be empty does not make a release a root, and a declared root is
	// refused when the mirror holds anything at all.
	Root bool
	// Mirror is where the snapshot goes. There is NO default.
	Mirror string
	// ReviewedTree is the tree id the release review recorded. Publish
	// refuses any other tree.
	ReviewedTree string
	// PrevSource is the source commit of the previous release. Every
	// snapshot that is not a declared root needs it: Parent must be, tree
	// for tree, its export. There is no other trusted parent -- the fixed
	// "baseline" commit this field used to fall back to named the head of a
	// mirror that no longer exists, and is gone.
	PrevSource string
	// Ref selects what is pushed: "main" (the snapshot commit to
	// refs/heads/main) or "tag" (the tag object to refs/tags/<version>,
	// only once main already is the snapshot).
	Ref string
	// Confirm must equal the id of the object about to be pushed. Without
	// it Publish does everything except push, and says what it would push.
	Confirm string
	Check   CheckOptions
}

// PublishResult is the report of `publish`.
type PublishResult struct {
	*CommitResult
	Mirror string `json:"mirror"`
	Ref    string `json:"ref"`
	Object string `json:"object"`
	Pushed bool   `json:"pushed"`
}

// Publish is the ONLY function in this package that writes to a mirror.
//
// It recomputes everything from origin (it trusts no file from an earlier
// run), and pushes one object to one ref only if every one of these holds:
//
//  1. a mirror was named explicitly, and -- unless it is a local directory --
//     the environment variable PublishEnv holds the version;
//  2. the public tree passes Check, scanner included, and the messages pass
//     the guard (Commit);
//  3. the computed tree is the reviewed tree;
//  4. the mirror is what it must be. For a declared ROOT export: it is
//     empty -- no branch, no tag, no ref of any kind -- and a mirror whose
//     main is itself an export is refused by name (a previous export exists;
//     the release belongs on top of it). Otherwise: its main is still
//     exactly Parent (nobody pushed or merged there), so the push is a
//     fast-forward by construction;
//  5. Parent is what it must be: absent for a root; otherwise the export of
//     PrevSource, tree for tree, with PrevSource an ancestor of the source.
//     No parent is trusted on any other ground;
//  6. the snapshot is exactly one new commit carrying the tree: on Parent,
//     or -- a root -- with no parent and no history but itself;
//  7. Confirm names the exact object id.
//
// With everything but 7 it returns the result with Pushed false and a
// NotConfirmed error: a dry run that shows what would be pushed.
func (r *Repo) Publish(src string, opt PublishOptions) (*PublishResult, error) {
	// Fences that need no network come first.
	if strings.TrimSpace(opt.Mirror) == "" {
		return nil, refuse("publish needs an explicit mirror; there is no default")
	}
	if !isLocalDir(opt.Mirror) && os.Getenv(PublishEnv) != opt.Version {
		return nil, refuse("publishing to a remote mirror requires %s=%s in the environment", PublishEnv, opt.Version)
	}
	if !validID(opt.ReviewedTree) {
		return nil, refuse("publish needs the reviewed tree id (40 hex)")
	}
	if opt.Ref != "main" && opt.Ref != "tag" {
		return nil, refuse("publish pushes \"main\" or \"tag\", not %q", opt.Ref)
	}
	switch {
	case opt.Root && opt.Parent != "":
		return nil, refuse("a root snapshot has no parent; declare a root export or name a parent, not both")
	case opt.Root && opt.PrevSource != "":
		return nil, refuse("a root snapshot has no previous release; declare a root export or name the previous source, not both")
	case opt.Root:
	case opt.PrevSource == "":
		return nil, refuse("a snapshot that is not a declared root export needs the previous release's source commit: its parent must be, tree for tree, the export of that source, and no parent is trusted on any other ground")
	case !validID(opt.PrevSource):
		return nil, refuse("the previous source must be a full 40-hex commit id")
	}

	commitOpt := CommitOptions{Version: opt.Version, Root: opt.Root, Check: opt.Check}
	if !opt.Root {
		// The parent is fetched from the mirror, so the mirror must have a
		// main to fetch.
		head, err := r.lsRemote(opt.Mirror, "refs/heads/main")
		if err != nil {
			return nil, err
		}
		if head == "" {
			return nil, refuse("the mirror has no main (it is empty): nothing can go on top of %s there. The first release of a new public history is a root export", opt.Parent)
		}
		commitOpt.Parent, commitOpt.Mirror = opt.Parent, opt.Mirror
	}
	cr, err := r.Commit(src, commitOpt)
	res := &PublishResult{CommitResult: cr, Mirror: opt.Mirror, Ref: opt.Ref}
	if err != nil {
		return res, err
	}
	if cr.Tree != opt.ReviewedTree {
		return res, refuse("the computed tree %s is not the reviewed tree %s; what is published must be what was reviewed", cr.Tree, opt.ReviewedTree)
	}

	if opt.PrevSource != "" {
		if opt.PrevSource == src {
			return res, refuse("the previous source and the source are the same commit")
		}
		ok, err := r.gitOK("merge-base", "--is-ancestor", opt.PrevSource, src)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, refuse("source %s does not descend from the previous release's source %s; releases move forward", src, opt.PrevSource)
		}
		// The mirror head must BE the previous export: same tree, naming
		// that source. This proves there was no drift without trusting a log.
		if _, err := r.verifyCommit(opt.PrevSource, opt.Parent); err != nil {
			return res, refuse("parent %s is not the export of the previous source %s: %v", opt.Parent, opt.PrevSource, err)
		}
	}

	head, err := r.lsRemote(opt.Mirror, "refs/heads/main")
	if err != nil {
		return res, err
	}
	tagRef := "refs/tags/" + opt.Version
	var target string
	switch opt.Ref {
	case "main":
		if opt.Root {
			if err := r.assertMirrorEmpty(opt.Mirror); err != nil {
				return res, err
			}
		} else if head != opt.Parent {
			return res, refuse("the mirror's main is %s, not the parent %s: someone pushed or merged there, and this would not be a fast-forward", orNone(head), opt.Parent)
		}
		res.Object, target = cr.Commit, "refs/heads/main"
	case "tag":
		if head != cr.Commit {
			return res, refuse("the mirror's main is %s, not the snapshot %s; the tag is pushed only after the commit", orNone(head), cr.Commit)
		}
		existing, err := r.lsRemote(opt.Mirror, tagRef)
		if err != nil {
			return res, err
		}
		if existing != "" {
			return res, refuse("the mirror already has %s", tagRef)
		}
		res.Object, target = cr.Tag, tagRef
	}

	if err := r.assertShape(cr.Commit, opt.Parent, opt.ReviewedTree, opt.Root); err != nil {
		return res, err
	}

	if opt.Confirm != res.Object {
		return res, &NotConfirmed{Object: res.Object, Target: target, Mirror: opt.Mirror}
	}

	// The one push. No force flag, ever: a mirror that moved between the
	// check above and now makes this fail, which is the right outcome.
	if _, err := netGit(r.dir, "push", "--quiet", "--", opt.Mirror, res.Object+":"+target); err != nil {
		return res, err
	}
	after, err := r.lsRemote(opt.Mirror, target)
	if err != nil {
		return res, err
	}
	if after != res.Object {
		return res, fmt.Errorf("after the push the mirror's %s is %s, not %s", target, orNone(after), res.Object)
	}
	res.Pushed = true
	return res, nil
}

// NotConfirmed is the outcome of a publish that passed every check and was
// not given the confirmation: nothing was pushed.
type NotConfirmed struct{ Object, Target, Mirror string }

func (n *NotConfirmed) Error() string {
	return fmt.Sprintf("NOT PUBLISHED (dry run): every check passed; to push %s to %s on %s, repeat with the confirmation set to that object id", n.Object, n.Target, n.Mirror)
}

// assertMirrorEmpty holds the precondition of a root export: the mirror has
// no ref at all. A branch, a tag or a pull-request ref each pin history that a
// root snapshot would sit beside, not replace.
//
// When the mirror's main is itself an export, the refusal says so: a root was
// asked for where a previous export exists, and the release belongs on top of
// it.
func (r *Repo) assertMirrorEmpty(mirror string) error {
	out, err := netGit(r.dir, "ls-remote", "--", mirror)
	if err != nil {
		return err
	}
	var refs []string
	main := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		refs = append(refs, f[1])
		if f[1] == "refs/heads/main" {
			main = f[0]
		}
	}
	if len(refs) == 0 {
		return nil
	}
	if main != "" {
		if _, err := netGit(r.dir, "fetch", "--quiet", "--no-tags", "--", mirror, "+refs/heads/main:refs/mirror/main"); err != nil {
			return err
		}
		if data, ok, err := r.show(main, exportguard.ExportSourcePath); err != nil {
			return err
		} else if ok {
			from := "an unreadable source"
			if es, err := exportguard.ParseExportSource(data); err == nil {
				from = es.SourceCommit
			}
			return refuse("the mirror %s is not empty: a previous export exists there (main is %s, exported from %s). A root snapshot goes to an empty mirror only; publish this release on top of that one, naming its source as the previous source", mirror, main, from)
		}
	}
	return refuse("the mirror %s is not empty (it has %s); a root snapshot goes to an empty mirror only", mirror, strings.Join(refs, ", "))
}

// lsRemote returns the object id of one ref on the mirror, or "" when the
// mirror has no such ref.
func (r *Repo) lsRemote(mirror, ref string) (string, error) {
	out, err := netGit(r.dir, "ls-remote", "--", mirror, ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == ref {
			return f[0], nil
		}
	}
	return "", nil
}

func orNone(id string) string {
	if id == "" {
		return "(absent)"
	}
	return id
}

// isLocalDir reports whether a mirror is a directory on this machine named
// by an absolute path -- a stand-in, as opposed to anything reached over a
// network.
func isLocalDir(mirror string) bool {
	if !filepath.IsAbs(mirror) {
		return false
	}
	st, err := os.Stat(mirror)
	return err == nil && st.IsDir()
}
