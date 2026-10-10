# ADR-008 prototype: one Kubernetes Job per session

Spike artifact for **T-45** (`gonk-t45`). Supports
[`docs/adr/ADR-008-session-runtime.md`](../../adr/ADR-008-session-runtime.md).

**This is not production code.** Nothing here is wired into `make gate`, the
chart, or `images/`. It exists to answer one question with evidence: can a
`batch/v1` Job carry everything a Gas City session carries, and can gonk-gate
read completion and the proposed-effects batch back out without an HTTP client,
an event stream, or a tmux pane?

**Answer: yes.** Recorded run: [`RUN-2026-09-23-l1-job.log`](RUN-2026-09-23-l1-job.log).

## Files

| File | What it is |
|---|---|
| `job.yaml` | The Job. Every field the bead names, with the upstream bug each one closes in a comment. |
| `entrypoint.sh` | Stand-in agent. Reads prompt / key / checkout from Secret volumes, runs the real L1 suite, emits the fenced batch. |
| `Dockerfile` | Spike image: alpine + the compiled L1 test binary. |
| `run.sh` | What gonk-gate would do: create secrets, apply the Job, poll **Job status**, read the batch from **pod logs**, assert on fields. |
| `RUN-2026-09-23-l1-job.log` | The recorded passing run. |

## Running it

Needs a disposable cluster. `run.sh` **refuses** any kubectl context other than
`k3d-gonk-t45` / `kind-gonk-t45` — the reference cluster is read-only for this work.

```sh
# 1. Build the L1 binary on the host (it is fully self-contained).
CGO_ENABLED=0 go test -c ./test/integration -o .spike-run/integration.test

# 2. Build the spike image.
cp .spike-run/integration.test docs/spikes/adr-008-jobs-prototype/
podman build -t localhost/gonk-l1-agent:spike docs/spikes/adr-008-jobs-prototype/

# 3. Get it into the cluster. `k3d image import` wants a docker socket, so go
#    to the node's containerd directly.
podman save --format docker-archive -o /tmp/l1-agent.tar localhost/gonk-l1-agent:spike
podman cp /tmp/l1-agent.tar k3d-gonk-t45-server-0:/tmp/l1-agent.tar
podman exec k3d-gonk-t45-server-0 ctr -n k8s.io images import /tmp/l1-agent.tar

# 4. Run it.
bash docs/spikes/adr-008-jobs-prototype/run.sh
```

## The two things a reader should take away

1. **The prompt is a file, not keystrokes.** `gonk-njz` is the bug where 3 of 4
   live runs never received their prompt because it was typed into an opencode
   TUI pane. Here the kubelet mounts a Secret: it either happens, or the pod
   fails. It cannot half-arrive.
2. **Nothing gonk owns reaps anything.** `ttlSecondsAfterFinished` was verified
   by observation to delete the Job *and* its pod, and `activeDeadlineSeconds`
   was verified to kill an overrunning session with `DeadlineExceeded`. Those
   two fields are what delete `cmd/gonk-gate/reap.go` and `reap_pods.go`.

## Label note — read this before T-27

The pod template deliberately keeps `app: gc-agent`. The observed labels on the
Job's pod were:

```
app=gc-agent
app.kubernetes.io/component=agent-session
app.kubernetes.io/name=gonk
batch.kubernetes.io/controller-uid=<uid>
batch.kubernetes.io/job-name=gonk-session-l1
controller-uid=<uid>
job-name=gonk-session-l1
```

`chart/gonk/values.yaml` sets `networkPolicy.agentPodSelector: {app: gc-agent}`,
and the pod reaper and the helm-test netpol probe join on the same value. A
wrong selector renders fine and enforces nothing, so keeping `app: gc-agent`
through the runtime swap is deliberate. See ADR-008 §9.
