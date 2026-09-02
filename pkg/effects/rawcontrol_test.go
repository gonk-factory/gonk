package effects

import "testing"

// A model asked for a multi-line comment body writes the newline literally
// instead of as \n. That is invalid JSON, and before this normalisation it cost
// the whole batch -- observed on issue !43 as
// "invalid character '\n' in string literal" (gonk-2tb).
func TestBatchWithRawNewlineInABodyStillParses(t *testing.T) {
	raw := []byte("{\"effects\":[{\"kind\":\"comment\",\"body\":\"line one\nline two\ttabbed\"}]}")
	b, err := ParseBatch(raw)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Effects) != 1 {
		t.Fatalf("effects = %d, want 1", len(b.Effects))
	}
	if got, want := b.Effects[0].Body, "line one\nline two\ttabbed"; got != want {
		t.Fatalf("body = %q, want %q -- the text must survive intact, not be flattened", got, want)
	}
}

// The normalisation must be a byte-for-byte identity on valid input: an already
// escaped \n is a backslash and an n, not a control byte, and must not be
// double-escaped into a literal backslash-n in the decoded text.
func TestAlreadyEscapedNewlinesAreUntouched(t *testing.T) {
	raw := []byte(`{"effects":[{"kind":"comment","body":"line one\nline two"}]}`)
	b, err := ParseBatch(raw)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if got, want := b.Effects[0].Body, "line one\nline two"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// A quote escaped inside a string must not be read as the string ending, or
// every control byte after it would be treated as outside a string.
func TestEscapedQuoteDoesNotEndTheString(t *testing.T) {
	raw := []byte("{\"effects\":[{\"kind\":\"comment\",\"body\":\"he said \\\"hi\\\"\nthen left\"}]}")
	b, err := ParseBatch(raw)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if got, want := b.Effects[0].Body, "he said \"hi\"\nthen left"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// Structural whitespace BETWEEN tokens is not inside a string and must be left
// alone -- a pretty-printed batch is still valid and must stay valid.
func TestPrettyPrintedBatchStillParses(t *testing.T) {
	raw := []byte("{\n  \"effects\": [\n    {\n      \"kind\": \"comment\",\n      \"body\": \"ok\"\n    }\n  ]\n}")
	b, err := ParseBatch(raw)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Effects) != 1 || b.Effects[0].Kind != "comment" {
		t.Fatalf("unexpected batch: %+v", b)
	}
}

// Normalisation must not rescue genuinely broken input: it is not a JSON repair.
func TestStillRejectsGenuinelyMalformedInput(t *testing.T) {
	for _, raw := range []string{
		`{"effects":[{"kind":"comment","body":"unterminated`,
		`{"effects":[{"kind":"comment","body":"x"},`,
		`not json at all`,
	} {
		if _, err := ParseBatch([]byte(raw)); err == nil {
			t.Fatalf("ParseBatch(%q) succeeded; malformed input must still be rejected", raw)
		}
	}
}
