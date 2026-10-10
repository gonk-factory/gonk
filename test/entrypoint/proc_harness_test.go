// Process lifetime for every entrypoint.sh this package starts (gonk-gp44).
//
// THE BUG THIS EXISTS FOR. On 2026-09-24 a `sh -eu .../entrypoint.sh` started
// by TestPromptFetchRetriesTransportFailureThenSucceeds on 09-21 was found
// still alive, reparented to init, looping the Gas City hold (`while :; do
// sleep 3600; done`) three days later, pinning its worktree. The harness DID
// kill the script's process group -- but only from inside the test function,
// so a test binary that never got there (killed by `go test -timeout`, by a
// `make hunt` round, by a runner, by ^C -- which never reaches the script at
// all, because Setpgid takes it out of the terminal's foreground group) left
// the script running forever.
//
// So every start goes through startEntrypointProc, which:
//   - puts the script in its OWN process group (one signal reaches the shell
//     and every child it forked: curl, opencode, jq, the hold's sleep);
//   - sets Pdeathsig=SIGKILL on it (Linux; procattr_linux_test.go), so the
//     KERNEL kills the shell when the test binary dies for any reason at all,
//     including SIGKILL, where no Go code runs;
//
// and the testing.T wrapper, startEntrypoint, registers a t.Cleanup that kills
// the group, waits, and FAILS THE TEST if any member of the group is still
// alive afterwards (requireNoSurvivor). TestAKilledTestBinaryTakesItsEntrypointWithIt
// proves the Pdeathsig half by SIGKILLing a helper test binary mid-hold.
package entrypoint

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// survivorGuard bounds requireNoSurvivor's poll. A SAFETY NET, not a budget
// (docs/testing-time.md rule 5): after SIGKILL the group is gone as soon as
// the kernel has delivered it, which is effectively immediate; this only stops
// a genuinely surviving process from wedging the suite, and its failure text
// says a process SURVIVED, never that anything was slow.
const survivorGuard = 60 * time.Second

// markerWriter is the child's combined stdout+stderr: a buffer that closes
// seen the first time marker appears in everything written so far.
type markerWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	seen   chan struct{}
	once   sync.Once
}

func (w *markerWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if w.marker != "" && bytes.Contains(w.buf.Bytes(), []byte(w.marker)) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *markerWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// entrypointProc is one running entrypoint.sh (possibly under a prefix such
// as `unshare ...`), in its own process group.
type entrypointProc struct {
	cmd  *exec.Cmd
	out  *markerWriter
	pgid int
	// exited is closed once cmd.Wait has returned; waitErr is its result.
	exited  chan struct{}
	waitErr error
}

// killGroup SIGKILLs every process in the script's group. Safe to call more
// than once and after the group is gone.
func (p *entrypointProc) killGroup() {
	_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
}

// startEntrypointProc starts argv with env as the child's ENTIRE environment,
// its combined output captured into a markerWriter for marker. It has no
// testing.T so the helper process (TestMain) can use it too; callers in tests
// use startEntrypoint, which adds the cleanup and the survivor check.
func startEntrypointProc(argv []string, env []string, marker string) (*entrypointProc, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = entrypointSysProcAttr()
	// A safety net for cmd.Wait's output copy: once the shell has exited, do
	// not wait forever on a pipe some escaped grandchild still holds open.
	cmd.WaitDelay = survivorGuard
	w := &markerWriter{marker: marker, seen: make(chan struct{})}
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &entrypointProc{cmd: cmd, out: w, pgid: cmd.Process.Pid, exited: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// entrypointArgv is `sh -eu <entrypoint.sh> args...` behind an optional
// prefix (e.g. an `unshare` invocation). `sh -eu` matches the script's own
// `set -eu`, so a failure mode here is the one production hits.
func entrypointArgv(t *testing.T, prefix []string, args ...string) []string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("sh not found on the test runner's own PATH: %v", err)
	}
	argv := append([]string{}, prefix...)
	argv = append(argv, sh, "-eu", entrypointPath(t))
	return append(argv, args...)
}

// startEntrypoint starts entrypoint.sh for a test and guarantees, in
// t.Cleanup, that nothing it started outlives the test: the group is killed,
// the shell reaped, and any member still alive afterwards fails the test.
func startEntrypoint(t *testing.T, prefix []string, marker string, env []string, args ...string) *entrypointProc {
	t.Helper()
	p, err := startEntrypointProc(entrypointArgv(t, prefix, args...), env, marker)
	if err != nil {
		t.Fatalf("start entrypoint: %v", err)
	}
	t.Cleanup(func() {
		p.killGroup()
		select {
		case <-p.exited:
		case <-time.After(survivorGuard):
			t.Errorf("SAFETY NET (%s): the entrypoint shell (pid %d) was SIGKILLed and still was not "+
				"reaped -- a process survived, this is not slowness", survivorGuard, p.pgid)
		}
		requireNoSurvivor(t, p.pgid)
	})
	return p
}

// requireNoSurvivor fails t unless no live (non-zombie) process remains in
// process group pgid. A zombie is dead -- it only awaits its parent's wait --
// so it does not count; everything else does, and is named in the failure.
func requireNoSurvivor(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(survivorGuard)
	for {
		alive := liveGroupMembers(pgid)
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL) // do not leave them behind either way
			t.Errorf("process group %d still has live members %s after SIGKILL -- the entrypoint "+
				"harness leaked processes (gonk-gp44)", pgid, strings.Join(alive, "; "))
			return
		}
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // poll interval of a wait-until with a safety-net deadline (docs/testing-time.md rule 6)
	}
}

// ---- The helper process (TestAKilledTestBinaryTakesItsEntrypointWithIt) ----

// helperEnvMode selects helper mode in TestMain; helperEnvArgv is the argv
// (a JSON array) the helper starts, helperEnvMarker the line it waits for.
// None of them reaches the entrypoint.
const (
	helperEnvMode   = "GONK_ENTRYPOINT_TEST_HELPER"
	helperEnvArgv   = "GONK_ENTRYPOINT_TEST_HELPER_ARGV"
	helperEnvMarker = "GONK_ENTRYPOINT_TEST_HELPER_MARKER"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnvMode) == "hold" {
		os.Exit(runHoldHelper())
	}
	os.Exit(m.Run())
}

// runHoldHelper is a stand-in for "a test binary that is running an
// entrypoint when it is killed". It starts the entrypoint exactly as the
// tests do (startEntrypointProc), waits for the marker, reports the
// entrypoint's pgid on stdout as "PGID=<n>", and then blocks forever -- it
// never kills anything itself. Its parent test then SIGKILLs it.
func runHoldHelper() int {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, helperEnvMode+"=") || strings.HasPrefix(kv, helperEnvArgv+"=") ||
			strings.HasPrefix(kv, helperEnvMarker+"=") {
			continue
		}
		env = append(env, kv)
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv(helperEnvArgv)), &argv); err != nil || len(argv) == 0 {
		fmt.Printf("START-FAILED bad %s: %v\n", helperEnvArgv, err)
		return 1
	}
	p, err := startEntrypointProc(argv, env, os.Getenv(helperEnvMarker))
	if err != nil {
		fmt.Printf("START-FAILED %v\n", err)
		return 1
	}
	select {
	case <-p.out.seen:
		fmt.Printf("PGID=%d\n", p.pgid)
	case <-p.exited:
		fmt.Printf("EXITED %v\n%s\n", p.waitErr, p.out.String())
		return 1
	}
	select {} // hold until killed
}

// TestAKilledTestBinaryTakesItsEntrypointWithIt is gonk-gp44's regression
// test, for the path that actually leaked: the test binary dying while an
// entrypoint is in the Gas City hold, with no Go code left to run a cleanup.
//
// A helper copy of this test binary starts the hold scenario and blocks; this
// test SIGKILLs the helper and then requires the entrypoint SHELL -- the
// process that loops forever -- to be gone. Without Pdeathsig it survives,
// reparented to init, exactly as found on 09-24.
//
// WHAT PDEATHSIG CANNOT DO, stated rather than hidden: it is not inherited, so
// the shell's CURRENT `sleep 3600` is orphaned rather than killed. That sleep
// ends on its own within the hour and nothing restarts it (the loop was the
// shell). The test's own cleanup kills the rest of the group and asserts it
// empty, so this test leaves nothing behind either.
func TestAKilledTestBinaryTakesItsEntrypointWithIt(t *testing.T) {
	dir := t.TempDir()
	env := append(baseEnv(t, dir),
		"GONK_MODEL=test-model",
		"GONK_LITELLM_URL=http://litellm.invalid",
		"GC_WEBHOOK_ARG_LITELLM_KEY=fake-session-key",
		"GONK_OPENCODE_OVERLAY="+dir+"/opencode.json",
		`FAKE_CURL_BODY={"prompt":"do the assigned task"}`,
	)
	const holding = "session end: runtime=gascity -- holding"
	argvJSON, err := json.Marshal(entrypointArgv(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(env,
		helperEnvMode+"=hold",
		helperEnvArgv+"="+string(argvJSON),
		helperEnvMarker+"="+holding,
	)
	// The helper itself must not outlive THIS binary either.
	helper.SysProcAttr = entrypointSysProcAttr()
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var helperErr bytes.Buffer
	helper.Stderr = &helperErr
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	helperDone := make(chan struct{})
	go func() { _ = helper.Wait(); close(helperDone) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-helper.Process.Pid, syscall.SIGKILL)
		<-helperDone
	})

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	var pgid int
	guard := time.After(hangGuard)
	for pgid == 0 {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("helper exited without reporting the entrypoint's pgid; stderr:\n%s", helperErr.String())
			}
			if v, found := strings.CutPrefix(l, "PGID="); found {
				if pgid, err = strconv.Atoi(v); err != nil {
					t.Fatalf("bad PGID line %q", l)
				}
			} else if strings.HasPrefix(l, "EXITED") || strings.HasPrefix(l, "START-FAILED") {
				t.Fatalf("helper's entrypoint never reached the hold: %s", l)
			}
		case <-guard:
			t.Fatalf("hang guard (%s): the helper never reported its entrypoint reaching the hold", hangGuard)
		}
	}
	// Whatever happens below, nothing in that group outlives this test.
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		requireNoSurvivor(t, pgid)
	})

	if !processAlive(pgid) {
		t.Fatalf("the entrypoint shell (pid %d) is not alive BEFORE the helper was killed -- "+
			"this test would pass vacuously", pgid)
	}

	// THE EVENT: the test binary dies, and no Go code in it runs again.
	if err := syscall.Kill(helper.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	<-helperDone

	deadline := time.Now().Add(survivorGuard)
	for processAlive(pgid) {
		if time.Now().After(deadline) {
			t.Fatalf("the entrypoint shell (pid %d) SURVIVED its test binary being SIGKILLed: it is "+
				"now an orphan looping the Gas City hold forever -- the gonk-gp44 leak", pgid)
		}
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // poll interval of a wait-until with a safety-net deadline (docs/testing-time.md rule 6)
	}
}
