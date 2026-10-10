# ADR-007: gonk is a broker-shaped orchestrator, and where ground truth comes from

Status: accepted 2026-08-13

This ADR records two decisions that were made in code over July–August 2026 but
never written down, and which together supersede the spec's founding description
of the system.

1. **gonk is no longer "a pack plus glue."** It is a broker-shaped orchestrator
   that happens to deploy Gas City. Section 1 records the drift, why it was
   correct, and what claims it falsifies.
2. **Where a change's acceptance criteria come from**, now that agents author
   MRs. Section 4 defines three tiers of ground truth and the rule that governs
   which artifacts may serve as one.

It cross-references ADR-002 (config precedence), ADR-003 (intake trust
boundary), ADR-004 (rung policy and budget enforcement), ADR-005 (pack and
images), ADR-006 (chart seams), the triage-broker design
(`docs/superpowers/specs/2026-07-27-triage-broker-design.md`), and the Warp
research in `docs/research/`.

Numbering note, following ADR-006's example: this document is **ADR-007**. Go by
`ls docs/adr/`, never by a plan or spec's guess at its own number.

---

## 1. The founding description is false and should stop being repeated

`docs/superpowers/specs/2026-07-12-gonk-stack-design.md:17` states:

> Design philosophy: gonk is **a Gas City pack plus glue**, not a new orchestrator.

That was true when written and is now false. The system as built:

- Owns the **dispatch decision** (`cmd/gonk-gate dispatch` is the only code path
  that pours work, and it re-decides unconditionally — it never trusts an order
  var's `rung`, `reservation_id`, or `key_ref`).
- Owns the **write plane**. Agents emit a proposed-effects batch; a deterministic
  broker validates its shape and applies every mutation under its own bot PAT.
  Agent pods hold no forge credentials.
- Owns **work admission and money** (`gonk-meter`, ledger, reservations,
  virtual-key provisioning), **intake and its trust boundary** (`gonk-intake`,
  ADR-003), **per-session repository delivery** (`pkg/rig`, grant-gated tarball,
  no forge credential in the pod), and the **mutation-plane authentication**
  (ed25519 grants on the city `[api]`).
- Routes its two live triggers *around* Gas City's native formula idiom entirely
  (see §3).

Gas City supplies a supervisor, a session provider, a bead store, and an event
log. That is a runtime dependency, not an architecture. Every decision that
determines whether work happens, what it costs, and what it is permitted to
change lives in gonk.

**Decision.** Describe gonk as *a broker-shaped orchestrator for a self-hosted
forge, running agent sessions on Gas City*. Amend the spec's §1 philosophy line
rather than leaving it to mislead. The "pack plus glue" framing additionally
distorts planning — it makes owned surface area look like integration work, and
it is part of why the formula path survived as long as it did.

## 2. Why the drift was correct

The spec's original shape (§4.3) had the agent post its own comments via the
GitLab API with a scoped token. The broker replaced it. This was the best
decision the project has made, and the evidence is external: **Warp shipped the
naive version first and converged on the same design.** Their June 2026 triage
workflow granted the agent `issues: write`; the current one splits it into a
read-only agent emitting structured JSON plus a deterministic apply job, with the
stated reason that agent write access

> "would introduce a prompt injection risk (like commenting on other PRs or even
> deleting the PR)."

gonk reached that design by construction rather than after an incident. Two
properties follow, and both should be treated as load-bearing:

- **There is no credential in the pod to exfiltrate.** Warp's equivalent accepts
  a short-lived, user-scoped forge token in the sandbox and buys attribution with
  it. gonk buys non-exfiltratability instead and must synthesise attribution —
  which is exactly what `gonk-m6t` broke, and why that bead is not cosmetic.
- **Blast radius is bounded by the effect shape, not by the prompt.** A prompt
  injection can make the agent *ask* for anything; it cannot make the broker
  *do* anything outside `effect-shape.toml`. This is why `gonk-066` (the shape
  gate is syntactic and cannot express workflow scope) blocks any file-effect
  kind shipping.

## 3. The formula path is deprecated

`cmd/gonk-gate/broker_inject.go` ports `issue-triage` and `scaffold` onto the
broker. `mention` still pours a formula, and that path provably cannot deliver a
prompt (upstream Gas City drops caller vars, gonk-6gs / #4668). The result is not
a degraded feature; it is a routing-table entry that fails silently. Tracked as
`gonk-ecn`.

**Decision.** Port `mention` onto the broker, then delete `pack/formulas/`, the
formula orders, and the `[steps.check]` machinery. Maintaining two orchestration
idioms produced the dead trigger and three zombie carry-forwards in PLAN.md.
Until that lands, a dead trigger must fail loudly rather than idle.

Counter-argument recorded: formulas are Gas City's native idiom and would return
if upstream fixed the var-substitution bug. Response: both live triggers already
route around them, upstream has shown no movement, and every plan since 04 has
paid a documentation tax on the corpse.

## 4. Ground truth: three tiers, and who may author one

Once an agent authors a merge request, "is this change correct?" needs a
referent. The failure mode to avoid is **circular validation**: a model drafts a
spec from an issue, another model implements against that spec, a third checks
the implementation against it, and every step passes while the work is wrong,
because nothing in the loop traces to a human statement of intent.

Warp's answer to this is easy to miss and is the load-bearing part of their
design: the spec is **LLM-drafted and human-ratified**, and their published
framing calls human spec approval *"the highest-leverage checkpoint in the
loop."* The ratification, not the authorship, is what makes it ground truth.

gonk generalises this into three tiers, each with a different author, lifetime,
and check:

| Tier | Artifact | Authored by | Lifetime | Answers |
|---|---|---|---|---|
| **Intent** | the issue; for large work, a ratified spec | human (issue author or ratifier) | per change | did we build the thing that was asked for? |
| **Orientation** | Navigator conventions, `roadmap.md`, `vision.md`, ADRs | human, durable | project | does it fit how we build things here? |
| **Mechanics** | build, tests, lint, type-check, shape gate, annotated-diff validator | deterministic | continuous | is it valid, and does it do what it claims? |

**The governing rule, which is the existing invariant applied to specs:**

> A model may **draft** an artifact at any tier. No model-authored artifact
> becomes **ground truth** until a human ratifies it or a deterministic check
> validates it.

This is the same principle already stated in `docs/HANDOFF-next-session.md` —
*"models supply constraints and objections; only deterministic checks and humans
grant permission"* — extended to cover acceptance criteria.

### 4.1 For a triaged issue that becomes an MR, the issue is the spec

The four-state triage rubric already encodes the answer, and this is the reason
to adopt it beyond routing:

- **`ready-to-implement` means the issue is specific enough to serve as its own
  acceptance criteria.** No separate spec artifact is authored or needed.
- **`needs-info` is the cheap spec gate.** It is the state for "this is not
  specific enough to be ground truth," which is why its comment must carry *the
  smallest set of concrete questions whose answers would unblock re-triage*. The
  answers come from a human, and the amended issue becomes the referent.
- **`ready-to-spec`** is the only state that needs a spec artifact — reserved for
  work with genuine product ambiguity or cross-cutting scope.

Consequence: the spec-authoring agent is **not on the critical path** for the
majority of work, which is why the local-parity roadmap defers it behind
verification. When it is built, it is a drafting aid the operator edits and
commits — never an autonomous stage whose output is trusted unread.

### 4.2 Review checks orientation; it is a separate check from intent

Reviewing a diff against durable project documents is a different question from
reviewing it against a per-change spec, and it has properties the per-change
check does not: it needs no per-change artifact, it is stable across changes, and
the mechanical half of it (`nav lint`, build, tests) is deterministic.

This is where Navigator survives, and the research supports the split. Warp's
`AGENTS.md` is a conventions document with the validator removed; they have no
lint equivalent, and their own alignment story leans on a learn-conventions-over-
time loop that they never validated. gonk should keep the authored-and-linted
half — a deterministic conventions check fits the no-LLM-judge invariant
natively, where prose instructions a small model is merely expected to honour do
not.

**Boundary, stated explicitly so it is not eroded later:** a model reading a diff
and commenting on its fit with project conventions is a model judgment, and that
is permitted **only** because it produces advisory comments for a human. It must
never feed the escalation ladder. `pkg/gate.Classify` stays pure and total, and
the regression test that greps the package for model references stays. Agent
review is non-blocking by design, matching the merge policy already in force:
agents comment, humans merge, no auto-merge in any version.

### 4.3 Model diversity in review is a weak control here

Using a different model to review than to implement avoids self-consistency bias,
and is worth doing when it is free — the local fleet makes it free, since LiteLLM
already serves several distinct 128K models (a vLLM-hosted NVFP4 Qwen3.6-35B, an
Ollama-hosted Qwen3.6-35B, and Nemotron3-33B), so reviewer and implementer can be
genuinely different models rather than two calls to the same one.

It still should not be *relied* on. A peer-capability model reviewing a peer's
diff is a weak signal, and it is a model judgment, which §4.2 permits only as
advisory output. The deterministic tier carries the weight — build, tests, lint,
the shape gate, and the annotated-diff coordinate validator, which eliminates
hallucinated line numbers as a failure class rather than asking a second model to
catch them.

## 5. Consequences

- The spec's §1 philosophy line and §4.3 agent-posts-via-API design are
  **superseded**; the spec should reference this ADR rather than be silently
  contradicted by the code.
- `gonk-ecn` (dead mention trigger) and the formula deletion are now
  architecture decisions, not cleanup.
- The four-state triage rubric acquires a second justification: it is the
  mechanism that decides whether an issue can serve as ground truth. This raises
  its priority relative to spec-authoring machinery.
- Navigator's scope narrows and sharpens: the **conventions-plus-lint** half is
  retained as the orientation tier; the "faithful plugin port" ambition (spec
  §7.2 v2) is not justified by anything in the research and should be re-argued
  before it is scheduled.
- Any future stage that introduces a new artifact must declare which tier it
  belongs to and who ratifies it.

## 6. Open

- **Attribution under the broker** (`gonk-m6t`) is the cost of §2's trade and is
  unpaid. Per-project cost attribution is a genuine differentiator — it is an
  open feature request at Warp — and it is currently broken.
- **Workflow-scope gating** (`gonk-066`) must close before any file-effect kind
  ships. Non-negotiable ordering.
- **Ratification mechanics for `ready-to-spec` work** are unspecified: what
  marks a spec as human-ratified, and what prevents an unratified spec from
  being consumed by an implementation run. To be settled when the spec stage is
  built, not before.
