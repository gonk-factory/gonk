# ADR-006: the chart's umbrella scope, its seams, and its fail-closed guards

Status: accepted 2026-07-17

This ADR records the architecture decisions Plan 05 (`chart/gonk`,
`internal/charttest`) locks in — what the chart deploys and what it delegates,
how it fails closed, and the limitations it ships with its eyes open. It
cross-references ADR-002 (config precedence), ADR-003 (Plan 02's intake trust
boundary), ADR-004 (Plan 03's rung policy and budget enforcement, and the ledger
spike), and ADR-005 (Plan 04's pack and images).

A numbering note, because a future maintainer will otherwise "fix" it: the chart
plan's own text sometimes calls this document "ADR-004" or "ADR-005". Both numbers
are taken on `main` (Plan 03's and Plan 04's). This document is **ADR-006**. Go by
`ls docs/adr/`, never by a plan document's guess at its own number.

---

## 1. The chart is an UMBRELLA, not a pack-plus-glue

**Owner decision 2026-07-13.** The chart deploys the whole factory: the Gas City
controller, Dolt (beads), `gonk-intake`, `gonk-meter`, and the operator config,
each behind a `<component>.enabled` toggle. It wires to a **BYO** GitLab, LiteLLM,
and CNPG operator.

This **reverses the earlier "Gas City and Dolt are BYO" framing** (OD-4). The
reason is concrete: the target cluster has neither a Gas City controller nor a Dolt
server, so a pack-plus-glue chart that assumed them would deploy nothing that runs.
An umbrella that stands up its own controller and beads store is the only shape
that produces a working factory on this cluster. The default render is therefore
the **whole factory** — proven by `TestChartLintsAndRendersWithDefaults` and the
`values-default` golden (27 objects: controller + Dolt + intake + meter + operator
config).

## 2. Only LiteLLM is BYO among the model/runtime layer (OD-3)

`litellm.mode` is a single-value enum, `external`. The chart bundles no LiteLLM and
no Ollama. On the target cluster LiteLLM is already deployed (`litellm:4000`); the
chart consumes it by URL. The cost: the **synthetic-price agreement** between the
rung catalog and LiteLLM's `model_cost_map` is unenforceable at chart time — a
local model priced at `$0` in LiteLLM would leave the token ceiling open. The chart
mitigates by rendering an **advisory** `gonk-litellm-model-prices` ConfigMap with
the same prices; Plan 06 verifies the operator actually pasted them.

## 3. The Gas City controller is templated, not charted — and image-gated

There is no upstream Gas City chart, so this chart templates the controller as a
first-party workload. Three deploy facts could not be derived from design and were
settled by a **smoke deploy** against real tooling (Task 0.5,
`chart/gonk/smoke/gc-controller-smoke.md`):

- **supervisor port 9443** (the per-city `[api]` listener; port 8372 is a
  loopback-only admin API and is never exposed);
- **prebaked pack delivery** (`/opt/gonk/pack` copied into a writable `/city`, then
  `gc init --preserve-existing --bootstrap-profile k8s-cell`; a read-only ConfigMap
  at `/city` is not viable);
- **workload = Deployment, `strategy: Recreate`** (single-instance; no leader
  election exists).

**This is IMAGE-GATED (bead `gonk-fsl`).** The Plan 04 `gonk-controller` image is
missing `dolt`, `tmux`, `jq`, `lsof`, `pgrep` and bundles `bd < 1.0.4`, so `gc
init` cannot yet finish a city; the 9443 listener is config-derived, not
socket-confirmed. The chart encodes the settled **shape** and the tests assert it
**renders**; a re-smoke after the image fix is owed. The chart's dependency on
Plan 04's image (`gascity.image`) is hard and partially preceding.

## 4. Beads is Dolt-only; CNPG is the ledger only

There is no beads-on-Postgres path (`docs/environment.md`: the beads backend enum
is Dolt/DoltLite; sqlite/coordstore were removed and hard-error). So
`dolt.enabled: false` means an **external Dolt** (e.g. self-hosted DoltLab),
guarded by G16 — never "Postgres for beads". Conversely, CNPG can only ever replace
the **ledger**; the bundled Dolt stays deployed regardless of
`ledger.postgres.mode`. The two are orthogonal, proven by `values-ledger-cnpg`
(Dolt + a CNPG ledger Cluster render together).

## 5. The ledger is Postgres, unconditionally — the spike settled it

`ledger.backend` is a **single-value enum**, `postgres` (ADR-004 Decision 10).
Task 0b's spike (`docs/spikes/dolt-reservation-isolation.md`) ran: 32 concurrent
writers raced a ceiling with headroom for 2, and **every one of 90 iterations
overspent 12.8×**, at every isolation level including `SELECT ... FOR UPDATE` — a
measured no-op on Dolt 2.1.10. `cmd/gonk-meter/main.go`'s `openStore` has exactly
one case, `postgres`; there is no `store.OpenDolt`. So `ledger.backend: dolt` is
not a value the chart may offer — it would render a meter that **fails to start**,
not one with a different durability property. The `postgres` enum exists so a
future backend can be added without a values-key rename, and so the G15 render-time
`fail` has something to check when someone sets `dolt` by hand.
`ledger.postgres.mode` (shared / cnpg / external) is the only real ledger switch,
and all three modes render, lint, and validate independently.

## 6. Secrets are file mounts from externally-provisioned Secrets, two slots

**The chart renders no `Secret` and uses no `secretKeyRef` anywhere.** Every
credential is provisioned outside the chart (Vault/1Password → ExternalSecret → k8s
Secret) and consumed as a **projected file mount** (`defaultMode: 0400`,
`readOnly`), passed to the binary as a `*_FILE` env var — **never** `envFrom`,
**never** `valueFrom.secretKeyRef`, because env leaks into `/proc/<pid>/environ`,
crash dumps, and every child process (owner decision 2026-07-13). This is a
**deliberate deviation from the gitops repo's `env` house style**; ESO *delivery*
is identical (same ClusterSecretStore `vault-backend`, same `eso/gonk/<concern>`
paths, same rotation annotations as `apps/renovate/`), only *consumption* differs.
Recorded here so it is not "fixed" later. Every credential has **two rotation
slots** (slot 2 opt-in per credential, because a projected volume with an absent
key fails the pod to start). `secrets_test.go` enforces the whole contract
key-by-key and asserts **no `Secret` object and no `secretKeyRef`** in the render.

## 7. Meter is single-replica by default — a footprint choice, not a guard

`meter.replicaCount: 1` + `strategy: Recreate` is the default for the modest initial
footprint. It is **no longer a correctness guard**: AD-10 is lifted (§5; the
atomicity is in Postgres, proven across replicas). The chart deliberately has **no**
`fail` on `meter.replicaCount > 1` and no `unsafeAllowMultipleReplicas` key — a
higher value renders cleanly. The **controller**, separately, must stay
single-instance (no leader election), enforced by `Recreate`/StatefulSet.

## 8. The default install cannot spend a dollar (AD-4)

`operatorConfig.instance.budget.monthly_cost_usd` defaults to `0`, and the chart
ships no rung catalog and no ladder — so the safe default cannot spend, and in fact
cannot render until the operator supplies a ladder/catalog/defaultRung (a
site-local default naming a phantom model would 404 at first token; Plan 04 AD-1).
For a system whose failure mode is **money**, the right default is the one that
spends nothing and forces three deliberate edits (catalog + ladder + ceiling) to
enable cloud spend. `values-full-monitoring` is the only profile that makes all
three.

## 9. The guards are three layers; G1–G20

Fail-closed is enforced in three places, cheapest first:

- **Layer A — `values.schema.json`** (Helm enforces it before any template runs):
  image tags non-empty and not `latest` (G11), the ledger-backend enum (G15), the
  secret-ref `minLength`/`additionalProperties` shape (G19's backstop), etc. These
  are not "loud", they are impossible.
- **Layer B — `_guards.tpl` `fail`s** (G1–G20): everything the schema cannot
  express — a ladder rung absent from the catalog (G2), a local/cloud rung with the
  wrong price shape (G3/G4), an onboarding default outside the ladder (G5),
  unmetered dispatch (G8/G18), a BYO controller with no URL (G9/G17), a disabled
  Dolt with no external host (G16), an empty ledger DSN in any Postgres mode (G19),
  an empty/everything NetworkPolicy selector (G10), a widened ingress path (G12),
  and the bundled Dolt/controller image-tag pins (G20).
- **Layer C — startup** (the binaries' own fail-closed on missing files / empty
  ladder), out of this chart's scope but the reason the mounts are `optional: false`.

Each guard message says **what breaks**, not just that something is wrong — a guard
nobody can act on gets disabled.

## 10. `helm unittest` was replaced by Go tests

The chart gate is `go test -tags chart ./internal/charttest/...`, driving the real
`helm` binary, not `helm unittest` (spec 10.3). Two reasons: no plugin to install
on an offline runner, and — the one that matters — a Go test can assert that
`helm template` **FAILS with a specific message**, which is what every fail-closed
guard in this chart is. The same suite renders each `ci/` profile, diffs it against
a **golden manifest** (the drift gate; regenerate with `-update` and read the
diff), `helm lint --strict`s it, and `kubeconform -strict` validates it against the
Kubernetes schemas plus vendored CRD schemas (`chart/gonk/tests/crd-schemas`, **not**
`-ignore-missing-schemas`, which would silently skip the CRDs).

## 11. THE NETWORKPOLICY GAP — an accepted, UN-CLOSED limitation

Not a mitigated risk; an open one. **NetworkPolicies are not enforced on this
cluster** — Flannel does not implement them, and the Cilium HelmRelease that would
is suspended. LiteLLM routes local models to an **unauthenticated Ollama**
(`192.168.1.142:11434`), so an agent pod can bypass metering entirely.
**Consequence: cloud-rung budgets are hard (a cloud call needs the per-project
virtual key, and LiteLLM refuses at the ceiling); local-model budgets are
advisory.** Spec §9's "budgets cannot be bypassed" is, at the network layer for
local models, a **known-false** statement today, not merely unverified.

**Owner decision 2026-07-13: ship the policy, document the gap, do not gate on it.**
The policies are correct and start enforcing the day Cilium is unsuspended. The
gap is closed by infra work outside gonk (unsuspend Cilium, or credential Ollama),
and by **un-skipping Plan 06's egress-denial test — which is the gate.** The
decision is recorded here *with the fact that it is still open*. The gap is stated
loudly in four places — every `networkpolicy-*.yaml`, `values.yaml`,
`chart/gonk/README.md` §0, and this ADR — and
`TestEveryNetworkPolicyDeclaresItIsNotEnforced` fails the build if any policy loses
its banner.

## 12. The agent-pod NetworkPolicy selector (OD-5) is an unverified assumption

`networkPolicy.agentPodSelector` defaults to `app: gc-agent` (read from Gas City's
`internal/runtime/k8s/pod.go`, unverified live). It **will still be an unverified
assumption on the day Cilium lands** — a *wrong* selector renders fine and enforces
nothing. G10 refuses an **empty** selector; it cannot detect a **wrong** one. The
proof is the test, not the template: Plan 06's egress-denial run, and only that.
This is the most dangerous unknown in the chart, and it is called out as such.

## 13. Flux consumes the chart as an OCI artifact; charts are never vendored

The deployment lives in `gitops:clusters/orac/apps/gonk/`; the chart is pulled from
`oci://registry.orac.local/agentic/gonk-project/charts` by an `OCIRepository`. The
gitops repo's `charts/` root is gitignored — vendoring a chart into it is rejected.
One Kubernetes object per file, `<kind>-<name>.yaml`; `apps/renovate/` and
`apps/nagus/` are the templates to copy.

---

## What this ADR does not decide (handed forward)

- **Whether the NetworkPolicy actually enforces** — infra (Cilium) + Plan 06's
  un-skipped test. Open.
- **The controller image completeness** — Plan 04 (`gonk-fsl`); a re-smoke is owed.
- **The chart's OCI publish target** (OD-8) and the CI tools image (OD-7) — moot
  until a runner exists; both are documented-manual in `.gitlab-ci.yml`.
