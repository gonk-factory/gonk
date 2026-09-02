# Design note: trajectory evaluation

_2026-09-02. Prompted by the owner reading Google's "The New SDLC with Vibe
Coding" whitepaper, which names **trajectory evaluation** as a validation
primitive for agentic development. Beads: `gonk-y8r` (design), `gonk-p8j`
(slice 1), `gonk-hsb` (slice 2). **A design note, not a plan** — nothing here is
scheduled, and the interesting half of it is an argument for building *less*
than the whitepaper suggests._

## What it is, and the question it was asked to answer

Trajectory evaluation checks **what a session did**, not only what it produced:
which tools it called, which skills it loaded, in what order. The whitepaper's
worked example is "did the feature-implementation session actually write and run
tests" — asserted against the session's tool log rather than against the code
it emitted.

The question put to this note was whether that fits gonk, and where it would
attach.

## It fits, and the architecture already has the shape twice

gonk has two deterministic classifiers with the same signature, and neither has
a model in it:

| package | input | decides |
|---|---|---|
| `pkg/gate/outcome.go` | `Signals` | how one session attempt ended |
| `pkg/verify/verdict.go` | `Evidence` | whether a change passed red-green |
| *(proposed)* `pkg/trace` | `Trace` | whether the session did what its role requires |

A trajectory check is `Trace -> Classify -> Verdict`: a total function over
observations of session behaviour. It sits **beside** those two rather than
inside either, and it honours spec §6.3's "no LLM judges" without amendment —
the evidence is mechanical and the rules are set arithmetic over tool names.

So the structural answer is yes. The rest of this note is about which question
to point it at, because the obvious one is already answered better elsewhere.

## The whitepaper's headline example is the wrong one for gonk

"Did the session write and run tests" is exactly what `pkg/verify` decides, and
it decides it **strictly better**.

- A trajectory assertion says: *the agent invoked the test tool.*
- The red-green sequence proves: *a new test exists, it was red on the base
  commit, it is green after the code diff, and no previously-passing test was
  lost* (`RuleMonotonicity`).

One is a proxy for diligence. The other is the thing diligence was supposed to
produce. `gonk-066`'s notes already make this argument against the
protected-path denylist — outcome evidence beats process evidence wherever both
exist — and it transfers verbatim.

**Do not build trajectory checking to answer that question.** It would add a
weaker second opinion to a settled one, and a weaker opinion that can fail
independently is a source of false rejections, not of safety.

## The real gap is triage, and `gonk-066` does not cover it

`gonk-066` is about code-writing agents, where an artifact exists to verify. The
gap this note is actually for is the opposite case.

**A `reply-only` triage verdict produces no artifact at all.** The entire gate is
four syntactic checks, at `cmd/gonk-gate/broker_apply.go:231-251`:

```
effects.ParseBatch       valid JSON, known kinds
effects.Validate         per-kind cardinality from effect-shape.toml
effects.ValidateTargets  target_iid must be in the injected context
effects.ValidatePaths    file effects canonical, relative, under .agent/
```

Nothing in that chain reads the work. An agent that reads nothing, fetches no
linked MR, greps no code, and confabulates a plausible-sounding diagnosis emits
a batch that passes all four cleanly — and **no downstream experiment will ever
catch it, because there is no change to run a suite against.** The verifier
pipeline has nothing to say about a comment.

That is the case where a trajectory predicate earns its place, and it is
narrow enough to state in one line:

> a `reply-only` or `close` verdict requires at least one observed read of the
> session's primary target.

Two lesser wins are worth recording. **Negative assertions** ("never invoked a
forge write", "never read outside scope") are cheaper and more direct as trace
predicates than as inferences from the effect batch. And **loop/thrash
forensics** — an agent burning turns re-reading the same file — is real
operational value, though it is diagnosis, not gating, and should not be
conflated with it.

## Provenance: a trace the agent can edit is not evidence

This is the constraint that determines the whole implementation order, so it
goes before the design rather than after it.

The obvious source is the session's own tool log, and it is the wrong one.
`pack/agents/triage/agent.toml` sets `OPENCODE_PERMISSION` to allow-all and
pins `XDG_DATA_HOME=/tmp/gonk-home/.local/share`, so opencode's session storage
sits in a directory the agent's own shell tools can write. `gonk-e9m`
establishes that issue text can obtain a shell in that pod.

**A trace the agent can edit is not evidence — it is a field the agent fills
in.** Gating on it would be strictly worse than not gating at all, because it
would look like a control while being one. This is the same limit `pkg/verify`'s
header states about a test framework running in-process with the agent's code,
and it deserves the same volume.

There are two honest collection points:

| route | why it is trustworthy | cost |
|---|---|---|
| **the meter** | LiteLLM sees every request server-side, outside the pod; `pkg/atags` already joins its rows to project/rig/bead/session/rung/attempt | extends an ingest path that exists |
| **Gas City, out-of-band** | the session provider records tool events the pod cannot reach | `pkg/gcapi/transcript.go` carries `TranscriptTurn{Role,Text}` only; no tool events in the contract — an upstream ask |

The meter route goes first: it needs nothing from a dependency we do not
control, and it inherits the trust boundary that already makes
`pkg/gate.Signals.ModelTokens` admissible.

**And its honest limit:** the meter observes what the *model asked for*, not
what the *harness did*. A tool call opencode refused, retried, or truncated
looks identical to one that ran. For the triage read-predicate that is
acceptable — the case being caught is a model that never even asked — but a
predicate that assumes execution will eventually be wrong about one that was
denied, and that must be written down rather than discovered.

## The invariant

> **A trajectory predicate may only reject. It may never grant, and it must
> never escalate a rung.**

This is the same invariant `gonk-066` settled for the intent phase ("a model may
only NARROW the envelope, never widen it") and the same one `pkg/gate.Escalates`
guards, and it is not stylistic. **Any process assertion becomes an incentive
target the moment it has consequences.** "Must have run tests" is satisfiable by
running the suite against an empty package — the exact isomorph of `gonk-066`'s
CI-deletion example. "Must have read the issue" is satisfiable by a read the
model never used.

Concretely, two things follow:

- `pkg/trace`'s outcomes must not be reachable from `pkg/gate.Escalates`. If a
  *missing* trace escalates a rung, we have bought an escalation out of our own
  telemetry outage — the precise failure `gate.Classify` exists to prevent.
- No downstream policy may key off a *passing* trajectory check to unlock
  anything: no auto-merge, no widened path allowlist, no skipped gate.

These predicates are worth having as a floor under confabulation and worthless
as a ceiling on anything.

The corollary, inherited from both existing classifiers: **anything we could not
observe outranks anything we judged.** An `ABSENT` or `PARTIAL` trace gets its
own outcome class and is never read as "the agent did nothing". Completeness
must be a field on the evidence from the first line of the collector; a consumer
written against a plain list of tool names cannot have it retrofitted.

## Where the code goes

| piece | location |
|---|---|
| `Trace` collection, joined by `pkg/atags` | `pkg/spend` / `internal/meter` (`gonk-p8j`) |
| `pkg/trace` — `Trace`, `Classify`, closed `Outcome` set, pure | new, modelled on `pkg/verify` (`gonk-hsb`) |
| per-agent predicates | `[trajectory]` in `pack/agents/<agent>/effect-shape.toml` |
| enforcement | fifth gate at `cmd/gonk-gate/broker_apply.go`, after `ValidatePaths` |
| explicitly **not** | `pkg/gate.Escalates` |

The `effect-shape.toml` placement is the load-bearing choice. That file is
already the broker's answer to *what is this agent allowed to say*; trajectory
is *what must it have done to be allowed to say it*, and the two belong in one
place. It must be read from the **baked pack** (`/opt/gonk/pack/agents/<agent>/`),
exactly as the shape is today, and never from anything the agent can write —
the same self-widening loophole `pkg/verify` closes by reading test declarations
from the base commit.

Keep the predicate vocabulary small and decidable: required-tool,
forbidden-tool, minimum-count. **If a predicate cannot be evaluated by set
arithmetic over tool names, it is a judgement and does not belong here.**

## Sequencing

1. **`gonk-p8j` — collect, and gate on nothing.** Ship the evidence path alone,
   observe real sessions, measure how often the trace is complete. A predicate
   enabled on unmeasured evidence will reject honest batches, and a rejected
   batch on the triage path re-slings the bead.
2. **`gonk-hsb` — classify and enforce**, starting with the single triage
   read-predicate.

Deliberately not sequenced: the Gas City upstream ask. File it if the meter
route proves insufficient, not before.

## What this note does not settle

- **Whether LiteLLM's spend log retains response bodies in this deployment**, or
  whether tool-call capture needs a proxy-side callback instead. This is a real
  cost and belongs in `gonk-p8j`'s estimate rather than as a surprise inside it.
  Check `internal/meter/litellm` before committing to a shape.
- **What normalised argument shape is sufficient** to answer "did it read the
  target" without turning the ledger into a transcript store. `pkg/spend/rows.go`
  holds no bodies today and that property is worth keeping: prompt and response
  text carry untrusted issue content and, on cloud rungs, left our premises to
  begin with.
- **Whether any predicate beyond the triage read one is worth its false
  positives.** The presumption in this note is no, until the collector has run
  long enough to say otherwise.
