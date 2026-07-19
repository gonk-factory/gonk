# RECONCILIATION-06 — Plan 06 (e2e harness) vs shipped Plans 02–05

**Task 0 for Plan 06.** Plan 06 (`docs/superpowers/plans/2026-07-13-plan-06-e2e-harness.md`,
3194 lines) was written when only Plan 01 had shipped. Plans 02 (gitlab-intake),
03 (gonk-meter), 04 (pack & images) and 05 (chart) have since **landed and
merged** (see `git log`: gonk-yku/vsy/5we/fsl dispatch-fix chain, `171760c Merge
plan-05`). This document re-validates every external symbol / flag / endpoint /
value Plan 06 names against the real tree, exactly as Plan 05's Task 0 did.

**This is READ + WRITE-A-DOC only. No suite is built here.**

Legend: **OK** = exists as cited · **Δ** = exists but renamed/moved/reshaped ·
**GONE** = cited thing does not exist · **NEW** = shipped reality the plan predates.

---

## 0. The load-bearing deltas (read these first)

These four change the *design* of the harness, not just a symbol name. Every one
postdates the plan text.

### D1 — Dispatch is ed25519 grant-gated (the single biggest thing the plan predates)

`pkg/gcapi` grew a signing layer (`writeauth.go`, merged gonk-yku). The e2e's
dispatch path now needs a **signed grant** or the controller answers **401**.

| Cited by plan | Shipped reality |
|---|---|
| "intake dispatches to the supervisor" (unsigned) | `intake.NewHTTPDispatcher(url, signer, nil)` — a `*gcapi.Signer` is **required** to fire against a grant-gated controller. Nil Signer ⇒ no grant headers ⇒ 401 `IsWriteAuthRequired`. |
| — | **NEW:** `gcapi.Signer`, `gcapi.LoadSigner(path, kid, ...SignerOption)`, `WithCID`, `WithEpoch`, `WithTTL` (`pkg/gcapi/writeauth.go`). Grant = `X-GC-City-Write` header, single-use ed25519, request-bound, aud `gc-city-write.v2`. |
| — | **NEW env (cmd/gonk-intake, cmd/gonk-gate):** `GONK_GC_WRITE_KEY_FILE` (PEM PKCS#8 ed25519 private key mount), `GONK_GC_WRITE_KEY_ID` (kid), `GONK_GC_WRITE_CID` (optional tenancy cid). |
| — | **NEW chart values (chart/gonk/values.yaml):** `gascity.writeAuth.verifyKey` = `"kid:base64"` PUBLIC key (G22: REQUIRED when `gascity.enabled`, no `allowUnauthenticated` escape hatch), `gascity.writeAuth.cid`; the matching PRIVATE half is `secrets.gcWriteKey` (a file mount **shared by gonk-intake and the in-pod gonk-gate**, both sign with the same kid). |

**Harness consequence (Task 7/8):** the e2e fixture must **mint an ed25519 keypair
at runtime** (never committed — `make secrets-scan` will catch it), pass the public
half as `gascity.writeAuth.verifyKey`, mount the private half as `secrets.gcWriteKey`,
and point `GONK_GC_WRITE_KEY_FILE` at it for intake and gate. A dispatch test that
does not provision the grant proves nothing but a 401. **This is net-new harness
work the plan has zero lines for.**

### D2 — Ledger is Postgres-only; there is no Dolt store and no `GONK_LEDGER`

The plan's OD-5 ("support **both**, `GONK_LEDGER=dolt|postgres`, `make component`
runs both") is **dead**. Task 0b settled it (owner-approved) — Dolt failed the
reservation-race isolation spike (`docs/spikes/dolt-reservation-isolation.md`,
ADR-004 Decision 10).

| Cited by plan | Shipped reality |
|---|---|
| `GONK_LEDGER=dolt\|postgres` env | **GONE.** Real env is `GONK_METER_STORE_BACKEND` — **required, no default**, and the only accepted value is `postgres` (`cmd/gonk-meter/main.go:208-230`: "dolt is deliberately NOT a case here: there is no `store.OpenDolt`"). |
| `internal/meter/store` Dolt backend | **GONE.** `internal/meter/store/` has `memory.go` + `postgres.go` only. No `dolt.go`. |
| `test/harness/ledgerdb.go` = "Dolt \| CNPG-Postgres fixture" | Must be **Postgres-only**. No dual-backend loop. |
| chart `dolt.enabled` implies ledger=dolt | **Δ:** bundled Dolt **backs BEADS ONLY** (`chart/values-e2e.yaml:62-69` self-note); `ledger.backend: postgres` is unconditional. `postgres.mode` ∈ `shared\|cnpg\|external` (e2e never uses `shared`). |
| Task 5 "runs the suite twice, once per backend" | **Runs once, Postgres.** The race test (`internal/meter/store/postgres_race_test.go` exists) is Postgres-only. |

### D3 — Controller command & port: `gc start --foreground /city` on **:9443** (grant-gated), not supervisor **:8372**

| Cited by plan | Shipped reality |
|---|---|
| old "supervisor-8372" assumption | **Δ.** The bundled controller runs **`gc start --foreground /city`** (settled gonk-fsl re-smoke, `chart/gonk/values.yaml:225`), binding the per-city **[api] listener on 0.0.0.0:9443** (`gascity.supervisorPort: 9443`, any-host, `allow_mutations=true`). |
| — | **8372** (`gcapi.DefaultPort`) is the machine-wide supervisor admin API — **127.0.0.1 loopback only**, never exposed by a Service/Ingress. The e2e never targets it. |
| dispatch endpoint | `POST /v0/city/{cityName}/order/gonk-dispatch/run`, body `{"vars":{...}}`, on the derived `gascity.supervisorURL` (in-chart Service → 9443). Grant-gated. |
| controller delivery/workload | **NEW settled (Task 0.5):** `delivery: prebaked` (initContainer copies pack to writable emptyDir `/city`, runs `gc init --preserve-existing --bootstrap-profile k8s-cell`); `workload: deployment` strategy **Recreate** (exactly one controller, no leader election). |

*Note an internal doc wrinkle:* `chart/gonk/values.yaml:179` still says "runs `gc
supervisor run`… NOT `gc start --foreground /city`", contradicted by line 225's
later re-smoke ("runs `gc start --foreground /city`"). The **operative** facts —
port 9443, grant-gating — are unambiguous; the command-string comment is stale and
harmless, but flag it if Task 7 shells the controller directly.

### D4 — `trigger = "webhook"` is the ORDER trigger, distinct from the semantic `atags.Trigger*`

Merged gonk-vsy. There are **two** "trigger" concepts and the harness must not
conflate them.

| Concept | Value(s) | Where |
|---|---|---|
| **Order** trigger (gates the `/run` endpoint) | `"webhook"` (fired orders: gonk-dispatch, gonk-triage, gonk-scaffold, gonk-mention); `"cooldown"` (gonk-sweep) | `pack/orders/*.toml`. The `/run` endpoint fires **only** webhook-trigger orders; a manual trigger is **rejected 422**. |
| **Semantic** trigger (a var carried in the order body / `OrderRequest.Trigger`) | `issue-triage`, `scaffold`, `mention-reply`, `onboarding` | `pkg/atags/tags.go`. **There is no `atags.TriggerWebhook`.** |

Exec orders pass vars to `gonk-gate` via `GC_WEBHOOK_ARG_*` env
(`pack/scripts/gonk-dispatch.sh`, `pack/scripts/gonk-check.sh`).

---

## 1. Wire contract — `pkg/meterapi` (the L1/L2 seam; P3-10, P2-9)

| Plan reference | Shipped | Status |
|---|---|---|
| `POST /v1/policy/decide` `DecideRequest`→`DecideResponse` | `meterapi.DecidePath = "/v1/policy/decide"`; both types present | **OK** |
| `POST /v1/policy/outcome` `OutcomeRequest`→`OutcomeResponse` | `OutcomePath = "/v1/policy/outcome"` | **OK** |
| DELETE de-onboard | `DELETE /v1/projects/{project}` → **204** (`meterapi.ProjectPath`, `internal/meter/service/http.go:50,161`); idempotent (unknown project = 204, not 404) | **OK** |
| "bad `.gonk.yml` is **422 not 400**, carrying `gonkcfg.Load`'s message" | Confirmed: service returns **422** with a **full `ProjectResponse`** `State==StateInvalid` + `Error` set (NOT an `ErrorResponse`; `meterapi.go:506-517`, `service_test.go:210/244`). `ErrorResponse{error}` is the 400/500 body only. | **OK** |
| decide `defer` + `retry_after` | `DecideResponse.Decision` ∈ `run\|defer\|deny`; `RetryAfter time.Time` "set on EVERY defer" | **OK** |
| outcome classification enum | `OutcomeSuccess`/`OutcomeGateFailed`/`OutcomeInfraFailed`/`OutcomeAborted` (`"success"/"gate-failed"/"infra-failed"/"aborted"`); only `gate-failed` escalates | **OK** |
| no attempt field on request | `TestDecideRequestHasNoAttemptField` present (Decision 2 forgery guard) | **OK** |
| `AdminSpendSyncPath` | `= "/admin/spend/sync"` (HB-2) | **OK** |
| `config_hash` on ProjectResponse | `ProjectResponse.ConfigHash` present | **OK** |
| cost endpoints | `CostBeadPath`/`CostSessionPath`/`CostProjectPath`/`CostInstancePath` + `BeadCostResponse`/`…` present | **OK** |

---

## 2. Meter internals & metrics — `pkg/{budget,opercfg,spend,rung}`, `internal/meter/*`, `cmd/gonk-meter`

| Plan reference | Shipped | Status |
|---|---|---|
| `pkg/rung.Decide` | `pkg/rung/decide.go:130 func Decide(Input) Decision` | **OK** |
| `pkg/budget.Remain` | `pkg/budget/remaining.go:50 func Remain(Budget, Spend) Remaining` | **OK** |
| `pkg/opercfg.Load` | `pkg/opercfg/opercfg.go:161 func Load(raw []byte)` | **OK** |
| `pkg/spend` | present: `Row`, `Totals`, `Sum`, `ProjectTotals`, `BeadTotals`, `SessionTotals`, `ByRung`, `ByTrigger`, `Window` | **OK** |
| `internal/meter/{store,litellm,keysink,service}` | all present (+ `metrics`, `tagmint`) | **OK** |
| `cmd/gonk-meter` store selection | `GONK_METER_STORE_BACKEND` required, `postgres` only (see D2) | **Δ** |
| `gonk_meter_catalog_drift_total` (budget suite / P3-8) | `internal/meter/metrics/metrics.go:191` | **OK** |
| reservation-race counter | `gonk_meter_reservation_races_lost_total` (`:187`) | **OK** (real name) |
| HB-2 predicate gauge | `gonk_meter_spend_synced_at_seconds` (`:151`, "WAIT-ON-AS-A-PREDICATE series") + `…_spend_sync_age_seconds` (`:147`, alerting) | **OK** |
| other asserted metrics | `reservations_open`, `reservations_expired_total`, `clock_skew_seconds`, `clock_skew_unknown_total`, `budget_window_start_seconds`, `cold_start_total`, `spend_usd_total`, `tokens_total`, `budget_remaining_usd/tokens`, `policy_decisions_total`, `ladder_escalations_total`, `gate_outcomes_total`, `deferred_beads`, `project_state`, `virtual_keys`, `spend_rows_unattributed_total`, `spend_sync_failures_total` | **OK** |
| LiteLLM admin calls (P3-1) | `internal/meter/litellm/admin_http.go`: `EnsureKey`/`updateByAlias`/`RotateKey`/`DeleteKey` → `/key/generate`, `/key/update`, `/key/delete`, `/key/info` | **OK** |
| `/spend/logs` (P3-1, determinism #6) | **Δ:** real path is **`/spend/logs/v2`** (`spendsource_http.go:40`), mandatory `start_date`+`end_date`, paginated, 10k cap. The plan cites `/spend/logs` (the legacy one that OOM-killed the live pod). Harness/asserts must target v2. Date-bound guard already enforced in `HTTPSpendSource.Since`. | **Δ** |

---

## 3. Intake & dispatch — `pkg/{glab,ghook,intake}`, `cmd/gonk-intake`, `pkg/gcapi`

| Plan reference | Shipped | Status |
|---|---|---|
| `pkg/glab`, `pkg/glab/glabtest`, `pkg/ghook`, `pkg/intake`, `cmd/gonk-intake` | all present | **OK** |
| `OrderRequest.BeadAnchor` dedup field (P2-8, HB-4.4) | `pkg/intake/dispatch.go:56 BeadAnchor string`; `func BeadAnchor(projectID, issueIID)` ; "controller MUST dedupe on it"; pack order param `bead_anchor {required=true}` | **OK** |
| dispatcher = signing gcapi client | `intake.NewHTTPDispatcher(SupervisorURL, signer, nil)`; `Dispatcher.FireOrder(ctx, OrderRequest)` | **Δ** (now signed — see **D1**) |
| `MeterResyncInterval` / staleness (P2-9 kill switch) | `reconcile.go:107-115 MeterResyncInterval`; `:509 resyncDue` re-registers with meter **even when config unchanged**, defeating the `config_hash` short-circuit — exactly the kill-switch path P2-9 needs. Dispatch-side staleness = `Dispatch.StalenessWindow` (`DefaultStalenessWindow = 30m`), env `GONK_DISPATCH_STALENESS_WINDOW`. | **OK** |
| 422-not-400 seam | see §1 | **OK** |
| DELETE deletes the LiteLLM key (P2-9) | `deleteProject` → keysink/admin `DeleteKey` | **OK** |
| supervisor env | `GONK_SUPERVISOR_URL` (+ the three `GONK_GC_WRITE_*`). `""` ⇒ `LogDispatcher` (dry run). | **OK** |

---

## 4. The pack — `pack/orders/*.toml`, formulas, `cmd/gonk-gate`

| Plan reference | Shipped | Status |
|---|---|---|
| dispatch order name | `pack/orders/gonk-dispatch.toml`, `exec = scripts/gonk-dispatch.sh` ("GATE 2, the only path that pours a formula") | **OK** |
| trigger values | `trigger = "webhook"` on fired orders (see **D4**) | **Δ** |
| gonk-gate contract | `cmd/gonk-gate/{main,dispatch,sweep,check,artifact,meter}.go`; subcommands `dispatch\|sweep\|check` | **OK** |
| `GC_WEBHOOK_ARG_*` vars | `pack/scripts/gonk-dispatch.sh`, `gonk-check.sh` (exec-orders convention) | **OK** |
| HB-4a classifier | `pkg/gate.Classify(Signals) string` (pure; `pkg/gate/outcome.go:59`); `Signals{ArtifactPresent, ModelTokens, …}`; load-bearing marker `pkg/gate/marker.go`; reported via `POST /v1/policy/outcome` + stdout JSON from `gonk-gate sweep` | **OK** (event-bus *publish* half still deferred, as the plan states) |
| HB-4b deterministic gate failure | Classify: `ArtifactPresent:false && ModelTokens>0` ⇒ `gate-failed`; `ModelTokens<=0` ⇒ `infra-failed` | **OK** |
| sweeper forces spend sync before classifying | `gonk-gate sweep` calls `/admin/spend/sync` and waits on `spend_as_of` (`outcome.go:45`); ⇒ HB-2 is a **production** dependency of the pack | **OK** |

---

## 5. Chart & values — `chart/gonk`, `chart/values-e2e.yaml` (HB-5)

| Plan reference | Shipped | Status |
|---|---|---|
| `chart/values-e2e.yaml` exists (HB-5) | present (111 lines), **INSTALLED not rendered**; already self-reconciled ("RECONCILED, Task 0" notes in-file) | **OK** |
| `componentImages.agent` tag seam | `chart/gonk/values.yaml:53 componentImages.agent.repository: gonk-agent` (chart does NOT deploy the agent; the controller's k8s session provider pulls it at runtime) | **OK** |
| testclock meter image | `meter.image.tag` set to `gonk-meter:e2e-<runid>-testclock` (values-e2e:40) | **OK** |
| write-auth values (D1) | `gascity.writeAuth.verifyKey` / `.cid` + `secrets.gcWriteKey` | **OK** (harness must provision — see D1) |
| ledger default | `ledger.backend: postgres` unconditional; `postgres.mode` harness-set (never `shared`) | **OK** (Δ from plan's dual-backend) |
| litellm.externalURL harness-deployed | `litellm.mode: external`, `externalURL: ""` harness-set → its own LiteLLM, stub upstream (no `litellm.enabled: true` — never existed, per HB-5 correction) | **OK** |
| e2e rung catalog, round synthetic prices | `operatorConfig.rungs`: `qwen-local`(local, `synthetic_usd_per_1m_tokens: 0.25`, model `stub-local`), `glm-test`(cloud, `est_cost_usd: 0.20`, model `stub-cloud`); ladder `[qwen-local, glm-test]`; `enforce_ladder_order: true`; `defaultRung: qwen-local` | **Δ names** |
| 5 dolt boot fixes | merged (`221baf2 fix(chart): controller + Dolt boot clean from scratch (5 blockers)`) | **OK** |
| `agentPodSelector` (NetworkPolicy gap / OD-5 hand-back #3) | `networkPolicy.agentPodSelector: {app: gc-agent}` default (Gas City hardcodes `app: gc-agent`); NetworkPolicy rendered but **not enforced** (Flannel; Cilium suspended) — egress test stays written-and-skipped | **OK** |

**Δ names note:** the plan's Task 5 L2 price table (`qwen-local $0.25`, `glm $2.00`,
`sonnet $6.00`; models `stub-qwen/stub-glm/stub-sonnet`) is the **L2 harness-configured
LiteLLM** list, independent of the chart. The **chart** (L3) uses `qwen-local`/`glm-test`
with models `stub-local`/`stub-cloud`. Task 5 and Task 7/8 must each use their own
catalog; don't cross-cite. Only `qwen-local @ $0.25` synthetic is common to both.

---

## 6. Testclock seam — `cmd/gonk-meter/clock_testclock.go` (HB-3 / OD-7)

**OK, exactly as specified.** `//go:build testclock`; `Now()` reads
`GONK_TESTCLOCK_FILE`, parses a **signed second offset**, returns
`time.Now().UTC().Add(offset)`; missing/empty/unparseable file ⇒ plain
`time.Now().UTC()` (monotone-safe). Production build `clock.go` (`//go:build
!testclock`) has no file and no symbol — so Task 9 Step 2's
`TestProductionMeterImageHasNoTestClock` (nm/strings scan for `testclock` +
`GONK_TESTCLOCK_FILE`) is enforceable as written.

**Harness moves the clock by writing an offset (seconds) to the file named by
`GONK_TESTCLOCK_FILE`.** `test/harness/clock.go` writes it; e2e image is the
`testclock` build; L1 injects the clock directly (Plan 03's pure funcs).

---

## 7. Existing test conventions — `test/images/` (Plan 04 smokes)

Present: `agent_smoke_test.go`, `controller_smoke_test.go`, `nolatest_test.go`,
`packvalidate_test.go`, `servers_smoke_test.go`, `trailers_hook_test.go`. Plan 06's
new suites should match these conventions (build-tag gating, podman `--network=host`
where containers are used, per-run tags never `latest`). `internal/charttest` and
`internal/packtest` exist for render/pack validation and are the model for
non-container gates.

---

## 8. Hand-off verification (every P2-*/P3-*/HB-* item)

### Hand-**backs** (HB-1..HB-5) — *the deterministic-harness prerequisites*

| # | Dependency | Shipped? | Verdict |
|---|---|---|---|
| **HB-1** | intake `POST /admin/reconcile?wait=true` → `ReconcileSummary` | `pkg/intake/server.go:17-27,108,152`. Fields: `started_at,finished_at,projects,states,meter_pushes,dispatched,errors,result` (a **superset** of the plan's cited `states/meter_pushes/dispatched/errors/result`). `Pass.WaitForNextPass` blocks for a pass starting at/after the call. Bare POST = 202. | **SATISFIED** |
| **HB-2** | meter `POST /admin/spend/sync` (blocking) + `spend_as_of` + synced-at gauge | `AdminSpendSyncPath`; `SpendSyncResponse{spend_as_of, rows_ingested, unattributed, synced, error}`; gauge `gonk_meter_spend_synced_at_seconds`. | **SATISFIED** |
| **HB-3** | testclock clock seam | §6 — exact. | **SATISFIED** |
| **HB-4** | outcome classification (a) + deterministic gate failure (b) | §4 — `pkg/gate.Classify` + `/v1/policy/outcome`; HB-4b full. Event-bus *publish* still **deferred** (as the plan itself records — subscribe to `/v1/policy/outcome`, the strongest surface). | **SATISFIED** (with the documented deferral) |
| **HB-5** | `chart/values-e2e.yaml` | §5 — present; corrected to Postgres-only + external-LiteLLM + grant-gated bundled controller. | **SATISFIED** |

**No hand-back shipped missing.** The harness is **never forced onto `time.Sleep`** —
every non-determinism source (#4 clock, #6 spend poll, #7 reconcile) has its
predicate/seam in shipped code. This is the good news; the plan feared the opposite.

### From Plan 02 (P2-1 … P2-10)

| # | Depends on | Status |
|---|---|---|
| P2-1 golden payload refresh | `pkg/ghook/testdata/*.json` exist; refresh is a **live-GitLab** act (Task 7), not a code gap | dep OK; needs gitlab-ce |
| P2-2 CE `created_at` on member payload | AD-3 re-invite rule lives in intake; whether CE emits `created_at` is a **live-instance** question | dep OK; needs gitlab-ce |
| P2-3 hook provisioning / `?gen=N` | `WebhookTokenGen`/`WebhookPublicURL` wired (`cmd/gonk-intake/main.go`) | dep OK; needs gitlab-ce |
| P2-4 single `assignee_id` (CE) | onboarding MR render (`pkg/intake/render.go`, `onboard.go`) | dep OK; needs gitlab-ce |
| P2-5 webhook TLS `enable_ssl_verification` | `GONK_WEBHOOK_SSL_VERIFY`; chart `gitlab.caCert.existingConfigMap: trust-bundle` | dep OK; needs gitlab-ce |
| P2-6 hook auto-disable / 200-on-drop | intake webhook handler | dep OK; needs gitlab-ce |
| P2-7 full onboarding scenario | `pkg/intake/onboard.go` + state machine | dep OK; needs L3 |
| P2-8 kill/dedup on `BeadAnchor` | §3 — BeadAnchor idempotency contract present | **dep OK** |
| P2-9 intake↔meter seam (422, DELETE-deletes-key, kill switch) | §1/§3 — all three present | **dep OK** |
| P2-10 quiet hours end-to-end | `DecideResponse` defer+`retry_after`; schedule via `opercfg`; testclock crosses the window | **dep OK** |

### From Plan 03 (P3-1 … P3-10)

| # | Depends on | Status |
|---|---|---|
| P3-1 real LiteLLM admin calls | `admin_http.go` EnsureKey/RotateKey/DeleteKey; spend via `/spend/logs/v2` | **dep OK** (Δ: v2) — needs real LiteLLM (L2) |
| P3-2 `1mo` boundary semantics | L2 measurement; no code gap | needs L2 |
| P3-3 spend-log lag vs `max_spend_staleness` | `operatorConfig.meter.max_spend_staleness: 5m` | needs L2 measurement |
| P3-4 synthetic price agreement | rung `synthetic_usd_per_1m_tokens` vs LiteLLM price | needs L2 |
| P3-5 USD door on local-only exhaustion | rung/budget + real proxy | needs L2 |
| P3-6 dedicated admin key | live LiteLLM | needs L2 |
| P3-7 exhaustion ⇒ defer (spec 11.5) | `DecideResponse` defer+retry_after | **dep OK** |
| P3-8 catalog drift = 0 | `gonk_meter_catalog_drift_total` | **dep OK** |
| P3-9 reservation race end-to-end | `postgres_race_test.go` + `gonk_meter_reservation_races_lost_total`; **Postgres-only** (D2) | **dep OK** (single backend) |
| P3-10 real wire vs real client | `pkg/meterapi` client ⇄ `internal/meter/service` (L1) | **dep OK** |

---

## 9. Execution-order recommendation for Tasks 1–9 (given the deltas)

1. **Task 1 — `test/stubmodel`.** Unchanged, zero repo deps, **build first**. No delta touches it.
2. **Task 2 — `test/harness` (runtime/doctor/wait/ports/creds/clock).** Build next. **Amend `creds.go`** to also mint the **ed25519 write-auth keypair** (D1) — it is now a first-class runtime credential alongside the GitLab PAT. `clock.go` writes the `GONK_TESTCLOCK_FILE` offset (HB-3, confirmed).
3. **Task 3 — `test/corpus` + `test/ledger`.** Unchanged; `ledger/assert.go`'s three-way view (stub log ⇄ LiteLLM `/spend/logs/v2` ⇄ meter cost API) still holds.
4. **Task 4 — L1 integration.** Highest value, no containers, closes P3-10/P2-8/P2-9 in-process. Real `meterapi` client ⇄ real service confirmed wireable. Do this before any container work.
5. **Task 5 — L2 component.** **Collapse `ledgerdb.go` to Postgres-only** (D2): drop the dual-backend loop and any `GONK_LEDGER` reference; set `GONK_METER_STORE_BACKEND=postgres`. Target `/spend/logs/v2`. This is where P3-1..P3-9 get answered.
6. **Task 6 — kill framework + L2 kill matrix.** Unchanged in shape; add a Postgres-restart variant of the race (P3-9), no Dolt variant.
7. **Task 7 — L3 e2e (cluster + gitlab-ce + chart).** **Most-changed task.** New must-dos: (a) mint + wire the **grant keypair** (`gascity.writeAuth.verifyKey` public, `secrets.gcWriteKey` private, `GONK_GC_WRITE_KEY_FILE` for intake+gate); (b) target the **grant-gated [api] on :9443**, not :8372; (c) expect controller `workload: deployment/Recreate`, `delivery: prebaked`; (d) ledger `postgres.mode` (cnpg/external), never `shared`. Un-skip criterion for the egress test unchanged (Cilium). Note the image-gate caveat (`gonk-fsl`): the controller image was still maturing — confirm the pod actually boots before trusting a dispatch assertion.
8. **Task 8 — L3 budget suite.** Depends on Task 7's grant wiring being real, and on HB-2 (`/admin/spend/sync`) — if a gate mysteriously classifies `infra-failed`, check HB-2 first (it's satisfied, so a failure there is a wiring bug, not a missing endpoint).
9. **Task 9 — docs/gates/re-validation.** `TestProductionMeterImageHasNoTestClock` is enforceable (§6). `make secrets-scan` must now also catch a leaked **ed25519 private key** (D1), not just PAT/JWT shapes. Fold this reconciliation's deltas into ADR-006.

**Net new work the plan has no lines for:** the ed25519 grant provisioning in the
harness (D1) and the Postgres-only collapse (D2). Everything else is a rename or a
citation fix. No hand-back is missing, so no suite is forced to sleep.
