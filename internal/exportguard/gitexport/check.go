package gitexport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/gonk-factory/gonk/internal/exportguard"
)

// ScanFinding is one secret-scanner finding: the rule and the place, never
// the secret.
type ScanFinding struct {
	Rule string `json:"rule"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// Scanner scans a materialised tree (a directory) and returns its findings.
type Scanner func(dir string) ([]ScanFinding, error)

// CheckOptions selects the scanner.
type CheckOptions struct {
	// Scanner is the secret scanner. Required unless SkipScanner is set.
	Scanner Scanner
	// SkipScanner runs the deny-pattern guard only. The result then says the
	// scanner did not run, and Commit and Publish refuse it.
	SkipScanner bool
}

// CheckResult is the report of `check`.
type CheckResult struct {
	*TreeResult
	OK           bool                  `json:"ok"`
	Problems     []string              `json:"problems"`
	Findings     []exportguard.Finding `json:"findings"`
	Summary      []exportguard.Summary `json:"summary"`
	ScannerRan   bool                  `json:"scanner_ran"`
	ScanFindings []ScanFinding         `json:"scanner_findings"`
	DanglingRefs int                   `json:"files_referencing_internal_only_paths"`
	guard        *exportguard.Guard
}

// Check applies the deny-pattern guard to every path and every blob of the
// public tree, then runs the secret scanner on the materialised tree. The
// result is OK only with zero guard findings, zero stale exemptions, zero
// structural problems and zero scanner findings.
//
// The deny data and the allow-list are read from the SOURCE COMMIT, like the
// internal-only list: the rules that judge a release are the rules that were
// committed with it.
func (r *Repo) Check(tr *TreeResult, opt CheckOptions) (*CheckResult, error) {
	if !opt.SkipScanner && opt.Scanner == nil {
		return nil, fmt.Errorf("no secret scanner configured")
	}
	deny, ok, err := r.show(tr.Source, exportguard.DenyPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, refuse("%s does not exist at %s", exportguard.DenyPath, tr.Source)
	}
	allow, ok, err := r.show(tr.Source, exportguard.AllowPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, refuse("%s does not exist at %s", exportguard.AllowPath, tr.Source)
	}
	guard, err := exportguard.New(deny, allow)
	if err != nil {
		return nil, refuse("%v", err)
	}
	listBytes, _, err := r.show(tr.Source, exportguard.ListPath)
	if err != nil {
		return nil, err
	}
	list, err := exportguard.ParseList(listBytes)
	if err != nil {
		return nil, refuse("%v", err)
	}

	res := &CheckResult{TreeResult: tr, guard: guard, Problems: []string{}, Findings: []exportguard.Finding{}, ScanFindings: []ScanFinding{}}

	// The per-file rules are exportguard.TreeScan's, shared with the gate.
	scan := &exportguard.TreeScan{Guard: guard, List: list}
	for _, e := range tr.entries {
		scan.Path(e.Path)
	}
	err = r.eachBlob(tr.entries, func(e treeEntry, data []byte) error {
		scan.Blob(e.Path, data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	scan.Finish()
	res.Findings = append(res.Findings, scan.Findings...)
	res.Problems = append(res.Problems, scan.Problems...)
	res.DanglingRefs = scan.DanglingRefs

	if opt.SkipScanner {
		res.Problems = append(res.Problems, "the secret scanner was NOT run (guard-only check); this tree is not cleared for export")
	} else {
		dir, err := r.materialise(tr.Tree)
		if err != nil {
			return nil, err
		}
		fs, err := opt.Scanner(dir)
		_ = os.RemoveAll(dir)
		if err != nil {
			return nil, fmt.Errorf("secret scanner: %v", err)
		}
		res.ScannerRan = true
		res.ScanFindings = append(res.ScanFindings, fs...)
	}

	res.Summary = exportguard.Summarize(res.Findings)
	res.OK = len(res.Findings) == 0 && len(res.Problems) == 0 && len(res.ScanFindings) == 0 && res.ScannerRan
	return res, nil
}

// GuardClean reports whether the deny-pattern guard alone passed: no
// finding and no problem other than the scanner not having run.
func (c *CheckResult) GuardClean() bool {
	if len(c.Findings) != 0 {
		return false
	}
	for _, p := range c.Problems {
		if !strings.HasPrefix(p, "the secret scanner was NOT run") {
			return false
		}
	}
	return true
}

// Refusal describes why a check failed, one line per reason, without
// repeating any matched text.
func (c *CheckResult) Refusal() string {
	var b strings.Builder
	for _, p := range c.Problems {
		fmt.Fprintf(&b, "problem: %s\n", p)
	}
	for _, f := range c.Findings {
		fmt.Fprintf(&b, "deny: %s\n", f)
	}
	for _, f := range c.ScanFindings {
		fmt.Fprintf(&b, "scanner: X1 %s %s:%d\n", f.Rule, f.File, f.Line)
	}
	return b.String()
}

// materialise writes the files of a tree into a fresh directory, from a
// private index. It deliberately does not use `git archive`, which honours
// export-ignore attributes and would hand the scanner less than the tree.
func (r *Repo) materialise(tree string) (string, error) {
	dir := r.tmp("tree")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	index := gitCmd{env: []string{"GIT_INDEX_FILE=" + r.tmp("index")}}
	if _, err := r.gitWith(index, "read-tree", tree); err != nil {
		return "", err
	}
	if _, err := r.gitWith(index, "checkout-index", "--all", "--prefix="+dir+string(filepath.Separator)); err != nil {
		return "", err
	}
	return dir, nil
}

// The secret scanner is pinned: gitleaks 8.30.1, the version the public
// mirror's own secrets workflow runs. The digest is of the BINARY inside the
// release archive whose own digest that workflow pins.
const (
	GitleaksVersion       = "8.30.1"
	gitleaksSHA256LinuxX6 = "88f91962aa2f93ac6ab281d553b9e125f5197bbbce38f9f2437f7299c32e5509"
)

// Gitleaks returns the pinned scanner, run from the binary at path. It
// refuses a binary whose digest is not the pinned one. The scan is
// `gitleaks dir <tree>` with no ignore file, no baseline and no
// environment of the caller's.
func Gitleaks(path string) (Scanner, error) {
	if path == "" {
		return nil, fmt.Errorf("no gitleaks binary given (it must be version %s)", GitleaksVersion)
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("the pinned gitleaks digest is for linux/amd64 only; this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != gitleaksSHA256LinuxX6 {
		return nil, fmt.Errorf("%s is not the pinned gitleaks %s binary (sha256 %s)", path, GitleaksVersion, got)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return func(dir string) ([]ScanFinding, error) {
		return runGitleaks(abs, dir)
	}, nil
}

func runGitleaks(bin, dir string) ([]ScanFinding, error) {
	work, err := os.MkdirTemp("", "relexport-scan-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	report := filepath.Join(work, "report.json")

	cmd := exec.Command(bin, "dir", dir, "--redact", "--no-banner", "--no-color",
		"--report-format", "json", "--report-path", report, "--exit-code", "1")
	// An empty directory as the working directory and a bare environment:
	// no .gitleaksignore, no baseline and no configuration of the caller's
	// can be picked up.
	cmd.Dir = work
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	data, err := os.ReadFile(report)
	if err != nil {
		return nil, fmt.Errorf("gitleaks wrote no report (%v): %s", runErr, strings.TrimSpace(out.String()))
	}
	var raw []struct {
		RuleID    string
		File      string
		StartLine int
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gitleaks report: %v", err)
	}
	if runErr != nil && len(raw) == 0 {
		return nil, fmt.Errorf("gitleaks failed without findings: %v: %s", runErr, strings.TrimSpace(out.String()))
	}
	if runErr == nil && len(raw) != 0 {
		return nil, fmt.Errorf("gitleaks exited 0 with %d findings in its report", len(raw))
	}
	fs := make([]ScanFinding, 0, len(raw))
	for _, f := range raw {
		rel, err := filepath.Rel(dir, f.File)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = f.File
		}
		fs = append(fs, ScanFinding{Rule: f.RuleID, File: filepath.ToSlash(rel), Line: f.StartLine})
	}
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		return fs[i].Line < fs[j].Line
	})
	return fs, nil
}
