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

	// ReserveIfFits ATOMICALLY re-reads the project's observed spend and open
	// reservations, checks that r still fits under ceiling, and writes r -- or
	// reports that it does not fit. It is the ONLY way a reservation may be
	// created.
	//
	// *** THIS IS A STORE METHOD, NOT A SERVICE METHOD, AND THAT IS THE POINT. ***
	//
	// The check and the write have to be one atomic step, and the atomicity has
	// to live where the DATA lives. A service-side "read totals, decide, write
	// reservation" sandwiched in an in-process mutex is correct for exactly one
	// replica and SILENTLY WRONG for two: the second pod's mutex knows nothing
	// about the first pod's, both see the same headroom, and both spend it.
	//
	// The service's per-project keyedMutex stays -- it is cheap contention
	// control and it keeps Decide deterministic within one process -- but it is
	// NOT what makes the ceiling safe. This method is. The Postgres
	// implementation enforces it with SELECT ... FOR UPDATE inside a
	// transaction (verified against a real Postgres server by
	// TestReserveIfFitsRace in postgres_race_test.go; see also Task 0b's spike,
	// which measured the SAME shape failing under Dolt).
	//
	// Returning (false, nil) is a LOST RACE, not an error: the caller turns it
	// into a defer.
	ReserveIfFits(ctx context.Context, project string, ceiling budget.Budget, want budget.Spend, r Reservation) (bool, error)

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
}
