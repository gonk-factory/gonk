package main

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// THE LAUNCH GAP (gonk-alw, needed before deploy). The dispatch budget reserves
// budgetPreExec (10s) for the time between Gas City starting the order clock
// (order.fired) and gonk-gate capturing processStart. That reserve is not
// measured. This measures it, per run, so it can be: the kernel's start time
// for this PID -- set at fork, and kept across exec -- against processStart.
// Joining each record to its order.fired event gives the whole gap; this record
// alone gives the exec-to-main part (binary page-in and Go runtime init), which
// is the part I/O pressure can stretch.
//
// Caveat: this PID's fork is the shell that finally exec'd gonk-gate. If `sh -c`
// forked a separate shell for scripts/gonk-dispatch.sh, the fork of the outer sh
// is earlier and not seen here; the order.fired join covers it.

// userHZ is the clock-tick unit /proc reports starttime in. It is USER_HZ,
// which the kernel fixes at 100 for /proc regardless of CONFIG_HZ.
const userHZ = 100

type launchTiming struct {
	ProcessStart time.Time
	ExecStart    time.Time // zero when /proc could not be read
	Err          error
}

// readLaunchTiming is called FIRST in main, next to processStart, so the uptime
// read and processStart are microseconds apart.
func readLaunchTiming(processStart time.Time, read func(string) ([]byte, error)) launchTiming {
	lt := launchTiming{ProcessStart: processStart}
	uptimeRaw, err := read("/proc/uptime")
	if err != nil {
		lt.Err = err
		return lt
	}
	statRaw, err := read("/proc/self/stat")
	if err != nil {
		lt.Err = err
		return lt
	}
	uptime, err := parseUptime(string(uptimeRaw))
	if err != nil {
		lt.Err = err
		return lt
	}
	ticks, err := procStartTicks(string(statRaw))
	if err != nil {
		lt.Err = err
		return lt
	}
	sinceStart := uptime - time.Duration(ticks)*time.Second/userHZ
	lt.ExecStart = processStart.Add(-sinceStart)
	return lt
}

// parseUptime reads the first field of /proc/uptime: seconds since boot, to
// the hundredth.
func parseUptime(s string) (time.Duration, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, fmt.Errorf("launch gap: empty /proc/uptime")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("launch gap: /proc/uptime %q: %w", f[0], err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// procStartTicks returns field 22 of /proc/<pid>/stat, starttime, in clock
// ticks since boot. Field 2 (comm) is parenthesised and may itself contain
// spaces or ')', so fields are counted from after the LAST ')': the first
// field there is field 3.
func procStartTicks(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("launch gap: malformed /proc/self/stat")
	}
	fields := strings.Fields(stat[i+1:])
	const idx = 22 - 3
	if len(fields) <= idx {
		return 0, fmt.Errorf("launch gap: /proc/self/stat has %d fields after comm, want > %d", len(fields), idx)
	}
	v, err := strconv.ParseUint(fields[idx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("launch gap: starttime %q: %w", fields[idx], err)
	}
	return v, nil
}

// logLaunchGap records the measurement. INFO, one line per dispatch.
func logLaunchGap(log *slog.Logger, lt launchTiming, bead string) {
	if lt.Err != nil {
		log.Warn("gonk-gate launch timing unavailable", "bead", bead, "err", lt.Err,
			"process_start", lt.ProcessStart.UTC().Format(time.RFC3339Nano))
		return
	}
	log.Info("gonk-gate launch timing",
		"bead", bead,
		"exec_start", lt.ExecStart.UTC().Format(time.RFC3339Nano),
		"process_start", lt.ProcessStart.UTC().Format(time.RFC3339Nano),
		"exec_to_main_ms", lt.ProcessStart.Sub(lt.ExecStart).Milliseconds(),
		"pre_exec_reserve_ms", budgetPreExec.Milliseconds())
}
