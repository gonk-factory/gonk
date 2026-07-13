# gonk plan index

Spec: docs/superpowers/specs/2026-07-12-gonk-stack-design.md

| Plan | Scope | Status |
|---|---|---|
| 01 foundation & config contract | scaffold, CI, gonkcfg, atags | done |
| 02 gitlab-intake | webhooks, reconciliation, onboarding MR | not started |
| 03 gonk-meter | rung policy, key provisioning, ledger | not started |
| 04 pack & images | agents/formulas/orders, docker images | not started |
| 05 chart | Helm chart, BYO seams | not started |
| 06 e2e harness | kind + gitlab-ce + stub model, kill tests | not started |

Update the Status column as tasks complete (house rule: progress lives here).

## Contracts published by plan 01

- `pkg/gonkcfg` — `.gonk.yml`: schema (`Validate`), typed load (`Load`), and
  precedence resolution (`Resolve` -> `Effective`). Semantics are ADR-002.
- `pkg/atags` — attribution tags joining LiteLLM spend to beads/sessions.
  The literal key and trigger strings are the ledger contract (spec 10.1).
- `docs/schemas/gonk-config.v1.schema.json` — published schema, gated against
  drift from the embedded canonical copy and against accidental change.

## Carried into later plans

- **Plan 03 (highest-value item):** `Effective.Budget`'s "unlimited" sentinels
  are `math.Inf(1)` (`MonthlyCostUSD`) and `math.MaxInt64`
  (`MonthlyTokens`/`PerTaskTokens`). `gonk-meter`'s ledger/API will serialize
  `Effective.Budget`, and **`+Inf` is not JSON-serializable** —
  `encoding/json.Marshal` returns an error rather than a number. Special-case
  it (e.g. `null`, a sentinel string, or omission) before marshaling, or an
  unlimited project will fail to serialize. See ADR-002.
- **Whoever owns schema v2:** `SchemaVersion` was a decorative, unreferenced
  constant until this pass — it is now enforced by exactly one test
  (`TestSchemaVersionConstMatchesEmbeddedSchema` in
  `pkg/gonkcfg/drift_test.go`), which fails if the const and the schema's
  `properties.version.const` drift apart. Keep both in sync when cutting v2.
- **Plan 05:** the instance-level ladder is load-bearing operator config, not
  optional. Per spec 5.4 the ladder is an allow-list, so silence at a layer
  means "impose no constraint," not "allow nothing" — if the operator sets no
  instance ladder, a project may name any rung it likes, bounded only by
  budget ceilings. Total silence at *every* layer fails closed (see ADR-002),
  but partial silence (operator silent, project sets a ladder) is fail-open on
  rung choice. **The chart's default values should ship a non-empty instance
  ladder** rather than relying on an empty one to be safe.
- **Plan 03:** nothing validates operator-supplied instance/group `Policy` —
  they bypass the schema and the non-finite-float check. `Resolve` fails closed
  on the one dangerous case (a NaN ceiling), so this is not exploitable today,
  but the operator-config path needs its own validation. See ADR-002 "Known gap".
- **Plan 03:** `atags` accepts any string for `Project`/`Rung`. Safe inside the
  package, but a value containing a newline or comma could cause injection or
  column-shift bugs if metadata is serialized into a header, log line, or CSV
  downstream. Enforce at the boundary, not in the contract package.
- **CI:** the `lint`/`test` jobs have never run — every runner on
  gitlab.orac.local was offline during plan 01. The standing gate is local:
  `gofmt -l .`, `go vet ./...`, `go test ./... -race -count=1`,
  `golangci-lint run ./...`.
- **Lint version skew:** the local gate runs golangci-lint v2.12.2 while CI
  pins v2.1.6. Both are green today, but they bundle different staticcheck
  versions and can disagree later — worth pinning the local tool to match CI,
  or reconciling the CI pin forward, before it causes a confusing local-pass/
  CI-fail split.
