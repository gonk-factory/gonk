// Package stubmodel is a deterministic, OpenAI-compatible model server. It is the
// foundation of gonk's e2e determinism (spec goal 7): every token count, every
// failure, and every tool call in a test comes from a script, not from a model.
//
// NOTHING IN THE GONK TEST SUITE MAY TALK TO A REAL MODEL. Determinism is the
// product here; "the model said something reasonable" is not an assertion.
package stubmodel

import "time"

// Forever makes a step serve every matching request for the rest of the script.
const Forever = -1

// Usage is what the stub REPORTS to LiteLLM. LiteLLM multiplies it by the price
// configured for the model, and that product is the row in the spend ledger. So
// this struct is, transitively, how a test decides what a thing costs.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string, as OpenAI sends it
}

// Response is the assistant turn. Content with no ToolCalls finishes "stop";
// ToolCalls finish "tool_calls".
//
// The distinction is load-bearing: a canned tool call is how a test forces the
// outcome gate to PASS, and a canned prose-only reply (the agent did nothing) is
// how it forces a GENUINE gate failure -- as opposed to an infra failure, which
// is Status/Hang below. The escalation ladder turns on exactly that difference.
type Response struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Match selects which requests a Step applies to. A zero Match matches anything.
type Match struct {
	Model    string `json:"model,omitempty"`    // exact model name as LiteLLM forwards it
	Contains string `json:"contains,omitempty"` // substring of the last user message
	Tag      string `json:"tag,omitempty"`      // "key=value" in the request's metadata, IF LiteLLM forwards it
}

// Step is one scripted behaviour.
type Step struct {
	Match Match `json:"match"`

	// Exactly one behaviour. Precedence: Hang > Status != 0 > RawBody != "" > Response.
	Hang     bool          `json:"hang,omitempty"`     // never respond; the client must time out
	Status   int           `json:"status,omitempty"`   // non-2xx; an INFRA failure
	RawBody  string        `json:"raw_body,omitempty"` // malformed/garbage body, served with 200
	Response Response      `json:"response,omitempty"`
	Usage    Usage         `json:"usage,omitempty"`
	Delay    time.Duration `json:"delay,omitempty"` // slow-drip; use with a client timeout

	// Repeat: 0 or 1 => serve once. Forever (-1) => serve every matching request.
	Repeat int `json:"repeat,omitempty"`

	served int // internal
}

func (s *Step) exhausted() bool {
	if s.Repeat == Forever {
		return false
	}
	n := s.Repeat
	if n == 0 {
		n = 1
	}
	return s.served >= n
}
