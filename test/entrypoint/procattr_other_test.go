//go:build !linux

package entrypoint

import "syscall"

// entrypointSysProcAttr off Linux: own process group only. There is no
// Pdeathsig, so a killed test binary CAN leak its entrypoint here
// (gonk-gp44); TestAKilledTestBinaryTakesItsEntrypointWithIt says so by
// failing rather than skipping.
func entrypointSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// processAlive: signal 0 probes existence (a zombie still answers).
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// liveGroupMembers: signal 0 to the group; no per-member detail off Linux.
func liveGroupMembers(pgid int) []string {
	if syscall.Kill(-pgid, 0) == nil {
		return []string{"(some member of the group; no /proc to name it)"}
	}
	return nil
}
