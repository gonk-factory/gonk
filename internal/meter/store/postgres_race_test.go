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
					ID:         fmt.Sprintf("iter-%d-r-%d", iter, i),
					Project:    project,
					BeadID:     fmt.Sprintf("gk-%d", i), // distinct beads: this race is about the MONTHLY ceiling, not the per-task one
					SessionKey: fmt.Sprintf("s-%d", i),  // distinct sessions too, so idempotency never dedups these apart from the budget check
					CostUSD:    raceEach,
					CreatedAt:  now,
					ExpiresAt:  now.Add(time.Hour),
				}
				res, err := s.ReserveIfFits(ctx, project, ceiling, budget.Spend{}, r)
				switch {
				case err != nil:
					errs.Add(1)
					t.Logf("iter=%d writer=%d: unexpected error: %v", iter, i, err)
				case res.Fits:
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
// (e.g. two retries of the same attempt firing concurrently). Each writer uses
// a DISTINCT session_key: concurrent retries of one bead are distinct sessions,
// so /decide idempotency (keyed on project+bead+session) must NOT collapse them
// -- this race is about the token CEILING, not idempotency.
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
				ID:         fmt.Sprintf("tok-r-%d", i),
				Project:    project,
				BeadID:     beadID,
				SessionKey: fmt.Sprintf("s-%d", i), // distinct sessions: concurrent retries of one bead, NOT the same /decide
				Tokens:     eachTokens,
				CreatedAt:  now,
				ExpiresAt:  now.Add(time.Hour),
			}
			res, err := s.ReserveIfFits(ctx, project, ceiling, budget.Spend{}, r)
			switch {
			case err != nil:
				errs.Add(1)
				t.Logf("writer=%d: unexpected error: %v", i, err)
			case res.Fits:
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

// TestReserveIsIdempotentAcrossReplicas is FIX-A's cross-replica proof, the
// double-reserve analogue of TestReserveIfFitsRace's overspend proof. It fires
// idempotencyWriters goroutines ALL reserving the SAME
// (project, bead_id, session_key) -- the exact shape of intake's Gate 1 and the
// pack's Gate 2 racing, times N, with NO in-process service mutex to serialize
// them (s.ReserveIfFits is called directly). It asserts, on real Postgres:
//
//  1. exactly ONE open reservation exists for that key (not N, not two);
//  2. every caller got Fits == true (nobody is spuriously refused);
//  3. every caller got the SAME reservation id back (the one winner's);
//  4. exactly one caller inserted (Existing == false) and the rest were
//     idempotent hits (Existing == true).
//
// This is what proves the partial UNIQUE index + FOR UPDATE lock -- NOT any
// in-process mutex -- enforce idempotency, so meter is safe with >1 replica.
func TestReserveIsIdempotentAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	ceiling := budget.Budget{
		MonthlyCostUSD: budget.CostLimit(100), // generous: this race is about idempotency, not the ceiling
		MonthlyTokens:  budget.UnlimitedTokens,
		PerTaskTokens:  budget.UnlimitedTokens,
	}

	const idempotencyWriters = 32
	s := newTestDatabase(t)

	for iter := 0; iter < raceIterations; iter++ {
		project := fmt.Sprintf("idem/project/%d", iter)
		const beadID, sessionKey = "gk-anchor", "sess-fixed"
		now := time.Now().UTC()

		var inserts, hits, refused, errs atomic.Int64
		ids := make([]string, idempotencyWriters)
		var wg sync.WaitGroup
		for i := 0; i < idempotencyWriters; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := store.Reservation{
					ID:         fmt.Sprintf("iter-%d-writer-%d", iter, i), // DISTINCT candidate ids: only one may win
					Project:    project,
					BeadID:     beadID,
					SessionKey: sessionKey,
					Rung:       "glm",
					Attempt:    1,
					CostUSD:    0.40,
					CreatedAt:  now,
					ExpiresAt:  now.Add(time.Hour),
				}
				res, err := s.ReserveIfFits(ctx, project, ceiling, budget.Spend{}, r)
				switch {
				case err != nil:
					errs.Add(1)
					t.Logf("iter=%d writer=%d: unexpected error: %v", iter, i, err)
				case !res.Fits:
					refused.Add(1)
				case res.Existing:
					hits.Add(1)
					ids[i] = res.Reservation.ID
				default:
					inserts.Add(1)
					ids[i] = res.Reservation.ID
				}
			}(i)
		}
		wg.Wait()

		open, err := s.OpenReservations(ctx, project, now)
		if err != nil {
			t.Fatalf("iter=%d: OpenReservations: %v", iter, err)
		}

		t.Logf("iter=%d inserts=%d idempotent_hits=%d refused=%d errs=%d open_reservation_count=%d",
			iter, inserts.Load(), hits.Load(), refused.Load(), errs.Load(), len(open))

		if errs.Load() > 0 {
			t.Errorf("iter=%d: %d unexpected errors -- see log", iter, errs.Load())
		}
		if refused.Load() > 0 {
			t.Errorf("iter=%d: %d callers were refused (Fits=false); idempotent reserves of one key must all succeed", iter, refused.Load())
		}
		if len(open) != 1 {
			t.Errorf("iter=%d: %d open reservations for one (project,bead,session), want exactly 1 -- a duplicate was minted (this is the cross-replica double-reserve the partial unique index exists to prevent)", iter, len(open))
		}
		if inserts.Load() != 1 {
			t.Errorf("iter=%d: %d inserts (Existing=false), want exactly 1", iter, inserts.Load())
		}
		if hits.Load() != idempotencyWriters-1 {
			t.Errorf("iter=%d: %d idempotent hits, want %d", iter, hits.Load(), idempotencyWriters-1)
		}
		// Every caller that got a reservation must have gotten the SAME one.
		var winner string
		if len(open) == 1 {
			winner = open[0].ID
		}
		for i, id := range ids {
			if id != "" && winner != "" && id != winner {
				t.Errorf("iter=%d writer=%d got reservation id %q but the single open reservation is %q", iter, i, id, winner)
			}
		}
	}
}
