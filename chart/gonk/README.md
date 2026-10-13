# gonk -- the chart

A local software factory for a self-hosted GitLab: a Gas City pack plus the glue
that runs it. This chart is an **umbrella**. It deploys the Gas City controller
(which also runs `gonk-gate`), Dolt (the controller's bead store), `gonk-intake`,
`gonk-meter` and the operator config together, each component individually
toggleable, and wires them to a GitLab, a LiteLLM proxy and a Postgres that
**you** bring. `values.schema.json` is the first gate; a set of `fail`-closed
guards in `templates/_guards.tpl` is the second.

> The defaults are **safe, not useful**. Out of the box this chart cannot spend a
> dollar (`operatorConfig.instance.budget.monthly_cost_usd: 0`), exposes nothing
> (`ingress.enabled: false`), and refuses to render without the image tag, the
> URLs and the operator ladder and catalog it cannot guess. You make it useful
> on purpose, in the open.

What a deployment has to provide before any of this, in prose:
`docs/environment.md`.

---

## 0. What is and is not enforced -- read this first

The chart renders a default-deny `NetworkPolicy` and one policy per component.
The one for agent session pods would confine them to DNS, LiteLLM,
gonk-intake's private listener and gonk-meter. **Whether any of that is enforced is a property of your
cluster, not of this chart**: the API server accepts a NetworkPolicy whether or
not the network plugin implements it, and some do not.

Find out, after installing:

```bash
helm test <release> -n <namespace>
```

runs a probe from a pod carrying the session pods' labels
(`networkPolicy.agentPodSelector`) and reports ENFORCED or NOT ENFORCED. Leave
`networkPolicy.enforcementProbe.enabled` on even where the answer is NOT
ENFORCED: turning the probe off does not change the answer.

**What this means for budgets, precisely:**

- **Cloud-rung budgets are hard.** A cloud call needs an API key. The only key a
  session pod ever holds is its project's LiteLLM virtual key, and LiteLLM
  refuses at the ceiling. This does not depend on NetworkPolicy.
- **Local-model budgets are hard only where the egress policy is enforced.**
  If it is not, and a model server on your network answers without a
  credential, a session pod can call it directly, bypass LiteLLM, and use it
  with no metering and no ceiling. To close that, use a network plugin that
  enforces NetworkPolicy or put a credential in front of the model server.

Describe budgets that way everywhere. Every rendered `NetworkPolicy` carries
an annotation to the same effect
(`gonk-factory.github.io/network-policy-enforcement`), and a chart test
(`TestEveryNetworkPolicyDeclaresEnforcementDependsOnTheCNI`) fails if one
loses it.

### Where NetworkPolicy IS enforced: one value you must set

The policies deny everything by default, in both directions, and add back what
each pod needs. One thing they cannot add back on their own is the **Kubernetes
API server**. The controller creates every session as a Job through it, and the
meter writes every project's key Secret through it; it is not a pod, so a policy
can name it only by address, and the chart does not know yours.

`networkPolicy.kubeAPI.cidrs` is empty by default, and empty renders no rule. On
a cluster that enforces NetworkPolicy that means **no session Job is created and
no key is delivered** until you set it. On a cluster that does not enforce,
it changes nothing.

Set it to the addresses behind the `kubernetes` Service, not the Service's own
cluster IP (policy is evaluated after the Service address is translated, so the
cluster IP on port 443 matches nothing):

```bash
kubectl --context <context> -n default get endpointslices \
  -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{range .items[*]}{.endpoints[*].addresses[*]}{" port "}{.ports[*].port}{"\n"}{end}'
```

```yaml
networkPolicy:
  kubeAPI:
    cidrs: ["192.0.2.10/32"]   # each address from the output, or the control-plane subnet
    ports: [6443]              # the port from the output
```

What to know about it:

- The addresses must be stable. If a control-plane node comes back on another
  address, the rule no longer matches and sessions stop until the value
  follows it. A subnet is accepted; a prefix shorter than `/8` (IPv4) or
  `/16` (IPv6) is refused, so neither a `/0` nor the two halves of the
  address space can be written here.
- Some network plugins do not match an `ipBlock` against a node's address
  (Cilium does not unless it is configured to). There the API server has to be
  allowed by a policy of that plugin's own kind, beside this chart.
- Only the controller and the meter get the rule. Session pods do not call the
  API and do not mount a ServiceAccount token.
- It has been observed working on k3s (kube-router), which is what the
  repository's `hack/netpol_enforcement_e2e.sh` runs against. No other
  network plugin has been tried with this rule.

Two more things are true where the policies are enforced, and neither is
closed by this chart:

- **A session pod can reach everything on gonk-intake's private listener**, not
  only its own checkout. The policy admits it to the port; it cannot tell one
  route from another. That includes the unauthenticated `POST /admin/reconcile`
  and the unauthenticated route that registers a checkout, so a session can
  register and fetch any project the forge token can read. (Where the policies
  are not enforced, this is equally true.)
- **`ledger.postgres.mode: cnpg` has no policy for its Postgres pods.** They
  are in the release namespace, so the default-deny policy selects them, and
  the chart renders nothing that admits a connection to them: not the meter's,
  not the operator's. This is read from the rendered policies and has not been
  tried on a cluster. Use `shared` or `external`, or add a policy for those
  pods yourself.

---

## 1. Prerequisites

| You bring | The chart needs | Value |
|---|---|---|
| Kubernetes 1.28 or later, one namespace | the right to create Roles and RoleBindings in it; a default StorageClass for the bundled Dolt (or `dolt.persistence.storageClass`) | -- |
| helm 3, version 3.14 or later (not helm 4) | The chart is developed and tested with helm 3 only. Its tests are pinned to helm 3 (the goldens were made with 3.19) and refuse helm 4, which renders the same objects with different whitespace; `make gate HELM=/path/to/helm3` chooses the binary | -- |
| A self-hosted GitLab | its URL, a bot user and an access token for it; GitLab must be able to POST to the webhook URL | `gitlab.url`, `gitlab.botUsername`, `intake.webhookPublicURL` |
| A LiteLLM proxy | its URL and an admin key; at least one model configured behind it | `litellm.externalURL` |
| Postgres | a database and a role that owns it, for the meter's ledger | `ledger.postgres.*`, `secrets.ledger` |
| The Secrets in section 3 | they exist in the release namespace before the install | `secrets.*` |

Optional: an ingress controller (`ingress.enabled`), the Prometheus Operator
(`monitoring.serviceMonitor`, `monitoring.prometheusRule`), a Grafana dashboard
sidecar (`monitoring.dashboards`), the CloudNativePG operator
(`ledger.postgres.mode: cnpg`), and a private CA (section 5). The chart
installs none of them and creates no Secret.

---

## 2. Installing

### The images and the chart

The release workflow (`.github/workflows/release.yml`) publishes four images
for a release tag `vX.Y.Z`:

```
ghcr.io/gonk-factory/gonk/gonk-agent:vX.Y.Z
ghcr.io/gonk-factory/gonk/gonk-controller:vX.Y.Z
ghcr.io/gonk-factory/gonk/gonk-intake:vX.Y.Z
ghcr.io/gonk-factory/gonk/gonk-meter:vX.Y.Z
```

`image.registry` defaults to `ghcr.io/gonk-factory/gonk` and no pull secret is
needed. The same workflow packages this chart and attaches it to the GitHub
Release as `gonk-<chart version>.tgz`. The chart version (`version` in
`Chart.yaml`) is not the release version: read it off the asset's name. The
chart is not published to a Helm or OCI repository. Install the asset, or the
directory `chart/gonk` of a checkout of the release tag.

**A plain release tag needs `imageFloor.acceptUnrankedTags: true`.** The chart
records the oldest build of each image it may run against and refuses anything
older (section 6). That comparison needs a build number, and a tag such as
`v0.2.1` has none, so the render refuses it unless you say the images and the
chart come from the same release. Without the setting, `helm install` fails
with a message that says `IMAGE FLOOR`.

### The minimal values

The smallest values file that renders and is right for a site other than the
maintainers'. Leave out `gitlab.url`, `intake.webhookPublicURL`,
`litellm.externalURL`, `gascity.writeAuth.verifyKey`, `dolt.image.tag`, the
ladder, the catalog, `onboarding.defaultRung` or a tag, and the render fails
with a message naming the value. The names are examples.

```yaml
# my-values.yaml
releaseTag: v0.2.1                    # the one tag all four gonk images deploy at
imageFloor:
  acceptUnrankedTags: true            # required for a plain vX.Y.Z tag (above)

gitlab:
  url: https://gitlab.example.internal

intake:
  # The URL GitLab will POST to. Required even with ingress.enabled: false.
  webhookPublicURL: https://gonk.example.internal/hook/gitlab
  # Projects gonk must never work on. The default, [gonk-factory/gonk], is
  # the upstream project's path. If a copy of gonk's own repository is on this
  # GitLab under another path, name it here (path with namespace, or numeric
  # id); leave the value out to keep the default.
  blockedProjects: [my-group/gonk]

litellm:
  externalURL: http://litellm.litellm.svc.cluster.local:4000

gascity:
  writeAuth:
    # "<kid>:<base64 of the raw 32-byte ed25519 PUBLIC key>". Not a secret.
    # The private half is the Secret gonk-gc-write-key (section 3).
    verifyKey: "k1:<base64>"

dolt:
  image:
    tag: 2.1.7                        # docker.io/dolthub/dolt-sql-server; no default

ledger:
  postgres:
    mode: external                    # a choice, not a requirement: section 7

operatorConfig:
  instance:
    ladder: [local-small]             # the rungs a project may use, in order
  rungs:
    - name: local-small               # a name you choose
      kind: local
      model: my-local-model           # the model's name in YOUR LiteLLM
      est_cost_usd: 0
      est_tokens: 200K
      synthetic_usd_per_1m_tokens: 0.20   # configure the same price in LiteLLM

onboarding:
  defaultRung: local-small            # must be in the ladder
```

With those values the chart deploys gonk-intake (2 replicas), gonk-meter, the
controller and Dolt, exposes nothing, and no project can spend real money.

**Defaults that describe the maintainers' environment, not yours.** They are
still in `values.yaml` in this release. Check each and override what differs:

| Value | Default | What it does |
|---|---|---|
| `ledger.postgres.mode` | `shared` | Means "a database on a CloudNativePG cluster that already exists". Like `external`, it renders no database. `ledger.postgres.shared.clusterName` and `.clusterNamespace` (`postgres`, `databases-app`) are not read by any template. |
| `networkPolicy.ledgerPostgres.namespaceSelector` | namespace `databases-app` | gonk-meter and the controller may reach Postgres (port 5432) only in namespaces this matches. Set it to your Postgres namespace. Not used with `mode: cnpg`. |
| `ingress.className` | `traefik` | Only read when `ingress.enabled`. |
| `networkPolicy.ingressController.namespaceSelector` | namespace `traefik` | Who may reach the webhook listener. |
| `networkPolicy.gitlabInCluster` | enabled, namespace `gitlab` | For a GitLab outside the cluster set `enabled: false` and list `networkPolicy.gitlab.cidrs`. |
| `networkPolicy.litellm`, `networkPolicy.litellmProxy` | namespace `litellm`, port 4000, pod label `app.kubernetes.io/name: litellm` | Where the model gateway runs. |
| `networkPolicy.prometheus.namespaceSelector` | namespace `monitoring` | Who may scrape. |

A wrong selector is only visible on a cluster that enforces NetworkPolicy, as a
connection that times out.

### Install

Create the Secrets of section 3 first. Then:

```bash
# see exactly what would be applied, and every refusal, before touching a cluster
helm template gonk ./chart/gonk --namespace gonk --values my-values.yaml

helm install gonk ./chart/gonk --namespace gonk --create-namespace --values my-values.yaml

# does this cluster enforce the egress policy? (section 0)
helm test gonk --namespace gonk
```

(`./chart/gonk` may equally be the downloaded `gonk-<chart version>.tgz`.)
The pods do not become ready until the Secrets exist, the bot token works,
LiteLLM answers and the ledger is reachable. `docs/observability.md` says how
to read what each service logs.

To receive webhooks, either enable the chart's Ingress (`ingress.enabled`,
`ingress.className`, `ingress.host`; the path is `/hook/gitlab` and must not be
widened) or route `intake.webhookPublicURL` to Service `gonk-intake` (port 8080)
yourself. The host must resolve from wherever GitLab runs; if GitLab is in the
same cluster and the cluster's DNS does not know the host, the symptom is a
webhook that never arrives.

---

## 3. The Secrets the chart needs

**No secret value appears in `values.yaml`, and none is an environment variable
in a rendered manifest.** The chart creates no Secret. It mounts Secrets you
created, as files (mode 0400), and each binary reads the file. Projected volumes
are not optional, so a missing Secret or key is a pod that does not start
(`CreateContainerConfigError`), which is the loud failure intended.

How the Secrets get there is yours to choose: an operator that syncs them from
a secret store, or `kubectl create secret generic`. If you use a syncing
operator, write the value to the store before you apply the object that reads
it.

Names and keys below are the defaults; each is a value under `secrets`. Files
are mounted under `secrets.mountRoot` (`/etc/gonk/secrets`).

| Secret | Key | What it holds | Read by |
|---|---|---|---|
| `gonk-gitlab` | `token` | the bot user's GitLab access token | intake, controller |
| `gonk-gitlab` | `admin-token` | only with `gitlab.mode: split-credential`: the token used for webhook management | intake |
| `gonk-webhook` | `token` | the shared secret GitLab sends with each webhook; any long random string | intake |
| `gonk-meter-api` | `token` | the bearer token for gonk-meter's API; any long random string | intake and controller present it, meter verifies it |
| `gonk-litellm` | `admin-key` | the LiteLLM admin key | meter, controller |
| `gonk-ledger` | `dsn` | the ledger's Postgres connection string (section 7) | meter |
| `gonk-gc-write-key` | `key` | the ed25519 private key that signs orders to the controller; the public half is `gascity.writeAuth.verifyKey` | intake, controller |
| `gonk-dolt` | `root-password` | the bundled Dolt's root password (bundled Dolt only) | dolt |
| `gonk-dolt` | `gc-password` | the password of the `gc` user the controller connects to Dolt as | dolt, controller |
| `gonk-workstore` | `dsn` | only with `workstore.enabled: true`: the work store's Postgres connection string, role and schema `gonk_workstore` | controller |

A second, optional key per credential (`token-previous`,
`admin-key-previous`) is the rotation slot described below; it is mounted only
when you switch it on.

For example, with `kubectl`:

```bash
kubectl -n gonk create secret generic gonk-gitlab    --from-file=token=./gitlab-token
kubectl -n gonk create secret generic gonk-webhook   --from-file=token=./webhook-token
kubectl -n gonk create secret generic gonk-meter-api --from-file=token=./meter-token
kubectl -n gonk create secret generic gonk-litellm   --from-file=admin-key=./litellm-admin-key
kubectl -n gonk create secret generic gonk-ledger    --from-file=dsn=./ledger-dsn
kubectl -n gonk create secret generic gonk-dolt \
  --from-file=root-password=./dolt-root-password --from-file=gc-password=./dolt-gc-password

# the order-signing key pair
openssl genpkey -algorithm ed25519 -out gc-write.pem
kubectl -n gonk create secret generic gonk-gc-write-key --from-file=key=./gc-write.pem
# the value for gascity.writeAuth.verifyKey is "k1:" followed by:
openssl pkey -in gc-write.pem -pubout -outform DER | tail -c 32 | base64
```

Keep those files out of version control.

**`gonk-dolt` is consumed differently, and still not as a rendered env value.**
The Dolt image's entrypoint and Gas City's `gc` binary read
`DOLT_ROOT_PASSWORD` / `DOLT_PASSWORD` / `GC_DOLT_PASSWORD` from the
environment and have no file-based alternative. The Secret is still mounted as
a file; the Dolt StatefulSet's command and the controller's commands each `cat`
their file into an `export` for their own process only, immediately before
exec-ing the real entrypoint. The value never appears in `helm template` output
or in a Pod object. `TestDoltIsNotRootOpen` (`internal/charttest`) asserts the
rendered wiring. Root is bound to `localhost` inside the Dolt pod; the
controller authenticates only as the `gc` user, which is granted the bead
database alone.

Besides the Secrets, gonk-meter **writes** one Secret per onboarded project,
named `<secrets.keySink.namePrefix><project slug>` (default prefix
`gonk-key-`), holding that project's LiteLLM virtual key. The namespace should
hold nothing else whose name starts with that prefix.

### Rotation

Each credential has an optional second slot, so that a rotation need not be an
outage. The chart mounts slot 2 only when asked, because a projected volume
that names a key the Secret does not have stops the pod from starting.

1. Write **new -> slot 1** (`token`), **old -> slot 2** (`token-previous`) in the
   Secret.
2. Set `secrets.rotation.<x>Previous: true` (`gitlabPrevious`,
   `webhookPrevious`, `meterAPIPrevious`, `litellmAdminPrevious`).
3. For the **webhook token only**, also bump `intake.webhookTokenGen` so that
   reconcile repairs every GitLab hook to carry the new generation.
4. `helm upgrade`.
5. Wait one `intake.reconcileInterval`.
6. Remove slot 2 from the Secret; set `secrets.rotation.<x>Previous: false`;
   upgrade.

If you do not populate slot 2, a rotation is a hard cutover: there is no
overlap in which both the old and the new value are accepted.

**The two meanings of "slot 2" are not the same mechanism:**

- A credential gonk **verifies** (the webhook token, the meter bearer): **both
  slots are accepted** during the overlap. No request fails mid-rotation.
- A credential gonk **presents** (the GitLab token): **slot 1 is presented, and a
  `401` falls back to slot 2 once**, incrementing
  `gonk_intake_gitlab_auth_fallback_total`. **Alert on that counter being > 0: it
  means a rotation is half-finished** (slot 1 is wrong and slot 2 is carrying you).

### The image pull secret, for a mirror

The default `image.registry` is public and `image.pullSecrets` is empty. If you
mirror the images into a private registry, set `image.registry` to it and name
a pull Secret in `image.pullSecrets`. **The chart does not create that
Secret.** The Dolt image is pulled from `dolt.image.registry` (`docker.io`),
which is set separately.

---

## 4. Deploying with a GitOps controller

`helm install` works. So does a GitOps controller that renders the chart from
a values document (for example a Flux `HelmRelease`); nothing in the chart
depends on which. Three things are worth knowing if you use one:

- **Create the Secrets before the release, in the same namespace.** A secret
  syncing operator cannot copy a Secret across namespaces; a credential needed
  in two namespaces is two objects.
- **A controller that deploys on chart-version change only** will not pick up
  a chart edit that keeps its version. This repository refuses such an edit
  itself (`chart/CHART-SEAL`).
- **Do not make `helm test` a release gate.** On a cluster that does not
  enforce NetworkPolicy the probe is meant to fail, and a controller that treats
  a failed test as a failed release would roll gonk back every time.

---

## 5. Trusting a private CA

The published images trust the public roots of their base image and **nothing
else**. No CA is baked in, and no image sets a variable that points TLS trust at
a path (`internal/buildgate/image_trust_test.go` holds that). With the chart
defaults, the chart sets no trust variable either, and a GitLab with a publicly
trusted certificate needs nothing from this section.

If your GitLab (or LiteLLM, or a store) presents a certificate signed by
your own CA, there are two routes. Both **add** your CA to the public roots. The
same pods also talk to public endpoints, so the public roots must stay trusted.

**Route 1 (default): a ConfigMap, at run time.** Put the CA certificate (PEM; a
bundle of several is fine) in a ConfigMap in the release namespace and name it:

```yaml
gitlab:
  caCert:
    existingConfigMap: gitlab-ca   # a ConfigMap you create, or one trust-manager publishes
    key: ca.crt                    # the key in it that holds the PEM
    # mountPath: /etc/ssl/gonk/ca.crt   (the default)
```

```sh
kubectl -n <namespace> create configmap gitlab-ca --from-file=ca.crt=./my-ca.pem
```

The chart then mounts that one key at `mountPath` in `gonk-intake`, `gonk-meter`
and both containers of `gonk-controller`, and sets in each of them

```
SSL_CERT_DIR=/etc/ssl/certs:/etc/ssl/gonk
```

Go's `crypto/x509` reads one bundle file plus every file in each listed
directory. `SSL_CERT_DIR` *replaces* the directory list, which is why the system
directory is listed first; `SSL_CERT_FILE` is left unset, so the base image's
bundle is still read. The result is the system roots plus your CA.
(`SSL_CERT_FILE` set to a file holding only your CA replaces the system bundle
file. The public roots then survive only because these base images also keep
them under `/etc/ssl/certs`, which Go reads as a directory: an accident of the
base image, not something to build on, and the reason the chart does not use
it.)
Set `existingConfigMap` and `key` together; one without the other is refused at
render (guard G25). The mount is a `subPath` mount, so a changed ConfigMap is
picked up at the next pod restart, not live.

This route covers every program the chart runs against GitLab, the meter and the
stores, because all of them are Go binaries. It does **not** reach:

- `git` inside the controller image, which is linked against GnuTLS and reads
  neither variable. Nothing in the chart points it at an HTTPS remote.
- session (agent) pods. They receive no CA at all: an agent pod never talks to
  the forge, and reaches only the in-cluster endpoints the controller hands it.
  If your LiteLLM is served over HTTPS from your own CA, use route 2 for the
  agent image.

**Route 2: extend the image.** Build your own image `FROM` the published one and
add the CA to the system store, then point `componentImages` (or
`image.registry`) at it. For the Debian-based images (`gonk-controller`,
`gonk-agent`):

```dockerfile
FROM ghcr.io/gonk-factory/gonk/gonk-agent:<tag>
USER 0
COPY my-ca.pem /usr/local/share/ca-certificates/my-ca.crt
RUN update-ca-certificates
# The JavaScript runtime opencode runs on reads its own variable; this one is additive.
ENV NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/my-ca.crt
USER 65532:65532
```

`update-ca-certificates` rebuilds `/etc/ssl/certs/ca-certificates.crt` as the
public roots plus yours, which is the file Go, `git` and `curl` all read, so
nothing has to be replaced and no trust variable is needed for them.
`gonk-intake` and `gonk-meter` are distroless and have no shell; for those,
build the combined bundle in an earlier stage and copy it over the system one:

```dockerfile
FROM debian:trixie-slim AS ca
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates
COPY my-ca.pem /usr/local/share/ca-certificates/my-ca.crt
RUN update-ca-certificates

FROM ghcr.io/gonk-factory/gonk/gonk-intake:<tag>
COPY --from=ca /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
```

With route 2, leave `gitlab.caCert` empty. Do not set `GIT_SSL_CAINFO`,
`CURL_CA_BUNDLE` or `SSL_CERT_FILE` to a file that holds only your CA: each of
those replaces the public roots for the client that reads it.

Nothing in this repository turns TLS verification off in a spelling the build
checks for, and no chart value does. That is the whole claim, and
`internal/buildgate/tls_verification_test.go` is what holds it. It reads:

- every rendered golden manifest (as YAML: keys, string values, environment
  entries and argument lists), and this chart's `templates/`, `values.yaml`
  and `ci/` value files (every line that is not a comment);
- every file under `images/`, `pack/` and `hack/` that is neither Go nor
  Markdown (every line that is not a comment);
- every Go file in the repository, for one thing: code that sets
  `InsecureSkipVerify`.

In the first two it fails on: `insecure` anywhere in a key, flag or value
(`--insecure`, `insecureSkipVerify`); `curl -k`, alone or in a cluster such as
`-sk`; `skip-verify` and `skip-tls-verify`; git's `sslVerify` set to
`false`/`0`/`no`/`off`, as `http.sslVerify=false`, `sslVerify: false` or
`git config http.sslVerify false`; `GIT_SSL_NO_VERIFY`;
`NODE_TLS_REJECT_UNAUTHORIZED` and `rejectUnauthorized: false`;
`--no-check-certificate`; `verify_ssl`, `verify_tls`, `verify_peer` or
`verify` set to `false`/`none`; `sslmode=disable`; Python's `CERT_NONE` and
`_create_unverified_context`; and an environment variable named like a
verify switch whose rendered value is not `true`.

It does not read Markdown, strings inside Go code, or anything gonk does not
build; and it knows no spelling that is not in that list. The ledger's
connection string is yours: `sslmode` in a DSN you supply is not checked.
(`intake.webhookSSLVerify` is a different thing: whether GitLab verifies the
webhook endpoint's certificate when it delivers a hook. Leave it `true` and fix
the trust.)

---

## 6. Upgrading

**Every chart upgrade rolls every pod.** The `helm.sh/chart` label carries the
chart version and is on every pod template, so gonk-intake, gonk-meter, the
controller and the Dolt StatefulSet all restart on any change of chart
version, whatever else changed. The meter's `checksum/operator-config`
annotation changes with every chart version too (the checksum covers the
operator ConfigMap's labels). What follows is what changes *besides* those
labels and that annotation, found by rendering each version with the same
values.

**0.2.0 to 0.2.2: nothing but text.** 0.2.1 differs from 0.2.0 only in this
README. 0.2.2 rewords comments and guard messages, lists every Secret in the
install notes, and rewords the text of the
`gonk-factory.github.io/network-policy-enforcement` annotation on each
NetworkPolicy.

**0.2.3 changes two NetworkPolicies** and adds one value.

- `gonk-intake` gains an ingress rule: the agent session pods and the
  controller are admitted on the private listener (`intake.ports.private`).
  Both already had the matching egress rule, which did nothing without this
  one.
- `gonk-controller` allows DNS on TCP as well as UDP.
- `networkPolicy.kubeAPI` is new. Left unset, the `gonk-controller` and
  `gonk-meter` policies render as before apart from the DNS rule above. Set
  it on a cluster that enforces NetworkPolicy (section 0).

On a cluster that does not enforce NetworkPolicy, 0.2.3 changes no behaviour.
On one that does, the two rules above open what was closed by mistake, and
sessions still do not run until `networkPolicy.kubeAPI.cidrs` is set.

**0.2.4 is the chart of the v0.2.1 release.** `appVersion` is 0.2.1, so the
`app.kubernetes.io/version` label changes on every object and gonk-intake's
`GONK_VERSION` with it. Two more things change:

- `intake.blockedProjects` defaults to `[gonk-factory/gonk]`, the upstream
  project's path; it used to name a project on the maintainers' GitLab. If
  you relied on the old default to keep gonk off a copy of its own source,
  list that copy's path yourself (section 2).
- `networkPolicy.kubeAPI.cidrs` refuses a prefix shorter than `/8` (IPv4) or
  `/16` (IPv6), where 0.2.3 refused only a `/0`. A value that 0.2.3 rendered
  and that is this wide now fails the render.

No NetworkPolicy, Role or Service changes between 0.2.3 and 0.2.4.

The notes below apply when upgrading from the v0.1.0 release.

### Upgrading from the v0.1.0 release: the label key domain changed, and the defaults name no site

Three things changed after the v0.1.0 release and need an operator's attention. None applies to
a first install.

**1. Label and annotation keys moved to `gonk-factory.github.io/`.** Every key gonk
owns (`.../component`, `.../session`, `.../exposure`, `.../project`,
`.../image-floor.<image>`, ...) used to be spelled under another domain. For
almost all of them the change is atomic within one `helm upgrade`. **Session Jobs
are the exception:** gonk-gate labels a session's Job, pod and Secret when it
creates them and finds them again *by that label*, so a session started by the
old gate is invisible to the new one -- never listed, never reaped, its session
never resolved, its Secret never deleted. Nothing errors.

So drain first. Before the upgrade, with `<old-domain>` the domain your running
release uses (read it off any session Job with `kubectl get jobs --show-labels`):

```bash
kubectl -n <namespace> get jobs,pods,secrets -l <old-domain>/component=agent-session
```

must print `No resources found`. If it does not, wait for the sessions to finish
(they are bounded by their deadline) and look again. Run the same command after
the upgrade; anything it shows then is a straggler the new gate cannot see, and
is deleted by hand (`kubectl -n <namespace> delete <the names it printed>`).

**2. Site-local defaults are gone.** `image.registry` now defaults to the public
registry and `image.pullSecrets` to none; `gitlab.url`, `gitlab.caCert.existingConfigMap`,
`gitlab.caCert.key`, `ingress.host` and `ingress.annotations` default to empty.
A release that relied on any of the old defaults must set it explicitly **before**
upgrading: `gitlab.url` is required (the render fails without it), and a private
CA that is no longer named is simply not mounted. The CA is now mounted at
`gitlab.caCert.mountPath` = `/etc/ssl/gonk/ca.crt` (volume `gitlab-ca`).

**3. The trust variable changed.** The images no longer set `SSL_CERT_FILE`, and
the chart no longer sets it either. With a CA configured the chart sets
`SSL_CERT_DIR=/etc/ssl/certs:<the directory of mountPath>` instead, which adds
the CA to the public roots rather than replacing them ("Trusting a private CA"
above). Nothing has to be set by hand, and the change is safe in either order
of arrival: an older image under this chart still carries its own
`SSL_CERT_FILE` naming a file that is no longer mounted, which Go skips, and the
directories then supply both the public roots and the CA; a newer image under
the previous chart gets `SSL_CERT_FILE` and the mount from that chart, as before.
If you set `SSL_CERT_FILE` yourself through an extra-env mechanism of your own,
remove it.

### Image floors: the chart can arrive before its images

The chart and the four images can reach a cluster separately. If something
deploys the chart when its version changes and each image when that image is
built, then for a while a new chart runs against old images, and a chart that
invokes something an older image does not have (a subcommand in a readiness
probe, say) takes the workload down.

`Chart.yaml` therefore names the oldest build of each image the chart may run
against (`gonk-factory.github.io/image-floor.<controller|intake|meter>`), and guard
**G23 refuses to render** against an older tag. A refused render changes nothing
in the cluster: Helm writes no release, so the running one stays up.

- **Tags without a build number** (`v0.2.0`, `e2e-<runid>`, feature-branch
  builds) cannot be ranked and are refused. Set
  `imageFloor.acceptUnrankedTags=true` only when the images come from the same
  release as the chart. That is the case for a published release tag, and it
  is why section 2 sets it.
- The floors are tags of the maintainers' internal builds
  (`v<version>-<build>.<sha12>`); they are not published images and you never
  need to pull one.

For people changing the chart:

- **When the chart starts using something new in a binary** (a subcommand, a
  flag, a probe, an env var it now depends on), raise that image's floor, to a
  tag that exists (`hack/require_image_tags.py <tag>`). So the change takes two
  pushes: the binary first, then the floor.
- **`chart/IMAGE-CONTRACT`** lists every gonk binary invocation in the rendered
  chart goldens. `internal/buildgate` fails if the file and the goldens differ,
  or if an image's lines changed against `origin/main` and its floor did not go
  up. It does not check that the floor's build actually has the feature, and it
  cannot see env vars, HTTP paths or `gc` flags.
- **Binaries expand before they contract.** A binary keeps accepting everything
  the previous chart release invokes for at least one chart release. This is a
  written rule, not a gate.

---

## 7. The ledger (Postgres)

`ledger.backend` is Postgres, always: it has exactly one valid value (ADR-004,
`docs/spikes/dolt-reservation-isolation.md`). What varies is
`ledger.postgres.mode`:

- **`external`** -- you run Postgres. Nothing is rendered; meter reads the DSN
  you provide.
- **`shared`** (the default) -- a database on a CloudNativePG cluster that
  already exists. Also renders nothing; the chart only consumes the DSN.
- **`cnpg`** -- the chart emits a new `postgresql.cnpg.io/v1` `Cluster`
  (`gonk-ledger-postgres`). Requires the CloudNativePG operator; the chart does
  not install it. CNPG generates its own app Secret (`gonk-ledger-postgres-app`).
  Point `secrets.ledger.existingSecret` at a Secret with a DSN-shaped key or
  make your own. **The chart never copies a credential from one Secret into
  another.**

In every mode you provide the Secret named by `secrets.ledger.existingSecret`
(G19), holding a connection string for a role that owns the database
(`ledger.postgres.database`, default `gonk_ledger`):

```
postgres://<role>:<password>@<primary host>:5432/gonk_ledger
```

**Connect to the primary, never a read replica** (on CloudNativePG: the `-rw`
Service, never `-ro`). A reservation written to a read-only replica is a
reservation that does not exist, and a reservation that does not exist is a
budget that gets spent twice.

On a CloudNativePG cluster shared with other tenants, a database is created
with a `Database` resource and the role as a managed role on the `Cluster`;
`bootstrap.initdb` applies to a new cluster only and will not create a database
on an existing one. The role's password Secret must be in the cluster's own
namespace and the DSN Secret in gonk's, so that is two Secrets from one value.

---

## 8. Component toggles and BYO seams

The umbrella deploys Gas City + Dolt + intake + meter + the pack, each
`<component>.enabled`, and wires to a BYO GitLab / LiteLLM / CNPG operator.
**Turning a component off makes its external replacement required** (fail-closed):

| Knob | Off => required | Guard | Worked example (a `ci/` profile renders it) |
|---|---|---|---|
| `gascity.enabled: false` | `gascity.supervisorURL` | G17 | `values-byo-gascity`, `values-byo-everything` |
| `dolt.enabled: false` | `dolt.external.host` | G16 | `values-external-dolt`, `values-byo-everything` |
| `meter.enabled: false` (with intake on) | -- (**refuses**: unmetered dispatch) | G8 | fail-closed cell in the toggle matrix |
| `ledger.postgres.mode` | a DSN Secret in **all three** modes | G19 | `values-cnpg`, `values-byo-everything` |
| `ingress.enabled` | `className` + `host` | G12 | `values-cnpg`, `values-full-monitoring` |
| `networkPolicy.enabled` | non-empty `agentPodSelector` + a GitLab reach | G10 | `values-full-monitoring` |

Two invariants, stated plainly:

- **Beads is Dolt-only.** There is no beads-on-Postgres path. `dolt.enabled: false`
  means an **external Dolt**, never "Postgres for beads".
- **CNPG is the ledger only.** It never backs beads. The bundled Dolt stays
  deployed regardless of `ledger.postgres.mode` (see `values-ledger-cnpg`).

**The Gas City controller** has no upstream chart; this chart templates it. Three
of its deploy facts were settled by a smoke deploy, not by design --
see `smoke/gc-controller-smoke.md` for the evidence:

- **`gascity.supervisorPort: 9443`** -- the per-city `[api]` listener
  (`bind=0.0.0.0`, `allow_mutations=true`) that intake dispatches to and the
  Service/probes target. Port **8372** is a separate loopback-only supervisor
  admin API; it is **never** exposed by a Service/Ingress/NetworkPolicy.
- **`gascity.delivery: prebaked`** -- the pack is baked into the image at
  `/opt/gonk/pack`; an initContainer copies it into a writable `/city` and runs
  `gc init --preserve-existing --bootstrap-profile k8s-cell`. A read-only ConfigMap
  at `/city` is not viable.
- **`gascity.workload: deployment`** with `strategy: Recreate` -- no leader election
  exists, so exactly one controller must ever run; Recreate terminates the old pod
  before the new starts (`RollingUpdate`'s `maxSurge` could run two).

---

## 9. Enabling a cloud rung -- the deliberate three-part edit

A cloud rung costs real money, so turning one on is three edits, on purpose. Any
one alone does nothing (or fails the render); all three together is a decision:

1. **Add the rung to `operatorConfig.rungs`** -- `kind: cloud`, `est_cost_usd > 0`,
   and **no** `synthetic_usd_per_1m_tokens` (a cloud price is real and lives in
   LiteLLM's own model config; the render refuses a synthetic one).
2. **Add its name to `operatorConfig.instance.ladder`** -- the ladder is an
   allow-list; a rung not in it is unreachable (ADR-002), and a rung named in the
   ladder but absent from the catalog fails the render (G2).
3. **Raise `operatorConfig.instance.budget.monthly_cost_usd`** above 0 -- ceilings
   only tighten downward, so with a `$0` instance ceiling no project can spend a
   cent whatever its `.gonk.yml` says.

`ci/values-full-monitoring.yaml` is the profile that makes all three edits; it is
the only `ci/` profile where a dollar can be spent.

And the synthetic price: for **local** rungs, the same
`synthetic_usd_per_1m_tokens` you put here must be configured as that model's
cost in your LiteLLM. A local model priced at `$0` in LiteLLM means the USD
virtual-key ceiling never closes on tokens. The chart renders an **advisory**
`gonk-litellm-model-prices` ConfigMap with those prices for you to copy
(`litellm.emitModelPricesConfigMap`); it cannot verify that you did.

---

## 10. Meter replicas

`meter.replicaCount` defaults to **1** with `strategy: Recreate`. This is now a
**resource-footprint** choice, not a correctness guard -- meter's `ReserveIfFits`
atomicity lives in Postgres (`SELECT ... FOR UPDATE` + a partial UNIQUE index),
proven across replicas (`internal/meter/store/postgres_race_test.go`); AD-10 is
lifted (ADR-004). You **may** raise it and the chart renders cleanly with no
fail-closed guard in the way. The **controller**, by contrast, must stay
single-instance (no leader election) -- that is still a hard requirement, enforced
by `strategy: Recreate` / StatefulSet, for a different reason than meter's.

---

## 11. What this chart cannot verify

A chart that renders is not a chart that works. `helm template` cannot prove any
of these; check them on your cluster:

1. **The NetworkPolicy actually blocks** (section 0). `helm test` answers it.
2. **The Secrets exist and carry those exact key names.** A missing Secret or a
   wrong key is a `CreateContainerConfigError` at run time, not a render error.
3. **The probes pass** -- intake `/readyz` needs the bot identity, the first
   reconcile, and meter `/healthz`; meter `/readyz` is false until its first
   spend sync.
4. **The selectors under `networkPolicy` match your namespaces** (section 2).
5. **The synthetic prices agree** -- that you configured in LiteLLM the prices
   in the rung catalog (section 9).
6. **ServiceMonitors are scraped** and the **Grafana sidecar discovers** the
   dashboard ConfigMaps (the labels must match your Prometheus and Grafana).

---

## 12. RBAC

The chart renders namespaced `Role`s and `RoleBinding`s in the release
namespace and nothing cluster-wide: no `ClusterRole`, no `ClusterRoleBinding`.
Three ServiceAccounts are bound:

| Role | Bound to | Grants (resource: verbs) | Rendered when |
|---|---|---|---|
| `gc-controller` | `gc-controller` | `pods`: create, delete, get, list, patch, update, watch; `pods/exec`: create; `pods/log`: get; `configmaps`: create, delete, get, list, patch, update, watch; `secrets`: get | `gascity.enabled` |
| `gonk-gate-jobs` | `gc-controller` | `jobs`: create, delete, get, list; `secrets`: create, delete, get; `pods`: list; `pods/log`: get | `gascity.enabled` |
| `gonk-gate-podreap` | `gc-controller` | `pods`: delete, list | `gascity.enabled` and `podReap.enabled` |
| `gonk-meter` | `gonk-meter` | `secrets`: create, delete, update | `meter.enabled` |
| `gc-agent` | `gc-agent` | `pods`: get | `gascity.enabled` |

Read that table for what it grants, not for what the names suggest:

- **No rule carries `resourceNames`.** Every grant on Secrets is on every Secret
  in the namespace. RBAC cannot restrict `create` by name, has no name prefixes,
  and the Secrets involved are named per project and per session at run time.
- **The controller's ServiceAccount can read, create and delete every Secret in
  the namespace** (`get` from `gc-controller`; `create`, `get`, `delete` from
  `gonk-gate-jobs`), but cannot list them: it must already know a name. It can
  also create, delete and exec into any pod in the namespace. The controller pod
  already mounts the forge token and the LiteLLM admin key, so this adds reach
  over Secrets that are not mounted into it, such as the ledger's.
- **The meter's ServiceAccount can create, overwrite and delete every Secret in
  the namespace, and can read none.** `secrets.keySink.namePrefix` names the
  Secrets it writes; it does not limit which ones it may write.
- **A session pod's ServiceAccount (`gc-agent`) can read pod objects** and
  nothing else, and a session pod created by the Jobs runtime does not mount
  its token.

The practical consequence: give gonk a namespace of its own, and keep no Secret
in it that gonk does not use.

---

## Quick reference: rendering locally

```bash
# render the safe default profile (the whole factory, nothing exposed, $0 ceiling)
helm template gonk chart/gonk --namespace gonk --values chart/gonk/ci/values-default.yaml

# the full chart suite (needs helm 3, 3.14 or later and not helm 4, and
# kubeconform, on PATH; `make gate` runs a subset that needs helm only):
go test -tags chart ./internal/charttest/... -count=1

# regenerate the golden manifests after an INTENTIONAL template change, then READ the diff:
go test -tags chart ./internal/charttest/ -run Golden -update
```

The profiles under `ci/` are render fixtures for those tests, not install
recipes: their names, tags and the model they mention are examples.
