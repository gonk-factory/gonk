# Changelog

All notable changes to gonk, one section per release. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning, and before 1.0 a minor version may break compatibility --
each release says where under "Upgrading".

Each release also has notes with more detail in `docs/releases/<tag>.md`.
Identifiers of the form `gonk-xxxx` are from the maintainers' issue tracker,
which is not public; they are opaque labels here.

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
  before the upgrade (`chart/gonk/README.md`, "Upgrading to 0.1.17").
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
