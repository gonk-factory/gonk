package effects

import (
	"strings"
	"testing"
)

// T-06 (closes R-03, R-16): a comment effect's body is posted verbatim under
// the bot's own PAT, and GitLab executes quick actions found in Notes-API
// note bodies. ValidateComments is the deterministic gate that refuses such a
// batch before any write.

func commentBatch(body string) Batch {
	return Batch{Effects: []Effect{{Kind: KindComment, Body: body}}}
}

// A line that is exactly a quick action is refused.
func TestValidateCommentsRejectsQuickAction(t *testing.T) {
	if err := ValidateComments(commentBatch("/close")); err == nil {
		t.Fatal("body = \"/close\": err = nil, want an error")
	}
}

// The rule applies to ANY line in a multi-line body, not just the first or
// last -- an agent could bury the quick action in the middle of an otherwise
// innocuous comment.
func TestValidateCommentsRejectsQuickActionOnAnyLine(t *testing.T) {
	body := "Thanks for the report -- this looks fixed already.\n/close\nClosing as a duplicate of #12."
	if err := ValidateComments(commentBatch(body)); err == nil {
		t.Fatal("quick action mid-body: err = nil, want an error")
	}
}

// Leading whitespace is handled by TrimSpace before the check runs.
func TestValidateCommentsRejectsIndentedQuickAction(t *testing.T) {
	if err := ValidateComments(commentBatch("   /label ~bug")); err == nil {
		t.Fatal("indented quick action: err = nil, want an error")
	}
}

// Several of GitLab's actual quick actions, each on its own single-line body.
func TestValidateCommentsRejectsKnownQuickActions(t *testing.T) {
	for _, action := range []string{"/close", "/label ~bug", "/assign @alice", "/confidential", "/due tomorrow"} {
		if err := ValidateComments(commentBatch(action)); err == nil {
			t.Errorf("body = %q: err = nil, want an error", action)
		}
	}
}

// `/` NOT followed by a letter is ordinary text, not a quick action: a bare
// slash, a slash-then-digit, and a slash-then-slash must all pass.
func TestValidateCommentsAcceptsSlashThatIsNotAQuickAction(t *testing.T) {
	for _, body := range []string{"/", "/2 of the work is done", "//comment", "/ leading space after slash"} {
		if err := ValidateComments(commentBatch(body)); err != nil {
			t.Errorf("body = %q: err = %v, want nil", body, err)
		}
	}
}

// A legitimate body that mentions a path MID-LINE (not as the first
// character of a line) must pass -- this is the exact case the review asked
// to be pinned, since "path/to/file" and "/close" share no structure once you
// require the slash to be the first non-whitespace character of a line.
func TestValidateCommentsAcceptsPathMidLine(t *testing.T) {
	body := "The failing assertion is in path/to/file at line 42; see the stack trace above."
	if err := ValidateComments(commentBatch(body)); err != nil {
		t.Fatalf("body with mid-line path: err = %v, want nil", err)
	}
}

// A comment at exactly the cap passes.
func TestValidateCommentsAcceptsBodyAtCap(t *testing.T) {
	body := strings.Repeat("a", MaxCommentBytes)
	if err := ValidateComments(commentBatch(body)); err != nil {
		t.Fatalf("body at exactly %d bytes: err = %v, want nil", MaxCommentBytes, err)
	}
}

// One byte over the cap is refused.
func TestValidateCommentsRejectsOversizeBody(t *testing.T) {
	body := strings.Repeat("a", MaxCommentBytes+1)
	if err := ValidateComments(commentBatch(body)); err == nil {
		t.Fatalf("body at %d bytes: err = nil, want an error", MaxCommentBytes+1)
	}
}

// The cap is measured in BYTES, not runes. "日" encodes to 3 bytes in UTF-8;
// 21,847 copies of it are 65,541 bytes (over the 65,536-byte cap) but only
// 21,847 runes (nowhere near it). An implementation that counted runes
// instead of bytes would wrongly accept this body -- this test fails on that
// mutation and passes on the real (byte-based) one.
func TestValidateCommentsCapIsMeasuredInBytes(t *testing.T) {
	const runeCount = 21847
	body := strings.Repeat("日", runeCount)
	if byteLen := len(body); byteLen <= MaxCommentBytes {
		t.Fatalf("test setup: body is %d bytes, want > %d", byteLen, MaxCommentBytes)
	}
	if runeLen := len([]rune(body)); runeLen >= MaxCommentBytes {
		t.Fatalf("test setup: body is %d runes, want < %d (so a rune-based cap would wrongly accept it)", runeLen, MaxCommentBytes)
	}
	if err := ValidateComments(commentBatch(body)); err == nil {
		t.Fatal("multi-byte body over the byte cap: err = nil, want an error")
	}
}

// A batch with no comment effects is untouched -- ValidateComments must be
// safe to call unconditionally alongside ValidatePaths.
func TestValidateCommentsNoOpWithoutCommentEffects(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindLabel, Add: []string{"bug"}}}}
	if err := ValidateComments(b); err != nil {
		t.Fatalf("batch with no comment effects: err = %v, want nil", err)
	}
}

// A batch with one bad comment among other, otherwise-fine effects is refused
// as a whole -- ValidateComments itself has no notion of "apply the good ones
// and skip the bad one"; the caller must treat any non-nil error as a refusal
// of the entire batch (proven at the broker level in
// TestSweepBrokerRefusesWholeBatchOnQuickAction).
func TestValidateCommentsRejectsBatchWithOneBadCommentAmongGoodEffects(t *testing.T) {
	b := Batch{Effects: []Effect{
		{Kind: KindLabel, Add: []string{"bug"}},
		{Kind: KindComment, Body: "/close"},
	}}
	if err := ValidateComments(b); err == nil {
		t.Fatal("batch with one bad comment: err = nil, want an error")
	}
}
