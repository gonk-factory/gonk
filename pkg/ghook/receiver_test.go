package ghook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type capture struct {
	events   []*Event
	labels   []string // the (sanitized) event label the observer was handed
	outcomes []Outcome
	full     bool
}

func (c *capture) WebhookOutcome(event string, o Outcome) {
	c.labels = append(c.labels, event)
	c.outcomes = append(c.outcomes, o)
}
func (c *capture) sink(e *Event) bool {
	if c.full {
		return false
	}
	c.events = append(c.events, e)
	return true
}

func newHandler(t *testing.T, c *capture) *Handler {
	t.Helper()
	v, err := NewVerifier(secretA)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(v, NewDeduper(time.Hour, 1024), 7, c.sink, c)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The bot-loop guard protects money: if BotUserID is left at its zero value the
// guard silently no-ops (no real GitLab user is id 0), so gonk would react to
// its own comments and loop. Construction must fail closed, exactly as
// NewVerifier does for the token.
func TestNewHandlerRejectsUnsafeBotID(t *testing.T) {
	v, err := NewVerifier(secretA)
	if err != nil {
		t.Fatal(err)
	}
	sink := func(*Event) bool { return true }
	if _, err := NewHandler(v, NewDeduper(time.Hour, 8), 0, sink, nil); err == nil {
		t.Error("BotUserID 0 must be rejected: the loop guard would silently no-op")
	}
	if _, err := NewHandler(v, NewDeduper(time.Hour, 8), -1, sink, nil); err == nil {
		t.Error("negative BotUserID must be rejected")
	}
	if _, err := NewHandler(v, NewDeduper(time.Hour, 8), 7, sink, nil); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func post(t *testing.T, h *Handler, event, token, ctype, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/hook/gitlab", strings.NewReader(body))
	if token != "" {
		r.Header.Set("X-Gitlab-Token", token)
	}
	r.Header.Set("X-Gitlab-Event", event)
	r.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const issueOpen = `{"object_kind":"issue","project":{"id":42,"path_with_namespace":"g/r","default_branch":"main"},"user":{"id":9,"username":"human"},"object_attributes":{"iid":3,"action":"open","title":"t"}}`

func TestAcceptsGoodRequest(t *testing.T) {
	c := &capture{}
	if got := post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", issueOpen).Code; got != 200 {
		t.Fatalf("code = %d", got)
	}
	if len(c.events) != 1 || c.events[0].Issue.IID != 3 {
		t.Fatalf("events = %+v", c.events)
	}
}

func TestRejectsBadToken(t *testing.T) {
	c := &capture{}
	w := post(t, newHandler(t, c), "Issue Hook", "wrong-but-long-enough-token-aaaaaaaa", "application/json", issueOpen)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	if len(c.events) != 0 {
		t.Fatal("forged request reached the sink")
	}
	if c.outcomes[0] != OutcomeBadToken {
		t.Fatalf("outcome = %q", c.outcomes[0])
	}
}

// An unauthenticated attacker sets X-Gitlab-Event to anything. That header
// becomes a Prometheus label (Task 10), so it must never reach the observer
// verbatim — only a value from a closed set may, or an attacker drives unbounded
// label cardinality until the metrics backend OOMs.
func TestUnauthenticatedEventLabelIsSanitized(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	// No token: this is the pre-authentication path where the header is fully
	// attacker-controlled.
	r := httptest.NewRequest("POST", "/hook/gitlab", nil)
	r.Header.Set("X-Gitlab-Event", "GARBAGE-9999")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	if len(c.labels) != 1 {
		t.Fatalf("observer calls = %d, want 1", len(c.labels))
	}
	if c.labels[0] == "GARBAGE-9999" {
		t.Fatalf("raw attacker-controlled header leaked as a metric label: %q", c.labels[0])
	}
	if c.labels[0] != "other" {
		t.Fatalf("label = %q, want sanitized %q", c.labels[0], "other")
	}
}

// A handled event keeps its own (closed-set) label; only the unknown ones fold
// into "other".
func TestHandledEventLabelPreserved(t *testing.T) {
	c := &capture{}
	post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", issueOpen)
	if len(c.labels) != 1 || c.labels[0] != "Issue Hook" {
		t.Fatalf("labels = %v, want [Issue Hook]", c.labels)
	}
}

// Verification must precede reading the body: a forged request must not be able
// to make us allocate a megabyte.
func TestBodyIsNotReadBeforeVerification(t *testing.T) {
	c := &capture{}
	r := httptest.NewRequest("POST", "/hook/gitlab", &explodingReader{t: t})
	r.Header.Set("X-Gitlab-Event", "Issue Hook")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	newHandler(t, c).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
}

type explodingReader struct{ t *testing.T }

func (e *explodingReader) Read([]byte) (int, error) {
	e.t.Fatal("body was read before the token was verified")
	return 0, nil
}

func TestRejectsMethodAndContentType(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	r := httptest.NewRequest("GET", "/hook/gitlab", nil)
	r.Header.Set("X-Gitlab-Token", secretA)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d", w.Code)
	}
	if got := post(t, h, "Issue Hook", secretA, "text/plain", issueOpen).Code; got != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type code = %d", got)
	}
	// charset suffixes are legal
	if got := post(t, h, "Issue Hook", secretA, "application/json; charset=utf-8", issueOpen).Code; got != 200 {
		t.Fatalf("charset code = %d", got)
	}
}

func TestOversizeBodyRejected(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	big := `{"object_kind":"issue","x":"` + strings.Repeat("a", MaxBodyBytes+1) + `"}`
	if got := post(t, h, "Issue Hook", secretA, "application/json", big).Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", got)
	}
	if len(c.events) != 0 {
		t.Fatal("oversize body reached the sink")
	}
}

// An event we do not handle is not an error: 4xx makes GitLab disable the hook
// after repeated failures, which would permanently break the latency path.
func TestUnhandledEventIsDroppedWith200(t *testing.T) {
	c := &capture{}
	w := post(t, newHandler(t, c), "Pipeline Hook", secretA, "application/json", `{"object_kind":"pipeline"}`)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if len(c.events) != 0 || c.outcomes[0] != OutcomeUnhandledEvent {
		t.Fatalf("events=%d outcome=%q", len(c.events), c.outcomes[0])
	}
}

func TestBotAuthoredEventDropped(t *testing.T) {
	c := &capture{}
	body := strings.Replace(issueOpen, `"user":{"id":9`, `"user":{"id":7`, 1) // bot user id
	post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", body)
	if len(c.events) != 0 {
		t.Fatal("bot-authored event was not suppressed (this is the infinite-loop guard)")
	}
	if c.outcomes[0] != OutcomeBotAuthored {
		t.Fatalf("outcome = %q", c.outcomes[0])
	}
}

// The real loop vector is not an issue event but a note: gonk posts a comment,
// GitLab sends a Note Hook authored by the bot, and gonk must not react. This
// drives the actual note-by-bot fixture through the full receiver and confirms
// the guard keys on the numeric user.id (not a spoofable name).
func TestBotAuthoredNoteDropped(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c) // BotUserID = 7, which is note-by-bot.json's author id
	w := post(t, h, "Note Hook", secretA, "application/json", string(fixture(t, "note-by-bot")))
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if len(c.events) != 0 {
		t.Fatal("bot-authored note reached the sink (loop guard failed on the realistic Note Hook path)")
	}
	if c.outcomes[0] != OutcomeBotAuthored {
		t.Fatalf("outcome = %q, want bot_authored", c.outcomes[0])
	}
}

func TestDuplicateEventDropped(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	for range 2 {
		r := httptest.NewRequest("POST", "/hook/gitlab", strings.NewReader(issueOpen))
		r.Header.Set("X-Gitlab-Token", secretA)
		r.Header.Set("X-Gitlab-Event", "Issue Hook")
		r.Header.Set("X-Gitlab-Event-UUID", "same-uuid")
		r.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if len(c.events) != 1 {
		t.Fatalf("events = %d, want 1 (second delivery is a duplicate)", len(c.events))
	}
}

// A full queue is the ONLY drop reported as a failure, and it must not be a 200.
//
// It was a 200 until 2026-09-07, on the reasoning that "a 5xx only gets the hook
// disabled". Measured against the live instance: a hook with twenty consecutive
// `internal error` deliveries is still alert_status=executable, so this GitLab
// does not auto-disable. GitLab still does not RETRY, so the 503 does not
// recover the event -- the reconciler does that (spec 5.2). What the 503 buys is
// that GitLab's delivery log and gonk's metrics agree about what happened,
// instead of the log recording a discarded event as success.
//
// Contrast the 200s above: duplicate, bot-authored and unhandled-event all mean
// "handled, deliberately". This one means "thrown away".
func TestQueueFullIsReportedAsAFailureNotA200(t *testing.T) {
	c := &capture{full: true}
	w := post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", issueOpen)
	if w.Code == 200 {
		t.Fatal("code = 200: a discarded event must not be recorded in GitLab's delivery log as success")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if c.outcomes[0] != OutcomeQueueFull {
		t.Fatalf("outcome = %q", c.outcomes[0])
	}
}

// The other drops stay 200, because they are not drops of work: nothing was
// thrown away. If this ever fails alongside the test above, someone has changed
// "we discarded your event" and "we handled it" to the same answer again.
func TestHandledOutcomesStay200(t *testing.T) {
	botBody := strings.Replace(issueOpen, `"user":{"id":9`, `"user":{"id":7`, 1)

	for _, tc := range []struct {
		name  string
		event string
		body  string
		want  Outcome
	}{
		{"bot authored", "Issue Hook", botBody, OutcomeBotAuthored},
		{"unhandled event", "Pipeline Hook", issueOpen, OutcomeUnhandledEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &capture{}
			w := post(t, newHandler(t, c), tc.event, secretA, "application/json", tc.body)
			if w.Code != 200 {
				t.Fatalf("code = %d, want 200 for outcome %s", w.Code, tc.want)
			}
			if c.outcomes[0] != tc.want {
				t.Fatalf("outcome = %q, want %q", c.outcomes[0], tc.want)
			}
		})
	}
}
