package pipelinectx

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MinGroundedRunes: an evidence quote shorter than this (after norm) is only
// accepted when it equals a whole excerpt line. Shorter fragments ("error",
// "exit code 1") occur in nearly every log and prove nothing.
const MinGroundedRunes = 12

// Norm trims s and collapses every run of Unicode whitespace to one space.
func Norm(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// Grounded is the hallucination check (plan D8 step 6). An evidence line is
// grounded in a job's excerpt iff
//
//   - it is one line: it holds no "\n" or "\r" (no joins across lines);
//   - norm(line) is non-empty and either has >= MinGroundedRunes runes or
//     equals norm(L) of a whole excerpt line L; and
//   - norm(line) is a substring of norm(L) for some SINGLE line L of excerpt.
//
// excerpt is JobExcerpt.Text of THAT job, exactly as recorded. Lines gonk
// inserted (markers) are not log text and never ground anything, and a
// LineTruncatedSuffix is not part of the line it ends. Matching is
// case-sensitive and literal: no ellipses, no paraphrase.
func Grounded(excerpt, line string) bool {
	if strings.ContainsAny(line, "\n\r") {
		return false
	}
	q := Norm(line)
	if q == "" {
		return false
	}
	short := utf8.RuneCountInString(q) < MinGroundedRunes
	for _, l := range strings.Split(excerpt, "\n") {
		if isMarker(l) {
			continue
		}
		n := Norm(strings.TrimSuffix(l, LineTruncatedSuffix))
		if short {
			if n == q {
				return true
			}
			continue
		}
		if strings.Contains(n, q) {
			return true
		}
	}
	return false
}
