package buildgate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard"
	"github.com/gonk-factory/gonk/internal/exportguard/gitexport/exporttest"
)

// The tree guard's own negative controls. They run it on scratch trees with
// SYNTHETIC internal names (exporttest.Deny: made-up names, one rule per
// specific class), so that every class is seen refused here without this
// file spelling anything real. Positives are assembled at run time: this
// file is in the tree the guard scans.

func glue(parts ...string) string { return strings.Join(parts, "") }

// plant is one instance of one class, as a line of text.
type plant struct {
	class exportguard.Class
	text  string
}

func plants() []plant {
	return []plant{
		{exportguard.G1, "node at " + fmt.Sprintf("%d.%d.%d.%d", 10, 20, 30, 40)},
		{exportguard.G2, "tailnet " + fmt.Sprintf("%d.%d.%d.%d", 100, 64, 0, 1)},
		{exportguard.G3, "v6 " + glue("fd12", ":3456", ":789a", "::1")},
		{exportguard.G4, glue("-----BEGIN ", "CERTIFICATE-----")},
		{exportguard.G5, glue("https://", "builder:", "hunter2", "@", "forge.corp.zz/x.git")},
		{exportguard.G6, "ask " + glue("someone", "@", "corp.zz")},
		{exportguard.G7, "see " + glue("/ho", "me/", "alice/notes.txt")},
		{exportguard.I1, "host " + glue("gitlab.", "zork", ".lan")},
		{exportguard.I2, "deployed on " + glue("zork", "02")},
		{exportguard.I3, "issuer " + glue("lamp", "-ca")},
		{exportguard.I4, "path " + glue("clusters/", "zork") + "/apps"},
		{exportguard.I5, "context " + glue("admin", "@", "zork")},
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// internalTree is a small INTERNAL tree: the three data files, an
// internal-only directory, one public document.
func internalTree() map[string]string {
	return map[string]string{
		exportguard.ListPath:     "notes/internal/  # build plans\nrelease/     # the export's own data\n",
		exportguard.DenyPath:     exporttest.Deny,
		exportguard.AllowPath:    "# no exemptions\n",
		"notes/internal/plan.md": "internal\n",
		"docs/guide.md":          "A guide.\nSee gitlab.example.internal and 192.0.2.7.\n",
		"README.md":              "# a tree\n",
	}
}

// publicTree is the same tree as an export would leave it.
func publicTree() map[string]string {
	src, err := exportguard.ExportSource{SourceCommit: strings.Repeat("ab", 20), ListSHA256: strings.Repeat("cd", 32)}.Render()
	if err != nil {
		panic(err)
	}
	return map[string]string{
		exportguard.ExportSourcePath: string(src),
		"docs/guide.md":              "A guide.\nSee gitlab.example.internal and 192.0.2.7.\n",
		"README.md":                  "# a tree\n",
	}
}

func isSpecific(c exportguard.Class) bool {
	for _, s := range exportguard.SpecificClasses {
		if s == c {
			return true
		}
	}
	return false
}

func TestTheTreeGuardRefusesEachOfTheTwelveClasses(t *testing.T) {
	if len(plants()) != len(exportguard.AllClasses()) {
		t.Fatalf("%d plants for %d classes", len(plants()), len(exportguard.AllClasses()))
	}
	// Clean first: with nothing planted both trees pass. Without this, a
	// guard that refused everything would look like one that works.
	for name, files := range map[string]map[string]string{"internal": internalTree(), "public": publicTree()} {
		rep, err := scanTree(writeTree(t, files), nil)
		if err != nil || !rep.clean() || string(rep.Mode) != name {
			t.Fatalf("the clean %s tree: err %v, mode %s, findings %v, problems %v", name, err, rep.Mode, rep.Findings, rep.Problems)
		}
	}
	for _, p := range plants() {
		// INTERNAL mode: every class is refused, at the planted line.
		files := internalTree()
		files["docs/guide.md"] = "A guide.\n\n" + p.text + "\n"
		rep, err := scanTree(writeTree(t, files), nil)
		if err != nil {
			t.Fatalf("%s: %v", p.class, err)
		}
		if len(rep.Findings) != 1 {
			t.Errorf("%s (internal): %d findings, want exactly 1: %v", p.class, len(rep.Findings), rep.Findings)
			continue
		}
		f := rep.Findings[0]
		if f.Class != p.class || f.Path != "docs/guide.md" || f.Line != 3 || f.Kind != exportguard.KindContent {
			t.Errorf("%s (internal): got %s", p.class, f)
		}
		if !strings.Contains(f.String(), string(p.class)) || !strings.Contains(f.String(), "docs/guide.md:3:") {
			t.Errorf("%s: the refusal does not name class, path and line: %q", p.class, f.String())
		}

		// PUBLIC mode: the generic classes are refused the same way; the
		// specific ones cannot be -- their rules are not published -- and
		// that is exactly what the mode means.
		files = publicTree()
		files["docs/guide.md"] = "A guide.\n\n" + p.text + "\n"
		rep, err = scanTree(writeTree(t, files), nil)
		if err != nil {
			t.Fatalf("%s: %v", p.class, err)
		}
		switch {
		case isSpecific(p.class):
			// The I5 plant is also e-mail shaped; nothing else may fire.
			for _, f := range rep.Findings {
				if isSpecific(f.Class) {
					t.Errorf("%s (public): a specific rule ran in public mode: %s", p.class, f)
				}
			}
		case len(rep.Findings) != 1 || rep.Findings[0].Class != p.class || rep.Findings[0].Line != 3:
			t.Errorf("%s (public): findings %v", p.class, rep.Findings)
		}
	}
}

func TestTheTreeGuardScansPathsAndSkipsOnlyTheList(t *testing.T) {
	bad := plants()[0].text // a private address

	// A file NAME is scanned like content.
	files := internalTree()
	files[glue("docs/", "zork", "02", "-notes.md")] = "nothing here\n"
	rep, err := scanTree(writeTree(t, files), nil)
	if err != nil || len(rep.Findings) != 1 || rep.Findings[0].Kind != exportguard.KindPath || rep.Findings[0].Class != exportguard.I2 {
		t.Fatalf("a path with an internal name in it: %v %v", err, rep.Findings)
	}

	// Under an internal-only entry anything goes: it is never published.
	files = internalTree()
	files["notes/internal/plan.md"] = bad + "\n" + glue("gitlab.", "zork", ".lan") + "\n"
	rep, err = scanTree(writeTree(t, files), nil)
	if err != nil || !rep.clean() {
		t.Fatalf("an internal-only file was scanned: %v %v", err, rep.Findings)
	}
	// ...but a sibling that only LOOKS listed is scanned.
	files = internalTree()
	files["notes/internalX/plan.md"] = bad + "\n"
	rep, err = scanTree(writeTree(t, files), nil)
	if err != nil || len(rep.Findings) != 1 || rep.Findings[0].Path != "notes/internalX/plan.md" {
		t.Fatalf("notes/internalX is not on the list: %v %v", err, rep.Findings)
	}

	// A binary file is scanned.
	files = internalTree()
	files["img/logo.bin"] = "\x00\x01\x02" + bad + "\x00\xff"
	rep, err = scanTree(writeTree(t, files), nil)
	if err != nil || len(rep.Findings) != 1 || rep.Findings[0].Path != "img/logo.bin" {
		t.Fatalf("a binary file: %v %v", err, rep.Findings)
	}
}

func TestTheTreeGuardReportsProblems(t *testing.T) {
	has := func(rep *treeReport, want string) bool {
		for _, p := range rep.Problems {
			if strings.Contains(p, want) {
				return true
			}
		}
		return false
	}
	t.Run("a list entry that removes nothing", func(t *testing.T) {
		files := internalTree()
		files[exportguard.ListPath] += "docs/gone/  # nothing is there any more\n"
		rep, err := scanTree(writeTree(t, files), nil)
		if err != nil || !has(rep, "docs/gone/") {
			t.Fatalf("%v %v", err, rep)
		}
	})
	t.Run("an exemption that excuses nothing", func(t *testing.T) {
		files := internalTree()
		files[exportguard.AllowPath] = "path\tdocs/guide.md\t" + plants()[0].text + "\tno longer there\n"
		rep, err := scanTree(writeTree(t, files), nil)
		if err != nil || len(rep.Problems) != 1 {
			t.Fatalf("%v %+v", err, rep)
		}
	})
	t.Run("an exemption that excuses exactly its literal in its file", func(t *testing.T) {
		addr := fmt.Sprintf("%d.%d.%d.%d", 10, 20, 30, 40)
		files := internalTree()
		files[exportguard.AllowPath] = "path\tdocs/guide.md\t" + addr + "\ta documented example\n"
		files["docs/guide.md"] = "use " + addr + "\n"
		files["docs/other.md"] = "use " + addr + "\n"
		rep, err := scanTree(writeTree(t, files), nil)
		if err != nil || len(rep.Findings) != 1 || rep.Findings[0].Path != "docs/other.md" || len(rep.Problems) != 0 {
			t.Fatalf("%v %+v", err, rep)
		}
	})
	t.Run("UTF-16 text", func(t *testing.T) {
		files := internalTree()
		files["docs/wide.txt"] = "\xff\xfeh\x00i\x00"
		rep, err := scanTree(writeTree(t, files), nil)
		if err != nil || !has(rep, "UTF-16") {
			t.Fatalf("%v %+v", err, rep)
		}
	})
	t.Run("a scanner ignore file, and a scanner configuration nobody allowed", func(t *testing.T) {
		files := publicTree()
		files[exportguard.ScannerIgnorePath] = "abc:file:rule:1\n"
		files[exportguard.ScannerConfigPath] = "[allowlist]\n"
		rep, err := scanTree(writeTree(t, files), nil)
		if err != nil || !has(rep, exportguard.ScannerIgnorePath) || !has(rep, "scanner-config") {
			t.Fatalf("%v %+v", err, rep)
		}
		// With the entry the configuration is accepted; the ignore file never is.
		rep, err = scanTree(writeTree(t, files), []byte("scanner-config\t.gitleaks.toml\tallowed for this test\n"))
		if err != nil || len(rep.Problems) != 1 || !has(rep, exportguard.ScannerIgnorePath) {
			t.Fatalf("%v %+v", err, rep)
		}
	})
	t.Run("an EXPORT-SOURCE that is not one", func(t *testing.T) {
		files := publicTree()
		files[exportguard.ExportSourcePath] = "format: 1\n"
		rep, err := scanTree(writeTree(t, files), nil)
		if err != nil || !has(rep, exportguard.ExportSourcePath) {
			t.Fatalf("%v %+v", err, rep)
		}
	})
}

// The mode is decided by the tree, and a tree that cannot say which it is
// is refused: scanning it with the generic rules alone would call an
// internal tree clean.
func TestTheTreeGuardCannotBeTalkedOutOfItsMode(t *testing.T) {
	t.Run("both EXPORT-SOURCE and release/", func(t *testing.T) {
		files := internalTree()
		for k, v := range publicTree() {
			files[k] = v
		}
		if _, err := scanTree(writeTree(t, files), nil); err == nil || !strings.Contains(err.Error(), "both") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("neither", func(t *testing.T) {
		if _, err := scanTree(writeTree(t, map[string]string{"README.md": "x\n"}), nil); err == nil || !strings.Contains(err.Error(), "neither") {
			t.Fatalf("err = %v", err)
		}
	})
	for _, missing := range []string{exportguard.DenyPath, exportguard.AllowPath, exportguard.ListPath} {
		t.Run("internal mode without "+filepath.Base(missing), func(t *testing.T) {
			files := internalTree()
			delete(files, missing)
			_, err := scanTree(writeTree(t, files), nil)
			if err == nil || !strings.Contains(err.Error(), missing+" is missing") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("deny data that lost a class", func(t *testing.T) {
		files := internalTree()
		files[exportguard.DenyPath] = strings.Join(strings.Split(exporttest.Deny, "\n")[:3], "\n") + "\n"
		if _, err := scanTree(writeTree(t, files), nil); err == nil {
			t.Fatal("deny data with no rule for most classes was accepted")
		}
	})
}

// In a git checkout the guard reads what `git add -A` would commit: tracked
// files and untracked files that are not ignored -- and nothing git ignores.
func TestTheTreeGuardReadsTrackedAndUntrackedButNotIgnored(t *testing.T) {
	bad := plants()[0].text + "\n"
	files := internalTree()
	files[".gitignore"] = "scratch/\n"
	files["tracked.md"] = bad
	root := writeTree(t, files)
	git := func(args ...string) {
		t.Helper()
		if out, err := exporttest.GitIn(root, 0, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet", "--initial-branch=main")
	git("add", "--all")
	git("commit", "--quiet", "-m", "base")
	for name, body := range map[string]string{"untracked.md": bad, "scratch/ignored.md": bad} {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A tracked file deleted from the working tree is not an error.
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	// What a git hook exports must not redirect the listing.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "no-such-repo.git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "no-such-index"))

	rep, err := scanTree(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range rep.Findings {
		got = append(got, f.Path)
	}
	if strings.Join(got, " ") != "tracked.md untracked.md" {
		t.Fatalf("findings in %v, want tracked.md and untracked.md only", got)
	}
}

// `make gate` reaches the tree guard as its own step, before the long test
// run, through the wrapper that refuses a filter matching no test.
func TestTheGateRunsTheTreeGuard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range treeGuardGateProblems(string(data)) {
		t.Error(p)
	}
}

func treeGuardGateProblems(makefile string) []string {
	deps, recipes := parseMakeTargets(makefile)
	var problems []string
	idx := func(name string) int {
		for i, d := range deps["gate"] {
			if d == name {
				return i
			}
		}
		return -1
	}
	switch {
	case idx("guard") < 0:
		problems = append(problems, fmt.Sprintf("`guard` is not a prerequisite of `gate` (gate: %s)", strings.Join(deps["gate"], " ")))
	case idx("test") >= 0 && idx("guard") > idx("test"):
		problems = append(problems, "`guard` comes after `test` in `gate`; it is the fast step and must refuse first")
	}
	recipe := recipes["guard"]
	for _, want := range []string{
		"hack/require_nonzero_go_tests.sh",
		"./internal/buildgate/",
		"'^TestPublicTreeHasNoInternalDetail$$'",
		"-count=1",
	} {
		if !strings.Contains(recipe, want) {
			problems = append(problems, fmt.Sprintf("the `guard` recipe does not contain %s", want))
		}
	}
	for _, bad := range []string{"|| true", "-skip", "relexport"} {
		if strings.Contains(recipe, bad) {
			problems = append(problems, fmt.Sprintf("the `guard` recipe contains %q: the gate's guard step may not be softened, and may not need a clone or a scanner", bad))
		}
	}
	return problems
}

func TestTheTreeGuardGateCheckActuallyFails(t *testing.T) {
	good := "gate: toolchain fmt vet guard test goldens lint\nguard:\n\t@hack/require_nonzero_go_tests.sh ./internal/buildgate/ -run '^TestPublicTreeHasNoInternalDetail$$' -count=1\ntest:\n\tgo test ./...\n"
	if p := treeGuardGateProblems(good); len(p) != 0 {
		t.Fatalf("the good Makefile was refused: %v", p)
	}
	for name, bad := range map[string]string{
		"guard not in gate":   strings.Replace(good, " guard test", " test", 1),
		"guard after test":    strings.Replace(good, "guard test goldens", "test guard goldens", 1),
		"no wrapper":          strings.Replace(good, "hack/require_nonzero_go_tests.sh", "go test", 1),
		"another test":        strings.Replace(good, "TestPublicTreeHasNoInternalDetail", "TestSomethingElse", 1),
		"softened":            strings.Replace(good, "-count=1", "-count=1 || true", 1),
		"cached":              strings.Replace(good, " -count=1", "", 1),
		"no recipe":           "gate: guard test\nguard:\ntest:\n\tgo test ./...\n",
		"needs the export":    strings.Replace(good, "@hack/", "@go run ./hack/relexport check && hack/", 1),
		"unanchored test run": strings.Replace(good, "'^TestPublicTreeHasNoInternalDetail$$'", "TestPublicTree", 1),
	} {
		if len(treeGuardGateProblems(bad)) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
}
