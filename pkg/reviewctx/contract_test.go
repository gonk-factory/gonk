package reviewctx_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/reviewctx"
)

// Independence, property N2 of the plan (section 3.2): the reviewer's context
// is built from the merge request ALONE. These tests keep that true BY
// CONSTRUCTION -- there is no parameter, field or import through which the
// author session's record, transcript, rationale, rung or model could arrive.

const module = "github.com/gonk-factory/gonk/"

// allowedGonk: the only gonk packages pkg/reviewctx may reach, directly or
// through another package. Both are pure and import no gonk package besides
// each other (which this test checks, not assumes).
var allowedGonk = map[string]bool{
	"pkg/reviewctx":   true,
	"pkg/diffmap":     true, // the annotated diff and the line map
	"pkg/pipelinectx": true, // Redact, Norm, the "[gonk: " marker prefix
}

func nonTestSources(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out[n] = f
	}
	if len(out) == 0 {
		t.Fatalf("no Go sources in %s; the guard is looking in the wrong place", dir)
	}
	return out
}

func TestImportsNothingOfTheAuthorSession(t *testing.T) {
	std := map[string]bool{
		"crypto/sha256": true, "encoding/hex": true, "fmt": true, "regexp": true, "strconv": true,
		"strings": true, "unicode": true, "unicode/utf8": true,
	}
	direct := 0
	seen := map[string]bool{}
	var walk func(rel, via string)
	walk = func(rel, via string) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		if !allowedGonk[rel] {
			switch {
			case rel == "pkg/beadstore" || strings.HasPrefix(rel, "pkg/beadstore/"):
				t.Errorf("%s imports %s: the bead store holds the AUTHOR's record (model, rung, session, state). The reviewer's context must not be able to read it", via, rel)
			case strings.HasPrefix(rel, "cmd/"):
				t.Errorf("%s imports %s: cmd/ is where sessions, transcripts and the store are wired together", via, rel)
			default:
				t.Errorf("%s imports %s, which is not on pkg/reviewctx's allowlist: a new import is a new way author-side state could reach the reviewer -- justify it and add it", via, rel)
			}
			return
		}
		for name, f := range nonTestSources(t, filepath.Join("..", "..", filepath.FromSlash(rel))) {
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if rest, ok := strings.CutPrefix(p, module); ok {
					walk(rest, rel+"/"+name)
					continue
				}
				if rel == "pkg/reviewctx" {
					direct++
					if !std[p] {
						t.Errorf("%s imports %s, which is not on pkg/reviewctx's allowlist (no os, no net, no clock, no encoding of foreign records)", name, p)
					}
				}
			}
		}
	}
	walk("pkg/reviewctx", "(root)")
	if direct < 5 || !seen["pkg/diffmap"] || !seen["pkg/pipelinectx"] {
		t.Fatalf("saw %d standard imports, reached %v; the guard is vacuous", direct, seen)
	}
}

// wantLeaves is the WHOLE of ReviewFacts: every leaf field, its kind, and
// where the controller reads it from. It is the allow-list the field contract
// enforces and the provenance table the canary test fills from. A field that
// is not here does not exist; to add one, say where it comes from -- and it
// may not come from the author's session.
var wantLeaves = map[string]struct{ kind, from string }{
	"MRIID":                        {"int64", "GitLab: merge request iid"},
	"MRTitle":                      {"string", "GitLab: merge request title (controller-rendered for a gonk MR; fix plan section 5)"},
	"TargetBranch":                 {"string", "GitLab: merge request target_branch"},
	"HeadSHA":                      {"string", "GitLab: merge request sha"},
	"BaseSHA":                      {"string", "GitLab: merge request diff_refs.base_sha"},
	"Diff[].OldPath":               {"string", "GitLab: merge request diffs"},
	"Diff[].NewPath":               {"string", "GitLab: merge request diffs"},
	"Diff[].NewFile":               {"bool", "GitLab: merge request diffs"},
	"Diff[].RenamedFile":           {"bool", "GitLab: merge request diffs"},
	"Diff[].DeletedFile":           {"bool", "GitLab: merge request diffs"},
	"Diff[].Diff":                  {"string", "GitLab: merge request diffs"},
	"Diff[].TooLarge":              {"bool", "GitLab: merge request diffs"},
	"Diff[].Collapsed":             {"bool", "GitLab: merge request diffs"},
	"Issue.IID":                    {"int64", "GitLab: the linked issue"},
	"Issue.Title":                  {"string", "GitLab: the linked issue's title"},
	"Issue.Body":                   {"string", "GitLab: the linked issue's description"},
	"Pipeline.Read":                {"bool", "controller: whether the head pipeline was read"},
	"Pipeline.ID":                  {"int64", "GitLab: head pipeline id"},
	"Pipeline.SHA":                 {"string", "GitLab: head pipeline sha"},
	"Pipeline.Status":              {"string", "GitLab: head pipeline status"},
	"Pipeline.Jobs[].Name":         {"string", "GitLab: pipeline jobs"},
	"Pipeline.Jobs[].Stage":        {"string", "GitLab: pipeline jobs"},
	"Pipeline.Jobs[].Status":       {"string", "GitLab: pipeline jobs"},
	"Pipeline.Jobs[].AllowFailure": {"bool", "GitLab: pipeline jobs"},
	"CIDelta":                      {"string", "controller: cidef.Diff of base and head (none, preserving, exec-changing)"},
}

// forbiddenNames: what a field carrying author-side state would be called.
// Matched case-insensitively against every field name on the way to a leaf.
var forbiddenNames = []string{
	"rationale", "transcript", "summary", "session", "rung", "model", "bead", "record",
	"author", "attempt", "description", "explanation", "reason", "note", "comment", "verdict", "evidence",
}

// leaves walks a struct type to its scalar leaves. Only strings, bools and
// int64s are data; a struct must be one of the two packages' own; anything
// that could hold an arbitrary value (interface, map, func, chan, a foreign
// type) is reported in bad.
func leaves(rt reflect.Type, path string, out map[string]string, bad *[]string) {
	switch rt.Kind() {
	case reflect.Pointer:
		leaves(rt.Elem(), path, out, bad)
	case reflect.Slice:
		leaves(rt.Elem(), path+"[]", out, bad)
	case reflect.String, reflect.Bool, reflect.Int64:
		if rt.PkgPath() != "" {
			*bad = append(*bad, path+" has named type "+rt.String()+" (plain string, bool and int64 only)")
		}
		out[path] = rt.Kind().String()
	case reflect.Struct:
		pkg := strings.TrimPrefix(rt.PkgPath(), module)
		if pkg != "pkg/reviewctx" && pkg != "pkg/diffmap" {
			*bad = append(*bad, path+" has foreign struct type "+rt.String())
			return
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				*bad = append(*bad, path+"."+f.Name+" is unexported: a field the contract cannot see")
				continue
			}
			if f.Anonymous {
				*bad = append(*bad, path+"."+f.Name+" is embedded: its fields would arrive unnamed")
				continue
			}
			for _, word := range forbiddenNames {
				if strings.Contains(strings.ToLower(f.Name), word) {
					*bad = append(*bad, strings.TrimPrefix(path+"."+f.Name, ".")+" is named like author-side state ("+word+")")
				}
			}
			leaves(f.Type, strings.TrimPrefix(path+"."+f.Name, "."), out, bad)
		}
	default:
		*bad = append(*bad, path+" has kind "+rt.Kind().String()+": ReviewFacts must stay plain facts, with nowhere to put an arbitrary value")
	}
}

func TestReviewFactsHoldsOnlyAllowListedFields(t *testing.T) {
	got := map[string]string{}
	var bad []string
	leaves(reflect.TypeOf(reviewctx.ReviewFacts{}), "", got, &bad)
	for _, b := range bad {
		t.Error(b)
	}
	var paths []string
	for p := range got {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		want, ok := wantLeaves[p]
		switch {
		case !ok:
			t.Errorf("ReviewFacts has a field %s (%s) that is not on the allow-list. The reviewer is told only what the merge request itself says: if this is a fact of the MR, add it to wantLeaves with where it is read from; if it is anything of the author's session, it must not exist", p, got[p])
		case want.kind != got[p]:
			t.Errorf("ReviewFacts.%s is %s, the allow-list says %s", p, got[p], want.kind)
		}
	}
	for p := range wantLeaves {
		if _, ok := got[p]; !ok {
			t.Errorf("the allow-list names %s, which ReviewFacts no longer has", p)
		}
	}
	if len(got) < 20 {
		t.Fatalf("walked %d leaves; the guard is vacuous", len(got))
	}
}

// Build takes ReviewFacts and nothing else, and it is the package's only
// function: there is no second entry point, no option, no setter and no
// package variable through which anything more could be handed in.
func TestBuildIsTheOnlyWayIn(t *testing.T) {
	bt := reflect.TypeOf(reviewctx.Build)
	if bt.NumIn() != 1 || bt.In(0) != reflect.TypeOf(reviewctx.ReviewFacts{}) || bt.IsVariadic() {
		t.Fatalf("Build is %s; it must take exactly one ReviewFacts", bt)
	}
	if bt.NumOut() != 2 || bt.Out(0) != reflect.TypeOf(reviewctx.Context{}) {
		t.Fatalf("Build returns %s", bt)
	}
	wantFuncs := map[string]bool{"Build": true, "(*Skip).Error": true, "(*Skip).Fact": true, "(*BadFacts).Error": true}
	gotFuncs := map[string]bool{}
	for name, f := range nonTestSources(t, ".") {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				n := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					var b strings.Builder
					b.WriteString("(")
					if star, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
						b.WriteString("*" + star.X.(*ast.Ident).Name)
					} else {
						b.WriteString(d.Recv.List[0].Type.(*ast.Ident).Name)
					}
					n = b.String() + ")." + n
				}
				gotFuncs[n] = true
				if !wantFuncs[n] {
					t.Errorf("%s: exported func %s is not on the list. A second way to build or adjust the reviewer's context is a way around ReviewFacts", name, n)
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					for _, id := range spec.(*ast.ValueSpec).Names {
						if id.IsExported() {
							t.Errorf("%s: exported package variable %s: package state is a side channel into Build", name, id.Name)
						}
					}
				}
			}
		}
	}
	for n := range wantFuncs {
		if !gotFuncs[n] {
			t.Errorf("expected exported func %s; the guard no longer describes the package", n)
		}
	}
	// The result carries the prompt, its hash and the map -- and nothing a
	// caller could have smuggled through.
	ct := reflect.TypeOf(reviewctx.Context{})
	var fields []string
	for i := 0; i < ct.NumField(); i++ {
		fields = append(fields, ct.Field(i).Name+" "+ct.Field(i).Type.String())
	}
	if got := strings.Join(fields, ", "); got != "Prompt string, PromptSHA256 string, Map *diffmap.Map" {
		t.Errorf("Context is {%s}", got)
	}
}
