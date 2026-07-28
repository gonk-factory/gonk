# Handoff — next session

_Last updated: 2026-07-28. Branch: `main` (we develop on main per owner's call). Everything below is committed and pushed._

## TL;DR — where to start

Build **C2** (bead `gonk-7v5`): the triage broker's inject/correlate step. It is the one remaining hard knot; everything after it (C4 sweep → C6 chart → C7 e2e) is straightforward once C2 lands. The API paths and the return channel are already pinned (below). Read `bd show gonk-7v5` and `docs/superpowers/plans/2026-07-27-triage-broker-implementation.md` (Phase 0 S1/C2a + Phase 3/4) first.

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
