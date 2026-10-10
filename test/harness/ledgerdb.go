package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Postgres is a REAL Postgres server in a container, shared by a component-test
// binary. It backs TWO independent things that must never share a database:
// LiteLLM's own spend log (database `litellm`) and gonk-meter's reservation
// ledger (a FRESH database per test, via FreshLedger).
//
// The ledger is Postgres-ONLY (Plan 06 reconciliation D2): Dolt failed the
// reservation-race isolation spike, the owner settled on Postgres, and
// GONK_METER_STORE_BACKEND accepts only "postgres". There is no dual-backend
// loop here and no GONK_LEDGER switch.
//
// FRESH DB PER TEST is not hygiene, it is correctness: a reservation leaked from
// a previous test is a false PASS on a budget assertion (headroom that should be
// gone is still there). Every ledger test gets its own empty database; meter's
// store.NewPostgres applies the schema itself (CREATE TABLE IF NOT EXISTS).
type Postgres struct {
	// DSNFor builds a connection string for a named database.
	container string
	rt        *Runtime
	user      string
	password  string
	port      int
	seq       atomic.Int64
}

// StartPostgres boots postgres:16 on the given loopback port (pick a free one --
// the dev box's 5432 is often occupied; the L1 fix work used 5544). It waits for
// readiness and ensures the `litellm` database exists. Returns the handle and a
// stop func the caller invokes at teardown.
func StartPostgres(rt *Runtime, c *Creds, image string, port int) (_ *Postgres, stop func(), err error) {
	password, err := RandomToken(16)
	if err != nil {
		return nil, nil, err
	}
	name := "gonk-l2-pg-" + shortID()
	args := []string{"run", "-d", "--name", name}
	if rt.HostNetwork {
		args = append(args, "--network=host")
	}
	args = append(args,
		"-e", "POSTGRES_USER=gonk",
		"-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB=postgres",
		image, "-p", itoa(port),
	)
	if out, rerr := exec.Command(rt.Bin, args...).CombinedOutput(); rerr != nil {
		return nil, nil, fmt.Errorf("harness: start postgres: %v: %s", rerr, out)
	}
	stop = func() { _ = exec.Command(rt.Bin, "rm", "-f", name).Run() }
	defer func() {
		if err != nil {
			stop()
		}
	}()

	p := &Postgres{container: name, rt: rt, user: "gonk", password: password, port: port}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if werr := WaitFor(ctx, "postgres ready", 60*time.Second, func() (bool, error) {
		out, e := exec.Command(rt.Bin, "exec", name, "pg_isready", "-U", "gonk", "-p", itoa(port)).CombinedOutput()
		return e == nil && strings.Contains(string(out), "accepting connections"), nil
	}); werr != nil {
		return nil, nil, werr
	}

	if err = p.createDB(litellmDBName); err != nil {
		return nil, nil, fmt.Errorf("harness: create litellm db: %w", err)
	}
	return p, stop, nil
}

// DSN is a connection string for a database in this server (loopback, sslmode
// disabled). This is the value for GONK_METER_STORE_DSN_FILE / DATABASE_URL.
func (p *Postgres) DSN(db string) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", p.user, p.password, p.port, db)
}

// BaseDSN is a DSN whose database name can be swapped by dsnWithDB (LiteLLM's
// DATABASE_URL is built from this).
func (p *Postgres) BaseDSN() string { return p.DSN("postgres") }

// FreshLedger creates a brand-new empty database and returns its DSN. Each ledger
// test calls this so no reservation ever leaks across tests. The database is left
// behind (the whole container is torn down at package teardown), which is fine
// and far cheaper than a container per test.
func (p *Postgres) FreshLedger(t testing.TB) string {
	t.Helper()
	name := fmt.Sprintf("ledger_%d_%s", p.seq.Add(1), shortID())
	if err := p.createDB(name); err != nil {
		t.Fatalf("harness: FreshLedger create %q: %v", name, err)
	}
	return p.DSN(name)
}

func (p *Postgres) createDB(name string) error {
	out, err := exec.Command(p.rt.Bin, "exec",
		"-e", "PGPASSWORD="+p.password,
		p.container, "psql", "-U", p.user, "-h", "127.0.0.1", "-p", itoa(p.port),
		"-d", "postgres", "-c", "CREATE DATABASE "+name+";",
	).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
