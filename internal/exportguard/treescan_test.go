package exportguard

import (
	"strings"
	"testing"
)

// A tree scanned in parts (one guard each) must answer exactly as a tree
// scanned whole: the same findings in the same order, and an exemption is
// stale only when no part used it.
func TestMergeScansAnswersLikeOneScan(t *testing.T) {
	addr := ip4(10, 1, 2, 3)
	allow := "path\tdocs/a.md\t" + addr + "\ta documented example\n" +
		"path\tdocs/gone.md\t" + addr + "\tnothing is there\n"
	files := []struct{ path, body string }{
		{"docs/a.md", "use " + addr + "\n"},                    // excused
		{"docs/b.md", "one\n" + addr + "\nand " + addr + "\n"}, // two findings
		{"docs/c.md", "clean\n"},
		{"a/first.md", "x " + ip4(192, 168, 7, 7) + "\n"}, // sorts first
		{"wide.txt", "\xff\xfeh\x00"},                     // a problem
	}

	whole := &TreeScan{Guard: newGuard(t, testDeny, allow)}
	for _, f := range files {
		whole.Path(f.path)
		whole.Blob(f.path, []byte(f.body))
	}
	whole.Finish()

	for _, n := range []int{1, 2, 3, 7} {
		parts := make([]*TreeScan, n)
		for i := range parts {
			parts[i] = &TreeScan{Guard: newGuard(t, testDeny, allow)}
		}
		for i, f := range files {
			parts[i%n].Path(f.path)
			parts[i%n].Blob(f.path, []byte(f.body))
		}
		got := MergeScans(parts)
		if got.Files != len(files) || got.Files != whole.Files {
			t.Errorf("%d parts: %d files, want %d", n, got.Files, len(files))
		}
		if len(got.Findings) != 3 || len(got.Findings) != len(whole.Findings) {
			t.Fatalf("%d parts: %d findings, the whole scan has %d: %v", n, len(got.Findings), len(whole.Findings), got.Findings)
		}
		for i := range got.Findings {
			if got.Findings[i] != whole.Findings[i] {
				t.Errorf("%d parts: finding %d is %s, the whole scan has %s", n, i, got.Findings[i], whole.Findings[i])
			}
		}
		if got.Findings[0].Path != "a/first.md" || got.Findings[1].Line != 2 || got.Findings[2].Line != 3 {
			t.Errorf("%d parts: findings are not in path and offset order: %v", n, got.Findings)
		}
		// Exactly two problems: the UTF-16 file, and the ONE exemption no
		// part used. The exemption for docs/a.md was used by one part and is
		// unused in every other; it must not be reported.
		if len(got.Problems) != 2 || len(whole.Problems) != 2 {
			t.Fatalf("%d parts: problems %q, the whole scan has %q", n, got.Problems, whole.Problems)
		}
		joined := strings.Join(got.Problems, "\n")
		if !strings.Contains(joined, "UTF-16") || !strings.Contains(joined, "docs/gone.md") || strings.Contains(joined, "docs/a.md") {
			t.Errorf("%d parts: problems %q", n, got.Problems)
		}
	}
}
