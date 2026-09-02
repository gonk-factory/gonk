package main

import (
	"strings"
	"testing"
	"time"
)

// The footer exists because the affordance was invisible: gonk answers, the
// reporter replies in the thread, and nothing happens, because intake only
// dispatches on an actual at-mention. Working-and-undiscoverable is, for a
// conversational surface, the same as broken.
func TestMentionFooterNamesTheBot(t *testing.T) {
	f := mentionFooter("gonk")
	if !strings.Contains(f, "@gonk") {
		t.Fatalf("footer must name the bot to mention: %q", f)
	}
	if mentionFooter("") != "" {
		t.Fatal("with no known bot username there is no correct instruction to give, so say nothing")
	}
}

// The deferred notice must say WHY and WHEN, or it is just a shrug.
func TestDeferredBodyExplainsItselfAndSaysWhenItRetries(t *testing.T) {
	at := time.Date(2026, 9, 2, 8, 30, 0, 0, time.UTC)
	body := deferredBody("virtual-key-missing", "LiteLLM virtual key not provisioned yet", at, "gonk")

	if !strings.Contains(body, "budget key") {
		t.Fatalf("a known reason must be explained in words a reporter can act on: %q", body)
	}
	if !strings.Contains(body, "LiteLLM virtual key not provisioned yet") {
		t.Fatal("the machine detail must survive for whoever is debugging it")
	}
	if !strings.Contains(body, "2026-09-02T08:30:00Z") {
		t.Fatalf("must say when it will retry: %q", body)
	}
	if !strings.Contains(body, "@gonk") {
		t.Fatal("the deferred notice should carry the mention affordance too")
	}
}

// An unrecognised reason must produce NO explanatory sentence rather than a
// confident wrong one. The raw reason still travels in the detail line.
func TestUnknownDeferReasonIsNotGuessedAt(t *testing.T) {
	if got := deferReasonText("some-new-reason"); got != "" {
		t.Fatalf("deferReasonText invented an explanation: %q", got)
	}
	body := deferredBody("some-new-reason", "raw detail here", time.Time{}, "gonk")
	if !strings.Contains(body, "raw detail here") {
		t.Fatal("the detail must still reach the reader when the reason is unknown")
	}
	if !strings.Contains(body, "try again automatically") {
		t.Fatalf("with no retry time it must still promise a retry: %q", body)
	}
}

// The status marker must be distinct from the bead marker: one means "this is
// gonk's answer" and the gate reads it, the other means "this is a placeholder
// that may be edited or replaced". Conflating them would let the gate accept a
// placeholder as an answer.
func TestStatusMarkerIsDistinctFromTheBeadMarker(t *testing.T) {
	sm := statusMarker("gonk:7:issue:3")
	if !strings.Contains(sm, "gonk:status:") {
		t.Fatalf("status marker = %q", sm)
	}
	if strings.Contains(sm, "gonk:bead:") {
		t.Fatal("the status marker must not look like the bead marker the gate reads")
	}
}
