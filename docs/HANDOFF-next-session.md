# Handoff: build the gonk-agent image and prove gonk END-TO-END on a real repo

Written 2026-07-19. `main` @ `3ee5a35` (origin/main, CI pipeline 1548 green).

## THE GOAL (do this, in this order)

**Prove gonk works end to end on a REAL repo, with the agent we already have.**
We have never run a full triage where a *real agent session* produces a *real
triage comment*. Everything so far stops short of a live model-driven run:
L1/L2 use the `SyntheticSession` driver + the stub model directly; L3 proved the
webhook + the deterministic, zero-token onboarding MR. The missing proof is the
actual work path:

> file an issue on the test project -> intake -> meter `/decide` -> `gonk-dispatch`
> (grant-gated) -> `gonk-triage` -> a live **opencode** agent session (calling the
> model via LiteLLM) -> a triage **comment** lands on the issue.

That requires the **`gonk-agent` image**, which has never been built. Build it,
then run the scenario above against the real GitLab Orac test project.

### EXPLICITLY DEFERRED — do NOT do this yet
`origin/design-gonk-agent-harness` holds a 357-line spec
(`docs/superpowers/specs/2026-07-19-gonk-agent-harness-design.md`) for a
**broker-lite / proposed-effects / shape-gated** redesign of the agent harness.
That is **next-generation** work. **Do not implement it until gonk is proven
end-to-end on a real repo with the current opencode harness.** The branch is
pushed so it survives; leave it be for now.

## Step 1 - build + push the gonk-agent image

- `images/Dockerfile.agent` (Plan 04): opencode + glab + bd + the attribution
  overlay + the private-CA trust. `make agent-image` (podman `--network=host`;
  fetches opencode/glab/bd from GitHub -- the box reaches github.com).
- Pins in `images/versions.env` (verify current before building):
  `OPENCODE_VERSION=1.18.3`, `GLAB_VERSION=1.108.0`, `BD_VERSION=1.1.0`,
  `GO_VERSION=1.26.5@sha256:...`, `DEBIAN_BASE=debian:trixie-slim@sha256:...`.
  Confirm the opencode 1.18.3 linux-x64 static binary + its sha256 in
  `Dockerfile.agent` still resolve (github.com/anomalyco/opencode v1.18.3).
- Registry push needs a deploy token (owner-authorized workflow -- the classifier
  blocks autonomous creation; the owner has approved this repeatedly):
  `POST /api/v4/projects/69/deploy_tokens {scopes:[read_registry,write_registry]}`
  via steve's admin token, then `podman login registry.orac.local`. **Revoke the
  token when done.**
- `podman push registry.orac.local/agentic/gonk-project/gonk-agent:<TAG>`
  (TAG = `v0.1.0-<gitsha>`). The chart does NOT deploy the agent -- the Gas City
  controller's k8s session provider pulls it at runtime. Set the e2e run's
  `componentImages.agent.tag` to `<TAG>`.
- Also build the **meter-testclock** image for the quiet-hours / month-boundary
  parts of Task 8: `make meter-testclock-image` -> `gonk-meter:<TAG>-testclock`.

## Step 2 - stand up the real-repo e2e (reuse the L3 runbook, Task 7)

Fixtures that PERSIST on GitLab Orac (created 2026-07-18):
- `gonk` **bot user = GitLab user id 49** (the production factory bot). KEPT.
- Test project **agentic/gonk-e2e-1784441480 = project id 75**. KEPT.
- **Tokens do NOT persist** (they were in this session's scratchpad, now gone).
  Re-mint next session via steve's admin token:
  - bot PAT: `POST /api/v4/users/49/personal_access_tokens {scopes:[api,write_repository]}`
  - admin PAT: `POST /api/v4/users/2/personal_access_tokens {scopes:[api]}`
- Service images in the registry (rebuild+push if code changed since `65d404b`):
  `gonk-intake:v0.1.0-65d404b58852`, `gonk-meter:v0.1.0-65d404b58852`,
  `gonk-controller:v0.1.0-470ca24be25a`. Dolt = `docker.io/dolthub/dolt-sql-server:2.1.7`.

Deploy (throwaway ns `gonk-e2e-<runid>`, **trap-teardown FIRST**):
- In-ns throwaway infra: `postgres:16` (ledger, apply meter schema, DSN secret);
  **LiteLLM v1.92.0 + stub model** (meter's litellm; stub as the ONLY upstream;
  mint a **proxy_admin** litellm key -- a plain key 401s on admin routes). NEVER
  point at production `litellm.litellm.svc` (real GPU/spend).
- Mint an ed25519 write-auth keypair (`test/harness` `WriteAuthCreds`, or openssl
  per `docs/spikes/gascity-writeauth-spec.md` sec.4): private key -> a Secret
  (`secrets.gcWriteKey`), public `kid:base64` -> `gascity.writeAuth.verifyKey`.
- `helm install gonk chart/gonk -f chart/values-e2e.yaml` with overrides:
  image tags (use **meter-testclock** tag for the budget tests); `gitlab.url=https://gitlab.orac.local`;
  `intake.webhookPublicURL=http://gonk-intake.<ns>.svc.cluster.local:8080/hook/gitlab`
  (ingress OFF -- GitLab is in-cluster and reaches the internal Service);
  `litellm.externalURL=<throwaway litellm svc>`; `ledger.postgres.mode=external` + DSN;
  `dolt.persistence.storageClass=longhorn` (orac has NO default StorageClass);
  `componentImages.agent.tag=<agent TAG>`; the rung catalog / ladder / synthetic
  prices; the write-auth verifyKey + secret. Satisfy every fail-closed guard
  (G22 needs write-auth). All 4 pods must reach Ready.
- Enable GitLab local webhooks:
  `PUT /application/settings {allow_local_requests_from_web_hooks_and_services:true}`
  (capture the prior value; **restore at teardown**).

## Step 3 - the end-to-end proof (the milestone)

1. Add bot (user 49) as a maintainer of project 75; force intake reconcile
   (`POST /admin/reconcile?wait=true`) -> onboarding MR (already proven at L3).
2. **Merge the onboarding MR** (bot's `.gonk.yml` lands) -> project goes `pending`
   -> scaffold MR -> merge -> `valid`. (Spec 11.2; P2-7.)
3. **File an issue** on project 75 -> webhook -> intake `/decide` (meter must be
   Ready) -> `gonk-dispatch` (signed grant) -> `gonk-triage` -> a **real opencode
   agent pod** spawns, calls the model via the throwaway LiteLLM (stub upstream),
   and posts a **triage comment** on the issue. THIS is the proof.
4. Confirm the money path: the LiteLLM spend row carries the 7 attribution keys;
   meter's ledger matches; the rung was chosen deterministically.

## Step 4 - Task 8 (L3 budget suite), after the e2e proof

`sed -n '2966,3061p' docs/superpowers/plans/2026-07-13-plan-06-e2e-harness.md`.
Reuses everything above. Tests: budget exhaustion blocks cloud rungs -> defer
(P3-7); quiet-hours (issue in quiet window -> intake fires -> meter defers ->
runs when window ends, via meter-testclock; P2-10); **pod-level kills** (SIGKILL
an agent pod mid-session -> no double-spend, no duplicate comment; P2-8). Then
Task 6 (formalize the kill framework -- L2 already did meter-restart-mid-race)
and Task 9 (docs/gates + `make secrets-scan`).

## Teardown (MANDATORY, every run)
Delete the ns; delete project 75's webhooks (`GET/DELETE /projects/75/hooks`);
restore the GitLab `allow_local_requests...` setting; revoke any deploy token you
minted. KEEP the bot (49) and project 75. Verify: `kubectl get ns | grep gonk-e2e`
empty, `GET /projects/75/hooks` count 0.

## Gotchas (learned the hard way)
- Scratchpad does NOT persist across sessions -> re-mint all tokens.
- P3-3 LiteLLM spend-log lag is severe in a single-node rig (best-effort batch
  writer). Budget tests must NOT depend on real spend-log timing -- drive syncs
  deterministically (`POST /admin/spend/sync`, HB-2) and cross time with the
  testclock, never `time.Sleep`.
- The controller crash-loops (does not wait) while Dolt is unreachable during
  boot -- cosmetic once Dolt is up; worth a small fix.
- The in-pod `gonk-sweep` exec order logs `GONK_METER_TOKEN_FILE unset` -- check
  it does not break re-sling dispatch.
- Meter needs a **proxy_admin**-role LiteLLM key (deployment concern).
- Dolt image tag is now `2.1.7` (a nonexistent `v1.43.0` was the default; fixed).

## Open beads (bd)
- `gonk-4nk` (P1): serialize `resolveProject` -- key-provisioning split-brain under
  overlapping tickers.
- `gonk-0qi` (P2): key preserve-branch does not self-heal a deleted k8s Secret.
- `gonk-wgq` (P1): report LiteLLM unbounded `/spend/logs` OOM upstream (owner).
- `gonk-mu9` (P2): GitLab Duo flows research (post-MVP).
- (Closed this session: `gonk-2g4`, `gonk-huy`, `gonk-tff`, `gonk-fsl`, `gonk-yku`,
  `gonk-vsy`, `gonk-5we`.)
