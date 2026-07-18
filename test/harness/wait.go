// Package harness holds the shared fixtures for gonk's integration (L1),
// component (L2), and e2e (L3) suites.
//
// ONE RULE ABOVE ALL OTHERS: nothing here sleeps. Every wait is WaitFor on an
// observable predicate with a deadline. A time.Sleep in a distributed test is a
// flake generator, and a flaky budget test is worse than no budget test -- it
// trains people to ignore red.
package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrTimeout = errors.New("harness: timed out")

// pollInterval is short on purpose: these predicates are cheap local HTTP calls,
// and the cost of polling is far below the cost of a slow suite nobody runs.
const pollInterval = 100 * time.Millisecond

type fatalErr struct{ error }

// Fatal marks a predicate error as unrecoverable: WaitFor gives up immediately
// instead of retrying for the rest of the deadline. Use it for 401s, 400s, and
// anything else that will still be wrong in two minutes.
func Fatal(err error) error { return fatalErr{err} }

// WaitFor polls until cond returns true, cond returns a Fatal error, or the
// deadline passes. desc is what we were waiting FOR -- it goes in the timeout
// message, and a bad desc makes a 25-minute e2e failure undiagnosable.
func WaitFor(ctx context.Context, desc string, deadline time.Duration, cond func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var last error
	for {
		ok, err := cond()
		var fe fatalErr
		if errors.As(err, &fe) {
			return fmt.Errorf("harness: waiting for %s: %w", desc, fe.error)
		}
		if err != nil {
			last = err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w after %s waiting for %s (last error: %v)", ErrTimeout, deadline, desc, last)
			}
			return fmt.Errorf("%w after %s waiting for %s", ErrTimeout, deadline, desc)
		case <-time.After(pollInterval):
		}
	}
}
