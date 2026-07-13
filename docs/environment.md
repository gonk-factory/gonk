# Deployment environment (orac cluster)

Facts read off the live cluster and GitLab instance on 2026-07-13. Plans should
cite this file rather than guessing or re-asking. **Re-verify before relying on
any of it** — it is a snapshot, not a contract.

## GitLab

- `https://gitlab.orac.local`, **version 18.10.1, Community Edition** (`enterprise: false`).
- CE matters: multiple MR assignees is an EE feature, so anything assigning an MR
  must pick exactly one assignee deterministically.
- Runs **in-cluster**, namespace `gitlab`.
- Repo: `agentic/gonk-project` (project id 69). Container registry available at
  the GitLab registry — the chosen home for gonk's images.
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

## Dev box

- WSL2. Go 1.26.4, golangci-lint v2.12.2, `glab` (authenticated), `kubectl`.
- Container runtime is **podman**, and its CNI bridge is broken — container runs
  need `--network=host`.
