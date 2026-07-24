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

- [x] T1 image: add tmux (entrypoint reused, not a separate configure script);
      CI-built at v0.1.0-e1a3c47129a4.
- [x] T2 chart: wire GC_K8S_* controller env (consume componentImages.agent).
- [x] T3 chart: gc-agent SA pull secret; creds threaded via dispatch order vars
      (litellm_url/key + bot_token) instead of git-credentials seam.
- [x] T4 pack: triage agent start_command=gonk-agent-entrypoint (escape hatch),
      prompt_mode=flag, OPENCODE_PERMISSION allow; dispatch + formula vars.
- [x] T5 deploy to throwaway ns (gonk-e2e-opencode-e1a3c471); onboarding ->
      merge -> file issue. Project 75 is `valid`, webhook 3 delivers, issues #2-#5
      filed, `gonk_intake_dispatched_total{trigger="issue-triage"}` incremented.
- [~] T6 prove the metered opencode session. UNBLOCKED 2026-07-24 (see below):
      Bailey serves again and the whole model path is verified by hand. Remaining:
      the pour itself, which needed blocker 4.
- [ ] T7 teardown; document findings in test/e2e/.

## Recovering this run after a scratchpad wipe (2026-07-24)

The session scratchpad (`e2e-run/`: NS, overrides.yaml, deploy.sh, teardown.sh)
was lost to a `/tmp` clear. NOTHING in the cluster was lost, and the run dir is
reconstructible -- record how, because it will happen again:

- namespace: `gonk-e2e-opencode-e1a3c471` (the only `gonk-*` ns).
- values: `helm get values -n <ns> gonk -o yaml` IS the overrides file, verbatim.
- secrets/keys: all still in-cluster (`gonk-gc-write-key`, `gonk-litellm` key
  `admin-key`, `gonk-gitlab` key `token`, `gonk-meter-api`, `gonk-webhook`);
  nothing needs regenerating and nothing was only ever on local disk.
- GitLab: project 75 = `agentic/gonk-e2e-1784441480`, hook id 3 -> intake, bot 49.

Teardown is therefore: delete the ns, delete hook 3 on project 75, restore the
GitLab local-network-webhook setting. Keep bot 49 and project 75.

## Verified live, by hand, on 2026-07-24

- Bailey/Ollama serves: `qwen3:14b` answered `/api/chat` in 21s, HTTP 200.
- The METERED hop works end to end: LiteLLM `stub-local` -> `ollama_chat/qwen3:14b`
  returned HTTP 200 in 12s WITH a usage block (so a spend row is written and the
  attribution header rides along). The money path is real, not assumed.
- The blocker-4 diagnosis is confirmed by the controller's own log, once a minute:
  `gonk-sweep output: {"level":"ERROR","msg":"misconfiguration","err":"GONK_METER_TOKEN_FILE [redacted] unset"}`.
  Note `[redacted]` -- that is Gas City's secret filter admitting it is the one
  that removed the value.

## Blockers found by running the real path (each hid the next)

The dispatch->session path had NEVER run, so it was a stack of latent bugs; each
fix revealed the next. All fixed this session (commits on the branch):

1. `bead_id` missing from intake's dispatch order vars -> every dispatch 422
   "missing required param(s): bead_id". (pkg/intake/dispatch.go). The L3 smoke
   saw this exact 422 and misread it as success.
2. `GONK_METER_TOKEN_FILE unset` on the controller -> the in-controller gonk-gate
   exec orders (gonk-dispatch/sweep) can't call meter -> exit 2, no pour.
   (chart: meter URL + meter-api mount on the controller).
3. Formula orders (gonk-triage/scaffold/mention) had no `[order.params]` -> the
   supervisor rejects webhook orders with an empty params block and skips them at
   load -> RunOrder(gonk-triage) 404. (pack: add [order.params]).

4. Gas City STRIPS secret-marked env from exec orders. `internal/execenv`'s
   `IsSensitiveKey` flags any key containing TOKEN/SECRET/PASSWORD/API_KEY/... and
   `FilterInherited` drops it before running an exec order -- and gonk-dispatch IS
   an exec order. So `GONK_METER_TOKEN_FILE`, `GONK_LITELLM_KEY` and
   `GONK_BOT_TOKEN` all arrived EMPTY: meter unreachable (exit 2, no pour ever),
   and the session would have had no creds even if it had poured. `[order.env]`
   is NOT the workaround it looks like -- `gc init` drops the entire block when it
   copies the pack into `/city` (verified: `/opt/gonk/pack` has it, `/city` does
   not). FIX: deliver every gonk-gate secret as a MARKER-FREE PATH env pointing at
   a mounted file -- `GONK_METER_BEARER_FILE`, `GONK_LITELLM_KEY_FILE`,
   `GONK_BOT_FILE` (note: not `*_TOKEN_FILE`, which would be stripped too) -- and
   have gonk-gate read the file. Chart mounts the litellm + gitlab secrets on the
   controller; the key material never sits in a controller env var.

5. `envArg` UPPERCASED the param name. Gas City sets `GC_WEBHOOK_ARG_<name>` with
   the `[order.params]` key VERBATIM (`internal/webhookmatch/extract.go`'s
   `ExecEnvVars` is a plain map copy), and every gonk param is lower_snake_case.
   So EVERY dispatch arg read back `""` and gonk-dispatch died on
   `unknown trigger; pouring nothing` with `trigger=""` -- AFTER asking meter,
   i.e. it burned a budget decision and poured nothing. Fixed: verbatim first,
   uppercase kept as a fallback.

6. **Per-session config CANNOT reach a session pod as env.** This one invalidated
   the T3/T4 design, and only reading Gas City proved it:
   - `GC_WEBHOOK_ARG_*` is an EXEC-ORDER overlay only --
     `internal/orderdispatch/dispatch.go`: *"ExecEnv is the environment overlay
     applied when Order is an exec order."* `gonk-triage` is a FORMULA order.
   - A session pod's env is `resolved.Env` (STATIC agent/provider/city config)
     plus a FIXED passthrough allow-list (`internal/processenv/provider.go`:
     PATH, HOME, USER, TZ, CLAUDE_*, locale -- no `GONK_*`) plus Dolt/city path
     projections.

   So the entrypoint's required `GC_WEBHOOK_ARG_{MODEL,METADATA_JSON,...}` could
   never be set and it would have exited 1 at its own guard.

   FIX (owner's call, 2026-07-24): the two PER-SESSION values ride the one
   per-session channel, the PROMPT -- `gonk-triage` stamps
   `<!-- gonk:model:... -->` / `<!-- gonk:meta:... -->`, the entrypoint parses
   them out of its own `--prompt` arg, strips them, and passes the rest to
   opencode. NOTHING SECRET goes in the prompt. The STATIC per-install creds
   (LiteLLM URL/key, bot token) are written into the per-agent `[env]` in /city
   by the bootstrap initContainer from the mounted secrets. This is the shape v2
   keeps: the broker injects context the same way, minus the marker hack.

7. The controller never set `GONK_BEAD_REPO_DIR`, so gonk-gate used an IN-MEMORY
   bead store -- its own warning says "never for the controller". Parked beads
   would never unpark and outcomes would never be reported, silently. Set to
   `/city`.

Deploy-time (not code): the webhook secret must be >= 32 bytes (intake fatals
otherwise); `.agent/` added to project 75 main to reach `valid` deterministically
(the model-gated scaffold agent is itself an opencode session, deferred).

## Bailey GPU note (2026-07-19)

The real-model path points the throwaway LiteLLM's `stub-local` at
`ollama_chat/qwen3:14b` on `ollama.bailey-gpu.svc` (owner's call: use a real
tool-capable model, NOT the 120b nemotron; do not roll back to the stub). Bailey
is currently wedged -- inference calls hang indefinitely though `/api/tags`
answers and the node is idle. Left as-is for the hardware/GitOps teams; the e2e
harness is correct and will complete once Bailey serves inference again.

## CI is currently broken by an infrastructure outage (2026-07-24)

Pipeline 1582 @ 383ada07: `lint`, `test`, `chart-lint`, `no-latest` ALL PASS --
the image jobs fail on the homelab network, not on this code:

```
zot.registry-system.svc:5000/v2/library/golang/manifests/sha256:ae5a...
  -> NAME_UNKNOWN: repository name not known to registry
fallback -> index.docker.io -> dial tcp 34.206.143.55:443: i/o timeout
```

The zot pull-through mirror no longer serves `library/golang` / `library/debian`,
and the cluster's fallback egress to Docker Hub times out; two jobs then sat
retrying to the 1h timeout. Same handling as Bailey: leave it to hardware/GitOps,
do NOT "fix" it by moving pins. CI-build proof for the images is already banked
(pipeline 1555, memory `proof-2026-07-19-gonk-agent-image-built`).

Iteration meanwhile uses a LOCAL build + push, which the owner explicitly allowed
("the build must be RUNNABLE in CI, but you can use local resources"). Pushing
needs the in-cluster path, because the ingress 499s on large layers:

```
kubectl port-forward -n gitlab svc/gitlab-registry 5000:5000
podman login localhost:5000 -u steve --password-stdin   # glab's token works
podman tag  registry.orac.local/agentic/gonk-project/<img>:<tag> localhost:5000/agentic/gonk-project/<img>:<tag>
podman push --tls-verify=false localhost:5000/agentic/gonk-project/<img>:<tag>
```

## Known risks / open checks

- ~~Does the dispatch order deliver model + metadata (+ key) to the session as
  `GC_WEBHOOK_ARG_*` pod env?~~ ANSWERED, AND THE ANSWER WAS NO -- see blocker 6.
  This was the risk that mattered.
- opencode config schema at the pinned OPENCODE_VERSION (overlay key path) — the
  entrypoint comment flags this as needing live verification (Plan 06). This run
  IS that verification.
- Whether `opencode --prompt` (non-interactive) reliably drives `glab` to post a
  single comment against the stub model's output. May need prompt tuning.

## State at 2026-07-24 handoff

Blockers 4-9 fixed this session; agent SESSION PODS NOW SPAWN AND RUN. The chain
dispatch -> meter decide -> pour -> routing -> session -> POD is proven live.

8. `GC_SESSION_PROVIDER` is a CHART INVENTION -- the string exists nowhere in Gas
   City. The runtime silently fell back to tmux: sessions logged
   `outcome=success duration=33ms` while also logging `tmux server unreachable`,
   and NO pod was ever created (zero pod events in the namespace). The real
   override is `GC_SESSION` (cmd/gc `effectiveProviderName`), plus city.toml's
   `[session] provider` (written by the bootstrap; `gc init` has no flag for it).
   With both set, control-dispatcher and triage pods reach Running.
9. The controller's `HOME=/home/gonk` (an emptyDir that exists only in the
   controller pod) rides Gas City's session passthrough allow-list into every
   agent pod, which died on `mkdir: cannot create directory '/home/gonk'`.
   Overridden per-agent to `/tmp/gonk-home` (agent [env] beats the passthrough).

### THE NEXT THING TO DEBUG (exact state)

Triage pods reach Running but are reaped after ~60s and respawned. Inside a live
pod: `tmux ls` -> `no server running on /tmp/tmux-65532/default`, and
`/etc/gonk/overlay/` is EMPTY -- so gonk-agent-entrypoint NEVER RAN. The k8s
provider is supposed to launch `tmux new-session -d -s <s> "$CMD"` in the pod
(internal/runtime/k8s/pod.go). Start there: find what the pod's container is
actually running, and why the provider's tmux launch is not happening (candidate:
GC_K8S_PREBAKED=true skipping a step that also sets up the launch, or the pod
command/args not being what the provider expects).

### SECURITY: ROTATE THE BOT TOKEN

`gc config explain` prints agent `[env]` values UNREDACTED, so the GitLab bot PAT
(bot user 49) was printed in plaintext to a terminal and into a session
transcript. ROTATE IT. This is a direct consequence of the v1 cred-injection
compromise (creds in city agent config) and widens its blast radius: any operator
running `gc config explain` sees the key and the token. Worth reconsidering in v2
(the broker keeps creds out of the pod entirely) and worth a redaction bug
upstream.

## 2026-07-24 (later): opencode PROVEN on the metered path; sessions stable

Merged to main (owner: "let's just develop on main for now"). Two more blockers
fixed, and the harness is proven working:

10. The entrypoint made a missing model FATAL, but Gas City launches agent pods
    as POOL sessions -- `tmux new-session -d -s main "gonk-agent-entrypoint" &&
    sleep infinity`, with NO prompt appended; the prompt arrives later. So the
    entrypoint exited, took the tmux session with it, and the pod was reaped and
    respawned every ~60s. Now: model = marker > GC_WEBHOOK_ARG_MODEL > GONK_MODEL
    (static, injected by the chart from onboarding.defaultRung's model in the
    OPERATOR's catalog -- the pack still names no model). Missing attribution
    metadata warns loudly and OMITS the header rather than sending an empty one.
11. XDG leak. Overriding HOME was not enough: processenv's allow-list also copies
    XDG_CONFIG_HOME from the controller (pointing under /home/gonk, which exists
    only in the controller pod), so opencode -- XDG-aware -- died in Bun on
    `EACCES: permission denied, mkdir '/home/gonk'` WITH the overlay already
    rendered correctly, which reads like a config bug and is not one. All four
    XDG dirs are now pinned under the writable HOME in the agent [env].

### PROVEN, not inferred (standalone probe pod, real entrypoint, real image)

- overlay rendered: `model: gonk/stub-local`, `apiKey: {file:/tmp/gonk/llkey}`,
  attribution header correctly OMITTED when no marker is present.
- prompt markers stripped from argv before opencode saw them.
- opencode ran a FULL loop: created a session, `stream providerID=gonk
  modelID=stub-local`, `process`, `exiting loop`.
- LiteLLM recorded THREE spend rows against `ollama_chat/qwen3:14b` --
  1783 tokens / $0.00044575 and 124 tokens / $0.000031. THE MONEY PATH IS REAL.
- in the REAL triage pod: `tmux ls` -> `main: 1 windows`, opencode resident, and
  `gc session list` shows triage-1/2/3 ACTIVE for 2-5m (no more 60s reaping).

### THE NEXT BLOCKER (exact, reproducible)

EVERY CALLER-SUPPLIED FORMULA VAR RENDERS EMPTY. `bd show` on a triage bead:

    Read issue ! in project `` and triage it.
    Apply labels to the issue, each prefixed gonk::.
    METADATA  city_bead_id:

`label_prefix` renders ("gonk::") because it is the ONE var with a `default`.
Everything gonk-gate passes in the RunOrder body -- project, issue_iid,
bead_anchor, city_bead_id, model, metadata_json -- arrives empty, and the
`<!-- gonk:bead:... -->` marker line is gone with them.

So the order params are not reaching formula var expansion. This ALSO blocks the
marker seam (blocker 6's fix), since {{model}}/{{metadata_json}} render empty.
Start at gcapi's RunOrder body shape vs what the supervisor expects for a
formula order's vars (webhookmatch.MatchResult.Vars -> formula ExpandVars is the
documented channel; ExecEnvVars is the exec-order one and is NOT it).

The agent never gets a usable prompt until this is fixed, which is why no triage
comment has been posted yet even though the harness itself now works.
