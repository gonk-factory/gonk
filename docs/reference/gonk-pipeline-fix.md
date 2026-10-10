# Pipeline-fix configuration: `actions.pipeline_fixes` and `pipeline_fix:`

Reference for the configuration surface of pipeline-fix merge requests
(gonk-2usw). Design: `docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md`,
sections 3.1 (opt-in), 3.4 (budget and ladder), 4.2-4.3 (scope and caps) and 9
(auto-merge); owner decisions in 14.1. Code: `pkg/gonkcfg/pipelinefix.go`.
Schemas: `docs/schemas/gonk-config.v1.schema.json` (a project's `.gonk.yml`)
and `docs/schemas/gonk-operator.v1.schema.json` (instance and group layers);
each key's `description` there says the same thing as this page.

STATUS (2026-10-07, plan parts through VERIFY): **a project that opts in gets
fix merge requests opened, JUDGED on their own pipeline, and confirmed after a
person merges them. gonk merges nothing.** With `actions.pipeline_fixes: true`,
gonk-sweep decides after each diagnosis whether a fix may be dispatched, a
read-only `pipefix` session proposes edits, and the controller gates them,
commits them to a `gonk/pipefix-*` branch and opens ONE merge request linked
to the diagnosis (sections 6 and 7). It then reads that merge request's
pipeline and says what it found -- on the merge request, on the diagnosis, and
to the meter -- tries again within `max_attempts` when the failure is still
there, and after the merge request is merged checks the target's next pipeline
(section 8). NOT built: any automatic merge (`auto_merge` is read and not
acted on), and the items of section 9. A project that has not opted in sees
nothing at all.

## 1. Everything is off by default

A `.gonk.yml` that does not mention these keys behaves exactly as it did
before they existed: `actions.pipeline_fixes` resolves to false, no lift can
be granted, and auto-merge is off. No existing file needs to change.

To opt in, a project writes both actions:

```yaml
actions:
  pipelines: true        # diagnose failed pipelines
  pipeline_fixes: true   # ...and allow a fix merge request to follow a diagnosis
```

`pipeline_fixes: true` without `pipelines: true` in the same file is a
**configuration error**: there is no diagnosis to fix. The project is marked
invalid at registration (HTTP 422 from `PUT /v1/projects/{project}`, state
`invalid`), its virtual key is removed, and nothing is dispatched or spent
until the file is corrected. The same is true of any unknown key or
out-of-range value below.

## 2. Keys

### `actions`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `pipeline_fixes` | boolean | `false` | A diagnosis may be followed by ONE small fix merge request, judged on whether that specific failure stops recurring. Requires `pipelines: true`. In an operator layer it is a veto only: `false` switches fixes off for every project under the layer, `true` opts nobody in. |

The resolved value (`gonkcfg.Effective.Actions.PipelineFixes`) is true only
when the project opted in, no operator layer vetoed it, AND
`actions.pipelines` resolved to true. An operator that vetoes diagnoses has
vetoed fixes.

### `pipeline_fix`

Every key is optional. Every key can only **tighten**: the ceilings are the
most the workflow is ever given, and a value above one is refused, not
clamped. The section may be written while `pipeline_fixes` is false (it is
validated and otherwise unused).

| Key | Type | Default | Ceiling / allowed values | Meaning |
|---|---|---|---|---|
| `classes` | list | `[code, test]` | subset of `code`, `test`, `configuration` | Diagnosis classes that may be followed by a fix. `[]` allows none. `flaky`, `infrastructure` and `unknown` cannot be listed: no configuration makes them eligible. `configuration` also needs `allow_ci`. |
| `targets` | list | `[ref]` | subset of `ref`, `mr` | Which failed pipelines may get a fix: the default branch (`ref`), merge request pipelines (`mr`). `mr` is a per-project opt-in (owner decision Q7). |
| `allow_ci` | boolean | `false` | | The project's opt-in to the per-dispatch lift of the `ci` path class, so a `configuration` fix may edit the pipeline definition. See section 4. |
| `ci_exec_changes` | string | `manual-run` | `deny`, `manual-run`, `auto` | What happens to a CI fix that changes what a job executes. `manual-run`: committed with `[skip ci]`, a maintainer starts the pipeline. `deny`: refused. `auto`: its pipeline runs unattended. Never merged automatically under any value (owner decision Q6). |
| `max_attempts` | integer | `2` | 1 to 3 | Attempts at one fix. Each also costs the project a pipeline run. |
| `max_chain` | integer | `5` | 1 to 10 | Fix merge requests per red streak (since the target was last green). |
| `verify_timeout` | string | `2h` | more than 0, at most `6h` | How long the exit check waits for the fix merge request's pipeline. A whole number and one unit: `s`, `m` or `h` (`90m`, not `1h30m`). Also lengthens the meter's reservation; see section 5. |
| `max_files` | integer | `3` | 1 to 3 | Files one fix may change. |
| `max_changed_lines` | integer | `40` | 1 to 40 | Added plus removed lines of one fix, on the diff gonk computes. |
| `paths` | list of patterns | unset | at most 64 patterns | Extra narrowing: when set, a fix may only edit paths matching one of them, in addition to every other bound. Pattern syntax is `scope`'s (`docs/reference/gonk-scope.md`, "Path patterns"). **Project only.** |
| `auto_merge.mode` | string | `off` | `off`, `after-human-approval`, `after-agentic-review` | When gonk may merge a fix that met its exit criterion. `after-human-approval`: after a project member other than the bot approved. `after-agentic-review`: after an independent review session on a different model than the author found nothing blocking; that workflow (gonk-td7) is configuration only so far (`docs/reference/gonk-review.md`), so this mode merges nothing today, and it is reported UNAVAILABLE with a reason whenever no such review can exist. |
| `auto_merge.classes` | list | `[docs]` | subset of `docs`, `ci` | Which OBSERVED diffs qualify, by `scope` path class: documentation only, or a pipeline-definition change that leaves every job's execution unchanged. Source and test changes cannot be listed (owner decision Q4). For merging, `docs` means prose files only (gonk-cuwn owner decision Q4). |
| `auto_merge.max_per_day` | integer | `3` | 1 to 3 | Automatic merges on the project in any 24 hours (gonk-cuwn plan 5.1 M11). Added by gonk-cuwn part X-CONFIG; nothing merges yet. |

`mode: off` note: YAML 1.1 tools read a bare `off` as boolean false. gonk's
parser reads it as the string and accepts it, quoted or not; quote it
(`mode: "off"`) if your own tooling complains.

## 3. Layers and the fold

The same section is accepted in the operator configuration, under `instance`
and under each group, with one difference: **`paths` is project-only** (an
operator narrows paths with `scope.protected_paths`).

`gonkcfg.ResolvePipelineFix(instance, group, project)` produces
`gonkcfg.EffectivePipelineFix`; `gonkcfg.Resolve` stores it in
`Effective.PipelineFix`. The base is the PROJECT'S value, or the default when
the project is silent. An operator layer can only tighten that base:

| Key | Fold | An operator layer can | An operator layer cannot |
|---|---|---|---|
| `actions.pipeline_fixes` | project opts in; any explicit `false` vetoes | switch fixes off | switch them on |
| `classes`, `targets`, `auto_merge.classes` | intersection | remove entries | add an entry the project (or the default) lacks |
| `allow_ci` | project opts in; any explicit `false` vetoes | forbid the lift | grant it |
| `ci_exec_changes` | strictest of `deny` < `manual-run` < `auto` | make it stricter | loosen it |
| `max_attempts`, `max_chain`, `max_files`, `max_changed_lines`, `verify_timeout`, `auto_merge.max_per_day` | minimum | lower it | raise it |
| `auto_merge.mode` | the project's mode stands only if every operator layer that names a mode names the SAME one; otherwise `off` | veto (`off`), or allow exactly one mode | switch auto-merge on |

Nested groups are folded coarsest-first by `opercfg.GroupFor` with
`gonkcfg.FoldPipelineFix`, under the same rules, so a nested group can never
loosen its parent.

`ResolvePipelineFix` is total: for a policy that never passed a schema it
drops unknown list members, reads an unknown `ci_exec_changes` as `deny` and
an unknown mode as `off`, and clamps numbers to `[0, ceiling]`.
`TestNoPipelineFixFoldIsWiderThanTheCeiling` is the property test.

## 4. `allow_ci` and the scope lift

`allow_ci` is the project's third of the conjunction `pkg/scope` requires
before a `ci` lift can be granted (`docs/reference/gonk-scope.md`, section 4):
the workflow's pack row lists `ci` as liftable, the project opted in, and no
operator layer vetoes.

```go
eff := gonkcfg.Resolve(instance, group, *projectConfig)
folded, err := scope.Fold(scope.Input{
    Row:       row,                                              // baked pack
    Instance:  instance.Scope, Group: group.Scope,
    Project:   eff.PipelineFix.TightenScope(projectConfig.Scope), // max_files / max_changed_lines
    BaseSHA:   pinnedSHA,
    Enabled:   eff.Actions.PipelineFixes,
    LiftOptIn: eff.PipelineFix.LiftOptIn(),                      // [ci] iff allow_ci
})
```

- `EffectivePipelineFix.LiftOptIn()` returns `[ci]` when `allow_ci` resolved
  true and nil otherwise. Nothing else in the section produces a lift;
  listing `configuration` in `classes` without `allow_ci` grants none.
- `EffectivePipelineFix.TightenScope(project scope.Policy)` returns the
  project's `scope:` layer with `caps.max_files` and `caps.max_lines` lowered
  to `max_files` and `max_changed_lines`. `scope.Fold` minimises caps, so a
  lower `scope.caps` value stands and a higher one changes nothing.
- `paths` is applied by the controller as a further narrowing
  (`scope.Effective.WithBead`), alongside the diagnosis's cited files.

`.gonk.yml`, `CODEOWNERS`, `.git/` and secret files are the floor: no setting
here reaches them.

## 5. The meter: trigger, ladder, reservation

| Item | Value | Where |
|---|---|---|
| trigger literal | `pipeline-fix` (`atags.TriggerPipelineFix`) | `pkg/atags`; pinned by `TestContractLiterals` |
| action gate | `/decide` denies with `action-not-allowed` unless `Effective.Actions.PipelineFixes` | `rung.ActionFor` |
| ladder floor | index 1 (the second rung): the prompt carries file content | `rung.LadderFloors` |
| floor retries | 1: the first gate failure re-runs the floor rung | `rung.FloorRetries` |
| reservation TTL | the operator's `meter.reservation_ttl` + the project's effective `verify_timeout` | `rung.ReservationTTL` |
| budgets | the project's existing monthly and per-task budgets; none per trigger | unchanged |

The longer reservation exists because a fix attempt is judged after the fix
merge request's pipeline finishes, not when its session ends. With the
defaults (60m + 2h) a `pipeline-fix` reservation is held for 3h; at the
ceiling, `reservation_ttl` + 6h. Every other trigger's TTL is unchanged.

HTTP contract (additive): `effective.actions.pipeline_fixes` (boolean) and
`effective.pipeline_fix` (object: `classes`, `targets`, `allow_ci`,
`ci_exec_changes`, `max_attempts`, `max_chain`, `verify_timeout_seconds`,
`max_files`, `max_changed_lines`, `paths`, `auto_merge{mode, classes,
max_per_day}`) on the project response; `pipeline-fix` as a value of `trigger`. A client that
predates them ignores the fields; a meter that predates them omits them, and
the zero value allows nothing.

Also additive (part DISPATCH): `operator_scope` on the project response --
`{instance, group}`, each one operator layer's `scope:` section as written
(`classes`, `caps{max_files, max_lines, max_deletes, max_file_bytes,
max_patch_bytes}`, `protected_paths`, `ci_paths`, `test_paths`, `docs_paths`,
`source_paths`, `deny_lift`, `profiles`, `allowed_profiles`). It is the
operator half of the scope fold gonk-gate makes when it decides a fix's
envelope; gonk-meter is the only holder of the operator config. `null` and
`[]` are different values for `classes`, `profiles` and `allowed_profiles`
(says nothing / allows none). The field is `null` for an invalid project, and
absent from a meter that predates it; a reader treats both as "operator layers
unknown" and dispatches nothing. It reflects the operator config as it is at
the time of the read, not as it was at registration.

## 6. Dispatch (plan part DISPATCH)

`cmd/gonk-gate/sweep_pipefix.go` decides, on every run-maintenance look at an
open diagnosed run, whether a fix may be dispatched: it fills
`pipefix.EligibleFacts` and acts on `pipefix.Eligible`. A fact group that
could not be read stays unread and the answer is "unknown", never eligible.
One line per look, `sweep: pipeline fix eligibility`, carries the reason code
of the first row that refused (`reason=E4.class-never-eligible`, ...;
`pkg/pipefix` lists them, plus `E6.outside-paths` and `Q2.class-not-confirmed`
from the sweep itself).

| Setting | Read where | Used for |
|---|---|---|
| `actions.pipeline_fixes`, `actions.pipelines` | meter's effective config | E1 |
| `targets` | meter | E2 |
| `classes` | meter | E4 |
| `allow_ci` | meter -> `scope.Input.LiftOptIn` | the `ci` lift of a `configuration` fix (section 4); never from the batch, the diagnosis or the order |
| `max_files`, `max_changed_lines` | meter -> `TightenScope` | the fold's caps, recorded on the fix |
| `paths` | meter -> `Effective.WithBead` | which cited files may be edited; none inside it is needs-scope |
| `max_chain` | meter | E11 |
| `max_attempts` | recorded on the fix; checked by gonk-dispatch against meter's attempt number | a fix past it gets no session and is `denied` |
| `ci_exec_changes`, `verify_timeout` | recorded on the fix | parts APPLY / VERIFY / CI-LIFT |
| the project's `scope:` section | `.gonk.yml` read by gonk-gate at the default branch's head commit, pinned per look | the fold |

The scope envelope per diagnosis class (plan 4.2; `pipefix_envelope.go`):

| Diagnosis class | Writable path classes | Lift |
|---|---|---|
| `code` | `source`, `docs` | none |
| `test` | `test` | none |
| `configuration` | none | `ci`, only when the fold says it is liftable (pack row, `allow_ci`, no operator `deny_lift`); and only after a person put `gonk::fix-class-confirmed` on the diagnosis landing |

always inside the pack / operator / project fold, and never `control` or
`secret`. A cited file outside the envelope is `needs-scope`, decided at
dispatch for zero tokens.

**What keeps it inert is the opt-in.** The pass is on in every build
(`pipefixPrepare` is part AGENT's preparer). For a project that has not opted
in it costs one read of the project's meter registration per look and no
GitLab request, writes nothing, stores nothing and fires nothing
(`TestAnOptedOutProjectIsInert`). A `pipeline-fix` order is refused before the
meter is asked unless gonk-sweep recorded the fix it names; the meter's action
gate denies the trigger for a project without the action; and the preparer and
the apply each read the effective configuration again and do nothing for a
project that is not opted in, whatever a record or an order says.

## 7. The session and the apply (plan parts AGENT and APPLY)

gonk-dispatch folds the fix's scope envelope AGAIN from its record
(`pipefix_refold.go`): the pack row, the operator layers and `pipeline_fix` as
the meter holds them now, and the project's `scope:` section at the commit the
look pinned. The result must name exactly the classes, lift and caps the sweep
recorded, or no session starts. The `ci` lift is minted again by
`scope.Effective.GrantLift`; the record's list cannot grant one. The session
(`pack/agents/pipefix`: the diagnosis agent's read-only permission map, no
write tool) is shown the diagnosis, the failing jobs' log excerpts, and the
cited files it may change; what it was shown is recorded on the fix record
(`PipelineFix.Context`). If no cited file can be edited at the base commit
(missing, empty, not text, generated, too large) no session starts and the fix
is `needs-scope`, for zero tokens.

When the session finishes, gonk-sweep runs the static gates over its answer
BEFORE ANY WRITE and refuses the whole batch on the first failure, by rule
code (`docs/reference/effects-contract.md`, "The fix gate" and "The gates the
apply adds"). What a project owner should know of them:

| | |
|---|---|
| what a fix may touch | only files the diagnosis cited, inside `pipeline_fix.paths`, of the path classes its diagnosis class allows (section 6), within the caps; never `.gonk.yml`, `CODEOWNERS`, or a secret; never a symlink or a submodule |
| tests | a Go fix that deletes or renames a test, adds a skip, or changes a file's build constraints is refused (`SC-WEAKEN`). **For every other language gonk has no such check**: the merge request says so in words, the record says `Weakening: not-decidable`, and such a fix is never a candidate for an automatic merge |
| CI definition | only with `allow_ci` and a person's `gonk::fix-class-confirmed`. A change to it is committed with `[skip ci]` and waits for a maintainer to start the pipeline (`awaiting-run`) unless `ci_exec_changes: auto`; with `deny`, a change to what a job executes is `needs-scope` and nothing is left in the repository. THIS BUILD treats every CI change that way, not only execution-changing ones: whether a change alters what a job executes is known only from the merged definition of the commit, after it exists |
| the merge request | one per fix, from `gonk/pipefix-<12 hex>`, targeting the diagnosed branch; `Related to` the diagnosis, never closing it; labels `gonk::pipeline-fix` and `gonk::fix-verifying` or `gonk::fix-awaiting-run`; **no assignee, no reviewer, no approval, and gonk does not merge it** |
| `gonk/*` branches | must not be protected branches (protected CI variables stay out of a fix's reach) |

Record fields, labels, markers, the commit message and the description's
layout: `docs/reference/gonk-state-record.md`, "Pipeline fix".

## 8. What happens to an open fix merge request

Zero tokens: GitLab reads, and writes of gonk's own notes, labels and the
status block at the top of the merge request's description.

**The exit check.** At most every two minutes gonk reads the merge request,
its pipelines on the commit gonk made, every attempt of their jobs (retried
ones included) and the logs of failed diagnosed jobs, and compares the
pipeline definition at that commit with the one the failure was diagnosed
under. It answers only when every such pipeline has finished AND no job
attempt of it is still waiting to run. "The pipeline is green" is not the
question; "each diagnosed job still ran, and passed, under the same
definition" is.

| What gonk found | State | Label on the merge request | Then |
|---|---|---|---|
| every diagnosed job ran and passed on every attempt, and the pipeline succeeded | `fixed` | `gonk::fix-verified` | waits for a person to merge |
| the diagnosed failure is gone and something else fails (the same job differently, or another job) | `fixed-new-failure` | `gonk::fix-verified`, `gonk::fix-new-failure` | the other failure is diagnosed AFTER the merge, as its own run, and may get its own fix merge request then: one at a time, never stacked |
| a diagnosed job still fails the same way, or the change altered what it is judged by (the job allowed to fail, its rules changed) | `not-fixed` | `gonk::fix-verifying` | another attempt adds a commit to the same branch and merge request, up to `max_attempts`; then `denied`, `gonk::fix-failed`, `gonk::needs-human` |
| a diagnosed job both failed and passed (a retry does not count as a pass) | `flaky` | `gonk::fix-inconclusive` | a person; `gonk::needs-human` on the diagnosis |
| the job did not run, the pipeline never finished within `verify_timeout`, the definition could not be compared, GitLab could not be read | `unverifiable` | `gonk::fix-inconclusive` | a person |
| a job that passed before the change fails now | `regressed` | `gonk::fix-inconclusive` | a person. It is not treated as the fix's fault: that job may be unstable by itself |
| somebody else pushed to the fix branch | `taken-over` | `gonk::fix-inconclusive` | gonk stops judging and never touches the branch again |
| the merge request was closed | `closed-unmerged` | `gonk::fix-inconclusive` | gonk does not propose this fix again |

A change committed with `[skip ci]` (`awaiting-run`, section 7) is judged on
the pipeline a maintainer starts; if nobody starts one within
`verify_timeout` it ends `unverifiable`.

A merge request carries exactly one of `gonk::fix-verifying`,
`gonk::fix-awaiting-run`, `gonk::fix-verified`, `gonk::fix-inconclusive`,
`gonk::fix-failed` at a time.

**What it costs.** Each attempt is one session and one pipeline run. To the
meter, every row above but `not-fixed` is a `success` -- which there means
"do not spend a more expensive model on this", NOT "the pipeline is fixed";
`not-fixed` is a failed attempt (it moves to the next rung of your ladder
when another attempt follows).

**After you merge it.** gonk looks at the next pipeline on the branch the fix
merged into. If the diagnosed job passes there, or fails differently, the fix
is `confirmed`; a failure that remains is a new run, diagnosed separately. If
the diagnosed job fails the same way again, the fix `recurred`: gonk labels
the diagnosis `gonk::needs-human` and proposes no revert. A green pipeline in
which the diagnosed job did not run confirms nothing; gonk waits for one in
which it does.

**What gonk cannot tell you** is unchanged by any of this (plan 6.5): whether
a change to product code fixes the defect or only satisfies the test; whether
a test edit corrects the test or guts it (gonk checks Go for removed and
skipped tests only); whether a passing job still ran every test it used to;
whether a pipeline-definition change that alters what a job executes still
tests anything; whether a different failure in the same job is a new failure
or the old one in other words; whether the fix caused the pass. A person
reviewing the merge request is the control for all six.

## 9. Not built (stated so it is not mistaken for done)

(How the deployed workflow is checked end to end is section 10.)

- `auto_merge` is resolved and exposed, and nothing acts on it. gonk merges
  nothing.
- `ci_exec_changes` is acted on only as far as section 7 says.
- `max_attempts` is NOT enforced by the meter: `/decide` does not look at it.
  gonk-dispatch enforces it against the meter's attempt number, which also
  counts attempts that ended as infrastructure failures.
- A metric per fix state. gonk-sweep is a short-lived process with no
  metrics endpoint: the fix state and the meter outcome of every settled
  attempt are on one log line (`sweep: pipeline fix settled`) and on the fix
  record, not in Prometheus.
- A pipeline-definition fix is judged by asking GitLab to lint both commits;
  the shortcut for a fix that touches no pipeline file is taken by the apply
  only, so a project whose lint cannot be read ends `unverifiable`.
- The configuration is read by gonk-intake from the default branch at
  reconcile time and registered with the meter. Reading it at the pinned base
  SHA of each dispatch (066 invariant 3), and at the target branch head for
  auto-merge (plan 9.1 A1), is the controller's job in later parts.

## 10. The dev smoke check

`hack/dev_smoke.sh <merged-sha> --pipeline-fix` (check 9; plan section 10,
rung L3) runs this whole page against the deployed dev system on the
synthetic e2e project, which opts in for it (`pipeline_fixes: true`, `targets:
[ref, mr]`, `classes: [code, test]`, `scope.profiles: [python, docs-md]`;
`allow_ci` and `auto_merge` off). It opens a throwaway merge request whose one
job fails on a wrong constant in a Python file, waits for gonk to diagnose it,
to open ONE fix merge request against the throwaway branch and to settle it,
compares gonk's verdict with its own reading of the fix merge request's
pipeline, then merges the fix as the person running it and waits for
`confirmed`. It never writes the project's default branch and merges nothing
but gonk's fix into its own throwaway branch.

| Result | Exit | Means |
|---|---|---|
| PASS | 0 | `fixed` on one clean attempt of the diagnosed job under an unchanged CI file; one state label, the status block, the marker, linked both ways; no assignee, reviewer, approval or closing pattern; not merged by gonk; one meter outcome per attempt; one fix order; then `confirmed` after the person's merge, with the diagnosed merge request still open |
| PASS-WITH-WARNING | 0 | as PASS, but the fix changed a file other than the one holding the defect, or needed a second attempt |
| INCONCLUSIVE | 3 | the model produced no acceptable fix (every refusal is printed), the diagnosis made no fix eligible, the project is not opted in, or a synthetic pipeline did not behave as designed. Never a pass |
| FAIL | 1 | a forbidden write, a verdict the check's own reading contradicts, two state labels, a second fix merge request or order, an ERROR logged for the fix, a missing outcome |

What it cannot see: the meter's ledger (it reads the controller's log line for
each outcome), and anything section 8 lists under "What gonk cannot tell you".
