// Every `uses:` in .github/workflows/ must name a 40-hex commit, and must
// say in a trailing comment which release that commit is.
//
// WHY THIS IS A TEST AND NOT A ONE-OFF GREP. `uses: actions/checkout@v4` is
// not a version, it is a third party's PROMISE not to move a tag. Pinning by
// commit removes that promise from the trust model, which is the entire
// point -- and a promise removed once comes straight back the next time
// somebody adds a step by copying an example from a README. T-18 pinned all
// 34 of them; without a gate, that is a snapshot, not a property.
//
// The version comment is required too, and it is not decoration: a bare
// 40-hex SHA is unreadable, so a reviewer cannot tell v4.4.0 from a commit
// on somebody's fork without leaving the diff. It is deliberately NOT
// verified against the upstream repository here -- this suite is hermetic
// and must stay that way (ci.yml runs with GOPROXY=off and no network
// contract). A comment can therefore lie; resolving each SHA against the
// real upstream is a human step at pin time. What this gate guarantees is
// narrower and still worth having: the ref is a commit, and SOMETHING
// claims which release it is.
package buildgate

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// usesLineRe captures every `uses:` value in a workflow, whatever its form:
// the ref is `(\S+)` rather than a hex pattern precisely so an UNPINNED line
// is matched and then rejected below. A regex that only matched good lines
// would make this test pass vacuously on a file full of floating tags.
var usesLineRe = regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*(\S+)\s*(.*)$`)

// pinnedRefRe is what a `uses:` ref must look like: owner/repo@<40 hex>.
// Lowercase hex only -- git spells object names in lowercase, and accepting
// mixed case would let a typo'd ref through on a technicality.
var pinnedRefRe = regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)

// versionCommentRe is the trailing `# vX.Y.Z` (or `# v0.24.2`, or a bare
// `# v4`) naming the release the commit corresponds to.
var versionCommentRe = regexp.MustCompile(`^#\s*v\d+(\.\d+)*\S*\s*$`)

// workflowFiles returns every workflow file under .github/workflows/, by
// path, failing if there are none -- an empty scan is a broken test, not a
// clean repo.
func workflowFiles(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	if len(paths) == 0 {
		t.Fatalf("no workflow files found under %s -- the scan is broken, not the repo", dir)
	}
	sort.Strings(paths)
	return paths
}

// TestEveryActionIsPinnedByCommitSHA is exit criterion 1 of T-18, made
// permanent: `grep -n 'uses:' .github/workflows/*.yml` shows only 40-hex
// refs, each with a version comment.
func TestEveryActionIsPinnedByCommitSHA(t *testing.T) {
	root := repoRoot(t)

	total := 0
	for _, path := range workflowFiles(t, root) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		name := filepath.Base(path)
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			m := usesLineRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			total++
			ref, rest := m[1], strings.TrimSpace(m[2])
			if !pinnedRefRe.MatchString(ref) {
				t.Errorf("%s:%d: `uses: %s` is not pinned by commit SHA. A moving tag is a third party's promise not to change what it points at; pin owner/repo@<40-hex> and put the version in a trailing comment.", name, i+1, ref)
				continue
			}
			if !versionCommentRe.MatchString(rest) {
				t.Errorf("%s:%d: `uses: %s` is pinned but carries no `# vX.Y.Z` comment (found %q). A bare SHA cannot be reviewed.", name, i+1, ref, rest)
			}
		}
	}

	// A scan that found no `uses:` at all would report every file clean.
	if total == 0 {
		t.Fatal("scanned every workflow and matched zero `uses:` lines -- usesLineRe is broken, not the workflows")
	}
	t.Logf("uses: lines checked: %d", total)
}

// TestTheActionPinGateActuallyRejects proves the patterns above can tell a
// bad line from a good one. Without it, a regex that matched nothing would
// make TestEveryActionIsPinnedByCommitSHA green on a repo with no pins at
// all -- the same vacuous-pass failure TestTheLintPinGateActuallyFails
// exists to rule out for the lint pins.
func TestTheActionPinGateActuallyRejects(t *testing.T) {
	const sha = "11d5960a326750d5838078e36cf38b85af677262"

	good := []string{
		"      - uses: actions/checkout@" + sha + " # v4.4.0",
		"        uses: anchore/sbom-action@" + sha + " # v0.24.2",
	}
	for _, line := range good {
		m := usesLineRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("usesLineRe failed to match a well-formed pinned line: %q", line)
		}
		if !pinnedRefRe.MatchString(m[1]) {
			t.Errorf("pinnedRefRe rejected a valid pin: %q", m[1])
		}
		if !versionCommentRe.MatchString(strings.TrimSpace(m[2])) {
			t.Errorf("versionCommentRe rejected a valid version comment: %q", m[2])
		}
	}

	// Each of these must be CAUGHT, and each is a mistake somebody will
	// actually make.
	bad := []struct {
		line string
		why  string
	}{
		{"      - uses: actions/checkout@v4", "a floating major tag"},
		{"      - uses: actions/checkout@v4.4.0", "an exact tag is still a mutable ref"},
		{"      - uses: actions/checkout@main", "a branch"},
		{"      - uses: actions/checkout@11d5960", "an abbreviated SHA"},
		{"      - uses: actions/checkout@11D5960A326750D5838078E36CF38B85AF677262", "uppercase hex"},
	}
	for _, c := range bad {
		m := usesLineRe.FindStringSubmatch(c.line)
		if m == nil {
			t.Fatalf("usesLineRe failed to match %s at all (%q) -- an unmatched bad line is an unreported bad line", c.why, c.line)
		}
		if pinnedRefRe.MatchString(m[1]) {
			t.Errorf("pinnedRefRe accepted %s: %q", c.why, m[1])
		}
	}

	// Pinned, but with no version comment: caught by the second assertion,
	// not the first.
	m := usesLineRe.FindStringSubmatch("      - uses: actions/checkout@" + sha)
	if m == nil {
		t.Fatal("usesLineRe failed to match a pinned line with no comment")
	}
	if !pinnedRefRe.MatchString(m[1]) {
		t.Fatal("pinnedRefRe rejected a bare valid pin")
	}
	if versionCommentRe.MatchString(strings.TrimSpace(m[2])) {
		t.Error("versionCommentRe accepted an empty trailing comment")
	}
	// A trailing comment that is not a version must not satisfy it either.
	m = usesLineRe.FindStringSubmatch("      - uses: actions/checkout@" + sha + " # pinned, see T-18")
	if m == nil {
		t.Fatal("usesLineRe failed to match a pinned line with a prose comment")
	}
	if versionCommentRe.MatchString(strings.TrimSpace(m[2])) {
		t.Error("versionCommentRe accepted a prose comment in place of a version")
	}
}
