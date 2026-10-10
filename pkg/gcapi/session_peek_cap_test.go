package gcapi

// gonk-lv3y. The peek read's body size is a function of a parameter GONK
// passes, and it was held to a constant that knew nothing about that
// parameter. Measured live on 2026-09-21 (project 75 issue 79 attempt 1): the
// supervisor answered 200 with a 400-line preview of a 4000-column tmux pane,
// this client refused it at 64 KiB, and because the refusal was
// indistinguishable from an unreachable supervisor the sweep retried it for
// thirteen minutes, the reservation expired, the bead was classified
// infra-failed, re-slung, and denied -- with the session's finished work never
// read. These tests pin the two halves of the fix: the cap follows the
// request, and it is still a hard bound.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// peekBody writes a SessionView whose last_output is n KiB of filler.
func peekBody(t *testing.T, w http.ResponseWriter, kib int) {
	t.Helper()
	_, _ = fmt.Fprint(w, `{"id":"gc-1","state":"stopped","running":false,"last_output":"`)
	chunk := strings.Repeat("x", 1024)
	for range kib {
		_, _ = fmt.Fprint(w, chunk)
	}
	_, _ = fmt.Fprint(w, `"}`)
}

// THE REGRESSION, at the level of the read itself.
//
// 400 lines of a 4000-column pane is up to 1.6 MB of terminal capture, and the
// live failure crossed 64 KiB at an average captured-line length of only 164
// characters -- arithmetically impossible on the 80-column pane brokerPeekLines
// was calibrated against, and routine on the pane the agent image configures
// today (images/agent/entrypoint.sh, gonk-d6h).
//
// 100 KiB is over the generic cap and well inside what peekLines=400 asked
// for, so it must be READ, not refused.
func TestGetSessionOutputReadsPeekLargerThanTheGenericCap(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("peek_lines") != "400" {
			t.Errorf("peek_lines = %q, want 400", r.URL.Query().Get("peek_lines"))
		}
		peekBody(t, w, 100)
	}))

	view, err := c.GetSessionOutput(context.Background(), "gonk.triage.p75.i79.a1", 400)
	if err != nil {
		t.Fatalf("GetSessionOutput: %v -- a 100 KiB preview is well inside what "+
			"peekLines=400 asked for; refusing it is what denied issue 79", err)
	}
	if len(view.LastOutput) != 100*1024 {
		t.Fatalf("last_output = %d bytes, want the whole 100 KiB preview", len(view.LastOutput))
	}
}

// ...AND THE CAP IS STILL A CAP. The trust-boundary reason for maxResponseBytes
// (spec 9) is untouched: this is a bound the caller's own request implies, not
// a bound removed. A supervisor cannot grow it -- only gonk can, by asking for
// more lines -- and past it the read is refused with the shared named sentinel
// so the caller can tell it apart from a network failure.
func TestGetSessionOutputRefusesPeekBeyondItsOwnCap(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peekBody(t, w, 2*1024) // 2 MiB, past the ~848 KiB a 205-row capture allows
	}))

	_, err := c.GetSessionOutput(context.Background(), "gonk.triage.p75.i79.a1", 0)
	if err == nil {
		t.Fatal("want error: the peek cap is proportionate, not absent")
	}
	if !IsResponseTooLarge(err) {
		t.Fatalf("err = %v, want the shared response-too-large sentinel so a caller "+
			"can fall back to GetSessionState instead of failing the judgement", err)
	}
}

// The cap is derived from the two pane constants that actually govern the
// payload, and it MOVES WITH THEM. The whole of gonk-lv3y is that gonk-d6h
// changed one of them (80 -> 4000 columns) and nothing downstream noticed.
func TestSessionPeekCapIsDerivedFromPaneGeometry(t *testing.T) {
	// The capture is scrollback PLUS the whole visible pane, so the default
	// request is not 5 rows, it is 205.
	if got, want := capturedPeekRows(0), AgentPaneRows+GascityDefaultPeekLines; got != want {
		t.Fatalf("capturedPeekRows(0) = %d, want %d: `capture-pane -S -N` has no -E, "+
			"so it returns N rows of scrollback AND the whole visible pane", got, want)
	}
	if got, want := capturedPeekRows(400), AgentPaneRows+400; got != want {
		t.Fatalf("capturedPeekRows(400) = %d, want %d", got, want)
	}
	// A request BELOW the server's own default cannot shrink the reply: the
	// server floors it at 5. A cap that believed otherwise would be too small.
	if capturedPeekRows(1) != capturedPeekRows(0) {
		t.Fatalf("capturedPeekRows(1) = %d, capturedPeekRows(0) = %d: the server floors "+
			"peek_lines at its own default, so asking for fewer buys nothing",
			capturedPeekRows(1), capturedPeekRows(0))
	}
	if got, want := SessionPeekMaxResponseBytes(0),
		int64(sessionViewEnvelopeBytes)+int64(AgentPaneRows+GascityDefaultPeekLines)*(AgentPaneCols+peekRowOverheadBytes); got != want {
		t.Fatalf("cap(0) = %d, want %d", got, want)
	}
	// And it is comfortably above the ~65 KB a real 205-row exploration turn
	// produced live -- the number that sat on the old cap and denied issue 79.
	if got := SessionPeekMaxResponseBytes(0); got <= 65*1024 {
		t.Fatalf("cap(0) = %d, want well above the ~65 KB measured live", got)
	}
}

// THE CONSTANTS MUST NOT DRIFT FROM THE AGENT IMAGE AGAIN.
//
// gonk-d6h widened the pane in images/agent/entrypoint.sh and every size
// assumption downstream silently became wrong; the read kept working for
// months and then stopped, mid-incident, with no test anywhere in this tree
// able to say why. This reads the entrypoint and fails if the geometry the cap
// is derived from is no longer the geometry the agent is given.
func TestPeekCapIsDerivedFromTheAgentPaneGeometry(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "images", "agent", "entrypoint.sh"))
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	m := regexp.MustCompile(`resize-window -t main -x (\d+) -y (\d+)`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("no `resize-window -t main -x <cols> -y <rows>` in images/agent/entrypoint.sh. " +
			"If the agent no longer sizes its pane there, SessionPeekMaxResponseBytes is " +
			"derived from nothing and must be re-derived -- do not delete this test.")
	}
	cols, _ := strconv.Atoi(m[1])
	rows, _ := strconv.Atoi(m[2])
	if cols != AgentPaneCols || rows != AgentPaneRows {
		t.Fatalf("entrypoint.sh sizes the pane %dx%d but the peek cap is derived from %dx%d. "+
			"This exact drift (80 -> 4000 columns, gonk-d6h) is what made the sweep unable "+
			"to read a session at all (gonk-lv3y).", cols, rows, AgentPaneCols, AgentPaneRows)
	}
}

// The wire name. gonk sent peekLines; upstream binds peek_lines
// (GASCITY_REF internal/api/huma_types_sessions.go:43) and huma silently
// ignores unknown query parameters, so the supervisor answered 200 and used
// its own default every time -- brokerPeekLines was inert for the whole life
// of this code and no test could see it, because the fake carried the same
// typo as the client.
func TestGetSessionOutputSendsTheSnakeCaseWireName(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("peek_lines"); got != "25" {
			t.Errorf("peek_lines = %q, want 25 -- upstream binds snake_case and ignores anything else", got)
		}
		if q.Has("peekLines") {
			t.Errorf("sent peekLines: upstream has never read that name (no hits in the pinned clone)")
		}
		_, _ = fmt.Fprint(w, `{"id":"gc-1","state":"running","running":true,"last_output":"hi"}`)
	}))
	if _, err := c.GetSessionOutput(context.Background(), "gonk.triage.p1.i1.a0", 25); err != nil {
		t.Fatalf("GetSessionOutput: %v", err)
	}
}

// GetSessionState asks for no preview at all. That is the whole point of it:
// it is the read that cannot be made too large by the agent's terminal, so it
// is what a caller falls back to when the preview is.
func TestGetSessionStateSendsNoPeekAndReadsTheControlPlaneView(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("peek") != "" || q.Get("peekLines") != "" {
			t.Errorf("query = %q, want no peek parameters at all", r.URL.RawQuery)
		}
		_, _ = fmt.Fprint(w, `{"id":"go-avph","alias":"gonk.triage.p75.i79.a1",`+
			`"template":"triage","state":"stopped","running":false}`)
	}))

	view, err := c.GetSessionState(context.Background(), "gonk.triage.p75.i79.a1")
	if err != nil {
		t.Fatalf("GetSessionState: %v", err)
	}
	if view.State != "stopped" || view.Running {
		t.Fatalf("view = %+v, want the finished/still-working decision intact without a preview", view)
	}
	if view.LastOutput != "" {
		t.Fatalf("last_output = %q, want empty: this read does not ask for one", view.LastOutput)
	}
}

// THE SCROLLBACK FLOOR MUST NOT DRIFT EITHER (gonk-xflt). AgentPaneHistoryLimit
// is tmux's default because nothing in the agent image sets one. If the
// entrypoint or the image starts setting history-limit (the obvious way to
// widen what a fence-scan can see), this constant -- and the deep read sized
// from it -- must follow, or the sweep asks for scrollback that does not exist
// or stops short of scrollback that does.
func TestAgentPaneHistoryLimitIsTmuxDefault(t *testing.T) {
	for _, f := range []string{
		filepath.Join("..", "..", "images", "agent", "entrypoint.sh"),
		filepath.Join("..", "..", "images", "Dockerfile.agent"),
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if m := regexp.MustCompile(`history-limit\s+(\d+)`).FindStringSubmatch(string(b)); m != nil {
			if n, _ := strconv.Atoi(m[1]); n != AgentPaneHistoryLimit {
				t.Fatalf("%s sets tmux history-limit %d but AgentPaneHistoryLimit is %d", f, n, AgentPaneHistoryLimit)
			}
		} else if strings.Contains(string(b), "history-limit") || strings.Contains(string(b), "tmux.conf") {
			t.Fatalf("%s configures tmux scrollback in a form this test cannot read; "+
				"re-derive AgentPaneHistoryLimit from it", f)
		}
	}
	if AgentPaneHistoryLimit != 2000 {
		t.Fatalf("AgentPaneHistoryLimit = %d with no history-limit set anywhere; tmux's default is 2000", AgentPaneHistoryLimit)
	}
}
