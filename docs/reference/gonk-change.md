# Issue-change configuration: `actions.features` and `change:`

Reference for the configuration surface of the issue -> merge request workflow
(gonk-cuwn, feature bead gonk-3so). Design:
`docs/plans/2026-10-05-gonk-cuwn-epic-remainder.md`, sections 2.1 (trigger),
2.7 (config), 2.9 (context window), 5.3 (merge modes) and 8.1 (owner
decisions, which override the earlier sections). Code: `pkg/gonkcfg/change.go`.
Schemas: `docs/schemas/gonk-config.v1.schema.json` (a project's `.gonk.yml`)
and `docs/schemas/gonk-operator.v1.schema.json` (instance and group layers);
each key's `description` there says the same thing as this page.

STATUS (2026-10-07, plan part X-CONFIG): **NOT ACTIVE YET. This is
configuration only.** The keys below are validated, folded and reported by
gonk-meter. Nothing dispatches the `issue-change` trigger, no agent exists for
it, and gonk opens no merge request from an issue under any setting. A project
that writes `change:` today gets a validated section and nothing else. The
parts that make it live (CH-DISPATCH, CH-AGENT, CH-APPLY, CH-VERIFY) must
rewrite this paragraph; nothing enforces that they do.

## 1. Everything is off by default

A `.gonk.yml` that does not mention these keys resolves exactly as it did
before they existed (`TestExistingConfigsResolveByteIdentically`, against a
golden written before the keys were added). No existing file needs to change.

The opt-in is the existing action:

```yaml
actions:
  features: true
```

and even then gonk does nothing for an issue until a person asks (section 3).

An unknown key, a value outside its set, or a number above its ceiling is a
**configuration error**: the project is marked invalid at registration (HTTP
422 from `PUT /v1/projects/{project}`, state `invalid`), its virtual key is
removed, and nothing is dispatched or spent until the file is corrected. A
value above a ceiling is refused, never clamped.

## 2. Keys

### `change`

Every key is optional. The section may be written while `actions.features` is
false (it is validated and otherwise unused), with one exception: see
`auto_merge.mode`.

| Key | Type | Default | Ceiling / allowed values | Meaning |
|---|---|---|---|---|
| `triggers` | list | `[label]` | subset of `label`, `command`, `mention` | Which forms of an explicit request start the workflow (section 3). `[]` means nothing starts it. `triage-verdict` is not a value: no model's verdict starts this workflow in v1. |
| `create_classes` | list | `[test, docs]` | subset of `source`, `test`, `docs` | `scope` path classes in which a change may CREATE a new file. `source` is a per-project opt-in. `[]`: a change may only edit existing files. |
| `require_docs` | string | `when-labelled` | `never`, `when-labelled`, `always` | When a change to source code must also change documentation. `when-labelled`: when the issue carries one of `docs_labels`. |
| `docs_labels` | list of label names | `[feature, enhancement, "type::feature"]` | at most 16 labels of 1 to 255 characters, one line, no leading or trailing space | The issue labels that make documentation required under `when-labelled`. A label can only ADD the requirement. |
| `require_test_report` | boolean | `false` | | `true`: the workflow does not start unless the default branch pipeline publishes a JUnit test report, and a change whose pipeline publishes none is unverifiable rather than verified. |
| `max_attempts` | integer | `2` | 1 to 3 | Attempts at one issue. Each also costs the project a pipeline run. |
| `max_open` | integer | `1` | 1 to 3 | Open change merge requests on the project at once. |
| `verify_timeout` | string | `2h` | more than 0, at most `6h` | How long the exit check waits for the merge request's pipeline. A whole number and one unit: `s`, `m` or `h`. Also lengthens the meter's reservation (section 5). |
| `max_files` | integer | `6` | 1 to 8 | Files one change may touch, edited plus created. |
| `max_changed_lines` | integer | `200` | 1 to 300 | Added plus removed lines of one change; every line of a created file counts. |
| `paths` | list of patterns | unset | at most 64 patterns | Extra narrowing: when set, a change may only touch paths matching one of them. Pattern syntax is `scope`'s. **Project only.** |
| `auto_merge.mode` | string | `off` | `off`, `after-human-approval`, `after-agentic-review` | When gonk may merge a change that met its exit criterion. Any mode but `off` **requires `actions.features: true`** in the same file, and `after-agentic-review` additionally **requires `actions.reviews: true`**; written without, the file is invalid. See section 6. |
| `auto_merge.classes` | list | `[docs]` | subset of `docs` | Which OBSERVED diffs qualify. `docs` means PROSE ONLY: every changed file is a documentation text file (`.md`, `.rst`, `.txt`, `.adoc`). A script or configuration file under `docs/` does not qualify, and `source`, `test` and `ci` cannot be listed. `[]` allows none. |
| `auto_merge.max_per_day` | integer | `3` | 1 to 3 | Automatic merges on the project in any 24 hours. |

The plan's section 2.7 wrote a single `trigger:` key with the values `label`
and `triage-verdict`. Owner decision Q2 (8.1) replaced it: the key is the list
`triggers`, its values are the three request forms, and `trigger:` is refused
as an unknown key.

Not keys: the caps the plan's section 2.2 fixes for every project (at most 3
created files, 16 KiB per created file, 64 KiB per edited file, no deletes).
They belong to the agent's pack row and no layer can change them.

## 3. Who can start it, and how

Owner decision Q2: issue -> merge request is triggered ONLY by an explicit
request from a project **Owner or Maintainer**, in one of three forms:

| Form (`triggers` value) | The request |
|---|---|
| `label` | that member applies the implement label to the issue |
| `command` | that member writes a slash-style command in an issue note |
| `mention` | that member at-mentions the bot and asks for implementation |

- The minimum access level is `gonkcfg.ChangeMinAccessLevel` = 40
  (Maintainer). It is a constant, not a setting; Developer is not enough.
- The actor's access level is read from GitLab and the request form is matched
  syntactically. No model decides whether an issue should be implemented.
- `triggers` selects which forms a project accepts; it cannot widen who may
  ask.
- Deferred, not built and not configurable: a `.gonk.yml` statement naming
  labels whose issues are implemented with no per-issue request.

What the session may do is fixed by owner decision Q1: no shell, no local
execution; it reads a checkout and proposes edits and new files as text, and
the project's own merge request pipeline is the only verifier.

Context window (owner decision Q3, closing gonk-gyj): the v1 design window is
131072 tokens. The trigger starts at ladder index 1 for that reason, and the
defaults above (6 files, 200 lines) are sized to it.

## 4. Layers and the fold

The same section is accepted in the operator configuration, under `instance`
and under each group, minus `paths`.

`gonkcfg.ResolveChange(instance, group, project)` produces
`gonkcfg.EffectiveChange`; `gonkcfg.Resolve` stores it in `Effective.Change`.
The base is the PROJECT'S value, or the default when the project is silent. An
operator layer can only tighten that base:

| Key | Fold | An operator layer can | An operator layer cannot |
|---|---|---|---|
| `triggers`, `create_classes`, `auto_merge.classes` | intersection | remove entries | add an entry the project (or the default) lacks |
| `require_docs` | strictest of `never` < `when-labelled` < `always` | make it stricter | loosen it |
| `docs_labels` | the project's list (or the default), plus every operator layer's (union) | add labels | remove one |
| `require_test_report` | `true` in any layer applies | require the report | waive it |
| `max_attempts`, `max_open`, `max_files`, `max_changed_lines`, `verify_timeout`, `auto_merge.max_per_day` | minimum | lower it | raise it |
| `auto_merge.mode` | the project's mode stands only if every operator layer that names a mode names the SAME one; otherwise `off` | veto (`off`), or allow exactly one mode | switch auto-merge on |

Nested groups are folded coarsest-first by `opercfg.GroupFor` with
`gonkcfg.FoldChange`, under the same rules.

`ResolveChange` is total: for a policy that never passed a schema it drops
unknown list members (so `triage-verdict`) and malformed labels, reads an
unknown `require_docs` as `always` and an unknown mode as `off`, and clamps
numbers to `[0, ceiling]`. `TestNoChangeFoldIsWiderThanTheCeiling` is the
property test.

Helpers on `EffectiveChange`: `AllowsTrigger(form)`,
`AllowsCreateClass(class)`, `DocsRequired(issueLabels)`, and
`TightenScope(project scope.Policy)`, which lowers `scope.caps.max_files` and
`max_lines` to `max_files` and `max_changed_lines` exactly as the pipeline-fix
one does.

## 5. The meter: trigger, ladder, reservation

| Item | Value | Where |
|---|---|---|
| trigger literal | `issue-change` (`atags.TriggerIssueChange`) | `pkg/atags`; pinned by `TestContractLiterals` |
| action gate | `/decide` denies with `action-not-allowed` unless `Effective.Actions.Features` | `rung.ActionFor` |
| ladder floor | index 1 (the second rung) | `rung.LadderFloors` |
| floor retries | 1 | `rung.FloorRetries` |
| reservation TTL | the operator's `meter.reservation_ttl` + the project's effective `change.verify_timeout` | `rung.ReservationTTL` |
| intake pre-filter | `Classification.MayChangeIssues` | `pkg/intake/state.go` |

HTTP contract (additive): `effective.change` on the project response --
`triggers`, `create_classes`, `require_docs`, `docs_labels`,
`require_test_report`, `max_attempts`, `max_open`, `verify_timeout_seconds`,
`max_files`, `max_changed_lines`, `paths`, `auto_merge{mode, classes,
max_per_day}` -- and `issue-change` as a value of `trigger`. Lists are `[]`,
never `null`. A meter that predates the field omits it, and the zero value
allows nothing: no trigger form, zero attempts, zero open merge requests.
`meterapi.Change.EffectiveChange()` is the reader's conversion.

The same folded values are stored with the project's registration
(`internal/meter/store`, the `effective` JSON column, keys `pipeline_fix`,
`change`, `review`).

## 6. Automatic merge

Default OFF: with no `auto_merge` block, or with `mode: off`, a human merges
every change. gonk never approves and never assigns itself.

| Mode | Meaning |
|---|---|
| `off` | a human merges |
| `after-human-approval` | gonk merges once a project member other than the bot approved |
| `after-agentic-review` | gonk merges once an independent review on a DIFFERENT model than the author found nothing blocking. When no distinct reviewer model is configured the mode is UNAVAILABLE: it merges nothing and says why. See `docs/reference/gonk-review.md`, section 5. |

A consumer must not read `auto_merge.mode` directly. It calls
`gonkcfg.UsableAutoMergeMode(actionOn, autoMerge, reviewMergeStatus)`, which
answers `off` with a reason when the workflow is off (`action-off`) or when
the agentic mode cannot be satisfied (`reviews-off`, `no-review-rung`,
`no-distinct-model`, ...).

Never eligible under any configuration: a diff touching a `source` or `test`
path.

## 7. Not built (stated so it is not mistaken for done)

- Everything that acts on this configuration: the dispatcher and its member
  check, the agent, the gates, the apply, the exit check, the merge engine.
- The label name, the slash command's spelling and the mention's syntax are
  the dispatcher's (CH-DISPATCH) and are not defined here.
- `max_attempts` and `max_open` are NOT enforced by the meter: `/decide` does
  not read them.
- The configuration is read by gonk-intake from the default branch at
  reconcile time. Reading it at the pinned base SHA of each dispatch, and at
  the target branch head for a merge, is the controller's job in later parts.
