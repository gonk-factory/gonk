# Component test: the Jobs session runtime on a real cluster

`jobs_component_test.go` (build tag `component`) drives the real
`pkg/runtime/jobs` runtime against a real Kubernetes cluster, with the real
agent image and its real entrypoint. It is gonk-t55 plan part P6
(`docs/plans/2026-09-25-t55-session-runtime.md`). It is NOT part of `make gate`
and not run by CI: it needs a disposable k3d (or kind) cluster.

## Re-run it

```sh
PATH=/snap/go/11262/bin:$PATH make component-jobs
```

That is three steps; each can be run by hand.

1. Build the agent image with exactly `make agent-image`'s build args (from
   `images/versions.env`), tagged with the commit:

   ```sh
   make agent-image REGISTRY=localhost        # -> localhost/gonk-agent:v0.1.0-<sha12>
   ```

2. Import it into the k3d node. `k3d image import` needs a docker socket, so
   it goes through podman and containerd directly (the T-45 route):

   ```sh
   IMG=localhost/gonk-agent:v0.1.0-$(git rev-parse --short=12 HEAD)
   podman save --format docker-archive -o /tmp/agent.tar "$IMG"
   podman cp /tmp/agent.tar k3d-gonk-t45-server-0:/tmp/agent.tar
   podman exec k3d-gonk-t45-server-0 ctr -n k8s.io images import /tmp/agent.tar
   ```

3. Run the tests. The context is always explicit (never the kubectl
   default), and the test refuses any context that is not `k3d-*` or `kind-*`:

   ```sh
   GONK_COMPONENT_KUBE_CONTEXT=k3d-gonk-t45 \
   GONK_COMPONENT_AGENT_IMAGE="$IMG" \
   GONK_COMPONENT_STUB_LISTEN=192.0.2.1:0 \
     go test -tags component ./cmd/gonk-gate/ -run '^TestComponentJobs' -race -count=1 -v
   ```

The contract-mismatch test also needs `GONK_COMPONENT_SKEW_IMAGE`. That is the
agent image with its `GONK_SUPPORTED_SESSION_CONTRACTS` line rewritten to
`"999"`. `make component-jobs-image` builds it FROM the agent image as
`localhost/gonk-agent:<tag>-skew999` and imports it the same way.

Without these settings the tests FAIL. They do not skip, because a
skipped component test reads as green.

## The stub model

The model endpoint (LiteLLM's `/v1/chat/completions`, streamed) and the
checkout listener (`/rig.tar.gz`) are served by the test binary itself
(`jobs_component_stub_test.go`). `GONK_COMPONENT_STUB_LISTEN` is where it
listens, and pods use the same address to reach it. For k3d under podman,
use the k3d network's gateway, which is the host:

```sh
podman inspect k3d-gonk-t45-server-0 --format '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}'
```

On this box the API server is reached at `https://192.0.2.3:6443` (the
kubeconfig's own server). Podman's published port does not answer from here.

## What it asserts

- `TestComponentJobsSessionRunsTheRealAgentImage`:
  - Start creates the Job, then its owned Secret.
  - A second Start of the same name returns the same Job (same UID) and
    creates no second Secret.
  - The pod carries `GONK_SESSION_CONTRACT=1`, and no env value contains
    the rig checkout URL. The URL is mounted as `rig-url`.
  - The pod's env names are exactly D5's, with none of
    `GC_DOLT*`/`BEADS_*`/`GC_INSTANCE*`/`GC_CITY_WRITE*`.
  - The stub saw the mounted key, the metadata and the prompt text, and no
    prompt fetch.
  - The session reaches Succeeded.
  - gonk-gate's `extractBatch` reads exactly the stub model's batch out of
    `Output`, and the batch passes the triage shape.
  - The prompt is not echoed into the log.
  - Stop removes the Job, the pod and the Secret.
- `TestComponentJobsAModelThatEmitsNoBatchYieldsNoBatch`: the model emits
  no fence, and `extractBatch` does not find the prompt's EXAMPLE fence.
- `TestComponentJobsASessionContractMismatchExitsEightBeforeTouchingAnything`:
  the real `Start` (contract 1) runs against the skew image. The pod exits
  8, and Status is Failed with `ExitCode` 8. The log holds `session contract
  mismatch: sent=1 supported=[999]` and the refusal line. The stub sees no
  requests.
- `TestComponentJobsDeadlineExceededReachesFailedWithTheSweepsReason`: a
  session gets a 5s deadline and a stub that never answers. It reaches
  Failed with the exact reason the sweep's infra-failed case matches.
- `TestComponentJobsALoopingModelIsStoppedByTheWatchdog` (gonk-un7z): the
  stub model answers every tool-offering call with the same `read` of a file
  the checkout lacks, so real opencode fails the same call repeatedly. The
  entrypoint's loop watchdog must stop it at the second failure: Status is
  Failed with `ExitCode` 9, the stub answered 2 (at most 3) tool calls of the
  8 it would have, and the sweep's `loopWatchdogCause` reads the repeated
  call from the log. This pins the watchdog to the real opencode format; it
  is what goes red when an opencode bump changes the failed-call line.
- `TestComponentJobsMalformedToolCallsAreToldApartByTheWatchdog` (gonk-mf3v):
  the stub model answers with `glob` tool calls whose arguments are not JSON
  (one closing brace too many -- what a model sent on the first live pipeline
  diagnosis). Real opencode prints every such call as the bare line `Invalid
  Tool` and names the tool and the argument text on the NEXT line. Two
  subtests: two DIFFERENT malformed calls followed by an answer finish
  Succeeded, not stopped; the IDENTICAL malformed call twice is stopped with
  `ExitCode` 9 and a cause that names `glob` and `README*`. test/entrypoint's
  `malformed-*.txt` fixtures are cut from this test's pod logs.

- `TestComponentJobsPipelineAgentGroundedDiagnosisIsAccepted` (gonk-tios
  P8 (a), `jobs_component_pipeline_test.go`): the pipeline agent's real
  prompt (`fitPipelinePrompt` over a fake GitLab's failed pipeline) in a real
  Job of the real image. The pod's env is D5's with `GONK_AGENT=pipeline` and
  the pipeline pack's `OPENCODE_PERMISSION` byte for byte. The stub model
  quotes a line from the excerpt, `extractBatch` reads it back out of the pod
  log, and the real sweep chain (`applyPipelineBatch`) ACCEPTS it. The run
  note lands `diagnosed` with the class label.
- `TestComponentJobsPipelineAgentUngroundedLineIsRefused` (P8 (b)): the same
  session, but the model quotes a line that is not in the excerpt. The chain
  refuses it with `evidence_ungrounded` (gate-failed) and writes nothing.
- The loop-watchdog case (P8 (c), gonk-un7z) is pending: the watchdog is not
  merged on this branch.

- `TestComponentJobsPipefixAgentFixIsAcceptedByTheGate` (gonk-2usw part
  AGENT (a), `jobs_component_pipefix_test.go`): the pipefix agent's real
  prompt (`fitPipefixPrompt` over a fake GitLab's failed pipeline and
  repository) in a real Job of the real image. The session is a `batch/v1`
  Job; the pod's env is D5's with `GONK_AGENT=pipefix` and
  `OPENCODE_PERMISSION` equal to the PIPELINE pack's byte for byte. The tool
  list real opencode offered the model -- read off the chat requests the stub
  received -- holds no `bash`, `edit`, `write`, `patch`, `webfetch`,
  `websearch`, `task` or `question`, and does hold `read`. The stub model
  emits the fixture's fix; `extractBatch` reads it back out of the pod log;
  the pipefix shape, `verifyPipefixContext` and `effects.PlanFix` ACCEPT it,
  and the plan is the one corrected file. Nothing is written to GitLab.
- `TestComponentJobsPipefixAgentCheatingBatchIsRefused` (part AGENT (b)): the
  same session, but the model weakens the failing test instead of correcting
  the code. The edit applies cleanly to the blob; the gate refuses it with
  `SC-CLASS(test)` and yields nothing to apply.

  Run just these two: `make component-jobs-test COMPONENT_RUN='^TestComponentJobsPipefix'`.
  There is no pipeline-fix apply chain yet (plan part APPLY), so "the gate"
  here is the shape, gate A9 and `PlanFix`; nothing commits or opens an MR.

Each test runs in its own namespace, `gonk-t55c-<random>`, which it deletes
when it finishes.

- `jobs_component_pipefix_apply_test.go` (gonk-2usw part APPLY): the fix
  session is prepared by gonk-dispatch's production preparer, runs as a real
  Job, and is then judged by the real sweep, whose apply writes to the fake
  GitLab (`pkg/glab/glabtest`).
  - `TestComponentJobsPipefixAcceptedBatchOpensAMergeRequest`: the fake
    GitLab's write log is exactly branch, commit, merge request, fix note; the
    committed file is the patched content; the fix record is `verifying`; no
    outcome is reported; the Job is gone.
  - `TestComponentJobsPipefixWeakeningBatchIsRefusedAndWritesNothing`: a
    skip added to a Go test is refused `SC-WEAKEN(added-skip)`, reported
    gate-failed, and the write log is empty.

- `jobs_component_pipefix_verify_test.go` (gonk-2usw part VERIFY): the fix
  sessions run as real Jobs and are applied AND JUDGED by the real sweep; the
  fix merge request's pipeline is played by the fake GitLab.
  - `TestComponentJobsPipefixGreenPipelineEndsFixedWithOneSuccess`: while the
    (fake) merge request pipeline runs nothing is reported; once it is green
    the fix is `fixed`, the meter got exactly ONE `success`, the merge request
    carries `gonk::fix-verified` and is still open (nothing merges), the Job
    is gone.
  - `TestComponentJobsPipefixRecurringPipelineIsReslungOnceThenDenied`
    (`max_attempts: 2`): the pipeline fails the way it was diagnosed ->
    `gate-failed`, a re-sling order; the SECOND session -- a second real Job,
    in its own namespace, whose prompt carries the exit check's feedback -- is
    applied as a second commit on top of the first, on the same merge
    request; its pipeline recurs too -> `gate-failed` again, the fix ends
    `denied` (plan 7.1: `not-fixed` is not terminal), the merge request
    carries `gonk::fix-failed`, and no third order is fired.

  Run just these two: `make component-jobs-test COMPONENT_RUN='^TestComponentJobsPipefix(Green|Recurring)'`.
  `TestComponentJobsPipefixAcceptedBatchOpensAMergeRequest` now runs the sweep
  as a build WITHOUT the exit check (it asserts the state the apply leaves).

- `jobs_component_retry_feedback_test.go` (gonk-nf9i): the refused-answer
  feedback reaches a real session.
  - `TestComponentJobsPipefixRefusedAnswerIsCorrectedWhenTheFeedbackReachesTheSession`:
    ONE stub model serves both attempts of one fix (`sessionStub.ifPrompt` /
    `answerThen`): it misspells the `fix_meta` key `reason` unless the chat
    request it receives carries gonk's `== Your previous answer was refused ==`
    block. Attempt 1 is a real Job whose prompt has no block: the model
    misspells, the real sweep refuses the batch (`parse`), reports
    `gate-failed`, records `RetryFeedback`, fires the re-sling and writes
    nothing. Attempt 2 is prepared by gonk-dispatch's production preparer and
    run as a SECOND real Job in its own namespace: its prompt ends with the
    block, the stub saw the block (and the misspelled key named) in the
    requests real opencode sent it, the model answers correctly, and the real
    sweep applies it as one merge request and clears the feedback.

  Run just it: `make component-jobs-test COMPONENT_RUN='^TestComponentJobsPipefixRefusedAnswer'`.
