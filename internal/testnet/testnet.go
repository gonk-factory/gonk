//go:build unix

// Package testnet holds TEST-ONLY network helpers shared by several packages'
// tests. Nothing in a shipped binary imports it.
//
// It exists because "listen on :0, close, then treat the address as refused"
// is a flake (gonk-th5r, gonk-myvu, gonk-d2kb, gonk-uv82): the kernel hands a
// freed ephemeral port to the next binder -- another test's httptest server,
// another package's gate run on this box -- so the "refused" address can
// answer. This box's ephemeral range is only 4,096 ports, which makes it
// likely enough to reach the gate (docs/testing-time.md, rule 4).
//
// The first version of this package (gonk-d2kb) freed the port and then PROVED
// it refused by dialling it. That proof is true at the instant it is made and
// no longer: gonk-uv82 is another process binding the proven port before the
// script under test got round to probing it. A released port cannot be made
// safe by checking it harder. So nothing here releases a port any more.
package testnet

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"syscall"
	"testing"
)

// RefusedAddr returns a loopback "host:port" that REFUSES connections for the
// rest of the test, by construction rather than by observation:
//
//   - the port is BOUND, by a socket this process holds until the test's
//     cleanup. The kernel gives a bound TCP port to nobody else -- not to a
//     bind(0) looking for a free one, and not to an explicit bind of that
//     number (the holder sets neither SO_REUSEADDR nor SO_REUSEPORT, and
//     both sides must set one for a port to be shared);
//   - the socket never calls listen(2). A SYN to a bound port with no
//     listener is answered with RST -- ECONNREFUSED -- exactly as on a port
//     nobody has bound. (A LISTENING socket that never accepts would not do:
//     the kernel completes handshakes into its backlog, and with the backlog
//     full it drops SYNs, so the caller sees a hang, not a refusal.)
//
// So no neighbour can turn "refused" into "answered", however long the caller
// takes and however many processes it starts in between. This is rule 4's
// hold-then-check: the resource is held for the whole check.
//
// The address is only valid until the test (and its subtests) finish.
func RefusedAddr(t testing.TB) string {
	t.Helper()
	addr, release, err := hold()
	if err != nil {
		t.Fatalf("testnet: holding a refused loopback port: %v", err)
	}
	t.Cleanup(func() { _ = release() })
	return addr
}

// RefusedURL is "http://" + RefusedAddr(t).
func RefusedURL(t testing.TB) string {
	t.Helper()
	return "http://" + RefusedAddr(t)
}

// fallbackTries is how many explicit ports hold tries when the kernel has no
// ephemeral port to give.
const fallbackTries = 64

// hold binds a TCP socket to a loopback port WITHOUT listening on it, and
// returns the address and the function that lets go of it.
func hold() (addr string, release func() error, err error) {
	return holdWith(syscall.Bind, randomPort)
}

// randomPort is an unprivileged port from anywhere in the port space.
func randomPort() int { return 1024 + rand.Intn(65536-1024) }

// holdWith is hold with the bind call and the fallback port choice injected,
// so the fallback can be tested without exhausting a real port range.
//
// It asks for port 0 first: the kernel picks a port that is free NOW, and it
// stays ours. That can fail with EADDRINUSE while ports are plentiful: the
// holder sets no SO_REUSEADDR (it must not -- see RefusedAddr), so unlike an
// ordinary listener it cannot take an ephemeral port that still has a
// connection in TIME_WAIT on it, and a burst of loopback connections leaves
// one of those on every port of a small range for a minute. Measured under
// `make hunt CHURN=32` (512-port range): 110 of 1,000 runs could not get a
// port right after a package that had just made ~500 connections.
//
// So on EADDRINUSE it binds explicit ports instead, picked at random from the
// whole unprivileged space -- nearly all of it outside the ephemeral range,
// where no client connection ever leaves a TIME_WAIT. WHICH port is held does
// not matter; that it is held does, and every port this returns was bound by
// the same exclusive bind.
func holdWith(bind func(fd int, sa syscall.Sockaddr) error, pick func() int) (addr string, release func() error, err error) {
	// ForkLock + CloseOnExec: the callers run subprocesses (a shell script,
	// gonk-gate), and the holder must not leak into them.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return "", nil, fmt.Errorf("socket: %w", err)
	}
	loopback := [4]byte{127, 0, 0, 1}
	err = bind(fd, &syscall.SockaddrInet4{Addr: loopback})
	for i := 0; i < fallbackTries && errors.Is(err, syscall.EADDRINUSE); i++ {
		err = bind(fd, &syscall.SockaddrInet4{Addr: loopback, Port: pick()})
	}
	if err != nil {
		_ = syscall.Close(fd)
		return "", nil, fmt.Errorf("bind 127.0.0.1 (port 0, then up to %d explicit ports): %w", fallbackTries, err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return "", nil, fmt.Errorf("getsockname: %w", err)
	}
	in4, ok := sa.(*syscall.SockaddrInet4)
	if !ok || in4.Port == 0 {
		_ = syscall.Close(fd)
		return "", nil, fmt.Errorf("getsockname returned %#v, want a bound IPv4 address", sa)
	}
	ip := net.IPv4(in4.Addr[0], in4.Addr[1], in4.Addr[2], in4.Addr[3])
	return net.JoinHostPort(ip.String(), fmt.Sprint(in4.Port)), func() error { return syscall.Close(fd) }, nil
}
