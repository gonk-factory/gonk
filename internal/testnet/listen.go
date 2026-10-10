//go:build unix

package testnet

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
)

// NewServer is httptest.NewServer on Listen's listener: a started test server
// whose URL is ALWAYS "http://127.0.0.1:<port>", closed when the test ends.
//
// Use it for any fake whose URL the code under test looks AT, rather than
// only dials (gonk-ltep). httptest.NewServer does not promise an address
// family: it listens on 127.0.0.1:0 and, when that fails, SILENTLY listens on
// [::1]:0 instead, so the fake's URL becomes "http://[::1]:<port>". The bind
// of 127.0.0.1:0 fails -- EADDRINUSE, with nothing wrong in the test -- when
// every port of the ephemeral range has a connection in TIME_WAIT on it, and
// on this box that range is 4,096 ports and one neighbouring `go test` is
// enough to fill it (docs/testing-time.md, rule 4). Code that treats a
// bracketed host differently then behaves differently for as long as the
// range stays full: gonk-gate's pipeline notes link a pipeline only when the
// project's web URL is made of a conservative alphabet, which has no "[", so
// two tests asserting on those links went red together and green again a
// moment later.
func NewServer(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	srv := &httptest.Server{Listener: Listen(t), Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// Listen returns a TCP listener on the IPv4 loopback, 127.0.0.1, and never on
// any other address: the family is the test's choice, not a function of which
// ports the kernel had to spare. The caller closes it (httptest.Server.Close
// does).
//
// It asks for port 0 first. When the kernel has no ephemeral port to give
// (EADDRINUSE; see NewServer) it listens on explicit ports instead, picked at
// random from the whole unprivileged space -- nearly all of it outside the
// ephemeral range, where no client connection ever leaves a TIME_WAIT -- the
// same fallback RefusedAddr uses. A port it returns is held by this listener
// for as long as the listener is open, so this is rule 4's direction: nothing
// is freed and then trusted.
func Listen(t testing.TB) net.Listener {
	t.Helper()
	l, err := listenWith(net.Listen, randomPort)
	if err != nil {
		t.Fatalf("testnet: %v", err)
	}
	return l
}

// listenWith is Listen with the listen call and the fallback port choice
// injected, so the fallback can be tested without exhausting a real range.
// Only EADDRINUSE is worth another port; any other error is returned at once.
func listenWith(listen func(network, address string) (net.Listener, error), pick func() int) (net.Listener, error) {
	l, err := listen("tcp4", "127.0.0.1:0")
	for i := 0; i < fallbackTries && errors.Is(err, syscall.EADDRINUSE); i++ {
		l, err = listen("tcp4", fmt.Sprintf("127.0.0.1:%d", pick()))
	}
	if err != nil {
		return nil, fmt.Errorf("listen on 127.0.0.1 (port 0, then up to %d explicit ports): %w", fallbackTries, err)
	}
	return l, nil
}
