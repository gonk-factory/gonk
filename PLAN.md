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
