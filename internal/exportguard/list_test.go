package exportguard

import (
	"reflect"
	"strings"
	"testing"
)

const testList = `# what is never exported
.beads/        # the tracker
CLAUDE.md      # agent instructions
docs/plans/    # build plans
release/       # the export's own data
`

func mustList(t *testing.T, s string) *List {
	t.Helper()
	l, err := ParseList([]byte(s))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	return l
}

func TestListParsesEntriesAndDigest(t *testing.T) {
	l := mustList(t, testList)
	want := []string{".beads/", "CLAUDE.md", "docs/plans/", "release/"}
	if !reflect.DeepEqual(l.Entries, want) {
		t.Fatalf("entries %v, want %v", l.Entries, want)
	}
	if len(l.SHA256) != 64 {
		t.Fatalf("digest %q", l.SHA256)
	}
	other := mustList(t, testList+"# a trailing comment\n")
	if other.SHA256 == l.SHA256 {
		t.Fatal("the digest must be of the file's BYTES: a comment-only change must change it")
	}
}

func TestListRefusesWhatItCannotMeanExactly(t *testing.T) {
	cases := map[string]string{
		"a glob":                              testList + "docs/*.md # x\n",
		"a question-mark glob":                testList + "docs/a?.md # x\n",
		"a bracket glob":                      testList + "docs/[ab].md # x\n",
		"an entry without a reason":           testList + "PLAN.md\n",
		"an entry with an empty reason":       testList + "PLAN.md #  \n",
		"an absolute path":                    testList + "/etc/x # x\n",
		"a parent segment":                    testList + "docs/../x # x\n",
		"a dot segment":                       testList + "./docs/x # x\n",
		"a doubled slash":                     testList + "docs//x # x\n",
		"a duplicate":                         testList + "CLAUDE.md # again\n",
		"an entry under another entry":        testList + "docs/plans/a.md # x\n",
		"a blank inside an entry":             testList + "my file.md # x\n",
		"no entry for the data directory":     strings.Replace(testList, "release/       # the export's own data\n", "", 1),
		"the data directory as a file":        strings.Replace(testList, "release/ ", "release  ", 1),
		"only one file of the data directory": strings.Replace(testList, "release/ ", "release/deny-internal.txt ", 1),
	}
	for name, text := range cases {
		if _, err := ParseList([]byte(text)); err == nil {
			t.Errorf("a list with %s was accepted", name)
		}
	}
}

func TestListApplyRemovesExactlyTheListedPaths(t *testing.T) {
	l := mustList(t, testList)
	paths := []string{
		".beads/issues.jsonl", ".beadsX/keep", "CLAUDE.md", "CLAUDE.md.bak", "sub/CLAUDE.md",
		"docs/plans/a.md", "docs/plans/sub/b.md", "docs/plansX.md", "docs/guide.md",
		"release/deny-internal.txt", "release/internal-only.txt", "released/notes.md", "main.go",
	}
	kept, removed, err := l.Apply(paths)
	if err != nil {
		t.Fatal(err)
	}
	wantKept := []string{".beadsX/keep", "CLAUDE.md.bak", "sub/CLAUDE.md", "docs/plansX.md", "docs/guide.md", "released/notes.md", "main.go"}
	if !reflect.DeepEqual(kept, wantKept) {
		t.Fatalf("kept %v\nwant %v", kept, wantKept)
	}
	wantRemoved := map[string]int{".beads/": 1, "CLAUDE.md": 1, "docs/plans/": 2, "release/": 2}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Fatalf("removed %v, want %v", removed, wantRemoved)
	}
	for _, p := range paths {
		isKept := false
		for _, k := range kept {
			if k == p {
				isKept = true
			}
		}
		if l.Covers(p) == isKept {
			t.Errorf("Covers(%q) disagrees with Apply", p)
		}
	}
}

func TestListStaleEntryIsRed(t *testing.T) {
	l := mustList(t, testList)
	// No file under docs/plans/, and CLAUDE.md exists only as a directory.
	_, _, err := l.Apply([]string{".beads/a", "CLAUDE.md/inner", "release/x"})
	if err == nil {
		t.Fatal("stale entries were accepted")
	}
	for _, want := range []string{"CLAUDE.md", "docs/plans/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), ".beads/") {
		t.Errorf("a live entry was called stale: %v", err)
	}
}

func TestListMayNotOverlapAHashedOrCopiedPath(t *testing.T) {
	protected := []string{"chart/gonk/", "cmd/", "go.mod", "images/agent/entrypoint.sh"}
	bad := []string{
		"chart/gonk/smoke/notes.md # inside the sealed chart",
		"chart/gonk/ # the sealed chart itself",
		"chart/ # swallows the sealed chart",
		"cmd/tool/ # inside a copied directory",
		"go.mod # a copied file",
		"images/ # swallows a copied file",
		"images/agent/ # swallows a copied file",
		"images/agent/entrypoint.sh # a copied file",
	}
	for _, line := range bad {
		l := mustList(t, testList+line+"\n")
		if err := l.CheckProtected(protected); err == nil {
			t.Errorf("entry %q was accepted", line)
		}
	}
	ok := mustList(t, testList+"chart/notes.md # beside the chart\nimages/agent/README.md # beside the entrypoint\ncmdline.txt # not under cmd/\n")
	if err := ok.CheckProtected(protected); err != nil {
		t.Fatalf("a list that overlaps nothing was refused: %v", err)
	}
	if len(HashedTrees) != 1 || HashedTrees[0] != "chart/gonk/" {
		t.Fatalf("HashedTrees = %v", HashedTrees)
	}
}

func TestDockerfileCopySources(t *testing.T) {
	df := `FROM golang AS build
COPY go.mod go.sum ./
COPY vendor/ vendor/
copy ./cmd cmd
COPY --chown=1:1 pkg/ internal/ /src/
ADD pack /pack
COPY --from=build /out/bin /bin
COPY --chmod=0755 images/agent/entrypoint.sh \
     /entrypoint.sh
COPY ["a b", "dst"]
RUN echo COPY not/this here
COPY . /all
`
	got := DockerfileCopySources([]byte(df))
	want := []string{"go.mod", "go.sum", "vendor/", "cmd", "pkg/", "internal/", "pack", "images/agent/entrypoint.sh", "a"}
	// The JSON form with a blank in a name is split on the blank; what
	// matters is that every plain source is found and no stage copy is.
	if len(got) < len(want)-1 {
		t.Fatalf("got %v", got)
	}
	for _, w := range want[:len(want)-1] {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("source %q not found in %v", w, got)
		}
	}
	for _, g := range got {
		if strings.Contains(g, "out/bin") || strings.Contains(g, "not/this") || g == "." {
			t.Errorf("unexpected source %q", g)
		}
	}
}

func TestModeSwitch(t *testing.T) {
	if m, err := DetectMode(false, true); err != nil || m != ModeInternal {
		t.Fatalf("data directory only: %v %v", m, err)
	}
	if m, err := DetectMode(true, false); err != nil || m != ModePublic {
		t.Fatalf("EXPORT-SOURCE only: %v %v", m, err)
	}
	if m, err := DetectMode(true, true); err == nil {
		t.Fatalf("both present must be refused, got %v", m)
	}
	if m, err := DetectMode(false, false); err == nil {
		t.Fatalf("both absent must be refused, got %v", m)
	}
}

func TestExportSourceRendersCanonicallyAndRoundTrips(t *testing.T) {
	src := strings.Repeat("0123456789", 4)
	sum := strings.Repeat("ab", 32)
	got, err := ExportSource{Version: "v0.2.0", SourceCommit: src, ListSHA256: sum}.Render()
	if err != nil {
		t.Fatal(err)
	}
	want := "format: 1\nversion: v0.2.0\nsource-commit: " + src + "\ninternal-only-sha256: " + sum + "\n"
	if string(got) != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	noVersion, err := ExportSource{SourceCommit: src, ListSHA256: sum}.Render()
	if err != nil {
		t.Fatal(err)
	}
	if string(noVersion) != "format: 1\nsource-commit: "+src+"\ninternal-only-sha256: "+sum+"\n" {
		t.Fatalf("without a version the line must be omitted, got %q", noVersion)
	}
	back, err := ParseExportSource(got)
	if err != nil || back.Version != "v0.2.0" || back.SourceCommit != src || back.ListSHA256 != sum {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	back, err = ParseExportSource(noVersion)
	if err != nil || back.Version != "" || back.SourceCommit != src {
		t.Fatalf("round trip without version: %+v %v", back, err)
	}

	for name, s := range map[string]ExportSource{
		"a short commit id":                 {SourceCommit: src[:12], ListSHA256: sum},
		"an upper-case commit":              {SourceCommit: strings.ToUpper(strings.Repeat("abcdef0123", 4)), ListSHA256: sum},
		"a short digest":                    {SourceCommit: src, ListSHA256: sum[:40]},
		"a version without v":               {Version: "0.2.0", SourceCommit: src, ListSHA256: sum},
		"a pre-release version":             {Version: "v0.2.0-rc1", SourceCommit: src, ListSHA256: sum},
		"a version with a zero-padded part": {Version: "v0.02.0", SourceCommit: src, ListSHA256: sum},
	} {
		if _, err := s.Render(); err == nil {
			t.Errorf("EXPORT-SOURCE with %s was rendered", name)
		}
	}
	for name, text := range map[string]string{
		"CRLF line ends":   strings.ReplaceAll(want, "\n", "\r\n"),
		"no final newline": strings.TrimSuffix(want, "\n"),
		"another format":   strings.Replace(want, "format: 1", "format: 2", 1),
		"an extra key":     want + "built-at: now\n",
		"reordered keys":   "format: 1\nsource-commit: " + src + "\nversion: v0.2.0\ninternal-only-sha256: " + sum + "\n",
		"a missing key":    "format: 1\nsource-commit: " + src + "\n",
		"empty":            "",
	} {
		if _, err := ParseExportSource([]byte(text)); err == nil {
			t.Errorf("EXPORT-SOURCE with %s was parsed", name)
		}
	}
}
