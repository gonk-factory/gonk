package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/envname"
)

// THE BINARY HALF OF THE CHART JOIN (gonk-b6pb).
//
// internal/charttest asserts the RENDERED manifest against envname's constants.
// That is only worth anything while THIS binary reads the same constants, and
// verification of 0d5e6a7 proved the first attempt at guarding that did not
// hold. The attack was one line, and every step after it is what an author
// doing a routine rename would do anyway:
//
//	const podReapNamespaceEnv = "GONK_POD_REAP" + "_NAMESPACE"
//	// then rename envname.PodReapNamespace AND the template together,
//	// make chart-goldens, bump Chart.yaml, make chart-seal
//
// `make gate` came back exit 0, 38 ok packages, 0 lint issues -- with the chart
// setting GONK_REAPER_NS, this binary reading GONK_POD_REAP_NAMESPACE,
// newPodReaper("") returning nil and runPodReap returning at its first line. A
// pod reaper that never arms is indistinguishable from a cluster with no leaks,
// which is the whole reason gonk-b6pb is a bug and not a style note.
//
// The guard it walked through was a syntax-tree search for the name as a
// literal, and the assembled string was not one. So the guard here does not
// look at how the string is SPELLED at all: it compares the values the compiler
// computed. Concatenation, a chain of intermediate constants, a rune slice put
// back together -- all of them produce a value, and the value is what is
// checked.
//
// A NOTE ON THE COMPARISON LOOKING TRIVIAL. While reap_pods.go says
// `= envname.PodReapNamespace`, this can never fail, and that is exactly the
// shape gonk-3nir was filed against. The difference is what is being compared.
// gonk-3nir's test declared its own copy of a string and then inspected that
// copy -- there was no second party, so no edit anywhere could make it
// disagree. This compares TWO INDEPENDENTLY EDITABLE DECLARATIONS in two
// packages, and the edit that makes them disagree is a real one that a real
// reviewer really made. Proven, not asserted: the attack above turns this red.
func TestPodReapNamespaceEnvIsTheSharedConstant(t *testing.T) {
	if podReapNamespaceEnv != envname.PodReapNamespace {
		t.Fatalf("cmd/gonk-gate reads %q but the chart is checked against %q.\n"+
			"These are the same variable or the pod reaper does not arm: gonk-gate would call "+
			"os.Getenv on a name the chart never sets, newPodReaper(\"\") returns nil, and every "+
			"sweep tick reaps no pods and says nothing about it -- which looks exactly like a "+
			"cluster with no leaks (gonk-b6pb).\n"+
			"Fix reap_pods.go to define podReapNamespaceEnv from envname.PodReapNamespace, or "+
			"rename BOTH the constant and chart/gonk/templates/workload-gonk-controller.yaml.",
			podReapNamespaceEnv, envname.PodReapNamespace)
	}
}

// AND THE READ MUST STILL BE THERE, THROUGH THE CONSTANT.
//
// The comparison above is satisfied by a constant nothing uses. This asserts
// that cmd/gonk-gate actually calls os.Getenv on the shared names -- and does
// it over the syntax tree, not the file text, so a commented-out call or a
// mention in prose does not satisfy it. (The earlier version of this check was
// a regexp over the raw source and would have.)
func TestGonkGateReadsItsEnvNamesThroughEnvname(t *testing.T) {
	want := map[string][]string{
		// name -> the argument spellings that are acceptable for it.
		//
		// PodReapNamespace is read AGAIN (gonk-t55 plan 3.4 C1): it arms the
		// sweep's legacy Gas City pod pass (sweep_legacy.go), which deletes
		// the pods of sessions in flight at the cutover with the pod reaper's
		// list/delete. The Gas City reaper itself stays off the sweep (D7).
		// A one-sided rename would silently disarm that cleanup.
		"PodReapNamespace": {"podReapNamespaceEnv", "envname.PodReapNamespace"},
		"KeysinkNamespace": {"envname.KeysinkNamespace"},
	}
	found := map[string]int{}
	var seen []string

	for _, f := range gonkGateFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Getenv" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
				return true
			}
			arg := exprName(call.Args[0])
			seen = append(seen, arg)
			for k, spellings := range want {
				for _, s := range spellings {
					if arg == s {
						found[k]++
					}
				}
			}
			return true
		})
	}

	sort.Strings(seen)
	for k, spellings := range want {
		if found[k] == 0 {
			t.Errorf("no os.Getenv(%s) in cmd/gonk-gate.\n"+
				"Either the read was deleted -- in which case that feature is off and the chart "+
				"assertion for it is checking a variable nothing consumes -- or it was changed to "+
				"a name envname does not own, which detaches this binary from every chart test "+
				"that guards it (gonk-b6pb).\n"+
				"os.Getenv arguments actually found: %s",
				strings.Join(spellings, " / "), strings.Join(seen, ", "))
		}
	}
	if len(seen) == 0 {
		t.Fatal("found no os.Getenv calls at all in cmd/gonk-gate; the walk is broken and this " +
			"check would pass against a binary that reads nothing")
	}
}

// exprName renders an os.Getenv argument as source-like text, for matching and
// for the failure message. Anything it does not understand comes back as a
// description rather than "", so the message names what it saw.
func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok {
			return pkg.Name + "." + x.Sel.Name
		}
		return "?." + x.Sel.Name
	case *ast.BasicLit:
		return x.Value
	case *ast.BinaryExpr:
		return exprName(x.X) + " " + x.Op.String() + " " + exprName(x.Y)
	case *ast.ParenExpr:
		return "(" + exprName(x.X) + ")"
	}
	return "<expr>"
}

// gonkGateFiles parses every non-test .go file in this package.
func gonkGateFiles(t *testing.T) []*ast.File {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", n, perr)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatalf("no non-test .go files found in %s", dir)
	}
	return out
}
