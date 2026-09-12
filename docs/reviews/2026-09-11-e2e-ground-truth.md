# E2E ground truth: what actually stands between `main` and a real triage comment

- **Date:** 2026-09-11 (cluster reads taken 2026-09-11 19:10–19:40 PDT / 2026-09-12 02:10–02:40 UTC)
- **Scope:** investigation only. Nothing in the tree, the chart, the cluster or GitLab was
  modified. Every claim below carries its evidence inline.
- **Repo position:** `main` at `61e973e` (docs-only), parent `b7a22ea`. Working tree clean.
- **Deployed:** `v0.1.0-339.b7a22ea5ae37` — i.e. the deployed build IS `main` minus one
  documentation commit.

---

## 0. The headline, before anything else

**`gonk-712`'s milestone as literally worded has already been met. It was met on 2026-09-06
and 2026-09-07, six times, and nobody updated the bead.**

Real triage comments, authored by the real `gonk` bot (GitLab user id 49), produced by a real
opencode agent session, are on the real test project (id 75) right now:

```
$ glab api "projects/75/issues/68/notes"
  author: gonk id 49 | system: False | created: 2026-09-07T02:19:58
  body: 'The `pkg/trace` directory does not contain any files, so it is unclear what
         constitutes a "complete" trace or which field determines the verdict. Please
         provide more context or clarify the expected behavior.\n\n_Reply with `@gonk`
         to send this back to me -- I only see comments that mention me._\n\n
         <!-- gonk:bead:gonk:75:issue:68 -->'
```

Same shape on issues `!64`, `!65`, `!66`, `!67` (2026-09-07) and `!59` (2026-09-06). Issue
`!59`'s comment cites a real file it read out of the granted checkout:

```
'/workspace/internal/paging/paging.go: The function `Slice` does not account for filters
 when calculating the items for a page. ...'
```

That is the whole chain: webhook → intake → meter `/decide` → dispatch → prompt stored →
pod pulled it → opencode ran against LiteLLM → model emitted a fence → sweep parsed it →
broker posted the comment and the labels under the bot PAT. The labels landed too
(`gonk::verdict-reply-only`, `gonk::needs-more-info`, `gonk::code-change`,
`gonk::needs-maintainer`), and gonk's own bead records for those issues carry `gonk::done`:

```
$ curl http://<controller>:9443/v0/city/gonk/beads?limit=200
 {"id":"go-lfkv","title":"gonk:75:issue:68","labels":["gonk-anchor:gonk:75:issue:68","gonk::done"]}
 {"id":"go-p275","title":"gonk:75:issue:65","labels":["gonk-anchor:gonk:75:issue:65","gonk::done"]}
 {"id":"go-3hgb","title":"gonk:75:issue:59","labels":["gonk-anchor:gonk:75:issue:59","gonk::done"]}
```

So the question is **not** "can gonk ever post a triage comment". It is:

1. **Can the CURRENT build do it?** It has never been observed doing it. 164 commits have
   landed since 2026-09-06, ~20 of them in `cmd/gonk-gate`, including deleting the formula
   layer and changing the alias-reservation protocol. Nothing has run a triage since.
2. **Can it do it RELIABLY?** The last observed session on this cluster was *abandoned* by
   the dispatcher because of node I/O pressure (`gonk-cw3`), and that pressure is higher
   right now than it was when that happened.
3. **Can anything but a human notice if it stops working?** No. There is no automated test
   anywhere that crosses the three physical handoffs the comment depends on.

Those three are the real content of this report.

---

## 1. The end-to-end path as the code does it TODAY

### Components and handoffs

| # | Handoff | Where | Automated test that RUNS in CI |
|---|---|---|---|
| H1 | GitLab issue/note webhook → `gonk-intake` public listener `:8080/hook/gitlab` | project 75 hook id 4 | **Yes** — `test/integration/` (no build tag, runs in bare `go test ./...` in both GitLab `test` and GitHub `gate`) |
| H2 | intake → `POST /v0/city/gonk/order/gonk-dispatch/run` on the Gas City controller (signed write grant) | `pkg/intake`, `pkg/ghook` | **Yes** for the intake-side decision; **No** for the real HTTP hop to a real `gc` |
| H3 | `gonk-dispatch.sh` → `gonk-gate dispatch` → meter `/v1/policy/decide` | `/city/scripts/gonk-dispatch.sh` → `cmd/gonk-gate/dispatch.go` | **Yes** — `cmd/gonk-gate/dispatch_test.go` against a hand-rolled `httptest` fake meter |
| H4 | gate resolves the project's LiteLLM virtual key from the keysink Secret; **fails closed** if absent | `cmd/gonk-gate/keyread.go`, `broker_inject.go:855-880` | **Yes** (unit) |
| H5 | gate `PutPrompt` → meter stores prompt row keyed by session alias | `cmd/gonk-gate/meter.go`, `internal/meter/service` | **Both halves tested separately; the hop is NOT** — see §1.2 |
| H6 | gate `CreateSession` (Gas City, async 202) → k8s provider creates the agent pod | `pkg/gcapi`, `broker_inject.go:1011` | **No** — fake `gcapi` only |
| H7 | pod's `entrypoint.sh` GETs `$GONK_PROMPT_URL/v1/prompt/$GC_ALIAS` (one-shot; 410 on re-read) | `images/agent/entrypoint.sh:235-315` | **Partially** — `test/entrypoint/prompt_fetch_retry_test.go` runs the real script but against a **fakebin `curl`**, never a real or httptest meter |
| H8 | gate `awaitPromptFetched` polls meter until `fetched_at` is set, else tears the session down | `broker_inject.go:1046-1090` | **Yes** (unit, fake meter) |
| H9 | entrypoint renders `/etc/gonk/overlay/opencode.json`, asserts `opencode models` lists only `gonk/`, runs `opencode run -- <prompt>` | `entrypoint.sh:425-635` | Provider-allowlist assertion: **Yes**, in `test/images/agent_smoke_test.go` (GitHub `images` job only) |
| H10 | opencode → LiteLLM (per-project virtual key, spend-logs-metadata header) → model | LiteLLM `v1.92.0`, ollama/vLLM on bailey | **Yes** for billing correctness (`test/component`, GitHub only); **No** for a real opencode turn |
| H11 | agent prints `GONK_BATCH_START … GONK_BATCH_END` into the tmux pane and then **holds** (`while :; do sleep 3600; done`) so tmux stays alive for the read | `entrypoint.sh:600-634` | **No** |
| H12 | `gonk-sweep` (30s cooldown) → `gonk-gate sweep` reads the transcript via `gcapi`, extracts the LAST fence, `effects.ParseBatch`, shape/trajectory gates | `cmd/gonk-gate/sweep.go`, `broker_apply.go` | **Yes** (unit + `broker_roundtrip_test.go`) |
| H13 | sweep applies effects: `POST /api/v4/projects/:id/issues/:iid/notes` + labels, under the bot PAT | `pkg/glab/write.go:132,174` | **NO.** `broker_roundtrip_test.go` applies to `recordingApplier`, an in-memory fake. `pkg/glab/client_test.go:333` tests the HTTP shape in isolation. **Nothing joins them.** |

### 1.1 What that means in one sentence

The *decision* logic (gate 1, gate 2, budget, fence parse, effect validation, label
reservation) is densely tested and those tests genuinely run on every push. The three
**physical** hops the comment actually depends on — gate→meter over HTTP, entrypoint's
`curl`→meter, and effects→GitLab's REST API — are each tested only against a fake on the far
end, and none of them is ever stitched together.

### 1.2 The precise gaps (from a full read of CI config and test tree)

- **Gate `PutPrompt` → entrypoint fetch:** `cmd/gonk-gate/broker_prompt_test.go:30,71` PUTs
  to a hand-rolled `httptest` fake, not `internal/meter/service`. The meter's real
  `/v1/prompt/{alias}` handler is unit-tested at `internal/meter/service/http_test.go:469-590`
  with Go's `http.Client`. `test/entrypoint/prompt_fetch_retry_test.go:100` points the real
  script at `GONK_PROMPT_URL=https://prompt.invalid` and answers with a fake `curl` binary.
  **No test connects the writer to the reader.**
- **Fence → GitLab comment:** `grep` for `Apply: gl.Client()` across `cmd/gonk-gate/*_test.go`
  returns nothing. `glabtest` is wired to the sweep's **read** side only (`sweep.go:31` `GL:`).
- **Whole chain:** `test/e2e/` contains exactly one markdown file
  (`L3-real-gitlab-findings.md`) documenting a one-time **manual** install. There is no `.go`
  test and no CI job.
- **`chart-test` never runs on GitLab:** `.gitlab-ci.yml:160` is gated on
  `$GONK_CHART_TOOLS_IMAGE`, which is defined nowhere. The chart gate runs only in GitHub
  Actions (`ci.yml:110`). Single point of failure for the chart seal.
- **`image-scan` and `chart-publish` are `rules: [when: never]`.**
- **`internal/meter/litellm/live_component_test.go`** is `//go:build component` but the
  GitHub `component` job only runs `./test/component/...`, so this package is **never
  invoked at all**. `internal/buildgate`'s `TestEveryBuildTagRunsInCI` is a tag-name-only
  check and does not catch it.

### 1.3 Architecture direction (relevant because it changes what is worth fixing)

`docs/reviews/2026-09-08-delivery-plan.md` line 676 onward: **T-45 decides Kubernetes Jobs
replace Gas City as the session runtime**, behind a `SessionRuntime` interface (T-55), with
the Gas City surface deleted at T-56 and `gonk-gate` becoming its own Deployment at T-57.
T-45 explicitly closes the "upstream-blocked class" `gonk-alw`, `gonk-p2e`, `gonk-6gs`,
`gonk-njz`, `gonk-cw3`. **T-45, T-55, T-56, T-57, T-58 are all still OPEN** (`bd list --status
open --priority 1`). T-21 ("the v1 scenario runs end to end in CI") is the task the plan says
closes `gonk-712`, and it depends on T-56, T-57, T-16, T-18.

So the project's own plan reads `gonk-712` as "e2e in CI", not "one live comment". By that
reading it is a long way off. By the bead's literal wording it is done.

---

## 2. What is deployed right now

```
$ kubectl get pods -n gonk -o wide
NAME                               READY   STATUS    RESTARTS   AGE    NODE
gonk-controller-656c6f65bc-7pvvk   1/1     Running   0          20m    orac04
gonk-dolt-0                        1/1     Running   0          27h    orac04
gonk-intake-647544cbc8-5d545       1/1     Running   0          20m    orac04
gonk-meter-5dc9f7bbc-tbt49         1/1     Running   0          20m    orac04
gonk-netpol-probe                  0/1     Error     0          4d6h   orac04
```

All three service images are `registry.orac.local/agentic/gonk-project/<x>:v0.1.0-339.b7a22ea5ae37`.
`GC_K8S_IMAGE` on the controller points at `gonk-agent:v0.1.0-339.b7a22ea5ae37`.

**All four image tags exist** (asserted on the `name` field, per CLAUDE.md):

```
$ python3 hack/require_image_tags.py v0.1.0-339.b7a22ea5ae37
  PRESENT  gonk-agent / gonk-controller / gonk-intake / gonk-meter
  all present; safe to bump the gitops tag           EXIT=0
```

The agent image is a real multi-arch index (matters because agent pods have no node selector
and `bailey` is arm64):

```
$ GET /v2/agentic/gonk-project/gonk-agent/manifests/v0.1.0-339.b7a22ea5ae37
 mediaType: application/vnd.oci.image.index.v1+json
 manifests: [ {amd64/linux}, {arm64/linux,v8} ]
```

`HelmRelease gonk` is `Ready=True`, `UpgradeSucceeded … gonk.v916 with chart gonk@0.1.7`.

### Config and connectivity the deployed stack needs

| Dependency | State | Evidence |
|---|---|---|
| Meter reachable | **Yes**, `/healthz` 200, `/readyz` 200 | port-forward probe |
| Project 75 registered & active | **Yes** | meter `/v1/projects/agentic%2Fgonk-e2e-1784441480` → `"state":"active"`, ladder `["qwen3-14b","qwen3-6-35b"]`, `key_ref.secret_name=gonk-key-agentic-gonk-e2e-1784441480-a449462a`, `updated_at 2026-09-12T02:11:35Z` |
| Keysink Secret exists | **Yes** | `kubectl get secret -n gonk gonk-key-agentic-gonk-e2e-1784441480-a449462a` (created 2026-09-08) |
| LiteLLM reachable & healthy | **Yes** | `/health/readiness` → `{"status":"healthy","db":"connected"}` |
| Project virtual key present, single, correctly scoped | **Yes** | `/key/list` → exactly 1 key per alias (8 total, no duplicates); e2e key models `["qwen3-14b","qwen3.6-35b-vllm-nothink"]`, `max_budget 25.0`, `spend 0.0`, `blocked None` |
| Rung→model mapping consistent with the key's allowlist | **Yes** | `cm/gonk-operator-config`: `qwen3-14b → qwen3-14b`, `qwen3-6-35b → qwen3.6-35b-vllm-nothink`. Both are in the key's `models`. (The third instance rung `claude-sonnet` is unreachable for this project: its `.gonk.yml` sets `monthly_cost_usd: 0`.) |
| Model backends actually serving | **Yes, hours ago** | `/spend/logs/v2` shows successful `ollama_chat/qwen3:14b` turns at 2026-09-11T23:45Z and `openai/qwen3.6-35b-nvfp4` at 29k tokens/turn at 23:17Z (other tenants' traffic) |
| Bot PAT valid | **Yes, today** | intake log 01:51:35Z `{"msg":"bot identity","username":"gonk","id":49}`; controller's `GONK_BOT_FILE` projects the same Secret `gonk-gitlab` |
| Dolt | **Yes**, `gonk-dolt-0` Running 27h; beads cache reconciling every ~60s | controller log |
| Webhook | **Yes**, `alert_status: "executable"`, last 20 deliveries all HTTP 200 | `glab api projects/75/hooks/4/events` |
| Pack loaded fresh (no `/city` drift) | **Yes** | `/city/*` all mtime `Sep 12 01:52`; `/city/formulas` exists but is **empty**, matching `/opt/gonk/pack` which ships none |

### The one thing that is NOT healthy

```
$ kubectl exec -n gonk gonk-controller-… -- cat /proc/pressure/io
some avg10=92.04 avg60=87.25 avg300=89.02
$ cat /proc/loadavg
62.89 62.15 52.73 40/2260
$ kubectl top node orac04
orac04   3614m   91%   4805Mi   65%
```

Controller log, continuously:

```
supervisor: FS pressure high (some avg60=91.40 > threshold=50.0), skipping tick
supervisor: FS pressure high (some avg60=87.62 > threshold=50.0), forcing tick after 5 skipped ticks
trace: slow_storage_degraded: gonk-1-…-000033 durable
```

All four gonk pods are on `orac04`, which also carries the Longhorn `instance-manager`
(1132m CPU), Vault, MinIO, GitLab webservice spill, and 46 pods total.

**This is `gonk-cw3`, live and worse than when it was measured.** `gonk-cw3` recorded
`some avg60=89.59` on 2026-09-07 when issue `!66`'s first attempt was abandoned. It is
`87.25` now with a load average of 63.

---

## 3. The three suspect beads — verdicts

### 3.1 `gonk-e9m` — "prompts delivered as KEYSTROKES; a bang runs a shell" · **P1 OPEN**

**Verdict: the named mechanism is GONE. The hazard it names has MOVED, not closed. Keep the
bead open; rewrite it.**

**Gone, structurally:**
- `grep -rn "send-keys" cmd/ pkg/ internal/ images/ chart/ hack/ test/` → **no matches.**
- `sanitizeForKeystrokeDelivery` no longer exists as code. Only three comments referencing
  its deletion survive: `cmd/gonk-gate/broker_inject.go:111`, `:855`, `dispatch_test.go:226`.
- The prompt is now **pulled** by the pod: `images/agent/entrypoint.sh:235-247` GETs
  `${GONK_PROMPT_URL}/v1/prompt/${GC_ALIAS}` and reads it out of JSON with `jq -r`.
  `GONK_PROMPT_URL = "http://gonk-meter.gonk.svc:8080"` is in **all three** live agent
  configs (`/city/agents/{triage,mention,scaffold}/agent.toml`).
- It is handed to the model as an **argv element**, never typed:
  `entrypoint.sh:628` — `opencode run -- "${_clean}"`. The `--` is explicit so a leading
  dash is not parsed as a flag. There is no TUI in the live path; `exec opencode "$@"` is a
  no-prompt fallback only.
- `cmd/gonk-gate/broker_inject.go:855` — "There is no `sanitizeForKeystrokeDelivery` call here
  any more, and its deletion is the point rather than a side effect: the prompt is no longer
  TYPED anywhere, so a `!` in an issue body is just text."
- The bead's own 2026-08-11 note already proved this out-of-band with the exact payload that
  used to execute (`Triage GitLab issue !18`, gonk marker line and all): exit 0, zero shell
  executions, well-formed fence.

**So: is there still any path where issue text reaches a shell or a TUI? YES — a different
one, and it is live today.**

The untrusted GitLab issue title and body are still spliced verbatim into the prompt
(`broker_inject.go:319` `buildIssueContext` → `:128` `renderTriagePrompt`, capped by
`capBody`), and the pod runs opencode with:

```
# /city/agents/triage/agent.toml  (live, read out of the running controller)
OPENCODE_PERMISSION = '{"*":"allow"}'
# comment in the same file: "opencode's permission gate: '*' -> allow, so the session can
#  run glab (read the issue, post the comment) without an interactive approval prompt."
```

Every tool, auto-approved, no prompt. So attacker-influenced text still reaches a shell in the
agent pod — via the model's own tool use rather than via a TUI composer. The mitigating facts
are real but they are all *credential* mitigations, not *execution* mitigations:

- no `glab`, no `bd`, no forge CA in the image (`images/Dockerfile.agent:71-76`, `:143-150`)
- `GIT_TERMINAL_PROMPT=0`, `PAGER=cat`, `GIT_PAGER=cat` (`Dockerfile.agent:~175`)
- the prompt explicitly tells the model it holds no credentials

And the layer that was supposed to contain the blast radius **does not exist**:

```
$ kubectl logs -n gonk gonk-netpol-probe
  ok permitted-port  gonk-intake-internal:9090  expected reachable got reached
  !! forbidden-port  gonk-intake:8080           expected blocked   got reached
  !! forbidden-pod   gonk-controller:9443       expected blocked   got reached
  VERDICT: NOT ENFORCED
```

That is `gonk-dku`, measured, and it composes badly with the controller's own startup banner:

```
WARNING: 0.0.0.0 is a non-loopback bind with mutations enabled — the READ plane is UNAUTHENTICATED.
  Anyone who can reach this port can read, with no credential:
    - beads (work items and their payloads) and mail
    - session peeks and full transcripts
    - the event stream, including 202 rig-provisioning progress
```

**Net: an injected agent pod cannot write to GitLab, but it can execute arbitrary shell in its
pod and read every bead, every session transcript and every rig grant in the city, with no
credential.** That is a genuine finding, it is verified today, and it is not what the bead
currently says.

**Recommended disposition:** close the *keystroke* half with the evidence above; re-file the
residual as its own bug ("untrusted issue text reaches an all-permissions tool runner in a pod
with unenforced network isolation and an unauthenticated control-plane read port next door"),
linked to `gonk-dku` and `gonk-7oz`.

### 3.2 `gonk-njz` — "prompt delivery intermittent: 1 of 4 runs" · **P1 OPEN**

**Verdict: superseded. The mechanism it describes no longer exists, and the replacement is
proven to work — by the six comments in §0 and by an explicit dispatcher-side acknowledgement
check. Close it.**

The bead's own root-cause note is the key: the failure was that Gas City **pushed** the
prompt into the TUI after startup and `GC_STARTUP_PROMPT_DELIVERED=1` was set even when the
composer was empty. Both halves of that are gone:

- The push is gone. `broker_inject.go:~880` — "NOTE the create carries **NO Message**. … The
  prompt is delivered by the submit below instead. Do NOT 'restore' Message here once
  upstream (`gonk-drf`) is fixed: that would deliver the prompt twice."
- The self-reported success flag is gone. Dispatch no longer believes its own submit; it waits
  for the **pod's own acknowledgement**, `broker_inject.go:1046`:

  > "This replaced submit-and-correlate-an-event as the evidence that the agent got its work.
  > The difference is what is being believed: the old path believed its own submit, and
  > reported success even when the prompt landed on a splash screen that was not listening…
  > `fetched_at` is the pod's own acknowledgement, recorded by the store when it consumed the
  > row."

  And if it never arrives, the session is **torn down** rather than left idling
  (`abandonSession`, `broker_inject.go:976`), so the queue-blocking "compounding effect" the
  bead names is also closed.
- The prompt row is **one-shot**: a second GET of a consumed alias returns 410, and the
  entrypoint treats 410 as a distinct fatal (`exit 3`, "respawn past the fetch, or theft").
- `gonk-mzd` (the fix this bead depends on) is `in_progress` but its own note records
  "DEPLOYED AND PROVEN WORKING 2026-09-01, v0.1.0-2e536c9c1fac". It is live: `GONK_PROMPT_URL`
  is in the running agent config, and `entrypoint.sh:235` consumes it.

**Caveat that keeps a piece of this alive under a different name:** the *outcome* `njz`
describes (dispatch gives up and the bead loses an attempt) is still reachable — but the cause
is now node I/O pressure delaying pod start past the 40×3s fetch window, not a composer race.
That is `gonk-cw3`, and it is the live one.

### 3.3 `gonk-qfk` — "deliver orac CA to agent session pods" · **P1 OPEN**

**Verdict: definitively obsolete. Close it. Do NOT implement it.**

The bead's own 2026-07-28 note predicted this ("LIKELY SUPERSEDED by the v2 broker … close as
obsoleted … Do NOT deliver the orac CA to agent pods unless a concrete remaining
agent→orac-TLS need is found"). The evidence is now conclusive:

- `images/Dockerfile.agent:143-150`: "No forge CA is trusted here. Under the broker design
  (spec 9/10) the agent pod **never reaches gitlab.orac.local** — the broker reads the issue
  and posts the comment on its behalf — so the orac private CA is NOT mounted or trusted in
  this image (the `/etc/ssl/orac/ca.crt` file this image used to point at via
  `SSL_CERT_FILE`/`GIT_SSL_CAINFO` **was never even mounted**)."
- `Dockerfile.agent:71-76`: **no `glab` and no `bd` in the image at all** (`gonk-0de`).
- `entrypoint.sh:~420`: "NO forge credentials in the agent pod … there is deliberately no
  glab auth here, and the pod holds no `GONK_BOT_TOKEN` / `GITLAB_HOST` / `GITLAB_TOKEN`.
  Its only secret is the LiteLLM virtual key."
- The controller does all forge I/O with its own PAT: `broker_inject.go:319` reads the issue,
  `pkg/glab/write.go:132` posts the note. The controller mounts `orac-ca` and
  `secret-gitlab-bot → Secret gonk-gitlab`, and that credential resolved as bot user 49 today.
- **The decisive evidence is the comments themselves.** Six triage comments landed while the
  agent pod had no CA and no forge credential. If `qfk` were live, none of them could exist.

The bead's warning stands: delivering the CA now would re-widen a trust surface the broker
deliberately shrank — and given §3.1 (all-permissions tool runner, unenforced NetworkPolicy)
it would be materially worse than when the bead was written.

---

## 4. The P0s

### 4.1 `gonk-alw` — cold controller accepts a session and never creates the pod

**Live hazard on the current build: YES, partially mitigated. It would fire during an e2e
attempt only if dispatch runs within ~40s of a controller restart — which is exactly the
window a redeploy-then-file-an-issue e2e run creates.**

Of the three things the bead asked for:

| Ask | State | Evidence |
|---|---|---|
| (1) find out whether gc's k8s provider fails silently | **Not done** | no upstream work in-tree; superseded by T-45 |
| (2) dispatch must refuse to sling until the provider can create pods | **Not done** | `awaitCreate` (`broker_inject.go:1011-1044`) still **discards the error and returns `(false, nil)` at DEBUG** — the exact defect the bead's own 2026-09-08 note names. It is now a *deliberate* design (a create-success event legitimately lags up to 120s), but the consequence is unchanged: UNKNOWN and NOT-YET-CONFIRMED remain indistinguishable, and an attempt is still burned. |
| (3) distinguish "no pod was ever created" from "a pod ran and did not fetch" | **DONE** | `describeSessionRuntime` (`broker_inject.go:1091`) now asks Gas City what it believes and emits `"NO AGENT EVER RAN -- session state %q, not running, no output; the session was accepted but nothing started it"` |

The bead's own note says its plan is **superseded in direction** by T-45 (Jobs replace Gas
City) and must not be implemented as written. What survives as a requirement on the
replacement — "an unconfirmed create must never be treated as a silent success; UNKNOWN with
no agent started must park rather than consume an attempt" — is **not** satisfied today.

Contributing, and unchanged: **all three controller probes are `tcpSocket` on the supervisor
port** (`gonk-uxup`), verified live:

```
livenessProbe  {"tcpSocket":{"port":"supervisor"},"initialDelaySeconds":15,…}
readinessProbe {"tcpSocket":{"port":"supervisor"},"initialDelaySeconds":10,…}
startupProbe   {"tcpSocket":{"port":"supervisor"},"failureThreshold":30,…}
```

So the pod is Ready while `gc` is still in `startup-orders` — measured **32.4 s** in today's
log (`gc start: startup phase=startup-orders elapsed=32.394s`, total `startup ready
elapsed=36.724s`). A dispatch arriving in that window is accepted and may create nothing. The
bead correctly notes the probe window alone never explained the bug; but 36.7s of "Ready but
not ready" is a wider door than the ~2.3s it was measured at.

**Also visible every restart, and worth one line:** the controller's warm-up doctor check has
failed on *every* city start since at least 2026-09-08 —

```
go-wisp-0r7hk "gonk:meter-reachable alert during city warm-up" (2026-09-12T01:53Z)
go-wisp-ekc9gx (2026-09-10), go-wisp-ppfxdo (2026-09-09), go-wisp-g610yu, go-wisp-qhg2gp, …
"✗ city — gonk:meter-reachable: gonk-meter at http://gonk-meter:8080 did not answer /healthz"
```

The meter answers `/healthz` 200 now. This is the same cold-start ordering: the controller
comes up before the meter. It is currently noise, but it is noise that would mask a real
meter outage.

### 4.2 `gonk-bvy` — two meters race, duplicate LiteLLM keys, project wedges permanently

**Live hazard: YES for the wedge, and the more likely trigger is `gonk-4nk` (single-process),
not the two-pod case the bead describes. It is NOT currently firing — no duplicates exist
today — and it would fire during an e2e attempt only if a key-provisioning race happened to
occur.**

State right now — **no duplicates**:

```
$ GET /key/list?return_full_object=true&size=100   (8 keys total, 1 per alias)
   gonk-agentic-gonk-e2e-1784441480 -> 1   token 8507264aa3cb  created 2026-09-08T06:36:35Z
   gonk-homelab-talos-toolkit       -> 1
   gonk-meter-admin, opencode, pi, aether-scene-writer, OpenClaw-Agent, (null)
```

**Prevention: still absent, and now demonstrably so.** `EnsureKey`
(`internal/meter/litellm/admin_http.go:160`) POSTs `/key/generate` and only falls back to
`updateByAlias` on a 4xx whose body matches `alreadyExists` (`:153`, itself flagged as a
"deliberately loose … unverified against a live proxy" heuristic). That `alreadyExists` branch
has been there since the **original** commit `89f26bb`
(`git log -S alreadyExists -- internal/meter/litellm/admin_http.go`), i.e. it was already in
place on 2026-09-02 when two keys were created **in the same second and both succeeded**.
Therefore LiteLLM does not enforce alias uniqueness atomically on `/key/generate`, and the
current prevention strategy is proven insufficient by the incident itself.

**Recovery: still terminal.** `resolveTokenIDByAlias` (`admin_http.go:222`) ends:

```go
if len(lr.Keys) > 1 {
    return "", fmt.Errorf("litellm: /key/list: %d keys share alias, refusing to guess which to update", len(lr.Keys))
}
```

Nothing reconciles a duplicate away. The project stays `key-missing`, `/decide` defers with
`virtual-key-missing`, and intake answers every webhook 200 and drops it. The only exit is a
human deleting a key.

**What has changed for the better:** the meter Deployment is `strategy: Recreate, replicas: 1`
(verified live), so a *normal rollout* never runs two meters. The bead's trigger required a
node going NotReady plus a force-delete.

**What is worse than the bead says:** `gonk-4nk` gets there **within one process, with no node
failure**. `resolveProject` — which calls `EnsureKey` at `service.go:427` — is reached from
two unsynchronised places: `Register` (`service.go:262`, driven by intake, which re-registers
on a loop) and the `Reresolve` ticker (`service.go:1312`). The service's only per-project lock
is `keyedMutex` (`service.go:218`), and its own doc comment scopes it to `Decide`:
"keyedMutex serializes **Decide** per project WITHIN one process … contention control, NOT the
safety mechanism". `resolveProject` takes no lock at all. Both paths are demonstrably active
right now — the meter registration and the LiteLLM key were both touched within one second at
`2026-09-12T02:11:35Z` / `02:11:36.318Z` by `gonk-meter`.

`gonk-bvy` is blocked on `gonk-fvzl` and `gonk-xmp6` and its v4 plan is a substantial piece of
work (LiteLLM teams, stored key hashes, group-tier orgs). **The recovery half is much smaller
than the prevention half and would convert an outage into an incident: nothing today turns a
duplicate back into a working project.**

---

## 5. The test project

**Both exist and the bot still has access.**

```
$ glab api projects/75
 {"id":75,"path_with_namespace":"agentic/gonk-e2e-1784441480","default_branch":"main",
  "visibility":"private","last_activity_at":"2026-09-07T05:25:43.076-05:00"}

$ glab api users/49
 {"id":49,"username":"gonk","name":"gonk","state":"active","locked":false}

$ glab api projects/75/members/all
 2  steve  access_level 50
 49 gonk   access_level 40          <- Maintainer; can comment and label
```

- **68 issues**, most recent `!68` (2026-09-07). Nothing has been filed since.
- **Webhook id 4** → `http://gonk-intake.gonk.svc.cluster.local:8080/hook/gitlab?gen=1`,
  `issues_events: true`, `note_events: true`, `alert_status: "executable"`,
  `disabled_until: null`. Last 20 deliveries all **HTTP 200** (responses `accepted` /
  `bot_authored`).
- **`.gonk.yml` on `main` is valid and enabled**: `enabled: true`, `actions.triage: true`,
  `ladder: [qwen3-14b, qwen3-6-35b]`, `monthly_cost_usd: 0` (so the `claude-sonnet` rung is
  correctly unreachable here), `per_task_tokens: 2M`, `respond_to_mentions: true`.
- The project is **not** on intake's blocklist (`blocklist active projects:
  ["agentic/gonk-project"]` — only gonk's own repo).

**Nothing about the test fixture blocks an attempt.**

---

## 6. The real blocker list

Ordered by what must be fixed first. Each carries: what breaks · evidence it is broken TODAY ·
class of fix.

### BLOCKS a triage comment appearing at all

**B1. Nothing — on the evidence available.** Every precondition I could check is green: images
exist and are multi-arch, the pack loads, the project is registered and active, its virtual key
is single and correctly scoped, LiteLLM is healthy and both model backends served traffic
hours ago, the bot PAT authenticated today, the webhook is executable and its last 20
deliveries were 200s, and the same code shape posted six comments five days ago.

**The honest qualifier: the current build has never been observed doing it.** 164 commits have
landed since the last successful run, ~20 in `cmd/gonk-gate`, including
`fc7d619 feat(gate): delete the formula layer (ADR-007 §3)`,
`b1f3c81 reserve the session alias before the delivery window, not after`,
`0df81f1 release a pending-prompt reservation to its PRIOR record`,
`3deae25 reaper matches broker session aliases with nonce`,
`d3ec333 refuse comment effects carrying quick actions, JSON note bodies`,
`4e8432f make verdict-label reservation structurally impossible to drift`, and
`850d362 give the transcript read its own 4 MiB cap`. Several of those change exactly the
handoffs §1 shows have no cross-process test. **This is a risk, not a known break, and it must
be reported as such.** The cheapest way to resolve it is to file one issue on project 75 and
watch — not to reason about the diff.

### MAKES IT UNRELIABLE

**B2. `gonk-cw3` — orac04 I/O saturation abandons sessions that were about to work.**
*(infra/cluster fix; highest-probability cause of a failed attempt today)*

- Breaks: pod start exceeds dispatch's 40 × 3 s fetch window (`defaultSubmitAttempts = 40`,
  `submitBackoffInterval = 3s`, `defaultSubmitDeadline = 240s` — `cmd/gonk-gate/dispatch.go:114-121`),
  dispatch tears the session down, the attempt is burned and the bead re-slings or escalates.
- Evidence today: `/proc/pressure/io some avg60=87.25`, `loadavg 62.89`, `kubectl top node
  orac04 → 91% CPU`, and the controller logging `FS pressure high (avg60=91.40 > threshold=50.0),
  skipping tick` continuously. `gonk-cw3` recorded `avg60=89.59` when it abandoned issue `!66`.
  All four gonk pods plus Longhorn's instance-manager, Vault and MinIO share this node.
- Fix class: **cluster/infra** (move gonk off orac04, or relieve Longhorn, or both). A code
  workaround — widening the deadline — would only hide it, and `gonk-cw3` says so.

**B3. `gonk-alw` — an unconfirmed session create still burns an attempt.**
*(code fix in gonk; direction owned by upstream-replacement T-45/T-55)*

- Breaks: dispatch within ~40 s of a controller start can be accepted with no pod created.
- Evidence today: `awaitCreate` still `return false, nil` on error (`broker_inject.go:1030-1038`);
  all three probes `tcpSocket` with `gc` taking 36.7 s to reach ready (controller log).
  Diagnosis message is now correct (item 3 done), the behaviour is not (items 1 and 2 open).
- Fires during an e2e attempt if the run is "redeploy, then immediately file an issue". Easily
  avoided operationally by waiting ~60 s after a controller restart.

**B4. No test anywhere crosses the three physical handoffs.**
*(code fix — test infrastructure)*

- Breaks: a regression in gate→meter, entrypoint→meter, or effects→GitLab ships green.
- Evidence: §1.2. `test/e2e/` contains only markdown. `Apply: gl.Client()` appears in no test.
  `broker_roundtrip_test.go` applies to `recordingApplier`. The entrypoint's prompt fetch is
  tested against a fakebin `curl`.
- This is the substance of T-21, which the delivery plan says closes `gonk-712`.

**B5. `chart-test` never runs on GitLab; `image-scan` and `chart-publish` are `when: never`.**
*(config fix, small)*

- Breaks: the chart seal (`gonk-sjb`) has exactly one CI home (GitHub `ci.yml:110`). If GitHub
  Actions is unavailable for a repo state, a chart edit that skips the version bump ships and
  Flux never deploys it.
- Evidence: `.gitlab-ci.yml:160` `rules: [if: $GONK_CHART_TOOLS_IMAGE]`, a variable defined
  nowhere (the file's own comment says so). `.gitlab-ci.yml:556,571` `rules: [when: never]`.
- Per CLAUDE.md's red-gate rule these are gates that cannot go red, which is the same problem
  wearing a different hat.

**B6. `internal/meter/litellm`'s own `component` suite is never invoked.**
*(config fix, small)*

- GitHub's `component` job runs only `./test/component/...`.
  `internal/buildgate`'s `TestEveryBuildTagRunsInCI` is tag-name-only and does not catch it.

**B7. `gonk-bvy` / `gonk-4nk` — a key-provisioning race wedges the project with no way out.**
*(code fix in gonk; prevention is large, recovery is small)*

- Breaks: two concurrent `EnsureKey` calls create two keys with one alias; every later
  `EnsureKey` then fails terminally, the project sits in `key-missing`, `/decide` defers
  forever, intake 200s and drops every webhook.
- Evidence today: no duplicates exist, so it is not currently firing. But the refusal at
  `admin_http.go:242` is unchanged; `alreadyExists` predates the incident that it failed to
  prevent (`git log -S`); and `resolveProject` (`service.go:350`, `EnsureKey` at `:427`) is
  called unserialised from both `Register` (`:262`) and `Reresolve` (`:1312`), with the only
  per-project lock scoped to `Decide` by its own doc comment (`service.go:204-218`).
- Meter is `Recreate`/`replicas:1`, so the two-pod trigger needs an abnormal event; the
  single-process trigger (`gonk-4nk`) does not.

**B8. `gonk-w41` is closed in substance but its dependency is not.** The 35 B rung now points
at `qwen3.6-35b-vllm-nothink` (verified in `cm/gonk-operator-config` and in the project key's
model allowlist), which the bead's own note measured at ~70 s/turn vs ~16 min on Ollama. The
first rung `qwen3-14b` is what an e2e attempt will use and it produced a well-formed fence in
32.6 s. Not a blocker.

### MAKES IT UNSAFE

**U1. Untrusted issue text reaches an all-permissions tool runner in a pod with no enforced
network isolation, next to an unauthenticated control-plane read port.**
*(mixed: chart/config for the permission scope, cluster/infra for the CNI, code for the read
plane)*

- `OPENCODE_PERMISSION = '{"*":"allow"}'` in all three live agent configs.
- `gonk-dku`: `kubectl logs gonk-netpol-probe` → `VERDICT: NOT ENFORCED`; both denied
  destinations reachable, including `gonk-controller:9443`.
- Controller startup banner: the read plane is unauthenticated and exposes beads, mail,
  **full session transcripts** and the event stream including rig-provisioning grants.
- The agent *cannot* write to GitLab (no credential, no `glab`, no CA). It *can* execute
  shell and read the city.
- This is the live residue of `gonk-e9m` and it is not what that bead currently describes.

**U2. `gonk-7oz` is understated.** It says the agent NetworkPolicy allows any port to the
gitlab namespace. Given `gonk-dku`, **no** policy restricts the agent to anything. The bead's
note says the netpol landed 2026-08-11; the enforcement did not.

**U3. The one-shot prompt row is the right shape and is working.** `GC_ALIAS` is a 128-bit
capability; the entrypoint deliberately never logs the URL, logs only a 6-char alias prefix,
and a second GET returns 410 (`gonk-mzd`'s T6 acceptance, half met — confirmed by hand). Noted
as a *positive* so it is not accidentally regressed.

---

## 7. Bead hygiene this investigation implies

| Bead | Current | Should be |
|---|---|---|
| `gonk-712` | OPEN P1, "never run a full triage" | **Factually wrong.** Six real comments, 2026-09-06/07. Either close with that evidence and let T-21 carry "e2e in CI", or restate it as "the CURRENT build has never been observed producing one". |
| `gonk-e9m` | OPEN P1 (keystrokes) | Close the keystroke half; re-file the residual hazard (§3.1 / U1). |
| `gonk-njz` | OPEN P1 (TUI delivery race) | **Close.** Mechanism gone, replacement proven, acknowledgement check added. |
| `gonk-qfk` | OPEN P1 (deliver orac CA) | **Close as obsolete.** Its own note predicted this; §3.3 confirms it. Implementing it would be actively harmful. |
| `gonk-mzd` | IN_PROGRESS P1 | Deployed and working since 2026-09-01; live in the running agent config. Close or reduce. |
| `gonk-01hd` | OPEN P1 (route-scoped meter credential) | Appears **done in production**: `gonk-litellm/admin-key` is now a scoped key restricted to `['/key/generate','/key/update','/key/list','/key/delete','/spend/logs/v2']` — verified by a 403 on `/model/info`. Confirm against the bead's acceptance and close. |
| `gonk-alw` | OPEN P0 | Item 3 done; items 1–2 open and superseded in direction by T-45. Record the partial. |
| `gonk-w41` | OPEN P1 | Substantively resolved by the vLLM repoint (bead's own note). Re-check. |

---

## 8. What I did NOT check

Stated plainly, because an unchecked thing reported as checked is the failure mode this repo
keeps paying for.

1. **I did not run a triage.** No issue was filed on project 75, no dispatch was fired, no
   agent pod was created. Everything in §6 B1 about the current build is therefore *inference
   from static and configuration evidence*, not observation. **The single highest-value next
   action is to file one issue on project 75 and watch the chain.** Nothing else in this report
   substitutes for it.
2. **I did not make a model call.** The evidence that qwen3-14b and qwen3.6-35b-nvfp4 are
   serving is other tenants' spend rows (23:17Z and 23:45Z on 2026-09-11), not a request of
   mine. The project's own key shows `spend 0.0` and has **never been used** — it was created
   2026-09-08, after the last successful triage — so its ability to authenticate against
   LiteLLM is **unverified**.
3. **I did not run the test suite.** The CI map in §1 is from reading `.gitlab-ci.yml`,
   `.github/workflows/*`, the `Makefile` and the test tree. I did not execute `make gate`,
   `go test ./...`, or any tagged suite, and I did not confirm that the green pipelines 2502/2503
   on `main` in fact ran the jobs I believe they ran (I read the pipeline list, not the job list).
4. **I did not verify the agent image boots.** The manifest is a real multi-arch index and the
   tag exists, but no pod has run it. Whether `opencode`, `tmux`, `jq` and `curl` behave in
   `v0.1.0-339.b7a22ea5ae37` is untested since `bd00cac refactor(images): drop gonk-gate from
   the agent image`.
5. **I did not read Gas City's source.** Claims about `gc`'s async-create semantics, the k8s
   provider, `internal/runtime/carrier.go`'s `send-keys`, or `resolved.Env` are taken from
   gonk's own comments and from `pkg/gcapi`'s documented live probes, not from upstream at
   `GASCITY_REF`. In particular, I could not independently confirm that the Gas City
   startup-prompt push is *gone* rather than merely *unused* — gonk no longer supplies a
   `Message` on create, which makes it unreachable from gonk's side, and that is the extent of
   what I verified.
6. **I did not verify the write-grant signing path.** Intake mounts `secret-gc-write-key` and
   the controller announces grant-gated mutations with `GC_CITY_WRITE_CID` empty; I did not
   exercise a signed mutation.
7. **I did not test the webhook end of the wire.** GitLab's delivery log shows 200s up to
   2026-09-07; I did not send one, and I did not confirm that GitLab can still resolve
   `gonk-intake.gonk.svc.cluster.local` today.
8. **I did not check the `homelab/talos-toolkit` project**, which is also registered with the
   meter and holds a second virtual key. Any statement here about "the" project refers to 75.
9. **I did not check gitops.** Whether the deployed tag matches what the gitops repo pins, and
   whether Flux would revert a manual change, is unexamined.
10. **I did not read `PLAN.md` (61 KB) or the two other review documents in full** — only the
    sections I grepped for (`T-21`, `T-45`, `T-55`, `T-56`, `gonk-712`, `end-to-end`). There may
    be decisions in them that bear on §6.
11. **The `gonk-netpol-probe` evidence is 4 days old** (pod started 2026-09-07T12:24). I did not
    re-run it. The CNI is still `kube-flannel` (`kubectl get pods -A`), and no policy controller
    appeared in the pod list, so I have no reason to believe it changed — but I did not prove it
    today.
