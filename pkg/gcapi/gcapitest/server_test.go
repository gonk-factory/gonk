package gcapitest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
)

func TestFakeRecordsPoursForAssertion(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")

	res, err := c.RunOrder(context.Background(), "gonk-dispatch", map[string]string{
		"bead_anchor": "gonk:42:issue:3",
		"rung":        "qwen-local",
	})
	if err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	if res.Status != "queued" || res.TrackingID == "" {
		t.Fatalf("res = %+v", res)
	}
	if len(s.Poured) != 1 {
		t.Fatalf("Poured = %+v, want 1 entry", s.Poured)
	}
	got := s.Poured[0]
	if got.Order != "gonk-dispatch" || got.Vars["bead_anchor"] != "gonk:42:issue:3" || got.Vars["rung"] != "qwen-local" {
		t.Fatalf("Poured[0] = %+v", got)
	}
	if names := s.PouredNames(); len(names) != 1 || names[0] != "gonk-dispatch" {
		t.Fatalf("PouredNames = %v", names)
	}
}

// A second Pour is a second bead/session slot burned. A test driving Task 3's
// re-sling path needs to see exactly how many times, and with what vars, each
// named order was actually poured -- append-only, in order.
func TestFakeRecordsMultiplePoursInOrder(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")
	ctx := context.Background()

	if _, err := c.RunOrder(ctx, "gonk-dispatch", map[string]string{"attempt": "1"}); err != nil {
		t.Fatalf("RunOrder 1 = %v", err)
	}
	if _, err := c.RunOrder(ctx, "gonk-dispatch", map[string]string{"attempt": "2"}); err != nil {
		t.Fatalf("RunOrder 2 = %v", err)
	}
	if _, err := c.RunOrder(ctx, "gonk-scaffold", nil); err != nil {
		t.Fatalf("RunOrder 3 = %v", err)
	}
	names := s.PouredNames()
	if len(names) != 3 || names[0] != "gonk-dispatch" || names[1] != "gonk-dispatch" || names[2] != "gonk-scaffold" {
		t.Fatalf("PouredNames = %v", names)
	}
	if s.Poured[0].Vars["attempt"] != "1" || s.Poured[1].Vars["attempt"] != "2" {
		t.Fatalf("Poured = %+v", s.Poured)
	}
}

// Fail lets a test drive the real client's retry path (Task 3's dispatch
// tests need to prove a transient supervisor 5xx does not abort a poured
// order) without a second, hand-rolled httptest.Server per test.
func TestFakeEmitsConfiguredFailuresThenSucceeds(t *testing.T) {
	s := gcapitest.New(t)
	s.Fail = map[string]int{"gonk-dispatch": 2}
	c := s.Client("gonk-city")

	if _, err := c.RunOrder(context.Background(), "gonk-dispatch", nil); err != nil {
		t.Fatalf("RunOrder should have retried through the failures: %v", err)
	}
	if len(s.Poured) != 1 {
		t.Fatalf("Poured = %+v, want exactly 1 (the failed attempts must not be recorded as pours)", s.Poured)
	}
	if s.Fail["gonk-dispatch"] != 0 {
		t.Fatalf("Fail[gonk-dispatch] = %d, want 0 (fully consumed)", s.Fail["gonk-dispatch"])
	}
}

// Client must return a real *gcapi.Client wired to this fake, not a stub --
// callers (Task 3's dispatch code) construct it exactly like they would the
// real supervisor client.
func TestFakeClientIsWiredToTheFake(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")
	if c.City != "gonk-city" || c.BaseURL != s.URL() {
		t.Fatalf("client = %+v, want City=gonk-city BaseURL=%s", c, s.URL())
	}
	if _, err := gcapi.New(s.URL(), "gonk-city").RunOrder(context.Background(), "gonk-dispatch", nil); err != nil {
		t.Fatalf("a client built directly against the fake's URL should work identically: %v", err)
	}
}

// The fake models peek as a bounded PREVIEW WINDOW while the transcript route
// returns the whole output. That divergence is not incidental: production reads
// the effects batch out of a 400-line peek (gonk-u1p.3), so a batch pushed past
// the window by a chatty agent is silently unreadable -- the bead is judged "no
// batch" and re-slung onto a pricier rung. A fake that always returned
// everything could not express the bug, so it could never be tested.
func TestFakePeekIsAWindowButTranscriptIsWhole(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")

	// A batch buried under far more chatter than a small peek window admits.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "tool call %d\n", i)
	}
	b.WriteString("GONK_BATCH_START\n{\"effects\":[]}\nGONK_BATCH_END\n")
	s.FinishSession("alias-1", b.String())

	// A window smaller than the chatter still shows the TAIL, so a batch at the
	// very end survives...
	view, err := c.GetSessionOutput(context.Background(), "alias-1", 5)
	if err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if strings.Contains(view.LastOutput, "tool call 0") {
		t.Fatalf("peek returned the whole output, so it is not modelling a window:\n%s", view.LastOutput)
	}
	if !strings.Contains(view.LastOutput, "GONK_BATCH_START") {
		t.Fatalf("a trailing batch should survive a tail window:\n%s", view.LastOutput)
	}

	// ...but a window of 2 lines cuts into the fence itself, which is exactly
	// the silent-truncation failure mode.
	view, err = c.GetSessionOutput(context.Background(), "alias-1", 2)
	if err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if strings.Contains(view.LastOutput, "GONK_BATCH_START") {
		t.Fatalf("a 2-line window must not contain the whole fence:\n%s", view.LastOutput)
	}

	// The transcript route is the output of record: untruncated, fence intact.
	// Read through the real client so the fake's SHAPE is exercised too -- the
	// response is structured turns, not a flat string, and a fake that invented
	// a convenient shape would pass here and fail in production.
	tr, err := c.GetSessionTranscript(context.Background(), "alias-1")
	if err != nil {
		t.Fatalf("GetSessionTranscript = %v", err)
	}
	if len(tr.Turns) == 0 {
		t.Fatal("transcript returned no turns")
	}
	whole := tr.Text()
	if !strings.Contains(whole, "tool call 0") || !strings.Contains(whole, "GONK_BATCH_END") {
		t.Fatalf("transcript should be whole:\n%s", whole)
	}
}

// A running session is not judgeable and must be distinguishable from one that
// finished with no output -- the distinction a flat output map could not make.
func TestFakeDistinguishesRunningFromFinished(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")

	s.RunSession("alias-1", "thinking...\n")
	view, err := c.GetSessionOutput(context.Background(), "alias-1", 0)
	if err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if !view.Running || view.State != string(gcapitest.SessionRunning) {
		t.Fatalf("running session read as running=%v state=%q", view.Running, view.State)
	}

	s.FinishSession("alias-1", "done\n")
	view, err = c.GetSessionOutput(context.Background(), "alias-1", 0)
	if err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if view.Running || view.State != string(gcapitest.SessionStopped) {
		t.Fatalf("stopped session read as running=%v state=%q", view.Running, view.State)
	}
}

// tail=0 means "all segments" upstream; omitting it returns only the most
// recent. GetSessionTranscript must send tail=0 -- a caller that forgets it
// reads a fragment and believes it read everything, which is the same silent
// truncation the peek window causes, one layer up.
func TestFakeTranscriptRequiresTailZeroForEverything(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")
	s.FinishSession("alias-1", "line one\nline two\nGONK_BATCH_START\n{}\nGONK_BATCH_END")

	// The client asks for tail=0 and therefore sees all of it.
	tr, err := c.GetSessionTranscript(context.Background(), "alias-1")
	if err != nil {
		t.Fatalf("GetSessionTranscript = %v", err)
	}
	if !strings.Contains(tr.Text(), "line one") {
		t.Fatalf("client did not request every segment (tail=0):\n%s", tr.Text())
	}

	// Without it, the fake returns only the last segment -- proving the fake
	// actually distinguishes the two, so the assertion above has teeth.
	body := s.GetJSON(t, "/v0/city/gonk-city/session/alias-1/transcript")
	turns, _ := body["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("turns = %v, want one", turns)
	}
	first, _ := turns[0].(map[string]any)
	text, _ := first["text"].(string)
	if strings.Contains(text, "line one") {
		t.Fatalf("a tail-less read should NOT return everything:\n%s", text)
	}
}
