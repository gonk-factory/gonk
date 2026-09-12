# Triage prompt: make false absence structurally hard (gonk-cl4p)

## The observation

Two live triage runs on project 75 (`agentic/gonk-e2e-1784441480`), same repo, same
prompt shape, same rung (`qwen3-14b`), opposite outcomes:

- **!71 -- correct.** 9 model calls, 62,653 prompt tokens. Named
  `internal/paging/sort.go`, the `SortByDate` case, the always-`<` comparison, and
  the fix. Labels `gonk::bug`, `gonk::needs-fix`, `gonk::verdict-code-change`.
- **!70 -- wrong.** 6 model calls, 38,486 prompt tokens. "I cannot find any
  relevant code files in the repository." All six files, `sort.go` included, were
  present in `/workspace`, verified by exec'ing the live pod before it was reaped.
  Label `gonk::verdict-reply-only`.

Six model calls means five tool round-trips actually ran: opencode and tool calling
are not at fault, and the rig checkout is not at fault. **The model looked, stopped
after five round-trips, and reported ABSENCE rather than uncertainty.**

The ladder defect (a non-answer booked as rung success) is gonk-2xev and is NOT in
scope here. The model is not a lever. **The prompt is the only lever in this bead.**

## What changes

`renderTriagePrompt` in `cmd/gonk-gate/broker_inject.go` only. No change to
`pack/agents/triage/effect-shape.toml`, to the verdict vocabulary, to
`broker_verdict.go`, to `broker_label.go`, to the ladder, or to the model.

### 1. Make false absence structurally hard

The existing prompt already says "LOOK AT WHAT IS ACTUALLY IN THE TREE BEFORE YOU
SEARCH FOR IT. List the directory first" (added for issue !49). It did not hold on
!70. A sentence of advice is not a gate.

Replace it with a **numbered search procedure** and a **precondition on the negative
conclusion**:

- The procedure is ordered and explicit (list top level -> list plausible
  subdirectories -> search for the issue's own nouns and identifiers -> open and
  read the most likely file end to end -> only then decide).
- A comment asserting that the relevant code is absent is only permitted after the
  procedure is complete, and **must name the directories listed and the search terms
  run**. An unsupported negative therefore cannot be written without visibly
  omitting the required evidence.
- A failed glob is named as what it is: evidence the guess was wrong, not evidence
  the code is absent.

**Large-repo question.** "Enumerate the tree" is not scalable and must not be
written as an unconditional instruction: on a monorepo it is either impossible or it
burns the whole context on `ls` output. The instruction is therefore written as
*breadth-first and bounded* -- list the top level, then descend only into
directories that could plausibly hold the behaviour -- and the load-bearing
requirement is not exhaustiveness but **accountability**: a negative conclusion must
state what was searched. That requirement costs the same on six files as on sixty
thousand, and on a large repo it degrades honestly: the comment says "I searched
these four directories for these terms and did not find it", which is a true and
useful statement, rather than "there is no relevant code", which is a false one.

### 2. Separate "I looked and found nothing" from "I could not complete the search"

Today both collapse into a reply-only comment that reads as a confident negative.
The verdict vocabulary is closed (`reply-only` / `code-change` / `close`) and the
classifier is not mine to change, so the distinction is carried **inside the
comment**, as a marker a later classifier can key on:

- If the procedure completed and nothing relevant was found: say so, and list what
  was searched. Normal `reply-only`.
- If the procedure did NOT complete (ran out of turns, a search failed, the tree
  could not be listed): the comment MUST begin with the literal token
  `SEARCH INCOMPLETE:` and MUST NOT assert that the code does not exist, and the
  batch carries `gonk::search-incomplete`.

`search-incomplete` is not in `ReservedLabels` (`broker_label.go`), so it is a legal
agent-proposed label and normalises into the project's configured prefix like any
other. The marker gives gonk-2xev something objective to escalate on without this
bead touching the classifier.

### 3. Reduce run-to-run variance

The numbered procedure in (1) IS the variance fix: 6-vs-9 round-trips on identical
input is what an unordered "go investigate" instruction produces. An explicit order
of operations plus a checklist that must be satisfied before concluding gives the
run a floor it cannot stop above.

### 4. Do NOT induce false positives

This is the failure mode to guard against actively. Every pressure added above is
pressure to **look**, never pressure to **find**. Concretely:

- The prompt states outright that finding no defect is a correct outcome, and that
  "I looked at these files and this behaves as designed" is a welcome answer.
- The `code-change` verdict gets a **citation precondition**: name the file and the
  function/symbol whose behaviour is wrong. A defect that cannot be pointed at is
  not a `code-change`. This is the interlock -- the checklist raises the cost of
  giving up, and the citation requirement raises the cost of inventing.
- No wording anywhere implies a bug exists or that the reporter is probably right.

## Tests

`cmd/gonk-gate/broker_inject_test.go`, asserting the properties claimed, not the
prose:

1. The granted-checkout prompt carries the ordered procedure and the
   "state what you searched" precondition.
2. The `SEARCH INCOMPLETE:` marker and its rule are present, and the marker is
   coupled to "do not assert absence".
3. `code-change` carries the citation precondition.
4. The prompt still states that finding nothing is a legitimate outcome
   (anti-false-positive).
5. The contract survives: the three verdict strings verbatim, the `gonk::` label
   prefix instruction, the `GONK_BATCH_START`/`GONK_BATCH_END` sentinels, one
   comment effect, single-line JSON, "do NOT fetch anything yourself".
6. `search-incomplete` is NOT a reserved label (so proposing it cannot get the whole
   batch refused) -- asserted against `ReservedLabels` itself, not restated.
7. The existing invariants still hold: no checkout claimed when none granted; the
   no-`.agent/` prompt survives verbatim as the suffix of the with-`.agent/` one.

## Validation and its limits

- Prompt rendering is a pure function; the tests above are deterministic and prove
  STRUCTURE.
- `test/stubmodel` is a scripted OpenAI-compatible server: it can prove a prompt
  reaches a model and that a canned reply is parsed, but it cannot prove a real
  model behaves better, because its replies are scripted.
- A live run exercises the DEPLOYED build. This branch is not deployed, so any live
  run made from here tests the CURRENT prompt, not this one. That must be stated as
  such and not implied otherwise.
- Variance is the whole problem: no quality claim is made from a single run.

## Independent review, and what it found (ba1f7c9 -> HEAD)

A fresh agent that did not write the change graded it against this plan. It
confirmed items 1 and the test set, and found seven things wrong. All seven are
fixed in the follow-up commit; they are recorded here because the plan is the
thing later sessions read, and a plan that hides its own review is worthless.

1. **A NEW WAY TO LOSE THE WHOLE RUN, opened by item 1 and not closed by it.**
   `pkg/effects.ValidateComments` refuses an ENTIRE batch when any comment line
   begins with a slash and a letter -- GitLab would execute it as a quick action.
   The obvious way to answer "which directories did you search" is a list of
   absolute paths, one per line, which is exactly that shape. So the change as
   first committed converted the most honest possible report into a silently
   dropped run: the same inversion `searchIncompleteLabel` is kept OUT of
   `ReservedLabels` to avoid, reintroduced two paragraphs later. Fixed with an
   explicit leading-slash warning, and a test that runs
   `effects.ValidateComments` against the hazardous shape so the warning cannot
   outlive its reason.
2. **The incomplete-search path named no verdict**, while `close` sat in the same
   menu -- and a model that has just admitted it could not finish is precisely
   the one that should not reach for a terminal verdict. Now routed explicitly to
   `reply-only`, never `close`.
3. **A dangling reference on the no-checkout path.** The brevity clause said
   "never at the cost of the search evidence required above" on BOTH renderings,
   so the degraded no-repository prompt demanded evidence from a search of a tree
   it does not have -- a small instance of the gonk-msz failure this change set
   out to respect. Reworded to "the evidence this prompt requires of you", which
   is true on both paths, and a test now forbids any search reference in the
   no-checkout rendering.
4. **Three unqualified brevity instructions against one qualification**, in a
   prompt that now asks for more comment content. One ("one short triage
   comment") dropped.
5. **Both war stories ended with the code having been found**, so a small model
   matching on story shape saw two examples of "the last run's mistake was not
   finding it" and none of the opposite -- find-pressure arriving by tone rather
   than by instruction, against goal 4. The mirror-image failure is now narrated
   too, and labelled as having no incident behind it: inventing an anecdote to
   balance the tone would be the same fabrication the paragraph forbids.
6. **Two weak assertions.** `Contains(got, "gonk::")` was tautological -- that
   substring also occurs in the batch template, so deleting the instruction left
   the test green; it now asserts the instruction phrase. And the step checks
   asserted only that `1.` through `5.` appeared SOMEWHERE, which an interleaved
   rewrite would pass, though ordering is the whole of the variance fix; they now
   assert ascending position within the procedure block.
7. **This Progress block had already been ticked** for "independent verification"
   before the verification ran -- the mark-your-own-homework pattern CLAUDE.md
   exists to stop, committed in the same change that quotes it. Rewritten below
   to say what actually happened and when.

It also confirmed, independently rather than on the author's word, that 24 of 34
assertion substrings fail against the old prompt, and that nothing outside the
three intended files was touched.

## Progress

- [x] Plan written (before implementation)
- [x] `renderTriagePrompt` rewritten -- ba1f7c9
- [x] Tests written, and each new assertion checked to FAIL against the old
      prompt rather than merely to pass against the new one
- [x] `make gate` green on ba1f7c9 (`GATE_EXIT=0`, zero FAIL lines)
- [x] Independent verification against this plan by a fresh agent -- ran AFTER
      ba1f7c9, found the seven items above
- [x] All seven review findings fixed, with tests, and each new test checked to
      fail against ba1f7c9
- [x] `make gate` green again after the fixes
- [x] Pushed to `feat/triage-prompt-no-false-absence`
- [ ] **NOT VALIDATED LIVE.** No run of this wording against a real model exists.
      A live triage exercises the DEPLOYED build and this branch is not deployed,
      so a live run from here would test the OLD prompt. `test/stubmodel` cannot
      stand in: its replies are scripted, so it proves delivery and parsing, not
      model behaviour. And one run would prove nothing anyway -- 6-vs-9
      round-trips on identical input is the defect. Whoever deploys this should
      file the same `sort.go` issue on project 75 at least five times and count
      how many runs name the file.
