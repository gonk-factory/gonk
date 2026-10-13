package effects

import (
	"errors"
	"testing"
)

func TestBatchUnmarshalKnownKinds(t *testing.T) {
	raw := `{"effects":[{"kind":"comment","body":"hi"},{"kind":"label","add":["gonk::bug"]}]}`
	b, err := ParseBatch([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Effects) != 2 || b.Effects[0].Kind != KindComment || b.Effects[1].Kind != KindLabel {
		t.Fatalf("unexpected batch: %+v", b)
	}
}

func TestBatchRejectsUnknownKind(t *testing.T) {
	if _, err := ParseBatch([]byte(`{"effects":[{"kind":"nuke"}]}`)); err == nil {
		t.Fatal("want error for unknown effect kind")
	}
}

func TestBatchRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseBatch([]byte(`{not json`)); err == nil {
		t.Fatal("want error for invalid JSON")
	}
}

// A KEY OF THE BATCH IS STATED ONCE. encoding/json takes the LAST of two equal
// keys and says nothing, so `{"effects":[],"effects":[...]}` parsed as its
// second list: whoever reads the text sees two lists and the gate saw one.
// Found by the v0.2.1 verification. The rule is about the batch object's own
// keys; a key repeated INSIDE an effect is not covered (see the last case).
func TestParseBatchRefusesAKeyOfTheBatchStatedTwice(t *testing.T) {
	const eff = `[{"kind":"comment","body":"hi"}]`
	for name, raw := range map[string]string{
		"effects twice, the empty one first": `{"effects":[],"effects":` + eff + `}`,
		"effects twice, the empty one last":  `{"effects":` + eff + `,"effects":[]}`,
		"effects twice, a verdict between":   `{"effects":[],"verdict":"reply-only","effects":` + eff + `}`,
		"verdict twice":                      `{"verdict":"fix","effects":` + eff + `,"verdict":"reply-only"}`,
		"verdict twice, the same value":      `{"verdict":"reply-only","verdict":"reply-only","effects":` + eff + `}`,
		// encoding/json matches a key to its field without regard to case, so
		// these are the same key to the decoder.
		"effects and Effects": `{"Effects":[],"effects":` + eff + `}`,
		"verdict and VERDICT": `{"verdict":"fix","VERDICT":"reply-only","effects":` + eff + `}`,
		"an escaped spelling": `{"effects":[],"\u0065ffects":` + eff + `}`,
		"three times":         `{"effects":[],"effects":[],"effects":` + eff + `}`,
	} {
		b, err := ParseBatch([]byte(raw))
		if !errors.Is(err, ErrBatchDuplicateKey) {
			t.Errorf("%s: got batch %+v, err %v; want %v", name, b, err, ErrBatchDuplicateKey)
		}
		if len(b.Effects) != 0 || b.Verdict != "" {
			t.Errorf("%s: a refused parse returned %+v", name, b)
		}
	}
	// Near misses: each key once, in either order and either case; the word
	// "effects" as a VALUE or inside a string; and the same key in two
	// DIFFERENT effects.
	for name, raw := range map[string]string{
		"each once":              `{"verdict":"reply-only","effects":` + eff + `}`,
		"the other order":        `{"effects":` + eff + `,"verdict":"reply-only"}`,
		"effects alone":          `{"effects":` + eff + `}`,
		"capitalised, once":      `{"Effects":` + eff + `,"Verdict":"reply-only"}`,
		"the word in a body":     `{"effects":[{"kind":"comment","body":"\"effects\":[],\"effects\":[]"}]}`,
		"two effects, same keys": `{"effects":[{"kind":"comment","body":"hi"},{"kind":"comment","body":"ho"}]}`,
	} {
		b, err := ParseBatch([]byte(raw))
		if err != nil || len(b.Effects) == 0 || b.Effects[0].Kind != KindComment {
			t.Errorf("%s: got %+v, %v; want the comment", name, b, err)
		}
	}
	// NOT covered, and held here so that a change is a decision: a key
	// repeated inside ONE effect still takes its last value.
	b, err := ParseBatch([]byte(`{"effects":[{"kind":"comment","body":"first","body":"last"}]}`))
	if err != nil || len(b.Effects) != 1 || b.Effects[0].Body != "last" {
		t.Errorf("a key repeated inside an effect: got %+v, %v; the documented behaviour is last-wins", b, err)
	}
}

// gonk-s0td: the batch is ONE JSON value. Until 2026-10-12 ParseBatch decoded
// the first value and never looked at what followed it, so an object followed
// by prose, or two objects, parsed as the first.
func TestParseBatchRefusesAnythingAfterTheBatch(t *testing.T) {
	const one = `{"effects":[{"kind":"comment","body":"hi"}]}`
	for name, raw := range map[string]string{
		"a second batch":            one + one,
		"a second batch, next line": one + "\n" + `{"effects":[{"kind":"label","add":["gonk::bug"]}]}`,
		"prose":                     one + " trailing",
		"a stray brace":             one + "}",
		"a stray bracket":           one + "\n]",
		"a scalar":                  one + " 1",
		"null":                      one + "null",
		"an unterminated string":    one + ` "x`,
		"a comma":                   one + ",",
	} {
		b, err := ParseBatch([]byte(raw))
		if !errors.Is(err, ErrBatchTrailingData) {
			t.Errorf("%s: got batch %+v, err %v; want %v", name, b, err, ErrBatchTrailingData)
		}
		if len(b.Effects) != 0 {
			t.Errorf("%s: a refused parse returned effects: %+v", name, b)
		}
	}
	// Near misses that ARE one value: white space around it, in every form
	// JSON allows (and the raw newline a model leaves after the object).
	for name, raw := range map[string]string{
		"exact":                one,
		"trailing newline":     one + "\n",
		"trailing white space": one + " \t\r\n\n",
		"leading white space":  "\n  " + one,
		"pretty-printed":       "{\n  \"effects\": [\n    {\"kind\": \"comment\", \"body\": \"hi\"}\n  ]\n}\n",
		"text that looks like a second batch, inside a string": `{"effects":[{"kind":"comment","body":"{\"effects\":[]} trailing"}]}`,
	} {
		b, err := ParseBatch([]byte(raw))
		if err != nil || len(b.Effects) != 1 || b.Effects[0].Kind != KindComment {
			t.Errorf("%s: got %+v, %v; want the one comment", name, b, err)
		}
	}
}
