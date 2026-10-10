// Command portchurn is the NEIGHBOUR for `make hunt CHURN=N` (gonk-uv82): it
// keeps N loopback listeners bound to kernel-chosen ephemeral ports, each
// answering every HTTP request with 204, and replaces each one every HOLD or
// so. That is what another package's httptest servers -- or another agent's
// whole gate run on this box -- look like to a test that picked a "free" port
// and expects nobody to be there: a freed port is handed to the next binder.
//
// CPU burners (the rest of the hunt's load) cannot reproduce that class at
// all, which is why three port flakes in a row (gonk-th5r, gonk-myvu,
// gonk-d2kb) were fixed without ever being seen red on demand. This can.
//
// hack/flake_hunt.sh builds and starts it, and ONLY inside a private network
// namespace: churning the box's real loopback would hand these listeners to
// every other gate running here and make THEIR tests flake. Keep N small and
// narrow the namespace's port range instead (the hunt does): bind() on this
// box costs ~11 ms, so a thousand rotating listeners mostly queue in bind().
//
// It exits when its parent does, like the hunt's burners.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	ports := flag.Int("ports", 32, "listeners to keep bound at any moment")
	hold := flag.Duration("hold", time.Second, "mean lifetime of one listener before it is replaced")
	flag.Parse()
	if *ports < 1 || *hold <= 0 {
		fmt.Fprintln(os.Stderr, "portchurn: -ports must be >= 1 and -hold > 0")
		os.Exit(2)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for i := 0; i < *ports; i++ {
		go churn(handler, *hold)
	}

	parent := os.Getppid()
	for os.Getppid() == parent {
		time.Sleep(200 * time.Millisecond)
	}
}

// churn holds one listener at a time, forever: bind a kernel-chosen port,
// serve on it for 0.5x-1.5x hold, close it, bind another.
func churn(h http.Handler, hold time.Duration) {
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			// The range is exhausted or the namespace is going away; back off
			// rather than spin.
			time.Sleep(hold)
			continue
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		time.Sleep(hold/2 + time.Duration(rand.Int63n(int64(hold))))
		_ = srv.Close()
	}
}
