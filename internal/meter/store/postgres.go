package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// Postgres is the durable Store, backed by the owner's CNPG cluster (owner
// decision, 2026-07-14 -- see docs/spikes/dolt-reservation-isolation.md).
//
// ReserveIfFits' atomicity -- the money-safety-critical property this whole
// package exists for -- comes from a Postgres transaction that takes
// SELECT ... FOR UPDATE on a per-project row before reading the sum of open
// reservations. That is the SAME shape Task 0b's spike measured Dolt failing
// under every isolation strategy (see the spike doc); the difference is that
// Postgres actually blocks a second SELECT ... FOR UPDATE on the same row
// until the first transaction commits or rolls back. TestReserveIfFitsRace
// (postgres_race_test.go, build-tagged `integration`) proves this against a
// real server rather than assuming it.
//
// ReserveIfFits also enforces /decide IDEMPOTENCY in the same transaction: a
// partial UNIQUE index on the open (project, bead_id, session_key) key (plus an
// in-transaction pre-check under the same FOR UPDATE lock) makes the second of
// the two /decide gates return the FIRST reservation instead of minting a
// duplicate that holds double the headroom.
//
// This is NOT single-replica-only, and it closes BOTH money-safety races
// across pods: the lock and the unique index live in the database, not in an
// in-process mutex, so N gonk-meter pods sharing one Postgres are exactly as
// safe as one -- for overspend AND for double-reserve. AD-10's single-replica
// constraint is therefore lifted (see the forthcoming ADR-004, finalized in
// Task 10). Both properties are proven against a real server by
// TestReserveIfFitsRace (overspend) and TestReserveIsIdempotentAcrossReplicas
// (double-reserve), both build-tagged `integration`.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres connects to connString, verifies connectivity, and ensures the
// schema exists (idempotent -- CREATE TABLE IF NOT EXISTS). It is the caller's
// responsibility to Close the returned Postgres.
func NewPostgres(ctx context.Context, connString string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("store: connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping postgres: %w", err)
	}
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() {
	p.pool.Close()
}

const schemaDDL = `
CREATE TABLE IF NOT EXISTS registrations (
	project         TEXT PRIMARY KEY,
	rig             TEXT NOT NULL,
	raw             BYTEA NOT NULL,
	state           TEXT NOT NULL,
	invalid_detail  TEXT NOT NULL DEFAULT '',
	effective       JSONB NOT NULL,
	quiet_hours     JSONB,
	key_secret_name TEXT NOT NULL DEFAULT '',
	key_secret_key  TEXT NOT NULL DEFAULT '',
	key_alias       TEXT NOT NULL DEFAULT '',
	updated_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS attempts (
	project        TEXT NOT NULL,
	bead_id        TEXT NOT NULL,
	reservation_id TEXT NOT NULL,
	seq            BIGSERIAL,
	attempt        INT NOT NULL,
	rung           TEXT NOT NULL,
	outcome        TEXT NOT NULL,
	PRIMARY KEY (project, bead_id, reservation_id)
);
CREATE INDEX IF NOT EXISTS attempts_bead_seq_idx ON attempts (project, bead_id, seq);

-- project_locks is a per-project mutex row for ReserveIfFits. It is separate
-- from registrations because ReserveIfFits must work before a project is
-- registered (reservations are keyed by project name, not by a foreign key
-- into registrations), and because locking the registrations row would make
-- every reservation contend with PutRegistration/reresolve traffic for no
-- reason.
CREATE TABLE IF NOT EXISTS project_locks (
	project TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS reservations (
	id                 TEXT PRIMARY KEY,
	project            TEXT NOT NULL,
	bead_id            TEXT NOT NULL,
	session_key        TEXT NOT NULL,
	rung               TEXT NOT NULL,
	attempt            INT NOT NULL,
	cost_usd           DOUBLE PRECISION NOT NULL,
	synthetic_cost_usd DOUBLE PRECISION NOT NULL,
	tokens             BIGINT NOT NULL,
	created_at         TIMESTAMPTZ NOT NULL,
	expires_at         TIMESTAMPTZ NOT NULL,
	settled            BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS reservations_project_expires_idx ON reservations (project, expires_at);
CREATE INDEX IF NOT EXISTS reservations_settled_expires_idx ON reservations (settled, expires_at);

-- reservations_open_key_idx is the DB-enforced idempotency of /decide, and it
-- is what makes meter multi-replica-safe against DOUBLE-RESERVE (the FOR
-- UPDATE project lock handles OVERSPEND). At most one OPEN (settled = false)
-- reservation may exist for a given (project, bead_id, session_key): intake's
-- Gate 1 and the pack's Gate 2 reserve the same work before any outcome, and
-- the second reserve must return the first, not mint a duplicate that holds
-- double the headroom. A settled reservation (outcome reported) OR an expired
-- one the janitor has reclaimed both carry settled = true and so leave this
-- index, letting a legitimate re-sling reserve the same key again. A partial
-- index cannot reference now(), so "still holding budget" is not part of the
-- predicate; settled = false is the durable proxy, and expiry is turned into
-- settled = true by the janitor.
CREATE UNIQUE INDEX IF NOT EXISTS reservations_open_key_idx
    ON reservations (project, bead_id, session_key) WHERE settled = false;

CREATE TABLE IF NOT EXISTS spend_rows (
	call_id            TEXT PRIMARY KEY,
	project            TEXT NOT NULL,
	rig                TEXT NOT NULL,
	bead_id            TEXT NOT NULL,
	session_key        TEXT NOT NULL,
	rung               TEXT NOT NULL,
	attempt            INT NOT NULL,
	trigger            TEXT NOT NULL,
	cost_usd           DOUBLE PRECISION NOT NULL,
	prompt_tokens      BIGINT NOT NULL,
	completion_tokens  BIGINT NOT NULL,
	at                 TIMESTAMPTZ NOT NULL,
	synthetic          BOOLEAN NOT NULL
);
CREATE INDEX IF NOT EXISTS spend_rows_project_idx ON spend_rows (project);

-- meter_meta is a singleton row (id is always TRUE) holding the spend
-- poller's cursor/syncedAt and the current budget window.
CREATE TABLE IF NOT EXISTS meter_meta (
	id           BOOLEAN PRIMARY KEY DEFAULT TRUE,
	spend_cursor TIMESTAMPTZ,
	synced_at    TIMESTAMPTZ,
	window_start TIMESTAMPTZ,
	window_end   TIMESTAMPTZ,
	CONSTRAINT meter_meta_singleton CHECK (id)
);
INSERT INTO meter_meta (id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING;
`

func (p *Postgres) migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, schemaDDL); err != nil {
		return fmt.Errorf("store: migrate schema: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- registrations

func (p *Postgres) PutRegistration(ctx context.Context, r Registration) error {
	effRaw, err := marshalEffective(r.Effective)
	if err != nil {
		return err
	}
	qhRaw, err := marshalQuietHours(r.QuietHours)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO registrations
			(project, rig, raw, state, invalid_detail, effective, quiet_hours,
			 key_secret_name, key_secret_key, key_alias, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8,$9,$10,$11)
		ON CONFLICT (project) DO UPDATE SET
			rig = EXCLUDED.rig,
			raw = EXCLUDED.raw,
			state = EXCLUDED.state,
			invalid_detail = EXCLUDED.invalid_detail,
			effective = EXCLUDED.effective,
			quiet_hours = EXCLUDED.quiet_hours,
			key_secret_name = EXCLUDED.key_secret_name,
			key_secret_key = EXCLUDED.key_secret_key,
			key_alias = EXCLUDED.key_alias,
			updated_at = EXCLUDED.updated_at
	`, r.Project, r.Rig, r.Raw, string(r.State), r.InvalidDetail, string(effRaw), qhRawArg(qhRaw),
		r.KeyRef.SecretName, r.KeyRef.SecretKey, r.KeyAlias, r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: put registration %q: %w", r.Project, err)
	}
	return nil
}

// qhRawArg turns a nil []byte into a typed nil so pgx sends SQL NULL for the
// quiet_hours::jsonb parameter instead of erroring on an empty string.
func qhRawArg(raw []byte) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

func (p *Postgres) GetRegistration(ctx context.Context, project string) (Registration, bool, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT project, rig, raw, state, invalid_detail, effective, quiet_hours,
		       key_secret_name, key_secret_key, key_alias, updated_at
		FROM registrations WHERE project = $1
	`, project)

	var (
		reg     Registration
		state   string
		effRaw  []byte
		qhRaw   []byte
		keyName string
		keyKey  string
	)
	err := row.Scan(&reg.Project, &reg.Rig, &reg.Raw, &state, &reg.InvalidDetail, &effRaw, &qhRaw,
		&keyName, &keyKey, &reg.KeyAlias, &reg.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Registration{}, false, nil
		}
		return Registration{}, false, fmt.Errorf("store: get registration %q: %w", project, err)
	}
	reg.State = State(state)
	reg.KeyRef = KeyRef{SecretName: keyName, SecretKey: keyKey}
	reg.Effective, err = unmarshalEffective(effRaw)
	if err != nil {
		return Registration{}, false, err
	}
	reg.QuietHours, err = unmarshalQuietHours(qhRaw)
	if err != nil {
		return Registration{}, false, err
	}
	return reg, true, nil
}

func (p *Postgres) DeleteRegistration(ctx context.Context, project string) error {
	if _, err := p.pool.Exec(ctx, `DELETE FROM registrations WHERE project = $1`, project); err != nil {
		return fmt.Errorf("store: delete registration %q: %w", project, err)
	}
	return nil
}

func (p *Postgres) ListRegistrations(ctx context.Context) ([]Registration, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT project, rig, raw, state, invalid_detail, effective, quiet_hours,
		       key_secret_name, key_secret_key, key_alias, updated_at
		FROM registrations ORDER BY project
	`)
	if err != nil {
		return nil, fmt.Errorf("store: list registrations: %w", err)
	}
	defer rows.Close()

	var out []Registration
	for rows.Next() {
		var (
			reg     Registration
			state   string
			effRaw  []byte
			qhRaw   []byte
			keyName string
			keyKey  string
		)
		if err := rows.Scan(&reg.Project, &reg.Rig, &reg.Raw, &state, &reg.InvalidDetail, &effRaw, &qhRaw,
			&keyName, &keyKey, &reg.KeyAlias, &reg.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list registrations: scan: %w", err)
		}
		reg.State = State(state)
		reg.KeyRef = KeyRef{SecretName: keyName, SecretKey: keyKey}
		reg.Effective, err = unmarshalEffective(effRaw)
		if err != nil {
			return nil, err
		}
		reg.QuietHours, err = unmarshalQuietHours(qhRaw)
		if err != nil {
			return nil, err
		}
		out = append(out, reg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list registrations: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- attempts

// RecordAttempt upserts by (project, bead_id, reservation_id). The `seq`
// column (assigned once, on first insert, and never touched by the ON
// CONFLICT branch) preserves the order attempts were first recorded, exactly
// like Memory's append-once-then-overwrite-in-place.
func (p *Postgres) RecordAttempt(ctx context.Context, project, beadID, reservationID string, a rung.Attempt) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO attempts (project, bead_id, reservation_id, attempt, rung, outcome)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (project, bead_id, reservation_id) DO UPDATE SET
			attempt = EXCLUDED.attempt,
			rung = EXCLUDED.rung,
			outcome = EXCLUDED.outcome
	`, project, beadID, reservationID, a.Attempt, a.Rung, string(a.Outcome))
	if err != nil {
		return fmt.Errorf("store: record attempt (%q,%q,%q): %w", project, beadID, reservationID, err)
	}
	return nil
}

func (p *Postgres) Attempts(ctx context.Context, project, beadID string) ([]rung.Attempt, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT attempt, rung, outcome FROM attempts
		WHERE project = $1 AND bead_id = $2
		ORDER BY seq ASC
	`, project, beadID)
	if err != nil {
		return nil, fmt.Errorf("store: attempts (%q,%q): %w", project, beadID, err)
	}
	defer rows.Close()

	var out []rung.Attempt
	for rows.Next() {
		var a rung.Attempt
		var outcome string
		if err := rows.Scan(&a.Attempt, &a.Rung, &outcome); err != nil {
			return nil, fmt.Errorf("store: attempts (%q,%q): scan: %w", project, beadID, err)
		}
		a.Outcome = rung.Outcome(outcome)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: attempts (%q,%q): %w", project, beadID, err)
	}
	return out, nil
}

// ---------------------------------------------------------------- reservations

// ReserveIfFits is the money-safety-critical operation, and it enforces BOTH
// multi-replica guarantees. See the Postgres package doc above,
// TestReserveIfFitsRace (overspend) and TestReserveIsIdempotentAcrossReplicas
// (double-reserve) in postgres_race_test.go for the proofs.
//
// The transaction:
//  1. Ensures a project_locks row exists for `project` (INSERT ... ON
//     CONFLICT DO NOTHING) and takes SELECT ... FOR UPDATE on it. This is what
//     makes everything below exclusive per project across replicas: a second
//     transaction's FOR UPDATE on the same row blocks until this one commits.
//  2. IDEMPOTENCY. Looks for an existing OPEN (settled = false) reservation
//     for r's (project, bead_id, session_key). If one exists, returns it --
//     {existing, Fits: true, Existing: true} -- and inserts nothing. This is
//     what makes the two /decide gates safe: the second reserve returns the
//     first reservation instead of a duplicate holding double the headroom.
//  3. Otherwise sums open reservations (as of r.CreatedAt) INSIDE the lock,
//     computes remaining budget, and checks r fits. If not, rolls back and
//     returns Fits == false -- a lost race, not an error: the caller defers.
//  4. Inserts r with ON CONFLICT on the partial unique index DO NOTHING as a
//     hard backstop (the pre-check + lock already prevent a duplicate; this
//     catches any lock-logic bug at the DB rather than in review), then
//     commits.
func (p *Postgres) ReserveIfFits(ctx context.Context, project string, ceiling budget.Budget, want budget.Spend, r Reservation) (ReserveResult, error) {
	if err := validateFiniteMoney(r.CostUSD, r.SyntheticCostUSD); err != nil {
		return ReserveResult{}, err
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return ReserveResult{}, fmt.Errorf("store: reserve %q: begin: %w", r.ID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	if _, err := tx.Exec(ctx, `INSERT INTO project_locks (project) VALUES ($1) ON CONFLICT (project) DO NOTHING`, project); err != nil {
		return ReserveResult{}, fmt.Errorf("store: reserve %q: seed lock: %w", r.ID, err)
	}
	var locked string
	if err := tx.QueryRow(ctx, `SELECT project FROM project_locks WHERE project = $1 FOR UPDATE`, project).Scan(&locked); err != nil {
		return ReserveResult{}, fmt.Errorf("store: reserve %q: lock project: %w", r.ID, err)
	}

	// IDEMPOTENCY: an existing OPEN reservation for this key wins. Matches the
	// partial unique index predicate exactly (settled = false), so Memory and
	// Postgres agree.
	if existing, found, err := reserveOpenForKey(ctx, tx, project, r.BeadID, r.SessionKey); err != nil {
		return ReserveResult{}, fmt.Errorf("store: reserve %q: check idempotency: %w", r.ID, err)
	} else if found {
		return ReserveResult{Reservation: existing, Fits: true, Existing: true}, nil
	}

	var sumCost, sumSynthetic float64
	var sumTokens, sumTaskTokens int64
	err = tx.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(cost_usd), 0),
			COALESCE(SUM(synthetic_cost_usd), 0),
			COALESCE(SUM(tokens), 0),
			COALESCE(SUM(tokens) FILTER (WHERE bead_id = $3), 0)
		FROM reservations
		WHERE project = $1 AND expires_at > $2
	`, project, r.CreatedAt, r.BeadID).Scan(&sumCost, &sumSynthetic, &sumTokens, &sumTaskTokens)
	if err != nil {
		return ReserveResult{}, fmt.Errorf("store: reserve %q: sum open reservations: %w", r.ID, err)
	}

	want.ReservedCostUSD += sumCost
	want.ReservedSyntheticCostUSD += sumSynthetic
	want.ReservedTokens += sumTokens
	want.ReservedTaskTokens += sumTaskTokens

	rem := budget.Remain(ceiling, want)
	// gonk-2g4: skip the cost leg for a ZERO-cost (local) rung, mirroring
	// rung.Decide (decide.go: `spec.EstCostUSD > 0 && !rem.FitsCost(...)`) and
	// kept byte-consistent with Memory.ReserveIfFits. A zero-cost reservation
	// holds no real dollars, so FitsCost(0) -- false under a $0 ceiling -- must
	// not gate it, or the onboarding default (monthly_cost_usd: 0, ladder:
	// [qwen-local]) bricks every project. The TOKEN legs are NOT skipped.
	if (r.CostUSD > 0 && !rem.FitsCost(r.CostUSD)) || !rem.FitsMonthTokens(r.Tokens) || !rem.FitsTaskTokens(r.Tokens) {
		return ReserveResult{}, nil // lost race: rolled back by the deferred Rollback
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO reservations
			(id, project, bead_id, session_key, rung, attempt, cost_usd, synthetic_cost_usd, tokens, created_at, expires_at, settled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,FALSE)
		ON CONFLICT (project, bead_id, session_key) WHERE settled = false DO NOTHING
	`, r.ID, r.Project, r.BeadID, r.SessionKey, r.Rung, r.Attempt, r.CostUSD, r.SyntheticCostUSD, r.Tokens, r.CreatedAt, r.ExpiresAt)
	if err != nil {
		return ReserveResult{}, fmt.Errorf("store: reserve %q: insert: %w", r.ID, err)
	}
	if tag.RowsAffected() == 0 {
		// Hard backstop fired: a concurrent open reservation for this key exists
		// despite the pre-check + lock (should be unreachable). Return it rather
		// than the un-inserted r, so the caller never holds a reservation id the
		// DB does not have.
		existing, found, err := reserveOpenForKey(ctx, tx, project, r.BeadID, r.SessionKey)
		if err != nil || !found {
			return ReserveResult{}, fmt.Errorf("store: reserve %q: insert hit the open-key index but no open reservation was found (found=%v): %w", r.ID, found, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return ReserveResult{}, fmt.Errorf("store: reserve %q: commit: %w", r.ID, err)
		}
		return ReserveResult{Reservation: existing, Fits: true, Existing: true}, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return ReserveResult{}, fmt.Errorf("store: reserve %q: commit: %w", r.ID, err)
	}
	return ReserveResult{Reservation: r, Fits: true}, nil
}

// reserveOpenForKey returns the single OPEN (settled = false) reservation for
// (project, bead_id, session_key), if any. The partial unique index
// reservations_open_key_idx guarantees at most one, so LIMIT 1 is exact, not
// arbitrary.
func reserveOpenForKey(ctx context.Context, tx pgx.Tx, project, beadID, sessionKey string) (Reservation, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, project, bead_id, session_key, rung, attempt, cost_usd, synthetic_cost_usd, tokens, created_at, expires_at, settled
		FROM reservations
		WHERE project = $1 AND bead_id = $2 AND session_key = $3 AND settled = false
		LIMIT 1
	`, project, beadID, sessionKey)
	res, err := scanReservation(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Reservation{}, false, nil
		}
		return Reservation{}, false, err
	}
	return res, true, nil
}

func (p *Postgres) GetReservation(ctx context.Context, id string) (Reservation, bool, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id, project, bead_id, session_key, rung, attempt, cost_usd, synthetic_cost_usd, tokens, created_at, expires_at, settled
		FROM reservations WHERE id = $1
	`, id)
	r, err := scanReservation(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Reservation{}, false, nil
		}
		return Reservation{}, false, fmt.Errorf("store: get reservation %q: %w", id, err)
	}
	return r, true, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanReservation(row rowScanner) (Reservation, error) {
	var r Reservation
	err := row.Scan(&r.ID, &r.Project, &r.BeadID, &r.SessionKey, &r.Rung, &r.Attempt,
		&r.CostUSD, &r.SyntheticCostUSD, &r.Tokens, &r.CreatedAt, &r.ExpiresAt, &r.Settled)
	return r, err
}

// Settle is a no-op (not an error) on an unknown id: outcome reports are
// retried, and a retried Settle of an id that never existed (or that has
// already expired and been deleted -- Postgres keeps expired reservations
// rather than deleting them, but a future GC could) must not fail the caller.
func (p *Postgres) Settle(ctx context.Context, id string, holdUntil time.Time) error {
	if _, err := p.pool.Exec(ctx, `UPDATE reservations SET settled = TRUE, expires_at = $2 WHERE id = $1`, id, holdUntil); err != nil {
		return fmt.Errorf("store: settle %q: %w", id, err)
	}
	return nil
}

func (p *Postgres) OpenReservations(ctx context.Context, project string, now time.Time) ([]Reservation, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, project, bead_id, session_key, rung, attempt, cost_usd, synthetic_cost_usd, tokens, created_at, expires_at, settled
		FROM reservations WHERE project = $1 AND expires_at > $2
	`, project, now)
	if err != nil {
		return nil, fmt.Errorf("store: open reservations %q: %w", project, err)
	}
	defer rows.Close()

	var out []Reservation
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, fmt.Errorf("store: open reservations %q: scan: %w", project, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: open reservations %q: %w", project, err)
	}
	return out, nil
}

// ExpireReservations atomically claims every unsettled, past-TTL reservation
// via a single UPDATE ... RETURNING: Postgres's row locking makes this safe
// against a concurrent janitor tick claiming the same reservation twice (the
// second UPDATE's WHERE settled = FALSE simply excludes rows the first one
// already flipped).
func (p *Postgres) ExpireReservations(ctx context.Context, now time.Time) ([]Reservation, error) {
	rows, err := p.pool.Query(ctx, `
		UPDATE reservations SET settled = TRUE
		WHERE settled = FALSE AND expires_at <= $1
		RETURNING id, project, bead_id, session_key, rung, attempt, cost_usd, synthetic_cost_usd, tokens, created_at, expires_at, settled
	`, now)
	if err != nil {
		return nil, fmt.Errorf("store: expire reservations: %w", err)
	}
	defer rows.Close()

	var out []Reservation
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, fmt.Errorf("store: expire reservations: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: expire reservations: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- spend rows

// AddSpendRows rejects (drops, without erroring the whole batch) any row
// whose CostUSD is not finite -- LiteLLM's spend log is external, unvalidated
// input, and a NaN/+-Inf value must never reach a budget computation. Every
// other row is inserted with ON CONFLICT (call_id) DO NOTHING so a re-polled
// overlapping window cannot double-count.
func (p *Postgres) AddSpendRows(ctx context.Context, rows []spend.Row) (int, error) {
	added := 0
	for _, r := range rows {
		if !financeSafe(r.CostUSD) {
			continue
		}
		tag, err := p.pool.Exec(ctx, `
			INSERT INTO spend_rows
				(call_id, project, rig, bead_id, session_key, rung, attempt, trigger,
				 cost_usd, prompt_tokens, completion_tokens, at, synthetic)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (call_id) DO NOTHING
		`, r.CallID, r.Tags.Project, r.Tags.Rig, r.Tags.BeadID, r.Tags.SessionKey, r.Tags.Rung,
			r.Tags.Attempt, r.Tags.Trigger, r.CostUSD, r.PromptTokens, r.CompletionTokens, r.At, r.Synthetic)
		if err != nil {
			return added, fmt.Errorf("store: add spend row %q: %w", r.CallID, err)
		}
		added += int(tag.RowsAffected())
	}
	return added, nil
}

func (p *Postgres) SpendRows(ctx context.Context, project string) ([]spend.Row, error) {
	return p.querySpendRows(ctx, `
		SELECT call_id, project, rig, bead_id, session_key, rung, attempt, trigger,
		       cost_usd, prompt_tokens, completion_tokens, at, synthetic
		FROM spend_rows WHERE project = $1
	`, project)
}

func (p *Postgres) AllSpendRows(ctx context.Context) ([]spend.Row, error) {
	return p.querySpendRows(ctx, `
		SELECT call_id, project, rig, bead_id, session_key, rung, attempt, trigger,
		       cost_usd, prompt_tokens, completion_tokens, at, synthetic
		FROM spend_rows
	`)
}

func (p *Postgres) querySpendRows(ctx context.Context, q string, args ...any) ([]spend.Row, error) {
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: spend rows: %w", err)
	}
	defer rows.Close()

	var out []spend.Row
	for rows.Next() {
		var r spend.Row
		if err := rows.Scan(&r.CallID, &r.Tags.Project, &r.Tags.Rig, &r.Tags.BeadID, &r.Tags.SessionKey,
			&r.Tags.Rung, &r.Tags.Attempt, &r.Tags.Trigger, &r.CostUSD, &r.PromptTokens, &r.CompletionTokens,
			&r.At, &r.Synthetic); err != nil {
			return nil, fmt.Errorf("store: spend rows: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: spend rows: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- meta

func (p *Postgres) SpendCursor(ctx context.Context) (time.Time, error) {
	return p.metaTime(ctx, "spend_cursor")
}

func (p *Postgres) SetSpendCursor(ctx context.Context, t time.Time) error {
	return p.setMetaTime(ctx, "spend_cursor", t)
}

func (p *Postgres) SyncedAt(ctx context.Context) (time.Time, error) {
	return p.metaTime(ctx, "synced_at")
}

func (p *Postgres) SetSyncedAt(ctx context.Context, t time.Time) error {
	return p.setMetaTime(ctx, "synced_at", t)
}

func (p *Postgres) metaTime(ctx context.Context, column string) (time.Time, error) {
	var t *time.Time
	q := fmt.Sprintf(`SELECT %s FROM meter_meta WHERE id = TRUE`, column) //nolint:gosec // column is one of a fixed internal set, never caller input
	if err := p.pool.QueryRow(ctx, q).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("store: read meta %s: %w", column, err)
	}
	if t == nil {
		return time.Time{}, nil
	}
	return *t, nil
}

func (p *Postgres) setMetaTime(ctx context.Context, column string, t time.Time) error {
	q := fmt.Sprintf(`UPDATE meter_meta SET %s = $1 WHERE id = TRUE`, column) //nolint:gosec // column is one of a fixed internal set, never caller input
	if _, err := p.pool.Exec(ctx, q, t); err != nil {
		return fmt.Errorf("store: set meta %s: %w", column, err)
	}
	return nil
}

func (p *Postgres) Window(ctx context.Context) (spend.Window, error) {
	var start, end *time.Time
	if err := p.pool.QueryRow(ctx, `SELECT window_start, window_end FROM meter_meta WHERE id = TRUE`).Scan(&start, &end); err != nil {
		return spend.Window{}, fmt.Errorf("store: read window: %w", err)
	}
	var w spend.Window
	if start != nil {
		w.Start = *start
	}
	if end != nil {
		w.End = *end
	}
	return w, nil
}

func (p *Postgres) SetWindow(ctx context.Context, w spend.Window) error {
	if _, err := p.pool.Exec(ctx, `UPDATE meter_meta SET window_start = $1, window_end = $2 WHERE id = TRUE`, w.Start, w.End); err != nil {
		return fmt.Errorf("store: set window: %w", err)
	}
	return nil
}

var _ Store = (*Postgres)(nil)
