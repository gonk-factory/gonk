# Spike: does Dolt serialize the concurrent-reservation race?

**Plan 03, Task 0b.** Blocking spike, run before any code depends on the
answer. Status: **RUN. Decision made. Dolt is not viable for enforcing the
budget-ceiling race at the store layer.**

## The property under test

> Given a budget ceiling with headroom for exactly **K** reservations, when
> **N ≫ K** concurrent writers each (a) read the sum of open reservations,
> (b) decide whether their reservation fits, and (c) write it — **exactly K
> succeed, and the sum of persisted reservations never exceeds the
> ceiling.**

This is the read-then-write race, and it is the only thing standing between
a budget and two sessions spending it twice. It is not a durability question
and not a "does Dolt work" question.

## What was tested

- **Dolt version:** `2.1.10` (`SELECT dolt_version()`), from the image
  `docker.io/dolthub/dolt-sql-server:latest`, pulled 2026-07-14. Image
  digests at pull time:
  `sha256:73ddd95b35dc71774b062226c730a0fe865fc213c70e2598519bfef0d7d19759`
  (and a second-arch manifest
  `sha256:dc8a878e80b06fc7fd7bc661a2bd0d0e83b771a5965e6818deb83c61ed73e4a0`).
- **How it was stood up:**
  `docker run -d --rm --network=host --name gonk-dolt-spike dolthub/dolt-sql-server:latest`.
  Podman's CNI bridge networking is broken in this environment (`plugin
  firewall does not support config version "1.0.0"` on every bridged
  network); `--network=host` is required for the container to be reachable
  at `127.0.0.1:3306` at all. The image defaults to port `3306`, auth is
  `root` with no password and no TLS (confirmed by connecting, not assumed).
- **Driver:** `github.com/go-sql-driver/mysql v1.9.3`, added to `go.mod` and
  vendored (`go mod vendor`) so CI — which cannot reach the Go proxy — still
  builds. This is also the driver Task 6's `store/dolt.go` will use.
- **Test file:** `internal/meter/store/dolt_race_test.go`, build-tagged
  `//go:build dolt` so it is invisible to the normal gate (`go build ./...`,
  `go vet ./...`, `go test ./... -race`, `golangci-lint run ./...` never
  touch it; there is no Dolt in CI). Run it explicitly:

  ```bash
  docker run -d --rm --network=host --name gonk-dolt-spike dolthub/dolt-sql-server:latest
  GONK_DOLT_DSN="root@tcp(127.0.0.1:3306)/" \
    go test -tags dolt ./internal/meter/store/ -race -run Race -count=2 -v
  docker stop gonk-dolt-spike
  ```

## Schema and race shape

Per project (`p`), a `projects` row carries the ceiling (`$1.00`); a
`reservations` table carries one row per accepted reservation
(`amount = $0.40`), so headroom exists for **exactly 2** of **32** concurrent
writers. Each writer runs, with **no in-process lock** (the property under
test is the store's atomicity, not a service-side mutex):

```sql
BEGIN;
SELECT COALESCE(SUM(amount),0) FROM reservations WHERE project = ?;
-- if sum + amount > ceiling: ROLLBACK
INSERT INTO reservations (id, project, amount) VALUES (?, ?, ?);
COMMIT;
```

Three strategies were run, in the escalation order Task 0b Step 3
prescribes:

| Strategy | Mechanism |
|---|---|
| `naive_default_isolation` | Plain `BEGIN` (driver/server default isolation), no explicit lock. The shape a "read, check, write" implementation takes if nobody asks the database for anything extra. |
| `serializable_isolation` | Same shape, `BeginTx` with `sql.LevelSerializable` (`SET TRANSACTION ISOLATION LEVEL SERIALIZABLE`) instead of the default. |
| `select_for_update` | The production-shaped strategy: locks the project's ceiling row first — `SELECT ceiling FROM projects WHERE name = ? FOR UPDATE` — then reads the sum and inserts, all in one transaction. This is "the sequence `Service.Decide` performs, with NO in-process lock," per Task 0b's test comment. |

Each strategy was run for 5 iterations per `go test` invocation
(`raceIterations` in the test file), with a **fresh schema per iteration**
(a new Dolt database, created and dropped per run), and the whole binary was
invoked with `-count=2` on top of that — **30 independent race iterations
per strategy** across the runs that produced the numbers below. `-count`
alone is not decoration: a race that manifests one run in three is still a
race.

## Result: FAIL, decisively, at every escalation level

**Every single iteration, under all three strategies, all 32 writers won.**

```
[naive_default_isolation]  summary: 5/5 iterations overspent, 0/5 iterations won exactly 2
[serializable_isolation]   summary: 5/5 iterations overspent, 0/5 iterations won exactly 2
[select_for_update]        summary: 5/5 iterations overspent, 0/5 iterations won exactly 2
```

(Reproduced identically across two full `-count=2` binary invocations — 30
iterations per strategy, 90 total — with zero variance.)

Per iteration: `wins=32 losses=0 deferrals=0 otherErrors=0 total=$12.80
ceiling=$1.00`. Not one transaction reported a serialization failure, a
lock-wait timeout, or any error at all — every one of the 32 concurrent
transactions read `sum=0` (or whatever partial sum its snapshot showed at
the moment it read), decided it fit, and committed. Persisted total was
**$12.80 against a $1.00 ceiling — 12.8x overspend**, every time, at every
isolation level and locking strategy tested, including the one
(`select_for_update`) shaped exactly like the production `ReserveIfFits`
sequence.

### `SELECT ... FOR UPDATE` was independently confirmed to be a no-op

Because "all 32 win even under `FOR UPDATE`" is a strong enough claim to
deserve direct verification (not just inference from the race numbers), a
second, targeted check was run outside the race harness: transaction 1 runs
`SELECT v FROM t WHERE name='p' FOR UPDATE` and **does not commit**.
Transaction 2, on a separate connection, immediately runs the identical
`SELECT ... FOR UPDATE` against the same row. If Dolt enforced row locking,
transaction 2 would block until transaction 1 committed or rolled back.

**It did not block.** Transaction 2's `SELECT ... FOR UPDATE` returned in
**462.847µs** — while transaction 1's lock was still held, uncommitted. This
confirms the race result is not a harness artifact: `FOR UPDATE` on Dolt
2.1.10 does not take a lock that blocks a concurrent reader/writer at all.

### Why this happens (consistent with Dolt's documented concurrency model)

Dolt's concurrency control is optimistic and commit-time, not
pessimistic/lock-based. Because every writer in the race inserts a
**distinct row** (a unique reservation `id`), there is no row-level
write-write conflict for an optimistic-commit engine to detect — the check
(`sum + amount > ceiling`) is evaluated against a stale read with nothing
downstream ever noticing the aggregate itself was raced. `SELECT ... FOR
UPDATE` and `SERIALIZABLE` are accepted syntactically (no error, no
rejection) but do not change this behavior in the version tested.

## Decision

**Dolt is not viable for enforcing `ReserveIfFits`'s atomicity.** This
result is unambiguous, reproduced across 90 independent iterations with zero
variance, and independently corroborated by a direct lock-blocking test — it
is not "inconclusive," it is a clean **FAIL** at the strongest escalation
Task 0b's decision table names (`SERIALIZABLE` + explicit `FOR UPDATE`). Per
Task 0b Step 3's table: *"More than 2 win even under `SERIALIZABLE` → Dolt
cannot back the reservation path. Fall back."*

Task 0b names two fallbacks, in preference order:

1. **Single-writer path per project (recommended by the plan).** Keep Dolt
   for durability and the audit history; make **meter** authoritative for
   serialization by running **single-replica** (AD-10). The service's
   existing per-project `keyedMutex` then *is* the isolation — not a
   contention optimization on top of a store guarantee that turned out not
   to exist — and the store only has to not lose writes, which this spike
   never tested and has no reason to doubt. Cheapest option; costs
   horizontal scaling of a component (meter) that doesn't need it.
2. **Postgres for the ledger, on the owner's existing CNPG cluster
   (owner-approved, 2026-07-13).** `SERIALIZABLE` / `SELECT ... FOR UPDATE`
   is well-trodden for exactly this race on Postgres — unlike Dolt, Postgres
   implements true predicate/row locking and commit-time serializable
   snapshot isolation, so this is a mechanism verified by decades of
   production use, not a novel claim needing its own spike. Not a new
   stateful dependency — CNPG is already run in-cluster. Costs Dolt's
   versioned audit history (mitigated by append-only ledger tables).

**OWNER DECISION (2026-07-14): Fallback 2 — CNPG Postgres for the ledger.**
The spike agent recommended Fallback 1 (single-replica Dolt + in-process
mutex); the owner overrode it in favour of Postgres, for a decisive reason:
the reservation race is THE core money-safety property, and Fallback 1 makes
it depend on meter never running more than one replica — a single
`replicas: 2` misconfiguration silently reintroduces the 12.8× overspend this
spike found, with no error. That is too fragile a foundation for the
guarantee. Postgres on the existing CNPG cluster enforces the race in the
database itself (`SERIALIZABLE` / `SELECT ... FOR UPDATE`, decades of
production use), so the safety property holds regardless of replica count, and
meter can scale. CNPG is already deployed — not a new dependency. The cost is
Dolt's versioned audit history for the ledger, mitigated by append-only ledger
tables. **Dolt is NOT dropped from the system** — it remains Gas City's beads
store (versioned audit history, no reservation-race requirement); it is simply
not meter's reservation store.

**Consequence for Task 6:** the store targets **Postgres/CNPG**. `store.Store`'s
`ReserveIfFits` gets its atomicity from Postgres `SERIALIZABLE` +
`SELECT ... FOR UPDATE` (the verified mechanism), NOT from an in-process mutex
and NOT from Dolt. There is no `store/dolt.go` for the ledger. Task 6 adds a
Postgres driver (pgx or lib/pq), and this spike's `go-sql-driver/mysql`
dependency + the `//go:build dolt` race test are removed (the finding lives in
this doc; the test can never run in CI without Dolt anyway). `meter` is NOT
constrained to single-replica by the store.

**`ADR-004` (Task 10) cites this document** and must record explicitly that
Dolt's transactional guarantees were measured and found insufficient for
this property — not assumed, not skipped.

## Raw numbers (for citation)

- Iterations run: 30 per strategy × 3 strategies = 90 total (two `-count=2`
  binary invocations, 5 internal iterations per strategy per invocation).
- Wins per iteration: **32 / 32** writers won, every iteration, every
  strategy (expected: exactly 2).
- Overspend: **$12.80 persisted against a $1.00 ceiling**, every iteration,
  every strategy (12.8x).
- Errors/deferrals: **0** — no serialization failures, no lock-wait
  timeouts, no errors of any kind were ever raised by Dolt during the race.
- Direct `FOR UPDATE` lock-blocking check: lock **not enforced** — a second
  transaction acquired the "locked" row in 462.847µs while the first
  transaction's lock was still held, uncommitted.
