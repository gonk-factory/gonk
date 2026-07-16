//go:build integration

// Package store_test (this file) runs the SAME conformance suite Memory
// satisfies (internal/meter/store/memory_test.go) against a REAL Postgres
// server, so a behaviour Memory has by accident cannot hide behind green
// tests.
//
// This file is build-tagged `integration` so `go build ./...`, `go vet ./...`,
// `go test ./...`, and `golangci-lint run ./...` never touch it -- there is
// no Postgres in CI. Run it explicitly against a throwaway Postgres:
//
//	docker run -d --rm --network=host -e POSTGRES_PASSWORD=gonk postgres:16
//	GONK_POSTGRES_DSN="postgres://postgres:gonk@127.0.0.1:5432/postgres?sslmode=disable" \
//	  go test -tags integration ./internal/meter/store/ -race -v
//
// Podman's CNI bridge is broken in this environment (see
// docs/spikes/dolt-reservation-isolation.md); --network=host is required or
// the container is unreachable from the host.
package store_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/store/storetest"
)

// defaultPostgresDSN matches `docker run ... -e POSTGRES_PASSWORD=gonk
// postgres:16` reachable via --network=host on the host loopback: user
// postgres, password gonk, default "postgres" administrative database.
const defaultPostgresDSN = "postgres://postgres:gonk@127.0.0.1:5432/postgres?sslmode=disable"

func postgresAdminDSN() string {
	if dsn := os.Getenv("GONK_POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return defaultPostgresDSN
}

// newTestDatabase creates a fresh, uniquely-named Postgres database for one
// test, connects to it (the dbname baked into the DSN, not a `USE` statement
// run against a shared pool -- see the note in postgres_race_test.go about
// why a pooled connection needs the database in its own startup handshake),
// runs the schema migration, and registers cleanup.
func newTestDatabase(t *testing.T) *store.Postgres {
	t.Helper()
	ctx := context.Background()

	adminDSN := postgresAdminDSN()
	adminCfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("connect to postgres admin db (is it up? see the docstring at the top of this file): %v", err)
	}
	// admin stays open for the LIFE OF THE TEST, not just this setup call: the
	// DROP DATABASE below runs in a t.Cleanup, long after newTestDatabase has
	// returned, so closing admin here (e.g. via a bare `defer`) would close it
	// before that cleanup ever runs and every drop would silently fail with
	// "conn closed" -- which is exactly the bug this comment is here to keep
	// from coming back. admin.Close is itself registered as a (LIFO-last)
	// cleanup below, after the drop.
	name := fmt.Sprintf("gonk_test_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	// Cleanups run LIFO: this DROP DATABASE (registered first, so it runs
	// LAST of these two) must run AFTER s.Close (registered second, so it runs
	// FIRST) closes every connection s's pool holds open to this database --
	// Postgres refuses to drop a database with live connections.
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
		_ = admin.Close(context.Background())
	})

	dbDSN := dsnWithDatabase(adminCfg, name)
	s, err := store.NewPostgres(ctx, dbDSN)
	if err != nil {
		t.Fatalf("NewPostgres(%s): %v", name, err)
	}
	t.Cleanup(s.Close)
	return s
}

// dsnWithDatabase rebuilds a connection string pointed at a different
// database name, so every physical connection pgxpool opens gets the right
// database from the wire protocol's initial handshake (COM_INIT_DB
// equivalent), not from a `USE`/`SET search_path` run against one arbitrary
// pooled connection -- the latter only ever fixes that one connection, which
// a concurrent test would otherwise dial around. (This exact failure mode is
// documented at length in docs/spikes/dolt-reservation-isolation.md; it cost
// an hour of debugging there and is avoided here from the start.)
func dsnWithDatabase(cfg *pgx.ConnConfig, dbName string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, dbName)
}

func TestPostgresConformance(t *testing.T) {
	storetest.Run(t, func() store.Store { return newTestDatabase(t) })
}
