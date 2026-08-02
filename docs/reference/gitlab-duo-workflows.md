# Reference: GitLab Duo's workflow architecture

_Researched 2026-07-19; written up 2026-08-02 because it existed only in a
conversation and was invisible to anyone reading the repo. Not a design
document — this is what the reference implementation does, kept so we can be
deliberate about where gonk agrees and where it diverges._

## What Duo ships

**~10 "foundational flows"**, several of which map onto gonk's roadmap almost
one-to-one:

| Duo flow | gonk equivalent |
|---|---|
| Developer (issues → MRs) | "contribute code in a branch / MR" (`actions.features`) |
| Fix CI/CD Pipeline | "fix broken pipeline" (`actions.pipelines`) |
| Code Review | the reviewer discussed in the verified-change-pipeline note |
| Security Review, Agentic SAST resolution, breaking-change resolution | not on gonk's roadmap |

**Agents vs flows** is Duo's top-level split, and it is a useful one: *agents*
are interactive and synchronous (chat); *flows* are autonomous, multi-agent and
asynchronous, running on CI/CD compute. Everything gonk does is a flow.

## The decomposition that matters: two component types

Custom flows are **YAML against a "flow registry v1" spec** with top-level
fields `version, environment, components, prompts, routers, flow`. The component
types are the whole point:

- **`AgentComponent`** — agentic, LLM-driven.
- **`OneOffComponent`** — deterministic. Scripts, API calls, no model.

**A flow is an explicit interleaving of the two.** That is the same conclusion
gonk reached from the other direction — the effects contract exists precisely so
the model proposes and deterministic code applies — but Duo makes it a
first-class authoring primitive rather than an architectural principle. Worth
noting that gonk currently has no vocabulary for "this step is deterministic";
it is implicit in which binary runs.

**Composition is sequential, state-machine style, via `routers`** — explicitly
*not* a DAG. Under the hood the service is Python gRPC on LangGraph, and it
**checkpoints workflow state back to GitLab**.

**Triggers are events, never LLM decisions**: chat/UI, GitLab events (issue/MR/
epic mentions, assignments, MR ready/conflict/approval, work-item and pipeline
events), or the REST Flows API. gonk's `intake.Decide()` is the same idea.

**Execution is configured, not inferred** — `.gitlab/duo/agent-config.yml`
carries `image`, `setup_script`, `cache`, `network_policy`, `id_tokens`. Note
`network_policy` in particular: Duo treats the sandbox's egress as part of the
flow definition. Directly relevant to hardening gonk's verifier.

## The owner's synthesis at the time (2026-07-19)

Recorded because today's design work rediscovered most of it independently, and
one piece of it we have NOT yet built:

1. **A planning stage that classifies the work** — feature request vs bug vs
   infra — and determines complexity shape before anything is implemented.
2. **Implementation validated against the plan**: an adversarially-prompted
   code-review agent compares the implementor's output to the original plan and
   flags divergence.
3. **Deterministic gates BETWEEN stages.** The load-bearing example: *"an issue
   that turns out to be a feature request should not result in code changes; a
   CI failure that looks like infrastructure should not result in infrastructure
   changes"* — instead they generate issues or follow-up work.
4. LLM output is not individually trusted without deterministic
   testing/validation.
5. Git hosted inside the pod so the broker controls commits, branches and pushes
   **based on validations passing**.

Point 5 is now built: the broker commits and opens the MR, and the agent holds
no forge credentials. Point 4 is the verified-change-pipeline design note.

## Where this lands on the current roadmap

**Point 3 is the piece we have not designed and should.** It is a *routing*
gate, and it is the sharpest answer available to the danger in "fix broken
pipeline": an agent told to make CI green has deleting the failing test as a
locally optimal move. Classifying the failure first — and routing an
infrastructure diagnosis to *file an issue* rather than to *write a diff* —
removes the perverse objective instead of policing its output. The red-green
monotonicity check is the backstop; this is the thing that means it rarely has
to fire.

It also fits the "narrow, never widen" invariant cleanly: a classifier choosing
between "produce a diff" and "produce an issue" is selecting a *smaller*
capability set, never granting one.

**One genuine divergence to keep deliberate.** The 2026-07-19 synthesis has a
review agent that can *approve*; the verified-change-pipeline note argues a
model may veto but never authorise. These are reconcilable — "approve this batch
for application, behind a deterministic gate" is not the same as "approve this
MR for merge into the default branch" — but the distinction is exactly the sort
that erodes if it is not written down. It is written down here.

**Not adopted, and worth saying why.** Duo's `routers`/state-machine
composition and LangGraph runtime are a heavier orchestration model than gonk
needs: gonk's flows are short, its state lives on beads, and its composition is
a controller loop. The idea worth stealing is the *typed distinction between
agentic and deterministic steps*, not the machinery for expressing it.
