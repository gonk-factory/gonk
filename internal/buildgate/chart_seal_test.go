package buildgate

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// -reseal appends the current chart version + content hash to the ledger.
// Deliberately NOT named -update: `go test ./... -update` regenerates the chart
// goldens, and that must not also silently append a release to this ledger.
var reseal = flag.Bool("reseal", false, "append the current chart version and content hash to chart/CHART-SEAL")

const (
	sealFile     = "chart/CHART-SEAL"
	sealChartDir = "chart/gonk"
)

// ---------------------------------------------------------------------------
// The ledger
// ---------------------------------------------------------------------------

// sealEntry is one released chart version and the hash of the chart tree that
// was released under it.
type sealEntry struct {
	Version string
	SHA256  string
	Line    int
}

var (
	semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
	hexRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// semverCompare returns -1, 0 or 1. Both arguments must already have matched
// semverRe; the chart's versions are plain X.Y.Z and the parser rejects
// anything else, which keeps this free of prerelease/build-metadata precedence
// rules that nothing here needs.
func semverCompare(a, b string) int {
	am, bm := semverRe.FindStringSubmatch(a), semverRe.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(am[i])
		y, _ := strconv.Atoi(bm[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseSeal reads the ledger. Blank lines and #-comments are skipped; every
// other line must be "<semver> <sha256>".
func parseSeal(data []byte) ([]sealEntry, error) {
	var out []sealEntry
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want `<version> <sha256>`, got %q", sealFile, i+1, line)
		}
		if !semverRe.MatchString(fields[0]) {
			return nil, fmt.Errorf("%s:%d: %q is not a plain X.Y.Z version", sealFile, i+1, fields[0])
		}
		if !hexRe.MatchString(fields[1]) {
			return nil, fmt.Errorf("%s:%d: %q is not a sha256 digest", sealFile, i+1, fields[1])
		}
		out = append(out, sealEntry{Version: fields[0], SHA256: fields[1], Line: i + 1})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The hash
// ---------------------------------------------------------------------------

// hashChartTree hashes every file under root, deterministically. Paths are
// sorted and each file contributes `relpath\0len\0content`, so neither a rename
// nor a byte change can collide with the other.
//
// Skips NOTHING deliberately: a file added under chart/gonk later -- a
// .helmignore, a new template, a vendored CRD schema -- is covered by
// construction rather than by remembering to extend a list.
func hashChartTree(root string) (string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("no files under %s", root)
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, rel := range paths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		var content []byte
		// A symlink contributes its TARGET STRING, not the file it points at:
		// following it would hash something outside the chart.
		if fi, lerr := os.Lstat(full); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, rerr := os.Readlink(full)
			if rerr != nil {
				return "", rerr
			}
			content = []byte("symlink:" + target)
		} else {
			content, err = os.ReadFile(full)
			if err != nil {
				return "", err
			}
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(content))
		h.Write(content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// chartVersion reads `version:` out of Chart.yaml without a YAML dependency --
// it is a top-level scalar and a regex cannot be fooled by nesting here.
var chartVersionRe = regexp.MustCompile(`(?m)^version:\s*(\S+)\s*$`)

func chartVersion(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, sealChartDir, "Chart.yaml"))
	if err != nil {
		return "", err
	}
	m := chartVersionRe.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("no top-level `version:` in %s/Chart.yaml", sealChartDir)
	}
	return strings.Trim(string(m[1]), `"'`), nil
}

// ---------------------------------------------------------------------------
// The assertions, as pure functions so the negative controls can drive them
// ---------------------------------------------------------------------------

const resealHint = "Bump `version:` in " + sealChartDir + "/Chart.yaml, then run `make chart-seal`."

// sealProblems is the whole gate. It answers one question: would Flux actually
// deploy the chart currently in this tree?
//
// Flux decides whether to repackage a chart on NAME+VERSION alone
// (source-controller internal/helm/chart/builder_local.go reuses the cached
// archive when name and version match, and Force is tied to the HelmChart
// object's generation, which a new git commit does not change). So a version
// that has been released before is a version Flux will not rebuild -- the
// commit lands, CI is green, and the cluster keeps running the old chart.
//
// That is why "the seal is self-consistent" is NOT the property to check. The
// version has to be NEW.
func sealProblems(entries []sealEntry, chartVer, computedHash string) []string {
	var problems []string

	if len(entries) == 0 {
		return []string{sealFile + " has no entries; it must list every released chart version. " + resealHint}
	}

	seen := map[string]int{}
	for _, e := range entries {
		if prev, dup := seen[e.Version]; dup {
			problems = append(problems, fmt.Sprintf(
				"%s: version %s appears twice (lines %d and %d). Flux reuses its cached archive for a "+
					"name+version it has already packaged, so re-releasing a version means the change is "+
					"NEVER DEPLOYED. Pick a new version.", sealFile, e.Version, prev, e.Line))
		}
		seen[e.Version] = e.Line
	}

	last := entries[len(entries)-1]
	for _, e := range entries[:len(entries)-1] {
		if semverCompare(last.Version, e.Version) <= 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: the newest entry %s (line %d) is not greater than %s (line %d). The ledger is "+
					"append-only and must climb, or a revert can re-release a version Flux will not rebuild.",
				sealFile, last.Version, last.Line, e.Version, e.Line))
			break
		}
	}

	if last.Version != chartVer {
		problems = append(problems, fmt.Sprintf(
			"%s/Chart.yaml says version %s but the newest %s entry is %s. Whichever you changed, the "+
				"other has to follow. %s", sealChartDir, chartVer, sealFile, last.Version, resealHint))
	}

	if last.SHA256 != computedHash {
		problems = append(problems, fmt.Sprintf(
			"%s content has changed but no new version was released.\n  recorded %s (version %s)\n"+
				"  actual   %s\nUnder reconcileStrategy: ChartVersion, Flux only deploys when Chart.yaml's "+
				"version changes -- so as things stand this edit would sit in git and NEVER reach the "+
				"cluster, with CI green. %s", sealChartDir, last.SHA256, last.Version, computedHash, resealHint))
	}

	return problems
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// A chart edit that does not bump the version is invisible: Flux declines to
// deploy it and nothing else complains (gonk-sjb). This gate is the only thing
// that says so, and it runs in the ordinary `go test ./...` gate rather than
// behind -tags chart, because the chart-test job is gated on
// $GONK_CHART_TOOLS_IMAGE, which is defined nowhere -- so the golden gate does
// not run in CI at all and this one is the only chart guard on every commit.
func TestChartSealMatchesTheChart(t *testing.T) {
	root := repoRoot(t)

	computed, err := hashChartTree(filepath.Join(root, sealChartDir))
	if err != nil {
		t.Fatalf("hash %s: %v", sealChartDir, err)
	}
	version, err := chartVersion(root)
	if err != nil {
		t.Fatalf("read chart version: %v", err)
	}

	if *reseal {
		if err := appendSeal(root, version, computed); err != nil {
			t.Fatalf("reseal: %v", err)
		}
		t.Logf("sealed %s at version %s (%s)", sealChartDir, version, computed[:12])
		return
	}

	data, err := os.ReadFile(filepath.Join(root, sealFile))
	if err != nil {
		t.Fatalf("read %s: %v\n%s", sealFile, err, resealHint)
	}
	entries, err := parseSeal(data)
	if err != nil {
		t.Fatalf("parse %s: %v", sealFile, err)
	}
	for _, p := range sealProblems(entries, version, computed) {
		t.Error(p)
	}
}

// appendSeal implements `make chart-seal`. It REFUSES to rewrite an existing
// entry: the ledger is append-only, and quietly re-hashing the current version
// is exactly the mistake the gate exists to catch -- it would make the tree
// self-consistent while leaving the cluster on the old chart.
func appendSeal(root, version, hash string) error {
	path := filepath.Join(root, sealFile)

	var entries []sealEntry
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if entries, err = parseSeal(data); err != nil {
			return err
		}
	case !os.IsNotExist(err):
		return err
	default:
		data = []byte("# Append-only ledger of released chart versions.\n" +
			"# Generated by `make chart-seal`. Never edit or reorder by hand.\n" +
			"# <semver> <sha256 of " + sealChartDir + ">\n")
	}

	for _, e := range entries {
		if e.Version != version {
			continue
		}
		if e.SHA256 == hash {
			return nil // already sealed, nothing to do
		}
		return fmt.Errorf(
			"version %s is already in %s with a different hash.\n"+
				"The ledger is append-only, so this cannot be rewritten -- and rewriting it is the bug: "+
				"Flux will not repackage a version it has already built, so the cluster would keep the old "+
				"chart while the tree looked correct.\nBump `version:` in %s/Chart.yaml first.",
			version, sealFile, sealChartDir)
	}

	if n := len(entries); n > 0 && semverCompare(version, entries[n-1].Version) <= 0 {
		return fmt.Errorf("chart version %s is not greater than the last released %s; the ledger must climb",
			version, entries[n-1].Version)
	}

	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		data = append(data, '\n')
	}
	return os.WriteFile(path, append(data, []byte(version+" "+hash+"\n")...), 0o644)
}

// ---------------------------------------------------------------------------
// Negative controls
// ---------------------------------------------------------------------------

// Every assertion in sealProblems, proven to fail. A gate whose failure path is
// never exercised is a gate that can quietly stop checking -- and this one
// guards a silent no-deploy, so its silence would look exactly like success.
func TestTheChartSealGateActuallyFails(t *testing.T) {
	const (
		hashA = "1111111111111111111111111111111111111111111111111111111111111111"
		hashB = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	cases := []struct {
		name     string
		entries  []sealEntry
		version  string
		hash     string
		wantSaid string
	}{
		{
			name: "empty ledger", entries: nil, version: "0.1.1", hash: hashA,
			wantSaid: "no entries",
		},
		{
			name:    "version released twice",
			entries: []sealEntry{{"0.1.0", hashA, 4}, {"0.1.1", hashB, 5}, {"0.1.1", hashA, 6}},
			version: "0.1.1", hash: hashA,
			wantSaid: "appears twice",
		},
		{
			name:    "ledger goes backwards",
			entries: []sealEntry{{"0.1.2", hashA, 4}, {"0.1.1", hashB, 5}},
			version: "0.1.1", hash: hashB,
			wantSaid: "is not greater than",
		},
		{
			name:    "Chart.yaml bumped but not resealed",
			entries: []sealEntry{{"0.1.0", hashA, 4}},
			version: "0.1.1", hash: hashA,
			wantSaid: "the other has to follow",
		},
		{
			name:    "chart edited but version not bumped",
			entries: []sealEntry{{"0.1.1", hashA, 4}},
			version: "0.1.1", hash: hashB,
			wantSaid: "NEVER reach the cluster",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := sealProblems(tc.entries, tc.version, tc.hash)
			if len(problems) == 0 {
				t.Fatalf("sealProblems accepted %q, which Flux would not deploy", tc.name)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.wantSaid) {
				t.Errorf("the failure did not explain itself; wanted %q in:\n%s", tc.wantSaid, strings.Join(problems, "\n"))
			}
		})
	}

	// And the passing case, so the above is not vacuously true of every input.
	if p := sealProblems([]sealEntry{{"0.1.0", hashA, 4}, {"0.1.1", hashB, 5}}, "0.1.1", hashB); len(p) != 0 {
		t.Errorf("sealProblems rejected a correct ledger: %v", p)
	}
}

func TestTheSealParserRejectsJunk(t *testing.T) {
	for _, bad := range []string{
		"0.1.1\n",        // no hash
		"0.1.1 nothex\n", // not a digest
		"v0.1.1 " + strings.Repeat("a", 64) + "\n", // not plain X.Y.Z
		"0.1 " + strings.Repeat("a", 64) + "\n",    // not three components
		"0.1.1 " + strings.Repeat("A", 64) + "\n",  // uppercase digest
	} {
		if _, err := parseSeal([]byte(bad)); err == nil {
			t.Errorf("parseSeal accepted %q", strings.TrimSpace(bad))
		}
	}
	// Comments and blanks are skipped, not rejected.
	got, err := parseSeal([]byte("# header\n\n0.1.0 " + strings.Repeat("a", 64) + "\n"))
	if err != nil {
		t.Fatalf("parseSeal rejected a valid ledger: %v", err)
	}
	if len(got) != 1 || got[0].Version != "0.1.0" {
		t.Errorf("parseSeal mis-read a valid ledger: %+v", got)
	}
}

func TestSemverCompareOrdersReleases(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.1.1", "0.1.0", 1}, {"0.1.0", "0.1.1", -1}, {"0.1.0", "0.1.0", 0},
		{"0.2.0", "0.1.9", 1}, {"1.0.0", "0.9.9", 1},
		{"0.1.10", "0.1.9", 1}, // string comparison would get this backwards
	} {
		if got := semverCompare(tc.a, tc.b); got != tc.want {
			t.Errorf("semverCompare(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// The hash must notice a change anywhere under the chart, including in files no
// golden ever renders -- values.schema.json, a removed guard, a vendored CRD
// schema. That set is why the seal is not redundant with the golden gate.
func TestChartHashNoticesEveryKindOfChange(t *testing.T) {
	base := t.TempDir()
	mk := func(root, rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(base, "Chart.yaml", "name: gonk\nversion: 0.1.1\n")
	mk(base, "values.schema.json", `{"required":["a"]}`)
	mk(base, "templates/x.yaml", "kind: X\n")

	h0, err := hashChartTree(base)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(root string)
	}{
		{"schema loosened", func(r string) { mk(r, "values.schema.json", `{"required":[]}`) }},
		{"template edited", func(r string) { mk(r, "templates/x.yaml", "kind: Y\n") }},
		{"file added", func(r string) { mk(r, "tests/crd-schemas/z.json", "{}") }},
		{"file renamed", func(r string) {
			os.Rename(filepath.Join(r, "templates/x.yaml"), filepath.Join(r, "templates/y.yaml"))
		}},
		{"file removed", func(r string) { os.Remove(filepath.Join(r, "values.schema.json")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if out, err := execCopyTree(base, dir); err != nil {
				t.Fatalf("copy: %v %s", err, out)
			}
			tc.mutate(dir)
			h, err := hashChartTree(dir)
			if err != nil {
				t.Fatal(err)
			}
			if h == h0 {
				t.Errorf("hash did not change after %q -- this change would ship undetected", tc.name)
			}
		})
	}

	// Same content, different directory: the hash is of CONTENT, not location.
	same := t.TempDir()
	if out, err := execCopyTree(base, same); err != nil {
		t.Fatalf("copy: %v %s", err, out)
	}
	h1, err := hashChartTree(same)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h0 {
		t.Errorf("hash is not stable across identical trees: %s vs %s", h0, h1)
	}
}

// execCopyTree copies a directory tree, so the mutation cases above each start
// from an identical baseline.
func execCopyTree(src, dst string) (string, error) {
	return "", filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}
