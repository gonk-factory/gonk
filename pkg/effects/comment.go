package effects

// The `comment` effect's content gate.
//
// A comment effect's Body is posted verbatim, under the bot's OWN PAT, into a
// GitLab Notes-API note. GitLab parses "quick actions" out of note bodies: a
// line whose first non-whitespace character is `/` followed by a command word
// executes as an action on the issue -- `/close`, `/label`, `/assign`,
// `/confidential`, `/due`, and more -- authored as if a human typed it into
// the web UI. A model asked to describe a bug can, deliberately or not, just
// as easily assert the actions of a maintainer; nothing about "propose a
// comment" should ever have meant "and also maybe close/reassign/deconfidential
// the issue" (R-03).
//
// This is a REFUSAL, not a repair. Stripping the offending line would leave a
// batch that looks accepted while quietly discarding part of what the model
// asked for -- exactly the silent mismatch ParseBatch's own control-character
// normalisation is careful NOT to be (see its doc comment). A batch containing
// one bad comment refuses the WHOLE batch; the caller (broker_apply.go) must
// run this before any write, the same discipline T-05's label gate uses.
//
// WHAT THE RULE COVERS, AND WHAT IT DELIBERATELY DOES NOT (T-06 review --
// no allow-list, no quick-action parser is built here, so these gaps are
// left open on purpose rather than by oversight):
//
//   - Leading whitespace is handled: TrimSpace runs before the check, so
//     "  /close" (an indented line) is caught just like "/close".
//   - A quick action after a blockquote marker (`> /close`) or inside a
//     fenced code block (```` ``` ```` ... `/close` ... ```` ``` ````) is NOT
//     caught by this rule: the trimmed line starts with `>` or backticks, not
//     `/`. This is not a bypass of the THREAT, though -- GitLab's own
//     quick-action parser also does not execute a quick action inside a
//     blockquote or code fence, so a comment shaped that way was never going
//     to execute anything either way. It IS a bypass of a differently-worded
//     rule ("body CONTAINS a quick action anywhere"), which is deliberately
//     not the rule implemented here.
//   - A quick action preceded by a zero-width character (U+200B ZERO WIDTH
//     SPACE and similar) before the `/` is NOT caught: TrimSpace does not
//     strip zero-width characters, so such a line does not "start with /"
//     under this check. Whether GitLab's own parser honours a leading
//     zero-width character is untested here; this rule takes no position on
//     it either way -- it is simply outside what "trim, then check the first
//     rune" can see.

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxCommentBytes caps one comment body. Measured in BYTES: len() on a Go
// string is the byte length of its UTF-8 encoding, not a rune count, so a
// body built from multi-byte characters hits this cap at far fewer than
// 65536 characters. See comment_test.go's multi-byte case, which pins this by
// constructing a body that is over the byte cap while under a naive rune
// count.
const MaxCommentBytes = 64 << 10 // 64 KiB

// ValidateComments enforces the comment-effect rules on a batch: no line may
// read as a GitLab quick action, and no body may exceed MaxCommentBytes. It
// is a no-op for batches with no comment effects, so the broker can call it
// unconditionally, the same way it calls ValidatePaths.
func ValidateComments(b Batch) error {
	for i, e := range b.Effects {
		if e.Kind != KindComment {
			continue
		}
		if n := len(e.Body); n > MaxCommentBytes {
			return fmt.Errorf("effects: effect %d: comment body is %d bytes, over the %d-byte cap", i, n, MaxCommentBytes)
		}
		for _, line := range strings.Split(e.Body, "\n") {
			trimmed := strings.TrimSpace(line)
			if isQuickActionLine(trimmed) {
				return fmt.Errorf("effects: effect %d: comment body line %q reads as a GitLab quick action -- refused, not stripped", i, trimmed)
			}
		}
	}
	return nil
}

// isQuickActionLine reports whether an ALREADY-TRIMMED line would be read by
// GitLab as a quick action: `/` immediately followed by a letter. A bare `/`,
// or `/` followed by anything that is not a letter (a digit, punctuation,
// whitespace, or end of line), is ordinary text -- "1/2 of the tests failed"
// and "a/b testing wraps up Friday" do not trip this, but "/close" does, and
// so does a line that is JUST "/close" with nothing else on it.
func isQuickActionLine(trimmed string) bool {
	if trimmed == "" || trimmed[0] != '/' {
		return false
	}
	rest := trimmed[1:]
	if rest == "" {
		return false
	}
	r, size := utf8.DecodeRuneInString(rest)
	if r == utf8.RuneError && size <= 1 {
		return false
	}
	return unicode.IsLetter(r)
}
