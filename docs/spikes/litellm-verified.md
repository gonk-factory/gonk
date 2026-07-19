# LiteLLM verified — Plan 06 L2 first-contact spike

**Status: the real `internal/meter/litellm` adapter is BROKEN against real LiteLLM
v1.92.0. Plan 06 Task 5 (the L2 component suite) is BLOCKED on a Plan 03 fix.**

This document is Step 6 of Plan 06 Task 5. It records what was verified against a
**real** `ghcr.io/berriai/litellm-database:v1.92.0` (pinned, OD-6) — the FIRST time
gonk's LiteLLM adapters (Plan 03 Task 7) spoke to a real proxy instead of an
`httptest.Server` mock. It caught two fatal adapter bugs exactly the way L1 caught
the zero-cost reservation brick (gonk-2g4): correct test, real dependency, real
defect. The bugs are filed as **gonk-huy (P0)**.

**A LiteLLM version bump invalidates this document and re-runs the spike.**

## How this was measured

- Real LiteLLM `ghcr.io/berriai/litellm-database:v1.92.0`, podman `--network=host`,
  master key generated at runtime.
- Real Postgres 16 as LiteLLM's own DB (a throwaway container; NOT the owner's DB).
- A tiny OpenAI-compatible stub upstream on `127.0.0.1:8081` returning a scripted
  `usage` block; LiteLLM configured with EXACTLY three upstreams, all pointed at
  that one stub (`qwen-local`→stub-qwen $0.25/1M, `glm`→stub-glm $2.00/1M,
  `sonnet`→stub-sonnet $6.00/1M — the Task 5 price table).
- Probes were raw HTTP against the running proxy, plus direct SQL against
  `LiteLLM_SpendLogs`.

### Minimal LiteLLM config that boots and enforces budgets (for the harness)

```yaml
model_list:
  - {model_name: qwen-local, litellm_params: {model: openai/stub-qwen,   api_base: http://127.0.0.1:8081/v1, api_key: sk-stub, input_cost_per_token: 0.00000025, output_cost_per_token: 0.00000025}}
  - {model_name: glm,        litellm_params: {model: openai/stub-glm,    api_base: http://127.0.0.1:8081/v1, api_key: sk-stub, input_cost_per_token: 0.000002,   output_cost_per_token: 0.000002}}
  - {model_name: sonnet,     litellm_params: {model: openai/stub-sonnet, api_base: http://127.0.0.1:8081/v1, api_key: sk-stub, input_cost_per_token: 0.000006,   output_cost_per_token: 0.000006}}
litellm_settings: {drop_params: true}
general_settings:
  master_key: sk-master-<runtime>
  proxy_batch_write_at: 1        # smaller detail-log flush interval (still laggy; see P3-3)
```
Env: `DATABASE_URL=postgresql://.../litellm`, `LITELLM_MASTER_KEY=<same as master_key>`,
`STORE_MODEL_IN_DB=True`. Command: `--config /app/config.yaml --port 4000`.
Boot to `GET /health/liveliness == 200` took ~15–31s.

---

## THE BLOCKER — two Plan 03 adapter bugs (gonk-huy, P0)

Both adapters were "written against the docs and tested only against an
`httptest.Server`" (their own package comments admit the live-infra boundary). The
mock in `spendsource_test.go` returns a **bare JSON array** and **ignores the query
dates** — which is precisely why neither bug was caught before real contact.

### BUG A — the spend poller cannot read real `/spend/logs/v2` (FATAL)

`internal/meter/litellm/spendsource_http.go`. Consequence: meter's spend sync
errors on **every** call → `synced` never becomes true → `Ready()` never true →
**every budgeted `/decide` defers forever.** The factory stalls permanently while
believing it is being careful.

1. **Date format.** `fetchPage` sends `start_date`/`end_date` as `time.RFC3339`
   (`2026-07-01T00:00:00Z`). Real v2 rejects it:
   ```
   GET /spend/logs/v2?start_date=2026-07-01T00:00:00Z&end_date=...  ->  HTTP 400
   {"error":{"message":"Invalid date format: 2026-07-01T00:00:00Z.
     Expected: 'YYYY-MM-DD' or 'YYYY-MM-DD HH:MM:SS'", ...}}
   ```
   With `start_date=2026-07-01&end_date=2026-08-01` → **200**. Fix: format as
   `2006-01-02` (or `2006-01-02 15:04:05`).
2. **Response envelope.** `Since` does `json.Unmarshal(body, &entries)` where
   `entries` is `[]logEntry`. Real v2 returns an **object**, not a bare array:
   `{"data":[...], "total":N, "page":P, "page_size":S, "total_pages":T}`. Even
   with dates fixed, the unmarshal fails. Fix: decode
   `{"data": []logEntry, total, page, page_size, total_pages}` and read `.data`.
3. **Pagination.** The loop advances on `header.Get("X-Next-Page")`, which v2 does
   **not** send (verified: response headers carry `Date` and `content-type` only).
   Real pagination is the body fields `page`/`total_pages`. As written, only page 1
   is ever read → rows past `page_size` are silently dropped (the 10k-cap hazard).
   Fix: loop while `page < total_pages`.

**Good news within Bug A:** the per-**row** field mapping is correct. A real v2 row
carries `request_id`, `spend`, `prompt_tokens`, `completion_tokens`, `startTime`,
and `metadata.spend_logs_metadata` — exactly the json tags `logEntry` expects. Only
the query format, the envelope, and the pager are wrong.

### BUG B — admin `updateByAlias` cannot look a key up by alias (breaks re-provisioning)

`internal/meter/litellm/admin_http.go`. `EnsureKey` is idempotent-by-alias: a
`/key/generate` on an existing alias returns
`400 "Key with alias 'X' already exists. Unique key aliases across all keys are
required."` → `alreadyExists()` matches → `updateByAlias()` runs:

1. `GET /key/info?key_alias=<alias>` → **HTTP 404**
   `{"error":{"message":"Key not found in database", "type":"not_found_error", ...}}`.
   v1.92.0 `/key/info` accepts **only** `?key=<token>`, never `?key_alias=`. So the
   second `EnsureKey` for any project — every budget change, every `ReconcileKeys`
   tick, every re-register — **errors**.
2. **Shape (also wrong).** `GET /key/info?key=<token>` returns
   `{"key":"sk-...", "info":{"key_alias":..., "max_budget":..., "budget_reset_at":...}}`
   — the fields are **nested under `info`**, but `decodeKeyResponse` reads
   top-level `key_alias`.

Fix: resolve the existing token without `?key_alias=` (LiteLLM offers `/key/list`
which supports `key_alias=`; or keep the token from create; or parse `.info`), and
decode the nested `/key/info` shape.

---

## The P3 answers (the questions L2 exists to close)

| # | Question | Answer |
|---|---|---|
| P3-1 | Do the real admin adapters work? | **NO — see Bug A/B.** `/key/generate`, `/key/update` (raise+lower `max_budget`), `/key/delete` DO work; `updateByAlias` and the spend poller do NOT. |
| P3-2 | Is `budget_duration: "1mo"` a calendar month? | **YES — a UTC calendar month.** Key created 2026-07-19 → `budget_reset_at: 2026-08-01T00:00:00+00:00`. Not rolling-30-days. Soft door (meter, UTC calendar month, Decision 6) and hard door agree. |
| P3-3 | Real spend-log lag vs `max_spend_staleness` (5m)? | **Could not be pinned down — and cannot be, through the real code path, until Bug A is fixed.** See below; treat as a live risk. |
| P3-4 | Do synthetic prices agree (catalog ↔ LiteLLM)? | **YES.** `/model/info` returns `input_cost_per_token`: qwen-local `2.5e-07` (= $0.25/1M), glm `2e-06`, sonnet `6e-06` — exactly the configured/catalog prices. |
| P3-5 | Does the hard door close? | **YES — the crux result. See below.** |
| P3-6 | Does a dedicated admin key suffice, or are we forced onto the master key? | **A dedicated proxy-admin key SUFFICES.** A plain generated key gets 401 on `/key/generate` and `/spend/logs/v2` (200 on `/model/info` only). A key issued to a `POST /user/new {"user_role":"proxy_admin"}` user performs every admin call (generate/delete/spend-logs). **Meter must be given a proxy-admin-role key, not just any key — and then the master key is NOT required.** |
| P3-8 | Catalog drift = 0? | Checkable and clean: `/model/info` prices match the catalog (see P3-4). |
| — | Does LiteLLM forward `metadata` upstream? | **NO.** The stub upstream saw only `["messages","model"]` in the request body; the `metadata` object was not forwarded. So the stub log is an independent check on **VOLUME** (call/token counts) only, **never on ATTRIBUTION**. Attribution must be read from the v2 row's `metadata.spend_logs_metadata` (which the `x-litellm-spend-logs-metadata` header populates — consistent with docs/environment.md's 2026-07-13 smoke test). |

### THE HARD DOOR CLOSES (P3-5, the single most important result)

A $1.00 key on `glm` ($2.00/1M), 100k prompt tokens/call = $0.20/call, hammered
**directly** (meter fully bypassed):

```
call 1..5 -> 200
call 6    -> 429  {"error":{"message":"Budget has been exceeded! Key=gonk-harddoor
                    Current cost: 1.0, Max budget: 1.0","type":"budget_exceeded"}}
call 7..12-> 429  (stays closed)
```

Five calls = exactly $1.00, the sixth is refused. **Overshoot = $0.00** (well within
the one-call documented slack). The refusal is **HTTP 429** `budget_exceeded`. The
money cannot escape even with meter out of the loop. `/key/delete` also closes the
door immediately: a deleted key's token is refused **401** on the very next call.

### P3-3 — spend-log lag could not be measured (and why it matters)

LiteLLM has **two** spend surfaces and they behave very differently:

- The **per-key budget counter** that enforces the hard door updates **immediately**
  (proven: the door closed at exactly $1.00, synchronously).
- The **detailed `LiteLLM_SpendLogs` table** that meter POLLS is written by a
  best-effort background batch task. In this single-node podman setup its write lag
  was **large and unreliable**: a real completion produced one correct row
  (`call_type=acompletion`, `model=openai/stub-glm`, `spend=0.2`,
  `prompt_tokens=100000`), but subsequent completions (even at
  `proxy_batch_write_at: 1`) frequently produced **no visible row within 100s**, and
  after a proxy restart against the same DB, completions produced no rows in-window
  at all.

This is a **real risk for P3-3**: if the detail-log lag exceeds `max_spend_staleness`
(default 5m), meter stalls while believing it is careful. It must be characterized
by the harness `TestMeasureLiteLLMSpendLogLag` — **but that test necessarily runs
through the real `HTTPSpendSource`, which is Bug A and cannot query v2 yet.** So the
lag number is deferred behind the Bug A fix. (Some of the "no row" observations are
likely spike artifacts of restarting a single-node proxy mid-flight; a stable,
never-restarted proxy driven by the fixed adapter is the right measurement rig.)

---

## Request/response shapes that differed from Plan 03's assumptions

| Adapter assumed | Real v1.92.0 |
|---|---|
| `/spend/logs/v2?start_date=<RFC3339>` | Wants `YYYY-MM-DD` or `YYYY-MM-DD HH:MM:SS`; RFC3339 → 400 |
| `/spend/logs/v2` → bare `[ {...} ]` | `{"data":[...], "total", "page", "page_size", "total_pages"}` |
| pagination via `X-Next-Page` header | body `page`/`total_pages`; no such header |
| `/key/info?key_alias=<alias>` → `{key, key_alias}` | 404; only `?key=<token>` works, and the shape is `{key, info:{...}}` |
| `/key/generate` dup-alias error wording | `"Key with alias 'X' already exists. Unique key aliases across all keys are required."` (`alreadyExists()` heuristic DOES match) |
| any admin key can call admin routes | admin routes require `proxy_admin` role; plain key → 401 |
| `metadata` reaches the upstream | it does not; only `spend_logs_metadata` (from the header) is persisted in the log |

Verified unchanged/correct: `/key/generate` returns `{"key":"sk-...","key_alias":...,
"max_budget":..., "budget_duration":..., "metadata":...}` (flat, top-level `key`);
`/key/update {"key":"sk-...","max_budget":5.0}` takes effect (raise and lower);
`/key/delete {"key_aliases":[...]}` → `{"deleted_keys":[...]}`; v2 row field names
match `logEntry`; `input_cost_per_token` on `/model/info` matches the catalog.
