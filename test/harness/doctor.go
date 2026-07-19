package harness

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Doctor is the preflight that refuses to waste your 25 minutes. It checks, in
// order, everything a layer needs before a single container starts, and on any
// failure it exits non-zero with a SPECIFIC remedy -- never "something went
// wrong". `make e2e-doctor` runs it; a future executor who ignores it and loses a
// day to podman has only themselves to blame.
//
// The checks are pure functions of DoctorOptions so the whole report is testable
// without a container runtime: a test injects a LookPath that reports missing
// tools and asserts the doctor flags them, with a remedy.

// Level marks how much a failed check hurts.
type Level int

const (
	// Required: an L1/L2 prerequisite. A failure fails the doctor (non-zero exit).
	Required Level = iota
	// L3Only: only e2e (cluster) needs it. A failure is reported but, on its own,
	// does not fail the doctor -- L1/L2 still run.
	L3Only
	// Info: a note (e.g. a warm gitlab volume ⇒ a 12-minute run), never a failure.
	Info
)

// Check is one preflight result.
type Check struct {
	Name   string
	OK     bool
	Level  Level
	Detail string // what we found ("podman 5.2.1", "kubectl not on PATH")
	Remedy string // the one-sentence fix, shown only on failure
}

// Report is the ordered result of every check.
type Report struct {
	Checks []Check
}

// OK reports whether the doctor passes: no Required check failed. An L3Only
// failure does not sink it (L1/L2 can still run); an Info check never can.
func (r Report) OK() bool {
	for _, c := range r.Checks {
		if !c.OK && c.Level == Required {
			return false
		}
	}
	return true
}

// Failures returns every failed check that matters (Required or L3Only).
func (r Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK && c.Level != Info {
			out = append(out, c)
		}
	}
	return out
}

// String renders the check table. A ✅/❌ marker, the detail, and -- on failure --
// the remedy on its own indented line so it cannot be missed.
func (r Report) String() string {
	var b strings.Builder
	for _, c := range r.Checks {
		var mark string
		switch {
		case c.OK:
			mark = "OK  "
		case c.Level == Info:
			mark = "note"
		default:
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "[%s] %-28s %s\n", mark, c.Name, c.Detail)
		if !c.OK && c.Remedy != "" {
			fmt.Fprintf(&b, "        -> %s\n", c.Remedy)
		}
	}
	if r.OK() {
		b.WriteString("doctor: OK\n")
	} else {
		b.WriteString("doctor: FAIL -- fix the checks above before running the suite\n")
	}
	return b.String()
}

// DoctorOptions carries the injectable seams so Doctor is deterministic under
// test. All fields are optional; the zero value uses real system probes.
type DoctorOptions struct {
	// LookPath resolves a binary on PATH. Defaults to exec.LookPath.
	LookPath func(string) (string, error)
	// Ports to check for occupancy. Defaults to AllFixedPorts().
	Ports []int
	// MinMemBytes is the free-memory floor (gitlab-ce alone wants ~4 GB).
	// Defaults to 8 GiB.
	MinMemBytes uint64
	// AvailMemBytes, when non-nil, supplies free memory instead of probing
	// /proc/meminfo (for tests and non-Linux boxes).
	AvailMemBytes func() (uint64, bool)
	// LedgerBackend, when non-empty, overrides GONK_METER_STORE_BACKEND (OD-5 /
	// RECONCILIATION-06 D2: postgres is the only accepted value).
	LedgerBackend string
	// SkipL3 drops the L3-only checks (kind/kubectl/helm/bridge). Useful when the
	// caller only intends to run L1/L2.
	SkipL3 bool
}

const defaultMinMem = 8 * 1024 * 1024 * 1024 // 8 GiB

// Doctor runs every preflight check and returns the report. It NEVER exits or
// prints; the cmd wrapper (test/harness/cmd/doctor) decides the exit code, so the
// checks stay unit-testable.
func Doctor(ctx context.Context, opts DoctorOptions) Report {
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	ports := opts.Ports
	if ports == nil {
		ports = AllFixedPorts()
	}
	minMem := opts.MinMemBytes
	if minMem == 0 {
		minMem = defaultMinMem
	}
	availMem := opts.AvailMemBytes
	if availMem == nil {
		availMem = availableMemBytes
	}
	backend := opts.LedgerBackend
	if backend == "" {
		backend = os.Getenv("GONK_METER_STORE_BACKEND")
	}

	var r Report
	add := func(c Check) { r.Checks = append(r.Checks, c) }

	// 1. A container runtime, and which one.
	rtCheck, rt := runtimeCheck(lookPath)
	add(rtCheck)

	// 2. L3: bridge networking. Podman-under-WSL's CNI bridge is broken, which is
	//    what decides OD-3; if it is broken, kind cannot work and the remedy is to
	//    run L3 against the existing k3s cluster via a kubeconfig.
	if !opts.SkipL3 {
		add(bridgeCheck(ctx, rt))
		// 3. L3 tools.
		for _, tool := range []string{"kind", "kubectl", "helm"} {
			add(toolCheck(lookPath, tool, L3Only,
				fmt.Sprintf("install %s (only L3/e2e needs it; L1/L2 run without it)", tool)))
		}
	}

	// 4. Free memory.
	add(memCheck(availMem, minMem))

	// 5. All fixed ports free.
	add(portsCheck(ports))

	// 6. Ledger backend selected and valid (RECONCILIATION-06 D2: postgres only).
	add(ledgerCheck(backend))

	return r
}

func runtimeCheck(lookPath func(string) (string, error)) (Check, *Runtime) {
	for _, bin := range []string{"podman", "docker"} {
		if _, err := lookPath(bin); err != nil {
			continue
		}
		// The real DetectRuntime shells out for the version; if that fails we still
		// know a binary exists, which is enough for the doctor to proceed.
		rt, err := DetectRuntime()
		if err != nil {
			return Check{
				Name: "container runtime", OK: false, Level: Required,
				Detail: bin + " on PATH but not responding",
				Remedy: "start the runtime (e.g. `systemctl --user start podman`) and re-run the doctor",
			}, nil
		}
		detail := string(rt.Kind) + " (" + rt.Bin + ")"
		if rt.HostNetwork {
			detail += " -- host networking (broken CNI bridge)"
		}
		return Check{Name: "container runtime", OK: true, Level: Required, Detail: detail}, rt
	}
	return Check{
		Name: "container runtime", OK: false, Level: Required,
		Detail: "no podman or docker on PATH",
		Remedy: "install podman (preferred on this box) or docker; the whole suite needs a container runtime",
	}, nil
}

func toolCheck(lookPath func(string) (string, error), tool string, level Level, remedy string) Check {
	if path, err := lookPath(tool); err == nil {
		return Check{Name: tool, OK: true, Level: level, Detail: path}
	}
	return Check{Name: tool, OK: false, Level: level, Detail: "not on PATH", Remedy: remedy}
}

func bridgeCheck(ctx context.Context, rt *Runtime) Check {
	const name = "bridge networking (L3/kind)"
	if rt == nil {
		return Check{Name: name, OK: false, Level: L3Only, Detail: "no runtime to probe",
			Remedy: "install a container runtime first"}
	}
	if rt.HostNetwork {
		// Podman here means the CNI bridge is the very thing we distrust. Probe it:
		// create a throwaway network, run alpine on it, require a non-loopback IP.
		if err := probeBridge(ctx, rt); err != nil {
			return Check{
				Name: name, OK: false, Level: L3Only,
				Detail: "CNI bridge networking is broken (" + err.Error() + ")",
				Remedy: "kind will not work here; run L3 against the existing cluster: `export GONK_E2E_KUBECONFIG=~/.kube/config`",
			}
		}
	}
	return Check{Name: name, OK: true, Level: L3Only, Detail: "bridge reachable"}
}

// probeBridge creates a scratch network, runs `alpine ip addr` on it, and
// requires a non-loopback IPv4. Any failure (network create, run, or a
// loopback-only result) means the bridge is broken.
func probeBridge(ctx context.Context, rt *Runtime) error {
	tok, err := RandomToken(6)
	if err != nil {
		return err
	}
	net := "gonk-doctor-" + strings.ToLower(strings.TrimRight(tok, "="))
	if out, err := exec.CommandContext(ctx, rt.Bin, "network", "create", net).CombinedOutput(); err != nil {
		return fmt.Errorf("network create: %s", firstLine(out))
	}
	defer func() { _ = exec.Command(rt.Bin, "network", "rm", "-f", net).Run() }()

	out, err := exec.CommandContext(ctx, rt.Bin, "run", "--rm", "--network="+net,
		"docker.io/library/alpine:3.20", "ip", "-4", "addr", "show").CombinedOutput()
	if err != nil {
		return fmt.Errorf("alpine run: %s", firstLine(out))
	}
	if !hasNonLoopbackIPv4(string(out)) {
		return fmt.Errorf("container has only loopback addressing")
	}
	return nil
}

func hasNonLoopbackIPv4(ipAddrOut string) bool {
	for _, line := range strings.Split(ipAddrOut, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "inet ") {
			continue
		}
		addr := strings.TrimPrefix(line, "inet ")
		if i := strings.IndexByte(addr, '/'); i >= 0 {
			addr = addr[:i]
		}
		if addr != "" && !strings.HasPrefix(addr, "127.") {
			return true
		}
	}
	return false
}

func memCheck(availMem func() (uint64, bool), minMem uint64) Check {
	const name = "free memory (>= 8 GiB)"
	avail, ok := availMem()
	if !ok {
		return Check{Name: name, OK: true, Level: Info, Detail: "unknown (could not read /proc/meminfo)"}
	}
	detail := fmt.Sprintf("%.1f GiB available", float64(avail)/(1024*1024*1024))
	if avail < minMem {
		return Check{
			Name: name, OK: false, Level: Required, Detail: detail,
			Remedy: "free memory or close other workloads; gitlab-ce alone wants ~4 GiB and the suite needs ~8 GiB",
		}
	}
	return Check{Name: name, OK: true, Level: Required, Detail: detail}
}

func portsCheck(ports []int) Check {
	const name = "fixed loopback ports free"
	if err := CheckPortsFree(ports...); err != nil {
		return Check{
			Name: name, OK: false, Level: Required,
			Detail: err.Error(),
			Remedy: "stop the leftover container(s) holding the port(s), e.g. `podman rm -f $(podman ps -aq)`",
		}
	}
	return Check{Name: name, OK: true, Level: Required, Detail: strconv.Itoa(len(ports)) + " ports free"}
}

func ledgerCheck(backend string) Check {
	const name = "ledger backend (GONK_METER_STORE_BACKEND)"
	switch backend {
	case "postgres":
		return Check{Name: name, OK: true, Level: Required, Detail: "postgres"}
	case "":
		return Check{
			Name: name, OK: false, Level: Required, Detail: "unset",
			Remedy: "export GONK_METER_STORE_BACKEND=postgres (RECONCILIATION-06 D2: postgres is the only ledger; dolt was dropped)",
		}
	default:
		return Check{
			Name: name, OK: false, Level: Required, Detail: backend + " (not supported)",
			Remedy: "set GONK_METER_STORE_BACKEND=postgres; there is no Dolt ledger store (RECONCILIATION-06 D2)",
		}
	}
}

// availableMemBytes reads MemAvailable from /proc/meminfo (Linux). The second
// return is false on any box without it, in which case the memory check is Info.
func availableMemBytes() (uint64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
