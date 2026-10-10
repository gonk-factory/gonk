// Package pipefix holds the two deterministic decisions of the pipeline-fix
// workflow (gonk-2usw plan, docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md):
//
//   - Eligible (section 3.2, rows E1-E12): may a fix be dispatched for this
//     diagnosed run?
//   - Judge (section 6.3, steps J0-J5): does the fix merge request meet the
//     exit criterion "the SPECIFIC diagnosed failure no longer recurs"?
//
// plus the pure mappings verdict -> fix state (7.1) -> meter outcome (7.3).
//
// It is PURE: fact structs in, a decision out. No GitLab client, no store, no
// clock except the `now` the caller passes. THERE IS NO LLM HERE and nothing
// in any input comes from a session's answer -- every fact is one the
// controller recorded or read from GitLab.
//
// THE ONE RULE EVERYTHING HERE SERVES: a fact that was not read is not a
// fact. Every group of facts carries an explicit "this was read" marker whose
// zero value is "not read", and an unread fact yields NO decision -- Eligible
// answers "unknown, look again", Judge answers DecisionCannotJudge -- never
// eligibility and never a pass. The zero value of every input struct is
// therefore refused, and so is a half-filled one. Past the verify deadline a
// judge-time fact that still cannot be read becomes the verdict UNVERIFIABLE
// (plan 6.3: "a read failure is never a verdict; bounded by the deadline"),
// which is not a pass either.
//
// Judge is CONSERVATIVE ACROSS PIPELINES AND ATTEMPTS: every attempt of a
// diagnosed job, retried ones included, in every candidate pipeline is
// evidence. One recurring attempt anywhere outweighs a pass elsewhere, and a
// retry (gonk never retries; a human may) cannot launder a failure into
// PASSED.
//
// A pipeline that RAN NOTHING is not evidence and not the end of the wait: a
// `[skip ci]` commit leaves pipelines that are `skipped` with no job object,
// and Judge sets them aside (Result.Skipped) instead of counting them as
// candidates. Until a pipeline that runs something exists the answer is
// DecisionWait; at the verify deadline it is UNVERIFIABLE (J1.TIMEOUT).
//
// A pipeline's STATUS is not the end of the wait either. GitLab can report a
// pipeline `failed` while an automatic retry of the failed job is still
// `created` and has not run (bead gonk-5b2f). A candidate is settled only
// when its status is terminal AND none of its job attempts is still active
// (ActiveJobStatuses); otherwise the answer is DecisionWait until the verify
// deadline. This only ever withholds a verdict.
//
// "Pipeline green" is not the criterion. It appears only in FIXED's last
// clause; FIXED_NEW_FAILURE is the criterion met on a red pipeline.
//
// Job names: a diagnosed job is matched to live job objects by EXACT name.
// The record holds the canonical single-line form of a job name (FACTS,
// plan section 17), so the caller must put every live job name through the
// same canonicalisation before filling these structs. A mismatch fails closed
// (the job reads as absent: UNVERIFIABLE), never open.
package pipefix
