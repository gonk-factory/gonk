# Handoff — next session

_Last updated: 2026-08-19. Branch: `main` (we develop on main per owner's call).
Everything below is committed and pushed._

**Read the top section and stop.** Sections below it are dated history, kept
because their measurements and dead ends are still worth not repeating. Any
"START HERE" in a dated section is superseded by the one in the current section.

---

## 2026-08-19: three bugs that all looked like success

Four things shipped and were verified live. The thread joining them is worth
stating once, because it is now the thing to expect from this codebase: **every
one of these reported success while doing nothing.** A grant that logged twice
and delivered nothing; a green pipeline that built no images; a sweep that
retried forever rather than concluding. Assume a discarded error before a wrong
value.

### Position

| | |
|---|---|
| `main` | `94b24ee` |
| deployed images | `v0.1.0-030a6c58ee2e` (pipeline 2195) |
| ns `gonk` | four service pods Running |
| closed this session | `gonk-u6p`, `gonk-j9z`, `gonk-6n8`, `gonk-mzm`, `gonk-1t4`, `gonk-zp3`, `gonk-2do` |
| filed this session | `gonk-au1` (P1), `gonk-0de`, `gonk-ak0`, `gonk-ckb`, `gonk-6n8`, `gonk-03f` (+5 children) |

### START HERE

**Still `gonk-ob5` (P1) — opencode fails OPEN to a built-in cloud provider.**
Unchanged from the section below, and now the last untouched Phase 0 blocker of
real severity: it defeats both core invariants at once (local-only inference and
hard budget enforcement) and does so silently. The full three-layer fix is
already written on the bead — an `enabled_providers: ["gonk"]` allowlist (in
opencode's schema but not its `--help`), plus an entrypoint assertion that
opencode actually resolved a gonk-only provider list, because config cannot
guard its own absence.

**Then `gonk-712`** — see "Where prompt delivery actually stands" below. Prompts
now demonstrably arrive; no triage comment has ever been produced. That gap is
the milestone and it is no longer blocked on delivery.

Phase 0 of the local-parity roadmap now stands at: `mzm` ✓, `j9z` ✓, `e9m`
functionally resolved (security half open), `ob5` / `bgx` / `msz` / `7oz` open.

### `gonk-j9z` — the rig checkout had NEVER worked. Fixed and verified.

`tar --no-absolute-names` **is not a GNU tar flag.** GNU tar spells the opt-out
`-P/--absolute-names` and strips leading `/` by default, so the protection it was
reaching for was already on and the invented flag simply made tar exit 64 — every
session, since the rig slice landed a week earlier.

Invisible three ways at once, which is why it survived: the fetch is non-fatal by
design so it logged a WARNING and continued; the entrypoint runs under
`tmux new-session -d`, so its stdout never reaches the container log; and **both
grant sites logged success throughout**. `rig: checkout granted` was true and
meant nothing.

Verified on issue !27 (session `s-go-wuvq`): `/workspace` now carries `.agent/`,
`.gonk.yml`, `README.md`, `RIG_PROOF.md`. The two assumptions the bead had
flagged as only-checkable-live — the archive's directory prefix and `GC_ALIAS`'s
runtime value — were both fine.

Guard: `test/entrypoint/`, deliberately **no build tag**. It parses the real tar
invocation out of `entrypoint.sh` and runs those flags against a real
GitLab-shaped archive.

### `gonk-u6p` — beads can reach a terminal state again. Fixed and verified.

`sweepRunning` computed the reservation deadline and then returned on any
session-read error *before* the switch that consumes it, so `gate.Classify` was
unreachable for a session that could not be read. An alias resolving to two
sessions 409s forever, so `gonk:75:issue:24` sat in `StateRunning` for **eight
days**. The adjacent branch, for the same class of "cannot judge yet", already
guarded on `!expired` — which is what makes it an oversight rather than a design
choice. Live proof: the bead classified `infra-failed`, hit
`infra-retries-exhausted`, and stopped.

**It was never the `xkm` shape.** No pod leak, both sessions already ended, and
reservations self-release via `ExpireReservations`. The cost was one
permanently-unfinishable bead plus log noise.

### `gonk-6n8` — one bead+attempt can no longer mint two sessions.

`runDispatch` builds its record **fresh from the webhook args**, so `SessionID`
is always empty and nothing ever checked whether a session already existed. Two
dispatches 4m29s apart both created `gonk.triage.p75.i24.a1`. Gas City's
create-time alias uniqueness considers only ACTIVE sessions while resolution
considers all of them, so once the first ended the duplicate succeeded — and the
alias was ambiguous forever. Teardown resolves by alias too, so an ambiguous
alias cannot be reliably closed either.

The guard keys on the **attempt**, not "a session exists": a re-sling is a new
attempt and must still get its own session. Not done, and deliberately:
unique-by-construction aliases. They would not have prevented this (both creates
shared a reservation) and `reap.go` matches gonk's sessions with an anchored
regex — changing the alias shape without it stops the reaper recognising gonk
sessions and turns a 409 into a leak.

### `gonk-0de` — the registry bloat was never image size

Comparing two consecutive agent builds layer by layer: **1 of 10 layers shared**.
The apt layer, opencode, glab and bd all take a fresh digest every build despite
being pinned and byte-identical. Each build writes ~182 MB of new blobs to
deliver a 3.6 MB change — which reconciles exactly with `gonk-1t4`'s 26 GB across
203 tags. Pruning treats the symptom.

Shipped and measured: `glab` and `bd` removed from the agent image —
**210.6 MB → 138.3 MB** compressed, 10 layers → 8, the predicted 72.3 MB exactly.
Confirmed in a live pod: `glab ABSENT`, `bd ABSENT`, opencode still present, and
the checkout still lands. That is a hardening change as much as a size one — a
forge CLI in a deliberately credential-free pod is an invitation to
re-credential it.

**The bead stays OPEN because the headline finding is not fixed.** Layers still
do not dedupe: each build now writes ~110 MB of fresh blobs (was ~182 MB) for a
3.6 MB change. Removing two binaries shrank what is needlessly re-uploaded; it
did not stop the re-uploading. The remaining fix is a toolchain base image
(debian + apt + opencode) rebuilt only when `versions.env` changes, with
`Dockerfile.agent` reduced to `FROM` that base plus one `COPY`. It is a
build-topology change — new image, new CI job, build-ordering dependency, and a
chicken-and-egg on first introduction — so it wants its own session against a
clean baseline. Getting it wrong breaks every agent build at once.

### Traps learned the hard way this session

- **Do not test immediately after a deploy, and the reconcile kick is not a
  shortcut past it.** `gonk-fan` is alive: intake restarts, the project reads
  `unsynced`, and webhooks are accepted with 200 and dropped. This cost two
  verification rounds. The second is the instructive one — kicking
  `POST /admin/reconcile` right after rollout ALSO fails, because the reconcile
  itself has to reach `gonk-meter`, which is still starting. **Wait until
  `meter registration failed` stops appearing in the intake log, THEN kick, then
  test.** Kicking once the meter is up works immediately (`{"kicked":true}`,
  202).
- **`test/images/` has never run in CI** (`//go:build images` vs a bare
  `go test ./...`). The one suite that tests the real container is excluded, and
  that is how the tar flag shipped. Filed as `gonk-ak0`. Any assertion that can
  be made without podman belongs in an untagged test instead.
- **A green gonk pipeline does not imply images exist.** When the `changes:`
  rules do not match, image jobs fall through to `when: manual` and are skipped;
  pipeline 2187 went green having built nothing.
- **The entrypoint's stdout is invisible.** It runs under `tmux new-session -d`,
  so `kubectl logs` on a session pod shows nothing. Use
  `tmux capture-pane -p -S -3000 -t main` inside the pod.

### Where prompt delivery actually stands

The tmux pane of a live session shows **the full triage prompt rendered in
opencode's composer**, fence instructions and all. So the bang-stripping
mitigation works and prompts ARE arriving — `gonk-e9m`'s remaining scope is the
SECURITY half (the keystroke channel is an injection boundary), not the
functional half. Note this contradicts the older sections below; trust this one.

What still has not happened is `gonk-712`'s milestone: a real triage COMMENT —
and as of this session **we know exactly why**. See `gonk-au1`.

### `gonk-au1` — the milestone is blocked by a five-minute timer

The deployed HelmRelease sets `reservation_ttl: 5m`, overriding the chart's 60m.
A qwen3-14b triage turn takes longer than that, so the sweep reaps every session
at the deadline before it can emit its fence. Measured on issue !27: created
`01:15:27`, four consecutive "still running; nothing to judge yet" ticks, reaped
at `01:19:53` with `reservation_expired_at=01:19:10`. The agent was still
working and was killed for it.

It hid this long because **every layer reports correctly**: the "still running"
lines are INFO, the reap is the `gonk-u1p.6` machinery working as designed, and
`infra-failed` correctly does not escalate. The bead then re-slings, is reaped
again, and exhausts `max_infra_retries: 2`.

The value is suspicious on its face — it sits directly beneath
`max_clock_skew: 5m` and `max_spend_staleness: 5m` with no comment, which reads
like it was matched to its neighbours rather than to a model turn. **Raise it,
but measure first**: one unreaped run gives the real distribution, and the trade
is that a longer TTL means a wedged session holds its budget reservation longer.
It is a deployment value in gitops, not a chart default.

---

## 2026-08-17: state of the world, and the two weeks the doc had lost

This doc had gone stale by two weeks — it ended at session 6 (2026-08-03) and
still told you to start on `gonk-4xr`, naming a deployed sha two deploys old.
Sessions 7–10 (2026-08-04 → 08-13) existed only in beads, commit messages and
`docs/research/`. That is the gap this section closes.

### Position

| | |
|---|---|
| `main` | `bed8792`, clean, in sync with origin. Last commit **2026-08-13** |
| deployed images | `v0.1.0-4b3c7e185b55` (2026-08-10) — **behind HEAD** |
| ns `gonk` | four service pods Running, controller 3 restarts |
| beads | 137 total / 80 open / **0 in progress** / 16 blocked / 57 closed |

### START HERE

**`gonk-ob5` (P1) — opencode fails OPEN to a built-in cloud provider.** The
local-parity roadmap's revision 3 promotes it above everything else in Phase 0,
and the argument is right: it defeats *both* core invariants at once (local-only
inference and hard budget enforcement) and does so silently, where the keystroke
bug is at least bounded by a sanitizer. It was found incidentally by the
prompt-by-reference T0 spike, and **the full three-layer fix is already written
on the bead** — an `enabled_providers: ["gonk"]` allowlist (in opencode's schema
but not its `--help`), plus an entrypoint assertion that opencode actually
resolved a gonk-only provider list, because config cannot guard its own absence.

Worth knowing before you start: the exposure is wider than "config missing". The
built-in providers were present **even with the overlay loaded**, and an
unguarded pod answers an explicit `-m opencode/big-pickle` override happily,
from inside the cluster, with no gonk credential and outside LiteLLM entirely.

Then: **`gonk-e9m`** (the plan is written, T1–T6, see below), and **a deploy**
(see "Deploy drift").

### What happened in sessions 7–10

**`gonk-zp3` was root-caused, and it was two faults wearing one signature**
(2026-08-10, and it needed `gonk-6a6`'s log sink to be fixed first — before that,
every structured line `gonk-gate` emitted was discarded, which is why four
investigations bounced off this).

- *Fault 1, fixed in `49d0d7d`*: the meter could not provision a project's
  LiteLLM virtual key, because `updateByAlias` re-sent `key_alias` on
  `/key/update` and LiteLLM rejects that as a duplicate of itself. Consequence:
  intake answered **every** webhook `200` and dropped it as state_key-missing, so
  dispatch never fired at all.
- *Fault 2, the original bug*: with dispatch working, the sweep said in one line
  what four investigations could not — `no GONK_BATCH_START/END fence in session
  transcript`. Fetching the live transcript repeatedly until the session ended
  returned the same single turn every time: opencode's TUI splash and nothing
  else. **The agent never receives its prompt.** So `zp3` is `gonk-e9m`, and
  extraction, validation, the shape gate and the peek window are all exonerated.

**The out-of-band channel exists, but one of its two shapes is broken.**
`gonk-2do`: `opencode run --attach <url>` emits a single `step_start`, exits 0,
and produces no text, no tool call and no error on any stream — proven side by
side against a working standalone `run` in one pod, seconds apart. Not a display
loss; `opencode export <sessionID>` against the server afterwards also returns
nothing, and neither LiteLLM nor ollama ever saw a chat completion, so the turn
is abandoned before any provider request. **The distinction that matters is
argument vs keystroke, not serve vs TUI** — and `e9m`'s design field proposed
`opencode serve`, which is now refuted. The correction is on the bead.

**The rig checkout slice landed** (`gonk-j9z`, `gonk-msz`, part of `gonk-7oz`):
per-session checkout granted at the decision point, agent→GitLab egress dropped
from the NetworkPolicy, triage given a checkout too, grant revoked at teardown.
The nice result is item 4 — a pooled generic pod **can** learn its own
per-session URL without any new channel, because the URL splits into a
per-install base (`GONK_RIG_BASE_URL`, chart-injected) and a per-session
`GC_ALIAS` that Gas City puts in every agent pod itself. So the checkout never
needed `e9m` after all.

**The registry filled to 100% and was fixed** (`gonk-mzm`, `gonk-1t4`). 295G of
unreferenced blobs against 684M of manifests — the signature of a registry that
has never been garbage collected. Fixed by a tag prune (921 tags) plus a
registry-side GC, and the CI cause was fixed too (`10dda82` stopped rebuilding
every image on every commit).

**2026-08-13 was a research day, and it reordered the plan.** A Warp.dev
comparison, a 363-URL capability inventory, a 54KB local-parity roadmap, a Fable
second-opinion that caught two ordering errors in it, and ADR-007. Outputs:
epic `gonk-6sp` (phases 0–6) and `gonk-4v8` (first real targets decided:
`nagus`, `rom`, `quark`). Read
[`the roadmap`](research/2026-08-13-gonk-local-parity-roadmap.md)
§11b and §11c before planning anything — they supersede the phase table above
them, and §11c.1 in particular invalidates a whole class of earlier reasoning.

### The correction that matters most: the context ceiling was the wrong model

`qwen3-14b` at **16,384** is what gonk's rung catalog names, and
`enforce_ladder_order=true` makes it where every bead starts. Meanwhile the
cluster already serves several **131,072**-context models, including a vLLM
NVFP4 35B MoE. The routing fix is `gonk-gyj`.

Two separate conclusions had been drawn from that one bad number and both were
wrong: `gonk-2do`'s first hypothesis (rung 1's context limit explains the empty
turn — killed by measuring a real triage prompt at 6,676 input tokens), and the
roadmap's revision 1 (context isolation is a *precondition*, so move the search
subagent into Phase 1 — withdrawn; at 128K it is a cost optimization and moves
back to Phase 4/5). What moves **up** instead is the annotated-diff coordinate
scheme and validator, the anti-lying validator, and the opencode edit-tool
investigation: the binding Phase-1 risk is no longer "will it fit" but "will the
model emit a valid, appliable edit."

### Deploy drift — the rig checkout is committed and NOT running

Images are pinned at `v0.1.0-4b3c7e185b55` (2026-08-10). Seven code commits sit
after it:

```
82f9f9b  fix(agent-image)  PAGER/GIT_PAGER/GIT_TERMINAL_PROMPT (gonk-sxg)
d09b10a  fix(rig)          check resp.Body.Close
26dfc3b  feat(rig)         triage gets a checkout too; revoke at teardown
100c70a  feat(rig)         wire the checkout end to end
d7de8fc  feat(rig)         per-session checkout, granted at the decision point
2a70711  fix(netpol)       drop agent->GitLab egress, add checkout path to intake
2739633  fix(scaffold)     stop telling the agent it has a checkout it does not have
```

The pods are only ~4 days old, which is misleading: the 2026-08-13 docs commits
re-released the chart (its version embeds the git sha) but the image tags never
moved. Everything above is unit- and chart-tested and **none of it has ever run
in a cluster** — that is the whole of what remains on `gonk-j9z`.

To roll forward: confirm CI published a complete four-image set at HEAD, then
bump the four tags in `steve/gitops`
`clusters/orac/apps/gonk/helmrelease-gonk.yaml` and commit **straight to main**
(that repo takes no MRs).

### Tracker corrections made 2026-08-17

The tracker was actively misleading, so this session fixed it rather than
working around it. Closed: `gonk-mzm` (P0 — the registry had been at 35% for
days while this blocked the critical path as a P0), `gonk-1t4` (reclaim
confirmed), `gonk-zp3` (diagnosed; `e9m` carries the fix), `gonk-2do`
(diagnosed; upstream draft split to the new `gonk-ckb`). Moved out of
`in_progress`, where nothing was actually being worked: `gonk-712` (claimed for
a month) and `gonk-pev`. `in_progress` is now empty and honest.

Two structural fixes worth naming:

- **`gonk-712`, the project's milestone bead, had no dependency edges at all.**
  `zp3`'s description asserted "Blocks gonk-712" in prose and no edge was ever
  created. Now correctly blocked by `gonk-e9m`, which is the honest chain:
  everything upstream of prompt delivery works, and the only reason the milestone
  has never been reached is that the agent never gets its prompt.
- **`gonk-j9z` items 5 and 6 had already landed** in `26dfc3b`, whose message
  says so explicitly — but no `bd` update followed, so the bead still listed them
  as open. Only item 7 (never proven live) remains, which makes `j9z` gated
  purely on the deploy.

### The critical path

Everything funnels into `gonk-3so` (Phase 1: implementation agent), now blocked
by six: `gonk-4v8`, `gonk-8g9`, `gonk-bgx`, `gonk-e9m`, `gonk-gyj`, `gonk-j9z`.
`gonk-4v8` (onboard the real target repos) is itself blocked by `gonk-066`,
`gonk-bgx` and `gonk-msz` — so **scaffold/onboarding is a hard blocker now**: it
gates the test repo, which gates everything.

**Designed but deliberately NOT started: the buildkit migration** (`gonk-03f`,
[design](superpowers/specs/2026-08-18-buildkit-migration-design.md)). It is a CI
wall-clock and cache-bloat fix, not a correctness fix — none of the recent build
failures were kaniko's — and it is off the critical path. Do not pick it up
ahead of Phase 0. If someone does start it, `gonk-sl1` (evaluate the Chainguard
kaniko fork) comes first and may cancel the whole epic for the cost of an image
swap.

Note what the roadmap says to *stop*: delete the formula / `[steps.check]`
machinery rather than maintain two orchestration idioms, and cut the
self-improvement loop (keep only the versioned marker as a join key). Carrying
both idioms is what produced the silently-dead mention trigger (`gonk-ecn`) and
the stale-work-bead pool demand (`gonk-p2e`) that caused the 2026-08-03 capacity
wedge.

### Traps, current

- **The failure shape of this project is the silent success.** A 202 read as a
  receipt; a create that succeeds asynchronously and fails later; a turn that
  exits 0 having produced nothing; `ENOSPC` surfacing as "check push
  permissions"; a checkout that degrades quietly rather than going red. When
  something does not work and nothing is red, assume a discarded error before
  assuming a wrong value.
- **`gonk:63:scaffold` has ONE infra retry left** of `max_infra_retries=2`. A
  second failure re-poisons it and needs another manual ledger delete
  (`gonk-9nx`, still open). Prefer a fresh bead over re-triggering that one.
- **Every commit to gonk `main` re-releases the chart** and recreates pods, even
  for docs-only commits, because the chart version embeds the git sha.
- **`make push` can silently clobber a CI-published multi-arch index**
  (`gonk-9ub`) — `GONK_TAG` comes from git HEAD at invocation, so a commit
  between build and push drifts the tag. Mitigated in `1e89c38`; know it exists.
- **Traefik 504s on the ~500MB agent/controller layers.** Push those through
  `kubectl port-forward svc/gitlab-registry 5000` to `localhost:5000`.
- **Upstream issues: draft them, do not post them.** Owner reviews, rewrites and
  posts. `gonk-ckb` now tracks the three pending drafts.
- All five worktrees under `.worktrees/` are clean and all five branches are
  already merged into main — safe to remove. `design-gonk-agent-harness` is the
  only genuinely unmerged branch (2 commits, 2026-07-19).
- `graphify-out/` is from 2026-07-30 and predates everything above.

---

## (historical) 2026-08-03 (session 6): the agent completed the loop; onboarding is one step short

> ⚠️ **Its "START HERE: `gonk-4xr`" is superseded.** That line of work became
> `gonk-pev`, which was root-caused on 2026-08-03 and is no longer where to
> start — the pool-session contention was a capacity leak (`gonk-xkm`, now
> closed), not an `agent.toml` problem. See the current section above.

_Everything below is committed and pushed. HEAD `31ee5e0`, deployed
`v0.1.0-a682e5d3e7ce`. Gates green: gofmt, vet, `go test ./...`, chart, pack._

### START HERE: `gonk-4xr` (P1)

The scaffold broker port works up to delivery. The broker created
`gonk.scaffold.p63.a1` on the real repo, and delivery failed **loudly and
precisely**:

```
ERROR prompt delivery failed; session will idle -- re-sling will retry
  alias=gonk.scaffold.p63.a1 bead=gonk:63:scaffold
  err=... never accepted its prompt after 40 attempts: session is not running yet
```

That is session 5's work paying off — before it, this exact situation reported
SUCCESS and stranded a silent idle session.

**The blocker**: the broker-created session never reaches `running` inside
deliverPrompt's ~90s budget. The suspicious signal is that the controller is
ALSO spawning scaffold **pool** sessions — `poolDesired: scaffold = 2` — even
though `min_active_sessions = 0`. Three scaffold pods contend, with k8s API
throttling visible while the controller polls for them.

**First thing to check**: did adding `start_command`/`[env]` to
`pack/agents/scaffold/agent.toml` make the agent look poolable to
`buildDesiredState` in a way it did not before? Triage has the identical block
and does NOT do this — that is the comparison to run. **Do not raise the
delivery budget first**; the budget is not the bug if the pod is never
scheduled.

**⚠️ ONE RETRY LEFT.** `gonk:63:scaffold` carries one `infra-failed` attempt.
`max_infra_retries` is 2, so one more failure re-poisons it and it needs another
manual ledger delete. Diagnose before re-triggering.

### What shipped

- **Prompt delivery actually works** (`gonk-u1p.7`, closed). Session 4's
  diagnosis was wrong: `SubmitSession` was fine and the bug was ours — dispatch
  treated the async 202 as a delivery receipt, and that route never 404s. It now
  correlates the terminal event on the city log. Three further silent failures
  fell out: the create is async too, a prompt submitted into a start_pending
  session is parked and never arrives, and the 120s order timeout was killing
  the delivery it waited for (now 300s).
- **Prompts were being EXECUTED, not asked.** opencode's composer treats `!` as
  its shell-mode trigger, and Gas City delivers messages as keystrokes. Every
  wedged session was the whole prompt running as `/bin/sh`. Mitigated by
  stripping bangs; `gonk-e9m` is the real fix.
- **Scaffold ported to the broker** (`gonk-bgx`). First trigger whose artifact
  is repository CONTENT: the agent proposes `file` effects and the broker
  commits them and opens the MR. `effects.ValidatePaths` is a security boundary
  — mutation-tested.
- **Protected-path denylist** — CI definitions, `.gonk.yml`, CODEOWNERS, `.git/`
  refused for EVERY agent regardless of shape. Redundant today on purpose.
- **`TestEveryModelAgentCanActuallyStart`** — scaffold and mention shipped with
  agent.toml files carrying no `start_command` at all, so no session could ever
  be created and every other test stayed green.

### Design work (no code)

- [`verified-change-pipeline`](superpowers/specs/2026-08-02-verified-change-pipeline-design.md)
  — intent → work → deterministic verify, with the split-diff red/green check.
  **Reviewed by Fable, which falsified the flagship claim**: red→green does NOT
  make test-weakening unreachable. The fix is a test-set monotonicity invariant.
  Read the CORRECTION section before building anything on it.
- [`gitlab-duo-workflows`](reference/gitlab-duo-workflows.md) — recovered from a
  2026-07-19 conversation via episodic memory. It informed the architecture and
  was invisible in the repo; most of it got re-derived from scratch. Contains
  the one piece never built: **classify the work before implementing, and route
  a non-code diagnosis to an ISSUE rather than a diff** (`gonk-2wq`). That is
  the sharpest answer to the danger in "fix broken pipeline".
- The invariant that kept reappearing from three directions, and is worth
  applying to any new effect kind: **models supply constraints and objections;
  only deterministic checks and humans grant permission.** Intent may narrow but
  not widen; protected paths subtract but never add; a reviewer may veto but
  never authorise.

### Open, ranked

| bead | what |
|---|---|
| `gonk-4xr` P1 | scaffold session never reaches running (START HERE) |
| `gonk-e9m` P1 | prompts delivered as keystrokes — injection boundary |
| `gonk-066` P1 | shape gate is syntactic; blocks any code-writing agent |
| `gonk-2wq` P1 | classify-before-implement; blocks `actions.pipelines` |
| `gonk-ob5` P1 | opencode fails OPEN to a cloud provider if config is unreachable |
| `gonk-9nx` P2 | no way to reset a bead poisoned by a since-fixed bug |
| `gonk-m6t` P2 | attribution no longer reaches the pod |
| `gonk-kp5` P2 | dispatch re-fetches issue data the webhook already delivered |

### Traps (in addition to session 5's)

- **`gonk-fan` bit FOUR times today.** Every intake restart races the meter,
  both projects read `unsynced`, and nothing recovers for 10 minutes without
  `POST /admin/reconcile` on the intake private port. After any deploy, kick it.
- **A dispatch that never creates a session still burns a ladder attempt.** The
  reservation is made before the session exists, so infra that fails before any
  model runs is charged against the retry budget. Feeds `gonk-9nx`.
- **Backgrounded `kubectl port-forward` does not survive between tool calls.**
  Start it and query it in the same invocation.
- `graphify-out/` is from 2026-07-30 and does not contain this session's work;
  `graphify . --update` refreshes it.

## 2026-08-01 (session 5): THE AGENT WORKS. A gonk-spawned opencode session completed the loop.

Issue 21 → dispatch → session `go-4m4` → the agent read the issue, wrote a real
triage analysis, and emitted a well-formed `GONK_BATCH_START`/`END` fence. That
is the first time any agent has finished. Verified live on ns `gonk`.

### Session 4's diagnosis was wrong, and it is worth knowing why

`gonk-u1p.7` said both prompt-delivery mechanisms were broken upstream and that
the only way forward was option (c) — carry the prompt in out-of-band. That was
inference from source reading. Three experiments against the live cluster
overturned it in about twenty minutes:

1. **tmux delivery works.** Running the carrier's exact two-step `send-keys` by
   hand typed into the live opencode TUI and the model answered (`PONG`, 5.3s).
   `Provider.Nudge` really does swallow the carrier's error — but the carrier
   was not failing.
2. **`SubmitSession` works.** A probe making the exact signed POST that
   `cmd/gonk-gate` makes delivered a prompt; the model answered in 498ms. The
   shipped fix was not merely "correct against the contract" — it was the
   unblock all along.
3. **The bug was ours.** `POST .../submit` resolves the session *after*
   answering 202, so a submit to a session that does not exist yet looks
   identical to one that lands. `deliverPrompt` retried only on 404 — a status
   that route never returns.

**The lesson worth carrying: read the source to form the hypothesis, then go and
run it.** `gcapitest` had encoded the *assumed* contract (a 404 window upstream
does not have), so the round-trip test confirmed the assumption instead of the
behaviour. A fake built from a spec you have not exercised will agree with you.

### The second bug: the prompt was being executed, not asked

Gas City delivers messages as tmux `send-keys -l` — **keystrokes into a TUI** —
and opencode's composer treats `!` as its shell-mode trigger. gonk's prompt
opened with `<!-- gonk:model:... -->` and said `issue !18`, so the entire prompt
ran as a shell command. opencode created an empty session and sat on a spinner
with no LLM request in its log. Reproduced minimally: `"...see issue !42 and
reply with GOLF"` → `$ ...see issue 42...` → `/bin/sh: 1: Hello: not found`.
Multi-line text, backticks and JSON braces were each tested and are fine — the
bang alone does it, anywhere in the string.

Mitigated by stripping bangs before submit and dropping the marker lines. **This
is a mitigation, not a fix** — see `gonk-e9m`. gonk hands attacker-influenced
issue text to a component that types it into a terminal, and a keystroke channel
cannot be made safe by escaping at the sender.

### Where to start

**The design and plan are written — start by reading them, not by re-deriving.**

- [`specs/2026-08-01-prompt-by-reference-design.md`](superpowers/specs/2026-08-01-prompt-by-reference-design.md)
  — approved 2026-08-01. Prompt-by-reference: dispatch stores the rendered
  prompt in gonk-meter keyed by alias, the entrypoint fetches it by `GC_ALIAS`
  at startup. No keystrokes anywhere. Includes the channels already **ruled out
  by experiment**, so don't re-test them.
- [`plans/2026-08-01-prompt-by-reference-implementation.md`](superpowers/plans/2026-08-01-prompt-by-reference-implementation.md)
  — six tasks, T1→T6, none started. Update its status boxes as you go.
- The crux to get right is **T2's credential scoping**: the meter's bearer token
  also unlocks `PUT/DELETE /v1/projects` and `/v1/policy/decide`. An agent pod
  holding it could rewrite its own budget. The prompt route needs a strictly
  weaker, route-scoped principal.

Both closed by that plan:

- **`gonk-e9m` (P1)** — the keystroke/injection boundary. Bang-stripping today
  is a mitigation; T5 deletes it, and T6 proves the channel with a bang in the
  issue body.
- **`gonk-m6t` (P2)** — the same row carries `model` + `metadata`, restoring the
  attribution seam.
  Per-bead attribution no longer reaches the pod; spend rows are attributed
  per-install. Ruled out by experiment: create `options` (select-only, rejects
  free text), file staging (skipped — `GC_K8S_PREBAKED=true`), session env (no
  env field on the create body). **What works: the pod knows its own
  `GC_ALIAS`** (verified in the live pod env), and it already has netpol egress
  to `gonk-meter`. Prompt-by-reference is the shape.
- **`gonk-9lp` (P3)** — qwen3-14b emitted `"add":"gonk::bug","gonk::needs-..."`
  instead of an array. The shape gate catches it, which is the design working;
  worth tightening the prompt before blaming the ladder.

### Facts measured live (do not re-derive these)

| thing | value |
|---|---|
| pod create → commandable session | ~90s |
| submit → terminal event, idle | 10.8s |
| submit → terminal event, agent mid-turn | 36–51s |
| submit → `resolve_failed` event | 262ms |
| create → `create_failed` event | sub-second |

The asymmetry is structural, not noise: a create *failure* is plain validation
and fires immediately, a create *success* waits on `WaitForSessionCommandable`
(up to 120s upstream). `gonk-dispatch`'s order timeout went 120s → 300s because
of it — the old value killed the delivery it was waiting for.

### Debugging recipes that paid off

- **Read the city event log.** `GET /v0/city/{city}/events?type=...&limit=N` is
  unsigned and is the *only* place an async request's outcome exists. Every
  silent failure in this session was already being reported there.
- **A throwaway signed probe.** Build a tiny binary against `pkg/gcapi`,
  `kubectl cp` it into the controller pod, run it there — it reads the mounted
  write key, so no secret leaves the cluster, and you get the exact signed call
  gonk-gate makes without a deploy.
- **`kubectl cp` the rebuilt `gonk-gate` and run it by hand** with
  `GC_WEBHOOK_ARG_*` set. Full real dispatch path, no chart release, no pod
  recreation — which sidesteps both traps below entirely.

### Deploy state at handoff — READ THIS BEFORE TRUSTING THE CLUSTER

**The fix is committed and pushed but NOT DEPLOYED.** HEAD is `3b04f0bf77c3`;
the running controller is still `v0.1.0-d0443ac159e7`. Every live result above
was produced by a hand-copied `gonk-gate` binary at `/tmp/gonk-gate-new` inside
the controller pod — which is *gone* the moment that pod restarts. **The
webhook path in the cluster right now still has the old, broken dispatch.**

Roll forward the same way as session 4: wait for the pipeline on HEAD, then bump
the four tags in `steve/gitops`
`clusters/orac/apps/gonk/helmrelease-gonk.yaml` and commit straight to main.

Also parked in `ns gonk`: **eight agent pods**, most of them wedged from this
session's experiments. `go-4m4` is the good one (issue 21, completed with a
fence). `go-8oj` and `go-55a` are the shell-mode wedge, preserved as evidence
for `gonk-e9m`. `go-d3y`/`go-57b` were the manual test rigs. Reap them when the
reaping fix (`fc77fb4`) actually ships. Test issues 18–21 exist in project 75.


## 2026-07-31 (session 4): deployed and running; ONE blocker left, and it is upstream

**Start here: `gonk-u1p.7`.** Everything else in the broker slice now works. The
agent still never receives its prompt, and both delivery mechanisms are broken
in Gas City's k8s runtime provider.

### The state of the world

The stack is DEPLOYED and HEALTHY for the first time. `ns gonk`, Flux-managed,
HelmRelease `Ready=True`, four pods on **multi-arch** images
`v0.1.0-d0443ac159e7`. GitLab wiring confirmed end to end: an issue on project
75 produces `POST /order/gonk-dispatch/run` → `POST /sessions` → `POST
/session/{alias}/submit`, all 2xx, and sweep reads the session and its
transcript. Project state resolves `valid`. The meter virtual-key blocker that
haunted earlier sessions did NOT reappear.

What still does not happen: the agent gets a prompt. opencode boots, wires to
`qwen3-14b` via LiteLLM, and sits at its idle splash forever.

### Why, precisely (both paths, both silent)

1. **Create-time inject.** `template_overrides.initial_message` lands on
   `runtime.Config.PromptSuffix`, which the PROVIDER must append to the launch
   command. tmux/acp/herdr/t3bridge do; `internal/runtime/k8s` never references
   the field. Reported as **gastownhall/gascity#4891**.
2. **Post-create submit** (the workaround this session shipped).
   `internal/runtime/k8s/provider.go:535` — `Nudge` discards the carrier's error
   and returns `nil` unconditionally. Delivery fails, the API answers 202,
   nobody can tell. `SendKeys` is the same.

So `pkg/gcapi.SubmitSession` + `deliverPrompt` are correct against the
documented contract and well tested — and cannot work on this backend. **Do not
revert them**; they become right the moment upstream delivers. They are simply
not the unblock.

**The remaining path is option (c):** carry the prompt in out-of-band and have
`gonk-agent-entrypoint` — which is OURS — feed it to opencode directly. Needs
nothing from Gas City. Read `internal/runtime/k8s/staging.go` first: it already
stages files into the pod, which is the likeliest channel. `gonk-u1p.7` has the
reasoning and an explicit warning not to "fix" this with a resubmit loop (it
cannot distinguish undelivered from still-thinking).

### What landed this session

- **Multi-arch images** (`gonk-owi`, `gonk-owi.1`). orac is 5/7 arm64 and the
  images were amd64-only, which took gonk-meter down with `exec format error`.
  `homelab/ci-templates!1` and `!2` added rootless native per-arch legs +
  a crane manifest merge; gonk builds `TARGETARCH`-aware images. Note
  `--custom-platform`, opencode's `x64` naming, and dolt's arch-specific tar dir.
- **Four broker fixes**: `.1` prompt delivery (see above), `.5` nothing ever
  stamped `SessionEndedAt` so sweep skipped every bead forever, `.2` an in-flight
  session was judged as failed and escalated, `.3` the batch was read from a
  400-line peek preview instead of the transcript.
- **`gonk-u1p.4`**: the session round-trip test that did not exist. `gcapitest`
  now models a lifecycle (running/stopped/crashed) instead of a static output
  map, which is what let all of the above hide.
- **A regression I introduced and fixed** (`fc77fb4`): declining to judge a
  running session must not mean waiting forever. The reservation is the deadline.

### Traps to know before you touch anything

- **Every commit to gonk `main` re-releases the chart** (its version embeds the
  git SHA), recreating pods — even for docs-only commits.
- **…and every intake restart then drops webhooks for up to 10 minutes**
  (`gonk-fan`): the project reads `unsynced` until the next reconcile, and
  events are accepted with a 200 and thrown away. Silent from GitLab's side. It
  bit twice this session. Together these make live iteration painful.
- **Three wedged agent sessions are parked in `ns gonk`** (`go-844`, `go-d3y`,
  `go-1pr` — issues 16 and 17). They have no prompt, so they never finish. The
  reaping fix (`fc77fb4`) is committed but **NOT YET DEPLOYED**; once it is,
  they get reaped at reservation expiry. Until then they idle.
- **`steve/gitops` takes DIRECT COMMITS to main** — no MRs. `homelab/ci-templates`
  does take an MR (its globals merge into every consuming pipeline).
- **Upstream issues: draft them, do not post them.** The owner reviews, rewrites
  and posts. See `docs/upstream/CONTRIBUTIONS.md` and the bd memory
  `upstream-issues-owner-posts-them-not-me`.

### Deploy state at handoff

- Deployed: `v0.1.0-d0443ac159e7`. HEAD is `e9775b8` — so the reaping fix and
  the latest bead updates are **committed but not deployed**.
- To roll forward: wait for the pipeline on HEAD to go green, then bump the four
  tags in `steve/gitops` `clusters/orac/apps/gonk/helmrelease-gonk.yaml` and
  commit straight to main.

## ⚠️ 2026-07-28 (session 3): broker slice CODE + CHART COMPLETE; C7 blocked on a homelab infra incident

**All broker code is merged** (`main@a677e09`): C2, C4, **C6** (`gonk-0y6`, chart drops agent-pod forge creds), **`gonk-uvv`** (controller's own `GONK_GITLAB_URL` + bot-PAT fallback — was a real broker blocker), and a CI **lint fix** (`opercfg` staticcheck nit that was failing every image build). The overlay controller image `v0.1.0-a677e095b958` is built and correct (`helm upgrade --dry-run` clean).

**C7 (`gonk-dxo`) is blocked on a cluster infra incident, not code** — filed as **`gonk-1lu`**. Four failure modes, all egress/networking: CI image builds hang at the zot-mirror→Docker Hub pull; local full builds fail on `proxy.golang.org` resets; registry pushes 499 through Ingress; and `kubectl port-forward` to the registry won't bind (same class as the CI runner "pod watcher not synced"). **The exact finish-recipe (offline `gonk-gate` overlay build → push → `helm upgrade` → test issue in project 75) is on `gonk-dxo`.** Once the registry-push path is healthy, C7 is ~20 min of work.

## TL;DR — where to start

**The triage broker's whole software path (dispatch inject + sweep apply) is code-complete, tested, and pushed** as of session 2 (2026-07-28). What remains is a **live e2e deploy** (`gonk-dxo`, C7) — which needs a cluster + rebuilt images — plus one small P2 chart-hygiene item (`gonk-0y6`, C6, recipe on the bead). If you have a cluster: build+push images, `helm upgrade`, file a test issue, and confirm the broker posts a triage comment with zero creds in the agent pod. If not: do C6 (golden-testable, no cluster) or pick from the remaining P1s below.

### Session 2 (2026-07-28) — what landed
- **C2 `gonk-7v5`** (closed): dispatch creates the triage agent session directly (one signed `POST …/sessions` with a unique alias + the rendered prompt as `initial_message`), records the alias on `Record.SessionID`, and injects controller-fetched, 8 KiB-capped issue context. No formula (#4668 moot).
- **C4 `gonk-gf3`** (closed): sweep reads `GetSessionOutput(peek)`, extracts the `GONK_BATCH_START/END` fence, `effects.ParseBatch`→`LoadShape`→`Validate`→`ValidateTargets`, and applies comment(+marker)+labels under the bot PAT. Apply result folds into `gate.Signals` so the whole ladder is reused. Mutation-tested.
- **`gonk-uvv`** (closed, NEW bug found+fixed): the controller had **no** `GONK_GITLAB_URL` and no marker-free token file, so `cfg.gl()` built a tokenless client — every broker forge call (and v1 sweep's reads) would have 401'd. Fixed: `GitLabTokenFile` falls back to the marker-free `GONK_BOT_FILE`; chart adds `GONK_GITLAB_URL`. This was the real blocker for the broker working live.
- **Closed 4 stale 2026-07-24 deploy bugs after verifying each is already fixed**: `gonk-4td` (envArg verbatim — added regression tests), `gonk-nke` (`GONK_BEAD_REPO_DIR=/city`), `gonk-aql` (`GC_K8S_IMAGE` wired), `gonk-tff` (dolt tag no longer the dead `v1.43.0`). The deploy is much closer than the tracker implied.

### Remaining (no particular order)
- **`gonk-0y6` (C6, P2)** — chart: stop injecting `GONK_BOT_TOKEN`/`GONK_GITLAB_URL` into agent configs. Exact recipe on the bead. Functionally non-blocking (entrypoint already ignores them); golden-testable, no cluster.
- **`gonk-dxo` (C7, P1)** — live e2e zero-creds proof + the C2 return-read observation (does `initial_message` reach opencode; does the fence land in `GetSession(peek)`). Needs cluster + images.
- **`gonk-qfk` (P1)** — likely SUPERSEDED by the broker (agent makes no forge/git calls; PREBAKED skips clone). Re-evaluate during C7; note on the bead.
- Other P1s untouched: `gonk-4nk` (meter key split-brain), `gonk-wgq` (report LiteLLM OOM upstream), `gonk-6gs` (blocked on upstream #4668).

## C2 is done — what landed this session (all green, pushed)

The create/correlate/inject linchpin resolved **more cleanly than the prior plan**: the earlier "no simple CreateSession REST call" was an incomplete search. The city-scoped `POST /v0/city/{city}/sessions` (huma `humaHandleSessionCreate`) for `kind:agent` is always-async and **accepts both a unique `alias` (the correlation marker) and a `message` (→ `template_overrides.initial_message`, the inject)** — so create + correlate + inject collapse into **one signed POST**. No formula (so #4668 is moot), no `ListSessions` scan, no separate submit.

- `pkg/gcapi` (commit `e55827a`): `CreateSession` (signed exactly like `RunOrder`) + `GetSessionOutput` (UNSIGNED GET `?peek=true&peekLines=N`; 404 → `IsNotFound` so sweep can tell "no session for this alias" from a transport error). Extracted `doRequest` — RunOrder's tests guard the refactor.
- `cmd/gonk-gate` dispatch (commit `f52bf78`): `issue-triage` → `runBrokerDispatch` — creates the `triage` agent session with alias `gonk.triage.p<pid>.i<iid>.a<attempt>` (colon-free per `session.ValidateAlias`, attempt-suffixed so re-slings don't collide), records it on `Record.SessionID`, injects the rendered prompt. **scaffold/mention still pour their formulas** (not ported). Anti-drift test preserved on the broker path.
- Context injection (commit `397df6f`): the pod has **no forge creds**, so the controller fetches the issue (`dispatchDeps.Forge`, `cfg.gl()`) and splices title/labels/body into the prompt, **8 KiB size-capped** on a UTF-8 boundary. Best-effort: a fetch miss → reference-only prompt, logged, run proceeds. Added `glab.Issue.Description`.
- Closed **`gonk-4td`** (P0): the `envArg` verbatim-name fix was already in `main.go`; added the missing regression tests.

**The one thing C2 has NOT proven** (deferred to C7, needs a live cluster + rebuilt images): that `template_overrides.initial_message` actually reaches opencode as its first prompt, and that the `GONK_BATCH_START`/`END` fence lands in `GetSession(peek).LastOutput` at `peekLines=N`. C4 can be built now against `GetSessionOutput(peek).LastOutput` — that read is well-grounded from the gascity source.

## (historical) The original C2 knot — kept for context

## What this project is doing right now

Building the **triage broker** — the first slice of the v2 agent build loop. Spec:
`docs/superpowers/specs/2026-07-27-triage-broker-design.md`. The idea: the agent is a
pure producer of a **proposed-effects batch** (`[{comment,body},{label,add}]`); a
controller-side broker validates its **shape deterministically** and applies it under
its own bot PAT. The agent holds **no forge creds**. Escalation is cost-class-gated in
the meter (local-free, cloud gated by a human allowance).

## Done and merged this session (all green, pushed)

- `pkg/effects` — proposed-effects contract, deterministic per-kind cardinality **shape gate**, target-binding, `effect-shape.toml` loader; `pack/agents/triage/effect-shape.toml` = `comment {1,1}, label {0,N}`.
- **Meter cost-class escalation** — `opercfg.RungSpec.MaxTurns` (local 30 / cloud 12), default-off `CloudAllowed()`, and the gate in `rung.Decide` (cloud rung + no allowance → the existing Deny→needs-human). Service wires `cfg.CloudAllowed()` into `rung.Input`. **Budget arithmetic untouched.**
- `pkg/glab.CreateIssueNote(ctx, projectID, issueIID int64, body string)` — the broker's comment-apply (mirrors `AddIssueLabel`; body is a query param — fine for a comment).
- `beadstore.Record.SessionID` — dispatch↔sweep correlation (whole-struct JSON, round-trips free).
- **Agent pod stripped of forge creds/CA** — `entrypoint.sh` glab/bot-token block gone; `Dockerfile.agent` `SSL_CERT_FILE`/`GIT_SSL_CAINFO` gone. Only the LiteLLM key + tool binaries remain.

Beads closed: `gonk-bwl/4ji/yy9/jll` (effects), `gonk-xcp/6qh/fma` (meter), `gonk-904` (S1), `gonk-h8c` (glab note), `gonk-ffu` (SessionID), `gonk-1t3` (strip creds). Epic: `gonk-u1p`.

Validation at handoff: `gofmt` clean, `go build ./...` OK, `go vet ./...` clean, `go test ./...` all pass, `internal/packtest` pass, `go test -tags chart ./internal/charttest/...` pass.

## The channel — settled by two spikes (don't re-litigate)

- **S0 (proven):** the agent **pod has NO working `bd`/`gc`** (`no beads database found`; split store topology). The return channel is **NOT** the pod writing a bead. Do not try to make the pod write beads.
- **S1/C2a (pinned):** inject and read go over the Gas City session API, both signed exactly like `pkg/gcapi` `RunOrder` (X-GC-City-Write grant; see `pkg/gcapi/client.go`):
  - **Inject prompt:** `SubmitSession(id, msg, intent)` → `POST /v0/city/{city}/session/{id}/messages`.
  - **Read result:** `GetSession(id, peek=true, peekLines=N)` → `SessionView.last_output` preview (a single signed GET; **NOT** `gc session logs`, which needs a session_key and reads reconciler traces).
- Verified against a gascity source clone at `GASCITY_REF` (`images/versions.env`). If the clone is gone, re-clone `github.com/gastownhall/gascity` at that SHA and grep `internal/api/client.go`.

## C2 — the open design decision (the linchpin)

There is **no simple `CreateSession` REST call** — `gc session new` goes through
`config.ResolveSessionCreateTransport`. So the broker must correlate a session to a
dispatch some other way. **Preferred approach:** reuse the existing supervisor
**pool-spawn** of the triage session, stamp a **unique marker** (the bead anchor as a
session alias/metadata) at dispatch, find the session via `ListSessions`, and record
its id in `Record.SessionID`. Alternative: drive the transport-create path directly
(heavier). Decide this first, then:

1. Add thin `pkg/gcapi` wrappers `SubmitSession` + `GetSessionOutput` (TDD, mirror `RunOrder` signing).
2. Wire `cmd/gonk-gate/dispatch.go` to create/correlate + inject the rendered prompt (fetch issue context with the controller PAT, size-cap it, tell opencode: "emit your proposed-effects JSON fenced with `GONK_BATCH_START`/`GONK_BATCH_END`; call no external API").
3. **First live run OBSERVES** whether `GetSession(peek, peekLines=N)` holds the whole fenced batch — this closes the last C2a question and unblocks C4. Record the result in the plan's "Return-read observation" line.

Then: **C4** (`gonk-gf3`, `sweep.go`) reads via `SessionID`→`GetSession`, `effects.ParseBatch`+`Validate`+`ValidateTargets`, applies comment(+marker)/labels via `pkg/glab`, decides apply/escalate/reject. **C6** (`gonk-0y6`) drops the chart's agent-pod cred injection. **C7** (`gonk-dxo`) is the zero-creds e2e proof + the two safety cases.

## Environment / operational notes

- **Throwaway e2e namespace `gonk-e2e-opencode-e1a3c471` is still running** (all gonk + ecosystem pods). GitLab project 75 = `agentic/gonk-e2e-1784441480`, bot user 49, hook id 3. Bailey GPU is up; the metered path (LiteLLM `stub-local` → `ollama_chat/qwen3:14b`) works. Teardown when done: delete the ns, delete hook 3, restore the GitLab local-webhook setting; keep bot 49 + project 75.
- **⚠️ Rotate the GitLab bot token.** Earlier this session `gc config explain` printed the bot PAT in plaintext (it renders agent `[env]` unredacted). Rotate it. (The broker design removes the token from the pod anyway, but the leaked value should be rotated.)
- **Local build → in-cluster registry push recipe** (CI image jobs are blocked by a homelab zot-mirror/Docker-Hub egress outage — infra, not our code; `lint`/`test`/`chart-lint` pass in CI): `kubectl port-forward -n gitlab svc/gitlab-registry 5000:5000`, `podman login localhost:5000 -u steve` (glab token), tag+push to `localhost:5000/agentic/gonk-project/<img>:<tag>` with `--tls-verify=false`. Ingress 499s on large layers, hence the port-forward.
- **Upstream bug filed:** `gastownhall/gascity#4668` (formula-order dispatch drops caller vars). The broker slice **sidesteps it** (dispatch creates work directly, not via the `gonk-triage` formula), so it is not a blocker — it would only let us keep the tidier formula path if fixed.

## Subagent workflow that worked

Dispatched pure-Go, disjoint-package tasks as `isolation: worktree` subagents (effects, meter, glab-note, SessionID, strip-creds), then merged + **re-verified each myself** (`go test ./...` on the whole tree, not just the touched package). That caught a real regression the subagents' narrow scopes missed (the cloud gate denying cloud rungs across the meter-service + integration suites). Keep doing that. The remaining C2→C7 work is **sequential integration**, not parallelizable — drive it directly.

## Beads

- Epic: `gonk-u1p`. Next: `gonk-7v5` (C2) → `gonk-gf3` (C4) → `gonk-0y6` (C6) → `gonk-dxo` (C7).
- `bd show gonk-7v5` has the pinned API paths + correlate decision on it.
- `bd memories broker` recalls the cross-session summary.
