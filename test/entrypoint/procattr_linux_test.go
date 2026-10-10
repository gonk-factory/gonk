//go:build linux

package entrypoint

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// entrypointSysProcAttr: its own process group (one signal reaches every
// child), and Pdeathsig so the kernel SIGKILLs it if this test binary dies
// without running any cleanup (gonk-gp44; see proc_harness_test.go).
func entrypointSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// procStat returns /proc/<pid>/stat's state and pgrp fields.
func procStat(pid string) (state string, pgrp int, ok bool) {
	b, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return "", 0, false
	}
	// comm (field 2) is parenthesised and may contain spaces; fields after the
	// LAST ')' are: state ppid pgrp ...
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 3 {
		return "", 0, false
	}
	g, err := strconv.Atoi(f[2])
	if err != nil {
		return "", 0, false
	}
	return f[0], g, true
}

// dead reports whether a /proc state letter is a process that has exited.
func dead(state string) bool { return state == "Z" || state == "X" }

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid int) bool {
	state, _, ok := procStat(strconv.Itoa(pid))
	return ok && !dead(state)
}

// liveGroupMembers lists "pid cmdline" for every non-zombie process whose
// process group is pgid.
func liveGroupMembers(pgid int) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return []string{"cannot read /proc: " + err.Error()}
	}
	var alive []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		state, g, ok := procStat(e.Name())
		if !ok || g != pgid || dead(state) {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		alive = append(alive, e.Name()+" "+strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " ")))
	}
	return alive
}
