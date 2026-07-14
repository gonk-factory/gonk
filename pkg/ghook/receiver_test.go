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
	outcomes []Outcome
	full     bool
}

func (c *capture) WebhookOutcome(_ string, o Outcome) { c.outcomes = append(c.outcomes, o) }
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
	return &Handler{Verifier: v, Deduper: NewDeduper(time.Hour, 1024), BotUserID: 7, Sink: c.sink, Obs: c}
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

// Queue full is a drop, not a 5xx: GitLab does not retry webhooks, and a 5xx
// only gets the hook disabled. Reconciliation is the correctness path (spec 5.2).
func TestQueueFullDropsWith200(t *testing.T) {
	c := &capture{full: true}
	w := post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", issueOpen)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if c.outcomes[0] != OutcomeQueueFull {
		t.Fatalf("outcome = %q", c.outcomes[0])
	}
}
