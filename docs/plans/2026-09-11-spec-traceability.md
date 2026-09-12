# Spec traceability: make "what is shipped" a file read, not archaeology

> **STATUS: NOT APPROVED FOR BUILD. Independently reviewed 2026-09-11 and the
> reviewer's verdict was DO NOT BUILD AS DESIGNED** -- see
> `docs/reviews/2026-09-11-spec-and-traceability-review.md`. Kept in the repo
> because the review is only legible next to what it reviewed. Three of this
> plan's premises are known false, verified in tree:
>
> 1. **§1.4 is wrong.** Spec §11.4 already names `continuity: fresh` as an
>    acceptable outcome of M4, so E10 is the spec taking its own escape hatch,
>    not contradicting it. T-43's text already says "spec §4.2 and §11.4
>    amended", so "nothing reconciles them" (§1.4) and "filed separately" (§5)
>    are both false.
> 2. **§2.1's ID scheme collides.** `G1`-`G20` already name the chart's
>    fail-closed guards (`chart/gonk/README.md:8`, `PLAN.md:866`,
>    `ADR-006:128`).
> 3. **§2.3 assertion 5 is not buildable as described.** `ci_build_tags_test.go`
>    contains zero references to `.gitlab-ci.yml` and extracts `-tags` flags,
>    not packages.
>
> The reviewer also found the spec's eight goals are not a gradable unit (G3
> alone spans Demo + Not done + Withdrawn), and that the delivery plan's
> E1-E10 already are that unit. The proposed replacement is a two-column
> amendment to the delivery plan's §1, not a new artifact.
>
> **The sharpest finding is about the diagnosis, not the design:** this plan
> proposes a fifth document to fix a problem caused by not reading three
> existing ones.

**Filed** 2026-09-11. **Bead** gonk-trace (to be created).

## 1. The failure this exists to stop

On 2026-09-11 the owner asked "what features do we have tested and shipped?"
The session answered by reading `git log`, `bd list`, and the HelmRelease --
roughly twenty minutes of archaeology. The owner's response was the diagnosis:

> you should be going from spec to test to implementation to documentation to
> release, so you shouldn't have to go all the way to source to answer this
> question. particularly if you are checking alignment along the way.

That is correct, and the embarrassing part is that **the chain already exists**:

| Rung | Artifact | State |
|---|---|---|
| Spec | `docs/superpowers/specs/2026-07-12-gonk-stack-design.md` §2 goals, §11 milestones | Exists. Goals are numbered 1-8, milestones 1-6. |
| Plan | `PLAN.md` (index) + `docs/superpowers/plans/*` | Exists. Index carries a per-plan status column. |
| Review | `docs/reviews/2026-09-08-independent-code-review.md` §2.1/§2.2 | Exists. Grades every goal and milestone with evidence. |
| Delivery | `docs/reviews/2026-09-08-delivery-plan.md` §1 | Exists. Ten exit criteria E1-E10, each citing spec ids and closing tasks. |

Four rungs, all present, and the session climbed none of them. So the problem
is not *absence*. It is that nothing makes the chain **trustworthy enough to
read instead of the source**, for three specific reasons:

### 1.1 PLAN.md and the review contradict each other

`PLAN.md` marks plans 01-05 `done`. The review grades the goals those same
plans deliver `Demo`, `Partial`, and in one case `Not done`:

| Goal | PLAN.md implies | Review says |
|---|---|---|
| 1 single chart, one bot user | done (plan 05) | **Demo** -- installed by hand once, no `helm install` in any CI job |
| 2 Renovate-style onboarding | done (plan 02) | **Partial** -- MR half works, post-merge half never has |
| 3 v1 triage + follow-up + scaffold MR | done (plans 02-04) | **Demo** -- comment twice by hand, `@gonk` path silently dead |
| 7 trustworthy by construction | done (plan 01) | **Not done** -- no e2e suite, zero otel in `cmd/ pkg/ internal/` |

Both are defensible under their own definition -- `done` means "the plan's
tasks were executed", `Demo` means "worked by hand once, no automated
evidence" -- but neither document says which question it is answering. A
reader cannot tell, so a careful reader goes to the source. **That is the
whole bug.**

### 1.2 Nothing cites the spec's identifiers

The spec numbers its goals. No test, plan, bead, or CI job references those
numbers. The independent reviewer had to coin "G1-G8" and "M1-M6" to have
vocabulary at all, and those labels exist **only inside the review file** --
`grep -rl "Single Helm chart onto an existing cluster"` returns that file and
nothing else. There is therefore no mechanical way to ask *"which test proves
goal 3?"*

### 1.3 The status table is a snapshot, not a ledger

The review is a point-in-time artifact by an outside reviewer, dated
2026-09-08. Nineteen plan tasks have landed since. Nothing in the task-close
protocol updates it. The delivery plan's own §2.4 already requires each
verifying agent to grade every exit criterion -- and those grades go into a
bead close note and die there.

### 1.4 A live misalignment, found while writing this plan

Spec §11 milestone 4 makes **session resume across pod recreation** a v1 exit
criterion. Delivery-plan criterion E10 says the opposite: session resume is
*formally dropped* from v1 and `continuity` is removed from the schema (T-43).
Both documents are current. Nothing reconciles them. This is exactly the class
of drift the artifact below is meant to make impossible to miss.

## 2. What we are building

Three things, in order.

### 2.1 Stable identifiers in the spec (prerequisite)

Add `G1.`-`G8.` to §2 Goals and `M1.`-`M6.` to §11 milestones in
`docs/superpowers/specs/2026-07-12-gonk-stack-design.md`, matching the
numbering the review already used, so the ids are 1:1 with what is in
circulation. **Renumbering is forbidden** -- these become permanent addresses.
Text edits to a goal are fine; changing which goal an id refers to is not.

### 2.2 `docs/spec-traceability.md` -- the living ledger

One row per spec goal and per validation milestone. Columns:

| Column | Meaning |
|---|---|
| `id` | `G1`-`G8`, `M1`-`M6` |
| `statement` | one-line restatement of the spec text (not a paraphrase that drifts) |
| `exit` | the delivery-plan criteria that close it, e.g. `E2, E3`, or `-` |
| `status` | from the closed vocabulary below |
| `evidence` | named, checkable references -- see §2.3 |
| `graded` | date + commit of the last grading |

**Status vocabulary** (closed set; the gate rejects anything else). Inherited
from the independent review so the two documents can be compared directly,
plus one addition:

- `Done` -- implemented, wired into a real binary or chart path, and tested
  for the property, by a test that runs in CI
- `Partial` -- some of the property holds and is tested; the row must say what
  does not
- `Demo` -- worked by hand at least once; **no automated evidence**
- `Not done`
- `Ornamental` -- parsed or typed, consumed by nothing
- `Withdrawn` -- deliberately removed from v1 scope, with the decision
  referenced (this is what M4 needs, per §1.4)

**Definitions are load-bearing.** `Demo` is not a weak `Done`; it is a
statement that nothing will notice when it breaks. A row may only be `Done` if
its evidence includes a test that a CI job actually runs.

### 2.3 `internal/buildgate/traceability_test.go` -- the anti-rot gate

A hand-maintained table rots; that is what the review became. The table is
only worth building if something checks it. `internal/buildgate` is already a
family of "parse the repo's own config and assert a property" tests
(`ci_build_tags_test.go`, `chart_seal_test.go`, `ci_tag_shape_test.go`), so
this belongs there and runs in plain `go test ./...`.

The gate asserts:

1. **Completeness.** Every `G<n>`/`M<n>` id defined in the spec appears exactly
   once in the table. A goal cannot be silently dropped; adding a goal to the
   spec without grading it goes red.
2. **Vocabulary.** Every `status` is in the closed set.
3. **Claims carry evidence.** Every row whose status is `Done` or `Partial`
   names at least one evidence reference. `Withdrawn` must name the decision.
4. **Named Go tests exist.** Every evidence reference of the form
   `<pkg>.<TestFunc>` resolves to a test function that is actually in the tree.
5. **Named tests run.** For any row claiming `Done`, at least one of its named
   tests must live in a package that some CI job actually executes -- reusing
   the workflow/`.gitlab-ci.yml` parsing that `ci_build_tags_test.go` already
   does. **A `Done` backed by a test nothing runs is the exact failure
   `CLAUDE.md` records** (`test/component` and `test/images` broken for weeks
   with nobody watching).
6. **Exit criteria are covered.** Every criterion `E1`-`E10` in the delivery
   plan appears in at least one row's `exit` column.

Each assertion needs a negative control in the same file, in the house style of
`TestTheTagShapeGateActuallyFails`: mutate the input, assert the gate goes red.
A gate that has never been observed failing is not known to work.

### 2.4 Reconcile PLAN.md

Add one line to `PLAN.md` stating what its status column means ("the plan's
tasks were executed", not "the goal is proven") and pointing at
`docs/spec-traceability.md` for delivery status. The contradiction in §1.1
then stops being a contradiction and becomes two documents answering two
questions.

## 3. Grading discipline

The initial grading is the risky part: it is a judgement call across fourteen
rows, and the default failure mode is generosity (`CLAUDE.md`: *"the author
knows what each line was meant to do, so a line that looks right reads as
working"*).

Therefore:

- **Seed from the review, do not copy it.** The 2026-09-08 grades are the
  starting hypothesis. Nineteen tasks (T-01..T-19) have landed since; each row
  must be re-checked against current `main`.
- **A grade without a named artifact is `Demo` at best.** If the grader cannot
  name a test or a CI job, the status is not `Done`, whatever the code looks
  like.
- **The grader is not the verifier.** Per `CLAUDE.md`, a fresh agent re-grades
  every row against the evidence named, and reports per-row agreement or
  disagreement. Disagreements are resolved by looking, not by averaging.

## 4. Keeping it current

Amend the delivery plan's §2.4 so the verifying agent's per-criterion grades
land in `docs/spec-traceability.md` as well as the bead close note. The gate in
§2.3 then makes a stale claim visible: if a task closes claiming E-something is
met and the table still says otherwise, that is a diff someone must write.

## 5. Out of scope

- Re-grading the R-01..R-51 findings table (the delivery plan owns that).
- Any change to what the goals *are*. This plan makes the existing goals
  addressable and their status checkable; it does not renegotiate v1.
- Fixing the misalignment found in §1.4. The table records M4 as `Withdrawn`
  with a pointer to E10; whether the **spec** should be edited to match is the
  owner's call and is filed separately.
