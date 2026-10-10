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
//
// NO VERDICT HERE DEPENDS ON WALL-CLOCK SCHEDULING (gonk-4f0v, gonk-47l6).
// These tests used to decide pass/fail by wall time: "reached the run within
// an 8s kill" (red 36/36 under CPU load -- the shell was still forking its way
// through the retry loop when the kill fired, which says nothing about the
// loop), "exited within 5s" on 410, and "at least 2 attempts inside a real 3s
// deadline". Now:
//   - the success scenario waits for the entrypoint's OWN end-of-run log line
//     (or its exit), however long the box takes to produce it;
//   - the loop's deadline is driven by an injected clock (testdata/fakeclock/
//     date: time advances per `date +%s` call, not per second), so which
//     attempt the deadline lands on is a function of the loop, not the load;
//   - what is asserted is the retry behaviour itself: the exact sequence of
//     fetch attempts the fake curl recorded (FAKE_CURL_LOG_FILE) and the
//     outcome that followed;
//   - the only wall-time bound left is hangGuard, a deadlock backstop whose
//     failure text says so. It is never the assertion.
package entrypoint

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hangGuard is the ONLY wall-clock bound in this file, and it is a deadlock
// backstop, not a budget: every scenario below finishes on an observable
// event (a log line, or the process exiting), and hangGuard exists only so a
// regression that turns the retry loop into a real infinite one fails the test
// instead of hanging `go test`. Measured 2026-09-21 under 36 CPU burner
// processes plus three parallel -race binaries at GOMAXPROCS=2 (and another
// agent's 36 burners on the same 12 cores), the slowest correct run took
// 37.5s; this is ~8x that, and still under go test's default 10m package
// timeout. If it fires, the script is stuck, not slow. Do not tune it to make
// a test pass -- a scenario that needs it is broken.
const hangGuard = 5 * time.Minute

// runOutcome is how runEntrypointUntil's child finished.
type runOutcome int

const (
	// outcomeMarker: the marker line appeared in the output (the child was
	// then killed -- a successful run holds forever, see below).
	outcomeMarker runOutcome = iota
	// outcomeExited: the child exited on its own before any marker.
	outcomeExited
	// outcomeHung: hangGuard fired. Always a failure.
	outcomeHung
)

// runEntrypointUntil runs entrypoint.sh like runEntrypoint and returns at the
// first of: marker appearing in its output (the child is then killed),
// the child exiting on its own, or hangGuard. An empty marker means "wait for
// exit".
//
// WHY A MARKER AND NOT EXIT. A successful non-interactive run holds the shell
// open on purpose after opencode exits (`while :; do sleep 3600; done`,
// gonk-2tb -- see the comment above that loop in entrypoint.sh) so Gas City's
// tmux-backed transcript read has something to attach to. So "the run
// happened" has no natural process exit; the entrypoint's own
// "session end: opencode run exited rc=" line, printed immediately before the
// hold, is the event that says it. The previous version of this helper killed
// the child at a fixed wall time and then inspected whatever it had printed,
// which made a slow box indistinguishable from a broken retry (gonk-4f0v).
//
// The child is started by startEntrypoint (proc_harness_test.go): its own
// process group, so one signal to -pgid kills the whole tree (the hold loop's
// `sleep 3600`, and curl/opencode/tmux/jq on the way there), Pdeathsig so it
// dies with this binary, and a t.Cleanup that kills the group and FAILS the
// test if anything in it survives (gonk-gp44).
func runEntrypointUntil(t *testing.T, marker string, env []string, args ...string) (output string, outcome runOutcome, waitErr error) {
	t.Helper()
	return runEntrypointUntilPrefixed(t, nil, marker, env, args...)
}

// runEntrypointUntilPrefixed is runEntrypointUntil with a command prefix
// (e.g. an `unshare` invocation that makes the script PID 1).
func runEntrypointUntilPrefixed(t *testing.T, prefix []string, marker string, env []string, args ...string) (output string, outcome runOutcome, waitErr error) {
	t.Helper()
	p := startEntrypoint(t, prefix, marker, env, args...)

	guard := time.NewTimer(hangGuard)
	defer guard.Stop()

	select {
	case <-p.exited:
		// Exited on its own. If the marker is in the output anyway (it
		// cannot be for the hold-forever success path, but a future marker
		// might precede an exit), report it as the marker outcome.
		select {
		case <-p.out.seen:
			return p.out.String(), outcomeMarker, p.waitErr
		default:
			return p.out.String(), outcomeExited, p.waitErr
		}
	case <-p.out.seen:
		p.killGroup()
		<-p.exited
		return p.out.String(), outcomeMarker, nil
	case <-guard.C:
		p.killGroup()
		<-p.exited
		return p.out.String(), outcomeHung, nil
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

// clockEnv is baseEnv with the injected clock (testdata/fakeclock/date) on
// PATH: every `date +%s` the entrypoint makes advances time by exactly 1s,
// starting at fakeClockStart. The deadline check therefore lands on the same
// attempt every run, whatever the scheduler does. The returned path is the
// clock's call counter, so a test can confirm the fake was actually consulted
// (a PATH mistake would otherwise fall back to the real date silently).
const fakeClockStart = 1000000

func clockEnv(t *testing.T, runtimeDir string) (env []string, clockFile string) {
	t.Helper()
	clockFile = filepath.Join(runtimeDir, "fake_date_calls")
	env = baseEnv(t, runtimeDir)
	env[0] = "PATH=" + fakeClockPATH(t) // baseEnv's first entry is PATH
	env = append(env,
		"FAKE_DATE_FILE="+clockFile,
		"FAKE_DATE_START="+strconv.Itoa(fakeClockStart),
		"FAKE_DATE_STEP=1",
	)
	return env, clockFile
}

// readAttempts returns the fake curl's per-call attempt log (FAKE_CURL_LOG_FILE).
func readAttempts(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read curl attempt log %s: %v", path, err)
	}
	return string(b)
}

// requireClockConsulted fails unless the injected clock answered at least one
// `date +%s` -- otherwise the real date served the deadline and this test's
// determinism is an illusion.
func requireClockConsulted(t *testing.T, clockFile string) {
	t.Helper()
	b, err := os.ReadFile(clockFile)
	if err != nil || strings.TrimSpace(string(b)) == "" || strings.TrimSpace(string(b)) == "0" {
		t.Fatalf("the injected clock (testdata/fakeclock/date) was never consulted (err=%v, calls=%q): "+
			"the entrypoint's deadline ran on the REAL clock, so this test is not deterministic", err, b)
	}
}

// TestPromptFetchRetriesTransportFailureThenSucceeds is exit criterion 1:
// one refused connection followed by a successful fetch must still start
// the run, not exit 4 after zero retries.
//
// WHAT THIS PROVES, AND HOW. The fake curl refuses its first call and answers
// 200 on its second, logging each. The test asserts that exact attempt
// sequence ("1 transport-failure", then "2 status=200") -- i.e. the loop DID
// retry after the refusal and stopped on the success -- and that the run then
// started and finished. The deadline (10 injected seconds, 1 per `date` call)
// cannot land before the second attempt under any load, so a failure here is
// the loop's fault, never the scheduler's: a loop that treats the refusal as
// terminal exits 4 (outcomeExited), and one that gives up before its deadline
// does the same.
func TestPromptFetchRetriesTransportFailureThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "curl_attempts")
	env, clockFile := clockEnv(t, dir)
	env = append(env,
		"GONK_PROMPT_WAIT_SECS=10",
		"GONK_MODEL=test-model",
		"GONK_LITELLM_URL=http://litellm.invalid",
		"GC_WEBHOOK_ARG_LITELLM_KEY=fake-session-key",
		"GONK_OPENCODE_OVERLAY="+filepath.Join(dir, "opencode.json"),
		"FAKE_CURL_FAIL_COUNT=1",
		"FAKE_CURL_FAIL_EXIT=7",
		"FAKE_CURL_COUNTER_FILE="+filepath.Join(dir, "curl_calls"),
		"FAKE_CURL_LOG_FILE="+attempts,
		"FAKE_CURL_STATUS=200",
		`FAKE_CURL_BODY={"prompt":"do the assigned task"}`,
	)

	// The line printed immediately before the tmux-hold loop: the run
	// started and opencode returned. See runEntrypointUntil.
	const ranToCompletion = "session end: opencode run exited rc="
	out, outcome, waitErr := runEntrypointUntil(t, ranToCompletion, env)
	switch outcome {
	case outcomeHung:
		t.Fatalf("hang guard (%s) fired: the entrypoint neither exited nor reached %q -- it is stuck, "+
			"not slow:\n%s", hangGuard, ranToCompletion, out)
	case outcomeExited:
		t.Fatalf("entrypoint exited on its own (err=%v) instead of running opencode and holding -- "+
			"one refused connection should not have stopped the retry loop:\n%s", waitErr, out)
	}
	requireClockConsulted(t, clockFile)

	// THE RETRY ITSELF: exactly one refusal, then exactly one success, and
	// no further fetch. The loop logs nothing per attempt, so this record
	// comes from the fake curl (FAKE_CURL_LOG_FILE).
	const wantAttempts = "1 transport-failure exit=7\n2 status=200 exit=0\n"
	if got := readAttempts(t, attempts); got != wantAttempts {
		t.Errorf("prompt-fetch attempt sequence:\n got: %q\nwant: %q\noutput:\n%s", got, wantAttempts, out)
	}

	// The concatenated "000000" the refused first attempt produces is not
	// independently observable here -- TestPromptFetchExhaustsRetriesOnPersistentTransportFailure
	// asserts on it via the FATAL line's "last status". What this test
	// proves is its behavioural consequence: that string did NOT stop the
	// loop, because the second attempt happened and the run below started.
	for _, want := range []string{
		"prompt fetched (",
		"starting opencode run (non-interactive)",
		"session end: opencode run exited rc=0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// TestPromptFetchExhaustsRetriesOnPersistentTransportFailure is exit
// criterion 2: when every attempt refuses, the loop must still terminate at
// the deadline and exit 4 with a logged reason -- the fix must not turn a
// persistent transport failure into an infinite retry.
//
// THE DEADLINE RUNS ON THE INJECTED CLOCK. GONK_PROMPT_WAIT_SECS=3 with time
// advancing 1s per `date +%s` call: the deadline is taken at T, the checks
// after attempts 1, 2 and 3 read T+1, T+2 and T+3, so the loop makes EXACTLY
// three attempts and breaks on the third check. That count is a property of
// the loop, not of how fast the box forks -- the earlier version asserted
// ">= 2 attempts inside a real 3s window", which is counting events in a
// wall-clock window. Exit criterion 3 (do not change the deadline default) is
// about the entrypoint's own `${GONK_PROMPT_WAIT_SECS:-120}` fallback, which
// this test does not touch.
func TestPromptFetchExhaustsRetriesOnPersistentTransportFailure(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "curl_attempts")
	env, clockFile := clockEnv(t, dir)
	env = append(env,
		"GONK_PROMPT_WAIT_SECS=3",
		"FAKE_CURL_FAIL_COUNT=99999",
		"FAKE_CURL_FAIL_EXIT=7",
		"FAKE_CURL_COUNTER_FILE="+filepath.Join(dir, "curl_calls"),
		"FAKE_CURL_LOG_FILE="+attempts,
	)

	out, outcome, waitErr := runEntrypointUntil(t, "", env)
	if outcome == outcomeHung {
		t.Fatalf("hang guard (%s) fired: a persistent transport failure must still terminate at the "+
			"GONK_PROMPT_WAIT_SECS deadline, and this one never did:\n%s", hangGuard, out)
	}
	requireClockConsulted(t, clockFile)

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
	// Every attempt fails the same way, so the FATAL line's "last status"
	// must show the concatenated "000000" -- confirming the deadline broke
	// out of the SAME keep-waiting branch the success test exercises, not
	// some other early exit.
	if !strings.Contains(out, "000000") {
		t.Errorf("expected the FATAL line to name the concatenated \"000000\" last status:\n%s", out)
	}

	// Retried up to the deadline and not one attempt past it.
	const wantAttempts = "1 transport-failure exit=7\n2 transport-failure exit=7\n3 transport-failure exit=7\n"
	if got := readAttempts(t, attempts); got != wantAttempts {
		t.Errorf("prompt-fetch attempt sequence (3 injected seconds, 1 per date call):\n got: %q\nwant: %q\noutput:\n%s",
			got, wantAttempts, out)
	}
}

// TestPromptFetch410ExitsImmediatelyWithoutRetrying is exit criterion 3:
// this change must not touch 410 handling. A 410 is a REAL HTTP status (the
// transport succeeded; the server answered), unlike the "000"/"000000"
// transport-failure sentinels the other two tests exercise, so it must
// still break the retry loop on the very first attempt and exit 3.
//
// "IMMEDIATELY" IS PROVEN BY THE ATTEMPT LOG, NOT A STOPWATCH (gonk-47l6).
// This used to also assert elapsed < 5s, which measured the scheduler (4.41s
// and 4.64s observed under load on a correct loop) and added nothing: a loop
// that waited against the 30s window would do so by RETRYING, and a second
// entry in the attempt log is exactly that, observed directly.
func TestPromptFetch410ExitsImmediatelyWithoutRetrying(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "curl_attempts")
	env, clockFile := clockEnv(t, dir)
	env = append(env,
		"GONK_PROMPT_WAIT_SECS=30",
		"FAKE_CURL_STATUS=410",
		"FAKE_CURL_COUNTER_FILE="+filepath.Join(dir, "curl_calls"),
		"FAKE_CURL_LOG_FILE="+attempts,
	)

	out, outcome, waitErr := runEntrypointUntil(t, "", env)
	if outcome == outcomeHung {
		t.Fatalf("hang guard (%s) fired on a 410 -- it must exit without retrying:\n%s", hangGuard, out)
	}
	requireClockConsulted(t, clockFile)

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

	// NOT RETRIED: exactly one attempt despite a 30s window -- the "do not
	// change the 410 handling" guarantee this task must hold.
	const wantAttempts = "1 status=410 exit=0\n"
	if got := readAttempts(t, attempts); got != wantAttempts {
		t.Errorf("prompt-fetch attempt sequence (no retry on 410):\n got: %q\nwant: %q\noutput:\n%s", got, wantAttempts, out)
	}
}
