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
- **Every CI runner is currently OFFLINE** (stale registrations from cycled
  `gitlab-gitlab-runner-*` pods). CI has never executed for this repo. The
  standing gate is local; first images must be built and pushed by hand.
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

## Ledger backend

- Primary: **Dolt** (already used for the beads store). Plan 03 Task 0b is a
  blocking spike that must empirically prove Dolt can serialize the concurrent
  reservation race.
- Fallback (owner-approved): **Postgres on the existing CNPG cluster** —
  `databases-app/postgres`, healthy, 2 instances. Not a new dependency. Take it
  without hesitation if the spike fails *or* is inconclusive.

## Gas City

- **Not deployed yet** (owner, 2026-07-13). Deploying it is a prerequisite for
  Plan 04 (pack & images).
- Consequence: the agent-pod label selector that the "budgets cannot be
  bypassed" NetworkPolicy depends on **is not yet knowable**. The chart takes it
  from values with a render guard against an empty selector, and Plan 06's
  egress-denial test is the gate — a *wrong* selector renders fine and enforces
  nothing, so the test, not the template, is what proves it.

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
- **Secrets:** ClusterSecretStore `vault-backend`, Vault KV path
  `eso/gonk/<concern>` (hierarchical — newer convention; older entries are flat).
  Rotation is opt-in via `homelab.orac.local/rotation: enabled` + annotations,
  as renovate's PAT does. **Gotcha, from a real incident: write the Vault value
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
