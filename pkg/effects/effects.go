// Package effects is the proposed-effects contract: the typed batch an agent
// returns instead of making live GitLab writes, plus its shape validator.
// It is pure -- no GitLab, no bd -- so the broker's decision is deterministic
// and reproducible in tests.
package effects

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type Kind string

const (
	KindComment  Kind = "comment"
	KindLabel    Kind = "label"
	KindNewIssue Kind = "new_issue" // reserved: contract admits it, apply not built (spec N1)
	// KindFile is repository content proposed by an agent that holds no forge
	// credentials: the broker commits it. Its path gate lives in file.go and is
	// a security boundary -- see the header there before touching either.
	KindFile Kind = "file"
	// merge_request/commit are reserved and deliberately NOT defined here:
	// naming them is a forward-compat note, and a constant with no
	// validator/apply would invite half-support. Note that scaffold does NOT
	// need them -- the broker opens the MR, so the agent never proposes one.
)

var knownKinds = map[Kind]bool{KindComment: true, KindLabel: true, KindNewIssue: true, KindFile: true}

// Effect is one intended externally-visible change. Fields are a superset;
// which are meaningful depends on Kind (validated per-kind elsewhere).
type Effect struct {
	Kind   Kind     `json:"kind"`
	Body   string   `json:"body,omitempty"`   // comment
	Add    []string `json:"add,omitempty"`    // label
	Remove []string `json:"remove,omitempty"` // label
	Title  string   `json:"title,omitempty"`  // new_issue
	// Path/Content carry a `file` effect. Content is the WHOLE file: the broker
	// replaces rather than patches, so a batch is a complete statement of what
	// .agent/ should contain and there is no diff for a model to get subtly
	// wrong.
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	// TargetIID names an existing resource this effect acts on (0 = the session's
	// primary target, bound from injected context in shape.go).
	TargetIID int64 `json:"target_iid,omitempty"`
}

// Verdict is what the agent concluded about the WORK ITEM as a whole, as
// opposed to Effect, which is one change to make. Triage without it has exactly
// one outcome -- comment and labels -- so an agent that decides "this is a real
// bug with an obvious fix" and one that decides "this is a duplicate, close it"
// produce the identical result and the broker cannot tell them apart.
//
// A verdict is MODEL-AUTHORED, so it is a PROPOSAL and never ground truth
// (ADR-007). It is validated against the closed set below before anything acts
// on it, exactly as effect kinds are. What a verdict MEANS -- whether an issue
// gets closed, whether an MR is opened -- is decided by the broker
// deterministically; the model states a conclusion, it does not perform one.
type Verdict string

const (
	// VerdictReplyOnly: no code change is needed, but the reporter needs an
	// answer -- a question, a clarification, "works as designed and here is why".
	VerdictReplyOnly Verdict = "reply-only"
	// VerdictCodeChange: a genuine defect with an identifiable fix. What happens
	// next depends on the project's actions.features policy, not on the model.
	VerdictCodeChange Verdict = "code-change"
	// VerdictClose: terminal -- duplicate, obsolete, already fixed, not
	// reproducible. The only verdict that ends a conversation, and so the one to
	// be most conservative about; the prompt tells the agent to prefer
	// reply-only when unsure.
	VerdictClose Verdict = "close"
)

var knownVerdicts = map[Verdict]bool{
	VerdictReplyOnly: true, VerdictCodeChange: true, VerdictClose: true,
}

// Batch is the ordered list a run returns.
type Batch struct {
	Effects []Effect `json:"effects"`
	// Verdict is OPTIONAL: a batch without one is treated as reply-only, so
	// existing agents and existing tests keep their current behaviour and this
	// stays additive. An UNKNOWN verdict is not tolerated -- that is a gate
	// failure, like an unknown kind, because it means the agent believes it
	// asked for something we are not going to do.
	Verdict Verdict `json:"verdict,omitempty"`
}

// EffectiveVerdict is Verdict with the empty-means-reply-only rule applied, so
// callers never re-implement the default.
func (b Batch) EffectiveVerdict() Verdict {
	if b.Verdict == "" {
		return VerdictReplyOnly
	}
	return b.Verdict
}

// ParseBatch decodes and structurally validates a batch. It rejects invalid
// JSON and unknown effect kinds -- a run that cannot produce a valid batch
// fails; it never half-applies.
// escapeRawControlsInStrings rewrites literal newline, carriage-return and tab
// bytes that appear INSIDE a JSON string literal into their escaped forms.
//
// JSON forbids raw control characters in strings, and models break that rule
// constantly: asked for a multi-line comment body, a model writes the newline
// literally instead of as \n. Observed on issue !43, where an otherwise correct
// triage batch was rejected with "invalid character '\n' in string literal" --
// the agent's analysis was fine and we threw it away over a byte.
//
// This is a NORMALISATION, not a repair, and deliberately the narrowest one that
// helps: it cannot change the meaning of any VALID batch, because valid JSON
// cannot contain these bytes inside a string in the first place, so for
// well-formed input it is a byte-for-byte identity. It does not balance braces,
// close quotes, strip prose or guess at intent -- anything still malformed after
// this stays malformed and is still rejected. The shape gate remains the sole
// authority on what a batch is allowed to DO; this only decides whether we can
// read it at all.
//
// Byte-wise is safe for UTF-8: only ASCII control bytes are special-cased, and
// multi-byte sequences are >= 0x80, so they pass through untouched.
func escapeRawControlsInStrings(raw []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(raw))
	inString, escaped := false, false
	for _, b := range raw {
		if !inString {
			if b == '"' {
				inString = true
			}
			out.WriteByte(b)
			continue
		}
		if escaped {
			out.WriteByte(b)
			escaped = false
			continue
		}
		switch b {
		case '\\':
			out.WriteByte(b)
			escaped = true
		case '"':
			inString = false
			out.WriteByte(b)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			out.WriteByte(b)
		}
	}
	return out.Bytes()
}

func ParseBatch(raw []byte) (Batch, error) {
	var b Batch
	dec := json.NewDecoder(bytes.NewReader(escapeRawControlsInStrings(raw)))
	// DisallowUnknownFields is CORRECT and intentional: Effect is a superset
	// struct, so {"kind":"comment","body":"x"} decodes cleanly and fields like
	// `add`/`title` are known struct fields (never wrongly rejected); a genuinely
	// unknown key IS rejected. Do not remove it.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Batch{}, fmt.Errorf("effects: parse batch: %w", err)
	}
	for i, e := range b.Effects {
		if !knownKinds[e.Kind] {
			return Batch{}, fmt.Errorf("effects: effect %d has unknown kind %q", i, e.Kind)
		}
	}
	if b.Verdict != "" && !knownVerdicts[b.Verdict] {
		return Batch{}, fmt.Errorf("effects: unknown verdict %q", b.Verdict)
	}
	return b, nil
}
