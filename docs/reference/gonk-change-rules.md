# Issue-change and merge rules: the tables the tests read

This page holds the NORMATIVE tables of the issue -> merge request workflow
and of automatic merge: who may start an issue change, when one is eligible,
how "done" is judged when there is no failing job to turn green, the `change:`
example whose values are the defaults, and the facts an automatic merge needs.

It is the single source of truth for them. Each is read by a test, which fails
when the page and the code disagree -- so a change here is a change to the
product's rules, and is made together with the code:

| Section | Read by |
|---|---|
| 1 | `pkg/changejudge` `TestEveryPlanRowHasAFixture` (the trigger subtest) |
| 2 | `pkg/changejudge` `TestEveryPlanRowHasAFixture` (the eligibility subtest) |
| 3 | `pkg/changejudge` `TestEveryPlanRowHasAFixture` (the K block and outcome table subtests) |
| 4 | `pkg/gonkcfg` `TestPlan27ExampleIsTheDefaults` |
| 5 | `pkg/mergepolicy` `TestEveryPlanMRowHasAFixture` |

Configuration keys, defaults and the fold are in
[`gonk-change.md`](gonk-change.md) and [`gonk-review.md`](gonk-review.md). The
pipeline-fix tables are in
[`gonk-pipeline-fix-rules.md`](gonk-pipeline-fix-rules.md).

## 1. Who may start an issue change

The decision the trigger implements, as decided by the project owner:

THREE trigger forms, each valid only when the actor is a project OWNER or MAINTAINER (tighter than the Developer+ this plan recommended): (a) the implement label applied by such a member (label event actor checked); (b) a slash-style command in an issue note by such a member; (c) an at-mention of the bot asking for implementation by such a member. All deterministic: the actor's access level is read from GitLab, the request form is matched syntactically, no model decides whether to implement. DEFERRED, filed as its own bead: a `.gonk.yml` statement naming labels whose issues are implemented automatically (no per-issue human trigger). The triage-verdict trigger stays out of v1.

[`gonk-change.md`](gonk-change.md) section 3 explains the three forms and the
`triggers` key that selects among them.

## 2. Eligibility (I rows)

"The mode's trigger fact" in I2 is section 1's: one of the three request
forms, made by a member who may make it.

Eligibility `change.Eligible(facts) (bool, Reason)`, pure, first failure wins,
`N` = a note on the issue:

| # | Condition | On failure |
|---|---|---|
| I1 | `actions.features` effective true | silent |
| I2 | the mode's trigger fact (table above); a label added by a non-member or by the bot is not the fact | silent; N once for "label added by someone who cannot request this" |
| I3 | issue is open, not confidential (an MR would publish it), not bot-authored | N |
| I4 | step 0: the default branch head SHA has a pipeline and its status is `success` | N: "the default branch is red (or has no pipeline); gonk does not build on a base it cannot judge". No pipeline at all = `no-verifier-path`, zero tokens (VCP "Step 0 is not optional"; 066 4.2 moves base-broken before dispatch) |
| I5 | dedup: no change record for this anchor in flight or terminal; no bot MR, open or closed-unmerged, whose metadata block carries this anchor | N on closed-unmerged ("a maintainer closed !N; re-add the label to ask again" -- re-ask is section 7, deferred) |
| I6 | serial: open change MRs on this project `< change.max_open` (default 1) | silent, re-evaluated |
| I7 | `.gonk.yml` at the pinned base SHA is valid and the scope fold succeeds | OURS: no dispatch, no note blaming the issue |
| I8 | when `change.require_test_report`: the base pipeline's test report (H1) has `total_count > 0` | N: `no-verifier-path` |

## 3. "Done" when there is no failing job to turn green

`TB` and `TH` are the test-report case sets of the base pipeline and of the
pipeline of the merge request's head; `ChangeCommitSHA` is the commit gonk
wrote. Everything named here is a fact read from GitLab or recorded at
dispatch, never something a session reported.

```
K0 identity     MR.state == opened AND MR.sha == ChangeCommitSHA
                  closed -> closed-unmerged ; merged -> merged ; sha differs -> taken-over
K1 settle       C = pipelines of the MR with sha == ChangeCommitSHA, source != parent_pipeline
                  empty or unsettled: before the deadline -> wait ; after -> UNVERIFIABLE(timeout)
K2 definition   every ci-class blob SHA at head == at base (SC-CIDEF), else UNVERIFIABLE(definition)
                  (cannot differ for a gonk commit: ci is in no envelope. Checked, not assumed.)
K3 jobs         A(j) = all attempts of job j across C
                  some attempt failed, some succeeded          -> FLAKY
                  every attempt of some non-allow_failure j failed -> RED(j)
                  any c in C not `success` otherwise (canceled, skipped, manual-blocked) -> UNVERIFIABLE(status)
K4 tests        only when the diff touches `source` or `test`:
                  TH unreadable                                 -> UNKNOWN: no verdict this pass
                  TH.total == 0                                 -> TESTS_UNOBSERVED
                  P(B) = cases with status success in TB ; P(H) likewise
                  TB present and P(B) not a subset of P(H)      -> TESTS_REDUCED
                  profile go: an added test function has no success case named for it in TH -> NEW_NOT_RUN
                  otherwise: TB present and P(H) minus P(B) is empty -> NEW_NOT_RUN
                  else                                          -> TESTS_OBSERVED
K5 verdict, first match:
                  any RED                    -> NOT_GREEN
                  NEW_NOT_RUN                -> TESTS_NOT_RUN
                  any FLAKY                  -> FLAKY
                  UNVERIFIABLE from K1-K3    -> UNVERIFIABLE
                  TESTS_REDUCED              -> UNVERIFIABLE(tests-reduced)
                  TESTS_UNOBSERVED           -> GREEN_UNOBSERVED   (UNVERIFIABLE when change.require_test_report)
                  else                       -> GREEN
```

What each verdict becomes -- the state, the meter outcome, whether the next
attempt escalates, and the label:

| Verdict | State | Meter | Escalates | MR label |
|---|---|---|---|---|
| static gate refuses the batch | -- | gate-failed | yes | -- (nothing written) |
| `needs-scope` | `needs-scope` | success | no | issue: `gonk::needs-human` |
| NOT_GREEN, TESTS_NOT_RUN | `not-verified` | gate-failed | yes; the next attempt adds a commit and is shown the failing job excerpt or "your new test was not collected" | `gonk::change-verifying` |
| GREEN | `verified` | success | no | `gonk::change-verified`, `gonk::change-tests-observed` |
| GREEN_UNOBSERVED | `verified-unobserved` | success | no | `gonk::change-verified`, `gonk::change-tests-unobserved` |
| FLAKY, UNVERIFIABLE(*) | `inconclusive` | success ("more tokens cannot answer it") | no | `gonk::change-inconclusive`, issue `gonk::needs-human` |
| ours (config, base read, apply) | -- | infra-failed | no | -- |

## 4. The `change:` example

The values in this example are the defaults, and its comments state the
ceilings. It is kept exactly as first written, INCLUDING the key `trigger`,
which was replaced by the list `triggers` (section 1) before anything shipped:
a configuration that still says `trigger:` is refused, and the test that reads
this block asserts both that refusal and that every other line resolves to the
defaults.

```yaml
actions:
  features: true          # EXISTING key; today it only labels gonk::fix-queued
change:                   # NEW; every key optional; every key can only TIGHTEN
  trigger: label          # label | triage-verdict
  create_classes: [test, docs]     # ceiling [source, test, docs]
  require_docs: when-labelled      # never | when-labelled | always
  docs_labels: [feature, enhancement, "type::feature"]
  require_test_report: false
  max_attempts: 2         # ceiling 3
  max_open: 1             # ceiling 3
  verify_timeout: 2h      # ceiling 6h
  max_files: 6
  max_changed_lines: 200
  paths: []
  auto_merge:             # section 5; same sub-schema as pipeline_fix.auto_merge
    mode: off
    classes: [docs]       # ceiling [docs] for this workflow
```

## 5. Automatic merge: the facts required (M rows)

ALL of them are required, for a pipeline-fix merge request and for an
issue-change one, and each is re-read in the evaluation pass that merges: it
is fetched from GitLab or the store inside the same pass, and nothing is
carried over from the pass that judged. Any fact that cannot be read means
"do not merge". Rows M1-M10 carry the pipeline-fix facts A1-A9 of
[`gonk-pipeline-fix-rules.md`](gonk-pipeline-fix-rules.md) section 4.

| # | Fact | Source, re-read at merge time |
|---|---|---|
| M1 | `auto_merge.mode != off` for this MR kind in `.gonk.yml` at the TARGET branch head, no operator veto, modes agree across layers | config fetched at the target head SHA (not the reconcile cache) |
| M2 | gonk authored it and nobody else touched it: bot author; source branch has the workflow's prefix; metadata anchor matches the record; `MR.sha ==` the judged commit; not draft; no `gonk::hold`; `blocking_discussions_resolved` | `GetMergeRequest` + record |
| M3 | the workflow's exit verdict on that SHA is its best one: pipefix `FIXED` with every target job PASSED on its only attempt; change `GREEN` | the verdict is RE-COMPUTED from live pipelines and jobs, not read from the record |
| M4 | CI delta is `none` or `preserving` | re-computed (blob SHAs; `cidef.Diff` when a `ci` path changed) |
| M5 | scope holds on the MR's CURRENT diff: `ListMRDiffs` -> reclassify -> static gates; observed classes are a subset of `auto_merge.classes` | GitLab |
| M6 | class-specific evidence (5.2) | GitLab + record |
| M7 | target branch head `==` the base SHA the judged pipeline was built on | `ResolveRef` |
| M8 | mode `after-human-approval`: >= 1 approver who is a project member with access >= Developer, is not the bot, and whose approval is newer than the head commit (H7). If H7 cannot be observed on this GitLab: only an MR with exactly one gonk commit is eligible | `GetMRApprovals`, `ListMembers`, MR notes |
| M9 | mode `after-agentic-review`: `actions.reviews` true AND a review fact exists for exactly this head SHA with verdict `no-blocking-findings` AND `independence == distinct-model` (unless `review.require_distinct_model: false`). Under EVERY mode: no review fact for this SHA says `blocking` | review record |
| M10 | GitLab agrees: `detailed_merge_status == mergeable` | `GetMergeRequest` |
| M11 | rate: automatic merges on this project in the last 24h `< auto_merge.max_per_day` (default 3) | store |

## 6. Reading the codes

Codes and references in the cells:

- The row codes this page defines (`I1`-`I8`, `M1`-`M11`) are stable: code, tests and
  log lines use them. A row is never renumbered; a retired row keeps its number.
- Rule identifiers such as `D1`-`D3`, `J1`-`J3`, `SC-*`, `U1`-`U6`, `K0`-`K5`
  name checks in the code and are used the same way there.
- Bare section numbers (`4.2`, `7.2`, `section 15`), and ids such as `066`,
  `H1`, `H7`, `G8`, `R5` or `VCP`, refer to the design notes these tables were
  first written in. Those notes are working documents and are not part of the
  published tree; the numbers are kept because the rows are kept word for word.
- Identifiers of the form `gonk-xxxx` are issue ids in the maintainers'
  tracker. They are opaque here.
