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
  known. The owner added a clarifying comment to the issue. Recorded here rather
  than quietly fixed, because the whole point of the preference is that
  provenance is visible.
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
human to post must not offer labour on their behalf.

---

## litellm — unbounded `/spend/logs` OOM DoS

- **Found by**: earlier session. Bead `gonk-wgq` (open).
- **Posted by**: not yet posted.
- Write-up: [`litellm-spend-logs-unbounded-dos.md`](litellm-spend-logs-unbounded-dos.md).
- Found while verifying gonk's attribution chain against a live LiteLLM v1.92.0;
  an unbounded query OOM-killed it.
