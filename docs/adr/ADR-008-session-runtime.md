# ADR-008: the session runtime is a Kubernetes Job

Status: **proposed** 2026-09-23 (T-45 spike). The decision is the owner's to
confirm; this document exists to make it decidable on evidence.

gonk runs one agent session per unit of work. Today that session is a Gas City
session: gonk-gate calls a Gas City controller over HTTP, which pours a bead,
ticks a reconciler, and eventually asks its Kubernetes provider for a pod. This
ADR proposes replacing that with gonk-gate creating a Kubernetes `Job` directly.

It cross-references ADR-005 (pack and images), ADR-006 (chart seams), and
ADR-007 §3 (the formula path is deprecated), whose decision this one completes.

Numbering note, following ADR-006 and ADR-007: this is **ADR-008**. Go by
`ls docs/adr/`, never by a plan's guess at its own number.

Gas City was read at the pinned `GASCITY_REF` from `images/versions.env`,
`4fda5a28445f42d6e789fc7f5751645ac4fecd19`; the working tree at
`/root/src/gascity` was confirmed clean at exactly that SHA before any citation
below was taken. All Gas City line numbers are at that commit.

---

## 1. Decision

**Create one Kubernetes `Job` per session, from gonk-gate, using the existing
agent image.** Prompt, checkout tarball and LiteLLM virtual key are mounted as
`Secret` volumes. `activeDeadlineSeconds` is the reservation TTL.
`backoffLimit: 0` — a session is never silently retried, because a retry spends
budget twice and gonk's reservation is already consumed. `ttlSecondsAfterFinished`
bounds the corpse. Completion is read from `Job.status`; the proposed-effects
batch is read from the pod's logs.

The default is Jobs. Gas City had to make its case, and §2 records honestly that
its strongest argument survives — just not in a form gonk can reach.

---

## 2. The premise, and the finding that contradicts it

T-45 was written on a stated premise: *"Gas City's model is refinery workers
converging on beads with `[steps.check]` as the judgment, so acceptance is
model-mediated,"* against gonk's invariant of deterministic judgment before and
after dispatch with no LLM judges.

**That premise is false, and this section says so plainly.** It is the stated
reversal condition's first conjunct, and it is a finding, not a failure.

### 2(a) Can Gas City's acceptance path host a non-model pre-dispatch gate, and a non-model post-dispatch pre-merge gate, without bypassing its own convergence?

**Yes.** Gas City's acceptance decision is *already* a subprocess exit code, not
a model call.

The convergence gate has three modes — `manual`, `condition`, `hybrid`
(`internal/convergence/metadata.go:66-70`) — and acceptance is a single switch
on the gate outcome, where only `GatePass` reaches `TerminalApproved`
(`internal/convergence/handler.go:355-368`). `GatePass` is produced in exactly
one place, and only by a subprocess exiting zero
(`internal/convergence/condition.go:394-399`, verified directly for this ADR):

```go
	// Successful exit (code 0).
	code := 0
	return GateResult{
		Outcome:   GatePass,
		ExitCode:  &code,
```

Non-zero exit is `GateFail` (`condition.go:374-379`), a per-script deadline is
`GateTimeout` (`condition.go:362-368`), and an unrunnable script is `GateError`
(`condition.go:385-390`). **None of those can reach `TerminalApproved`**, so the
gate is fail-closed: a gate that cannot run never approves. The gate program
itself is `exec.CommandContext(execCtx, scriptPath)`
(`internal/convergence/condition.go:325`).

`[steps.check]` — named in the premise as "the judgment" — is not a model turn
and *cannot be made into one*. `RalphCheckSpec.Mode` accepts the single value
`exec`, rejected at validation (`internal/formula/types.go:1486-1492`, verified
directly):

```go
		} else if spec.Check.Mode != "exec" {
			*errs = append(*errs, fmt.Sprintf("%s.check.check: unsupported mode %q (only exec is supported)", prefix, spec.Check.Mode))
		}
```

and refused again at runtime (`internal/dispatch/ralph.go:39-41`). It executes
through the same convergence exec core (`internal/dispatch/ralph.go:325-333`).

The model-verdict channel exists but is **advisory, fail-closed, and inert**:
hybrid mode passes the verdict only as an environment variable and the script
still decides (`internal/convergence/hybrid.go:14-21`); an absent or unknown
verdict normalizes to `block` (`internal/convergence/metadata.go:118-138`); and
nothing anywhere in the tree writes `convergence.agent_verdict` outside
`internal/convergence` itself (verified: every hit of that string under
`--include=*.go` is inside that one package). `ValidateForConvergence`, which
would require an evaluate prompt, has no non-test callers, and the default
evaluate prompt file does not exist in the tree.

Deterministic veto points, all blocking:

| Need | Mechanism | Blocking |
|---|---|---|
| Pre-dispatch (may this run?) | convergence `--trigger event --trigger-condition <prog>`; non-zero holds in `waiting_trigger`, and it gates iteration 1 | Yes (`internal/convergence/trigger.go:93-99`, `create.go:147-152`) |
| Pre-dispatch, per order | order `trigger = "condition"`, `check = "<prog>"` | Yes (`internal/orders/triggers.go:285-320`; refused at `cmd/gc/order_dispatch.go:597-602`) |
| Pre-dispatch, per bead | agent `sling_query` wrapped in a program | Yes — non-zero aborts the sling (`internal/sling/sling_core.go:536-543`) |
| Pre-start, per session | `pre_start` shell commands | Yes — failure is fatal to session creation (`internal/runtime/tmux/adapter.go:1357-1371`; honored by the k8s provider at `internal/runtime/k8s/pod.go:221`) |
| Post-dispatch, pre-merge | `[steps.check]` on the implementing step, merge step in the same scope with `gc.on_fail="abort_scope"` | Yes — exhaustion closes `gc.outcome=fail`, and `abortScope` skips every open scope member (`internal/dispatch/runtime.go:373-380`, `:489-507`) |

One structural caveat matters: **Gas City never merges anything.** There is no
merge code path to intercept; merge is delegated to the agent via a prompt and a
`merge_strategy` metadata stamp (`internal/sling/sling_core.go:549`). That is
good news rather than bad — gonk owns its own merge, so gonk's post-dispatch
gate sits immediately before gonk's own merge call regardless of runtime.

The one acceptance-shaped surface that *is* model-mediated is
`internal/reviewquorum/`, whose lanes are LLM reviewers. It is dead code — zero
callers outside the package (verified) — and its own shipped formula says the Go
finalizer is not wired. gonk must simply not enable it.

**What this finding does and does not change.** It removes the *architectural*
objection to Gas City: adopting it would not force LLM judges on gonk. It does
not, however, make those gates reachable for gonk, for two reasons that are
independent of this ADR:

1. The two strongest gates — `[steps.check]` and the convergence gate — live on
   the **formula/convergence path**, which **ADR-007 §3 already decided to
   delete**, because upstream drops caller vars from instantiated bead text and
   so a formula provably cannot deliver gonk a prompt at all (`gonk-6gs`,
   upstream #4668, still unfixed at the pinned ref). Reaching them means
   reversing ADR-007 §3 and re-adopting an idiom that is broken for gonk's
   actual use.
2. gonk does not use the acceptance path *at all*. It uses Gas City as an
   externally driven pod launcher — seven client methods, four of which are
   start/read/stop (§3). gonk's Gate 1, Gate 2, classifier and broker are
   already deterministic and already outside Gas City.

So the honest statement of the case is: **the argument for Jobs does not rest on
"Gas City forces model judgment", because it does not.** It rests on §4 and §5 —
the reliability of the session lifecycle gonk actually depends on, and the amount
of gonk that exists only to work around it.

### 2(b) Do convoys coordinate items in a way that could carry deterministic gates between grouped items, or do they presuppose model judgment of the group?

**They are fully deterministic and presuppose no model judgment — but a convoy
by itself carries no gate at all.**

A convoy is a bead of `type=convoy` plus a set of `tracks` dependency edges
(`internal/convoy/convoy.go:48-77`; `internal/convoy/membership.go:13`). The
package is 466 non-test lines and imports nothing agent-, prompt- or
model-shaped. Completion is counting, not judgment
(`internal/convoy/convoy.go:99-114`):

```go
	for _, c := range children {
		if IsTerminalStatus(c.Status) { closed++ }
	}
	return ConvoyProgressResult{ ..., Complete: total > 0 && closed == total }, nil
```

and auto-close is an all-children-closed sweep with a fixed reason string
(`cmd/gc/cmd_convoy.go:1909-1930`, `:1416`), fired in-process on every bead close
(`cmd/gc/api_state.go:608-623`). Nothing constructs a prompt describing the
group; the only convoy fact exposed to a formula is a bare `convoy_id` string
(`internal/graphv2/invocation.go:27-29`, `:162-163`).

**The negative finding that matters:** `tracks` is not a ready-blocking
dependency type (`internal/beads/beads.go:421-431`), so membership alone
sequences nothing — a bare convoy fans its members out concurrently
(`internal/sling/sling_core.go:1201`, member loop at `:1348`).

Deterministic inter-item gating does exist, but from three mechanisms layered
*on top* of convoys, not from the convoy type:

- **drain** topologically sorts members over `blocks`/`waits-for` edges with
  cycle detection (`internal/dispatch/drain.go:868-950`) and projects those
  edges onto the generated item workflows (`drain.go:663-733`), so item 2 can be
  held until item 1's workflow root closes — but only if an operator authored
  the edge;
- **`context = "shared"` drains** serialize strictly, creating row *N+1* only
  after row *N* closes (`drain.go:495-540`);
- **`check` steps**, per §2(a), an `exec`'d program.

Drain completion is likewise a metadata read, not a judgment: `gc.outcome ==
pass` per row root (`drain.go:426-437`).

**Consequence for gonk.** Convoys are a real capability and a genuinely
deterministic one, and §6 records their loss as the most substantive cost of
this decision. gonk does not use them today.

---

## 3. What gonk actually uses

Seven exported client methods, from `pkg/gcapi`. Their complete non-test call
sites, at this commit:

| Method | Non-test callers |
|---|---|
| `CreateSession` | `cmd/gonk-gate/broker_inject.go:1066` |
| `CloseSession` | `cmd/gonk-gate/broker_inject.go:1378`, `sweep.go:889`, `reap.go:180`, `reap_pods.go:201` |
| `ListSessions` | `cmd/gonk-gate/reap.go:115` |
| `GetSessionOutput` | `cmd/gonk-gate/broker_apply.go:665`, `:815` |
| `GetSessionTranscript` | `cmd/gonk-gate/broker_apply.go:384`, `:941` |
| `RunOrder` | `cmd/gonk-gate/sweep.go:1032`, `pkg/intake/dispatch.go:692` |
| `AwaitRequestOutcome` | `cmd/gonk-gate/broker_inject.go:1430` |

Four of the seven are start/read/stop. `RunOrder` is gonk asking Gas City to run
gonk's own binary. `AwaitRequestOutcome` exists only because `CreateSession`
returns 202 and the real outcome arrives later on an event stream.

A `Job` supplies start (create), stop (delete), read (pod logs) and completion
(`Job.status`) natively, with no event-stream correlation and no second binary in
the cluster.

---

## 4. The upstream bugs that disappear

Each of these is a filed gonk bug whose cause is a Gas City mechanism, not a gonk
mistake. This is the substance of the case.

| Bug | Cause | Under Jobs |
|---|---|---|
| `gonk-alw` (P0, open) | A cold controller accepted `POST /sessions` with 202 and then never created a pod, logging nothing. | Gone. `Job` creation is admitted or rejected by the API server; there is no accept-then-maybe. |
| `gonk-cw3` (P1, open) | Session start is enqueued inside `CityRuntime.tick`, which skips itself whenever `/proc/pressure/io some avg60 > 50`, forcing a tick only after five skips. On a worker node avg60 sits at 60–98, so a real tick is ~180s apart and a create waits ~150s before any start is attempted. | Gone. The kubelet starts the pod; there is no 30s patrol gated on host I/O pressure. |
| `gonk-njz` (P1, open) | Prompt delivery into the opencode TUI is intermittent — 1 of 4 consecutive live runs received its prompt; the others sat at the splash screen for the session's life. | Gone. The prompt is a file in a `Secret` volume, read by the process, not keystrokes injected into a tmux pane. |
| `gonk-6gs` (P1, open) | Order-dispatch drops caller vars from instantiated bead text, so `{{issue_iid}}` renders empty. Upstream #4668, unfixed at the pinned ref. | Gone. No orders; gonk renders its own prompt in Go, as the broker already does. |
| `gonk-p2e` (P1, closed) | The default `scale_check` counts ready unassigned work beads and turns each into permanent new-session demand; `min_active_sessions = 0` does not suppress it. | Gone. No pool; a Job exists because gonk created it. |
| `gonk-mfsr` (P1, closed) | Gas City rolls a pending session create back — closing the session bead — and then starts the runtime anyway. The pod then holds 500m/1Gi indefinitely and **no call gonk can make reaches it**, because `ListSessions` is the open-bead list and close/stop/kill all 404 on a closed bead. | Gone. The pod is owned by the Job; deleting the Job deletes the pod, and `ttlSecondsAfterFinished` does it unattended. |
| `gonk-lv3y` (P1, closed) | The sweep could not read a broker session past the 64KB gcapi cap; a real 16-minute triage run hit it. Compounded by the peek-lines spelling mismatch (`peekLines` sent, `peek_lines` bound), so every peek this tree ever took used the server default of 5 rows. | Gone. Pod logs are a stream with no pane, no scrollback window and no 64KB envelope. |

Two of these (`gonk-alw`, `gonk-cw3`) are the reason `cmd/gonk-gate/prompt_wait.go`
exists at all; one (`gonk-mfsr`) is the reason `reap_pods.go` exists; one
(`gonk-alw`'s 300s order timeout, which SIGTERMs the whole process group
including in-flight `bd` children mid-write) is the reason `order_budget.go`
exists.

---

## 5. The deletion list and the measured LOC ledger

Measured with `wc -l` at this commit. Nothing was deleted to produce these
numbers — the bead's "do not delete anything" is binding and was observed.

### 5.1 Deleted wholesale — code that exists only because the runtime is Gas City

| Path | Lines | Why it goes |
|---|---:|---|
| `pkg/gcapi/` (10 files, prod + test) | **4639** | The client itself, incl. `gcapitest/server.go` (1250) and `writeauth.go` (279, the ed25519 write-grant signer Gas City requires) |
| `cmd/gonk-gate/prompt_wait.go` + test | 691 | Exists solely to survive `gonk-alw`/`gonk-cw3` start latency |
| `cmd/gonk-gate/reap.go` + test | 556 | Orphan-session reaping; `ttlSecondsAfterFinished` + owner refs replace it |
| `cmd/gonk-gate/reap_pods.go` + test | 1237 | Pod-level recovery for `gonk-mfsr` leaked pods |
| `cmd/gonk-gate/order_budget.go` | 180 | Phase budgeting against the 300s order timeout |
| `pack/orders/` | 135 | The `gonk-dispatch` exec order |
| `pack/agents/`, `pack/scripts/`, `pack/doctor/`, `pack/pack.toml` | 411 | The Gas City pack |
| `images/Dockerfile.controller` | 149 | Builds `gc` + `bd` + `dolt` into a controller image |
| **Subtotal, Go + pack + image** | **7998** | |

Chart objects that would go with it — **listed, not touched**, since the bead
forbids modifying the chart (they are T-27's to land):
`role-gc-agent.yaml`, `role-gc-controller.yaml`, `rolebinding-gc-agent.yaml`,
`rolebinding-gc-controller.yaml`, `serviceaccount-gc-agent.yaml`,
`serviceaccount-gc-controller.yaml`, `statefulset-gonk-dolt.yaml`,
`service-gonk-dolt.yaml`, `networkpolicy-gonk-dolt.yaml` — **342 lines**, plus
the `gc` container in `workload-gonk-controller.yaml` and the Dolt dependency it
carries.

**Measured total on the deletion list: 8340 lines.**

### 5.2 Shrinks, amount not claimed

Four files keep their purpose and lose their Gas City mechanics:
`broker_inject.go` (1579), `broker_apply.go` (951), `sweep.go` (1037),
`pkg/intake/dispatch.go` (761). **No line count is claimed for these**, because
the honest number is only knowable by doing the port, and a grep for
Gas-City-shaped identifiers over-counts badly — "session" is a domain word in
gonk that survives the change. What can be said precisely is that every
`gcapi.*` call site in §3 is in one of these four files plus the three deleted
reap/wait files.

`cmd/gonk-gate/transcript_archive.go` + test (679) is a special case: it is
*motivated* by a Gas City property (the transcript lives only in the supervisor's
session and the close destroys it, `gonk-pop3`) but its `transcriptRecord`
metadata half is runtime-independent forensics worth keeping. Counted as a shrink,
not a deletion.

### 5.3 Added

A `SessionRuntime` interface and a Jobs implementation (§7) — the Job spec, a
status poll, a log read, and Secret volume construction. T-55 owns the real
number; this spike does not estimate it, and the prototype's own line count is
recorded in §8 rather than presented as the production figure.

---

## 6. What is lost

Stated plainly, because these are real and the decision is not free.

1. **Session resume.** Gas City can relaunch a session against an existing
   workspace; a `Job` pod that dies is gone, and `backoffLimit: 0` means it is
   not even retried. Spec 11.4's named risk (ACP session resume across pod
   recreation) stops being a risk and becomes a non-feature. **T-43 owns the
   continuity decision and is unblocked by this ADR.**
2. **Pool scaling.** `scale_check` maintaining warm capacity ahead of demand.
   gonk gets cold-start latency on every session. Note this is also the source of
   `gonk-p2e`, so the loss is partly a relief.
3. **Pack composition.** Agents, orders, formulas, prompts as a distributable
   unit with a registry. gonk renders prompts in Go instead.
4. **Convoys.** Per §2(b) the most substantive loss: deterministic grouping with
   topological ordering, projected blocking edges, strictly-serialized shared
   drains, and `exec` check steps between members — none of it model-mediated.
   gonk does not use convoys today, and §7 is the seam by which they return.

---

## 7. `SessionRuntime` — the re-adoption seam

The decision is reversible at one interface, and T-55 owns its real shape:

```go
// SessionRuntime starts, observes and stops one agent session. It is the ONLY
// seam through which gonk-gate touches a session runtime; nothing above it may
// name a Job or a Gas City alias.
type SessionRuntime interface {
	// Start launches a session and returns a handle. It does not return until
	// the runtime has ACCEPTED the session -- never 202-and-hope (gonk-alw).
	Start(ctx context.Context, spec SessionSpec) (SessionHandle, error)

	// Await blocks until the session reaches a terminal state, or ctx ends.
	Await(ctx context.Context, h SessionHandle) (SessionOutcome, error)

	// Batch reads the proposed-effects batch the agent fenced in its output.
	Batch(ctx context.Context, h SessionHandle) ([]byte, error)

	// Stop destroys the session and everything it owns. It MUST be idempotent,
	// and a "not found" MUST NOT be reported as a destroyed session (gonk-wmb7).
	Stop(ctx context.Context, h SessionHandle) error
}
```

Two implementations can coexist: `jobs` (this ADR) and `gascity` (the current
code, kept alive behind the seam if the owner wants a retreat path). Convoys
return in v3 as a second implementation or as a composite runtime above this
interface, without gonk-gate learning about them.

---

## 8. Prototype status — PASSING, with its limits stated

The prototype lives in `docs/spikes/adr-008-jobs-prototype/`. **The recorded run
log is `docs/spikes/adr-008-jobs-prototype/RUN-2026-09-23-l1-job.log`.**

It ran on a disposable k3d cluster (`k3d-gonk-t45`, k3s v1.35.5+k3s1) on
2026-09-23. The reference cluster was not touched; `run.sh` refuses to execute
against any context other than the spike cluster.

**What was observed, not inferred:**

| Bead requirement | Evidence |
|---|---|
| One Job per session, existing-shaped agent image | `job.yaml`, applied and completed |
| Prompt as a Secret volume | log: `prompt mounted, 140 bytes` |
| Virtual key as a Secret volume | log: `virtual key mounted, 42 bytes (value not logged)` |
| Checkout tarball as a Secret volume | log: `checkout extracted, 1 files` |
| `backoffLimit: 0` | set; and proven live — the first run failed and the Job went straight to `BackoffLimitExceeded` with no retry |
| `activeDeadlineSeconds` = reservation TTL | **verified by observation**: a probe Job with `activeDeadlineSeconds: 10` and a 600s sleep was killed with `reason: DeadlineExceeded`, pod gone |
| `ttlSecondsAfterFinished` | **verified by observation**: a probe Job with TTL 15s was `NotFound` afterwards and `kubectl get pods -l job-name=...` returned `No resources found` — the Job *and* its pod were reaped with no gonk code running |
| Completion from Job status | `status.succeeded: 1`, conditions `SuccessCriteriaMet` / `Complete`; no event stream, no session read model |
| Batch read from pod logs | the fenced batch was extracted from `kubectl logs` between the **existing** `GONK_BATCH_START` / `GONK_BATCH_END` sentinels from `cmd/gonk-gate/broker_apply.go` |
| Runs the existing L1 synthetic scenario end to end | `PASS`, `L1 exit=0` — see below |

**The L1 scenario is the real one.** `test/integration` was compiled with
`CGO_ENABLED=0 go test -c` and the resulting binary shipped into the image. It
is the same suite the standing gate runs, driving
`test/harness.SyntheticSession` through `/decide` → LLM calls → `/outcome`
against the real intake and meter packages. It is not a reimplementation. It was
first confirmed green on the host (`ok ... 9.088s`) and confirmed self-contained
by running it from an empty working directory with no repo present.

**Limits — stated so nobody over-reads this.**

1. **The image is a spike image, not `images/Dockerfile.agent`.** The real agent
   image lives in the in-cluster GitLab registry, which is not reachable from
   this host. The prototype therefore proves the *Job mechanics* carry a
   session; it does not prove opencode runs unchanged under them. T-55 must do
   that against the real image.
2. **No LiteLLM, no NetworkPolicy, no meter in the cluster.** L1 is by design an
   in-process suite with fakes (`docs/superpowers/plans/2026-07-13-plan-06-e2e-harness.md`:
   *"L1 needs no containers at all"*). The egress-policy question is untouched
   here and remains §9's and T-27's.
3. **The batch content is synthetic.** The prototype emits a well-formed fenced
   batch describing the L1 outcome; it does not emit a real triage effect set.
   The read path is proven; the authoring path is not in scope for this spike.
4. **Single-node k3d, not kind.** kind was attempted first and failed for a host
   reason unrelated to Jobs — see §11.

**Workarounds the Job design itself required: zero.** The Job spec in `job.yaml`
is plain `batch/v1` with no tricks. One bug was found and fixed, and it was in
the prototype's own entrypoint, not the design: `tar` must be given
`--no-same-owner --no-same-permissions` because the agent runs as an
unprivileged UID and cannot restore the archive's ownership onto an `emptyDir`.
That is a real finding for T-55 — the checkout extraction must not assume root —
and it is recorded in `entrypoint.sh` at the line that fixes it.

---

## 9. Which labels the Job pods carry — for T-27

**This is the trap the bead names, and it is live.** Gas City's k8s provider
labels session pods `app: gc-agent`. Two things in this repo join on that exact
value:

- `chart/gonk/values.yaml:763-764` sets `networkPolicy.agentPodSelector` to
  `{app: gc-agent}`, consumed by `chart/gonk/templates/networkpolicy-gonk-agent.yaml`
  as the `podSelector` of the egress policy — the policy on which spec 9's
  "cloud access exists only through the metered proxy" claim rests;
- the pod reaper merged this week (`gonk-mfsr`,
  `cmd/gonk-gate/reap_pods.go`) lists pods by that label, and its own bead note
  records that **nobody has yet read the `gc-agent` label value off a live pod**.

A `Job` pod does not get `app: gc-agent` for free. It carries what the Job's
`spec.template.metadata.labels` says, plus Kubernetes' own
`batch.kubernetes.io/job-name`, `batch.kubernetes.io/controller-uid` and
`job-name`/`controller-uid`.

**Recommendation for T-27:** have the Jobs implementation set
`app: gc-agent` explicitly on the pod template, *plus* a gonk-owned label such as
`gonk-factory.github.io/component: agent-session` and
`gonk-factory.github.io/session: <alias>`. Keeping `app: gc-agent` is deliberately
ugly and deliberately safe: it means the NetworkPolicy selector, the helm-test
netpol probe (`chart/gonk/templates/test-netpol-probe.yaml`, which selects
`app=gc-agent,status.phase=Running`) and the reaper keep matching on day one,
so the runtime swap is not also a silent egress-policy regression. A wrong
selector renders fine and enforces nothing — that is the failure mode the
template's own banner warns about. Rename to a gonk-native label only as a
separate, tested change, with the probe un-skipped as the gate.

---

## 10. Why no workflow engine

No Argo Workflows, Tekton, Temporal or similar is adopted, and none was
evaluated — the bead forbids it and the reasoning does not need one. gonk's unit
of work is a single agent session: one pod, one prompt, one fenced batch, one
deterministic gate before it and one after. That is not a workflow; it is a
process with a timeout. Every candidate engine would add a controller, a CRD set
and an upgrade surface to a Flux-deployed cluster in exchange for orchestration
gonk does not currently perform — and the entire evidence of §4 is that adding a
second scheduler between gonk-gate and the kubelet is precisely what has been
costing this project P0s. If multi-step orchestration is ever genuinely needed,
it is gonk-gate chaining Jobs, with the chaining logic in Go where the existing
deterministic gates already live and can be unit-tested.

---

## 11. The reversal condition

Stated as the bead requires, and it must be read against §2's finding.

> **Reopen this decision if (a) is yes AND the Jobs prototype needs more than
> three workarounds to run the L1 synthetic scenario end to end.**

**(a) is yes.** §2(a) records it: Gas City's acceptance path can host
deterministic pre- and post-dispatch gates, because its acceptance decision is
already a subprocess exit code. The first conjunct of the reversal condition is
therefore **met**, and the decision rests entirely on the second.

On the second conjunct, the workarounds this spike actually needed are recorded
here rather than summarized, because the count is the load-bearing number:

**Workarounds required to stand up the prototype cluster** (all environmental,
none in the Job design):

1. The installed `kind` wrapper at `/root/.local/bin/kind` requires Docker
   Desktop's WSL socket, which is not running; fell back to
   `KIND_EXPERIMENTAL_PROVIDER=podman`.
2. podman 3.4.4 writes `cniVersion: 1.0.0` into every network conflist, but the
   distro's CNI plugins at `/usr/lib/cni` support only `0.4.0`, so every podman
   network was unusable. Fixed at the root by installing
   containernetworking-plugins v1.6.2 into `/usr/libexec/cni` (searched *before*
   `/usr/lib/cni`, so nothing existing was replaced or downgraded).
3. `iptables` pointed at the `nft` backend while WSL's own `nat` table lives in
   nft and a legacy `nat` table also existed, so the CNI bridge plugin could not
   program NAT at all (`table 'nat' is incompatible`). Switched the
   `update-alternatives` link to `iptables-legacy`.
4. `fs.inotify.max_user_instances` was 128, well below what a kind control plane
   needs; raised to 8192.
5. kind still failed after all of the above, and **not for a Jobs reason**: the
   `kindest/node:v1.37.0` containerd could not open its bbolt metadata database
   (`failed to open database file: file size too small 4096`), so its CRI plugin
   never loaded and the kubelet died in a restart loop with
   `unknown service runtime.v1.RuntimeService`. Switched to **k3d/k3s**, which
   the bead's own unblocking note offers as the second option, and which worked
   immediately once (2) and (3) were fixed.
6. `k3d image import` shells out to a tools container that wants
   `/var/run/docker.sock`; imported the image into the node's containerd
   directly with `ctr -n k8s.io images import` instead.
7. k3d wrote `server: https://0.0.0.0:45703` into the kubeconfig and podman's
   host port mapping did not route; pointed the kubeconfig at the node
   container's own address instead.

**These are host-environment defects, not Jobs defects.** None of them is a
workaround *in the Job spec* — the thing the reversal condition is actually
asking about. **Workarounds in the Job design: zero** (§8). The one code fix the
prototype needed was in its own entrypoint's `tar` invocation, and it is a
genuine finding for T-55 rather than a workaround: the checkout extraction must
not assume root.

**Therefore the reversal condition is not met**, and the proposed decision
stands — but it stands on §4 and §5, not on the premise in §2, which is false
and is retracted here.

A note on how to read conjunct (a) being true. It means the *stated* reason for
this decision was wrong, and a reviewer is entitled to ask whether the decision
survives losing it. It does, but on narrower ground: not "Gas City would force
LLM judges on gonk" (it would not), but "gonk uses seven methods of Gas City,
four of them start/read/stop, and that thin usage has cost two open P1s, one
open P0 and four closed P1s whose causes are all Gas City lifecycle mechanics"
(§4), while 8340 measured lines of gonk exist to speak to it or to survive it
(§5). If the owner weighs those differently, §7 is the seam that makes the
reversal cheap.

If the owner reverses, T-55..T-58 are replaced by their Gas City-shaped
equivalents, and the shape they would take is §2(a)'s table: `sling_query` as the
pre-dispatch veto (it needs no formula, so it survives ADR-007 §3) and gonk's own
pre-merge check in gonk-gate, since Gas City does not merge.

## 12. Owner decision, 2026-09-23

**CONFIRMED, WITH ONE GATE: prove opencode under a Job before anything else moves.**

The decision to make Kubernetes Jobs the session runtime stands, on the narrower
grounds §11 sets out -- the lifecycle bug ledger and the 8340 measured lines --
and explicitly NOT on the premise T-45 was written with. That premise, that Gas
City's acceptance is model-mediated, is false at the pinned ref: `GatePass` is
produced only on a subprocess exit 0 (`internal/convergence/condition.go:394-399`),
`[steps.check]` refuses any mode but `exec` (`internal/formula/types.go:1486-1492`),
and the one model-mediated acceptance surface, `reviewquorum`, has no callers.
Answer (a) is YES. Anyone revisiting this decision should start from that fact
rather than from the bead's framing.

The stated reversal condition -- "(a) is yes AND the prototype needs more than N
workarounds" -- does not fire, because the prototype needed **zero** workarounds.
But it was authored expecting (a) to be no, so it is recorded here that the
condition was not applied mechanically: the owner re-weighed the evidence and
reached the same destination by a different route.

**THE GATE.** T-55 must prove that opencode actually runs under a Job with the
REAL agent image (`images/Dockerfile.agent`) before T-56, T-57, T-58, T-20, T-21,
T-41 or T-42 proceed. The prototype used a purpose-built spike image, because the
real one lives in an in-cluster registry the spike cluster could not reach, so
**opencode-under-Jobs is unproven**. The prototype proves the Job lifecycle --
completion from `status.succeeded`, batch read from the existing
`GONK_BATCH_START/END` sentinels, `ttlSecondsAfterFinished` and
`activeDeadlineSeconds` each observed rather than assumed -- and nothing about
the agent runtime inside it.

That distinction is the whole reason for the gate. A Job that schedules, runs and
reports is not evidence that an interactive-shaped agent behaves inside one, and
this project has been bitten before by treating a green proxy as the measurement.
If opencode does not run cleanly under a Job, that is a finding which reopens
this ADR, not an obstacle for T-55 to work around.

Also carried forward from the prototype, for T-55: checkout extraction must pass
`tar --no-same-owner --no-same-permissions`, because the agent is not root; and
the Job pod keeps the `app: gc-agent` label deliberately, so the chart's
NetworkPolicy selector, the helm-test netpol probe and the pod reaper merged on
2026-09-23 all keep matching through the swap. A wrong selector renders fine and
enforces nothing.

---

## 13. The §12 gate, discharged: 2026-09-24

**MET, with the model endpoint stubbed. This ADR is not reopened.**

opencode from the REAL agent image -- built locally from
`images/Dockerfile.agent` with podman, `opencode --version` = 1.18.3 = the
`OPENCODE_VERSION` pin -- started under a `batch/v1` Job, took its prompt by the
**pull** path, resolved the gonk provider, sent real chat-completions requests
with the attribution header attached, and left a `GONK_BATCH_START/END` fence in
the pod log; the Job reported `status.succeeded: 1`. Evidence, per clause, in
[`docs/spikes/adr-008-t55-gate/`](../spikes/adr-008-t55-gate/).

**What is stubbed and what that costs.** The model is a stub: the real LiteLLM
is reachable but driving it needs a live virtual key, which this run was not
permitted to materialize. So the *mechanism* is proven -- provider resolution,
the `{file:...}` key indirection, SSE parsing, tool-call execution, stdout
capture -- and the *model's behaviour* is not. Nothing is known from this run
about real latency, context limits, rate-limit or error responses, or whether a
real model in this harness emits a well-formed batch. A live-model run remains
worth doing; it is no longer what the downstream tasks are waiting on.

**The one blocking defect the gate found, and it was in gonk, not in opencode.**
`images/agent/entrypoint.sh` ended `opencode run` with
`while :; do sleep 3600; done` -- the tmux hold that keeps Gas City's transcript
readable (gonk-2tb). Under a Job the container never exits: the first gate run
finished opencode with `rc=0` in ~8 seconds and the Job was still `active` four
minutes later, until `activeDeadlineSeconds` reported it `Failed` /
`DeadlineExceeded`. **A clean session was indistinguishable from an overrun
one**, which is precisely the class of bug this ADR exists to end. The hold is
now conditional on `GONK_SESSION_RUNTIME`, defaulting to the Gas City behaviour;
the Jobs runtime sets `GONK_SESSION_RUNTIME=jobs` and gets `exit $rc`.

Three further observations T-55's implementation must carry, all recorded with
their evidence in the spike README: every `log()` line is **duplicated** in a Job
because the entrypoint is PID 1 and `log()` writes to both stderr and
`/proc/1/fd/1`; checkout extraction as uid 65532 **succeeded without**
`--no-same-owner`, because GNU tar implies it for a non-root user (§12's
carry-forward is sound advice but is not what would have broken);
and `opencode run` **exits 0 after a rejected tool call**, so with the hold
removed a refused session becomes a *succeeded* Job -- the exit code is not the
outcome, the batch is.

---

## 14. T-55 design decisions, as decided and as built: 2026-09-25

The plan these were made in is
[`docs/plans/2026-09-25-t55-session-runtime.md`](../plans/2026-09-25-t55-session-runtime.md)
section 3.0; this section is the durable record. Where a part refined a
decision while building it, the refinement is stated with the decision.

**Owner decision the whole set rests on (2026-09-25, cutover option A).** Dev
cuts over to Jobs AT the T-55 merge. There is no `gascity` implementation
behind the seam: dispatch and sweep call `SessionRuntime`, and its only
production implementation is `pkg/runtime/jobs`. Rollback is a `git revert` of
the merge. `pkg/gcapi` is not deleted (T-56); it stops being reachable from
dispatch and sweep. §7's "two implementations can coexist" remains possible and
is simply not built.

**§3 undercounted: gonk uses EIGHT Gas City methods, not seven.** The table in
§3 misses `GetSessionState` (`cmd/gonk-gate/broker_apply.go:684`,
`broker_inject.go:1574` at main 073fca9), which the sweep's session view and the
inject path both call. It is a status read, so it maps onto `Status` below and
changes nothing about the decision; the table is left as written and corrected
here.

**D1 -- the interface, and the deviation from §7.** `pkg/runtime`:

```go
type SessionRuntime interface {
	Start(ctx, Spec) (h Handle, created bool, err error) // returns once ADMITTED; an existing name -> that session, created=false, nil error
	Status(ctx, Handle) (Status, error) // non-blocking
	Output(ctx, Handle) ([]byte, error) // full container log, no size envelope
	Stop(ctx, Handle) error             // unknown handle -> ErrNotFound, never "destroyed"
	List(ctx) ([]Handle, error)         // sessions this runtime owns
}
```

`Spec` carries Name, Agent, Image, non-secret Env, and the MOUNTED payloads
(Prompt, LiteLLMKey, Metadata, and -- since the handshake part -- RigURL), a
Deadline and Labels. **§7's `Await`
and `Batch` are not built.** The sweep is a periodic process, not a waiter, so
it needs a non-blocking `Status` rather than a blocking `Await`; and batch
extraction stays in gonk-gate (`extractBatch`) over `Output`, so the
`GONK_BATCH_START/END` fence parser keeps exactly one owner. `List` is added,
for the orphan pass (D7). As built (runtime part): `Handle` is a `string` type
whose value IS the name (it round-trips through `Record.SessionID`); `Phase`
has `PhaseUnknown` at ZERO so a Status nobody filled in cannot read as
"pending, keep waiting"; `Status` of an unknown handle is `PhaseNotFound` with a
nil error (absent is an answer), while `Output` and `Stop` wrap `ErrNotFound`.
Added by the handshake part: `Status.ExitCode *int`, the agent container's
terminated exit code for a finished Job's newest pod (nil when there is none),
read by the sweep for exactly one code (the contract refusal, below). Added by
cutover-safety (plan 3.4 C2): `Start` reports `created` -- true when this call
made the session, false when it adopted an existing one. The name is
deterministic, so two dispatches for one bead and attempt can reach one
session; dispatch's failed-record-write teardown stops the session only when
it created it, never another dispatch's recorded session.

**D2 -- name = handle = `Record.SessionID`.**
`gonk-<agent>-p<projID>-<i|m><iid>-a<attempt>`, DNS-1123, at most 52
characters (the Job name feeds pod names and the 63-character job-name label).
Too long or not a valid label -> sanitized, truncated prefix plus 8 hex of
sha256 of the PLAIN form. A project-scoped session omits the issue segment
(`gonk-scaffold-p42-a1`). Deterministic, so a re-dispatch of the same bead and
attempt meets AlreadyExists and gets the existing session -- what the
pending-prompt reservation approximated; the reservation machinery stays, as
belt and braces.

CORRECTION (gate-rewire part): the name is NOT a capability, but the rig
checkout grant IS -- pkg/rig serves a granted tree to anyone presenting its
key on gonk-intake's private listener, which every agent pod can reach, and
the Gas City alias used to be that key. So the grant is keyed by
`<name>.<128-bit base32 nonce>` (`rigGrantKey`), never by the bare name, and
the session is handed the full URL (mounted, D5). Consequence: the sweep cannot
name the grant, so `closeSession` no longer revokes it; it expires on its TTL,
and Stop deletes the only pod that holds its URL.

**D3 -- delivery by one Secret per Job.** Keys `prompt`, `litellm-key`,
`metadata.json` (and `rig-url` when a checkout was granted, D5), mounted
read-only and NON-optional at `/run/gonk/session`.
Created AFTER the Job with a controller ownerReference to the Job's UID, so it
is collected with the Job; kubelet holds the pod until it exists. If the
Secret create fails, Start deletes the Job (on a context detached from the
caller's, so a cancelled caller cannot strand it) and returns the error: no
half-session. The key comes from the existing fail-closed `resolveLiteLLMKey`,
never an ambient one (gonk-8gb). As built: `blockOwnerDeletion` is left unset
(it would need `update` on `jobs/finalizers`, which Role `gonk-gate-jobs` does
not grant); the volume's `defaultMode` is 0440 with `fsGroup` 65532, so the
non-root agent reads the files by group.

**D4 -- the checkout stays on the rig grant.** The per-session rig token
fetched from gonk-intake already is the "per-Job token fetch" the bead allowed
for tarballs over the 1 MiB Secret limit; it has no size limit and its
NetworkPolicy rule exists. No tarball goes in a Secret. The entrypoint's
extraction gains `--no-same-owner --no-same-permissions` (§12's carry-forward,
though §13 found GNU tar implies it for a non-root user).

**D5 -- the agent's env comes from the pack, not from Gas City.** The pack's
`[env]` for the agent, plus `GONK_SESSION_RUNTIME=jobs`,
`GONK_SESSION_DIR=/run/gonk/session`, `GONK_LITELLM_URL`, `GONK_RIG_BASE_URL`,
`GONK_MODEL`, and the session name/agent/attempt. The rig fetch URL is NOT env
(handshake part, plan 3.3 item 4): it embeds the grant's key (D2 correction),
so it is `Spec.RigURL`, written to the session Secret as `rig-url` and read by
the entrypoint from that file; the runtime refuses a Spec whose env carries it,
and the entrypoint unsets any `GONK_RIG_URL` in jobs mode. NOTHING else: no `GC_DOLT_*`, `BEADS_*`, `GC_INSTANCE_*`, `GC_CITY_WRITE_*`, all of
which a live Gas City agent pod carries (gonk-426v). As built (jobs part): the
runtime REFUSES a Spec whose Env names one of those prefixes before any API
call, and itself always sets `GONK_SESSION_RUNTIME`, `GONK_SESSION_DIR`,
`GONK_SESSION_NAME`, `GONK_AGENT` and (handshake part) `GONK_SESSION_CONTRACT`
(a Spec may repeat them with the same value, never contradict them). `HOME` and the four `XDG_*` default under
`/tmp/gonk-home` when the Spec does not set them; the pack does, and wins.

The controller-side configuration the runtime reads is a separate set -- chart
env, never copied to the pod -- named in `internal/envname`:
`GONK_SESSION_NAMESPACE` and `GONK_AGENT_IMAGE` (required),
`GONK_SESSION_SERVICE_ACCOUNT`, `GONK_SESSION_TTL_SECONDS`,
`GONK_SESSION_CPU_REQUEST`, `GONK_SESSION_MEMORY_REQUEST`,
`GONK_SESSION_CPU_LIMIT`, `GONK_SESSION_MEMORY_LIMIT` (defaulted to D6's
values). All marker-free, so Gas City's exec-order env filter keeps them.

**D6 -- the pod spec.** Kept from the live Gas City pod (s-go-do58): pull
secret `gitlab-registry-pull-creds`, requests 500m/1Gi, limits 2/4Gi,
`restartPolicy: Never`, `HOME=/tmp/gonk-home` and the XDG dirs under it.
Changed: ServiceAccount `gc-agent` with `automountServiceAccountToken: false`;
`runAsNonRoot`, uid/gid 65532; `allowPrivilegeEscalation: false`; drop ALL
capabilities; no tmux -- no command override at all, the image's ENTRYPOINT is
the session. Job: `backoffLimit: 0`; `activeDeadlineSeconds` = seconds until
the meter reservation expires (rounded up, so the Job never dies before the
reservation does); `ttlSecondsAfterFinished: 3600`, so the ~60s sweep can read
a finished session's log. Labels: `app: gc-agent` on the pod (§9: the
NetworkPolicy and the netpol probe select it), `gonk-factory.github.io/component:
agent-session` and `gonk-factory.github.io/session: <name>` on the Job, the pod and
the Secret.

**D7 -- reaping.** As decided, this said `reap_pods.go` would delete every Job
pod because Job pods carry `app: gc-agent`. **That premise was false**
(verify round 1): the pod half deletes only pods whose `gc-agent` label is a
gonk alias (`gonkSessionAlias`), and Job pods carry no `gc-agent` label. The
real reason `runReap` left the sweep path is that its work is CLOSING GAS CITY
SESSIONS it finds by listing Gas City: the sweep holds no Gas City client by
design (exit criterion 2 -- it sees Gas City only through two one-method
interfaces wired in `main.go`, `orderRunner` and `legacySessionCloser`), and once dispatch starts Jobs gonk creates no Gas
City sessions for it to find. `TestSweepNeverRunsTheGasCityReaper` walks
runSweep's call graph over every non-test file and fails if `runReap` or
`runPodReap` becomes reachable.

As built: orphan Jobs are the Job's own job (`activeDeadlineSeconds` +
`ttlSecondsAfterFinished`), with `reapStaleSessions` as the backstop:
`Runtime.List`, minus every session a `running` OR `pending-prompt` record
names (a reservation names its session before Start), stopping one only when
`Status` gives it an age (FinishedAt, else StartedAt) past
`staleSessionAfter` = the DEFAULT reservation TTL + 1h (the Job TTL; the
runtime does not expose a session's own deadline, and a deployment with a
shorter reservation_ttl only makes the pass later, never earlier). A failed
store read stops nothing. `reap.go`, `runPodReap` and `prompt_wait.go` stay in
the tree, unreferenced, until T-56; `order_budget.go` stays wired (gonk-gate is
still a Gas City order until T-57).

The sessions in flight AT THE CUTOVER are the one Gas City class left, and they
are handled explicitly (plan 3.4 C1, `cmd/gonk-gate/sweep_legacy.go`): a
running or pending-prompt record whose SessionID has the legacy alias shape is
PARKED without judgement -- no outcome to meter, no attempt charged, the
reservation kept, re-dispatched as a Job by the same pass's unpark. The park
then CLOSES that alias's Gas City session through `legacySessionCloser` (an
interface in the sweep, implemented in `main.go` by the gcapi client exactly
as `orderRunner` is, so no dispatch or sweep file imports `pkg/gcapi`; a 404
is "already closed", INFO; any other error is a WARN) and only then deletes its
pods by the `gc-agent=<alias>` label. Closing first matters because Gas City's
reconciler restarts the pod of an OPEN session whose pod has gone and ignores
closed ones (read from Gas City source, not observed). A standing per-pass
legacy pod pass is the backstop: it deletes every `app=gc-agent` pod whose
`gc-agent` label is a gonk alias that no running or pending-prompt record
claims, with the pod reaper's list/delete (Role `gonk-gate-podreap`, armed by
`GONK_POD_REAP_NAMESPACE`); once a close succeeded it finds nothing for that
alias. It is safe on label shape alone because after the cutover nothing
creates a Gas City session for gonk (single Recreate controller). This close
is not the reaper: it closes the one alias a record named, never something
found by listing Gas City. If the legacy reservation has passed or passes
before the retry settles, meter's janitor records that attempt infra-failed
and the retry is attempt N+1.

**D8 -- re-sling.** `refire` and intake's pour call Gas City's `RunOrder`,
which is gonk-gate's own scheduler until T-57, not the session runtime. They go
behind a narrow `orderRunner` interface wired in `main.go`, so no dispatch or
sweep file imports `pkg/gcapi`; `main.go` importing it for that runner is the
stated exception.

**D9 -- the sweep's judgement.** Pending/Running -> not finished (until the
reservation expires). Succeeded OR Failed -> read `Output`, extract the batch,
judge ON THE BATCH: opencode exits 0 after a rejected tool call (§13), so the
exit code is never the outcome. Failed with reason `DeadlineExceeded` ->
`infra-failed`. NotFound -> the existing released-by-absent path. The
peek/deep-peek/transcript-fence/pane-width ladder in `brokerSessionView` is
deleted from the sweep path, not ported; the transcript archive takes `[]byte`
from `Output`. As built (jobs part): Succeeded/Failed come only from the Job's
`Complete`/`Failed` conditions. `FailureTarget` and `SuccessCriteriaMet` are
NOT terminal -- the pod may still be terminating and its log incomplete -- and
read as Running (a FailureTarget's reason is carried in `Reason`). Before that,
a waiting container's reason (`ImagePullBackOff`, `CreateContainerConfigError`)
or an unschedulable pod's reason is surfaced in `Status.Reason` while Pending.
A Job with no pod has produced no output: `Output` is empty with a nil error.
As built (gate-rewire): a finished session whose Output is empty is judged
infra-failed at once (ArtifactUnknown, cause logged), and every infra-failed
verdict logs `sweep: classified infra-failed` with a `cause=`. As extended by
the handshake part: BEFORE the phase switch, a Failed session with `ExitCode`
8 whose output holds the entrypoint's contract-refusal line is image skew --
parked, never judged (below). As extended by cutover-safety: a record naming a
legacy Gas City alias never reaches `Status` at all (D7).

**D10 -- the entrypoint under Jobs** (`GONK_SESSION_RUNTIME=jobs`): read prompt,
key and metadata from `$GONK_SESSION_DIR`; any missing or empty -> log and exit
with a distinct non-zero code, never wait, never fetch. `log()` writes once
when the entrypoint is PID 1 (§13's duplicated lines). Exit with opencode's rc
(§13's fix). The Gas City fetch loop and tmux hold remain only for
`GONK_SESSION_RUNTIME` unset, until T-56 deletes them. As extended by the
handshake part: in jobs mode, before reading anything, the entrypoint checks
`GONK_SESSION_CONTRACT` against its `GONK_SUPPORTED_SESSION_CONTRACTS` (today
`"1"`); unset or unlisted -> `FATAL: session contract mismatch: sent=<v>
supported=[<set>]`, `session end: refused (session contract mismatch)`, exit
**8**. The checkout URL comes only from the mounted `rig-url` file.
As extended by gonk-un7z (the loop watchdog): opencode's output passes
through a line filter (stdout and stderr merged, bytes unchanged; opencode
in its own process group via `setsid`, a FIFO rather than a pipeline so its
rc survives dash's lack of pipefail). When the SAME tool call FAILS for the
`GONK_LOOP_WATCHDOG_THRESHOLD`-th time (default 2) -- a line whose
ANSI-stripped text starts with opencode's U+2717 glyph, signature = tool and
arguments -- the filter SIGKILLs opencode's group, the entrypoint logs
`loop watchdog: stopped opencode -- the same tool call failed <n> times
(threshold <t>): <call>` and exits **9** (`runtime.ExitLoopWatchdog`). The
sweep judges it like any session (no batch + model spend = gate-failed,
escalates; stale spend stays infra-failed) and logs the call as the cause on
`sweep: classified gate-failed`. Not a contract bump: no peer misreads 9.
Pinned to the real opencode format by the k3d component test.
Amended by gonk-mf3v: a call whose arguments are not JSON has no arguments
for opencode to print -- every one is the bare line `Invalid Tool`, with the
tool and the argument text on the NEXT line -- so its signature is `Invalid
Tool: <that next line>`. Two different malformed calls are not a loop; the
identical one twice is. Without a detail line it is not counted.

**The session contract (plan 3.3, "handshake").** The controller and the agent
image are released separately, and dev has run them at different versions for
~20 minutes at a time. Everything they must agree on is versioned by one
integer, `runtime.SessionContract` (1), which the Jobs runtime always sets as
`GONK_SESSION_CONTRACT`. A session the entrypoint refuses (exit 8 AND the
refusal line; the code alone is not proof, since jobs mode passes opencode's rc
through) is logged at ERROR with both versions, reports NO outcome (so it is
not an attempt: meter's `/decide` hands the open reservation back verbatim), is
parked with a 2-minute retry-after and its refused Job stopped; Start refuses
to adopt a same-name Job that is still being deleted. The limit: a skew that
outlasts the reservation is charged by meter's janitor as an infra-failed
attempt, and the retry only gets what is left of the reservation. At the 60m
default TTL that takes a skew about three times the widest observed (~20 min);
at dev's 30m `reservation_ttl` it is about 1.5 times, and a retry after a long
skew may itself run out of time. The handshake protects only skews in which
both images speak contract >= 1: an agent image from before the Jobs
entrypoint exits 5 ("no alias"), not 8, so the first cutover needs the single
release tag (plan 3.3 release-unit) live before it.
