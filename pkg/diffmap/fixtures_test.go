package diffmap

// Invented fixtures (held-out rule: nothing here is, or resembles, a real
// project's change). A tiny "stock" package that counts crates on shelves.

const diffCount = "@@ -3,7 +3,8 @@ package stock\n" +
	" import \"errors\"\n" +
	" \n" +
	" // Count returns how many crates fit on a shelf.\n" +
	"-func Count(width int) int {\n" +
	"+func Count(width, depth int) int {\n" +
	"+\t// depth is measured in crates, not metres\n" +
	" \tif width < 0 {\n" +
	" \t\treturn 0\n" +
	" \t}\n" +
	"@@ -20,4 +21,4 @@ func Shelf(n int) string {\n" +
	" \tif n == 0 {\n" +
	" \t\treturn \"empty\"\n" +
	" \t}\n" +
	"-\treturn \"full\"\n" +
	"+\treturn \"stocked\"\n"

const diffRename = "@@ -1,3 +1,3 @@\n" +
	" # Shelves\n" +
	"-Each shelf holds four crates.\n" +
	"+Each shelf holds six crates.\n" +
	" See the stock package.\n"

const diffNew = "@@ -0,0 +1,3 @@\n" +
	"+package stock\n" +
	"+\n" +
	"+const DefaultDepth = 2\n"

const diffDeleted = "@@ -1,2 +0,0 @@\n" +
	"-old shelf notes\n" +
	"-do not use\n" +
	"\\ No newline at end of file\n"

// Both counts omitted ("-1 +1" means one line each), no newline on either side.
const diffVersion = "@@ -1 +1 @@\n" +
	"-1.4.0\n" +
	"\\ No newline at end of file\n" +
	"+1.5.0\n" +
	"\\ No newline at end of file\n"

const diffCRLF = "@@ -1,2 +1,2 @@\r\n" +
	" echo off\r\n" +
	"-set SHELVES=4\r\n" +
	"+set SHELVES=6\r\n"

const diffBinary = "Binary files /dev/null and b/img/shelf.png differ\n"

// The older GitLab shape: file header lines before the first hunk.
const diffPreamble = "--- a/Makefile\n+++ b/Makefile\n" +
	"@@ -4,2 +4,3 @@ all:\n" +
	" \tgo build ./...\n" +
	"+\tgo vet ./...\n" +
	" .PHONY: all\n"

var (
	fdCount    = FileDiff{OldPath: "pkg/stock/count.go", NewPath: "pkg/stock/count.go", Diff: diffCount}
	fdRename   = FileDiff{OldPath: "docs/shelves.md", NewPath: "docs/storage/shelves.md", RenamedFile: true, Diff: diffRename}
	fdNew      = FileDiff{OldPath: "pkg/stock/depth.go", NewPath: "pkg/stock/depth.go", NewFile: true, Diff: diffNew}
	fdDeleted  = FileDiff{OldPath: "legacy/notes.txt", NewPath: "legacy/notes.txt", DeletedFile: true, Diff: diffDeleted}
	fdVersion  = FileDiff{OldPath: "VERSION", NewPath: "VERSION", Diff: diffVersion}
	fdCRLF     = FileDiff{OldPath: "win/setup.bat", NewPath: "win/setup.bat", Diff: diffCRLF}
	fdBinary   = FileDiff{OldPath: "img/shelf.png", NewPath: "img/shelf.png", NewFile: true, Diff: diffBinary}
	fdTooLarge = FileDiff{OldPath: "data/crates.csv", NewPath: "data/crates.csv", TooLarge: true}
	fdCollapse = FileDiff{OldPath: "data/shelves.csv", NewPath: "data/shelves.csv", Collapsed: true, Diff: diffRename}
	fdMoved    = FileDiff{OldPath: "LICENSE.txt", NewPath: "LICENSE", RenamedFile: true}
	fdPreamble = FileDiff{OldPath: "Makefile", NewPath: "Makefile", Diff: diffPreamble}
)

// allText is every reviewable fixture file in one merge request.
func allText() []FileDiff {
	return []FileDiff{fdCount, fdRename, fdNew, fdDeleted, fdVersion, fdCRLF, fdMoved, fdPreamble}
}
