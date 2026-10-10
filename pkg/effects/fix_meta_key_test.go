package effects

import (
	"strings"
	"testing"
)

// gonk-58bp: the fix agent's model cannot spell `rationale`, so fix_meta's
// explanation key is `reason` (a word it spells correctly, and already the
// key of scope_request). These tests go through the wire, not the Go struct,
// so they say the same thing about the old and the new code.

const metaKeyEdit = `{"kind":"edit","path":"a.py","old":"x","new":"y"}`

func TestFixMetaKeyIsReason(t *testing.T) {
	b, err := ParseBatch([]byte(`{"verdict":"fix","effects":[` + metaKeyEdit + `,{"kind":"fix_meta","reason":"why"}]}`))
	if err != nil {
		t.Fatalf("a fix_meta with key reason is refused at parse: %v", err)
	}
	if err := ValidateFix(b); err != nil {
		t.Fatalf("a fix_meta with key reason is refused by the shape gate: %v", err)
	}
}

func TestFixMetaOldKeyRationaleIsAnUnknownField(t *testing.T) {
	raw := []byte(`{"verdict":"fix","effects":[` + metaKeyEdit + `,{"kind":"fix_meta","rationale":"why"}]}`)
	_, err := ParseBatch(raw)
	if err == nil {
		t.Fatal("the old key rationale is accepted; it must be refused like any unknown field")
	}
	fb := ExplainParse(raw, err, FixKinds())
	if fb.Code != "parse" || !strings.Contains(fb.What, `"rationale"`) {
		t.Errorf("feedback does not name the unknown key: %+v", fb)
	}
	if want := "the keys of an effect of kind fix_meta are: kind, reason"; fb.Allowed != want {
		t.Errorf("feedback allowed = %q, want %q", fb.Allowed, want)
	}
}

func TestFixMetaReasonKeyForeignFieldRules(t *testing.T) {
	parse := func(effects string) Batch {
		t.Helper()
		b, err := ParseBatch([]byte(`{"verdict":"fix","effects":[` + effects + `]}`))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	meta := `{"kind":"fix_meta","reason":"why"}`
	// reason on an edit is another kind's field
	if r := AsRefusal(ValidateFix(parse(`{"kind":"edit","path":"a.py","old":"x","new":"y","reason":"z"},` + meta))); r == nil || r.Code() != "FX-SHAPE(foreign-field)" {
		t.Errorf("reason on an edit: got %v, want foreign-field", r)
	}
	// paths on a fix_meta is another kind's field
	if r := AsRefusal(ValidateFix(parse(metaKeyEdit + `,{"kind":"fix_meta","reason":"why","paths":["a.py"]}`))); r == nil || r.Code() != "FX-SHAPE(foreign-field)" {
		t.Errorf("paths on a fix_meta: got %v, want foreign-field", r)
	}
	// a fix-only key on a scope_request is still refused at parse (no such key now)
	b, err := ParseBatch([]byte(`{"verdict":"needs-scope","effects":[{"kind":"scope_request","paths":["a.py"],"reason":"why"}]}`))
	if err != nil || ValidateFix(b) != nil {
		t.Errorf("scope_request is unchanged: parse %v, validate %v", err, ValidateFix(b))
	}
	// the sub-codes
	if r := AsRefusal(ValidateFix(parse(metaKeyEdit + `,{"kind":"fix_meta","reason":" "}`))); r == nil || r.Code() != "FX-SHAPE(reason-empty)" {
		t.Errorf("blank reason: got %v, want reason-empty", r)
	}
	long := strings.Repeat("a", MaxFixReasonChars+1)
	if r := AsRefusal(ValidateFix(parse(metaKeyEdit + `,{"kind":"fix_meta","reason":"` + long + `"}`))); r == nil || r.Code() != "FX-SHAPE(reason-cap)" {
		t.Errorf("long reason: got %v, want reason-cap", r)
	}
}
