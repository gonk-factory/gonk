package rung

import (
	"testing"
	"time"

	_ "time/tzdata" // the quiet-hours cases call time.LoadLocation

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
)

type saboteur struct {
	name string
	bug  func(Input) Decision
}

func saboteurs() []saboteur {
	return []saboteur{
		{"ignores the budget entirely", func(in Input) Decision {
			in.Spend = budget.Spend{}
			return Decide(in)
		}},
		// The three reservation fields get THREE saboteurs, not one. A single
		// saboteur that zeroes all three is caught by the cost row alone, which
		// masks a missing token row -- and the token side has only a LOOSE
		// backstop behind it (Decision 9's synthetic USD ceiling), so this tight
		// gate is what actually holds.
		{"ignores reserved COST (the concurrency hole, dollars)", func(in Input) Decision {
			in.Spend.ReservedCostUSD = 0
			return Decide(in)
		}},
		{"ignores reserved MONTHLY TOKENS", func(in Input) Decision {
			in.Spend.ReservedTokens = 0
			return Decide(in)
		}},
		{"ignores reserved PER-TASK TOKENS", func(in Input) Decision {
			in.Spend.ReservedTaskTokens = 0
			return Decide(in)
		}},
		{"counts SYNTHETIC dollars against the real cost gate", func(in Input) Decision {
			// Decision 9's failure mode, and the one someone will genuinely write
			// while "cleaning up the two cost fields". It bricks the onboarding
			// default: a monthly_cost_usd: 0 project can no longer afford its own
			// local rung. Caught by "synthetic spend NEVER blocks a local rung".
			in.Spend.CostUSD += in.Spend.SyntheticCostUSD
			in.Spend.ReservedCostUSD += in.Spend.ReservedSyntheticCostUSD
			return Decide(in)
		}},
		{"ignores the action veto", func(in Input) Decision {
			in.Trigger = atags.TriggerOnboarding // the one trigger no action gates
			return Decide(in)
		}},
		{"reports an unloadable config as merely 'disabled'", func(in Input) Decision {
			in.Invalid = false
			return Decide(in)
		}},
		{"counts infra retries across ALL rungs instead of per-rung", func(in Input) Decision {
			// The bug: ConsecutiveInfraFailures ignores which rung failed, so
			// infra flakiness on a cheap rung eats the expensive rung's retries.
			idx := Escalations(in.Prior)
			if idx >= len(in.Effective.Ladder) {
				return Decide(in)
			}
			target := in.Effective.Ladder[idx]
			p := append([]Attempt(nil), in.Prior...)
			for i := range p {
				if p[i].Outcome == OutcomeInfraFailed {
					p[i].Rung = target
				}
			}
			in.Prior = p
			return Decide(in)
		}},
		{"escalates on infra failures (spec 6.3's central promise)", func(in Input) Decision {
			p := append([]Attempt(nil), in.Prior...)
			for i := range p {
				if p[i].Outcome == OutcomeInfraFailed {
					p[i].Outcome = OutcomeGateFailed
				}
			}
			in.Prior = p
			return Decide(in)
		}},
		{"never escalates", func(in Input) Decision {
			in.Prior = nil
			return Decide(in)
		}},
		{"off-by-one on the ladder index", func(in Input) Decision {
			d := Decide(in)
			if d.Kind != Run {
				return d
			}
			for i, r := range in.Effective.Ladder {
				if r == d.Rung && i+1 < len(in.Effective.Ladder) {
					next := in.Effective.Ladder[i+1]
					d.Rung, d.Model = next, in.Catalog[next].Model
					return d
				}
			}
			return d
		}},
		{"trusts stale spend data", func(in Input) Decision {
			in.SpendAsOf = in.Now
			return Decide(in)
		}},
		{"ignores quiet hours", func(in Input) Decision {
			in.QuietHours = nil
			return Decide(in)
		}},
		{"runs without a virtual key", func(in Input) Decision {
			in.KeyReady = true
			return Decide(in)
		}},
		{"runs an unregistered project", func(in Input) Decision {
			in.Registered = true
			return Decide(in)
		}},
		{"defers instead of denying (parks a doomed bead forever)", func(in Input) Decision {
			d := Decide(in)
			if d.Kind == Deny {
				d.Kind = Defer
				d.RetryAfter = in.Now.Add(time.Hour)
			}
			return d
		}},
		{"denies instead of deferring (throws away recoverable work)", func(in Input) Decision {
			d := Decide(in)
			if d.Kind == Defer {
				d.Kind, d.RetryAfter = Deny, time.Time{}
			}
			return d
		}},
		{"has no infra-retry cap", func(in Input) Decision {
			in.MaxInfraRetries = 1 << 30
			return Decide(in)
		}},
		{"treats the per-task ceiling as monthly (defers instead of denying)", func(in Input) Decision {
			in.Spend.TaskTokens = 0
			return Decide(in)
		}},
	}
}

func TestSaboteursAreCaught(t *testing.T) {
	cases := decideCases()
	for _, sab := range saboteurs() {
		caught := false
		for _, c := range cases {
			in := c.in(t)
			got := sab.bug(in)
			if got.Kind != c.want.Kind || got.Rung != c.want.Rung || got.Attempt != c.want.Attempt ||
				got.Reason != c.want.Reason || !got.RetryAfter.Equal(c.want.RetryAfter) {
				caught = true
				break
			}
		}
		if !caught {
			t.Errorf("saboteur %q produced the expected decision on EVERY table row.\n"+
				"The table does not constrain this behavior, so nothing would stop us shipping the bug.\n"+
				"Add a row that catches it -- do not weaken the saboteur.", sab.name)
		}
	}
}
