//go:build integration

package beadstore_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	meterstore "github.com/gonk-factory/gonk/internal/meter/store"
	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// OWNER DECISION (gonk-t41 review, merge blocker): the controller must not hold
// the meter's role. The review verified that with the meter's `gonk` role the
// controller could UPDATE reservations SET settled=true (overspend), DELETE FROM
// spend_rows (erase spend), SELECT litellm_key FROM prompts (read every
// project's LiteLLM key) and rewrite registrations.effective.
//
// This builds the reference cluster's shape in one database: a role that owns the database and
// runs the REAL meter migration (store.NewPostgres, tables in public), and
// role gonk_workstore owning schema gonk_workstore. Connected as gonk_workstore
// with the default search_path, all four of the review's statements are denied,
// and the work store opens, lands its tables in its own schema, and works.
func TestWorkStoreRoleCannotTouchTheMeterLedger(t *testing.T) {
	ctx := context.Background()
	wsDSN := newWorkStoreDatabase(t)
	wsCfg, err := pgx.ParseConfig(wsDSN)
	if err != nil {
		t.Fatal(err)
	}
	_, admin := newDatabaseConnFor(t, wsCfg.Database)

	const meterRole = "gonk_ledger_owner_test"
	if _, err := admin.Exec(ctx, `DO $$ BEGIN CREATE ROLE `+meterRole+` LOGIN PASSWORD 'm';
		EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `ALTER DATABASE `+pgx.Identifier{wsCfg.Database}.Sanitize()+` OWNER TO `+meterRole); err != nil {
		t.Fatal(err)
	}
	meterDSN := fmt.Sprintf("postgres:"+"//%s:m@%s:%d/%s?sslmode=disable", meterRole, wsCfg.Host, wsCfg.Port, wsCfg.Database)
	meter, err := meterstore.NewPostgres(ctx, meterDSN)
	if err != nil {
		t.Fatalf("meter migration as the database owner: %v", err)
	}
	meter.Close()

	ws := openWorkStore(t, wsDSN)
	if err := ws.Put(ctx, beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StateRunning}); err != nil {
		t.Fatalf("work store Put as gonk_workstore: %v", err)
	}
	c := rawConn(t, wsDSN)
	var where string
	if err := c.QueryRow(ctx, `SELECT schemaname FROM pg_tables WHERE tablename = 'work_items'`).Scan(&where); err != nil || where != "gonk_workstore" {
		t.Fatalf("work_items is in schema %q (%v), want gonk_workstore", where, err)
	}
	for _, stmt := range []string{
		`UPDATE reservations SET settled = true`,
		`DELETE FROM spend_rows`,
		`SELECT litellm_key FROM prompts`,
		`UPDATE registrations SET effective = '{}'::jsonb`,
		`CREATE TABLE public.sneaky (x int)`,
	} {
		_, err := c.Exec(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("as gonk_workstore, %q = %v; want permission denied", stmt, err)
		}
	}
}

// The store refuses to open where its tables would land in public (the schema
// was not created first) -- rather than creating them there, after which
// moving them is a migration.
func TestWorkStoreRefusesToLiveInPublic(t *testing.T) {
	adminDSN, _ := newDatabase(t)
	_, err := beadstore.OpenPostgres(context.Background(), adminDSN)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("OpenPostgres with only public available = %v; want a refusal naming the schema", err)
	}
}

// gonk-p44x (a): the ONLY case requireOwnSchema catches by itself. Deleting the
// guard leaves TestWorkStoreRefusesToLiveInPublic green, because that DSN's role
// has no schema of its own at all and the open then fails later, in CREATE
// TABLE, with a message that happens to contain "schema". The case nothing else
// catches is a schema NAMED for the role (so search_path "$user" resolves to it)
// but OWNED BY SOMEONE ELSE: the role can create there, so without the guard
// the work store would quietly land its tables in a schema whose owner can read
// and rewrite every work record -- exactly the isolation the owner decided
// against.
func TestWorkStoreRefusesASchemaOwnedBySomeoneElse(t *testing.T) {
	ctx := context.Background()
	const guest = "gonk_ws_guest"
	const guestPass = "gonk-ws-guest-test"
	adminDSN, admin := newDatabase(t)
	if _, err := admin.Exec(ctx, `DO $$ BEGIN
		CREATE ROLE `+guest+` LOGIN PASSWORD '`+guestPass+`';
	EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`); err != nil {
		t.Fatalf("create role %s: %v", guest, err)
	}
	// The schema is named for the role -- so "$user" resolves to it -- but the
	// ADMIN owns it, and the role may create in it.
	for _, stmt := range []string{
		`CREATE SCHEMA ` + guest,
		`GRANT USAGE, CREATE ON SCHEMA ` + guest + ` TO ` + guest,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	dsn := fmt.Sprintf("postgres:"+"//%s:%s@%s:%d/%s?sslmode=disable", guest, guestPass, cfg.Host, cfg.Port, cfg.Database)

	s, err := beadstore.OpenPostgres(ctx, dsn)
	if err == nil {
		s.Close()
		t.Fatal("OpenPostgres accepted a schema the role does not own; the work store would live in someone else's schema")
	}
	if !strings.Contains(err.Error(), "schema its own role owns") {
		t.Fatalf("OpenPostgres = %v; want the refusal naming the ownership rule", err)
	}
	// Proof the refusal is the guard's and not a permission failure: nothing was
	// created, though the role CAN create there.
	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = $1`, guest).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tables in schema %s = %d, %v; want none", guest, n, err)
	}
	if _, err := admin.Exec(ctx, `CREATE TABLE `+guest+`.proof_the_role_could_create ()`); err != nil {
		t.Fatalf("the admin cannot create in %s either, so the test proves nothing: %v", guest, err)
	}
}

// newDatabaseConnFor opens an admin connection to an existing test database.
func newDatabaseConnFor(t *testing.T, name string) (string, *pgx.Conn) {
	t.Helper()
	adminCfg, err := pgx.ParseConfig(postgresAdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	dsn := fmt.Sprintf("postgres:"+"//%s:%s@%s:%d/%s?sslmode=disable", adminCfg.User, adminCfg.Password, adminCfg.Host, adminCfg.Port, name)
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return dsn, c
}

// Verification defect 2: tables with the store's names readable in PUBLIC must
// not be found and used. The role's search_path is "$user" only, so the store
// creates and uses its own tables in its own schema and leaves public's alone.
func TestWorkStoreNeverResolvesItsTablesInPublic(t *testing.T) {
	ctx := context.Background()
	wsDSN := newWorkStoreDatabase(t)
	wsCfg, err := pgx.ParseConfig(wsDSN)
	if err != nil {
		t.Fatal(err)
	}
	_, admin := newDatabaseConnFor(t, wsCfg.Database)
	for _, stmt := range []string{
		`CREATE TABLE public.workstore_meta (id BOOLEAN PRIMARY KEY, schema_version INT NOT NULL, bd_imported_at TIMESTAMPTZ)`,
		`INSERT INTO public.workstore_meta VALUES (TRUE, 99, now())`,
		`CREATE TABLE public.work_items (anchor TEXT PRIMARY KEY, revision TEXT, state TEXT, record JSONB, created_at TIMESTAMPTZ DEFAULT now(), written_at TIMESTAMPTZ DEFAULT now())`,
		`GRANT SELECT, INSERT, UPDATE ON public.workstore_meta, public.work_items TO ` + workStoreRole,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	s := openWorkStore(t, wsDSN)
	if done, err := s.BdImportCompleted(ctx); err != nil || done {
		t.Fatalf("BdImportCompleted = %v, %v: the store read public.workstore_meta's import marker", done, err)
	}
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StateRunning}); err != nil {
		t.Fatal(err)
	}
	var inPublic, inOwn int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM public.work_items`).Scan(&inPublic); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM gonk_workstore.work_items`).Scan(&inOwn); err != nil {
		t.Fatalf("no work_items in schema gonk_workstore: %v", err)
	}
	if inPublic != 0 || inOwn != 1 {
		t.Fatalf("rows: public.work_items %d, gonk_workstore.work_items %d; want 0 and 1", inPublic, inOwn)
	}
}

// Verification defect 4: a database migrated at schema version 1 (the
// superseded build) gains the version-2 additions on open -- and the
// version-3 one (gonk-tios R6: the open-pipeline-run partial index).
func TestWorkStoreMigratesAVersionOneSchema(t *testing.T) {
	ctx := context.Background()
	wsDSN := newWorkStoreDatabase(t)
	c := rawConn(t, wsDSN)
	for _, stmt := range []string{
		`CREATE TABLE work_items (anchor TEXT PRIMARY KEY, revision TEXT NOT NULL, state TEXT NOT NULL, record JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), written_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE workstore_meta (id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id), schema_version INT NOT NULL, bd_imported_at TIMESTAMPTZ)`,
		`INSERT INTO workstore_meta (id, schema_version) VALUES (TRUE, 1)`,
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	openWorkStore(t, wsDSN)
	var v int
	var quarantine, check, runIdx bool
	if err := c.QueryRow(ctx, `SELECT schema_version,
		to_regclass('work_items_quarantine') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'work_items_revision_set'),
		to_regclass('work_items_pipeline_run_open_idx') IS NOT NULL
		FROM workstore_meta`).Scan(&v, &quarantine, &check, &runIdx); err != nil {
		t.Fatal(err)
	}
	if v != 3 || !quarantine || !check || !runIdx {
		t.Fatalf("after open: schema_version %d, quarantine table %v, revision CHECK %v, open-run index %v; want 3, true, true, true",
			v, quarantine, check, runIdx)
	}
}
