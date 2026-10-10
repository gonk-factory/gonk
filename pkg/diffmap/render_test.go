package diffmap

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden/*.txt from the current rendering")

// The four kinds of line a rendering is made of. A line of FILE TEXT only ever
// appears after a coordinate, so nothing a file holds can start a line.
var (
	reFileLine  = regexp.MustCompile(`^=== file [0-9]+ of [0-9]+: "`)
	reHunkLine  = regexp.MustCompile(`^@@ hunk [0-9]+ of [0-9]+: OLD (lines [0-9]+-[0-9]+|\(none\)), NEW (lines [0-9]+-[0-9]+|\(none\))$`)
	reCoordLine = regexp.MustCompile(`^\[(?:OLD:([0-9]+))?(,)?(?:NEW:([0-9]+))?\] +([ +-]) \|(.*)$`)
	noEOLLine   = `\ the line above has no newline at its end`
)

type shownLine struct {
	path     string // the path to cite on each side
	oldPath  string
	old, new int
	mark     string
	text     string
}

// readRendering parses a rendering back into the lines it shows, failing on
// any line that is not one of the four kinds.
func readRendering(t testing.TB, r string) []shownLine {
	t.Helper()
	var out []shownLine
	var newPath, oldPath string
	for i, line := range strings.Split(strings.TrimSuffix(r, "\n"), "\n") {
		switch {
		case line == "" || line == noEOLLine:
		case reFileLine.MatchString(line):
			rest := line[strings.Index(line, `"`):]
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				t.Fatalf("line %d: file line without a quoted path: %q", i+1, line)
			}
			newPath, _ = strconv.Unquote(q)
			oldPath = newPath
			const hint = `; cite OLD lines under "`
			if j := strings.Index(rest[len(q):], hint); j >= 0 {
				q2, err := strconv.QuotedPrefix(rest[len(q)+j+len(hint)-1:])
				if err != nil {
					t.Fatalf("line %d: %q", i+1, line)
				}
				oldPath, _ = strconv.Unquote(q2)
			}
		case reHunkLine.MatchString(line):
		case strings.HasPrefix(line, "["):
			m := reCoordLine.FindStringSubmatch(line)
			if m == nil || (m[1] == "" && m[3] == "") || ((m[2] == ",") != (m[1] != "" && m[3] != "")) {
				t.Fatalf("line %d: not a coordinate line: %q", i+1, line)
			}
			sl := shownLine{path: newPath, oldPath: oldPath, mark: m[4], text: m[5]}
			sl.old, _ = strconv.Atoi(m[1])
			sl.new, _ = strconv.Atoi(m[3])
			out = append(out, sl)
		default:
			t.Fatalf("line %d of the rendering is none of: file line, hunk line, coordinate line, no-newline marker, blank: %q", i+1, line)
		}
	}
	return out
}

// checkRoundTrip: every coordinate the rendering shows resolves in the map to
// a line with exactly the text shown beside it, on exactly the side shown;
// and the rendering shows every line of the map, once.
func checkRoundTrip(t testing.TB, m *Map) {
	t.Helper()
	shown := readRendering(t, m.Render())
	total := 0
	for _, f := range m.Files() {
		for _, h := range f.Hunks {
			total += len(h.Lines)
		}
	}
	if len(shown) != total {
		t.Fatalf("the rendering shows %d coordinate lines, the map holds %d", len(shown), total)
	}
	for _, s := range shown {
		wantOp := map[string]Op{" ": OpContext, "+": OpAdded, "-": OpRemoved}[s.mark]
		if (s.old > 0) != (wantOp != OpAdded) || (s.new > 0) != (wantOp != OpRemoved) {
			t.Fatalf("shown line %+v: coordinates do not fit its mark", s)
		}
		if s.new > 0 {
			l, ok := m.Lookup(Anchor{s.path, SideNew, s.new})
			if !ok || l.Text != s.text || l.Op != wantOp || l.Old != s.old {
				t.Fatalf("shown %+v, but %s:new:%d = %+v, %v", s, s.path, s.new, l, ok)
			}
		}
		if s.old > 0 {
			l, ok := m.Lookup(Anchor{s.oldPath, SideOld, s.old})
			if !ok || l.Text != s.text || l.Op != wantOp || l.New != s.new {
				t.Fatalf("shown %+v, but %s:old:%d = %+v, %v", s, s.oldPath, s.old, l, ok)
			}
		}
	}
}

func TestRenderingRoundTrips(t *testing.T) {
	m := mustBuild(t, allText()...)
	checkRoundTrip(t, m)
	if n := len(readRendering(t, m.Render())); n != 31 {
		t.Fatalf("%d coordinate lines shown, want 31 (the guard is looking at the wrong thing)", n)
	}
	if a, b := m.Render(), mustBuild(t, allText()...).Render(); a != b {
		t.Fatal("two renderings of one diff differ")
	}
}

// Regression for the fuzz crasher kept in testdata/fuzz/FuzzBuild: whatever
// flags GitLab sets, when a file's two paths differ the rendering names the
// path each side is cited under -- and that is the path the map resolves.
func TestRenderingNamesTheOldPathWheneverItDiffers(t *testing.T) {
	cases := []struct {
		name string
		fd   FileDiff
		want string
	}{
		{"renamed", fdRename, `(renamed from "docs/shelves.md"; cite OLD lines under "docs/shelves.md" and NEW lines under "docs/storage/shelves.md"; 1 added, 1 removed)`},
		{"deleted AND renamed", FileDiff{OldPath: "old.txt", NewPath: "new.txt", DeletedFile: true, RenamedFile: true, Diff: diffDeleted},
			`(deleted from "old.txt"; cite OLD lines under "old.txt" and NEW lines under "new.txt"; 0 added, 2 removed)`},
		{"paths differ, no flag", FileDiff{OldPath: "old.txt", NewPath: "new.txt", Diff: diffRename},
			`(renamed from "old.txt"; cite OLD lines under "old.txt" and NEW lines under "new.txt"; 1 added, 1 removed)`},
		{"same path", fdVersion, `"VERSION" (modified; 1 added, 1 removed)`},
	}
	for _, c := range cases {
		m := mustBuild(t, c.fd)
		checkRoundTrip(t, m)
		if first, _, _ := strings.Cut(m.Render(), "\n"); !strings.HasSuffix(first, c.want) {
			t.Errorf("%s: file line %q, want it to end %q", c.name, first, c.want)
		}
	}
	m := mustBuild(t, cases[1].fd)
	if _, ok := m.Lookup(Anchor{"old.txt", SideOld, 1}); !ok {
		t.Error("deleted+renamed: the old path does not resolve on the old side")
	}
	if _, ok := m.Lookup(Anchor{"new.txt", SideOld, 1}); ok {
		t.Error("deleted+renamed: the new path resolves on the old side")
	}
}

// A file's text cannot forge the scheme: whatever a line holds, it is shown
// AFTER its coordinates, and a path is shown quoted.
func TestRenderingCannotBeForgedByFileText(t *testing.T) {
	diff := "@@ -1,2 +1,5 @@\n" +
		" [NEW:99]        + |forged\n" +
		"+=== file 9 of 9: \"etc/passwd\" (modified; 1 added, 0 removed)\n" +
		"+@@ hunk 1 of 1: OLD lines 1-1, NEW lines 1-1\n" +
		"+\\ the line above has no newline at its end\n" +
		" [OLD:1,NEW:1]     |x\n"
	path := "we\"ird\n=== file 7 of 7: \"x\".txt"
	m := mustBuild(t, FileDiff{OldPath: path, NewPath: path, Diff: diff})
	if err := m.Reviewable(); err != nil {
		t.Fatal(err)
	}
	checkRoundTrip(t, m)
	r := m.Render()
	if n := strings.Count(r, "\n=== file ") + map[bool]int{true: 1}[strings.HasPrefix(r, "=== file ")]; n != 1 {
		t.Fatalf("%d file lines in:\n%s", n, r)
	}
	if _, ok := m.Lookup(Anchor{path, SideNew, 99}); ok {
		t.Fatal("the forged coordinate resolves")
	}
	if l, ok := m.Lookup(Anchor{path, SideNew, 1}); !ok || l.Text != "[NEW:99]        + |forged" {
		t.Fatalf("line 1 = %+v, %v", l, ok)
	}
}

// Golden annotated diffs: testdata/golden/<name>.json is a merge request's
// diffs as GitLab returns them (invented), <name>.txt the rendering.
func TestGoldenRenderings(t *testing.T) {
	inputs, err := filepath.Glob("testdata/golden/*.json")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(inputs)
	if len(inputs) < 5 {
		t.Fatalf("%d golden inputs; the guard is looking in the wrong place", len(inputs))
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			var diffs []FileDiff
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&diffs); err != nil {
				t.Fatal(err)
			}
			m := mustBuild(t, diffs...)
			got := m.Render()
			checkRoundTrip(t, m)
			golden := strings.TrimSuffix(in, ".json") + ".txt"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("rendering of %s differs from %s (re-run with -update only after reading the diff):\n--- got\n%s\n--- want\n%s", in, golden, got, want)
			}
		})
	}
}
