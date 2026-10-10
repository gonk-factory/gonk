package mergepolicy

import (
	"fmt"
	"path"
	"strings"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// proseProfiles is THE PROSE ALLOW-LIST of MP-DOCSPURE: for each shipped
// documentation profile of pkg/scope, the file extensions that are prose.
//
//	docs-md    `.md`
//	docs-rst   `.rst`
//	docs-txt   `.txt`
//	docs-adoc  `.adoc`
//
// These are epic plan 5.2's four, and nothing else is prose to the merge
// policy. In particular NOT on the list, although pkg/scope calls every one of
// them class `docs`: anything else under `docs/` (`docs/tool.py`,
// `docs/build.sh`, `docs/Makefile`, `mkdocs.yml`, images, HTML, `.mdx`);
// extensionless files (`CHANGELOG`, `LICENSE`); `.asciidoc`, which docs-adoc
// matches but the plan does not list; and any other spelling of a listed
// extension (`.MD`). The match is exact and case-sensitive.
//
// An extension counts only when its profile is SELECTED for the project: a
// `.rst` file in a project that selected docs-md alone is not prose here.
//
// A test pins this map to scope.Profiles(): a documentation profile shipped
// later has no prose until someone decides what its prose is.
var proseProfiles = map[string][]string{
	"docs-md":   {".md"},
	"docs-rst":  {".rst"},
	"docs-txt":  {".txt"},
	"docs-adoc": {".adoc"},
}

// ProseProfiles returns a copy of the allow-list: documentation profile ->
// prose extensions.
func ProseProfiles() map[string][]string {
	out := make(map[string][]string, len(proseProfiles))
	for name, exts := range proseProfiles {
		out[name] = append([]string(nil), exts...)
	}
	return out
}

// ProseExtensions returns the prose extensions of the selected profiles, in
// the order the profiles are given, each once. No selected documentation
// profile means no extension, and then nothing is prose.
func ProseExtensions(profiles []string) []string {
	var out []string
	for _, name := range profiles {
		for _, ext := range proseProfiles[name] {
			if !containsString(out, ext) {
				out = append(out, ext)
			}
		}
	}
	return out
}

// DocsPureCode names one way a changed path fails MP-DOCSPURE.
type DocsPureCode string

const (
	// DPEmpty: there is no changed path. "Every path is prose" over nothing
	// is not a yes.
	DPEmpty DocsPureCode = "DP-EMPTY"
	// DPForm: the entry contradicts itself (created and deleted, created and
	// renamed) or has no path.
	DPForm DocsPureCode = "DP-FORM"
	// DPClass: the path's class is not `docs`.
	DPClass DocsPureCode = "DP-CLASS"
	// DPExt: the path's extension is not a prose extension of a selected
	// documentation profile.
	DPExt DocsPureCode = "DP-EXT"
	// DPMode: the file is not a regular, non-executable file after the change
	// (executable, symlink, submodule, unread), or a deleted file still has a
	// mode.
	DPMode DocsPureCode = "DP-MODE"
	// DPOldMode: the file was not a regular, non-executable file before the
	// change -- so the change removes or replaces something that was not
	// prose -- or a created file had a mode.
	DPOldMode DocsPureCode = "DP-OLDMODE"
	// DPRename: a renamed file's old path is unknown, was not class `docs`,
	// or did not have a prose extension (`docs/tool.py` -> `docs/tool.md`
	// removes a script).
	DPRename DocsPureCode = "DP-RENAME"
)

// DocsPureCodes returns every code, in the order DocsPure reports them.
func DocsPureCodes() []DocsPureCode {
	return []DocsPureCode{DPEmpty, DPForm, DPClass, DPExt, DPMode, DPOldMode, DPRename}
}

// DocsPureFinding is one reason one path is not prose.
type DocsPureFinding struct {
	Path   string
	Code   DocsPureCode
	Detail string
}

// DocsPure is MP-DOCSPURE (epic plan 5.2, owner decision 8.1 Q4 i): is this
// diff PROSE FILES ONLY? It returns nothing when it is, and every reason it is
// not otherwise.
//
// pkg/scope's class `docs` is not that question: under docs-md everything
// below `docs/` is class `docs`, including a script the documentation build
// runs (epic plan S7). A changed path is prose only when ALL of these hold:
//
//   - its class is `docs`;
//   - its extension is on the allow-list (proseProfiles: `.md`, `.rst`,
//     `.txt`, `.adoc`) for a documentation profile the project SELECTED;
//   - after the change it is a regular, non-executable file (mode 100644) --
//     or does not exist, when the change deletes it;
//   - before the change it was a regular, non-executable file -- or did not
//     exist, when the change creates it;
//   - if it was renamed, the old path was class `docs` with an allowed
//     extension too.
//
// A mode that was not read ("") is not a regular file. The predicate looks at
// names and modes only: it cannot tell prose from a script saved as `.md`, and
// does not need to, because nothing executes a 100644 `.md` file by its name.
func DocsPure(paths []ChangedPath, profiles []string) []DocsPureFinding {
	if len(paths) == 0 {
		return []DocsPureFinding{{Code: DPEmpty, Detail: "the diff has no changed path"}}
	}
	exts := ProseExtensions(profiles)
	var out []DocsPureFinding
	for _, p := range paths {
		name := p.Decision.Path
		add := func(code DocsPureCode, format string, args ...any) {
			out = append(out, DocsPureFinding{Path: name, Code: code, Detail: fmt.Sprintf(format, args...)})
		}
		if name == "" {
			add(DPForm, "an entry of the diff has no path")
			continue
		}
		if p.Created && (p.Deleted || p.Renamed) {
			add(DPForm, "the entry says the file was created and also deleted or renamed")
			continue
		}
		if p.Decision.Class != scope.ClassDocs {
			add(DPClass, "class %q, not docs", p.Decision.Class)
		}
		if !proseName(name, exts) {
			add(DPExt, "the extension %q is not a prose extension of the selected documentation profiles %v", path.Ext(name), exts)
		}
		switch {
		case p.Deleted && !absentMode(p.NewMode):
			add(DPMode, "the file is deleted and has mode %q after the change", p.NewMode)
		case !p.Deleted && p.NewMode != FileModeRegular:
			add(DPMode, "mode %q after the change, not a regular non-executable file (%s)", p.NewMode, FileModeRegular)
		}
		switch {
		case p.Created && !absentMode(p.OldMode):
			add(DPOldMode, "the file is created and had mode %q before the change", p.OldMode)
		case !p.Created && p.OldMode != FileModeRegular:
			add(DPOldMode, "mode %q before the change, not a regular non-executable file (%s)", p.OldMode, FileModeRegular)
		}
		switch {
		case p.Renamed && (p.OldClass != scope.ClassDocs || !proseName(p.OldPath, exts)):
			add(DPRename, "renamed from %q (class %q), which was not a prose file", p.OldPath, p.OldClass)
		case !p.Renamed && (p.OldPath != "" || p.OldClass != ""):
			add(DPRename, "the entry has an old path %q and is not marked renamed", p.OldPath)
		}
	}
	return out
}

// proseName reports whether the file name has an allowed extension and
// something before it.
func proseName(name string, exts []string) bool {
	base := path.Base(name)
	ext := path.Ext(base)
	return ext != base && containsString(exts, ext)
}

// absentMode reports whether the mode says "no file on this side".
func absentMode(mode string) bool { return mode == "0" || mode == "000000" }

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func docsPureText(findings []DocsPureFinding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("%s `%s`: %s", f.Code, f.Path, f.Detail))
	}
	return strings.Join(parts, "; ")
}
