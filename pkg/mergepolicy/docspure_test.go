package mergepolicy

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/scope"
)

func docsPath(p string) ChangedPath {
	return ChangedPath{
		Decision: scope.Decision{Path: p, Class: scope.ClassDocs, Builtin: scope.ClassDocs, Profile: "docs-md"},
		OldMode:  FileModeRegular, NewMode: FileModeRegular,
	}
}

var allDocsProfiles = []string{"go", "docs-md", "docs-rst", "docs-txt", "docs-adoc"}

// One fixture per MP-DOCSPURE code, failing only that code.
func TestDocsPure(t *testing.T) {
	type tc struct {
		name     string
		profiles []string
		path     func() ChangedPath
		want     []DocsPureCode // nil: pure
	}
	mod := func(p string, f func(*ChangedPath)) func() ChangedPath {
		return func() ChangedPath {
			cp := docsPath(p)
			if f != nil {
				f(&cp)
			}
			return cp
		}
	}
	cases := []tc{
		// ---- pure
		{"an edited Markdown file", nil, mod("docs/guide.md", nil), nil},
		{"a Markdown file outside docs/", nil, mod("README.md", nil), nil},
		{"a created Markdown file (old mode 0)", nil, mod("docs/new.md", func(c *ChangedPath) { c.Created, c.OldMode = true, "0" }), nil},
		{"a created Markdown file (old mode 000000)", nil, mod("docs/new.md", func(c *ChangedPath) { c.Created, c.OldMode = true, "000000" }), nil},
		{"a deleted Markdown file", nil, mod("docs/old.md", func(c *ChangedPath) { c.Deleted, c.NewMode = true, "0" }), nil},
		{"a Markdown file renamed to another Markdown file", nil, mod("docs/b.md", func(c *ChangedPath) {
			c.Renamed, c.OldPath, c.OldClass = true, "docs/a.md", scope.ClassDocs
		}), nil},
		{"reStructuredText, with docs-rst selected", allDocsProfiles, mod("docs/index.rst", nil), nil},
		{"plain text, with docs-txt selected", allDocsProfiles, mod("docs/notes.txt", nil), nil},
		{"AsciiDoc, with docs-adoc selected", allDocsProfiles, mod("docs/manual.adoc", nil), nil},

		// ---- DP-EXT
		{"docs/tool.py", nil, mod("docs/tool.py", nil), []DocsPureCode{DPExt}},
		{"docs/build.sh", nil, mod("docs/build.sh", nil), []DocsPureCode{DPExt}},
		{"docs/Makefile (no extension)", nil, mod("docs/Makefile", nil), []DocsPureCode{DPExt}},
		{"CHANGELOG (no extension)", nil, mod("CHANGELOG", nil), []DocsPureCode{DPExt}},
		{"docs/mkdocs.yml", nil, mod("docs/mkdocs.yml", nil), []DocsPureCode{DPExt}},
		{"docs/diagram.svg", nil, mod("docs/diagram.svg", nil), []DocsPureCode{DPExt}},
		{"docs/page.html", nil, mod("docs/page.html", nil), []DocsPureCode{DPExt}},
		{"docs/page.mdx (Markdown that executes)", nil, mod("docs/page.mdx", nil), []DocsPureCode{DPExt}},
		{"an upper-case extension", nil, mod("docs/GUIDE.MD", nil), []DocsPureCode{DPExt}},
		{"a double extension ending in a script's", nil, mod("docs/guide.md.sh", nil), []DocsPureCode{DPExt}},
		{"a file named only .md", nil, mod("docs/.md", nil), []DocsPureCode{DPExt}},
		{"a trailing dot", nil, mod("docs/guide.md.", nil), []DocsPureCode{DPExt}},
		{"a .rst file when only docs-md is selected", nil, mod("docs/index.rst", nil), []DocsPureCode{DPExt}},
		{"a .txt file when only docs-md is selected", nil, mod("docs/notes.txt", nil), []DocsPureCode{DPExt}},
		{"a .md file when no docs profile is selected", []string{"go"}, mod("docs/guide.md", nil), []DocsPureCode{DPExt}},
		{"a .md file when NO profile is given", []string{}, mod("docs/guide.md", nil), []DocsPureCode{DPExt}},
		{".asciidoc is not on the list", allDocsProfiles, mod("docs/manual.asciidoc", nil), []DocsPureCode{DPExt}},

		// ---- DP-CLASS
		{"a Markdown file that is not class docs (a chart's README)", nil, mod("chart/README.md", func(c *ChangedPath) {
			c.Decision.Class, c.Decision.Builtin = scope.ClassDeploy, scope.ClassDeploy
		}), []DocsPureCode{DPClass}},
		{"a Markdown file with no class", nil, mod("docs/guide.md", func(c *ChangedPath) { c.Decision.Class = "" }), []DocsPureCode{DPClass}},

		// ---- DP-MODE
		{"an executable Markdown file", nil, mod("docs/run.md", func(c *ChangedPath) { c.NewMode = FileModeExecutable }), []DocsPureCode{DPMode}},
		{"a symlink named like Markdown", nil, mod("docs/link.md", func(c *ChangedPath) { c.NewMode = FileModeSymlink }), []DocsPureCode{DPMode}},
		{"a submodule named like Markdown", nil, mod("docs/sub.md", func(c *ChangedPath) { c.NewMode = FileModeSubmodule }), []DocsPureCode{DPMode}},
		{"the new mode was not read", nil, mod("docs/guide.md", func(c *ChangedPath) { c.NewMode = "" }), []DocsPureCode{DPMode}},
		{"a mode with the group-write bit", nil, mod("docs/guide.md", func(c *ChangedPath) { c.NewMode = "100664" }), []DocsPureCode{DPMode}},
		{"a deleted file that still has a mode", nil, mod("docs/old.md", func(c *ChangedPath) { c.Deleted = true }), []DocsPureCode{DPMode}},
		{"a file that is not deleted and has no new side", nil, mod("docs/guide.md", func(c *ChangedPath) { c.NewMode = "0" }), []DocsPureCode{DPMode}},

		// ---- DP-OLDMODE
		{"it WAS executable (the change drops the bit)", nil, mod("docs/run.md", func(c *ChangedPath) { c.OldMode = FileModeExecutable }), []DocsPureCode{DPOldMode}},
		{"it WAS a symlink", nil, mod("docs/link.md", func(c *ChangedPath) { c.OldMode = FileModeSymlink }), []DocsPureCode{DPOldMode}},
		{"the old mode was not read", nil, mod("docs/guide.md", func(c *ChangedPath) { c.OldMode = "" }), []DocsPureCode{DPOldMode}},
		{"a created file that had a mode", nil, mod("docs/new.md", func(c *ChangedPath) { c.Created = true }), []DocsPureCode{DPOldMode}},
		{"a file that is not created and had no old side", nil, mod("docs/guide.md", func(c *ChangedPath) { c.OldMode = "0" }), []DocsPureCode{DPOldMode}},
		{"deleting an executable", nil, mod("docs/old.md", func(c *ChangedPath) {
			c.Deleted, c.NewMode, c.OldMode = true, "0", FileModeExecutable
		}), []DocsPureCode{DPOldMode}},

		// ---- DP-RENAME
		{"docs/tool.py renamed to docs/tool.md", nil, mod("docs/tool.md", func(c *ChangedPath) {
			c.Renamed, c.OldPath, c.OldClass = true, "docs/tool.py", scope.ClassDocs
		}), []DocsPureCode{DPRename}},
		{"a source file renamed into docs", nil, mod("docs/count.md", func(c *ChangedPath) {
			c.Renamed, c.OldPath, c.OldClass = true, "tally/count.md", scope.ClassSource
		}), []DocsPureCode{DPRename}},
		{"a rename whose old path is unknown", nil, mod("docs/b.md", func(c *ChangedPath) { c.Renamed = true }), []DocsPureCode{DPRename}},
		{"a rename whose old class is unknown", nil, mod("docs/b.md", func(c *ChangedPath) { c.Renamed, c.OldPath = true, "docs/a.md" }), []DocsPureCode{DPRename}},
		{"an old path on a file that is not a rename", nil, mod("docs/b.md", func(c *ChangedPath) { c.OldPath, c.OldClass = "docs/a.md", scope.ClassDocs }), []DocsPureCode{DPRename}},

		// ---- DP-FORM
		{"created and deleted at once", nil, mod("docs/x.md", func(c *ChangedPath) {
			c.Created, c.Deleted, c.OldMode, c.NewMode = true, true, "0", "0"
		}), []DocsPureCode{DPForm}},
		{"created and renamed at once", nil, mod("docs/x.md", func(c *ChangedPath) {
			c.Created, c.Renamed, c.OldMode, c.OldPath, c.OldClass = true, true, "0", "docs/w.md", scope.ClassDocs
		}), []DocsPureCode{DPForm}},
		{"no path", nil, mod("", nil), []DocsPureCode{DPForm}},

		// ---- more than one thing wrong: every one is reported
		{"a new executable script", nil, mod("docs/tool.py", func(c *ChangedPath) {
			c.Created, c.OldMode, c.NewMode = true, "0", FileModeExecutable
		}), []DocsPureCode{DPExt, DPMode}},
	}
	seen := map[DocsPureCode]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profiles := c.profiles
			if profiles == nil {
				profiles = scope.DefaultProfiles()
			}
			cp := c.path()
			got := DocsPure([]ChangedPath{cp}, profiles)
			var codes []DocsPureCode
			for _, f := range got {
				codes = append(codes, f.Code)
				if f.Path != cp.Decision.Path || f.Detail == "" {
					t.Errorf("finding %+v: want path %q and a detail", f, cp.Decision.Path)
				}
			}
			if !reflect.DeepEqual(codes, c.want) {
				t.Errorf("DocsPure = %v, want %v", codes, c.want)
			}
			// one impure path among pure ones is still found
			if c.want != nil && len(DocsPure([]ChangedPath{docsPath("docs/a.md")}, profiles)) == 0 {
				mixed := DocsPure([]ChangedPath{docsPath("docs/a.md"), cp, docsPath("docs/z.md")}, profiles)
				if len(mixed) != len(c.want) {
					t.Errorf("among two pure paths: %d findings, want %d", len(mixed), len(c.want))
				}
			}
		})
		for _, code := range c.want {
			if len(c.want) == 1 {
				seen[code] = true
			}
		}
	}
	for _, code := range DocsPureCodes() {
		if code != DPEmpty && !seen[code] {
			t.Errorf("no fixture fails %s alone", code)
		}
	}
}

// "Every changed path" over no path is not a yes.
func TestDocsPureOfNothingIsNotPure(t *testing.T) {
	for _, paths := range [][]ChangedPath{nil, {}} {
		got := DocsPure(paths, scope.DefaultProfiles())
		if len(got) != 1 || got[0].Code != DPEmpty {
			t.Errorf("DocsPure(no paths) = %+v, want one %s", got, DPEmpty)
		}
	}
}

// The allow-list, pinned to pkg/scope's shipped documentation profiles: every
// one of them has an entry (a new docs profile forces a decision here), every
// entry is a shipped docs profile, and every extension is one that profile
// classifies as docs.
func TestProseExtensionsFollowTheShippedDocsProfiles(t *testing.T) {
	want := map[string][]string{
		"docs-md":   {".md"},
		"docs-rst":  {".rst"},
		"docs-txt":  {".txt"},
		"docs-adoc": {".adoc"},
	}
	if got := ProseProfiles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProseProfiles() = %v\nwant %v (plan 5.2: .md, .rst, .txt, .adoc)", got, want)
	}
	for _, pr := range scope.Profiles() {
		exts, ok := want[pr.Name]
		if pr.Kind == scope.ProfileDocs && !ok {
			t.Errorf("shipped docs profile %q has no prose extensions: decide what its prose is", pr.Name)
		}
		if pr.Kind != scope.ProfileDocs && ok {
			t.Errorf("%q has prose extensions and is not a docs profile", pr.Name)
		}
		for _, ext := range exts {
			found := false
			for _, row := range pr.Rows {
				for _, pat := range row.Patterns {
					found = found || (row.Class == scope.ClassDocs && pat == "**/*"+ext)
				}
			}
			if !found {
				t.Errorf("profile %q does not classify **/*%s as docs", pr.Name, ext)
			}
		}
	}
	for name := range want {
		if _, ok := scope.LookupProfile(name); !ok {
			t.Errorf("%q is not a shipped profile", name)
		}
	}
	for _, tc := range []struct {
		profiles []string
		want     []string
	}{
		{nil, nil},
		{[]string{"go"}, nil},
		{scope.DefaultProfiles(), []string{".md"}},
		{[]string{"docs-rst", "python"}, []string{".rst"}},
		{allDocsProfiles, []string{".adoc", ".md", ".rst", ".txt"}},
		{[]string{"docs-md", "docs-md"}, []string{".md"}},
		{[]string{"docs-nonesuch", "DOCS-MD"}, nil},
	} {
		got := ProseExtensions(tc.profiles)
		sort.Strings(got)
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ProseExtensions(%v) = %v, want %v", tc.profiles, got, tc.want)
		}
	}
	// ProseProfiles hands out a copy.
	ProseProfiles()["docs-md"][0] = ".py"
	ProseProfiles()["docs-evil"] = []string{".sh"}
	if got := ProseExtensions([]string{"docs-md", "docs-evil"}); !reflect.DeepEqual(got, []string{".md"}) {
		t.Errorf("a caller changed the allow-list: %v", got)
	}
}

// Every name the doc comment of DocsPure promises is in the source it documents.
func TestTheAllowListIsDocumented(t *testing.T) {
	src := readFile(t, "docspure.go")
	for _, ext := range []string{"`.md`", "`.rst`", "`.txt`", "`.adoc`"} {
		if !strings.Contains(src, ext) {
			t.Errorf("docspure.go's comments do not name %s", ext)
		}
	}
}
