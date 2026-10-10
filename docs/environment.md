# What a deployment must provide

gonk does not bundle the things it works against. This page lists what has to
exist before the chart is installed, what the chart does with each, and what
gonk assumes when something is missing. Names such as
`gitlab.example.internal` are examples, not defaults. The install steps and the
value names are in `chart/gonk/README.md`; a value quoted here in `code` is a
chart value.

Comments elsewhere in this repository cite this file for a measurement (a
smoke test against a model gateway, a CI runner's limits, a development
machine's container runtime). Those measurements were taken on the maintainers'
own deployment and its description is not published. Each requirement that
came out of one is stated below in general terms.

## GitLab

- A self-hosted GitLab that the cluster can reach over HTTP(S). `gitlab.url`
  is required and has no default.
- A bot user (`gitlab.botUsername`, default `gonk`) and an access token for
  it. With `gitlab.mode: bot-does-everything` one token with the Maintainer
  role on the projects gonk works on does everything; with
  `split-credential` a second token is used only to manage webhooks.
- GitLab must be able to deliver webhooks to `intake.webhookPublicURL`.
  gonk-intake creates and repairs the hooks itself and asks GitLab to verify
  the receiving certificate (`intake.webhookSSLVerify: true`).
- Community Edition is enough. Anything that assigns a merge request picks
  exactly one assignee, because several assignees is an Enterprise feature.
- List gonk's own repository, if it lives on the same GitLab, in
  `intake.blockedProjects`: an agent that writes code must not be handed the
  repository of the thing that gates it.

## A model gateway: LiteLLM in front of an OpenAI-compatible endpoint

- A running LiteLLM proxy with its own database, named by
  `litellm.externalURL`. The chart does not install LiteLLM. gonk has been
  exercised against LiteLLM v1.92.0 (`docs/spikes/litellm-verified.md`).
- An admin key for it. gonk-meter uses the key to create one virtual key per
  project, with a spend ceiling, and writes each into a Secret named
  `<secrets.keySink.namePrefix><project slug>`. A session pod holds only its
  project's virtual key.
- At least one model behind the proxy. Model names are yours: the rung catalog
  (`operatorConfig.rungs`) maps a rung name you choose to a LiteLLM model, and
  the chart refuses to render without a catalog, an instance ladder and
  `onboarding.defaultRung`. The chart ships no model name.
- Local models need a price. Give every local rung a
  `synthetic_usd_per_1m_tokens` above zero and configure the same number as
  that model's cost in LiteLLM. A local model priced at zero in LiteLLM never
  moves a virtual key's spend, so its ceiling never closes.
- Attribution: every session sends the header
  `x-litellm-spend-logs-metadata` with the keys in `pkg/atags`, and gonk-meter
  reads them back from the proxy's spend log.

### OPERATIONAL HAZARD: `/spend/logs` must always be bounded

An unbounded read of LiteLLM's spend log loads the whole table and can exhaust
the proxy's memory, which takes the gateway down for everything that uses it.
gonk-meter therefore reads `/spend/logs/v2` only, always with both a start and
an end date, and pages (`internal/meter/litellm/spendsource_http.go`). A change
that issues a spend query without a date bound must not be merged.

## A registry, and exact tags

- The release workflow publishes four images, `gonk-agent`, `gonk-controller`,
  `gonk-intake` and `gonk-meter`, under `ghcr.io/gonk-factory/gonk/`.
  `image.registry` defaults to that path and needs no pull secret. To use a
  mirror, set `image.registry` and name a pull Secret in `image.pullSecrets`;
  the chart does not create it.
- Pin exact tags -- never `latest`. The chart schema refuses `latest`,
  `make no-latest` and `internal/buildgate` refuse a floating tag anywhere in
  this repository, and every build pin is in `images/versions.env`.

## Kubernetes

- Version 1.28 or later (`kubeVersion` in `chart/gonk/Chart.yaml`).
- One namespace. Every object the chart creates is namespaced; it creates no
  ClusterRole and no CustomResourceDefinition.
- The right to create Roles and RoleBindings in that namespace. The chart
  grants, inside the namespace only:
  - ServiceAccount `gc-controller` (the controller pod, which also runs
    gonk-gate): `pods` get, list, watch, create, update, patch, delete;
    `pods/exec` create; `pods/log` get; `configmaps` get, list, watch, create,
    update, patch, delete; `secrets` get (one project key at a time; it
    cannot list them). The pod and ConfigMap verbs are what the embedded Gas
    City controller's Kubernetes provider uses.
  - Role `gonk-gate-jobs`, for the Jobs session runtime: `jobs` (`batch`)
    create, get, list, delete; `secrets` create, get, delete; `pods` list;
    `pods/log` get.
  - ServiceAccount `gc-agent` (session pods): `pods` get.
  - gonk-meter: `secrets` create, update, delete, for the per-project keys.
- A default StorageClass, or `dolt.persistence.storageClass`, for the bundled
  Dolt server's volume.
- Optional, and off by default: an ingress controller (`ingress.enabled`), the
  Prometheus Operator CRDs (`monitoring.serviceMonitor`,
  `monitoring.prometheusRule`), a Grafana dashboard sidecar
  (`monitoring.dashboards`), and the CloudNativePG operator
  (`ledger.postgres.mode: cnpg`). The chart installs none of them.

## Secrets

The chart creates no Secret and no value file ever holds one. You create the
Secrets, by whatever means you use (an operator that syncs from a secret store,
or `kubectl create secret`), and the chart mounts them as files. A missing
Secret or key stops the pod from starting. The list is in
`chart/gonk/README.md`.

## Postgres, and Dolt

- **The meter's ledger is Postgres**, always. You supply a database, a role
  that owns it, and the connection string in a Secret (`secrets.ledger`). It
  must reach the primary: a reservation written to a read-only replica does not
  exist. `ledger.postgres.mode` says where the server is; only `cnpg` makes the
  chart render one.
- **The work store** (`workstore.enabled`, off by default) is a second
  Postgres role and schema, both named `gonk_workstore`, never the meter's
  role. Create the role, the schema it owns and the Secret before turning it
  on. `docs/reference/gonk-state-record.md` has the detail.
- **Dolt backs the embedded Gas City controller's bead store** and nothing
  else. The chart deploys a Dolt server unless `dolt.enabled: false`, in which
  case `dolt.external.host` is required. There is no Postgres option for it,
  and the ledger is never Dolt (`docs/spikes/dolt-reservation-isolation.md`).

## Optional: a private certificate authority

The images trust the public roots of their base image and nothing else. If
GitLab or another endpoint presents a certificate from your own CA, name a
ConfigMap that holds it (`gitlab.caCert.existingConfigMap` and
`gitlab.caCert.key`); the chart mounts it and adds its directory to
`SSL_CERT_DIR` beside the system roots. No chart value makes a gonk service
skip TLS verification.
`chart/gonk/README.md`, "Trusting a private CA", has both routes.

## KNOWN LIMITATION: NetworkPolicy is only as real as the cluster's CNI

The chart renders a default-deny NetworkPolicy for the namespace and one
policy per component. By default the one for session pods allows egress to
DNS, to the model gateway, and to gonk-intake and gonk-meter, and to nothing
else.

The Kubernetes API accepts a NetworkPolicy whether or not anything enforces
it. Some network plugins do not. **gonk cannot tell from inside the chart, so
it assumes nothing: check.** `helm test <release> -n <namespace>` runs a probe
from a pod carrying the session pods' labels and reports ENFORCED or NOT
ENFORCED for your cluster.

What does not depend on enforcement, and what does:

- **Cloud-rung budgets are hard either way.** A cloud call needs a provider
  key, and the only key a session pod holds is its project's LiteLLM virtual
  key, which LiteLLM refuses at the ceiling.
- **Local-model budgets are hard only if session pods cannot reach a model
  server directly.** If policies are not enforced and a model server on your
  network answers without a credential, a session pod can call it, skip
  LiteLLM, and use it unmetered. Then local-model budgets are advisory.
- To close it: use a network plugin that enforces NetworkPolicy, or put a
  credential in front of the model server so that a direct call fails.

Say it this way wherever budgets are described: cloud-rung budgets are hard;
local-model budgets are hard only where the egress policy is enforced.

The selectors the policies use for GitLab, LiteLLM, the ingress controller,
Prometheus, DNS and the ledger are values under `networkPolicy`. Their defaults
are namespace names that may not be yours; `chart/gonk/README.md` lists them.

## Building and testing this repository

- The build is vendored and offline: `vendor/` is committed and the Makefile
  sets `GOFLAGS=-mod=vendor` and `GOPROXY=off`. A change that adds a Go
  dependency runs `go mod vendor` and commits the result.
- `make gate` needs Go at the series `images/versions.env` pins, helm 3 and
  kubeconform. `CONTRIBUTING.md` says what it runs.
- The container-backed suites (`test/component`, `test/images`) use `podman` or
  `docker`, whichever `test/harness` finds, and run containers with host
  networking so that they do not depend on a working container bridge network.
  `make images` builds with `podman build --network=host` for the same reason.
- `docs/testing-time.md` has the rules for tests that involve time.
