# Chart reconciliation checklist (Plan 05, Task 0)

Scratch checklist. Delete in Task 9 after folding anything surprising into
`chart/gonk/README.md` (per Task 0's own instructions).

Executed against `main` at commit `f78a0df` ("Merge plan-04: pack & images
(Gate 2, the pack, container images)"), worktree `plan-05-chart` / branch
`plan-05-chart`. Plans 01-04 are merged; this is the reconciliation pass
Plan 05's own Task 0 mandates before any chart code is written.

The full narrative delta list lives in the "RECONCILIATION (Task 0, done at
execution)" note at the top of
`docs/superpowers/plans/2026-07-13-plan-05-chart.md`. This file is the
row-by-row verification record the plan's Task 0 Step 1/2 asked for.

## Step 1: name-by-name verification

| What | Plan assumed | Verified in | Result |
|---|---|---|---|
| intake env vars | `GONK_GITLAB_URL`, `GONK_GITLAB_TOKEN_FILE`, `GONK_GITLAB_TOKEN_PREVIOUS_FILE`, `GONK_GITLAB_ADMIN_TOKEN_FILE`, `GONK_WEBHOOK_SECRET_FILE`, `GONK_WEBHOOK_SECRET_PREVIOUS_FILE`, `GONK_WEBHOOK_TOKEN_GEN`, `GONK_WEBHOOK_PUBLIC_URL`, `GONK_WEBHOOK_SSL_VERIFY`, `GONK_BOT_USERNAME`, `GONK_METER_URL`, `GONK_METER_TOKEN_FILE`, `GONK_SUPERVISOR_URL`, `GONK_RECONCILE_INTERVAL`, `GONK_LISTEN_ADDR`, `GONK_PRIVATE_ADDR`, `GONK_VERSION` | `cmd/gonk-intake/main.go` `loadConfig` | MATCH. Also real, not itemized in the plan's table: `GONK_INSTANCE_LADDER` (required, fails closed if empty/unset -- ADR-002) and `GONK_DISPATCH_STALENESS_WINDOW` (optional). `SSL_CERT_FILE` is set by the image (`images/Dockerfile.intake`'s `ENV`), not read as a `Config` field. |
| intake listeners | public `:8080` = `POST /hook/gitlab` only; private `:9090` = health/ready/metrics/admin-reconcile | `pkg/intake/server.go` | MATCH (contract unchanged since the plan was written). |
| meter flags/env | `--operator-config`, `--listen`, `LITELLM_URL`, `LITELLM_ADMIN_KEY_FILE`, `LITELLM_ADMIN_KEY_PREVIOUS_FILE`, `GONK_METER_TOKEN_FILE`, `GONK_METER_TOKEN_PREVIOUS_FILE`, `GONK_METER_STORE_BACKEND`, `GONK_METER_STORE_DSN_FILE`, `GONK_KEYSINK_NAMESPACE`, `GONK_KEYSINK_PREFIX` | `cmd/gonk-meter/main.go` | MATCH on names. `GONK_METER_STORE_BACKEND` valid values corrected: `postgres` ONLY, not `dolt \| postgres` (see ledger delta below). `SSL_CERT_FILE` set by `images/Dockerfile.meter`'s `ENV`. |
| meter listener | one port `:8080`; health/ready/metrics unauthenticated, rest bearer | `internal/meter/service/http.go` | MATCH. |
| meter clock seam | `//go:build testclock` variant, production build excludes it | `images/Dockerfile.meter`, `test/images/servers_smoke_test.go` | MATCH, and more precise than assumed: `make meter-testclock-image` tags `$(GONK_TAG)-testclock`, never the production tag; asserted by `TestProductionMeterImageHasNoTestClock` / `TestTestclockMeterImageHasTheSeam`. |
| operator config keys | `version`, `instance`, `groups`, `rungs[].{...}`, `meter.{...}` | `pkg/opercfg/gonk-operator.v1.schema.json` | MATCH, verbatim. |
| KeySink Secret shape | name `gonk-key-<slug>`, key `LITELLM_API_KEY` | `internal/meter/keysink/keysink.go`, `k8s.go` | MATCH. `k8s.go`'s `SecretKey` constant, with an explicit "changing this breaks the agent image" comment. |
| metric names | `gonk_intake_*`, `gonk_meter_*` | `pkg/intake/metrics.go`, `internal/meter/metrics/metrics.go` | MATCH. Full meter list in the plan's Task 0 verification table. |
| `gonk_meter_spend_usd_total` labels | `project`, `rung`, `synthetic` | `internal/meter/metrics/metrics.go` | MATCH, plus `trigger` (real, not named in the original row). |

## Step 1 (continued): drift found beyond the original 8 known items

1. **Ledger backend.** Plan assumed `GONK_METER_STORE_BACKEND` accepts
   `dolt | postgres`. Shipped: `postgres` ONLY (`cmd/gonk-meter/main.go`'s
   `openStore`). No `store/dolt.go` exists. Dolt remains Gas City's beads
   store only. See ADR-004 Decision 10 and
   `docs/spikes/dolt-reservation-isolation.md`.
2. **Meter replica safety.** AD-10 (single-replica) is LIFTED. Postgres
   enforces `ReserveIfFits`'s atomicity across replicas
   (`TestReserveIfFitsRace`, `TestReserveIsIdempotentAcrossReplicas`,
   `internal/meter/store/postgres_race_test.go`). The chart must not
   fail-closed-guard `meter.replicaCount > 1`.
3. **Image `repository` values carried a spurious `gonk/` prefix**
   (`gonk/gonk-agent` etc.) not matching what the Makefile actually builds
   (`registry.orac.local/agentic/gonk-project/gonk-agent:<tag>`, no `gonk/`
   segment). Fixed in `values.yaml`.
4. **The `gonk.image` helper had no per-image registry override**, so
   `dolt.image` (`docker.io/dolthub/dolt-sql-server`, a third-party image)
   would have inherited the gonk-project private registry and rendered an
   unpullable path. Fixed: `.image.registry` now overrides the global
   default per-image; `dolt.image.registry: docker.io` set explicitly.
5. **ADR numbering.** Plan 04 (merged) took `ADR-005` for
   `pack-and-images.md`. This plan's chart-seams ADR is renumbered
   `ADR-006` throughout.
6. **Meter's egress NetworkPolicy had no rule for `ledger.postgres.mode:
   shared`** (the real target-cluster default) or `external` — only `cnpg`
   had a target. Fixed: `networkPolicy.ledgerPostgres.namespaceSelector`
   added (default: `databases-app`), with a namespaceSelector egress rule
   for the non-`cnpg` modes.
7. **Dolt's ingress NetworkPolicy admitted `gonk-meter`** ("the ledger, when
   `ledger.backend: dolt`") — that state never shipped. Fixed: only the Gas
   City controller is an admitted ingress source to the bundled Dolt.

## Step 2: the two contract additions Plan 03 owns

**(a) Meter store backend** — VERIFIED SHIPPED, with the backend-enum
correction above.

```
grep -rn "GONK_METER_STORE_BACKEND\|GONK_METER_STORE_DSN_FILE" cmd/gonk-meter/
# -> both present, loadConfig + openStore

grep -rn "func OpenDolt\|func NewPostgres" internal/meter/store/
# -> no OpenDolt; NewPostgres in postgres.go
```

**(b) `keysink.K8s`** — VERIFIED SHIPPED.

```
grep -rn "func NewK8s" internal/meter/keysink/
# -> k8s.go, func NewK8s(cs kubernetes.Interface, namespace, prefix string) *K8s

grep -rn "GONK_KEYSINK_NAMESPACE" cmd/gonk-meter/
# -> main.go loadConfig + openKeysink
```

Both present; neither reimplemented here. This chart still owns the
`Role`/`RoleBinding`/`ServiceAccount` (Task 4) that make `k8s.go`'s writes
legal on a real API server -- unchanged by this reconciliation.

## Reference documents read for this pass

- `docs/adr/ADR-004-rung-policy-and-budget-enforcement.md` (Decision 10; "The
  ledger is multi-replica-safe; AD-10 is lifted"; the network-layer bypass).
- `docs/spikes/dolt-reservation-isolation.md` (the Task 0b spike: 32/32
  writers won a ceiling-of-2 race, 12.8x overspend, at every isolation level,
  90 iterations, zero variance; `SELECT ... FOR UPDATE` independently
  confirmed to be a no-op on Dolt 2.1.10).
- `docs/environment.md` (cluster facts snapshot; ledger backend settled
  section; Gas City controller deployment facts; the three still-unsettled
  controller unknowns).
- `cmd/gonk-meter/main.go`, `cmd/gonk-intake/main.go` (env-var contracts).
- `internal/meter/store/` (`postgres.go`, `memory.go`; no `dolt.go`).
- `internal/meter/keysink/keysink.go`, `k8s.go`.
- `internal/meter/metrics/metrics.go`, `pkg/intake/metrics.go`.
- `pkg/opercfg/gonk-operator.v1.schema.json`.
- `images/versions.env`, `images/Dockerfile.{agent,controller,intake,meter}`,
  `Makefile` (image names/tags, the `no-latest` gate, the testclock variant).
- `docs/adr/ADR-001` through `ADR-005` (index; confirms `ADR-005` is taken by
  `pack-and-images.md`, so this plan's chart-seams ADR is `ADR-006`).

## Addendum: Task 2 test-fixture drift found by TDD (not a Task 0 item, noted here for the same audience)

Three of Task 2's own `guards_test.go` cases, copied verbatim from the plan
text, failed against the code actually shipped in this worktree (Task 1's
`values.yaml`/`values.schema.json`, committed separately). All three are test
bugs, not chart bugs; fixed in `internal/charttest/guards_test.go`:

1. **`TestGuardOnboardingRungNotInInstanceLadder`** used plain `--set` for
   `operatorConfig.rungs[1].est_cost_usd=0.4`. Helm's plain `--set` never
   infers `float64` (only int64/bool/string), so `0.4` arrived at
   `values.schema.json` as the *string* `"0.4"` and failed `"type": "number"`
   before the G5 guard this test targets ever ran -- the exact trap
   `Minimum()`'s own comment already documents for `synthetic_usd_per_1m_tokens`.
   Fixed: `--set-json`.
2. **`TestGuardIngressWithoutHost` / `TestGuardIngressWithoutClassName`**
   assumed `ingress.host` / `ingress.className` have no default. They do:
   OD-2 (`docs/environment.md`) answered them with real cluster values
   (`gonk.orac.local`, `traefik`), so the un-set field was never actually
   empty and the render succeeded. Fixed: both tests now explicitly blank the
   field under test.
3. **`TestGuardLedgerNeedsDSNSecret`** (G19) expected the guard's own message
   (`secrets.ledger.existingSecret`). `values.schema.json`'s
   `$defs.secretRef` already requires `existingSecret` to be non-empty
   (`minLength: 1`, `additionalProperties: false` -- no `--set-json` smuggling
   route around it) for every secret ref, `secrets.ledger` included. Layer A
   (the schema) rejects this misconfig before layer B's (`_guards.tpl`'s) G19
   `fail()` ever runs -- the same "belt-and-braces" relationship G3/G4
   document for the rung catalog, except here there is no relaxed-schema path
   that would ever let G19 fire. `_guards.tpl` keeps the G19 `fail()` as
   written (harmless, and a real backstop if `minLength` is ever relaxed);
   the test now asserts the schema's actual (slash-path) error text instead
   of the guard's dot-path message.

None of these change chart behavior: the render still fails closed in all
three cases, with an actionable message. They only change which layer (A vs
B) is observed to fire, and the test file was corrected to match.
