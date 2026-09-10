// Package beadstore is gonk-gate's per-bead work record: the small amount of
// state the gate needs between a dispatch and its outcome.
//
// It lives BEHIND AN INTERFACE with two implementations (AD-2):
//
//	Memory -- in-memory. EVERY test in cmd/gonk-gate runs against this. No Dolt,
//	          no containers, no `bd` binary.
//	BdCLI  -- shells the MIT `bd` binary against the shared Dolt bead store
//	          (spec 4.2). Its exact subcommands are CONFIRMED AGAINST THE REAL
//	          `bd` in Task 6's container smoke test, not guessed at here.
//
// That split is deliberate: it confines the one thing this plan cannot verify
// offline (bd's CLI surface) to a single file, and leaves the gate's entire
// decision logic testable with `go test`.
package beadstore

import (
	"context"
	"time"
)

type State string

const (
	// StateRunning: a formula was poured; we are waiting for the session to finish.
	StateRunning State = "running"
	// StateParked: meter said `defer` (budget exhausted, quiet hours, capacity).
	// RetryAfter says when to ask again. THIS IS A NORMAL STATE, NOT AN ERROR.
	StateParked State = "parked"
	// StateNeedsHuman: meter said `deny` (ladder exhausted, action not allowed,
	// project not registered). We stop. The sweeper must NEVER pick these up
	// again -- re-sweeping a denied bead is how you spend money on work you
	// already decided not to do.
	StateNeedsHuman State = "needs-human"
	// StateDone: the gate passed.
	StateDone State = "done"
	// StatePendingPrompt: the v2 broker has RESERVED a session alias for this
	// bead+attempt but has not yet confirmed the session running (gonk-6n8,
	// T-13). It exists to close the ≤240s gap between minting the alias and
	// the broker's final StateRunning write, during which a re-dispatch for
	// the same attempt used to see no record at all and mint a second session
	// under the same alias -- see cmd/gonk-gate/broker_inject.go's
	// runBrokerDispatch for the full history (gonk-u6p).
	//
	// INTERIM ONLY. gonk-sweep does not List this state (it Lists StateRunning
	// and StateParked only, see cmd/gonk-gate/sweep.go's runSweep), so a
	// record that never leaves this state is never reclaimed by the sweeper --
	// runBrokerDispatch is responsible for releasing its own reservation
	// (clearing SessionID) on every path that does not reach StateRunning.
	// T-55/T-58 replace this whole mechanism with a deterministic Job name and
	// a 409 at the API server; T-58 deletes this state once that lands.
	StatePendingPrompt State = "pending-prompt"
)

// Record is one work item. BeadAnchor is the IDEMPOTENCY KEY (Plan 02): firing
// the same order twice must not create a second bead, because a second bead is
// a second session and a second session is duplicate spend.
type Record struct {
	BeadAnchor string // "gonk:{project_id}:issue:{iid}" -- the key
	BeadID     string
	Project    string
	ProjectID  int64
	Rig        string
	SessionKey string
	Trigger    string
	IssueIID   int64
	ConfigHash string

	State   State
	Rung    string
	Model   string
	Attempt int
	// ReservationID and ReservationExpiresAt come from METER's DecideResponse,
	// verbatim, on the pour that put this record into StateRunning. The
	// sweeper needs both: ReservationID to bind an OutcomeRequest to the
	// reservation meter actually minted (an unbound outcome is a forgery
	// vector), and ReservationExpiresAt to compute gate.Signals.ReservationExpired
	// without asking meter again.
	ReservationID        string
	ReservationExpiresAt time.Time
	RetryAfter           time.Time
	// SessionEndedAt is stamped by whatever observes the session finish (Gas
	// City's tracking-id lifecycle, out of this plan's available facts -- see
	// ADR/TODO in cmd/gonk-gate/sweep.go). The sweeper only classifies a
	// `running` record once this is non-zero: a zero value means "still
	// running, nothing to do this tick", not "ended at the epoch".
	SessionEndedAt time.Time
	// SessionID is the Gas City session id (from `gc session new`) the broker
	// dispatched for this bead; stamped at inject, read by sweep to locate the
	// returned effects batch. Empty on pre-broker records.
	SessionID string
	UpdatedAt time.Time
}

type Store interface {
	// Put upserts on BeadAnchor.
	Put(ctx context.Context, r Record) error
	Get(ctx context.Context, beadAnchor string) (Record, bool, error)
	List(ctx context.Context, state State) ([]Record, error)
}
