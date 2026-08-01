# Pitch: getting agent prompt delivery off the keystroke channel

_2026-08-01. For evaluation. Beads `gonk-e9m` (P1), `gonk-m6t` (P2)._
_Full design: [`../specs/2026-08-01-prompt-by-reference-design.md`](../specs/2026-08-01-prompt-by-reference-design.md).
Plan: [`../plans/2026-08-01-prompt-by-reference-implementation.md`](../plans/2026-08-01-prompt-by-reference-implementation.md)._

## 1. Context, in one paragraph

gonk is a controller that triages GitLab issues with LLM agents. A GitLab
webhook reaches `gonk-intake`; `gonk-gate dispatch` asks `gonk-meter` for a
budget decision, renders a prompt containing the issue's title and body, and
creates an agent session through **Gas City** (`gascity`), a third-party
orchestrator we do not control. Gas City runs each session as a **Kubernetes
pod** running `opencode` (an interactive terminal agent) inside `tmux`. The
agent emits a JSON "proposed effects" batch between sentinels; `gonk-gate sweep`
reads it back, validates its shape deterministically, and applies the comment
and labels under the controller's own credentials. **The agent pod holds no
forge credentials** — that is the core of the security design. Its only secret
is a LiteLLM virtual key.

## 2. The problem

Gas City delivers a message to a session by typing it into the pane:

```go
// gascity internal/runtime/carrier.go:83-87
if _, err := c.tmux(ctx, name, "send-keys", "-t", c.target, "-l", message); err != nil {
    return err
}
_, err := c.tmux(ctx, name, "send-keys", "-t", c.target, "Enter")
```

`-l` stops *tmux* interpreting the text as key names. It cannot stop the
**receiving application** interpreting it. opencode's composer treats `!` as its
shell-mode trigger, so a prompt containing a bang is not asked — it is executed.

Reproduced live, 2026-08-01, against a healthy session:

```
submitted: Hello there, see issue !42 and reply with exactly the word GOLF and nothing else.
rendered:  $ Hello there, see issue 42 and reply with exactly the word GOLF and nothing else.
produced:  /bin/sh: 1: Hello: not found
```

The model never saw it. The API reported success (`request.result.session.submit`,
`queued=false`). Control cases — multi-line text, backticks, JSON braces — each
pass through correctly, so the bang alone is the trigger, at any position.

This wedged every triage session gonk ever ran: the prompt began
`<!-- gonk:model:… -->` and referenced `issue !18`, so the whole prompt ran as a
shell command while opencode sat on a spinner.

**Why this is not just a bug.** The prompt embeds an **untrusted GitLab issue
title and body**. gonk hands attacker-influenced text to a component that types
it into a terminal where the receiving TUI decides what is a command. We ship a
mitigation today — strip `!` before submitting — but:

- the trigger set belongs to the TUI, not to us, and changes when the agent
  binary is upgraded, silently;
- it is per-provider, so every caller must know the input grammar of every agent
  CLI anyone might configure;
- the failure is invisible: the session looks alive and the transport reports
  success.

A second cost: the prompt used to carry `<!-- gonk:model:… -->` /
`<!-- gonk:meta:… -->` marker lines that our entrypoint parsed to select the
model and stamp per-bead spend attribution. Those are gone (they contain the
fatal `!`, and see §3), so **LiteLLM spend rows are now attributed per-install
rather than per-bead** — a regression against a stated project goal.

## 3. Constraints — what is actually available

All verified against the live cluster or `gascity@4fda5a28…` on 2026-08-01.

| candidate channel | verdict |
|---|---|
| create-time prompt (`PromptSuffix`) | **Dead upstream.** The k8s provider never references `PromptSuffix`/`PromptFlag` — `grep -c` returns 0 for every file in `internal/runtime/k8s/`. Reported as gastownhall/gascity#4891. |
| session-create `options` | **Dead.** `ProviderOption` is `type:"select"` only; `ResolveExplicitOptions` rejects any value outside the declared `choices`. Arbitrary text cannot ride it. |
| file staging into the pod | **Dead.** `Provider.Start` guards all staging with `if !p.prebaked`; gonk runs `GC_K8S_PREBAKED=true` (settled separately). |
| per-session env | **Dead.** `SessionCreateBody` has no env field; `resolved.Env` is static agent/city config. |
| **the pod's own identity** | **LIVE.** `GC_ALIAS` is in the pod env, and the alias is **caller-chosen** — gonk mints it. |
| **agent → gonk-meter egress** | **LIVE.** Already permitted by the agent NetworkPolicy. |

Two further facts that shape the answer:

- The agent NetworkPolicy permits egress to **DNS, LiteLLM, GitLab and
  gonk-meter only** — *not* the Gas City API. ~~A session cannot enumerate other
  sessions.~~ **← FALSE, see the evaluation, note 1.** The policy is written and
  **unenforced** (Flannel implements no NetworkPolicy; Cilium is suspended). Left
  in place because the evaluation critiques this exact sentence; the design's
  justification has been rebuilt without it.
- `gonk-meter`'s API is bearer-authenticated with **one admin token**, which
  also unlocks `PUT/DELETE /v1/projects/{project}`, `POST /v1/policy/decide` and
  `POST /admin/spend/sync`.

## 4. The proposal

**Prompt-by-reference, with the key as the capability.**

1. `gonk-gate dispatch` renders the prompt and `PUT`s it to `gonk-meter` keyed
   by alias, **before** creating the session (the pod can be running before the
   create call returns).
2. The alias carries **128 bits of base32 entropy**:
   `gonk.triage.p75.i18.a1.k7m2q9x4nf3bt8wc5rj6ydzp1h`. `ValidateAlias` accepts
   `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$` up to 64 chars; we use 22 today, so this fits.
3. The pod starts. `gonk-agent-entrypoint` (**ours**) fetches
   `GET /v1/prompt/$GC_ALIAS` and hands the prompt to opencode **as an
   argument**, not as keystrokes. The row also carries `model` and `metadata`,
   restoring per-bead attribution.
4. The fetch is **one-shot** (second read is `410`) with a TTL.
5. `fetched_at` on the row is a **delivery receipt from the consumer** — the
   controller learns the prompt actually landed, which nothing in the current
   architecture can tell it.

**Holding the alias is the authorisation. No credential is distributed to agent
pods at all.** This was the deliberate choice over a scoped bearer token:
because the meter's admin token also controls project registration and policy
decisions, any credential in the agent pod must be proven strictly weaker
route-by-route, and an agent that can rewrite project registrations can rewrite
its own budget. Not shipping a credential beats scoping one correctly.

Rejected alternatives, with evidence, so they are not re-litigated:

- **Session id** (`go-4m4`) — short, low entropy, and gonk does not have it when
  it stores the prompt (agent create is async and returns no id).
- **`GC_INSTANCE_TOKEN`** — a real 128-bit `crypto/rand` per-incarnation secret
  in the pod env, but **no API route exposes it**, so the controller can never
  learn it to pre-authorise against.
- **Push by `kubectl exec`** — needs pod/exec RBAC and reimplements pod
  discovery and readiness that the provider already owns.
- **A separate `gonk-postbox` service** — new Deployment, Service, NetworkPolicy,
  values schema and dashboard for the same guarantee.

## 5. Known weaknesses — argue with these

Stated deliberately, because a pitch that hides them is not evaluable.

1. **The capability is a secret in a *name*.** The alias reaches gonk's logs,
   Gas City events, session lists and bead titles. It defends against a peer
   agent pod. It does **not** defend against an operator, a log aggregator, or
   anything with control-plane read access. Judged acceptable for a
   single-tenant install where the operator can already read the issue text;
   explicitly *not* acceptable multi-tenant.
   **← INCOMPLETE, see the evaluation, notes 1 and 4.** The list omits the pod's
   k8s label and `.beads/issues.jsonl` (committed and pushed), and the "defends
   against a peer agent pod" premise depends on an unenforced NetworkPolicy. The
   defence that actually holds is time-bounding — the capability is spent before
   untrusted text ever runs — and it is now the design's stated argument.
2. **It makes one meter route unauthenticated.** `GET /v1/prompt/{alias}` cannot
   require the admin bearer (the pod has no credential), so it joins
   `/healthz`, `/readyz`, `/metrics` in the auth-exempt set — on a service that
   otherwise guards project registration. Mitigations planned: exemption scoped
   to that method *and* path; refuse an alias with no nonce; one-shot.
3. **It routes around #4891 rather than fixing it.** If upstream ever composes
   `PromptSuffix`, there are two delivery paths and dispatch must not use both.
4. **We do not control the agent pod spec**, so the stronger identity option
   (the pod's projected ServiceAccount token, whose claims carry a real per-pod
   UUID, verified via TokenReview) cannot be given a custom audience — we would
   be accepting API-server-audience tokens with a one-year lifetime. That is why
   it is the documented upgrade path, not the proposal.
5. **The prompt still reaches opencode as an argument**, so we depend on
   argument handling being inert where keystroke handling was not. Believed
   sound (it is how every other Gas City provider delivers), but it is an
   assumption about someone else's binary. The stronger form — `opencode run`,
   non-interactive, no composer at all — is sequenced *after* this, because
   sweep currently reads output by capturing the live tmux pane and that read
   path would have to move at the same time.

## 6. What we are asking

Evaluate the proposal in §4 against the problem in §2 and the constraints in §3.

Constraints are load-bearing: we do **not** control Gas City or opencode, and
upstream fixes are not available on this timescale. A proposal that requires
changing either is not actionable here, however correct.

---

## Evaluation — Fable, 2026-08-01

**VERDICT: APPROVED WITH NOTES.**

Fable independently re-verified every "dead channel" claim in §3 against the
pinned gascity clone and confirmed all of them, plus the gonk-side claims
(`bearerAuth` switching on path alone, the single `brokerSessionAlias` call
site, sweep resolving via `Record.SessionID`). It judged that the proposal
solves the stated problem rather than relocating it, and found no missed channel
— it specifically hunted the create-body `Message`, `Title`/`ProjectID` free
text, LiteLLM key metadata, GitLab-as-mailbox and `GC_WEBHOOK_ARG_*`.

It also contributed the argument this design should have led with, which the
author had not articulated: **the capability is already spent by the time
untrusted text first executes in the model**, since the entrypoint consumes the
row before opencode starts. Exfiltrating `GC_ALIAS` via prompt injection yields
a dead key. That defence does not depend on the CNI — which matters, given note 1.

### Notes, and what was done about each

1. **The peer-pod isolation claim was wrong.** The pitch asserted "a session
   cannot enumerate other sessions" on the strength of the agent NetworkPolicy.
   That policy is *written and unenforced*: the chart's own banner says Flannel
   does not implement NetworkPolicy and Cilium is suspended, and Gas City's
   read-auth is opt-in. **Confirmed and corrected** — the design's justification
   is rebuilt on the time-bounding argument, which holds without the CNI.
2. **Front-load the argument-inertness spike.** The plan rests on "arguments are
   inert" but only tested it at T6, after five tasks. If opencode seeds its
   composer from `--prompt`, the sequencing inverts. **Added as T0**, before
   anything is built.
3. **Respawn × one-shot.** Gas City relaunches a dead agent into the warm pod;
   after any crash past the fetch the entrypoint gets `410` and churns until
   sweep reaps. **Added to T4**: distinct exit codes and diagnostics for `410`
   (consumed) versus `404` timeout (never delivered).
4. **Two leak surfaces missing.** The alias lands verbatim in the pod's k8s
   label, and in `.beads/issues.jsonl`, **which is committed and pushed** —
   verified empirically; aliases from this session are already in the tracked
   file. **Both added**, with the time-bounding argument stated explicitly as
   what makes a leaked *spent* capability acceptable.
5. **`fetched_at` proves the fetch, not that opencode accepted the argument.**
   **Added to T5**: keep the session-health checks; the receipt is not terminal
   success.
6. **Make the nonce check a strict format check** (26 base32 chars) and run it
   before touching the store, so an unauthenticated route is not a free database
   lookup. **Added to T2.**

Citation fix: the `send-keys` pair is `carrier.go:83-87`, not `74-88`.
