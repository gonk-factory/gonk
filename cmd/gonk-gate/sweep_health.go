// A DEAD SWEEP MUST BE LOUD (gonk-pop3 item 4).
//
// THE FAILURE THIS EXISTS TO STOP, verbatim from gonk-p7qh. gonk-sweep could
// not reach its own database and logged
//
//	sweep: list running beads failed
//	err: bd list --label gonk::running --json: Access denied for user 'root'
//
// every thirty seconds, FOR DAYS. Meanwhile agent sessions ran to completion
// with real model output, nothing was ever posted, the reaper closed each one
// as orphaned, and beads were left carrying an empty `gonk::` state label that
// no sweep query will ever match again -- permanently stranded. The whole
// outcome path was dead and its only signal was a log line nobody read.
//
// WHAT "LOUD" MEANS HERE, and why it is readiness. Three candidates were on the
// table:
//
//   - A METRIC. Rejected: gonk-sweep is a short-lived exec order, forked by Gas
//     City every 30s. There is no process for Prometheus to scrape, and a
//     pushgateway is new infrastructure to carry one number.
//
//   - REFUSING TO REPORT SUCCESS. Necessary but insufficient: runSweep already
//     returns 1 on a store failure, and that is precisely the signal that went
//     unread for days.
//
//   - READINESS. This is the answer. `kubectl get pods` showing 0/1 READY is
//     the one signal in this stack that is visible WITHOUT knowing to look for
//     it, and it is already wired to something that matters: an unready
//     controller leaves the Service's endpoints, so intake's FireOrder fails
//     and logs `fire_error`, and the work is simply NOT DISPATCHED -- instead
//     of being dispatched into a session whose outcome can never be written,
//     which is exactly how gonk-p7qh stranded beads permanently. Nothing kills
//     the pod (liveness is untouched), the sweep keeps retrying, and readiness
//     returns on its own the moment the store is reachable again.
//
// A MISSING HEALTH FILE IS NOT A FAILURE. A fresh pod has not run a sweep yet,
// and a probe that failed on absence would hold a healthy controller out of its
// own Service until the first tick. Absence reads as "not yet known".
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

// sweepHealthFileEnv names the health record's path. The default lives on the
// controller's /city emptyDir, which every gonk-gate exec order in the pod can
// write and which the readiness probe (same container) can read.
const sweepHealthFileEnv = "GONK_SWEEP_HEALTH_FILE"

const defaultSweepHealthFile = "/city/gonk-sweep-health.json"

// defaultSweepMaxAge is how stale a health record may be before the probe
// treats it as a dead sweep. The sweep order runs every 30s
// (pack/orders/gonk-sweep.toml interval) with a 300s timeout, so one pass can
// legitimately take five minutes. Three times that is a margin wide enough that
// a slow pass never flaps readiness, and narrow enough that an outage is
// visible in single-digit minutes rather than in days.
const defaultSweepMaxAge = 15 * time.Minute

// sweepHealth is the record every pass writes. It is deliberately small and
// carries no project, issue or session identifiers: it answers "is the outcome
// path alive", nothing else.
type sweepHealth struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Stage string    `json:"stage,omitempty"` // which list failed
	Err   string    `json:"err,omitempty"`

	// ConsecutiveFailures counts passes since the last good one. It is what
	// turns an identical line every 30s into one cumulative statement.
	ConsecutiveFailures int       `json:"consecutive_failures"`
	FirstFailureAt      time.Time `json:"first_failure_at,omitempty"`
	LastGoodAt          time.Time `json:"last_good_at,omitempty"`
}

// Outage is how long the current failure run has lasted. Zero when healthy.
func (h sweepHealth) Outage() time.Duration {
	if h.OK || h.FirstFailureAt.IsZero() {
		return 0
	}
	return h.At.Sub(h.FirstFailureAt)
}

func sweepHealthFile() string {
	if v := os.Getenv(sweepHealthFileEnv); v != "" {
		return v
	}
	return defaultSweepHealthFile
}

// readSweepHealth returns the previous record. A missing or unreadable file
// yields the zero value with found=false -- "not yet known", never "failed".
func readSweepHealth(path string) (sweepHealth, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return sweepHealth{}, false
	}
	var h sweepHealth
	if err := json.Unmarshal(b, &h); err != nil {
		return sweepHealth{}, false
	}
	return h, true
}

// writeSweepHealth persists the record atomically. It NEVER fails the sweep:
// losing the health record is a smaller problem than a classification pass that
// aborts, and a sweep that died writing its own health file would reproduce
// gonk-p7qh in a new costume.
func writeSweepHealth(log *slog.Logger, path string, h sweepHealth) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warn("sweep health: could not create the directory", "dir", dir, "err", err)
		return
	}
	b, err := json.Marshal(h)
	if err != nil {
		log.Warn("sweep health: could not encode the record", "err", err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".gonk-sweep-health-")
	if err != nil {
		log.Warn("sweep health: could not open a temp file", "dir", dir, "err", err)
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		log.Warn("sweep health: write failed", "err", err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		log.Warn("sweep health: close failed", "err", err)
		return
	}
	// World-readable on purpose: the readiness probe may run as a different
	// uid than the exec order that wrote it, and this record is not secret.
	if err := os.Chmod(name, 0o644); err != nil {
		log.Warn("sweep health: chmod failed", "err", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		log.Warn("sweep health: rename failed", "path", path, "err", err)
	}
}

// recordSweepFailure folds one failed pass into the health record, writes it,
// and escalates the log.
//
// THE ESCALATION IS THE POINT. The old behaviour wrote the same ERROR every 30
// seconds, so 8640 identical lines a day said exactly as much as one -- there
// was no way to learn from the log how long it had been broken. This writes one
// line that states the outage duration and the number of passes, so a reader
// greps once and knows.
func recordSweepFailure(log *slog.Logger, path, stage string, now time.Time, cause error) sweepHealth {
	prev, _ := readSweepHealth(path)

	h := sweepHealth{
		At: now, OK: false, Stage: stage, Err: errString(cause),
		ConsecutiveFailures: prev.ConsecutiveFailures + 1,
		FirstFailureAt:      prev.FirstFailureAt,
		LastGoodAt:          prev.LastGoodAt,
	}
	if prev.OK || prev.FirstFailureAt.IsZero() {
		h.FirstFailureAt = now
	}
	writeSweepHealth(log, path, h)

	attrs := []any{
		"stage", stage, "err", errString(cause),
		"consecutive_failures", h.ConsecutiveFailures,
		"outage", h.Outage().String(),
		"health_file", path,
	}
	if !h.LastGoodAt.IsZero() {
		attrs = append(attrs, "last_good_pass", h.LastGoodAt)
	}
	// Two consecutive passes is 30 seconds of outage at the sweep's cooldown
	// interval -- long enough that a single transient blip (a restarting Dolt,
	// one dropped connection) does not shout, short enough that a real outage
	// is named almost immediately. The readiness probe does NOT wait for this
	// threshold: it fails on the first recorded failure, because a partial
	// outcome path is already one that can strand a bead.
	if h.ConsecutiveFailures >= deadSweepThreshold {
		log.Error("SWEEP IS DEAD: the outcome path is down -- no session can be classified, "+
			"no outcome reported, and every finished session is being discarded", attrs...)
	} else {
		log.Error("sweep: pass failed", attrs...)
	}
	return h
}

const deadSweepThreshold = 2

// recordSweepSuccess clears the failure run and says so ONCE, when it is news.
func recordSweepSuccess(log *slog.Logger, path string, now time.Time) {
	prev, found := readSweepHealth(path)
	h := sweepHealth{At: now, OK: true, LastGoodAt: now}
	writeSweepHealth(log, path, h)

	if found && !prev.OK && prev.ConsecutiveFailures > 0 {
		log.Warn("sweep: RECOVERED -- the outcome path is alive again",
			"failed_passes", prev.ConsecutiveFailures, "outage", prev.Outage().String())
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------- the probe

// runSweepHealthCheck is `gonk-gate sweep-health`: the readiness probe's body,
// and an operator's one-liner. Exit 0 means the outcome path is believed alive.
//
// It also optionally dials the supervisor port, because a Kubernetes container
// gets exactly one readinessProbe -- so replacing the controller's tcpSocket
// probe with an exec one has to keep doing what the tcpSocket probe did, or the
// change trades one blind spot for another.
func runSweepHealthCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sweep-health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", sweepHealthFile(), "path to the sweep health record")
	maxAge := fs.Duration("max-age", defaultSweepMaxAge, "how stale a health record may be before it reads as a dead sweep")
	supervisor := fs.String("supervisor-addr", "", "host:port to dial as well (empty skips the dial)")
	dialTimeout := fs.Duration("dial-timeout", 3*time.Second, "timeout for --supervisor-addr")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *supervisor != "" {
		conn, err := net.DialTimeout("tcp", *supervisor, *dialTimeout)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "NOT READY: supervisor %s is not accepting connections: %v\n", *supervisor, err)
			return 1
		}
		_ = conn.Close()
	}

	h, found := readSweepHealth(*file)
	if !found {
		// A pod that has not swept yet. Absence is not failure -- see the file
		// comment. Say so, and pass.
		_, _ = fmt.Fprintf(stdout, "READY: no sweep health record at %s yet (a fresh pod has not completed a pass)\n", *file)
		return 0
	}
	if !h.OK {
		_, _ = fmt.Fprintf(stderr, "NOT READY: the sweep has failed %d consecutive passes over %s (stage=%s err=%s)\n",
			h.ConsecutiveFailures, h.Outage(), h.Stage, h.Err)
		return 1
	}
	if age := time.Since(h.At); age > *maxAge {
		_, _ = fmt.Fprintf(stderr, "NOT READY: the last sweep pass was %s ago (max %s) -- the cooldown order is not running\n",
			age.Round(time.Second), *maxAge)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "READY: last sweep pass ok at %s\n", h.At.Format(time.RFC3339))
	return 0
}
