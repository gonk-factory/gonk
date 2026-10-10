// `gonk-gate http-probe`: one HTTP GET, with a timeout, and a message that
// says WHICH failure happened. Nothing else -- no auth, no body parsing, no
// retry, no knowledge of what is on the other end.
//
// WHY IT EXISTS (gonk-hckm). pack/doctor/meter-reachable/run.sh used to dial
// the meter with curl. images/Dockerfile.controller deliberately keeps curl out
// of the runtime image ("A separate stage so curl/tar never ship in the runtime
// image"), so the shell returned 127, `if ! curl ...` swallowed that as a
// failed request, and the check printed
//
//	gonk-meter at http://gonk-meter:8080 did not answer /healthz
//
// on every controller boot from 2026-09-08 onward -- WITHOUT EVER DIALLING THE
// METER. The meter was healthy the whole time (/healthz 200, /readyz 200). The
// cost was not the failing check; it was that the message named a network path
// that no packet ever travelled, so reading it sent an operator hunting a
// NetworkPolicy gap (gonk-zjk7) instead of a missing binary.
//
// THE LESSON THIS FILE ENCODES: a check that COULD NOT RUN must never render as
// a check that RAN AND FAILED, and "unreachable" must never render the same way
// as "reached it, it is sick". Every branch below names which of those it is,
// in words, because the doctor exit protocol has only three codes (0/1/2) and
// cannot carry the distinction on its own.
//
// It is on gonk-gate rather than in a new binary because gonk-gate already
// ships in the controller image -- the image the check runs in -- and because
// `gonk-gate capabilities` already gives a caller a way to ask whether this
// subcommand exists before depending on it.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// httpProbeDefaultTimeout matches the 5s the curl invocation this replaced
// used (`curl --max-time 5`). A doctor check is a warm-up, not a diagnosis:
// it must answer quickly or say it could not.
const httpProbeDefaultTimeout = 5 * time.Second

// The probe's exit codes, inside gonk-gate's own documented contract
// (main.go): 0 did what it was asked, 1 an infrastructure problem, 2 a
// misconfiguration a retry cannot fix.
//
// 1 covers BOTH "could not reach it" and "reached it, got a 5xx" on purpose:
// they are the same kind of answer -- the endpoint is not serving -- and the
// caller that needs to tell them apart reads the message, which always does.
// Splitting them into 1 and 3 would have put a third code into a contract the
// pack's order wrappers already depend on, to carry information the message
// carries anyway.
const (
	probeExitOK        = 0
	probeExitUnhealthy = 1
	probeExitUsage     = 2
)

// probeBodyPeek bounds how much of the response body is drained. The body is
// not inspected at all; draining a little keeps a well-behaved server from
// seeing a truncated read, and the cap keeps a misbehaving one from streaming
// into this process forever.
const probeBodyPeek = 4 << 10

// runHTTPProbe is `gonk-gate http-probe [--timeout d] <url>`.
func runHTTPProbe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("http-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", httpProbeDefaultTimeout, "how long to wait for the whole request")
	if err := fs.Parse(args); err != nil {
		return probeExitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintf(stderr, "http-probe: want exactly one URL to GET, got %d. "+
			"Usage: gonk-gate http-probe [--timeout 5s] <url>. Nothing was dialled.\n", fs.NArg())
		return probeExitUsage
	}
	target := fs.Arg(0)
	if msg := probeURLProblem(target); msg != "" {
		_, _ = fmt.Fprintf(stderr, "http-probe: %s. Nothing was dialled.\n", msg)
		return probeExitUsage
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintf(stderr, "http-probe: --timeout must be positive, got %s. Nothing was dialled.\n", *timeout)
		return probeExitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "http-probe: %s is not a URL this can GET: %v. Nothing was dialled.\n", target, err)
		return probeExitUsage
	}
	req.Header.Set("User-Agent", "gonk-gate/http-probe")

	// The default transport, so the process's own trust store applies -- the
	// image's public roots, plus the private CA when the chart mounts one and
	// lists its directory in SSL_CERT_DIR. There is no skip-verify here and
	// there must never be one: a probe that ignores the certificate would
	// report a healthy meter across a broken TLS path.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, describeProbeError(target, *timeout, err))
		return probeExitUnhealthy
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyPeek))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = fmt.Fprintf(stderr, "GET %s answered HTTP %d, not a 2xx. "+
			"THE ENDPOINT WAS REACHED: this is its own state, not a network, DNS or missing-dependency problem.\n",
			target, resp.StatusCode)
		return probeExitUnhealthy
	}
	_, _ = fmt.Fprintf(stdout, "GET %s answered HTTP %d\n", target, resp.StatusCode)
	return probeExitOK
}

// probeURLProblem returns a human sentence naming what is wrong with target,
// or "" when it is a URL this can GET. It is separate from runHTTPProbe so the
// refusal can be asserted without a network.
func probeURLProblem(target string) string {
	if target == "" {
		return "the URL is empty"
	}
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Sprintf("%q is not a parseable URL: %v", target, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return fmt.Sprintf("%q has no scheme; it must start with http:// or https://", target)
	default:
		return fmt.Sprintf("%q has scheme %q; only http and https can be probed", target, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Sprintf("%q has no host", target)
	}
	return ""
}

// describeProbeError turns a transport failure into a line that names the
// failure MODE, not just the Go error. These messages are the whole point of
// the subcommand: the doctor protocol has three exit codes and five distinct
// things that can go wrong, so the words have to carry it.
//
// Every branch states explicitly whether the endpoint was contacted, because
// gonk-hckm was a case of a message implying contact that never happened.
func describeProbeError(target string, timeout time.Duration, err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Sprintf("GET %s: CANNOT RESOLVE the host %q (%s). "+
			"No connection was attempted -- the name does not resolve from this process.",
			target, dnsErr.Name, dnsErr.Err)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Sprintf("GET %s: CONNECTION REFUSED. "+
			"The host resolved and was dialled, but nothing accepted on that port.", target)
	}
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &unknownCA) || errors.As(err, &hostnameErr) {
		return fmt.Sprintf("GET %s: TLS VERIFICATION FAILED: %v. "+
			"The endpoint answered; its certificate is not trusted by this process (the system roots, plus SSL_CERT_DIR).", target, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeoutErr(err) {
		return fmt.Sprintf("GET %s: NO RESPONSE within %s. "+
			"The request was sent; nothing came back in time.", target, timeout)
	}
	return fmt.Sprintf("GET %s: REQUEST FAILED: %v.", target, err)
}

func isTimeoutErr(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
