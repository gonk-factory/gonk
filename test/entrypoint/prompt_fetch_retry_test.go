// Regression coverage for R-07: the prompt-fetch retry loop in
// images/agent/entrypoint.sh (images/agent/entrypoint.sh lines ~219-298)
// used to treat a transport failure as terminal after zero retries, despite
// a GONK_PROMPT_WAIT_SECS (default 120s) window that exists exactly to
// survive one.
//
// THE BUG, PRECISELY: `curl -sS -o f -w '%{http_code}' ... || echo 000`
// captures curl's own -w output ("000", the sentinel curl prints for
// %{http_code} when no HTTP response was ever received) AND, because curl's
// exit status is also nonzero on that same failure, the `|| echo 000`
// fallback -- both land in ONE command substitution, giving "000000". The
// old loop's `case` matched only "200" and "404" explicitly and treated
// everything else (including "000000") as a terminal status, breaking the
// loop after a single refused connection. This file exercises the fixed
// loop, which treats any non-3-digit or "000" code like "404" (keep
// waiting) while leaving every real HTTP status -- including 410 -- exactly
// as terminal as before.
//
// Uses the shared fake-PATH harness from fakepath_harness_test.go (fakebin
// curl/opencode, fakePATH, entrypointPath) plus the sequential-failure
// extension added to testdata/fakebin/curl for this task (see the comment
// block at the top of that script): FAKE_CURL_FAIL_COUNT leading
// invocations behave like a transport failure before falling through to the
// normal FAKE_CURL_EXIT/STATUS/BODY behavior, counted via
// FAKE_CURL_COUNTER_FILE since each curl invocation is a separate process.
package entrypoint

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runEntrypointBounded runs entrypoint.sh like runEntrypoint, but under a
// wall-clock deadline it enforces itself rather than trusting the caller not
// to hang forever.
//
// THIS IS NOT DECORATION. A successful non-interactive run holds the shell
// open on purpose after opencode exits (`while :; do sleep 3600; done`,
// gonk-2tb -- see the comment above that loop in entrypoint.sh) so Gas
// City's tmux-backed transcript read has something to attach to. A test
// asserting "the run started" therefore has NO natural process exit to wait
// on, and a test asserting "it exited on time" needs a hard backstop in
// case a regression turns the retry loop into a real infinite one -- a hung
// `go test` reads as "the run is slow", not "the deadline stopped working",
// which is exactly the property this file is checking for.
//
// cmd.Process is put in its OWN process group (Setpgid) so the timeout path
// can kill the whole tree with one signal to -pid: entrypoint.sh's hold
// loop's `sleep 3600` (and, on the way there, curl/opencode/tmux/jq) are
// children of the shell process, not the process this func started, so
// killing only cmd.Process would leave them running as orphans.
func runEntrypointBounded(t *testing.T, timeout time.Duration, env []string, args ...string) (output string, waitErr error, timedOut bool) {
	t.Helper()
	sh, lookErr := exec.LookPath("sh")
	if lookErr != nil {
		t.Fatalf("sh not found on the test runner's own PATH: %v", lookErr)
	}
	cmdArgs := append([]string{"-eu", entrypointPath(t)}, args...)
	cmd := exec.Command(sh, cmdArgs...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start entrypoint: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr = <-done:
		return buf.String(), waitErr, false
	case <-time.After(timeout):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return buf.String(), nil, true
	}
}

// baseEnv is what every scenario in this file needs regardless of how the
// prompt fetch resolves: a session identity, the prompt-fetch endpoint
// switched on, and a private runtime dir so nothing here ever touches
// /tmp/gonk or /etc/gonk (entrypoint.sh's real-pod defaults).
func baseEnv(t *testing.T, runtimeDir string) []string {
	t.Helper()
	return []string{
		"PATH=" + fakePATH(t),
		"GC_ALIAS=test-alias-0000000000000000",
		"GONK_PROMPT_URL=https://prompt.invalid",
		"GONK_RUNTIME_DIR=" + runtimeDir,
	}
}

// TestPromptFetchRetriesTransportFailureThenSucceeds is exit criterion 1:
// one refused connection followed by a successful fetch must still start
// the run, not exit 4 after zero retries.
func TestPromptFetchRetriesTransportFailureThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "curl_calls")
	env := append(baseEnv(t, dir),
		"GONK_PROMPT_WAIT_SECS=10",
		"GONK_MODEL=test-model",
		"GONK_LITELLM_URL=http://litellm.invalid",
		"GONK_LITELLM_KEY=fake-static-key",
		"GONK_OPENCODE_OVERLAY="+filepath.Join(dir, "opencode.json"),
		"FAKE_CURL_FAIL_COUNT=1",
		"FAKE_CURL_FAIL_EXIT=7",
		"FAKE_CURL_COUNTER_FILE="+counter,
		"FAKE_CURL_STATUS=200",
		`FAKE_CURL_BODY={"prompt":"do the assigned task"}`,
	)

	// A successful run never exits on its own (it holds for the tmux
	// transcript read), so reaching the timeout IS the passing outcome here
	// -- see runEntrypointBounded's doc comment. 8s is generous against the
	// one 2s retry sleep this scenario needs.
	out, waitErr, timedOut := runEntrypointBounded(t, 8*time.Second, env)
	if !timedOut {
		t.Fatalf("entrypoint exited on its own (err=%v) instead of reaching the tmux-hold loop after a "+
			"successful run -- one refused connection should not have stopped the retry loop:\n%s", waitErr, out)
	}

	// The loop logs nothing per-attempt (only the final outcome), so the
	// concatenated "000000" the refused first attempt actually produces is
	// not independently observable here -- TestPromptFetchExhaustsRetriesOnPersistentTransportFailure
	// asserts on it directly via the FATAL line's "last status". What this
	// test proves is the behavioral consequence: that exact string did NOT
	// stop the loop, because the run below started anyway.
	if !strings.Contains(out, "starting opencode run (non-interactive)") {
		t.Fatalf("entrypoint never reached the non-interactive run after the second (successful) fetch:\n%s", out)
	}
	if !strings.Contains(out, "session end: opencode run exited rc=0") {
		t.Errorf("opencode run did not report a clean exit:\n%s", out)
	}

	calls, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read curl call counter: %v", err)
	}
	if got := strings.TrimSpace(string(calls)); got != "2" {
		t.Errorf("expected exactly 2 curl invocations (one refusal, one success), got %s\noutput:\n%s", got, out)
	}
}

// TestPromptFetchExhaustsRetriesOnPersistentTransportFailure is exit
// criterion 2: when every attempt refuses, the loop must still terminate at
// the deadline and exit 4 with a logged reason -- the fix must not turn a
// persistent transport failure into an infinite retry.
//
// GONK_PROMPT_WAIT_SECS is driven down to 3s (rather than the 120s
// production default) so this test proves the SAME mechanism at a scale
// that finishes in seconds: the loop still measures elapsed time against
// GONK_PROMPT_WAIT_SECS and still breaks at the deadline, it is just given
// a much shorter one. Exit criterion 3 (do not change the deadline default)
// is about the entrypoint's own `${GONK_PROMPT_WAIT_SECS:-120}` fallback,
// which this test does not touch -- it overrides the env var the same way
// the entrypoint's real operators already can.
func TestPromptFetchExhaustsRetriesOnPersistentTransportFailure(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "curl_calls")
	env := append(baseEnv(t, dir),
		"GONK_PROMPT_WAIT_SECS=3",
		"FAKE_CURL_FAIL_COUNT=99999",
		"FAKE_CURL_FAIL_EXIT=7",
		"FAKE_CURL_COUNTER_FILE="+counter,
	)

	// Safety net far above the ~4-5s this scenario needs (a 3s deadline plus
	// up to one more 2s sleep before the next deadline check fires) -- if
	// this fires, the retry loop stopped terminating at all, which is
	// exactly the "infinite retry past the deadline" failure mode to rule
	// out.
	out, waitErr, timedOut := runEntrypointBounded(t, 20*time.Second, env)
	if timedOut {
		t.Fatalf("entrypoint did not exit within the 20s safety net; a persistent transport failure must "+
			"still terminate at the GONK_PROMPT_WAIT_SECS deadline:\n%s", out)
	}

	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("expected the script to exit non-zero after exhausting retries, got err=%v\noutput:\n%s", waitErr, out)
	}
	const wantExitCode = 4
	if exitErr.ExitCode() != wantExitCode {
		t.Fatalf("expected exit code %d, got %d\noutput:\n%s", wantExitCode, exitErr.ExitCode(), out)
	}

	if !strings.Contains(out, "session end: refused (no prompt)") {
		t.Errorf("exhaustion did not log a \"session end:\" reason:\n%s", out)
	}
	// Every attempt in this scenario fails the same way, so the FATAL
	// line's "last status" must still show the concatenated "000000" --
	// confirming the deadline broke out of the SAME keep-waiting branch
	// test (a) exercises, not some other early exit.
	if !strings.Contains(out, "000000") {
		t.Errorf("expected the FATAL line to name the concatenated \"000000\" last status:\n%s", out)
	}

	calls, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read curl call counter: %v", err)
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(string(calls)))
	if convErr != nil {
		t.Fatalf("curl call counter is not a number: %q", calls)
	}
	if n < 2 {
		t.Errorf("expected the loop to retry more than once before the deadline (proving this is the "+
			"exhaustion path, not a single early exit), got %d call(s)\noutput:\n%s", n, out)
	}
}

// TestPromptFetch410ExitsImmediatelyWithoutRetrying is exit criterion 3:
// this change must not touch 410 handling. A 410 is a REAL HTTP status (the
// transport succeeded; the server answered), unlike the "000"/"000000"
// transport-failure sentinels the other two tests exercise, so it must
// still break the retry loop on the very first attempt and exit 3.
func TestPromptFetch410ExitsImmediatelyWithoutRetrying(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "curl_calls")
	env := append(baseEnv(t, dir),
		"GONK_PROMPT_WAIT_SECS=30",
		"FAKE_CURL_STATUS=410",
		"FAKE_CURL_COUNTER_FILE="+counter,
	)

	start := time.Now()
	out, waitErr, timedOut := runEntrypointBounded(t, 10*time.Second, env)
	elapsed := time.Since(start)
	if timedOut {
		t.Fatalf("entrypoint did not exit within the safety-net timeout on a 410 -- it must exit "+
			"immediately without retrying:\n%s", out)
	}

	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("expected the script to exit non-zero on 410, got err=%v\noutput:\n%s", waitErr, out)
	}
	const wantExitCode = 3
	if exitErr.ExitCode() != wantExitCode {
		t.Fatalf("expected exit code %d, got %d\noutput:\n%s", wantExitCode, exitErr.ExitCode(), out)
	}
	if !strings.Contains(out, "session end: refused (prompt already consumed)") {
		t.Errorf("410 did not log the expected refusal reason:\n%s", out)
	}

	// NOT RETRIED: exactly one curl call despite a 30s window, and well
	// under it in wall-clock time -- the "do not change the 410 handling"
	// guarantee this task must hold.
	calls, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read curl call counter: %v", err)
	}
	if got := strings.TrimSpace(string(calls)); got != "1" {
		t.Errorf("expected exactly 1 curl invocation (no retry on 410), got %s\noutput:\n%s", got, out)
	}
	if elapsed > 5*time.Second {
		t.Errorf("410 took %s to exit; expected near-immediate, not a wait against the 30s window:\n%s", elapsed, out)
	}
}
