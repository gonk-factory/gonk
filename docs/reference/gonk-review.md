# Independent-review configuration: `actions.reviews` and `review:`

Reference for the configuration surface of independent review (gonk-cuwn,
feature bead gonk-td7) and of the part of optional merge that depends on it
(gonk-rk1m). Design: `docs/plans/2026-10-05-gonk-cuwn-epic-remainder.md`,
sections 3.1 (opt-in and targets), 3.2 N5 (the reviewer rung), 5.1 M9 and 5.3
(merge modes), and 8.1 (owner decisions, which override the earlier sections).
Code: `pkg/gonkcfg/review.go`, `rung.LadderFor`, `rung.ResolveReviewMerge`.
Schemas: `docs/schemas/gonk-config.v1.schema.json` and
`docs/schemas/gonk-operator.v1.schema.json`.

STATUS (2026-10-07, plan part X-CONFIG): **NOT ACTIVE YET. This is
configuration only.** The keys below are validated, folded and reported by
gonk-meter. Nothing dispatches the `mr-review` trigger, no review agent
exists, and gonk writes no review note and merges nothing under any setting.
The parts that make it live (RV-DISPATCH, RV-AGENT, RV-APPLY, MP-*) must
rewrite this paragraph; nothing enforces that they do.

## 1. Everything is off by default

A `.gonk.yml` or operator configuration that does not mention these keys
resolves exactly as before. To opt in, a project writes the action together
with a workflow that authors merge requests:

```yaml
actions:
  pipelines: true
  pipeline_fixes: true   # or: features: true
  reviews: true
```

`reviews: true` with neither `pipeline_fixes: true` nor `features: true` in
the same file is a **configuration error**: v1 reviews only merge requests
gonk itself authored, and with neither workflow there is none. The project is
marked invalid at registration (HTTP 422, state `invalid`), its virtual key is
removed and nothing is dispatched until the file is corrected. The same is
true of any unknown key or out-of-range value below.

## 2. Keys

### `actions`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `reviews` | boolean | `false` | A merge request gonk authored that passed its own exit check may be read by an independent review session. Requires `pipeline_fixes: true` or `features: true`. In an operator layer it is a veto only. |

The resolved value (`gonkcfg.Effective.Actions.Reviews`) is true only when the
project opted in, no operator layer vetoed it, AND at least one of
`pipeline_fixes` and `features` resolved to true.

### `review` in a project's `.gonk.yml`

| Key | Type | Default | Ceiling / allowed values | Meaning |
|---|---|---|---|---|
| `targets` | list | `[gonk]` | subset of `gonk` | Which merge requests are reviewed. `gonk`: merge requests gonk authored. It is the only value in v1 (owner decision Q5); `human` is refused. `[]` reviews nothing. |
| `max_notes` | integer | `20` | 1 to 20 | Findings one review may report. |
| `max_reviews_per_mr` | integer | `3` | 1 to 3 | Reviews of one merge request, one per head commit. |

**A project may not write `rung`.** It is refused by the schema (an unknown
key) and by `ReviewPolicy.Validate`: which model reviews is not the reviewed
project's choice.

### `review` in the operator configuration

The same keys under `instance` and each group, plus:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `rung` | rung name | unset | **Operator only.** The ONE rung of the rung catalog every review runs on. It is the operator's statement of which model is adequate for review; gonk never infers that. It must name a rung in `rungs` or gonk-meter does not start. Unset: no review can run, and `after-agentic-review` is unavailable. |

**There is no `require_distinct_model` key, in any layer** (owner decision,
2026-10-07, after the plan was written): the same model can never satisfy the
agentic-review merge mode, so there is nothing to switch. Writing the key is
refused as an unknown key. A review may still RUN on the author's model; its
note says so, and it authorises no merge.

What a review may write (one note, no threads, no approval, no reviewer field,
no merge) is fixed by owner decision Q5 and is not configurable.

## 3. Layers and the fold

`gonkcfg.ResolveReview(instance, group, project)` produces
`gonkcfg.EffectiveReview`; `gonkcfg.Resolve` stores it in `Effective.Review`.

| Key | Fold | An operator layer can | An operator layer cannot |
|---|---|---|---|
| `actions.reviews` | project opts in; any explicit `false` vetoes | switch reviews off | switch them on |
| `targets` | intersection | remove the target | add one |
| `rung` | operator layers only. Layers naming the same rung keep it; layers naming DIFFERENT rungs fold to none | name the reviewer | -- (a project cannot name it at all) |
| `max_notes`, `max_reviews_per_mr` | minimum | lower it | raise it |

Two operator layers that disagree about the rung give NO reviewer rather than
letting the more specific layer replace the reviewer with another model: a
nested group can tighten its parent, never swap the model its parent declared
adequate. `opercfg.GroupFor` folds nested groups with `gonkcfg.FoldReview`.

## 4. The meter: trigger, ladder, reservation

| Item | Value | Where |
|---|---|---|
| trigger literal | `mr-review` (`atags.TriggerMRReview`) | `pkg/atags`; pinned by `TestContractLiterals` |
| action gate | `/decide` denies with `action-not-allowed` unless `Effective.Actions.Reviews` | `rung.ActionFor` |
| ladder | NOT the project's. The one rung `review.rung` names. With none, `/decide` denies with `ladder-exhausted` ("no reviewer rung") | `rung.LadderFor` |
| floor / floor retries | 0 / 1: a refused batch re-runs the reviewer rung once, then the review is denied. A review never climbs | `rung.FloorRetries` |
| reservation TTL | the operator's `meter.reservation_ttl`, unchanged | `rung.ReservationTTL` |
| intake pre-filter | `Classification.MayReview` | `pkg/intake/state.go` |

HTTP contract (additive): `effective.actions.reviews` (boolean, never omitted)
and `effective.review` on the project response:

```
review: {
  targets: ["gonk"],
  rung: "<operator's reviewer rung, or empty>",
  max_notes: 20,
  max_reviews_per_mr: 3,
  agentic_merge: {
    pipeline_fix: { available: false, reason: "reviews-off" },
    change:       { available: false, reason: "reviews-off" }
  }
}
```

A meter that predates `review` yields no target, no rung and both statuses
unavailable. Nothing on the wire can say that a same-model review may
authorise a merge.

## 5. `after-agentic-review` and the distinct model

Owner decision Q4: an automatic merge after agentic review requires a reviewer
model DISTINCT from the author's, and when no distinct adequate reviewer model
is configured the mode is UNAVAILABLE -- a named reason -- never silently
satisfied by the same model, and never satisfied by it on request either: no
project or operator setting waives the requirement.

gonk-meter computes one status per author workflow (`pipeline_fix`, `change`)
at registration: `gonkcfg.Resolve` settles what the configuration alone
decides, then `rung.ResolveReviewMerge` compares models against the rung
catalog. First match:

| Reason | When |
|---|---|
| `reviews-off` | `actions.reviews` did not resolve to true |
| `no-review-rung` | no operator layer names `review.rung`, or two layers name different ones |
| `review-rung-not-in-catalog` | the rung is not in the catalog (refused at meter start; listed for totality) |
| `no-author-rung` | the project's ladder has no rung at or above the author workflow's floor |
| (available) | at least one rung the author can run on (the ladder from the workflow's floor up) uses a model other than the reviewer's |
| `no-distinct-model` | every rung the author can run on uses the reviewer's model |
| `models-not-compared` | `rung.ResolveReviewMerge` was not run (a caller that used `gonkcfg.Resolve` alone). Unavailable. |

"Available" is a statement about the CONFIGURATION: such a review can exist.
Whether one merge request may merge is decided per merge request, on the
recorded models of its author and its reviewer (plan 5.1 M9). A project whose
ladder is `[small, big, cloud]` with reviewer `big` is "available" because the
author may have run on `cloud`; a change the author wrote on `big` still does
not merge.

Review (feature 4) is not affected by any of this: it runs whenever
`actions.reviews` is on and a reviewer rung is named, and its note says which
model reviewed and whether that is the author's model.

`gonkcfg.UsableAutoMergeMode(actionOn, autoMerge, status)` is the one function
a merge decision may ask: it returns `off` and the reason when the mode is
`after-agentic-review` and the status is not available.

`pipeline_fix.auto_merge.mode: after-agentic-review` could be written before
`actions.reviews` existed, so such a file stays VALID and its status is
`reviews-off`. `change.auto_merge` is new, and there the same combination is
refused at registration (`docs/reference/gonk-change.md`, section 2).

## 6. Not built (stated so it is not mistaken for done)

- Everything that acts on this configuration: dispatch, the review agent, the
  gates on its output, the note, the review fact, the merge engine.
- The project's LiteLLM virtual key is provisioned with the models of the
  PROJECT'S ladder only. A reviewer rung that is not on the project's ladder
  is not on its key, so a review dispatched there would fail at the proxy.
  Part RV-DISPATCH must add the reviewer's model to the key
  (`internal/meter/service/service.go`, the two `litellm.KeySpec` sites).
- `max_notes` and `max_reviews_per_mr` are not enforced by the meter.
