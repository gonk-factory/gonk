package harness

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// Fixed loopback ports. Under --network=host there is no port remapping to hide
// behind, so these are THE ports. Preflight refuses to run if any is occupied --
// a component silently talking to a leftover container from the last run is a
// debugging experience nobody should have.
const (
	PortStubModel = 8081
	PortLiteLLM   = 4000
	PortDolt      = 3307
	PortPostgres  = 5433
	PortMeter     = 9091
	PortIntake    = 9092
	PortGitLab    = 8929 // gitlab-ce, long-lived host container
	PortSkewProxy = 4001 // Date-rewriting proxy in front of LiteLLM (Task 5)
)

// AllFixedPorts is every port the harness binds under host networking. The
// doctor's port preflight (OD-5) checks all of them.
func AllFixedPorts() []int {
	return []int{
		PortStubModel, PortLiteLLM, PortDolt, PortPostgres,
		PortMeter, PortIntake, PortGitLab, PortSkewProxy,
	}
}

// CheckPortsFree returns an error naming EVERY occupied port, not just the first:
// a preflight that stops at the first collision makes you run it N times to find
// N leftovers. A port is "free" if we can bind 127.0.0.1:p and immediately
// release it.
func CheckPortsFree(ports ...int) error {
	var busy []int
	for _, p := range ports {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			busy = append(busy, p)
			continue
		}
		_ = ln.Close()
	}
	if len(busy) == 0 {
		return nil
	}
	sort.Ints(busy)
	parts := make([]string, len(busy))
	for i, p := range busy {
		parts[i] = strconv.Itoa(p)
	}
	return fmt.Errorf("harness: port(s) already in use: %s (stop the leftover container(s), e.g. `podman rm -f $(podman ps -aq)`, then retry)", strings.Join(parts, ", "))
}
