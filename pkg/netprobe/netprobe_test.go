package netprobe

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeNet answers each address with a fixed outcome, and counts attempts.
type fakeNet struct {
	reach map[string]bool
	calls map[string]int
	// flip makes an address change answer after n attempts, for the settle and
	// mid-probe-failure cases.
	flip  map[string]int
	after map[string]bool
}

func (f *fakeNet) dial(_, addr string, _ time.Duration) (net.Conn, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[addr]++
	ok := f.reach[addr]
	if n, has := f.flip[addr]; has && f.calls[addr] > n {
		ok = f.after[addr]
	}
	if !ok {
		return nil, errors.New("i/o timeout")
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

const (
	allowAddr = "gonk-intake-internal:9090"
	denyAddr  = "gonk-intake:8080"
	l3Addr    = "gonk-controller:9443"
)

func cfg(f *fakeNet) Config {
	return Config{
		Allow:         Leg{Name: "allowed", Addr: allowAddr, Expect: Reachable, Why: "policy permits intake:9090"},
		Deny:          Leg{Name: "denied-port", Addr: denyAddr, Expect: Blocked, Why: "same pod, port not permitted"},
		DenyL3:        Leg{Name: "denied-pod", Addr: l3Addr, Expect: Blocked, Why: "pod not named by the policy"},
		DialTimeout:   time.Millisecond,
		SettleTimeout: 200 * time.Millisecond,
		RetryInterval: time.Millisecond,
		Dial:          f.dial,
	}
}

func TestVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reach map[string]bool
		want  Verdict
		says  string
	}{
		{
			name:  "enforced",
			reach: map[string]bool{allowAddr: true},
			want:  Enforced,
			says:  "is enforced on this cluster",
		},
		{
			// orac today: flannel, no policy controller.
			name:  "nothing enforces",
			reach: map[string]bool{allowAddr: true, denyAddr: true, l3Addr: true},
			want:  NotEnforced,
			says:  "no controller",
		},
		{
			// A CNI that honours podSelector but ignores ports:.
			name:  "ports ignored",
			reach: map[string]bool{allowAddr: true, denyAddr: true},
			want:  PortsIgnored,
			says:  "ignores WHICH PORTS",
		},
		{
			// The premise fails: the pod cannot reach what it IS allowed to.
			// Reporting NOT ENFORCED here would be the probe's worst bug.
			name:  "allow leg never comes up",
			reach: map[string]bool{},
			want:  Inconclusive,
			says:  "which the policy PERMITS",
		},
		{
			name:  "incoherent: port blocked but unnamed pod reachable",
			reach: map[string]bool{allowAddr: true, l3Addr: true},
			want:  Inconclusive,
			says:  "incoherent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Run(context.Background(), cfg(&fakeNet{reach: tc.reach}))
			if rep.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (%s)", rep.Verdict, tc.want, rep.Reason)
			}
			if !strings.Contains(rep.Reason, tc.says) {
				t.Errorf("reason does not explain itself; wanted %q in:\n  %s", tc.says, rep.Reason)
			}
		})
	}
}

// The allow leg is retried, because policy programming for a brand-new pod is
// not instantaneous on any iptables-based controller and a probe that races it
// reports a false NOT ENFORCED.
func TestAllowLegIsRetriedUntilItSettles(t *testing.T) {
	f := &fakeNet{
		reach: map[string]bool{},
		flip:  map[string]int{allowAddr: 2},
		after: map[string]bool{allowAddr: true},
	}
	rep := Run(context.Background(), cfg(f))
	if rep.Verdict != Enforced {
		t.Fatalf("verdict = %q (%s); a slow-to-program policy must not read as unenforced", rep.Verdict, rep.Reason)
	}
	if f.calls[allowAddr] < 3 {
		t.Errorf("allow leg attempted %d times, want at least 3 (2 failures then success)", f.calls[allowAddr])
	}
}

// If connectivity dies midway, the denied legs' failures are meaningless.
func TestConnectivityLostMidProbeIsInconclusive(t *testing.T) {
	f := &fakeNet{
		reach: map[string]bool{allowAddr: true},
		flip:  map[string]int{allowAddr: 1}, // works once, then stops
		after: map[string]bool{allowAddr: false},
	}
	rep := Run(context.Background(), cfg(f))
	if rep.Verdict != Inconclusive {
		t.Fatalf("verdict = %q, want INCONCLUSIVE: connectivity changed midway", rep.Verdict)
	}
	if !strings.Contains(rep.Reason, "changed midway") {
		t.Errorf("reason = %q", rep.Reason)
	}
}

// The errno must never decide the verdict. kube-router (k3s) REJECTs a denied
// connection, surfacing as ECONNREFUSED; Calico and Cilium drop it, surfacing
// as a timeout. Keying on that would misread a correct k3s denial.
func TestTheVerdictIgnoresHowTheConnectionFailed(t *testing.T) {
	var got []Verdict
	for _, errText := range []string{"i/o timeout", "connection refused", "no such host", "network is unreachable"} {
		f := &fakeNet{reach: map[string]bool{allowAddr: true}}
		c := cfg(f)
		base := c.Dial
		c.Dial = func(n, addr string, d time.Duration) (net.Conn, error) {
			conn, err := base(n, addr, d)
			if err != nil {
				return nil, errors.New(errText)
			}
			return conn, nil
		}
		got = append(got, Run(context.Background(), c).Verdict)
	}
	for i, v := range got {
		if v != Enforced {
			t.Errorf("failure mode %d produced verdict %q; the measurement is differential, "+
				"the errno is diagnostic only", i, v)
		}
	}
}

func TestExitCodeIsZeroOnlyWhenEnforced(t *testing.T) {
	for _, v := range []Verdict{NotEnforced, PortsIgnored, Inconclusive} {
		if (Report{Verdict: v}).ExitCode() == 0 {
			t.Errorf("%q exits 0; helm test must go red on anything but ENFORCED", v)
		}
	}
	if (Report{Verdict: Enforced}).ExitCode() != 0 {
		t.Error("ENFORCED must exit 0")
	}
}

// The report has to be readable by somebody who does not already know what a
// CNI is -- that is the whole point of shipping it to other people's clusters.
func TestReportNamesEveryLegAndTheVerdict(t *testing.T) {
	f := &fakeNet{reach: map[string]bool{allowAddr: true, denyAddr: true, l3Addr: true}}
	rep := Run(context.Background(), cfg(f))
	var sb strings.Builder
	if err := Write(&sb, rep); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := sb.String()
	for _, want := range []string{allowAddr, denyAddr, l3Addr, "VERDICT", string(NotEnforced), "Flannel"} {
		if !strings.Contains(out, want) {
			t.Errorf("report omits %q:\n%s", want, out)
		}
	}
	// A mismatched leg must be visibly marked, not buried in prose.
	if !strings.Contains(out, "!!") {
		t.Errorf("a leg that contradicted the policy was not flagged:\n%s", out)
	}
}
