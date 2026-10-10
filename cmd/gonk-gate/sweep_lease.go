package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ONE gonk-sweep PASS AT A TIME, AND GONK HAS TO ENFORCE IT ITSELF (gonk-p8w8).
//
// Gas City does not. Its order gate refuses to fire an order while an open
// order-tracking bead exists for it, but the controller's tracking watchdog
// closes EVERY open tracking bead older than two minutes
// (orderTrackingSweepWatchdogStaleAfter, cmd/gc/order_dispatch.go at
// GASCITY_REF) whether or not the process it tracks is still running -- and
// gonk-sweep's order timeout is 300s. So any pass slower than two minutes
// re-opens the gate under itself. On 2026-09-13 that is exactly what happened:
// sweep A started ~15:46:15, the watchdog closed its tracking bead at
// ~15:48:35, and sweep B was reading the same records by ~15:49:00. B's
// write-back of what it read erased a dispatch's reservation and put two
// sessions on one bead+attempt.
//
// WHY flock(2). Every gonk-sweep pass is a child process of the one gc in the
// controller container, so they share a kernel and a filesystem (/city, the
// emptyDir the health file already lives on). A flock is released by the
// kernel when the holder's last descriptor closes -- including when gc SIGKILLs
// the pass at its order timeout -- so there is no stale lease to expire, no TTL
// to tune, and no clock involved. A lease in the bead store would have all
// three problems plus bd's non-atomic writes.
//
// The descriptor is opened O_CLOEXEC (Go's default), so the `bd` processes the
// sweep execs do not inherit it and cannot keep a dead pass's lock alive.
//
// WHAT IT DOES NOT COVER: a second controller replica (gonk runs one), and any
// writer that is not a sweep -- dispatch writes concurrently with a sweep by
// design. The store's revision precondition (beadstore.PutIf) is what protects
// against those, and it is still in force when this lease cannot be taken.

// sweepLockFileEnv overrides where the lease lives.
const sweepLockFileEnv = "GONK_SWEEP_LOCK_FILE"

// sweepLockFile is the lease path: GONK_SWEEP_LOCK_FILE, or gonk-sweep.lock
// beside the health file -- the directory the controller already guarantees
// is writable and shared by every pass (see sweepHealthFile).
func sweepLockFile() string {
	if v := os.Getenv(sweepLockFileEnv); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(sweepHealthFile()), "gonk-sweep.lock")
}

// errSweepLeaseHeld means another pass holds the lease right now.
var errSweepLeaseHeld = errors.New("another gonk-sweep pass holds the lease")

// acquireSweepLease takes the lease without waiting. On success it returns the
// release func. If another pass holds it, the error wraps errSweepLeaseHeld and
// names the holder as best it can. Any other error means the lease could not
// be evaluated at all. path "" disables the lease (unit tests).
func acquireSweepLease(path string, now time.Time) (release func(), err error) {
	if path == "" {
		return func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open sweep lease %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", errSweepLeaseHeld, strings.TrimSpace(string(holder)))
		}
		return nil, fmt.Errorf("lock sweep lease %s: %w", path, err)
	}
	// Say who holds it, for the pass that is refused. Advisory text only: the
	// lock is the flock, not these bytes.
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "pid %d since %s\n", os.Getpid(), now.UTC().Format(time.RFC3339))
	}
	return func() { _ = f.Close() }, nil
}
