package effects

import "testing"

// A batch without a verdict must keep working exactly as before: this is
// additive, and every agent and test that predates verdicts still produces one.
func TestAbsentVerdictDefaultsToReplyOnly(t *testing.T) {
	b, err := ParseBatch([]byte(`{"effects":[{"kind":"comment","body":"x"}]}`))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Verdict != "" {
		t.Fatalf("Verdict = %q, want empty (absent)", b.Verdict)
	}
	if got := b.EffectiveVerdict(); got != VerdictReplyOnly {
		t.Fatalf("EffectiveVerdict = %q, want %q", got, VerdictReplyOnly)
	}
}

func TestEachKnownVerdictParses(t *testing.T) {
	for _, v := range []Verdict{VerdictReplyOnly, VerdictCodeChange, VerdictClose} {
		raw := `{"verdict":"` + string(v) + `","effects":[{"kind":"comment","body":"x"}]}`
		b, err := ParseBatch([]byte(raw))
		if err != nil {
			t.Fatalf("ParseBatch(%q): %v", v, err)
		}
		if b.EffectiveVerdict() != v {
			t.Fatalf("EffectiveVerdict = %q, want %q", b.EffectiveVerdict(), v)
		}
	}
}

// An unknown verdict is a GATE FAILURE, not something to shrug off. It means
// the agent believes it asked for an outcome we are not going to deliver, and
// applying the effects while silently discarding the conclusion would be the
// worst of both -- especially if the agent thought it was closing an issue.
func TestUnknownVerdictIsRejected(t *testing.T) {
	for _, v := range []string{"escalate", "merge", "REPLY-ONLY", "reply_only", " close"} {
		raw := `{"verdict":"` + v + `","effects":[{"kind":"comment","body":"x"}]}`
		if _, err := ParseBatch([]byte(raw)); err == nil {
			t.Fatalf("ParseBatch accepted unknown verdict %q", v)
		}
	}
}
