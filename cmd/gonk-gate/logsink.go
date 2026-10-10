package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// defaultLogSink is the container's own stdout, reached through PID 1's file
// descriptor.
//
// WHY THIS EXISTS (gonk-6a6). gonk-dispatch and gonk-sweep run as Gas City
// EXEC ORDERS, and Gas City runs them with BOTH cmd.Stdout and cmd.Stderr
// pointed at one in-memory buffer (cmd/gc/order_dispatch.go shellExecRunner,
// GASCITY_REF 4fda5a28). That buffer is logged only when the order FAILS; on
// success `output` is never referenced again. gonk-sweep is deliberately built
// to exit 0 -- a sweep that failed its order would re-run the whole tick -- so
// in production EVERY line it wrote was discarded, including the ones written
// specifically to be seen:
//
//	"sweep: session close failed -- ITS POD IS LEAKED"
//	"reap: cannot read the bead store; reaping NOTHING this tick"
//	"sweep: triage batch rejected, applied nothing"
//
// An ERROR nobody can read is the same as no ERROR.
//
// Writing to /proc/1/fd/1 side-steps that buffer entirely: PID 1 in the
// controller container is `gc` itself and its stdout is the container log
// pipe, so a write here lands in `kubectl logs deploy/gonk-controller`.
// Verified against the live cluster 2026-08-10 (wrote a marker, read it back
// out of the pod log).
//
// The pod log rather than a file on the city volume, deliberately: /city is an
// emptyDir, nothing rotates it, and this namespace has already lost pods to
// eviction. The kubelet does rotate the container log.
//
// One caveat worth knowing: gc writes to the same pipe. A single slog record
// is one Write of one line, and pipe writes under PIPE_BUF (4096) are atomic,
// so ordinary records cannot interleave with gc's. A record longer than that
// could tear.
const defaultLogSink = "/proc/1/fd/1"

// logSinkEnv names the override. Set it to a path to send records elsewhere,
// or to "off"/"none"/"" to disable the extra sink and log to stderr only.
const logSinkEnv = "GONK_LOG_SINK"

// logSinkPath resolves the sink path from the environment.
//
// UNSET means "use the default", not "disabled": a chart that forgets to set
// this must still get readable logs, because silence is the bug being fixed.
// Disabling is available but has to be asked for by name.
func logSinkPath(lookup func(string) (string, bool)) string {
	v, ok := lookup(logSinkEnv)
	if !ok {
		return defaultLogSink
	}
	switch v {
	case "", "off", "none":
		return ""
	}
	return v
}

// openLogSink returns the writer gonk-gate's logger should use: stderr, plus a
// durable sink when one can be opened.
//
// It NEVER reports a failure the caller should exit on. Logging is not worth
// failing a run over, and `gonk-gate trailers` in particular is invoked from a
// git hook where a non-zero exit blocks a commit. Every failure degrades to
// "stderr only" and explains itself through the returned note, which the
// caller should log once.
func openLogSink(path string, stderr *os.File) (w io.Writer, closeFn func(), note string) {
	noop := func() {}
	if path == "" {
		return stderr, noop, ""
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, os.ErrNotExist) {
		// Only meaningful for a real filesystem path an operator chose; the
		// default sink already exists or is unusable.
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	}
	if err != nil {
		return stderr, noop, fmt.Sprintf(
			"log sink %q unusable (%v); logging to stderr ONLY, which a Gas City exec order discards on success", path, err)
	}

	// If the sink and stderr are the same open file, writing to both would
	// double every line. Cheap to detect, and confusing to read otherwise.
	if sameOpenFile(f, stderr) {
		_ = f.Close()
		return stderr, noop, ""
	}

	return io.MultiWriter(stderr, f), func() { _ = f.Close() }, ""
}

// sameOpenFile reports whether two open files are the same underlying file.
// Any stat failure answers false: duplicated logs are a smaller problem than
// dropped ones.
func sameOpenFile(a, b *os.File) bool {
	if a == nil || b == nil {
		return false
	}
	ai, err := a.Stat()
	if err != nil {
		return false
	}
	bi, err := b.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
