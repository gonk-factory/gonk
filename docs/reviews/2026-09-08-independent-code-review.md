# gonk: independent code review

- **Date:** 2026-09-08
- **Scope:** the whole `gonk` monorepo at `76df825` (branch `main`), read against
  its own stated goals: `docs/superpowers/specs/2026-07-12-gonk-stack-design.md`
  (§2 goals, §11 milestones), `PLAN.md`, the later specs/ADRs, and the beads
  tracker (`.beads/issues.jsonl`, 197 rows, 104 open).
- **Method:** eight parallel reviewers, one per subsystem, each asked the same
  three questions (claimed vs delivered; over-engineering; defects), each
  required to cite `path:line`. Every P0/P1 finding below was then re-verified by
  hand against the code. The default gate (`go build ./... && go vet ./... &&
  go test ./... -count=1`) is **green** on this box. The checkout is a shallow
  clone (50 commits, 2026-09-05..07), so history claims could not be re-checked.
- **Companion document:** `docs/reviews/2026-09-08-delivery-plan.md` turns
  these findings into sequenced tasks with exit criteria.

Finding IDs (`R-nn`) are stable and are what the delivery plan references.

---

## 1. Executive summary

1. **v1 has been demonstrated, never delivered.** The walking skeleton
   (invite bot → onboarding MR → merge → issue → triage comment) has produced a
   real triage comment exactly twice, as one-off live runs (2026-07-26 in the
   formula shape, 2026-09-07 in the broker shape). The milestone bead
   (`gonk-712`) and its proof (`gonk-dxo`) are still open, and four P0/P1
   reliability bugs sit on the path (`gonk-alw`, `gonk-bvy`, `gonk-njz`,
   `gonk-w41`). Nothing automated exercises the flow end to end.
2. **The parts that are built are mostly well built.** Config precedence,
   the rung decision function, webhook hardening, Gate-1/Gate-2 "always decide",
   the zero-LLM outcome classifier, prompt-by-reference, and the LiteLLM key
   provisioning are real, wired, and tested against expected values. Unit test
   quality is high. The problem is not craftsmanship; it is that **a third of
   the code is dead, a third of the tests never run, and the security posture
   rests on a control that is not enforced.**
3. **Dead code (~4.5k LOC) is shipped and documented as delivered:** the whole
   formula layer (`pack/formulas`, `[steps.check]`, `gonk-gate check`,
   `control-dispatcher`, `prompt.template.md`), `pkg/verify` (zero importers),
   `gonk-gate trailers` + the commit hook (never fires in a pod),
   `gcapi.SubmitSession`, `beadstore` as a JSON-in-comments KV store, and the
   entrypoint's marker-parsing and static-key fallbacks. ADR-007 already says to
   delete the formula layer; it has not happened, and the stale beads it leaves
   behind generate permanent pool demand (`gonk-p2e`).
4. **Four suites that carry the load-bearing claims have never run in any CI:**
   `-tags component` (the real LiteLLM hard door, meter SIGKILL mid-race),
   `-tags integration` (Postgres `ReserveIfFits` atomicity), `-tags images`
   (provider allowlist, non-root, pins; currently RED on `main`), and
   `-tags live`. 168 test functions vs 779 in the default gate. Spec §10.4's
   "deterministic e2e on every merge" does not exist: `test/e2e/` contains one
   hand-written report and zero Go files.
5. **Security: three P1 holes, all new.** (a) Any in-cluster pod can register a
   rig grant for any project the bot PAT can read and download the tree
   (`POST /rig/{alias}` is unauthenticated by design). (b) Agent-proposed
   labels escape the `gonk::` namespace via commas and can mint the broker's
   own "trustworthy" audit labels. (c) Agent-proposed comment bodies are posted
   verbatim under the bot PAT, so GitLab quick actions (`/close`, `/assign`,
   `/confidential`) in a prompt-injected batch execute. All three assume a
   NetworkPolicy that the repo itself documents is not enforced (`gonk-dku`).
   (d) Dolt runs as `root@%` with no password, so the beads store is
   rewritable from any pod. And the policies themselves are wrong for the
   bundled topology: the day a policy controller lands, the controller cannot
   reach meter or intake and intake cannot reach the supervisor, so nothing
   spawns.
6. **Reliability: the orphan reaper cannot match any session gonk now
   creates** (regex predates the nonce suffix), so the scheduler-wedge failure
   it exists for (`gonk-xkm`) is back. The agent entrypoint aborts before its
   first log line if `GC_ALIAS` is unset (`set -u`), and a single transport
   blip on the prompt fetch burns a ladder attempt.
7. **Budget truth is softer than the docs say.** LiteLLM's hard ceiling is
   set only when *both* cost and token ceilings are finite; the spend poller's
   2-minute overlap can miss calls longer than 2 minutes (local models routinely
   are); `spend_rows` has no retention and is full-scanned on the hot path;
   per-project TPM/RPM limits are never set; `max_turns` never leaves the
   meter process; plaintext virtual keys accumulate forever in the `prompts`
   table.
8. **Three duplicated control loops** reconcile LiteLLM keys against each
   other (meter `reresolve` 5m, meter `reconcile-keys` 1m, intake resync
   re-PUT), and that churn is the trigger surface for the P0 duplicate-key wedge
   (`gonk-bvy`). Budget arithmetic is triplicated across `rung.Decide` and the
   two `ReserveIfFits` implementations.
9. **Scope has outrun the skeleton.** Six specs beyond v1 (triage broker,
   verified change pipeline, source beads, buildkit, trajectory evaluation, Warp
   parity with a 6-phase roadmap and 28 child beads) total ~3,200 lines of
   design. Two of them self-assess as "not ready" or "presupposes an agent that
   does not exist"; one (trajectory) shipped a slice before v1 closed.
10. **The repo is not publishable as-is** (spec goal 8): ~800 lines carry
    homelab hostnames, RFC1918 addresses, a GPU host IP, Vault paths, GitLab
    project/user IDs, the owner's local filesystem path, and an unresolved
    "bot PAT printed in plaintext" note; the open beads are a vulnerability
    inventory. `gonk-73dh` already says this; it is restated because the GitHub
    mirror and a v0.1.0 release already exist.

**Bottom line.** gonk is a serious, unusually well-documented attempt whose
documents are more honest than its status table. The fastest path to its own
goals is not more design: it is (1) delete what ADR-007 already condemned,
(2) run the suites that already exist, (3) close the three P1 trust-boundary
holes and the reaper, and (4) automate the one scenario that defines v1.

---

## 2. Stated goals vs. delivered

Status vocabulary: **Done** (implemented, wired into a real binary/chart path,
tested for the property); **Partial**; **Demo** (worked by hand at least once,
no automated evidence); **Not done**; **Ornamental** (parsed/typed, consumed by
nothing).

### 2.1 Master spec §2 goals

| # | Goal | Status | Evidence |
|---|---|---|---|
| G1 | Single Helm chart onto an existing cluster, one bot user | **Demo** | Installed by hand once (`test/e2e/L3-real-gitlab-findings.md`); no `helm install` in any CI job; kind never used (`docs/environment.md:46`) |
| G2 | Renovate-style opt-in: invite → deterministic onboarding MR → merge enables | **Partial** | MR half works (`pkg/intake/onboard.go`); post-merge half never has: `pending` gates triage on `.agent/`, scaffold sits on the dead formula path (`gonk-bgx`, `gonk-msz`) |
| G3 | v1 = issue triage + in-thread follow-up + scaffold MR | **Demo** | Triage comment twice by hand; `@gonk` mention path silently dead (`gonk-ecn`; §3.2, §3.4); scaffold never produced an MR |
| G4 | Token/cost attributed at every granularity; hard budgets per project/group/instance | **Partial** | Per-project virtual keys, `/decide` reservations, cost API: Done. Hard door has a hole (R-14); ledger "join with event bus → bead → GitLab artifact" not built (§3.3); group/instance tiers are meter-side soft ceilings only (`gonk-ay87`) |
| G5 | Local-first, deterministic ladder, defer semantics | **Partial** | `rung.Decide` Done and exhaustively tested. "Local-only" not enforceable: no NetworkPolicy controller (`gonk-dku`); opencode fail-open fixed 08-30 |
| G6 | Project state lives in the project | **Done** (with an ornamental field) | `continuity: resume|fresh` is parsed and consumed by nothing (`pack/pack.toml:75` hard-codes `resume`) |
| G7 | Trustworthy by construction: stub-model e2e, contract-tested schemas, observability, audit | **Not done** | No e2e suite; zero otel code in `cmd/ pkg/ internal/`; schemas exist for config/operator only (no atags/order-name schema); image suite never runs |
| G8 | Open-sourceable: no deployment-specific material | **Not done** | §7; `gonk-73dh` |

### 2.2 Master spec §11 validation milestones

| # | Milestone | Automated evidence | Manual evidence |
|---|---|---|---|
| M1 | Helm install on kind and real cluster; idle = zero agent pods | **None.** `netpol.yml` stands up kind but applies rendered policies to agnhost stand-ins, never `helm install` | Real cluster: yes. Idle-zero is violated by stale formula beads generating pool demand (`gonk-p2e`) |
| M2 | Onboarding MR e2e vs real GitLab | Only against `glabtest` | Yes (L3 report) |
| M3 | Triage e2e with stub, then bailey | Stub: **never.** L1 `SyntheticSession` proves decide→LLM→outcome money path but asserts no comment lands | bailey: two one-offs |
| M4 | Session resume across pod recreation | **Zero tests**; no bead; never attempted | — |
| M5 | Attribution visible in Grafana; exhaustion → `defer` | Dashboard JSON validated (chart tag, GH only); exhaustion→defer at L1; hard door at L2 (never runs) | Attribution was broken until 09-06/07 (`gonk-8gb`, `gonk-1zm`) |
| M6 | Kill tests: dolt/LiteLLM/GitLab restart, no lost work, no dup comments, no infra escalation | One L2 test (meter SIGKILL mid-race), never runs in CI | The outages happened in production instead (`gonk-alw`, `gonk-bvy`) |

### 2.3 PLAN.md plans

| Plan | PLAN.md says | Actually |
|---|---|---|
| 01 foundation/config | done | Done. |
| 02 gitlab-intake | done | Done, with the defects in §3.2. |
| 03 gonk-meter | done | Done for `/decide`, keys, cost API; not done for ledger join, TPM/RPM, `max_turns`, admin-key rotation slot, `config_hash` no-op (§3.3). |
| 04 pack & images | done | Formula half is a corpse (ADR-007); images are built but their test suite has never run and is red. |
| 05 chart | done | `PLAN.md:772,798` still say "Plan 05 has not started". See §3.6. |
| 06 e2e harness | not started | Stubmodel, harness, corpus, ledger, L1 and L2 landed in `6a603f9`; the e2e scenario and kill framework did not. |

### 2.4 Later specs

| Spec | Self-assessed status | Reviewer's reading |
|---|---|---|
| Triage broker (07-27) | G1–G4 shipped; G5 (identical output, no PAT in pod, safety cases) open (`gonk-dxo`) | Built and live. Shape gate is cardinality-only (R-02, R-03). |
| Prompt-by-reference (08-01) | plan says T1–T6 "not started" | Implemented and live (`entrypoint.sh:193-268`); plan doc stale; `gonk-mzd` in_progress, `gonk-e9m` still open |
| Verified change pipeline (08-02) | "design note… presupposes a code-writing agent that does not exist" | `pkg/verify` exists with zero importers |
| Source beads (08-10) | "NOT READY TO IMPLEMENT"; review found half its claims wrong (`gonk-84m`) | Only the issue-open sweep shipped (`gonk-vrf`) |
| Buildkit migration (08-18) | "DESIGN ONLY" | Off the critical path |
| Trajectory evaluation (09-02) | "design note, not a plan" | Slice 1 shipped; `GONK_ENFORCE_TRAJECTORY` set nowhere → observe-only; `Input.Target` never populated |

---

## 3. Findings by subsystem

Each subsection is condensed from the reviewer's report; only items that
survived hand verification or are marked PLAUSIBLE are kept.

### 3.1 Configuration & policy (`pkg/gonkcfg`, `pkg/opercfg`, `pkg/atags`, `pkg/budget`, `pkg/rung`, `pkg/spend`)

**Delivered and good:** schema + published copy + sha256 drift pins
(`pkg/gonkcfg/drift_test.go:18-61`, same in `opercfg`); `Resolve` with
tighten-only budgets and ladder intersection (`resolve.go:82-108`), one real
call site (`internal/meter/service/service.go:383`); `rung.Decide` pure with
clock as input, 33-row table plus 19 mutation saboteurs; `defer`/`deny`
semantics; operator config validated at startup with hot reload.

**Not delivered / ornamental**

- `max_turns` per rung: computed (`decide.go:285-287`), dropped at the HTTP
  boundary (`http.go:260-273`), absent from `meterapi.DecideResponse`, and
  Gas City has no turn cap (`pack/agents/triage/agent.toml:68-74`). Spec §6.3's
  "within turn caps" gate is enforced nowhere. `gonk-xcp` was closed as done.
- `continuity`, `actions.pipelines`: parsed, unused.
- `triage.respond_to_mentions`: enforced only in intake
  (`pkg/intake/state.go:121-125`), not at `/decide` as `decide.go:69-72` claims.
- "JSON Schema for attribution tags" (spec §10.1) does not exist.
- `fuzz_test.go:13-16` claims 60s of fuzzing "in the standing gate"; no
  `-fuzz` anywhere in Makefile or CI.

**Defects**

| ID | Sev | Finding | Where |
|---|---|---|---|
| R-20 | P2 | Operator `schedule.quiet_hours` without `timezone` passes `opercfg.Load`; `ParseQuietHours("")` then errors for **every** project → `invalid()` → `deleteKeyIfAny` + 422. One ConfigMap edit + hot reload deletes all virtual keys next tick | `opercfg.go:376-378`, `service.go:352-394`, `outcome.go:88-91` |
| R-21 | P3 | DST off-by-one: `QuietHours.EndAfter` uses elapsed-since-midnight, not wall clock (probe: `22:00-07:00` on 2026-11-01, 06:30 EST not quiet) | `outcome.go:118-127` |
| R-22 | P3 | `Schedule` replaced as a unit: a project setting only `quiet_hours` loses the instance timezone → 422, key deleted; setting only `timezone` erases instance quiet hours | `resolve.go:98-101` |
| R-23 | P3 | `Effective.Schedule` not persisted; after restart `GET /v1/projects/{p}` reports `schedule: null` while quiet hours are enforced | `store/postgres_wire.go:35-44` |
| R-24 | P3 | Group keys ending in `/` are schema-valid and never match (`HasPrefix(project, g+"/")`) — a silently dead kill switch | `opercfg.go:389` |
| R-25 | P3 | Lost-race defer always labelled `monthly-cost-exhausted` even when the token leg lost | `service.go:685-693` |

**Over-engineering:** `foldPolicy` (70 LOC) reimplements `Resolve` (112 LOC);
budget-fits logic triplicated (`rung.Decide`, `postgres.ReserveIfFits`,
`memory.ReserveIfFits`, with "kept byte-consistent with" comments); four shapes
of Effective/Budget with two independent `+Inf`→`null` encoders; `pkg/budget`
exists only to serve `rung` and the store; `rejectNonFinite` duplicated;
`CloudAllowance{Enabled bool}` plus an "EXTENSION POINT" essay for one boolean.

### 3.2 GitLab intake (`pkg/glab`, `pkg/ghook`, `pkg/intake`, `cmd/gonk-intake`)

**Delivered and good:** constant-time multi-slot token check before body read;
1 MiB cap; bounded dedupe; fail-closed handler construction; reconciliation
(memberships → `.gonk.yml` → hook → meter registration → issue sweep with cap 5
and 24h recency); onboarding MR with decline/re-invite semantics and the
Reporter-role issue fallback; state machine; Gate-1 `/decide` + `MayFire`
(mechanically compared against Gate 2); deny label; self-repo blocklist;
two-listener server; file-mounted secrets; PAT rotation; no
`InsecureSkipVerify`. `HTTPDispatcher` is already a thin wrapper over
`pkg/gcapi` — `PLAN.md:90-91` and `:632-639` are stale.

**Not delivered:** the instance-level `user_add_to_team` system hook (spec
§5.2); "rig registration" is meter registration only — nothing creates a Gas
City rig; the `@gonk` mention path fires an order that cannot run (§3.4) and
opens a meter reservation each time; `OrderRequest.NoteID`/`MRIID` never
populated; note/reopen events dropped in the unsynced window are not recovered
(`gonk-pjoo`).

**Defects**

| ID | Sev | Finding | Where |
|---|---|---|---|
| R-15 | P2 | Archived project never deregistered from meter: `seen[p.ID]` is set before `reconcileProject` returns early on `Archived`, so the vanished-membership sweep skips it and the LiteLLM key stays live | `reconcile.go:509, 527-541, 683` |
| R-16 | P2 | Comment bodies sent as URL query parameters on the live broker path: >~8 KB → 414; comment text in proxy/access logs | `pkg/glab/write.go:116,153`; `broker_apply.go:292,301` |
| R-26 | P3 | Sweep loop guard is opt-out by zero value (`if r.BotUserID != 0 && …`), unlike the webhook guard which fails closed | `reconcile.go:857` vs `receiver.go:80` |
| R-27 | P3 | PAT fallback fires on 403 (a plain permission denial re-sends with the previous PAT and increments `auth_fallback_total`) | `client.go:170` |
| R-28 | P3 (plausible) | Non-idempotent POSTs (`CreateHook`, `CreateMergeRequest`, `CreateIssue`, `CreateCommit`) retried on 5xx | `client.go:168` |
| R-29 | P3 | Sweep re-fires dispatched-but-unlabelled issues every pass for 24h; intake keeps no "already fired" memory for triage (it does for scaffold) | `reconcile.go:135-145` vs `cache.go:29` |
| R-30 | P3 | `ReconcileSummary.Dispatched` counts scaffold fires only | `reconcile.go:516` |

**Over-engineering:** `pkg/glab` (~770 LOC) reimplements retry/pagination/typed
errors that `gitlab.com/gitlab-org/api/client-go` provides, and has grown
broker-side methods; three overlapping timing mechanisms for "meter not
answering" (startup ladder, `UnsettledRetryInterval`, `MeterResyncInterval`)
plus `StalenessWindow`; `Loop` runs a t=0 pass and `main.go:169-170` also
`Kick()`s.

**Test gaps:** split-credential admin token has zero behavioural tests
(`glabtest` cannot distinguish tokens); `glabtest` ignores `updated_after`
server-side; `TestReconcileOnceCallsTheSweep` is a source grep; the
service-level test drains the queue by hand and never runs the worker
goroutine or startup ladder.

### 3.3 gonk-meter (`internal/meter/**`, `cmd/gonk-meter`, `pkg/meterapi`)

**Delivered and good:** `/decide` idempotent on open reservation, attempt
derived server-side, no caller attempt field; `/outcome` bound to the minted
reservation; reservation TTL janitor; bearer auth with two slots; K8s Secret
keysink; cost API; per-project virtual key with `max_budget`; `+Inf` handled.

**Not delivered as specified**

- Ledger join "spend logs × gc event bus (SSE) → bead → GitLab artifact" (spec
  §6.2.1): no SSE, gcapi, or GitLab code under `internal/meter`. The "join" is
  the attribution tags on each spend row. That is arguably the better design;
  the spec should say so.
- "…and rate limits from resolved config": `KeySpec.TPMLimit/RPMLimit` exist
  (`admin.go:54-55`), nothing sets them.
- LiteLLM admin-key rotation slot 2 read and discarded (`main.go:108-112`).
- No-op on unchanged `config_hash` (`PLAN.md:610`): `resolveProject` always
  calls `EnsureKey`.
- "Meter is never in the request path": true for LLM calls, false for session
  start — the pod cannot start without `GET /v1/prompts/{alias}` from meter.

**Defects**

| ID | Sev | Finding | Where |
|---|---|---|---|
| R-12 | P1 (plausible) | 2-minute poll overlap: cursor = max `startTime`; a call whose generation + async log flush exceeds 2 min is never fetched → silent under-count → soft gate fails open. Local models routinely exceed 2 min/call | `service.go:44-47, 931-951` |
| R-13 | P2 | Plaintext virtual keys accumulate forever: `ExpirePrompts` has no non-test caller (verified); `TakePrompt` leaves `litellm_key` in the row | `store.go:291-294`, `postgres.go:774-777`, `service.go:1170` |
| R-14 | P2 | `MaxBudgetFor` returns nil (no LiteLLM `max_budget` at all) when **either** ceiling is unlimited — spec §6.2's "hard refusal in LiteLLM" is false for a project that sets only `monthly_cost_usd` | `litellm/admin.go:83-90` |
| R-17 | P2 | `spend_rows` unbounded and full-scanned: `Decide` loads every row for the project, the handler loads them again, `RefreshGauges` does it per project every 15s, `SessionCost` scans the whole table per sweep; inserts are one statement per row | `service.go:604, 1116-1140`; `http.go:254, 599`; `postgres.go:626-646` |
| R-18 | P2 (plausible) | First poll asks since year 1; LiteLLM v2 caps at 10k rows; `Since` never compares `Total` to fetched → cursor advances past rows never ingested | `spendsource_http.go:139-172` |
| R-19 | P2 (plausible) | Key-missing project can never be invalidated/disabled if `/key/delete` 4xx's on an unknown alias → 500 forever; `Fake.DeleteKey` silently succeeds so untested | `service.go:303-318, 342-346` |
| R-31 | P3 | Janitor flips `settled` before `RecordAttempt`; on error those reservations are never returned again | `postgres.go:596`, `service.go:1180-1184` |
| R-32 | P3 | Idempotent fast path returns `run` + `KeyRef` from an open reservation even when the project is deleted/disabled/invalid, or quiet hours began between Gate 1 and Gate 2 | `service.go:582-600` |
| R-33 | P3 | tagmint rejections mapped to 500 (comment says 400) | `http.go:245-252` |
| R-34 | P3 | Unauthenticated `GET /v1/prompts/{alias}` returns the live LiteLLM key; alias is in pod env and is logged in clear by gonk-gate (`broker_inject.go:345,663,673`) | `http.go:872-879` |

Known and still open in code: `gonk-bvy` (only `Decide` takes `s.locks`;
`Register`/`Reresolve`/`ReconcileKeys` are unserialized), `gonk-4nk`, `gonk-0qi`
(preserve branch never checks the Secret exists).

**Over-engineering:** the key travels K8s Secret → gonk-gate reads it back
(`keyread.go:86-98`) → plaintext to meter's `prompts` row → unauthenticated GET
by the pod. The Secret exists only to be read by another gonk binary;
`keysink` (~180 LOC + RBAC) can go. `seenCallIDs` (~70 LOC) reimplements the
store's CallID dedupe for counters; `ForceSpendSync` coalescing is
`singleflight`; `postgres_wire.go` (139 LOC) persists a pure function of stored
inputs; three key-reconcile loops each do generate→400→list→update per project
per tick. `contrib/litellm/*.py` is referenced by nothing; README's
`make litellm-callback-check` does not exist.

### 3.4 Gate / broker (`cmd/gonk-gate`, `pkg/{gate,gcapi,beadstore,effects,rig,trace,verify,netprobe}`)

**Delivered and good:** Gate 2 decides before any branch and never trusts order
vars (`dispatch.go:156-162`, pinned by `TestDispatchAlwaysDecidesEvenWhenVarsCarryARung`);
zero-LLM classifier with the infra-never-escalates invariant enumerated;
broker pipeline transcript → `ParseBatch` → `LoadShape` → `Validate` →
`ValidateTargets` → `ValidatePaths`; 128-bit `crypto/rand` alias; one-shot
prompt (410 on re-read); ed25519 request signing (client side; verified only
upstream — `gcapitest` accepts unsigned mutations).

**Dead or inert**

| Item | Evidence |
|---|---|
| `check` subcommand | still reads `GC_WEBHOOK_ARG_*` which `[steps.check]` never sets; only formulas invoke it; only `mention` still pours a formula and is dead |
| `trailers` + `prepare-commit-msg` | hook installed only if `RIG_DIR/.git` exists; the rig delivers a tarball; hook reads env the entrypoint never exports; broker agent never commits. 736 LOC |
| `pkg/verify` | zero non-test importers (verified). 750 LOC |
| `gcapi.SubmitSession` | no non-test caller since prompt-by-reference. 251 LOC |
| `pkg/trace` fifth gate | `GONK_ENFORCE_TRAJECTORY` set nowhere in chart (verified) → observe-only; `Input.Target` never populated so `require_target_read_for` is dead by construction (`effect-shape.toml:56` admits it) |
| `discussion_id` | intake sends it; `dispatchArgs` has no field; mention formula still carries the KNOWN GAP comment |

**Defects**

| ID | Sev | Finding | Where |
|---|---|---|---|
| R-01 | **P1** | Orphan reaper regex `^gonk\.…\.a\d+$` predates the nonce suffix every broker alias carries → matches nothing gonk creates (verified: `reap.go:43` vs `broker_inject.go:87-94`). Every orphan (order timeout, `Put` failure after failed `abandonSession`, dedupe-window duplicate) leaks forever — the `gonk-xkm` wedge. All 10 reaper tests use old-style aliases | `reap.go:43` |
| R-02 | **P1** | Label effects escape the namespace: `AddIssueLabel` sends `add_labels=<v>` which GitLab splits on commas; `normaliseLabel("bug,security::critical")` → `gonk::bug,security::critical`. Also `normaliseLabel("gonk::fix-queued")` passes unchanged, so the agent can mint the labels `broker_verdict.go:9-13` calls trustworthy | `broker_label.go:32-46`; `write.go:98-103` |
| R-03 | **P1** (plausible) | Comment bodies posted verbatim under the bot PAT; GitLab executes quick actions (`/close`, `/label`, `/assign`, `/confidential`, `/due`) in Notes-API notes. No stripping anywhere (verified); no body size cap | `broker_apply.go:285-301`; `effects.go:35` |
| R-04 | **P1** | `POST /rig/{alias}` is unauthenticated (by design, `rig/http.go:26-36`); any in-cluster pod can register a grant for any `project_id`/`ref` and `GET` the tree. Relies on NetworkPolicy that is unenforced; even if enforced, agent egress to intake:9090 is allowed while intake ingress admits only Prometheus, so enforcement breaks the checkout (`gonk-559l`). Grant is re-fetchable for 30 min | `rig/http.go:30-34, 100-123` |
| R-05 | P1 (latent) | `claimedSessionIDs` builds the "live" set from `List(StateRunning)` which truncates at bd's default 50 (`gonk-kx3`); above 50 running beads the reaper closes live sessions. Unreachable today only because of R-01 | `reap.go:126-138`; `bd.go:90,220` |
| R-10 | P2 | Transcript reads hard-fail above 64 KiB; the prompt now orders directory listing and code reading, so real transcripts exceed it → `ArtifactUnknown` → infra-failed → refire until `max_infra_retries` with a valid batch unread | `transcript.go:72-81`; `client.go:186,276,304-313` |
| R-11 | P2 | Dedupe by attempt: record is `Put` only after the prompt fetch, so a re-dispatch inside the ≤240s window mints a second session and the second `Put` overwrites `SessionID`, orphaning the first (compounded by R-01) | `broker_inject.go:572-585, 740-743` |

**Over-engineering:** Gas City is used as a pod launcher plus transcript reader
— formulas/convergence bypassed, the event bus used for one correlation, orders
are two exec wrappers of one binary; `sweep.go` is a hand-rolled convergence
loop and `beadstore/bd.go` (241 LOC) is a JSON KV store implemented as
`<!-- gonk-state {json} -->` comments plus labels, five subprocess calls per
`Put`, non-atomic. The meter already has Postgres. Two meter HTTP clients
(`cmd/gonk-gate/meter.go` vs `pkg/intake`). `gcapitest/server.go` (959 LOC) is
a second Gas City whose fidelity is never checked against upstream.

### 3.5 Pack & images (`pack/`, `images/`, `internal/packtest`, `test/images`, `test/entrypoint`)

**Delivered and good:** prompt-by-reference fetch + `opencode run`
(non-interactive) is the live path; `enabled_providers` allowlist plus the
`opencode models` assertion is fail-closed and not bypassable from the prompt;
lifecycle log tees to pid 1; redaction test with negative control; jq-rendered
attribution header; digest-pinned bases; non-root.

**Not delivered / stale**

- Spec §4.3's flow (formula → `.agent/` loader → agent posts) is superseded by
  the broker; the spec was not updated; no `.agent/` loader exists.
- ADR-007 says delete `pack/formulas/` and `[steps.check]`; all three formulas,
  their orders, `gonk-check.sh`, `prompt.template.md` (used by no code path)
  and `control-dispatcher` (execs `gc`, which the agent image does not ship)
  are still baked into the controller image.
- `pack/doctor/meter-reachable/run.sh` needs `curl`; the controller image has
  none → guaranteed false negative. Nothing invokes `gc doctor` anyway.
- `GONK_LITELLM_KEY_FILE` is set for the controller only; the pod always
  materialises the key from the prompt HTTP body into `/tmp/gonk/llkey`. The
  static `GONK_LITELLM_KEY` fallback branch is dead. `PLAN.md:238-240`
  ("file mount, never read into env") is no longer true.
- `agent.toml` still declares `prompt_mode="flag"` and the entrypoint's last
  line is `exec opencode "$@"` (interactive TUI) whenever no prompt URL arrives.

**Defects**

| ID | Sev | Finding | Where |
|---|---|---|---|
| R-06 | **P1** | `set -eu` + `${GC_ALIAS#??????}` at line 68: an unset `GC_ALIAS` aborts before the first log line, into a detached tmux pane — no "session start", no "session end". Breaks spec §8.1's guarantee and the (never-run) image suite (`agent_smoke_test.go:415-423` sets no `GC_ALIAS`) | `entrypoint.sh:34, 68` |
| R-07 | P2 | Prompt fetch does not retry transport failures: `curl … -w '%{http_code}' \|\| echo 000` yields `000000`, the `*)` arm breaks, `exit 4` after one refused connection despite the 120s window; dispatch re-slings a fresh attempt → a startup DNS race burns ladder attempts | `entrypoint.sh:218-231` |
| R-08 | P2 | Mention path still sends `litellm_key` and `bot_token` as order vars through the unauthenticated supervisor route onto the bead before dying | `dispatch.go:319-321` |
| R-09 | P2 (plausible) | The virtual key is `cat`-able by the model (`/tmp/gonk/llkey`, overlay `opencode.json`, permission `{"*":"allow"}`) and nothing scrubs effect bodies; an injected issue can get the key posted publicly. Blast radius = that project's budget | `entrypoint.sh:363-371`; `pkg/effects` |
| R-35 | P3 (plausible) | HTML-comment markers `<!-- gonk:model:… -->` anywhere in the prompt (which embeds the raw issue body) select model/metadata when the row's fields are empty; nothing asserts they are non-empty pod-side | `entrypoint.sh:270-278, 310-323` |
| R-36 | P3 (plausible) | Repo-supplied `opencode.json`/`.opencode/` in the checkout may override model (`gonk/<pricier>` passes the `^gonk/` check) or load plugin code. Not traced in opencode 1.18.3 source | `entrypoint.sh:475` |
| R-37 | P3 | Redaction test's forbidden list omits `_clean`; `_pkey` never unset | `lifecycle_log_test.go:33`; `entrypoint.sh:268` |

### 3.6 Helm chart (`chart/`, `internal/charttest`, `internal/buildgate`)

Environment note: the reviewer built helm 3.16 locally; 44 charttest tests
fail on it because `charttest.go:173` passes no `--kube-version` against the
chart's `kubeVersion: ">=1.28.0-0"`, and two more assert helm 3.19's error
message format. CI pins 3.19 so it is green there; the README does not say the
local gate needs it (R-55). All eight `ci/` profiles render and kubeconform
clean.

**Delivered and good:** the whole factory renders from defaults and cannot
spend (`monthly_cost_usd: 0`; empty ladder refuses to render; a cloud rung
with zero price is rejected); no `Secret` objects, no `secretKeyRef`, all file
mounts with two rotation slots; every guard tried with `--set …=null` fails
closed; CNPG `Cluster` CR seam; Dolt bundled/external; LiteLLM BYO; all 26
dashboard metric names exist in Go; chart version seal (`internal/buildgate`)
runs in the default gate and does catch the "Flux never deploys it" failure;
netpol enforcement probe as a helm test plus a GitHub kind matrix
(kube-router, calico).

**Not delivered / stale**

- `monitoring.otlpEndpoint` → `OTEL_EXPORTER_OTLP_ENDPOINT` on meter and
  intake; zero consumers anywhere in `cmd/ pkg/ internal/ go.mod` (verified).
  Spec §8's "OTLP traces stitched webhook→order→session" exists in no binary.
- ServiceMonitors for meter and intake only (spec §8: "every component");
  no `gonk_*session*` series is registered anywhere, so the "sessions
  spawned/resumed/retired" panels cannot exist.
- `postgresql.mode: bundled` (spec §7.4) does not exist; the default
  `ledger.postgres.mode=shared` renders nothing for the ledger and needs a
  4-object out-of-band recipe (`README.md:186`, G19).
- `gonk-7oz` (agent egress to whole GitLab namespace) is fixed in the chart
  (`values.yaml:621`) but the bead is still open.
- Private CA never reaches agent pods (`gonk-qfk` confirmed open).
- `keysink/k8s.go:25` says "key material NEVER travels through meter's HTTP
  responses" — false since prompt-by-reference.

**Defects**

| ID | Sev | Finding | Where |
|---|---|---|---|
| R-46 | **P1** | The bundled controller cannot function under an enforcing CNI: its egress policy allows DNS, Dolt and GitLab only (verified), but gonk-gate in that pod must reach meter (`GONK_METER_URL`) and intake:9090 (`GONK_RIG_BASE_URL`), and intake's ingress admits only traefik/monitoring. The day a policy controller lands: no `/decide`, no grant, no checkout, no session | `networkpolicy-gonk-controller.yaml:29-57`; `networkpolicy-gonk-intake.yaml` |
| R-47 | **P1** | Intake's supervisor egress rule targets `networkPolicy.gascity.namespaceSelector` (an external namespace) on 8080; the bundled controller lives in `.Release.Namespace` on 9443 (verified). Under enforcement every dispatch fails. The test checks the URL, not the policy | `networkpolicy-gonk-intake.yaml:78-86` |
| R-48 | **P1** | Dolt is `root@%`, no password, no TLS (verified: `DOLT_ROOT_HOST: "%"`), and the "controller only" ingress is unenforced. Any pod, including an agent pod running attacker-influenced issue text, can `mysql -h gonk-dolt -u root` and rewrite the beads store. Not tracked by any bead | `statefulset-gonk-dolt.yaml:48-59` |
| R-49 | P2 | LiteLLM **admin** key is mounted into the controller pod (verified) and nothing there reads it since `gonk-8gb`; the same pod holds the bot PAT and forwards it into order vars, runs `pods/exec`, and sweeps agent output — the highest-value credential in the system sitting unused in the most exposed pod | `workload-gonk-controller.yaml:309-310, 429` |
| R-50 | P2 | `componentImages.agent` is absent from `values.schema.json` and has no guard (verified); `--set componentImages.agent.tag=latest` or `""` renders `GC_K8S_IMAGE: ".../gonk-agent:"` and fails at first spawn, not at render | `workload-gonk-controller.yaml:257` |
| R-51 | P2 (plausible) | `gc-agent` has `pods get` namespace-wide with the SA token automounted; one agent can read another agent pod's env, and `GC_ALIAS` is the sole capability for the prompt+key GET and the checkout. One-shot narrows the window to the race before the victim fetches | `role-gc-agent.yaml`; `serviceaccount-gc-agent.yaml` |
| R-52 | P2 | `gc-controller` has unrestricted `secrets get`: it can read `gonk-webhook`, `gonk-ledger`, and every `gonk-key-*` | `role-gc-controller.yaml:48` |
| R-53 | P3 | Intake runs as the namespace `default` SA with its token automounted while terminating attacker-reachable webhooks | `deployment-gonk-intake.yaml` |
| R-54 | P3 | Top-level `values.schema.json` has no `additionalProperties: false`; a typo (`secretz.foo`) renders clean and can silently disable a security toggle | `values.schema.json` |
| R-55 | P3 | charttest is helm-version-coupled (no `--kube-version`; asserts 3.19 message formats); README does not say so | `charttest.go:173`; `guards_test.go:202`; `toggle_test.go:196` |
| R-56 | P3 | `factory.json:36` groups `gonk_intake_webhook_total` by `result`; the label is `outcome` — panel renders one unlabeled series. The dashboard test checks names from a hand-copied list, not labels, not the Go registry | `chart/gonk/dashboards/factory.json:36`; `monitoring_test.go:152` |

**Over-engineering:** golden manifests are 15.6k lines / 672 KB across eight
profiles that mostly differ by one toggle, and they snapshot comments (which is
how `192.168.1.142` ends up in `golden/values-byo-gascity.yaml`); `values.yaml`
is 701 lines, ~85% prose that duplicates ADR-006; `charttest` (2.7k LOC)
reimplements helm-unittest and a third of its tests are substring checks a
comment would satisfy (`netpol_test.go:88`, the author notes the trap at
`:80-83`); dead values (`monitoring.otlpEndpoint`, `gascity.delivery`
single-value enum, `ledger.postgres.shared.*` with zero template references,
`ledger.postgres.port` read via `default 5432` but absent from values/schema).
Seal + goldens + eight lint profiles + kubeconform are four gates on one
artifact with one consumer; the seal is the one that matters. Nothing here
should become a chart `dependencies:` entry — the chart is not duplicating
upstream charts, it is over-documenting itself.

**Test gaps:** no test asserts that each component's *egress* policy is a
superset of what its binary dials (R-46/R-47 slipped past 21 netpol tests);
`TestAgentEgressIsLimitedToGitLabLiteLLMAndDNS` still says GitLab in its name
while asserting the opposite; nothing asserts the controller's mounted secrets
are each read by something (R-49); the un-skipped egress-denial test the
README promises does not exist in this repo.

### 3.7 Tests & CI

**What runs where**

| Suite | Tag | GitLab CI | GitHub CI | Ever run? |
|---|---|---|---|---|
| Unit + L1 integration (`test/integration`) | none | yes | yes | yes, green |
| Chart golden + helm lint + kubeconform | `chart` | gated on undefined var | yes | GH only |
| L2 component: real LiteLLM + Postgres + stub; hard door; meter SIGKILL | `component` | no | no | **never** |
| Postgres store conformance + `ReserveIfFits` race | `integration` | no | no | **never** |
| Image smoke: provider allowlist, non-root, pins, pack accepted by real `gc` | `images` | no | no | **never; RED on main** (`nolatest_test.go` 10 false positives; R-06 also breaks it) |
| Live drift | `live` | no | no | never; 3/3 `t.Skip` → PASS |
| Deterministic e2e (spec §10.4) | — | — | — | does not exist |

**Defects**

| ID | Sev | Finding | Where |
|---|---|---|---|
| R-38 | P1 | `make no-latest` converts a grep *error* into a pass (`! grep … 2>/dev/null \|\| …`; grep exit 2 → target succeeds). The comment above it says this class was fixed | `Makefile:174-178` |
| R-39 | P1 | The Go no-latest gate with real coverage is red and invisible (§ above) | `test/images/nolatest_test.go:104-121` |
| R-40 | P2 | `-tags live`/`-tags images` are green when nothing runs (skips); a future CI job "adding the tag" would look green with no images present | `agent_smoke_test.go:93`, `servers_smoke_test.go:30,48,67` |
| R-41 | P2 | `KUBECONFORM_SHA256` echoed, never compared; header says "everything is pinned" while 24 actions float on major tags | `.github/workflows/ci.yml:99-115` |
| R-42 | P2 | `v0.1.0` tag re-cut (runs #4/#5 on different SHAs) | GH release history |
| R-43 | P2 | `.golangci.yml` has no `build-tags`; ~10k LOC of tagged tests are never linted | `.golangci.yml` |
| R-44 | P3 | `make lint` takes any host `golangci-lint` regardless of version; `make gate` is unrunnable here (v2.5.0, exit 3) | `Makefile:263-266` |
| R-45 | P3 | Trivy: GitLab `image-scan: when: never`; GH `fail-build: false` | `.gitlab-ci.yml:459`; `release.yml:203-208` |

**Test quality:** sampled unit tests assert expected values, not
well-formedness; helper-driven assertions are real. Placement, not quality, is
the problem. `test/harness/doctor.go` (354 LOC) is a preflight for an L3 that
does not exist; `hack/require_image_tags.py` and `registry_prune.py` are wired
into nothing; two no-latest gates disagree.

---

## 4. Defects ranked (cross-cutting)

| ID | Sev | One line | Slice |
|---|---|---|---|
| R-01 | P1 | Reaper regex matches no live alias; orphans leak forever | gate |
| R-02 | P1 | Agent labels escape `gonk::` via comma; can mint audit labels | gate |
| R-03 | P1 | Agent comment bodies execute GitLab quick actions under bot PAT | gate |
| R-04 | P1 | Unauthenticated rig register/deliver = read any project the bot can | gate/intake |
| R-05 | P1 latent | Reaper closes live sessions above 50 running beads | gate |
| R-06 | P1 | Entrypoint aborts silently on unset `GC_ALIAS` | images |
| R-12 | P1 plausible | Spend poller overlap misses calls >2 min → soft gate fails open | meter |
| R-38/39 | P1 | Both no-latest gates are broken (one passes on error, one never runs) | ci |
| R-46 | P1 | Controller egress policy omits meter and intake; enforcement = no sessions | chart |
| R-47 | P1 | Intake→supervisor egress targets the wrong namespace/port for the bundled controller | chart |
| R-48 | P1 | Dolt root, no password, reachable from any pod; beads store rewritable | chart |
| R-07 | P2 | Prompt fetch: one transport blip burns a ladder attempt | images |
| R-08 | P2 | Secrets in order vars on the dead mention path | gate |
| R-09 | P2 plausible | Model can exfiltrate its own virtual key via a comment | images/gate |
| R-10 | P2 | Transcripts >64 KiB → infra-failed loop | gate |
| R-11 | P2 | Dedupe window double-spends and orphans | gate |
| R-13 | P2 | Plaintext keys accumulate in `prompts` forever | meter |
| R-14 | P2 | LiteLLM `max_budget` unset when either ceiling unlimited | meter |
| R-15 | P2 | Archived project's key never revoked | intake |
| R-16 | P2 | Comment body in URL query (414, logs) | intake |
| R-17 | P2 | Unbounded `spend_rows`, hot-path full scans | meter |
| R-18 | P2 plausible | 10k cap silently truncates first poll | meter |
| R-19 | P2 plausible | Key-missing project stuck at 500 | meter |
| R-20 | P2 | Operator quiet_hours without tz bricks every project | config |
| R-49 | P2 | LiteLLM admin key mounted unused into the controller pod | chart |
| R-50 | P2 | Agent image tag unguarded; `latest`/empty renders | chart |
| R-51 | P2 plausible | Agent SA can read other agent pods' `GC_ALIAS` | chart |
| R-52 | P2 | Controller SA can read every Secret in the namespace | chart |
| R-40..43 | P2 | Skips-as-green, unpinned actions, re-cut tag, unlinted tags | ci |
| R-21..37, R-44/45, R-53..56 | P3 | see §3 | — |

Open beads already covering P0 items not restated above: `gonk-alw` (cold
controller accepts session, never creates pod), `gonk-bvy` (duplicate keys
wedge). Both are consistent with what this review found (unserialized meter
loops; no create-outcome timeout handling that survives a controller restart).

---

## 5. Over-engineering: delete, merge, replace

Approximate non-test LOC in parentheses.

**Delete (dead by the repo's own ADRs or by call-graph)**

| What | LOC | Why |
|---|---|---|
| `pack/formulas/*`, `pack/orders/gonk-{triage,scaffold,mention}.toml`, `pack/scripts/gonk-check.sh`, `pack/agents/*/prompt.template.md`, `pack/agents/control-dispatcher/` | ~600 TOML/md | ADR-007 §3; no code path renders the templates; `check` cannot run under `[steps.check]`; stale beads → `gonk-p2e` |
| `cmd/gonk-gate/check.go` (+test) | ~250 | invoked only by the above |
| `cmd/gonk-gate/trailers.go` (+test), `images/agent/prepare-commit-msg` | ~740 | hook never installs in a pod; agent never commits |
| `pkg/verify` | ~750 | zero importers; belongs with the (non-existent) code-writing agent |
| `pkg/gcapi/submit.go` (+test) | ~250 | superseded by prompt-by-reference |
| Entrypoint marker parsing + `GONK_LITELLM_KEY` static fallback | ~100 | dead branches; R-35 attack surface |
| `test/harness/doctor.go` + `cmd/doctor`, `test/stubmodel/record.go` | ~600 | preflight/recorder for suites that don't exist or don't run |
| `hack/require_image_tags.py`, `hack/registry_prune.py` | ~400 | wire into a Make target or delete; today referenced only from CLAUDE.md |

**Merge**

- `pkg/budget` into `pkg/rung`; one `budget.Fits()` used by `rung.Decide` and
  both `ReserveIfFits`.
- `opercfg.foldPolicy` → `gonkcfg.Resolve(layers...)`.
- One `Effective`/`Budget` wire shape and one `+Inf` encoder.
- One meter HTTP client (`pkg/meterapi` client used by both intake and gate).
- One key-reconcile loop (meter-side), driven by change, not by three tickers.
- `beadstore.BdCLI` → a table in the meter's Postgres (closes `gonk-kx3`, the
  non-atomic `Put`, and five subprocess calls per state change).

**Replace with a library or an upstream feature**

- `pkg/glab` → `gitlab.com/gitlab-org/api/client-go` behind a `RoundTripper`
  that enforces the byte cap and scrubs tokens from errors (keeps the two
  properties the package exists for).
- `ForceSpendSync` → `golang.org/x/sync/singleflight`.
- `keysink` → return the key on an authenticated meter route to gate; drop
  the Secret round-trip and the RBAC that makes it legal.
- Gas City: decide whether gonk uses its convergence/beads/event bus or not.
  Today it pays for a controller and uses it as `kubectl run` plus a
  transcript reader. If the broker shape is permanent, a k8s Job per session
  with the entrypoint as-is removes the supervisor, the signed mutation plane,
  `gcapitest` (959 LOC), the reaper, `beadstore`, and the `gonk-alw`/`gonk-p2e`
  class of upstream bugs. If Gas City stays, use its beads as the state store
  and its convergence as the sweep. The current half-and-half is the most
  expensive option.

**Chart**

- Cut golden profiles from eight to the three or four that differ
  structurally; stop snapshotting comments (`helm template` then strip `#`
  lines before comparing).
- Move decision prose out of `values.yaml`/`_guards.tpl` into ADR-006 (which
  already repeats it).
- Delete dead values (`monitoring.otlpEndpoint` until a binary consumes it,
  `gascity.delivery`, `ledger.postgres.shared.*`); add `ledger.postgres.port`
  to the schema.

---

## 6. Architectural assessment

1. **Trust boundary rests on a control that does not exist.** At least five
   code comments (rig register/deliver, `/admin/reconcile`, prompts GET, the
   agent egress policy, the gonk-meter "not in the request path" claim) defer
   to NetworkPolicy for admission. The cluster has no policy controller
   (`gonk-dku`, measured). Until one exists, every in-cluster pod can: fetch
   any project's source (R-04), read any pending prompt+key if it can guess a
   128-bit alias (fine) or read a pod's env (not fine), and call
   `/admin/reconcile`. The fix is not "install Cilium" alone; it is to make each
   private route carry its own credential so the policy is defence in depth,
   not the only defence.
2. **The "money door" is real for the decision and soft for the ledger.**
   Gate 1 and Gate 2 genuinely cannot spawn without `/decide`, and `/decide`
   genuinely reserves against a ceiling. But what the ceiling is compared
   against (the spend ledger) can silently under-count (R-12, R-18), and the
   backstop (LiteLLM `max_budget`) is absent for the common config (R-14). A
   budget-conscious operator should assume the enforced number is LiteLLM's,
   set only when both ceilings are finite.
3. **Two orchestration models coexist and neither is finished.** The formula
   model (spec §4.3) is dead but shipped; the broker model (ADR-007) is live but
   its safety envelope (shape gate) is cardinality-only. The
   supervisor/beads/reaper machinery serves the dead model's assumptions
   (sessions have beads, beads converge) while the live model keys everything
   on the meter's reservation. Pick one.
4. **"Silent success" is the house failure mode, and it is structural.** The
   handoff says so; this review found seven more instances (reaper matching
   nothing, `ExpirePrompts` never scheduled, `max_turns` dropped at the wire,
   trailers hook never installing, `no-latest` passing on error, tagged suites
   green on skip, `check` reading env that is never set). The common cause is
   that the property is asserted in a test that is either excluded from the
   gate, or asserts the *shape* of the thing rather than its *effect*. The
   structural fix is one buildgate test — every `//go:build` tag in the repo
   must appear in a `go test -tags` line of some CI job — plus turning
   `t.Skip` into `t.Fatal` under `CI=true`.
5. **The documentation is a liability in a specific way.** It is candid and
   detailed, which is why this review could be done at all, but the *status*
   surfaces (`PLAN.md` table, `HANDOFF` "START HERE", README layout) lag the
   candid text by weeks. `PLAN.md` says plans 01–05 are done and, 760 lines
   later, that plan 05 has not started. A reader trusting the table would
   deploy a system whose own ADR says a third of it is deprecated.

---

## 7. Documentation drift and sensitive material

**Contradictions to resolve** (each is one edit):

| Doc says | Reality |
|---|---|
| Spec: MIT | `LICENSE` Apache-2.0 |
| Spec §4.3/§7.1: agent image = opencode+git+glab+bd; agent posts via API | glab/bd removed; broker posts; ADR-007 supersedes and the spec does not reference it |
| Spec §4.1: Dolt bead store with backups; LiteLLM bundled with Postgres | ledger on CNPG Postgres after the Dolt spike failed; LiteLLM BYO; ledger has no PDB/backup (`PLAN.md:923`) |
| Spec §7: `gonk-navigator` and `gonk-city` repos | neither exists (`docs/environment.md:117`) |
| README: `pack/ images/ chart/ test/` are "later plans" | all exist and are deployed |
| `PLAN.md` table: 01–05 done | `PLAN.md:772,798`: "Plan 05 has not started"; ADR-007 deprecates half of 04 |
| `HANDOFF` "START HERE" = `gonk-ob5` | closed 08-30; doc last updated 08-19; cites beads `gonk-4xr`/`gonk-2wq` that do not exist |
| Prompt-by-reference plan: T1–T6 not started | implemented and live |
| ADR-003 §3: queue full → 200 | code returns 503 |
| `PLAN.md:90-91, 632-639`: `HTTPDispatcher` not yet a gcapi wrapper | it is |
| `PLAN.md:238-240`: key is a file mount never read into env | pod materialises it from the prompt body |
| `fuzz_test.go:13-16`: 60s fuzz in standing gate | no `-fuzz` anywhere |

**Sensitive material** (spec goal 8; `gonk-73dh`): `gitlab.orac.local` /
`registry.orac.local` / `gonk.orac.local` in ~780 places including
`Chart.yaml`, the Go module path, and CI; Ollama at `192.168.1.142` in a chart
template plus the beads that explain it needs no credential and nothing
enforces the policy; `10.0.5.7/32`, `192.168.3.33`, `34.206.143.55`; owner
email and `/mnt/c/Users/steve/…` paths; Vault paths and ClusterSecretStore
names; GitLab project ids 6/63/65/69/75, bot user 49, hook id 3; node names
and taints; `HANDOFF:792` "printed the bot PAT in plaintext" with no rotation
record; a `sk-…` literal in a test (reported dead). The GitHub mirror and a
v0.1.0 release already exist.

---

## 8. Upstream dependencies

| Dependency | Reported? | Posture |
|---|---|---|
| Gas City #4891 (k8s provider drops PromptSuffix) | posted | worked around: prompt-by-reference |
| Gas City #4668 (formula-order vars dropped) | posted | worked around: broker; `gonk-6gs` "BLOCKED" is stale |
| Gas City keystroke delivery = injection boundary | drafted | worked around; `gonk-e9m` open |
| Gas City cold controller accepts session, never creates pod | no draft | **waiting** (`gonk-alw` P0) |
| Gas City `scale_check` demand from stale beads | no draft | waiting (`gonk-p2e`) |
| Gas City alias uniqueness over active sessions only | no draft | worked around |
| opencode `!` shell mode; fail-open providers; `run --attach` | drafted / not filed | worked around |
| LiteLLM `/spend/logs` unbounded (OOM DoS) | write-up ready since 07-14, not posted (`gonk-wgq` P1) | bounded polls (with R-18) |
| LiteLLM streaming tool-call deltas | — | patched at proxy (`contrib/litellm/custom_callbacks.py`, deployed from another repo) |
| LiteLLM `spend_logs_metadata` enterprise-labelled | — | relies on it being unenforced |
| No NetworkPolicy controller | infra | probe shipped; gap open |
