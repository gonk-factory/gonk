// Package storetest is the single conformance suite every store.Store
// backend must satisfy. Run(t, func() store.Store) runs the whole suite
// against a fresh, empty store; every test runs against BOTH Memory (see
// ../memory_test.go) and Postgres (see ../postgres_test.go, gated behind the
// `integration` build tag), so a behaviour the memory store has by accident
// (and the real one does not) cannot hide behind green tests.
package storetest

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func unlimited() budget.Budget {
	return budget.Budget{
		MonthlyCostUSD: budget.Unlimited,
		MonthlyTokens:  budget.UnlimitedTokens,
		PerTaskTokens:  budget.UnlimitedTokens,
	}
}

// zeroCostBudget is the onboarding default (spec 5.3): monthly_cost_usd: 0 with
// room in the token ceilings. A local (zero-cost) rung must run freely under it
// -- that is exactly the "dead on arrival" brick gonk-2g4 fixes -- while a
// priced (cloud) rung must never be affordable.
func zeroCostBudget() budget.Budget {
	return budget.Budget{
		MonthlyCostUSD: 0,
		MonthlyTokens:  1_000_000,
		PerTaskTokens:  1_000_000,
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustFit(t *testing.T, res store.ReserveResult, err error) {
	t.Helper()
	must(t, err)
	if !res.Fits {
		t.Fatal("ReserveIfFits.Fits = false, want true")
	}
}

// Run is the single conformance suite. newStore must return a fresh, empty
// store on every call.
func Run(t *testing.T, newStore func() store.Store) {
	t.Run("RegistrationRoundTrip", testRegistrationRoundTrip(newStore))
	t.Run("AttemptsAreScopedToTheBeadAndKeyedByReservation", testAttemptsAreScopedToTheBeadAndKeyedByReservation(newStore))
	t.Run("RecordAttemptIsIdempotentPerReservation", testRecordAttemptIsIdempotentPerReservation(newStore))
	t.Run("ATerminalOutcomeSupersedesTheJanitorsGuess", testATerminalOutcomeSupersedesTheJanitorsGuess(newStore))
	t.Run("ReservationsHoldBudgetAndExpire", testReservationsHoldBudgetAndExpire(newStore))
	t.Run("ZeroCostRungReservesUnderZeroCostBudget", testZeroCostRungReservesUnderZeroCostBudget(newStore))
	t.Run("PricedRungRejectedUnderZeroCostBudget", testPricedRungRejectedUnderZeroCostBudget(newStore))
	t.Run("ZeroCostRungStillBoundByTokenGate", testZeroCostRungStillBoundByTokenGate(newStore))
	t.Run("ReserveIsIdempotentPerOpenKey", testReserveIsIdempotentPerOpenKey(newStore))
	t.Run("SettleHoldsBudgetUntilTheSpendCatchesUp", testSettleHoldsBudgetUntilTheSpendCatchesUp(newStore))
	t.Run("SpendRowsAreDeduped", testSpendRowsAreDeduped(newStore))
	t.Run("SpendCursorAndSyncedAtAndWindowRoundTrip", testSpendCursorAndSyncedAtAndWindowRoundTrip(newStore))
	t.Run("NonFiniteSpendRowIsRejectedNotPersisted", testNonFiniteSpendRowIsRejectedNotPersisted(newStore))
	t.Run("NonFiniteReservationIsRejected", testNonFiniteReservationIsRejected(newStore))
	t.Run("ListRegistrationsReturnsEveryProject", testListRegistrationsReturnsEveryProject(newStore))
	t.Run("PromptRoundTripAndOneShot", testPromptRoundTripAndOneShot(newStore))
	t.Run("TakePromptHasExactlyOneWinnerUnderConcurrency", testTakePromptHasExactlyOneWinnerUnderConcurrency(newStore))
	t.Run("ExpiredPromptReadsAsAbsentNotConsumed", testExpiredPromptReadsAsAbsentNotConsumed(newStore))
}

// The prompt row is what the agent pod fetches instead of having its prompt
// typed into a TUI (gonk-mzd). Three properties matter, and each is a distinct
// failure if it is wrong.
func testPromptRoundTripAndOneShot(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := time.Now().UTC().Truncate(time.Millisecond)

		if _, found, err := s.PromptStatus(ctx, "nope"); err != nil || found {
			t.Fatalf("empty store: found=%v err=%v", found, err)
		}
		// An absent alias must read as NOT-found rather than as consumed: the
		// two answers become 404 and 410, which mean opposite things to the
		// entrypoint ("never delivered" vs "respawn or theft").
		if _, found, consumed, err := s.TakePrompt(ctx, "nope", now); err != nil || found || consumed {
			t.Fatalf("take of absent alias: found=%v consumed=%v err=%v", found, consumed, err)
		}

		want := store.Prompt{
			Alias:  "gonk.triage.p42.i3.a1.ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			Prompt: "triage this issue", Model: "qwen3-14b",
			Metadata:  `{"gonk_project":"g/r"}`,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := s.PutPrompt(ctx, want); err != nil {
			t.Fatalf("PutPrompt: %v", err)
		}

		// Status must NOT consume -- dispatch polls it to confirm the fetch.
		st, found, err := s.PromptStatus(ctx, want.Alias)
		if err != nil || !found {
			t.Fatalf("status after put: found=%v err=%v", found, err)
		}
		if !st.FetchedAt.IsZero() {
			t.Fatalf("PromptStatus consumed the prompt: fetched_at=%v", st.FetchedAt)
		}

		got, found, consumed, err := s.TakePrompt(ctx, want.Alias, now)
		if err != nil || !found || consumed {
			t.Fatalf("first take: found=%v consumed=%v err=%v", found, consumed, err)
		}
		if got.Prompt != want.Prompt || got.Model != want.Model || got.Metadata != want.Metadata {
			t.Fatalf("round trip lost fields: %+v", got)
		}
		if got.FetchedAt.IsZero() {
			t.Fatal("take did not stamp fetched_at -- dispatch has no way to confirm delivery")
		}

		// Second take is the one-shot guarantee: the alias is an unauthenticated
		// capability, so a replayable read is the whole risk.
		if _, found, consumed, err = s.TakePrompt(ctx, want.Alias, now); err != nil || !found || !consumed {
			t.Fatalf("second take: found=%v consumed=%v err=%v (want found+consumed => 410)", found, consumed, err)
		}
	}
}

// Two pods racing must not both receive the prompt. In postgres this is one
// UPDATE ... WHERE fetched_at IS NULL ... RETURNING; in memory it is one
// critical section. If either degrades to read-then-write, this fails.
func testTakePromptHasExactlyOneWinnerUnderConcurrency(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := time.Now().UTC()
		alias := "gonk.triage.p42.i9.a1.RACERACERACERACERACERACERA"
		if err := s.PutPrompt(ctx, store.Prompt{Alias: alias, Prompt: "p", Model: "m", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatalf("PutPrompt: %v", err)
		}

		const n = 16
		var wg sync.WaitGroup
		wins := make([]bool, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, found, consumed, err := s.TakePrompt(ctx, alias, time.Now().UTC())
				wins[i] = err == nil && found && !consumed
			}(i)
		}
		close(start)
		wg.Wait()

		won := 0
		for _, w := range wins {
			if w {
				won++
			}
		}
		if won != 1 {
			t.Fatalf("%d/%d callers got the prompt, want exactly 1 -- the one-shot "+
				"guarantee is the only thing between an unauthenticated read and a "+
				"replayable one", won, n)
		}
	}
}

// Expired must read as ABSENT, not as consumed: the janitor is about to drop it
// and the caller should retry or fail, not be told someone else took it.
func testExpiredPromptReadsAsAbsentNotConsumed(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := time.Now().UTC()
		alias := "gonk.triage.p42.i4.a1.EXPIREDEXPIREDEXPIREDEXPIR"
		if err := s.PutPrompt(ctx, store.Prompt{Alias: alias, Prompt: "p", Model: "m", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}); err != nil {
			t.Fatalf("PutPrompt: %v", err)
		}
		if _, found, consumed, err := s.TakePrompt(ctx, alias, now); err != nil || found || consumed {
			t.Fatalf("expired take: found=%v consumed=%v err=%v, want absent", found, consumed, err)
		}
		n, err := s.ExpirePrompts(ctx, now)
		if err != nil || n != 1 {
			t.Fatalf("ExpirePrompts = %d, %v, want 1", n, err)
		}
	}
}

func testRegistrationRoundTrip(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		if _, ok, _ := s.GetRegistration(ctx, "group/repo"); ok {
			t.Fatal("empty store returned a registration")
		}
		// UNLIMITED BUDGET ON PURPOSE. Effective encodes "unlimited" as
		// math.Inf(1) (cost) and math.MaxInt64 (tokens). +Inf is NOT
		// JSON-serializable -- json.Marshal(reg.Effective) RETURNS AN ERROR --
		// and a Postgres double precision column rejects Inf in some configs
		// and silently stores NaN in others. A durable store that naively
		// marshals reg.Effective would fail to persist the FIRST
		// unlimited-budget project (fail-closed, but a bug). A zero Effective
		// (the old fixture) hid this completely; this fixture forces every
		// backend's persistence to be +Inf-safe.
		reg := store.Registration{
			Project: "group/repo", Rig: "group-repo", State: store.StateActive,
			Raw: []byte("version: 1\n"),
			Effective: gonkcfg.Effective{
				Enabled: true,
				Ladder:  []string{"qwen-local"},
				Budget: gonkcfg.EffectiveBudget{
					MonthlyCostUSD: math.Inf(1),
					MonthlyTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
					PerTaskTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
				},
			},
		}
		if err := s.PutRegistration(ctx, reg); err != nil {
			t.Fatalf("PutRegistration of an UNLIMITED-budget project failed: %v", err)
		}
		got, ok, err := s.GetRegistration(ctx, "group/repo")
		if err != nil || !ok || got.State != store.StateActive || got.Rig != "group-repo" {
			t.Fatalf("GetRegistration = %+v %v %v", got, ok, err)
		}
		// The unlimited sentinels must survive the round trip, whatever on-disk
		// convention the backend uses: the value read back must still be
		// unlimited.
		if !math.IsInf(got.Effective.Budget.MonthlyCostUSD, 1) ||
			got.Effective.Budget.MonthlyTokens != gonkcfg.TokenQuantity(math.MaxInt64) {
			t.Fatalf("unlimited budget did not round-trip: %+v", got.Effective.Budget)
		}
		if len(got.Effective.Ladder) != 1 || got.Effective.Ladder[0] != "qwen-local" {
			t.Fatalf("ladder did not round-trip: %+v", got.Effective.Ladder)
		}
		if err := s.DeleteRegistration(ctx, "group/repo"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.GetRegistration(ctx, "group/repo"); ok {
			t.Fatal("registration survived deletion")
		}
	}
}

func testAttemptsAreScopedToTheBeadAndKeyedByReservation(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		must(t, s.RecordAttempt(ctx, "group/repo", "gk-1", "rsv-1",
			rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeGateFailed}))
		must(t, s.RecordAttempt(ctx, "group/repo", "gk-1", "rsv-2",
			rung.Attempt{Attempt: 2, Rung: "glm", Outcome: rung.OutcomeSuccess}))
		must(t, s.RecordAttempt(ctx, "group/repo", "gk-2", "rsv-3",
			rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeInfraFailed}))

		got, err := s.Attempts(ctx, "group/repo", "gk-1")
		if err != nil || len(got) != 2 || got[0].Rung != "qwen-local" || got[1].Outcome != rung.OutcomeSuccess {
			t.Fatalf("Attempts(gk-1) = %+v %v", got, err)
		}
		if other, _ := s.Attempts(ctx, "group/repo", "gk-2"); len(other) != 1 {
			t.Fatalf("bead histories bled into each other: %+v", other)
		}
		// The returned slice must not alias the store's: a caller mutating it must
		// not rewrite the ladder history.
		got[0].Outcome = rung.OutcomeSuccess
		again, _ := s.Attempts(ctx, "group/repo", "gk-1")
		if again[0].Outcome != rung.OutcomeGateFailed {
			t.Fatal("Attempts aliased the store's slice; a caller just rewrote history")
		}
	}
}

// Outcome reports are retried. Recording the same reservation's outcome twice
// must not add a second attempt -- that would be a free escalation.
func testRecordAttemptIsIdempotentPerReservation(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		a := rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeGateFailed}
		must(t, s.RecordAttempt(ctx, "p", "gk-1", "rsv-1", a))
		must(t, s.RecordAttempt(ctx, "p", "gk-1", "rsv-1", a))
		if got, _ := s.Attempts(ctx, "p", "gk-1"); len(got) != 1 {
			t.Fatalf("a retried outcome report recorded %d attempts, want 1 (that is a free escalation)", len(got))
		}
	}
}

// A session that outlives reservation_ttl gets an infra-failed attempt from the
// janitor, and may THEN report its real outcome. The real one must win, or the
// ladder silently stops working for any session longer than the TTL.
func testATerminalOutcomeSupersedesTheJanitorsGuess(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		must(t, s.RecordAttempt(ctx, "p", "gk-1", "rsv-1",
			rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeInfraFailed})) // janitor
		must(t, s.RecordAttempt(ctx, "p", "gk-1", "rsv-1",
			rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeGateFailed})) // the truth, late

		got, _ := s.Attempts(ctx, "p", "gk-1")
		if len(got) != 1 || got[0].Outcome != rung.OutcomeGateFailed {
			t.Fatalf("attempts = %+v; the real gate failure must supersede the janitor's infra-failed", got)
		}
		if rung.Escalations(got) != 1 {
			t.Fatal("the bead earned an escalation and did not get one")
		}
	}
}

func testReservationsHoldBudgetAndExpire(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := at("2026-07-13T10:00:00Z")
		r := store.Reservation{
			ID: "rsv-1", Project: "group/repo", BeadID: "gk-1", SessionKey: "s1",
			Rung: "glm", Attempt: 2, CostUSD: 0.40, Tokens: 200_000,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		res, err := s.ReserveIfFits(ctx, "group/repo", unlimited(), budget.Spend{}, r)
		if err != nil || !res.Fits {
			t.Fatalf("ReserveIfFits = %+v, %v; an unlimited ceiling must always fit", res, err)
		}

		open, err := s.OpenReservations(ctx, "group/repo", now.Add(30*time.Minute))
		if err != nil || len(open) != 1 || open[0].CostUSD != 0.40 {
			t.Fatalf("OpenReservations = %+v %v", open, err)
		}
		// Another project's ceiling is untouched by this one.
		if other, _ := s.OpenReservations(ctx, "other/repo", now); len(other) != 0 {
			t.Fatal("a reservation leaked across projects")
		}
		// Past its TTL it no longer holds budget...
		if late, _ := s.OpenReservations(ctx, "group/repo", now.Add(2*time.Hour)); len(late) != 0 {
			t.Fatal("an expired reservation is still holding budget")
		}
		// ...and Expire hands it back exactly once, so the janitor can record the
		// infra failure without recording it twice.
		expired, err := s.ExpireReservations(ctx, now.Add(2*time.Hour))
		if err != nil || len(expired) != 1 || expired[0].ID != "rsv-1" {
			t.Fatalf("ExpireReservations = %+v %v", expired, err)
		}
		if again, _ := s.ExpireReservations(ctx, now.Add(3*time.Hour)); len(again) != 0 {
			t.Fatal("ExpireReservations returned the same reservation twice")
		}
	}
}

// gonk-2g4 (the brick): the onboarding default is monthly_cost_usd: 0 plus a
// local ladder [qwen-local], so a fresh project's ONLY rung is a zero-cost
// (CostUSD == 0) reservation. ReserveIfFits must let it through -- it holds no
// real dollars -- exactly as rung.Decide (decide.go: `spec.EstCostUSD > 0 &&
// !rem.FitsCost(...)`) already skips the cost gate for it. Before the fix the
// stores applied FitsCost unconditionally, FitsCost(0) is false under a $0
// ceiling, so every onboarded project was DEAD ON ARRIVAL. The token ceilings
// still have room here, so the ONLY thing that could reject this is the cost
// leg -- which is what this test pins.
func testZeroCostRungReservesUnderZeroCostBudget(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := at("2026-07-13T10:00:00Z")
		r := store.Reservation{
			ID: "rsv-local", Project: "group/repo", BeadID: "gk-1", SessionKey: "s1",
			Rung: "qwen-local", Attempt: 1, CostUSD: 0, Tokens: 1000,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		res, err := s.ReserveIfFits(ctx, "group/repo", zeroCostBudget(), budget.Spend{}, r)
		if err != nil {
			t.Fatalf("ReserveIfFits(zero-cost local rung) errored: %v", err)
		}
		if !res.Fits {
			t.Fatal("a zero-cost local rung was REJECTED under monthly_cost_usd: 0 -- gonk-2g4 brick: every onboarded project is dead on arrival")
		}
		if open, _ := s.OpenReservations(ctx, "group/repo", now.Add(30*time.Minute)); len(open) != 1 {
			t.Fatalf("OpenReservations = %d, want 1 (the zero-cost reservation must actually be held)", len(open))
		}
	}
}

// The money-safety counterweight to the brick fix: skipping the cost leg is
// legal ONLY for a zero-cost rung. A PRICED (cloud) rung with CostUSD > 0 still
// holds real dollars, so under a $0 cost ceiling it must STILL be rejected --
// the fix must not open an overspend path. Token room is available, so the ONLY
// thing that can (and must) reject this is the cost leg.
func testPricedRungRejectedUnderZeroCostBudget(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := at("2026-07-13T10:00:00Z")
		r := store.Reservation{
			ID: "rsv-cloud", Project: "group/repo", BeadID: "gk-1", SessionKey: "s1",
			Rung: "glm", Attempt: 1, CostUSD: 0.40, Tokens: 1000,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		res, err := s.ReserveIfFits(ctx, "group/repo", zeroCostBudget(), budget.Spend{}, r)
		if err != nil {
			t.Fatalf("ReserveIfFits(priced rung) errored: %v", err)
		}
		if res.Fits {
			t.Fatal("a PRICED rung (CostUSD > 0) was admitted under monthly_cost_usd: 0 -- the cost gate was weakened; this is an overspend hole")
		}
		if open, _ := s.OpenReservations(ctx, "group/repo", now.Add(30*time.Minute)); len(open) != 0 {
			t.Fatalf("OpenReservations = %d, want 0 (a rejected priced rung must hold no budget)", len(open))
		}
	}
}

// The other half of money-safety: skipping the cost leg for a zero-cost rung
// must NOT skip the TOKEN legs. Local rungs are bounded by the token ceilings
// (synthetic pricing makes USD ceilings unreliable for them via LiteLLM), so a
// zero-cost rung whose token budget is exhausted must STILL be rejected.
func testZeroCostRungStillBoundByTokenGate(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := at("2026-07-13T10:00:00Z")
		// A tight monthly-token ceiling with no cost ceiling. The rung is free but
		// wants more tokens than remain.
		tightTokens := budget.Budget{
			MonthlyCostUSD: 0,
			MonthlyTokens:  500,
			PerTaskTokens:  1_000_000,
		}
		r := store.Reservation{
			ID: "rsv-local-big", Project: "group/repo", BeadID: "gk-1", SessionKey: "s1",
			Rung: "qwen-local", Attempt: 1, CostUSD: 0, Tokens: 1000,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		res, err := s.ReserveIfFits(ctx, "group/repo", tightTokens, budget.Spend{}, r)
		if err != nil {
			t.Fatalf("ReserveIfFits(zero-cost, token-exhausted) errored: %v", err)
		}
		if res.Fits {
			t.Fatal("a zero-cost rung over its MONTHLY-token ceiling was admitted -- the fix skipped a token leg, not just the cost leg")
		}

		// Same again, but the PER-TASK token ceiling is the binding one.
		tightTask := budget.Budget{
			MonthlyCostUSD: 0,
			MonthlyTokens:  1_000_000,
			PerTaskTokens:  500,
		}
		res, err = s.ReserveIfFits(ctx, "group/repo", tightTask, budget.Spend{}, r)
		if err != nil {
			t.Fatalf("ReserveIfFits(zero-cost, per-task-exhausted) errored: %v", err)
		}
		if res.Fits {
			t.Fatal("a zero-cost rung over its PER-TASK token ceiling was admitted -- the per-task token leg was skipped")
		}
	}
}

// ReserveIfFits is idempotent on an OPEN reservation keyed by
// (project, bead_id, session_key): the two /decide gates reserve the same work
// before any outcome, and the second reserve must return the first, not mint a
// duplicate holding double the headroom. This is the store half of FIX-A, and
// in Postgres it is enforced by a partial UNIQUE index so it holds across
// replicas -- proven under real concurrency by
// TestReserveIsIdempotentAcrossReplicas (postgres_race_test.go).
func testReserveIsIdempotentPerOpenKey(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := at("2026-07-13T10:00:00Z")
		mk := func(id, bead, session string) store.Reservation {
			return store.Reservation{
				ID: id, Project: "group/repo", BeadID: bead, SessionKey: session,
				Rung: "glm", Attempt: 1, CostUSD: 0.40, Tokens: 1000,
				CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			}
		}

		// First reserve for (gk-1, s1): a fresh insert.
		first, err := s.ReserveIfFits(ctx, "group/repo", unlimited(), budget.Spend{}, mk("rsv-1", "gk-1", "s1"))
		mustFit(t, first, err)
		if first.Existing {
			t.Fatal("the FIRST reserve for a key reported Existing = true")
		}
		if first.Reservation.ID != "rsv-1" {
			t.Fatalf("first reserve returned id %q, want rsv-1", first.Reservation.ID)
		}

		// Second reserve for the SAME (gk-1, s1) with a DIFFERENT id: idempotent
		// hit. It must return the FIRST reservation and insert nothing.
		second, err := s.ReserveIfFits(ctx, "group/repo", unlimited(), budget.Spend{}, mk("rsv-2", "gk-1", "s1"))
		mustFit(t, second, err)
		if !second.Existing {
			t.Fatal("the second reserve for the same open key reported Existing = false -- it minted a duplicate")
		}
		if second.Reservation.ID != "rsv-1" {
			t.Fatalf("second reserve returned id %q, want the first reservation's id rsv-1", second.Reservation.ID)
		}
		if _, ok, _ := s.GetReservation(ctx, "rsv-2"); ok {
			t.Fatal("the duplicate reservation rsv-2 was persisted; idempotency must insert nothing")
		}
		if open, _ := s.OpenReservations(ctx, "group/repo", now); len(open) != 1 {
			t.Fatalf("OpenReservations = %d, want exactly 1 after two reserves of the same key", len(open))
		}

		// A DIFFERENT session_key for the same bead is genuinely different work.
		diffSession, err := s.ReserveIfFits(ctx, "group/repo", unlimited(), budget.Spend{}, mk("rsv-3", "gk-1", "s2"))
		mustFit(t, diffSession, err)
		if diffSession.Existing || diffSession.Reservation.ID != "rsv-3" {
			t.Fatalf("a different session_key was folded into an existing reservation: %+v", diffSession)
		}

		// A DIFFERENT bead for the same session is also genuinely different work.
		diffBead, err := s.ReserveIfFits(ctx, "group/repo", unlimited(), budget.Spend{}, mk("rsv-4", "gk-2", "s1"))
		mustFit(t, diffBead, err)
		if diffBead.Existing || diffBead.Reservation.ID != "rsv-4" {
			t.Fatalf("a different bead_id was folded into an existing reservation: %+v", diffBead)
		}
		if open, _ := s.OpenReservations(ctx, "group/repo", now); len(open) != 3 {
			t.Fatalf("OpenReservations = %d, want 3 (gk-1/s1, gk-1/s2, gk-2/s1)", len(open))
		}

		// After the (gk-1, s1) reservation is SETTLED, it leaves the open-key
		// index, so a legitimate re-sling of that exact key reserves anew.
		must(t, s.Settle(ctx, "rsv-1", now.Add(5*time.Minute)))
		resling, err := s.ReserveIfFits(ctx, "group/repo", unlimited(), budget.Spend{}, mk("rsv-5", "gk-1", "s1"))
		mustFit(t, resling, err)
		if resling.Existing || resling.Reservation.ID != "rsv-5" {
			t.Fatalf("a re-sling after settle did not get a fresh reservation: %+v (a settled reservation must leave the open-key index)", resling)
		}
	}
}

// Settling does NOT free the budget immediately. The session's spend rows land
// seconds-to-minutes after it ends; freeing the hold the moment the outcome
// arrives would over-report headroom by the session's entire actual cost, on
// EVERY successful session, at exactly the moment that cost is unbilled.
func testSettleHoldsBudgetUntilTheSpendCatchesUp(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := at("2026-07-13T10:00:00Z")
		res, err := s.ReserveIfFits(ctx, "p", unlimited(), budget.Spend{}, store.Reservation{
			ID: "rsv-1", Project: "p", CostUSD: 1,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
		mustFit(t, res, err)

		holdUntil := now.Add(5 * time.Minute) // now + max_spend_staleness
		must(t, s.Settle(ctx, "rsv-1", holdUntil))

		open, _ := s.OpenReservations(ctx, "p", now.Add(time.Minute))
		if len(open) != 1 || open[0].CostUSD != 1 {
			t.Fatal("a settled reservation stopped holding budget immediately; the spend rows have not landed yet")
		}
		if late, _ := s.OpenReservations(ctx, "p", now.Add(6*time.Minute)); len(late) != 0 {
			t.Fatal("the hold outlived holdUntil")
		}
		// The janitor must not touch it: its session is not dead, it reported.
		if expired, _ := s.ExpireReservations(ctx, now.Add(6*time.Minute)); len(expired) != 0 {
			t.Fatal("the janitor tried to record an infra failure for a session that reported a real outcome")
		}
		// Settling twice (a retried outcome POST) must not error.
		if err := s.Settle(ctx, "rsv-1", holdUntil); err != nil {
			t.Fatalf("second settle = %v, want nil (outcome reports are retried)", err)
		}
	}
}

func testSpendRowsAreDeduped(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		r := spend.Row{
			CallID: "c1", CostUSD: 0.40, PromptTokens: 1000, At: at("2026-07-05T10:00:00Z"),
			Tags: atags.Tags{Project: "group/repo", Rig: "group-repo", BeadID: "gk-1",
				SessionKey: "s1", Rung: "glm", Attempt: 1, Trigger: atags.TriggerIssueTriage},
		}
		n, err := s.AddSpendRows(ctx, []spend.Row{r})
		if err != nil || n != 1 {
			t.Fatalf("first add: %d %v", n, err)
		}
		// The poller overlaps its windows on purpose. The same call WILL arrive
		// twice; counting it twice is a wrong budget.
		n, err = s.AddSpendRows(ctx, []spend.Row{r})
		if err != nil || n != 0 {
			t.Fatalf("re-adding the same CallID added %d rows, want 0", n)
		}
		rows, _ := s.SpendRows(ctx, "group/repo")
		if len(rows) != 1 {
			t.Fatalf("store holds %d rows, want 1", len(rows))
		}
		all, _ := s.AllSpendRows(ctx)
		if len(all) != 1 {
			t.Fatalf("AllSpendRows = %d rows, want 1", len(all))
		}
	}
}

func testSpendCursorAndSyncedAtAndWindowRoundTrip(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		if c, err := s.SpendCursor(ctx); err != nil || !c.IsZero() {
			t.Fatalf("empty store SpendCursor = %v %v, want zero", c, err)
		}
		if sa, err := s.SyncedAt(ctx); err != nil || !sa.IsZero() {
			t.Fatalf("empty store SyncedAt = %v %v, want zero", sa, err)
		}
		if w, err := s.Window(ctx); err != nil || !w.Zero() {
			t.Fatalf("empty store Window = %+v %v, want zero", w, err)
		}

		cursor := at("2026-07-05T10:00:00Z")
		must(t, s.SetSpendCursor(ctx, cursor))
		got, err := s.SpendCursor(ctx)
		if err != nil || !got.Equal(cursor) {
			t.Fatalf("SpendCursor = %v %v, want %v", got, err, cursor)
		}

		syncedAt := at("2026-07-05T10:05:00Z")
		must(t, s.SetSyncedAt(ctx, syncedAt))
		gotSyncedAt, err := s.SyncedAt(ctx)
		if err != nil || !gotSyncedAt.Equal(syncedAt) {
			t.Fatalf("SyncedAt = %v %v, want %v", gotSyncedAt, err, syncedAt)
		}

		w := spend.Window{Start: at("2026-07-01T00:00:00Z"), End: at("2026-08-01T00:00:00Z")}
		must(t, s.SetWindow(ctx, w))
		gotW, err := s.Window(ctx)
		if err != nil || !gotW.Start.Equal(w.Start) || !gotW.End.Equal(w.End) {
			t.Fatalf("Window = %+v %v, want %+v", gotW, err, w)
		}
	}
}

// NaN/+Inf must never reach the ledger: LiteLLM's spend log is external,
// unvalidated input, and a corrupted cost row must not corrupt budget math.
func testNonFiniteSpendRowIsRejectedNotPersisted(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		bad := spend.Row{
			CallID: "bad-1", CostUSD: math.NaN(), At: at("2026-07-05T10:00:00Z"),
			Tags: atags.Tags{Project: "group/repo", Rig: "group-repo", BeadID: "gk-1",
				SessionKey: "s1", Rung: "glm", Attempt: 1, Trigger: atags.TriggerIssueTriage},
		}
		n, err := s.AddSpendRows(ctx, []spend.Row{bad})
		if err != nil {
			t.Fatalf("AddSpendRows with a NaN cost row returned an error (should be silently dropped): %v", err)
		}
		if n != 0 {
			t.Fatalf("AddSpendRows reported %d added for a NaN-cost row, want 0", n)
		}
		rows, _ := s.SpendRows(ctx, "group/repo")
		if len(rows) != 0 {
			t.Fatalf("a NaN-cost row was persisted: %+v", rows)
		}

		infRow := bad
		infRow.CallID = "bad-2"
		infRow.CostUSD = math.Inf(1)
		n, err = s.AddSpendRows(ctx, []spend.Row{infRow})
		if err != nil {
			t.Fatalf("AddSpendRows with an Inf cost row returned an error (should be silently dropped): %v", err)
		}
		if n != 0 {
			t.Fatalf("AddSpendRows reported %d added for an Inf-cost row, want 0", n)
		}
	}
}

// A reservation's own held amounts must be finite before it ever reaches a
// SQL numeric column.
func testNonFiniteReservationIsRejected(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		now := at("2026-07-13T10:00:00Z")
		r := store.Reservation{
			ID: "rsv-nan", Project: "p", CostUSD: math.NaN(),
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		res, err := s.ReserveIfFits(ctx, "p", unlimited(), budget.Spend{}, r)
		if err == nil {
			t.Fatalf("ReserveIfFits with a NaN CostUSD = (%+v, nil), want an error", res)
		}
		if res.Fits {
			t.Fatal("ReserveIfFits with a NaN CostUSD reported fits=true")
		}
		if _, ok, _ := s.GetReservation(ctx, "rsv-nan"); ok {
			t.Fatal("a NaN-cost reservation was persisted")
		}
	}
}

func testListRegistrationsReturnsEveryProject(newStore func() store.Store) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, s := context.Background(), newStore()
		if got, err := s.ListRegistrations(ctx); err != nil || len(got) != 0 {
			t.Fatalf("ListRegistrations on an empty store = %+v %v, want none", got, err)
		}

		must(t, s.PutRegistration(ctx, store.Registration{
			Project: "group/a", Rig: "group-a", State: store.StateActive,
			Raw: []byte("version: 1\n"),
		}))
		must(t, s.PutRegistration(ctx, store.Registration{
			Project: "group/b", Rig: "group-b", State: store.StateDisabled,
			Raw: []byte("version: 1\n"),
		}))

		got, err := s.ListRegistrations(ctx)
		if err != nil || len(got) != 2 {
			t.Fatalf("ListRegistrations = %+v %v, want 2 projects", got, err)
		}
		seen := map[string]store.State{}
		for _, r := range got {
			seen[r.Project] = r.State
		}
		if seen["group/a"] != store.StateActive || seen["group/b"] != store.StateDisabled {
			t.Fatalf("ListRegistrations = %+v, want both projects with their own state", got)
		}

		must(t, s.DeleteRegistration(ctx, "group/a"))
		if got, err := s.ListRegistrations(ctx); err != nil || len(got) != 1 || got[0].Project != "group/b" {
			t.Fatalf("ListRegistrations after delete = %+v %v, want only group/b", got, err)
		}
	}
}
