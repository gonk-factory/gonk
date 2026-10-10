# gonk

A local software factory for self-hosted GitLab: Gas City orchestration,
opencode agent sessions in Kubernetes, LiteLLM-routed local-first inference,
and hard per-project token/cost budgets.

Design record: the ADRs under `docs/adr/`.
Installing it: `chart/gonk/README.md`.
Operating it: `docs/observability.md` — what every record says, and how to
answer "why did nothing happen?" from `kubectl logs` alone.
Releases: `CHANGELOG.md` and `docs/releases/`. Contributing: `CONTRIBUTING.md`
(this repository is a release mirror; read that first).

## Status

What each workflow does today. A project opts in to each in its `.gonk.yml`.

| Workflow | Status |
|---|---|
| Issue triage | active |
| Pipeline-failure diagnosis | active |
| Pipeline-fix merge requests | active; gonk opens and judges them, a person merges |
| Issue to merge request | not active: configuration only, nothing is dispatched |
| Independent review | not active: configuration only |
| Merge policy (automatic merge) | not active: gonk merges nothing under any setting |

The reference pages under `docs/reference/` each open with the exact status of
what they describe.

## Why "gonk"?

In Night City slang a gonk is a meathead — and this bot is a tireless one:
not brilliant, never idle, gets it done. Naming software after a mild
pejorative has precedent (see: git).

## Layout

- `pkg/gonkcfg` — `.gonk.yml` contract: schema, parsing, budget precedence
- `pkg/scope` -- workflow scope: path classes, the pack/operator/project fold,
  the per-dispatch lift (see `docs/reference/gonk-scope.md`)
- `pkg/atags` — attribution tags contract (request metadata -> spend ledger)
- `pkg/glab` — minimal GitLab REST client on the trust boundary (+
  `pkg/glab/glabtest`, an in-memory fake GitLab for tests)
- `pkg/ghook` — GitLab webhook verification, hardened receipt, event parsing,
  dedupe
- `pkg/meterapi` — wire contract between gitlab-intake, gonk-meter, and the
  Gas City pack
- `pkg/intake` — reconciliation, project state machine, deterministic
  onboarding, Gate-1 dispatch, metrics, the two-listener HTTP server
- `cmd/gonk-intake` — the GitLab-side service: webhooks, reconciliation,
  onboarding merge request, dispatch to Gas City (see `docs/adr/ADR-003-*.md`)
- `cmd/gonk-meter` — budgets, the rung decision, per-project keys, the spend
  ledger (`docs/api/gonk-meter-v1.md`)
- `cmd/gonk-gate` — the controller: dispatch, the Jobs session runtime
  (`docs/adr/ADR-008-*.md`), the sweep, and every write to GitLab
- `pack/` — the Gas City pack: agents, orders, prompts
- `images/` — the four images and every version pin (`images/versions.env`)
- `chart/` — the Helm chart; `test/` — component, integration, image and
  end-to-end suites

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

gonk runs as a [Gas City](https://github.com/gastownhall/gascity) pack and its
controller embeds Gas City, which is MIT licensed. The contents of `pack/` were
written clean-room from the MIT-licensed Gas City specifications and loader
source; nothing here derives from `gastownhall/gascity-packs`, which carries no
licence. See [NOTICE](NOTICE) for the detail.
