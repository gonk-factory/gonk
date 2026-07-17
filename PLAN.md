# gonk plan index

Spec: docs/superpowers/specs/2026-07-12-gonk-stack-design.md

| Plan | Scope | Status |
|---|---|---|
| 01 foundation & config contract | scaffold, CI, gonkcfg, atags | done |
| 02 gitlab-intake | webhooks, reconciliation, onboarding MR | done |
| 03 gonk-meter | rung policy, key provisioning, ledger | done |
| 04 pack & images | agents/formulas/orders, docker images | in progress (Tasks 2, 4, 5, 6, 7 done) |
| 05 chart | Helm chart, BYO seams | not started |
| 06 e2e harness | kind + gitlab-ce + stub model, kill tests | not started |

Update the Status column as tasks complete (house rule: progress lives here).

## Contracts published by plan 01

- `pkg/gonkcfg` — `.gonk.yml`: schema (`Validate`), typed load (`Load`), and
  precedence resolution (`Resolve` -> `Effective`). Semantics are ADR-002.
- `pkg/atags` — attribution tags joining LiteLLM spend to beads/sessions.
  The literal key and trigger strings are the ledger contract (spec 10.1).
- `docs/schemas/gonk-config.v1.schema.json` — published schema, gated against
  drift from the embedded canonical copy and against accidental change.

## Contracts published by plan 02

- `pkg/glab` (+ `pkg/glab/glabtest`) — a minimal typed GitLab REST client on
  the trust boundary: its own size caps, retry policy, PAT rotation
  (`SetPreviousToken` / `AuthFallback`, AD-4b), split-credential hook
  management (`AdminToken`, AD-4), and an `APIError` that never carries the
  token. `glabtest` is an in-memory fake GitLab good enough to drive every
  test in this plan with no network. TLS verification is never disabled
  (ADR-003.13) — trust the private CA via `SSL_CERT_FILE`, never
  `InsecureSkipVerify`.
- `pkg/ghook` — webhook trust boundary: `Verifier` (constant-time, multi-slot
  token check), `NewHandler` (fails closed on a nil verifier/deduper/sink or a
  zero `BotUserID` — never construct a bare `ghook.Handler{}`), `Deduper`,
  event parsing (`ParseEvent`), and the sanitized `Observer.WebhookOutcome`
  interface (the attacker-controlled `X-Gitlab-Event` header is collapsed to
  the handled allow-list or `"other"` before it ever reaches an observer —
  Prometheus labels are never attacker-controlled here).
- `pkg/meterapi` — the wire contract between gonk-meter (server, Plan 03),
  gitlab-intake (config + Gate-1 client, this plan), and the Gas City pack
  (Gate-2 client, Plan 04). **Plan 03 must import this package as its
  normative source and re-derive nothing** — its Task 0 is where the contract
  is authoritative; this plan merely lands the file first and conforms to it.
- `pkg/intake` — reconciliation (`Reconciler.ReconcileOnce`, `.Loop`, `.Kick`,
  `.WaitForNextPass` — HB-1), the project state machine (`Classify`, `State`,
  the `May*` predicates), deterministic onboarding (`GitLabOnboarder`,
  `RenderDefaultConfig`'s empty-ladder backstop), Gate-1 dispatch (`Decide`,
  `Dispatch.Handle`, `MayFire`, the staleness cutoff), the naming functions
  every other plan joins on (`RigName`, `SessionKey`, `BeadAnchor`), the
  Prometheus `Metrics` (spec 8's exact series names), and the two-listener
  `Server` (`Public()` = hook only, `Private()` = health/metrics/admin).
- `cmd/gonk-intake` — the binary: file-mounted secrets only (never an env
  value), the bot-identity refusal (wrong token owner refuses to start), and
  `ADR-003` (the trust-boundary decisions this plan locks in).

## Contracts published by plan 04 (Task 2, in progress)

- `pkg/gcapi` (+ `pkg/gcapi/gcapitest`) — the real Gas City supervisor
  order-run client, closing Plan 02's OD-A: `POST
  /v0/city/{cityName}/order/{name}/run`, body `{"vars":{...}}`, response
  `{status, scoped_name, tracking_id}`. Bounded retry (`MaxRetries`, default
  3) on 429/5xx only, a 64 KiB response cap (`readCapped`, error not
  truncate), `APIError`/`IsNotFound`, and an empty `City` refused at call
  time rather than silently building `/v0/city//order/...`. Order name and
  city are `url.PathEscape`d, never concatenated. `APIError` carries only the
  RESPONSE status/path/body — never the request's `vars` — so a `key_ref`
  (a Secret NAME, never key material) cannot leak into a log line via
  `err.Error()`. `gcapitest.Server` is an in-memory fake recording every
  `Pour` (order + vars) for Task 3's dispatch/re-sling tests, with an
  injectable `Fail` count per order name to drive the retry path. **Not yet
  wired in**: `pkg/intake.HTTPDispatcher` (the Plan 02 interim client) still
  exists unchanged — Task 2's brief was `pkg/gcapi` itself, not swapping
  callers. The wire shape is verified byte-identical to `HTTPDispatcher`'s
  (same route, same `{"vars":{...}}` envelope), so a later swap (or a thin
  wrapper) is a no-op; whichever plan/task wires it into `cmd/gonk-gate`
  (Task 3) or replaces `HTTPDispatcher` should do so deliberately, not by
  accident of import order.

## Contracts published by plan 04 (Task 4)

- `pack/` — the gonk Gas City pack: `pack.toml` (schema 2, `[pack]` +
  `[agent_defaults]` only — every other legal table is unused), three agents
  (`agents/{triage,scaffold,mention}/agent.toml` + `prompt.template.md`),
  three formulas (`formulas/gonk-{triage,scaffold,mention}.toml`, each a
  single `[[steps]]` with a `[steps.check]` exec verification loop), and five
  orders (`orders/gonk-dispatch.toml` and `orders/gonk-sweep.toml`, both
  exec/no-pool; `orders/gonk-{triage,scaffold,mention}.toml`, formula
  orders). No `[[webhook]]`, no `[[service]]` (see pack.toml's own comments
  on both). `internal/packtest` is the offline structural-validation gate (17
  tests) — it is not a substitute for Task 6's real loader in a container,
  but it does prove (with a permanent regression test,
  `TestPackTOMLUsesOnlyKnownTopLevelTablesCatchesABadKey`) that an invented
  `pack.toml` key is caught before Task 6 ever runs.
- **Step 1's research turned up three corrections to this plan's own Task 4
  worked examples** (verified against the MIT `gascity` source at
  `4fda5a28445f42d6e789fc7f5751645ac4fecd19`, not guessed):
  1. **`schema` lives at `[pack].schema`, not as a bare top-level key.**
     `PackConfig` (`internal/config/pack.go`) has no top-level `Schema`
     field; `PackMeta.Schema` (`internal/config/config.go`) is what the
     loader reads. A bare `schema = 2` above `[pack]` is exactly the kind of
     stray key the undecoded-key check is built to catch.
  2. **`[[steps]]` has no `agent` field.** Routing to a specific agent is via
     the *order's* `pool` (a pool can be a single agent's own name — Gas
     City's tutorial 07 confirms this is a supported target). This pack
     gives each of the three agents its **own** order-level pool
     (`pool = "triage"` / `"scaffold"` / `"mention"`), superseding this
     plan's OD-2 "one shared pool" assumption: Gas City routes a pool's
     ready work to *any* agent whose work query matches that pool label
     (`docs/tutorials/06-beads.md`), so one shared pool across three agents
     with three different prompts would let any of them pick up any other's
     bead. OD-2 itself flagged this as revisable with trivial blast radius.
  3. **`bead_id` is a formulas-v2 *reserved* variable name.**
     `internal/graphv2/invocation.go`'s `ValidateNoReservedUserVars` rejects
     ANY caller-supplied vars map containing a `bead_id` key —
     `"formulas v2 reserved variable \"bead_id\" cannot be supplied by the
     caller"` — regardless of whether the formula declares it. Every pour in
     `cmd/gonk-gate/dispatch.go`'s `runDispatch` targets a formula order, and
     its vars map had a literal `"bead_id"` key (from Task 3). **Fixed**:
     renamed to `"city_bead_id"` in the vars map (dispatch.go) and in every
     formula (`pack/formulas/*.toml`); regression test
     `TestDispatchNeverSendsAReservedFormulaVarName`
     (`cmd/gonk-gate/dispatch_test.go`) and
     `TestFormulaVarsNeverDeclareAReservedFormulasV2Name`
     (`internal/packtest`) pin it from both sides. Nothing else about the
     wire (meterapi's own `bead_id` field, `GC_WEBHOOK_ARG_BEAD_ID` on the
     *exec* orders) changed — the reservation is graph.v2-formula-vars-only.
- **Two known gaps, flagged rather than silently patched over (both need a
  decision/implementation this task's remit does not cover):**
  1. **`[steps.check]`'s real exec environment does not match
     `cmd/gonk-gate check`'s input contract.** Confirmed from
     `internal/convergence/condition.go`: Gas City sets `GC_BEAD_ID` /
     `GC_ITERATION` / `GC_WORK_DIR` / `GC_STORE_PATH` / `GC_ARTIFACT_DIR` /
     `GC_MOLECULE_DIR` for a check script — **never** `GC_WEBHOOK_ARG_*`
     (that convention is exec-*order*-only:
     `internal/webhookmatch/extract.go`). `cmd/gonk-gate check`
     (`cmd/gonk-gate/main.go`) currently reads
     `project_id`/`issue_iid`/`bead_id`/`trigger` exclusively via
     `GC_WEBHOOK_ARG_*`, which will be **unset** at real invocation time.
     Each formula step now stamps `project_id`/`issue_iid`/`city_bead_id`/
     `trigger` onto the checked bead's own metadata
     (`[steps.metadata]`) so a fix has somewhere to read them *from* — but
     the read-back itself is not implemented, because it needs the `bd` CLI's
     exact invocation surface, which `pkg/beadstore`'s own doc comment says
     is confirmed in Task 6's container smoke test, not here. **Task 6 must
     close this before `[steps.check]` can pass against the real loader** —
     see `pack/scripts/gonk-check.sh`'s comment for the full trail.
  2. **`discussion_id` (mention-reply's thread target) is declared in
     `orders/gonk-dispatch.toml`'s `[order.params]` and in
     `formulas/gonk-mention.toml`'s `[vars]`, but `cmd/gonk-gate/dispatch.go`
     does not actually plumb it through**: `dispatchArgs` has no
     `DiscussionID` field, and the pour step's `vars` map has no
     `discussion_id` entry. Mention-reply will load and dispatch correctly,
     but the agent will not know which thread to answer in until this is
     added (a `dispatchArgs.DiscussionID` field, an `envArg("discussion_id")`
     read in `main.go`, and a `vars["discussion_id"]` entry in the pour).

## Contracts published by plan 04 (Task 5)

- `images/versions.env` — the ONE file every image pin lives in. Real,
  verified pins (not guessed): `OPENCODE_VERSION=1.18.3` (OD-3 settled —
  npm `opencode-ai`'s `latest` dist-tag as of 2026-07-16; the matching
  `github.com/anomalyco/opencode` release ships a self-contained
  `opencode-linux-x64.tar.gz`, sha256-verified in the Dockerfile — note
  upstream `sst/opencode` release URLs redirect to `anomalyco/opencode`),
  `GLAB_VERSION=1.108.0` / `BD_VERSION=1.0.3` (match the `glab`/`bd` already
  on this box; fetched from their real release artifacts with sha256
  checksums verified in-Dockerfile — a mismatch fails the build), `GASCITY_REF`
  carried over from Task 4, and `DEBIAN_BASE` / the `golang` build stage
  pinned by **digest** (`Docker-Content-Digest` header, resolved live, not
  guessed) ahead of Task 7's own digest-pinning gate.
- `images/Dockerfile.agent` — multi-stage (gonk-gate build stage; a `fetch`
  stage for opencode/glab/bd so `curl`/`tar` never ship in the runtime image
  and a checksum mismatch fails the BUILD; the runtime stage). Runs as
  `65532:65532`, `SSL_CERT_FILE`/`GIT_SSL_CAINFO` point at the private-CA
  mount, no `InsecureSkipVerify` anywhere. Builds clean with `make images`
  (podman, `--network=host`) — verified for real, ~2m8s, image then removed
  to avoid leaving ~560MB of cruft on this sandbox.
- `images/agent/entrypoint.sh` — the attribution seam (OD-7), VERIFIED against
  opencode's own source at the pinned tag (not assumed): `@ai-sdk/openai-compatible`
  is compiled into the opencode binary (no npm-registry egress at session
  start — matters for spec 9's "agent pods reach only GitLab and LiteLLM"),
  `provider.gonk.options.headers["x-litellm-spend-logs-metadata"]` is
  rendered with `jq` (not string concatenation, so an embedded quote in the
  metadata JSON cannot corrupt the config) from `GC_WEBHOOK_ARG_METADATA_JSON`
  stamped verbatim, and the virtual key is opencode's own `{file:<path>}`
  config substitution — `entrypoint.sh` never reads the key into its own
  process at all. Proven in a running container
  (`test/images/agent_smoke_test.go`'s
  `TestAgentImageAttributionOverlayCarriesAllSevenAtags`, which round-trips
  the rendered header through `pkg/atags.FromMetadata` itself, not a
  hand-copied key list).
- `images/agent/prepare-commit-msg` — a **provisional passthrough stub**
  (Task 8 owns the real body; Task 5's own Dockerfile COPIES this path, so it
  has to exist to build). Defers to `gonk-gate trailers` the moment that
  subcommand exists; never blocks a commit.
- `Makefile` targets `images` (agent only for now — Task 6/7 each add their
  own Dockerfile's build line), `push` (correct, not run — no LAN reach to
  `registry.orac.local` from this sandbox), `no-latest` (Task 7's exact gate,
  working today; note the banned substring is assembled via `$(empty)` so the
  check's OWN source line does not trip itself), and `pack-validate` (fails
  loud with a named reason — Task 6's real-loader validation does not exist
  yet; this is deliberately NOT a silent no-op).
- `test/images/agent_smoke_test.go` (build tag `images`) — 5 tests, all real,
  all run against a live built container: pinned-version assertions
  (opencode/glab/bd), git works, non-root (`65532`), gonk-gate binary present
  with its documented exit-code contract, and the attribution overlay test
  above. One named skip: `gonk-gate --version`/`trailers --help` (see gaps
  below).
- **Two gaps found and flagged, not silently patched:**
  1. **`cmd/gonk-gate` has no `--version` flag and no `trailers` subcommand**
     (confirmed against `main.go`, 2026-07-16). `trailers` is Task 8's own
     file — out of Task 5's remit. `--version` is a smaller, cross-task gap:
     both this task's own smoke-test wishlist AND Task 6's controller smoke
     test want it, so it is not exclusively Task 8's problem — whichever
     task adds `var version string` + a `--version`/`-v` check to
     `cmd/gonk-gate/main.go` closes it for everyone. Until then,
     `images/Dockerfile.agent`'s `-ldflags -X main.version=...` is a
     harmless no-op linker directive (confirmed: `-X` on a nonexistent
     symbol does not fail a Go build).
  2. **The virtual key's file-mount path has no owner yet.**
     `cmd/gonk-gate/dispatch.go` hands the pack `key_secret_name` +
     `key_secret_key` — a Kubernetes Secret name + key — as order vars.
     Turning that into an actual Secret volume mount on the agent POD is a
     Gas City session-provider / chart concern Task 5 cannot reach (it owns
     the image and its entrypoint, not the pod spec). `entrypoint.sh` reads
     the key's path from `GONK_LITELLM_KEY_FILE` (one more `*_FILE` env var,
     matching every other secret in this repo) and refuses to start if it is
     unset or missing — but nothing yet sets `GONK_LITELLM_KEY_FILE` to
     wherever the Secret named by `key_secret_name`/`key_secret_key` actually
     lands. **Flagged for Plan 05 (chart / session-provider pod spec) or
     Plan 06 (live wiring) to close.**
- **Plan 06 must live-verify**: the attribution overlay is confirmed against
  opencode's own source at the pin (compiled code, not documentation), but
  NOT against a real request reaching a real LiteLLM through a real running
  opencode process — `docs/environment.md`'s own smoke test used a hand-built
  HTTP request, not opencode itself. A future opencode version bump must
  re-confirm `provider.<id>.options.headers` still exists at the new pin
  before trusting it (Task 5 Step 4's own instruction).

## Contracts published by plan 04 (Task 6)

- `images/Dockerfile.controller` — `gc` built from MIT gascity source at the
  pinned `GASCITY_REF` (`git clone` + `git checkout` to the exact SHA, refused
  if unset) + `gonk-gate` (vendored, offline, `-X main.version=$(GONK_TAG)`) +
  `bd` (checksum-verified fetch) + the pack baked at `/opt/gonk/pack/`. Runs
  as `65532:65532`, `SSL_CERT_FILE` at the private-CA mount. Builds clean with
  `make controller-image` — verified for real (~5m19s cold, ~1m12s with the
  `gc` build-stage layer cached), image then removed after testing.
- **GO_VERSION bumped 1.26.2 -> 1.26.5** (`images/versions.env`), a REAL,
  REPRODUCED build failure, not a hypothetical one: gascity's own `go.mod`
  AT THE EXACT PINNED `GASCITY_REF` SHA requires `go >= 1.26.5`; the `gc`
  build stage failed outright against 1.26.2. 1.26.5 still satisfies gonk's
  own `go 1.26` directive and is what this dev box's local toolchain already
  is, so this is a safe, shared bump. Digest re-resolved the same way as the
  original pin (`Docker-Content-Digest` on the tag's manifest-list).
- **The REAL Gas City loader result: gc lint ACCEPTS the gonk pack, verified
  by actually building and running `gc` from the MIT source at the pinned
  SHA** (not simulated, not assumed — confirmed both by hand and in
  `test/images/packvalidate_test.go`'s `TestGasCityLoaderAcceptsTheGonkPack`,
  which asserts `passed:true, error_count:0` on `gc lint /opt/gonk/pack --json`
  inside the built controller image). The negative control
  (`TestGasCityLoaderRejectsAnUnknownKey`) proves the check is real: an
  injected `[gonk_invented_table]` is rejected, by name, in the error text.
- **REAL FINDING: `gc lint` does NOT validate order-level semantics**
  (formula XOR exec, no pool on an exec order — `internal/orders/order.go`'s
  `Validate`). Confirmed by hand: an order declaring both `formula` and
  `exec` passes `gc lint` with zero diagnostics. The actual enforcement point
  is `orderdiscovery.ScanAll` (`internal/orderdiscovery/discovery.go`),
  reached via `gc order list --city <dir> --json` — THE SAME CODE PATH the
  real controller's `order_dispatch.go` uses to build its live dispatch set.
- **REAL FINDING, and the more important one: that path does NOT hard-fail
  a bad order.** Every caller in gascity's own `cmd/gc` (order list/show, and
  the controller's own dispatch scan) wires `OnValidateError` to log the
  violation and continue — `internal/orderdiscovery`'s `validateOrders`
  drops the one bad order and keeps the rest. Confirmed by hand: an injected
  formula+exec order (and, separately, an injected exec+pool order) makes
  `gc order list --json`'s order count drop from 5 to 4, names the order and
  the exact rule violated on stderr, and **still exits 0**. So a malformed
  order in production does not crash the controller — it silently vanishes
  from the routing table, with only a log line (easy to miss) marking the
  loss. `test/images/packvalidate_test.go`'s `TestLoaderDropsAnOrderWithBothFormulaAndExec`
  / `TestLoaderDropsAnExecOrderWithAPool` assert this REAL (not fabricated)
  behavior — not a fictional non-zero exit code — and
  `TestGasCityLoaderLoadsAllFiveGonkOrders` is the positive control proving
  all 5 real gonk orders load with none dropped.
- **`pkg/beadstore/bd.go`'s guessed `bd` invocations were WRONG in FOUR
  places, all confirmed and fixed against the real `bd` 1.0.3 binary**
  (matches `images/versions.env`'s `BD_VERSION` and the `bd` already on this
  box) — this is worse than Task 4's `bead_id` finding, because every one of
  these four bugs was **silent**: `Put`/`Get` would have run against a real
  Dolt bead store, reported success, and done nothing.
  1. `bd label <id> remove/add <label>` (verb after the id) is not valid
     usage — cobra prints the `label` subcommand's help TO STDOUT and exits
     **0**. The real order is `bd label remove/add <id> <label>`.
  2. `bd label remove <id> "gonk::*"` does **not** glob-expand (confirmed:
     the literal label `gonk::*` is "removed" — a no-op — while the real
     `gonk::<state>` label stays). Fixed by enumerating the bead's actual
     labels (`bd label list <id> --json`) and removing each `gonk::`-prefixed
     one by its literal name (`removeGonkStateLabels`).
  3. `bd create ... ` with no `--json` flag prints human-readable text, not
     JSON — `Put`'s very first bead-creation call would have failed to
     decode on every cold start. Fixed: `--json` added (and `--label` ->
     `--labels`, the documented flag name, confirmed to also work).
  4. `bd comments <id> --json` names the comment body **`text`**, not
     `body` — the original struct tag (`json:"body"`) silently decoded to
     an empty string on every row, so `Get`/`List` would NEVER find an
     existing gonk-state comment no matter how many times `Put` had run.
     Fixed: `json:"text"`.
  All four are pinned by `bd_test.go` (a fake `Run` hook, no real `bd`
  needed for the normal gate) AND were independently verified end-to-end
  against the real local `bd` 1.0.3 binary (create -> comment -> label
  transition -> list-by-state round trip, by hand, before writing the
  file). Also fixed in the same pass: `bd list`/`bd comments --format json`
  -> the documented global `--json` boolean flag (the former happened to
  also produce JSON, undocumented — not worth depending on).
- **`cmd/gonk-gate --version` added** (`var version = "dev"`, stamped via
  `-ldflags -X main.version=$(GONK_TAG)` in both Dockerfiles), closing the
  cross-task gap Task 5 flagged and this task's own controller smoke test
  needed. Verified in the real image:
  `gonk-gate --version` → the exact `GONK_TAG` string, exit 0, no
  `GONK_CITY`/meter-token config required (unlike every real subcommand).
- `test/images/controller_smoke_test.go` + `test/images/packvalidate_test.go`
  (build tag `images`, excluded from the normal gate — verified: a plain
  `go test ./...` run shows no `test/images` package at all) — 9 tests, all
  real, all run against the live-built controller image: pinned-version
  assertions (`gc version --long` contains the full `GASCITY_REF` SHA —
  REAL FINDING: `gc --version` is not a flag, `gc version` is a subcommand,
  and only `--long` includes the commit; `bd --version`), `gonk-gate
  --version`, the pack baked in, non-root, the real-loader accept/reject
  pair, and the three order-semantics tests above.
- **Not closed by this task, carried forward honestly**: PLAN.md's Task 4
  notes flagged that `cmd/gonk-gate check`'s env-var contract
  (`GC_WEBHOOK_ARG_*`) does not match Gas City's real `[steps.check]`
  invocation (`GC_BEAD_ID`/`GC_ITERATION`/etc, per
  `internal/convergence/condition.go`), and hinted this task's `bd`
  confirmation would unblock a fix. It does (the invocation surface is now
  correct), but implementing the `GC_BEAD_ID` -> `bd show`/metadata
  read-back path itself is a distinct, non-trivial change to
  `cmd/gonk-gate/main.go`/`check.go` outside this task's six explicit steps
  (Dockerfile, loader-validation surface, pack-validation test, controller
  smoke test, `bd` pin, commit) — flagging it again here rather than
  rushing it in. **Whoever runs a formula through a real `[steps.check]`
  next (Plan 06, or an earlier owner of this gap) must close it before
  that formula's check step can pass against a real running city.**

## Contracts published by plan 04 (Task 7)

- `images/Dockerfile.intake` / `images/Dockerfile.meter` — the two Plan
  02/03 service binaries, each a vendored offline Go build
  (`-mod=vendor`, no `go mod download` — CI/this box cannot reach
  proxy.golang.org, same as Dockerfile.agent/.controller) onto a
  distroless `nonroot` runtime (`65532:65532`, no shell, no apt
  packages). `SSL_CERT_FILE` points at the private-CA mount; NEITHER
  Dockerfile bakes a secret or sets one as an ENV value — gonk-meter's
  LiteLLM admin key, meter bearer token(s), and store DSN are all FILE
  MOUNTS the chart wires at runtime (Plan 05), matching
  `cmd/gonk-meter/main.go`'s own Config doc comment. Both built clean with
  `make intake-image` / `make meter-image` (podman, `--network=host`) —
  verified for real (~76s / ~106s cold), and smoke-run inside the
  container: each binary's own fail-closed startup error surfaced
  correctly on distroless (`GONK_INSTANCE_LADDER is unset` /
  `LITELLM_URL is required`), proving the static binary executes on a
  base with no libc assumptions beyond what `CGO_ENABLED=0` already
  buys. Both confirmed running as uid/gid `65532:65532`
  (`podman inspect --format '{{.Config.User}}'` — distroless has no
  `id` binary to shell out to). Images removed after testing, per house
  rule.
- **The `testclock` variant** (`make meter-testclock-image`,
  `--build-arg BUILD_TAGS=testclock`) — the e2e-only meter whose clock
  can be moved by `cmd/gonk-meter/clock_testclock.go`'s
  `GONK_TESTCLOCK_FILE` seam (Plan 03's HB-3, Plan 06's gate). Tagged
  `$(GONK_TAG)-testclock`, never the production tag. Built clean
  (~106s cold).
- `images/versions.env` — added `DISTROLESS_BASE`
  (`gcr.io/distroless/static-debian12:nonroot`), pinned by digest the
  same way as `DEBIAN_BASE` (`Docker-Content-Digest` on the tag's
  manifest-list, resolved 2026-07-16, confirmed pullable with `podman
  pull` from this box — gcr.io is not blocked here).
- `Makefile` — `intake-image` / `meter-image` / `meter-testclock-image`
  targets; `images` now builds the full four-image list (was
  agent+controller only); `push` pushes all four plus the testclock
  tag; a new `scan` target (Trivy, `--ignore-unfixed`, spec 10.3 — not
  run this task, no `trivy` binary on this sandbox, flagged not
  silently skipped).
- **REAL BUG FOUND AND FIXED in the `no-latest` gate itself** (inherited
  from Task 5, exposed by this task's own required "verify it has
  teeth" step): `chart/` does not exist yet (it is Plan 05's own
  deliverable). GNU grep exits **2** — an ERROR, not "no match" (1) —
  when *any* path argument passed to it is missing, regardless of what
  it finds in the paths that *do* exist. The old recipe was
  `! grep -rn ... images/ Makefile chart/ || (echo FAIL; exit 1)`; `!`
  only distinguishes zero from nonzero, so exit 2 (error, because
  `chart/` is absent) and exit 1 (no match) both flip to a silent pass
  — **a real planted `:late$(empty)st` in `images/Dockerfile.intake`
  did NOT fail `make no-latest`** until this was fixed. Verified both
  ways: `grep -rn ... chart/` alone reports exit 2 while still printing
  the matched line; the same grep with `chart/` dropped from the
  argument list correctly reports exit 0 (match found, `!` then trips
  the FAIL branch). Fixed by only passing paths that currently exist to
  grep. **This means Task 5's `no-latest` target has had zero teeth on
  this box since it landed** — flagging it here rather than treating
  it as already covered by the earlier task's own "verified" claim.
- `test/images/nolatest_test.go` (build tag `images`, but PURE STATIC
  ANALYSIS — no podman needed to run it, unlike every other test in
  this package) — `TestNothingSaysLatest` (walks `images/**`,
  `Makefile`, `chart/**` — skipped, not failed, while absent —
  `test/**`; catches a floating tag, an untagged `FROM`, an untagged
  `image:`) and `TestBaseImagesArePinnedByDigest` (resolves every
  `${VAR}` FROM-line against `images/versions.env` and asserts the
  result carries `@sha256:`). **Verified RED then GREEN for real**: a
  planted `FROM debian:latest AS planted-test` line in
  `images/Dockerfile.intake` failed both `make no-latest` and
  `go test ./test/images/... -tags images -run TestNothingSaysLatest`
  before being reverted, confirmed clean again after.
- `test/images/servers_smoke_test.go` (build tag `images`) —
  pinned-image-exists smoke tests for intake and the production meter
  (each proves the real binary runs on distroless via its own
  fail-closed startup error), non-root checks for both, and the
  testclock guard Task 7 Step 2 calls for:
  `TestProductionMeterImageHasNoTestClock` (extracts
  `/usr/local/bin/gonk-meter` from the production image via
  `podman create` + `podman cp` — distroless has no shell to `cat`
  with — and asserts neither the `GONK_TESTCLOCK_FILE` literal nor the
  `testclock` string appears in the binary) and its converse,
  `TestTestclockMeterImageHasTheSeam`, against the testclock image.
- **REAL FINDING, non-blocking**: `cmd/gonk-meter/main.go`'s
  `loadConfig` checks its five required env vars via
  `for name, v := range map[string]string{...}` — Go map iteration
  order is randomized, so *which* one gonk-meter reports first when
  several are missing is not deterministic across runs (confirmed: an
  early version of `TestMeterImagePinsMatchVersionsEnv` asserting the
  message named `LITELLM_URL` specifically flaked across repeated
  runs). Not a correctness bug (every branch still fails closed,
  `exit 1`) but an operator-facing rough edge — a real deployment
  missing two secrets gets a different first error message each
  restart. The test here was corrected to assert only what is actually
  guaranteed (`exit 1`, the generic "is required" shape); the
  underlying nondeterminism in `main.go` is flagged, not fixed, as
  outside this task's remit.
- **`neither cmd/gonk-intake nor cmd/gonk-meter declares `var version
  string``** (confirmed against both `main.go` files, 2026-07-16) —
  unlike `cmd/gonk-gate` (Task 6 added `--version`), the two service
  binaries have no linkable version symbol yet. Both Dockerfiles still
  pass `-ldflags -X main.version=$(GONK_TAG)` (harmless no-op, same
  precedent as Task 5's note about gonk-gate before Task 6 closed it)
  so adding the symbol later needs no Dockerfile change. gonk-intake's
  own version string (used in onboarding MR templates) already comes
  from the `GONK_VERSION` env var at runtime (`main.go`'s
  `Config.Version`), which is a distinct thing from a `--version` flag.
  Flagged, not fixed — outside this task's four Dockerfiles-and-gate
  scope.

## Carried into later plans

- **Plan 03 (highest-value item):** `Effective.Budget`'s "unlimited" sentinels
  are `math.Inf(1)` (`MonthlyCostUSD`) and `math.MaxInt64`
  (`MonthlyTokens`/`PerTaskTokens`). `gonk-meter`'s ledger/API will serialize
  `Effective.Budget`, and **`+Inf` is not JSON-serializable** —
  `encoding/json.Marshal` returns an error rather than a number. Special-case
  it (e.g. `null`, a sentinel string, or omission) before marshaling, or an
  unlimited project will fail to serialize. See ADR-002.
- **Whoever owns schema v2:** `SchemaVersion` was a decorative, unreferenced
  constant until this pass — it is now enforced by exactly one test
  (`TestSchemaVersionConstMatchesEmbeddedSchema` in
  `pkg/gonkcfg/drift_test.go`), which fails if the const and the schema's
  `properties.version.const` drift apart. Keep both in sync when cutting v2.
- **Plan 05:** the instance-level ladder is load-bearing operator config, not
  optional. Per spec 5.4 the ladder is an allow-list, so silence at a layer
  means "impose no constraint," not "allow nothing" — if the operator sets no
  instance ladder, a project may name any rung it likes, bounded only by
  budget ceilings. Total silence at *every* layer fails closed (see ADR-002),
  but partial silence (operator silent, project sets a ladder) is fail-open on
  rung choice. **The chart's default values should ship a non-empty instance
  ladder** rather than relying on an empty one to be safe.
- **Plan 03:** nothing validates operator-supplied instance/group `Policy` —
  they bypass the schema and the non-finite-float check. `Resolve` fails closed
  on the one dangerous case (a NaN ceiling), so this is not exploitable today,
  but the operator-config path needs its own validation. See ADR-002 "Known gap".
- **Plan 03:** `atags` accepts any string for `Project`/`Rung`. Safe inside the
  package, but a value containing a newline or comma could cause injection or
  column-shift bugs if metadata is serialized into a header, log line, or CSV
  downstream. Enforce at the boundary, not in the contract package.
- **CI:** the `lint`/`test` jobs have never run — every runner on
  gitlab.orac.local was offline during plan 01. The standing gate is local:
  `gofmt -l .`, `go vet ./...`, `go test ./... -race -count=1`,
  `golangci-lint run ./...`.
- **Toolchain:** Go 1.26 minimum (`go.mod` says `go 1.26`). The lint image must
  be built with a Go >= that directive or golangci-lint refuses to run at all
  (`golangci-lint:v2.1.6` is built with go1.24.2 and exits 3 against this
  module). CI pins `golangci-lint:v2.12.2` (go1.26.2) and `golang:1.26`; v2.12.2
  is also the local gate's version, so local and CI cannot disagree. Keep them
  pinned together when bumping either.
- **Dependency:** `github.com/prometheus/client_golang` (and its transitive
  deps) is vendored under `vendor/` as of plan 02 — CI has no reach to the
  module proxy, so `go mod vendor` after any `go get` is mandatory, not
  optional. `go build`/`go test` in this repo run with the implicit
  `-mod=vendor` a committed `vendor/` triggers; forgetting to re-vendor after
  adding an import fails the build with "import lookup disabled by
  -mod=vendor", not a proxy error, which is a good thing to recognize on
  sight.
- **Plan 03 Task 0b (blocking spike, RUN, decisive FAIL):**
  `docs/spikes/dolt-reservation-isolation.md` records that Dolt
  `2.1.10` (`dolthub/dolt-sql-server:latest`) does **not** serialize the
  concurrent-reservation race under any tested strategy — default isolation,
  explicit `SERIALIZABLE`, and explicit `SELECT ... FOR UPDATE` all let all
  32 racing writers win against headroom for 2, every one of 90 iterations,
  zero variance. A direct check confirmed `SELECT ... FOR UPDATE` does not
  block a concurrent holder at all (a second transaction acquired the
  "locked" row in <1ms while the first held it, uncommitted).
  **Decision (per the plan's own preference order): Fallback 1 — keep Dolt
  for durability, run meter single-replica (AD-10), and let the in-process
  `keyedMutex` be the actual atomicity for `ReserveIfFits`.** Fallback 2
  (Postgres on the owner's CNPG cluster, owner-approved 2026-07-13) remains
  available without further spike work if meter ever needs to scale
  horizontally. **Task 6's `store/dolt.go` must not assume Dolt transactions
  make `ReserveIfFits` atomic** — see the spike doc for the full mechanism
  and raw numbers. `github.com/go-sql-driver/mysql` (+ `filippo.io/edwards25519`)
  is now a direct dependency and vendored, for Task 6's use as well as this
  spike's. **`ADR-004` (Task 10) must cite this document and state the
  decision in as many words** — an unverified transactional guarantee under
  a budget ceiling is exactly the thing that must not be quietly assumed,
  and this one was checked, not assumed.

## Carried into later plans (plan 02)

- **Plan 03 (meter) — the shape is settled, not yet built:** meter **owns
  `pkg/meterapi`** (see "Contracts published by plan 02" above) and
  implements `PUT`/`GET`/`DELETE /v1/projects/{project}` + `GET /healthz`.
  **Meter validates and resolves the raw `.gonk.yml`** intake sends —
  `gonkcfg.Resolve` is called in exactly one place in the whole system, and it
  is there (ADR-003.6). Meter **owns quiet hours**, surfaced to intake as a
  `defer` (ADR-003.7). A `422` from `PUT /v1/projects/{project}` means *the
  project's yaml is bad* (meter records it, deletes the key, echoes its error
  to intake as `ErrInvalidConfig`); a `400` means *intake's request is bad* and
  is a bug on intake's side, never retried blindly. `null` in a
  `meterapi.Budget` field means *unlimited*; `meterapi.ZeroBudget()` is the
  explicit fail-closed value, never an empty `Budget{}` (that is all-nil, i.e.
  unlimited). Meter no-ops on an unchanged `config_hash`, **except** that
  `Reconciler.MeterResyncInterval` forces a periodic re-`PUT` even then,
  because meter's answer can change (an operator's instance/group policy
  moving) without the project's file moving. **Intake calls
  `POST /v1/policy/decide` as Gate 1** (before the first dispatch, and again
  for the `.agent/` scaffold order); the pack's exec order is Gate 2 (spec
  6.2.3) — the actual enforcement point, re-deciding on every pour.
  `meterapi.DecideRequest` carries no attempt field on either gate, ever (a
  caller-supplied attempt is a ladder-climb forgery vector — meter reads its
  own store).
- **Reservation reclamation (Plan 03) + idempotent order-firing (Plan 04)** —
  found in Plan 02's final review. When intake gets a `run` from Gate-1
  `/decide`, meter has already opened a reservation; if the subsequent
  `FireOrder` then fails (a supervisor blip), intake logs `fire_error` and drops
  the event — it does NOT requeue, and the reconcile loop re-derives only
  `scaffold`, never issue-triage, so that one triage is silently lost and meter's
  reservation dangles until it expires. This is the SAFE direction (no spend),
  but: **Plan 03 must reclaim/expire dangling reservations (a TTL), and Plan 04's
  `pkg/gcapi` should make `FireOrder` idempotent-retryable keyed on the
  deterministic `BeadAnchor`** so a transient supervisor failure retries the same
  order rather than dropping the work.
- **Plan 04 (pack):** (a) the Gas City order API shape — **OD-A is RESOLVED**:
  `POST /v0/city/{cityName}/order/gonk-dispatch/run`, body `{"vars":{...}}`,
  no per-route auth (admission by network position). `intake.HTTPDispatcher`
  (`pkg/intake/dispatch.go`) is a **minimal, interim client** for this shape —
  it marshals `OrderRequest` through its own JSON tags into `vars` and posts
  to a hardcoded `cityName` of `"gonk"` (there is exactly one Gas City
  instance in this design). **Plan 04's `pkg/gcapi` does not exist yet**;
  when it lands, either replace `HTTPDispatcher` with a thin wrapper over it,
  or confirm `gcapi`'s wire shape is byte-identical to what `HTTPDispatcher`
  already sends, so a swap is a no-op. (b) **the controller must treat
  `OrderRequest.BeadAnchor` as an idempotency key** — intake can fire the same
  order twice across a restart (duplicate delivery, crash mid-flight), and a
  duplicate bead means duplicate spend. (c) **the pack is Gate 2**: it calls
  `POST /v1/policy/decide` before every session spawn and never trusts
  intake's order vars; a `defer` is a NORMAL answer (park the bead, retry at
  `retry_after`). Quiet hours land here (and, as a metric only, at intake's
  Gate 1); intake carries no `not_before`. Task 3 Step 8's shared decision
  table (intake's `MayFire` vs. the pack's pour rule) still needs writing.
  (d) the pack must **never** send an attempt count. (e) the session posts
  every triage label/comment; intake's only GitLab write on the dispatch path
  is the narrow Gate-1 `deny` label (`intake.GitLabDenyLabeler`, default
  `gonk::denied`, via the new `glab.Client.AddIssueLabel`).
- **Plan 05 (chart):** the chart's **default instance ladder must be
  non-empty** — it is the source `GONK_INSTANCE_LADDER` sets from, which
  `GitLabOnboarder`/`RenderDefaultConfig` render a new project's `ladder:`
  from (OD-B). `cmd/gonk-intake`'s `loadConfig` refuses to start on an
  empty/unset `GONK_INSTANCE_LADDER` (`TestLoadConfigRejectsEmptyInstanceLadder`),
  and `RenderDefaultConfig` refuses to emit an empty `ladder:` even if that
  guard were bypassed — belt and braces, because intake never runs `opercfg`
  (a different process, `opercfg.Load`, separately refuses to start *meter*
  on the same condition). Secrets are **file mounts from existingSecret
  refs**, never env values: `GONK_GITLAB_TOKEN_FILE` +
  `GONK_GITLAB_TOKEN_PREVIOUS_FILE` (AD-4b), `GONK_GITLAB_ADMIN_TOKEN_FILE`
  (optional, no second slot), `GONK_WEBHOOK_SECRET_FILE` +
  `GONK_WEBHOOK_SECRET_PREVIOUS_FILE`, and `GONK_METER_TOKEN_FILE` (intake
  presents slot 1 only — `GONK_METER_TOKEN_PREVIOUS_FILE` is **meter's** env
  var). Ship an **alert rule on `gonk_intake_projects{state="invalid"} > 0`**
  and one on `gonk_intake_gitlab_auth_fallback_total > 0`. Two listeners: only
  `GONK_LISTEN_ADDR` (`:8080`, the hook) goes behind the Ingress;
  `GONK_PRIVATE_ADDR` (`:9090`: metrics/health/`POST /admin/reconcile`) must
  not. The image needs no system tzdata for intake's own logic (quiet hours
  moved to meter) but `cmd/gonk-intake/main.go` still imports `time/tzdata`
  defensively; it **does** need the private CA — mount trust-manager's
  `trust-bundle` ConfigMap (key `tls-ca-bundle.pem`) and set
  `SSL_CERT_FILE=/etc/ssl/orac/ca.crt`. There is no config value anywhere
  that disables TLS verification.
- **Plan 06 (e2e):** **HB-1 is shipped**: `POST /admin/reconcile?wait=true`
  (`pkg/intake/server.go`, backed by `Reconciler.WaitForNextPass`) blocks
  until a pass that started at or after the request completes, then returns a
  JSON `ReconcileSummary` — this is what lets the harness assert on
  reconciliation without sleeping. The items in "Plan 06 e2e verification"
  below (golden payloads, real-GitLab hook provisioning, the full onboarding
  scenario, the kill test, the real-meter seam, quiet hours end to end) are
  unchanged from the plan text and still need a live GitLab/meter to check —
  nothing in plan 02's own test suite substitutes for them. Plan 06 also owns
  the honest accounting of the NetworkPolicy gap: the egress-denial test is
  written and skipped until Cilium lands.
