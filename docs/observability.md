# Observability: answering "why did nothing happen?" from logs

**Status: implemented by gonk-pop3.** The design argument, including the call on
OpenTelemetry, lives in `docs/plans/dev-observability.md`.

This document is the operator's half: what records exist, what to grep, and
which switch to turn on when a grep is not enough.

## The one thing to know

There is no trace id. **The correlator is the bead anchor**,
`gonk:<project_id>:issue:<iid>`, which every process computes identically from
the work item itself. One grep follows a work item across all four:

```
kubectl logs deploy/gonk-intake     | grep gonk:75:issue:71
kubectl logs deploy/gonk-controller | grep gonk:75:issue:71
```

On the inbound edge, before a bead anchor exists, the correlator is `delivery` --
GitLab's `X-Gitlab-Event-UUID`, which also appears in GitLab's own hook delivery
log at *Settings -> Webhooks -> Edit -> Recent events*. The receiver's record and
the dispatch record for one delivery carry the same value.

## What is always on

None of these can be switched off, and none of them carry issue text, comment
bodies, model output or credentials.

| record | written by | one per | says |
|---|---|---|---|
| `webhook delivery` | gonk-intake | GitLab delivery | delivery id, event, outcome, HTTP status, project/issue/note ids |
| `intake decision` | gonk-intake | queued event | `decision` (dispatched / deferred / denied / ignored / error) and, for everything but a dispatch, a `reason` |
| `transcript read` | gonk-gate sweep | classified session | turn count, role histogram, byte length, sha256 prefix, whether the batch fence was found |
| `session spend` | gonk-gate sweep | classified session | model call count, prompt/completion token split, cost, per-rung breakdown, `complete` |
| sweep health | gonk-gate sweep | sweep pass (30s) | whether the outcome path is alive; escalates to `SWEEP IS DEAD` |

### Diagnosing a comment or issue that produced nothing

```
kubectl logs deploy/gonk-intake | grep '"msg":"intake decision"' | grep <issue iid>
```

Every non-dispatch names a reason. The vocabulary:

| reason | meaning |
|---|---|
| `mr_event` | a merge-request event; it is a reconcile signal, never work |
| `unknown_project` | the project is not in intake's cache yet; a reconcile was kicked |
| `stale-config` | the project's cache entry is older than the staleness window -- fail closed |
| `blocked_project` | `GONK_BLOCKED_PROJECTS` names it |
| `not_a_trigger` | an issue `update`, or a note on something other than an issue |
| `no_mention` | the comment does not address the bot |
| `mentions_disabled` | `.gonk.yml` sets `respond_to_mentions: false` |
| `state_<x>` | the project is not dispatchable in that state |
| `action_disabled` | triage is off for the project |
| `decide_error` / `fire_error` / `decide_unknown` | a dependency failed; the work is retried, not lost |
| `BUG_no_reason_recorded` | **a defect in gonk**. A code path reached the record without setting a reason. File it. |

A `decision=dispatched` record means intake did its job: the order was fired,
and the investigation moves to the controller. Grep the bead anchor there.

## The dev-only switch: session transcript archive

`gonk-sweep` reads a finished session's transcript to classify it, and the
reaper then destroys the agent pod. **The transcript exists nowhere else.** The
always-on `transcript read` record describes its shape; the archive keeps the
bytes.

```yaml
# values.yaml -- DEV ONLY, off by default
dev:
  transcripts:
    enabled: true
    dir: /transcripts
    sizeLimit: 512Mi
```

Then:

```
kubectl exec deploy/gonk-controller -- ls -lt /transcripts
kubectl exec deploy/gonk-controller -- cat /transcripts/gonk-75-issue-71--<alias>.json
```

**Retention, stated plainly.** The archive is an `emptyDir` on the controller
pod. It outlives the agent pod -- which is where the evidence was being lost --
and it does **not** outlive a controller restart. `gonk-gate` prunes to the
newest 200 files, each file capped at 4 MiB and written 0600.

**Why not a PVC.** A transcript is untrusted model output over untrusted user
input. Durable storage of that, with no retention policy and no access control,
is a much bigger commitment than "let me see what the agent said this
afternoon". If you need durability, ship the records to a log store rather than
persisting a volume.

**Why it is off by default and the metadata record is not.** Default-off logging
that nobody turns on would not have caught any of the 2026-09-12 failures. So
only the expensive-or-untrusted half is gated; everything cheap and safe is
always on.

## A dead subsystem shows up as `0/1 READY`, not as a log line

On 2026-09-12 `gonk-sweep` could not reach its own database and logged an ERROR
every 30 seconds **for days** while the entire outcome path was dead: sessions
completed, nothing was ever posted, and beads were stranded permanently
(gonk-p7qh).

Every sweep pass now records its own health to
`GONK_SWEEP_HEALTH_FILE` (default `/city/gonk-sweep-health.json`), and the
controller's **readinessProbe** is `gonk-gate sweep-health`. So:

```
$ kubectl get pods -l app.kubernetes.io/component=controller
NAME                               READY   STATUS
gonk-controller-7c9f4b8d6-xk2ql    0/1     Running
```

Readiness is how a dead outcome path becomes **visible**. It is not what stops
dispatch: that was the original design claim (gonk-pop3), and a live outage
showed it is wrong -- intake refuses because Gas City refuses, from the first
minute, whatever readiness says (below). An unready controller does leave its
Service's endpoints, which turns that refusal into `connection refused`.
Nothing kills the pod: liveness is untouched, the sweep keeps retrying, and
readiness returns on its own.

**What a dead bead store actually looks like** (gonk-7s9p, observed on a kind
cluster with the Dolt `gc` password rotated server-side):

- **It is caught by staleness, not by a failed pass.** Gas City will not launch
  `gonk-sweep` while it cannot read its own order-tracking beads, so no pass
  runs, nothing records a failure, and `SWEEP IS DEAD` is never logged. The ok
  record just ages. `gascity.sweepHealthMaxAge` is therefore the time to go
  `0/1`: 14m31s after the break, on 15m. The probe says so, and names the
  likely cause: `NOT READY: the last sweep pass was ... -- the cooldown order is
  not running; Gas City does not launch it while the bead store is unreachable`.
  The confirming line is Gas City's own, in the controller log:
  `gc: order dispatch: checking open work for gonk-sweep: ... Access denied`.
- **Intake refuses from the first minute, not from `0/1`.** Gas City's order-run
  endpoint answers `503 ... creating tracking bead` while the store is down, and
  intake logs that as `reason=fire_error`. Once the pod is unready the error
  becomes `connection refused`. Either way nothing is dispatched.
- **Refused issue work comes back; refused mentions do not.** The reconciler's
  issue sweep re-dispatches open issues with no `gonk::` label (updated in the
  last 24h, 5 per project per pass), so a refused issue is picked up on the first
  reconcile pass after recovery -- 6m and 10m later in two runs; the bound is
  `intake.reconcileInterval` (10m). Until then every pass logs `fire_error`. A
  refused `@gonk` mention is not replayed: notes have no reconcile path (read
  from `pkg/intake/reconcile.go`, not observed), so the mention reply is lost,
  and on an issue that already carries a `gonk::` label nothing at all comes
  back. An outage longer than 24h also ages issues out of the sweep.
- Recovery: 11-19s from the store returning to a passing record, 16-26s to
  rejoin the Service.

**Why the window stays at 15m** (owner decision, gonk-7s9p). A 7m window was
tried and went `0/1` 7m15s after the break, but it would flap on a healthy
controller under disk pressure, which gonk must tolerate. Under pressure Gas
City dispatches orders only on every 6th patrol tick (180s); each tick runs two
bounded open-work gates, either of which skips a non-idempotent order when it
times out, and nothing bounds how many ticks in a row that happens; and a tick
that overruns its slot drops pending ticks. One gate miss already makes a
healthy gap of 11m30s, two make 14m30s. A false unready is the one case where
readiness changes dispatch -- and a refused mention is never replayed -- while
a tighter window would only show an outage intake is already refusing a few
minutes sooner. Faster dead-store detection is gonk-hkjh.

Ask it directly:

```
kubectl exec deploy/gonk-controller -- gonk-gate sweep-health
kubectl exec deploy/gonk-controller -- cat /city/gonk-sweep-health.json
```

A **missing** health record reads as READY, not failed: a fresh pod has not
completed a pass yet. The bootstrap initContainer seeds an ok record stamped at
pod start, so the staleness check runs from then -- a `gonk-sweep` order that
never runs at all cannot hide behind "no record yet".

When the sweep does run and its store reads fail (the gonk-p7qh shape: broken
for the sweep, working for Gas City), a repeated failure escalates from
`sweep: pass failed` to one cumulative statement, so the log says how long it
has been broken. It is NOT logged when the whole store is down, because then
the sweep never runs (above):

```
level=ERROR msg="SWEEP IS DEAD: the outcome path is down -- no session can be
classified, no outcome reported, and every finished session is being discarded"
stage=list_running consecutive_failures=1442 outage=12h1m0s
```

Recovery says so once: `sweep: RECOVERED -- the outcome path is alive again`.

## Knobs

| setting | default | what it does |
|---|---|---|
| `dev.transcripts.enabled` | `false` | dev-only transcript archive |
| `dev.transcripts.dir` | `/transcripts` | where it writes (`GONK_TRANSCRIPT_DIR`) |
| `dev.transcripts.sizeLimit` | `512Mi` | the emptyDir bound |
| `gascity.sweepHealthMaxAge` | `15m` | how stale a health record may be before readiness fails -- and so how long a dead bead store takes to go `0/1` (gonk-7s9p). Deliberately not tighter; see "Why the window stays at 15m". `internal/buildgate/sweep_health_max_age_test.go` refuses anything below 12m |
| `GONK_SWEEP_HEALTH_FILE` | `/city/gonk-sweep-health.json` | where the sweep records its health |
| `GONK_LOG_SINK` | `/proc/1/fd/1` | where `gonk-gate` writes in addition to stderr (see `cmd/gonk-gate/logsink.go`) |

## What is deliberately NOT here

- **OpenTelemetry.** Argued in `docs/plans/dev-observability.md`: gonk's most
  interesting hop is a Gas City exec-order fork, and upstream Gas City provably
  drops caller vars (gonk-6gs), so a trace context cannot survive it. The bead
  anchor can, and already does.
- **A log line per model turn.** gonk does not own the model loop; opencode runs
  inside the agent pod, and a per-turn line would have to come out of the pod the
  reaper is about to destroy. The turn count on `transcript read` and the call
  count on `session spend` answer the same question from processes that survive.
