package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/netprobe"
)

// runNetpolProbe is the `netpol-probe` subcommand: the chart's `helm test` hook
// runs this, in a pod carrying the agent's labels, to find out whether the
// cluster it is deployed to actually enforces the gonk-agent egress policy.
//
// IT LIVES IN THIS BINARY ON PURPOSE. The alternative was a shell image with
// nc, and there is no such image here: gonk-intake and gonk-meter are
// gcr.io/distroless/static-debian12:nonroot -- no shell, no nc, no curl -- and
// gonk-agent, which does have tooling, is the wrong image to hand a probe. A Go
// subcommand in the intake image inherits four things for free: a base that
// already satisfies PodSecurity `restricted` (runAsNonRoot, 65532,
// RuntimeDefault), typed connection errors instead of exit codes, air-gap
// support via the chart's existing image.registry and pullSecrets, and a pinned
// tag that `make no-latest` already polices.
//
// Addresses come from flags rather than being derived here, because the chart
// knows the release name and this binary does not.
func runNetpolProbe(args []string) int {
	fs := newProbeFlags()
	if err := fs.parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "gonk-intake netpol-probe:", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), fs.deadline)
	defer cancel()

	rep := netprobe.Run(ctx, netprobe.Config{
		Allow: netprobe.Leg{
			Name: "permitted-port", Addr: fs.allow, Expect: netprobe.Reachable,
			Why: "the agent egress policy names this pod on this port",
		},
		Deny: netprobe.Leg{
			Name: "forbidden-port", Addr: fs.deny, Expect: netprobe.Blocked,
			Why: "the SAME pod on a port the policy does not name -- only the port differs from the leg above",
		},
		DenyL3: netprobe.Leg{
			Name: "forbidden-pod", Addr: fs.denyL3, Expect: netprobe.Blocked,
			Why: "a pod the policy does not name at all",
		},
		DialTimeout:   fs.dialTimeout,
		SettleTimeout: fs.settleTimeout,
	})

	if err := netprobe.Write(os.Stdout, rep); err != nil {
		// The verdict is worthless if nobody can read it, so this is a failure
		// in its own right rather than something to log and carry on from.
		fmt.Fprintln(os.Stderr, "gonk-intake netpol-probe: writing the report:", err)
		return 2
	}
	return rep.ExitCode()
}

type probeFlags struct {
	allow, deny, denyL3                  string
	dialTimeout, settleTimeout, deadline time.Duration
	parse                                func([]string) error
}

func newProbeFlags() *probeFlags {
	p := &probeFlags{
		dialTimeout:   3 * time.Second,
		settleTimeout: 30 * time.Second,
		deadline:      2 * time.Minute,
	}
	p.parse = func(args []string) error {
		for i := 0; i < len(args); i++ {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %q needs a value", args[i])
			}
			v := args[i+1]
			var err error
			switch args[i] {
			case "-allow":
				p.allow = v
			case "-deny":
				p.deny = v
			case "-deny-l3":
				p.denyL3 = v
			case "-dial-timeout":
				p.dialTimeout, err = time.ParseDuration(v)
			case "-settle-timeout":
				p.settleTimeout, err = time.ParseDuration(v)
			case "-deadline":
				p.deadline, err = time.ParseDuration(v)
			default:
				return fmt.Errorf("unknown flag %q", args[i])
			}
			if err != nil {
				return fmt.Errorf("%s: %w", args[i], err)
			}
			i++
		}
		if p.allow == "" || p.deny == "" || p.denyL3 == "" {
			return fmt.Errorf("-allow, -deny and -deny-l3 are all required (host:port)")
		}
		return nil
	}
	return p
}
