# Deployment environment (orac cluster)

Facts read off the live cluster and GitLab instance on 2026-07-13. Plans should
cite this file rather than guessing or re-asking. **Re-verify before relying on
any of it** — it is a snapshot, not a contract.

## GitLab

- `https://gitlab.orac.local`, **version 18.10.1, Community Edition** (`enterprise: false`).
- CE matters: multiple MR assignees is an EE feature, so anything assigning an MR
  must pick exactly one assignee deterministically.
- Runs **in-cluster**, namespace `gitlab`.
- Repo: `agentic/gonk-project` (project id 69). Images go to the in-cluster
  GitLab container registry at **`registry.orac.local`** (owner decision
  2026-07-13), path `registry.orac.local/agentic/gonk-project/<image>:<tag>`.
- **CI works (verified 2026-07-13, pipeline 1437 green).** A live Kubernetes
  executor runner picks up jobs (`kubernetes` executor, namespace `gitlab`).
  Caveat: the project-scoped `/runners` API still lists 20 stale registrations
  as "0 online" — ignore it; the proof is that pipelines actually run and
  complete. Don't re-conclude "no runner" from that endpoint.
- **CI is vendored/hermetic.** golangci-lint was timing out (`run.timeout: 5m`)
  because the runner pod stalls downloading modules (can't reach
  `proxy.golang.org`, or is heavily throttled). Fix: **dependencies are vendored
  (`vendor/` committed)**, so `go build`, `go test -race`, and `golangci-lint`
  all build offline. **Any plan that adds a Go dependency MUST run
  `go mod vendor` and commit `vendor/`, or CI goes red again.** Verified both CI
  images pass with `--network=none`.
- First-party images still must be built and pushed by hand for now (image build
  is not yet a CI stage).
- Serves a **private CA** cert. Clients must trust the CA — mount the bundle.
  Do not disable TLS verification in any service. (The repo's git remote uses
  `http.sslVerify=false` as a local convenience; services must not copy that.)

## Cluster

- Context `admin@orac`, Kubernetes **v1.34**, 3 nodes (`orac01` control-plane,
  `johnny`, `bailey`). `bailey-gpu` namespace exists (DGX-class inference).
- **Ingress: Traefik** (`traefik.io/ingress-controller`), the only IngressClass.
- **cert-manager** with ClusterIssuers: `orac-cluster-ca-issuer`,
  `orac-services-ca-issuer`, `selfsigned-cluster-issuer` (private CA).
- **external-secrets** operator installed (`external-secrets-system`) — this is
  how Vault/1Password material reaches a k8s Secret. Never commit a secret;
  never pass one via env. ExternalSecret -> Secret -> **file mount**.
- **Flux** installed (`flux-system`) — GitOps is the deployment path; the chart
  is consumed as a HelmRelease, not `helm install` by hand.
- **`kind` is NOT installed.** A real cluster is reachable, so the e2e harness
  should target it in a throwaway namespace rather than standing up kind. This
  also sidesteps the broken CNI bridge networking on the WSL dev box.

## LiteLLM (bring-your-own — owner decision 2026-07-13)

- **Already deployed**: namespace `litellm`, Service `litellm:4000`
  (`http://litellm.litellm.svc.cluster.local:4000`), with its own
  `litellm-postgresql:5432` and a `valkey:6379`.
- The chart does NOT bundle LiteLLM (resolves the spec's §4.1-vs-§7.4
  contradiction in favour of §7.4). It takes an `externalURL` + admin credential.
- Local rungs must be given a **synthetic** per-token price here so the USD
  virtual-key ceiling is a hard door for local models too. Synthetic dollars are
  firewalled from real spend — see plans 03/05.
- **SITE-LOCAL, NOT DEFAULTS.** This instance's models are `qwen3-14b`
  (`ollama_chat/qwen3:14b` on bailey) and `qwen3-8b-bailey`. There is no model
  called `qwen-local`. **These names must never appear as defaults in gonk's
  chart, pack, or onboarding template** — another operator's LiteLLM has
  entirely different models, and hardcoding ours would be wrong for them and
  silently wrong for us the day bailey's model list changes.
  Gonk ships the *mechanism*: a rung name (operator-chosen, matching
  `^[a-z0-9][a-z0-9-]*$`) mapped by the **rung catalog** (`pkg/opercfg`, Plan 03)
  to a real LiteLLM model plus its synthetic price. The catalog and the instance
  ladder are **site-local operator config**, supplied via HelmRelease values in
  the gitops repo. **The chart must FAIL to render without them, never guess.**
  On this cluster the mapping happens to be `qwen-local` -> `qwen3-14b`; that is
  an example, not a default.

## VERIFIED: the attribution chain works (smoke-tested 2026-07-13)

Tested against the live LiteLLM (`ghcr.io/berriai/litellm-database:v1.92.0`) with
one real completion to `qwen3-14b`. **Do not re-litigate this; it is measured,
not assumed.**

- **Mechanism (confirmed):** set the request header
  `x-litellm-spend-logs-metadata: <JSON>`. LiteLLM parses it into
  `data["metadata"]["spend_logs_metadata"]` and persists it to
  `LiteLLM_SpendLogs.metadata.spend_logs_metadata`. **All 7 `pkg/atags` keys
  round-tripped intact** (`gonk_project`, `gonk_rig`, `gonk_bead_id`,
  `gonk_session_key`, `gonk_rung`, `gonk_attempt`, `gonk_trigger`).
- **opencode delivers it:** opencode supports static custom headers per provider
  (`provider.<id>.options.headers`, spread into the AI-SDK factory). Attribution
  is per-session and **one session == one pod**, so the session's tags are baked
  into that pod's opencode config at spawn. No per-request plumbing needed, and
  no per-session virtual keys.
- **Per-key metadata merges with per-request** (request wins, key fills gaps), so
  one virtual key per project composes with per-session headers.
- **LiteLLM's docs claim per-k/v `spend_logs_metadata` is an "enterprise
  feature". It is NOT enforced in v1.92.0** — the smoke test wrote and read it
  back on this instance. Fallback if a future upgrade starts enforcing it:
  `x-litellm-tags` -> the `request_tags` column (also verified populated).
- **`spend: 0.0` for the local model, on a real 25-token call.** This is the
  synthetic-pricing problem demonstrated rather than argued: real GPU work that
  LiteLLM records as costing nothing, so a USD ceiling never closes on it.

### OPERATIONAL HAZARD: `/spend/logs` must always be bounded

**An unbounded `GET /spend/logs` (no `start_date`) OOM-killed the LiteLLM pod**
(2Gi limit, exit 137, 2 restarts, ~90s outage) during this smoke test. It tries
to load the whole spend table into memory.

**gonk-meter polls this endpoint.** It MUST always pass a bounded window, must
paginate, and must never issue a naked query — a poll bug here takes down the
inference gateway for the entire cluster, not just gonk. Treat a missing date
bound as a code-review blocker in Plan 03. Prefer `request_id=` lookups when
resolving a single row.

## Deployment topology: ONE uber-chart (owner decision 2026-07-13)

**Correction of an earlier framing error.** An earlier pass treated Gas City and
Dolt as "bring-your-own / already exists." They are NOT. Verified against the
live cluster: **no Dolt SQL server is deployed, no Gas City is deployed, and no
`gonk-city` repo exists.** Only **GitLab, LiteLLM, and the CNPG Postgres
operator** pre-exist and stay BYO.

Gonk ships **one umbrella Helm chart** (Plan 05) that deploys the whole factory:

- the **Gas City controller** (reconcile loop + supervisor REST/SSE on :8372 +
  hosts pack services + spawns agent pods via its k8s session provider),
- a **Dolt SQL server** (backs the Gas City beads store AND gonk-meter's ledger),
- **gonk-intake** and **gonk-meter**,
- the **gonk pack** (installed into the controller),

wiring to BYO GitLab / LiteLLM / CNPG. `helm install` (in practice a Flux
HelmRelease) yields a working factory.

**Every bundled component is individually toggleable** (`<component>.enabled`),
so a component can be disabled when it is managed or replaced elsewhere — e.g.
migrate to a self-hosted DoltHub/DoltLab one day, set `dolt.enabled: false`, and
point meter + Gas City at the external server. Standard umbrella-chart /
conditional-subchart pattern. The chart must be honest about a disabled
component's external replacement being required (a render `fail` if, say, Dolt
is disabled but no external DSN is supplied).

**Instance wiring lives in the gitops repo** — the Flux HelmRelease, the values
(operator rung catalog + instance ladder + synthetic prices), and the
ExternalSecrets. A private `gonk-city` app-level repo is kept as a *future
option* if app-level instance config outgrows the gitops repo; not needed now.

## Ledger backend — SETTLED: CNPG Postgres (Task 0b spike ran, Dolt failed)

- **gonk-meter's ledger / reservation store is Postgres on the existing CNPG
  cluster** (`databases-app/postgres`, healthy, 2 instances) — owner decision
  2026-07-14, after the Task 0b spike. **Dolt was tested and FAILED the
  reservation-race isolation test decisively:** 32 concurrent writers all won a
  ceiling-of-2 race, persisting 12.8× the ceiling, under default isolation,
  `SERIALIZABLE`, AND `SELECT ... FOR UPDATE` — the last confirmed a no-op
  (Dolt's concurrency is optimistic/commit-time keyed on write-set overlap;
  distinct reservation rows never conflict, so the locking keywords are accepted
  but not enforced). See `docs/spikes/dolt-reservation-isolation.md`. Postgres
  enforces the race in the DB (`SERIALIZABLE`/`FOR UPDATE`), so meter can run
  >1 replica safely — the money guarantee does NOT depend on single-replica.
- **Dolt is NOT dropped** — it remains **Gas City's beads store** (versioned
  audit history, no reservation-race requirement; `[beads] backend` is Dolt-only,
  no Postgres path). The chart deploys a Dolt server for beads; meter's ledger
  is CNPG Postgres. Two stores, two engines, by design.
- Do NOT re-frame this as "Dolt primary, Postgres fallback" — that was the
  pre-spike plan. The spike settled it: Postgres for the ledger, full stop.
- **The Gas City beads store is Dolt-ONLY — verified, no Postgres path exists**
  (`[beads] backend` enum is `dolt`/`doltlite`; sqlite/coordstore were removed
  and hard-error). So the `dolt.enabled: false` toggle means "point Gas City AND
  the ledger at an **external** Dolt (e.g. self-hosted DoltLab) via host/port",
  NOT "use Postgres for beads". CNPG can only ever replace the *ledger*, never
  the beads store. External Dolt is a first-class prod mode
  (`GC_DOLT_HOST`/`GC_DOLT_PORT`, or `[dolt] host`/`port`), but the shipped Dolt
  auths as `root` / empty password / `--no-tls` — there is no user/password/DSN
  key, so the chart must not pretend to configure credentials it can't.

## Deploying the Gas City controller (chart templates it — no upstream chart)

Verified from `gastownhall/gascity`:

- **No Helm chart, no controller manifest upstream.** `contrib/k8s/` ships raw
  manifests for the namespace, RBAC, a Dolt StatefulSet+Service, and a mail
  sidecar — but the controller itself is deployed by an imperative bash script
  (`contrib/session-scripts/gc-controller-k8s`) that creates a bare
  `kind: Pod` (`restartPolicy: Never`) and `kubectl cp`s the city dir in. **Our
  chart templates the controller as a proper workload.**
- **Config:** the controller runs `gc start --foreground /city` and reads
  `/city/city.toml` (`[workspace]`, `[session] provider="k8s"`, `[session.k8s]`,
  `[beads]`, `[dolt]`, `[daemon]`), overridable by `GC_*` env. The **pack + city
  config** are delivered at `/city` — upstream via `kubectl cp` into an emptyDir;
  a declarative chart must instead bake them into the `gonk-controller` image
  (`prebaked=true`) or mount a ConfigMap, and confirm the `gc init` → `.gc-start`
  sentinel handshake still fires.
- **RBAC (namespace-scoped Role, in-cluster ServiceAccount `gc-controller`):**
  `pods` [get,list,watch,create,update,patch,delete], `pods/exec` [create],
  `pods/log` [get], `configmaps` [get,list,watch,create,update,patch,delete].
  Agent SA `gc-agent`: `pods` [get].
- **Single replica, no leader election** — it is a reconcile loop; upstream runs
  exactly one `restartPolicy: Never` Pod. Whether it tolerates a Deployment
  (`restartPolicy: Always`) without duplicate-reconcile / Dolt-lock issues is
  **unverified** (smoke-test below).
- **Supervisor listener:** must set `bind=0.0.0.0` + `allow_mutations=true` for
  in-container use (defaults to `127.0.0.1`). **Port is uncertain — config docs
  say `9443`, earlier research assumed `8372`. Settle by smoke deploy before
  wiring the Service/probes/NetworkPolicy.** No per-route auth by default
  (network-position trust); optional ed25519 grant gates exist
  (`GC_CITY_WRITE_PUBKEY`/`GC_CITY_READ_PUBKEY`) for an edge to mint grants.
- **Health probes:** `/health`, `/v0/readiness`, `/v0/provider-readiness`
  (unauthenticated). Upstream sets no k8s probes (greps logs); we wire these.
- **THREE unknowns require a smoke deploy before the chart is trustworthy**, and
  a smoke deploy needs the `gonk-controller` image (Plan 04): (a) the real
  supervisor port; (b) the declarative pack/city delivery + sentinel handshake;
  (c) Deployment vs bare-Pod tolerance. Plan 05 must gate on settling these, not
  assume them.

## Gas City (deployed by the chart)

- **Not deployed yet, and the uber-chart is what deploys it.** Its container
  image (`gonk-controller`) is built in Plan 04; the chart runs it as a
  Deployment + Service (:8372) with the RBAC its k8s session provider needs to
  create/manage agent pods, plus state storage.
- Agent-pod label selector for the "budgets cannot be bypassed" NetworkPolicy is
  now KNOWN, not guessed: Gas City hardcodes **`app: gc-agent`** on every agent
  pod (`internal/runtime/k8s/pod.go`). Plan 06's egress-denial test is still the
  gate that proves the policy actually binds — but see the network-layer
  limitation below: it is unenforced until Cilium lands regardless.

## KNOWN LIMITATION: the network-layer budget bypass is real and NOT closed

**Spec §9's claim that "budgets cannot be bypassed" is NOT MET at the network
layer today.** Owner decision (2026-07-13): ship anyway, document the gap, do
not gate on it. So it is documented here, loudly, and it must not be quietly
downgraded to a footnote.

- NetworkPolicies **are not enforced on this cluster**. Flannel does not
  implement them, and the Cilium HelmRelease that would is **suspended**
  (`gitops:clusters/orac/foundation/kustomization.yaml`). Every existing policy
  in the gitops repo carries a comment saying exactly this. There is no
  cluster-wide default-deny and no enforced egress anywhere.
- The concrete bypass: LiteLLM routes local models to **Ollama at
  `http://192.168.1.142:11434`, which requires no credential.** An agent pod
  that can reach the network can call Ollama directly, skip LiteLLM entirely,
  and burn local GPU with **zero metering and no ceiling**. The LiteLLM
  virtual-key hard door only binds traffic that actually goes through LiteLLM;
  the NetworkPolicy is the only thing that forces it to.
- What gonk does about it: the chart **ships the correct egress NetworkPolicy
  anyway** (documentation-as-code; it becomes real the day Cilium is
  unsuspended), and Plan 06 **writes the egress-denial test but skips it**, with
  a skip message naming Cilium. When Cilium lands, un-skip the test — if it
  fails, the selector was wrong, which is the failure mode that renders fine and
  enforces nothing.
- Until then: **local-model budgets are advisory, not hard.** Cloud-rung budgets
  ARE hard (a cloud call needs a key the pod only has via LiteLLM). Do not claim
  otherwise in a README, a dashboard, or a demo.
- Closing this is infra work outside gonk's repo: unsuspend Cilium, or put a
  credential in front of Ollama so a direct call fails on auth.

## gitops conventions gonk must follow

The cluster is deployed from `/mnt/c/Users/steve/Code/gitops` (Flux). Its
`CLAUDE.md` + `docs/superpowers/specs/2026-04-28-gitops-conventions-design.md`
are authoritative, and `scripts/verify-conventions.sh` enforces them in a
pre-commit hook. Conform; do not invent.

- **Layout:** `clusters/orac/apps/gonk/`, one K8s object per file, named
  `<kind>-<name>.yaml`, `metadata.name` matching. The dir's `kustomization.yaml`
  must list every tracked yaml (a check fails the commit otherwise). Register in
  `clusters/orac/apps/kustomization.yaml`. Needs `namespace-gonk.yaml`, pod
  label `app: <name>`, and the `homelab.orac.local/service-tier` +
  `data-tier` labels.
- **Charts are never vendored.** `charts/` at the repo root is gitignored.
  Charts are referenced remotely via a `HelmRepository`/`OCIRepository` in
  `clusters/orac/sources/`. Gonk's chart is pushed as OCI to
  `registry.orac.local/agentic/gonk-project/charts`.
- **Copy `clusters/orac/apps/renovate/`** — it is nearly gonk's twin (GitLab bot
  PAT + webhook secret + CA trust). `apps/nagus/` is the model for a
  first-party image + shared CNPG.
- **Secrets:** ClusterSecretStore `vault-backend`. **gonk is one of the flat
  entries, not the hierarchical convention:** every gonk credential but one lives
  in a single Vault secret, `eso/gonk/broker`, as properties (`gitlab_token`,
  `webhook_token`, `meter_api_token`, `litellm_admin_key`, `postgres_password`,
  `gc_write_key`). The exception is `eso/gonk/dolt` (`root-password`,
  `gc-password`), added 2026-09-09 for the chart's Dolt credentials. Verified
  against `clusters/orac/apps/gonk/` — do not infer paths from `eso/gonk/<concern>`.
  Rotation is opt-in via `homelab.orac.local/rotation: enabled` + annotations,
  as renovate's PAT does — **but no gonk ExternalSecret maps a `*-previous` key,
  so gonk rotations are hard cutovers with up to a 1h resync** (`gonk-9snw`). **Gotcha, from a real incident: write the Vault value
  BEFORE merging the ExternalSecret** — a missing key leaves the ExternalSecret
  NotReady and can wedge the whole reconcile.
  *Deviation:* house style consumes secrets as **env** (`secretKeyRef`); gonk
  uses **file mounts** (owner decision 2026-07-13) because env leaks into
  process listings, crash dumps, and child processes. ESO delivery is identical;
  only consumption differs. Note the deviation in the chart.
- **Images:** `registry.orac.local/agentic/gonk-project/<image>:<tag>` (the
  in-cluster GitLab registry — Harbor is NOT deployed; zot is a pull-through
  cache only). Per-namespace `externalsecret-gitlab-registry-pull-creds.yaml`,
  then `imagePullSecrets: [{name: gitlab-registry-pull-creds}]`. **Pin exact
  tags — never `latest`;** Renovate autodiscovers and bumps them.
- **TLS / private CA:** trust-manager publishes ConfigMap **`trust-bundle`** (key
  `tls-ca-bundle.pem`) into every namespace. Go services mount it and set
  `SSL_CERT_FILE=/etc/ssl/orac/ca.crt`. **Never disable TLS verification.** This
  is the correct fix for reaching `https://gitlab.orac.local`.
- **Ingress:** Traefik; annotate `external-dns.alpha.kubernetes.io/hostname` and
  `cert-manager.io/cluster-issuer: orac-services-ca-issuer`; host
  `gonk.orac.local`. **Also add the hostname to the CoreDNS hosts block**
  (`clusters/orac/foundation/coredns/configmap-coredns.yaml`) or in-cluster
  resolution fails silently. Do NOT combine an issuer annotation with an
  explicit `Certificate` — that flip-flop once reissued 863 certs.
- **CNPG tenancy** needs four objects (managed role on the shared `postgres`
  cluster, an ExternalSecret for the role, a `Database` CR, and a second
  ExternalSecret in gonk's namespace — ESO cannot replicate cross-namespace).
- **Monitoring:** `servicemonitor-<name>.yaml` in the workload namespace with
  `labels: {release: kube-prometheus-stack}`; Grafana dashboards as a ConfigMap
  in ns `monitoring` labelled `grafana_dashboard: "1"`.
- **LiteLLM's model list and pricing live in one file:**
  `clusters/orac/apps/litellm/litellm.yaml`, under
  `spec.values.proxy_config.model_list` and
  `litellm_settings.model_cost_map`. **Local models are currently priced at
  zero there** — that map is exactly where synthetic pricing goes.
- **Issue tracking is `bd` (beads), not markdown TODOs**, in that repo.

## Dev box

- WSL2. Go 1.26.4, golangci-lint v2.12.2, `glab` (authenticated), `kubectl`.
- Container runtime is **podman**, and its CNI bridge is broken — container runs
  need `--network=host`.
