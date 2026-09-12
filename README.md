# gonk

A local software factory for self-hosted GitLab: Gas City orchestration,
opencode agent sessions in Kubernetes, LiteLLM-routed local-first inference,
and hard per-project token/cost budgets.

Design spec: `docs/superpowers/specs/2026-07-12-gonk-stack-design.md`
Plan index: `PLAN.md`
Operating it: `docs/observability.md` — what every record says, and how to
answer "why did nothing happen?" from `kubectl logs` alone.

## Why "gonk"?

In Night City slang a gonk is a meathead — and this bot is a tireless one:
not brilliant, never idle, gets it done. Naming software after a mild
pejorative has precedent (see: git).

## Layout

- `pkg/gonkcfg` — `.gonk.yml` contract: schema, parsing, budget precedence
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
- `cmd/` — later services (gonk-meter; later plans)
- `pack/`, `images/`, `chart/`, `test/` — later plans

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

gonk runs as a [Gas City](https://github.com/gastownhall/gascity) pack and its
controller embeds Gas City, which is MIT licensed. The contents of `pack/` were
written clean-room from the MIT-licensed Gas City specifications and loader
source; nothing here derives from `gastownhall/gascity-packs`, which carries no
licence. See [NOTICE](NOTICE) for the detail.
