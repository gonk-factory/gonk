# Plan: the agent pod lifecycle is legible from `kubectl logs`

_2026-09-07. Owner's direction: log every major branch and event of the agent
pod lifecycle to pid 1's stdout, publish in the spec that we do this and that it
is where you look, and never put an unredacted secret there._

## Why

Gas City launches the agent as `tmux new-session -d ... && sleep infinity`. tmux
detaches, so the entrypoint's stderr reaches the tmux pane and **nothing reaches
the container log**. `kubectl logs` on a session pod has always been empty, and
that emptiness reads as "nothing happened" rather than "you cannot see what
happened" (`gonk-dot`).

Every failure in this class looks identical from outside — *session created,
transcript empty* — while having completely different causes: no key, no prompt,
no model, no checkout, a dead tmux, or a pod that never ran. The entrypoint knows
which one it hit every time. It just had no way to say so.

`log()` already tees to `/proc/1/fd/1`. This plan makes the SET of things it says
complete, and writes the guarantee down.

## Items

1. **A session-start line, first thing.** Identity an operator can correlate:
   alias, agent, attempt, and whether a checkout/prompt/key was expected.
   Nothing useful today precedes the first branch.
2. **Every branch logs which way it went**, including the quiet success paths.
   Specifically the ones that are currently silent when they succeed or are
   skipped: checkout skipped (no rig base URL), prompt fetch started, model
   source chosen (marker vs arg vs static), metadata present.
3. **A terminal line on every exit path**, including the refusals, so a reader
   can tell "ended" from "still going". Today several `exit 1` paths log a reason
   but nothing marks the end.
4. **No unredacted secrets, ever, and a test that proves it.** Key VALUES, bot
   tokens and prompt bodies must never be logged. Paths, lengths, byte counts and
   which-source-was-used are fine and are what make it debuggable.
5. **Publish it in the spec**: that gonk logs the agent lifecycle to the pod's
   stdout, that `kubectl logs <session-pod>` is where you look, and what you can
   expect to find. A guarantee nobody documented is not a guarantee.

## Verification

Per `CLAUDE.md`, this plan is checked by a FRESH agent against the numbered items
above, not by the session that wrote it. For each item: what is the evidence, and
where does it live? Items with no evidence are not done.

Specific checks worth naming:
- Grep every `log` call for anything that could interpolate a credential.
- Confirm the redaction test actually fails when a secret is logged (negative
  control), rather than passing vacuously.
- Confirm the spec text matches what the code does, not what this plan intends.
