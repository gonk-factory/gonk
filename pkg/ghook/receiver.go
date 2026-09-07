package ghook

import (
	"errors"
	"fmt"
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

// NewHandler is the sanctioned way to build a Handler: it fails closed on a
// configuration that would disable a safety property. In particular BotUserID
// must be a positive GitLab user id — at its zero value the loop guard silently
// no-ops (no real user is id 0), gonk reacts to its own comments, and spend runs
// away. This mirrors NewVerifier's refusal to accept a weak token.
func NewHandler(v *Verifier, d *Deduper, botUserID int64, sink func(*Event) bool, obs Observer) (*Handler, error) {
	if v == nil {
		return nil, errors.New("ghook: nil verifier")
	}
	if d == nil {
		return nil, errors.New("ghook: nil deduper")
	}
	if botUserID <= 0 {
		return nil, fmt.Errorf("ghook: BotUserID must be a positive GitLab user id, got %d (the loop guard cannot be disabled)", botUserID)
	}
	if sink == nil {
		return nil, errors.New("ghook: nil sink")
	}
	if obs == nil {
		obs = NopObserver{}
	}
	return &Handler{Verifier: v, Deduper: d, BotUserID: botUserID, Sink: sink, Obs: obs}, nil
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
		// A full queue is the one drop we report as a FAILURE. Every other 200
		// above means "handled": a duplicate really was handled, a bot comment
		// really should be ignored. This one means we threw the event away.
		//
		// This used to be a 200, on the reasoning that "GitLab does not retry
		// webhooks, and a 5xx only gets the hook disabled".
		//
		// THE AUTO-DISABLE HALF IS UNPROVEN, and I over-claimed it. On
		// 2026-09-07 project 75's stale hook 3 had twenty consecutive
		// `internal error` deliveries and still read alert_status=executable,
		// which looked like proof that this instance never disables. It is not:
		// that hook has since been deleted, no failing hook exists anywhere on
		// the instance to re-check, and GitLab 18.10.1 CE documents project
		// hooks failing four consecutive times as temporarily_disabled with
		// backoff. Treat "it will not be disabled" as unverified.
		//
		// It does not change the answer, for two reasons that do hold. A
		// temporary disable self-recovers. And reaching this line at all means
		// sustaining a flood past a 256-deep channel drained by a dedicated
		// goroutine -- if that is happening, a quiet 200 is the worse outcome.
		//
		// The rest stands and is the real answer: GitLab does not retry,
		// so a 503 does not get the event back either. What it buys is HONESTY
		// -- the drop shows up in GitLab's own delivery log instead of being
		// recorded there as success, so the two systems agree about what
		// happened. Recovery is the reconciler's job, per spec 5.2:
		// "Reconciliation is the correctness path; webhooks are the latency
		// optimization."
		h.finish(w, event, OutcomeQueueFull, http.StatusServiceUnavailable)
		return
	}
	h.finish(w, event, OutcomeAccepted, http.StatusOK)
}

func (h *Handler) finish(w http.ResponseWriter, event string, o Outcome, code int) {
	if h.Obs != nil {
		// The raw X-Gitlab-Event header is attacker-controlled on every path,
		// including the pre-authentication ones (bad_method, bad/missing_token).
		// It must never reach the observer verbatim, or an unauthenticated flood
		// of random header values explodes Prometheus label cardinality. Collapse
		// anything outside the handled allow-list to a single fixed label.
		h.Obs.WebhookOutcome(eventLabel(event), o)
	}
	w.WriteHeader(code)
	// Body is deliberately terse: it is an error channel to an attacker.
	_, _ = io.WriteString(w, string(o)+"\n")
}

// eventLabel maps the attacker-controlled X-Gitlab-Event header onto a closed
// set of metric label values: the events we actually handle, or "other".
func eventLabel(event string) string {
	if handledEvents[event] {
		return event
	}
	return "other"
}

func isJSON(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.TrimSpace(mt) == "application/json"
}
