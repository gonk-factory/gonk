# gonk — the chart

A local software factory for a self-hosted GitLab: a Gas City pack plus the glue
that runs it. This chart is an **umbrella** — it deploys the Gas City controller,
Dolt (beads), `gonk-intake`, `gonk-meter`, and the operator config together, each
component individually toggleable, wiring to a **BYO** GitLab, LiteLLM, and CNPG
operator. `values.schema.json` is the first gate; a set of `fail`-closed guards
(`_guards.tpl`, G1–G20) is the second.

> The defaults are **safe, not useful**. Out of the box this chart cannot spend a
> dollar (`operatorConfig.instance.budget.monthly_cost_usd: 0`, and the only
> shippable rung is local), exposes nothing (`ingress.enabled: false`), and
> refuses to render without the image tags, URLs, and operator ladder/catalog it
> cannot guess. You make it useful on purpose, in the open.

---

## 0. ⚠ WHAT IS AND IS NOT ENFORCED — read this before anything else

gonk ships an egress `NetworkPolicy` that would confine agent pods to GitLab and
LiteLLM. **On the orac cluster it is not enforced.** Flannel does not implement
NetworkPolicy, and the Cilium HelmRelease that would is suspended. An agent pod
can therefore reach **Ollama at `http://192.168.1.142:11434`, which requires no
credential**, bypass LiteLLM entirely, and burn local GPU with **no metering and
no ceiling**.

**What this means for your budgets, precisely:**

- **Cloud-rung budgets are HARD.** A cloud call needs an API key. The only key an
  agent pod ever holds is the per-project LiteLLM virtual key, and LiteLLM refuses
  at the ceiling. This holds today.
- **Local-model budgets are ADVISORY.** They are enforced *if* the traffic goes
  through LiteLLM, and nothing currently forces it to.

The policy is shipped anyway — it is correct, and it starts enforcing the day
Cilium is unsuspended. Plan 06 carries a written, **skipped** egress-denial test;
**un-skipping it is the gate.** Closing this gap is infra work outside gonk:
unsuspend Cilium, or put a credential in front of Ollama.

**Do not repeat spec §9's "budgets cannot be bypassed" in a dashboard, a demo, or
a status update. It is not true yet.** The honest phrasing, everywhere:
**cloud-rung budgets are hard; local-model budgets are advisory.**

The same banner is on every `networkpolicy-*.yaml` template, in `values.yaml`, and
in `docs/adr/ADR-006`. A test (`TestEveryNetworkPolicyDeclaresItIsNotEnforced`)
fails the build if a policy template loses it.

---

## 1. The secret provisioning contract (the hard requirement)

**No secret value appears in `values.yaml`. No secret is committed. No secret is
an env var.** The path is: **Vault/1Password → ExternalSecret → k8s Secret →
projected file mount → the binary reads the file.** Every credential has **two
rotation slots** so a rotation is not an outage.

**The chart creates no Secrets. If these do not exist, the pods will not start.**
Projected volumes are `optional: false`, which is the loud failure we want.

Every one of these is delivered by an **ExternalSecret** in the gitops repo, from
ClusterSecretStore **`vault-backend`**, Vault KV path **`eso/gonk/<concern>`**.

> ***Write the Vault value BEFORE merging the ExternalSecret.*** A missing key
> leaves the ExternalSecret NotReady and can **wedge the entire Flux reconcile** —
> not just gonk's. This has already happened once on this cluster.

| Secret (default name) | Key | Vault path | Mounted at | Env var | Consumer |
|---|---|---|---|---|---|
| `gonk-gitlab` | `token` | `eso/gonk/gitlab` | `/etc/gonk/secrets/gitlab/token` | `GONK_GITLAB_TOKEN_FILE` | intake |
| `gonk-gitlab` | `token-previous` *(rotation slot 2, opt-in)* | `eso/gonk/gitlab` | `/etc/gonk/secrets/gitlab/token-previous` | `GONK_GITLAB_TOKEN_PREVIOUS_FILE` | intake |
| `gonk-gitlab` | `admin-token` *(only when `gitlab.mode=split-credential`)* | `eso/gonk/gitlab` | `/etc/gonk/secrets/gitlab/admin-token` | `GONK_GITLAB_ADMIN_TOKEN_FILE` | intake |
| `gonk-webhook` | `token` | `eso/gonk/webhook` | `/etc/gonk/secrets/webhook/token` | `GONK_WEBHOOK_SECRET_FILE` | intake |
| `gonk-webhook` | `token-previous` *(rotation slot 2, opt-in)* | `eso/gonk/webhook` | `/etc/gonk/secrets/webhook/token-previous` | `GONK_WEBHOOK_SECRET_PREVIOUS_FILE` | intake |
| `gonk-meter-api` | `token` | `eso/gonk/meter-api` | `/etc/gonk/secrets/meter-api/token` | `GONK_METER_TOKEN_FILE` | **intake and meter** |
| `gonk-meter-api` | `token-previous` *(slot 2, opt-in)* | `eso/gonk/meter-api` | `/etc/gonk/secrets/meter-api/token-previous` | `GONK_METER_TOKEN_PREVIOUS_FILE` | **meter only** (it is the verifier; intake presents slot 1) |
| `gonk-litellm` | `admin-key` | `eso/gonk/litellm` | `/etc/gonk/secrets/litellm/admin-key` | `LITELLM_ADMIN_KEY_FILE` | meter |
| `gonk-litellm` | `admin-key-previous` *(slot 2, opt-in)* | `eso/gonk/litellm` | `/etc/gonk/secrets/litellm/admin-key-previous` | `LITELLM_ADMIN_KEY_PREVIOUS_FILE` | meter |
| `gonk-ledger` | `dsn` | `eso/gonk/ledger` *(or CNPG's generated Secret)* | `/etc/gonk/secrets/ledger/dsn` | `GONK_METER_STORE_DSN_FILE` | meter |

Plus one **non-credential** mount, just as load-bearing:

| ConfigMap | Key | Mounted at | Env var | Consumer |
|---|---|---|---|---|
| **`trust-bundle`** (published into every namespace by trust-manager) | `tls-ca-bundle.pem` | `/etc/ssl/orac/ca.crt` | **`SSL_CERT_FILE`** | **intake** (and meter, for symmetry) |

That is how a Go service trusts `https://gitlab.orac.local`'s **private CA**. Go's
`crypto/x509` reads `SSL_CERT_FILE` with no application code at all. **There is no
value in this chart that disables TLS verification, and there never will be.** (A
test greps the render for `sslVerify: false`, `insecure`, and `InsecureSkipVerify`
and fails on a hit.)

**Deliberate deviation from house style — do not "fix" it.** The gitops convention
consumes secrets as **env** (`secretKeyRef`); gonk consumes them as **file mounts**
(owner decision 2026-07-13), because env leaks into `/proc/<pid>/environ`, crash
dumps, and every child process. ESO *delivery* is identical (the same
ClusterSecretStore, the same Vault paths, the same rotation annotations as
`apps/renovate/`'s PAT); only *consumption* differs. Say so, or the next person to
read `verify-conventions.sh` will think it is a mistake.

**Example ExternalSecret** (one per Secret above; this is the `gonk-gitlab` one —
copy the shape, change `name` / `remoteRef.key` / `data` keys):

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: gonk-gitlab
  namespace: gonk
  annotations:
    homelab.orac.local/rotation: enabled     # exactly as apps/renovate/'s PAT
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: vault-backend
  target:
    name: gonk-gitlab                          # the Secret gonk mounts
  data:
    - secretKey: token
      remoteRef: {key: eso/gonk/gitlab, property: token}
    # add token-previous only once you have written it to Vault, and only when
    # you set secrets.rotation.gitlabPrevious: true — a projected volume with a
    # key that does not exist makes the pod fail to start.
```

### Rotation — uniform across all three credentials, no exception, no outage

Both GitLab PAT slots are mounted, so there is no single-slot exception any more.
The procedure is the same for every credential:

1. Write **new → slot 1**, **old → slot 2** in Vault.
2. Set `secrets.rotation.<x>Previous: true` (e.g. `gitlabPrevious`).
3. For the **webhook token only**, also bump `intake.webhookTokenGen` so reconcile
   repairs every GitLab hook to carry the new generation.
4. `helm upgrade`.
5. Wait one `intake.reconcileInterval`.
6. Drop slot 2 from Vault; set `secrets.rotation.<x>Previous: false`; upgrade.

**The two meanings of "slot 2" — they are not the same mechanism:**

- A credential gonk **verifies** (the webhook token, the meter bearer): **both
  slots are accepted** during the overlap. No request fails mid-rotation.
- A credential gonk **presents** (the GitLab PAT): **slot 1 is presented, and a
  `401` falls back to slot 2 once**, incrementing
  `gonk_intake_gitlab_auth_fallback_total`. **Alert on that counter being > 0: it
  means a rotation is half-finished** (slot 1 is wrong and slot 2 is carrying you).

### The required image pull secret

Images live in the in-cluster GitLab registry (`registry.orac.local`). Every pull
needs the per-namespace Secret **`gitlab-registry-pull-creds`** — itself an
ExternalSecret; **the chart does not create it.** `image.pullSecrets` references it
by name.

---

## 2. How gonk is actually deployed: Flux, from the gitops repo

`helm install` is for a laptop. The real path is Flux consuming the chart as an
**OCI artifact**; charts are **never vendored** into the gitops repo (`charts/` at
its root is gitignored). File by file, because the gitops repo's pre-commit hook
rejects anything else:

| File in `gitops:clusters/orac/` | What |
|---|---|
| `sources/ocirepository-gonk.yaml` | points at `oci://registry.orac.local/agentic/gonk-project/charts` |
| `apps/gonk/namespace-gonk.yaml` | ns `gonk`, with the `homelab.orac.local/service-tier` + `data-tier` labels |
| `apps/gonk/helmrelease-gonk.yaml` | the HelmRelease, `targetNamespace: gonk` |
| `apps/gonk/externalsecret-gonk-gitlab.yaml` | …and one file per Secret. **One K8s object per file, named `<kind>-<name>.yaml`.** |
| `apps/gonk/externalsecret-gitlab-registry-pull-creds.yaml` | image pulls |
| `apps/gonk/externalsecret-gonk-ledger.yaml` | the DSN — **the second one; ESO cannot replicate across namespaces** |
| `apps/gonk/kustomization.yaml` | **must list every yaml above, or the commit fails** |
| `apps/kustomization.yaml` | register `gonk/` |
| `foundation/coredns/configmap-coredns.yaml` | **add `gonk.orac.local` to the hosts block, or in-cluster resolution fails silently** |

**`apps/renovate/` is gonk's near-twin** (GitLab bot PAT + webhook + CA trust) and
**`apps/nagus/`** is the model for a first-party image + shared CNPG. **Copy them.
Do not invent a fifth way.**

The required CoreDNS hosts entry is not optional: without `gonk.orac.local` in the
CoreDNS hosts block, in-cluster resolution of the ingress host fails silently and
the webhook simply never arrives.

---

## 3. The ledger (Postgres) — and the four objects `mode: shared` needs

`ledger.backend` is Postgres, always (`ledger.backend` has exactly one valid value;
ADR-004 Decision 10 / `docs/spikes/dolt-reservation-isolation.md`). What varies is
`ledger.postgres.mode`:

- **`shared`** (default) — tenancy on the CNPG cluster the owner already runs
  (`databases-app/postgres`). The chart renders **nothing** for it; it just
  consumes the DSN.
- **`cnpg`** — the chart emits a new `postgresql.cnpg.io/v1` `Cluster`
  (`gonk-ledger-postgres`). Requires the CNPG operator; the chart does not install
  it. CNPG generates its own app Secret — point `secrets.ledger.existingSecret` at
  it (with a `dsn`-shaped key) or synthesize your own. **The chart never copies a
  credential from one Secret into another.**
- **`external`** — nothing rendered; meter reads the DSN you provide.

**`mode: shared` needs FOUR objects in the gitops repo, not one.** This is the
single most commonly got-wrong thing, and getting it wrong fails in a way that
looks like a network problem. `apps/nagus/` is the worked example — copy it.

| # | Object | Where | Why it cannot be anywhere else |
|---|---|---|---|
| 1 | A **managed role** `gonk` on the shared `Cluster` (`spec.managed.roles`) | gitops, patching `databases-app/postgres` | CNPG creates DB roles declaratively; a hand-made role drifts away on reconcile. |
| 2 | An **ExternalSecret** for that role's password, **in `databases-app`** | gitops, `databases-app` | The `Cluster`'s `passwordSecret` must be readable **by CNPG**, in the cluster's own namespace. |
| 3 | A **`Database` CR** (`postgresql.cnpg.io/v1`) naming `gonk_ledger`, owner `gonk` | gitops, `databases-app` | This is how a database is created on a **shared** cluster. `bootstrap.initdb` applies to a **new** cluster only — it will **not** create a database on an existing one, and reaching for it here is the classic mistake. |
| 4 | A **second ExternalSecret**, **in the `gonk` namespace**, producing `gonk-ledger` with a `dsn` key | gitops, `apps/gonk/` | **ESO cannot replicate a Secret across namespaces.** The same Vault value is materialised twice, by two ExternalSecrets. There is no `copy` — and this is exactly the step people skip, after which meter's pod cannot start and the error looks like DNS. |

The DSN (object 4) is what `GONK_METER_STORE_DSN_FILE` points at, and it must use
the **`-rw`** Service, never `-ro`:
`postgres://gonk:<pw>@postgres-rw.databases-app.svc.cluster.local:5432/gonk_ledger`.
A reservation written to a read-only replica is a reservation that does not exist,
and a reservation that does not exist is a budget that gets spent twice.

---

## 4. Component toggles and BYO seams

The umbrella deploys Gas City + Dolt + intake + meter + the pack, each
`<component>.enabled`, and wires to a BYO GitLab / LiteLLM / CNPG operator.
**Turning a component off makes its external replacement required** (fail-closed):

| Knob | Off ⇒ required | Guard | Worked example (a `ci/` profile renders it) |
|---|---|---|---|
| `gascity.enabled: false` | `gascity.supervisorURL` | G17 | `values-byo-gascity`, `values-byo-everything` |
| `dolt.enabled: false` | `dolt.external.host` | G16 | `values-external-dolt`, `values-byo-everything` |
| `meter.enabled: false` (with intake on) | — (**refuses**: unmetered dispatch) | G8/G18 | fail-closed cell in the toggle matrix |
| `ledger.postgres.mode` | a DSN Secret in **all three** modes | G19 | `values-cnpg`, `values-byo-everything` |
| `ingress.enabled` | `className` + `host` | G12 | `values-cnpg`, `values-full-monitoring` |
| `networkPolicy.enabled` | non-empty `agentPodSelector` + a GitLab reach | G10 | `values-full-monitoring` |

Two invariants, stated plainly:

- **Beads is Dolt-only.** There is no beads-on-Postgres path. `dolt.enabled: false`
  means an **external Dolt**, never "Postgres for beads".
- **CNPG is the ledger only.** It never backs beads. The bundled Dolt stays
  deployed regardless of `ledger.postgres.mode` (see `values-ledger-cnpg`).

**The Gas City controller** has no upstream chart; this chart templates it. Three
of its deploy facts were settled by a smoke deploy (Task 0.5), not by design —
see `smoke/gc-controller-smoke.md` for the evidence:

- **`gascity.supervisorPort: 9443`** — the per-city `[api]` listener
  (`bind=0.0.0.0`, `allow_mutations=true`) that intake dispatches to and the
  Service/probes target. Port **8372** is a separate loopback-only supervisor
  admin API; it is **never** exposed by a Service/Ingress/NetworkPolicy.
- **`gascity.delivery: prebaked`** — the pack is baked into the image at
  `/opt/gonk/pack`; an initContainer copies it into a writable `/city` and runs
  `gc init --preserve-existing --bootstrap-profile k8s-cell`. A read-only ConfigMap
  at `/city` is not viable.
- **`gascity.workload: deployment`** with `strategy: Recreate` — no leader election
  exists, so exactly one controller must ever run; Recreate terminates the old pod
  before the new starts (`RollingUpdate`'s `maxSurge` could run two).

> **The controller image is currently incomplete (bead `gonk-fsl`).** The Plan 04
> `gonk-controller` image is missing `dolt`, `tmux`, `jq`, `lsof`, `pgrep` and
> bundles `bd < 1.0.4`, so `gc init` cannot yet finish a city and the 9443 listener
> is **config-derived, not socket-confirmed**. The chart renders the settled
> **shape**; the chart tests assert it **renders**, not that the pod boots. A
> re-smoke after the image fix is owed (`smoke/gc-controller-smoke.md`, open item 2).

---

## 5. Enabling a cloud rung — the deliberate three-part edit

A cloud rung costs real money, so turning one on is three edits, on purpose. Any
one alone does nothing (or fails the render); all three together is a decision:

1. **Add the rung to `operatorConfig.rungs`** — `kind: cloud`, `est_cost_usd > 0`,
   and **no** `synthetic_usd_per_1m_tokens` (a cloud price is real and lives in
   LiteLLM's own model config; declaring a synthetic one is a lie — G4 fails it).
2. **Add its name to `operatorConfig.instance.ladder`** — the ladder is an
   allow-list; a rung not in it is unreachable (ADR-002), and a rung named in the
   ladder but absent from the catalog fails the render (G2).
3. **Raise `operatorConfig.instance.budget.monthly_cost_usd`** above 0 — ceilings
   only tighten downward, so with a `$0` instance ceiling no project can spend a
   cent whatever its `.gonk.yml` says.

`values-full-monitoring.yaml` is the profile that makes all three edits; it is the
only `ci/` profile where a dollar can be spent.

And the synthetic-price firewall: for **local** rungs, the same
`synthetic_usd_per_1m_tokens` you put here must be pasted into LiteLLM's
`model_cost_map` (`clusters/orac/apps/litellm/litellm.yaml`). A local model priced
at `$0` in LiteLLM means the USD virtual-key ceiling never closes on tokens. The
chart renders an **advisory** `gonk-litellm-model-prices` ConfigMap with those
prices for you to copy; it cannot verify you did (Plan 06 does).

---

## 6. The single-replica warning for meter

`meter.replicaCount` defaults to **1** with `strategy: Recreate`. This is now a
**resource-footprint** choice, not a correctness guard — meter's `ReserveIfFits`
atomicity lives in Postgres (`SELECT ... FOR UPDATE` + a partial UNIQUE index),
proven across replicas (`internal/meter/store/postgres_race_test.go`); AD-10 is
lifted (ADR-004). You **may** raise it and the chart renders cleanly with no
fail-closed guard in the way. The **controller**, by contrast, must stay
single-instance (no leader election) — that is still a hard requirement, enforced
by `strategy: Recreate` / StatefulSet, for a different reason than meter's.

---

## 7. What this chart cannot verify — handed to Plan 06

A chart that renders is not a chart that works. `helm template` cannot prove any of
these; Plan 06's e2e harness does:

1. **The NetworkPolicy actually blocks — AND ON THIS CLUSTER IT DOES NOT** (§0).
   Plan 06 writes the egress-denial test (exec a session pod, `curl` Ollama at
   `192.168.1.142:11434`, require a timeout) and **skips** it until Cilium lands.
   Un-skipping it is the gate. **Lead with this one.**
2. **The Secrets actually exist and mount with those exact key names.** A missing
   Secret or wrong key is a `CreateContainerConfigError` at runtime, not a render
   error.
3. **The probes actually pass** — intake `/readyz` needs the bot identity, the
   first reconcile, and meter `/healthz`; meter `/readyz` is false until its first
   spend sync.
4. **The RBAC actually permits** meter's KeySink to write per-project Secrets (the
   Task 4 tests use a fake clientset, which does not enforce RBAC).
5. **CNPG's `Cluster` reconciles / the Dolt PVC binds / Dolt serializes the
   reservation race** (the Task 0b spike measured that it does **not** — that is
   why the ledger is Postgres).
6. **The Gas City controller actually runs as templated** — Task 0.5 settled the
   three deploy facts against real tooling but the image is incomplete (`gonk-fsl`);
   a full run (RBAC sufficiency, `/health` + `/v0/readiness` probes, the
   `gc init` → `.gc-start` sentinel, session spawn) is Plan 06's.
7. **The synthetic price agreement** — that the operator actually pasted the prices
   into LiteLLM (§5). Plan 06 diffs LiteLLM's `/model/info` against the catalog.
8. **ServiceMonitors actually scrape** and the **Grafana sidecar actually
   discovers** the dashboard ConfigMaps (label selectors must match).
9. **`helm install` on kind and on the real cluster**, idle = platform services
   only, zero agent pods.

---

## Quick reference: a laptop render

```bash
# render the safe default (the whole factory, nothing exposed, $0 ceiling)
helm template gonk chart/gonk --namespace gonk --values chart/gonk/ci/values-default.yaml

# the full chart gate (needs helm + kubeconform on PATH):
go test -tags chart ./internal/charttest/... -count=1

# regenerate the golden manifests after an INTENTIONAL template change, then READ the diff:
go test -tags chart ./internal/charttest/ -run Golden -update
```
