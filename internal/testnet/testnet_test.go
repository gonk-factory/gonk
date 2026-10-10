//go:build unix

package testnet

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dial is a hang guard around one connection attempt (rule 3): the bound is
// far above anything a loopback RST takes, and is not what the tests assert.
func dial(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 30*time.Second)
	if err != nil {
		return err
	}
	_ = c.Close()
	return nil
}

// The returned address must refuse -- ECONNREFUSED, not a hang and not an
// accept. A holder that LISTENED without accepting would pass a weaker "the
// dial did not succeed" check on some kernels and hang on others; the callers
// assert on the words "CONNECTION REFUSED", so the errno is the contract.
func TestRefusedAddrRefusesWithECONNREFUSED(t *testing.T) {
	for i := 0; i < 4; i++ {
		addr := RefusedAddr(t)
		err := dial(addr)
		if err == nil {
			t.Fatalf("RefusedAddr returned %s but it accepted a connection", addr)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("dialling %s failed with %v, want ECONNREFUSED", addr, err)
		}
	}
}

// THE PROPERTY gonk-uv82 NEEDED: while the test runs, nobody else can bind the
// address, so nobody else can answer on it. Go's net.Listen sets SO_REUSEADDR,
// which is the most permissive ordinary binder there is (it is what every
// httptest server on this box uses).
//
// This is hold-then-check (docs/testing-time.md rule 4): the port is ours for
// the whole assertion, so no other process can change the answer.
func TestRefusedAddrCannotBeBoundByAnyoneElseWhileHeld(t *testing.T) {
	addr := RefusedAddr(t)

	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
		t.Fatalf("a second socket bound and listened on %s while RefusedAddr held it: "+
			"a neighbour could answer on the 'refused' address (gonk-uv82)", addr)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("binding the held address %s failed with %v, want EADDRINUSE", addr, err)
	}
	// And the failed attempt changed nothing: it still refuses.
	if err := dial(addr); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("after a rival's failed bind, dialling %s gave %v, want ECONNREFUSED", addr, err)
	}
}

// The kernel must not hand a held port to a bind(0) either: not to a second
// RefusedAddr, and not to a listener asking for any free port. Hold-then-check
// again -- every address is still held while the listeners are opened.
//
// Only 8 of each: a bind costs ~11 ms on this box (docs/testing-time.md), and
// with 64 this one test took 1.4 s in the gate and timed a 500-run hunt out.
func TestRefusedAddrsAreDistinctAndNeverHandedToAListener(t *testing.T) {
	held := map[string]bool{}
	for i := 0; i < 8; i++ {
		a := RefusedAddr(t)
		if held[a] {
			t.Fatalf("RefusedAddr returned %s twice while the first was still held", a)
		}
		held[a] = true
	}
	for i := 0; i < 8; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		collided := held[ln.Addr().String()]
		if collided {
			t.Errorf("the kernel handed the held address %s to a new listener", ln.Addr())
		}
		_ = ln.Close()
	}
}

// The hold ends with the test, not before and not never: release succeeds
// exactly once (a second close of the same descriptor is EBADF).
func TestHoldReleasesItsSocket(t *testing.T) {
	_, release, err := hold()
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// With the ephemeral range unusable (every port 0 bind says EADDRINUSE, as it
// does when each port has a connection in TIME_WAIT), hold must still come
// back holding a port -- an explicit one -- and that port must have exactly
// the properties of any other: refuses, and cannot be bound by a rival.
func TestHoldFallsBackToAnExplicitPortWhenTheKernelHasNoneToGive(t *testing.T) {
	var asked []int
	bind := func(fd int, sa syscall.Sockaddr) error {
		port := sa.(*syscall.SockaddrInet4).Port
		asked = append(asked, port)
		if len(asked) <= 3 { // port 0, then two explicit ports "taken"
			return syscall.EADDRINUSE
		}
		return syscall.Bind(fd, sa)
	}
	// The fallback picks at random and the test accepts whichever port it
	// really bound: handing it a "known free" port would mean freeing one.
	addr, release, err := holdWith(bind, randomPort)
	if err != nil {
		t.Fatalf("holdWith: %v (asked for ports %v)", err, asked)
	}
	defer func() { _ = release() }()

	if len(asked) < 4 || asked[0] != 0 {
		t.Fatalf("bind was asked for ports %v, want port 0 first and then explicit ports until one binds", asked)
	}
	for _, p := range asked[1:] {
		if p < 1024 || p > 65535 {
			t.Fatalf("the fallback asked for port %d, want an unprivileged port", p)
		}
	}
	_, port, _ := net.SplitHostPort(addr)
	if got := asked[len(asked)-1]; port != strconv.Itoa(got) {
		t.Fatalf("hold returned %s, but the port it last bound was %d", addr, got)
	}
	if err := dial(addr); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dialling the fallback address %s gave %v, want ECONNREFUSED", addr, err)
	}
	if ln, err := net.Listen("tcp", addr); err == nil {
		_ = ln.Close()
		t.Fatalf("a rival bound and listened on the fallback address %s while it was held", addr)
	}
}

// It gives up, with the errno, rather than looping or returning an address it
// does not hold; and an error that is not "in use" is not retried at all.
func TestHoldGivesUpAndNeverReturnsAnAddressItDoesNotHold(t *testing.T) {
	calls := 0
	inUse := func(int, syscall.Sockaddr) error { calls++; return syscall.EADDRINUSE }
	addr, _, err := holdWith(inUse, randomPort)
	if err == nil || addr != "" {
		t.Fatalf("holdWith = %q, %v; want no address and an error when nothing can be bound", addr, err)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("error %v does not carry EADDRINUSE", err)
	}
	if calls != 1+fallbackTries {
		t.Fatalf("bind was tried %d times, want %d (port 0, then %d explicit ports)", calls, 1+fallbackTries, fallbackTries)
	}

	calls = 0
	denied := func(int, syscall.Sockaddr) error { calls++; return syscall.EACCES }
	if _, _, err := holdWith(denied, randomPort); !errors.Is(err, syscall.EACCES) || calls != 1 {
		t.Fatalf("holdWith = %v after %d bind(s); want EACCES after exactly 1 -- only EADDRINUSE is worth another port", err, calls)
	}
}

func TestRefusedURLIsAnHTTPURLOfARefusedAddr(t *testing.T) {
	u := RefusedURL(t)
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Fatalf("RefusedURL = %q, want http://127.0.0.1:PORT", u)
	}
	if err := dial(strings.TrimPrefix(u, "http://")); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dialling %s gave %v, want ECONNREFUSED", u, err)
	}
}
