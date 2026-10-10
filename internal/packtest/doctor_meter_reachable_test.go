// doctor/meter-reachable, run FOR REAL in an environment with no curl.
//
// gonk-hckm exists because the check worked everywhere it was ever tried. It
// shells out to an HTTP client, and every developer box, CI runner and test
// harness has curl on it; the one place it does NOT is the controller runtime
// image, which is the only place the check actually runs. So a test that runs
// the script with the ambient PATH proves nothing at all -- it reproduces
// exactly the substitution that let this ship and fail on every controller boot
// for a fortnight.
//
// Every test in this file therefore runs run.sh with PATH pointing at a
// sandbox directory holding ONLY the binaries the runtime image would give it,
// and asserts up front that curl is not reachable from there.
package packtest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gonk-factory/gonk/internal/testnet"
)

func meterReachableScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(packRoot(t), "doctor", "meter-reachable", "run.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("doctor/meter-reachable/run.sh: %v", err)
	}
	return p
}

var (
	gateBuildOnce sync.Once
	gateBuildPath string
	gateBuildErr  error
)

// buildGonkGate compiles the real cmd/gonk-gate once per test binary. The real
// binary, not a stub: the thing under test is whether the check can perform an
// HTTP GET with no curl in the image, and a stub gonk-gate would assume the
// answer.
func buildGonkGate(t *testing.T) string {
	t.Helper()
	gateBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gonk-gate-build-")
		if err != nil {
			gateBuildErr = err
			return
		}
		out := filepath.Join(dir, "gonk-gate")
		repo := filepath.Join(packRoot(t), "..")
		cmd := exec.Command("go", "build", "-mod=vendor", "-o", out, "./cmd/gonk-gate")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=vendor", "GOPROXY=off")
		if b, err := cmd.CombinedOutput(); err != nil {
			gateBuildErr = fmt.Errorf("go build ./cmd/gonk-gate: %v\n%s", err, b)
			return
		}
		gateBuildPath = out
	})
	if gateBuildErr != nil {
		t.Fatalf("building gonk-gate for the doctor check: %v", gateBuildErr)
	}
	return gateBuildPath
}

// curllessPATH builds a directory that stands in for the controller runtime
// image's PATH: grep (the image has it -- the chart's own workstore-import
// probe pipes into it), optionally gonk-gate, and NOTHING ELSE. In particular
// no curl and no wget.
//
// It asserts its own premise. A sandbox that accidentally leaked curl would
// make every test below green for the wrong reason, which is the precise shape
// of the bug being fixed.
func curllessPATH(t *testing.T, gate string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"grep"} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not on this box, so the runtime-image PATH cannot be mirrored: %v", tool, err)
		}
		if err := os.Symlink(src, filepath.Join(dir, tool)); err != nil {
			t.Fatalf("symlink %s: %v", tool, err)
		}
	}
	if gate != "" {
		if err := os.Symlink(gate, filepath.Join(dir, "gonk-gate")); err != nil {
			t.Fatalf("symlink gonk-gate: %v", err)
		}
	}
	for _, forbidden := range []string{"curl", "wget"} {
		probe := exec.Command("/bin/sh", "-c", "command -v "+forbidden)
		probe.Env = []string{"PATH=" + dir}
		if err := probe.Run(); err == nil {
			t.Fatalf("%s IS reachable from the sandbox PATH %s -- these tests would prove nothing", forbidden, dir)
		}
	}
	return dir
}

// runMeterCheck executes run.sh with the given PATH and environment and returns
// its combined output and exit code.
func runMeterCheck(t *testing.T, path string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", meterReachableScript(t))
	cmd.Env = append([]string{"PATH=" + path}, env...)
	b, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running run.sh: %v\n%s", err, b)
		}
		code = ee.ExitCode()
	}
	return string(b), code
}

func healthyMeter(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// THE TEST THAT WOULD HAVE CAUGHT gonk-hckm. A healthy meter, no curl anywhere
// on PATH: the check must PASS.
func TestMeterReachableCheckPassesWithoutCurl(t *testing.T) {
	path := curllessPATH(t, buildGonkGate(t))
	srv := healthyMeter(t, http.StatusOK)

	out, code := runMeterCheck(t, path, "GONK_METER_URL="+srv.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0, against a healthy meter with no curl on PATH; output:\n%s", code, out)
	}
	if !strings.Contains(out, "gonk-meter is reachable") {
		t.Fatalf("output %q does not report the meter as reachable", out)
	}
	if !strings.Contains(out, "HTTP 200") {
		t.Fatalf("output %q does not name the status the meter actually returned -- the check must report what it observed", out)
	}
}

// A trailing slash on GONK_METER_URL must not produce //healthz. The old
// script trimmed it; so must this one.
func TestMeterReachableCheckTrimsATrailingSlash(t *testing.T) {
	path := curllessPATH(t, buildGonkGate(t))
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out, code := runMeterCheck(t, path, "GONK_METER_URL="+srv.URL+"/")
	if code != 0 {
		t.Fatalf("exit %d, want 0; output:\n%s", code, out)
	}
	if got != "/healthz" {
		t.Fatalf("the check requested %q, want /healthz", got)
	}
}

// A CHECK THAT CANNOT RUN MUST NOT LOOK LIKE A CHECK THAT RAN AND FAILED.
// With no gonk-gate on PATH the check has no way to dial anything, and must
// say exactly that -- not blame the meter, which is what the curl version did.
func TestMeterReachableCheckSaysSoWhenItCannotRunAtAll(t *testing.T) {
	path := curllessPATH(t, "")
	srv := healthyMeter(t, http.StatusOK)

	out, code := runMeterCheck(t, path, "GONK_METER_URL="+srv.URL)
	if code != 2 {
		t.Fatalf("exit %d, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "CANNOT RUN") {
		t.Fatalf("output %q does not announce that the check could not run", out)
	}
	// The REASON has to be right too, not just the verdict. Without the
	// explicit PATH guard the capability handshake still catches a missing
	// binary -- `gonk-gate capabilities` exits 127, the pipe to grep finds
	// nothing -- but it then reports the binary as too OLD, which is a
	// different bug with a different fix.
	if !strings.Contains(out, "not on PATH") {
		t.Fatalf("output %q does not say that gonk-gate is not on PATH -- a check that cannot run "+
			"must name the reason it cannot run, not merely that it could not", out)
	}
	if !strings.Contains(out, "NOT contacted") {
		t.Fatalf("output %q does not state that the meter was never contacted", out)
	}
	if strings.Contains(out, "is not healthy") {
		t.Fatalf("output %q blames the meter for a missing dependency -- that is gonk-hckm", out)
	}
}

// An image whose gonk-gate predates the probe is ALSO a check that cannot run.
// Without the capability handshake this would fall through to gonk-gate's
// unknown-subcommand path, which loads the gate config and exits 2 saying
// "misconfiguration" -- the same lie in different words.
func TestMeterReachableCheckSaysSoWhenGonkGateIsTooOld(t *testing.T) {
	path := curllessPATH(t, "")
	stub := filepath.Join(path, "gonk-gate")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = capabilities ]; then\n" +
		"  printf 'workstore-import\\nworkstore-export\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo 'misconfiguration' >&2\n" +
		"exit 2\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the old-gonk-gate stub: %v", err)
	}
	srv := healthyMeter(t, http.StatusOK)

	out, code := runMeterCheck(t, path, "GONK_METER_URL="+srv.URL)
	if code != 2 {
		t.Fatalf("exit %d, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "CANNOT RUN") || !strings.Contains(out, "http-probe capability") {
		t.Fatalf("output %q does not say the binary is too old to carry the probe", out)
	}
	if strings.Contains(out, "is not healthy") {
		t.Fatalf("output %q blames the meter for an out-of-date binary", out)
	}
}

func TestMeterReachableCheckSaysSoWhenTheURLIsUnset(t *testing.T) {
	path := curllessPATH(t, buildGonkGate(t))

	out, code := runMeterCheck(t, path)
	if code != 2 {
		t.Fatalf("exit %d, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "GONK_METER_URL is not set") {
		t.Fatalf("output %q does not name the unset variable", out)
	}
	if !strings.Contains(out, "not an unhealthy meter") {
		t.Fatalf("output %q does not separate missing configuration from an unhealthy meter", out)
	}
}

// THE NEGATIVE DIRECTION. A genuinely unreachable address must be reported as
// unreachable, distinctly from every "cannot run" case above.
//
// The address is unreachable BY CONSTRUCTION, for as long as the script and
// the gonk-gate it starts take: testnet.RefusedAddr holds the port bound and
// unlistened until the test ends. gonk-uv82 was this test with a port that had
// only been proved refused a moment earlier -- a neighbour's server was handed
// it before the probe arrived, and the check truthfully printed "reachable ...
// HTTP 204".
func TestMeterReachableCheckReportsAGenuinelyUnreachableMeter(t *testing.T) {
	path := curllessPATH(t, buildGonkGate(t))
	addr := testnet.RefusedAddr(t)

	out, code := runMeterCheck(t, path, "GONK_METER_URL=http://"+addr)
	if code != 2 {
		t.Fatalf("exit %d, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "CONNECTION REFUSED") {
		t.Fatalf("output %q does not name a refused connection", out)
	}
	if strings.Contains(out, "CANNOT RUN") {
		t.Fatalf("output %q reports a real network failure as an unrunnable check", out)
	}
}

// A meter that answers 503 was reached. Conflating that with "unreachable" is
// how an operator ends up reading NetworkPolicy instead of meter logs.
func TestMeterReachableCheckReportsAnUnhealthyButReachableMeter(t *testing.T) {
	path := curllessPATH(t, buildGonkGate(t))
	srv := healthyMeter(t, http.StatusServiceUnavailable)

	out, code := runMeterCheck(t, path, "GONK_METER_URL="+srv.URL)
	if code != 2 {
		t.Fatalf("exit %d, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "HTTP 503") || !strings.Contains(out, "THE ENDPOINT WAS REACHED") {
		t.Fatalf("output %q does not say the meter answered, badly", out)
	}
}

// THE REQUIREMENT, ASSERTED END TO END: five situations, five different
// messages, out of the real script. gonk-hckm was one message for all of them.
//
// THE URLS ARE ERASED BEFORE COMPARING, and that is the whole test. The first
// version of this compared the raw outputs -- which always embed a different
// ephemeral 127.0.0.1:PORT per case, so every pair differed no matter what the
// script said, and collapsing two branches to the identical sentence left it
// green. A mutation probe caught it (M19: drop ${probe_out} from the
// not-healthy branch, making "refused" and "bad status" read identically). A
// distinctness test that a per-case value makes trivially true asserts nothing.
func TestMeterReachableCheckGivesEachSituationItsOwnMessage(t *testing.T) {
	gate := buildGonkGate(t)
	withGate := curllessPATH(t, gate)
	withoutGate := curllessPATH(t, "")

	// The dead address is HELD for the whole test (testnet.RefusedAddr binds
	// it and never listens), so neither of the live servers below nor any
	// other process on the box can be handed it. Before that it was a freed
	// port: started in the wrong order the 503 and 200 servers themselves were
	// handed it (gonk-myvu, 1/90 under make hunt), and in the right order a
	// neighbour's server still could be (gonk-uv82).
	sickURL := healthyMeter(t, http.StatusServiceUnavailable).URL
	okURL := healthyMeter(t, http.StatusOK).URL
	refusedURL := testnet.RefusedURL(t)

	cases := []struct {
		name string
		path string
		env  []string
	}{
		{"gonk-gate missing", withoutGate, []string{"GONK_METER_URL=" + refusedURL}},
		{"GONK_METER_URL unset", withGate, nil},
		{"connection refused", withGate, []string{"GONK_METER_URL=" + refusedURL}},
		{"bad status", withGate, []string{"GONK_METER_URL=" + sickURL}},
		{"healthy", withGate, []string{"GONK_METER_URL=" + okURL}},
	}
	erase := strings.NewReplacer(refusedURL, "<URL>", sickURL, "<URL>", okURL, "<URL>")

	seen := map[string]string{}
	for _, tc := range cases {
		raw, _ := runMeterCheck(t, tc.path, tc.env...)
		out := strings.TrimSpace(erase.Replace(raw))
		if out == "" {
			t.Fatalf("%s produced no message at all", tc.name)
		}
		if prev, dup := seen[out]; dup {
			t.Fatalf("%q and %q produce the IDENTICAL message (once the address is erased) %q "+
				"-- that is the gonk-hckm defect", tc.name, prev, out)
		}
		seen[out] = tc.name
	}
}

// The check must never reacquire a curl dependency. Comments about the old one
// are fine and wanted; an executable line that calls it is not.
func TestMeterReachableCheckExecutesNoHTTPCLI(t *testing.T) {
	b, err := os.ReadFile(meterReachableScript(t))
	if err != nil {
		t.Fatalf("reading run.sh: %v", err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		for _, tool := range []string{"curl", "wget"} {
			if strings.Contains(code, tool) {
				t.Errorf("run.sh:%d executes %s, which the controller runtime image does not ship "+
					"(images/Dockerfile.controller keeps it out on purpose): %s", i+1, tool, code)
			}
		}
	}
}
