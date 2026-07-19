# Plan 06 L3 — the gonk chart against the REAL GitLab Orac

**Status: DONE.** The chart installed onto the live `admin@orac` cluster into a
throwaway namespace, all four gonk pods reached Ready, GitLab (18.10.1 CE)
delivered real webhooks to intake over the in-cluster Service, and the
deterministic onboarding MR was opened on the test project. The namespace, the
project's webhooks, and the GitLab admin setting were all torn down.

This is the FIRST time the `chart/gonk` umbrella was `helm install`-ed on a real
Kubernetes cluster (the CI profiles under `chart/gonk/ci/` are render-only
goldens; nothing had ever installed them). L3 caught one deploy-blocking config
bug that render goldens structurally cannot catch — a dead image tag — exactly
the class of defect this layer exists for.

## Run coordinates

| | |
|---|---|
| Cluster / context | `admin@orac` (cluster-admin), k8s v1.34 |
| Namespace | `gonk-e2e-1784441480` (throwaway; deleted at teardown) |
| GitLab | `https://gitlab.orac.local` (18.10.1 CE), in-cluster |
| Test project | id **75**, `agentic/gonk-e2e-1784441480` (NOT deleted — left for parent) |
| Bot | `gonk`, user id **49** (NOT deleted — left for parent) |
| Chart images | `gonk-intake:v0.1.0-65d404b58852`, `gonk-meter:v0.1.0-65d404b58852` (plain, not testclock), `gonk-controller:v0.1.0-470ca24be25a`, `dolthub/dolt-sql-server` (see bug below) |
| Profile | `chart/values-e2e.yaml` + a per-run overrides file (image tags, in-cluster URLs, `ledger.postgres.mode=external`, `dolt.persistence.storageClass=longhorn`, `gascity.writeAuth.verifyKey`) |

## What deployed cleanly

All secrets were provisioned as k8s Secrets in the file-mount shape the chart
expects (bot token, random webhook secret, random meter bearer, LiteLLM admin
key, ledger DSN, ed25519 gc-write PEM, registry pull creds from a freshly-minted
`read_registry` deploy token on project 69). The real `trust-bundle` ConfigMap
(key `tls-ca-bundle.pem`) was already present in the fresh namespace
(trust-manager), and the chart mounted it — intake resolved the bot identity over
`https://gitlab.orac.local` with the private CA and **no `InsecureSkipVerify`
anywhere**, confirming the SSL_CERT_FILE trust path works on a real cluster.

Throwaway ecosystem stood up in-namespace (deleted with the ns; production
LiteLLM / owner Postgres never touched): Postgres 16 (hosting both `gonk_ledger`
and LiteLLM's own `litellm` DB), a deterministic OpenAI-compatible stub upstream,
and **LiteLLM v1.92.0** (`ghcr.io/berriai/litellm-database:v1.92.0`, pinned)
whose only upstreams point at the stub.

`helm install` → `STATUS: deployed, REVISION: 1`. Guards: the full guard set
(`_guards.tpl`) rendered clean with the e2e values; G8/G19/G22 (write-auth,
ledger DSN, meter↔intake) were all satisfied by the provisioned secrets.

### Pods that reached Ready

| Pod | Result |
|---|---|
| `gonk-intake` | **Ready** — `/readyz` HTTP 200 (bot identity resolved + first reconcile complete + meter `/healthz` OK) |
| `gonk-meter` | **Ready** — `/readyz` HTTP 200. `/readyz` requires the FIRST SPEND SYNC to complete against real LiteLLM v1.92.0, so this Ready is meaningful (see note on the spend-poller adapter below). |
| `gonk-controller` | **Ready** — TCP :9443 probe passing; the `gc start --foreground` per-city `[api]` plane bound `0.0.0.0:9443` and grant-gating is on. (5 restarts while Dolt was unreachable — see Dolt bug.) |
| `gonk-dolt-0` | **Ready** — after the image-tag fix below. |

## The milestone — a real webhook from GitLab to intake

1. **Onboarding trigger**: bot (user 49) added as maintainer (access_level 40)
   of project 75.
2. **Reconcile forced** via `POST /admin/reconcile?wait=true` on intake's private
   listener (port-forward): `{"projects":1,"states":{"absent":1,...},"result":"ok"}`
   — the bot's one membership was picked up, project classified `absent`
   (needs onboarding).
3. **Webhook provisioned (P2-3)** — `GET /projects/75/hooks`:
   ```
   id 2  url http://gonk-intake.gonk-e2e-1784441480.svc.cluster.local:8080/hook/gitlab?gen=1
        issues=true  merge_requests=true  note=true  push=false  ssl_verify=false
   ```
   The `?gen=1` generation marker is present (P2-3 / ADR-003).
4. **Real delivery, GitLab → intake** — proven two independent ways:
   - intake metric `gonk_intake_webhook_total`:
     `{event="Merge Request Hook",outcome="bot_authored"} 1` and
     `{event="Issue Hook",outcome="accepted"} 1`.
   - GitLab's own hook delivery log (`GET /projects/75/hooks/2/events`): both
     deliveries returned **HTTP 200** (`merge_request_hooks` → `bot_authored`,
     `issue_hooks` → `accepted`). GitLab reached intake's internal Service
     in-cluster (`allow_local_requests_from_web_hooks_and_services` was already
     `true`; captured and restored unchanged).

   The MR-hook delivery is the bot's own onboarding MR firing back (intake
   correctly self-identifies via bot user id → `bot_authored`). A separately
   created issue produced the `Issue Hook` → `accepted` delivery.
5. **Onboarding MR created (P2-7)** — `GET /projects/75/merge_requests`:
   `!1 "gonk: enable automated issue triage"`, `gonk/onboard → main`, author
   `gonk`, **exactly one assignee `steve` (P2-4)**. Deterministic / zero-token:
   commit message `chore: add .gonk.yml (gonk onboarding)\n\nGenerated-By:
   gonk/0.1.0 (deterministic onboarding; no model)`. The shipped `.gonk.yml`
   is `version: 1`, `enabled: true`, ladder `[qwen-local, glm-test]` (seeded from
   the operator instance ladder), budget `monthly_cost_usd: 0`.

## P2 results

| # | Question | Result |
|---|---|---|
| **P2-1** | Capture real member + webhook payloads | **Captured.** Member payload = the `POST /members` response. Webhook payloads = GitLab hook event log `request_data`. Issue-hook top-level keys: `changes, event_type, labels, object_attributes, object_kind, project, repository, user`. MR-hook additionally carries `assignees`. (The `X-Gitlab-Token` request header carries the webhook secret and is redacted here — it is the shared secret, never committed.) |
| **P2-2** | Does the member payload carry `created_at`? | **YES.** `created_at: 2026-07-19T01:48:04.858-05:00`. Full member keys: `access_level, avatar_url, created_at, created_by, expires_at, id, locked, name, public_email, state, username, web_url`. |
| **P2-3** | Does intake provision a webhook with the `?gen=` marker? | **YES.** hook id 2, URL ends `/hook/gitlab?gen=1`. |
| **P2-4** | Onboarding MR assigned to a single assignee? | **YES.** exactly one: `steve`. |
| **P2-7** | Onboarding MR created, deterministic / zero-token? | **YES.** `!1`, author `gonk`, `Generated-By: gonk/... (deterministic onboarding; no model)`. No model call in the path. |

(P2-5 / P2-6 were not targeted by this run.)

## Chart / config bugs L3 surfaced

### BUG 1 (deploy-blocking) — `dolt.image.tag: v1.43.0` does not exist on Docker Hub

Every render-golden under `chart/gonk/ci/` (`values-default.yaml`,
`values-all-bundled.yaml`, `values-cnpg.yaml`, `values-full-monitoring.yaml`,
`values-byo-gascity.yaml`, `values-ledger-cnpg.yaml`) pins
`dolt.image.tag: v1.43.0`. On a real install the StatefulSet pod went
`ImagePullBackOff`:

```
Failed to pull image "docker.io/dolthub/dolt-sql-server:v1.43.0":
  ... not found
```

`dolthub/dolt-sql-server` tags carry **no `v` prefix** and there is no `1.43.0`
at all (the line runs `... 1.88.1 → 2.0.0 → 2.2.1`). The repo already knows this:
`chart/gonk/smoke/gc-controller-smoke.md` records "the `dolt-sql-server:v1.43.0`
tag no longer exists on Docker Hub; `2.1.7` is the current tag matching our
`DOLT_VERSION` pin." The **chart values were never updated to match the smoke's
own finding.** Render goldens don't pull images, so this is invisible until a
real install — precisely L3's job. Fixed for this run by
`kubectl set image ... dolt=docker.io/dolthub/dolt-sql-server:2.1.7`, after which
Dolt and then the controller reached Ready. **Recommendation:** bump every
`chart/gonk/ci/*.yaml` `dolt.image.tag` to `2.1.7` (the DOLT_VERSION pin) and add
a note that it must track `deps.env DOLT_VERSION`.

### BUG 2 (minor) — controller crash-loops (rather than waits) until Dolt is reachable

`gonk-controller` refuses to start while the Dolt server is unreachable
(`managed Dolt server unreachable while inspecting existing store 'bd_gonk';
refusing to force-reinitialize (data-safety)`) and exits, so it crash-loops
(5 restarts) until Dolt is up — then it recovers cleanly. The refuse-on-unsafe
behavior is correct; the noise is a startup-ordering gap (no init wait / readiness
gate on Dolt). Not deploy-blocking. Worth a follow-up (an initContainer that
waits for `gonk-dolt:3306`).

### Observation (not a chart bug) — controller exec-order env

The in-pod `gonk-gate` exec order (`gonk-sweep`) logs
`GONK_METER_TOKEN_FILE ... unset`. The controller's `[api]` plane and grant
verification are up regardless; this only affects the in-pod sweep/dispatch exec
orders, which also have no agent runtime (`tmux server unreachable`) in this run.
Out of scope here (no agent image); flagged for whoever wires the exec-order env.

### Good news — the LiteLLM spend-poller adapter works against real v1.92.0

`docs/spikes/litellm-verified.md` recorded the Plan 03 spend poller (Bug A,
gonk-huy) as FATAL against real LiteLLM v1.92.0 — meter would never sync and
`/readyz` would never flip. In this run **`gonk-meter` reached `/readyz` HTTP 200**
against a real v1.92.0 proxy, i.e. the first spend sync completed. The
`gonk-meter:v0.1.0-65d404b58852` image carries the fix; the L2 blocker is closed
on the deployed artifact.

## Step 10 (optional decide→controller) — boundary reached, not crossed

The created issue was `accepted` by intake but not dispatched: the project is not
yet onboarded (the onboarding MR is still open, no `.gonk.yml` on `main`, project
state `absent`), so triage has no resolved policy and correctly does not fire a
`/decide`. Crossing this needs the onboarding MR merged AND an agent session image
(not built for this run) — out of scope, as anticipated.

## Teardown

Idempotent teardown (`teardown.sh`) ran: deleted the test project's webhooks,
deleted the namespace, restored `allow_local_requests_from_web_hooks_and_services`
to its captured original value (`true` — it was already enabled, so a no-op
restore). Project 75 and the gonk bot were left intact for the parent to decide.
Verification checks are recorded in the run report. No stub image was built
(a Python stub via ConfigMap was used instead of building/pushing the Go
`gonk-stubmodel`), so no image tag lingers in any shared registry.
