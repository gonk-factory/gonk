//go:build images

// Package images holds container-build smoke tests (build tag `images`) --
// house rule: "if an app runs in a container, test it in a container." These
// tests shell out to `podman` (this box's docker; docs/environment.md) and
// need `make images` to have built the image first.
//
// Run: go test ./test/images/ -tags images -run Agent -v
package images

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

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
	// against it -- a clearer failure than "podman run" 125-ing on every
	// subtest individually.
	if err := exec.Command("podman", "image", "exists", image).Run(); err != nil {
		t.Skipf("image %s not present locally -- run `make images` first (skip, not fail: this is a container smoke test, not a unit test)", image)
	}
	return image, pins
}

// runIn execs the given binary as the container's ENTRYPOINT override and
// returns the CONTAINER's stdout (never podman's own stderr -- podman prints
// noisy CNI-validation warnings on this box that would otherwise corrupt an
// exact-match or JSON assertion on the container's real output) and the exit
// code. --rm: this is a smoke test, not a fixture; --network=host: the CNI
// bridge is broken on this box (docs/environment.md).
func runIn(t *testing.T, image, entrypoint string, args ...string) (string, int) {
	t.Helper()
	full := append([]string{"run", "--rm", "--network=host", "--entrypoint", entrypoint, image}, args...)
	cmd := exec.Command("podman", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("podman run %v: %v\nstderr:\n%s", full, err, stderr.String())
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
		{"glab", "/usr/local/bin/glab", []string{"--version"}, pins["GLAB_VERSION"]},
		{"bd", "/usr/local/bin/bd", []string{"--version"}, pins["BD_VERSION"]},
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

// gonk-gate ships in the agent image for `check`/`trailers` (AD-3). It has no
// `--version` flag and no `trailers` subcommand as of this task (confirmed
// against cmd/gonk-gate/main.go, 2026-07-16) -- that is Task 8's (trailers)
// and a cross-task gap this task cannot close without touching a binary
// outside its own remit (images/Dockerfile.agent + images/agent/*). What IS
// true today, and what this asserts: the binary is present, executable, and
// honours its documented exit-code contract (main.go's own doc comment: 2 =
// misconfiguration, never retried).
func TestAgentImageGonkGateBinaryPresent(t *testing.T) {
	image, _ := agentImage(t)
	out, code := runIn(t, image, "/usr/local/bin/gonk-gate")
	if code != 2 {
		t.Fatalf("gonk-gate with no args: exit %d, want 2 (its own documented usage-error contract):\n%s", code, out)
	}

	t.Run("version_and_trailers_KNOWN_GAP", func(t *testing.T) {
		t.Skip("gonk-gate has no --version flag and no `trailers` subcommand yet " +
			"(Task 8 adds trailers; --version is wanted by both this task's own smoke " +
			"test wishlist and Task 6's controller smoke test -- flagged in PLAN.md, " +
			"not silently skipped)")
	})
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
	cmd := exec.Command("podman", "run", "--rm", "--network=host",
		"-e", "GC_WEBHOOK_ARG_MODEL=qwen-local",
		"-e", "GC_WEBHOOK_ARG_METADATA_JSON="+string(metadataJSON),
		"-e", "GONK_LITELLM_URL=http://litellm.litellm.svc.cluster.local:4000",
		"-e", "GONK_LITELLM_KEY_FILE=/run/secrets/litellm-key",
		"-v", fakeKeyFile(t)+":/run/secrets/litellm-key:ro",
		"--entrypoint", "sh",
		image, "-c", script,
	)
	// stdout and stderr kept SEPARATE (not `2>&1`): podman prints noisy
	// CNI-validation warnings on this box, and this test needs stdout to be
	// exactly the rendered file, nothing else, for json.Unmarshal below.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("render overlay in container: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}
	buf := stdout

	var cfg struct {
		Provider map[string]struct {
			Options struct {
				Headers map[string]string `json:"headers"`
				APIKey  string            `json:"apiKey"`
				BaseURL string            `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(buf.Bytes(), &cfg); err != nil {
		t.Fatalf("rendered opencode.json is not valid JSON: %v\n--- raw ---\n%s", err, buf.String())
	}
	gonk, ok := cfg.Provider["gonk"]
	if !ok {
		t.Fatalf("rendered config has no provider.gonk:\n%s", buf.String())
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
		t.Fatalf("provider.gonk.options.headers has no x-litellm-spend-logs-metadata key:\n%s", buf.String())
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
