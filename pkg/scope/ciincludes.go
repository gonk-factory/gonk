package scope

import (
	"fmt"
	"strings"
)

// CIIncludes is the set of in-repository files a pipeline definition
// includes, resolved from the root file through every literal local include
// (gonk-9q0y).
type CIIncludes struct {
	// Paths: every `include: local:` entry reachable from the root file, the
	// root's own first and in its order, then each included file's, breadth
	// first, each once. An entry is a path, or -- GitLab allows `*` and `**`
	// -- a pattern (CIIncludeIsPattern), or a path with a `$` variable in it,
	// as written (CIIncludeHasVariable).
	Paths []string
	// Complete: every entry the walk saw was one literal path, so Paths names
	// every local file of the definition. Three things make it false:
	//
	//   - a pattern: it is not expanded (that needs the tree) and the files it
	//     matches are not read, so THEIR includes are not in Paths;
	//   - an entry with a variable: which file it names is decided when the
	//     pipeline is created, from values this code does not have. It is not
	//     read, and unlike a pattern it does not make any file class `ci`;
	//   - an include under a custom YAML tag (`include: !reference [...]`):
	//     it is not a list of entries and is not read as one.
	//
	// A caller that needs the whole definition must then not rely on Paths
	// alone (the apply's "definition unchanged" shortcut does not).
	Complete bool
}

// MaxCIIncludeFiles bounds the walk by the number of files read. It is
// GitLab's own default limit on the includes of one pipeline (150). One more
// is an error, never a silently shorter set.
const MaxCIIncludeFiles = 150

// CIIncludeIsPattern reports whether a local include entry is a wildcard
// pattern and not one file's path.
func CIIncludeIsPattern(entry string) bool { return strings.ContainsAny(entry, "*?") }

// CIIncludeHasVariable reports whether a local include entry contains a `$`.
// GitLab expands CI/CD variables (`$VAR`, `${VAR}`) in include paths when it
// creates the pipeline, so such an entry is not the path of one known file.
// Every `$` counts: treating a literal dollar sign as a variable costs only
// the shortcut, and the other way round would claim a completeness that is
// not there.
func CIIncludeHasVariable(entry string) bool { return strings.Contains(entry, "$") }

// ResolveCILocalIncludes follows `include: local:` from the root pipeline
// definition through the files it includes, and the files THOSE include.
// A local include is rooted at the repository whichever file states it, so a
// nested entry is read as written (CILocalIncludes strips a leading slash).
//
// read returns one file's content at the commit the caller pinned; found is
// false when the file does not exist there. A missing file is still named in
// Paths (the definition names it; GitLab rejects such a definition) and has
// nothing to follow. Any other read failure, a file that is not YAML, an
// include that is not a clean in-repository pattern, and a walk past
// MaxCIIncludeFiles are errors: the definition cannot be understood, and the
// caller must not guess which files are CI. On an error the result is empty.
//
// Until 2026-10-12 only the root file was read, so a file included by an
// included file kept its built-in class and was outside every comparison.
func ResolveCILocalIncludes(root []byte, read func(path string) (content []byte, found bool, err error)) (CIIncludes, error) {
	first, opaque, err := ciLocalIncludes(root)
	if err != nil {
		return CIIncludes{}, err
	}
	out := CIIncludes{Complete: !opaque} // includes:opaque-root
	seen := map[string]bool{}
	var queue []string
	add := func(entries []string) {
		for _, e := range entries {
			if seen[e] { // includes:once
				continue
			}
			seen[e] = true
			out.Paths = append(out.Paths, e)
			if CIIncludeIsPattern(e) { // includes:pattern
				out.Complete = false
				continue
			}
			if CIIncludeHasVariable(e) { // includes:variable
				out.Complete = false
				continue
			}
			queue = append(queue, e)
		}
	}
	add(first)
	for files := 0; len(queue) > 0; {
		p := queue[0]
		queue = queue[1:]
		if files++; files > MaxCIIncludeFiles { // includes:bound
			return CIIncludes{}, fmt.Errorf("scope: the pipeline definition includes more than %d local files", MaxCIIncludeFiles)
		}
		content, found, rerr := read(p)
		if rerr != nil {
			return CIIncludes{}, fmt.Errorf("scope: read the included file %q: %w", p, rerr)
		}
		if !found { // includes:missing
			continue
		}
		nested, nopaque, nerr := ciLocalIncludes(content)
		if nerr != nil {
			return CIIncludes{}, fmt.Errorf("scope: the included file %q: %w", p, nerr)
		}
		if nopaque { // includes:opaque-nested
			out.Complete = false
		}
		add(nested)
	}
	return out, nil
}
