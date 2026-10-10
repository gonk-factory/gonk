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

// logLine matches the argument of a log "..." call ANYWHERE on a line.
//
// It was originally anchored with ^\s*, which missed every log call that is not
// first on its line -- and the entrypoint already has two of those, inside a
// `while read` loop and behind a `[ -n ... ] &&` guard. A leak added to either
// form passed CI. The lesson generalises: a check that only inspects the shapes
// you happened to think of reports a safety it has not established.
var logLine = regexp.MustCompile(`log "([^"]*)"`)

// forbidden names a variable whose VALUE is, or may contain, a credential.
// Interpolating one into a log line publishes it, because log() tees to
// /proc/1/fd/1 -- the pod log the spec now tells operators to read.
var forbidden = []string{
	"GONK_LITELLM_KEY", "GC_WEBHOOK_ARG_LITELLM_KEY", "_key", "_pkey",
	"GONK_BOT", "BOT_TOKEN",
	"GONK_PROMPT", "_prompt",
	// _clean holds the prompt body with only the gonk:model/gonk:meta marker
	// lines stripped -- everything else the caller (or a hostile issue body)
	// put in the prompt is still in it, same as GONK_PROMPT itself.
	"_clean",
	"GC_CITY_WRITE_KEY",
	// GC_ALIAS IS A CAPABILITY, not merely an identifier: it addresses the
	// prompt row AND the rig checkout, and the rig grant is TTL-bounded and
	// RE-FETCHABLE (pkg/rig: DefaultGrantTTL 30m, no consume-on-read). A full
	// alias in an operator-facing log is a live credential for half an hour.
	"GC_ALIAS",
}

// allowed names things that merely START like a forbidden name but denote a
// path, a length or a flag rather than a value.
var allowed = []string{
	"GONK_LITELLM_KEY_FILE", "GONK_PROMPT_URL", "GONK_PROMPT_WAIT_SECS",
	"GONK_ALIAS_PREFIX",
}

// leaksACredential is the shared matcher, so the real check and its negative
// control can never drift apart.
func leaksACredential(rawLine string) bool {
	m := logLine.FindStringSubmatch(rawLine)
	if m == nil {
		return false
	}
	line := m[1]
	for _, bad := range forbidden {
		// BOTH FORMS. ${VAR} and bare $VAR are equally valid POSIX sh, and
		// checking only the braced one let `log "key=$GONK_LITELLM_KEY"`
		// through -- confirmed by injection, not assumed.
		idx, skip := strings.Index(line, "${"+bad), 2
		if idx < 0 {
			idx, skip = strings.Index(line, "$"+bad), 1
		}
		if idx < 0 {
			continue
		}
		var ok bool
		for _, good := range allowed {
			if strings.HasPrefix(line[idx+skip:], good) {
				ok = true
				break
			}
		}
		if !ok {
			return true
		}
	}
	return false
}

// NOTHING THAT COULD BE A CREDENTIAL MAY REACH THE CONTAINER LOG. Every log call
// is a publication decision. Paths, byte counts, exit codes and
// which-source-was-used are what make a session debuggable; values are never
// acceptable. A source check, so it fails on the commit that introduces the leak
// rather than after a credential has been shipped to an aggregator.
func TestNoLogLineCanInterpolateACredential(t *testing.T) {
	src := entrypointSource(t)
	var checked int
	for _, m := range logLine.FindAllStringSubmatch(src, -1) {
		checked++
		if leaksACredential(`log "` + m[1] + `"`) {
			t.Errorf("log line may publish a credential:\n  log \"%s\"\n"+
				"Log the SOURCE, LENGTH or PATH instead of the value.", m[1])
		}
	}
	if checked == 0 {
		t.Fatal("inspected no log lines at all; this test would pass vacuously")
	}
	t.Logf("inspected %d log lines", checked)
}

// THE NEGATIVE CONTROL. A redaction check that cannot be shown to FAIL is a
// check nobody should trust. Each shape below was a genuine blind spot found by
// review, not a hypothetical.
func TestTheRedactionCheckActuallyFails(t *testing.T) {
	leaks := map[string]string{
		"braced":              `	log "key=${GONK_LITELLM_KEY}"`,
		"bare dollar":         `	log "key=$GONK_LITELLM_KEY"`,
		"not first on line":   `	[ -n "$x" ] && log "key=${_key}"`,
		"inside a while loop": `	printf '%s' "$x" | while read -r l; do log "tok=${GONK_BOT}"; done`,
		"the session alias":   `	log "alias=${GC_ALIAS}"`,
		"prompt body":         `	log "prompt=${GONK_PROMPT}"`,
	}
	for name, line := range leaks {
		t.Run(name, func(t *testing.T) {
			if !leaksACredential(line) {
				t.Fatalf("the matcher did NOT flag a leak it must catch:\n  %s", line)
			}
		})
	}

	// And it must not cry wolf: a check that flags safe lines gets disabled.
	for _, line := range []string{
		`	log "materialized LiteLLM key file at ${GONK_LITELLM_KEY_FILE}"`,
		`	log "prompt fetched (${#GONK_PROMPT} bytes)"`,
		`	log "alias=${GONK_ALIAS_PREFIX}... agent=${GC_AGENT:-triage}"`,
		`	log "session end: refused (no model)"`,
	} {
		if leaksACredential(line) {
			t.Errorf("the matcher flagged a SAFE line:\n  %s", line)
		}
	}
}

// The lifecycle must be legible: a reader has to be able to tell where a session
// got to. These are the anchors the spec promises.
func TestLifecycleAnchorsArePresent(t *testing.T) {
	src := entrypointSource(t)
	for _, want := range []string{
		"session start:", "session end:", "fetching prompt", "provider check",
		"no checkout for this session", "source=",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("entrypoint never logs %q; the lifecycle is not legible from the pod log", want)
		}
	}
}

// log() must reach pid 1's stdout, or none of it is visible -- tmux detaches, so
// stderr alone goes nowhere a reader can get to (gonk-dot).
func TestLogTeesToTheContainerLog(t *testing.T) {
	src := entrypointSource(t)
	if !strings.Contains(src, "/proc/1/fd/1") {
		t.Fatal("log() does not tee to /proc/1/fd/1; kubectl logs on a session pod will be empty")
	}
	if !strings.Contains(src, "2>/dev/null || true") {
		t.Error("the tee is not best-effort; a logging line must never be why an agent fails to start")
	}
}

// EVERY EXIT MUST BE ACCOUNTED FOR. A refusal that does not say it ended leaves
// the reader unable to tell "stopped" from "still going" -- and four exit paths
// were silent when this was first written.
func TestEveryExitPathLogsATerminalLine(t *testing.T) {
	lines := strings.Split(entrypointSource(t), "\n")
	for i, l := range lines {
		stmt := strings.TrimSpace(l)
		if !strings.HasPrefix(stmt, "exit ") || strings.HasPrefix(stmt, "exit 0") {
			continue
		}
		var found bool
		for back := i; back >= 0 && back > i-6; back-- {
			if strings.Contains(lines[back], "session end:") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("entrypoint.sh:%d: %q is not preceded by a \"session end:\" log; "+
				"a reader cannot tell \"stopped\" from \"still going\"", i+1, stmt)
		}
	}
}
