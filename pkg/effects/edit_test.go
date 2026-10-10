package effects

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// ApplyEdits: base blob + edits -> new blob, or a typed refusal (plan 4.1,
// 5.3 A5). All fixtures invented.

func fixWith(edits ...Effect) Batch {
	return Batch{Verdict: VerdictFix, Effects: append(edits, metaEffect())}
}

func TestApplyEdits(t *testing.T) {
	const f = "src/a.txt"
	cases := []struct {
		name  string
		base  BaseBlobs
		edits []Effect
		want  map[string]string // path -> content; nil when refused
		rule  Rule
		sub   string
	}{
		{name: "one match",
			base: BaseBlobs{f: []byte("one\ntwo\nthree\n")}, edits: []Effect{editEffect(f, "two", "2")},
			want: map[string]string{f: "one\n2\nthree\n"}},
		{name: "a deletion",
			base: BaseBlobs{f: []byte("one\ntwo\nthree\n")}, edits: []Effect{editEffect(f, "two\n", "")},
			want: map[string]string{f: "one\nthree\n"}},
		{name: "multi-line old, with multi-byte text around it",
			base: BaseBlobs{f: []byte("\u00e9 one\ntwo\nthree \u00fc\n")}, edits: []Effect{editEffect(f, "one\ntwo\n", "1\n2\n")},
			want: map[string]string{f: "\u00e9 1\n2\nthree \u00fc\n"}},
		{name: "two edits to one file, the second sees the first's result",
			base: BaseBlobs{f: []byte("alpha\n")}, edits: []Effect{editEffect(f, "alpha", "beta"), editEffect(f, "beta", "gamma")},
			want: map[string]string{f: "gamma\n"}},
		{name: "edits to two files",
			base:  BaseBlobs{f: []byte("a\n"), "src/b.txt": []byte("b\n"), "src/untouched.txt": []byte("u\n")},
			edits: []Effect{editEffect("src/b.txt", "b", "B"), editEffect(f, "a", "A")},
			want:  map[string]string{f: "A\n", "src/b.txt": "B\n"}},

		// THE rule: exactly once.
		{name: "old matches twice",
			base: BaseBlobs{f: []byte("x = 1\ny = 2\nx = 1\n")}, edits: []Effect{editEffect(f, "x = 1", "x = 3")},
			rule: RuleEditAmbiguous},
		{name: "old matches twice, overlapping",
			base: BaseBlobs{f: []byte("aaa\n")}, edits: []Effect{editEffect(f, "aa", "b")},
			rule: RuleEditAmbiguous},
		{name: "old matches three times",
			base: BaseBlobs{f: []byte("k\nk\nk\n")}, edits: []Effect{editEffect(f, "k\n", "j\n")},
			rule: RuleEditAmbiguous},
		{name: "old matches nowhere",
			base: BaseBlobs{f: []byte("one\n")}, edits: []Effect{editEffect(f, "two", "2")},
			rule: RuleEditNoMatch},
		{name: "old differs only in whitespace: no fuzzy match",
			base: BaseBlobs{f: []byte("\treturn 1\n")}, edits: []Effect{editEffect(f, "    return 1", "return 2")},
			rule: RuleEditNoMatch},
		{name: "old differs only in case: no fuzzy match",
			base: BaseBlobs{f: []byte("Total\n")}, edits: []Effect{editEffect(f, "total", "sum")},
			rule: RuleEditNoMatch},
		// "as it stands after the batch's earlier edits to that path"
		{name: "unique in the base, ambiguous after an earlier edit",
			base: BaseBlobs{f: []byte("cat\ndog\n")}, edits: []Effect{editEffect(f, "dog", "cat"), editEffect(f, "cat", "cow")},
			rule: RuleEditAmbiguous},
		{name: "present in the base, gone after an earlier edit",
			base: BaseBlobs{f: []byte("cat\ndog\n")}, edits: []Effect{editEffect(f, "dog", "eel"), editEffect(f, "dog", "cow")},
			rule: RuleEditNoMatch},

		// No creation, no deletion (plan 4.1, section 15).
		{name: "the file does not exist at the base",
			base: BaseBlobs{"src/other.txt": []byte("x\n")}, edits: []Effect{editEffect(f, "x", "y")},
			rule: RuleEditMissing},
		{name: "an edit that empties the file is a deletion in all but name",
			base: BaseBlobs{f: []byte("only\n")}, edits: []Effect{editEffect(f, "only\n", "")},
			rule: RuleEditEmpties},
		{name: "an empty base file cannot be edited (nothing can match)",
			base: BaseBlobs{f: []byte{}}, edits: []Effect{editEffect(f, "x", "y")},
			rule: RuleEditNoMatch},

		{name: "the base blob is not UTF-8",
			base: BaseBlobs{f: []byte("ok\n\xff\xfe\nmore\n")}, edits: []Effect{editEffect(f, "ok", "fine")},
			rule: RuleEditEncoding},
		{name: "the base blob holds a NUL (binary)",
			base: BaseBlobs{f: []byte("ok\n\x00\n")}, edits: []Effect{editEffect(f, "ok", "fine")},
			rule: RuleEditEncoding},
		{name: "edits that cancel out leave the file unchanged",
			base: BaseBlobs{f: []byte("cat\n")}, edits: []Effect{editEffect(f, "cat", "dog"), editEffect(f, "dog", "cat")},
			rule: RuleEditNoop},
		{name: "an unclean path is refused here too, not used as a map key",
			base: BaseBlobs{"src/../src/a.txt": []byte("x\n")}, edits: []Effect{editEffect("src/../src/a.txt", "x", "y")},
			rule: RuleForm, sub: "path"},
	}
	for _, tc := range cases {
		before := cloneBlobs(tc.base)
		got, err := ApplyEdits(fixWith(tc.edits...), tc.base)
		if !reflect.DeepEqual(before, tc.base) {
			t.Errorf("%s: ApplyEdits modified the base blobs", tc.name)
		}
		if tc.rule != "" {
			if got != nil {
				t.Errorf("%s: refused, yet returned files %+v", tc.name, got)
			}
			wantRefusal(t, tc.name, err, tc.rule, tc.sub)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		have := map[string]string{}
		for i, pf := range got {
			have[pf.Path] = string(pf.Content)
			if !bytes.Equal(pf.Base, tc.base[pf.Path]) {
				t.Errorf("%s: PatchedFile.Base for %s is not the base blob", tc.name, pf.Path)
			}
			if i > 0 && got[i-1].Path >= pf.Path {
				t.Errorf("%s: files not sorted by path: %q before %q", tc.name, got[i-1].Path, pf.Path)
			}
		}
		if !reflect.DeepEqual(have, tc.want) {
			t.Errorf("%s: result = %q, want %q", tc.name, have, tc.want)
		}
	}
}

func cloneBlobs(b BaseBlobs) BaseBlobs {
	out := BaseBlobs{}
	for k, v := range b {
		out[k] = append([]byte{}, v...)
	}
	return out
}

// The refusal names the effect and the path, so a session (and a log reader)
// can tell which edit failed.
func TestApplyEditsRefusalNamesTheEdit(t *testing.T) {
	b := fixWith(editEffect("a.txt", "a", "A"), editEffect("b.txt", "zz", "Z"))
	_, err := ApplyEdits(b, BaseBlobs{"a.txt": []byte("a\n"), "b.txt": []byte("b\n")})
	r := AsRefusal(err)
	if r == nil || r.Rule != RuleEditNoMatch || r.Effect != 1 || r.Path != "b.txt" {
		t.Fatalf("refusal = %+v (%v), want ED-NOMATCH on effect 1, b.txt", r, err)
	}
	if !strings.Contains(err.Error(), "ED-NOMATCH") || !strings.Contains(err.Error(), "b.txt") {
		t.Fatalf("error text %q names neither the rule nor the path", err)
	}
}

// Only `edit` effects are read. Nothing else in a batch -- a diagnosis's
// files, a scope_request's paths, a file effect -- can become a patched file.
func TestOnlyEditEffectsBecomePatchedFiles(t *testing.T) {
	base := BaseBlobs{"a.txt": []byte("a\n"), "b.txt": []byte("b\n"), ".agent/x": []byte("x\n")}
	for k := range knownKinds {
		if k == KindEdit {
			continue
		}
		e := Effect{Kind: k, Path: "a.txt", Old: "a", New: "A", Content: "c", Files: []string{"b.txt"}, Paths: []string{"b.txt"}}
		got, err := ApplyEdits(Batch{Effects: []Effect{e}}, base)
		if err != nil || len(got) != 0 {
			t.Errorf("kind %q produced patched files %+v (err %v)", k, got, err)
		}
	}
	if w := FileWrites(fixWith(editEffect("a.txt", "a", "A"))); len(w) != 0 {
		t.Errorf("FileWrites turned an edit into a whole-file write: %+v", w)
	}
}

// Replacing a valid-UTF-8 substring of valid UTF-8 with valid UTF-8 yields
// valid UTF-8 -- so the result check in ApplyEdits can only fire on a bad BASE.
// This holds the claim rather than assuming it.
func TestApplyEditsResultIsValidUTF8(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := []string{"a", "b", "\n", "\u00e9", "\u65e5", " ", "x"}
	gen := func(n int) string {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			sb.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return sb.String()
	}
	applied := 0
	for i := 0; i < 3000; i++ {
		base, old, new := gen(12+rng.Intn(20)), gen(1+rng.Intn(3)), gen(rng.Intn(4))
		got, err := ApplyEdits(fixWith(editEffect("f.txt", old, new)), BaseBlobs{"f.txt": []byte(base)})
		if err != nil {
			continue
		}
		applied++
		if !utf8.Valid(got[0].Content) {
			t.Fatalf("base %q old %q new %q -> invalid UTF-8 %q", base, old, new, got[0].Content)
		}
		if want := strings.Replace(base, old, new, 1); string(got[0].Content) != want {
			t.Fatalf("base %q old %q new %q -> %q, want %q", base, old, new, got[0].Content, want)
		}
	}
	if applied < 200 {
		t.Fatalf("only %d of 3000 generated edits applied; the property is under-exercised", applied)
	}
}

// ---- DiffFacts ----

func TestDiffFacts(t *testing.T) {
	lines := func(n int, f string) string {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, f+"\n", i)
		}
		return sb.String()
	}
	cases := []struct {
		name           string
		base           BaseBlobs
		edits          []Effect
		files          int
		added, removed int
		newBytes       int
		addedLines     map[string][]string
	}{
		{name: "one line changed",
			base: BaseBlobs{"a.txt": []byte("one\ntwo\nthree\n")}, edits: []Effect{editEffect("a.txt", "two", "2")},
			files: 1, added: 1, removed: 1, newBytes: 1, addedLines: map[string][]string{"a.txt": {"2\n"}}},
		{name: "one line removed",
			base: BaseBlobs{"a.txt": []byte("one\ntwo\nthree\n")}, edits: []Effect{editEffect("a.txt", "two\n", "")},
			files: 1, added: 0, removed: 1, newBytes: 0, addedLines: map[string][]string{"a.txt": nil}},
		{name: "three lines inserted after one",
			base: BaseBlobs{"a.txt": []byte("one\ntwo\n")}, edits: []Effect{editEffect("a.txt", "one\n", "one\na\nb\nc\n")},
			files: 1, added: 3, removed: 0, newBytes: 10, addedLines: map[string][]string{"a.txt": {"a\n", "b\n", "c\n"}}},
		{name: "a change inside a line counts the whole line, both sides",
			base: BaseBlobs{"a.txt": []byte("alpha beta gamma\n")}, edits: []Effect{editEffect("a.txt", "beta", "BETA")},
			files: 1, added: 1, removed: 1, newBytes: 4, addedLines: map[string][]string{"a.txt": {"alpha BETA gamma\n"}}},
		{name: "the final newline is part of the last line",
			base: BaseBlobs{"a.txt": []byte("one\ntwo")}, edits: []Effect{editEffect("a.txt", "two", "two\n")},
			files: 1, added: 1, removed: 1, newBytes: 4, addedLines: map[string][]string{"a.txt": {"two\n"}}},
		{name: "two edits far apart in one file: the lines between are not counted",
			base:  BaseBlobs{"a.txt": []byte("head\n" + lines(50, "mid %d") + "tail\n")},
			edits: []Effect{editEffect("a.txt", "head", "HEAD"), editEffect("a.txt", "tail", "TAIL")},
			files: 1, added: 2, removed: 2, newBytes: 8, addedLines: map[string][]string{"a.txt": {"HEAD\n", "TAIL\n"}}},
		{name: "two files; totals are sums",
			base:  BaseBlobs{"a.txt": []byte("a\nb\n"), "b.txt": []byte("c\nd\n")},
			edits: []Effect{editEffect("b.txt", "c\n", "c1\nc2\n"), editEffect("a.txt", "b\n", "")},
			files: 2, added: 2, removed: 2, newBytes: 6, addedLines: map[string][]string{"a.txt": nil, "b.txt": {"c1\n", "c2\n"}}},
		{name: "an edit undone by a later one still counts its bytes, not its lines",
			base:  BaseBlobs{"a.txt": []byte("x\ny\n")},
			edits: []Effect{editEffect("a.txt", "x", "q"), editEffect("a.txt", "q", "x"), editEffect("a.txt", "y", "z")},
			files: 1, added: 1, removed: 1, newBytes: 3, addedLines: map[string][]string{"a.txt": {"z\n"}}},
	}
	for _, tc := range cases {
		files, err := ApplyEdits(fixWith(tc.edits...), tc.base)
		if err != nil {
			t.Errorf("%s: ApplyEdits: %v", tc.name, err)
			continue
		}
		d := ComputeDiffFacts(files)
		if d.FilesChanged != tc.files || d.LinesAdded != tc.added || d.LinesRemoved != tc.removed || d.NewBytes != tc.newBytes {
			t.Errorf("%s: facts = files %d +%d -%d new %dB, want files %d +%d -%d new %dB",
				tc.name, d.FilesChanged, d.LinesAdded, d.LinesRemoved, d.NewBytes, tc.files, tc.added, tc.removed, tc.newBytes)
		}
		if d.Lines() != tc.added+tc.removed || d.Edits != len(tc.edits) || len(d.Files) != tc.files {
			t.Errorf("%s: Lines() = %d, Edits = %d, len(Files) = %d", tc.name, d.Lines(), d.Edits, len(d.Files))
		}
		for _, fd := range d.Files {
			if want := tc.addedLines[fd.Path]; len(fd.AddedLines)+len(want) > 0 && !reflect.DeepEqual(fd.AddedLines, want) {
				t.Errorf("%s: %s added lines = %q, want %q", tc.name, fd.Path, fd.AddedLines, want)
			}
			if fd.Added != len(fd.AddedLines) || fd.BaseBytes != len(tc.base[fd.Path]) || !fd.Exact {
				t.Errorf("%s: %s BaseBytes = %d Exact = %v", tc.name, fd.Path, fd.BaseBytes, fd.Exact)
			}
		}
	}
}

// DiffFacts notes a file whose BASE content marks it generated: that is a
// class the path alone cannot tell (scope.IsGenerated), and SC-CLASS uses it.
func TestDiffFactsMarksGeneratedFiles(t *testing.T) {
	base := BaseBlobs{
		"pkg/gen/zz.go":   []byte("// Code generated by example-gen. DO NOT EDIT.\npackage gen\n\nconst A = 1\n"),
		"pkg/gen/hand.go": []byte("package gen\n\nconst B = 1\n"),
	}
	files, err := ApplyEdits(fixWith(editEffect("pkg/gen/zz.go", "A = 1", "A = 2"), editEffect("pkg/gen/hand.go", "B = 1", "B = 2")), base)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, fd := range ComputeDiffFacts(files).Files {
		got[fd.Path] = fd.Generated
	}
	if want := map[string]bool{"pkg/gen/zz.go": true, "pkg/gen/hand.go": false}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Generated = %v, want %v", got, want)
	}
}

// lineDiff against the textbook LCS, on generated inputs: the counts are those
// of a MINIMAL line diff, and the added/removed lines are real ones in order.
func TestLineDiffIsMinimal(t *testing.T) {
	lcs := func(a, b []string) int {
		dp := make([][]int, len(a)+1)
		for i := range dp {
			dp[i] = make([]int, len(b)+1)
		}
		for i := 1; i <= len(a); i++ {
			for j := 1; j <= len(b); j++ {
				if a[i-1] == b[j-1] {
					dp[i][j] = dp[i-1][j-1] + 1
				} else {
					dp[i][j] = max(dp[i-1][j], dp[i][j-1])
				}
			}
		}
		return dp[len(a)][len(b)]
	}
	subsequence := func(sub, of []string) bool {
		i := 0
		for _, l := range of {
			if i < len(sub) && sub[i] == l {
				i++
			}
		}
		return i == len(sub)
	}
	rng := rand.New(rand.NewSource(11))
	gen := func() []string {
		out := make([]string, rng.Intn(14))
		for i := range out {
			out[i] = string(rune('a'+rng.Intn(4))) + "\n"
		}
		return out
	}
	nonTrivial := 0
	for i := 0; i < 5000; i++ {
		a, b := gen(), gen()
		removed, added, exact := lineDiff(a, b)
		common := lcs(a, b)
		if !exact || len(removed) != len(a)-common || len(added) != len(b)-common {
			t.Fatalf("lineDiff(%q, %q) = -%d +%d exact=%v, want -%d +%d", a, b, len(removed), len(added), exact, len(a)-common, len(b)-common)
		}
		if !subsequence(removed, a) || !subsequence(added, b) {
			t.Fatalf("lineDiff(%q, %q): removed %q / added %q are not subsequences of the inputs", a, b, removed, added)
		}
		if len(removed) > 0 && len(added) > 0 && common > 0 {
			nonTrivial++
		}
	}
	if nonTrivial < 1000 {
		t.Fatalf("only %d non-trivial diffs generated", nonTrivial)
	}
}

// Past maxExactDiffLines the diff stops searching and reports the whole
// differing region -- a valid diff and an UPPER bound, flagged inexact, never
// an under-count.
func TestLineDiffFallsBackToAnUpperBound(t *testing.T) {
	var a, b []string
	for i := 0; i < maxExactDiffLines+50; i++ {
		a = append(a, fmt.Sprintf("old %d\n", i))
		b = append(b, fmt.Sprintf("new %d\n", i))
	}
	removed, added, exact := lineDiff(a, b)
	if exact || len(removed) != len(a) || len(added) != len(b) {
		t.Fatalf("lineDiff past the limit = -%d +%d exact=%v, want -%d +%d exact=false", len(removed), len(added), exact, len(a), len(b))
	}
}
