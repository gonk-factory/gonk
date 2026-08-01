# Prompt-by-reference: getting prompt delivery off the keystroke channel

_Design, 2026-08-01. Beads: `gonk-e9m` (the injection boundary), `gonk-m6t`
(attribution). Supersedes the "option (c)" sketch on the closed `gonk-u1p.7`._

## The problem, stated exactly

Gas City delivers a session message with `tmux send-keys -l`
(`internal/runtime/carrier.go`) — literal **keystrokes into whatever TUI holds
the pane**. opencode's composer treats `!` as its shell-mode trigger, so a
prompt containing a bang is not asked, it is **executed**.

Proven live 2026-08-01 against session `go-57b`:

```
submitted: Hello there, see issue !42 and reply with exactly the word GOLF and nothing else.
rendered:  $ Hello there, see issue 42 and reply with exactly the word GOLF and nothing else.
produced:  /bin/sh: 1: Hello: not found
```

The model never saw the message. Multi-line text, backticks and JSON braces were
each tested separately and are fine; the bang alone does it, anywhere in the
string, not only at position 0.

gonk currently strips bangs before submitting
(`sanitizeForKeystrokeDelivery`). **That is a mitigation, not a fix.** The
prompt embeds an untrusted GitLab issue title and body, so gonk is handing
attacker-influenced text to a component that types it into a terminal where the
receiving TUI decides what is a command. Escaping at the sender cannot make that
safe: any other input-mode trigger opencode has — or grows in a future version
— is the same hole, and we would not know until it fired.

There is a second, quieter cost. The prompt used to carry
`<!-- gonk:model:… -->` and `<!-- gonk:meta:… -->` marker lines that
`gonk-agent-entrypoint` parsed out of its `--prompt` argument. On this backend
there **is** no such argument — the k8s provider never composes `PromptSuffix`
(gastownhall/gascity#4891) — so those markers were unreadable by construction,
and `<!--` contains the very bang that broke delivery. They are gone, and with
them per-bead attribution: LiteLLM spend rows are now attributed per-install
(`gonk-m6t`). Any fix for the channel should carry the metadata too.

## What is actually available (verified, not assumed)

Everything below was checked against the live cluster or gascity at
`GASCITY_REF=4fda5a28…` on 2026-08-01. **Do not re-derive these.**

| channel | verdict |
|---|---|
| session-create `options` | **Dead.** `ProviderOption` is `type: "select"` only; `ResolveExplicitOptions` rejects any value outside the declared `choices`. Arbitrary text cannot ride it. |
| file staging (`internal/runtime/k8s/staging.go`) | **Dead.** `Provider.Start` guards all staging with `if !p.prebaked`, and gonk runs `GC_K8S_PREBAKED=true` (settled, smoke U2). |
| per-session env | **Dead.** `SessionCreateBody` has no env field; `resolved.Env` is static agent/city config. |
| create-time `PromptSuffix` | **Dead upstream.** #4891. |
| **the pod's own identity** | **LIVE.** `GC_ALIAS=gonk.triage.p75.i17.a1` is in the agent pod's env, verified by `printenv` in a running pod. `GC_SESSION_ID`, `GC_INSTANCE_TOKEN` are there too. |
| **agent → gonk-meter egress** | **LIVE.** Already allowed by `networkpolicy-gonk-agent.yaml` on `.Values.meter.port`. No new network surface. |
| **static agent env** | **LIVE.** The chart's `bootstrap-city` initContainer rewrites each `/city/agents/*/agent.toml` `[env]` table, which is how `GONK_LITELLM_KEY` / `GONK_MODEL` reach the pod today. |

So the shape is forced: **the pod cannot be handed the prompt, but it can be
handed its own name.** Delivery must be by reference — gonk stores the prompt
keyed by alias, and the pod fetches it.

## The design

```
gonk-gate dispatch                          gonk-meter                agent pod
──────────────────                          ──────────                ─────────
  render prompt
  PUT /v1/prompt/{alias}  ─────────────────▶ store (durable, TTL)
      (BEFORE create — the pod may be up
       before the create call even returns)
  POST /sessions {alias}  ────────────────────────────────────────────▶ pod starts
                                                                        entrypoint:
                            store ◀───────── GET /v1/prompt/$GC_ALIAS ──  fetch
                                             (200 + delete, one-shot)
                                                                        exec opencode
  confirm delivery ◀───────  fetched_at  ──  (recorded on the row)
```

### Why gonk-meter and not a new service

It is already reachable from the agent pod, already the per-session authority
the agent talks to, and already has a durable store behind it
(`internal/meter/store`, Postgres in the shipped chart). A dedicated
`gonk-postbox` would mean a new Deployment, Service, NetworkPolicy rule, values
schema and dashboard for exactly the same guarantee. If prompt hand-off later
grows beyond a keyed blob, splitting it out is a cheap refactor; starting there
is not.

### The credential problem — the crux of this design

**gonk-meter's API is bearer-authenticated with one admin token**
(`internal/meter/service/http.go`, `bearerAuth`). That token also unlocks
`PUT/DELETE /v1/projects/{project}`, `POST /v1/policy/decide` and
`POST /admin/spend/sync`.

**The agent pod must never hold it.** An agent that can rewrite project
registrations can rewrite its own budget, which would make the meter decorative.
The pod holds no forge credentials by design (broker spec §9/10); its only
secret today is the LiteLLM virtual key.

So the prompt route needs its own, strictly weaker credential:

- **A separate token slot**, distinct from the admin bearer, valid **only** for
  `GET /v1/prompt/{alias}`. Every other route rejects it. This is a
  route-scoped principal, not a second copy of the admin token.
- Delivered to the pod the same way the LiteLLM key is: written into the
  agent's `[env]` by `bootstrap-city`, then **materialised by the entrypoint
  into a `0600` file and unset**, so it never reaches `ps`, `/proc/<pid>/environ`
  or a child process. The entrypoint already has this exact pattern.
- **It is a bearer token, so it is a namespace-wide reader.** Any agent pod can
  fetch any alias's prompt. That is acceptable for a single-tenant install and
  it must be written down rather than implied — see Open question 1 for the
  per-session capability that would close it.

### One-shot, TTL, and the confirmation signal

- **One-shot.** A successful `GET` returns the prompt and marks the row
  consumed. A second fetch is a `410 Gone`, not a replay. A prompt is delivered
  exactly once or the session is broken; there is no legitimate second reader.
- **TTL.** Rows expire (proposal: 30 min, ≥ the reservation window). An
  undelivered prompt must not sit in the store indefinitely holding issue text.
- **`fetched_at` is the delivery receipt gonk has been missing.** The whole of
  `gonk-u1p.7` was gonk unable to tell "delivered" from "dropped". Here the
  controller owns both ends: dispatch writes the row and can poll it until
  `fetched_at` is set. That is a **positive** confirmation from the consumer,
  strictly better than the event-log correlation we have now, which only reports
  that the supervisor typed something.

### What the entrypoint does

`images/agent/entrypoint.sh` is ours, so this half needs nothing from upstream:

1. Fetch `GET /v1/prompt/$GC_ALIAS` with the scoped token, retrying with backoff
   while the answer is `404` (the pod can legitimately beat the controller).
2. On success, hand the prompt to opencode **as an argument, not as keys**.
3. Restore the model/metadata seam: the row carries `model` and `metadata`
   alongside the prompt text, so the overlay is rendered from per-session values
   again and `gonk-m6t` closes with the same change.

**Failure must be loud.** If no prompt arrives within the window, the entrypoint
exits non-zero with a clear log rather than falling through to an idle splash.
An idle agent that looks healthy is the exact failure this whole line of work
exists to end. (`min_active_sessions = 0` on the triage agent, so every session
is gonk-created and a missing prompt is always a real error — there is no pool
session to be gentle about.)

### Interactive TUI or `opencode run`?

Holding the prompt at startup makes non-interactive mode possible for the first
time, and it is tempting for three reasons: no TUI means no composer, no
shell-mode trigger and **no input modes at all** — the injection boundary
disappears rather than being escaped. It also produces plain stdout instead of a
wrapped, box-drawn pane, which is far easier to parse a
`GONK_BATCH_START`/`END` fence out of, and it gives the session a real
lifecycle (`running` → `stopped`) instead of idling forever.

The catch is the read path. Sweep reads the fence from
`GetSessionOutput(peek)`, which is a `tmux capture-pane` of the live pane. If
opencode exits, the entrypoint exits, tmux dies and the pane — and possibly the
pod — goes away before sweep's next tick. **Do not couple the two changes.**
Recommended sequencing:

1. Land prompt-by-reference with opencode still in TUI mode. The keystroke
   channel is gone at that point, which is the security-relevant win, and the
   read path is untouched.
2. Then move to `opencode run`, with the output tee'd to a file and the pane
   held open (`sleep` bounded by the reservation) so sweep's existing read keeps
   working across the switch.
3. Only then consider replacing the pane read itself.

## What this does not fix

- **#4891 stays open and stays correct.** This routes around it; it does not
  make the k8s provider compose `PromptSuffix`. If upstream fixes it, the
  create-time path becomes available again — and dispatch must not then deliver
  twice.
- **`Nudge`/`SendKeys` still discard the carrier error.** gonk stops depending
  on that path, which makes it not gonk's problem, but it remains a real
  upstream defect for anyone else on a TUI-backed provider.
- **Every other Gas City caller still delivers by keystrokes.** This is a gonk
  fix. The general problem is upstream's, hence the draft in
  `docs/upstream/gascity-keystroke-delivery-injection.md`.

## Open questions

1. **Per-session capability instead of a shared reader token?** A one-time,
   high-entropy claim ticket minted per dispatch would make a pod able to fetch
   exactly its own prompt and nothing else. The obstacle is getting the ticket
   *into* the pod — that is the same delivery problem, one level down. The pod
   does hold `GC_INSTANCE_TOKEN`, a per-session secret, but gascity mints it and
   `SessionView` does not expose it, so the controller cannot pre-authorise
   against it. **Recommendation: ship the scoped shared token, write the
   limitation down, revisit if gonk ever becomes multi-tenant.**
2. **Does the prompt row belong in the ledger Postgres?** It is not ledger data
   and it holds issue text, which argues for a separate table with its own
   retention. The alternative — in-memory in the meter — is disqualified by the
   known trap that *every commit to gonk main re-releases the chart and restarts
   the pods*, which would drop prompts between store and fetch.
