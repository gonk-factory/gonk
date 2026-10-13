package scope

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// 066 invariant 3: scope policy is read ONLY from the baked pack, the operator
// config, and .gonk.yml at the pinned base SHA -- never from the batch, the
// session's tree, `.agent/` or issue text.
//
// This package cannot do otherwise by construction, and these tests keep the
// construction: it imports no gonk package (so it cannot name a Batch, a
// session or a forge client), it opens exactly one file (the pack's
// effect-shape.toml, in LoadRow), and the only time it says `.agent` is the
// pattern that CLASSIFIES that directory.

type srcFile struct {
	name string
	file *ast.File
}

func packageSources(t *testing.T) []srcFile {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []srcFile
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, srcFile{n, f})
	}
	if len(out) < 5 {
		t.Fatalf("parsed %d source files; the guard is looking in the wrong place", len(out))
	}
	return out
}

func TestScopeImportsNothingThatCouldCarryABatch(t *testing.T) {
	allowed := map[string]bool{
		"bytes": true, "errors": true, "fmt": true, "log/slog": true, "os": true, "path": true, "path/filepath": true,
		"regexp": true, "strings": true,
		// io: for io.EOF alone, the end of a multi-document YAML stream
		// (ciLocalIncludes reads every document of a CI file). It reads from
		// nowhere by itself.
		"io":                         true,
		"github.com/BurntSushi/toml": true, "gopkg.in/yaml.v3": true,
	}
	seen := 0
	for _, sf := range packageSources(t) {
		for _, imp := range sf.file.Imports {
			seen++
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "github.com/gonk-factory/gonk/") {
				t.Errorf("%s imports %s: pkg/scope must import no gonk package -- it would be able to see a batch, a session or the forge, and policy must come from none of them", sf.name, p)
				continue
			}
			if !allowed[p] {
				t.Errorf("%s imports %s, which is not on pkg/scope's allowlist; a new import here is a new place policy could be read from -- justify it and add it", sf.name, p)
			}
		}
	}
	if seen < 8 {
		t.Fatalf("saw %d imports; the guard is vacuous", seen)
	}
}

func TestScopeOpensOnlyThePackFile(t *testing.T) {
	// Every selector on a package that can reach the filesystem, by file.
	type use struct{ file, fn, sel string }
	var uses []use
	for _, sf := range packageSources(t) {
		for _, decl := range sf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			name := "(package level)"
			if ok {
				name = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch pkg.Name {
				case "os", "toml", "filepath", "ioutil", "fs", "exec", "http", "embed":
					uses = append(uses, use{sf.name, name, pkg.Name + "." + sel.Sel.Name})
				}
				return true
			})
		}
	}
	want := map[string]bool{
		"pack.go LoadRow filepath.Join":   true,
		"pack.go LoadRow toml.DecodeFile": true,
		"pack.go LoadRow os.IsNotExist":   true,
	}
	got := map[string]bool{}
	for _, u := range uses {
		key := u.file + " " + u.fn + " " + u.sel
		got[key] = true
		if !want[key] {
			t.Errorf("%s: %s uses %s -- pkg/scope reads ONE file, the baked pack's effect-shape.toml, in LoadRow. Policy from anywhere else (the session tree, .agent/, a batch) is the self-widening hole gonk-066 closes", u.file, u.fn, u.sel)
		}
	}
	for key := range want {
		if !got[key] {
			t.Errorf("expected %q; LoadRow changed shape and this guard no longer describes it", key)
		}
	}

	// And that one file is the pack's: the path LoadRow opens is built from
	// exactly these literal elements.
	for _, sf := range packageSources(t) {
		if sf.name != "pack.go" {
			continue
		}
		ast.Inspect(sf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Join" {
				return true
			}
			var parts []string
			for _, a := range call.Args {
				switch v := a.(type) {
				case *ast.Ident:
					parts = append(parts, v.Name)
				case *ast.BasicLit:
					parts = append(parts, v.Value)
				default:
					parts = append(parts, "?")
				}
			}
			if got := strings.Join(parts, ","); got != `packDir,"agents",agent,"effect-shape.toml"` {
				t.Errorf("LoadRow opens filepath.Join(%s); want (packDir, \"agents\", agent, \"effect-shape.toml\")", got)
			}
			return true
		})
	}
}

func TestScopeNeverReadsAgentDir(t *testing.T) {
	var lits []string
	for _, sf := range packageSources(t) {
		ast.Inspect(sf.file, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && strings.Contains(bl.Value, ".agent") {
				lits = append(lits, sf.name+": "+bl.Value)
			}
			return true
		})
	}
	if len(lits) != 1 || lits[0] != `class.go: ".agent/**"` {
		t.Errorf("string literals naming .agent in pkg/scope: %q\nwant exactly one: the agent-context class pattern in class.go. Nothing scope-relevant is read from .agent/ -- it is agent-writable (066 finding F6)", lits)
	}
}

// What a fold reads is its Input, and Input can hold only this package's own
// types, strings and bools: there is no field a batch, an effect or a
// transcript could be passed in.
func TestFoldInputHoldsOnlyPolicyTypes(t *testing.T) {
	var check func(rt reflect.Type, where string)
	n := 0
	check = func(rt reflect.Type, where string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice {
			rt = rt.Elem()
		}
		switch rt.Kind() {
		case reflect.String, reflect.Bool, reflect.Int64:
			n++
			return
		case reflect.Struct:
			if rt.PkgPath() != reflect.TypeOf(Input{}).PkgPath() {
				t.Errorf("%s has foreign type %s", where, rt)
				return
			}
			for i := 0; i < rt.NumField(); i++ {
				check(rt.Field(i).Type, where+"."+rt.Field(i).Name)
			}
		default:
			t.Errorf("%s has kind %s; Input must stay plain policy data", where, rt.Kind())
		}
	}
	check(reflect.TypeOf(Input{}), "Input")
	if n < 20 {
		t.Fatalf("walked %d leaf fields; the guard is vacuous", n)
	}
}
