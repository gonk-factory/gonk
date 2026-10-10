//go:build unix

package testnet

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
)

// inUse is the error net.Listen returns when the kernel has no port to give:
// EADDRINUSE under an *net.OpError and an *os.SyscallError.
func inUse(network, address string) error {
	return &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
}

// gonk-ltep. The kernel has no ephemeral IPv4 port: httptest.NewServer would
// now listen on [::1]. Listen asks for explicit IPv4 loopback ports instead,
// and every address it ever asks for is 127.0.0.1's.
func TestListenWithNoEphemeralPortStaysOnIPv4Loopback(t *testing.T) {
	var asked []string
	listen := func(network, address string) (net.Listener, error) {
		asked = append(asked, network+" "+address)
		if len(asked) <= 3 { // port 0, then two explicit ports somebody holds
			return nil, inUse(network, address)
		}
		return net.Listen("tcp4", "127.0.0.1:0") // any real listener: what was ASKED is asserted
	}
	ports := []int{40001, 40002, 40003}
	pick := func() int { p := ports[0]; ports = ports[1:]; return p }
	l, err := listenWith(listen, pick)
	if err != nil {
		t.Fatalf("listenWith = %v, want a listener from the third explicit port", err)
	}
	defer func() { _ = l.Close() }()
	want := []string{"tcp4 127.0.0.1:0", "tcp4 127.0.0.1:40001", "tcp4 127.0.0.1:40002", "tcp4 127.0.0.1:40003"}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Fatalf("listen was asked for %v, want %v: port 0 first, then explicit ports, IPv4 loopback every time", asked, want)
	}
}

// It gives up with the errno rather than looping, and an error that is not
// "in use" is not retried at all.
func TestListenGivesUpWithTheErrno(t *testing.T) {
	calls := 0
	never := func(network, address string) (net.Listener, error) { calls++; return nil, inUse(network, address) }
	if l, err := listenWith(never, randomPort); err == nil || l != nil || !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("listenWith = %v, %v; want no listener and EADDRINUSE", l, err)
	}
	if calls != 1+fallbackTries {
		t.Fatalf("listen was tried %d times, want %d (port 0, then %d explicit ports)", calls, 1+fallbackTries, fallbackTries)
	}
	calls = 0
	denied := func(string, string) (net.Listener, error) { calls++; return nil, syscall.EACCES }
	if _, err := listenWith(denied, randomPort); !errors.Is(err, syscall.EACCES) || calls != 1 {
		t.Fatalf("listenWith = %v after %d listen(s); want EACCES after exactly 1", err, calls)
	}
}

// Against the real kernel, in the one direction this test controls: the
// listener it is handed is on 127.0.0.1 and it answers.
func TestNewServerIsOnIPv4LoopbackAndServes(t *testing.T) {
	srv := NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "here") }))
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Fatalf("NewServer's URL = %q, want http://127.0.0.1:PORT", srv.URL)
	}
	if a, ok := srv.Listener.Addr().(*net.TCPAddr); !ok || a.IP.To4() == nil || !a.IP.IsLoopback() {
		t.Fatalf("NewServer listens on %v, want the IPv4 loopback", srv.Listener.Addr())
	}
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "here" {
		t.Fatalf("the server answered %q, want its handler's reply", body)
	}
}
