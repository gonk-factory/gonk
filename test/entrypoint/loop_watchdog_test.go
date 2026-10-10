// The loop watchdog (gonk-un7z): under GONK_SESSION_RUNTIME=jobs the
// entrypoint passes opencode's output through a line filter that stops the
// session the moment the SAME tool call fails for the second time, so a model
// looping on a failing call stops spending tokens instead of being classified
// after it has spent them.
//
// The fixtures in testdata/opencode-output are the REAL lines from the pod
// log of agentic/rom#1 attempt 1 (Job gonk-triage-p84-i1-a1, 2026-10-04,
// opencode 1.18.3), ANSI colour codes and all -- cut, never retyped:
//
//	rom1-loop.txt                   up to and including the SECOND failure of
//	                                the same Read (adapter.go): must fire.
//	rom1-two-distinct-failures.txt  adapter.go and adapter_test.go failing
//	                                once each: must NOT fire.
//	rom1-successful-repeat.txt      handlers.go Read twice, both succeeding:
//	                                must NOT fire.
//	fenced-answer.txt               the successful lines plus a GONK_BATCH
//	                                fence: must pass through byte for byte.
//
// The malformed-* fixtures (gonk-mf3v) are REAL opencode 1.18.3 output too,
// cut from the pod logs of the k3d component test
// TestComponentJobsMalformedToolCallsAreToldApartByTheWatchdog (2026-10-04,
// the real agent image; everything opencode printed between the watchdog's
// "armed" line and the entrypoint's "session end" line). The stub model there
// sends the two argument strings the model sent on the first live pipeline
// diagnosis (project 75, pipeline 3246, attempt 1) -- each with one closing
// brace too many. They are NOT cut from that live session's own pod log,
// which was gone with its Job; the "Invalid Tool" and detail lines are the
// ones that session's sweep log and the bead quote.
//
//	malformed-two-different-calls.txt  glob "**/.gitlab-ci*" then glob
//	                                   "README*", both malformed, then an
//	                                   answer: must NOT fire (it did, live).
//	malformed-same-call-twice.txt      glob "README*" malformed twice: must
//	                                   fire, naming the call.
//
// The component test (cmd/gonk-gate/jobs_component_test.go) runs the same
// watchdog against REAL opencode in the real image; these tests pin the
// script's logic against the fake.
package entrypoint

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gonkruntime "github.com/gonk-factory/gonk/pkg/runtime"
)

// loopHangSecs is how long the looping fake opencode lingers after printing
// its lines (it and its child). Only the watchdog's kill ends it sooner. A
// SAFETY NET in the sense of docs/testing-time.md rule 5, not a budget: the
// pass condition is the exit code, the rc=137 line and the processes being
// dead -- never how long the run took. If the watchdog does NOT fire, the fake
// exits 0 after this long, and the test fails on the exit code.
const loopHangSecs = 120

// rom1LoopCall is the signature the watchdog must name for rom1-loop.txt:
// tool and arguments, ANSI stripped, opencode's " failed" marker removed.
const rom1LoopCall = "Read internal/widget/adapter.go [limit=2000]"

func opencodeOutputFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(testdataDir(t), "opencode-output", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s: %v", p, err)
	}
	return p
}

// fakeOpencodeProcs is what the fake opencode recorded about itself.
type fakeOpencodeProcs struct {
	pid, pgid, child int // child is 0 when the fake started none
}

func readFakeOpencodeProcs(t *testing.T, path string) fakeOpencodeProcs {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake opencode recorded no pids (%v): `opencode run` never ran", err)
	}
	f := strings.Fields(string(b))
	if len(f) != 3 {
		t.Fatalf("pid file %q: want \"<pid> <pgid> <child|->\"", b)
	}
	var p fakeOpencodeProcs
	if p.pid, err = strconv.Atoi(f[0]); err != nil {
		t.Fatalf("pid file %q: %v", b, err)
	}
	if p.pgid, err = strconv.Atoi(f[1]); err != nil {
		t.Fatalf("pid file %q: %v", b, err)
	}
	if f[2] != "-" {
		if p.child, err = strconv.Atoi(f[2]); err != nil {
			t.Fatalf("pid file %q: %v", b, err)
		}
	}
	return p
}

// killFakeOpencodeOnCleanup makes sure nothing the fake started outlives the
// test, whatever the assertions found. In jobs mode the fake runs in ITS OWN
// process group (the watchdog's setsid), outside the harness's group, so the
// harness's own cleanup (startEntrypoint) cannot reach it.
func killFakeOpencodeOnCleanup(t *testing.T, pidFile string) {
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		f := strings.Fields(string(b))
		if len(f) == 3 {
			if g, err := strconv.Atoi(f[1]); err == nil && g > 1 {
				_ = syscall.Kill(-g, syscall.SIGKILL)
			}
		}
		for _, s := range f {
			if n, err := strconv.Atoi(s); err == nil && n > 1 {
				_ = syscall.Kill(n, syscall.SIGKILL)
			}
		}
	})
}

// requireDead fails t unless pid is dead (or a zombie). Polls with the
// survivor SAFETY NET: SIGKILL takes effect at once, so this only stops a
// genuine survivor from wedging the suite, and the failure says it SURVIVED.
func requireDead(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(survivorGuard)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Errorf("%s (pid %d) SURVIVED the loop watchdog: the kill did not reach opencode's whole process group", what, pid)
			return
		}
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // poll interval of a wait-until with a safety-net deadline (docs/testing-time.md rule 6)
	}
}

// requireVerbatim fails t unless the fixture's bytes appear, contiguously and
// unchanged, in out: the pod log is the transcript and the fence's carrier,
// so the filter must not alter a byte of what it passes through.
func requireVerbatim(t *testing.T, out, fixture string) {
	t.Helper()
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, string(b)) {
		t.Errorf("opencode's output did not reach the log byte for byte; want the whole of %s (%d bytes) "+
			"as one unchanged run of the output:\n%q\noutput:\n%q", filepath.Base(fixture), len(b), b, out)
	}
}

// TestLoopWatchdogStopsTheRom1Loop is the bead's case: the second identical
// failing Read stops the session -- exit runtime.ExitLoopWatchdog, the line
// naming the call and the count, opencode and its child killed (the kill
// reaches the whole group), and opencode's output passed through untouched.
func TestLoopWatchdogStopsTheRom1Loop(t *testing.T) {
	s := newJobsSession(t)
	pidFile := filepath.Join(s.dir, "opencode.pids")
	killFakeOpencodeOnCleanup(t, pidFile)
	fixture := opencodeOutputFixture(t, "rom1-loop.txt")
	out, code := runJobs(t, s.env(t,
		"FAKE_OPENCODE_RUN_OUTPUT="+fixture,
		"FAKE_OPENCODE_RUN_HANG_SECS="+strconv.Itoa(loopHangSecs),
		"FAKE_OPENCODE_PID_FILE="+pidFile,
	))
	if code != gonkruntime.ExitLoopWatchdog {
		t.Fatalf("exit %d, want %d (runtime.ExitLoopWatchdog): the watchdog did not stop the loop:\n%s",
			code, gonkruntime.ExitLoopWatchdog, out)
	}
	want := fmt.Sprintf("gonk-agent-entrypoint: loop watchdog: stopped opencode -- the same tool call failed 2 times (threshold 2): %s\n", rom1LoopCall)
	if !strings.Contains(out, want) {
		t.Errorf("no watchdog line %q (count 2, the call with ANSI stripped and \" failed\" removed):\n%s", want, out)
	}
	// SIGKILLed, not exited: opencode's own wait status is 128+9. Proves it
	// was the KILL that ended it, not the fake's linger running out.
	if !strings.Contains(out, "session end: opencode run exited rc=137\n") {
		t.Errorf("opencode was not ended by SIGKILL (want rc=137):\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("session end: runtime=jobs -- exiting %d (loop watchdog)", gonkruntime.ExitLoopWatchdog)) {
		t.Errorf("no session-end line for the watchdog exit:\n%s", out)
	}
	requireVerbatim(t, out, fixture)

	procs := readFakeOpencodeProcs(t, pidFile)
	if procs.pgid != procs.pid {
		t.Errorf("opencode ran in process group %d, want its own (%d): without its own group the kill cannot reach its children", procs.pgid, procs.pid)
	}
	requireDead(t, procs.pid, "opencode")
	if procs.child == 0 {
		t.Fatalf("the fake opencode started no child; this test proves nothing about the group kill")
	}
	requireDead(t, procs.child, "opencode's child process")
}

// TestLoopWatchdogIgnoresWhatIsNotALoop: failing calls that are DIFFERENT,
// and a successful call repeated, never fire; neither does the rom#1 loop
// when the threshold is raised above its count. opencode's rc passes through.
func TestLoopWatchdogIgnoresWhatIsNotALoop(t *testing.T) {
	cases := []struct {
		name, fixture string
		extra         []string
	}{
		{"two different failing calls", "rom1-two-distinct-failures.txt", nil},
		{"the same successful call twice", "rom1-successful-repeat.txt", nil},
		{"the rom1 loop under threshold 3", "rom1-loop.txt", []string{"GONK_LOOP_WATCHDOG_THRESHOLD=3"}},
		// gonk-mf3v: opencode prints every malformed call as the same bare
		// "Invalid Tool" line; what was called is on the line after it.
		{"two different malformed (Invalid Tool) calls", "malformed-two-different-calls.txt", nil},
		{"the same malformed call twice under threshold 3", "malformed-same-call-twice.txt", []string{"GONK_LOOP_WATCHDOG_THRESHOLD=3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newJobsSession(t)
			pidFile := filepath.Join(s.dir, "opencode.pids")
			killFakeOpencodeOnCleanup(t, pidFile)
			fixture := opencodeOutputFixture(t, c.fixture)
			out, code := runJobs(t, s.env(t, append([]string{
				"FAKE_OPENCODE_RUN_OUTPUT=" + fixture,
				"FAKE_OPENCODE_PID_FILE=" + pidFile,
			}, c.extra...)...))
			if code != 0 {
				t.Fatalf("exit %d, want 0 (opencode's own rc; the watchdog must not fire):\n%s", code, out)
			}
			if strings.Contains(out, "loop watchdog: stopped") {
				t.Errorf("the watchdog fired on something that is not a repeated failing call:\n%s", out)
			}
			if !strings.Contains(out, "loop watchdog: armed") {
				t.Errorf("the watchdog was never armed, so its silence proves nothing:\n%s", out)
			}
			requireVerbatim(t, out, fixture)
			// Ran under the watchdog (its own group), not the unwatched path.
			if p := readFakeOpencodeProcs(t, pidFile); p.pgid != p.pid {
				t.Errorf("opencode ran in group %d, not its own (%d): not under the watchdog", p.pgid, p.pid)
			}
		})
	}
}

// malformedLoopCall is the signature the watchdog must name for
// malformed-same-call-twice.txt: "Invalid Tool" and the detail line after it,
// which is where opencode says WHICH tool was called with WHAT.
const malformedLoopCall = `Invalid Tool: The arguments provided to the tool are invalid: Invalid input for tool glob: ` +
	`JSON parsing failed: Text: {"pattern": "README*"}}.`

// TestLoopWatchdogStopsTheSameMalformedCallTwice (gonk-mf3v): telling
// different malformed calls apart must not stop the watchdog seeing the SAME
// one repeated -- second identical failure, exit ExitLoopWatchdog, the call
// named with its detail, opencode killed.
func TestLoopWatchdogStopsTheSameMalformedCallTwice(t *testing.T) {
	s := newJobsSession(t)
	pidFile := filepath.Join(s.dir, "opencode.pids")
	killFakeOpencodeOnCleanup(t, pidFile)
	fixture := opencodeOutputFixture(t, "malformed-same-call-twice.txt")
	out, code := runJobs(t, s.env(t,
		"FAKE_OPENCODE_RUN_OUTPUT="+fixture,
		"FAKE_OPENCODE_RUN_HANG_SECS="+strconv.Itoa(loopHangSecs),
		"FAKE_OPENCODE_PID_FILE="+pidFile,
	))
	if code != gonkruntime.ExitLoopWatchdog {
		t.Fatalf("exit %d, want %d (runtime.ExitLoopWatchdog): the same malformed call twice did not stop the session:\n%s",
			code, gonkruntime.ExitLoopWatchdog, out)
	}
	want := fmt.Sprintf("gonk-agent-entrypoint: loop watchdog: stopped opencode -- the same tool call failed 2 times (threshold 2): %s\n", malformedLoopCall)
	if !strings.Contains(out, want) {
		t.Errorf("no watchdog line %q (the call must be named WITH its detail line):\n%s", want, out)
	}
	if !strings.Contains(out, "session end: opencode run exited rc=137\n") {
		t.Errorf("opencode was not ended by SIGKILL (want rc=137):\n%s", out)
	}
	requireVerbatim(t, out, fixture)
	procs := readFakeOpencodeProcs(t, pidFile)
	requireDead(t, procs.pid, "opencode")
	if procs.child == 0 {
		t.Fatalf("the fake opencode started no child; this test proves nothing about the group kill")
	}
	requireDead(t, procs.child, "opencode's child process")
}

// TestLoopWatchdogDoesNotCountAnInvalidToolLineWithoutItsDetail: a bare
// "Invalid Tool" line says nothing about what was called, so without the
// detail line after it two of them cannot be told apart and are NOT counted
// (the guard fails open; the component test is what notices if real opencode
// stops printing the detail). Derived from the real fixture by taking the
// detail lines out -- this is not output opencode 1.18.3 produces.
func TestLoopWatchdogDoesNotCountAnInvalidToolLineWithoutItsDetail(t *testing.T) {
	real, err := os.ReadFile(opencodeOutputFixture(t, "malformed-same-call-twice.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// What stands where each detail line was (%d: 1 for the first, 2 for the
	// second, so a failed Read put there is a DIFFERENT Read each time).
	for name, detail := range map[string]string{
		"an empty line follows":                   "\x1b[0m\n",
		"only the blank separator line follows":   "",
		"a different failed call follows at once": "\x1b[0m\xe2\x9c\x97 \x1b[0mRead file-%d.go failed\x1b[90m [limit=2000]\x1b[0m\n",
	} {
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			dropped := 0
			for _, l := range strings.SplitAfter(string(real), "\n") {
				switch {
				case strings.HasPrefix(l, "The arguments provided to the tool are invalid:"):
					dropped++
					if strings.Contains(detail, "%d") {
						fmt.Fprintf(&b, detail, dropped)
					} else {
						b.WriteString(detail)
					}
				case strings.HasPrefix(l, "Error message:"):
				default:
					b.WriteString(l)
				}
			}
			if dropped != 2 {
				t.Fatalf("took %d detail lines out of the real fixture, want 2", dropped)
			}
			s := newJobsSession(t)
			fixture := filepath.Join(s.dir, "no-detail.txt")
			if err := os.WriteFile(fixture, []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			out, code := runJobs(t, s.env(t, "FAKE_OPENCODE_RUN_OUTPUT="+fixture))
			if code != 0 || strings.Contains(out, "loop watchdog: stopped") {
				t.Errorf("exit %d: two bare Invalid Tool lines with no detail were counted as one call failing twice:\n%s", code, out)
			}
			if !strings.Contains(out, "loop watchdog: armed") {
				t.Errorf("the watchdog was never armed, so its silence proves nothing:\n%s", out)
			}
			requireVerbatim(t, out, fixture)
		})
	}
}

// TestLoopWatchdogCountsAFailedCallThatFollowsAnInvalidToolLine: a failed
// call printed straight after a bare "Invalid Tool" line is a call of its
// own, not that line's detail -- the same Read failing twice there is still
// the loop it is, and is named as the Read. Derived from the real fixture
// like the test above.
func TestLoopWatchdogCountsAFailedCallThatFollowsAnInvalidToolLine(t *testing.T) {
	real, err := os.ReadFile(opencodeOutputFixture(t, "malformed-same-call-twice.txt"))
	if err != nil {
		t.Fatal(err)
	}
	const failedRead = "\x1b[0m\xe2\x9c\x97 \x1b[0mRead internal/widget/adapter.go failed\x1b[90m [limit=2000]\x1b[0m\n"
	var b strings.Builder
	replaced := 0
	for _, l := range strings.SplitAfter(string(real), "\n") {
		switch {
		case strings.HasPrefix(l, "The arguments provided to the tool are invalid:"):
			replaced++
			b.WriteString(failedRead)
		case strings.HasPrefix(l, "Error message:"):
		default:
			b.WriteString(l)
		}
	}
	if replaced != 2 {
		t.Fatalf("replaced %d detail lines of the real fixture, want 2", replaced)
	}
	s := newJobsSession(t)
	fixture := filepath.Join(s.dir, "read-after-invalid-tool.txt")
	if err := os.WriteFile(fixture, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runJobs(t, s.env(t, "FAKE_OPENCODE_RUN_OUTPUT="+fixture))
	want := fmt.Sprintf("loop watchdog: stopped opencode -- the same tool call failed 2 times (threshold 2): %s\n", rom1LoopCall)
	if code != gonkruntime.ExitLoopWatchdog || !strings.Contains(out, want) {
		t.Errorf("exit %d, want %d and the line %q: the Read that failed twice was swallowed as an Invalid Tool detail:\n%s",
			code, gonkruntime.ExitLoopWatchdog, want, out)
	}
}

// TestLoopWatchdogPassesExitCodeAndFenceThrough: a normal run's rc comes out
// unchanged (dash has no pipefail; the FIFO design is what keeps it), and the
// GONK_BATCH fence reaches the log byte for byte.
func TestLoopWatchdogPassesExitCodeAndFenceThrough(t *testing.T) {
	for _, rc := range []int{0, 3} {
		t.Run("rc "+strconv.Itoa(rc), func(t *testing.T) {
			s := newJobsSession(t)
			fixture := opencodeOutputFixture(t, "fenced-answer.txt")
			out, code := runJobs(t, s.env(t,
				"FAKE_OPENCODE_RUN_OUTPUT="+fixture,
				"FAKE_OPENCODE_RUN_EXIT="+strconv.Itoa(rc),
			))
			if code != rc {
				t.Fatalf("exit %d, want opencode's rc %d passed through the watchdog:\n%s", code, rc, out)
			}
			if !strings.Contains(out, fmt.Sprintf("session end: opencode run exited rc=%d\n", rc)) {
				t.Errorf("session-end line does not carry opencode's rc %d:\n%s", rc, out)
			}
			requireVerbatim(t, out, fixture)
			if strings.Contains(out, "loop watchdog: stopped") {
				t.Errorf("the watchdog fired on a clean run:\n%s", out)
			}
		})
	}
}

// TestLoopWatchdogRejectsANonsenseThreshold: anything but a positive integer
// is refused LOUDLY and the default (2) is used -- never "0 = off" by accident.
func TestLoopWatchdogRejectsANonsenseThreshold(t *testing.T) {
	for _, v := range []string{"0", "two", "-1", "02"} {
		t.Run(v, func(t *testing.T) {
			s := newJobsSession(t)
			pidFile := filepath.Join(s.dir, "opencode.pids")
			killFakeOpencodeOnCleanup(t, pidFile)
			out, code := runJobs(t, s.env(t,
				"GONK_LOOP_WATCHDOG_THRESHOLD="+v,
				// No linger: the watchdog's verdict is recorded even when
				// opencode has already exited, and this test is about the
				// threshold, not the kill.
				"FAKE_OPENCODE_RUN_OUTPUT="+opencodeOutputFixture(t, "rom1-loop.txt"),
				"FAKE_OPENCODE_PID_FILE="+pidFile,
			))
			if !strings.Contains(out, "WARNING: GONK_LOOP_WATCHDOG_THRESHOLD="+v+" is not a positive integer; using 2") {
				t.Errorf("no warning for threshold %q:\n%s", v, out)
			}
			if code != gonkruntime.ExitLoopWatchdog || !strings.Contains(out, "failed 2 times (threshold 2)") {
				t.Errorf("exit %d, want %d at the default threshold 2:\n%s", code, gonkruntime.ExitLoopWatchdog, out)
			}
		})
	}
}

// TestLoopWatchdogLeavesGasCityAlone: the Gas City path runs opencode exactly
// as before -- no watchdog, no FIFO, opencode in the ENTRYPOINT's process
// group, and a loop that would stop a Job is passed straight to the hold.
func TestLoopWatchdogLeavesGasCityAlone(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "opencode.pids")
	killFakeOpencodeOnCleanup(t, pidFile)
	fixture := opencodeOutputFixture(t, "rom1-loop.txt")
	env := append(baseEnv(t, dir),
		"GONK_MODEL=test-model",
		"GONK_LITELLM_URL=http://litellm.invalid",
		"GC_WEBHOOK_ARG_LITELLM_KEY=fake-session-key",
		"GONK_OPENCODE_OVERLAY="+dir+"/opencode.json",
		`FAKE_CURL_BODY={"prompt":"do the assigned task"}`,
		"FAKE_OPENCODE_RUN_OUTPUT="+fixture,
		"FAKE_OPENCODE_PID_FILE="+pidFile,
	)
	const holding = "session end: runtime=gascity -- holding"
	out, outcome, _ := runEntrypointUntil(t, holding, env)
	if outcome != outcomeMarker {
		t.Fatalf("the Gas City path never reached its hold (outcome %d):\n%s", outcome, out)
	}
	if strings.Contains(out, "loop watchdog") {
		t.Errorf("the loop watchdog ran on the Gas City path, which must not change:\n%s", out)
	}
	if !strings.Contains(out, "session end: opencode run exited rc=0\n") {
		t.Errorf("opencode's rc on the Gas City path is not 0:\n%s", out)
	}
	requireVerbatim(t, out, fixture)
	if p := readFakeOpencodeProcs(t, pidFile); p.pgid == p.pid {
		t.Errorf("on the Gas City path opencode led its own process group (%d): it must run in the entrypoint's, as before", p.pgid)
	}
}
