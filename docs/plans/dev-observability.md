# Plan: dev observability (gonk-pop3)

> "at least during dev, we REALLY need to have enough logging/tracing to
> understand why these things are happening." -- the owner, 2026-09-12

Diagnosing three live failures on 2026-09-12 required, respectively: exec'ing
into an agent pod before the reaper got it (the window was missed once and the
evidence is gone forever), port-forwarding gonk-meter and authenticating to its
cost API, and querying GitLab's own webhook delivery log. None of the three was
answerable from `kubectl logs`. This plan closes the four gaps that forced that.

## The motivating case, and the acceptance test

On 2026-09-12 02:40:32 the owner replied `@gonk you are absolutely right choomy,
that sort function SHOULD honor the desc parameter. you have permission to fix
it.` on project 75 issue !71. Nothing happened.

- GitLab delivered it: `projects/75/hooks/4/events` shows
  `2026-09-12T02:40:32 note_hooks -> HTTP 200`, and the hook has `note_events: True`.
- Intake logged **nothing**. Its last line for project 75 was a checkout grant
  eight minutes earlier.
- `pkg/intake/dispatch.go` handles `ghook.KindNote` and `Mentions()` matches that
  body (the line begins `@gonk ` and `mentionRe` captures exactly `gonk`).

So the drop is invisible for TWO reasons, not one:

1. an event that falls out of `Decide()` increments a Prometheus counter and
   returns, leaving no log line at all; and
2. **the dispatch SUCCESS path is equally silent** -- `d.Obs.Dispatched(trigger)`
   is a counter, nothing more. A reader of `kubectl logs` cannot distinguish
   "intake dropped it" from "intake fired the order and the next hop ate it".

**Acceptance: the log output must say which.** Not a description of what it would
say -- the observed records.

## The call on OpenTelemetry: NO. Structured logging plus the bead anchor.

The independent review grades observability "Not done -- zero otel code in
`cmd/ pkg/ internal/`". I am not adding otel, and this is the argument.

gonk's handoffs are not in-process RPC. They are: GitLab -> intake (HTTP),
intake -> a bounded in-process queue, intake -> Gas City order-run (HTTP),
Gas City -> `gonk-gate dispatch` (a **forked exec order**), and a 30s cooldown
exec order (`gonk-sweep`) that picks the work back up from a database. A trace
would have to survive a process fork through Gas City's order vars -- and
upstream Gas City **provably drops caller vars** (gonk-6gs, upstream #4668/#4891;
it is the defect that killed the whole formula layer). Otel would therefore buy
a distributed trace that breaks at precisely the hop that lost !71, at the cost
of a collector, a storage backend, a Deployment, a NetworkPolicy and an SDK in
four binaries.

What gonk already has instead is better suited and free: `BeadAnchor`
(`gonk:<project>:issue:<iid>`) is a **deterministic** function of the work item.
It is already computed identically by intake, gonk-gate and gonk-meter, already
appears in most of their log records, and is stable across re-slings and
restarts. It is gonk's trace id. The work is to make sure every record that
matters carries it, and that no path is silent.

The one genuinely missing correlator is on the inbound edge, where a bead anchor
does not exist yet for a dropped event. That gets a `delivery` id -- GitLab's
`X-Gitlab-Event-UUID`, or `ghook.DedupeKey`'s derived fallback -- threaded from
the receiver onto the `Event` and logged by both stages. One field, no
dependencies.

## Scope control: what is always on, and why

The owner said "at least during dev". Default-off logging that nobody turns on
would not have caught a single one of today's failures, so the split is:

**ALWAYS ON** (cheap, bounded, one line per real-world occurrence):
- the intake webhook delivery record (one per delivery -- GitLab delivers a
  handful per minute at gonk's scale);
- the intake dispatch decision record (one per queued event);
- the transcript *metadata* record in the sweep (one per classified session);
- the spend record in the sweep (one per classified session);
- the sweep health record and the dead-sweep escalation.

**DEV ONLY, explicit and documented** (unbounded in size, or carries untrusted
user text):
- the session transcript **archive** (`GONK_TRANSCRIPT_DIR`, empty = off).

**NEVER**: note bodies, issue descriptions, tokens, PATs, bearer files, model
keys. Issue and comment text is untrusted input; `TestSecretsAreNeverEnvironment-
Variables` and the repo's secret rules apply. The always-on records carry ids,
lengths, hashes and decisions -- never content.

---

## Item 1 -- intake accounts for every inbound event

**Gap.** `ghook.Handler` reports outcomes only to a Prometheus counter.
`Dispatch.Handle` has eleven return paths and logs on four of them; the other
seven -- including the *success* path -- are silent.

**Build.**

1.1 `ghook.Event` gains `DeliveryID`, set by the receiver from
    `X-Gitlab-Event-UUID` (falling back to `DedupeKey`'s derived hash). It is a
    correlator, never a decision input.

1.2 `ghook.Handler` gains a `Log *slog.Logger`. `finish()` -- the single
    function every return path already goes through -- emits exactly one
    record per delivery: delivery id, event label (already collapsed to a
    bounded set for the attacker-controlled header), outcome, HTTP status, and
    the project/issue/note ids **when the payload parsed**. Pre-authentication
    outcomes log at WARN with only the bounded fields, so an unauthenticated
    flood cannot write attacker text into the log.

1.3 `Dispatch.Handle` emits exactly one `intake decision` record, built at the
    top and emitted from a **single `defer`**. This is mechanical totality: a
    new `return` cannot be added without the record being written. Fields:
    `delivery`, `kind`, `project`, `project_id`, `issue_iid`, `note_id`,
    `session_key`, `bead`, `decision`, `reason`, `trigger`.
    Decision vocabulary: `dispatched | deferred | denied | ignored | error`.
    **`ignored` and `error` always carry a reason** -- the type makes an empty
    reason impossible to emit silently (it renders as
    `reason=BUG_no_reason_recorded`, which is a grep-able defect report).

1.4 Tests:
    - every `Decide` drop reason and every `Handle` branch yields exactly one
      record, asserted by a table that walks the reason list;
    - a record is emitted on the **success** path too;
    - no record ever contains the note body or issue description;
    - the acceptance test: replay the real !71 Note Hook payload through the
      real `ghook.Handler` + queue + `Dispatch.Handle` and assert the records
      name the trigger and the outcome.

## Item 2 -- session transcripts survive the session

**Gap.** `gonk-sweep` reads a finished session's transcript
(`broker_apply.go`), classifies from it, and the pod is then reaped. The
transcript is gone. On 2026-09-12 a wrong triage answer could not be
investigated at all.

**Build.**

2.1 ALWAYS ON: a `transcript read` record at the point of the read -- bead
    anchor, session alias, byte length, turn count, `sha256` prefix, whether the
    `GONK_BATCH_START/END` fence was found, and whether the read was paginated
    or empty. No content. This is what makes an archived transcript findable and
    is, on its own, enough to tell "the agent said nothing" from "we could not
    read what it said" -- the exact ambiguity that cost issue !42 an escalation.

2.2 DEV ONLY: `GONK_TRANSCRIPT_DIR` (empty = disabled, which is the default).
    When set, the sweep writes the transcript to
    `<dir>/<bead-anchor-slug>-<session>.json` with 0600 permissions, an atomic
    rename, a per-file size cap, and pruning to a bounded file count (oldest
    first). A write failure is logged and NEVER fails the sweep.

2.3 Retention decision, documented: the archive is an **emptyDir on the
    controller pod**. It therefore outlives the agent pod -- which is the actual
    loss -- but not a controller restart. That is deliberate: a PVC for
    transcripts is durable storage of untrusted model and user text with no
    retention policy and no access control, which is a bigger commitment than
    "let me see what the agent said this afternoon". Chart value
    `dev.transcripts.enabled` (default false), mounting an emptyDir with a
    `sizeLimit`.

## Item 3 -- the model's behaviour is legible without a port-forward

**Gap.** Learning that one run made 6 model calls and another 9 required
`GET /v1/cost/bead/<anchor>` with a bearer token, through a port-forward.

**Build.** The sweep already talks to the meter and already holds a bearer
token. After classification it emits one `session spend` record: calls, prompt /
completion / total tokens, cost (and synthetic cost), the per-rung split, and
`complete`. Best effort -- a failed read logs a WARN and changes no verdict.

Deliberately NOT built: a log line per model turn. gonk does not own the model
loop (opencode runs inside the agent pod); a per-turn line would have to come
from the pod that gets reaped, which is the problem this plan exists to solve.
The turn count from 2.1 plus the call count here answers the same question from
a process that survives.

## Item 4 -- a dead subsystem is loud

**Gap (gonk-p7qh).** `gonk-sweep` could not reach its own database and logged
one ERROR every 30 seconds **for days** while the entire outcome path was dead:
sessions completed, nothing was ever posted, beads were stranded with an empty
`gonk::` state label that no query matches. Its only signal was a log line
nobody read.

**What "loud" means here, and why.** Three candidates:

- *A metric.* Rejected: `gonk-sweep` is a short-lived exec order, forked every
  30s. There is no process for Prometheus to scrape, and a pushgateway is new
  infrastructure to carry one number.
- *Refusal to report success.* Necessary but insufficient -- `runSweep` already
  returns 1, and that is exactly the signal that went unread for days.
- *Readiness.* This is the answer. A `kubectl get pods` showing `0/1 READY` is
  the one signal in this stack that is visible without knowing to look for it,
  and it is already wired to something that MATTERS: the controller Service.
  An unready controller drops out of the Service's endpoints, so intake's
  `FireOrder` fails, `DispatchDropped("fire_error")` fires, and the work is
  simply not dispatched -- instead of being dispatched into a session whose
  outcome can never be written, which is how gonk-p7qh stranded beads
  **permanently**. Failing readiness converts a silent partial outage into a
  visible, self-healing one: nothing kills the pod, the sweep keeps retrying,
  and readiness returns on its own the moment the store is reachable.

**Build.**

4.1 `runSweep` writes a health record to `GONK_SWEEP_HEALTH_FILE` (default
    `/city/gonk-sweep-health.json`) on **every** pass: timestamp, ok/failed,
    the failing stage, the error, consecutive failure count, and the time of
    the last good pass. Atomic rename; a write failure never fails the sweep.

4.2 The repeated-failure log escalates to ONE unmistakable cumulative line
    rather than an identical line every 30s:
    `SWEEP IS DEAD: the outcome path has been down for <duration> (<n> consecutive passes)`.
    A reader greps one line and learns the outage duration.

4.3 `gonk-gate sweep-health` -- a new subcommand that reads the file and exits
    non-zero when the last pass failed or the file is staler than a threshold.
    A missing file is OK (boot), so this cannot deadlock a fresh pod.
    `--supervisor-addr` additionally dials the supervisor port, so one exec
    probe covers both checks a `tcpSocket` probe used to.

4.4 Chart: the controller `readinessProbe` becomes that exec probe. Requires a
    `Chart.yaml` version bump (0.1.8 -> 0.1.9), `make chart-seal` and
    `make chart-goldens` (gonk-sjb: a chart edit without a version bump is one
    Flux never deploys).

## Out of scope, deliberately

- **Fixing gonk-ecn.** The mention trigger is a separate bead. This plan makes
  the drop diagnosable; it does not make the mention work.
- **otel / distributed tracing.** Argued above.
- **A PVC for transcripts.** Argued in 2.3.
- **Per-turn model logging.** Argued in item 3.
- **Reclaiming the `gonk::`-labelled stranded beads** from gonk-p7qh's blast
  radius. That is a correctness fix, not an observability one.

## Progress

- [x] Item 1 -- intake decision record (pkg/ghook/receiver.go, pkg/intake/decision_record.go, cmd/gonk-gate/dispatch.go)
- [ ] Item 2 -- transcript metadata (always) + dev archive
- [ ] Item 3 -- session spend record
- [ ] Item 4 -- sweep health, escalation, `sweep-health`, readiness probe
- [ ] `make gate` green
