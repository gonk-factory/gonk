package buildgate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard/gitexport"
	"gopkg.in/yaml.v3"
)

// The release notes of a version are ONE file, docs/releases/<tag>.md, and
// three things read it: the export (its first paragraph is the body of the
// public release commit), the release workflow (it is the text of the GitHub
// Release), and people. These tests hold the three to the same file.

// releaseNotesStep returns the script of the release workflow's step that
// creates the Release.
func releaseNotesStep(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, step := range doc.Jobs["publish"].Steps {
		if strings.Contains(step.Run, "gh release create") {
			return step.Run
		}
	}
	t.Fatal("release.yml: no step of the `publish` job runs `gh release create`")
	return ""
}

func TestTheReleaseWorkflowPublishesTheNotesFileOfTheTag(t *testing.T) {
	script := releaseNotesStep(t)
	if strings.Contains(script, "<<") {
		t.Error("the release step has a heredoc: release notes are a file in the release commit, never text in the workflow")
	}
	for _, want := range []string{`notes="docs/releases/${TAG}.md"`, `--notes-file /tmp/notes.md`, `cp "$notes" /tmp/notes.md`} {
		if !strings.Contains(script, want) {
			t.Errorf("the release step does not contain %s", want)
		}
	}

	// Run the part that chooses the notes, for real.
	start := strings.Index(script, `notes="docs/releases/${TAG}.md"`)
	end := strings.Index(script, `cp "$notes" /tmp/notes.md`)
	if start < 0 || end < start {
		t.Fatal("cannot find the notes selection in the release step")
	}
	for _, c := range []struct {
		name, content string
		wantExit      int
		wantOut       string
	}{
		{"the notes of the tag", "# gonk v9.8.7\n\nA release.\n", 0, "chosen"},
		{"no notes file", "", 1, "No release notes"},
		{"an empty notes file", "\x00empty", 1, "No release notes"},
		{"another release's notes", "# gonk v9.8.6\n\nThe previous release.\n", 1, "Wrong release notes"},
		{"a prefix of the tag", "# gonk v9.8.70\n\nx\n", 1, "Wrong release notes"},
	} {
		dir := t.TempDir()
		if c.content != "" {
			body := c.content
			if body == "\x00empty" {
				body = ""
			}
			if err := os.MkdirAll(filepath.Join(dir, "docs", "releases"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "docs", "releases", "v9.8.7.md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		exit, out := runSh(t, "set -eu\ncd '"+dir+"'\n"+script[start:end]+"\necho chosen\n", "TAG=v9.8.7")
		if exit != c.wantExit || !strings.Contains(out, c.wantOut) {
			t.Errorf("%s: exit %d, want %d; output %q must contain %q", c.name, exit, c.wantExit, out, c.wantOut)
		}
	}
}

var reGonkVersion = regexp.MustCompile(`(?m)^GONK_VERSION=(v[0-9]+\.[0-9]+\.[0-9]+)$`)

// Every notes file names its own version, has the paragraph the release
// commit is made from, and has a changelog section. And the version this tree
// would release as has notes -- so the commit that bumps the version cannot
// forget them. (v0.1.0 is the one version with none: it predates the file.)
func TestEveryReleaseHasItsNotesAndItsChangelogSection(t *testing.T) {
	root := repoRoot(t)
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "docs", "releases", "v*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no docs/releases/v*.md at all")
	}
	have := map[string]bool{}
	for _, f := range files {
		version := strings.TrimSuffix(filepath.Base(f), ".md")
		have[version] = true
		for _, p := range releaseNotesProblems(version, mustRead(t, f), changelog) {
			t.Errorf("%s: %s", filepath.Base(f), p)
		}
	}
	env := mustRead(t, filepath.Join(root, "images", "versions.env"))
	m := reGonkVersion.FindSubmatch(env)
	if m == nil {
		t.Fatal("images/versions.env has no GONK_VERSION=vX.Y.Z line")
	}
	if v := string(m[1]); v != "v0.1.0" && !have[v] {
		t.Errorf("images/versions.env says GONK_VERSION=%s and docs/releases/%s.md does not exist: the version bump and its notes are one commit", v, v)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func releaseNotesProblems(version string, notes, changelog []byte) []string {
	var problems []string
	first, _, _ := strings.Cut(string(notes), "\n")
	if first != "# gonk "+version {
		problems = append(problems, "the first line is "+first+", want `# gonk "+version+"` (the release workflow refuses anything else)")
	}
	para, err := gitexport.FirstParagraph(notes)
	switch {
	case err != nil:
		problems = append(problems, "no first paragraph: "+err.Error())
	case strings.HasPrefix(para, "#") || len(para) < 40:
		problems = append(problems, "the first paragraph is the body of the public release commit and must say what the release is; got: "+para)
	}
	section := "## [" + strings.TrimPrefix(version, "v") + "]"
	if !strings.Contains("\n"+string(changelog), "\n"+section+"\n") {
		problems = append(problems, "CHANGELOG.md has no `"+section+"` section")
	}
	return problems
}

func TestTheReleaseNotesCheckActuallyFails(t *testing.T) {
	good := []byte("# gonk v1.2.3\n\nThe release that does a thing worth a whole sentence.\n\n## More\n")
	log := []byte("# Changelog\n\n## [1.2.3]\n\n- x\n")
	if p := releaseNotesProblems("v1.2.3", good, log); len(p) != 0 {
		t.Fatalf("good notes refused: %v", p)
	}
	for name, c := range map[string][2][]byte{
		"another version's heading": {[]byte("# gonk v1.2.4\n\nThe release that does a thing worth a whole sentence.\n"), log},
		"no heading":                {[]byte("The release that does a thing worth a whole sentence.\n"), log},
		"no paragraph":              {[]byte("# gonk v1.2.3\n\n## Added\n"), log},
		"a stub paragraph":          {[]byte("# gonk v1.2.3\n\nTBD.\n"), log},
		"no changelog section":      {good, []byte("# Changelog\n\n## [1.2.30]\n")},
		"changelog section inline":  {good, []byte("# Changelog\n\nsee ## [1.2.3] below\n")},
	} {
		if len(releaseNotesProblems("v1.2.3", c[0], c[1])) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
}
