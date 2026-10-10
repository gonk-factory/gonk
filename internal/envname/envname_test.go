package envname

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is the module root, found from this file's own path.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("computed repo root %s has no go.mod: %v", root, err)
	}
	return root
}

// declFile is the one file allowed to contain these names as literals.
const declFile = "envname.go"

// foldString evaluates a constant string expression as far as the syntax tree
// alone allows: a literal, a parenthesised one, and any `+` tree whose leaves
// are all string literals.
//
// THE `+` CASE IS NOT HYPOTHETICAL. The first version of this test compared
// whole *ast.BasicLit values, and verification of 0d5e6a7 broke it in one line:
//
//	const podReapNamespaceEnv = "GONK_POD_REAP" + "_NAMESPACE"
//
// Neither half is the name, so nothing matched, the envname constant and the
// chart template were then renamed together, and `make gate` came back exit 0
// with 38 ok packages, 0 lint issues -- while the chart set GONK_REAPER_NS, the
// binary read GONK_POD_REAP_NAMESPACE, newPodReaper("") returned nil and the
// pod reaper never armed. That is the gonk-b6pb failure itself, walking straight
// through the guard built to stop it.
//
// This closes the spelling. The CLASS is closed in cmd/gonk-gate by
// TestPodReapNamespaceEnvIsTheSharedConstant, which compares the values the
// compiler computed and so does not care how the string was assembled.
func foldString(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(x.Value)
		if err != nil {
			return "", false
		}
		return v, true
	case *ast.ParenExpr:
		return foldString(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok := foldString(x.X)
		if !ok {
			return "", false
		}
		r, ok := foldString(x.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

// THE POINT OF THIS PACKAGE, ENFORCED.
//
// The chart tests join the rendered manifests to these constants, which makes a
// one-sided rename go red -- but ONLY while the constants are what the binaries
// actually read. An os.Getenv("GONK_POD_REAP_NAMESPACE") re-inlined anywhere
// under cmd/ silently detaches the binary from the thing the chart is checked
// against, and restores exactly the hole gonk-b6pb was filed for: the reaper
// reads a variable nobody sets, arms nothing, and looks like a cluster with no
// leaks.
//
// So: outside envname.go, these strings may not appear as Go string constant
// expressions at all -- not as a literal and not assembled from literals.
// Comments are fine: this reads the parsed syntax tree, not the bytes, precisely
// so prose explaining the variable is not collateral damage.
func TestNoGoSourceRepeatsAnEnvNameLiteral(t *testing.T) {
	root := repoRoot(t)
	want := map[string]bool{}
	for _, n := range All {
		want[n] = true
	}

	type hit struct {
		file string
		line int
		name string
	}
	var hits []hit
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		if rel == filepath.Join("internal", "envname", declFile) {
			return nil
		}

		fset := token.NewFileSet()
		// Build tags are irrelevant here: parsing is unconditional, so a
		// `//go:build chart` file is read on an ordinary `go test ./...` too.
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			v, ok := foldString(e)
			if !ok {
				return true
			}
			if want[v] {
				hits = append(hits, hit{rel, fset.Position(e.Pos()).Line, v})
				// Do not descend: the halves of a `+` are not themselves the
				// name, and reporting them as well would only add noise.
				return false
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// A walk that found nothing to read would report a clean tree forever.
	if scanned < 100 {
		t.Fatalf("only %d .go files parsed under %s; the walk is broken and this test "+
			"would accept any duplication at all", scanned, root)
	}

	for _, h := range hits {
		t.Errorf("%s:%d spells out the env name %q as a string constant expression.\n"+
			"Use the constant in internal/envname instead: the chart tests assert the rendered "+
			"manifests against THAT, so a literal here is a binary reading a variable no chart "+
			"assertion covers -- the silent-disarm shape of gonk-b6pb and gonk-8gb.",
			h.file, h.line, h.name)
	}
}

// All must list every constant declared here, or a test that walks All quietly
// stops covering the newest name -- the failure mode being a new env var that
// no chart assertion joins, which is the whole thing this package prevents.
//
// Read out of the source rather than compared against a second hand-written
// list, which would only move the problem.
func TestAllListsEveryDeclaredEnvName(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(thisFile), declFile)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var declared []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				uq, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					continue
				}
				declared = append(declared, uq)
			}
		}
	}
	if len(declared) == 0 {
		t.Fatalf("found no string constants in %s; the parse is broken and this test "+
			"would accept an empty All", src)
	}

	got := append([]string{}, All...)
	sort.Strings(got)
	sort.Strings(declared)
	if strings.Join(got, ",") != strings.Join(declared, ",") {
		t.Fatalf("All = %v but %s declares %v.\nEvery declared env name must be in All, "+
			"or the tests that walk All silently skip it.", got, declFile, declared)
	}
}
