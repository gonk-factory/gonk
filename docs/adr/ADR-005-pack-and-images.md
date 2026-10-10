# ADR-005: the pack, the two-gate budget model, and the container images

Status: accepted 2026-07-17

This ADR records the decisions Plan 04 (`pack/`, `pkg/gate`, `pkg/gcapi`,
`pkg/beadstore`, `cmd/gonk-gate`, `images/`) locks in — the Gas City pack, the
deterministic gate binary that is the only path from "work exists" to "a
session spawns", and the four container images. It cross-references ADR-002
(config precedence), ADR-003 (Plan 02's intake trust boundary), and ADR-004
(Plan 03's rung policy and budget enforcement).

**A numbering note, because a future maintainer will otherwise "fix" it: this
plan's own text calls this document "ADR-004".** That number was already
taken — `docs/adr/ADR-004-rung-policy-and-budget-enforcement.md` is Plan 03's,
merged to `main` before this plan started. This document is **ADR-005**.
**Consequence: Plan 05's own chart ADR, wherever the chart plan's text refers
to itself, is `ADR-006`, not `ADR-004` or `ADR-005`.** The plan sequence's ADR
numbers have shifted by one from whatever any earlier plan doc predicted; go
by what is actually merged on `main` (`ls docs/adr/`), never by a plan
document's own guess at its number. Task 1 of this plan separately added a
short "known false positive" paragraph to the *existing* `ADR-004` (see
below) — that is correct and deliberate, not a numbering slip: the paragraph
is topically part of the rung/outcome-classification ADR, and it was not
moved here.

---

## 1. Spec §6.2.3 is wrong; the rung gate is in two places

Spec §6.2.3 says *"before session spawn, the dispatch formula asks meter for
the rung."* **A formula cannot make an HTTP call.** Per
`formula-spec-v2.md`, a formula is a DAG of `[[steps]]` handed to *agents*;
the only code a formula can execute is `[steps.check]`, an inline
verification loop whose only mode is `exec`, and even that is re-run
repeatedly and must be side-effect-free. If the rung decision lived in a
formula step, it would be made by an LLM — which violates spec §6.3's "no
LLM judges" at the exact point where money is spent.

**Owner decision (settled, not reopened): the decision is gated in two
places, and both speak `pkg/meterapi`.**

- **Gate 1 — `gonk-intake`** gates the *first* dispatch. It calls
  `POST /v1/policy/decide` and fires the order only on `decision: "run"`,
  passing `rung`, `model`, `metadata`, `key_ref`, `reservation_id` as order
  vars (never an attempt count — see §2). On `defer` it records
  `retry_after` and fires nothing; on `deny` it labels and fires nothing.
- **Gate 2 — `orders/gonk-dispatch.toml`**, an **exec** order (no `pool` —
  exec orders may not have one), wraps `gonk-gate dispatch`. It calls
  `/v1/policy/decide` *again*, immediately before pouring the formula, and
  it is **the only code path in the entire system that pours a formula.**
  A ladder escalation after a failed gate is controller-initiated — it never
  passes through intake at all.

Gate 1 is an optimization: it avoids churning a bead meter will certainly
deny, and gives the operator an early `defer` metric. **Gate 2 is the
enforcement point.**

**The drift table, copied verbatim from the plan, especially the
CATASTROPHIC row:**

| Drift | Consequence | Severity |
|---|---|---|
| Intake says `run`, Gate 2 says `defer` | Order fires, no session spawns, bead parks. Wasted order. Self-heals on the next sweep. | **Benign** — this is the *designed* race, and it is why Gate 2 exists. |
| Intake skips `/decide` entirely (regression to Plan 02 as originally written) | Orders fire for out-of-budget / quiet-hours / disabled projects; Gate 2 refuses them all. Cost: churned orders and a noisy `defer` metric. | **Benign but loud.** Gate 2 still holds the money door. |
| **Gate 2 trusts intake's order vars instead of re-deciding** | **CATASTROPHIC.** Every controller-initiated re-sling reuses the *original* rung, the *original* `reservation_id` and the *original* `key_ref`. The escalated attempt runs **unbudgeted**, spend is **mis-attributed to the previous attempt's tags**, and `POST /v1/policy/outcome` is bound to a reservation that is already closed — so meter's ladder state and reality diverge silently. **"Budgets cannot be bypassed" becomes false at the application layer, not just the network layer.** | **This is the one. `TestDispatchAlwaysDecidesEvenWhenVarsCarryARung` (`cmd/gonk-gate/dispatch_test.go`) exists solely to prevent it. Never delete it.** |
| They disagree on `deny` (intake treats it as retryable) | Intake re-fires the same doomed order every reconcile pass, forever. No spend (Gate 2 refuses), but a metric/log flood and a churned bead per pass. | **Bounded.** Caught by `gonk_intake_orders_denied_total` climbing monotonically. |
| They disagree on `defer` retry semantics (both retry the same bead) | Two retriers → potentially two sessions on one bead → **duplicate spend**. | **Mitigated by `BeadAnchor` idempotency** (Plan 02's carry-forward; Plan 06 kill tests K13/K18). The controller **must** dedupe on `BeadAnchor`. If it does not, this drift is a spend bug. |

## 2. The invariant

> **No formula is ever poured except by `gonk-gate dispatch`, and
> `gonk-gate dispatch` always calls `/v1/policy/decide` first. There is no
> second pour path. A rung escalation must be paid for by a decision meter
> made.**

Both gates key meter's budget/ladder state on the **deterministic
`BeadAnchor`** (`gonk:{project}:issue:{iid}`), never the Gas City internal
bead id — the BeadAnchor is the only identifier both gates share (the Gas
City bead does not exist yet at Gate 1) and it is stable across re-slings,
which is exactly what `/decide`'s open-reservation idempotency (ADR-004)
needs. Neither gate ever sends an attempt count — `meterapi.DecideRequest`
has no such field, on either gate, ever (a caller-supplied attempt is a
ladder-climb forgery vector; meter derives attempt from its own outcome
history). `cmd/gonk-gate/contract_test.go`'s `TestBothGatesSendTheSameBeadID`
and the shared `pkg/gate.MayPour` / `pkg/intake.MayFire` predicates pin the
two gates' semantics together mechanically, not by comment.

## 3. The classifier is a sweeper order, not a formula step

Because every `[[steps]]` becomes a bead handed to an agent, a formula
cannot contain a deterministic, non-LLM final step, and `[steps.check]` is a
re-run verification loop that must stay side-effect-free — it can answer "is
the artifact there yet?" but cannot distinguish `gate-failed` from
`infra-failed`, and asking it to try would put a judgement inside a loop that
runs unboundedly.

- **`[steps.check]` runs `gonk-gate check`** — a pure, idempotent GitLab
  query (is the bot's comment carrying the bead marker on the issue?). No
  POST, no state change, safe to run in a loop.
- **`orders/gonk-sweep.toml`** — an exec, cooldown order (`interval = "30s"`,
  no pool) running `gonk-gate sweep`. This is the deterministic brain that
  classifies every finished session, posts `/v1/policy/outcome`, re-fires
  `gonk-dispatch` on `escalate`/`retry`, and re-fires it for parked beads
  whose `retry_after` has passed. No LLM anywhere in it.

## 4. Infra failures never escalate a rung; an escalation must be paid for

`pkg/gate.Classify` (`pkg/gate/outcome.go`) is a pure, total function of
`Signals` gathered from GitLab and gonk-meter — never from the agent, which
could otherwise hallucinate `gate-failed` and climb its own project's ladder
for free. Its priority order:

1. `Aborted` (a human closed the bead, or config changed under the session)
   wins outright — neither escalates nor retries.
2. Any infra indicator (`ReservationExpired`, `ArtifactUnknown`,
   `SpendStale`) wins over any judgement about the work — the gate refuses
   to convert its own outage into somebody's escalation.
3. `ArtifactPresent` → `success`, even at zero tokens: the work exists, and
   success costs nobody a rung.
4. No artifact, and `ModelTokens <= 0` → `infra-failed`. The session did not
   *fail at* the work; it never got *to* the work. This is the line that
   makes "infra failures never escalate" true for pod evictions, cold-start
   timeouts, and LiteLLM 5xx, none of which announce themselves any other
   way.
5. No artifact, and `ModelTokens > 0` → `gate-failed`. This, and only this,
   buys a rung.

`Escalates(outcome) = outcome == "gate-failed"` is a one-line function
guarding the entire cost model; meter enforces the identical rule
independently (`pkg/rung`'s `Outcome.Escalates`, ADR-004) — two agreeing
enforcers, on purpose.

**Known false positive — record it, do not hide it.** A pod evicted *after*
at least one successful completion but *before* posting its comment
classifies as `gate-failed` and buys **one** unearned escalation. It is
bounded (one rung, one attempt) and the escalated attempt still passes
`/decide`, so it cannot exceed budget. Closing it properly needs a
pod-termination signal from Gas City's session provider, which is not in the
facts available to this plan. **Hand-off:** Plan 06 should measure how often
it fires (the K-series kill tests already evict pods); if it is common, it
becomes an upstream ask for Gas City. (This paragraph is the same one
recorded in `pkg/gate/outcome.go` and, verbatim, in ADR-004's "Failure-mode
matrix" addendum from Task 1 of this plan.)

## 5. The bead marker is load-bearing

`pkg/gate.BeadMarker(beadID)` renders `<!-- gonk:bead:<id> -->`. It is how a
deterministic gate reads an agent's output without judging prose — the
triage/scaffold/mention agents must post it verbatim in the one comment they
write, and `gonk-gate check`/`sweep` grep for its literal presence. **A
prompt rewrite that drops it makes the gate blind**: every successful
session is classified `gate-failed` (no artifact found, but tokens were
spent), and the project climbs to its most expensive rung — the bill
arrives before the bug report. This is the single most fragile point in the
system to someone who edits a prompt template without reading this ADR.

## 6. No `[[webhook]]` in the pack

GitLab authenticates webhooks with a constant-time `X-Gitlab-Token`
comparison. Gas City's webhook verification registry
(`internal/config/webhook.go`) is a **closed set of five schemes**:
`github-hmac-sha256`, `hmac-sha256`, `slack-v0`, `discord-ed25519`,
`jwt-jwks`. None of them is a constant-time shared-secret compare —
`hmac-sha256` is an HMAC **over the body** and will not substitute; wiring
it up would either reject every real GitLab delivery or, worse, get "fixed"
by someone disabling verification on the one endpoint exposed to the
ingress.

Even if it were expressible, a `[[webhook.rule]]` cannot suppress
bot-authored events, dedupe redeliveries, fetch and size-cap `.gonk.yml`,
register the project with meter, or decide the project is even onboarded —
`gonk-intake` (Plan 02) already does all of that, on the trust boundary,
with tests. Reconciliation is the correctness path (spec §5.2); webhooks are
a latency optimization only, so losing the pack's own receiver costs nothing
correctness-wise.

**Upstream contribution opportunity, not a dependency:** a `gitlab-token`
verification scheme is a small, clean PR to Gas City's verify registry (spec
§4.4 already names it as a candidate). **Nothing in this plan is built on
the assumption it lands.**

## 7. No `[[service]]` in the pack

`gonk-intake` and `gonk-meter` are standalone Kubernetes Deployments shipped
by the Helm chart (Plan 05), not pack `[[service]]` proxy-process entries. A
`proxy_process` is a controller-supervised local process; gonk's two Go
servers need a CNPG database, ExternalSecrets, independent probes and
scaling, and their own NetworkPolicy — and gonk-meter is **the budget
enforcer**, whose availability must not be coupled to the Gas City
controller's own lifecycle (owner decision, 2026-07-13). This is still
pack-first (spec §1): the pack carries agents/formulas/orders; the chart
carries the two servers.

## 8. The pack names no model, no price, and no site

`AD-1`: the rung catalog and the instance ladder are site-local operator
config (`pkg/opercfg`, Plan 03) — the pack ships neither. A rung *name* is
gonk's (`.gonk.yml`'s `ladder`); the rung *catalog* maps a name to a real
LiteLLM model plus a synthetic price, and that mapping belongs in the
operator's config, not in an open-source pack. `pkg/gate`/`cmd/gonk-gate`
stamp `DecideResponse.Model` verbatim; nothing in `pack/` or `pkg/gate`
hardcodes a model name, and Task 4's own build step greps the whole pack for
model-shaped strings and fails if one appears. The same discipline applies
to `[[pricing]]` (see `pack.toml`'s own comment): synthetic prices for local
rungs are mirrored into LiteLLM's `model_cost_map` by gitops, never by the
pack — a price in an open-source pack would hardcode one site's economics.

## 9. The licensing constraint

`github.com/gastownhall/gascity` is MIT — its source and its spec docs
(`docs/reference/specs/pack-spec.md`, `docs/reference/specs/formula-spec-v2.md`,
`docs/reference/config.md`, `docs/tutorials/06-beads.md`,
`docs/tutorials/07-orders.md`) are the clean derivation basis for the pack,
`pkg/gate`, `pkg/gcapi`, and the images. `github.com/gastownhall/gascity-packs`
carries **no top-level LICENSE**, i.e. all rights reserved. **`gascity-packs`
was never opened during this plan's execution** — it was not read, copied,
adapted, or paraphrased at any point. Every structural choice in `pack/`
(the table layout, the per-agent pools, the `[order.params]` shapes, the
prompt structure) traces to a citation in the MIT `gascity` repo's own spec
docs or loader source, not to the packs repo. Record this in writing so a
future open-sourcing review (spec §12.1) can trust it rather than take it on
faith — the owner is asking the `gascity-packs` maintainers for a written
grant, and until it lands, this pack remains clean-room.

## 10. OD-3 (the opencode pin) and OD-7 (the metadata seam)

**OD-3 — the opencode version.** `images/versions.env` pins
`OPENCODE_VERSION=1.18.3` — the npm `opencode-ai` package's `latest`
dist-tag as of 2026-07-16, with the matching GitHub release's
`opencode-linux-x64.tar.gz` sha256-verified inside the Dockerfile's `fetch`
stage (a checksum mismatch fails the build, not a warning). Never `latest`:
a floating opencode version makes spec §11.4's named risk (ACP session
resume across pod recreation) untestable, because the behaviour under test
would change under you between builds.

**OD-7 — how opencode attaches attribution metadata to LiteLLM. ANSWERED,
VERIFIED IN DESIGN.** opencode supports a static per-provider header,
`provider.<id>.options.headers`, spread into its AI-SDK factory at spawn.
Because one session equals one pod, the session's tags are baked into that
pod's config once, at spawn — no per-request plumbing needed.
`images/agent/entrypoint.sh` renders `overlay/opencode.json` setting
`provider.gonk.options.headers["x-litellm-spend-logs-metadata"]` to the
verbatim JSON of the **seven `pkg/atags` keys**
(`gonk_project`, `gonk_rig`, `gonk_bead_id`, `gonk_session_key`, `gonk_rung`,
`gonk_attempt`, `gonk_trigger`) that meter minted in `DecideResponse.Metadata`
and handed the pod via the `GC_WEBHOOK_ARG_METADATA_JSON` order var. The
render uses `jq`, not string concatenation, so an embedded quote in the
metadata JSON cannot corrupt the config. This mechanism was **smoke-tested
against a live LiteLLM v1.92.0** on 2026-07-13 (`docs/environment.md`,
"VERIFIED: the attribution chain works") — LiteLLM parses the header into
`data["metadata"]["spend_logs_metadata"]` and persists it to
`LiteLLM_SpendLogs.metadata.spend_logs_metadata`, all seven keys intact. It
was independently re-confirmed against opencode's own compiled source at the
pinned tag in Task 5
(`test/images/agent_smoke_test.go`'s
`TestAgentImageAttributionOverlayCarriesAllSevenAtags`, which round-trips the
rendered header through `pkg/atags.FromMetadata` itself, not a hand-copied
key list). **Enterprise-gate caveat:** LiteLLM's docs claim per-k/v
`spend_logs_metadata` is an enterprise feature; it was **not** enforced on
v1.92.0. If a future upgrade starts enforcing it, the documented fallback is
the `x-litellm-tags` header, which populates the `request_tags` column (also
verified populated).

**What is NOT yet verified, in bold, because spec goal 4 (attribution at
every granularity) rests on it:** neither the LiteLLM smoke test nor Task
5's opencode-source confirmation exercised this against **a real running
opencode process making a real request through a real LiteLLM.** Both
checks were against static configuration/source, not a live round trip
through the actual binary in the actual container. **Plan 06 owns the LIVE
proof.** If that live proof ever fell through to "project-level attribution
only" (the per-key virtual-key metadata, which is real but coarser), **that
is a finding for the owner, not something to paper over as success** — say
so in the record, in bold, exactly the way this paragraph does.

---

## The two-gate budget model, restated as one picture

```
GitLab event ──> gonk-intake (Gate 1: /v1/policy/decide, optimization only)
                       │  run
                       ▼
        POST /v0/city/{city}/order/gonk-dispatch/run   (pkg/gcapi, idempotent
                       │                                 on BeadAnchor)
                       ▼
        orders/gonk-dispatch.toml (exec, Gate 2: /v1/policy/decide AGAIN,
                       │           trusts nothing from Gate 1's vars)
                       ▼ run
        pours a formula order (gonk-{triage,scaffold,mention}) ──> agent pod
                       │                                          (opencode,
                       │                                     attribution overlay,
                       │                                        seven atags)
                       ▼
        [steps.check] == `gonk-gate check` (pure, idempotent; is the marker
                       │                     on the issue?)
                       ▼
        orders/gonk-sweep.toml (exec, cooldown 30s) == `gonk-gate sweep`:
          classify (pkg/gate.Classify) -> POST /v1/policy/outcome
                                        -> re-sling on escalate/retry
                                        -> unpark on retry_after elapsed
```

Deterministic, zero-LLM gates: `gonk-intake`'s Gate 1 dispatch path, every
line of `orders/gonk-dispatch.toml`'s script and `cmd/gonk-gate dispatch`,
`[steps.check]`/`cmd/gonk-gate check`, and `orders/gonk-sweep.toml`/
`cmd/gonk-gate sweep`. `grep -rniE "openai|anthropic|completion" cmd/gonk-gate/
pkg/gate/` finds nothing — there is no LLM on the gate path, and the outcome
classifier's own package doc comment states the invariant in capital
letters for exactly this reason.

---

## The images

Four images, one pin file (`images/versions.env` — every version and every
base-image digest lives there, nowhere else):

- **`gonk-agent`** (`images/Dockerfile.agent`) — opencode (pinned, OD-3) +
  `glab` + `bd` + `gonk-gate` (for the `trailers` subcommand, AD-3). Runs as
  `65532:65532`, `SSL_CERT_FILE`/`GIT_SSL_CAINFO` at the private-CA mount, no
  `InsecureSkipVerify` anywhere. The `fetch` stage curls and checksum-verifies
  opencode/glab/bd so `curl`/`tar` never ship in the runtime image and a
  checksum mismatch fails the *build*, not silently produces a bad image.
- **`gonk-controller`** — `gc` built from the MIT `gascity` source at
  `GASCITY_REF` (an exact commit SHA, refused if unset) + `gonk-gate`
  (vendored, offline) + `bd` + the pack baked at `/opt/gonk/pack/`. Runs as
  `65532:65532`.
- **`gonk-intake`** / **`gonk-meter`** — the two Plan 02/03 service binaries,
  vendored offline Go builds onto distroless `nonroot` (`65532:65532`, no
  shell). Neither Dockerfile bakes a secret or sets one as an env value — all
  credentials are file mounts the chart wires at runtime (Plan 05). `meter`
  also builds a **`-testclock` variant** (a separate image *tag*, never a
  runtime flag) for Plan 06's e2e clock-control seam (Plan 03 HB-3); the
  production tag is proven, by test, not to contain the testclock symbol at
  all.

**The `no-latest` gate** (`make no-latest`, `test/images/nolatest_test.go`)
walks `images/**`, `Makefile`, and `chart/**` (skipped while `chart/` does
not yet exist, not failed) for a floating tag, an untagged `FROM`, or an
untagged `image:` reference, and separately resolves every `${VAR}` FROM-line
against `images/versions.env` and asserts the result carries `@sha256:` — no
base image floats even on its own numbered tag. **A real bug was found and
fixed in this gate's own recipe** (Task 7): GNU `grep` exits 2, not 1, when
any path argument is missing, and the original `! grep ... || (echo FAIL;
exit 1)` recipe could not distinguish "no match" from "grep errored because
`chart/` does not exist yet" — both looked like a pass. **This means the gate
had zero teeth from the moment it was written in Task 5 until Task 7 fixed
it**, which is recorded here rather than treated as already covered by the
earlier task's "verified" claim. It is now proven both ways: a planted
`:late$(empty)st` string fails the gate; the gate is clean with the string
removed.

**Provenance trailers** (`cmd/gonk-gate trailers`, installed as a
`prepare-commit-msg` git hook at session start, AD-4 — never agent prose,
because an agent asked to write a trailer will eventually write a
plausible-looking wrong one): `commit_trailers` defaults **on**,
`include_usage` defaults **off** — cost in public git history is a
per-project choice, not gonk's default. When meter reports
`complete: false` for a session's cost (the usual case at commit time, since
the session is normally still open), the hook writes `Gonk-Usage: pending`,
**never a number** — a wrong cost baked into permanent git history can never
be corrected. A trailer lookup never fails a commit: every meter-unreachable
path degrades to a safe default or to `pending`, and losing an agent's real
work over a missing metadata footer is a far worse trade than a missing
footer.

---

## Carry-forwards found during Plan 04's own execution

Recorded here, not silently patched over, because a plan that claims more
than it proves is worse than one that admits the gap (the same standard this
plan holds Plan 06's HB-4 to).

1. **`cmd/gonk-gate sweep`'s "completed session" half is inert until
   something stamps `Record.SessionEndedAt`.** `sweepRunning`
   (`cmd/gonk-gate/sweep.go`) returns immediately for a bead whose
   `SessionEndedAt` is the zero value — "still running, nothing to do this
   tick" — and nothing in this plan's own facts sets it: it needs a real Gas
   City session-lifecycle signal (the supervisor's own `tracking_id`
   completion event), which is outside what this plan can observe (Gas City
   is not deployed during Plan 04's own execution). This fails **closed** —
   no bead is ever misclassified for lack of a signal, so there is no spend
   leak — but it means the sweeper's classify-and-re-sling logic, while
   fully tested against `beadstore.Memory`, does nothing in a real
   deployment until Plan 06 (which deploys a real Gas City) wires the signal
   in.
2. **`cmd/gonk-gate check`'s env contract does not match Gas City's real
   `[steps.check]` invocation.** `check.go`/`main.go` read
   `project_id`/`issue_iid`/`bead_id`/`trigger` via `GC_WEBHOOK_ARG_*`
   (`envArg`), which is the convention for an **exec order's** declared
   `[order.params]` (`internal/webhookmatch/extract.go`) — but Task 6
   confirmed, by reading `internal/convergence/condition.go`, that Gas City
   instead sets `GC_BEAD_ID` / `GC_ITERATION` / `GC_WORK_DIR` /
   `GC_STORE_PATH` / `GC_ARTIFACT_DIR` / `GC_MOLECULE_DIR` for a formula's
   check step, and never the `GC_WEBHOOK_ARG_*` names. Each formula step now
   stamps `project_id`/`issue_iid`/`city_bead_id`/`trigger` onto the checked
   bead's own metadata (`[steps.metadata]`) so a fix has somewhere to read
   them *from*, but the `GC_BEAD_ID` → `bd show`/metadata read-back itself
   is not implemented. This is Plan 06's fix (or an earlier owner of a real
   running city), and it must close before a formula's check step can pass
   against a real running city.
3. **`pkg/beadstore/bd.go`'s `bd` invocations were guessed in Task 4 and
   CONFIRMED-AND-FIXED against the real `bd` 1.0.3 binary in Task 6** — this
   carry-forward is resolved, not open. Four silent-failure bugs were found
   and fixed: (a) `bd label <id> remove/add <label>` is invalid usage — the
   real order is `bd label remove/add <id> <label>`; the wrong order printed
   cobra's help to stdout and exited 0; (b) `bd label remove <id>
   "gonk::*"`  does not glob-expand — fixed by enumerating the bead's actual
   labels (`bd label list <id> --json`) and removing each `gonk::`-prefixed
   one by its literal name; (c) `bd create` needs an explicit `--json` flag
   or it prints human-readable text, not JSON, which would have failed to
   decode on every cold start; (d) `bd comments <id> --json` names the
   comment body `text`, not `body` — the original `json:"body"` struct tag
   silently decoded to an empty string on every row, so a round trip would
   never have found an existing gonk-state comment. All four are pinned by
   `bd_test.go` (a fake `Run` hook, no real `bd` needed for the normal gate)
   and were independently verified end-to-end against the real local `bd`
   1.0.3 binary before the file was written.
4. **`discussion_id` (mention-reply's thread target) is declared but not yet
   plumbed through.** It exists in `orders/gonk-dispatch.toml`'s
   `[order.params]` and in `formulas/gonk-mention.toml`'s `[vars]`, but
   `cmd/gonk-gate/dispatch.go`'s `dispatchArgs` has no `DiscussionID` field
   and the pour step's vars map has no `discussion_id` entry. Mention-reply
   will load and dispatch correctly, but the agent will not know which
   thread to answer in until this is added — a small, well-scoped follow-up
   for Plan 06 or an earlier owner.
5. **The real `gc lint` does not validate order-level semantics** (formula
   XOR exec, no pool on an exec order — `internal/orders/order.go`'s
   `Validate`). Confirmed by hand in Task 6: an order declaring both
   `formula` and `exec` passes `gc lint` with zero diagnostics. The actual
   enforcement point is `orderdiscovery.ScanAll`
   (`internal/orderdiscovery/discovery.go`), reached via
   `gc order list --city <dir> --json` — the same code path the real
   controller's own dispatch scan uses. **And that path does not hard-fail a
   bad order either**: every real caller in `gascity`'s own `cmd/gc` wires
   `OnValidateError` to log the violation and continue, so a malformed order
   silently drops out of the routing table (`gc order list`'s count drops,
   the violation is named on stderr, and the process **still exits 0**).
   `internal/packtest` is therefore **the real gate** for order-level
   semantics in this codebase — not `gc lint`, and not the controller's own
   runtime behavior, which will not tell you your order vanished except in
   a log line. This is proven both positively
   (`TestGasCityLoaderLoadsAllFiveGonkOrders`) and negatively
   (`TestLoaderDropsAnOrderWithBothFormulaAndExec`,
   `TestLoaderDropsAnExecOrderWithAPool`,
   `test/images/packvalidate_test.go`).
6. **Neither `cmd/gonk-intake` nor `cmd/gonk-meter` declares `var version
   string`**, so both Dockerfiles' `-ldflags -X main.version=$(GONK_TAG)` is
   a harmless no-op linker directive (`-X` targeting a nonexistent symbol
   does not fail a Go build) rather than an actual version stamp. Unlike
   `cmd/gonk-gate` (which got `--version` in Task 6, closing the same gap
   flagged in Task 5), the two service binaries have no linkable version
   symbol at all yet. A trivial follow-up: add the `var` and a `--version`
   flag to both `main.go` files; no Dockerfile change is needed when it
   happens, because the linker flag is already there waiting for a symbol
   to bind to.

---

## See also

- `docs/adr/ADR-002-config-precedence-semantics.md` — `.gonk.yml` precedence
  and the `+Inf`/`MaxInt64` → `null` encoding.
- `docs/adr/ADR-003-intake-trust-boundary-and-seams.md` — intake's half of
  the Gate-1/Gate-2 split, and the trust boundary this plan's Gate 2 sits
  behind.
- `docs/adr/ADR-004-rung-policy-and-budget-enforcement.md` — meter's half:
  the three-valued decision, reservation idempotency on `(project, bead_id,
  session_key)`, the ledger backend, and — as of Task 1 of this plan — the
  classifier's known false positive, recorded there verbatim as well as
  here.
- `docs/environment.md` — the live-cluster facts this plan cites without
  re-deriving: GitLab CE 18.10.1, the private CA, the OD-7 LiteLLM smoke
  test, the network-layer NetworkPolicy bypass (Flannel; Cilium suspended).
- `docs/superpowers/plans/2026-07-13-plan-04-pack-and-images.md` — the plan
  this ADR closes out; its "Upstream amendments required" section and
  "Hand-offs: what this plan genuinely CANNOT verify" table are the source
  of the carry-forwards recorded above and in `PLAN.md`.
