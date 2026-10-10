// Package basefacts reads, at ONE pinned commit, the facts about the
// repository's base that an edit or a create is judged against (gonk-cuwn
// X-GLAB2; plan 2.3 CR-EXISTS, CR-PARENT and the fix plan's symlink gap).
//
// One reader serves BOTH workflows (fix and change). The three outcomes for a
// path are kept apart on purpose, because callers treat them completely
// differently:
//
//   - present as a regular file: its bytes (Facts.Blobs), or its tree entry
//     (Facts.Exists for a create path);
//   - absent: the tree says there is no entry (Facts.Absent);
//   - not something an edit may touch: a symlink, submodule or directory at
//     the path, or one in place of an ancestor directory (Facts.Refusals, typed).
//
// A READ ERROR is none of these: Read returns an error and no Facts. Treating
// "the forge did not answer" as "the file is absent" would let a create
// overwrite, or an edit be refused, on our own failure.
package basefacts

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gonk-factory/gonk/pkg/glab"
)

// MaxBlobBytes caps one blob read. A larger file is an ERROR (not readable),
// never silently absent or truncated.
const MaxBlobBytes int64 = 1 << 20

// Reader is the part of *glab.Client that Read uses.
type Reader interface {
	ListTree(ctx context.Context, projectID int64, ref, dir string) ([]glab.TreeEntry, error)
	GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error)
}

// Reason says why a path is refused.
type Reason string

const (
	ReasonSymlink   Reason = "symlink"            // the path is a symlink (120000)
	ReasonSubmodule Reason = "submodule"          // the path is a submodule (160000)
	ReasonDirectory Reason = "directory"          // an edit path is a directory
	ReasonParent    Reason = "parent-not-a-dir"   // an ancestor of the path exists and is not a directory
	ReasonOther     Reason = "not-a-regular-file" // any other non-regular entry
)

// Refusal is a path the reader will not hand to an edit or a create.
type Refusal struct {
	Path   string
	Reason Reason
	// Entry is the offending tree entry: the path itself, or for ReasonParent
	// the ancestor that is not a directory.
	Entry glab.TreeEntry
}

func (r Refusal) Error() string {
	return fmt.Sprintf("basefacts: %s: %s (%s, mode %s)", r.Path, r.Reason, r.Entry.Path, r.Entry.Mode)
}

// Facts is what Read found at the pinned SHA.
type Facts struct {
	SHA string
	// Blobs: edit path -> content, ONLY for a regular file (100644/100755).
	Blobs map[string][]byte
	// Absent: edit paths with no tree entry at the base.
	Absent map[string]bool
	// Exists: create path -> the entry already there, of ANY type (CR-EXISTS).
	Exists map[string]glab.TreeEntry
	// Refusals, sorted by path.
	Refusals []Refusal
}

// Read resolves every edit path and create path against the tree at sha, and
// reads the blob of each edit path that is a regular file. sha must be a full
// commit SHA (never a branch name: the base must not move under us).
//
// Any forge error -- a tree page, an unknown sha, a blob read that fails or is
// over MaxBlobBytes, including a 404 on a blob the tree just listed -- is
// returned as the error and Facts is nil.
func Read(ctx context.Context, r Reader, projectID int64, sha string, editPaths, createPaths []string) (*Facts, error) {
	if !glab.IsCommitSHA(sha) {
		return nil, fmt.Errorf("basefacts: base %q is not a full commit SHA", sha)
	}
	f := &Facts{
		SHA:    sha,
		Blobs:  map[string][]byte{},
		Absent: map[string]bool{},
		Exists: map[string]glab.TreeEntry{},
	}
	w := &walker{r: r, projectID: projectID, sha: sha, dirs: map[string][]glab.TreeEntry{}}
	// The root must list: it is what proves the sha is real, so that a 404 on
	// a SUBDIRECTORY can safely mean "no such directory".
	if _, err := w.list(ctx, ""); err != nil {
		return nil, fmt.Errorf("basefacts: read root tree at %s: %w", sha, err)
	}

	for _, p := range sortedUnique(editPaths) {
		res, err := w.resolve(ctx, p)
		if err != nil {
			return nil, err
		}
		switch {
		case res.blocker != nil:
			f.Refusals = append(f.Refusals, Refusal{Path: p, Reason: ReasonParent, Entry: *res.blocker})
		case !res.found:
			f.Absent[p] = true
		case res.entry.IsRegularFile():
			b, err := r.GetRawFile(ctx, projectID, p, sha, MaxBlobBytes)
			if err != nil {
				return nil, fmt.Errorf("basefacts: read blob %s at %s: %w", p, sha, err)
			}
			f.Blobs[p] = b
		default:
			f.Refusals = append(f.Refusals, Refusal{Path: p, Reason: reasonFor(res.entry), Entry: res.entry})
		}
	}
	for _, p := range sortedUnique(createPaths) {
		res, err := w.resolve(ctx, p)
		if err != nil {
			return nil, err
		}
		switch {
		case res.blocker != nil:
			f.Refusals = append(f.Refusals, Refusal{Path: p, Reason: ReasonParent, Entry: *res.blocker})
		case res.found:
			f.Exists[p] = res.entry
		}
	}
	sort.SliceStable(f.Refusals, func(i, j int) bool { return f.Refusals[i].Path < f.Refusals[j].Path })
	return f, nil
}

func reasonFor(e glab.TreeEntry) Reason {
	switch {
	case e.Mode == glab.ModeSymlink:
		return ReasonSymlink
	case e.Mode == glab.ModeSubmodule || e.Type == "commit":
		return ReasonSubmodule
	case e.IsDir():
		return ReasonDirectory
	}
	return ReasonOther
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range in {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

type walker struct {
	r         Reader
	projectID int64
	sha       string
	dirs      map[string][]glab.TreeEntry // listing cache; nil entry slice = listed
}

// list returns the entries of dir, or (nil, nil) with found=false semantics
// folded in: a 404 on a non-root dir means the directory is not there.
func (w *walker) list(ctx context.Context, dir string) ([]glab.TreeEntry, error) {
	if es, ok := w.dirs[dir]; ok {
		return es, nil
	}
	es, err := w.r.ListTree(ctx, w.projectID, w.sha, dir)
	if err != nil {
		if dir != "" && glab.IsNotFound(err) {
			w.dirs[dir] = []glab.TreeEntry{}
			return w.dirs[dir], nil
		}
		return nil, err
	}
	if es == nil {
		es = []glab.TreeEntry{}
	}
	w.dirs[dir] = es
	return es, nil
}

type resolution struct {
	found   bool
	entry   glab.TreeEntry
	blocker *glab.TreeEntry // an ancestor that exists and is not a directory
}

func (w *walker) resolve(ctx context.Context, p string) (resolution, error) {
	comps := strings.Split(p, "/")
	for _, c := range comps {
		if c == "" || c == "." || c == ".." {
			return resolution{}, fmt.Errorf("basefacts: path %q is not clean and relative", p)
		}
	}
	dir := ""
	for i, name := range comps {
		es, err := w.list(ctx, dir)
		if err != nil {
			return resolution{}, fmt.Errorf("basefacts: read tree %q at %s: %w", dir, w.sha, err)
		}
		var hit *glab.TreeEntry
		for j := range es {
			if es[j].Name == name {
				hit = &es[j]
				break
			}
		}
		if hit == nil {
			return resolution{}, nil
		}
		if i == len(comps)-1 {
			return resolution{found: true, entry: *hit}, nil
		}
		if !hit.IsDir() {
			return resolution{blocker: hit}, nil
		}
		dir = hit.Path
	}
	return resolution{}, nil
}
