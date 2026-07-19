# Minimal opencode leg — finishing the v1 agent-session path

Status: IN PROGRESS (started 2026-07-19). Owner directive: "build a minimal
version of the opencode leg of the implementation in the spec, setting aside the
broker stuff, so that we are on a relatively clear upgrade path to what's in the
next-gen spec."

Bead: `gonk-aql` (the umbrella gap). This doc is the working plan; update
progress here as tasks land.

## Why this exists

The dispatch -> agent-pod -> triage-comment path was NEVER wired. Reading Gas
City's k8s session provider (`internal/runtime/k8s/{provider,pod}.go`,
`internal/config/{config,provider,launch_command}.go`,
`internal/worker/builtin/profiles.go`) at the pinned `GASCITY_REF` against the
gonk chart/pack/image found five concrete gaps (see gonk-aql). This plan closes
them minimally.

## Gas City facts (pinned REF) that constrain the design

- The k8s provider spawns ONE image from `GC_K8S_IMAGE` (required for Start);
  namespace from `GC_K8S_NAMESPACE`, pod SA from `GC_K8S_SERVICE_ACCOUNT`,
  `GC_K8S_PREBAKED=true` skips staging. None are set by the gonk chart today.
- The provider OVERRIDES the image ENTRYPOINT: it runs
  `tmux new-session -d -s <s> "$CMD"` where `$CMD` is the resolved agent command.
  So the agent image needs `tmux`, and gonk's `entrypoint.sh` is NOT the process
  unless it IS the agent command.
- The agent command comes from a PROVIDER preset (`profiles.go`). There is a
  builtin `opencode` provider: `command=opencode`, `PromptMode=flag`,
  `PromptFlag=--prompt`, `Env={OPENCODE_PERMISSION:{"*":"allow"}}`.
- Per-agent config (`internal/config` Agent struct) supports: `provider`,
  `start_command` (override provider command), `pre_start` (shell commands run
  IN THE POD before the agent starts), `env` (literal env), `prompt_template`.
- Session pod env = the session's `cfg.Env` (agent.env + order webhook args
  `GC_WEBHOOK_ARG_*` + inherited) minus a controller-only skip list. Plus ONE
  optional secret seam: k8s Secret `git-credentials` key `token` -> pod env
  `GITHUB_TOKEN`. There is NO general secret-VOLUME mount for session pods.

## Scope (settled 2026-07-19): the v2 opencode leg, minus broker

"Build the opencode leg of the implementation IN THE SPEC, setting aside the
broker" = the v2 data flow (spec 4.3) with the broker STUBBED:

- BUILD: the agent SESSION runs opencode against injected context, calls the
  model through the metered LiteLLM path, and PRODUCES the triage output
  (comment body + labels) as its result.
- STUB (broker's future job): context-injection, effect-shape validation, and
  APPLY (posting the comment). For this proof, the produced output is applied by
  a minimal stand-in step, not by the agent.
- CONSEQUENCE (clean upgrade path): the pod holds NO GitLab bot token (v2
  removes creds from the pod anyway). The ONLY cred in the pod is the LiteLLM
  key to call the model. Attribution rides in the request HEADER (per-session,
  from the order metadata), so a single shared virtual key still attributes
  correctly on the spend row.

This sidesteps the cred crux (gonk-dispatch passes the LiteLLM key as a secret
REF that Gas City's provider cannot mount): with only one cred needed, it fits
Gas City's single native secret seam (`git-credentials` key `token` ->
`GITHUB_TOKEN`), repurposed to carry the LiteLLM virtual key. No Gas City fork,
no gonk-gate change for this proof.

## Design (minimal, no Gas City fork)

1. **Image** (`images/Dockerfile.agent`): add `tmux` (provider launch needs it).
   Extract the overlay-render logic from `entrypoint.sh` into a standalone
   `gonk-agent-configure` script the pod can call from `pre_start` (ENTRYPOINT is
   bypassed by the provider). Keep `entrypoint.sh` for any non-gascity use.
2. **Provider/agent** (pack `agents/triage/agent.toml`): `provider = "opencode"`;
   `pre_start` runs `gonk-agent-configure` (renders `opencode.json`: LiteLLM base
   URL, the meter-chosen model, the attribution header, the virtual key) and sets
   up `glab` auth from `GITHUB_TOKEN` (bridge `GITLAB_TOKEN=$GITHUB_TOKEN`). The
   rendered prompt arrives via `--prompt` (opencode PromptFlag). opencode uses
   `glab` (a tool, permission "*"=allow) to read the issue and post the comment.
3. **Chart controller env** (`gonk.controllerEnv` / controller container): set
   `GC_K8S_IMAGE` from `componentImages.agent` (finally consuming the dead
   config), `GC_K8S_NAMESPACE={{.Release.Namespace}}`,
   `GC_K8S_SERVICE_ACCOUNT=gc-agent`, `GC_K8S_PREBAKED=true`, plus the LiteLLM URL
   the session needs (as env that passes through to the pod).
4. **SA / secrets**: attach the registry pull secret to the `gc-agent` SA (the
   provider sets no imagePullSecrets on the session pod). Provide the bot token as
   the `git-credentials` Secret (key `token`).
5. **Cred compromise (v1-only, explicit):** the LiteLLM virtual key reaches the
   pod as env (order arg / configure-script writes it to a file at runtime), NOT
   a mounted file. This is the one place the minimal leg diverges from the
   "key is always a file mount" principle; v2's broker removes it by holding all
   creds out of the pod.

## Upgrade path to v2 (why this is not throwaway)

- The harness image stays THIN (opencode + tools); prompt/model/context are
  injected as data (spec 4.1). v2 adds the broker in front; the image is reused.
- The rung already carries the model; v2 adds `rung.Harness` — additive.
- v2 swaps direct-post (opencode -> glab) for propose-effects + broker-apply. The
  agent-session substrate built here is exactly what the broker drives.

## Tasks

- [ ] T1 image: add tmux; add `gonk-agent-configure`; rebuild via CI.
- [ ] T2 chart: wire GC_K8S_* controller env (consume componentImages.agent).
- [ ] T3 chart: gc-agent SA pull secret; git-credentials + litellm key plumbing.
- [ ] T4 pack: triage agent provider=opencode + pre_start + prompt wiring.
- [ ] T5 deploy to throwaway ns; drive onboarding -> merge -> file issue.
- [ ] T6 prove: a real opencode session runs, meters against stub LiteLLM, and a
      triage comment lands. Verify the 7 attribution atags on the spend row.
- [ ] T7 teardown; document findings in test/e2e/.

## Known risks / open checks

- Does the dispatch order deliver model + metadata (+ key) to the session as
  `GC_WEBHOOK_ARG_*` pod env? Verify against gonk-dispatch order args.
- opencode config schema at the pinned OPENCODE_VERSION (overlay key path) — the
  entrypoint comment flags this as needing live verification (Plan 06). This run
  IS that verification.
- Whether `opencode --prompt` (non-interactive) reliably drives `glab` to post a
  single comment against the stub model's output. May need prompt tuning.
