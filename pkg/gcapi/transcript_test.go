package gcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A real triage transcript, now that the prompt tells the model to list
// directories and read code before answering, routinely clears the generic
// 64 KiB API cap. GetSessionTranscript must read it anyway, against its OWN
// dedicated cap -- not the shared one every other supervisor route is held to.
//
// The padding here stands in for exactly that: tool-call/tool-result turns an
// agent produces while exploring a repo, well past 64 KiB, with the actual
// fenced batch arriving last, as the real harness emits it.
func TestGetSessionTranscriptReadsAbove64KiB(t *testing.T) {
	const padTarget = 300 * 1024 // 300 KiB, comfortably over the 64 KiB generic cap
	pad := strings.Repeat("ls -la; cat pkg/gcapi/client.go # exploring the repo\n", padTarget/52+1)
	transcriptText := pad + "\nGONK_BATCH_START\n" +
		`{"effects":[{"kind":"comment","body":"done"}]}` +
		"\nGONK_BATCH_END\n"
	if len(transcriptText) <= 64*1024 {
		t.Fatalf("test setup: transcript is only %d bytes, want > 64 KiB", len(transcriptText))
	}
	if len(transcriptText) >= transcriptMaxResponseBytes {
		t.Fatalf("test setup: transcript is %d bytes, want < the 4 MiB transcript cap", len(transcriptText))
	}

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := json.Marshal(map[string]any{
			"id": "gc-1", "template": "triage", "provider": "open-code", "format": "conversation",
			"turns": []map[string]any{{"role": "assistant", "text": transcriptText}},
		})
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))

	tr, err := c.GetSessionTranscript(context.Background(), "gonk.triage.p1.i1.a0")
	if err != nil {
		t.Fatalf("GetSessionTranscript = %v, want a transcript above the generic 64 KiB cap to still read", err)
	}
	if !strings.Contains(tr.Text(), "GONK_BATCH_START") || !strings.Contains(tr.Text(), `"kind":"comment"`) {
		t.Fatalf("transcript text missing the fenced batch: len=%d", len(tr.Text()))
	}
}

// A transcript above the DEDICATED 4 MiB transcript cap must still be refused
// -- the cap is generous, not unbounded -- and the refusal must be a NAMED
// error a caller can test for with errors.Is (via IsTranscriptTooLarge), not
// merely "some error came back".
func TestGetSessionTranscriptAbove4MiBIsRefusedWithNamedError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"gc-1","template":"triage","provider":"open-code","format":"conversation","turns":[{"role":"assistant","text":"`)
		// 5 MiB of filler, comfortably over the 4 MiB transcriptMaxResponseBytes cap.
		chunk := strings.Repeat("x", 1024)
		for range 5 * 1024 {
			_, _ = fmt.Fprint(w, chunk)
		}
		_, _ = fmt.Fprint(w, `"}]}`)
	}))

	_, err := c.GetSessionTranscript(context.Background(), "gonk.triage.p1.i1.a0")
	if err == nil {
		t.Fatal("want error: a transcript above the 4 MiB dedicated cap must be refused")
	}
	if !IsTranscriptTooLarge(err) {
		t.Fatalf("err = %v, want IsTranscriptTooLarge (a NAMED refusal, not just any error)", err)
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want a diagnosable 'too large' message too", err)
	}
}

// The dedicated 4 MiB transcript cap must NOT have widened the generic 64 KiB
// cap every other supervisor route is held to (client.go's maxResponseBytes).
// GetSessionOutput (the peek/preview read, GetSessionTranscript's closest
// sibling) is the direct check: an oversize peek response must still be
// refused at 64 KiB, exactly as before this change.
func TestGetSessionOutputStillCappedAt64KiB(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"gc-1","state":"idle","last_output":"`)
		chunk := strings.Repeat("x", 1024)
		for range 100 { // 100 KiB, over the 64 KiB generic cap but nowhere near 4 MiB
			_, _ = fmt.Fprint(w, chunk)
		}
		_, _ = fmt.Fprint(w, `"}`)
	}))

	_, err := c.GetSessionOutput(context.Background(), "gonk.triage.p1.i1.a0", 0)
	if err == nil {
		t.Fatal("want error: GetSessionOutput must still enforce the generic 64 KiB cap")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want 'too large'", err)
	}
	// And it must be the SAME sentinel the transcript cap check uses -- proving
	// this is the shared size-cap refusal, not a different failure mode.
	if !IsTranscriptTooLarge(err) {
		t.Fatalf("err = %v, want the shared response-too-large sentinel (errors.Is)", err)
	}
}
