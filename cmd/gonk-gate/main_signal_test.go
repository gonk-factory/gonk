package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// mainSignalHelperEnv, when set, makes TestMainDispatchStopsOnSIGTERM run the
// real main() in this (child) process instead of the test.
const mainSignalHelperEnv = "GONK_GATE_TEST_MAIN_SIGNAL_HELPER"

// main must hand dispatch the SIGNAL-AWARE context. The unit tests of
// terminationContext cannot see that wiring, and a regression there is WORSE
// than having no handler at all: signal.Notify still swallows SIGTERM, so a
// dispatch given the plain context would ignore Gas City's order-timeout
// SIGTERM until the SIGKILL, where one with no handler dies at once.
//
// So this runs the real main() in a child process (the test binary re-executed),
// parks it on a meter that never answers, sends SIGTERM, and requires exit 1
// within 1.5s -- inside Gas City's 2s grace -- with the termination logged.
func TestMainDispatchStopsOnSIGTERM(t *testing.T) {
	if os.Getenv(mainSignalHelperEnv) == "1" {
		os.Args = []string{"gonk-gate", "dispatch"}
		// The runtime is not what this tests (main's signal wiring is), and
		// the production one needs a cluster.
		newSessionRuntime = func() (runtime.SessionRuntime, error) { return newTestRuntime(), nil }
		main()
		return
	}

	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	meter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		// Never answers. (Waiting on r.Context() alone is not enough to let
		// Close finish: with an unread request body the server never notices
		// the dead child's connection.)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer meter.Close()
	defer close(release)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainDispatchStopsOnSIGTERM$")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		mainSignalHelperEnv + "=1",
		"GONK_LOG_SINK=off",
		"GONK_CITY=gonk",
		"GONK_SUPERVISOR_URL=" + meter.URL,
		"GONK_METER_URL=" + meter.URL,
		"GONK_METER_TOKEN_FILE=" + tok,
		"GC_WEBHOOK_ARG_TRIGGER=issue-triage",
		"GC_WEBHOOK_ARG_BEAD_ANCHOR=gonk:1:issue:1",
		"GC_WEBHOOK_ARG_PROJECT=g/r",
		"GC_WEBHOOK_ARG_PROJECT_ID=1",
		"GC_WEBHOOK_ARG_ISSUE_IID=1",
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()

	select {
	case <-reached:
	case err := <-exited:
		t.Fatalf("child exited before reaching the meter: %v\n%s", err, out.String())
	case <-time.After(20 * time.Second):
		t.Fatalf("child never reached the meter\n%s", out.String())
	}

	sent := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("child after SIGTERM: err=%v, want exit status 1 (a handled stop, not death by signal)\n%s", err, out.String())
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("main ignored SIGTERM for 1.5s: dispatch is not running under the signal-aware context\n%s", out.String())
	}
	if !strings.Contains(out.String(), "signalled to stop") {
		t.Fatalf("termination cause not logged:\n%s", out.String())
	}
	// main also records the launch timing the pre-exec reserve is to be
	// measured from (launch_gap.go), for every dispatch.
	if !strings.Contains(out.String(), `"msg":"gonk-gate launch timing"`) {
		t.Fatalf("launch timing record not logged by main:\n%s", out.String())
	}
	t.Logf("child exited %v after SIGTERM", time.Since(sent))
}
