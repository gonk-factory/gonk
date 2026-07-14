package ghook

import (
	"errors"
	"io"
	"net/http"
	"strings"
)

// MaxBodyBytes caps a webhook payload. Issue/note/MR payloads are a few KiB;
// 1 MiB is generous and still bounds what an unauthenticated flood can make us
// allocate (bodies are only read after the token verifies).
const MaxBodyBytes = 1 << 20

// Outcome is why a delivery ended the way it did. These strings are metric label
// values and dashboard keys: keep them stable.
type Outcome string

const (
	OutcomeAccepted       Outcome = "accepted"
	OutcomeMissingToken   Outcome = "missing_token"
	OutcomeBadToken       Outcome = "bad_token"
	OutcomeBadMethod      Outcome = "bad_method"
	OutcomeBadContentType Outcome = "bad_content_type"
	OutcomeTooLarge       Outcome = "too_large"
	OutcomeMalformed      Outcome = "malformed"
	OutcomeUnhandledEvent Outcome = "unhandled_event"
	OutcomeDuplicate      Outcome = "duplicate"
	OutcomeBotAuthored    Outcome = "bot_authored"
	OutcomeQueueFull      Outcome = "queue_full"
)

// Observer receives one call per delivery. The Prometheus implementation lives
// in pkg/intake (Task 10); ghook stays dependency-free.
type Observer interface {
	WebhookOutcome(event string, o Outcome)
}

type NopObserver struct{}

func (NopObserver) WebhookOutcome(string, Outcome) {}

// handledEvents is the X-Gitlab-Event allow-list. Anything else is dropped with
// a 200 (see TestUnhandledEventIsDroppedWith200).
var handledEvents = map[string]bool{
	"Issue Hook":         true,
	"Note Hook":          true,
	"Merge Request Hook": true,
}

// Handler is the /hook/gitlab endpoint: the only path gonk exposes to the
// ingress (spec 9).
type Handler struct {
	Verifier  *Verifier
	Deduper   *Deduper
	BotUserID int64
	// Sink hands the event to the dispatcher. It must not block or do IO; it
	// returns false when its queue is full, which is a drop, not an error.
	Sink func(*Event) bool
	Obs  Observer
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-Gitlab-Event")

	if r.Method != http.MethodPost {
		h.finish(w, event, OutcomeBadMethod, http.StatusMethodNotAllowed)
		return
	}
	// Verify BEFORE touching the body. A forged request must cost us nothing.
	if err := h.Verifier.Verify(r); err != nil {
		out := OutcomeBadToken
		if errors.Is(err, ErrMissingToken) {
			out = OutcomeMissingToken
		}
		h.finish(w, event, out, http.StatusUnauthorized)
		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		h.finish(w, event, OutcomeBadContentType, http.StatusUnsupportedMediaType)
		return
	}
	if !handledEvents[event] {
		h.finish(w, event, OutcomeUnhandledEvent, http.StatusOK)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.finish(w, event, OutcomeTooLarge, http.StatusRequestEntityTooLarge)
			return
		}
		h.finish(w, event, OutcomeMalformed, http.StatusBadRequest)
		return
	}

	ev, err := ParseEvent(event, body)
	if err != nil {
		h.finish(w, event, OutcomeMalformed, http.StatusBadRequest)
		return
	}
	if h.Deduper.Seen(DedupeKey(r.Header.Get("X-Gitlab-Event-UUID"), ev)) {
		h.finish(w, event, OutcomeDuplicate, http.StatusOK)
		return
	}
	// Loop guard (spec 4.3 step 2): the bot's own comments must never trigger
	// the bot.
	if ev.User.ID == h.BotUserID {
		h.finish(w, event, OutcomeBotAuthored, http.StatusOK)
		return
	}
	if !h.Sink(ev) {
		h.finish(w, event, OutcomeQueueFull, http.StatusOK)
		return
	}
	h.finish(w, event, OutcomeAccepted, http.StatusOK)
}

func (h *Handler) finish(w http.ResponseWriter, event string, o Outcome, code int) {
	if h.Obs != nil {
		h.Obs.WebhookOutcome(event, o)
	}
	w.WriteHeader(code)
	// Body is deliberately terse: it is an error channel to an attacker.
	_, _ = io.WriteString(w, string(o)+"\n")
}

func isJSON(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.TrimSpace(mt) == "application/json"
}
