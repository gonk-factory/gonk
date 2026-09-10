//go:build images

// Package images holds container-build smoke tests (build tag `images`) --
// house rule: "if an app runs in a container, test it in a container." These
// tests shell out to a real container runtime (this dev box's `podman`;
// docker/setup-buildx-action's genuine Docker daemon in CI --
// docs/environment.md, containerBin below) and need the images already
// built (`make images` locally; the `images` CI job builds them fresh).
//
// Run: go test ./test/images/ -tags images -run Agent -v
package images

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/test/harness"
)

// containerBin resolves the container runtime binary ONCE per process and
// reuses it for every test in this package: harness.DetectRuntime() shells
// out to probe for podman/docker, and running that probe once instead of
// once per test/subtest keeps the suite from re-forking it dozens of times.
//
// This is the "abstract the runtime" half of T-16 (gonk-ak0): this box's
// podman is what docs/environment.md calls "this box's docker" (a broken CNI
// bridge means every call here also needs --network=host, T-15's
// harness.Runtime.HostNetwork), but a GitHub Actions runner ships a genuine
// Docker daemon and no podman binary at all. harness.DetectRuntime() (T-15,
// test/component's TestMain) already resolves exactly this difference --
// reusing it here means test/images needs no runtime-specific branch of its
// own; `podman image inspect`/`docker image inspect`, `create`, `cp`, `run`
// and `inspect --format` are the same subcommands and flags on both.
var (
	containerBinOnce sync.Once
	containerBinPath string
	containerBinErr  error
)

func containerBin(t *testing.T) string {
	t.Helper()
	containerBinOnce.Do(func() {
		rt, err := harness.DetectRuntime()
		if err != nil {
			containerBinErr = err
			return
		}
		containerBinPath = rt.Bin
	})
	if containerBinErr != nil {
		t.Fatalf("no container runtime found (tried podman, docker): %v", containerBinErr)
	}
	return containerBinPath
}

// repoRoot walks up from this file's own directory (test/images/) to the
// module root -- stable regardless of the caller's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// versionPins reads images/versions.env the same way the Makefile does: a
// flat KEY=value file, `#`-comments allowed after a value. This is the SAME
// file `make images` builds from -- if this parse and the Makefile's
// `include` ever disagree, that is a bug in this file, not in versions.env.
func versionPins(t *testing.T, root string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "images", "versions.env"))
	if err != nil {
		t.Fatalf("read images/versions.env: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		val := kv[1]
		if i := strings.Index(val, "#"); i >= 0 {
			val = val[:i]
		}
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(val)
	}
	return out
}

// gonkTag reproduces the Makefile's `GONK_TAG ?= $(GONK_VERSION)-$(shell git
// rev-parse --short=12 HEAD)` so this test targets the exact image `make
// images` just built, without a second source of truth for the tag.
func gonkTag(t *testing.T, root, gonkVersion string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--short=12", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return gonkVersion + "-" + strings.TrimSpace(string(out))
}

func agentImage(t *testing.T) (image string, pins map[string]string) {
	t.Helper()
	root := repoRoot(t)
	pins = versionPins(t, root)
	registry := pins["REGISTRY"]
	if registry == "" {
		t.Fatal("REGISTRY missing from images/versions.env")
	}
	tag := gonkTag(t, root, pins["GONK_VERSION"])
	image = registry + "/gonk-agent:" + tag
	// Confirm the image actually exists locally before running anything
	// against it -- a clearer failure than the runtime's own `run` 125-ing
	// on every subtest individually. `image inspect` (not podman's own
	// `image exists` convenience command, which docker has no equivalent
	// of) exits nonzero on both runtimes when the image is absent.
	bin := containerBin(t)
	present := exec.Command(bin, "image", "inspect", image).Run() == nil
	harness.RequireInfra(t, "image "+image+" (run `make images` first)", present)
	return image, pins
}

// runIn execs the given binary as the container's ENTRYPOINT override and
// returns the CONTAINER's stdout (never the runtime's own stderr -- podman
// prints noisy CNI-validation warnings on this box that would otherwise
// corrupt an exact-match or JSON assertion on the container's real output)
// and the exit code. --rm: this is a smoke test, not a fixture;
// --network=host: the CNI bridge is broken on this box (docs/environment.md)
// and, per harness.Runtime's own doc comment, load-bearing on a genuine
// Docker daemon too for the container-backed harness suites.
func runIn(t *testing.T, image, entrypoint string, args ...string) (string, int) {
	t.Helper()
	bin := containerBin(t)
	full := append([]string{"run", "--rm", "--network=host", "--entrypoint", entrypoint, image}, args...)
	cmd := exec.Command(bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("%s run %v: %v\nstderr:\n%s", bin, full, err, stderr.String())
		}
	}
	if stdout.Len() == 0 && stderr.Len() > 0 {
		// Nothing came back on stdout (e.g. the process crashed before
		// printing anything) -- surface stderr so a failure is diagnosable.
		return stderr.String(), code
	}
	return stdout.String(), code
}

func TestAgentImagePinsMatchVersionsEnv(t *testing.T) {
	image, pins := agentImage(t)

	cases := []struct {
		name       string
		entrypoint string
		args       []string
		want       string // substring the output must contain
	}{
		{"opencode", "/usr/local/bin/opencode", []string{"--version"}, pins["OPENCODE_VERSION"]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.want == "" {
				t.Fatalf("expected version for %s is empty -- check images/versions.env", c.name)
			}
			out, code := runIn(t, image, c.entrypoint, c.args...)
			if code != 0 {
				t.Fatalf("%s --version exited %d:\n%s", c.name, code, out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("%s --version = %q, want it to contain pinned version %q (a floating agent is not an agent)", c.name, out, c.want)
			}
		})
	}
}

// gonk-0de. glab and bd are NOT in this image, and their absence is a property
// worth pinning rather than a detail that happens to be true today.
//
// They cost 19.5 MB and 52.8 MB of every build -- 72.3 MB per arch, a third of
// the image -- while nothing invoked either. But size is the lesser reason. The
// broker model strips this pod of forge credentials on purpose, so a forge CLI
// in it is a standing invitation to re-credential the pod "just for this one
// case" and quietly undo the whole zero-creds design (cf gonk-7oz). And spike S0
// established the pod has no working beads store at all, so a bd here can only
// mislead whoever finds it.
//
// If this test starts failing because someone added them back, the question to
// answer first is not "how do we shrink the image" but "why does the agent need
// a credentialed CLI" -- the answer is meant to be that it does not: writes
// happen as effects the BROKER applies.
func TestAgentImageShipsNoForgeOrBeadsCLI(t *testing.T) {
	image, _ := agentImage(t)
	for _, bin := range []string{"glab", "bd"} {
		t.Run(bin, func(t *testing.T) {
			out, code := runIn(t, image, "/bin/sh", "-c", "command -v "+bin+" || echo ABSENT")
			if !strings.Contains(out, "ABSENT") {
				t.Fatalf("%s is present in the agent image at %q -- the pod holds no forge "+
					"credentials and no beads store, so this binary can only mislead or tempt "+
					"(gonk-0de). exit=%d", bin, strings.TrimSpace(out), code)
			}
		})
	}
}

func TestAgentImageGitWorks(t *testing.T) {
	image, _ := agentImage(t)
	out, code := runIn(t, image, "/usr/bin/git", "--version")
	if code != 0 {
		t.Fatalf("git --version exited %d:\n%s", code, out)
	}
}

func TestAgentImageRunsAsNonRoot(t *testing.T) {
	image, _ := agentImage(t)
	out, code := runIn(t, image, "/usr/bin/id", "-u")
	if code != 0 {
		t.Fatalf("id -u exited %d:\n%s", code, out)
	}
	if got := strings.TrimSpace(out); got != "65532" {
		t.Fatalf("id -u = %q, want 65532 (not root)", got)
	}
}

// The agent pod must never be able to hang on something waiting for a keypress.
// Two env vars carry that, and both are load-bearing for a reason the test
// harness itself cannot reproduce:
//
//   - PAGER/GIT_PAGER: git pages only when stdout is a TTY. `podman run` here
//     has no TTY, so `git log` would pass this suite whether or not a pager is
//     configured. The AGENT runs under opencode's PTY, where git WILL page and
//     then block forever on a keypress until the reservation TTL reaps it --
//     a silent stall indistinguishable from a slow model.
//   - GIT_TERMINAL_PROMPT: the pod holds no forge credential by design, so a
//     git operation reaching for a remote must fail rather than sit on a
//     username prompt. Failing closed turns a hang into a classifiable error.
//
// Asserting the environment (rather than trying to provoke a pager) is
// deliberate: it is the property that actually has to hold, and it holds
// regardless of whether this suite can allocate a TTY.
func TestAgentImageCannotHangOnAPagerOrCredentialPrompt(t *testing.T) {
	image, _ := agentImage(t)
	want := map[string]string{
		"PAGER":               "cat",
		"GIT_PAGER":           "cat",
		"GIT_TERMINAL_PROMPT": "0",
	}
	for _, k := range []string{"PAGER", "GIT_PAGER", "GIT_TERMINAL_PROMPT"} {
		out, code := runIn(t, image, "/bin/sh", "-c", "printf %s \"$"+k+"\"")
		if code != 0 {
			t.Fatalf("reading $%s exited %d:\n%s", k, code, out)
		}
		if got := strings.TrimSpace(out); got != want[k] {
			t.Errorf("$%s = %q, want %q -- without it the agent can hang "+
				"silently under opencode's PTY until its reservation expires", k, got, want[k])
		}
	}
}

// gonk-ob5: opencode fails OPEN. Given no resolvable config it does not error
// -- it silently falls back to a built-in cloud provider, answers correctly and
// exits 0, so a model call happens off-meter and gonk cannot know. Worse, the
// built-ins are present even when our overlay DOES load: the overlay adds a
// provider, it never removed theirs.
//
// This is the A/B that proves the allowlist is what closes it, run against the
// real pinned opencode rather than asserted from its docs. The control half
// matters: without it, a future opencode that ignored `enabled_providers`
// entirely would still pass the guarded half and this test would be decoration.
func TestAgentImageOpencodeResolvesOnlyTheGonkProvider(t *testing.T) {
	image, _ := agentImage(t)

	const overlay = `{
  "$schema": "https://opencode.ai/config.json",
  %s
  "model": "gonk/qwen3-14b",
  "provider": {"gonk": {"npm": "@ai-sdk/openai-compatible", "name": "gonk (LiteLLM)",
    "options": {"baseURL": "http://127.0.0.1:1/v1", "apiKey": "unused"},
    "models": {"qwen3-14b": {"name": "qwen3-14b"}}}}
}`
	script := func(cfg string) string {
		return "set -e\n" +
			"printf '%s' '" + cfg + "' > /workspace/oc.json\n" +
			"export OPENCODE_CONFIG=/workspace/oc.json\n" +
			"export OPENCODE_DISABLE_MODELS_FETCH=1\n" +
			"opencode models 2>/dev/null || true\n"
	}

	// Control: no allowlist -> opencode's built-in providers are reachable.
	ctrl, _ := runIn(t, image, "/bin/sh", "-c", script(fmt.Sprintf(overlay, "")))
	var ctrlForeign []string
	for _, ln := range strings.Split(strings.TrimSpace(ctrl), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "gonk/") {
			ctrlForeign = append(ctrlForeign, ln)
		}
	}
	if len(ctrlForeign) == 0 {
		t.Skipf("control listed no non-gonk providers, so this opencode build "+
			"exposes none and the allowlist assertion below would be vacuous; "+
			"re-check gonk-ob5 against opencode %s. control output:\n%s",
			"(pinned)", ctrl)
	}

	// Guarded: with the allowlist, nothing but gonk/ may appear.
	got, _ := runIn(t, image, "/bin/sh", "-c",
		script(fmt.Sprintf(overlay, `"enabled_providers": ["gonk"],`)))
	var foreign []string
	for _, ln := range strings.Split(strings.TrimSpace(got), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "gonk/") {
			foreign = append(foreign, ln)
		}
	}
	if len(foreign) > 0 {
		t.Errorf("enabled_providers did not confine opencode to gonk/.\n"+
			"leaked: %v\nfull output:\n%s\n"+
			"(control saw %d non-gonk provider(s), so the allowlist is the "+
			"thing that failed, not the fixture)", foreign, got, len(ctrlForeign))
	}
}

// gonk-gate ships in the agent image for `check`/`trailers` (AD-3). What this
// asserts: the binary is present, executable, and honours its documented
// exit-code contract (main.go's own doc comment: 2 = misconfiguration, never
// retried).
func TestAgentImageGonkGateBinaryPresent(t *testing.T) {
	image, _ := agentImage(t)
	out, code := runIn(t, image, "/usr/local/bin/gonk-gate")
	if code != 2 {
		t.Fatalf("gonk-gate with no args: exit %d, want 2 (its own documented usage-error contract):\n%s", code, out)
	}
}

// TestAgentImageGonkGateVersionMatchesTag closes half the gap this task
// originally flagged and skipped ("gonk-gate has no --version flag ... yet"):
// `--version` landed (cmd/gonk-gate/main.go), stamped at build time via
// `-ldflags -X main.version=$(GONK_TAG)` (images/Dockerfile.agent's `gate`
// build stage), the same mechanism Task 6's controller smoke test proved.
func TestAgentImageGonkGateVersionMatchesTag(t *testing.T) {
	image, _ := agentImage(t)
	root := repoRoot(t)
	pins := versionPins(t, root)
	tag := gonkTag(t, root, pins["GONK_VERSION"])

	out, code := runIn(t, image, "/usr/local/bin/gonk-gate", "--version")
	if code != 0 {
		t.Fatalf("gonk-gate --version exited %d:\n%s", code, out)
	}
	if got := strings.TrimSpace(out); got != tag {
		t.Fatalf("gonk-gate --version = %q, want %q (GONK_TAG)", got, tag)
	}
}

// TestAgentImageGonkGateTrailersSplicesACommitMessage closes the other half
// of the gap this task originally flagged and skipped: `trailers` did not
// exist yet ("Task 8 adds trailers"). It now does
// (cmd/gonk-gate/trailers.go), and this runs the REAL binary THIS IMAGE
// SHIPS -- not a locally-built one -- against a commit message file on a
// bind mount, with GONK_METER_URL deliberately empty (proving the "never
// fail a commit" fallback to the shipped default, commit_trailers=on,
// documented in trailers.go and exercised end-to-end against a real `git
// commit` by cmd/gonk-gate's own TestHookAttachesTrailersToARealCommit in
// this same package).
func TestAgentImageGonkGateTrailersSplicesACommitMessage(t *testing.T) {
	image, _ := agentImage(t)

	msgDir := t.TempDir()
	// t.TempDir() defaults to 0700, owned by the host user (root on this
	// dev box) -- but the container reads and WRITES this bind mount as
	// uid 65532 (USER 65532:65532, images/Dockerfile.agent), which is
	// neither the owner nor in the owning group of anything created here.
	// Two separate permission bits are needed, and this test was missing
	// BOTH the first time it was actually run against a real image (T-16 --
	// this suite never ran anywhere before, so nobody noticed):
	//   - the DIRECTORY needs "other" execute to be traversable at all --
	//     without it, `gonk-gate trailers` can't even open the file
	//     ("permission denied" reading it).
	//   - the FILE itself needs "other" write, or the splice can read the
	//     original message fine but fails ("permission denied") writing
	//     the updated one back.
	// Either failure is swallowed by gonk-gate's own "never fail a commit"
	// contract (runTrailers logs and returns 0 regardless), so the test
	// read back the UNCHANGED message and reported "trailers missing" with
	// no hint that the real cause was a permission mismatch, not a code
	// bug. Mirrors copyPackToTemp's identical fix in packvalidate_test.go,
	// which got the directory half of this right the first time (and goes
	// further, at 0o777 on its copied files too, since gc also needs to
	// read them as uid 65532).
	if err := os.Chmod(msgDir, 0o777); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(msgDir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgPath, []byte("feat: something\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile's mode argument is masked by this process's umask at
	// creation time (0022 on this box, which strips exactly the "other
	// write" bit trailers needs to overwrite the file as uid 65532) -- a
	// SEPARATE os.Chmod bypasses the umask the same way `chmod` always has.
	// Confirmed by hand: without this, WriteFile(..., 0o666) here silently
	// produces 0644, and the container fails the same "permission denied"
	// write with no other symptom.
	if err := os.Chmod(msgPath, 0o666); err != nil {
		t.Fatal(err)
	}

	tags := atags.Tags{
		Project: "group/repo", Rig: "repo", BeadID: "gk-1a2b",
		SessionKey: "gonk-42-issue-3", Rung: "cheap", Attempt: 2,
		Trigger: atags.TriggerIssueTriage,
	}
	metadataJSON, err := json.Marshal(tags.Metadata())
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(containerBin(t), "run", "--rm", "--network=host",
		"-e", "GC_WEBHOOK_ARG_MODEL=some-model",
		"-e", "GC_WEBHOOK_ARG_METADATA_JSON="+string(metadataJSON),
		"-e", "GONK_METER_URL=",
		"-v", msgDir+":/tmp/msg:rw",
		"--entrypoint", "/usr/local/bin/gonk-gate",
		image, "trailers", "--commit-msg-file", "/tmp/msg/COMMIT_EDITMSG",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("gonk-gate trailers in the agent image: %v\nstderr:\n%s", err, stderr.String())
	}

	got, err := os.ReadFile(msgPath)
	if err != nil {
		t.Fatal(err)
	}
	// gonk-gate trailers never returns non-zero for its own internal
	// failures (runTrailers' documented contract: log and move on), so
	// cmd.Run() above succeeding is not evidence anything actually
	// happened -- include stderr here too, where the real reason (a
	// permission error, an unreachable meter, a bad metadata blob) is
	// logged, or a genuine regression here again reads as a bare
	// "missing" with no clue why.
	if !strings.Contains(string(got), "Gonk-Bead: gk-1a2b") {
		t.Fatalf("trailers missing from the commit message after running the SHIPPED "+
			"gonk-gate binary inside the agent image:\n%s\nstderr:\n%s", got, stderr.String())
	}
}

// *** THE ATTRIBUTION SEAM (OD-7), THE POINT OF THIS WHOLE IMAGE. ***
//
// Runs the REAL entrypoint script, inside the container, with a synthetic
// meter Decide answer, and asserts the rendered overlay/opencode.json's
// provider header carries all seven pkg/atags keys, round-tripped through
// atags.FromMetadata itself -- not a hand-rolled key list that could drift
// from the real contract.
func TestAgentImageAttributionOverlayCarriesAllSevenAtags(t *testing.T) {
	image, _ := agentImage(t)

	want := atags.Tags{
		Project: "group/repo", Rig: "repo", BeadID: "gk-1a2b",
		SessionKey: "sess-9", Rung: "qwen-local", Attempt: 3,
		Trigger: atags.TriggerIssueTriage,
	}
	metadataJSON, err := json.Marshal(want.Metadata())
	if err != nil {
		t.Fatalf("marshal atags metadata: %v", err)
	}

	// The render script backgrounds the entrypoint (its last step execs
	// opencode, which would otherwise replace the shell and prevent the
	// following `cat`), polls for the rendered file (bounded), then prints
	// it. This is a test-only harness detail -- it does not change what the
	// image ships or how it is invoked in production (Gas City execs the
	// entrypoint directly as PID 1).
	script := `
set -eu
/usr/local/bin/gonk-agent-entrypoint --version >/tmp/opencode.out 2>/tmp/entrypoint.log &
for i in $(seq 1 50); do
  [ -f /etc/gonk/overlay/opencode.json ] && break
  sleep 0.1
done
cat /etc/gonk/overlay/opencode.json
`
	cmd := exec.Command(containerBin(t), "run", "--rm", "--network=host",
		// GC_ALIAS: entrypoint.sh's own Step 0 refuses to start at all
		// without a non-empty session alias ("no alias: GC_ALIAS is unset
		// or empty -- refusing to start without a session identity") --
		// added after this test was written, and this suite never ran
		// anywhere to notice the gap (REAL FINDING, T-16: without this the
		// container exits before ever reaching the overlay render, and
		// stdout -- the file this test's json.Unmarshal below needs -- is
		// simply empty). Any non-empty value satisfies the check; only its
		// length is used (for the logged prefix), never its content.
		"-e", "GC_ALIAS=testaliastestaliastestalias",
		"-e", "GC_WEBHOOK_ARG_MODEL=qwen-local",
		"-e", "GC_WEBHOOK_ARG_METADATA_JSON="+string(metadataJSON),
		"-e", "GONK_LITELLM_URL=http://litellm.litellm.svc.cluster.local:4000",
		"-e", "GONK_LITELLM_KEY_FILE=/run/secrets/litellm-key",
		"-v", fakeKeyFile(t)+":/run/secrets/litellm-key:ro",
		"--entrypoint", "sh",
		image, "-c", script,
	)
	// stdout and stderr kept SEPARATE (not `2>&1`): podman prints noisy
	// CNI-validation warnings on this box. stdout is NOT exactly the
	// rendered file, though -- entrypoint.sh's own log() function ALSO
	// writes every status line to /proc/1/fd/1 "NOT JUST THIS PROCESS'S
	// STDERR" (its own comment, gonk-dot: a detached tmux session has no
	// other way to reach `kubectl logs`), and PID 1 in THIS container is
	// the `sh -c script` running `cat` -- so those lines land on this
	// script's own stdout ahead of the JSON regardless of the `>
	// /tmp/entrypoint.log` redirect on the backgrounded entrypoint
	// process. REAL FINDING (T-16, first real run of this suite): find the
	// JSON object rather than assume `cat`'s output is the whole stream,
	// the same way packvalidate_test.go's decodeFirstJSONObject already
	// has to for gc's combined stdout+stderr.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("render overlay in container: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}
	stdoutRaw := stdout.String()
	i := strings.IndexByte(stdoutRaw, '{')
	if i < 0 {
		t.Fatalf("no JSON object found in container stdout:\n%s", stdoutRaw)
	}

	var cfg struct {
		Provider map[string]struct {
			Options struct {
				Headers map[string]string `json:"headers"`
				APIKey  string            `json:"apiKey"`
				BaseURL string            `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(stdoutRaw[i:]), &cfg); err != nil {
		t.Fatalf("rendered opencode.json is not valid JSON: %v\n--- raw ---\n%s", err, stdoutRaw)
	}
	gonk, ok := cfg.Provider["gonk"]
	if !ok {
		t.Fatalf("rendered config has no provider.gonk:\n%s", stdoutRaw)
	}
	if gonk.Options.BaseURL != "http://litellm.litellm.svc.cluster.local:4000/v1" {
		t.Errorf("provider.gonk.options.baseURL = %q", gonk.Options.BaseURL)
	}
	if !strings.HasPrefix(gonk.Options.APIKey, "{file:") {
		t.Errorf("provider.gonk.options.apiKey = %q, want opencode's own {file:...} substitution "+
			"(the virtual key must be a FILE MOUNT, never an env value)", gonk.Options.APIKey)
	}
	header, ok := gonk.Options.Headers["x-litellm-spend-logs-metadata"]
	if !ok {
		t.Fatalf("provider.gonk.options.headers has no x-litellm-spend-logs-metadata key:\n%s", stdoutRaw)
	}

	// THE assertion: round-trip the header value through the REAL contract
	// (pkg/atags.FromMetadata), not a hand-copied list of key names, so this
	// test breaks the moment the two disagree.
	var raw map[string]string
	if err := json.Unmarshal([]byte(header), &raw); err != nil {
		t.Fatalf("x-litellm-spend-logs-metadata is not valid JSON: %v\nvalue: %s", err, header)
	}
	got, err := atags.FromMetadata(raw)
	if err != nil {
		t.Fatalf("x-litellm-spend-logs-metadata failed atags.FromMetadata: %v\nvalue: %s", err, header)
	}
	if got != want {
		t.Fatalf("attribution round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}

	// Belt and braces: the exact seven `gonk_*` key strings, not just a
	// struct that happens to compare equal.
	for _, key := range []string{
		atags.KeyProject, atags.KeyRig, atags.KeyBeadID, atags.KeySessionKey,
		atags.KeyRung, atags.KeyAttempt, atags.KeyTrigger,
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("x-litellm-spend-logs-metadata missing key %q", key)
		}
	}
	if len(raw) != 7 {
		t.Errorf("x-litellm-spend-logs-metadata has %d keys, want exactly 7: %v", len(raw), raw)
	}
}

func fakeKeyFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "litellm-key")
	if err := os.WriteFile(path, []byte("sk-fake-test-key-never-a-real-secret\n"), 0o600); err != nil {
		t.Fatalf("write fake key file: %v", err)
	}
	return path
}
