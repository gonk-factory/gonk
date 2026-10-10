//go:build integration

// The work store against a REAL Postgres (gonk-t41). Build-tagged `integration`,
// run by the integration job in .github/workflows/ci.yml, which provides
// postgres:16. Locally:
//
//	docker run -d --rm -p 127.0.0.1:5432:5432 -e POSTGRES_PASSWORD=gonk postgres:16
//	GONK_POSTGRES_DSN="postgres://postgres:gonk@127.0.0.1:5432/postgres?sslmode=disable" \
//	  go test -tags integration ./pkg/beadstore/ -race -count=10
//
// Every test gets its own freshly created database, the pattern
// internal/meter/store/postgres_test.go established (and its notes on why the
// database is named in the DSN rather than switched to on a pooled connection).
package beadstore_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/beadstore/storetest"
	"github.com/gonk-factory/gonk/test/harness"
)

const defaultPostgresDSN = "postgres://postgres:gonk@127.0.0.1:5432/postgres?sslmode=disable"

func postgresAdminDSN() string {
	if dsn := os.Getenv("GONK_POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return defaultPostgresDSN
}

// The role the work store runs as, as on the reference cluster: a login role owning a schema of
// its own name, and nothing else. Roles are cluster-wide, so it is created once
// per server (idempotently -- test packages run concurrently).
// workStoreRolePass IS DELIBERATELY HOSTILE, and must stay that way (gonk-ajzq).
//
// On 2026-09-19 the live cutover crash-looped for 11 minutes because the
// gonk_workstore password Vault generated contains a SPACE, which is illegal
// unescaped in a URL's userinfo: pgx rejected the whole DSN with "failed to
// parse as URL (net/url: invalid userinfo)". Four rounds of verification --
// including container runs against a real Postgres -- missed it, for one
// reason: every fixture used a friendly password. This one did too; it was
// "gonk-workstore-test".
//
// So the fixture now carries a space, a single quote and a backslash, and
// newWorkStoreDatabase hands OpenPostgres the keyword/value DSN the gitops
// ExternalSecret renders. The whole integration suite is now the regression
// test for that class, rather than one case beside it.
const (
	workStoreRole     = "gonk_workstore"
	workStoreRolePass = `gonk ws'test\pass`
)

// pgQuoteLiteral quotes s as a Postgres string literal (doubling quotes), for
// the CREATE ROLE that cannot take a bind parameter.
func pgQuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// dsnWith appends `key=value` settings to a keyword/value DSN. Tests used to
// write `dsn+"&k=v"`, which is URL query syntax: correct while the fixture DSN
// was a URL, invalid the moment it stopped being one. Separator, not syntax.
func dsnWith(dsn string, settings ...string) string {
	return dsn + " " + strings.Join(settings, " ")
}

// kvDSN builds the keyword/value DSN, escaping exactly as pgconn's parser
// defines -- the form clusters/example/apps/gonk/externalsecret-gonk-workstore.yaml
// renders, chosen over a URL because a URL cannot carry this password.
func kvDSN(user, pass, host string, port uint16, database string) string {
	esc := strings.ReplaceAll(pass, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `'`, `\'`)
	return fmt.Sprintf("host=%s port=%d user=%s password='%s' dbname=%s sslmode=disable",
		host, port, user, esc, database)
}

// newWorkStoreDatabase creates an empty database for one test, with schema
// gonk_workstore owned by role gonk_workstore, and returns a DSN that connects
// AS gonk_workstore with no search_path -- exactly the production shape.
func newWorkStoreDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	adminDSN, admin := newDatabase(t)
	// The password is a hostile literal (see workStoreRolePass), so it is quoted
	// rather than concatenated, and ALTER follows CREATE: the role is
	// cluster-wide and may survive from an older run that used the friendly one.
	if _, err := admin.Exec(ctx, `DO $$ BEGIN
		CREATE ROLE `+workStoreRole+` LOGIN PASSWORD `+pgQuoteLiteral(workStoreRolePass)+`;
	EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`); err != nil {
		t.Fatalf("create role %s: %v", workStoreRole, err)
	}
	if _, err := admin.Exec(ctx, `ALTER ROLE `+workStoreRole+` PASSWORD `+pgQuoteLiteral(workStoreRolePass)); err != nil {
		t.Fatalf("alter role %s password: %v", workStoreRole, err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+workStoreRole+` AUTHORIZATION `+workStoreRole); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	return kvDSN(workStoreRole, workStoreRolePass, cfg.Host, cfg.Port, cfg.Database)
}

// newDatabase creates an empty database, returning an ADMIN DSN for it and an
// admin connection to it. Dropped at cleanup (after every connection to it
// closes: cleanups run LIFO, and callers register their Close after this).
func newDatabase(t *testing.T) (string, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	adminCfg, err := pgx.ParseConfig(postgresAdminDSN())
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	harness.RequireInfra(t, fmt.Sprintf("postgres admin connection (see the top of postgres_test.go): %v", err), err == nil)
	name := fmt.Sprintf("gonk_ws_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		// WITH (FORCE): a killed-client test can leave a backend the server has
		// not reaped yet.
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
		_ = admin.Close(context.Background())
	})
	dsn := fmt.Sprintf("postgres:"+"//%s:%s@%s:%d/%s?sslmode=disable",
		adminCfg.User, adminCfg.Password, adminCfg.Host, adminCfg.Port, name)
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return dsn, c
}

func openWorkStore(t *testing.T, dsn string) *beadstore.Postgres {
	t.Helper()
	s, err := beadstore.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func newWorkStore(t *testing.T) (*beadstore.Postgres, string) {
	t.Helper()
	dsn := newWorkStoreDatabase(t)
	return openWorkStore(t, dsn), dsn
}

func rawConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dsn, err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestPostgresConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) beadstore.Store { s, _ := newWorkStore(t); return s })
	storetest.RunCompareAndSwap(t, func(t *testing.T) beadstore.Store { s, _ := newWorkStore(t); return s })
}

// PutIfSuccessIsFinal, harder: more writers, more rounds, several anchors at
// once, so the row lock is contended and the pool is exhausted (writers queue
// for connections as well as for the row).
func TestPostgresManyWritersOneChain(t *testing.T) {
	s, _ := newWorkStore(t)
	ctx := context.Background()
	const writers, rounds = 32, 50
	anchors := []string{"gonk:1:issue:1", "gonk:1:issue:2", "gonk:1:issue:3"}

	type win struct {
		attempt int
		rev, by string
	}
	var mu sync.Mutex
	wins := map[string][]win{}
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				anchor := anchors[(w+i)%len(anchors)]
				cur, found, err := s.Get(ctx, anchor)
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				next := beadstore.Record{BeadAnchor: anchor, State: beadstore.StateRunning, Attempt: 1,
					SessionID: fmt.Sprintf("w%d.r%d", w, i)}
				ifRev := ""
				if found {
					next.Attempt, ifRev = cur.Attempt+1, cur.Revision
				}
				rev, err := s.PutIf(ctx, next, ifRev)
				switch {
				case err == nil:
					mu.Lock()
					wins[anchor] = append(wins[anchor], win{next.Attempt, rev, next.SessionID})
					mu.Unlock()
				case errors.Is(err, beadstore.ErrStale):
				default:
					t.Errorf("PutIf = %v, want applied or ErrStale", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	total := 0
	for _, anchor := range anchors {
		ws := wins[anchor]
		total += len(ws)
		sort.Slice(ws, func(i, j int) bool { return ws[i].attempt < ws[j].attempt })
		for i, w := range ws {
			if w.attempt != i+1 {
				t.Fatalf("%s: applied attempts %v are not the chain 1..%d", anchor, ws, len(ws))
			}
		}
		if len(ws) == 0 {
			t.Fatalf("%s: nothing applied", anchor)
		}
		got, ok, err := s.Get(ctx, anchor)
		last := ws[len(ws)-1]
		if err != nil || !ok || got.Revision != last.rev || got.SessionID != last.by || got.Attempt != last.attempt {
			t.Fatalf("%s: stored %+v, want the last applied write %+v", anchor, got, last)
		}
	}
	t.Logf("%d writers x %d rounds: %d writes applied as one chain per anchor, the rest refused", writers, rounds, total)
}

// gonk-9sf9 made impossible, not merely unlikely: the state column and the
// record's State are written by one statement, and a write that tries to make
// them disagree is refused by the database itself.
func TestPostgresStateColumnCannotDisagreeWithTheRecord(t *testing.T) {
	s, dsn := newWorkStore(t)
	ctx := context.Background()
	rec := beadstore.Record{BeadAnchor: "gonk:1:issue:9", State: beadstore.StateRunning}
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	c := rawConn(t, dsn)
	for _, stmt := range []string{
		`UPDATE work_items SET state = 'parked' WHERE anchor = 'gonk:1:issue:9'`,
		`UPDATE work_items SET revision = 'x' WHERE anchor = 'gonk:1:issue:9'`,
		`UPDATE work_items SET anchor = 'gonk:1:issue:10' WHERE anchor = 'gonk:1:issue:9'`,
		`UPDATE work_items SET record = jsonb_set(record, '{State}', '"done"') WHERE anchor = 'gonk:1:issue:9'`,
	} {
		_, err := c.Exec(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "work_items_record_agrees") {
			t.Fatalf("%s = %v, want the work_items_record_agrees CHECK to refuse it", stmt, err)
		}
	}
	if recs, err := s.List(ctx, beadstore.StateRunning); err != nil || len(recs) != 1 {
		t.Fatalf("List(running) = %+v, %v after refused writes", recs, err)
	}
}

// List is an index range scan. With so few rows the planner would pick a
// sequential scan or a bitmap scan plus a sort anyway, so this switches those
// plans off and asserts the index is USABLE for List's exact query, order
// included: an Index Scan on work_items_state_idx and no Sort node.
func TestPostgresListUsesTheStateIndex(t *testing.T) {
	s, dsn := newWorkStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if err := s.Put(ctx, beadstore.Record{BeadAnchor: fmt.Sprintf("gonk:1:issue:%d", i), State: beadstore.StateParked}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	c := rawConn(t, dsn)
	for _, off := range []string{"enable_seqscan", "enable_bitmapscan", "enable_sort"} {
		if _, err := c.Exec(ctx, "SET "+off+" = off"); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := c.Query(ctx, `EXPLAIN SELECT record FROM work_items WHERE state = 'parked' ORDER BY anchor COLLATE "C"`)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "Index Scan using work_items_state_idx") || strings.Contains(joined, "Sort") {
		t.Fatalf("List's query does not use work_items_state_idx for both filter and order:\n%s", joined)
	}
}

// The database's own collation must not decide the order: Memory sorts by
// byte order, and so does List.
func TestPostgresListIsInAnchorByteOrder(t *testing.T) {
	s, _ := newWorkStore(t)
	ctx := context.Background()
	anchors := []string{"gonk:9:issue:1", "gonk:10:issue:1", "gonk:1:issue:20", "gonk:1:issue:3", "Gonk:1:issue:3", "gonk-1"}
	for _, a := range anchors {
		if err := s.Put(ctx, beadstore.Record{BeadAnchor: a, State: beadstore.StateParked}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	recs, err := s.List(ctx, beadstore.StateParked)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), anchors...)
	sort.Strings(want)
	var got []string
	for _, r := range recs {
		got = append(got, r.BeadAnchor)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("List order %v, want %v", got, want)
	}
}

// Every gonk-gate process opens the store. The first ones after a deploy race
// to create the schema; none may fail, and the schema must come out whole.
func TestPostgresSchemaOpenIsIdempotentAndConcurrent(t *testing.T) {
	dsn := newWorkStoreDatabase(t)
	ctx := context.Background()
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := beadstore.OpenPostgres(ctx, dsn)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent OpenPostgres: %v", err)
	}
	s := openWorkStore(t, dsn) // and once more, on the fast path
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StateDone}); err != nil {
		t.Fatalf("Put after concurrent opens: %v", err)
	}
	var v int
	if err := rawConn(t, dsn).QueryRow(ctx, `SELECT schema_version FROM workstore_meta`).Scan(&v); err != nil || v != 3 {
		t.Fatalf("schema_version = %d, %v", v, err)
	}
}

// The connection parameters that make a killed writer unable to land late are
// in force on the store's own connections (postgres_kill_test.go tests their
// effect; this pins that the DSN cannot silently drop them).
func TestPostgresSessionParametersAreApplied(t *testing.T) {
	dsn := newWorkStoreDatabase(t)
	// A DSN that asks for the opposite must not win.
	s := openWorkStore(t, dsnWith(dsn, "lock_timeout=0", "client_connection_check_interval=0"))
	ctx := context.Background()
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StateDone}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range []string{"client_connection_check_interval", "lock_timeout", "statement_timeout", "idle_in_transaction_session_timeout"} {
		v, err := beadstore.ShowSetting(ctx, s, p)
		if err != nil {
			t.Fatalf("SHOW %s: %v", p, err)
		}
		got = append(got, p+"="+v)
	}
	want := "client_connection_check_interval=1s lock_timeout=5s statement_timeout=15s idle_in_transaction_session_timeout=15s"
	if strings.Join(got, " ") != want {
		t.Fatalf("session parameters %q, want %q", strings.Join(got, " "), want)
	}
}

// TestPostgresStoreWriteLatency MEASURES what one store operation costs, for
// revisiting gonk-alw's 50s reserve for "one bead-store write" -- sized on
// BdCLI, whose Put was five bd processes and PutIf about nine, 12-57s under the
// Dolt pressure of 2026-09-13. It logs percentiles; its only assertion is a
// deliberately loose ceiling that catches a pathological regression (a lock
// wait, a DDL on every open) without turning CI scheduling noise into red.
//
// Open is measured as gonk-gate pays it: a new pool, connect, ping, and the
// schema fast path -- every dispatch and sweep process does exactly this once.
func TestPostgresStoreWriteLatency(t *testing.T) {
	s, dsn := newWorkStore(t)
	ctx := context.Background()
	const n = 300
	for i := 0; i < 200; i++ { // a List with something to return
		if err := s.Put(ctx, beadstore.Record{BeadAnchor: fmt.Sprintf("gonk:2:issue:%d", i), State: beadstore.StateRunning}); err != nil {
			t.Fatal(err)
		}
	}
	measure := func(name string, op func(i int) error) {
		ds := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			start := time.Now()
			if err := op(i); err != nil {
				t.Fatalf("%s #%d: %v", name, i, err)
			}
			ds = append(ds, time.Since(start))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		pct := func(p float64) time.Duration { return ds[int(p*float64(len(ds)-1))].Round(10 * time.Microsecond) }
		t.Logf("%-6s n=%d p50=%v p95=%v p99=%v max=%v", name, n, pct(0.50), pct(0.95), pct(0.99), ds[len(ds)-1].Round(10*time.Microsecond))
		if ds[len(ds)/2] > 250*time.Millisecond {
			t.Errorf("%s median %v: a store operation should take milliseconds", name, ds[len(ds)/2])
		}
	}
	measure("Open", func(int) error {
		p, err := beadstore.OpenPostgres(ctx, dsn)
		if err == nil {
			p.Close()
		}
		return err
	})
	rev := ""
	measure("PutIf", func(i int) error {
		r, err := s.PutIf(ctx, beadstore.Record{BeadAnchor: "gonk:3:issue:1", State: beadstore.StatePendingPrompt, Attempt: i}, rev)
		rev = r
		return err
	})
	measure("Put", func(i int) error {
		return s.Put(ctx, beadstore.Record{BeadAnchor: "gonk:3:issue:2", State: beadstore.StateRunning, Attempt: i})
	})
	measure("Get", func(int) error { _, _, err := s.Get(ctx, "gonk:3:issue:2"); return err })
	measure("List", func(int) error { _, err := s.List(ctx, beadstore.StateRunning); return err })
}

// THE THIRD OUTCOME. ErrStale means "nothing was written, and never will be" --
// callers act on it as settled (a dispatch re-reads and may back off; a release
// leaves the record alone). A write that FAILED has not been refused: it may
// have committed on the server after the client lost the reply. So every error
// that is not a zero-rows refusal must come back as something other than
// ErrStale, which callers already treat as an unknown outcome
// (broker_inject.go reservationUnknown). Two real failures here: the caller's
// context ending while the statement waits on a lock (a row lock for PutIf(rev)
// and Put; an uncommitted insert of the same anchor for PutIf(""), which is
// what an INSERT ... ON CONFLICT waits on), and the store's own lock_timeout.
func TestPostgresAFailedWriteIsNeverReportedStale(t *testing.T) {
	s, dsn := newWorkStore(t)
	ctx := context.Background()
	const anchor = "gonk:1:issue:1"
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: anchor, State: beadstore.StateRunning}); err != nil {
		t.Fatal(err)
	}
	rev := mustGetRevision(t, s, anchor)
	locker := rawConn(t, dsn)
	tx, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM work_items WHERE anchor = $1 FOR UPDATE`, anchor); err != nil {
		t.Fatal(err)
	}
	const inserting = "gonk:1:issue:2"
	if _, err := tx.Exec(ctx, `INSERT INTO work_items (anchor, revision, state, record)
		VALUES ($1, 'r', 'running', jsonb_build_object('BeadAnchor', $1::text, 'State', 'running', 'Revision', 'r'))`, inserting); err != nil {
		t.Fatal(err)
	}

	next := beadstore.Record{BeadAnchor: anchor, State: beadstore.StateDone}
	fresh := beadstore.Record{BeadAnchor: inserting, State: beadstore.StatePendingPrompt}
	writes := map[string]func(context.Context) error{
		"PutIf(rev)":  func(c context.Context) error { _, err := s.PutIf(c, next, rev); return err },
		"PutIf(\"\")": func(c context.Context) error { _, err := s.PutIf(c, fresh, ""); return err },
		"Put":         func(c context.Context) error { return s.Put(c, next) },
	}
	for name, write := range writes {
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		err := write(cctx)
		cancel()
		if err == nil || errors.Is(err, beadstore.ErrStale) {
			t.Errorf("%s with its context ended while blocked = %v; want an error that is NOT ErrStale", name, err)
		}
	}
	start := time.Now()
	_, err = s.PutIf(ctx, next, rev) // no deadline: the store's lock_timeout (5s) ends it
	if err == nil || errors.Is(err, beadstore.ErrStale) || time.Since(start) > 10*time.Second {
		t.Errorf("PutIf past lock_timeout = %v after %v; want a non-stale error at ~5s", err, time.Since(start))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustGetRevision(t, s, anchor); got != rev {
		t.Fatalf("revision moved to %q after only failed writes", got)
	}
}

func mustGetRevision(t *testing.T, s *beadstore.Postgres, anchor string) string {
	t.Helper()
	r, ok, err := s.Get(context.Background(), anchor)
	if err != nil || !ok {
		t.Fatalf("Get(%s) = %v %v", anchor, ok, err)
	}
	return r.Revision
}

// gonk-upxg: the pool must be BOUNDED by the store, not by the box. pgxpool's
// default is max(4, NumCPU) per pool, and this suite opens one store per test
// while internal/meter/store opens its own pools in the same `go test ./...`
// run -- which peaked at 79 backends against postgres:16's 100-connection
// default and failed the integration job with SQLSTATE 53300 at -count=3.
func TestWorkStorePoolIsBoundedAndTheDSNCanStillChooseIt(t *testing.T) {
	dsn := newWorkStoreDatabase(t)
	if got := beadstore.PoolMaxConns(openWorkStore(t, dsn)); got != 8 {
		t.Errorf("pool MaxConns = %d, want 8 (the store's own ceiling, not the box's CPU count)", got)
	}
	// A DSN that asks wins -- INCLUDING when it asks for exactly pgxpool's own
	// default for this machine, which is the case a MaxConns comparison cannot
	// tell from silence (round-3 verification, N5). 3 alone never catches it.
	for _, want := range []int32{3, int32(runtime.NumCPU()), 4} {
		s, err := beadstore.OpenPostgres(context.Background(), dsnWith(dsn, fmt.Sprintf("pool_max_conns=%d", want)))
		if err != nil {
			t.Fatal(err)
		}
		if got := beadstore.PoolMaxConns(s); got != want {
			t.Errorf("pool MaxConns with pool_max_conns=%d in the DSN = %d; the DSN must win", want, got)
		}
		s.Close()
	}
}

// gonk-tios verify report R6: ListOpenPipelineRuns is answered from the
// partial index, never by scanning work_items. Its WHERE clause must stay the
// index's predicate exactly; if the two drifted, the planner would silently
// fall back to a scan of every record ever finished -- the cost the method
// exists to remove -- and nothing else would notice. Sequential scans are
// disabled so the answer does not depend on the table's (tiny) size.
func TestPostgresOpenPipelineRunsUseThePartialIndex(t *testing.T) {
	dsn := newWorkStoreDatabase(t)
	ctx := context.Background()
	openWorkStore(t, dsn)
	c := rawConn(t, dsn)
	if _, err := c.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, `EXPLAIN SELECT record FROM work_items WHERE `+beadstore.PipelineRunOpenPredicate+` ORDER BY anchor COLLATE "C"`)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	if p := strings.Join(plan, "\n"); !strings.Contains(p, "work_items_pipeline_run_open_idx") {
		t.Fatalf("ListOpenPipelineRuns' query does not use the open-run partial index:\n%s", p)
	}
}
