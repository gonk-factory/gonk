package diffmap

import (
	"fmt"
	"strconv"
	"strings"
)

// NoEOLMarker is the line Render puts under a line the diff marks as having
// no newline at its end. It is not a line of any file and has no coordinate.
const NoEOLMarker = `\ the line above has no newline at its end`

// Coord is a line's coordinate as Render prints it and as a reviewer cites
// it: "[OLD:n,NEW:m]" for a context line, "[NEW:m]" for an added one,
// "[OLD:n]" for a removed one.
func Coord(l Line) string {
	switch l.Op {
	case OpAdded:
		return "[NEW:" + strconv.Itoa(l.New) + "]"
	case OpRemoved:
		return "[OLD:" + strconv.Itoa(l.Old) + "]"
	}
	return "[OLD:" + strconv.Itoa(l.Old) + ",NEW:" + strconv.Itoa(l.New) + "]"
}

// Render is the annotated diff: the map written out as an addressing scheme.
// It is deterministic, and made of exactly four kinds of line (plus blank
// lines between files):
//
//	=== file 1 of 2: "<new path>" (<what happened>; <n> added, <m> removed)
//	@@ hunk 1 of 2: OLD lines 3-9, NEW lines 3-10
//	[OLD:7,NEW:8]   |<text of a context line>
//	[OLD:6]       - |<text of a removed line>
//	[NEW:6]       + |<text of an added line>
//	\ the line above has no newline at its end
//
// The text of a line is everything after the FIRST "|" of its line, byte for
// byte (leading whitespace included), so the scheme can be read back without
// knowing the column width. A line of FILE TEXT only ever appears after its
// coordinate, and a path is
// printed quoted (strconv.Quote), so nothing a file or a file name holds can
// start a line or pass for one of gonk's. A renamed file's line says under
// which path each side is cited. A file that is not reviewable is listed, with
// the words NOT REVIEWABLE and its status, and no lines.
func (m *Map) Render() string {
	var b strings.Builder
	for fi, f := range m.files {
		if fi > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "=== file %d of %d: %s (%s", fi+1, len(m.files), strconv.Quote(f.NewPath), what(f))
		switch f.Status {
		case StatusText:
			fmt.Fprintf(&b, "; %d added, %d removed)\n", f.Added, f.Removed)
		case StatusNoLines:
			b.WriteString("; no line changed, nothing to cite)\n")
		default:
			fmt.Fprintf(&b, ") NOT REVIEWABLE (%s): gonk could not read this file's changes; no line of it can be cited\n", f.Status)
		}
		width := 0
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				width = max(width, len(Coord(l)))
			}
		}
		for hi, h := range f.Hunks {
			fmt.Fprintf(&b, "@@ hunk %d of %d: OLD %s, NEW %s\n", hi+1, len(f.Hunks), span(h.OldStart, h.OldLines), span(h.NewStart, h.NewLines))
			for _, l := range h.Lines {
				c := Coord(l)
				mark := " "
				switch l.Op {
				case OpAdded:
					mark = "+"
				case OpRemoved:
					mark = "-"
				}
				b.WriteString(c + strings.Repeat(" ", width-len(c)+1) + mark + " |" + l.Text + "\n")
				if l.NoEOL {
					b.WriteString(NoEOLMarker + "\n")
				}
			}
		}
	}
	return b.String()
}

func what(f File) string {
	w := "modified"
	switch {
	case f.New:
		w = "added"
	case f.Deleted:
		w = "deleted"
	case f.Renamed || f.OldPath != f.NewPath:
		w = "renamed"
	}
	// Whatever the flags say, when the two paths differ the old side is cited
	// under the old one, and the reader must be told (fuzz crasher
	// testdata/fuzz/FuzzBuild/364e31a52947d6d1: a file flagged deleted AND
	// renamed was shown under its new path only).
	if f.OldPath != f.NewPath { // rule:render-old-path
		w += " from " + strconv.Quote(f.OldPath) + "; cite OLD lines under " + strconv.Quote(f.OldPath) + " and NEW lines under " + strconv.Quote(f.NewPath)
	}
	return w
}

func span(start, count int) string {
	if count == 0 {
		return "(none)"
	}
	return fmt.Sprintf("lines %d-%d", start, start+count-1)
}
