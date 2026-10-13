package buildgate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard"
)

// No published file may READ an unpublished one.
//
// The public mirror receives this tree minus the paths on
// release/internal-only.txt. A test, script or CI job that opens one of those
// paths works here and fails there -- or, worse, is written to skip when the
// file is missing and so checks nothing in public. That is how ten tests came
// to read normative tables out of build plans that are never published; the
// tables now live in docs/reference/ and this test keeps the class from
// growing back.
//
// WHAT IT CATCHES. In every tracked, published .go, .py and .sh file, Makefile
// and CI workflow: a path that, once leading "./" and "../" segments are
// dropped, IS a listed file or lies UNDER a listed directory. In Go that is a
// string literal, or the literal arguments of a filepath.Join / path.Join
// call taken together (Join(root, "docs", "plans", "x.md")); elsewhere it is
// a whitespace- or quote-delimited token.
//
// WHAT IT DOES NOT CATCH, stated so it is not mistaken for more: a path that
// is assembled at run time from pieces this cannot see, and a path mentioned
// in the middle of a sentence (a comment or an error message that CITES a
// plan is not a read; those are counted by the export report). The stronger
// proof is the export tool's own: build the public tree and run the gate in
// it.
//
// A NAME THE PRODUCT USES AS DATA (targetProjectNames). One listed path is
// also what gonk calls a file in the repository it WORKS ON: the product
// classifies, protects and cites the target project's pipeline definition by
// that name all through pkg/scope, pkg/effects and cmd/gonk-gate. In a Go file
// such a name is therefore a read only where the file system is asked for it:
// as an argument of an os, filepath, path or ioutil call (which includes a
// Join). As a bare literal anywhere else it is data. In a script, Makefile or
// workflow it stays a read, as every listed path does. What that leaves
// unseen is a read through a named constant -- which is how the one
// sanctioned reader (maintainer_ci_test.go) is written, and why every other
// reader must go through it.
//
// EXEMPTIONS are exact and each has its reason: a file (or a directory,
// written with a trailing "/") whose subject IS the internal-only paths, or
// one path in one file that only looks like a listed one. One that no longer
// matches anything is itself a failure.

// internalReadExempt: published files whose SUBJECT is the internal-only
// paths, so that they name them without reading them from a published tree.
var internalReadExempt = map[string]string{
	"internal/exportguard/":                          "the export guard and tool: the list's parser, its constants, and tests that build scratch trees holding listed paths",
	"hack/mutate_relexport.py":                       "mutates the export tool and its data; meaningful only where release/ exists",
	"internal/buildgate/internal_only_reads_test.go": "this test: its tables of violations and of exemptions spell the paths",
	"internal/buildgate/rename_module_test.go":       "builds a scratch repository that has an internal-only list of its own",
	"hack/rename_module.sh":                          "reads the list only if it exists, to know what NOT to rewrite",
}

// notARead: one path in one published file that the rule matches and that is
// not a read of this repository's file -- a name looked for in ANOTHER
// repository, a branch, a glob in a fixture, a pattern that excludes.
var notARead = map[[2]string]string{
	{"Makefile", ".claude/worktrees/"}:                  "a grep -v pattern: nested worktrees are EXCLUDED from the gofmt check",
	{"cmd/gonk-gate/broker_inject.go", "CLAUDE.md"}:     "a file name probed for in the TARGET project's repository",
	{"cmd/gonk-gate/workstore_test.go", "docs/plans/"}:  "a NEGATIVE assertion: the refusal's text must not point at a plan",
	{"cmd/gonk-gate/broker_inject.go", "AGENTS.md"}:     "a file name probed for in the TARGET project's repository",
	{"pkg/opercfg/scope_test.go", "release/**"}:         "a path glob in a scope-policy fixture",
	{"pkg/scope/classify_test.go", "release/**"}:        "a path glob in a classification fixture",
	{"pkg/scope/classify_test.go", "release/notes.md"}:  "a changed path in a classification fixture",
	{"pkg/pipelinectx/anchor_test.go", "release/1.x"}:   "a branch name in a fixture",
	{"pkg/pipelinectx/fixepoch_test.go", "release/1.x"}: "a branch name in a fixture",
}

// targetProjectNames: listed paths whose name the product also uses as DATA,
// for a file of the target project. See the note above. The key is the list
// entry.
var targetProjectNames = map[string]string{
	".gitlab-ci.yml": "the target project's pipeline definition: gonk classifies it (pkg/scope), protects it (pkg/effects) " +
		"and cites it in diagnoses (cmd/gonk-gate); fixtures name it constantly and none of them opens this repository's file",
}

// internalRead is one path in a published file that points into the list.
type internalRead struct {
	File string // the published file
	Line int
	Path string // as written
	// FSCall: the path is an argument of an os, filepath, path or ioutil
	// call (Go files only). It decides whether a targetProjectNames entry is
	// a read or data, and nothing else.
	FSCall bool
}

// fsPackages are the packages whose calls take a path to open, stat or build.
var fsPackages = map[string]bool{"os": true, "filepath": true, "path": true, "ioutil": true}

// stripUp drops leading "./" and "../" segments: where a path is read FROM
// does not change what it reads.
func stripUp(p string) string {
	for {
		switch {
		case strings.HasPrefix(p, "./"):
			p = p[2:]
		case strings.HasPrefix(p, "../"):
			p = p[3:]
		default:
			return p
		}
	}
}

// pointsIntoList reports whether a path, as written in a source file, names a
// listed file or something under a listed directory.
//
// A directory entry also matches its own name without the trailing slash --
// but only when what was written is visibly a path (it has a "/" in it, as
// "docs/plans" and "../release" do). A bare word that happens to be a
// directory's name ("release": a Helm release, a CI stage) is not a path.
func pointsIntoList(list *exportguard.List, written string) bool {
	if written == "" || strings.ContainsAny(written, " \t\n") {
		return false
	}
	p := stripUp(written)
	if p == "" {
		return false
	}
	if list.Covers(p) {
		return true
	}
	if !strings.Contains(written, "/") {
		return false
	}
	for _, e := range list.Entries {
		if strings.HasSuffix(e, "/") && p == strings.TrimSuffix(e, "/") {
			return true
		}
	}
	return false
}

// goInternalReads finds the string literals and Join calls of one Go file
// that point into the list.
func goInternalReads(list *exportguard.List, name string, src []byte) ([]internalRead, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	var out []internalRead
	fsArg := map[token.Pos]bool{} // literals that are arguments of a file-system call
	add := func(pos token.Pos, written string, fsCall bool) {
		out = append(out, internalRead{name, fset.Position(pos).Line, written, fsCall})
	}
	lit := func(e ast.Expr) (string, bool) {
		b, ok := e.(*ast.BasicLit)
		if !ok || b.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(b.Value)
		return s, err == nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ImportSpec:
			return false // an import path is not a file path
		case *ast.BasicLit:
			if s, ok := lit(x); ok && pointsIntoList(list, s) {
				add(x.Pos(), s, fsArg[x.Pos()])
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || !fsPackages[pkg.Name] {
				return true
			}
			// Inspect reaches a call before its arguments, so the literals
			// among them are marked by the time they are visited.
			for _, a := range x.Args {
				if _, ok := lit(a); ok {
					fsArg[a.Pos()] = true
				}
			}
			if sel.Sel.Name != "Join" || (pkg.Name != "filepath" && pkg.Name != "path") {
				return true
			}
			// The literal arguments, in order, skipping the non-literal ones
			// (a root, a temp dir) that come before them.
			var parts []string
			for _, a := range x.Args {
				if s, ok := lit(a); ok {
					parts = append(parts, s)
				} else if len(parts) > 0 {
					break
				}
			}
			if len(parts) < 2 {
				return true // a single literal is already seen as a BasicLit
			}
			if joined := strings.Join(parts, "/"); pointsIntoList(list, joined) {
				add(x.Pos(), joined, true)
			}
		}
		return true
	})
	return out, nil
}

// pyQuoted matches a Python string literal that sits on one line.
var pyQuoted = regexp.MustCompile(`'([^'\n]*)'|"([^"\n]*)"`)

// textInternalReads finds the paths of a script, Makefile or workflow that
// point into the list.
//
// Shell, make and workflow files write paths bare, so there a path is a
// token: it ends at whitespace, a quote, or a character that cannot be part
// of a path as these files write them. Python writes paths as string
// literals and prose as docstrings, so there a path is the WHOLE content of
// a quoted string -- a docstring that cites a document is not a read.
func textInternalReads(list *exportguard.List, name string, src []byte) []internalRead {
	var out []internalRead
	python := strings.HasSuffix(name, ".py")
	split := func(r rune) bool {
		return r == ' ' || r == '\t' || strings.ContainsRune("\"'`=:;,()[]{}<>|&$\\", r)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "#") {
			continue // a comment cites; it does not read
		}
		var candidates []string
		if python {
			for _, m := range pyQuoted.FindAllStringSubmatch(line, -1) {
				candidates = append(candidates, m[1]+m[2])
			}
		} else {
			candidates = strings.FieldsFunc(line, split)
		}
		for _, tok := range candidates {
			if pointsIntoList(list, tok) {
				out = append(out, internalRead{File: name, Line: i + 1, Path: tok})
			}
		}
	}
	return out
}

// scannedForInternalReads: is this published file one whose paths are read at
// run time?
func scannedForInternalReads(path string) (goFile, ok bool) {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(path, ".go"):
		return true, true
	case strings.HasSuffix(path, ".py"), strings.HasSuffix(path, ".sh"),
		base == "Makefile", strings.HasSuffix(path, ".mk"),
		path == ".gitlab-ci.yml",
		strings.HasPrefix(path, ".github/workflows/") && (strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml")):
		return false, true
	}
	return false, false
}

func testInternalOnlyList(t *testing.T) *exportguard.List {
	t.Helper()
	l, err := exportguard.ParseList([]byte(
		".beads/          # tracker\n" +
			"CLAUDE.md        # instructions\n" +
			"docs/plans/      # plans\n" +
			"hack/dev_smoke.py # a listed script\n" +
			"release/         # the list itself\n"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The rule itself, on constructed files: each violation is found, and each
// near-miss is not.
func TestInternalReadsAreFound(t *testing.T) {
	list := testInternalOnlyList(t)
	goCases := []struct {
		name string
		src  string
		want []string // the paths reported, as written
	}{
		{"a relative read from a package", `package p
const planFile = "../../docs/plans/2026-01-01-x.md"`, []string{"../../docs/plans/2026-01-01-x.md"}},
		{"a root-relative read", `package p
var f = "docs/plans/x.md"`, []string{"docs/plans/x.md"}},
		{"a listed file", `package p
var f = "../CLAUDE.md"`, []string{"../CLAUDE.md"}},
		{"a raw string", "package p\nvar f = `./.beads/issues.jsonl`", []string{"./.beads/issues.jsonl"}},
		{"the directory itself, with and without the slash", `package p
var a, b = "docs/plans/", "docs/plans"`, []string{"docs/plans/", "docs/plans"}},
		{"a Join of literal segments", `package p
import "path/filepath"
var f = filepath.Join(root, "docs", "plans", "x.md")`, []string{"docs/plans/x.md"}},
		{"a Join that climbs first", `package p
import "path/filepath"
var f = filepath.Join("..", "..", "docs", "plans")`, []string{"../../docs/plans"}},
		{"a Join ending in a listed file", `package p
import "path"
var f = path.Join(root, "hack", "dev_smoke.py")`, []string{"hack/dev_smoke.py"}},

		{"a path cited inside a sentence is not a read", `package p
var msg = "see docs/plans/x.md section 6"`, nil},
		{"a sentence that begins with a path is not a read", `package p
var msg = "docs/plans/x.md is where the design is"`, nil},
		{"a sibling directory", `package p
var f = "docs/plansX/q.md"`, nil},
		{"a longer file name", `package p
var f = "CLAUDE.md.bak"`, nil},
		{"the same name deeper in the tree", `package p
var f = "sub/CLAUDE.md"`, nil},
		{"a published neighbour of a listed script", `package p
var f = "hack/dev_smoke.py.orig"`, nil},
		{"an import path", `package p
import _ "release/x"`, nil},
		{"a bare word that is a directory's name is not a path", `package p
var kind, stage = "release", ".beads"`, nil},
		{"the same word written as a path is", `package p
var a, b = "./release", "../.beads"`, []string{"./release", "../.beads"}},
		{"a Join of one literal is not counted twice", `package p
import "path/filepath"
var f = filepath.Join(root, "docs/plans/x.md")`, []string{"docs/plans/x.md"}},
		{"a Join whose literals are split by a variable", `package p
import "path/filepath"
var f = filepath.Join("docs", name, "plans")`, nil},
		{"another package's Join", `package p
var f = strings.Join("docs", "plans")`, nil},
	}
	for _, c := range goCases {
		got, err := goInternalReads(list, "x.go", []byte(c.src))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var paths []string
		for _, r := range got {
			paths = append(paths, r.Path)
		}
		if strings.Join(paths, "|") != strings.Join(c.want, "|") {
			t.Errorf("go: %s:\n got %q\nwant %q", c.name, paths, c.want)
		}
	}

	textCases := []struct {
		name string
		src  string
		want []string
	}{
		{"a shell read", "cat docs/plans/x.md | head\n", []string{"docs/plans/x.md"}},
		{"a bare word is not a path", "stage: release\n  needs: [release]\n", nil},
		{"a make prerequisite", "gate: ../CLAUDE.md\n", []string{"../CLAUDE.md"}},
		{"a workflow step", "      run: python3 hack/dev_smoke.py --all\n", []string{"hack/dev_smoke.py"}},
		{"an assignment", "LIST=release/internal-only.txt\n", []string{"release/internal-only.txt"}},
		{"line numbers", "a\nb\ncat .beads/issues.jsonl\n", []string{".beads/issues.jsonl"}},
		{"a comment cites", "# see docs/plans/x.md\n   # and CLAUDE.md\n", nil},
		{"a sibling", "cat docs/plansX/q.md released/notes.md\n", nil},
		{"a pathspec that EXCLUDES is still a mention of the path", "git grep x -- ':!docs/plans'\n", nil},
	}
	for _, c := range textCases {
		got := textInternalReads(list, "x.sh", []byte(c.src))
		var paths []string
		for _, r := range got {
			paths = append(paths, r.Path)
		}
		if strings.Join(paths, "|") != strings.Join(c.want, "|") {
			t.Errorf("text: %s:\n got %q\nwant %q", c.name, paths, c.want)
		}
	}
	pyCases := []struct {
		name string
		src  string
		want []string
	}{
		{"a quoted path", "PLAN = 'docs/plans/2026-x.md'\n", []string{"docs/plans/2026-x.md"}},
		{"a double-quoted path", "open(\"../CLAUDE.md\")\n", []string{"../CLAUDE.md"}},
		{"a docstring cites", "\"\"\"Break things (CLAUDE.md: seen red).\n\nsee docs/plans/x.md section 3\n\"\"\"\n", nil},
		{"a bare token is not a Python path", "x = docs/plans/x.md\n", nil},
		{"a comment cites", "# 'docs/plans/x.md'\n", nil},
		{"a sentence in quotes", "print('see docs/plans/x.md')\n", nil},
	}
	for _, c := range pyCases {
		got := textInternalReads(list, "x.py", []byte(c.src))
		var paths []string
		for _, r := range got {
			paths = append(paths, r.Path)
		}
		if strings.Join(paths, "|") != strings.Join(c.want, "|") {
			t.Errorf("python: %s:\n got %q\nwant %q", c.name, paths, c.want)
		}
	}
	if got := textInternalReads(list, "x.sh", []byte("a\nb\ncat .beads/issues.jsonl\n")); len(got) != 1 || got[0].Line != 3 || got[0].File != "x.sh" {
		t.Errorf("line and file of a text hit: %+v", got)
	}
	if got, _ := goInternalReads(list, "y.go", []byte("package p\n\nvar f = \"CLAUDE.md\"\n")); len(got) != 1 || got[0].Line != 3 || got[0].File != "y.go" {
		t.Errorf("line and file of a Go hit: %+v", got)
	}
}

func TestWhichFilesAreScannedForInternalReads(t *testing.T) {
	for path, want := range map[string][2]bool{
		"pkg/a/a.go":                   {true, true},
		"pkg/a/a_test.go":              {true, true},
		"hack/x.py":                    {false, true},
		"hack/x.sh":                    {false, true},
		"Makefile":                     {false, true},
		"sub/Makefile":                 {false, true},
		"images/common.mk":             {false, true},
		".gitlab-ci.yml":               {false, true},
		".github/workflows/ci.yml":     {false, true},
		".github/workflows/x.yaml":     {false, true},
		".github/dependabot.yml":       {false, false},
		"docs/reference/gonk-scope.md": {false, false},
		"chart/gonk/values.yaml":       {false, false},
		"pkg/a/testdata/x.json":        {false, false},
	} {
		goFile, ok := scannedForInternalReads(path)
		if goFile != want[0] || ok != want[1] {
			t.Errorf("scannedForInternalReads(%q) = %v, %v; want %v, %v", path, goFile, ok, want[0], want[1])
		}
	}
}

// internalReadReport is what one check of a tree found.
type internalReadReport struct {
	Public    bool           // an exported tree: nothing to check against
	Found     []internalRead // sorted by file, then line
	Stale     []string       // exemptions that matched nothing
	GoFiles   int            // published Go files scanned
	TextFiles int            // published scripts, Makefiles and workflows scanned
}

// internalReadCheck checks the tree at root. tracked returns the tree's
// tracked paths (slash-separated, relative to root); it is called only for an
// internal tree. minGo and minText are the fewest files of each kind a real
// scan must have seen: below that the walk looked in the wrong place, and a
// check that scanned nothing must not report "clean".
func internalReadCheck(root string, tracked func() ([]string, error), fileExempt map[string]string,
	pathExempt map[[2]string]string, dataNames map[string]string, minGo, minText int) (internalReadReport, error) {
	var rep internalReadReport
	_, srcErr := os.Stat(filepath.Join(root, exportguard.ExportSourcePath))
	_, dataErr := os.Stat(filepath.Join(root, exportguard.SelfDir))
	mode, err := exportguard.DetectMode(srcErr == nil, dataErr == nil)
	if err != nil {
		return rep, fmt.Errorf("cannot tell which tree this is: %w", err)
	}
	if mode == exportguard.ModePublic {
		// The list is not published, and does not need to be: in an exported
		// tree the listed paths do not exist, so a read of one fails by
		// itself. There is nothing here to check against.
		rep.Public = true
		return rep, nil
	}
	data, err := os.ReadFile(filepath.Join(root, exportguard.ListPath))
	if err != nil {
		return rep, fmt.Errorf("read the internal-only list: %w", err)
	}
	list, err := exportguard.ParseList(data)
	if err != nil {
		return rep, fmt.Errorf("the internal-only list: %w", err)
	}
	paths, err := tracked()
	if err != nil {
		return rep, err
	}
	exemptFor := func(path string) string {
		if _, ok := fileExempt[path]; ok {
			return path
		}
		for k := range fileExempt {
			if strings.HasSuffix(k, "/") && strings.HasPrefix(path, k) {
				return k
			}
		}
		return ""
	}
	fileHit := map[string]bool{}
	pathHit := map[[2]string]bool{}
	dataHit := map[string]bool{}
	for _, path := range paths {
		if path == "" || strings.HasPrefix(path, "vendor/") || list.Covers(path) {
			continue
		}
		goFile, ok := scannedForInternalReads(path)
		if !ok {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return rep, fmt.Errorf("read %s: %w", path, err)
		}
		var hits []internalRead
		if goFile {
			rep.GoFiles++
			if hits, err = goInternalReads(list, path, src); err != nil {
				return rep, fmt.Errorf("parse %s: %w", path, err)
			}
		} else {
			rep.TextFiles++
			hits = textInternalReads(list, path, src)
		}
		if key := exemptFor(path); key != "" {
			if len(hits) > 0 {
				fileHit[key] = true
			}
			continue
		}
		for _, h := range hits {
			// A name the product uses as data is a read, in a Go file, only
			// where the file system is asked for it.
			if entry := stripUp(h.Path); goFile && !h.FSCall && dataNames[entry] != "" {
				dataHit[entry] = true
				continue
			}
			key := [2]string{h.File, h.Path}
			if _, ok := pathExempt[key]; ok {
				pathHit[key] = true
				continue
			}
			rep.Found = append(rep.Found, h)
		}
	}
	if rep.GoFiles < minGo || rep.TextFiles < minText {
		return rep, fmt.Errorf("scanned %d Go files and %d scripts/workflows, fewer than %d and %d: the walk is looking in the wrong place",
			rep.GoFiles, rep.TextFiles, minGo, minText)
	}
	sort.Slice(rep.Found, func(i, j int) bool {
		if rep.Found[i].File != rep.Found[j].File {
			return rep.Found[i].File < rep.Found[j].File
		}
		return rep.Found[i].Line < rep.Found[j].Line
	})
	for path, reason := range fileExempt {
		if !fileHit[path] {
			rep.Stale = append(rep.Stale, fmt.Sprintf("internalReadExempt[%q] (%s)", path, reason))
		}
	}
	for key, reason := range pathExempt {
		if !pathHit[key] {
			rep.Stale = append(rep.Stale, fmt.Sprintf("notARead[%s, %q] (%s)", key[0], key[1], reason))
		}
	}
	for entry, reason := range dataNames {
		// It must be a FILE on the list (a directory's name is never data),
		// and it must have excused something.
		onList := false
		for _, e := range list.Entries {
			onList = onList || e == entry
		}
		if !onList || strings.HasSuffix(entry, "/") || !dataHit[entry] {
			rep.Stale = append(rep.Stale, fmt.Sprintf("targetProjectNames[%q] (%s)", entry, reason))
		}
	}
	sort.Strings(rep.Stale)
	return rep, nil
}

// scratchTree writes files under a temp dir and returns the dir and a
// `tracked` that lists them.
func scratchTree(t *testing.T, files map[string]string) (string, func() ([]string, error)) {
	t.Helper()
	root := t.TempDir()
	var paths []string
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return root, func() ([]string, error) { return paths, nil }
}

const scratchList = ".beads/      # tracker\ndocs/plans/  # plans\nCLAUDE.md    # instructions\nrelease/     # the list itself\n"

func scratchInternal() map[string]string {
	return map[string]string{
		"release/internal-only.txt": scratchList,
		"docs/plans/p.md":           "a plan\n",
		"docs/plans/tool_test.go":   "package plans\nvar f = \"../../CLAUDE.md\" // a listed file may read a listed file\n",
		"vendor/v/v.go":             "package v\nvar f = \"docs/plans/p.md\" // not ours\n",
		"pkg/a/a.go":                "package a\nvar ok = \"docs/reference/x.md\"\n",
		"pkg/a/a_test.go":           "package a\nconst plan = \"../../docs/plans/p.md\"\n",
		"pkg/b/b_test.go":           "package b\nvar branch = \"release/1.x\"\nvar read = \"../../release/allow.txt\"\n",
		"tool/t.go":                 "package tool\nvar list = \"release/internal-only.txt\"\n",
		"hack/run.sh":               "#!/bin/sh\ncat .beads/issues.jsonl\n",
		"hack/ok.py":                "X = 'docs/reference/x.md'\n",
		"docs/guide.md":             "see docs/plans/p.md\n",
	}
}

func readPaths(rs []internalRead) []string {
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%s:%d:%s", r.File, r.Line, r.Path))
	}
	return out
}

// The whole check, on a scratch tree: what is reported, what an exemption
// excuses and exactly how far, and that an exemption which excuses nothing is
// reported too.
func TestInternalReadCheckOnAScratchTree(t *testing.T) {
	root, tracked := scratchTree(t, scratchInternal())
	fileExempt := map[string]string{"tool/": "the tool"}
	pathExempt := map[[2]string]string{{"pkg/b/b_test.go", "release/1.x"}: "a branch"}

	rep, err := internalReadCheck(root, tracked, fileExempt, pathExempt, nil, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Public {
		t.Fatal("an internal tree was taken for an exported one")
	}
	// Reported: the test that reads a plan, the OTHER path of the file with a
	// path exemption, the script. Not reported: the listed file, vendor/, the
	// exempt directory, the exempt path, the markdown file.
	want := []string{
		"hack/run.sh:2:.beads/issues.jsonl",
		"pkg/a/a_test.go:2:../../docs/plans/p.md",
		"pkg/b/b_test.go:3:../../release/allow.txt",
	}
	if got := readPaths(rep.Found); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("found:\n got %q\nwant %q", got, want)
	}
	if len(rep.Stale) != 0 {
		t.Errorf("stale exemptions reported though each matched: %q", rep.Stale)
	}
	if rep.GoFiles != 4 || rep.TextFiles != 2 {
		t.Errorf("scanned %d Go files and %d text files, want 4 (a.go, a_test.go, b_test.go, t.go) and 2 (run.sh, ok.py)", rep.GoFiles, rep.TextFiles)
	}

	// The right path in the wrong file is not excused, and both exemptions
	// are then stale: neither matched anything.
	rep, err = internalReadCheck(root, tracked,
		map[string]string{"tools/": "a directory that is not there", "pkg/a/a.go": "a file with no hit"},
		map[[2]string]string{{"pkg/a/a_test.go", "release/1.x"}: "the wrong file"}, nil, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"hack/run.sh:2:.beads/issues.jsonl",
		"pkg/a/a_test.go:2:../../docs/plans/p.md",
		"pkg/b/b_test.go:2:release/1.x",
		"pkg/b/b_test.go:3:../../release/allow.txt",
		"tool/t.go:2:release/internal-only.txt",
	}
	if got := readPaths(rep.Found); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("found, with exemptions that match nothing:\n got %q\nwant %q", got, want)
	}
	wantStale := []string{
		`internalReadExempt["pkg/a/a.go"] (a file with no hit)`,
		`internalReadExempt["tools/"] (a directory that is not there)`,
		`notARead[pkg/a/a_test.go, "release/1.x"] (the wrong file)`,
	}
	if strings.Join(rep.Stale, "|") != strings.Join(wantStale, "|") {
		t.Errorf("stale:\n got %q\nwant %q", rep.Stale, wantStale)
	}
}

// A listed name that the product also uses as data: in a Go file it is a read
// only as an argument of a file-system call. Everything else about the check
// is unchanged -- a script still reads it, and an entry that is not a listed
// file or excuses nothing is reported.
func TestADataNameIsAReadOnlyWhereTheFileSystemIsAsked(t *testing.T) {
	files := scratchInternal()
	files["release/internal-only.txt"] = scratchList + "ci.yml       # the private pipeline\n"
	files["ci.yml"] = "stages: [test]\n"
	files["pkg/c/data.go"] = "package c\nvar protected = map[string]bool{\"ci.yml\": true, \"./ci.yml\": false}\n" +
		"func f() { put(\"ci.yml\", nil); proj.PutFile(\"ci.yml\") }\n"
	files["pkg/c/read_test.go"] = "package c\nimport (\"os\"; \"path/filepath\")\n" +
		"var a, _ = os.ReadFile(\"../../ci.yml\")\n" +
		"var b = filepath.Join(root(), \"ci.yml\")\n" +
		"var c, _ = os.Stat(\"ci.yml\")\n"
	files["hack/ci.sh"] = "#!/bin/sh\ngrep -c stages ci.yml\n"
	root, tracked := scratchTree(t, files)
	data := map[string]string{"ci.yml": "the target project's pipeline"}

	// Without the entry every literal is reported: this is the rule the
	// entry relaxes, seen first.
	rep, err := internalReadCheck(root, tracked, nil, nil, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	strict := 0
	for _, r := range rep.Found {
		if strings.HasSuffix(r.Path, "ci.yml") {
			strict++
		}
	}
	if strict != 8 {
		t.Fatalf("with no data name, want all 8 mentions of ci.yml reported (4 data, 3 reads, 1 script), got %d: %q", strict, readPaths(rep.Found))
	}

	rep, err = internalReadCheck(root, tracked, nil, nil, data, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rep.Found {
		if strings.HasSuffix(r.Path, "ci.yml") {
			got = append(got, fmt.Sprintf("%s:%d:%s", r.File, r.Line, r.Path))
		}
	}
	want := []string{
		"hack/ci.sh:2:ci.yml",
		"pkg/c/read_test.go:3:../../ci.yml",
		"pkg/c/read_test.go:4:ci.yml",
		"pkg/c/read_test.go:5:ci.yml",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("with the data name:\n got %q\nwant %q", got, want)
	}
	if len(rep.Stale) != 0 {
		t.Errorf("stale reported though the entry excused the data literals: %q", rep.Stale)
	}

	// Stale: a name that is not on the list, a directory, and one that
	// excused nothing.
	rep, err = internalReadCheck(root, tracked, nil, nil,
		map[string]string{"nope.yml": "not listed", "docs/plans/": "a directory", "CLAUDE.md": "listed, never a bare literal here"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantStale := []string{
		`targetProjectNames["CLAUDE.md"] (listed, never a bare literal here)`,
		`targetProjectNames["docs/plans/"] (a directory)`,
		`targetProjectNames["nope.yml"] (not listed)`,
	}
	if strings.Join(rep.Stale, "|") != strings.Join(wantStale, "|") {
		t.Errorf("stale:\n got %q\nwant %q", rep.Stale, wantStale)
	}
}

// A scan that saw too few files reports that, not "clean".
func TestInternalReadCheckRefusesAScanThatSawTooLittle(t *testing.T) {
	root, tracked := scratchTree(t, scratchInternal())
	for _, floor := range [][2]int{{5, 2}, {4, 3}} {
		_, err := internalReadCheck(root, tracked, nil, nil, nil, floor[0], floor[1])
		if err == nil || !strings.Contains(err.Error(), "looking in the wrong place") {
			t.Errorf("floors %v on a tree with 4 Go and 2 text files: err = %v", floor, err)
		}
	}
	if _, err := internalReadCheck(root, tracked, nil, nil, nil, 4, 2); err != nil {
		t.Errorf("floors equal to what the tree holds: %v", err)
	}
}

// Which tree is this? An exported tree has no list and is not scanned; a tree
// that is neither, or both, is an error and never "clean".
func TestInternalReadCheckKnowsWhichTreeItIsIn(t *testing.T) {
	never := func() ([]string, error) {
		t.Error("an exported tree was listed: it has no list to check against")
		return nil, nil
	}
	public := scratchInternal()
	for name := range public {
		if strings.HasPrefix(name, "release/") || strings.HasPrefix(name, "docs/plans/") {
			delete(public, name)
		}
	}
	public[exportguard.ExportSourcePath] = "generated\n"
	root, _ := scratchTree(t, public)
	rep, err := internalReadCheck(root, never, nil, nil, nil, 300, 20)
	if err != nil || !rep.Public || len(rep.Found) != 0 {
		t.Errorf("an exported tree: rep = %+v, err = %v; want Public and nothing else", rep, err)
	}

	both := scratchInternal()
	both[exportguard.ExportSourcePath] = "generated\n"
	root, tracked := scratchTree(t, both)
	if _, err := internalReadCheck(root, tracked, nil, nil, nil, 0, 0); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("a tree with EXPORT-SOURCE and release/: err = %v, want a refusal", err)
	}

	neither := scratchInternal()
	delete(neither, "release/internal-only.txt")
	root, tracked = scratchTree(t, neither)
	if _, err := internalReadCheck(root, tracked, nil, nil, nil, 0, 0); err == nil || !strings.Contains(err.Error(), "neither") {
		t.Errorf("a tree with neither: err = %v, want a refusal", err)
	}

	broken := scratchInternal()
	broken["release/internal-only.txt"] = "docs/plans/\n" // no reason, no release/ entry
	root, tracked = scratchTree(t, broken)
	if _, err := internalReadCheck(root, tracked, nil, nil, nil, 0, 0); err == nil || !strings.Contains(err.Error(), "internal-only list") {
		t.Errorf("a list that does not parse: err = %v, want a refusal", err)
	}
}

// The real tree.
func TestNoPublicFileReadsAnInternalOnlyPath(t *testing.T) {
	root := repoRoot(t)
	tracked := func() ([]string, error) {
		out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-files: %w (the question is what is TRACKED, so git is required)", err)
		}
		return strings.Split(strings.TrimRight(string(out), "\x00"), "\x00"), nil
	}
	// Not vacuous: the tree has hundreds of Go files and dozens of scripts.
	rep, err := internalReadCheck(root, tracked, internalReadExempt, notARead, targetProjectNames, 300, 20)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Public {
		t.Logf("exported tree: the internal-only list is not here to check against; a read of an unpublished path fails on its own in this tree")
		return
	}
	for _, r := range rep.Found {
		t.Errorf("%s:%d reads %q, which is on %s and is NOT in the published tree. "+
			"Move what it needs into a published file (docs/reference/ for a rule a test pins), "+
			"or, if this is not a read of this repository's file at all, add it to notARead with the reason.",
			r.File, r.Line, r.Path, exportguard.ListPath)
	}
	for _, s := range rep.Stale {
		t.Errorf("%s matches nothing any more: remove it", s)
	}
}
