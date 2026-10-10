// Package pipelinectx is the pure core of gonk-tios pipeline triage (plan
// docs/plans/2026-10-04-gonk-tios-pipeline-triage.md, part P1): it turns a raw
// GitLab job trace into a bounded, redacted, deterministic excerpt; computes
// the failing-job-set fingerprint and the run anchor; and decides whether an
// evidence line a model quoted is GROUNDED in that excerpt.
//
// Two of these are security controls, not conveniences:
//
//   - Redact is the secret scrub applied to every byte of log text before it
//     reaches a model, and to every comment before gonk posts it.
//   - Grounded is the hallucination check: an answer whose evidence is not
//     literally in the log it was shown is refused whole.
//
// Everything here is a pure function of its arguments: no network, no clock,
// no randomness, no package state that changes. Same input -> same bytes, which
// is what lets the sweep re-derive an excerpt at apply time and compare its
// hash with the one recorded at dispatch.
package pipelinectx

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MarkerPrefix starts every line gonk itself inserts into an excerpt (repeat
// collapse, omission, truncation). Lines that are exactly a gonk marker are
// never grounding targets: they are gonk's words, not the pipeline's.
const MarkerPrefix = "[gonk: "

// LineTruncatedSuffix is appended to a log line cut at MaxLineBytes. Grounded
// strips it before matching, so it can never be quoted as log text.
const LineTruncatedSuffix = " " + MarkerPrefix + "line truncated]"

// MinRepeatCollapse: a run of MORE than 3 identical consecutive lines (so 4 or
// more) is collapsed to one copy plus a repeat marker. Runs of 2 or 3 stay.
const MinRepeatCollapse = 4

// sectionRe matches GitLab's collapsible-section markers, e.g.
//
//	section_start:1700000000:step_script[collapsed=true]\r\x1b[0K
//	section_end:1700000000:step_script\r\x1b[0K
//
// The trailing \r and erase-line are part of the marker: left in place, the \r
// would make carriage-return resolution throw away the rest of the line.
var sectionRe = regexp.MustCompile(`section_(?:start|end):[0-9]+:[A-Za-z0-9_.\-]+(?:\[[^\]\r\n]*\])?\r?(?:\x1b\[0?K)?`)

// ansiRe matches ANSI escape sequences: CSI (ESC [ params final), OSC
// (ESC ] ... BEL|ST, or unterminated up to end of line), and the remaining
// two-byte ESC sequences.
var ansiRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b\n]*(?:\x07|\x1b\\)?|\x1b[@-Z\\-_]`)

// tracePrefixRe matches the prefix GitLab puts on EVERY line of a timestamped
// job trace (the runner's FF_TIMESTAMPS format; what GET jobs/:id/trace returns
// on GitLab 17+/runner 17+, read off a real trace on 2026-10-04 --
// testdata/gitlab_real_trace_project75_job28617.log):
//
//	2026-10-04T21:25:31.525472Z 01O $ exit 3
//	2026-10-04T21:25:22.027086Z 00O+section_start:1791149122:prepare_script...
//
// an RFC 3339 UTC timestamp (fraction optional), a space, two hex digits (the
// stream number), the stream type O (stdout) or E (stderr), and then ONE of
//
//	' '  the text after it starts a new line;
//	'+'  the text after it CONTINUES the previous line (the runner cut one
//	     line in two): GitLab's own log view appends it to that line.
//
// The prefix is the runner's envelope, not the job's output: left in, no
// command line of the job ("$ make test") is a whole excerpt line any more, so
// a model quoting it verbatim was refused as ungrounded (gonk-mf3v); the first
// error line and its signature carried a timestamp; and ~32 bytes per line of
// the excerpt budget went to it.
var tracePrefixRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z [0-9a-fA-F]{2}[OE][ +]`)

// stripTracePrefix removes tracePrefixRe from the start of every line that has
// it, and joins a '+' continuation line to the line before it. A line without
// the prefix (an older GitLab, a plain log) is left byte for byte as it was,
// and a trace with no prefixed line at all is returned unchanged. A
// continuation with no line before it (the first line of a head-truncated
// read) has nothing to join and stays a line of its own.
func stripTracePrefix(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	for len(raw) > 0 {
		line, rest, nl := raw, []byte(nil), false
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, rest, nl = raw[:i], raw[i+1:], true
		}
		raw = rest
		if m := tracePrefixRe.Find(line); m != nil {
			if m[len(m)-1] == '+' && len(out) > 0 && out[len(out)-1] == '\n' {
				out = out[:len(out)-1]
			}
			line = line[len(m):]
		}
		out = append(out, line...)
		if nl {
			out = append(out, '\n')
		}
	}
	return out
}

// Normalize turns a raw job trace into display lines:
//
//  0. GitLab's timestamped-trace line prefix is removed and continuation
//     lines are joined to the line they continue (stripTracePrefix);
//  1. GitLab section_start/section_end markers are removed;
//  2. ANSI CSI/OSC/ESC sequences are removed;
//  3. invalid UTF-8 is replaced with U+FFFD;
//  4. per line: CR-LF endings are dropped, then a carriage-return overwrite is
//     resolved by keeping only the text after the LAST \r (what a terminal
//     would finally show);
//  5. remaining control characters other than TAB are dropped;
//  6. trailing whitespace is trimmed;
//  7. a run of >= MinRepeatCollapse identical consecutive lines becomes one
//     line plus "[gonk: previous line repeated N times]";
//  8. trailing empty lines are dropped.
//
// It does NOT redact: Excerpt runs Redact after Normalize.
func Normalize(raw []byte) []string {
	b := stripTracePrefix(raw)
	b = sectionRe.ReplaceAll(b, nil)
	b = ansiRe.ReplaceAll(b, nil)
	s := strings.ToValidUTF8(string(b), "\uFFFD")

	parts := strings.Split(s, "\n")
	lines := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimRight(p, "\r")
		if i := strings.LastIndexByte(p, '\r'); i >= 0 {
			p = p[i+1:]
		}
		p = dropControls(p)
		p = strings.TrimRightFunc(p, unicode.IsSpace)
		lines = append(lines, p)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return collapseRepeats(lines)
}

func dropControls(s string) string {
	clean := true
	for _, r := range s {
		if r != '\t' && unicode.IsControl(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if r == '\t' || !unicode.IsControl(r) {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// RepeatMarker is the line that replaces the extra copies of a collapsed run.
func RepeatMarker(n int) string {
	return MarkerPrefix + "previous line repeated " + strconv.Itoa(n) + " times]"
}

func collapseRepeats(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		run := j - i
		if run >= MinRepeatCollapse {
			out = append(out, lines[i], RepeatMarker(run-1))
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return out
}

// isMarker reports whether a whole line is one gonk inserted.
func isMarker(line string) bool {
	return strings.HasPrefix(line, MarkerPrefix) && strings.HasSuffix(line, "]")
}

// truncateLine cuts a line to at most max bytes (suffix included) at a rune
// boundary, appending LineTruncatedSuffix.
func truncateLine(line string, max int) string {
	if len(line) <= max {
		return line
	}
	cut := max - len(LineTruncatedSuffix)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + LineTruncatedSuffix
}
