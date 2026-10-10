// Package mergepolicy is the optional-merge decision of gonk-rk1m (epic
// gonk-cuwn): may the controller press "merge" on a merge request gonk itself
// authored?
//
// Plans: docs/plans/2026-10-05-gonk-cuwn-epic-remainder.md, section 5 (facts
// M1-M11, the two eligible classes, the three modes), and
// docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md, section 9 (facts A1-A9;
// this package is that plan's part MERGE, pure half). Every rule code starts
// with the epic plan's row ("M5.mixed-classes"); ARow maps a row to the fix
// plan's.
//
// It is PURE: Decide(Facts, Config, now) -> Decision. No GitLab client, no
// store, no clock but the `now` the caller passes. THERE IS NO LLM HERE, and
// no field of any input is a session's answer: a review enters only as the
// review FACT the controller recorded (verdict, head SHA, the two model
// names), never as a label and never as text.
//
// WHAT IT NEVER DOES, under any Config:
//
//   - merge when the mode is `off` -- and `off` is the zero value of Config;
//   - merge a diff outside the two classes of EligibleClasses (prose-only
//     documentation, and CI red-to-green). No Config.Classes entry adds a
//     third: `source`, `test` and everything else are refused first;
//   - merge a `fixed-new-failure` fix, or an execution-changing CI change;
//   - count the bot's own approval, or take a review by the author's own
//     model as the review `after-agentic-review` needs. With no distinct
//     reviewer model that mode is UNAVAILABLE (M9.same-model); there is no
//     switch that lets the same model satisfy it;
//   - take a review as authority. A review is one necessary fact among
//     eleven and a veto under every mode; it never makes up for another.
//
// THE ONE RULE EVERYTHING HERE SERVES, as in pkg/pipefix: a fact that was not
// read is not a fact. Every group of facts carries ReadAt, whose zero value
// is "not read". A group that was not read, was read more than MaxFactAge
// before `now`, or claims to have been read after `now`, fails its row
// ("M7.unread", "M7.stale") and the answer is DO NOT MERGE. The zero value of
// Facts is therefore refused, and so is a half-filled one.
//
// Decide evaluates EVERY row, in the plan's order M1..M11, and returns every
// failure. Decision.Rule is the first one: exactly one deciding code. A row
// that needs a fact of another row which was not read does not guess; the
// other row's own failure already blocks.
//
// THE CONFIG INPUT IS A PLAIN STRUCT (Config). The schema that will fill it
// -- `pipeline_fix.auto_merge` / `change.auto_merge` in .gonk.yml, the
// operator veto, `auto_merge.max_per_day` -- is plan part X-CONFIG, not built
// when this package was written. Nothing here reads pkg/gonkcfg.
//
// The merge call itself is MP-WIRE's. What this package fixes about it:
// Decision.SHA is the judged commit, a full 40-hex SHA, set only when Merge
// is true, and the merge request must be accepted with exactly that `sha`.
package mergepolicy
