package gate

import "strings"

// BeadMarker is the machine-readable token the triage/scaffold/mention agents
// MUST embed in the comment they post. It is how a deterministic gate knows the
// artifact landed WITHOUT an LLM judging prose (spec 6.3: no LLM judges).
//
// IT IS LOAD-BEARING. cmd/gonk-gate's `check` and `sweep` grep for it. If a
// prompt rewrite drops it, the gate goes blind: every successful session is
// classified `gate-failed`, every project climbs its ladder to the most expensive
// rung, and the bill arrives before the bug report. See OD-4.
func BeadMarker(beadID string) string { return "<!-- gonk:bead:" + beadID + " -->" }

// MarkerPresent reports whether body carries THIS bead's marker. An empty beadID
// matches nothing -- a zero value must never satisfy a gate.
func MarkerPresent(body, beadID string) bool {
	if beadID == "" {
		return false
	}
	return strings.Contains(body, BeadMarker(beadID))
}
