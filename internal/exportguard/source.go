package exportguard

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reSHA1    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	reSHA256  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// ValidCommitID reports whether s is a full, lower-case, 40-hex object id.
func ValidCommitID(s string) bool { return reSHA1.MatchString(s) }

// ValidVersion reports whether s is a release version, vX.Y.Z.
func ValidVersion(s string) bool { return reVersion.MatchString(s) }

// ExportSource is the content of the generated EXPORT-SOURCE file.
type ExportSource struct {
	Version      string // "" when the tree was built without a version
	SourceCommit string
	ListSHA256   string
}

// Render writes the file: fixed key order, LF line ends, no timestamp, so
// the same inputs give the same bytes anywhere.
func (s ExportSource) Render() ([]byte, error) {
	if s.Version != "" && !ValidVersion(s.Version) {
		return nil, fmt.Errorf("EXPORT-SOURCE: %q is not a vX.Y.Z version", s.Version)
	}
	if !ValidCommitID(s.SourceCommit) {
		return nil, fmt.Errorf("EXPORT-SOURCE: source commit must be 40 hex digits")
	}
	if !reSHA256.MatchString(s.ListSHA256) {
		return nil, fmt.Errorf("EXPORT-SOURCE: list digest must be 64 hex digits")
	}
	var b strings.Builder
	b.WriteString("format: 1\n")
	if s.Version != "" {
		b.WriteString("version: " + s.Version + "\n")
	}
	b.WriteString("source-commit: " + s.SourceCommit + "\n")
	b.WriteString("internal-only-sha256: " + s.ListSHA256 + "\n")
	return []byte(b.String()), nil
}

// ParseExportSource reads the file back, strictly: it accepts exactly what
// Render writes, so a hand-edited file is not mistaken for a generated one.
func ParseExportSource(data []byte) (ExportSource, error) {
	var s ExportSource
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 || lines[len(lines)-1] != "" {
		return s, fmt.Errorf("EXPORT-SOURCE: not LF-terminated")
	}
	lines = lines[:len(lines)-1]
	if len(lines) == 0 || lines[0] != "format: 1" {
		return s, fmt.Errorf("EXPORT-SOURCE: unknown format")
	}
	for _, line := range lines[1:] {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			return s, fmt.Errorf("EXPORT-SOURCE: malformed line")
		}
		switch k {
		case "version":
			s.Version = v
		case "source-commit":
			s.SourceCommit = v
		case "internal-only-sha256":
			s.ListSHA256 = v
		default:
			return s, fmt.Errorf("EXPORT-SOURCE: unknown key %q", k)
		}
	}
	want, err := s.Render()
	if err != nil {
		return s, err
	}
	if string(want) != string(data) {
		return s, fmt.Errorf("EXPORT-SOURCE: not in canonical form")
	}
	return s, nil
}
