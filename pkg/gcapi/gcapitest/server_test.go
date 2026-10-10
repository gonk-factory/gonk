package gcapitest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/gcapi"
	"github.com/gonk-factory/gonk/pkg/gcapi/gcapitest"
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
// returns the whole output. That divergence is not incidental: a batch pushed
// past the window by a chatty agent is silently unreadable, the bead is judged
// "no batch" and re-slung onto a pricier rung (gonk-u1p.3). A fake that always
// returned everything could not express the bug.
//
// The window's real shape (GASCITY_REF 4fda5a28, corrected in gonk-lv3y): the
// supervisor runs `capture-pane -p -S -<peek_lines>` with no -E, so the reply
// is peek_lines rows of SCROLLBACK plus the whole visible pane; peek_lines is
// floored at the server's own default of 5; and last_output is populated ONLY
// while the session is Running (handler_sessions.go:604). The fake used to
// model a window of exactly peek_lines served in any state, which let tests
// assert truncation upstream cannot perform and a preview upstream would never
// have sent.
func TestFakePeekIsAWindowButTranscriptIsWhole(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")

	// A batch buried under far more chatter than even the pane-sized window
	// admits.
	var b strings.Builder
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, "tool call %d\n", i)
	}
	b.WriteString("GONK_BATCH_START\n{\"effects\":[]}\nGONK_BATCH_END\n")
	// RUNNING, not finished: a peek is a live-terminal read and upstream sends
	// no last_output at all once the session has stopped.
	s.RunSession("alias-1", b.String())

	// The default window is scrollback plus the whole pane (gcapitest.PeekWindowLines), so the head
	// of a 600-line session is gone and the trailing fence survives.
	view, err := c.GetSessionOutput(context.Background(), "alias-1", 0)
	if err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if strings.Contains(view.LastOutput, "tool call 0") {
		t.Fatalf("peek returned the whole output, so it is not modelling a window:\n%s", view.LastOutput)
	}
	if !strings.Contains(view.LastOutput, "GONK_BATCH_START") {
		t.Fatalf("a trailing fence must survive the window -- it is the completion signal:\n%s", view.LastOutput)
	}

	// Asking for FEWER rows than the server's default cannot shrink the reply:
	// the server floors peek_lines at 5 and the capture always runs to the
	// bottom of the pane. A test that believed otherwise was asserting a
	// truncation upstream cannot perform.
	narrow, err := c.GetSessionOutput(context.Background(), "alias-1", 2)
	if err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if narrow.LastOutput != view.LastOutput {
		t.Fatal("a peek_lines below the server default must return the same window")
	}

	// A STOPPED session gets no preview at all, whatever is asked for.
	s.FinishSession("alias-1", b.String())
	stopped, err := c.GetSessionOutput(context.Background(), "alias-1", 0)
	if err != nil {
		t.Fatalf("GetSessionOutput = %v", err)
	}
	if stopped.LastOutput != "" {
		t.Fatalf("a stopped session must carry no preview; upstream only peeks a running one:\n%s", stopped.LastOutput)
	}

	// For a STOPPED session the fake's transcript is the whole output (the
	// conversation path; see handleGetTranscript for why that is not the
	// deployed case, and TestFakeTranscriptOfARunningSessionIsGascitysPaneCapture
	// for what a running session gets). Read through the real client so the fake's
	// SHAPE is exercised too: the response is structured turns, not a flat
	// string, and a fake that invented a convenient shape would pass here and
	// fail in production.
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

// THE FAKE'S WINDOW IS THE PRODUCTION PANE, NOT A COPY OF IT (gonk-z61b).
//
// gcapi.AgentPaneRows is the one copy of the pane height that
// TestPeekCapIsDerivedFromTheAgentPaneGeometry holds against
// images/agent/entrypoint.sh. This pins what the fake actually SERVES to that
// copy, so a fake that goes back to restating the height (it was a literal
// 200, and cmd/gonk-gate's window a literal 205) goes red on the first resize
// instead of quietly modelling the old window.
func TestFakePeekWindowIsTheDriftGuardedPaneGeometry(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")
	total := 3 * (gcapi.AgentPaneRows + 50)
	rows := make([]string, total)
	for i := range rows {
		rows[i] = fmt.Sprintf("row %d", i)
	}
	s.RunSession("alias-1", strings.Join(rows, "\n"))

	for _, asked := range []int{0, 2, gcapi.GascityDefaultPeekLines, 40} {
		scrollback := max(asked, gcapi.GascityDefaultPeekLines)
		want := gcapi.AgentPaneRows + scrollback
		view, err := c.GetSessionOutput(context.Background(), "alias-1", asked)
		if err != nil {
			t.Fatalf("GetSessionOutput(%d) = %v", asked, err)
		}
		if got := len(strings.Split(view.LastOutput, "\n")); got != want {
			t.Fatalf("peek_lines=%d served %d rows, want %d: the pane (gcapi.AgentPaneRows=%d) "+
				"plus %d rows of scrollback", asked, got, want, gcapi.AgentPaneRows, scrollback)
		}
		if got := gcapitest.PeekWindowLines(asked); got != want {
			t.Fatalf("PeekWindowLines(%d) = %d, want %d -- the window tests compute against "+
				"must be the window the fake serves", asked, got, want)
		}
	}
}

// WHAT PRODUCTION SERVES AS A RUNNING SESSION'S TRANSCRIPT (gonk-xflt). Gascity
// resolves no session log for the deployed agent and falls back to a pane
// capture of GascityTranscriptPeekLines rows of scrollback plus the pane, format
// "text", one role "output" turn (GASCITY_REF 4fda5a28
// handler_session_transcript.go:140; every live read is roles "output=1"). A
// fake serving the whole output here let a fix pass that production defeats.
func TestFakeTranscriptOfARunningSessionIsGascitysPaneCapture(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")
	total := gcapi.AgentPaneHistoryLimit + gcapi.AgentPaneRows + 500
	rows := make([]string, total)
	for i := range rows {
		rows[i] = fmt.Sprintf("row %d", i)
	}
	s.RunSession("alias-1", strings.Join(rows, "\n"))

	tr, err := c.GetSessionTranscript(context.Background(), "alias-1")
	if err != nil {
		t.Fatalf("GetSessionTranscript = %v", err)
	}
	if !tr.IsPaneCapture() || len(tr.Turns) != 1 || tr.Turns[0].Role != gcapi.TurnRoleOutput {
		t.Fatalf("format=%q turns=%d: want gascity's text fallback, one output turn", tr.Format, len(tr.Turns))
	}
	want := gcapi.AgentPaneRows + gcapi.GascityTranscriptPeekLines
	if got := len(strings.Split(tr.ModelText(), "\n")); got != want || got != gcapitest.PeekWindowLines(gcapi.GascityTranscriptPeekLines) {
		t.Fatalf("running transcript has %d rows, want %d (pane + %d of scrollback)", got, want, gcapi.GascityTranscriptPeekLines)
	}
}

// THE FAKE'S DEEP CAPTURE IS REAL TMUX'S, MEASURED (gonk-xflt NEW-1). tmux 3.2a,
// a 4000x200 pane, the default history-limit, `seq 1 N` printed and the cursor
// parked on the next (empty) row, then `capture-pane -p -S -10000 | wc -l`.
// A full history does not sit at the limit: tmux drops a tenth of it at once,
// so the rows a capture returns saw with N. The figures are the measurement,
// not a formula, so a fake that drifts from real tmux cannot re-derive its way
// back to green.
func TestFakeCaptureMatchesMeasuredTmux(t *testing.T) {
	if gcapi.AgentPaneRows != 200 || gcapi.AgentPaneHistoryLimit != 2000 {
		t.Skip("measured for a 200-row pane and history-limit 2000; re-measure for this geometry")
	}
	measured := map[int]int{1500: 1501, 2150: 2151, 2200: 2001, 2202: 2003, 2300: 2101,
		2399: 2200, 2401: 2002, 3000: 2001, 5000: 2001}
	for n, want := range measured {
		s := gcapitest.New(t)
		c := s.Client("gonk-city")
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "%d\n", i)
		}
		s.RunSession("alias-1", b.String())
		view, err := c.GetSessionOutput(context.Background(), "alias-1", 10000)
		if err != nil {
			t.Fatalf("GetSessionOutput = %v", err)
		}
		if got := len(strings.Split(view.LastOutput, "\n")); got != want {
			t.Errorf("N=%d: fake captured %d rows, real tmux %d", n, got, want)
		}
	}
	if got := gcapitest.PeekWindowLines(10000); got != 2001 {
		t.Errorf("PeekWindowLines(10000) = %d, want 2001: the least any phase of real tmux returns", got)
	}
	if got := gcapitest.PeekWindowLinesMax(10000); got != 2200 {
		t.Errorf("PeekWindowLinesMax(10000) = %d, want 2200: the most any phase returns", got)
	}
}

// ModelText keeps what the model and its terminal produced and drops the
// prompt: the prompt carries an example fence (gonk-xflt D2).
func TestModelTextDropsTheUserTurn(t *testing.T) {
	tr := &gcapi.SessionTranscript{Turns: []gcapi.TranscriptTurn{
		{Role: gcapi.TurnRoleUser, Text: "PROMPT GONK_BATCH_START {} GONK_BATCH_END"},
		{Role: gcapi.TurnRoleAssistant, Text: "answer"},
		{Role: gcapi.TurnRoleOutput, Text: "pane"},
	}}
	if got := tr.ModelText(); got != "answer\npane" {
		t.Fatalf("ModelText = %q, want only the assistant and output turns", got)
	}
}
