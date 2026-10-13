package harness

import (
	"errors"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The verdict must be exactly the probe's verdict: nil iff every probe
// succeeded, otherwise an error naming every port whose probe failed and no
// port whose probe succeeded. The probe is injected so the test controls the
// state it asserts about -- a real port's freedom cannot be held still on a
// shared machine (gonk-th5r).
func TestCheckPortsFreeVerdictIsTheProbesVerdict(t *testing.T) {
	ports := []int{6303, 4101, 5202}
	cases := []struct {
		name     string
		busy     []int
		wantBusy []int // nil means CheckPortsFree must return nil
	}{
		{"all free", nil, nil},
		{"one busy among free ports", []int{4101}, []int{4101}},
		{"first and last busy", []int{6303, 5202}, []int{5202, 6303}},
		{"all busy", []int{6303, 4101, 5202}, []int{4101, 5202, 6303}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			busy := map[int]bool{}
			for _, p := range tc.busy {
				busy[p] = true
			}
			var probed []int
			probe := func(p int) error {
				probed = append(probed, p)
				if busy[p] {
					return &fakeBindErr{p}
				}
				return nil
			}

			err := checkPortsFree(probe, ports...)

			// Every port is probed, whatever the earlier verdicts were.
			if !reflect.DeepEqual(probed, ports) {
				t.Fatalf("probed %v, want every port in order %v", probed, ports)
			}
			if tc.wantBusy == nil {
				if err != nil {
					t.Fatalf("every probe succeeded, but CheckPortsFree = %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("probe reported %v busy, but CheckPortsFree = nil", tc.busy)
			}
			if got := namedPorts(t, err.Error()); !reflect.DeepEqual(got, tc.wantBusy) {
				t.Fatalf("error names ports %v, want exactly %v (every busy port, no free one): %v", got, tc.wantBusy, err)
			}
		})
	}
}

type fakeBindErr struct{ port int }

func (e *fakeBindErr) Error() string { return "fake: address in use: " + strconv.Itoa(e.port) }

// namedPorts extracts the port list from CheckPortsFree's error, which reads
// "harness: port(s) already in use: A, B, C (stop the leftover ...".
func namedPorts(t *testing.T, msg string) []int {
	t.Helper()
	const lead = "already in use: "
	i := strings.Index(msg, lead)
	if i < 0 {
		t.Fatalf("error does not contain %q: %s", lead, msg)
	}
	rest := msg[i+len(lead):]
	if j := strings.Index(rest, " ("); j >= 0 {
		rest = rest[:j]
	}
	var out []int
	for _, f := range strings.Split(rest, ", ") {
		p, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			t.Fatalf("port list %q has a non-port %q: %s", rest, f, msg)
		}
		out = append(out, p)
	}
	if !sort.IntsAreSorted(out) {
		t.Fatalf("ports %v are not sorted: %s", out, msg)
	}
	return out
}

// The REAL probe, in the free direction: asked for port 0 -- "any port the
// kernel can give" -- it must report free. This is the one test that fails if
// bindLoopback is broken so that it reports busy for a port that can be bound.
//
// ONE observation, not two (gonk-njmr; docs/testing-time.md rule 4). The only
// honest reason for the probe to report port 0 busy is that the kernel has no
// ephemeral port to give, and that reason is IN the error the probe returns:
// EADDRINUSE. So the errno the probe itself saw decides the verdict, and no
// second bind is made to "confirm" it -- a second bind is a second look at a
// shared resource, and gonk-th5r is what inferring from one costs. A full
// range is the host's state (4,096 ports on the box this was written on, and
// every other worktree's tests draw on them); it is logged and is not a
// failure. Any other error is.
//
// This replaces TestCheckPortsFreeReportsABindablePortFree, which asserted
// CheckPortsFree(0) == nil outright and so was red whenever the range was
// full. What is given up: in a run where the range IS full this test observes
// nothing about the free direction, and a probe broken so that it returns
// EADDRINUSE for every port would pass it. The verdict logic itself does not
// depend on the kernel at all (TestCheckPortsFreeVerdictIsTheProbesVerdict).
func TestTheRealProbeReportsAKernelChosenPortFreeUnlessTheKernelHasNone(t *testing.T) {
	err := bindLoopback(0)
	if msg := realProbeVerdict(err); msg != "" {
		t.Fatal(msg)
	}
	if err != nil {
		t.Logf("the kernel had no ephemeral loopback port to give (%v): the host's range is full, so the free direction was not observed in this run", err)
	}
}

// realProbeVerdict is the judgement the test above makes of what the real
// probe returned for port 0: "" for free, "" for "the kernel had no port"
// (EADDRINUSE), and a failure message for anything else.
func realProbeVerdict(err error) string {
	if err == nil || errors.Is(err, syscall.EADDRINUSE) {
		return ""
	}
	return "bindLoopback(0) = " + err.Error() + "; port 0 asks the kernel for any free loopback port, and the only error that is the host's doing is EADDRINUSE (no ephemeral port to give), so the real probe is broken"
}

// The judgement above, pinned: free and a full range pass, anything else is
// named as the probe's fault.
func TestRealProbeVerdictBlamesTheProbeOnlyForAnErrnoThatIsNotAFullRange(t *testing.T) {
	full := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	if msg := realProbeVerdict(nil); msg != "" {
		t.Errorf("a bind that succeeded was judged a failure: %s", msg)
	}
	if msg := realProbeVerdict(full); msg != "" {
		t.Errorf("EADDRINUSE from port 0 (the host has no ephemeral port) was judged the probe's failure: %s", msg)
	}
	for _, err := range []error{syscall.EACCES, errors.New("always busy")} {
		if msg := realProbeVerdict(err); !strings.Contains(msg, "the real probe is broken") || !strings.Contains(msg, err.Error()) {
			t.Errorf("realProbeVerdict(%v) = %q; want a failure that names the error and blames the probe", err, msg)
		}
	}
}
