// The entrypoint under the Kubernetes Jobs runtime (gonk-t55, ADR-008 D3/D10;
// plan docs/plans/2026-09-25-t55-session-runtime.md, part P4).
//
// Under GONK_SESSION_RUNTIME=jobs the prompt, the LiteLLM key and the
// attribution metadata are MOUNTED FILES in GONK_SESSION_DIR (one Secret per
// Job), never fetched. These tests run the real script against the fake PATH
// and assert on what it actually did: the overlay it rendered, the argv and
// env the fake opencode received, and every curl it made (bead exit
// criterion 3: no curl for prompt or key).
package entrypoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gonkruntime "github.com/gonk-factory/gonk/pkg/runtime"
)

const (
	jobsPrompt   = "Triage issue 42.\nBe brief.\n"
	jobsKey      = "sk-mounted-session-key-0000"
	jobsMetadata = `{"bead_id":"gonk-abc","project":"75","attempt":"1"}`

	// Distinct values planted on the OLD (Gas City) channels. A jobs-mode run
	// that honoured any of them would carry one of these into its output.
	trapKey      = "sk-TRAP-webhook-arg-key"
	trapMetadata = `{"trap":"webhook-arg-metadata"}`
	trapModel    = "trap-model"
	trapAlias    = "trapalias0000000000000000000000"
)

// jobsSession is one jobs-mode scenario's directory layout.
type jobsSession struct {
	dir        string // t.TempDir()
	sessionDir string // GONK_SESSION_DIR
	overlay    string
	curlArgv   string // FAKE_CURL_ARGV_LOG
	ocEnv      string // FAKE_OPENCODE_ENV_FILE base
	ocArgv     string // FAKE_OPENCODE_ARGV_FILE base
}

// newJobsSession writes the three session files (any of them may then be
// removed or truncated by the caller) and returns the layout.
func newJobsSession(t *testing.T) *jobsSession {
	t.Helper()
	dir := t.TempDir()
	s := &jobsSession{
		dir:        dir,
		sessionDir: filepath.Join(dir, "run-gonk-session"),
		overlay:    filepath.Join(dir, "overlay", "opencode.json"),
		curlArgv:   filepath.Join(dir, "curl_argv"),
		ocEnv:      filepath.Join(dir, "opencode_env"),
		ocArgv:     filepath.Join(dir, "opencode_argv"),
	}
	if err := os.MkdirAll(s.sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"prompt":        jobsPrompt,
		"litellm-key":   jobsKey,
		"metadata.json": jobsMetadata,
	} {
		if err := os.WriteFile(filepath.Join(s.sessionDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// trapRigURL is planted as GONK_RIG_URL in env: jobs mode reads the checkout
// URL only from the mounted rig-url file, so a fetch of this is a failure.
const trapRigURL = "https://trap-env-rig.invalid/rig/from-env.tar.gz"

// writeRigURL mounts the checkout URL the way the jobs runtime does: the
// session Secret's `rig-url` key (pkg/runtime/jobs KeyRigURL).
func (s *jobsSession) writeRigURL(t *testing.T, url string) {
	t.Helper()
	if s.sessionDir == "" {
		t.Fatal("no session dir")
	}
	if err := os.MkdirAll(s.sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.sessionDir, "rig-url"), []byte(url), 0o600); err != nil {
		t.Fatal(err)
	}
}

// env is the Job's env as D5 describes it, PLUS every Gas City channel the
// fetched-row path used, set to trap values: GC_ALIAS with GONK_PROMPT_URL
// (what a fallback fetch would need) and the GC_WEBHOOK_ARG_* the prompt row
// used to fill.
func (s *jobsSession) env(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{
		"PATH=" + fakePATH(t),
		"GONK_SESSION_RUNTIME=jobs",
		// The contract the jobs runtime always sets (pkg/runtime/jobs), from
		// the SAME Go constant: plan 3.3's handshake.
		gonkruntime.EnvSessionContract + "=" + strconv.Itoa(gonkruntime.SessionContract),
		"GONK_SESSION_DIR=" + s.sessionDir,
		"GONK_SESSION_NAME=gonk-triage-p75-i42-a1",
		"GONK_AGENT=triage",
		"GONK_ATTEMPT=1",
		"GONK_MODEL=qwen3-14b",
		"GONK_LITELLM_URL=http://litellm.invalid",
		"GONK_RUNTIME_DIR=" + filepath.Join(s.dir, "runtime"),
		"GONK_RIG_DIR=" + filepath.Join(s.dir, "workspace"),
		"GONK_OPENCODE_OVERLAY=" + s.overlay,
		"FAKE_CURL_ARGV_LOG=" + s.curlArgv,
		"FAKE_OPENCODE_ENV_FILE=" + s.ocEnv,
		"FAKE_OPENCODE_ARGV_FILE=" + s.ocArgv,
		// traps
		"GC_ALIAS=" + trapAlias,
		"GONK_PROMPT_URL=https://prompt.invalid",
		"GONK_RIG_BASE_URL=https://rig-base.invalid",
		"GC_WEBHOOK_ARG_LITELLM_KEY=" + trapKey,
		"GC_WEBHOOK_ARG_METADATA_JSON=" + trapMetadata,
		"GC_WEBHOOK_ARG_MODEL=" + trapModel,
	}, extra...)
}

// curlCalls returns every curl argv line the fake recorded (none if the file
// was never created).
func (s *jobsSession) curlCalls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(s.curlArgv)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// runJobs runs a jobs-mode scenario to its OWN exit. Under the Jobs runtime
// the script must never hold, so a hang-guard outcome is itself a failure.
func runJobs(t *testing.T, env []string) (out string, exitCode int) {
	t.Helper()
	out, outcome, err := runEntrypointUntil(t, "", env)
	if outcome == outcomeHung {
		t.Fatalf("hang guard (%s) fired: under GONK_SESSION_RUNTIME=jobs the entrypoint must EXIT, "+
			"never hold -- a held Job is reported DeadlineExceeded:\n%s", hangGuard, out)
	}
	return out, exitCodeOf(t, err, out)
}

func exitCodeOf(t *testing.T, err error, out string) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("entrypoint did not run to an exit status: %v\n%s", err, out)
	}
	return exitErr.ExitCode()
}

// TestJobsModeReadsTheMountedSessionFiles is the P4 happy path: the three
// mounted files reach opencode exactly as the fetched prompt row's fields did
// -- the prompt as `opencode run`'s argument, the key through the {file:...}
// indirection, the metadata verbatim as the x-litellm-spend-logs-metadata
// header -- and nothing is fetched except the checkout.
func TestJobsModeReadsTheMountedSessionFiles(t *testing.T) {
	s := newJobsSession(t)
	const rigURL = "https://intake.invalid/rig/granted-checkout.tar.gz"
	// The checkout URL is MOUNTED as rig-url (plan 3.3 handshake item 4); a
	// GONK_RIG_URL planted in env is a trap jobs mode must ignore.
	s.writeRigURL(t, rigURL)
	out, code := runJobs(t, s.env(t, "GONK_RIG_URL="+trapRigURL))
	if code != 0 {
		t.Fatalf("exit %d, want 0 (fake opencode run exits 0):\n%s", code, out)
	}

	// EXIT CRITERION 3: the only curl is the checkout. No prompt fetch, no key
	// fetch, and the planted GC_ALIAS composed no URL.
	calls := s.curlCalls(t)
	if len(calls) != 1 || !strings.Contains(calls[0], rigURL) {
		t.Errorf("curl calls = %q, want exactly one: the checkout fetch of %s", calls, rigURL)
	}
	for _, c := range calls {
		for _, bad := range []string{"/v1/prompt", "prompt.invalid", trapAlias, "rig-base.invalid", "key", trapRigURL} {
			if strings.Contains(c, bad) {
				t.Errorf("curl call %q contains %q: jobs mode fetched something other than the checkout", c, bad)
			}
		}
	}

	// The overlay: model from the Job env, key by file indirection pointing at
	// the MOUNT, metadata header verbatim from metadata.json.
	b, err := os.ReadFile(s.overlay)
	if err != nil {
		t.Fatalf("overlay not rendered: %v\n%s", err, out)
	}
	var cfg struct {
		Model    string `json:"model"`
		Provider struct {
			Gonk struct {
				Options struct {
					BaseURL string            `json:"baseURL"`
					APIKey  string            `json:"apiKey"`
					Headers map[string]string `json:"headers"`
				} `json:"options"`
			} `json:"gonk"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("overlay is not JSON: %v\n%s", err, b)
	}
	if cfg.Model != "gonk/qwen3-14b" {
		t.Errorf("overlay model = %q, want gonk/qwen3-14b (GONK_MODEL, not the trap %q)", cfg.Model, trapModel)
	}
	wantKey := "{file:" + filepath.Join(s.sessionDir, "litellm-key") + "}"
	if got := cfg.Provider.Gonk.Options.APIKey; got != wantKey {
		t.Errorf("overlay apiKey = %q, want %q (the mounted key, via opencode's file indirection)", got, wantKey)
	}
	if got := cfg.Provider.Gonk.Options.Headers["x-litellm-spend-logs-metadata"]; got != jobsMetadata {
		t.Errorf("attribution header = %q, want metadata.json verbatim %q", got, jobsMetadata)
	}
	if bytes.Contains(b, []byte(jobsKey)) || bytes.Contains(b, []byte(trapKey)) {
		t.Errorf("the rendered overlay contains key BYTES; it must only reference the file:\n%s", b)
	}

	// The prompt opencode ran on: the mounted file, trailing newlines
	// stripped exactly as $(jq -r .prompt) stripped them on the fetched row.
	argv, err := os.ReadFile(s.ocArgv + ".run")
	if err != nil {
		t.Fatalf("opencode run was never invoked: %v\n%s", err, out)
	}
	wantArgv := "run\x00--\x00" + strings.TrimRight(jobsPrompt, "\n") + "\x00"
	if string(argv) != wantArgv {
		t.Errorf("opencode run argv = %q, want %q", argv, wantArgv)
	}

	// No key value in opencode's environment -- neither the mounted one nor
	// the planted webhook arg.
	envDump, err := os.ReadFile(s.ocEnv + ".run")
	if err != nil {
		t.Fatalf("opencode run env not dumped: %v", err)
	}
	for _, secret := range []string{jobsKey, trapKey} {
		if bytes.Contains(envDump, []byte(secret)) {
			t.Errorf("a LiteLLM key value reached opencode's ENVIRONMENT (%q); it must only be a file", secret)
		}
	}
	// Nor the rig URL: it is a capability, read into an UNEXPORTED shell
	// variable, and the planted env one was unset.
	for _, u := range []string{rigURL, trapRigURL} {
		if bytes.Contains(envDump, []byte(u)) {
			t.Errorf("a rig URL reached opencode's ENVIRONMENT (%q)", u)
		}
		if strings.Contains(out, u) {
			t.Errorf("a rig URL was LOGGED (%q); it is a re-fetchable capability:\n%s", u, out)
		}
	}

	for _, want := range []string{
		"session start: runtime=jobs session=gonk-triage-p75-i42-a1 agent=triage attempt=1",
		"prompt read from the session mount (",
		"source=session-env",
		"session end: runtime=jobs -- exiting 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "holding so tmux stays alive") {
		t.Errorf("jobs mode took the Gas City hold path:\n%s", out)
	}
}

// TestJobsModeRefusesIncompleteSessionFiles: a missing or empty session file
// is fatal AT ONCE with the distinct exit code 6 -- no wait, no fallback
// fetch, no checkout fetch spent first, and opencode never started.
func TestJobsModeRefusesIncompleteSessionFiles(t *testing.T) {
	const wantExit = 6
	cases := map[string]func(t *testing.T, s *jobsSession){}
	for _, f := range []string{"prompt", "litellm-key", "metadata.json"} {
		cases[f+" missing"] = func(t *testing.T, s *jobsSession) {
			if err := os.Remove(filepath.Join(s.sessionDir, f)); err != nil {
				t.Fatal(err)
			}
		}
		cases[f+" empty"] = func(t *testing.T, s *jobsSession) {
			if err := os.WriteFile(filepath.Join(s.sessionDir, f), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range []string{"prompt", "metadata.json"} {
		cases[f+" newlines only"] = func(t *testing.T, s *jobsSession) {
			if err := os.WriteFile(filepath.Join(s.sessionDir, f), []byte("\n\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases["session dir missing"] = func(t *testing.T, s *jobsSession) {
		if err := os.RemoveAll(s.sessionDir); err != nil {
			t.Fatal(err)
		}
	}

	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			s := newJobsSession(t)
			// A granted checkout is mounted first, so "fetched nothing"
			// below means the refusal came before the checkout fetch.
			s.writeRigURL(t, "https://intake.invalid/rig/x.tar.gz")
			breakIt(t, s)
			out, code := runJobs(t, s.env(t))
			if code != wantExit {
				t.Fatalf("exit %d, want %d (incomplete session files):\n%s", code, wantExit, out)
			}
			if !strings.Contains(out, "session end: refused (session files incomplete") {
				t.Errorf("no refusal line naming the incomplete session files:\n%s", out)
			}
			if calls := s.curlCalls(t); len(calls) != 0 {
				t.Errorf("curl was called %q; a refused jobs session must fetch nothing -- not a "+
					"prompt, not a key, not even the checkout", calls)
			}
			for _, sub := range []string{"models", "run"} {
				if _, err := os.Stat(s.ocArgv + "." + sub); err == nil {
					t.Errorf("opencode %s was invoked despite the refusal", sub)
				}
			}
		})
	}
}

// TestJobsModeExitsWithOpencodesExitCode: the Job's terminal status is the
// container's exit code, so the script must pass opencode's rc through and
// never hold. (The outcome is still judged on the batch, not the rc -- opencode
// exits 0 after a rejected tool call -- but a non-zero rc must not be eaten.)
func TestJobsModeExitsWithOpencodesExitCode(t *testing.T) {
	s := newJobsSession(t)
	out, code := runJobs(t, s.env(t, "FAKE_OPENCODE_RUN_EXIT=42"))
	if code != 42 {
		t.Fatalf("exit %d, want opencode's rc 42:\n%s", code, out)
	}
	if !strings.Contains(out, "session end: opencode run exited rc=42") {
		t.Errorf("no session-end line with opencode's rc:\n%s", out)
	}
}

// TestUnknownSessionRuntimeIsRefused: a typo in GONK_SESSION_RUNTIME must not
// silently select the Gas City path (whose hold makes a clean Job look like a
// deadline overrun).
func TestUnknownSessionRuntimeIsRefused(t *testing.T) {
	s := newJobsSession(t)
	env := s.env(t)
	for i, kv := range env {
		if kv == "GONK_SESSION_RUNTIME=jobs" {
			env[i] = "GONK_SESSION_RUNTIME=Jobs"
		}
	}
	out, code := runJobs(t, env)
	if code != 7 {
		t.Fatalf("exit %d, want 7 (unknown session runtime):\n%s", code, out)
	}
	if !strings.Contains(out, "session end: refused (unknown session runtime)") {
		t.Errorf("no refusal line:\n%s", out)
	}
}

// pid1Prefix returns an `unshare` prefix that runs the script as PID 1 of a
// fresh PID namespace with its own /proc -- so /proc/1/fd/1 is the script's
// OWN stdout, exactly as in a Job's container. As root a plain PID namespace
// is used; otherwise an unprivileged user namespace. Each candidate is probed
// for BOTH "$$ is 1" and a working jq (a snap-packaged jq refuses to run in a
// user namespace). Skips -- loudly, as not-passed -- when neither works, e.g.
// a CI container under a default seccomp profile.
func pid1Prefix(t *testing.T) []string {
	t.Helper()
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("SKIPPED, NOT PASSED: no unshare binary (%v), so the script cannot be run as PID 1 here", err)
	}
	ns := []string{"--pid", "--fork", "--mount-proc", "--kill-child"}
	var candidates [][]string
	if os.Geteuid() == 0 {
		candidates = append(candidates, append([]string{unshare}, ns...))
	}
	candidates = append(candidates, append([]string{unshare, "--user", "--map-root-user"}, ns...))
	var tried []string
	for _, prefix := range candidates {
		args := append(append([]string{}, prefix[1:]...), "sh", "-c", `[ "$$" = 1 ] && jq -n 1 >/dev/null`)
		probe := exec.Command(unshare, args...)
		probe.Env = []string{"PATH=" + os.Getenv("PATH")}
		out, err := probe.CombinedOutput()
		if err == nil {
			return prefix
		}
		tried = append(tried, strings.Join(prefix, " ")+": "+err.Error()+": "+strings.TrimSpace(string(out)))
	}
	t.Skipf("SKIPPED, NOT PASSED: cannot run the script as PID 1 with a working jq here, so the "+
		"PID-1 log path is untested on this runner:\n  %s", strings.Join(tried, "\n  "))
	return nil
}

// TestLogWritesEachLineOnceAsPID1 is the gate run's duplicated-log finding
// (gonk-t55 notes, (a)): log() tees to /proc/1/fd/1 for tmux's sake, and when
// the script IS PID 1 that is its own stdout, so every line landed in the pod
// log twice. Run for real as PID 1, every log line must appear exactly once.
func TestLogWritesEachLineOnceAsPID1(t *testing.T) {
	prefix := pid1Prefix(t)
	s := newJobsSession(t)
	out, outcome, err := runEntrypointUntilPrefixed(t, prefix, "", s.env(t))
	if outcome == outcomeHung {
		t.Fatalf("hang guard (%s) fired:\n%s", hangGuard, out)
	}
	if code := exitCodeOf(t, err, out); code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	counts := map[string]int{}
	var n int
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "gonk-agent-entrypoint: ") {
			counts[l]++
			n++
		}
	}
	if n == 0 {
		t.Fatalf("no log lines at all:\n%s", out)
	}
	for l, c := range counts {
		if c != 1 {
			t.Errorf("logged %d times, want once: %q", c, l)
		}
	}
	if counts["gonk-agent-entrypoint: session start: runtime=jobs session=gonk-triage-p75-i42-a1 agent=triage attempt=1"] != 1 {
		t.Errorf("the session-start line is not in the output exactly once:\n%s", out)
	}
}
