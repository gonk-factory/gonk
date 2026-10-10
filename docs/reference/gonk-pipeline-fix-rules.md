# Pipeline-fix rules: the tables the tests read

This page holds the NORMATIVE tables of the pipeline-fix workflow: when a
diagnosed pipeline failure may be handed to a fix session, which CI keys a fix
may and may not change, which ways of "making the job green" are refused and by
what, and which facts an automatic merge needs.

It is the single source of truth for them. Each table is read by a test, which
fails when the table and the code disagree -- so a change here is a change to
the product's rules, and is made together with the code:

| Section | Read by |
|---|---|
| 1 | `pkg/pipefix` `TestEveryPlanERowHasAFixture` |
| 2 | `pkg/cidef` `TestKeyListsMatchThePlan`, `TestImplicitStagesMatchThePlan`, and the key-driven tests that take their cases from the table |
| 3 | `pkg/pipefix` `TestEveryPlanCheatIsAccountedFor` |
| 4 | `pkg/mergepolicy` `TestEveryPlanARowHasAFixture` |

Configuration keys, defaults and the fold are in
[`gonk-pipeline-fix.md`](gonk-pipeline-fix.md). The rules for the issue ->
merge request workflow, and the merge facts shared by both workflows, are in
[`gonk-change-rules.md`](gonk-change-rules.md).

## 1. Eligibility: what starts a fix dispatch (E rows)

`T` is the target set: the failed, non-`allow_failure` jobs of the diagnosed
pipeline that the diagnosis's evidence names. `N` marks a failure that is
reported in a note; the others are silent.

`Eligible` -- all must hold, each a fact, in this order; the first failure is
the logged and (where marked N) noted reason:

| # | Condition | On failure |
|---|---|---|
| E1 | `actions.pipeline_fixes` AND `actions.pipelines` effective true | silent (not opted in) |
| E2 | target kind in `pipeline_fix.targets` | silent |
| E3 | run note state is `diagnosed` and `PipelineRunOpen` | silent |
| E4 | classification in `{code, test, configuration}` AND in `pipeline_fix.classes`. `flaky`, `infrastructure`, `unknown` are NEVER eligible and no config can make them so | N: "no fix attempted: classified `<class>`" |
| E5 | `T` is non-empty (a zero-job / YAML-error run has no job to judge; deferred, section 15) | N |
| E6 | every cited file's path class is inside the class row's envelope after the config fold (4.2) | N: `needs-scope` decided AT DISPATCH, zero tokens, label `gonk::needs-human` |
| E7 | the target's head SHA `==` the SHA of the run's newest failing pipeline (the fix is written against a tree whose pipeline is known red); else wait for the head's pipeline | silent, re-evaluated next look |
| E8 | cause not drifted: for each job in `T`, the newest failing pipeline's job failure key (recomputed controller-side, zero tokens) equals the recorded one | N: "the same jobs now fail differently; not fixing a stale diagnosis" |
| E9 | dedup: no fix record for this fix anchor in a terminal state; no bot-authored MR, open OR closed-unmerged, carrying this cause key in its metadata block (a human who closed a fix said no) | N on closed-unmerged; silent otherwise |
| E10 | serial: no other open fix MR on this target (`max_open = 1`, not configurable in v1) | silent |
| E11 | chain: fix MRs merged on this target since its last green pipeline `< max_chain` | N: label `gonk::needs-human` |
| E12 | correlation hold (no-op until gonk-lreg exists): the run is not linked into a roll-up whose common cause is outside this project | N |

## 2. CI definition integrity: the key groups

A fix that changes the CI definition cannot be its own evidence. Every key of
every job of the base definition belongs to one group, and the group decides
what a change to it means.

| Key group | Keys | Rule (for EVERY job in the base definition, not only `T`) |
|---|---|---|
| existence | job name, `stage` | every base job exists at head with the same stage (D1) |
| GATING | `rules`, `only`, `except`, `when`, `allow_failure`, `retry`, `start_in`, `trigger`, `parallel`; global `workflow`, `stages` (base order preserved), `default.retry` | must be EQUAL at head (D2). No lift changes this. |
| EXEC | `script`, `before_script`, `after_script`, `image`, `services`, `variables` (job and global), `hooks`, `id_tokens`, `secrets`, `inherit` | equal -> delta `preserving`. Different -> delta `exec-changing`, permitted only for jobs in `T`, only with the `ci` lift, only when `ci_exec_changes != deny` (D3). A changed EXEC key on a job outside `T` is a violation. |
| free | `needs`, `dependencies`, `artifacts`, `cache`, `tags`, `timeout`, `interruptible`, `coverage`, `environment`, `resource_group`, new jobs, new stages appended | may change under the `ci` lift |

Implicit stages. GitLab gives every pipeline the stages `.pre` (before every
other stage) and `.post` (after every other), whether or not the project's
stage list names them and wherever it names them, and its merged YAML always
carries both, first and last. They are therefore not stages OF THE LIST: they
are taken out of BOTH sides before "base order preserved" is checked, so a
stage appended after the project's last one is free although it sits before the
trailing `.post`, a stage inserted among the project's own is still a D2
violation, and naming or not naming the implicit two changes nothing. Only the
list is normalised: a job's own `stage` is compared as written, so a job moved
INTO `.pre` or `.post` is a D1 violation like any other stage change
(gonk-er0o).

## 3. The cheats, and what catches each (C rows)

| # | Cheat | Caught by | Layer | Deterministic? |
|---|---|---|---|---|
| C1 | delete the failing job (or rename it) | D1 existence; J3 `NOT_OBSERVED(absent)` | static + runtime | YES |
| C2 | `allow_failure: true` | A7 early; D2 GATING; J3 `WEAKENED` (API field) | static + runtime | YES |
| C3 | `when: manual` / `when: never` / `start_in` | D2; J3 `NOT_OBSERVED(manual)` | static + runtime | YES |
| C4 | narrow `rules` / `only` / `except` / `workflow:rules` so the job or the pipeline never runs | D2 equality (any change refused, not interpreted); J1/J3 not observed | static + runtime | YES |
| C5 | raise `retry:` or press retry until green | D2 (`retry`); J3 all-attempts rule -> `FLAKY`, never `PASSED` | static + runtime | YES |
| C6 | move the failing command into a new allowed-to-fail job | EXEC key of a `T` job changed -> `exec-changing` | static | flagged YES; intent NO (see U2) |
| C7 | edit the Makefile / test runner config / CI-included script so the job runs less | `test-config` and `other` classes are in no envelope (`SC-CLASS`); local includes are `ci` class | static | YES |
| C8 | delete the test, add `t.Skip`, add a build tag (Go) | `test` not in the `code` envelope; `SC-WEAKEN` in the `test` row | static (AST) | YES for Go |
| C9 | edit `.gonk.yml` / CODEOWNERS to widen its own scope or approvals | floor, not liftable | static | YES |
| C10 | touch files beyond the diagnosis; sprawl | cited-files bound; `SC-SIZE` | static | YES |
| C11 | swallow the exit code in the script (an appended or-true, `set +e`, `; exit 0`, a wrapper) | EXEC key changed -> `exec-changing`; A7 refuses the obvious spellings | static | flagged YES; **every spelling NO -> U2** |
| C12 | change `image` / `services` / `variables` so the command no-ops or dry-runs | EXEC key changed -> `exec-changing` | static | flagged YES; **whether it no-ops NO -> U2** |
| C13 | make the job fail differently (perturb the message, lower `timeout`, break it earlier) so the "specific" failure is gone | J3 `CHANGED`, never `PASSED`; generic-signature rule | runtime | flagged YES; **"is it the same failure in other words" NO -> U1** |
| C14 | special-case the test in product code (detect CI, hard-code the expected value) | nothing | -- | **NO -> U3** |
| C15 | weaken an assertion inside a test (`test` row), or skip a test in a language without an AST predicate | nothing in v1 (Go skip/remove only) | -- | **NO -> U4** |
| C16 | break test discovery so the job passes having run nothing | nothing in v1 | -- | **NO -> U5** |
| C17 | "fix" a failure that was never deterministic (a no-op change passes by luck) | E4 excludes `flaky`; J3 `FLAKY` when attempts disagree; A6 `min_recurrences` for auto-merge; 7.4 post-merge confirmation | runtime | **PARTIAL -> U6** |

## 4. Automatic merge: the facts required (A rows)

ALL of them are required; any fact that cannot be read means "do not merge".

| # | Fact | Source |
|---|---|---|
| A1 | `pipeline_fix.auto_merge.mode != off` in `.gonk.yml` at the TARGET branch head, and no instance/group veto | config fold |
| A2 | gonk authored it and nobody else touched it: source branch is the fix branch, the metadata block's anchor matches the record, `MR.sha == FixCommitSHA`, not draft, no `gonk::hold` label, all threads resolved | GitLab + record |
| A3 | exit verdict is FIXED: every job in `T` PASSED on its only attempt, every candidate pipeline `success`, nothing REMAINING / REVEALED / CHANGED / REGRESSED | 6.3 |
| A4 | CI delta is `none` or `preserving` (6.2). `exec-changing` is never eligible: that change is its own evidence (R5, item e) | 6.2 |
| A5 | scope still holds on the MR's CURRENT diff: gates A4-A7 of 5.3 re-run; observed diff classes are a subset of `auto_merge.classes` | 066 5.3 |
| A6 | the diagnosed failure was observed at least twice (two pipelines of the run, or two attempts of the base job, with the same job failure key) | record |
| A7 | the target branch head still equals `FixBaseSHA` (the judged pipeline is not stale against the target). Otherwise no automatic merge in v1 | GitLab |
| A8 | the mode's human or review fact (9.3) | GitLab / gonk-td7 |
| A9 | GitLab agrees: `detailed_merge_status == mergeable` (project approval rules, "pipelines must succeed", conflicts) | GitLab |

## 5. Reading the codes

Codes and references in the cells:

- The row codes this page defines (`E1`-`E12`, `C1`-`C17`, `A1`-`A9`) are stable: code, tests and
  log lines use them. A row is never renumbered; a retired row keeps its number.
- Rule identifiers such as `D1`-`D3`, `J1`-`J3`, `SC-*`, `U1`-`U6`, `K0`-`K5`
  name checks in the code and are used the same way there.
- Bare section numbers (`4.2`, `7.2`, `section 15`), and ids such as `066`,
  `H1`, `H7`, `G8`, `R5` or `VCP`, refer to the design notes these tables were
  first written in. Those notes are working documents and are not part of the
  published tree; the numbers are kept because the rows are kept word for word.
- Identifiers of the form `gonk-xxxx` are issue ids in the maintainers'
  tracker. They are opaque here.
