# Plan 03: gonk-meter (rung policy, key provisioning, ledger)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `cmd/gonk-meter`: the wait-vs-spend brain. It resolves per-project policy, provisions per-project LiteLLM virtual keys, decides the rung (or `defer`) for every session attempt deterministically, joins LiteLLM spend logs to beads/sessions via `pkg/atags`, and exports budget metrics and a cost API.

**Architecture:** The money logic is a set of **pure functions** in `pkg/` — `budget` (JSON-safe limits + remaining math), `spend` (month windows + spend aggregation), `opercfg` (validated operator config), `rung` (`Decide(Input) Decision`). They take config + spend + attempt history and return a decision; they touch no network, no clock (the clock is an input), and no database. Everything that needs infrastructure — the LiteLLM admin API, the spend-log source, the key sink, the HTTP server, the store — sits behind interfaces in `internal/meter/` with in-memory fakes, so the entire plan is testable with `go test ./...` and zero live LiteLLM, zero live GitLab, zero Kubernetes. Live-infrastructure verification is Plan 06's job and is flagged, not smuggled in.

**Tech Stack:** Go 1.26, stdlib `net/http` (Go 1.22+ method+wildcard `ServeMux` patterns), `github.com/prometheus/client_golang`, existing `pkg/gonkcfg` + `pkg/atags`. No new runtime dependency beyond the Prometheus client.

**Spec:** `docs/superpowers/specs/2026-07-12-gonk-stack-design.md` sections 6 (budgets, attribution, ladder), 5.4 (config precedence), 8 (observability), 10.1 (contract change rules).
**Contract inputs:** `docs/adr/ADR-002-config-precedence-semantics.md` (precedence is **published and settled** — this plan *consumes* `gonkcfg.Resolve`; it must not redefine precedence, budget folding, or `DisabledReason`).

**Environment:** `docs/environment.md` — read off the live cluster, and **authoritative** for every fact about the deployment target. What binds this plan:

- **LiteLLM is bring-your-own and ALREADY DEPLOYED**: namespace `litellm`, Service `litellm:4000`, i.e. `http://litellm.litellm.svc.cluster.local:4000` (plain HTTP, in-cluster — no CA involved on that hop). The chart does **not** bundle it. `LITELLM_URL` points here.
- **LiteLLM's model list *and* its prices live in ONE file in the gitops repo:** `clusters/orac/apps/litellm/litellm.yaml`, under `spec.values.proxy_config.model_list` and `spec.values.litellm_settings.model_cost_map`. **Local models are priced at ZERO there today.** That is exactly the hole Decision 9 exists to close, and that map is where the synthetic prices go. See "Synthetic pricing" below — including the one genuine ambiguity, which is flagged rather than assumed.
- **The ledger fallback is real and pre-approved:** Postgres on the existing CNPG cluster (`databases-app/postgres`, healthy, 2 instances). Task 0b decides; **`cmd/gonk-meter` must be able to run against either backend**, which is why `GONK_METER_STORE_BACKEND` exists (Task 8).
- **Every GitLab runner is offline; CI has never executed for this repo.** The standing gate below is the *only* gate. The first `gonk-meter` image is built and pushed **by hand** to `registry.orac.local/agentic/gonk-project/gonk-meter:<tag>`.
- **NetworkPolicies are NOT enforced on this cluster** (Flannel does not implement them; the Cilium HelmRelease is suspended). This has a direct, load-bearing consequence for what this plan may claim — see "What the hard door actually covers" below. Read it before you write the word "cannot" anywhere near the word "budget".

**Standing verification gate (from Plan 01, run before every commit):**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
```

`gofmt -l .` must print nothing. CI pins `golang:1.26` and `golangci-lint:v2.12.2`; no GitLab runner was online during Plan 01, so **the local gate is the real gate**.

---

## Decisions this plan makes (and why)

These are recorded so the executor does not re-litigate them mid-task. Each is written into `docs/adr/ADR-004-rung-policy-and-budget-enforcement.md` in Task 10.

1. **The decision is three-valued: `run` | `defer` | `deny`.** Spec 6.2 says "return rung or `defer`". `defer` means *not now, retry at `retry_after`* (budget exhausted, quiet hours, stale spend, key not yet provisioned). But a project that is disabled, has an invalid config, or has exhausted its ladder will **never** succeed on retry — parking it in `waiting-for-capacity` forever is a bug, not a policy. `deny` is that outcome. This is an extension of spec 6.2, not a contradiction: the spec never says what happens to a disabled project, and the alternative is an infinite retry loop.
2. **Meter owns ladder state.** `/v1/policy/decide` does **not** accept an attempt count or prior-outcome list from the caller. It reads them from its own store, written by `/v1/policy/outcome`. A caller-supplied attempt number is a forgery vector (raise it and you skip straight to the expensive rung) and a skew vector. Spec 6.2.4 says rung/attempt are "recorded on the bead" — the pack still writes them there for humans and for bead history; meter's copy is the authority for the decision. **`meterapi.DecideRequest` has no `attempt` field and never will; Plan 02 and Plan 04 must not add one.**
3. **Escalation is counted from gate failures only.** `attempt` increments on every attempt; the **rung index** is `count(prior attempts whose outcome == gate-failed)`. An infra failure (connection error, LiteLLM 5xx, pod eviction, reservation expiry) retries the *same* rung and never advances the index (spec 6.3). Consecutive infra failures at one rung are capped (`max_infra_retries`, default 5) and then `deny`.
4. **Reservations close the concurrency and spend-lag holes.** A `run` decision writes a reservation (estimated cost + tokens for that rung) into the store *inside the same per-project critical section that read the spend totals*. Remaining budget is always `ceiling - (observed spend + open reservations)`. A reservation is released by `/v1/policy/outcome`, or expires after `reservation_ttl` (default 60m) — and an expired reservation is recorded as an `infra-failed` attempt, so a session that dies silently retries its rung instead of escalating. **`/decide` is idempotent on an open reservation:** because the decision is gated in two places (intake Gate 1, then the pack's `gonk-dispatch` Gate 2) a bead can be `/decide`d twice before any outcome is reported, so a second call for the same `(project, bead_id, session_key)` must **return the existing open, unexpired reservation, not mint a second one** — otherwise one attempt holds double the budget headroom. See `/v1/policy/decide` below.
5. **"Unlimited" serializes as JSON `null`** for both cost and tokens. See Task 1.
6. **The budget month is the UTC calendar month.** Not project-local, not rolling-30d. Two projects in different timezones must not disagree about which month a charge lands in. Rollover is **monotone**: a backwards clock jump can never reset a project's spend.
7. **Clock skew is a health problem, not a math problem.** Meter compares its clock to the spend source's HTTP `Date` header on every sync. Skew beyond `max_clock_skew` (default 5m) makes `/readyz` fail and makes every decision `defer`. The budget math never tries to be clever about a wrong clock.
8. **Meter resolves `.gonk.yml`, and meter owns quiet hours.** Settled by the controller; see "Division of responsibility" below. Intake pushes **raw** `.gonk.yml` bytes; meter validates, resolves, and owns the resulting `Effective`. `schedule.quiet_hours` is a wait-vs-spend decision, so it lands here as a `defer` with `retry_after` = end of the quiet window. Intake has no quiet-hours code and no resolver.

### Owner decisions folded in (settled; do not re-open)

9. **Local models are priced synthetically in LiteLLM, so the USD virtual-key ceiling is a hard door for *every* rung.** LiteLLM's virtual-key budget is USD-denominated: it has no token counter, and local models at $0 meant the dollar door never closed on them either — so `monthly_tokens` had no hard enforcement anywhere. Decision: give every local rung a **nonzero synthetic per-token price** in LiteLLM's model config (supplied by the operator's rung catalog, `pkg/opercfg`), and have meter convert the project's **token** budget into the dollar ceiling it provisions on the virtual key. See "Synthetic pricing" below for the full contract — including the invariant it must not break.
10. **The ledger backend is Dolt.** It is already run for the beads store, so it adds no new dependency, and its versioned history suits an audit ledger. **This is not yet verified.** The reservation path needs real transactional guarantees, and Task 0b is an explicit, blocking spike that *proves* them empirically before any code depends on them. Do not proceed on the assumption that it works.
11. **The spend source is LiteLLM's `/spend/logs` HTTP API**, not its Postgres. Rationale: a supported surface that survives upgrades, with no coupling to LiteLLM's internal schema. The cost is polling lag, and meter must account for it everywhere it matters — see "Spend lag" below. **The poller MUST always pass a bounded date window and paginate; a naked, unbounded `/spend/logs` query is forbidden.** This is not a nicety: a verified smoke test (`docs/environment.md`, "OPERATIONAL HAZARD") **OOM-killed the live LiteLLM pod** (2Gi limit, exit 137, ~90s outage) with a single unbounded query, because it loads the whole spend table into memory — and meter shares that LiteLLM with the whole cluster, so a poll bug here takes down the entire cluster's inference gateway, not just gonk. **A missing date bound is a code-review blocker.** Prefer the bounded `/spend/logs/v2` (paginated, mandatory dates, 10k cap) where the pinned LiteLLM version offers it; note the version dependency. See the `HTTPSpendSource` task.
12. **Secrets are file mounts, with two rotation slots.** Vault/1Password → k8s Secret → **file mount**. Never an env var (env leaks into `ps`, crash dumps, and every child process), never a flag, never the config file, never git. Applies to the LiteLLM admin credential and the meter API bearer token. Each has a current slot and a previous slot so a rotation is not an outage. Same contract as Plan 02's GitLab bot token and webhook secret.

---

## Division of responsibility with gitlab-intake (settled)

Plans 02 and 03 were written in parallel by authors who could not see each other, and both claimed config resolution and quiet hours. The controller settled it. **This table is the contract; neither plan may re-derive the other's half.**

| Responsibility | Owner | Notes |
|---|---|---|
| Fetching `.gonk.yml` from GitLab | **intake** | Size-capped; the bytes are attacker-controlled. |
| Classifying a project's *GitLab* state (member / absent / declined / `.agent/` present) | **intake** | Derived from GitLab alone. Intake may call `gonkcfg.Load` to answer "do these bytes parse", for its own onboarding UX and metrics. |
| **Validating + resolving `.gonk.yml` into an `Effective`** | **meter** | `gonkcfg.Resolve` is called in exactly one place in the whole system: `service.Register`. Intake **must not** call `Resolve`. |
| Operator instance/group policy (`pkg/opercfg`) | **meter** | Meter is the only component that holds it. This is *why* resolution lands here: two independent resolvers means two sources of truth for a budget ceiling. |
| Folding nested GitLab groups into the single group layer `Resolve` accepts | **meter** | `opercfg.GroupFor`. See "Nested groups" below — a dropped ancestor ceiling is a budget escape. |
| Is this project enabled? What is its ladder? What is its budget? | **meter** | Published in the `PUT /v1/projects/{project}` response. Intake **reads** it; intake never computes it. |
| **Quiet hours** (`schedule.quiet_hours`) | **meter** | Enforced as a `defer` with `retry_after` = end of the window. Intake has no quiet-hours code. |
| Rung choice | **meter** | `POST /v1/policy/decide`, called in **two places**: **intake (Gate 1)** to gate the first dispatch, and the **pack's `gonk-dispatch` exec order (Gate 2)** to re-decide on every pour. A formula cannot make the call (Plan 04's correction of spec 6.2.3). `/decide` is idempotent on an open reservation so the two callers never double-reserve. |
| Budget enforcement (soft: refuse to start) | **meter** | The reservation gate in `/decide`. |
| Budget enforcement (hard: refuse mid-flight) | **LiteLLM** | The virtual key's USD `max_budget`. Meter provisions it; see "Synthetic pricing". |
| Virtual-key provisioning | **meter** | Meter returns only a `key_ref`; the key material goes to a `KeySink` (Plan 05's k8s Secret). |
| Ladder / attempt state | **meter** | Its own store, written by `/v1/policy/outcome`. Never caller-supplied. |
| Attribution-tag minting (`atags`) | **meter** | `/decide` returns `metadata`; the pack stamps it verbatim. Meter is the single charset boundary. |
| The order / dispatch call (GitLab event → Gas City order) | **intake** | Intake fires an order. It does **not** ask meter for permission first — the pack does, at `/decide`, immediately before the session spawns. |
| Parking a deferred bead and retrying at `retry_after` | **pack (Plan 04)** | A `defer` never reaches intake. |

**Consequence for intake's state machine.** Intake's states split cleanly into three groups:

- **GitLab-determined** (intake computes these): `unmanaged`, `absent`, `declined`.
- **Meter-determined** (intake *records* these from the `PUT` response and cannot compute them): `invalid`, `disabled`, `key-missing`, `active`.
- **The join** (meter says `active`, and intake observes the repo): `pending` (no `.agent/` yet) and `valid` (`.agent/` present).

Plan 02's `Classify` therefore takes an `Observation` (from GitLab) **and meter's response**, and takes no `gonkcfg.Policy` arguments at all.

---

## What the hard door actually covers (read before you claim a budget "cannot" be exceeded)

Spec §9 says agent-pod egress is confined to GitLab and LiteLLM, "**so budgets
cannot be bypassed**". **On the target cluster that sentence is false at the
network layer, today.** `docs/environment.md` records why, and the owner's
decision is **ship it, document the gap, do not gate on it.** This plan implements
that decision faithfully — which means it must never let the gap be mistaken for
a guarantee.

The concrete bypass: **NetworkPolicies are not enforced** (Flannel does not
implement them; the Cilium HelmRelease is suspended), and LiteLLM routes local
models to an **Ollama at `http://192.168.1.142:11434` that requires no
credential**. An agent pod can therefore skip LiteLLM entirely, call Ollama
directly, and burn local GPU with **zero metering and no ceiling**. Every door in
this plan — the reservation gate, the virtual key's `max_budget`, the synthetic
price — binds only traffic that *actually goes through LiteLLM*. The
NetworkPolicy is the only thing that forces it to, and it is not enforced.

**Say it exactly this way, everywhere (ADR-004, the metrics help text, the
dashboards, the README):**

> **Cloud-rung budgets are hard.** A cloud call needs an API key, the pod's only
> route to one is LiteLLM's virtual key, and LiteLLM refuses at the ceiling.
> **Local-model budgets are advisory** until Cilium is unsuspended and the egress
> policy is actually enforced. Decision 9's synthetic pricing gives the token
> budget a hard door *through LiteLLM*; it does not give it one *around* LiteLLM.

What this plan still does, and it is not nothing: it makes the door **correct**,
so that the day Cilium lands, the guarantee is real without another line of Go.
Plan 05 ships the egress NetworkPolicy anyway (documentation-as-code); Plan 06
**writes the egress-denial test and skips it**, naming Cilium in the skip
message. Un-skipping that test is the gate. Nothing in this plan may be written
as though that day has already come.

---

## Synthetic pricing: making the USD door a hard door for local rungs

**The problem.** Spec 6.2 says "hard refusal happens in LiteLLM, at the only door." But a LiteLLM virtual key enforces a **USD** `max_budget` and nothing else. There is no token counter. And local models are priced at $0, so the dollar counter never moves for them and the door never closes. Result: `monthly_tokens` and `per_task_tokens` had **no hard enforcement anywhere**, and a local-only project (the onboarding default!) had no hard door at all.

**The decision.** Price local models synthetically. Every rung gets a nonzero per-token price in LiteLLM's model config — *real* for cloud rungs, *synthetic* for local ones — and meter converts the project's token ceiling into the USD ceiling it provisions on the virtual key. The dollar door now closes for every rung.

**The invariant this must not break.** The onboarding MR ships `budget: { monthly_cost_usd: 0 }` with `ladder: [qwen-local]` (spec 5.3), and that project **must be able to run its local rung**. So:

> **Real dollars and synthetic dollars are two different currencies and must never be added together in a policy decision.**
>
> - `.gonk.yml`'s `budget.monthly_cost_usd` means **real money**. It is what a cloud rung costs. It is unchanged by this decision.
> - `rung.Decide`'s cost gate is checked against **real** dollars only. `monthly_cost_usd: 0` therefore still means "local rungs run freely, a cloud rung is never affordable, forever" — exactly as before. **This is the property that keeps the onboarding default alive, and Task 4's table asserts it directly.**
> - Synthetic dollars exist for exactly one purpose: to give LiteLLM's single USD counter teeth over the *token* budget. They are an internal accounting unit. **They are not spend.**

**The mechanism.**

1. `pkg/opercfg`'s rung catalog gains `synthetic_usd_per_1m_tokens`, **required on local rungs and forbidden on cloud rungs** (a cloud rung's price is real, and lives in LiteLLM's model config; declaring a synthetic price for it would be a lie). The operator must configure the *same* price in LiteLLM's model config for the corresponding local model — a drift Plan 06 verifies (P3-4).

   **Where that price physically goes — it is one named file, not "LiteLLM's config somewhere":**

   | | |
   |---|---|
   | Repo | the gitops repo (`/mnt/c/Users/steve/Code/gitops`), **not** this one |
   | File | **`clusters/orac/apps/litellm/litellm.yaml`** |
   | Models | `spec.values.proxy_config.model_list` |
   | **Prices** | **`spec.values.litellm_settings.model_cost_map`** |
   | Today | **local models are priced at ZERO there.** That is the hole. |

   **A flagged ambiguity — do not silently assume it away.** LiteLLM accepts a
   per-token price in *two* plausible places: the `litellm_settings.model_cost_map`
   the repo actually uses, and a per-model `model_info: {input_cost_per_token, …}`
   block inside `model_list`. **The repo only uses `model_cost_map`, so that is
   what this plan targets** — conform to the house convention rather than
   introducing a second one. But the two are not obviously equivalent for
   *locally-routed* models, and **nobody has verified which one LiteLLM actually
   consults when it writes a `/spend/logs` row for an Ollama-backed model.**
   **Plan 06, Task 5 must answer that empirically** (it is a superset of P3-4: not
   just "do the numbers agree" but "does the number we set actually move the
   spend"). If `model_cost_map` turns out not to price local routes, the synthetic
   dollar never lands in a spend row, the USD door never closes on tokens, and
   Decision 9 buys nothing. **That is a finding for the owner, not a footnote.**
2. Meter computes a per-token price for each rung uniformly:

   ```
   pricePerToken(r) = r.SyntheticUSDPer1MTokens / 1e6         if r.Kind == local
                    = r.EstCostUSD / float64(r.EstTokens)      if r.Kind == cloud
   ```

3. Meter provisions the virtual key with

   ```
   max_budget = monthly_cost_usd  +  monthly_tokens × max(pricePerToken(r) for r in effective ladder)
   ```

   If either ceiling is unlimited, `max_budget` is **omitted** (unlimited) — the hard door genuinely cannot exist for an unlimited project, and the plan says so rather than pretending.

   This ceiling is deliberately **loose**: it is an upper bound, not a tight one. It cannot close *early* on legitimate work (that would be worse than no door), and it *does* close on a project that blows its token budget on local inference — which is the hole this decision exists to shut. Meter's reservation gate remains the primary control; **LiteLLM is now the hard backstop, and meter is defense in depth.**

**Consequences the code must carry.**

- **Synthetic dollars are labelled everywhere, and never presented as real spend.**
  - `spend.Row` gains `Synthetic bool`, set at ingest from `catalog[row.Tags.Rung].Kind == local`. A row whose rung is **not in the catalog** is treated as **real** (fail closed: it counts against the money ceiling).
  - `budget.Spend` splits cost into `CostUSD` (real; cloud only) and `SyntheticCostUSD` (local only). `budget.Remain` computes remaining against `monthly_cost_usd` from **`CostUSD` alone**.
  - The cost API carries `cost_usd` (real), `synthetic_cost_usd`, and `cost_synthetic: bool` on every per-rung breakdown. **A client must never sum them into one number.**
  - Prometheus: `gonk_meter_spend_usd_total{project,rung,trigger,synthetic="true|false"}`. Dashboards filter on the label; the Cost dashboard shows real spend by default and synthetic as a separate, clearly-titled panel.
- **Reservations hold both currencies.** `store.Reservation` carries `CostUSD` (real) and `SyntheticCostUSD`. A local rung's reservation holds *synthetic* dollars and *zero* real dollars, so it cannot make a `monthly_cost_usd: 0` project unaffordable to itself.
- `opercfg` validation flips accordingly (Task 2): a local rung must have `est_cost_usd: 0` **and** `synthetic_usd_per_1m_tokens > 0`; a cloud rung must have `est_cost_usd > 0` **and** no synthetic price.

---

## Spend lag: what "LiteLLM's spend API" costs us

Reading `/spend/logs` over HTTP (rather than LiteLLM's Postgres) is the supported, upgrade-safe surface — but it is a **poll**, so meter's view of spend is always slightly behind reality. Every place that matters:

| Where the lag bites | How it is handled |
|---|---|
| Budget-remaining check at `/decide` | Open **reservations** cover the gap: `remaining = ceiling − observed − reserved`. The reservation is written before the session starts and is *settled*, not freed, when the outcome arrives — the hold moves to `now + max_spend_staleness`, i.e. until the rows are expected to have landed. |
| A snapshot that is too old to decide on | `Decide` defers with `spend-data-stale` for any project with a finite ceiling. A project with *no* ceilings has nothing to be stale about and runs. |
| **Month rollover with a late row** | Rows are windowed by **the row's own timestamp** (`w.Contains(r.At)`), never by ingest time. A row for July that lands on 1 August is counted against **July**, not against the new window. Task 3 tests this explicitly — getting it wrong would hand every project a free budget on the 1st. |
| Cost in commit trailers (spec 6.1, `provenance.include_usage`) | `/v1/cost/session/{key}` is read *while the session is still open*, so it will frequently return `complete: false`. **The pack must not write a cost it does not know.** See Owner decision on trailers below. |
| Meter restart | Reservations are now **durable** (Dolt), so they survive it. This was the strongest argument for a durable store and it is why Owner decision 10 exists. |
| LiteLLM's lag exceeding `max_spend_staleness` | The staleness gate fires and everything defers: the system **stalls rather than overspends**. Plan 06 must measure the real lag and set `max_spend_staleness` above it. |

---

## Nested groups: `Resolve` takes ONE group layer, and GitLab groups nest

**This is a budget escape if you get it wrong, and it is easy to get wrong.**

`gonkcfg.Resolve(instance, group, project)` accepts exactly **one** group `Policy`. GitLab group paths **nest arbitrarily**: `agentic`, `agentic/experiments`, `agentic/experiments/spike`. So somebody has to fold a chain of ancestor groups into the single layer `Resolve` will accept, and that somebody is `opercfg.GroupFor` (Task 2), in meter, and nowhere else.

The tempting implementation is **longest-prefix match** — find the most specific group that matches and return it. **That is the bug.** Consider:

```yaml
groups:
  agentic:              { budget: { monthly_cost_usd: 50 } }   # the parent sets a ceiling
  agentic/experiments:  { enabled: true }                      # the child sets no budget at all
```

Project `agentic/experiments/spike` longest-prefix-matches `agentic/experiments`, whose `Budget` is empty. `Resolve` sees a silent group layer, applies no group ceiling, and the project is bounded only by the **instance's** ceiling. The parent's $50 ceiling has **silently vanished** — a nested group has effectively loosened its parent's budget, a direct contradiction of spec 5.4 and ADR-002 ("ceilings only tighten downward").

`GroupFor` therefore **folds every matching ancestor**, coarsest-first, using ADR-002's own precedence semantics one layer at a time:

- **budgets take the minimum** over every layer that sets one (so an ancestor's ceiling survives a silent child, and a child can only tighten);
- `enabled` and `actions` are **vetoable** (an explicit `false` anywhere in the chain sticks);
- **ladders intersect**, and an empty intersection must be a **non-nil zero-length slice** — a `nil` there means "this layer is silent" (ADR-002 says so in as many words), so a group chain that allows *no* rung would otherwise be read as imposing *no constraint*, and the project's cloud rung would run;
- other scalars are most-specific-wins.

And prefixes match on **path-segment boundaries only**: group `agentic` must not capture project `agentic-other/repo`.

Task 2's `TestGroupFor`, `TestGroupForCeilingsOnlyTighten`, and the nil-vs-empty-ladder assertion are the tests that hold this line. **Do not weaken them.**

---

## Failure-mode matrix (requirement: hard budgets fail closed)

| Failure | Meter's behavior | Why it is closed |
|---|---|---|
| LiteLLM admin API unreachable at onboarding (key never provisioned) | Registration state `key-missing`; `/decide` returns **defer** (`virtual-key-missing`, `retry_after` = now + 5m). Reconcile loop retries provisioning. | No key, no session, no spend. Never `run`. |
| LiteLLM spend source unreachable / spend snapshot older than `max_spend_staleness` (default 5m) | `/decide` returns **defer** (`spend-data-stale`) for any project with *any* finite ceiling. Projects with all-unlimited budgets still run (nothing to be stale about). | We would be deciding on numbers we know are wrong. |
| Spend-log lag (calls made, rows not yet written) | Open reservations cover the lag window: remaining = ceiling − observed − reserved. The reservation is written **before** the session starts, and reporting an outcome does **not** free it — it *settles* it, moving the hold to `now + max_spend_staleness`, i.e. until the rows are expected to have landed. If they have not, the snapshot is stale by definition and the next row of this table takes over. LiteLLM's USD ceiling is the hard backstop. | The hold ends when the *data* catches up, not when the *session* does. |
| Clock skew at month rollover (NTP correction, VM restore) | Backwards jump: `spend.Advance` is monotone, window does not move, spend is not reset. Forwards jump beyond `max_clock_skew` vs the spend source's clock: `/readyz` fails, and every project **with any finite ceiling** defers (an all-unlimited project has no budget to protect, so it runs). | An early rollover is a free budget. Refuse to roll rather than guess. |
| Spend source omits its `Date` header, so skew cannot be measured | `spend.SkewOK` returns true (blocking the whole factory because a proxy stripped a header is worse than the risk), but `gonk_meter_clock_skew_unknown_total` increments. **This is the one place the skew guard is advisory rather than fail-closed — alert on that counter.** | Stated rather than hidden. |
| Concurrent sessions racing the same ceiling | `/decide` takes a **per-project lock** across `read spend + read reservations + decide + write reservation`. Losers see the winner's reservation and `defer`. Proven by a `-race` test: 20 goroutines, headroom for 2, exactly 2 `run`. | The check and the reservation are one atomic step. |
| Session dies without reporting an outcome | Reservation expires at `reservation_ttl`; the janitor records an `infra-failed` attempt. | Budget stays held for the TTL; the rung does **not** escalate. |
| Meter restarts | Cold start: `/readyz` false until the first spend sync completes. **Reservations survive** — they are in the durable store (Dolt, Decision 10). This is exactly why the in-memory store is not the shipping backend: losing reservations on restart is fail-open on headroom for the length of the spend-log lag. | Metric `gonk_meter_cold_start_total`. |
| **Dolt does not actually provide the isolation the reservation race needs** | **Task 0b proves or disproves this before anything depends on it.** If the racing test overspends, the fallback is (a) keep meter single-replica so the in-process per-project lock *is* the serialization and Dolt supplies durability only, or (b) move the ledger to **Postgres on the owner's existing CNPG cluster** (owner-approved 2026-07-13 — not a new dependency; take it without hesitation if the spike fails OR is inconclusive). The plan does not proceed on the assumption. | An unverified transactional guarantee under a budget ceiling is exactly the thing that must not be assumed. |
| Two meter **replicas** race the same ceiling | The per-project `keyedMutex` is **in-process**: it serializes decisions inside one meter, and does nothing across pods. **Assumed default: meter runs single-replica** (`replicas: 1`, `strategy: Recreate` — Plan 05). With >1 replica, correctness depends entirely on the store's isolation, which is what Task 0b measures. | Stated, not hidden. A silent second replica is a silent budget escape. |
| A local rung burns the whole token budget **through LiteLLM** | LiteLLM's USD `max_budget` now covers it: local models carry a **synthetic** per-token price, and meter folds the token ceiling into the dollar ceiling it provisions (Decision 9). Meter's reservation gate refuses first; LiteLLM refuses if meter is wrong. | The token budget has a hard door **on that path**. |
| **An agent pod calls Ollama (`http://192.168.1.142:11434`) DIRECTLY, around LiteLLM** | **NOTHING IN THIS PLAN STOPS IT.** Ollama needs no credential, and the NetworkPolicy that would forbid the egress **is not enforced** (Flannel; Cilium suspended). Zero metering, no ceiling, no attribution. | **NOT CLOSED.** Owner decision: ship, document, do not gate. **Local-model budgets are advisory** until Cilium lands; cloud-rung budgets remain hard (a cloud call needs a key the pod only holds via LiteLLM). Plan 05 ships the policy; Plan 06 skip-tests it; un-skipping that test is the gate. |
| Synthetic dollars get counted as real spend | `spend.Row.Synthetic` is set at ingest from the rung's `kind`; `budget.Spend` keeps `CostUSD` (real) and `SyntheticCostUSD` separate; `rung.Decide`'s cost gate reads **real only**. A rung missing from the catalog is treated as **real** (fail closed). | If synthetic dollars reached the cost gate, `monthly_cost_usd: 0` would make the onboarding default's own local rung unaffordable — the project would be dead on arrival. |
| A spend row for last month lands after the window rolled | Rows are windowed by **the row's timestamp**, never by ingest time. A July row arriving on 1 August is charged to July. | Otherwise every project gets a free budget on the 1st. |
| `.gonk.yml` invalid / project disabled / ladder exhausted / per-task tokens gone | **deny** with a machine reason. Never `run`, never an infinite `defer`. | Retrying cannot help. |
| Attribution tag contains a newline, comma, or control character | Rejected at the boundary with 400 before any tag is minted (Task 5). | Injection into log lines / CSV / headers downstream. |
| Operator instance/group `Policy` malformed (NaN ceiling, unknown rung, negative budget, unpriced cloud rung, **unpriced local rung**, rung with no token estimate) | `opercfg.Load` refuses to start the process (Task 2). Meter does not run on a config it cannot validate. | ADR-002's "Known gap", closed. An unpriced *local* rung is now just as fatal as an unpriced cloud one: with no synthetic price it has no hard door (Decision 9). |
| A caller forges an outcome (`gate-failed` on repeat) to climb to an expensive rung | `/v1/policy/outcome` binds the outcome to a reservation **meter** minted: an unknown `reservation_id`, or one whose `(project, bead, attempt)` does not match, is a 400 and records nothing. Attempts are keyed by reservation, so a replay overwrites instead of appending. | Meter owns ladder state; the caller cannot vote twice. |
| A project reorders its ladder to put a cloud rung first | `opercfg.CheckLadderOrder` requires the project's ladder to be a subsequence of the instance's (`enforce_ladder_order`, default on), and `Load` refuses to start if the guard is on with no instance ladder to order against. | Spec 6.3: "everything starts at the cheapest rung its project config allows". |
| A nested group tries to loosen its parent's ceiling | `opercfg.GroupFor` folds **every** matching ancestor group, min-folding budgets, before `Resolve` ever sees them. | Ceilings only tighten downward (spec 5.4, ADR-002). |
| A LiteLLM row carries a negative `spend` (a credit or adjustment) | `budget.Remain` clamps negative consumption to zero; it can never *raise* a ceiling. | External input must not be able to mint headroom. |
| Two projects whose names flatten to the same slug (`a/b` and `a-b`) | `keysink.Slug` appends a hash of the original path, so the LiteLLM key alias is unique. Otherwise the second project would silently adopt the first's key — and its `max_budget`. | One virtual key, one project, one hard ceiling. |
| A project turns off an action (`actions.triage: false`) at any layer | `/decide` denies with `action-not-allowed` and mints no tags and returns no `key_ref`. `/decide` is the only chokepoint before a session spawns, so if meter does not check `Effective.Actions`, nothing does. | The ADR-002 veto surface is real, not decorative. |

---

## File structure

```
cmd/gonk-meter/main.go                    wiring, flags, secret FILES, tzdata import,
                                          STORE BACKEND SELECTION (Task 8 Step 5)
cmd/gonk-meter/clock.go                   //go:build !testclock -- Now() = time.Now
cmd/gonk-meter/clock_testclock.go         //go:build testclock  -- Now() reads GONK_TESTCLOCK_FILE
                                          (Plan 06 HB-3. NEVER in a production image.)
pkg/meterapi/meterapi.go                  * THE SHARED CONTRACT (Task 0). Plan 02 imports it.
pkg/meterapi/meterapi_test.go             wire-literal golden tests
pkg/meterapi/testdata/contract.sha256     drift gate: a field rename fails CI
pkg/budget/limits.go                      CostLimit/TokenLimit: JSON-safe "unlimited"
pkg/budget/remaining.go                   Budget, Spend (real + synthetic), Remaining, Remain()
pkg/spend/window.go                       UTC month Window, monotone Advance, SkewOK
pkg/spend/rows.go                         Row (spend log entry x atags, Synthetic flag), aggregations
pkg/opercfg/opercfg.go                    OperatorConfig: instance/group Policy + rung catalog + meter knobs
pkg/opercfg/gonk-operator.v1.schema.json  canonical schema (embedded, drift-gated)
pkg/rung/decide.go                        THE pure policy: Decide(Input) Decision
pkg/rung/outcome.go                       Outcome/Attempt/gate classification, QuietHours
internal/meter/tagmint/tagmint.go         boundary-validated atags minting
internal/meter/store/store.go             Store interface (registrations, attempts, reservations, spend)
internal/meter/store/memory.go            in-memory Store (tests only; NEVER selectable in production)
internal/meter/store/dolt.go              the Decision-10 Store (gated on Task 0b)
internal/meter/store/postgres.go          the OWNER-APPROVED FALLBACK Store (CNPG). Task 0b decides
                                          which one ships; GONK_METER_STORE_BACKEND selects at runtime.
internal/meter/store/storetest/suite.go   ONE conformance suite, run against ALL implementations
internal/meter/litellm/admin.go           Admin interface + HTTP client (key provisioning/rotation)
internal/meter/litellm/spendsource.go     SpendSource interface + HTTP client (/spend/logs)
internal/meter/litellm/fake.go            fakes for both, used by every test
internal/meter/keysink/keysink.go         KeySink interface + memory sink
internal/meter/keysink/k8s.go             * the KUBERNETES KeySink (Task 7 Step 4b).
                                          THE GO CODE IS THIS PLAN'S. The Role/RoleBinding
                                          that make it legal are PLAN 05's (Task 4).
internal/meter/service/service.go         Service: registration, decide, outcome, reconcile loops
internal/meter/service/http.go            HTTP handlers over pkg/meterapi's types
internal/meter/metrics/metrics.go         Prometheus collectors
docs/api/gonk-meter-v1.md                 published service API (intake/pack contract)
docs/adr/ADR-004-rung-policy-and-budget-enforcement.md
docs/schemas/gonk-operator.v1.schema.json published copy (drift-gated)
```

---

## The service API — `pkg/meterapi` is the SINGLE SOURCE OF TRUTH

**Plan 02 (gitlab-intake) and Plan 04 (pack) are clients of this API. This plan owns it.**

There is exactly **one** shared Go package for the intake↔meter↔pack wire contract: **`pkg/meterapi`**. Plan 03 owns it (meter is the server; the clients conform). Plan 02 **imports** it and does not restate a single field. The normative Go source is **Task 0** below, and a sha256 drift gate (`pkg/meterapi/testdata/contract.sha256`) makes a silent field rename fail CI — the same idiom `pkg/gonkcfg` already uses for its schema.

> **Sequencing note.** Plan 02 executes *before* this plan, so Plan 02 will physically land `pkg/meterapi` first, by copying Task 0's source **verbatim**. That does not transfer ownership. If Plan 02's executor finds they need a field that is not in Task 0, **that is a change to this plan's contract and must be made here first.** Task 0's checkbox is satisfied by verifying the shipped package is byte-identical to the normative source.

All endpoints are JSON over HTTP, cluster-internal, and require `Authorization: Bearer <token>` (read from a **file**, two rotation slots — see Decision 12) except `/healthz`, `/readyz`, `/metrics`.

### The endpoint set (complete; nothing else exists)

| Method + path | Caller | Purpose |
|---|---|---|
| `PUT /v1/projects/{project}` | intake | Register/update a project from its **raw `.gonk.yml`**. Meter validates, resolves, provisions the virtual key. |
| `GET /v1/projects/{project}` | intake, **pack** (`gonk-gate trailers` reads `effective.provenance`), humans | Current state, effective config, budget, remaining. |
| `DELETE /v1/projects/{project}` | intake | De-onboard: disable the project and delete its virtual key. |
| `POST /v1/projects/{project}/key/rotate` | operator | Rotate the virtual key. |
| `POST /v1/policy/decide` | **intake (Gate 1)** and the **pack `gonk-dispatch` exec order (Gate 2)** | **The rung decision.** run / defer / deny. Called in TWO places — see "The rung gate is in two places" below. Idempotent on an open reservation. |
| `POST /v1/policy/outcome` | **pack** gate step (`gonk-gate sweep`) | Report a terminal outcome; settles the reservation, records ladder state. |
| `GET /v1/cost/bead/{bead_id}` | pack, humans | Cost/tokens for one bead (all attempts, all time). |
| `GET /v1/cost/session/{session_key}` | agent (commit trailers) | Cost/tokens for one session. |
| `GET /v1/cost/project/{project}` | intake, dashboards | Cost/tokens for the current budget window + remaining. |
| `GET /v1/cost/instance` | dashboards | Instance rollup. |
| **`POST /admin/spend/sync`** | operator, **test harness**, **pack (`gonk-gate sweep`)** | Force **one** spend-log poll, **block** until it completes, return `spend_as_of`. Bearer-authenticated. **Plan 06 hand-back HB-2** — without it every "assert the ledger says X" is a sleep, and a sleeping e2e is a flaky e2e (Task 9 Step 3b). **It is ALSO a production dependency of the pack:** the sweeper forces a spend poll (every ~30s) before classifying an outcome, so it must accept the pack's bearer token and be safe to call repeatedly. |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | k8s, Prometheus | Unauthenticated. |

`{project}` is the GitLab `path_with_namespace`, **URL-path-escaped** (`group%2Frepo`). Use `meterapi.ProjectPath(project)` — never hand-build it.

**Intake calls five of these:** `PUT`/`GET`/`DELETE /v1/projects/{project}`, `GET /healthz`, **and `POST /v1/policy/decide` (Gate 1)**. Spec 6.2.3's single-gate model (only the dispatch formula decides) is **impossible** — a formula cannot make an HTTP call to meter — so the rung/budget decision is gated in **two places**: intake gates the FIRST dispatch (Gate 1), and the pack's `gonk-dispatch` exec order re-decides on every pour (Gate 2, the enforcement point). See "The rung gate is in two places" below. Intake fires the order **only** on `decision: "run"`; it still never sends an attempt count (`DecideRequest` has none).

### `PUT /v1/projects/{project}` — intake pushes RAW config, meter resolves

This is the resolved half of **Conflict A**. Intake sends the `.gonk.yml` **bytes it read from GitLab, verbatim**, plus the GitLab metadata meter cannot know. Meter validates them, folds the operator's instance policy and the (nested-group-folded) group policy over them, calls `gonkcfg.Resolve` — **the only call to `Resolve` in the entire system** — and owns the resulting `Effective`.

```json
// request (meterapi.ProjectRequest)
{ "project": "group/repo",
  "project_id": 42,
  "rig": "group-repo",
  "default_branch": "main",
  "config_commit_sha": "9f2a1c…",          // commit .gonk.yml was read at; "" if unknown
  "gonk_yml": "version: 1\nenabled: true\n..."   // RAW BYTES. Meter is the validator.
}
```

Three response codes, and the distinction matters:

**`200` — the config loaded and resolved.**

```json
// meterapi.ProjectResponse
{ "project": "group/repo", "rig": "group-repo",
  "state": "active",                        // active | disabled | key-missing
  "disabled_reason": "",                    // gonkcfg Effective.DisabledReason, verbatim
  "effective": {
    "enabled": true,
    "actions": {"triage": true, "pipelines": false, "features": false},
    "ladder": ["qwen-local","glm"],
    "continuity": "resume",
    "triage": {"label_prefix":"gonk::","respond_to_mentions":true},
    "provenance": {"commit_trailers":true,"include_usage":false},
    "schedule": {"quiet_hours":"22:00-07:00","timezone":"America/New_York"}
  },
  "budget": { "monthly_cost_usd": 10, "monthly_tokens": 50000000, "per_task_tokens": 2000000 },
  "key_ref": { "secret_name": "gonk-key-group-repo-1a2b3c4d", "secret_key": "LITELLM_API_KEY" },
  "config_hash": "sha256:…",                // of the gonk_yml meter resolved
  "updated_at": "2026-07-13T10:00:00Z" }
```

**`422` — the `.gonk.yml` is present but will not load.** Schema-invalid, non-finite budget, unknown timezone, reordered ladder. This is a **successful, idempotent registration of an invalid config**, not a bad HTTP request: meter has recorded `state: invalid` and **deleted the project's virtual key** (a broken config must not keep spending).

```json
{ "project": "group/repo", "rig": "group-repo",
  "state": "invalid",
  "disabled_reason": "",
  "error": ".gonk.yml: jsonschema validation failed with '…#'\n- at '/budget/monthly_tokens': …",
  "effective": null,                        // NULL when invalid. There is no Effective. (ADR-002)
  "budget": { "monthly_cost_usd": 0, "monthly_tokens": 0, "per_task_tokens": 0 },
  "key_ref": {"secret_name":"","secret_key":""},
  "config_hash": "sha256:…", "updated_at": "…" }
```

Intake echoes `error` into the MR/issue comment. Note the fail-closed budget: **`null` would mean *unlimited*, which is the exact opposite of what an invalid config must get.**

**`400` — the *request* is malformed.** Missing `project_id`, an attribution-unsafe project path, an oversized body. **Nothing is recorded.** This is the only 4xx that means "intake has a bug".

Hard rules on this seam:

- **`null` means unlimited.** `Effective.Budget` encodes unlimited as `math.Inf(1)` / `math.MaxInt64`, and **`+Inf` is not JSON-serializable** — `json.Marshal` returns an error (PLAN.md's highest-value carry-forward, ADR-002). `meterapi.Budget` uses `*float64`/`*int64` with `nil` == unlimited. **Nobody may marshal `gonkcfg.EffectiveBudget` directly.**
- `effective` is `null` **if and only if** `state == "invalid"`. ADR-002: `Resolve`'s return value is the only legitimate way to produce an `Effective`, so an invalid config gets *no* `Effective` rather than a zero one.
- `disabled_reason` is non-empty **iff** `state == "disabled"` (ADR-002's invariant, carried onto the wire).
- `key_ref` is a **pointer to** where the key lives. **It is never key material.** Intake logs this struct.
- Meter is idempotent on `config_hash`: intake may PUT the same bytes every reconcile pass (every 10 minutes, per project) and meter no-ops.
- A `DELETE` (de-onboard) disables the project and deletes the virtual key. It is idempotent; deleting an unknown project is `204`.

### `POST /v1/policy/decide` — the rung decision (two callers: intake Gate 1, pack Gate 2)

```json
// request (meterapi.DecideRequest)
// NOTE: NO attempt number. NO prior-outcome list. Meter owns ladder state (Decision 2).
{ "project": "group/repo", "rig": "group-repo", "bead_id": "gk-1a2b",
  "session_key": "sess-9", "trigger": "issue-triage" }

// 200, run (meterapi.DecideResponse)
{ "decision": "run", "rung": "glm", "model": "glm-5", "attempt": 2,
  "reason": "", "detail": "",
  "metadata": { "gonk_project":"group/repo", "gonk_rig":"group-repo", "gonk_bead_id":"gk-1a2b",
                "gonk_session_key":"sess-9", "gonk_rung":"glm", "gonk_attempt":"2",
                "gonk_trigger":"issue-triage" },
  "key_ref": { "secret_name":"gonk-key-group-repo-1a2b3c4d", "secret_key":"LITELLM_API_KEY" },
  "reservation_id": "rsv-7f3c", "reservation_expires_at": "2026-07-13T11:00:00Z",
  "budget":    { "monthly_cost_usd": 10, "monthly_tokens": 50000000, "per_task_tokens": 2000000 },
  "remaining": { "monthly_cost_usd": 3.42, "monthly_tokens": 18000000, "per_task_tokens": 1500000 },
  "spend_as_of": "2026-07-13T09:58:00Z" }

// 200, defer  (a defer is a NORMAL ANSWER, not an error status)
{ "decision": "defer", "rung": "", "attempt": 2,
  "reason": "monthly-cost-exhausted",
  "detail": "rung \"glm\" needs $0.40, $0.02 remains this window",
  "retry_after": "2026-08-01T00:00:00Z",
  "metadata": {}, "key_ref": {"secret_name":"","secret_key":""},
  "budget": {…}, "remaining": {…}, "spend_as_of": "…" }

// 200, deny
{ "decision": "deny", "rung": "", "attempt": 3,
  "reason": "ladder-exhausted", "detail": "2 rungs, 2 gate failures",
  "metadata": {}, "key_ref": {"secret_name":"","secret_key":""},
  "budget": {…}, "remaining": {…}, "spend_as_of": "…" }
```

- **`defer` and `deny` are HTTP 200.** Only a malformed request (400) or an internal failure (500) is an error status. An unknown project is 200 + `deny`/`project-not-registered`.
- `remaining.monthly_cost_usd` is **real dollars only** — synthetic local-model dollars are not spend (Decision 9).
- `metadata` is `atags.Metadata()` — **meter mints it**, so meter is the single boundary where tag values are charset-validated (Plan 01 carry-forward 3). The pack stamps it verbatim onto every LiteLLM request.
- A `defer`/`deny` carries an **empty** `metadata` and an **empty** `key_ref` and **no** `reservation_id`. A decision that is not going to run hands out neither an attribution identity nor a route to a credential.
- **Quiet hours arrive here**, as `decision: "defer"` with `reason: "quiet-hours"` and `retry_after` = the end of the window in the project's timezone. Intake (Gate 1) now *handles* a `defer` — it records `retry_after` and fires nothing — but it still has no quiet-hours code of its own; the decision is made here.

#### The rung gate is in two places, and `/decide` MUST be idempotent on an open reservation

Spec §6.2.3 says the dispatch *formula* asks for the rung. A Gas City formula **cannot** make an HTTP call (Plan 04's central correction), so the decision is gated in two places, both importing `pkg/meterapi` (a compile-time type contract):

1. **Gate 1 — `gonk-intake`** calls `/decide` and fires the order **only** on `run`. An optimization: it avoids churning a bead meter would certainly `deny`, and surfaces an early `defer` metric.
2. **Gate 2 — the pack's `gonk-dispatch` exec order** (`gonk-gate dispatch`) calls `/decide` **again**, immediately before pouring the formula, on **every** pour including every controller-initiated re-sling. **This is the enforcement point, and the only path that pours a formula.**

**What breaks if the two callers drift (Plan 03 owns this endpoint, so it states it):**
- **Gate 2 must NEVER trust a rung/reservation passed in order vars — it always re-decides.** If it trusted intake's vars, a controller-initiated re-sling would spawn a higher rung against the *original* reservation and `key_ref`, i.e. **spend unmetered and mis-attributed** to the previous attempt's tags, and `/outcome` would be bound to a reservation already closed. That is "budgets cannot be bypassed" becoming false at the application layer. (Plan 04 Task 3's `TestDispatchAlwaysDecidesEvenWhenVarsCarryARung` guards it.)
- **`/decide` MUST be idempotent on an OPEN RESERVATION.** Because a bead can be `/decide`d twice before any outcome is reported (intake, then `gonk-dispatch`), a second call for the same `(project, bead_id, session_key)` **MUST return the existing open, unexpired reservation, not mint a second one.** Attempt and rung are naturally stable (meter derives them from **outcome** history, which has not changed between the two calls); the reservation is the only thing at risk. *If it minted a second:* two reservations would hold **double the budget headroom** for one attempt, `/outcome` (bound to a reservation meter minted) would settle one while the other leaks until `reservation_ttl`, remaining budget would be understated, and a spurious early `defer` would fire for a reason nobody can find. **Tested: `TestDecideIsIdempotentOnOpenReservation`** — call `/decide` twice for one `(project, bead, session)` with no intervening `/outcome`, assert one reservation exists and the two responses carry the same `reservation_id`.
- `run`/`defer`/`deny` are all **HTTP 200** — normal answers, identical whichever gate asks.

Machine `reason` codes (bounded set; also the Prometheus label value):
`project-not-registered`, `disabled`, `invalid-config`, `action-not-allowed`, `virtual-key-missing`, `quiet-hours`, `spend-data-stale`, `ladder-exhausted`, `infra-retries-exhausted`, `per-task-tokens-exhausted`, `monthly-tokens-exhausted`, `monthly-cost-exhausted`.

### `POST /v1/policy/outcome`

```json
// request (meterapi.OutcomeRequest)
{ "project":"group/repo", "bead_id":"gk-1a2b", "session_key":"sess-9",
  "attempt": 2, "rung": "glm", "reservation_id": "rsv-7f3c",
  "outcome": "gate-failed" }        // success | gate-failed | infra-failed | aborted

// 200 (meterapi.OutcomeResponse)
{ "ok": true, "recorded_attempt": 2, "next": "escalate", "next_rung": "sonnet" }
```

`next`/`next_rung` are **advisory** (a preview for logs and dashboards); the authoritative answer is the next `/decide`. An unknown `reservation_id`, or one whose `(project, bead, attempt)` does not match, is a **400** and records nothing — the outcome is what buys an escalation, so it must be bound to a reservation *meter* minted.

### Cost endpoints — and the synthetic-dollar labelling

```json
// GET /v1/cost/bead/gk-1a2b  (meterapi.BeadCostResponse)
{ "bead_id":"gk-1a2b", "project":"group/repo",
  "cost_usd": 0.42,                 // REAL money. Cloud rungs only.
  "synthetic_cost_usd": 0.02,       // local rungs, priced synthetically. NOT SPEND.
  "prompt_tokens": 120000, "completion_tokens": 8000, "total_tokens": 128000,
  "by_rung": [ {"rung":"qwen-local","kind":"local","cost_usd":0,"synthetic_cost_usd":0.02,
                "cost_synthetic":true,"total_tokens":100000,"calls":14},
               {"rung":"glm","kind":"cloud","cost_usd":0.42,"synthetic_cost_usd":0,
                "cost_synthetic":false,"total_tokens":28000,"calls":6} ],
  "attempts": [ {"attempt":1,"rung":"qwen-local","outcome":"gate-failed"},
                {"attempt":2,"rung":"glm","outcome":"success"} ],
  "as_of": "2026-07-13T09:58:00Z", "complete": true }
```

- **`cost_usd` and `synthetic_cost_usd` are different currencies and a client must never add them.** `cost_synthetic: true` on a rung breakdown means its dollars are an accounting fiction used to give LiteLLM's USD door teeth over tokens (Decision 9). Dashboards must label them as such; the Cost dashboard shows real spend by default.
- `as_of` and `complete` appear on **every** cost response. `complete: false` means an open reservation exists for this bead/session — spend rows for it may not have landed.
- `GET /v1/cost/session/{key}` is what commit trailers read (spec 6.1, `provenance.include_usage`), **while the session is still open** — so it will frequently return `complete: false`. See the trailer decision in "Owner decisions" below: the session must not write a cost it does not know.
- `GET /v1/cost/project/{project}` adds `window` (`{"start":…,"end":…}`), `budget`, `remaining`, `by_trigger`, and `stale: bool`.

---

## Open questions

Four of the original thirteen are **settled** by the controller and are now Decisions 9–12 above: the ledger backend (Dolt), the spend source (LiteLLM's API), token budgets (synthetic pricing), and secrets (file mounts, two slots). Of the rest, each is either an **assumed default** the executor must build (and which the owner may overturn cheaply) or a genuine **owner decision** that needs a fact only the owner has.

### Assumed defaults (build these; revisit if wrong)

**AD-1 — Virtual-key delivery: meter writes a per-project Kubernetes Secret and returns only a `key_ref`.**
The alternative — returning the raw token in the `/decide` response — puts a live credential into the Gas City event bus and into every log line that echoes a decision. That is not a close call.

> **OWNERSHIP, SETTLED (this was a real gap; both plans pointed at each other).**
> Earlier drafts of this plan said *"Plan 05 must implement the k8s Secret sink
> (+ RBAC) or meter cannot deliver keys to pods at all"* — i.e. Plan 03 declared a
> hard dependency on a component **no plan specified**, and Plan 05 then wrote the
> Go code for a package Plan 03 owns. That is two plans owning one file. Split on
> the natural seam:
>
> | Artifact | Owner |
> |---|---|
> | `internal/meter/keysink/keysink.go` — the `KeySink` interface, `Memory`, `Slug` | **Plan 03**, Task 7 Step 4 |
> | **`internal/meter/keysink/k8s.go` + `k8s_test.go` — the Kubernetes sink** | **Plan 03**, Task 7 **Step 4b** (NEW). It is Go, it is `internal/meter/`, and it is tested against a fake clientset with no cluster. |
> | `GONK_KEYSINK_NAMESPACE` / `GONK_KEYSINK_PREFIX` wiring in `cmd/gonk-meter` | **Plan 03**, Task 8 Step 5 |
> | **`role-gonk-meter.yaml`, `rolebinding-gonk-meter.yaml`, `serviceaccount-gonk-meter.yaml`, and the chart values that set the env** | **Plan 05**, Task 4 |
> | Proving the RBAC actually permits the writes on a real API server | **Plan 06** |
>
> **Neither half works alone.** Without Plan 03's `k8s.go`, meter has no sink;
> without Plan 05's Role, the sink gets a `403` on every `Put` and every project
> sits in `key-missing` forever — which is fail-*closed*, and loud, but it is
> still gonk not working. Plan 05 **cites** this table; it does not restate it.

*Blast radius if wrong:* small, and it is now a wiring question rather than a missing component. If `GONK_KEYSINK_NAMESPACE` is unset, meter falls back to `keysink.NewMemory()` **and logs a warning at `ERROR` level that keys are not being delivered to pods** — never silently.

**AD-2 — Meter API auth: a bearer token from a file-mounted Secret (two rotation slots, constant-time compare) *and* a NetworkPolicy.** Both, not either.
Anything that can call `/decide` can mint attribution tags and obtain a `key_ref`. A NetworkPolicy alone fails open the moment something else lands in the namespace; a token alone fails open if it leaks.
*Blast radius if wrong:* small — dropping one of the two layers is a config change, not a code change.

**AD-3 — The rung catalog lives in the operator config (`rungs:` in `gonk-city`), not derived from LiteLLM's `/model/info`.**
It has to agree with LiteLLM's model list either way; putting it next to the LiteLLM model list in the same repo is the version of that agreement a human can actually check. It is also where the **synthetic prices** must live (Decision 9), and those have no source in `/model/info`.
*Blast radius if wrong:* the catalog drifts from LiteLLM's real model list, `EnsureKey` names a model LiteLLM does not have, key provisioning fails, the project sits in `key-missing`, and `/decide` **defers**. It fails closed. Mitigation: meter fetches `/model/info` at startup and **logs + increments `gonk_meter_catalog_drift_total`** on any mismatch, but does **not** refuse to start (a LiteLLM outage must not prevent meter from booting). Plan 06 verifies the real list.

**AD-4 — `enforce_ladder_order: true` by default**, and `opercfg.Load` refuses to start when it is on with no instance ladder to order against.
`gonkcfg.Resolve` preserves the *project's* ladder order, so a project could list `[sonnet, qwen-local]` and make a cloud rung its **first** attempt — while spec 6.3 says "everything starts at the cheapest rung its project config allows". A project's ladder must be a **subsequence** of the instance's.
*Blast radius if wrong:* if the owner wants projects free to reorder, flip one boolean. If we left it *off* and were wrong, every project could opt straight into its most expensive rung.

**AD-5 — The trigger→action map** (`/decide`'s veto surface; the spec never states it):

| Trigger | Gating action |
|---|---|
| `issue-triage` | `Actions.Triage` |
| `mention-reply` | `Actions.Triage` |
| `scaffold` | `Actions.Triage` — the `.agent/` scaffold MR is "authorized by the just-merged config" (spec 5.3), and `triage: true` is the only thing the conservative default `.gonk.yml` turns on |
| `onboarding` | **none** — the onboarding MR is deterministic and spends zero tokens (spec 5.3); it must work on a project that has opted into nothing yet |

*Blast radius if wrong:* `scaffold` is the risky row. If it should *not* be gated by `Actions.Triage`, a project that opted into nothing still gets a metered session. If it should be gated by something else, a project that opted into triage never gets its scaffold MR and sits in `pending` forever. Both are visible immediately in `gonk_meter_policy_decisions_total{reason="action-not-allowed"}`.

**AD-6 — Ladder exhaustion labels the bead for a human; it does not close it.**
On `deny`/`ladder-exhausted`, the pack labels the issue `gonk::needs-human` and stops. A silently-closed bead is work that vanished.
*Blast radius if wrong:* cosmetic; it is one label name. **Carried to Plan 04** (the bead lifecycle is the pack's).

**AD-7 — Cost in commit trailers: when `complete` is false, do not write a number.**
`provenance.include_usage: true` makes the session write `Gonk-Tokens:`/`Gonk-Cost-USD:` trailers sourced from `/v1/cost/session/{key}` **at commit time — while the session is still open**, so the spend rows for its own last calls have not landed and `complete` will usually be `false`. Writing the number anyway publishes a **wrong cost into permanent git history**.
*Assumed default:* if `complete: false`, the session omits the two usage trailers and writes `Gonk-Usage: pending` instead. The authoritative number is always the cost API.
*Blast radius if wrong:* if the owner would rather have an approximate number than none, flip it — but then the trailer must say it is approximate. **Carried to Plan 04.**

**AD-8 — Ledger retention: 13 months**, pruned by a daily job (spend rows, settled reservations, attempt history).
Twelve months of dashboards plus the current window. Dolt makes this cheap and the history is the audit trail.
*Blast radius if wrong:* the store grows or shrinks. Trivially tunable. The alternative we must **not** ship is what the plan had before: unbounded growth.

**AD-9 — LiteLLM key `budget_duration: "1mo"`**, to match meter's UTC calendar month.
*Blast radius if wrong — and this one is real:* if LiteLLM's `1mo` turns out to be a **rolling 30 days** rather than a calendar month, the soft door (meter) and the hard door (LiteLLM) reset on **different days**, and there is a window each month where LiteLLM refuses work that meter thinks is affordable (or, worse, permits work meter has already stopped counting). **Plan 06 must verify this against the pinned LiteLLM version.** It is listed there.

**AD-10 — Meter runs single-replica** (`replicas: 1`, `strategy: Recreate`).
The per-project `keyedMutex` that makes `read-spend → decide → reserve` atomic is **in-process**. It does nothing across pods. Until Task 0b proves the store's isolation is sufficient on its own, a second replica is a budget escape.
*Blast radius if wrong:* meter is a small stateless-ish service on the decision path, not the request path; a single replica costs a few seconds of unavailability on a rolling update, during which `/decide` is unreachable and the pack retries. That is the correct trade. **Carried to Plan 05.**

### Owner decision needed (a fact only you have)

**OD-A — The LiteLLM admin credential: which credential, and where does it come from?**
*Settled:* it is a **file mount** from a k8s Secret synced out of Vault/1Password, two rotation slots, never an env var, never committed (Decision 12). *Assumed default:* a **dedicated admin key**, not LiteLLM's master key — the master key's blast radius is the entire proxy.
*Still needed:* the 1Password item / Vault path, and confirmation that a dedicated admin key on the pinned LiteLLM version can actually call `/key/generate`, `/key/update`, `/key/delete`, and `/spend/logs`. If it cannot, we are forced onto the master key and should know that now.

**OD-B — Reservation estimates and TTL: the numbers.**
The *mechanism* is fixed by this plan. The *values* are yours: per-rung `est_cost_usd` and `est_tokens` (what one attempt is expected to consume) and `reservation_ttl` (default 60m). Too low and concurrent sessions overshoot the soft ceiling; too high and the factory stalls holding budget for sessions that already finished.
The example configs in this plan carry **placeholders**, and `ADR-004` records that reservations are estimates: a session that consumes far more than its rung's estimate overshoots by the difference, and LiteLLM's hard USD ceiling is what stops it. Alert on `actual >> estimated`.

**OD-C — The synthetic prices for local rungs.**
Decision 9 requires every local rung to carry `synthetic_usd_per_1m_tokens > 0`, configured identically in the operator config *and* in LiteLLM's price map. What should they be? A price near a real cloud model's makes the synthetic dollars comparable and the dashboards intuitive; a price far below it makes the hard door very loose. There is no defensible default — it depends on how you want to read the Cost dashboard.
*Where the number has to be typed, on the deployed instance:* the **gitops** repo, `clusters/orac/apps/litellm/litellm.yaml`, under `spec.values.litellm_settings.model_cost_map`. **Local models are priced at ZERO there today** — which is exactly why `monthly_tokens` currently has no hard door anywhere. Setting them is a gitops MR, not a gonk change.

**OD-D — The canonical local rung name.** The onboarding template ships `ladder: [qwen-local]` (spec 5.4's example, Plan 02's default `.gonk.yml`). That string must **exactly** match a LiteLLM model name **and** appear in the instance ladder, or ADR-002's empty-ladder rule disables every freshly-onboarded project. Is `qwen-local` the real name?
*Where the answer lives:* the **gitops** repo, `clusters/orac/apps/litellm/litellm.yaml`, under `spec.values.proxy_config.model_list` — read the `model_name` values. Do not guess. (Shared with Plan 02's OD-B — same question, one answer.)

---

### Task 0: `pkg/meterapi` — the shared wire contract (NORMATIVE)

**This task defines the single source of truth for every type and endpoint shared between gonk-meter, gitlab-intake (Plan 02), and the pack (Plan 04).** The Go source below is normative. Plan 02 lands it verbatim (it executes first); this task verifies it is byte-identical and adds the drift gate. **A field added, renamed, or retyped anywhere else is a bug.**

**Files:** Create `pkg/meterapi/meterapi.go`, `pkg/meterapi/meterapi_test.go`, `pkg/meterapi/testdata/contract.sha256`

- [ ] **Step 1: Write `pkg/meterapi/meterapi.go`** (or, if Plan 02 already landed it, diff it against this and reconcile *here*)

```go
// Package meterapi is the wire contract between gonk-meter (the server),
// gitlab-intake (the config client), and the Gas City pack (the policy client).
// All three import it; NONE of them re-derives the JSON shape.
//
// Ownership: gonk-meter (Plan 03) owns this contract. gitlab-intake conforms to
// it. If a client needs a field that is not here, that is a change to the METER
// contract and must be made here first, with the sha256 drift gate updated in
// the same commit.
//
// Endpoints (see docs/api/gonk-meter-v1.md):
//
//	PUT    /v1/projects/{project}            ProjectRequest  -> ProjectResponse   (intake)
//	GET    /v1/projects/{project}                            -> ProjectResponse   (intake)
//	DELETE /v1/projects/{project}                            -> 204               (intake)
//	POST   /v1/projects/{project}/key/rotate                 -> ProjectResponse   (operator)
//	POST   /v1/policy/decide                 DecideRequest   -> DecideResponse    (intake Gate 1 + pack Gate 2; idempotent on an open reservation)
//	POST   /v1/policy/outcome                OutcomeRequest  -> OutcomeResponse    (pack)
//	GET    /v1/cost/bead/{bead_id}                           -> BeadCostResponse
//	GET    /v1/cost/session/{session_key}                    -> SessionCostResponse
//	GET    /v1/cost/project/{project}                        -> ProjectCostResponse
//	GET    /v1/cost/instance                                 -> InstanceCostResponse
//	GET    /healthz | /readyz | /metrics                     (unauthenticated)
//
// Everything except the health/metrics endpoints requires
// `Authorization: Bearer <token>`.
package meterapi

import (
	"fmt"
	"math"
	"net/url"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// ---------------------------------------------------------------- paths

// ProjectPath builds the project path. {project} is the GitLab
// path_with_namespace, URL-path-escaped: "group/repo" -> "group%2Frepo".
// Never hand-build this: an unescaped "/" silently routes to a different (or no)
// handler.
func ProjectPath(project string) string {
	return "/v1/projects/" + url.PathEscape(project)
}

func ProjectKeyRotatePath(project string) string { return ProjectPath(project) + "/key/rotate" }

func CostBeadPath(beadID string) string      { return "/v1/cost/bead/" + url.PathEscape(beadID) }
func CostSessionPath(sessionKey string) string { return "/v1/cost/session/" + url.PathEscape(sessionKey) }
func CostProjectPath(project string) string  { return "/v1/cost/project/" + url.PathEscape(project) }

const (
	DecidePath      = "/v1/policy/decide"
	OutcomePath     = "/v1/policy/outcome"
	CostInstancePath = "/v1/cost/instance"
	HealthzPath     = "/healthz"
	ReadyzPath      = "/readyz"
	MetricsPath     = "/metrics"
)

// ---------------------------------------------------------------- budget

// Budget is an EffectiveBudget, wire-shaped. NULL MEANS UNLIMITED.
//
// gonkcfg encodes "unlimited" as math.Inf(1) (cost) and math.MaxInt64 (tokens)
// -- ADR-002. Neither survives contact with JSON: encoding/json REFUSES to
// marshal +Inf (it returns an error, not a number), so an unlimited project
// would fail to serialize AT ALL; and MaxInt64 loses precision in any
// float64-based parser, which is every JavaScript client, so a "limit" silently
// becomes a DIFFERENT limit. `null` is unambiguous, symmetric across both types,
// and round-trips.
//
// NOBODY may marshal gonkcfg.EffectiveBudget directly. This type is the only
// correct way across the wire.
type Budget struct {
	MonthlyCostUSD *float64 `json:"monthly_cost_usd"`
	MonthlyTokens  *int64   `json:"monthly_tokens"`
	PerTaskTokens  *int64   `json:"per_task_tokens"`
}

const unlimitedTokens = gonkcfg.TokenQuantity(math.MaxInt64)

// BudgetFrom converts a resolved budget for the wire. It errors on a non-finite
// cost ceiling: Resolve pins those to 0 and disables the project, so this is
// unreachable in practice -- but the seam does not trust that, because an
// unvalidated operator policy is exactly the gap ADR-002 leaves open.
func BudgetFrom(b gonkcfg.EffectiveBudget) (Budget, error) {
	var out Budget
	switch {
	case math.IsInf(b.MonthlyCostUSD, 1):
		// unlimited -> null
	case math.IsNaN(b.MonthlyCostUSD) || math.IsInf(b.MonthlyCostUSD, -1):
		return Budget{}, fmt.Errorf("meterapi: monthly_cost_usd is not a finite number (%v)", b.MonthlyCostUSD)
	default:
		v := b.MonthlyCostUSD
		out.MonthlyCostUSD = &v
	}
	if b.MonthlyTokens != unlimitedTokens {
		v := int64(b.MonthlyTokens)
		out.MonthlyTokens = &v
	}
	if b.PerTaskTokens != unlimitedTokens {
		v := int64(b.PerTaskTokens)
		out.PerTaskTokens = &v
	}
	return out, nil
}

// Effective is the inverse: null becomes the unlimited sentinels again.
func (b Budget) Effective() gonkcfg.EffectiveBudget {
	out := gonkcfg.EffectiveBudget{
		MonthlyCostUSD: math.Inf(1),
		MonthlyTokens:  unlimitedTokens,
		PerTaskTokens:  unlimitedTokens,
	}
	if b.MonthlyCostUSD != nil {
		out.MonthlyCostUSD = *b.MonthlyCostUSD
	}
	if b.MonthlyTokens != nil {
		out.MonthlyTokens = gonkcfg.TokenQuantity(*b.MonthlyTokens)
	}
	if b.PerTaskTokens != nil {
		out.PerTaskTokens = gonkcfg.TokenQuantity(*b.PerTaskTokens)
	}
	return out
}

// ZeroBudget is the fail-closed budget: nothing is affordable. It is what an
// INVALID project gets. Never send an empty Budget{} for a project you are
// disabling -- an empty Budget{} is all-nil, and nil means UNLIMITED.
func ZeroBudget() Budget {
	zero, zeroTokens := 0.0, int64(0)
	return Budget{MonthlyCostUSD: &zero, MonthlyTokens: &zeroTokens, PerTaskTokens: &zeroTokens}
}

// ---------------------------------------------------------------- project state

// State is a project's registration state AS DETERMINED BY METER. Intake
// records it; intake cannot compute it (only meter resolves config).
type State string

const (
	StateActive     State = "active"      // resolved, enabled, virtual key provisioned
	StateDisabled   State = "disabled"    // resolved, Effective.Enabled == false
	StateInvalid    State = "invalid"     // .gonk.yml would not load; there is NO Effective
	StateKeyMissing State = "key-missing" // resolved and enabled, but LiteLLM key not provisioned yet
)

// KeyRef points at WHERE a LiteLLM virtual key lives. It NEVER contains the key
// itself: a token in an API response ends up in the event bus and in every log
// line that echoes it.
type KeyRef struct {
	SecretName string `json:"secret_name"`
	SecretKey  string `json:"secret_key"`
}

type Actions struct {
	Triage    bool `json:"triage"`
	Pipelines bool `json:"pipelines"`
	Features  bool `json:"features"`
}

type Triage struct {
	LabelPrefix       string `json:"label_prefix"`
	RespondToMentions bool   `json:"respond_to_mentions"`
}

type Provenance struct {
	CommitTrailers bool `json:"commit_trailers"`
	IncludeUsage   bool `json:"include_usage"`
}

type Schedule struct {
	QuietHours string `json:"quiet_hours"`
	Timezone   string `json:"timezone"`
}

// Effective is gonkcfg.Effective, wire-shaped. It is produced by exactly ONE
// call site in the entire system: gonk-meter's service.Register, which calls
// gonkcfg.Resolve. Per ADR-002, Resolve's return value is the only legitimate
// way to produce an Effective -- so intake reads this off the wire and NEVER
// computes it.
//
// Note the ABSENCE of Budget: the budget lives on ProjectResponse, because a
// disabled or invalid project has a budget (zero) but no meaningful Effective.
type Effective struct {
	Enabled    bool       `json:"enabled"`
	Actions    Actions    `json:"actions"`
	Ladder     []string   `json:"ladder"`
	Continuity string     `json:"continuity"`
	Triage     Triage     `json:"triage"`
	Provenance Provenance `json:"provenance"`
	Schedule   *Schedule  `json:"schedule"`
}

// EffectiveFrom is meter-side only: it projects a resolved gonkcfg.Effective
// onto the wire type.
func EffectiveFrom(e gonkcfg.Effective) Effective {
	out := Effective{
		Enabled:    e.Enabled,
		Actions:    Actions{Triage: e.Actions.Triage, Pipelines: e.Actions.Pipelines, Features: e.Actions.Features},
		Ladder:     append([]string(nil), e.Ladder...),
		Continuity: e.Continuity,
		Triage:     Triage{LabelPrefix: e.Triage.LabelPrefix, RespondToMentions: e.Triage.RespondToMentions},
		Provenance: Provenance{CommitTrailers: e.Provenance.CommitTrailers, IncludeUsage: e.Provenance.IncludeUsage},
	}
	if e.Schedule != nil {
		out.Schedule = &Schedule{QuietHours: e.Schedule.QuietHours, Timezone: e.Schedule.Timezone}
	}
	return out
}

// ---------------------------------------------------------------- PUT/GET/DELETE /v1/projects/{project}

// ProjectRequest is an idempotent upsert of one project, FROM ITS RAW .gonk.yml.
//
// Intake sends the BYTES IT READ FROM GITLAB, verbatim. It does NOT resolve them
// -- meter is the only component that holds operator (instance/group) policy, and
// two independent resolvers would mean two sources of truth for a budget ceiling.
// Meter validates, folds the nested-group-aware group policy over the instance
// policy, calls gonkcfg.Resolve, and owns the resulting Effective.
type ProjectRequest struct {
	Project         string `json:"project"`           // GitLab path_with_namespace
	ProjectID       int64  `json:"project_id"`
	Rig             string `json:"rig"`
	DefaultBranch   string `json:"default_branch"`
	ConfigCommitSHA string `json:"config_commit_sha"` // commit .gonk.yml was read at; "" if unknown
	GonkYML         string `json:"gonk_yml"`          // RAW .gonk.yml bytes. Meter is the validator.
}

// ProjectResponse is meter's answer, and the ONLY legitimate source of a
// project's effective policy for any other component.
//
// Invariants (asserted by the golden tests, and relied on by intake):
//   - Effective is nil IF AND ONLY IF State == StateInvalid. An invalid config
//     has no Effective; per ADR-002 a zero-value Effective must never be treated
//     as a resolved one.
//   - Error is non-empty IF AND ONLY IF State == StateInvalid.
//   - DisabledReason is non-empty IF AND ONLY IF State == StateDisabled
//     (ADR-002's invariant, carried onto the wire).
//   - KeyRef is populated IF AND ONLY IF State == StateActive.
//   - Budget is ZeroBudget() when State is Invalid or Disabled. NEVER an empty
//     Budget{} -- that is all-nil, and nil means UNLIMITED.
type ProjectResponse struct {
	Project        string     `json:"project"`
	Rig            string     `json:"rig"`
	State          State      `json:"state"`
	DisabledReason string     `json:"disabled_reason"`
	Error          string     `json:"error,omitempty"`
	Effective      *Effective `json:"effective"`
	Budget         Budget     `json:"budget"`
	KeyRef         KeyRef     `json:"key_ref"`
	ConfigHash     string     `json:"config_hash"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ---------------------------------------------------------------- POST /v1/policy/decide

// Decision kinds. defer and deny are HTTP 200: they are normal answers.
const (
	DecisionRun   = "run"
	DecisionDefer = "defer"
	DecisionDeny  = "deny"
)

// Machine-readable decision reasons. A BOUNDED set on purpose: they are
// Prometheus label values and part of this contract.
const (
	ReasonNotRegistered          = "project-not-registered"
	ReasonDisabled               = "disabled"
	ReasonInvalidConfig          = "invalid-config"
	ReasonActionNotAllowed       = "action-not-allowed"
	ReasonKeyMissing             = "virtual-key-missing"
	ReasonQuietHours             = "quiet-hours"
	ReasonSpendStale             = "spend-data-stale"
	ReasonLadderExhausted        = "ladder-exhausted"
	ReasonInfraRetriesExhausted  = "infra-retries-exhausted"
	ReasonPerTaskTokensExhausted = "per-task-tokens-exhausted"
	ReasonMonthlyTokensExhausted = "monthly-tokens-exhausted"
	ReasonMonthlyCostExhausted   = "monthly-cost-exhausted"
)

// DecideRequest asks meter which rung the next attempt runs at.
//
// THERE IS DELIBERATELY NO ATTEMPT COUNT AND NO PRIOR-OUTCOME LIST, and there
// never will be. Meter owns ladder state, reading it from its own store. A
// caller-supplied attempt number is a forgery vector: raise it and you skip
// straight to the most expensive rung. Any client that tries to send one is
// wrong; any server that reads one is a bug.
type DecideRequest struct {
	Project    string `json:"project"`
	Rig        string `json:"rig"`
	BeadID     string `json:"bead_id"`
	SessionKey string `json:"session_key"`
	Trigger    string `json:"trigger"` // an atags.Trigger* value
}

// DecideResponse. On a defer or a deny, Metadata is empty, KeyRef is zero, and
// ReservationID is empty: a decision that is not going to run hands out neither
// an attribution identity nor a route to a credential.
type DecideResponse struct {
	Decision string `json:"decision"` // run | defer | deny
	Rung     string `json:"rung"`
	Model    string `json:"model,omitempty"`
	Attempt  int    `json:"attempt"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail"`
	// RetryAfter is set on EVERY defer. A defer with no retry_after is an
	// infinite park.
	RetryAfter time.Time `json:"retry_after,omitzero"`
	// Metadata is atags.Metadata(). METER mints it, so meter is the single
	// boundary where tag values are charset-validated. The pack stamps it
	// verbatim onto every LiteLLM request.
	Metadata             map[string]string `json:"metadata"`
	KeyRef               KeyRef            `json:"key_ref"`
	ReservationID        string            `json:"reservation_id,omitempty"`
	ReservationExpiresAt time.Time         `json:"reservation_expires_at,omitzero"`
	Budget               Budget            `json:"budget"`
	// Remaining is REAL dollars and real tokens. Synthetic local-model dollars
	// are not spend and never appear here.
	Remaining Budget    `json:"remaining"`
	SpendAsOf time.Time `json:"spend_as_of"`
}

// ---------------------------------------------------------------- POST /v1/policy/outcome

// Outcome values. Only OutcomeGateFailed escalates the ladder.
const (
	OutcomeSuccess     = "success"
	OutcomeGateFailed  = "gate-failed"
	OutcomeInfraFailed = "infra-failed"
	OutcomeAborted     = "aborted"
)

// OutcomeRequest reports how one attempt ended. ReservationID binds the outcome
// to a reservation METER minted: without it, `gate-failed` is a forgery vector
// (repeat it and a project walks itself up to its most expensive rung).
type OutcomeRequest struct {
	Project       string `json:"project"`
	BeadID        string `json:"bead_id"`
	SessionKey    string `json:"session_key"`
	Attempt       int    `json:"attempt"`
	Rung          string `json:"rung"`
	ReservationID string `json:"reservation_id"`
	Outcome       string `json:"outcome"`
}

// OutcomeResponse. Next/NextRung are ADVISORY -- a preview for logs and
// dashboards. The authoritative answer is always the next /decide.
type OutcomeResponse struct {
	OK              bool   `json:"ok"`
	RecordedAttempt int    `json:"recorded_attempt"`
	Next            string `json:"next"`      // escalate | retry | done
	NextRung        string `json:"next_rung"`
}

// ---------------------------------------------------------------- cost

// RungCost is one rung's slice of a total.
//
// CostUSD and SyntheticCostUSD are DIFFERENT CURRENCIES and a client must never
// add them. Local rungs cost no real money; they are priced synthetically in
// LiteLLM so that the USD virtual-key ceiling is a hard door for tokens too. A
// synthetic dollar is an accounting unit, NOT SPEND. Present it labelled or not
// at all.
type RungCost struct {
	Rung             string  `json:"rung"`
	Kind             string  `json:"kind"` // local | cloud
	CostUSD          float64 `json:"cost_usd"`
	SyntheticCostUSD float64 `json:"synthetic_cost_usd"`
	CostSynthetic    bool    `json:"cost_synthetic"` // true iff Kind == "local"
	TotalTokens      int64   `json:"total_tokens"`
	Calls            int     `json:"calls"`
}

type TriggerCost struct {
	Trigger          string  `json:"trigger"`
	CostUSD          float64 `json:"cost_usd"`
	SyntheticCostUSD float64 `json:"synthetic_cost_usd"`
	TotalTokens      int64   `json:"total_tokens"`
	Calls            int     `json:"calls"`
}

type AttemptView struct {
	Attempt int    `json:"attempt"`
	Rung    string `json:"rung"`
	Outcome string `json:"outcome"`
}

type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// BeadCostResponse: lifetime, not windowed. The per-task ceiling is a property
// of the WORK ITEM, so a bead that straddles a month boundary keeps the same
// per-task budget.
type BeadCostResponse struct {
	BeadID           string        `json:"bead_id"`
	Project          string        `json:"project"`
	CostUSD          float64       `json:"cost_usd"`
	SyntheticCostUSD float64       `json:"synthetic_cost_usd"`
	PromptTokens     int64         `json:"prompt_tokens"`
	CompletionTokens int64         `json:"completion_tokens"`
	TotalTokens      int64         `json:"total_tokens"`
	ByRung           []RungCost    `json:"by_rung"`
	Attempts         []AttemptView `json:"attempts"`
	AsOf             time.Time     `json:"as_of"`
	// Complete is false iff an open reservation exists for this scope: spend
	// rows for it may not have landed. A client that publishes a cost (a commit
	// trailer!) MUST NOT publish one when Complete is false.
	Complete bool `json:"complete"`
}

type SessionCostResponse struct {
	SessionKey       string     `json:"session_key"`
	Project          string     `json:"project"`
	BeadID           string     `json:"bead_id"`
	CostUSD          float64    `json:"cost_usd"`
	SyntheticCostUSD float64    `json:"synthetic_cost_usd"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	TotalTokens      int64      `json:"total_tokens"`
	ByRung           []RungCost `json:"by_rung"`
	AsOf             time.Time  `json:"as_of"`
	Complete         bool       `json:"complete"`
}

type ProjectCostResponse struct {
	Project          string        `json:"project"`
	Window           Window        `json:"window"`
	CostUSD          float64       `json:"cost_usd"`
	SyntheticCostUSD float64       `json:"synthetic_cost_usd"`
	PromptTokens     int64         `json:"prompt_tokens"`
	CompletionTokens int64         `json:"completion_tokens"`
	TotalTokens      int64         `json:"total_tokens"`
	ByRung           []RungCost    `json:"by_rung"`
	ByTrigger        []TriggerCost `json:"by_trigger"`
	Budget           Budget        `json:"budget"`
	Remaining        Budget        `json:"remaining"`
	AsOf             time.Time     `json:"as_of"`
	Complete         bool          `json:"complete"`
	Stale            bool          `json:"stale"`
}

type InstanceCostResponse struct {
	Window           Window                `json:"window"`
	CostUSD          float64               `json:"cost_usd"`
	SyntheticCostUSD float64               `json:"synthetic_cost_usd"`
	TotalTokens      int64                 `json:"total_tokens"`
	ByProject        []ProjectCostResponse `json:"by_project"`
	AsOf             time.Time             `json:"as_of"`
	Complete         bool                  `json:"complete"`
}

// ---------------------------------------------------------------- errors

// ErrorResponse is the body of a 400 (a malformed REQUEST) and of a 500.
//
// It is NOT the body of a 422: a 422 means the project's .gonk.yml would not
// load, which is a successful, idempotent registration of an invalid config, and
// it returns a full ProjectResponse with State == StateInvalid and Error set.
type ErrorResponse struct {
	Error string `json:"error"`
}
```

- [ ] **Step 2: Write `pkg/meterapi/meterapi_test.go` — the LITERAL contract**

The literal JSON field names **are** the contract with intake and the pack (spec 10.1). A symmetric round-trip test would happily accept a rename on both sides at once; this must not.

```go
// Unlimited must come out as null, both ways. This is PLAN.md's highest-value
// carry-forward: json.Marshal(math.Inf(1)) RETURNS AN ERROR, so an unlimited
// project would fail to serialize at all.
func TestUnlimitedBudgetSerializesAsNull(t *testing.T)
// Proof of the hazard first: assert json.Marshal of the raw +Inf float ERRORS.
// Then BudgetFrom -> `{"monthly_cost_usd":null,"monthly_tokens":null,"per_task_tokens":null}`.

func TestZeroBudgetIsNotAnEmptyBudget(t *testing.T)
// ZeroBudget() marshals to `{"monthly_cost_usd":0,...}` -- NOT to nulls.
// An empty Budget{} means UNLIMITED, which for an invalid project is the exact
// opposite of fail-closed. This test is the guardrail.

func TestBudgetFromRejectsNonFinite(t *testing.T)          // NaN, -Inf -> error
func TestFiniteBudgetRoundTrips(t *testing.T)              // incl. an explicit 0 ceiling (a REAL ceiling, ADR-002)

func TestDecideRequestHasNoAttemptField(t *testing.T)
// Marshal a DecideRequest and assert the JSON contains NO "attempt" key, and
// reflect over the struct asserting no field named Attempt. A caller-supplied
// attempt count is a forgery vector for climbing the ladder (Decision 2), and
// this test is what stops someone "helpfully" adding one.

func TestProjectPathEscapes(t *testing.T)
// ProjectPath("group/repo") == "/v1/projects/group%2Frepo". An unescaped slash
// routes to a different handler, or to none.

func TestWireContractLiterals(t *testing.T)
// Marshal one fully-populated value of EVERY request/response type and compare
// BYTE FOR BYTE against testdata/golden/*.json. A field rename fails CI.
```

- [ ] **Step 3: Add the drift gate**

```bash
sha256sum pkg/meterapi/meterapi.go | cut -d' ' -f1 > pkg/meterapi/testdata/contract.sha256
```

```go
// TestContractIsFrozen fails on ANY edit to meterapi.go. That is the point: this
// file is a contract between three plans, and a change to it is a change to all
// three. Update the checksum in the SAME commit that updates the clients, and
// say so in the message.
func TestContractIsFrozen(t *testing.T) { /* sha256 of the source vs testdata/contract.sha256 */ }
```

- [ ] **Step 4: Run it, gate, commit**

```bash
go test ./pkg/meterapi/ -race -count=1 -v
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add pkg/meterapi && git commit -m "feat(meterapi): the intake<->meter<->pack wire contract (owned by plan 03)"
```

---

### Task 0b: Verify Dolt's transactional guarantees BEFORE anything depends on them

**This is a blocking spike, and it is the single riskiest assumption in the plan.** Decision 10 chose Dolt because it is already run for the beads store. That reasoning is about *dependencies*, not about *isolation* — and the reservation path needs real isolation, because two concurrent sessions must not both win the last of a budget.

**Do not write `store/dolt.go` before this task passes. Do not "get to it later".** If Dolt cannot give us what we need, we want to know now, while the `Store` interface is still the only thing anything depends on.

**Files:** Create `internal/meter/store/dolt_race_test.go` (a `//go:build dolt`-tagged integration test), `docs/spikes/dolt-reservation-isolation.md`

- [ ] **Step 1: State the property being tested, precisely**

> Given a budget ceiling with headroom for exactly **K** reservations, when **N ≫ K** concurrent writers each (a) read the sum of open reservations, (b) decide whether their reservation fits, and (c) write it — **exactly K succeed, and the sum of persisted reservations never exceeds the ceiling.**

This is the `read-then-write` race, and it is the *only* thing that stands between a budget and two sessions spending it twice. It is not a durability question and it is not a "does Dolt work" question.

- [ ] **Step 2: Write the racing test**

```go
//go:build dolt

// Run against a real dolt sql-server:
//   dolt sql-server --port 3307 &
//   go test -tags dolt ./internal/meter/store/ -race -run Race -count=5 -v
//
// -count=5 is not decoration: a race that only manifests one run in three is
// still a race, and a budget escape that happens once a month is still a budget
// escape.
func TestDoltReservationRaceDoesNotOverspend(t *testing.T) {
	s := newDoltStore(t)                       // fresh schema per run
	const ceiling = 1.00                       // dollars
	const each = 0.40                          // one glm attempt
	const N = 32                               // writers
	// headroom for exactly 2.

	var wins atomic.Int64
	var wg sync.WaitGroup
	for i := range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// EXACTLY the sequence Service.Decide performs, with NO in-process
			// lock -- we are testing the STORE, not the mutex.
			ok, err := s.ReserveIfFits(ctx, "p", ceiling, each, mkReservation(i))
			if err != nil { /* a serialization failure is a legitimate LOSS, not an error */ }
			if ok { wins.Add(1) }
		}()
	}
	wg.Wait()

	if got := wins.Load(); got != 2 {
		t.Fatalf("%d writers won against headroom for 2; a budget ceiling is raceable", got)
	}
	total := sumOpenReservations(t, s, "p")
	if total > ceiling {
		t.Fatalf("persisted reservations total $%.2f against a $%.2f ceiling: OVERSPEND", total, ceiling)
	}
}
```

**Note the shape of the fix this forces.** The check and the write must be **one atomic step in the store**, not two calls the service sandwiches in a mutex — otherwise the guarantee evaporates the moment there is a second replica. So the `Store` interface gains:

```go
// ReserveIfFits atomically re-reads the project's open reservations, checks that
// r still fits under the ceiling, and writes r -- or reports that it does not
// fit. It is the ONLY way a reservation may be created.
//
// This is a store method rather than a service method on purpose: the atomicity
// has to live where the data lives. A service-side "read, check, write" under an
// in-process mutex is correct for exactly one replica and silently wrong for two.
ReserveIfFits(ctx context.Context, project string, ceiling budget.Budget, want budget.Spend, r Reservation) (bool, error)
```

Retrofit this into Task 6's interface and into the `Memory` store (where it is trivially correct under its existing mutex). **The service's per-project `keyedMutex` stays** — it is a cheap contention optimization and it keeps `Decide` deterministic within one process — but it is no longer what makes the ceiling safe. **The store is.**

- [ ] **Step 3: Run it, and believe the result**

Expected outcomes, and what each one means:

| Result | Meaning | What we do |
|---|---|---|
| Exactly 2 win, every run, `-count=5` | Dolt gives us what we need. | Proceed. Record the isolation level and the Dolt version in the spike doc. |
| More than 2 win | **Dolt's default isolation does not serialize this.** | Retry with an explicit `SELECT ... FOR UPDATE` / `SERIALIZABLE`. If that fixes it, that is the implementation, and the spike doc says so. |
| More than 2 win even under `SERIALIZABLE` | Dolt cannot back the reservation path. | **Fall back.** See Step 4. |
| Fewer than 2 win, or spurious errors | Over-serialization / lock contention. | Tune, or treat serialization failures as `defer` (a lost race is a legitimate defer, not an error). |

- [ ] **Step 4: Write down the fallback BEFORE you need it** — `docs/spikes/dolt-reservation-isolation.md`

Two fallbacks, in preference order:

1. **Single-writer path per project.** Keep Dolt for durability and the audit history, and make meter authoritative for serialization by keeping it **single-replica** (AD-10) — the in-process `keyedMutex` then *is* the isolation, and the store only needs to not lose writes. This is cheap, it is where the plan already is, and it costs us horizontal scaling of a component that does not need it. **This is the recommended fallback.**
2. **Postgres for the ledger (owner-approved, 2026-07-13).** `SERIALIZABLE` / `SELECT FOR UPDATE` is well-trodden for exactly this race. **The owner already runs CloudNativePG (CNPG) in-cluster, so this is NOT a new stateful dependency** — provision a CNPG `Cluster` and point `internal/meter/store.Store` at it; nothing above that seam changes. The only real cost is losing Dolt's versioned audit history, which the append-only ledger tables mitigate. Take this fallback without hesitation if the Task 0b spike shows Dolt cannot serialize the reservation race, or if it is inconclusive — a budget ceiling defended by an unproven isolation guarantee is not defended.

Record in the doc: the Dolt version tested, the isolation level, the observed result, and which fallback (if any) is now in force. **`ADR-004` cites this document.** If the spike is skipped or inconclusive, that fact goes in the ADR in as many words — an unverified transactional guarantee under a budget ceiling is exactly the thing that must not be quietly assumed.

- [ ] **Step 5: Commit**

```bash
git add internal/meter/store docs/spikes
git commit -m "spike(meter): verify Dolt's isolation for the concurrent-reservation race"
```

---

### Task 1: `pkg/budget` — JSON-safe limits and the remaining-budget math

**Closes Plan 01 carry-forward 1.** `gonkcfg.EffectiveBudget` encodes "unlimited" as `math.Inf(1)` (cost) and `math.MaxInt64` (tokens). `encoding/json.Marshal` **returns an error** on `+Inf` (`json: unsupported value: +Inf`), so an unlimited project would fail to serialize at all. And `MaxInt64` is not safe either: a JavaScript client parsing `9223372036854775807` gets `9223372036854775808` back, so a "limit" silently becomes a *different* limit.

**Decision: unlimited serializes as JSON `null`, for both cost and tokens.** `null` is unambiguous, symmetric across the two types, round-trips, and is the only encoding that cannot be mistaken for a real number. Prometheus is unaffected — its text format supports `+Inf` natively, so the gauges keep the sentinel (Task 9).

**Files:**
- Create: `pkg/budget/limits.go`, `pkg/budget/limits_test.go`
- Create: `pkg/budget/remaining.go`, `pkg/budget/remaining_test.go`, `pkg/budget/mutation_test.go`

- [ ] **Step 1: Write the failing limits test**

`pkg/budget/limits_test.go`:

```go
package budget

import (
	"encoding/json"
	"math"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// The whole point of this package: gonkcfg's unlimited sentinels are not
// JSON-serializable (+Inf errors) or not JSON-safe (MaxInt64 rounds in a
// float64 parser). Unlimited must come out as null, both ways.
func TestUnlimitedMarshalsAsNull(t *testing.T) {
	b := FromEffective(gonkcfg.EffectiveBudget{
		MonthlyCostUSD: math.Inf(1),
		MonthlyTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
		PerTaskTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
	})
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal(unlimited) = %v; the +Inf sentinel escaped", err)
	}
	want := `{"monthly_cost_usd":null,"monthly_tokens":null,"per_task_tokens":null}`
	if string(got) != want {
		t.Fatalf("Marshal = %s, want %s", got, want)
	}
}

func TestFiniteMarshalsAsNumbers(t *testing.T) {
	b := FromEffective(gonkcfg.EffectiveBudget{
		MonthlyCostUSD: 10.5,
		MonthlyTokens:  50_000_000,
		PerTaskTokens:  2_000_000,
	})
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"monthly_cost_usd":10.5,"monthly_tokens":50000000,"per_task_tokens":2000000}`
	if string(got) != want {
		t.Fatalf("Marshal = %s, want %s", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, in := range []Budget{
		{Unlimited, UnlimitedTokens, UnlimitedTokens},
		{0, 0, 0},
		{10.5, 50_000_000, 2_000_000},
	} {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal(%+v) = %v", in, err)
		}
		var out Budget
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("Unmarshal(%s) = %v", raw, err)
		}
		if out != in {
			t.Fatalf("round trip: %s -> %+v, want %+v", raw, out, in)
		}
	}
}

// A NaN or -Inf ceiling must never be silently emitted as a number. Resolve
// already fails closed on one (ADR-002), but this type is the last line before
// the wire.
func TestNonFiniteRefusesToMarshal(t *testing.T) {
	for name, c := range map[string]CostLimit{
		"NaN":  CostLimit(math.NaN()),
		"-Inf": CostLimit(math.Inf(-1)),
	} {
		if _, err := json.Marshal(c); err == nil {
			t.Errorf("%s: Marshal succeeded, want error", name)
		}
	}
	if _, err := json.Marshal(TokenLimit(-1)); err == nil {
		t.Error("negative TokenLimit marshaled, want error")
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	for _, raw := range []string{`-1`, `"lots"`, `1e400`} {
		var c CostLimit
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			t.Errorf("CostLimit accepted %s", raw)
		}
	}
}
```

- [ ] **Step 2: Run it, watch it fail**

Run: `go test ./pkg/budget/ -v`
Expected: FAIL — `no Go files` / undefined `Budget`, `CostLimit`, `TokenLimit`, `FromEffective`.

- [ ] **Step 3: Implement `pkg/budget/limits.go`**

```go
// Package budget is the JSON-safe money layer over gonkcfg.EffectiveBudget.
//
// gonkcfg encodes "unlimited" as concrete sentinels: math.Inf(1) for cost and
// math.MaxInt64 for tokens (ADR-002). Neither survives contact with JSON --
// encoding/json REFUSES to marshal +Inf (it returns an error, not a number),
// and MaxInt64 loses precision in any float64-based parser, which is every
// JavaScript client. This package translates both to JSON null on the way out
// and back on the way in, so "unlimited" means exactly one thing on the wire.
package budget

import (
	"encoding/json"
	"fmt"
	"math"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// Unlimited is the cost sentinel (gonkcfg.EffectiveBudget.MonthlyCostUSD).
//
// It is a var and not a const because Go has no constant expression for +Inf
// (math.Inf is a function call, not a constant). TestUnlimitedSentinelIsInf
// guards it, so a package that reassigned it would be caught.
var Unlimited = CostLimit(math.Inf(1))

// UnlimitedTokens is the token sentinel (gonkcfg.EffectiveBudget token fields).
const UnlimitedTokens = TokenLimit(math.MaxInt64)

// CostLimit is a USD ceiling. +Inf means unlimited and marshals as null.
type CostLimit float64

func (c CostLimit) Unlimited() bool { return math.IsInf(float64(c), 1) }

func (c CostLimit) MarshalJSON() ([]byte, error) {
	f := float64(c)
	switch {
	case math.IsInf(f, 1):
		return []byte("null"), nil
	case math.IsNaN(f) || math.IsInf(f, -1) || f < 0:
		return nil, fmt.Errorf("budget: refusing to marshal cost ceiling %v", f)
	}
	return json.Marshal(f)
}

func (c *CostLimit) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*c = Unlimited
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("budget: cost ceiling: %w", err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return fmt.Errorf("budget: cost ceiling must be a finite number >= 0, got %v", f)
	}
	*c = CostLimit(f)
	return nil
}

// TokenLimit is a token ceiling. MaxInt64 means unlimited and marshals as null.
type TokenLimit int64

func (t TokenLimit) Unlimited() bool { return t == UnlimitedTokens }

func (t TokenLimit) MarshalJSON() ([]byte, error) {
	if t.Unlimited() {
		return []byte("null"), nil
	}
	if t < 0 {
		return nil, fmt.Errorf("budget: refusing to marshal token ceiling %d", int64(t))
	}
	return json.Marshal(int64(t))
}

func (t *TokenLimit) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*t = UnlimitedTokens
		return nil
	}
	var i int64
	if err := json.Unmarshal(raw, &i); err != nil {
		return fmt.Errorf("budget: token ceiling: %w", err)
	}
	if i < 0 {
		return fmt.Errorf("budget: token ceiling must be >= 0, got %d", i)
	}
	*t = TokenLimit(i)
	return nil
}

// Budget is an Effective budget, wire-shaped.
type Budget struct {
	MonthlyCostUSD CostLimit  `json:"monthly_cost_usd"`
	MonthlyTokens  TokenLimit `json:"monthly_tokens"`
	PerTaskTokens  TokenLimit `json:"per_task_tokens"`
}

// FromEffective converts gonkcfg's resolved budget. It is the ONLY legitimate
// way to build a Budget outside tests: per ADR-002, gonkcfg.Resolve is the only
// legitimate way to build an Effective.
func FromEffective(b gonkcfg.EffectiveBudget) Budget {
	return Budget{
		MonthlyCostUSD: CostLimit(b.MonthlyCostUSD),
		MonthlyTokens:  TokenLimit(b.MonthlyTokens),
		PerTaskTokens:  TokenLimit(b.PerTaskTokens),
	}
}

// AllUnlimited reports whether no ceiling constrains this project at all.
// Used to decide whether stale spend data even matters.
func (b Budget) AllUnlimited() bool {
	return b.MonthlyCostUSD.Unlimited() && b.MonthlyTokens.Unlimited() && b.PerTaskTokens.Unlimited()
}
```

- [ ] **Step 4: Watch it pass**

Run: `go test ./pkg/budget/ -run 'Marshal|RoundTrip|Unmarshal' -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Write the failing remaining-math test**

`pkg/budget/remaining_test.go`:

```go
package budget

import (
	"math"
	"testing"
)

// Every case here uses NON-DEFAULT values. Plan 01's review found a resolver
// that ignored all three config layers and still passed, because the expected
// values coincided with the defaults. A test that would pass against
// `return Remaining{}` is not a test.
func TestRemain(t *testing.T) {
	cases := []struct {
		name string
		b    Budget
		s    Spend
		want Remaining
	}{
		{
			name: "observed and reserved both subtract",
			b:    Budget{MonthlyCostUSD: 10, MonthlyTokens: 50_000_000, PerTaskTokens: 2_000_000},
			s: Spend{
				CostUSD: 4, Tokens: 10_000_000, TaskTokens: 500_000,
				ReservedCostUSD: 1.5, ReservedTokens: 2_000_000, ReservedTaskTokens: 100_000,
			},
			want: Remaining{MonthlyCostUSD: 4.5, MonthlyTokens: 38_000_000, PerTaskTokens: 1_400_000},
		},
		{
			name: "overspend clamps to zero, never negative",
			b:    Budget{MonthlyCostUSD: 5, MonthlyTokens: 1_000, PerTaskTokens: 100},
			s:    Spend{CostUSD: 9.75, Tokens: 4_000, TaskTokens: 900},
			want: Remaining{MonthlyCostUSD: 0, MonthlyTokens: 0, PerTaskTokens: 0},
		},
		{
			name: "unlimited stays unlimited after spending",
			b:    Budget{MonthlyCostUSD: Unlimited, MonthlyTokens: UnlimitedTokens, PerTaskTokens: UnlimitedTokens},
			s:    Spend{CostUSD: 1234.56, Tokens: 9_000_000_000, TaskTokens: 8_000_000},
			want: Remaining{MonthlyCostUSD: Unlimited, MonthlyTokens: UnlimitedTokens, PerTaskTokens: UnlimitedTokens},
		},
		{
			name: "a zero ceiling is a real ceiling, not 'unset' (ADR-002)",
			b:    Budget{MonthlyCostUSD: 0, MonthlyTokens: 50_000_000, PerTaskTokens: 2_000_000},
			s:    Spend{Tokens: 1_000_000, TaskTokens: 1_000},
			want: Remaining{MonthlyCostUSD: 0, MonthlyTokens: 49_000_000, PerTaskTokens: 1_999_000},
		},
		{
			name: "mixed: unlimited cost, finite tokens",
			b:    Budget{MonthlyCostUSD: Unlimited, MonthlyTokens: 8_000_000, PerTaskTokens: 3_000_000},
			s:    Spend{CostUSD: 40, Tokens: 7_500_000, TaskTokens: 2_999_999, ReservedTokens: 100_000},
			want: Remaining{MonthlyCostUSD: Unlimited, MonthlyTokens: 400_000, PerTaskTokens: 1},
		},
		{
			// LiteLLM rows are external, unvalidated input. A credit or refund
			// row carrying a negative spend must NOT hand the project extra
			// headroom -- that is this package's only fail-open path.
			name: "a negative spend does not raise the ceiling",
			b:    Budget{MonthlyCostUSD: 10, MonthlyTokens: 50_000_000, PerTaskTokens: 2_000_000},
			s:    Spend{CostUSD: -100, Tokens: -5, TaskTokens: -5},
			want: Remaining{MonthlyCostUSD: 10, MonthlyTokens: 0, PerTaskTokens: 0},
		},
		{
			// THE SYNTHETIC-DOLLAR SEPARATION (Decision 9). Local rungs carry a
			// synthetic price so LiteLLM's USD key ceiling can act as a hard door
			// for TOKENS. Those dollars are an accounting unit, not spend, and
			// they must never touch monthly_cost_usd.
			//
			// $100 of synthetic spend and $50 of synthetic reservations against a
			// $10 REAL ceiling with $4 of real spend: remaining is $6. If the
			// synthetic dollars leaked in, it would be $0 -- and the onboarding
			// default (monthly_cost_usd: 0, ladder: [qwen-local]) could not afford
			// its own only rung.
			name: "synthetic local-rung dollars do NOT consume the real cost ceiling",
			b:    Budget{MonthlyCostUSD: 10, MonthlyTokens: 50_000_000, PerTaskTokens: 2_000_000},
			s: Spend{
				CostUSD: 4, SyntheticCostUSD: 100, ReservedSyntheticCostUSD: 50,
				Tokens: 1_000_000, TaskTokens: 300_000,
			},
			want: Remaining{MonthlyCostUSD: 6, MonthlyTokens: 49_000_000, PerTaskTokens: 1_700_000},
		},
		{
			// ...and the tokens a local rung burns DO count, against both token
			// ceilings. That is the whole reason it is metered at all (spec 6.3:
			// "local rungs are cost-0 dollars but metered in tokens").
			name: "a local rung's TOKENS still consume both token ceilings",
			b:    Budget{MonthlyCostUSD: 0, MonthlyTokens: 1_000_000, PerTaskTokens: 400_000},
			s:    Spend{SyntheticCostUSD: 3, Tokens: 750_000, TaskTokens: 150_000, ReservedTokens: 50_000},
			want: Remaining{MonthlyCostUSD: 0, MonthlyTokens: 200_000, PerTaskTokens: 250_000},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Remain(c.b, c.s); got != c.want {
				t.Fatalf("Remain(%+v, %+v)\n got %+v\nwant %+v", c.b, c.s, got, c.want)
			}
		})
	}
}

// A NaN spend must not produce a NaN remaining (which loses every comparison
// and would read as "affordable"). Fail closed: treat it as fully spent.
func TestRemainFailsClosedOnNaNSpend(t *testing.T) {
	got := Remain(
		Budget{MonthlyCostUSD: 10, MonthlyTokens: 1_000, PerTaskTokens: 100},
		Spend{CostUSD: math.NaN()},
	)
	if got.MonthlyCostUSD != 0 {
		t.Fatalf("NaN spend gave remaining %v, want 0", got.MonthlyCostUSD)
	}
}

// Saturating arithmetic: a hostile or corrupt reservation total must not wrap.
func TestRemainDoesNotOverflow(t *testing.T) {
	got := Remain(
		Budget{MonthlyCostUSD: 10, MonthlyTokens: 1_000, PerTaskTokens: 100},
		Spend{Tokens: math.MaxInt64 - 1, ReservedTokens: math.MaxInt64 - 1},
	)
	if got.MonthlyTokens != 0 {
		t.Fatalf("overflowing spend gave remaining %d, want 0", int64(got.MonthlyTokens))
	}
}

func TestFits(t *testing.T) {
	r := Remaining{MonthlyCostUSD: 0.30, MonthlyTokens: 500_000, PerTaskTokens: 250_000}
	if !r.FitsCost(0.30) {
		t.Error("exactly-affordable cost rejected")
	}
	if r.FitsCost(0.31) {
		t.Error("unaffordable cost accepted")
	}
	// A zero remaining never fits a priced rung, even at a zero estimate:
	// an unpriced cloud rung must not slip through a $0 budget.
	zero := Remaining{MonthlyCostUSD: 0}
	if zero.FitsCost(0) {
		t.Error("priced rung admitted against a zero cost budget")
	}
	if !r.FitsMonthTokens(500_000) || r.FitsMonthTokens(500_001) {
		t.Error("month token fit wrong")
	}
	if !r.FitsTaskTokens(250_000) || r.FitsTaskTokens(250_001) {
		t.Error("task token fit wrong")
	}
	unl := Remaining{MonthlyCostUSD: Unlimited, MonthlyTokens: UnlimitedTokens, PerTaskTokens: UnlimitedTokens}
	if !unl.FitsCost(1e9) || !unl.FitsMonthTokens(math.MaxInt64-1) || !unl.FitsTaskTokens(math.MaxInt64-1) {
		t.Error("unlimited failed to fit")
	}
}
```

- [ ] **Step 6: Run it, watch it fail**

Run: `go test ./pkg/budget/ -run 'Remain|Fits' -v`
Expected: FAIL — undefined `Spend`, `Remaining`, `Remain`.

- [ ] **Step 7: Implement `pkg/budget/remaining.go`**

```go
package budget

import "math"

// Spend is consumption within one budget window: what LiteLLM's spend log has
// already reported, plus what open reservations have committed us to but not
// yet billed. Reservations are what make the check survive spend-log lag and
// concurrent sessions (ADR-004).
//
// CostUSD IS REAL MONEY AND ONLY REAL MONEY. Local rungs are priced
// synthetically in LiteLLM so that its USD virtual-key ceiling is a hard door
// for TOKENS too (Decision 9) -- but those synthetic dollars are an accounting
// unit, not spend, and they are tracked SEPARATELY here. They must never reach
// the cost gate.
//
// The reason is not fastidiousness, it is the onboarding default. The .gonk.yml
// the onboarding MR ships carries `monthly_cost_usd: 0` and `ladder:
// [qwen-local]` (spec 5.3). If a local rung's synthetic dollars counted against
// monthly_cost_usd, that project could not afford its own only rung and would be
// DEAD ON ARRIVAL. So: budget.Remain computes remaining cost from CostUSD alone,
// rung.Decide's cost gate reads it, and `monthly_cost_usd: 0` keeps meaning
// exactly what it has always meant -- "local rungs run freely, a cloud rung is
// never affordable".
type Spend struct {
	// Real money. Cloud rungs only.
	CostUSD         float64 `json:"cost_usd"`
	ReservedCostUSD float64 `json:"reserved_cost_usd"`

	// Synthetic money. Local rungs only. REPORTING AND THE LITELLM KEY CEILING
	// ONLY -- never a policy input. Nothing in Remain() or Fits*() reads these.
	SyntheticCostUSD         float64 `json:"synthetic_cost_usd"`
	ReservedSyntheticCostUSD float64 `json:"reserved_synthetic_cost_usd"`

	Tokens             int64 `json:"tokens"`
	TaskTokens         int64 `json:"task_tokens"` // this bead only, all attempts, all time
	ReservedTokens     int64 `json:"reserved_tokens"`
	ReservedTaskTokens int64 `json:"reserved_task_tokens"`
}

// Remaining is ceiling - (observed + reserved), clamped at zero. Unlimited
// ceilings stay unlimited: +Inf - x is +Inf for free, but MaxInt64 - x is NOT
// MaxInt64, so the token fields must special-case it or an "unlimited" project
// would slowly acquire a limit.
type Remaining struct {
	MonthlyCostUSD CostLimit  `json:"monthly_cost_usd"`
	MonthlyTokens  TokenLimit `json:"monthly_tokens"`
	PerTaskTokens  TokenLimit `json:"per_task_tokens"`
}

func Remain(b Budget, s Spend) Remaining {
	return Remaining{
		MonthlyCostUSD: remainCost(b.MonthlyCostUSD, s.CostUSD, s.ReservedCostUSD),
		MonthlyTokens:  remainTokens(b.MonthlyTokens, s.Tokens, s.ReservedTokens),
		PerTaskTokens:  remainTokens(b.PerTaskTokens, s.TaskTokens, s.ReservedTaskTokens),
	}
}

func remainCost(ceiling CostLimit, observed, reserved float64) CostLimit {
	if ceiling.Unlimited() {
		return Unlimited
	}
	used := observed + reserved
	// Fail closed on garbage: a NaN loses every comparison, so a NaN remaining
	// would read as "affordable" at every call site.
	if math.IsNaN(used) || math.IsInf(used, 1) || math.IsNaN(float64(ceiling)) {
		return 0
	}
	// A NEGATIVE spend must not RAISE the ceiling. LiteLLM rows are external,
	// unvalidated input, and a credit/refund/adjustment row with a negative
	// `spend` would otherwise hand the project extra headroom -- the one place
	// this arithmetic could fail open.
	//
	// Note this clamps to "nothing spent" while remainTokens (via addSat) clamps
	// a negative token count to "fully spent". That asymmetry is deliberate: a
	// negative dollar amount is a MEANINGFUL row (a credit), so we neither grant
	// the headroom nor punish the project for it; a negative TOKEN COUNT is
	// nonsense, so it can only be corruption, and corruption fails closed.
	if used < 0 {
		used = 0
	}
	if r := float64(ceiling) - used; r > 0 {
		return CostLimit(r)
	}
	return 0
}

func remainTokens(ceiling TokenLimit, observed, reserved int64) TokenLimit {
	if ceiling.Unlimited() {
		return UnlimitedTokens
	}
	used := addSat(observed, reserved)
	if r := int64(ceiling) - used; r > 0 {
		return TokenLimit(r)
	}
	return 0
}

// addSat saturates at MaxInt64 instead of wrapping to a negative number, which
// would read as "nothing has been spent".
func addSat(a, b int64) int64 {
	if a < 0 || b < 0 {
		return math.MaxInt64 // nonsense input: treat as fully spent
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// FitsCost reports whether a rung whose estimated cost is est can be started.
// est is 0 only for local rungs, which are free by definition and skip this
// check entirely (rung.Decide never calls FitsCost for them) -- so a caller
// reaching here with est == 0 is asking about a PRICED rung with no price, and
// the honest answer against a zero budget is no.
func (r Remaining) FitsCost(est float64) bool {
	if r.MonthlyCostUSD.Unlimited() {
		return true
	}
	if float64(r.MonthlyCostUSD) <= 0 {
		return false
	}
	return float64(r.MonthlyCostUSD) >= est
}

func (r Remaining) FitsMonthTokens(est int64) bool { return fits(r.MonthlyTokens, est) }
func (r Remaining) FitsTaskTokens(est int64) bool  { return fits(r.PerTaskTokens, est) }

func fits(rem TokenLimit, est int64) bool {
	if rem.Unlimited() {
		return true
	}
	if rem <= 0 {
		return false
	}
	return int64(rem) >= est
}
```

- [ ] **Step 8: Watch it pass**

Run: `go test ./pkg/budget/ -race -v`
Expected: PASS (all).

- [ ] **Step 9: Hoist the table into `remainCases()` FIRST (the mutation test needs it)**

In `remaining_test.go`, move the table out of `TestRemain` into a package-level helper. Do this **before** writing `mutation_test.go`, or the package will not compile between the two steps.

```go
type remainCase struct {
	name string
	b    Budget
	s    Spend
	want Remaining
}

func remainCases() []remainCase { return []remainCase{ /* the six cases, verbatim */ } }

func TestRemain(t *testing.T) {
	for _, c := range remainCases() {
		t.Run(c.name, func(t *testing.T) {
			if got := Remain(c.b, c.s); got != c.want {
				t.Fatalf("Remain(%+v, %+v)\n got %+v\nwant %+v", c.b, c.s, got, c.want)
			}
		})
	}
}

// The sentinel is a package var because Go has no constant +Inf. Guard it.
func TestUnlimitedSentinelIsInf(t *testing.T) {
	if !math.IsInf(float64(Unlimited), 1) || int64(UnlimitedTokens) != math.MaxInt64 {
		t.Fatal("the unlimited sentinels have been reassigned; every budget check downstream is now wrong")
	}
}
```

**A note on float equality.** These cases compare `float64` exactly, and they are chosen so that they can: every expected value is exactly representable and every sum is exact in binary64 (`0.40 + 0.25 == 0.65` holds exactly; `10 - (4 + 1.5) == 4.5` holds exactly). That is deliberate, not luck — **if you add a case, verify the arithmetic is exact, or use an epsilon comparison for that case.** A money table that fails on the last bit teaches everyone to stop trusting it.

- [ ] **Step 10: Write the mutation test — prove the table is not vacuous**

`pkg/budget/mutation_test.go`. This is the requirement that Plan 01's review earned: a resolver that ignored every config layer passed its tests because the expectations happened to equal the defaults. Here we *sabotage* the implementation and demand the table notice.

```go
package budget

import "testing"

// A saboteur is a deliberately-wrong Remain. Each one MUST be caught by at
// least one row of TestRemain's table. If a saboteur survives the whole table,
// the table does not actually constrain the money math and this test fails,
// naming the bug we would have shipped.
type saboteur struct {
	name string
	bug  func(Budget, Spend) Remaining
}

func saboteurs() []saboteur {
	return []saboteur{
		{"ignores reserved spend", func(b Budget, s Spend) Remaining {
			s.ReservedCostUSD, s.ReservedTokens, s.ReservedTaskTokens = 0, 0, 0
			return Remain(b, s)
		}},
		{"ignores observed spend", func(b Budget, s Spend) Remaining {
			s.CostUSD, s.Tokens, s.TaskTokens = 0, 0, 0
			return Remain(b, s)
		}},
		{"treats a zero ceiling as unset", func(b Budget, s Spend) Remaining {
			if b.MonthlyCostUSD == 0 {
				b.MonthlyCostUSD = Unlimited
			}
			return Remain(b, s)
		}},
		{"lets an unlimited ceiling decay", func(b Budget, s Spend) Remaining {
			if b.MonthlyTokens.Unlimited() {
				b.MonthlyTokens = TokenLimit(int64(UnlimitedTokens) - 1)
			}
			return Remain(b, s)
		}},
		{"ignores the per-task ceiling", func(b Budget, s Spend) Remaining {
			b.PerTaskTokens = UnlimitedTokens
			return Remain(b, s)
		}},
		{"confuses task tokens with month tokens", func(b Budget, s Spend) Remaining {
			s.TaskTokens = s.Tokens
			return Remain(b, s)
		}},
		{"lets a negative spend raise the ceiling", func(b Budget, s Spend) Remaining {
			// The bug: no `if used < 0 { used = 0 }` guard in remainCost.
			r := Remain(b, s)
			if s.CostUSD+s.ReservedCostUSD < 0 && !b.MonthlyCostUSD.Unlimited() {
				r.MonthlyCostUSD = CostLimit(float64(b.MonthlyCostUSD) - (s.CostUSD + s.ReservedCostUSD))
			}
			return r
		}},
		{"counts SYNTHETIC dollars against the real cost ceiling", func(b Budget, s Spend) Remaining {
			// The bug someone WILL write: "cost is cost, add it all up." It kills
			// the onboarding default -- a monthly_cost_usd: 0 project can no
			// longer afford its own local rung -- and it silently reports
			// accounting fiction as real money on the Cost dashboard.
			s.CostUSD += s.SyntheticCostUSD
			s.ReservedCostUSD += s.ReservedSyntheticCostUSD
			return Remain(b, s)
		}},
		{"drops a local rung's TOKENS because it is 'free'", func(b Budget, s Spend) Remaining {
			// The mirror-image bug: "local is free, so don't meter it." Spec 6.3
			// says local rungs are cost-0 dollars but METERED IN TOKENS, and with
			// Decision 9 the token reservation is what the hard door is built on.
			if s.SyntheticCostUSD > 0 || s.ReservedSyntheticCostUSD > 0 {
				s.Tokens, s.TaskTokens, s.ReservedTokens, s.ReservedTaskTokens = 0, 0, 0, 0
			}
			return Remain(b, s)
		}},
	}
}

func TestSaboteursAreCaught(t *testing.T) {
	// Reuse TestRemain's table by re-declaring it through the same helper.
	for _, sab := range saboteurs() {
		caught := false
		for _, c := range remainCases() {
			if sab.bug(c.b, c.s) != c.want {
				caught = true
				break
			}
		}
		if !caught {
			t.Errorf("saboteur %q survived every table row: the budget table is vacuous and would ship this bug", sab.name)
		}
	}
}
```

- [ ] **Step 11: Run the mutation test, watch every saboteur get caught**

Run: `go test ./pkg/budget/ -run Saboteurs -v`
Expected: PASS. If any saboteur "survived every table row", **add a table row that catches it** — do not weaken the saboteur.

- [ ] **Step 12: Full gate and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add pkg/budget && git commit -m "feat(budget): JSON-safe unlimited sentinels and remaining-budget math"
```

---

### Task 2: `pkg/opercfg` — validated operator config (instance + group policy, rung catalog)

**Closes Plan 01 carry-forward 2 and ADR-002's "Known gap".** Nothing in-repo validates operator-supplied instance/group `Policy`: they bypass the JSON Schema and the non-finite-float check that guard project `.gonk.yml`. `Resolve` fails closed on the one dangerous case (a NaN ceiling), but it should not be the only thing standing there. This package is the `Load` equivalent for the operator layers — and meter **refuses to start** on a config it cannot validate.

It also carries the two things `rung.Decide` needs that `.gonk.yml` cannot supply: the **rung catalog** (which rungs are local vs cloud, which LiteLLM model each maps to, what one attempt is estimated to consume) and the **meter knobs** (staleness, TTLs, retry caps).

**Files:**
- Create: `pkg/opercfg/gonk-operator.v1.schema.json`, `pkg/opercfg/opercfg.go`, `pkg/opercfg/opercfg_test.go`, `pkg/opercfg/testdata/schema.v1.sha256`, `pkg/opercfg/drift_test.go`
- Create: `docs/schemas/gonk-operator.v1.schema.json` (published copy)

- [ ] **Step 1: Write the canonical schema**

`pkg/opercfg/gonk-operator.v1.schema.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://gitlab.orac.local/agentic/gonk-project/-/raw/main/docs/schemas/gonk-operator.v1.schema.json",
  "title": "gonk operator configuration (instance defaults, group overrides, rung catalog), schema version 1",
  "type": "object",
  "additionalProperties": false,
  "required": ["version", "rungs"],
  "properties": {
    "version": { "const": 1 },
    "instance": { "$ref": "#/$defs/policy" },
    "groups": {
      "type": "object",
      "propertyNames": { "pattern": "^[a-zA-Z0-9][a-zA-Z0-9._/-]*$" },
      "additionalProperties": { "$ref": "#/$defs/policy" }
    },
    "rungs": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "kind", "model", "est_tokens"],
        "properties": {
          "name": { "type": "string", "pattern": "^[a-z0-9][a-z0-9-]*$" },
          "kind": { "enum": ["local", "cloud"] },
          "model": { "type": "string", "minLength": 1 },
          "est_cost_usd": { "type": "number", "minimum": 0 },
          "est_tokens": { "$ref": "#/$defs/tokenQuantity" },
          "synthetic_usd_per_1m_tokens": { "type": "number", "exclusiveMinimum": 0 }
        }
      }
    },
    "meter": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "max_spend_staleness": { "$ref": "#/$defs/duration" },
        "reservation_ttl": { "$ref": "#/$defs/duration" },
        "max_clock_skew": { "$ref": "#/$defs/duration" },
        "key_retry_backoff": { "$ref": "#/$defs/duration" },
        "max_infra_retries": { "type": "integer", "minimum": 1, "maximum": 100 },
        "enforce_ladder_order": { "type": "boolean" }
      }
    }
  },
  "$defs": {
    "duration": { "type": "string", "pattern": "^[0-9]+(ns|us|ms|s|m|h)$" },
    "tokenQuantity": {
      "oneOf": [
        { "type": "integer", "minimum": 0 },
        { "type": "string", "pattern": "^[0-9]+[KMG]?$" }
      ]
    },
    "policy": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "enabled": { "type": "boolean" },
        "actions": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "triage": { "type": "boolean" },
            "pipelines": { "type": "boolean" },
            "features": { "type": "boolean" }
          }
        },
        "schedule": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "quiet_hours": {
              "type": "string",
              "pattern": "^([01][0-9]|2[0-3]):[0-5][0-9]-([01][0-9]|2[0-3]):[0-5][0-9]$"
            },
            "timezone": { "type": "string", "minLength": 1 }
          }
        },
        "budget": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "monthly_cost_usd": { "type": "number", "minimum": 0 },
            "monthly_tokens": { "$ref": "#/$defs/tokenQuantity" },
            "per_task_tokens": { "$ref": "#/$defs/tokenQuantity" }
          }
        },
        "ladder": {
          "type": "array",
          "minItems": 1,
          "uniqueItems": true,
          "items": { "type": "string", "pattern": "^[a-z0-9][a-z0-9-]*$" }
        },
        "continuity": { "enum": ["resume", "fresh"] },
        "triage": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "label_prefix": { "type": "string", "minLength": 1 },
            "respond_to_mentions": { "type": "boolean" }
          }
        },
        "provenance": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "commit_trailers": { "type": "boolean" },
            "include_usage": { "type": "boolean" }
          }
        }
      }
    }
  }
}
```

Note `$defs/policy` mirrors `.gonk.yml`'s shape minus `version` — deliberately, so `gonkcfg.Policy` decodes both. The `ladder` here gets `uniqueItems: true` for the same reason Plan 01 added it to the project schema (a duplicated rung makes escalation retry the same rung).

- [ ] **Step 2: Write the failing test**

`pkg/opercfg/opercfg_test.go`:

```go
package opercfg

import (
	"strings"
	"testing"
	"time"

	_ "time/tzdata" // checkPolicy calls time.LoadLocation; CI images have no zoneinfo
)

const validOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local, glm, sonnet]
  budget: { monthly_cost_usd: 200, monthly_tokens: "5G" }
groups:
  agentic:
    budget: { monthly_cost_usd: 50 }
  agentic/experiments:
    enabled: false
rungs:
  - { name: qwen-local, kind: local,  model: qwen3-coder-30b, est_cost_usd: 0,    est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.20 }
  - { name: glm,        kind: cloud,  model: glm-5,           est_cost_usd: 0.40, est_tokens: "200K" }
  - { name: sonnet,     kind: cloud,  model: claude-sonnet,   est_cost_usd: 1.20, est_tokens: "200K" }
meter:
  max_spend_staleness: 5m
  reservation_ttl: 60m
  max_infra_retries: 5
`

func TestLoadOperatorConfig(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	// Assert NON-DEFAULT values, so a Load that returned a zero struct fails.
	if oc.Instance.Budget.MonthlyCostUSD == nil || *oc.Instance.Budget.MonthlyCostUSD != 200 {
		t.Fatalf("instance cost ceiling: %+v", oc.Instance.Budget)
	}
	if oc.Instance.Budget.MonthlyTokens == nil || *oc.Instance.Budget.MonthlyTokens != 5_000_000_000 {
		t.Fatalf("instance token ceiling: %+v", oc.Instance.Budget)
	}
	if got := oc.Instance.Ladder; len(got) != 3 || got[2] != "sonnet" {
		t.Fatalf("instance ladder: %v", got)
	}
	glm, ok := oc.Catalog["glm"]
	if !ok || glm.Kind != KindCloud || glm.Model != "glm-5" || glm.EstCostUSD != 0.40 || glm.EstTokens != 200_000 {
		t.Fatalf("catalog[glm] = %+v", glm)
	}
	if oc.Meter.MaxSpendStaleness != 5*time.Minute || oc.Meter.ReservationTTL != 60*time.Minute {
		t.Fatalf("meter knobs: %+v", oc.Meter)
	}
	// Defaults applied where silent.
	if oc.Meter.MaxClockSkew != 5*time.Minute || !oc.Meter.EnforceLadderOrder {
		t.Fatalf("meter defaults: %+v", oc.Meter)
	}
}

// GroupFor must FOLD every matching ancestor, not just return the longest
// prefix. Returning only the deepest match would silently drop an ancestor's
// ceiling -- a budget escape, and a direct contradiction of ADR-002's
// "ceilings only tighten downward".
func TestGroupFor(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	for project, wantEnabledSet := range map[string]bool{
		"agentic/gonk-project":      false, // agentic sets budget only, not enabled
		"agentic/experiments/spike": true,  // agentic/experiments sets enabled: false
		"unrelated/thing":           false, // no group matches -> zero Policy
	} {
		p := oc.GroupFor(project)
		if (p.Enabled != nil) != wantEnabledSet {
			t.Errorf("GroupFor(%q).Enabled set = %v, want %v", project, p.Enabled != nil, wantEnabledSet)
		}
	}
	if p := oc.GroupFor("agentic/gonk-project"); p.Budget.MonthlyCostUSD == nil || *p.Budget.MonthlyCostUSD != 50 {
		t.Errorf("GroupFor(agentic/...) lost the group budget: %+v", p.Budget)
	}
	// THE money case: the nested group sets no budget, so the PARENT's $50
	// ceiling must survive the fold. A longest-prefix-only GroupFor returns the
	// experiments policy alone and this project silently gets the instance's $200.
	p := oc.GroupFor("agentic/experiments/spike")
	if p.Budget.MonthlyCostUSD == nil || *p.Budget.MonthlyCostUSD != 50 {
		t.Errorf("a nested group dropped its ancestor's ceiling: %+v", p.Budget)
	}
	if p.Enabled == nil || *p.Enabled {
		t.Errorf("the nested group's enabled:false was lost in the fold: %+v", p.Enabled)
	}
	// A prefix must match on a path SEGMENT boundary, not a string prefix:
	// group "agentic" must not capture project "agentic-other/repo".
	if p := oc.GroupFor("agentic-other/repo"); p.Budget.MonthlyCostUSD != nil {
		t.Error("group prefix matched across a segment boundary")
	}
}

// A nested group may TIGHTEN its parent's ceiling and may never LOOSEN it.
func TestGroupForCeilingsOnlyTighten(t *testing.T) {
	oc, err := Load([]byte(`
version: 1
instance: { ladder: [a, b] }
groups:
  top:            { budget: { monthly_cost_usd: 50, monthly_tokens: "1G" } }
  top/tighter:    { budget: { monthly_cost_usd: 5 } }
  top/looser:     { budget: { monthly_cost_usd: 500, monthly_tokens: "9G" } }
rungs:
  - { name: a, kind: local, model: m, est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.2 }
  - { name: b, kind: cloud, model: n, est_cost_usd: 0.4, est_tokens: "200K" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := oc.GroupFor("top/tighter/repo").Budget.MonthlyCostUSD; got == nil || *got != 5 {
		t.Errorf("a tightening nested group = %v, want 5", got)
	}
	// A group chain whose ladders do not overlap allows NOTHING, and must hand
	// Resolve a non-nil empty slice to say so. A nil would mean "this layer is
	// silent" (ADR-002), Resolve would skip the group allow-list entirely, and
	// the project would run a rung the group hierarchy forbade.
	oc2, err := Load([]byte(`
version: 1
instance: { ladder: [a, b] }
groups:
  top:     { ladder: [b] }
  top/sub: { ladder: [a] }
rungs:
  - { name: a, kind: local, model: m, est_tokens: "200K", synthetic_usd_per_1m_tokens: 0.2 }
  - { name: b, kind: cloud, model: n, est_cost_usd: 0.4, est_tokens: "200K" }
`))
	if err != nil {
		t.Fatal(err)
	}
	l := oc2.GroupFor("top/sub/repo").Ladder
	if l == nil {
		t.Fatal("a group chain that allows no rung produced a NIL ladder; Resolve reads that as 'no constraint' and the project escapes the group allow-list")
	}
	if len(l) != 0 {
		t.Fatalf("group ladders [b] and [a] do not intersect; got %v", l)
	}
	loose := oc.GroupFor("top/looser/repo").Budget
	if loose.MonthlyCostUSD == nil || *loose.MonthlyCostUSD != 50 {
		t.Errorf("a nested group LOOSENED its parent's cost ceiling to %v; ceilings only tighten (ADR-002)", loose.MonthlyCostUSD)
	}
	if loose.MonthlyTokens == nil || *loose.MonthlyTokens != 1_000_000_000 {
		t.Errorf("a nested group LOOSENED its parent's token ceiling to %v", loose.MonthlyTokens)
	}
}

// This is the whole point of the package: the operator layers must not be able
// to smuggle in what .gonk.yml cannot.
//
// NOTE: every fixture below must be otherwise-VALID, or it is rejected for the
// wrong reason and the case proves nothing. `base` is the minimum valid
// document; each case perturbs exactly one thing. TestLoadRejectsBaseIsValid
// guards that.
func base(extra string) string {
	return "version: 1\n" +
		"instance: { ladder: [a] }\n" +
		"rungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\n" + extra
}

func TestLoadRejectsBaseIsValid(t *testing.T) {
	if _, err := Load([]byte(base(""))); err != nil {
		t.Fatalf("the reject-table's base document is itself invalid (%v); every case below would pass vacuously", err)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"NaN cost ceiling":      "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], budget: { monthly_cost_usd: .nan } }",
		"Inf cost ceiling":      "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], budget: { monthly_cost_usd: .inf } }",
		"negative cost ceiling": "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], budget: { monthly_cost_usd: -1 } }",
		"fractional tokens":     "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], budget: { monthly_tokens: 1.5 } }",
		"unknown key":           "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], bananas: true }",
		"empty ladder":          "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [] }",
		"duplicate rung in ladder": "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a, a] }",
		"no rungs":              "version: 1\nrungs: []",
		"ladder names an unknown rung": "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [ghost] }",
		"duplicate catalog entry": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}, {name: a, kind: cloud, model: n, est_cost_usd: 1, est_tokens: \"200K\"}]",
		"cloud rung with no price": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: cloud, model: m, est_tokens: \"200K\"}]",
		"local rung with a REAL price": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_cost_usd: 0.5, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]",
		// DECISION 9. A local rung with no SYNTHETIC price costs LiteLLM's USD
		// counter nothing, so the virtual-key ceiling never moves for it and
		// monthly_tokens has no hard door anywhere. This is the hole the synthetic
		// price exists to close; refusing to start is how it stays closed.
		"local rung with no SYNTHETIC price": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\"}]",
		// ...and the mirror: a cloud rung's price is real and lives in LiteLLM's
		// model config. Declaring a synthetic one for it is a lie that
		// PricePerToken would silently believe.
		"cloud rung with a synthetic price": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: cloud, model: m, est_cost_usd: 0.4, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]",
		// A rung that reserves no tokens holds no token budget. With Decision 9
		// the token reservation is what the whole hard door is built on, so this
		// is a real hole, not a schema formality.
		"rung with no est_tokens":   "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, synthetic_usd_per_1m_tokens: 0.2}]",
		"rung with zero est_tokens": "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: 0, synthetic_usd_per_1m_tokens: 0.2}]",
		// enforce_ladder_order defaults on, and it is toothless with no instance
		// ladder to order against.
		"ladder order enforced with no instance ladder": "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]",
		"group ladder reorders the instance ladder": "version: 1\n" +
			"instance: { ladder: [a, b] }\n" +
			"groups: { g: { ladder: [b, a] } }\n" +
			"rungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}, {name: b, kind: cloud, model: n, est_cost_usd: 0.4, est_tokens: \"200K\"}]",
		"bad duration":     "version: 1\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\nmeter: { reservation_ttl: soon }",
		"unknown timezone": "version: 1\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]\ninstance: { ladder: [a], schedule: { quiet_hours: \"22:00-07:00\", timezone: Mars/Olympus } }",
		"wrong version":    "version: 2\ninstance: { ladder: [a] }\nrungs: [{name: a, kind: local, model: m, est_tokens: \"200K\", synthetic_usd_per_1m_tokens: 0.2}]",
		"not yaml":         "{{{{",
	}
	for name, doc := range cases {
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("%s: Load accepted %q, want error", name, strings.TrimSpace(doc))
		}
	}
}

// A cloud rung MUST be priced, because rung.Decide only applies the cost gate
// to priced rungs -- an unpriced cloud rung would be free money.
func TestCloudRungsMustBePriced(t *testing.T) {
	_, err := Load([]byte("version: 1\ninstance: { ladder: [glm] }\n" +
		"rungs: [{name: glm, kind: cloud, model: glm-5, est_cost_usd: 0, est_tokens: \"200K\"}]"))
	if err == nil {
		t.Fatal("a cloud rung with est_cost_usd: 0 was accepted; it would bypass the cost gate")
	}
}

func TestCheckLadderOrder(t *testing.T) {
	instance := []string{"qwen-local", "glm", "sonnet"}
	if err := CheckLadderOrder(instance, []string{"qwen-local", "sonnet"}); err != nil {
		t.Errorf("a subsequence was rejected: %v", err)
	}
	if err := CheckLadderOrder(instance, []string{"sonnet", "qwen-local"}); err == nil {
		t.Error("a project reordered cloud ahead of local and was accepted (spec 6.3: start at the cheapest rung)")
	}
	if err := CheckLadderOrder(nil, []string{"sonnet", "qwen-local"}); err != nil {
		t.Errorf("no instance ladder means no ordering constraint: %v", err)
	}
}
```

- [ ] **Step 3: Run it, watch it fail**

Run: `go test ./pkg/opercfg/ -v`
Expected: FAIL — undefined `Load`, `OperatorConfig`, `CheckLadderOrder`.

- [ ] **Step 4: Implement `pkg/opercfg/opercfg.go`**

```go
// Package opercfg is the contract for gonk's OPERATOR configuration: instance
// defaults, group overrides, the rung catalog, and gonk-meter's knobs. It lives
// in the private gonk-city repo and the chart's values, and it is mounted into
// gonk-meter as a single YAML document.
//
// Why this package exists: pkg/gonkcfg validates the project's .gonk.yml, but
// NOTHING validated the instance and group Policy values fed into
// gonkcfg.Resolve -- they bypassed the JSON Schema and the non-finite-float
// check (ADR-002, "Known gap"). Resolve fails closed on the one case that would
// otherwise be a silent budget escape (a NaN ceiling reading as "unlimited"),
// but the resolver should not be the only guard. Load is the missing gate:
// gonk-meter refuses to start on an operator config it cannot validate.
package opercfg

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// SchemaVersion is the operator-config schema major version this package speaks.
const SchemaVersion = 1

//go:embed gonk-operator.v1.schema.json
var schemaJSON []byte

var compiled = mustCompile()

func mustCompile() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("opercfg: embedded schema unreadable: %v", err))
	}
	var meta struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(schemaJSON, &meta); err != nil || meta.ID == "" {
		panic("opercfg: embedded schema has no $id")
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(meta.ID, doc); err != nil {
		panic(err)
	}
	s, err := c.Compile(meta.ID)
	if err != nil {
		panic(fmt.Sprintf("opercfg: embedded schema invalid: %v", err))
	}
	return s
}

// Kind classifies a rung. It is the difference between "costs tokens" and
// "costs money", which is the difference between a defer and a run.
type Kind string

const (
	KindLocal Kind = "local"
	KindCloud Kind = "cloud"
)

// RungSpec is one rung of the ladder: the LiteLLM model it dispatches to, and
// what one attempt at it is estimated to consume. The estimates are what a
// reservation reserves (ADR-004).
type RungSpec struct {
	Name  string `yaml:"name"`
	Kind  Kind   `yaml:"kind"`
	Model string `yaml:"model"`

	// EstCostUSD is REAL MONEY: what one attempt at this rung actually costs.
	// Cloud rungs: > 0, required. Local rungs: exactly 0 -- local inference is
	// free, and this is what makes `monthly_cost_usd: 0` mean "local rungs only,
	// forever" fall straight out of the arithmetic in rung.Decide.
	EstCostUSD float64 `yaml:"est_cost_usd"`

	// EstTokens is what one attempt is estimated to consume. REQUIRED ON EVERY
	// RUNG, local ones included: rung.Reserve reserves it, and a rung that
	// reserves no tokens holds no token budget.
	EstTokens gonkcfg.TokenQuantity `yaml:"est_tokens"`

	// SyntheticUSDPer1MTokens is the price we CONFIGURE IN LITELLM for a local
	// model, so that LiteLLM's USD virtual-key ceiling is a hard door for TOKENS
	// too (Decision 9). Without it a local rung costs $0, the dollar counter
	// never moves, and monthly_tokens has no hard enforcement anywhere.
	//
	// REQUIRED on local rungs (> 0). FORBIDDEN on cloud rungs: a cloud rung's
	// price is real and lives in LiteLLM's own model config, and declaring a
	// synthetic price for it would be a lie.
	//
	// The operator MUST configure the same number in LiteLLM's model list. That
	// agreement is unverifiable from here; Plan 06 checks it.
	SyntheticUSDPer1MTokens float64 `yaml:"synthetic_usd_per_1m_tokens"`
}

// PricePerToken is what LiteLLM's USD counter will be charged per token for this
// rung: the real price for a cloud rung (derived from its estimates), the
// synthetic price for a local one. It is what Service uses to convert a
// project's TOKEN ceiling into the USD max_budget it provisions on the virtual
// key (Decision 9).
//
// Both terms are operator-supplied estimates and both are approximations. That
// is acceptable: the resulting max_budget is a deliberately LOOSE backstop, not
// the primary control. Meter's reservation gate is the primary control.
func (r RungSpec) PricePerToken() float64 {
	if r.Kind == KindLocal {
		return r.SyntheticUSDPer1MTokens / 1e6
	}
	if r.EstTokens <= 0 {
		return 0 // unreachable: Load rejects a rung with est_tokens <= 0
	}
	return r.EstCostUSD / float64(r.EstTokens)
}

// MeterConfig holds gonk-meter's operational knobs, all with fail-closed
// defaults.
type MeterConfig struct {
	MaxSpendStaleness  time.Duration
	ReservationTTL     time.Duration
	MaxClockSkew       time.Duration
	KeyRetryBackoff    time.Duration
	MaxInfraRetries    int
	EnforceLadderOrder bool
}

// OperatorConfig is the validated operator layer.
type OperatorConfig struct {
	Instance gonkcfg.Policy
	Groups   map[string]gonkcfg.Policy
	Catalog  map[string]RungSpec
	Meter    MeterConfig
}

// rawConfig is the on-disk shape. Durations are strings in YAML ("5m") and
// time.Duration in the parsed struct, so the two shapes are separate types.
type rawConfig struct {
	Version  int                       `yaml:"version"`
	Instance gonkcfg.Policy            `yaml:"instance"`
	Groups   map[string]gonkcfg.Policy `yaml:"groups"`
	Rungs    []RungSpec                `yaml:"rungs"`
	Meter    struct {
		MaxSpendStaleness  *string `yaml:"max_spend_staleness"`
		ReservationTTL     *string `yaml:"reservation_ttl"`
		MaxClockSkew       *string `yaml:"max_clock_skew"`
		KeyRetryBackoff    *string `yaml:"key_retry_backoff"`
		MaxInfraRetries    *int    `yaml:"max_infra_retries"`
		EnforceLadderOrder *bool   `yaml:"enforce_ladder_order"`
	} `yaml:"meter"`
}

// Load validates raw operator-config bytes and decodes them. Every error is
// fatal: gonk-meter must not start on a config it cannot make sense of,
// because every budget decision it makes flows from this file.
func Load(raw []byte) (*OperatorConfig, error) {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("operator config: not valid YAML: %w", err)
	}
	// Same guard as gonkcfg.Validate: jsonschema/v6 v6.0.2 SIGSEGVs on a
	// non-finite float reaching a "minimum" keyword. Operator config is
	// trusted-ish, but a typo must not be a crash.
	if err := rejectNonFinite(doc); err != nil {
		return nil, fmt.Errorf("operator config: %w", err)
	}
	if err := compiled.Validate(doc); err != nil {
		return nil, fmt.Errorf("operator config: %w", err)
	}

	var rc rawConfig
	if err := yaml.Unmarshal(raw, &rc); err != nil {
		return nil, fmt.Errorf("operator config: %w", err)
	}

	oc := &OperatorConfig{
		Instance: rc.Instance,
		Groups:   rc.Groups,
		Catalog:  make(map[string]RungSpec, len(rc.Rungs)),
		Meter: MeterConfig{ // fail-closed defaults
			MaxSpendStaleness:  5 * time.Minute,
			ReservationTTL:     60 * time.Minute,
			MaxClockSkew:       5 * time.Minute,
			KeyRetryBackoff:    5 * time.Minute,
			MaxInfraRetries:    5,
			EnforceLadderOrder: true,
		},
	}

	// Parse the meter knobs FIRST: the checks below consult
	// Meter.EnforceLadderOrder, so it must already hold the operator's value
	// rather than the default.
	m := &oc.Meter
	for _, d := range []struct {
		raw *string
		dst *time.Duration
		key string
	}{
		{rc.Meter.MaxSpendStaleness, &m.MaxSpendStaleness, "max_spend_staleness"},
		{rc.Meter.ReservationTTL, &m.ReservationTTL, "reservation_ttl"},
		{rc.Meter.MaxClockSkew, &m.MaxClockSkew, "max_clock_skew"},
		{rc.Meter.KeyRetryBackoff, &m.KeyRetryBackoff, "key_retry_backoff"},
	} {
		if d.raw == nil {
			continue
		}
		v, err := time.ParseDuration(*d.raw)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("operator config: meter.%s: want a positive duration like \"5m\", got %q", d.key, *d.raw)
		}
		*d.dst = v
	}
	if rc.Meter.MaxInfraRetries != nil {
		m.MaxInfraRetries = *rc.Meter.MaxInfraRetries
	}
	if rc.Meter.EnforceLadderOrder != nil {
		m.EnforceLadderOrder = *rc.Meter.EnforceLadderOrder
	}

	// enforce_ladder_order only has teeth if there is an instance ladder to be
	// ordered against: CheckLadderOrder imposes nothing when the instance is
	// silent, so a project could still put a cloud rung first. Refuse to start
	// rather than pretend the guard is on. (This also discharges PLAN.md's
	// Plan-05 carry-forward: the chart's default values must ship a non-empty
	// instance ladder.)
	if m.EnforceLadderOrder && len(rc.Instance.Ladder) == 0 {
		return nil, fmt.Errorf("operator config: meter.enforce_ladder_order is on but instance.ladder is empty, so nothing constrains a project's rung order; set an instance ladder or turn the guard off deliberately")
	}

	for _, r := range rc.Rungs {
		if _, dup := oc.Catalog[r.Name]; dup {
			return nil, fmt.Errorf("operator config: rung %q defined twice", r.Name)
		}
		// EVERY rung must estimate its token consumption, local ones included.
		// Two reasons, both load-bearing:
		//   1. rung.Reserve reserves EstTokens. A rung estimating 0 tokens holds
		//      NO token budget, so N concurrent sessions can blow monthly_tokens
		//      and per_task_tokens freely. And under Decision 9 the token ceiling
		//      is converted into LiteLLM's USD key ceiling VIA est_tokens, so a
		//      rung with no token estimate gets NO HARD DOOR either -- it breaks
		//      both layers of the defence at once.
		//   2. Local rungs are cost-0 dollars but are metered in tokens for
		//      fairness visibility (spec 6.3). A local rung with no token
		//      estimate is invisible to exactly the accounting it exists for.
		if r.EstTokens <= 0 {
			return nil, fmt.Errorf("operator config: rung %q needs est_tokens > 0; a rung that reserves no tokens cannot be held against monthly_tokens or per_task_tokens, and LiteLLM does not enforce token budgets", r.Name)
		}
		switch r.Kind {
		case KindCloud:
			// rung.Decide applies the cost gate ONLY to rungs with a positive
			// REAL estimate. An unpriced cloud rung would sail past a $0 budget.
			if !(r.EstCostUSD > 0) {
				return nil, fmt.Errorf("operator config: cloud rung %q needs est_cost_usd > 0, else it bypasses the cost budget", r.Name)
			}
			// A cloud rung's price is REAL and lives in LiteLLM's model config.
			// A synthetic price for it would be a lie, and PricePerToken would
			// silently use the wrong one.
			if r.SyntheticUSDPer1MTokens != 0 {
				return nil, fmt.Errorf("operator config: cloud rung %q must not set synthetic_usd_per_1m_tokens; its price is real and comes from LiteLLM's model config", r.Name)
			}
		case KindLocal:
			if r.EstCostUSD != 0 {
				return nil, fmt.Errorf("operator config: local rung %q must have est_cost_usd 0 (local inference costs no real money; price it as a cloud rung if it does)", r.Name)
			}
			// DECISION 9. Without a synthetic price, a local rung costs LiteLLM's
			// USD counter nothing, the virtual-key ceiling never moves for it, and
			// monthly_tokens has NO HARD DOOR ANYWHERE. Meter's reservation would
			// be the only protection -- which is exactly the hole the synthetic
			// price exists to close. Refuse to start.
			if !(r.SyntheticUSDPer1MTokens > 0) {
				return nil, fmt.Errorf("operator config: local rung %q needs synthetic_usd_per_1m_tokens > 0; without a synthetic price LiteLLM's USD key ceiling never moves for this rung and monthly_tokens has no hard enforcement (see ADR-004, Decision 9)", r.Name)
			}
		}
		oc.Catalog[r.Name] = r
	}

	if err := oc.checkPolicy("instance", rc.Instance); err != nil {
		return nil, err
	}
	for g, p := range rc.Groups {
		if err := oc.checkPolicy("group "+g, p); err != nil {
			return nil, err
		}
		// A group ladder may narrow the instance's, but may not REORDER it --
		// the same rule the project ladder obeys, for the same reason (spec 6.3:
		// start at the cheapest rung).
		if oc.Meter.EnforceLadderOrder {
			if err := CheckLadderOrder(rc.Instance.Ladder, p.Ladder); err != nil {
				return nil, fmt.Errorf("operator config: group %s: %w", g, err)
			}
		}
	}

	return oc, nil
}

// checkPolicy enforces what the JSON Schema cannot: cross-references into the
// rung catalog, and timezone validity.
func (oc *OperatorConfig) checkPolicy(who string, p gonkcfg.Policy) error {
	for _, r := range p.Ladder {
		if _, ok := oc.Catalog[r]; !ok {
			return fmt.Errorf("operator config: %s ladder names rung %q, which is not in the rung catalog", who, r)
		}
	}
	if p.Schedule != nil && p.Schedule.Timezone != "" {
		if _, err := time.LoadLocation(p.Schedule.Timezone); err != nil {
			return fmt.Errorf("operator config: %s schedule.timezone %q: %w", who, p.Schedule.Timezone, err)
		}
	}
	return nil
}

// GroupFor returns the single group Policy that applies to a project path, by
// FOLDING every matching ancestor group coarsest-first.
//
// Returning only the longest-prefix match would be a budget escape: with groups
// "agentic" (ceiling $50) and "agentic/experiments" (which sets no budget),
// project agentic/experiments/spike would inherit NO group ceiling at all and be
// bounded only by the instance's $200. Spec 5.4 and ADR-002 say ceilings only
// tighten downward, so a nested group must never be able to LOOSEN its parent's.
//
// The fold uses exactly ADR-002's semantics, one layer at a time: budgets take
// the minimum, `enabled` and `actions` are vetoable (an explicit false anywhere
// in the chain wins), ladders intersect, and other scalars are most-specific-wins.
//
// Prefixes match on SEGMENT boundaries only: group "agentic" does not capture
// project "agentic-other/repo".
func (oc *OperatorConfig) GroupFor(project string) gonkcfg.Policy {
	var matches []string
	for g := range oc.Groups {
		if project == g || strings.HasPrefix(project, g+"/") {
			matches = append(matches, g)
		}
	}
	// Coarsest first, so the most specific layer is folded last.
	sort.Slice(matches, func(i, j int) bool { return len(matches[i]) < len(matches[j]) })

	var out gonkcfg.Policy
	for _, g := range matches {
		out = foldPolicy(out, oc.Groups[g])
	}
	return out
}

// foldPolicy folds `next` (more specific) onto `base` (coarser) using ADR-002's
// precedence rules. It exists because gonkcfg.Resolve takes exactly ONE group
// layer, and a nested group hierarchy has several.
func foldPolicy(base, next gonkcfg.Policy) gonkcfg.Policy {
	out := base

	// Vetoes: an explicit false at ANY level wins, so once false, stays false.
	if next.Enabled != nil {
		if base.Enabled == nil || *base.Enabled {
			out.Enabled = next.Enabled
		}
	}
	out.Actions = gonkcfg.ActionsPolicy{
		Triage:    foldVeto(base.Actions.Triage, next.Actions.Triage),
		Pipelines: foldVeto(base.Actions.Pipelines, next.Actions.Pipelines),
		Features:  foldVeto(base.Actions.Features, next.Actions.Features),
	}

	// Ceilings only tighten: minimum over the layers that set one.
	out.Budget = gonkcfg.BudgetPolicy{
		MonthlyCostUSD: minFloat(base.Budget.MonthlyCostUSD, next.Budget.MonthlyCostUSD),
		MonthlyTokens:  minTokens(base.Budget.MonthlyTokens, next.Budget.MonthlyTokens),
		PerTaskTokens:  minTokens(base.Budget.PerTaskTokens, next.Budget.PerTaskTokens),
	}

	// Ladder: the more specific list is the base ordering, narrowed by the
	// coarser allow-list. Same rule Resolve applies.
	switch {
	case next.Ladder != nil && base.Ladder != nil:
		allowed := make(map[string]struct{}, len(base.Ladder))
		for _, r := range base.Ladder {
			allowed[r] = struct{}{}
		}
		// NOT `var l []string`. An empty intersection must be a NON-NIL
		// zero-length slice, because Resolve's allow-list fold is gated on
		// `Ladder != nil` -- ADR-002 says so in as many words ("a naive
		// `Ladder == nil` check would miss the non-nil zero-length slice an
		// empty intersection produces"). A nil here means "this layer is
		// silent", so a group hierarchy that allows NO rung would be read as
		// imposing no constraint, and the project's cloud rung would run.
		l := next.Ladder[:0:0]
		for _, r := range next.Ladder {
			if _, ok := allowed[r]; ok {
				l = append(l, r)
			}
		}
		out.Ladder = l
	case next.Ladder != nil:
		out.Ladder = append([]string(nil), next.Ladder...)
	}

	// Most-specific-wins scalars.
	if next.Schedule != nil {
		sc := *next.Schedule
		out.Schedule = &sc
	}
	if next.Continuity != nil {
		out.Continuity = next.Continuity
	}
	if next.Triage.LabelPrefix != nil {
		out.Triage.LabelPrefix = next.Triage.LabelPrefix
	}
	if next.Triage.RespondToMentions != nil {
		out.Triage.RespondToMentions = next.Triage.RespondToMentions
	}
	if next.Provenance.CommitTrailers != nil {
		out.Provenance.CommitTrailers = next.Provenance.CommitTrailers
	}
	if next.Provenance.IncludeUsage != nil {
		out.Provenance.IncludeUsage = next.Provenance.IncludeUsage
	}
	return out
}

// foldVeto: nil means "silent". An explicit false anywhere in the chain sticks.
func foldVeto(base, next *bool) *bool {
	if base != nil && !*base {
		return base
	}
	if next != nil {
		return next
	}
	return base
}

func minFloat(a, b *float64) *float64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b < *a:
		return b
	}
	return a
}

func minTokens(a, b *gonkcfg.TokenQuantity) *gonkcfg.TokenQuantity {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b < *a:
		return b
	}
	return a
}

// CheckLadderOrder reports whether a project's ladder respects the instance's
// escalation ordering. gonkcfg.Resolve preserves the PROJECT's order when it
// intersects (ADR-002), so without this a project could list [sonnet,
// qwen-local] and make a cloud rung its first attempt -- while spec 6.3 says
// "everything starts at the cheapest rung its project config allows". A
// project's ladder must be a SUBSEQUENCE of the instance's. If the instance
// sets no ladder, it imposes no ordering.
func CheckLadderOrder(instance, project []string) error {
	if len(instance) == 0 || len(project) == 0 {
		return nil
	}
	pos := make(map[string]int, len(instance))
	for i, r := range instance {
		pos[r] = i
	}
	last := -1
	for _, r := range project {
		i, ok := pos[r]
		if !ok {
			continue // Resolve's intersection will drop it anyway
		}
		if i < last {
			return fmt.Errorf("ladder order: %v reorders the instance ladder %v; rungs must escalate cheapest-first (spec 6.3)", project, instance)
		}
		last = i
	}
	return nil
}

func rejectNonFinite(v any) error {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return fmt.Errorf("not a finite number")
		}
	case map[string]any:
		for _, val := range t {
			if err := rejectNonFinite(val); err != nil {
				return err
			}
		}
	case map[any]any:
		for _, val := range t {
			if err := rejectNonFinite(val); err != nil {
				return err
			}
		}
	case []any:
		for _, val := range t {
			if err := rejectNonFinite(val); err != nil {
				return err
			}
		}
	}
	return nil
}
```

**Note for the implementer:** `gonkcfg.Policy`'s YAML tags already match this schema's `$defs/policy` (that is why the schema mirrors `.gonk.yml`'s shape), so `yaml.Unmarshal` into `gonkcfg.Policy` works directly — including `TokenQuantity.UnmarshalYAML`, which is what rejects `monthly_tokens: 1.5` at the *type* level even though the schema's `oneOf` would let `2.0` through as a zero-fraction integer. Both layers must run; do not skip the decode.

- [ ] **Step 5: Watch the tests pass**

Run: `go test ./pkg/opercfg/ -race -v`
Expected: PASS. If `unknown timezone` fails, confirm `time.LoadLocation` has a tzdata source — see Task 8, which imports `_ "time/tzdata"` in `main.go` so the container image needs no zoneinfo files.

- [ ] **Step 6: Publish the schema and gate it against drift**

Same pattern as `pkg/gonkcfg` (Plan 01 Task 6).

```bash
mkdir -p pkg/opercfg/testdata
cp pkg/opercfg/gonk-operator.v1.schema.json docs/schemas/
sha256sum pkg/opercfg/gonk-operator.v1.schema.json | cut -d' ' -f1 > pkg/opercfg/testdata/schema.v1.sha256
```

`pkg/opercfg/drift_test.go`:

```go
package opercfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPublishedSchemaMatchesCanonical(t *testing.T) {
	published, err := os.ReadFile("../../docs/schemas/gonk-operator.v1.schema.json")
	if err != nil {
		t.Fatalf("published schema missing: %v", err)
	}
	if !bytes.Equal(published, schemaJSON) {
		t.Fatal("docs/schemas copy differs from the embedded canonical schema; re-run the publish copy step")
	}
}

func TestSchemaChangeIsDeliberate(t *testing.T) {
	want, err := os.ReadFile("testdata/schema.v1.sha256")
	if err != nil {
		t.Fatalf("checksum file missing: %v", err)
	}
	sum := sha256.Sum256(schemaJSON)
	if got := hex.EncodeToString(sum[:]); got != strings.TrimSpace(string(want)) {
		t.Fatalf("operator schema changed (sha256 %s). Additive change -> update testdata/schema.v1.sha256 and the docs copy; breaking change -> new version file + SchemaVersion bump. See spec 10.1.", got)
	}
}

// Plan 01's carry-forward for whoever cuts a v2: keep the const and the schema
// in step. Exactly one test enforces it, and this is it.
func TestSchemaVersionConstMatchesEmbeddedSchema(t *testing.T) {
	var s struct {
		Properties struct {
			Version struct {
				Const int `json:"const"`
			} `json:"version"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		t.Fatal(err)
	}
	if s.Properties.Version.Const != SchemaVersion {
		t.Fatalf("SchemaVersion = %d but schema says version const = %d", SchemaVersion, s.Properties.Version.Const)
	}
}
```

- [ ] **Step 7: Prove the gate gates**

Run: `printf ' ' >> pkg/opercfg/gonk-operator.v1.schema.json && go test ./pkg/opercfg/ -run Schema ; git checkout pkg/opercfg/gonk-operator.v1.schema.json`
Expected: FAIL (both drift tests) while modified, then restored.

- [ ] **Step 8: Full gate and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add pkg/opercfg docs/schemas/gonk-operator.v1.schema.json
git commit -m "feat(opercfg): validated operator config (closes ADR-002 known gap), rung catalog, drift gates"
```

---

### Task 3: `pkg/spend` — the budget window and the ledger rows

The month window is where clock skew becomes money. `Advance` is **monotone**: a backwards clock jump (NTP correction, VM snapshot restore, a node with a dead RTC) must never roll a project back into a fresh budget. Forwards skew is not solved here — it is a *health* problem, handled by `SkewOK` and `/readyz` (Task 8), because no arithmetic can distinguish "it is really August" from "my clock is wrong".

**Files:** Create `pkg/spend/window.go`, `pkg/spend/window_test.go`, `pkg/spend/rows.go`, `pkg/spend/rows_test.go`

- [ ] **Step 1: Write the failing window test**

`pkg/spend/window_test.go`:

```go
package spend

import (
	"testing"
	"time"

	_ "time/tzdata" // or LoadLocation returns nil and time.Date PANICS
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMonthWindow(t *testing.T) {
	w := MonthWindow(at("2026-07-13T10:30:00Z"))
	if !w.Start.Equal(at("2026-07-01T00:00:00Z")) || !w.End.Equal(at("2026-08-01T00:00:00Z")) {
		t.Fatalf("July window = %v", w)
	}
	// A non-UTC input still yields the UTC calendar month it falls in.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("tzdata unavailable: %v", err) // never a nil *Location into time.Date
	}
	w = MonthWindow(time.Date(2026, 7, 31, 21, 0, 0, 0, ny)) // = 2026-08-01T01:00Z
	if !w.Start.Equal(at("2026-08-01T00:00:00Z")) {
		t.Fatalf("a local-July instant that is already August in UTC must land in August: %v", w)
	}
	// December rolls the year.
	w = MonthWindow(at("2026-12-31T23:59:59Z"))
	if !w.End.Equal(at("2027-01-01T00:00:00Z")) {
		t.Fatalf("December window = %v", w)
	}
}

func TestAdvanceIsMonotone(t *testing.T) {
	july := MonthWindow(at("2026-07-13T00:00:00Z"))

	if w, rolled := Advance(july, at("2026-07-31T23:59:59Z")); rolled || w != july {
		t.Fatal("rolled over before the window ended")
	}
	if w, rolled := Advance(july, at("2026-08-01T00:00:00Z")); !rolled || !w.Start.Equal(at("2026-08-01T00:00:00Z")) {
		t.Fatalf("failed to roll exactly at the boundary: %v %v", w, rolled)
	}
	// THE money case: the clock jumps backwards into last month. If we rolled
	// back, July's spend would vanish and the project would get a free budget.
	if w, rolled := Advance(july, at("2026-06-15T00:00:00Z")); rolled || w != july {
		t.Fatalf("a backwards clock jump reset the budget window: %v", w)
	}
	// A long outage: skipping a month is fine, we land on the current one.
	if w, rolled := Advance(july, at("2026-10-05T00:00:00Z")); !rolled || !w.Start.Equal(at("2026-10-01T00:00:00Z")) {
		t.Fatalf("failed to catch up after an outage: %v", w)
	}
	// A zero window (cold start) always initializes.
	if w, rolled := Advance(Window{}, at("2026-07-13T00:00:00Z")); !rolled || w != july {
		t.Fatalf("cold start = %v %v", w, rolled)
	}
}

func TestSkewOK(t *testing.T) {
	local := at("2026-07-13T10:00:00Z")
	max := 5 * time.Minute
	for _, c := range []struct {
		remote time.Time
		want   bool
	}{
		{at("2026-07-13T10:04:59Z"), true},
		{at("2026-07-13T09:55:01Z"), true},
		{at("2026-07-13T10:05:01Z"), false}, // we are behind
		{at("2026-07-13T09:54:59Z"), false}, // we are ahead -- the rollover risk
		{time.Time{}, true},                 // no Date header: cannot judge, do not block
	} {
		if got := SkewOK(local, c.remote, max); got != c.want {
			t.Errorf("SkewOK(local=%v, remote=%v) = %v, want %v", local, c.remote, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run it, watch it fail.** Run: `go test ./pkg/spend/ -v` — FAIL, undefined `Window`/`MonthWindow`/`Advance`/`SkewOK`.

- [ ] **Step 3: Implement `pkg/spend/window.go`**

```go
// Package spend is the ledger side of gonk-meter: the budget window that spend
// is measured over, and the spend-log rows joined to attribution tags.
package spend

import "time"

// Window is the half-open budget period [Start, End). It is the UTC calendar
// month -- not the project's local month, not a rolling 30 days. One global
// boundary means two projects in different timezones cannot disagree about
// which month a charge lands in, and it is the boundary LiteLLM's own key
// budget_duration must be configured to match (see AD-9).
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (w Window) Contains(t time.Time) bool {
	u := t.UTC()
	return !u.Before(w.Start) && u.Before(w.End)
}

func (w Window) Zero() bool { return w.Start.IsZero() }

// MonthWindow is the UTC calendar month containing now.
func MonthWindow(now time.Time) Window {
	u := now.UTC()
	start := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return Window{Start: start, End: start.AddDate(0, 1, 0)}
}

// Advance returns the window that should be in force at now, given the one
// currently in force. It is MONOTONE: it never returns a window that starts
// earlier than prev.
//
// This is a money invariant, not tidiness. A backwards clock jump -- an NTP
// correction, a restored VM snapshot, a node with a dead RTC -- would otherwise
// move the window back into a month whose spend has already been counted,
// discard it, and hand every project a second budget. Refusing to move
// backwards costs nothing (a genuinely-past window is simply the one we are
// already in) and closes the hole completely.
//
// Forwards skew is NOT solved here: no arithmetic can tell "it is really
// August" from "my clock is wrong". That is a health problem -- see SkewOK,
// which fails /readyz and defers every decision rather than guessing.
func Advance(prev Window, now time.Time) (Window, bool) {
	next := MonthWindow(now)
	if prev.Zero() {
		return next, true
	}
	if now.UTC().Before(prev.End) {
		return prev, false // still inside it, INCLUDING a jump backwards
	}
	if !next.Start.After(prev.Start) {
		return prev, false // belt and braces: never move backwards
	}
	return next, true
}

// SkewOK reports whether our clock agrees with the spend source's clock (its
// HTTP Date header) closely enough to be trusted with a rollover decision. A
// zero remote time means the source told us nothing, which is not evidence of
// skew -- do not block the factory on a missing header.
func SkewOK(local, remote time.Time, max time.Duration) bool {
	if remote.IsZero() {
		return true
	}
	d := local.Sub(remote)
	if d < 0 {
		d = -d
	}
	return d <= max
}
```

- [ ] **Step 4: Watch it pass.** Run: `go test ./pkg/spend/ -run 'Window|Advance|Skew' -v` — PASS.

- [ ] **Step 5: Write the failing rows test**

`pkg/spend/rows_test.go`:

```go
package spend

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

// row mimics what the ingest path does: it sets Synthetic from the rung's kind.
// Here, "qwen-local" is the local rung, so its dollars are an accounting fiction
// (Decision 9) and must land in SyntheticCostUSD, never in CostUSD.
func row(callID, bead, session, rung string, cost float64, prompt, completion int64, ts string) Row {
	return Row{
		CallID: callID,
		Tags: atags.Tags{
			Project: "group/repo", Rig: "group-repo", BeadID: bead, SessionKey: session,
			Rung: rung, Attempt: 1, Trigger: atags.TriggerIssueTriage,
		},
		CostUSD: cost, PromptTokens: prompt, CompletionTokens: completion, At: at(ts),
		Synthetic: rung == "qwen-local",
	}
}

func rows() []Row {
	return []Row{
		row("c1", "gk-1", "s1", "qwen-local", 0, 10_000, 2_000, "2026-07-02T10:00:00Z"),
		row("c2", "gk-1", "s1", "qwen-local", 0, 5_000, 1_000, "2026-07-02T10:01:00Z"),
		row("c3", "gk-1", "s2", "glm", 0.40, 8_000, 1_500, "2026-07-05T10:00:00Z"),
		row("c4", "gk-2", "s3", "glm", 0.25, 4_000, 500, "2026-07-06T10:00:00Z"),
		// Last month: inside the bead's lifetime, OUTSIDE the budget window.
		row("c5", "gk-1", "s0", "qwen-local", 0, 90_000, 9_000, "2026-06-28T10:00:00Z"),
	}
}

func TestProjectTotalsRespectsTheWindow(t *testing.T) {
	w := MonthWindow(at("2026-07-13T00:00:00Z"))
	got := ProjectTotals(rows(), w, "group/repo")
	// c1..c4 only: c5 is June.
	if got.CostUSD != 0.65 {
		t.Errorf("cost = %v, want 0.65", got.CostUSD)
	}
	// c1 12k + c2 6k + c3 9.5k + c4 4.5k = 32k. (c5 is June: out of the window.)
	if got.TotalTokens() != 32_000 {
		t.Errorf("tokens = %d, want 32000", got.TotalTokens())
	}
	if got.Calls != 4 {
		t.Errorf("calls = %d, want 4", got.Calls)
	}
}

// The per-task ceiling is a property of the WORK ITEM, not the month: a bead
// that straddles a month boundary keeps spending against the same per-task
// budget. So bead totals are lifetime, not windowed.
func TestBeadTotalsAreLifetime(t *testing.T) {
	got := BeadTotals(rows(), "group/repo", "gk-1")
	if got.TotalTokens() != 126_500 { // includes June's c5
		t.Fatalf("bead tokens = %d, want 126500 (June's row must count)", got.TotalTokens())
	}
	if got.CostUSD != 0.40 {
		t.Fatalf("bead cost = %v, want 0.40", got.CostUSD)
	}
}

func TestSessionTotals(t *testing.T) {
	got := SessionTotals(rows(), "s1")
	if got.Calls != 2 || got.TotalTokens() != 18_000 {
		t.Fatalf("session s1 = %+v", got)
	}
}

// Spend logs are polled with overlapping windows, so the same call WILL be
// read twice. Double-counting is a wrong budget.
func TestDedupeByCallID(t *testing.T) {
	dup := append(rows(), row("c3", "gk-1", "s2", "glm", 0.40, 8_000, 1_500, "2026-07-05T10:00:00Z"))
	deduped, dropped := Dedupe(dup)
	if dropped != 1 || len(deduped) != len(rows()) {
		t.Fatalf("Dedupe dropped %d, len %d", dropped, len(deduped))
	}
	w := MonthWindow(at("2026-07-13T00:00:00Z"))
	if got := ProjectTotals(deduped, w, "group/repo"); got.CostUSD != 0.65 {
		t.Fatalf("cost after dedupe = %v, want 0.65", got.CostUSD)
	}
}

func TestByRungAndByTrigger(t *testing.T) {
	w := MonthWindow(at("2026-07-13T00:00:00Z"))
	byRung := ByRung(rows(), w, "group/repo")
	if byRung["glm"].CostUSD != 0.65 || byRung["qwen-local"].CostUSD != 0 {
		t.Fatalf("by rung = %+v", byRung)
	}
	if byRung["qwen-local"].TotalTokens() != 18_000 {
		t.Fatalf("local rungs cost no REAL money but MUST still be metered in tokens (spec 6.3): %+v", byRung["qwen-local"])
	}
	if got := ByTrigger(rows(), w, "group/repo")[atags.TriggerIssueTriage].Calls; got != 4 {
		t.Fatalf("by trigger calls = %d, want 4", got)
	}
}

// DECISION 9. A local rung's LiteLLM `spend` is a SYNTHETIC dollar -- priced only
// so that the USD virtual-key ceiling is a hard door for tokens. It must land in
// SyntheticCostUSD and NEVER in CostUSD, or it flows into rung.Decide's cost gate
// and bricks every project whose monthly_cost_usd is 0 (i.e. every freshly
// onboarded one).
func TestSyntheticSpendIsNeverCountedAsRealSpend(t *testing.T) {
	w := MonthWindow(at("2026-07-13T00:00:00Z"))
	// A local row that LiteLLM billed at a synthetic $2.50.
	rs := append(rows(), row("c6", "gk-3", "s4", "qwen-local", 2.50, 50_000, 5_000, "2026-07-07T10:00:00Z"))

	got := ProjectTotals(rs, w, "group/repo")
	if got.CostUSD != 0.65 {
		t.Fatalf("real cost = %v, want 0.65: a synthetic local-rung dollar was counted as real spend", got.CostUSD)
	}
	if got.SyntheticCostUSD != 2.50 {
		t.Fatalf("synthetic cost = %v, want 2.50 (it must be reported, just never as spend)", got.SyntheticCostUSD)
	}
	if byRung := ByRung(rs, w, "group/repo"); byRung["qwen-local"].CostUSD != 0 ||
		byRung["qwen-local"].SyntheticCostUSD != 2.50 {
		t.Fatalf("qwen-local = %+v; local dollars are synthetic, and the split must survive grouping", byRung["qwen-local"])
	}
}

// THE MONTH-ROLLOVER MONEY BUG. Spend rows are POLLED (Decision 11), so a row can
// land days after the call it describes -- including after the budget window has
// rolled. Rows must be windowed by the ROW'S OWN TIMESTAMP, never by ingest time.
//
// Get this wrong and, on the 1st of every month, July's late-arriving rows are
// charged to August -- or worse, July's spend is never charged at all and every
// project silently gets a free budget.
func TestALateRowIsChargedToTheMonthItHappenedIn(t *testing.T) {
	// It is now 1 August. This row is for a call made on 31 July, and it has only
	// just landed.
	late := row("c9", "gk-9", "s9", "glm", 5.00, 10_000, 1_000, "2026-07-31T23:59:00Z")

	august := MonthWindow(at("2026-08-01T00:30:00Z"))
	if got := ProjectTotals([]Row{late}, august, "group/repo"); got.CostUSD != 0 {
		t.Fatalf("a July call was charged $%.2f against AUGUST's budget", got.CostUSD)
	}
	july := MonthWindow(at("2026-07-15T00:00:00Z"))
	if got := ProjectTotals([]Row{late}, july, "group/repo"); got.CostUSD != 5.00 {
		t.Fatalf("a late-landing July call was charged $%.2f against July, want $5.00; July's spend has vanished", got.CostUSD)
	}
}
```

- [ ] **Step 6: Run it, watch it fail.** Run: `go test ./pkg/spend/ -run 'Totals|Dedupe|By' -v` — FAIL.

- [ ] **Step 7: Implement `pkg/spend/rows.go`**

```go
package spend

import (
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

// Row is one LiteLLM spend-log entry, joined to its attribution tags. The tags
// ARE the join key: pkg/atags is the contract that connects a model call to a
// bead, a session, and a GitLab artifact (spec 6.1).
type Row struct {
	CallID           string     // LiteLLM request id; the dedupe key
	Tags             atags.Tags // gonk_project, gonk_bead_id, ... (spec 6.1)
	CostUSD          float64    // as LiteLLM billed it -- REAL for cloud, SYNTHETIC for local
	PromptTokens     int64
	CompletionTokens int64
	At               time.Time // UTC

	// Synthetic marks a row whose CostUSD is an accounting fiction: a local
	// model, priced in LiteLLM only so that its USD virtual-key ceiling is a hard
	// door for TOKENS (Decision 9).
	//
	// It is set AT INGEST, from the rung catalog: catalog[Tags.Rung].Kind ==
	// KindLocal. A row whose rung is NOT IN THE CATALOG is treated as REAL --
	// fail closed, so an unknown rung counts against the money ceiling rather
	// than being waved through as fiction.
	//
	// LiteLLM has one `spend` column and does not know the difference. This flag
	// is the ONLY thing that keeps synthetic dollars out of the cost gate and off
	// the "real spend" panel.
	Synthetic bool
}

// Totals is an aggregate over rows. CostUSD and SyntheticCostUSD are DIFFERENT
// CURRENCIES: never add them together, and never show them in one number.
type Totals struct {
	CostUSD          float64 `json:"cost_usd"`           // real money (cloud rungs)
	SyntheticCostUSD float64 `json:"synthetic_cost_usd"` // accounting fiction (local rungs)
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Calls            int     `json:"calls"`
}

func (t Totals) TotalTokens() int64 { return t.PromptTokens + t.CompletionTokens }

func (t *Totals) add(r Row) {
	if r.Synthetic {
		t.SyntheticCostUSD += r.CostUSD
	} else {
		t.CostUSD += r.CostUSD
	}
	t.PromptTokens += r.PromptTokens
	t.CompletionTokens += r.CompletionTokens
	t.Calls++
}

// Sum aggregates the rows that match.
func Sum(rows []Row, match func(Row) bool) Totals {
	var out Totals
	for _, r := range rows {
		if match(r) {
			out.add(r)
		}
	}
	return out
}

// ProjectTotals is spend against the MONTHLY ceilings: windowed.
func ProjectTotals(rows []Row, w Window, project string) Totals {
	return Sum(rows, func(r Row) bool {
		return r.Tags.Project == project && w.Contains(r.At)
	})
}

// BeadTotals is spend against the PER-TASK ceiling: lifetime, not windowed. A
// bead that straddles a month boundary keeps the same per-task budget -- the
// ceiling is a property of the work item, not of the calendar.
func BeadTotals(rows []Row, project, beadID string) Totals {
	return Sum(rows, func(r Row) bool {
		return r.Tags.Project == project && r.Tags.BeadID == beadID
	})
}

// SessionTotals is what commit provenance trailers report (spec 6.1).
func SessionTotals(rows []Row, sessionKey string) Totals {
	return Sum(rows, func(r Row) bool { return r.Tags.SessionKey == sessionKey })
}

func ByRung(rows []Row, w Window, project string) map[string]Totals {
	return groupBy(rows, w, project, func(r Row) string { return r.Tags.Rung })
}

func ByTrigger(rows []Row, w Window, project string) map[string]Totals {
	return groupBy(rows, w, project, func(r Row) string { return r.Tags.Trigger })
}

func groupBy(rows []Row, w Window, project string, key func(Row) string) map[string]Totals {
	out := map[string]Totals{}
	for _, r := range rows {
		if r.Tags.Project != project || !w.Contains(r.At) {
			continue
		}
		t := out[key(r)]
		t.add(r)
		out[key(r)] = t
	}
	return out
}

// Dedupe removes rows whose CallID has already been seen, keeping the first.
// Spend logs are polled with overlapping time windows, so the same call WILL
// be read more than once; counting it twice is a wrong budget, which is a
// wrong decision.
func Dedupe(rows []Row) ([]Row, int) {
	seen := make(map[string]struct{}, len(rows))
	out := rows[:0:0]
	dropped := 0
	for _, r := range rows {
		if _, dup := seen[r.CallID]; dup {
			dropped++
			continue
		}
		seen[r.CallID] = struct{}{}
		out = append(out, r)
	}
	return out, dropped
}
```

- [ ] **Step 8: Watch it pass, gate, commit**

```bash
go test ./pkg/spend/ -race -v
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add pkg/spend && git commit -m "feat(spend): monotone UTC budget window, clock-skew check, ledger row aggregation"
```

---

### Task 4: `pkg/rung` — the deterministic ladder policy (the heart of this plan)

`Decide(Input) Decision` is a **pure function of (config, spend, attempts, clock)**. No LLM judges (spec 6.3). No network. No `time.Now()` — the clock is an input, so every case including month rollover and quiet hours is a table row.

Two rules do the most work:
- The **rung index is the count of prior gate failures.** Infra failures (connection errors, LiteLLM 5xx, pod evictions, expired reservations) retry the *same* rung and never escalate. This is spec 6.3's central promise and it is one line — guard it with tests, because it is exactly the kind of thing a refactor quietly breaks.
- The **cost gate applies only to priced rungs.** `opercfg` guarantees cloud rungs are priced and local rungs are not, so "a project with `monthly_cost_usd: 0` runs local rungs freely and can never touch a cloud rung" falls out of the arithmetic instead of being a special case. That is exactly the conservative default `.gonk.yml` the onboarding MR ships (spec 5.3).

**Files:** Create `pkg/rung/outcome.go`, `pkg/rung/decide.go`, `pkg/rung/decide_test.go`, `pkg/rung/mutation_test.go`

- [ ] **Step 1: Write `pkg/rung/outcome.go` first (it is pure data, and the test needs it)**

```go
package rung

// Outcome is how one session attempt ended. The classification is STRICT and
// it is the entire escalation trigger (spec 6.3): only a completed-but-failed
// gate escalates. Infrastructure failures must never cost a project a rung --
// a flaky pod is not evidence that a bigger model is needed.
type Outcome string

const (
	// OutcomeSuccess: the session completed and passed its gate.
	OutcomeSuccess Outcome = "success"
	// OutcomeGateFailed: the session COMPLETED and FAILED its objective gate
	// (v1 triage: did not post the required artifacts within the turn/token
	// caps). This is the ONLY outcome that escalates.
	OutcomeGateFailed Outcome = "gate-failed"
	// OutcomeInfraFailed: connection error, LiteLLM 5xx, pod eviction, expired
	// reservation. Retry the SAME rung.
	OutcomeInfraFailed Outcome = "infra-failed"
	// OutcomeAborted: cancelled by a human or by a config change. Neither
	// escalates nor retries.
	OutcomeAborted Outcome = "aborted"
)

func (o Outcome) Valid() bool {
	switch o {
	case OutcomeSuccess, OutcomeGateFailed, OutcomeInfraFailed, OutcomeAborted:
		return true
	}
	return false
}

// Escalates reports whether this outcome advances the ladder.
func (o Outcome) Escalates() bool { return o == OutcomeGateFailed }

// Attempt is one recorded attempt on a bead. Meter owns this history (ADR-004):
// a caller-supplied attempt count would let anyone skip straight to the most
// expensive rung.
type Attempt struct {
	Attempt int     `json:"attempt"` // 1-based
	Rung    string  `json:"rung"`
	Outcome Outcome `json:"outcome"`
}

// Escalations counts the gate failures in an attempt history. This IS the rung
// index: rung N is earned by failing the gate N times.
func Escalations(prior []Attempt) int {
	n := 0
	for _, a := range prior {
		if a.Outcome.Escalates() {
			n++
		}
	}
	return n
}

// ConsecutiveInfraFailures counts infra failures at the END of the history at
// the given rung. A rung that keeps dying on infrastructure must eventually
// stop rather than retry forever.
func ConsecutiveInfraFailures(prior []Attempt, at string) int {
	n := 0
	for i := len(prior) - 1; i >= 0; i-- {
		a := prior[i]
		if a.Outcome == OutcomeInfraFailed && a.Rung == at {
			n++
			continue
		}
		break
	}
	return n
}
```

- [ ] **Step 2: Write the failing decision test**

`pkg/rung/decide_test.go`. Note the two rules this file obeys, both earned by Plan 01's review:

1. **`Effective` is built by calling `gonkcfg.Resolve`, never by struct literal.** ADR-002 says `Resolve` is the only legitimate producer; hand-building one silently violates every invariant. It also means these tests exercise the real precedence chain, so a row where the *instance* ceiling binds actually proves the instance layer is consulted.
2. **Every row uses non-default values.** A test whose expectations coincide with the zero value is a test that a stub would pass.

```go
package rung

import (
	"testing"
	"time"

	// Without tzdata, time.LoadLocation("America/New_York") fails in a scratch
	// container AND in any CI image with no zoneinfo -- and the quiet-hours case
	// would fail for a reason that has nothing to do with the policy.
	_ "time/tzdata"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func b(v bool) *bool                          { return &v }
func f(v float64) *float64                    { return &v }
func tq(v gonkcfg.TokenQuantity) *gonkcfg.TokenQuantity { return &v }

// catalog: qwen-local costs no REAL money (but carries a synthetic price, so
// LiteLLM's USD door still closes on it -- Decision 9); glm is $0.40/attempt and
// sonnet $1.20/attempt of real money.
func catalog() map[string]opercfg.RungSpec {
	return map[string]opercfg.RungSpec{
		"qwen-local": {Name: "qwen-local", Kind: opercfg.KindLocal, Model: "qwen3-coder-30b",
			EstCostUSD: 0, EstTokens: 200_000, SyntheticUSDPer1MTokens: 0.20},
		"glm":    {Name: "glm", Kind: opercfg.KindCloud, Model: "glm-5", EstCostUSD: 0.40, EstTokens: 200_000},
		"sonnet": {Name: "sonnet", Kind: opercfg.KindCloud, Model: "claude-sonnet", EstCostUSD: 1.20, EstTokens: 200_000},
	}
}

// effective builds an Effective the ONLY legitimate way: through Resolve.
// instBudget/projBudget are deliberately different so a resolver that ignored
// a layer would be caught here rather than in gonkcfg's own tests.
func effective(t *testing.T, instance, group gonkcfg.Policy, project gonkcfg.Policy) gonkcfg.Effective {
	t.Helper()
	return gonkcfg.Resolve(instance, group, gonkcfg.ProjectConfig{Version: 1, Policy: project})
}

func instancePolicy() gonkcfg.Policy {
	return gonkcfg.Policy{
		Enabled: b(true),
		Ladder:  []string{"qwen-local", "glm", "sonnet"},
		Budget:  gonkcfg.BudgetPolicy{MonthlyCostUSD: f(200), MonthlyTokens: tq(5_000_000_000)},
	}
}

func projectPolicy() gonkcfg.Policy {
	return gonkcfg.Policy{
		Enabled: b(true),
		Actions: gonkcfg.ActionsPolicy{Triage: b(true)},
		Ladder:  []string{"qwen-local", "glm"},
		Budget: gonkcfg.BudgetPolicy{
			MonthlyCostUSD: f(10),
			MonthlyTokens:  tq(50_000_000),
			PerTaskTokens:  tq(2_000_000),
		},
	}
}

type decideCase struct {
	name string
	in   func(t *testing.T) Input
	want Decision
}

func baseInput(t *testing.T) Input {
	t.Helper()
	return Input{
		Effective:       effective(t, instancePolicy(), gonkcfg.Policy{}, projectPolicy()),
		Catalog:         catalog(),
		Registered:      true,
		KeyReady:        true,
		Trigger:         atags.TriggerIssueTriage,
		Prior:           nil,
		Spend:           budget.Spend{},
		SpendAsOf:       at("2026-07-13T09:59:00Z"),
		Now:             at("2026-07-13T10:00:00Z"),
		Window:          spend.MonthWindow(at("2026-07-13T10:00:00Z")),
		MaxSpendStale:   5 * time.Minute,
		MaxInfraRetries: 5,
	}
}

func decideCases() []decideCase {
	return []decideCase{
		{
			name: "attempt 1 starts at the cheapest allowed rung",
			in:   baseInput,
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			name: "a gate failure escalates one rung",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				return in
			},
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 2},
		},
		{
			name: "an infra failure retries the SAME rung and does not escalate",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed}}
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 2},
		},
		{
			name: "infra failures never escalate, however many there are",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 3, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 4},
		},
		{
			name: "infra failures are capped, then deny",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.MaxInfraRetries = 3
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 3, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
				}
				return in
			},
			want: Decision{Kind: Deny, Attempt: 4, Reason: ReasonInfraRetriesExhausted},
		},
		{
			name: "a mixed history escalates only on the gate failures",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeGateFailed},
					{Attempt: 3, Rung: "glm", Outcome: OutcomeInfraFailed},
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 4},
		},
		{
			name: "ladder exhausted -> deny, never an infinite defer",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed},
					{Attempt: 2, Rung: "glm", Outcome: OutcomeGateFailed},
				}
				return in
			},
			want: Decision{Kind: Deny, Attempt: 3, Reason: ReasonLadderExhausted},
		},
		{
			name: "the PROJECT's tighter cost ceiling binds: $10 - $9.80 spent < $0.40 for glm",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{CostUSD: 9.80, Tokens: 3_000_000, TaskTokens: 200_000}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			name: "the INSTANCE's tighter token ceiling binds even though the project asked for more",
			in: func(t *testing.T) Input {
				inst := instancePolicy()
				inst.Budget.MonthlyTokens = tq(4_000_000) // tighter than the project's 50M
				proj := projectPolicy()
				in := baseInput(t)
				in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
				in.Spend = budget.Spend{Tokens: 3_900_000, TaskTokens: 100_000}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonMonthlyTokensExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			name: "a GROUP ceiling binds too",
			in: func(t *testing.T) Input {
				grp := gonkcfg.Policy{Budget: gonkcfg.BudgetPolicy{MonthlyCostUSD: f(1)}}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), grp, projectPolicy())
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{CostUSD: 0.80}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			// $10 ceiling, $9.00 billed -- $1.00 left, which fits glm's $0.40.
			// But two sessions are ALREADY committed to $0.40 each and their
			// spend rows have not landed. Net headroom is $0.20, so this one
			// must wait. Without the reservation term it runs, and all three
			// sessions overshoot together.
			name: "open reservations count against the cost ceiling (concurrency + spend-log lag)",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{CostUSD: 9.00, ReservedCostUSD: 0.80}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			// The SAME hole on the token side. Decision 9 gives tokens a hard
			// door (LiteLLM's USD ceiling, fed by synthetic prices), but that door
			// is a LOOSE backstop -- this reservation is the tight one. 50M ceiling,
			// 49.7M billed, 200K reserved -> 100K left, and the rung needs 200K.
			name: "open reservations count against the MONTHLY TOKEN ceiling",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Spend = budget.Spend{Tokens: 49_700_000, ReservedTokens: 200_000, TaskTokens: 100_000}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonMonthlyTokensExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			// ...and on the PER-TASK token ceiling. 2M ceiling, 1.7M billed for
			// this bead, 200K reserved by a sibling attempt -> 100K left, rung
			// needs 200K. A month rollover will not refill it, so: deny.
			name: "open reservations count against the PER-TASK token ceiling",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Spend = budget.Spend{TaskTokens: 1_700_000, ReservedTaskTokens: 200_000}
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonPerTaskTokensExhausted},
		},
		{
			name: "an action the project did not opt into is denied, however much budget it has",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{} // silent -> Resolve gives Triage: false
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			name: "a coarser layer's action veto is honored (ADR-002)",
			in: func(t *testing.T) Input {
				grp := gonkcfg.Policy{Actions: gonkcfg.ActionsPolicy{Triage: b(false)}}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), grp, projectPolicy())
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonActionNotAllowed},
		},
		{
			// The onboarding MR spends zero tokens and must work on a project
			// that has opted into nothing -- that is the whole point of it
			// (spec 5.3). It must not be caught by the action gate.
			name: "the onboarding trigger is gated by no action",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Actions = gonkcfg.ActionsPolicy{}
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Trigger = atags.TriggerOnboarding
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			name: "a .gonk.yml that would not load denies with invalid-config, not 'disabled'",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Invalid = true
				in.InvalidDetail = ".gonk.yml: at '/budget/monthly_tokens': got string, want integer"
				in.Effective = gonkcfg.Effective{} // exactly what the service holds in this state
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonInvalidConfig},
		},
		{
			// The budget window is zero until the first spend sync. A defer that
			// says retry_after: 0001-01-01 is an infinite park -- the caller
			// wakes at once, gets the same answer, and spins.
			name: "a defer against an unsynced (zero) budget window still has a usable retry_after",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Window = spend.Window{}
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				in.Spend = budget.Spend{CostUSD: 9.90}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-07-13T10:05:00Z")}, // Now + MaxSpendStale
		},
		{
			// ConsecutiveInfraFailures must count failures at THIS rung, not at
			// any rung: two infra failures on qwen-local followed by an
			// escalation must not count against glm's retry budget.
			name: "the infra-retry cap is per-rung, and escalating resets it",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.MaxInfraRetries = 3
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeInfraFailed},
					{Attempt: 3, Rung: "qwen-local", Outcome: OutcomeGateFailed},
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "glm", Model: "glm-5", Attempt: 4},
		},
		{
			// The trailing infra failures happened at a rung that is NO LONGER
			// the target -- which is not a hypothetical: the reresolve loop
			// re-runs Resolve when the operator config changes, so an operator
			// who tightens the instance ladder mid-bead moves the target under a
			// bead whose recent attempts ran somewhere else. Those failures must
			// not be charged against the new rung's retry budget.
			//
			// Real:      ConsecutiveInfraFailures(prior, "qwen-local") == 0 -> run.
			// Saboteur:  counts them anyway -> 2 >= MaxInfraRetries -> deny.
			// This row is the ONLY thing that catches the "counts infra retries
			// across ALL rungs" saboteur: in every other history, the trailing
			// infra run is already at the target rung, so the rung filter has
			// nothing to do and a saboteur that drops it is invisible.
			name: "infra failures at a rung that is no longer the target do not count",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.MaxInfraRetries = 2
				in.Prior = []Attempt{
					{Attempt: 1, Rung: "glm", Outcome: OutcomeInfraFailed},
					{Attempt: 2, Rung: "glm", Outcome: OutcomeInfraFailed},
				}
				return in
			},
			// Zero gate failures -> index 0 -> qwen-local, with a clean slate.
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 3},
		},
		{
			name: "a zero cost budget runs local rungs freely and can never reach a cloud rung",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Budget.MonthlyCostUSD = f(0) // the onboarding MR's conservative default
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Spend = budget.Spend{Tokens: 1_000_000, TaskTokens: 300_000}
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			// *** THE DECISION-9 REGRESSION GUARD. ***
			// The exact onboarding default (monthly_cost_usd: 0, local ladder), with
			// a MOUNTAIN of accumulated SYNTHETIC spend and synthetic reservations
			// from previous local sessions. It must STILL RUN. If synthetic dollars
			// ever reach the cost gate, this project cannot afford its own only
			// rung, every freshly-onboarded project bricks itself the moment it does
			// any work, and the entire onboarding flow is dead.
			//
			// This row is the whole reason budget.Spend keeps two separate cost
			// fields. Do not delete it, and do not "simplify" it.
			name: "synthetic spend NEVER blocks a local rung on a zero-cost budget",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Budget.MonthlyCostUSD = f(0)
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Spend = budget.Spend{
					SyntheticCostUSD:         987.65, // priced local inference, in accounting fiction
					ReservedSyntheticCostUSD: 43.21,
					Tokens:                   1_000_000,
					TaskTokens:               300_000,
				}
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			name: "...and the moment it must escalate to a priced rung, it defers",
			in: func(t *testing.T) Input {
				proj := projectPolicy()
				proj.Budget.MonthlyCostUSD = f(0)
				in := baseInput(t)
				in.Effective = effective(t, instancePolicy(), gonkcfg.Policy{}, proj)
				in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
				return in
			},
			want: Decision{Kind: Defer, Attempt: 2, Reason: ReasonMonthlyCostExhausted,
				RetryAfter: at("2026-08-01T00:00:00Z")},
		},
		{
			name: "per-task tokens exhausted -> DENY, not defer: a month rollover will not refill it",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Spend = budget.Spend{TaskTokens: 1_950_000} // ceiling 2M, est 200K
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonPerTaskTokensExhausted},
		},
		{
			name: "stale spend data -> defer, because we would be deciding on numbers we know are wrong",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.SpendAsOf = at("2026-07-13T09:50:00Z") // 10m old, max 5m
				return in
			},
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonSpendStale,
				RetryAfter: at("2026-07-13T10:05:00Z")},
		},
		{
			name: "stale spend data does not stop a project with no ceilings at all",
			in: func(t *testing.T) Input {
				inst := gonkcfg.Policy{Enabled: b(true), Ladder: []string{"qwen-local", "glm"}}
				proj := gonkcfg.Policy{Enabled: b(true), Actions: gonkcfg.ActionsPolicy{Triage: b(true)},
					Ladder: []string{"qwen-local"}}
				in := baseInput(t)
				in.Effective = effective(t, inst, gonkcfg.Policy{}, proj)
				in.SpendAsOf = at("2026-07-13T08:00:00Z")
				return in
			},
			want: Decision{Kind: Run, Rung: "qwen-local", Model: "qwen3-coder-30b", Attempt: 1},
		},
		{
			name: "a disabled project is DENIED and carries gonkcfg's own reason",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Effective = effective(t, gonkcfg.Policy{Enabled: b(false)}, gonkcfg.Policy{}, projectPolicy())
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonDisabled,
				Detail: "disabled by instance policy"},
		},
		{
			name: "an unregistered project is denied before anything else is even looked at",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Registered = false
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonNotRegistered},
		},
		{
			name: "no virtual key yet -> defer: provisioning is retried, the work should resume",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.KeyReady = false
				in.KeyRetryBackoff = 5 * time.Minute
				return in
			},
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonKeyMissing,
				RetryAfter: at("2026-07-13T10:05:00Z")},
		},
		{
			name: "quiet hours -> defer until the window ends, in the project's timezone",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				in.Now = at("2026-07-14T03:30:00Z") // = 23:30 the 13th in New York
				in.Window = spend.MonthWindow(in.Now)
				in.SpendAsOf = in.Now
				q, err := ParseQuietHours("22:00-07:00", "America/New_York")
				if err != nil {
					t.Fatal(err)
				}
				in.QuietHours = q
				return in
			},
			// 07:00 New York on the 14th = 11:00Z.
			want: Decision{Kind: Defer, Attempt: 1, Reason: ReasonQuietHours,
				RetryAfter: at("2026-07-14T11:00:00Z")},
		},
		{
			name: "a rung the catalog has never heard of is a config error, not a run",
			in: func(t *testing.T) Input {
				in := baseInput(t)
				delete(in.Catalog, "qwen-local")
				return in
			},
			want: Decision{Kind: Deny, Attempt: 1, Reason: ReasonInvalidConfig},
		},
	}
}

func TestDecide(t *testing.T) {
	for _, c := range decideCases() {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(c.in(t))
			if got.Kind != c.want.Kind || got.Rung != c.want.Rung || got.Attempt != c.want.Attempt ||
				got.Reason != c.want.Reason || !got.RetryAfter.Equal(c.want.RetryAfter) {
				t.Fatalf("\n got %+v\nwant %+v", got, c.want)
			}
			if c.want.Model != "" && got.Model != c.want.Model {
				t.Fatalf("model = %q, want %q", got.Model, c.want.Model)
			}
			if c.want.Detail != "" && got.Detail != c.want.Detail {
				t.Fatalf("detail = %q, want %q", got.Detail, c.want.Detail)
			}
			// Invariants that must hold for EVERY decision.
			if got.Kind == Run && got.Rung == "" {
				t.Fatal("a run decision with no rung")
			}
			if got.Kind == Defer && got.RetryAfter.IsZero() {
				t.Fatal("a defer with no retry_after is an infinite park")
			}
			if got.Kind != Run && got.Reason == "" {
				t.Fatal("a defer/deny with no machine reason")
			}
			if got.Kind == Run && got.Reason != "" {
				t.Fatalf("a run carrying a reason: %q", got.Reason)
			}
		})
	}
}

// Non-vacuity guard: the table must exercise every decision kind and a
// meaningful spread of reasons, or it is not constraining the policy.
func TestDecideTableCoversTheContract(t *testing.T) {
	kinds := map[Kind]int{}
	reasons := map[string]int{}
	for _, c := range decideCases() {
		kinds[c.want.Kind]++
		if c.want.Reason != "" {
			reasons[c.want.Reason]++
		}
	}
	for _, k := range []Kind{Run, Defer, Deny} {
		if kinds[k] == 0 {
			t.Errorf("no table row expects a %q decision", k)
		}
	}
	for _, r := range []string{
		ReasonNotRegistered, ReasonDisabled, ReasonInvalidConfig, ReasonActionNotAllowed,
		ReasonKeyMissing, ReasonQuietHours, ReasonSpendStale, ReasonLadderExhausted,
		ReasonInfraRetriesExhausted, ReasonPerTaskTokensExhausted, ReasonMonthlyTokensExhausted,
		ReasonMonthlyCostExhausted,
	} {
		if reasons[r] == 0 {
			t.Errorf("no table row expects reason %q; it is untested and may be unreachable", r)
		}
	}
}
```

- [ ] **Step 3: Run it, watch it fail.** Run: `go test ./pkg/rung/ -v` — FAIL, undefined `Decide`/`Input`/`Decision`.

- [ ] **Step 4: Implement `pkg/rung/decide.go`**

```go
// Package rung is gonk's wait-vs-spend brain: given a project's resolved
// config, its spend, and its attempt history, decide which ladder rung the next
// session attempt runs at -- or that it must wait, or that it must not run.
//
// Decide is a PURE FUNCTION. No network, no database, no time.Now() (the clock
// is an input). There are NO LLM judges anywhere in this package and there must
// never be: escalation is earned by failing an objective gate, not by a model's
// opinion (spec 6.3). Every decision is therefore reproducible from its inputs
// and exhaustively table-testable with zero infrastructure.
package rung

import (
	"fmt"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// Kind is what to do next.
type Kind string

const (
	// Run: spawn the attempt at Decision.Rung.
	Run Kind = "run"
	// Defer: not now. The bead parks in waiting-for-capacity and is retried at
	// RetryAfter (spec 6.2). Every Defer carries a RetryAfter -- a park with no
	// wake-up is a lost work item.
	Defer Kind = "defer"
	// Deny: this attempt must not run, and retrying will not change that. The
	// project is disabled, its config is invalid, or it has run out of ladder or
	// per-task budget. Spec 6.2 names only "rung or defer"; Deny exists because
	// parking a permanently-blocked bead in waiting-for-capacity is an infinite
	// loop, not a policy. See ADR-004.
	Deny Kind = "deny"
)

// Machine-readable decision reasons. A BOUNDED set on purpose: they are
// Prometheus label values AND part of the /v1/policy/decide wire contract.
//
// They are ALIASES of pkg/meterapi's constants, not copies. Two independent
// lists of the same bounded set is how one of them silently drifts, and the one
// that drifts is whichever the dashboards are not built on.
const (
	ReasonNotRegistered          = meterapi.ReasonNotRegistered
	ReasonDisabled               = meterapi.ReasonDisabled
	ReasonInvalidConfig          = meterapi.ReasonInvalidConfig
	ReasonActionNotAllowed       = meterapi.ReasonActionNotAllowed
	ReasonKeyMissing             = meterapi.ReasonKeyMissing
	ReasonQuietHours             = meterapi.ReasonQuietHours
	ReasonSpendStale             = meterapi.ReasonSpendStale
	ReasonLadderExhausted        = meterapi.ReasonLadderExhausted
	ReasonInfraRetriesExhausted  = meterapi.ReasonInfraRetriesExhausted
	ReasonPerTaskTokensExhausted = meterapi.ReasonPerTaskTokensExhausted
	ReasonMonthlyTokensExhausted = meterapi.ReasonMonthlyTokensExhausted
	ReasonMonthlyCostExhausted   = meterapi.ReasonMonthlyCostExhausted
)

// ActionFor maps an atags trigger onto the .gonk.yml action that gates it.
// Returning false means "no action gates this trigger".
//
// The spec does not state this mapping (see AD-5), and
// meter has to have one: /decide is the ONLY chokepoint before a session
// spawns, so if meter does not check Effective.Actions, `actions.triage: false`
// -- a documented veto surface in ADR-002 -- means nothing at all.
//
// `onboarding` is gated by NOTHING on purpose: the onboarding MR is
// deterministic and spends zero tokens (spec 5.3), and it must work on a
// project that has not opted into anything yet -- that is the entire point of it.
func ActionFor(eff gonkcfg.Effective, trigger string) (allowed bool, gated bool) {
	switch trigger {
	case atags.TriggerIssueTriage, atags.TriggerMentionReply, atags.TriggerScaffold:
		return eff.Actions.Triage, true
	case atags.TriggerOnboarding:
		return true, false
	}
	return false, true // an unknown trigger is not allowed to do anything
}

// Input is everything Decide is allowed to look at. If it is not in here, it
// cannot influence the decision -- which is the point.
type Input struct {
	// Effective MUST come from gonkcfg.Resolve. Per ADR-002 it is a plain
	// struct with no constructor, so a hand-built one silently violates every
	// invariant the resolver establishes -- which is exactly why Invalid is a
	// separate flag below rather than "a zero Effective".
	Effective  gonkcfg.Effective
	Catalog    map[string]opercfg.RungSpec
	Registered bool // meter has a registration for this project
	// Invalid: the project's .gonk.yml would not load, so Effective is not
	// meaningful. InvalidDetail carries the loader's error.
	Invalid       bool
	InvalidDetail string
	KeyReady      bool // the LiteLLM virtual key exists

	Trigger string // atags trigger; gates against Effective.Actions

	Prior []Attempt    // this bead's attempt history, oldest first
	Spend budget.Spend // observed + reserved, for THIS project and THIS bead

	SpendAsOf time.Time    // when the spend snapshot was last synced
	Now       time.Time    // the clock, as an input
	Window    spend.Window // the budget window in force

	QuietHours      *QuietHours
	MaxSpendStale   time.Duration
	KeyRetryBackoff time.Duration
	MaxInfraRetries int
}

// Decision is the answer. Invariants (asserted in the tests): a Run always
// names a Rung and carries no Reason; a Defer always carries a RetryAfter and a
// Reason; a Deny always carries a Reason.
type Decision struct {
	Kind       Kind      `json:"decision"`
	Rung       string    `json:"rung"`
	Model      string    `json:"model,omitempty"`
	Attempt    int       `json:"attempt"`
	Reason     string    `json:"reason"`
	Detail     string    `json:"detail"`
	RetryAfter time.Time `json:"retry_after,omitzero"`
}

// Decide is the whole policy. Read it top to bottom: the gates are ordered from
// "nothing can help" to "later might".
func Decide(in Input) Decision {
	attempt := len(in.Prior) + 1
	deny := func(reason, detail string) Decision {
		return Decision{Kind: Deny, Attempt: attempt, Reason: reason, Detail: detail}
	}
	deferTo := func(reason, detail string, until time.Time) Decision {
		// A defer with a RetryAfter in the past (or the zero time -- e.g. a
		// budget Window that has not been synced yet) is an infinite park: the
		// caller wakes immediately, gets the same defer, and spins. Clamp it.
		if !until.After(in.Now) {
			until = in.Now.Add(in.MaxSpendStale)
		}
		return Decision{Kind: Defer, Attempt: attempt, Reason: reason, Detail: detail, RetryAfter: until}
	}

	// 1. Is this project allowed to run at all? Retrying cannot fix any of these.
	if !in.Registered {
		return deny(ReasonNotRegistered, "no registration in gonk-meter; onboard the project first")
	}
	if in.Invalid {
		// The .gonk.yml would not load, so Effective is a zero value and means
		// nothing. Say the true thing rather than reporting a bogus "disabled".
		return deny(ReasonInvalidConfig, in.InvalidDetail)
	}
	if !in.Effective.Enabled {
		// gonkcfg already worked out WHY, and its invariant is that
		// DisabledReason is non-empty exactly when Enabled is false (ADR-002).
		return deny(ReasonDisabled, in.Effective.DisabledReason)
	}
	// The action veto. Without this, `actions.triage: false` -- at any layer --
	// is decoration: /decide is the only thing standing between a trigger and a
	// metered session.
	if allowed, gated := ActionFor(in.Effective, in.Trigger); gated && !allowed {
		return deny(ReasonActionNotAllowed,
			fmt.Sprintf("trigger %q is not enabled for this project", in.Trigger))
	}

	// 2. Is the door even there? No key, no metered call, no session. The
	//    reconcile loop retries provisioning, so this is a wait, not a refusal.
	if !in.KeyReady {
		return deferTo(ReasonKeyMissing, "LiteLLM virtual key not provisioned yet",
			in.Now.Add(in.KeyRetryBackoff))
	}

	// 3. Quiet hours. A wait-vs-spend decision, so it lands here (spec 5.4).
	if in.QuietHours != nil {
		if end, quiet := in.QuietHours.EndAfter(in.Now); quiet {
			return deferTo(ReasonQuietHours,
				fmt.Sprintf("quiet hours until %s", end.Format(time.RFC3339)), end)
		}
	}

	// 4. Where are we on the ladder? The rung index is the count of GATE
	//    failures. Infra failures retry the same rung -- a flaky pod is not
	//    evidence that a bigger model is needed (spec 6.3).
	idx := Escalations(in.Prior)
	if idx >= len(in.Effective.Ladder) {
		return deny(ReasonLadderExhausted,
			fmt.Sprintf("%d rungs, %d gate failures", len(in.Effective.Ladder), idx))
	}
	target := in.Effective.Ladder[idx]
	spec, ok := in.Catalog[target]
	if !ok {
		return deny(ReasonInvalidConfig,
			fmt.Sprintf("rung %q is not in the operator rung catalog", target))
	}
	if n := ConsecutiveInfraFailures(in.Prior, target); n >= in.MaxInfraRetries {
		return deny(ReasonInfraRetriesExhausted,
			fmt.Sprintf("rung %q failed on infrastructure %d times in a row", target, n))
	}

	// 5. Money. Every check below is against ceiling - (observed + RESERVED):
	//    the reservation is what makes this survive spend-log lag and two
	//    sessions racing the same headroom.
	bud := budget.FromEffective(in.Effective.Budget)
	rem := budget.Remain(bud, in.Spend)

	// Stale spend is not a number we may decide on. A project with no ceilings
	// at all has nothing to be stale about, so it runs.
	if !bud.AllUnlimited() && in.Now.Sub(in.SpendAsOf) > in.MaxSpendStale {
		return deferTo(ReasonSpendStale,
			fmt.Sprintf("spend snapshot is %s old (max %s)",
				in.Now.Sub(in.SpendAsOf).Round(time.Second), in.MaxSpendStale),
			in.SpendAsOf.Add(in.MaxSpendStale))
	}

	// Per-task tokens are a property of the WORK ITEM. A month rollover will not
	// refill them, so this is a deny, not a defer.
	if !rem.FitsTaskTokens(int64(spec.EstTokens)) {
		return deny(ReasonPerTaskTokensExhausted,
			fmt.Sprintf("bead needs ~%d tokens, %s remain of its per-task budget",
				spec.EstTokens, tokenStr(rem.PerTaskTokens)))
	}
	// Monthly ceilings DO refill. Park until the window rolls (spec 6.2).
	if !rem.FitsMonthTokens(int64(spec.EstTokens)) {
		return deferTo(ReasonMonthlyTokensExhausted,
			fmt.Sprintf("rung %q needs ~%d tokens, %s remain this window",
				target, spec.EstTokens, tokenStr(rem.MonthlyTokens)), in.Window.End)
	}
	// The cost gate applies ONLY to rungs with a positive REAL price. opercfg
	// guarantees cloud rungs have one (est_cost_usd > 0) and local rungs do not
	// (est_cost_usd == 0), so "monthly_cost_usd: 0 means local rungs only,
	// forever" falls straight out of the arithmetic -- which is exactly the
	// conservative default the onboarding MR ships (spec 5.3).
	//
	// *** THIS IS WHERE DECISION 9 COULD HAVE BROKEN EVERYTHING, AND MUST NOT. ***
	// Local rungs are now priced SYNTHETICALLY in LiteLLM, so that its USD
	// virtual-key ceiling is a hard door for tokens too. Those synthetic dollars
	// live in budget.Spend.SyntheticCostUSD and are NOT visible to rem.FitsCost,
	// which reads real dollars only. If they ever leaked in here, an onboarded
	// project (monthly_cost_usd: 0, ladder: [qwen-local]) could not afford its
	// own only rung and would be DEAD ON ARRIVAL. Do not "unify" the two.
	//
	// Note we do NOT fall back to a cheaper rung here. The cheaper rungs already
	// failed their gate; re-running them would loop. Spec 6.3: "defer applies
	// when only-cloud-rungs-remain and budget is exhausted."
	if spec.EstCostUSD > 0 && !rem.FitsCost(spec.EstCostUSD) {
		return deferTo(ReasonMonthlyCostExhausted,
			fmt.Sprintf("rung %q needs $%.2f, $%s remain this window",
				target, spec.EstCostUSD, costStr(rem.MonthlyCostUSD)), in.Window.End)
	}

	return Decision{Kind: Run, Rung: target, Model: spec.Model, Attempt: attempt}
}

// Reserve is what a Run decision commits: the estimated consumption held against
// the ceilings until the spend log catches up or the reservation expires.
//
// It returns BOTH currencies. costUSD is real money and is what the cost gate
// will see next time (zero for a local rung). syntheticUSD is what LiteLLM's
// virtual-key counter will actually be charged for a local rung (zero for a
// cloud rung, whose real price already is that charge). They are held separately
// and they never mix.
func Reserve(spec opercfg.RungSpec) (costUSD, syntheticUSD float64, tokens int64) {
	tokens = int64(spec.EstTokens)
	if spec.Kind == opercfg.KindLocal {
		return 0, spec.PricePerToken() * float64(tokens), tokens
	}
	return spec.EstCostUSD, 0, tokens
}

func tokenStr(t budget.TokenLimit) string {
	if t.Unlimited() {
		return "unlimited"
	}
	return fmt.Sprintf("%d", int64(t))
}

func costStr(c budget.CostLimit) string {
	if c.Unlimited() {
		return "unlimited"
	}
	return fmt.Sprintf("%.2f", float64(c))
}
```

- [ ] **Step 5: Implement `QuietHours` (append to `pkg/rung/outcome.go`)**

```go
// QuietHours is a resolved schedule.quiet_hours window (spec 5.4). It is
// resolved ONCE, at project registration, so that a bad timezone is a 400 at
// onboarding rather than a surprise at 3am -- and so that Decide stays pure.
type QuietHours struct {
	Start time.Duration // since local midnight
	End   time.Duration
	Loc   *time.Location
}

// ParseQuietHours parses "22:00-07:00" in the named IANA timezone. A window
// whose end is before its start wraps midnight.
//
// An EMPTY timezone is an error, not a default. The .gonk.yml schema does not
// require `timezone` when `quiet_hours` is set, and time.LoadLocation("")
// returns UTC with a nil error -- so a project writing quiet hours and no
// timezone would silently get a UTC window, quiet at the wrong nine hours of the
// day, with nothing to tell them. Make it a 400 at registration instead.
func ParseQuietHours(window, timezone string) (*QuietHours, error) {
	if window == "" {
		return nil, nil
	}
	if timezone == "" {
		return nil, fmt.Errorf("quiet hours %q: schedule.timezone is required when quiet_hours is set (an empty timezone would silently mean UTC)", window)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("quiet hours: timezone %q: %w", timezone, err)
	}
	var sh, sm, eh, em int
	if _, err := fmt.Sscanf(window, "%2d:%2d-%2d:%2d", &sh, &sm, &eh, &em); err != nil {
		return nil, fmt.Errorf("quiet hours %q: want HH:MM-HH:MM: %w", window, err)
	}
	q := &QuietHours{
		Start: time.Duration(sh)*time.Hour + time.Duration(sm)*time.Minute,
		End:   time.Duration(eh)*time.Hour + time.Duration(em)*time.Minute,
		Loc:   loc,
	}
	if q.Start == q.End {
		return nil, fmt.Errorf("quiet hours %q: a zero-length window", window)
	}
	return q, nil
}

// EndAfter reports whether now is inside the quiet window and, if so, the next
// instant it ends. It walks forward from local midnight rather than doing
// arithmetic on wall-clock durations, so a DST transition inside the window
// cannot produce an end time that is in the past.
func (q *QuietHours) EndAfter(now time.Time) (time.Time, bool) {
	local := now.In(q.Loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, q.Loc)
	since := local.Sub(midnight)

	inWindow := false
	if q.Start < q.End {
		inWindow = since >= q.Start && since < q.End
	} else { // wraps midnight, e.g. 22:00-07:00
		inWindow = since >= q.Start || since < q.End
	}
	if !inWindow {
		return time.Time{}, false
	}
	end := midnight.Add(q.End)
	if !end.After(now) { // we are in the pre-midnight half of a wrapping window
		end = midnight.AddDate(0, 0, 1).Add(q.End)
	}
	return end.UTC(), true
}
```

Add `"time"` to `outcome.go`'s imports (and `"fmt"`).

- [ ] **Step 6: Watch the table pass.** Run: `go test ./pkg/rung/ -race -v` — PASS (all rows + both invariant tests).

- [ ] **Step 7: Write the mutation test — prove the table cannot pass vacuously**

`pkg/rung/mutation_test.go`. Same technique as Task 1, aimed at the money: each saboteur is a *plausible bug* someone will actually write. If the table lets one through, the table is the problem.

```go
package rung

import (
	"testing"
	"time"

	_ "time/tzdata" // the quiet-hours cases call time.LoadLocation

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
)

type saboteur struct {
	name string
	bug  func(Input) Decision
}

func saboteurs() []saboteur {
	return []saboteur{
		{"ignores the budget entirely", func(in Input) Decision {
			in.Spend = budget.Spend{}
			return Decide(in)
		}},
		// The three reservation fields get THREE saboteurs, not one. A single
		// saboteur that zeroes all three is caught by the cost row alone, which
		// masks a missing token row -- and the token side has only a LOOSE
		// backstop behind it (Decision 9's synthetic USD ceiling), so this tight
		// gate is what actually holds.
		{"ignores reserved COST (the concurrency hole, dollars)", func(in Input) Decision {
			in.Spend.ReservedCostUSD = 0
			return Decide(in)
		}},
		{"ignores reserved MONTHLY TOKENS", func(in Input) Decision {
			in.Spend.ReservedTokens = 0
			return Decide(in)
		}},
		{"ignores reserved PER-TASK TOKENS", func(in Input) Decision {
			in.Spend.ReservedTaskTokens = 0
			return Decide(in)
		}},
		{"counts SYNTHETIC dollars against the real cost gate", func(in Input) Decision {
			// Decision 9's failure mode, and the one someone will genuinely write
			// while "cleaning up the two cost fields". It bricks the onboarding
			// default: a monthly_cost_usd: 0 project can no longer afford its own
			// local rung. Caught by "synthetic spend NEVER blocks a local rung".
			in.Spend.CostUSD += in.Spend.SyntheticCostUSD
			in.Spend.ReservedCostUSD += in.Spend.ReservedSyntheticCostUSD
			return Decide(in)
		}},
		{"ignores the action veto", func(in Input) Decision {
			in.Trigger = atags.TriggerOnboarding // the one trigger no action gates
			return Decide(in)
		}},
		{"reports an unloadable config as merely 'disabled'", func(in Input) Decision {
			in.Invalid = false
			return Decide(in)
		}},
		{"counts infra retries across ALL rungs instead of per-rung", func(in Input) Decision {
			// The bug: ConsecutiveInfraFailures ignores which rung failed, so
			// infra flakiness on a cheap rung eats the expensive rung's retries.
			idx := Escalations(in.Prior)
			if idx >= len(in.Effective.Ladder) {
				return Decide(in)
			}
			target := in.Effective.Ladder[idx]
			p := append([]Attempt(nil), in.Prior...)
			for i := range p {
				if p[i].Outcome == OutcomeInfraFailed {
					p[i].Rung = target
				}
			}
			in.Prior = p
			return Decide(in)
		}},
		{"escalates on infra failures (spec 6.3's central promise)", func(in Input) Decision {
			p := append([]Attempt(nil), in.Prior...)
			for i := range p {
				if p[i].Outcome == OutcomeInfraFailed {
					p[i].Outcome = OutcomeGateFailed
				}
			}
			in.Prior = p
			return Decide(in)
		}},
		{"never escalates", func(in Input) Decision {
			in.Prior = nil
			return Decide(in)
		}},
		{"off-by-one on the ladder index", func(in Input) Decision {
			d := Decide(in)
			if d.Kind != Run {
				return d
			}
			for i, r := range in.Effective.Ladder {
				if r == d.Rung && i+1 < len(in.Effective.Ladder) {
					next := in.Effective.Ladder[i+1]
					d.Rung, d.Model = next, in.Catalog[next].Model
					return d
				}
			}
			return d
		}},
		{"trusts stale spend data", func(in Input) Decision {
			in.SpendAsOf = in.Now
			return Decide(in)
		}},
		{"ignores quiet hours", func(in Input) Decision {
			in.QuietHours = nil
			return Decide(in)
		}},
		{"runs without a virtual key", func(in Input) Decision {
			in.KeyReady = true
			return Decide(in)
		}},
		{"runs an unregistered project", func(in Input) Decision {
			in.Registered = true
			return Decide(in)
		}},
		{"defers instead of denying (parks a doomed bead forever)", func(in Input) Decision {
			d := Decide(in)
			if d.Kind == Deny {
				d.Kind = Defer
				d.RetryAfter = in.Now.Add(time.Hour)
			}
			return d
		}},
		{"denies instead of deferring (throws away recoverable work)", func(in Input) Decision {
			d := Decide(in)
			if d.Kind == Defer {
				d.Kind, d.RetryAfter = Deny, time.Time{}
			}
			return d
		}},
		{"has no infra-retry cap", func(in Input) Decision {
			in.MaxInfraRetries = 1 << 30
			return Decide(in)
		}},
		{"treats the per-task ceiling as monthly (defers instead of denying)", func(in Input) Decision {
			in.Spend.TaskTokens = 0
			return Decide(in)
		}},
	}
}

func TestSaboteursAreCaught(t *testing.T) {
	cases := decideCases()
	for _, sab := range saboteurs() {
		caught := false
		for _, c := range cases {
			in := c.in(t)
			got := sab.bug(in)
			if got.Kind != c.want.Kind || got.Rung != c.want.Rung || got.Attempt != c.want.Attempt ||
				got.Reason != c.want.Reason || !got.RetryAfter.Equal(c.want.RetryAfter) {
				caught = true
				break
			}
		}
		if !caught {
			t.Errorf("saboteur %q produced the expected decision on EVERY table row.\n"+
				"The table does not constrain this behavior, so nothing would stop us shipping the bug.\n"+
				"Add a row that catches it -- do not weaken the saboteur.", sab.name)
		}
	}
}
```

- [ ] **Step 8: Run the mutation test**

Run: `go test ./pkg/rung/ -run Saboteurs -v`
Expected: PASS. Every saboteur must be caught. If one survives, **add a table row**.

- [ ] **Step 9: Full gate and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add pkg/rung && git commit -m "feat(rung): deterministic outcome-gated ladder policy (run/defer/deny), no LLM judges"
```

---

### Task 5: `internal/meter/tagmint` — the attribution-tag boundary

**Closes Plan 01 carry-forward 3.** `atags.Validate` checks that fields are non-empty and that the trigger is known — it accepts *any* string for `Project` and `Rung`. That is correct for the contract package (it is a data contract, not a firewall), but these values travel into LiteLLM request metadata, into spend-log rows, into Loki labels, into Prometheus label values, into the cost API's JSON, and into CSV exports. A newline or a comma in one of them is a log-injection or column-shift bug.

**Meter is the boundary**, because meter mints the tags: `/v1/policy/decide` returns `metadata` and the pack stamps it verbatim. Nothing else in the system constructs tags. So the charset gate lives here, not in `pkg/atags`.

**Files:** Create `internal/meter/tagmint/tagmint.go`, `internal/meter/tagmint/tagmint_test.go`

- [ ] **Step 1: Write the failing test**

`internal/meter/tagmint/tagmint_test.go`:

```go
package tagmint

import (
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

func req() Request {
	return Request{
		Project: "group/repo", Rig: "group-repo", BeadID: "gk-1a2b",
		SessionKey: "sess-9", Rung: "qwen-local", Attempt: 2,
		Trigger: atags.TriggerIssueTriage,
	}
}

func TestMintValid(t *testing.T) {
	tags, err := Mint(req())
	if err != nil {
		t.Fatalf("Mint = %v", err)
	}
	md := tags.Metadata()
	if md[atags.KeyProject] != "group/repo" || md[atags.KeyAttempt] != "2" {
		t.Fatalf("metadata = %v", md)
	}
	if err := tags.Validate(); err != nil {
		t.Fatalf("minted tags fail the atags contract: %v", err)
	}
}

// Everything here is accepted by atags.Validate today. None of it may reach a
// log line, a metadata header, or a CSV column.
func TestMintRejectsHostileValues(t *testing.T) {
	bad := map[string]func(*Request){
		"newline in project":        func(r *Request) { r.Project = "group/repo\ninjected: true" },
		"CR in project":             func(r *Request) { r.Project = "group/repo\rX" },
		"tab in rig":                func(r *Request) { r.Rig = "repo\tX" },
		"comma in rung":             func(r *Request) { r.Rung = "glm,sonnet" },
		"NUL in bead id":            func(r *Request) { r.BeadID = "gk-1\x00" },
		"DEL in session key":        func(r *Request) { r.SessionKey = "sess\x7f9" },
		"unicode line separator":    func(r *Request) { r.Project = "group/repo x" },
		"RTL override":              func(r *Request) { r.Project = "group/‮repo" },
		"quote in project":          func(r *Request) { r.Project = `group/"repo"` },
		"backslash in project":      func(r *Request) { r.Project = `group\repo` },
		"leading space":             func(r *Request) { r.Project = " group/repo" },
		"overlong value":            func(r *Request) { r.Project = strings.Repeat("a", 300) },
		"uppercase rung":            func(r *Request) { r.Rung = "GLM" },
		"empty project":             func(r *Request) { r.Project = "" },
		"attempt zero":              func(r *Request) { r.Attempt = 0 },
		"unknown trigger":           func(r *Request) { r.Trigger = "vibes" },
	}
	for name, mut := range bad {
		r := req()
		mut(&r)
		if _, err := Mint(r); err == nil {
			t.Errorf("%s: Mint accepted %+v", name, r)
		}
	}
}

// The legal shapes must keep working: GitLab paths nest, bead IDs and session
// keys carry dots and colons.
func TestMintAcceptsRealisticValues(t *testing.T) {
	ok := []func(*Request){
		func(r *Request) { r.Project = "agentic/experiments/deep-nest.v2" },
		func(r *Request) { r.BeadID = "gk-1a2b.3" },
		func(r *Request) { r.SessionKey = "triage:issue-42:sess-9" },
		func(r *Request) { r.Rung = "qwen-local-30b" },
		func(r *Request) { r.Attempt = 99 },
		func(r *Request) { r.Trigger = atags.TriggerScaffold },
	}
	for i, mut := range ok {
		r := req()
		mut(&r)
		if _, err := Mint(r); err != nil {
			t.Errorf("case %d: Mint rejected a legitimate value %+v: %v", i, r, err)
		}
	}
}
```

- [ ] **Step 2: Run it, watch it fail.** Run: `go test ./internal/meter/tagmint/ -v`

- [ ] **Step 3: Implement `internal/meter/tagmint/tagmint.go`**

```go
// Package tagmint is the boundary where gonk's attribution tags are created.
//
// pkg/atags is a data CONTRACT: it checks that fields are present and that the
// trigger is known, and it deliberately accepts any string for project and
// rung. That is safe inside the package -- and unsafe the moment the values
// leave it. Tag values end up in LiteLLM request metadata, spend-log rows, Loki
// labels, Prometheus label values, JSON responses, and CSV exports. A newline
// in a project name is log injection; a comma is a shifted CSV column.
//
// gonk-meter is the ONLY thing that mints tags (POST /v1/policy/decide returns
// the metadata map and the pack stamps it verbatim), so this is the one place
// the charset gate has to exist. Plan 01 carry-forward: "enforce at the
// boundary, not in the contract package."
package tagmint

import (
	"fmt"
	"regexp"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

// maxLen bounds every tag value. Unbounded label values are a memory and
// cardinality problem in every downstream system that stores them.
const maxLen = 200

var (
	// GitLab path_with_namespace: segments of alnum/._- joined by /.
	reProject = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
	// Rung names match the .gonk.yml schema's ladder item pattern exactly.
	reRung = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// Bead IDs, session keys, rig names: printable, no separators that any
	// downstream serializer treats as structure.
	reID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// Request is a tag-minting request. Attempt and Rung come from a rung.Decision;
// the rest come from the caller.
type Request struct {
	Project    string
	Rig        string
	BeadID     string
	SessionKey string
	Rung       string
	Attempt    int
	Trigger    string
}

// Mint validates every field against a strict charset and returns tags that are
// safe to serialize anywhere. It ALSO runs atags.Validate, so the contract's own
// rules (non-empty, known trigger, attempt >= 1) still apply -- this is an
// additional gate, never a replacement.
func Mint(r Request) (atags.Tags, error) {
	for _, f := range []struct {
		name, value string
		pattern     *regexp.Regexp
	}{
		{"project", r.Project, reProject},
		{"rig", r.Rig, reID},
		{"bead_id", r.BeadID, reID},
		{"session_key", r.SessionKey, reID},
		{"rung", r.Rung, reRung},
	} {
		if err := check(f.name, f.value, f.pattern); err != nil {
			return atags.Tags{}, err
		}
	}
	t := atags.Tags{
		Project: r.Project, Rig: r.Rig, BeadID: r.BeadID,
		SessionKey: r.SessionKey, Rung: r.Rung, Attempt: r.Attempt,
		Trigger: r.Trigger,
	}
	if err := t.Validate(); err != nil {
		return atags.Tags{}, err
	}
	return t, nil
}

func check(name, value string, pattern *regexp.Regexp) error {
	if value == "" {
		return fmt.Errorf("tagmint: %s is empty", name)
	}
	if len(value) > maxLen {
		return fmt.Errorf("tagmint: %s is %d bytes, max %d", name, len(value), maxLen)
	}
	// Reject control characters and non-ASCII outright before the pattern, so
	// the error names the real problem (an invisible byte) rather than "does not
	// match ^[A-Za-z0-9]...". U+2028/U+2029 and the bidi overrides are exactly
	// the characters that survive a naive regexp and then break a log parser.
	for i, r := range value {
		if r < 0x20 || r == 0x7f || r > 0x7e {
			return fmt.Errorf("tagmint: %s contains a control or non-ASCII character at byte %d (%U); "+
				"tag values are serialized into log lines, metadata headers, and CSV", name, i, r)
		}
	}
	if !pattern.MatchString(value) {
		return fmt.Errorf("tagmint: %s %q does not match %s", name, value, pattern)
	}
	return nil
}
```

- [ ] **Step 4: Watch it pass, gate, commit**

```bash
go test ./internal/meter/tagmint/ -race -v
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add internal/meter/tagmint
git commit -m "feat(meter): charset-validated attribution tag minting at the service boundary"
```

---

### Task 6: `internal/meter/store` — registrations, attempts, reservations, spend rows

The store is an **interface with an in-memory implementation**. The durable backend is **Dolt** (Decision 10) and it is **gated on Task 0b** -- do not write `dolt.go` until the racing test passes. Nothing above the interface depends on which backend wins; the conformance suite in `storetest/` runs against BOTH implementations.

Reservations are the mechanism that makes the budget check survive spend-log lag and concurrency (ADR-004). Three rules:
- **`ReserveIfFits` is a single atomic store operation** — check-and-write, not check-then-write. Task 0b explains why this cannot live in the service. The service's per-project mutex remains, but it is contention control, not the safety property.
- An **expired** reservation is recorded as an `infra-failed` attempt — a session that died without reporting must retry its rung, not escalate it, and must not hold budget forever.
- A reservation holds **both currencies** (real and synthetic dollars) and **tokens**. They never mix.

**Two implementations, one conformance suite.** `Memory` is for tests. `Dolt` is what ships (Decision 10) — and **you may not write it until Task 0b passes.** Both must satisfy the *same* suite:

```go
// internal/meter/store/storetest/suite.go
//
// Run(t, func() store.Store) is the single conformance suite. Every test below
// runs against BOTH Memory and Dolt, so a behaviour the memory store has by
// accident (and the real one does not) cannot hide behind green tests.
//
//	func TestMemory(t *testing.T) { storetest.Run(t, func() store.Store { return store.NewMemory() }) }
//	//go:build dolt
//	func TestDolt(t *testing.T)   { storetest.Run(t, func() store.Store { return newDoltStore(t) }) }
func Run(t *testing.T, newStore func() store.Store)
```

**Files:** Create `internal/meter/store/store.go`, `internal/meter/store/memory.go`, `internal/meter/store/dolt.go` (**gated on Task 0b**), `internal/meter/store/storetest/suite.go`, `internal/meter/store/memory_test.go`, `internal/meter/store/dolt_test.go`

- [ ] **Step 1: Write the failing test**

`internal/meter/store/memory_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestRegistrationRoundTrip(t *testing.T) {
	ctx, s := context.Background(), NewMemory()
	if _, ok, _ := s.GetRegistration(ctx, "group/repo"); ok {
		t.Fatal("empty store returned a registration")
	}
	reg := Registration{Project: "group/repo", Rig: "group-repo", State: StateActive}
	if err := s.PutRegistration(ctx, reg); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetRegistration(ctx, "group/repo")
	if err != nil || !ok || got.State != StateActive || got.Rig != "group-repo" {
		t.Fatalf("GetRegistration = %+v %v %v", got, ok, err)
	}
	if err := s.DeleteRegistration(ctx, "group/repo"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetRegistration(ctx, "group/repo"); ok {
		t.Fatal("registration survived deletion")
	}
}

func TestAttemptsAreScopedToTheBeadAndKeyedByReservation(t *testing.T) {
	ctx, s := context.Background(), NewMemory()
	must(t, s.RecordAttempt(ctx, "group/repo", "gk-1", "rsv-1",
		rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeGateFailed}))
	must(t, s.RecordAttempt(ctx, "group/repo", "gk-1", "rsv-2",
		rung.Attempt{Attempt: 2, Rung: "glm", Outcome: rung.OutcomeSuccess}))
	must(t, s.RecordAttempt(ctx, "group/repo", "gk-2", "rsv-3",
		rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeInfraFailed}))

	got, err := s.Attempts(ctx, "group/repo", "gk-1")
	if err != nil || len(got) != 2 || got[0].Rung != "qwen-local" || got[1].Outcome != rung.OutcomeSuccess {
		t.Fatalf("Attempts(gk-1) = %+v %v", got, err)
	}
	if other, _ := s.Attempts(ctx, "group/repo", "gk-2"); len(other) != 1 {
		t.Fatalf("bead histories bled into each other: %+v", other)
	}
	// The returned slice must not alias the store's: a caller mutating it must
	// not rewrite the ladder history.
	got[0].Outcome = rung.OutcomeSuccess
	again, _ := s.Attempts(ctx, "group/repo", "gk-1")
	if again[0].Outcome != rung.OutcomeGateFailed {
		t.Fatal("Attempts aliased the store's slice; a caller just rewrote history")
	}
}

// Outcome reports are retried. Recording the same reservation's outcome twice
// must not add a second attempt -- that would be a free escalation.
func TestRecordAttemptIsIdempotentPerReservation(t *testing.T) {
	ctx, s := context.Background(), NewMemory()
	a := rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeGateFailed}
	must(t, s.RecordAttempt(ctx, "p", "gk-1", "rsv-1", a))
	must(t, s.RecordAttempt(ctx, "p", "gk-1", "rsv-1", a))
	if got, _ := s.Attempts(ctx, "p", "gk-1"); len(got) != 1 {
		t.Fatalf("a retried outcome report recorded %d attempts, want 1 (that is a free escalation)", len(got))
	}
}

// A session that outlives reservation_ttl gets an infra-failed attempt from the
// janitor, and may THEN report its real outcome. The real one must win, or the
// ladder silently stops working for any session longer than the TTL.
func TestATerminalOutcomeSupersedesTheJanitorsGuess(t *testing.T) {
	ctx, s := context.Background(), NewMemory()
	must(t, s.RecordAttempt(ctx, "p", "gk-1", "rsv-1",
		rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeInfraFailed})) // janitor
	must(t, s.RecordAttempt(ctx, "p", "gk-1", "rsv-1",
		rung.Attempt{Attempt: 1, Rung: "qwen-local", Outcome: rung.OutcomeGateFailed})) // the truth, late

	got, _ := s.Attempts(ctx, "p", "gk-1")
	if len(got) != 1 || got[0].Outcome != rung.OutcomeGateFailed {
		t.Fatalf("attempts = %+v; the real gate failure must supersede the janitor's infra-failed", got)
	}
	if rung.Escalations(got) != 1 {
		t.Fatal("the bead earned an escalation and did not get one")
	}
}

func TestReservationsHoldBudgetAndExpire(t *testing.T) {
	ctx, s := context.Background(), NewMemory()
	now := at("2026-07-13T10:00:00Z")
	r := Reservation{
		ID: "rsv-1", Project: "group/repo", BeadID: "gk-1", SessionKey: "s1",
		Rung: "glm", Attempt: 2, CostUSD: 0.40, Tokens: 200_000,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	fits, err := s.ReserveIfFits(ctx, "group/repo", unlimited(), budget.Spend{}, r)
	if err != nil || !fits {
		t.Fatalf("ReserveIfFits = %v, %v; an unlimited ceiling must always fit", fits, err)
	}

	open, err := s.OpenReservations(ctx, "group/repo", now.Add(30*time.Minute))
	if err != nil || len(open) != 1 || open[0].CostUSD != 0.40 {
		t.Fatalf("OpenReservations = %+v %v", open, err)
	}
	// Another project's ceiling is untouched by this one.
	if other, _ := s.OpenReservations(ctx, "other/repo", now); len(other) != 0 {
		t.Fatal("a reservation leaked across projects")
	}
	// Past its TTL it no longer holds budget...
	if late, _ := s.OpenReservations(ctx, "group/repo", now.Add(2*time.Hour)); len(late) != 0 {
		t.Fatal("an expired reservation is still holding budget")
	}
	// ...and Expire hands it back exactly once, so the janitor can record the
	// infra failure without recording it twice.
	expired, err := s.ExpireReservations(ctx, now.Add(2*time.Hour))
	if err != nil || len(expired) != 1 || expired[0].ID != "rsv-1" {
		t.Fatalf("ExpireReservations = %+v %v", expired, err)
	}
	if again, _ := s.ExpireReservations(ctx, now.Add(3*time.Hour)); len(again) != 0 {
		t.Fatal("ExpireReservations returned the same reservation twice")
	}
}

// Settling does NOT free the budget immediately. The session's spend rows land
// seconds-to-minutes after it ends; freeing the hold the moment the outcome
// arrives would over-report headroom by the session's entire actual cost, on
// EVERY successful session, at exactly the moment that cost is unbilled.
func TestSettleHoldsBudgetUntilTheSpendCatchesUp(t *testing.T) {
	ctx, s := context.Background(), NewMemory()
	now := at("2026-07-13T10:00:00Z")
	mustFit(t, s.ReserveIfFits(ctx, "p", unlimited(), budget.Spend{}, Reservation{
		ID: "rsv-1", Project: "p", CostUSD: 1,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour)}))

	holdUntil := now.Add(5 * time.Minute) // now + max_spend_staleness
	must(t, s.Settle(ctx, "rsv-1", holdUntil))

	open, _ := s.OpenReservations(ctx, "p", now.Add(time.Minute))
	if len(open) != 1 || open[0].CostUSD != 1 {
		t.Fatal("a settled reservation stopped holding budget immediately; the spend rows have not landed yet")
	}
	if late, _ := s.OpenReservations(ctx, "p", now.Add(6*time.Minute)); len(late) != 0 {
		t.Fatal("the hold outlived holdUntil")
	}
	// The janitor must not touch it: its session is not dead, it reported.
	if expired, _ := s.ExpireReservations(ctx, now.Add(6*time.Minute)); len(expired) != 0 {
		t.Fatal("the janitor tried to record an infra failure for a session that reported a real outcome")
	}
	// Settling twice (a retried outcome POST) must not error.
	if err := s.Settle(ctx, "rsv-1", holdUntil); err != nil {
		t.Fatalf("second settle = %v, want nil (outcome reports are retried)", err)
	}
}

func TestSpendRowsAreDeduped(t *testing.T) {
	ctx, s := context.Background(), NewMemory()
	r := spend.Row{
		CallID: "c1", CostUSD: 0.40, PromptTokens: 1000, At: at("2026-07-05T10:00:00Z"),
		Tags: atags.Tags{Project: "group/repo", Rig: "group-repo", BeadID: "gk-1",
			SessionKey: "s1", Rung: "glm", Attempt: 1, Trigger: atags.TriggerIssueTriage},
	}
	n, err := s.AddSpendRows(ctx, []spend.Row{r})
	if err != nil || n != 1 {
		t.Fatalf("first add: %d %v", n, err)
	}
	// The poller overlaps its windows on purpose. The same call WILL arrive
	// twice; counting it twice is a wrong budget.
	n, err = s.AddSpendRows(ctx, []spend.Row{r})
	if err != nil || n != 0 {
		t.Fatalf("re-adding the same CallID added %d rows, want 0", n)
	}
	rows, _ := s.SpendRows(ctx, "group/repo")
	if len(rows) != 1 {
		t.Fatalf("store holds %d rows, want 1", len(rows))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run it, watch it fail.** Run: `go test ./internal/meter/store/ -v`

- [ ] **Step 3: Implement `internal/meter/store/store.go` (the interface + types)**

```go
// Package store is gonk-meter's derived state: project registrations, ladder
// attempt history, budget reservations, and the spend rows read from LiteLLM.
//
// It is DERIVED CACHE, not a source of truth (spec 4.2: "Gonk-side per-project
// state is derived cache only"). The sources of truth are the project's
// .gonk.yml, the operator config, and LiteLLM's spend log. Everything here can
// be rebuilt from them -- with ONE exception, open reservations, which exist
// precisely because the spend log has not caught up yet. Losing them on restart
// is fail-open on headroom for the duration of the spend-log lag; LiteLLM's own
// USD ceiling is the backstop in that window. That is the argument for a durable
// backend (Decision 10: Dolt, gated on Task 0b), and the reason this is an interface.
package store

import (
	"context"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// State is a project's registration state, as reported by GET /v1/projects/{p}.
type State string

const (
	StateActive     State = "active"
	StateDisabled   State = "disabled"    // Effective.Enabled == false
	StateInvalid    State = "invalid"     // .gonk.yml would not load
	StateKeyMissing State = "key-missing" // virtual key not provisioned
)

// KeyRef points at where the project's LiteLLM virtual key lives. It NEVER
// contains the key: a token in an API response ends up in the event bus and in
// every log line that echoes it.
type KeyRef struct {
	SecretName string `json:"secret_name"`
	SecretKey  string `json:"secret_key"`
}

type Registration struct {
	Project string
	Rig     string
	// Raw is the .gonk.yml we resolved from. It is kept so the reresolve loop
	// can re-run gonkcfg.Resolve when the OPERATOR config changes -- without it,
	// the instance kill switch (ADR-002) would not reach an already-registered
	// project until intake happened to re-register it.
	Raw []byte
	// Effective is meaningful ONLY when State is active or disabled. When State
	// is invalid it is the zero value, which per ADR-002 must never be treated
	// as a resolved config -- that is what InvalidDetail and rung.Input.Invalid
	// are for.
	Effective     gonkcfg.Effective
	InvalidDetail string // the gonkcfg.Load error, when State == StateInvalid
	QuietHours    *rung.QuietHours
	State         State
	KeyRef        KeyRef
	KeyAlias      string
	UpdatedAt     time.Time
}

// Reservation is budget committed to a session that has started but whose spend
// rows have not landed. Remaining budget is always
// ceiling - (observed spend + open reservations).
type Reservation struct {
	ID         string
	Project    string
	BeadID     string
	SessionKey string
	Rung       string
	Attempt    int

	// CostUSD is REAL money held against monthly_cost_usd. It is 0 for a local
	// rung -- which is what lets a monthly_cost_usd: 0 project (the onboarding
	// default) run its local rung at all.
	CostUSD float64
	// SyntheticCostUSD is what LiteLLM's virtual-key counter will actually be
	// charged for a local rung (Decision 9). It is 0 for a cloud rung, whose real
	// price already IS that charge. Held for reporting and for the key-ceiling
	// math; NEVER a policy input.
	SyntheticCostUSD float64

	Tokens    int64
	CreatedAt time.Time
	// ExpiresAt is when this reservation stops holding budget. A reservation
	// holds budget until then whether or not its session has finished.
	ExpiresAt time.Time
	// Settled means a terminal outcome has been recorded (or the janitor has
	// given up on it), so the janitor must not touch it again. It does NOT mean
	// the reservation has stopped holding budget -- see Settle.
	Settled bool
}

type Store interface {
	PutRegistration(ctx context.Context, r Registration) error
	GetRegistration(ctx context.Context, project string) (Registration, bool, error)
	DeleteRegistration(ctx context.Context, project string) error
	ListRegistrations(ctx context.Context) ([]Registration, error)

	// RecordAttempt upserts the attempt produced by ONE reservation, keyed by
	// reservationID. Upsert, not append, for two reasons:
	//
	//  1. Outcome reports are retried. Appending would record the same attempt
	//     twice and buy an escalation the project never earned.
	//  2. A session that outlives reservation_ttl gets an infra-failed attempt
	//     written by the janitor -- and may THEN report its real gate-failed.
	//     The real terminal outcome must SUPERSEDE the janitor's guess, or the
	//     ladder silently stops working for long sessions.
	RecordAttempt(ctx context.Context, project, beadID, reservationID string, a rung.Attempt) error
	// Attempts returns the bead's history in the order the attempts were first
	// recorded.
	Attempts(ctx context.Context, project, beadID string) ([]rung.Attempt, error)

	// ReserveIfFits ATOMICALLY re-reads the project's observed spend and open
	// reservations, checks that r still fits under ceiling, and writes r -- or
	// reports that it does not fit. It is the ONLY way a reservation may be
	// created.
	//
	// *** THIS IS A STORE METHOD, NOT A SERVICE METHOD, AND THAT IS THE POINT. ***
	//
	// The check and the write have to be one atomic step, and the atomicity has
	// to live where the DATA lives. A service-side "read totals, decide, write
	// reservation" sandwiched in an in-process mutex is correct for exactly one
	// replica and SILENTLY WRONG for two: the second pod's mutex knows nothing
	// about the first pod's, both see the same headroom, and both spend it.
	//
	// The service's per-project keyedMutex stays -- it is cheap contention
	// control and it keeps Decide deterministic within one process -- but it is
	// NOT what makes the ceiling safe. This method is. Task 0b is what proves the
	// backing store can actually deliver it.
	//
	// Returning (false, nil) is a LOST RACE, not an error: the caller turns it
	// into a defer.
	ReserveIfFits(ctx context.Context, project string, ceiling budget.Budget, want budget.Spend, r Reservation) (bool, error)

	// GetReservation is how /v1/policy/outcome binds a caller-supplied outcome
	// to a reservation METER minted. Without it, `outcome: gate-failed` is a
	// forgery vector: repeat it and a project walks itself up to its most
	// expensive rung.
	GetReservation(ctx context.Context, id string) (Reservation, bool, error)

	// Settle marks a reservation's outcome as recorded, and moves its budget
	// hold to holdUntil.
	//
	// It does NOT free the budget immediately, and that is the entire point. A
	// session's spend rows land in LiteLLM's log SECONDS TO MINUTES after the
	// session ends. If the reservation were released the moment the outcome
	// arrived, then for that whole window remaining budget would be computed as
	// `ceiling - observed(not counting this session) - 0` -- over-reporting
	// headroom by the session's entire actual cost, at exactly the moment that
	// cost is maximal and unbilled. That would happen on EVERY successful
	// session, not just on a restart, and on the token side LiteLLM's ceiling is
	// only a loose backstop (Decision 9), so it will not catch a small overshoot.
	//
	// So the service passes holdUntil = now + max_spend_staleness: the hold ends
	// exactly when the spend data is expected to have caught up. If it has not,
	// the spend snapshot is by definition stale and Decide defers anyway -- the
	// two guards meet cleanly with no gap between them.
	Settle(ctx context.Context, id string, holdUntil time.Time) error

	// OpenReservations returns every reservation still holding budget at now --
	// in-flight AND settled-but-still-holding. Expiry is the only thing that
	// frees a hold.
	OpenReservations(ctx context.Context, project string, now time.Time) ([]Reservation, error)
	// ExpireReservations returns the UNSETTLED reservations that have run past
	// their TTL, EXACTLY ONCE each, and marks them settled -- so the janitor
	// records one infra-failed attempt per dead session, not one per tick. A
	// reservation that has already been settled by an outcome report is not
	// returned: its session is not dead, and its hold expires on its own.
	ExpireReservations(ctx context.Context, now time.Time) ([]Reservation, error)

	// AddSpendRows appends rows, dropping any CallID already stored, and reports
	// how many were new.
	AddSpendRows(ctx context.Context, rows []spend.Row) (int, error)
	SpendRows(ctx context.Context, project string) ([]spend.Row, error)
	AllSpendRows(ctx context.Context) ([]spend.Row, error)

	// SpendCursor is the newest row timestamp the poller has seen; the next poll
	// starts a little before it (overlap is safe: AddSpendRows dedupes).
	SpendCursor(ctx context.Context) (time.Time, error)
	SetSpendCursor(ctx context.Context, t time.Time) error
	// SyncedAt is when the last successful spend sync completed. Its age is what
	// rung.Decide checks against MaxSpendStale.
	SyncedAt(ctx context.Context) (time.Time, error)
	SetSyncedAt(ctx context.Context, t time.Time) error

	// Window is the budget window currently in force. It is persisted so that
	// spend.Advance's monotonicity survives a restart.
	Window(ctx context.Context) (spend.Window, error)
	SetWindow(ctx context.Context, w spend.Window) error
}
```

- [ ] **Step 4: Implement `internal/meter/store/memory.go`**

A single `sync.RWMutex` guarding maps. Key points the implementer must not get wrong (each is covered by a test above):

- `Attempts` and `OpenReservations` return **copies**; never hand out the backing slice.
- `AddSpendRows` keeps a `map[string]struct{}` of seen `CallID`s and returns the count of *new* rows.
- `ExpireReservations` deletes as it collects, so a reservation is reported expired exactly once.
- `Settle` on an unknown ID is a **no-op, not an error** — outcome reports get retried.
- `Settle` moves `ExpiresAt`; it does **not** stop the reservation holding budget. Only expiry does.
- `ExpireReservations` returns only **unsettled** past-TTL reservations.

```go
package store

import (
	"context"
	"sync"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// Memory is the in-memory Store. It is the only implementation this plan ships:
// see the package doc; the SHIPPING store is Dolt (Decision 10), gated on Task 0b.
// attemptRecord ties an attempt to the reservation that produced it, so a
// retried or late outcome report supersedes rather than duplicates.
type attemptRecord struct {
	reservationID string
	Attempt       rung.Attempt
}

type Memory struct {
	mu           sync.RWMutex
	regs         map[string]Registration
	attempts     map[string][]attemptRecord // key: project + "\x00" + beadID
	reservations map[string]Reservation
	rows         []spend.Row
	seen         map[string]struct{}
	cursor       time.Time
	syncedAt     time.Time
	window       spend.Window
}

func NewMemory() *Memory {
	return &Memory{
		regs:         map[string]Registration{},
		attempts:     map[string][]attemptRecord{},
		reservations: map[string]Reservation{},
		seen:         map[string]struct{}{},
	}
}

func beadKey(project, beadID string) string { return project + "\x00" + beadID }

func (m *Memory) PutRegistration(_ context.Context, r Registration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.regs[r.Project] = r
	return nil
}

func (m *Memory) GetRegistration(_ context.Context, project string) (Registration, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.regs[project]
	return r, ok, nil
}

func (m *Memory) DeleteRegistration(_ context.Context, project string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.regs, project)
	return nil
}

func (m *Memory) ListRegistrations(_ context.Context) ([]Registration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Registration, 0, len(m.regs))
	for _, r := range m.regs {
		out = append(out, r)
	}
	return out, nil
}

// RecordAttempt upserts by reservation ID. A retried outcome report overwrites
// rather than appends; a real terminal outcome overwrites the janitor's
// infra-failed for the same reservation.
func (m *Memory) RecordAttempt(_ context.Context, project, beadID, reservationID string, a rung.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := beadKey(project, beadID)
	for i, rec := range m.attempts[k] {
		if rec.reservationID == reservationID {
			m.attempts[k][i].Attempt = a // supersede in place, keeping the order
			return nil
		}
	}
	m.attempts[k] = append(m.attempts[k], attemptRecord{reservationID: reservationID, Attempt: a})
	return nil
}

func (m *Memory) Attempts(_ context.Context, project, beadID string) ([]rung.Attempt, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.attempts[beadKey(project, beadID)]
	out := make([]rung.Attempt, len(src)) // copy: history is not the caller's to edit
	for i, rec := range src {
		out[i] = rec.Attempt
	}
	return out, nil
}

// ReserveIfFits: check-and-write under ONE lock. In the memory store this is
// trivially atomic; in the Dolt store it is a transaction, and Task 0b is what
// proves that transaction is strong enough. Both must behave identically -- that
// is what storetest exists for.
func (m *Memory) ReserveIfFits(_ context.Context, project string, ceiling budget.Budget, want budget.Spend, r Reservation) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-read open reservations INSIDE the lock and fold them into `want`, which
	// already carries the observed spend. Reading them outside would be exactly
	// the check-then-write race this method exists to close.
	for _, o := range m.reservations {
		if o.Project == project && r.CreatedAt.Before(o.ExpiresAt) {
			want.ReservedCostUSD += o.CostUSD
			want.ReservedSyntheticCostUSD += o.SyntheticCostUSD
			want.ReservedTokens += o.Tokens
			if o.BeadID == r.BeadID {
				want.ReservedTaskTokens += o.Tokens
			}
		}
	}
	rem := budget.Remain(ceiling, want)
	// The rung's own draw must still fit on top of everything already held.
	// NOTE the cost check is against REAL dollars only: r.SyntheticCostUSD is
	// deliberately absent here (Decision 9).
	if !rem.FitsCost(r.CostUSD) || !rem.FitsMonthTokens(r.Tokens) || !rem.FitsTaskTokens(r.Tokens) {
		return false, nil // a lost race is a DEFER, not an error
	}
	m.reservations[r.ID] = r
	return true, nil
}

func (m *Memory) GetReservation(_ context.Context, id string) (Reservation, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.reservations[id]
	return r, ok, nil
}

// Settle records that the outcome is in, and moves the budget hold to holdUntil
// rather than dropping it -- the session's spend rows have not landed yet.
func (m *Memory) Settle(_ context.Context, id string, holdUntil time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.reservations[id]; ok {
		r.Settled = true
		r.ExpiresAt = holdUntil
		m.reservations[id] = r
	}
	return nil // unknown id: a retried outcome report, not an error
}

// OpenReservations: everything still holding budget, settled or not.
func (m *Memory) OpenReservations(_ context.Context, project string, now time.Time) ([]Reservation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Reservation
	for _, r := range m.reservations {
		if r.Project == project && now.Before(r.ExpiresAt) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ExpireReservations reports each UNSETTLED, past-TTL reservation exactly once
// -- it marks them settled rather than deleting them, so the janitor writes one
// infra-failed attempt per dead session, not one per tick, and a late real
// outcome can still find its reservation and supersede that attempt.
func (m *Memory) ExpireReservations(_ context.Context, now time.Time) ([]Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Reservation
	for id, r := range m.reservations {
		if !r.Settled && !now.Before(r.ExpiresAt) {
			out = append(out, r)
			r.Settled = true
			m.reservations[id] = r
		}
	}
	return out, nil
}

func (m *Memory) AddSpendRows(_ context.Context, rows []spend.Row) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	added := 0
	for _, r := range rows {
		if _, dup := m.seen[r.CallID]; dup {
			continue
		}
		m.seen[r.CallID] = struct{}{}
		m.rows = append(m.rows, r)
		added++
	}
	return added, nil
}

func (m *Memory) SpendRows(_ context.Context, project string) ([]spend.Row, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []spend.Row
	for _, r := range m.rows {
		if r.Tags.Project == project {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *Memory) AllSpendRows(_ context.Context) ([]spend.Row, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]spend.Row(nil), m.rows...), nil
}

func (m *Memory) SpendCursor(_ context.Context) (time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cursor, nil
}

func (m *Memory) SetSpendCursor(_ context.Context, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = t
	return nil
}

func (m *Memory) SyncedAt(_ context.Context) (time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.syncedAt, nil
}

func (m *Memory) SetSyncedAt(_ context.Context, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncedAt = t
	return nil
}

func (m *Memory) Window(_ context.Context) (spend.Window, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.window, nil
}

func (m *Memory) SetWindow(_ context.Context, w spend.Window) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.window = w
	return nil
}

var _ Store = (*Memory)(nil)
```

- [ ] **Step 5: Watch it pass, gate, commit**

```bash
go test ./internal/meter/store/ -race -v
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add internal/meter/store
git commit -m "feat(meter): store interface + in-memory ledger (registrations, attempts, reservations, deduped spend)"
```

---

### Task 7: `internal/meter/litellm` + `internal/meter/keysink` — the LiteLLM seam

Everything that touches LiteLLM sits behind two interfaces with fakes. **The interfaces are the contract; the HTTP adapters are replaceable** — which is what makes Owner decisions 2 and 3 answerable later without touching a line of policy code.

> **Live-infrastructure boundary.** The HTTP adapters here are written against LiteLLM's documented admin and spend-log endpoints and tested against `httptest` servers. **They are not verified against a real LiteLLM until Plan 06's e2e harness**, and the exact request/response shapes of the pinned LiteLLM version must be confirmed there. That verification is deliberately out of scope for this plan; do not attempt it here.

**Files:** Create `internal/meter/litellm/admin.go`, `internal/meter/litellm/spendsource.go`, `internal/meter/litellm/fake.go`, `internal/meter/litellm/litellm_test.go`, `internal/meter/keysink/keysink.go`

- [ ] **Step 1: Write the interfaces + fakes first (every later test depends on them)**

`internal/meter/litellm/admin.go`:

```go
// Package litellm is gonk-meter's seam onto the LiteLLM proxy: the admin API
// that provisions per-project virtual keys, and the spend log that is the raw
// ledger (spec 6.1, 6.2).
//
// Meter is NEVER in the request path. Agent pods talk to LiteLLM directly with
// their project's virtual key; LiteLLM performs the hard budget refusal at the
// only door. Meter provisions the key, reads the log, and decides policy.
//
// NOTE (Decision 9): a LiteLLM virtual key enforces a USD max_budget and
// nothing else -- there is no token counter. Left alone, that means local rungs
// (priced at $0) never move the dollar counter, so the hard door never fires for
// them, and monthly_tokens has no hard enforcement anywhere.
//
// So we PRICE LOCAL MODELS SYNTHETICALLY. Every local rung carries a nonzero
// synthetic per-token price (opercfg's rung catalog), configured identically in
// LiteLLM's model list, and meter folds the project's TOKEN ceiling into the USD
// max_budget it provisions:
//
//	max_budget = monthly_cost_usd + monthly_tokens * max(pricePerToken(ladder))
//
// The USD door is now a genuine hard door for every rung. It is deliberately
// LOOSE (an upper bound, so it can never cut off legitimate work early); meter's
// per-rung reservation gate remains the tight, primary control. Defence in depth.
//
// The dollars this produces for local models ARE SYNTHETIC and must never be
// presented as real spend -- see spend.Row.Synthetic and the cost API's
// cost_synthetic flag.
package litellm

import (
	"context"
	"time"
)

// KeySpec is the virtual key gonk wants LiteLLM to have for a project. Alias is
// the idempotency key: EnsureKey creates or updates by alias.
type KeySpec struct {
	Alias string
	// MaxBudgetUSD is THE HARD DOOR. nil = unlimited (and then there IS no hard
	// door -- say so, do not pretend). Build it with MaxBudgetFor, never by hand.
	MaxBudgetUSD   *float64
	BudgetDuration string // must be the UTC calendar month; see AD-9
	Models         []string
	Metadata       map[string]string
	TPMLimit       *int
	RPMLimit       *int
}

// MaxBudgetFor converts a project's resolved budget into the single USD ceiling
// LiteLLM can actually enforce. This is the mechanical heart of Decision 9.
//
// LiteLLM's virtual key has ONE counter and it is denominated in dollars. It has
// no token counter. So to give `monthly_tokens` a hard door at all, we fold it
// into the dollar ceiling, using the per-token price each rung will actually be
// charged at (real for cloud, synthetic for local):
//
//	max_budget = monthly_cost_usd + monthly_tokens * max(pricePerToken(ladder))
//
// Pricing the token term at the MOST EXPENSIVE rung in the ladder makes the
// ceiling an UPPER BOUND: it can never close early on legitimate work, which
// would be worse than having no door. It is correspondingly LOOSE -- a project
// that runs entirely on its cheapest rung can exceed monthly_tokens before the
// dollar counter runs out. That is fine and it is the design: meter's per-rung
// reservation gate is the tight, primary control, and this is the backstop that
// catches meter being wrong or being bypassed.
//
// If EITHER ceiling is unlimited, there is no finite dollar ceiling to compute
// and we return nil: the project genuinely has no hard door. Do not silently
// substitute a number.
func MaxBudgetFor(b gonkcfg.EffectiveBudget, ladder []string, catalog map[string]opercfg.RungSpec) *float64 {
	cost := budget.CostLimit(b.MonthlyCostUSD)
	tokens := budget.TokenLimit(b.MonthlyTokens)
	if cost.Unlimited() || tokens.Unlimited() {
		return nil // no finite hard door exists. ADR-004 records this as a known limit.
	}
	var maxPrice float64
	for _, name := range ladder {
		if p := catalog[name].PricePerToken(); p > maxPrice {
			maxPrice = p
		}
	}
	v := float64(cost) + float64(tokens)*maxPrice
	return &v
}

type KeyInfo struct {
	Alias     string
	Token     string // the secret. Handed straight to a KeySink; never logged, never returned by the API.
	ExpiresAt time.Time
}

// Admin is LiteLLM's key-management API.
type Admin interface {
	EnsureKey(ctx context.Context, spec KeySpec) (KeyInfo, error)
	RotateKey(ctx context.Context, spec KeySpec) (KeyInfo, error)
	DeleteKey(ctx context.Context, alias string) error
}
```

`internal/meter/litellm/spendsource.go`:

```go
package litellm

import (
	"context"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// SpendSource is the raw ledger. Since returns every spend row at or after t,
// plus the SOURCE'S OWN CLOCK (LiteLLM's HTTP Date header). The clock is not a
// curiosity: meter compares it to its own before it will trust itself with a
// month-rollover decision (spend.SkewOK). A meter whose clock has jumped
// forward would otherwise roll the window early and hand every project a second
// budget.
type SpendSource interface {
	Since(ctx context.Context, t time.Time) (rows []spend.Row, sourceClock time.Time, err error)
}
```

`internal/meter/litellm/fake.go`:

```go
package litellm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// Fake implements Admin and SpendSource in memory. It is the ONLY LiteLLM the
// tests in this plan see: rung policy and budget accounting are pure functions
// of (config, spend, attempts), so nothing here needs a live proxy (spec 10.2).
type Fake struct {
	mu   sync.Mutex
	Keys map[string]KeyInfo
	Rows []spend.Row
	Now  time.Time

	// Failure injection: the failure-mode matrix is a test, not a paragraph.
	AdminErr error
	SpendErr error
	// ClockOffset skews the source clock relative to Now, to drive SkewOK.
	ClockOffset time.Duration

	n int
}

func NewFake() *Fake { return &Fake{Keys: map[string]KeyInfo{}} }

func (f *Fake) EnsureKey(_ context.Context, spec KeySpec) (KeyInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AdminErr != nil {
		return KeyInfo{}, f.AdminErr
	}
	if k, ok := f.Keys[spec.Alias]; ok {
		return k, nil // idempotent by alias
	}
	f.n++
	k := KeyInfo{Alias: spec.Alias, Token: fmt.Sprintf("sk-fake-%d", f.n)}
	f.Keys[spec.Alias] = k
	return k, nil
}

func (f *Fake) RotateKey(_ context.Context, spec KeySpec) (KeyInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AdminErr != nil {
		return KeyInfo{}, f.AdminErr
	}
	f.n++
	k := KeyInfo{Alias: spec.Alias, Token: fmt.Sprintf("sk-fake-%d", f.n)}
	f.Keys[spec.Alias] = k
	return k, nil
}

func (f *Fake) DeleteKey(_ context.Context, alias string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AdminErr != nil {
		return f.AdminErr
	}
	delete(f.Keys, alias)
	return nil
}

func (f *Fake) Since(_ context.Context, t time.Time) ([]spend.Row, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SpendErr != nil {
		return nil, time.Time{}, f.SpendErr
	}
	var out []spend.Row
	for _, r := range f.Rows {
		if !r.At.Before(t) {
			out = append(out, r)
		}
	}
	return out, f.Now.Add(f.ClockOffset), nil
}

// AddRows is how a test says "LiteLLM billed this".
func (f *Fake) AddRows(rows ...spend.Row) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Rows = append(f.Rows, rows...)
}

var (
	_ Admin       = (*Fake)(nil)
	_ SpendSource = (*Fake)(nil)
)
```

- [ ] **Step 2: Write the failing HTTP-adapter test**

`internal/meter/litellm/litellm_test.go` — an `httptest.Server` standing in for LiteLLM. Assert: the admin key goes in the `Authorization` header and **never** appears in an error message; an unlimited budget sends **no** `max_budget` field (not `null`, not `+Inf`); spend rows are parsed back into `atags.Tags` via `atags.FromMetadata`; a row whose metadata is missing or malformed is **skipped with a counter, not fatal** (an un-attributable call must not stall the ledger, but it must be visible); and the `Date` header is returned as the source clock.

```go
package litellm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPAdminEnsureKey(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"sk-real-abc","key_alias":"gonk-group-repo"}`))
	}))
	defer srv.Close()

	a := NewHTTPAdmin(srv.URL, "sk-master-SECRET", srv.Client())
	limit := 10.0
	info, err := a.EnsureKey(context.Background(), KeySpec{
		Alias: "gonk-group-repo", MaxBudgetUSD: &limit, BudgetDuration: "1mo",
		Models: []string{"qwen3-coder-30b", "glm-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Token != "sk-real-abc" {
		t.Fatalf("token = %q", info.Token)
	}
	if gotAuth != "Bearer sk-master-SECRET" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"max_budget":10`) {
		t.Fatalf("body did not carry the budget: %s", gotBody)
	}
}

// An unlimited budget must OMIT max_budget. Sending +Inf is a marshal error and
// sending null may be read as "no limit" or as "zero" depending on the version
// -- omission is the only unambiguous encoding.
func TestHTTPAdminUnlimitedOmitsBudget(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"key":"sk-x","key_alias":"a"}`))
	}))
	defer srv.Close()
	if _, err := NewHTTPAdmin(srv.URL, "k", srv.Client()).
		EnsureKey(context.Background(), KeySpec{Alias: "a"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotBody, "max_budget") {
		t.Fatalf("unlimited budget still sent a max_budget field: %s", gotBody)
	}
}

// A 500 from LiteLLM must not put the admin key in the error -- errors get
// logged, and logs get shipped.
func TestHTTPAdminErrorDoesNotLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	_, err := NewHTTPAdmin(srv.URL, "sk-master-SECRET", srv.Client()).
		EnsureKey(context.Background(), KeySpec{Alias: "a"})
	if err == nil {
		t.Fatal("a 500 was not an error")
	}
	if strings.Contains(err.Error(), "sk-master-SECRET") {
		t.Fatalf("the admin key leaked into an error string: %v", err)
	}
}

func TestHTTPSpendSourceParsesTagsAndClock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// THE BOUND IS NOT OPTIONAL. An unbounded /spend/logs OOM-killed the live
		// LiteLLM (docs/environment.md, OPERATIONAL HAZARD). Every poll MUST carry a
		// start_date; a request without one is the bug this assertion exists to catch.
		if r.URL.Query().Get("start_date") == "" {
			t.Fatalf("spend poll had no start_date -- an unbounded /spend/logs takes down the cluster's gateway")
		}
		w.Header().Set("Date", "Mon, 13 Jul 2026 10:00:00 GMT")
		w.Header().Set("Content-Type", "application/json")
		// LiteLLM persists the header-supplied tags at metadata.spend_logs_metadata
		// (VERIFIED, docs/environment.md). The row's `metadata` object nests them there.
		_, _ = w.Write([]byte(`[
		  {"request_id":"c1","spend":0.40,"prompt_tokens":8000,"completion_tokens":1500,
		   "startTime":"2026-07-05T10:00:00Z",
		   "metadata":{"spend_logs_metadata":{
		               "gonk_project":"group/repo","gonk_rig":"repo","gonk_bead_id":"gk-1",
		               "gonk_session_key":"s1","gonk_rung":"glm","gonk_attempt":"1",
		               "gonk_trigger":"issue-triage"}}},
		  {"request_id":"c2","spend":0.10,"startTime":"2026-07-05T11:00:00Z","metadata":{}}
		]`))
	}))
	defer srv.Close()

	s := NewHTTPSpendSource(srv.URL, "k", srv.Client())
	rows, clock, err := s.Since(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// c2 has no attribution tags: skipped, not fatal. An un-attributable call
	// must not stall the whole ledger -- but it MUST be counted (Task 9's
	// gonk_meter_spend_rows_unattributed_total).
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (the untagged row must be skipped)", len(rows))
	}
	if rows[0].Tags.BeadID != "gk-1" || rows[0].CostUSD != 0.40 || rows[0].CallID != "c1" {
		t.Fatalf("row = %+v", rows[0])
	}
	if !clock.Equal(time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("source clock = %v (the Date header is what guards month rollover)", clock)
	}
	if n := s.Unattributed(); n != 1 {
		t.Fatalf("unattributed counter = %d, want 1", n)
	}
}
```

- [ ] **Step 3: Implement the two HTTP adapters**

Guidance for the implementer (write these in `admin.go` and `spendsource.go`):

- `NewHTTPAdmin(baseURL, adminKey string, c *http.Client) *HTTPAdmin`. `EnsureKey` POSTs `/key/generate`; on a "already exists" response it POSTs `/key/update`. `RotateKey` POSTs `/key/generate` with the same alias after `/key/delete`. `DeleteKey` POSTs `/key/delete`.
- The request body type uses `MaxBudget *float64` with `json:"max_budget,omitempty"` — **never** marshal `+Inf`. The value comes from `MaxBudgetFor(eff.Budget, eff.Ladder, catalog)` and from nowhere else; `nil` (unlimited) omits the field entirely.
- **Write `TestMaxBudgetFor`.** It is the arithmetic the whole hard door rests on. Cover, at minimum: a cloud+local ladder (the token term prices at the *most expensive* rung); a local-only ladder with `monthly_cost_usd: 0` (the onboarding default — its `max_budget` must be **> 0**, which is the entire point of Decision 9: *before* this change that project's hard door was $0 or nonexistent); an unlimited cost ceiling → `nil`; an unlimited token ceiling → `nil`; and an empty ladder → the token term is 0 (a project with no rungs cannot spend, and `Resolve` has already disabled it anyway).
- **Every error must be constructed from the status code and the response body only.** Never format the request (it carries the admin key) or the `KeyInfo` (it carries the project's token) into an error.
- `NewHTTPSpendSource(baseURL, adminKey string, c *http.Client) *HTTPSpendSource`. `Since` GETs `/spend/logs?start_date=<RFC3339>` (**Decision 11: the HTTP API, not LiteLLM's Postgres** — a supported surface that survives upgrades), decodes the array, and for each entry reads the attribution tags from **`metadata.spend_logs_metadata`** — the sub-object LiteLLM persists opencode's `x-litellm-spend-logs-metadata` header into (VERIFIED, `docs/environment.md`) — by calling `atags.FromMetadata(entry.Metadata.SpendLogsMetadata)`. On error: increment an `unattributed` counter (exposed by `Unattributed() int`) and **skip the row**. Parse the `Date` header with `http.ParseTime`; a missing or unparseable header yields the zero time, which `spend.SkewOK` treats as "no evidence".
- **THE POLL MUST ALWAYS BE BOUNDED, PAGINATED, AND NEVER NAKED.** `Since` **must** set `start_date` (and an end bound) on every request and page through the results; it must **never** issue a `/spend/logs` query with no date window. This is a **code-review blocker**, not a preference: a verified smoke test OOM-killed the live LiteLLM pod (2Gi, exit 137, ~90s outage) with one unbounded query, and meter shares that LiteLLM with the whole cluster (`docs/environment.md`, "OPERATIONAL HAZARD"). Where the pinned LiteLLM version offers the bounded **`/spend/logs/v2`** (paginated, mandatory dates, 10k cap), prefer it and record the version dependency; when resolving a single row prefer a `request_id=` lookup over a window scan. `TestHTTPSpendSourceParsesTagsAndClock` asserts the request carries a `start_date`; keep that assertion.
- **Per-key metadata is a backstop, not the source of truth.** LiteLLM merges per-key metadata (set at virtual-key creation) with the per-request header metadata, **request wins, key fills gaps**. So meter MAY set `gonk_project`/`gonk_rig` as key-metadata when it provisions a project's virtual key, as a backstop for any row that somehow arrives without the header — but the **authoritative** per-session tags (bead, session, rung, attempt, trigger) come from opencode's per-pod header, and that is what the ledger joins on.
- **Enterprise-gate fallback (documented contingency).** LiteLLM's docs *claim* per-k/v `spend_logs_metadata` is an enterprise feature; it was **not** enforced on v1.92.0 (the smoke test wrote and read it back). If a future LiteLLM upgrade starts enforcing the gate, the fallback is the `x-litellm-tags` header → the `request_tags` column (also verified populated); the source would then read tags from `request_tags` instead of `metadata.spend_logs_metadata`. Plan 06 detects the break.
- **`Since` must set `Row.Synthetic`** from the rung catalog: `catalog[tags.Rung].Kind == opercfg.KindLocal`. A rung the catalog has never heard of is **NOT synthetic** — fail closed, so an unknown rung's dollars count against the real money ceiling instead of being waved through as accounting fiction. Give the source the catalog at construction; it is the only place the flag can be set correctly, because LiteLLM has a single `spend` column and does not know the difference (Decision 9).
- Poll with an **overlap**: the service passes `cursor - 2 * pollInterval`. `store.AddSpendRows` dedupes by `CallID`, so overlap is free and it closes the window where a row is written with a timestamp slightly before one we already consumed.

- [ ] **Step 4: Implement `internal/meter/keysink/keysink.go`**

```go
// Package keysink is where a provisioned LiteLLM virtual key is put so that an
// agent pod can read it.
//
// The key is a live credential. It must never travel through the /decide
// response, the Gas City event bus, or a log line -- meter returns only a
// KeyRef (a pointer to where the key lives), never the key itself.
//
// Plan 03 ships the interface and a memory sink, which is enough for every test
// here. The REAL sink writes a per-project Kubernetes Secret, which needs RBAC
// and a live API server -- that is Plan 05's chart (AD-1).
// **gonk-meter cannot deliver keys to pods until Plan 05 implements KeySink.**
package keysink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
)

type KeySink interface {
	// Put stores the token and returns a reference to it. It must be idempotent.
	Put(ctx context.Context, project, token string) (store.KeyRef, error)
	Delete(ctx context.Context, project string) error
}

// Memory is a KeySink for tests. Tokens live only in the process.
type Memory struct {
	mu     sync.Mutex
	tokens map[string]string
}

func NewMemory() *Memory { return &Memory{tokens: map[string]string{}} }

func (m *Memory) Put(_ context.Context, project, token string) (store.KeyRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[project] = token
	return store.KeyRef{SecretName: "gonk-key-" + Slug(project), SecretKey: "LITELLM_API_KEY"}, nil
}

func (m *Memory) Delete(_ context.Context, project string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, project)
	return nil
}

// Token is for tests only: it is the assertion that a key was actually stored.
func (m *Memory) Token(project string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens[project]
}

// Slug turns a GitLab path into a DNS-1123 name fragment, with a hash suffix.
//
// The suffix is NOT decoration. Flattening `/`, `.` and `_` to `-` collides:
// projects "a/b" and "a-b" both flatten to "a-b". They would then share a Secret
// name AND a LiteLLM key alias -- and since EnsureKey is idempotent BY ALIAS,
// the second project would silently adopt the first project's virtual key, and
// with it the first project's max_budget. Two projects sharing one hard USD
// ceiling is the exact failure this whole package exists to prevent.
//
// tagmint has already guaranteed the input is [A-Za-z0-9._/-] and <= 200 bytes.
func Slug(project string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(project) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default: // /, ., _, - all collapse to a single -
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	sum := sha256.Sum256([]byte(project)) // the ORIGINAL path, so a/b != a-b
	return name + "-" + hex.EncodeToString(sum[:4])
}

var _ KeySink = (*Memory)(nil)
```

Write a `keysink_test.go` covering:

- `Slug("Agentic/Gonk_Project.v2")` starts with `agentic-gonk-project-v2-` and has no leading, trailing, or doubled `-`.
- **The collision test, which is the money one:** `Slug("a/b") != Slug("a-b")`. Two projects that flattened to the same alias would share one LiteLLM virtual key and therefore one hard USD ceiling.
- `Slug` is stable across calls (it is a key alias — an unstable slug orphans the previous key on every restart).
- The result is a legal DNS-1123 subdomain (lowercase alphanumeric and `-`, starts and ends alphanumeric, <= 63 chars) — it becomes a Kubernetes Secret name in Plan 05.
- `Put` is idempotent and returns the same `KeyRef` for the same project.

- [ ] **Step 4b: Implement `internal/meter/keysink/k8s.go` — THE KUBERNETES SINK**

**This step exists because the plan previously did not have one, and said so:**
*"`keysink.KeySink` has no Kubernetes implementation… gonk-meter cannot deliver
virtual keys to agent pods at all."* A hard dependency on a component **no plan
owned**. It is Go, it lives in `internal/meter/`, and it is fully testable against
a fake clientset with **no cluster** — so it is this plan's. (The `Role` and
`RoleBinding` that make the writes *legal* are **Plan 05's Task 4**; see AD-1.)

Add client-go, pinned — nothing in this repo floats:

```bash
go get k8s.io/client-go@v0.32.0 k8s.io/api@v0.32.0 k8s.io/apimachinery@v0.32.0
go mod tidy
```

```go
package keysink

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
)

// SecretKey is the key inside the per-project Secret. The agent image reads it as
// its LiteLLM virtual key; the pack (Plan 04) finds it through the KeyRef meter
// returns. *** CHANGING THIS STRING BREAKS THE AGENT IMAGE. *** It is also the
// literal the chart hard-codes (Plan 05) and Plan 06 asserts.
const SecretKey = "LITELLM_API_KEY"

// K8s writes each project's LiteLLM virtual key into its own Kubernetes Secret.
//
// This is the delivery mechanism for the whole attribution chain: an agent pod
// gets a key scoped to ONE project with ONE budget, so LiteLLM's hard refusal
// lands on the right project. The key material NEVER travels through meter's HTTP
// responses, the Gas City event bus, or a log line -- meter hands out only a
// KeyRef, and this is what the ref points at.
//
// One Secret per project (not one Secret with N keys) so that RBAC can later
// scope an agent pod to exactly its own key.
type K8s struct {
	cs        kubernetes.Interface
	namespace string
	prefix    string
}

func NewK8s(cs kubernetes.Interface, namespace, prefix string) *K8s {
	return &K8s{cs: cs, namespace: namespace, prefix: prefix}
}

func (k *K8s) name(project string) string { return k.prefix + Slug(project) }

// Put is idempotent: intake re-registers every project on every reconcile pass
// (every 10 minutes), and a rotation must overwrite rather than duplicate.
func (k *K8s) Put(ctx context.Context, project, token string) (store.KeyRef, error) {
	name := k.name(project)
	ref := store.KeyRef{SecretName: name, SecretKey: SecretKey}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gonk-meter",
				"app.kubernetes.io/part-of":    "gonk",
				"gonk.orac.local/project":      Slug(project),
			},
			Annotations: map[string]string{
				// Slug is lossy; keep the real path for a human and for meter's own
				// reconcile. The PATH is not a secret; the token is.
				"gonk.orac.local/project-path": project,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{SecretKey: []byte(token)},
	}

	_, err := k.cs.CoreV1().Secrets(k.namespace).Create(ctx, sec, metav1.CreateOptions{})
	if err == nil {
		return ref, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		// NEVER wrap the token into an error: errors get logged.
		return store.KeyRef{}, fmt.Errorf("keysink: create secret %s/%s: %w", k.namespace, name, err)
	}
	if _, err := k.cs.CoreV1().Secrets(k.namespace).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return store.KeyRef{}, fmt.Errorf("keysink: update secret %s/%s: %w", k.namespace, name, err)
	}
	return ref, nil
}

// Delete removes a project's key. De-onboarding must leave no live credential
// behind. Deleting an absent key is a no-op.
func (k *K8s) Delete(ctx context.Context, project string) error {
	err := k.cs.CoreV1().Secrets(k.namespace).Delete(ctx, k.name(project), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("keysink: delete secret %s/%s: %w", k.namespace, k.name(project), err)
	}
	return nil
}

var _ KeySink = (*K8s)(nil)
```

`k8s_test.go`, against `k8s.io/client-go/kubernetes/fake` (no cluster, runs in the
standing gate):

- `TestK8sPutCreatesASecret` — type `Opaque`, key `LITELLM_API_KEY`, a
  `gonk.orac.local/project` label so a human can find it.
- `TestK8sPutIsIdempotentAndUpdates` — a second `Put` with a **new** token yields
  the **same** `SecretName` and **one** Secret, whose value is the new token.
  (Intake re-registers every 10 minutes; a non-idempotent `Put` is a Secret storm.)
- `TestK8sDeleteIsIdempotent` — deleting an absent key is not an error.
- `TestK8sHandlesHostileProjectPaths` — `Group/Repo`, a 60-segment nested path,
  `group/repo.with.dots`, `group/_leading-underscore`: every resulting name is a
  legal DNS-1123 subdomain (≤ 63 chars, lowercase). `Slug` is what guarantees
  this, and a project whose key cannot be *stored* is a project that cannot
  *spend* — fail-closed, but baffling to debug.

**What this does NOT prove:** a fake clientset **does not enforce RBAC**. That
Plan 05's `Role` actually permits these writes on a real API server is **Plan 06's**
(it is in Plan 05's "what `helm template` cannot prove" list, item 6).

- [ ] **Step 5: Watch it pass, gate, commit**

```bash
go test ./internal/meter/... -race -v
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add internal/meter/litellm internal/meter/keysink go.mod go.sum
git commit -m "feat(meter): LiteLLM admin + spend-source seams with fakes, key sink interface, Kubernetes key sink"
```

---

### Task 8: `internal/meter/service` — the service, the HTTP API, and the fail-closed behaviors

This is where the pure pieces become a program. The two things that must be right:

1. **`/decide` holds a per-project lock across `read totals -> decide -> reserve`.** If those three steps are not atomic, two concurrent sessions both see the same headroom and both run. The test is not optional: 20 goroutines against headroom for 2, under `-race`, must produce exactly 2 `run`s.
2. **The failure-mode matrix at the top of this plan is a test file, not a paragraph.** Every row gets a test.

**Files:** Create `internal/meter/service/service.go`, `internal/meter/service/http.go`, `internal/meter/service/service_test.go`, `internal/meter/service/http_test.go`, `cmd/gonk-meter/main.go`

- [ ] **Step 1: Write the failing service test (registration + the fail-closed matrix)**

`internal/meter/service/service_test.go`. Sketch of what each test asserts — write them all:

```go
package service

// newTestService wires: memory store, litellm.Fake (Admin + SpendSource),
// keysink.Memory, a fixed clock, and the opercfg from the test YAML. The clock
// is a field, not time.Now(), so month rollover and staleness are ordinary
// table rows.

func TestRegisterProvisionsAKeyAndResolvesConfig(t *testing.T)
//   PUT a .gonk.yml with monthly_cost_usd: 10 under an instance ceiling of 200.
//   Assert: state=active; Effective.Budget.MonthlyCostUSD == 10 (the TIGHTER
//   layer won -- a service that ignored the project layer would report 200);
//   the fake Admin holds a key aliased gonk-group-repo with MaxBudgetUSD == 10;
//   the keysink holds a token; and the RESPONSE contains no token anywhere
//   (assert the raw JSON does not contain "sk-").

func TestRegisterUnlimitedBudgetOmitsMaxBudget(t *testing.T)
//   No budget at any layer -> KeySpec.MaxBudgetUSD is nil, not +Inf, not 0.
//   (An Inf here is the Plan 01 carry-forward failing in a different costume:
//   it would either error the marshal or, worse, be coerced to 0.)

func TestRegisterRejectsInvalidGonkYML(t *testing.T)
//   422 (NOT 400 -- a 400 means INTAKE sent a malformed request; a 422 means the
//   PROJECT's yaml is bad, which is a successful registration of an invalid
//   config). Body is a full meterapi.ProjectResponse carrying gonkcfg's error
//   verbatim (intake echoes it into the MR), state=invalid, effective=null,
//   budget=ZeroBudget (NOT nulls -- nulls mean UNLIMITED), and NO key.

func TestRegisterInvalidConfigDELETESAnExistingKey(t *testing.T)
//   Register a VALID config (key provisioned), then PUT a broken one. Assert the
//   virtual key is GONE from the fake Admin. A project whose .gonk.yml breaks
//   must stop being able to spend -- a config that no longer parses cannot keep
//   authorizing money. This is the fail-closed half of Conflict A.

func TestRegisterResolvesUsingNESTEDGroupPolicy(t *testing.T)
//   Operator config: group "agentic" sets monthly_cost_usd 50; group
//   "agentic/experiments" sets only enabled: true. Register
//   "agentic/experiments/spike" whose .gonk.yml asks for 200.
//   Assert the resolved ceiling is 50 -- the ANCESTOR's. A longest-prefix-match
//   GroupFor returns the experiments policy alone, the group ceiling vanishes,
//   and the project silently gets the instance's. That is a budget escape, and
//   this is the service-level test that catches it (opercfg has the unit test).

func TestRegisterDisabledProjectProvisionsNoKey(t *testing.T)
//   enabled: false -> state=disabled, no key. A disabled project must not hold
//   a live credential.

func TestRegisterRejectsAReorderedLadder(t *testing.T)
//   project ladder [sonnet, qwen-local] against instance [qwen-local, glm,
//   sonnet] with enforce_ladder_order -> 400. (AD-4.)

func TestRegisterRejectsABadTimezone(t *testing.T)
//   schedule.timezone: Mars/Olympus -> 400 at onboarding, not a surprise at 3am.

func TestKeyProvisioningFailureIsFailClosed(t *testing.T)
//   fake.AdminErr = errors.New("connection refused").
//   Register -> state=key-missing (NOT active, NOT an error the caller can
//   ignore). Decide -> defer/virtual-key-missing. Then clear AdminErr, run the
//   reconcile tick, and assert the key appears and Decide returns run.

func TestDecideIsSerializedPerProject(t *testing.T)
//   FIXTURE MATTERS -- get this wrong and the test proves nothing:
//   the project's ladder must be `[glm]` ONLY. With the usual
//   [qwen-local, glm] ladder, 20 fresh beads all have zero prior attempts, so
//   they all target qwen-local -- a LOCAL rung, which skips the cost gate
//   entirely -- and all 20 legitimately run. The race would be invisible.
//
//   Ceiling $1.00; glm costs $0.40/attempt -> headroom for exactly 2.
//   20 goroutines call Decide concurrently on 20 DIFFERENT beads.
//   Assert: exactly 2 run, 18 defer/monthly-cost-exhausted, and the sum of open
//   reservations is exactly $0.80. Run with -race.
//   THIS IS THE TEST THAT PROVES THE CEILING IS NOT RACEABLE. If it is flaky,
//   the lock is in the wrong place -- do not add a retry.

func TestDecideIsSerializedPerProjectOnTokensToo(t *testing.T)
//   The same test against monthly_tokens (ladder [qwen-local], a free local
//   rung, est_tokens 200K, monthly_tokens 500K -> headroom for exactly 2).
//   The LiteLLM backstop for tokens is only a LOOSE one (Decision 9), so if the
//   token reservation races, nothing will catch the overshoot in time.

func TestDeniedActionNeverMintsTagsOrReturnsAKeyRef(t *testing.T)
//   actions.triage: false -> deny/action-not-allowed, and the response has an
//   empty metadata map, an empty key_ref, and no reservation. A denied decision
//   must not hand out an attribution identity or a pointer to a credential.

func TestOutcomeMustMatchAReservation(t *testing.T)
//   POST outcome with an unknown reservation_id -> 400, and NO attempt is
//   recorded. Then POST a valid one twice -> exactly one attempt, and the bead's
//   escalation count is 1, not 2. A replayable gate-failed is a free escalation.

func TestLateOutcomeSupersedesTheJanitorsInfraFailure(t *testing.T)
//   decide -> let the reservation expire -> janitor writes infra-failed -> the
//   session finally reports gate-failed for the SAME reservation.
//   Assert the bead now has ONE attempt, outcome gate-failed, and the next
//   decide escalates. Without this, no session longer than reservation_ttl can
//   ever climb the ladder.

func TestOperatorConfigChangeReResolvesRegisteredProjects(t *testing.T)
//   Register a project (active). Then flip the operator config's instance
//   `enabled: false` and run the reresolve tick.
//   Assert: the registration is now disabled with DisabledReason
//   "disabled by instance policy", and decide returns deny.
//   THE INSTANCE KILL SWITCH MUST WORK WITHOUT A RESTART (ADR-002).

func TestDecideReservesAndOutcomeSettlesWithoutFreeingTheBudget(t *testing.T)
//   decide -> a reservation holds $0.40. POST outcome(success).
//   Assert: the attempt is recorded, AND the reservation STILL holds $0.40 --
//   because the session's spend rows have not landed yet. Advance the clock past
//   max_spend_staleness and assert the hold is gone.
//   Releasing the hold on the outcome report would over-report headroom by the
//   session's full actual cost, on every successful session, in exactly the
//   window where it is unbilled. This test is what stops that.

func TestSettledReservationIsNotJanitored(t *testing.T)
//   decide -> outcome(success) -> advance past reservation_ttl -> janitor tick.
//   Assert: NO infra-failed attempt is recorded. The session reported; it is not
//   dead. Recording one would corrupt the ladder history of a healthy bead.

func TestAnOutcomeCannotBeFlippedAfterTheFact(t *testing.T)
//   POST outcome(success) then POST outcome(gate-failed) for the same
//   reservation. Assert the recorded outcome is still `success` and the bead has
//   earned no escalation. (Only the janitor's infra-failed may be superseded.)

func TestExpiredReservationBecomesAnInfraFailure(t *testing.T)
//   decide (run at qwen-local), never report an outcome, advance the clock past
//   reservation_ttl, run the janitor tick.
//   Assert: the reservation no longer holds budget; the bead's history now has
//   one infra-failed attempt; and the NEXT decide returns qwen-local again --
//   NOT glm. A dead pod must never buy an escalation.

func TestSpendSyncDedupesAndAdvancesTheWindow(t *testing.T)
//   fake returns overlapping rows across two syncs -> stored once.
//   Advance the clock past the month end -> the window rolls, project spend
//   resets, and a project that was deferring on monthly-cost-exhausted now runs.
//   Roll the clock BACKWARDS a month -> the window does NOT move and the spend
//   does NOT reset.

func TestClockSkewFailsReadinessAndDefersBudgetedProjects(t *testing.T)
//   fake.ClockOffset = 20m (> max_clock_skew 5m).
//   Assert: Ready() is false, /readyz is 503, and a project WITH a finite
//   ceiling gets defer/spend-data-stale.
//   NOTE the fixture must have a ceiling: an all-unlimited project has no budget
//   to protect and correctly keeps running -- Decide skips the staleness gate for
//   it by design. Asserting "everything defers" would be asserting a bug.
//   (A meter that trusts a forward-skewed clock rolls the window early and hands
//   every project a second budget.)

func TestSpendSourceOutageMakesSpendStale(t *testing.T)
//   fake.SpendErr set; advance the clock past max_spend_staleness.
//   Assert: a project WITH a ceiling gets defer/spend-data-stale; a project with
//   NO ceilings still runs (it has nothing to be stale about).

func TestColdStartDefersUntilTheFirstSync(t *testing.T)
//   A brand-new service that has never synced: Ready() false, Decide defers.
```

- [ ] **Step 2: Run them, watch them fail.** Run: `go test ./internal/meter/service/ -v`

- [ ] **Step 3: Implement `internal/meter/service/service.go`**

Structure (write the code; this is the shape it must have):

```go
package service

type Service struct {
	cfg   *opercfg.OperatorConfig
	store store.Store
	admin litellm.Admin
	spend litellm.SpendSource
	keys  keysink.KeySink
	mx    *metrics.Metrics

	// now is the clock. It is a field so that every time-dependent behavior --
	// staleness, quiet hours, reservation expiry, month rollover -- is a table
	// row rather than a sleep.
	now func() time.Time

	locks   keyedMutex // per-project serialization of decide+reserve
	mu      sync.RWMutex
	synced  bool // has a spend sync ever succeeded?
	skewOK  bool
}

// keyedMutex serializes work per project. The whole point of /decide is that
// the budget read and the reservation write are ONE step.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}
func (k *keyedMutex) Lock(key string) func() { /* get-or-create, lock, return unlock */ }
```

**Register(ctx, req meterapi.ProjectRequest)** — the whole onboarding path, and **the resolved half of Conflict A**.

Intake sends the **raw `.gonk.yml` bytes** (`req.GonkYML`) plus GitLab metadata. It does **not** resolve them and it does **not** send an `Effective`. Meter is the only component that holds operator instance/group policy, so meter is the only component that can resolve — two resolvers would be two sources of truth for a budget ceiling.

1. `gonkcfg.Load([]byte(req.GonkYML))` -> on error: store `Registration{State: StateInvalid, InvalidDetail: err.Error(), Raw: …}`, **delete any existing virtual key** (a broken config must not keep spending), and return **422** with a full `meterapi.ProjectResponse`: `State: StateInvalid`, `Error: err.Error()` verbatim (intake echoes it into the MR), `Effective: nil`, `Budget: meterapi.ZeroBudget()`.
   - **Do not store a zero `gonkcfg.Effective`** — ADR-002 says `Resolve`'s return value is the only legitimate way to produce one, and a zero `Effective` violates the `DisabledReason != "" iff !Enabled` invariant. `StateInvalid` is what `rung.Input.Invalid` is for.
   - **422, not 400.** A 400 means *intake sent a malformed request* (missing `project_id`, an attribution-unsafe path, an oversized body) and records nothing. A 422 is a **successful, idempotent registration of an invalid config**. Conflating them means intake cannot tell its own bug from a project's bad YAML.
2. `opercfg.CheckLadderOrder(cfg.Instance.Ladder, pc.Ladder)` if `cfg.Meter.EnforceLadderOrder` -> on error: `StateInvalid`, 422 (same path as above).
3. `eff := gonkcfg.Resolve(cfg.Instance, cfg.GroupFor(req.Project), *pc)` — **THE ONLY CALL TO `gonkcfg.Resolve` IN THE ENTIRE SYSTEM.** ADR-002: `Resolve` is the only legitimate producer of an `Effective`. Note `cfg.GroupFor` has already folded **every ancestor group** into one `Policy` (see "Nested groups"), because `Resolve` takes exactly one group layer and GitLab groups nest.
4. Quiet hours, if the project set any:
   ```go
   var qh *rung.QuietHours
   if eff.Schedule != nil && eff.Schedule.QuietHours != "" {
       qh, err = rung.ParseQuietHours(eff.Schedule.QuietHours, eff.Schedule.Timezone)
       if err != nil { /* StateInvalid, 400 */ }
   }
   ```
   (`eff.Schedule` is a `*gonkcfg.Schedule` with `QuietHours` and `Timezone` string fields — `ParseQuietHours` takes the two strings, not the struct.)
5. If `!eff.Enabled`: store `State: StateDisabled` with `eff.DisabledReason`, **delete any existing key** (a disabled project must not hold a live credential), return **200** with `Budget: meterapi.ZeroBudget()`.
6. Build the `litellm.KeySpec`: alias `gonk-<slug>`, `Models` = the LiteLLM model of every rung in `eff.Ladder`, `BudgetDuration` = the operator's month setting (AD-9), and **`MaxBudgetUSD = litellm.MaxBudgetFor(eff.Budget, eff.Ladder, cfg.Catalog)`** — the token ceiling folded into the dollar ceiling (Decision 9). Never build this number by hand.
7. `admin.EnsureKey` -> on error: store `State: StateKeyMissing` and return **200 with that state** (not a 500 — intake's onboarding is not at fault, and the reconcile loop will retry). Metric `gonk_meter_virtual_keys{state="missing"}`. `/decide` will `defer` with `virtual-key-missing` until it lands.
8. `keys.Put(project, info.Token)` -> `KeyRef`. Store `State: StateActive`.

**The response is `meterapi.ProjectResponse`, and its invariants are load-bearing for intake:** `Effective` is nil **iff** `state == invalid`; `Error` is set **iff** `state == invalid`; `DisabledReason` is set **iff** `state == disabled`; `KeyRef` is populated **iff** `state == active`; and `Budget` is `ZeroBudget()` (not an empty `Budget{}`, which is all-nil and means **unlimited**) whenever the project is invalid or disabled. Golden-test every one of them.

**`Delete(ctx, project)`** (de-onboard, `DELETE /v1/projects/{project}`): disable the project, `admin.DeleteKey`, `keys.Delete`, drop the registration. Idempotent — deleting an unknown project is `204`, not `404`: intake retries.

**Decide(ctx, req)** — the money path:

```go
// DecideExtras is everything a Run decision needs that is not policy: the minted
// attribution tags, a POINTER to the key (never the key), and the reservation.
// All three are zero for a defer or a deny -- a decision that is not going to
// run must not hand out an attribution identity or a route to a credential.
type DecideExtras struct {
	Metadata    map[string]string
	KeyRef      store.KeyRef
	Reservation store.Reservation
}

// newID returns a short unguessable id, e.g. "rsv-" + 8 hex from crypto/rand.
func newID(prefix string) string { /* crypto/rand, not math/rand */ }

func (s *Service) Decide(ctx context.Context, req DecideRequest) (rung.Decision, DecideExtras, error) {
	var extras DecideExtras
	unlock := s.locks.Lock(req.Project)   // <-- the critical section starts here
	defer unlock()

	reg, ok, _ := s.store.GetRegistration(ctx, req.Project)
	now := s.now()

	in := rung.Input{
		Registered:      ok,
		Invalid:         ok && reg.State == store.StateInvalid,
		InvalidDetail:   reg.InvalidDetail,
		KeyReady:        ok && reg.State == store.StateActive,
		Effective:       reg.Effective,
		Catalog:         s.cfg.Catalog,
		Trigger:         req.Trigger,
		QuietHours:      reg.QuietHours,
		Now:             now,
		MaxSpendStale:   s.cfg.Meter.MaxSpendStaleness,
		KeyRetryBackoff: s.cfg.Meter.KeyRetryBackoff,
		MaxInfraRetries: s.cfg.Meter.MaxInfraRetries,
	}
	// A cold start or a skewed clock means we do not trust our own numbers.
	// Present that to Decide as maximally-stale spend, so there is exactly ONE
	// place in the codebase that decides what stale spend means.
	if !s.Ready() {
		in.SpendAsOf = time.Time{}
	} else {
		in.SpendAsOf, _ = s.store.SyncedAt(ctx)
	}
	in.Window, _ = s.store.Window(ctx)
	in.Prior, _ = s.store.Attempts(ctx, req.Project, req.BeadID)
	in.Spend = s.spendFor(ctx, req.Project, req.BeadID, now) // observed + open reservations

	// The ceilings, needed again below for the atomic re-check at reserve time.
	bud := budget.FromEffective(reg.Effective.Budget)

	d := rung.Decide(in)
	s.mx.RecordDecision(req.Project, d)

	if d.Kind == rung.Run {
		spec := s.cfg.Catalog[d.Rung]
		cost, synthetic, tokens := rung.Reserve(spec)
		res := store.Reservation{
			ID: newID("rsv"), Project: req.Project, BeadID: req.BeadID,
			SessionKey: req.SessionKey, Rung: d.Rung, Attempt: d.Attempt,
			CostUSD: cost, SyntheticCostUSD: synthetic, Tokens: tokens,
			CreatedAt: now, ExpiresAt: now.Add(s.cfg.Meter.ReservationTTL),
		}

		// The reservation is written by an ATOMIC check-and-write IN THE STORE.
		// We are inside the per-project mutex, but that mutex is contention
		// control, not the safety property -- it does nothing across replicas.
		// ReserveIfFits re-checks the ceiling against the data as it stands at
		// write time, so the ceiling holds even if two pods get here at once.
		fits, err := s.store.ReserveIfFits(ctx, req.Project, bud, in.Spend, res)
		if err != nil {
			return rung.Decision{}, DecideExtras{}, err // fail closed: no reservation, no run
		}
		if !fits {
			// We lost a race against a concurrent session. rung.Decide said yes on
			// a snapshot that is now stale. This is a DEFER, not an error and not a
			// deny: the budget is real, it is just spoken for right now.
			s.mx.RecordLostRace(req.Project)
			return rung.Decision{
				Kind: rung.Defer, Attempt: d.Attempt,
				Reason:     rung.ReasonMonthlyCostExhausted,
				Detail:     "lost a concurrent reservation race for the remaining budget",
				RetryAfter: now.Add(s.cfg.Meter.MaxSpendStaleness),
			}, DecideExtras{}, nil
		}

		tags, err := tagmint.Mint(tagmint.Request{ /* ...req..., Rung: d.Rung, Attempt: d.Attempt */ })
		if err != nil {
			// The session will never start, so drop the hold entirely: settle it
			// into the past rather than holding budget for a session that does
			// not exist.
			_ = s.store.Settle(ctx, res.ID, now)
			return rung.Decision{}, DecideExtras{}, err // 400: a hostile tag never leaves the building
		}
		extras.Metadata, extras.KeyRef, extras.Reservation = tags.Metadata(), reg.KeyRef, res
	}
	return d, extras, nil
}
```

`spendFor` sums `spend.ProjectTotals(rows, window, project)` and `spend.BeadTotals(rows, project, bead)` and folds in `store.OpenReservations(project, now)` into `budget.Spend`'s `Reserved*` fields (splitting out the reservations that belong to *this bead* for `ReservedTaskTokens`).

**Outcome(ctx, req)** — and this one is a **money endpoint too**, because the outcome is what buys an escalation:

```go
// The outcome is CALLER-supplied. Plan Decision 2 refuses a caller-supplied
// attempt count as a forgery vector; a caller-supplied outcome is exactly as
// forgeable -- repeat `gate-failed` and a project walks itself up to its most
// expensive rung. So bind it to a reservation METER minted.
res, ok, err := s.store.GetReservation(ctx, req.ReservationID)
if err != nil { return err }
if !ok || res.Project != req.Project || res.BeadID != req.BeadID || res.Attempt != req.Attempt {
    return ErrUnknownReservation // 400. No reservation, no attempt recorded.
}
if !req.Outcome.Valid() { return ErrBadOutcome } // 400

// SETTLE, do not release. The budget hold moves to now + max_spend_staleness,
// because this session's spend rows have NOT landed yet -- see store.Settle.
_ = s.store.Settle(ctx, res.ID, s.now().Add(s.cfg.Meter.MaxSpendStaleness))
_ = s.store.RecordAttempt(ctx, req.Project, req.BeadID, res.ID, rung.Attempt{
    Attempt: res.Attempt, Rung: res.Rung, Outcome: req.Outcome,
})
```

`RecordAttempt` is keyed by reservation ID, which buys two things for free: a **retried** outcome report overwrites instead of appending (no free escalation), and a **late** real outcome overwrites the janitor's `infra-failed` for the same reservation (a session that outlived its TTL still gets its escalation).

**One restriction on that supersede,** or it becomes a forgery surface of its own: an outcome may overwrite a previously recorded attempt **only if the recorded one was the janitor's `infra-failed`.** Otherwise a caller holding a valid `reservation_id` could keep re-POSTing to flip its own already-recorded outcome (bounded — one rung per reservation — but still a vote it does not get). Enforce it in the service, and test it.

**Loops** (started by `main`, each a `time.Ticker` whose period comes from the operator config):

- `syncSpend`: `spend.Since(cursor - overlap)` -> check `spend.SkewOK(now, sourceClock, maxSkew)` -> `AddSpendRows` (deduped) -> `SetSpendCursor` / `SetSyncedAt` -> `spend.Advance(window, now)` and `SetWindow`. On error: leave `SyncedAt` alone (it goes stale on its own) and increment `gonk_meter_spend_sync_failures_total`. **Never** advance `SyncedAt` on a failed sync.
- `janitor`: `ExpireReservations(now)` -> for each (these are the **unsettled** ones — a session that reported is not dead) `RecordAttempt(..., res.ID, infra-failed)` + `gonk_meter_reservations_expired_total`.
- `reconcileKeys`: for every registration in `StateKeyMissing`, retry `EnsureKey`.
- `reresolve`: for every registration, re-run `gonkcfg.Resolve(cfg.Instance, cfg.GroupFor(p), Load(reg.Raw))` and `PutRegistration`, then update the LiteLLM key's budget if the ceiling changed. **This loop is why `Registration.Raw` exists.** Without it, the operator's instance kill switch (`enabled: false` — ADR-002's whole point) has no effect on an already-registered project until intake happens to re-register it. The operator config itself is re-read from disk on each tick (the chart mounts it as a ConfigMap, and a ConfigMap update is a file change, not a restart); if it fails to validate, **keep the previous config and alert** — never fall back to an unvalidated one, and never fall back to no config at all.

**`Ready()`**: `synced && skewOK`. `/readyz` returns 503 otherwise, so Kubernetes takes meter out of service rather than letting it answer with numbers it does not trust.

- [ ] **Step 4: Implement `internal/meter/service/http.go`**

Go 1.22+ `ServeMux` patterns, wire types with explicit JSON tags matching the API section of this plan, and:

- `bearerAuth` middleware comparing the presented token against **both rotation slots** (read from `GONK_METER_TOKEN_FILE` / `GONK_METER_TOKEN_PREVIOUS_FILE`) with `crypto/subtle.ConstantTimeCompare`, **ORing the results rather than short-circuiting** — a short-circuit leaks which slot matched, and how many are configured. Exempt `/healthz`, `/readyz`, `/metrics`. An empty configured token is a fatal startup error, not an open door.
- **A `defer`/`deny` is HTTP 200.** It is a normal answer. Only a malformed request (400), a missing project (still 200 with `deny`), or an internal failure (500) is an error status.
- **`PUT /v1/projects/{project}` has three codes and they mean different things:** `200` (resolved), `422` (the *project's* `.gonk.yml` will not load — recorded as `state: invalid`, key deleted, `error` echoed to intake), `400` (the *request* is malformed — intake's bug; nothing recorded). Do not collapse 422 into 400.
- **All wire types come from `pkg/meterapi`** (Task 0). `http.go` declares no request/response structs of its own; if it needs one, the contract is missing a type and Task 0 is where it gets added.
- `{project}` path values are `url.PathUnescape`d, then handed to `tagmint` — a project name is untrusted input from GitLab.
- Request bodies are read with `http.MaxBytesReader` (1 MiB; a `.gonk.yml` is tiny).
- Every handler takes `r.Context()` through to the store.

`http_test.go` drives the real mux with `httptest`: assert the JSON shapes character-for-character against golden files in `testdata/golden/*.json`, assert `401` without the bearer token, assert an unlimited budget renders `"monthly_cost_usd": null`, and assert that no response body anywhere contains the string `sk-` (the fake's token prefix).

- [ ] **Step 5: Implement `cmd/gonk-meter/main.go`**

```go
package main

import (
	_ "time/tzdata" // quiet_hours needs IANA data; the image has no /usr/share/zoneinfo
	// ...
)

func main() {
	// Flags/env:
	//   --operator-config                path to the mounted operator YAML (required)
	//   --listen                         :8080
	//   LITELLM_URL                      LiteLLM base URL (required).
	//                                    In the real deployment this is
	//                                    http://litellm.litellm.svc.cluster.local:4000
	//                                    -- BYO, already running (docs/environment.md).
	//   LITELLM_ADMIN_KEY_FILE           path to the admin credential      (required)
	//   LITELLM_ADMIN_KEY_PREVIOUS_FILE  rotation slot 2                   (optional)
	//   GONK_METER_TOKEN_FILE            path to this API's bearer token   (required)
	//   GONK_METER_TOKEN_PREVIOUS_FILE   rotation slot 2                   (optional)
	//
	//   *** THE STORE. THIS IS THE ONE THE PLAN USED TO BE MISSING. ***
	//   GONK_METER_STORE_BACKEND         dolt | postgres                   (REQUIRED)
	//   GONK_METER_STORE_DSN_FILE        path to a file holding the DSN    (REQUIRED)
	//
	//   *** THE KEY SINK (AD-1). Without it, provisioned keys reach nobody. ***
	//   GONK_KEYSINK_NAMESPACE           k8s namespace for per-project key Secrets
	//                                    (unset -> memory sink + a LOUD error log)
	//   GONK_KEYSINK_PREFIX              default "gonk-key-"
	//
	// *** SECRETS ARE READ FROM FILES. *** (Decision 12, and the same rule Plan 02
	// follows for the GitLab bot token and the webhook secret.)
	//
	// Never an env VALUE: the environment leaks into `ps`/`/proc/<pid>/environ`,
	// into crash dumps, and into every child process. Never a flag: flags are
	// visible in `ps` to anything on the node. Never the config file: that file is
	// a ConfigMap and lives in git.
	//
	// The path is Vault/1Password -> ExternalSecret -> k8s Secret -> projected FILE
	// MOUNT. TWO ROTATION SLOTS for each credential, so a rotation is not an
	// outage: write the new secret to slot 1, the old to slot 2, roll, then drop
	// slot 2. The bearer-token check accepts EITHER slot (constant-time compare
	// against both, ORing the results -- do not short-circuit, or the timing
	// reveals which slot matched).
	//
	// NO SECRET MATERIAL IS EVER COMMITTED. House rule; not negotiable. The DSN
	// carries a password, which is exactly why it is a FILE and not a value.
	//
	// Use the same readSecretFile helper Plan 02 specifies: trim exactly one
	// trailing newline, and REFUSE AN EMPTY FILE (an empty bearer token would
	// accept every request; an empty DSN would silently fall back to nothing).
	//
	// opercfg.Load failing is FATAL: meter must not start on a config it cannot
	// validate, because every budget decision flows from it.
}

// openStore selects the durable store. THERE IS NO DEFAULT, AND `memory` IS NOT
// SELECTABLE.
//
// The in-memory store loses reservations on restart, which is FAIL-OPEN on
// headroom for the length of the spend-log lag -- so a typo in an env var must
// never be able to produce it. An unknown backend, an empty DSN file, or a store
// that will not connect is a FATAL startup error. Meter refuses to run without a
// durable store, and that refusal is the whole point.
//
// WHICH backend is in force is decided by Task 0b (Dolt, or the owner-approved
// CNPG Postgres fallback -- docs/environment.md). BOTH are supported here, today,
// so that flipping is one env var and one Secret, not a rewrite. Plan 05's chart
// sets these two vars from `ledger.backend` and `secrets.ledger`.
func openStore(ctx context.Context) (store.Store, error) {
	backend := os.Getenv("GONK_METER_STORE_BACKEND")
	dsn, err := readSecretFile(os.Getenv("GONK_METER_STORE_DSN_FILE"))
	if err != nil {
		return nil, fmt.Errorf("store DSN: %w", err)
	}
	switch backend {
	case "dolt":
		return store.OpenDolt(ctx, dsn)
	case "postgres":
		return store.OpenPostgres(ctx, dsn)
	case "":
		return nil, errors.New("GONK_METER_STORE_BACKEND is required (dolt|postgres); " +
			"there is no default, because defaulting to the in-memory store would lose " +
			"reservations on restart and fail OPEN on budget headroom")
	default:
		return nil, fmt.Errorf("GONK_METER_STORE_BACKEND=%q is not a store (want dolt|postgres); "+
			"`memory` is deliberately not selectable", backend)
	}
}
```

`_ "time/tzdata"` is load-bearing: `time.LoadLocation("America/New_York")` fails in a `scratch`/`distroless` image without it, which would turn every quiet-hours project into a `400` at registration.

**Both store implementations must pass `storetest.Suite` unchanged.** That suite is what makes `GONK_METER_STORE_BACKEND` a *switch* rather than a *fork*: if `ReserveIfFits` means something different on Postgres than on Dolt, the switch is a lie and the budget ceiling depends on which env var somebody set.

- [ ] **Step 5b: The `testclock` seam (Plan 06 hand-back HB-3)**

**Why this exists.** Month rollover, quiet-hours windows and `reservation_ttl`
expiry are the three most important behaviours in this service, and **none of them
can be tested against the real binary by waiting** — the shortest wait is an hour
and the longest is a month. Plan 06's L1 layer injects the clock directly (every
pure function in `pkg/rung`, `pkg/spend` and `pkg/budget` already takes `now` as an
input, deliberately), but at L2/L3 the thing under test is the **binary**, and the
binary has a clock. Without this seam, `TestQuietHoursDeferAndResume`,
`TestBudgetExhaustionBlocksCloudRungsAndProducesDefer` and the rollover tests are
**never proven against the real service** — say so plainly if the seam is refused.

Two files, one symbol:

```go
// cmd/gonk-meter/clock.go
//go:build !testclock

package main

import "time"

// Now is the service's only clock. Production: the wall clock, full stop.
func Now() time.Time { return time.Now().UTC() }
```

```go
// cmd/gonk-meter/clock_testclock.go
//go:build testclock

package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// *** THIS FILE MUST NEVER BE IN A PRODUCTION IMAGE. ***
//
// A gonk-meter that reads its clock from a file is a gonk-meter whose BUDGET
// WINDOW CAN BE MOVED BY ANYONE WHO CAN WRITE THAT FILE. Moving the window
// forward resets every project's spend to zero. That is not a test seam in
// production; it is a budget bypass.
//
// It is therefore behind a build tag, shipped ONLY in the e2e image, and
// Plan 06's Task 9 Step 2 asserts the production binary contains neither the
// `testclock` symbol nor the GONK_TESTCLOCK_FILE literal.
//
// The file holds a signed offset in seconds, applied to the wall clock. An offset
// (not an absolute time) keeps the clock MONOTONE, which spend.Advance requires:
// a backwards jump must never reset a window.
func Now() time.Time {
	path := os.Getenv("GONK_TESTCLOCK_FILE")
	if path == "" {
		return time.Now().UTC()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Now().UTC()
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.Now().UTC().Add(time.Duration(secs) * time.Second)
}
```

`Now` is passed into the service as a `func() time.Time` — it is **not** called
from `pkg/`. The pure packages keep taking `now` as a parameter; this seam moves
only the *binary's* idea of the present.

Ship it as an owner decision (**Plan 06's OD-7**), not silently. If it is refused,
the rollover and quiet-hours behaviours are **L1-only forever**, and Plan 06 must
say so in `docs/adr/ADR-006`.

- [ ] **Step 6: Watch everything pass**

```bash
go test ./internal/meter/... -race -count=1 -v
```

Expected: PASS, including `TestDecideIsSerializedPerProject` under `-race`.

- [ ] **Step 7: Gate and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add internal/meter/service cmd/gonk-meter
git commit -m "feat(meter): service + HTTP API with per-project serialized budget decisions"
```

---

### Task 9: Prometheus metrics and the cost API

Spec 8's required series, plus the cost API of spec 6.2. **One cardinality rule: never label a metric with `bead_id` or `session_key`.** They are unbounded, and a Prometheus label with unbounded values kills the TSDB. Per-bead and per-session cost live in the API and in Loki (where `bead_id` is already a label), not in a metric.

**Files:** Create `internal/meter/metrics/metrics.go`, `internal/meter/metrics/metrics_test.go`; extend `internal/meter/service/http.go`

- [ ] **Step 1: Add the dependency, pinned**

Everything else in this repo is pinned to an exact version (`golang:1.26`, `golangci-lint:v2.12.2`). Do not float this one.

```bash
go get github.com/prometheus/client_golang@v1.20.5
go mod tidy
```

(If v1.20.5 is unavailable, pin whatever exact version resolves and record it — the point is an exact tag in `go.mod`, not that particular number.)

- [ ] **Step 2: Write the failing metrics test**

Use `prometheus/client_golang/prometheus/testutil` to assert exposition text exactly.

```go
func TestBudgetRemainingGaugeEmitsInfForUnlimited(t *testing.T)
//   Prometheus's text format DOES support +Inf, so unlimited stays +Inf here --
//   unlike the JSON API, where it must be null (pkg/budget). Assert the
//   exposition line is `gonk_meter_budget_remaining_usd{project="group/repo"} +Inf`.
//   This is the carry-forward's other half: the SAME value has two correct
//   encodings, and confusing them is how an unlimited project ends up with a
//   9.22e18 gauge in Grafana.

func TestBudgetRemainingTokensGaugeEmitsInfForUnlimited(t *testing.T)
//   The token sentinel is MaxInt64, NOT +Inf -- so this gauge needs an explicit
//   conversion that the USD one does not, and it is the one that actually
//   produces the 9.22e18 in Grafana if you forget. Assert the exposition line is
//   `gonk_meter_budget_remaining_tokens{project="group/repo"} +Inf`.

func TestLocalRungsCostNoRealMoneyButAreMeteredInTokens(t *testing.T)
//   spec 6.3: "Local rungs are cost-0 dollars but metered in tokens for fairness
//   visibility." Assert gonk_meter_tokens_total{rung="qwen-local"} > 0 while
//   gonk_meter_spend_usd_total{rung="qwen-local",synthetic="false"} == 0.

func TestSyntheticSpendIsLabelledAndNeverMixedWithReal(t *testing.T)
//   DECISION 9. A local rung's dollars are an accounting fiction. Assert:
//     gonk_meter_spend_usd_total{rung="qwen-local",synthetic="true"}  > 0
//     gonk_meter_spend_usd_total{rung="qwen-local",synthetic="false"} == 0
//     gonk_meter_spend_usd_total{rung="glm",synthetic="true"}         == 0
//     gonk_meter_spend_usd_total{rung="glm",synthetic="false"}        > 0
//   The label is what lets the Cost dashboard show REAL spend by default. Without
//   it, every dashboard silently reports fiction as money.

func TestNoUnboundedLabels(t *testing.T)
//   Gather every metric and assert no label named bead_id or session_key exists.
//   This test is the guardrail: it will fail the day someone adds one.

func TestDecisionCounterUsesTheBoundedReasonSet(t *testing.T)
//   Every meterapi.Reason* constant is a legal label value and nothing else is.
```

**Dashboard rule, carried to Plan 05.** The shipped Cost dashboard must default to `synthetic="false"` on every money panel, and show synthetic spend only on a separate panel titled so a human cannot mistake it for real money (e.g. "Local inference — synthetic pricing (not billed)"). **Presenting a synthetic dollar as spend is the one way this decision can do real harm.**

- [ ] **Step 3: Implement the collectors**

| Metric | Type | Labels |
|---|---|---|
| `gonk_meter_spend_usd_total` | Counter | `project`, `rung`, `trigger`, **`synthetic`** (`true`/`false`) |
| `gonk_meter_tokens_total` | Counter | `project`, `rung`, `trigger`, `kind` (`prompt`/`completion`) |
| `gonk_meter_budget_remaining_usd` | Gauge | `project` |
| `gonk_meter_budget_remaining_tokens` | Gauge | `project` |
| `gonk_meter_policy_decisions_total` | Counter | `project`, `decision`, `reason` |
| `gonk_meter_ladder_escalations_total` | Counter | `project`, `from_rung`, `to_rung` |
| `gonk_meter_gate_outcomes_total` | Counter | `project`, `rung`, `outcome` |
| `gonk_meter_deferred_beads` | Gauge | `project` |
| `gonk_meter_project_state` | Gauge | `project`, `state` |
| `gonk_meter_virtual_keys` | Gauge | `state` |
| `gonk_meter_spend_sync_age_seconds` | Gauge | — |
| **`gonk_meter_spend_synced_at_seconds`** | Gauge | — |
| `gonk_meter_spend_sync_failures_total` | Counter | — |
| `gonk_meter_spend_rows_unattributed_total` | Counter | — |
| `gonk_meter_reservations_open` | Gauge | `project` |
| `gonk_meter_reservations_expired_total` | Counter | `project` |
| `gonk_meter_clock_skew_seconds` | Gauge | — |
| `gonk_meter_clock_skew_unknown_total` | Counter | — |
| `gonk_meter_budget_window_start_seconds` | Gauge | — |
| `gonk_meter_cold_start_total` | Counter | — |
| `gonk_meter_reservation_races_lost_total` | Counter | `project` |
| `gonk_meter_catalog_drift_total` | Counter | — |

The spend/token counters are updated from the sync loop as new rows land (never recomputed from scratch — a counter that goes backwards breaks `rate()`); the gauges are refreshed on a `refreshGauges` tick.

`gonk_meter_spend_synced_at_seconds` is a **Unix timestamp**, not an age: it is the
absolute `spend_as_of` of the last successful sync, and it is what a test (or a
human) **waits on as a predicate**. `gonk_meter_spend_sync_age_seconds` (`now −
spend_as_of`) stays as the *alerting* series; an age is right for "is this
stale?", an absolute timestamp is right for "has my call landed yet?". Both, and
they are cheap.

- [ ] **Step 3b: `POST /admin/spend/sync` — the forced sync (Plan 06 hand-back HB-2)**

**Why this exists.** Meter's view of spend is a **poll** of LiteLLM's
`/spend/logs` (Decision 11). Every assertion of the form *"the ledger now says the
project spent $X"* therefore needs **a predicate to wait on**, not a sleep — and
without a way to force the poll, the harness's only options are to sleep for the
poll interval (flaky) or to sleep for longer (slow **and** flaky). *A flaky budget
test is worse than no budget test: it trains people to ignore red.*

| Request | Behaviour | Response |
|---|---|---|
| `POST /admin/spend/sync` | Run **one** spend-log poll and **block** until it has completed and its rows are committed. Coalesces: concurrent callers wait on the same pass. | `200` `{"spend_as_of":"2026-07-13T09:58:00Z","rows_ingested":14,"unattributed":0,"synced":true}` |
| — sync fails | Body reports the failure; the endpoint itself is not an error. | `200` `{"synced":false,"error":"…","spend_as_of":"<last good>"}` |
| any other method | — | `405` |

Rules, and they are the same rules as everything else here:

- **It is on the same listener as everything else (`:8080`), and it is
  `Authorization: Bearer`-authenticated** like every non-`/healthz` route. Meter
  has one port; the *intake* service is the one with a public/private split,
  because intake is the one behind an Ingress.
- **It forces a poll; it does not fabricate one.** `spend_as_of` is still LiteLLM's
  truth, and `complete: false` still means what it meant. This endpoint removes a
  *timer*, not a *guarantee*.
- The harness's wait predicate is `spend_as_of >= the timestamp of the call I
  made`, with a deadline — never "sleep 5s and hope". (Plan 06, "Determinism",
  row 6.)
- `harness.forceSpendSync` calls it. Nothing else does, in production or otherwise
  — but it is safe if it is, which is why it is not gated behind a build tag.

- [ ] **Step 4: Implement the cost API handlers**

`GET /v1/cost/bead/{bead_id}`, `/v1/cost/session/{session_key}`, `/v1/cost/project/{project}`, `/v1/cost/instance`, exactly the shapes in the API section above. Every response carries `as_of` (the last successful sync) and `complete` (false iff an open reservation exists for that scope). `GET /v1/cost/project/{p}` also carries `budget`, `remaining`, `window`, and `stale`.

Test that `complete: false` appears while a reservation is open and flips to `true` once the outcome is reported and the spend row has landed — this is the field that stops a commit trailer from claiming a cost it does not yet know (spec 6.1).

- [ ] **Step 5: Gate and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1 && golangci-lint run ./...
git add internal/meter/metrics internal/meter/service go.mod go.sum
git commit -m "feat(meter): Prometheus metrics and the cost API"
```

---

### Task 10: Publish the contracts, record the ADR, verify, update the plan index

- [ ] **Step 1: Write `docs/api/gonk-meter-v1.md`**

The API section of this plan, verbatim, as the published contract. This is what **Plan 02 (gitlab-intake)** and **Plan 04 (pack)** build against. State at the top: the endpoints, request/response shapes, the bounded `reason` set, and the three-valued decision. State that `defer`/`deny` are HTTP 200.

- [ ] **Step 2: Golden-file the wire contract (spec 10.1)**

`internal/meter/service/testdata/golden/*.json` — one file per response shape. `TestWireContractLiterals` asserts the **literal JSON field names** (`"decision"`, `"rung"`, `"attempt"`, `"reason"`, `"retry_after"`, `"metadata"`, `"key_ref"`, `"budget"`, `"remaining"`, `"reservation_id"`, ...), the way `pkg/atags`'s `TestContractLiterals` does. A round-trip test that uses the same struct on both sides would happily accept a rename; this must not.

```go
// The literal field names ARE the contract with gitlab-intake and the pack
// (spec 10.1). A rename is a breaking change and must fail CI even though a
// symmetric round-trip test would pass.
func TestWireContractLiterals(t *testing.T) { /* compare marshaled output to the golden bytes */ }
```

- [ ] **Step 3: Write `docs/adr/ADR-004-rung-policy-and-budget-enforcement.md`**

Contents: the three-valued decision and why `deny` exists; the rung index = gate-failure count (and that infra failures never escalate); reservations, their TTL, and why an expired one becomes an `infra-failed` attempt; the UTC calendar month and monotone rollover; clock skew as a readiness gate; the `+Inf`/`MaxInt64` -> `null` encoding and the fact that Prometheus keeps `+Inf` (**the same value has two correct encodings — say so explicitly**); the cost gate applying only to rungs with a **real** price, and what that makes `monthly_cost_usd: 0` mean; **and the failure-mode matrix, copied verbatim from this plan.** Link ADR-002; state plainly that this ADR does not modify it. Link ADR-003 (Plan 02's intake trust boundary) and state that **meter, not intake, resolves config and enforces quiet hours.**

Plus the four folded-in owner decisions, each with its rationale and its consequences:

- **Synthetic pricing (Decision 9).** Why LiteLLM's USD-only ceiling left token budgets with no hard door; how local rungs get a synthetic per-token price; the `max_budget` conversion formula and why it is a deliberately *loose upper bound*; and — **the load-bearing part** — that real and synthetic dollars are **different currencies that must never be summed in a policy decision**, because if synthetic dollars reached the cost gate, the onboarding default (`monthly_cost_usd: 0`, `ladder: [qwen-local]`) could not afford its own only rung.
- **Dolt (Decision 10),** citing `docs/spikes/dolt-reservation-isolation.md` for what was actually *measured*. If the spike was skipped or inconclusive, **say so in as many words** — an unverified transactional guarantee under a budget ceiling is exactly the thing that must not be quietly assumed. Record that `ReserveIfFits` is an atomic **store** operation precisely because the service's per-project mutex is per-*process*, and that meter therefore runs single-replica (AD-10) unless the spike says otherwise.
- **LiteLLM's spend API (Decision 11),** and every place the polling lag is handled: reservations bridging it, the staleness gate, month-rollover-by-row-timestamp, and `complete: false` on the cost API.
- **Secrets (Decision 12):** file mounts, two rotation slots, never env, never committed.

Also record the **known limits**, so the next reader does not have to rediscover them:
- **THE NETWORK-LAYER BYPASS IS OPEN, AND THIS ADR IS ONE OF THE THREE PLACES THAT MUST SAY SO.** NetworkPolicies are not enforced on the target cluster (Flannel does not implement them; the Cilium HelmRelease is suspended), and LiteLLM routes local models to an **unauthenticated Ollama at `http://192.168.1.142:11434`**. An agent pod can call it directly and burn local GPU with **zero metering**. Every door in this ADR binds only traffic that goes *through* LiteLLM. Therefore: **cloud-rung budgets are hard; local-model budgets are advisory** until Cilium lands. Owner decision 2026-07-13: ship, document, do not gate. `docs/environment.md` is the primary record; Plan 05 ships the policy anyway and Plan 06 skip-tests it. **Do not soften this paragraph.**
- Token budgets now DO have a hard door, but a loose one: LiteLLM's USD ceiling, fed by synthetic local-model prices (Decision 9). The tight gate is still meter's reservation, and it is still session-start only -- meter can refuse to start a session, but cannot stop one mid-flight.
- **The synthetic price has to land in a LiteLLM spend row to do anything at all.** It is configured in the gitops repo at `clusters/orac/apps/litellm/litellm.yaml` (`litellm_settings.model_cost_map`), where local models are priced at **zero** today. Whether `model_cost_map` (the map this repo uses) or a per-model `model_info` block is what LiteLLM actually consults for a **locally-routed** model is **not verified** — Plan 06 Task 5 must answer it. If it is the wrong one, Decision 9 buys nothing.
- Synthetic dollars are an accounting unit, not spend. Every surface that shows them must label them (`cost_synthetic`, the `synthetic="true"` metric label). A dashboard that sums them with real spend is lying.
- The in-memory store loses reservations on restart, which is why it is a TEST-ONLY implementation; the shipping store is Dolt (Decision 10), and Task 0b is what proves its isolation is good enough to be trusted with a ceiling.
- `KeySink` has no Kubernetes implementation until Plan 05 (AD-1).
- Reservations are *estimates*. A session that consumes far more than its rung's `est_cost_usd` overshoots the meter's soft ceiling by the difference; LiteLLM's hard USD ceiling is what stops it, and there is no equivalent for tokens. Tune the estimates (OD-B) and alert on `actual >> estimated`.
- **Meter's spend view is eventually consistent, and the reservation hold is what bridges the gap.** The hold ends at `now + max_spend_staleness` after an outcome, on the assumption that the rows have landed by then. If LiteLLM's spend-log lag ever exceeds `max_spend_staleness`, that assumption is wrong — but the staleness gate then fires and defers, so the system stalls rather than overspends. Verify the real lag in Plan 06 and set `max_spend_staleness` above it.
- The store grows without bound (spend rows, settled reservations, attempt history). Retention is **13 months** (AD-8), pruned by a daily job.

- [ ] **Step 4: Update `PLAN.md`**

Set plan 03's Status to `done`. Add to "Contracts published by plan 03":

- **`pkg/meterapi` — THE shared intake↔meter↔pack wire contract (Task 0).** Plan 03 owns it; Plan 02 and Plan 04 import it and restate nothing. A sha256 drift gate makes a field rename fail CI. **`DecideRequest` has no `attempt` field, and adding one is a forgery vector, not a feature.**
- `pkg/budget` — JSON-safe budget encoding: unlimited is `null` on the wire, `+Inf` in Prometheus. Real and **synthetic** dollars are separate fields and never mix.
- `pkg/opercfg` — the operator-config contract + `docs/schemas/gonk-operator.v1.schema.json`. **This closes ADR-002's "Known gap"**; the "nothing validates operator-supplied Policy" carry-forward can be struck. It also carries the **rung catalog**, including local rungs' **synthetic prices** (Decision 9), and `GroupFor`, which folds **nested** GitLab groups into the single group layer `Resolve` accepts.
- `pkg/rung` — the deterministic ladder policy. `Decide` is pure: config + spend + attempts -> run/defer/deny. **Its cost gate reads real dollars only.**
- `docs/api/gonk-meter-v1.md` — the service API Plans 02 and 04 call.
- `docs/spikes/dolt-reservation-isolation.md` — what Task 0b actually measured.

And add to "Carried into later plans":

- **Plan 02 (intake) — already reconciled, but restate it:** intake pushes **raw** `.gonk.yml` to `PUT /v1/projects/{project}` and **never calls `gonkcfg.Resolve`**. Anything requiring an `Effective` (enabled? ladder? budget?) comes from meter's response. Intake has **no quiet-hours code**. Intake **does** call `/policy/decide` as **Gate 1** (it gates the first dispatch and fires the order only on `run`); the pack's `gonk-dispatch` exec order calls it again as **Gate 2**, the enforcement point. `/decide` is idempotent on an open reservation so the two callers never double-reserve.
- **Plan 04 (pack):** (a) the `gonk-dispatch` **exec order** (Gate 2, not the formula — a formula cannot make the call) must call `POST /v1/policy/decide` before every pour, **re-deciding every time and never trusting a rung/reservation passed in order vars** (a controller re-sling that trusted them would spend unmetered), and stamp the returned `metadata` **verbatim** as opencode's `x-litellm-spend-logs-metadata` header on every LiteLLM request; intake calls the same endpoint as Gate 1, and `/decide` is idempotent on an open reservation so the two never double-reserve; (b) it must **never** send an attempt count — meter owns ladder state; (c) a `defer` is a **normal answer**: park the bead in `waiting-for-capacity` and retry at `retry_after` (**this is where quiet hours land**, and it is the only place a deferral is handled); (d) a `deny`/`ladder-exhausted` labels the bead `gonk::needs-human` and stops (AD-6); (e) the gate step must call `POST /v1/policy/outcome` with a **strictly classified** outcome — if it reports an infra failure as `gate-failed`, it buys an escalation the project did not earn, and **that classification is the single most important thing the pack gets right**; (f) with `provenance.include_usage`, the session must **not write a cost trailer when the cost API returns `complete: false`** (AD-7) — it would publish a wrong number into permanent git history.
- **Plan 05 (chart):** `internal/meter/keysink.K8s` is **this plan's** (Task 7 Step 4b); **the chart owns the `Role`, `RoleBinding` and `ServiceAccount` that make its writes legal, and the values that set `GONK_KEYSINK_NAMESPACE` / `GONK_KEYSINK_PREFIX`** (AD-1's ownership table). Without the Role, every `Put` is a `403`, every project sits in `key-missing`, and `/decide` defers forever — fail-closed, loud, and still broken. The chart must also set **`GONK_METER_STORE_BACKEND`** (`dolt`|`postgres`, no default) and **`GONK_METER_STORE_DSN_FILE`** (a file, from a Secret — the DSN carries a password), from `ledger.backend` and `secrets.ledger`. **Meter runs single-replica** (`replicas: 1`, `strategy: Recreate`) unless Task 0b proved the store's isolation is sufficient without it (AD-10). Secrets are **file mounts with two rotation slots** — `LITELLM_ADMIN_KEY_FILE`, `LITELLM_ADMIN_KEY_PREVIOUS_FILE`, `GONK_METER_TOKEN_FILE`, `GONK_METER_TOKEN_PREVIOUS_FILE` — from `existingSecret` refs, **never env values** (Decision 12). The operator config is a mounted ConfigMap, and **LiteLLM's price map must carry the same synthetic prices for local models as the rung catalog does** (Decision 9) — in the deployed instance that map is `clusters/orac/apps/litellm/litellm.yaml` → `spec.values.litellm_settings.model_cost_map` in the **gitops** repo, where **local models are priced at zero today**. The instance ladder must be non-empty (`opercfg.Load` refuses to start otherwise). The **Cost dashboard must default to `synthetic="false"`** and never present synthetic dollars as spend. And the chart must ship the egress NetworkPolicy **with the honest caveat that it is not enforced on this cluster** — see "What the hard door actually covers".
- **Plan 06 (e2e) — the hand-backs this plan now SATISFIES, so the harness never sleeps:** **HB-2** = `POST /admin/spend/sync` + the `gonk_meter_spend_synced_at_seconds` gauge (Task 9 Step 3b). **HB-3** = the `//go:build testclock` clock seam reading `GONK_TESTCLOCK_FILE` (Task 8 Step 5b) — and Plan 06 Task 9 Step 2 must assert the **production** image contains neither the symbol nor the literal, because a meter whose clock can be moved by a file is a meter whose budget window can be reset by anyone who can write that file.
- **Plan 06 (e2e) — and it must ALSO verify:** none of the LiteLLM HTTP adapters have ever spoken to a real LiteLLM. Verify `/key/generate`, `/key/update`, `/key/delete`, and `/spend/logs` against the pinned version. Verify that a LiteLLM key's `budget_duration: "1mo"` resets on the **same boundary** as meter's UTC calendar month (AD-9) — if it is a rolling 30 days, the soft and hard doors reset on different days and that is a real defect. **Measure LiteLLM's actual spend-log lag** and confirm `max_spend_staleness` is set above it (Decision 11). **Verify a local model's synthetic price is configured identically in LiteLLM and in the rung catalog, and that the USD door actually closes on a local-only project that exhausts its token budget** — that is Decision 9's whole claim, and nothing before Plan 06 tests it against a real proxy. Confirm a dedicated (non-master) LiteLLM admin key can perform every admin call (OD-A). Also verify spec 11.5: budget exhaustion demonstrably blocks cloud rungs and produces `defer`.

- [ ] **Step 5: Run everything CI runs, from a clean tree**

```bash
gofmt -l .                       # must print NOTHING
go vet ./...
go test ./... -race -count=1
golangci-lint run ./...
go build ./cmd/gonk-meter
```

- [ ] **Step 6: Prove the mutation tests still bite**

The saboteur suites are the guarantee that the money tables are not vacuous. Confirm they are actually running (not skipped, not compiled out):

```bash
go test ./pkg/budget/ ./pkg/rung/ -run Saboteurs -v -count=1
```

Expected: both PASS, and the verbose output lists every saboteur. If either package reports `no tests to run`, the suite has been lost in a refactor — restore it.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "docs: publish gonk-meter v1 API, ADR-004, and complete plan 03"
```

---

## Definition of done

- `gofmt -l .` prints nothing; `go vet ./...`, `go test ./... -race -count=1`, and `golangci-lint run ./...` are green (the standing local gate — CI's runners were offline through Plan 01).
- **Task 0b has actually been run, and its result is written down.** Dolt either provides the isolation the reservation race needs, or it does not and a fallback is in force. **"We assume it works" is not a passing state for this item.**
- `pkg/meterapi` exists, is byte-identical to Task 0's normative source, and the drift gate is armed. `internal/meter/service/http.go` declares **no wire types of its own**. `grep -rn "attempt" pkg/meterapi/` shows no field on `DecideRequest`.
- **`gonkcfg.Resolve` is called in exactly one place in the repo.** `grep -rn "gonkcfg.Resolve" --include=*.go | grep -v _test.go` returns **one** line, in `internal/meter/service`. (Intake calling it is Conflict A regressing.)
- `pkg/rung.Decide` and `pkg/budget.Remain` are **pure** and exhaustively table-tested, and **every saboteur in both mutation suites is caught** — including the two new ones that guard the real/synthetic firewall. No test in this plan requires LiteLLM, GitLab, or Kubernetes (the Dolt racing test is `//go:build dolt`-tagged and is Task 0b's, not the unit gate's).
- **The Decision-9 invariant holds:** a project with `monthly_cost_usd: 0` and `ladder: [qwen-local]` **runs**, no matter how much synthetic spend it has accumulated. That is one named table row in `pkg/rung` and one in `pkg/budget`, and if either is missing the onboarding flow is dead on arrival.
- All three Plan 01 carry-forwards are closed: `+Inf` serializes (Task 0/1), operator `Policy` is validated (Task 2), attribution tag values are charset-gated at the boundary (Task 5).
- Every row of the failure-mode matrix has a test in `internal/meter/service`, including the concurrency race under `-race`.
- **No secret is read from an env value, a flag, or the config file.** `grep -rn "os.Getenv" cmd/gonk-meter/` shows only *paths* (including `GONK_METER_STORE_DSN_FILE` — the DSN carries a password and is therefore a file), never material.
- **The store is selectable and `memory` is not.** `GONK_METER_STORE_BACKEND` (`dolt`|`postgres`, **no default**) and `GONK_METER_STORE_DSN_FILE` are both required; an unknown backend, an empty DSN file, or a store that will not connect is a **fatal startup error**. `store.OpenDolt` and `store.OpenPostgres` both pass `storetest.Suite` **unchanged** — otherwise the switch is a fork and the ceiling depends on an env var.
- **`internal/meter/keysink.K8s` exists and is tested against a fake clientset** (Task 7 Step 4b). With `GONK_KEYSINK_NAMESPACE` unset, meter falls back to the memory sink **and logs at ERROR** that keys are reaching nobody. (The `Role` that makes the writes legal is Plan 05's; that it actually works is Plan 06's.)
- **The two Plan 06 hand-backs this plan owns are shipped:** `POST /admin/spend/sync` (HB-2) blocks and returns `spend_as_of`; the `testclock` build (HB-3) exists **and the production build does not contain it**.
- **Nothing in this plan claims a budget cannot be bypassed at the network layer.** ADR-004, the metric help strings, and the dashboards all say it the honest way: **cloud-rung budgets are hard; local-model budgets are advisory** until Cilium lands (`docs/environment.md`). Grep the diff for the phrase "cannot be bypassed" and make sure every occurrence is qualified.
- `docs/api/gonk-meter-v1.md` is published and golden-tested; `ADR-004` is written; `docs/spikes/dolt-reservation-isolation.md` records what was measured; `PLAN.md` records the new contracts and the new carry-forwards.
- Every remaining open question is either an **"Assumed default"** the code actually implements, or an **"Owner decision needed"** block that is still flagged in `PLAN.md`. Nothing has been silently decided.




