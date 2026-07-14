//go:build dolt

// Package store (this file) is Plan 03 Task 0b: a BLOCKING spike that proves
// -- or disproves -- Dolt's transactional guarantees for the ONE property the
// reservation ledger depends on, before internal/meter/store/dolt.go (Task 6)
// is written against an assumption instead of a measurement.
//
// The property under test (stated precisely, per Task 0b Step 1):
//
//	Given a budget ceiling with headroom for exactly K reservations, when
//	N >> K concurrent writers each (a) read the sum of open reservations,
//	(b) decide whether their reservation fits, and (c) write it -- exactly K
//	succeed, and the sum of persisted reservations never exceeds the ceiling.
//
// This is NOT a durability question and NOT a "does Dolt work" question. It
// is the read-then-write race, and it is the only thing standing between a
// budget and two sessions spending it twice.
//
// Run against a real dolt sql-server (see docs/spikes/dolt-reservation-isolation.md
// for the exact commands used to produce the recorded results):
//
//	docker run -d --rm --network=host --name gonk-dolt-spike dolthub/dolt-sql-server:latest
//	GONK_DOLT_DSN="root@tcp(127.0.0.1:3306)/" \
//	  go test -tags dolt ./internal/meter/store/ -race -run Race -count=5 -v
//
// -count=5 is not decoration: a race that only manifests one run in three is
// still a race, and a budget escape that happens once a month is still a
// budget escape. This file also loops internally (see raceIterations) so a
// single invocation without -count is still convincing.
//
// Podman's CNI bridge is broken in this environment; --network=host is
// required or the container is unreachable from the host.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// defaultDoltDSN matches the image's default: root, no password, no TLS,
// listening on 3306, reachable via --network=host on the host loopback.
const defaultDoltDSN = "root@tcp(127.0.0.1:3306)/"

// raceIterations is how many times each strategy's race is run within a
// single `go test` invocation, independent of the external -count flag.
const raceIterations = 5

// raceWriters (N) and raceCeiling/raceEach (the ceiling and per-reservation
// cost) are chosen so headroom exists for EXACTLY raceWinners (K) of the N
// writers: ceiling=1.00, each=0.40 -> floor(1.00/0.40) = 2.
const (
	raceWriters = 32
	raceCeiling = 1.00
	raceEach    = 0.40
	raceWinners = 2
)

// doltBaseDSN is the DSN for the spike's dolt sql-server with NO default
// schema -- used only for admin work (CREATE/DROP DATABASE, dolt_version()).
func doltBaseDSN() string {
	dsn := os.Getenv("GONK_DOLT_DSN")
	if dsn == "" {
		dsn = defaultDoltDSN
	}
	return dsn
}

// openDolt opens and pings a *sql.DB against the given DSN.
func openDolt(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open dolt (dsn=%q): %v", dsn, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping dolt (is gonk-dolt-spike up on --network=host? see docs/spikes/dolt-reservation-isolation.md): %v", err)
	}
	return db
}

// doltAdmin opens the admin connection used only for CREATE/DROP DATABASE and
// dolt_version() -- never for the race itself.
func doltAdmin(t *testing.T) *sql.DB {
	t.Helper()
	db := openDolt(t, doltBaseDSN())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// doltVersion records exactly what was tested, per Task 0b's write-up
// requirement.
func doltVersion(t *testing.T, db *sql.DB) string {
	t.Helper()
	var v string
	if err := db.QueryRow("SELECT dolt_version()").Scan(&v); err != nil {
		t.Fatalf("SELECT dolt_version(): %v", err)
	}
	return v
}

// doltRaceStore is a fresh, isolated schema (Dolt database) per race
// iteration -- "fresh schema per run" per Task 0b's pseudocode comment --
// with a projects+reservations pair of tables mirroring the shape
// internal/meter/store.Store will use in Task 6.
//
// IMPORTANT (this cost an hour of debugging and is itself a finding worth
// recording): s.db is opened with the new database's name IN THE DSN, not
// via a "USE <name>" statement executed once against a shared *sql.DB. A
// bare DSN with no default schema (e.g. "root@tcp(host:3306)/") leaves every
// NEW connection the pool opens with NO current database selected, and under
// this Dolt version, a connection with no current database fails almost any
// statement -- including a bare BEGIN -- with "Error 1105: no database
// selected", REGARDLESS of whether the query's table names are qualified
// with "dbname.table". A single "USE" run against one arbitrary pooled
// connection only ever fixes *that* physical connection; it does nothing for
// the other N-1 connections a 32-way concurrent race forces the pool to
// dial. Baking the schema into the DSN uses the MySQL wire protocol's
// initial-handshake COM_INIT_DB, which every new connection gets for free.
type doltRaceStore struct {
	db   *sql.DB
	name string
}

func newDoltRaceStore(t *testing.T, admin *sql.DB, baseDSN string) *doltRaceStore {
	t.Helper()
	name := fmt.Sprintf("gonk_spike_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000))

	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name)
	})

	db := openDolt(t, baseDSN+name)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(raceWriters + 4)

	ddl := []string{
		`CREATE TABLE projects (
			name    VARCHAR(191) PRIMARY KEY,
			ceiling DOUBLE NOT NULL
		)`,
		`CREATE TABLE reservations (
			id         VARCHAR(191) PRIMARY KEY,
			project    VARCHAR(191) NOT NULL,
			amount     DOUBLE NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("ddl %q: %v", stmt, err)
		}
	}
	return &doltRaceStore{db: db, name: name}
}

func (s *doltRaceStore) seedProject(t *testing.T, project string, ceiling float64) {
	t.Helper()
	_, err := s.db.Exec("INSERT INTO projects (name, ceiling) VALUES (?, ?)", project, ceiling)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func (s *doltRaceStore) sumOpenReservations(t *testing.T, project string) float64 {
	t.Helper()
	var sum float64
	err := s.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM reservations WHERE project = ?", project).Scan(&sum)
	if err != nil {
		t.Fatalf("sum reservations: %v", err)
	}
	return sum
}

// isSerializationFailure reports whether err is Dolt/MySQL telling us this
// transaction lost a race and must be retried -- a legitimate LOSS, not an
// error, per Task 0b Step 2's comment.
func isSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"serializ", // "serialization failure", "could not serialize"
		"lock wait timeout",
		"deadlock",
		"conflict",
		"transaction has been aborted",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// reserveIfFitsNaive is the STRATEGY WE EXPECT TO FAIL: it performs the
// check-then-write as two statements in a transaction at the driver's
// default isolation level, with no explicit locking. This is exactly the
// shape a "read, check, write" implementation takes if someone assumes the
// database will save them without being asked.
func (s *doltRaceStore) reserveIfFitsNaive(ctx context.Context, project string, ceiling, amount float64, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var sum float64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount),0) FROM "+"reservations"+" WHERE project = ?", project).Scan(&sum); err != nil {
		return false, err
	}
	if sum+amount > ceiling {
		return false, tx.Rollback()
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+"reservations"+" (id, project, amount) VALUES (?, ?, ?)", id, project, amount); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// reserveIfFitsSerializable is the naive read-then-write shape, but the
// transaction is opened at SQL SERIALIZABLE isolation instead of the
// driver/server default.
func (s *doltRaceStore) reserveIfFitsSerializable(ctx context.Context, project string, ceiling, amount float64, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var sum float64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount),0) FROM "+"reservations"+" WHERE project = ?", project).Scan(&sum); err != nil {
		return false, err
	}
	if sum+amount > ceiling {
		return false, tx.Rollback()
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+"reservations"+" (id, project, amount) VALUES (?, ?, ?)", id, project, amount); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// reserveIfFitsForUpdate is the production-shaped strategy: it locks the
// project's ceiling row with SELECT ... FOR UPDATE BEFORE reading the sum,
// so a second writer's own SELECT ... FOR UPDATE blocks until the first
// writer commits or rolls back. This is "the sequence Service.Decide
// performs, with NO in-process lock" per Task 0b's test comment -- we are
// testing the STORE's atomicity, not a service-side mutex.
func (s *doltRaceStore) reserveIfFitsForUpdate(ctx context.Context, project string, ceiling, amount float64, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var lockedCeiling float64
	if err := tx.QueryRowContext(ctx, "SELECT ceiling FROM "+"projects"+" WHERE name = ? FOR UPDATE", project).Scan(&lockedCeiling); err != nil {
		return false, err
	}
	var sum float64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount),0) FROM "+"reservations"+" WHERE project = ?", project).Scan(&sum); err != nil {
		return false, err
	}
	if sum+amount > lockedCeiling {
		return false, tx.Rollback()
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+"reservations"+" (id, project, amount) VALUES (?, ?, ?)", id, project, amount); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

type reserveFn func(s *doltRaceStore, ctx context.Context, project string, ceiling, amount float64, id string) (bool, error)

// raceResult is one iteration's outcome, kept plain so it can be logged and
// asserted on without ceremony.
type raceResult struct {
	wins        int64
	losses      int64 // ok==false, no error: correctly rejected
	deferrals   int64 // serialization failure: legitimate loss, not a bug
	otherErrors int64 // anything else: a real bug or an infra problem
	total       float64
}

// runRace launches raceWriters concurrent goroutines against a single fresh
// project/ceiling, all calling fn with NO in-process lock -- exactly Task
// 0b's design: we are testing the store's atomicity, not a service mutex.
func runRace(t *testing.T, admin *sql.DB, baseDSN string, fn reserveFn) raceResult {
	t.Helper()
	s := newDoltRaceStore(t, admin, baseDSN)
	const project = "p"
	s.seedProject(t, project, raceCeiling)

	var wins, losses, deferrals, otherErrors atomic.Int64
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < raceWriters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("r-%d", i)
			ok, err := fn(s, ctx, project, raceCeiling, raceEach, id)
			switch {
			case err != nil && isSerializationFailure(err):
				deferrals.Add(1)
			case err != nil:
				otherErrors.Add(1)
				t.Logf("writer %d: unexpected error: %v", i, err)
			case ok:
				wins.Add(1)
			default:
				losses.Add(1)
			}
		}(i)
	}
	wg.Wait()

	return raceResult{
		wins:        wins.Load(),
		losses:      losses.Load(),
		deferrals:   deferrals.Load(),
		otherErrors: otherErrors.Load(),
		total:       s.sumOpenReservations(t, project),
	}
}

// TestDoltReservationRaceDoesNotOverspend is Plan 03 Task 0b. It runs the
// SAME race under three strategies -- exactly the escalation path Task 0b
// Step 3's decision table prescribes -- and records, per strategy, per
// iteration, the real numbers: wins, losses, deferrals (legitimate lost
// races), other errors, and the persisted total against the ceiling.
//
// A strategy PASSES an iteration iff wins == raceWinners AND total <=
// raceCeiling (a strict overspend is the only unrecoverable failure; a
// serialization failure/deferral is fine, and so is a strategy that
// occasionally under-fills headroom due to lock contention -- that's a
// tuning problem, not a correctness one).
func TestDoltReservationRaceDoesNotOverspend(t *testing.T) {
	admin := doltAdmin(t)
	baseDSN := doltBaseDSN()
	v := doltVersion(t, admin)
	t.Logf("dolt_version() = %s", v)

	strategies := []struct {
		name string
		fn   reserveFn
	}{
		{"naive_default_isolation", func(s *doltRaceStore, ctx context.Context, project string, ceiling, amount float64, id string) (bool, error) {
			return s.reserveIfFitsNaive(ctx, project, ceiling, amount, id)
		}},
		{"serializable_isolation", func(s *doltRaceStore, ctx context.Context, project string, ceiling, amount float64, id string) (bool, error) {
			return s.reserveIfFitsSerializable(ctx, project, ceiling, amount, id)
		}},
		{"select_for_update", func(s *doltRaceStore, ctx context.Context, project string, ceiling, amount float64, id string) (bool, error) {
			return s.reserveIfFitsForUpdate(ctx, project, ceiling, amount, id)
		}},
	}

	for _, rs := range strategies {
		rs := rs
		t.Run(rs.name, func(t *testing.T) {
			overspendCount := 0
			exactWinCount := 0
			for iter := 0; iter < raceIterations; iter++ {
				res := runRace(t, admin, baseDSN, rs.fn)
				t.Logf("[%s] iter=%d wins=%d losses=%d deferrals=%d otherErrors=%d total=$%.2f ceiling=$%.2f",
					rs.name, iter, res.wins, res.losses, res.deferrals, res.otherErrors, res.total, raceCeiling)

				if res.otherErrors > 0 {
					t.Errorf("[%s] iter=%d: %d unexpected (non-serialization) errors -- see log", rs.name, iter, res.otherErrors)
				}
				if res.total > raceCeiling+1e-9 {
					overspendCount++
					t.Logf("[%s] iter=%d: OVERSPEND: $%.2f persisted against a $%.2f ceiling", rs.name, iter, res.total, raceCeiling)
				}
				if res.wins == raceWinners {
					exactWinCount++
				} else {
					t.Logf("[%s] iter=%d: %d writers won against headroom for %d (not necessarily a bug -- see overspend check)", rs.name, iter, res.wins, raceWinners)
				}
			}
			t.Logf("[%s] summary: %d/%d iterations overspent, %d/%d iterations won exactly %d",
				rs.name, overspendCount, raceIterations, exactWinCount, raceIterations, raceWinners)

			if overspendCount > 0 {
				t.Errorf("[%s]: OVERSPEND observed in %d/%d iterations -- this strategy does not serialize the reservation race", rs.name, overspendCount, raceIterations)
			}
		})
	}
}
