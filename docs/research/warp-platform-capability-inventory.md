# Warp / Oz platform capability inventory

**Living document — this is a comparison baseline, not a point-in-time analysis.**

| | |
|---|---|
| **Snapshot date** | 2026-08-13 |
| **Bead** | gonk-edk |
| **Re-survey cadence** | Quarterly, or on any major Warp announcement (open-sourcing the server/Oz, ACP landing, a local harness shipping, a pricing change) |
| **Next re-survey due** | 2026-11-13 |
| **Companions** | `2026-08-13-warp-comparison.md` (adopt/integrate verdict) · `2026-08-13-warp-blog-lessons.md` (mechanisms) · `2026-08-13-gonk-local-parity-roadmap.md` (build plan) |

**How to use this.** Columns are: what Warp ships, which tier gates it, and where gonk stands. When re-surveying, update the *Warp* columns, flip any changed gonk column, and append to the changelog at the bottom. Rows are grouped so a diff against a later snapshot reads cleanly.

**gonk status legend:** **HAVE** shipped · **PARTIAL** exists but incomplete · **FLIGHT** open bead · **NONE** not built · **WON'T** deliberately out of scope · **AHEAD** gonk exceeds Warp here

---

## 0. The load-bearing architectural facts

These determine everything else and should be re-checked first at each survey.

| Fact | State as of 2026-08-13 | Why it matters |
|---|---|---|
| **All inference proxies through Warp's server** | True. Custom OpenAI-compatible endpoints require a **publicly reachable HTTPS URL**; localhost/private IPs rejected. Warp's client sends your endpoint URL *and API key* to their backend | This single fact blocks adoption for a LAN-local factory |
| **Agent harness is server-side and closed** | True. Client is AGPL-3.0 open (Apr 2026); server, Warp Drive backend, and Oz remain proprietary. FAQ: *"Warp's built-in agent harness runs server-side and isn't open in this repo today"* | You cannot BYO-model into their harness, only into their proxy |
| **Client-side local harness** | **NOT SHIPPED — and receding.** May 6 2026: *"I expect work to start within the next few weeks."* **July 30 2026: *"the way our agent architecture is set up currently doesn't allow us to support local models."*** Zero code, no spec dir, no milestone | This was the #1 watch item; it moved backwards |
| **ACP (Agent Client Protocol)** | Team-committed on the May–June roadmap; **no implementation**. No `agent-client-protocol` crate, no `specs/GH7326/`. Spec-only community PR open and untouched since 2026-05-03; a 4,953-line community implementation was **closed same-day on process grounds** | The stated path to local models via OpenCode |
| **Server/Oz open-sourcing** | Explicitly non-committal: *"We haven't committed to a date and don't want to overpromise"* | — |
| **Strategic direction** | Countercurrent. Jul 2026 post: *"If you actually want governance, security, and automation you should move all your coding agents to the cloud"* / *"software belongs in the cloud, not on individuals' desktops."* An open PR would **remove** OpenCode multi-harness support — the harness named as the ACP path to local models | A well-resourced local push is unlikely near-term |
| **Justification hardening** | Jul 2026: SSRF into warp-server, plaintext transit, response tampering, *"compliance/security obligations… with SOC2 (we enforce encryption-in-transit)"* | They now have a security rationale for keeping the proxy |

**Demand signal:** issue #4339 *"Make Warp work with Local Language Models (like Ollama)"* — **1,426 reactions, open since 2024-02-26**, still `triaged`, never `ready-to-spec`. By far their top issue. At least four community forks exist because upstream won't ship it.

---

## 1. Agent platform (Oz)

| Capability | What it does | Tier | gonk |
|---|---|---|---|
| Cloud agent runs | Containerized agent session per task; container destroyed after each run | Free (limited) → paid | **HAVE** — k8s pods via Gas City |
| **Environments** | Docker image + N git repos + ordered setup commands, reusable across triggers; team-shared by default | Paid | **NONE** — knowledge split across image/pack/chart (roadmap 4.5) |
| **Runners / instance shapes** | Separate object from environment: OS, arch, `--vcpus`, `--memory-gb`; per-run override. Enterprise to 32 vCPU / 64 GiB | Paid | **NONE** — fixed pod resources |
| Harnesses | Warp Agent (default), Claude Code, Codex — same triggers/secrets/observability. Only Warp Agent can be an orchestration parent | Paid | **PARTIAL** — opencode only; rung seam designed |
| Agent identities | Service accounts with own secrets/skills/permissions; only humans may create them | Paid | **PARTIAL** — one bot user |
| Skills-as-agents | Any skill launchable as an agent, including Claude Code / Codex / `.agents/` skills | Paid | **NONE** |
| Orchestration | Parent/child **exactly one level deep**; durable server-backed message bus, per-agent inboxes; **run-state transitions and messages share a global sequence number**; parent success is **not** transitive | Paid | **WON'T** (their own benchmarks rejected multi-agent) |
| Scheduled agents | `oz schedule --cron` as a first-class trigger | Paid | **PARTIAL** — `gonk-sweep` cooldown only |
| REST API + Python/TS SDKs | Create/monitor runs; retries and timeouts built in; queueing/rate-limiting explicitly pushed to the app layer | Paid | **PARTIAL** — meterapi, no run API |
| Agent CLI (`oz`) | Headless; documented for CI, GitHub Actions, Kubernetes | Free+ | **HAVE** — everything is headless |
| Handoff local↔cloud↔cloud | Session + context transfer in all directions | Paid | **WON'T** |
| Workspace snapshots | End-of-run declarations script emits JSONL (`repo` entries **diffed not copied**; `file` entries verbatim); per-run output path so concurrent runs don't clobber; malformed lines skipped not fatal; best-effort | Paid | **NONE** — interesting pattern, low priority |
| Self-hosted execution | `oz-agent-worker` daemon, WebSocket dial-out. Backends: **Docker, Kubernetes (Job per task, Helm chart), Direct, Command**. Linux-only. **Control plane stays with Warp** | Enterprise | **AHEAD** — gonk owns both planes |

### Self-hosting detail worth mining (Kubernetes backend)

| Mechanism | Detail |
|---|---|
| Preflight Job | Verifies RBAC, admission policy, image pullability **before accepting any tasks**; worker exits on failure. Surfaces config problems at deploy time |
| RBAC | Namespace-scoped only: `create,get,list,watch,delete` jobs; `get,list,watch` pods; `get` pods/log; `list` events. **No CRDs, no cluster-scoped RBAC** |
| Scaling | **One replica per worker ID**; run multiple Helm releases rather than scaling one horizontally |
| Native sidecars | Service deps go in `initContainers` with `restartPolicy: Always`; worker **ignores their exit codes** (kubelet SIGTERMs them, JVMs exit 143) |
| Disruption | Worker termination **preserves active task Jobs**; task pods are *not* disruption-proof — schedule worker and task pods on separate node pools |
| Cleanup asymmetry | Successful Jobs deleted immediately; **failed Jobs left in place for post-mortem**, reaped by `ttlSecondsAfterFinished` (24h default) |
| `unschedulable_timeout` | Default 30s — fail a task early if its pod can't schedule |
| Command backend | Worker doesn't run the agent; invokes an operator-owned dispatch command. **Task payload and secrets arrive as JSON on stdin — deliberately kept out of env and argv** |

---

## 2. Triggers and integrations

| Capability | Detail | Tier | gonk |
|---|---|---|---|
| GitHub App | `@oz-agent` mentions on issues/PRs/review threads | Paid | n/a |
| **GitLab** | **No native integration.** Documented path is manual: a `read_repository` token in a setup command to clone. Cannot push | — | **AHEAD** — GitLab is gonk's native forge |
| Slack / Linear / Jira / Bitbucket / Azure DevOps | First-party event triggers | Paid | **NONE** |
| Custom webhooks | Arbitrary event → agent run | Paid | **HAVE** |
| Cron schedules | Time-based triggers | Paid | **PARTIAL** |
| GitHub Actions (`oz-agent-action`) | Run a skill from CI; `cloud: true` routes to Oz | Paid | n/a |
| **Dedupe model** | **Thread-scoped, one run per thread.** A new mention during an active run is delivered as a *follow-up instruction*, not a second agent. Bot-authored mentions ignored. **Edits don't trigger** | — | **PARTIAL** — `ghook` dedupe; no thread-scoped run coalescing |
| Idempotency keys | **None in the API** — pushed to the app layer | — | **AHEAD** — deterministic `BeadAnchor` |
| Context extracted per event | Issue: title/desc/labels/state/thread. PR review comment: PR + thread + **diff of the commented file**. CI: failure logs | — | **PARTIAL** — `gonk-kp5` (re-fetches data the webhook already delivered) |

---

## 3. Agent behavior configuration

| Capability | Detail | Tier | gonk |
|---|---|---|---|
| Rules files | `AGENTS.md` (recommended) / `WARP.md` (legacy, wins if both). Also links `CLAUDE.md`, `.cursorrules`, `GEMINI.md`, `.clinerules`, copilot-instructions. **Filename must be ALL CAPS.** Precedence: subdir → repo root → global. Subdir application is **best-effort, not guaranteed** | Free+ | **PARTIAL** — pack prompt templates |
| **Skills** | `SKILL.md` + `scripts/` + `references/` + `assets/` under `.agents/skills/` (also `.claude/`, `.opencode/`, …). Frontmatter is just `name` + `description`. **Progressive disclosure**: only name+description loaded at start; body on trigger; references on demand. Authoring rule: **≤200 lines**, split to `references/`, one level deep | Free+ | **NONE** (roadmap 2.5) |
| Skill distribution | `npx skills@latest add <org>/<repo> --skill '*' --agent warp` | Free+ | **NONE** |
| Skills ship with evals | `evals/evals.json`: `prompt`, `expected_output`, `assertions` — **including negative cases** ("Selects no report") | — | **NONE** (roadmap 4.12) |
| Agent Profiles | Named (model + permission set) bundle — the unit of least privilege | Paid | **PARTIAL** — rungs are budget-shaped |
| Permission toggles | Auto-accept diffs; read files without asking; run commands autonomously; **command allowlist + denylist**; per-MCP-server trust | Free+ | **PARTIAL** — pod has no forge creds by construction |
| **Agent Memory** | Cross-harness persistent memory, pluggable sources, writable. **Research preview, waitlist-gated, no mechanism published** | Enterprise (preview) | **PARTIAL** — beads memories |
| Codebase context | Embeddings index, git-tracked files only, ≥5,000 files/codebase. Re-indexes at each new conversation. **With it off, the agent falls back to grep/sed** | Free+ | **WON'T** — their own SWE-bench agent uses no embeddings |
| MCP | stdio + Streamable HTTP/SSE; OAuth, env vars, or Bearer headers. **OAuth MCP unsupported for cloud agents.** Team-shared configs with automatic secret scrubbing | Free+ | **NONE** (roadmap 5.6) |
| **MCP search subagent** | Tool defs cost **up to 50k tokens**, unused in **~90%** of conversations. Search moved to a subagent with a fresh window on a **smaller, cheaper model**: −26% tokens where MCP used, −10% where merely available; LLM-judge showed no quality regression | shipped 2026-01 | **NONE** — precondition for gonk (roadmap 4.1) |

---

## 4. Inference and model routing

| Capability | Detail | Tier | gonk |
|---|---|---|---|
| Managed models | OpenAI, Anthropic, Google, xAI + open-weight (GLM, DeepSeek, Kimi, MiniMax, Qwen) — all open models hosted via **Fireworks**, not self-run | All | **AHEAD** — local Ollama |
| **Auto routing tiers** | `auto` (Responsive), `auto-efficient`, `auto-genius` (escalates by complexity), `auto-open` (best open-weight) | All | **PARTIAL** — rung ladder is budget-driven |
| **Custom routers** | YAML in `~/.warp/custom_model_routers/`, hot-reloaded. Exactly two types: **`complexity`** (easy/medium/hard) and **`prompt`** (ordered description→model, first match wins). **Required `default`.** Cannot target Auto models, other routers, or custom endpoints (no cycles). Unparseable file → non-blocking error, that router skipped | Paid; team-synced at Enterprise | **NONE** — highest-value transferable idea (roadmap 5.4) |
| Routing granularity | **Per conversation, not per step** — switching mid-conversation resets prompt caching | — | n/a |
| Routing observability | Per message: `user_selected_model` **and** resolved `model_ids` **and** `credit_charged` — requested policy and resolution stored separately | Enterprise | **NONE** — copy this schema |
| Provider failover | Automatic on outage/capacity, swaps back when restored. Benchmark chain crosses **vendors** because *"retrying with the same model often produced repeat failures"* | — | **FLIGHT** — `gonk-ob5` fails *open* to cloud (roadmap 5.5) |
| BYOK | Own Anthropic/OpenAI/Google key; keys in device secure storage. **Auto models still burn Warp credits even with BYOK.** Free-tier BYOK for orgs ≤10 people | Free+ | **AHEAD** |
| **Custom endpoints** | Any OpenAI Chat Completions endpoint (also Responses + Anthropic Messages schemas since 2026-07-31). **Must be public HTTPS.** Not available to cloud agents. LiteLLM and OpenRouter named as intended targets | Free (≤10 ppl) / Business+ | **AHEAD** |
| Enterprise BYOLLM | AWS Bedrock, Vertex AI (Azure "coming soon") — routes into *your* cloud account, no Warp transit, no credits. **Cloud providers only; no self-hosted option** | Enterprise | **AHEAD** |
| Offline mode | **None.** First launch needs internet; AI features always need connectivity | — | **AHEAD** |

---

## 5. The software factory loop

Their canonical definition: *"an automation loop around the SDLC — **triage → spec → implement → review → verify → ship → monitor** — where at every step a mix of agents and humans moves the process forward."*

| Stage | Warp mechanism | gonk |
|---|---|---|
| **Triage** | Skill emits structured JSON (`state`, `label`, `remove_labels`, `comment`); **agent is read-only**, a deterministic `apply` job performs writes. Four states: `ready-to-implement` / `ready-to-spec` / `needs-info` / `wait-to-implement`. Inputs include `roadmap.md` + `vision.md` | **HAVE** (2 outcomes; roadmap 2.1–2.2 for the rest) |
| **Spec** | `specs/<slug>/PRODUCT.md` (numbered testable invariants, no implementation) + `TECH.md` (commit-pinned links, tests mapped to invariant numbers). **Human approval = "the highest-leverage checkpoint"** | **NONE** (roadmap 2.3–2.4) |
| **Implement** | Skill → branch → PR. Anti-lying guardrails; scope control; `Closes #N` vs `Related to #N`; commit trailer | **NONE** — the gap (roadmap Phase 1) |
| **Review** | `review.json` (`verdict` APPROVE/REJECT, `body`, `comments`), read-only agent, deterministic publish job. **Annotated diff** `[OLD:n]`/`[NEW:n]` as the *"only location source"*; validator enforces ≤50 comments, ≤10-line ranges, closed severity enum. **Non-blocking** | **NONE** (roadmap 3.1–3.2) |
| **Verify** | `verify-behavior` skill, two modes **`reproduce`** / **`verify`**; computer use via Xvfb in a headless container; **status derived from artifact existence, not agent assertion**; `blocked` first-class. **No attempt cap** | **FLIGHT** — `gonk-3jm`, `gonk-qhe`; computer-use is **WON'T** |
| **Ship** | Agents open PRs; **agents never merge**. Humans merge | **HAVE** (auto-merge is a permanent non-goal) |
| **Monitor** | Monitoring agent files issues, closing the loop | **NONE** (roadmap Phase 6) |
| **Self-improvement (outer loop)** | Daily/weekly scheduled agent reads runs + human corrections → **PR editing the skill file**. Bounded write surface (`## Learned guidelines`, 15-bullet cap), never-touch contract list, "no change" is valid, human merges | **NONE** (roadmap 4.8–4.12) |

**Their own honest status:** *"My view is that it's half-working"* — **1,300 issues stuck in `ready-to-implement`**. Published automation rate **20–30% of PRs end-to-end** (the "60%" marketing figure includes human-in-the-loop).

---

## 6. Governance, cost, observability

| Capability | Detail | Tier | gonk |
|---|---|---|---|
| **Spend caps** | Team-wide monthly caps (cloud/local/total) with approach-alerts and hard block. **Per-user caps Enterprise-only** | Paid / Enterprise | **AHEAD** — hard per-**project** ceilings at the LiteLLM door |
| **Cost attribution** | Per-user and per-team. **No per-project attribution** ([#12075](https://github.com/warpdotdev/warp/issues/12075) open) | Enterprise | **AHEAD by design** — atags; currently broken (`gonk-m6t`) |
| Credits | Combined **inference + compute** in one number. Per-conversation footer: credits, context consumed, tools invoked, model cost indicator | All | **PARTIAL** — Grafana only (roadmap 5.3) |
| Analytics API | `summary` / `users` / `events`. Deterministic acceptance schema: file changes and LOC **suggested vs accepted**, `was_edited_by_user`. Aggregates only, never conversation text | Enterprise | **NONE** — copy the schema |
| Run states | `QUEUED` / `PENDING` / `CLAIMED` / `INPROGRESS` / `SUCCEEDED` / `FAILED` / `BLOCKED` / `ERROR` / `CANCELLED` | — | **NONE** (roadmap 5.1) |
| Error taxonomy | **18 codes, split by fault: `FAILED` = caller's fault, `ERROR` = platform's fault**, with `retryable` carried on the wire. Includes `environment_setup_failed`, `agent_process_failed` (incl. OOM, non-retryable), **`infrastructure_timeout`** (a stale-task reaper — their *entire* runaway-loop backstop), `content_policy_violation`, `budget_exceeded`, `insufficient_credits`, `conflict`/`resource_unavailable`/`internal_error` (all retryable). RFC 7807 error bodies with `trace_id` | — | **NONE** — the fault split is the part to copy (roadmap 5.1) |
| Credits are **three buckets** | **AI** (inference), **compute** (sandbox; cloud runs only), **platform** (run lifecycle/orchestration/observability — charged on *every* cloud run regardless of harness or inference source). One run can draw from all three | — | **PARTIAL** — gonk meters USD only; GPU-seconds is the real local axis |
| Outbound webhooks | **None.** Warp never pushes events to you; you poll `GET /agent/runs/{id}` or watch the message bus | — | **AHEAD** — gonk is event-driven end to end |
| Documented rate limits | **None anywhere**, including the OpenAPI spec | — | n/a |
| Worker metrics | OTel catalog: connected gauge, active/max-concurrent, claimed/rejected(`reason`)/completed(`result`) counters, duration histogram, reconnects(`reason`). **All series pre-seeded at startup** | Enterprise | **PARTIAL** — service metrics, no agent-run metrics (roadmap 5.2) |
| Session sharing / steering | Join a live session; web viewer is the Warp app compiled to WASM | Paid | **WON'T** |
| Stuck-agent detection | **No semantic detector.** Only: `BLOCKED` state, `unschedulable_timeout` (30s), `activeDeadlineSeconds`, `--idle-on-complete` (45m), app-layer expiry | — | **PARTIAL** — reservation TTL |
| Secrets | Team/personal scope; **write-only** (never readable after creation), client-side encrypted, injected as env vars. Deleting breaks referencing schedules silently | Paid | **AHEAD** — file mounts 0400, no env vars, two rotation slots |
| Sandbox credentials | **Short-lived GitHub token scoped to the triggering user** — accepts exfiltration risk to buy attribution | — | **AHEAD** — zero forge creds in pod + broker |
| Network egress | **Unrestricted.** *"restricted network egress to only trusted domains"* listed as future work since Dec 2025 | — | **FLIGHT** — `gonk-7oz`, Cilium suspended (comparable gap) |
| Secret redaction | **Regex only, at render time, client-side.** Not applied to prompts or telemetry. Enterprise ask (redact whole output of `kubectl get secret`-shaped commands) **unbuilt** | Free+ | **NONE** — a control a self-hosted factory should own |
| Privacy / ZDR | Telemetry on by default, opt-out (Free requires it on for AI). ZDR enforced at Business+. Prompts + codebase context transit Warp's US-hosted proxy | Business+ | **AHEAD** — nothing leaves the LAN |
| SSO / SCIM / SOC 2 | SAML, Okta/Entra/Google, SCIM; SOC 2 Type II | Business/Enterprise | **WON'T** — one operator |

---

## 7. Terminal / client

Included for completeness; almost none of it is gonk-relevant.

| Capability | Detail | gonk |
|---|---|---|
| Rust + GPU (wgpu) terminal | Blocks model: typed command+output units carrying cwd, git state, **exit code**. Shell contract is small: `precmd`/`preexec` hooks emitting ANSI-wrapped JSON | **WON'T** |
| Third-party CLI agents | 14 auto-detected and "enhanced" incl. **opencode** and Claude Code — rich editor, vertical tabs, inline review comments | Optional personal cockpit |
| Blocks as agent context | Commands run during a conversation auto-included as context for the next query | **WON'T** |
| Warp Drive | Workflows (typed args, static or **dynamic** enum from a shell command), Notebooks, Prompts, Env vars (static or dynamic from 1Password/LastPass/Vault) | **WON'T** |
| Agent CLI (standalone) | tmux-like PTY indirection layer → persistent sessions across `cd`, **SSH to remote hosts with no remote binary**, drives sqlite/python/gdb/vim | Interesting for the rung interface |
| OS support | macOS, Linux, Windows (client + CLI) | n/a |

---

## 8. Pricing (2026-08-13)

| Tier | Price | Included | Notes |
|---|---|---|---|
| Free | $0 | **No included credits** (pay-as-you-go reload) | BYO inference allowed; limited cloud agents; login required for AI |
| Build | $20/mo ($18 annual) | 1,500 credits | Team spend cap, auto-reload |
| Max | $200/mo ($180) | 18,000 credits | 12× Build |
| Business | $50/user/mo ($45), ≤25 seats | 1,500/seat | SAML SSO, ZDR, team metrics, custom endpoints |
| Enterprise | Custom | Custom pools | Per-user caps, Analytics API, BYOLLM, SCIM, self-hosted execution |

**Volatility:** pricing changed **three times in ~14 months** (2025 request-limit overhaul → Oct 2025 credit re-base → 2026 tier restructure; free credits apparently removed in 2026). Warp's own words on the old overages: they *"felt like a rip-off."*

---

## 9. Documented gaps in Warp (opportunities, and honesty checks)

| Gap | Evidence |
|---|---|
| Local/private-network inference | #4339 (1,426 reactions), #12142, #11589. Architecture explicitly cited as the blocker, Jul 2026 |
| Offline mode | #5640 (111 reactions) |
| Per-project cost attribution | #12075 open; analytics is per-user/per-team only |
| Restricted egress from sandboxes | Listed as future work since Dec 2025 |
| Org-enforced command-output secret redaction | Unbuilt; existing redaction is regex, single-line, render-time |
| Per-worktree LSP + worktree lifecycle | On the roadmap, unbuilt |
| Eval methodology for self-improvement loops | Promised in a Jul 2026 post; **never published**. No accuracy metric for any loop |
| Attempt cap on iterate-until-verified | None; only a cost warning |
| Plaintext config export | #3447 (200 reactions) |
| Published scaling post-mortems | **None exist.** The "2M agents" post is a traction announcement about *local desktop* agents |
| Metric inconsistency | "60% of PRs agent-created" (marketing) vs "20–30% automated end-to-end" (articles) — same company, same period |
| 2024 → 2026 reversal | In 2024 they published that sandboxed, minimally-supervised agents were a *misconception*; in 2026 they sell exactly that, unremarked |

---

## 9b. Mechanisms a self-hosted competitor wouldn't think to build

From a full sweep of all 363 doc pages plus the OpenAPI spec (21 paths, 28 operations, 85 schemas). Ranked by relevance to gonk; these are the non-obvious design choices, not the feature list.

1. **Harness as a run-config *field*, not a deployment.** `harness: oz | claude | codex`, with per-harness auth secrets and a dedicated endpoint to fetch the *native* third-party transcript separately from the normalized one. gonk already runs opencode pods; making the harness a field rather than a fork is the cheap generalization of the rung seam (roadmap 5.6).
2. **A durable inter-agent mailbox as a first-class primitive.** `oz run message list|read|watch --since-sequence N|send --to|mark-delivered`, addressed by agent ID, harness-agnostic, cross-location. Two non-obvious properties: **a child in a terminal state is still addressable and wakes on a new message**, and **messages and lifecycle events share a global sequence number** so a parent can never observe `SUCCEEDED` before the message that produced it.
3. **Per-run federated identity.** `oz federate issue-token`, callable **only from inside a running agent**, mints a short-lived OIDC JWT with a composable `--subject-template` over `principal, scoped_principal, email, teams, environment, agent_name, skill_spec, run_id, host`. You can write an IAM policy scoped to "this exact skill, as this team, in this environment." No static cloud credentials anywhere. Conceptually adjacent to gonk's ed25519 grant model, and a good pattern if gonk ever needs cloud resources.
4. **Declarative end-of-run workspace snapshots.** A pluggable script emits JSONL `{kind: repo|file, path}`; **repos are diffed, not copied**; per-run output path so concurrent runs can't clobber; malformed lines skipped rather than fatal; best-effort so a snapshot failure never fails the run.
5. **Denylist ≥ allowlist ≥ autonomy, as a documented invariant, with a two-tier trust boundary.** "Run until completion" bypasses the *user's* denylist by default (with a named opt-out setting), but **admin denylist rules are structurally non-bypassable under any setting**. Most homegrown guardrails have one tier and ambiguous precedence. Their shipped defaults are instructive: allow `cat`/`echo`/`find`/`grep`/`ls`/`which`; deny `bash`/`sh`/`zsh`/`curl`/`wget`/`eval`/`exec`/`source`/`ssh`/`scp`/`rsync`/`rm`/`dig`. Note the footgun they document: **setting a denylist replaces rather than merges the defaults.**
6. **Three-layer secret scoping with an explicit empty-list opt-out.** Owner scope → environment-attached *names* (so rotation is free) → per-run list, where an empty list means zero injection. And the rule gonk's broker should adopt: **personal secrets are never injected into user-less triggers** (schedules, automated integrations) — only team-scoped ones.
7. **Approval gating on MCP *config file edits*, separate from tool-call approval.** The dangerous moment is a cloned repo silently registering a command-executing server, not calling an existing one.
8. **`--idle-on-complete` (default 45m).** The worker keeps the agent process alive after the conversation ends so a human can attach and send a follow-up. Fire-and-forget pod designs make this impossible — worth knowing before gonk hard-codes pod teardown.
9. **Fleet and environment health as queryable metadata**: `GET /agent/connected-self-hosted-workers` (live heartbeat roster), plus `setup_failed` and `last_task_created` on environments — so you find broken environments proactively rather than at run time.
10. **Runner separated from environment.** "What the agent works on" and "what hardware it runs on" are distinct objects, overridable per run and per orchestration child.
11. **Cancelling a parent deliberately does *not* cancel children**, and they say why; self-hosted/local/Action runs return 422 on cancel because Warp doesn't own their lifecycle. An honest boundary, explicitly documented.
12. **`RunSourceType` as a queryable enum** (`LINEAR, API, SLACK, LOCAL, SCHEDULED_AGENT, WEB_APP, GITHUB_ACTION, CLOUD_MODE, CLI`), with **separate `creator` and `executor`** fields — modeling "a human delegated this to an agent running as a different principal."
13. **Analytics `credit_charged` is an apportioned per-message share** of a multi-message LLM request — real chargeback accounting rather than request-level cost. Directly relevant to gonk's per-bead attribution (`gonk-m6t`).
14. **PR artifact attachment is a tri-state** (Disabled / Link only / Embed) precisely because Embed makes agent screenshots downloadable by anyone who can read the PR.
15. **Computer-use recordings are post-processed before leaving the sandbox** — idle time cut, actions burned in as overlays, **typed text masked as "typing…"** so secrets don't end up on camera.
16. **Dynamic environment variables store the *retrieval command*, never the value** (`vault kv get …` is the stored object).
17. **`osc52_clipboard_access` defaults to `deny`** — the escape sequence letting a sandboxed process write your clipboard is treated as a three-level security boundary.
18. **OSC 9 / OSC 777 as a zero-dependency notification channel** — two `printf`s from any job produce a native desktop notification. A free "agent needs you" signal for any harness.
19. **Orchestration notifications fire only on the parent**; children appear in a pill bar. Deliberate anti-fatigue design for fan-out.
20. **Git worktrees as the documented parallel-agent isolation strategy**, with per-worktree index and review panel — a real alternative to container-per-job, and cheaper.

**A rules-file constraint worth internalizing:** their docs state bluntly that the *entire* rules file is prepended to every prompt, so keep it under ~500 lines. With gonk at 16K context, the equivalent budget is far tighter — which is the argument for skills' progressive disclosure (name+description at start, body on trigger, references on demand) rather than one large always-on instruction file.

---

## 10. Where gonk is ahead

Worth stating plainly, because it's easy to lose in a parity exercise:

1. **Hard per-project budget ceilings** enforced at the model door via LiteLLM virtual keys + synthetic pricing. Warp caps per team (per user only at Enterprise), inside their billing system.
   ⚠️ **Architecturally true; operationally true today only for cloud rungs.** Verified 2026-08-13: LiteLLM's `model_cost_map` contains two entries, both `qwen3-14b`, both priced at **zero**, while the HelmRelease declares synthetic prices gonk-side that were never copied across. So the USD ceiling does not close on any *local* rung — a second, independent gap from the Ollama network bypass, since even correctly-routed traffic is unpriced. gonk-meter's own token ceilings still bind. Filed as `gonk-1zm`; the claim above is restored once that closes.
2. **Per-project cost attribution** — an open feature request at Warp.
3. **Zero forge credentials in the agent pod.** Warp injects a short-lived scoped token; gonk's broker means there is nothing to exfiltrate.
4. **Deterministic classification** — `pkg/gate.Classify` is pure and total with a regression test. Warp's `verdict` is model-produced; only the vocabulary is checked.
5. **Local-first inference with no vendor in the path**, which Warp's own architecture cannot offer today.
6. **Secrets as file mounts** (0400, no env vars, two rotation slots, chart creates no Secret objects).
7. **Both control and execution planes self-owned.** Warp's self-hosting splits them: execution yours, orchestration theirs.
8. **GitLab as a native forge.** Warp has no GitLab integration; the documented path is a clone-only token.

---

## Changelog

| Date | Change |
|---|---|
| 2026-08-13 | Initial snapshot. Twelve parallel research passes over warp.dev/blog (133 posts), docs.warp.dev, the GitHub org, and the public demo repos. Key state: client AGPL open-source since Apr 2026, server/harness/Oz proprietary; all inference proxied through Warp; local harness announced May 2026 and walked back Jul 2026. |
| 2026-08-13 | Added §9b from a complete docs sweep — all **363** doc URLs plus `openapi.json` (21 paths / 28 operations / 85 schemas). Nothing was unfetchable; the only gap is *inside* the docs, which never state prices, seat limits, or credit allowances (they defer to the pricing page). Corrections folded in: run states are nine not six; the error taxonomy is **18 codes split by fault** (`FAILED` = caller, `ERROR` = platform) with `retryable` on the wire; credits are **three independent buckets** (AI / compute / platform); there are **no outbound webhooks and no documented rate limits**. |

### Re-survey checklist

Work top-down; §0 is the part that can change the adoption verdict.

1. **§0 architectural facts** — has a client-side harness or ACP shipped? Check for an `agent-client-protocol` crate in any `Cargo.toml` and for a `specs/GH7326/` directory in `warpdotdev/warp`. Either appearing is the signal.
2. Has the server or Oz been open-sourced?
3. Do custom endpoints still reject private IPs? (Test the docs statement, not the marketing page.)
4. New tiers, credit changes, or feature re-gating (§8) — the history says assume drift.
5. New capabilities in §1–§6; flip any gonk column that changed.
6. Re-check §9 gaps: any closed? Any that gonk has since closed first?
7. Re-read the top issues by reactions for demand shifts.
8. Append a changelog row; update the snapshot date and next-due date at the top.
