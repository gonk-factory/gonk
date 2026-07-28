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
	// code/merge_request/commit are reserved for Phase 5 and deliberately NOT
	// defined here yet -- naming them is a spec-level forward-compat note, and a
	// constant with no validator/apply would invite half-support.
)

var knownKinds = map[Kind]bool{KindComment: true, KindLabel: true, KindNewIssue: true}

// Effect is one intended externally-visible change. Fields are a superset;
// which are meaningful depends on Kind (validated per-kind elsewhere).
type Effect struct {
	Kind   Kind     `json:"kind"`
	Body   string   `json:"body,omitempty"`   // comment
	Add    []string `json:"add,omitempty"`    // label
	Remove []string `json:"remove,omitempty"` // label
	Title  string   `json:"title,omitempty"`  // new_issue
	// TargetIID names an existing resource this effect acts on (0 = the session's
	// primary target, bound from injected context in shape.go).
	TargetIID int64 `json:"target_iid,omitempty"`
}

// Batch is the ordered list a run returns.
type Batch struct {
	Effects []Effect `json:"effects"`
}

// ParseBatch decodes and structurally validates a batch. It rejects invalid
// JSON and unknown effect kinds -- a run that cannot produce a valid batch
// fails; it never half-applies.
func ParseBatch(raw []byte) (Batch, error) {
	var b Batch
	dec := json.NewDecoder(bytes.NewReader(raw))
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
	return b, nil
}
