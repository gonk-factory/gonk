# Wave 1–3 verification: what landed, what it proves, what it broke

- **Date:** 2026-09-10
- **Scope:** `origin/main` at `d823cc8`. Nineteen plan tasks closed (T-01..T-15,
  T-26, T-34, T-37, T-38), T-48 partially landed, T-16 in progress. Reviewed
  against `docs/reviews/2026-09-08-delivery-plan.md` (rev 2) using its own
  §2.4 protocol: four fresh verifier agents, one per area, grading every exit
  criterion from evidence and re-running the named tests. Every P1 below was
  then re-checked by hand.
- **Not reviewed:** T-16 (no branch visible), the gitops repo, the live
  cluster.

---

## 1. Verdict

Sixteen of nineteen closed tasks are all-pass on their exit criteria. The
craftsmanship is good: tests assert values, revert spot-checks show they are
load-bearing, scope fences were respected, and the implementers filed
seventeen follow-up beads from what they found. Three things are wrong enough
to stop wave 4 until fixed:

1. **GitHub CI is red on every push to `main` since T-15 merged** (`ci`
   workflow, component job: `TestMeasureLiteLLMSpendLogLag` fails on a
   100-second lag). The hard-door and meter-SIGKILL tests inside that job
   pass, but nobody can see it. Confirmed on run 16 (`d823cc8`).
2. **The `secrets` workflow has never been green, and the leaked `sk-` literal
   is still reachable by a named ref.** The GitHub tag `v0.1.0` still points
   at the pre-rewrite lineage, `actions/checkout` fetches all tags, so
   gitleaks scans 713 commits including the two old blobs. The handoff's
   "tag was re-pointed" and "fresh clone scans clean" are true for the GitLab
   origin only. Confirmed on run 5 (`d823cc8`).
3. **T-13 introduced a regression worse in visibility than the bug it
   fixed.** The deferred release re-writes the record with `State =
   pending-prompt` and only `SessionID` cleared, so *any* failed re-dispatch
   (transient prompt PUT, key read, session create) strands the bead in a
   state neither sweep pass lists. The filed bead `gonk-ihj6` says only a
   hard crash can cause this; that is wrong.

A fourth item is a live-cluster gap rather than a code defect: **T-26 does not
close R-48 on the deployed Dolt.** The pinned Dolt entrypoint creates users
only `IF NOT EXISTS` and never alters them, so the existing passwordless
`root@'%'` on the PVC survives the upgrade. An operator has to drop it by
hand, and no task or runbook says so.

---

## 2. Per-task grades

| Task | Verdict | What is not clean |
|---|---|---|
| T-01 reaper | all-pass | new tests in T-05/06/07 keep adding nonce-free aliases (`gonk-plvm` scope grew) |
| T-02 entrypoint alias | all-pass | exit code 5 documented only in the script |
| T-03 prompt fetch retry | all-pass | — |
| T-04 no-latest | partial | criterion 3 (`go test ./internal/buildgate/` green) fails only because T-14's test is red by design |
| T-05 labels | all-pass | `e.Remove` unvalidated (nothing applies it yet) |
| T-06 comments | all-pass | plan named `CreateDiscussionNote`, which never existed; skipped silently |
| T-07 transcript | partial | criterion 2 not done: L1 has no session/transcript route to enlarge; not reported |
| T-08 onboarding seed | partial | criterion 5: `gonk-bgx`/`gonk-msz` closed with no pointer; stale "pending gates triage" text in spec §1 and `broker_inject.go:54` |
| T-09 delete formulas | partial | criterion 4: bead-closing script exists but no count was produced; `pack.toml` still advertises mention replies |
| T-10 delete dead Go | all-pass | **P2** onboarding MR still promises commit trailers (`gonk-92jq`), `commit_trailers` still a live schema key |
| T-11 opercfg guards | all-pass | — |
| T-12 archived deregister | all-pass | **P2** deregisters every archived project on every pass forever |
| T-13 dedupe window | partial | **P1** regression (§1 item 3) |
| T-14 build tags | partial | `RequireInfra` not used by `test/component`; two data-absence skips remain; `-skip` in Makefile and CI instead of the planned `-run` exclusion (equivalent) |
| T-15 L2 on GitHub | partial | **P1** component job red since merge; bead links no run |
| T-26 Dolt interim | all-pass on criteria | **P1** live `root@'%'` survives; **P2** `gc` password rotation is a no-op; deployment near-miss (§4) |
| T-34 prompt key scrub | all-pass | `ExpirePrompts` count discarded, unlike its sibling |
| T-37 quiet hours | partial | criterion 1: the spring-forward 03:00–04:00 probe row is missing; L1 defer test pre-existed; **P3** ambiguous fall-back end time can yield a `RetryAfter` in the past |
| T-38 intake hygiene | all-pass | — |
| T-48 secrets (partial) | partial | criterion 1 fails (§1 item 2) |

Local gate on `main`: `go build`, `go vet`, and `go test ./...` pass except
`TestEveryBuildTagRunsInCI`, which is red by T-14's design and skipped by
both `make test` and CI with a dated TODO. That skip is a standing exception
that T-16, T-17 and `gonk-0bvc` must remove.

---

## 3. Findings by severity

### P1

| ID | Finding | Where | Fix |
|---|---|---|---|
| W-01 | `ci` workflow red on `main` since T-15: `TestMeasureLiteLLMSpendLogLag` asserts a measurement, not an invariant, and fails at 100 s | `test/component/litellm_test.go`; run 34444900094 | Make it a recorded measurement with no assertion, or assert against a documented budget the runner can meet. Do not skip it. |
| W-02 | `secrets` workflow never green; leaked literal reachable via `refs/tags/v0.1.0` on GitHub | run 34444900123; `docs/HANDOFF-next-session.md:806` overstates | Force-push the re-pointed tag to GitHub (or delete and recreate it), re-run, then close T-48. Correct the handoff line. |
| W-03 | T-13 release path strands any failed re-dispatch in `pending-prompt` | `cmd/gonk-gate/broker_inject.go:735-737` | Release must re-Put the *prior* record (state and session as they were), and `runSweep` should list `StatePendingPrompt` with an age cutoff so the crash case is also reclaimed. Rewrite `gonk-ihj6` accordingly. |
| W-04 | T-26 does not fix the deployed Dolt: `root@'%'` with no password persists across the upgrade; only fresh volumes get the new users | pinned Dolt entrypoint (`v2.1.7`) | Operator runbook: `DROP USER 'root'@'%'` and set the root password once on the live server; record in the handoff. Chart cannot do this. |

### P2

| ID | Finding | Where | Fix |
|---|---|---|---|
| W-05 | Every archived project is deregistered on every reconcile pass forever (meter DELETE, Secret delete, DB delete, `MeterPush("ok")` each time; `Errors++` per project per pass when meter is down) | `pkg/intake/reconcile.go:721-745` | Tombstone in the cache after a successful deregister, or `GET` the registration first and skip when absent. Add a two-pass test. |
| W-06 | Onboarding MR still promises commit trailers T-10 deleted; `commit_trailers` remains a validated key | `pkg/intake/render.go:71-74,167-168`; both schemas | Remove the promise from the template and explanation; deprecate the key (accept, ignore, document). `gonk-92jq` is correctly filed; it is small and should land before any project is onboarded. |
| W-07 | `gc` user password rotation is a no-op: the entrypoint only `CREATE USER IF NOT EXISTS` | Dolt entrypoint | Document that rotation requires an `ALTER USER` by hand; or drop Dolt at T-56 and accept the gap until then. |
| W-08 | `testclock` is a modifier tag, not a suite; no task covers it, so the `-skip` on `TestEveryBuildTagRunsInCI` can never be removed as written | `cmd/gonk-meter/clock.go`; `gonk-0bvc` | Treat modifier tags as covered when `go vet -tags` compiles them (option b in `gonk-0bvc`). |
| W-09 | T-07 criterion 2 was not implementable (L1 has no transcript route) and was neither done nor reported; the sweep still loops on a >4 MiB transcript until the reservation expires | `test/integration/main_test.go:685`; `cmd/gonk-gate/sweep.go` | Amend the criterion (below); make the sweep treat `IsTranscriptTooLarge` as a terminal `infra-failed`. |
| W-10 | T-26 deployment near-miss: the default `secrets.dolt.existingSecret: gonk-dolt` means the guard would *not* have fired; Flux would have rendered, the single-replica StatefulSet would have rolled, and the new pod would have hung on `FailedMount` with the beads store down | `chart/gonk/values.yaml:453-458`; `_guards.tpl:99-102`; `gonk-qrsy` | Caught by a human reading the gitops repo 18 minutes after merge. The bead's stated mechanism ("guard refuses to render") is wrong. Add a gitops-side check to §2.3 (below). |

### P3 (grouped)

- Stale text after deletions: spec §1 item 3 still says triage waits for
  `.agent/`; `broker_inject.go:54-56` same; `pack/pack.toml:27` advertises
  mention replies; `contract_test.go:27` "pour a formula"; `Dockerfile.agent:15`
  "needs trailers/check".
- Tests that pin absence by grep rather than behaviour:
  `TestDispatchSourceNeverBuildsLegacyFormulaCredentialVars` (T-09);
  `TestDoltIsNotRootOpen`'s `value: "%"` check is decorative;
  `TestDoltSecretGuardFailsClosed` asserts `err != nil` only.
- No negative test pins the deleted `GONK_LITELLM_KEY` fallback (T-10).
- `IsTranscriptTooLarge` is true for any route's cap error (T-07).
- `require_nonzero_go_tests.sh` counts `--- SKIP` as ran (T-15).
- Fall-back DST: `wallClockOn` resolves an ambiguous wall time to the first
  occurrence, so a window ending inside the repeated hour can produce a
  `RetryAfter` already in the past (T-37).
- `.agent/` read failure on a transport error silently yields the thinner
  prompt (T-08, `broker_inject.go:800-805`).
- `TestBaseImagesArePinnedByDigest` still globs only `Dockerfile.*` (T-04).
- Reaper's `claimedSessionIDs` ignores `pending-prompt`; safe only because
  the 10-minute grace exceeds the 240-second window (T-01 × T-13).
- helm 3.16 locally fails two charttest cases that pass on CI's 3.19; pin or
  document.

---

## 4. Process observations

These matter more than any single finding, because they decide whether the
next thirty tasks can be trusted.

1. **Bead close notes carry no evidence.** Every closed task's `close_reason`
   is the bare string `Closed`. The merge commit bodies say "merged after
   independent verification", and for T-15 that verification cannot have
   observed a green component job because there has never been one. The
   verifier's per-criterion table needs to be *in* the bead, or a reviewer
   cannot distinguish "verified" from "asserted".
2. **Plan phantoms were absorbed silently.** T-06 named a function that never
   existed; T-07's criterion 2 named a test route that never existed. Both
   implementers worked around it without saying so. The plan's §2.3 says to
   STOP and report when a task cannot be completed as written; it should say
   the same when the task is *wrong* as written.
3. **The near-miss (W-10) was outside every gate.** Chart tests prove the
   render; nothing in the repo can see whether the deployed cluster has the
   Secret a new default references. It was caught by a person reading another
   repository. Any task that adds a required Secret or changes a default that
   Flux will apply needs a gitops-side check in its Verify step.
4. **Green claims about CI were not checked against CI.** Two workflows have
   been red on `main` for the whole of waves 2–3 and no bead links a run.
   Rule for the orchestrator: a task whose criterion says "CI run shows X"
   closes only with the run URL in the close note.
5. **Deletions leave promises behind.** T-09 and T-10 removed features and
   left user-facing text (the pack description, the onboarding MR) promising
   them. A grep for the feature's name outside `cmd/` belongs in every
   deletion task's Verify.

---

## 5. Actions before wave 4

In order. The first three are blocking.

1. **Fix W-01** (component lag test) and **W-02** (GitHub tag) so both
   workflows are green on `main`. Close T-48 with the run URL.
2. **Fix W-03** (T-13 release path plus sweep listing `pending-prompt`).
   Add the parked-bead re-dispatch failure as a test. Rewrite `gonk-ihj6`.
3. **Operator runbook for W-04 and W-07** in the handoff, executed on the
   live Dolt, with the `SHOW GRANTS` output recorded.
4. Land W-05 (tombstone) and W-06 (`gonk-92jq`) as small sonnet tasks; both
   are S.
5. Apply the plan amendments below, then resume wave 3's remaining tasks.

### Plan amendments (applied in this commit)

- §2.3 rule 2 extended: if the task names a function, file, route or test
  that does not exist, STOP and report; do not substitute.
- §2.3 rule 9: the report's exit-criteria lines must be pasted into the
  bead's close note by the orchestrator, with the verifier's grades.
- §2.3 new rule 10: a criterion phrased "CI run shows …" is satisfied only
  by a run URL in the report.
- §2.3 new rule 11: a task that adds a required Secret, changes a chart
  default, or changes anything Flux applies must state the gitops-side
  prerequisite in its report and in the chart README; the orchestrator
  checks the gitops repo before merging.
- T-07 criterion 2 replaced: "the sweep classifies a transcript above the
  cap as terminal `infra-failed`, not a retry loop (test)".
- T-12 gains criterion 2: "a second reconcile pass after a successful
  deregistration issues no meter call (test)".
- T-13 gains criterion 3: "a failed re-dispatch of a parked or running bead
  leaves the record in its prior state (test); `runSweep` reclaims
  `pending-prompt` records older than the delivery window (test)".
- T-26 gains a note: the chart cannot alter existing Dolt users; the live
  server needs a one-time `DROP USER 'root'@'%'`; rotation of `gc-password`
  requires `ALTER USER` by hand until T-56.
- T-58 gains: remove the `-skip` on `TestEveryBuildTagRunsInCI` from the
  Makefile and CI once `images`, `live` and the modifier-tag rule
  (`gonk-0bvc`) are covered.
