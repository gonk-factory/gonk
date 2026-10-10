package buildgate

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard"
	"github.com/gonk-factory/gonk/internal/exportguard/gitexport"
)

// THE TREE GUARD (gonk-cuwn, part ENFORCE).
//
// What is published is this tree minus a list of internal-only paths. The
// export refuses a tree that holds internal detail -- a private address, a
// certificate, a personal path, one of the maintainers' internal names -- but
// the export runs at a release, and commits go to main every day. This test
// applies the SAME rules (exportguard.TreeScan) to the tree being gated, so a
// commit that would make the next export fail goes red at `make gate`, on the
// machine that made it, before it is pushed anywhere.
//
// It needs no network and no scanner binary. It runs in one of two modes,
// decided by the tree itself and by nothing else:
//
//	INTERNAL  release/ is present. The generic rules AND the maintainers'
//	          specific rules (release/deny-internal.txt) apply to every file
//	          that release/internal-only.txt does not remove.
//	PUBLIC    EXPORT-SOURCE is present (an exported tree; release/ is gone).
//	          The generic rules apply to every file. This is what keeps an
//	          outside contribution clean.
//
// Both present, or neither, is a failure: a tree that cannot say which it is
// would be scanned with the generic rules only and called clean.
//
// It does NOT run the secret scanner (the export and CI do), and it reads the
// WORKING TREE: every tracked file, plus every untracked file git does not
// ignore -- the files `git add -A` would commit.

// treeReport is the outcome of one scan of one tree.
type treeReport struct {
	Mode     exportguard.Mode
	Files    int
	Findings []exportguard.Finding
	Problems []string
}

func (r *treeReport) clean() bool { return len(r.Findings) == 0 && len(r.Problems) == 0 }

// treeFiles lists the files of a tree, slash-separated and sorted. In a git
// checkout: tracked files plus untracked files that are not ignored. Anywhere
// else (an unpacked tarball): every file under root.
func treeFiles(root string) ([]string, error) {
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		cmd.Dir = root
		// A hook exports GIT_DIR and GIT_INDEX_FILE; with them this would
		// list another repository's files.
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GIT_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-files in %s: %v: %s", root, err, strings.TrimSpace(stderr.String()))
		}
		seen := map[string]bool{}
		var files []string
		for _, p := range strings.Split(string(out), "\x00") {
			if p != "" && !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		}
		sort.Strings(files)
		return files, nil
	}
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

// readTreeFile returns what git would store for a path: a file's bytes, or a
// symbolic link's target. ok is false for a path that is listed but gone (a
// tracked file deleted in the working tree) or is not a file.
func readTreeFile(root, path string) (data []byte, ok bool, err error) {
	full := filepath.Join(root, filepath.FromSlash(path))
	st, err := os.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case st.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(full)
		return []byte(target), err == nil, err
	case !st.Mode().IsRegular():
		return nil, false, nil
	}
	data, err = os.ReadFile(full)
	return data, err == nil, err
}

// scanTree applies the tree guard to the tree at root. publicAllow is the
// allow-list used in PUBLIC mode, where release/allow.txt does not exist.
func scanTree(root string, publicAllow []byte) (*treeReport, error) {
	files, err := treeFiles(root)
	if err != nil {
		return nil, err
	}
	hasSource, hasData := false, false
	for _, f := range files {
		if f == exportguard.ExportSourcePath {
			hasSource = true
		}
		if strings.HasPrefix(f, exportguard.SelfDir) {
			hasData = true
		}
	}
	mode, err := exportguard.DetectMode(hasSource, hasData)
	if err != nil {
		return nil, err
	}
	rep := &treeReport{Mode: mode}

	need := func(path string) ([]byte, error) {
		data, ok, err := readTreeFile(root, path)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%s is missing: in %s mode the tree guard cannot run without it", path, mode)
		}
		return data, nil
	}

	// One guard per worker: a Guard records which exemptions it used, so it
	// serves one goroutine. newGuard is called once per worker.
	var newGuard func() (*exportguard.Guard, error)
	var list *exportguard.List
	kept := files
	switch mode {
	case exportguard.ModeInternal:
		deny, err := need(exportguard.DenyPath)
		if err != nil {
			return nil, err
		}
		allow, err := need(exportguard.AllowPath)
		if err != nil {
			return nil, err
		}
		listData, err := need(exportguard.ListPath)
		if err != nil {
			return nil, err
		}
		newGuard = func() (*exportguard.Guard, error) { return exportguard.New(deny, allow) }
		if list, err = exportguard.ParseList(listData); err != nil {
			return nil, err
		}
		kept = kept[:0:0]
		for _, f := range files {
			if !list.Covers(f) {
				kept = append(kept, f)
			}
		}
		// A list entry that removes nothing has rotted. Reported, not fatal:
		// the scan below still says what else is wrong.
		if _, _, err := list.Apply(files); err != nil {
			rep.Problems = append(rep.Problems, err.Error())
		}
	case exportguard.ModePublic:
		src, err := need(exportguard.ExportSourcePath)
		if err != nil {
			return nil, err
		}
		if _, err := exportguard.ParseExportSource(src); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", exportguard.ExportSourcePath, err))
		}
		newGuard = func() (*exportguard.Guard, error) { return exportguard.New(nil, publicAllow) }
	}

	// The files are dealt to the workers round-robin; each worker scans its
	// own share with its own guard, and the shares are merged.
	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	parts := make([]*exportguard.TreeScan, workers)
	errs := make([]error, workers)
	for w := range parts {
		g, err := newGuard()
		if err != nil {
			return nil, err
		}
		parts[w] = &exportguard.TreeScan{Guard: g, List: list}
	}
	var wg sync.WaitGroup
	for w := range parts {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(kept); i += workers {
				data, ok, err := readTreeFile(root, kept[i])
				if err != nil {
					errs[w] = err
					return
				}
				if !ok {
					continue
				}
				parts[w].Path(kept[i])
				parts[w].Blob(kept[i], data)
			}
		}(w)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	scan := exportguard.MergeScans(parts)
	rep.Files = scan.Files
	rep.Findings = scan.Findings
	rep.Problems = append(rep.Problems, scan.Problems...)
	return rep, nil
}

// publicAllowList is the allow-list of PUBLIC mode: the exemptions of
// release/allow.txt, which is not published. Its literals are assembled here
// so that this file holds nothing a rule matches;
// TestThePublicAllowListIsTheInternalOne holds the two equal wherever both
// exist.
func publicAllowList() []byte {
	join := func(parts ...string) string { return strings.Join(parts, "") }
	rows := [][]string{
		{"upstream", "vendor/", "third-party code published elsewhere"},
		{"path", "chart/gonk/Chart.yaml", gitexport.IdentityEmail, "the maintainer field: the identity on every public commit"},
		{"path", "internal/exportguard/gitexport/identity.go", gitexport.IdentityEmail, "the snapshot commit identity"},
		{"path", ".github/workflows/netpol.yml", join("192", ".168", ".0.0/16"), "Calico's documented default pod CIDR for the kind cluster the workflow creates"},
		{"domain", "berri.ai", "an upstream security contact, in the upstream report under docs/upstream/"},
		{"scanner-config", ".gitleaks.toml", "one allowlist for the session-key grammar; held to that shape by TestTheScannerConfig*"},
		{"path", "chart/gonk/smoke/gc-controller-smoke.md", join("/ho", "me/gonk/"), "the HOME of the gonk user inside the controller container; a product path"},
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(strings.Join(r, "\t") + "\n")
	}
	return []byte(b.String())
}

// allowKeys reduces allow-list data to what an entry MEANS: every field but
// its reason.
func allowKeys(data []byte) []string {
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		keys = append(keys, strings.Join(f[:len(f)-1], " | "))
	}
	sort.Strings(keys)
	return keys
}

func TestPublicTreeHasNoInternalDetail(t *testing.T) {
	rep, err := scanTree(repoRoot(t), publicAllowList())
	if err != nil {
		t.Fatalf("the tree guard could not run: %v", err)
	}
	t.Logf("tree guard: %s mode, %d files scanned, %d findings, %d problems", rep.Mode, rep.Files, len(rep.Findings), len(rep.Problems))
	// A scan that saw almost nothing is not a clean tree.
	if rep.Files < 1000 {
		t.Fatalf("the tree guard scanned %d files; this tree has thousands -- it is looking in the wrong place", rep.Files)
	}
	for _, p := range rep.Problems {
		t.Errorf("problem: %s", p)
	}
	for _, f := range rep.Findings {
		// class, rule, kind, path:line:column -- never the matched text.
		t.Errorf("deny: %s", f)
	}
	if !rep.clean() {
		t.Log("THE TREE GUARD REFUSED THIS TREE. Each `deny:` line is CLASS rule kind path:line:col. " +
			"Use a reserved value instead (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, example.internal); " +
			"a file that is internal working material belongs on the internal-only list, not in the published tree. " +
			"See CONTRIBUTING.md.")
	}
}

// The public allow-list exists only because release/allow.txt is not
// published. Where both exist they must say the same thing.
func TestThePublicAllowListIsTheInternalOne(t *testing.T) {
	root := repoRoot(t)
	internal, ok, err := readTreeFile(root, exportguard.AllowPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		if _, has, _ := readTreeFile(root, exportguard.ExportSourcePath); !has {
			t.Fatalf("neither %s nor %s exists", exportguard.AllowPath, exportguard.ExportSourcePath)
		}
		t.Skipf("%s is not in a published tree (it is internal-only): there is nothing to compare the public allow-list with here. The comparison runs in the maintainers' tree.", exportguard.AllowPath)
	}
	got, want := allowKeys(publicAllowList()), allowKeys(internal)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("publicAllowList() and %s differ; the gate would accept in one tree what it refuses in the other.\npublic:\n  %s\ninternal:\n  %s",
			exportguard.AllowPath, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if len(got) < 5 {
		t.Fatalf("only %d allow-list entries were compared", len(got))
	}
}
