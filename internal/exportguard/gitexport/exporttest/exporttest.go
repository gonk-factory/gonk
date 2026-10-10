// Package exporttest builds scratch git repositories for the release-export
// tests: a deterministic "origin" with the export data in it, and a bare
// stand-in for the public mirror. Nothing here touches a network or any
// repository outside the test's temporary directory.
//
// The fixtures are DETERMINISTIC: fixed identity, fixed dates, fixed file
// modes. Two origins built by the same calls in different directories hold
// the same commit ids, which is what lets a test pin a tree id as a literal.
package exporttest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Deny is SYNTHETIC specific-rule data with made-up names: at least one rule
// for each of the five specific classes. The real data is internal-only.
const Deny = `# synthetic deny data for tests
I1 zone        *      (?i)zork\.lan
I2 cluster     *      (?i)\bzork(0[0-9])?\b
I3 ca-name     *      lamp-ca
I4 gitops      *      clusters/zork
I5 context     *      admin@zork
`

// List is the internal-only list of the base fixture.
const List = `# never exported
.beads/      # the tracker
CLAUDE.md    # agent instructions
docs/plans/  # build plans
release/     # the export's own data
`

// Notes are the release notes of v0.2.0 in the base fixture.
const Notes = "# gonk v0.2.0\n\nThe second release: change requests and reviews,\nwrapped over two lines.\n\n## Details\n\nMore text.\n"

// BaseFiles is a small tree with everything an export needs: the three data
// files, internal-only paths that spell the synthetic internal names, a
// sealed chart, an image Dockerfile and release notes.
func BaseFiles() map[string]string {
	return map[string]string{
		"release/internal-only.txt":  List,
		"release/deny-internal.txt":  Deny,
		"release/allow.txt":          "# no exemptions\n",
		".beads/issues.jsonl":        "{\"note\":\"deployed to gitlab.zork.lan\"}\n",
		"CLAUDE.md":                  "internal instructions for zork\n",
		"docs/plans/plan.md":         "the plan names clusters/zork and admin@zork\n",
		"docs/plans/sub/evidence.md": "lamp-ca\n",
		"docs/releases/v0.2.0.md":    Notes,
		"docs/guide.md":              "A guide. See gitlab.example.internal.\n",
		"README.md":                  "# gonk\n",
		"cmd/tool/main.go":           "package main\n\nfunc main() {}\n",
		"chart/gonk/Chart.yaml":      "name: gonk\nversion: 0.1.0\n",
		"images/Dockerfile.tool":     "FROM scratch\nCOPY cmd/ cmd/\nCOPY go.mod ./\n",
		"go.mod":                     "module example.test/gonk\n",
	}
}

// Origin is a scratch repository with a `main` branch.
type Origin struct {
	t   testing.TB
	Dir string
	n   int
}

// NewOrigin creates an empty repository. The first Commit creates `main`.
func NewOrigin(t testing.TB) *Origin {
	t.Helper()
	o := &Origin{t: t, Dir: filepath.Join(t.TempDir(), "origin")}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	o.Git("init", "--quiet", "--initial-branch=main")
	return o
}

// Git runs git in the repository with a fixed, isolated environment and
// returns its trimmed output. It fails the test on error.
func (o *Origin) Git(args ...string) string {
	o.t.Helper()
	out, err := o.TryGit(args...)
	if err != nil {
		o.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TryGit is Git that returns the error.
func (o *Origin) TryGit(args ...string) (string, error) {
	return GitIn(o.Dir, o.n, args...)
}

// GitIn runs an isolated git command in dir. tick moves the fixed clock.
func GitIn(dir string, tick int, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// The author date differs from the committer date, so that a test can
	// tell which of the two a derived date came from.
	date := fmt.Sprintf("%d +0200", 1700000000+tick*60)
	authored := fmt.Sprintf("%d +0200", 1700000000+tick*60-17)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Fixture Author", "GIT_AUTHOR_EMAIL=author@example.com", "GIT_AUTHOR_DATE=" + authored,
		"GIT_COMMITTER_NAME=Fixture Committer", "GIT_COMMITTER_EMAIL=committer@example.com", "GIT_COMMITTER_DATE=" + date,
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// Write puts files into the working tree without committing. A value of
// DELETE removes the file.
func (o *Origin) Write(files map[string]string) {
	o.t.Helper()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		full := filepath.Join(o.Dir, filepath.FromSlash(p))
		if files[p] == DELETE {
			if err := os.RemoveAll(full); err != nil {
				o.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			o.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[p]), 0o644); err != nil {
			o.t.Fatal(err)
		}
	}
}

// DELETE as a file's content removes the file.
const DELETE = "\x00delete\x00"

// Commit writes files, commits everything on the current branch and returns
// the new commit id.
func (o *Origin) Commit(message string, files map[string]string) string {
	o.t.Helper()
	o.Write(files)
	o.n++
	o.Git("add", "--all", "--force")
	o.Git("commit", "--quiet", "--allow-empty", "-m", message)
	return o.Git("rev-parse", "HEAD")
}

// Head returns the id of the current commit.
func (o *Origin) Head() string { return o.Git("rev-parse", "HEAD") }

// EmptyMirror creates a bare stand-in for a public mirror that has just been
// created: no branch, no tag, no object. It returns its absolute path.
func (o *Origin) EmptyMirror() string {
	o.t.Helper()
	dir := filepath.Join(o.t.TempDir(), "mirror.git")
	out, err := GitIn(o.t.TempDir(), 0, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if err != nil {
		o.t.Fatalf("init --bare: %v\n%s", err, out)
	}
	return dir
}

// RemoteRef returns the id of a ref in a repository directory, or "".
func RemoteRef(t testing.TB, dir, ref string) string {
	t.Helper()
	out, err := GitIn(dir, 0, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return out
}
