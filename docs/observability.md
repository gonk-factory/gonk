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

An unready controller leaves its Service's endpoints, so intake's `FireOrder`
fails (`reason=fire_error`) and work is **not dispatched** -- rather than
dispatched into a session whose outcome can never be written, which is exactly
how gonk-p7qh stranded beads. Nothing kills the pod: liveness is untouched, the
sweep keeps retrying, and readiness returns on its own.

Ask it directly:

```
kubectl exec deploy/gonk-controller -- gonk-gate sweep-health
kubectl exec deploy/gonk-controller -- cat /city/gonk-sweep-health.json
```

A **missing** health record reads as READY, not failed: a fresh pod has not
completed a pass yet.

In the log, a repeated failure escalates from `sweep: pass failed` to one
cumulative statement, so the log says how long it has been broken:

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
| `gascity.sweepHealthMaxAge` | `15m` | how stale a health record may be before readiness fails |
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
