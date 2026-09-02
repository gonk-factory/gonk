// Package trace is the trajectory-evidence contract: what a session was
// OBSERVED to do, as opposed to what it produced.
//
// It is pure -- no HTTP, no store, no clock -- so that a future verdict engine
// over these values is as testable as pkg/gate and pkg/verify, the two
// deterministic classifiers this is modelled on. THIS SLICE DEFINES AND
// COLLECTS EVIDENCE ONLY (gonk-p8j). Nothing gates on a Trace until gonk-hsb,
// so it can be observed on real sessions and have its completeness measured
// before it is ever allowed to reject a batch.
//
// WHAT IS DELIBERATELY ABSENT: prompt and response text. Bodies carry untrusted
// issue content and, on cloud rungs, left our premises to begin with, and
// pkg/spend holds none today -- a property worth keeping. A Trace records tool
// NAMES and a normalised argument SHAPE, never a raw argument blob.
package trace

import "sort"

// Completeness says how much of a session we actually saw. IT IS NOT OPTIONAL
// AND IT IS THE MOST IMPORTANT FIELD HERE.
//
// It is the direct analogue of pkg/verify.Evidence.Incomplete and
// pkg/gate.Signals.SpendStale: "we could not observe" must be representable and
// must outrank any judgement built on top of it. Proxy logging can be off, a
// row can be dropped, a callback can fail. A consumer that cannot tell ABSENT
// from "the agent did nothing" converts OUR telemetry outage into SOMEBODY'S
// rejected batch -- and on the triage path a rejected batch re-slings the bead.
//
// This cannot be retrofitted: a consumer written against a plain list of tool
// names has already lost the distinction.
type Completeness string

const (
	// Complete: the collector saw the whole session and says so.
	Complete Completeness = "complete"
	// Partial: some turns were observed and some were known to be missed.
	Partial Completeness = "partial"
	// Absent: nothing was observed. NOT the same as "no tools were called" --
	// that is Complete with an empty Calls.
	Absent Completeness = "absent"
)

// Valid reports whether c is one of the closed set. An unknown completeness
// must never be treated as Complete by default.
func (c Completeness) Valid() bool {
	switch c {
	case Complete, Partial, Absent:
		return true
	}
	return false
}

// Call is one observed tool invocation.
type Call struct {
	// Tool is the tool name as the model named it ("read", "grep", "bash").
	Tool string `json:"tool"`
	// Target is a NORMALISED argument shape, not the raw arguments: a cleaned
	// relative path, or an issue reference. Empty when the call had no target
	// worth normalising, or when normalisation could not be done safely.
	//
	// Normalisation is what keeps this from becoming a transcript store, and it
	// is also what makes the future read-predicate decidable by set arithmetic
	// rather than by judgement.
	Target string `json:"target,omitempty"`
}

// Trace is everything observed for one (bead, attempt).
//
// The join key is the one pkg/atags already owns, so this lands beside spend
// rows rather than inventing a second identity for the same work.
type Trace struct {
	SessionKey string `json:"session_key"`
	Attempt    int    `json:"attempt"`
	BeadID     string `json:"bead_id,omitempty"`
	Project    string `json:"project,omitempty"`

	// Completeness FIRST in meaning if not in field order: read it before
	// reading anything below it.
	Completeness Completeness `json:"completeness"`

	// Calls in the order they were observed. Order is retained because a future
	// predicate may care about sequence ("read before it concluded"), and it is
	// free to keep.
	Calls []Call `json:"calls"`

	// Turns is the number of assistant turns observed, which is not the same as
	// len(Calls): a turn may make several calls or none.
	Turns int `json:"turns"`
}

// ToolCounts is the per-tool call count. Returned as a map because every
// predicate this is meant to serve -- required-tool, forbidden-tool,
// minimum-count -- is set arithmetic over tool names.
func (t Trace) ToolCounts() map[string]int {
	if len(t.Calls) == 0 {
		return map[string]int{}
	}
	m := make(map[string]int, len(t.Calls))
	for _, c := range t.Calls {
		m[c.Tool]++
	}
	return m
}

// Tools is the sorted set of distinct tool names observed. Sorted so callers
// and golden tests get a stable order out of an unordered set.
func (t Trace) Tools() []string {
	m := t.ToolCounts()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Observed reports whether we saw enough to reason about at all. A predicate
// MUST consult this before drawing any conclusion from Calls: an Absent trace
// with no calls means "we do not know", never "it did nothing".
func (t Trace) Observed() bool { return t.Completeness == Complete || t.Completeness == Partial }
