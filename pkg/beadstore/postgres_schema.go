package beadstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// workStoreSchemaVersion is the version of workStoreDDL. Bump it with every
// change to the DDL: a process only runs DDL when the stored version is older.
//
// 2: work_items_revision_set CHECK and work_items_quarantine (gonk-t41 rework).
// Version 1 was never deployed; the bump makes any database migrated by the
// superseded 5499ee0 build pick the additions up.
//
// 3: work_items_pipeline_run_open_idx, the partial index behind
// ListOpenPipelineRuns (gonk-tios verify report R6). Additive: an older binary
// never reads it and Postgres maintains it on every write.
const workStoreSchemaVersion = 3

// pipelineRunOpenPredicate is the partial index's predicate AND
// ListOpenPipelineRuns' WHERE clause. One constant, so the two cannot drift
// apart -- if they did, the planner would silently fall back to a scan.
// Record has no json tags, so the key is the Go field name; a record written
// before the field existed has no key, ->> is NULL, and it is not open.
const pipelineRunOpenPredicate = `(record->>'PipelineRunOpen') = 'true'`

// workStoreSchemaLock is the pg_advisory_xact_lock key the migration takes, so
// two first processes cannot race CREATE TABLE (Postgres can fail the loser of
// that race with a unique violation on pg_type even under IF NOT EXISTS).
// Fixed: the first eight bytes of sha256("gonk-workstore-schema"), b514e1a1f9785366,
// read as a big-endian int64.
const workStoreSchemaLock int64 = -5398441967518985370

// workStoreDDL is THE SCHEMA of gonk-gate's work-item state (gonk-t41). It is
// documented, with the reasons for each choice, in
// docs/reference/gonk-state-record.md ("Storage in Postgres") and
// docs/plans/2026-09-14-t41-postgres-workstore.md section 2. Keep all three in
// step.
//
// Every statement is idempotent (IF NOT EXISTS), the same mechanism the meter's
// store uses (internal/meter/store/postgres.go schemaDDL). What differs is only
// WHEN it runs: see ensureWorkStoreSchema.
const workStoreDDL = `
CREATE TABLE IF NOT EXISTS work_items (
	anchor     TEXT PRIMARY KEY,
	revision   TEXT NOT NULL,
	state      TEXT NOT NULL,
	record     JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	written_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT work_items_revision_set CHECK (revision <> ''),
	CONSTRAINT work_items_record_agrees CHECK (
		record->>'BeadAnchor' = anchor AND
		record->>'State'      = state  AND
		record->>'Revision'   = revision)
);
CREATE INDEX IF NOT EXISTS work_items_state_idx ON work_items (state, anchor COLLATE "C");

-- Added in schema version 3 (gonk-tios R6): the open pipeline runs only.
CREATE INDEX IF NOT EXISTS work_items_pipeline_run_open_idx ON work_items (anchor COLLATE "C")
	WHERE ` + pipelineRunOpenPredicate + `;

-- Added in schema version 2; ADD CONSTRAINT has no IF NOT EXISTS, so guard it.
DO $$ BEGIN
	ALTER TABLE work_items ADD CONSTRAINT work_items_revision_set CHECK (revision <> '');
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS work_items_bd_sync (
	anchor      TEXT PRIMARY KEY,
	bd_digest   TEXT NOT NULL,
	bd_revision TEXT NOT NULL,
	pg_revision TEXT NOT NULL,
	synced_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS work_items_quarantine (
	anchor         TEXT NOT NULL,
	quarantined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	reason         TEXT NOT NULL,
	original_state TEXT NOT NULL,
	last_write_at  TIMESTAMPTZ,
	bd_record      JSONB NOT NULL,
	PRIMARY KEY (anchor, quarantined_at)
);

CREATE TABLE IF NOT EXISTS workstore_meta (
	id             BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
	schema_version INT NOT NULL,
	bd_imported_at TIMESTAMPTZ
);
`

// ensureWorkStoreSchema makes the schema current.
//
// THE FAST PATH IS THE POINT. The meter runs its DDL once per pod start;
// gonk-gate opens this store in every dispatch and every 30-second sweep, each
// a separate short-lived process. `CREATE INDEX IF NOT EXISTS` takes a SHARE
// lock on the table even when the index already exists, so running the DDL on
// every open would make every process briefly queue behind -- and block -- the
// writes this store exists to serve. So: one read of workstore_meta, and only
// if the schema is missing or older does the DDL run, in a transaction under an
// advisory lock.
//
// A stored version NEWER than this binary's is accepted: a rolled-back binary
// must still be able to open a store a newer one migrated, and every change is
// required to be additive.
func ensureWorkStoreSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('workstore_meta') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("beadstore: check work store schema: %w", err)
	}
	if exists {
		var v int
		err := pool.QueryRow(ctx, `SELECT schema_version FROM workstore_meta WHERE id`).Scan(&v)
		switch {
		case err == nil && v >= workStoreSchemaVersion:
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("beadstore: read work store schema version: %w", err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beadstore: migrate work store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, workStoreSchemaLock); err != nil {
		return fmt.Errorf("beadstore: migrate work store: lock: %w", err)
	}
	if _, err := tx.Exec(ctx, workStoreDDL); err != nil {
		return fmt.Errorf("beadstore: migrate work store: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workstore_meta (id, schema_version) VALUES (TRUE, $1)
		ON CONFLICT (id) DO UPDATE SET schema_version = GREATEST(workstore_meta.schema_version, EXCLUDED.schema_version)
	`, workStoreSchemaVersion); err != nil {
		return fmt.Errorf("beadstore: migrate work store: record version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("beadstore: migrate work store: commit: %w", err)
	}
	return nil
}
