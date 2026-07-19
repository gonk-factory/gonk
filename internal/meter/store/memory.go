package store

import (
	"context"
	"sync"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// Memory is the in-memory Store. It backs every non-integration test; the
// SHIPPING store is Postgres (internal/meter/store/postgres.go).
//
// attemptRecord ties an attempt to the reservation that produced it, so a
// retried or late outcome report supersedes rather than duplicates.
type attemptRecord struct {
	reservationID string
	Attempt       rung.Attempt
}

type Memory struct {
	mu           sync.RWMutex
	regs         map[string]Registration
	attempts     map[string][]attemptRecord // key: project + "\x00" + beadID
	reservations map[string]Reservation
	rows         []spend.Row
	seen         map[string]struct{}
	cursor       time.Time
	syncedAt     time.Time
	window       spend.Window
}

func NewMemory() *Memory {
	return &Memory{
		regs:         map[string]Registration{},
		attempts:     map[string][]attemptRecord{},
		reservations: map[string]Reservation{},
		seen:         map[string]struct{}{},
	}
}

func beadKey(project, beadID string) string { return project + "\x00" + beadID }

func (m *Memory) PutRegistration(_ context.Context, r Registration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.regs[r.Project] = r
	return nil
}

func (m *Memory) GetRegistration(_ context.Context, project string) (Registration, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.regs[project]
	return r, ok, nil
}

func (m *Memory) DeleteRegistration(_ context.Context, project string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.regs, project)
	return nil
}

func (m *Memory) ListRegistrations(_ context.Context) ([]Registration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Registration, 0, len(m.regs))
	for _, r := range m.regs {
		out = append(out, r)
	}
	return out, nil
}

// RecordAttempt upserts by reservation ID. A retried outcome report overwrites
// rather than appends; a real terminal outcome overwrites the janitor's
// infra-failed for the same reservation.
func (m *Memory) RecordAttempt(_ context.Context, project, beadID, reservationID string, a rung.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := beadKey(project, beadID)
	for i, rec := range m.attempts[k] {
		if rec.reservationID == reservationID {
			m.attempts[k][i].Attempt = a // supersede in place, keeping the order
			return nil
		}
	}
	m.attempts[k] = append(m.attempts[k], attemptRecord{reservationID: reservationID, Attempt: a})
	return nil
}

func (m *Memory) Attempts(_ context.Context, project, beadID string) ([]rung.Attempt, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.attempts[beadKey(project, beadID)]
	out := make([]rung.Attempt, len(src)) // copy: history is not the caller's to edit
	for i, rec := range src {
		out[i] = rec.Attempt
	}
	return out, nil
}

// ReserveIfFits: idempotent check-and-write under ONE lock. In the memory
// store this is trivially atomic; in the Postgres store it is a transaction
// with SELECT ... FOR UPDATE plus a partial unique index. Both must behave
// identically -- that is what storetest exists for.
func (m *Memory) ReserveIfFits(_ context.Context, project string, ceiling budget.Budget, want budget.Spend, r Reservation) (ReserveResult, error) {
	if err := validateFiniteMoney(r.CostUSD, r.SyntheticCostUSD); err != nil {
		return ReserveResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// IDEMPOTENCY, mirroring Postgres's partial UNIQUE index on
	// (project, bead_id, session_key) WHERE settled = false: an existing OPEN
	// (unsettled) reservation for the same key means this exact work was already
	// reserved. Return it, insert nothing -- otherwise one attempt would hold
	// double the headroom. Settled (outcome-reported OR janitor-reclaimed)
	// reservations are OUT of this check, so a legitimate re-sling reserves anew.
	for _, o := range m.reservations {
		if o.Project == project && o.BeadID == r.BeadID && o.SessionKey == r.SessionKey && !o.Settled {
			return ReserveResult{Reservation: o, Fits: true, Existing: true}, nil
		}
	}

	// Re-read open reservations INSIDE the lock and fold them into `want`, which
	// already carries the observed spend. Reading them outside would be exactly
	// the check-then-write race this method exists to close.
	for _, o := range m.reservations {
		if o.Project == project && r.CreatedAt.Before(o.ExpiresAt) {
			want.ReservedCostUSD += o.CostUSD
			want.ReservedSyntheticCostUSD += o.SyntheticCostUSD
			want.ReservedTokens += o.Tokens
			if o.BeadID == r.BeadID {
				want.ReservedTaskTokens += o.Tokens
			}
		}
	}
	rem := budget.Remain(ceiling, want)
	// The rung's own draw must still fit on top of everything already held.
	// NOTE the cost check is against REAL dollars only: r.SyntheticCostUSD is
	// deliberately absent here (Decision 9).
	//
	// gonk-2g4: skip the cost leg for a ZERO-cost (local) rung, mirroring
	// rung.Decide (decide.go: `spec.EstCostUSD > 0 && !rem.FitsCost(...)`). A
	// zero-cost reservation holds no real dollars, so FitsCost(0) -- which is
	// false under a $0 ceiling -- must not gate it, or the onboarding default
	// (monthly_cost_usd: 0, ladder: [qwen-local]) bricks every project. The
	// TOKEN legs are NOT skipped: local rungs are bounded by the token ceilings.
	if (r.CostUSD > 0 && !rem.FitsCost(r.CostUSD)) || !rem.FitsMonthTokens(r.Tokens) || !rem.FitsTaskTokens(r.Tokens) {
		return ReserveResult{}, nil // a lost race is a DEFER, not an error
	}
	m.reservations[r.ID] = r
	return ReserveResult{Reservation: r, Fits: true}, nil
}

func (m *Memory) GetReservation(_ context.Context, id string) (Reservation, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.reservations[id]
	return r, ok, nil
}

// Settle records that the outcome is in, and moves the budget hold to holdUntil
// rather than dropping it -- the session's spend rows have not landed yet.
func (m *Memory) Settle(_ context.Context, id string, holdUntil time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.reservations[id]; ok {
		r.Settled = true
		r.ExpiresAt = holdUntil
		m.reservations[id] = r
	}
	return nil // unknown id: a retried outcome report, not an error
}

// OpenReservations: everything still holding budget, settled or not.
func (m *Memory) OpenReservations(_ context.Context, project string, now time.Time) ([]Reservation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Reservation
	for _, r := range m.reservations {
		if r.Project == project && now.Before(r.ExpiresAt) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ExpireReservations reports each UNSETTLED, past-TTL reservation exactly once
// -- it marks them settled rather than deleting them, so the janitor writes one
// infra-failed attempt per dead session, not one per tick, and a late real
// outcome can still find its reservation and supersede that attempt.
func (m *Memory) ExpireReservations(_ context.Context, now time.Time) ([]Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Reservation
	for id, r := range m.reservations {
		if !r.Settled && !now.Before(r.ExpiresAt) {
			out = append(out, r)
			r.Settled = true
			m.reservations[id] = r
		}
	}
	return out, nil
}

func (m *Memory) AddSpendRows(_ context.Context, rows []spend.Row) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	added := 0
	for _, r := range rows {
		if _, dup := m.seen[r.CallID]; dup {
			continue
		}
		// Non-finite money must not reach the ledger (LiteLLM's spend log is
		// external, unvalidated input). Mark it seen so a poisoned row does not
		// spin the poller forever, but do not count it as added or store it.
		if !financeSafe(r.CostUSD) {
			m.seen[r.CallID] = struct{}{}
			continue
		}
		m.seen[r.CallID] = struct{}{}
		m.rows = append(m.rows, r)
		added++
	}
	return added, nil
}

func (m *Memory) SpendRows(_ context.Context, project string) ([]spend.Row, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []spend.Row
	for _, r := range m.rows {
		if r.Tags.Project == project {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *Memory) AllSpendRows(_ context.Context) ([]spend.Row, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]spend.Row(nil), m.rows...), nil
}

func (m *Memory) SpendCursor(_ context.Context) (time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cursor, nil
}

func (m *Memory) SetSpendCursor(_ context.Context, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = t
	return nil
}

func (m *Memory) SyncedAt(_ context.Context) (time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.syncedAt, nil
}

func (m *Memory) SetSyncedAt(_ context.Context, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncedAt = t
	return nil
}

func (m *Memory) Window(_ context.Context) (spend.Window, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.window, nil
}

func (m *Memory) SetWindow(_ context.Context, w spend.Window) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.window = w
	return nil
}

var _ Store = (*Memory)(nil)
