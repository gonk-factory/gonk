// Package changejudge holds the two deterministic decisions of the issue -> MR
// workflow `change` (gonk-cuwn plan, docs/plans/2026-10-05-gonk-cuwn-epic-remainder.md,
// part CH-JUDGE):
//
//   - Eligible (section 2.1, rows I1-I8, with the owner's decision 8.1 Q2 on
//     what a trigger is): may a change be dispatched for this issue?
//   - Judge (section 2.5, steps K0-K5): what does the project's own merge
//     request pipeline say about the commit gonk authored?
//
// plus the pure mappings verdict -> change state -> meter outcome -> labels
// (the outcome table of 2.5).
//
// It is PURE: fact structs in, a decision out. No GitLab client, no store, no
// clock except the `now` the caller passes. THERE IS NO LLM HERE and nothing
// in any input comes from a session's answer. It also DECIDES NOTHING ABOUT
// TEXT: whether a note is a slash command, whether a comment mentions the bot,
// which label is the implement label -- the caller matches those syntactically
// and hands over the result as a fact (a Request with its Form and its Actor).
//
// THE ONE RULE EVERYTHING HERE SERVES: a fact that was not read is not a
// fact. Every group of facts carries an explicit "this was read" marker whose
// zero value is "not read", and an unread fact yields NO decision -- Eligible
// answers "unknown, look again", Judge answers DecisionCannotJudge -- never
// eligibility and never a pass. The zero value of every input struct is
// therefore refused, and so is a half-filled one. Past the verify deadline a
// judge-time fact that still cannot be read becomes the verdict UNVERIFIABLE,
// which is not a pass either.
//
// THERE IS NO LOCAL EXECUTION (owner decision 8.1 Q1): the project's merge
// request pipeline is the only verifier. So everything Judge can say is what
// GitLab's pipeline, job and test-report objects say, and the package is
// CONSERVATIVE about each of them:
//
//   - every attempt of every job in every candidate pipeline is evidence; a
//     job that failed and then passed is FLAKY, whoever retried it, and never
//     green;
//   - "the tests ran" is decided from the test report only, by test-case
//     identity (suite, classname, name) counted as a MULTISET, so a duplicate
//     name cannot hide a lost pass; a report that is absent says
//     TESTS_UNOBSERVED, and a report that VANISHED (the base had passing
//     cases, the head reports nothing) says TESTS_REDUCED;
//   - a verdict that would escalate the ladder (NOT_GREEN, TESTS_NOT_RUN) is
//     only ever derived from a pipeline that ran; a truncated report of a
//     canceled or unfinished pipeline proves nothing about what was collected.
//
// What it does NOT decide, because no GitLab fact decides it (plan 2.5, "what
// is and is not decidable"): that the test report is truthful, that the tests
// test the change, that the change does what the issue asks. GREEN is evidence
// for a reviewer. It is never merge authority.
//
// Where the plan left a choice, the reading taken is recorded in the plan's
// evidence log (section 10, "CH-JUDGE"), each with the direction it was
// resolved in and whether the owner should confirm it.
package changejudge
