package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gonk-p8w8, item (a) on gonk's side: one sweep pass at a time.

func judgeableSweep(t *testing.T) (sweepDeps, *fakeOutcomeMeter) {
	t.Helper()
	rt, orders, gl, store := staleSweepFixture(t)
	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	return sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	}, fm
}

// THE OVERLAP ITSELF. Pass A is still running (it holds the lease) when gc's
// tracking watchdog lets pass B start. B must not touch a single bead: not read
// a session, not report an outcome, not re-fire a dispatch.
func TestSweepSkipsItsPassWhileAnotherPassHoldsTheLease(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "gonk-sweep.lock")
	releaseA, err := acquireSweepLease(lock, time.Now())
	if err != nil {
		t.Fatalf("pass A's lease: %v", err)
	}

	deps, fm := judgeableSweep(t)
	var log strings.Builder
	deps.LockFile, deps.Log = lock, testLogger(&log)
	if code := runSweep(context.Background(), deps); code != 0 {
		t.Fatalf("pass B exit = %d, want 0 -- a skipped tick is not a failed one", code)
	}
	if n := len(fm.requests()); n != 0 {
		t.Fatalf("pass B reported %d outcome(s) while pass A held the lease, want 0", n)
	}
	if !strings.Contains(log.String(), "another pass is still running") {
		t.Fatalf("a skipped pass must say so:\n%s", log.String())
	}

	releaseA()
	if code := runSweep(context.Background(), deps); code != 0 {
		t.Fatalf("pass after A finished: exit = %d", code)
	}
	if n := len(fm.requests()); n != 1 {
		t.Fatalf("once A released the lease the next pass reported %d outcome(s), want 1", n)
	}
}

// A pass that finishes must give the lease back, or every later tick skips.
func TestSweepReleasesTheLeaseWhenThePassEnds(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "gonk-sweep.lock")
	deps, _ := judgeableSweep(t)
	deps.LockFile = lock
	if code := runSweep(context.Background(), deps); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	release, err := acquireSweepLease(lock, time.Now())
	if err != nil {
		t.Fatalf("lease still held after the pass returned: %v", err)
	}
	release()
}

// The lease must never become the reason the outcome path stops (gonk-p7qh):
// if it cannot even be opened, sweep anyway and say so. The store's revision
// precondition still guards every write.
func TestSweepRunsUnguardedWhenTheLeaseCannotBeOpened(t *testing.T) {
	deps, fm := judgeableSweep(t)
	var log strings.Builder
	deps.LockFile = filepath.Join(t.TempDir(), "no-such-dir", "gonk-sweep.lock")
	deps.Log = testLogger(&log)
	if code := runSweep(context.Background(), deps); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if n := len(fm.requests()); n != 1 {
		t.Fatalf("outcomes = %d, want 1: an unopenable lease must not stop the sweep", n)
	}
	if !strings.Contains(log.String(), "running unguarded") {
		t.Fatalf("an unguarded pass must say so:\n%s", log.String())
	}
}

// NO STALE LEASE. gc kills a pass at its 300s order timeout with SIGKILL, which
// runs no defer. The kernel must still release the lock, or one killed pass
// would stop every later one. Proven with a real process, really killed.
func TestSweepLeaseOfAKilledPassIsReleased(t *testing.T) {
	if os.Getenv("GONK_SWEEP_LEASE_HOLDER") != "" {
		return // this is the helper, see below
	}
	lock := filepath.Join(t.TempDir(), "gonk-sweep.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSweepLeaseHolderProcess$")
	cmd.Env = append(os.Environ(), "GONK_SWEEP_LEASE_HOLDER="+lock)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	held := false
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "HELD" {
			held = true
			break
		}
	}
	if !held {
		t.Fatalf("helper never took the lease (scan err %v)", sc.Err())
	}
	if _, err := acquireSweepLease(lock, time.Now()); !errors.Is(err, errSweepLeaseHeld) {
		t.Fatalf("lease while the helper holds it: err = %v, want errSweepLeaseHeld", err)
	}

	if err := cmd.Process.Kill(); err != nil { // SIGKILL: no defer, no release()
		t.Fatal(err)
	}
	_ = cmd.Wait()

	release, err := acquireSweepLease(lock, time.Now())
	if err != nil {
		t.Fatalf("lease after its holder was SIGKILLed: %v -- a killed pass would block every later one", err)
	}
	release()
}

// TestSweepLeaseHolderProcess is not a test: it is the process
// TestSweepLeaseOfAKilledPassIsReleased starts and kills.
func TestSweepLeaseHolderProcess(t *testing.T) {
	lock := os.Getenv("GONK_SWEEP_LEASE_HOLDER")
	if lock == "" {
		return // only meaningful as the helper process; a no-op in a normal run
	}
	if _, err := acquireSweepLease(lock, time.Now()); err != nil {
		t.Fatalf("helper: %v", err)
	}
	_, _ = os.Stdout.WriteString("HELD\n")
	time.Sleep(time.Minute) //nolint:forbidigo // helper process holds the lease until the test kills it; nothing is timed
}

func TestSweepLockFileDefaultsBesideTheHealthFile(t *testing.T) {
	t.Setenv(sweepLockFileEnv, "")
	t.Setenv(sweepHealthFileEnv, "/city/somewhere/health.json")
	if got := sweepLockFile(); got != "/city/somewhere/gonk-sweep.lock" {
		t.Fatalf("sweepLockFile() = %q, want it beside the health file", got)
	}
	t.Setenv(sweepHealthFileEnv, "")
	if got, want := sweepLockFile(), filepath.Join(filepath.Dir(defaultSweepHealthFile), "gonk-sweep.lock"); got != want {
		t.Fatalf("sweepLockFile() = %q, want %q", got, want)
	}
	t.Setenv(sweepLockFileEnv, "/elsewhere/lease")
	if got := sweepLockFile(); got != "/elsewhere/lease" {
		t.Fatalf("sweepLockFile() = %q, want the override", got)
	}
}
