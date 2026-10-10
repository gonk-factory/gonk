package pipelinectx

import (
	"testing"
)

func TestGrounded(t *testing.T) {
	// Two jobs' excerpts, as recorded.
	jobA := "$ make build\n" +
		"./src/widget.go:12:3: undefined: FrobnicateAll\n" +
		"make: *** [Makefile:7: build] Error 1\n" +
		"exit 3\n" +
		RepeatMarker(5) + "\n" +
		OmittedMarker(40) + "\n" +
		"x  spaced\tout   words here\n" +
		"/usr/bin/tool: starts with a slash and is long\n" +
		"cut here because this line was too long" + LineTruncatedSuffix + "\n"
	jobB := "test_widget.py::test_blit FAILED - assert 3 == 4\n"

	cases := []struct {
		name    string
		excerpt string
		line    string
		want    bool
	}{
		{"whole line verbatim", jobA, "./src/widget.go:12:3: undefined: FrobnicateAll", true},
		{"substring of one line >= 12 runes", jobA, "undefined: FrobnicateAll", true},
		{"exactly 12 runes", jobA, "FrobnicateAl", true},
		{"11-rune fragment refused", jobA, "robnicateAl", false},
		{"whole short line accepted", jobA, "exit 3", true},
		{"whole short line, whitespace variant", jobA, "  exit\t3 ", true},
		{"short fragment of a longer line refused", jobA, "Error 1", false},
		{"short text that is not a line refused", jobA, "exit", false},
		{"whitespace collapsed in the quote", jobA, "undefined:   FrobnicateAll", true},
		{"whitespace collapsed in the excerpt", jobA, "x spaced out words here", true},
		{"leading/trailing whitespace trimmed", jobA, "\t undefined: FrobnicateAll  ", true},
		{"paraphrase refused", jobA, "undefined symbol FrobnicateAll", false},
		// B3 (verify report): punctuation is part of the line. A quote that
		// differs ONLY in punctuation -- dropped, added or swapped -- is a
		// paraphrase, not a copy.
		{"punctuation dropped refused", jobA, "./src/widget.go 12 3 undefined FrobnicateAll", false},
		{"one colon dropped refused", jobA, "undefined FrobnicateAll", false},
		{"punctuation swapped refused", jobA, "./src/widget.go:12:3; undefined: FrobnicateAll", false},
		{"punctuation added refused", jobA, "undefined: FrobnicateAll.", false},
		{"bracket dropped refused", jobA, "make: *** Makefile:7: build] Error 1", false},
		{"case changed refused", jobA, "UNDEFINED: FrobnicateAll", false},
		{"ellipsis join refused", jobA, "./src/widget.go:12:3: ... FrobnicateAll", false},
		{"two lines joined with a space refused", jobA, "FrobnicateAll make: *** [Makefile:7", false},
		{"two lines joined with a newline refused", jobA, "undefined: FrobnicateAll\nmake: *** [Makefile:7: build] Error 1", false},
		{"one line with a CR refused", jobA, "undefined: FrobnicateAll\r", false},
		{"cross-job line refused", jobA, "test_widget.py::test_blit FAILED", false},
		{"same line against its own job", jobB, "test_widget.py::test_blit FAILED", true},
		{"gonk marker is not evidence", jobA, RepeatMarker(5), false},
		{"gonk omission marker is not evidence", jobA, OmittedMarker(40), false},
		{"truncation suffix is not log text", jobA, "was too long [gonk: line truncated]", false},
		{"text before the truncation suffix is", jobA, "this line was too long", true},
		{"line starting with a slash", jobA, "/usr/bin/tool: starts with a slash", true},
		{"empty refused", jobA, "", false},
		{"whitespace only refused", jobA, " \t ", false},
		{"empty excerpt grounds nothing", "", "undefined: FrobnicateAll", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Grounded(c.excerpt, c.line); got != c.want {
				t.Fatalf("Grounded(%q) = %v, want %v", c.line, got, c.want)
			}
		})
	}
}

// The redacted excerpt is what is recorded; a quote holding the placeholder
// grounds, a quote holding the original secret does not (it was never shown).
func TestGroundedAgainstRedactedExcerpt(t *testing.T) {
	raw := secretsLog(t)
	e := Excerpt(raw)
	if !Grounded(e.Text, "aws id [REDACTED:aws_access_key] used") {
		t.Fatal("redacted quote refused")
	}
	if Grounded(e.Text, "aws id "+fxAKIA+" used") {
		t.Fatal("quote of a redacted secret accepted")
	}
}

func TestNorm(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"  a  ":            "a",
		"a \t\u00a0 b":     "a b",
		"a\u2003b\u3000 c": "a b c",
	} {
		if got := Norm(in); got != want {
			t.Errorf("Norm(%q) = %q, want %q", in, got, want)
		}
	}
}
