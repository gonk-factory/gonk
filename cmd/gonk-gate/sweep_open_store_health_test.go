package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// mainOpenStoreHelperEnv, when set, makes this test run the real main() in the
// child process instead of the test.
const mainOpenStoreHelperEnv = "GONK_GATE_TEST_MAIN_OPEN_STORE_HELPER"

// gonk-p44x (d): a sweep that cannot OPEN its work store must record the
// failure where the readiness probe looks, under stage "open_work_store", and
// exit non-zero. Nothing tested that wiring: deleting the recordSweepFailure
// call in main left the whole suite green, and the cost of that regression is
// silent -- the controller stays READY while every sweep pass dies at the
// store, and nothing says so until sweepHealthMaxAge of silence expires
// (gonk-p7qh's lesson, which is why the call exists).
//
// It runs the real main() in a child process, because the wiring under test is
// in main's `sweep` case, and asserts the record it wrote.
func TestSweepThatCannotOpenItsWorkStoreRecordsItInSweepHealth(t *testing.T) {
	if os.Getenv(mainOpenStoreHelperEnv) == "1" {
		os.Args = []string{"gonk-gate", "sweep"}
		// A working runtime, so the store is the only thing that can fail.
		newSessionRuntime = func() (runtime.SessionRuntime, error) { return newTestRuntime(), nil }
		main()
		return
	}

	dir := t.TempDir()
	health := filepath.Join(dir, "gonk-sweep-health.json")
	// A DSN nothing answers on: port 1 on loopback, with a short connect
	// timeout so the child fails fast rather than burning the store's own.
	dsn := dsnFile(t, "postgres://gonk:pw@127.0.0.1:1/gonk?sslmode=disable&connect_timeout=2")
	tok := filepath.Join(dir, "meter-token")
	if err := os.WriteFile(tok, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSweepThatCannotOpenItsWorkStoreRecordsItInSweepHealth$")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		mainOpenStoreHelperEnv + "=1",
		"GONK_LOG_SINK=off",
		"GONK_CITY=gonk",
		"GONK_METER_URL=http://127.0.0.1:1",
		"GONK_SUPERVISOR_URL=http://127.0.0.1:1",
		"GONK_METER_TOKEN_FILE=" + tok,
		sweepHealthFileEnv + "=" + health,
		workStoreDSNFileEnv + "=" + dsn,
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the sweep exited 0 with an unreachable work store:\n%s", out)
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("exit code %d, want 1 (unreachable, not a misconfiguration):\n%s", code, out)
	}

	b, rerr := os.ReadFile(health)
	if rerr != nil {
		t.Fatalf("no sweep health record was written (%v); the readiness probe would keep reporting the last good pass:\n%s", rerr, out)
	}
	var h sweepHealth
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("sweep health record does not decode: %v\n%s", err, b)
	}
	if h.OK || h.Stage != "open_work_store" || h.ConsecutiveFailures != 1 || h.At.IsZero() || h.FirstFailureAt.IsZero() {
		t.Fatalf("sweep health = %+v; want ok=false stage=open_work_store consecutive_failures=1, stamped", h)
	}
	if !strings.Contains(h.Err, "work store") {
		t.Errorf("sweep health err = %q; want it to name the work store", h.Err)
	}
}

// mainNoRuntimeHelperEnv, when set, makes the no-runtime test run the real
// main() in the child process.
const mainNoRuntimeHelperEnv = "GONK_GATE_TEST_MAIN_NO_RUNTIME_HELPER"

// A sweep that cannot build its SessionRuntime (gonk-t55) cannot see a single
// session: that is a dead outcome path exactly like a dead store, so it is
// recorded where the readiness probe looks -- stage session_runtime -- and it
// is a MISCONFIGURATION (exit 2), not a transient. This runs the real main()
// with the production newSessionRuntime, which outside a cluster cannot build
// one.
func TestSweepWithNoSessionRuntimeRecordsItInSweepHealth(t *testing.T) {
	if os.Getenv(mainNoRuntimeHelperEnv) == "1" {
		os.Args = []string{"gonk-gate", "sweep"}
		main()
		return
	}
	dir := t.TempDir()
	health := filepath.Join(dir, "gonk-sweep-health.json")
	tok := filepath.Join(dir, "meter-token")
	if err := os.WriteFile(tok, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSweepWithNoSessionRuntimeRecordsItInSweepHealth$")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		mainNoRuntimeHelperEnv + "=1",
		"GONK_LOG_SINK=off",
		"GONK_CITY=gonk",
		"GONK_METER_URL=http://127.0.0.1:1",
		"GONK_SUPERVISOR_URL=http://127.0.0.1:1",
		"GONK_METER_TOKEN_FILE=" + tok,
		sweepHealthFileEnv + "=" + health,
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the sweep exited 0 with no session runtime:\n%s", out)
	}
	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Errorf("exit code %d, want 2 (misconfiguration):\n%s", code, out)
	}
	b, rerr := os.ReadFile(health)
	if rerr != nil {
		t.Fatalf("no sweep health record was written (%v):\n%s", rerr, out)
	}
	var h sweepHealth
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("sweep health record does not decode: %v\n%s", err, b)
	}
	if h.OK || h.Stage != "session_runtime" || h.ConsecutiveFailures != 1 {
		t.Fatalf("sweep health = %+v; want ok=false stage=session_runtime consecutive_failures=1", h)
	}
}
