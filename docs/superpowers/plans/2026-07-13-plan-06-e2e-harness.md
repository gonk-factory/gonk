# Plan 06: End-to-end test harness (kind + gitlab-ce + stub model, kill tests)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the layered test harness that makes gonk *trustworthy by construction* (spec goal 7): a deterministic stub model server, a hostile-config corpus, a three-way ledger assertion library, and three suites — in-process integration, real-container component, and full-stack e2e — that prove hard budgets are hard, that every component fails **closed** when killed mid-flight, and that the escalation ladder never escalates on an infra failure.

**Architecture:** Three layers, each honest about what it proves and what it costs. **L1** (`test/integration`, plain `go test`) wires the *real* intake and *real* meter binaries' packages in one process against fakes — fast, runs in the standing gate. **L2** (`test/component`, `-tags component`) runs the *real* LiteLLM and the *real* ledger backend in containers with the stub model — this is where every "we have never spoken to a real LiteLLM" question from Plan 03 gets answered. **L3** (`test/e2e`, `-tags e2e`) installs the chart on a cluster with a real gitlab-ce and the stub model — the spec-11 walking-skeleton milestones. A **synthetic session** driver (`test/harness.SyntheticSession`) reproduces the pack's `/decide` → LLM calls → `/outcome` cycle without opencode or Kubernetes, so the entire money path is assertable at L1 and L2.

**Tech Stack:** Go 1.26, `net/http` stdlib, `go test` (build tags for the heavy layers), podman (`--network=host`), **the real 3-node orac cluster in a throwaway namespace** (kind is *not installed* and is *not* the default — see below), `gitlab/gitlab-ce:18.10.1`, LiteLLM, Dolt **or** CNPG Postgres, Helm.

**Spec:** `docs/superpowers/specs/2026-07-12-gonk-stack-design.md` — goal 7, sections 6 (budgets/ladder), 9 (security), **10.4 and 10.6** (e2e + rebuild discipline), **11 (validation milestones, incl. 11.5 budget exhaustion and 11.6 kill tests)**.

**Environment:** `docs/environment.md` — read off the live cluster, and **authoritative**. It answers four of this plan's owner decisions and **rewrites L3**:

| Fact | What it changes here |
|---|---|
| **`kind` is NOT installed. A real 3-node cluster IS reachable** (`admin@orac`, k8s v1.34). | **L3's primary path is the real cluster, in a throwaway namespace `gonk-e2e-<runid>`.** kind is a fallback that may never be used. This also sidesteps podman's broken CNI bridge entirely — the problem the old plan spent a page working around. **OD-3 is answered.** |
| **GitLab is 18.10.1, Community Edition.** | The e2e fixture pins **`gitlab/gitlab-ce:18.10.1`**. The golden payloads are only valid for that version. **Nothing may exercise an EE feature.** **OD-2 is answered.** |
| **Images:** `registry.orac.local/agentic/gonk-project/<image>:<tag>`, pull secret `gitlab-registry-pull-creds`, **exact tags, never `latest`**. | The harness **pushes** its per-run images to that registry and the cluster **pulls** them. No `kind load`, no `ctr images import`, no `sudo`, no SSH. **OD-1 is answered — and this was flagged as "the most likely thing to block Task 7 on day one".** |
| **Private CA.** trust-manager publishes a `trust-bundle` ConfigMap into **every** namespace (including the throwaway one). | Services get `SSL_CERT_FILE`; **no test may disable TLS verification.** |
| **CI has never run — every GitLab runner is offline.** | Unchanged, and this plan was already honest about it. **The real gate is local, forever, until a runner exists.** |
| **NetworkPolicies are NOT ENFORCED** (Flannel; Cilium suspended), and LiteLLM routes local models to an **unauthenticated Ollama at `192.168.1.142:11434`**. | **The egress-denial test is WRITTEN AND SKIPPED.** See "The NetworkPolicy gap" below. This is the single most important change in this plan, and it must not be softened into a TODO. |

---

## The NetworkPolicy gap: the one test in this plan that is written to be skipped

Spec §9's *"budgets cannot be bypassed"* is **not met at the network layer** on the
target cluster. `docs/environment.md` records it; the owner's decision is **ship
it, document the gap, do not gate on it.**

- **NetworkPolicies are not enforced.** Flannel does not implement them; the
  **Cilium HelmRelease is suspended**.
- **The bypass is concrete:** LiteLLM routes local models to **Ollama at
  `http://192.168.1.142:11434`, which requires no credential.** An agent pod can
  call it directly, skip LiteLLM, and burn local GPU with **zero metering and no
  ceiling.**

**What this plan does about it, exactly — and an executor must not do anything
else:**

1. **Write the egress-denial test.** In full, correctly, as if it ran.
2. **`t.Skip` it**, with a message that names Cilium (Task 7, Step 3).
3. **Do NOT delete it. Do NOT let it pass vacuously.** A test that "passes" because
   the pod could not be created, or because `curl` was missing, is worse than no
   test: it is a green light over an open door. Assert the *preconditions* before
   skipping — if the agent pod is not there, that is a **failure**, not a skip.
4. **When Cilium lands, un-skipping this test is the gate.** And note the trap:
   **a wrong `agentPodSelector` (Plan 05's OD-5) renders fine and enforces
   nothing.** So **the test, not the chart template, is what will actually prove
   it** — which is precisely why deleting it now would cost more than it saves.

Everywhere else in this plan, the claim is stated honestly: **cloud-rung budgets
are hard** (a cloud call needs a key the pod only ever holds via LiteLLM's virtual
key, and LiteLLM refuses at the ceiling — `TestVirtualKeyIsAHardDoorInTheRealDeployment`
proves it); **local-model budgets are advisory** until Cilium is unsuspended.

---

## ⚠️ READ THIS FIRST: this plan is written against unexecuted plans

**Only Plan 01 has shipped.** `pkg/gonkcfg` and `pkg/atags` exist. **Nothing else does.**

Plans 02 (gitlab-intake), 03 (gonk-meter), 04 (pack & images) and 05 (chart) are **written but not executed**. Every service, package, flag, endpoint, image, chart value and pack formula this plan tests **does not exist yet**. Every reference below is cited as *plan + task* so you can check it against reality:

| This plan depends on | Which is defined in | Status |
|---|---|---|
| `pkg/glab`, `pkg/glab/glabtest`, `pkg/ghook`, `pkg/intake`, `cmd/gonk-intake` | Plan 02, Tasks 1–10 | **not written** |
| `pkg/meterapi` (the wire contract) | Plan 03, Task 0 (normative); Plan 02 Task 6 lands it | **not written** |
| `pkg/budget`, `pkg/opercfg`, `pkg/spend`, `pkg/rung` | Plan 03, Tasks 1–4 | **not written** |
| `internal/meter/{store,litellm,keysink,service}`, `cmd/gonk-meter` | Plan 03, Tasks 5–9 | **not written** |
| **Which ledger backend is in force (Dolt or CNPG Postgres)** | Plan 03, **Task 0b** — a blocking spike | **unrun** |
| The pack, the dispatch formula, the outcome gate, the agent image | Plan 04 | **not written** |
| The Helm chart, `values-e2e.yaml`, the k8s `KeySink` | Plan 05 | **not written** |

**Consequences for the executor of this plan:**

1. **Do not start Plan 06 before Plans 02–05 have landed.** The one exception is **Task 1 (the stub model server)**, which depends on nothing in this repo and can be built at any time.
2. **Re-validate every symbol before you use it.** When you reach a task, `grep` for the package/function/flag/endpoint it names. If it is absent or has a different shape, **that is the plan being stale, not you being wrong** — fix the harness against reality and record the delta in an `**Amendment (found in execution, <date>)**` block, exactly as Plan 01 did. Plans 01–03 all carry such blocks; this system attracts this class of drift.
3. **Some of what this plan asks for does not exist in Plans 02–05 and must be added there.** Those are listed under "Hand-backs" below. They are small, but the harness cannot be deterministic without them.

---

## Hand-offs from Plans 02 and 03 that land here (the checklist)

Both plans were instructed to flag anything needing live infrastructure and hand it to Plan 06. **This is the complete list. Every item maps to a task below and every one must be closed before Plan 06 is `done`.**

### From Plan 02 (`docs/superpowers/plans/2026-07-13-plan-02-gitlab-intake.md`, "Plan 06 e2e verification items", lines 5173–5205)

| # | Item | Lands in |
|---|---|---|
| **P2-1** | **Golden payload refresh.** `pkg/ghook/testdata/*.json` were hand-built from GitLab docs, never captured from a real instance. Capture from gitlab.orac.local, diff, repeat on every GitLab upgrade (spec 12.6). | Task 7 |
| **P2-2** | **Does the CE member payload carry `created_at`?** Plan 02's AD-3 re-invite rule ("re-invite = `member.created_at > MR.closed_at`") depends on it. If absent, decline is permanent-until-manual-reset — a degraded but safe fallback we must consciously accept. | Task 7 |
| **P2-3** | **Hook provisioning against real GitLab CE** — including whether GitLab accepts a hook URL carrying a query parameter (Plan 02's `?gen=N` freshness marker) and returns it unchanged. | Task 7 |
| **P2-4** | **MR assignee on CE:** a single `assignee_id` (multi-assignee is paid). Confirm the onboarding MR is assigned and visible to maintainers. | Task 7 |
| **P2-5** | **Webhook TLS:** does GitLab trust the ingress certificate with `enable_ssl_verification: true`? | Task 7 |
| **P2-6** | **Hook auto-disable:** GitLab disables hooks that fail repeatedly. Confirm Plan 02's 200-on-drop policy (ADR-003.3) actually prevents it. | Task 7 |
| **P2-7** | **The full onboarding scenario** (spec 11.2): invite bot → onboarding MR → merge → `pending` → scaffold MR → `valid` → file an issue → triage comment lands. | Task 7 |
| **P2-8** | **Kill test:** restart intake mid-flow. No duplicate onboarding MRs, no duplicate triage comments — i.e. confirm the controller really dedupes on `OrderRequest.BeadAnchor`. | Tasks 6, 8 |
| **P2-9** | **The intake↔meter seam against a real meter:** raw `.gonk.yml` round-trips; a bad one is **422 not 400**, carrying `gonkcfg.Load`'s message; a de-onboard `DELETE` actually deletes the LiteLLM key; and **an operator flipping the instance kill switch turns a project `disabled` in intake's cache within one `MeterResyncInterval` without the project's config changing** (the failure mode meter's `config_hash` short-circuit could hide). | Tasks 4 (seam), 5 (key deletion), 7 (kill switch) |
| **P2-10** | **Quiet hours end to end:** file an issue inside the quiet window → intake **fires the order** → meter **defers** → the pack **runs it when the window ends**. Nothing lost, nothing run early. | Task 8 |

### From Plan 03 (`docs/superpowers/plans/2026-07-13-plan-03-gonk-meter.md`, "Carried into later plans → Plan 06", line 6476, plus lines 94, 135, 180, 424, 455)

| # | Item | Lands in |
|---|---|---|
| **P3-1** | **No LiteLLM adapter has ever spoken to a real LiteLLM.** Verify `/key/generate`, `/key/update`, `/key/delete`, `/spend/logs` against the pinned version. | Task 5 |
| **P3-2** | **`budget_duration: "1mo"` — calendar month or rolling 30 days?** (AD-9.) If rolling, meter's soft door (UTC calendar month) and LiteLLM's hard door reset on **different days** and there is a window each month where they disagree. **That is a real defect, and this is the only place it can be found.** | Task 5 |
| **P3-3** | **Measure LiteLLM's actual spend-log lag** and confirm `max_spend_staleness` (Decision 11) is set above it. Too low and the factory stalls; too high and reservations under-cover the gap. | Task 5 |
| **P3-4** | **Synthetic price agreement** (Decision 9): a local rung's `synthetic_usd_per_1m_tokens` in the rung catalog must equal the `input/output_cost_per_token` LiteLLM has configured for the same model. Nothing before Plan 06 can check this — they are two different files. | Task 5 |
| **P3-5** | **The USD door actually closes on a local-only project that exhausts its token budget.** That is Decision 9's entire claim and it has never been tested against a real proxy. | Task 5 |
| **P3-6** | **A dedicated (non-master) LiteLLM admin key can perform every admin call** (OD-A). If it cannot, we are forced onto the master key and must know now. | Task 5 |
| **P3-7** | **Spec 11.5:** budget exhaustion demonstrably blocks cloud rungs and produces `defer`. | Tasks 4, 8 |
| **P3-8** | **Rung-catalog ↔ LiteLLM `/model/info` drift** (AD-3): verify the real model list matches the catalog; `gonk_meter_catalog_drift_total` must be 0 in a correct deployment. | Task 5 |
| **P3-9** | **The concurrent-reservation race against the store that is actually in force.** Task 0b is a *spike* — it proves `ReserveIfFits` in isolation. Plan 06 must prove it **end to end, through `/decide`**, against **whichever backend Task 0b left in force (Dolt, or Postgres on CNPG — the fallback is owner-approved)**, and with **meter restarted mid-race**. | Tasks 5, 6, 8 |
| **P3-10** | **`fakeMeter` proved behaviour, not the wire.** Plan 02's tests all run against a fake meter (Plan 02 line 2715: "only on behaviour, which is what Plan 06 is for"). Confirm the real meter's wire shape against the real intake client. | Task 4 |

### Hand-**backs**: things Plans 02–05 must add, or the harness cannot be deterministic

**Status: four of the five have been ACCEPTED INTO THE OWNING PLANS.** They are no longer requests; they are cited tasks. **Verify each one exists before you rely on it** (Task 9's re-validation checklist) — if a plan shipped without its hand-back, the harness's only alternative is `time.Sleep`, and *a sleeping e2e test is a flaky e2e test, and a flaky budget test is worse than no budget test: it trains people to ignore red.*

| # | Owner | What | Status | Why the harness needs it |
|---|---|---|---|---|
| **HB-1** | **Plan 02, Task 10 Step 5** (`cmd/gonk-intake` private listener, `:9090`) | **`POST /admin/reconcile?wait=true`** — kick a pass **that starts at or after the request**, block until it completes, return an `intake.ReconcileSummary` (JSON: `states`, `meter_pushes`, `dispatched`, `errors`, `result`). Bare `POST /admin/reconcile` still returns `202` immediately. | **✅ ACCEPTED** — specified in Plan 02, Task 10 Step 5, with `TestAdminReconcileWaitBlocksAndSummarizes`. | Intake's reconcile loop runs every ~10 minutes. Without this the harness either **sleeps ten minutes per assertion** or races the timer. Note the subtlety Plan 02 encodes: waiting on a pass *already in flight* would observe a **pre-request** world, which is the exact race this endpoint exists to remove. |
| **HB-2** | **Plan 03, Task 9 Step 3b** (meter, `:8080`, bearer-auth like every other route) | **`POST /admin/spend/sync`** — force one spend-log poll, **block**, return `{"spend_as_of":…,"rows_ingested":…,"synced":…}`. Plus the **`gonk_meter_spend_synced_at_seconds`** gauge (an absolute Unix timestamp — a *predicate to wait on*, distinct from the `…_age_seconds` gauge, which is for *alerting*). | **✅ ACCEPTED** — specified in Plan 03, Task 9 Step 3b; the endpoint is in Plan 03's endpoint table. | Meter's view of spend is a **poll**. Every "assert the ledger says X" needs a predicate: `spend_as_of >= the timestamp of the call I made`, with a deadline. |
| **HB-3** | **Plan 03, Task 8 Step 5b** | A **clock seam**: `cmd/gonk-meter/clock_testclock.go` behind `//go:build testclock`, whose `Now()` applies a **signed second offset** read from `GONK_TESTCLOCK_FILE`. An *offset*, not an absolute time, so the clock stays **monotone** (`spend.Advance` requires it). The production build has no such file and no such symbol. | **✅ ACCEPTED** — specified in Plan 03, Task 8 Step 5b. **Task 9 Step 2 of THIS plan asserts the production binary contains neither the symbol nor the literal.** | Month rollover, quiet-hours windows and `reservation_ttl` expiry cannot be tested by waiting: the shortest wait is an hour and the longest is a month. See **OD-7**. |
| **HB-4** | **Plan 04** (the pack) | The outcome gate must (a) publish its classification (`success` / `gate-failed` / `infra-failed` / `aborted`) to the event bus, and (b) be drivable to a **deterministic gate failure** by a canned model response that produces no artifact. | **⚠ NOT ACCEPTED — PLAN 04 IS NOT WRITTEN YET.** It is therefore **recorded here as a requirement Plan 04 must satisfy**, and it is repeated in the "Requirements for Plan 04" section below so that whoever writes Plan 04 cannot miss it. **Do not let it evaporate:** without (a) and (b), *"infra failures never escalate a rung"* — the single subtlest invariant in the system — **cannot be distinguished from "nothing escalates at all"**, and the ladder tests are vacuous. |
| **HB-5** | **Plan 05, Task 9 Step 1b** | **`chart/values-e2e.yaml`** — the per-run image tags, the **`testclock`** meter image, the e2e rung catalog with **round** synthetic prices, a fixed instance ladder, and `litellm.externalURL` pointed at a **harness-deployed** LiteLLM whose only upstream is the stub. | **✅ ACCEPTED, with one correction.** The original ask said *"bundled LiteLLM… `litellm.enabled: true`"* — **there is no such value.** Plan 05's chart is **external-LiteLLM-only** (its OD-3), and adding a subchart to satisfy a test would be the tail wagging the dog. **The harness deploys its own LiteLLM into the run namespace and passes the URL.** | The harness must not hand-assemble the deployment it is supposed to be testing: the first time the two diverge, e2e goes green on something nobody deploys. |

### Requirements for Plan 04 (which does not exist yet — carry these forward)

**Plan 04 has not been written.** These are the things it must satisfy for this harness to mean anything. Whoever writes Plan 04: this list is a hand-back, not a suggestion.

1. **HB-4a — publish the outcome classification.** The gate step emits `success` | `gate-failed` | `infra-failed` | `aborted` onto the Gas City event bus, and calls `POST /v1/policy/outcome` with the same value. Plan 03 is blunt about why this matters: *"if it reports an infra failure as `gate-failed`, it buys an escalation the project did not earn, and that classification is the single most important thing the pack gets right."*
2. **HB-4b — a deterministic gate failure.** A canned stub-model response that produces **no artifact** must drive the gate to `gate-failed`, reliably, every run. Without it, `TestInterleavedFailuresCountOnlyGateFailures` cannot tell a working ladder from a dead one.
3. **The agent-pod labels** (`networkPolicy.agentPodSelector`, Plan 05's **OD-5**). Gas City is **not deployed yet**, so nobody knows them. **A wrong selector renders fine and enforces nothing** — this test suite is the only thing that can catch that, and only once Cilium is unsuspended.
4. **`OrderRequest.BeadAnchor` is an idempotency key** (Plan 02's carry-forward): intake can fire the same order twice across a restart, and a duplicate bead is duplicate spend. Kill tests **K13** and **K18** rest on this entirely.
5. **The pack never sends an attempt count** to `/decide` (Plan 03, Decision 2 — a caller-supplied attempt is a forgery vector for climbing the ladder).

---

## Test layers: what each one proves, and what it cannot

**Not everything needs a kind cluster, and pretending otherwise is how you end up with a suite nobody runs.**

### L0 — Unit + contract (already owned by Plans 01–03; listed for completeness)

Pure `go test ./...`. `gonkcfg` schema/drift, `atags` literals, `rung.Decide`'s exhaustive table, `budget.Remain`'s saboteurs, `glabtest`-driven intake, `httptest`-driven meter.
**Proves:** the money logic and the parsers, exhaustively.
**Cannot prove:** that the pieces are wired to each other correctly, or that any external system behaves as its docs claim.
**Cost:** seconds. Runs on every commit.

### L1 — In-process integration (`test/integration/`, no build tag)

The **real** `pkg/intake` and the **real** `internal/meter/service` in one process, on `httptest` listeners, against: `glabtest` (fake GitLab, Plan 02 Task 2), a fake LiteLLM admin+spend API, the memory `Store`, and the **real stub model server** (in-process). A `SyntheticSession` driver plays the pack's part.
**Proves:** the intake↔meter wire contract *as the real client speaks it to the real server* (closes **P3-10**); the state machine end to end; the hostile-config corpus never crashes anything; the escalation ladder's infra-vs-gate rule; the concurrent-reservation race against the memory store; **the onboarding-default "brick" regression**; that no assertion depends on model prose.
**Cannot prove:** anything about LiteLLM's real behaviour, the real store's isolation, real GitLab's payloads, or Kubernetes.
**Cost:** target **≤ 60 s**, `-race`. **Runs in the standing gate** (`go test ./... -race`). This is where the majority of the value is, and it needs no containers at all.

### L2 — Component, real containers (`test/component/`, `-tags component`)

Real **LiteLLM** (pinned tag), real **ledger backend** (Dolt **or** CNPG Postgres — whichever Task 0b left in force; `GONK_LEDGER` selects), the **stub model** and the **real `cmd/gonk-meter` binary**, all as containers on the host network. No GitLab, no Kubernetes, no opencode.
**Proves:** every Plan 03 live-infra hand-off (P3-1 … P3-9): the admin API shapes, the `1mo` boundary, the real spend-log lag, the synthetic-price agreement, that the **USD door is a hard door** even when meter is bypassed entirely, and that the reservation race holds against the **real** store, including across a meter restart.
**Cannot prove:** GitLab behaviour, chart correctness, pod-level failure modes.
**Cost:** target **≤ 5 min** warm. Requires containers; see the **podman/WSL** section.

### L3 — Full stack (`test/e2e/`, `-tags e2e`)

**A throwaway namespace `gonk-e2e-<runid>` on the REAL orac cluster** (the default and the documented path), the full Helm chart, a **real `gitlab-ce:18.10.1`**, a **harness-deployed LiteLLM** whose only upstream is the stub, real opencode agent pods.
**Proves:** spec 11's walking-skeleton exit criteria — helm install with zero idle agent pods; the onboarding MR flow against real GitLab; triage end to end; spend attribution at bead/project granularity; budget exhaustion blocking a cloud rung; **that the LiteLLM virtual key is a hard door even with meter bypassed**; and the **pod-level kill tests** (spec 11.6). Closes every Plan 02 hand-off (P2-1 … P2-10).
**Cannot prove:** **that the agent-pod egress is actually denied — the cluster does not enforce NetworkPolicy at all** (Flannel; Cilium suspended). That test is written and **skipped**; see "The NetworkPolicy gap". Cannot prove that a real model does useful work (that is the opt-in nightly against bailey, spec 10.5 — **out of scope**). Cannot prove cloud-provider behaviour; there is no real cloud spend anywhere in this harness, by design.
**Cost:** **this is the expensive one.** See below.

> **Why the real cluster and not kind: `kind` is not installed, and the cluster is.**
> The old plan made kind the default and spent a page working around podman's
> broken CNI bridge — a bridge kind **requires** and that **no flag fixes**. On this
> box the whole problem evaporates: run L3 against `admin@orac` in a namespace we
> create and destroy. It is faster (no node containers to boot), it exercises the
> **actual** CNI, the **actual** Traefik, the **actual** cert-manager and the
> **actual** trust-manager `trust-bundle` — and *those* are the things a chart test
> most needs to be real. **It also means L3 is the only layer that can ever tell us
> the NetworkPolicy is unenforced, which is exactly what it did.**
>
> **The rules that make this safe** (a throwaway namespace on a live cluster is a
> loaded gun, and Task 7 treats it that way):
>
> - **Everything is namespaced to `gonk-e2e-<runid>`**, created at the start and
>   deleted at the end (`t.Cleanup`, plus a `make e2e-clean` that reaps orphans by
>   label). **No test may touch an object outside its namespace**, and a preflight
>   refuses to run if `kubectl config current-context` is not the expected one.
> - **The harness NEVER points at the production LiteLLM
>   (`litellm.litellm.svc.cluster.local:4000`).** It deploys its own into the run
>   namespace, upstream = the stub model, and asserts it. The real one routes local
>   models to **Ollama on a real GPU** and writes to the owner's **real spend
>   ledger**: an e2e run against it would burn real compute, pollute real cost data,
>   and make every dollar assertion nondeterministic.
> - **The ledger is in-namespace too** (a bundled Dolt StatefulSet, or a CNPG
>   `Cluster` — `postgres.mode: cnpg`, **never** `mode: shared`). **The e2e suite
>   must not get tenancy on `databases-app/postgres`**, which is the owner's real
>   database.
> - **Images go through the real registry**: build, tag `e2e-<runid>`, push to
>   `registry.orac.local/agentic/gonk-project/`, and let the cluster pull with
>   `gitlab-registry-pull-creds`. **No sideload, no `sudo`, no SSH.** Tags carry the
>   run id, so a cached `latest` cannot silently win. (This closes **OD-1**, which
>   the plan previously called "the most likely thing to block Task 7 on day one".)
> - **`make e2e` always rebuilds and re-pushes.** House rule: *always rebuild an
>   image to deliver a code change; never copy a file into a running container.*

### The honest runtime table

| Layer | Cold | Warm | Containers | Runs where |
|---|---|---|---|---|
| L0 unit | 10 s | 10 s | none | every commit, standing gate |
| L1 integration | 60 s | 60 s | none | every commit, standing gate |
| L2 component | ~6 min | ~3 min | LiteLLM, ledger, stub (host network) | `make component`, pre-merge (local) |
| L3 e2e | **~18 min** | **~10 min** | gitlab-ce (host) + a namespace on the **real** cluster | `make e2e`, on demand |

(L3's numbers drop because there are no kind node containers to boot. **Re-measure
them and write the real figures into ADR-006** — this table is an estimate until
someone runs it, and a plan that states an unmeasured number as fact is doing the
thing this whole plan exists to prevent.)

**gitlab-ce is heavy and slow to boot.** A cold `gitlab/gitlab-ce` container runs `gitlab-ctl reconfigure` and is not usable for **4–8 minutes**; on a warm data volume it is **60–180 s**. It wants ~4 GB RAM and will thrash below that.

**Therefore gitlab-ce is cached and reused, and it is NOT inside the cluster.** Two decisions that follow from that:

1. **gitlab-ce runs as a long-lived host container**, not a chart component and not a kind workload. It is a *test fixture*, not part of gonk. `make gitlab-up` boots it once into a **named persistent volume** and leaves it running across many e2e runs; `make gitlab-down` is a separate, deliberate act. The cluster reaches it over the host IP. This also sidesteps a large amount of in-cluster networking pain.
2. **No test may assume a clean GitLab.** Each run creates a **fresh group** `gonk-e2e-<runid>` and works only inside it. Tests are therefore idempotent against an instance that already has fifty runs' worth of debris. This is the single biggest thing that keeps L3 from being a 25-minute suite you can only run once.

---

## The container runtime problem: podman under WSL with broken CNI

**This WILL bite. Address it before you write a single container line.**

The dev box's "docker" is **podman under WSL2, and its CNI bridge networking is broken**. Containers cannot get a bridge IP; the only thing that works is `--network=host`.

What that means, layer by layer:

- **L1 needs no containers at all.** This is not a coincidence — it is *why* L1 is designed the way it is. The most valuable tests must not be hostage to the runtime.
- **L2 works fine with `--network=host`.** Every component gets a fixed loopback port (stub `:8081`, LiteLLM `:4000`, ledger `:3307`/`:5432`, meter `:9091`). There is no inter-container DNS to break, because everything is `127.0.0.1`. The harness **detects podman and adds `--network=host` automatically** (Task 2), and a preflight refuses to run if the ports are occupied.
- **L3 does not use kind, so the bridge never comes up.** This used to be the hardest problem in the plan; it is now a non-problem, and the reason is one line of `docs/environment.md`: **`kind` is not installed, and a real 3-node cluster is reachable.** kind's node containers *require* a working bridge (they get an IP on a `kind` network and publish the API server from it), `--network=host` is not an option for them, and **no flag fixes a broken bridge.** So we do not use kind. We use the cluster.

**L3 speaks to *a kubeconfig*, never to kind.** `test/harness/cluster.go` has one job: get a `*rest.Config` and a namespace.

| Provider | Selected by | Images reach the cluster by | Status |
|---|---|---|---|
| **existing cluster (DEFAULT)** | nothing — this is the path. `GONK_E2E_KUBECONFIG` overrides the default kubeconfig; `GONK_E2E_CONTEXT` guards against pointing at the wrong cluster. | **pushed to `registry.orac.local/agentic/gonk-project/` with tag `e2e-<runid>`** and pulled with `gitlab-registry-pull-creds`. No sideload, no `sudo`, no SSH. | **The documented, supported path.** |
| **kind** | `GONK_E2E_PROVIDER=kind`, **and only if `kind` is on `PATH` and the bridge works** | `kind load docker-image` | **Not installed on this box; kept only so the harness is portable.** If the doctor cannot create a bridge network, this provider is **refused with a diagnosis**, not attempted. |

**`make e2e-doctor` is still a mandatory preflight** (Task 2), but its job changed. It now:

1. Confirms `kubectl` can reach a cluster and prints **which context** — and **fails loudly if that context is not the one the run expects**. *Creating a throwaway namespace on the wrong cluster is the one mistake this harness could make that hurts.*
2. Confirms the `trust-bundle` ConfigMap will exist in a new namespace (trust-manager publishes into every namespace; if it does not, TLS to `gitlab.orac.local` fails and the failure looks like DNS).
3. Confirms the run can **push** to `registry.orac.local` and that the namespace will have `gitlab-registry-pull-creds` (an `ExternalSecret`, not something the harness invents).
4. Checks the host's free ports for L2's fixed loopback map, and memory for gitlab-ce (~4 GB).
5. **Reports that NetworkPolicy is unenforced** (Flannel; Cilium suspended) so the skipped egress test is expected, not a surprise.
6. Only if `GONK_E2E_PROVIDER=kind`: probes the bridge and refuses with the exact diagnosis if it is broken.

A future executor must not discover any of this by watching something hang.

Two rules that come from the house rules and are not negotiable here:

- **Always rebuild an image to deliver a code change. Never copy a file into a running container.** Every suite's fixture rebuilds the images it needs and reloads them into the cluster. `make e2e` builds; there is no "fast path" that skips it.
- **If an app runs in a container, test it in a container.** L1's in-process wiring is a *complement* to L2/L3, never a substitute. A change to `cmd/gonk-meter`'s wiring is not proven by L1.

---

## Determinism is a requirement, not an aspiration

**A flaky budget test is worse than no budget test: it trains people to ignore red.** Every source of nondeterminism in this system, and the mechanism that controls it:

| # | Source | Control |
|---|---|---|
| 1 | **Model output** (prose, tool calls, whether it does the task at all) | The **stub model server** (Task 1). Canned, scripted per test. **No test in this plan may reach a real model.** The harness deploys **its own LiteLLM** whose only upstream is the stub, and **asserts the config** — because the NetworkPolicy that would *enforce* it is **not enforced on this cluster** (Flannel; Cilium suspended), so a config assertion is the only control we actually have. **The harness must never point at the production LiteLLM**, which routes local models to a real Ollama on a real GPU and writes to the owner's real spend ledger. |
| 2 | **Token counts** | The stub *reports* usage; the script says exactly how many prompt/completion tokens each call consumed. Cost is then LiteLLM's `tokens × configured price` — a pure function of two things the harness controls. |
| 3 | **Float arithmetic on money** | Test prices are fixed and round (`$0.25/1M` synthetic local, `$2.00/1M` cloud); token counts are round. Comparisons go through `ledger.ApproxUSD(got, want)` with a `1e-9` tolerance. **Never `==` on a dollar.** |
| 4 | **Wall clock**: month rollover, quiet-hours windows, `reservation_ttl` expiry | The **clock seam** (**HB-3**): a `testclock` meter build reads a monotonic offset from a file; the harness advances it. **No test crosses a time boundary by sleeping.** L1 injects the clock directly (Plan 03 made it an input to the pure functions). |
| 5 | **Clock skew detection** (meter compares its clock to the spend source's `Date` header) | A **`Date`-rewriting reverse proxy** in front of LiteLLM's `/spend/logs` (Task 5). Skew is *injected*, not waited for. |
| 6 | **Spend-log polling lag** (meter's view of spend trails reality) | Forced sync (**HB-2**) + **wait on a predicate**: poll until `spend_as_of` advances past the timestamp of the call we made, with a deadline. At L2 the real lag is **measured and recorded** (P3-3), and `max_spend_staleness` is set above it. |
| 7 | **Intake's 10-minute reconcile loop** | Forced reconcile (**HB-1**). Also: the harness prefers *forcing a reconcile* over *waiting for a webhook* wherever both would work — reconciliation is the correctness path (spec 5.2), so it is also the deterministic one. |
| 8 | **Container / pod start order** | Every fixture blocks on readiness before proceeding: `/readyz` for gonk services, LiteLLM's `/health/liveliness`, a `SELECT 1` for the ledger, `kubectl wait --for=condition=Ready` in-cluster. **No fixture sleeps.** |
| 9 | **GitLab's async job processing** (Sidekiq delivers webhooks and renders MRs out-of-band) | Poll the **GitLab API for the observable end state** (the MR exists; the note exists; the branch exists) with a deadline. Never sleep, never assume a hook arrived. Where a test does not specifically care about hook delivery, force a reconcile instead. |
| 10 | **Goroutine scheduling in the race tests** | Assert the **invariant**, never the winner: *exactly K reservations succeeded* and *the persisted total never exceeded the ceiling*. Run `-count=5` minimum — a race that manifests one run in three is still a budget escape. |
| 11 | **Port collisions / concurrent runs** | L1 uses `httptest` ephemeral ports. L2 uses fixed loopback ports with a **preflight that refuses to start if any is occupied** (forced by `--network=host`). L3 uses a per-run namespace and a per-run GitLab group. |
| 12 | **Stale images** | Always rebuild; always reload. Image tags carry the run id, so a cached `:latest` cannot silently win. |
| 13 | **Accumulated GitLab state across runs** | Per-run group; no test depends on a clean instance; no test deletes anything it did not create. |
| 14 | **Map iteration order in assertions** | `test/ledger` sorts every row set on `(bead_id, attempt, rung)` before comparing. |
| 15 | **HTTP retry jitter** | Clients get `RetryBackoff = func(int) time.Duration { return 0 }` in-process; at L2/L3 the deadline is the control, not the backoff. |
| 16 | **DNS / hostnames under host networking** | Everything is `127.0.0.1:<fixed port>`. The one place a name is needed — the cluster reaching gitlab-ce and the stub — uses the **host IP captured at preflight** and injected as a chart value. |

---

## Owner decisions needed / assumed defaults

### Owner decision needed

**OD-1 — Registry for test images. [ANSWERED — `docs/environment.md`]**
Images go to the **in-cluster GitLab container registry**:
`registry.orac.local/agentic/gonk-project/<image>:e2e-<runid>`, pulled with the
per-namespace **`gitlab-registry-pull-creds`** Secret. **No sideload, no `sudo`,
no SSH, no `kind load`.** `values-e2e.yaml` (HB-5) carries the registry and the
pull secret. Tags carry the run id, so a stale `latest` cannot silently win —
and `latest` is rejected by the chart's schema anyway (Plan 05, G11).
*This was previously flagged as "the most likely thing to block Task 7 on day
one". It is now answered, and it is the reason the real cluster is a cheaper L3
target than kind.*

**OD-2 — The gitlab-ce version pin. [ANSWERED — `docs/environment.md`]**
**`gitlab/gitlab-ce:18.10.1`.** The live instance is **18.10.1, Community Edition**
(`enterprise: false`). Record it in `test/e2e/versions.env` and **assert it at
runtime** against both the fixture's and the real instance's `/api/v4/version` —
**the suite fails loudly if the pin, the fixture, and the goldens disagree**,
because a golden payload is only valid for the version it was captured from
(spec 12.6).
**CE is not a detail — it is a constraint:** group webhooks are Premium and
multiple MR assignees are EE, so **no test may take a path a CE user cannot**.
Plan 02's onboarding MR sets exactly one `assignee_id`; **P2-4 verifies it.**

**OD-3 — Can the dev box run kind? [ANSWERED — it does not have to.]**
**`kind` is NOT installed, and a real 3-node cluster IS reachable.** L3's primary
path is therefore a **throwaway namespace `gonk-e2e-<runid>` on the real orac
cluster**, which also sidesteps podman's broken CNI bridge entirely. kind remains
behind `GONK_E2E_PROVIDER=kind` purely so the harness is portable to a machine
that has it; on this box it will never run.
*What the owner is accepting by this, stated plainly:* **e2e runs create and
destroy a namespace on the live cluster.** The guardrails are in Task 7 — a
context check that refuses to run against an unexpected cluster; strict namespace
scoping; **never** the production LiteLLM (it routes to a real GPU and a real
spend ledger); **never** tenancy on `databases-app/postgres` (the owner's real
database). If any of that is unacceptable, the fallback is to fix podman's bridge
(`netavark`) and install kind — and say so now, not after the first surprise.

**OD-4 — How long may a full e2e run take, and where does it run?**
*Assumed default:* **L1 in the standing gate; L2 and L3 are `make` targets run on demand by a human.** They are **not** in CI, because — per PLAN.md — **every runner on gitlab.orac.local is offline and this repo's CI has never executed.** The harness is therefore designed to be **meaningful when run locally** and CI-ready if runners ever appear; the `.gitlab-ci.yml` jobs added in Task 9 are `manual` and `allow_failure: false`, so they are honest about never having run.
*Needed from the owner:* if L3 should ever gate a merge, it needs a runner with ≥8 CPU / ≥16 GB and privileged containers. Neither exists today.

**OD-5 — Which ledger backend is in force?** Depends entirely on **Plan 03 Task 0b**, which has not been run.
*Assumed default:* the harness supports **both**, selected by `GONK_LEDGER=dolt|postgres`, and the race suite runs against whichever is set. **Until one is formally retired, `make component` runs both.** If Task 0b fell back to CNPG Postgres (owner-approved 2026-07-13), `GONK_LEDGER=postgres` is the default and the Dolt path is kept only as a regression net.

**OD-6 — The LiteLLM version pin.** Every P3-* answer is version-specific. `budget_duration: "1mo"` semantics, the spend-log lag, and the admin-key permission model can all change between releases.
*Assumed default:* pin an exact tag in `test/e2e/versions.env`; Task 5 writes `docs/spikes/litellm-verified.md` recording *which version was verified for what*. **A version bump invalidates that document and re-runs Task 5.**
*Needed from the owner:* the version they will actually deploy.

**OD-7 — Is a `testclock` build acceptable?** (**HB-3**.) It is a second binary that can have its clock moved by a file.
*Assumed default:* yes, behind a `//go:build testclock` tag, shipped **only** in the e2e image. A test in Task 9 asserts the **production** image's binary does not contain the `testclock` symbol — otherwise we have shipped a way to move a budget window.
*Alternative if refused:* the month-rollover and quiet-hours tests become L1-only (where the clock is already an injected input), and P2-10 / the rollover behaviour are **never proven against the real service**. That is a real loss; say so.

**OD-8 — GitLab test credentials.** The harness needs a root token and a `gonk` bot user on the e2e instance.
*Assumed default:* **generated at runtime, never committed.** `make gitlab-up` mints a root PAT via `gitlab-rails runner` inside the container, writes it to `$(TMPDIR)/gonk-e2e/<runid>/gitlab-root.token` (mode 0600, in a directory the harness creates and removes), and creates the `gonk` bot user + its PAT the same way. **Nothing under `test/` may ever contain a credential**; Task 9 adds a `make secrets-scan` gate that greps the tree for token-shaped strings and fails the build.

### Assumed defaults (build these)

- **AD-1 — The stub model is an OpenAI-compatible HTTP server, not a LiteLLM mock.** LiteLLM stays real at every layer above L1, because LiteLLM *is* the hard door and mocking it would mock away the thing we most need to prove.
- **AD-2 — The stub has a `record` mode.** Canned tool-call payloads must match opencode's real tool schema (Plan 04), which nobody can author from imagination. The stub can proxy once to a real upstream, capture a **cassette**, and replay it forever. Cassettes are committed, scrubbed of keys, and **recorded by a human out of band — never in a test run.**
- **AD-3 — A `defer` is a success, not a failure.** Several tests assert that the system *refuses to work*. The harness's vocabulary reflects that: `AssertDeferred(t, reason)` is as first-class as `AssertRan`.
- **AD-4 — No test asserts on model prose.** Task 4 proves this **mechanically**: the whole L1 suite runs twice with two different canned prose bodies (identical tool calls, identical usage) and the ledger and outcomes must be byte-identical.

---
## File structure

```
test/
  stubmodel/
    server.go        OpenAI-compatible deterministic stub: /v1/chat/completions, /v1/models
    script.go        Step / Match / Usage / Response — the per-test script
    record.go        record mode: proxy once to a real upstream, freeze a cassette
    log.go           the request log — independent ground truth for call count + usage
    server_test.go
    cassettes/       committed, scrubbed, human-recorded (AD-2)
  harness/
    runtime.go       container runtime detection; podman => --network=host
    doctor.go        `make e2e-doctor` — the preflight that refuses to waste your 25 minutes
    wait.go          WaitFor(predicate, deadline). THE ONLY WAY THE HARNESS WAITS.
    ports.go         fixed loopback port map + occupancy preflight
    creds.go         runtime-generated test credentials (never committed)
    clock.go         the testclock file seam (HB-3)
    session.go       SyntheticSession: /decide -> LLM calls -> /outcome, without opencode
    litellm.go       LiteLLM container fixture + the Date-rewriting skew proxy
    ledgerdb.go      Dolt | CNPG-Postgres container fixture (GONK_LEDGER)
    gitlab.go        gitlab-ce host-container fixture: boot, cache, per-run group
    cluster.go       kind | existing-kubeconfig; image sideload; namespace lifecycle
    kill.go          the kill-test primitives (Task 6)
  ledger/
    assert.go        three-way ledger assertion: stub log <-> LiteLLM spend <-> meter cost API
    money.go         ApproxUSD and friends — never == on a dollar
  corpus/
    gonkyml/*.yml    the hostile .gonk.yml corpus
    corpus.go        loader + the generated (not committed) oversize cases
    fuzz_test.go     FuzzGonkYMLNeverPanics — the regression net for Plan 01's .nan crash
  integration/       L1 — no build tag, runs in the standing gate
    main_test.go     the in-process world: real intake + real meter + fakes
    onboarding_test.go  scaffold_test.go  triage_test.go
    hostile_test.go  ladder_test.go  race_test.go  brick_test.go  prose_test.go
  component/         L2 — //go:build component
    litellm_test.go  ledger_test.go  harddoor_test.go  rollover_test.go  kill_test.go
  e2e/               L3 — //go:build e2e
    versions.env     pinned gitlab-ce / LiteLLM / kind image tags (OD-2, OD-6)
    e2e_test.go      chart install, idle-state, the spec-11 scenario
    gitlab_test.go   P2-1..P2-6: the things a fake GitLab cannot prove
    kill_test.go     pod-level kill matrix (spec 11.6)
    budget_test.go   the crown jewel: hard budgets, the race, the hard door, quiet hours
cmd/gonk-stubmodel/main.go
images/stubmodel/Dockerfile
Makefile                       e2e-doctor, integration, component, e2e, gitlab-up/down, secrets-scan
docs/adr/ADR-006-test-harness-layers-and-determinism.md
docs/spikes/litellm-verified.md      what Task 5 actually measured, against which version
```

---

### Task 1: `test/stubmodel` — the deterministic stub model server

**This is a deliverable, not a fixture.** It is the reason the whole suite can be deterministic, and it is the only task in this plan that depends on nothing else in the repo — **it can be built today, before Plans 02–05 land.**

**Files:** Create `test/stubmodel/script.go`, `test/stubmodel/log.go`, `test/stubmodel/server.go`, `test/stubmodel/server_test.go`, `cmd/gonk-stubmodel/main.go`, `images/stubmodel/Dockerfile`.

**What it must do, and why each part exists:**

| Capability | Why |
|---|---|
| OpenAI-compatible `POST /v1/chat/completions` and `GET /v1/models` | LiteLLM speaks OpenAI to its upstreams. This must be a drop-in `api_base`. |
| **Report exact `usage`** (`prompt_tokens`, `completion_tokens`) from the script | LiteLLM computes cost as `usage × configured price`. Controlling usage is how the harness controls **every dollar in the ledger** (determinism #2). |
| Canned responses, including **`tool_calls`** | An opencode agent that posts a triage comment does so by calling a tool. A canned tool call is how a test forces a *successful gate*; a canned response with **no** tool call is how it forces a **genuine gate failure** (HB-4). |
| Induced failures: HTTP status, hang, slow-drip, malformed body, mid-stream abort | The infra-failure half of the escalation ladder. **These must produce `infra-failed`, never an escalation.** |
| A **request log** with the full received body | Independent ground truth for *how many calls happened* and *what usage was reported* — the first leg of the three-way ledger check (Task 3). |
| Determinism: fixed `id`s, fixed `created`, no randomness, no time in the body | Byte-stable responses. Two runs produce identical bytes. |
| A control plane (`/_control/*`) | Scripts are set per test, not per process. One stub serves a whole suite. |
| Record mode | AD-2: nobody can author opencode's tool-call schema from imagination. |

- [ ] **Step 1: Write the failing test** — `test/stubmodel/server_test.go`

```go
package stubmodel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

func newStub(t *testing.T) (*stubmodel.Server, string) {
	t.Helper()
	s := stubmodel.New()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func chat(t *testing.T, base, model string, extra map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	body := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "triage this issue"}},
	}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// The whole point: the harness dictates the token counts, so it dictates the cost.
func TestReportsScriptedUsageExactly(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{
		Response: stubmodel.Response{Content: "ok"},
		Usage:    stubmodel.Usage{PromptTokens: 1000, CompletionTokens: 250},
		Repeat:   stubmodel.Forever,
	}})

	for range 3 {
		resp, out := chat(t, base, "stub-qwen", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		u := out["usage"].(map[string]any)
		if u["prompt_tokens"].(float64) != 1000 || u["completion_tokens"].(float64) != 250 ||
			u["total_tokens"].(float64) != 1250 {
			t.Fatalf("usage = %+v", u)
		}
	}
	if got := s.Log().Calls(); len(got) != 3 {
		t.Fatalf("log has %d calls, want 3", len(got))
	}
	if got := s.Log().TotalTokens(); got != 3750 {
		t.Fatalf("total tokens = %d, want 3750", got)
	}
}

// Two runs must produce byte-identical responses, or "deterministic" is a lie.
func TestResponsesAreByteStable(t *testing.T) {
	script := []stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 10, CompletionTokens: 2}, Repeat: stubmodel.Forever}}

	var bodies []string
	for range 2 {
		s, base := newStub(t)
		s.SetScript(script)
		_, out := chat(t, base, "stub-qwen", nil)
		b, _ := json.Marshal(out)
		bodies = append(bodies, string(b))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("responses differ across runs:\n%s\n%s", bodies[0], bodies[1])
	}
	if strings.Contains(bodies[0], time.Now().Format("2006")) {
		t.Fatal("response embeds the current time; it cannot be byte-stable")
	}
}

// A canned tool call is how a test forces a PASSING outcome gate.
func TestCannedToolCall(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{
		Response: stubmodel.Response{ToolCalls: []stubmodel.ToolCall{{
			ID: "call_1", Name: "gitlab_comment",
			Arguments: `{"body":"triaged: bug"}`,
		}}},
		Usage:  stubmodel.Usage{PromptTokens: 100, CompletionTokens: 20},
		Repeat: stubmodel.Forever,
	}})
	_, out := chat(t, base, "stub-qwen", nil)
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	tc := msg["tool_calls"].([]any)
	if len(tc) != 1 {
		t.Fatalf("tool_calls = %+v", tc)
	}
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
}

// The infra-failure half of the ladder. A 500 must NOT become a gate failure.
func TestInducedFailures(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{
		{Status: 500, Repeat: 2},
		{Status: 429, Repeat: 1},
		{Response: stubmodel.Response{Content: "ok"}, Usage: stubmodel.Usage{PromptTokens: 1, CompletionTokens: 1}, Repeat: stubmodel.Forever},
	})
	for _, want := range []int{500, 500, 429, 200} {
		resp, _ := chat(t, base, "stub-qwen", nil)
		if resp.StatusCode != want {
			t.Fatalf("status = %d, want %d", resp.StatusCode, want)
		}
	}
	// A failed call still appears in the log: it happened, even though it cost nothing.
	if got := len(s.Log().Calls()); got != 4 {
		t.Fatalf("log has %d calls, want 4", got)
	}
	// ...and it reported NO usage, so it must contribute NO tokens.
	if got := s.Log().TotalTokens(); got != 2 {
		t.Fatalf("failed calls contributed tokens: total = %d, want 2", got)
	}
}

func TestHangProducesClientTimeout(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{Hang: true, Repeat: stubmodel.Forever}})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions",
		strings.NewReader(`{"model":"stub-qwen","messages":[]}`))
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("want a timeout, got a response")
	}
	_ = s
}

// Steps can target a specific rung's model, so one script drives a whole ladder run.
func TestMatchOnModel(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{
		{Match: stubmodel.Match{Model: "stub-qwen"}, Status: 500, Repeat: stubmodel.Forever},
		{Match: stubmodel.Match{Model: "stub-glm"},
			Response: stubmodel.Response{Content: "ok"},
			Usage:    stubmodel.Usage{PromptTokens: 5, CompletionTokens: 5}, Repeat: stubmodel.Forever},
	})
	if r, _ := chat(t, base, "stub-qwen", nil); r.StatusCode != 500 {
		t.Fatalf("qwen status = %d", r.StatusCode)
	}
	if r, _ := chat(t, base, "stub-glm", nil); r.StatusCode != 200 {
		t.Fatalf("glm status = %d", r.StatusCode)
	}
}

// Running off the end of the script is a LOUD failure, not a silent default: a test
// that makes more calls than it scripted has stopped being deterministic.
func TestExhaustedScriptIs500AndRecorded(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 1, CompletionTokens: 1}, Repeat: 1}})
	_, _ = chat(t, base, "stub-qwen", nil)
	resp, out := chat(t, base, "stub-qwen", nil)
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(out["error"].(map[string]any)["message"].(string), "script exhausted") {
		t.Fatalf("error = %+v", out["error"])
	}
	if !s.Log().ScriptExhausted() {
		t.Fatal("Log().ScriptExhausted() must flag this — a suite must be able to fail on it")
	}
}

// LiteLLM may or may not forward `metadata` to its upstream (version-dependent).
// The stub records the WHOLE body so Task 5 can find out empirically rather than
// by reading release notes.
func TestLogCapturesFullRequestBody(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 1, CompletionTokens: 1}, Repeat: stubmodel.Forever}})
	chat(t, base, "stub-qwen", map[string]any{"metadata": map[string]string{"gonk_rung": "qwen-local"}})

	c := s.Log().Calls()[0]
	if !strings.Contains(string(c.RawBody), "gonk_rung") {
		t.Fatalf("raw body did not capture metadata: %s", c.RawBody)
	}
	if c.Metadata["gonk_rung"] != "qwen-local" {
		t.Fatalf("metadata = %+v (if this is empty against a REAL LiteLLM, it strips metadata — record that in Task 5)", c.Metadata)
	}
}

func TestModelsEndpoint(t *testing.T) {
	_, base := newStub(t)
	resp, err := http.Get(base + "/v1/models")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /v1/models = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestControlPlaneResetsScriptAndLog(t *testing.T) {
	s, base := newStub(t)
	s.SetScript([]stubmodel.Step{{Response: stubmodel.Response{Content: "ok"},
		Usage: stubmodel.Usage{PromptTokens: 1, CompletionTokens: 1}, Repeat: stubmodel.Forever}})
	chat(t, base, "stub-qwen", nil)

	req, _ := http.NewRequest("POST", base+"/_control/reset", nil)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 204 {
		t.Fatalf("reset = %v, %v", r, err)
	}
	if len(s.Log().Calls()) != 0 {
		t.Fatal("reset must clear the log")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./test/stubmodel/ -v`
Expected: FAIL — no Go files / undefined `stubmodel.New`.

- [ ] **Step 3: Implement `test/stubmodel/script.go`**

```go
// Package stubmodel is a deterministic, OpenAI-compatible model server. It is the
// foundation of gonk's e2e determinism (spec goal 7): every token count, every
// failure, and every tool call in a test comes from a script, not from a model.
//
// NOTHING IN THE GONK TEST SUITE MAY TALK TO A REAL MODEL. Determinism is the
// product here; "the model said something reasonable" is not an assertion.
package stubmodel

import "time"

// Forever makes a step serve every matching request for the rest of the script.
const Forever = -1

// Usage is what the stub REPORTS to LiteLLM. LiteLLM multiplies it by the price
// configured for the model, and that product is the row in the spend ledger. So
// this struct is, transitively, how a test decides what a thing costs.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string, as OpenAI sends it
}

// Response is the assistant turn. Content with no ToolCalls finishes "stop";
// ToolCalls finish "tool_calls".
//
// The distinction is load-bearing: a canned tool call is how a test forces the
// outcome gate to PASS, and a canned prose-only reply (the agent did nothing) is
// how it forces a GENUINE gate failure -- as opposed to an infra failure, which
// is Status/Hang below. The escalation ladder turns on exactly that difference.
type Response struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Match selects which requests a Step applies to. A zero Match matches anything.
type Match struct {
	Model    string `json:"model,omitempty"`    // exact model name as LiteLLM forwards it
	Contains string `json:"contains,omitempty"` // substring of the last user message
	Tag      string `json:"tag,omitempty"`      // "key=value" in the request's metadata, IF LiteLLM forwards it
}

// Step is one scripted behaviour.
type Step struct {
	Match Match `json:"match"`

	// Exactly one behaviour. Precedence: Hang > Status != 0 > RawBody != "" > Response.
	Hang     bool          `json:"hang,omitempty"`     // never respond; the client must time out
	Status   int           `json:"status,omitempty"`   // non-2xx; an INFRA failure
	RawBody  string        `json:"raw_body,omitempty"` // malformed/garbage body, served with 200
	Response Response      `json:"response,omitempty"`
	Usage    Usage         `json:"usage,omitempty"`
	Delay    time.Duration `json:"delay,omitempty"` // slow-drip; use with a client timeout

	// Repeat: 0 or 1 => serve once. Forever (-1) => serve every matching request.
	Repeat int `json:"repeat,omitempty"`

	served int // internal
}

func (s *Step) exhausted() bool {
	if s.Repeat == Forever {
		return false
	}
	n := s.Repeat
	if n == 0 {
		n = 1
	}
	return s.served >= n
}
```

- [ ] **Step 4: Implement `test/stubmodel/log.go`**

```go
package stubmodel

import (
	"sync"
	"time"
)

// Call is one request the stub received. This log is the harness's INDEPENDENT
// ground truth for how many model calls happened and what they consumed --
// the first leg of the three-way ledger check in test/ledger. If LiteLLM's spend
// log and this log disagree, a row was lost or double-counted, and that is the
// whole reason this type exists.
type Call struct {
	At       time.Time         // wall time, for lag measurement only -- NEVER asserted on
	Model    string
	Metadata map[string]string // present only if LiteLLM forwards metadata upstream
	RawBody  []byte            // the entire request, so Task 5 can discover what LiteLLM sends
	Status   int               // what we served
	Usage    Usage             // what we REPORTED (zero for a failure)
}

type Log struct {
	mu        sync.Mutex
	calls     []Call
	exhausted bool
}

func (l *Log) add(c Call) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
}

func (l *Log) markExhausted() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.exhausted = true
}

// ScriptExhausted reports whether any request ran off the end of the script. A
// suite MUST fail on this: a call the test did not script is a call the test does
// not control, and an uncontrolled call is a nondeterministic cost.
func (l *Log) ScriptExhausted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exhausted
}

func (l *Log) Calls() []Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Call, len(l.calls))
	copy(out, l.calls)
	return out
}

// TotalTokens counts only what was actually REPORTED: a failed call reports no
// usage and therefore costs nothing, which is exactly what LiteLLM will record.
func (l *Log) TotalTokens() int {
	var n int
	for _, c := range l.Calls() {
		n += c.Usage.PromptTokens + c.Usage.CompletionTokens
	}
	return n
}

// CallsWithTag filters by a forwarded metadata tag (see Call.Metadata).
func (l *Log) CallsWithTag(k, v string) []Call {
	var out []Call
	for _, c := range l.Calls() {
		if c.Metadata[k] == v {
			out = append(out, c)
		}
	}
	return out
}

func (l *Log) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls, l.exhausted = nil, false
}
```

- [ ] **Step 5: Implement `test/stubmodel/server.go`**

```go
package stubmodel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fixedCreated keeps every response byte-identical across runs. A wall-clock
// timestamp in the body would make "deterministic" a lie (see
// TestResponsesAreByteStable).
const fixedCreated int64 = 1752400000

// maxBody caps a request. The stub is only ever reachable from the test network,
// but a cap here is free and an unbounded read is never right.
const maxBody = 4 << 20

type Server struct {
	mu     sync.Mutex
	script []Step
	seq    int
	log    Log
}

func New() *Server { return &Server{} }

func (s *Server) SetScript(steps []Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = make([]Step, len(steps))
	copy(s.script, steps)
	s.seq = 0
}

func (s *Server) Log() *Log { return &s.log }

func (s *Server) Reset() {
	s.mu.Lock()
	s.script, s.seq = nil, 0
	s.mu.Unlock()
	s.log.reset()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/chat/completions":
		s.chat(w, r)
	case r.Method == "GET" && r.URL.Path == "/v1/models":
		writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "stub-qwen", "object": "model", "owned_by": "gonk-stub"},
			{"id": "stub-glm", "object": "model", "owned_by": "gonk-stub"},
			{"id": "stub-sonnet", "object": "model", "owned_by": "gonk-stub"},
		}})
	case r.Method == "POST" && r.URL.Path == "/_control/script":
		var steps []Step
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&steps); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.SetScript(steps)
		w.WriteHeader(204)
	case r.Method == "POST" && r.URL.Path == "/_control/reset":
		s.Reset()
		w.WriteHeader(204)
	case r.Method == "GET" && r.URL.Path == "/_control/requests":
		writeJSON(w, 200, map[string]any{
			"calls":            s.log.Calls(),
			"script_exhausted": s.log.ScriptExhausted(),
			"total_tokens":     s.log.TotalTokens(),
		})
	case r.URL.Path == "/_control/health" || r.URL.Path == "/health":
		w.WriteHeader(200)
	default:
		http.NotFound(w, r)
	}
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	// LiteLLM MAY forward this. Whether it does is version-dependent and is one of
	// the things Task 5 discovers empirically (P3-1). We record it either way.
	Metadata map[string]string `json:"metadata"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", 400)
		return
	}
	var req chatRequest
	_ = json.Unmarshal(raw, &req) // a malformed request is still a call; log it

	step := s.pick(req)
	if step == nil {
		s.log.markExhausted()
		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata, RawBody: raw, Status: 500})
		writeJSON(w, 500, map[string]any{"error": map[string]any{
			"message": "stubmodel: script exhausted — the test made a model call it did not script",
			"type":    "stub_error",
		}})
		return
	}

	if step.Delay > 0 {
		time.Sleep(step.Delay)
	}
	if step.Hang {
		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata, RawBody: raw, Status: 0})
		<-r.Context().Done() // the client's deadline is the control
		return
	}

	switch {
	case step.Status != 0 && (step.Status < 200 || step.Status >= 300):
		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata, RawBody: raw, Status: step.Status})
		writeJSON(w, step.Status, map[string]any{"error": map[string]any{
			"message": "stubmodel: induced failure", "type": "stub_induced", "code": strconv.Itoa(step.Status),
		}})
	case step.RawBody != "":
		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata, RawBody: raw, Status: 200})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, step.RawBody)
	default:
		s.mu.Lock()
		s.seq++
		id := fmt.Sprintf("chatcmpl-stub-%04d", s.seq)
		s.mu.Unlock()

		s.log.add(Call{At: time.Now(), Model: req.Model, Metadata: req.Metadata,
			RawBody: raw, Status: 200, Usage: step.Usage})
		writeJSON(w, 200, completion(id, req.Model, step.Response, step.Usage))
	}
}

// pick returns the first non-exhausted step that matches, and charges it a use.
func (s *Server) pick(req chatRequest) *Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.script {
		st := &s.script[i]
		if st.exhausted() || !matches(st.Match, req) {
			continue
		}
		st.served++
		cp := *st
		return &cp
	}
	return nil
}

func matches(m Match, req chatRequest) bool {
	if m.Model != "" && m.Model != req.Model {
		return false
	}
	if m.Contains != "" {
		var last string
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				last = msg.Content
			}
		}
		if !strings.Contains(last, m.Contains) {
			return false
		}
	}
	if m.Tag != "" {
		k, v, ok := strings.Cut(m.Tag, "=")
		if !ok || req.Metadata[k] != v {
			return false
		}
	}
	return true
}

func completion(id, model string, resp Response, u Usage) map[string]any {
	msg := map[string]any{"role": "assistant"}
	finish := "stop"
	if len(resp.ToolCalls) > 0 {
		finish = "tool_calls"
		tcs := make([]map[string]any, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			tcs = append(tcs, map[string]any{
				"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
			})
		}
		msg["tool_calls"] = tcs
		msg["content"] = nil
	} else {
		msg["content"] = resp.Content
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "created": fixedCreated, "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
			"total_tokens": u.PromptTokens + u.CompletionTokens,
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 6: Run the tests**

Run: `go test ./test/stubmodel/ -race -count=1 -v`
Expected: PASS (10 tests).

- [ ] **Step 7: Add `cmd/gonk-stubmodel/main.go` and its image**

```go
// Command gonk-stubmodel runs the deterministic stub model server as a container,
// for the component (L2) and e2e (L3) suites, where LiteLLM must reach it over the
// network. Tests drive it through /_control/script and /_control/requests.
//
// It has no auth: it is only ever reachable from a test network, it holds nothing,
// and it must never be deployed anywhere real. The image is tagged with the run id
// and is never pushed to a shared registry.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	flag.Parse()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           stubmodel.New(),
		ReadHeaderTimeout: 5 * time.Second,
		// NO WriteTimeout: Step.Hang deliberately never responds, and a server-side
		// write timeout would turn that into a server error instead of the CLIENT
		// timeout the infra-failure tests need.
	}
	slog.Info("gonk-stubmodel listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("stubmodel", "err", err)
		os.Exit(1)
	}
}
```

`images/stubmodel/Dockerfile`:

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gonk-stubmodel ./cmd/gonk-stubmodel

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gonk-stubmodel /gonk-stubmodel
EXPOSE 8081
ENTRYPOINT ["/gonk-stubmodel"]
```

- [ ] **Step 8: Write `test/stubmodel/record.go`** (AD-2 — do not skip; Task 7 depends on it)

A `-record <upstream-url>` flag on `cmd/gonk-stubmodel` that proxies each request to a real upstream, writes `{request, response}` pairs to `test/stubmodel/cassettes/<name>.json`, and **scrubs any `Authorization`/`api_key` field before writing**. `stubmodel.LoadCassette(name) []Step` turns one into a script.

> **Why:** the canned `tool_calls` a passing outcome gate needs must match **opencode's real tool schema**, which is defined by Plan 04 and cannot be invented here. A human records one real turn against bailey, reviews the cassette, and commits it. **Record mode never runs inside a test.** Add `TestCassetteScrubsCredentials` asserting a recorded cassette containing `"api_key":"sk-live-..."` is written with the value redacted.

- [ ] **Step 9: Commit**

```bash
gofmt -l . && go vet ./... && go test ./test/stubmodel/ -race -count=1 && golangci-lint run ./...
git add test/stubmodel cmd/gonk-stubmodel images/stubmodel
git commit -m "test(stubmodel): deterministic OpenAI-compatible stub model server with scripted usage and induced failures"
```

---
### Task 2: `test/harness` — runtime detection, the doctor, and the no-sleep waiter

**Files:** Create `test/harness/runtime.go`, `test/harness/doctor.go`, `test/harness/wait.go`, `test/harness/ports.go`, `test/harness/creds.go`, `test/harness/clock.go`, `test/harness/harness_test.go`, and the repo `Makefile`.

**This task exists to stop a future executor losing a day to podman.**

- [ ] **Step 1: Write the failing tests** — `test/harness/harness_test.go`

```go
package harness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/test/harness"
)

// WaitFor is the ONLY way anything in this harness waits. A sleep is a bug.
func TestWaitForSucceedsOnPredicate(t *testing.T) {
	start := time.Now()
	var n int
	err := harness.WaitFor(context.Background(), "counter reaches 3", 2*time.Second, func() (bool, error) {
		n++
		return n >= 3, nil
	})
	if err != nil {
		t.Fatalf("WaitFor = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("WaitFor slept instead of polling: took %s", time.Since(start))
	}
}

// The failure message must name the thing that never happened, or a 25-minute e2e
// failure is undiagnosable.
func TestWaitForTimesOutWithADescription(t *testing.T) {
	err := harness.WaitFor(context.Background(), "gitlab becomes ready", 100*time.Millisecond,
		func() (bool, error) { return false, nil })
	if err == nil {
		t.Fatal("want a timeout")
	}
	if !strings.Contains(err.Error(), "gitlab becomes ready") {
		t.Fatalf("useless timeout message: %v", err)
	}
	if !errors.Is(err, harness.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

// A predicate that errors hard (not "not yet") must abort immediately: waiting two
// more minutes on a 401 is a waste of everyone's time.
func TestWaitForAbortsOnFatalPredicateError(t *testing.T) {
	boom := errors.New("401 unauthorized")
	err := harness.WaitFor(context.Background(), "x", 5*time.Second, func() (bool, error) {
		return false, harness.Fatal(boom)
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fatal error", err)
	}
}

// Podman under WSL has broken CNI bridge networking. Every container run must
// carry --network=host, and the harness must decide that itself.
func TestRuntimeAddsHostNetworkForPodman(t *testing.T) {
	rt := harness.Runtime{Bin: "podman", Kind: harness.Podman}
	args := rt.RunArgs("gonk-stub", "gonk/stubmodel:test", nil, []string{"-addr", ":8081"})
	if !containsArg(args, "--network=host") {
		t.Fatalf("podman run must use host networking (broken CNI bridge): %v", args)
	}
	// ...and therefore must NOT publish ports: -p is meaningless (and an error)
	// on the host network.
	if containsArg(args, "-p") {
		t.Fatalf("--network=host and -p are mutually exclusive: %v", args)
	}
}

func TestRuntimePublishesPortsForDocker(t *testing.T) {
	rt := harness.Runtime{Bin: "docker", Kind: harness.Docker}
	args := rt.RunArgs("gonk-stub", "gonk/stubmodel:test", map[int]int{8081: 8081}, nil)
	if !containsArg(args, "-p") {
		t.Fatalf("docker should publish ports: %v", args)
	}
}

// The credential file must never be readable by anyone else, and must never live
// inside the repo. Both are cheap to get wrong and expensive to discover.
func TestCredsAreOutsideTheRepoAndMode0600(t *testing.T) {
	dir := t.TempDir()
	c, err := harness.NewCreds(dir, "run123")
	if err != nil {
		t.Fatalf("NewCreds = %v", err)
	}
	p, err := c.WriteSecret("gitlab-root.token", "glpat-"+strings.Repeat("x", 20))
	if err != nil {
		t.Fatalf("WriteSecret = %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	wd, _ := os.Getwd()
	if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("secret written INSIDE the repo tree: %s", p)
	}
}

// Generated, never fixed: a committed test token is a committed secret.
func TestGeneratedTokensAreRandom(t *testing.T) {
	a, _ := harness.RandomToken(32)
	b, _ := harness.RandomToken(32)
	if a == b {
		t.Fatal("RandomToken is not random")
	}
	if len(a) < 32 {
		t.Fatalf("token too short for ghook.MinSecretLen: %d", len(a))
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./test/harness/ -v` → FAIL, package does not exist.

- [ ] **Step 3: Implement `test/harness/wait.go`**

```go
// Package harness holds the shared fixtures for gonk's integration (L1),
// component (L2), and e2e (L3) suites.
//
// ONE RULE ABOVE ALL OTHERS: nothing here sleeps. Every wait is WaitFor on an
// observable predicate with a deadline. A time.Sleep in a distributed test is a
// flake generator, and a flaky budget test is worse than no budget test -- it
// trains people to ignore red.
package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrTimeout = errors.New("harness: timed out")

// pollInterval is short on purpose: these predicates are cheap local HTTP calls,
// and the cost of polling is far below the cost of a slow suite nobody runs.
const pollInterval = 100 * time.Millisecond

type fatalErr struct{ error }

// Fatal marks a predicate error as unrecoverable: WaitFor gives up immediately
// instead of retrying for the rest of the deadline. Use it for 401s, 400s, and
// anything else that will still be wrong in two minutes.
func Fatal(err error) error { return fatalErr{err} }

// WaitFor polls until cond returns true, cond returns a Fatal error, or the
// deadline passes. desc is what we were waiting FOR -- it goes in the timeout
// message, and a bad desc makes a 25-minute e2e failure undiagnosable.
func WaitFor(ctx context.Context, desc string, deadline time.Duration, cond func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var last error
	for {
		ok, err := cond()
		var fe fatalErr
		if errors.As(err, &fe) {
			return fmt.Errorf("harness: waiting for %s: %w", desc, fe.error)
		}
		if err != nil {
			last = err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w after %s waiting for %s (last error: %v)", ErrTimeout, deadline, desc, last)
			}
			return fmt.Errorf("%w after %s waiting for %s", ErrTimeout, deadline, desc)
		case <-time.After(pollInterval):
		}
	}
}
```

- [ ] **Step 4: Implement `test/harness/runtime.go`**

```go
package harness

import (
	"fmt"
	"os/exec"
	"strconv"
)

type RuntimeKind string

const (
	Podman RuntimeKind = "podman"
	Docker RuntimeKind = "docker"
)

type Runtime struct {
	Bin  string
	Kind RuntimeKind
	// HostNetwork forces --network=host. It is set automatically for podman,
	// because the dev box's podman-under-WSL has BROKEN CNI BRIDGE NETWORKING and
	// a container without host networking simply has no route to anything.
	HostNetwork bool
}

// DetectRuntime finds a container runtime and configures it for THIS box's
// reality. If `docker` is actually a podman shim (very common under WSL), we must
// still treat it as podman.
func DetectRuntime() (*Runtime, error) {
	for _, bin := range []string{"podman", "docker"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		out, err := exec.Command(path, "version", "--format", "{{.Client.Version}}").CombinedOutput()
		if err != nil {
			continue
		}
		kind := Docker
		if bin == "podman" || looksLikePodman(out) {
			kind = Podman
		}
		return &Runtime{Bin: path, Kind: kind, HostNetwork: kind == Podman}, nil
	}
	return nil, fmt.Errorf("harness: no container runtime found (tried podman, docker)")
}

func looksLikePodman(version []byte) bool {
	// `docker` on this box may be a podman shim. Ask podman-specific info.
	out, err := exec.Command("docker", "info", "--format", "{{.Host.BuildahVersion}}").CombinedOutput()
	return err == nil && len(out) > 1
}

// RunArgs builds a detached `run` invocation. Under host networking, port
// publishing is meaningless (and podman errors on it), so ports are IGNORED and
// the caller must simply use the container's own listen port on 127.0.0.1.
func (r Runtime) RunArgs(name, image string, ports map[int]int, cmd []string) []string {
	args := []string{"run", "-d", "--name", name}
	if r.HostNetwork {
		args = append(args, "--network=host")
	} else {
		for host, ctr := range ports {
			args = append(args, "-p", strconv.Itoa(host)+":"+strconv.Itoa(ctr))
		}
	}
	args = append(args, image)
	return append(args, cmd...)
}
```

- [ ] **Step 5: Implement `test/harness/ports.go`, `creds.go`, `clock.go`**

`ports.go` — a fixed loopback port map, because host networking gives us no choice, plus a preflight:

```go
package harness

// Fixed loopback ports. Under --network=host there is no port remapping to hide
// behind, so these are THE ports. Preflight refuses to run if any is occupied --
// a component silently talking to a leftover container from the last run is a
// debugging experience nobody should have.
const (
	PortStubModel = 8081
	PortLiteLLM   = 4000
	PortDolt      = 3307
	PortPostgres  = 5433
	PortMeter     = 9091
	PortIntake    = 9092
	PortGitLab    = 8929 // gitlab-ce, long-lived host container
	PortSkewProxy = 4001 // Date-rewriting proxy in front of LiteLLM (Task 5)
)

// CheckPortsFree returns an error naming EVERY occupied port, not just the first.
func CheckPortsFree(ports ...int) error { /* net.Listen on 127.0.0.1:p, close, collect */ }
```

`creds.go` — **no credential may ever enter the repo tree** (house rule; spec 9):

```go
package harness

// Creds owns a per-run directory OUTSIDE the repo (under os.TempDir()) holding
// every generated test credential: the GitLab root PAT, the bot PAT, the webhook
// token, the meter bearer token, the LiteLLM admin key. All are generated at
// runtime and removed at teardown.
//
// NOTHING UNDER test/ MAY EVER CONTAIN A CREDENTIAL. `make secrets-scan` (Task 9)
// greps the tree and fails the build if one appears.
type Creds struct{ dir string }

func NewCreds(baseDir, runID string) (*Creds, error)      // 0700 dir
func (c *Creds) WriteSecret(name, value string) (string, error) // 0600 file, returns path
func (c *Creds) Path(name string) string
func (c *Creds) Cleanup() error

// RandomToken returns n bytes of crypto/rand, base64url-encoded. Used for the
// webhook secret (>= ghook.MinSecretLen = 32) and the meter bearer token.
func RandomToken(n int) (string, error)
```

Every gonk service reads its secrets **from files with two rotation slots** (Plan 02 AD/Decision 12, Plan 03 Decision 12), so `Creds` writes files and the harness passes **paths**: `GONK_WEBHOOK_SECRET_FILE`, `GONK_METER_TOKEN_FILE`, `LITELLM_ADMIN_KEY_FILE`, `GONK_GITLAB_TOKEN_FILE`. **Never an env value.** A harness that passed a secret by env would be testing a shape we do not ship.

`clock.go` — the **HB-3** seam:

```go
package harness

// TestClock drives the `testclock` build of gonk-meter. The meter binary built
// with `-tags testclock` reads a monotonic offset (in seconds) from the file at
// $GONK_TESTCLOCK_FILE on every Now(); the production build has no such code path
// at all, and Task 9 asserts the production binary does not contain the symbol.
//
// This is how the harness crosses a month boundary, expires a reservation_ttl, and
// enters a quiet-hours window WITHOUT SLEEPING. There is no other honest way: a
// test that waits 60 minutes for a reservation to expire is a test nobody runs.
type TestClock struct{ path string }

func NewTestClock(c *Creds) (*TestClock, error) // writes "0"
func (tc *TestClock) Path() string
func (tc *TestClock) Advance(d time.Duration) error // offset += d; MONOTONE, never negative
func (tc *TestClock) SetOffset(d time.Duration) error
```

**`Advance` is monotone by construction.** Plan 03 Decision 6 says a backwards clock jump must never reset a project's spend; the harness must not be the thing that violates it. The one test that *does* need a backwards jump (`TestBackwardsClockDoesNotResetSpend`) uses `SetOffset` explicitly and is the only caller allowed to.

- [ ] **Step 6: Implement `test/harness/doctor.go` — the preflight**

`Doctor()` runs and prints a verdict, and `make e2e-doctor` calls it. It checks, in order:

1. A container runtime exists; which one; whether it is podman (⇒ host networking).
2. **Bridge networking works** — `<rt> network create gonk-doctor-<rand>` then run `alpine ip addr` on it and require a non-loopback IP. **If this fails, kind cannot work**, and the doctor says so, in one sentence, with the fix: `export GONK_E2E_KUBECONFIG=~/.kube/config` to run L3 against the existing k3s cluster instead.
3. `kind`, `kubectl`, `helm` on `PATH` (only needed for L3).
4. Free memory ≥ 8 GB (gitlab-ce alone wants ~4).
5. All fixed ports free (`CheckPortsFree`).
6. `GONK_LEDGER` is set to `dolt` or `postgres`, and the corresponding client is reachable if already running (**OD-5**).
7. Whether a warm gitlab-ce volume already exists (⇒ a ~12-minute run instead of ~25).

Output is a table of ✅/❌ and, on any ❌, a **non-zero exit and a specific remedy**. It never says "something went wrong".

- [ ] **Step 7: Create the `Makefile`**

```makefile
# The standing gate. CI has NEVER RUN on this repo -- every runner on
# gitlab.orac.local was offline through Plan 01 -- so THIS is the real gate, and
# every target below must be runnable on a laptop.
GO ?= go
RUNID ?= $(shell date +%s)
export GONK_RUNID = $(RUNID)

.PHONY: gate
gate: fmt vet test lint

fmt:  ; gofmt -l . | tee /dev/stderr | (! read)
vet:  ; $(GO) vet ./...
test: ; $(GO) test ./... -race -count=1          # L0 + L1. NO containers. < 90s.
lint: ; golangci-lint run ./...

.PHONY: integration
integration: ; $(GO) test ./test/integration/ -race -count=1 -v

.PHONY: e2e-doctor
e2e-doctor: ; $(GO) run ./test/harness/cmd/doctor

.PHONY: images
images:  ## ALWAYS rebuild. Never copy a file into a running container (house rule).
	$(GO) run ./test/harness/cmd/build-images -tag $(RUNID)

.PHONY: component
component: e2e-doctor images
	$(GO) test -tags component ./test/component/ -count=1 -timeout 20m -v

.PHONY: gitlab-up gitlab-down
gitlab-up:   ; $(GO) run ./test/harness/cmd/gitlab -up     # slow ONCE; cached after
gitlab-down: ; $(GO) run ./test/harness/cmd/gitlab -down   # deliberate; destroys the warm volume

.PHONY: e2e
e2e: e2e-doctor images
	$(GO) test -tags e2e ./test/e2e/ -count=1 -timeout 45m -v

.PHONY: secrets-scan
secrets-scan: ; $(GO) run ./test/harness/cmd/secrets-scan
```

- [ ] **Step 8: Run and commit**

```bash
go test ./test/harness/ -race -count=1 -v   # PASS
make e2e-doctor                             # prints a verdict; may legitimately FAIL on this box (that IS the finding)
gofmt -l . && go vet ./... && golangci-lint run ./...
git add test/harness Makefile
git commit -m "test(harness): container-runtime detection, e2e doctor, no-sleep waiter, runtime credentials"
```

> **Record the doctor's verdict on this box in the commit message.** If bridge networking is broken (it is expected to be), that is not a failure of this task — it is the fact that decides **OD-3**, and it must be written down where Task 7's executor will see it.

---

### Task 3: `test/corpus` (hostile `.gonk.yml`) and `test/ledger` (the three-way assertion)

**Files:** Create `test/corpus/gonkyml/*.yml`, `test/corpus/corpus.go`, `test/corpus/corpus_test.go`, `test/corpus/fuzz_test.go`, `test/ledger/money.go`, `test/ledger/assert.go`, `test/ledger/assert_test.go`.

**Why the corpus.** `.gonk.yml` is **attacker-controlled**: anyone with push access to any project the bot is invited to controls those bytes. Plan 01 shipped a **one-line remote crash** — `budget: { monthly_cost_usd: .nan }` nil-panicked the validator inside the process that enforces every project's budget and kill switch. That bug is fixed, but *the class of bug is not*, and there is no test in the repo whose job is to keep it fixed. This corpus is that test.

- [ ] **Step 1: Write the corpus** — one file per hostile input, `test/corpus/gonkyml/`

| File | The attack |
|---|---|
| `nan-cost.yml` | `budget: { monthly_cost_usd: .nan }` — **the Plan 01 crash. This is the regression test.** |
| `inf-cost.yml`, `neg-inf-cost.yml` | `.inf` / `-.inf` — the same upstream `big.Rat` nil |
| `nan-in-nested.yml` | a non-finite float somewhere `Validate`'s walk might not reach |
| `negative-budget.yml` | `monthly_cost_usd: -1` — a negative ceiling is not a small ceiling |
| `huge-tokens.yml` | `monthly_tokens: "999999999999G"` — integer overflow in the suffix grammar |
| `float-tokens.yml` | `monthly_tokens: 1.5` — the reason token quantities are strings |
| `billion-laughs.yml` | YAML anchor/alias expansion bomb — memory exhaustion |
| `deep-nesting.yml` | 10 000 nested maps — stack exhaustion in the decoder |
| `duplicate-keys.yml` | `enabled: true` twice — which one wins, and does anything crash |
| `ladder-injection.yml` | a rung name containing `\n`, `,`, and `\r` — **PLAN.md carry-forward 3: tag injection into a log line / CSV / header** |
| `project-nul.yml` | a NUL byte and invalid UTF-8 in a string field |
| `ladder-dup.yml` | `[glm, glm]` — the `uniqueItems` amendment |
| `ladder-reorder.yml` | `[sonnet, qwen-local]` — cloud rung first (Plan 03 AD-4, `enforce_ladder_order`) |
| `version-2.yml` | `version: 2` — an unknown schema version must not be accepted |
| `unknown-key.yml` | `bananas: true` — `additionalProperties: false` |
| `bad-timezone.yml` | `timezone: Mars/Olympus` |
| `bad-quiet-hours.yml` | `quiet_hours: "25:00-99:00"` |
| `empty.yml`, `not-yaml.yml`, `bom.yml`, `crlf.yml` | degenerate inputs: empty, `\x00\xff`, UTF-8 BOM, CRLF line endings |
| *(generated, not committed)* | a **64 MiB** `.gonk.yml` — asserts Plan 02's `GetRawFile` size cap fires **before** the parse, not after |

- [ ] **Step 2: Write the assertion** — `test/corpus/corpus_test.go`

```go
package corpus_test

// Every file in the corpus must be REJECTED, and rejection must be an ERROR --
// never a panic, never a crash, never a fallback to a permissive default.
//
// gonkcfg.Load is the only parser that may ever see these bytes (Plan 02's threat
// model). If a future change routes .gonk.yml through anything else, this test is
// where we find out.
func TestEveryHostileConfigIsRejectedAndNothingPanics(t *testing.T) {
	for _, c := range corpus.GonkYML(t) {
		t.Run(c.Name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC on hostile input %s: %v\n%s", c.Name, r, debug.Stack())
				}
			}()
			cfg, err := gonkcfg.Load(c.Bytes)
			if err == nil {
				t.Fatalf("hostile input ACCEPTED: %s -> %+v", c.Name, cfg)
			}
			// The error is echoed into a GitLab MR comment (Plan 02's 422 path), so
			// it must be a sane string, not a megabyte of YAML.
			if len(err.Error()) > 4096 {
				t.Fatalf("%s: error is %d bytes; it gets posted to GitLab", c.Name, len(err.Error()))
			}
		})
	}
}

// Bounded memory: a YAML bomb must not be able to OOM the process that enforces
// every project's budget.
func TestHostileConfigsDoNotExhaustMemory(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, c := range corpus.GonkYML(t) {
		_, _ = gonkcfg.Load(c.Bytes)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if grew := after.HeapAlloc - min(after.HeapAlloc, before.HeapAlloc); grew > 256<<20 {
		t.Fatalf("hostile corpus grew the heap by %d MiB", grew>>20)
	}
}
```

- [ ] **Step 3: Add the fuzz target** — `test/corpus/fuzz_test.go`

```go
// FuzzGonkYMLNeverPanics is the net under the whole class of bug that Plan 01
// shipped. The corpus is the seed set; the fuzzer finds the ones we did not think
// of. Load may return ANY error; it may never panic and it may never hang.
//
//	go test ./test/corpus/ -run Fuzz -fuzz FuzzGonkYMLNeverPanics -fuzztime 60s
//
// 60s in the standing gate is enough to catch a regression; a long soak is a
// deliberate act, not a merge gate.
func FuzzGonkYMLNeverPanics(f *testing.F) {
	for _, c := range corpus.GonkYML(f) {
		f.Add(c.Bytes)
	}
	f.Add([]byte("version: 1\nenabled: true\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<20 {
			t.Skip() // the size cap is enforced upstream, in glab.GetRawFile
		}
		_, _ = gonkcfg.Load(b)
	})
}
```

**Also add the same corpus to `pkg/opercfg`** once Plan 03 lands it: Plan 01's carry-forward says *nothing validates operator-supplied instance/group `Policy`* and Plan 03 Task 2 closes it — but the operator config is `gonk-city` content, not attacker content, so it gets the corpus for *robustness*, not for *security*. Distinguish those in the comment; do not overclaim.

- [ ] **Step 4: Implement `test/ledger/money.go`**

```go
package ledger

// USDEpsilon: cost is tokens x a configured per-token price, and neither the price
// nor the product is exactly representable in binary floating point. NEVER compare
// dollars with ==.
const USDEpsilon = 1e-9

func ApproxUSD(got, want float64) bool { return math.Abs(got-want) <= USDEpsilon }

// AssertUSD fails with BOTH numbers and the delta. "expected 0.025, got 0.025" is
// the worst possible test failure.
func AssertUSD(t testing.TB, what string, got, want float64) {
	t.Helper()
	if !ApproxUSD(got, want) {
		t.Fatalf("%s = %.12f, want %.12f (delta %.2e)", what, got, want, got-want)
	}
}
```

- [ ] **Step 5: Implement `test/ledger/assert.go` — the three-way check**

**This is the heart of requirement 4: assert on the ledger, not just on the happy path.**

Three independent views of the same spend, which must agree. If any two disagree, a row was **lost**, **double-counted**, or **misattributed** — and each of those is a distinct, named bug we would otherwise never see:

| View | Source | What it is ground truth for |
|---|---|---|
| **The stub's request log** | `stubmodel.Log()` | **How many model calls actually happened, and what usage each reported.** Nothing else in the system knows this independently. |
| **LiteLLM's spend log** | `GET /spend/logs` | What the **ledger** believes: one row per call, with the attribution `metadata` the pack stamped. |
| **meter's cost API** | `GET /v1/cost/bead/{id}`, `/cost/project/{p}` | What gonk **reports** — the join of spend → bead → GitLab artifact (spec 6.2.1). |

```go
package ledger

// Want is what a scenario claims the ledger should say. Every field is required:
// an assertion that omits `Trigger` is an assertion that would pass if attribution
// were broken, and pkg/atags exists precisely so that it cannot be.
type Want struct {
	Project    string
	Rig        string
	BeadID     string
	SessionKey string
	Rung       string
	Attempt    int
	Trigger    string // atags.TriggerIssueTriage | TriggerOnboarding | TriggerScaffold | TriggerMentionReply

	Calls            int     // model calls attributable to THIS (bead, attempt)
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64 // REAL dollars. Cloud rungs only.
	SyntheticUSD     float64 // local rungs. NOT SPEND. (Plan 03 Decision 9)
}

// Assert is the whole point of this package. It checks, in order:
//
//  1. STUB <-> LITELLM: the number of spend rows equals the number of successful
//     stub calls, and the token totals match. A mismatch means a lost or
//     double-counted row -- the single worst thing a billing ledger can do.
//  2. NO UNATTRIBUTED ROWS: every spend row's metadata parses via
//     atags.FromMetadata. A row LiteLLM recorded that gonk cannot attribute is
//     spend nobody is accountable for, and it must be impossible.
//  3. EXACT ATTRIBUTION: for each Want, exactly the rows with that
//     (project, rig, bead, session, rung, attempt, trigger) tuple exist, with
//     exactly those tokens and that cost. Not "at least" -- EXACTLY, so a
//     double-charge fails.
//  4. NO EXTRA ROWS: the union of the Wants accounts for EVERY row. An
//     unaccounted-for row is unmetered spend.
//  5. CURRENCY FIREWALL: a row on a LOCAL rung has CostUSD == 0 and its dollars
//     live in SyntheticUSD; a row on a CLOUD rung has SyntheticUSD == 0. If these
//     ever mix, the onboarding default (monthly_cost_usd: 0 + a local-only ladder)
//     can no longer afford its own only rung and every new project bricks itself.
//     See TestOnboardingDefaultCanAffordItsOnlyRung (Task 4).
//  6. METER AGREES: meter's cost API returns the same totals as the raw spend log,
//     per bead and per project.
//  7. SCRIPT NOT EXHAUSTED: the stub served no unscripted call. An unscripted call
//     is an uncontrolled cost.
func Assert(t testing.TB, v Views, want []Want)

// Views bundles the three sources. Each layer supplies them differently (L1 wires
// fakes; L2/L3 point at the real containers), but the ASSERTION IS THE SAME at
// every layer -- that is what makes a scenario portable up the stack.
type Views struct {
	Stub    *stubmodel.Log
	Spend   SpendSource   // GET /spend/logs (real LiteLLM) or the fake
	Cost    CostSource    // meter's /v1/cost/*
	Catalog map[string]string // rung -> "local" | "cloud", from pkg/opercfg
}

// AssertNoSpend is the fail-CLOSED assertion, and half the kill tests end in it:
// after a refusal, a defer, a deny, or a component being killed, there must be
// ZERO rows. Not "few". Zero.
func AssertNoSpend(t testing.TB, v Views)

// AssertAttempts checks the LADDER, from meter's own attempt history:
// the rungs, in order, with their outcomes. This is how "an infra failure never
// escalates" is asserted -- as data, not as a log line.
func AssertAttempts(t testing.TB, v Views, beadID string, want []Attempt)

type Attempt struct {
	N       int
	Rung    string
	Outcome string // success | gate-failed | infra-failed | aborted
}
```

- [ ] **Step 6: Test the assertion library itself** — `test/ledger/assert_test.go`

**An assertion library that cannot fail is worse than none.** Feed `Assert` deliberately-corrupted `Views` and require each one to be caught:

```go
func TestAssertCatchesADoubleCharge(t *testing.T)      // duplicate the spend row -> must FAIL
func TestAssertCatchesALostRow(t *testing.T)           // drop a spend row -> must FAIL
func TestAssertCatchesAnUnattributedRow(t *testing.T)  // strip gonk_bead_id -> must FAIL
func TestAssertCatchesMisattribution(t *testing.T)     // swap gonk_rung -> must FAIL
func TestAssertCatchesCurrencyMixing(t *testing.T)     // put real dollars on a local rung -> must FAIL
func TestAssertCatchesAnExhaustedScript(t *testing.T)  // an unscripted call -> must FAIL
func TestAssertNoSpendCatchesOneRow(t *testing.T)      // a single row -> must FAIL
```

Each uses a `testing.TB` recorder so a *failure* is the *pass*. This is the same idiom as Plan 03's saboteur suites, and for the same reason: without it, the ledger assertions could be quietly vacuous and every scenario below would be green and meaningless.

- [ ] **Step 7: Run and commit**

```bash
go test ./test/corpus/ ./test/ledger/ -race -count=1 -v
go test ./test/corpus/ -run Fuzz -fuzz FuzzGonkYMLNeverPanics -fuzztime 60s
gofmt -l . && go vet ./... && golangci-lint run ./...
git add test/corpus test/ledger
git commit -m "test: hostile .gonk.yml corpus with a fuzz target, and the three-way ledger assertion library"
```

---
### Task 4: L1 — the in-process integration suite (`test/integration`)

**No build tag. This runs in the standing gate, on every commit, with `-race`, in under a minute, with zero containers.** It is where most of the value is, and it must stay fast enough that nobody is tempted to skip it.

**Files:** Create `test/integration/main_test.go`, `onboarding_test.go`, `triage_test.go`, `hostile_test.go`, `ladder_test.go`, `race_test.go`, `brick_test.go`, `prose_test.go`; `test/harness/session.go`.

**The world it builds** (`main_test.go`): the **real** `pkg/intake` reconciler and HTTP server (Plan 02, Tasks 7/9/10) and the **real** `internal/meter/service` (Plan 03, Task 8), each on an `httptest` listener, talking to each other over **real HTTP through the real `meterapi` client** — that is what closes **P3-10** (Plan 02's tests all ran against a `fakeMeter`; this is the first time the real client speaks to the real server). Around them: `glabtest` (Plan 02 Task 2), a fake LiteLLM admin+spend API, the memory `Store`, and the stub model, in-process.

```go
// test/integration/main_test.go
package integration_test

// World is the whole system, minus the things that need a container. Everything
// here is REAL except GitLab, LiteLLM, and the model:
//
//	real: pkg/intake (reconciler, onboarding, dispatch), internal/meter/service
//	      (resolve, decide, outcome, reservations), pkg/rung, pkg/budget,
//	      pkg/opercfg, pkg/meterapi (over real HTTP), pkg/atags
//	fake: glabtest (GitLab), fakelitellm (admin API + /spend/logs), memory Store
//	stub: stubmodel
//
// The fake LiteLLM is the one uncomfortable substitution, and it is why Task 5
// exists: L1 proves the WIRING, L2 proves LITELLM ACTUALLY BEHAVES THAT WAY.
type World struct {
	GitLab *glabtest.Server
	Meter  *meterService     // real service, httptest listener
	Intake *intakeServer     // real service, httptest listener
	LLM    *fakelitellm.Server
	Model  *stubmodel.Server
	Clock  *harness.FakeClock
	Store  store.Store       // memory
	Views  ledger.Views
}

func NewWorld(t *testing.T, opts ...Option) *World
func (w *World) Reconcile(t *testing.T)                  // forces one intake pass (HB-1)
func (w *World) SyncSpend(t *testing.T)                  // forces one meter spend poll (HB-2)
func (w *World) Session(bead, sess, trigger string) *harness.SyntheticSession
```

**`harness.SyntheticSession` is the load-bearing abstraction of this plan.** It plays the pack's part (Plan 04) without opencode and without Kubernetes:

```go
// test/harness/session.go
//
// SyntheticSession reproduces EXACTLY the sequence the pack's dispatch formula and
// gate step perform (spec 6.2.3, Plan 03's contract):
//
//  1. POST /v1/policy/decide            -> run | defer | deny
//  2. on `run`: read key_ref, and make N model calls THROUGH LiteLLM, stamping the
//     returned `metadata` VERBATIM (never a metadata the test made up -- the whole
//     attribution chain depends on the pack not inventing tags)
//  3. POST /v1/policy/outcome           with the outcome the SCRIPT dictates
//
// It exists so the entire money path -- decide, reserve, spend, attribute, settle,
// escalate -- is assertable at L1 and L2, where a real opencode session is
// impossible. L3 replaces it with the real pack and asserts the SAME ledger.
//
// It NEVER sends an attempt count: meterapi.DecideRequest has no such field, and
// adding one is a forgery vector for climbing the ladder (Plan 03 Decision 2).
type SyntheticSession struct { ... }

func (s *SyntheticSession) Decide(t testing.TB) meterapi.DecideResponse
func (s *SyntheticSession) Call(t testing.TB, n int)                // n model calls at the decided rung
func (s *SyntheticSession) Report(t testing.TB, outcome string)     // success|gate-failed|infra-failed|aborted
func (s *SyntheticSession) Run(t testing.TB, calls int, outcome string) meterapi.DecideResponse // all three
```

- [ ] **Step 1: The seam + happy path** — `onboarding_test.go`, `triage_test.go`

```go
// The spec-11.2 walk, with fakes. Closes P3-10 and the intake<->meter half of P2-9.
func TestOnboardingToTriage(t *testing.T) {
	w := NewWorld(t)
	p := w.GitLab.AddProject("acme/widget", glab.AccessMaintainer) // bot invited, no .gonk.yml

	w.Reconcile(t)
	// 1. Bot is a member with no .gonk.yml -> a deterministic onboarding MR, ZERO tokens.
	mr := requireOneMR(t, w.GitLab, p.ID, "gonk/onboard")
	ledger.AssertNoSpend(t, w.Views) // spec 5.3: the onboarding MR is deterministic

	// 2. Merging it puts .gonk.yml on the default branch.
	w.GitLab.MergeMR(p.ID, mr.IID)
	w.Reconcile(t)

	// 3. Intake PUT the RAW bytes; meter resolved them. Intake never resolved.
	pr := w.Meter.GetProject(t, "acme/widget")
	if pr.State != "active" || !pr.Effective.Actions.Triage {
		t.Fatalf("project = %+v", pr)
	}
	// pending: .agent/ does not exist yet.
	requireIntakeState(t, w, "acme/widget", "pending")

	// 4. The scaffold MR is the FIRST METERED WORK (spec 5.3).
	w.Model.SetScript(scriptPostsScaffoldMR())
	sess := w.Session("gk-1", "sess-1", atags.TriggerScaffold)
	d := sess.Run(t, 2, "success")
	if d.Decision != "run" || d.Rung != "qwen-local" {
		t.Fatalf("scaffold decision = %+v", d)
	}
	w.SyncSpend(t)

	// 5. THE LEDGER SAYS EXACTLY THIS. Not "roughly". Exactly.
	ledger.Assert(t, w.Views, []ledger.Want{{
		Project: "acme/widget", Rig: "acme-widget", BeadID: "gk-1", SessionKey: "sess-1",
		Rung: "qwen-local", Attempt: 1, Trigger: atags.TriggerScaffold,
		Calls: 2, PromptTokens: 2000, CompletionTokens: 500,
		CostUSD: 0,            // local rung: ZERO real dollars, forever
		SyntheticUSD: 0.000625, // 2500 tokens x $0.25/1M
	}})
}

// P2-9's nastiest half: an operator kill switch must reach a project whose OWN
// config never changed -- which is exactly what meter's config_hash short-circuit
// could silently swallow.
func TestInstanceKillSwitchDisablesAProjectWithoutAConfigChange(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget")
	requireMeterState(t, w, "acme/widget", "active")

	w.Meter.SetOperatorConfig(t, opercfgWithInstanceEnabled(false)) // same .gonk.yml bytes!
	w.Reconcile(t)

	requireMeterState(t, w, "acme/widget", "disabled")
	requireIntakeState(t, w, "acme/widget", "disabled")
	if d := w.Session("gk-2", "sess-2", atags.TriggerIssueTriage).Decide(t); d.Decision != "deny" {
		t.Fatalf("a killed instance must DENY, got %+v", d)
	}
	ledger.AssertNoSpend(t, w.Views)
}
```

- [ ] **Step 2: The hostile corpus, end to end** — `hostile_test.go`

```go
// Every hostile .gonk.yml, pushed to a real project through the real intake, into
// the real meter. Requirements, all of them fail-CLOSED:
//   - meter answers 422 (the PROJECT's yaml is bad), never 400 (intake's bug),
//     never 500, never a panic;
//   - the project lands in `invalid`; effective is NULL (not a zero Effective);
//   - budget is ZERO, not null -- null means UNLIMITED and would be catastrophic here;
//   - NO virtual key exists (a broken config must not keep spending);
//   - NO order is fired and NO session may run;
//   - and BOTH PROCESSES ARE STILL UP afterwards.
func TestHostileConfigsFailClosedThroughTheWholeStack(t *testing.T) {
	for _, c := range corpus.GonkYML(t) {
		t.Run(c.Name, func(t *testing.T) {
			w := NewWorld(t)
			p := w.GitLab.AddProject("acme/"+c.Name, glab.AccessMaintainer)
			p.PutFile(".gonk.yml", c.Bytes)

			w.Reconcile(t) // must not panic, must not hang

			pr := w.Meter.GetProjectRaw(t, "acme/"+c.Name)
			if pr.Status != 422 {
				t.Fatalf("status = %d, want 422 (400 means we blamed intake for the project's bad yaml)", pr.Status)
			}
			if pr.Body.Effective != nil {
				t.Fatal("an invalid config has NO Effective (ADR-002), not a zero one")
			}
			if pr.Body.Budget.MonthlyCostUSD == nil {
				t.Fatal("null budget means UNLIMITED; an invalid project must get ZERO")
			}
			if w.LLM.HasKeyFor("acme/" + c.Name) {
				t.Fatal("an invalid project holds a live virtual key")
			}
			if d := w.Session("gk-h", "sess-h", atags.TriggerIssueTriage).Decide(t); d.Decision != "deny" {
				t.Fatalf("decision = %+v, want deny/invalid-config", d)
			}
			ledger.AssertNoSpend(t, w.Views)
			requireAlive(t, w) // /healthz on both, after every single hostile input
		})
	}
}

// PLAN.md carry-forward 3, proven at the boundary rather than asserted in a doc.
func TestAttributionUnsafeValuesNeverReachTheLedger(t *testing.T) {
	// A project whose ladder rung name contains a newline and a comma. Meter mints
	// the tags (it is the single charset boundary, Plan 03 Task 5), so meter must
	// refuse -- and no spend row may ever carry a value that would column-shift a
	// CSV or forge a log line.
}
```

- [ ] **Step 3: The escalation ladder** — `ladder_test.go`. **The settled, load-bearing decision: infra failures NEVER escalate a rung.**

```go
// Spec 6.3, and the single most important behavioural claim in this system after
// "budgets are hard". The rung index is the count of prior GATE failures. An infra
// failure retries the SAME rung, forever, up to max_infra_retries. If this ever
// breaks, a flaky network silently promotes every project to its most expensive
// model and the bill arrives at the end of the month.
func TestInfraFailuresNeverEscalate(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*World, *harness.SyntheticSession)
	}{
		{"litellm 500", func(w *World, _ *harness.SyntheticSession) { w.Model.SetScript(always(500)) }},
		{"litellm 429", func(w *World, _ *harness.SyntheticSession) { w.Model.SetScript(always(429)) }},
		{"model hangs / client timeout", func(w *World, _ *harness.SyntheticSession) { w.Model.SetScript(hangs()) }},
		{"malformed upstream body", func(w *World, _ *harness.SyntheticSession) { w.Model.SetScript(garbage()) }},
		{"session dies silently (reservation TTL expires)", func(w *World, s *harness.SyntheticSession) {
			s.Decide(t); /* no Report at all */ w.Clock.Advance(61 * time.Minute); w.Meter.RunJanitor(t)
		}},
		{"pod evicted (aborted)", func(w *World, s *harness.SyntheticSession) { s.Run(t, 1, "infra-failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWorld(t)
			onboard(t, w, "acme/widget", withLadder("qwen-local", "glm", "sonnet"))

			for range 4 { // four consecutive infra failures
				s := w.Session("gk-1", "sess-x", atags.TriggerIssueTriage)
				tc.fail(w, s)
				d := s.Decide(t)
				if d.Decision == "run" && d.Rung != "qwen-local" {
					t.Fatalf("ESCALATED ON AN INFRA FAILURE to %q -- this is the bug", d.Rung)
				}
			}
			// Every attempt at the cheapest rung. Not one climb.
			ledger.AssertAttempts(t, w.Views, "gk-1", []ledger.Attempt{
				{N: 1, Rung: "qwen-local", Outcome: "infra-failed"},
				{N: 2, Rung: "qwen-local", Outcome: "infra-failed"},
				{N: 3, Rung: "qwen-local", Outcome: "infra-failed"},
				{N: 4, Rung: "qwen-local", Outcome: "infra-failed"},
			})
			// And after max_infra_retries: DENY, not a silent climb to sonnet.
			if d := w.Session("gk-1", "sess-x", atags.TriggerIssueTriage).Decide(t); d.Decision != "deny" ||
				d.Reason != "infra-retries-exhausted" {
				t.Fatalf("decision = %+v, want deny/infra-retries-exhausted", d)
			}
		})
	}
}

// The mirror: a GENUINE outcome-gate failure escalates EXACTLY ONE rung. No LLM
// judged anything; the model succeeded and simply produced no artifact.
func TestGateFailureEscalatesExactlyOneRung(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local", "glm", "sonnet"), withBudget(10.0))

	// A 200 with prose and NO tool call: the session completed and posted nothing.
	// That is a gate failure, and it is the ONLY thing that buys an escalation.
	w.Model.SetScript(prosesOnly())
	w.Session("gk-1", "s1", atags.TriggerIssueTriage).Run(t, 1, "gate-failed")
	w.Session("gk-1", "s2", atags.TriggerIssueTriage).Run(t, 1, "gate-failed")
	d := w.Session("gk-1", "s3", atags.TriggerIssueTriage).Decide(t)
	if d.Rung != "sonnet" || d.Attempt != 3 {
		t.Fatalf("decision = %+v, want sonnet at attempt 3", d)
	}
}

// The one that catches the subtle bug: INTERLEAVED failures. gate, infra, gate.
// The rung index must be 2, not 3. An implementation that counts `attempt` instead
// of `gate failures` passes every test above and fails this one.
func TestInterleavedFailuresCountOnlyGateFailures(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local", "glm", "sonnet"), withBudget(10.0))

	w.Session("gk-1", "s1", atags.TriggerIssueTriage).Run(t, 1, "gate-failed")  // -> glm next
	w.Session("gk-1", "s2", atags.TriggerIssueTriage).Run(t, 1, "infra-failed") // -> still glm
	d := w.Session("gk-1", "s3", atags.TriggerIssueTriage).Decide(t)
	if d.Rung != "glm" {
		t.Fatalf("rung = %q after gate+infra, want glm (an infra failure must not advance the index)", d.Rung)
	}
	w.Session("gk-1", "s3", atags.TriggerIssueTriage).Report(t, "gate-failed") // -> sonnet
	if d := w.Session("gk-1", "s4", atags.TriggerIssueTriage).Decide(t); d.Rung != "sonnet" {
		t.Fatalf("rung = %q, want sonnet", d.Rung)
	}
	ledger.AssertAttempts(t, w.Views, "gk-1", []ledger.Attempt{
		{N: 1, Rung: "qwen-local", Outcome: "gate-failed"},
		{N: 2, Rung: "glm", Outcome: "infra-failed"},
		{N: 3, Rung: "glm", Outcome: "gate-failed"},
	})
}

// No LLM judges anywhere (a settled decision, spec 6.3 and the decision table).
// Mechanical proof: the gate's decision is a pure function of the outcome signal,
// so a model that returns pure garbage still produces the same ladder.
func TestGateNeverConsultsAModel(t *testing.T) { /* garbage prose + a valid tool call -> success */ }

// A caller must not be able to forge its way up the ladder (Plan 03 Decision 2).
func TestOutcomeBoundToAMeterMintedReservation(t *testing.T) {
	// POST /v1/policy/outcome with a made-up reservation_id -> 400, records NOTHING.
	// Replaying a real one overwrites; it does not append a second escalation.
}
```

- [ ] **Step 4: The concurrent-reservation race** — `race_test.go`. **The crown jewel.**

```go
// TWO SESSIONS RACING THE LAST OF A BUDGET. EXACTLY ONE MAY WIN.
//
// This is the property that makes a "hard budget" hard. It is also Plan 03's
// blocking spike (Task 0b) -- which proves ReserveIfFits in ISOLATION. This proves
// it END TO END, through the real /decide, which is the only thing a user of the
// system ever touches.
//
// At L1 the store is the memory store (trivially correct under its mutex), so this
// test proves the SERVICE does not have a check-then-act hole ABOVE the store. The
// same test body runs at L2 against the real backend (Task 5) and at L3 against the
// real cluster (Task 8). ONE SCENARIO, THREE LAYERS -- that is the design.
func TestConcurrentSessionsCannotOverspendACeiling(t *testing.T) {
	w := NewWorld(t)
	// Ceiling with headroom for EXACTLY TWO glm attempts (est_cost_usd 0.40 each).
	onboard(t, w, "acme/widget", withLadder("glm"), withBudget(1.00))

	const N = 32
	var wins atomic.Int64
	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			d := w.Session(fmt.Sprintf("gk-%d", i), fmt.Sprintf("s-%d", i), atags.TriggerIssueTriage).Decide(t)
			switch d.Decision {
			case "run":
				wins.Add(1)
			case "defer":
				if d.Reason != "monthly-cost-exhausted" {
					t.Errorf("loser deferred for the wrong reason: %q", d.Reason)
				}
			default:
				t.Errorf("unexpected decision %+v", d)
			}
		})
	}
	wg.Wait()

	if got := wins.Load(); got != 2 {
		t.Fatalf("%d of %d sessions won against headroom for 2 -- A BUDGET CEILING IS RACEABLE", got, N)
	}
	// The invariant, not the winner: the PERSISTED reservations never exceed the ceiling.
	if total := w.Store.OpenReservationCostUSD(t, "acme/widget"); total > 1.00+ledger.USDEpsilon {
		t.Fatalf("persisted reservations total $%.4f against a $1.00 ceiling: OVERSPEND", total)
	}
	// And the losers spent NOTHING. A defer that still burns tokens is not a defer.
	w.SyncSpend(t)
	if calls := len(w.Model.Log().Calls()); calls != 0 {
		t.Fatalf("%d model calls from sessions that never ran", calls)
	}
}
```

Run this one at `-count=10`: *a race that manifests one run in three is still a budget escape.*

- [ ] **Step 5: The brick test** — `brick_test.go`. **This is the regression the brief specifically asked for.**

```go
// THE ONBOARDING DEFAULT MUST BE ABLE TO AFFORD ITS OWN ONLY RUNG.
//
// The onboarding MR ships `budget: {monthly_cost_usd: 0}` with `ladder: [qwen-local]`
// (spec 5.3). Local rungs are priced SYNTHETICALLY in LiteLLM so the USD virtual-key
// ceiling is a hard door for every rung (Plan 03 Decision 9) -- but real dollars and
// synthetic dollars are TWO DIFFERENT CURRENCIES and rung.Decide's cost gate reads
// REAL dollars ONLY.
//
// IF ANYONE EVER UNIFIES THOSE TWO FIELDS, every freshly-onboarded project can no
// longer afford its only rung and BRICKS ITSELF ON ARRIVAL -- silently, at onboarding
// time, for every new project. This test is the wall in front of that cliff.
//
// It deliberately uses:
//   - the ACTUAL .gonk.yml the onboarding MR ships   (pkg/intake's render.go template)
//   - the ACTUAL default instance ladder             (Plan 05's chart values-e2e)
//   - the ACTUAL rung catalog                        (pkg/opercfg)
// Not a hand-written copy of any of them. A test that reproduces the template by
// hand cannot catch the template drifting.
func TestOnboardingDefaultCanAffordItsOnlyRung(t *testing.T) {
	w := NewWorld(t, withOperatorConfig(chartDefaultOperatorConfig(t))) // from the CHART
	p := w.GitLab.AddProject("acme/fresh", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", intake.DefaultGonkYML(t)) // the ACTUAL onboarding template

	w.Reconcile(t)
	requireMeterState(t, w, "acme/fresh", "active")

	d := w.Session("gk-1", "s1", atags.TriggerScaffold).Decide(t)
	if d.Decision != "run" {
		t.Fatalf("A FRESHLY ONBOARDED PROJECT CANNOT RUN: %+v\n"+
			"If reason is monthly-cost-exhausted, someone has let SYNTHETIC dollars "+
			"reach the REAL cost gate. See Plan 03 Decision 9.", d)
	}

	// ...and it STILL runs after burning an arbitrary pile of synthetic dollars,
	// because synthetic dollars are NOT SPEND.
	for i := range 50 {
		w.Session("gk-1", fmt.Sprintf("s-%d", i), atags.TriggerScaffold).Run(t, 5, "success")
	}
	w.SyncSpend(t)
	if d := w.Session("gk-1", "s-last", atags.TriggerScaffold).Decide(t); d.Decision != "run" {
		t.Fatalf("synthetic spend closed the REAL cost door: %+v", d)
	}
	// The ledger agrees: real spend is exactly zero, synthetic spend is not.
	c := w.Views.Cost.Project(t, "acme/fresh")
	ledger.AssertUSD(t, "real spend", c.CostUSD, 0)
	if c.SyntheticCostUSD <= 0 {
		t.Fatal("local spend was not recorded as synthetic at all — the token door has no teeth")
	}
}

// The saboteur, which is what makes the test above non-vacuous: if the cost gate
// summed the two currencies, the test above MUST go red.
func TestBrickTestWouldCatchCurrencyUnification(t *testing.T) { /* inject a summing budget.Remain; require FAIL */ }
```

- [ ] **Step 6: The prose-independence proof** — `prose_test.go` (**AD-4**)

```go
// "No test may depend on model output being good." Proven mechanically, not promised:
// run the whole L1 scenario set twice with DIFFERENT canned prose (identical tool
// calls, identical usage) and require the ledger and every outcome to be identical.
func TestNoAssertionDependsOnModelProse(t *testing.T) {
	a := runAllScenarios(t, withProse("I have triaged this issue."))
	b := runAllScenarios(t, withProse("znork blimp 42 ¡¡ <script>"))
	if diff := cmp.Diff(a, b); diff != "" {
		t.Fatalf("an assertion depends on model prose:\n%s", diff)
	}
}
```

- [ ] **Step 7: Run and commit**

```bash
go test ./test/integration/ -race -count=1 -v      # < 60s
go test ./test/integration/ -race -run Race -count=10
gofmt -l . && go vet ./... && golangci-lint run ./...
git add test/integration test/harness/session.go
git commit -m "test(integration): L1 in-process suite — seam, hostile corpus, ladder determinism, the reservation race, the brick test"
```

---

### Task 5: L2 — the component suite (`test/component`, real LiteLLM + real ledger)

**`//go:build component`. This is where every "we have never spoken to a real LiteLLM" question gets an answer.** It runs the **real `cmd/gonk-meter` binary** in a container against the **real LiteLLM** and the **real ledger backend**, with the stub model behind LiteLLM. No GitLab, no Kubernetes.

**Files:** Create `test/harness/litellm.go`, `test/harness/ledgerdb.go`, `test/component/litellm_test.go`, `ledger_test.go`, `harddoor_test.go`, `rollover_test.go`, `docs/spikes/litellm-verified.md`.

> **Podman note:** every container in this task runs with `--network=host` (Task 2's `Runtime`), so everything is on `127.0.0.1` at a fixed port and there is no bridge, no DNS, and nothing for the broken CNI to break. **L2 is expected to work on the dev box even though L3 may not.**

> **Ledger note (OD-5):** `GONK_LEDGER=dolt|postgres` selects the backend. **Plan 03's Task 0b decides which one is real.** Until one is retired, `make component` runs the suite **twice**, once per backend. The race test below is the one that matters, and it is the end-to-end form of Task 0b's spike.

- [ ] **Step 1: `test/harness/litellm.go` + `ledgerdb.go` — the fixtures**

```go
// LiteLLM runs as a real container from a PINNED tag (OD-6). It is configured to
// see EXACTLY ONE upstream: the stub model. A test that could reach a real model
// is not a deterministic test, and TestLiteLLMHasNoRealUpstream asserts it.
//
// The model list carries the prices that make every dollar in the ledger a
// function of the token counts the stub reports:
//
//	qwen-local  -> stub-qwen,   $0.25 / 1M in+out   SYNTHETIC (local, Decision 9)
//	glm         -> stub-glm,    $2.00 / 1M in+out   REAL      (cloud)
//	sonnet      -> stub-sonnet, $6.00 / 1M in+out   REAL      (cloud)
//
// These MUST equal the rung catalog's synthetic_usd_per_1m_tokens for the local
// rungs. TestSyntheticPricesAgree (P3-4) is the only place in the system that can
// check it -- they live in two different files, and nothing before Plan 06 reads both.
func StartLiteLLM(t testing.TB, rt *Runtime, c *Creds, stubURL string) *LiteLLM

// SkewProxy sits in front of LiteLLM's /spend/logs and REWRITES THE `Date` HEADER.
// Meter measures clock skew against that header (Plan 03 Decision 7), so this is how
// skew gets INJECTED deterministically instead of waited for.
func (l *LiteLLM) SkewProxy(t testing.TB, skew time.Duration) string

// LedgerDB starts Dolt (sql-server) or Postgres per GONK_LEDGER, applies meter's
// schema, and returns a DSN. It NEVER reuses a database across tests: a leaked
// reservation from a previous test is a false pass on a budget assertion.
func StartLedgerDB(t testing.TB, rt *Runtime, c *Creds) *LedgerDB
```

- [ ] **Step 2: The LiteLLM admin API — closes P3-1, P3-6, P3-8** — `litellm_test.go`

```go
//go:build component

// P3-1: NONE of meter's LiteLLM adapters (Plan 03 Task 7) has ever spoken to a real
// LiteLLM. They were written against the docs and tested against httptest. This is
// the first contact.
func TestLiteLLMAdminAPIShapes(t *testing.T) {
	// Drive the REAL internal/meter/litellm client against the REAL proxy:
	//   /key/generate  -> a usable key with a USD max_budget and budget_duration
	//   /key/update    -> raising and lowering max_budget takes effect
	//   /key/delete    -> the key stops working IMMEDIATELY (not eventually)
	//   /spend/logs    -> rows appear, carrying the metadata we stamped
	//   /model/info    -> the model list matches the rung catalog (P3-8; assert
	//                     gonk_meter_catalog_drift_total == 0)
	// FAILURE HERE IS A PLAN 03 BUG, NOT A HARNESS BUG. Fix internal/meter/litellm,
	// record the real shape in docs/spikes/litellm-verified.md, and amend Plan 03.
}

// P3-6 (Plan 03 OD-A): can a DEDICATED admin key do all of this, or are we forced
// onto the master key -- whose blast radius is the entire proxy?
func TestDedicatedAdminKeyCanPerformEveryAdminCall(t *testing.T) {
	// Generate a non-master admin key; repeat every call above with it.
	// If any call requires the master key, THAT IS THE FINDING. Record it, tell the
	// owner, and do not quietly switch the harness to the master key -- that would
	// hide a real security decision behind a green test.
}

// Determinism guard: LiteLLM must have exactly ONE upstream and it must be the stub.
func TestLiteLLMHasNoRealUpstream(t *testing.T) {
	// Every api_base in the rendered config points at 127.0.0.1:PortStubModel.
	// Nothing here may ever reach bailey or a cloud provider.
}

// Discovered empirically, because the answer is version-dependent and the docs are
// not trustworthy on it: does LiteLLM forward `metadata` to the upstream?
func TestWhetherLiteLLMForwardsMetadataUpstream(t *testing.T) {
	// Make one tagged call; read the stub's request log.
	// EITHER outcome is fine -- but the answer must be WRITTEN DOWN, because it
	// decides whether the stub's log can be used as an independent check on
	// ATTRIBUTION (it can always be used as a check on VOLUME).
	// Record in docs/spikes/litellm-verified.md.
}
```

- [ ] **Step 3: The hard door — closes P3-5 and half of P3-7** — `harddoor_test.go`

**This is the single most important test in the suite.** Everything else proves meter *refuses politely*. This proves that **even with meter completely bypassed, the money cannot escape.**

```go
//go:build component

// THE HARD DOOR. Meter is the soft door -- it refuses to START a session. LiteLLM's
// virtual-key USD ceiling is the HARD door: it refuses a request MID-FLIGHT, at the
// only place agent pods can reach a model (spec 9: NetworkPolicy egress to LiteLLM
// only). If the hard door does not close, a runaway session can spend past its
// ceiling and NOTHING will stop it.
//
// So this test DELIBERATELY BYPASSES METER: it takes the project's virtual key and
// hammers LiteLLM directly, past the ceiling. LiteLLM MUST refuse.
func TestVirtualKeyCeilingRefusesEvenWithMeterBypassed(t *testing.T) {
	w := startComponentWorld(t)
	key := w.Meter.ProvisionKeyFor(t, "acme/widget", meterapi.Budget{MonthlyCostUSD: ptr(1.00)})

	// $2.00/1M x 100k tokens = $0.20 per call. Five calls = $1.00. The sixth must die.
	spent := 0.0
	var refusedAt int
	for i := 1; i <= 12; i++ {
		w.Model.SetScript(oneCall(100_000, 0))
		status := w.CallLiteLLMDirectly(t, key, "glm") // NO meter, NO reservation
		if status == 200 {
			spent += 0.20
			continue
		}
		refusedAt = i
		break
	}
	if refusedAt == 0 {
		t.Fatalf("LITELLM NEVER REFUSED. The hard door does not exist. $%.2f spent "+
			"against a $1.00 ceiling. This is a budget escape and it is fatal.", spent)
	}
	if spent > 1.00+0.20 { // one call's overshoot is the documented, accepted slack
		t.Fatalf("overspent by $%.2f before the door closed", spent-1.00)
	}
	t.Logf("hard door closed at call %d, after $%.2f of a $1.00 ceiling", refusedAt, spent)
}

// P3-5 / Decision 9's WHOLE CLAIM, and nothing before this line has ever tested it:
// a LOCAL-ONLY project (monthly_cost_usd: 0, ladder: [qwen-local]) has its TOKEN
// budget converted into the USD ceiling on its virtual key -- via the SYNTHETIC
// price -- so the dollar door closes on a project that never spends a real dollar.
func TestUSDDoorClosesOnALocalOnlyProjectThatExhaustsItsTokenBudget(t *testing.T) {
	w := startComponentWorld(t)
	// monthly_cost_usd: 0, monthly_tokens: 1M, ladder: [qwen-local] @ $0.25/1M synthetic
	// => max_budget = 0 + 1M x $0.25/1M = $0.25
	key := w.Meter.ProvisionKeyFor(t, "acme/local", localOnlyBudget())
	if got := w.LiteLLM.KeyMaxBudget(t, key); !ledger.ApproxUSD(got, 0.25) {
		t.Fatalf("max_budget = %v, want 0.25 (the token ceiling, priced synthetically)", got)
	}
	// Burn 1M tokens directly against LiteLLM. The door must close.
	// If it does NOT, monthly_tokens has NO HARD ENFORCEMENT ANYWHERE and Decision 9
	// is a fiction. That is the finding, and it must be reported, not papered over.
}

// P3-4: the two files that must agree, and the only place anything reads both.
func TestSyntheticPricesAgreeBetweenTheRungCatalogAndLiteLLM(t *testing.T) {
	cat := opercfg.MustLoad(t, e2eOperatorConfig)
	info := w.LiteLLM.ModelInfo(t)
	for _, r := range cat.Rungs {
		if r.Kind != opercfg.KindLocal {
			continue
		}
		want := r.SyntheticUSDPer1MTokens / 1e6
		got := info[r.Model].InputCostPerToken
		if !ledger.ApproxUSD(got, want) {
			t.Fatalf("rung %q: catalog says $%g/token, LiteLLM says $%g/token. "+
				"The hard door is set to a DIFFERENT ceiling than meter thinks. "+
				"These live in two different files and NOTHING ELSE CHECKS THEM.", r.Name, want, got)
		}
	}
}
```

- [ ] **Step 4: Time, lag, and the month boundary — closes P3-2, P3-3** — `rollover_test.go`

```go
//go:build component

// P3-3: MEASURE the real spend-log lag. Plan 03's max_spend_staleness (default 5m)
// is a GUESS, and if it is below the real lag the factory stalls permanently while
// still believing it is being careful.
func TestMeasureLiteLLMSpendLogLag(t *testing.T) {
	// Make a call at T. Poll /spend/logs until the row appears at T+lag.
	// Repeat 20x; report min/median/p95/max; FAIL if p95 > max_spend_staleness.
	// WRITE THE NUMBER into docs/spikes/litellm-verified.md -- it is the input to a
	// production config value, and a number nobody wrote down is a number nobody
	// will believe in six months.
}

// P3-2, and Plan 03 says outright this "is a real defect" if it goes the wrong way.
// Meter's budget month is the UTC CALENDAR month (Decision 6). LiteLLM's virtual key
// uses budget_duration: "1mo" (AD-9). ARE THOSE THE SAME BOUNDARY?
//
// If LiteLLM's "1mo" is a ROLLING 30 DAYS, then the soft door and the hard door reset
// on DIFFERENT DAYS, and every month there is a window in which LiteLLM refuses work
// meter thinks is affordable -- or, far worse, PERMITS work meter has already stopped
// counting.
func TestLiteLLMBudgetDurationIsACalendarMonth(t *testing.T) {
	// Provision a key with budget_duration "1mo" near a month boundary; read back
	// budget_reset_at from /key/info; compare to meter's spend.Window().End.
	// If they differ by more than a minute, FAIL LOUDLY and record the delta.
	//
	// If it IS a rolling 30 days, DO NOT SILENTLY ADJUST THE HARNESS TO MATCH. That
	// is a product defect: report it, and the fix (in Plan 03) is either to reset the
	// key explicitly on rollover or to move meter's window. The harness's job here is
	// to find it, not to accommodate it.
}

// Decision 6 + the harness's monotone clock: a backwards jump (NTP correction, VM
// restore) must NEVER reset a project's spend, because an early rollover is a free
// budget.
func TestBackwardsClockDoesNotResetSpend(t *testing.T) {
	// Spend to 90% of the ceiling; SetOffset backwards by 40 days; re-decide.
	// The window must NOT move and the spend must NOT reset.
}

// Decision 7: skew is a HEALTH problem, not a math problem. Injected by the
// Date-rewriting proxy -- never by touching the system clock.
func TestClockSkewMakesEveryBudgetedProjectDefer(t *testing.T) {
	// Point meter at SkewProxy(+10m) (> max_clock_skew 5m):
	//   /readyz         -> not ready
	//   /decide         -> defer, reason spend-data-stale, for any FINITE ceiling
	//   an UNLIMITED project -> still runs (it has no budget to protect)
	// AssertNoSpend throughout.
}

// A July row that lands on 1 August is charged to JULY. Getting this wrong hands
// every project a free budget on the 1st of the month.
func TestLateRowIsWindowedByItsOwnTimestamp(t *testing.T) { /* clock across the boundary, then sync */ }
```

- [ ] **Step 5: The reservation race against the REAL store — closes P3-9** — `ledger_test.go`

```go
//go:build component

// The end-to-end form of Plan 03's Task 0b. Task 0b proved ReserveIfFits in
// ISOLATION, against the store. This proves the SAME PROPERTY through the real
// /decide, in the real meter binary, against the real ledger -- which is the only
// configuration that ships.
//
// RUNS AGAINST WHICHEVER BACKEND IS IN FORCE (GONK_LEDGER=dolt|postgres). Both are
// supported; the Postgres/CNPG fallback is owner-approved. Do not assume Dolt.
func TestConcurrentDecidesCannotOverspend_RealStore(t *testing.T) {
	// Byte-for-byte the same scenario as L1's race test, one layer down.
	// 32 concurrent /decide calls; headroom for exactly 2; exactly 2 `run`.
	// -count=5 minimum. A serialization failure in the store is a LEGITIMATE `defer`,
	// not an error -- a lost race is a lost race.
}

// The nastiest version, and the one that finds the bugs a single-process test cannot:
// KILL METER MID-RACE. Reservations are durable (Decision 10) precisely so that this
// cannot open a hole.
func TestMeterRestartMidRaceDoesNotOpenHeadroom(t *testing.T) {
	// Start 32 concurrent decides; SIGKILL meter at ~50%; restart it; let the rest run.
	// STILL exactly 2 winners in total. If a restart resurrects headroom, reservations
	// are not durable and the "meter restart" row of Plan 03's failure matrix is wrong.
}

// If meter runs with >1 replica, the in-process keyedMutex does NOTHING and the
// ceiling is defended by the STORE ALONE (Plan 03 AD-10). Plan 05 ships replicas: 1
// on that basis. THIS TEST IS WHAT WOULD LET US LIFT THAT RESTRICTION -- and until it
// passes, it is the evidence that we must not.
func TestTwoMeterReplicasCannotOverspend(t *testing.T) {
	// Two meter processes, one ledger, one project, N concurrent decides.
	// If this FAILS: that is the EXPECTED result under the recommended fallback, and
	// it must be recorded as such -- t.Skip with the reason if AD-10 is in force, and
	// a link to Task 0b's spike doc. DO NOT DELETE THE TEST: it is the acceptance
	// criterion for ever running meter with two replicas.
}
```

- [ ] **Step 6: Write `docs/spikes/litellm-verified.md`**

The version pinned; what was verified against it; **the measured spend-log lag (min/median/p95/max)**; the `budget_duration: "1mo"` boundary result; whether metadata is forwarded upstream; whether a dedicated admin key suffices; and the exact request/response shapes that differed from what Plan 03 assumed. **A LiteLLM version bump invalidates this document and re-runs this task.**

- [ ] **Step 7: Run and commit**

```bash
make e2e-doctor
GONK_LEDGER=dolt     go test -tags component ./test/component/ -count=1 -timeout 20m -v
GONK_LEDGER=postgres go test -tags component ./test/component/ -count=1 -timeout 20m -v
go test -tags component ./test/component/ -run Race -count=5
git add test/component test/harness/litellm.go test/harness/ledgerdb.go docs/spikes/litellm-verified.md
git commit -m "test(component): L2 — real LiteLLM + real ledger; hard door, spend lag, month boundary, reservation race"
```

---
### Task 6: The kill-test framework, and the L2 kill matrix

**Files:** Create `test/harness/kill.go`, `test/component/kill_test.go`.

**The claim being tested is not "it recovers". It is "it fails CLOSED."** Recovery is nice. What is *required* is that killing any component mid-flight produces **none** of these:

| The five forbidden outcomes | What it would mean |
|---|---|
| **Unmetered spend** | A model call happened that no ledger row accounts for. Money left the building unobserved. |
| **Double-charge** | One model call, two ledger rows — or one attempt, two reservations. The bill is wrong and the budget closes early. |
| **A lost bead** | Work vanished. A user filed an issue and nothing will ever happen. |
| **A stuck-forever bead** | Work parked in `waiting-for-capacity` with no `retry_after` that will ever arrive. Silently dead. |
| **A budget escape** | Spend exceeded a ceiling, or a killed component resurrected headroom that was already committed. |
| *(and the ladder's own version)* **A free escalation** | An infra failure bought a climb to a more expensive rung. The system pays for its own flakiness. |

`test/harness/kill.go` gives every layer the same vocabulary:

```go
// Kill is a HARD kill: SIGKILL to a process, `podman kill -s KILL` to a container,
// `kubectl delete pod --grace-period=0 --force` to a pod. NEVER a graceful shutdown.
//
// A graceful shutdown tests the shutdown path. A SIGKILL tests the INVARIANTS, and
// the invariants are what we actually promise. Real components die hard: OOM kills,
// node evictions, and `kubectl delete` do not send SIGTERM and wait politely.
func Kill(t testing.TB, target Target)
func Restart(t testing.TB, target Target)  // Kill + start + WaitFor(ready)
func Pause(t testing.TB, target Target) func()  // pause/unpause: a NETWORK PARTITION, not a crash

// AssertFailsClosed is the shared post-condition of EVERY kill test. It is the whole
// point of the framework, and every kill test ends in it.
func AssertFailsClosed(t testing.TB, v ledger.Views, want FailClosed)

type FailClosed struct {
	NoUnmeteredSpend bool // every stub call has exactly one attributed ledger row
	NoDoubleCharge   bool // no ledger row appears twice; no (bead,attempt) has two reservations
	NoLostBead       bool // the bead still exists and is either done or retryable
	NoStuckBead      bool // if parked, it carries a retry_after in the FUTURE but not the far future
	NoBudgetEscape   bool // observed + reserved <= ceiling, at every instant
	NoFreeEscalation bool // the rung index still equals the count of GATE failures
}
```

- [ ] **Step 1: Write the L2 kill matrix** — `test/component/kill_test.go`

Each row is a `t.Run`, each kills one thing at one moment, each ends in `AssertFailsClosed`.

| # | Kill | At this moment | Required outcome |
|---|---|---|---|
| **K1** | **meter** (SIGKILL) | after `ReserveIfFits` wrote the reservation, **before** the `/decide` response reached the caller | The reservation is **durable**. The caller retries, gets a *fresh* decision, and the total reservation for that bead is **one**, not two. **No double-charge, no headroom resurrection.** |
| **K2** | **meter** | after a session started, **before** `/outcome` | On restart: `/readyz` false until the first spend sync; the reservation is still held; when its TTL expires the janitor records **`infra-failed`** → **same rung, no escalation**. |
| **K3** | **meter** | mid-race (32 concurrent decides) | Still **exactly K winners in total**. (This is `TestMeterRestartMidRaceDoesNotOpenHeadroom` from Task 5, re-asserted through the kill framework.) |
| **K4** | **LiteLLM** (container kill) | mid-session, model calls in flight | The session sees a connection error → **`infra-failed`** → **same rung**. And while LiteLLM is down, meter's spend snapshot goes stale → **every budgeted project defers** → **no unmetered spend**. Never `run` on numbers we know are wrong. |
| **K5** | **LiteLLM** | between a model call and meter's spend sync | The call **still** lands in the ledger once LiteLLM returns (its own store persisted it). **Not lost, not doubled.** If LiteLLM *does* lose the row, that is a finding: record it and raise `max_spend_staleness` is **not** the fix — reservations are. |
| **K6** | **the ledger** (Dolt/Postgres) | mid-`ReserveIfFits` | Meter `/readyz` false; `/decide` **fails closed** (defer/5xx → the pack retries); **no session spawns**. Half-written reservations do not exist (that is what the transaction is for). |
| **K7** | **the ledger** | between a reservation and its settlement | On recovery the reservation is **still open** and still holding budget. It expires normally → `infra-failed` → same rung. |
| **K8** | **the stub model** | mid-request (hang, then kill) | LiteLLM returns 5xx → **`infra-failed`** → **same rung**. The failed call contributes **zero** tokens to the ledger. |
| **K9** | **the network between meter and LiteLLM** (`Pause`, a partition — *not* a crash) | during key provisioning | The project sits in **`key-missing`** → `/decide` **defers** (`virtual-key-missing`) → **no key, no session, no spend**. The reconcile loop repairs it when the partition heals. |
| **K10** | **meter** | **between provisioning the LiteLLM key and writing it to the store** | The nastiest one: an **orphan key** with a budget exists in LiteLLM that meter does not know about. Required: re-provisioning is **idempotent on the key alias** (`keysink.Slug`), so the repair **adopts or replaces** the orphan rather than creating a second key with a second ceiling. **Two keys for one project = two ceilings = a doubled budget.** |

```go
// The shape every row takes. Note that the assertion is the SAME for all of them --
// that is deliberate: a kill test that needs a bespoke assertion is usually a kill
// test that has quietly lowered its standards.
func TestKillMatrix(t *testing.T) {
	for _, tc := range killMatrix {
		t.Run(tc.name, func(t *testing.T) {
			w := startComponentWorld(t)
			onboard(t, w, "acme/widget")

			tc.arrange(t, w)          // get the system to the exact moment
			harness.Kill(t, tc.target) // SIGKILL. No mercy.
			tc.recover(t, w)          // restart / heal / advance the clock

			harness.AssertFailsClosed(t, w.Views, harness.FailClosed{
				NoUnmeteredSpend: true, NoDoubleCharge: true, NoLostBead: true,
				NoStuckBead: true, NoBudgetEscape: true, NoFreeEscalation: true,
			})
		})
	}
}
```

- [ ] **Step 2: Prove the kill framework can fail** (same idiom as the ledger saboteurs)

```go
// A kill framework that cannot detect a violation is decoration. Inject each of the
// five forbidden outcomes and require AssertFailsClosed to catch it.
func TestAssertFailsClosedCatchesEachViolation(t *testing.T) {
	// duplicate a reservation      -> NoDoubleCharge must FAIL
	// add a spend row with no bead -> NoUnmeteredSpend must FAIL
	// park a bead with retry_after in the year 2099 -> NoStuckBead must FAIL
	// bump the rung index after an infra failure    -> NoFreeEscalation must FAIL
	// push observed+reserved past the ceiling       -> NoBudgetEscape must FAIL
}
```

- [ ] **Step 3: Run and commit**

```bash
GONK_LEDGER=$GONK_LEDGER go test -tags component ./test/component/ -run 'Kill|FailsClosed' -count=3 -timeout 25m -v
git add test/harness/kill.go test/component/kill_test.go
git commit -m "test(component): kill-test framework and the L2 kill matrix — every component dies hard, the system fails closed"
```

---

### Task 7: L3 — the full-stack e2e (`test/e2e`): cluster + gitlab-ce + chart

**`//go:build e2e`. This is the expensive layer. Read the runtime table and the podman section before you start.**

**Files:** Create `test/harness/gitlab.go`, `test/harness/cluster.go`, `test/e2e/versions.env`, `test/e2e/e2e_test.go`, `test/e2e/gitlab_test.go`.

> **BEFORE YOU START:** run `make e2e-doctor`. **Do not try to use kind — it is not installed, and podman's CNI bridge (which kind requires and no flag fixes) is broken.** L3 runs against the **real orac cluster** in a throwaway namespace `gonk-e2e-<runid>` (**OD-3**, answered by `docs/environment.md`). The doctor's first job is to tell you **which cluster you are pointed at**, and to refuse if it is not the one you meant.
>
> **The three things you must not do on a live cluster, and which Step 2 enforces:**
> 1. **Do not touch anything outside `gonk-e2e-<runid>`.**
> 2. **Do not point at the production LiteLLM** (`litellm.litellm.svc.cluster.local:4000`). It routes local models to a **real Ollama on a real GPU** and writes to the owner's **real spend ledger**. The harness deploys its **own** LiteLLM, upstream = the stub, and asserts it.
> 3. **Do not take tenancy on `databases-app/postgres`** (the owner's real database). L3's ledger is **in-namespace**: bundled Dolt, or `postgres.mode: cnpg`. **Never `mode: shared`.**

- [ ] **Step 1: `test/harness/gitlab.go` — the cached gitlab-ce fixture**

```go
// GitLab is a LONG-LIVED HOST CONTAINER, not a chart component and not a kind
// workload. It is a TEST FIXTURE, and treating it as one is what makes L3 runnable
// more than once a day:
//
//   - `make gitlab-up` boots gitlab/gitlab-ce:<pin> into a NAMED PERSISTENT VOLUME.
//     COLD: 4-8 MINUTES (gitlab-ctl reconfigure). WARM: 60-180s. It wants ~4GB RAM.
//   - It then STAYS UP across many e2e runs. `make gitlab-down` is a deliberate act.
//   - --network=host (podman's CNI bridge is broken), so it is 127.0.0.1:8929 and the
//     CLUSTER reaches it via the HOST IP, captured at preflight and passed as a chart
//     value. This is also why gitlab is not IN the cluster: it does not need to be, and
//     putting it there would make every one of these problems worse.
//
// EACH RUN CREATES A FRESH GROUP `gonk-e2e-<runid>` AND TOUCHES NOTHING ELSE. No test
// may assume a clean instance, and no test may delete anything it did not create. That
// is the single decision that lets a 25-minute suite be re-run on a warm instance in 12.
func StartGitLab(t testing.TB, rt *Runtime, c *Creds) *GitLab

// Credentials are MINTED AT RUNTIME via `gitlab-rails runner` inside the container and
// written to the Creds dir (0600, outside the repo). NOTHING IS EVER COMMITTED (OD-8).
func (g *GitLab) RootToken(t testing.TB) string
func (g *GitLab) BotUser(t testing.TB) (user glab.User, token string) // the `gonk` bot
func (g *GitLab) NewGroup(t testing.TB, runID string) *Group

// AssertVersionMatchesPin: the pin is gitlab/gitlab-ce:18.10.1 -- the version
// gitlab.orac.local actually runs, and it is COMMUNITY EDITION (OD-2, answered by
// docs/environment.md). A golden payload is valid for ONE version, so this asserts
// the fixture's /api/v4/version against test/e2e/versions.env AND fails if
// `enterprise` is true: an EE instance would let a test take a path a CE user
// cannot (group webhooks are Premium; multiple MR assignees are EE).
func (g *GitLab) AssertVersionMatchesPin(t testing.TB)
```

`test/e2e/versions.env`:

```sh
# The version gitlab.orac.local ACTUALLY RUNS (docs/environment.md, 2026-07-13).
# The golden webhook payloads in pkg/ghook/testdata are valid for THIS VERSION AND
# NO OTHER. On a GitLab upgrade: bump this, re-run
# TestGoldenWebhookPayloadsMatchRealGitLab, and READ THE DIFF (spec 12.6).
GITLAB_IMAGE=gitlab/gitlab-ce:18.10.1
GITLAB_EDITION=ce            # asserted: enterprise=false. Nothing may need EE.
LITELLM_IMAGE=              # OD-6: the owner must name the tag they will deploy
```

- [ ] **Step 2: `test/harness/cluster.go` — cluster-agnostic by construction**

```go
// Cluster is A KUBECONFIG AND A NAMESPACE.
//
// DEFAULT AND DOCUMENTED PATH: the REAL orac cluster, in a throwaway namespace
// `gonk-e2e-<runid>`, created here and DELETED in t.Cleanup. kind is NOT installed
// on this box, and kind's node containers require a working CNI bridge that podman
// here does not have -- so kind is behind GONK_E2E_PROVIDER=kind and exists only so
// the harness is portable. (docs/environment.md; OD-3.)
//
// *** THE SAFETY RAILS ARE PART OF THE FIXTURE, NOT A CONVENTION. ***
// This is a live cluster with the owner's real GitLab, real LiteLLM, real Ollama
// and real database on it.
//
//   1. StartCluster REFUSES to run unless the current context matches
//      GONK_E2E_CONTEXT (default "admin@orac"). Creating a throwaway namespace on
//      the WRONG cluster is the one mistake this harness could make that hurts.
//   2. Every object the suite creates is IN THE RUN NAMESPACE. Nothing else is
//      touched, and nothing the suite did not create is ever deleted.
//   3. The namespace is labelled gonk.orac.local/e2e-run=<runid> so `make e2e-clean`
//      can reap orphans from a run that was killed mid-flight.
//   4. LiteLLM is DEPLOYED BY THE HARNESS, INTO THE RUN NAMESPACE, with the stub as
//      its only upstream -- and asserted. The production litellm.litellm.svc routes
//      local models to a REAL GPU and writes to the owner's REAL spend ledger.
//   5. The ledger is IN-NAMESPACE (bundled Dolt, or postgres.mode=cnpg). NEVER
//      postgres.mode=shared: that is tenancy on databases-app/postgres, which is
//      the owner's real database.
//
// IMAGES GO THROUGH THE REAL REGISTRY (OD-1, answered):
//   build -> tag registry.orac.local/agentic/gonk-project/<img>:e2e-<runid> -> push
//   -> the cluster pulls with `gitlab-registry-pull-creds`.
// No sideload, no `sudo`, no SSH, no `kind load`. ALWAYS REBUILDS AND RE-PUSHES;
// never copies a file into a running container (house rule). The run id in the tag
// is what stops a cached image from silently winning.
//
// TLS: the run namespace gets trust-manager's `trust-bundle` ConfigMap like every
// other namespace, and the chart sets SSL_CERT_FILE from it. NO TEST MAY DISABLE
// TLS VERIFICATION.
func StartCluster(t testing.TB, rt *Runtime) *Cluster
func (c *Cluster) Namespace() string                                 // gonk-e2e-<runid>
func (c *Cluster) PushImages(t testing.TB, tag string, names ...string)
func (c *Cluster) InstallChart(t testing.TB, values map[string]any)  // chart/gonk + chart/values-e2e.yaml (HB-5)
func (c *Cluster) InstallLiteLLM(t testing.TB, stubURL string)       // the harness's OWN LiteLLM
func (c *Cluster) WaitReady(t testing.TB)                            // kubectl wait; NEVER a sleep
func (c *Cluster) PortForward(t testing.TB, svc string, port int) string

// NetworkPolicyEnforced MEASURES whether this CNI drops what a policy forbids, by
// applying a deny-all to a scratch pod and trying to reach a known-good endpoint.
// On orac it returns FALSE (Flannel; Cilium suspended), which is what skips the
// egress test in Step 3. It is a positive control, not a version check: a CNI can
// be installed and not enforce.
func NetworkPolicyEnforced(t testing.TB) bool
```

- [ ] **Step 3: The spec-11 walking skeleton** — `test/e2e/e2e_test.go`

```go
//go:build e2e

// Spec 11.1: install, and IDLE MEANS IDLE.
func TestHelmInstallIdleStateIsPlatformServicesOnly(t *testing.T) {
	// After the chart settles: controller, dolt/postgres, intake, meter, litellm.
	// ZERO agent pods. min_active_sessions = 0 everywhere (spec 4.2).
	// A single idle agent pod is a scale-to-zero regression and it costs real money
	// on a real cluster, forever.
}

// Spec 11.2 + 11.3 + 11.5: THE SCENARIO. Closes P2-7.
func TestOnboardTriageAndAttribute(t *testing.T) {
	g := gitlabGroup(t)                       // fresh group, this run only
	p := g.NewProject(t, "widget")            // no .gonk.yml

	// 1. Invite the bot -> a deterministic onboarding MR, ZERO tokens (spec 5.3).
	p.AddMember(t, botUser, glab.AccessMaintainer)
	forceReconcile(t)                          // HB-1: do not wait ten minutes
	mr := waitForMR(t, p, "gonk/onboard")      // WaitFor on the GitLab API; never a sleep
	ledger.AssertNoSpend(t, views)             // the onboarding MR must cost NOTHING

	// P2-4: on CE, a single assignee_id. Confirm it is assigned and visible.
	requireAssignedToMaintainer(t, mr)

	// 2. Merge -> pending (.agent/ does not exist).
	p.MergeMR(t, mr.IID)
	forceReconcile(t)
	requireState(t, p, "pending")

	// 3. The scaffold MR: the FIRST METERED WORK, and the first real opencode session.
	stub.SetScript(cassette(t, "scaffold-turn"))   // AD-2: a RECORDED opencode turn
	waitForMR(t, p, "gonk/scaffold")
	forceSpendSync(t)                              // HB-2
	requireState(t, p, "valid")

	// 4. File an issue -> a triage comment lands, posted by the SESSION (not intake).
	iss := p.NewIssue(t, "the widget is broken")
	stub.SetScript(cassette(t, "triage-turn"))
	note := waitForBotNote(t, p, iss.IID)
	requireLabelPrefix(t, iss, "gonk::")

	// 5. AND THE LEDGER SAYS EXACTLY THIS. Right bead, right session, right rung,
	//    right attempt, right trigger. This is what pkg/atags exists for, and it is
	//    the difference between "the demo worked" and "the system is trustworthy."
	forceSpendSync(t)
	ledger.Assert(t, views, []ledger.Want{
		{BeadID: beadFor(t, p, "scaffold"), Trigger: atags.TriggerScaffold,
			Rung: "qwen-local", Attempt: 1, CostUSD: 0 /* local: never a real dollar */},
		{BeadID: beadFor(t, iss), Trigger: atags.TriggerIssueTriage,
			Rung: "qwen-local", Attempt: 1, CostUSD: 0},
	})
	// Spec 11.5's other half: the attribution is visible at bead AND project granularity.
	requireCostAPI(t, "/v1/cost/bead/"+beadFor(t, iss))
	requireCostAPI(t, "/v1/cost/project/"+p.Path)
}

// ============================================================================
// SPEC 9's CENTRAL CLAIM. THIS TEST IS WRITTEN, AND IT IS SKIPPED.
// ============================================================================
//
// "Agent pods have egress only to GitLab and LiteLLM, so budgets cannot be
// bypassed." On the orac cluster that is FALSE, today, at the network layer:
//
//   * NetworkPolicies are NOT ENFORCED. Flannel does not implement them, and the
//     Cilium HelmRelease that would is SUSPENDED.
//   * LiteLLM routes local models to Ollama at http://192.168.1.142:11434, WHICH
//     REQUIRES NO CREDENTIAL. An agent pod can call it directly, skip LiteLLM
//     entirely, and burn local GPU with ZERO METERING and NO CEILING.
//
// Owner decision (2026-07-13): SHIP IT, DOCUMENT THE GAP, DO NOT GATE ON IT.
//
// So this test EXISTS, IN FULL, and it SKIPS.
//
//   * DO NOT DELETE IT. When Cilium lands, UN-SKIPPING THIS TEST IS THE GATE --
//     and note the trap: a WRONG networkPolicy.agentPodSelector (Plan 05's OD-5;
//     Gas City is not even deployed, so nobody knows the labels) RENDERS FINE AND
//     ENFORCES NOTHING. The chart's render test cannot catch that. THIS TEST IS
//     THE ONLY THING THAT CAN.
//   * DO NOT LET IT PASS VACUOUSLY. A "pass" because the agent pod did not exist,
//     or because `curl` was missing from the image, is a green light over an open
//     door. The preconditions below are therefore FAILURES, not skips: we skip
//     because the CNI does not enforce policy, and for NO OTHER REASON.
//
// Until then, the honest claim, and the only one anyone may make:
//     CLOUD-rung budgets are HARD    (a cloud call needs a key the pod only ever
//                                     holds via LiteLLM's virtual key, and LiteLLM
//                                     refuses at the ceiling -- proven by
//                                     TestVirtualKeyIsAHardDoorInTheRealDeployment)
//     LOCAL-model budgets are ADVISORY.
func TestAgentPodCannotReachAnythingButGitLabAndLiteLLM(t *testing.T) {
	pod := runningAgentPod(t)          // FAILS (not skips) if there is no agent pod
	requireExecutable(t, pod, "curl")  // FAILS (not skips) if we could not test even in principle

	if !harness.NetworkPolicyEnforced(t) {
		t.Skip("SKIP: NetworkPolicy egress is unenforced — Flannel does not implement it " +
			"and Cilium is suspended; un-skip when Cilium lands")
	}

	// ---- Everything below is the real test, and it runs the day Cilium lands. ----

	// The one door that must be OPEN.
	requireReachable(t, pod, litellmURL(t)+"/health/liveliness")

	// Every door that must be SHUT. Each of these, if open, makes every budget in
	// this system a suggestion.
	requireUnreachable(t, pod, "http://192.168.1.142:11434/api/tags") // Ollama: NO CREDENTIAL. THE bypass.
	requireUnreachable(t, pod, stubModelURL(t)+"/v1/models")          // the model, around LiteLLM
	requireUnreachable(t, pod, "https://api.anthropic.com/v1/models") // a cloud model, around LiteLLM
	requireUnreachable(t, pod, "http://1.1.1.1")                      // the internet at all
}
```

**`harness.NetworkPolicyEnforced` must actually measure, not assume** (Task 2). It
is a **positive control**: apply a deny-all `NetworkPolicy` to a scratch pod in the
run namespace, try to reach a known-good endpoint, and report whether the packet
was dropped. Then delete it.

```go
// NetworkPolicyEnforced answers "does this CNI drop what a NetworkPolicy forbids?"
// by MEASURING it, not by checking a version string or a values flag.
//
// Why a positive control and not `cilium status`: what we need to know is whether
// a policy has TEETH, and the only honest way to learn that is to bite something.
// A CNI can be installed and not enforce; a policy can be present and not match.
//
// The day this starts returning true, the egress test above un-skips ITSELF -- and
// if it then FAILS, the agentPodSelector was wrong all along, which is exactly the
// failure mode that renders fine and enforces nothing.
func NetworkPolicyEnforced(t testing.TB) bool
```

**`make e2e` must print the skip.** A skipped security test that scrolls past in a
`-v` dump has not been communicated. Task 9's `make e2e` target ends with a summary
line: `SKIPPED (unenforced NetworkPolicy): 1 — cloud budgets hard, local budgets
advisory. See docs/environment.md.`

- [ ] **Step 4: The things only a real GitLab can answer** — `test/e2e/gitlab_test.go`. **Closes P2-1 … P2-6.**

```go
//go:build e2e

// P2-1. pkg/ghook/testdata/*.json were HAND-BUILT FROM DOCS and have never been
// compared to a byte GitLab actually sent. Every field pkg/ghook/event.go reads is
// listed in its doc comment -- that list is the checklist.
//
// This test CAPTURES the real payloads (issue open, note-with-mention, MR merged) and
// diffs them against the goldens. On a diff it writes the new payload to
// pkg/ghook/testdata/ and FAILS, so a human reviews the change. Re-run on EVERY GitLab
// upgrade (spec 12.6) -- payload drift is a named risk, and this is the only thing
// standing under it.
func TestGoldenWebhookPayloadsMatchRealGitLab(t *testing.T)

// P2-2. Plan 02's AD-3 says a re-invite is `member.created_at > MR.closed_at`. THAT
// FIELD MAY NOT EXIST ON CE. If it does not, "declined" becomes permanent until a
// human intervenes -- a degraded but SAFE fallback we must consciously accept, in
// writing, rather than discover in production.
func TestMemberPayloadCarriesCreatedAt(t *testing.T) {
	// If absent: FAIL with the exact remedy, and file the Plan 02 amendment.
}

// P2-3. Does GitLab accept a hook URL with a query parameter (`?gen=N`, Plan 02's
// freshness marker) and return it UNCHANGED? The whole hook-rotation design rests on it.
func TestHookURLWithQueryParamRoundTrips(t *testing.T)

// P2-5. Does GitLab trust the ingress certificate with enable_ssl_verification: true?
// If not, every webhook silently fails and the only symptom is latency (reconciliation
// still works -- which is exactly why this could go unnoticed for weeks).
func TestWebhookTLSVerificationAgainstTheIngressCert(t *testing.T)

// P2-6. GitLab AUTO-DISABLES hooks that fail repeatedly. Plan 02's ADR-003.3 answers
// this with a 200-on-drop policy. Does it actually work?
func TestRepeatedDropsDoNotAutoDisableTheHook(t *testing.T) {
	// Send 50 events intake will drop (bot-authored). The hook must still be enabled.
}
```

- [ ] **Step 5: Run and commit**

```bash
make e2e-doctor
make gitlab-up                       # SLOW, ONCE. 4-8 minutes cold.
make e2e                             # ~12 min warm
git add test/e2e test/harness/gitlab.go test/harness/cluster.go
git commit -m "test(e2e): L3 full stack — cluster + cached gitlab-ce + chart; the spec-11 scenario and the real-GitLab answers"
```

> **If a golden payload differs, or `created_at` is absent, or the hook URL loses its query param: that is a FINDING, not a test failure.** Record it, amend Plan 02, and say so in the commit. These questions exist precisely because nobody knows the answers.

---

### Task 8: L3 — the budget suite: hard budgets, the race, quiet hours, and the pod-level kills

**Files:** Create `test/e2e/budget_test.go`, `test/e2e/kill_test.go`.

**Everything in Task 4 and Task 5 proves a property in a controlled environment. This proves the same properties in the environment we actually ship.** The scenarios are deliberately the *same scenarios*, run one layer up — that is the design, and it is why `ledger.Assert` takes a `Views` instead of being written per-layer.

- [ ] **Step 1: `budget_test.go` — the crown jewel, in the cluster**

```go
//go:build e2e

// The whole promise, in the real deployment: A PROJECT CANNOT EXCEED ITS CEILING.
// Spec 11.5. Closes P3-7.
func TestBudgetExhaustionBlocksCloudRungsAndProducesDefer(t *testing.T) {
	p := onboardedProject(t, withLadder("qwen-local", "glm"), withBudget(1.00))
	// Force escalation to the CLOUD rung with genuine gate failures, then burn the budget.
	// The next decision must be `defer`, reason `monthly-cost-exhausted`, with a
	// retry_after at the NEXT MONTH BOUNDARY -- not a deny (retrying next month DOES help),
	// and not a stuck bead.
	//
	// The bead parks in `waiting-for-capacity`. Then ADVANCE THE TESTCLOCK past the month
	// boundary (HB-3), and it RUNS. Nothing lost, nothing run early. That is what
	// "defer until capacity/budget" (spec goal 5) actually means, and it is the first
	// time anything has tested it end to end.
	ledger.Assert(t, views, /* not one token past the ceiling */)
}

// THE RACE, IN THE CLUSTER. Same scenario as L1's and L2's, against the real chart,
// the real meter deployment, and whichever ledger backend is in force.
func TestConcurrentSessionsCannotOverspend_InCluster(t *testing.T) {
	// 32 concurrent triage issues filed at once, headroom for exactly 2 cloud attempts.
	// Exactly 2 sessions run. The other 30 park. The ledger never exceeds the ceiling.
	// -count=3.
}

// THE HARD DOOR, IN THE CLUSTER, WITH METER BYPASSED ENTIRELY: read the project's
// virtual key out of its k8s Secret (the KeySink, Plan 05) and hammer LiteLLM directly
// from inside the namespace. LiteLLM MUST refuse at the ceiling.
//
// This is the test that says: even if meter has a bug, even if the pack forgets to
// call /decide, even if an agent goes berserk -- THE MONEY CANNOT ESCAPE.
func TestVirtualKeyIsAHardDoorInTheRealDeployment(t *testing.T)

// THE BRICK TEST, IN THE CLUSTER. The chart's DEFAULT instance ladder + the onboarding
// MR's DEFAULT .gonk.yml. A freshly onboarded project must be able to run.
//
// PLAN 05's chart values are the input here, and this is the ONLY test that reads them
// together with the onboarding template. If the chart ships an instance ladder that does
// not contain the onboarding template's rung (Plan 02 OD-B / Plan 03 OD-D -- the SAME
// open question), EVERY newly onboarded project resolves to `disabled` (ADR-002's
// empty-ladder rule) and gonk is dead on arrival for every new user. This test is the
// thing that catches it.
func TestFreshlyOnboardedProjectCanRunWithChartDefaults(t *testing.T)

// P2-10: quiet hours, end to end, and the ONE place the whole intake/meter/pack split
// is visible: intake FIRES the order (it has no quiet-hours code at all), METER DEFERS
// it, and the PACK runs it when the window ends.
func TestQuietHoursDeferAndResume(t *testing.T) {
	// Set the testclock inside the project's quiet window. File an issue.
	//   - intake fired the order          (assert: the bead exists)
	//   - meter deferred                  (assert: reason "quiet-hours", retry_after = window end)
	//   - NOTHING RAN                     (AssertNoSpend)
	// Advance the testclock past the window. The bead runs. Nothing lost, nothing early.
}
```

- [ ] **Step 2: `kill_test.go` — the pod-level kill matrix (spec 11.6)**

Spec 11.6 names three; the brief names five components. All of them, `kubectl delete pod --grace-period=0 --force` (a real pod dies hard):

| # | Kill | Required outcome |
|---|---|---|
| **K11** | **intake**, mid-onboarding (branch pushed, MR not yet created) | On restart: **exactly one** `gonk/onboard` MR, exactly one branch. No duplicate MR. (**P2-8**) |
| **K12** | **intake**, between webhook receipt and firing the order | The webhook is **lost** — and that is *fine*, because reconciliation is the correctness path. The next reconcile pass recovers the work. **No lost bead.** |
| **K13** | **intake**, after firing an order, before recording it | The order is fired **twice** across the restart. The controller must dedupe on `OrderRequest.BeadAnchor` → **one bead, one session, one triage comment, one charge.** (**P2-8**; this is the load-bearing claim Plan 02 hands to Plan 04.) |
| **K14** | **the agent session pod**, mid-flight with model calls in progress | Spend already made is **still attributed** (LiteLLM logged it). The reservation expires → `infra-failed` → **the same rung retries**. **No lost bead. No free escalation. No double-charge on the retry.** |
| **K15** | **meter**, mid-session | Reservations survive (durable store). Cold start: `/readyz` false until the first spend sync. `/decide` unavailable → the pack **retries**; it does **not** spawn unmetered. |
| **K16** | **LiteLLM**, mid-session (spec 11.6) | Session sees 5xx → `infra-failed` → **same rung, no escalation** (this is spec 11.6's exact wording). Meanwhile every budgeted project **defers** on stale spend. |
| **K17** | **the ledger** (dolt restart — spec 11.6) | No lost work items. Meter `/readyz` false, then recovers with every reservation intact. |
| **K18** | **GitLab**, mid-flow (spec 11.6) | Intake retries; events lost during the outage are recovered by **reconciliation**; **no duplicate comments** (the bead is already anchored). |
| **K19** | **the stub model** | LiteLLM 5xx → `infra-failed` → same rung. Zero tokens billed for the failed calls. |
| **K20** | **the whole namespace** (delete + reinstall the chart, ledger PVC retained) | Every reservation, every attempt, every spend row survives. **A reinstall must not hand a project a fresh budget.** |

Every row ends in `harness.AssertFailsClosed(...)` with all six flags set. There is no row where a weaker assertion is acceptable.

- [ ] **Step 3: Run and commit**

```bash
make e2e                                        # full suite
go test -tags e2e ./test/e2e/ -run 'Budget|Race|HardDoor|Kill' -count=3 -timeout 60m -v
git add test/e2e/budget_test.go test/e2e/kill_test.go
git commit -m "test(e2e): hard budgets, the concurrent-reservation race, quiet hours, and the pod-level kill matrix"
```

---

### Task 9: Docs, gates, and the re-validation checklist

**Files:** Create `docs/adr/ADR-006-test-harness-layers-and-determinism.md`; modify `Makefile`, `.gitlab-ci.yml`, `PLAN.md`; create `test/harness/cmd/secrets-scan/main.go`.

- [ ] **Step 1: `make secrets-scan` — a gate, not a guideline**

Greps the whole tree for token-shaped strings (`glpat-`, `sk-`, `xoxb-`, long base64 runs, anything matching a PAT/JWT shape) and **fails the build** on a hit. Never commit an unencrypted secret (house rule; spec 9). Every credential in this harness is **generated at runtime into a directory outside the repo** — this gate is what keeps it that way when someone is in a hurry.

- [ ] **Step 2: Assert the production images cannot have their clock moved** (**OD-7**)

```go
// The testclock build (HB-3) is how the harness crosses a month boundary without
// waiting a month. It must NEVER ship. A production gonk-meter that reads its clock
// from a file is a production gonk-meter whose BUDGET WINDOW CAN BE MOVED BY ANYONE
// WHO CAN WRITE THAT FILE.
func TestProductionMeterImageHasNoTestClock(t *testing.T) {
	// Build cmd/gonk-meter with NO tags; `go tool nm` (or a strings scan) must show
	// no `testclock` symbol and no GONK_TESTCLOCK_FILE literal.
}
```

- [ ] **Step 3: Write `docs/adr/ADR-006-test-harness-layers-and-determinism.md`**

Records: the three layers and **what each one cannot prove** (the honest half); the stub model as the determinism foundation and the rule that **no test may reach a real model**; the full nondeterminism table from this plan, verbatim; the kill matrix and the **five forbidden outcomes**; **why gitlab-ce is a cached host container and not a chart component**; why L3 is cluster-agnostic (**podman's broken CNI bridge means kind may simply not work on the dev box**); the three-way ledger check; and the runtime table with real numbers measured on the dev box.

Link ADR-002 (config precedence), ADR-003 (intake trust boundary), ADR-004 (rung policy). State plainly that **this ADR modifies none of them** — the harness's job is to prove them, not to reinterpret them.

- [ ] **Step 4: CI jobs, honestly labelled**

Add to `.gitlab-ci.yml`:

```yaml
# L1 runs in the standing `test` job: it needs no containers and takes < 60s.
# L2/L3 are MANUAL, because:
#   (a) EVERY RUNNER ON gitlab.orac.local IS OFFLINE and this repo's CI HAS NEVER
#       EXECUTED (PLAN.md carry-forward). These jobs are aspirational, and pretending
#       otherwise would be dishonest.
#   (b) L3 needs privileged containers, >=8 CPU, >=16 GB, and ~25 minutes.
# THE REAL GATE IS LOCAL: `make gate` and `make component` / `make e2e` on a laptop.
component:
  stage: test
  when: manual
  allow_failure: false
  script: [ "make component" ]

e2e:
  stage: test
  when: manual
  allow_failure: false
  script: [ "make e2e" ]
```

- [ ] **Step 5: The re-validation checklist** — the top of the plan promised it; deliver it

Add `test/README.md` with a **"before you trust this suite"** checklist, because this plan was written against **four unexecuted plans**:

- [ ] Every `pkg/` and `internal/` symbol this harness imports exists with the shape assumed. (`go build ./test/...` is the check.)
- [ ] **HB-1** — `POST /admin/reconcile?wait=true` exists on intake's private listener and **blocks**, returning a `ReconcileSummary` [**Plan 02, Task 10, Step 5**]. If it does not, every `forceReconcile` in L1/L3 is a `sleep` — **find out which, and do not proceed on hope.**
- [ ] **HB-2** — `POST /admin/spend/sync` exists on meter and blocks, and `gonk_meter_spend_synced_at_seconds` is exported [**Plan 03, Task 9, Step 3b**].
- [ ] **HB-3** — the `//go:build testclock` build of `cmd/gonk-meter` exists and reads `GONK_TESTCLOCK_FILE` [**Plan 03, Task 8, Step 5b**], **and Task 9 Step 2's guard passes** (the production image has neither the symbol nor the literal).
- [ ] **HB-4** — **PLAN 04 IS NOT WRITTEN.** Confirm it landed with (a) the published outcome classification and (b) a deterministic gate failure [**this plan, "Requirements for Plan 04"**]. Without both, the escalation-ladder tests are **vacuous** and must be marked so, not quietly passed.
- [ ] **HB-5** — `chart/values-e2e.yaml` exists [**Plan 05, Task 9, Step 1b**], points at a **harness-deployed** LiteLLM (**never** `litellm.litellm.svc`), and contains no credential.
- [ ] **Plan 03 Task 0b has actually been run**, and `GONK_LEDGER` is set to whichever backend it left in force (**OD-5**). *"We assume Dolt works" is not a passing state, and a budget ceiling defended by an unproven isolation guarantee is not defended.*
- [ ] The store env vars the chart sets — `GONK_METER_STORE_BACKEND`, `GONK_METER_STORE_DSN_FILE` [**Plan 03, Task 8, Step 5**] — and the key sink — `keysink.NewK8s`, `GONK_KEYSINK_NAMESPACE` [**Plan 03, Task 7 Step 4b**; RBAC from **Plan 05, Task 4**] — exist. Without the key sink, **no project ever leaves `key-missing` and nothing runs at all.**
- [ ] The rung names in `chart/values-e2e.yaml` and the operator config match the **real** LiteLLM model list — which lives in the **gitops** repo at `clusters/orac/apps/litellm/litellm.yaml` (`spec.values.proxy_config.model_list`). (**Plan 02 OD-B / Plan 03 OD-D — one question, one answer, still open.**)
- [ ] The pinned gitlab-ce version is **18.10.1** and equals gitlab.orac.local's (**OD-2**), and it is **CE** — or the golden payloads are lying and some test may be taking an EE-only path.
- [ ] **`harness.NetworkPolicyEnforced` returns false, the egress test skips, and the skip is REPORTED.** If it ever returns **true**, un-skip the test — and if the test then **fails**, the `agentPodSelector` was wrong all along (Plan 05's OD-5), which is the failure that renders fine and enforces nothing.

- [ ] **Step 6: Update `PLAN.md`**

Set plan 06's Status to `done`. Add to "Contracts published by plan 06":

- `test/stubmodel` — **the determinism foundation.** Any future plan needing a model in a test uses this. **Nothing in the test suite may ever reach a real model.**
- `test/harness` — runtime detection (**podman ⇒ `--network=host`; the CNI bridge is broken and kind may not work on this box**), `WaitFor` (**the harness never sleeps**), runtime-generated credentials (**never committed**), `SyntheticSession`, and the kill primitives.
- `test/ledger` — the three-way ledger assertion (stub log ↔ LiteLLM spend ↔ meter's cost API) and the **five forbidden outcomes**.
- `test/corpus` — the hostile `.gonk.yml` corpus and `FuzzGonkYMLNeverPanics`, **the regression net under Plan 01's one-line remote crash**.
- `make gate` (L0+L1, every commit) / `make component` (L2) / `make e2e` (L3) — **and `make e2e-doctor`, which you run first, every time.**

And to "Carried into later plans":

- **Anyone touching the budget code:** `TestOnboardingDefaultCanAffordItsOnlyRung` is the wall in front of a cliff. **The onboarding default is `monthly_cost_usd: 0` with a local-only ladder. Real dollars and synthetic dollars are different currencies. If anyone ever sums them in a policy decision, every freshly-onboarded project can no longer afford its own only rung and bricks itself silently, on arrival, forever.** That test, and its saboteur, are the only things standing there.
- **Anyone touching the gate:** infra failures never escalate a rung. `TestInterleavedFailuresCountOnlyGateFailures` is the one that catches the subtle version (counting `attempt` instead of gate failures passes every other test).
- **On every GitLab upgrade:** re-run `TestGoldenWebhookPayloadsMatchRealGitLab` (spec 12.6). The goldens are only valid for the pinned version.
- **On every LiteLLM upgrade:** `docs/spikes/litellm-verified.md` is **invalidated**. Re-run Task 5. The `1mo` boundary, the spend-log lag, and the admin-key permission model are all version-specific.
- **Whoever brings a CI runner online:** the `component` and `e2e` jobs exist and are `manual`. They have never run.

- [ ] **Step 7: Final verification and commit**

```bash
make gate                    # gofmt, vet, test -race (L0 + L1), golangci-lint
make secrets-scan
make e2e-doctor
make component
make e2e
git add -A && git commit -m "docs: publish ADR-006, the e2e harness gates, and the re-validation checklist; complete plan 06"
```

---

## Definition of done

- `gofmt -l .` prints nothing; `go vet ./...`, `go test ./... -race -count=1`, `golangci-lint run ./...` are green. **This is the standing gate — CI has never run on this repo.**
- **L1 (`test/integration`) runs in the standing gate, in under 60 seconds, with no containers.** If it needs a container, it is in the wrong layer.
- **Every hand-off in the checklist at the top of this plan is closed** — all ten from Plan 02, all ten from Plan 03 — **or is explicitly recorded as a finding** (e.g. "CE's member payload has no `created_at`; Plan 02's AD-3 falls back to permanent-decline"). A hand-off that is neither closed nor recorded is an open hole.
- **The reservation race passes at all three layers** — L1 (memory store), L2 (**the real backend, whichever Task 0b left in force**), L3 (in-cluster) — at `-count≥3`. Exactly K winners against headroom for K, every run.
- **The hard door closes with meter bypassed entirely**, at L2 and L3. If LiteLLM never refuses, **that is a fatal finding and it goes to the owner immediately**, not into a follow-up ticket.
- **The NetworkPolicy egress test is WRITTEN, and it SKIPS with exactly this message:**
  `SKIP: NetworkPolicy egress is unenforced — Flannel does not implement it and Cilium is suspended; un-skip when Cilium lands`
  It is **not deleted**, it does **not pass vacuously** (a missing agent pod or a
  missing `curl` is a **failure**, not a skip), and `harness.NetworkPolicyEnforced`
  **measures** enforcement with a positive control rather than assuming it. **The
  skip is reported in `make e2e`'s summary line**, not buried in `-v` output.
  **Un-skipping it is the gate when Cilium lands.**
- **No document, test name, or summary produced by this plan claims budgets cannot
  be bypassed.** The honest form, everywhere: **cloud-rung budgets are hard**
  (proven by `TestVirtualKeyIsAHardDoorInTheRealDeployment`); **local-model budgets
  are advisory** until Cilium lands (`docs/environment.md`).
- **L3 runs against the real cluster in a throwaway namespace, safely:** the context
  check refuses an unexpected cluster; nothing outside `gonk-e2e-<runid>` is
  touched; **the production LiteLLM is never used** (it routes to a real GPU and a
  real spend ledger); **`postgres.mode: shared` is never used** (that is the
  owner's real database); images go through `registry.orac.local` with per-run
  tags, and `make e2e` always rebuilds and re-pushes.
- **`TestOnboardingDefaultCanAffordItsOnlyRung` passes, and its saboteur proves it is not vacuous.**
- **Every kill test ends in `AssertFailsClosed` with all six flags**, and `TestAssertFailsClosedCatchesEachViolation` proves the assertion can actually fail.
- **The hostile corpus is green and the fuzz target runs.** No input crashes anything; every one is a 422; none provisions a key or fires an order.
- **Determinism is mechanical, not aspirational:** `TestNoAssertionDependsOnModelProse` passes; `grep -rn "time.Sleep" test/` returns **nothing** outside `stubmodel` (where `Step.Delay` is a deliberate, scripted feature); no test reaches a real model.
- **No credential is committed.** `make secrets-scan` is green; every token is generated at runtime into a directory outside the repo; every service reads its secrets from **files**, never env values.
- **The production `gonk-meter` image contains no `testclock`.**
- `docs/spikes/litellm-verified.md` records **what was actually measured**, against a **named version** — the spend-log lag as a number, the `1mo` boundary as a yes or a no. **"It probably works" is not a passing state for anything in this plan.**
- `ADR-006` is written; `PLAN.md` records the contracts and the carry-forwards; `test/README.md` carries the re-validation checklist.
- Every remaining open question is either an **"Assumed default"** the harness actually implements, or an **"Owner decision needed"** block still flagged in `PLAN.md`. **Nothing has been silently decided.**

