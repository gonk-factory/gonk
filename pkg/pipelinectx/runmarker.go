package pipelinectx

import (
	"fmt"
	"regexp"
	"strings"
)

// The run-note marker (plan D3/D9). gonk-gate (P7) WRITES it on the one note
// per run it keeps on the landing (the MR, or the ref's tracking issue), and
// gonk-intake (P3) READS it to decide whether a run is already handled. Both
// sides use these functions and constants, so the format cannot drift between
// the writer and the reader:
//
//	<!-- gonk:pipeline-run:<anchor> state=<state> -->
//
// Only a marker in a note AUTHORED BY THE BOT counts; enforcing that is the
// reader's job (it knows the bot's user id), not this package's.

// Run-note states, in lifecycle order (D9).
const (
	RunStateDiagnosing = "diagnosing" // dispatch wrote it after the running record
	RunStateDeferred   = "deferred"   // meter deferred; a retry time is in the note
	RunStateDenied     = "denied"     // ladder exhausted / not allowed
	RunStateDiagnosed  = "diagnosed"  // the validated diagnosis landed
	RunStateResolved   = "resolved"   // the target went green
	RunStateSuperseded = "superseded" // a different failing-job set began
)

// RunStates is the closed set, in lifecycle order.
func RunStates() []string {
	return []string{RunStateDiagnosing, RunStateDeferred, RunStateDenied,
		RunStateDiagnosed, RunStateResolved, RunStateSuperseded}
}

// KnownRunState reports whether s is one of RunStates.
func KnownRunState(s string) bool {
	for _, k := range RunStates() {
		if s == k {
			return true
		}
	}
	return false
}

// RunHandled reports whether a run in state s must NOT be diagnosed again.
//
// D3 names diagnosed, denied and resolved. superseded is added: a superseded
// run is closed, and D2 accepts that a flip-flop A -> B -> A "recurs on anchor
// A, which is already diagnosed: no third session" -- but once B began, A's
// note reads superseded, not diagnosed, so leaving superseded out would buy
// exactly the third session D2 rules out. diagnosing and deferred are in
// flight: the anchor dedupes them at meter and gonk-gate, so intake proceeds.
// An unknown state is NOT handled here; callers fail closed on it separately.
func RunHandled(s string) bool {
	switch s {
	case RunStateDiagnosed, RunStateDenied, RunStateResolved, RunStateSuperseded:
		return true
	}
	return false
}

const runMarkerPrefix = "<!-- gonk:pipeline-run:"

// RunMarker renders the marker line for anchor in state. The anchor must be
// Anchor-shaped and the state one of RunStates, so a writer cannot emit a
// marker the reader would misparse.
func RunMarker(anchor, state string) (string, error) {
	if !anchorRe.MatchString(anchor) {
		return "", fmt.Errorf("pipelinectx: %q is not a pipeline anchor", anchor)
	}
	if !KnownRunState(state) {
		return "", fmt.Errorf("pipelinectx: unknown run state %q", state)
	}
	return runMarkerPrefix + anchor + " state=" + state + " -->", nil
}

// runMarkerRe matches any run marker. The state group is deliberately wider
// than the closed set so a marker from a NEWER writer is found and reported
// (and the reader fails closed on it) instead of silently not matching.
var runMarkerRe = regexp.MustCompile(`<!-- gonk:pipeline-run:(\S+) state=([a-z_-]+) -->`)

// RunMarkerStates returns the state of every run marker for exactly anchor in
// body, in order of appearance (normally zero or one). Markers for other
// anchors are ignored. A returned state may be outside RunStates.
func RunMarkerStates(body, anchor string) []string {
	if !strings.Contains(body, runMarkerPrefix) {
		return nil
	}
	var out []string
	for _, m := range runMarkerRe.FindAllStringSubmatch(body, -1) {
		if m[1] == anchor {
			out = append(out, m[2])
		}
	}
	return out
}

// RefIssueMarker is the description marker of the one gonk-owned tracking
// issue per ref (D9): `<!-- gonk:pipeline-ref:<ref> -->`. The reader matches
// it as an exact substring, so the ref needs no escaping to be found.
func RefIssueMarker(ref string) string {
	return "<!-- gonk:pipeline-ref:" + ref + " -->"
}
