# Design note: the verified change pipeline

_2026-08-02. Owner's architecture, written up with the engineering consequences.
Bead: `gonk-066`. **A design note, not a plan** — nothing here is scheduled, and
it presupposes a code-writing agent that does not exist yet._

## The shape

Three phases around the session, not one gate after it:

```
1. INTENT      model reads the issue, proposes a SCOPE          } narrows
   (pre)       -> frozen as a constraint before any work starts }
                                                                 
2. WORK        session runs, steered by .agent/                  
   (session)   -> returns a split diff + non-code effects        
                                                                 
3. VERIFY      NON-AGENTIC. applies the diff to a checkout,       } decides
   (post)      runs the suite, checks red->green, checks paths    }
```

Phase 1 supplies constraints. Phase 3 decides. **No model appears in phase 3**,
which is what keeps spec 6.3 intact: the model may say what a change is *about*,
never whether it is *acceptable*.

## Phase 1 — intent, and the one rule that makes it safe

The scope gap in `gonk-066` is that "which files are in scope for this bug" has
to come from somewhere, and today it comes from nowhere. A model reading the
issue is a reasonable source. It becomes dangerous the moment its output can
grant rather than restrict.

**The invariant: a model may only NARROW the envelope, never widen it. Its
output INTERSECTS with the statically-configured envelope; it never UNIONS onto
it.**

- Safe: intent proposes `src/auth/**`, the broker freezes it, the batch is
  rejected if it touches anything else. Narrow it wrongly and the run fails
  closed — a re-sling, no damage.
- Fatal: intent proposes "this one needs a CI change, allow `.gitlab-ci.yml`".
  Now the untrusted issue body chooses its own permissions and every
  deterministic gate downstream is decorative.

Exhaust deterministic sources first — issue labels, linked MRs, CODEOWNERS for
the touched area, git history, and `.agent/` itself, which exists precisely to
state repo conventions as fact rather than inference. Reach for the model for
what those cannot supply.

And note what phase 1 does **not** do: it does not pick a rung. Spec 6.3's "no
upfront complexity classification" stands. Everything starts at the cheapest
rung and earns escalation by failing objective gates, because a classifier that
sets the starting rung lets whoever writes the issue text choose how much money
gonk spends.

## Phase 3 — the red-green check, and why it is the good idea

The session returns **two diffs**: one that adds tests, one that changes code. A
non-agentic verifier applies them in sequence against a checkout of the source
commit:

| step | tree | required result |
|---|---|---|
| 0 | base commit alone | **GREEN** |
| 1 | base + test diff | **RED**, and red *in the new tests specifically* |
| 2 | base + test diff + code diff | **GREEN** |

**This converts "did the agent actually fix the bug" from a judgement into an
experiment.** Step 1 proves the test exercises the defect; step 2 proves the
code fixes it.

**It also closes the CI example from `gonk-066` — completely, and by
construction.** An agent that deletes or weakens a test to turn a pipeline green
cannot produce a red→green transition, because removing a test makes step 1
*green*, and step 1 must be red. The failure mode that could not be judged
becomes a state the sequence simply cannot reach. That is a much stronger result
than the protected-path denylist, which only says "not that file"; this says
"not that *behaviour*, wherever you attempt it".

### Step 0 is not optional

Without it, red→green is meaningless: a repository whose suite is already
failing is red in step 1 for reasons that have nothing to do with the change,
and "green" in step 2 could be the agent fixing an unrelated pre-existing
breakage. Step 0 also gives an honest answer for repos gonk should not be
writing to yet.

### "Red" must be red for the right reason

A test diff that does not compile is also red, and an agent that writes a broken
test then "fixes" it in the code diff produces a perfect red→green trace having
fixed nothing. So step 1 must distinguish **assertion failure** from **build or
collection error**, and must assert the *newly added tests* are the ones failing
— not merely that the suite is non-green. A pre-existing unrelated failure
otherwise satisfies "red" for free.

### The split diff needs its own path gate

Nothing stops an agent putting production code in the test diff. The test diff
must be constrained to files matching the repository's declared test patterns —
and *where those patterns are declared* is `.agent/`, which is the second place
this design leans on scaffold's output being real.

### Flakiness is a correctness problem here, not an annoyance

A flaky suite makes the gate nondeterministic, and the tempting fix — retry
until green — silently converts "flaky" into "pass". Retries must be bounded and
the flake must be *recorded on the bead* as its own outcome class, distinct from
both pass and fail.

## Phase 3 — the non-code effects

The owner's list: comment on a work item or thread, add/remove a label, assign a
reviewer, approve for merge, rebase onto a SHA, close the issue. These extend the
effects contract the same way `file` did. They are not equally risky, and the
shape gate should not treat them as though they are:

| effect | risk | deterministic control |
|---|---|---|
| comment, label | low | already shipped |
| rebase onto SHA | low | mechanical; the SHA must be an ancestor of the target branch |
| assign reviewer | medium | allowlist from project membership; never self-assign |
| close issue | medium | only with a linked, merged MR — closing without fixing is a failure wearing a success |
| **approve for merge** | **do not build** | see below, incl. the larger-reviewer variant |

**Approve-for-merge should not exist, and this is the one item in the list I would
argue against.** Every safety property in this design rests on the merge request
being a human review boundary — it is what makes "the model proposed something
wrong" survivable. An agent that can approve has removed the only control that
does not depend on a validator being complete. Even restricted to "never approve
your own MR", it means one agent's output can authorise another's, and the two
share a failure mode: they are the same model reading the same untrusted text.

If the goal is *unattended* merges for a trusted class of change, the honest
mechanism is a **deterministic auto-merge policy owned by the operator** — e.g.
"scaffold MRs touching only `.agent/`, with step 0/1/2 green, may auto-merge" —
expressed in `.gonk.yml` and enforced by the controller. That is a rule the
operator wrote, not a judgement an agent made, and it can be reasoned about
without trusting a model at all.

### "Could a larger model approve a smaller model's work?"

Raised by the owner, and it defeats one of the objections above: the capability
gradient means this is *not* the same model reading the same text, so "they
share a failure mode" does not apply as stated. A larger model genuinely does
catch competence errors a smaller one makes. That much is real and is the
premise of a lot of working practice.

It still does not rescue *approval*, for three reasons the gradient does not
touch:

1. **Capability is not independence.** What a reviewer must provide is
   uncorrelated failure, not a higher score. Models with overlapping training
   data and shared lineage fail in correlated *classes*, and the errors that
   pass both reviewers are exactly the correlated ones. Scaling raises the
   competence floor; it does not buy independence.
2. **The reviewer reads attacker-controlled text too** — the issue body, the
   diff, the comments in it. Injection aimed at a reviewer is a live attack, and
   "larger" is not monotonically more resistant: a model that follows
   instructions more faithfully can be more steerable, not less. The gradient
   does not reliably run the right way on the one axis that matters.
3. **It inverts the cost model.** If the approver must be the expensive model,
   every merge pays cloud-rung prices in a system whose ladder exists to ration
   exactly that. A control that is costly to run attracts downward pressure, and
   the day someone drops the approver a rung to save money the safety argument
   evaporates with nothing failing.

**What to build instead: a large model as a reviewer that CANNOT merge.** The
asymmetry is the same invariant this design has now reached three times —
intent may narrow but not widen, protected paths may subtract but not add,
review may **veto but not authorise**. A false veto costs a re-sling; a false
approval merges bad code. So let the larger model fail the gate deterministically
and post its reasoning as a comment (both are effects that exist or are cheap),
and the capability gradient delivers its real benefit without holding the one
control that does not depend on a validator being complete.

Stated plainly: **if you want unattended merges, buy them with the verifier, not
with a bigger reviewer.** Step 0/1/2 green is evidence — reproducible,
inspectable, fails closed. An approval is an opinion about text an attacker
helped write.

## The verifier is the most dangerous component in the system

It runs attacker-influenced code, by design. That is its whole job, and it
deserves to be treated as hostile:

- **No credentials.** No bot PAT, no LiteLLM key — it is non-agentic and needs no
  model, so it should be the only component in gonk holding nothing at all.
- **No network**, or a denylist-by-default egress policy. A test suite that
  phones home is exfiltration with a green tick.
- **Hard resource and wall-clock caps**, killed and recorded as infra-failed
  rather than gate-failed, so a hang cannot escalate a bead onto a pricier rung.
- **Fresh sandbox per run**, never reused across projects.

Note this is a *different* threat model from the agent pod. The agent pod runs a
model over untrusted text; the verifier runs untrusted *code*. Today's
NetworkPolicy is unenforced (Flannel; Cilium suspended) and the owner has a CNI
plan — the verifier is the component that makes that plan load-bearing rather
than advisory.

## Where the diff comes from

The agent holds no git credentials, consistent with everything else. Two options:

1. **Diff as an effect** — the agent emits patch text in the batch. Consistent
   with the existing contract, inspectable, bounded by the batch size cap; large
   changes strain it.
2. **Read the rig** — the controller execs `git diff` in the session pod. No
   size limit, but it puts the controller back into the pod and re-introduces a
   read path that the broker design otherwise avoids.

(1) is the one that fits. The size cap is a real constraint and probably an
acceptable one: a change too large to express as a bounded patch is a change that
wants a human anyway.

## What this does not solve

- **Scope is still the model's proposal.** Freezing it makes it enforceable, not
  correct. An intent phase that consistently proposes too-narrow scopes produces
  a stream of runs failing closed; too-wide, and it authorises nothing it
  shouldn't but constrains nothing either.
- **A test can be green and the change still wrong.** Red→green proves the
  defect is exercised and fixed. It says nothing about whether the fix is any
  good, or whether it broke something no test covers. The MR is still the answer.
- **Cost.** Phase 1 and phase 3 are both spend on every run. For triage on a
  local rung they almost certainly cost more than they save; for a multi-turn
  feature agent they plausibly pay for themselves. Measure before assuming.

## Sequencing

Nothing here is buildable until there is a code-writing agent, and the pieces
have a natural order:

1. The **verifier** first, and alone. It is useful on its own — it can gate
   scaffold MRs and any human MR — and it is the component that needs the most
   hardening, so it should exist before anything depends on it.
2. The **split-diff effect kinds** and their path gates.
3. The **red-green sequence**, once there is something producing split diffs.
4. The **intent phase** last: it is the only part with a model in it, and it is
   worthless until there is a scope for it to narrow.
