package weaken

import (
	"strings"
	"testing"
)

const baseTest = `package calc

import "testing"

func TestTotal(t *testing.T) {
	if got := Total([]int{1, 2}); got != 3 {
		t.Fatalf("Total = %d, want 3", got)
	}
}

func TestEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	if Total(nil) != 0 {
		t.Fatal("want 0")
	}
}

func helper(t *testing.T) { t.Helper() }

func BenchmarkTotal(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Total([]int{1})
	}
}
`

// Each row changes exactly one thing in baseTest. A row with want "" is a
// change the predicate must ACCEPT -- the predicate refusing every edit to a
// test file would make the `test` row of the envelope unusable.
func TestGo(t *testing.T) {
	rep := func(old, new string) string {
		if !strings.Contains(baseTest, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(baseTest, old, new, 1)
	}
	cases := []struct {
		name    string
		patched string
		want    string
		detail  string // a substring the detail must carry
	}{
		{"an assertion corrected", rep("want 3", "want three"), "", ""},
		{"a new test added", baseTest + "\nfunc TestNew(t *testing.T) {}\n", "", ""},
		{"a comment added", rep("func helper", "// helper marks a helper.\nfunc helper"), "", ""},
		{"an existing skip reworded", rep(`t.Skip("short")`, `t.Skip("skipped in short mode")`), "", ""},

		{"a test deleted", rep("func TestTotal(t *testing.T) {\n\tif got := Total([]int{1, 2}); got != 3 {\n\t\tt.Fatalf(\"Total = %d, want 3\", got)\n\t}\n}\n", ""), RemovedTest, "TestTotal"},
		{"a test renamed out of go test's sight", rep("func TestTotal(", "func testTotal("), RemovedTest, "TestTotal"},
		{"a test renamed with a prefix", rep("func TestTotal(", "func XTestTotal("), RemovedTest, "TestTotal"},
		{"a benchmark deleted", rep("func BenchmarkTotal(b *testing.B) {\n\tfor i := 0; i < b.N; i++ {\n\t\tTotal([]int{1})\n\t}\n}\n", ""), RemovedTest, "BenchmarkTotal"},
		{"a test turned into a method", rep("func TestTotal(t", "func (s suite) TestTotal(t"), RemovedTest, "TestTotal"},

		{"t.Skip added", rep("\tif got := Total", "\tt.Skip(\"flaky\")\n\tif got := Total"), AddedSkip, "TestTotal"},
		{"t.Skipf added", rep("\tif got := Total", "\tt.Skipf(\"flaky %d\", 1)\n\tif got := Total"), AddedSkip, "TestTotal"},
		{"t.SkipNow added", rep("\tif got := Total", "\tt.SkipNow()\n\tif got := Total"), AddedSkip, "TestTotal"},
		{"a second skip added beside an existing one", rep("\tif Total(nil) != 0 {", "\tt.Skip()\n\tif Total(nil) != 0 {"), AddedSkip, "TestEmpty"},
		{"a skip added in a helper every test calls", rep("{ t.Helper() }", "{ t.Helper(); t.Skip() }"), AddedSkip, "helper"},
		{"a skip added inside a closure", rep("\tif got := Total", "\tfunc() { t.Skip() }()\n\tif got := Total"), AddedSkip, "TestTotal"},
		{"a bare return added first", rep("\tif got := Total", "\treturn\n\tif got := Total"), AddedSkip, "TestTotal"},

		{"a go:build tag added", "//go:build never\n\n" + baseTest, BuildConstraint, "never"},
		{"a +build tag added", "// +build ignore\n\n" + baseTest, BuildConstraint, "ignore"},

		{"left unparseable", rep("func TestTotal(t *testing.T) {", "func TestTotal(t *testing.T) {{"), Unparseable, ""},
		{"emptied to a comment", "// nothing\n", Unparseable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Go([]byte(baseTest), []byte(tc.patched))
			if got.Sub != tc.want {
				t.Fatalf("Go = %q (%s), want %q", got.Sub, got.Detail, tc.want)
			}
			if got.Found() != (tc.want != "") {
				t.Fatalf("Found = %v for sub %q", got.Found(), got.Sub)
			}
			if tc.detail != "" && !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail %q does not name %q", got.Detail, tc.detail)
			}
		})
	}
}

// An existing constraint that is CHANGED or REMOVED is a finding too: removing
// `//go:build integration` moves tests INTO the default build, which is not a
// weakening -- but the predicate cannot tell a tag that widens from one that
// narrows without knowing the project's build matrix, so any change is
// refused and a person decides.
func TestGoBuildConstraintChangesEitherWay(t *testing.T) {
	tagged := "//go:build integration\n\n" + baseTest
	for name, patched := range map[string]string{
		"removed":   baseTest,
		"changed":   "//go:build integration && never\n\n" + baseTest,
		"respelled": "// +build integration\n\n" + baseTest,
	} {
		if got := Go([]byte(tagged), []byte(patched)); got.Sub != BuildConstraint {
			t.Errorf("%s: Go = %q, want %q", name, got.Sub, BuildConstraint)
		}
	}
	// Whitespace inside the same constraint line is not a change.
	if got := Go([]byte(tagged), []byte("//go:build   integration\n\n"+baseTest)); got.Found() {
		t.Errorf("a respaced constraint is reported: %+v", got)
	}
	// A "//go:build" in a comment AFTER the package clause is not a constraint.
	late := strings.Replace(baseTest, "func helper", "//go:build never\nfunc helper", 1)
	if got := Go([]byte(baseTest), []byte(late)); got.Found() {
		t.Errorf("a comment after the package clause is read as a constraint: %+v", got)
	}
}

// A base that does not parse gives no test list to compare: the result must
// still parse and keep its constraints, and nothing else is claimed.
func TestGoWithAnUnparseableBase(t *testing.T) {
	broken := "package calc\n\nfunc TestTotal(t *testing.T) {\n"
	if got := Go([]byte(broken), []byte(baseTest)); got.Found() {
		t.Errorf("repairing a file that did not parse is reported: %+v", got)
	}
	if got := Go([]byte(broken), []byte(broken)); got.Sub != Unparseable {
		t.Errorf("a result that does not parse = %q, want %q", got.Sub, Unparseable)
	}
	if got := Go([]byte(broken), []byte("//go:build never\n\n"+baseTest)); got.Sub != BuildConstraint {
		t.Errorf("a tag added while repairing = %q, want %q", got.Sub, BuildConstraint)
	}
}
