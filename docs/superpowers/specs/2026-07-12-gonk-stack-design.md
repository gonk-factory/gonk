# The Gonk Stack: A Local Software Factory for Self-Hosted GitLab

- **Status:** Draft for review
- **Date:** 2026-07-12
- **Owner:** Steve (leftathome@gmail.com)
- **Intended license:** MIT (open source after validation)

## 1. Summary

Gonk is a self-hosted "software factory" that lets a Gas City orchestrator maintain and
contribute to projects on a self-hosted GitLab instance as a bot user, using opencode
agent sessions running in Kubernetes pods, with all LLM traffic routed through LiteLLM
so that token usage and cost are attributed, budgeted, and enforced per project, per
group, and per instance. Local inference (vLLM on DGX-class hardware) is the default;
cloud models are opt-in rungs on a deterministic escalation ladder with hard budgets.

Design philosophy: gonk is **a Gas City pack plus glue**, not a new orchestrator.
Gas City provides the controller, bead store, event bus, webhook receivers, orders,
session providers (including native k8s), and pack composition. Gonk provides the
GitLab integration, the opencode agent images, the budget/attribution brain, the
Navigator-style project onboarding contract, and a Helm chart that assembles it all.

## 2. Goals and non-goals

### Goals

1. Deploy via a single Helm chart onto an existing cluster; integrate with an existing
   self-hosted GitLab via one bot user.
2. Per-project opt-in ("tagging in") with Renovate-style onboarding: inviting the bot
   yields a deterministic onboarding MR; merging it enables gonk for that project.
3. v1 capability: **issue triage** (labels, analysis comments, clarifying questions,
   conversational follow-up in-thread) plus the deterministic onboarding MR flow
   and the post-merge `.agent/` scaffold MR (the first metered work; triage cannot
   run while a project is `pending`, so the scaffold MR is in v1 scope).
4. Token usage and cost attributable at every granularity: per turn, per
   comment-or-commit, per work item (bead), per branch, per project, per group,
   per instance. Budgets settable at project/group/instance; hard-enforced.
5. Local-first inference with a deterministic (zero-LLM-judge) escalation ladder to
   larger/cloud models, and "defer until capacity/budget" semantics.
6. Project state lives in the project (`.gonk.yml` + `.agent/`), not in gonk
   (the Renovate model). Gonk-side per-project state is derived cache only.
7. Trustworthy by construction: deterministic e2e tests with a stub model server,
   contract-tested schemas, full observability through existing Prometheus/Grafana/
   Loki/otel stacks, append-only audit via the gc event bus and bead history.
8. Open-sourceable: no deployment-specific material in the product repos;
   bring-your-own-infrastructure seams in the chart.

### Non-goals (v1)

- Pipeline fixing, feature-decomposition execution (design docs to task beads),
  and the faithful Navigator plugin port. These are v2+; v1 lays their rails.
- Cluster-event ingestion (reacting to pod crashes, Flux failures). The seam is
  "another webhook plus rules file"; explicitly out of v1.
- Multi-harness rungs (Claude Code as an escalation target). The rung interface
  accommodates it; v1 ships model rungs only, all via opencode.
- Local-queue fairness scheduling. v1 meters local throughput for visibility only.
- Auto-merge of anything, ever, in any version.

## 3. Context and prior decisions

- Cluster: k3s-style homelab, Flux GitOps, Longhorn storage, Prometheus + Grafana +
  Loki + otel, self-hosted GitLab (CE assumed; see 7.2), GitLab CI runners on-cluster.
- Inference: bailey.orac.local (DGX Spark, 128GB) running vLLM as primary local
  endpoint; LiteLLM proxy planned as the abstraction layer; qwen3-family models
  evaluated for agent work; cloud spillover candidates: GLM-5.x, Anthropic models.
- Upstream: Gas City (github.com/gastownhall/gascity, MIT) with sibling repos
  gascity-packs (NO top-level license; see 12.1), beads (MIT, Dolt-backed),
  gastown (MIT), wasteland (MIT), gascity-otel (Apache-2.0).
- Upstream PRs to Gas City are permitted where seams are missing.

Decisions settled during design review:

| Decision | Choice |
|---|---|
| v1 walking skeleton | Issue triage only, plus deterministic onboarding MR |
| Cost ledger | Hybrid: LiteLLM enforces hard budgets; gonk-meter derives attribution and owns policy |
| Agent runtime | opencode only in v1; rung interface leaves seam for other harnesses |
| Escalation | Deterministic outcome-gated ladder; no LLM judges; no upfront complexity classification |
| Navigator | Staged: v1 conventions + thin loader + consistency gate; v2 faithful opencode plugin port |
| Architecture | Pack-first: gonk is a Gas City pack + chart + services, not a bridge or custom orchestrator |
| Repo layout | `gonk` monorepo + `gonk-navigator` repo + private `gonk-city` instance repo |
| Onboarding | Renovate-style: bot invite triggers deterministic onboarding MR carrying `.gonk.yml` |

## 4. Architecture

### 4.1 Components (all in one namespace, deployed by the chart)

| Component | Kind | Role |
|---|---|---|
| gascity controller | Deployment | The city: reconcile loop, supervisor REST API + SSE event bus, order evaluation, k8s session provider |
| dolt sql-server | StatefulSet | The shared bead store (beads server mode). Tier-0 stateful backbone: PDB, probes, scheduled `dolt push` backups to a remote |
| gitlab-intake | pack `[[service]]` (runs as its own Deployment) | Webhook receipt + `X-Gitlab-Token` verification, event-to-order/conversation routing, bot-authored-event suppression, membership reconciliation, `.gonk.yml` discovery/validation, rig auto-registration, webhook provisioning |
| opencode session pods | Pods (created by k8s session provider) | One per active agent session; agent image = opencode + git + glab + bd; workspace = project rig clone |
| LiteLLM proxy | Deployment (+ Postgres) | Single door to all models (bailey vLLM + cloud). Virtual key per project; hard budget/rate enforcement; tagged spend logs; otel |
| gonk-meter | Deployment | Ledger joins (spend log x event bus), attribution metrics/API, virtual-key provisioning, rung policy decisions (see 6) |
| Ingress | Ingress | Webhook endpoint(s) only; everything else cluster-internal |

### 4.2 State model

- **Bead store:** one shared Dolt database for the city and all rigs; isolation by
  bead-ID prefix. Dolt server mode for concurrent writers.
- **Rigs:** passive registrations (path, branch, prefix). No standing processes.
  Clones are created lazily at first session and auto-reaped.
- **Agent pools scale to zero.** `min_active_sessions = 0` everywhere; each
  controller tick sizes pools to demand via `scale_check`; idle sessions are retired.
  An idle gonk installation runs only the platform services above.
- **Session continuity:** durable, layered. (1) live session; (2) provider resume:
  `wake_mode = "resume"` re-launches a pod with the same `{{.SessionKey}}`, requiring
  opencode session state on durable storage (PVC); (3) reconstruction from bead
  history + the GitLab thread + `.agent/` (always available). Correctness never
  depends on LLM context surviving. Per-project `continuity: resume | fresh`.
- **City config is a git repo** (`gonk-city`, private): platform config only —
  pack imports at pins, providers/upstreams, orders, LiteLLM model list, budget
  values, secret pointers. Flux syncs it; the chart mounts it. Onboarded projects
  never require commits here (rig registration is derived cache, auto-managed).

### 4.3 v1 data flow (issue triage)

1. GitLab fires an issue webhook to `/hook/gitlab` on the supervisor edge.
2. gitlab-intake verifies the token, drops bot-authored events, checks the project
   is onboarded (valid `.gonk.yml`), and dispatches per its rules:
   new work -> fire order; follow-up on a known thread -> route to conversation.
3. The order creates a bead (anchored to project + issue IID) and slings the triage
   formula to the triage pool.
4. The controller's k8s session provider spawns an opencode pod. Session env carries
   the project's LiteLLM virtual key and attribution tags (project, rig, bead_id,
   session_key, rung, attempt, trigger).
5. The session loads `.agent/` context (thin loader), reads the issue, posts
   labels/comments via the GitLab API as the bot user.
6. Gate step evaluates deterministic outcome signals; bead closes on success or
   re-slings per the ladder (see 6.3).
7. Follow-up user replies route to the same conversation; the controller wakes or
   resumes the worker (subject to `max_wakes_per_tick`).
8. gonk-meter joins spend logs to beads and GitLab artifacts; Prometheus metrics
   and cost API update; Grafana dashboards reflect it.

### 4.4 Upstream gaps to close (candidate Gas City PRs)

1. **`gitlab-token` webhook verification scheme** — GitLab authenticates webhooks
   with a constant `X-Gitlab-Token` header, not an HMAC signature. Either an
   upstream verify scheme, or verification handled entirely inside gitlab-intake
   (fallback that requires no upstream change; chosen default until PR lands).
2. **Opencode/ACP session resume across pod recreation** — named v1 validation
   milestone (see 11). If resume via the k8s provider has gaps, upstream PR or
   `continuity: fresh` fallback.

## 5. GitLab integration and onboarding

### 5.1 Bot identity and roles

One dedicated GitLab bot user. PAT stored in Vault/1Password, synced to the cluster
as a secret (existingSecret ref). "Tagged in" = bot is a member AND repo contains a
valid `.gonk.yml`; either alone is inert. De-onboard by removing either.

Role tiers (documented, enforced by feature): Reporter = triage only (comment,
label, read). Developer = branch pushes + MRs (onboarding MR, v2 pipeline fixes).
Maintainer = webhook self-provisioning. Chart modes: `bot-does-everything`
(Maintainer; homelab default) or `split-credential` (low-privilege bot + admin
provisioning token used only by intake for webhook management).

### 5.2 Discovery and reconciliation

Reconciliation is the correctness path; webhooks are the latency optimization.
Every N minutes (and on demand) intake lists the bot's project memberships
(`membership=true`), fetches and validates `.gonk.yml`, idempotently provisions or
repairs the project webhook, and auto-registers the rig if missing. An optional
instance-level system hook (`user_add_to_team`) makes invite detection instant;
system hooks do not carry issue events, and group webhooks are GitLab Premium,
so per-project webhooks are the CE-compatible event path.

### 5.3 Onboarding MR (Renovate-style, deterministic, zero tokens)

On detecting bot membership without `.gonk.yml`: push branch `gonk/onboard`, open
an MR assigned to maintainers containing (a) a conservative default `.gonk.yml`
(triage-only, local rungs only, zero cloud budget) and (b) a rendered explanation
of what gonk is, every configurable key, and the exact consequences of merging,
re-rendered from the actual config values so text cannot drift from settings.
Closing unmerged = declined; recorded; not re-opened unless re-invited. If the bot
lacks Developer, it opens an issue stating the role it needs instead.

The `.agent/` scaffold is a second, post-merge MR, generated by an agent session
that reads the repo — the first metered work, authorized by the just-merged config.
Until `.agent/` exists, the project is `pending` (visible in metrics; no LLM actions
except the scaffold MR itself).

### 5.4 `.gonk.yml` (policy contract; JSON Schema published and versioned)

```yaml
version: 1                  # schema version; contract tests gate breaking changes
enabled: true
actions: { triage: true, pipelines: false, features: false }
schedule: { quiet_hours: "22:00-07:00", timezone: "America/New_York" }
budget: { monthly_cost_usd: 0, monthly_tokens: "50M", per_task_tokens: "2M" }
ladder: [qwen-local]        # allowed rungs, in order; cloud rungs must be listed
continuity: resume          # resume | fresh
triage: { label_prefix: "gonk::", respond_to_mentions: true }
provenance: { commit_trailers: true, include_usage: false }
```

Token quantities are strings with a defined suffix grammar (`K`/`M`/`G`), decided
deliberately in the schema so YAML numeric parsing ambiguity cannot bite.

Precedence: instance defaults (chart values) -> group overrides (gonk-city) ->
project `.gonk.yml`; most specific wins, except budget ceilings only tighten
downward (a project can spend less than its group allows, never more).

### 5.5 Conversational surface

`@<bot>` mentions in issue/MR comments route via the conversation sink to the
thread's worker session (wake/resume). This is also the v1 escalation valve for
humans: ask the bot to re-triage or explain itself in-thread.

## 6. Budgets, attribution, and the escalation ladder

### 6.1 Attribution chain

Agent pods talk only to LiteLLM, using a per-project virtual key, with per-request
metadata tags: `project`, `rig`, `bead_id`, `session_key`, `rung`, `attempt`,
`trigger`. LiteLLM spend logs are the raw ledger (tokens + computed cost per call,
keyed by all tags).

**Commit provenance trailers (default on):** every bot-authored commit carries
git trailers identifying the generator, e.g.
`Generated-By: gonk/<version> (opencode <version>; <model> via litellm)`.
With `provenance.include_usage: true` (default off — cost in public history is a
per-project choice), trailers also carry `Gonk-Tokens:` and `Gonk-Cost-USD:`
for the producing session, sourced from gonk-meter at commit time. Controlled by
`provenance` in `.gonk.yml`.

### 6.2 gonk-meter responsibilities

1. **Ledger joins:** tail LiteLLM spend logs + gc event bus (SSE); join spend ->
   bead -> GitLab artifact. Export Prometheus metrics and a query API
   (`/cost/bead/{id}`, `/cost/project/{id}`, ...). (Per-comment cost footers are
   v1.5, per the roadmap; the API fields they need exist from v1.)
2. **Enforcement delegation:** meter provisions per-project virtual keys via the
   LiteLLM admin API at onboarding and sets budgets/rate limits from resolved
   config. Hard refusal happens in LiteLLM, at the only door. Meter is never in
   the request path.
3. **Policy (the wait-vs-spend brain):** before session spawn, the dispatch formula
   asks meter for the rung: resolve ladder from `.gonk.yml` + group/instance
   ceilings, check remaining budget, return rung or `defer` (bead parked in
   `waiting-for-capacity`; retried on month rollover or freed capacity).
   Deterministic inputs only: config, spend totals, attempt count.
4. **Ladder state:** rung/attempt recorded on the bead.

### 6.3 The ladder (no LLM judges)

Everything starts at the cheapest rung its project config allows; escalation is
earned by failing objective gates. Gate signals (v1 triage): session completed and
posted required artifacts within turn/token caps. (v2 pipelines: patch applies,
CI green.) Gate classification is strict about infra vs quality: connection
errors, LiteLLM 5xx, pod evictions -> retry same rung with backoff; only
completed-but-failed-gate outcomes escalate. Rungs are model-shaped in v1
(qwen-local -> glm -> sonnet -> ...), all via opencode + LiteLLM model param;
harness-shaped rungs (Claude Code) are a later rung type behind the same
"spawn attempt at rung R" interface. Local rungs are cost-0 dollars but metered
in tokens for fairness visibility; `defer` applies when only-cloud-rungs-remain
and budget is exhausted.

## 7. Repository map

### 7.1 `gonk` monorepo (open source, MIT)

```
docs/     architecture, ADRs, schema contracts (.gonk.yml, attribution tags,
          order names), PLAN.md progress file
pack/     Gas City pack: pack.toml, agents/ (triage), formulas/, orders/,
          webhook rule templates, doctor/ checks, overlay/ (opencode config),
          template-fragments/
intake/   gitlab-intake (Go)
meter/    gonk-meter (Go)
images/   Dockerfiles: agent (opencode+git+glab+bd), controller (gc+bd+pack),
          intake, meter
chart/    Helm chart (see 7.3)
test/     kind e2e harness: real gitlab-ce + full chart + stub model server
```

Monorepo rationale: the `.gonk.yml` schema, attribution tags, pack order names,
and chart wiring evolve together; one repo makes those changes atomic with one CI
pipeline and one plan file. CI still publishes independent artifacts: images to
registry, chart as OCI, pack consumed by `git-source//pack#ref` pin.

### 7.2 `gonk-navigator` (open source, separate repo)

`.agent/` conventions schema; `nav init` scaffolder; `nav lint` consistency checker
(used by pack doctor checks, as an includable GitLab CI job template, and as the
post-session gate); thin opencode context-loader plugin. v2: faithful Navigator
plugin port (own npm release cadence, audience beyond gonk).

### 7.3 `gonk-city` (private instance repo)

The deployed city: city.toml / root pack.toml importing `gonk//pack` at a pin,
provider/upstream definitions (bailey, cloud), LiteLLM model list, budget values,
secret pointers only. The line between product and deployment.

### 7.4 Chart bring-your-own-infrastructure seams

- Postgres: `postgresql.mode: bundled | cnpg | external` (CNPG emits a Cluster CR).
- Monitoring: never installs Prometheus/Grafana; optional ServiceMonitor/PodMonitor
  CRs, Grafana dashboards as label-discovered ConfigMaps, OTLP endpoint value.
- LiteLLM: `litellm.enabled: false` + `externalUrl` + admin-key secret ref
  supported.
- Dolt: bundled StatefulSet default; external endpoint option.
- Ingress: standard ingress-class config.
- Secrets: existingSecret refs everywhere; no secret material in values.
- GitLab edition: CE-compatible paths only (per-project webhooks); optional
  system-hook enhancement documented for instance admins.

## 8. Observability

- Prometheus metrics via ServiceMonitors from every component. Key series:
  intake webhook receipt/verify/drop and reconciliation results; project config
  states (valid/pending/invalid/declined); orders fired; sessions
  spawned/resumed/retired; gate outcomes; ladder escalations; deferred beads;
  spend by project/rung/trigger; budget-remaining gauges; local token throughput.
- Three shipped Grafana dashboards: Factory (work in flight, outcomes, queue age),
  Cost (project/group/rung, local vs cloud), Health (components, dolt, LiteLLM).
- OTLP traces stitched webhook -> order -> session attempt -> LiteLLM calls.
- Audit: append-only gc event bus + per-bead history; Loki labels carry bead_id.

## 9. Security posture

- Agent pods: NetworkPolicy allows egress only to GitLab and LiteLLM. No direct
  internet; cloud access exists only through the metered proxy, so budgets cannot
  be bypassed.
- Bot writes limited to `gonk/*` branches, comments, labels (v1). No auto-merge.
  Protected branches untouched.
- All credentials via existingSecret refs; PAT scopes documented per role tier;
  webhook shared secrets rotated via secret_key rotation slots.
- Supervisor API not exposed beyond the namespace; webhook path is the only
  ingress. (Gas City's read/write grant gating available for hardening later.)

## 10. Testing and CI

1. **Contract tests:** JSON Schemas for `.gonk.yml`, attribution tags, order
   names; breaking change fails CI without version bump. Schemas double as docs.
2. **Unit (Go):** go vet + golangci-lint; table-driven tests. Meter's rung
   decision is a pure function (config + spend + attempt -> rung/defer):
   exhaustively tested with zero infrastructure. Intake tested against httptest
   fake GitLab with golden webhook payloads captured from real GitLab versions
   and refreshed on GitLab upgrades.
3. **Component:** pack TOML validation and prompt render tests; helm lint +
   helm unittest + kubeconform; image builds smoke-tested and trivy-scanned.
4. **E2E (deterministic, every merge):** kind + real gitlab-ce container + full
   chart + stub OpenAI-compatible model server behind LiteLLM. Scenario: invite
   bot -> onboarding MR -> merge -> file issue -> triage comment lands -> spend
   attribution correct. Zero real tokens, zero model flakiness.
5. **E2E (live, opt-in nightly):** same scenario against bailey.
6. Containers are rebuilt to deliver changes (never copied into); tests that
   touch databases/APIs re-run when schemas change (house rules).

## 11. v1 validation milestones (walking skeleton exit criteria)

1. Helm install on a kind cluster and on the real cluster; idle state = platform
   services only, zero agent pods.
2. Onboarding MR flow end-to-end against real GitLab (deterministic path).
3. Triage flow end-to-end with stub model, then with bailey.
4. **Session resume across pod recreation with opencode/ACP under the k8s
   provider** — named risk; if broken, file upstream PR or fall back to
   `continuity: fresh` (correctness preserved via layered continuity).
5. Spend attribution visible in Grafana at bead/project/instance granularity;
   budget exhaustion demonstrably blocks cloud rungs and produces `defer`.
6. Kill tests: dolt restart, LiteLLM outage, GitLab restart mid-flow — no lost
   work items, no duplicate comments, no rung escalation from infra failures.

## 12. Risks and open questions

1. **gascity-packs licensing:** repo has no top-level LICENSE. We derive patterns
   (not code) from the github pack; confirm a grant with maintainers before
   open-sourcing anything derivative, or clean-room the pack (feasible: config +
   prompts). Gas City core, beads, gastown, wasteland are MIT.
2. **GitLab edition assumptions:** design targets CE (per-project webhooks).
   Premium features (group webhooks) are optional enhancements only.
3. **Opencode/ACP resume fidelity in k8s** — see 11.4.
4. **Pack `[[service]]` deployment shape:** upstream runs pack services under the
   controller; we prefer intake as its own Deployment. Validate the pack-service
   contract supports external execution, else run intake standalone and keep the
   pack's service entry as documentation.
5. **Local capacity contention:** multiple projects sharing bailey. v1 ships
   visibility only; fairness scheduling deferred until real contention observed.
6. **Webhook payload drift across GitLab upgrades:** mitigated by golden-payload
   refresh discipline tied to GitLab upgrades.

## 13. Roadmap after v1

- v1.5: bot-generated `.agent/` scaffold MR quality pass; per-comment cost footers;
  additional triage skills (duplicate detection, label taxonomy learning from repo
  history — still gate-checked, no judges).
- v2: pipeline fixing (failed-pipeline webhook -> analyze logs -> fix branch + MR
  with CI-green gate); harness-shaped ladder rungs (Claude Code); faithful
  Navigator plugin port; group-level dashboards.
- v3: feature-decomposition (design doc -> epic bead -> task beads -> execution);
  cluster-event ingestion; wasteland federation experiments.

## Appendix A: Bot name

**Decided: `gonk`** (GitLab username `gonk`, mentions `@gonk`, label prefix
`gonk::`, onboarding branch `gonk/onboard`). Other candidates considered:
plugbot, dinky, blackthumb, organic-mechanic, sprocket, vulcan, cogsworth,
stakhanov, magrat.
