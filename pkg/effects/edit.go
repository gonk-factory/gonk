package effects

// Applying a fix batch's edits, and the facts of the resulting change
// (gonk-2usw plan 4.1, 5.3 A5-A6).
//
// ApplyEdits is a pure function: base blobs + edits -> new blobs, or a typed
// refusal. It reads no repository and no forge; the CONTROLLER reads each blob
// at the pinned base SHA and passes it in. That is the whole reason `edit`
// exists instead of a git-format patch: the model states an exact replacement,
// and the diff -- with nothing in it the model could get subtly wrong, and no
// binary hunk, mode change, rename, symlink or gitlink it could smuggle in --
// is computed here.
//
// THE RULE: `old` must occur EXACTLY ONCE in the file as it stands after the
// batch's earlier edits to the same path. Not "the first occurrence", not a
// whitespace-insensitive or fuzzy match: an edit that could mean two things,
// or that matches nothing, is refused, and the whole batch with it.

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// BaseBlobs holds the content, at the pinned base SHA, of the files a fix
// batch edits, keyed by repository path. The controller builds it: one entry
// per path of EditPaths(batch) that EXISTS at the base. A path with no entry
// does not exist there, and an edit to it is refused (ED-MISSING: an edit
// cannot create a file).
//
// A blob the controller could not READ (a forge error) must not be passed as
// an absent entry: that is our failure (ArtifactUnknown), and the caller stops
// before calling anything here.
//
// ONLY REGULAR FILES. A blob says nothing about its tree entry's MODE, and a
// symlink's blob is its target path as text -- perfectly editable UTF-8. So
// the caller must leave out (or refuse) any path whose tree entry at the base
// is not a regular file (mode 100644 / 100755): a symlink (120000) or a
// submodule (160000) is not something an edit may retarget. Nothing in this
// package can check that; it is the reader's obligation.
type BaseBlobs map[string][]byte

// PatchedFile is one file a fix changes: its base content and its content
// after the batch's edits. It is what the apply commits (an `update` action
// with Content, on a branch from the base SHA).
type PatchedFile struct {
	Path string
	// Base is the blob the edits were applied to, as given.
	Base []byte
	// Content is the result. Valid UTF-8, no NUL, non-empty, and different
	// from Base.
	Content []byte
	// Edits is the number of edit effects applied to this file; NewBytes is the
	// total length of their `new` texts.
	Edits    int
	NewBytes int
}

// EditPaths returns the distinct paths a batch's edit effects name, sorted: the
// blobs the controller must read to build BaseBlobs. Only `edit` effects
// contribute. Read them only after ValidateEditPaths has passed.
func EditPaths(b Batch) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range b.Effects {
		if e.Kind == KindEdit && !seen[e.Path] {
			seen[e.Path] = true
			out = append(out, e.Path)
		}
	}
	sort.Strings(out)
	return out
}

// ApplyEdits applies every `edit` effect of b, in batch order, to base, and
// returns the changed files sorted by path. It reads ONLY edit effects --
// with FileWrites (file.go) it is one of the two constructors of repository
// writes, and no other kind, whatever path-like fields it carries, contributes.
//
// The first edit that cannot be applied refuses the batch: the error is a
// *Refusal and no file is returned. base is not modified.
func ApplyEdits(b Batch, base BaseBlobs) ([]PatchedFile, error) {
	files, r := applyEdits(b, base)
	if r != nil {
		return nil, r
	}
	return files, nil
}

func applyEdits(b Batch, base BaseBlobs) ([]PatchedFile, *Refusal) {
	byPath := map[string]*PatchedFile{}
	last := map[string]int{} // path -> index of the last edit effect touching it
	for i, e := range b.Effects {
		if e.Kind != KindEdit { // apply:edit-only
			continue
		}
		// The form rules again, here: this function must be safe on a batch
		// nothing else has looked at, and an unclean path is not a map key.
		if r := editForm(i, e); r != nil {
			return nil, r
		}
		pf := byPath[e.Path]
		if pf == nil {
			blob, ok := base[e.Path]
			if !ok { // apply:missing
				return nil, refuse(RuleEditMissing, "", i, e.Path, "no such file at the base commit; an edit changes an existing file and cannot create one")
			}
			if !utf8.Valid(blob) || bytes.IndexByte(blob, 0) >= 0 { // apply:encoding
				return nil, refuse(RuleEditEncoding, "", i, e.Path, "the file is binary or not valid UTF-8; it cannot be edited")
			}
			pf = &PatchedFile{Path: e.Path, Base: blob, Content: blob}
			byPath[e.Path] = pf
		}
		switch n := occurrences(pf.Content, []byte(e.Old)); {
		case n == 0: // apply:nomatch
			return nil, refuse(RuleEditNoMatch, "", i, e.Path, "`old` does not occur in the file (as it stands after this batch's earlier edits); it must match exactly, byte for byte")
		case n > 1: // apply:ambiguous
			return nil, refuse(RuleEditAmbiguous, "", i, e.Path, "`old` occurs more than once in the file (as it stands after this batch's earlier edits); include enough surrounding text to match exactly once")
		}
		pf.Content = bytes.Replace(pf.Content, []byte(e.Old), []byte(e.New), 1)
		pf.Edits++
		pf.NewBytes += len(e.New)
		last[e.Path] = i
	}
	out := make([]PatchedFile, 0, len(byPath))
	for _, pf := range byPath {
		out = append(out, *pf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	for _, pf := range out {
		if len(pf.Content) == 0 { // apply:empties
			return nil, refuse(RuleEditEmpties, "", last[pf.Path], pf.Path, "the edits leave the file empty; a file cannot be deleted or emptied by an edit")
		}
		if bytes.Equal(pf.Content, pf.Base) { // apply:noop
			return nil, refuse(RuleEditNoop, "", last[pf.Path], pf.Path, "the edits to this file cancel out; it would be committed unchanged")
		}
	}
	return out, nil
}

// occurrences counts the positions at which sub occurs in s, OVERLAPPING ones
// included and stopping at 2 (the caller only asks "zero, one, or more").
// Overlaps count because "aa" in "aaa" really is ambiguous: bytes.Count would
// say 1 and the edit would silently take the first.
func occurrences(s, sub []byte) int {
	n := 0
	for from := 0; n < 2; {
		i := bytes.Index(s[from:], sub)
		if i < 0 {
			break
		}
		n++
		from += i + 1 // count:overlap
	}
	return n
}

// DiffFacts are the facts of a fix's change, computed by the broker from the
// base blobs and the patched blobs -- never stated by the agent. They are what
// the size, secret and class predicates read (066 4.1 gate 6).
type DiffFacts struct {
	// Files is one entry per changed file, sorted by path.
	Files []FileDiff
	// FilesChanged is len(Files).
	FilesChanged int
	// LinesAdded and LinesRemoved are the totals of a line diff of each file.
	LinesAdded, LinesRemoved int
	// NewBytes is the total length of the `new` text of every edit in the
	// batch (the "patch bytes" cap), whether or not a later edit replaced it.
	NewBytes int
	// Edits is the number of edit effects applied.
	Edits int
}

// FileDiff is one changed file's facts.
type FileDiff struct {
	Path string
	// Class is the file's scope class under the dispatch's envelope. It is
	// EMPTY as ComputeDiffFacts returns it; PlanFix fills it in.
	Class scope.Class
	// Generated reports that the BASE content marks the file as generated
	// (scope.IsGenerated): a class no path pattern can tell.
	Generated bool
	Edits     int
	// BaseBytes and ResultBytes are the file's size before and after.
	BaseBytes, ResultBytes int
	// Added and Removed are len(AddedLines) and the count of removed lines.
	Added, Removed int
	// AddedLines are the lines the change adds, in order, each with its line
	// terminator. A line changed in place is one removed and one added line.
	//
	// THEY MAY HOLD A SECRET until SC-SECRET has passed: do not log or render
	// a DiffFacts of a batch that was refused.
	AddedLines []string `json:"-"`
	// Exact is false when the line diff gave up searching for a minimal diff
	// (more than maxExactDiffLines changed lines): Added and Removed are then
	// an UPPER bound -- the whole differing region -- never an under-count.
	Exact bool
}

// Lines is added + removed lines: what the `max_lines` cap bounds.
func (d DiffFacts) Lines() int { return d.LinesAdded + d.LinesRemoved }

// Classes returns the distinct classes of the changed files, in class order.
// It is empty until PlanFix has classified them.
func (d DiffFacts) Classes() []scope.Class {
	set := map[scope.Class]bool{}
	for _, f := range d.Files {
		set[f.Class] = true
	}
	out := []scope.Class{}
	for _, c := range scope.Classes() {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}

// ComputeDiffFacts computes the facts of the change files describes (the
// result of ApplyEdits). Pure.
func ComputeDiffFacts(files []PatchedFile) DiffFacts {
	d := DiffFacts{Files: make([]FileDiff, 0, len(files)), FilesChanged: len(files)}
	for _, pf := range files {
		removed, added, exact := lineDiff(splitLines(pf.Base), splitLines(pf.Content))
		d.Files = append(d.Files, FileDiff{
			Path: pf.Path, Generated: scope.IsGenerated(pf.Base), Edits: pf.Edits,
			BaseBytes: len(pf.Base), ResultBytes: len(pf.Content),
			Added: len(added), Removed: len(removed), AddedLines: added, Exact: exact,
		})
		d.LinesAdded += len(added)
		d.LinesRemoved += len(removed)
		d.NewBytes += pf.NewBytes
		d.Edits += pf.Edits
	}
	return d
}

// splitLines splits content into lines that KEEP their terminator, so a
// change to the final newline is a change to the last line, as git reports it.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(content), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// maxExactDiffLines bounds the search for a minimal line diff: past this many
// changed lines, lineDiff reports the whole differing region instead. It is
// far above any size cap (the pack ceiling is 40 lines), so the fallback only
// ever turns "much too large" into "at least as large".
const maxExactDiffLines = 1000

// lineDiff returns the lines of a minimal diff from a to b: those removed from
// a and those added in b, each in order (Myers' O(ND) algorithm, after
// trimming the common prefix and suffix). exact is false when more than
// maxExactDiffLines lines differ; the whole differing region is then returned.
func lineDiff(a, b []string) (removed, added []string, exact bool) {
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return a, b, true
	}
	limit := n + m
	if limit > maxExactDiffLines {
		limit = maxExactDiffLines
	}
	// v[k+limit+1] is the furthest x reached on diagonal k (x - y = k).
	v := make([]int32, 2*limit+3)
	off := limit + 1
	var trace [][]int32 // trace[d] = v as it was BEFORE round d, window [-d, d]
	for d := 0; d <= limit; d++ {
		trace = append(trace, append([]int32(nil), v[off-d:off+d+1]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = int(v[off+k+1]) // down: a line of b is added
			} else {
				x = int(v[off+k-1]) + 1 // right: a line of a is removed
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = int32(x)
			if x >= n && y >= m {
				return backtrack(a, b, trace, d)
			}
		}
	}
	return a, b, false
}

// backtrack walks the recorded rounds from the end to the start and collects
// the removed and added lines.
func backtrack(a, b []string, trace [][]int32, d int) (removed, added []string, exact bool) {
	x, y := len(a), len(b)
	for ; d > 0; d-- {
		prev := trace[d] // window [-d, d] of v before round d
		at := func(k int) int { return int(prev[k+d]) }
		k := x - y
		var pk int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(pk)
		py := px - pk
		if pk == k+1 {
			added = append(added, b[py]) // moved down from (px, py)
		} else {
			removed = append(removed, a[px]) // moved right from (px, py)
		}
		x, y = px, py
	}
	reverse(removed)
	reverse(added)
	return removed, added, true
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
