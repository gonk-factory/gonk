package exportguard

import (
	"bytes"
	"fmt"
	"sort"
)

// TreeScan applies a guard to the files of ONE tree: every path, every
// content, the rules about the secret scanner's own files, and the
// exemptions that excused nothing. It holds the per-file half of a check, so
// that the export (which reads blobs out of a commit) and the gate (which
// reads a working tree) cannot come to apply different rules.
//
// Use: Path for every file, Blob for every file's bytes, then Finish once.
type TreeScan struct {
	Guard *Guard
	// List, when set, is used only to COUNT the files that mention an
	// internal-only path (references that will dangle in the public tree).
	// Which files are scanned at all is the caller's decision.
	List *List

	Findings []Finding
	Problems []string
	// DanglingRefs is how many files mention an internal-only path.
	DanglingRefs int
	// Files is how many blobs were scanned.
	Files int
}

// Path scans one path of the tree, and holds the two rules about the files
// the secret scanner reads from the root of what it scans.
func (s *TreeScan) Path(path string) {
	s.Findings = append(s.Findings, s.Guard.Path(path)...)
	switch path {
	case ScannerIgnorePath:
		// An ignore file is never acceptable in an exported tree: its
		// fingerprints name commits, and a snapshot's id depends on its tree.
		s.Problems = append(s.Problems, fmt.Sprintf("%s is in the exported tree; the scanner would skip whatever it lists", path))
	case ScannerConfigPath:
		// A configuration file decides what the scanner calls a finding, so
		// it is acceptable only when the allow-list says so.
		if !s.Guard.AllowsScannerConfig(path) {
			s.Problems = append(s.Problems, fmt.Sprintf("%s is in the exported tree and changes what the secret scanner accepts; it needs a scanner-config entry in %s", path, AllowPath))
		}
	}
}

// Blob scans the bytes of the file at path.
func (s *TreeScan) Blob(path string, data []byte) {
	s.Files++
	if HasUTF16BOM(data) {
		// The rules read bytes. UTF-16 text hides every ASCII name from
		// them, so it is refused rather than passed unread.
		s.Problems = append(s.Problems, fmt.Sprintf("%s is UTF-16 text, which the guard cannot read; convert it to UTF-8", path))
	}
	s.Findings = append(s.Findings, s.Guard.Content(path, data)...)
	if s.List != nil {
		for _, entry := range s.List.Entries {
			if bytes.Contains(data, []byte(entry)) {
				s.DanglingRefs++
				break
			}
		}
	}
}

// Finish closes the scan: exemptions that excused nothing become problems,
// and the findings are put in path order.
func (s *TreeScan) Finish() {
	s.Problems = append(s.Problems, s.Guard.Stale()...)
	sortFindings(s.Findings)
}

// MergeScans closes several scans of DISJOINT parts of one tree as one scan:
// a tree may be scanned in parallel, one Guard per part (a Guard records
// which exemptions it used, so it serves one goroutine). An exemption is
// stale only if NO part used it. None of the parts may have been finished.
func MergeScans(parts []*TreeScan) *TreeScan {
	out := &TreeScan{}
	staleIn := map[string]int{}
	var order []string
	for _, p := range parts {
		out.Findings = append(out.Findings, p.Findings...)
		out.Problems = append(out.Problems, p.Problems...)
		out.DanglingRefs += p.DanglingRefs
		out.Files += p.Files
		for _, st := range p.Guard.Stale() {
			if staleIn[st] == 0 {
				order = append(order, st)
			}
			staleIn[st]++
		}
	}
	sort.Strings(out.Problems)
	for _, st := range order {
		if staleIn[st] == len(parts) {
			out.Problems = append(out.Problems, st)
		}
	}
	sortFindings(out.Findings)
	return out
}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Offset < b.Offset
	})
}

// HasUTF16BOM reports whether data starts with a UTF-16 byte order mark.
func HasUTF16BOM(data []byte) bool {
	return len(data) >= 2 && ((data[0] == 0xff && data[1] == 0xfe) || (data[0] == 0xfe && data[1] == 0xff))
}
