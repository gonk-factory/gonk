# gonk vs. Warp: an independent second opinion

**Date:** 2026-08-13
**Author:** Fable (independent second-opinion analysis; not a summary of the prior research)
**Reads:** the four 2026-08-13 Warp research docs, the approved spec (`docs/superpowers/specs/2026-07-12-gonk-stack-design.md`), `PLAN.md`, `docs/HANDOFF-next-session.md`, `docs/environment.md`, ADRs 001-006, `git log` (298 commits, 2026-07-12 → 2026-08-13), and the live bead tracker (126 issues: 70 open, 4 in progress, 52 closed).
**Stance:** opinionated. Where I disagree with the existing research docs or the roadmap, I say so and why. The invariants (hard budgets at the LiteLLM door, no LLM judges, zero forge creds in pods, fail-closed config, no auto-merge) are treated as fixed; I argue for changing none of them.

---

## 1. Original plan vs. reality

### 1.1 The philosophy drifted, and nobody has written that down

Spec §1: *"gonk is a Gas City pack plus glue, not a new orchestrator."* Spec §4.3 step 5: the agent session *"posts labels/comments via the GitLab API as the bot user."* Spec §9: agent pods get egress to GitLab.

Reality: none of those three sentences describes the running system.

- The **triage broker** (2026-07-27 spec, sessions 2-6) inverted §4.3: agents are pure producers of proposed-effects batches; a deterministic controller-side broker applies them under the bot PAT; agent pods hold zero forge credentials (`images/agent/entrypoint.sh` stripped, `Dockerfile.agent` CA removed, session 2).
- The **formula/order machinery** the pack was built around (Plan 04, Tasks 3-4: five orders, three formulas, `[steps.check]`) is mostly dead in the hot path. `cmd/gonk-gate/broker_inject.go` routes `issue-triage` and `scaffold` through `runBrokerDispatch`; only **mention** still falls through to the formula pour — a path that upstream #4891 proves *cannot deliver a prompt* on the k8s provider. The conversational surface (spec §5.5, a stated v1 capability) is therefore silently dead code: it will dispatch, spawn, and idle forever.
- gonk now owns dispatch, prompt delivery, session correlation, outcome sweeping, and effect application. That is an orchestrator. Gas City's remaining contribution is pod lifecycle and the Dolt beads store — and both have been the source of the worst bugs (keystroke delivery `gonk-e9m`, silent `Nudge` error-swallowing, async-202-is-not-a-receipt, Dolt failing the reservation-isolation spike).

**Verdict on the broker drift: correction, and the single best decision in the project.** Warp shipped the naive version (agent holds `issues: write`) in June 2026 and then rebuilt it into exactly gonk's shape — read-only agent, structured JSON, deterministic apply job — citing prompt injection as the reason. gonk got there by construction, before being burned. The blog-lessons doc says this; I'll add the sharper point: **the broker is now gonk's identity, and the spec should be amended to say so.** "Pack plus glue" is false advertising to your future self. The next person (you, in three months) reading the spec will design against the wrong architecture. Write ADR-007: "gonk is a deterministic effects broker; Gas City is a session substrate," and mark spec §4.3 superseded.

**Verdict on the formula-path residue: mistake, still open.** Two orchestration idioms live in the codebase simultaneously. The formula path carries three unresolved carry-forwards that have haunted every plan since 04 (`[steps.check]` env contract, `discussion_id` plumbing, `gc lint` not validating order semantics). Every one of those is maintenance on a corpse. See recommendation R4.

### 1.2 Other drifts, judged

| Drift | Spec section | Correction or mistake? |
|---|---|---|
| vLLM → Ollama on bailey | §3 | Incidental — but it created the credential-less-Ollama budget bypass that makes local budgets advisory. The spec's §9 "budgets cannot be bypassed" was falsified by this swap plus Flannel. Owner-documented honestly (`docs/environment.md`), which is the right handling. |
| Dolt → Postgres for the meter ledger | §4.1 | Correction, exemplary: a spike (`docs/spikes/dolt-reservation-isolation.md`) falsified Dolt's isolation decisively (32 writers past a ceiling of 2, 90/90 iterations), decision recorded in ADR-004. This is how drift should be done. |
| kind e2e → throwaway ns on the real cluster | §10.4, §11.1 | Reasonable substitution (kind not installed, WSL CNI broken) — but it became cover for not building the deterministic harness at all. See §1.3. |
| Session resume (§4.2 continuity, §11.4) | Never validated | Moot-by-architecture: broker sessions are one-shot attempts; correctness reconstructs from beads + GitLab, exactly the spec's layer-3 fallback. This is fine — but retire the criterion formally instead of leaving it as an unmet named risk. |
| Per-comment attribution regressed | Goal 4 | Mistake, known (`gonk-m6t`): attribution metadata no longer reaches the pod on the submit path. This was a *met* exit criterion that regressed. It is also gonk's genuine differentiator over Warp (their #12075 is open). Prompt-by-reference T1-T6 closes it and is unstarted since 2026-08-01. |

### 1.3 v1 exit criteria (§11): the honest scorecard

1. **Helm install; idle = zero agent pods** — half met. Real-cluster deploy works (Flux, HelmRelease Ready). But idle-scale-to-zero is violated in practice: session 6 observed `poolDesired: scaffold = 2` despite `min_active_sessions = 0` (`gonk-4xr`), plus eight wedged pods parked in the namespace. Still matters — a factory that idles hot on a 128GB shared GPU box (bailey is already memory-saturated, `gonk-38o`) is spending its scarcest resource on nothing.
2. **Onboarding MR e2e** — built (Plan 02), then broken: `gonk-bgx`/`gonk-msz` mean a newly-onboarded project can never reach triage today. Scaffold's broker port landed but delivery stalls (`gonk-4xr`, one retry left before re-poisoning). **Unmet, and it matters more than anything else in §11** — onboarding is the product's front door.
3. **Triage e2e: stub model, then bailey** — met for bailey (issue 21, session 5 — real and verified). The **stub-model deterministic path was never built** (Plan 06: "not started"). This is the largest standing violation of the spec, and of spec goal 7 ("trustworthy by construction: deterministic e2e"). It still matters — see §4 and R2; I'd argue it matters *more* now than when the spec was written, because the past three sessions were consumed by exactly the silent async failures a deterministic harness catches in CI.
4. **Session resume** — unmet, and no longer matters as framed. Retire via ADR (see §1.2).
5. **Spend attribution + budget-exhaustion defer** — attribution met then regressed (`gonk-m6t`); the defer path is unit-tested but has never been demonstrated live against an exhausted budget. Half met. Still matters: it is the money invariant.
6. **Kill tests** — never run. Still matter, but narrow them: the three that pay are (a) intake restart mid-webhook (already known lossy — `gonk-fan` bit four times in one session; `gonk-icj` files the fix), (b) dolt restart mid-sweep, (c) LiteLLM outage mid-session (must classify infra, never escalate — the classifier's known false positive from ADR-004/005 has never been measured, a named Plan 06 TODO).

**Score: ~1.5 of 6.** The project is deployed and has done one real end-to-end triage — a real milestone the scorecard undersells — but by the spec's own definition of done, v1 is not done, and the tracker's center of gravity (source beads, local-parity epic, verifier steps) has already moved on to v2 work. That is the same pattern as Warp's factory: the front of the loop outruns the exit criteria of the previous stage. Warp's cost for it was 1,300 stuck issues; gonk's version is a broken front door (`gonk-bgx`) sitting behind three research epics.

---

## 2. Architecture: the consequential shapes

Not a feature table. Five load-bearing differences.

### 2.1 Unit of work: bead vs. run — gonk is genuinely better

Warp's unit is the **run**: an ephemeral conversation, container destroyed after, durable state pushed out to forge labels and PR bodies. Their own operational notes document the cost: label-as-event-bus doesn't chain under the default token; the cloud action returns on *spawn*, not completion, forcing a hand-rolled 250-line poller; idempotency is "pushed to the app layer" (i.e., absent).

gonk's unit is the **bead**: durable, anchored to a forge object, carrying ladder state (`rung`, `attempt`, `ReservationID`, `SessionEndedAt`), with `BeadAnchor` as an idempotency key enforced at both gates and in meter's partial unique index. Sessions are *attempts against* a bead, not the work itself. This is why gonk can survive restarts, retry without double-spending, and escalate deterministically — capabilities Warp's shape cannot express without their server's proprietary state.

**Consequence to protect:** keep the state machine in beads and treat forge labels as a *projection*, never a bus. The 4-state triage rubric (roadmap 2.2) and the source-beads epic both flirt with label-driven chaining; Warp's `GITHUB_TOKEN`-doesn't-retrigger scar is the general warning about building state machines out of forge-side events under a bot identity. gonk's suppression of bot-authored webhook events (`pkg/ghook`) means gonk has the same non-chaining property *by design* — good — so any stage-to-stage handoff must go bead → dispatch, never label → webhook → dispatch.

### 2.2 The trust boundary: gonk is stronger on writes, weaker on the prompt channel

Both systems now treat the *model* as untrusted on the write side. Warp's apply job validates a JSON contract; gonk's broker validates `effect-shape.toml` + `ValidatePaths` (mutation-tested) + a protected-path denylist. Roughly equal, gonk slightly stronger (the classifier itself is model-free, theirs is a model-emitted `verdict` enum).

On the *input* side the systems diverge and gonk is currently worse than anything Warp ever shipped:

- **Warp never had a keystroke channel.** Prompts are API messages. gonk delivers attacker-influenced issue text as tmux keystrokes into a TUI whose composer treats `!` as shell-mode (`gonk-e9m`). A bang in an issue body executed the prompt as `/bin/sh` in the agent pod — that happened, live, session 5. Bang-stripping is the current mitigation. The prompt-by-reference plan (T0 passed, T1-T6 unstarted since 2026-08-01) is the fix and it is twelve days stale.
- **Warp's interpolation rule** — only forge-generated non-freeform values in the prompt; the agent *fetches* untrusted text via a tool — is structurally unavailable to gonk, because the zero-creds pod cannot fetch anything. gonk instead splices controller-fetched issue text (8 KiB-capped) into the prompt string. With `qwen3-14b`, assume injection *will* succeed at steering the model. The honest security claim is therefore not "the prompt is clean" but "**the blast radius is the shape gate**": whatever the model is tricked into wanting, it can only emit a comment and labels on the anchored issue. That claim is true today and is the right one to defend — which raises the stakes on `gonk-066` (the shape gate is syntactic and cannot express workflow scope) before any `mr`/`commit` effect kind exists. An injected implementation agent that can emit file effects is a different animal from one that can emit a comment.

### 2.3 Failure shape: gonk fails silent-and-stuck; Warp fails loud-and-unbounded

Warp's published failure modes are runaway shapes: unbounded verification loops (no attempt cap, only a cost warning), agents claiming uploads succeeded, redundant reviews on rapid pushes. Their backstop is a stale-task reaper (`infrastructure_timeout`), which is their *entire* runaway defense.

gonk's failure modes, from three sessions of live archaeology, are the mirror image: silence and permanence. The async 202 that isn't a delivery receipt; `Nudge` returning nil unconditionally; sessions idling forever at the splash screen; intake accepting webhooks with a 200 and discarding them for 10 minutes after every restart (`gonk-fan`); a bead poisoned under a since-fixed bug staying denied forever (`gonk-csz`); a dispatch that never creates a session still burning a ladder attempt; the mention trigger dispatching down a path that cannot deliver. gonk's ladder makes unbounded loops nearly impossible by construction (reservation TTL, `max_infra_retries`, escalation only bought by a completed-but-failed gate) — genuinely better than Warp — but the same rigor makes *termination too permanent*. There is no un-poison path, no dead-letter review, no "this bead has been in `waiting` for N days" alert.

**The team has half-noticed this** (individual beads exist) but not named the pattern: **every async boundary in gonk needs a positive completion signal and a staleness alarm, as policy.** Warp's `unschedulable_timeout: 30s` (fail a task early if the pod can't schedule) is a direct, cheap answer to `gonk-4xr`'s exact symptom — a broker session burning its ~90s delivery budget against a pod that was never going to schedule. Their FAILED-vs-ERROR fault split (caller's fault vs. platform's fault, `retryable` on the wire) is gonk's infra-vs-gate split with better naming and 18 concrete codes; adopt the taxonomy when building roadmap 5.1.

### 2.4 State topology: three databases is a lot of factory for one triage comment

gonk runs Dolt (beads), CNPG Postgres (ledger), and GitLab (truth), plus LiteLLM's own Postgres. Each is justified in isolation (the spike genuinely killed Dolt-as-ledger), but the aggregate is a heavy stateful backbone for a system whose delivered capability is triage comments. Warp's equivalent state for the *factory loop* is markdown in git plus forge labels — their self-improvement loop deliberately uses "no memory database, no vector store." The spec's own instinct (goal 6, Renovate model: project state lives in the project) points the same way.

I am *not* recommending ripping anything out. I am recommending refusing to add a fourth: several roadmap items (run records, agent metrics store, eval corpora, feedback corpus) will each tempt a new table or store. The default answer should be "a bead, a git file, or a Prometheus series," in that order. Dolt has already bitten twice (`gonk-kx3` silent 50-row truncation, the isolation spike) and the beads store is the one component with no PDB, no backup schedule, and a `root`/no-password/no-TLS auth posture (env doc) — it is simultaneously the most load-bearing and least hardened piece of the stack.

### 2.5 The dependency gonk claims not to own

The "not a new orchestrator" premise means the interface gonk depends on most — Gas City's session lifecycle — is the one it controls least. Four of the project's worst weeks trace to that interface: #4668 (formula vars dropped), #4891 (initial_message never delivered on k8s), the silent Nudge, keystroke delivery. The pragmatic responses (broker-direct session create, out-of-band prompt-by-reference through gonk-meter, correlating on the city event log) all share a shape: **route around Gas City's session semantics using components gonk owns.** Prompt-by-reference completes this: after T6, Gas City's role collapses to "create pod, report events." That is a *good* end state — but recognize it explicitly, because it changes the upstream calculus: PRs to fix #4891 stop being on the critical path, and a future Gas City upgrade is a risk to the workarounds rather than a source of fixes. Pin harder, upgrade rarely, and stop waiting for upstream.

---

## 3. Velocity: the honest comparison

**Warp:** ~102 people, ~3 years on the agent platform, ~$73M raised, frontier models, weekly-to-daily releases — and their own CEO calls the factory "half-working," with 1,300 issues stuck at `ready-to-implement` and 20-30% of PRs fully automated as the *success* number.

**gonk:** one engineer, 33 calendar days of real building (298 commits, ~9/day median, zero-commit days rare), ~50,500 lines of non-vendored Go plus a chart, a pack, and four images; deployed via Flux; one real model-driven triage completed end-to-end; 126 beads filed, 52 closed, 70 open and growing.

Three things that ratio actually implies:

**3.1 The per-capability cost is not the lesson; the plateau is.** gonk's month bought roughly what Warp's triage skill does, at 1/1000th the person-hours — because the triage stage is the cheap stage. Warp's numbers are the empirical curve for what comes next: a funded team with frontier models plateaued at 20-30% automation *at the implementation stage*. gonk will attempt that stage with a 14B model in a 16K window. The prior expectation for merge-without-human-edit on the first month of implementation MRs should be **single digits**, and the plan should be built to be worthwhile at that rate (it can be: an MR that is 70% right and honestly labeled still saves time — *if* verification is deterministic enough that you can trust the label).

**3.2 The ops tax is fixed and Warp doesn't pay it.** A visible fraction of the last month — I'd estimate 25-35% of commits and the majority of two sessions — went to operating the substrate, not building the product: registry volume 100% full (P0 `gonk-mzm`, 921 tags pruned by hand-written `hack/registry_prune.py`), bailey memory saturation (`gonk-38o`), CI egress outages, multi-arch rebuilds, WSL/podman quirks, webhook-drop archaeology. Warp bought Namespace precisely to not do this. gonk cannot buy its way out (the whole point is owning the substrate), so the only lever is **refusing to add standing infrastructure** and automating the janitorial (the registry-bound bead `gonk-07f` is the right shape: policy + alert, not another manual prune).

**3.3 Backlog math.** 70 open beads, one person, and the tracker's own discovery rate exceeded its closure rate every week since mid-July. That is Warp's 1,300-stuck curve at gonk's scale — and the deepest irony is that gonk's own tracker is the first queue that needs the implementation agent. Resist that irony (see R8): the first repo a code-writing agent touches should not be the one containing its own broker.

**Reachable vs. fantasy**, at observed pace (rough calendar estimates, one person, including the ops tax):

| Roadmap item | Estimate | Verdict |
|---|---|---|
| Phase 0 (registry✓-ish, `e9m` T1-T6, `ob5`, scaffold, `j9z`) | 3-5 weeks | Reachable; mandatory; mostly diagnosis-bound not typing-bound |
| Phase 1 minimal (implementation agent, code effects, anti-lying validator, `m6t`, MR marker) | 5-8 weeks | Reachable — *if* the Phase-4 runtime minimums come with it (see R3) |
| Phase 2 cheap half (roadmap.md/vision.md, 4-state rubric) | 1 week | Reachable; do it inside Phase 1 |
| Phase 2 expensive half (spec agents, PRODUCT/TECH) | 4-6 weeks | Deferrable — you are the spec gate at this issue volume (see R5) |
| Phase 3 (annotated diff + validator lifted from MIT source, reproduce/verify, attempt cap) | 4-6 weeks | Reachable; the highest-leverage lift-don't-write opportunity |
| Phase 4b self-improvement loop + evals | 8-12 weeks | **Fantasy for one operator.** Warp, with a team, never validated theirs; your correction corpus will be a handful of events per week — below any generalization threshold. Cut (R6). |
| Phase 5 multi-harness, MCP, complexity routing, run-state platform | months, open-ended | Fantasy as a set. Failover chain and the error taxonomy are the two worthwhile slivers. |
| Phase 6 janitor agents (deps, docs sync, link sweeps) | 1-2 weeks *each* once Phase 1 works | Reachable and probably the highest real ROI in the whole roadmap — these are the agents that pay a solo operator back. |

The one-line budget: **everything through Phase 3 plus two or three Phase-6 janitors is a realistic six-month program. Everything else on the parity table is where a one-person project goes to die.** The parity *table* itself is a hazard: it frames Warp's feature surface as the denominator. gonk's denominator should be "what buys back Steve's hours," and by that measure half the table is noise.

---

## 4. Warp's operational scars, mapped onto gonk

Which of their published failure modes gonk will hit, has hit, or has made impossible:

| Warp scar | gonk status | Action |
|---|---|---|
| **Pager/REPL hang** (agent runs `git log`, deadlocks in pager, times out silently) | **Will hit.** Verified today: no `PAGER`/`GIT_PAGER`/`--no-pager` anywhere in `images/agent/entrypoint.sh` or `Dockerfile.agent`, and git ships in the agent image. With keystroke-era debugging history, a silent pager wedge will be indistinguishable from every other silent stall. | File a bead; two lines in `entrypoint.sh` (`PAGER=cat`, `GIT_PAGER=cat`). Cheapest fix in this entire document. |
| **Edits are the most-failed tool call** | **Will hit at Phase 1, and gonk can't easily patch it** — the edit ladder lives inside opencode 1.18.3, not gonk. Warp's fix (exact → indentation-agnostic → Jaro-Winkler, failure reasons echoed back, hunk-only output) is harness-internal. | Before building the implementation agent, verify what opencode's pinned edit tool actually does on fuzzy misses and how much of a file it echoes. If it echoes whole files, that alone can eat the 16K window. This is the first real test of whether opencode remains the right harness for the code-writing rung — the rung-interface seam (spec §6.3) was designed for exactly this question. |
| **Model-specific tool naming/prompting** | Will hit on the first rung change (qwen → anything). | Keep prompts per-rung in the pack, never shared; already mostly true. |
| **Unbounded verification loops** | **Structurally impossible in gonk** — reservation TTL + budget ceiling + `max_infra_retries` + escalation-only-on-completed-gate. Genuinely better than Warp. | But gonk's inverse bug is real: attempts burned by infra before any model ran, and permanent poisoning (`gonk-csz`, `gonk-p2e`). Build the un-poison path and a staleness alarm. |
| **Label-as-event-bus doesn't chain** | Made impossible by design (bot-event suppression + bead-driven dispatch) — *as long as* no future stage chains via labels. | Write it down as a rule in the source-beads design review (`gonk-84m`): labels are projections, beads are the bus. |
| **Cloud action returns on spawn, not completion** | **Already hit and already paid for** — the 202-is-not-a-receipt bug cost session 4's wrong diagnosis and session 5's re-diagnosis. gonk's fix (correlate the terminal event on the city event log) is sound. | Treat `GET /v0/city/{city}/events` as a pinned contract — it is now the only truth for every async outcome, and it is unsigned and undocumented upstream. A regression test against the pinned Gas City SHA is warranted. |
| **~1MB job output caps → pass artifacts, not logs** | **Will hit at Phase 1, predictably.** Today's return channel is the session transcript read via peek (`GetSessionOutput`), with a 64 KiB cap in `pkg/gcapi` and a prior bug where the batch was read from a 400-line peek preview. A comment-sized effects batch fits; a multi-file diff will not. | Decide *before* building code effects: repository content returns via the rig (agent commits to its granted checkout; broker validates and pushes), never through session-output text. The `gonk-3so` design should state this on day one. |
| **Prompt injection via PR/issue content** | Half-hit already (the bang incident). Post-`e9m`, injection persists via interpolated issue text (see §2.2); the real defense is blast-radius (shape gate + anchored targets), same as Warp's. | Close `gonk-066` before any file-effect kind ships. Non-negotiable ordering. |
| **Sandboxes shipped with open egress** | **Currently true of gonk too** (Flannel, Cilium suspended, `gonk-7oz`) — same gap, smaller blast radius (no forge creds to exfiltrate; the exposure is unmetered Ollama burn + LAN reachability). | The cheapest hard fix is not Cilium: **put a credential in front of Ollama** (auth proxy on bailey), making the LiteLLM door the only holder. That converts local budgets from advisory to hard with one small deployment, no CNI migration. `docs/environment.md` names this option; nobody has filed it as a bead. File it. |

**What Warp does operationally that gonk hasn't thought about at all:**

1. **Deploy-time preflight.** `oz-agent-worker` runs a preflight Job verifying RBAC, admission, and image pullability *before accepting any task*. gonk's guards are all render-time (`_guards.tpl` G1-G20); nothing verifies the deployed reality (can the SA create pods? does the registry pull? is the meter reachable?) until the first real dispatch fails. A `gonk-gate preflight` subcommand run as a Helm hook would have caught several past deploy-state surprises.
2. **Cleanup asymmetry.** Successful task pods deleted immediately; failed ones kept for post-mortem with a TTL. gonk's current behavior is the worst of both: wedged pods accumulate (eight parked in session 6) with no policy at all.
3. **Debounce.** Rapid successive events on one object → one run (their `sleep 60`; their thread-scoped "new mention during active run becomes a follow-up instruction, not a second agent"). gonk dedupes deliveries but does not coalesce logical work; a user editing an issue three times in two minutes is three dispatches and three reservations.
4. **Secret redaction in transcripts.** Sweep reads full session output; the keystroke era proves shell output ends up in it. Nothing scrubs transcripts before they land in beads/logs. Warp's regex-at-render redaction is weak, but gonk's is absent.
5. **Pre-seeded metric series** so alerts can reference them before the first run. Trivial, worth copying into roadmap 5.2.

---

## 5. Recommendations, ranked

**R1. Finish Phase 0, but reorder it: `ob5` first, then `e9m`.** `gonk-ob5` (opencode fails open to a built-in *cloud* provider when its config is unreachable) is the single worst bug in the stack: it silently defeats both the local-only property and the budget door in one failure mode, and it can fire during any config hiccup. It is smaller than prompt-by-reference and should land first. Counterargument: `e9m` is the active injection hole. Response: the injection hole's blast radius is capped by the shape gate; `ob5`'s is capped by nothing but an unenforced NetworkPolicy.

**R2. Build the stub-model deterministic e2e before Phase 1 — the spec was right and the last month proved it.** Plan 06 was skipped in favor of live iteration, and the measured cost is now in the record: session 4's wrong diagnosis, session 5's re-diagnosis, `gonk-fan` biting four times in one day, every deploy recreating pods and dropping webhooks for 10 minutes. Every phase after this multiplies the async surface (implementation, review, verify are each new async loops). Scope it down from the spec's version: no kind, no real GitLab container — the throwaway-namespace harness plus the stub model server plus `glabtest` covers 80%. Warp's `factory-agents-template` ships mock tracker/forge providers for exactly this reason; the roadmap's own appendix names it and then schedules evals in Phase 4. That is too late. Counterargument: harness work ships no user-visible capability and a solo dev's time is the budget. Response: at the observed silent-failure rate, the harness *is* velocity; three sessions of archaeology would have paid for it.

**R3. Pull four Phase-4 runtime items into Phase 1; they are preconditions, not polish.** The roadmap's own §0.1 states that Warp's context-isolation patterns are "preconditions for gonk, not optimizations" at 16K — and then schedules them in Phase 4, after the implementation agent that needs them. At minimum, Phase 1 must include: (a) `PAGER=cat`/`GIT_PAGER=cat` in the pod; (b) capped, windowed read/search behavior verified in opencode (or imposed via prompt + tool config); (c) hunk-only edit echo verified (see the edit-scar row above); (d) the repository-content return path via rig, not transcript. Without these, the implementation agent will thrash its window on the first non-trivial repo and the failure will be misread as "the model is too small."

**R4. STOP maintaining the formula path. Delete it.** Triage and scaffold are broker-shaped; mention dispatches down a channel that provably cannot deliver a prompt (#4891) — it is not a degraded feature, it is a lie in the routing table. Either port mention to the broker (small: `agentForTrigger["mention"]` + a reply effect kind + the `discussion_id` plumbing that has been a carry-forward since Plan 04) or remove the trigger and the spec §5.5 claim until it's real. Then delete `pack/formulas/`, the formula orders, the `[steps.check]` scripts, and the three zombie carry-forwards they anchor. Counterargument: formulas are Gas City's native idiom and would come back if upstream fixes #4668/#4891. Response: gonk already routed its two real triggers around them, upstream has shown no movement, and every plan since 04 has paid a documentation-and-confusion tax on the corpse. Sunk cost.

**R5. Reorder Phases 2/3: verification before spec agents.** The roadmap puts the spec gate (Phase 2) before review/verify (Phase 3), on Warp's "highest-leverage checkpoint" argument. That argument assumes a team generating enough issue volume that spec-writing is a bottleneck worth automating. gonk has one operator and single-digit external issues per week: **you are the spec gate, and writing PRODUCT.md by hand is cheap.** The thing that actually consumes the solo operator is reviewing wrong diffs — which is what deterministic verification (annotated-diff coordinates, artifact-backed status, reproduce/verify, attempt cap) reduces. Better ordering: Phase 0 → Phase 1 (+ the cheap half of Phase 2: `roadmap.md`/`vision.md`, 4-state rubric — one week, do it inline) → Phase 3 → the expensive half of Phase 2 (spec *agents*) only when issue volume justifies it. This also front-loads the best lift-don't-write opportunity: `annotate_diff.py` and `validate_review_json.py` are MIT and directly portable.

**R6. STOP: do not build the self-improvement loop (Phase 4b) this year.** Warp — with a team, frontier models, and a real correction corpus — published no accuracy metric, no eval, and conceded local-maxima risk. gonk's corpus will be a few corrections a week from one human; the roadmap's own ≥2-instance generalization threshold will essentially never fire. Build the zero-cost substrate instead: the versioned comment marker (4.8, needed anyway for metrics) and the habit of recording *why* you intervened as a bd comment. Revisit when there are ≥50 recorded interventions. Counterargument: the loop is the "compound interest" of the factory. Response: compound interest on a principal of zero is zero.

**R7. STOP: strike multi-harness rungs, MCP support, and complexity-based model routing from the active roadmap.** (Roadmap 5.4/5.6, capability-inventory PARTIALs.) Each is a months-scale program serving a scale gonk doesn't have. The rung interface already *is* the seam; leave it a seam. Keep from Phase 5 only: the cross-local-model failover chain (it is the principled fix for `ob5`'s failure mode), the FAILED/ERROR error taxonomy, and the cost footer + 80% budget alert (small, real operator value).

**R8. First implementation target: not gonk.** Point the Phase-1 agent at a low-stakes repo (a docs repo, `homelab/ci-templates`, a toy service) for its first month. An agent whose effect batches can touch the repository containing its own broker, gate, and shape definitions is the exact scenario the protected-path denylist and `gonk-066` exist for — and `gonk-066` is open. Dogfooding on gonk itself is the natural temptation (70 open beads!) and should wait for the workflow-scope gate.

**R9. Credential Ollama now.** One small auth proxy on bailey converts local budgets from advisory to hard, independent of the suspended Cilium migration, and un-falsifies spec §9 for the local rung. This is infra outside the gonk repo, one evening of work, and closes the largest honest asterisk in every gonk document. File the bead; it currently exists only as a sentence in `docs/environment.md`.

**R10. Instrument the two numbers from the first implementation MR** (roadmap §11 has this right; endorse loudly): fraction of closed beads reaching merge with zero human edits, and cost per merged change (GPU-seconds + operator minutes). Warp proposed the metric and never published their own value. Publishing yours — even when it is embarrassing — is both the steering signal and, given the open-sourcing intent, the most credible marketing gonk could have.

---

## 6. Predictions (falsifiable, check in Q4 2026)

1. **Implementation quality:** of the first 20 implementation-agent MRs on `qwen3-14b`, fewer than 3 merge without human edits, and the dominant failure will be context exhaustion / partial edits on repos over ~30 files — not wrong intent. Falsified if ≥20% merge clean.
2. **The return channel breaks on schedule:** the first multi-file code change will overflow the transcript/peek return path, and code effects will move to rig-commit-plus-broker-push within two weeks of Phase 1 starting. Falsified if transcript-carried diffs survive a month of real use.
3. **The 4-state rubric over-parks:** after adopting it, the share of issues landing `needs-info`/`wait-to-implement` will exceed 60% for the first month — the cautious tie-break rule plus a cautious 14B model compounds — and it will be read (wrongly) as the rubric failing rather than needing vocabulary tuning.
4. **`gonk-fan` claims another real event** (a webhook silently dropped in the post-restart window causing a missed triage on a real issue) before webhook parking (`gonk-icj`) ships — and parking gets built well before its scheduled Phase 5 slot as a result.
5. **The context-isolation plan collides with bailey's memory:** running a distinct search-subagent model resident alongside `qwen3-14b` won't fit (bailey already saturated, `gonk-38o`/`gonk-m4k`), forcing either `qwen3-8b-bailey` as the subagent model or sequential model loading with painful latency. The roadmap does not currently acknowledge this constraint anywhere.
6. **Nobody runs the §11.6 kill tests before an incident runs one for real** — most likely candidate: a LiteLLM restart mid-session being classified as a gate failure and burning an escalation, i.e., the ADR-004/005 known-false-positive firing in production before it was ever measured.
7. **A Gas City version bump breaks a workaround** (event-log shape, session async semantics, or pod labeling) before any upstream fix lands for #4668/#4891 — validating the pin-harder posture of §2.5.
8. **If R4 is not taken**, the mention/formula corpse costs at least one more confused debugging session in which a dispatched mention silently idles and is investigated as a new bug.

---

## 7. The one-paragraph verdict

The prior research's conclusion — no adopt, no integrate, no cutover, mine the mechanisms — is right, and the roadmap it produced is 80% right. The 20% that's wrong is ordering and appetite: it schedules preconditions (context isolation, deterministic harness) after the phase that needs them, schedules spec-agent machinery a solo operator doesn't need yet, and keeps fantasy line items (self-improvement, multi-harness, MCP) on the books where they'll leak attention. gonk's architecture is better than Warp's where it counts for this deployment — the bead as unit of work, the deterministic gate, the zero-cred broker, budgets at a door you own — and worse in exactly two places, both fixable and both currently open beads: the prompt channel (`gonk-e9m`/`ob5`) and the absence of the deterministic test harness the spec demanded in 2026-07 and the last month spent proving necessary. Warp's deepest lesson isn't any mechanism; it's the curve: a 102-person company stalled at the implement stage with 1,300 issues queued behind a triage system that works great. gonk is one working triage path away from the same cliff. The next quarter should be spent making one repo's issues become merged MRs with honest verification — and saying no to everything else on the parity table.
