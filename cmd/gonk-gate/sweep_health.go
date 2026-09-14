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
//     it. Nothing kills the pod (liveness is untouched), the sweep keeps
//     retrying, and readiness returns on its own once the outcome path is back.
//
// WHAT READINESS DOES AND DOES NOT DO, as observed on a live outage (gonk-7s9p,
// kind, 2026-09-14: the Dolt `gc` user's password rotated server-side, so every
// client holding the mounted Secret was denied). This corrects the original
// design claim that an unready controller is what stops intake dispatching:
//
//   - IT DOES NOT CAUSE INTAKE'S REFUSAL. Intake refused from the first minute,
//     while the pod was still 1/1: Gas City's order-run endpoint answers 503
//     when it cannot write its own tracking bead, and intake logs that as
//     `fire_error`. Once the pod went 0/1 the same refusal became a refused
//     connection. Readiness makes a dead store VISIBLE; it does not gate work.
//     (The one case where readiness does change dispatch is a FALSE unready on
//     a healthy controller -- which is why the window is not tight.)
//   - A DEAD STORE IS CAUGHT BY STALENESS, NOT BY THE FAILED-PASS BRANCH. Gas
//     City would not launch gonk-sweep at all (its order dispatcher reads the
//     same store first and fails closed), so no failure was recorded, the ok
//     record went stale, and no "SWEEP IS DEAD" line was ever logged. Detection
//     time is therefore the probe's --max-age: 14m31s on 15m. The failed-pass
//     branch still covers a store that fails for the sweep but not for Gas City,
//     which is the gonk-p7qh shape; it was not reproducible on the kind run.
//   - Refused ISSUE work is replayed by intake's reconciler issue sweep after
//     recovery; a refused mention is not.
//
// Recovery took 11-19s from the store coming back to a passing record, and
// 16-26s to the pod rejoining its endpoints, over two runs. One pod ran 2h39m
// unready with no restart.
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
// treats it as a dead sweep. It matches the chart's gascity.sweepHealthMaxAge,
// which is what the controller's readinessProbe actually passes.
//
// 15m IS DELIBERATE, NOT SLACK (owner decision, gonk-7s9p). It is also the time
// it takes a dead bead store to show as 0/1 (see the file comment), and it was
// tempting to tighten it. It stays, because under disk pressure -- which gonk
// must tolerate -- Gas City delays the sweep's launch by amounts its source does
// not bound:
//
//   - A pressure-skipped supervisor tick returns before order dispatch, and only
//     every 6th patrol tick is forced through (cmd/gc/fs_pressure.go,
//     cmd/gc/city_runtime.go at GASCITY_REF): up to 180s to launch.
//   - Each tick runs TWO bounded open-work gates for the order (open tracking,
//     then open work; cmd/gc/order_dispatch.go). If either times out, a
//     non-idempotent order like gonk-sweep is skipped for that tick -- and
//     nothing counts consecutive misses or fails open, so each miss can cost
//     another forced tick.
//   - A tick that overruns its 30s slot drops the ticker's pending ticks.
//
// One named scenario -- 30s interval, one forced-tick wait, ONE gate miss, a
// 300s pass, no overrun -- already reaches 11m30s; two misses reach 14m30s. A
// false unready on a healthy but pressured controller is the one case where
// readiness changes dispatch (intake refuses, and a refused mention is never
// replayed), while tightening would buy only a few minutes of earlier 0/1 for an
// outage intake is already refusing. Faster dead-store detection is gonk-hkjh.
//
// internal/buildgate/sweep_health_max_age_test.go keeps the chart value from
// being tightened below that one-miss scenario.
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
		// The likeliest cause is named, because it is the one this message would
		// not suggest on its own: Gas City will not launch the order at all while
		// the bead store is unreachable (gonk-7s9p), so a dead store looks like a
		// stopped order rather than a failing one.
		_, _ = fmt.Fprintf(stderr, "NOT READY: the last sweep pass was %s ago (max %s) -- the cooldown order is not running; "+
			"Gas City does not launch it while the bead store is unreachable, so check the controller log for "+
			"`checking open work for gonk-sweep`\n",
			age.Round(time.Second), *maxAge)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "READY: last sweep pass ok at %s\n", h.At.Format(time.RFC3339))
	return 0
}
