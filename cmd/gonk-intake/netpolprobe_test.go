package main

import "testing"

// The probe's flags come from the chart, so a parser that quietly accepts a
// half-specified invocation would produce a probe that measures the wrong
// thing and still exits 0 -- worse than one that fails.
func TestProbeFlagsRefuseAnIncompleteInvocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"nothing at all", nil},
		{"allow only", []string{"-allow", "a:1"}},
		{"missing the L3 leg", []string{"-allow", "a:1", "-deny", "b:2"}},
		{"flag with no value", []string{"-allow"}},
		{"unknown flag", []string{"-allow", "a:1", "-deny", "b:2", "-deny-l3", "c:3", "-nope", "x"}},
		{"unparseable duration", []string{"-allow", "a:1", "-deny", "b:2", "-deny-l3", "c:3", "-dial-timeout", "soon"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := newProbeFlags().parse(tc.args); err == nil {
				t.Errorf("accepted %v; an under-specified probe reports a verdict it did not measure", tc.args)
			}
		})
	}
}

func TestProbeFlagsParseAFullInvocation(t *testing.T) {
	p := newProbeFlags()
	err := p.parse([]string{
		"-allow", "gonk-intake-internal:9090",
		"-deny", "gonk-intake:8080",
		"-deny-l3", "gonk-controller:9443",
		"-settle-timeout", "45s",
		"-dial-timeout", "2s",
		"-deadline", "3m",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.allow != "gonk-intake-internal:9090" || p.deny != "gonk-intake:8080" || p.denyL3 != "gonk-controller:9443" {
		t.Errorf("addresses not carried through: %+v", p)
	}
	if p.settleTimeout.String() != "45s" || p.dialTimeout.String() != "2s" || p.deadline.String() != "3m0s" {
		t.Errorf("durations not carried through: settle=%v dial=%v deadline=%v",
			p.settleTimeout, p.dialTimeout, p.deadline)
	}
}

// Unset durations must fall back to values that work, since the chart may omit
// them and a zero dial timeout would make every leg fail instantly and read as
// a perfectly enforced cluster.
func TestProbeFlagDurationsHaveWorkingDefaults(t *testing.T) {
	p := newProbeFlags()
	if err := p.parse([]string{"-allow", "a:1", "-deny", "b:2", "-deny-l3", "c:3"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.dialTimeout <= 0 || p.settleTimeout <= 0 || p.deadline <= 0 {
		t.Fatalf("a zero duration would make every leg fail instantly and read as ENFORCED: %+v", p)
	}
	if p.settleTimeout > p.deadline {
		t.Errorf("settle timeout %v exceeds the overall deadline %v, so the probe can never finish settling",
			p.settleTimeout, p.deadline)
	}
}
