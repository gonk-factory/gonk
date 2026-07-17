# Plan 04: the gonk pack & container images

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the Gas City pack that carries gonk's agents, formulas and orders,
the **deterministic, zero-LLM gate binary** (`gonk-gate`) that is the *only* path
from "work exists" to "a session spawns", and the four container images
(`gonk-agent`, `gonk-controller`, `gonk-intake`, `gonk-meter`) at exact tags.

**Architecture:** Pack-first (spec §1). The pack carries **agents, formulas,
orders**; the chart (Plan 05) carries the two Go servers. Every path that could
spend money passes through `gonk-gate`, a deterministic Go binary with **no model
call in it, ever**, which asks `gonk-meter` `POST /v1/policy/decide` immediately
before pouring a formula, and reports a **strictly classified outcome** to
`POST /v1/policy/outcome` afterwards. The pack itself contains no model name, no
price, and no site-specific value: it stamps whatever meter hands it.

**Tech Stack:** Gas City pack format **schema 2** (TOML), Go 1.26, opencode
(pinned — **OD-3**), podman (`--network=host`, see `docs/environment.md`), GitLab
container registry at `registry.orac.local`.

**Spec:** `docs/superpowers/specs/2026-07-12-gonk-stack-design.md` §§4.2, 4.3,
4.4, 5.3, 5.5, 6.1, 6.2, 6.3, 7.1, 9, 10.3.
**§6.2.3 of the spec is WRONG and this plan corrects it — see "The rung gate is
in two places" below.**

**Environment:** `docs/environment.md` is authoritative for every fact about the
deployment target. Read it before Task 5.

---

## Gas City is not RUNNING during this plan — and that is fine: the chart deploys it, from an image THIS plan builds

**Correcting an earlier framing error.** Gas City is NOT a prerequisite the owner
must stand up separately. Gonk ships **one umbrella Helm chart (Plan 05) that
deploys Gas City** (the controller), a Dolt server, gonk-intake/meter, and this
pack — there is no separate "deploy Gas City first" step. And the dependency runs
*toward* this plan, not away from it: **this plan (Tasks 6/9) builds the
`gonk-controller` image that the chart runs to bring Gas City up.**

So during Plan 04's own execution Gas City is not yet running — not because
anyone is waiting to deploy it, but because it comes up when Plan 05's chart is
installed. Anything requiring a *live* city is therefore verified **downstream**:
Plan 05's Task 0.5 smoke-deploy (which consumes this plan's controller image) and
Plan 06's L3 e2e — NOT here. That does not block most of Plan 04. The split:

| Buildable and testable **today** (no running Gas City) | Verified downstream, once Plan 05's chart deploys Gas City |
|---|---|
| `pkg/gate` — the outcome classifier (pure function, exhaustive table tests) | That the controller actually *fires* `gonk-dispatch` when intake POSTs the order-run route |
| `pkg/gcapi` — the typed supervisor client, against an `httptest` fake | That a poured formula actually spawns an opencode pod |
| `cmd/gonk-gate` — decide / pour / park / classify / re-sling / check / trailers | That `[steps.check]`'s exec runs where we think it runs |
| The pack files (`pack.toml`, agents, formulas, orders) + **structural validation** (Task 4) | That resume (`wake_mode = "resume"`) survives pod recreation (spec 11.4) |
| The pack run through **Gas City's real loader**, offline, in a container (Task 6) | The event-bus publish API (**OD-6**) |
| All four images, built and pushed at exact tags | |
| Commit provenance trailers (git hook, temp-repo tests) | |

**Task 6 is the honest one:** it builds `gc` from the MIT gascity source at a pin
and runs **the real loader** against our pack **in a container**. That is not a
schema approximation — it is the actual code that will reject an unknown key. It
needs no cluster. Everything past that (does the city *behave*) is handed to
Plan 06 and listed under "Hand-offs".

---

## LICENSING — A HARD CONSTRAINT ON HOW YOU MAY WORK

- **`github.com/gastownhall/gascity` is MIT.** Its source, and its spec docs
  (`docs/reference/specs/pack-spec.md`, `docs/reference/specs/formula-spec-v2.md`,
  `docs/reference/config.md`, `docs/tutorials/07-orders.md`), are a **clean
  derivation basis**. Read them. Cite them.
- **`github.com/gastownhall/gascity-packs` has NO top-level LICENSE.** That means
  **all rights reserved**. Its `github/` pack — its `pack.toml`, its intake
  scripts, its `rules.toml` — is **the obvious template and you may not use it.**

> **DO NOT OPEN `gascity-packs`. DO NOT COPY, ADAPT, PARAPHRASE, OR CLOSELY
> DERIVE FROM IT.** Not its file layout choices, not its prompt text, not its
> schema shapes. Everything structural this plan needs is **fully specified in
> the MIT `gascity` repo's own spec docs and loader source**, which is where every
> citation in this plan points. If you find yourself reaching for the packs repo
> "just to see how they did it", stop: that is the exact act the constraint
> forbids, and it would make gonk un-open-sourceable (spec §12.1).

The owner is asking the maintainers for a grant. Until it lands in writing, this
pack is **clean-room from the MIT specs**. If you are ever unsure whether a
detail came from the spec or from the packs repo, **delete it and re-derive it
from the spec.**

---

## GAS CITY FACTS (researched from the MIT source — do not contradict, do not invent beyond)

Supervisor REST API + SSE on **:8372**.

**`pack.toml`** at the pack root, **`schema = 2`**. The loader (`internal/config/pack.go`)
accepts exactly these top-level tables and **REJECTS UNKNOWN KEYS** — an invented
field is a hard load failure, not a warning:

```
pack  imports  agent_defaults  agent  named_session  service  webhook
providers  upstreams  runtimes  patches  doctor  commands  global  pricing
```

- **There is NO `[[order]]` and NO `[[formula]]` in `pack.toml`.** Orders are
  `orders/<name>.toml`; formulas are `formulas/*.toml`. Both are discovered by
  well-known directory.
- **Agents** are `agents/<name>/agent.toml` + `agents/<name>/prompt.template.md`.
  **The DIRECTORY NAME is the agent name** — a `name` field inside the file is
  ignored. Inline `[[agent]]` in `pack.toml` is legacy; **do not use it.**
- **`[[webhook]]` IS valid** in `pack.toml` (the prose spec table omits it; the
  loader accepts it — trust the loader). Mounted at `/hook/{name}`. Fields:
  `name`, `scope` (`city`|`rig`), `rig`, `[webhook.publication]`,
  `[webhook.verify]`, `[[webhook.rule]]`, `max_per_minute`. `WebhookRule.target`
  is `order` | `conversation`, default `order`.
- **Webhook verification is a closed registry of FIVE schemes**:
  `github-hmac-sha256`, `hmac-sha256`, `slack-v0`, `discord-ed25519`, `jwt-jwks`.
  **GitLab's `X-Gitlab-Token` is a constant-time shared-secret compare, not an
  HMAC over the body — `hmac-sha256` will NOT substitute.** See "Why gonk's pack
  ships no `[[webhook]]`" below.
- **Orders** (`orders/<name>.toml`):
  ```toml
  [order]
  description = "..."
  formula = "pancakes"     # XOR exec = "scripts/x.sh" -- NEVER BOTH
  trigger = "cooldown"     # cooldown | cron | condition | event | manual
  interval = "5m"
  pool = "worker"          # EXEC ORDERS MAY NOT HAVE A POOL
  timeout = "60s"
  ```
  Fired externally: `POST /v0/city/{cityName}/order/{name}/run`, body
  `{"vars": {k: v}}`, response `{status, scoped_name, tracking_id}`. Declared
  `[order.params]` are namespaced into an exec order's environment as
  **`GC_WEBHOOK_ARG_*`**. **The order-run route has NO per-route auth scheme —
  admission is by network position.** There is also `gc order run <name> [--rig <rig>]`.
- **Formulas** (`formula-spec-v2.md`): a DAG of `[[steps]]`; **each step becomes a
  bead handed to an agent** (i.e. every step is an LLM step). Top-level keys:
  `formula`, `vars`, `steps`, `extends`, `pour`, `requires`.
  **A formula CANNOT make an arbitrary HTTP call.** The only place a formula runs
  code is **`[steps.check]`**, an inline verification loop whose **only mode is
  `exec`**. *This is load-bearing and it shapes the whole design — see "Why the
  classifier is a sweeper order".*
- **k8s session provider** labels agent pods **hardcoded** (`internal/runtime/k8s/pod.go`):
  `app: gc-agent`, `gc-session: <label>`, `gc-agent: <agentLabel>`; annotation
  `gc-session-name`. The provider lists by `app=gc-agent`.
  **=> Plan 05's OD-5 is ANSWERED: `networkPolicy.agentPodSelector` is
  `matchLabels: {app: gc-agent}`.**
- **Session continuity:** `wake_mode = resume | fresh` (default `resume`);
  `resume_command` supports `{{.SessionKey}}` and takes precedence over the
  provider's resume flag.
- **Opencode** is a built-in session provider (ACP transport).
- Dockerfiles exist at `contrib/k8s/Dockerfile.{agent,base,controller,mail}` (MIT —
  a legitimate derivation basis for Tasks 5 and 6).

---

## THE CENTRAL CORRECTION: spec §6.2.3 is wrong, and the rung gate is in TWO places

Spec §6.2.3 says *"before session spawn, **the dispatch formula** asks meter for
the rung"*. **A formula cannot do that.** Per `formula-spec-v2.md`, a formula is a
DAG of steps handed to *agents*, and the only code it can execute is
`[steps.check]`. There is no HTTP verb in a formula. If we left this as written,
the rung decision would have to be made by an LLM — which violates spec §6.3's
"no LLM judges" at the exact point where money is spent.

**Owner decision (settled — implement, do not re-open): the decision is gated in
two places, and both speak `pkg/meterapi`.**

### Gate 1 — `gonk-intake` gates the FIRST dispatch

Intake calls `POST /v1/policy/decide`. It fires the order **only** on
`decision: "run"`, passing the chosen `rung`, `model`, `metadata`, `key_ref`,
`reservation_id` and `attempt` as order vars. On `defer` it records
`retry_after` and fires nothing; on `deny` it labels and fires nothing.

> **This makes Plan 02's "intake does NOT call `/policy/rung`" WRONG.** Plan 02
> states it in three places (the responsibility table, "The gonk-meter seam", and
> `dispatch.go`'s comments). It is listed under **"Upstream amendments required"**
> below. **You may not edit Plan 02 from this plan** — raise the amendment.

### Gate 2 — the `gonk-dispatch` **exec order** gates EVERY pour, including every re-sling

`orders/gonk-dispatch.toml` is an **exec** order (`exec = "scripts/gonk-dispatch.sh"`,
no pool — exec orders may not have one). Its script is a thin wrapper over
`gonk-gate dispatch`, a deterministic Go binary. It calls `/v1/policy/decide`
**again**, immediately before pouring the formula, and **it is the only code path
in the entire system that pours a formula.**

**This is the load-bearing half.** A ladder escalation after a failed gate is
**controller-initiated** — it never passes through intake at all. Without Gate 2,
an escalation would spawn a session at a higher rung with **no budget check and no
fresh reservation**, i.e. escalations spend unmetered. Gate 1 is an optimization
(don't churn a bead that meter will certainly deny, and give the operator an early
`defer` metric). **Gate 2 is the enforcement point.**

### The two call sites MUST agree. Here is exactly what breaks if they drift.

They agree by construction on the *types* (both import `pkg/meterapi`; a
compile-time contract), and Task 3 Step 8 asserts they agree on the *semantics*
with a shared table test. What each drift costs:

| Drift | Consequence | Severity |
|---|---|---|
| Intake says `run`, Gate 2 says `defer` | Order fires, no session spawns, bead parks. Wasted order. Self-heals on the next sweep. | **Benign** — this is the *designed* race, and it is why Gate 2 exists. |
| Intake skips `/decide` entirely (regression to Plan 02 as written) | Orders fire for out-of-budget / quiet-hours / disabled projects; Gate 2 refuses them all. Cost: churned orders and a noisy `defer` metric. | **Benign but loud.** Gate 2 still holds the money door. |
| **Gate 2 trusts intake's order vars instead of re-deciding** | **CATASTROPHIC.** Every controller-initiated re-sling reuses the *original* rung, the *original* `reservation_id` and the *original* `key_ref`. The escalated attempt runs **unbudgeted**, spend is **mis-attributed to the previous attempt's tags**, and `POST /v1/policy/outcome` is bound to a reservation that is already closed — so meter's ladder state and reality diverge silently. **"Budgets cannot be bypassed" becomes false at the application layer, not just the network layer.** | **This is the one. Task 3 Step 4's `TestDispatchAlwaysDecidesEvenWhenVarsCarryARung` exists solely to prevent it. Never delete it.** |
| They disagree on `deny` (intake treats it as retryable) | Intake re-fires the same doomed order every reconcile pass, forever. No spend (Gate 2 refuses), but a metric/log flood and a churned bead per pass. | **Bounded.** Caught by `gonk_intake_orders_denied_total` climbing monotonically. |
| They disagree on `defer` retry semantics (both retry the same bead) | Two retriers → potentially two sessions on one bead → **duplicate spend**. | **Mitigated by `BeadAnchor` idempotency** (Plan 02's carry-forward; Plan 06 kill tests K13/K18). The controller **must** dedupe on `BeadAnchor`. If it does not, this drift is a spend bug. |

**The invariant, stated once, to be quoted in ADR-004:**

> **No formula is ever poured except by `gonk-gate dispatch`, and
> `gonk-gate dispatch` always calls `/v1/policy/decide` first. There is no second
> pour path. A rung escalation must be paid for by a decision meter made.**

**Assumption to confirm with the owner:** both gates key meter's budget/ladder
state on the **deterministic `BeadAnchor`** (`gonk:{project}:issue:{iid}`), NOT on
the Gas City internal bead id. The BeadAnchor is the only identifier both gates
share (the Gas City bead does not exist at Gate 1) and it is stable across
re-slings, which is exactly what `/decide`'s open-reservation idempotency needs.
The Gas City bead id remains available at Gate 2 for the comment marker and the
bead-store record, but it never reaches meter.

---

## Why the classifier is a **sweeper order**, not a formula step

Gas City fact: **every `[[steps]]` becomes a bead handed to an agent.** So a
formula cannot contain a deterministic, non-LLM final step. And `[steps.check]` —
the only exec inside a formula — is a *verification loop*: it can answer "is the
artifact there yet?" (pass/fail), but it **cannot distinguish `gate-failed` from
`infra-failed`**, and it is re-run repeatedly, so it must be **side-effect-free**.

Therefore:

- **`[steps.check]` runs `gonk-gate check`** — a pure, idempotent GitLab query:
  *is the bot's comment carrying the bead marker on the issue?* Exit 0 = yes.
  **No POST, no state change, safe to run in a loop.**
- **`orders/gonk-sweep.toml`** — an **exec, cooldown** order (`interval = "30s"`,
  no pool) running `gonk-gate sweep`. This is the deterministic brain that (a)
  classifies every finished session, (b) POSTs `/v1/policy/outcome`, (c) re-fires
  `gonk-dispatch` on `escalate`/`retry`, and (d) re-fires `gonk-dispatch` for
  parked beads whose `retry_after` has passed. **No LLM anywhere in it.**

Both facts are consequences of the formula spec, not preferences. Record them in
ADR-004 so nobody "simplifies" the sweeper back into the formula.

---

## Why gonk's pack ships **no `[[webhook]]`** (decision, with the reason)

**Decision: every GitLab event goes to `gonk-intake`'s own receiver. gonk's
`pack.toml` contains no `[[webhook]]` table at all.**

1. **It is not expressible.** GitLab authenticates webhooks with a constant
   `X-Gitlab-Token` header. Gas City's verification registry is a **closed set of
   five** schemes, none of which is a constant-time shared-secret compare.
   `hmac-sha256` is an HMAC **over the body** and will not substitute — wiring it
   up would either reject every real GitLab delivery or, worse, be "fixed" by
   someone disabling verification on the one endpoint exposed to the ingress.
2. **It would not be enough anyway.** A `[[webhook.rule]]` can route to an `order`
   or a `conversation`. It cannot suppress bot-authored events, dedupe redeliveries,
   fetch and size-cap `.gonk.yml`, register the project with meter, or decide the
   project is even onboarded. Plan 02 already does all of that, on the trust
   boundary, with tests.
3. **Reconciliation is the correctness path** (spec §5.2). Webhooks are a latency
   optimization. Losing the pack's receiver costs nothing correctness-wise.

**Upstream contribution opportunity (do NOT depend on it):** a `gitlab-token`
verification scheme is a small, clean PR to Gas City's verify registry (spec §4.4
already names it as a candidate). Note it in ADR-004 as future work. **Nothing in
this plan may be built on the assumption it lands.**

---

## `pkg/meterapi` — imported, never restated

`pkg/meterapi` is **owned by Plan 03** (its Task 0) and lands physically in
Plan 02's Task 6. This plan **imports** it and does not restate one field of it.
The fields this plan uses:

- `meterapi.DecideRequest{Project, Rig, BeadID, SessionKey, Trigger}` —
  **there is NO attempt field and there never will be.** A caller-supplied attempt
  is a forgery vector for climbing the ladder (Plan 03, Decision 2). Task 3
  Step 3's `TestDispatchNeverSendsAnAttempt` asserts the marshalled JSON has no
  `attempt` key. **`BeadID` is the deterministic BeadAnchor** (`gonk:{project}:issue:{iid}`),
  the SAME value intake sent at Gate 1 and stable across re-slings — **not** the Gas
  City internal bead id. Gate 2 sends `dispatchArgs.BeadAnchor` here, never
  `dispatchArgs.BeadID`; Task 3 Step 8's `TestBothGatesSendTheSameBeadID` asserts it.
- `meterapi.DecideResponse{Decision, Rung, Model, Attempt, Reason, Detail,
  RetryAfter, Metadata, KeyRef, ReservationID, ReservationExpiresAt, Budget,
  Remaining, SpendAsOf}`. `defer` and `deny` are **HTTP 200** — normal answers,
  not errors.
- `meterapi.OutcomeRequest{Project, BeadID, SessionKey, Attempt, Rung,
  ReservationID, Outcome}` / `OutcomeResponse{OK, RecordedAttempt, Next, NextRung}`.
  `Next`/`NextRung` are **advisory**; the authoritative answer is the next `/decide`.
- Outcome constants: `OutcomeSuccess`, `OutcomeGateFailed`, `OutcomeInfraFailed`,
  `OutcomeAborted`. **Only `gate-failed` escalates.**
- `meterapi.SessionCostResponse` (`GET /v1/cost/session/{key}`) — `TotalTokens`
  and `Complete`. **A client must not publish a cost when `Complete` is false**
  (Plan 03, AD-7, carried here).
- `meterapi.ProjectResponse.Effective.Provenance` (`GET /v1/projects/{project}`) —
  the trailer switches.

**If you need a field that is not there, that is a change to Plan 03's contract,
made there, not here.**

---

## Owner decisions needed (facts only the owner has)

**OD-1 — The city name.** The supervisor route is
`POST /v0/city/{cityName}/order/{name}/run`. The city lives in the private
`gonk-city` repo (spec §7.3) and this plan cannot know its name.
*Built:* `gonk-gate` and intake both take it from **`GONK_CITY`** (env, no
default). `gonk-gate` **exits 2 with a named error if it is unset** — it does not
guess.
*Blast radius:* a wrong city name makes every dispatch a 404. **Loud, not silent** —
nothing runs, `gonk_gate_dispatch_errors_total{reason="supervisor_404"}` climbs.
Acceptable failure mode; still, get the real value before Plan 06.

**OD-2 — The pool name, and whether one pool or three.**
*Assumed default (revisit if wrong):* **one pool, `gonk`**, shared by all three
agents, `min_active_sessions = 0` (spec §4.2 — pools scale to zero).
*Blast radius:* trivial. A second pool is one line per order file. Split only when
real contention is observed (spec §12.5 defers fairness scheduling anyway).

**OD-3 — Which opencode version to pin.** `images/Dockerfile.agent` must pin an
exact opencode version. **Never `latest`** (`docs/environment.md`).
*Blast radius if left floating:* an agent image whose behaviour changes under you,
and **spec §11.4's named risk — ACP session resume across pod recreation — is
version-sensitive.** A floating opencode makes that milestone untestable.
**This one genuinely blocks Task 5.** If the owner has no preference, pin whatever
version is current at execution time, record the digest, and say so in the ADR.

**OD-4 — The triage prompt's label taxonomy and tone.** The prompts in Task 4 are
deliberately minimal and mechanical.
*Assumed default:* the agent must post **exactly one** comment containing the
machine-readable marker `<!-- gonk:bead:<bead_id> -->`, and apply labels prefixed
with the project's `triage.label_prefix` (default `gonk::`).
**The marker is LOAD-BEARING, not decorative:** `gonk-gate check` and
`gonk-gate sweep` grep for it. It is how a deterministic gate knows the artifact
landed without an LLM judging prose. If the owner rewrites the prompt, **the
marker survives or the gate goes blind.**

**OD-5 — Synthetic prices for local rungs.** Gonk ships **none**. They are
site-local operator config: the rung catalog (`pkg/opercfg`, Plan 03) carries
`synthetic_usd_per_1m_tokens`, and the **same** number must be set in LiteLLM's
`litellm_settings.model_cost_map` (gitops:`clusters/orac/apps/litellm/litellm.yaml`,
where local models are currently priced at **zero**). Plan 06's P3-4 verifies they
agree.

**OD-6 — The Gas City event-bus publish API.** Plan 06's **HB-4a** asks the gate
to *publish* its classification to the event bus. **The publish API is not among
the Gas City facts available to this plan, and this plan will not invent one.**
See "How HB-4 is satisfied" below for what is delivered instead, and what is
handed to Plan 06.

**OD-7 — How opencode attaches metadata to LiteLLM. [ANSWERED — VERIFIED]**
opencode sets a **static per-provider header** at spawn:
`provider.<gonk>.options.headers["x-litellm-spend-logs-metadata"]` = the JSON of
the seven `pkg/atags` keys. LiteLLM parses it into
`data["metadata"]["spend_logs_metadata"]` and persists it to
`LiteLLM_SpendLogs.metadata.spend_logs_metadata`. **All seven keys round-tripped
intact on a live LiteLLM v1.92.0** (smoke-tested 2026-07-13; see
`docs/environment.md`, "VERIFIED: the attribution chain works"). One session ==
one pod, so the session's tags are baked into that pod's opencode config at spawn
— no per-request plumbing, no per-session virtual keys. *Enterprise-gate caveat:*
LiteLLM's docs claim per-k/v `spend_logs_metadata` is an enterprise feature; it
was **not** enforced on v1.92.0. Documented fallback if a future upgrade starts
enforcing it: the `x-litellm-tags` header → the `request_tags` column (also
verified populated). Spec goal 4 (attribution at every granularity) rests on this;
**Plan 06 owns the live proof against a real opencode + LiteLLM.** See "The
attribution seam" below.

---

## Assumed defaults (build these; revisit if wrong)

**AD-1 — The rung catalog and the instance ladder are SITE-LOCAL. The pack and the
chart ship NEITHER.**
The real LiteLLM on this cluster has models `qwen3-14b` (Ollama on bailey,
`ollama_chat/qwen3:14b`) and `qwen3-8b-bailey`. **There is no model called
`qwen-local`.** The *rung name* is ours (`.gonk.yml`'s `ladder`, constrained to
`^[a-z0-9][a-z0-9-]*$`); the *rung catalog* (Plan 03's `pkg/opercfg`) maps a rung
name to a real LiteLLM model plus a synthetic price. So `qwen-local -> qwen3-14b`
is a **natural site-local mapping — and it belongs in the operator's config, not
in gonk.**
**The pack contains no model name anywhere.** It stamps `DecideResponse.Model`
verbatim. Task 4 Step 7 greps the whole pack for model-shaped strings and fails
the build if one appears.
*Blast radius if a default is shipped anyway:* a chart that "works" out of the box
by pointing at a model that does not exist on the operator's LiteLLM — a 404 at
first token, blamed on gonk. **The chart must FAIL to render without an operator
rung catalog and instance ladder, not guess.** (Upstream amendment to Plan 05.)

**AD-2 — Bead state lives in the bead store, behind an interface.**
`gonk-gate` needs per-bead work state (parked / running / needs-human,
`retry_after`, `reservation_id`, `rung`, `attempt`). It keeps it in the beads
store (Dolt, spec §4.2) via **`pkg/beadstore`, an interface with two impls**:
`BdCLI` (shells the MIT `bd` binary) and `Memory` (tests). **All of `gonk-gate`'s
logic is tested against `Memory`** — zero Dolt, zero containers.
*Blast radius:* the exact `bd` subcommands are confirmed against the real `bd` in
Task 6's container smoke test. If a subcommand is wrong, exactly one file
(`beadstore/bd.go`) changes. The risk is isolated by construction.

**AD-3 — `gonk-gate` is a single binary with subcommands, shipped in BOTH the
controller image and the agent image.** The controller needs `dispatch`, `sweep`,
`check`; the agent needs `trailers`. One binary, one contract, one test suite.
*Blast radius:* ~8 MB of duplicated static binary in the agent image. Cheap.

**AD-4 — Commit provenance trailers are installed as a `prepare-commit-msg` git
hook in the rig clone at session start**, not as agent prose.
A trailer the agent is *asked* to write is a trailer the agent will sometimes
forget, reword, or hallucinate a cost into. A git hook is deterministic and the
agent cannot bypass it with a plausible-sounding commit message.
*Blast radius:* git hooks are not cloned, so session setup must install it (Task 8).
If Gas City's session provider wipes the clone's `.git/hooks`, trailers vanish
silently — Task 8 Step 6 adds a `gonk-gate check --trailers` self-test, and Plan 06
must assert a real bot commit carries them.

**AD-5 — On `complete: false`, write `Gonk-Usage: pending`, never a number.**
Straight from Plan 03's AD-7, carried here as instructed: `/v1/cost/session/{key}`
is read *while the session is still open*, so `complete` is usually `false`.
**Writing the number anyway publishes a wrong cost into permanent git history.**

**AD-6 — Uncertainty never escalates.** If `gonk-gate sweep` cannot *prove* the
model answered, it classifies `infra-failed`. See the classifier below.

---

## The attribution seam (OD-7) — VERIFIED

Every LiteLLM request from an agent pod must carry `atags.Metadata()` — the seven
`gonk_*` keys — or **spend cannot be joined to a bead, and spec goal 4 is not met.**
Meter **mints** the metadata (it is the single charset-validation boundary,
Plan 01 carry-forward) and returns it in `DecideResponse.Metadata`. The pack must
**stamp it verbatim** onto every request.

**The mechanism is verified, not assumed** (smoke-tested against live LiteLLM
v1.92.0, 2026-07-13; `docs/environment.md`, "VERIFIED: the attribution chain
works"). opencode supports **static per-provider headers** via
`provider.<id>.options.headers` (spread into the AI-SDK factory). Because one
session == one pod, the session's tags are baked into that pod's config once, at
spawn — no per-request plumbing.

- **The specified mechanism (build this):** the agent entrypoint renders
  `overlay/opencode.json` setting
  `provider.<gonk>.options.headers["x-litellm-spend-logs-metadata"]` to the
  **verbatim `DecideResponse.Metadata` JSON** (the seven `pkg/atags` keys), passed
  to the pod as an order var (`GC_WEBHOOK_ARG_METADATA_JSON`). LiteLLM parses that
  header into `data["metadata"]["spend_logs_metadata"]` and persists it to
  `LiteLLM_SpendLogs.metadata.spend_logs_metadata`; **all seven keys round-tripped
  intact.** Per-key metadata (one virtual key per project) **merges** with the
  per-session header (request wins, key fills gaps), so the two compose.
- **Enterprise-gate fallback (documented contingency):** LiteLLM's docs claim
  per-k/v `spend_logs_metadata` is an enterprise feature; it was **not** enforced
  on v1.92.0. If a future upgrade starts enforcing it, switch the header to
  `x-litellm-tags`, which populates the `request_tags` column (also verified). Plan
  06 detects if an upgrade breaks the primary path.
- **Deeper fallback, only if opencode's header seam ever disappears:** `gonk-gate`
  asks meter for a **short-lived, per-attempt LiteLLM virtual key whose LiteLLM
  key-metadata carries the atags** (LiteLLM records key metadata on every spend
  row). Meter already owns key provisioning (Plan 03, AD-1), so this would be a
  **Plan 03 amendment**, not a new component: `KeyRef` becomes per-attempt rather
  than per-project. It is contingency-only — the header path is verified to work.
- **Contingency, if attribution ever fully failed:** spend would be attributable
  only to the **project**, not to bead/session/rung/attempt, and spec goal 4 would
  be partially unmet — a finding for the owner, not something to paper over. The
  mechanism is now verified, so this is the tail risk, not the expected case.

**Plan 06 owns the live proof against a real opencode + LiteLLM (see hand-offs).**

---

## File structure

```
pkg/gate/
  outcome.go        Classify(Signals) Outcome -- PURE. The heart of HB-4a.
  outcome_test.go   exhaustive table + the "infra never escalates" property
  marker.go         BeadMarker(beadID) -> "<!-- gonk:bead:gk-1a2b -->"; MarkerPresent()
  marker_test.go
pkg/gcapi/
  client.go         Gas City supervisor: RunOrder(city, name, vars) -> RunResult
  client_test.go    httptest fake supervisor
  gcapitest/server.go   in-memory supervisor (records poured orders)
pkg/beadstore/
  store.go          interface: Get/Put/List work records; labels
  memory.go         in-memory impl (every gonk-gate test runs on this)
  bd.go             BdCLI impl -- shells `bd`. Smoke-tested in Task 6's container.
  store_test.go
cmd/gonk-gate/
  main.go           subcommand router + exit-code contract
  dispatch.go       GATE 2: /decide -> pour | park | deny
  sweep.go          classify finished sessions -> /outcome -> re-sling; unpark
  check.go          [steps.check]: is the marker on the issue? PURE, idempotent.
  trailers.go       prepare-commit-msg trailer block
  *_test.go
pack/
  pack.toml
  agents/triage/agent.toml          agents/triage/prompt.template.md
  agents/scaffold/agent.toml        agents/scaffold/prompt.template.md
  agents/mention/agent.toml         agents/mention/prompt.template.md
  formulas/gonk-triage.toml
  formulas/gonk-scaffold.toml
  formulas/gonk-mention.toml
  orders/gonk-dispatch.toml         EXEC. Gate 2. The only pour path.
  orders/gonk-sweep.toml            EXEC, cooldown. Classify + re-sling + unpark.
  orders/gonk-triage.toml           formula = gonk-triage
  orders/gonk-scaffold.toml         formula = gonk-scaffold
  orders/gonk-mention.toml          formula = gonk-mention
  scripts/gonk-dispatch.sh          thin: exec gonk-gate dispatch
  scripts/gonk-sweep.sh             thin: exec gonk-gate sweep
  scripts/gonk-check.sh             thin: exec gonk-gate check
  overlay/opencode.json.tmpl        rendered at session start; sets the verified
                                    x-litellm-spend-logs-metadata provider header (OD-7)
internal/packtest/
  pack_test.go      STRUCTURAL VALIDATION -- the 15-table allow-list, order XOR,
                    exec-no-pool, agent-dir contract, no model names, no secrets
  golden_test.go
images/
  Dockerfile.agent        opencode + git + glab + bd + gonk-gate
  Dockerfile.controller   gc (built from MIT gascity @ pin) + bd + pack + gonk-gate
  Dockerfile.intake       static Go
  Dockerfile.meter        static Go (+ testclock variant, Plan 03 HB-3)
  versions.env            EVERY pin lives here. One file. No `latest` anywhere.
Makefile                  images, push, pack-validate, no-latest
docs/adr/ADR-004-pack-gate-and-images.md
```

---

### Task 1: `pkg/gate` — the outcome classifier (pure; this is HB-4a's core)

**This is the single most important function in the pack.** Plan 03 says it
plainly: *"if it reports an infra failure as `gate-failed`, it buys an escalation
the project did not earn, and that classification is the single most important
thing the pack gets right."*

**The design rule: the classifier reads only OBJECTIVE signals from services gonk
owns. It never asks the agent how it did.** An agent that self-reports its own
outcome can hallucinate `gate-failed` and walk the project up to its most
expensive rung. There is no LLM in this function and there never will be.

**Files:**
- Create: `pkg/gate/outcome.go`, `pkg/gate/marker.go`
- Test: `pkg/gate/outcome_test.go`, `pkg/gate/marker_test.go`

- [x] **Step 1: Write the failing test** — `pkg/gate/outcome_test.go`

```go
package gate

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// The classification table IS the contract. Every row is a real, reachable state.
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		in   Signals
		want string
	}{
		// --- aborted: a human or a config change stopped it. Neither retries nor escalates.
		{"aborted beats everything", Signals{Aborted: true, ArtifactPresent: true, ModelTokens: 500}, meterapi.OutcomeAborted},
		{"aborted with no work done", Signals{Aborted: true}, meterapi.OutcomeAborted},

		// --- infra: never escalates, always retries the SAME rung (spec 6.3).
		{"reservation expired", Signals{ReservationExpired: true, ArtifactPresent: true, ModelTokens: 500}, meterapi.OutcomeInfraFailed},
		{"gitlab unreachable -> we cannot even LOOK for the artifact", Signals{ArtifactUnknown: true, ModelTokens: 500}, meterapi.OutcomeInfraFailed},
		{"spend data stale -> we cannot prove the model answered", Signals{SpendStale: true}, meterapi.OutcomeInfraFailed},

		// --- THE INVARIANT: no tokens means no escalation, ever.
		// A session that never got a completion did not FAIL at the work; it never
		// got to do the work. Pod evicted before first token, LiteLLM 5xx on every
		// call, model cold-start timeout -- all land here, and all retry the same rung.
		{"no artifact, ZERO tokens", Signals{ModelTokens: 0}, meterapi.OutcomeInfraFailed},

		// --- gate-failed: the ONLY outcome that escalates. It must be EARNED:
		// the model answered (tokens > 0) and the work still is not there.
		{"no artifact, model answered", Signals{ModelTokens: 500}, meterapi.OutcomeGateFailed},
		{"no artifact, one token", Signals{ModelTokens: 1}, meterapi.OutcomeGateFailed},

		// --- success.
		{"artifact present", Signals{ArtifactPresent: true, ModelTokens: 500}, meterapi.OutcomeSuccess},
		// Success with zero tokens is bizarre (a cached/duplicate comment?) but it
		// is still success: the work is there. It costs nobody an escalation.
		{"artifact present, zero tokens", Signals{ArtifactPresent: true}, meterapi.OutcomeSuccess},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in); got != tc.want {
				t.Fatalf("Classify(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The load-bearing property, asserted independently of the table above so that
// deleting a table row cannot silently delete the invariant.
//
// SPEC 6.3: "connection errors, LiteLLM 5xx, pod evictions -> retry same rung
// with backoff; only completed-but-failed-gate outcomes escalate."
func TestInfraFailureNeverEscalates(t *testing.T) {
	// Every combination of signals that includes ANY infra indicator must
	// classify as infra-failed, and infra-failed must never be the escalating
	// outcome.
	for _, art := range []bool{false, true} {
		for _, tok := range []int64{0, 1, 1_000_000} {
			for _, sig := range []Signals{
				{ReservationExpired: true, ArtifactPresent: art, ModelTokens: tok},
				{ArtifactUnknown: true, ArtifactPresent: art, ModelTokens: tok},
				{SpendStale: true, ArtifactPresent: art, ModelTokens: tok},
			} {
				got := Classify(sig)
				if got != meterapi.OutcomeInfraFailed {
					t.Fatalf("Classify(%+v) = %q, want infra-failed", sig, got)
				}
				if Escalates(got) {
					t.Fatalf("infra-failed must NEVER escalate a rung")
				}
			}
		}
	}
}

// Only ONE outcome buys an escalation. If this test ever needs updating, stop and
// think very hard about what you are about to make free.
func TestOnlyGateFailedEscalates(t *testing.T) {
	for _, o := range []string{meterapi.OutcomeSuccess, meterapi.OutcomeInfraFailed, meterapi.OutcomeAborted} {
		if Escalates(o) {
			t.Fatalf("%q must not escalate", o)
		}
	}
	if !Escalates(meterapi.OutcomeGateFailed) {
		t.Fatal("gate-failed must escalate, or the ladder never climbs and every test of it is vacuous")
	}
}

// A gate failure must be PAID FOR. This is the anti-forgery property: you cannot
// climb the ladder for free by having a session die instantly.
func TestEscalationRequiresSpentTokens(t *testing.T) {
	for _, tok := range []int64{0} {
		if o := Classify(Signals{ModelTokens: tok}); Escalates(o) {
			t.Fatalf("a session that burned %d tokens escalated (%q) -- a free ride up the ladder", tok, o)
		}
	}
	if o := Classify(Signals{ModelTokens: 1}); !Escalates(o) {
		t.Fatal("a session that got a real completion and produced nothing must escalate")
	}
}
```

`pkg/gate/marker_test.go`:

```go
package gate

import "testing"

func TestMarkerRoundTrip(t *testing.T) {
	m := BeadMarker("gk-1a2b")
	if m != "<!-- gonk:bead:gk-1a2b -->" {
		t.Fatalf("marker = %q", m)
	}
	body := "I looked at this issue and here is my analysis.\n\n" + m + "\n"
	if !MarkerPresent(body, "gk-1a2b") {
		t.Fatal("marker not found in a comment that carries it")
	}
	// A DIFFERENT bead's marker must not satisfy this bead's gate: otherwise one
	// successful triage would mark every other bead on the issue as done.
	if MarkerPresent(body, "gk-9999") {
		t.Fatal("a foreign bead marker satisfied the gate")
	}
	// A human quoting gonk's comment must not satisfy the gate either -- the gate
	// asks "did the BOT post it", and the caller filters by author, but belt and
	// braces: an empty bead id matches nothing.
	if MarkerPresent(body, "") {
		t.Fatal("empty bead id must never match")
	}
}
```

- [x] **Step 2: Run it and watch it fail**

Run: `go test ./pkg/gate/ -v`
Expected: FAIL — no Go files / undefined `Classify`, `Signals`, `Escalates`, `BeadMarker`.

- [x] **Step 3: Implement `pkg/gate/outcome.go`**

```go
// Package gate is gonk's deterministic outcome classifier: the pure function
// that decides how one session attempt ended.
//
// THERE IS NO LLM IN THIS PACKAGE, AND THERE NEVER WILL BE (spec 6.3: "no LLM
// judges"). Everything here is a total function of objective signals gathered
// from services gonk owns -- GitLab (did the artifact land?) and gonk-meter (did
// the model actually answer?). The agent is NEVER asked how it did: a session
// that can classify itself can hallucinate `gate-failed` and walk its project up
// to its most expensive rung for free.
//
// The one invariant to defend with your life (spec 6.3, and Plan 06's HB-4):
//
//	INFRA FAILURES NEVER ESCALATE A RUNG. ONLY A COMPLETED-BUT-FAILED GATE DOES.
//
// and its corollary, which is what makes the first one enforceable:
//
//	AN ESCALATION MUST BE PAID FOR. If we cannot PROVE the model answered
//	(ModelTokens > 0), the outcome is infra-failed, not gate-failed.
package gate

import "gitlab.orac.local/agentic/gonk-project/pkg/meterapi"

// Signals are the objective inputs to a classification. Every field is gathered
// by gonk-gate from GitLab or gonk-meter. None of them comes from the agent.
type Signals struct {
	// Aborted: a human closed the bead, or the project's config changed under the
	// session (spec: "cancelled by a human or by a config change"). Neither
	// escalates nor retries.
	Aborted bool

	// ReservationExpired: meter's reservation TTL elapsed before the session
	// reported. The session may or may not have done work; either way, the
	// reservation it held is gone and the attempt is void. INFRA.
	ReservationExpired bool

	// ArtifactUnknown: we could not ASK GitLab whether the artifact landed
	// (GitLab 5xx, network error, auth failure). Absence of evidence is not
	// evidence of a gate failure -- classifying it as one would buy an escalation
	// out of our own outage. INFRA.
	ArtifactUnknown bool

	// SpendStale: meter's view of spend has not caught up past the end of this
	// session, so ModelTokens is not trustworthy. We CANNOT PROVE the model
	// answered, therefore we do not escalate. INFRA. (gonk-gate forces a spend
	// sync and waits on a predicate before setting this -- see cmd/gonk-gate/sweep.go.)
	SpendStale bool

	// ArtifactPresent: the bot's comment carrying this bead's marker is on the
	// GitLab issue. This is the GATE, and it is a fact, not a judgement.
	ArtifactPresent bool

	// ModelTokens: total tokens this session consumed, per meter's ledger. The
	// PROOF that the model answered. Zero means it never did.
	ModelTokens int64
}

// Classify is total: every Signals value maps to exactly one outcome. The order
// of the checks IS the priority order, and it is deliberate.
func Classify(s Signals) string {
	// 1. Abort wins outright. A human said stop; nothing about the work matters.
	if s.Aborted {
		return meterapi.OutcomeAborted
	}
	// 2. Any infra indicator wins over any judgement about the work. We refuse to
	//    convert our own outage into somebody's escalation.
	if s.ReservationExpired || s.ArtifactUnknown || s.SpendStale {
		return meterapi.OutcomeInfraFailed
	}
	// 3. The artifact is there. Done. (Even at zero tokens -- the work exists, and
	//    success costs nobody a rung.)
	if s.ArtifactPresent {
		return meterapi.OutcomeSuccess
	}
	// 4. No artifact, and we cannot prove the model ever answered. It did not FAIL
	//    at the work; it never got to do the work. Retry the same rung.
	//    THIS IS THE LINE THAT MAKES "infra failures never escalate" TRUE for pod
	//    evictions, cold-start timeouts and LiteLLM 5xx, none of which announce
	//    themselves any other way.
	if s.ModelTokens <= 0 {
		return meterapi.OutcomeInfraFailed
	}
	// 5. The model answered, and the work is not there. THIS, and only this, is a
	//    gate failure, and only this buys a rung.
	return meterapi.OutcomeGateFailed
}

// Escalates reports whether an outcome buys the next rung on the ladder. It is a
// one-line function guarding the entire cost model. Meter enforces the same rule
// independently (Plan 03's rung.Outcome.Escalates) -- two agreeing enforcers, on
// purpose.
func Escalates(outcome string) bool { return outcome == meterapi.OutcomeGateFailed }
```

**Known false positive — record it, do not hide it.** A pod evicted *after* at
least one successful completion but *before* posting its comment classifies as
`gate-failed` and buys **one** unearned escalation. It is bounded (one rung, one
attempt) and the escalated attempt still passes `/decide`, so it cannot exceed
budget. Closing it properly needs a **pod-termination signal from Gas City's
session provider**, which is not in the facts available to this plan.
**Hand-off:** Plan 06 should measure how often it fires (K-series kill tests
already evict pods); if it is common, it becomes an upstream ask. Put this
paragraph in ADR-004 verbatim.

- [x] **Step 4: Implement `pkg/gate/marker.go`**

```go
package gate

import "strings"

// BeadMarker is the machine-readable token the triage/scaffold/mention agents
// MUST embed in the comment they post. It is how a deterministic gate knows the
// artifact landed WITHOUT an LLM judging prose (spec 6.3: no LLM judges).
//
// IT IS LOAD-BEARING. cmd/gonk-gate's `check` and `sweep` grep for it. If a
// prompt rewrite drops it, the gate goes blind: every successful session is
// classified `gate-failed`, every project climbs its ladder to the most expensive
// rung, and the bill arrives before the bug report. See OD-4.
func BeadMarker(beadID string) string { return "<!-- gonk:bead:" + beadID + " -->" }

// MarkerPresent reports whether body carries THIS bead's marker. An empty beadID
// matches nothing -- a zero value must never satisfy a gate.
func MarkerPresent(body, beadID string) bool {
	if beadID == "" {
		return false
	}
	return strings.Contains(body, BeadMarker(beadID))
}
```

- [x] **Step 5: Run the tests**

Run: `go test ./pkg/gate/ -race -count=1 -v`
Expected: PASS (5 tests, ~30 subtests).

Then prove the negative — there is no model call and no judgement in here:

```bash
grep -rniE "openai|litellm|prompt|llm|model\." pkg/gate/ && echo "FAIL: a judge crept into the classifier" || echo "ok"
```

Expected: `ok`.

- [x] **Step 6: Commit**

```bash
gofmt -l . && go vet ./pkg/gate/ && go test ./pkg/gate/ -race -count=1
git add pkg/gate
git commit -m "feat(gate): deterministic outcome classifier -- infra failures never escalate a rung"
```

---

### Task 2: `pkg/gcapi` — the Gas City supervisor client (this closes Plan 02's OD-A)

Plan 02 shipped `intake.HTTPDispatcher` as an **explicit placeholder**: *"the real
supervisor endpoint and payload are not in the spec… Plan 04 must reconcile
`HTTPDispatcher` with Gas City's actual contract."* **This task is that
reconciliation.**

The real contract (from `docs/tutorials/07-orders.md` and the supervisor source):

```
POST /v0/city/{cityName}/order/{name}/run     body: {"vars": {k: v}}
200                                           -> {"status": ..., "scoped_name": ..., "tracking_id": ...}
```

**Note what is NOT there: any authentication.** The order-run route has **no
per-route auth scheme — admission is by network position.** That is a fact about
upstream, not a choice of ours, and it has a consequence the plan must state:
**anything that can reach the supervisor's port can pour a formula.** What stops
that from being a spend bug is that the poured formula still has to pass
`gonk-gate dispatch`'s `/decide` — Gate 2 — which is exactly why Gate 2 is the
enforcement point and not an optimization. **The supervisor must never be exposed
beyond the namespace** (spec §9; Plan 05 already keeps it internal, and gonk's
Ingress exposes only `/hook/gitlab`).

**Files:**
- Create: `pkg/gcapi/client.go`, `pkg/gcapi/gcapitest/server.go`
- Test: `pkg/gcapi/client_test.go`

- [x] **Step 1: Write the failing test** — `pkg/gcapi/client_test.go`

```go
package gcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "gonk-city")
	c.RetryBackoff = func(int) time.Duration { return 0 }
	return c
}

func TestRunOrderPostsToTheRealRoute(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"status":"queued","scoped_name":"gonk-city/gonk-dispatch","tracking_id":"trk-1"}`)
	}))

	res, err := c.RunOrder(context.Background(), "gonk-dispatch", map[string]string{
		"bead_anchor": "gonk:42:issue:3",
		"trigger":     "issue-triage",
	})
	if err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	if gotPath != "/v0/city/gonk-city/order/gonk-dispatch/run" {
		t.Fatalf("path = %q", gotPath)
	}
	vars, _ := gotBody["vars"].(map[string]any)
	if vars["bead_anchor"] != "gonk:42:issue:3" || vars["trigger"] != "issue-triage" {
		t.Fatalf("vars = %+v", vars)
	}
	if res.TrackingID != "trk-1" || res.ScopedName != "gonk-city/gonk-dispatch" {
		t.Fatalf("res = %+v", res)
	}
}

// The city name is part of the ROUTE. A wrong one is a 404 on every dispatch, and
// an EMPTY one silently builds "/v0/city//order/x/run", which is a different route
// that may 404 or may match something else. Refuse at construction.
func TestEmptyCityIsRefused(t *testing.T) {
	if _, err := New("http://x", "").RunOrder(context.Background(), "o", nil); err == nil {
		t.Fatal("an empty city name must be an error, not a URL with an empty path segment")
	}
}

// A name with a slash would escape the route. Escape it, do not concatenate.
func TestOrderNameIsEscaped(t *testing.T) {
	var gotPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		fmt.Fprint(w, `{"status":"queued"}`)
	}))
	_, _ = c.RunOrder(context.Background(), "a/b", nil)
	if strings.Contains(gotPath, "/a/b/") {
		t.Fatalf("order name was not escaped: %q", gotPath)
	}
}

func TestRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"status":"queued","tracking_id":"t"}`)
	}))
	if _, err := c.RunOrder(context.Background(), "gonk-dispatch", nil); err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestDoesNotRetry4xx(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	_, err := c.RunOrder(context.Background(), "nope", nil)
	if err == nil {
		t.Fatal("want error")
	}
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want IsNotFound (a 404 here almost always means a wrong GONK_CITY -- OD-1)", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

// Vars are strings on the wire. A nil map must send `{"vars":{}}`, not
// `{"vars":null}` -- a null could deserialize differently upstream.
func TestNilVarsSendsEmptyObject(t *testing.T) {
	var raw string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		raw = string(b[:n])
		fmt.Fprint(w, `{"status":"queued"}`)
	}))
	_, _ = c.RunOrder(context.Background(), "o", nil)
	if !strings.Contains(raw, `"vars":{}`) {
		t.Fatalf("body = %q, want vars to be an empty object", raw)
	}
}
```

- [x] **Step 2: Run it and watch it fail**

Run: `go test ./pkg/gcapi/ -v`
Expected: FAIL — undefined `New`, `Client`, `RunOrder`, `IsNotFound`.

- [x] **Step 3: Implement `pkg/gcapi/client.go`**

```go
// Package gcapi is a minimal typed client for the ONE Gas City supervisor route
// gonk uses: firing an order.
//
//	POST /v0/city/{cityName}/order/{name}/run   {"vars": {k: v}}
//	                                        ->  {status, scoped_name, tracking_id}
//
// (Source: gascity's docs/tutorials/07-orders.md and the supervisor's route
// table. gascity is MIT. NOTHING in this package is derived from gascity-packs,
// which is all-rights-reserved -- see the licensing constraint at the top of
// Plan 04.)
//
// SECURITY NOTE, and it is not a small one: THE ORDER-RUN ROUTE HAS NO
// PER-ROUTE AUTH SCHEME. Admission to it is by NETWORK POSITION. Anything that
// can reach the supervisor's port (:8372) can pour a formula. Two consequences,
// both of which the rest of gonk is built around:
//
//  1. The supervisor MUST NOT be exposed beyond the namespace (spec 9). gonk's
//     Ingress exposes /hook/gitlab and nothing else (Plan 05).
//  2. Pouring a formula is NOT the same as spending money. Every pour goes
//     through `gonk-gate dispatch`, which asks gonk-meter /v1/policy/decide
//     first. That is Gate 2, and this is precisely why it is the enforcement
//     point rather than an optimization.
package gcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultPort is the supervisor's REST/SSE port.
const DefaultPort = 8372

type Client struct {
	BaseURL      string // e.g. http://gascity.gascity.svc:8372
	City         string // OD-1: from GONK_CITY. NO DEFAULT. An empty city is an error.
	HTTP         *http.Client
	MaxRetries   int
	RetryBackoff func(attempt int) time.Duration
	UserAgent    string
}

func New(baseURL, city string) *Client {
	return &Client{
		BaseURL:      strings.TrimSuffix(baseURL, "/"),
		City:         city,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		MaxRetries:   3,
		RetryBackoff: func(a int) time.Duration { return time.Duration(1<<a) * 250 * time.Millisecond },
		UserAgent:    "gonk-gate",
	}
}

// RunResult is the supervisor's answer.
type RunResult struct {
	Status     string `json:"status"`
	ScopedName string `json:"scoped_name"`
	TrackingID string `json:"tracking_id"`
}

type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gascity: POST %s: %d: %s", e.Path, e.Status, e.Body)
}

// IsNotFound: on the order-run route a 404 nearly always means the CITY NAME is
// wrong (OD-1) or the order is not in the loaded pack. Both are configuration
// errors, and both are loud -- which is the failure mode we chose deliberately
// over guessing a default city name.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

type runBody struct {
	// Vars is never nil on the wire: a nil map marshals to `null`, and `{}` and
	// `null` are not the same document.
	Vars map[string]string `json:"vars"`
}

// RunOrder fires one order. Vars become the order's declared [order.params],
// which Gas City namespaces into an exec order's environment as GC_WEBHOOK_ARG_*.
func (c *Client) RunOrder(ctx context.Context, name string, vars map[string]string) (*RunResult, error) {
	if c.City == "" {
		return nil, errors.New("gascity: city name is empty (set GONK_CITY -- there is no default, and an empty one builds a route that silently is not the one you meant)")
	}
	if name == "" {
		return nil, errors.New("gascity: order name is empty")
	}
	if vars == nil {
		vars = map[string]string{}
	}
	path := fmt.Sprintf("/v0/city/%s/order/%s/run", url.PathEscape(c.City), url.PathEscape(name))
	payload, err := json.Marshal(runBody{Vars: vars})
	if err != nil {
		return nil, fmt.Errorf("gascity: encode vars: %w", err)
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", c.UserAgent)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("gascity: POST %s: %w", path, err)
		} else {
			body, rerr := readCapped(resp.Body, 1<<16)
			_ = resp.Body.Close()
			switch {
			case rerr != nil:
				return nil, fmt.Errorf("gascity: POST %s: %w", path, rerr)
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				var out RunResult
				if err := json.Unmarshal(body, &out); err != nil {
					return nil, fmt.Errorf("gascity: POST %s: decode: %w", path, err)
				}
				return &out, nil
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				lastErr = &APIError{Status: resp.StatusCode, Path: path, Body: truncate(body)}
			default:
				return nil, &APIError{Status: resp.StatusCode, Path: path, Body: truncate(body)}
			}
		}
		if attempt >= c.MaxRetries {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.RetryBackoff(attempt)):
		}
	}
}
```

Add the two helpers (`readCapped`, `truncate`) with the same semantics as
`pkg/glab/client.go` (Plan 02, Task 1) — cap the body, error rather than
truncate, never embed a credential in an error string.

- [x] **Step 4: Implement `pkg/gcapi/gcapitest/server.go`**

An in-memory supervisor for the rest of the plan's tests. Behaviour contract:

```go
// Package gcapitest is an in-memory Gas City supervisor: enough of the order-run
// route to drive gonk-gate's tests with no city, no cluster, no containers.
package gcapitest

// Server records every order poured against it, so a test can assert exactly what
// was fired and with which vars -- which is how Task 3 proves the re-sling path.
type Server struct {
	Poured []Pour       // append-only, in order
	Fail   map[string]int // order name -> remaining 5xx responses to emit
}

type Pour struct {
	Order string
	Vars  map[string]string
}

func New(t *testing.T) *Server           // httptest.NewServer, t.Cleanup'd
func (s *Server) URL() string
func (s *Server) Client(city string) *gcapi.Client
func (s *Server) PouredNames() []string  // convenience for assertions
```

- [x] **Step 5: Run the tests**

Run: `go test ./pkg/gcapi/... -race -count=1 -v`
Expected: PASS (6 tests).

- [x] **Step 6: Commit**

```bash
gofmt -l . && go vet ./pkg/gcapi/... && go test ./pkg/gcapi/... -race -count=1
git add pkg/gcapi
git commit -m "feat(gcapi): Gas City supervisor order-run client (closes Plan 02's OD-A)"
```

---

### Task 3: `cmd/gonk-gate` — the deterministic gate binary

**This is Gate 2, the sweeper, the `[steps.check]` verifier, and the trailer
generator, in one static binary with no model call in it.** It ships in both the
controller image (Task 6) and the agent image (Task 5).

**Files:**
- Create: `pkg/beadstore/store.go`, `pkg/beadstore/memory.go`, `pkg/beadstore/bd.go`
- Create: `cmd/gonk-gate/main.go`, `cmd/gonk-gate/dispatch.go`, `cmd/gonk-gate/sweep.go`, `cmd/gonk-gate/check.go`
- Test: `pkg/beadstore/store_test.go`, `cmd/gonk-gate/dispatch_test.go`, `cmd/gonk-gate/sweep_test.go`, `cmd/gonk-gate/check_test.go`

(`cmd/gonk-gate/trailers.go` is Task 8.)

**The exit-code contract** — the orders' shell wrappers depend on it, so it is
part of the pack's contract, not an implementation detail:

| Code | Meaning | Order script reaction |
|---|---|---|
| `0` | The command did what it was asked. (For `dispatch`: **poured, parked, or denied** — all three are *successful decisions*.) | success |
| `1` | Infrastructure error: meter unreachable, supervisor 5xx, bead store failure. | order fails; Gas City retries per its own policy |
| `2` | **Misconfiguration** — `GONK_CITY` unset, no meter token file, etc. Never retry; a human must fix it. | order fails loudly |
| `3` | `check` only: **the artifact is not there yet.** The `[steps.check]` loop keeps polling. | keep polling |

**Note that `defer` and `deny` are exit `0`.** They are normal answers
(`meterapi`: *"defer and deny are HTTP 200"*). An order that "fails" on a `defer`
would make quiet hours look like an outage on every dashboard in the building.

- [x] **Step 1: Write `pkg/beadstore` and its failing test**

`pkg/beadstore/store_test.go`:

```go
package beadstore

import (
	"context"
	"testing"
	"time"
)

func TestMemoryRoundTrip(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	rec := Record{
		BeadAnchor:    "gonk:42:issue:3",
		BeadID:        "gk-1a2b",
		Project:       "group/repo",
		Rig:           "group-repo",
		SessionKey:    "gonk-42-issue-3",
		Trigger:       "issue-triage",
		IssueIID:      3,
		ProjectID:     42,
		State:         StateRunning,
		Rung:          "qwen-local",
		Attempt:       1,
		ReservationID: "rsv-1",
	}
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	got, ok, err := s.Get(ctx, "gonk:42:issue:3")
	if err != nil || !ok || got.BeadID != "gk-1a2b" || got.State != StateRunning {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
}

// The sweeper's whole job is "find the beads that need me". Two queries, and they
// must not overlap: a running bead is not a parked bead.
func TestListByState(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	now := time.Now()
	_ = s.Put(ctx, Record{BeadAnchor: "a", State: StateRunning})
	_ = s.Put(ctx, Record{BeadAnchor: "b", State: StateParked, RetryAfter: now.Add(-time.Minute)})
	_ = s.Put(ctx, Record{BeadAnchor: "c", State: StateParked, RetryAfter: now.Add(time.Hour)})
	_ = s.Put(ctx, Record{BeadAnchor: "d", State: StateNeedsHuman})
	_ = s.Put(ctx, Record{BeadAnchor: "e", State: StateDone})

	running, err := s.List(ctx, StateRunning)
	if err != nil || len(running) != 1 || running[0].BeadAnchor != "a" {
		t.Fatalf("running = %+v, %v", running, err)
	}
	parked, err := s.List(ctx, StateParked)
	if err != nil || len(parked) != 2 {
		t.Fatalf("parked = %+v, %v", parked, err)
	}
	// A DONE or NEEDS-HUMAN bead must never come back to the sweeper: needs-human
	// means a human, and re-sweeping it would spend money on a bead we already
	// gave up on (meterapi: `deny` / ladder-exhausted).
	for _, st := range []State{StateDone, StateNeedsHuman} {
		got, _ := s.List(ctx, StateRunning)
		for _, r := range got {
			if r.State == st {
				t.Fatalf("%s bead came back as running", st)
			}
		}
	}
}

// Put is an UPSERT keyed on BeadAnchor. Firing the same order twice (a duplicate
// webhook delivery, an intake restart mid-flight) must not create a second work
// record -- that is Plan 02's BeadAnchor idempotency carried into the pack, and
// Plan 06's K13/K18 rest on it.
func TestPutIsIdempotentOnBeadAnchor(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	_ = s.Put(ctx, Record{BeadAnchor: "gonk:1:issue:1", Attempt: 1, State: StateRunning})
	_ = s.Put(ctx, Record{BeadAnchor: "gonk:1:issue:1", Attempt: 2, State: StateRunning})
	all, _ := s.List(ctx, StateRunning)
	if len(all) != 1 {
		t.Fatalf("got %d records, want 1 -- BeadAnchor is the idempotency key and a duplicate bead is DUPLICATE SPEND", len(all))
	}
	if all[0].Attempt != 2 {
		t.Fatalf("attempt = %d, want the upsert to win", all[0].Attempt)
	}
}
```

- [x] **Step 2: Implement `pkg/beadstore`**

```go
// Package beadstore is gonk-gate's per-bead work record: the small amount of
// state the gate needs between a dispatch and its outcome.
//
// It lives BEHIND AN INTERFACE with two implementations (AD-2):
//
//	Memory -- in-memory. EVERY test in cmd/gonk-gate runs against this. No Dolt,
//	          no containers, no `bd` binary.
//	BdCLI  -- shells the MIT `bd` binary against the shared Dolt bead store
//	          (spec 4.2). Its exact subcommands are CONFIRMED AGAINST THE REAL
//	          `bd` in Task 6's container smoke test, not guessed at here.
//
// That split is deliberate: it confines the one thing this plan cannot verify
// offline (bd's CLI surface) to a single file, and leaves the gate's entire
// decision logic testable with `go test`.
package beadstore

import (
	"context"
	"sync"
	"time"
)

type State string

const (
	// StateRunning: a formula was poured; we are waiting for the session to finish.
	StateRunning State = "running"
	// StateParked: meter said `defer` (budget exhausted, quiet hours, capacity).
	// RetryAfter says when to ask again. THIS IS A NORMAL STATE, NOT AN ERROR.
	StateParked State = "parked"
	// StateNeedsHuman: meter said `deny` (ladder exhausted, action not allowed,
	// project not registered). We stop. The sweeper must NEVER pick these up
	// again -- re-sweeping a denied bead is how you spend money on work you
	// already decided not to do.
	StateNeedsHuman State = "needs-human"
	// StateDone: the gate passed.
	StateDone State = "done"
)

// Record is one work item. BeadAnchor is the IDEMPOTENCY KEY (Plan 02): firing
// the same order twice must not create a second bead, because a second bead is
// a second session and a second session is duplicate spend.
type Record struct {
	BeadAnchor string // "gonk:{project_id}:issue:{iid}" -- the key
	BeadID     string
	Project    string
	ProjectID  int64
	Rig        string
	SessionKey string
	Trigger    string
	IssueIID   int64
	ConfigHash string

	State         State
	Rung          string
	Model         string
	Attempt       int
	ReservationID string
	RetryAfter    time.Time
	SessionEndedAt time.Time
	UpdatedAt     time.Time
}

type Store interface {
	// Put upserts on BeadAnchor.
	Put(ctx context.Context, r Record) error
	Get(ctx context.Context, beadAnchor string) (Record, bool, error)
	List(ctx context.Context, state State) ([]Record, error)
}

// Memory is the test implementation and the reference semantics.
type Memory struct {
	mu   sync.Mutex
	recs map[string]Record
}

func NewMemory() *Memory { return &Memory{recs: map[string]Record{}} }

func (m *Memory) Put(_ context.Context, r Record) error { /* upsert on r.BeadAnchor, stamp UpdatedAt */ }
func (m *Memory) Get(_ context.Context, k string) (Record, bool, error) { /* ... */ }
func (m *Memory) List(_ context.Context, st State) ([]Record, error) { /* filter, sorted by BeadAnchor for determinism */ }
```

`pkg/beadstore/bd.go` — the `BdCLI` impl. It shells `bd` and stores the `Record`
as a single structured comment on the bead (`<!-- gonk-state {json} -->`) plus a
`gonk::<state>` label so a human can see it in the beads UI.
**Do not guess the subcommands.** Task 6 Step 5 runs `bd --help` inside the
controller image and pins the exact invocations. Until then `bd.go` is written
against `bd comment` / `bd label` / `bd list --label`, and **a wrong guess changes
exactly this one file.**

- [x] **Step 3: Write the failing dispatch test** — `cmd/gonk-gate/dispatch_test.go`

```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// fakeMeter serves /v1/policy/decide with a canned answer and records what it was
// asked. It is deliberately dumb: the POINT of these tests is what gonk-gate DOES
// with an answer, not how meter computes one (that is Plan 03's exhaustive table).
type fakeMeter struct {
	resp     meterapi.DecideResponse
	lastBody []byte
	calls    int
}

func (f *fakeMeter) server(t *testing.T) string { /* httptest, records body, serves f.resp */ }

func TestDispatchPoursOnRun(t *testing.T) {
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "some-model-from-the-catalog",
		Attempt: 1, ReservationID: "rsv-1",
		Metadata: map[string]string{"gonk_project": "group/repo", "gonk_rung": "cheap"},
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
	}}

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Args: dispatchArgs{
			Project: "group/repo", ProjectID: 42, Rig: "group-repo", IssueIID: 3,
			BeadAnchor: "gonk:42:issue:3", BeadID: "gk-1a2b",
			SessionKey: "gonk-42-issue-3", Trigger: "issue-triage",
		},
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}

	// It poured the TRIGGER'S formula-order, exactly once.
	if len(gc.Poured) != 1 || gc.Poured[0].Order != "gonk-triage" {
		t.Fatalf("poured = %+v, want one gonk-triage", gc.Poured)
	}
	// And it handed the formula EXACTLY what meter said -- the rung, the model, the
	// reservation, the key_ref, and atags VERBATIM. Meter mints the metadata; the
	// pack stamps it. The pack must never construct a tag itself.
	v := gc.Poured[0].Vars
	if v["rung"] != "cheap" || v["model"] != "some-model-from-the-catalog" || v["reservation_id"] != "rsv-1" {
		t.Fatalf("vars = %+v", v)
	}
	if v["key_secret_name"] != "gonk-key-abc" || v["key_secret_key"] != "LITELLM_API_KEY" {
		t.Fatalf("key_ref not passed through: %+v", v)
	}
	var md map[string]string
	if err := json.Unmarshal([]byte(v["metadata_json"]), &md); err != nil || md["gonk_rung"] != "cheap" {
		t.Fatalf("metadata not stamped verbatim: %q", v["metadata_json"])
	}
	// The key_ref is a POINTER. If the key MATERIAL is anywhere in these vars, it is
	// now in the event bus and in every log line that echoes an order.
	for k, val := range v {
		if val == "sk-secret" {
			t.Fatalf("key material leaked into order var %q", k)
		}
	}

	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateRunning || rec.Rung != "cheap" || rec.Attempt != 1 {
		t.Fatalf("record = %+v", rec)
	}
}

// A `defer` is a NORMAL ANSWER. It parks the bead and exits 0. If it exited
// non-zero, every project's quiet hours would light up the dashboards as an
// outage, and people would learn to ignore red.
func TestDispatchParksOnDefer(t *testing.T) {
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	retry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours,
		Detail: "quiet hours until 07:00", RetryAfter: retry, Attempt: 1,
	}}

	code := runDispatch(context.Background(), dispatchDeps{ /* ... as above ... */ })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 -- a defer is a normal answer, not a failure", code)
	}
	if len(gc.Poured) != 0 {
		t.Fatal("a deferred bead must NOT pour a formula -- that is the whole point of the gate")
	}
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateParked || !rec.RetryAfter.Equal(retry) {
		t.Fatalf("record = %+v, want parked with retry_after", rec)
	}
}

func TestDispatchStopsOnDeny(t *testing.T) {
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionDeny, Reason: meterapi.ReasonLadderExhausted,
		Detail: "2 rungs, 2 gate failures", Attempt: 3,
	}}
	code := runDispatch(context.Background(), dispatchDeps{ /* ... */ })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(gc.Poured) != 0 {
		t.Fatal("a denied bead must not pour")
	}
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateNeedsHuman {
		t.Fatalf("state = %q, want needs-human (a human must look at a ladder-exhausted bead)", rec.State)
	}
}

// *** THE TEST THAT PREVENTS THE CATASTROPHIC DRIFT. DO NOT DELETE IT. ***
//
// intake already called /decide and passed a rung in the order vars. The exec
// order must IGNORE that and ask meter AGAIN. If it ever trusts the vars, then a
// controller-initiated re-sling (which never passes intake at all) runs at the
// OLD rung with the OLD reservation and NO budget check -- and escalations spend
// unmetered. See "The two call sites MUST agree" at the top of this plan.
func TestDispatchAlwaysDecidesEvenWhenVarsCarryARung(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "expensive", Model: "m2", Attempt: 2, ReservationID: "rsv-2",
		Metadata: map[string]string{"gonk_rung": "expensive"},
	}}
	args := dispatchArgs{ /* ... */ }
	args.Rung = "cheap"          // intake's hint, from a PREVIOUS decision
	args.ReservationID = "rsv-1" // stale
	args.Model = "m1"            // stale

	code := runDispatch(context.Background(), dispatchDeps{Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(), Args: args})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if fm.calls != 1 {
		t.Fatalf("meter /decide called %d times, want exactly 1 -- THE GATE MUST ALWAYS ASK", fm.calls)
	}
	v := gc.Poured[0].Vars
	if v["rung"] != "expensive" || v["reservation_id"] != "rsv-2" || v["model"] != "m2" {
		t.Fatalf("the gate used the STALE order vars instead of meter's live answer: %+v\n"+
			"This is the bug that makes ladder escalations spend unmetered.", v)
	}
}

// Meter owns ladder state (Plan 03, Decision 2). A caller-supplied attempt is a
// forgery vector: raise it and you skip straight to the most expensive rung.
func TestDispatchNeverSendsAnAttempt(t *testing.T) {
	fm := &fakeMeter{resp: meterapi.DecideResponse{Decision: meterapi.DecisionDefer, Attempt: 1, RetryAfter: time.Now().Add(time.Hour)}}
	_ = runDispatch(context.Background(), dispatchDeps{ /* ... */ })

	var raw map[string]any
	if err := json.Unmarshal(fm.lastBody, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, forbidden := range []string{"attempt", "outcome", "rung", "prior_outcomes"} {
		if _, ok := raw[forbidden]; ok {
			t.Fatalf("DecideRequest carried %q -- meter owns ladder state; sending it is a forgery vector for climbing the ladder", forbidden)
		}
	}
}

// If meter is unreachable we do NOT pour. No unmetered work, ever -- the same
// fail-closed rule intake follows.
func TestDispatchFailsClosedWhenMeterIsDown(t *testing.T) {
	gc := gcapitest.New(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)

	code := runDispatch(context.Background(), dispatchDeps{Meter: meterClient(down.URL), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(), Args: dispatchArgs{ /* ... */ }})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (infra error)", code)
	}
	if len(gc.Poured) != 0 {
		t.Fatal("METER WAS DOWN AND WE POURED A FORMULA ANYWAY. That is unmetered spend.")
	}
}

// OD-1: no default city. A missing GONK_CITY is a misconfiguration, exit 2, and it
// must never be papered over with a guess.
func TestDispatchRefusesWithoutCity(t *testing.T) {
	code := runDispatch(context.Background(), dispatchDeps{ /* GC built with city "" */ })
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (misconfiguration)", code)
	}
}
```

- [x] **Step 4: Run it and watch it fail**

Run: `go test ./cmd/gonk-gate/ -run Dispatch -v`
Expected: FAIL — undefined `runDispatch`, `dispatchDeps`, `dispatchArgs`.

- [x] **Step 5: Implement `cmd/gonk-gate/dispatch.go`**

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// orderForTrigger maps a trigger to the formula-order that runs it. This is the
// ONLY mapping from work to formula, and it is a closed set: an unknown trigger
// pours nothing.
var orderForTrigger = map[string]string{
	"issue-triage":  "gonk-triage",
	"scaffold":      "gonk-scaffold",
	"mention-reply": "gonk-mention",
	// NOTE: "onboarding" is deliberately absent. The onboarding MR is
	// DETERMINISTIC AND ZERO-TOKEN (spec 5.3) -- intake writes it directly. There
	// is no formula, no agent and no rung for it, and adding one would put an LLM
	// on the one path that is specified not to have one.
}

type dispatchArgs struct {
	Project   string
	ProjectID int64
	Rig       string
	IssueIID  int64
	// BeadAnchor is the deterministic, re-sling-stable identifier
	// (`gonk:{project_id}:issue:{iid}`). IT is what goes on the wire as
	// meterapi.DecideRequest.BeadID -- the budget/ladder/reservation key, identical
	// to the value intake sent at Gate 1.
	BeadAnchor string
	// BeadID is the Gas City internal bead id (e.g. gk-1a2b). It exists by Gate 2
	// (intake's order created the bead) and is used for the comment marker and the
	// bead-store record -- but it MUST NOT be sent to meter: it differs between the
	// gates and is not stable across re-slings.
	BeadID     string
	SessionKey string
	Trigger    string
	ConfigHash string

	// Rung / Model / ReservationID arrive from intake's own /decide (Gate 1).
	// THEY ARE A HINT FOR LOGS AND NOTHING ELSE. runDispatch re-decides and uses
	// meter's live answer. See TestDispatchAlwaysDecidesEvenWhenVarsCarryARung.
	Rung          string
	Model         string
	ReservationID string
}

type dispatchDeps struct {
	Meter *meterapi.Client // Plan 03's client, or a thin wrapper over it
	GC    *gcapi.Client
	Store beadstore.Store
	Log   *slog.Logger
	Args  dispatchArgs
}

// runDispatch is GATE 2: the only code path in gonk that pours a formula.
//
// It ALWAYS asks gonk-meter /v1/policy/decide first -- on the first dispatch, on
// a re-sling after a gate failure, on an unpark after a defer, on every single
// pour without exception. There is no fast path, no cache, and no "intake already
// checked". A ladder escalation is controller-initiated and never passes intake,
// so if this function trusts its inputs, escalations spend unmetered.
//
// Exit codes: 0 = a decision was made and acted on (run|defer|deny); 1 = infra;
// 2 = misconfiguration.
func runDispatch(ctx context.Context, d dispatchDeps) int {
	a := d.Args

	order, ok := orderForTrigger[a.Trigger]
	if !ok {
		d.Log.Error("unknown trigger; pouring nothing", "trigger", a.Trigger)
		return 2
	}
	if d.GC.City == "" {
		d.Log.Error("GONK_CITY is unset; refusing to guess a city name (OD-1)")
		return 2
	}

	// ---- THE GATE. Every pour. No exceptions. -------------------------------
	// NOTE what is NOT in this request: an attempt count. Meter owns ladder state
	// (Plan 03, Decision 2); a caller-supplied attempt is a forgery vector.
	//
	// bead_id IS THE BeadAnchor -- the SAME value intake sent at Gate 1, NOT the Gas
	// City bead id (a.BeadID). Meter keys /decide idempotency, ladder state and the
	// reservation on bead_id; if Gate 1 sent the BeadAnchor and Gate 2 sent the Gas
	// City bead id, meter would see two different beads for one work item and mint a
	// SECOND reservation -- double budget headroom, and Gate 1's reservation leaks
	// until its TTL. The Gas City bead id stays in a.BeadID for the marker/record,
	// but it never reaches meter. See the cross-plan BeadAnchor contract.
	dec, err := d.Meter.Decide(ctx, meterapi.DecideRequest{
		Project:    a.Project,
		Rig:        a.Rig,
		BeadID:     a.BeadAnchor, // bead_id == BeadAnchor, the same value Gate 1 sent
		SessionKey: a.SessionKey,
		Trigger:    a.Trigger,
	})
	if err != nil {
		// FAIL CLOSED. An unreachable budget enforcer is not permission to spend.
		d.Log.Error("meter /decide failed; pouring nothing", "err", err, "bead", a.BeadAnchor)
		return 1
	}

	base := beadstore.Record{
		BeadAnchor: a.BeadAnchor, BeadID: a.BeadID, Project: a.Project, ProjectID: a.ProjectID,
		Rig: a.Rig, SessionKey: a.SessionKey, Trigger: a.Trigger, IssueIID: a.IssueIID,
		ConfigHash: a.ConfigHash, Attempt: dec.Attempt,
	}

	switch dec.Decision {
	case meterapi.DecisionDefer:
		// A NORMAL ANSWER (meterapi: defer and deny are HTTP 200). Quiet hours and
		// budget exhaustion both land here. Park it; gonk-sweep unparks it at
		// RetryAfter -- by calling THIS function again, which decides again.
		base.State = beadstore.StateParked
		base.RetryAfter = dec.RetryAfter
		if err := d.Store.Put(ctx, base); err != nil {
			return 1
		}
		d.Log.Info("bead parked", "bead", a.BeadAnchor, "reason", dec.Reason,
			"detail", dec.Detail, "retry_after", dec.RetryAfter)
		return 0

	case meterapi.DecisionDeny:
		// The ladder is exhausted, or the action is not allowed, or the project is
		// not registered. Stop. A human decides what happens next; the sweeper
		// never picks a needs-human bead up again.
		base.State = beadstore.StateNeedsHuman
		if err := d.Store.Put(ctx, base); err != nil {
			return 1
		}
		d.Log.Warn("bead denied", "bead", a.BeadAnchor, "reason", dec.Reason, "detail", dec.Detail)
		return 0

	case meterapi.DecisionRun:
		// fall through

	default:
		d.Log.Error("meter returned an unknown decision; pouring nothing", "decision", dec.Decision)
		return 1
	}

	// ---- Pour, with METER'S answer. Never with the caller's. ----------------
	md, err := json.Marshal(dec.Metadata)
	if err != nil {
		return 1
	}
	vars := map[string]string{
		"project":     a.Project,
		"project_id":  fmt.Sprint(a.ProjectID),
		"rig":         a.Rig,
		"issue_iid":   fmt.Sprint(a.IssueIID),
		"bead_anchor": a.BeadAnchor,
		"bead_id":     a.BeadID,
		"session_key": a.SessionKey,
		"trigger":     a.Trigger,
		"config_hash": a.ConfigHash,

		// From meter, verbatim. The pack does not choose a rung, does not choose a
		// model, and does not mint a tag.
		"rung":           dec.Rung,
		"model":          dec.Model,
		"attempt":        fmt.Sprint(dec.Attempt),
		"reservation_id": dec.ReservationID,
		"metadata_json":  string(md),

		// A POINTER to the key. NEVER the key. A credential in an order var is a
		// credential in the event bus and in every log line that echoes it.
		"key_secret_name": dec.KeyRef.SecretName,
		"key_secret_key":  dec.KeyRef.SecretKey,
	}

	if _, err := d.GC.RunOrder(ctx, order, vars); err != nil {
		if gcapi.IsNotFound(err) {
			// Almost always a wrong GONK_CITY (OD-1) or a pack that did not load.
			d.Log.Error("supervisor 404 -- check GONK_CITY and that the pack loaded", "err", err)
			return 2
		}
		d.Log.Error("pour failed", "order", order, "err", err)
		return 1
	}

	base.State = beadstore.StateRunning
	base.Rung, base.Model, base.ReservationID = dec.Rung, dec.Model, dec.ReservationID
	if err := d.Store.Put(ctx, base); err != nil {
		return 1
	}
	d.Log.Info("poured", "order", order, "bead", a.BeadAnchor, "rung", dec.Rung, "attempt", dec.Attempt)
	return 0
}

var errNoCity = errors.New("GONK_CITY is unset")
```

- [x] **Step 6: Write and implement `cmd/gonk-gate/sweep.go`** — the classifier's driver

`sweep` is the **cooldown exec order's** body. It runs every 30 s and does three
things, all deterministic:

1. **Classify every `running` bead whose session has ended.** Gather `gate.Signals`:
   - `ArtifactPresent` / `ArtifactUnknown` ← GitLab: list the issue's notes, find one
     authored by the bot containing `gate.BeadMarker(beadID)`. A GitLab error sets
     **`ArtifactUnknown`**, not "absent" — we never convert our own outage into
     somebody's escalation.
   - `ModelTokens` / `SpendStale` ← meter. **Force a spend sync first**
     (`POST /admin/spend/sync`, Plan 03's HB-2 endpoint), then poll
     `GET /v1/cost/session/{key}` until `spend_as_of >= SessionEndedAt`, with a
     bounded deadline (default 60 s). **On deadline, set `SpendStale: true`** — which
     classifies `infra-failed`, which does **not** escalate. *Uncertainty never
     escalates* (AD-6).
   - `ReservationExpired` ← `DecideResponse.ReservationExpiresAt` from the record vs now.
   - `Aborted` ← the bead is closed, or `config_hash` changed under the session.
2. **`POST /v1/policy/outcome`** with the classification, bound to the
   `reservation_id` **meter minted** (an unbound outcome is a forgery vector).
   Then act on `OutcomeResponse.Next`:
   - `escalate` / `retry` → **re-fire `gonk-dispatch` for the same `BeadAnchor`**.
     That is the re-sling, and it goes through Gate 2 by construction.
   - `done` → `StateDone`.
3. **Unpark** every `parked` bead whose `RetryAfter` has passed → re-fire
   `gonk-dispatch`. (Which decides again. Which may park it again. Which is fine.)

**The re-sling is a `gonk-dispatch` call. It is not a pour.** There is no second
pour path, and Task 4 Step 7's grep enforces that `gcapi.RunOrder` is called with
a formula-order name from exactly one place in the tree (`dispatch.go`).

`cmd/gonk-gate/sweep_test.go` must contain at minimum:

```go
// The sweeper's contract, in five tests. All against fakes; no cluster, no Dolt.

func TestSweepClassifiesSuccessAndFinishes(t *testing.T)
// marker present -> outcome success -> POST /outcome -> StateDone -> NO re-fire.

func TestSweepEscalatesOnGateFailure(t *testing.T)
// no marker + tokens > 0 -> gate-failed -> POST /outcome (next: escalate)
// -> re-fires gonk-dispatch for the SAME BeadAnchor, exactly once.

func TestSweepDoesNotEscalateOnInfraFailure(t *testing.T)
// no marker + ZERO tokens -> infra-failed -> POST /outcome -> re-fires
// gonk-dispatch (a retry at the SAME rung, which meter will choose) and the
// OutcomeRequest.Outcome is "infra-failed", never "gate-failed".
// *** This is HB-4's whole point. If it ever flips, escalations become free. ***

func TestSweepTreatsStaleSpendAsInfraNotGate(t *testing.T)
// meter's spend_as_of never advances past session end -> SpendStale ->
// infra-failed. Assert the OutcomeRequest says infra-failed. We could not PROVE
// the model answered, so we did not sell an escalation on a guess.

func TestSweepTreatsGitLabOutageAsInfraNotGate(t *testing.T)
// GitLab 500 on the notes query -> ArtifactUnknown -> infra-failed.
// An outage of OURS must never buy an escalation for a project.

func TestSweepUnparksOnlyWhenRetryAfterHasPassed(t *testing.T)
// two parked beads, one due one not -> exactly one gonk-dispatch fired.

func TestSweepNeverTouchesNeedsHumanBeads(t *testing.T)
// a needs-human bead is never classified, never re-fired, never spent on.

func TestSweepBindsOutcomeToMetersReservation(t *testing.T)
// the OutcomeRequest carries the reservation_id from the RECORD (which came from
// meter), never a synthesized one. An unbound `gate-failed` is a free rung.

func TestSweepIsIdempotentAcrossRuns(t *testing.T)
// running sweep twice over the same finished session POSTs /outcome ONCE and
// re-fires ONCE. The cooldown order runs every 30s; a non-idempotent sweeper
// would escalate a project once per tick.
```

**`TestSweepIsIdempotentAcrossRuns` is not optional.** A cooldown order fires
every 30 seconds forever. A sweeper that re-reports an outcome each tick walks a
project to the top of its ladder in a couple of minutes.

- [x] **Step 7: Write and implement `cmd/gonk-gate/check.go`** — the `[steps.check]` body

```go
// runCheck is what a formula's [steps.check] executes. It is PURE and IDEMPOTENT:
// it asks GitLab one question -- "is the bot's comment carrying this bead's
// marker on the issue?" -- and answers with an exit code. NOTHING ELSE.
//
// It is a VERIFICATION LOOP body (formula-spec-v2: [steps.check]'s only mode is
// exec, and it is re-run until it passes or times out). So it MUST NOT POST an
// outcome, MUST NOT reserve budget, MUST NOT write to the bead store, and MUST
// NOT decide anything. Every one of those belongs to `gonk-gate sweep`, which
// runs once per bead.
//
// Exit: 0 = the artifact is there. 3 = not yet (keep polling). 1 = we could not
// ask GitLab (infra) -- and note that this is NOT the same as "not there", which
// is exactly the distinction the classifier is built on.
func runCheck(ctx context.Context, d checkDeps) int
```

`check_test.go`: marker present → 0; marker absent → 3; a *different* bead's
marker present → 3; a *human's* comment quoting the marker → 3 (author must be the
bot); GitLab 500 → 1.

- [x] **Step 8: The shared-semantics test — Gate 1 and Gate 2 must mean the same thing**

`cmd/gonk-gate/contract_test.go`:

```go
// The owner's requirement, made executable: intake (Gate 1) and gonk-gate (Gate 2)
// must agree EXACTLY on what run/defer/deny mean. They already share the TYPES
// (both import pkg/meterapi, a compile-time contract). This asserts they share the
// SEMANTICS.
//
// The table is the single source of truth for both call sites. If someone changes
// one side's behaviour, this test fails and they have to come and change the
// meaning here, in the open, where a reviewer sees it.
func TestGateSemanticsAreSharedWithIntake(t *testing.T) {
	for _, tc := range []struct {
		decision   string
		mayPour    bool // Gate 2: pour a formula?
		mayFire    bool // Gate 1: fire the order?
		terminal   bool // does the bead stop here until a human or a clock intervenes?
	}{
		{meterapi.DecisionRun, true, true, false},
		{meterapi.DecisionDefer, false, false, false}, // a clock un-parks it
		{meterapi.DecisionDeny, false, false, true},   // only a human revives it
	} {
		if gotPour := MayPour(tc.decision); gotPour != tc.mayPour {
			t.Fatalf("MayPour(%q) = %v, want %v", tc.decision, gotPour, tc.mayPour)
		}
		if gotFire := intake.MayFire(tc.decision); gotFire != tc.mayFire {
			t.Fatalf("intake.MayFire(%q) = %v, want %v -- GATE 1 AND GATE 2 HAVE DRIFTED.\n"+
				"Read 'The two call sites MUST agree' in Plan 04 before you 'fix' this test.", tc.decision, gotFire, tc.mayFire)
		}
	}
	// An unknown decision must be refused by BOTH. A new decision kind added to
	// meter must not be silently treated as `run` by an old binary.
	if MayPour("something-new") || intake.MayFire("something-new") {
		t.Fatal("an unknown decision kind must fail closed on both sides")
	}
}

// Gate 1 (intake) and Gate 2 (gonk-dispatch) MUST send meter the SAME bead_id for
// one work item, or meter mints TWO reservations for one attempt -- double budget
// headroom, and Gate 1's reservation leaks until its TTL. Both sides pin bead_id
// to the deterministic BeadAnchor, NOT the Gas City bead id. Here we drive Gate 2
// with a BeadAnchor equal to intake's own BeadAnchor(42, 3) AND a *different* Gas
// City bead id, and assert the /decide body carries the BeadAnchor -- so meter
// reuses the one open reservation Gate 1 already opened.
func TestBothGatesSendTheSameBeadID(t *testing.T) {
	anchor := intake.BeadAnchor(42, 3) // the exact value Gate 1 sends
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionDefer, Attempt: 1, RetryAfter: time.Now().Add(time.Hour),
	}}
	_ = runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gcapitest.New(t).Client("gonk-city"),
		Store: beadstore.NewMemory(),
		Args: dispatchArgs{
			Project: "group/repo", ProjectID: 42, Rig: "group-repo", IssueIID: 3,
			BeadAnchor: anchor, BeadID: "gk-1a2b", // Gas City id differs ON PURPOSE
			SessionKey: "gonk-42-issue-3", Trigger: "issue-triage",
		},
	})

	var got map[string]any
	if err := json.Unmarshal(fm.lastBody, &got); err != nil {
		t.Fatalf("decode /decide body: %v", err)
	}
	if got["bead_id"] != anchor {
		t.Fatalf("Gate 2 sent bead_id=%v, want the BeadAnchor %q that Gate 1 sends -- "+
			"a mismatch mints a SECOND reservation for one attempt", got["bead_id"], anchor)
	}
	if got["bead_id"] == "gk-1a2b" {
		t.Fatal("Gate 2 leaked the Gas City bead id onto the wire; it must send the BeadAnchor")
	}
}
```

This requires a tiny exported helper on each side (`gate.MayPour`,
`intake.MayFire`). **Adding `intake.MayFire` is part of the Plan 02 amendment**
listed at the bottom of this plan — it is three lines, and it is what makes the
drift mechanically detectable instead of a comment nobody reads.

- [x] **Step 9: Run everything**

```bash
go test ./pkg/beadstore/ ./cmd/gonk-gate/ -race -count=1 -v
```
Expected: PASS.

Then prove the two negatives that matter:

```bash
# No LLM anywhere on the gate path.
grep -rniE "openai|anthropic|completion|chat/completions" cmd/gonk-gate/ pkg/gate/ pkg/beadstore/ \
  && echo "FAIL: a model call is on the gate path" || echo "ok"

# Exactly ONE place pours a formula.
grep -rn "RunOrder" cmd/gonk-gate/ | grep -v _test.go
# Expected: dispatch.go (the pour) and sweep.go (re-firing gonk-dispatch, which is
# NOT a formula-order). Any THIRD call site is a second pour path and a bug.
```

- [x] **Step 10: Commit**

```bash
gofmt -l . && go vet ./... && go test ./pkg/... ./cmd/... -race -count=1
git add pkg/beadstore cmd/gonk-gate
git commit -m "feat(gonk-gate): deterministic dispatch gate, outcome sweeper, and check verifier"
```

---

### Task 4: the pack — `pack.toml`, agents, formulas, orders (+ structural validation)

**Read this before you write a single line of TOML:**

1. **The loader REJECTS UNKNOWN KEYS.** An invented field is a hard load failure.
   The 15 legal top-level tables are listed in "Gas City facts" above.
2. **Do not open `gascity-packs`.** It is all-rights-reserved. Derive everything
   from `pack-spec.md`, `formula-spec-v2.md`, `config.md` and the loader source in
   the MIT `gascity` repo. (See the licensing constraint at the top.)
3. **Step 1 is a READING step, and it is not optional.** Several exact key names —
   `[steps.check]`'s fields, the step-var template syntax, `[order.params]`'s
   declaration shape — are things this plan tells you to *look up*, not to guess.
   A guess here is caught by Task 6 (the real loader), but only after you have
   written it wrong.

- [x] **Step 1: Pin gascity and transcribe the exact key names — do not guess**

```bash
mkdir -p /tmp/gonk-upstream && cd /tmp/gonk-upstream
git clone --depth 50 https://github.com/gastownhall/gascity
cd gascity && git rev-parse HEAD    # record this SHA -- it goes in images/versions.env
```

Read, and write the exact key names into a scratch note before writing any TOML:

- `docs/reference/specs/pack-spec.md` — the `[pack]`, `[agent_defaults]`, `[doctor]`,
  `[commands]` tables.
- `docs/reference/specs/formula-spec-v2.md` — **`[[steps]]`'s fields, `[steps.check]`'s
  exact keys (mode/command/timeout/interval — transcribe them), and the var
  templating syntax.**
- `docs/tutorials/07-orders.md` — the `[order]` table and `[order.params]`.
- `internal/config/pack.go` — **the loader.** It is the authority when the prose
  and the code disagree (they do: `[[webhook]]` is in the code and not in the prose
  table).
- `internal/runtime/k8s/pod.go` — confirm the agent-pod labels are still
  `app: gc-agent` / `gc-session` / `gc-agent`. **Plan 05's NetworkPolicy selector
  depends on this**, and a wrong selector renders fine and enforces nothing.

**Confirm the licence:** `LICENSE` in `gascity` says MIT. `gascity-packs` has none.
If that has changed, stop and tell the owner.

- [x] **Step 2: Write `pack/pack.toml`**

```toml
# gonk -- a Gas City pack. Clean-room from the MIT gascity specs
# (docs/reference/specs/pack-spec.md, formula-spec-v2.md, config.md) and the
# loader at internal/config/pack.go. NOTHING here is derived from
# gastownhall/gascity-packs, which carries no licence.
#
# THE LOADER REJECTS UNKNOWN KEYS. Every table below is one of the 15 it accepts.

schema = 2

[pack]
name = "gonk"
version = "0.1.0"
description = "Issue triage, .agent/ scaffolding and mention replies on self-hosted GitLab, with hard budgets."

# ---------------------------------------------------------------------------
# WHY THERE IS NO [[webhook]] HERE
#
# GitLab authenticates webhooks with a constant X-Gitlab-Token header. Gas City's
# verification registry is a CLOSED SET OF FIVE schemes (github-hmac-sha256,
# hmac-sha256, slack-v0, discord-ed25519, jwt-jwks) and none of them is a
# constant-time shared-secret compare. `hmac-sha256` is an HMAC OVER THE BODY and
# WILL NOT SUBSTITUTE.
#
# So every GitLab event goes to gonk-intake's own receiver instead (Plan 02), which
# verifies the token before reading a byte of body, suppresses bot-authored events,
# dedupes redeliveries, fetches and size-caps .gonk.yml, and registers the project
# with gonk-meter -- none of which a [[webhook.rule]] could do anyway.
#
# A `gitlab-token` verify scheme is a clean upstream PR (spec 4.4). NOTHING HERE
# DEPENDS ON IT LANDING.
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# WHY THERE IS NO [[service]] HERE
#
# gonk-intake and gonk-meter are standalone Kubernetes Deployments shipped by our
# Helm chart (Plan 05), NOT pack [[service]] proxy_process entries. A proxy_process
# is a controller-supervised local process; our services are Go servers that need a
# CNPG database, ExternalSecrets, probes, independent scaling and their own
# NetworkPolicy -- and gonk-meter is THE BUDGET ENFORCER, whose availability must
# not be coupled to the Gas City controller's lifecycle.
#
# This is still pack-first: the pack carries the agents, formulas and orders; the
# chart carries the two servers.  (Owner decision, 2026-07-13.)
# ---------------------------------------------------------------------------

[agent_defaults]
# Pools scale to zero (spec 4.2): an idle gonk installation runs no agent pods.
min_active_sessions = 0
# spec 4.2 / 5.4: durable, layered continuity. `.gonk.yml`'s `continuity` selects
# per project; this is the pack-level default. If ACP resume across pod recreation
# turns out to be broken (spec 11.4 -- a NAMED RISK), the fallback is `fresh`, and
# correctness survives because it never depended on LLM context (bead history + the
# GitLab thread + .agent/ reconstruct everything).
wake_mode = "resume"

[doctor]
# Deterministic preflight checks. No model calls.
[[doctor.check]]
name = "meter-reachable"
description = "gonk-meter answers /healthz. Without it NOTHING may spend, and every dispatch fails closed."
command = "gonk-gate doctor --meter"

[[doctor.check]]
name = "city-configured"
description = "GONK_CITY is set. There is no default (OD-1): a wrong city name 404s every dispatch."
command = "gonk-gate doctor --city"

[[doctor.check]]
name = "no-model-names-in-pack"
description = "The pack names no model. Rungs map to models in the OPERATOR's catalog, never here."
command = "gonk-gate doctor --no-model-names"
```

**Verify against the loader's table list before committing.** If `[doctor.check]`
is not the exact shape `pack-spec.md` gives, use the shape it gives — this plan is
authoritative about *what the checks are for*, not about the loader's grammar.

- [x] **Step 3: Write the agents** — `agents/<name>/agent.toml` + `prompt.template.md`

**The directory name IS the agent name.** A `name` field inside `agent.toml` is
ignored — do not put one there and then believe it.

`pack/agents/triage/agent.toml`:

```toml
description = "Reads a GitLab issue and posts exactly one triage comment plus labels."

# The model is NOT named here. It arrives per-session from gonk-meter's
# /v1/policy/decide response, which reads the OPERATOR's rung catalog
# (pkg/opercfg). The pack is model-agnostic BY CONSTRUCTION: naming a model here
# would hardcode a site's inference topology into an open-source pack, and would
# silently disagree with the rung the budget was checked against.

[limits]
# Deterministic caps. The gate signal (spec 6.3) is "session completed and posted
# required artifacts WITHIN TURN/TOKEN CAPS", so the caps are part of the gate.
max_turns = 12
```

`pack/agents/triage/prompt.template.md` (**OD-4** — content is an owner decision;
this is the assumed default, and **the marker line is load-bearing**):

```markdown
You are gonk, a triage bot on a self-hosted GitLab instance.

## Your task

Read issue !{{.Vars.issue_iid}} in project `{{.Vars.project}}` and triage it.

Load `.agent/` from the repository first: it is the project's own context and it
overrides anything you would otherwise assume.

## What you must produce

1. Apply labels to the issue, each prefixed `{{.Vars.label_prefix}}`.
2. Post **exactly one** comment containing:
   - a short analysis of what the issue is asking for;
   - anything genuinely ambiguous, phrased as a direct question to the reporter;
   - **this line, verbatim, on its own line, as the last line of the comment:**

     <!-- gonk:bead:{{.Vars.bead_id}} -->

## Rules

- **The marker line is not decoration. It is how the system knows you did the
  work.** A deterministic gate greps for it; there is no LLM judging your prose.
  If you omit it, your work is recorded as a failure and the task is retried at a
  more expensive model. Do not reformat it, do not wrap it in a code fence, do not
  explain it.
- Post **one** comment. Not two. A second comment is a second notification for
  every subscriber on that issue.
- Do not close the issue. Do not open a merge request. Do not push a branch.
  You have Reporter-level intent even if the token has more.
- If you cannot do the work, say so plainly in the comment **and still include the
  marker.** An honest "I don't have enough information, here is what I need" is a
  SUCCESS. Silence is not.
```

`pack/agents/scaffold/prompt.template.md` — the `.agent/` scaffold MR (spec §5.3:
*"a second, post-merge MR, generated by an agent session that reads the repo — the
first metered work"*). Same marker rule; the artifact is a **merge request** from
branch `gonk/scaffold`, so `gonk-gate check` for this trigger looks for the MR, not
a comment (`check --artifact mr --source-branch gonk/scaffold`).

`pack/agents/mention/prompt.template.md` — the conversational surface (spec §5.5).
It replies **in the thread** (`discussion_id` is an order var), and it carries the
same marker.

**One rule across all three prompts, and it is the only one with teeth:**
*the agent must emit the marker, and the gate reads nothing else.*

- [x] **Step 4: Write the formulas** — `pack/formulas/*.toml`

```toml
# pack/formulas/gonk-triage.toml
formula = "gonk-triage"

[vars]
# Every one of these is supplied by `gonk-gate dispatch` via the order-run route's
# {"vars": {...}} body. The three that came from gonk-meter -- rung, model,
# metadata_json -- are stamped VERBATIM and never chosen here.
project = ""
project_id = ""
rig = ""
issue_iid = ""
bead_id = ""
bead_anchor = ""
session_key = ""
trigger = ""
config_hash = ""
rung = ""
model = ""
attempt = ""
reservation_id = ""
metadata_json = ""
key_secret_name = ""
key_secret_key = ""
label_prefix = "gonk::"

[[steps]]
name = "triage"
agent = "triage"          # -> pack/agents/triage/ (THE DIRECTORY NAME IS THE AGENT NAME)
description = "Read the issue, apply labels, post one marker-carrying comment."

# The GATE. formula-spec-v2: [steps.check]'s ONLY mode is exec, and it is a
# VERIFICATION LOOP -- re-run until it passes or times out. So it must be pure and
# idempotent: `gonk-gate check` asks GitLab one question and answers with an exit
# code. It does NOT post an outcome and does NOT decide anything -- `gonk-gate
# sweep` (the cooldown exec order) does that, exactly once per bead.
[steps.check]
mode = "exec"
command = "scripts/gonk-check.sh"
# TRANSCRIBE the real key names from formula-spec-v2.md in Step 1. If it is
# `timeout`/`interval`/`retries`, use those. Task 6 runs the real loader and will
# reject a wrong key -- but write it right the first time.
```

`gonk-scaffold.toml` and `gonk-mention.toml` are the same shape with their own
agent and their own `check` arguments.

- [x] **Step 5: Write the orders** — `pack/orders/*.toml`

**The rule: an order is `formula` XOR `exec`. Never both. And an EXEC ORDER MAY
NOT HAVE A POOL.**

```toml
# pack/orders/gonk-dispatch.toml
#
# *** THIS IS GATE 2. IT IS THE ONLY PATH IN GONK THAT POURS A FORMULA. ***
#
# It asks gonk-meter /v1/policy/decide immediately before pouring -- on the first
# dispatch, on a re-sling after a gate failure, on an unpark after a defer, on
# every single pour without exception.
#
# WHY THIS EXISTS AT ALL: spec 6.2.3 said "the dispatch FORMULA asks meter for the
# rung". A FORMULA CANNOT DO THAT -- per formula-spec-v2 a formula is a DAG of
# steps handed to AGENTS, and the only code it can run is [steps.check]. There is
# no HTTP verb in a formula. If the rung decision lived in a formula it would be
# made by an LLM, which violates spec 6.3's "no LLM judges" at the exact point
# where money is spent. So it lives HERE, in an exec order, with no model in it.
#
# A ladder escalation is CONTROLLER-INITIATED and never passes gonk-intake. If
# this order ever trusts the rung in its vars instead of re-deciding, escalations
# spend UNMETERED. See TestDispatchAlwaysDecidesEvenWhenVarsCarryARung.
[order]
description = "THE BUDGET GATE. Ask gonk-meter for a rung, then pour the trigger's formula -- or park, or stop."
exec = "scripts/gonk-dispatch.sh"     # XOR `formula`. NEVER BOTH.
trigger = "manual"                    # fired by gonk-intake and by gonk-sweep, via
                                      # POST /v0/city/{city}/order/gonk-dispatch/run
timeout = "120s"
# NO `pool` -- exec orders may not have one.

[order.params]
# Declared params are namespaced into the exec's environment as GC_WEBHOOK_ARG_*.
# TRANSCRIBE the exact declaration shape from docs/tutorials/07-orders.md in Step 1.
project = { description = "GitLab path_with_namespace", required = true }
project_id = { description = "GitLab numeric project id", required = true }
rig = { description = "Gas City rig name", required = true }
issue_iid = { description = "GitLab issue IID", required = false }
bead_anchor = { description = "IDEMPOTENCY KEY. The controller must dedupe on it: a duplicate bead is duplicate spend.", required = true }
bead_id = { description = "Bead id", required = true }
session_key = { description = "Stable across triage and every follow-up on the same issue", required = true }
trigger = { description = "issue-triage | scaffold | mention-reply", required = true }
config_hash = { description = "The .gonk.yml that authorized this work", required = false }
discussion_id = { description = "For mention-reply: the thread to answer in", required = false }
# rung / model / reservation_id may arrive from gonk-intake's own /decide (Gate 1).
# THEY ARE A HINT FOR LOGS. This order RE-DECIDES and uses meter's live answer.
rung = { description = "HINT ONLY -- this order re-decides. Never trusted.", required = false }
model = { description = "HINT ONLY -- this order re-decides. Never trusted.", required = false }
reservation_id = { description = "HINT ONLY -- this order re-decides. Never trusted.", required = false }
```

```toml
# pack/orders/gonk-sweep.toml
#
# The deterministic outcome brain. NO LLM IN IT.
#
# WHY IT IS AN ORDER AND NOT A FORMULA STEP: every [[steps]] in a formula becomes a
# bead handed to an AGENT (formula-spec-v2), so a formula cannot contain a
# deterministic final step. And [steps.check] -- the only exec inside a formula --
# is a re-run verification LOOP, so it must be side-effect-free and cannot POST an
# outcome. Therefore the classifier lives here, in a cooldown exec order that runs
# once every 30s and does each bead exactly once.
#
# It: (1) classifies every finished session from OBJECTIVE signals (GitLab: did the
# artifact land? meter: did the model actually answer?); (2) POSTs
# /v1/policy/outcome bound to the reservation METER minted; (3) re-fires
# gonk-dispatch on escalate/retry -- WHICH RE-DECIDES, so the re-sling is gated;
# (4) unparks deferred beads whose retry_after has passed -- ALSO via gonk-dispatch.
#
# INFRA FAILURES NEVER ESCALATE A RUNG (spec 6.3). If this order ever reports an
# infra failure as `gate-failed`, it buys an escalation the project did not earn.
[order]
description = "Classify finished sessions, report outcomes, re-sling escalations, unpark deferred beads. Deterministic; no model calls."
exec = "scripts/gonk-sweep.sh"
trigger = "cooldown"
interval = "30s"
timeout = "300s"
# NO `pool` -- exec orders may not have one.
```

```toml
# pack/orders/gonk-triage.toml
#
# The formula-order. It is fired ONLY by gonk-dispatch, ONLY after /v1/policy/decide
# said `run`. Nothing else may fire it -- and note that the supervisor's order-run
# route has NO AUTH (admission is by network position), so "nothing else CAN reach
# it" is a property of the NetworkPolicy and the namespace, not of this file.
[order]
description = "Triage one GitLab issue. Poured by gonk-dispatch after a budget decision."
formula = "gonk-triage"               # XOR `exec`. NEVER BOTH.
trigger = "manual"
pool = "gonk"                         # OD-2: one pool, scale-to-zero.
timeout = "20m"
```

`gonk-scaffold.toml` and `gonk-mention.toml` likewise.

`pack/scripts/gonk-dispatch.sh` (and its two siblings) are **thin**:

```sh
#!/bin/sh
# Thin wrapper. ALL logic is in the Go binary, where it is tested.
# Gas City namespaces an order's declared [order.params] into the exec environment
# as GC_WEBHOOK_ARG_*; gonk-gate reads them from there.
set -eu
exec gonk-gate dispatch
```

**Keep them at four lines.** Logic that creeps into a shell script is logic with
no tests. If you find yourself writing an `if` here, it belongs in `dispatch.go`.

- [x] **Step 6: Write the structural validation test** — `internal/packtest/pack_test.go`

This is the "test" for the declarative artifacts, and it runs with **no Gas City,
no containers, and no network**. It is not a substitute for the real loader
(Task 6) — it is the fast gate that catches the mistakes we *know* we can make.

```go
package packtest

// These tests read pack/ off disk and assert the things the loader would reject,
// plus the things the loader would happily ACCEPT that would still be wrong for us.

// The loader REJECTS UNKNOWN KEYS. Catch it in milliseconds instead of in a
// container.
func TestPackTOMLUsesOnlyKnownTopLevelTables(t *testing.T) {
	known := map[string]bool{
		"schema": true, "pack": true, "imports": true, "agent_defaults": true,
		"agent": true, "named_session": true, "service": true, "webhook": true,
		"providers": true, "upstreams": true, "runtimes": true, "patches": true,
		"doctor": true, "commands": true, "global": true, "pricing": true,
	}
	// decode pack.toml into map[string]any; every key must be in `known`.
}

// Orders are orders/<name>.toml and formulas are formulas/*.toml. There is NO
// [[order]] and NO [[formula]] in pack.toml, and putting one there is a load error.
func TestPackTOMLHasNoInlineOrdersOrFormulas(t *testing.T)

// Owner decision: the two Go servers are Deployments from the chart, not pack
// services. A [[service]] here would couple the BUDGET ENFORCER's availability to
// the Gas City controller's lifecycle.
func TestPackTOMLHasNoService(t *testing.T)

// The pack ships no webhook (see the comment in pack.toml). If someone adds one,
// its verify scheme must be one of the FIVE the loader knows -- and X-Gitlab-Token
// is not among them, so adding a GitLab webhook here is a bug, not a feature.
func TestNoWebhookOrOnlyAKnownVerifyScheme(t *testing.T) {
	schemes := map[string]bool{
		"github-hmac-sha256": true, "hmac-sha256": true, "slack-v0": true,
		"discord-ed25519": true, "jwt-jwks": true,
	}
	// ...
}

// formula XOR exec, never both. And an EXEC ORDER MAY NOT HAVE A POOL.
func TestOrdersAreFormulaXorExecAndExecOrdersHaveNoPool(t *testing.T)

// The DIRECTORY NAME is the agent name. Both files must exist, and a stray `name`
// field inside agent.toml is IGNORED by the loader -- which means someone will one
// day set it, believe it, and be wrong.
func TestEveryAgentDirHasBothFilesAndNoNameField(t *testing.T)

// Every agent referenced by a formula step exists as a directory.
func TestFormulaStepsReferenceRealAgents(t *testing.T)

// Every order named in cmd/gonk-gate's orderForTrigger map exists as an order file.
// (This is the map that turns a trigger into a pour. A typo here is a 404 at
// dispatch time, in production, on the money path.)
func TestEveryTriggerOrderExists(t *testing.T)

// *** THE MARKER IS LOAD-BEARING. *** gonk-gate check and gonk-gate sweep grep for
// it. If a prompt rewrite drops it, the gate goes blind: every successful session
// classifies as `gate-failed`, every project climbs its ladder to the most
// expensive rung, and the bill arrives before the bug report.
func TestEveryPromptEmitsTheBeadMarker(t *testing.T) {
	for _, p := range promptTemplates(t) {
		if !strings.Contains(read(p), "<!-- gonk:bead:") {
			t.Fatalf("%s does not instruct the agent to emit the bead marker.\n"+
				"Without it the deterministic gate cannot tell success from failure, "+
				"and every project escalates to its most expensive rung.", p)
		}
	}
}
```

- [x] **Step 7: The anti-drift greps — and they go in the standing gate**

```bash
# AD-1: THE PACK NAMES NO MODEL. Rungs map to models in the OPERATOR's catalog
# (pkg/opercfg), never here. A model name in an open-source pack hardcodes one
# site's inference topology and silently disagrees with the rung the budget was
# checked against.
grep -rniE "qwen|llama|gpt-|claude|sonnet|opus|haiku|glm|mistral|gemini|ollama|vllm" pack/ \
  && echo "FAIL: the pack names a model" || echo "ok"

# The pack holds no credential and no URL of a real service.
grep -rniE "sk-|glpat-|password|secret:|token:" pack/ \
  | grep -v "key_secret_name\|key_secret_key\|shared-secret" \
  && echo "FAIL: credential-shaped string in the pack" || echo "ok"

# No LLM on the gate path (belt and braces with Task 3's grep).
grep -rn "gonk-gate" pack/scripts/ | grep -c "" # each script is a one-line exec
```

Wire all three into `make lint-pack`, and call `make lint-pack` from the standing
gate.

- [x] **Step 8: Run and commit**

```bash
go test ./internal/packtest/ -race -count=1 -v      # PASS
make lint-pack                                       # ok / ok
git add pack internal/packtest
git commit -m "feat(pack): gonk Gas City pack -- agents, formulas, and the deterministic gate orders"
```

**What this task does NOT prove:** that Gas City's *real* loader accepts the pack.
That is Task 6, and it is the honest one.

---

### Task 5: `images/Dockerfile.agent` — opencode + gonk's tooling

**Every version is pinned in `images/versions.env`. There is no `latest` anywhere
in this repo, ever** (`docs/environment.md`: *"Pin exact tags — never `latest`;"*
Renovate autodiscovers and bumps them).

**Containers are rebuilt to deliver changes, never copied into** (house rule).
**Podman's CNI bridge is broken on this box — every `podman run` needs
`--network=host`** (`docs/environment.md`).

**Files:**
- Create: `images/versions.env`, `images/Dockerfile.agent`, `images/agent/entrypoint.sh`
- Create: `Makefile` (targets: `images`, `push`, `pack-validate`, `no-latest`, `lint-pack`)
- Test: `test/images/agent_smoke_test.go` (build tag `images`)

- [x] **Step 1: Write `images/versions.env` — one file, every pin**

```sh
# EVERY pin lives here. Renovate bumps this file. NOTHING may say `latest`.
#
# The registry is the in-cluster GitLab registry (docs/environment.md). Harbor is
# NOT deployed; zot is a pull-through cache only.
REGISTRY=registry.orac.local/agentic/gonk-project

# The image tag. NEVER `latest`. Set by `make images` from the git SHA:
#   GONK_TAG=v0.1.0-$(git rev-parse --short=12 HEAD)
GONK_VERSION=v0.1.0

# --- OD-3: OWNER DECISION NEEDED -----------------------------------------------
# Pin an exact opencode version AND its digest. A floating opencode is an agent
# whose behaviour changes under you -- and spec 11.4's NAMED RISK (ACP session
# resume across pod recreation) is version-sensitive, so a floating opencode makes
# that milestone untestable. If the owner has no preference, pin whatever is
# current at execution time and record it in ADR-004.
OPENCODE_VERSION=
# -------------------------------------------------------------------------------

GO_VERSION=1.26
GLAB_VERSION=          # exact release tag
BD_VERSION=            # beads (MIT) release tag
GASCITY_REF=           # the SHA recorded in Task 4 Step 1
DEBIAN_BASE=          # e.g. debian:trixie-slim@sha256:...  -- pin the DIGEST
```

- [x] **Step 2: Write `images/Dockerfile.agent`**

Derive the base layout from gascity's own `contrib/k8s/Dockerfile.agent` (**MIT — a
legitimate basis**). Do **not** look at any pack repo.

```dockerfile
# gonk agent image: opencode + the tooling a session needs to do real work on a
# GitLab project. One pod per active session (spec 4.1); pools scale to zero.
#
# Derived from gascity's contrib/k8s/Dockerfile.agent (MIT).

ARG GO_VERSION
ARG DEBIAN_BASE

# ---- build gonk-gate (static; the agent needs `trailers`) --------------------
FROM golang:${GO_VERSION} AS gate
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${GONK_VERSION}" \
    -o /out/gonk-gate ./cmd/gonk-gate

# ---- runtime ----------------------------------------------------------------
FROM ${DEBIAN_BASE}
ARG OPENCODE_VERSION
ARG GLAB_VERSION
ARG BD_VERSION

RUN test -n "${OPENCODE_VERSION}" || (echo "OPENCODE_VERSION is unset -- refusing to build an agent on a floating opencode (OD-3)" && exit 1)

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# opencode, glab, bd -- each at an EXACT version.
RUN install-opencode "${OPENCODE_VERSION}"   # pinned; verify the checksum
RUN install-glab     "${GLAB_VERSION}"
RUN install-bd       "${BD_VERSION}"

COPY --from=gate /out/gonk-gate /usr/local/bin/gonk-gate
COPY images/agent/entrypoint.sh /usr/local/bin/gonk-agent-entrypoint
COPY images/agent/prepare-commit-msg /usr/local/share/gonk/prepare-commit-msg

# THE PRIVATE CA. gitlab.orac.local and gonk's own services serve certs from the
# cluster's private CA. trust-manager publishes ConfigMap `trust-bundle`
# (key tls-ca-bundle.pem) into EVERY namespace; the chart mounts it here.
#
# *** NEVER DISABLE TLS VERIFICATION. *** The fix is to TRUST the CA, and Go, git
# and curl all honour SSL_CERT_FILE with no code change. An agent pod that will
# talk to anything answering on 443 is an agent pod that can be phished off the
# metered path.
ENV SSL_CERT_FILE=/etc/ssl/orac/ca.crt
ENV GIT_SSL_CAINFO=/etc/ssl/orac/ca.crt

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/gonk-agent-entrypoint"]
```

- [x] **Step 3: Write `images/agent/entrypoint.sh`**

It does exactly three things, in order:

1. **Install the `prepare-commit-msg` hook** into the rig clone's `.git/hooks/`
   (Task 8 — hooks are not cloned, so the session must install it).
2. **Render `overlay/opencode.json`** from the environment — the LiteLLM base URL,
   the **model from `GC_WEBHOOK_ARG_MODEL`** (meter's answer; never a literal), the
   virtual key read **from a file mount** (never an env value — env leaks into `ps`,
   `/proc/<pid>/environ`, crash dumps and every child process), and the **attribution
   metadata** set as the static provider header
   `provider.<gonk>.options.headers["x-litellm-spend-logs-metadata"]` from
   **`GC_WEBHOOK_ARG_METADATA_JSON`, stamped verbatim** (see **OD-7 — VERIFIED**).
3. `exec` opencode.

- [x] **Step 4: OD-7 — render the VERIFIED metadata seam**

The mechanism is **verified**, not open (live LiteLLM v1.92.0, 2026-07-13;
`docs/environment.md`, "VERIFIED: the attribution chain works"). The config key is
`provider.<id>.options.headers`; the header is `x-litellm-spend-logs-metadata`; its
value is the verbatim seven-key `DecideResponse.Metadata` JSON. Spec goal 4
(attribution at every granularity) rests on it.

```bash
# The mechanism is settled. Two things to do here:
#
# 1. CONFIRM (version-sensitive) that the PINNED opencode still exposes
#    `provider.<id>.options.headers` at that version -- read its own provider/config
#    docs at the pin. The seam is verified on the smoke-tested build; a pin bump
#    could move or rename it, so confirm before rendering.
# 2. RENDER it in entrypoint.sh from GC_WEBHOOK_ARG_METADATA_JSON, stamped verbatim,
#    and add a test that asserts a request reaching a FAKE LiteLLM carries all seven
#    gonk_* keys under x-litellm-spend-logs-metadata.
#
# FALLBACK (enterprise-gate contingency): LiteLLM's docs call per-k/v
# spend_logs_metadata an enterprise feature; it was NOT enforced on v1.92.0. If a
# future upgrade starts enforcing it, switch the header to x-litellm-tags, which
# populates the request_tags column (also verified). Plan 06 detects a break.
#
# DEEPER FALLBACK (only if the header seam is gone at the pin): gonk-gate asks meter
# for a SHORT-LIVED, PER-ATTEMPT virtual key whose LiteLLM key-metadata carries the
# atags -- KeyRef becomes per-attempt (Plan 03 AD-1). THIS IS A PLAN 03 AMENDMENT --
# raise it, do not build it here. Contingency-only; the header path is verified.
```

**Record the rendered config in ADR-004** and hand the live verification to
Plan 06 (new item — see hand-offs).

- [x] **Step 5: Build it, and smoke-test it in a container**

House rule: *if an app runs in a container, test it in a container.*

```bash
set -a; . images/versions.env; set +a
export GONK_TAG="${GONK_VERSION}-$(git rev-parse --short=12 HEAD)"

podman build --network=host \
  --build-arg GO_VERSION --build-arg DEBIAN_BASE --build-arg OPENCODE_VERSION \
  --build-arg GLAB_VERSION --build-arg BD_VERSION \
  -f images/Dockerfile.agent -t "${REGISTRY}/gonk-agent:${GONK_TAG}" .
```

`test/images/agent_smoke_test.go` (build tag `images`) asserts, **inside the
container**:

```
opencode --version   == the pinned OPENCODE_VERSION   (a floating agent is not an agent)
glab --version       == the pinned GLAB_VERSION
bd --version         == the pinned BD_VERSION
git --version        exits 0
gonk-gate --version  == GONK_TAG
gonk-gate trailers --help  exits 0        (the agent's only gonk-gate subcommand)
id -u                == 65532             (not root)
```

Run: `go test ./test/images/ -tags images -run Agent -v`

- [x] **Step 6: Commit**

```bash
git add images Makefile test/images
git commit -m "feat(images): pinned opencode agent image with git, glab, bd and gonk-gate"
```

**Done (2026-07-16). Real, verified pins, not guessed:**
- **OD-3 settled**: `OPENCODE_VERSION=1.18.3` (npm `opencode-ai` dist-tag `latest`
  as of 2026-07-16; `github.com/anomalyco/opencode` release `v1.18.3` — note the
  upstream `sst/opencode` release redirects there — ships a self-contained
  `opencode-linux-x64.tar.gz`, sha256 verified and pinned in the Dockerfile).
  `GLAB_VERSION=1.108.0` and `BD_VERSION=1.0.3` match the `glab`/`bd` already
  installed on this box; both fetched from their real release artifacts
  (gitlab.com generic package registry; `github.com/gastownhall/beads`) with
  sha256 checksums verified in-Dockerfile (a mismatch fails the BUILD, not a
  running pod). `DEBIAN_BASE` and the `golang` build-stage base are pinned by
  **digest** (`Docker-Content-Digest` header, not guessed), ahead of Task 7's
  own `TestBaseImagesArePinnedByDigest`.
- **The attribution overlay is real, not assumed**: cloned
  `github.com/anomalyco/opencode` at the pinned tag and confirmed
  `provider.<id>.options.headers` (packages/opencode/src/session/llm/native-runtime.ts,
  packages/opencode/src/provider/provider.ts's `BUNDLED_PROVIDERS` map — `@ai-sdk/openai-compatible`
  is COMPILED IN, no npm-registry fetch at session start, so spec 9's
  "agent pods reach only GitLab and LiteLLM" holds) and opencode's own
  `{file:<path>}` config-variable substitution
  (packages/opencode/src/config/variable.ts) for the virtual key — the
  entrypoint never reads the key into its own env or a shell variable at all.
  `entrypoint.sh` renders `overlay/opencode.json` with `jq` (not string
  concatenation, so an embedded quote in the metadata JSON cannot corrupt the
  config) and was proven, in a running container, to carry all seven
  `pkg/atags` keys round-tripped through `atags.FromMetadata` itself
  (`test/images/agent_smoke_test.go`'s
  `TestAgentImageAttributionOverlayCarriesAllSevenAtags`). **Still Plan
  06's to verify live**: this is opencode's own source at the pin, not a
  request that actually reached LiteLLM through opencode itself (the
  environment.md smoke test used a hand-built HTTP request).
- **Two gaps found and flagged, not silently patched**:
  1. `cmd/gonk-gate` has **no `--version` flag and no `trailers` subcommand**
     (confirmed against `main.go`). `trailers` is squarely Task 8's own file
     (`cmd/gonk-gate/trailers.go`) — out of this task's remit. `--version` is
     smaller and wanted by **both** this task's own smoke-test wishlist and
     Task 6's controller smoke test, so it is a cross-task gap, not a
     Task-8-only one; `images/Dockerfile.agent`'s `-ldflags -X main.version=`
     is a harmless no-op linker directive until some task adds `var version
     string`. `images/agent/prepare-commit-msg` is a **provisional
     passthrough stub** (Task 5's own Dockerfile step COPIES a file Task 8's
     file list says Task 8 creates — an ordering wrinkle in the plan itself)
     that defers to `gonk-gate trailers` the moment it exists and otherwise
     never blocks a commit. `test/images/agent_smoke_test.go` skips the
     `--version`/`trailers --help` assertions with a named reason rather than
     asserting something false.
  2. **The virtual key's file-mount path has no owner yet.**
     `cmd/gonk-gate/dispatch.go` hands the pack `key_secret_name` +
     `key_secret_key` (a Kubernetes Secret name + key) as order vars; turning
     that into an actual Secret volume mount on the agent POD is a Gas City
     session-provider / chart concern this task cannot reach (Task 5 is the
     image and its entrypoint, not the pod spec). `entrypoint.sh` reads the
     key's path from `GONK_LITELLM_KEY_FILE` (one more `*_FILE` env var,
     matching every other secret in this repo) and refuses to start if it is
     unset or the file is missing — but **something in Plan 05/06 must set
     `GONK_LITELLM_KEY_FILE` to wherever the Secret named by
     `key_secret_name`/`key_secret_key` actually lands**, and nothing does
     that yet. Flagged for whichever of Plan 05 (chart / session-provider
     pod spec) or Plan 06 (live wiring) owns it.
- **Push deferred, not blocked on**: `make push` exists and is correct, but
  this sandbox has no LAN reach to `registry.orac.local`
  (docs/environment.md) — not run this session. `make images` (agent only;
  Task 6/7 extend it to controller/intake/meter) was run for real:
  `podman build --network=host ...` succeeded in ~2m8s, and
  `go test ./test/images/ -tags images -run Agent -v` passed (5 tests, 1
  named skip) against the real, running container. The built image was
  removed afterward (`podman rmi` + `system prune`) to avoid leaving ~560MB
  of cruft on this box.

---

### Task 6: `images/Dockerfile.controller` — and validating the pack against **Gas City's real loader**

**This is the honest validation task.** Task 4's `internal/packtest` catches the
mistakes we *know* we can make. This one runs **the actual code that will reject an
unknown key** — offline, in a container, with no cluster and no deployed city.

That is possible because the loader is a **library inside the `gc` binary**, and
`gc` is MIT and buildable from source. We do not need a *running* Gas City to run
its *parser*.

**Files:**
- Create: `images/Dockerfile.controller`
- Test: `test/images/controller_smoke_test.go`, `test/images/packvalidate_test.go` (tag `images`)

- [x] **Step 1: Write `images/Dockerfile.controller`**

```dockerfile
# gonk controller image: Gas City's `gc` (built from the MIT source at a pinned
# SHA) + `bd` + gonk's pack + `gonk-gate`.
#
# gonk-gate ships HERE because the pack's exec orders (gonk-dispatch, gonk-sweep)
# run on the orchestrator. THEY ARE THE BUDGET GATE, and they have no model call
# in them.
#
# Derived from gascity's contrib/k8s/Dockerfile.controller (MIT). NOTHING here is
# derived from gascity-packs, which carries no licence.

ARG GO_VERSION
ARG DEBIAN_BASE

FROM golang:${GO_VERSION} AS gc
ARG GASCITY_REF
RUN test -n "${GASCITY_REF}" || (echo "GASCITY_REF is unset -- refusing to build against a moving upstream" && exit 1)
WORKDIR /src
RUN git clone https://github.com/gastownhall/gascity . && git checkout "${GASCITY_REF}"
RUN CGO_ENABLED=0 go build -trimpath -o /out/gc ./cmd/gc

FROM golang:${GO_VERSION} AS gate
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/gonk-gate ./cmd/gonk-gate

FROM ${DEBIAN_BASE}
ARG BD_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*
RUN install-bd "${BD_VERSION}"

COPY --from=gc   /out/gc        /usr/local/bin/gc
COPY --from=gate /out/gonk-gate /usr/local/bin/gonk-gate
COPY pack/ /opt/gonk/pack/

ENV SSL_CERT_FILE=/etc/ssl/orac/ca.crt
USER 65532:65532
```

- [x] **Step 2: Discover `gc`'s offline validation surface — then pin it**

```bash
podman run --rm --network=host "${REGISTRY}/gonk-controller:${GONK_TAG}" gc --help
podman run --rm --network=host "${REGISTRY}/gonk-controller:${GONK_TAG}" gc pack --help  || true
podman run --rm --network=host "${REGISTRY}/gonk-controller:${GONK_TAG}" gc doctor --help || true
```

**Take the strongest option `gc` actually offers, in this order:**

1. A dedicated **pack validate/lint** subcommand → use it. Best case: the loader
   runs, unknown keys are rejected, exit code is the gate.
2. `gc doctor` against the pack directory → use it.
3. **Neither exists** → **start the controller with the pack and no city, and assert
   it loads the pack before it fails on anything else.** The loader runs at startup;
   a pack with an unknown key fails there, loudly, with a message naming the key.
   Grep for a load error. **This is still the real loader** — it is just a clumsier
   way to invoke it.

Whichever you land on, **write it into the `Makefile` as `make pack-validate`** and
into `test/images/packvalidate_test.go` so it is a *test*, not a thing someone
remembers to run.

- [x] **Step 3: Write the pack-validation test** — `test/images/packvalidate_test.go`

```go
//go:build images

package images

// THE REAL LOADER, OFFLINE. No cluster, no deployed city -- just the parser that
// will reject our pack in production, run against our pack now.

func TestGasCityLoaderAcceptsTheGonkPack(t *testing.T) {
	out, code := runInController(t, packValidateCmd()...)
	if code != 0 {
		t.Fatalf("Gas City's loader REJECTED the gonk pack (exit %d):\n%s", code, out)
	}
}

// The negative control, and it is the one that gives the positive its meaning.
// A test that "validates" a pack but would also pass on a pack with a garbage key
// is not validating anything. Inject an unknown key, assert the loader rejects it,
// and assert the ERROR NAMES THE KEY.
func TestGasCityLoaderRejectsAnUnknownKey(t *testing.T) {
	dir := copyPackToTemp(t)
	appendToPackTOML(t, dir, "\n[gonk_invented_table]\nnope = true\n")

	out, code := runInControllerWithPack(t, dir, packValidateCmd()...)
	if code == 0 {
		t.Fatal("the loader ACCEPTED an unknown top-level table.\n" +
			"That means this test proves NOTHING about our pack, and Task 4's\n" +
			"internal/packtest allow-list is the only thing standing between us and\n" +
			"a pack that silently ignores half of what we wrote.")
	}
	if !strings.Contains(out, "gonk_invented_table") {
		t.Fatalf("the loader rejected the pack but did not name the offending key:\n%s", out)
	}
}

// Same control for the two mistakes the format actively invites.
func TestLoaderRejectsAnOrderWithBothFormulaAndExec(t *testing.T)
func TestLoaderRejectsAnExecOrderWithAPool(t *testing.T)
```

**If `gc` turns out to have no offline validation path at all** (option 3 above also
fails — e.g. it refuses to start without a reachable bead store), then **say so in
ADR-004 and hand pack validation to Plan 06 as a first-class gate**, marked as *not
covered offline*. **Do not pretend `internal/packtest` is equivalent.** It is a
useful allow-list; it is not the loader, and the difference is exactly the class of
bug that only shows up in production.

- [x] **Step 4: Controller smoke test** — `test/images/controller_smoke_test.go`

```
gc --version         exits 0, matches GASCITY_REF
gonk-gate --version  == GONK_TAG
bd --version         == BD_VERSION
/opt/gonk/pack/pack.toml exists
id -u == 65532
```

- [x] **Step 5: Pin `bd`'s real subcommands (AD-2's escape hatch)**

`pkg/beadstore/bd.go` was written against *guessed* `bd` invocations. Confirm them
now, against the real binary, in the container:

```bash
podman run --rm --network=host "${REGISTRY}/gonk-controller:${GONK_TAG}" bd --help
podman run --rm --network=host "${REGISTRY}/gonk-controller:${GONK_TAG}" bd comment --help
podman run --rm --network=host "${REGISTRY}/gonk-controller:${GONK_TAG}" bd label --help
podman run --rm --network=host "${REGISTRY}/gonk-controller:${GONK_TAG}" bd list --help
```

Correct `bd.go` to match. **This is the one file the guess was allowed to be wrong
in** — that was the point of putting `beadstore` behind an interface. Every
`gonk-gate` test still runs against `Memory` and is unaffected.

- [x] **Step 6: Commit**

```bash
make pack-validate                                    # exit 0
go test ./test/images/ -tags images -race -count=1 -v # PASS
git add images pkg/beadstore/bd.go test/images Makefile
git commit -m "feat(images): controller image; validate the pack against Gas City's real loader, offline"
```

---

### Task 7: `gonk-intake` + `gonk-meter` images, exact tags, and the `no-latest` gate

**Files:**
- Create: `images/Dockerfile.intake`, `images/Dockerfile.meter`
- Modify: `Makefile`
- Test: `test/images/servers_smoke_test.go`, `test/images/nolatest_test.go`

- [x] **Step 1: Write both Dockerfiles**

Both are the same shape: a `golang:${GO_VERSION}` build stage producing a static
binary, then a minimal runtime.

```dockerfile
# images/Dockerfile.meter
ARG GO_VERSION
FROM golang:${GO_VERSION} AS build
ARG GONK_TAG
ARG BUILD_TAGS=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -tags "${BUILD_TAGS}" \
    -ldflags="-s -w -X main.version=${GONK_TAG}" -o /out/gonk-meter ./cmd/gonk-meter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gonk-meter /usr/local/bin/gonk-meter
# The private CA (docs/environment.md). trust-manager's `trust-bundle` ConfigMap is
# mounted here by the chart. NEVER disable TLS verification.
ENV SSL_CERT_FILE=/etc/ssl/orac/ca.crt
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/gonk-meter"]
```

**The `testclock` variant (Plan 03's HB-3, Plan 06's gate).** Plan 03 ships a
`//go:build testclock` clock seam so the e2e harness can cross a month boundary
without waiting a month. It is built as a **separate image**:

```bash
# The e2e-only meter. It has a clock that can be MOVED BY A FILE.
podman build --network=host --build-arg BUILD_TAGS=testclock \
  -f images/Dockerfile.meter -t "${REGISTRY}/gonk-meter:${GONK_TAG}-testclock" .
```

- [x] **Step 2: Write the guard that keeps the testclock out of production**

Plan 06's Task 9 Step 2 asserts *"the production binary contains neither the symbol
nor the literal."* **Make that a test in THIS repo**, at image-build time, where it
is cheap:

```go
//go:build images

// A meter whose clock can be moved by a file is a meter whose MONTH BOUNDARY can
// be moved by a file. In production that is a budget bypass: reset the window,
// reset the spend. The testclock build exists ONLY for the e2e harness, and the
// production image must not contain a trace of it.
func TestProductionMeterImageHasNoTestClock(t *testing.T) {
	bin := extractFromImage(t, meterImage(), "/usr/local/bin/gonk-meter")
	for _, needle := range []string{"GONK_TESTCLOCK_FILE", "testclock"} {
		if bytes.Contains(bin, []byte(needle)) {
			t.Fatalf("the PRODUCTION gonk-meter image contains %q.\n"+
				"A clock that can be moved by a file is a budget window that can be "+
				"reset by a file.", needle)
		}
	}
}

// And the converse, or the harness is testing a binary it cannot drive.
func TestTestclockMeterImageHasTheSeam(t *testing.T)
```

- [x] **Step 3: The `no-latest` gate**

`docs/environment.md` is unambiguous: *"Pin exact tags — never `latest`."*

```go
//go:build images

// Renovate autodiscovers and bumps PINNED tags. It cannot bump `latest`, and
// `latest` cannot be rolled back, cannot be reasoned about, and does not tell you
// what is running. This test walks every Dockerfile, every Makefile, every chart
// values file and every k8s manifest in the repo.
func TestNothingSaysLatest(t *testing.T) {
	// walk: images/**, Makefile, chart/** (Plan 05), test/**
	// fail on: `:latest`, `FROM x` with no tag, `image: x` with no tag
}

// A base image without a DIGEST is a base image that changed under you.
func TestBaseImagesArePinnedByDigest(t *testing.T)
```

- [x] **Step 4: `make images` and `make push` — exact tags, never `latest`**

```makefile
GONK_TAG ?= $(GONK_VERSION)-$(shell git rev-parse --short=12 HEAD)

# PODMAN, and --network=host: the CNI bridge is broken on this box
# (docs/environment.md). Do not "fix" it by removing the flag.
PODMAN := podman
BUILD  := $(PODMAN) build --network=host

.PHONY: images
images: no-latest
	$(BUILD) -f images/Dockerfile.agent      -t $(REGISTRY)/gonk-agent:$(GONK_TAG) .
	$(BUILD) -f images/Dockerfile.controller -t $(REGISTRY)/gonk-controller:$(GONK_TAG) .
	$(BUILD) -f images/Dockerfile.intake     -t $(REGISTRY)/gonk-intake:$(GONK_TAG) .
	$(BUILD) -f images/Dockerfile.meter      -t $(REGISTRY)/gonk-meter:$(GONK_TAG) .
	$(BUILD) --build-arg BUILD_TAGS=testclock \
	         -f images/Dockerfile.meter      -t $(REGISTRY)/gonk-meter:$(GONK_TAG)-testclock .

.PHONY: push
push: images
	# EVERY GITLAB RUNNER IS OFFLINE (docs/environment.md). CI has never executed
	# for this repo. The first images are pushed BY HAND, from this box, and that is
	# expected -- not a workaround to be embarrassed about.
	@echo "pushing $(GONK_TAG) to $(REGISTRY)"
	for i in gonk-agent gonk-controller gonk-intake gonk-meter; do \
	  $(PODMAN) push $(REGISTRY)/$$i:$(GONK_TAG); done
	$(PODMAN) push $(REGISTRY)/gonk-meter:$(GONK_TAG)-testclock

.PHONY: no-latest
no-latest:
	@! grep -rn ':latest' images/ Makefile chart/ 2>/dev/null || \
	  (echo "FAIL: ':latest' found. Pin an exact tag (docs/environment.md)." && exit 1)
```

- [x] **Step 5: Trivy-scan every image** (spec §10.3: *"image builds smoke-tested and
trivy-scanned"*)

```makefile
.PHONY: scan
scan:
	for i in gonk-agent gonk-controller gonk-intake gonk-meter; do \
	  trivy image --exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed \
	    $(REGISTRY)/$$i:$(GONK_TAG) || exit 1; done
```

`--ignore-unfixed` is deliberate: failing the build on a CVE with no fix available
teaches people to pass `--skip`, and then the scan is worthless.

- [x] **Step 6: Build, scan, push, verify the tags landed**

```bash
make images scan
make push
glab api "projects/69/registry/repositories?tags=true" | grep "${GONK_TAG}"
```

- [x] **Step 7: Commit**

```bash
go test ./test/images/ -tags images -race -count=1 -v
git add images Makefile test/images
git commit -m "feat(images): intake/meter images, testclock guard, and the no-latest gate"
```

---

### Task 8: commit provenance trailers (default on; usage default off)

Spec §6.1: *every bot-authored commit carries git trailers identifying the
generator.* `provenance.commit_trailers` defaults **on**; `provenance.include_usage`
defaults **off** — *"cost in public history is a per-project choice."*

**AD-4: this is a `prepare-commit-msg` git hook, not a prompt instruction.** A
trailer the agent is *asked* to write is a trailer the agent will sometimes forget,
reword, or hallucinate a number into. A hook is deterministic, and the agent cannot
talk its way around it.

**AD-5 (from Plan 03's AD-7, carried here as instructed): when meter says
`complete: false`, write `Gonk-Usage: pending` — NEVER a number.**
`GET /v1/cost/session/{key}` is read *at commit time, while the session is still
open*, so the spend rows for its own last calls have not landed and `complete` is
usually `false`. **Writing the number anyway publishes a wrong cost into permanent
git history**, where it cannot be corrected.

**Files:**
- Create: `cmd/gonk-gate/trailers.go`, `images/agent/prepare-commit-msg`
- Test: `cmd/gonk-gate/trailers_test.go`

- [ ] **Step 1: Write the failing test** — `cmd/gonk-gate/trailers_test.go`

```go
package main

import (
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

func TestTrailersDefaultOn(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: false},
		GonkVersion: "0.1.0", OpencodeVersion: "1.2.3", Model: "some-model",
		BeadID: "gk-1a2b", SessionKey: "gonk-42-issue-3", Rung: "cheap", Attempt: 2,
	})
	want := []string{
		"Generated-By: gonk/0.1.0 (opencode 1.2.3; some-model via litellm)",
		"Gonk-Bead: gk-1a2b",
		"Gonk-Session: gonk-42-issue-3",
		"Gonk-Rung: cheap",
		"Gonk-Attempt: 2",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("trailers missing %q:\n%s", w, got)
		}
	}
	// include_usage is OFF by default: cost in public history is a per-project choice.
	if strings.Contains(got, "Gonk-Cost-USD") || strings.Contains(got, "Gonk-Tokens") {
		t.Fatalf("usage trailers leaked with include_usage=false:\n%s", got)
	}
}

func TestNoTrailersWhenDisabled(t *testing.T) {
	got := renderTrailers(trailerInput{Provenance: meterapi.Provenance{CommitTrailers: false}})
	if strings.TrimSpace(got) != "" {
		t.Fatalf("commit_trailers=false must produce NOTHING, got:\n%s", got)
	}
}

func TestUsageTrailersWhenComplete(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: true},
		Cost: &meterapi.SessionCostResponse{TotalTokens: 128000, CostUSD: 0.42, Complete: true},
	})
	if !strings.Contains(got, "Gonk-Tokens: 128000") || !strings.Contains(got, "Gonk-Cost-USD: 0.42") {
		t.Fatalf("usage trailers missing:\n%s", got)
	}
	if strings.Contains(got, "pending") {
		t.Fatalf("a COMPLETE cost must be written as a number:\n%s", got)
	}
}

// *** AD-5 / Plan 03's AD-7. THE ONE THAT MATTERS. ***
//
// At commit time the session is still open, so meter usually has not seen the
// spend rows for its own last calls. Writing the number anyway publishes a WRONG
// COST INTO PERMANENT GIT HISTORY, where nobody can correct it and everybody will
// quote it.
func TestPendingUsageWhenCostIsIncomplete(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: true},
		Cost: &meterapi.SessionCostResponse{TotalTokens: 90000, CostUSD: 0.30, Complete: false},
	})
	if strings.Contains(got, "Gonk-Cost-USD") || strings.Contains(got, "Gonk-Tokens") {
		t.Fatalf("A COST WAS WRITTEN INTO GIT HISTORY THAT METER DOES NOT YET KNOW.\n"+
			"complete=false means the spend rows have not landed. The number in this\n"+
			"trailer is WRONG and it is PERMANENT:\n%s", got)
	}
	if !strings.Contains(got, "Gonk-Usage: pending") {
		t.Fatalf("want `Gonk-Usage: pending`, got:\n%s", got)
	}
}

// Meter unreachable at commit time is the same situation: we do not know the cost.
func TestPendingUsageWhenMeterIsUnreachable(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: true},
		Cost:       nil, // the lookup failed
	})
	if !strings.Contains(got, "Gonk-Usage: pending") {
		t.Fatalf("want pending, got:\n%s", got)
	}
	// And the commit still happens. A trailer lookup must NEVER block a commit --
	// losing the agent's work over a metadata footer is a terrible trade.
	if strings.Contains(got, "error") {
		t.Fatalf("an error must not end up in git history:\n%s", got)
	}
}

// A trailer is `Key: value` on its own line, in a trailer block at the END of the
// message, separated by a blank line -- or `git interpret-trailers` (and GitLab,
// and every tool that reads them) will not see them.
func TestTrailerBlockIsWellFormed(t *testing.T)

// Attribution safety (Plan 01 carry-forward): a value with a newline would forge a
// trailer. Refuse rather than sanitize -- a silently-mangled bead id is worse than
// a missing one.
func TestValuesWithNewlinesAreRefused(t *testing.T)
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./cmd/gonk-gate/ -run Trailer -v`
Expected: FAIL — undefined `renderTrailers`, `trailerInput`.

- [ ] **Step 3: Implement `cmd/gonk-gate/trailers.go`**

```go
// `gonk-gate trailers` renders the commit-provenance trailer block (spec 6.1). It
// is invoked by a prepare-commit-msg git hook installed into the rig clone at
// session start (AD-4) -- NOT by asking the agent to write trailers, because an
// agent asked to write a trailer will one day write a plausible-looking wrong one.
//
// TWO RULES, both of which are about not lying in permanent history:
//
//  1. include_usage defaults OFF. Cost in public git history is a per-project
//     choice (spec 6.1), not ours.
//  2. When meter says `complete: false` -- which at commit time it USUALLY does,
//     because the session is still open and its own last calls have not landed --
//     WRITE `Gonk-Usage: pending`, NEVER A NUMBER. A wrong cost in a commit
//     trailer is permanent, uncorrectable, and will be quoted back at you.
//
// And a rule about not losing work: a trailer lookup must NEVER fail a commit. If
// meter is unreachable, we write `pending` and move on. Losing the agent's actual
// work over a metadata footer is a terrible trade.
func renderTrailers(in trailerInput) string
```

- [ ] **Step 4: Write `images/agent/prepare-commit-msg`**

```sh
#!/bin/sh
# Installed into the rig clone's .git/hooks/ by the agent entrypoint (hooks are not
# cloned, so the session must install it).
#
# It appends gonk's provenance trailers to every commit the session makes. It is
# deterministic and the agent cannot bypass it with a well-worded commit message.
#
# IT MUST NEVER FAIL A COMMIT. If gonk-gate cannot reach meter, it writes
# `Gonk-Usage: pending` and exits 0. Losing real work over a footer is not a trade
# we make.
set -u
gonk-gate trailers --commit-msg-file "$1" || true
exit 0
```

- [ ] **Step 5: Run the tests**

Run: `go test ./cmd/gonk-gate/ -race -count=1 -v` → PASS.

- [ ] **Step 6: Prove it end to end in a real git repo**

A unit test on a string formatter does not prove that `git` will actually attach
these. Add `TestHookAttachesTrailersToARealCommit` (build tag `images`): in a temp
git repo, install the hook, `git commit`, then assert with **git's own reader**:

```bash
git log -1 --format='%(trailers:key=Generated-By,valueonly)'   # non-empty
git log -1 --format='%(trailers:key=Gonk-Bead,valueonly)'      # == gk-1a2b
```

If `git interpret-trailers` cannot see them, they are not trailers — they are just
text at the bottom of a commit message, and every tool that consumes them will
disagree with us.

- [ ] **Step 7: Commit**

```bash
gofmt -l . && go vet ./... && go test ./cmd/gonk-gate/ -race -count=1
git add cmd/gonk-gate/trailers.go cmd/gonk-gate/trailers_test.go images/agent/prepare-commit-msg
git commit -m "feat(gonk-gate): commit provenance trailers; never publish a cost meter does not know"
```

---

### Task 9: ADR-004, PLAN.md, and the amendment register

**Files:**
- Create: `docs/adr/ADR-004-pack-gate-and-images.md`
- Modify: `PLAN.md`

- [ ] **Step 1: Write `docs/adr/ADR-004-pack-gate-and-images.md`**

It records the decisions a future maintainer will otherwise "simplify" away.
**Each of these must appear with its reason, not just its conclusion:**

1. **Spec §6.2.3 is wrong; the rung gate is in two places.** A formula cannot make
   an HTTP call (`formula-spec-v2.md`), so the rung decision cannot live in a
   dispatch *formula* without putting an LLM on the money path. Gate 1 (intake) is
   an optimization; **Gate 2 (`gonk-dispatch`, an exec order) is the enforcement
   point.** Copy the drift table from this plan **verbatim** — especially the row
   marked CATASTROPHIC.
2. **The invariant:** *no formula is ever poured except by `gonk-gate dispatch`, and
   `gonk-gate dispatch` always calls `/v1/policy/decide` first.*
3. **The classifier is a sweeper order, not a formula step** — because every
   `[[steps]]` becomes a bead handed to an agent, and `[steps.check]` is a re-run
   verification loop that must stay side-effect-free.
4. **Infra failures never escalate a rung, and an escalation must be paid for.**
   Include the classification table and **the known false positive** (a pod evicted
   after ≥1 completion but before posting classifies as `gate-failed` and buys one
   unearned escalation — bounded, budget-checked, and handed to Plan 06 to measure).
5. **The bead marker is load-bearing.** It is how a deterministic gate reads an
   agent's output without judging prose. A prompt rewrite that drops it makes every
   project climb to its most expensive rung.
6. **No `[[webhook]]` in the pack** — `X-Gitlab-Token` is not in Gas City's closed
   five-scheme verify registry, and `hmac-sha256` will not substitute. Note the
   upstream PR opportunity; note that nothing depends on it.
7. **No `[[service]]` in the pack** — the budget enforcer's availability must not be
   coupled to the controller's lifecycle (owner decision).
8. **The pack names no model, no price, and no site.** Rung name → rung catalog →
   real LiteLLM model + synthetic price, all in the **operator's** config.
9. **The licensing constraint:** clean-room from MIT `gascity`; `gascity-packs` is
   all-rights-reserved and was not opened. Record this so an open-sourcing review
   can trust it.
10. **OD-3 (the opencode pin) and OD-7 (the metadata seam)** — record what was
    actually chosen. OD-7 is **answered/verified in design**: the
    `x-litellm-spend-logs-metadata` provider header, round-tripped on LiteLLM
    v1.92.0 (`docs/environment.md`). Plan 06 owns the LIVE proof against a real
    opencode + LiteLLM. Spec goal 4 rests on it; if the live proof ever fell
    through to "project-level attribution only", **say so in bold**.

- [ ] **Step 2: Update `PLAN.md`**

Set plan 04's status, and add to "Carried into later plans":

```markdown
| 04 pack & images | agents/formulas/orders, docker images | done |

## Carried into later plans (from plan 04)

- **Plan 02 MUST be amended:** intake **does** call `/v1/policy/decide` (Gate 1).
  Three places say it does not. See Plan 04, "Upstream amendments required".
- **Plan 05:** `networkPolicy.agentPodSelector` is **KNOWN**:
  `matchLabels: {app: gc-agent}`. OD-5 is answered. It is still not *enforced*
  (Flannel; Cilium suspended) — the selector being right and the policy doing
  nothing are two different facts, and both are true.
- **Plan 05:** the chart must ship **no rung catalog and no instance ladder
  defaults**, and must **fail to render without them**. A default that names a
  model gonk cannot see is worse than no default.
- **Plan 06:** HB-4 is satisfied — `POST /v1/policy/outcome` + a stable stdout JSON
  line + bead history. **The event-bus publish half is NOT delivered** (OD-6: the
  publish API is not in the Gas City facts). Plan 06 must subscribe to what exists.
- **The bead marker `<!-- gonk:bead:<id> -->` is a contract**, not a prompt detail.
```

- [ ] **Step 3: Commit**

```bash
git add docs/adr/ADR-004-pack-gate-and-images.md PLAN.md
git commit -m "docs(adr-004): the two-place rung gate, the outcome classifier, and the clean-room constraint"
```

---

## How Plan 06's **HB-4** is satisfied

Plan 06 is blunt: *"without (a) and (b), 'infra failures never escalate a rung' —
the single subtlest invariant in the system — cannot be distinguished from 'nothing
escalates at all', and the ladder tests are vacuous."* Here is exactly what it gets.

### HB-4a — publish the outcome classification

**Delivered:**

1. **`POST /v1/policy/outcome`** with `success` | `gate-failed` | `infra-failed` |
   `aborted`, bound to the `reservation_id` **meter minted**. This is the
   authoritative report and it is `meterapi`'s own enum — no new vocabulary.
   (`cmd/gonk-gate/sweep.go`, Task 3 Step 6.)
2. **A stable, machine-readable stdout line** from `gonk-gate sweep`, one per
   classified bead: `{"event":"gonk.outcome","bead_id":…,"session_key":…,
   "attempt":…,"rung":…,"outcome":…,"signals":{…}}`. The `signals` object is the
   **full `gate.Signals` input**, so a failing e2e test can see *why* it classified
   the way it did instead of just *that* it did. The controller captures order
   output; Loki indexes it (spec §8 already puts `bead_id` in Loki's labels).
3. **The bead's own history** (`bd`), which spec §8 names as the audit surface:
   append-only, per-bead, durable.

**NOT delivered — and this is honest, not a hedge:** HB-4a's literal words are
*"emits … onto the Gas City event bus."* **The event-bus publish API is not among
the Gas City facts available to this plan** (**OD-6**), and this plan will not
invent an endpoint for a subsystem it cannot see.

**Hand-off to Plan 06:** subscribe to whichever of the three surfaces above the
harness can actually reach. `POST /v1/policy/outcome` is the strongest — it is
*already* the thing the ladder tests care about, because it is what meter acts on.
If the SSE bus turns out to expose an order-completion event carrying our stdout
line, use that; if a publish API exists, wiring it into `sweep.go` is a handful of
lines. **Do not mark the ladder tests green against a surface that does not exist.**

### HB-4b — a deterministic gate failure

**Delivered, and it falls out of the classifier's design rather than being bolted
on.** The harness gets **two independent dials**, and they produce *different*
outcomes — which is precisely what makes the invariant testable rather than vacuous:

| Stub-model behaviour | What the session does | `gate.Signals` | Outcome | Ladder |
|---|---|---|---|---|
| Canned response with a **tool call** that posts the marker comment | posts the artifact | `ArtifactPresent: true`, `ModelTokens > 0` | **`success`** | done |
| Canned response with **no tool call** (prose only) | burns tokens, posts nothing | `ArtifactPresent: false`, `ModelTokens > 0` | **`gate-failed`** | **escalates** |
| Stub returns **HTTP 500** on every call | no completion at all | `ArtifactPresent: false`, `ModelTokens == 0` | **`infra-failed`** | **retries SAME rung** |
| Stub is fine, but **GitLab** 500s on the notes query | unknown | `ArtifactUnknown: true` | **`infra-failed`** | **retries SAME rung** |

Rows 2 and 3 are the two halves of `TestInterleavedFailuresCountOnlyGateFailures`.
Row 2 is HB-4b exactly as asked: *"a canned stub-model response that produces no
artifact must drive the gate to `gate-failed`, reliably, every run."*
**And row 3 is the one that makes row 2 mean something** — without it, a green
ladder test cannot distinguish a working ladder from a dead one.

The mechanism that separates them is **`ModelTokens`**: an escalation must be paid
for. A session that never got a completion did not *fail at* the work; it never got
*to* the work.

**One warning for Plan 06.** Row 3 depends on meter's spend view catching up
(`spend_as_of >= session end`), which is why `sweep` forces a spend sync (HB-2) and
polls a predicate. **If it times out, the classification is `infra-failed`, not
`gate-failed`** — uncertainty never escalates (AD-6). That is the right default, but
it means **a harness with a broken `POST /admin/spend/sync` will see every gate
failure as an infra failure and the ladder will never climb.** If your escalation
test goes mysteriously quiet, check HB-2 first.

---

## Upstream amendments required (other plans must change because of this one)

**These are amendments, not suggestions. This plan may not edit those files.**

### Plan 02 (gitlab-intake) — REQUIRED, and one of them is now factually wrong

1. **"Intake does NOT call `/policy/decide`" is WRONG.** Owner decision: **intake
   gates the first dispatch.** It calls `POST /v1/policy/decide` and fires the order
   **only** on `run`, passing `rung`, `model`, `metadata_json`, `key_ref`,
   `reservation_id` and `attempt` as order vars; on `defer` it records `retry_after`
   and fires nothing; on `deny` it fires nothing. Three places say otherwise and all
   three must change:
   - the "Division of responsibility" table (*"Rung choice (`/policy/decide`) |
     **meter**, called by the **pack**"* → **called by intake AND the pack**);
   - *"Consequence: intake never sees a `defer`"* — **it does now**;
   - `pkg/intake/dispatch.go`'s comments and `OrderRequest` (which grows the fields
     above).
   **What does NOT change:** intake still never sends an attempt count
   (`DecideRequest` has no such field, deliberately), and intake still has **no
   quiet-hours code** — quiet hours arrive as a `defer`, which intake now simply
   *handles* instead of never seeing.
2. **Add `intake.MayFire(decision string) bool`** — three lines. It is what makes
   Task 3 Step 8's shared-semantics test able to detect Gate-1/Gate-2 drift
   mechanically instead of by comment.
3. **`intake.HTTPDispatcher` is no longer a placeholder.** Its OD-A is **closed**:
   the real contract is `POST /v0/city/{cityName}/order/gonk-dispatch/run` with body
   `{"vars": {…}}`. Replace it with `pkg/gcapi` (Task 2). **Note the route has no
   auth** — admission is by network position.
4. **The onboarding template's `ladder:` must not be a hardcoded `[qwen-local]`.**
   It must be rendered from the **operator's instance ladder** (chart values). See
   the Plan 05 amendment below; a literal here would name a rung the operator's
   catalog may not contain, and ADR-002's empty-ladder rule then disables every
   freshly-onboarded project.

### Plan 03 (gonk-meter) — REQUIRED

1. **`/v1/policy/decide` must be idempotent on an OPEN RESERVATION.** With two gate
   call sites, a bead can be `/decide`d twice before any outcome is reported (intake,
   then `gonk-dispatch`). Attempt and rung are naturally stable (meter derives them
   from **outcome** history, which has not changed) — but **the reservation is not**.
   A second `/decide` must **return the existing open, unexpired reservation for
   `(project, bead_id, session_key)`**, not mint a second one.
   *If it mints a second:* two reservations hold **double the budget headroom** for
   one attempt, and `POST /v1/policy/outcome` — which is bound to a reservation
   meter minted — will be bound to one of them while the other leaks until TTL. The
   project's remaining budget is understated, and a `defer` fires early for a reason
   nobody can find.
2. **The pack is a legitimate caller of `GET /v1/projects/{project}`** (read-only —
   `gonk-gate trailers` needs `effective.provenance`) **and of
   `POST /admin/spend/sync`** (the sweeper forces a spend poll before classifying).
   Plan 03 lists `/admin/spend/sync` as an e2e-harness endpoint (HB-2); **it is now
   a production dependency of the pack.** It must accept the pack's bearer token,
   and it must be safe to call every 30 s.
3. **OD-7's fallback, if it is taken:** if opencode cannot send custom headers, meter
   must mint a **short-lived, per-attempt** virtual key whose LiteLLM key-metadata
   carries the atags — i.e. `KeyRef` becomes per-attempt rather than per-project
   (Plan 03's AD-1). **Do not build this speculatively.** The primary header
   mechanism (`x-litellm-spend-logs-metadata`) is now **VERIFIED** on LiteLLM
   v1.92.0, so this per-attempt-key path is contingency-only. Task 5 Step 4 decides it.

### Plan 05 (chart) — REQUIRED

1. **OD-5 is ANSWERED. `networkPolicy.agentPodSelector` is `matchLabels: {app: gc-agent}`.**
   Gas City's k8s session provider labels agent pods **hardcoded** in
   `internal/runtime/k8s/pod.go`: `app: gc-agent`, `gc-session: <label>`,
   `gc-agent: <agentLabel>`, annotation `gc-session-name`; the provider itself lists
   by `app=gc-agent`. Ship it as the **default**, not as a required value.
   **This does not close the gap.** NetworkPolicy is **not enforced** on this cluster
   (Flannel; Cilium suspended). A *right* selector on an *unenforced* policy still
   blocks nothing. Plan 06's egress-denial test stays **written and skipped**, and
   un-skipping it is the gate. Do not let "OD-5 answered" get quoted as "spec §9
   satisfied" — they are not the same sentence.
2. **NO SITE-SPECIFIC MODEL NAMES IN CHART DEFAULTS.** `onboarding.defaultRung:
   qwen-local` must **stop being a default**. The rung *name* is operator-chosen; the
   rung *catalog* maps it to a real LiteLLM model (`qwen3-14b`, `qwen3-8b-bailey` on
   this cluster — **there is no model called `qwen-local`**) plus a synthetic price.
   **The chart must FAIL to render** when `operatorConfig.rungs` or
   `operatorConfig.instance.ladder` is empty, and when `onboarding.defaultRung` is
   not in the instance ladder (guard G5 already does the last one — keep it, and
   remove the default that makes it pass by accident).
   *Why:* a chart that "works out of the box" by pointing at a model the operator's
   LiteLLM has never heard of fails at the first token, and gonk gets blamed.
3. **Add the two new images** to values: `gonk-agent` and `gonk-controller`
   (`registry.orac.local/agentic/gonk-project/<image>:<tag>`, exact tags). The chart
   does not deploy them — Gas City's session provider pulls the agent image, and the
   controller image is the city's — but the **values must carry the pins** so
   Renovate can bump them and so the e2e values file can override them per run.
4. **The `testclock` meter image is a separate tag** (`…:<tag>-testclock`), not a
   flag. `values-e2e.yaml` (HB-5) selects it. Task 7 Step 2 guards the production
   image against containing the seam at all.

### Plan 06 (e2e harness) — REQUIRED

1. **HB-4 is satisfied with a caveat.** See "How Plan 06's HB-4 is satisfied" above.
   The classification is delivered via `POST /v1/policy/outcome` + a stable stdout
   JSON line + bead history. **The event-bus publish half is not delivered** (OD-6).
   Subscribe to what exists; **do not mark the ladder tests green against a surface
   that is not there.**
2. **The escalation test depends on HB-2.** `gonk-gate sweep` forces a spend sync
   and waits on `spend_as_of` before it will call anything `gate-failed`. **If
   `POST /admin/spend/sync` is broken, every gate failure classifies as
   `infra-failed` and the ladder never climbs** — which will look exactly like "the
   ladder is broken". Check HB-2 first.
3. **NEW hand-off — verify the attribution seam (OD-7) live.** The mechanism is
   **answered/verified in design** (the `x-litellm-spend-logs-metadata` provider
   header round-tripped on LiteLLM v1.92.0; `docs/environment.md`). Plan 06 owns the
   LIVE proof: assert that a LiteLLM spend row produced by a **real agent pod**
   carries **all seven** `gonk_*` metadata keys, and detect the enterprise-gate
   regression (fall back to `x-litellm-tags` → `request_tags` if a LiteLLM upgrade
   starts enforcing it). **Nothing before Plan 06 exercises this against a real
   opencode**, and **spec goal 4 rests on it.** If the live proof fails, spend is
   attributable to the project and no further, and that is a finding the owner must
   hear.
4. **NEW hand-off — pack validation, if `gc` has no offline path.** Task 6 tries hard
   to run the real loader in a container. If `gc` turns out to have no offline
   validate path at all, pack validation becomes a **first-class Plan 06 gate**, and
   `internal/packtest` must be described as an allow-list, **not** as loader
   validation.
5. **NEW hand-off — measure the classifier's known false positive.** A pod evicted
   *after* ≥1 completion but *before* posting classifies as `gate-failed` and buys
   one unearned escalation. The K-series kill tests already evict pods: **count how
   often it fires.** If it is common, it becomes an upstream ask for a
   pod-termination signal from Gas City's session provider.
6. **`bd`'s real CLI surface** (AD-2) is pinned in Task 6 Step 5 against the real
   binary. If the e2e run shows `beadstore.BdCLI` mis-driving `bd`, exactly one file
   changes.

---

## Hand-offs: what this plan genuinely CANNOT verify

Stated plainly, because a plan that claims more than it proves is worse than one
that admits the gap.

| # | Claim | Why not verifiable here | Who verifies |
|---|---|---|---|
| 1 | Gas City's controller fires `gonk-dispatch` when intake POSTs the order-run route | **Gas City is not deployed.** `pkg/gcapi` is tested against a fake supervisor. | Plan 06 (L3) |
| 2 | A poured formula actually spawns an opencode pod, with our vars reaching the agent | ditto | Plan 06 (L3) |
| 3 | `[steps.check]`'s exec runs where and how we think it does | ditto — and the exact `[steps.check]` keys are transcribed, not tested | Task 6 (loader), then Plan 06 |
| 4 | **The attribution seam (OD-7)** — that every LiteLLM request carries all seven `gonk_*` tags | Mechanism **verified** on LiteLLM v1.92.0 (`x-litellm-spend-logs-metadata` header); the LIVE proof still needs a real agent pod + LiteLLM. **Spec goal 4 rests on this.** | Task 5 Step 4 (design, verified), **Plan 06 (live proof)** |
| 5 | Session resume across pod recreation (spec §11.4 — a **named risk**) | Needs a real k8s session provider and real opencode | Plan 06 |
| 6 | The classifier's false-positive rate (evicted-after-first-token) | Needs real pod evictions | Plan 06 (K-series) |
| 7 | The event-bus publish API (OD-6) | **Not in the facts available.** Not invented. | Plan 06 |
| 8 | That the agent-pod NetworkPolicy actually denies egress | **The cluster does not enforce NetworkPolicy at all** (Flannel; Cilium suspended). A right selector on an unenforced policy blocks nothing. | Plan 06 — **written and SKIPPED** until Cilium lands |

---

## Definition of done

- [ ] `gofmt -l .` prints nothing; `go vet ./...` clean; `golangci-lint run ./...` clean (v2.12.2, pinned with CI — see PLAN.md).
- [ ] `go test ./... -race -count=1` passes.
- [ ] `go test ./test/images/ -tags images -race -count=1` passes (needs podman, `--network=host`).
- [ ] `make lint-pack` — **the pack names no model, holds no credential**.
- [ ] `make pack-validate` — **Gas City's real loader accepts the pack**, *and rejects a pack with an injected unknown key* (the negative control is what gives the positive its meaning).
- [ ] `make no-latest` — **nothing anywhere says `:latest`**; base images pinned by digest.
- [ ] `make scan` — trivy clean (HIGH/CRITICAL, fixed only).
- [ ] All five images built and **pushed by hand** to `registry.orac.local/agentic/gonk-project/` at `${GONK_VERSION}-${SHORT_SHA}` (every runner is offline; this is expected).
- [ ] `grep -rniE "openai|anthropic|completion" cmd/gonk-gate/ pkg/gate/` → **nothing**. There is no LLM on the gate path.
- [ ] `TestDispatchAlwaysDecidesEvenWhenVarsCarryARung` passes. **This is the test that stops ladder escalations from spending unmetered. If it is ever deleted, the money door is open.**
- [ ] `TestInfraFailureNeverEscalates` and `TestEscalationRequiresSpentTokens` pass.
- [ ] `TestSweepIsIdempotentAcrossRuns` passes (a cooldown order runs every 30 s forever).
- [ ] `TestPendingUsageWhenCostIsIncomplete` passes. **No cost meter does not know goes into permanent git history.**
- [ ] `TestProductionMeterImageHasNoTestClock` passes.
- [ ] ADR-004 written, **including the known false positive and the licensing statement**.
- [ ] The **Upstream amendments** above are filed against Plans 02, 03, 05 and 06. **Plan 02's "intake does not call `/policy/decide`" is the one that is now factually wrong; do not let it sit.**
- [ ] `PLAN.md` updated.
- [ ] **`gascity-packs` was never opened.** If it was, say so — the open-sourcing review depends on this being true, not on it being claimed.
