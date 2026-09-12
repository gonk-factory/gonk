// The dispatch DECISION RECORD: exactly one structured log line per inbound
// event, always on, saying what arrived and what was decided about it.
//
// WHY IT EXISTS. On 2026-09-12 the owner replied "@gonk ... you have permission
// to fix it" on project 75 issue !71. GitLab delivered the note event and
// intake answered HTTP 200 -- and then nothing happened, and intake had written
// nothing at all. Diagnosing it took a query against GitLab's own webhook
// delivery log, because the only intake-side evidence was a Prometheus counter
// that nobody was watching per-event. Handle had eleven return paths and logged
// on four of them; the SUCCESS path was silent too, so even "intake fired the
// order" was unobservable.
//
// A 200 with no explanation is the bug. Every event that reaches Handle now
// produces one record, and every non-dispatch carries a reason.
package intake

import (
	"log/slog"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
)

// The decision vocabulary. These strings are what an operator greps for, so
// keep them stable and keep the set closed.
const (
	// DecisionDispatched: an order was fired. This is the only value that
	// means gonk spent something.
	DecisionDispatched = "dispatched"
	// DecisionDeferred: meter said wait (quiet hours, budget). Normal.
	DecisionDeferred = "deferred"
	// DecisionDenied: meter refused. Normal, and a human may need to look.
	DecisionDenied = "denied"
	// DecisionIgnored: intake itself decided there is no work here. ALWAYS
	// carries a reason.
	DecisionIgnored = "ignored"
	// DecisionError: something intake depends on failed. ALWAYS carries a
	// reason, and never means "no work" -- it means "unknown, retried later".
	DecisionError = "error"
)

// noReason is what an empty reason renders as. It is deliberately ugly and
// deliberately grep-able: an ignored event with no reason is a DEFECT in this
// package, and the record says so rather than quietly emitting `reason=""`,
// which reads like a legitimate answer.
const noReason = "BUG_no_reason_recorded"

// decisionRecord accumulates what will be said about one event. Nothing here
// is ever the event's TEXT: a note body and an issue description are untrusted
// user input, and this record goes to a log an operator (and a log shipper)
// reads. Ids, kinds, decisions and reasons only.
type decisionRecord struct {
	Delivery string
	Kind     string
	Project  string
	State    string

	ProjectID int64
	IssueIID  int64
	NoteID    int64
	MRIID     int64

	Decision   string
	Reason     string
	Trigger    string
	SessionKey string
	Bead       string

	extra []any
}

func newDecisionRecord(ev *ghook.Event) *decisionRecord {
	r := &decisionRecord{
		Delivery:  ev.DeliveryID,
		Kind:      string(ev.Kind),
		Project:   ev.Project.PathWithNamespace,
		ProjectID: ev.Project.ID,
		// UNSET IS A BUG, NOT A DEFAULT. If a future return path forgets to set
		// a decision, the record says so instead of reading as a clean ignore.
		Decision: DecisionError,
		Reason:   noReason,
	}
	if ev.Issue != nil {
		r.IssueIID = ev.Issue.IID
	}
	if ev.Note != nil {
		r.NoteID = ev.Note.ID
	}
	if ev.MergeRequest != nil {
		r.MRIID = ev.MergeRequest.IID
	}
	return r
}

func (r *decisionRecord) ignore(reason string) {
	r.Decision, r.Reason = DecisionIgnored, reason
}

func (r *decisionRecord) fail(reason string, err error) {
	r.Decision, r.Reason = DecisionError, reason
	if err != nil {
		r.extra = append(r.extra, "err", err.Error())
	}
}

func (r *decisionRecord) deny(reason string) {
	r.Decision, r.Reason = DecisionDenied, reason
}

func (r *decisionRecord) deferred(reason string, retryAfter time.Time) {
	r.Decision, r.Reason = DecisionDeferred, reason
	r.extra = append(r.extra, "retry_after", retryAfter)
}

// dispatched records the one outcome that spends money. It names the rung, the
// model and the reservation, because those are what the NEXT hop (gonk-gate's
// Gate 2, then gonk-sweep) will report against -- so a reader can follow one
// work item across three processes with a single `grep <bead anchor>`.
func (r *decisionRecord) dispatched(rung, model, reservationID string) {
	r.Decision, r.Reason = DecisionDispatched, ""
	r.extra = append(r.extra, "rung", rung, "model", model, "reservation", reservationID)
}

func (r *decisionRecord) with(kv ...any) { r.extra = append(r.extra, kv...) }

// emit writes the record. A nil logger falls back to slog.Default() rather than
// dropping it: silence is the defect, so the degraded path must still speak.
//
// LEVELS. dispatched is INFO (the normal, wanted outcome). ignored/deferred/
// denied are WARN: each of them is a reason something a human did produced no
// work, which is precisely the question an operator arrives with. error is
// ERROR. Note that a WARN here is NOT necessarily a fault -- "the bot was not
// mentioned" is a perfectly correct ignore -- but it is always an answer to
// "why did nothing happen?", and that is what the level is selecting for.
func (r *decisionRecord) emit(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if r.Decision != DecisionDispatched && r.Reason == "" {
		r.Reason = noReason
	}

	attrs := []any{
		"delivery", r.Delivery,
		"kind", r.Kind,
		"project", r.Project,
		"project_id", r.ProjectID,
		"decision", r.Decision,
	}
	if r.Reason != "" {
		attrs = append(attrs, "reason", r.Reason)
	}
	if r.State != "" {
		attrs = append(attrs, "project_state", r.State)
	}
	if r.IssueIID != 0 {
		attrs = append(attrs, "issue_iid", r.IssueIID)
	}
	if r.NoteID != 0 {
		attrs = append(attrs, "note_id", r.NoteID)
	}
	if r.MRIID != 0 {
		attrs = append(attrs, "mr_iid", r.MRIID)
	}
	if r.Trigger != "" {
		attrs = append(attrs, "trigger", r.Trigger)
	}
	if r.SessionKey != "" {
		attrs = append(attrs, "session_key", r.SessionKey)
	}
	if r.Bead != "" {
		attrs = append(attrs, "bead", r.Bead)
	}
	attrs = append(attrs, r.extra...)

	switch r.Decision {
	case DecisionDispatched:
		log.Info("intake decision", attrs...)
	case DecisionError:
		log.Error("intake decision", attrs...)
	default:
		log.Warn("intake decision", attrs...)
	}
}
