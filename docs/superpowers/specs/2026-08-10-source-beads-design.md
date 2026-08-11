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

**OD-3 RESOLVED (owner, 2026-08-10): config resolution is per source type.**
There is no universal answer and the design should stop looking for one.

- **Repo-backed sources** (GitLab today, GitHub — which has an equivalent
  per-project config file) keep `.gonk.yml`, layered under the instance
  backstop exactly as now.
- **Sources with no repo** (Alertmanager, Jira) are configured by an
  **instance-scoped ConfigMap**, the pattern already used for GitLab backstops.

So config resolution joins the adapter seam:

```
Ingest(event)   -> anchor, external state, observed-at
Resolve(anchor) -> open | resolved | gone
Config(anchor)  -> effective eligibility config
```

The precedent is already in-tree and does not need inventing.
`gonk-operator-config` is:

```yaml
groups: {}          # a scoping layer, present and currently unused
instance:
  budget: {monthly_cost_usd: 25, monthly_tokens: 500M, per_task_tokens: 2M}
  enabled: true
  ladder: [qwen3-14b, qwen3-6-35b, claude-sonnet]
```

i.e. an **instance -> groups -> project** tightening chain. Non-repo sources
extend the same idea with a per-source-type section.

**That section must be scoped, not one global block.** Otherwise Alertmanager is
all-or-nothing across every service, and there is no way to enable it for one and
not another. `groups: {}` is the existing shape for that scoping.

**Consequence worth naming: the consent model differs.** On GitLab a maintainer
opts in by committing `.gonk.yml` — consent is expressed in the repo, by someone
with write access to it, and the instance can only ever set a ceiling the project
tightens. A non-repo source has no such artifact: the operator turns it on
centrally and is the *only* authority, with no tightening layer beneath. That is
a legitimate model for infrastructure alerts, but it means "enabled" in the
instance ConfigMap is the whole decision to spend, so it should read as a
deliberate operator action and not a default.

### Migration: rewrite-on-read, not a batch job

**OD-1 RESOLVED (owner, 2026-08-10):** ship a version that reads the old format
and rewrites it to the new one whenever it sees it. We are the only gonk
instance, so a one-time rewrite would be acceptable, but lazy migration needs no
maintenance window and no separate job.

**Where the fallback goes is load-bearing, and the obvious placement is wrong.**

The intuition is "one startup pass migrates the instance during upgrade". That
is *nearly* true and its gap is the dangerous part:

- `BdCLI.List(state)` (bd.go:219) enumerates by the **state label**
  `gonk::<state>`, and `runSweep` only ever calls it with `StateRunning` and
  `StateParked`. A startup pass therefore migrates **in-flight work only**.
- `done` beads are never enumerated by anything.

And `done` beads are exactly the ones that matter here. `findBeadID` (bd.go:89)
resolves an anchor by the **label** `gonk-anchor:<anchor>`, so a new-format
lookup cannot match an old-format label. A new event on an issue whose old bead
is `done` would miss, create a fresh bead, and **re-run triage** — the exact
failure this epic exists to prevent.

So the fallback belongs at the **lookup seam**, not (only) in enumeration:

```
findBeadID(anchor):
    try new-format label
    on miss: try old-format label
    on hit:  rewrite title + anchor label to the new form, then return
```

With that, every path that touches a bead migrates it:

- startup/patrol enumeration migrates in-flight beads eagerly;
- a terminal bead migrates the instant it becomes relevant again, which is
  precisely when correctness depends on it;
- nothing needs a migration job, and a half-migrated store is always correct.

Properties to preserve: the rewrite is idempotent (label add/remove on one bead
id, so two processes racing converge); the fallback costs one extra `bd list`
**only on a miss**, so steady state pays it on genuinely new objects only; and
it is removable in a later release behind its own bead.

State labels are unaffected — the new states (`observed`, `ineligible`,
`deferred`, `resolved`) are additive `gonk::<state>` labels, so no state
migration is needed.

Still to confirm: bd's label length tolerance for the longer anchor. Colons are
already proven fine (the current anchor contains them); length is not.

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

## The link chain

The point is to get from an event to the in-flight work without guessing:

```
external event  ->  source bead  ->  running workflow
(issue #24 updated) (gonk:gitlab:orac:75:issue:24) (session gonk.triage.p75.i24.a1)
```

Half of this exists already. `Record.SessionID` holds the session alias, and the
alias encodes agent, project, issue and attempt — so **source bead -> workflow
is done**, and it is what `runSweep` already follows to judge and close a
session.

What is missing is the **left-hand link**. `pkg/ghook/dedupe.go:84` computes an
event identity (`issue:{iid}:{action}`) but it is in-memory, per-process, and
never persisted, so after a restart there is no way to ask "which bead did that
delivery produce?". The source bead should carry the originating event's
identity (delivery id where the source provides one, else the computed identity)
plus `FirstSeenAt`/`LastEventAt`, so the chain is traversable in both directions
and survives a restart.

Note the asymmetry, deliberately: **many events, one source bead, many workflow
runs.** A bead accumulates events on its left and attempts on its right; it
never multiplies.

## Receipt path

Intake, before any gate: find-or-create on the anchor, stamp `LastEventAt` and
the observed external state, then run the existing gate. The gate's outcome is
written onto the bead as a `Disposition` rather than being the only thing that
decides whether a record exists.

`beadstore.Put` already upserts on the anchor, so a replayed webhook converges
instead of duplicating.

**OD-2 RESOLVED (owner, 2026-08-10): intake creates the bead.** Create it as
soon as we see the work; any other process may amend it afterwards, which
`beadstore.Put`'s upsert-on-anchor already supports.

An earlier draft of this spec objected that writing from intake would widen a
deliberately narrow credential surface. **That was wrong**, and the deployment
says so plainly:

```
GONK_GC_WRITE_KEY_FILE, GONK_GC_WRITE_KEY_ID=gonk
GONK_SUPERVISOR_URL=http://gonk-controller.gonk.svc:9443
```

Intake already holds an ed25519 grant-signing key and already signs mutations
against the city — that is how it fires grant-gated orders today
(cmd/gonk-intake/main.go:270-275). There is no new credential to grant.

The real constraint is mechanical and much narrower: intake is a **distroless
image carrying one binary**, so it cannot use `beadstore.BdCLI`, which shells out
to `bd`. It needs a `beadstore.Store` implementation that writes over the city
API with the grant it already holds.

That is the better design anyway: intake stays distroless with no subprocess, the
write travels the same signed path as everything else, and no new secret is
mounted anywhere.

The rejected alternatives, and why:

- **A `gonk-observe` exec order.** Depends on the supervisor ticking, and it
  demonstrably skips ticks under FS pressure
  (`supervisor.fs_pressure.skipped_tick`, observed live 2026-08-10). It would be
  lost in exactly the windows this exists to survive.
- **Folding into dispatch.** Does not survive the webhook being dropped before
  dispatch, which is the whole point.

### Failure mode: make the source retry

Once the source bead is the first thing that happens, a failure to write it
should **fail the webhook** (non-2xx) rather than today's 200-and-drop. GitLab
retries deliveries, so the forge becomes the durable queue and we do not build
one.

Bound it: GitLab disables a hook after sustained failures (the hook's
`alert_status` field, `executable` when healthy), so this must not flap. But a
retried delivery is strictly better than a silently discarded one, which is the
behaviour that lost issue !23.

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
