package gitexport

import (
	"strings"

	"github.com/gonk-factory/gonk/internal/exportguard"
)

// VerifyResult is the report of `verify`.
type VerifyResult struct {
	Source       string `json:"source_commit"`
	Version      string `json:"version"`
	PublicCommit string `json:"public_commit"`
	PublicTree   string `json:"public_tree"`
	Recomputed   string `json:"recomputed_tree"`
	Equal        bool   `json:"equal"`
}

// Verify answers "is this published commit the export of this source?"
// without trusting any earlier run: it fetches public (a commit id, or a ref
// such as refs/tags/v0.2.0) from the mirror, read-only, recomputes the public
// tree of src here, and compares tree ids. It also requires the published
// commit and its EXPORT-SOURCE file to NAME src.
func (r *Repo) Verify(src, mirror, public string) (*VerifyResult, error) {
	if mirror == "" {
		return nil, refuse("verify needs the mirror to read the published commit from")
	}
	if err := r.fetchMirror(mirror); err != nil {
		return nil, err
	}
	rev := public
	if !validID(public) {
		// A ref name on the mirror: heads were fetched to refs/mirror, tags
		// to refs/public-tags.
		switch {
		case strings.HasPrefix(public, "refs/tags/"):
			rev = "refs/public-tags/" + strings.TrimPrefix(public, "refs/tags/")
		case public == "refs/heads/main":
			rev = "refs/mirror/main"
		default:
			return nil, refuse("public must be a commit id, refs/heads/main or refs/tags/<name>; got %q", public)
		}
	}
	commit, err := r.git("rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return nil, refuse("%s is not a commit on the mirror", public)
	}
	return r.verifyCommit(src, commit)
}

// verifyCommit compares a public commit that is already in the clone.
func (r *Repo) verifyCommit(src, commit string) (*VerifyResult, error) {
	res := &VerifyResult{Source: src, PublicCommit: commit}
	var err error
	res.PublicTree, err = r.git("rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return nil, err
	}
	data, ok, err := r.show(commit, exportguard.ExportSourcePath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return res, refuse("%s has no %s: it is not an export", commit, exportguard.ExportSourcePath)
	}
	es, err := exportguard.ParseExportSource(data)
	if err != nil {
		return res, refuse("%s: %v", commit, err)
	}
	res.Version = es.Version
	if es.SourceCommit != src {
		return res, refuse("%s says it was exported from %s, not from %s", commit, es.SourceCommit, src)
	}
	tr, err := r.Tree(src, es.Version)
	if err != nil {
		return res, err
	}
	res.Recomputed = tr.Tree
	res.Equal = res.PublicTree == res.Recomputed
	if !res.Equal {
		return res, refuse("tree mismatch: %s carries %s, the recomputed export of %s is %s", commit, res.PublicTree, src, res.Recomputed)
	}
	msg, err := r.git("show", "-s", "--format=%B", commit)
	if err != nil {
		return res, err
	}
	if got := trailer(msg, "Gonk-Source-Commit"); got != src {
		return res, refuse("the message of %s names source %q, not %s", commit, got, src)
	}
	if got := trailer(msg, "Gonk-Export-Tree"); got != res.PublicTree {
		return res, refuse("the message of %s names tree %q, not its own tree %s", commit, got, res.PublicTree)
	}
	return res, nil
}
