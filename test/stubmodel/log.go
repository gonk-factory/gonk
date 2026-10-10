package stubmodel

import (
	"sync"
	"time"
)

// Call is one request the stub received. This log is the harness's INDEPENDENT
// ground truth for how many model calls happened and what they consumed --
// the first leg of the three-way ledger check in test/ledger. If LiteLLM's spend
// log and this log disagree, a row was lost or double-counted, and that is the
// whole reason this type exists.
type Call struct {
	At       time.Time // wall time, for lag measurement only -- NEVER asserted on
	Model    string
	Metadata map[string]string // present only if LiteLLM forwards metadata upstream
	RawBody  []byte            // the entire request, so Task 5 can discover what LiteLLM sends
	Status   int               // what we served
	Usage    Usage             // what we REPORTED (zero for a failure)
}

type Log struct {
	mu        sync.Mutex
	calls     []Call
	exhausted bool
}

func (l *Log) add(c Call) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
}

func (l *Log) markExhausted() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.exhausted = true
}

// ScriptExhausted reports whether any request ran off the end of the script. A
// suite MUST fail on this: a call the test did not script is a call the test does
// not control, and an uncontrolled call is a nondeterministic cost.
func (l *Log) ScriptExhausted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exhausted
}

func (l *Log) Calls() []Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Call, len(l.calls))
	copy(out, l.calls)
	return out
}

// TotalTokens counts only what was actually REPORTED: a failed call reports no
// usage and therefore costs nothing, which is exactly what LiteLLM will record.
func (l *Log) TotalTokens() int {
	var n int
	for _, c := range l.Calls() {
		n += c.Usage.PromptTokens + c.Usage.CompletionTokens
	}
	return n
}

// CallsWithTag filters by a forwarded metadata tag (see Call.Metadata).
func (l *Log) CallsWithTag(k, v string) []Call {
	var out []Call
	for _, c := range l.Calls() {
		if c.Metadata[k] == v {
			out = append(out, c)
		}
	}
	return out
}

func (l *Log) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls, l.exhausted = nil, false
}
