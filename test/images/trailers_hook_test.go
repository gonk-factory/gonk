//go:build images

// This file shares the `images` build tag with the rest of this package (not
// because it needs a container -- it does not -- but because, like them, it
// is a slow, real-process proof kept out of the fast day-to-day gate: a
// `go build` plus a real `git init`/`commit`/`--amend`).
//
// cmd/gonk-gate/trailers_test.go's TestTrailerBlockIsWellFormed proves the
// STRING renderTrailers produces looks like a git trailer. It does not prove
// git agrees: a unit test on a string formatter cannot. This file installs
// the REAL images/agent/prepare-commit-msg hook into a REAL git repo, drives
// a REAL `git commit`, and reads the result back with git's OWN trailer
// reader (`git log --format='%(trailers:...)'`) -- the same mechanism
// GitLab and every other consumer uses. If git's reader does not see these
// as trailers, nothing downstream will either.
package images

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

// buildGonkGate compiles cmd/gonk-gate fresh (offline, -mod=vendor, matching
// how every image in this repo builds it -- images/Dockerfile.agent's own
// `gate` stage) into a temp dir, and returns that dir (for PATH).
func buildGonkGate(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "gonk-gate")
	cmd := exec.Command("go", "build", "-mod=vendor", "-o", bin, "./cmd/gonk-gate")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=vendor", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/gonk-gate: %v\n%s", err, out)
	}
	return dir
}

func TestHookAttachesTrailersToARealCommit(t *testing.T) {
	root := repoRoot(t)
	gateDir := buildGonkGate(t)

	repo := t.TempDir()
	run := func(env []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = repo
		if env != nil {
			cmd.Env = env
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}

	run(nil, "git", "init", "-q")
	run(nil, "git", "config", "user.email", "gonk@example.com")
	run(nil, "git", "config", "user.name", "gonk")

	if err := os.MkdirAll(filepath.Join(repo, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	hookSrc, err := os.ReadFile(filepath.Join(root, "images", "agent", "prepare-commit-msg"))
	if err != nil {
		t.Fatalf("read images/agent/prepare-commit-msg: %v", err)
	}
	hookPath := filepath.Join(repo, ".git", "hooks", "prepare-commit-msg")
	if err := os.WriteFile(hookPath, hookSrc, 0o755); err != nil {
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

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(nil, "git", "add", "README.md")

	// Same env every commit in this test uses: the hook's own binary on
	// PATH, the two GC_WEBHOOK_ARG_* values the agent pod's session already
	// carries for OD-7's overlay render, and GONK_METER_URL deliberately
	// EMPTY -- proving the "never fail a commit" fallback (commit_trailers
	// defaults on even when meter cannot be asked).
	commitEnv := append(os.Environ(),
		"PATH="+gateDir+":"+os.Getenv("PATH"),
		"GC_WEBHOOK_ARG_MODEL=some-model",
		"GC_WEBHOOK_ARG_METADATA_JSON="+string(metadataJSON),
		"GONK_METER_URL=",
		"GONK_METER_TOKEN_FILE=",
	)

	run(commitEnv, "git", "commit", "-m", "feat: something", "-q")

	// *** THE POINT OF THIS TEST: git's OWN reader, not ours. ***
	get := func(key string) string {
		return strings.TrimSpace(run(nil, "git", "log", "-1", "--format=%(trailers:key="+key+",valueonly)"))
	}
	if got := get("Generated-By"); got == "" {
		t.Fatal("Generated-By trailer missing (git's own reader saw nothing)")
	}
	if got := get("Gonk-Bead"); got != "gk-1a2b" {
		t.Fatalf("Gonk-Bead = %q, want gk-1a2b", got)
	}
	if got := get("Gonk-Session"); got != "gonk-42-issue-3" {
		t.Fatalf("Gonk-Session = %q, want gonk-42-issue-3", got)
	}
	if got := get("Gonk-Rung"); got != "cheap" {
		t.Fatalf("Gonk-Rung = %q, want cheap", got)
	}
	if got := get("Gonk-Attempt"); got != "2" {
		t.Fatalf("Gonk-Attempt = %q, want 2", got)
	}
	// include_usage defaults off (the shipped default, and the only answer
	// available when meter cannot be asked) -- no usage line at all, and
	// certainly no cost/token number.
	rawMsg := run(nil, "git", "log", "-1", "--format=%B")
	if strings.Contains(rawMsg, "Gonk-Usage") || strings.Contains(rawMsg, "Gonk-Cost-USD") || strings.Contains(rawMsg, "Gonk-Tokens") {
		t.Fatalf("include_usage defaults off -- no usage trailer should appear at all:\n%s", rawMsg)
	}
	if n := strings.Count(rawMsg, "Generated-By:"); n != 1 {
		t.Fatalf("want exactly one Generated-By trailer after the first commit, got %d:\n%s", n, rawMsg)
	}

	// *** IDEMPOTENCY: git re-invokes prepare-commit-msg on `commit --amend`.
	// A second run over the SAME inputs must not duplicate the block. ***
	run(commitEnv, "git", "commit", "--amend", "--no-edit", "-q")
	amendedMsg := run(nil, "git", "log", "-1", "--format=%B")
	if n := strings.Count(amendedMsg, "Generated-By:"); n != 1 {
		t.Fatalf("prepare-commit-msg ran again on --amend and DUPLICATED the trailer block "+
			"-- want exactly 1 Generated-By, got %d:\n%s", n, amendedMsg)
	}
	if got := strings.TrimSpace(run(nil, "git", "log", "-1", "--format=%(trailers:key=Gonk-Bead,valueonly)")); got != "gk-1a2b" {
		t.Fatalf("Gonk-Bead after amend = %q, want gk-1a2b", got)
	}
}
