package buildgate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE FREED-PORT RULE (gonk-uv82; docs/testing-time.md rule 4).
//
// "Open a listener, close it, then use its address as one nobody answers on"
// has reached the gate four times (gonk-th5r, gonk-myvu, gonk-d2kb,
// gonk-uv82). Each fix made the freed port a little safer -- start the live
// servers first, dial it to prove it refuses -- and each left the window the
// next incident came through, because a freed port belongs to whoever binds
// next, and on this box that is another agent's gate. The fix that ends the
// class is internal/testnet.RefusedAddr, which HOLDS the port (bound, never
// listening) for the whole test.
//
// A helper that is merely available does not stop the fifth incident: the
// freed-port idiom is three obvious lines, and pkg/intake had one that was
// never routed through the helper at all (`srv.Close() // closed before use`).
// So the idiom itself is rejected here, in every Go file in the repo:
//
//   - a listener or httptest server is CLOSED (not deferred, not in a
//     Cleanup), and its address -- .URL, .Addr(), .Listener, or a variable
//     built from one of those -- is used afterwards in the same function;
//   - or its close is DEFERRED and the function RETURNS its address (the
//     "freePort()" helper: the port is free again the moment it is returned).
//
// A use that is genuinely safe says why on the Close line (or the line above):
//
//	// freed-port-ok: <reason>
//
// This is syntax, not types: it follows variables by name inside one function
// and knows a listener by the name of the call that made it. It will miss a
// freed port laundered through a struct field or a second function. It is
// here to catch the three obvious lines.

// listenerMakers are the calls whose result owns a port: net.Listen and
// friends, httptest.New*Server, and local helpers named like them
// (mustListen, startStubServer, ...).
var listenerMakers = regexp.MustCompile(`(?i)^(must)?listen|^New(TLS|Unstarted)?Server$|listener$`)

const freedPortWaiver = "freed-port-ok:"

// freedPortUses returns one message per violation of the rule in src.
func freedPortUses(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	waived := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if i := strings.Index(c.Text, freedPortWaiver); i >= 0 && strings.TrimSpace(c.Text[i+len(freedPortWaiver):]) != "" {
				line := fset.Position(c.Pos()).Line
				waived[line], waived[line+1] = true, true
			}
		}
	}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		for _, v := range freedPortUsesInFunc(fn) {
			if waived[fset.Position(v.close).Line] {
				continue
			}
			out = append(out, fmt.Sprintf("%s: %s is closed here and its address is %s at line %d",
				fset.Position(v.close), v.name, v.how, fset.Position(v.use).Line))
		}
	}
	return out, nil
}

type freedPortUse struct {
	name       string
	close, use token.Pos
	how        string
}

func calleeName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// addressOf reports whether e reads the address of a tracked listener: x.URL,
// x.Addr, x.Listener, or a variable derived from one.
func addressOf(e ast.Node, tracked map[string]bool, derived map[string]string) (owner string, found bool) {
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && tracked[id.Name] {
				switch x.Sel.Name {
				case "URL", "Addr", "Listener":
					owner, found = id.Name, true
					return false
				}
			}
		case *ast.Ident:
			if o, ok := derived[x.Name]; ok {
				owner, found = o, true
				return false
			}
		}
		return true
	})
	return owner, found
}

func freedPortUsesInFunc(fn *ast.FuncDecl) []freedPortUse {
	tracked := map[string]bool{}   // variables that own a listener
	derived := map[string]string{} // variable -> the listener its value came from

	// Pass 1: who owns a listener, and which variables carry its address.
	// Two rounds so `u := srv.URL` is seen whatever order Inspect visits in.
	for round := 0; round < 2; round++ {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, rhs := range as.Rhs {
				if c, ok := rhs.(*ast.CallExpr); ok && listenerMakers.MatchString(calleeName(c)) {
					if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
						tracked[id.Name] = true
					}
					continue
				}
				if owner, ok := addressOf(rhs, tracked, derived); ok {
					for _, l := range as.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name != "_" && id.Name != "err" && !tracked[id.Name] {
							derived[id.Name] = owner
						}
					}
				}
			}
			return true
		})
	}
	if len(tracked) == 0 {
		return nil
	}

	// Pass 2: closes. A close inside a defer or a t.Cleanup runs at the end,
	// so it frees nothing the function body can still use -- but it does free
	// what the function RETURNS.
	closedAt := map[string]token.Pos{}   // immediate close
	deferredAt := map[string]token.Pos{} // close at function exit
	var walk func(n ast.Node, atExit bool)
	walk = func(n ast.Node, atExit bool) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.DeferStmt:
				walk(x.Call, true)
				return false
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "Cleanup" && !atExit {
						for _, a := range x.Args {
							walk(a, true)
						}
						return false
					}
					if id, ok := sel.X.(*ast.Ident); ok && sel.Sel.Name == "Close" && tracked[id.Name] {
						m := closedAt
						if atExit {
							m = deferredAt
						}
						if _, seen := m[id.Name]; !seen {
							m[id.Name] = x.Pos()
						}
					}
				}
			}
			return true
		})
	}
	walk(fn.Body, false)

	// Pass 3: uses of the address after an immediate close, and returns of
	// the address of anything closed at exit.
	var out []freedPortUse
	reported := map[string]bool{}
	report := func(name string, cl, use token.Pos, how string) {
		if !reported[name] {
			reported[name] = true
			out = append(out, freedPortUse{name, cl, use, how})
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ReturnStmt:
			for _, r := range x.Results {
				if owner, ok := addressOf(r, tracked, derived); ok {
					if cl, ok := deferredAt[owner]; ok {
						report(owner, cl, r.Pos(), "returned to the caller after the deferred close")
					}
				}
			}
		case *ast.SelectorExpr, *ast.Ident:
			owner, ok := addressOf(x, tracked, derived)
			if !ok {
				return true
			}
			if cl, ok := closedAt[owner]; ok && x.Pos() > cl {
				// The assignment that DEFINES a derived variable is not a use.
				report(owner, cl, x.Pos(), "used")
			}
			return false
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].close < out[j].close })
	return out
}

func TestNoTestUsesAFreedPortAsADeadAddress(t *testing.T) {
	root := repoRoot(t)
	var bad []string
	files := 0
	for _, top := range []string{"cmd", "pkg", "internal", "test", "hack"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "vendor" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files++
			rel, _ := filepath.Rel(root, p)
			msgs, err := freedPortUses(rel, src)
			if err != nil {
				return fmt.Errorf("parse %s: %w", rel, err)
			}
			bad = append(bad, msgs...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 100 {
		t.Fatalf("scanned only %d Go files -- the walk is stale and this check would pass vacuously", files)
	}
	if len(bad) != 0 {
		t.Fatalf("a freed port is used as an address nobody answers on (gonk-uv82). A closed listener's port "+
			"belongs to the next binder -- on this box, another agent's gate -- so it can ANSWER. Use "+
			"internal/testnet.RefusedAddr / RefusedURL, which hold the port for the whole test; or, if this "+
			"use is safe, say why with `// %s <reason>` on the Close line.\n  %s",
			freedPortWaiver, strings.Join(bad, "\n  "))
	}
}

// The rule must actually fire on each shape that has reached the gate, and
// must stay quiet on the shapes every other test in the repo uses.
func TestTheFreedPortCheckActuallyFails(t *testing.T) {
	wrap := func(body string) string {
		return "package p\nfunc TestX(t *testing.T) {\n" + body + "\n}\n"
	}
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"gonk-th5r/myvu shape: listen, take the address, close, use it", wrap(`
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	run(t, "http://"+addr)`), 1},
		{"blank-assigned close is still a close", wrap(`
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	run(t, addr)`), 1},
		{"pkg/intake shape: httptest server closed before use", wrap(`
	srv := httptest.NewServer(h)
	srv.Close() // closed before use
	c := NewMeterClient(srv.URL, tok, nil)
	_ = c`), 1},
		{"a client built before the close and used after it", wrap(`
	srv := httptest.NewServer(h)
	c := NewMeterClient(srv.URL, tok, nil)
	srv.Close()
	c.Register(ctx)`), 1},
		{"freePort helper: deferred close, address returned", `package p
func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	defer ln.Close()
	return ln.Addr().String()
}`, 1},
		{"freePort helper via a local helper and a variable", `package p
func freePort(t *testing.T) int {
	ln := mustListen(t)
	p := portOf(t, ln.Addr())
	defer func() { _ = ln.Close() }()
	return p
}`, 1},
		{"waived with a reason", wrap(`
	srv := httptest.NewServer(h)
	// freed-port-ok: the assertion is only that the call returns, either way
	srv.Close()
	get(srv.URL)`), 0},
		{"a waiver with no reason waives nothing", wrap(`
	srv := httptest.NewServer(h)
	srv.Close() // freed-port-ok:
	get(srv.URL)`), 1},
		{"the ordinary test: deferred close, address used while open", wrap(`
	srv := httptest.NewServer(h)
	defer srv.Close()
	get(srv.URL)`), 0},
		{"t.Cleanup close, address used while open", wrap(`
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { srv.Close() })
	get(srv.URL)`), 0},
		{"hold-then-check: the port is held while it is asserted busy", wrap(`
	ln := mustListen(t)
	defer ln.Close()
	if err := CheckPortsFree(portOf(t, ln)); err == nil { t.Fatal("held port reported free") }`), 0},
		{"explicit close as the last thing, nothing after", wrap(`
	srv := httptest.NewServer(h)
	get(srv.URL)
	srv.Close()`), 0},
		{"a helper that returns the live server itself", `package p
func healthy(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}`, 0},
	} {
		got, err := freedPortUses("x_test.go", []byte(tc.src))
		if err != nil {
			t.Fatalf("%s: fixture does not parse: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d finding(s), want %d: %v", tc.name, len(got), tc.want, got)
		}
	}
}
