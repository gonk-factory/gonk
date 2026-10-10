//go:build images

package images

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/testnet"
)

// doctor/meter-reachable, RUN INSIDE THE IMAGE IT ACTUALLY RUNS IN.
//
// gonk-hckm: the check shelled out to curl, images/Dockerfile.controller keeps
// curl out of the runtime image on purpose, and so the check reported the meter
// unreachable on every controller boot for a fortnight WITHOUT DIALLING IT. It
// survived because it works everywhere else -- every dev box and CI runner has
// curl. The only environment that disproves it is this image, so the test lives
// here.
//
// These run against the built image (`make controller-image`) under the
// `images` tag; `make gate` does not run them. internal/packtest's
// doctor_meter_reachable_test.go mirrors them in a curl-less PATH sandbox for
// the fast gate.

// THE PREMISE. If curl ever appeared in the runtime image, the tests below
// would still pass while proving nothing, and the original bug could be
// reintroduced unseen. Assert the absence first.
func TestControllerImageShipsNoHTTPCLI(t *testing.T) {
	image, _ := controllerImage(t)
	for _, tool := range []string{"curl", "wget"} {
		out, code := runIn(t, image, "/bin/sh", "-c", "command -v "+tool)
		if code == 0 {
			t.Fatalf("%s IS in the controller runtime image (%s) -- images/Dockerfile.controller "+
				"keeps it out deliberately, and the doctor check must not depend on one", tool, strings.TrimSpace(out))
		}
	}
}

// runDoctorCheckInController executes the baked pack's meter-reachable check
// inside the image with the given GONK_METER_URL (empty leaves it unset).
func runDoctorCheckInController(t *testing.T, meterURL string) (string, int) {
	t.Helper()
	script := "exec /opt/gonk/pack/doctor/meter-reachable/run.sh"
	if meterURL != "" {
		script = "GONK_METER_URL='" + meterURL + "' " + script
	}
	image, _ := controllerImage(t)
	return runIn(t, image, "/bin/sh", "-c", script)
}

// THE TEST THAT WOULD HAVE CAUGHT gonk-hckm: a healthy endpoint, the real
// image, no curl in it -- the check must PASS.
//
// The container runs with --network=host (runIn), so a server bound to the
// host's loopback is the same 127.0.0.1 inside it.
func TestControllerImageMeterReachableCheckPassesAgainstAHealthyEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out, code := runDoctorCheckInController(t, srv.URL)
	if code != 0 {
		t.Fatalf("doctor/meter-reachable exited %d against a healthy endpoint inside the controller image; output:\n%s", code, out)
	}
	if !strings.Contains(out, "gonk-meter is reachable") || !strings.Contains(out, "HTTP 200") {
		t.Fatalf("output %q does not report the observed status", out)
	}
}

// THE NEGATIVE DIRECTION, in the same image: a name that does not resolve must
// be reported as unresolvable, and must NOT read like a check that could not
// run. `.invalid` is reserved by RFC 6761 and never resolves.
func TestControllerImageMeterReachableCheckReportsAnUnresolvableMeter(t *testing.T) {
	out, code := runDoctorCheckInController(t, "http://gonk-meter-does-not-exist.invalid:8080")
	if code != 2 {
		t.Fatalf("doctor/meter-reachable exited %d against an unresolvable host, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "CANNOT RESOLVE") {
		t.Fatalf("output %q does not name a name-resolution failure", out)
	}
	if strings.Contains(out, "CANNOT RUN") {
		t.Fatalf("output %q reports a real DNS failure as an unrunnable check", out)
	}
}

// A dead TCP address, same image.
//
// THE ASSERTION IS DELIBERATELY "refused OR timed out", and that is a property
// of the BOX, not slack in the test: on the WSL2 dev box a connection to a
// closed loopback port is dropped rather than refused, and curl reports the
// identical "Connection timed out after 5004 milliseconds" there, so demanding
// ECONNREFUSED would fail for a reason that has nothing to do with the image.
// The classification of a refused connection is pinned exactly, on a real
// ECONNREFUSED, by cmd/gonk-gate's
// TestHTTPProbeSaysConnectionRefusedWhenNothingIsListening. What this test owns
// is the part the image decides: a dead address must read as a transport
// failure and NEVER as a check that could not run.
func TestControllerImageMeterReachableCheckReportsADeadAddress(t *testing.T) {
	addr := testnet.RefusedAddr(t)

	out, code := runDoctorCheckInController(t, "http://"+addr)
	if code != 2 {
		t.Fatalf("doctor/meter-reachable exited %d against a dead address, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "CONNECTION REFUSED") && !strings.Contains(out, "NO RESPONSE within") {
		t.Fatalf("output %q names neither a refused connection nor a timeout", out)
	}
	if strings.Contains(out, "CANNOT RUN") {
		t.Fatalf("output %q reports a real network failure as an unrunnable check", out)
	}
	if !strings.Contains(out, "is not healthy") {
		t.Fatalf("output %q does not report the meter as unhealthy", out)
	}
}

// Missing configuration is not an unhealthy meter, and the image must say which
// it is.
func TestControllerImageMeterReachableCheckReportsAnUnsetURL(t *testing.T) {
	out, code := runDoctorCheckInController(t, "")
	if code != 2 {
		t.Fatalf("doctor/meter-reachable exited %d with GONK_METER_URL unset, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "GONK_METER_URL is not set") {
		t.Fatalf("output %q does not name the unset variable", out)
	}
}

// The image's own gonk-gate must advertise the probe the baked pack depends on.
// Same shape as the chart's workstore-import probe, and the same failure if it
// ever drifts: the check would call itself unrunnable on an image that can run
// it perfectly well.
func TestControllerImageGonkGateAdvertisesHTTPProbe(t *testing.T) {
	image, _ := controllerImage(t)
	out, code := runIn(t, image, "/bin/sh", "-c",
		"if gonk-gate capabilities 2>/dev/null | grep -qx http-probe; then echo probe-passed; fi")
	if code != 0 || strings.TrimSpace(out) != "probe-passed" {
		t.Fatalf("the pack's http-probe capability handshake did not pass in the controller image (exit %d):\n%s", code, out)
	}
}
