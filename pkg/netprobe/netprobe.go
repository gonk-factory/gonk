// Package netprobe answers one question about a running cluster: does it
// actually ENFORCE the NetworkPolicies gonk's chart ships?
//
// That is a property of the CLUSTER, not of the chart, and no amount of CI can
// settle it for somebody else's cluster. The API server accepts NetworkPolicy
// objects whether or not any controller implements them -- which is exactly how
// gonk ran for months with six policies that blocked nothing (gonk-dku). So the
// chart ships the instrument alongside the claim, and a reader does not have to
// trust the README.
//
// WHAT THIS DOES NOT DO. It does not prove the policies are CORRECT. It probes
// egress from ONE policy, the agent's, and a CNI that enforces egress but not
// ingress passes. Say "the gonk-agent egress policy is enforced on this
// cluster", never "this cluster enforces NetworkPolicy".
package netprobe

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"
)

// Expectation is what the policy says should happen to a leg.
type Expectation int

const (
	// Reachable: the policy explicitly permits this destination.
	Reachable Expectation = iota
	// Blocked: the policy does not permit it, so an enforcing cluster refuses.
	Blocked
)

func (e Expectation) String() string {
	if e == Reachable {
		return "reachable"
	}
	return "blocked"
}

// Leg is one destination and what the policy says about it.
type Leg struct {
	Name   string
	Addr   string // host:port -- a bare TCP connect target, never a URL
	Expect Expectation
	// Why names the policy rule, so a failure report explains itself without
	// the reader having to go and read the NetworkPolicy.
	Why string
}

// Result is one leg, measured.
type Result struct {
	Leg       Leg
	Connected bool
	Err       error
	Took      time.Duration
}

// Match reports whether the measurement agrees with the policy.
func (r Result) Match() bool { return r.Connected == (r.Leg.Expect == Reachable) }

// Verdict is the answer, and "inconclusive" is a first-class one.
type Verdict string

const (
	// Enforced: the allow leg connected and the denied legs did not.
	Enforced Verdict = "ENFORCED"
	// NotEnforced: a destination the policy forbids was reachable anyway.
	NotEnforced Verdict = "NOT ENFORCED"
	// PortsIgnored: L3 is enforced but `ports:` is not -- a destination pod the
	// policy names is reachable on a port it does not.
	PortsIgnored Verdict = "PARTIALLY ENFORCED"
	// Inconclusive: the probe could not establish the premise its answer rests
	// on. NEVER report this as either of the others.
	Inconclusive Verdict = "INCONCLUSIVE"
)

// Dialer is net.DialTimeout, injectable so tests need no sockets.
type Dialer func(network, addr string, timeout time.Duration) (net.Conn, error)

// Config is one probe run.
type Config struct {
	// Allow is a destination the policy PERMITS. It is the control: if this
	// cannot be reached, nothing can be concluded from the others, because a
	// pod with broken networking fails every leg and looks exactly like a pod
	// behind a perfect policy.
	Allow Leg
	// Deny is a destination the policy FORBIDS. Point it at the SAME POD as
	// Allow on a different port: same node, same route, same DNS zone, so the
	// only thing that differs between the two legs is the port the policy
	// names. Legs that differ in namespace, subnet or NAT path prove nothing.
	Deny Leg
	// DenyL3 is a destination the policy does not mention at all. It separates
	// a CNI that enforces L3 but ignores `ports:` from one that enforces
	// neither -- without it those two are indistinguishable.
	DenyL3 Leg

	// DialTimeout bounds one connect attempt.
	DialTimeout time.Duration
	// SettleTimeout bounds how long to wait for the allow leg before giving up.
	// Policy programming for a brand-new pod is not instantaneous on any
	// iptables-based controller, and a probe that races it reports a false
	// NOT ENFORCED.
	SettleTimeout time.Duration
	// RetryInterval paces the settle loop.
	RetryInterval time.Duration

	Dial Dialer
	Now  func() time.Time
}

func (c *Config) defaults() {
	if c.DialTimeout <= 0 {
		c.DialTimeout = 3 * time.Second
	}
	if c.SettleTimeout <= 0 {
		c.SettleTimeout = 30 * time.Second
	}
	if c.RetryInterval <= 0 {
		c.RetryInterval = 500 * time.Millisecond
	}
	if c.Dial == nil {
		c.Dial = net.DialTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Report is the whole outcome.
type Report struct {
	Verdict Verdict
	// Reason is one sentence naming the likely cause, for an operator who does
	// not already know what a CNI is.
	Reason  string
	Results []Result
}

// Run performs the probe. The order is deliberate: the allow leg first (to
// establish the premise and to let policy programming settle), then the denied
// legs, then the allow leg AGAIN -- because if connectivity died midway, the
// denied legs' failures mean nothing and the honest answer is inconclusive.
func Run(ctx context.Context, c Config) Report {
	c.defaults()

	pre, ok := settle(ctx, c, c.Allow)
	rep := Report{Results: []Result{pre}}
	if !ok {
		rep.Verdict = Inconclusive
		rep.Reason = fmt.Sprintf(
			"could not reach %s (%s), which the policy PERMITS, within %s. Nothing can be "+
				"concluded about the denied destinations: a pod that cannot reach what it is "+
				"allowed to reach fails every leg and looks identical to one behind a perfect "+
				"policy. Check that the destination is running.",
			c.Allow.Addr, c.Allow.Name, c.SettleTimeout)
		return rep
	}

	deny := attempt(ctx, c, c.Deny)
	l3 := attempt(ctx, c, c.DenyL3)
	post := attempt(ctx, c, c.Allow)
	rep.Results = append(rep.Results, deny, l3, post)

	if !post.Connected {
		rep.Verdict = Inconclusive
		rep.Reason = fmt.Sprintf(
			"%s (%s) was reachable at the start of the probe and not at the end, so connectivity "+
				"changed midway and the denied legs prove nothing. Re-run it.",
			c.Allow.Addr, c.Allow.Name)
		return rep
	}

	switch {
	case deny.Connected && l3.Connected:
		rep.Verdict = NotEnforced
		rep.Reason = "both denied destinations were reachable. Nothing is enforcing NetworkPolicy on " +
			"this cluster -- the objects exist and the API server accepted them, but no controller " +
			"implements them. Flannel alone does this; you need Calico, Cilium, kube-router or similar."
	case deny.Connected && !l3.Connected:
		rep.Verdict = PortsIgnored
		rep.Reason = fmt.Sprintf(
			"%s was blocked but %s was reachable, so this CNI enforces WHICH PODS may be reached "+
				"and ignores WHICH PORTS. The agent is confined to the right pods on any port they "+
				"listen on.", c.DenyL3.Addr, c.Deny.Addr)
	case !deny.Connected && l3.Connected:
		rep.Verdict = Inconclusive
		rep.Reason = fmt.Sprintf(
			"%s was blocked but %s -- a destination the policy does not mention at all -- was "+
				"reachable. That combination is incoherent for a policy engine; suspect the "+
				"destination rather than the CNI.", c.Deny.Addr, c.DenyL3.Addr)
	default:
		rep.Verdict = Enforced
		rep.Reason = "the permitted destination was reachable and both forbidden ones were not, " +
			"so the gonk-agent egress policy is enforced on this cluster."
	}
	return rep
}

// settle retries the allow leg until it succeeds or SettleTimeout expires.
func settle(ctx context.Context, c Config, leg Leg) (Result, bool) {
	deadline := c.Now().Add(c.SettleTimeout)
	var last Result
	for {
		last = attempt(ctx, c, leg)
		if last.Connected {
			return last, true
		}
		if c.Now().After(deadline) || ctx.Err() != nil {
			return last, false
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-time.After(c.RetryInterval):
		}
	}
}

// attempt opens a bare TCP connection and closes it.
//
// A CONNECT, NEVER A REQUEST. gonk-intake's private listener serves
// POST /admin/reconcile UNAUTHENTICATED; a probe that spoke HTTP to it could
// trigger real work as a side effect of asking a question about the network.
func attempt(ctx context.Context, c Config, leg Leg) Result {
	start := c.Now()
	conn, err := c.Dial("tcp", leg.Addr, c.DialTimeout)
	r := Result{Leg: leg, Err: err, Took: c.Now().Sub(start)}
	if err == nil {
		r.Connected = true
		_ = conn.Close()
	}
	return r
}

// errWriter collects the first write error so Write can report the whole thing
// once instead of checking every line. A probe whose output failed to reach the
// log has not told anybody anything, so the error is returned rather than
// dropped.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, a ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, a...)
}

// Write renders the report. The failure modes are reported as DIAGNOSTIC TEXT
// and never as the verdict: a denial presents as a timeout on Calico and Cilium
// (silent drop) but as connection-refused on kube-router, which k3s bundles, so
// keying the answer on the errno would misread a correct k3s denial as a
// missing target. The measurement is differential; the errno only helps an
// operator name their CNI.
func Write(w io.Writer, rep Report) error {
	e := &errWriter{w: w}
	e.printf("gonk NetworkPolicy enforcement probe\n\n")
	for _, r := range rep.Results {
		state := "BLOCKED"
		if r.Connected {
			state = "reached"
		}
		mark := "ok "
		if !r.Match() {
			mark = "!! "
		}
		e.printf("  %s%-22s %-34s expected %-9s got %-8s (%s)\n",
			mark, r.Leg.Name, r.Leg.Addr, r.Leg.Expect, state, r.Took.Round(time.Millisecond))
		e.printf("      %s\n", r.Leg.Why)
		if r.Err != nil {
			e.printf("      detail: %v\n", r.Err)
		}
	}
	e.printf("\n  VERDICT: %s\n  %s\n", rep.Verdict, rep.Reason)
	if rep.Verdict == Enforced {
		e.printf("\n  Note: this tested EGRESS from ONE policy. A cluster that enforces egress\n" +
			"  but not ingress passes this probe.\n")
	}
	return e.err
}

// ExitCode is 0 only for Enforced. Anything else fails `helm test`, which is
// the point: on a cluster that does not enforce, this SHOULD be red.
func (r Report) ExitCode() int {
	if r.Verdict == Enforced {
		return 0
	}
	return 1
}
