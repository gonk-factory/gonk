package main

import "testing"

// An unreadable transcript must resolve to UNKNOWN (an error -> retry), never to
// a gate failure. Scoring it as "no fence" charges the agent for our own failed
// read: it burns the attempt, re-slings onto a pricier rung and can deny the
// bead outright while the agent's completed batch sits unread in the pod. That
// is exactly what happened on issue !42 (gonk-2tb).
func TestEmptyTranscriptIsUnknownNotAGateFailure(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"empty", ""},
		{"whitespace only", "   \n\t\n  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := extractBatch(tc.text); ok {
				t.Fatalf("extractBatch reported a batch in %q", tc.text)
			}
			// The guard the broker applies before ever asking extractBatch.
			if !isUnreadableTranscript(tc.text) {
				t.Fatalf("text %q must be treated as unreadable, not as a silent agent", tc.text)
			}
		})
	}
}

// A transcript that genuinely contains agent output but no fence IS a real gate
// failure and must stay one -- otherwise the retry guard above would swallow
// every honest "the agent did not emit a batch".
func TestNonEmptyTranscriptWithoutAFenceStillFails(t *testing.T) {
	text := "gonk-agent-entrypoint: starting opencode run\n> build - qwen3-14b\nI could not find the file.\n"
	if isUnreadableTranscript(text) {
		t.Fatal("a transcript with real output must not be treated as unreadable")
	}
	if _, ok := extractBatch(text); ok {
		t.Fatal("extractBatch found a batch where there is none")
	}
}
