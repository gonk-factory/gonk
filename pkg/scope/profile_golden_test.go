package scope

import (
	"os"
	"strings"
	"testing"
)

// The golden files behind gonk-wj14's "the default selection is what it was".
//
// testdata/profile_paths.txt is a broad INVENTED path list (no real project's
// tree). testdata/builtin_before_profiles.golden is what Builtin said about
// each of them BEFORE ecosystem profiles existed: it was generated in commit
// b859037, which changes no classifier code. It is never regenerated: it is
// the record of the old table.
//
// Two statements are proved against it, and they are different statements:
//
//  1. `go` + `docs-md`, with no other profile shipped, ARE the old table: byte
//     for byte the same class for every path
//     (TestGoAndDocsMdAloneAreTheTableBeforeProfiles).
//  2. With the whole menu shipped, a project that selects nothing differs from
//     the old table in exactly the paths listed in
//     testdata/builtin_default_tightened.golden, and every one of them moved
//     to a sensitive class that is matched EARLIER, because a sensitive row of
//     a profile it did not select names it
//     (TestDefaultSelectionDiffersFromBeforeOnlyByTightening).
//
// (2) is not "byte-identical", and cannot be: 066 section 11 item 2 says
// package.json is `deps` on every project, and before profiles it was `other`.
// What is identical is everything a workflow could be ALLOWED to write by
// being given source, test and docs: no path gained one of those classes.
const (
	goldenPathsFile     = "testdata/profile_paths.txt"
	goldenBeforeFile    = "testdata/builtin_before_profiles.golden"
	goldenTightenedFile = "testdata/builtin_default_tightened.golden"
)

func goldenPaths(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(goldenPathsFile)
	if err != nil {
		t.Fatal(err)
	}
	paths := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(paths) < 1500 {
		t.Fatalf("%s holds %d paths; the golden is not broad", goldenPathsFile, len(paths))
	}
	return paths
}

// renderGolden is the golden file's format: one "class<TAB>path" line per path.
func renderGolden(paths []string, classify func(string) Class) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(string(classify(p)))
		b.WriteByte('\t')
		b.WriteString(p)
		b.WriteByte('\n')
	}
	return b.String()
}

// goldenBefore returns the old table's answer per path.
func goldenBefore(t *testing.T) (raw string, byPath map[string]Class) {
	t.Helper()
	b, err := os.ReadFile(goldenBeforeFile)
	if err != nil {
		t.Fatal(err)
	}
	byPath = map[string]Class{}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		c, p, ok := strings.Cut(line, "\t")
		if !ok || !Class(c).Valid() {
			t.Fatalf("%s: bad line %q", goldenBeforeFile, line)
		}
		byPath[p] = Class(c)
	}
	return string(b), byPath
}

func profilesNamed(t *testing.T, names ...string) []Profile {
	t.Helper()
	var out []Profile
	for _, n := range names {
		pr, ok := LookupProfile(n)
		if !ok {
			t.Fatalf("no shipped profile %q", n)
		}
		out = append(out, pr)
	}
	return out
}

func TestGoAndDocsMdAloneAreTheTableBeforeProfiles(t *testing.T) {
	want, _ := goldenBefore(t)
	tbl := buildTable(profilesNamed(t, "go", "docs-md"), DefaultProfiles())
	got := renderGolden(goldenPaths(t), func(p string) Class {
		c, _ := tbl.classify(p, BaseFacts{})
		return c
	})
	if got == want {
		return
	}
	gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
	n := 0
	for i := range wl {
		if i < len(gl) && gl[i] != wl[i] {
			if n++; n <= 10 {
				t.Errorf("now %q, before profiles %q", gl[i], wl[i])
			}
		}
	}
	t.Fatalf("the go and docs-md profiles are not the table that existed before profiles: %d of %d paths differ (%s)", n, len(wl)-1, goldenBeforeFile)
}

func TestDefaultSelectionDiffersFromBeforeOnlyByTightening(t *testing.T) {
	_, before := goldenBefore(t)
	paths := goldenPaths(t)
	var b strings.Builder
	same, moved := 0, 0
	for _, p := range paths {
		was, ok := before[p]
		if !ok {
			t.Fatalf("%q is not in %s", p, goldenBeforeFile)
		}
		now, profile := defaultTable.classify(p, BaseFacts{})
		if Builtin(p) != now {
			t.Fatalf("Builtin(%q) = %s, the default table says %s", p, Builtin(p), now)
		}
		if now == was {
			same++
			continue
		}
		moved++
		b.WriteString(string(was) + "\t" + string(now) + "\t" + profile + "\t" + p + "\n")

		// Each of these is the rule, not the list: whatever the list says, a
		// move that is not a tightening fails here.
		if now != ClassTestConfig && now != ClassDeps && now != ClassGenerated {
			t.Errorf("%q: %s -> %s; the default selection may differ from before only by a path becoming test-config, deps or generated", p, was, now)
		}
		if classRank(now) >= classRank(was) {
			t.Errorf("%q: %s -> %s is not a move to an EARLIER class", p, was, now)
		}
		if was.Sensitivity() == Protected {
			t.Errorf("%q left the protected class %s", p, was)
		}
		if profile == "" || profile == "go" || profile == "docs-md" {
			t.Errorf("%q: %s -> %s was decided by profile %q; only a profile the default does NOT select can explain a difference", p, was, now, profile)
		}
	}
	got := b.String()
	if os.Getenv("UPDATE_GOLDEN") == "tightened" {
		if err := os.WriteFile(goldenTightenedFile, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenTightenedFile)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the set of paths the default selection classifies differently from before profiles has changed; it is recorded, line for line, in %s (was<TAB>now<TAB>profile<TAB>path). Regenerate deliberately: UPDATE_GOLDEN=tightened", goldenTightenedFile)
	}
	// Not vacuous in either direction.
	if same < 1200 || moved < 100 {
		t.Fatalf("%d paths unchanged and %d tightened; the golden no longer exercises both", same, moved)
	}
	// No path gained a class an ordinary workflow is given.
	for _, p := range paths {
		if now := Builtin(p); now.Sensitivity() == Normal && before[p] != now {
			t.Errorf("%q is %s by default and was %s: the default selection made a path writable-as-normal that was not", p, now, before[p])
		}
	}
}
