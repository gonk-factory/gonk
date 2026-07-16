# gonk-meter v1 API

**Status: published.** This is the contract Plan 02 (`gitlab-intake`) and
Plan 04 (the Gas City pack) build against. The normative Go source is
`pkg/meterapi` (Plan 03's Task 0); this document restates it in prose, and
a sha256 drift gate (`pkg/meterapi/testdata/contract.sha256`) fails CI if
`pkg/meterapi/meterapi.go` changes without a deliberate, reviewed update.
The literal JSON field names below are also golden-tested byte-for-byte —
`pkg/meterapi/meterapi_test.go`'s `TestWireContractLiterals` (the Go types
in isolation) and `internal/meter/service/wire_contract_test.go`'s
`TestWireContractLiterals` (the same shapes as they actually leave the real
HTTP mux) — so a field rename fails CI even though a symmetric round-trip
test would happily accept it. See ADR-004 for the policy this API encodes;
this document is the wire shape only.

## At a glance

- All endpoints are JSON over HTTP, cluster-internal.
- Every endpoint requires `Authorization: Bearer <token>` **except**
  `GET /healthz`, `GET /readyz`, `GET /metrics`. The token is read from a
  file with two rotation slots (current + previous), never an env value, a
  flag, or the config file (Decision 12). Either slot is accepted.
- **The decision is three-valued: `run` | `defer` | `deny`.** `defer` means
  *not now, retry at `retry_after`* (budget exhausted this window, quiet
  hours, stale spend data, key not yet provisioned). `deny` means *never on
  retry* (disabled project, invalid config, ladder exhausted, per-task
  tokens gone) — parking a `deny`-worthy bead in a retry loop forever would
  be a bug, not a policy.
- **`defer` and `deny` are HTTP `200`.** They are normal policy answers, not
  errors. Only a malformed request (`400`) or an internal failure (`500`) is
  a non-200 from `/v1/policy/decide` or `/v1/policy/outcome`.
- **`null` means unlimited**, for both cost and token budgets, on every wire
  `Budget`. `+Inf`/`MaxInt64` (gonkcfg's in-process "unlimited" sentinels,
  ADR-002) never reach JSON directly — `encoding/json.Marshal(math.Inf(1))`
  returns an error, not a number. `meterapi.ZeroBudget()` (all-`0`, never
  all-`null`) is the explicit fail-closed value an invalid/disabled project
  gets; an empty `Budget{}` is all-`nil`, i.e. unlimited — never send one to
  mean "no budget".
- **`meterapi.DecideRequest` has no `attempt` field, and adding one is a
  forgery vector, not a feature.** Meter owns ladder/attempt state; it reads
  its own store, written only by `/v1/policy/outcome`.
- `{project}` is the GitLab `path_with_namespace`, **URL-path-escaped**
  (`group/repo` -> `group%2Frepo`). Build it with `meterapi.ProjectPath` (or
  the sibling `*Path` helpers) — never hand-build it; an unescaped `/`
  silently routes to a different handler or none.

## The endpoint set (complete — nothing else exists)

| Method + path | Caller(s) | Purpose |
|---|---|---|
| `PUT /v1/projects/{project}` | intake | Register/update a project from its raw `.gonk.yml`. Meter validates, resolves, provisions the virtual key. |
| `GET /v1/projects/{project}` | intake, pack, humans | Current state, effective config, budget, remaining. |
| `DELETE /v1/projects/{project}` | intake | De-onboard: disable the project and delete its virtual key. Idempotent; `204` either way. |
| `POST /v1/projects/{project}/key/rotate` | operator | Rotate the virtual key. |
| `POST /v1/policy/decide` | intake (**Gate 1**), pack `gonk-dispatch` exec order (**Gate 2**) | The rung decision: `run` / `defer` / `deny`. Idempotent on an open reservation. |
| `POST /v1/policy/outcome` | pack gate step | Report a terminal outcome; settles the reservation, records ladder state. |
| `GET /v1/cost/bead/{bead_id}` | pack, humans | Cost/tokens for one bead, all attempts, all time. |
| `GET /v1/cost/session/{session_key}` | agent (commit trailers) | Cost/tokens for one session. |
| `GET /v1/cost/project/{project}` | intake, dashboards | Cost/tokens for the current budget window, plus remaining. |
| `GET /v1/cost/instance` | dashboards | Instance rollup. |
| `POST /admin/spend/sync` | operator, test harness, pack sweep step | Force one spend-log poll, **block until it completes**, return `spend_as_of`. Bearer-authenticated, same token as every other route. |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | k8s, Prometheus | Unauthenticated. |

**Intake calls five of these:** `PUT`/`GET`/`DELETE /v1/projects/{project}`,
`GET /healthz`, and `POST /v1/policy/decide` (Gate 1). It fires the Gas City
order only on `decision: "run"`, and it never sends an attempt count.

**The rung gate is in two places.** A Gas City formula cannot make an HTTP
call, so the decision is asked twice: intake gates the *first* dispatch
(Gate 1, an optimization — it avoids churning a bead meter would certainly
deny), and the pack's `gonk-dispatch` exec order re-decides on *every* pour
(Gate 2 — the actual enforcement point, including every controller-initiated
re-sling). Gate 2 must never trust a rung or reservation passed in order
vars; it always re-decides. Because a bead can therefore be `/decide`d twice
before any outcome is reported, **`/decide` is idempotent on an open
reservation**: a second call for the same `(project, bead_id, session_key)`
returns the existing open, unexpired reservation rather than minting a
second one. This is why `bead_id` at both gates is pinned to the
deterministic `BeadAnchor`.

## `PUT /v1/projects/{project}` — intake pushes raw config, meter resolves

Intake sends the `.gonk.yml` bytes it read from GitLab, **verbatim**, plus
GitLab metadata meter cannot know. Meter validates them, folds the
operator's instance and (nested-group-folded) group policy over them, and
calls `gonkcfg.Resolve` — **the only call to `Resolve` in the entire
system**.

```json
// request (meterapi.ProjectRequest)
{ "project": "group/repo",
  "project_id": 42,
  "rig": "group-repo",
  "default_branch": "main",
  "config_commit_sha": "9f2a1c…",
  "gonk_yml": "version: 1\nenabled: true\n..." }
```

Three response codes, and the distinction matters:

**`200`** — the config loaded and resolved.

```json
// meterapi.ProjectResponse
{ "project": "group/repo", "rig": "group-repo",
  "state": "active",
  "disabled_reason": "",
  "effective": {
    "enabled": true,
    "actions": {"triage": true, "pipelines": false, "features": false},
    "ladder": ["qwen-local","glm"],
    "continuity": "resume",
    "triage": {"label_prefix":"gonk::","respond_to_mentions":true},
    "provenance": {"commit_trailers":true,"include_usage":false},
    "schedule": {"quiet_hours":"22:00-07:00","timezone":"America/New_York"}
  },
  "budget": { "monthly_cost_usd": 10, "monthly_tokens": 50000000, "per_task_tokens": 2000000 },
  "key_ref": { "secret_name": "gonk-key-group-repo-1a2b3c4d", "secret_key": "LITELLM_API_KEY" },
  "config_hash": "sha256:…",
  "updated_at": "2026-07-13T10:00:00Z" }
```

**`422`** — the `.gonk.yml` is present but will not load (schema-invalid,
non-finite budget, unknown timezone, reordered ladder). This is a
**successful, idempotent registration of an invalid config**, not a bad HTTP
request: meter records `state: invalid` and **deletes the project's virtual
key**.

```json
{ "project": "group/repo", "rig": "group-repo",
  "state": "invalid",
  "disabled_reason": "",
  "error": ".gonk.yml: jsonschema validation failed with '…#'\n- at '/budget/monthly_tokens': …",
  "effective": null,
  "budget": { "monthly_cost_usd": 0, "monthly_tokens": 0, "per_task_tokens": 0 },
  "key_ref": {"secret_name":"","secret_key":""},
  "config_hash": "sha256:…", "updated_at": "…" }
```

Note the fail-closed budget on a `422`: `null` would mean *unlimited*, the
exact opposite of what an invalid config must get.

**`400`** — the *request* is malformed (missing `project_id`, an
attribution-unsafe project path, an oversized body). **Nothing is
recorded.** This is the only 4xx that means "intake has a bug".

Hard rules on this seam:

- `effective` is `null` **if and only if** `state == "invalid"`.
- `disabled_reason` is non-empty **iff** `state == "disabled"`.
- `key_ref` is a pointer to where the key lives, **never key material**.
- Meter is idempotent on `config_hash`: intake may `PUT` the same bytes
  every reconcile pass and meter no-ops.
- A `DELETE` disables the project and deletes the virtual key. Idempotent;
  deleting an unknown project is `204`.

## `POST /v1/policy/decide` — the rung decision

```json
// request (meterapi.DecideRequest)
// NOTE: no attempt number, no prior-outcome list. Meter owns ladder state.
{ "project": "group/repo", "rig": "group-repo", "bead_id": "gk-1a2b",
  "session_key": "sess-9", "trigger": "issue-triage" }

// 200, run (meterapi.DecideResponse)
{ "decision": "run", "rung": "glm", "model": "glm-5", "attempt": 2,
  "reason": "", "detail": "",
  "metadata": { "gonk_project":"group/repo", "gonk_rig":"group-repo", "gonk_bead_id":"gk-1a2b",
                "gonk_session_key":"sess-9", "gonk_rung":"glm", "gonk_attempt":"2",
                "gonk_trigger":"issue-triage" },
  "key_ref": { "secret_name":"gonk-key-group-repo-1a2b3c4d", "secret_key":"LITELLM_API_KEY" },
  "reservation_id": "rsv-7f3c", "reservation_expires_at": "2026-07-13T11:00:00Z",
  "budget":    { "monthly_cost_usd": 10, "monthly_tokens": 50000000, "per_task_tokens": 2000000 },
  "remaining": { "monthly_cost_usd": 3.42, "monthly_tokens": 18000000, "per_task_tokens": 1500000 },
  "spend_as_of": "2026-07-13T09:58:00Z" }

// 200, defer -- a NORMAL ANSWER, not an error status
{ "decision": "defer", "rung": "", "attempt": 2,
  "reason": "monthly-cost-exhausted",
  "detail": "rung \"glm\" needs $0.40, $0.02 remains this window",
  "retry_after": "2026-08-01T00:00:00Z",
  "metadata": {}, "key_ref": {"secret_name":"","secret_key":""},
  "budget": {"…":"…"}, "remaining": {"…":"…"}, "spend_as_of": "…" }

// 200, deny
{ "decision": "deny", "rung": "", "attempt": 3,
  "reason": "ladder-exhausted", "detail": "2 rungs, 2 gate failures",
  "metadata": {}, "key_ref": {"secret_name":"","secret_key":""},
  "budget": {"…":"…"}, "remaining": {"…":"…"}, "spend_as_of": "…" }
```

- `remaining.monthly_cost_usd` is **real dollars only** — synthetic
  local-model dollars are never counted as spend.
- `metadata` is `atags.Metadata()`, minted by meter — meter is the single
  charset-validation boundary for attribution-tag values. The pack stamps
  it verbatim onto every LiteLLM request.
- A `defer`/`deny` carries **empty** `metadata`, an **empty** `key_ref`, and
  **no** `reservation_id`. A decision that will not run hands out neither an
  attribution identity nor a route to a credential.
- **Quiet hours arrive here**, as `decision: "defer"`, `reason:
  "quiet-hours"`, `retry_after` = the end of the window in the project's
  timezone. Meter owns quiet hours end to end; intake has no quiet-hours
  code.
- An unknown project is `200` + `deny` / `project-not-registered`.

**Bounded `reason` set** (also the Prometheus label value):
`project-not-registered`, `disabled`, `invalid-config`,
`action-not-allowed`, `virtual-key-missing`, `quiet-hours`,
`spend-data-stale`, `ladder-exhausted`, `infra-retries-exhausted`,
`per-task-tokens-exhausted`, `monthly-tokens-exhausted`,
`monthly-cost-exhausted`.

## `POST /v1/policy/outcome`

```json
// request (meterapi.OutcomeRequest)
{ "project":"group/repo", "bead_id":"gk-1a2b", "session_key":"sess-9",
  "attempt": 2, "rung": "glm", "reservation_id": "rsv-7f3c",
  "outcome": "gate-failed" }        // success | gate-failed | infra-failed | aborted

// 200 (meterapi.OutcomeResponse)
{ "ok": true, "recorded_attempt": 2, "next": "escalate", "next_rung": "sonnet" }
```

`next`/`next_rung` are **advisory** (a preview for logs/dashboards); the
authoritative answer is the next `/decide`. An unknown `reservation_id`, or
one whose `(project, bead, attempt)` does not match, is a **400** and
records nothing — the outcome is what buys an escalation, so it must be
bound to a reservation *meter* minted. The classification the pack sends
here is load-bearing: reporting an infra failure as `gate-failed` buys an
escalation the project did not earn.

## Cost endpoints, and the synthetic-dollar labelling

```json
// GET /v1/cost/bead/gk-1a2b  (meterapi.BeadCostResponse)
{ "bead_id":"gk-1a2b", "project":"group/repo",
  "cost_usd": 0.42,                 // REAL money. Cloud rungs only.
  "synthetic_cost_usd": 0.02,       // local rungs, priced synthetically. NOT SPEND.
  "prompt_tokens": 120000, "completion_tokens": 8000, "total_tokens": 128000,
  "by_rung": [ {"rung":"qwen-local","kind":"local","cost_usd":0,"synthetic_cost_usd":0.02,
                "cost_synthetic":true,"total_tokens":100000,"calls":14},
               {"rung":"glm","kind":"cloud","cost_usd":0.42,"synthetic_cost_usd":0,
                "cost_synthetic":false,"total_tokens":28000,"calls":6} ],
  "attempts": [ {"attempt":1,"rung":"qwen-local","outcome":"gate-failed"},
                {"attempt":2,"rung":"glm","outcome":"success"} ],
  "as_of": "2026-07-13T09:58:00Z", "complete": true }
```

- **`cost_usd` and `synthetic_cost_usd` are different currencies and a
  client must never add them.** `cost_synthetic: true` on a rung breakdown
  means its dollars are an accounting fiction that gives LiteLLM's USD door
  teeth over tokens — see ADR-004's Decision 9. Dashboards must label them
  as such and default to showing real spend.
- `as_of` and `complete` appear on **every** cost response. `complete:
  false` means an open reservation exists for this bead/session — spend
  rows for it may not have landed yet.
- `GET /v1/cost/session/{key}` is what commit trailers read
  (`provenance.include_usage`), **while the session is still open** — it
  will frequently return `complete: false`. The pack must not write a cost
  trailer it does not know.
- `GET /v1/cost/project/{project}` adds `window` (`{"start":…,"end":…}`),
  `budget`, `remaining`, `by_trigger`, and `stale: bool`.

## `POST /admin/spend/sync`

```json
// 200 (meterapi.SpendSyncResponse), success
{ "spend_as_of": "2026-07-13T10:00:00Z", "rows_ingested": 14, "unattributed": 0, "synced": true }

// 200, failure -- rows_ingested/unattributed OMITTED (unknown), spend_as_of
// is the last GOOD sync's value, never zero just because this call failed
{ "spend_as_of": "2026-07-13T09:30:00Z", "synced": false, "error": "litellm unreachable" }
```

Blocks until one poll completes. This is Plan 06's e2e hand-back HB-2 (no
sleeping to observe the ledger), and also a **production** dependency: the
pack's gate-sweep step calls it before classifying an outcome.

## Error shape

Any non-200 that is not one of the shapes above (`400`, `401`, `404`,
`500`) is `meterapi.ErrorResponse`:

```json
{ "error": "bead_id is required" }
```

## See also

- `docs/adr/ADR-004-rung-policy-and-budget-enforcement.md` — the policy this
  API encodes, the failure-mode matrix, and known limits (most importantly,
  what the network-layer bypass means for "budgets cannot be bypassed").
- `docs/adr/ADR-002-config-precedence-semantics.md` — `.gonk.yml` precedence
  and the `+Inf`/`MaxInt64` -> `null` encoding this API's `Budget` type
  implements.
- `docs/adr/ADR-003-intake-trust-boundary-and-seams.md` — intake's half of
  the division of responsibility this API enforces from the meter side.
- `pkg/meterapi/meterapi.go` — the normative Go source.
