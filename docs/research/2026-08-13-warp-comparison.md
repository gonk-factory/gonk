# Warp.dev vs gonk: integrate, build on, learn from, or cut over?

**Date:** 2026-08-13
**Bead:** gonk-7dl
**Question:** Should gonk integrate with Warp, build on top of it, just take design lessons from it — or should we replace gonk with Warp entirely for local inference / dev?
**Method:** Four parallel research passes (Warp product/architecture, Warp open-source footprint, Warp business/deployment terms, gonk current state from this repo at HEAD `5b09b8c`). Sources cited inline; all Warp claims are as of August 2026.

---

## TL;DR

**Recommendation: take design lessons (do this now), optionally adopt the open-source Warp client as the *human cockpit* for gonk's output, do not build on Warp, do not integrate with Oz, and do not cut over.**

The one-sentence reason: **Warp in 2026 is an open-source client welded to a mandatory proprietary cloud, and the cloud sits exactly where gonk's core invariants live.** Every AI request — including "bring your own endpoint" — routes through Warp's backend; a custom OpenAI-compatible endpoint must be *publicly reachable over HTTPS* (localhost/private IPs rejected; their docs recommend ngrok-tunneling Ollama); the agent harness is server-side and closed; custom endpoints don't work for the event-triggered cloud agents at all. gonk's whole point — self-hosted GitLab on a private LAN, in-cluster LiteLLM→Ollama inference that never leaves the network, hard per-project budgets enforced at the LiteLLM door, deterministic no-LLM-judge escalation — is structurally unserved by Warp today.

That said, Warp has solved several problems gonk has on its roadmap (multi-agent review UX, interactive code review loops, cost visibility UX, declarative agent environments), and the April 2026 open-sourcing means those solutions are now readable source, not just screenshots. That's where the value is.

**Re-evaluate if any of these land:** (1) Warp ships the promised local Rust harness with direct-to-model routing (announced May 2026, not shipped); (2) Warp open-sources the server/Oz ("no committed date"); (3) ACP (Agent Client Protocol) support lands ([warpdotdev/warp#9233](https://github.com/warpdotdev/warp/issues/9233)), which would let gonk-driven agents plug into Warp's UI cleanly.

> **UPDATE 2026-08-13 (same day, follow-up research — this strengthens the verdict).** Watch item (1) has moved *backwards*, not forwards. Timeline from Warp's own staff: **2026-05-06** — "I expect work to start within the next few weeks… completely client side"; **2026-05-20** — the blog restates the plan with no date; **2026-07-30** — "**Unfortunately the way our agent architecture is set up currently doesn't allow us to support local models.**" Zero code exists: no `agent-client-protocol` crate in any `Cargo.toml`, no `specs/GH7326/` directory, the ACP spec PR untouched since 2026-05-03, and a 4,953-line community ACP implementation **closed the same day on process grounds**. The May–June roadmap milestone is overdue and no successor has been published. Meanwhile Warp's Q3 2026 thesis is explicitly *"get agents off your machine"* — *"software belongs in the cloud, not on individuals' desktops"* — and an open PR would **remove** OpenCode multi-harness support, the very harness named as the ACP path to local models. They have also since hardened the *justification* for the proxy (SSRF exposure, plaintext transit, SOC 2 encryption-in-transit obligations), which makes it a stated security position rather than a temporary limitation. Demand is not the constraint: [#4339](https://github.com/warpdotdev/warp/issues/4339) "Make Warp work with Local Language Models" has **1,426 reactions**, is their top issue, has been open since 2024-02-26, and has never reached `ready-to-spec`; at least four community forks exist because upstream won't ship it. **Concrete signal to watch instead of the blog:** an `agent-client-protocol` crate entering a `Cargo.toml`, or a `specs/GH7326/` directory appearing in `warpdotdev/warp`. Tracked in `warp-platform-capability-inventory.md` §0.

---

## 1. What Warp is in 2026

Warp calls itself an **Agentic Development Environment (ADE)** — "not a terminal, not an IDE." Three AI layers: always-on hints, interactive Agent Mode in the terminal, and **Oz** — cloud-hosted autonomous agents with event triggers. Timeline of the relevant pivots:

- **2022–2024:** Rust/GPU terminal with the "blocks" UI; Agent Mode (June 2024); login requirement lifted (Nov 2024).
- **June 2025:** Warp 2.0 "ADE" rebrand — multi-agent management panel, parallel agents in vertical tabs, combined cross-agent diff review.
- **Sept 2025:** Warp Code — file editing, code review panel, "prompt to production."
- **Late 2025/2026:** **Warp Agent CLI / `oz` CLI** (headless, documented for CI and Kubernetes pods) + Oz platform (REST API, Python/TS SDKs, triggers from Slack/GitHub/**GitLab**/Jira/Linear/webhooks).
- **April 14, 2026:** third-party CLI agents become first-class — 14 agents auto-detected and "enhanced" in the UI, including **Claude Code, Codex, Gemini CLI, and opencode**.
- **April 28, 2026:** **the entire client goes open source** — AGPL-3.0 for the app, MIT for the `warpui`/`warpui_core` UI crates, with OpenAI as "founding sponsor." 64k stars, daily pushes. ([announcement](https://www.warp.dev/blog/warp-is-now-open-source), [repo](https://github.com/warpdotdev/warp))

**What stayed closed** (per Warp's own [FAQ](https://github.com/warpdotdev/warp/blob/master/FAQ.md)): the server, hosted auth, Warp Drive backend, **Oz**, and — critically — **the agent harness, which runs server-side**. Direct quote: "Warp's built-in agent harness runs server-side and isn't open in this repo today." On open-sourcing the server: "We haven't committed to a date and don't want to overpromise."

**The inference path** (this is the load-bearing fact for us): even with BYO API key or a custom OpenAI-compatible endpoint, "your local Warp client pulls your endpoint URL **and API key** from your device's secure storage and sends them up to **Warp's backend** along with your prompt" ([custom endpoint docs](https://docs.warp.dev/agent-platform/inference/custom-inference-endpoint/)). The endpoint must be public HTTPS — localhost, 127.x, and private IPs are rejected. Ollama requires an ngrok/Tailscale-Funnel tunnel. Custom endpoints don't apply to cloud agents at all. There is no offline mode. The only inference path that bypasses Warp's servers is Enterprise-only BYOLLM, limited to AWS Bedrock / Vertex AI.

**Business posture:** ~$73M raised (last priced round 2023), ~102 employees, ~500k claimed users, conflicting ARR estimates ($16–32M). Pricing churned **three times in 14 months** (the "unlimited AI" Turbo plan was cut to 3,000 requests/mo in 2025, partially reverted after backlash; credits re-based Oct 2025; free credits apparently removed in 2026). Current tiers: Free ($0, zero included credits, BYOK allowed ≤10-person orgs), Build $20/mo, Max $200/mo, Business $50/user/mo (SSO+ZDR), Enterprise (per-user spend caps, Analytics API, BYOLLM). ToS prohibits automated access to the Services outside the sanctioned CLI/SDK, and using the Services to build a competitive product. ([pricing](https://www.warp.dev/pricing), [ToS](https://www.warp.dev/legal/terms-of-service))

## 2. What gonk is today

(From the repo at `5b09b8c`, 2026-08-13 — see `docs/HANDOFF-next-session.md` and PLAN.md.)

A self-hosted software factory for **gitlab.orac.local** (GitLab 18.10.1 CE, private CA, private LAN): Gas City orchestrator + opencode agents as k8s pods + all LLM traffic through in-cluster LiteLLM → Ollama on bailey, with per-project **hard budget enforcement via LiteLLM virtual keys** (synthetic per-token pricing makes the USD ceiling bind even for local models), a **deterministic outcome-gated escalation ladder with no LLM judges anywhere** (a regression test greps the classifier package to prove no model is consulted), fail-closed config, secrets as file mounts, provenance trailers, and agent pods that hold **no forge credentials** (broker applies proposed-effects batches; rig checkout is grant-gated tarball fetch).

Status: deployed on orac via Flux; Plans 01–06 essentially done; **a real model-driven triage has run end-to-end** (issue 21 → dispatch → `qwen3-14b` → well-formed effects batch, session 5). Open pain: the scaffold path is broken (`gonk-4xr`/`gonk-bgx`), prompt delivery is still keystrokes into the opencode TUI (`gonk-e9m`, injection-mitigated but lossy; prompt-by-reference plan T1–T6 unstarted), per-bead spend attribution regressed on the submit path (`gonk-m6t`). 61 open issues. Known accepted gap: local-model budgets are advisory until NetworkPolicy is enforced (Flannel; Cilium suspended) because Ollama is credential-less.

## 3. Head-to-head on the dimensions that matter to gonk

| Dimension | gonk | Warp (Aug 2026) |
|---|---|---|
| Inference locality | In-cluster LiteLLM → Ollama; nothing leaves the LAN | All AI routes through Warp's cloud; custom endpoints must be **public HTTPS**; endpoint **API key is sent to Warp's backend**; no offline mode |
| Local models for autonomous/event-driven agents | The default (local-first ladder) | **Impossible** — custom endpoints don't apply to cloud agents |
| Budget enforcement | Hard, per-**project**, at the LiteLLM door (virtual keys + synthetic pricing); meter never in the request path. ⚠️ *Design holds; on this cluster the USD door currently closes only on **cloud** rungs — local models carry no price in LiteLLM's cost map (`gonk-1zm`). Meter token ceilings still bind.* | Credit caps per **team** (per-user = Enterprise); enforced in Warp's billing, not at your model door; BYOK spend is uncapped by Warp |
| Escalation policy | Deterministic ladder, infra failures never escalate, no LLM judges (tested invariant) | "Auto" mixed-model routing — model-driven, opaque, burns Warp credits even with BYOK |
| Forge | Self-hosted GitLab CE on private LAN, webhook → intake | GitLab trigger integration exists **but via Oz cloud** — needs your GitLab reachable from Warp's servers |
| Agent credentials | Pods hold no forge creds; broker applies effects under bot PAT; ed25519 grant-gated dispatch | Oz secrets store injects creds into (mostly Warp-hosted) runtime; "self-hosted cloud agents" is an Enterprise pricing bullet with **no documentation found** |
| Headless/programmatic | Everything is headless; k8s-native | `oz` CLI + REST/SDKs, documented for CI/k8s — but authenticates against Warp's backend, always |
| Provenance/audit | Commit trailers, beads audit trail, Grafana spend dashboards (per-bead attribution being repaired) | Enterprise Analytics API: per-user/per-team; **no per-project attribution** |
| Licensing | Intended MIT after validation | Client AGPL-3.0 (+MIT UI crates); server/harness/Oz proprietary; several org repos unlicensed (`oz-agent-worker`, `oz-agent-action`) |
| Multi-agent UX | None (this is Warp's strength) | Agent management panel, parallel agents, combined cross-agent diff, interactive review comments fed back to agents |
| Maintenance burden | All yours (real: 61 open issues, registry ops, chart, meter…) | Warp's problem; daily releases, funded team — but 3 pricing changes in 14 months and a closed core you can't fix |

## 4. Option A — Integrate with Warp

*Meaning: keep gonk as the factory; use Warp where it touches humans. Two sub-options.*

**A1. Warp client as the human cockpit (viable, cheap, optional).** The AGPL client is a genuinely good terminal, and since April 2026 it hosts third-party CLI agents — including **opencode and Claude Code** — with a rich input editor, vertical tabs, and inline review comments. Steve could run it as his daily terminal and use its Code Review panel to review gonk-produced MR branches locally. Using Warp as your terminal triggers no AGPL obligations (their FAQ says so explicitly), needs no account for base terminal use, and paid-plan telemetry can be off.

- *For:* zero coupling to gonk's architecture; free UX win on exactly the part gonk has no UI for (a human reviewing multiple agents' output); reversible any day.
- *Against:* Warp's own AI features still route through their cloud, so either don't use them or accept prompts transiting Warp's servers; it's a personal-tooling choice, not a gonk feature; WSL support quality for the client should be verified before adopting (untested here).

**A2. Oz integrations as gonk's intake/trigger layer (not viable).** Oz can trigger agents from GitLab events — but the integration runs in Warp's cloud, which cannot reach `gitlab.orac.local` without exposing your GitLab (and then your LiteLLM, and then your secrets pipeline) to the internet. It also can't use local models at all (see §3). This inverts every trust decision in the stack (ADR-003's verify-before-parse instance-secret webhook design exists *because* the intake is yours).

**Verdict: A1 yes if the cockpit appeals; A2 no.**

## 5. Option B — Build on top of Warp

*Meaning: adopt Warp components as gonk building blocks.*

What's actually on the shelf:

- **The AGPL client.** gonk is headless Go; the client is an interactive Rust app. There's nothing to embed, and AGPL is incompatible with gonk's intended MIT open-sourcing if any of it were vendored. Warp's FAQ is refreshingly honest: "AGPL is stricter than what some companies are comfortable embedding… we accept that."
- **`warpui`/`warpui_core` (MIT).** A Rust GUI framework. Irrelevant to gonk unless we someday build a native dashboard — and it's not even on crates.io yet.
- **The agent harness.** The thing we'd actually want — and it's server-side, closed, with no committed open-sourcing date. You cannot BYO-model into it.
- **Oz SDKs (Apache-2.0) / `oz-agent-worker`.** Client libraries for Warp's cloud; the worker repo (run Oz agents on your own infra) is **unlicensed** and still orchestrated by Warp's platform. Building on these means building on a proprietary control plane with a 3-changes-in-14-months pricing history and a ToS that bans building competitive products — and gonk *is* arguably a competing agent-orchestration product.
- **Skills/workflows formats (MIT/Apache).** `oz-skills`, `common-skills`, `workflows` are openly licensed content formats — fine to read and borrow patterns from (see Option C).

- *For:* the open client is real, buildable, and active (64k stars, daily pushes); if ACP lands, "gonk agents rendered in Warp's UI" becomes a clean, arms-length integration rather than a build-on.
- *Against:* the open parts are the parts gonk doesn't need; the parts gonk would want are closed; license posture (AGPL core, unlicensed edge repos) conflicts with MIT plans; strategic dependency on a VC-funded company mid-pivot with an OpenAI sponsorship.

**Verdict: no. Nothing on the open shelf replaces a gonk component, and the closed parts can't be built on safely.**

## 6. Option C — Take design lessons (recommended, actionable now)

Warp has production-tested answers to problems already on gonk's roadmap. Concrete lessons worth stealing:

1. **Combined cross-agent diff review.** Warp's "Changes vs. main" view merges all active agents' edits into one reviewable diff. gonk's verified-change pipeline design (2026-08-02 spec) needs exactly this shape for its human gate: when multiple sessions touch a repo, present one combined red/green diff, not N MRs. Lesson: review is a *view over agent output*, not a per-agent artifact.
2. **Interactive review comments fed back to the agent.** Warp batches inline diff comments and sends them to the agent as one revision pass. gonk's mention-agent + broker effects loop could adopt this: reviewer comments on the MR → batched → one re-sling with the comment set as context, rather than ad-hoc @-mention threads. Maps directly onto the existing `gonk-mention` order.
3. **Cost visibility UX.** Warp shows per-conversation credit totals and a `/cost` command; admins get caps with approach-alerts and hard stops. gonk's v1.5 "per-comment cost footers" should copy the *placement* (cost where the work is seen, not only in Grafana) — and the approach-alert pattern (warn at 80% of a project's monthly budget via a GitLab comment) is a cheap, high-value addition to gonk-meter's quiet-hours/defer machinery. Note Warp still lacks per-project attribution ([#12075](https://github.com/warpdotdev/warp/issues/12075)) — gonk's atags design is *ahead* here; fixing `gonk-m6t` protects a genuine differentiator.
4. **Declarative agent environments.** Oz "environments" = Docker image + repos + startup commands + secrets, as config. gonk's equivalent knowledge is currently spread across the agent image, pack agent.toml, and chart values. Worth consolidating into one per-project stanza in `.gonk.yml` v2 (rung-appropriate image, setup, allowed repos) — same fail-closed resolution as everything else.
5. **Skills as versioned, shareable content.** `oz-skills`/`common-skills`/AGENTS.md-pickup gave Warp a community contribution surface with no runtime coupling. The `.agent/` scaffold (v1.5 quality pass) should define its conventions so they're publishable standalone — that's also the gonk-navigator seam.
6. **Blocks as the structured substrate.** Warp's agents operate over typed command/output blocks, not scrollback. gonk's analog: session transcripts as typed events (already partially true via the effects batch). Keep pushing structure to the boundary — the broker's `effect-shape.toml` validation is the right instinct; extend it rather than parsing prose.
7. **A cautionary lesson, not just positives:** Warp's login/telemetry backlash (2022–2024) and pricing churn show how fast goodwill burns when the control plane is opaque. gonk's "models propose, deterministic checks and humans grant" invariant and open-sourcing plan are the opposite posture — document them loudly when open-sourcing.

## 7. Option D — Full cutover: replace gonk with Warp for local inference / dev

*What it would take, honestly, and why it fails today.*

**The cutover checklist (what you'd have to do):**

1. **Expose inference to the internet.** Stand up a public HTTPS endpoint for LiteLLM (ngrok/Tailscale Funnel/reverse proxy on a public host), because Warp rejects private IPs — then hand that URL **and its API key to Warp's backend**, accepting that every prompt and your key transit their servers. This directly abandons the "nothing leaves the LAN" property and widens the attack surface around a credential-less Ollama.
2. **Accept that the factory can't use local models anyway.** Custom endpoints only work for *interactive* sessions. The event-driven part — the actual gonk use case (issue opened → triage) — runs on Oz cloud agents, where custom endpoints are unsupported. Cutover therefore means **cloud models (Warp-metered or Enterprise Bedrock/Vertex) for all autonomous work**; the DGX Spark becomes decoration. The promised direct-to-model local harness (May 2026 blog) has not shipped.
3. **Expose GitLab.** Oz's GitLab integration needs to reach your instance; `gitlab.orac.local` with a private CA would need public exposure or an Enterprise arrangement ("self-hosted cloud agents" appears on the pricing page with no documentation — unverified).
4. **Rebuild budget policy inside Warp's billing.** Trade per-project hard USD ceilings at the model door for per-team credit caps (per-user only on Enterprise). No per-project attribution exists. Synthetic pricing for local rungs has no equivalent (moot, since local rungs don't exist there). BYOK spend isn't capped by Warp at all.
5. **Drop the invariants with no Warp equivalent:** deterministic no-judge escalation (Warp's "Auto" router is the opposite), provenance trailers, fail-closed config resolution, no-forge-creds-in-pod broker model, beads-auditable dispatch. These aren't features Warp does worse; they don't exist.
6. **Take on account/ToS/pricing risk:** per-seat costs for an audience of ~1 human + N bots (bot-driven "users" sits awkwardly with multi-account and automation clauses; the sanctioned path is the `oz` CLI under one account), three pricing changes in 14 months, and a vendor whose ToS bans building competitive products while gonk-navigator/gonk itself may someday be exactly that.

**The honest case FOR cutover (steelman):** gonk is expensive. Sixty-one open issues; the scaffold path is broken; prompt delivery is keystrokes with a sanitizer; you personally run the registry, the chart, the meter, and the cluster. Warp is a funded team shipping daily with a genuinely excellent multi-agent UX, working GitLab triggers, code review built in, and SDKs — for $20–50/user/mo you'd delete thousands of lines of Go and several ops responsibilities. If the goal were "agentic development with cloud models, minimum effort," Warp (or Claude Code + CI, for that matter) would beat building gonk.

**Why it still fails:** the goal isn't that. The spec's reason for existing is *local-first inference on your own GPU, on a private GitLab, with budgets you enforce at a door you own*. Every item above isn't a migration cost — it's the deletion of the requirement. Warp's own architecture (server-side harness, founding sponsorship by a model vendor, credits as the business model) is *structurally* aligned with cloud inference; the local-harness promise is real but unshipped, and when it ships it will still be inside an AGPL client designed for interactive use, not a headless k8s factory.

**Verdict: no cutover. Cost of adoption ≈ abandoning the project's premise. Re-check on the three watch items in the TL;DR.**

## 8. Recommendations (ranked)

1. **Now:** mine Option C — file beads for the design lessons worth building: budget approach-alerts as GitLab comments (extends gonk-meter), combined-diff review shape for the verified-change pipeline, batched review-comment re-sling for `gonk-mention`, declarative per-project agent environment stanza for `.gonk.yml` v2. (Filed as follow-up bead — see gonk-7dl notes.)
2. **Now:** fix `gonk-m6t` (per-bead attribution) with extra motivation: per-project cost attribution is something Warp's Enterprise Analytics API *doesn't have*. It's a real differentiator, not plumbing.
3. **Optional, personal:** try the open-source Warp client as the interactive cockpit (it hosts opencode and Claude Code as first-class agents); keep its cloud AI off or pinned to BYOK if used. No gonk coupling.
4. **Don't:** build on Warp components, route anything through Oz, or plan a cutover.
5. **Watch (re-evaluate on any):** local direct-to-model harness ships; server/Oz open-sourced; ACP support lands ([#9233](https://github.com/warpdotdev/warp/issues/9233)) — ACP would make "gonk sessions visible in Warp's UI" a clean integration and is the most plausible future convergence point.

## Appendix: source notes

Full sourced research notes from the four passes (product/architecture; open-source footprint; business/deployment; gonk state) are summarized above; key primary sources:

- https://www.warp.dev/blog/warp-is-now-open-source and https://github.com/warpdotdev/warp (FAQ.md — licensing, open/closed boundary, server-side harness)
- https://docs.warp.dev/agent-platform/inference/custom-inference-endpoint/ (public-HTTPS requirement, key-transits-backend quote, Ollama-via-ngrok)
- https://docs.warp.dev/agent-platform/cloud-agents/platform/ (Oz triggers, GitLab integration, environments)
- https://docs.warp.dev/enterprise/enterprise-features/bring-your-own-llm/ (Bedrock/Vertex only)
- https://www.warp.dev/blog/bring-your-own-inference-to-warp (local harness = roadmap)
- https://www.warp.dev/pricing, https://www.warp.dev/legal/terms-of-service
- https://github.com/warpdotdev/warp/issues/9233 (ACP), /issues/12075 (no per-task cost API)
- Repo: `docs/superpowers/specs/2026-07-12-gonk-stack-design.md`, `docs/HANDOFF-next-session.md`, `docs/environment.md`, PLAN.md
