# Review: the 2026-09-12 sequencing decision (branch `docs/sequencing-2026-09-12`, commit `0c371c5`)

Independent, read-only review. Nothing was edited, applied, committed or gated.
Every claim below carries the command or file:line it came from; anything I
could not verify is labelled as such.

## Verdict

**Land with named changes. Not as written.**

The planning direction (gates and the verifier substrate ahead of the
implementation agent) is sound and is in fact what the 2026-08-02 design
document already said. But the amendment mis-describes what it is doing in
three load-bearing places, and each mis-description would be believed by the
next reader:

1. It is a partial phase reversal, and §2 / §11d say it is not.
2. The trajectory-verifier rationale is attached to the wrong beads and the
   "existing seam" cannot carry the weight put on it.
3. The six-hop chain is undercounted, is partly prose rather than graph, and
   the amendment's own reading (a) of the owner's instruction is impossible
   under the graph the amendment relies on.

And the honest answer to "is the stated order achievable": **no, not as the
graph stands.** `gonk-3so` proper is behind `gonk-t45` through `gonk-t53`
(nine beads, not six) unless `gonk-4v8` is split or the `t53 -> 4v8` link is
declared to be prose. The amendment says this is the owner's call and then
does not give the owner the numbers to make it.

The changes are listed in §8. Everything else is evidence.

---

## 1. Does the amendment faithfully record the owner's decision?

Mostly yes; two over-interpretations, one of which is encoded into the epic.

**Faithful.** `2xev` first; `xsk3` and `7s9p` "tested and done"; `7s9p` P2 -> P1
(the owner called both "these P1s", so the promotion is the owner's word, not the
session's); `t24` to P2 (owner said "P2/P3"); the gates-then-verifier-then-3so
order. All present in
`docs/plans/2026-09-12-sequencing-decision.md` §1 and match the verbatim
instruction.

**Over-interpretation 1 -- `t24`.** The amendment paraphrases the owner's
"we're on the implementation path for that as soon as we land outstanding work
and address these P1s" as (§3.2) *"mentions arrive with the implementation
work"*, and uses that paraphrase to justify the edge cut. The owner's sentence
has an equally natural reading: *t24 is next after this P1 batch, so it does not
need to be P1 to get done.* Under that reading t24 is not deferred behind 3so at
all, and the argument for cutting `t53 -> t24` weakens (it may simply be done
before t53 anyway). The cut may still be right (see §4 below) but it should not
rest on a paraphrase the owner did not say.

**Over-interpretation 2 -- the double "3so" (amendment §3.5).** The amendment
picks reading (a): resume 3so's Phase 0 prerequisites (`4v8`, `gyj`, `e9m`),
*then* gates (`066`, `kxg`), then verifier, then 3so. **Reading (a) is
internally inconsistent with the graph the same document relies on.**
`gonk-4v8` depends on `gonk-066`:

```
$ bd show gonk-4v8
DEPENDS ON
  -> o gonk-066: The shape gate is syntactic ... P1
  -> v gonk-bgx ... (closed)
  -> v gonk-msz ... (closed)
```

So "resume the 3so track (4v8 ...) then the safety gates (066 ...)" cannot be
executed in that order; only `gyj` and `e9m` can precede `066`. Reading (b) --
the owner named the destination and then the path to it -- is at least as
natural and has no such problem.

**Is flagging sufficient?** No, because §4.5 writes the ordering into the epic
note on `gonk-6sp` ("Phase 0/1 preconditions as of 2026-09-12"), and §12's
Phase 1 row is rewritten around it. A one-line confirmation from the owner
costs nothing and removes the ambiguity from a document that will outlive the
conversation. Either get the line, or land the document with §4.5 held back.

---

## 2. §2's central claim: "no conflict with Phase 3"

**The distinction does not hold as stated.** Phase 3 and the `cyr -> 3jm -> qhe`
chain overlap substantially; the amendment is moving the *verify* half of
Phase 3 ahead of the Phase 1 agent and leaving the *review* half where it is.
That is a partial phase reversal. It is a justified one. It should be called
what it is.

Evidence, from `bd show gonk-td7` against `bd show gonk-cyr/3jm/qhe` and the
roadmap:

| `gonk-td7` (Phase 3) item | Where it already lives |
|---|---|
| (d) "Two verification modes reproduce/verify -- pre-state failure + post-state pass is the behavioral red/green" | That *is* the step 0/1/2 red-green sequence: `gonk-cyr` description ("Step 0 gate ... Step 1 gate ... Step 2 gate"), and `gonk-066` notes ("the red-green sequence"). Roadmap §6 3.3 says so in its own words. |
| (e) "Status from artifact existence, not agent assertion; blocked first-class; no artifact means a quoted failure string" | `gonk-cyr` outcome classes ("pass, gate-failed, flake, no-verifier-path, infra-failed"), `gonk-qhe` "per-step COLLECTED / PASSED / FAILED / SKIPPED test IDs". |
| (f) "explicit max-attempts and deterministic give-up" | `gonk-qhe`: "BOUNDED RETRIES IN BOTH DIRECTIONS". |
| (g) "three-stage privilege split resolve/work/apply" | Roadmap §6 3.6, verbatim: *"`gonk-qhe` already specifies the sandboxed runner."* |
| (a) annotated-diff coordinates, (b) validator, (c) read-only reviewer | Genuinely distinct: the review agent. Note (a)+(b) are *also* already listed under Phase 1 in §12 per §11c, so the Phase 1/3 boundary was blurry before this amendment. |

So of `td7`'s seven items, four are the substrate the amendment moves. "Phase 3
is the consumer of the verifier substrate" is true of items (a)-(c) and false
of (d)-(g). The amendment's sentence

> "Phase 3 is the **consumer** of the verifier substrate; `qhe`/`3jm` are the
> substrate. Moving the substrate earlier leaves Phase 3 where it is."

should instead say: *the verify half of Phase 3 (td7 d-g) moves ahead of the
Phase 1 agent; the review half (td7 a-c) stays.* Two further consequences the
amendment does not draw:

- **Roadmap §1 is now wrong and is left standing.** §1 says, quoting Warp's
  build guide, *"review comes after implementation, not before."* The
  amendment's §12 Phase 1 row now says the opposite for verification. A reader
  of §1 will be told one thing and of §12 another. §1 needs a one-line
  correction or a pointer to §11d.
- **§12's Phase 3 row is now duplicated.** It still reads "reproduce/verify
  modes, artifact-backed status, attempt cap" -- the same work §12's Phase 1 row
  now lists as "the verifier substrate 3jm -> qhe". Two rows claim one piece of
  work.

**The honest and stronger framing is available and unused.** The 2026-08-02
design document sequences the verifier first:

```
$ sed -n '329,345p' docs/superpowers/specs/2026-08-02-verified-change-pipeline-design.md
## Sequencing
...
1. The **verifier** first, and alone -- it needs the most hardening ...
2. The **split-diff effect kinds** and their path gates.
3. The **red-green sequence**, once there is something producing split diffs.
4. The **intent phase** last ...
```

and `gonk-066`'s notes repeat it ("SEQUENCING: verifier first and alone ...").
The epic `gonk-6sp` (filed 2026-08-14) put implement (Phase 1) before verify
(Phase 3), which contradicted the 08-02 design from the day it was filed. The
owner's 09-12 decision *resolves that contradiction in the design document's
favour*. That is a much better-supported rationale than a trajectory-verifier
aside, and the amendment does not mention it.

**One more thing `3jm` is.** `gonk-3jm` says *"The agent emits patch text in
the batch"* -- the split-diff effect kinds are the code-effects vocabulary that
roadmap Phase 1.2 ("Extend the effects vocabulary to code ... `branch`,
`commit`, `mr`") describes. So `3jm` is not "independent" of `3so` (amendment
§2: *"The verifier chain is independent, not downstream"*); it is a slice of
`3so`'s own scope filed under a different name. That is fine, but "independent"
is the wrong word and will mislead whoever scopes `3so`.

---

## 3. §3.1, the six-hop finding

### 3.1 Edge-by-edge verification

Every edge below is from `bd show <id>`, DEPENDS ON / BLOCKS sections, run in
this session.

| Claimed edge | Graph says | Note |
|---|---|---|
| `3so -> 4v8` | **Real.** `3so` DEPENDS ON `4v8`. | |
| `4v8 -> t53` ("t53 closes 4v8") | **NOT a graph edge.** `4v8` DEPENDS ON `066`, `bgx`(closed), `msz`(closed) only; `t53` has no BLOCKS section at all. | The link is `t53`'s description text *"Closes: gonk-4v8"*. The amendment's sentence *"The dependency graph says otherwise"* is therefore false: the graph says `3so` is two hops from an unblocked bead (`066`). The prose says nine. Which one is true is exactly the decision the owner has to make, and the amendment does not tell them the graph and the prose disagree. |
| `t53 -> t24` | **Real.** | But `t53` ALSO depends directly on `t21`, `t28`, `t31`. The §3.1 diagram draws `t24` as the only link between `t21` and `t53`; it is one of four parents. |
| `t24 -> t21` | Real (`t24` DEPENDS ON `t09` closed, `t21`). | |
| `t21 -> t20` | Real (`t05`-`t08` closed, `t20` open). | |
| `t20 -> t56, t57` | Real (`t16`, `t18` closed). | |
| chain roots at `t56`/`t57` | **No.** `t56` DEPENDS ON `t55`, `t41`, `t57`; `t57` DEPENDS ON `t55`; `t55` DEPENDS ON `t45`. `t45` is open and in `bd ready`. | The delivery plan already computed this: `docs/reviews/2026-09-08-delivery-plan.md:1753`: *"Critical path: T-45 -> T-55 -> T-57 -> T-56 -> T-20 -> T-21 -> {T-24, T-31, T-42} -> T-53 -> T-54."* The amendment neither cites it nor matches it. |

**Corrected count.** From the root to the implementation agent, via the prose
link: `t45, t55, t57, t56, t20, t21, t53, [4v8], 3so` -- seven open beads before
`4v8`, nine to `3so`, with `t53` additionally fanning in `t24`, `t28` (which is
behind `t27`, itself behind `t20`/`t56`/`t57`) and `t31` (behind `t21` and
`t30`). "Six hops, and the first two are an architectural cutover" undercounts
by three and puts the cutover in the wrong place: the root is a *decision*
bead (`t45`, the ADR-008 session-runtime decision), then the `SessionRuntime`
interface, then the cutover.

### 3.2 Are the edges real? (the question the amendment declines)

Judged edge by edge, with what breaks if cut.

**`t53 -> t21` (onboarding real repos requires the v1 scenario in CI).**
*Planning convenience for rom/quark; defensible for nagus.* The onboarding
flow already works on a live cluster today: project 75 was onboarded and
triaged live on 2026-09-12 (`gonk-2xev` description: two triage runs on project
75, one correct; `gonk-t24` notes: the webhook event delivered and 200'd). `t21`'s
exit criteria (`bd show gonk-t21`) are CI *automation* of that existing flow,
under 25 minutes, on every push. Onboarding rom/quark (undeployed, 3 commits a
quarter, per `gonk-4v8`) for triage needs the flow to work, which is observed,
not to be gated in CI. What breaks if cut: a regression on the onboarding path
is caught by hand instead of by CI. For nagus (deployed, handles untrusted
content, per `gonk-4v8`) I would keep it. **Suggest:** split `t53`/`4v8` --
rom/quark triage-only onboarding without `t21`; nagus behind `t21`. This also
matches §11c.2's own order, *"prove the plumbing on rom or quark first ... bring
nagus in third"*, which `t53` (all three at once) currently ignores.

**`t21 -> t20` (e2e in CI needs the chart installing on kind in CI).** Real.
An e2e scenario needs a cluster.

**`t20 -> t56/t57` (chart-in-CI needs the Jobs cutover).** *A choice, not a
necessity.* The chart could be installed in CI in its current shape; the
delivery plan chose not to build a CI leg for an architecture about to be
deleted (`t56` "Remove everything that existed only to drive Gas City"). That
is a reasonable engineering choice, but it is the choice that puts a decision
bead (`t45`) and a runtime rewrite (`t55`) at the head of every path to the
implementation agent. Naming it as a choice is what lets the owner revisit it.

**`t53 -> t24`.** Not real for onboarding. See §4.

**`4v8 -> 066` (onboarding real repos requires the workflow-scope gate).**
*Over-strict for triage-only onboarding, by 066's own text.* `gonk-066`:
*"Blocks any features/pipelines work. Not urgent for triage or scaffold as they
stand."* The reason it was attached to `4v8` (roadmap §11c.2, last paragraph)
is the self-watch hazard now tracked as `gonk-jn5`, which is IN_PROGRESS and
whose guard the owner redirected on 2026-09-07 to a fail-closed *blocklist*
(`bd show gonk-jn5`, notes). Once the blocklist lands, `066` is not what
protects onboarding; `jn5` is. The amendment says (§4.2) *"`066 -> 4v8 -> 3so`
already exists; do not duplicate it as a direct edge."* I disagree: **move it.**
`066 -> 3so` directly (where 066 says it belongs), `4v8 -> jn5` (what actually
guards onboarding). What breaks if not moved: real repos cannot be onboarded for
*triage* until a gate that 066 itself says triage does not need is closed.

**`3so -> 4v8` (implementation agent requires real repos onboarded).** *Real as
an acceptance criterion; over-strict as a build blocker.* §11c.2's argument
("a toy repo produces toy issues"; "Not gonk, until 066 closes") is about what
the agent must be *proven* against, not what it can be *built* against. Nothing
in `gonk-3so`'s description needs a real repo to write the agent, the effect
kinds, the validator or the `.gonk.yml` contract; gonk's own test project (75)
serves for that, as it did for triage. If the owner wants `3so` work to start
before the cutover lands, the honest move is to split `3so` into *build*
(unblocked by `4v8`) and *prove on a real repo* (blocked). Otherwise the stated
order -- "then 3so" -- is not reachable until `t45 ... t53` are done.

---

## 4. Cutting `gonk-t53 -> gonk-t24`

**Sound in principle; unsafe as proposed, because it omits a condition `t24`'s
own notes state.**

The principle holds. `t53`'s change text (`bd show gonk-t53`) is: three
projects through the MR flow, a triage comment on the next issue,
`/cost/project` shows spend, one ceiling exhausted, `defer` observed. No mention
step. The delivery plan attached `t24` because E5 ("an `@gonk` mention ...
yields exactly one follow-up comment", `docs/reviews/2026-09-08-delivery-plan.md:62`)
was mapped to `T-24` and `T-53` was made to wait on all of wave 8. Planning
convenience.

**What a newly onboarded real repo loses if `@gonk` does not work**, from the
code:

- Every triage answer gets the footer
  `cmd/gonk-gate/broker_status.go:39`:
  `"_Reply with `@%s` to send this back to me -- I only see comments that mention me._"`,
  appended at `cmd/gonk-gate/broker_apply.go:338`.
- The onboarding MR's `.gonk.yml` ships `respond_to_mentions: true`
  (`pkg/intake/render.go:69`), its README table says *"Mentioning `@bot` in an
  issue comment gets a reply in that thread"* (`render.go:161`) and the summary
  says *"Mentioning `@bot` in an issue comment reaches it"* (`render.go:343`).
- The trigger is refused outright: `cmd/gonk-gate/broker_inject.go:47-50`
  ("mention-reply is DELIBERATELY ABSENT ... runDispatch now refuses the
  trigger").

So the scaffold path and every triage comment *advertise* a surface that does
not answer -- and on nagus that is to real users, not the owner. `t24`'s own
note already says the fix: *"suppressing it is smaller than the port and should
not wait for it."* **Condition the cut** on a small bead ("suppress the mention
footer and the onboarding README/`.gonk.yml` mention lines while the trigger is
refused"), and make `t53` depend on that bead instead of `t24`. Without that,
the cut ships a broken promise into the first real repos.

Nothing else in the onboarding MR flow depends on mentions: `MayMentionReply`
(`pkg/intake/state.go:157`) is only consulted on the note-hook path
(`pkg/intake/dispatch.go:153-164`), and the meter does not check it
(`state.go:154`).

Also worth recording: cutting `t53 -> t24` removes exactly one bead from the
critical path, because `t53 -> t21` exists directly. The amendment's §3.2
heading, *"Downgrading `gonk-t24` does not take it off the critical path"*, is
right, but the §3.1 diagram makes the edge look structurally central when it is
a one-bead detour.

---

## 5. The trajectory-verifier rationale

**Not used honestly. Two separate problems.**

**Problem 1 -- the rationale is attached to the wrong beads.** A trajectory
verifier checks *what the agent did* (tool calls, reads, turns). The beads the
amendment moves (`3jm`, `qhe`) are the red/green *test-run* verifier: split
diffs, a sandboxed runner, test-ID set arithmetic. gonk's trajectory verifier is
a different thing with different beads: `pkg/trace` (`gonk-p8j`, closed) and
the fifth gate (`gonk-hsb`, **P2, open**). The owner's aside -- *"other people
... are including trajectory verifiers in their implementation agents"* --
supports "verifier before implementation" only loosely and supports
"`qhe`/`3jm` before `3so`" not at all. If the owner does want trajectory
checking inside the implementation agent, the bead to re-prioritise is
`gonk-hsb`, which the amendment does not mention.

**Problem 2 -- `broker_trajectory.go` cannot carry the weight.** The amendment
(§1, and §11d) says:

> "gonk already has the seam -- `cmd/gonk-gate/broker_trajectory.go` -- so
> this is a question of sequencing, not of new architecture."

and §11d:

> "a trajectory verifier constrains the run while it is still cheap to
> redirect."

Read against the file:

- It runs **after the session has ended**, on the finished batch, as the fifth
  gate in `broker_apply.go:287-292`, after `ValidatePaths` and
  `ValidateComments`. It does not constrain a run; it judges a result. That is
  precisely the "detector applied after the fact" the amendment contrasts
  itself against.
- It is **observing-only**: `broker_trajectory.go:16-21`, `:74-79`. Enforcement
  is `GONK_ENFORCE_TRAJECTORY == "1"` (`cmd/gonk-gate/main.go:148`), and
  `gonk-hsb`'s notes (2026-09-07) say the honest gate for turning it on has
  **not been met**: one observed false positive on an honest session (issue
  !49), predicate narrowed to `close`, and no `close` verdict observed since.
- Its predicates are deliberately triage-shaped: `gonk-hsb` says *"Start with
  the one predicate that pays for the slice, for triage"* and *"If a predicate
  cannot be evaluated by set arithmetic over tool names, it is a judgement and
  does not belong here."* Nothing in it is about an implementation agent's
  trajectory.
- It **imports `pkg/beadstore`** (`broker_trajectory.go:6`) and loads policy
  from `d.PackDir` = the baked `pack/` (`pkg/trace/policy_load.go:13`). `gonk-t56`
  deletes both: *"Delete `pkg/gcapi` ... `pkg/beadstore` ... `pack/`"*. So the
  "existing seam" is scheduled to be rewritten by the cutover that sits at the
  head of the chain in §3.1. Whether the seam survives T-56 in any form is
  undecided by any bead I found.

The owner offered an aside as supporting evidence. The amendment turned it
into an architecture claim ("verification lives inside the agent"), attached it
to beads it does not describe, and cited a file that is post-hoc,
observing-only, triage-scoped and about to lose its imports. Drop the
architecture claim; keep the owner's sentence as the aside it was; cite the
2026-08-02 design document's §Sequencing as the actual rationale (see §2).

---

## 6. What the amendment should say and does not

Ordered by consequence.

1. **`gonk-qhe` has an unfiled infrastructure dependency the new edge
   `3so -> qhe` would put on the implementation agent's critical path.**
   `gonk-qhe`: *"Today's NetworkPolicy is UNENFORCED (Flannel; Cilium
   suspended) ... it should not ship before that is settled."*
   `docs/environment.md:232-234` confirms: *"NetworkPolicies are not enforced
   on this cluster ... the Cilium HelmRelease that would is suspended."*
   `bd search cilium` and `bd search calico`: *No issues found.* So adding
   `3so -> qhe` makes the implementation agent wait on infra work that has no
   bead, and the amendment does not say so. Either file the bead and make `qhe`
   depend on it, or scope the new edge to `3so -> 3jm` and state that `qhe`
   waits on CNI. (T-20's "calico leg" is a kind-in-CI cluster, not the live
   one -- unverified whether the live cluster has changed since
   `environment.md` was written.)

2. **`gonk-kxg` is partially implemented and its remainder is not what `3so`
   needs.** `bd show gonk-kxg`, notes 2026-09-02: the triage verdict half
   (reply-only / code-change / close, validated, labelled) is **done**
   (`gonk-aib`). What remains: the *pipeline* classifier (for
   `actions.pipelines`), a `cannot-determine` route, and "the classification
   selects the agent -- because there is only one agent to select". That last
   item cannot be completed until a second agent exists, so `3so -> kxg` as a
   whole-bead edge is circular in intent. The amendment's justification for the
   edge (*"an agent told to make CI green has deleting the failing test as a
   locally optimal move"*) is the *pipelines* case, which is not the
   issue-to-MR path `3so` builds. Scope the edge to what `3so` actually needs
   from `kxg` (which may already be shipped), or split `kxg`.

3. **The graph/prose disagreement at `t53 -> 4v8`** (§3.1 above). The
   amendment should either add the graph edge (so `bd ready` stops being able
   to list `4v8` the moment `066` closes) or state explicitly that `4v8` may be
   closed without `t53`. Today the two tools disagree and nothing says which is
   authoritative.

4. **Two P0 bugs are ready and unmentioned.** `bd ready` (first 10 of 108):
   `gonk-p7qh` P0 *"R-48 dropped root@'%' and silently killed the entire
   outcome path"* and `gonk-alw` P0 *"A cold controller accepts a session and
   silently never creates the pod"*. A document that opens with "`gonk-2xev`
   ... *Next*" while two P0s sit ready should at least say why they are not
   next. Unverified whether `p7qh`'s dead outcome path affects `2xev`'s
   verification (2xev is about how outcomes are *booked*), but the relation is
   close enough to name.

5. **`2xev`'s fix escalates onto a rung with a reported broken output path.**
   `gonk-2xev`'s fix sends reply-only-at-lowest-rung to `qwen3-6-35b`;
   `gonk-w41`'s notes (2026-09-02): the vLLM repoint works (~70 s/turn) *"but
   the sweep still reports gate-failed, which points at the output PATH ...
   see gonk-2tb."* "Tested and done" for `2xev` should include one observed
   escalation that lands, or it is the proxy again.

6. **`3jm` is Phase 1.2, not an independent chain** (§2 above). Calling it
   independent will mislead whoever scopes `3so`.

7. **§1 and §12's Phase 3 row are left contradicting the amendment** (§2
   above).

8. **§11c.2's "rom or quark first, nagus third" is not reflected in `t53`**,
   which onboards all three at once behind the full CI chain. Either `t53`
   should be split or §11c.2's order should be marked superseded.

9. **The `t24` note's smallest fix is not carried as a bead.** *"Suppressing
   [the invitation] is smaller than the port and should not wait for it."* The
   amendment downgrades the port and says nothing about the suppression, which
   is the part that matters for real repos (§4 above).

10. Nit: §11d says "project 75 issue !71". `!` is GitLab's MR sigil; issues
    are `#`. `gonk-2xev` has the same slip. Unverified which it is; worth
    getting right in a document about a live observation.

Nothing in §11b is invalidated: Revision 2 ("build the detector first") is
strengthened, not contradicted. §11c.2's "Not gonk, until `gonk-066` closes"
stands. §1's "review comes after implementation" is the only roadmap sentence
this amendment reverses, and it should say so there.

---

## 7. Should this land as written?

**No. Land with the changes in §8.** The direction is right and the owner asked
for it. The document as written would leave the next reader believing four
things that are not true: that the phase order is untouched, that a trajectory
seam exists to build on, that the chain to `3so` is six graph edges, and that
reading (a) of the instruction is executable.

**On achievability:** with the graph as it stands and `t53 -> 4v8` treated as
real, "then 3so" is nine beads out, rooted in an undecided ADR (`t45`). The
stated order is achievable only if one of these is decided: (i) `4v8` is split
so rom/quark triage-only onboarding does not wait for CI; (ii) `3so` is split
into build and prove; or (iii) the owner accepts that `3so` proper follows the
cutover. The amendment says this is the owner's call. It should give the owner
the corrected count and these three options rather than a diagram that
undercounts.

---

## 8. Named changes required to land

1. §2 and §11d: replace "not a reversal of Phase 3" with "moves the verify half
   of Phase 3 (`td7` d-g) ahead of the Phase 1 agent; the review half (`td7`
   a-c) stays." Correct roadmap §1's "review comes after implementation" with a
   pointer to §11d. Remove the now-duplicated items from §12's Phase 3 row or
   mark them "moved to Phase 1 preconditions".
2. Drop the "verification lives inside the agent / existing seam" architecture
   claim. Keep the owner's sentence as an aside. Cite
   `docs/superpowers/specs/2026-08-02-verified-change-pipeline-design.md`
   §Sequencing as the rationale. If trajectory checking in the implementation
   agent is wanted, say it is `gonk-hsb` plus a post-T-56 rewrite, and file
   that.
3. §3.1: correct the chain to the delivery plan's own critical path
   (`docs/reviews/2026-09-08-delivery-plan.md:1753`) -- root `t45`, nine beads
   to `3so` -- and state that `t53 -> 4v8` is prose, not a graph edge, and that
   `t53` has three other parents (`t21`, `t28`, `t31`). Then give the owner the
   three options from §7.
4. §4.2: instead of "do not duplicate `066`", move `066 -> 4v8` to
   `066 -> 3so` and add `4v8 -> jn5`.
5. §4.2: scope `3so -> kxg` to the shipped verdict half or split `kxg`; scope
   `3so -> qhe` or file the CNI bead `qhe` says it needs.
6. §4.3: condition the `t53 -> t24` cut on a new bead that suppresses the
   mention footer (`broker_status.go:39`) and the onboarding mention lines
   (`render.go:69,161,343`), with `t53` depending on that bead.
7. §3.5 / §4.5: get the one-line confirmation of reading (a) vs (b) before
   writing the order into `gonk-6sp`; note that (a) as written is impossible
   because `4v8` is behind `066`.
8. Acknowledge the two ready P0s and `w41`/`2tb` next to "`2xev` next".

---

## 9. What I did NOT check

- Whether the live cluster still runs Flannel with Cilium suspended; I relied
  on `docs/environment.md:232-252` and the ADRs, not on the cluster.
- Whether `gonk-6sp`'s notes have changed since 2026-08-14 (its `Updated` field
  says 08-14; I did not diff bead history).
- The full `bd ready` list (only the first 10 of 108 were shown; my first grep
  of it was against a truncated list and is not evidence of anything).
- The body of `pkg/trace/verdict.go`'s `Classify`; I read its declarations and
  `gonk-hsb`'s notes, not the predicate logic.
- Whether project 75's "!71" is an issue or an MR.
- Whether `4v8` was ever intended to be closed independently of `t53` -- I
  found no bead or document saying either way.
- `make gate`, any test, any GitLab state, any bead mutation. Read-only
  throughout.

---

## 10. Re-check of the revision (commit `455134f`, rebased on `bd9ceb3`)

Read-only. `git diff origin/main docs/sequencing-2026-09-12`: two files,
+268/-4. Every claim carries its evidence; anything unverified is labelled.

### 10.1 Two corrections to my OWN review first

- **§6.1 / §8.5 were wrong: the CNI bead exists.** `gonk-dku` (P1, open,
  2026-09-06) "NetworkPolicies are not enforced: the cluster runs flannel with
  no policy controller"; its WHAT TO DO option 1 is *"Install a policy
  controller. Calico or Cilium can run alongside flannel"*, recommendation
  *"2 now and 1 deliberately"*. My `bd search cilium/calico` found nothing
  because search matches titles. The revision's §3.4 cites `gonk-dku` in the
  same sentence as *"there is no bead for a Cilium/Calico migration"*, which
  is false. The real gap is an EDGE: `gonk-qhe` has no dependency on
  `gonk-dku` (`bd show gonk-qhe`: DEPENDS ON `gonk-3jm` only).
- **§6.5 leaned on a closed bead.** `gonk-w41`'s "see gonk-2tb" points at a
  bead CLOSED 2026-09-02 ("RESOLVED AND PROVEN END TO END ... issue !45").
  The revision's §3.6 hardens this into *"gonk-w41 reports the escalation
  target's output path broken"* -- stale. The live fact is narrower: no
  qwen3-6-35b escalation has been OBSERVED to land since the vLLM repoint.

### 10.2 The four material corrections

| # | Status | Evidence |
|---|---|---|
| (a) six-hop -> two-hop, prose-vs-edge stated | **Folded in.** Plan §2 lines 45-56; cites delivery-plan `:1753`, root `t45`, nine beads. `bd show gonk-t53` still has no BLOCKS section; `gonk-4v8` DEPENDS ON `066`+2 closed. | Omits that `t53` fans in `t28`, `t31` as well as `t21`/`t24`. Minor. |
| (b) partial phase reversal, §1 honest | **Folded in for prose; NOT for §12.** Plan §3.2, roadmap §11d "This IS a partial phase reversal", §1 blockquote. §1's qualification is honest (review vs verification is the real distinction; house style already uses the same glyph at roadmap line 15). | **§12 Phase 3 row is byte-for-byte unchanged**: still "reproduce/verify modes, artifact-backed status, attempt cap" while the new Phase 1 row lists "verifier substrate 3jm -> qhe". §11d then says *"what each phase contains is not [unchanged]"* -- §12 contradicts it. Named change 1, clause 3, not done. |
| (c) trajectory rationale -> `gonk-hsb`, `broker_trajectory.go` withdrawn | **Withdrawn in plan §3.3; only half-withdrawn in roadmap §11d.** §11d still states the decision's ground as *"That relocates verification ... to a component inside the agent ... a trajectory verifier constrains the run while it is still cheap to redirect"* and only THEN says the wiring does not exist. The 08-02 design `§Sequencing` ("The verifier first, and alone") is cited **nowhere** in either file (`grep 08-02` on both: no hits). Named change 2, second half, not done. Line refs (`broker_apply.go:290`, `main.go:148`, beadstore import at `broker_trajectory.go:6`) verified. |
| (d) `qhe` CNI precondition, `kxg` partial | **Acknowledged, not resolved, and (d)-qhe is mis-stated** per 10.1. `kxg`: §3.5 says "name the part" and does not name it; §4.3 adds no edge; yet plan §1 item 4 and roadmap §12 Phase 1 still list `kxg` as a gate before `3so`. The part `3so` needs (verdict classification, code-change vs reply-only) shipped in `gonk-aib`; the doc should say so. |

### 10.3 The other four named changes

- **4 (move `066->4v8` to `066->3so`, add `4v8->jn5`):** half done. §4.2
  moves the edge and its Why column names `gonk-jn5`'s blocklist as
  "onboarding's real guard" -- then does not add the `4v8 -> jn5` edge. After
  the move `bd ready` lists `4v8` with `jn5` IN_PROGRESS and no graph link.
  The move itself is right: `066` says "Not urgent for triage or scaffold as
  they stand" and "must be closed before any code-writing agent ships";
  onboarding runs scaffold+triage only. What onboarding needs is the
  blocklist (`jn5` item 1) and that is the guard the revision names and fails
  to wire. Not blocking the DOCUMENT; blocking the AMENDMENT when applied.
- **6 (condition the `t53->t24` cut on a suppression bead):** condition
  stated (§4.4), bead not proposed, `t53` not re-pointed. And §4.4's last
  sentence is inverted: *"Onboarding nagus without the cut is a broken
  promise"* -- the cut is what creates the exposure; it should read "with
  the cut but without the suppression". §4.1's `t24` row cross-refers to
  "§3 of the review"; it is §4.
- **7 (owner confirmation):** obtained and recorded (plan §1 lines 23-26).
  Not overstated as to what the owner said. Overstated in the next clause:
  *"It is also the only reading the graph permits"*. The graph ruled out one
  ordering (`4v8` before `066`), not reading (a) wholesale (`gyj`/`e9m`
  could always precede `066`) -- and §4.2's own move makes even that
  ordering legal. §5's last paragraph has it right; §1 does not. The
  `gonk-6sp` epic-note from the first draft's §4.5 is silently dropped
  (no `6sp` in the revised plan); fine, but say so.
- **8 (acknowledge P0s, w41/2tb):** §3.6 lists them and adds three claims I
  cannot stand behind. `p7qh` "fixed by chart 0.1.8 and needs closing":
  commit `6e9441a` is chart 0.1.8, but the bead's notes record a SECOND
  defect ("a partial state write should not be able to produce a label that
  no query matches"; bead go-ffoe stranded with label `gonk::`) and three
  follow-ups, none of which has a bead (`bd search stranded / dolt.user /
  sweep-health`: none). "Needs closing" is wrong without a split. `alw`
  "confirmed live today on a warm controller, so its own diagnosis is
  wrong": **no evidence anywhere in the repo or beads** -- `gonk-alw` last
  updated 09-09, `docs/reviews/2026-09-11-e2e-ground-truth.md:354-368,533`
  describe only the cold (~40 s) window, and `gonk-2xev` records pods that
  ran. Unverified; must be labelled or sourced. `w41`: see 10.1. And the
  change asked "say why they are not next"; §3.6 still does not.

### 10.4 New in the revision

1. **§4.1 raises `gonk-hsb` to P1.** Over-correction. My finding was
   conditional ("if the owner does want trajectory checking inside the
   implementation agent"). The owner named 2xev, xsk3, 7s9p, t24, 066, kxg,
   3jm, qhe -- not hsb. `hsb` is blocked on EVIDENCE, not priority ("observe
   several CLOSE verdicts", notes 2026-09-07); its predicates are triage-only
   by design; it reads `pack/` and imports `pkg/beadstore`, both deleted by
   `gonk-t56` (the revision's own §3.3 says so). P1 with no edge to `3so` and
   no post-T-56 bead ranks a thing that cannot currently be worked and does
   not gate anything. Make it an owner question, not an amendment.
2. **§2's "gates come first by construction" is undercut by §4.2.** Once
   `066->4v8` moves to `066->3so`, `4v8` is unblocked and the chain to `3so`
   is one open hop plus `066` in parallel. §2 describes the pre-amendment
   graph as if it were permanent.
3. **§3.4 "work nobody has filed"** -- false, per 10.1. Fix: add `qhe -> dku`
   and note `dku` recommends the controller "deliberately", i.e. unscheduled.
4. **§4.4 inverted sentence** (10.3).
5. **§3.6's three unsourced/stale claims** (10.3).
6. §12 Phase 1 row now lists `3jm` twice (as "verifier substrate" and as
   "code effects"). Consistent with §3.2's own point; note it.
7. Nit carried forward: roadmap §11d "project 75 issue !71". `glab api
   projects/75/issues/71` returns "Descending date sort returns oldest
   first"; `merge_requests/71` is 404. It is `#71`.

### 10.5 §3.1's three-options framing

Fair as a statement of the choice, and it is the corrected count I asked for.
But the document has the evidence to recommend option 1 and declines:
`gonk-4v8`'s own description, owner-decided 2026-08-13, says *"prove the
plumbing on rom or quark first ... bring nagus in third once the loop is
trusted"*. Option 1 IS the owner's already-stated order, and option 3 is what
`gonk-t53` (all three at once, wave 9) silently chose instead. The honest
sentence is: "Option 1 matches the owner's 08-13 order; `t53` as filed is
option 3; recommend 1 unless the owner has changed their mind." Not picking
is not wrong; not saying the owner already picked is.

### 10.6 Verdict on the revision

**Not as-is. Land after the following edits, all of which are text:**

1. §12 Phase 3 row: strike or mark "moved to Phase 1 preconditions" the three
   items now in the Phase 1 row. (Named change 1, unfinished.)
2. Roadmap §11d: drop the "relocates verification ... inside the agent ...
   cheap to redirect" ground, or move it under the `hsb` paragraph as the
   owner's aside; cite
   `docs/superpowers/specs/2026-08-02-verified-change-pipeline-design.md`
   §Sequencing as the rationale for 3jm/qhe-first. (Named change 2,
   unfinished.)
3. §3.4 and §11d's last paragraph: replace "no bead / nobody has filed" with
   `gonk-dku` (P1, open, unscheduled) and change §4.3's condition to "add
   `qhe -> dku`". (My error, copied.)
4. §3.6: source or label the `alw` warm-controller claim; correct `p7qh` to
   "first defect fixed in chart 0.1.8 (`6e9441a`); second defect and three
   follow-ups unfiled"; correct `w41` to "2tb closed 09-02; no 35B escalation
   observed to land since".
5. §4.4: fix the inverted sentence; §4.1: fix "§3" -> "§4".
6. §4.1 `hsb` row: remove, or move to a "questions for the owner" list.
7. §1: delete "It is also the only reading the graph permits", or qualify it
   as pre-§4.2.

Should-do, not blocking: add `4v8 -> jn5` to §4.2; propose the suppression
bead in §4.4; state in §3.5 that `aib` already shipped the part of `kxg`
that `3so` needs and remove `kxg` from §12's Phase 1 gate list or say what
remains; recommend option 1 in §3.1 with the 08-13 citation; `!71` -> `#71`.

### 10.7 Not checked this pass

Live cluster state; whether chart 0.1.8/0.1.10 is what is deployed (xsk3
records the HelmRelease oscillating today); bead history for `alw` beyond
`bd show`; the `.beads/issues.jsonl` working-tree diff (memory rows only,
predates this re-check, untouched).
