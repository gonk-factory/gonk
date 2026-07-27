# Gonk Triage Broker (Foundation Slice): Design

Status: DRAFT (brainstormed 2026-07-27; pending spec review + user review)
Extends: `docs/superpowers/specs/2026-07-19-gonk-agent-harness-design.md` (the
357-line broker-lite draft, preserved on `origin/design-gonk-agent-harness`).
This document is the *buildable first slice* of that draft, reconciled with the
substrate proven end-to-end on 2026-07-26 and with four decisions taken in the
2026-07-27 brainstorm. Where this document and the draft disagree, this one wins;
the draft's open questions are resolved in §11.

## 1. Summary

Today, triage works by giving the agent pod a GitLab bot token and letting
opencode post the comment itself. That was proven end-to-end on 2026-07-26 (the
`gonk` bot posted a real triage comment on a real issue, model call metered), but
it is the wrong shape: the agent holds a credential and causes an irreversible
external side effect with no supervisor in between.

This slice removes that. The unifying idea, carried verbatim from the draft:

> **Agents are pure producers of _proposed effects_. A broker injects context,
> validates the proposal's _shape_ against what the task was supposed to
> produce, and applies the allowed effects under its own identity. The agent
> holds no forge credentials and causes no live external side effect; nothing
> reaches GitLab until the broker has seen the whole batch and decided.**

For triage the effect batch is metadata only: `[{comment, body}, {label, add}]`.
The broker validates the batch shape deterministically (exactly one comment,
labels, nothing else), then posts under its own bot PAT — or escalates, or
rejects. The agent posts nothing and holds only the model key.

"Propose, don't execute" applies only to **real-world** effects. Inside its
sandbox the agent still runs live. The proposal is the net externally-visible
outcome; the agent keeps full agency where it is cheap and reversible, and loses
it only at the irreversible boundary.

## 2. Goals and non-goals

### Goals

- **G1.** A `pkg/effects` contract: the typed proposed-effects batch plus a
  deterministic shape validator.
- **G2.** A **broker** folded into the controller: inject context → run the
  harness → validate the batch shape → apply allowed effects via the bot PAT.
  GitLab-metadata effects only in this slice.
- **G3.** **Zero forge credentials in the agent pod.** The pod holds only the
  LiteLLM key. No bot PAT, no glab auth, no orac CA (the agent never reaches the
  forge). This deletes two deployment blockers found on 2026-07-26.
- **G4.** **Cost-class-aware escalation** through the existing meter ladder:
  local→local escalation is free (auto + logged); crossing into a paid cloud rung
  is gated by a human-approved allowance scoped to instance/namespace/project,
  and cloud rungs get stingier turn caps.
- **G5. Proof:** triage on this substrate produces the *identical* comment +
  labels + marker it produces today, with a pod that has no PAT mounted, plus the
  two safety cases (out-of-shape batch applies nothing; no-batch run escalates in
  the local tier and lands `needs-human` when exhausted).

### Non-goals (this slice)

- **N1.** Code effects, the internal git remote, the coder↔reviewer loop, and
  human-event handling. All Phase 5 / director subsystem; the seam is §10.
- **N2.** A second harness image (`pi`/`oryx`). One harness (`opencode`) means a
  single configured agent image suffices, which sidesteps the Gas City
  image-selection question entirely (§11, OQ1).
- **N3.** An ML classifier over events or shapes. The evaluation here is a
  deterministic cardinality check; the ML router is a later, additive slice
  (§10) and needs an outcome-data flywheel that does not exist yet.
- **N4.** Any change to how money is *enforced*. The rung ladder, the LiteLLM
  virtual-key hard door, and reservation semantics are untouched. G4 adds a
  cost-class *policy* on top of the ladder the meter already walks; it does not
  change the arithmetic.

## 3. Decisions carried in (do not re-litigate)

From the 2026-07-19 draft (D1–D6 there) and the 2026-07-27 brainstorm:

- **D1. Deterministic-first.** The shape gate has no model in it. The ML
  classifier is a later drop-in upgrade to the *routing* step only, not a
  dependency here.
- **D2. Return via the beads store.** The agent writes its proposed-effects JSON
  onto its own work bead via `bd`; the broker reads it from the shared Dolt
  store. This is the only channel shared between the agent pod and the
  controller, and it is the one proven on 2026-07-26. (Resolves draft OQ3.)
- **D3. Broker folded into the controller.** Inject rides in `gonk-gate dispatch`;
  validate + apply ride in `gonk-sweep`. The bot PAT lives only controller-side
  (already true for intake), never in a pod. (Resolves draft OQ4; see §4.)
- **D4. Cost-class-gated escalation.** The free/paid boundary is the local/cloud
  rung boundary. Free local escalation is automatic and logged; cloud escalation
  is gated. Money authority stays in the meter, deterministic. (Resolves draft
  OQ2; see §7.)
- **D5. Output shape is a deterministic alignment signal.** A run whose expected
  shape is `{comment}` that instead proposes a `merge_request` is refused and
  applies nothing. This is the plan-vs-implementation gate, with no LLM judge at
  the decision point.
- **D6. Per-session, per-harness credential injection.** Credentials are injected
  per session, never baked into the pod. Triage injects `{model key}`; a future
  harness injects whatever *it* needs, additively, without re-adding a forge PAT.
  This is the Phase 5 seam (§10).

## 4. Architecture

### 4.1 Component home

The broker is **folded into the existing controller**, not a new service. The
controller already holds the bot PAT, `bd`/`/city`, meter access, and the
write-auth signing key — everything the broker needs. Its two phases map onto the
two places that already do that work:

| Phase | Home | Reuses |
|---|---|---|
| **Inject** (fetch context, spawn session) | `gonk-gate dispatch` | it already pours the session; adds context fetch + bd delivery |
| **Validate + apply + decide** | `gonk-sweep` | it is already the deterministic outcome brain that reads finished sessions and re-slings escalations |

Plus one new package, `pkg/effects` (the typed batch + validator + a thin apply
helper over the GitLab client).

The draft preferred a standalone `cmd/gonk-broker` (the meter precedent: an
irreversible decision decoupled from the pod lifecycle). We diverge because the
apply authority *already* lives controller-side (intake's onboarding writes, the
proven triage post), the bot PAT is *already* a controller-mounted secret, and
this slice is not yet the full director. The security property the draft cared
about — **the bot PAT never enters an agent pod** — is preserved exactly. If the
director slice later wants the broker extracted into its own service, `pkg/effects`
and the sweep-side apply move out cleanly; nothing here blocks that.

### 4.2 Data flow (triage on the new substrate)

1. Event arrives exactly as today: webhook → `gonk-intake` (token verify, bot
   suppression, onboarding check) → `gonk-dispatch` → meter `/decide` returns
   `{run, rung, model}`. Unchanged.
2. **Inject.** The broker (dispatch side) fetches the context triage needs — the
   issue body + existing labels, `.agent/` (thin loader), the `.gonk.yml` policy
   — using the controller's PAT + CA, size-capped on injection (§11, OQ5). It
   renders `prompt + context + model + attribution atags` and delivers it to the
   session through the **bd channel**: a pool-routed work bead carrying
   `template_overrides.initial_message` (fallback `gc session submit`). The agent
   reads none of this from GitLab.
3. **Produce.** The agent pod spawns on the single opencode harness image. Its
   env carries only the LiteLLM key — **no PAT, no bot token, no orac CA.** It
   runs opencode over the injected context and writes its **proposed-effects JSON
   batch onto its own work bead via `bd`**, then closes the bead. It makes no
   external API call.
4. **Validate.** The broker (sweep side) reads the batch from the bead and runs
   it through `pkg/effects`: schema-valid JSON, target-bound to the injected
   context, and shape-matching `triage`'s expected-effect-shape.
5. **Decide + apply.** On a valid, in-shape batch the broker posts the comment
   (stamping the `<!-- gonk:bead:… -->` marker itself) and sets the labels, under
   its own PAT. On no-batch or a shape violation it applies nothing and decides
   escalate-or-reject per §7. Outcome and metering continue as today.

Contrast with today: the pod currently holds the bot session and posts directly.
After this slice the pod holds no forge credential and posts nothing; the broker
does, only after the shape check. Because the broker writes the comment, the bead
marker is stamped deterministically — no longer relying on the model to get it
right (it dropped the closing `-->` in the 2026-07-26 proof).

## 5. The proposed-effects contract (`pkg/effects`)

An **effect** is one intended externally-visible change. v1 effect kinds:

- `comment` — post a note on an issue/MR (body; optional discussion thread).
- `label` — add/remove labels on an issue/MR.
- `new_issue` — open a follow-up issue (title, body, labels). *[Reserved for the
  router slice; contract admits it, apply path not built here.]*
- `code` / `merge_request` / `commit` — **named for forward-compatibility, not
  implemented in this slice** (Phase 5, §10). Naming them now keeps the contract
  stable so Phase 5 is additive.

An **effect batch** is the ordered list a run returns. It is:

- **Structured** — a schema'd JSON object the harness emits as its final output,
  validated at the contract layer (the same discipline as the meter's schema'd
  decisions). A harness that cannot produce valid JSON *fails the run*; it never
  half-applies.
- **Atomic and inspectable** — the broker sees the entire batch before applying
  any effect. This is what a future human "Approve" gate reads and what makes a
  run reproducible in a test (given context X, the run proposes batch Y).
- **Target-bound** — every effect on an existing resource names it by id (issue
  iid), cross-checked against the session's injected context so a run cannot
  propose an effect on a resource it was never handed. A create-type effect
  (`new_issue`, later `merge_request`) binds to a **parent** resource from the
  session context, so a follow-up is always anchored, never free-floating.

## 6. The deterministic shape gate

### 6.1 Expected-effect-shape

Each task profile declares an **expected-effect-shape**: a per-kind cardinality
map `{min, max}`, default `{0,0}` = forbidden. Allow-list is `max ≥ 1`; require
is `min ≥ 1`; "exactly one" is `{1,1}`. The max count is load-bearing — the
triage proof hinges on rejecting a *second* comment, which set membership alone
cannot express.

- `triage` → `comment {1,1}`, `label {0,N}`, everything else `{0,0}`.

**Where it lives.** `agents/<name>/effect-shape.toml`, colocated with the prompt
for authoring and diffing, but it is **broker data, not Gas City pack data.** The
broker reads it from the **baked pack** (`/opt/gonk/pack/agents/<name>/`), not
from `agent.toml`: Gas City's loader rejects unknown top-level keys, and reading
from the baked pack sidesteps the `gc init` block-stripping that bit `[order.env]`
earlier. It is data, not code.

### 6.2 The check is deterministic

The broker compares the batch's per-kind counts to the cardinality map with no
model in the loop — the same "no LLM judges at the decision point" discipline as
the rung ladder and `gonk-sweep`. It directly enforces the alignment cases: a run
routed to `triage` that proposes a `merge_request`, a `commit`, or a second
comment is refused and applies nothing.

The v1 classifier is the deterministic allow-list. The eventual ML-model-over-
shapes is a drop-in upgrade to the *routing* step (§10), not to this gate; the
apply/reject machinery around it is unchanged.

## 7. The decision: apply / escalate / reject

The broker turns **deterministic signals** into one of three outcomes; no model
in the decision, and every escalation is bounded by the meter's existing
ladder + budget hard door.

Signals available for triage, all deterministic:

- Did the agent emit a **valid batch** (schema-valid JSON)? yes/no
- Does the batch **pass the shape gate**? yes/no
- Did the session complete / did the model answer? (`gonk-sweep`'s existing
  objective signals)

Decision:

| Signal | Meaning | Outcome |
|---|---|---|
| Valid batch **+** passes shape | aligned work | **APPLY** — broker posts comment + labels under its PAT; meter joins spend as today |
| No batch / invalid JSON / session failed | capability/infra shortfall | **ESCALATE** (see §7.1) |
| Valid batch **but fails shape** | misalignment | **ESCALATE** in the free local tier (a local retry is free); never auto-cross to cloud |

### 7.1 Cost-class-gated escalation

The broker only emits a deterministic *needs-escalation* signal and re-slings
through `gonk-dispatch` → meter `/decide` — the machinery that already exists.
The meter owns the decision, now cost-class-aware:

| Next rung on the ladder | Meter's rule | Turn cap |
|---|---|---|
| **Local** (`kind: local`, $0) | Grant automatically, **log the escalation**. Bounded by the finite local ladder + attempt caps. | normal |
| **Cloud** (`kind: cloud`, cost > 0) | Grant **only if a human-approved cloud allowance exists** for this scope (instance / namespace / project); else defer → `needs-human`. A direct human decision also grants it. | **stingier** than local |
| Local ladder exhausted, no cloud allowance | — | `needs-human` |

Because a local retry costs nothing, we are permissive there and log-and-allow it
for any unsatisfactory outcome (no-batch *or* shape violation), bounded by the
finite local ladder. We are stingy and gated only at the **cloud boundary**:
crossing into paid spend needs a human, or a pre-approved allowance scoped to the
instance/namespace/project, and cloud turns are capped tighter than local ones.

The cloud allowance is a meter/`opercfg` concept enforced deterministically at
`/decide` — consistent with "no LLM/no broker judges money." Rungs gain a
per-rung turn cap; the scope carries an optional human-approved cloud allowance,
**default off** (so a cloud rung denies to human until someone opts in). In this
slice the triage proof runs entirely on free local rungs, so the cloud gate is
specced-and-reserved: the default-off path (`cloud rung → needs-human`) is
implemented and tested; the rich instance/namespace/project allowance hierarchy
is a follow-up.

## 8. Channels

- **Inject (context in).** `gonk-gate dispatch` fetches issue body/labels,
  `.agent/`, `.gonk.yml` with the controller PAT + CA, renders the prompt +
  context + model + atags, and delivers via the **bd channel** — a pool-routed
  work bead carrying `template_overrides.initial_message`, fallback
  `gc session submit`. Proven on 2026-07-26 to bind a session and drive opencode.
- **Produce.** The agent writes its proposed-effects JSON onto its own work bead
  via `bd`, then closes it. Touches no external API.
- **Return (effects out).** `gonk-sweep` reads the batch from the bead, runs
  `pkg/effects`, and applies under the controller PAT.

The batch travels as data on a bead the agent already owns; no new transport, and
the batch is inspectable and testable at rest.

## 9. What changes

- **Agent pod / pack.** The agent produces JSON and posts nothing.
  `GONK_BOT_TOKEN`, glab auth, and the orac CA all leave the pod; only the
  LiteLLM key remains. The entrypoint's glab-setup is removed. The injected
  prompt instructs "emit the proposed-effects batch to your bead; call no
  external API." `pack/agents/triage/` gains `effect-shape.toml`.
- **New `pkg/effects`.** Types, the schema validator, the cardinality shape
  validator, target-binding checks, and a thin apply helper over the GitLab
  client.
- **`gonk-gate`.** `dispatch` grows the inject step (fetch context + bd
  delivery). `sweep` grows the validate + apply + decide step.
- **Chart.** Drop the agent-pod PAT/CA wiring (a net simplification). The
  controller keeps its PAT + CA (already mounted).
- **Meter / `opercfg`.** Add the per-rung turn cap and the (default-off) cloud
  allowance flag; teach `/decide` the local-free / cloud-gated escalation rule.

## 10. Forward compatibility (the Phase 5 seam)

Phase 5 is the coder loop: gonk decides work is required and delivers it via a
targeted branch/MR, likely as multiple single-turn agents each evaluated by the
gate. Removing the forge PAT and CA now is what Phase 5 *requires*, not something
it fights:

- **The real-forge PAT is reserved to the broker in Phase 5 too** (draft D6:
  real-remote push is the broker's, never the agent's). A coder agent has the
  same posture as a triage agent — it emits a proposed effect; the broker
  applies. Removing the PAT here sets the correct precedent.
- **The orac CA is forge-facing only.** The agent keeps only what it needs to
  reach its *model endpoint* (LiteLLM; plain in-cluster HTTP today, so nothing).
  A Phase 5 internal remote is gonk-controlled trust — a separate, per-harness
  mount paired with scoped internal-remote creds, never the real-forge PAT.
- **Tooling stays in the image.** `git`/`glab` remain baked (the thin tool
  superset); we remove runtime *creds*, not binaries. The coder harness reuses
  the image with different injected credentials.

Two seams make Phase 5 additive rather than a rework:

1. **Per-session, per-harness credential injection (D6).** Triage injects
   `{model key}`. The coder harness injects `{model key, + branch-path creds}`
   without re-adding a forge PAT.
2. **The contract already reserves `code` / `merge_request` / `commit`.** Phase 5
   slots in as new effect kinds + new expected-shapes; the broker's apply grows a
   "create branch / open MR" path. The agent stays a pure single-turn producer.

One Phase 5 fork this slice deliberately leaves open (for the director spec): the
branch is returned either **as a `code` effect** (data in the batch → broker
creates the branch/MR → zero remote creds in the pod, the cleanest extension) or
via **push to an internal remote** (per-session ref-scoped creds + a pre-receive
hook, draft D6). Both are compatible with everything here; the reserved effect
kinds keep either path available.

## 11. Resolved open questions (from the 2026-07-19 draft)

- **OQ1 (k8s image selection).** DEFERRED. One harness (opencode) means one
  configured agent image; no per-session image override is needed. Revisit only
  when a second harness rung is added (N2).
- **OQ2 (shape-violation escalation).** RESOLVED by §7: cost-class-gated. Free
  local escalation for any unsatisfactory outcome (including shape violations);
  cloud gated by a scoped human-approved allowance; never auto-cross to cloud.
- **OQ3 (return channel).** RESOLVED: the beads store (D2).
- **OQ4 (broker home).** RESOLVED: folded into the controller (D3, §4.1).
- **OQ5 (context-injection budget).** CARRIED FORWARD: large issues/MRs are
  size-capped on injection, the same discipline as `.gonk.yml` fetch, to avoid an
  unbounded-context DoS. The cap is a broker constant; over-cap context is
  truncated with a marker the prompt acknowledges.

## 12. Testing and proof

- **T1. `pkg/effects` unit tests.** Batch schema validation; target-binding
  (existing-resource and create-type/parent-bound); the shape validator against
  per-kind cardinality maps — including every safety case as an explicit test
  (reject a second comment; reject a forbidden kind; accept the exact triage
  shape). Data-driven validator against fixtures; no routing logic here.
- **T2. Broker (sweep side).** Given injected context X and a recorded batch Y,
  assert the exact GitLab calls made or refused, against a stub GitLab client
  (like intake's). No model in the path, so it is fully reproducible.
- **T3. Harness emit.** A `test/stubmodel`-driven session (the existing
  deterministic OpenAI-compatible stub) makes the opencode harness emit a known
  batch to its bead, proving the emit mechanism without a live model.
- **T4. The proof (G5).** Triage on the new substrate produces the identical
  comment + labels + marker as current triage on the same fixture, with a pod
  that has **no PAT mounted** (assert the mount is absent).
- **T5. Shape-violation path.** A run proposing an out-of-shape effect applies
  nothing and records the violation. Mutation-tested: stripping the shape check
  makes this test fail.
- **T6. Escalation path.** A no-batch run re-slings and, on a local-only ladder,
  lands `needs-human` when the ladder is exhausted; a cloud rung with the default
  (no allowance) denies to `needs-human` rather than spending.

## 13. Milestones (walking skeleton)

1. `pkg/effects` contract + shape validator (T1).
2. Meter: per-rung turn cap + default-off cloud allowance + the local-free /
   cloud-gated `/decide` rule (T6).
3. Broker skeleton in `gonk-gate`/`gonk-sweep`: inject → run (stub) → validate →
   apply (T2, T3).
4. Harness image + pack: agent emits the effect batch to its bead; strip the
   forge creds/CA from the pod (T3, and the mount-absent assertion of T4).
5. Triage port + the zero-creds proof, in the throwaway e2e (T4, T5).
