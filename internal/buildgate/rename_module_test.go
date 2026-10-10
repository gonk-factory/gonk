package buildgate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hack/rename_module.sh moves the Go module path. It is committed so that a
// branch that missed the rename can run the SAME substitution on its own tip,
// which is only worth anything if the substitution is exact and repeatable.
// These tests run the real script in a scratch git repository whose module is
// a made-up one, and pin what it must and must not rewrite.

const (
	renameOld = "old.example.test/group/proj"
	renameNew = "github.com/gonk-factory/gonk"
)

// renameFixture is the scratch tree: path -> content. The names say what each
// file is there to prove.
func renameFixture() map[string]string {
	return map[string]string{
		"go.mod": "module " + renameOld + "\n\ngo 1.26\n",
		// An import block whose order changes with the path: the old path
		// sorts before "zzz.example.test", the new one too, but the new one
		// sorts before "golang.example.test" where the old one sorted after.
		"pkg/a/a.go": "package a\n\nimport (\n\t\"golang.example.test/x\"\n\t\"" + renameOld + "/pkg/b\"\n)\n\nvar _ = x.X\nvar _ = b.B\n",
		// The bare path as a string, a path with a trailing slash, and two
		// occurrences on one line separated by one character.
		"pkg/a/table_test.go": "package a\n\nconst module = \"" + renameOld + "/\"\n\nvar pair = \"" + renameOld + " " + renameOld + "\"\n\nvar bare = \"" + renameOld + "\"\n",
		// A mutation script quoting an import line.
		"hack/mutate_x.py": "MOD = '" + renameOld + "/'\nIMPORT = 'import \"" + renameOld + "/pkg/b\"'\n",
		// NOT module paths: a URL, a clone URL, a raw-file URL, a longer name.
		"docs/urls.md":   "home: https://" + renameOld + "\nclone: https://" + renameOld + ".git\nraw: https://" + renameOld + "/-/raw/main/x.json\n",
		"docs/longer.md": "another project: " + renameOld + "-docs and " + renameOld + "_two\n",
		// The path's dots are literal dots: a look-alike is another name.
		"docs/dots.md":  "real: " + renameOld + "/x\nlook-alike: " + strings.ReplaceAll(renameOld, ".", "Z") + "/x\n",
		"docs/mixed.md": "module " + renameOld + "/pkg/a, served at https://" + renameOld + "/pkg/a\n",
		"vendor/v/v.go": "package v\n\n// quoted from " + renameOld + "/pkg/a\n",
		// A listed file that is NOT Go keeps even a line shaped like an import.
		"docs/plans/p.md": "the module was " + renameOld + " then\n\t\"" + renameOld + "/pkg/a\"\n",
		// A Go file ON the list is still compiled: its import lines move, its
		// other text (here a sample a test feeds to a rule) does not.
		"release/r_test.go": "package release\n\nimport (\n\t\"testing\"\n\n\tg \"" + renameOld + "/internal/guard\"\n\t\"" + renameOld + "\" // the root\n)\n\nvar sample = \"module " + renameOld + "\"\n\nvar table = map[string]bool{\n\t\"" + renameOld + "/pkg/a\": true,\n}\n\nvar _ = g.X\nvar _ testing.T\n",
		"NOTES-internal":    "the module was " + renameOld + " then\n",
		"docs/plansX/q.md":  "not on the list: " + renameOld + "/pkg/a\n",
		"release/internal-only.txt": "# the list\n\n" +
			"docs/plans/      # a directory prefix\n" +
			"NOTES-internal   # a literal file\n" +
			"release/         # the list itself\n",
		"bin.dat": "\x00\x01binary " + renameOld + "/pkg/a\x00",
	}
}

// renameRepo builds the scratch repository and returns its root.
func renameRepo(t *testing.T, mutate func(script string) string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Fatalf("bash is required to test hack/rename_module.sh: %v", err)
	}
	dir := t.TempDir()
	for name, body := range renameFixture() {
		writeRenameFile(t, dir, name, body, 0o644)
	}
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "hack", "rename_module.sh"))
	if err != nil {
		t.Fatalf("read the script: %v", err)
	}
	body := string(script)
	if mutate != nil {
		body = mutate(body)
	}
	writeRenameFile(t, dir, "hack/rename_module.sh", body, 0o755)
	// An untracked file is not the script's to rewrite.
	gitRename(t, dir, "init", "-q")
	gitRename(t, dir, "add", "-A")
	gitRename(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "fixture")
	writeRenameFile(t, dir, "untracked.txt", "import \""+renameOld+"/pkg/a\"\n", 0o644)
	return dir
}

func writeRenameFile(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func gitRename(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func runRename(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(dir, "hack", "rename_module.sh")}, args...)...)
	cmd.Env = append(os.Environ(), "GOFMT="+gofmtPath(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rename_module.sh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// gofmtPath is the gofmt of the toolchain running this test, so the script's
// formatting step does not depend on what PATH happens to hold.
func gofmtPath(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "bin", "gofmt")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("no gofmt beside the toolchain: %v", err)
	}
	return p
}

func readRename(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wantRenamed is the whole tree after one run: every file, changed or not.
func wantRenamed() map[string]string {
	w := renameFixture()
	w["go.mod"] = "module " + renameNew + "\n\ngo 1.26\n"
	// gofmt re-sorted the block: "github.com/..." sorts before "golang...".
	w["pkg/a/a.go"] = "package a\n\nimport (\n\t\"" + renameNew + "/pkg/b\"\n\t\"golang.example.test/x\"\n)\n\nvar _ = x.X\nvar _ = b.B\n"
	w["pkg/a/table_test.go"] = "package a\n\nconst module = \"" + renameNew + "/\"\n\nvar pair = \"" + renameNew + " " + renameNew + "\"\n\nvar bare = \"" + renameNew + "\"\n"
	w["hack/mutate_x.py"] = "MOD = '" + renameNew + "/'\nIMPORT = 'import \"" + renameNew + "/pkg/b\"'\n"
	w["docs/dots.md"] = "real: " + renameNew + "/x\nlook-alike: " + strings.ReplaceAll(renameOld, ".", "Z") + "/x\n"
	w["release/r_test.go"] = "package release\n\nimport (\n\t\"testing\"\n\n\t\"" + renameNew + "\" // the root\n\tg \"" + renameNew + "/internal/guard\"\n)\n\nvar sample = \"module " + renameOld + "\"\n\nvar table = map[string]bool{\n\t\"" + renameOld + "/pkg/a\": true,\n}\n\nvar _ = g.X\nvar _ testing.T\n"
	w["docs/mixed.md"] = "module " + renameNew + "/pkg/a, served at https://" + renameOld + "/pkg/a\n"
	w["docs/plansX/q.md"] = "not on the list: " + renameNew + "/pkg/a\n"
	w["untracked.txt"] = "import \"" + renameOld + "/pkg/a\"\n"
	return w
}

func assertRenameTree(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	for name, body := range want {
		if got := readRename(t, dir, name); got != body {
			t.Errorf("%s after the rename:\n got: %q\nwant: %q", name, got, body)
		}
	}
}

func TestRenameModuleRewritesModulePathsAndNothingElse(t *testing.T) {
	dir := renameRepo(t, nil)
	out := runRename(t, dir)
	assertRenameTree(t, dir, wantRenamed())

	// It says what it left, so the URL-shaped remainder is handled on purpose.
	// docs/urls.md has 3 such lines, docs/longer.md 1, docs/mixed.md 1. The
	// vendored, listed, binary and untracked files are not in the count.
	if !strings.Contains(out, "5 line(s) still carry the old path") {
		t.Errorf("the script did not report the 5 lines it left behind:\n%s", out)
	}
	for _, name := range []string{"docs/urls.md:1:", "docs/urls.md:2:", "docs/urls.md:3:", "docs/longer.md:1:", "docs/mixed.md:1:"} {
		if !strings.Contains(out, "left (not a module path): "+name) {
			t.Errorf("the script did not name %s as left behind:\n%s", name, out)
		}
	}
	for _, name := range []string{"vendor/", "docs/plans/p.md", "NOTES-internal", "bin.dat", "untracked.txt", "release/"} {
		if strings.Contains(out, "left (not a module path): "+name) {
			t.Errorf("the script reported %s, which is not its to report:\n%s", name, out)
		}
	}
}

// Seven files carry a module-path occurrence outside vendor/ and the list,
// and one Go file on the list has imports.
func TestRenameModuleReportsHowManyFilesItRewrote(t *testing.T) {
	dir := renameRepo(t, nil)
	out := runRename(t, dir)
	if !strings.Contains(out, renameOld+" -> "+renameNew+": 8 file(s) rewritten") {
		t.Errorf("want 8 files rewritten:\n%s", out)
	}
	changed := strings.Fields(gitRename(t, dir, "diff", "--name-only"))
	want := []string{"docs/dots.md", "docs/mixed.md", "docs/plansX/q.md", "go.mod", "hack/mutate_x.py", "pkg/a/a.go", "pkg/a/table_test.go", "release/r_test.go"}
	if strings.Join(changed, " ") != strings.Join(want, " ") {
		t.Errorf("files changed:\n got: %v\nwant: %v", changed, want)
	}
}

func TestRenameModuleIsIdempotent(t *testing.T) {
	dir := renameRepo(t, nil)
	runRename(t, dir)
	gitRename(t, dir, "add", "-u") // tracked files only: untracked.txt stays untracked
	gitRename(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "renamed")

	out := runRename(t, dir)
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("a second run did not say it had nothing to do:\n%s", out)
	}
	if st := gitRename(t, dir, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(st) != "" {
		t.Errorf("a second run changed the tree:\n%s", st)
	}

	// And with the old path named explicitly: still nothing, because nothing
	// that is a module path is left.
	out = runRename(t, dir, "--from", renameOld)
	if !strings.Contains(out, "0 file(s) rewritten") {
		t.Errorf("a --from run on a renamed tree rewrote something:\n%s", out)
	}
	if st := gitRename(t, dir, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(st) != "" {
		t.Errorf("a --from run on a renamed tree changed it:\n%s", st)
	}
}

// A merge from a branch that was never renamed leaves a tree whose go.mod is
// already new but which has old imports. --from finishes it.
func TestRenameModuleFromFinishesAHalfMovedTree(t *testing.T) {
	dir := renameRepo(t, nil)
	writeRenameFile(t, dir, "go.mod", "module "+renameNew+"\n\ngo 1.26\n", 0o644)
	gitRename(t, dir, "add", "-A")

	if out := runRename(t, dir); !strings.Contains(out, "nothing to do") {
		t.Fatalf("without --from the script must not guess an old path:\n%s", out)
	}
	if got := readRename(t, dir, "hack/mutate_x.py"); !strings.Contains(got, renameOld) {
		t.Fatalf("without --from the script rewrote a file:\n%s", got)
	}

	runRename(t, dir, "--from", renameOld)
	want := wantRenamed()
	want["untracked.txt"] = "import \"" + renameNew + "/pkg/a\"\n" // tracked by the add above
	assertRenameTree(t, dir, want)
}

func TestRenameModuleDryRunChangesNothing(t *testing.T) {
	dir := renameRepo(t, nil)
	out := runRename(t, dir, "--dry-run")
	if !strings.Contains(out, "would rewrite: go.mod") {
		t.Errorf("dry run did not list go.mod:\n%s", out)
	}
	if st := gitRename(t, dir, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(st) != "" {
		t.Errorf("a dry run changed the tree:\n%s", st)
	}
}

// The script is itself published, so it may not spell the path it replaces:
// it reads that from go.mod. A literal old path in it would also be rewritten
// by its own run, which is how a rename script quietly stops being re-runnable.
func TestRenameModuleScriptNamesOnlyTheNewPath(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "hack", "rename_module.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(script, []byte(`NEW="`+renameNew+`"`)) {
		t.Errorf("hack/rename_module.sh does not set NEW to %s", renameNew)
	}
	gomod, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(gomod, []byte("module "+renameNew+"\n")) {
		t.Errorf("go.mod does not declare %s; the tree and the script disagree about the module path", renameNew)
	}
}
