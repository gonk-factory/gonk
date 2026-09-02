// Package store is gonk-meter's derived state: project registrations, ladder
// attempt history, budget reservations, and the spend rows read from LiteLLM.
//
// It is DERIVED CACHE, not a source of truth (spec 4.2: "Gonk-side per-project
// state is derived cache only"). The sources of truth are the project's
// .gonk.yml, the operator config, and LiteLLM's spend log. Everything here can
// be rebuilt from them -- with ONE exception, open reservations, which exist
// precisely because the spend log has not caught up yet. Losing them on restart
// is fail-open on headroom for the duration of the spend-log lag; LiteLLM's own
// USD ceiling is the backstop in that window.
//
// Backend: Postgres, on the owner's CNPG cluster (owner decision, 2026-07-14 --
// see docs/spikes/dolt-reservation-isolation.md). Task 0b's spike proved Dolt
// does NOT serialize the concurrent-reservation race under ANY tested strategy
// (default isolation, explicit SERIALIZABLE, and explicit SELECT ... FOR
// UPDATE all let every racing writer win); Postgres's SELECT ... FOR UPDATE
// does, and internal/meter/store/postgres.go is built and tested against a
// real Postgres to prove it, not assume it. Memory is the in-process
// implementation used by every non-integration test.
package store

import (
	"context"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// State is a project's registration state, as reported by GET /v1/projects/{p}.
type State string

const (
	StateActive     State = "active"
	StateDisabled   State = "disabled"    // Effective.Enabled == false
	StateInvalid    State = "invalid"     // .gonk.yml would not load
	StateKeyMissing State = "key-missing" // virtual key not provisioned
)

// KeyRef points at where the project's LiteLLM virtual key lives. It NEVER
// contains the key: a token in an API response ends up in the event bus and in
// every log line that echoes it.
type KeyRef struct {
	SecretName string `json:"secret_name"`
	SecretKey  string `json:"secret_key"`
}

// Registration is a project's registration, as gonk-meter has resolved it.
type Registration struct {
	Project string
	Rig     string
	// Raw is the .gonk.yml we resolved from. It is kept so the reresolve loop
	// can re-run gonkcfg.Resolve when the OPERATOR config changes -- without it,
	// the instance kill switch (ADR-002) would not reach an already-registered
	// project until intake happened to re-register it.
	Raw []byte
	// Effective is meaningful ONLY when State is active or disabled. When State
	// is invalid it is the zero value, which per ADR-002 must never be treated
	// as a resolved config -- that is what InvalidDetail and rung.Input.Invalid
	// are for.
	Effective     gonkcfg.Effective
	InvalidDetail string // the gonkcfg.Load error, when State == StateInvalid
	QuietHours    *rung.QuietHours
	State         State
	KeyRef        KeyRef
	KeyAlias      string
	UpdatedAt     time.Time
}

// Reservation is budget committed to a session that has started but whose spend
// rows have not landed. Remaining budget is always
// ceiling - (observed spend + open reservations).
type Reservation struct {
	ID         string
	Project    string
	BeadID     string
	SessionKey string
	Rung       string
	Attempt    int

	// CostUSD is REAL money held against monthly_cost_usd. It is 0 for a local
	// rung -- which is what lets a monthly_cost_usd: 0 project (the onboarding
	// default) run its local rung at all.
	CostUSD float64
	// SyntheticCostUSD is what LiteLLM's virtual-key counter will actually be
	// charged for a local rung (Decision 9). It is 0 for a cloud rung, whose real
	// price already IS that charge. Held for reporting and for the key-ceiling
	// math; NEVER a policy input.
	SyntheticCostUSD float64

	Tokens    int64
	CreatedAt time.Time
	// ExpiresAt is when this reservation stops holding budget. A reservation
	// holds budget until then whether or not its session has finished.
	ExpiresAt time.Time
	// Settled means a terminal outcome has been recorded (or the janitor has
	// given up on it), so the janitor must not touch it again. It does NOT mean
	// the reservation has stopped holding budget -- see Settle.
	Settled bool
}

// ReserveResult is the outcome of an idempotent check-and-reserve.
//
// Idempotency is keyed on (project, bead_id, session_key) over reservations
// that are still open (settled == false). Because the rung gate is checked in
// TWO places (intake Gate 1, the pack's Gate 2), the same work is /decide'd
// twice before any outcome is reported; the second reserve must return the
// FIRST reservation, not mint a second, or one attempt holds double the
// budget headroom. Postgres enforces this with a partial UNIQUE index and an
// in-transaction check under the same row lock that prevents overspend, so it
// holds ACROSS REPLICAS -- not only within one process.
type ReserveResult struct {
	// Reservation is the reservation that holds budget for
	// (project, bead_id, session_key) after the call: the one just inserted,
	// or the pre-existing open one an earlier call inserted (Existing == true).
	// It is the zero value when Fits is false.
	Reservation Reservation
	// Fits is true when a reservation holds budget for this key after the call,
	// whether newly inserted OR already present. It is false only when the
	// ceiling could not accommodate a NEW reservation (a lost race / genuine
	// exhaustion) -- the caller turns that into a defer.
	Fits bool
	// Existing is true when Reservation is a pre-existing open reservation
	// rather than the r passed in (an idempotent no-op insert). The service
	// returns the same run{} either way.
	Existing bool
}

// Store is gonk-meter's ledger: project registrations, ladder attempt
// history, budget reservations, and deduped spend rows.
type Store interface {
	PutRegistration(ctx context.Context, r Registration) error
	GetRegistration(ctx context.Context, project string) (Registration, bool, error)
	DeleteRegistration(ctx context.Context, project string) error
	ListRegistrations(ctx context.Context) ([]Registration, error)

	// RecordAttempt upserts the attempt produced by ONE reservation, keyed by
	// reservationID. Upsert, not append, for two reasons:
	//
	//  1. Outcome reports are retried. Appending would record the same attempt
	//     twice and buy an escalation the project never earned.
	//  2. A session that outlives reservation_ttl gets an infra-failed attempt
	//     written by the janitor -- and may THEN report its real gate-failed.
	//     The real terminal outcome must SUPERSEDE the janitor's guess, or the
	//     ladder silently stops working for long sessions.
	RecordAttempt(ctx context.Context, project, beadID, reservationID string, a rung.Attempt) error
	// Attempts returns the bead's history in the order the attempts were first
	// recorded.
	Attempts(ctx context.Context, project, beadID string) ([]rung.Attempt, error)

	// ReserveIfFits ATOMICALLY, in ONE step where the DATA lives:
	//
	//  1. IDEMPOTENCY. If an open (settled == false) reservation already exists
	//     for r's (project, bead_id, session_key), returns it -- ReserveResult
	//     {Reservation: existing, Fits: true, Existing: true} -- and inserts
	//     nothing. This is the money-path idempotency of /decide: intake's Gate
	//     1 and the pack's Gate 2 both reserve the same work before any outcome,
	//     and the second must NOT mint a second reservation.
	//  2. Otherwise re-reads observed spend + open reservations, checks r fits
	//     under ceiling, and writes r. Fits == false with no reservation is a
	//     LOST RACE (or genuine exhaustion), NOT an error: the caller defers.
	//
	// *** THIS IS A STORE METHOD, NOT A SERVICE METHOD, AND THAT IS THE POINT. ***
	//
	// Both guarantees -- no overspend AND no double-reserve -- have to be atomic
	// where the DATA lives, or they are correct for exactly one replica and
	// SILENTLY WRONG for two. A service-side "read, decide, write" in an
	// in-process mutex is exactly that trap: the second pod's mutex knows
	// nothing about the first pod's. The Postgres implementation makes both hold
	// ACROSS REPLICAS: overspend via SELECT ... FOR UPDATE on the project lock
	// row, double-reserve via a partial UNIQUE index on
	// (project, bead_id, session_key) WHERE settled = false plus an
	// in-transaction check under that same lock. The service's per-project
	// keyedMutex is a throughput/ordering optimization ONLY; removing it breaks
	// neither property (proven by TestReserveIfFitsRace and
	// TestReserveIsIdempotentAcrossReplicas in postgres_race_test.go, both of
	// which call this method directly with no service mutex in the way).
	//
	// A settled OR expired-and-reclaimed reservation leaves the index (both flip
	// settled = true), so a legitimate re-sling after settle/expiry reserves
	// again.
	ReserveIfFits(ctx context.Context, project string, ceiling budget.Budget, want budget.Spend, r Reservation) (ReserveResult, error)

	// GetReservation is how /v1/policy/outcome binds a caller-supplied outcome
	// to a reservation METER minted. Without it, `outcome: gate-failed` is a
	// forgery vector: repeat it and a project walks itself up to its most
	// expensive rung.
	GetReservation(ctx context.Context, id string) (Reservation, bool, error)

	// Settle marks a reservation's outcome as recorded, and moves its budget
	// hold to holdUntil.
	//
	// It does NOT free the budget immediately, and that is the entire point. A
	// session's spend rows land in LiteLLM's log SECONDS TO MINUTES after the
	// session ends. If the reservation were released the moment the outcome
	// arrived, then for that whole window remaining budget would be computed as
	// `ceiling - observed(not counting this session) - 0` -- over-reporting
	// headroom by the session's entire actual cost, at exactly the moment that
	// cost is maximal and unbilled. That would happen on EVERY successful
	// session, not just on a restart, and on the token side LiteLLM's ceiling is
	// only a loose backstop (Decision 9), so it will not catch a small overshoot.
	//
	// So the service passes holdUntil = now + max_spend_staleness: the hold ends
	// exactly when the spend data is expected to have caught up. If it has not,
	// the spend snapshot is by definition stale and Decide defers anyway -- the
	// two guards meet cleanly with no gap between them.
	Settle(ctx context.Context, id string, holdUntil time.Time) error

	// OpenReservations returns every reservation still holding budget at now --
	// in-flight AND settled-but-still-holding. Expiry is the only thing that
	// frees a hold.
	OpenReservations(ctx context.Context, project string, now time.Time) ([]Reservation, error)
	// ExpireReservations returns the UNSETTLED reservations that have run past
	// their TTL, EXACTLY ONCE each, and marks them settled -- so the janitor
	// records one infra-failed attempt per dead session, not one per tick. A
	// reservation that has already been settled by an outcome report is not
	// returned: its session is not dead, and its hold expires on its own.
	ExpireReservations(ctx context.Context, now time.Time) ([]Reservation, error)

	// AddSpendRows appends rows, dropping any CallID already stored, and reports
	// how many were new. A row whose CostUSD is not finite (NaN or +/-Inf --
	// LiteLLM's spend log is external, unvalidated input) is rejected rather
	// than persisted: Postgres's double precision column will silently corrupt
	// or refuse such a value depending on configuration, and either way it must
	// never reach the budget arithmetic.
	AddSpendRows(ctx context.Context, rows []spend.Row) (int, error)
	SpendRows(ctx context.Context, project string) ([]spend.Row, error)
	AllSpendRows(ctx context.Context) ([]spend.Row, error)

	// SpendCursor is the newest row timestamp the poller has seen; the next poll
	// starts a little before it (overlap is safe: AddSpendRows dedupes).
	SpendCursor(ctx context.Context) (time.Time, error)
	SetSpendCursor(ctx context.Context, t time.Time) error
	// SyncedAt is when the last successful spend sync completed. Its age is what
	// rung.Decide checks against MaxSpendStale.
	SyncedAt(ctx context.Context) (time.Time, error)
	SetSyncedAt(ctx context.Context, t time.Time) error

	// Window is the budget window currently in force. It is persisted so that
	// spend.Advance's monotonicity survives a restart.
	Window(ctx context.Context) (spend.Window, error)
	SetWindow(ctx context.Context, w spend.Window) error

	// --- prompt-by-reference (gonk-mzd) ------------------------------------
	//
	// The agent pod PULLS its prompt instead of having it typed into a TUI.
	// Delivery by keystroke was unreliable -- across five live runs one
	// composer received text, and the pod carried
	// GC_STARTUP_PROMPT_DELIVERED=1 every time, including when the composer was
	// visibly empty. A fetch either returns the prompt or fails loudly, which
	// is the property that channel never had.

	// PutPrompt stores the prompt the session named by alias will fetch. The
	// caller writes it BEFORE creating the session: the pod can be up before
	// CreateSession returns.
	PutPrompt(ctx context.Context, p Prompt) error

	// TakePrompt returns the prompt and marks it consumed, ATOMICALLY. One-shot
	// is the only thing separating "unauthenticated read" from "replayable
	// unauthenticated read", so read-and-mark must be a single statement, the
	// same discipline ReserveIfFits follows -- two pods racing must not both
	// win.
	//
	// found=false means never stored (or expired). consumed=true means it was
	// stored and already taken: the caller answers 410, not 404. The two are
	// opposite diagnoses -- "never delivered" versus "respawn or theft" -- and
	// collapsing them would hide exactly the case worth seeing.
	TakePrompt(ctx context.Context, alias string, now time.Time) (p Prompt, found bool, consumed bool, err error)

	// AppendTrace records observed tool-call evidence for one (session, attempt)
	// -- trajectory slice 1 (gonk-p8j). APPEND, not put: the collector reports
	// incrementally as a session runs, so a later report ADDS calls and turns
	// rather than replacing what was already seen.
	//
	// Completeness is monotonically DOWNWARD: once a collector reports that it
	// missed something, no later report may upgrade the trace back to complete.
	// A gap does not stop being a gap because the next turn was seen.
	AppendTrace(ctx context.Context, t Trace) error

	// GetTrace returns what was observed for one (session, attempt). found=false
	// means NOTHING WAS EVER RECORDED, which the caller must treat as Absent --
	// never as "the agent called no tools".
	GetTrace(ctx context.Context, sessionKey string, attempt int) (t Trace, found bool, err error)

	// PromptStatus reports without consuming, so dispatch can confirm the
	// entrypoint fetched. FetchedAt is zero until it does.
	PromptStatus(ctx context.Context, alias string) (p Prompt, found bool, err error)

	// ExpirePrompts drops rows past ExpiresAt. Prompts hold issue text and want
	// their own retention, so this runs alongside the existing janitor rather
	// than sharing a ledger table's lifetime.
	ExpirePrompts(ctx context.Context, now time.Time) (int, error)
}

// Prompt is one session's rendered prompt, held until that session fetches it.
//
// Model and Metadata travel WITH the prompt so the entrypoint can render its
// opencode overlay per session. That is what restores the attribution seam
// (gonk-m6t): today the overlay is built from pod env that the submit path does
// not set, so spend rows attribute per-install instead of per-bead.
type Prompt struct {
	Alias     string
	Prompt    string
	Model     string
	Metadata  string // the atags JSON, verbatim, for the spend-logs header
	CreatedAt time.Time
	FetchedAt time.Time // zero until taken
	ExpiresAt time.Time
}

// Trace is stored trajectory evidence for one (session, attempt): what a
// session was OBSERVED to do (gonk-p8j).
//
// It holds tool NAMES and normalised argument SHAPE only. No prompt or response
// bodies: they carry untrusted issue text and, on cloud rungs, left our
// premises to begin with, and the ledger holds no bodies today -- a property
// this must not break.
type Trace struct {
	SessionKey string
	Attempt    int
	BeadID     string
	Project    string
	// Completeness is the field every consumer must read first. "absent" and
	// "the agent did nothing" are different facts and must stay different.
	Completeness string
	// Calls is the observed sequence, encoded as JSON by the store layer.
	Calls []TraceCall
	Turns int
	// UpdatedAt is when the collector last reported.
	UpdatedAt time.Time
}

// TraceCall is one observed tool invocation. Target is a NORMALISED shape --
// a cleaned relative path or an issue reference -- never a raw argument blob.
type TraceCall struct {
	Tool   string `json:"tool"`
	Target string `json:"target,omitempty"`
}

// degrade folds two completeness values, keeping the WORSE of the two.
//
// Completeness only ever moves downward. A collector that reports a gap has
// observed a fact about the session that a later, cleaner report does not
// undo: the gap still happened, and a predicate built on top must keep seeing
// it. Anything unrecognised degrades to "absent" rather than being trusted.
func degrade(a, b string) string {
	rank := map[string]int{"complete": 3, "partial": 2, "absent": 1}
	ra, rb := rank[a], rank[b]
	if ra == 0 || rb == 0 {
		return "absent"
	}
	if rb < ra {
		return b
	}
	return a
}
