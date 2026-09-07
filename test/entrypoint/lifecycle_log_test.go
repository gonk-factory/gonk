//go:build !ignore

package entrypoint_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func entrypointSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "images", "agent", "entrypoint.sh"))
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	return string(b)
}

// logLine matches the argument of every log "..." call.
var logLine = regexp.MustCompile(`(?m)^\s*log "([^"]*)"`)

// NOTHING THAT COULD BE A CREDENTIAL MAY REACH THE CONTAINER LOG.
//
// log() tees to /proc/1/fd/1 -- pid 1's stdout, and therefore the pod log that
// gonk's spec now tells operators to read. That makes every log call a
// publication decision. Paths, byte counts, exit codes and which-source-was-used
// are exactly what makes a session debuggable; VALUES of keys, tokens and prompt
// bodies are never acceptable there.
//
// This is a source check rather than a runtime one on purpose: it fails in CI on
// the commit that introduces the leak, not after a credential has already been
// written to a log somebody has since shipped to an aggregator.
func TestNoLogLineCanInterpolateACredential(t *testing.T) {
	// Variables whose VALUE is or may contain a secret. Interpolating any of
	// these into a log line publishes it.
	forbidden := []string{
		"GONK_LITELLM_KEY", "GC_WEBHOOK_ARG_LITELLM_KEY", "_key",
		"GONK_BOT", "bot_token", "BOT_TOKEN",
		"GONK_PROMPT", "_pkey", "_prompt",
		"GC_CITY_WRITE_KEY",
	}
	// Names that are SAFE despite matching a forbidden prefix, because they name
	// a path or a flag rather than a value.
	allowed := []string{
		"GONK_LITELLM_KEY_FILE", "GONK_PROMPT_URL", "GONK_PROMPT_WAIT_SECS",
	}

	for _, m := range logLine.FindAllStringSubmatch(entrypointSource(t), -1) {
		line := m[1]
		for _, bad := range forbidden {
			ref := "${" + bad
			idx := strings.Index(line, ref)
			if idx < 0 {
				continue
			}
			var ok bool
			for _, good := range allowed {
				if strings.HasPrefix(line[idx+2:], good) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("log line interpolates %q, which may be a credential:\n  log \"%s\"\n"+
					"Log the SOURCE, LENGTH or PATH instead of the value.", bad, line)
			}
		}
	}
}

// The lifecycle must be legible: an operator reading the pod log has to be able
// to tell where a session got to. These are the anchors gonk's spec promises.
func TestLifecycleAnchorsArePresent(t *testing.T) {
	src := entrypointSource(t)
	for _, want := range []string{
		"session start:",  // identity, before anything can fail
		"session end:",    // every terminal path says so
		"fetching prompt", // the branch that is silent on success otherwise
		"provider check",  // the unmetered-start refusal
	} {
		if !strings.Contains(src, want) {
			t.Errorf("entrypoint never logs %q; the lifecycle is not legible from the pod log", want)
		}
	}
}

// log() must reach pid 1's stdout, or none of the above is visible at all --
// tmux detaches, so stderr alone goes nowhere a reader can get to (gonk-dot).
func TestLogTeesToTheContainerLog(t *testing.T) {
	src := entrypointSource(t)
	if !strings.Contains(src, "/proc/1/fd/1") {
		t.Fatal("log() does not tee to /proc/1/fd/1; kubectl logs on a session pod will be empty")
	}
	if !strings.Contains(src, "2>/dev/null || true") {
		t.Error("the tee is not best-effort; a logging line must never be why an agent fails to start")
	}
}
