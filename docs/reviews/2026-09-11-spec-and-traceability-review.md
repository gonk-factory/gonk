# Independent review: the v1 spec and the spec-traceability plan

- **Date:** 2026-09-11
- **Reviewer:** the same independent reviewer identity as
  `docs/reviews/2026-09-08-independent-code-review.md`. I did not write either
  document under review. Where I disagree with my own 09-08 review I say so.
- **Under review:** (A) `docs/superpowers/specs/2026-07-12-gonk-stack-design.md`;
  (B) `docs/plans/2026-09-11-spec-traceability.md`.
- **Method:** read-only. `git log`, `bd list`/`bd show`, `grep`, and reading
  the files named. No tests run, no branches, no edits outside this file.
- **Tree state:** `main` at `b7a22ea`, clean.

## 0. The one-paragraph verdict

Do not build the ledger and the gate described in plan (B) §2.2-§2.3 now.
Recommendation (c): the effort belongs on T-20/T-21 (`gonk-712`, the e2e
proof). Three reasons, each developed below. First, the unit (B) proposes to
trace against -- the spec's eight goals -- is not gradable: four of the eight
bundle three to seven separately-true-or-false propositions, and the delivery
plan's E1-E10 already ARE the gradable unit (§1.3). Second, the gate as
specified checks the ledger's syntax, not its truth; its one assertion with
teeth (5, "named tests run") is either vacuous or a duplicate of
`TestEveryBuildTagRunsInCI` once you read what that test actually parses
(§1.4). Third, on today's tree every row would read `Demo`, `Partial` or
`Not done` (review §2.1/§2.2, re-checked in §3.1 below), and nothing in
T-01..T-18 changes a single goal-level grade; the ledger would faithfully
record for another wave or two what the 09-08 review's executive summary
already says in one sentence. What survives of (B): the id prefixes (§2.1)
are harmless if the namespace collision in §1.5 is fixed, and the one-line
PLAN.md note (§2.4) is correct and costs nothing. Neither needs a gate.

The plan's diagnosis is partly a comfortable one (§1.2). It also mis-states
the one contradiction it found and misses at least five larger ones (§1.1).

## 1. Findings that change what gets built

### P1-1. The spec's internal contradictions are larger than (B) reports, and the one it reports is mis-stated

(B) §1.4 (lines 64-69) says:

> Spec §11 milestone 4 makes **session resume across pod recreation** a v1
> exit criterion. Delivery-plan criterion E10 says the opposite: session
> resume is *formally dropped* from v1 and `continuity` is removed from the
> schema (T-43). Both documents are current. Nothing reconciles them.

Four corrections, then what it missed.

1. **It is not "the opposite".** Spec §11.4 (lines 495-497) reads in full:
   "Session resume across pod recreation ... -- named risk; if broken, file
   upstream PR or fall back to `continuity: fresh` (correctness preserved via
   layered continuity)." §4.4.2 (lines 143-145) says the same. The spec
   already names dropping resume as an acceptable outcome of M4. E10 takes
   the spec's own escape hatch. What E10 actually contradicts is §4.2 line
   111 ("Per-project `continuity: resume | fresh`") -- E10 removes the knob
   rather than defaulting it. That is a real but much narrower disagreement.
2. **E10 is a planned criterion, not a current state.** `continuity` is
   still in the schema today: `pkg/gonkcfg/gonk-config.v1.schema.json:48`,
   `docs/schemas/gonk-config.v1.schema.json:48`,
   `pkg/opercfg/gonk-operator.v1.schema.json:102`, consumed by
   `pkg/gonkcfg/config.go:15`, `pkg/meterapi/meterapi.go:324`,
   `internal/meter/store/postgres_wire.go:43`, rendered into every
   onboarding MR by `pkg/intake/render.go:65,159`, and hard-coded to `resume`
   by `pack/pack.toml:76`. T-43 (`gonk-t43`) is open. The tree still agrees
   with the spec; the plan disagrees with both.
3. **Something does reconcile them.** T-43's text (delivery plan lines
   1491-1501) is: "`continuity` removed from schema v1 ...; **spec §4.2 and
   §11.4 amended**." The reconciliation is already filed; it has not run.
   (B) §5 (lines 184-186) says "whether the **spec** should be edited to
   match is the owner's call and is filed separately" -- it is not
   separate; it is T-43, and T-43 already decides it.
4. My 09-08 review already graded `continuity` `Ornamental` (review line
   116, G6 row). This was not newly "found while writing this plan".

**Contradictions (B) did not report**, each verified against the tree, in
order of size:

| # | Spec says | Tree / decision record | Evidence |
|---|---|---|---|
| C1 | §1 line 17: "gonk is **a Gas City pack plus glue**, not a new orchestrator." §4.1 lines 88-96 component table: gascity controller, dolt bead store, intake as pack `[[service]]`, opencode pods created by the k8s session provider. | ADR-007 §1 (lines 27-55): "That was true when written and is now false ... Amend the spec's §1 philosophy line rather than leaving it to mislead." `cmd/gonk-gate` -- the binary that "is the only code path that pours work" -- does not appear in §4.1 at all. `pack/pack.toml:49-53` explicitly has no `[[service]]`. T-45/T-56 (open) replace Gas City with Jobs and "delete the Gas City surface". | `docs/adr/ADR-007-...md:27-55`; `ls cmd` = gonk-gate, gonk-intake, gonk-meter, gonk-stubmodel; `pack/pack.toml:49` |
| C2 | §2 goal 3 lines 31-34: "... and the post-merge `.agent/` scaffold MR (the first metered work; triage cannot run while a project is `pending`, so the scaffold MR is in v1 scope)." | §5.3 lines 242-252 (rewritten 2026-09-09, `acb186f`): "Triage no longer waits ... The metered scaffold is opt-in and **out of v1**." Goal 3's parenthetical justification is now false by the spec's own §5.3. `git log -L31,34` shows goal 3 last touched `7cfd084` on 2026-07-12. | spec lines 31-34 vs 242-252; `git log -L` |
| C3 | Line 6: "Intended license: MIT". §7.1 line 346: "(open source, MIT)". | `LICENSE` is Apache License 2.0. | `head -3 LICENSE` |
| C4 | §7.1 lines 348-360: `intake/`, `meter/`, `test/` kind e2e harness. §7.2/§7.3: `gonk-navigator` and `gonk-city` repos. §3 line 81: "Repo layout: `gonk` monorepo + `gonk-navigator` repo + private `gonk-city`". | Layout is `cmd/ pkg/ internal/` (ADR-001). No `gonk-city` repo exists (`docs/environment.md:114-118`). `test/e2e/` contains one markdown file and zero Go files. The only `gonk-navigator`/`gonk-city` strings in the tree are in `cmd/gonk-gate/*_test.go` fixtures. | `ls`, `ls test/e2e`, grep |
| C5 | §8 line 402: "OTLP traces stitched webhook -> order -> session attempt -> LiteLLM calls." §7.4: OTLP endpoint value. | Zero otel code: `grep -rli "otel\|opentelemetry" cmd pkg internal` returns nothing; `go.mod` has no otel module. T-44 (open) plans to remove `monitoring.otlpEndpoint` and file otel as v1.5. | grep output |
| C6 | §4.1 line 91: dolt is "Tier-0 stateful backbone: PDB, probes, scheduled `dolt push` backups". | `chart/gonk/templates/` contains no PodDisruptionBudget, CronJob, or backup template (`ls ... | grep -iE "pdb|disruption|cron|backup"` is empty). Review §7 already lists "ledger has no PDB/backup". | `ls chart/gonk/templates` |
| C7 | §5.4 line 272 `.gonk.yml` contract still shows `provenance: { commit_trailers: true, ... }`. | §6.1 lines 297-311 (same document) says trailers are "NOT IN V1" and the code was deleted (T-10, `d3ca60e`). The schema keeps the key but documents it as "Accepted and validated, but ignored" (`pkg/gonkcfg/gonk-config.v1.schema.json:57-68`). The policy contract advertises a knob the same spec says does nothing. | spec 272 vs 297-311; schema 57-68 |
| C8 | §10.1 line 472: JSON Schemas for `.gonk.yml`, **attribution tags, order names**. | `docs/schemas/` holds `gonk-config.v1` and `gonk-operator.v1` only. | `ls docs/schemas` |
| C9 | §5.5 lines 282-286: `@<bot>` mentions route to the thread's worker session. §2 goal 3: "conversational follow-up in-thread". | `gonk-ecn` (open, P2): "The mention trigger is silently dead". `internal/packtest/pack_test.go:444` skips the mention-reply case with "T-24 ports it onto the broker". T-24 open. | `bd show gonk-ecn`; pack_test.go:444 |
| C10 | §9 lines 460-462: agent NetworkPolicy "allows egress only to GitLab and LiteLLM ... budgets cannot be bypassed." | `gonk-dku` (open): cluster runs flannel, no policy controller, so nothing is enforced. `gonk-7oz` (open): the agent policy allows any port to the whole gitlab namespace. The spec states as a property what the tree documents as unenforced. | `bd list` |

C1 is the one that matters for a traceability chain: **the spec's root
sentence has been formally declared false by an accepted ADR, and the spec
was not amended.** A ledger keyed on this spec's ids inherits that. T-50
(open, wave 9) owns fixing it. Plan (B) §5 says "Any change to what the
goals *are*" is out of scope -- but with C1 and C2 standing, the goals as
written are not the goals the project is pursuing, and grading them
faithfully produces grades nobody wants.

### P1-2. The diagnosis is partly a comfortable one

(B) §1 lines 15-26 concedes "Four rungs, all present, and the session climbed
none of them", then concludes the problem is that "nothing makes the chain
**trustworthy enough to read instead of the source**". That is the
diagnosis that blames the artifacts. The uncomfortable one:

- The 09-08 review's executive summary, line 23-28, answers the owner's
  question in one sentence: "**v1 has been demonstrated, never delivered** ...
  Nothing automated exercises the flow end to end." Three days old. Not
  read.
- `docs/HANDOFF-next-session.md` says "**Read the top section and stop**"
  (line 7) and its top section is dated 2026-08-19 (line 3) with a START
  HERE pointing at `gonk-ob5`, which closed 08-30 (`bd show gonk-ob5`:
  CLOSED). Review §7 (lines 627-650) already lists this. T-51 (open)
  requires HANDOFF "dated within a week with an open START HERE bead".

A fifth document would have been exactly as unread as the fourth. What
would have shortened the archaeology is a **current** pointer at the
**existing** status table -- T-51 -- not a new table. (B) §1.3's point
that "nothing in the task-close protocol updates" the review is fair, but
the delivery plan §2.4 (lines 150-170) already requires per-criterion
grades in every close note; (B) §4's amendment (also write them to the
ledger) is the only new idea, and it can be applied to delivery plan §1
itself (see §1.3).

### P1-3. The goals are not the right unit; E1-E10 already are

(B) §2.2 makes one row per goal. Test: can "is G4 done" be answered so that
two people agree?

| Goal | Distinct propositions bundled | Gradable as one row? |
|---|---|---|
| G1 (27-28) | single Helm chart; one bot user | Yes. E1 covers the install; the bot-user half has no criterion. |
| G2 (29-30) | invite yields MR; merging enables | Yes. E2. |
| G3 (31-34) | labels; analysis comments; clarifying questions; in-thread follow-up; onboarding MR; **scaffold MR (now out of v1 per §5.3, C2)** | **No.** Today: triage comment+labels = Demo (E3 open); follow-up = Not done (`gonk-ecn`, E5/T-24); scaffold = Withdrawn. Three statuses, one cell. |
| G4 (35-37) | attribution at 7 granularities (turn, comment-or-commit, bead, branch, project, group, instance); budgets at 3 tiers; hard enforcement | **No.** Bead/project = built and tested; per-turn, per-comment, per-branch = no code (review §3.3 "ledger join not built"); group/instance = soft, `gonk-ay87` open; hard door = R-14 open. E4 covers two of the seven granularities and one of the three tiers. |
| G5 (38-39) | local-first; deterministic ladder; defer semantics | **No, but splittable.** Ladder = Done (`pkg/rung`); defer = E4; "local-first" enforced only by NetworkPolicy = E8, and `gonk-dku` says it is not enforced. |
| G6 (40-41) | project state in project | Yes. Done, with `continuity` ornamental (review G6). No E criterion covers it. |
| G7 (42-44) | deterministic e2e with stub; contract-tested schemas; full observability via Prom/Grafana/Loki/otel; append-only audit | **No.** e2e = E3; schemas = partial (C8); otel = none (C5); audit via "gc event bus and bead history" = being deleted by T-56. |
| G8 (45-46) | no deployment-specific material | Yes. E9. |

Four of eight fail. The milestones M1-M6 are each a single testable
proposition, and the delivery plan already mapped them: M1=E1, M2=E2,
M3=E3, M4=E10, M5~E4, M6=E7. Note M5 is only *partly* E4: M5 says "visible
in **Grafana**" (spec line 498); E4 asserts `/cost/bead` and `/cost/project`
(delivery plan line 62). No E criterion asserts a dashboard shows anything.
So E1-E10 is the better unit but is itself incomplete on the Grafana half
of M5, the bot-user half of G1, and all of G6.

Consequence for (B) §2.3 assertion 6 ("every E1-E10 appears in at least
one row's `exit` column"): it checks the wrong direction. It is trivially
satisfied by listing every E once, and it says nothing about whether the
row's E is met. If a ledger is ever built, key it on E-criteria (plus the
three uncovered halves above), and let the E rows cite the G/M ids -- not
the reverse.

### P1-4. Assertion 5 is wishful, and the hole it cannot see is the real one

(B) §2.3 item 5 (lines 133-137):

> For any row claiming `Done`, at least one of its named tests must live in
> a package that some CI job actually executes -- reusing the
> workflow/`.gitlab-ci.yml` parsing that `ci_build_tags_test.go` already
> does.

Checked against the file:

- `ci_build_tags_test.go` reads **only** `.github/workflows/*.yml`
  (`workflowText`, lines 262-289: `filepath.Join(root, ".github",
  "workflows")`). It never opens `.gitlab-ci.yml`. The GitLab pipeline is
  parsed by `ciConfig` in `ci_autocancel_test.go:32-43`, a different file
  with a different data model (a YAML map, not concatenated text).
- What it extracts is **`-tags` flag arguments**, not package paths
  (`taggedInvocations`, lines 233-251, regex `tagsFlagRe`). There is no
  code that answers "does job X run package Y". That would need to be
  written from scratch and would have to understand `./...`, `./pkg/foo/...`,
  `-run` filters, and the `hack/require_nonzero_go_tests.sh` wrapper's
  argument passthrough.
- Both pipelines run an **untagged `go test ./...`** on every push
  (`.github/workflows/ci.yml:92-93`; `.gitlab-ci.yml` job `test`, script
  line `go test ./... -race -count=1`). So for any untagged test the
  assertion is **vacuous** -- every untagged package is run. For any tagged
  test, `TestEveryBuildTagRunsInCI` already asserts the tag is on a real
  `go test` line. Assertion 5 therefore adds nothing that is not already
  either automatic or already gated.

The failure it claims to close -- "a `Done` backed by a test nothing runs"
-- is not the failure this repo actually has today. The failure it has is
**a test that runs, passes, and proves nothing**:

- `internal/packtest/pack_test.go:444`: unconditional `t.Skip` on the
  mention-reply case. Runs in `go test ./...`, green, and the property
  (G3's follow-up) is not tested.
- `test/images/agent_smoke_test.go:320-324`: the provider-allowlist
  assertion skips itself when its control finds no foreign providers --
  precisely the `gonk-ob5` fail-open property, which would then be reported
  green by a job that ran it.
- `test/live/pricing_test.go:202-204`: `TestEveryProjectKeyIsScopedAndBudgeted`
  skips when the proxy has no keys. A nightly run against an empty proxy is
  green.

`test/harness/requireinfra.go:28-39` correctly makes *infra* skips fatal
under `CI=true`; none of the three above go through it. Assertion 5 cannot
see any of them, because "the package is executed" is true for all three.

**Red for unrelated reasons.** Assertion 5 would go red if a CI line is
refactored to `make test` (the invocation regex recognises only `go test`
and `require_nonzero_go_tests.sh`, line 224-228), or moved into a matrix
`strategy`, or if the GitLab `chart-test` job -- gated on
`rules: if: $GONK_CHART_TOOLS_IMAGE` -- is counted as "a CI job" when it
never runs. The plan does not say which of the two pipelines counts.

### P1-5. `G<n>` is already an identifier namespace in this repo

(B) §2.1 makes `G1`-`G8` "permanent addresses". The chart already uses
`G1`-`G20` as the ids of its fail-closed render guards: `chart/gonk/README.md:8`
("`_guards.tpl`, G1-G20"), `chart/gonk/values.yaml:512-534` ("Empty -> fail
(G1)", "(G2)"), `chart/gonk/README.md:281,325,328`, `PLAN.md:779,866`. A
ledger cell reading "G4" and a values comment reading "G4 fails it" refer to
different things. Assertion 1's "every `G<n>` id defined in the spec" is
mechanically fine if scoped to the spec file, but every *human* reader of
the ledger, the chart README and PLAN.md now has to disambiguate. Use a
prefix that is not taken (`SG1`, `V1-G1`, or the section number `2.3`).

## 2. Should be fixed

### P2-1. (B) §1.1 is too kind to PLAN.md and mischaracterises the review

(B) lines 40-44:

> Both are defensible under their own definition -- `done` means "the plan's
> tasks were executed", `Demo` means "worked by hand once, no automated
> evidence" -- but neither document says which question it is answering.

Three problems.

1. **PLAN.md is not defensible under its own definition, because it
   contradicts itself.** Its table (line 11) says plan 05 is `done`; its
   body, line 766, is headed "### Plan 05 (chart) -- STILL TODO (Plan 05
   has not started)". Same for plan 06 (line 12 vs 792). Review §2.3 (lines
   133-142) and §7 (line 636) already recorded this. The two documents do
   not "answer different questions"; one of them has a stale table.
2. **The plan-to-goal mapping in (B)'s table is invented.** Row "7
   trustworthy by construction | done (plan 01)": plan 01's stated goal
   (`docs/superpowers/plans/2026-07-12-plan-01-...md:5`) is "the `.gonk.yml`
   config library ... and the attribution-tags contract". Plan 01 never
   claimed the e2e suite or otel. Marking plan 01 `done` and grading G7
   `Not done` is not a contradiction; it is two true statements about
   different things. The same holds for "1 single chart | done (plan 05)":
   plan 05 is a chart that renders; G1 is a chart that installs on kind in
   CI. Only the G2/plan-02 and G3/plan-04 rows are genuine tensions, and
   review §2.3 adjudicated both ("Done, with the defects in §3.2";
   "Formula half is a corpse").
3. **The review DID say which question it answers**: line 104-108 defines
   its vocabulary before the table. PLAN.md, line 14, also says what its
   column is ("Update the Status column as tasks complete"). So (B) §2.4's
   proposed one-liner is correct but the claim that "neither document says"
   is not.

### P2-2. Counts and references in (B) are unverified

- Line 58 and 162: "Nineteen plan tasks have landed since" / "Nineteen
  tasks (T-01..T-19) have landed". `bd list --status=closed` shows
  `gonk-t01`..`gonk-t18` **plus** `t26, t34, t37, t38, t48` closed (23
  T-beads), and `gonk-t19` **open** (commit `56deae9`: "T-19 (partial)").
  Wrong in both directions. `git log --since=2026-09-08 | wc -l` = 122
  commits.
- Line 3: "Bead gonk-trace (to be created)". `gonk-llj0` already exists
  with this plan's path in its description.
- Line 119 cites `chart_seal_test.go` as a buildgate model; it exists, but
  the plan's own assertion 5 cites the wrong file for the parsing it wants
  (P1-4).

None of these is serious alone. Together they are the pattern CLAUDE.md
records: a document about verification whose own numbers were not verified.

### P2-3. The status vocabulary cannot express four real states

The six values (lines 100-108) are: Done, Partial, Demo, Not done,
Ornamental, Withdrawn.

- **Mixed.** G3 today is Demo + Not done + Withdrawn (P1-3). G6 needed a
  compound in my own review ("Done (with an ornamental field)", line 116).
  `Partial` absorbs everything, which makes it the status that carries no
  information -- the `close_reason: Closed` problem again (delivery plan
  line 165-170).
- **Withdrawal pending.** M4 today is not `Withdrawn`; T-43 is open and
  depends on T-45, which is open. It is "Not done, withdrawal proposed by
  an unaccepted decision". (B) §2.2 says "this is what M4 needs" -- marking
  it Withdrawn today cites a decision that does not exist yet
  (`docs/adr/ADR-008-session-runtime.md` is absent; `ls docs/adr` ends at
  007).
- **Superseded / redefined.** Spec §1 (C1) and G3's parenthetical (C2) are
  not withdrawn; the goal text is wrong and the intended goal survives.
  There is no value for "the spec text no longer states the goal".
- **Regressed.** A row that was Done at `graded` commit X and whose
  evidence test was gutted or whose CI job was made conditional after X.
  No value, and no assertion detects it (P2-5).
- **Ambiguity in `Done`.** Line 100-101: "by a test that runs in CI". E6
  says "runs on every push"; `live.yml` is nightly (`.github/workflows/live.yml:31-34`).
  Is a nightly-only test `Done`? The plan does not say. The GitLab
  `chart-test` job is conditional. Which pipeline counts? Not said.

### P2-4. The grading discipline is necessary and not sufficient

(B) §3's three rules are good. Missing, given this project's own standard:

1. **The plan's author must not grade.** CLAUDE.md "Implementing a Plan":
   "have a DIFFERENT agent implement each part". §3 says "the grader is
   not the verifier" but does not exclude the plan's author from being the
   grader. It should.
2. **`Done` requires the named test to have been observed failing for the
   property.** Every buildgate gate carries a negative control
   (`TestTheTagShapeGateActuallyFails`, `TestBuildTagKindDecidesWhatCIOwes`).
   A ledger `Done` should demand the same: name the negative control or
   the commit in which the test was red before the fix. Without it, the
   three skip-green tests in P1-4 qualify as evidence.
3. **Proxy evidence is not evidence.** My 09-08 M5 row cited "Dashboard
   JSON validated" -- that is a proxy for "attribution visible in Grafana".
   CLAUDE.md: "Verification means observing the thing, not the proxy for
   it." The rule should be written into §3 explicitly, with M5 as the
   worked example, because it is the row where I myself was generous.
4. **A staleness rule.** The `graded` column carries a commit. Nothing in
   §2.3 uses it. The one anti-rot assertion that would earn its keep:
   "if any file under a row's evidence paths changed after the `graded`
   commit, the row must be re-graded (its `graded` commit must be at or
   after the last change)". That is mechanical (`git log -1 --format=%H
   -- <paths>` vs the recorded commit) and it is the only assertion in the
   whole design that would catch regression. It is absent.

### P2-5. What the gate does NOT catch (all green)

Concrete, on the design as written:

1. Evidence test renamed -> red (good). Evidence test body replaced with
   `t.Log("ok")` -> green.
2. Evidence test exists, runs, tests an unrelated property -> green.
   `G7 | Done | buildgate.TestEveryBuildTagRunsInCI` passes assertions
   1-6.
3. Spec goal text edited so the goal means something else, id kept ->
   green. Assertion 1 checks ids, not text. The `statement` column (line
   90) is meant to be "not a paraphrase that drifts", but no assertion
   compares it to the spec.
4. `Withdrawn` naming `ADR-008` before ADR-008 exists -> green unless
   assertion 3 checks file existence, which it does not say.
5. `Done` row, CI job later made `continue-on-error`, `allow_failure`, or
   `rules: if $VAR` -> green.
6. Row graded at commit X, code regressed at X+n -> green (P2-4.4).
7. Under-claiming rot: `Not done` and `Demo` rows require no evidence
   (assertion 3 applies to Done/Partial only). T-21 lands; nobody edits
   the row; the ledger says "nothing proven" while the CI job proves it.
   The gate is asymmetric: it polices optimism and ignores pessimism, and
   a pessimistic ledger nobody trusts is the 09-08 review all over again.
8. E listed in the wrong row's `exit` column -> green.
9. Test runs on GitHub only; the GitLab pipeline -- the one that builds
   and deploys images -- does not run it. "Some CI job" is satisfied.

Items 1, 2 and 3 are unfixable by any mechanical gate; they are what the
human grading in §3 is for. Items 4-9 are fixable and are not in the
plan.

## 3. Worth knowing

### 3.1 The spec, section by section, against today's tree

Everything below is verified against the tree, not against (B)'s or the
review's claims about it. "Amended" means the spec text was changed in
place after 07-12 (spec `git log`: `1a2bc05`, `511f3cb` 09-06 for §8.1;
`6dbe8a8` 09-07 for §5.2; `acb186f` 09-09 for §5.3; `d3ca60e` 09-09 for
§6.1).

| Section | State | Evidence |
|---|---|---|
| §1 philosophy | **Invalidated**, not amended | ADR-007 §1 (C1) |
| §2 goals 1-8 | Unchanged since 07-12; G3 parenthetical now false | `git log -L31,34`; C2 |
| §2 non-goals | Still true | -- |
| §3 decisions table | "Architecture: Pack-first" invalidated by ADR-007; "Agent runtime: opencode only" still true; "Escalation: no LLM judges" still true (`pkg/rung`); "Repo layout" (three repos) false | C1, C4 |
| §4.1 components | gonk-gate missing; intake not a pack service; Gas City itself scheduled for deletion (T-45/T-56 open) | C1; `pack/pack.toml:49` |
| §4.2 scale-to-zero | Was violated by stale formula beads (`gonk-p2e`, now CLOSED via T-09); not re-verified after | `bd show gonk-p2e` |
| §4.2 continuity | Ornamental; removal planned (T-43 open) | P1-1 |
| §4.2 gonk-city repo | Does not exist | `docs/environment.md:114-118` |
| §4.3 data flow | Steps 3-4 (order slings formula, k8s provider spawns) are the deleted formula path; step 7 (follow-up wakes worker) is dead (`gonk-ecn`) | T-09 `fc7d619`; C9 |
| §4.4.1 webhook verification | Resolved: handled in intake (`pkg/ghook`) | PLAN.md contracts plan 02 |
| §4.4.2 resume | Being resolved by fallback (E10/T-43) | P1-1 |
| §5.1-§5.2 | Amended 09-07, honest about what is not reconciled (`gonk-pjoo` open) | spec 206-212 |
| §5.3 | Amended 09-09 (T-08); contradicts §2 G3 | C2 |
| §5.4 contract | `provenance` advertised, ignored (C7); `continuity` advertised, ornamental | schema :48, :57-68 |
| §5.5 mentions | Dead; T-24 open | C9 |
| §6.1 trailers | Amended 09-09: NOT IN V1 | spec 297-311 |
| §6.1 tags | `pkg/atags` exists; no published schema (C8) | `ls docs/schemas` |
| §6.2.1 ledger join | Not built per review §3.3; `spend_rows` on Postgres | review 246-296 |
| §6.2.2 hard door | Hole R-14; `gonk-bvy` race; T-30 open | `bd show gonk-bvy` |
| §6.2.3 defer | Built (`pkg/rung/decide.go:30-37`), exhaustively tested; E4 CI proof pending (T-31) | grep |
| §6.3 ladder | Built; the "session never started" case (`gonk-alw`) is not among the listed infra signals | §3.2 below |
| §7.1-§7.3 repo map | False (C4) | -- |
| §7.4 chart seams | Largely true; 8 ci profiles exist (`chart/gonk/ci/`); `litellm.enabled: false` path is the deployed one | `ls chart/gonk/ci` |
| §8 metrics/dashboards | Three dashboard ConfigMaps and ServiceMonitors for intake+meter exist in templates; none for gonk-gate (T-44) | `ls chart/gonk/templates` |
| §8 OTLP | Nothing (C5) | grep |
| §8 audit via gc event bus | Being deleted with Gas City (T-56) | delivery plan 837 |
| §8.1 lifecycle log | Amended 09-06; enforced by `test/entrypoint` | spec 405-456 |
| §9 NetworkPolicy | Templates exist; not enforced (`gonk-dku`); too permissive (`gonk-7oz`) | C10 |
| §9 bot writes limited | True by construction of the broker (ADR-007) | -- |
| §10.1 contract tests | Config + operator only (C8) | -- |
| §10.2 unit | True | review §3 |
| §10.3 component | Runs on GitHub since T-15/T-16; GitLab `chart-test` conditional on `$GONK_CHART_TOOLS_IMAGE` | ci.yml 160-161, 205, 242, 315; `.gitlab-ci.yml` chart-test |
| §10.4 e2e every merge | Does not exist (`test/e2e/` = one .md) | `ls test/e2e` |
| §10.5 live nightly | Exists (`live.yml`), but for pricing drift, not the scenario | live.yml:95 |
| §11 M1-M6 | All still open; see review §2.2, unchanged by T-01..T-18 | `bd list` open: t20, t21, t31, t42, t43 |
| §12.1 packs licensing | Overtaken: with T-45/T-56 the pack layer goes; also spec license is now Apache (C3) | -- |
| §12.2 CE assumption | Still true, unchanged | -- |
| §12.3 resume | = M4 | -- |
| §12.4 `[[service]]` shape | **Resolved** in practice, never closed in the spec: `pack/pack.toml:49` "WHY THERE IS NO [[service]] HERE" | pack.toml |
| §12.5 capacity contention | Observed, but as node I/O not GPU (`gonk-cw3` open: orac04 I/O saturation delays session start) | `bd list` |
| §12.6 payload drift | T-22 open | -- |
| §13 roadmap | v1.5 "scaffold MR quality pass" now consistent with §5.3 | -- |

Net: of six §12 risks, one is resolved in the tree and not in the spec
(12.4), one is overtaken (12.1), one is being resolved by its own fallback
(12.3), one is being observed in a different form than predicted (12.5),
and two remain open as stated (12.2, 12.6).

### 3.2 What a v1 spec for this system should state and does not

Derived from the two open P0s, which are the two failures the system has
actually had in production.

From `gonk-alw` (cold controller accepts a session and never creates the
pod):

- **Session-create outcome semantics.** The spec never says what a session
  provider must return. The bead's surviving requirement is exact: the
  runtime "must distinguish confirmed / failed / UNKNOWN, and UNKNOWN with
  no agent ever started must park rather than consume an attempt". §6.3's
  infra list ("connection errors, LiteLLM 5xx, pod evictions") should name
  "session never started" and "outcome unknown" explicitly, and say they
  never escalate.
- **A readiness contract for every control-plane component.** `gonk-uxup`
  (open): all controller probes are tcpSocket. The spec has no line on what
  "Ready" must mean for a component that accepts work.

From `gonk-bvy` (two meters race, project wedges permanently):

- **Multiplicity and idempotency of the meter.** §6.2.2 says "meter
  provisions per-project virtual keys" and nothing about what happens when
  two meters run (which an ordinary node failure produces). The v4 plan
  settles credential identity as a stored hash and team-scoped keys; the
  spec should state the invariant (at most one live key per project;
  ensure-key is idempotent under concurrency) rather than leave it in a
  plan file.
- **No terminal parked state without an operator-visible condition.** The
  bead's phrase: "A fail-closed state with no recovery path is an outage
  with extra steps." The spec is full of fail-closed rules (§5.1, §6.2,
  §9) and has no rule that every fail-closed outcome must be
  distinguishable from "still provisioning" and must have a named exit.
- **Budget tiers as LiteLLM constructs.** §5.4 precedence ("ceilings only
  tighten downward") is stated as config semantics; the tree enforces only
  the project tier in LiteLLM (`gonk-ay87`). The spec should say which
  tiers are hard (LiteLLM) and which are soft (meter), because today the
  reader assumes all three are hard.

Cross-cutting, from the rest of the open list:

- **Fail-open is a named defect class.** `gonk-ob5` (opencode falls open to
  a cloud provider) and `gonk-dku` (policy not enforced) both defeated a
  spec property silently. The spec should require that every
  fail-closed property have a runtime check that reports enforcement
  (the chart's netpol probe is the model, `test-netpol-probe.yaml`).
- **Identity injectivity.** `gonk-uom2` (open): `RigName` is not injective,
  so two projects can share a key and a budget. The spec keys everything on
  "project" without defining the identifier.
- **Two CI systems.** `.github/workflows/` and `.gitlab-ci.yml` both exist,
  run different job sets, and the spec's §10 knows about neither. Which is
  the gate of record matters for any "runs in CI" claim (P1-4).

### 3.3 On (B) §2.3 assertions 1-4 and 6, briefly

- 1 (completeness): mechanical if the spec is formatted exactly as the
  parser expects. Red for unrelated reasons whenever the spec's list
  formatting changes. Fix P1-5 first.
- 2 (vocabulary): mechanical, trivial, fine.
- 3 (claims carry evidence): "names at least one evidence reference" is a
  non-empty-string check unless evidence has a grammar. The plan gives a
  grammar for Go tests only; everything else (chart template, ADR, bead,
  CI job) is free text and un-checkable.
- 4 (named Go tests exist): mechanical via `go/parser`; the only assertion
  of the six that catches something real (a renamed or deleted test).
  Worth roughly one hour. It is also the only one the staleness rule in
  P2-4.4 could be bolted onto.
- 6 (E coverage): wrong direction (P1-3).

### 3.4 (B) §2.4 and §2.1

- §2.4 (one line in PLAN.md saying what `done` means) is correct and
  should be done regardless -- but PLAN.md line 14 already half-says it,
  and lines 766/792 need fixing more (P2-1). T-51 owns this.
- §2.1 (ids in the spec) is fine once renamed (P1-5), is a 10-minute
  edit, and helps T-50. It does not need a gate to be useful.

## 4. The recommendation, argued

**(c). Do not build §2.2-§2.3 now. Spend the effort on T-20/T-21.**

Against (a): P1-3, P1-4, P1-5 and P2-5 together mean the gate as designed
would be green on a ledger that is materially wrong in every way this
project has actually been wrong, and red on refactors that change
nothing. A gate that cannot be trusted red or green is worse than no gate,
because it will be believed.

Against (b): the "smaller thing" that gets most of the value already
exists. Delivery plan §1 is a ten-row table with `criterion`, `spec`,
`closed by`. Add a `status` and `graded` column to it, fill them at each
wave's §2.4 verification, and you have the ledger -- keyed on the right
unit, inside the document that already defines "done", with no new file
and no new gate. That is a 30-minute amendment to an existing artifact,
not a smaller version of (B); it is (B)'s §4 applied to the table (B) §1
already lists as rung four. I am not offering it as a middle option; I am
saying (B) §2.2 duplicates something that exists.

For (c): on today's tree every M row is open and every G row is at best
Partial (§3.1). T-01..T-18 moved zero goal-level grades; they were Phase A
bleeding-stops and Phase B suite-wiring, by design (delivery plan §0.1).
The first task that changes a grade is T-20 (E1), then T-21 (E2, E3, E4).
Until they land, a ledger records "nothing is proven end to end" fourteen
times, and the executive summary of the 09-08 review says it once. After
they land, the delivery plan §1 table with a status column says it in the
right place. The archaeology that prompted (B) is fixed by T-51 (a current
HANDOFF and README), which is filed, sized S, and would have prevented
this session's twenty minutes.

## 5. What I did not check

- I did not run any test, `make gate`, or `helm`. Every "runs in CI"
  statement is from reading workflow files, not from a CI run.
- I did not read the bodies of `docs/superpowers/plans/*` beyond plan 01's
  goal line and PLAN.md's index; my P2-1 point about plan-to-goal mapping
  rests on plan 01 and the PLAN.md table only.
- I did not verify the live cluster: `gonk-dku`, `gonk-7oz`, `gonk-uxup`,
  `gonk-cw3` are cited as open beads, not re-observed.
- I did not re-grade every row of review §2.1/§2.2 against the tree; I
  re-checked the rows my findings depend on (G3, G4, G6, G7, M4, M5) and
  the evidence cells I quote. The other rows are cited as the review's
  claims.
- I did not read the Gas City source or ADR-007 beyond §1 and the
  headings; C1 rests on ADR-007's own words about the spec.
- I did not check whether `chart_seal_test.go` would be a better parsing
  model for assertion 5 than the file (B) names; I checked only that the
  file (B) names does not do what (B) says.
- The `gonk-llj0` bead shows `Created: 2026-09-12` on a 09-11 review; I
  assume a timezone artifact and did not investigate.
