package diffmap

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func mustBuild(t *testing.T, diffs ...FileDiff) *Map {
	t.Helper()
	m, err := Build(diffs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if m == nil {
		t.Fatal("Build returned a nil map and no error")
	}
	return m
}

func fileOf(t *testing.T, m *Map, newPath string) File {
	t.Helper()
	for _, f := range m.Files() {
		if f.NewPath == newPath {
			return f
		}
	}
	t.Fatalf("no file %q in the map", newPath)
	return File{}
}

func TestParseHunkHeader(t *testing.T) {
	cases := []struct {
		in   string
		want HunkHeader
		ok   bool
	}{
		{"@@ -3,7 +3,8 @@ package stock", HunkHeader{3, 7, 3, 8, "package stock"}, true},
		{"@@ -3,7 +3,8 @@", HunkHeader{3, 7, 3, 8, ""}, true},
		// rule:omitted-count -- an omitted count is ONE line, not zero.
		{"@@ -1 +1 @@", HunkHeader{1, 1, 1, 1, ""}, true},
		{"@@ -5 +5,2 @@", HunkHeader{5, 1, 5, 2, ""}, true},
		{"@@ -5,2 +5 @@ x", HunkHeader{5, 2, 5, 1, "x"}, true},
		{"@@ -0,0 +1,3 @@", HunkHeader{0, 0, 1, 3, ""}, true},
		{"@@ -1,2 +0,0 @@", HunkHeader{1, 2, 0, 0, ""}, true},
		{"@@ -12,0 +13,2 @@", HunkHeader{12, 0, 13, 2, ""}, true},
		{"@@ -3,7 +3,8 @@ @@ -9,9 +9,9 @@", HunkHeader{3, 7, 3, 8, "@@ -9,9 +9,9 @@"}, true},
		{"", HunkHeader{}, false},
		{"@@", HunkHeader{}, false},
		{"@@ -3,7 +3,8", HunkHeader{}, false},
		{"@@ -3,7 +3,8 @", HunkHeader{}, false},
		{"@@ +3,7 -3,8 @@", HunkHeader{}, false},
		{"@@ -3,7 3,8 @@", HunkHeader{}, false},
		{"@@ -3, +3,8 @@", HunkHeader{}, false},
		{"@@ -,7 +3,8 @@", HunkHeader{}, false},
		{"@@ --3,7 +3,8 @@", HunkHeader{}, false},
		{"@@ -3,7 ++3,8 @@", HunkHeader{}, false},
		{"@@ -3,7,1 +3,8 @@", HunkHeader{}, false},
		{"@@ -3,7  +3,8 @@", HunkHeader{}, false},
		{" @@ -3,7 +3,8 @@", HunkHeader{}, false},
		{"@@@ -3,7 -3,7 +3,8 @@@", HunkHeader{}, false},
		{"@@ -99999999999999999999,1 +1,1 @@", HunkHeader{}, false},
		{"@@ -1,99999999999 +1,1 @@", HunkHeader{}, false},
		// rule:number-range -- ten digits, and still too large.
		{"@@ -4294967296,1 +1,1 @@", HunkHeader{}, false},
		{"@@ -1,2147483648 +1 @@", HunkHeader{}, false},
		{"@@ -1 +2147483648 @@", HunkHeader{}, false},
		{"@@ -2147483647 +2147483647 @@", HunkHeader{2147483647, 1, 2147483647, 1, ""}, true},
		{"@@ -1,1 +1,1 @@x", HunkHeader{}, false},
		{"@@ -1,1 +1,1 @@@", HunkHeader{}, false},
		{"@@ -\u0663,1 +1,1 @@", HunkHeader{}, false},
	}
	for _, c := range cases {
		got, err := ParseHunkHeader(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ParseHunkHeader(%q): err %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("ParseHunkHeader(%q) = %+v, want %+v", c.in, got, c.want)
		}
		if !c.ok && got != (HunkHeader{}) {
			t.Errorf("ParseHunkHeader(%q) refused but returned %+v", c.in, got)
		}
	}
}

// Every fixture file: its status, its counts, and the full line map written
// out as "<old>|<new>|<op>|<text>" so a wrong number on either side is a diff
// in this table and nowhere else.
func TestBuildLineMaps(t *testing.T) {
	cases := []struct {
		name    string
		fd      FileDiff
		status  Status
		added   int
		removed int
		lines   []string
	}{
		{"two hunks", fdCount, StatusText, 3, 2, []string{
			"3|3|context|import \"errors\"",
			"4|4|context|",
			"5|5|context|// Count returns how many crates fit on a shelf.",
			"6|0|removed|func Count(width int) int {",
			"0|6|added|func Count(width, depth int) int {",
			"0|7|added|\t// depth is measured in crates, not metres",
			"7|8|context|\tif width < 0 {",
			"8|9|context|\t\treturn 0",
			"9|10|context|\t}",
			"--",
			"20|21|context|\tif n == 0 {",
			"21|22|context|\t\treturn \"empty\"",
			"22|23|context|\t}",
			"23|0|removed|\treturn \"full\"",
			"0|24|added|\treturn \"stocked\"",
		}},
		{"rename with a change", fdRename, StatusText, 1, 1, []string{
			"1|1|context|# Shelves",
			"2|0|removed|Each shelf holds four crates.",
			"0|2|added|Each shelf holds six crates.",
			"3|3|context|See the stock package.",
		}},
		{"new file", fdNew, StatusText, 3, 0, []string{
			"0|1|added|package stock",
			"0|2|added|",
			"0|3|added|const DefaultDepth = 2",
		}},
		{"deleted file, no newline at its end", fdDeleted, StatusText, 0, 2, []string{
			"1|0|removed|old shelf notes",
			"2|0|removed|do not use\\",
		}},
		{"omitted counts, no newline on both sides", fdVersion, StatusText, 1, 1, []string{
			"1|0|removed|1.4.0\\",
			"0|1|added|1.5.0\\",
		}},
		{"CRLF", fdCRLF, StatusText, 1, 1, []string{
			"1|1|context|echo off",
			"2|0|removed|set SHELVES=4",
			"0|2|added|set SHELVES=6",
		}},
		{"file header lines before the first hunk", fdPreamble, StatusText, 1, 0, []string{
			"4|4|context|\tgo build ./...",
			"0|5|added|\tgo vet ./...",
			"5|6|context|.PHONY: all",
		}},
		{"the largest line number there is", FileDiff{OldPath: "huge.txt", NewPath: "huge.txt", Diff: "@@ -2147483646,2 +2147483646,2 @@\n a\n-b\n+c\n"}, StatusText, 1, 1, []string{
			"2147483646|2147483646|context|a",
			"2147483647|0|removed|b",
			"0|2147483647|added|c",
		}},
		{"pure rename", fdMoved, StatusNoLines, 0, 0, nil},
		{"binary", fdBinary, StatusBinary, 0, 0, nil},
		{"too large", fdTooLarge, StatusTooLarge, 0, 0, nil},
		{"collapsed, even with text", fdCollapse, StatusCollapsed, 0, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := mustBuild(t, c.fd)
			f := fileOf(t, m, c.fd.NewPath)
			if f.Status != c.status {
				t.Fatalf("status %q (%s), want %q", f.Status, f.Detail, c.status)
			}
			if f.Added != c.added || f.Removed != c.removed {
				t.Errorf("added %d removed %d, want %d %d", f.Added, f.Removed, c.added, c.removed)
			}
			if f.OldPath != c.fd.OldPath || f.New != c.fd.NewFile || f.Renamed != c.fd.RenamedFile || f.Deleted != c.fd.DeletedFile {
				t.Errorf("file facts not carried: %+v", f)
			}
			var got []string
			for i, h := range f.Hunks {
				if i > 0 {
					got = append(got, "--")
				}
				for _, l := range h.Lines {
					s := fmt.Sprintf("%d|%d|%s|%s", l.Old, l.New, l.Op, l.Text)
					if l.NoEOL {
						s += "\\"
					}
					got = append(got, s)
				}
			}
			if strings.Join(got, "\n") != strings.Join(c.lines, "\n") {
				t.Errorf("line map:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.lines, "\n"))
			}
			if c.status != StatusText && len(f.Hunks) != 0 {
				t.Errorf("a %s file carries %d hunks", c.status, len(f.Hunks))
			}
		})
	}
}

func TestStatusReviewable(t *testing.T) {
	want := map[Status]bool{
		StatusText: true, StatusNoLines: true,
		StatusTooLarge: false, StatusCollapsed: false, StatusBinary: false,
		StatusNoText: false, StatusMalformed: false, StatusEmpty: false,
		"": false, "TEXT": false,
	}
	for s, w := range want {
		if s.Reviewable() != w {
			t.Errorf("Status(%q).Reviewable() = %v", s, !w)
		}
	}
}

// A diff gonk cannot read line by line is NOT reviewable, and says why. It is
// never a map with no lines that reads as "nothing to object to".
func TestNotReviewable(t *testing.T) {
	bad := func(diff string) FileDiff { return FileDiff{OldPath: "a.txt", NewPath: "a.txt", Diff: diff} }
	cases := []struct {
		name   string
		diffs  []FileDiff
		path   string
		status Status
		detail string // substring of Reason.Detail
	}{
		{"no files at all", nil, "", StatusEmpty, ""},
		{"only a pure rename: nothing to cite", []FileDiff{fdMoved}, "", StatusEmpty, ""},
		{"too large", []FileDiff{fdCount, fdTooLarge}, "data/crates.csv", StatusTooLarge, ""},
		{"too large although text came with it", []FileDiff{{OldPath: "a.txt", NewPath: "a.txt", TooLarge: true, Diff: diffRename}}, "a.txt", StatusTooLarge, ""},
		{"collapsed", []FileDiff{fdCount, fdCollapse}, "data/shelves.csv", StatusCollapsed, ""},
		{"binary", []FileDiff{fdCount, fdBinary}, "img/shelf.png", StatusBinary, ""},
		{"binary, git patch form", []FileDiff{bad("index 1a2b3c4..5d6e7f8 100644\nGIT binary patch\nliteral 12\nabc\n")}, "a.txt", StatusBinary, ""},
		{"empty diff of a new file (binary? empty? gonk cannot tell)", []FileDiff{fdCount, {OldPath: "x.bin", NewPath: "x.bin", NewFile: true}}, "x.bin", StatusNoText, ""},
		{"empty diff of a modified file (mode change?)", []FileDiff{fdCount, {OldPath: "run.sh", NewPath: "run.sh"}}, "run.sh", StatusNoText, ""},
		{"header lines and no hunk", []FileDiff{bad("old mode 100644\nnew mode 100755\n")}, "a.txt", StatusNoText, ""},
		{"text before the first hunk", []FileDiff{bad("hello\n@@ -1 +1 @@\n-a\n+b\n")}, "a.txt", StatusMalformed, "before the first hunk"},
		{"bad hunk header", []FileDiff{bad("@@ -1 +1\n-a\n+b\n")}, "a.txt", StatusMalformed, "hunk header"},
		{"hunk shorter than its counts", []FileDiff{bad("@@ -1,2 +1,2 @@\n a\n-b\n")}, "a.txt", StatusMalformed, "ends early"},
		{"hunk longer than its counts", []FileDiff{bad("@@ -1,1 +1,1 @@\n-a\n+b\n+c\n")}, "a.txt", StatusMalformed, "hunk header"},
		{"more removed lines than the old count", []FileDiff{bad("@@ -1,1 +1,2 @@\n-a\n-b\n+c\n")}, "a.txt", StatusMalformed, "old side"},
		{"more added lines than the new count", []FileDiff{bad("@@ -1,2 +1,1 @@\n+a\n+b\n-c\n")}, "a.txt", StatusMalformed, "new side"},
		{"a context line past the old count", []FileDiff{bad("@@ -1,1 +1,3 @@\n-a\n+b\n c\n+d\n")}, "a.txt", StatusMalformed, "old side"},
		{"unknown line prefix", []FileDiff{bad("@@ -1,1 +1,1 @@\n*a\n+b\n")}, "a.txt", StatusMalformed, "prefix"},
		{"no-newline marker with no line before it", []FileDiff{bad("@@ -1,1 +1,1 @@\n\\ No newline at end of file\n-a\n+b\n")}, "a.txt", StatusMalformed, "no line before"},
		{"empty hunk", []FileDiff{bad("@@ -0,0 +0,0 @@\n")}, "a.txt", StatusMalformed, "empty hunk"},
		{"line zero on the old side", []FileDiff{bad("@@ -0,1 +1,1 @@\n-a\n+b\n")}, "a.txt", StatusMalformed, "line 0"},
		{"line zero on the new side", []FileDiff{bad("@@ -1,1 +0,1 @@\n-a\n+b\n")}, "a.txt", StatusMalformed, "line 0"},
		{"hunks overlap on the new side", []FileDiff{bad("@@ -1,1 +1,2 @@\n a\n+b\n@@ -5,1 +2,1 @@\n-c\n+d\n")}, "a.txt", StatusMalformed, "overlap"},
		{"hunks overlap on the old side", []FileDiff{bad("@@ -4,2 +4,1 @@\n a\n-b\n@@ -5,1 +9,1 @@\n-c\n+d\n")}, "a.txt", StatusMalformed, "overlap"},
		{"hunks out of order", []FileDiff{bad("@@ -9,1 +9,1 @@\n-a\n+b\n@@ -2,1 +2,1 @@\n-c\n+d\n")}, "a.txt", StatusMalformed, "overlap"},
		{"a hunk that runs past the largest line number, old side", []FileDiff{bad("@@ -2147483647,2 +1,2 @@\n a\n b\n")}, "a.txt", StatusMalformed, "runs past"},
		{"a hunk that runs past the largest line number, new side", []FileDiff{bad("@@ -1,2 +2147483647,2 @@\n a\n b\n")}, "a.txt", StatusMalformed, "runs past"},
		{"a new file with old-side lines", []FileDiff{{OldPath: "a.txt", NewPath: "a.txt", NewFile: true, Diff: diffRename}}, "a.txt", StatusMalformed, "new file"},
		{"a deleted file with new-side lines", []FileDiff{{OldPath: "a.txt", NewPath: "a.txt", DeletedFile: true, Diff: diffRename}}, "a.txt", StatusMalformed, "deleted file"},
		{"both new and deleted", []FileDiff{{OldPath: "a.txt", NewPath: "a.txt", NewFile: true, DeletedFile: true, Diff: diffNew}}, "a.txt", StatusMalformed, "both new and deleted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := mustBuild(t, c.diffs...)
			err := m.Reviewable()
			var nr *NotReviewable
			if !errors.As(err, &nr) {
				t.Fatalf("Reviewable() = %v, want a *NotReviewable", err)
			}
			found := false
			for _, r := range nr.Reasons {
				if r.Path == c.path && r.Status == c.status && strings.Contains(r.Detail, c.detail) {
					found = true
				}
			}
			if !found {
				t.Fatalf("reasons %+v: want {%q %s ...%q...}", nr.Reasons, c.path, c.status, c.detail)
			}
			if !strings.Contains(err.Error(), string(c.status)) {
				t.Errorf("error text %q does not name %s", err.Error(), c.status)
			}
			// Nothing of an unreadable file is anchorable, and the rendering
			// says so in words.
			if c.path != "" {
				f := fileOf(t, m, c.path)
				if len(f.Hunks) != 0 {
					t.Errorf("a %s file kept %d hunks", c.status, len(f.Hunks))
				}
				for _, side := range []Side{SideNew, SideOld} {
					for n := 0; n < 12; n++ {
						if _, ok := m.Lookup(Anchor{c.path, side, n}); ok {
							t.Errorf("anchor %s:%s:%d resolves in a %s file", c.path, side, n, c.status)
						}
					}
				}
				if !strings.Contains(m.Render(), "NOT REVIEWABLE ("+string(c.status)+")") {
					t.Errorf("rendering does not say NOT REVIEWABLE (%s):\n%s", c.status, m.Render())
				}
			}
		})
	}
}

func TestReviewableMap(t *testing.T) {
	m := mustBuild(t, allText()...)
	if err := m.Reviewable(); err != nil {
		t.Fatalf("Reviewable() = %v", err)
	}
	// 3+2, 1+1, 3, 2, 1+1, 1+1, 0, 1
	if got := m.ChangedLines(); got != 17 {
		t.Fatalf("ChangedLines() = %d, want 17", got)
	}
	if n := len(m.Files()); n != 8 {
		t.Fatalf("%d files", n)
	}
	if mustBuild(t, fdCount, fdTooLarge).Reviewable() == nil {
		t.Fatal("one unreadable file among readable ones: the map reads as reviewable")
	}
}

func TestBuildRefusesWhatItCannotKey(t *testing.T) {
	cases := []struct {
		name  string
		diffs []FileDiff
	}{
		{"empty new path", []FileDiff{{OldPath: "a", NewPath: "", Diff: diffRename}}},
		{"empty old path", []FileDiff{{OldPath: "", NewPath: "a", Diff: diffRename}}},
		{"the same new path twice", []FileDiff{fdCount, fdCount}},
		{"two files renamed onto one path", []FileDiff{
			{OldPath: "a", NewPath: "c", RenamedFile: true, Diff: diffRename},
			{OldPath: "b", NewPath: "c", RenamedFile: true, Diff: diffRename}}},
		{"one old path renamed twice", []FileDiff{
			{OldPath: "a", NewPath: "b", RenamedFile: true, Diff: diffRename},
			{OldPath: "a", NewPath: "c", RenamedFile: true, Diff: diffRename}}},
		{"the same unreadable path twice", []FileDiff{fdTooLarge, fdTooLarge}},
	}
	for _, c := range cases {
		if m, err := Build(c.diffs); !errors.Is(err, ErrBadInput) || m != nil {
			t.Errorf("%s: Build = %v, %v; want an error and no map", c.name, m, err)
		}
	}
	// A swap is two files and four distinct (path, side) keys: accepted.
	swap := []FileDiff{
		{OldPath: "a", NewPath: "b", RenamedFile: true, Diff: diffRename},
		{OldPath: "b", NewPath: "a", RenamedFile: true, Diff: diffCRLF},
	}
	m := mustBuild(t, swap...)
	if l, ok := m.Lookup(Anchor{"a", SideOld, 2}); !ok || l.Text != "Each shelf holds four crates." {
		t.Errorf("swap: a:old:2 = %+v, %v", l, ok)
	}
	if l, ok := m.Lookup(Anchor{"a", SideNew, 2}); !ok || l.Text != "set SHELVES=6" {
		t.Errorf("swap: a:new:2 = %+v, %v", l, ok)
	}
	// A file created at the old path of a renamed one: also four distinct keys.
	m = mustBuild(t, FileDiff{OldPath: "a", NewPath: "b", RenamedFile: true, Diff: diffRename},
		FileDiff{OldPath: "a", NewPath: "a", NewFile: true, Diff: diffNew})
	if l, ok := m.Lookup(Anchor{"a", SideOld, 1}); !ok || l.Text != "# Shelves" {
		t.Errorf("a:old:1 = %+v, %v", l, ok)
	}
	if l, ok := m.Lookup(Anchor{"a", SideNew, 1}); !ok || l.Text != "package stock" {
		t.Errorf("a:new:1 = %+v, %v", l, ok)
	}
}

// RV-ANCHOR's lookup. Each row is one (path, side, line) and what must be
// there; "" op means the anchor must NOT resolve.
func TestLookup(t *testing.T) {
	m := mustBuild(t, allText()...)
	const count, oldDoc, newDoc = "pkg/stock/count.go", "docs/shelves.md", "docs/storage/shelves.md"
	cases := []struct {
		name string
		a    Anchor
		op   Op
		text string
	}{
		// ---- inside a hunk
		{"added line, new side", Anchor{count, SideNew, 6}, OpAdded, "func Count(width, depth int) int {"},
		{"removed line, old side", Anchor{count, SideOld, 6}, OpRemoved, "func Count(width int) int {"},
		{"context line by its new number", Anchor{count, SideNew, 8}, OpContext, "\tif width < 0 {"},
		{"the same context line by its old number", Anchor{count, SideOld, 7}, OpContext, "\tif width < 0 {"},
		{"first line of the first hunk", Anchor{count, SideNew, 3}, OpContext, "import \"errors\""},
		{"last line of the first hunk, new", Anchor{count, SideNew, 10}, OpContext, "\t}"},
		{"last line of the first hunk, old", Anchor{count, SideOld, 9}, OpContext, "\t}"},
		{"second hunk, added", Anchor{count, SideNew, 24}, OpAdded, "\treturn \"stocked\""},
		{"second hunk, removed", Anchor{count, SideOld, 23}, OpRemoved, "\treturn \"full\""},
		{"blank context line", Anchor{count, SideNew, 4}, OpContext, ""},

		// ---- rule:outside-hunk -- a line no hunk holds is not in the diff
		{"OUTSIDE: before the first hunk", Anchor{count, SideNew, 2}, "", ""},
		{"OUTSIDE: line 1", Anchor{count, SideNew, 1}, "", ""},
		{"OUTSIDE: just past the first hunk, new", Anchor{count, SideNew, 11}, "", ""},
		{"OUTSIDE: just past the first hunk, old", Anchor{count, SideOld, 10}, "", ""},
		{"OUTSIDE: between the hunks", Anchor{count, SideNew, 15}, "", ""},
		{"OUTSIDE: between the hunks, old", Anchor{count, SideOld, 15}, "", ""},
		{"OUTSIDE: past the last hunk, new", Anchor{count, SideNew, 25}, "", ""},
		{"OUTSIDE: past the last hunk, old", Anchor{count, SideOld, 24}, "", ""},
		{"OUTSIDE: far past the end", Anchor{count, SideNew, 100000}, "", ""},
		{"line 0", Anchor{count, SideNew, 0}, "", ""},
		{"line 0, old", Anchor{count, SideOld, 0}, "", ""},
		{"negative line", Anchor{count, SideNew, -6}, "", ""},

		// ---- rule:sides -- old and new numbers are different spaces
		{"SIDES: new 7 is the added comment, not old 7", Anchor{count, SideNew, 7}, OpAdded, "\t// depth is measured in crates, not metres"},
		{"SIDES: old 20 opens the second hunk", Anchor{count, SideOld, 20}, OpContext, "\tif n == 0 {"},
		{"SIDES: new 20 is outside every hunk", Anchor{count, SideNew, 20}, "", ""},
		{"SIDES: new 21 opens the second hunk", Anchor{count, SideNew, 21}, OpContext, "\tif n == 0 {"},
		{"SIDES: old 21 is the line after it", Anchor{count, SideOld, 21}, OpContext, "\t\treturn \"empty\""},
		{"SIDES: a removed line has no new number (new file's old side)", Anchor{"pkg/stock/depth.go", SideOld, 1}, "", ""},
		{"SIDES: an added line has no old number (deleted file's new side)", Anchor{"legacy/notes.txt", SideNew, 1}, "", ""},
		{"SIDES: deleted file, old side", Anchor{"legacy/notes.txt", SideOld, 2}, OpRemoved, "do not use"},
		{"SIDES: new file, new side", Anchor{"pkg/stock/depth.go", SideNew, 3}, OpAdded, "const DefaultDepth = 2"},
		{"SIDES: VERSION old 1", Anchor{"VERSION", SideOld, 1}, OpRemoved, "1.4.0"},
		{"SIDES: VERSION new 1", Anchor{"VERSION", SideNew, 1}, OpAdded, "1.5.0"},
		{"unknown side", Anchor{count, "", 6}, "", ""},
		{"side in the wrong case", Anchor{count, "NEW", 6}, "", ""},
		{"side with a space", Anchor{count, "new ", 6}, "", ""},

		// ---- rule:rename -- the old side lives under the OLD path
		{"RENAME: old path, old side", Anchor{oldDoc, SideOld, 2}, OpRemoved, "Each shelf holds four crates."},
		{"RENAME: old path, old side, context", Anchor{oldDoc, SideOld, 3}, OpContext, "See the stock package."},
		{"RENAME: new path, new side", Anchor{newDoc, SideNew, 2}, OpAdded, "Each shelf holds six crates."},
		{"RENAME: the old path has no new side", Anchor{oldDoc, SideNew, 2}, "", ""},
		{"RENAME: the new path has no old side", Anchor{newDoc, SideOld, 2}, "", ""},

		// ---- paths
		{"unknown path", Anchor{"pkg/stock/other.go", SideNew, 6}, "", ""},
		{"empty path", Anchor{"", SideNew, 6}, "", ""},
		{"path in another case", Anchor{"PKG/stock/count.go", SideNew, 6}, "", ""},
		{"path with ./", Anchor{"./pkg/stock/count.go", SideNew, 6}, "", ""},
		{"path with a trailing space", Anchor{count + " ", SideNew, 6}, "", ""},
		{"pure rename: nothing to cite, new", Anchor{"LICENSE", SideNew, 1}, "", ""},
		{"pure rename: nothing to cite, old", Anchor{"LICENSE.txt", SideOld, 1}, "", ""},
		{"CRLF line carries no CR", Anchor{"win/setup.bat", SideNew, 2}, OpAdded, "set SHELVES=6"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, ok := m.Lookup(c.a)
			if c.op == "" {
				if ok {
					t.Fatalf("anchor %+v resolves to %+v; it is not a line of the diff", c.a, l)
				}
				if l != (Line{}) {
					t.Fatalf("a refused lookup returned %+v", l)
				}
				if q := m.Quote(c.a, "func Count(width, depth int) int {"); q != QuoteNoAnchor {
					t.Fatalf("Quote on a missing anchor = %q", q)
				}
				return
			}
			if !ok {
				t.Fatalf("anchor %+v does not resolve", c.a)
			}
			if l.Op != c.op || l.Text != c.text {
				t.Fatalf("anchor %+v = {%s %q}, want {%s %q}", c.a, l.Op, l.Text, c.op, c.text)
			}
			n := l.New
			if c.a.Side == SideOld {
				n = l.Old
			}
			if n != c.a.Line {
				t.Fatalf("anchor %+v resolved to a line numbered %d on that side", c.a, n)
			}
		})
	}
}

// RV-QUOTE's lookup: the quote against the text AT THE ANCHOR, under the rule
// gonk grounds pipeline evidence with (pipelinectx.Norm; one line; literal;
// case-sensitive).
func TestQuote(t *testing.T) {
	m := mustBuild(t, allText()...)
	const count = "pkg/stock/count.go"
	added := Anchor{count, SideNew, 7} // "\t// depth is measured in crates, not metres"
	cases := []struct {
		name  string
		a     Anchor
		quote string
		want  QuoteMatch
	}{
		{"the whole line, verbatim", added, "\t// depth is measured in crates, not metres", QuoteWhole},
		{"whole, indentation dropped", added, "// depth is measured in crates, not metres", QuoteWhole},
		{"whole, whitespace runs collapsed either way", added, "  //   depth is  measured in crates,\tnot metres  ", QuoteWhole},
		{"a part of the line", added, "measured in crates", QuoteWithin},
		{"a part, whitespace collapsed", added, "measured   in\tcrates", QuoteWithin},
		{"one character of it", added, "d", QuoteWithin},
		{"reworded", added, "depth is measured in metres", QuoteNoMatch},
		{"another case", added, "// Depth is measured in crates, not metres", QuoteNoMatch},
		{"whitespace removed inside a word boundary", added, "//depth is measured in crates, not metres", QuoteNoMatch},
		{"an ellipsis", added, "// depth is ... not metres", QuoteNoMatch},
		{"empty", added, "", QuoteNoMatch},
		{"only whitespace", added, " \t ", QuoteNoMatch},
		{"two lines joined with a newline", added, "func Count(width, depth int) int {\n\t// depth is measured in crates, not metres", QuoteNoMatch},
		{"the line plus a trailing newline", added, "// depth is measured in crates, not metres\n", QuoteNoMatch},
		{"a carriage return inside", added, "// depth is measured\rin crates", QuoteNoMatch},
		// The text of ANOTHER line of the same file, however real, is not at this anchor.
		{"the neighbouring line's text", added, "func Count(width, depth int) int {", QuoteNoMatch},
		{"the old side's text at the new side's number", Anchor{count, SideNew, 6}, "func Count(width int) int {", QuoteNoMatch},
		{"the new side's text at the old side's number", Anchor{count, SideOld, 6}, "func Count(width, depth int) int {", QuoteNoMatch},
		{"old text at the old anchor", Anchor{count, SideOld, 6}, "func Count(width int) int {", QuoteWhole},
		{"a blank line cannot be quoted", Anchor{count, SideNew, 4}, " ", QuoteNoMatch},
		{"anchor outside every hunk", Anchor{count, SideNew, 15}, "measured in crates", QuoteNoAnchor},
		{"renamed file, old path", Anchor{"docs/shelves.md", SideOld, 2}, "holds four crates", QuoteWithin},
		{"renamed file, old path on the new side", Anchor{"docs/shelves.md", SideNew, 2}, "holds six crates", QuoteNoAnchor},
		{"CRLF file: the quote needs no CR", Anchor{"win/setup.bat", SideOld, 2}, "set SHELVES=4", QuoteWhole},
	}
	for _, c := range cases {
		if got := m.Quote(c.a, c.quote); got != c.want {
			t.Errorf("%s: Quote(%+v, %q) = %q, want %q", c.name, c.a, c.quote, got, c.want)
		}
	}
	for q, want := range map[string]int{"": 0, "  ": 0, " a  b ": 3, "\tmeasured   in crates\n": 18, "\u00e9t\u00e9": 3} {
		if got := QuoteRunes(q); got != want {
			t.Errorf("QuoteRunes(%q) = %d, want %d", q, got, want)
		}
	}
}

func TestCoord(t *testing.T) {
	cases := []struct {
		l    Line
		want string
	}{
		{Line{Old: 7, New: 8, Op: OpContext}, "[OLD:7,NEW:8]"},
		{Line{New: 6, Op: OpAdded}, "[NEW:6]"},
		{Line{Old: 6, Op: OpRemoved}, "[OLD:6]"},
	}
	for _, c := range cases {
		if got := Coord(c.l); got != c.want {
			t.Errorf("Coord(%+v) = %q, want %q", c.l, got, c.want)
		}
	}
}

// Text gonk shows is one physical line whatever the file holds: a character
// that would break the line, hide text or reorder it is replaced, and the
// replaced text is what a quote is matched against.
func TestLineTextIsMadeSafeToShow(t *testing.T) {
	diff := "@@ -1,1 +1,7 @@\n" +
		" plain\n" +
		"+form\ffeed and vtab\vhere\n" +
		"+mid\rline CR\n" +
		"+esc \x1b[31m bell \a nul \x00.\n" +
		"+sep \u2028 para \u2029 nel \u0085.\n" +
		"+bidi \u202e gnp.exe \u2066 x\n" +
		"+bad utf8 \xff\xfe.\n"
	m := mustBuild(t, FileDiff{OldPath: "a.txt", NewPath: "a.txt", Diff: diff})
	f := fileOf(t, m, "a.txt")
	if f.Status != StatusText {
		t.Fatalf("status %s: %s", f.Status, f.Detail)
	}
	want := []string{
		"plain",
		"form feed and vtab here",
		"mid line CR",
		"esc ?[31m bell ? nul ?.",
		"sep   para   nel  .",
		"bidi ? gnp.exe ? x",
		"bad utf8 ?.",
	}
	for i, l := range f.Hunks[0].Lines {
		if l.Text != want[i] {
			t.Errorf("line %d text %q, want %q", i, l.Text, want[i])
		}
	}
	for _, line := range strings.Split(m.Render(), "\n") {
		for _, r := range line {
			if r != '\t' && (r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)) {
				t.Errorf("rendered line %q holds U+%04X", line, r)
			}
		}
	}
	if q := m.Quote(Anchor{"a.txt", SideNew, 3}, "mid line CR"); q != QuoteWhole {
		t.Errorf("quote of the shown text = %q", q)
	}
}
