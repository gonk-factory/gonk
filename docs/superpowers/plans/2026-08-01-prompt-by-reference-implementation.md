# Plan — prompt-by-reference (get delivery off the keystroke channel)

_Design: [`2026-08-01-prompt-by-reference-design.md`](../specs/2026-08-01-prompt-by-reference-design.md).
Beads: `gonk-e9m` (P1, the injection boundary), `gonk-m6t` (P2, attribution).
Status: **NOT STARTED** — design approved 2026-08-01, no code written._

Update the status boxes below as tasks land. A task is done when its
verification line has actually been run, not when the code looks right — that
distinction is what this whole line of work has been about.

## Sequencing rationale

T1–T4 land the channel with opencode **still in TUI mode**. That is the
security-relevant win and it leaves the read path untouched, so it can be proven
in one live run. T5 removes the mitigation, which is the real proof. T6+ are the
follow-on moves and are deliberately *not* bundled — see the design's
"Interactive TUI or `opencode run`?".

---

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

### T2 — route + scoped credential · `[ ]`

`internal/meter/service/http.go`:

- `PUT /v1/prompt/{alias}` — admin bearer only (the controller writes).
- `GET /v1/prompt/{alias}` — **the scoped prompt-reader token only**.
- `GET /v1/prompt/{alias}/status` — admin bearer; reports `fetched_at`.

**The load-bearing security property**: the prompt-reader token must be rejected
by *every other route*. `bearerAuth` today is one flat token set over the whole
mux; this needs a per-route principal, not a second token bolted into the same
slot. Get this wrong and the agent pod can rewrite project budgets.

**Verify**: a table test asserting the reader token gets `401` on
`PUT /v1/projects/{p}`, `DELETE /v1/projects/{p}`, `POST /v1/policy/decide`,
`POST /admin/spend/sync` and `GET /v1/cost/*` — and `200` on exactly one route.
Add it to the existing auth test so a future route is caught by default.
Also: second `GET` of a consumed prompt is `410`, not a replay.

### T3 — chart: mint and deliver the reader token · `[ ]`

- Generate/accept a `promptReader` token in the meter secret next to the admin
  bearer; meter gets it as `GONK_METER_PROMPT_TOKEN_FILE` (marker-free path env
  — `TOKEN` in the name is stripped from exec-order env, see
  `pack/orders/gonk-dispatch.toml`'s warning).
- `bootstrap-city` writes `GONK_PROMPT_URL` + `GONK_PROMPT_TOKEN` into each
  agent's `[env]`, in the same `awk` block that already injects
  `GONK_LITELLM_KEY` — and with the same `case */control-dispatcher/) continue`
  exclusion, since the control lane runs no model and needs no prompt.

**Verify**: `go test -tags chart ./internal/charttest/...`; golden files show
the reader token in agent configs and **not** in the control-dispatcher's; the
admin bearer appears in neither.

### T4 — entrypoint fetches its own prompt · `[ ]`

`images/agent/entrypoint.sh`:

- Materialise `GONK_PROMPT_TOKEN` into a `0600` file and unset it — reuse the
  existing LiteLLM-key pattern verbatim; the key never enters a child's env.
- `GET $GONK_PROMPT_URL/v1/prompt/$GC_ALIAS`, retrying with backoff on `404`
  (the pod can beat the controller), bounded.
- Feed the prompt to opencode **as an argument**.
- Restore the model/metadata seam from the row's `model`/`metadata` fields, so
  the overlay is rendered per-session again — **this is what closes `gonk-m6t`**.
- **Exit non-zero, loudly, if no prompt arrives in the window.** An idle agent
  that looks healthy is the failure mode this whole line of work exists to end.

**Verify**: the entrypoint's existing shell tests, plus a new case for the
no-prompt path asserting a non-zero exit and the diagnostic. Then live: the pod
comes up with the prompt already in hand.

### T5 — dispatch stores before create, confirms by `fetched_at` · `[ ]`

`cmd/gonk-gate/broker_inject.go`:

- `PUT` the prompt **before** `CreateSession` — the pod can be up before the
  create call returns.
- Replace the running-gate + submit + event-correlation delivery path with
  polling `fetched_at`. Keep `awaitCreate` (a create can still fail silently).
- **DELETE `sanitizeForKeystrokeDelivery` and its test.** Leaving it would mean
  shipping a channel whose safety we never actually exercised, and quietly
  losing every `!` in every issue for no reason.
- Keep `pkg/gcapi.SubmitSession` + `AwaitRequestOutcome`. They are correct,
  tested, and the right tools the moment anything needs a supervisor-side
  message; they simply stop being the prompt path.

**Verify**: `gcapitest` grows a prompt-store fake; dispatch tests assert the
`PUT` precedes the create and that an unfetched prompt fails the dispatch.

### T6 — live proof · `[ ]`

A real GitLab issue through the deployed stack — **not** a hand-copied binary
— producing a fenced batch, with a bang in the issue body and title.

**This is the acceptance criterion.** With T5 done there is no bang-stripping,
so a `!` surviving into a delivered prompt and *not* running a shell is the
whole point. Recipe and traps: `docs/HANDOFF-next-session.md`.

---

## Follow-on, deliberately not in this plan

- **`opencode run` instead of the TUI** — removes input modes entirely rather
  than routing around them, and gives plain stdout for fence parsing. Blocked on
  the read path: sweep reads a live `capture-pane`, so the pane must be held
  open past the run. Do it after T6, on its own.
- **Replace the pane read** with the agent posting its batch back. Bigger; moves
  sweep, the fence contract and `gonk-u1p.3`'s peek-window handling at once.
- **Per-session capability** instead of a shared reader token (design, Open
  question 1).

## Traps (from the handoff — read before touching the cluster)

- Every commit to gonk `main` re-releases the chart and recreates pods; every
  intake restart then drops webhooks for up to 10 min (`gonk-fan`).
- The controller pod holds no admin secret you should copy out. The probe
  pattern — build a small binary, `kubectl cp` it in, let it read the *mounted*
  key — keeps credentials in the cluster.
- Iterate with `kubectl cp` of a rebuilt binary rather than a chart release,
  right up until T6, which must be a real deploy or it proves nothing.
