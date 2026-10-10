package gitexport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard"
	"github.com/gonk-factory/gonk/internal/exportguard/gitexport/exporttest"
)

// Positives are ASSEMBLED AT RUN TIME: this file is published and scanned.
func cat(parts ...string) string { return strings.Join(parts, "") }

func ip4(a, b, c, d int) string { return fmt.Sprintf("%d.%d.%d.%d", a, b, c, d) }

func cleanScanner(string) ([]ScanFinding, error) { return nil, nil }

var withScanner = CheckOptions{Scanner: cleanScanner}

// world is an origin, a stand-in mirror that is EMPTY (as the real mirror is
// before its first release: the first snapshot is the root of a new public
// history), and a clone. baseline is the origin's first commit: it is NOT on
// the mirror, and serves only as a parent for commits that are never
// published.
type world struct {
	t        *testing.T
	origin   *exporttest.Origin
	mirror   string
	baseline string // the origin's first commit; not on the mirror
	src      string // origin main after the base commit
}

func newWorld(t *testing.T) *world {
	t.Helper()
	o := exporttest.NewOrigin(t)
	baseline := o.Commit("public history", map[string]string{"README.md": "# gonk\n"})
	mirror := o.EmptyMirror()
	src := o.Commit("internal work", exporttest.BaseFiles())
	return &world{t: t, origin: o, mirror: mirror, baseline: baseline, src: src}
}

func (w *world) clone() *Repo {
	w.t.Helper()
	r, err := Clone(w.origin.Dir, "main")
	if err != nil {
		w.t.Fatalf("Clone: %v", err)
	}
	w.t.Cleanup(r.Close)
	return r
}

func (w *world) mirrorMain() string {
	return exporttest.RemoteRef(w.t, w.mirror, "refs/heads/main")
}

func mustRefuse(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a refusal mentioning %q, got success", wants)
	}
	if !IsRefused(err) {
		t.Fatalf("want a refusal, got an operational error: %v", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("the refusal does not mention %q:\n%v", w, err)
		}
	}
}

func paths(tr *TreeResult) []string {
	out := make([]string, len(tr.entries))
	for i, e := range tr.entries {
		out[i] = e.Path
	}
	return out
}

func TestTreeIsTheSourceMinusTheListPlusExportSource(t *testing.T) {
	w := newWorld(t)
	r := w.clone()
	tr, err := r.Tree(w.src, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"EXPORT-SOURCE", "README.md", "chart/gonk/Chart.yaml", "cmd/tool/main.go",
		"docs/guide.md", "docs/releases/v0.2.0.md", "go.mod", "images/Dockerfile.tool",
	}
	got := paths(tr)
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("public tree paths:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if tr.FilesInSource != 14 || tr.FilesInTree != 8 {
		t.Fatalf("counts: %d in source, %d in tree; want 14 and 8", tr.FilesInSource, tr.FilesInTree)
	}
	wantRemoved := []RemovedEntry{{".beads/", 1}, {"CLAUDE.md", 1}, {"docs/plans/", 2}, {"release/", 3}}
	if fmt.Sprint(tr.Removed) != fmt.Sprint(wantRemoved) {
		t.Fatalf("removed %v, want %v", tr.Removed, wantRemoved)
	}

	es, ok, err := r.show(tr.Tree, "EXPORT-SOURCE")
	if err != nil || !ok {
		t.Fatalf("EXPORT-SOURCE: %v %v", ok, err)
	}
	list, _ := exportguard.ParseList([]byte(exporttest.List))
	wantES := "format: 1\nsource-commit: " + w.src + "\ninternal-only-sha256: " + list.SHA256 + "\n"
	if string(es) != wantES {
		t.Fatalf("EXPORT-SOURCE is %q\nwant %q", es, wantES)
	}

	// With a version, the file gains exactly the version line.
	trv, err := r.Tree(w.src, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	es, _, _ = r.show(trv.Tree, "EXPORT-SOURCE")
	if string(es) != "format: 1\nversion: v0.2.0\nsource-commit: "+w.src+"\ninternal-only-sha256: "+list.SHA256+"\n" {
		t.Fatalf("EXPORT-SOURCE with a version is %q", es)
	}
	if trv.Tree == tr.Tree {
		t.Fatal("the version is recorded in the tree, so it must change the tree id")
	}
}

// The independent recomputation of plan 3.4 row 3: with nothing but
// `git ls-tree`, drop the listed prefixes and compare (mode, type, blob,
// path) lines with the tool's tree minus EXPORT-SOURCE.
func TestTreeEqualsAnIndependentRecomputation(t *testing.T) {
	w := newWorld(t)
	r := w.clone()
	tr, err := r.Tree(w.src, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	lines := func(rev string) []string {
		var out []string
		for _, l := range strings.Split(w.origin.Git("ls-tree", "-r", rev), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	var want []string
	for _, l := range lines(w.src) {
		p := l[strings.IndexByte(l, '\t')+1:]
		drop := false
		for _, prefix := range []string{".beads/", "docs/plans/", "release/"} {
			if strings.HasPrefix(p, prefix) {
				drop = true
			}
		}
		if !drop && p != "CLAUDE.md" {
			want = append(want, l)
		}
	}
	// The tool's tree lives in the clone; read it there.
	out, err := r.git("ls-tree", "-r", tr.Tree)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasSuffix(l, "\tEXPORT-SOURCE") {
			got = append(got, l)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tree minus EXPORT-SOURCE:\n%s\nindependent recomputation:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(got)+1 != tr.FilesInTree {
		t.Fatalf("EXPORT-SOURCE must be the one added file")
	}
}

type classCase struct {
	class    exportguard.Class
	leak     string // exactly one instance of the class
	reserved string // the reserved example value that stands in for it
}

func classCases() []classCase {
	return []classCase{
		{exportguard.G1, ip4(192, 168, 1, 142), ip4(192, 0, 2, 10)},
		{exportguard.G2, ip4(100, 64, 3, 7), ip4(198, 51, 100, 7)},
		{exportguard.G3, strings.Join([]string{"fd12", "3456", "", "1"}, ":"), "2001:db8::1"},
		{exportguard.G4, cat("BEG", "IN CERTIFICATE"), "<PEM certificate>"},
		{exportguard.G5, cat("https://bot", ":s3cret", "@", "git.corp.zz/repo"), cat("https://bot", ":s3cret", "@", "gitlab.example.internal/repo")},
		{exportguard.G6, cat("someone", "@", "corp.zz"), cat("someone", "@", "example.com")},
		{exportguard.G7, cat("/ho", "me/", "alice/", "src"), "/home/<user>/src"},
		{exportguard.I1, cat("zork", ".lan"), "example.internal"},
		{exportguard.I2, "zork03", "example"},
		{exportguard.I3, cat("lamp", "-ca"), "gitlab-ca"},
		{exportguard.I4, cat("clusters/", "zork"), "clusters/example"},
		{exportguard.I5, cat("admin", "@", "zork"), "admin@example"},
	}
}

// TestEachOfTheTwelveClassesIsRefused: a tree with exactly one instance of a
// class is refused, naming the class, the path and the line; the same tree
// with the reserved example value in its place passes.
func TestEachOfTheTwelveClassesIsRefused(t *testing.T) {
	cases := classCases()
	if len(cases) != 12 || len(exportguard.AllClasses()) != 12 {
		t.Fatalf("the plan counts 12 classes; cases=%d classes=%d", len(cases), len(exportguard.AllClasses()))
	}
	w := newWorld(t)
	type pair struct{ leak, reserved string }
	commits := make([]pair, len(cases))
	for i, c := range cases {
		body := func(v string) string { return "# Setup\n\nline two\nconnect to " + v + " first\nline five\n" }
		commits[i].leak = w.origin.Commit("leak "+string(c.class), map[string]string{"docs/setup.md": body(c.leak)})
		commits[i].reserved = w.origin.Commit("reserved "+string(c.class), map[string]string{"docs/setup.md": body(c.reserved)})
	}
	r := w.clone()
	seen := map[exportguard.Class]bool{}
	for i, c := range cases {
		c, p := c, commits[i]
		seen[c.class] = true
		t.Run(string(c.class), func(t *testing.T) {
			tr, err := r.Tree(p.leak, "")
			if err != nil {
				t.Fatal(err)
			}
			res, err := r.Check(tr, withScanner)
			if err != nil {
				t.Fatal(err)
			}
			if res.OK {
				t.Fatalf("a tree with one %s instance passed", c.class)
			}
			if len(res.Findings) != 1 {
				t.Fatalf("want exactly one finding, got %d: %v", len(res.Findings), res.Findings)
			}
			f := res.Findings[0]
			if f.Class != c.class || f.Path != "docs/setup.md" || f.Line != 4 || f.Col != len("connect to ")+1 || f.Kind != exportguard.KindContent {
				t.Fatalf("finding %+v; want class %s at docs/setup.md line 4 col %d", f, c.class, len("connect to ")+1)
			}
			refusal := res.Refusal()
			if !strings.Contains(refusal, "deny: "+string(c.class)+" ") || !strings.Contains(refusal, "docs/setup.md:4:") {
				t.Fatalf("the refusal does not name the class, the path and the line:\n%s", refusal)
			}
			if strings.Contains(refusal, c.leak) {
				t.Fatalf("the refusal repeats the matched text")
			}
			for _, s := range res.Summary {
				want := 0
				if s.Class == c.class {
					want = 1
				}
				if s.Matches != want {
					t.Fatalf("summary: class %s has %d matches, want %d", s.Class, s.Matches, want)
				}
			}

			tr, err = r.Tree(p.reserved, "")
			if err != nil {
				t.Fatal(err)
			}
			res, err = r.Check(tr, withScanner)
			if err != nil {
				t.Fatal(err)
			}
			if !res.OK {
				t.Fatalf("the same tree with the reserved value was refused:\n%s", res.Refusal())
			}
		})
	}
	for _, c := range exportguard.AllClasses() {
		if !seen[c] {
			t.Errorf("class %s has no case", c)
		}
	}
}

func TestAPathIsRefusedLikeContent(t *testing.T) {
	w := newWorld(t)
	name := cat("docs/hosts/", "gitlab.", "zork", ".lan", ".md")
	src := w.origin.Commit("a path with an internal name", map[string]string{name: "nothing wrong in here\n"})
	r := w.clone()
	tr, err := r.Tree(src, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Check(tr, withScanner)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || len(res.Findings) != 1 || res.Findings[0].Kind != exportguard.KindPath || res.Findings[0].Class != exportguard.I1 || res.Findings[0].Path != name {
		t.Fatalf("want one I1 path finding, got ok=%v %v", res.OK, res.Findings)
	}
}

func TestInternalOnlyPathsNeverReachTheTreeAndAStrayOneIsRefused(t *testing.T) {
	list, err := exportguard.ParseList([]byte(exporttest.List))
	if err != nil {
		t.Fatal(err)
	}
	blob := func(n int) string { return fmt.Sprintf("%040x", n) }
	file := func(p string, n int) treeEntry { return treeEntry{Mode: "100644", Type: "blob", ID: blob(n), Path: p} }
	src := []treeEntry{
		file(".beads/issues.jsonl", 1), file("CLAUDE.md", 2), file("README.md", 3),
		file("docs/plans/p.md", 4), file("main.go", 5),
		file("release/deny-internal.txt", 6), file("release/internal-only.txt", 7),
	}
	es := file("EXPORT-SOURCE", 99)
	good := []treeEntry{es, file("README.md", 3), file("main.go", 5)}
	data := map[string]string{blob(6): "release/deny-internal.txt", blob(7): "release/internal-only.txt"}

	if err := checkPublicTree(list, src, good, es.ID, data); err != nil {
		t.Fatalf("the correct public tree was refused: %v", err)
	}
	with := func(extra ...treeEntry) []treeEntry { return append(append([]treeEntry{}, good...), extra...) }
	cases := []struct {
		name string
		tree []treeEntry
		want string
	}{
		{"a listed file is present", with(file("CLAUDE.md", 2)), "internal-only path CLAUDE.md is present"},
		{"a file under a listed directory is present", with(file("docs/plans/p.md", 4)), "internal-only path docs/plans/p.md is present"},
		{"a NEW file under a listed directory is present", with(file(".beads/new", 50)), "internal-only path .beads/new is present"},
		{"the deny data is present at its path", with(file("release/deny-internal.txt", 6)), "internal-only path release/deny-internal.txt is present"},
		{"the deny data is present under another name", with(file("docs/rules.txt", 6)), "docs/rules.txt is a copy of the export data file release/deny-internal.txt"},
		{"the list is present under another name", with(file("docs/list.txt", 7)), "docs/list.txt is a copy of the export data file release/internal-only.txt"},
		{"an extra file", with(file("extra.txt", 51)), "the recomputation expects 3"},
		{"a missing file", []treeEntry{es, file("README.md", 3)}, "the recomputation expects 3"},
		{"a changed blob", []treeEntry{es, file("README.md", 3), file("main.go", 52)}, "differs from the recomputation at main.go"},
		{"a changed mode", []treeEntry{es, file("README.md", 3), {Mode: "100755", Type: "blob", ID: blob(5), Path: "main.go"}}, "differs from the recomputation at main.go"},
		{"a renamed file", []treeEntry{es, file("README.md", 3), file("main2.go", 5)}, "differs from the recomputation at main2.go"},
		{"no EXPORT-SOURCE", []treeEntry{file("README.md", 3), file("main.go", 5)}, "not a public tree"},
		{"a different EXPORT-SOURCE", []treeEntry{file("EXPORT-SOURCE", 98), file("README.md", 3), file("main.go", 5)}, "differs from the recomputation at EXPORT-SOURCE"},
		{"a submodule", []treeEntry{es, file("README.md", 3), {Mode: "160000", Type: "commit", ID: blob(5), Path: "main.go"}}, "is a commit, not a file"},
	}
	for _, c := range cases {
		err := checkPublicTree(list, src, c.tree, es.ID, data)
		if err == nil || !IsRefused(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want a refusal mentioning %q, got %v", c.name, c.want, err)
		}
	}
}

func TestTheDenyDataCanNeverBeExported(t *testing.T) {
	t.Run("a list that does not remove the data directory is refused", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("list without release/", map[string]string{
			"release/internal-only.txt": strings.Replace(exporttest.List, "release/     # the export's own data\n", "", 1),
		})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "release/", "must be listed")
	})
	t.Run("a list that removes only one data file is refused", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("list with one file", map[string]string{
			"release/internal-only.txt": strings.Replace(exporttest.List, "release/ ", "release/internal-only.txt ", 1),
		})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "must be listed")
	})
	t.Run("a copy of the deny data under another name is refused", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("copy the deny data", map[string]string{"docs/rules.txt": exporttest.Deny})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "docs/rules.txt is a copy of the export data file release/deny-internal.txt")
	})
	t.Run("a copy of the list under another name is refused", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("copy the list", map[string]string{"docs/list.txt": exporttest.List})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "docs/list.txt is a copy of the export data file release/internal-only.txt")
	})
	t.Run("an edited copy of the deny data is refused by its own rules", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("edited copy", map[string]string{"docs/rules.txt": "# my notes\n" + exporttest.Deny})
		r := w.clone()
		tr, err := r.Tree(src, "")
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Check(tr, withScanner)
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || len(res.Findings) == 0 {
			t.Fatal("an edited copy of the deny data passed the check")
		}
		for _, f := range res.Findings {
			if f.Path != "docs/rules.txt" {
				t.Fatalf("unexpected finding %v", f)
			}
		}
	})
	t.Run("no blob of the exported tree is a data file, and none sits under release/", func(t *testing.T) {
		w := newWorld(t)
		r := w.clone()
		tr, err := r.Tree(w.src, "v0.2.0")
		if err != nil {
			t.Fatal(err)
		}
		dataIDs := map[string]bool{}
		for _, p := range []string{"release/internal-only.txt", "release/deny-internal.txt", "release/allow.txt"} {
			dataIDs[w.origin.Git("rev-parse", w.src+":"+p)] = true
		}
		for _, e := range tr.entries {
			if strings.HasPrefix(e.Path, "release/") || dataIDs[e.ID] {
				t.Fatalf("export data in the public tree: %s", e.Path)
			}
		}
	})
}

func TestTreeRefusals(t *testing.T) {
	t.Run("a stale list entry", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("stale", map[string]string{
			"release/internal-only.txt": exporttest.List + "docs/handoff/ # gone\n",
		})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "stale", "docs/handoff/")
	})
	t.Run("a file entry that names a directory", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("dir as file", map[string]string{
			"release/internal-only.txt": strings.Replace(exporttest.List, "docs/plans/ ", "docs/plans ", 1),
		})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "stale", "docs/plans")
	})
	t.Run("an entry inside the sealed chart", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("chart entry", map[string]string{
			"chart/gonk/smoke/notes.md": "notes\n",
			"release/internal-only.txt": exporttest.List + "chart/gonk/smoke/notes.md # internal notes\n",
		})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "chart/gonk/", "exported whole")
	})
	t.Run("an entry inside a directory an image build copies", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("copied entry", map[string]string{
			"cmd/tool/internal_notes.md": "notes\n",
			"release/internal-only.txt":  exporttest.List + "cmd/tool/internal_notes.md # internal notes\n",
		})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "cmd/", "exported whole")
	})
	t.Run("an entry that is a file an image build copies", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("copied file", map[string]string{
			"release/internal-only.txt": exporttest.List + "go.mod # no\n",
		})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "go.mod", "exported whole")
	})
	t.Run("EXPORT-SOURCE committed in the source", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("committed", map[string]string{"EXPORT-SOURCE": "format: 1\n"})
		_, err := w.clone().Tree(src, "")
		mustRefuse(t, err, "EXPORT-SOURCE already exists")
	})
	t.Run("no list at the source", func(t *testing.T) {
		w := newWorld(t)
		_, err := w.clone().Tree(w.baseline, "")
		mustRefuse(t, err, "release/internal-only.txt does not exist")
	})
	t.Run("a source that is not a full commit id", func(t *testing.T) {
		w := newWorld(t)
		r := w.clone()
		for _, bad := range []string{w.src[:12], "main", "HEAD", strings.ToUpper(w.src), w.src + "0", ""} {
			_, err := r.Tree(bad, "")
			mustRefuse(t, err, "full 40-hex")
		}
	})
	t.Run("a source that origin's main does not have", func(t *testing.T) {
		w := newWorld(t)
		_, err := w.clone().Tree(strings.Repeat("ab", 20), "")
		mustRefuse(t, err, "is not a commit")
	})
	t.Run("a source that exists but is not on origin's main", func(t *testing.T) {
		w := newWorld(t)
		w.origin.Git("checkout", "--quiet", "-b", "side")
		side := w.origin.Commit("side work", map[string]string{"side.txt": "x\n"})
		w.origin.Git("checkout", "--quiet", "main")
		r := w.clone()
		// Bring the side commit's objects into the clone without putting it
		// on main, as a stray fetch would.
		if _, err := netGit(r.dir, "fetch", "--quiet", "--", w.origin.Dir, "side"); err != nil {
			t.Fatal(err)
		}
		_, err := r.Tree(side, "")
		mustRefuse(t, err, "is not on origin/main")
	})
	t.Run("a version that is not vX.Y.Z", func(t *testing.T) {
		w := newWorld(t)
		_, err := w.clone().Tree(w.src, "0.2.0")
		mustRefuse(t, err, "vX.Y.Z")
	})
}

func TestCheckRefusals(t *testing.T) {
	t.Run("the scanner's findings refuse the tree, by rule and place", func(t *testing.T) {
		w := newWorld(t)
		r := w.clone()
		tr, err := r.Tree(w.src, "")
		if err != nil {
			t.Fatal(err)
		}
		var sawDir string
		var sawFiles []string
		res, err := r.Check(tr, CheckOptions{Scanner: func(dir string) ([]ScanFinding, error) {
			sawDir = dir
			_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					rel, _ := filepath.Rel(dir, p)
					sawFiles = append(sawFiles, filepath.ToSlash(rel))
				}
				return nil
			})
			return []ScanFinding{{Rule: "generic-api-key", File: "cmd/tool/main.go", Line: 3}}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || !res.ScannerRan || len(res.ScanFindings) != 1 || len(res.Findings) != 0 {
			t.Fatalf("want a refusal on the scanner finding alone, got ok=%v %+v", res.OK, res)
		}
		if !strings.Contains(res.Refusal(), "scanner: X1 generic-api-key cmd/tool/main.go:3") {
			t.Fatalf("refusal: %s", res.Refusal())
		}
		// The scanner was handed exactly the public tree.
		sort.Strings(sawFiles)
		want := paths(tr)
		sort.Strings(want)
		if strings.Join(sawFiles, "\n") != strings.Join(want, "\n") {
			t.Fatalf("the scanner saw:\n%s\nthe tree is:\n%s", strings.Join(sawFiles, "\n"), strings.Join(want, "\n"))
		}
		if _, err := os.Stat(sawDir); !os.IsNotExist(err) {
			t.Fatalf("the materialised tree was left behind at %s", sawDir)
		}
	})
	t.Run("a scanner that fails is an error, not a pass", func(t *testing.T) {
		w := newWorld(t)
		r := w.clone()
		tr, _ := r.Tree(w.src, "")
		res, err := r.Check(tr, CheckOptions{Scanner: func(string) ([]ScanFinding, error) { return nil, fmt.Errorf("boom") }})
		if err == nil || res != nil {
			t.Fatalf("want an error, got %v %v", res, err)
		}
	})
	t.Run("no scanner configured is an error, not a pass", func(t *testing.T) {
		w := newWorld(t)
		r := w.clone()
		tr, _ := r.Tree(w.src, "")
		if res, err := r.Check(tr, CheckOptions{}); err == nil {
			t.Fatalf("want an error, got %+v", res)
		}
	})
	t.Run("a guard-only check is never OK, and says the scanner did not run", func(t *testing.T) {
		w := newWorld(t)
		r := w.clone()
		tr, _ := r.Tree(w.src, "")
		res, err := r.Check(tr, CheckOptions{SkipScanner: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || res.ScannerRan || !res.GuardClean() || !strings.Contains(res.Refusal(), "NOT run") {
			t.Fatalf("ok=%v ran=%v guardClean=%v\n%s", res.OK, res.ScannerRan, res.GuardClean(), res.Refusal())
		}
	})
	t.Run("a stale allow-list entry", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("stale allow", map[string]string{
			"release/allow.txt": "path\tREADME.md\t" + cat("old", "@", "corp.zz") + "\tno longer there\n",
		})
		r := w.clone()
		tr, _ := r.Tree(src, "")
		res, err := r.Check(tr, withScanner)
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || res.GuardClean() || !strings.Contains(res.Refusal(), "excuses nothing") {
			t.Fatalf("a stale allow entry passed:\n%s", res.Refusal())
		}
	})
	t.Run("an allow-list entry excuses its literal in its file only", func(t *testing.T) {
		w := newWorld(t)
		addr := cat("maintainer", "@", "corp.zz")
		src := w.origin.Commit("allowed", map[string]string{
			"chart/gonk/Chart.yaml": "name: gonk\nmaintainers:\n  - email: " + addr + "\n",
			"release/allow.txt":     "path\tchart/gonk/Chart.yaml\t" + addr + "\tmaintainer field\n",
		})
		src2 := w.origin.Commit("and elsewhere", map[string]string{"docs/guide.md": "write to " + addr + "\n"})
		r := w.clone()
		tr, _ := r.Tree(src, "")
		res, err := r.Check(tr, withScanner)
		if err != nil || !res.OK {
			t.Fatalf("the allowed literal was refused: %v\n%s", err, res.Refusal())
		}
		tr, _ = r.Tree(src2, "")
		res, err = r.Check(tr, withScanner)
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || len(res.Findings) != 1 || res.Findings[0].Path != "docs/guide.md" || res.Findings[0].Class != exportguard.G6 {
			t.Fatalf("want one G6 finding in docs/guide.md, got %v", res.Findings)
		}
	})
	t.Run("a scanner configuration in the tree needs an explicit allow-list entry", func(t *testing.T) {
		w := newWorld(t)
		bare := w.origin.Commit("scanner config", map[string]string{".gitleaks.toml": "[extend]\nuseDefault = true\n"})
		allowed := w.origin.Commit("and the entry", map[string]string{"release/allow.txt": "scanner-config\t.gitleaks.toml\tQ4 (a), decided by the owner\n"})
		stale := w.origin.Commit("entry without the file", map[string]string{".gitleaks.toml": exporttest.DELETE})
		ignore := w.origin.Commit("an ignore file", map[string]string{".gitleaksignore": "abc:README.md:rule:1\n", "release/allow.txt": "# none\n"})
		r := w.clone()
		check := func(src string) *CheckResult {
			t.Helper()
			tr, err := r.Tree(src, "")
			if err != nil {
				t.Fatal(err)
			}
			res, err := r.Check(tr, withScanner)
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		if res := check(bare); res.OK || !strings.Contains(res.Refusal(), ".gitleaks.toml is in the exported tree and changes what the secret scanner accepts") {
			t.Fatalf("a scanner configuration passed without an entry:\n%s", res.Refusal())
		}
		if res := check(allowed); !res.OK {
			t.Fatalf("an allowed scanner configuration was refused:\n%s", res.Refusal())
		}
		if res := check(stale); res.OK || !strings.Contains(res.Refusal(), "scanner-config .gitleaks.toml) excuses nothing") {
			t.Fatalf("a scanner-config entry without the file passed:\n%s", res.Refusal())
		}
		if res := check(ignore); res.OK || !strings.Contains(res.Refusal(), ".gitleaksignore is in the exported tree") {
			t.Fatalf("an ignore file passed:\n%s", res.Refusal())
		}
	})
	t.Run("malformed deny data", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("bad deny", map[string]string{"release/deny-internal.txt": "I1 zone * zork\n"})
		r := w.clone()
		tr, _ := r.Tree(src, "")
		_, err := r.Check(tr, withScanner)
		mustRefuse(t, err, "has no rule")
	})
	t.Run("missing deny data", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("no deny", map[string]string{"release/deny-internal.txt": exporttest.DELETE})
		r := w.clone()
		tr, err := r.Tree(src, "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Check(tr, withScanner)
		mustRefuse(t, err, "release/deny-internal.txt does not exist")
	})
	t.Run("missing allow-list", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("no allow", map[string]string{"release/allow.txt": exporttest.DELETE})
		r := w.clone()
		tr, _ := r.Tree(src, "")
		_, err := r.Check(tr, withScanner)
		mustRefuse(t, err, "release/allow.txt does not exist")
	})
	t.Run("UTF-16 text cannot be read and is refused", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("utf16", map[string]string{"docs/win.txt": "\xff\xfeh\x00i\x00"})
		r := w.clone()
		tr, _ := r.Tree(src, "")
		res, err := r.Check(tr, withScanner)
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || !strings.Contains(res.Refusal(), "docs/win.txt is UTF-16") {
			t.Fatalf("UTF-16 text passed:\n%s", res.Refusal())
		}
	})
	t.Run("a binary blob is scanned", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("binary", map[string]string{"bin/blob.dat": "\x00\x01\x02" + ip4(10, 9, 8, 7) + "\x00\x00"})
		r := w.clone()
		tr, _ := r.Tree(src, "")
		res, err := r.Check(tr, withScanner)
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || len(res.Findings) != 1 || res.Findings[0].Path != "bin/blob.dat" || res.Findings[0].Class != exportguard.G1 {
			t.Fatalf("want one G1 finding in the binary blob, got %v", res.Findings)
		}
	})
	t.Run("the report counts public files that mention an internal-only path", func(t *testing.T) {
		w := newWorld(t)
		src := w.origin.Commit("dangling", map[string]string{"docs/guide.md": "see docs/plans/plan.md and CLAUDE.md\n", "README.md": "see .beads/ too\n"})
		r := w.clone()
		tr, _ := r.Tree(src, "")
		res, err := r.Check(tr, withScanner)
		if err != nil || !res.OK {
			t.Fatalf("%v %v", res, err)
		}
		if res.DanglingRefs != 2 {
			t.Fatalf("want 2 files referencing internal-only paths, got %d", res.DanglingRefs)
		}
	})
	t.Run("the report is JSON and repeats no matched text", func(t *testing.T) {
		w := newWorld(t)
		leak := ip4(10, 20, 30, 40)
		src := w.origin.Commit("leak", map[string]string{"docs/guide.md": "x " + leak + "\n"})
		r := w.clone()
		tr, _ := r.Tree(src, "")
		res, _ := r.Check(tr, withScanner)
		js, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(js), leak) || !strings.Contains(string(js), `"class":"G1"`) || !strings.Contains(string(js), `"tree":"`+tr.Tree+`"`) {
			t.Fatalf("report: %s", js)
		}
	})
}
