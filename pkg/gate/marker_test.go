package gate

import "testing"

func TestMarkerRoundTrip(t *testing.T) {
	m := BeadMarker("gk-1a2b")
	if m != "<!-- gonk:bead:gk-1a2b -->" {
		t.Fatalf("marker = %q", m)
	}
	body := "I looked at this issue and here is my analysis.\n\n" + m + "\n"
	if !MarkerPresent(body, "gk-1a2b") {
		t.Fatal("marker not found in a comment that carries it")
	}
	// A DIFFERENT bead's marker must not satisfy this bead's gate: otherwise one
	// successful triage would mark every other bead on the issue as done.
	if MarkerPresent(body, "gk-9999") {
		t.Fatal("a foreign bead marker satisfied the gate")
	}
	// A human quoting gonk's comment must not satisfy the gate either -- the gate
	// asks "did the BOT post it", and the caller filters by author, but belt and
	// braces: an empty bead id matches nothing.
	if MarkerPresent(body, "") {
		t.Fatal("empty bead id must never match")
	}
}
