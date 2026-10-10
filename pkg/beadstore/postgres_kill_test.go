//go:build integration

// A KILLED WRITER CANNOT LAND LATE (gonk-t41). This is the property bd + Dolt
// lacked (gonk-vred item 7): a gonk-gate process SIGKILLed by gc at its order
// timeout could have its write commit afterwards, in front of or over writes
// made by the processes that came after it.
//
// Each test runs the writer as a REAL separate process -- this test binary
// re-executed in a helper mode (see TestMain) -- and kills it with SIGKILL, the
// signal gc sends two seconds after SIGTERM. Nothing is simulated: the server
// sees a client socket vanish.
package beadstore_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

const (
	killHelperMode   = "GONK_WS_KILL_HELPER"
	killHelperDSN    = "GONK_WS_KILL_DSN"
	killHelperAnchor = "GONK_WS_KILL_ANCHOR"
	killHelperRev    = "GONK_WS_KILL_REV"
	deadWriter       = "the-killed-writer"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(killHelperMode); mode != "" {
		os.Exit(killHelper(mode))
	}
	os.Exit(m.Run())
}

// killHelper is the writer process. It prints READY once it is exactly where
// the test wants to kill it, then waits to be killed.
func killHelper(mode string) int {
	ctx := context.Background()
	dsn, anchor, rev := os.Getenv(killHelperDSN), os.Getenv(killHelperAnchor), os.Getenv(killHelperRev)
	switch mode {
	case "tx-before-commit":
		// An explicit transaction that has done its write and not committed.
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			fmt.Println("ERROR", err)
			return 1
		}
		if _, err := c.Exec(ctx, `SET application_name = 'gonk-kill-helper'`); err != nil {
			fmt.Println("ERROR", err)
			return 1
		}
		tx, err := c.Begin(ctx)
		if err != nil {
			fmt.Println("ERROR", err)
			return 1
		}
		tag, err := tx.Exec(ctx, `
			UPDATE work_items SET
				revision = 'dead-revision', state = 'done',
				record = jsonb_set(jsonb_set(jsonb_set(record, '{Revision}', '"dead-revision"'), '{State}', '"done"'), '{SessionID}', to_jsonb($3::text))
			WHERE anchor = $1 AND revision = $2`, anchor, rev, deadWriter)
		if err != nil || tag.RowsAffected() != 1 {
			fmt.Println("ERROR update", err, tag)
			return 1
		}
		fmt.Println("READY")
	case "putif-blocked":
		// The real store's PutIf, which the test has made wait on a row lock.
		s, err := beadstore.OpenPostgres(ctx, dsn)
		if err != nil {
			fmt.Println("ERROR", err)
			return 1
		}
		fmt.Println("READY")
		_, err = s.PutIf(ctx, beadstore.Record{BeadAnchor: anchor, State: beadstore.StateDone, SessionID: deadWriter}, rev)
		fmt.Println("RETURNED", err) // never reached if the test kills us first
	default:
		fmt.Println("ERROR unknown mode", mode)
		return 1
	}
	time.Sleep(time.Hour) //nolint:forbidigo // helper process idles here until the test SIGKILLs it; nothing is timed
	return 0
}

// startKillHelper runs the helper and returns once it printed READY.
func startKillHelper(t *testing.T, mode, dsn, anchor, rev string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), killHelperMode+"="+mode, killHelperDSN+"="+dsn,
		killHelperAnchor+"="+anchor, killHelperRev+"="+rev)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			ready <- sc.Text()
		}
		close(ready)
	}()
	select {
	case line := <-ready:
		if line != "READY" {
			t.Fatalf("helper %s: %q", mode, line)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("helper %s never became ready", mode)
	}
	return cmd
}

func sigkill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	_ = cmd.Wait()
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // poll interval of a wait-until with a 3-15s deadline; not a window that is counted
	}
}

func countBackends(t *testing.T, c *pgx.Conn, where string, args ...any) int {
	t.Helper()
	var n int
	if err := c.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND `+where, args...).Scan(&n); err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	return n
}

// assertNothingLanded checks the record is still the one written before the
// killed writer ran -- and that its revision is still the one to condition on,
// which it could not be if any write had landed.
func assertNothingLanded(t *testing.T, s *beadstore.Postgres, anchor, rev string) {
	t.Helper()
	ctx := context.Background()
	got, ok, err := s.Get(ctx, anchor)
	if err != nil || !ok {
		t.Fatalf("Get = %+v %v %v", got, ok, err)
	}
	if got.Revision != rev || got.SessionID == deadWriter || got.State != beadstore.StateRunning {
		t.Fatalf("the killed writer's write LANDED: record is %s rev %q session %q, want running rev %q",
			got.State, got.Revision, got.SessionID, rev)
	}
	if _, err := s.PutIf(ctx, beadstore.Record{BeadAnchor: anchor, State: beadstore.StateDone, SessionID: "survivor"}, rev); err != nil {
		t.Fatalf("PutIf on the revision from before the killed writer = %v, want applied", err)
	}
}

// A transaction whose client is killed before COMMIT is rolled back. The
// helper's UPDATE has executed inside its transaction (the test sees the
// session "idle in transaction" holding it) when it is killed.
func TestKilledClientTransactionIsRolledBack(t *testing.T) {
	s, dsn := newWorkStore(t)
	ctx := context.Background()
	anchor := "gonk:75:issue:73"
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: anchor, State: beadstore.StateRunning, SessionID: "live"}); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.Get(ctx, anchor)
	obs := rawConn(t, dsn)

	cmd := startKillHelper(t, "tx-before-commit", dsn, anchor, before.Revision)
	if n := countBackends(t, obs, `application_name = 'gonk-kill-helper' AND state = 'idle in transaction'`); n != 1 {
		t.Fatalf("helper backends idle in transaction = %d, want 1 (the uncommitted UPDATE)", n)
	}
	sigkill(t, cmd)
	waitFor(t, 15*time.Second, "the server to end the killed client's session", func() bool {
		return countBackends(t, obs, `application_name = 'gonk-kill-helper'`) == 0
	})
	time.Sleep(time.Second) //nolint:forbidigo // negative wait: gives a stray write time to land before asserting none did. Load cannot turn it red, only make it pass vacuously
	assertNothingLanded(t, s, anchor, before.Revision)
}

// THE ONE THAT MATTERS FOR gonk-gate. Its writes are autocommit statements, not
// transactions, and an autocommit statement that has reached the server keeps
// running after its client dies. So: the real store's PutIf is made to wait on
// a row lock another session holds; the writer process is SIGKILLed while it
// waits; the lock is held 3s longer (past client_connection_check_interval's
// 1s, short of lock_timeout's 5s) and then released. The dead writer's
// statement must have been cancelled -- it must NOT acquire the lock and commit
// once it is free, which is what it does without the check interval.
func TestKilledWriterBlockedOnALockNeverLands(t *testing.T) {
	s, dsn := newWorkStore(t)
	ctx := context.Background()
	anchor := "gonk:75:issue:73"
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: anchor, State: beadstore.StateRunning, SessionID: "live"}); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.Get(ctx, anchor)

	locker := rawConn(t, dsn)
	tx, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM work_items WHERE anchor = $1 FOR UPDATE`, anchor); err != nil {
		t.Fatal(err)
	}
	obs := rawConn(t, dsn)

	cmd := startKillHelper(t, "putif-blocked", dsn, anchor, before.Revision)
	waitFor(t, 15*time.Second, "the helper's PutIf to wait on the row lock", func() bool {
		return countBackends(t, obs, `application_name = 'gonk-workstore' AND wait_event_type = 'Lock'`) == 1
	})
	sigkill(t, cmd)
	start := time.Now()
	// Pinned: the statement was cancelled while the lock was STILL HELD, so it
	// is the connection check, not lock_timeout, that ended it.
	waitFor(t, 3*time.Second, "the server to cancel the dead writer's waiting statement", func() bool {
		return countBackends(t, obs, `application_name = 'gonk-workstore' AND wait_event_type = 'Lock'`) == 0
	})
	t.Logf("dead writer's waiting statement cancelled %v after SIGKILL, lock still held", time.Since(start).Round(time.Millisecond))
	time.Sleep(3*time.Second - time.Since(start)) //nolint:forbidigo // holds the row lock past client_connection_check_interval (1s) and short of lock_timeout (5s), server-side timers the test cannot inject
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) //nolint:forbidigo // negative wait after releasing the lock: a dead writer that was NOT cancelled would commit now. Load can only make it pass vacuously
	if n := countBackends(t, obs, `application_name = 'gonk-workstore' AND state = 'active' AND query LIKE '%UPDATE work_items%'`); n != 0 {
		t.Fatalf("%d dead-writer statements still active after the lock was released", n)
	}
	assertNothingLanded(t, s, anchor, before.Revision)
}

// sanity for the helper plumbing itself: a helper that is NOT killed does land,
// so the tests above cannot pass merely because the helper never wrote.
func TestKillHelperWritesWhenNotKilled(t *testing.T) {
	s, dsn := newWorkStore(t)
	ctx := context.Background()
	anchor := "gonk:75:issue:74"
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: anchor, State: beadstore.StateRunning}); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.Get(ctx, anchor)
	startKillHelper(t, "putif-blocked", dsn, anchor, before.Revision)
	waitFor(t, 15*time.Second, "the unkilled helper's PutIf to land", func() bool {
		got, _, _ := s.Get(ctx, anchor)
		return got.SessionID == deadWriter && strings.EqualFold(string(got.State), "done")
	})
}
