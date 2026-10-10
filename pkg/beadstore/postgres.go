package beadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is gonk-gate's work-item store (gonk-t41): one row per BeadAnchor in
// the work_items table of schema gonk_workstore, owned by the login role
// gonk_workstore -- its OWN role and schema, never the meter's ledger role
// (owner decision B4 on the review; requireOwnSchema refuses anything else).
// The schema and its reasons are in docs/reference/gonk-state-record.md.
//
// WHY IT EXISTS. PutIf is the compare-and-swap every duplicate-agent guard on
// the sweep and dispatch paths depends on, and BdCLI cannot provide one: bd
// orders a record's comments by a client timestamp rather than commit order
// (gonk-vred), and its first write to an anchor can create two beads
// (bd_notfinal_test.go). Here the compare-and-swap is one SQL statement against
// one row, and Postgres's row lock serialises every writer of that row.
//
// WHAT HOLDS, each pinned by a test against a real server (build tag
// integration):
//
//   - PutIf success is final and the applied writes form one chain
//     (storetest.RunCompareAndSwap, TestPostgresManyWritersOneChain).
//   - State cannot disagree with the record: both are written by the same
//     statement and a CHECK constraint makes a disagreement unrepresentable
//     (gonk-9sf9; TestPostgresStateColumnCannotDisagreeWithTheRecord).
//   - List is unbounded (gonk-kx3; storetest ListIsNotTruncated).
//   - A killed writer cannot land late (postgres_kill_test.go): see
//     workStoreRuntimeParams.
type Postgres struct {
	pool *pgxpool.Pool
}

// workStoreRuntimeParams are set on every connection the store opens. They are
// part of the store's semantics, not tuning, so they override the DSN.
//
// client_connection_check_interval is the one that matters. An autocommit
// statement that reached the server keeps running if its client dies, and a
// statement waiting on a row lock does not otherwise notice: without this, a
// gonk-gate process SIGKILLed at its order timeout while its PutIf waited on
// another writer's row lock would have that PutIf execute -- and commit --
// after the process was gone. With it the server polls the socket and cancels
// the statement within a second. It needs Postgres 14 or later (the reference cluster runs 17;
// CI 16).
//
// The timeouts are the backstop if that poll cannot run: no statement of this
// store waits on a lock for more than 5s or runs for more than 15s, and no
// transaction sits idle holding locks for more than 15s. The store's own
// statements take milliseconds.
var workStoreRuntimeParams = map[string]string{
	"client_connection_check_interval":    "1s",
	"lock_timeout":                        "5s",
	"statement_timeout":                   "15s",
	"idle_in_transaction_session_timeout": "15s",
	"application_name":                    "gonk-workstore",
	// search_path "$user" ONLY: every unqualified name resolves to the role's own
	// schema and NEVER falls through to public. With Postgres's default
	// ("$user", public), a work_items or workstore_meta readable in public but
	// absent from the role's schema would be found and used -- the schema check
	// in requireOwnSchema looks at current_schema() and cannot see that
	// (independent verification of the gonk-t41 rework, defect 2).
	"search_path": `"$user"`,
}

// workStoreConnectTimeout bounds a connection attempt when the DSN does not.
// gonk-gate opens the store at process start, inside its order's time budget.
const workStoreConnectTimeout = 10 * time.Second

// workStoreMaxConns bounds the pool (gonk-upxg). pgxpool's default is
// max(4, NumCPU) -- 32 on a 32-thread box, per store, and the tests open one
// store per test while the meter's suite opens its own pools in parallel. That
// peaked at 79 backends against postgres:16's 100-connection default and failed
// with SQLSTATE 53300 ("sorry, too many clients already") at -count=3: a
// capacity accident in the harness, not a defect in the code under test.
//
// 8 is well above what this store needs -- gonk-gate is a short-lived process
// that runs a handful of statements per invocation, one dispatch or sweep at a
// time -- and it makes the ceiling a property of the store rather than of the
// machine that happens to run it. A DSN that sets pool_max_conns still wins,
// whatever value it asks for (dsnSetsPoolMaxConns).
//
// The meter's PRODUCTION pool is a separate matter and still unbounded
// (gonk-155s): this cap covers gonk-gate's work store only.
const workStoreMaxConns = 8

// dsnSetsPoolMaxConns reports whether the DSN itself asked for a pool size.
//
// It must not be inferred from the parsed MaxConns: pgxpool has already put its
// own default there, and a DSN asking for exactly that default -- 12 on a
// 12-thread box -- is then indistinguishable from a DSN that said nothing, so
// an explicit request would be silently overridden. pgconn.ParseConfig keeps
// unrecognised keys in RuntimeParams, for BOTH DSN shapes (URL and
// keyword=value), which answers the question directly. A DSN pgconn cannot
// parse is treated as silent: the cap is the safe side.
func dsnSetsPoolMaxConns(dsn string) bool {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return false
	}
	_, ok := cfg.RuntimeParams["pool_max_conns"]
	return ok
}

// OpenPostgres connects to dsn, applies workStoreRuntimeParams, verifies the
// connection and makes the schema current. The caller must Close it.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx's parse error can quote the DSN, password included. Say which
		// thing failed, not what it contained.
		return nil, errors.New("beadstore: parse work store DSN: invalid connection string")
	}
	for k, v := range workStoreRuntimeParams {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = workStoreConnectTimeout
	}
	// Only when the DSN did not ask for a size of its own.
	if !dsnSetsPoolMaxConns(dsn) {
		cfg.MaxConns = workStoreMaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("beadstore: open work store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("beadstore: connect to work store: %w", err)
	}
	if err := requireOwnSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := ensureWorkStoreSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

// requireOwnSchema refuses a connection whose tables would land anywhere but a
// schema its own role owns (owner decision on the gonk-t41 review: isolation).
//
// The work store runs as login role gonk_workstore, owning schema
// gonk_workstore in database gonk; Postgres's default search_path ("$user",
// public) then resolves unqualified names to that schema with no code or DSN
// change. The controller must NOT hold the meter's `gonk` role -- with it, it
// could settle reservations, delete spend rows, read every project's LiteLLM key
// and rewrite registrations (the review verified all four).
//
// The failure this refuses is quiet: if the schema was not created before the
// first deploy, current_schema() falls through to public, and CREATE TABLE
// either fails (Postgres 15+ grants no CREATE on public) or, for a role that
// can, lands the tables in public -- after which moving them is a migration. So:
// no schema at all, public, or a schema someone else owns is an error that names
// the fix.
func requireOwnSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var schema, user string
	var owned bool
	err := pool.QueryRow(ctx, `
		SELECT coalesce(current_schema(), ''), current_user,
		       coalesce((SELECT pg_get_userbyid(nspowner) = current_user FROM pg_namespace WHERE nspname = current_schema()), false)
	`).Scan(&schema, &user, &owned)
	if err != nil {
		return fmt.Errorf("beadstore: check the work store's schema: %w", err)
	}
	if schema == "" || schema == "public" || !owned {
		return fmt.Errorf("beadstore: the work store must live in a schema its own role owns; role %q resolves to "+
			"schema %q (owned by it: %v). Create it first -- CREATE SCHEMA %s AUTHORIZATION %s, on CNPG the "+
			"Database CR's spec.schemas -- see docs/reference/gonk-state-record.md", user, schema, owned, user, user)
	}
	return nil
}

// Close releases every connection.
func (p *Postgres) Close() { p.pool.Close() }

// encodeRecord is the JSONB payload. The Record has no json tags, so the keys
// are the Go field names -- the same JSON BdCLI's gonk-state comments carry, and
// the names the work_items_record_agrees CHECK reads (BeadAnchor, State,
// Revision). Times keep nanoseconds, which a TIMESTAMPTZ column would not.
func encodeRecord(r Record) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("beadstore: encode record %s: %w", r.BeadAnchor, err)
	}
	return string(b), nil
}

// Put upserts r on its BeadAnchor, unconditionally, after stampWrite (the same
// UpdatedAt rule and fresh Revision as every store).
//
// One statement: the row, its state and its revision change together or not at
// all. It lands strictly before or after any other write to the row -- Postgres
// takes the row lock -- so it replaces whatever is stored when it lands, which
// is exactly the contract (Store.Put).
func (p *Postgres) Put(ctx context.Context, r Record) error {
	r = stampWrite(r, time.Now().UTC())
	payload, err := encodeRecord(r)
	if err != nil {
		return err
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO work_items (anchor, revision, state, record)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (anchor) DO UPDATE SET
			revision   = EXCLUDED.revision,
			state      = EXCLUDED.state,
			record     = EXCLUDED.record,
			written_at = now()
	`, r.BeadAnchor, r.Revision, string(r.State), payload); err != nil {
		return fmt.Errorf("beadstore: put %s: %w", r.BeadAnchor, err)
	}
	return nil
}

// PutIf writes r only if the stored record is still at ifRevision, and returns
// the revision it wrote.
//
// THE COMPARE-AND-SWAP IS ONE STATEMENT, so there is nothing between the compare
// and the swap for another writer to get into:
//
//   - ifRevision != "": UPDATE ... WHERE anchor = $1 AND revision = $2. Under
//     READ COMMITTED a concurrent UPDATE of the same row waits for the row lock
//     and then re-evaluates its WHERE against the row the first writer
//     committed, whose revision is new -- so of two writers on one revision,
//     exactly one matches.
//   - ifRevision == "": INSERT ... ON CONFLICT DO NOTHING. It lands only on an
//     absent anchor. In this store "" means exactly that and nothing more: no
//     row has an empty revision (CHECK revision <> ”; the bd import assigns
//     one to every record written before revisions existed -- review defect
//     A1). The Store contract's "or a record written before revisions existed"
//     therefore never arises here. A concurrent insert of the same anchor waits
//     for the first to commit and then does nothing.
//
// OUTCOMES. A nil error from Exec means the server executed and committed the
// statement, so zero rows affected is a settled refusal: ErrStale, nothing
// written, ever. ANY error is returned as it is, never as ErrStale: the
// statement may have committed after the reply was lost, so the outcome is
// unknown -- exactly how the callers already treat a non-stale error
// (broker_inject.go reservationUnknown).
func (p *Postgres) PutIf(ctx context.Context, r Record, ifRevision string) (string, error) {
	r = stampWrite(r, time.Now().UTC())
	payload, err := encodeRecord(r)
	if err != nil {
		return "", err
	}
	var affected int64
	if ifRevision == "" {
		tag, err := p.pool.Exec(ctx, `
			INSERT INTO work_items (anchor, revision, state, record)
			VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (anchor) DO NOTHING
		`, r.BeadAnchor, r.Revision, string(r.State), payload)
		if err != nil {
			return "", fmt.Errorf("beadstore: conditional put %s (expecting no record): outcome unknown: %w", r.BeadAnchor, err)
		}
		affected = tag.RowsAffected()
	} else {
		tag, err := p.pool.Exec(ctx, `
			UPDATE work_items SET
				revision   = $2,
				state      = $3,
				record     = $4::jsonb,
				written_at = now()
			WHERE anchor = $1 AND revision = $5
		`, r.BeadAnchor, r.Revision, string(r.State), payload, ifRevision)
		if err != nil {
			return "", fmt.Errorf("beadstore: conditional put %s (expecting revision %q): outcome unknown: %w", r.BeadAnchor, ifRevision, err)
		}
		affected = tag.RowsAffected()
	}
	switch affected {
	case 1:
		return r.Revision, nil
	case 0:
		return "", fmt.Errorf("%w: %s is not at revision %q", ErrStale, r.BeadAnchor, ifRevision)
	default:
		// The anchor is the primary key; more than one row cannot match.
		return "", fmt.Errorf("beadstore: conditional put %s affected %d rows", r.BeadAnchor, affected)
	}
}

func scanRecord(row pgx.Row) (Record, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("beadstore: decode work item: %w", err)
	}
	return r, nil
}

// Get returns the record stored for anchor.
func (p *Postgres) Get(ctx context.Context, anchor string) (Record, bool, error) {
	r, err := scanRecord(p.pool.QueryRow(ctx, `SELECT record FROM work_items WHERE anchor = $1`, anchor))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Record{}, false, nil
	case err != nil:
		return Record{}, false, fmt.Errorf("beadstore: get %s: %w", anchor, err)
	}
	return r, true, nil
}

// List returns EVERY record in state st -- no limit, no page (gonk-kx3) -- by
// the work_items_state_idx index, in byte order of the anchor (Memory's order;
// COLLATE "C" so the database locale cannot change it).
func (p *Postgres) List(ctx context.Context, st State) ([]Record, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT record FROM work_items WHERE state = $1 ORDER BY anchor COLLATE "C"`, string(st))
	if err != nil {
		return nil, fmt.Errorf("beadstore: list %s: %w", st, err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("beadstore: list %s: %w", st, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("beadstore: list %s: %w", st, err)
	}
	return out, nil
}

// ListOpenPipelineRuns is Store.ListOpenPipelineRuns, answered from the
// partial index work_items_pipeline_run_open_idx (schema version 3): its
// predicate is EXACTLY this WHERE clause, so the planner reads only the open
// runs' index entries -- a closed run, however many accumulate in done, is
// never scanned or decoded. Keep the two predicates byte-identical.
func (p *Postgres) ListOpenPipelineRuns(ctx context.Context) ([]Record, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT record FROM work_items WHERE `+pipelineRunOpenPredicate+` ORDER BY anchor COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("beadstore: list open pipeline runs: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("beadstore: list open pipeline runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("beadstore: list open pipeline runs: %w", err)
	}
	return out, nil
}

// BdImportCompleted reports whether a bd import has completed against this
// store (gonk-gate workstore-import; docs/plans/2026-09-14-t41-postgres-workstore.md
// section 6). gonk-gate refuses to dispatch or sweep on a store without one: an
// empty work store would show dispatch no reservations and the sweep no running
// records -- duplicate sessions and orphans.
func (p *Postgres) BdImportCompleted(ctx context.Context) (bool, error) {
	var done bool
	err := p.pool.QueryRow(ctx, `SELECT bd_imported_at IS NOT NULL FROM workstore_meta WHERE id`).Scan(&done)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("beadstore: read bd import marker: %w", err)
	}
	return done, nil
}

var _ Store = (*Postgres)(nil)
