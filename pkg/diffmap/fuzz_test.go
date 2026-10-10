package diffmap

import (
	"strings"
	"testing"
)

// FuzzParseHunkHeader: no input panics; an accepted header is in range, and
// printing it and parsing it again gives the same header.
func FuzzParseHunkHeader(f *testing.F) {
	for _, s := range []string{
		"@@ -3,7 +3,8 @@ package stock", "@@ -1 +1 @@", "@@ -0,0 +1,3 @@", "@@ -1,2 +0,0 @@",
		"@@ -5 +5,2 @@", "@@ -99999999999999999999,1 +1,1 @@", "@@ -1,1 +1,1 @@x", "@@@ -1 -1 +1 @@@",
		"@@ -,7 +3,8 @@", "@@ -3,7 +3,8 @@ @@ -9,9 +9,9 @@", "", "@@", "@@ -1 +1 @@\r",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, err := ParseHunkHeader(s)
		if err != nil {
			if h != (HunkHeader{}) {
				t.Fatalf("refused %q but returned %+v", s, h)
			}
			return
		}
		for _, n := range []int{h.OldStart, h.OldLines, h.NewStart, h.NewLines} {
			if n < 0 || n > MaxLineNumber {
				t.Fatalf("%q: number %d out of range", s, n)
			}
		}
		if !strings.HasPrefix(s, "@@ -") {
			t.Fatalf("accepted %q", s)
		}
		again, err := ParseHunkHeader(h.String())
		if err != nil || again != h {
			t.Fatalf("%q -> %+v -> %q -> %+v, %v", s, h, h.String(), again, err)
		}
	})
}

// FuzzBuild: no diff text panics; whatever it yields is either NOT reviewable
// with a reason, or a map whose rendering round-trips line for line and whose
// numbers on each side strictly increase.
func FuzzBuild(f *testing.F) {
	for _, d := range allText() {
		f.Add(d.Diff, d.NewFile, d.DeletedFile, d.RenamedFile)
	}
	f.Add(diffBinary, true, false, false)
	f.Add("@@ -1,2 +1,2 @@\n a\n-b\n", false, false, false)
	f.Add("@@ -1 +1 @@\n\\ No newline at end of file\n", false, false, false)
	f.Add("@@ -1,1 +1,2 @@\n a\n+b\n@@ -5,1 +2,1 @@\n-c\n+d\n", false, false, false)
	f.Add("@@ -2147483647,1 +2147483647,2 @@\n a\n+b\n", false, false, false)
	f.Fuzz(func(t *testing.T, diff string, isNew, isDeleted, isRenamed bool) {
		fd := FileDiff{OldPath: "old.txt", NewPath: "old.txt", NewFile: isNew, DeletedFile: isDeleted, RenamedFile: isRenamed, Diff: diff}
		if isRenamed {
			fd.NewPath = "new.txt"
		}
		m, err := Build([]FileDiff{fd})
		if err != nil {
			t.Fatalf("Build refused a keyable file: %v", err)
		}
		file := m.Files()[0]
		if rerr := m.Reviewable(); rerr != nil {
			if file.Status == StatusText {
				t.Fatalf("a text file is not reviewable: %v", rerr)
			}
			if len(file.Hunks) != 0 || m.ChangedLines() != 0 {
				t.Fatalf("a %s file carries lines", file.Status)
			}
			if !strings.Contains(m.Render(), "NOT REVIEWABLE") && file.Status != StatusNoLines {
				t.Fatalf("a %s file is not shown as not reviewable:\n%s", file.Status, m.Render())
			}
			return
		}
		if file.Status != StatusText {
			t.Fatalf("reviewable, with status %s", file.Status)
		}
		checkRoundTrip(t, m)
		lastOld, lastNew, added, removed := 0, 0, 0, 0
		for _, h := range file.Hunks {
			if len(h.Lines) == 0 {
				t.Fatal("empty hunk kept")
			}
			for _, l := range h.Lines {
				switch l.Op {
				case OpAdded:
					added++
				case OpRemoved:
					removed++
				case OpContext:
				default:
					t.Fatalf("op %q", l.Op)
				}
				if l.Op != OpAdded {
					if l.Old <= lastOld {
						t.Fatalf("old numbers do not increase: %d after %d", l.Old, lastOld)
					}
					lastOld = l.Old
				}
				if l.Op != OpRemoved {
					if l.New <= lastNew {
						t.Fatalf("new numbers do not increase: %d after %d", l.New, lastNew)
					}
					lastNew = l.New
				}
				if strings.ContainsAny(l.Text, "\n\r") {
					t.Fatalf("text %q holds a line break", l.Text)
				}
			}
		}
		if added != file.Added || removed != file.Removed || m.ChangedLines() != added+removed {
			t.Fatalf("counts: %d/%d vs %d/%d (%d)", added, removed, file.Added, file.Removed, m.ChangedLines())
		}
		if (isNew && removed+lastOld > 0) || (isDeleted && added+lastNew > 0) {
			t.Fatalf("new=%v deleted=%v but old=%d new=%d", isNew, isDeleted, lastOld, lastNew)
		}
	})
}
