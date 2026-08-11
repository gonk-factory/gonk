# Source beads: level-triggered work intake

Status: DESIGN, not yet implemented. Owner decisions marked **OD-n**.
Date: 2026-08-10.

## The problem, with today's evidence

gonk is **edge-triggered**. A webhook arrives, intake gates it, and if the gate
says no the event is gone. Nothing durable records that it ever happened.

Every `Store.Put` in the tree runs inside `cmd/gonk-gate/dispatch.go` or
`broker_inject.go` — i.e. **after** intake's Gate 1 and the meter's Gate 2 have
both passed. `pkg/intake.Dispatch` (dispatch.go:275) has no store field at all:
it computes a `BeadAnchor`, passes it as an order variable, and forgets it.

Observed live 2026-08-10: issue !24's predecessor !23 was accepted `200` by
intake and dropped as `state_key-missing` (a meter bug, since fixed). It left
**zero beads and zero city events**. The work item does not exist anywhere and
nothing will ever re-drive it; a human had to file a second issue to get another
trigger.

That hole is wider than `gonk-icj` ("park/re-drive webhooks received while
unsynced"). It is every pre-dispatch outcome:

- project `key-missing`, `unsynced`, `disabled`, `invalid`
- budget `defer` or `deny`
- the staleness refusal (`StalenessWindow`)
- any crash, restart or Helm upgrade between receipt and the order firing

and rollouts are frequent, because a push to gonk main re-renders the chart.

## The principle

**Record the fact at receipt. Decide what to do about it later, repeatedly.**

A source bead is a durable record that *an external object exists and something
happened to it*. It is NOT a work item. The decision to act is derived, and
re-derived, from the project's current `.gonk.yml` and the object's current
state.

This is the same shape as `pkg/intake/reconcile.go`, which already reconciles
*projects* level-triggered with a startup ladder and a periodic pass. This
applies that pattern to *work items*.

What it unlocks, beyond durability: a maintainer flipping
`actions.pipelines: true` means the next patrol picks up the backlog of pipeline
events gonk already saw and ignored. Today that config change only affects
events that arrive afterwards.

## Anchor vocabulary

Today: `gonk:{project_id}:issue:{iid}` (pkg/intake/dispatch.go:199). Only
issues; no instance identity, so project 75 on one forge collides with project
75 on another.

Proposed:

```
gonk:{source_type}:{source_id}:{scope}:{object_type}:{object_id}

gonk:gitlab:orac:75:issue:24
gonk:gitlab:orac:75:pipeline:1935
gonk:gitlab:orac:75:job:15574
gonk:github:com:anthropics/claude-code:issue:1234
gonk:prometheus:orac:platform:alert:{fingerprint}
```

- `source_type` — what KIND of source this is: `gitlab`, `github`, `prometheus`,
  … Not "forge kind": the door is deliberately open to sources that are not
  forges at all.
- `source_id` — which instance of that source (`orac`, `com`). Without it,
  project 75 on one instance collides with project 75 on another, and the bead
  store is shared across all of them.
- `scope` — **source-defined**, not assumed to be a numeric project id. GitLab
  uses the project id; GitHub uses `owner/repo`; Prometheus has no repo at all
  and uses whatever grouping the operator configures. The anchor parser must
  treat everything after `source_id` as a source-owned path, or the first
  non-forge source breaks the format.
- `object_type` — `issue` | `pipeline` | `job` | `mr` | `alert` | …, owned by
  the source adapter.
- One bead per external **object**, never per event: a bead accumulates events,
  it does not multiply.

## Multiple sources

The patrol's two source-specific questions are "what does this event identify?"
and "is that thing still live?". Everything else — dedupe, eligibility,
dispatch caps, retention — is source-agnostic. So the seam is a small adapter:

```
Ingest(event)  -> anchor, external state, observed-at
Resolve(anchor) -> external state (open | resolved | gone)
```

- **GitLab** (today): issue closed, pipeline green, MR merged, job superseded.
- **GitHub**: same shapes, different client. Additive.
- **Prometheus / Alertmanager**: the natural identity is the alert
  **fingerprint** (or `alertname` + label-set hash), and `Resolve` maps directly
  onto Alertmanager's own `resolved` status — arguably the cleanest fit of the
  three, because the source already tells you when the condition ended.

**OD-3: eligibility config for non-repo sources.** `.gonk.yml` lives in a
repository, which a Prometheus alert does not have. Either non-repo sources are
configured in the operator config, or an alert is mapped to an owning project
whose `.gonk.yml` then governs it. This does not block the GitLab work, but the
anchor format and the adapter seam have to be chosen now so it stays additive.

**Migration hazard (OD-1).** The anchor is the dedupe key AND the bead title AND
a label. Changing its shape orphans every existing bead: in-flight work would be
re-created under a new anchor and could be triaged twice. Either read-compat
both forms during a window, or run a one-time rewrite of titles and labels.
Also confirm bd's label charset/length tolerates the longer form — the current
anchor already contains colons, so colons are fine, but length is unverified.

## The source bead

Unrouted, always. `beadstore.Put` creates beads with
`bd create --title <anchor> --labels <anchor-label>` — no assignee, no
`gc.routed_to`. **This must stay true.** Gas City's default `scale_check` counts
ready+unassigned beads by `gc.routed_to` to size pools; the 2026-08-03 cluster
wedge was stale routed beads generating permanent pool demand until nothing
could schedule (see `gonk-p2e`, still open). A bead per received event, if
routed, reproduces that at much larger scale.

Records, in addition to today's `Record` fields:

- external identity: instance type/id, project, object type, object id
- `FirstSeenAt`, `LastEventAt`, and the last event's action
- `ExternalState` as last observed (`open`/`closed`, pipeline status)
- `Disposition` and its reason — why the patrol last decided not to act

States (extending `beadstore.State`):

| state | meaning |
|---|---|
| `observed` | recorded at receipt; no decision taken yet |
| `ineligible` | config says this category is off. Retained, re-evaluated |
| `deferred` | eligible, but gated now (budget, staleness, key-missing) |
| `running` | dispatched (today's `StateRunning`) |
| `parked` | today's meaning, unchanged |
| `done` | gonk produced its artifact |
| `resolved` | the external object went away or was fixed elsewhere |

## Receipt path

Intake, before any gate: find-or-create on the anchor, stamp `LastEventAt` and
the observed external state, then run the existing gate. The gate's outcome is
written onto the bead as a `Disposition` rather than being the only thing that
decides whether a record exists.

`beadstore.Put` already upserts on the anchor, so a replayed webhook converges
instead of duplicating.

**OD-2: who writes it.** Intake holds no bead store today, and the city is
grant-gated for mutations. Three options:

1. **Intake writes directly** (preferred). Anything order-mediated is lost in
   exactly the outage windows this exists to survive. Cost: intake needs store
   access, widening its credential surface — deliberately narrow today.
2. Intake fires a `gonk-observe` exec order; the controller records it. Cheap
   credential-wise, but depends on the supervisor ticking — and the supervisor
   demonstrably skips ticks under FS pressure (`supervisor.fs_pressure.skipped_tick`
   observed 2026-08-10).
3. Fold into dispatch as a pre-gate step. Simplest, but does not survive the
   webhook being dropped before dispatch — which is the whole point.

## The patrol

A reconciler over source beads. Startup pass plus periodic, mirroring
`Reconciler.Loop`'s startup ladder (`runStartupLadder`, gonk-fan) so a restart
converges promptly instead of one interval later.

Per open source bead:

1. **Resolve externally?** Ask the forge for the object's current state. Issue
   closed, pipeline green, MR merged, job superseded by a newer run on the same
   ref → `resolved`, close. This is what stops unbounded growth and stops gonk
   acting on stale work.
2. **Eligible?** Re-read the project's current config. Category off →
   `ineligible` (retained, not deleted — this is the backlog that a config
   change unlocks).
3. **Already satisfied?** Artifact present → `done`.
4. **Otherwise** hand to the existing dispatch path.

### Hazards the patrol must handle

- **Backlog stampede (the big one).** Flipping `actions.pipelines: true` on a
  busy project makes hundreds of beads eligible at once. The meter's budget gate
  is a real backstop, but it is a *spend* gate, not a *concurrency* gate. The
  patrol needs an explicit per-pass dispatch cap and an operator-visible count
  of what a config change is about to unlock. Silent bulk dispatch is how the
  cluster wedged before.
- **Forge API budget.** Per-bead GETs do not scale. Batch: list issues
  `updated_after`, list pipelines by ref, and reconcile against the bead set —
  the same shape the project reconciler already uses.
- **Retention.** `resolved`/`done` beads need compaction, or the store grows
  without bound. `bd compact` exists.
- **Ordering.** A bead may be `resolved` externally while a session is running.
  Resolution must not yank a live session; it should let teardown finish.

## What this does NOT change

- The prompt-delivery bug (`gonk-e9m`) — triage still cannot produce an artifact
  until that lands. Source beads make the failure *durable and re-drivable*,
  not fixed.
- The meter's reservation idempotency, which is already correct (FIX-A,
  `internal/meter/service/service.go:582`).
- What triage does once dispatched.

## Verification

The gate for this work, stated before building:

1. File an issue while the project is deliberately blocked (e.g. `key-missing`).
   A source bead must exist, in `deferred`, with the reason recorded.
2. Clear the block. The next patrol must dispatch it **without a second
   webhook**.
3. Restart the controller mid-flight. No duplicate triage, no lost bead.
4. Turn a category off, deliver events, turn it back on. The patrol must process
   the backlog — and must report how many items it is about to process.
5. Close the issue externally. The patrol must close the bead and must not
   dispatch it.
