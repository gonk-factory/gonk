# Changelog

All notable changes to gonk, one section per release. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning, and before 1.0 a minor version may break compatibility --
each release says where under "Upgrading".

Each release also has notes with more detail in `docs/releases/<tag>.md`.
Identifiers of the form `gonk-xxxx` are from the maintainers' issue tracker,
which is not public; they are opaque labels here.

## [0.2.1]

Notes: [`docs/releases/v0.2.1.md`](docs/releases/v0.2.1.md). A maintenance
release: no workflow is added. Chart version 0.2.4 (the release asset is
`gonk-0.2.4.tgz`).

### Fixed

- NetworkPolicy, on a cluster that enforces it: agent sessions and the
  controller are admitted to gonk-intake's private listener; the controller's
  DNS rule allows TCP; egress to the Kubernetes API server is rendered from
  the new value `networkPolicy.kubeAPI.cidrs` (empty by default: see
  "Upgrading").
- Pipeline-fix hardening: a fix merge request's title and commit message
  cannot carry a closing pattern; model text in a diagnosis note is quoted; a
  quick action after a carriage return is refused; a batch is exactly one
  JSON object (trailing text and a repeated top-level key are refused); the
  GitLab token is not sent to another origin on a redirect; nested CI
  includes are followed; a merged-results or merge-train pipeline is never
  used as evidence; a diagnosis carrying a fix kind's keys is refused.
- Retry feedback is wired for issue triage and scaffolding, and a refused
  diagnosis is told the failed job names and the fact keys.
- Two gate checks that could be evaded: a test that opens with a guarded
  `return`, and `|| :` followed by `;` or a comment.
- Two port-allocating tests no longer fail on a busy machine; the lint cache
  is per checkout.
- Downloads in the workflows are pinned by checksum, commit or digest, and a
  test refuses an unpinned one.
- The test behind the chart README's statement about TLS verification checks
  what the statement says, and the statement says what the test checks.
- Eleven files with no use outside the maintainers' environment are no longer
  published; documents and comments that described that environment as fact
  state conditions instead.

### Changed

- `change.triggers` defaults to `[label]` (was `[label, command, mention]`).
  The workflow it belongs to is not active.
- `intake.blockedProjects` defaults to `[gonk-factory/gonk]`, the upstream
  project's path, in place of a path on the maintainers' GitLab.
- A diagnosis note shows its summary and remedy as a code block.

### Upgrading from 0.2.0

- **Set `networkPolicy.kubeAPI.cidrs` on a cluster that enforces
  NetworkPolicy**, to the addresses and port of the `kubernetes`
  EndpointSlice (not the Service's cluster IP), or no session Job is created.
  Cilium needs the API server allowed by a policy of its own kind.
- An install that relied on the old `intake.blockedProjects` default must set
  the value.
- Every pod restarts (the version labels are on every pod template).
- An `http://` `gitlab.url` that GitLab redirects to `https://` loses the
  token on the redirect: use the `https://` URL.
- A diagnosis carrying a fix kind's keys, an answer with text after its JSON
  object and an answer that repeats `effects` or `verdict` are refused.
- A project whose CI has a nested include gonk's YAML parser rejects, or a
  local include of the form `configs/**.yml`, gets a diagnosis and no fix.

### Known issues

- gonk-intake's private listener is unauthenticated for checkout
  registration and reconcile, and on an enforcing cluster agent session pods
  can now reach it. It matters where one install serves projects with
  different trust boundaries, not for a single-tenant install
  (`gonk-37kg.3`).
- With `ledger.postgres.mode: cnpg` and default-deny, no policy admits a
  client to the Postgres pods (`gonk-37kg.4`).
- The NetworkPolicy fixes were observed on k3s with kube-router only; Calico
  and Cilium are untested.
- A fix on a project that uses merged-results pipelines or merge trains ends
  "unverifiable" (`gonk-ow8u`).
- A sizeable share of first diagnosis attempts are refused for format with
  the reference model, and the retry does not always recover; the pipeline
  then gets a "denied" note (`gonk-zxsx`).
- Four failed jobs with names near 255 characters overflow the diagnosis
  prompt and no session starts (`gonk-37kg.5`).
- The meter's Role covers every Secret in the namespace (`gonk-6by9`).
- A meter pod that starts during a model-gateway outage never becomes Ready;
  an outage still takes meter and intake readiness down (`gonk-88ez`).
- The release workflow's newly pinned downloads have not run yet
  (`gonk-5at0`).
- Images tagged plainly `v0.2.1` carry no build number: installing them needs
  `imageFloor.acceptUnrankedTags=true`.
- The other chart defaults listed under 0.2.0 still describe the maintainers'
  environment (`gonk-mp43`).

## [0.2.0]

Notes: [`docs/releases/v0.2.0.md`](docs/releases/v0.2.0.md). The first commit
of this repository's public history; chart version 0.2.1 (the chart's version
is its own: 0.2.1 is 0.2.0 with the chart README rewritten as an install
guide, and the release asset is `gonk-0.2.1.tgz`).

### Added

- Pipeline-failure diagnosis (`actions.pipelines`): one tracking issue per
  failing run, grounded in the job logs; the agent is read-only and the
  controller verifies what it quotes and cites.
- Pipeline-fix merge requests (`actions.pipeline_fixes`): one gated fix merge
  request per diagnosis, judged on its own pipeline by the exit check, retried
  within `max_attempts`, confirmed after a person merges it. gonk merges
  nothing.
- The Kubernetes Jobs session runtime (ADR-008): gonk-gate creates one Job per
  agent session.
- Workflow scope: path classes, the pack/operator/project fold, and ecosystem
  profiles (`docs/reference/gonk-scope.md`).
- Retry feedback for pipeline diagnosis and pipeline fixes: a refused answer's
  retry is told what was wrong, in controller-built text.
- One `releaseTag` for every image, and a per-image floor in `Chart.yaml` that
  the render enforces.
- Configuration surfaces for three workflows that are **not active** in this
  release -- issue to merge request (`actions.features`, `change:`),
  independent review (`actions.reviews`, `review:`) and merge policy
  (`auto_merge`). The keys are validated and reported. Nothing dispatches them,
  no agent exists for them, and gonk merges nothing under any setting.
- `CONTRIBUTING.md`, this changelog, and per-release notes.

### Changed

- **Breaking.** The Go module path is `github.com/gonk-factory/gonk`.
- **Breaking.** Seven chart values no longer default to the maintainers' own:
  `gitlab.url` is required; `image.registry` defaults to the public registry
  and `image.pullSecrets` to none; `gitlab.caCert.existingConfigMap`,
  `gitlab.caCert.key`, `ingress.host` and `ingress.annotations` default to
  empty. Other defaults still do: see "Known issues".
- `docs/environment.md` is now a short page on what a deployment must provide,
  and `chart/gonk/README.md` an install and configuration guide; neither
  describes a particular site any more.
- **Breaking.** Every label and annotation key gonk owns is under
  `gonk-factory.github.io/`. Session Jobs are found by label: drain sessions
  before the upgrade (`chart/gonk/README.md`, "Upgrading from the v0.1.0 release").
- **Breaking.** No image and no chart value sets `SSL_CERT_FILE`. With a CA
  configured the chart sets `SSL_CERT_DIR` to the system roots plus the CA's
  directory; the default mount path is `/etc/ssl/gonk/ca.crt`.
- **Breaking.** The configuration schemas' `$id`s are under
  `https://raw.githubusercontent.com/gonk-factory/gonk/main/docs/schemas/`.
- **Breaking.** A `fix_meta` effect's explanation key is `reason`, not
  `rationale`.
- The meter contract, the `.gonk.yml` and operator schemas and the state record
  gained optional keys and fields for the workflows above; a file that does not
  mention them resolves as before.
- The release notes of a release are `docs/releases/<tag>.md`; the release
  workflow publishes that file and fails if it is missing.

### Known issues

- A model-gateway outage takes gonk-meter and gonk-intake readiness down and
  fails a Helm upgrade in flight; no alert ships for it (`gonk-88ez`).
- Retry feedback is not wired for issue triage (`gonk-hpgy`).
- A diagnosis effect that carries a fix kind's keys is accepted and the keys
  ignored (`gonk-8yux`).
- Two port-allocating tests can fail on a heavily loaded machine (`gonk-njmr`).
- Images tagged plainly `v0.2.0` carry no build number: installing them needs
  `imageFloor.acceptUnrankedTags=true`.
- Some chart defaults still describe the maintainers' environment and must be
  overridden: `intake.blockedProjects`, `ledger.postgres.mode` with
  `ledger.postgres.shared.*`, `networkPolicy.ledgerPostgres.namespaceSelector`,
  `ingress.className` and `networkPolicy.ingressController.namespaceSelector`
  (`gonk-mp43`; `chart/gonk/README.md` section 2 has the table).

## [0.1.0]

Published from an earlier form of this repository that no longer exists. The
history you see starts at 0.2.0, and nothing here can vouch for what was
published as 0.1.0.
