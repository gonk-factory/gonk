package ghook

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// logRecords captures one JSON record per log line, which is the shape intake
// actually emits in the cluster (cmd/gonk-intake wires a slog JSONHandler).
type logRecords struct {
	b strings.Builder
}

func (l *logRecords) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&l.b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (l *logRecords) all(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func (l *logRecords) text() string { return l.b.String() }

func postUUID(t *testing.T, h *Handler, event, token, uuid, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/hook/gitlab", strings.NewReader(body))
	r.Header.Set("X-Gitlab-Event", event)
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("X-Gitlab-Token", token)
	}
	if uuid != "" {
		r.Header.Set("X-Gitlab-Event-UUID", uuid)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// payload loads a golden webhook body as a string. (event_test.go's fixture
// returns []byte and appends ".json"; this one keeps the full filename visible
// at the call site, which matters when the table below names six of them.)
func payload(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// EVERY delivery produces EXACTLY ONE record. This is the property gonk-ecn
// needed and did not have: a note event was delivered, accepted and 200'd and
// left no trace, so the only evidence it had arrived was in GitLab.
//
// The table walks every Outcome the handler can reach, including the
// pre-authentication ones.
func TestEveryDeliveryProducesExactlyOneRecord(t *testing.T) {
	note := payload(t, "note-mention-issue.json")

	cases := []struct {
		name    string
		outcome Outcome
		drive   func(t *testing.T, h *Handler, lr *logRecords)
	}{
		{"accepted", OutcomeAccepted, func(t *testing.T, h *Handler, _ *logRecords) {
			postUUID(t, h, "Note Hook", secretA, "u-1", note)
		}},
		{"missing_token", OutcomeMissingToken, func(t *testing.T, h *Handler, _ *logRecords) {
			postUUID(t, h, "Note Hook", "", "u-2", note)
		}},
		{"bad_token", OutcomeBadToken, func(t *testing.T, h *Handler, _ *logRecords) {
			postUUID(t, h, "Note Hook", strings.Repeat("z", 40), "u-3", note)
		}},
		{"bad_method", OutcomeBadMethod, func(t *testing.T, h *Handler, _ *logRecords) {
			r := httptest.NewRequest(http.MethodGet, "/hook/gitlab", nil)
			r.Header.Set("X-Gitlab-Event", "Note Hook")
			h.ServeHTTP(httptest.NewRecorder(), r)
		}},
		{"bad_content_type", OutcomeBadContentType, func(t *testing.T, h *Handler, _ *logRecords) {
			r := httptest.NewRequest(http.MethodPost, "/hook/gitlab", strings.NewReader(note))
			r.Header.Set("X-Gitlab-Event", "Note Hook")
			r.Header.Set("X-Gitlab-Token", secretA)
			r.Header.Set("Content-Type", "text/plain")
			h.ServeHTTP(httptest.NewRecorder(), r)
		}},
		{"unhandled_event", OutcomeUnhandledEvent, func(t *testing.T, h *Handler, _ *logRecords) {
			postUUID(t, h, "Pipeline Hook", secretA, "u-4", note)
		}},
		{"malformed", OutcomeMalformed, func(t *testing.T, h *Handler, _ *logRecords) {
			postUUID(t, h, "Note Hook", secretA, "u-5", "{not json")
		}},
		{"duplicate", OutcomeDuplicate, func(t *testing.T, h *Handler, lr *logRecords) {
			postUUID(t, h, "Note Hook", secretA, "u-6", note)
			lr.b.Reset() // keep only the SECOND delivery's record
			postUUID(t, h, "Note Hook", secretA, "u-6", note)
		}},
		{"bot_authored", OutcomeBotAuthored, func(t *testing.T, h *Handler, _ *logRecords) {
			postUUID(t, h, "Note Hook", secretA, "u-7", payload(t, "note-by-bot.json"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &capture{}
			h := newHandler(t, c)
			lr := &logRecords{}
			h.Log = lr.logger()

			tc.drive(t, h, lr)

			recs := lr.all(t)
			if len(recs) != 1 {
				t.Fatalf("got %d records, want exactly 1:\n%s", len(recs), lr.text())
			}
			if got := recs[0]["outcome"]; got != string(tc.outcome) {
				t.Errorf("outcome = %v, want %q\n%s", got, tc.outcome, lr.text())
			}
			if recs[0]["status"] == nil {
				t.Errorf("record carries no status:\n%s", lr.text())
			}
		})
	}
}

// A queue-full drop is the one outcome that LOSES an event, so it must be an
// ERROR and must say so in words a human recognizes.
func TestQueueFullRecordIsAnError(t *testing.T) {
	c := &capture{full: true}
	h := newHandler(t, c)
	lr := &logRecords{}
	h.Log = lr.logger()

	postUUID(t, h, "Note Hook", secretA, "u-full", payload(t, "note-mention-issue.json"))

	recs := lr.all(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1:\n%s", len(recs), lr.text())
	}
	if recs[0]["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR (a dropped event is lost work):\n%s", recs[0]["level"], lr.text())
	}
	if !strings.Contains(lr.text(), "DROPPED") {
		t.Errorf("the record does not say the delivery was dropped:\n%s", lr.text())
	}
}

// THE RECORD MUST NEVER CARRY THE COMMENT TEXT. A note body is untrusted user
// input; ids and decisions are what a log is for.
func TestRecordNeverCarriesUntrustedText(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	lr := &logRecords{}
	h.Log = lr.logger()

	postUUID(t, h, "Note Hook", secretA, "u-8", payload(t, "note-mention-issue.json"))

	if strings.Contains(lr.text(), "please re-triage this") {
		t.Errorf("the note body reached the log:\n%s", lr.text())
	}
	// The ids that make it diagnosable, though, MUST be there.
	for _, want := range []string{`"project_id":42`, `"issue_iid":3`, `"note_id":2001`} {
		if !strings.Contains(lr.text(), want) {
			t.Errorf("record is missing %s:\n%s", want, lr.text())
		}
	}
}

// The delivery id is a correlator taken from an attacker-controlled header. It
// must be bounded and printable, or a forged header can inject a whole extra
// log line (CR/LF) or a terminal escape into an operator's console.
func TestDeliveryIDIsSanitized(t *testing.T) {
	cases := []struct{ in, want string }{
		{"9f2a-ok", "9f2a-ok"},
		{"a\nb", "a?b"},
		{"a\r\nlevel=ERROR msg=forged", "a??level=ERROR msg=forged"},
		{"\x1b[31m", "?[31m"},
		{strings.Repeat("x", 200), strings.Repeat("x", maxDeliveryIDBytes)},
	}
	for _, tc := range cases {
		if got := safeDeliveryID(tc.in); got != tc.want {
			t.Errorf("safeDeliveryID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An accepted event carries its delivery id forward, so the receiver's record
// and the dispatcher's record for the SAME delivery can be joined by grep.
func TestAcceptedEventCarriesTheDeliveryIDForward(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	lr := &logRecords{}
	h.Log = lr.logger()

	postUUID(t, h, "Note Hook", secretA, "delivery-abc", payload(t, "note-mention-issue.json"))

	if len(c.events) != 1 {
		t.Fatalf("got %d sunk events, want 1", len(c.events))
	}
	if c.events[0].DeliveryID != "delivery-abc" {
		t.Errorf("Event.DeliveryID = %q, want %q", c.events[0].DeliveryID, "delivery-abc")
	}
	if !strings.Contains(lr.text(), `"delivery":"delivery-abc"`) {
		t.Errorf("record does not carry the delivery id:\n%s", lr.text())
	}
}

// With no X-Gitlab-Event-UUID (an operator's curl, an older GitLab) the record
// still gets a non-empty correlator, derived the same way the dedupe key is.
func TestDeliveryIDFallsBackWhenGitLabSendsNoUUID(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	lr := &logRecords{}
	h.Log = lr.logger()

	postUUID(t, h, "Note Hook", secretA, "", payload(t, "note-mention-issue.json"))

	if len(c.events) != 1 {
		t.Fatalf("got %d sunk events, want 1", len(c.events))
	}
	if c.events[0].DeliveryID == "" {
		t.Error("DeliveryID is empty; a record with no correlator cannot be joined to the dispatch record")
	}
	if !strings.HasPrefix(c.events[0].DeliveryID, "derived:") {
		t.Errorf("DeliveryID = %q, want the derived dedupe key", c.events[0].DeliveryID)
	}
}

// A nil Log must not mean silence -- silence is the defect. slog.Default() is
// the fallback, and this asserts the handler actually uses it.
func TestNilLoggerFallsBackToTheDefaultRatherThanSilence(t *testing.T) {
	var b strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&b, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := &capture{}
	h := newHandler(t, c) // h.Log deliberately left nil
	postUUID(t, h, "Note Hook", secretA, "u-nil", payload(t, "note-mention-issue.json"))

	if !strings.Contains(b.String(), "webhook delivery") {
		t.Errorf("a handler with no logger wrote nothing to slog.Default():\n%s", b.String())
	}
}
