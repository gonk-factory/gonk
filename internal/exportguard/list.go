package exportguard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// SelfDir is the directory that holds the export's own data: the
// internal-only list, the deny data (which spells the internal names) and the
// allow-list. It must be on every internal-only list -- the tool that removes
// internal paths may never be told to keep the file that names them.
const SelfDir = "release/"

// The data files, relative to the repository root.
const (
	ListPath  = SelfDir + "internal-only.txt"
	DenyPath  = SelfDir + "deny-internal.txt"
	AllowPath = SelfDir + "allow.txt"
)

// ExportSourcePath is the one generated file in the public tree.
const ExportSourcePath = "EXPORT-SOURCE"

// List is the internal-only path list.
type List struct {
	Entries []string // in file order
	SHA256  string   // of the file's bytes, hex
}

// ParseList reads the internal-only list. One entry per line:
//
//	ENTRY  # reason
//
// ENTRY is a literal file path, or a directory prefix ending in "/". There
// are no globs. Every entry needs a reason. Lines beginning with "#" and
// blank lines are ignored.
//
// Refused: a glob character, an absolute or dotted path, a duplicate, an
// entry nested under another entry, a missing reason, and a list that does
// not contain SelfDir.
func ParseList(data []byte) (*List, error) {
	sum := sha256.Sum256(data)
	l := &List{SHA256: hex.EncodeToString(sum[:])}
	seen := map[string]bool{}
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, reason, found := strings.Cut(line, "#")
		entry = strings.TrimSpace(entry)
		if !found || strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("internal-only list line %d: entry %q has no reason comment", n+1, entry)
		}
		if err := validEntry(entry); err != nil {
			return nil, fmt.Errorf("internal-only list line %d: %v", n+1, err)
		}
		if seen[entry] {
			return nil, fmt.Errorf("internal-only list line %d: %q is listed twice", n+1, entry)
		}
		seen[entry] = true
		l.Entries = append(l.Entries, entry)
	}
	for _, a := range l.Entries {
		for _, b := range l.Entries {
			if a != b && strings.HasSuffix(b, "/") && strings.HasPrefix(a, b) {
				return nil, fmt.Errorf("internal-only list: %q is already covered by %q", a, b)
			}
		}
	}
	if !seen[SelfDir] {
		return nil, fmt.Errorf("internal-only list: %q must be listed (it holds the deny data, which spells the internal names)", SelfDir)
	}
	return l, nil
}

func validEntry(e string) error {
	switch {
	case e == "":
		return fmt.Errorf("empty entry")
	case strings.ContainsAny(e, "*?[]\\"):
		return fmt.Errorf("%q: globs are not allowed; list a file or a directory ending in /", e)
	case strings.ContainsAny(e, " \t"):
		return fmt.Errorf("%q: an entry may not contain blanks", e)
	case strings.HasPrefix(e, "/"), e == "/":
		return fmt.Errorf("%q: entries are relative to the repository root", e)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(e, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%q: not a clean relative path", e)
		}
	}
	return nil
}

// covers reports whether one entry removes path.
func covers(entry, path string) bool {
	if strings.HasSuffix(entry, "/") {
		return strings.HasPrefix(path, entry)
	}
	return path == entry
}

// Covers reports whether path is internal-only.
func (l *List) Covers(path string) bool {
	for _, e := range l.Entries {
		if covers(e, path) {
			return true
		}
	}
	return false
}

// Apply splits paths into those that stay public and a per-entry count of
// those removed. An entry that removes nothing is an error: the list may not
// rot. A file entry that names a directory (or the reverse) removes nothing
// and is caught the same way.
func (l *List) Apply(paths []string) (kept []string, removed map[string]int, err error) {
	removed = map[string]int{}
	for _, e := range l.Entries {
		removed[e] = 0
	}
	for _, p := range paths {
		hit := false
		for _, e := range l.Entries {
			if covers(e, p) {
				removed[e]++
				hit = true
			}
		}
		if !hit {
			kept = append(kept, p)
		}
	}
	var stale []string
	for _, e := range l.Entries {
		if removed[e] == 0 {
			stale = append(stale, e)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		return nil, nil, fmt.Errorf("internal-only list: stale entries (they match no file): %s", strings.Join(stale, ", "))
	}
	return kept, removed, nil
}

// HashedTrees are directories whose content is sealed by a hash that is
// itself published (the chart seal). Removing a file from one at export time
// would publish a tree that does not match its own seal.
var HashedTrees = []string{"chart/gonk/"}

// CheckProtected refuses a list entry that overlaps a protected path: a
// hashed tree, or anything an image build copies. protected holds file paths
// and directory prefixes (ending in "/"). Overlap in either direction is
// refused -- an entry inside a protected directory, and an entry that
// swallows one.
func (l *List) CheckProtected(protected []string) error {
	for _, e := range l.Entries {
		for _, p := range protected {
			if overlaps(e, p) {
				return fmt.Errorf("internal-only list: %q overlaps %q, which is hashed or copied into an image and must be exported whole", e, p)
			}
		}
	}
	return nil
}

func overlaps(a, b string) bool {
	return covers(a, strings.TrimSuffix(b, "/")) || covers(b, strings.TrimSuffix(a, "/")) ||
		covers(a, b) || covers(b, a)
}

// DockerfileCopySources returns the build-context sources of every COPY and
// ADD instruction in a Dockerfile, skipping copies from another stage
// (--from=). A source that is a directory in the tree is returned as written;
// the caller decides file or directory by looking at the tree.
func DockerfileCopySources(dockerfile []byte) []string {
	var out []string
	text := strings.ReplaceAll(string(dockerfile), "\\\n", " ")
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if op := strings.ToUpper(f[0]); op != "COPY" && op != "ADD" {
			continue
		}
		args := f[1:]
		fromStage := false
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			if strings.HasPrefix(args[0], "--from=") {
				fromStage = true
			}
			args = args[1:]
		}
		if fromStage || len(args) < 2 {
			continue
		}
		for _, src := range args[:len(args)-1] {
			src = strings.TrimPrefix(strings.Trim(src, `"[],`), "./")
			if src != "" && src != "." {
				out = append(out, src)
			}
		}
	}
	return out
}
