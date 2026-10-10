package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogSinkPathDefaultsWhenUnset(t *testing.T) {
	// UNSET must mean "use the default", not "disabled". A chart that never
	// sets GONK_LOG_SINK must still produce readable logs -- silence is the
	// bug (gonk-6a6), so it cannot be the default.
	got := logSinkPath(func(string) (string, bool) { return "", false })
	if got != defaultLogSink {
		t.Fatalf("unset GONK_LOG_SINK = %q, want the default %q", got, defaultLogSink)
	}
}

func TestLogSinkPathDisablingIsExplicit(t *testing.T) {
	for _, v := range []string{"", "off", "none"} {
		got := logSinkPath(func(string) (string, bool) { return v, true })
		if got != "" {
			t.Errorf("GONK_LOG_SINK=%q = %q, want disabled", v, got)
		}
	}
}

func TestLogSinkPathHonoursAnExplicitPath(t *testing.T) {
	got := logSinkPath(func(string) (string, bool) { return "/var/log/gonk.log", true })
	if got != "/var/log/gonk.log" {
		t.Fatalf("explicit path = %q", got)
	}
}

// The whole point of the change: a record must reach the SINK even though
// stderr goes somewhere that gets thrown away. This is the production shape --
// Gas City hands the process a stderr it discards on success.
func TestOpenLogSinkWritesToTheSinkNotOnlyStderr(t *testing.T) {
	dir := t.TempDir()
	discarded := mustCreate(t, filepath.Join(dir, "stderr-that-gets-discarded"))
	sinkPath := filepath.Join(dir, "sink.log")

	w, closeFn, note := openLogSink(sinkPath, discarded)
	defer closeFn()
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}

	slog.New(slog.NewJSONHandler(w, nil)).Error("ITS POD IS LEAKED", "session", "s-go-dm67")

	if got := readFile(t, sinkPath); !strings.Contains(got, "ITS POD IS LEAKED") {
		t.Errorf("sink did not receive the record; got %q", got)
	}
	// stderr still gets a copy: outside an exec order that is where a human
	// is looking.
	if got := readFile(t, discarded.Name()); !strings.Contains(got, "ITS POD IS LEAKED") {
		t.Errorf("stderr lost the record; got %q", got)
	}
}

// Logging must never be able to stop a run. `gonk-gate trailers` is invoked
// from a git hook, where exiting non-zero blocks the commit.
func TestOpenLogSinkDegradesToStderrWhenTheSinkIsUnusable(t *testing.T) {
	dir := t.TempDir()
	stderr := mustCreate(t, filepath.Join(dir, "stderr"))

	bad := filepath.Join(dir, "no-such-directory", "sink.log")
	w, closeFn, note := openLogSink(bad, stderr)
	defer closeFn()

	if note == "" {
		t.Fatal("an unusable sink must report a note so the degradation is visible")
	}
	if !strings.Contains(note, "discards") {
		t.Errorf("note should say what the consequence is; got %q", note)
	}

	slog.New(slog.NewJSONHandler(w, nil)).Info("still running")
	if got := readFile(t, stderr.Name()); !strings.Contains(got, "still running") {
		t.Errorf("degraded writer stopped logging entirely; got %q", got)
	}
}

func TestOpenLogSinkCreatesAnAbsentFile(t *testing.T) {
	dir := t.TempDir()
	stderr := mustCreate(t, filepath.Join(dir, "stderr"))
	sinkPath := filepath.Join(dir, "created.log")

	w, closeFn, note := openLogSink(sinkPath, stderr)
	defer closeFn()
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}
	slog.New(slog.NewJSONHandler(w, nil)).Info("hello")
	if got := readFile(t, sinkPath); !strings.Contains(got, "hello") {
		t.Errorf("absent sink path was not created/written; got %q", got)
	}
}

// A sink that resolves to stderr itself (GONK_LOG_SINK=/dev/stderr, or a
// terminal) must not double every line.
func TestOpenLogSinkDoesNotDoubleWhenTheSinkIsStderr(t *testing.T) {
	dir := t.TempDir()
	stderrPath := filepath.Join(dir, "stderr")
	stderr := mustCreate(t, stderrPath)

	w, closeFn, note := openLogSink(stderrPath, stderr)
	defer closeFn()
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}

	slog.New(slog.NewJSONHandler(w, nil)).Info("once")
	if n := strings.Count(readFile(t, stderrPath), "once"); n != 1 {
		t.Errorf("record written %d times, want exactly 1", n)
	}
}

func TestOpenLogSinkDisabledUsesStderrOnly(t *testing.T) {
	dir := t.TempDir()
	stderr := mustCreate(t, filepath.Join(dir, "stderr"))

	w, closeFn, note := openLogSink("", stderr)
	defer closeFn()
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}
	if w != stderr {
		t.Errorf("disabled sink should hand back stderr unchanged")
	}
}

func mustCreate(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
