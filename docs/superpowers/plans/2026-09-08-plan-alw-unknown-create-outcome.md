# gonk-alw: an UNKNOWN create outcome must park the bead, not burn an attempt

> **SUPERSEDED IN DIRECTION 2026-09-08 by the independent review's T-45**
> (`docs/reviews/2026-09-08-delivery-plan.md`): Kubernetes Jobs replace Gas
> City as the session runtime by default, with convoys re-evaluated in v3
> behind a `SessionRuntime` interface.
>
> This plan is entirely about Gas City's startup ordering and its
> create/submit/fetch handshake, so it **should not be implemented against a
> runtime that is being replaced.** The DIAGNOSIS below still stands as a
> measured record of what the current runtime does, and the defect it found
> is worth carrying forward as a REQUIREMENT on whatever replaces it: an
> unconfirmed create must never be treated as a silent success. Keep that
> property; discard the implementation.

Status: PROPOSED, 2026-09-08
Issue: gonk-alw (P0) — *A cold controller accepts a session and silently never
creates the pod*

---

## 0. What the investigation established

gonk-alw asked three things. **(3) is already shipped** (`800a5f8`,
`describeSessionRuntime` reports `NO AGENT EVER RAN`). **(1) is answered below by
measurement.** **(2) is the work in this plan**, and it turned out to be gonk's,
not Gas City's.

### The controller serves before it finishes starting — measured

Polled from inside the cluster (from the `litellm` pod, so the sampler survives
pod replacement — a `kubectl port-forward` does not, and a first attempt captured
nothing but `socket=CLOSED` because of that), across two `rollout restart`s.

The controller's own log:

```
API server listening on http://0.0.0.0:9443
gc start: startup phase=adoption-barrier elapsed=19ms
21:35:51 api: GET /v0/city/gonk/sessions 200        <- served mid-startup
gc start: startup phase=config-reload elapsed=344ms
21:35:51 api: GET /health 200
```

This **contradicts gonk-fsl's premise** that 9443 does not bind until init
finishes. The socket binds first; routes serve while phases are still running.

And there is a window where the API answers but `/health` does not:

```
21:37:52.502   health=0     sessions=200
21:37:52.519   health=200   ready=True
```

`/health` **never once reported `ready:false`** — it does not answer at all until
it is ready. So the discriminator is *"health absent"*, not *"health says no"*.

All three chart probes are `tcpSocket`, which **passes** in that window; an
`httpGet /health` probe would fail it. So a probe swap does close a real window.

### …but that window cannot explain this bug

It measured **~2.3s** and **~17ms**. The incident happened at **42 seconds** of
uptime, by which point `/health` answers `ready:true`. The probe change is
worthwhile hardening and is **not** the fix. It is filed separately and must not
be reported as closing gonk-alw.

### The actual defect is in gonk

`cmd/gonk-gate/broker_inject.go`, `awaitCreate`:

```go
outcome, err := d.GC.AwaitRequestOutcome(...)
if err != nil {
    // Unobserved within the window: the pod is still starting.
    d.Log.Debug("create outcome not yet reported; the pod is still starting", ...)
    return false, nil
}
```

An outcome that will **never** arrive is indistinguishable from one that has not
arrived **yet**, and this assumes the benign case — at `Debug`. `gcapi`'s own
error string says it is not benign:

```
"no terminal event for request %s within %s: the outcome is UNKNOWN, not successful"
```

That warning is discarded. So is every `findRequestOutcome` transport failure: an
unreachable event stream also reads as *"the pod is still starting"*.

This is the incident's exact signature — 202 accepted, nothing observed, dispatch
proceeds anyway, and two minutes later `awaitPromptFetched` reports *"prompt was
never fetched"*, which reads as an **agent** fault and burns a ladder attempt.
The bead's own words: *"WHAT MAKES IT DANGEROUS is not the race but the
SILENCE."* This is where gonk manufactures its own.

---

## 1. The change

### 1.1 `awaitCreate` must return three states, not two

Today it returns `(ready bool, err error)` where `(false, nil)` conflates *"not
confirmed yet, which is normal"* with *"the outcome is UNKNOWN"*. Split them:

```go
type createOutcome int

const (
    createConfirmed createOutcome = iota // terminal success event observed
    createUnknown                        // no terminal event within the window,
                                         // or the event stream was unreachable
)
```

`createFailed` stays as it is — an observed `!outcome.OK` is already a hard error
and must keep returning one.

Distinguish the two `createUnknown` causes when logging, because they have
different operator actions: a **timeout** means the controller accepted and said
nothing; a **transport error** means gonk could not ask. Both are UNKNOWN, but
only the second is gonk's own connectivity.

Log at **Warn**, not Debug, carrying `request_id` and the elapsed window. A
condition that can end a dispatch must not be invisible at default verbosity.

### 1.2 An UNKNOWN create that produced no agent PARKS the bead

Carry the `createUnknown` fact forward to the `awaitPromptFetched` failure path.
There, `describeSessionRuntime` already determines whether anything ever ran.
Combine them:

| create outcome | `describeSessionRuntime` | action |
|---|---|---|
| confirmed | NO AGENT EVER RAN | infra failure, re-sling (today's behaviour) |
| confirmed | agent ran, did not fetch | agent failure, re-sling (today's behaviour) |
| **UNKNOWN** | **NO AGENT EVER RAN** | **PARK** — do not burn an attempt |
| UNKNOWN | agent ran, did not fetch | agent failure, re-sling |

Only the third row changes. It is the row the incident sat in, and it is the one
the bead names: *"an unready controller parks the bead as UNKNOWN rather than
burning an attempt."*

Park via the existing mechanism — `beadstore.StateParked`, which `gonk-sweep`
already unparks on a `RetryAfter` (`cmd/gonk-gate/sweep.go`). **No new state, no
new loop.** The park reason must be distinct (e.g. `create-outcome-unknown`) so
it is greppable and cannot be confused with a budget park.

`abandonSession` must still run — a session that was created but never fetched
still leaks a pod otherwise, and the existing comment is emphatic that this is
the only place that can tear it down.

### 1.3 Do not let the park hide a real outage

A park that repeats forever is the wedge gonk-bvy taught us to fear. So:

- Count it: `gonk_dispatch_create_outcome_unknown_total{reason}`.
- Cap it: after **N** consecutive UNKNOWN parks for the same bead, stop parking
  and fail loudly as infra, so a permanently broken provider surfaces instead of
  parking quietly forever. N and the retry interval are config with defaults, not
  constants buried in the path.

---

## 2. Explicitly out of scope

- **The readiness probe.** Filed separately. Real, but ~2.3s at most and it does
  not explain a failure at 42s.
- **Fixing Gas City's provider.** Whether the k8s provider failed silently or was
  genuinely not ready is a Gas City question in code gonk does not own. gonk's
  job is to stop *believing* an unconfirmed create. If an upstream report is
  warranted, **the owner posts it** — not this session.

---

## 3. Verification (fresh agent, against this plan)

1. A fake `GC` whose `AwaitRequestOutcome` **times out**, plus a session that
   never fetches, results in a **parked** bead with reason
   `create-outcome-unknown` — and **no** consumed ladder attempt. Assert the
   attempt counter directly; do not infer it from the absence of a log line.
2. The same, with `AwaitRequestOutcome` returning a **transport error**, parks
   with a distinguishable reason.
3. A **confirmed** create that then fails to fetch still re-slings exactly as
   today — a regression guard on the three unchanged rows of the table.
4. `abandonSession` runs on the park path too. Assert the teardown call, because
   "will idle forever" is not a figure of speech (`broker_inject.go`).
5. The UNKNOWN log line is emitted at **Warn** and carries `request_id`.
6. After N consecutive UNKNOWN parks the bead fails as infra instead of parking
   again.
7. `grep` proves no remaining path converts an `AwaitRequestOutcome` error into
   a silent `nil`.
