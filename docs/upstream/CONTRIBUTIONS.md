# Upstream contributions ledger

Bugs gonk found in its dependencies, and what happened to them.

## Why this file exists

Two reasons, one practical and one about accountability.

**Practical.** Upstream reports are posted by the human owner, in their own
words — not by the agent that found the bug. Some maintainers reject
agentically-generated issues and PRs out of hand, and an agentically-*augmented*
report from a human who has read and vouched for it is the version that actually
gets a response and a fix. The point is the upstream project ending up better,
so how the report lands is part of the work, not a formality.

That means the person posting needs to know **which claims were verified against
source and which were inferred**, so they can stand behind the report under their
own name. That is recorded here per contribution.

**Accountability.** The finding work and the posting are done by different
parties, and the posting party carries the reputational risk. Recording who
found what — and how well it was checked — keeps that honest in both directions,
and leaves a record to settle up from later if norms around agent contributions
change.

## Conventions

- **Found by** — the session/agent that located it, and the gonk bead tracking it.
- **Posted by** — whoever actually filed it upstream. Never the agent.
- **Verified** — claims checked directly against upstream source at a pinned
  ref, with file:line. These the poster can defend.
- **Inferred** — reasoning not directly confirmed. Flagged so it can be softened
  or cut before posting.

---

## gastownhall/gascity#4891 — k8s runtime provider never delivers PromptSuffix/PromptFlag

- **Found by**: Claude Code session, 2026-07-31. Bead `gonk-drf`; root cause on `gonk-u1p.1`.
- **Posted by**: ⚠️ **the agent, in error.** The owner's standing preference is to
  review and post upstream reports themselves; this was filed before that was
  known. The owner added a clarifying comment to the issue and chose to **leave
  it as filed** — the content is verified and the maintainers have seen it, so
  refiling would be noise. Recorded here rather than quietly fixed, because the
  whole point of the preference is that provenance stays visible.
- **Impact on gonk**: total. No gonk agent had ever received a prompt; sessions
  booted at opencode's idle splash and never finished. Worked around in
  `pkg/gcapi/submit.go` by delivering the prompt as a second signed call.

**Verified** against `4fda5a28445f42d6e789fc7f5751645ac4fecd19`:

| claim | evidence |
|---|---|
| `PromptSuffix` is contractually appended to `Command` | `internal/runtime/runtime.go:660` |
| `initial_message` is routed onto it | `cmd/gc/session_lifecycle_parallel.go:1100` |
| tmux composes it, with a prompt-file path over 1024 chars | `internal/runtime/tmux/adapter.go:1391,1413` |
| acp composes it | `internal/runtime/acp/acp.go:161` |
| k8s never references either field | `grep -c` returns 0 for **every** file in `internal/runtime/k8s/` |
| k8s encodes `cfg.Command` verbatim | `internal/runtime/k8s/pod.go:63` |
| conformance matrix has no k8s profile | `cmd/gc/template_resolve_phase2_test.go` — 8 `tmux-cli` refs, 0 `k8s` |
| `WC-INPUT-001` asserts at the `runtime.Config` layer | `cmd/gc/session_lifecycle_parallel_phase2_test.go` |
| k8s declares no prompt capability | `internal/runtime/k8s/provider.go:661` |

Plus a live reproduction: the session pod's command decoded to the bare
`start_command` with no arguments.

**Inferred, not verified**
- That `Provider.CopyTo` is a workable way to land a prompt file in the pod. It
  exists and does tar a file to `/workspace`, but no one has built the fix on it.
- That the omission is an oversight rather than a deliberate constraint. Nothing
  documents it either way; `Capabilities()` simply doesn't mention prompts.

**Overreach to avoid repeating**: the filed body ended "Happy to send a PR if the
approach looks right." That commits the *owner* to work. An agent drafting for a
human to post must not offer labour on their behalf. Both 2026-08-01 drafts were
checked against this rule.

### Re-checked 2026-08-01, still correct

Session 5 overturned a *different* gonk claim (see below) and, in the process,
re-verified this one from scratch at the same ref. All three load-bearing claims
hold: zero `PromptSuffix`/`PromptFlag` hits anywhere in `internal/runtime/k8s/`,
`agentCommandB64` still encodes `cfg.Command` verbatim, `Capabilities()` still
declares only `CanReportActivity`. **Nothing in #4891 needs correcting.**

**New evidence worth adding as a comment** — the system stamps a marker
asserting the prompt *was* delivered, on the path where it is dropped:

| claim | evidence |
|---|---|
| `promptDelivery` sets `Delivered=true` for a flag-mode provider purely because a prompt flag is configured | `cmd/gc/prompt_delivery.go:48-55` |
| that flag stamps `GC_STARTUP_PROMPT_DELIVERED=1` into the session env | `cmd/gc/template_resolve.go:829-838` |
| a consumer trusts it to suppress re-delivery (hook path) | `cmd/gc/cmd_prime.go:492` |
| **live**: an agent pod that never received a prompt had it set | `GC_STARTUP_PROMPT_DELIVERED=1` in `s-go-d3y`'s env, opencode idle at its splash |

So the marker records a *routing decision*, not a delivery. On k8s it is
asserting something false — which is why this failed silently for so long rather
than surfacing anywhere. (The `cmd_prime.go` consumer is gated on the managed
hook + `SessionStart`, so it does not bite gonk; flagged as the reason a false
marker is not merely cosmetic.)

---

## gastownhall/gascity — `Provider.Nudge`/`SendKeys` discard the carrier error (NOT POSTED, and the framing must change)

- **Found by**: session 4. Recorded on `gonk-u1p.7`, which proposed adding it to
  #4891 or filing a second report. **It was never posted — and it is as well.**
- **Status**: the code fact is real; the causal story attached to it was wrong.

`internal/runtime/k8s/provider.go:535` does discard the carrier's error and
return `nil` unconditionally, and `SendKeys` (line 541) does the same. That is a
genuine latent bug: a delivery failure is unreportable.

**But session 4 inferred from it that prompt delivery was failing there, and it
was not.** Proven live 2026-08-01: running the carrier's exact two-step
`send-keys` by hand typed into the live opencode TUI and the model answered. The
real defect was in gonk's own `deliverPrompt` (`gonk-u1p.7`, fixed). Had this
been posted as filed, it would have asserted a failure mode we had not observed
and blamed upstream for our bug.

**If it is ever posted, it must be reframed** as what it is — an unreportable
error path — with no claim about what it causes, and paired with the sharper
finding it is actually adjacent to: `Nudge` delivers text as *keystrokes* into
whatever TUI occupies the pane, which is a text-injection boundary for any
TUI-backed provider (`gonk-e9m`).

**Lesson recorded rather than quietly dropped**: the verified/inferred split in
this ledger did its job on #4891 and was not applied to the follow-on claim. An
inference about *cause* deserves the same treatment as a claim about source —
and cause is the one that needs a live reproduction, not a grep.

---

## gascity — session messages are delivered as keystrokes into a TUI (DRAFTED, NOT POSTED)

- **Found by**: session 5, 2026-08-01. Bead `gonk-e9m` (P1).
- **Posted by**: nobody yet. Draft:
  [`gascity-keystroke-delivery-injection.md`](gascity-keystroke-delivery-injection.md).
- **Impact on gonk**: every triage session wedged. The prompt embeds untrusted
  GitLab issue text, so this is a text-injection boundary, not just a bug.
  Mitigated in gonk by stripping bangs; the durable fix is
  [prompt-by-reference](../superpowers/specs/2026-08-01-prompt-by-reference-design.md).
- Carries its own evidence (source at the pinned ref + a live reproduction) and
  is the stronger of the two drafts. The remediation suggestions in it are
  **inferred** and flagged as such — bracketed paste is untested.

## opencode — a bang anywhere in delivered input switches to shell mode (DRAFTED, NOT POSTED)

- **Found by**: session 5, 2026-08-01. Bead `gonk-e9m`.
- **Posted by**: nobody yet. Draft:
  [`opencode-shell-mode-on-programmatic-input.md`](opencode-shell-mode-on-programmatic-input.md).
- **Weaker draft, deliberately.** It is a behaviour report from an unusual
  integration, not a defect we can prove: a leading-`!` shell escape is a normal
  TUI affordance, and we did not read opencode's input handling to learn which
  rule was intended. **Check the behaviour against current opencode before
  posting** — we pin 1.18.3 and did not verify `latest`.

---

## litellm — unbounded `/spend/logs` OOM DoS

- **Found by**: earlier session. Bead `gonk-wgq` (open).
- **Posted by**: not yet posted.
- Write-up: [`litellm-spend-logs-unbounded-dos.md`](litellm-spend-logs-unbounded-dos.md).
- Found while verifying gonk's attribution chain against a live LiteLLM v1.92.0;
  an unbounded query OOM-killed it.
