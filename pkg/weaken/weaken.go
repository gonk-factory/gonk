// Package weaken is the deterministic test-weakening predicate SC-WEAKEN for
// Go (gonk-066 S5; gonk-2usw plan 4.2, 5.3 A6, cheat C8): given one file's
// content before and after a proposed change, it says whether the change
// removes a test, adds a skip, or changes which builds the file is part of.
//
// It is PURE: bytes in, a verdict out. It reads no policy and no path class;
// the caller decides which files to ask about (pkg/scope's
// Decision.WeakeningDecidable says for which paths an answer EXISTS -- Go, and
// only Go).
//
// WHAT IT DECIDES, and nothing more:
//
//	removed-test      a top-level func named Test*, Benchmark*, Fuzz* or
//	                  Example* present before is absent after (deleted, or
//	                  renamed so `go test` no longer runs it), or the file is
//	                  no longer a _test.go-shaped package member at all
//	added-skip        a function has MORE calls to a method named Skip, Skipf
//	                  or SkipNow after than before (t.Skip, b.Skipf, ...), or
//	                  the FIRST statement of a test function became an early
//	                  `return`: a bare one, or an `if` with a branch that is
//	                  nothing but `return` (`if testing.Short() { return }`),
//	                  either of them directly or leading a bare block. A
//	                  guarded return that was already there is not a
//	                  finding; one made unconditional is
//	build-constraint  the file's build constraints differ (//go:build and
//	                  // +build lines, and a leading `package ... // import`
//	                  is not one): a tag can take the whole file, and every
//	                  test in it, out of the build
//	unparseable       the content after the change is not Go that parses: no
//	                  claim about its tests can be checked, and it cannot
//	                  compile either
//
// WHAT IT DOES NOT DECIDE (plan 6.5 U4, cheat C15): whether an assertion
// inside a surviving test was weakened, whether a table lost its failing row,
// whether an expected value was edited to match the bug. Those need a human.
package weaken

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// Sub codes of an SC-WEAKEN finding.
const (
	RemovedTest     = "removed-test"
	AddedSkip       = "added-skip"
	BuildConstraint = "build-constraint"
	Unparseable     = "unparseable"
)

// Finding is one way a change weakens the tests. The zero value means none.
type Finding struct {
	// Sub is one of the constants above, "" when nothing was found.
	Sub string
	// Detail is for a person and for the session's feedback. It names
	// functions, never file content.
	Detail string
}

// Found reports whether the change weakens the tests.
func (f Finding) Found() bool { return f.Sub != "" }

// Go judges one Go file. base is its content before the change, patched its
// content after. A base that does not parse gives no baseline to compare
// against: then only "the result parses" and the build constraints are
// checked (constraints are read from comments and need no parse of the body).
func Go(base, patched []byte) Finding {
	fset := token.NewFileSet()
	after, err := parser.ParseFile(fset, "after.go", patched, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return Finding{Sub: Unparseable, Detail: "the file does not parse as Go after the change"} // weaken:unparseable
	}
	if bc, ac := constraints(base), constraints(patched); bc != ac { // weaken:constraint
		return Finding{Sub: BuildConstraint, Detail: fmt.Sprintf("the file's build constraints change from %q to %q", bc, ac)}
	}
	before, err := parser.ParseFile(fset, "before.go", base, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return Finding{}
	}
	bf, af := testFuncs(before), testFuncs(after)
	var gone []string
	for name := range bf {
		if _, ok := af[name]; !ok { // weaken:removed
			gone = append(gone, name)
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		return Finding{Sub: RemovedTest, Detail: "the change removes or renames " + strings.Join(gone, ", ")}
	}
	// Skips are counted per top-level function, over EVERY function of the
	// file: a helper that every test calls is where a skip hides best.
	bs, as := skipCounts(before), skipCounts(after)
	var skipped []string
	for name, n := range as {
		if n > bs[name] { // weaken:skip
			skipped = append(skipped, name)
		}
	}
	for name, fn := range af {
		if old, ok := bf[name]; ok && leadingReturn(fn) > leadingReturn(old) { // weaken:return
			skipped = append(skipped, name)
		}
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		return Finding{Sub: AddedSkip, Detail: "the change adds a skip to " + strings.Join(dedup(skipped), ", ")}
	}
	return Finding{}
}

func dedup(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// testFuncs returns the file's top-level functions `go test` runs, by name.
func testFuncs(f *ast.File) map[string]*ast.FuncDecl {
	out := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
			if strings.HasPrefix(fn.Name.Name, prefix) {
				out[fn.Name.Name] = fn
				break
			}
		}
	}
	return out
}

// skipCounts counts, per top-level function (methods as "Recv.Name"), the
// calls to a method named Skip, Skipf or SkipNow anywhere in its body,
// closures included.
func skipCounts(f *ast.File) map[string]int {
	out := map[string]int{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			name = recvName(fn.Recv.List[0].Type) + "." + name
		}
		n := 0
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Skip", "Skipf", "SkipNow":
					n++
				}
			}
			return true
		})
		if n > 0 {
			out[name] = n
		}
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	}
	return "?"
}

// How much of a test its first statement switches off without calling Skip.
// Ordered: a change is a finding when the rank RISES.
const (
	noReturn      = iota // the body is reached
	guardedReturn        // `if <cond> { return }`: off whenever cond holds
	bareReturn           // `return`: off always
)

// leadingReturn ranks a function by its first statement.
func leadingReturn(fn *ast.FuncDecl) int {
	if fn.Body == nil {
		return noReturn
	}
	return returnRank(fn.Body)
}

// returnRank ranks the first statement of a block. Only the SHAPE is read,
// never the condition: `if true { return }` and `if testing.Short() { return }`
// are the same thing to it, and so is whatever else the `if` carries. A
// branch that does anything before it returns (t.Fatal(...); return) is not
// one: that is a test failing early, which is the opposite of a skip.
func returnRank(b *ast.BlockStmt) int {
	if len(b.List) == 0 { // weaken:empty
		return noReturn
	}
	switch s := b.List[0].(type) {
	case *ast.ReturnStmt:
		return bareReturn
	case *ast.BlockStmt: // weaken:block
		return returnRank(s)
	case *ast.IfStmt:
		for {
			if onlyReturn(s.Body) { // weaken:then
				return guardedReturn
			}
			switch e := s.Else.(type) {
			case *ast.IfStmt: // weaken:else-if
				s = e
				continue
			case *ast.BlockStmt:
				if onlyReturn(e) { // weaken:else
					return guardedReturn
				}
			}
			return noReturn
		}
	}
	return noReturn
}

// onlyReturn reports a block that is a single return and nothing else.
func onlyReturn(b *ast.BlockStmt) bool {
	if len(b.List) != 1 { // weaken:only
		return false
	}
	_, ok := b.List[0].(*ast.ReturnStmt)
	return ok
}

// constraints returns the file's build constraints in a canonical form: every
// //go:build and // +build line that appears before the package clause, in
// order, joined by newlines. "" when there are none.
func constraints(src []byte) string {
	var out []string
	for _, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "package ") || t == "package" {
			break
		}
		if constraint.IsGoBuild(t) || constraint.IsPlusBuild(t) {
			out = append(out, strings.Join(strings.Fields(t), " "))
		}
	}
	return strings.Join(out, "\n")
}
