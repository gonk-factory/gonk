# Sequencing decision, 2026-09-12: verifier substrate before the implementation agent

**Status: DECIDED 2026-09-12. Bead amendments in §4 are applied.** Written for review
first. Revised twice against independent review
(`docs/reviews/2026-09-12-sequencing-review.md`, including its §10 re-check).
§5 records what those reviews corrected — several corrections were substantial,
and two were errors in the review itself that an earlier draft of this document
copied.

## 1. The decision

Set by the owner on 2026-09-12, after the first live end-to-end triage run on
the deployed build, and refined the same day once the §6 questions were put to
them. **The order, as decided:**

1. **`gonk-alw`** (P0) — a controller accepts a session and silently never
   creates the pod. Moved ahead of `gonk-2xev` by the owner. Its stated
   cold-start diagnosis is wrong: it fired today on a controller up 4h24m
   (evidence on the bead), so a fix keyed on startup readiness will not cover
   it.
2. **`gonk-2xev`** — a non-answer is booked as rung success, so the escalation
   ladder never fires.
3. **`gonk-xsk3`** and **`gonk-7s9p`** — the chart/image skew gate, and
   verifying the readiness probe against a live cluster, in the fail direction.
   Both tested and done before moving on.
4. **`gonk-t24`** drops to nice-to-have, conditional on §4.4.
5. Then **resume the path to `gonk-3so` by landing its prerequisites**, behind
   the safety gate `gonk-066` and the verifier substrate (`gonk-3jm`,
   `gonk-qhe`).

Item 5 is the owner's own clarification of an instruction that named
`gonk-3so` twice: *"i meant resuming the path to 3so by landing the
prerequisites."*

**Three decisions taken on the §6 questions:**

- **`gonk-4v8` is SPLIT** (§3.1 option 1). One triage-only repo — rom or quark
  — unblocks `gonk-3so`; nagus and the full v1 CI proof follow behind. This
  matches `gonk-4v8`'s own 2026-08-13 wording and reverses the scope `gonk-t53`
  silently assumed.
- **`gonk-hsb` is NOT raised.** Instead, file the rescoping work first: what a
  trajectory verifier looks like once `gonk-t56` removes the `pack/` and
  `pkg/beadstore` surfaces it currently reads. Priority is revisited after
  that, not before.
- **`gonk-alw` goes before `gonk-2xev`**, per item 1 above.

An earlier draft also proposed an amendment to the `gonk-6sp` epic note. It is
dropped: roadmap §11d carries the same content and the epic should point at one
source rather than restate it.

## 2. The chain to `gonk-3so` is TWO hops

```
gonk-066  (open, P1, no dependencies)
   └─→ gonk-4v8  (onboard nagus, rom, quark)
         └─→ gonk-3so  (implementation agent)
```

`gonk-4v8` depends on `gonk-066`, `gonk-bgx` (closed) and `gonk-msz` (closed).

The longer route — `gonk-t53` → `gonk-t21` → `gonk-t20` → the Gas City cutover
— is real work but **`gonk-t53 → gonk-4v8` is prose, not a graph edge**.
`gonk-t53`'s description says *"Closes: `gonk-4v8`"* and no dependency exists.
An earlier draft treated that sentence as an edge and asserted a six-hop chain
rooted at `gonk-t56`/`gonk-t57`. That was wrong. The delivery plan's own
computation (`docs/reviews/2026-09-08-delivery-plan.md:1753`) reaches
`gonk-3so` in nine beads by the prose route, rooted at `gonk-t45`. (`gonk-t53`
also has parents `gonk-t28` and `gonk-t31` that neither count includes.)

**Which route applies is a scoping decision, not a graph fact** — see §3.1.

## 3. Findings

### 3.1 What `gonk-4v8` means decides everything, and the owner already answered it once

If `gonk-4v8` means *all three repos onboarded and proven through the full v1
CI scenario*, `gonk-3so` follows the cutover. If it means *one triage-only repo
onboarded*, `gonk-3so` is two hops out. Three ways to resolve:

1. **Split `gonk-4v8`** so rom-or-quark triage-only onboarding skips the CI
   chain, leaving nagus and the CI proof behind it.
2. **Split `gonk-3so`** into *build the agent* (needs any repo) and *prove it*
   (needs the real ones).
3. **Accept** that `gonk-3so` follows the Gas City cutover.

**DECIDED 2026-09-12: option 1.** The owner chose it when the question was put
directly, and it is what the bead already said: `gonk-4v8`'s own owner-decided
description (2026-08-13) reads *"prove the plumbing on rom or quark first (a
bad MR costs nothing, small enough to diagnose in one sitting), hold the
sibling as a control or A/B pair, bring nagus in third once the loop is
trusted."* `gonk-t53` as filed — all three, wave 9 — silently chose option 3,
and that is the scope this reverses.

### 3.2 This IS a partial phase reversal

An earlier draft claimed no conflict with Phase 3, on the grounds that
`gonk-td7` merely *consumes* the verifier substrate. That does not survive
inspection. `gonk-td7`'s items — (d) reproduce/verify red-green, (e)
artifact-backed status, (f) attempt cap, (g) resolve/work/apply privilege split
— are what `gonk-cyr`/`gonk-3jm`/`gonk-qhe` deliver, and roadmap §6 3.6 says
verbatim *"`gonk-qhe` already specifies the sandboxed runner"*. Nor is the
chain independent of Phase 1: `gonk-3jm` is Phase 1.2, the code-effects
vocabulary.

**The verify half of Phase 3 moves ahead of Phase 1's implementation agent; the
review half (a–c) stays.** Roadmap §1 and §12 are both amended to say so — §12's
Phase 3 row must not keep listing items the Phase 1 row now claims.

### 3.3 The trajectory rationale belongs on `gonk-hsb`

The owner offered trajectory verification as supporting evidence. An earlier
draft attached it to `gonk-3jm`/`gonk-qhe` and cited
`cmd/gonk-gate/broker_trajectory.go` as an existing seam. Both wrong:

- `gonk-3jm`/`gonk-qhe` are the **red/green test-run** verifier. gonk's
  trajectory verifier is `pkg/trace` + **`gonk-hsb`**.
- `broker_trajectory.go` runs **after** the session on the finished batch
  (`broker_apply.go:290`), enforcement is **off by default** (`main.go:148`,
  gated on `GONK_ENFORCE_TRAJECTORY`), it is triage-scoped, and it imports
  `pkg/beadstore` and reads `pack/` — both removed by `gonk-t56`.

**The actual documented rationale for verifier-first is already in the repo**
and should be cited instead of reconstructed: the §Sequencing section of
`docs/superpowers/specs/2026-08-02-verified-change-pipeline-design.md:329`.

**Decided (§6):** `gonk-hsb` is NOT raised. The rescoping work comes first —
what a trajectory verifier looks like once `gonk-t56` removes the `pack/` and
`pkg/beadstore` surfaces it reads — and priority is revisited after that.

### 3.4 `gonk-qhe` needs an edge to `gonk-dku`, which exists

`gonk-qhe` states it must not ship before CNI policy enforcement is settled.
**The bead for that exists:** `gonk-dku` (P1, open, 2026-09-06) — *"NetworkPolicies
are not enforced: the cluster runs flannel with no policy controller"*, whose
own remedy option 1 is installing Calico or Cilium alongside flannel.

An earlier draft said no such bead had been filed. That was false — it repeated
an error in the first review, which had searched titles only. The real gap is
an **edge**: `gonk-qhe` depends only on `gonk-3jm`. So the §4.3 condition is
"add `gonk-qhe → gonk-dku`", not "file the missing work".

### 3.5 `gonk-3so → gonk-kxg` should name a part, and that part has shipped

`gonk-kxg` is partially done: the verdict-classification half shipped in
`gonk-aib` (closed). The remainder is the **pipeline classifier**, and
"classification selects the agent" needs a second agent to exist — which is
`gonk-3so` itself. So a whole-bead edge is circular.

The part `gonk-3so` actually needs is already delivered. §4 therefore adds no
`gonk-kxg` edge, and roadmap §12 should stop listing `gonk-kxg` unqualified as
a gate in front of `gonk-3so`.

### 3.6 Adjacent open work

- **`gonk-p7qh`** (P0): the credential defect is fixed and deployed (chart
  0.1.8, `6e9441a`). The bead is **not** merely awaiting closure — it records a
  **second defect**, bead `go-ffoe` stranded with the malformed label `gonk::`
  and unreachable by any sweep pass, plus three follow-ups (no CI runs the
  sweep against a real database; `dolt.user: gc` is decorative; a sweep that
  cannot list beads should fail loudly). None are filed.
- **`gonk-alw`** (P0): its cold-start diagnosis is too narrow. Evidence is now
  **on the bead** (2026-09-12 note): project 75 issue #69 attempt 1, controller
  up 4h24m, session accepted at 06:13:36 and abandoned at 06:15:38 with no
  pod-creation attempt logged and no pod in the namespace events.
- **`gonk-w41`**: its pointer to `gonk-2tb` is stale — `gonk-2tb` is **closed**
  (2026-09-02, "RESOLVED AND PROVEN END TO END"). Whether the 35B rung's output
  path still fails is **unverified**; no escalation to it has been observed to
  land. This matters because `gonk-2xev` exists to make escalation fire.

**Why `gonk-2xev` before two open P0s is not argued here.** It is the owner's
stated order and defensible — `gonk-2xev` is small, and both P0s are latent
rather than currently firing — but the document should not imply the graph
demands it.

## 4. Proposed amendments — NOT YET APPLIED

### 4.1 Priorities

| Bead | From | To | Why |
|---|---|---|---|
| `gonk-7s9p` | P2 | **P1** | Owner wants it tested and done in this batch; it is the fail-direction verification of a probe that already took the controller down once (`gonk-xsk3`). |
| `gonk-t24` | P1 | **P2** | Owner: nice-to-have. Safe only with §4.4. |

`gonk-hsb` is deliberately **not** proposed for a priority change — see §6.

### 4.2 Edges to MOVE and ADD

| Change | Why |
|---|---|
| **Move** `gonk-066` blocks `gonk-4v8` → `gonk-066` blocks **`gonk-3so`** | `gonk-066` itself says it is "not urgent for triage or scaffold as they stand", and onboarding runs only scaffold and triage. The gate belongs in front of the code-writing agent it was filed for. |
| **Add** `gonk-4v8` depends on `gonk-jn5` | Required by the move above, not optional. `gonk-jn5`'s blocklist is onboarding's real guard; without this edge, removing `gonk-066` leaves `gonk-4v8` unguarded and `bd ready` will offer it while `gonk-jn5` is still in progress. |

### 4.3 Edge to ADD, conditionally

| Bead | Depends on | Condition |
|---|---|---|
| `gonk-3so` | `gonk-qhe` | Add `gonk-qhe → gonk-dku` at the same time, or `gonk-3so` inherits an unscheduled CNI migration as a hidden precondition (§3.4). |

### 4.4 Edge to CUT, conditionally

| Bead | Remove dependency | Condition |
|---|---|---|
| `gonk-t53` | `gonk-t24` | **File and land a suppression bead first.** Cutting the edge is what lets real repos be onboarded while `@gonk` is refused — and every triage comment carries *"Reply with `@gonk` to send this back to me"* (`broker_status.go:39`, appended at `broker_apply.go:338`), while the onboarding MR's `.gonk.yml` and README advertise mentions (`render.go:69,161,343`) and `runDispatch` refuses the trigger (`broker_inject.go:47-50`). So the cut creates the exposure; suppressing the invitation removes it. Nothing else in onboarding depends on mentions. |

**Proposed suppression bead:** stop emitting the `@gonk` reply invitation, and
the onboarding README/`.gonk.yml` mention copy, while `mention-reply` is not in
`agentForTrigger`. Smaller than `gonk-t24` itself and should not wait for it.

### 4.5 Roadmap

§11d records the decision. §12's Phase 1 entry gains the gates and verifier
substrate; §12's **Phase 3 row must drop the three items that moved**
(reproduce/verify, artifact-backed status, attempt cap) or mark them moved.
§1's *"review comes after implementation"* is qualified per §3.2.

## 5. What the independent reviews corrected

`docs/reviews/2026-09-12-sequencing-review.md` and its §10 re-check.

**Corrections to this document:** the six-hop chain was false (§2); "no conflict
with Phase 3" did not hold (§3.2); the trajectory rationale was on the wrong
beads and the cited seam is post-session and disabled (§3.3); `gonk-kxg`'s
partial state was missing (§3.5); §4.4's rationale was stated inverted; §4.2's
edge move needed a companion edge (§4.2); `gonk-hsb → P1` was an
over-correction (§6).

**Corrections to the review itself, which an earlier draft of this document
copied:** `gonk-dku` exists and is the CNI bead (§3.4) — the review's search
matched titles only; and `gonk-2tb` is closed, so the `gonk-w41` claim built on
it was stale (§3.6).

## 6. Owner questions — ANSWERED 2026-09-12

1. **Option 1, 2 or 3 in §3.1?** → **Option 1.** Split `gonk-4v8`; one
   triage-only repo (rom or quark) unblocks `gonk-3so`.
2. **Raise `gonk-hsb`?** → **No.** Rescope it after `gonk-t56` first, then
   revisit priority. Raising it now would rank work whose implementation
   assumptions are about to be deleted.
3. **Is `gonk-2xev` genuinely before the two open P0s?** → **No.**
   `gonk-alw` goes first.
