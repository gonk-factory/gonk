package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
)

// deadStore is the gonk-p7qh failure, reproduced: the sweep's own database
// refuses it. Observed live as
// `bd list --label gonk::running --json: Access denied for user 'root'`.
type deadStore struct{ beadstore.Store }

func (deadStore) List(context.Context, beadstore.State) ([]beadstore.Record, error) {
	return nil, errors.New("bd list --label gonk::running --json: Access denied for user 'root'")
}
func (deadStore) Put(context.Context, beadstore.Record) error { return nil }

// THE gonk-p7qh ACCEPTANCE CASE. A sweep that cannot list beads must (a) fail
// the pass, (b) record the outage durably, and (c) escalate from a repeated
// identical ERROR to ONE cumulative statement naming the outage duration.
//
// It PRINTS what `kubectl logs deploy/gonk-controller` shows, because the
// deliverable is the observed output.
func TestADeadStoreIsLoudAndCumulative(t *testing.T) {
	health := filepath.Join(t.TempDir(), "gonk-sweep-health.json")
	var logged bytes.Buffer

	base := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		now := base.Add(time.Duration(i) * 30 * time.Second)
		code := runSweep(context.Background(), sweepDeps{
			Store: deadStore{}, Log: testLogger(&logged), HealthFile: health,
			Now: func() time.Time { return now },
		})
		if code != 1 {
			t.Fatalf("pass %d exited %d, want 1 -- a sweep that cannot list beads has not succeeded", i, code)
		}
	}

	t.Logf("\n--- what `kubectl logs deploy/gonk-controller` now shows ---\n%s", logged.String())

	if !strings.Contains(logged.String(), "SWEEP IS DEAD") {
		t.Errorf("four failed passes never escalated past a routine ERROR:\n%s", logged.String())
	}
	// The cumulative fact: how long, and how many passes. Without these the log
	// says exactly as much after four days as after one line.
	if !strings.Contains(logged.String(), "consecutive_failures=4") {
		t.Errorf("the escalation does not count the failed passes:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "outage=1m30s") {
		t.Errorf("the escalation does not state the outage duration:\n%s", logged.String())
	}

	h, found := readSweepHealth(health)
	if !found {
		t.Fatal("no health record was written; the readiness probe would report READY through a total outage")
	}
	if h.OK || h.ConsecutiveFailures != 4 || h.Stage != "list_running" {
		t.Errorf("health record = %+v, want ok=false failures=4 stage=list_running", h)
	}
}

// Recovery must clear the run and say so once -- an operator watching a broken
// deployment needs to see it come back, not infer it from silence.
func TestRecoveryClearsTheOutageAndSaysSo(t *testing.T) {
	health := filepath.Join(t.TempDir(), "h.json")
	var logged bytes.Buffer
	now := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)

	recordSweepFailure(testLogger(&logged), health, "list_running", now, errors.New("boom"))
	recordSweepFailure(testLogger(&logged), health, "list_running", now.Add(30*time.Second), errors.New("boom"))
	logged.Reset()
	recordSweepSuccess(testLogger(&logged), health, now.Add(60*time.Second))

	if !strings.Contains(logged.String(), "RECOVERED") {
		t.Errorf("recovery was silent:\n%s", logged.String())
	}
	h, _ := readSweepHealth(health)
	if !h.OK || h.ConsecutiveFailures != 0 {
		t.Errorf("health record after recovery = %+v, want ok=true failures=0", h)
	}
}

// A healthy pass writes a fresh ok record, so the probe can tell "alive" from
// "the cooldown order stopped running".
func TestAHealthyPassRecordsItsSuccess(t *testing.T) {
	health := filepath.Join(t.TempDir(), "h.json")
	var logged bytes.Buffer
	now := time.Now()
	gc := gcapitest.New(t)
	code := runSweep(context.Background(), sweepDeps{
		Store: beadstore.NewMemory(), GC: gc.Client("gonk-city"),
		Log: testLogger(&logged), HealthFile: health,
		Now: func() time.Time { return now },
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	h, found := readSweepHealth(health)
	if !found || !h.OK {
		t.Fatalf("health record = %+v found=%v, want a healthy record", h, found)
	}
}

// --- the probe -----------------------------------------------------------

func probe(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = runSweepHealthCheck(args, &o, &e)
	return code, o.String(), e.String()
}

// A FRESH POD HAS NOT SWEPT YET, and absence must not read as failure -- a
// probe that failed on a missing file would hold a healthy controller out of
// its own Service until the first 30s tick.
func TestMissingHealthFileIsReadyNotFailed(t *testing.T) {
	code, out, errOut := probe(t, "--file", filepath.Join(t.TempDir(), "absent.json"))
	if code != 0 {
		t.Errorf("exit = %d, want 0 for a pod that has not swept yet (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "READY") {
		t.Errorf("stdout = %q, want it to say READY", out)
	}
}

// A recorded failure is the whole point: it must make the probe fail, and the
// message must say what is broken rather than just "not ready".
func TestRecordedFailureMakesTheProbeFail(t *testing.T) {
	health := filepath.Join(t.TempDir(), "h.json")
	var logged bytes.Buffer
	now := time.Now()
	recordSweepFailure(testLogger(&logged), health, "list_running", now,
		errors.New("Access denied for user 'root'"))

	code, _, errOut := probe(t, "--file", health)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 -- the outcome path is dead", code)
	}
	for _, want := range []string{"NOT READY", "list_running", "Access denied"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("probe message is missing %q:\n%s", want, errOut)
		}
	}
}

// A sweep that has STOPPED RUNNING is as dead as one that fails, and it leaves
// a stale ok record behind. The age check is what catches it.
func TestAStaleHealthRecordFailsTheProbe(t *testing.T) {
	health := filepath.Join(t.TempDir(), "h.json")
	old := time.Now().Add(-2 * time.Hour)
	b, err := json.Marshal(sweepHealth{At: old, OK: true, LastGoodAt: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(health, b, 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := probe(t, "--file", health)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 -- the cooldown order has not run for two hours", code)
	}
	if !strings.Contains(errOut, "not running") {
		t.Errorf("probe message does not explain the staleness:\n%s", errOut)
	}
	// ...and a record inside the window passes.
	fresh, err := json.Marshal(sweepHealth{At: time.Now(), OK: true, LastGoodAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(health, fresh, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := probe(t, "--file", health); code != 0 {
		t.Errorf("a fresh ok record exits %d, want 0: %s", code, errOut)
	}
}

// The probe replaces the controller's tcpSocket readinessProbe, so it must also
// do what that probe did. A container gets exactly one readinessProbe: without
// this, the change would trade one blind spot for another.
func TestProbeAlsoDialsTheSupervisorWhenAsked(t *testing.T) {
	health := filepath.Join(t.TempDir(), "h.json")
	b, _ := json.Marshal(sweepHealth{At: time.Now(), OK: true})
	if err := os.WriteFile(health, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// Nothing listening: the probe must fail even though the sweep is healthy.
	code, _, errOut := probe(t, "--file", health, "--supervisor-addr", "127.0.0.1:1", "--dial-timeout", "200ms")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 -- the supervisor port is not accepting", code)
	}
	if !strings.Contains(errOut, "supervisor") {
		t.Errorf("probe message does not name the supervisor:\n%s", errOut)
	}
}

// The default health path must be the controller's writable /city volume, and
// the env override must win. A probe reading a different file than the sweep
// writes is a probe that is always green.
func TestSweepHealthFilePathAndOverride(t *testing.T) {
	t.Setenv(sweepHealthFileEnv, "")
	if got := sweepHealthFile(); got != defaultSweepHealthFile {
		t.Errorf("sweepHealthFile() = %q, want %q", got, defaultSweepHealthFile)
	}
	t.Setenv(sweepHealthFileEnv, "/tmp/elsewhere.json")
	if got := sweepHealthFile(); got != "/tmp/elsewhere.json" {
		t.Errorf("sweepHealthFile() = %q, want the override", got)
	}
}
