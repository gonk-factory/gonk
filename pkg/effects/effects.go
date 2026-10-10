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
	// KindDiagnosis is a failed pipeline's diagnosis (gonk-tios D7a): a typed
	// answer, not a free comment, because the controller checks its evidence
	// against the job logs and derives labels from its closed classification
	// set. The controller renders and lands it; the agent never chooses where.
	// Its rules live in diagnosis.go.
	KindDiagnosis Kind = "diagnosis"
	// merge_request/commit are reserved and deliberately NOT defined here:
	// naming them is a forward-compat note, and a constant with no
	// validator/apply would invite half-support. Note that scaffold does NOT
	// need them -- the broker opens the MR, so the agent never proposes one.
)

// KindEdit, KindFixMeta and KindScopeRequest (the pipeline-fix vocabulary,
// gonk-2usw) are declared in fix.go with their rules.
var knownKinds = map[Kind]bool{
	KindComment: true, KindLabel: true, KindNewIssue: true, KindFile: true, KindDiagnosis: true,
	KindEdit: true, KindFixMeta: true, KindScopeRequest: true,
}

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
	// TargetType says WHICH resource family TargetIID is an iid of. An issue's
	// iid 5 and merge request !5 are different resources, so a bare iid is
	// ambiguous. OPTIONAL for backward compatibility: empty means the default
	// for the effect's kind (see effectTargetKinds in shape.go) -- "issue" for
	// every kind today -- so existing triage batches parse and validate
	// unchanged. A value the effect kind does not accept is a gate failure.
	TargetType TargetKind `json:"target_type,omitempty"`

	// Classification, Summary, Evidence, Remedy and Files carry a `diagnosis`
	// effect (diagnosis.go). Files are repository-relative paths the diagnosis
	// POINTS AT -- the agent proposes no change to them -- but they are paths
	// all the same, so effectPaths enumerates them (protected.go).
	Classification Classification `json:"classification,omitempty"`
	Summary        string         `json:"summary,omitempty"`
	Evidence       []EvidenceItem `json:"evidence,omitempty"`
	Remedy         string         `json:"remedy,omitempty"`
	Files          []string       `json:"files,omitempty"`

	// Old and New carry an `edit` effect, with Path (fix.go, edit.go): an
	// exact-match replacement in the EXISTING file at Path. New may be empty (a
	// removal). Unlike a `file` effect an edit never carries the whole file:
	// the controller applies it to the blob it read at the pinned base SHA.
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
	// Reason carries a `fix_meta` effect (the agent's explanation, rendered by
	// the controller into the merge request description) and a `scope_request`
	// effect (why the session needs the paths). One key for both: it was
	// `rationale` for fix_meta until gonk-58bp, when the fix agent's model
	// kept misspelling it; each kind still owns it separately (fixKindFields).
	// Paths also carries a `scope_request` effect (verdict needs-scope).
	// Paths are repository paths the session says it would have needed: a
	// CITATION, rendered as text and never written (citationOnlyPathFields).
	Paths  []string `json:"paths,omitempty"`
	Reason string   `json:"reason,omitempty"`
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

// VerdictFix and VerdictNeedsScope (gonk-2usw) are declared in fix.go. They
// are in the closed set for every agent's PARSE, and usable by none but the
// fix agent: Validate's verdict/kind rule ties each to effect kinds only that
// agent's shape lists (TestExistingAgentShapesRefuseTheFixVocabulary).
var knownVerdicts = map[Verdict]bool{
	VerdictReplyOnly: true, VerdictCodeChange: true, VerdictClose: true,
	VerdictFix: true, VerdictNeedsScope: true,
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

// ParseReason names which of ParseBatch's own closed-set checks refused a
// batch that DID decode.
type ParseReason string

const (
	ParseUnknownKind           ParseReason = "kind"
	ParseUnknownClassification ParseReason = "classification"
	ParseUnknownVerdict        ParseReason = "verdict"
)

// ParseError is a batch that decoded and was refused by a closed-set check.
// It says which, for the retry feedback (feedback.go); its text and chain are
// those of Err, so every message is what it was. Value is the batch's own
// text: UNTRUSTED, never to be rendered except through Token.
type ParseError struct {
	Reason ParseReason
	Effect int // the effect's index, -1 for the batch
	Value  string
	Err    error
}

func (e *ParseError) Error() string { return e.Err.Error() }
func (e *ParseError) Unwrap() error { return e.Err }

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
			return Batch{}, &ParseError{Reason: ParseUnknownKind, Effect: i, Value: string(e.Kind),
				Err: fmt.Errorf("effects: effect %d has unknown kind %q", i, e.Kind)}
		}
	}
	// A diagnosis's classification is checked HERE, at parse, and not left to
	// ValidateDiagnosis alone: like an unknown kind or verdict, an unknown class
	// means the agent believes it said something the contract has no word for,
	// and the batch is unreadable as a diagnosis. ValidateDiagnosis repeats the
	// check for batches built in code.
	for i, e := range b.Effects {
		if e.Kind == KindDiagnosis && !knownClassifications[e.Classification] {
			return Batch{}, &ParseError{Reason: ParseUnknownClassification, Effect: i, Value: string(e.Classification),
				Err: fmt.Errorf("effects: effect %d has unknown classification %q", i, e.Classification)}
		}
	}
	if b.Verdict != "" && !knownVerdicts[b.Verdict] {
		return Batch{}, &ParseError{Reason: ParseUnknownVerdict, Effect: -1, Value: string(b.Verdict),
			Err: fmt.Errorf("effects: unknown verdict %q", b.Verdict)}
	}
	return b, nil
}
