package scope

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// gonk-9q0y. Until 2026-10-12 only the root .gitlab-ci.yml was read: a file
// included BY an included file was not class `ci`, and the apply's
// "definition unchanged" shortcut never compared it.

// repo is an in-memory tree for ResolveCILocalIncludes's reader.
type repo struct {
	files map[string]string
	reads []string
	fail  map[string]error
}

func (r *repo) read(p string) ([]byte, bool, error) {
	r.reads = append(r.reads, p)
	if err := r.fail[p]; err != nil {
		return nil, false, err
	}
	s, ok := r.files[p]
	return []byte(s), ok, nil
}

func TestResolveCILocalIncludes(t *testing.T) {
	boom := errors.New("gitlab: 500")
	cases := []struct {
		name     string
		root     string
		files    map[string]string
		fail     map[string]error
		want     []string
		complete bool
		err      string
		reads    []string
	}{
		{name: "no include: nothing, complete, nothing read",
			root: "test:\n  script: [true]\n", complete: true},
		{name: "one level, as before",
			root:  "include:\n  - local: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "a:\n  script: [true]\n"},
			want:  []string{"ci/a.yml"}, complete: true, reads: []string{"ci/a.yml"}},
		{name: "THE DEFECT: an include of an include is pipeline definition too",
			root: "include:\n  - local: ci/a.yml\n",
			files: map[string]string{
				"ci/a.yml":         "include:\n  - local: /ci/nested/b.yml\n",
				"ci/nested/b.yml":  "include: ci/deep/c.yml\n",
				"ci/deep/c.yml":    "c:\n  script: [true]\n",
				"ci/unrelated.yml": "include: ci/never.yml\n",
			},
			want: []string{"ci/a.yml", "ci/nested/b.yml", "ci/deep/c.yml"}, complete: true,
			reads: []string{"ci/a.yml", "ci/nested/b.yml", "ci/deep/c.yml"}},
		{name: "a nested path is rooted at the repository, not at the including file",
			root:  "include: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "include: b.yml\n", "b.yml": "b: {script: [true]}\n", "ci/b.yml": "include: ci/wrong.yml\n"},
			want:  []string{"ci/a.yml", "b.yml"}, complete: true, reads: []string{"ci/a.yml", "b.yml"}},
		{name: "breadth first, the root's own entries first, each file once",
			root: "include:\n  - ci/a.yml\n  - ci/b.yml\n",
			files: map[string]string{
				"ci/a.yml": "include:\n  - ci/c.yml\n  - ci/b.yml\n",
				"ci/b.yml": "include: ci/c.yml\n",
				"ci/c.yml": "c: {script: [true]}\n",
			},
			want: []string{"ci/a.yml", "ci/b.yml", "ci/c.yml"}, complete: true,
			reads: []string{"ci/a.yml", "ci/b.yml", "ci/c.yml"}},
		{name: "a cycle ends",
			root:  "include: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "include: ci/b.yml\n", "ci/b.yml": "include: /ci/a.yml\n"},
			want:  []string{"ci/a.yml", "ci/b.yml"}, complete: true, reads: []string{"ci/a.yml", "ci/b.yml"}},
		{name: "a file that includes itself",
			root:  "include: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "include: ci/a.yml\n"},
			want:  []string{"ci/a.yml"}, complete: true, reads: []string{"ci/a.yml"}},
		{name: "an included file that does not exist is still named (GitLab rejects the definition; nothing to follow)",
			root: "include:\n  - ci/gone.yml\n  - ci/a.yml\n", files: map[string]string{"ci/a.yml": "include: ci/b.yml\n", "ci/b.yml": ""},
			want: []string{"ci/gone.yml", "ci/a.yml", "ci/b.yml"}, complete: true,
			reads: []string{"ci/gone.yml", "ci/a.yml", "ci/b.yml"}},
		{name: "a wildcard at the root: kept as a pattern, not read, and the set is NOT complete",
			root:  "include:\n  - local: 'ci/*.yml'\n  - local: lint.yml\n",
			files: map[string]string{"lint.yml": "include: ci/deep/x.yml\n", "ci/deep/x.yml": ""},
			want:  []string{"ci/*.yml", "lint.yml", "ci/deep/x.yml"}, complete: false, reads: []string{"lint.yml", "ci/deep/x.yml"}},
		{name: "a `?` is a wildcard too",
			root: "include:\n  - local: 'ci/?.yml'\n", want: []string{"ci/?.yml"}, complete: false},
		{name: "a wildcard in a NESTED file: not complete either",
			root:  "include: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "include:\n  - local: 'jobs/**/*.yml'\n"},
			want:  []string{"ci/a.yml", "jobs/**/*.yml"}, complete: false, reads: []string{"ci/a.yml"}},
		// v0.2.1 verification: three include forms for which "complete" was
		// claimed and was not true.
		{name: "a `spec:` header document: the definition is the document AFTER `---`",
			root:  "spec:\n  inputs:\n    flavor:\n      default: a\n---\ninclude:\n  - local: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "a: {script: [true]}\n"},
			want:  []string{"ci/a.yml"}, complete: true, reads: []string{"ci/a.yml"}},
		{name: "a `spec:` header in a NESTED file",
			root:  "include: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "spec:\n  inputs: {}\n---\ninclude: ci/b.yml\n", "ci/b.yml": ""},
			want:  []string{"ci/a.yml", "ci/b.yml"}, complete: true, reads: []string{"ci/a.yml", "ci/b.yml"}},
		{name: "a second document that is not YAML is an error, not an empty definition",
			root: "spec: {}\n---\n{{{{", err: ".gitlab-ci.yml"},
		{name: "an entry with a variable names no one file: kept as written, not read, and the set is NOT complete",
			root:  "include:\n  - local: 'ci/$FLAVOR.yml'\n  - local: lint.yml\n",
			files: map[string]string{"lint.yml": "", "ci/$FLAVOR.yml": "include: ci/never.yml\n"},
			want:  []string{"ci/$FLAVOR.yml", "lint.yml"}, complete: false, reads: []string{"lint.yml"}},
		{name: "a variable in a NESTED file's entry: not complete either",
			root:  "include: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "include: 'jobs/${KIND}/x.yml'\n"},
			want:  []string{"ci/a.yml", "jobs/${KIND}/x.yml"}, complete: false, reads: []string{"ci/a.yml"}},
		{name: "`include: !reference [...]` is not a list of two paths: nothing is named and the set is NOT complete",
			root: "include: !reference [.shared, include]\n", complete: false},
		{name: "a `!reference` entry among literal ones: the literal ones are followed, the set is NOT complete",
			root:  "include:\n  - !reference [.shared, include]\n  - local: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": ""},
			want:  []string{"ci/a.yml"}, complete: false, reads: []string{"ci/a.yml"}},
		{name: "a `!reference` in a NESTED file's include: not complete either",
			root:  "include: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "include:\n  - local: !reference [.x, y]\n"},
			want:  []string{"ci/a.yml"}, complete: false, reads: []string{"ci/a.yml"}},
		{name: "a `!reference` OUTSIDE include says nothing about the includes",
			root:  "include: ci/a.yml\njob:\n  script: !reference [.x, script]\n",
			files: map[string]string{"ci/a.yml": ""},
			want:  []string{"ci/a.yml"}, complete: true, reads: []string{"ci/a.yml"}},
		{name: "remote, project, template and component includes of a nested file name nothing here",
			root: "include: ci/a.yml\n",
			files: map[string]string{"ci/a.yml": "include:\n  - remote: https://example.invalid/x.yml\n  - project: g/p\n    file: /x.yml\n" +
				"  - template: Go.gitlab-ci.yml\n  - component: example.invalid/g/p/c@1\n  - https://example.invalid/y.yml\n"},
			want: []string{"ci/a.yml"}, complete: true, reads: []string{"ci/a.yml"}},
		{name: "a nested file that is not YAML: the definition cannot be understood",
			root: "include: ci/a.yml\n", files: map[string]string{"ci/a.yml": "{{{{"}, err: `"ci/a.yml"`, reads: []string{"ci/a.yml"}},
		{name: "a nested include that leaves the repository",
			root: "include: ci/a.yml\n", files: map[string]string{"ci/a.yml": "include:\n  - local: ../../etc/x\n"}, err: `"ci/a.yml"`,
			reads: []string{"ci/a.yml"}},
		{name: "the root is not YAML", root: "{{{{", err: ".gitlab-ci.yml"},
		{name: "a read that FAILS is an error, not an absent file",
			root: "include: ci/a.yml\n", fail: map[string]error{"ci/a.yml": boom}, err: "gitlab: 500", reads: []string{"ci/a.yml"}},
	}
	for _, c := range cases {
		r := &repo{files: c.files, fail: c.fail}
		got, err := ResolveCILocalIncludes([]byte(c.root), r.read)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want one naming %s", c.name, err, c.err)
			}
			if len(got.Paths) != 0 || got.Complete {
				t.Errorf("%s: a failed resolution returned %+v", c.name, got)
			}
		} else if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if c.err == "" && (strings.Join(got.Paths, "|") != strings.Join(c.want, "|") || got.Complete != c.complete) {
			t.Errorf("%s:\n got  %v complete=%v\n want %v complete=%v", c.name, got.Paths, got.Complete, c.want, c.complete)
		}
		if strings.Join(r.reads, "|") != strings.Join(c.reads, "|") {
			t.Errorf("%s: read %v, want %v", c.name, r.reads, c.reads)
		}
	}
}

// The walk is bounded by the number of FILES (GitLab's own default limit on
// includes is 150): at the bound it resolves, one past it is an error -- the
// set is never silently cut short.
func TestResolveCILocalIncludesIsBounded(t *testing.T) {
	chain := func(n int) *repo {
		r := &repo{files: map[string]string{}}
		for i := 0; i < n; i++ {
			r.files[fmt.Sprintf("ci/%d.yml", i)] = fmt.Sprintf("include: ci/%d.yml\n", i+1)
		}
		r.files[fmt.Sprintf("ci/%d.yml", n-1)] = "last: {script: [true]}\n"
		return r
	}
	r := chain(MaxCIIncludeFiles)
	got, err := ResolveCILocalIncludes([]byte("include: ci/0.yml\n"), r.read)
	if err != nil || len(got.Paths) != MaxCIIncludeFiles || !got.Complete {
		t.Errorf("%d files: %d paths, complete=%v, %v; want all of them", MaxCIIncludeFiles, len(got.Paths), got.Complete, err)
	}
	r = chain(MaxCIIncludeFiles + 1)
	got, err = ResolveCILocalIncludes([]byte("include: ci/0.yml\n"), r.read)
	if err == nil || len(got.Paths) != 0 {
		t.Errorf("%d files: %d paths, err %v; want an error and no partial set", MaxCIIncludeFiles+1, len(got.Paths), err)
	}
	if len(r.reads) > MaxCIIncludeFiles+1 {
		t.Errorf("read %d files past a bound of %d", len(r.reads), MaxCIIncludeFiles)
	}
}

// What the resolution is FOR: every file it names is class `ci`.
func TestNestedIncludesAreClassCI(t *testing.T) {
	r := &repo{files: map[string]string{
		"ci/a.yml":         "include: tools/jobs/b.yml\n",
		"tools/jobs/b.yml": "b: {script: [true]}\n",
	}}
	inc, err := ResolveCILocalIncludes([]byte("include: ci/a.yml\n"), r.read)
	if err != nil {
		t.Fatal(err)
	}
	eff := mustFold(t, Input{Row: Row{Classes: []Class{ClassSource}}, Enabled: true})
	with := quietClassifier(eff, BaseFacts{CILocalIncludes: inc.Paths})
	oneLevel, _ := CILocalIncludes([]byte("include: ci/a.yml\n"))
	before := quietClassifier(eff, BaseFacts{CILocalIncludes: oneLevel})
	if got := with.Classify("tools/jobs/b.yml").Class; got != ClassCI {
		t.Errorf("a nested include is class %q, want ci", got)
	}
	if got := before.Classify("tools/jobs/b.yml").Class; got == ClassCI {
		t.Errorf("fixture: the nested file is ci without the resolution; the test shows nothing")
	}
	// The near miss: a YAML file nobody includes keeps its built-in class.
	if got := with.Classify("tools/jobs/other.yml").Class; got == ClassCI {
		t.Errorf("a file no definition includes is class ci")
	}
}
