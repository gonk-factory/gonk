package diffmap_test

import (
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/diffmap"
	"github.com/gonk-factory/gonk/pkg/glab"
)

// FileDiff is glab.MRDiff field for field: the controller converts with
// diffmap.FileDiff(d) and nothing is dropped or renamed on the way. The
// conversion below stops compiling if a field's name, type or order drifts;
// the loop catches a drifted JSON tag.
func TestFileDiffIsGlabMRDiff(t *testing.T) {
	d := glab.MRDiff{OldPath: "a", NewPath: "b", NewFile: true, RenamedFile: true, DeletedFile: true, Diff: "x", TooLarge: true, Collapsed: true}
	got := diffmap.FileDiff(d)
	if got.OldPath != "a" || got.NewPath != "b" || !got.NewFile || !got.RenamedFile || !got.DeletedFile || got.Diff != "x" || !got.TooLarge || !got.Collapsed {
		t.Fatalf("conversion lost a field: %+v", got)
	}
	a, b := reflect.TypeOf(d), reflect.TypeOf(got)
	if a.NumField() != b.NumField() || a.NumField() != 8 {
		t.Fatalf("field counts %d and %d", a.NumField(), b.NumField())
	}
	for i := 0; i < a.NumField(); i++ {
		if a.Field(i).Tag.Get("json") != b.Field(i).Tag.Get("json") {
			t.Errorf("field %s: json tag %q vs %q", a.Field(i).Name, a.Field(i).Tag.Get("json"), b.Field(i).Tag.Get("json"))
		}
	}
}

// PURE: the package imports the standard library and pkg/pipelinectx (for the
// ONE normalisation rule a quote is matched under) and nothing else -- no
// forge client, no store, no os, no clock.
func TestDiffmapIsPure(t *testing.T) {
	allowed := map[string]bool{
		"errors": true, "fmt": true, "strconv": true, "strings": true, "unicode": true, "unicode/utf8": true,
		"github.com/gonk-factory/gonk/pkg/pipelinectx": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			seen++
			p, _ := strconv.Unquote(imp.Path.Value)
			if !allowed[p] {
				t.Errorf("%s imports %s: pkg/diffmap is pure (strings in, a map out)", n, p)
			}
		}
	}
	if seen < 4 {
		t.Fatalf("saw %d imports; the guard is vacuous", seen)
	}
}
