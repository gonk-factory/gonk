# gonk-ij2e — TestMeasureLiteLLMSpendLogLag: the row was never written

**Date:** 2026-09-10 · **Branch:** `spike/ij2e-spend-log-lag` · **Verdict: H1 —
the spend row never reaches Postgres at all — but the cause is OURS, not
LiteLLM's.** The stub upstream reused a completion id, and LiteLLM's spend-log
writer discards a duplicate `request_id` in silence.

---

## The answer in one paragraph

`test/stubmodel` numbered every scripted completion `chatcmpl-stub-%04d` from a
counter that `Server.Reset()` and `Server.SetScript()` both rewound to zero.
`newWorld(t)` calls `Reset` at the top of **every** component test (individual
tests call `SetScript` separately; either one alone was enough to rewind), so
every test's first completion was issued the id
`chatcmpl-stub-0001` — an id the hard-door test had already burned earlier in
the same package run. `LiteLLM_SpendLogs` has `request_id` as its **PRIMARY
KEY**, and LiteLLM's batch writer inserts with
`create_many(data=rows, skip_duplicates=True)`, i.e. `ON CONFLICT DO NOTHING`.
So the lag test's spend row was **silently discarded**: no error, no retry, no
log line, no row — ever. Run in isolation the table was empty, no id collided,
and the lag was 1–2s. That is the whole difference between "1–2s alone" and
"times out at 100s / 3m in the full package", and it is why the original spike
(`docs/spikes/litellm-verified.md`, P3-3) concluded LiteLLM's background writer
was "large and unreliable". It is not. It flushes in a few seconds. It was
being handed rows it had already stored.

**Hypotheses as posed:** H1 (lost at the writer) — **supported, with the cause
being our stub, not LiteLLM's batching**. H2 (gonk's `/spend/logs/v2` query is
wrong) — **ruled out by measurement**: at every sample the API's `total` equalled
the row count Postgres itself reported for the same window. H3 (late but
bounded) — **ruled out**: 20 minutes, still nothing, and nothing was pending
anywhere to arrive.

---

## Method

Rig: the component suite's own fixtures (`harness.StartPostgres` +
`harness.StartLiteLLM`, `postgres:16` and `ghcr.io/berriai/litellm-database:v1.92.0`
under podman `--network=host`), plus a scratch probe test that polls **two
surfaces at once** for the same completion:

1. **Raw SQL**, straight at Postgres over pgx, bypassing LiteLLM entirely:
   ```sql
   SELECT request_id, "startTime", "endTime"
     FROM "LiteLLM_SpendLogs"
    WHERE metadata::text LIKE '%' || $1 || '%'
    LIMIT 1;
   ```
   plus, every 15s, the in-window row count Postgres itself sees:
   ```sql
   SELECT count(*) FROM "LiteLLM_SpendLogs"
    WHERE "startTime" >= $1 AND "startTime" <= $2;
   ```
2. **The exact path gonk uses** — `HTTPSpendSource.Since`, plus a raw fetch of
   the same URL so the envelope could be read directly. The request gonk emits,
   copied from the probe log verbatim:
   ```
   GET http://127.0.0.1:4000/spend/logs/v2
       ?end_date=2026-09-10+08:08:31&page=1&start_date=2026-09-10+06:08:31
   Authorization: Bearer <proxy_admin key>
   ```
   The probe also walked **every** page (`1..total_pages`) looking for the row's
   `request_id`, so a pagination defect would have been distinguishable from an
   absent row.

The probe ran **last in the package**, after the hard-door, local-door,
reservation-race and key-churn tests had generated their real call and key
volume — the exact condition under which the failure was reported.

---

## Raw timings

### Run A — probe alone, with self-generated churn (8 keys × 3 calls, every key deleted)

```
PRE: LiteLLM_SpendLogs has 0 rows
churn: 8 keys x 3 calls, all keys deleted
POST-CHURN: LiteLLM_SpendLogs has 20 rows
t0=2026-09-10T08:03:45.000044367Z  project=acme/probe-369572-9
DB   FIRST SEEN at +4.115s  request_id=chatcmpl-stub-0025 startTime=2026-09-10T08:03:45.005Z
API  FIRST SEEN at +4.144s  call_id=chatcmpl-stub-0025
RESULT dbAt=4.115321482s apiAt=4.144144186s tableRows=25
```

Key-churn on its own does **not** reproduce it. The deleted keys' rows persisted
fine — `LiteLLM_SpendLogs` has no foreign key to `LiteLLM_VerificationToken`, so
deleting a key does not cost you its spend rows. **The key-churn mechanism for
H1 is disproved.** Note also that the ids here ran `0001..0024` for the churn and
`0025` for the probe: one `newWorld` at the top, then everything inside a single
test, so nothing rewound the counter and nothing collided.

### Run B — full package, probe last (the real reproduction)

```
TestMeasureLiteLLMSpendLogLag: row for acme/lag-630521-15 did NOT appear within 3m0s
--- FAIL: TestMeasureLiteLLMSpendLogLag (180.07s)

TestZZIJ2ESpendLogProbe:
PRE: LiteLLM_SpendLogs has 13 rows
t0=2026-09-10T08:08:31.603718665Z  project=acme/probe-29246-21
+0s     API blind. gonkRows=0 apiTotal=13 dbWindowRows=13 page_size=50 total_pages=1 dbRowFoundOnPage=-1
+15s    API blind. gonkRows=0 apiTotal=13 dbWindowRows=13 ...
+31s    ... +46s ... +1m1s ... +1m32s ... +2m2s ... +3m3s ... +3m34s ...
        (every 15s for twenty minutes; apiTotal and dbWindowRows never move off 13)
RESULT project=acme/probe-29246-21 dbAt=0s apiAt=0s tableRows=13
H1: row NEVER reached Postgres within 20m0s
--- FAIL: TestZZIJ2ESpendLogProbe (1201.64s)
```

**`dbWindowRows` — Postgres' own count, read over pgx with LiteLLM out of the
loop — stayed at 13 for the full twenty minutes.** The row is not late. It does
not exist.

**And `apiTotal == dbWindowRows` at every single sample.** The `/spend/logs/v2`
query gonk issues is a faithful mirror of the table: same window, same count,
every time. That is the measurement that kills H2.

### The table, at the moment the lag test was timing out

```
              request_id              |        startTime        |           alias
--------------------------------------+-------------------------+---------------------------
 chatcmpl-stub-0001                   | 2026-09-10 08:05:25.810 | "gonk-harddoor-258617-1"
 chatcmpl-stub-0002                   | 2026-09-10 08:05:26.057 | "gonk-harddoor-258617-1"
 chatcmpl-stub-0003                   | 2026-09-10 08:05:26.064 | "gonk-harddoor-258617-1"
 chatcmpl-stub-0004                   | 2026-09-10 08:05:26.073 | "gonk-harddoor-258617-1"
 chatcmpl-stub-0005                   | 2026-09-10 08:05:26.080 | "gonk-harddoor-258617-1"
 e688c00d-9544-47c5-a72e-e9dea87a23eb | 2026-09-10 08:05:26.092 | "gonk-harddoor-258617-1"
 chatcmpl-stub-0006                   | 2026-09-10 08:05:26.165 | "gonk-localdoor-600881-2"
 chatcmpl-stub-0007 .. 0010           | 2026-09-10 08:05:26.19x | "gonk-localdoor-600881-2"
 2eed95c4-d18e-4828-be92-238cf6e27b92 | 2026-09-10 08:05:26.206 | "gonk-localdoor-600881-2"
 1113fa0c-33a5-41f9-8fd9-bc095cd714e5 | 2026-09-10 08:05:29.511 | null
(13 rows)
```

`chatcmpl-stub-0001` belongs to `gonk-harddoor`, written at 08:05:25.810. The lag
test's first completion — made around 08:05:40 under key `gonk-lag-…` — was
handed that same id by the stub, and there is no `gonk-lag` row anywhere. Note
the three UUID ids: those are the rows LiteLLM writes for calls with **no
upstream response** (the 429 budget-exceeded refusals), where it mints its own
`request_id`. They are unique, which is exactly why the hard-door tests were
never affected by this and only the one test that waits for a row ever noticed.

---

## The mechanism, in LiteLLM's own source

`ghcr.io/berriai/litellm-database:v1.92.0`,
`litellm/proxy/utils.py`, `_create_spend_logs_with_poison_isolation`:

```python
await repo.table.create_many(data=rows, skip_duplicates=True)
```

and the schema, from `\d "LiteLLM_SpendLogs"` on the live database:

```
Indexes:
    "LiteLLM_SpendLogs_pkey" PRIMARY KEY, btree (request_id)
```

`skip_duplicates=True` compiles to `ON CONFLICT DO NOTHING`. A row whose
`request_id` is already present is dropped by Postgres without raising, so
LiteLLM's retry and its poison-row isolation never engage, and nothing is
logged. `grep -ic "duplicate|unique constraint"` over the proxy's full container
log for the failing run: **0**.

**This is a real production property, not only a test artefact.** If any
upstream ever returns a completion id gonk has already seen — a provider that
recycles ids, a replayed request, a cache — the meter loses that spend
permanently and silently, with no error to alert on. It undercounts. Worth
knowing; nothing to do about it today, because real providers do not reuse ids
and the only thing that did was ours.

## What the query path does, for the record (H2, ruled out)

Read in full and checked against the live endpoint's source
(`litellm/proxy/spend_tracking/spend_management_endpoints.py::ui_view_spend_logs`):

- `internal/meter/litellm/spendsource_http.go` sends `start_date` and `end_date`
  on every page in `2006-01-02 15:04:05`, UTC — the format the endpoint parses
  with `datetime.strptime(...).replace(tzinfo=timezone.utc)`. Correct, and the
  only accepted alternative is bare `YYYY-MM-DD`.
- The endpoint's SQL bounds are
  `"startTime" >= ($1::timestamptz AT TIME ZONE 'UTC')` and the matching `<=`,
  against a `timestamp`-typed column holding UTC. No timezone gap.
- Sort defaults are `sort_by=startTime`, `sort_order=desc`, `page_size=50`
  (max 100), so **a new row is always on page 1** and could not be hidden behind
  pagination. The probe confirmed this empirically by walking `1..total_pages`
  anyway.
- `end_date` is truncated to the second by `Format`, so a row can be excluded for
  at most ~1s until the wall clock ticks past it. Bounded, sub-second, harmless.

No defect found in gonk's query path. It is a correct and faithful reader of the
table.

---

## The fix

`test/stubmodel/server.go`: the completion-id counter is now **monotonic for the
life of the Server**. `Reset()` and `SetScript()` still clear the script and the
call log — that is what tests need from them — but neither rewinds the id.
A completion id is an identity, not a counter, and real providers never reuse
one.

Regression test: `TestCompletionIDsAreNeverReusedAcrossResetOrSetScript` in
`test/stubmodel/server_test.go`. It drives three Reset+SetScript rounds of two
calls each and requires six distinct ids. Against the pre-fix stub it fails on
the second round with `completion id "chatcmpl-stub-0001" reused`.
`TestResponsesAreByteStable` still passes: it compares two *fresh* `New()`
servers, so per-Server determinism is untouched.

**No threshold was changed.** `lagDangerThreshold` remains
`maxSpendStaleness - lagSafetyMargin` = 3m, and the test still asserts the real
invariant. It now measures the real lag instead of a lost row.

## Corrections to earlier documents

`docs/spikes/litellm-verified.md`, P3-3 ("Real spend-log lag") records that the
detailed-log write lag was "large and unreliable" and that completions
"frequently produced no visible row within 100s", and floats a proxy restart as
the explanation. **That conclusion was wrong**, and it was wrong in the
expensive direction: it attributed our own duplicate-id defect to LiteLLM's
batching and left a P1 "config/infra risk" open against it.

**WITHDRAWN AFTER REVIEW.** An earlier version of this paragraph also dismissed
the spike's "spike artifacts of restarting a single-node proxy mid-flight"
guess, on the grounds that a restart is "simply another way to rewind the stub's
counter". That does not follow: restarting the LiteLLM proxy leaves the
stubmodel `Server` process — and therefore its counter — untouched. Only
restarting the whole rig including the stub would rewind it, and nothing records
that that is what happened. **The proxy-restart observation remains
unexplained.** It is also worth being precise about the scope of what follows:
the duplicate-id defect is PROVEN to be the cause of the 2026-09-10 failure
(one change, four reproductions, green), and INFERRED — plausibly, but not
measured — to be the cause of the July observations, which were never re-run.

## P3-3, answered

With the fix, against the same rig and the same preceding load:

Full package, all 17 tests, single run (`full2.log`):

```
--- PASS: TestMeasureLiteLLMSpendLogLag (1.34s)
    lag sample 1: 237ms
    lag sample 2: 1.094s
    SPEND-LOG LAG (n=2): min=237ms median=1.094s max=1.094s
```

and the probe, running last, after every other test's load:

```
PRE: LiteLLM_SpendLogs has 22 rows
DB   FIRST SEEN at +2.068s  request_id=chatcmpl-stub-0020
API  FIRST SEEN at +2.085s  call_id=chatcmpl-stub-0020
RESULT dbAt=2.067654055s apiAt=2.084806128s
```

| | lag |
|---|---|
| spend row visible in Postgres | 0.24s – 2.07s |
| spend row visible via `/spend/logs/v2` (what meter polls) | 0.24s – 2.09s — **17ms** behind the DB |

The API is not the slow part. It was never the slow part.

`proxy_batch_write_at: 1` (already set by `renderLiteLLMConfig`) is doing its
job. The floor is a couple of seconds, set by LiteLLM's queue monitor
(`SPEND_LOG_QUEUE_POLL_INTERVAL` = 2.0s, with a 1.5x backoff to a 30s ceiling
while the queue stays under `SPEND_LOG_QUEUE_SIZE_THRESHOLD` = 100 — so on a
quiet proxy, expect worst-case ~30s, still an order of magnitude inside
`max_spend_staleness`). **`max_spend_staleness: 5m` is not wrong, and neither is
the deployment shape.**
