# Plan — prompt-by-reference (get delivery off the keystroke channel)

_Design: [`2026-08-01-prompt-by-reference-design.md`](../specs/2026-08-01-prompt-by-reference-design.md).
Beads: `gonk-e9m` (P1, the injection boundary), `gonk-m6t` (P2, attribution).
Status: **NOT STARTED** — design approved 2026-08-01, no code written._

Update the status boxes below as tasks land. A task is done when its
verification line has actually been run, not when the code looks right — that
distinction is what this whole line of work has been about.

## Sequencing rationale

T0 tests the assumption everything else rests on. T1–T4 then land the channel
with opencode **still in TUI mode**. That is the
security-relevant win and it leaves the read path untouched, so it can be proven
in one live run. T5 removes the mitigation, which is the real proof. T6+ are the
follow-on moves and are deliberately *not* bundled — see the design's
"Interactive TUI or `opencode run`?".

---

### T0 — SPIKE FIRST: is argument handling actually inert? · `[ ]`

**One hour, before anything else is built.** The entire plan assumes that
handing opencode the prompt as an argument avoids the composer, and that
assumption is currently only tested at T6 — after five tasks of work.

If opencode seeds its composer buffer from `--prompt` (entirely plausible for a
TUI), then a bang in an argument hits the same shell-mode trigger, this design
does not solve the problem, and the real fix is `opencode run` **first** — which
inverts the sequencing below.

**Do**: in a scratch pod from the agent image, run
`opencode --prompt 'see issue !42 and reply with exactly the word SPIKE'` and
then the same via `opencode run`. Watch for the `$` composer prefix and a
`/bin/sh` error.

**If the argument path is not inert**: stop, and re-sequence around
`opencode run` — which also means the read path (sweep captures the live tmux
pane) has to move at the same time, so the plan gets bigger before it gets
smaller. Better to learn that now than at T6.

### T1 — prompt store in gonk-meter · `[ ]`

`internal/meter/store`: a `PromptRow{Alias, Prompt, Model, Metadata, CreatedAt,
FetchedAt, ExpiresAt}` with `PutPrompt` / `TakePrompt` / `PromptStatus` on the
`Store` interface, implemented for both `memory` and `postgres` (the interface
has two implementations and `storetest` covers both — keep it that way).

`TakePrompt` must be **atomic**: read-and-mark-consumed in one statement, the
same discipline `ReserveIfFits` already follows, or two pods racing both get the
prompt and the one-shot guarantee is decorative.

- Its own table, not a ledger table. It holds issue text and wants its own
  retention (design, Open question 2).
- Expiry sweep alongside the existing janitor.

**Verify**: `storetest` passes for memory and postgres; a concurrent
`TakePrompt` test proves exactly one caller wins.

### T2 — routes, and the one deliberate auth exemption · `[ ]`

`internal/meter/service/http.go`:

- `PUT /v1/prompt/{alias}` — **admin bearer**, as every other write. The
  controller stores.
- `GET /v1/prompt/{alias}` — **no bearer**. The 128-bit alias *is* the
  capability (design: "The key IS the capability"), so this route joins
  `/healthz`, `/readyz` and `/metrics` in `bearerAuth`'s exempt switch.
- `GET /v1/prompt/{alias}/status` — **admin bearer**; reports `fetched_at`.

**This is the security-critical task.** Adding an unauthenticated route to a
service that otherwise guards project registration and policy decisions is
exactly the kind of change that goes wrong quietly. Two rules:

1. The exemption is for `GET` on that path **only** — `PUT`/`DELETE` to the same
   path must still demand the admin bearer. Go 1.22 pattern-matching makes this
   expressible; the exempt check in `bearerAuth` switches on `r.URL.Path` alone
   today, so it needs method awareness or it will hand write access away.
2. **Reject a nonce-free alias, by strict format.** The handler cannot measure
   entropy, only shape: require the final dot-separated label to be exactly 26
   base32 characters. This is also the cheap guard that keeps an unauthenticated
   route from turning into a free database lookup on the same listener that
   serves budget decisions — check the shape before touching the store.

**Verify** — this is the test that matters most in the plan:
- an exhaustive table over every route × method asserting exactly one
  unauthenticated `200`, and `401` everywhere else, so a future route is caught
  by default;
- `PUT` and `DELETE` on the prompt path still `401` without the bearer;
- a nonce-free alias is refused;
- a second `GET` of a consumed prompt is `410`, not a replay.

### T3 — chart: point agents at the meter · `[ ]`

Much smaller than it would have been with a token to mint and rotate — there is
no credential to distribute.

- `bootstrap-city` adds `GONK_PROMPT_URL` to each agent's `[env]`, in the same
  `awk` block that already injects `GONK_LITELLM_URL`, with the same
  `case */control-dispatcher/) continue` exclusion (the control lane runs no
  model and fetches no prompt).
- Do **not** rely on `GONK_METER_SERVICE_HOST`/`_PORT`. The kubelet does inject
  them (confirmed live in an agent pod), but service env vars only exist if the
  Service predates the pod, which is a startup-ordering dependency nobody should
  have to reason about. Inject the URL explicitly.

**Verify**: `go test -tags chart ./internal/charttest/...`; golden files show
`GONK_PROMPT_URL` in agent configs and not in the control-dispatcher's, and **no
new secret anywhere** — if a token appears in this diff, the design was
misread.

### T4 — entrypoint fetches its own prompt · `[ ]`

`images/agent/entrypoint.sh`:

- `GET $GONK_PROMPT_URL/v1/prompt/$GC_ALIAS`, retrying with backoff on `404`
  (the pod can beat the controller), bounded. **No credential** — the alias in
  `GC_ALIAS` is the capability, so there is no token to materialise here and the
  LiteLLM-key file dance does not apply.
- Treat the alias as secret in the entrypoint's own logging: it already logs
  freely, and `set -x` or an error path echoing the URL would publish the
  capability into the pane, which is captured and read by sweep.
- Feed the prompt to opencode **as an argument**.
- Restore the model/metadata seam from the row's `model`/`metadata` fields, so
  the overlay is rendered per-session again — **this is what closes `gonk-m6t`**.
- **Exit non-zero, loudly, if no prompt arrives in the window.** An idle agent
  that looks healthy is the failure mode this whole line of work exists to end.
- **Distinguish `410` from a `404` timeout, with different exit codes and
  diagnostics.** They mean opposite things: `404`-until-timeout is "never
  delivered"; `410` is "already consumed", which means either a respawn or a
  theft. Gas City relaunches a dead agent into the warm pod and re-runs the
  start command, and gonk has already hit a respawn loop in production (the
  `HOME`/XDG saga in `pack/agents/triage/agent.toml`). So after any crash past
  the fetch, the entrypoint will get `410` and exit, and the session will churn
  until sweep reaps it at the reservation deadline. That is not worse than today
  — a respawned session loses its submitted prompt now too — but it must be
  legible in the logs rather than looking like a delivery bug.

**Verify**: the entrypoint's existing shell tests, plus a new case for the
no-prompt path asserting a non-zero exit and the diagnostic. Then live: the pod
comes up with the prompt already in hand.

### T5 — dispatch stores before create, confirms by `fetched_at` · `[ ]`

`cmd/gonk-gate/broker_inject.go`:

- Give `brokerSessionAlias` a 128-bit base32 suffix (crypto/rand). Total length
  ~49 chars against `ValidateAlias`'s 64-char cap. Its doc comment currently
  promises determinism — rewrite it, and say why the entropy is load-bearing.
- `PUT` the prompt **before** `CreateSession` — the pod can be up before the
  create call returns.
- Replace the running-gate + submit + event-correlation delivery path with
  polling `fetched_at`. Keep `awaitCreate` (a create can still fail silently).
- **`fetched_at` proves the entrypoint fetched, not that opencode accepted.**
  It is strictly better than the event correlation it replaces, but it is not
  terminal success — keep dispatch's session-health checks rather than treating
  the receipt as proof the agent is working.
- **DELETE `sanitizeForKeystrokeDelivery` and its test.** Leaving it would mean
  shipping a channel whose safety we never actually exercised, and quietly
  losing every `!` in every issue for no reason.
- Keep `pkg/gcapi.SubmitSession` + `AwaitRequestOutcome`. They are correct,
  tested, and the right tools the moment anything needs a supervisor-side
  message; they simply stop being the prompt path.

**Verify**: `gcapitest` grows a prompt-store fake; dispatch tests assert the
`PUT` precedes the create and that an unfetched prompt fails the dispatch.

**Test-shape change this forces**: five tests currently recompute
`brokerSessionAlias(42, 3, 1)` and compare. With a random suffix they must
capture `gc.Created[0].Alias` instead — which is the better assertion anyway,
since it checks what was actually sent rather than re-running the generator and
agreeing with itself. Checked 2026-08-01: no *production* code reconstructs the
alias (one call site, `broker_inject.go:202`; sweep uses the stored
`Record.SessionID`), so this is a test-only change.

### T6 — live proof · `[ ]`

A real GitLab issue through the deployed stack — **not** a hand-copied binary
— producing a fenced batch, with a bang in the issue body and title.

**This is the acceptance criterion.** With T5 done there is no bang-stripping,
so a `!` surviving into a delivered prompt and *not* running a shell is the
whole point. Recipe and traps: `docs/HANDOFF-next-session.md`.

Check one thing by hand while you are in there: that a second `GET` of the same
alias returns `410`, from inside a pod. The one-shot guarantee is the only thing
standing between "unauthenticated read" and "replayable unauthenticated read".

---

## Follow-on, deliberately not in this plan

- **`opencode run` instead of the TUI** — removes input modes entirely rather
  than routing around them, and gives plain stdout for fence parsing. Blocked on
  the read path: sweep reads a live `capture-pane`, so the pane must be held
  open past the run. Do it after T6, on its own.
- **Replace the pane read** with the agent posting its batch back. Bigger; moves
  sweep, the fence contract and `gonk-u1p.3`'s peek-window handling at once.
- **Pod-identity auth** (SA token + TokenReview) instead of the capability
  alias, if gonk ever goes multi-tenant or the alias-in-logs exposure stops
  being acceptable. Design has the verified claims and the costs.

## Traps (from the handoff — read before touching the cluster)

- Every commit to gonk `main` re-releases the chart and recreates pods; every
  intake restart then drops webhooks for up to 10 min (`gonk-fan`).
- The controller pod holds no admin secret you should copy out. The probe
  pattern — build a small binary, `kubectl cp` it in, let it read the *mounted*
  key — keeps credentials in the cluster.
- Iterate with `kubectl cp` of a rebuilt binary rather than a chart release,
  right up until T6, which must be a real deploy or it proves nothing.
