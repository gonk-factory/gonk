//go:build integration

// Package store_test (this file) is the POSITIVE result Task 0b's spike
// demanded. That spike (docs/spikes/dolt-reservation-isolation.md) proved
// Dolt does NOT serialize the concurrent-reservation race under ANY tested
// strategy -- default isolation, explicit SERIALIZABLE, and explicit
// SELECT ... FOR UPDATE all let every one of 32 racing writers win against
// headroom for 2, on every one of 90 iterations, zero variance (a 12.8x
// overspend). The owner decision that followed was to target Postgres for
// the ledger. This file is what proves that decision actually holds: the
// EXACT SAME race shape, run against a real Postgres server, with no
// in-process lock (Store.ReserveIfFits is called directly, concurrently, by
// N goroutines -- there is no service-side mutex here to hide behind).
//
// Run it explicitly (see postgres_test.go's docstring for how to stand up
// Postgres):
//
//	GONK_POSTGRES_DSN="postgres://postgres:gonk@127.0.0.1:5432/postgres?sslmode=disable" \
//	  go test -tags integration ./internal/meter/store/ -race -run Race -count=5 -v
//
// -count=5 is not decoration, for the same reason it wasn't in the spike: a
// race that only manifests one run in three is still a race, and a budget
// escape that happens once a month is still a budget escape. This file also
// loops internally (see raceIterations) so a single invocation without
// -count is still convincing on its own.
package store_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
)

// raceIterations is how many times the race is run within a single `go test`
// invocation, independent of the external -count flag.
const raceIterations = 10

// raceWriters (N) and raceEach (the per-reservation real-dollar cost) are
// chosen so a $1.00 ceiling has headroom for EXACTLY raceWinners (K) of the N
// concurrent writers -- the identical shape Task 0b's spike used against
// Dolt, so the two results are directly comparable.
const (
	raceWriters = 32
	raceCeiling = 1.00
	raceEach    = 0.40
	raceWinners = 2
)

// TestReserveIfFitsRace is Plan 03 Task 6's money-safety proof: it launches
// raceWriters goroutines, ALL calling s.ReserveIfFits concurrently against
// ONE project with headroom for exactly raceWinners of them, and asserts,
// every iteration:
//
//  1. exactly raceWinners reservations were accepted (wins == raceWinners --
//     not "at most", exactly: fewer would mean the lock is too coarse or
//     something is spuriously failing, more is the overspend Dolt exhibited);
//  2. the total persisted (sum of OpenReservations' CostUSD) never exceeds
//     the ceiling.
func TestReserveIfFitsRace(t *testing.T) {
	ctx := context.Background()
	ceiling := budget.Budget{
		MonthlyCostUSD: budget.CostLimit(raceCeiling),
		MonthlyTokens:  budget.UnlimitedTokens,
		PerTaskTokens:  budget.UnlimitedTokens,
	}

	overspendCount := 0
	exactWinCount := 0

	// ONE database (and one pgxpool) for the whole test, reused across every
	// iteration -- each iteration uses a distinct project name to stay
	// isolated. Opening a fresh database (and a fresh connection pool) per
	// iteration exhausted Postgres's max_connections around iteration 7 in
	// practice: pgxpool's default pool size is a handful of connections per
	// pool, and pools from every prior iteration were still open (their
	// cleanup was deferred to the end of the whole test via t.Cleanup, not
	// per-iteration). That was a test-harness resource leak, not a
	// money-safety bug -- every iteration that DID run before the exhaustion
	// showed exactly 2 winners and zero overspend.
	s := newTestDatabase(t)

	for iter := 0; iter < raceIterations; iter++ {
		project := fmt.Sprintf("race/project/%d", iter)
		now := time.Now().UTC()

		var wins, losses, errs atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < raceWriters; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := store.Reservation{
					ID:        fmt.Sprintf("iter-%d-r-%d", iter, i),
					Project:   project,
					BeadID:    fmt.Sprintf("gk-%d", i), // distinct beads: this race is about the MONTHLY ceiling, not the per-task one
					CostUSD:   raceEach,
					CreatedAt: now,
					ExpiresAt: now.Add(time.Hour),
				}
				fits, err := s.ReserveIfFits(ctx, project, ceiling, budget.Spend{}, r)
				switch {
				case err != nil:
					errs.Add(1)
					t.Logf("iter=%d writer=%d: unexpected error: %v", iter, i, err)
				case fits:
					wins.Add(1)
				default:
					losses.Add(1)
				}
			}(i)
		}
		wg.Wait()

		open, err := s.OpenReservations(ctx, project, now)
		if err != nil {
			t.Fatalf("iter=%d: OpenReservations: %v", iter, err)
		}
		var total float64
		for _, r := range open {
			total += r.CostUSD
		}

		t.Logf("iter=%d wins=%d losses=%d errs=%d persisted_total=$%.2f ceiling=$%.2f open_reservation_count=%d",
			iter, wins.Load(), losses.Load(), errs.Load(), total, raceCeiling, len(open))

		if errs.Load() > 0 {
			t.Errorf("iter=%d: %d unexpected errors from ReserveIfFits -- see log", iter, errs.Load())
		}
		if wins.Load() != raceWinners {
			t.Errorf("iter=%d: wins=%d, want exactly %d", iter, wins.Load(), raceWinners)
		} else {
			exactWinCount++
		}
		if total > raceCeiling+1e-9 {
			overspendCount++
			t.Errorf("iter=%d: OVERSPEND: $%.2f persisted against a $%.2f ceiling (this is the exact failure Dolt exhibited -- see docs/spikes/dolt-reservation-isolation.md)",
				iter, total, raceCeiling)
		}
		if len(open) != int(wins.Load()) {
			t.Errorf("iter=%d: %d open reservations but %d wins reported -- a reservation was persisted without a corresponding win, or vice versa", iter, len(open), wins.Load())
		}
	}

	t.Logf("summary: %d/%d iterations overspent, %d/%d iterations won exactly %d",
		overspendCount, raceIterations, exactWinCount, raceIterations, raceWinners)
	if overspendCount > 0 {
		t.Fatalf("OVERSPEND observed in %d/%d iterations -- Postgres SELECT ... FOR UPDATE did NOT serialize the reservation race", overspendCount, raceIterations)
	}
	if exactWinCount != raceIterations {
		t.Fatalf("only %d/%d iterations won exactly %d -- the race is not landing the expected headroom every time", exactWinCount, raceIterations, raceWinners)
	}
}

// TestReserveIfFitsRacePerTaskTokenCeiling is the same property, exercised
// against the PER-TASK token ceiling instead of the monthly cost ceiling, and
// with all writers sharing ONE bead -- this is what actually protects a
// single work item from spending twice its per-task budget across a race
// (e.g. two retries of the same attempt firing concurrently).
func TestReserveIfFitsRacePerTaskTokenCeiling(t *testing.T) {
	ctx := context.Background()
	const (
		perTaskTokens = int64(100)
		eachTokens    = int64(40)
		winners       = 2
		writers       = 32
	)
	ceiling := budget.Budget{
		MonthlyCostUSD: budget.Unlimited,
		MonthlyTokens:  budget.UnlimitedTokens,
		PerTaskTokens:  budget.TokenLimit(perTaskTokens),
	}

	s := newTestDatabase(t)
	const project = "race/project"
	const beadID = "gk-shared"
	now := time.Now().UTC()

	var wins, losses, errs atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := store.Reservation{
				ID:        fmt.Sprintf("tok-r-%d", i),
				Project:   project,
				BeadID:    beadID,
				Tokens:    eachTokens,
				CreatedAt: now,
				ExpiresAt: now.Add(time.Hour),
			}
			fits, err := s.ReserveIfFits(ctx, project, ceiling, budget.Spend{}, r)
			switch {
			case err != nil:
				errs.Add(1)
				t.Logf("writer=%d: unexpected error: %v", i, err)
			case fits:
				wins.Add(1)
			default:
				losses.Add(1)
			}
		}(i)
	}
	wg.Wait()

	open, err := s.OpenReservations(ctx, project, now)
	if err != nil {
		t.Fatalf("OpenReservations: %v", err)
	}
	var totalTokens int64
	for _, r := range open {
		totalTokens += r.Tokens
	}

	t.Logf("wins=%d losses=%d errs=%d persisted_tokens=%d per_task_ceiling=%d",
		wins.Load(), losses.Load(), errs.Load(), totalTokens, perTaskTokens)

	if errs.Load() > 0 {
		t.Fatalf("%d unexpected errors -- see log", errs.Load())
	}
	if wins.Load() != winners {
		t.Fatalf("wins=%d, want exactly %d", wins.Load(), winners)
	}
	if totalTokens > perTaskTokens {
		t.Fatalf("OVERSPEND: %d tokens persisted against a %d per-task ceiling", totalTokens, perTaskTokens)
	}
}
