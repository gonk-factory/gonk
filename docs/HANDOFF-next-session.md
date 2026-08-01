# Handoff — next session

_Last updated: 2026-07-31 (session 4). Branch: `main` (we develop on main per owner's call). Everything below is committed and pushed._

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
