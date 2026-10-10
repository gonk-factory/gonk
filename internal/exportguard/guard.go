// Package exportguard decides whether a tree, a path, or a piece of release
// text may be published to the public mirror (gonk-cuwn, release export).
//
// It is PURE: bytes in, findings out. No git, no filesystem, no clock. The
// git plumbing that builds the public tree lives in the gitexport
// sub-package; the command is hack/relexport.
//
// Two kinds of rule:
//
//   - GENERIC rules (classes G1-G7) are code in this package and are public:
//     private address ranges, key and certificate material, credentials in
//     URLs, e-mail addresses, local filesystem paths.
//   - SPECIFIC rules (classes I1-I5) are DATA. They spell the names that must
//     not be published, so they cannot live in a published file; the caller
//     hands them in as bytes (see parseDeny for the format).
//
// A Finding never carries the text that matched -- only the class, the rule,
// the path and the position. A report built from findings can therefore be
// pasted into a ticket or a CI log without repeating what it found.
//
// Reserved documentation values pass because NO RULE MATCHES THEM, not
// because they are excused: the RFC 5737 address blocks are not private, and
// names under the reserved example zones are not internal.
package exportguard

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Class is one deny-pattern class. There are twelve.
type Class string

// The twelve classes. G* are generic (code, public); I* are specific (data).
const (
	G1 Class = "G1" // RFC 1918 private IPv4
	G2 Class = "G2" // carrier-grade NAT IPv4
	G3 Class = "G3" // IPv6 unique-local and link-local
	G4 Class = "G4" // PEM and kubeconfig material, certificate fingerprints
	G5 Class = "G5" // credentials in URLs
	G6 Class = "G6" // e-mail addresses outside the allowed domains
	G7 Class = "G7" // local filesystem paths
	I1 Class = "I1" // internal DNS zone
	I2 Class = "I2" // cluster and node names
	I3 Class = "I3" // PKI names
	I4 Class = "I4" // gitops paths
	I5 Class = "I5" // kube context and account names
)

// GenericClasses and SpecificClasses list the classes in report order.
var (
	GenericClasses  = []Class{G1, G2, G3, G4, G5, G6, G7}
	SpecificClasses = []Class{I1, I2, I3, I4, I5}
)

// AllClasses is every class, generic first.
func AllClasses() []Class {
	out := append([]Class{}, GenericClasses...)
	return append(out, SpecificClasses...)
}

// Kind says what was scanned.
type Kind string

// The three things a rule is applied to.
const (
	KindContent Kind = "content" // the bytes of a blob
	KindPath    Kind = "path"    // a file path in the tree
	KindMessage Kind = "message" // a commit message, tag message or notes text
)

// Finding is one refused match. It deliberately has no field for the matched
// text.
type Finding struct {
	Class  Class  `json:"class"`
	Rule   string `json:"rule"`
	Kind   Kind   `json:"kind"`
	Path   string `json:"path"`   // the file, or the name of the message
	Line   int    `json:"line"`   // 1-based
	Col    int    `json:"col"`    // 1-based byte column
	Offset int    `json:"offset"` // 0-based byte offset of the match
	Length int    `json:"length"`
}

// String is the one-line form used in refusals: class, rule, where.
func (f Finding) String() string {
	return fmt.Sprintf("%s %s %s %s:%d:%d", f.Class, f.Rule, f.Kind, f.Path, f.Line, f.Col)
}

// match is a rule hit before it is excused, suppressed or positioned.
type match struct {
	class      Class
	rule       string
	start, end int
}

// Guard applies the rules. It records which allow-list entries excused
// something, so that Stale can refuse an entry that no longer matches
// anything; one Guard therefore serves exactly one scan of one tree.
type Guard struct {
	specific []specificRule
	allow    *allowList
}

// New builds a guard.
//
// deny is the specific-rule data (parseDeny). nil means PUBLIC mode: generic
// rules only. Non-nil deny data must carry at least one rule for every one of
// I1-I5, so that deleting a class from the data is a refusal, not a silent
// narrowing.
//
// allow is the allow-list data (parseAllow); nil means nothing is excused.
func New(deny, allow []byte) (*Guard, error) {
	g := &Guard{allow: &allowList{}}
	if deny != nil {
		rules, err := parseDeny(deny)
		if err != nil {
			return nil, err
		}
		g.specific = rules
	}
	if allow != nil {
		a, err := parseAllow(allow)
		if err != nil {
			return nil, err
		}
		g.allow = a
	}
	return g, nil
}

// Content scans the bytes of the blob at path. Binary blobs are scanned like
// any other: the rules work on bytes.
func (g *Guard) Content(path string, data []byte) []Finding {
	return g.scan(KindContent, path, path, data)
}

// Path scans a file path. Path-scoped allow entries do not apply to the path
// itself (they excuse a literal INSIDE the file); tree-wide ones do.
func (g *Guard) Path(path string) []Finding {
	return g.scan(KindPath, path, "", []byte(path))
}

// Message scans release text that is not a blob: a commit message, a tag
// message. name labels it in findings. allowAs, when not empty, is the path
// whose allow entries also apply -- a message derived from a release-notes
// file is excused exactly what that file is excused, and nothing more.
func (g *Guard) Message(name string, data []byte, allowAs string) []Finding {
	return g.scan(KindMessage, name, allowAs, data)
}

func (g *Guard) scan(kind Kind, name, allowPath string, data []byte) []Finding {
	var ms []match

	// Generic rules are not applied to upstream (vendored) content: it is
	// third-party code published elsewhere, full of example addresses and
	// author e-mail. The specific rules ARE applied there -- an internal name
	// has no business in vendored code either.
	upstream := false
	if kind != KindMessage {
		upstream = g.allow.underUpstream(name)
	}
	if !upstream {
		ms = append(ms, genericMatches(data, g.allow.domainAllowed)...)
	}
	for i := range g.specific {
		r := &g.specific[i]
		if r.scope != "" {
			// A scoped rule is about a place in the tree. It applies to the
			// content and path of files under the scope, never to a message.
			if kind == KindMessage || !strings.HasPrefix(name, r.scope) {
				continue
			}
		}
		for _, loc := range r.re.FindAllIndex(data, -1) {
			if loc[1] > loc[0] {
				ms = append(ms, match{class: r.class, rule: r.name, start: loc[0], end: loc[1]})
			}
		}
	}
	if len(ms) == 0 {
		return nil
	}

	ms = g.allow.excuse(ms, allowPath, data)
	ms = suppressContained(ms)

	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].start != ms[j].start {
			return ms[i].start < ms[j].start
		}
		if ms[i].class != ms[j].class {
			return ms[i].class < ms[j].class
		}
		return ms[i].rule < ms[j].rule
	})
	out := make([]Finding, 0, len(ms))
	for _, m := range ms {
		line, col := position(data, m.start)
		out = append(out, Finding{
			Class: m.class, Rule: m.rule, Kind: kind, Path: name,
			Line: line, Col: col, Offset: m.start, Length: m.end - m.start,
		})
	}
	return out
}

// suppressContained drops a match that lies strictly inside a longer match.
// One piece of text then counts once, under the rule that saw the most of it:
// a host name under the internal zone is the zone rule's finding, not also
// the bare-name rule's. Equal spans are both kept.
func suppressContained(ms []match) []match {
	out := ms[:0:0]
	for i, m := range ms {
		contained := false
		for j, o := range ms {
			if i == j {
				continue
			}
			if o.start <= m.start && m.end <= o.end && (o.end-o.start) > (m.end-m.start) {
				contained = true
				break
			}
		}
		if !contained {
			out = append(out, m)
		}
	}
	return out
}

// position turns a byte offset into a 1-based line and byte column.
func position(data []byte, off int) (line, col int) {
	if off > len(data) {
		off = len(data)
	}
	line = 1 + bytes.Count(data[:off], []byte{'\n'})
	col = off - bytes.LastIndexByte(data[:off], '\n')
	return line, col
}

// The files the secret scanner reads from the root of the tree it scans, by
// itself, with no flag: its configuration (which can allow-list anything) and
// its ignore file (fingerprints of findings to skip).
const (
	ScannerConfigPath = ".gitleaks.toml"
	ScannerIgnorePath = ".gitleaksignore"
)

// AllowsScannerConfig reports whether the allow-list permits the exported
// tree to carry the scanner configuration file at path. The scanner loads
// that file from the tree it scans, so committing one changes what "zero
// findings" means; it is allowed only by an explicit scanner-config entry.
func (g *Guard) AllowsScannerConfig(path string) bool {
	return g.allow.scannerConfigAllowed(path)
}

// Stale returns one description per allow-list entry that excused nothing in
// everything scanned so far. A stale entry is a refusal: an exemption that
// outlives the thing it exempted is an open door nobody is watching.
func (g *Guard) Stale() []string {
	return g.allow.stale()
}

// Summary counts findings per class.
type Summary struct {
	Class   Class    `json:"class"`
	Matches int      `json:"matches"`
	Files   []string `json:"files"`
}

// Summarize groups findings by class, in class order, with the distinct
// paths each class was found in. Every class appears, zero or not, so a
// report shows what was looked for and not only what was found.
func Summarize(fs []Finding) []Summary {
	by := map[Class]*Summary{}
	seen := map[Class]map[string]bool{}
	for _, c := range AllClasses() {
		by[c] = &Summary{Class: c, Files: []string{}}
		seen[c] = map[string]bool{}
	}
	for _, f := range fs {
		s, ok := by[f.Class]
		if !ok {
			s = &Summary{Class: f.Class, Files: []string{}}
			by[f.Class] = s
			seen[f.Class] = map[string]bool{}
		}
		s.Matches++
		if !seen[f.Class][f.Path] {
			seen[f.Class][f.Path] = true
			s.Files = append(s.Files, f.Path)
		}
	}
	out := make([]Summary, 0, len(by))
	for _, c := range AllClasses() {
		sort.Strings(by[c].Files)
		out = append(out, *by[c])
	}
	return out
}
