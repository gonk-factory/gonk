package main

import (
	"context"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
)

// gonk-72np. gonk-r4u9 let a running record that cannot be judged fall through
// once it is past ReservationExpiresAt -- but a ZERO ReservationExpiresAt read
// as "never expires", so a running record with NO deadline was exactly as
// stuck as gonk:63:scaffold was: listed by every tick, returned from on every
// one, forever.
//
// Such a record is reachable. Both StateRunning writers copy the field from
// meter's DecideResponse, whose wire field is `omitzero`
// (pkg/meterapi/meterapi.go), and the T-41 import copies a bd record verbatim,
// quarantining only records older than a day -- so a fresh zero-deadline record
// imports as it stands.
//
// The rule now: a record with no deadline of its own expires at
// UpdatedAt + opercfg.DefaultReservationTTL -- the age at which meter's default
// would have expired the reservation that should have come with it.
func TestSweepSettlesARunningRecordWithNoDeadline(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)

	store := beadstore.NewMemory()
	rec := sessionlessRunningRecord(p.ID, time.Time{}) // NO deadline, no session
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seeded, _, err := store.Get(context.Background(), rec.BeadAnchor)
	if err != nil || seeded.UpdatedAt.IsZero() {
		t.Fatalf("seed read-back: %+v, %v -- the store must stamp UpdatedAt", seeded, err)
	}

	fm := &fakeOutcomeMeter{outcomeNext: "retry"}
	rt := newTestRuntime()
	orders := &fakeOrders{}
	meter := meterClient(fm.server(t))

	clock := seeded.UpdatedAt
	tick := func() {
		t.Helper()
		code := runSweep(context.Background(), sweepDeps{
			Meter: meter, Runtime: rt, Orders: orders, GL: gl.Client(),
			Store: store, BotUsername: "gonk", PackDir: repoPackDir,
			Now: func() time.Time { return clock },
		})
		if code != 0 {
			t.Fatalf("exit = %d at %v", code, clock)
		}
		clock = clock.Add(30 * time.Second)
	}

	// INSIDE THE BOUND IT STILL WAITS: the fallback is a deadline, not a
	// licence to settle a young record on sight.
	for clock.Before(seeded.UpdatedAt.Add(opercfg.DefaultReservationTTL)) {
		tick()
	}
	if n := len(fm.requests()); n != 0 {
		t.Fatalf("reported %d outcomes for a record younger than the fallback TTL (%v); it must wait",
			n, opercfg.DefaultReservationTTL)
	}

	// PAST IT, IT SETTLES. Another 100 minutes of 30s ticks.
	for i := 0; i < 200; i++ {
		tick()
	}
	reqs := fm.requests()
	if len(reqs) == 0 {
		got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
		t.Fatalf("no outcome reported for a running record with no deadline, %v after its last write; "+
			"it is still %q and will be forever", clock.Sub(seeded.UpdatedAt), got.State)
	}
	if reqs[0].Outcome != meterapi.OutcomeInfraFailed {
		t.Fatalf("outcome = %q, want infra-failed: a missing deadline is INFRA, it must not buy a rung",
			reqs[0].Outcome)
	}
	if len(reqs) > 20 {
		t.Fatalf("reported %d outcomes over 200 ticks -- one per tick walks the ladder for free", len(reqs))
	}
}

// The deadline rule itself, including the one input the store cannot produce
// today: a record with neither a deadline nor a write time. Nothing bounds that
// record by time at all, and every current Put stamps UpdatedAt, so such a
// record is legacy by construction -- it reads as ALREADY EXPIRED, because the
// alternative is "never", which is the bug.
func TestRunningDeadline(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		rec  beadstore.Record
		want time.Time
	}{
		{"own deadline wins", beadstore.Record{ReservationExpiresAt: at, UpdatedAt: at.Add(-time.Hour)}, at},
		{"no deadline: write time + default TTL", beadstore.Record{UpdatedAt: at}, at.Add(opercfg.DefaultReservationTTL)},
		{"neither: already past", beadstore.Record{}, time.Time{}},
	}
	for _, c := range cases {
		if got := runningDeadline(c.rec); !got.Equal(c.want) {
			t.Errorf("%s: runningDeadline = %v, want %v", c.name, got, c.want)
		}
	}
	if !reservationExpired(beadstore.Record{}, at) {
		t.Error("a record with no deadline and no write time must read as expired, never as 'never expires'")
	}
}
