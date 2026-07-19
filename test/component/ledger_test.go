//go:build component

package component_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// Race shape: a $1.00 monthly ceiling with glm at $0.40/attempt has headroom for
// EXACTLY 2 concurrent reservations (2 x $0.40 = $0.80 <= $1.00 < $1.20). This is
// the identical shape internal/meter/store's Postgres race test uses, one layer
// down -- here it runs through the real /decide in the real meter binary.
const (
	raceWriters = 32
	raceCeiling = 1.00
	raceWinners = 2
	raceRung    = "glm"
)

// fireConcurrentDecides fires n concurrent /decide calls at meter m for project,
// each with a DISTINCT bead (so they compete for headroom instead of deduping to
// one reservation), and returns how many were granted `run`.
func fireConcurrentDecides(t *testing.T, m *meterProc, project string, beadPrefix string, n int) int {
	t.Helper()
	var runs int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once -- maximize the race
			d, status := m.Decide(decideReq(project, fmt.Sprintf("%s-%d", beadPrefix, i), fmt.Sprintf("s-%s-%d", beadPrefix, i)))
			if status == 200 && d.Decision == meterapi.DecisionRun {
				atomic.AddInt64(&runs, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return int(runs)
}

// TestRaceConcurrentDecidesCannotOverspend_RealStore (P3-9). The end-to-end form
// of Plan 03 Task 0b: the SAME money-safety property, through the real /decide,
// in the real meter binary, against the real Postgres ledger -- the only
// configuration that ships. 32 concurrent decides, headroom for exactly 2, exactly
// 2 run. A serialization failure that yields a `defer` is a LEGITIMATE lost race,
// not an error. Run with -count=5.
func TestRaceConcurrentDecidesCannotOverspend_RealStore(t *testing.T) {
	w := newWorld(t)
	m := w.startMeter(meterOpts{})
	project := "acme/race-" + harnessShortID()
	m.MustRegister(project, 9001, gonkYMLBudgeted(raceCeiling, raceRung))

	runs := fireConcurrentDecides(t, m, project, "gk", raceWriters)
	if runs != raceWinners {
		t.Fatalf("%d of %d concurrent decides ran; want EXACTLY %d (a $%.2f ceiling with $0.40/attempt "+
			"has headroom for %d). More than %d is a real-money overspend the store failed to serialize.",
			runs, raceWriters, raceWinners, raceCeiling, raceWinners, raceWinners)
	}
}

// TestRaceMeterRestartMidRaceDoesNotOpenHeadroom (P3-9, the nastiest version).
// Reservations are durable (Decision 10) precisely so a crash cannot open a hole.
// Fire the first wave, SIGKILL the meter, restart it against the SAME ledger, and
// fire a second wave: STILL exactly 2 winners in total. If a restart resurrects
// headroom, reservations are not durable and Plan 03's failure matrix is wrong.
func TestRaceMeterRestartMidRaceDoesNotOpenHeadroom(t *testing.T) {
	w := newWorld(t)
	m := w.startMeter(meterOpts{})
	project := "acme/race-restart-" + harnessShortID()
	m.MustRegister(project, 9002, gonkYMLBudgeted(raceCeiling, raceRung))

	// Wave 1 fills the headroom (2 winners).
	w1 := fireConcurrentDecides(t, m, project, "gk-a", raceWriters/2)

	// Crash and restart against the same durable Postgres ledger.
	m.SigkillNow()
	m.Restart()

	// Wave 2 (distinct beads) must find NO headroom -- the wave-1 reservations
	// survived the crash.
	w2 := fireConcurrentDecides(t, m, project, "gk-b", raceWriters/2)

	if w1+w2 != raceWinners {
		t.Fatalf("total winners across a crash+restart = %d (wave1=%d wave2=%d); want EXACTLY %d. "+
			"A restart that resurrected headroom means reservations are not durable.", w1+w2, w1, w2, raceWinners)
	}
	t.Logf("durable reservations held across SIGKILL+restart: wave1=%d wave2=%d total=%d", w1, w2, w1+w2)
}

// TestRaceTwoMeterReplicasCannotOverspend (P3-9 / AD-10). With >1 replica the
// in-process keyedMutex does NOTHING and the ceiling is defended by the STORE
// ALONE. AD-10 was LIFTED (Postgres serializes the race via SERIALIZABLE/FOR
// UPDATE), so this MUST PASS. Two meter processes, one ledger, one project, N
// concurrent decides split across both: still exactly 2 winners. A failure here
// is a real finding -- report it, do not delete the test.
func TestRaceTwoMeterReplicasCannotOverspend(t *testing.T) {
	w := newWorld(t)
	ledgerDSN := w.PG.FreshLedger(t)
	a := w.startMeter(meterOpts{LedgerDSN: ledgerDSN, ListenPort: harness_PortMeterA})
	b := w.startMeter(meterOpts{LedgerDSN: ledgerDSN, ListenPort: harness_PortMeterB})

	project := "acme/race-2rep-" + harnessShortID()
	a.MustRegister(project, 9003, gonkYMLBudgeted(raceCeiling, raceRung))
	b.SyncSpend() // b shares the store; make sure it is Ready and sees the registration

	var runs int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	fire := func(m *meterProc, prefix string, n int) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				d, status := m.Decide(decideReq(project, fmt.Sprintf("%s-%d", prefix, i), fmt.Sprintf("s-%s-%d", prefix, i)))
				if status == 200 && d.Decision == meterapi.DecisionRun {
					atomic.AddInt64(&runs, 1)
				}
			}(i)
		}
	}
	fire(a, "gk-a", raceWriters/2)
	fire(b, "gk-b", raceWriters/2)
	close(start)
	wg.Wait()

	if int(runs) != raceWinners {
		t.Fatalf("%d decides ran across TWO replicas; want EXACTLY %d. The store alone must defend the ceiling "+
			"(AD-10 lifted); if this exceeds %d, two-replica meter overspends and replicas:1 must stand.",
			runs, raceWinners, raceWinners)
	}
	t.Logf("two meter replicas, one Postgres ledger: exactly %d winners -- the store serializes the race", runs)
}

const (
	harness_PortMeterA = 9091
	harness_PortMeterB = 9092
)
