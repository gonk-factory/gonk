package trace

import (
	"sort"
	"strings"
)

// Outcome is the closed set a trajectory check can reach. Modelled on
// pkg/gate.Classify and pkg/verify: pure, total, no LLM, no I/O.
//
// THE ORDERING PRINCIPLE, inherited verbatim from both: ANYTHING WE COULD NOT
// OBSERVE OUTRANKS ANYTHING WE JUDGED. An unobserved trace gets its own outcome
// and is never read as "the agent did nothing".
type Outcome string

const (
	// OutcomeSatisfied: every declared predicate held.
	OutcomeSatisfied Outcome = "satisfied"
	// OutcomeViolated: a predicate failed on POSITIVE evidence. The only
	// outcome that rejects a batch.
	OutcomeViolated Outcome = "violated"
	// OutcomeUnobserved: we did not see enough to judge. MUST NOT reject --
	// rejecting here would convert our own telemetry outage into a re-slung
	// bead, and eventually into an escalation onto a pricier rung.
	OutcomeUnobserved Outcome = "unobserved"
	// OutcomeNoPolicy: the agent declares no trajectory predicates. Distinct
	// from Satisfied so "nothing was asked of this agent" is never confused
	// with "everything asked of it held".
	OutcomeNoPolicy Outcome = "no-policy"
)

// Verdict is the outcome plus a human-readable reason. The reason is for the
// log and the violation string; nothing keys off it.
type Verdict struct {
	Outcome Outcome
	Reason  string
}

// Rejects reports whether this verdict should stop a batch being applied.
//
// EXACTLY ONE OUTCOME REJECTS. This is the invariant the whole slice is
// accountable to: a trajectory predicate may only reject, never grant. Nothing
// downstream may key off Satisfied to unlock anything -- no auto-merge, no
// widened path allowlist, no skipped gate -- because a process assertion
// becomes an incentive target the moment it has consequences.
func (v Verdict) Rejects() bool { return v.Outcome == OutcomeViolated }

// Policy is one agent's declared trajectory requirements, read from the BAKED
// pack's effect-shape.toml. Never from anything the agent can write: that is
// the same self-widening loophole pkg/verify closes by reading test
// declarations from the base commit.
type Policy struct {
	// RequiredTools must ALL appear at least once.
	RequiredTools []string
	// ForbiddenTools must NOT appear at all.
	ForbiddenTools []string
	// MinCalls is a per-tool floor.
	MinCalls map[string]int
	// RequireTargetReadFor lists the batch VERDICTS that may only be reached
	// after the session demonstrably read its primary target. This is the
	// predicate the slice exists for: reply-only and close produce no artifact,
	// so nothing downstream can catch a confabulated diagnosis.
	RequireTargetReadFor []string
	// ReadTools are the tool names that count as reading. Declared rather than
	// hardcoded because harnesses name them differently.
	ReadTools []string
}

// Empty reports whether the policy asks for nothing.
func (p Policy) Empty() bool {
	return len(p.RequiredTools) == 0 && len(p.ForbiddenTools) == 0 &&
		len(p.MinCalls) == 0 && len(p.RequireTargetReadFor) == 0
}

// Input is everything Classify needs. Kept as a struct so adding a field later
// cannot silently reorder positional arguments at a call site.
type Input struct {
	Trace Trace
	// Verdict is the batch's effects verdict ("reply-only", "code-change",
	// "close"), or "" when the batch declared none.
	Verdict string
	// Target is the session's primary target as the PACK would name it --
	// repo-relative. Matching against it normalises the collected value, which
	// is absolute because that is what the tool was actually given.
	Target string
}

// Classify is pure and total: same inputs, same verdict, no I/O, no clock.
func Classify(in Input, p Policy) Verdict {
	if p.Empty() {
		return Verdict{OutcomeNoPolicy, "no trajectory predicates declared for this agent"}
	}
	// UNOBSERVED IS CHECKED FIRST AND OUTRANKS EVERYTHING. Every rule below
	// reasons about what the trace contains, and on an unobserved trace every
	// one of them would read as a violation for want of evidence.
	if !in.Trace.Observed() {
		return Verdict{OutcomeUnobserved, "no trajectory evidence for this session; refusing to judge what we did not observe"}
	}

	counts := in.Trace.ToolCounts()

	// Forbidden first: it is the only rule that fires on the PRESENCE of
	// evidence, so it is the one least likely to be wrong about a partial trace.
	for _, tool := range p.ForbiddenTools {
		if counts[tool] > 0 {
			return Verdict{OutcomeViolated, "forbidden tool was used: " + tool}
		}
	}

	// A PARTIAL trace can only support presence, never absence: a tool we did
	// not see may simply be in the part we missed. So every remaining rule --
	// all of which reason about absence -- is unsafe on a partial trace.
	if in.Trace.Completeness == Partial {
		return Verdict{OutcomeUnobserved, "trajectory evidence is partial; absence of a tool call is not evidence it did not happen"}
	}

	for _, tool := range p.RequiredTools {
		if counts[tool] == 0 {
			return Verdict{OutcomeViolated, "required tool was never used: " + tool}
		}
	}
	for _, tool := range sortedKeys(p.MinCalls) {
		if counts[tool] < p.MinCalls[tool] {
			return Verdict{OutcomeViolated, "tool " + tool + " was used fewer times than required"}
		}
	}

	if in.Target != "" && contains(p.RequireTargetReadFor, effectiveVerdict(in.Verdict)) {
		if !readTarget(in.Trace, p.ReadTools, in.Target) {
			return Verdict{OutcomeViolated,
				"verdict " + effectiveVerdict(in.Verdict) + " requires an observed read of " + in.Target + ", and none was seen"}
		}
	}
	return Verdict{OutcomeSatisfied, "all declared trajectory predicates held"}
}

// effectiveVerdict mirrors effects.Batch.EffectiveVerdict's empty-means-
// reply-only rule WITHOUT importing pkg/effects, which would make this package
// depend on the contract it is meant to check independently.
func effectiveVerdict(v string) string {
	if v == "" {
		return "reply-only"
	}
	return v
}

// readTarget reports whether any read-ish call names the target.
//
// MATCHING IS SUFFIX-BASED BECAUSE THE COLLECTED PATH IS ABSOLUTE. A tool is
// given /workspace/internal/paging/paging.go while a pack or an issue names
// internal/paging/paging.go, and requiring equality would make this predicate
// false for every session ever recorded. Normalising HERE rather than at ingest
// is deliberate: the collected value is what actually happened, and rewriting
// evidence on the way in to suit today's predicate is how evidence stops being
// evidence.
func readTarget(t Trace, readTools []string, target string) bool {
	want := strings.TrimPrefix(strings.TrimSpace(target), "./")
	if want == "" {
		return false
	}
	for _, c := range t.Calls {
		if len(readTools) > 0 && !contains(readTools, c.Tool) {
			continue
		}
		got := strings.TrimPrefix(strings.TrimSpace(c.Target), "./")
		if got == "" {
			continue
		}
		if got == want || strings.HasSuffix(got, "/"+want) {
			return true
		}
	}
	return false
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// sortedKeys makes MinCalls evaluation order deterministic, so the REASON a
// batch was rejected does not depend on map iteration order.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
