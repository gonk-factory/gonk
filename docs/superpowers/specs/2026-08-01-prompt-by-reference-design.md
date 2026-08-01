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

### The key IS the capability — no credential in the pod

**Decided 2026-08-01 (owner).** The prompt is keyed by an alias carrying 128
bits of entropy, and holding that alias is what authorises the fetch. Nothing
secret is distributed to the agent pod at all.

```
gonk.triage.p75.i18.a1.k7m2q9x4nf3bt8wc5rj6ydzp1h
└──── 22 chars, unchanged ────┘└─ 26 chars, 128 bits base32 ─┘
```

**Why this is available.** The alias is caller-chosen: gonk mints it, and it
arrives in the pod as `GC_ALIAS` (verified live). `session.ValidateAlias`
accepts `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$` up to 64 characters
(`internal/session/names.go:39,44`); the current alias is 22, so 26 more fit
with room to spare.

**Why it authorises anything — CORRECTED 2026-08-01 after review.**

The first version of this section said "a peer agent pod cannot enumerate
aliases, because the agent NetworkPolicy permits egress to DNS, LiteLLM, GitLab
and gonk-meter and not the city API." **That was wrong, and it was wrong about
something this repo already documents.** `networkpolicy-gonk-agent.yaml`'s own
banner says it outright: Flannel does not implement NetworkPolicy and the Cilium
HelmRelease is suspended, so *right now an agent pod can reach anything*. The
policy is written and unenforced. Gas City's read-auth is opt-in besides
(`internal/api/readauth.go` — with no key configured the middleware is not
installed). A prompt-injected agent pod today can very likely reach the city API
and list sessions.

So network isolation is **not** what makes this safe. What does:

**The capability is already spent before untrusted text ever runs.** The
entrypoint fetches the prompt and the row goes to `410` *before* opencode
starts. The first moment attacker-controlled issue text reaches a model is
strictly after the capability it might exfiltrate has been consumed. Stealing
`GC_ALIAS` out of a running agent yields a dead key.

That makes the exposure window the seconds between `PUT` and the entrypoint's
fetch, during which the alias exists only in the controller's own memory and
logs — no agent has booted yet with it. A thief would have to already be
running, already know to look, and win a race against a pod that fetches on
startup.

**And theft is loud.** One-shot means a stolen prompt is a prompt the legitimate
entrypoint then fails to get: it receives `410`, exits non-zero, and the session
dies visibly instead of quietly triaging with someone else's context. The design
has no silent-compromise mode.

This argument does not depend on the CNI, which is the point — it holds today,
and it keeps holding when Cilium lands.

**What this deliberately avoids.** The obvious alternative was a shared
prompt-reader bearer token mounted into every agent pod. That was rejected:
gonk-meter's API is bearer-authenticated with **one admin token**
(`internal/meter/service/http.go`, `bearerAuth`) which also unlocks
`PUT/DELETE /v1/projects/{project}`, `POST /v1/policy/decide` and
`POST /admin/spend/sync`. Any design that puts a meter credential in the agent
pod has to prove, per route, that it is strictly weaker — and an agent that can
rewrite project registrations can rewrite its own budget, which would make the
meter decorative. Not shipping a credential is a stronger guarantee than
scoping one correctly.

**What this does NOT protect against, stated plainly.** This is a secret in a
*name*, and names travel. The alias reaches:

- gonk's own logs and Gas City's city events;
- session lists and `SessionView` responses;
- **the agent pod's k8s label** — `SanitizeLabel` leaves the alias intact and 49
  chars is under the 63-char cap (`internal/runtime/k8s/pod.go`), so
  `kubectl get pods --show-labels` prints the capability;
- **`.beads/issues.jsonl`, which is committed and pushed.** `Record.SessionID`
  stores the alias and the bead export is a tracked file — verified: aliases
  from this session are already in it. Under this design the capability would be
  pushed to a git remote.

Anyone with control-plane read, log access, or the git remote can read it. What
saves this is not secrecy from those parties but the time-bounding above: by the
time any of those surfaces has the alias, the row it unlocks is already `410`.
The leak is of a **spent** capability.

That is a real distinction and not a hand-wave, but it is also the whole
argument — so it must hold. Two things follow for the implementation: the
one-shot consume must be atomic (T1), and the entrypoint must not log the alias
into a pane that sweep captures (T4).

For a single-tenant install where the prompt is issue text the operator can
already read, this is the right trade. **It would not be, the moment gonk is
multi-tenant** — at which point the SA-token upgrade path below is the answer.

**The upgrade path, if that changes.** The agent pod carries a projected
ServiceAccount token whose claims are a real, unforgeable per-pod identity —
verified live on `s-go-4m4`:

```
sub:      system:serviceaccount:gonk:gc-agent
pod.name: s-go-4m4
pod.uid:  d10657cc-4cf1-443b-bed5-21116a68c13f
```

gonk-meter could verify it with a TokenReview and bind each prompt row to a pod
name/UID, leaking nothing into logs. It is not the choice here because it costs
`system:auth-delegator` (cluster-scoped) on the meter, and because we do not own
the agent pod spec — Gas City builds it, so we cannot request a bound token with
a custom audience and would be accepting API-server-audience tokens for our own
service, with a one-year lifetime. Worth revisiting only if the threat model
grows past "another pod in this namespace".

### Consequence: the alias stops being deterministic

`brokerSessionAlias`'s comment currently calls it "deterministic and
re-sling-stable EXCEPT for the attempt suffix". That property goes away.

**Checked, and nothing depends on it** (2026-08-01): `brokerSessionAlias` has
exactly one production call site, `cmd/gonk-gate/broker_inject.go:202`. Sweep
resolves the session through the stored `Record.SessionID`
(`cmd/gonk-gate/sweep.go:122`), never by recomputing the alias. Every other
reference is in tests.

Two things follow. The attempt suffix is no longer load-bearing for collision
avoidance — a random alias cannot collide — though it stays as useful
provenance. And the tests that recompute `brokerSessionAlias(42, 3, 1)` must
instead capture `gc.Created[0].Alias`, which is the better assertion anyway:
it checks what was actually sent rather than re-running the generator and
agreeing with itself.

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

1. Fetch `GET /v1/prompt/$GC_ALIAS`, retrying with backoff while the answer is
   `404` (the pod can legitimately beat the controller). No credential is
   presented — the alias is the capability.
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

1. ~~Per-session capability instead of a shared reader token?~~ **DECIDED
   2026-08-01: entropy in the alias** (see above). Recorded here because two
   candidates were checked and rejected on evidence, and should not be
   re-litigated:
   - **The session id** (`go-4m4`) is short and low-entropy, and gonk does not
     have it when it stores the prompt — agent create is async and returns no
     id. Not a secret and not available in time.
   - **`GC_INSTANCE_TOKEN`** *is* a real secret: `crypto/rand` 16 bytes → 128
     bits, per-incarnation, present in the pod env
     (`internal/session/lifecycle.go:21`). But no API route exposes it
     (`grep` over `internal/api/` finds nothing), so the controller can never
     learn it to pre-authorise against. Dead end.
2. **Does the prompt row belong in the ledger Postgres?** It is not ledger data
   and it holds issue text, which argues for a separate table with its own
   retention. The alternative — in-memory in the meter — is disqualified by the
   known trap that *every commit to gonk main re-releases the chart and restarts
   the pods*, which would drop prompts between store and fetch.
