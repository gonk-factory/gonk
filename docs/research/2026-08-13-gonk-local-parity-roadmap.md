# Roadmap: local parity with Warp's cloud platform

**Date:** 2026-08-13
**Bead:** gonk-gqx (research), roadmap items filed individually below
**Companions:** `2026-08-13-warp-comparison.md` (why not adopt), `2026-08-13-warp-blog-lessons.md` (source mechanisms), `warp-platform-capability-inventory.md` (living capability baseline)

**Goal.** Reach functional parity with Warp's Oz "cloud software factory" — triage → spec → implement → review → verify → ship → monitor — running entirely on self-hosted infrastructure with local inference, no vendor control plane, and gonk's existing invariants intact (hard per-project budgets, no LLM judges in the ladder, zero forge credentials in agent pods, fail-closed config).

---

## 0. Three constraints that reshape every item

Warp's mechanisms assume things gonk does not have. Every port below is filtered through these:

1. **Local models — and the window is 128K, not 16K.** ⚠️ **CORRECTED 2026-08-13.** An earlier version of this document asserted a 16K ceiling and built Phase ordering on it. That was wrong. `num_ctx 16384` is a property of **one Ollama alias**, `qwen3-14b` (`gonk-m4k`) — which happens to be the alias gonk's rung catalog names (`gonk-gyj`). LiteLLM actually serves seven local models, of which **five are 128K**, including a vLLM deployment on bailey hosting `RedHatAI/Qwen3.6-35B-A3B-NVFP4` (`--max-model-len=131072`, `--gpu-memory-utilization=0.35` ≈45GB, MoE with 3B active, fp8 KV cache, prefix caching, MTP speculative decoding) exposed as `qwen3.6-35b-vllm`. Note the trap: vLLM's `--max-num-batched-tokens=16384` is a *prefill batch budget*, not a context window, and reads exactly like one.
   **Consequence:** Warp's context-isolation patterns are **useful optimizations, not preconditions.** The binding constraint on Phase 1 is not "will the context fit" but "will the model emit a valid edit" — which moves the deterministic validator and the annotated-diff coordinate scheme up, and moves the search subagent down. See §11c.
   **What is newly available and matters:** the vLLM deployment runs `--enable-auto-tool-choice` with `--tool-call-parser=qwen3_xml` and `--reasoning-parser=qwen3`. Native tool-call parsing is a direct input to implementation-agent reliability, and it makes Warp's "tool names and schemas are model-specific" finding immediately testable rather than theoretical.
2. **No LLM judges in the ladder.** Warp's determinism is *contract-shaped* — the LLM still emits `verdict: APPROVE|REJECT`, only the vocabulary and coordinates are machine-checked. gonk's is *judgment-shaped*: `pkg/gate.Classify` is pure and total, protected by a regression test. Where Warp uses model judgment for classification, gonk needs a deterministic substitute or an explicit human gate.
3. **One operator.** Warp's "highest-leverage checkpoint" (human spec approval) and "agents comment, humans merge" both consume the scarcest resource here. Anything that adds review burden without removing more must be rejected.

**One more asymmetry, in gonk's favour:** Warp's own build-vs-buy framework lists four vendor red flags — single-harness lock-in, vendor capturing your data, compute inflexibility, forced token reselling. gonk has all four properties by construction. Their advice to "own the idiosyncratic parts and buy the rest" is unavailable here, because buying means routing inference through a vendor cloud. So gonk owns more than they'd recommend, which makes their **MVP-trap warning** the right check against every item below: *"the MVP is pretty easy, but the remaining pieces to manage agents at scale is actually where the bulk of the work is."*

---

## 1. The sequencing principle: throughput before intake

The single most important finding in the research is a warning. Warp's CEO on their own public factory, June 2026:

> "My view is that it's half-working" … "I look at build.warp.dev and see **1300 issues in the ready-to-implement state**."

Triage scales. Implementation doesn't. Their published rate is **20–30% of PRs fully automated**, and the marketing "60%" includes human-in-the-loop runs.

gonk is at exactly the same inflection: triage works (issue 21 → `qwen3-14b` → well-formed effects batch), and there is no implementation path at all. **Do not widen intake before the implement→verify→MR path can absorb the queue.** That defers the `gonk-6po` source-beads epic and instance-wide watching (`gonk-8s0`) behind Phases 1–3, despite them being ready to work.

Their build guide agrees, and states the ordering rule explicitly: *"Each part adds one skill to the loop, in the order that makes the next part possible. You can stop at any part and have something that works."* Review is called out as the bottleneck that appears **only after** agents produce code at volume — so review comes after implementation, not before.

---

## 2. Parity table

Legend: **HAVE** = shipped in gonk · **FLIGHT** = open bead, work started or specified · **BUILD** = new work, in this roadmap · **NO** = deliberately out of scope (§7)

### Core loop

| Warp capability | gonk today | Status | Phase |
|---|---|---|---|
| Event intake (webhook → orchestrator) | gonk-intake, verify-before-parse, dedupe, reconciliation | **HAVE** | — |
| Durable intake surviving restarts | source-beads epic `gonk-6po` (slices 1–5), `gonk-icj` webhook parking | **FLIGHT** | 5 |
| Triage agent → structured decision | triage agent + effects batch + broker | **HAVE** | — |
| 4-state triage rubric with routing hints | 2 outcomes, no roadmap/vision inputs | **BUILD** | 2 |
| `roadmap.md` / `vision.md` as triage inputs | none | **BUILD** | 2 |
| Spec agent → PRODUCT.md / TECH.md | none | **BUILD** | 2 |
| Human spec approval gate | none | **BUILD** | 2 |
| Implementation agent → branch + MR | **none** — this is the gap | **BUILD** | 1 |
| Code review agent → structured findings | none | **BUILD** | 3 |
| Verification (deterministic, sandboxed) | `gonk-qhe`, `gonk-3jm` | **FLIGHT** | 3 |
| Computer-use / visual verification | none | **NO** (§7) | — |
| Monitor agent → files issues | none | **BUILD** | 6 |
| Scheduled "janitor" agents | `gonk-sweep` order exists (cooldown) | **BUILD** | 6 |

### Agent runtime

| Warp capability | gonk today | Status | Phase |
|---|---|---|---|
| Container-per-session, destroyed after run | k8s pods via Gas City provider | **HAVE** | — |
| Environment = image + repos + setup commands | split across image, pack, chart values | **BUILD** | 4 |
| Per-session repo checkout, no forge creds | `pkg/rig` (grant-gated tarball, 64 MiB, 30-min TTL) | **HAVE**/`gonk-j9z` | 0 |
| Sidecar-merged harness tooling | opencode baked into `Dockerfile.agent` | **BUILD** | 4 |
| Warm shared cache volume | none | **BUILD** | 4 |
| Instance shapes (per-run CPU/mem) | fixed pod resources | **BUILD** | 4 |
| Structured prompt delivery | **keystrokes into TUI** (`gonk-e9m`) | **FLIGHT** | 0 |
| Long-running command control (PTY writes) | none | **BUILD** | 4 |
| Context-dependent tool availability | none | **BUILD** | 4 |
| Search/research subagent, own context window | none | **BUILD** | 4 |
| Cross-vendor model failover | none — `gonk-ob5` fails *open* to cloud | **FLIGHT**→**BUILD** | 0, 5 |
| Model routing by task complexity | rung ladder (budget-driven, not complexity) | **BUILD** | 5 |
| Multi-harness (Claude Code / Codex / opencode) | opencode only; rung seam designed | **BUILD** | 5 |
| Orchestration parent/child, one level | none | **NO**/later | — |
| MCP support | none | **BUILD** | 5 |

### Platform

| Warp capability | gonk today | Status | Phase |
|---|---|---|---|
| Hard budget enforcement | LiteLLM virtual keys + synthetic pricing — **gonk is ahead** | **HAVE** | — |
| Per-project cost attribution | atags design; broken on submit path (`gonk-m6t`) — **still ahead of Warp** | **FLIGHT** | 1 |
| Per-run cost visibility where work is seen | Grafana only | **BUILD** | 5 |
| Budget approach-alerts | none | **BUILD** | 5 |
| Run state machine + error taxonomy | ad-hoc | **BUILD** | 5 |
| Worker/agent metrics (OTel) | intake/meter metrics; no agent-run metrics | **BUILD** | 5 |
| Run record: prompt, commands, artifacts | beads + Gas City sessions, partial | **BUILD** | 5 |
| Session sharing / live steering | none | **NO** (§7) | — |
| Secrets as write-only, env-injected | file mounts, 0400, two rotation slots — **gonk is ahead** | **HAVE** | — |
| Skills as versioned files | pack agents + prompt templates | **BUILD** | 2 |
| Self-improvement loop (skill → MR) | none | **BUILD** | 4 |
| Evals per skill | L1/L2/L3 harness for services; none for agent behavior | **BUILD** | 4 |
| Network egress restriction | NetworkPolicy unenforced (Flannel; Cilium suspended); `gonk-7oz` | **FLIGHT** | 5 |

---

## 3. Phase 0 — Unblock the line

Nothing in this roadmap runs until these clear. All are existing beads.

| Bead | What | Why it blocks |
|---|---|---|
| `gonk-mzm` (P0) | Registry volume full | No image can be pushed at all |
| `gonk-e9m` (P1) | Prompts delivered as **keystrokes** into opencode's TUI; a `!` in issue text runs a shell in the pod | Every phase below sends structured prompts. The prompt-by-reference plan exists (T0 passed, T1–T6 unstarted). This is also the standing injection risk |
| `gonk-ob5` (P1) | opencode fails **open** to a built-in cloud provider when its config is unreachable | Silently defeats local-only inference *and* budget enforcement. Highest-severity integrity bug in the stack |
| `gonk-bgx`, `gonk-msz` (P1) | Scaffold gates every newly-onboarded project and is on the broken formula path; scaffold is told the repo is checked out but nothing clones it | No new project can reach triage |
| `gonk-j9z` (P1) | Wire rig checkout end to end (cmd mains, chart env, per-session URL) | Implementation agents need a working tree |
| `gonk-7oz` (P1) | Agent NetworkPolicy allows any port to the whole gitlab namespace | Falsifies the "no forge access" claim the broker model rests on |

**Exit criterion:** a fresh project onboards, receives a structured prompt (not keystrokes), checks out its repo, and reaches triage — with inference provably local.

---

## 4. Phase 1 — Close the loop to a merged MR

The point of this phase: turn `ready-to-implement` into a human-reviewable MR. Nothing else in the factory matters until this exists.

**1.1 Implementation agent.** A new pack agent + order, mirroring the triage agent's shape: read the issue, make the smallest cohesive change, run validation, emit an effects batch that includes a branch and MR. Port their scope-control rule verbatim:

> "Make the smallest cohesive change that satisfies the issue… Do not bundle unrelated refactors, formatting churn, dependency upgrades, or opportunistic cleanup. **If the issue turns out to be much larger or more ambiguous than expected, stop and comment with a concise recommendation rather than producing a risky partial implementation.**"

**1.2 Extend the effects vocabulary to code.** `pack/agents/triage/effect-shape.toml` currently allows `comment {1,1}` and `label {0,N}`. Implementation needs `branch`, `commit`, `mr`. The broker keeps sole write authority — the agent never touches GitLab. Preserve the existing asymmetry (removes non-fatal, adds fatal) and scope every mutation to the triggering issue/MR.

**1.3 Anti-lying guardrails.** Their skills carry a cluster of these, each apparently written after an incident. Port them into the effect validator so they are *enforced*, not merely requested:
- An effects batch claiming completion **must** carry a real MR URL; a batch that says "a PR will be opened later" is rejected.
- A batch claiming validation passed must carry the command and exit code.
- Progress comments capped (start comment + at most two).

**1.4 Validation commands are config, not inference.** Their `fix-errors` skill hardcodes the repo's real gates (`cargo fmt --check`, `clippy -D warnings`, `nextest run`). gonk's equivalent belongs in `.gonk.yml`, resolved fail-closed like everything else, so the agent never guesses how to test.

**1.5 Fix `gonk-m6t`** (attribution never reaches the pod on the submit path). Warp's Enterprise analytics has **no per-project attribution** — this is a genuine gonk differentiator, and it's currently broken.

**1.6 Deterministic MR↔bead join key.** Copy their `<!-- factory-agent: {...} -->` PR-body metadata block. gonk should stamp the bead id, rung, skill/prompt version, and session alias into the MR description. This is the join key Phases 3–4 need to attribute outcomes; retrofitting it later is expensive.

**Exit criterion:** a labelled issue produces a human-reviewable MR, with provenance trailers and correct per-bead attribution, without a human touching a terminal.

---

## 5. Phase 2 — Route honestly: the spec gate

**2.1 `roadmap.md` + `vision.md` at repo root.** The cheapest high-leverage item in the entire research. They convert triage from "is this well-written?" to "does this belong?", and they are what make a `wait-to-implement` state meaningful. Structure to copy: numbered product areas, each with a **routing hint** ("broad changes to editing state… may need specs"), plus a closing `## Triage guidance` section; `vision.md` carries the **non-goals**.

**2.2 Four-state triage rubric.** `ready-to-implement` / `ready-to-spec` / `needs-info` / `wait-to-implement`. Note what they deliberately omit: **no confidence score, no priority, no severity**. Low confidence is handled structurally —

- *"When evidence sits between states, choose the more cautious state."*
- `needs-info` must state *"the smallest set of concrete questions whose answers would unblock re-triage."*
- *"The goal is to route work honestly, not to make every issue appear actionable."*
- *"Do not use [`wait-to-implement`] merely because an issue is difficult; complex but cohesive work is usually ready-to-spec."*

This composes with existing beads `gonk-kxg` (classify before implementing; route non-code diagnoses to an issue) and `gonk-07d` (author role gates what an outcome may become).

**2.3 Spec agent → `specs/<issue-slug>/PRODUCT.md` + `TECH.md`.**
- `PRODUCT.md`: user-visible behavior only, as **numbered testable invariants**. No implementation, no validation section. *"Behavior is the spec. Everything else is framing."*
- `TECH.md`: context with **commit-pinned links** (`path:line @ <sha>`), proposed changes, and a Testing section that references PRODUCT invariants **by number** rather than restating them.
- **The numbering is the join key** across spec → implementation → verification. Cheap, and it's what makes Phase 3 checkable.
- Length heuristics: trivial → no spec; small → 30–60 lines; medium → 80–150.

**2.4 The human gate.** Spec MR requires human approval before `ready-to-implement` is applied. This is their named *"highest-leverage checkpoint"* — *"human review moves upstream, so you approve a spec instead of correcting a finished diff."* For a one-operator factory this is the item that most reduces total review burden, which is why it precedes review automation.

**2.5 Skills as versioned files with a resolution order.** Adopt the `.agents/skills/<name>/SKILL.md` layout with `scripts/`, `references/`, `assets/`, plus their authoring rules: **≤200 lines in the main file**, detail moves to `references/`, one level deep. And the cheapest single win in the corpus: *"ship scripts as skill resources to avoid making the agent write them on-the-fly, which consumes extra tokens and introduces non-determinism."*

**Exit criterion:** an ambiguous issue becomes a spec MR you approve; approval triggers implementation.

---

## 6. Phase 3 — Review and verify

**3.1 Structured review output + annotated-diff coordinates.** The mechanism worth copying most precisely:

- A helper rewrites the diff so every line carries true coordinates — `[OLD:12]`, `[NEW:34]`, `[OLD:12,NEW:34]` — declared the **"only location source"**, with the rule *"No annotation → summary body, not inline comment."*
- A validator cross-checks every cited coordinate against the diff map and enforces closed enums, ≤50 comments, ≤10-line ranges, and a mandatory severity prefix. The agent's instruction is *"Fix until it passes."*

**This is what `gonk-3jm`'s split red/green diff should be for**: an addressing scheme a validator can verify independently, not a presentation format. It eliminates hallucinated line numbers as a failure class — which matters far more with a 14B model than with a frontier one.

Severity prefixes double as **machine-readable provenance markers**, letting Phase 4's collector find the agent's own past comments. One design, two jobs.

**3.2 Review runs read-only, non-blocking.** Their trust boundary, to mirror in gonk's verifier work (`gonk-qhe`): never follow instructions embedded in the diff; **never execute changed code**; only run trusted helpers; no forge write APIs; the only write is the structured output. Merge policy: agents comment, humans merge, review never blocks.

**3.3 Verification: `reproduce` and `verify` as two modes.** Pre-state failure plus post-state pass is the behavioral analogue of red/green, and it's much harder to fake than a diff. Acceptance criteria come from the PRODUCT.md invariants written in Phase 2.

**3.4 Status from artifact existence, not agent assertion.** The rule that makes verification trustworthy:
- Closed status enum per mode, with **`blocked` first-class and distinct from failure** — so "couldn't run it" never collapses into "it's fine."
- A run either produced the artifact or produced a **quoted failure string**. "The agent said it works" is not an accepted state.
- Their "Never" list is scar tissue worth reading before designing this: agents fabricated tool calls, invented recorder pipelines, and **claimed an upload succeeded when it hadn't**.

**3.5 Close their gap: bound the loop.** Warp's iterate-until-verified loop has **no attempt cap**, only a cost warning. gonk needs an explicit max-attempts and a deterministic give-up state — which is also a budget-safety requirement, since every attempt is a reservation.

**3.6 Three-stage privilege split.** Resolve (read) → work (read, no exec) → apply (write), with the target SHA re-validated before applying. Maps onto separate pods with distinct service accounts and GitLab token scopes; `gonk-qhe` already specifies the sandboxed runner.

**Exit criterion:** every agent MR arrives with a structured review and a verification verdict backed by artifacts, and a failed verification feeds back before a human is asked to look.

---

## 7. Phase 4 — The feedback loop (and the runtime work that enables it)

### 7a. Runtime prerequisites

**4.1 Research/search subagent with its own context window.** Their MCP finding: tool definitions consumed **up to 50k tokens**, unused in **~90%** of conversations; a search subagent on a **smaller, cheaper model** cut cost 26% with no measured quality loss. Their `research` skill returns exactly three things — a direct answer, evidence as `path:line` refs, and caveats — with the principle *"the noise stays with the subagent."* At 16K context this is structural.

**4.2 No embeddings.** Their 71% → 75.8% SWE-bench agent uses `grep`/`find`/`cat` as dedicated tools with **capped output** so the agent scrolls rather than swallowing files. Do not build a vector index; build windowed search tools.

**4.3 Edit reliability.** *"Edits are still the most common failed tool call."* Port: the exact → indentation-agnostic → Jaro-Winkler ladder; failure reasons returned so the agent self-corrects; absolute paths demanded; and **return only the changed hunk ± k lines, never the whole file** (they were echoing 5,000 lines for a one-line change; fixing it improved cost *and* quality).

**4.4 Context-dependent tool availability.** Remove edit/run tools while a long-running command is in flight. Deterministic behavior control that costs no context. Also fixes the **pager hang** — an agent that runs `git log` eventually deadlocks; force `PAGER=cat`/`--no-pager` in the pod at minimum.

**4.5 Environment as one declarative object** (image + repos + ordered setup commands + runtime config), resolved fail-closed from `.gonk.yml` like everything else. Today this knowledge is spread across the agent image, pack `agent.toml`, and chart values.

**4.6 Sidecar-merge the harness.** Their base image *"doesn't even need to have Warp installed"*; tooling is mounted separately, so harness updates ship without touching user images. Decouples opencode upgrades from `Dockerfile.agent` — and directly serves `gonk-e0o` (split a base image so per-commit builds stop re-fetching the toolchain).

**4.7 Warm cache volume** carrying git objects and the Go module cache. First-token latency matters even on long tasks.

### 7b. The loop itself

**4.8 Versioned marker as the join key.** Every agent-authored comment starts with a hidden marker carrying the skill version — their `<!-- oz-triage v:1 -->`. This gives the outer loop (a) a way to find its own past decisions, (b) attribution to the exact skill revision, (c) an anchor for reactions. Without it, no attribution is possible. Pairs with the MR metadata block from 1.6.

**4.9 Deterministic feedback collection.** The signals, all structural, no model:
- **Maintainer relabels are ground truth** — the strongest signal, and it's a bare fact.
- Emoji reactions on the agent's comment (moderate).
- Human replies after the agent comment (strong when corrective).
- Bucket drift: bot's chosen state vs. the issue's current labels.
- Silence = weak positive.

gonk should beat their crude substring classifier easily using GitLab structural signals: thread resolution state, whether a suggested patch was applied verbatim, label change authorship, MR approval vs. changes-requested.

**4.10 A bounded write surface.** This is the rule that keeps a self-improving loop safe:
- The improver writes **only** to a designated `## Learned guidelines` section, capped at **15 bullets**, with forced consolidation at the cap.
- An explicit never-touch list: the output schema, severity labels, safety rules, evidence rules, the coordinate contract, the marker convention. **The improver may never edit the deterministic contract — only soft guidance.**
- Repo-specific conventions go to a `*-local` companion skill; the portable skill stays stable.
- **"No change" is a first-class outcome.** *"An empty run is a valid outcome — report 'no changes warranted' and stop without opening a PR."*
- Changes land as an **MR a human merges**. Never a direct commit. *"The skill never self-applies changes to main."*

**4.11 Teach generalization explicitly.** The most-repeated warning in the corpus: the agent turns every correction into a narrow rule. Told a reply was too marketing-y it wrote *"Never mention pricing in the first sentence"* when the principle was *"if someone is venting, lead with empathy."* Countermeasures: a separate learning skill with a fixed procedure, a good/bad lesson example pair, and a **≥2-instance generalization threshold** before a lesson is durable. Their summary: *"rules overfit and principles transfer."*

**4.12 Evals — and close their gap.** Warp publishes **no accuracy metric and no validation** for any self-improvement loop; one post concedes it is *"susceptible to finding local maxima."* gonk should not ship the loop without a gate it must not regress. Their own better artifact is the eval format shipped alongside skills: `evals/evals.json` with `prompt`, `expected_output`, and `assertions` — **including negative cases** ("Selects no report"). gonk's `test/stubmodel` cassettes are the natural substrate: agent-behavior evals that run deterministically in CI without touching a real model.

Also worth copying from their optimizer: **keep-or-revert gates** (build must pass, no blocker regression, cost must not regress >15% without justified quality gain) and **named convergence criteria** (diminishing diffs, quality plateau, cost floor) — with a plateau reported as *optimal*, not as failure.

**Exit criterion:** correcting the agent once changes its behavior next time, via an MR you approved, with an eval proving the change didn't regress anything.

---

## 8. Phase 5 — Platform hardening

**5.1 Run state machine + error taxonomy.** Adopt an explicit closed set: `INPROGRESS`, `SUCCEEDED`, `FAILED`, `BLOCKED` (awaiting human), `ERROR` (startup/infra), `CANCELLED`. Plus their failure codes, which map cleanly onto k8s: `environment_setup_failed`, `agent_process_failed` (includes OOM/segfault, non-retryable), `infrastructure_timeout` (a **stale-task reaper**, which is their entire runaway-loop backstop), `budget_exceeded`. Two details worth stealing: **run-state transitions and messages share a global sequence number**, so a parent never sees `SUCCEEDED` before the message that produced it; and **parent success is not transitive** — a parent can finish while a child is still running or failed.

**5.2 Agent-run metrics.** Their worker exposes an OTel catalog gonk can copy near-verbatim: active/max-concurrent gauges, claimed/rejected (with `reason` label)/completed (with `result` label) counters, a duration histogram, and a connectivity gauge. Best practice worth adopting: **pre-seed all series at startup** so dashboards and alerts can reference them before the first run.

**5.3 Cost where the work is seen.** A per-run footer on the MR or comment: rung, model, tokens, USD (real vs synthetic, firewalled as the existing dashboards already do), and remaining project budget. Plus **budget approach-alerts** — a GitLab comment at ~80% of monthly budget, extending gonk-meter's quiet-hours/defer machinery. Note their metering combines inference **and compute** into one number; for gonk with local inference, GPU-seconds on bailey is the real axis and the synthetic USD price is the proxy.

**5.4 Model routing.** Their shipped design is a YAML router of exactly two kinds — `complexity` (easy/medium/hard buckets) and `prompt` (ordered description→model rules, first match wins) — each with a **required `default`**, resolving **per conversation, not per step**, because switching mid-conversation resets prompt caching. Routers cannot target other routers or Auto models (no cycles). Fallback: target unavailable → router default → built-in default, with the user told. gonk's rung ladder is a budget-driven cousin; adding a complexity dimension (small local model for search/classification, larger for implementation) is the natural extension and pairs with 4.1. Their observability schema is the part to copy exactly: **store requested policy and resolved model separately, per message, with cost.**

**5.5 Cross-model failover.** *"Retrying with the same model often produced repeat failures"* — the chain must cross models. For gonk that means a fallback across different local models behind LiteLLM. This is the correct fix for `gonk-ob5`'s failure mode: a **deliberate, budget-aware local fallback chain** instead of an accidental fail-open to a cloud provider.

**5.6 Multi-harness rung.** Their argument for the seam: *"agent performance is a function of the harness and the model together"* — multi-model isn't enough. Their concrete interop trick is worth copying: **adopt the incumbent skill format** (`.agents/skills/`, readable by Claude Code, Codex, and opencode alike) rather than defining a new harness protocol. The abstraction lesson from their computer-use work is the general form — **narrow the interface to irreducible primitives, put provider-specific decomposition above the executor**, which is exactly gonk's broker/effects boundary.

**5.7 Close the egress gap.** `gonk-7oz` plus the standing Cilium/Ollama bypass. Worth recording that Warp shipped cloud agents with **unrestricted egress**, publicly listed as future work, while their sandboxes hold live forge tokens — gonk's exposure is narrower, but the fix is still owed.

**5.8 Now widen intake.** With throughput proven, unblock `gonk-6po` (source beads), `gonk-icj` (park and re-drive webhooks), `gonk-8s0` (instance-wide discovery). Add **scheduled triggers** as a first-class entry point alongside webhooks — their factory treats cron as equal to events, and gonk has only `gonk-sweep`'s cooldown today.

---

## 9. Phase 6 — The agents that pay for the factory

Their highest-value automations are mostly *not* code generation. These are cheap, run on schedules, and buy back the attention of the only human here:

1. **Dependency and CVE agent** — find outdated deps, upgrade, open MRs. Human keeps prioritization.
2. **Docs/changelog sync** — triggered by merges; their docs fleet is the best-quantified case study in the corpus (285 runs, 55 MRs merged, 80% of a migration in a 3-hour session).
3. **Broken-link / dead-code / stale-config sweeps** — open an MR *before* you notice the problem.
4. **CI failure diagnosis** — port `diagnose-ci-failures`, whose key discipline is *"Always create a plan first"*: categorize into formatting / linting / compilation / test / platform-specific and hand off to a separate fix skill. Pairs with `gonk-kxg`.
5. **Monitor agent** — turns production signals into filed issues, closing the loop.
6. **MR walkthrough generation** — a static page per MR; their skill enforces a size-scaled node budget and, notably, requires the generated artifact to be **validated before being called ready** (*"report canvas rendering as unverified instead of ready"*).

Their generalizable finding on which of these succeed: *"The projects that moved fastest shared a pattern: the person who built it was also the person who'd use it every day."* With one operator, that's every project — which is an argument for building only what you'll actually use.

---

## 10. Deliberately not doing

| Not building | Why |
|---|---|
| **Computer-use / visual verification** | Needs a GUI stack (Xvfb in-container is the easy part) *and* a capable vision model. Local 14B vision is not there. Their own version is single-threaded, expensive, and uncapped. Revisit only if a local vision model becomes viable — and note their pattern degrades gracefully: headless browser assertions over network/console observations are the reachable 80% |
| **Semantic/embedding codebase index** | Their own top-5 SWE-bench agent uses none. Windowed grep tools first |
| **Live session steering / handoff UI** | Their three human-in-the-loop primitives (steering, handoff, notifications) assume a team and a GUI client. With one operator, notifications via GitLab comments cover the need. This is on their own "don't build" list |
| **Mobile access, web session viewer** | Same reasoning. Explicitly on their don't-build list |
| **Multi-tenant isolation, per-user credit caps, SSO/SCIM** | One operator, one team |
| **Deep multi-agent orchestration** | Their benchmark work *rejected* multi-agent (dedicated planner/tester/reasoner, best@k) in favour of a single agent with a context-dependent tool set. The only justified subagent is context isolation (4.1). Their own orchestration is capped at one level deep |
| **LLM-judged escalation** | Violates the core invariant. Their `verdict` enum is model-produced; gonk's classifier must stay pure |
| **A vendor-style control plane** | The MVP trap. Every item above attaches to existing gonk services rather than adding a new orchestration layer |

---

## 11. Metrics: instrument these from Phase 1

Warp proposes a metric set and then **publishes no value for their own headline number**. Don't repeat that. Two numbers, tracked from the first implementation MR:

1. **Fraction of closed beads that reached merge with zero human edits.** Their claimed comparable: 20–30% (careful figure), 60% (marketing figure, includes human-in-the-loop). Anything above zero beats today.
2. **Cost per merged change** — GPU-seconds on bailey plus operator minutes. Their formula: `shipped product / (inference cost + human time cost)`.

Secondary, all cheap once 4.8's markers exist: acceptance rate of agent-suggested diffs; **intervention rate** (how often a human takes over); review cycles per MR; how often the reviewer is corrected. Their deterministic acceptance schema is worth mirroring — suggested vs. accepted file-changes and lines, with a `was_edited_by_user` flag.

And the discipline that costs nothing: *"Every time we use an interactive agent to write code, we view it as a failure to learn from."* Every human intervention gets recorded so Phase 4 has a corpus to learn from. For a one-person shop that's the cheapest possible version of a self-improvement loop — a habit of writing down *why* you intervened.

---

## 11b. Revisions from the second-opinion review (2026-08-13)

An independent analysis (`2026-08-13-fable-gonk-vs-warp-analysis.md`) reviewed this roadmap and caught two ordering errors. Both are accepted; the phase table above is superseded where they conflict.

**Revision 1 — the runtime items in Phase 4 are preconditions for Phase 1, not follow-on work.** §0.1 of this document argues that context isolation is *"a precondition for gonk, not an optimization"* at 16K context, and then schedules the enabling work (4.1 search subagent, 4.3 hunk-only edit returns, 4.4 context-dependent tools and the pager fix, plus windowed search tools from 4.2) three phases later. That is an internal contradiction. **Move 4.1–4.4 into Phase 1**, ahead of the implementation agent: an implementation agent that echoes whole files or swallows a repo tree will not fit in the window at all, so building it first guarantees rework. Phase 4 keeps the *loop* (4.8–4.12) and the packaging items (4.5–4.7).

> **⚠️ Revision 1 is PARTLY WITHDRAWN — see §11c.** Its premise was the 16K figure, which was wrong. At 128K the "won't fit at all" argument fails. The pager fix (`gonk-sxg`) and the edit-tool investigation stay in Phase 1 because they are cheap and load-bearing for a different reason; the **search subagent moves back out to Phase 4/5** as the cost optimization it actually is.

**Revision 2 — verification should precede the spec agent.** Phase 2 currently bundles two things of very different value: the cheap routing inputs (`roadmap.md`/`vision.md`, the four-state rubric) and the expensive spec-authoring machinery. The routing half stays in Phase 2. The **spec agent moves after Phase 3**. The argument: Warp's spec gate exists to reduce review burden across *a team of engineers with varying prompt skill* — the variance it removes doesn't exist in a one-operator shop, where the operator already *is* the spec gate. What actually consumes the single operator's time is wrong diffs, and the thing that makes wrong diffs cheap to detect is verification, not a pre-written spec. Build the detector first; add spec authoring when verification proves the model produces work worth specifying in advance.

**Revision 3 — within Phase 0, `gonk-ob5` outranks `gonk-e9m`.** Fail-open to a cloud provider defeats *both* core invariants simultaneously (local-only inference and hard budget enforcement) and does so silently. The keystroke-injection bug is bounded by a sanitizer; the fail-open is not bounded by anything.

**Two items to STOP, also accepted:**

- **Cut the self-improvement loop (4.8–4.12) from the plan.** Warp, with a team producing hundreds of runs per week, never validated theirs — no accuracy metric, no eval, and a published concession that it finds local maxima. A single operator's correction corpus is statistically empty; a loop that learns from a handful of relabels will overfit faster than it improves. **Keep only 4.8 (the versioned marker)** — it is cheap, and it is the join key that makes a future loop possible without retrofitting. Revisit when there is a corpus worth mining.
- **Delete the formula / `[steps.check]` machinery.** Carrying two orchestration idioms (broker path and formula pour) is what produced the silently-dead mention trigger (`gonk-ecn`) and three zombie carry-forwards in PLAN.md. Port mention onto the broker and remove the formula path rather than maintaining both.

**Also added from the review, not previously filed:** `gonk-8g9` (credential Ollama — one auth proxy makes local budgets hard without waiting on the Cilium migration; the highest value-per-effort item not on any list), `gonk-ecn` (the dead mention trigger), `gonk-sxg` (no `PAGER` in the agent image — Warp's documented pager-hang will hit gonk as-is).

---

## 11c. Corrections of 2026-08-13 (model routing, and the test repo)

Two facts arrived after the first draft. Both change priorities.

### 11c.1 The context ceiling was an artifact of pointing at the wrong model

Established against the live cluster, not from documentation:

| Alias in LiteLLM | Backend | Context |
|---|---|---|
| `qwen3-14b` | Ollama `qwen3:14b` | **16,384** ← what gonk's rung catalog names |
| `qwen3-8b-bailey` | Ollama `qwen3:8b` | 40,960 |
| `qwen3.6:35b`, `qwen3.6:35b-nothink` | Ollama | 131,072 |
| `nemotron3:33b` | Ollama | 131,072 |
| **`qwen3.6-35b-vllm`, `qwen3.6-35b-vllm-nothink`** | **vLLM → NVFP4 35B MoE** | **131,072** |

`docs/environment.md` still claims the instance has two models (`gonk-4ck`), and documents no vLLM deployment at all. The routing fix is `gonk-gyj`.

**What this changes:**

- **Search subagent / context isolation: Phase 1 → Phase 4–5.** It is a cost optimization now. Warp's own measurement (−26% tokens) is a cost result, not a capability result, and it was measured against a tool-definition blob gonk does not yet have.
- **What moves *up* instead.** The binding Phase-1 risk is no longer "will it fit" but "will the model emit a valid, appliable edit." That promotes: the **annotated-diff coordinate scheme + validator** (hallucinated line numbers are a model behaviour, unaffected by window size), the **anti-lying validator**, and the **opencode edit-tool investigation** — what its pinned edit tool does on a fuzzy miss, and how much of a file it echoes back. That last one is the first real test of whether opencode stays the right harness for a code-writing rung.
- **Tool-call parsing is now a lever.** The vLLM deployment has `--enable-auto-tool-choice` with a `qwen3_xml` parser. Warp's finding that tool names and schemas are model-specific (renaming `grep`→`ripgrep` measurably improved selection; preamble prompts caused stalls) is now directly testable rather than theoretical.
- **Reviewer/implementer model diversity is free.** Several distinct 128K models are already served, so ADR-007 §4.3's separate-reviewer idea costs nothing to arrange.
- **Predictions re-baselined.** Any forecast keyed to "qwen3-14b at 16K" was scored against the wrong model. In particular, the predicted dominant failure mode — *context exhaustion on repos over ~30 files* — is now unlikely; expect malformed or non-appliable edits instead. The pessimistic merge-rate forecast should be re-run against `qwen3.6-35b-vllm` before it is treated as a baseline.
- **Open question, not resolved here:** deployed `--max-model-len` is 131072 while the stated design target was ~250k. Decide whether that is a deliberate step-down (KV-cache headroom at 0.35 utilization, alongside a resident Ollama on the same box) or drift.

### 11c.2 The first implementation target must be a real repo that is not gonk

Accepted, with an added constraint that sharpens it: **a toy repo produces toy issues.** The failure modes worth discovering — ambiguous requests, cross-file changes, stale assumptions, work that should have been an issue reply instead of a diff — only appear against genuine activity. A scratch repo would validate the plumbing and teach nothing about the product.

**Why not gonk itself:** an agent whose effect batches can reach the repository containing its own broker, gate, and `effect-shape.toml` is precisely the scenario `gonk-066` (workflow-scope gating) exists to prevent — and `gonk-066` is open. The 70-bead backlog makes gonk the tempting first target; that temptation is the argument for the rule, not against it.

**Selection criteria:**

1. **Genuine issue flow** — you file issues against it in the ordinary course of work, without manufacturing them.
2. **Deterministic validation** — a real build and test command that runs in a container, so Phase 1's `.gonk.yml` validation contract has something true to name.
3. **Bounded blast radius** — a merged bad MR is embarrassing, not operational. This rules out the gitops repo outright: an MR there deploys.
4. **Diagnosable size** — small enough that an early failure can be read in one sitting.
5. **Not gonk**, until `gonk-066` closes.

**Decided 2026-08-13 (owner): `agentic/nagus`, `agentic/rom`, `agentic/quark`.** Tracked as `gonk-4v8`.

| | nagus | rom | quark |
|---|---|---|---|
| Commits, last 90d | **73** | 3 | 3 |
| Size / tests | 134 files, 33 test files | 61 / 4 | 61 / 4 |
| Deployed | **yes** — gitops, own image + shared CNPG | no | no |
| `.agent/` present | yes, richest (`tasks`, `grafana`, `sops`, `system`) | yes | yes |
| Provenance | first-party service | `homelab/go-service-template` | same template |

Three observations that matter to the plan:

- **The orientation tier already exists.** All three carry an initialized `.agent/`. ADR-007 §4.2's "review against durable project documents" is buildable now, not after a Navigator build-out.
- **The criteria split cleanly, which is why three beats one.** nagus supplies the genuine inflow but is deployed *and* handles untrusted listing content, so it carries real blast radius. rom and quark are undeployed and cheap to get wrong, but at 3 commits a quarter they generate little on their own. Neither profile satisfies criteria 1 and 3 together; the set does.
- **rom and quark are near-identical siblings** from one template. That is a free A/B: run the same change against both, or hold one as a control while tuning prompts against the other.

**Order within the set:** prove the plumbing on rom or quark, hold the sibling as control, bring nagus in third once the loop is trusted.

**A hazard found while checking this, now `gonk-jn5`.** `pkg/glab/projects.go:19-25` enumerates watched projects via `GET /api/v4/projects?membership=true`, and GitLab's `membership=true` **inherits group membership**. All three targets live in `agentic/` — and so does `gonk-project`. Granting the bot membership at *group* level would therefore make gonk watch itself, handing a code-writing agent a path to its own broker and `effect-shape.toml`, as a side effect of onboarding rather than as a decision. Onboard **per-project only**, and consider a hard project-ID denylist in intake so self-watching fails closed regardless of how membership is later granted in GitLab's UI — which is outside gonk's control. `gonk-066` now blocks `gonk-4v8` for the same reason.

**Priority shifts that follow:**

- **`gonk-bgx` is promoted from bug to hard blocker.** "A newly-onboarded project can never reach triage: scaffold gates it and scaffold is on the broken formula path" is no longer one broken path among several — it is the thing standing between the plan and its own test bed. Onboarding a second project is now on the critical path. Same for `gonk-msz` (scaffold told the repo is checked out; nothing clones it).
- **`.gonk.yml` validation commands move earlier**, from Phase 1.4 into the onboarding of the test repo. The agent must not guess how to test a codebase it did not write.
- **`gonk-m6t` gains real value.** A second project makes per-project attribution and per-project ceilings observable rather than theoretical — two projects is where the budget story is actually exercised, and it is the differentiator Warp's analytics lacks.
- **`gonk-8s0` stays deferred.** One deliberate extra project needs no instance-wide discovery model.
- **The metrics get a denominator.** §11's two numbers only mean something against a repo with genuine inflow; measured against gonk's own backlog they would be self-referential.

### 11c.3 The serving layer is a *development-environment* variable — not a gonk feature

Correcting a framing error, and then correcting the correction.

The earlier draft treated "local models" as exogenous: a fixed capability the roadmap must work around. That was wrong. But the fix is **not** that gonk drives the serving layer — that would be a worse error, because it would encode a deployment reality into the product.

**The accurate statement:** *our development of gonk on orac* drives what bailey serves and how. Because gonk is the primary workload on this cluster, the operator can tune the model/server mix to suit it. That is a fact about **this deployment**, and it belongs to `docs/environment.md`, the gitops repo, and the operator — the same place the site-local model names already live.

**What gonk the software may assume: nothing beyond an OpenAI-compatible endpoint.** Spec §2 goal 8 requires the product repos carry no deployment-specific material, and `docs/environment.md` already states the rule for exactly this class of fact — site-local model names *"must never appear as defaults in gonk's product code or chart."* The same rule governs serving assumptions. gonk must run against whatever LiteLLM fronts, on any site, including one with a single modest model behind Ollama and no vLLM at all.

So the two columns must stay separate:

| Development-environment activity (orac, operator, `environment.md`) | Product requirement (gonk repos, chart, pack) |
|---|---|
| Which models run on bailey, on which server, with which flags | A rung catalog that is **operator-supplied and site-local**, with the chart failing to render without one |
| Consolidating agent load onto vLLM; raising `--max-model-len` toward the ~250k target; resolving the vLLM/Ollama resident-memory contention | No assumption about batching, prefix caching, or resident-model behaviour |
| Evaluating SGLang or TensorRT-LLM as a better fit for sustained agent load than Ollama's low-commitment load/unload model | No assumption that any specific server is present |
| Enabling guided decoding on the backend | **Optional** use of it where advertised; never a load-bearing guarantee (see below) |

**Why this matters beyond tidiness.** Stage workloads genuinely differ — implementation resends the same repository prefix every turn and benefits enormously from prefix caching, while triage is short and one-shot — and it is right for *this deployment's operator* to act on that. It would be wrong for gonk to encode it, because a site with different hardware will make different choices and gonk must still work there.

**Grammar-constrained decoding — an optional optimization, never a guarantee.**

gonk's architecture depends on the agent emitting a well-formed effects batch, and Phase 3 adds a second schema-shaped artifact for review findings. Warp defends this with a *tolerant parser* — scanning for the last JSON object carrying the required keys, because models wrap output in prose despite instructions, a defence they implement in three separate places.

Where a site's backend supports guided decoding (vLLM's xgrammar/outlines, SGLang's structured output), constraining the sampler to the effect-batch schema removes an entire failure class at the most deterministic point available. That is worth taking **when it is offered**.

But it must be plumbed as an **optimization that reduces retries, never as the thing that makes output trustworthy.** The reasons are the same two that govern everything else here:

- **Portability.** A site with a backend that can't do it must still work. If the guarantee lived in the sampler, gonk would silently become vLLM-only.
- **Fail-closed.** The **shape gate remains the sole authority**. The broker validates the batch regardless of how it was produced — that is already the design, and it is what makes the agent's output untrusted-by-construction. Constrained decoding narrows what the model tends to emit; only the gate decides what is allowed to happen.

Practically: advertise it per-rung in the operator's catalog, use it where present, and keep the parser and gate unchanged either way. The main unknown is whether LiteLLM forwards guided-decoding parameters to the backend, or whether that capability is only reachable by addressing vLLM directly — which would itself be a portability cost worth weighing.

Worth noting as context rather than as a gonk claim: a cloud-proxied product like Warp cannot do this at all, since it sits in front of vendor APIs and can only constrain by prompting and retrying. Owning the inference layer is what makes the option *available* to this deployment — it does not make it part of the product.

**Consequence for the plan, split by column:**

*Development-environment work (not gonk product work):* `gonk-tqf` — pick the model/server mix that suits how we develop gonk here, resolve the vLLM/Ollama resident contention, and decide the `--max-model-len` target. Record the outcome in `environment.md` (`gonk-4ck`), not in the chart or the pack.

*Product work:* `gonk-gyj` — point the site-local rung catalog at a model that fits the workload, which is a config change in *this* deployment's operator values, not a default. `gonk-vpm` — support guided decoding as an advertised, optional per-rung capability, with the shape gate unchanged as the authority. And instrument requested-policy vs resolved-model vs cost per call (Warp's routing observability schema), which *is* product work and is what lets any operator steer their own catalog by measured demand rather than guesswork.

---

## 12. Sequencing summary

Revised per §11b **and §11c**:

```
Phase 0  Unblock          fail-open (ob5) FIRST, then registry, prompt-by-reference,
   │                      rig, egress; + credential Ollama (8g9), PAGER (sxg),
   │                      route rungs to the vLLM 35B (gyj);
   │                      SCAFFOLD/ONBOARDING (bgx, msz) is now a hard blocker —
   │                      it gates the test repo, which gates everything
   │                      + pick and onboard the real non-gonk test repo (§11c.2)
Phase 1  Throughput       edit-tool investigation + hunk-only returns, PAGER, tool-name
   │                      tuning for qwen3_xml; THEN implementation agent, code effects,
   │                      anti-lying validator, annotated-diff coordinates + validator,
   │                      MR↔bead marker, .gonk.yml validation contract, fix attribution
   │                      → THE gap; Warp's own factory stalled exactly here.
   │                        Search subagent is NOT here — 128K makes it an optimization
Phase 2  Route            roadmap.md/vision.md, 4-state rubric, skills-as-files
   │                      → cheap, high-leverage; the spec agent is NOT here any more
Phase 3  Review + verify  annotated-diff coordinates, validator, read-only reviewer,
   │                      reproduce/verify modes, artifact-backed status, attempt cap
   │                      → the detector for wrong diffs, which is what costs the operator
Phase 3b Spec agent       PRODUCT/TECH specs + human spec gate
   │                      → only once verification proves the work is worth pre-specifying
Phase 4  Package          environments as one object, sidecar-merged harness, warm cache
   │                      → self-improvement loop CUT; only the versioned marker survives
Phase 5  Harden           state machine + fault-split errors, metrics, cost footer +
   │                      alerts, complexity routing, local failover, multi-harness,
   │                      egress; delete the formula path; THEN widen intake
Phase 6  Compound         dependency/CVE, docs sync, sweeps, CI diagnosis, monitor
```

**The one-line version:** gonk already has the parts Warp charges for — hard per-project budgets, local inference, zero-credential agents, deterministic classification. What it lacks is the middle of the loop. Build implement → review → verify before anything else, then make it learn.

---

## Appendix: reusable source

Both MIT, so code can be lifted directly rather than reimplemented from prose:

- `github.com/warpdotdev-demos/cloud-factory-demo` — triage/spec/implementation/review skills, `annotate_diff.py`, `validate_review_json.py`, `collect_review_feedback.py`, workflows, `roadmap.md`/`vision.md` samples
- `github.com/warpdotdev/common-skills` — `write-product-spec`, `write-tech-spec`, `validate-changes-match-specs`, `check-impl-against-spec`, `diagnose-ci-failures`, `fix-errors`, `research`, `update-skill`, `pr-walkthrough`, plus `evals/evals.json` examples
- `github.com/warpdotdev-demos/issue-triage-loop` — the complete five-file self-improvement loop (versioned marker, bounded guidelines section, MR-only updates)
- `github.com/warpdotdev-demos/replatformer` — the optimizer with keep-or-revert gates, failure taxonomy, convergence criteria
- `github.com/warpdotdev-demos/factory-agents-template` — foreman + four step agents, **mock tracker/forge providers** so the full pipeline can be evaluated without touching real state (directly applicable to gonk's L1 harness)
- `github.com/warpdotdev/oz-for-oss` — webhook control plane: idempotency, cron drain loop, expiry semantics (360 attempts / 7 days), per-run error absorption
- `github.com/warpdotdev/oz-agent-worker` — Kubernetes backend, Helm chart, RBAC set, OTel metric catalog, preflight-job pattern

Licensing note: unlike `gascity-packs` (no LICENSE, still an open question for gonk), these are unambiguously MIT.
