package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/internal/testnet"
)

// runProbe drives runHTTPProbe and returns everything it wrote, on both
// streams, plus its exit code. Both streams, because the whole point of
// gonk-hckm is that the MESSAGE is what carries the distinction -- a test that
// looked at only one of them could miss it entirely.
// probeTestTimeout is what every test that is NOT about the timeout gives the
// probe. The 5s default is the probe's contract with its callers, not a bound
// a loaded CI runner must meet: on 2026-10-09 a TLS handshake against a local
// httptest server took longer than 5s under -race on a contended runner, and a
// test asserting "TLS VERIFICATION FAILED" saw "NO RESPONSE within 5s" instead
// (main pipeline 3448). Tests about classification must not race the clock
// (docs/testing-time.md); the timeout tests pass --timeout themselves.
const probeTestTimeout = "2m"

func runProbe(t *testing.T, args ...string) (out string, code int) {
	t.Helper()
	hasTimeout := false
	for _, a := range args {
		if a == "--timeout" || strings.HasPrefix(a, "--timeout=") || a == "-timeout" || strings.HasPrefix(a, "-timeout=") {
			hasTimeout = true
		}
	}
	if !hasTimeout {
		args = append([]string{"--timeout", probeTestTimeout}, args...)
	}
	var stdout, stderr bytes.Buffer
	code = runHTTPProbe(args, &stdout, &stderr)
	return stdout.String() + stderr.String(), code
}

func TestHTTPProbeExitsZeroOnTwoXX(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probe asked for %q, want /healthz", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("probe used %s, want GET", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	out, code := runProbe(t, srv.URL+"/healthz")
	if code != probeExitOK {
		t.Fatalf("exit %d, want %d; output:\n%s", code, probeExitOK, out)
	}
	if !strings.Contains(out, "HTTP 200") {
		t.Fatalf("output %q does not report the status it got", out)
	}
}

// A 204 is a 2xx. The old curl invocation used -f, which fails on >=400 only,
// so this keeps the replacement from being narrower than what it replaced.
func TestHTTPProbeAcceptsAnyTwoXX(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	out, code := runProbe(t, srv.URL)
	if code != probeExitOK {
		t.Fatalf("204 gave exit %d, want %d; output:\n%s", code, probeExitOK, out)
	}
}

// A meter that answers 503 was REACHED. Saying so is the difference between
// "fix the meter" and "fix the network", and it is the distinction gonk-hckm
// destroyed.
func TestHTTPProbeSaysABadStatusMeansTheEndpointWasReached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	out, code := runProbe(t, srv.URL+"/healthz")
	if code != probeExitUnhealthy {
		t.Fatalf("exit %d, want %d; output:\n%s", code, probeExitUnhealthy, out)
	}
	if !strings.Contains(out, "HTTP 503") {
		t.Fatalf("output %q does not name the status that came back", out)
	}
	if !strings.Contains(out, "THE ENDPOINT WAS REACHED") {
		t.Fatalf("output %q does not say the endpoint was reached", out)
	}
}

func TestHTTPProbeSaysConnectionRefusedWhenNothingIsListening(t *testing.T) {
	out, code := runProbe(t, "http://"+testnet.RefusedAddr(t)+"/healthz")
	if code != probeExitUnhealthy {
		t.Fatalf("exit %d, want %d; output:\n%s", code, probeExitUnhealthy, out)
	}
	if !strings.Contains(out, "CONNECTION REFUSED") {
		t.Fatalf("output %q does not name a refused connection", out)
	}
}

func TestHTTPProbeSaysNoResponseWhenTheServerHangs(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(block); srv.Close() }()

	out, code := runProbe(t, "--timeout", "150ms", srv.URL+"/healthz")
	if code != probeExitUnhealthy {
		t.Fatalf("exit %d, want %d; output:\n%s", code, probeExitUnhealthy, out)
	}
	if !strings.Contains(out, "NO RESPONSE within 150ms") {
		t.Fatalf("output %q does not name the timeout it waited", out)
	}
}

// TLS to the meter may go through a private CA (mounted by the chart, its
// directory in SSL_CERT_DIR). An untrusted certificate must not read as "unreachable":
// the endpoint answered.
func TestHTTPProbeSaysTLSVerificationFailedOnAnUntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out, code := runProbe(t, srv.URL+"/healthz")
	if code != probeExitUnhealthy {
		t.Fatalf("exit %d, want %d; output:\n%s", code, probeExitUnhealthy, out)
	}
	if !strings.Contains(out, "TLS VERIFICATION FAILED") {
		t.Fatalf("output %q does not name a TLS verification failure", out)
	}
}

// The probe must not be talked into trusting anything: a healthy endpoint
// behind an untrusted certificate is still a failure, because a probe that
// ignored the certificate would report a meter as healthy across a TLS path
// that no real client can use.
func TestHTTPProbeDoesNotSkipCertificateVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, code := runProbe(t, srv.URL); code == probeExitOK {
		t.Fatal("the probe accepted an untrusted certificate; it must not skip verification")
	}
}

func TestHTTPProbeRefusesAnUnusableURLWithoutDialling(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no argument", nil, "want exactly one URL"},
		{"two arguments", []string{"http://a/", "http://b/"}, "want exactly one URL"},
		{"empty", []string{""}, "the URL is empty"},
		{"no scheme", []string{"gonk-meter:8080/healthz"}, "only http and https"},
		{"bare host", []string{"gonk-meter/healthz"}, "has no scheme"},
		{"wrong scheme", []string{"ftp://gonk-meter/healthz"}, "only http and https"},
		{"no host", []string{"http:///healthz"}, "has no host"},
		{"unparseable", []string{"http://%zz/"}, "not a parseable URL"},
		{"non-positive timeout", []string{"--timeout", "0s", "http://gonk-meter/healthz"}, "--timeout must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runProbe(t, tc.args...)
			if code != probeExitUsage {
				t.Fatalf("exit %d, want %d (usage); output:\n%s", code, probeExitUsage, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("output %q does not contain %q", out, tc.want)
			}
			if !strings.Contains(out, "Nothing was dialled") {
				t.Fatalf("output %q does not state that nothing was dialled", out)
			}
		})
	}
}

// A DNS failure is constructed rather than provoked: a test that relied on a
// real NXDOMAIN would be at the mercy of whatever resolver the box has, and
// plenty of them hijack unknown names into a search page.
func TestDescribeProbeErrorNamesADNSFailureAsUnresolvable(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "http://gonk-meter.gonk.svc/healthz", Err: &net.DNSError{
		Err: "no such host", Name: "gonk-meter.gonk.svc", IsNotFound: true,
	}}
	got := describeProbeError("http://gonk-meter.gonk.svc/healthz", 5*time.Second, err)
	if !strings.Contains(got, "CANNOT RESOLVE") {
		t.Fatalf("message %q does not say the name cannot be resolved", got)
	}
	if !strings.Contains(got, "gonk-meter.gonk.svc") {
		t.Fatalf("message %q does not name the host that failed to resolve", got)
	}
	if !strings.Contains(got, "No connection was attempted") {
		t.Fatalf("message %q does not say that nothing was dialled", got)
	}
}

// THE REQUIREMENT, ASSERTED DIRECTLY. gonk-hckm was one message standing in for
// five different situations. Whatever else changes, no two of these may ever
// read the same again.
func TestDescribeProbeErrorGivesEachFailureModeItsOwnMessage(t *testing.T) {
	const target = "http://gonk-meter:8080/healthz"
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: target, Err: err} }

	modes := map[string]error{
		"dns":     wrap(&net.DNSError{Err: "no such host", Name: "gonk-meter", IsNotFound: true}),
		"refused": wrap(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}),
		"tls":     wrap(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}),
		"timeout": wrap(context.DeadlineExceeded),
		"other":   wrap(errors.New("http: server closed idle connection")),
	}
	seen := map[string]string{}
	for name, err := range modes {
		msg := describeProbeError(target, 5*time.Second, err)
		if msg == "" {
			t.Fatalf("%s produced an empty message", name)
		}
		if prev, dup := seen[msg]; dup {
			t.Fatalf("%s and %s produce the SAME message %q -- that is the gonk-hckm defect", name, prev, msg)
		}
		seen[msg] = name
	}

	// And each one must be recognisable on its own, not merely different.
	want := map[string]string{"dns": "CANNOT RESOLVE", "refused": "CONNECTION REFUSED",
		"tls": "TLS VERIFICATION FAILED", "timeout": "NO RESPONSE within", "other": "REQUEST FAILED"}
	for name, marker := range want {
		msg := describeProbeError(target, 5*time.Second, modes[name])
		if !strings.Contains(msg, marker) {
			t.Errorf("%s message %q does not contain %q", name, msg, marker)
		}
	}
}

// http-probe is what pack/doctor/meter-reachable/run.sh asks `gonk-gate
// capabilities` for before it depends on the subcommand. If it ever left this
// list, the check would report an image that HAS the probe as one too old to
// carry it -- an unrunnable check again, for the opposite reason.
func TestCapabilitiesAdvertisesHTTPProbe(t *testing.T) {
	var buf bytes.Buffer
	if code := runCapabilities(&buf); code != 0 {
		t.Fatalf("capabilities exited %d", code)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "http-probe" {
			found = true
		}
	}
	if !found {
		t.Fatalf("`gonk-gate capabilities` does not list http-probe on a line of its own:\n%s", buf.String())
	}
}

// The probe must not grow a second job. It reads no environment: a doctor
// check whose result depended on configuration it did not name would be a
// quieter version of the same bug.
func TestHTTPProbeIgnoresGonkEnvironment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("GONK_METER_URL", "http://not-this-one.invalid")
	t.Setenv("GONK_CITY", "")
	out, code := runProbe(t, srv.URL)
	if code != probeExitOK {
		t.Fatalf("exit %d, want %d; output:\n%s", code, probeExitOK, out)
	}
	if strings.Contains(out, "not-this-one.invalid") {
		t.Fatalf("the probe used GONK_METER_URL instead of its argument: %s", out)
	}
}
