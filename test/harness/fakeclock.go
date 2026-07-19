package harness

import (
	"sync"
	"time"
)

// FakeClock is the in-process clock the L1 integration suite injects directly
// into the real meter service (service.New's `now func() time.Time`). Unlike
// TestClock -- which writes an offset to a file for the `testclock` build of the
// meter BINARY at L2/L3 -- FakeClock is a plain in-memory clock read straight
// through a function value, so an L1 test crosses a month boundary, expires a
// reservation, or enters a quiet-hours window WITHOUT touching the filesystem
// and WITHOUT sleeping.
//
// It is safe for concurrent use: the reservation-race test drives dozens of
// goroutines through /decide, every one of which reads the clock.
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewFakeClock returns a clock reading t (normalized to UTC).
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t.UTC()} }

// Now is the value to pass as service.New's clock: fc.Now.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock FORWARD by d. A non-positive d is a no-op rather than
// a panic: the money-path invariants (spend windows, reservation expiry) are all
// monotone, and a test should never be the thing that moves the clock backwards.
func (c *FakeClock) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Set jumps the clock to an absolute instant (UTC). Used only to place the clock
// inside a quiet-hours window at construction; ordinary time passage uses Advance.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t.UTC()
}
