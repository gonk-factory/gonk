package harness

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// TestClock drives the `testclock` build of gonk-meter. The meter binary built
// with `-tags testclock` reads a monotonic offset (in seconds) from the file at
// $GONK_TESTCLOCK_FILE on every Now(); the production build has no such code path
// at all, and Task 9 asserts the production binary does not contain the symbol.
//
// This is how the harness crosses a month boundary, expires a reservation_ttl, and
// enters a quiet-hours window WITHOUT SLEEPING. There is no other honest way: a
// test that waits 60 minutes for a reservation to expire is a test nobody runs.
type TestClock struct {
	path   string
	offset time.Duration
}

// NewTestClock writes an initial "0" offset into the creds dir and returns a
// clock pointed at it. Set $GONK_TESTCLOCK_FILE to Path() for the meter under
// test.
func NewTestClock(c *Creds) (*TestClock, error) {
	tc := &TestClock{path: c.Path("testclock")}
	if err := tc.write(0); err != nil {
		return nil, err
	}
	return tc, nil
}

// Path is the file the testclock meter reads on every Now(); set
// GONK_TESTCLOCK_FILE to it.
func (tc *TestClock) Path() string { return tc.path }

// Offset is the current offset applied to the meter's clock.
func (tc *TestClock) Offset() time.Duration { return tc.offset }

// Advance moves the clock FORWARD by d and is monotone by construction: Plan 03
// Decision 6 says a backwards clock jump must never reset a project's spend, and
// the harness must not be the thing that violates it. A non-positive d is
// rejected; the one test that genuinely needs to go backwards uses SetOffset.
func (tc *TestClock) Advance(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("harness: TestClock.Advance is monotone; got non-positive %s (use SetOffset for a deliberate backwards jump)", d)
	}
	return tc.write(tc.offset + d)
}

// SetOffset sets the absolute offset, including a value smaller than the current
// one. It is the ONLY way to move the clock backwards and exists for exactly one
// caller: TestBackwardsClockDoesNotResetSpend. Everything else uses Advance.
func (tc *TestClock) SetOffset(d time.Duration) error { return tc.write(d) }

func (tc *TestClock) write(d time.Duration) error {
	secs := int64(d / time.Second)
	if err := os.WriteFile(tc.path, []byte(strconv.FormatInt(secs, 10)), 0o600); err != nil {
		return fmt.Errorf("harness: write testclock: %w", err)
	}
	tc.offset = d
	return nil
}
