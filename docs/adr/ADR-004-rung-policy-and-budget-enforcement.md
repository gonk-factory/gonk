# ADR-004: rung policy and budget enforcement

Status: accepted 2026-07-14

This ADR records the decisions Plan 03 (`pkg/rung`, `pkg/budget`,
`pkg/spend`, `pkg/opercfg`, `internal/meter/*`, `cmd/gonk-meter`) locks in —
gonk-meter's half of the split with gitlab-intake. It cross-references
ADR-002 (config precedence) and ADR-003 (Plan 02's intake trust boundary).

**This ADR does not modify ADR-002.** ADR-002 is `gonkcfg.Resolve`'s
precedence contract; this ADR is about what meter does with the `Effective`
that `Resolve` produces — the ladder, the budget, the reservations, the
clock. Where the two overlap (the `+Inf`/`MaxInt64` -> `null` encoding), this
ADR restates ADR-002's rule rather than re-deriving it.

**Link to ADR-003.** ADR-003 settles the division of labor with intake in
one sentence worth repeating here in as many words: **meter, not intake,
resolves config and enforces quiet hours.** `gonkcfg.Resolve` is called in
exactly one place in the whole system — `internal/meter/service.Register` —
and `schedule.quiet_hours` is evaluated nowhere but
`POST /v1/policy/decide`. Intake pushes raw `.gonk.yml` bytes and has no
resolver and no quiet-hours code of its own.

---

## The three-valued decision, and why `deny` exists

Spec 6.2 says "return rung or `defer`." That is incomplete. A project that
is disabled, whose config is invalid, or whose ladder is exhausted will
**never** succeed on retry — parking it in `waiting-for-capacity` forever is
a bug, not a policy. `deny` is the answer for "retrying cannot help."
`meterapi.DecideResponse.Decision` is therefore `run` | `defer` | `deny`,
and **`defer`/`deny` are both HTTP `200`** — normal policy answers, not
errors, identical from whichever gate asks.

## The rung index is the count of gate failures; infra failures never escalate

`pkg/rung.Decide`'s ladder position (`Escalations(in.Prior)`) counts only
attempts whose outcome was `gate-failed`. An infrastructure failure
(connection error, LiteLLM 5xx, pod eviction, reservation expiry) retries
the **same** rung and never advances the index — a flaky pod is not
evidence a bigger model is needed (spec 6.3). Consecutive infra failures at
one rung are capped (`max_infra_retries`, default `5`) and then `deny`
(`infra-retries-exhausted`). The classification a caller reports at
`/v1/policy/outcome` is therefore load-bearing: reporting an infra failure
as `gate-failed` buys an escalation the project did not earn, and reporting
a gate failure as `infra-failed` denies it one it did.

## Reservations, their TTL, and why an expired one becomes `infra-failed`

A `run` decision writes a reservation (estimated real + synthetic cost, and
tokens, for that rung) atomically with the read that decided it fits.
Remaining budget is always `ceiling - (observed spend + open
reservations)`. The reservation is not freed the instant an outcome is
reported — it is **settled**, moving its hold to `now +
max_spend_staleness` (default `5m`), i.e. until the session's spend rows
are expected to have landed in LiteLLM's log. An unsettled reservation
expires at `reservation_ttl` (default `60m`); the janitor records that as
one `infra-failed` attempt, so a session that died silently retries its
rung rather than being charged an escalation it never earned or a budget
hold it never releases.

**`/decide` is idempotent on an open reservation.** Because the decision is
gated in two places (intake's Gate 1, then the pack's Gate 2 on every
pour), a bead can be `/decide`d twice before any outcome is reported. A
second call for the same `(project, bead_id, session_key)` returns the
existing open, unexpired reservation rather than minting a second one —
otherwise one attempt would hold double the budget headroom. `bead_id` is
pinned to the deterministic `BeadAnchor` at both gates precisely so the two
calls look like the same bead.

## The UTC calendar month, and monotone rollover

The budget month is the **UTC calendar month** — not project-local, not a
rolling 30 days. Two projects in different timezones must not disagree
about which month a charge lands in. Rollover is **monotone**
(`spend.Advance`): a backwards clock jump (NTP correction, VM restore) can
never reset a project's spend or move the window early. Rows are windowed
by **the row's own timestamp**, never by ingest time — a July row that
lands on 1 August is charged to July, never handed a free budget by the
rollover.

## Clock skew is a health problem, not a math problem

Meter compares its own clock to the spend source's HTTP `Date` header on
every sync. Skew beyond `max_clock_skew` (default `5m`) fails `/readyz` and
makes every decision for a project with **any** finite ceiling `defer`
(`spend-data-stale`); an all-unlimited project has no budget to protect and
still runs. If the spend source omits its `Date` header entirely, skew
cannot be measured at all — `spend.SkewOK` returns `true` rather than
blocking the whole system on a stripped header, but
`gonk_meter_clock_skew_unknown_total` increments. **This is the one place
the skew guard is advisory rather than fail-closed, and it is stated here
rather than hidden.** The budget math itself never tries to be clever about
a clock it does not trust; it refuses to decide instead.

## `+Inf`/`MaxInt64` -> `null`, and the SAME value has two correct encodings

`gonkcfg.Resolve` encodes "unlimited" as concrete sentinels (ADR-002):
`math.Inf(1)` for a cost ceiling, `math.MaxInt64` for a token ceiling.
Neither survives JSON directly — `encoding/json.Marshal(math.Inf(1))`
**returns an error**, and `MaxInt64` loses precision in any float64-based
parser (every JavaScript client). `pkg/meterapi.Budget` therefore encodes
both as `null` on the `/v1/policy/decide`, `/v1/projects/{project}`, and
cost-endpoint wire.

**Prometheus keeps `+Inf`, on purpose, and this is not an inconsistency —
say so explicitly.** `internal/meter/metrics`'s remaining-budget gauges
render an unlimited ceiling as literal `+Inf`: Prometheus's text exposition
format supports it natively, and rendering the raw `MaxInt64` sentinel
instead would show up in Grafana as `~9.22e18`, which is not "unlimited",
it is a lie shaped like a very large number. So **the same fact — "this
project has no ceiling" — has two different, both-correct wire encodings**:
JSON `null` on the API, `+Inf` in the metrics text format. A client that
assumes one from having seen the other is wrong.

## The cost gate reads real dollars only, and what `monthly_cost_usd: 0` means

`rung.Decide`'s cost gate (`rem.FitsCost`) fires only for a rung with a
**positive real** price (`spec.EstCostUSD > 0`); `opercfg` guarantees cloud
rungs have one and local rungs do not. **A project's `monthly_cost_usd: 0`
therefore means "local rungs run freely, a cloud rung is never affordable,
forever"** — the exact conservative default the onboarding MR ships (spec
5.3, `ladder: [qwen-local]`). See Decision 9 below for why this had to
survive synthetic pricing without change.

## Failure-mode matrix (requirement: hard budgets fail closed)

Copied verbatim from the plan (`docs/superpowers/plans/2026-07-13-plan-03-gonk-meter.md`),
current as of Task 8's re-review:

| Failure | Meter's behavior | Why it is closed |
|---|---|---|
| LiteLLM admin API unreachable at onboarding (key never provisioned) | Registration state `key-missing`; `/decide` returns **defer** (`virtual-key-missing`, `retry_after` = now + 5m). Reconcile loop retries provisioning. | No key, no session, no spend. Never `run`. |
| LiteLLM spend source unreachable / spend snapshot older than `max_spend_staleness` (default 5m) | `/decide` returns **defer** (`spend-data-stale`) for any project with *any* finite ceiling. Projects with all-unlimited budgets still run (nothing to be stale about). | We would be deciding on numbers we know are wrong. |
| Spend-log lag (calls made, rows not yet written) | Open reservations cover the lag window: remaining = ceiling − observed − reserved. The reservation is written **before** the session starts, and reporting an outcome does **not** free it — it *settles* it, moving the hold to `now + max_spend_staleness`, i.e. until the rows are expected to have landed. If they have not, the snapshot is stale by definition and the next row of this table takes over. LiteLLM's USD ceiling is the hard backstop. | The hold ends when the *data* catches up, not when the *session* does. |
| Clock skew at month rollover (NTP correction, VM restore) | Backwards jump: `spend.Advance` is monotone, window does not move, spend is not reset. Forwards jump beyond `max_clock_skew` vs the spend source's clock: `/readyz` fails, and every project **with any finite ceiling** defers (an all-unlimited project has no budget to protect, so it runs). | An early rollover is a free budget. Refuse to roll rather than guess. |
| Spend source omits its `Date` header, so skew cannot be measured | `spend.SkewOK` returns true (blocking the whole factory because a proxy stripped a header is worse than the risk), but `gonk_meter_clock_skew_unknown_total` increments. **This is the one place the skew guard is advisory rather than fail-closed — alert on that counter.** | Stated rather than hidden. |
| Concurrent sessions racing the same ceiling | `/decide` takes a **per-project lock** across `read spend + read reservations + decide + write reservation`. Losers see the winner's reservation and `defer`. Proven by a `-race` test: 20 goroutines, headroom for 2, exactly 2 `run`. | The check and the reservation are one atomic step. |
| Session dies without reporting an outcome | Reservation expires at `reservation_ttl`; the janitor records an `infra-failed` attempt. | Budget stays held for the TTL; the rung does **not** escalate. |
| Meter restarts | Cold start: `/readyz` false until the first spend sync completes. **Reservations survive** — they are in the durable store (Postgres/CNPG). This is exactly why the in-memory store is not the shipping backend: losing reservations on restart is fail-open on headroom for the length of the spend-log lag. | Metric `gonk_meter_cold_start_total`. |
| Does the ledger's transactional guarantee actually hold? | **Measured, not assumed — see "The ledger backend" below.** Task 0b's spike found Dolt does *not* serialize the race; the shipping store is Postgres, whose `SELECT ... FOR UPDATE` was independently verified to block. | An unverified transactional guarantee under a budget ceiling is exactly the thing that must not be assumed. |
| Two meter **replicas** race the same ceiling | **Meter is multi-replica-safe.** The store enforces both money-safety properties across pods: overspend via `SELECT ... FOR UPDATE` on the project's ceiling row, and double-reserve of `/decide` via a partial `UNIQUE` index on the open `(project, bead_id, session_key)` key plus an in-transaction pre-check under the same lock. Proven by `TestReserveIfFitsRace` and `TestReserveIsIdempotentAcrossReplicas` (`internal/meter/store/postgres_race_test.go`), both of which call the store directly, with no service-side mutex in the way. The in-process `keyedMutex` is a throughput optimization only. | Both races close in the database, not in an in-process lock, so N pods are exactly as safe as one. |
| A local rung burns the whole token budget **through LiteLLM** | LiteLLM's USD `max_budget` now covers it: local models carry a **synthetic** per-token price, and meter folds the token ceiling into the dollar ceiling it provisions (Decision 9). Meter's reservation gate refuses first; LiteLLM refuses if meter is wrong. | The token budget has a hard door **on that path**. |
| **An agent pod calls Ollama (`http://192.168.1.142:11434`) DIRECTLY, around LiteLLM** | **NOTHING IN THIS PLAN STOPS IT.** Ollama needs no credential, and the NetworkPolicy that would forbid the egress **is not enforced** (Flannel; Cilium suspended). Zero metering, no ceiling, no attribution. | **NOT CLOSED.** Owner decision: ship, document, do not gate. **Local-model budgets are advisory** until Cilium lands; cloud-rung budgets are hard **only when the project also sets a finite `monthly_tokens`** (else `MaxBudgetFor` provisions no `max_budget` and the key is unbudgeted — see the row below). Plan 05 ships the policy; Plan 06 skip-tests it; un-skipping that test is the gate. |
| **A project sets `monthly_cost_usd` finite but leaves `monthly_tokens` unlimited** | `MaxBudgetFor` returns `nil` (LiteLLM omits `max_budget`): the virtual key has **no hard door at all**, not even on cost. The choice is deliberate (LiteLLM unifies real + synthetic USD, so a `max_budget = cost` alone would let synthetic local dollars close the door early on legitimate local work), but the consequence is real: only meter's soft reservation-estimate gate protects the cost ceiling, and a runaway session can overshoot it. | **Known limitation.** The hard cloud door requires a finite `monthly_tokens` too. Set both dimensions for a hard backstop. |
| Synthetic dollars get counted as real spend | `spend.Row.Synthetic` is set at ingest from the rung's `kind`; `budget.Spend` keeps `CostUSD` (real) and `SyntheticCostUSD` separate; `rung.Decide`'s cost gate reads **real only**. A rung missing from the catalog is treated as **real** (fail closed). | If synthetic dollars reached the cost gate, `monthly_cost_usd: 0` would make the onboarding default's own local rung unaffordable — the project would be dead on arrival. |
| A spend row for last month lands after the window rolled | Rows are windowed by **the row's timestamp**, never by ingest time. A July row arriving on 1 August is charged to July. | Otherwise every project gets a free budget on the 1st. |
| `.gonk.yml` invalid / project disabled / ladder exhausted / per-task tokens gone | **deny** with a machine reason. Never `run`, never an infinite `defer`. | Retrying cannot help. |
| Attribution tag contains a newline, comma, or control character | Rejected at the boundary with 400 before any tag is minted. | Injection into log lines / CSV / headers downstream. |
| Operator instance/group `Policy` malformed (NaN ceiling, unknown rung, negative budget, unpriced cloud rung, **unpriced local rung**, rung with no token estimate) | `opercfg.Load` refuses to start the process. Meter does not run on a config it cannot validate. | ADR-002's "Known gap", closed. An unpriced *local* rung is now just as fatal as an unpriced cloud one: with no synthetic price it has no hard door (Decision 9). |
| A caller forges an outcome (`gate-failed` on repeat) to climb to an expensive rung | `/v1/policy/outcome` binds the outcome to a reservation **meter** minted: an unknown `reservation_id`, or one whose `(project, bead, attempt)` does not match, is a 400 and records nothing. Attempts are keyed by reservation, so a replay overwrites instead of appending. | Meter owns ladder state; the caller cannot vote twice. |
| A project reorders its ladder to put a cloud rung first | `opercfg.CheckLadderOrder` requires the project's ladder to be a subsequence of the instance's (`enforce_ladder_order`, default on), and `Load` refuses to start if the guard is on with no instance ladder to order against. | Spec 6.3: "everything starts at the cheapest rung its project config allows". |
| A nested group tries to loosen its parent's ceiling | `opercfg.GroupFor` folds **every** matching ancestor group, min-folding budgets, before `Resolve` ever sees them. | Ceilings only tighten downward (spec 5.4, ADR-002). |
| A LiteLLM row carries a negative `spend` (a credit or adjustment) | `budget.Remain` clamps negative consumption to zero; it can never *raise* a ceiling. | External input must not be able to mint headroom. |
| Two projects whose names flatten to the same slug (`a/b` and `a-b`) | `keysink.Slug` appends a hash of the original path, so the LiteLLM key alias is unique. Otherwise the second project would silently adopt the first's key — and its `max_budget`. | One virtual key, one project, one hard ceiling. |
| A project turns off an action (`actions.triage: false`) at any layer | `/decide` denies with `action-not-allowed` and mints no tags and returns no `key_ref`. `/decide` is the only chokepoint before a session spawns, so if meter does not check `Effective.Actions`, nothing does. | The ADR-002 veto surface is real, not decorative. |

---

## Owner decisions folded in

### Decision 9 — synthetic pricing

LiteLLM's virtual-key budget is USD-denominated with no token counter, and
local models were priced at `$0` — so the dollar door never closed on them
and `monthly_tokens`/`per_task_tokens` had **no hard enforcement anywhere**.
The onboarding default (`monthly_cost_usd: 0`, `ladder: [qwen-local]`) had
no hard door at all.

**The decision:** every rung gets a nonzero per-token price in LiteLLM's
model config — real for cloud rungs, **synthetic** for local ones — and
meter converts the project's token ceiling into the USD ceiling it
provisions on the virtual key (`MaxBudgetFor`, folding
`monthly_cost_usd + monthly_tokens × max synthetic/real price across the
ladder`; omitted entirely — unlimited — if either dimension is unlimited,
because a genuinely unlimited project cannot get a hard door and the plan
does not pretend otherwise). This is a deliberately **loose upper bound**,
not a tight one: it must never close early on legitimate work, and its job
is only to close on a project that blows its token budget on local
inference.

**The load-bearing part, in as many words: real dollars and synthetic
dollars are different currencies that must never be summed in a policy
decision.** `.gonk.yml`'s `budget.monthly_cost_usd` means real money and is
unchanged by this decision; `rung.Decide`'s cost gate reads real dollars
only. If synthetic dollars ever reached that gate, the onboarding default
(`monthly_cost_usd: 0`, `ladder: [qwen-local]`) could not afford its own
only rung, and the project would be dead on arrival. `spend.Row.Synthetic`,
`budget.Spend.{CostUSD,SyntheticCostUSD}`, and every cost-endpoint field
pair (`cost_usd` / `synthetic_cost_usd`, `cost_synthetic`) keep the two
separated end to end; a rung missing from the catalog is treated as real
(fail closed).

**Where the price physically lives, and what is NOT yet verified:** the
operator's rung catalog (`pkg/opercfg`) carries
`synthetic_usd_per_1m_tokens`, required on local rungs and forbidden on
cloud ones. The corresponding LiteLLM-side price lives in the **gitops**
repo, `clusters/orac/apps/litellm/litellm.yaml`,
`spec.values.litellm_settings.model_cost_map` — where local models are
priced at **zero today**. LiteLLM accepts a per-token price in two
plausible places (`model_cost_map`, which the repo actually uses, and a
per-model `model_info` block); **nobody has verified which one LiteLLM
actually consults when it writes a `/spend/logs` row for a locally-routed
model.** Plan 06 Task 5 must answer that empirically. If `model_cost_map`
turns out not to price local routes, the synthetic dollar never lands in a
spend row, the USD door never closes on tokens, and this decision buys
nothing — that is a finding for the owner, not a footnote.

### Decision 10 — the ledger backend (SUPERSEDED: Postgres, not Dolt)

The plan's original text named Dolt: "it is already run for the beads
store, so it adds no new dependency, and its versioned history suits an
audit ledger," explicitly **not yet verified**, gated on Task 0b's blocking
spike. **That spike ran, and Dolt failed.**

`docs/spikes/dolt-reservation-isolation.md` records what was **measured**,
not assumed: against Dolt `2.1.10`
(`dolthub/dolt-sql-server:latest`), 32 concurrent writers raced a ceiling
with headroom for exactly 2, at three escalating isolation strategies —
plain default-isolation `BEGIN`, explicit `SERIALIZABLE`, and explicit
`SELECT ... FOR UPDATE` (the exact shape `ReserveIfFits` uses in
production). **All 32 writers won, every one of 90 independent iterations,
at every strategy, zero variance** — $12.80 persisted against a $1.00
ceiling, a 12.8x overspend, with zero errors, zero serialization failures,
zero lock-wait timeouts raised by Dolt at any point. A second, targeted
check confirmed `SELECT ... FOR UPDATE` is not merely insufficient but a
syntactic no-op on this version: a second transaction acquired the
"locked" row in 462.847µs while the first transaction's lock was still
held, uncommitted. This is consistent with Dolt's documented concurrency
model — optimistic, commit-time, per-row — which has nothing to detect when
every writer inserts a distinct row and the conflict is in an aggregate
nobody re-checks at commit.

**OWNER DECISION (2026-07-14): the ledger backend is Postgres, on the
owner's existing CNPG cluster.** Not a new dependency — CNPG already runs
in-cluster. `SELECT ... FOR UPDATE` / `SERIALIZABLE` on Postgres is a
mechanism verified by decades of production use, not a novel claim needing
its own spike, and `internal/meter/store/postgres.go` is built and tested
against a real Postgres to prove it holds here too, not to assume it does
(`TestReserveIfFitsRace`, `TestReserveIfFitsRacePerTaskTokenCeiling`,
`TestReserveIsIdempotentAcrossReplicas`,
`internal/meter/store/postgres_race_test.go`, run under `-race`). The cost
is Dolt's versioned audit history for the ledger specifically, mitigated by
append-only ledger tables. **Dolt is not dropped from the system** — it
remains Gas City's beads store, which has no reservation-race requirement;
it is simply not meter's ledger. `store/dolt.go` for the ledger does not
exist; there is no `GONK_METER_STORE_BACKEND=dolt` option for the
reservation store.

**Consequence for "the service's per-project mutex is per-process" — this
also changes.** The plan's original text tied Dolt's insufficiency (if
found) to running meter single-replica, with the in-process `keyedMutex`
supplying the actual atomicity. That is no longer the design: Postgres
supplies the atomicity, in the database, and it holds **across replicas**,
not just within one process. See "The ledger is multi-replica-safe" below —
AD-10 (single-replica) is lifted.

### Decision 11 — LiteLLM's spend API, and everywhere the polling lag is handled

The spend source is LiteLLM's `/spend/logs` HTTP API, not its Postgres — a
supported surface that survives upgrades, with no coupling to LiteLLM's
internal schema. The cost is polling lag, handled in four places: open
reservations bridge the gap between a session starting and its spend rows
landing (`ceiling - observed - reserved`); the staleness gate
(`spend-data-stale`) refuses to decide on a snapshot older than
`max_spend_staleness`; month rollover is by **row timestamp**, never ingest
time, so a late-arriving row never gets a free budget on the new month; and
`complete: false` on every cost-endpoint response tells a reader (most
importantly, the pack writing a commit trailer) that spend rows for an
open session/bead may not have landed yet. **The poller must always pass a
bounded date window and paginate** — a verified smoke test in
`docs/environment.md` ("OPERATIONAL HAZARD") **OOM-killed the live LiteLLM
pod** with one unbounded `/spend/logs` query, because it loads the whole
spend table into memory, and meter shares that LiteLLM with the whole
cluster. A missing date bound on this poller is a code-review blocker.

### Decision 12 — secrets are file mounts, two rotation slots

Vault/1Password -> Kubernetes Secret -> **file mount**. Never an env var
(leaks into `ps`, crash dumps, every child process), never a flag, never
the config file, never git. Applies to the LiteLLM admin credential and the
meter API bearer token, each with a current slot and a previous slot so a
rotation is not an outage — the same contract as Plan 02's GitLab bot token
and webhook secret.

---

## The ledger is multi-replica-safe; AD-10 (single-replica) is lifted

The plan's original assumed-default, AD-10, pinned meter to
`replicas: 1` on the theory that the store's isolation might turn out to be
insufficient and the in-process `keyedMutex` would then be doing the real
work of serializing `ReserveIfFits`. Task 8's re-review superseded that:
meter is **multi-replica-safe**, and the money-safety guarantee does not
depend on how many pods are running.

`ReserveIfFits` enforces **both** properties this system needs, in
Postgres, across replicas:

- **Overspend safety** — `SELECT ... FOR UPDATE` on the project's ceiling
  row serializes the read-sum-check-write sequence, so two pods contending
  for the same headroom cannot both win.
- **`/decide` idempotency** — a partial `UNIQUE` index on the open
  `(project, bead_id, session_key)` key, plus an in-transaction pre-check
  under the same lock, so two pods asked to `/decide` the same bead within
  the reservation's open window cannot both mint a reservation.

Both are proven by tests that call the store directly, with **no service
mutex in the way** — `TestReserveIfFitsRace` and
`TestReserveIsIdempotentAcrossReplicas`
(`internal/meter/store/postgres_race_test.go`) — precisely so the tests
cannot be accidentally passing because of the in-process lock rather than
the database. The service's per-project `keyedMutex` still exists; it is
now a **throughput/ordering optimization only** (it reduces contention on
the Postgres lock under load), not a correctness dependency. Removing it
breaks neither property.

**Consequence for Plan 05 (the chart):** meter may run more than one
replica. The chart may still **default** to `replicas: 1` for the modest
resource footprint of the initial deployment, but it must not hard-guard a
single replica as a correctness requirement, and nothing in this plan's
documentation, dashboards, or code may claim meter needs single-replica
operation to be safe.

---

## THE NETWORK-LAYER BYPASS IS OPEN, AND THIS ADR IS ONE OF THE THREE PLACES THAT MUST SAY SO

**NetworkPolicies are not enforced on the target cluster.** Flannel does
not implement them, and the Cilium HelmRelease that would is **suspended**.
LiteLLM routes local models to an **unauthenticated Ollama at
`http://192.168.1.142:11434`**. An agent pod that can reach the network can
therefore skip LiteLLM entirely, call Ollama directly, and burn local GPU
with **zero metering and no ceiling**.

Every door this ADR describes — the reservation gate, the virtual key's
`max_budget`, the synthetic price that makes the token budget hard through
LiteLLM — binds only traffic that actually goes **through** LiteLLM. The
NetworkPolicy is the only thing that would force it to, and it is not
enforced.

**Say it exactly this way, everywhere:** **cloud-rung budgets are hard —
when the project sets a finite `monthly_tokens` as well as a finite
`monthly_cost_usd`. Local-model budgets are advisory** until Cilium lands
and the egress policy is actually enforced. Owner decision (2026-07-13):
ship it, document the gap, do not gate on it. `docs/environment.md` is the
primary record of the infrastructure fact; this ADR and the metric help
strings/dashboards are the other two places it must be said, honestly, with
no softening. Plan 05 ships the egress NetworkPolicy anyway
(documentation-as-code — it becomes real the day Cilium is un-suspended);
Plan 06 writes the egress-denial test and **skips** it, naming Cilium in
the skip message. Un-skipping that test is the gate that proves the
guarantee is real. Nothing in this system may be written, logged, or
dashboarded as though that day has already come.

---

## Known limits

- **`pkg/gate.Classify`'s one known false positive — record it, do not hide
  it.** A pod evicted *after* at least one successful completion but
  *before* posting its comment classifies as `gate-failed` and buys **one**
  unearned escalation. It is bounded (one rung, one attempt) and the
  escalated attempt still passes `/decide`, so it cannot exceed budget.
  Closing it properly needs a **pod-termination signal from Gas City's
  session provider**, which is not in the facts available to this plan.
  Hand-off: Plan 06 should measure how often it fires (K-series kill tests
  already evict pods); if it is common, it becomes an upstream ask.
- **The network-layer bypass, above** — restated here for emphasis: this
  is not a corner case, it is the honest current state of the strongest
  claim this system makes, and grepping the diff for "cannot be bypassed"
  should find every occurrence qualified.
- **The hard cloud door needs a finite `monthly_tokens`, not just a finite
  `monthly_cost_usd`.** `MaxBudgetFor` provisions no `max_budget` at all if
  *either* budget dimension is unlimited — deliberately, because unifying
  real and synthetic USD into one counter and provisioning
  `max_budget = monthly_cost_usd` alone would let synthetic local dollars
  close the door early on legitimate local work. The consequence: a
  project with a finite cost ceiling but an unlimited token ceiling gets an
  **unbudgeted** virtual key, defended only by meter's soft
  reservation-estimate gate, which a runaway session can overshoot.
- Token budgets now have a hard door, but a loose one: LiteLLM's USD
  ceiling, fed by synthetic local-model prices (Decision 9). The *tight*
  gate is still meter's reservation, and it is session-start only — meter
  can refuse to start a session but cannot stop one mid-flight.
- **The synthetic price has to land in a LiteLLM spend row to do anything
  at all**, and whether `model_cost_map` (what the gitops repo actually
  uses) or a per-model `model_info` block is what LiteLLM consults for a
  **locally-routed** model's price is **not verified** — Plan 06 Task 5
  must answer it empirically. If it is the wrong one, Decision 9 buys
  nothing.
- **LiteLLM's admin API request/response shapes are best-effort, unverified
  against a live LiteLLM.** `/key/generate`, `/key/update`, `/key/delete`,
  and `/spend/logs`/`/spend/logs/v2` have never been exercised against the
  pinned LiteLLM version — only against `internal/meter/litellm.Fake`.
  Plan 06 must verify all four, and separately verify that a LiteLLM key's
  `budget_duration: "1mo"` resets on the same boundary as meter's UTC
  calendar month (a rolling-30-day reset would desynchronize the soft and
  hard doors, a real defect, not a rounding difference).
- **`internal/meter/keysink.K8s`'s `Put` fallback `Update()` may need a
  `ResourceVersion` a real API server enforces that the fake clientset used
  in this plan's own tests does not.** Untested against a real API server;
  Plan 06 verifies it.
- Synthetic dollars are an accounting unit, not spend. Every surface that
  shows them must label them (`cost_synthetic`, the `synthetic="true"`
  metric label). A dashboard that sums them with real spend is lying.
- `KeySink` has no Kubernetes implementation's RBAC proven against a real
  cluster until Plan 06; the Go code and its fake-clientset tests are this
  plan's (Task 7 Step 4b), but the `Role`/`RoleBinding` that make its
  writes legal on a real API server are Plan 05's (AD-1).
- **Reservations are estimates.** A session that consumes far more than its
  rung's `est_cost_usd` overshoots meter's soft ceiling by the difference;
  LiteLLM's hard USD ceiling is what stops it, and there is no equivalent
  hard stop for tokens mid-session. Tune the estimates and alert on
  `actual >> estimated`.
- **Meter's spend view is eventually consistent, and the reservation hold
  is what bridges the gap.** The hold ends at `now + max_spend_staleness`
  after an outcome, on the assumption the rows have landed by then. If
  LiteLLM's real spend-log lag ever exceeds `max_spend_staleness`, that
  assumption is wrong — but the staleness gate then fires and defers, so
  the system stalls rather than overspends. Plan 06 must measure the real
  lag and set `max_spend_staleness` above it.
- The store grows without bound (spend rows, settled reservations, attempt
  history). Retention is 13 months, pruned by a daily job.

---

## See also

- `docs/api/gonk-meter-v1.md` — the published wire contract this ADR's
  policy is expressed through.
- `docs/adr/ADR-002-config-precedence-semantics.md` — `.gonk.yml`
  precedence and the `+Inf`/`MaxInt64` -> `null` encoding.
- `docs/adr/ADR-003-intake-trust-boundary-and-seams.md` — intake's half of
  the division of responsibility.
- `docs/spikes/dolt-reservation-isolation.md` — the Decision-10 spike's full
  method, raw numbers, and decision record.
- `docs/environment.md` — the network-layer bypass's primary infrastructure
  record.
