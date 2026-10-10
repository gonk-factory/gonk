// Package diffmap turns a merge request's file diffs into a LINE MAP: for
// every changed file, which old-side and new-side line numbers are inside a
// hunk, what each of those lines is (added, removed, context) and what its
// text is.
//
// It exists for independent review (gonk-td7; plan
// docs/plans/2026-10-05-gonk-cuwn-epic-remainder.md section 3). A reviewer
// model is shown the diff rewritten as an ADDRESSING SCHEME (Render): every
// line carries its coordinates, and those coordinates are declared the only
// source of locations. What the model then cites -- (path, side, line) and a
// quoted line -- is checked against this map (Lookup, Quote). A coordinate
// that is not in the map is unreachable, not judged (gate RV-ANCHOR), and a
// quote must be the text AT that coordinate (gate RV-QUOTE).
//
// PURE: FileDiff values in, a Map out. No forge client, no store, no clock.
//
// THE RULE EVERYTHING HERE SERVES: a diff gonk could not read line by line
// is NOT REVIEWABLE, and says why. It is never a map with no lines, which
// would read as "nothing to object to". A file GitLab withheld (too_large,
// collapsed), a binary file, a file with no diff text, and text this package
// cannot parse EXACTLY each get a Status that is not reviewable; Reviewable
// then answers a typed *NotReviewable for the whole map. Parsing is strict
// for the same reason: a hunk whose body does not match its header's counts
// is refused whole, because a line map that is off by one cites the wrong
// line with full confidence.
package diffmap

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// FileDiff is one file of a merge request's diff as GitLab returns it (GET
// .../merge_requests/:iid/diffs). It is glab.MRDiff field for field, so the
// controller converts with diffmap.FileDiff(d) (TestFileDiffIsGlabMRDiff);
// this package does not import the forge client.
type FileDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	Diff        string `json:"diff"`
	TooLarge    bool   `json:"too_large"`
	Collapsed   bool   `json:"collapsed"`
}

// MaxLineNumber bounds every number of a hunk header and every line number in
// a map. A header naming a larger one is refused.
const MaxLineNumber = 1<<31 - 1

// Side says which numbering an anchor uses: the file after the change (new)
// or before it (old). They are different spaces: old line 7 and new line 7
// are, in general, different lines.
type Side string

const (
	SideNew Side = "new"
	SideOld Side = "old"
)

// Op is what a line of a hunk is.
type Op string

const (
	// OpContext: unchanged; it has an old AND a new number.
	OpContext Op = "context"
	// OpAdded: only in the new file; it has no old number.
	OpAdded Op = "added"
	// OpRemoved: only in the old file; it has no new number.
	OpRemoved Op = "removed"
)

// Status is what this package could make of one file's diff. The zero value
// is not a Status, and is not reviewable.
type Status string

const (
	// StatusText: parsed; every line of every hunk is in the map.
	StatusText Status = "text"
	// StatusNoLines: a rename with no content change. Nothing was withheld
	// and there is nothing to cite.
	StatusNoLines Status = "no-lines"
	// StatusTooLarge / StatusCollapsed: GitLab withheld the text.
	StatusTooLarge  Status = "too-large"
	StatusCollapsed Status = "collapsed"
	// StatusBinary: GitLab says the file is binary.
	StatusBinary Status = "binary"
	// StatusNoText: no diff text and not a plain rename. A binary file, an
	// empty file and a mode-only change all look like this; gonk cannot tell
	// them apart, so it does not guess "nothing changed".
	StatusNoText Status = "no-text"
	// StatusMalformed: the text is not a unified diff this package can map
	// EXACTLY. File.Detail says what was wrong.
	StatusMalformed Status = "malformed"
	// StatusEmpty is a reason of the MAP, never of a file: no file of it has
	// a line that could be cited.
	StatusEmpty Status = "empty"
)

// Reviewable reports whether a file with this status was read in full.
func (s Status) Reviewable() bool {
	return s == StatusText || s == StatusNoLines
}

// Line is one line of a hunk. Old is 0 on an added line, New is 0 on a
// removed one.
type Line struct {
	Old, New int
	Op       Op
	// Text is the line AS SHOWN: without its diff prefix and line ending,
	// and made safe to show on one physical line (see showable). It is what
	// Render prints and what Quote matches against.
	Text string
	// NoEOL: the diff marks this line "\ No newline at end of file".
	NoEOL bool
}

// HunkHeader is "@@ -OldStart,OldLines +NewStart,NewLines @@ Section".
type HunkHeader struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Section            string
}

// Hunk is a header and the lines under it.
type Hunk struct {
	HunkHeader
	Lines []Line
}

// File is one changed file.
type File struct {
	OldPath, NewPath      string
	New, Renamed, Deleted bool
	Status                Status
	// Detail explains a StatusMalformed.
	Detail string
	// Hunks is empty unless Status is StatusText.
	Hunks          []Hunk
	Added, Removed int
}

// Anchor is a location a reviewer cites. Path is the file's NEW path for
// SideNew and its OLD path for SideOld (they differ only for a rename).
type Anchor struct {
	Path string
	Side Side
	Line int
}

// Reason is one cause of a map not being reviewable.
type Reason struct {
	Path   string // the file's new path; "" for StatusEmpty
	Status Status
	Detail string
}

// NotReviewable is Reviewable's answer for a map gonk must not put in front
// of a reviewer as if it were the whole change.
type NotReviewable struct{ Reasons []Reason }

func (e *NotReviewable) Error() string {
	parts := make([]string, 0, len(e.Reasons))
	for _, r := range e.Reasons {
		s := string(r.Status)
		if r.Path != "" {
			s = strconv.Quote(r.Path) + " is " + s
		}
		if r.Detail != "" {
			s += " (" + r.Detail + ")"
		}
		parts = append(parts, s)
	}
	return "diffmap: not reviewable: " + strings.Join(parts, "; ")
}

// QuoteMatch is Quote's answer.
type QuoteMatch string

const (
	// QuoteNoAnchor: the anchor is not a line of the diff.
	QuoteNoAnchor QuoteMatch = "no-anchor"
	// QuoteNoMatch: the anchor exists and the quote is not its text.
	QuoteNoMatch QuoteMatch = "no-match"
	// QuoteWithin: the quote is a proper part of the line's text.
	QuoteWithin QuoteMatch = "within"
	// QuoteWhole: the quote is the whole line.
	QuoteWhole QuoteMatch = "whole"
)

// ErrBadInput: the diffs cannot be keyed (an empty path, or two files that
// claim the same path on the same side).
var ErrBadInput = errors.New("diffmap: bad input")

// Map is the line map of one merge request diff. Build makes it; it is not
// changed afterwards.
type Map struct {
	files []File
	index map[Anchor]lineRef
}

type lineRef struct{ file, hunk, line int }

// Build maps diffs, in the order given. It returns an error (and no map) only
// when the files cannot be keyed; a file whose text cannot be read is in the
// map with a Status that says so, and makes the map not Reviewable.
func Build(diffs []FileDiff) (*Map, error) {
	m := &Map{index: map[Anchor]lineRef{}}
	type sideKey struct {
		path string
		side Side
	}
	owner := map[sideKey]bool{}
	for fi, d := range diffs {
		if d.OldPath == "" || d.NewPath == "" {
			return nil, fmt.Errorf("%w: file %d has an empty path (old %q, new %q)", ErrBadInput, fi+1, d.OldPath, d.NewPath)
		}
		// A new file has no old side and a deleted one no new side, so a
		// file created where a renamed one used to be is not a clash.
		var keys []sideKey
		if !d.DeletedFile {
			keys = append(keys, sideKey{d.NewPath, SideNew})
		}
		if !d.NewFile {
			keys = append(keys, sideKey{d.OldPath, SideOld})
		}
		for _, k := range keys {
			if owner[k] { // rule:duplicate-path
				return nil, fmt.Errorf("%w: two files claim %q on the %s side", ErrBadInput, k.path, k.side)
			}
			owner[k] = true
		}
		f := parseFile(d)
		for hi, h := range f.Hunks {
			for li, l := range h.Lines {
				ref := lineRef{fi, hi, li}
				if l.Op != OpRemoved { // rule:sides
					m.index[Anchor{f.NewPath, SideNew, l.New}] = ref
				}
				if l.Op != OpAdded {
					m.index[Anchor{f.OldPath, SideOld, l.Old}] = ref // rule:rename
				}
			}
		}
		m.files = append(m.files, f)
	}
	return m, nil
}

// Files returns the files in the order GitLab listed them. Read-only.
func (m *Map) Files() []File { return m.files }

// ChangedLines is the number of added plus removed lines in the map.
func (m *Map) ChangedLines() int {
	n := 0
	for _, f := range m.files {
		n += f.Added + f.Removed
	}
	return n
}

// Reviewable answers nil when every file was read in full and at least one
// line can be cited; otherwise a *NotReviewable naming every file that was
// not, or StatusEmpty.
func (m *Map) Reviewable() error {
	var reasons []Reason
	for _, f := range m.files {
		if !f.Status.Reviewable() { // rule:unreadable-file
			reasons = append(reasons, Reason{Path: f.NewPath, Status: f.Status, Detail: f.Detail})
		}
	}
	if len(reasons) == 0 && len(m.index) == 0 { // rule:empty-map
		reasons = append(reasons, Reason{Status: StatusEmpty, Detail: "no line of the diff can be cited"})
	}
	if len(reasons) > 0 {
		return &NotReviewable{Reasons: reasons}
	}
	return nil
}

// Lookup resolves an anchor: the line of the diff at (path, side, line), or
// false. Only a line INSIDE A HUNK resolves; the old side resolves under the
// file's old path and the new side under its new path; a removed line has no
// new number and an added line no old one.
func (m *Map) Lookup(a Anchor) (Line, bool) {
	ref, ok := m.index[a]
	if !ok { // rule:outside-hunk
		return Line{}, false
	}
	return m.files[ref.file].Hunks[ref.hunk].Lines[ref.line], true
}

// Quote says whether quote is the text at anchor a, under the SAME
// normalisation rule gonk grounds pipeline evidence with
// (pipelinectx.Grounded): the quote is one line (no "\n" or "\r"), both
// sides go through pipelinectx.Norm (trim, collapse whitespace runs), the
// normalised quote is non-empty, and matching is literal and case-sensitive.
// It reports whether the quote is the WHOLE line or a part of it; how long a
// part must be to count is the caller's gate (see QuoteRunes).
func (m *Map) Quote(a Anchor, quote string) QuoteMatch {
	l, ok := m.Lookup(a)
	if !ok {
		return QuoteNoAnchor
	}
	if strings.ContainsAny(quote, "\n\r") { // rule:quote-one-line
		return QuoteNoMatch
	}
	q := pipelinectx.Norm(quote)
	if q == "" { // rule:quote-empty
		return QuoteNoMatch
	}
	text := pipelinectx.Norm(l.Text)
	switch {
	case text == q:
		return QuoteWhole
	case strings.Contains(text, q):
		return QuoteWithin
	}
	return QuoteNoMatch
}

// QuoteRunes is the length, in characters, of quote as Quote compares it.
func QuoteRunes(quote string) int {
	return len([]rune(pipelinectx.Norm(quote)))
}

func parseFile(d FileDiff) File {
	f := File{OldPath: d.OldPath, NewPath: d.NewPath, New: d.NewFile, Renamed: d.RenamedFile, Deleted: d.DeletedFile}
	malformed := func(format string, args ...any) File {
		f.Status, f.Detail, f.Hunks, f.Added, f.Removed = StatusMalformed, fmt.Sprintf(format, args...), nil, 0, 0
		return f
	}
	switch {
	case d.TooLarge: // rule:too-large -- the flag wins over any text that came with it
		f.Status = StatusTooLarge
		return f
	case d.Collapsed: // rule:collapsed
		f.Status = StatusCollapsed
		return f
	case d.NewFile && d.DeletedFile:
		return malformed("the file is marked both new and deleted")
	}
	hunks, binary, err := parseDiff(d.Diff)
	switch {
	case err != nil:
		return malformed("%s", err.Error())
	case binary: // rule:binary
		f.Status = StatusBinary
		return f
	case len(hunks) == 0:
		if d.RenamedFile && !d.NewFile && !d.DeletedFile && d.OldPath != d.NewPath && d.Diff == "" {
			f.Status = StatusNoLines
			return f
		}
		f.Status = StatusNoText // rule:no-text
		return f
	}
	for _, h := range hunks {
		for _, l := range h.Lines {
			switch l.Op {
			case OpAdded:
				f.Added++
			case OpRemoved:
				f.Removed++
			}
			if d.NewFile && l.Op != OpAdded { // rule:new-file-old-side
				return malformed("a new file has an old-side line (old line %d)", l.Old)
			}
			if d.DeletedFile && l.Op != OpRemoved { // rule:deleted-file-new-side
				return malformed("a deleted file has a new-side line (new line %d)", l.New)
			}
		}
	}
	f.Status, f.Hunks = StatusText, hunks
	return f
}

// preamblePrefixes lists what may stand before the first hunk: the file header
// lines git writes. GitLab's API omits them today; older versions sent
// "--- a/x" and "+++ b/x".
var preamblePrefixes = []string{
	"diff --git ", "index ", "--- ", "+++ ", "new file mode ", "deleted file mode ",
	"old mode ", "new mode ", "similarity index ", "dissimilarity index ", "rename from ", "rename to ",
}

func parseDiff(text string) (hunks []Hunk, binary bool, err error) {
	if text == "" {
		return nil, false, nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r") // rule:crlf
	}
	i := 0
	for ; i < len(lines) && !strings.HasPrefix(lines[i], "@@"); i++ {
		l := lines[i]
		if l == "GIT binary patch" || (strings.HasPrefix(l, "Binary files ") && strings.HasSuffix(l, " differ")) {
			return nil, true, nil
		}
		ok := false
		for _, p := range preamblePrefixes {
			ok = ok || strings.HasPrefix(l, p)
		}
		if !ok { // rule:preamble
			return nil, false, fmt.Errorf("line %d: text before the first hunk that is not a file header", i+1)
		}
	}
	lastOld, lastNew := 0, 0
	for i < len(lines) {
		hh, herr := ParseHunkHeader(lines[i])
		if herr != nil {
			return nil, false, fmt.Errorf("line %d: %w", i+1, herr)
		}
		i++
		if hh.OldLines == 0 && hh.NewLines == 0 {
			return nil, false, fmt.Errorf("line %d: empty hunk", i)
		}
		if (hh.OldLines > 0 && hh.OldStart == 0) || (hh.NewLines > 0 && hh.NewStart == 0) {
			return nil, false, fmt.Errorf("line %d: a hunk with lines starts at line 0", i)
		}
		// rule:hunk-order -- hunks ascend and never share a line number on
		// either side, or one number would name two texts.
		if (hh.OldLines > 0 && hh.OldStart <= lastOld) || (hh.NewLines > 0 && hh.NewStart <= lastNew) {
			return nil, false, fmt.Errorf("line %d: hunks overlap or are out of order", i)
		}
		// The last line of each side is start+count-1.
		if hh.OldStart-1 > MaxLineNumber-hh.OldLines || hh.NewStart-1 > MaxLineNumber-hh.NewLines { // rule:hunk-range
			return nil, false, fmt.Errorf("line %d: hunk runs past line %d", i, MaxLineNumber)
		}
		h := Hunk{HunkHeader: hh}
		oldN, newN := hh.OldStart, hh.NewStart
		remOld, remNew := hh.OldLines, hh.NewLines
		for remOld > 0 || remNew > 0 { // rule:hunk-counts
			if i >= len(lines) {
				return nil, false, fmt.Errorf("line %d: the hunk ends early: %d old and %d new lines missing", i, remOld, remNew)
			}
			raw := lines[i]
			i++
			if raw == "" {
				raw = " " // a blank context line whose single space was stripped
			}
			body := showable(raw[1:])
			switch raw[0] {
			case ' ':
				if remOld == 0 || remNew == 0 {
					return nil, false, fmt.Errorf("line %d: a context line past the count of the %s side", i, exhausted(remOld))
				}
				h.Lines = append(h.Lines, Line{Old: oldN, New: newN, Op: OpContext, Text: body})
				oldN, newN, remOld, remNew = oldN+1, newN+1, remOld-1, remNew-1
			case '-':
				if remOld == 0 {
					return nil, false, fmt.Errorf("line %d: a removed line past the count of the old side", i)
				}
				h.Lines = append(h.Lines, Line{Old: oldN, Op: OpRemoved, Text: body})
				oldN, remOld = oldN+1, remOld-1
			case '+':
				if remNew == 0 {
					return nil, false, fmt.Errorf("line %d: an added line past the count of the new side", i)
				}
				h.Lines = append(h.Lines, Line{New: newN, Op: OpAdded, Text: body})
				newN, remNew = newN+1, remNew-1
			case '\\': // rule:no-eol -- a marker about the line above, not a line
				if len(h.Lines) == 0 {
					return nil, false, fmt.Errorf("line %d: a no-newline marker with no line before it", i)
				}
				h.Lines[len(h.Lines)-1].NoEOL = true
			default:
				return nil, false, fmt.Errorf("line %d: unknown line prefix %q in a hunk", i, raw[:1])
			}
		}
		for i < len(lines) && strings.HasPrefix(lines[i], "\\") {
			h.Lines[len(h.Lines)-1].NoEOL = true
			i++
		}
		if hh.OldLines > 0 {
			lastOld = oldN - 1
		}
		if hh.NewLines > 0 {
			lastNew = newN - 1
		}
		hunks = append(hunks, h)
	}
	return hunks, false, nil
}

func exhausted(remOld int) string {
	if remOld == 0 {
		return "old"
	}
	return "new"
}

// ParseHunkHeader parses one hunk header line:
//
//	@@ -<start>[,<count>] +<start>[,<count>] @@[ <section>]
//
// An omitted count is 1 (not 0): "@@ -5 +5 @@" is a one-line hunk. Numbers
// are plain ASCII digits, at most MaxLineNumber. Combined-diff headers
// ("@@@ ... @@@") are refused.
func ParseHunkHeader(s string) (HunkHeader, error) {
	bad := func(why string) (HunkHeader, error) {
		return HunkHeader{}, fmt.Errorf("bad hunk header %q: %s", clip(s), why)
	}
	rest, ok := strings.CutPrefix(s, "@@ -")
	if !ok {
		return bad(`does not start with "@@ -"`)
	}
	var h HunkHeader
	if h.OldStart, h.OldLines, rest, ok = parseRange(rest); !ok {
		return bad("old range")
	}
	if rest, ok = strings.CutPrefix(rest, " +"); !ok {
		return bad(`no " +" after the old range`)
	}
	if h.NewStart, h.NewLines, rest, ok = parseRange(rest); !ok {
		return bad("new range")
	}
	if rest, ok = strings.CutPrefix(rest, " @@"); !ok {
		return bad(`no " @@" after the new range`)
	}
	if rest != "" {
		if rest[0] != ' ' {
			return bad(`text directly after the closing "@@"`)
		}
		h.Section = rest[1:]
	}
	return h, nil
}

// String is the header in the form ParseHunkHeader reads, counts written out.
func (h HunkHeader) String() string {
	s := fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	if h.Section != "" {
		s += " " + h.Section
	}
	return s
}

// parseRange reads "<start>" or "<start>,<count>" off the front of s.
func parseRange(s string) (start, count int, rest string, ok bool) {
	if start, rest, ok = parseNumber(s); !ok {
		return 0, 0, "", false
	}
	count = 1 // rule:omitted-count
	if after, found := strings.CutPrefix(rest, ","); found {
		if count, rest, ok = parseNumber(after); !ok {
			return 0, 0, "", false
		}
	}
	return start, count, rest, true
}

func parseNumber(s string) (n int, rest string, ok bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i > 10 {
		return 0, "", false
	}
	v, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil || v > MaxLineNumber {
		return 0, "", false
	}
	return int(v), s[i:], true
}

func clip(s string) string {
	if len(s) > 80 {
		return strings.ToValidUTF8(s[:80], "") + "..."
	}
	return s
}

// showable makes a line of file text safe to show on ONE physical line, and
// is applied once, when the map is built, so that what is shown and what a
// quote is matched against are the same string:
//
//   - invalid UTF-8 becomes "?";
//   - a character that would break the line (CR, VT, FF, NEL, LS, PS)
//     becomes a space;
//   - any other control character (a tab excepted) and the bidirectional
//     override and isolate characters, which can hide or reorder what a
//     reader sees, become "?".
func showable(s string) string {
	s = strings.ToValidUTF8(s, "?")
	return strings.Map(func(r rune) rune { // rule:showable
		switch {
		case r == '\t':
			return r
		case r == '\r' || r == '\v' || r == '\f' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029:
			return ' '
		case unicode.IsControl(r), r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return '?'
		}
		return r
	}, s)
}
