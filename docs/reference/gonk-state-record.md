# The gonk-state record (`pkg/beadstore`)

gonk-gate keeps one small record per work item between a dispatch and its
outcome. This is its schema, how it is stored (in Postgres since gonk-t41; in
bd before that), and the rules every reader and writer relies on. The Go type
is `beadstore.Record` (`pkg/beadstore/store.go`);
`pkg/beadstore/storetest/suite.go` is the executable form of the contract and
runs against every implementation (`Run`: Memory, BdCLI, Postgres;
`RunCompareAndSwap`: Memory, Postgres -- BdCLI fails it, see "Storage in bd").

Last changed: 2026-10-12, gonk-37kg.1 / gonk-hpgy (`RetryFeedback` is now also written for `issue-triage` and `scaffold` records, with the codes of their gates; the diagnosis prompt's block states the failed job names and the fact keys; no field added, work store schema version unchanged, no migration). Before that 2026-10-07, gonk-nf9i / gonk-zxsx (added the `RetryFeedback` object; section "Retry feedback"; additive JSON, work store schema version unchanged, no migration). Before that 2026-10-05, gonk-2usw part FACTS (added six failure-fact fields to each `PipelineJobs` entry and the `PipelineDiagnosis` object; section "Pipeline failure facts"; additive JSON, work store schema version unchanged at `3`, no migration). Before that 2026-10-04, gonk-tios P7 (added the `Pipeline*` fields, pipeline-diagnosis records only). Before that 2026-10-03, gonk-qdr4 (added `CheckoutSHA`). Before that 2026-09-13, gonk-p8w8 (added `Revision`, `ReslungAt`,
`ReslingFireBy`, the `IfRevision` envelope field, and the resolution rule).
2026-09-14: `ReslingFiredAt` (never deployed) replaced by `ReslingFireBy`.
2026-09-14: gonk-t41 -- the record moves to the `work_items` table in the
meter's Postgres (section "Storage in Postgres"); `PutIf` success is final.

## Fields

JSON field names are the Go field names (the struct has no json tags). Times
are RFC 3339 with nanoseconds; the zero time is `0001-01-01T00:00:00Z` and
means "unset".

| Field | Type | Written by | Meaning |
|---|---|---|---|
| `BeadAnchor` | string | every writer | `gonk:{project_id}:issue:{iid}`; for a pipeline-diagnosis record the RUN anchor `gonk:{project_id}:pipeline:{mr<iid>\|ref-<8 hex>}:{fp16}:s{last_success}` (gonk-tios D2), with the suffix `:f{fix_mr_iid}` when the run follows a merged gonk pipeline-fix MR (gonk-2usw; "Fix epoch" below). The key: one record per anchor. |
| `BeadID` | string | dispatch | Gas City bead id, for the marker/record only. Never sent to meter. |
| `Project`, `ProjectID`, `Rig`, `SessionKey`, `Trigger`, `IssueIID`, `ConfigHash` | | dispatch | Copied from the webhook args. |
| `State` | string | every writer | `running`, `parked`, `needs-human`, `done`, `pending-prompt`. |
| `Rung`, `Model`, `Attempt` | | dispatch | From meter's `/decide`. |
| `ReservationID`, `ReservationExpiresAt` | | dispatch | Meter's reservation, verbatim. The sweep binds its outcome to the id and uses the expiry as the session deadline. A zero expiry is NOT "never": the sweep bounds such a record at `UpdatedAt` + the default `reservation_ttl` (60m), and one with no `UpdatedAt` either as already expired (gonk-72np, `runningDeadline`). |
| `RetryAfter` | time | dispatch | When a `parked` record may be unparked. |
| `SessionEndedAt` | time | sweep | v1 path only in practice; nothing stamps it for broker records (gonk-u1p.5). |
| `SessionID` | string | dispatch | The session alias. Set on the `pending-prompt` reservation, before the session exists. |
| `CheckoutSHA` | string | dispatch (the `running` write) | Full commit id the session's checkout was pinned to at grant time (gonk-qdr4): the exact tree the agent was shown. `""` when no checkout was granted (shape needs none, rig unconfigured, ref unresolvable) and on records written before 2026-10-03. Additive JSON field, no migration. |
| `PipelineID`, `PipelineTarget`, `PipelineFingerprint`, `PipelineLastSuccess` | int64, string, string, int64 | dispatch (every write: park, deny, reservation, running) | pipeline-diagnosis only (gonk-tios D12): the run identity from the order -- the pipeline the session diagnoses, `mr:<iid>` or `ref:<name>`, the failing-job fingerprint and the target's last green pipeline (0 = none). `BeadAnchor` must equal `pipelinectx.Anchor(ProjectID, target, fingerprint, last_success, fix_epoch)` or dispatch refuses the order (`fix_epoch` is `PipelineFixEpoch`, next row). `PipelineID` is RECORDED: once a record names one, every later attempt and re-sling of the anchor keeps it (the session name is `gonk-pipeline-p<pid>-pl<PipelineID>-a<n>`). Zero on every other record. |
| `PipelineFixEpoch` | int64 | dispatch (every write: park, deny, reservation, running) | pipeline-diagnosis only (gonk-2usw part EPOCH): the fifth part of the run identity, from the order var `pipeline_fix_epoch` -- the iid of the merged gonk pipeline-fix MR this run FOLLOWS, i.e. the `:f<iid>` suffix of `BeadAnchor`. `0` = the run follows no merged fix and its anchor has no suffix. Stored with `omitempty`: a record whose epoch is 0 has NO `PipelineFixEpoch` key and is byte-identical to one written before the field existed. Specified in "Fix epoch" below. |
| `PipelineSHA` | string | dispatch (the `running` write) | The diagnosed pipeline's commit. The checkout is pinned to it (`CheckoutSHA`, when granted) and the sweep checks every cited file exists AT it -- with or without a checkout, hence its own field. |
| `PipelineProjectURL` | string | dispatch (the `running` write; carried over by every later dispatch of the run); the sweep fills an empty one | The project's WEB url, no trailing slash, e.g. `https://gitlab.example/group/project` (gonk-385g). Every controller-written run note and the tracking issue's description link what they name with it: `<PipelineProjectURL>/-/pipelines/<id>` and `/-/jobs/<id>` (job ids from `PipelineJobs`). DERIVED by the controller from the `web_url` of GitLab's own pipeline object -- which must be exactly `<project>/-/pipelines/<that pipeline's id>` -- never from the order, the configured API base URL (it may be an in-cluster address) or model output; stored only if it is an http(s) URL with a host made of letters, digits and `._~-:/`, and re-checked at every render (`safeProjectURL`). `""` on records written before 2026-10-05, and on a `parked`/`needs-human` record whose dispatch read no pipeline: a note then names its pipeline in plain text, never with a broken link. (That dispatch still links its own deferred/denied note from one best-effort pipeline read made AFTER the record write; the URL it finds is not written back.) Run maintenance fills an empty one from the pipelines it observes. Additive JSON field, no migration. |
| `PipelineJobs` | list of `{ID, Name, Excerpted, ExcerptSHA256, Groundable, Stage, FailureReason, AllowFailure, ErrorSignature, ErrorGeneric, SignatureVersion}` | dispatch (the `running` write) | Every failed job (status failed, allow_failure false) of the diagnosed pipeline, in prompt order: whether an excerpt of its log was shown, the sha256 of that excerpt, and whether it holds a quotable line. The sweep validates evidence job names against this list (never the batch) and re-derives each excerpt to compare the hash (D8 step 5). The last six fields are the job's FAILURE FACTS (gonk-2usw); each is specified in "Pipeline failure facts" below. |
| `PipelineExcerptVersion`, `PipelineExcerptMaxBytes` | int, int | dispatch (the `running` write) | `pipelinectx.ExcerptVersion`, and the ONE per-job byte cap every excerpt of the dispatch was built with (after the 28 KiB prompt fit). Recorded, not re-derived: the fit depends on every other block's size (P6). A version this build does not have makes the check UNKNOWN. |
| `PipelineFirstFailed`, `PipelineLatestFailed` | int64, int64 | dispatch; `PipelineLatestFailed` also the sweep | The run's first failing pipeline as dispatch saw it (0 = unknown), and the newest failing pipeline of the run seen so far (advanced by run maintenance while the run stays red with the same failing jobs, and by a dispatch fired by a later failure of the same run). Shown in the run note and the tracking issue. |
| `PipelineRunOpen` | bool | dispatch sets it (park, deny, running); the sweep clears it | The run has not ended. Cleared when the target goes green (resolved), a different failing-job set begins (superseded), or the MR target is merged/closed. Run maintenance reads only open runs (`Store.ListOpenPipelineRuns`) and acts on those in `done`, `needs-human` or `parked`. A `parked` run that resolves is moved to `done` so it is never unparked. |
| `PipelineCheckedAt` | time | sweep | When run maintenance last looked at this run; it looks at most every 5 minutes. |
| `PipelineRunFixMR`, `PipelineRunFixState` | int64, string | sweep (the pipeline-fix exit check and post-merge confirmation) | pipeline-diagnosis only (gonk-2usw part VERIFY): the fix merge request gonk opened for this run and the fix state last written to the run note's fix line. Absent until a fix merge request exists. See "Pipeline fix". |
| `PipelineDiagnosis` | object `{Class, Files, EvidenceJobs, Summary, Remedy}` | sweep (the write that follows a LANDED diagnosis; carried unchanged by every later sweep write of the record) | The diagnosis the sweep validated and wrote to the run's landing (gonk-2usw). All-zero (`Class == ""`) when none has landed, and on records written before 2026-10-05. Each field is specified in "Pipeline failure facts" below. |
| `UpdatedAt` | time | **the store** | When the store last wrote the record. Callers never set it. Exception: a `pending-prompt` record that already has one keeps it (a reservation's age runs from when it was reserved). |
| `Revision` | string | **the store** | 32 hex chars, random, new on EVERY write (including the `pending-prompt` exception above). Opaque: compare for equality only. `""` on records written before 2026-09-13. |
| `ReslungAt` | time | sweep; carried back verbatim by dispatch's release | When the sweep reported this record's outcome and fired the re-sling. Marks the record as already judged; see `ReslingFireBy` for how long that holds. Dispatch never sets it, but a failed dispatch restores the record it found, `ReslungAt` included. |
| `ReslingFireBy` | time | sweep, in the same write as `ReslungAt` (before firing); carried back verbatim by dispatch's release | `ReslungAt + 240s`: the deadline passed to the re-sling's dispatch as the `fire_by` order var. A gonk-dispatch that starts after it exits before doing anything. A broker record is not judged again until `ReslingFireBy + 300s` (dispatch timeout) `+ 2s` (gc's SIGTERM-to-SIGKILL grace) `+ 5s` (wall-clock margin). That bound holds however late, or how many times, gc fires the order: it assumes only that gc delivers its timeout kill, and a wall clock that does not step more than 5s between the sweep and dispatch processes (same container, same kernel clock). |
| `RetryFeedback` | object | sweep, in the write that records the re-sling; carried onto every record dispatch writes for the anchor (`pipeline-diagnosis`, `pipeline-fix`, `issue-triage` and `scaffold`); cleared by the sweep when an answer is applied | What the NEXT attempt's prompt is told about an answer a static gate refused. Absent (`omitzero`) when there is none. See "Retry feedback". |

## Retry feedback (gonk-nf9i, gonk-zxsx, gonk-37kg.1, gonk-hpgy)

`pipeline-diagnosis`, `pipeline-fix`, `issue-triage` and `scaffold` records
(the last two since gonk-hpgy). Before this an answer a
static gate refused -- no fenced batch, unparseable JSON, a shape or rule
refusal -- was retried with a byte-identical prompt. On dev the fix agent
misspelled one JSON key in 5 of 8 sessions; the retry was told nothing.

| Field | Type | Meaning |
|---|---|---|
| `Attempt` | int | The attempt whose answer was refused, from 1. |
| `Code` | string | The refusal's stable code: `no-batch`, `parse`, a fix rule code with its sub code (`FX-SHAPE(reason-empty)`, `SC-SHOWN(SC-CITED)`, ... -- the same string as `PipelineFix.Refusal`), or a diagnosis refusal's cause (`shape`, `target`, `diagnosis_invalid`, `evidence_ungrounded`, `cited_file_missing`, `would_forge_marker`, `would_post_secret`, `quick_action`, `trajectory`), or the gate of the issue-shaped chain that refused a triage or scaffold answer (`shape`, `target`, `path`, `comment`, `trajectory`, `label` -- the word the violation is prefixed with). |
| `What` | string | One controller-built sentence: what was wrong. |
| `Allowed` | string | The exact allowed set for whatever `What` names (the keys of the effect kind concerned, the fact keys, the classes, the per-kind counts); empty when the refusal is not about a set. |

**Who writes it.** `applyBrokerBatch` (for every wired agent: no batch, a
batch that does not parse, and each gate of the issue-shaped chain) and the two
pipeline apply chains set it on the
record at the point of refusal; the sweep's re-sling write persists it (the
same `PutIf` that stamps `ReslungAt`). A failure of OURS (an unknown: a read
that failed, a context that no longer hashes) writes nothing. An answer that
is APPLIED clears it. gonk-dispatch copies it from the record it read before
`/decide` onto the pending-prompt and running records, so it survives a
controller restart at any point and the re-dispatch renders the same block.

**What it may hold.** The refused answer is untrusted model output, so no
string here is copied from it. `What` and `Allowed` are built by
`pkg/effects` (`ExplainParse`, `ExplainDiagnosis`, `ExplainFix`,
`ExplainShape`) and by fixed sentences in `cmd/gonk-gate/retry_feedback.go`
from: fixed text; values of closed sets the controller owns (kinds, classes,
fact keys, rule codes, the JSON keys of `effects.Effect`); counts and
positions the controller computed; and AT MOST ONE `effects.Token` -- the
offending name (a misspelled key, an unknown kind, class, verdict or fact
key), cut to 32 bytes, every byte outside `[A-Za-z0-9_.-]` replaced by `?`,
between double quotes, with ` (cut)` after the closing quote when it was cut.
No free-text field of the answer (summary, remedy, reason, `old`,
`new`, a quoted log line, a path, a job name, a label, a comment) and no error
or `Refusal.Detail` text is ever read into it.

For `issue-triage` and `scaffold` (gonk-hpgy) the gates of the issue-shaped
chain report errors that quote the batch -- the label, the path, the comment
line -- and what a triage session read is an issue anyone may have written. So
each of those gates has ONE fixed sentence (`issueGateFeedback`): `target`,
`path`, `comment`, `trajectory`, `label`. `shape` uses `ExplainShape` (the
per-kind counts of the agent's shape file). A misspelled key is named by its
`Token`, but NO key list follows it: `comment`, `label` and `file` have no
per-kind key table in the validator, and none is invented for the feedback. A
sentence of `pkg/effects` that points at the pipeline prompts' `OUTPUT`
heading is recorded as pointing at "this message" instead, because the triage
and scaffold prompts have no such heading.

**How it is rendered.** `renderRetryFeedback` appends this block to the END
of the fix prompt, the diagnosis prompt, the triage prompt and the two scaffold
prompts that ask for a batch (not the one that tells the session to emit none,
when no repository material could be fetched), only when `Attempt` is lower
than the attempt being prompted:

```
== Your previous answer was refused ==
gonk checked the answer of attempt <Attempt> of this task and refused it; nothing of it was used.
refused by: <Code>                   (only when Code is set)
what was wrong: <What>
what is allowed: <Allowed>           (only when Allowed is set)
the failed jobs are, each name between double quotes: "<name>", "<name>"   (diagnosis prompt only)
the fact keys are: <key>, <key>, ...                                       (diagnosis prompt only)
gonk wrote the lines above from its own checks. Answer again in full, with this corrected.
```

Each field is filtered AGAIN at render (`feedbackText`): every byte that is
not printable ASCII, and each of `` ` `` `<` `>` `=`, becomes `?`, and the
field is cut to 64 (`Code`), 200 (`What`) or 200 (`Allowed`) bytes. The block
is therefore at most 768 bytes on six lines whatever the record holds; the
fix and diagnosis prompts' worst cases are budgeted for it. The triage and
scaffold prompts have no whole-prompt cap; the block adds at most 770 bytes
to them.

**The diagnosis block's closed sets (gonk-37kg.1).** On dev a diagnosis named
its failed job with two digits of the name swapped, was told the name must be
"copied exactly from its heading", and swapped two other digits on the retry.
So the DIAGNOSIS prompt's block, whatever the refusal was, ends with the two
closed sets an evidence item chooses from. Neither line is on the record; both
are built at render from what the gate reads:

- **The job names** are `effects.DiagnosisContext.JobNames()` of the context
  of the dispatch being prompted -- the enumeration `ValidateDiagnosis` builds
  the set of rule `evidence-job` from, over the jobs this dispatch writes to
  `PipelineJobs`. So the names shown are the ones THIS attempt's answer will be
  checked against. A job name is project text, not model output: the prompt
  already shows it, in this canonical form (`jobNameText`), in the job's
  heading. A name is shown WHOLE AND UNCHANGED OR NOT AT ALL, and only when
  it is ONE TOKEN: every byte an ASCII letter, a digit or one of `-` `_` `.`
  `:` `/`. The line stands above "gonk wrote the lines above from its own
  checks", and a job named like a sentence must not read as the controller's
  text; so a name with a space, a matrix suffix (`test: [go, 1.22]`), a quote
  or any other byte, and one that does not fit the line, are counted --
  `; N more, named in the headings under Failed jobs` -- never cut, escaped
  or repaired. What a shown token spells is still the project's choice. The
  line is at most 320 bytes; a name longer than
  about 210 bytes is therefore never shown. With no failed job the line reads
  `the failed jobs are: none; the only evidence item this pipeline takes is the fact pipeline_error, without "job"`.
- **The fact keys** are `effects.FactsText()`, built from the list the gate's
  set is built from (`factOrder`). The line is left out when the refusal's own
  `what is allowed` already is that line (an unknown fact key).

`TestDiagnosisFeedbackSetsAreTheGates` fails if either set shown differs from
the set the gate accepts. The gate itself is unchanged: a job name is compared
exactly, and nothing near a name is accepted or corrected. With both lines the
diagnosis block is at most 1206 bytes (768 + 321 + 117);
`TestPipelinePromptFitsWithFourMaximalJobs` holds a prompt at every block cap
with a block of that size inside the 28 KiB prompt cap (headroom 31 bytes).

**Hashing.** For a fix the block is part of the prompt, so it is covered by
`FixContext.PromptSHA256` and through it by `PipelineFix.ContextSHA256`, as
the rest of the prompt is. A diagnosis record holds no hash of its whole
prompt (only each excerpt's), so there the block is covered by nothing but
this record field itself.

**Not wired:** the `mention` agent, which is not dispatched. A triage or
scaffold answer refused because of a fault of OURS (the forge would not take
the note or the commit) is an unknown, not a refusal, and leaves no feedback,
as for the pipeline agents.

## Pipeline failure facts (gonk-2usw part FACTS)

Pipeline-diagnosis records only. These are the controller-held facts a
pipeline-fix dispatch and its exit check read (plan
`docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md` sections 3.2 and 6.1).
Before them the error signature existed only as a function and the diagnosis
only as a rendered note. NOTHING in this section is taken from a fix session's
batch, and the job facts are not taken from the diagnosis session's batch
either.

### Per failed job: the new fields of a `PipelineJobs` entry

Written by dispatch in the `running` write, in the same statement as the five
older fields, from GitLab's job object (`GET pipelines/:id/jobs`, latest
attempt) and from the job's log excerpt.

| Field | Type | Source | Meaning |
|---|---|---|---|
| `Stage` | string | GitLab job `stage`, as returned | The job's stage. NOT canonicalised (unlike `Name`): it is compared with GitLab's string again, never rendered from here. |
| `FailureReason` | string | GitLab job `failure_reason`, as returned | e.g. `script_failure`, `runner_system_failure`. |
| `AllowFailure` | bool | GitLab job `allow_failure` | Always `false` today, because `PipelineJobs` lists only the jobs that failed the pipeline (status failed AND allow_failure false). Recorded anyway so "this job was required" is a stored fact the exit check compares against (plan 6.3 J3 `WEAKENED`), not an inference from the list's membership rule. |
| `ErrorSignature` | string | `pipelinectx.ErrorSignature` of the excerpt's first error line | `es<SignatureVersion>-<16 hex>`. `""` when the job has no excerpt (`Excerpted` false: beyond the first four failed jobs, or its log was unreadable at dispatch) or its log has no line matching the error pattern. The first error line is found before the excerpt caps select what is shown, so the value does not depend on `PipelineExcerptMaxBytes`. |
| `ErrorGeneric` | bool | `pipelinectx.GenericFirstError` of that line | `true` when the first error line is the runner's own job-failed summary (`ERROR: Job failed: exit code <n>`, `... exit status <n>`, `... command terminated with exit code <n>` -- the closed list in `pipelinectx.GenericErrorLine`) or when there is no signature at all. Such a signature identifies an exit status, not a failure. Recorded because it cannot be recovered from `ErrorSignature`, which is a hash. |
| `SignatureVersion` | int | `pipelinectx.SignatureVersion` at dispatch | The canonicalisation the signature was built with (also the digit in its `es<n>-` prefix). It doubles as "the failure facts were recorded": it is `0` on a job recorded before these fields existed. |

Readers use two methods rather than the raw fields:

- `PipelineJob.FailureFactsRecorded()` is `SignatureVersion != 0`. When false,
  `Stage` and `FailureReason` are `""` because nobody wrote them, not because
  GitLab said so; such a job has no failure key to judge a fix against.
- `PipelineJob.GenericFailure()` is `ErrorSignature == "" || ErrorGeneric`: the
  plan's "generic signature". A job of an old record therefore reads as
  generic, the conservative side.

Derived, never stored:

| Name | Definition |
|---|---|
| job failure key | `pipelinectx.JobFailureKey{Stage, Name, FailureReason, ErrorSignature}`; two keys are the same failure of the same job iff they are `==`. `Name` is the record's canonical single-line job name, so a key built from a live GitLab job must canonicalise the name the same way first. Its string form (`String()`) is the four fields, each Go-quoted in ASCII, joined by single spaces. |
| target set `T` | the `PipelineJobs` entries whose `Name` is in `PipelineDiagnosis.EvidenceJobs` and whose `AllowFailure` is false. |
| cause key | `pipelinectx.CauseKey(keys)`: the first 16 hex of sha256 over `"pipelinectx-cause-key\x00"` followed by the sorted, de-duplicated string forms of the keys of `T`, each terminated by `\n`. Order and repetition of jobs do not matter. `""` for an empty `T`. Pinned by `TestFailureKeyGolden`. |

### The landed diagnosis: `PipelineDiagnosis`

Written by the sweep. `applyPipelineBatch` sets it on the record only after the
whole validation chain passed (D8 steps 2-9) AND the run note was written in
state `diagnosed`; the sweep's own record write that follows the outcome report
(`PutIf` on the listed revision) persists it. A refused batch, a batch that
could not be checked, and a run note that could not be written all leave it
untouched. If the outcome report fails, nothing is written and the next sweep
pass judges the session again, recording the same value (the run note is
edited in place, not duplicated). If the record write itself loses its
compare-and-swap (`ErrStale`), the newer record stands and carries no
diagnosis although the note says `diagnosed`. So `Recorded() == false` means
"no fix can be dispatched from this record", never "no diagnosis exists".

| Field | Type | Meaning |
|---|---|---|
| `Class` | string | The diagnosis's classification: `code`, `test`, `flaky`, `infrastructure`, `configuration` or `unknown` (`effects.Classification`). `""` = no diagnosis recorded (`PipelineDiagnosis.Recorded()` is false). |
| `Files` | list of string, or null | The cited repository-relative paths, in the diagnosis's order, at most 10. Each existed at `PipelineSHA` when the diagnosis landed (D8 step 7). Null when the diagnosis cites none. |
| `EvidenceJobs` | list of string, or null | The distinct job names the evidence items name, in first-cited order. Each is the `Name` of a `PipelineJobs` entry (D8 step 4 checks every evidence job against the record). A pipeline-level evidence item (`pipeline_error` with no job) contributes nothing, so this is null for a diagnosis resting on that alone. |
| `Summary`, `Remedy` | string | The diagnosis's prose, at most 1500 characters each, as validated, with NUL characters removed (JSONB cannot hold one, and a record that cannot be written after its note landed would be judged again on every sweep). Otherwise verbatim: not trimmed, not redacted further -- the rendered note that contains them was refused if `pipelinectx.Redact` found anything in it. |

It is MODEL-AUTHORED content that passed every gate: a later reader may use it
to narrow what it does (which class envelope applies, which files may be
edited, which jobs are judged) and never to widen it.

Not carried over: a later dispatch of the same run anchor builds its record
from the order, so a `parked`, `needs-human` or re-dispatched `running` record
has no `PipelineDiagnosis` (and a park/deny has no `PipelineJobs` either). A
diagnosed run is not dispatched again (intake reads the landing), so in
practice the object stays on the `done` record through run maintenance's
rewrites; `TestPipelineRoundTripRecordsFailureFacts` pins that.

### Compatibility

- Additive JSON keys inside the existing `record` payload. No table, column,
  index or constraint changed: `workStoreSchemaVersion` stays `3` and there is
  no migration. The bd comment body is the same JSON and changes the same way.
- A record written before 2026-10-05 has neither the six job keys nor
  `PipelineDiagnosis`; it decodes with their zero values and reads as "not
  recorded" (`TestRecordWrittenBeforeFailureFactsStillReads`). Such a run
  cannot be given a fix: it has no failure key.
- A gonk-gate older than this change reading a newer record ignores the new
  keys, and DROPS them if it rewrites the record (it re-marshals its own
  struct). After a rollback, a run it touched reads as "not recorded".
- `ErrorSignature` values are comparable only within one `SignatureVersion`.
  `pipelinectx.TestErrorSignatureGolden` pins version 1's bytes; this change
  did not touch the canonicalisation.

## Fix epoch (gonk-2usw part EPOCH)

Plan: `docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md` section 3.3.

**Why.** A run is identified by (target, fingerprint, last success), and the
fingerprint reads only `(stage, name, failure_reason)` of the failed jobs. When
gonk's fix MR for a run merges and the same job then fails with a DIFFERENT
error, none of the three has moved, so the new failure had the old run's
anchor, the landing answered "diagnosed", and it was never looked at. The fix
epoch makes a merged fix END the run it was written for: the next failing
pipeline on the target is a new run, whatever its fingerprint.

**What changed, and what did not.** Everything below is absent / unchanged for
a run that follows no merged fix. That is tested against bytes produced before
the change (`TestFixlessIdentityGolden`,
`TestPipelineOrderVarsWithoutAFixUnchanged`,
`TestPipelineRecordWithoutAFixUnchanged`).

| Where | Without a merged fix (unchanged) | After merged fix MR `!N` |
|---|---|---|
| run anchor (`BeadAnchor`, meter's bead key) | `gonk:<pid>:pipeline:<target-key>:<fp16>:s<last_success>` | the same, plus `:f<N>` |
| session key (`SessionKey`) | `gonk-<pid>-pl-<first 12 hex of sha256(anchor)>` | same formula over the longer anchor, so a different key |
| run-note marker | `<!-- gonk:pipeline-run:<anchor> state=<state> -->` | same format with the longer anchor. A marker is matched on the WHOLE anchor: `...:s880` and `...:s880:f12` never answer for each other, although one is a prefix of the other |
| session name | `gonk-pipeline-p<pid>-pl<PipelineID>-a<n>` | unchanged: it is keyed on the pipeline, not the anchor |
| order var (`pack/orders/gonk-dispatch.toml`) | no `pipeline_fix_epoch` var | `pipeline_fix_epoch=<N>` |
| record | no `PipelineFixEpoch` key | `"PipelineFixEpoch": N` |
| intake's decision record (log line) | no `pipeline_fix_epoch` attribute | `pipeline_fix_epoch=N` |

`N` is `[1-9][0-9]*`. `:f0` is NOT an anchor: "no fix" has exactly one
spelling. `pipelinectx.AnchorFixEpoch(anchor)` reads `N` back (0 for none);
never look for the substring `:f`, a fingerprint may itself begin with `f`.

**Which MR sets it.** Controller-read GitLab facts only; nothing from a
session, a batch, a label or the repository (`pkg/intake/pipeline_fixepoch.go`,
`pkg/pipelinectx/fixepoch.go`):

1. The candidates are the project's merge requests with `state == merged`,
   `author.id ==` the bot's user id, a source branch that is exactly
   `gonk/pipefix-<12 lowercase hex>` (`pipelinectx.IsFixBranch`) in the same
   project, and a target branch equal to the branch a fix for this target
   merges into: the ref itself for a `ref:` target, the MR's SOURCE branch for
   an `mr:` target. Each condition is re-checked on the returned object; the
   list filters only bound the request. The label `gonk::pipeline-fix` is
   deliberately not read -- any member can remove it.
2. A candidate counts for a failing pipeline when the pipeline is AFTER it --
   it ran on the MR's merge commit, squash commit or head commit, or it was
   created at or after `merged_at` -- and the target's last green pipeline is
   NOT (it did not run on one of those commits, and `merged_at` is after its
   creation; with no green pipeline, any earlier fix counts).
3. The epoch is the iid of the newest counting candidate by `merged_at` (a tie:
   the larger iid), or 0.

The "pipeline is after it" bound is not in the plan's wording of 3.3 and is
required: without it the old run's own failing pipelines -- re-delivered, or
found by the backfill sweep before the merge commit's pipeline finishes --
would compute the new anchor and be diagnosed a second time. The commit rule
exists because GitLab stamps `merged_at` after it creates the merge commit
(3 s later in one observation on the synthetic project), so the merge commit's
own pipeline can predate `merged_at`.

**Fail closed.** The epoch is part of the identity, so every read it depends on
failing -- the merged-MR list, the MR of an `mr:` target, the pipeline's
detail, a merged fix with no `merged_at`, a pipeline or last green with no
creation time while a fix exists -- makes intake drop the event
`pipeline_facts_error` and fire nothing. The backfill sweep retries.

**Cost.** One more GET per gathered failed pipeline (the bot's merged MRs into
the target's branch). The MR and the pipeline detail are read only when a
merged fix exists.

**Compatibility.** A gonk-gate built before this part does not read
`pipeline_fix_epoch`, and its identity check rebuilds the anchor without the
suffix, so it REFUSES an order whose anchor carries `:f<N>` (exit 2, nothing
started). Such an order exists only after a bot-authored `gonk/pipefix-*` MR
has merged, and nothing opens one before the plan's part APPLY; deploy
gonk-intake and gonk-gate from the same build regardless. Old records read as
`PipelineFixEpoch == 0`. No store schema change (additive JSON in the `record`
payload), no migration.

**The run the fix ended** is closed by the post-merge confirmation (plan 7.4;
`cmd/gonk-gate/sweep_pipefix_confirm.go`, "After the merge" below), not by the
epoch itself: once the fix is `confirmed` on a target pipeline that is still
red, the old run's note moves to `superseded` and its `PipelineRunOpen` is
cleared, so the failure that pipeline carries is the NEW run's (anchor suffix
`:f<N>`) and is never booked as "still failing" on the old one.

## Pipeline fix (gonk-2usw part DISPATCH)

Plan: `docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md` sections 3.2, 4.2,
7.1, 8.1. Code: `cmd/gonk-gate/sweep_pipefix.go` (who writes it),
`broker_pipefix_dispatch.go` (who dispatches from it), `pkg/pipelinectx/fixanchor.go`
(the identities and markers).

**One fix per diagnosed run, in its own record.** A fix record is a `Record`
keyed by the FIX anchor, with `Trigger == "pipeline-fix"`. Its `PipelineTarget`,
`PipelineFingerprint`, `PipelineLastSuccess` and `PipelineFixEpoch` are the
diagnosed run's (the anchor is rebuilt from them and must match);
`PipelineID` / `PipelineSHA` are the pipeline and commit the fix is written
AGAINST (the run's newest failing pipeline when the fix was decided);
`PipelineRunOpen` is always false -- a fix record is not a run, and run
maintenance is never handed one. Everything the controller decided about the
fix is in `PipelineFix`. A record that is not a fix record stores no
`PipelineFix` key at all (`omitzero`; `TestPipelineRecordWithoutAFixUnchanged`).
No store schema change, no migration.

### Identities (all derived from the run anchor; `pkg/pipelinectx`)

| Name | Form | Function |
|---|---|---|
| fix anchor (`BeadAnchor`, meter's bead key for the fix) | the run anchor with `:pipeline:` replaced by `:pipefix:`, e.g. `gonk:84:pipefix:ref-0d6e4079:0123456789abcdef:s880[:f<N>]` | `FixAnchor`, `RunAnchorOfFix`, `IsFixAnchor` |
| fix branch | `gonk/pipefix-<first 12 hex of sha256(fix anchor)>` | `FixBranch` (`IsFixBranch` accepts exactly this shape) |
| fix session key (`SessionKey`) | `gonk-<pid>-pf-<the same 12 hex>` | `FixSessionKey` |
| fix session name | `gonk-pipefix-p<pid>-pl<PipelineFix.BasePipelineID>-a<attempt>` | `runtime.Name` |
| fix note marker | `<!-- gonk:pipeline-fix:<fix anchor> state=<state> -->` | `FixMarker`, `FixMarkerStates` |
| fix MR marker | `<!-- gonk:pipeline-fix-mr:<fix anchor> cause=<cause key> -->` | `FixMRMarker`, `ParseFixMRMarker` |

The fix note's states are plan 7.1's fix states plus `not-eligible`
(`pipelinectx.FixNoteStates`). Only a marker in a note the BOT authored counts.
The fix MR marker belongs in the merge request's description (part APPLY
writes it); the dispatch's dedup reads it from bot-authored merge requests on
fix branches, and treats one it cannot read as "cause unknown" (nothing is
dispatched) unless the MR is on this fix's own branch.

### `PipelineFix` fields

| Field | Meaning | Source |
|---|---|---|
| `RunAnchor` | the diagnosed run's `BeadAnchor`; `""` = not a fix record | the run record |
| `State` | plan 7.1's fix state, set only by the part that decides it: `needs-scope` at dispatch; `awaiting-run`, `verifying`, `needs-scope`, `taken-over`, `closed-unmerged`, `merged` by the apply; `fixed`, `fixed-new-failure`, `flaky`, `unverifiable`, `regressed`, `denied`, `taken-over`, `closed-unmerged`, `merged` by the exit check; `merged`, `closed-unmerged`, `confirmed`, `recurred` by the post-merge confirmation. `""` while the fix is being proposed, and after a refused batch (see the mapping below) | controller |
| `CauseKey` | `pipelinectx.CauseKey` over `Targets`, 16 hex | computed |
| `Class` | the diagnosis class the envelope was chosen for | the run's `PipelineDiagnosis.Class` |
| `BaseSHA`, `BasePipelineID` | the commit the fix is written against and the run's newest failing pipeline, which ran on it. The target's head was this commit when the fix was decided (E7) | GitLab |
| `TargetBranch` | the branch a fix MR targets: the ref of a `ref:` target, the SOURCE branch of an `mr:` target | GitLab |
| `ConfigSHA` | the default-branch commit the project's `scope:` section was read at | GitLab |
| `Envelope` | the path classes the fix may write (`pkg/scope` class names, class order): plan 4.2's row for `Class` inside the pack / operator / project fold, lifted classes included | `scope.Fold`, `Effective.Narrow` |
| `Lift` | the PROTECTED classes lifted for this dispatch: `["ci"]` or absent. A `scope.Lift` cannot be stored (its decoders refuse); a later part mints it again from this list with `scope.Effective.GrantLift`, which re-checks the pack row, the project's `allow_ci` and the operator veto | `Effective.GrantLift` |
| `Files` | the diagnosis's cited files inside `pipeline_fix.paths` (all of them when `Paths` is empty): the only paths an edit may touch | the run's `PipelineDiagnosis.Files` |
| `Paths` | `pipeline_fix.paths` as folded | meter |
| `MaxFiles`, `MaxLines`, `MaxFileBytes`, `MaxPatchBytes` | the fold's size caps | `scope.Effective.Caps` |
| `CIExecChanges`, `MaxAttempts`, `VerifyTimeoutSeconds` | `pipeline_fix.ci_exec_changes`, `max_attempts`, `verify_timeout` as folded when the fix was decided | meter |
| `Targets` (list) | the target set T: `Stage`, `Name`, `FailureReason`, `ErrorSignature`, `Generic` of each failed, required job the diagnosis's evidence names. The exit check judges the fix on exactly these | the run's `PipelineJobs` through `pipefix.TargetSet` |
| `AlsoFailing` (list) | names of the run's other recorded failed jobs ("also failing, not addressed") | the run's `PipelineJobs` |
| `BaseJobs` (list) | every job of the base pipeline, latest attempts: canonical `Name`, `Status`, `AllowFailure` | GitLab, at the look that decided the fix |
| `ExcerptVersion`, `SignatureVersion` | `pkg/pipelinectx`'s versions under which `Targets`' signatures were recorded | the run record |
| `FiredAt` | when gonk-sweep fired the fix order | sweep clock |
| `ContextSHA256` | sha256 of the JSON encoding of `Context` (`pipefixContextHash`); `""` until a session started | gonk-dispatch |
| `Context` | what the fix session was shown: a `FixContext` (its own table below). Written by gonk-dispatch in the write that makes the record `running`, replaced on each later attempt. Absent (`omitzero`) until then | gonk-dispatch, from part AGENT's preparer |

Written by the APPLY (`cmd/gonk-gate/broker_pipefix_apply.go`), never before:

| Field | Meaning | Source |
|---|---|---|
| `AppliedAttempt` | the meter attempt whose batch passed every static gate and was applied; 0 until then | the record's `Attempt` |
| `FixCommitSHA` | the ONE commit gonk made on the fix branch for `AppliedAttempt`: the only head the exit check judges (plan 6.3 J0) | GitLab's reply to the commit, or the adopted commit on a resume |
| `MRIID` | the fix merge request's iid (source branch `FixBranch(fix anchor)`); 0 until it is open | GitLab |
| `ChangedFiles` (list) | each file the commit changed: `Path`, `Class` (the path class it was judged under), `LinesAdded`, `LinesRemoved` (broker-computed) | `effects.DiffFacts` |
| `Weakening` | the test-weakening fact of the applied change: `decided` -- every changed source or test path is a Go file under the `go` profile and `pkg/weaken` found nothing; `not-decidable` -- some changed path has no deterministic predicate, so NOTHING says its tests were not weakened and the fix must never be merged automatically (plan 6.5 U4). `""` until a fix was applied | `scope.Decision.WeakeningDecidable`, `pkg/weaken` |
| `WeakeningUndecided` | the changed paths that made `Weakening` `not-decidable`, sorted | the same |
| `CIDelta` | definition integrity of the commit against the base (plan 6.2; `pkg/cidef` `Kind`): `none`, `preserving` or `exec-changing`. A `violation` is a refusal and an `unresolvable` an error of ours; neither is recorded here | `pkg/cidef` over the merged CI definition at both commits, or the byte-identity shortcut when no `ci` path changed |
| `VerifyDeadline` | when the exit check stops waiting for the merge request's pipeline: the apply's clock + `VerifyTimeoutSeconds` | sweep clock |
| `Refusal` | the rule code of the static gate that refused the LAST judged batch of this fix (`SC-WEAKEN(removed-test)`, `CI-PRE(allow_failure)`, ...; the codes are listed in `docs/reference/effects-contract.md`), `""` when the last batch was accepted or none was judged | the apply |

Written by the EXIT CHECK (`cmd/gonk-gate/sweep_pipefix_verify.go`) and the
post-merge confirmation (`sweep_pipefix_confirm.go`), never before:

| Field | Meaning | Source |
|---|---|---|
| `JudgedAt` | when the exit check last looked at this fix's merge request. It looks at most every 2 minutes (`pipefixJudgeEvery`); cleared when a further attempt is re-slung, so that attempt's first look is the tick after its apply | sweep clock |
| `Verdict` | the exit check's answer for `VerdictAttempt`: `FORGED`, `NOT_FIXED`, `FLAKY`, `UNVERIFIABLE`, `REGRESSED`, `FIXED_NEW_FAILURE` or `FIXED` (`pkg/pipefix` `Verdict`); `""` until one was given, and when the fix ended without one (merged, closed or taken over before a verdict) | `pipefix.Judge` |
| `VerdictRule` | the rule code that decided: a row of plan 6.3 (`J3.RECURS`, `J5.FIXED`, `J0.MERGED`, ...) or the exit check's own bound, `VF.BOUND(cannot-judge)` / `VF.BOUND(wait)` (no verdict by the verify deadline plus 30 minutes) | `pipefix.Result.Rule`, or the exit check |
| `VerdictDetail` | the same in the controller's words, one line (it quotes job names; it is never rendered to GitLab) | `pipefix.Result.Detail` |
| `VerdictAttempt` | the meter attempt the verdict is about | the record's `Attempt` |
| `JudgedPipelines` | ids of the merge request pipelines the verdict was given on (plan 6.3's C), ascending | GitLab |
| `Findings` (list) | the per-job findings of that verdict, one per entry as `<rule> <job>`: J3 for each diagnosed job, J4 for each other failing job | `pipefix.Result.Jobs`, `.Others` |
| `Outcome`, `OutcomeAttempt` | the meter outcome reported for that attempt (`success`, `gate-failed`, `infra-failed`), and the attempt. Written in the record write that follows the report | `gate.Classify` over plan 7.3's signal |
| `NoAutoMerge` (list) | every reason this fix must NEVER be merged automatically, sorted: `weakening-not-decidable` (`Weakening` is not `decided`), `ci-exec-changing` (`CIDelta`, as recorded or as judged, is `exec-changing`), `verdict-<state>` (the fix ended in any state but `fixed`). Empty only for a `fixed` fix with decided weakening and no execution-changing delta -- and an empty list grants nothing: merging is a separate policy (plan 9) | the exit check |
| `Feedback` | the sentence the NEXT attempt's prompt is given about the attempt just judged: the verdict, the rule code, and each diagnosed job's finding. Controller-written (`fixFeedback`); set with a not-fixed verdict | the exit check |
| `MergedAt`, `MergedSHAs` | the fix merge request's `merged_at`, and the commits by which the fix can be on the target (merge commit, squash commit, head), once it was seen merged | GitLab |
| `ConfirmPipelineID` | for a `confirmed` / `recurred` fix: the target pipeline that decided it. For a `merged` fix: the last post-merge target pipeline that was looked at and decided nothing (the diagnosed job did not run in it), so the next one after it is looked at | GitLab |

`CIDelta` is REWRITTEN by the exit check with the delta its verdict was given
on (the one `pipefix.Judge` computed from the merged definitions at judge
time), when it could be compared.

On the DIAGNOSED RUN's record (not the fix's):

| Field | Meaning | Source |
|---|---|---|
| `PipelineRunFixMR` | the fix merge request gonk opened for this run; 0 until one exists | the fix record's `MRIID` |
| `PipelineRunFixState` | the fix state last written to the run note's fix line | the exit check, the post-merge confirmation |

`CIExecChanges` above is the recorded `pipeline_fix.ci_exec_changes` the
apply acts on (plan 5.5): a change to a `ci` path is committed with
`[skip ci]` unless it is `auto`.

### The record's `State`, and the fix state it means

`cmd/gonk-gate` `fixStateOf` is the one mapping; dedup (plan 3.2 E9) reads it.

| Record | Fix state | Who wrote it |
|---|---|---|
| `PipelineFix.State` set | that state | the part that decided it |
| `State ""`, inside the fire window (`ReslingFireBy` + the dispatch timeout) | `proposing` | gonk-sweep, before it fired the order |
| `State ""`, past the fire window, or with no window (released) | none: the next look decides again, writing over exactly this revision | gonk-sweep / a released dispatch |
| `pending-prompt`, `running`, `parked` | `proposing` | gonk-dispatch |
| `needs-human` | `denied` (meter denied, or `pipeline_fix.max_attempts` was reached) | gonk-dispatch |
| `running` with `PipelineFix.State` `awaiting-run` or `verifying` | that state: the fix merge request is open and nothing has judged it. The session is closed; the meter reservation is still held, and NO outcome has been reported (plan 7.3: the attempt is judged in two phases) | the apply |
| `running`, no `PipelineFix.State`, `PipelineFix.Refusal` set, `ReslungAt` set | `proposing`: the last batch was refused (gate-failed) and the next attempt is being dispatched from this record | gonk-sweep |
| `done` with `PipelineFix.State == "needs-scope"` | `needs-scope` | gonk-sweep at the look (E6), gonk-dispatch (nothing editable at the base), or the apply (the session's verdict; an execution-changing CI edit under `ci_exec_changes: deny`) |
| `done` with `PipelineFix.State` `taken-over`, `closed-unmerged` or `merged` | that state: the apply found the fix branch or its merge request no longer gonk's to write, and wrote nothing | the apply |
| `running`, no `PipelineFix.State`, `PipelineFix.Feedback` set, `ReslungAt` set | `proposing`: the exit check judged the last attempt not-fixed (reported, `Outcome` / `OutcomeAttempt` say what) and the next attempt is being dispatched from this record. `FixCommitSHA`, `MRIID` and `ChangedFiles` are kept: the next apply commits on top and adopts the merge request | the exit check |
| `done` with `PipelineFix.State` `fixed`, `fixed-new-failure`, `flaky`, `unverifiable`, `regressed`, `taken-over`, `closed-unmerged` or `merged` | that state: the exit check settled the attempt and reported `success` once | the exit check |
| `needs-human` with `PipelineFix.State == "denied"` | `denied`: the last attempt was not-fixed (reported `gate-failed`) and no further attempt may follow -- `pipeline_fix.max_attempts`, or the diagnosed run closed or moved | the exit check |
| `done` with `PipelineFix.State` `merged`, `confirmed`, `recurred` or `closed-unmerged`, and `MergedAt` set where merged | that state: what happened to the merge request after its verdict | the post-merge confirmation |
| `done` with no `PipelineFix.State` | UNKNOWN: nothing is dispatched for this fix | nothing in this build writes it |

### The states the apply writes (plan 7.1), and what the next part reads

| `PipelineFix.State` | Set when | Also on the record | GitLab |
|---|---|---|---|
| `verifying` | the merge request is open and its pipeline runs on push | `FixCommitSHA`, `MRIID`, `AppliedAttempt`, `ChangedFiles`, `Weakening`, `CIDelta`, `VerifyDeadline` | branch, one commit, the merge request (labels `gonk::pipeline-fix`, `gonk::fix-verifying`), the fix note |
| `awaiting-run` | as above, but the change touches a `ci` path and `CIExecChanges` is not `auto`: the commit carries `[skip ci]` and a maintainer must start the pipeline | the same | labels `gonk::pipeline-fix`, `gonk::fix-awaiting-run` (and `gonk::ci-exec-changed` when `CIDelta` is `exec-changing`) |
| `needs-scope` | the session answered needs-scope and its request passed the text gates; or `CIDelta` would be `exec-changing` under `ci_exec_changes: deny` (the branch is deleted) | -- | the fix note; `gonk::needs-human` on the diagnosis landing |
| `taken-over` | the fix branch's head is not the pinned base and not gonk's own commit of this attempt; or an OPEN merge request on the branch is not the bot's | -- | nothing written |
| `closed-unmerged`, `merged` | the bot's merge request on the fix branch is already closed / merged and none is open | -- | nothing written |
| (unset) | a gate refused the batch | `Refusal` | nothing written (a `CI-DEF` refusal, which by plan 5.4 runs after the commit, deletes the branch it created) |

A fix in `verifying` or `awaiting-run` is handed, on every sweep tick, to the
exit check (`pipefixJudge` = `judgeOpenFix`, next section).

### The exit check (plan 6, 7.1-7.3, 8.1; part VERIFY)

`cmd/gonk-gate/sweep_pipefix_verify.go`. For every `running` fix record whose
`PipelineFix.State` is `verifying` or `awaiting-run`, at most once per 2
minutes: the recorded half of `pipefix.JudgeFacts` is copied from the record
(`fixFactsOf`), the judge-time half is read from GitLab through six GETs
(`readPipefixJudgeFacts`), `pipefix.Judge` decides, and the decision is acted
on. Zero tokens. It never merges.

| Decision | `PipelineFix.State` | Record `State` | Meter outcome (once per attempt) | Also |
|---|---|---|---|---|
| wait, cannot-judge | unchanged (`awaiting-run` becomes `verifying` once a pipeline that runs something exists on the fix commit) | `running` | none | `JudgedAt`. GitLab is written only when the landing does not yet show this state (the run record's `PipelineRunFixMR` / `PipelineRunFixState` say what was last shown), so a look that changes nothing reads the merge request and its pipelines and nothing else. Past the verify deadline + 30 min with no verdict: settled as the next row with rule `VF.BOUND(...)` |
| `FIXED` | `fixed` | `done` | `success` | |
| `FIXED_NEW_FAILURE` | `fixed-new-failure` | `done` | `success` | the other failure is listed; it is diagnosed after the merge, as a new run |
| `FLAKY`, `UNVERIFIABLE`, `REGRESSED` | `flaky`, `unverifiable`, `regressed` | `done` | `success` | `gonk::needs-human` on the diagnosis landing |
| `FORGED`, `NOT_FIXED`, attempts left and the run unchanged | cleared (`""`) | `running`, re-slung (`ReslungAt`, `ReslingFireBy`) | `gate-failed` (`infra-failed` when meter cannot show the session's tokens: an escalation must be paid for) | `Feedback`; the gonk-dispatch order is fired; the fix note says `not-fixed` |
| `FORGED`, `NOT_FIXED`, otherwise | `denied` | `needs-human` | the same | `gonk::needs-human`; no order |
| stopped: merged / closed / head is not gonk's commit | `merged` / `closed-unmerged` / `taken-over` | `done` | `success` | no `Verdict` |

"success" means "do not spend another rung on this", not "the pipeline is
fixed" (plan 7.3). The fix STATE says what happened; both are on one log line
per settled attempt (`sweep: pipeline fix settled ... fix_state=... outcome=...`)
and on the record (`Verdict`, `Outcome`).

Order of a settlement: GitLab (the fix note, the merge request, the run note's
fix line), THEN the meter, THEN the record (`PutIf` over the revision the look
read). A step that fails leaves the record `verifying` and the next look
repeats the settlement; each GitLab write is a no-op when GitLab already says
it, and the meter's outcome is idempotent per reservation. Past the bound the
GitLab writes are no longer waited for.

A fix record that lost its reservation deadline is bounded by its last write
plus the DEFAULT reservation TTL plus its recorded `VerifyTimeoutSeconds`
(`runningDeadline`), and an unclaimed `gonk-pipefix-*` session is "stale"
only after the default TTL plus the verify-timeout ceiling plus one hour
(`staleFixSessionAfter`).

**What the exit check writes to GitLab.**

1. **The fix note** (`writeFixNote`), in the state of the table above
   (`not-fixed` while a further attempt follows). It never moves backwards
   (`fixNoteMayReplace`, `fixNoteRank`): rank 0 `not-eligible`, `proposing`,
   `needs-scope`, `denied`; rank 1 `awaiting-run`, `verifying`, `not-fixed`;
   rank 2 `fixed`, `fixed-new-failure`, `flaky`, `unverifiable`, `regressed`,
   `taken-over`, `closed-unmerged`; rank 3 `merged`; rank 4 `confirmed`,
   `recurred`. The exit check writes a higher rank, the same state again, or
   moves within rank 1; `denied` replaces ranks 0 and 1 only; a state this
   build does not know is never rewritten. Found among the BOT's notes only.
2. **The fix merge request**, in ONE `PUT` (`updateFixMR`): the status block
   between `<!-- gonk:pipeline-fix-status -->` and
   `<!-- /gonk:pipeline-fix-status -->` (the rest of the description is left
   byte for byte; a description that would match GitLab's closing pattern is
   refused), and the labels. EXACTLY ONE STATE LABEL at a time:

   | Fix state | State label |
   |---|---|
   | `verifying`, `not-fixed` (trying again) | `gonk::fix-verifying` |
   | `awaiting-run` | `gonk::fix-awaiting-run` |
   | `fixed`, `fixed-new-failure` | `gonk::fix-verified` |
   | `flaky`, `unverifiable`, `regressed`, `taken-over`, `closed-unmerged` | `gonk::fix-inconclusive` |
   | `denied` | `gonk::fix-failed` |
   | `merged`, `confirmed`, `recurred` | unchanged: a merged merge request is not edited |

   `gonk::fix-new-failure` is added exactly for `fixed-new-failure`;
   `gonk::ci-exec-changed` is the apply's and is left alone. Only a merge
   request the bot authored, on this fix's branch, with both ends in this
   project, is edited.
3. **The run note's fix line** (plan 5.4 step 4): while the diagnosed run's
   note is `diagnosed`, its status carries
   `- Fix: merge request !<N> -- <where it stands>`, rendered from
   `PipelineRunFixMR` / `PipelineRunFixState` on the run's record.

**The next attempt.** gonk-dispatch reads `Feedback` and renders, under
"Previous attempt" in the fix prompt, that sentence and the previous change
(`previousFixChange`: for each of `ChangedFiles`, the lines of `BaseSHA` the
commit `FixCommitSHA` replaced and the lines it put there -- two reads per
file, fenced, capped at 1024 bytes -- 1536 until gonk-nf9i, when the key
list and the refused-answer block needed the room). A fix answer refused in
phase A never becomes a commit and leaves no `Feedback`: its word for the
next attempt is the record's `RetryFeedback` (section "Retry feedback"),
rendered at the end of the prompt; an attempt can carry both. The apply then commits on top of
`FixCommitSHA` and adopts the open merge request. When that dispatch is
refused instead (meter denies, or `max_attempts` by meter's count), the fix
note moves from `not-fixed` to `denied` and the open merge request's state
label is swapped for `gonk::fix-failed` (`postFixNote`).

### After the merge (plan 7.4)

`cmd/gonk-gate/sweep_pipefix_confirm.go`, called by run maintenance for each
open diagnosed run BEFORE the run is maintained, when the fix pass is on.
Zero tokens, no meter call.

| Fix state found | What is read | Result |
|---|---|---|
| `fixed`, `fixed-new-failure`, `flaky`, `unverifiable`, `regressed`, `denied`, `taken-over` | the merge request | closed -> `closed-unmerged`; merged -> `merged` with `MergedAt`, `MergedSHAs`; else nothing |
| `merged` | the target's pipelines (the ref's newest 20, or the MR target's). The FIRST one after the merge -- it ran on one of `MergedSHAs`, or was created at or after `MergedAt` -- that is `success` or `failed`, with every attempt of its jobs and the logs of failed diagnosed jobs | `pipefix.Confirm`, below |
| `confirmed`, run still open | the confirming pipeline | if it `failed`: the run is closed (a retry of the row below) |

| `pipefix.Confirm` | Fix state | The diagnosed run |
|---|---|---|
| every diagnosed job PASSED or fails differently, target pipeline green | `confirmed` | resolved by run maintenance in the same look (existing: note `resolved`, tracking issue closed) |
| the same, target pipeline red | `confirmed` | CLOSED here: note `superseded` ("the fix merge request !N merged and the failure diagnosed below did not occur in pipeline #Q ..."), `PipelineRunOpen` cleared. The tracking issue stays open for the new run |
| a diagnosed job failed the way it was diagnosed, on any attempt | `recurred`, `gonk::needs-human` | stays open |
| a diagnosed job did not run (or ran allowed to fail), target pipeline red | stays `merged`; `ConfirmPipelineID` remembers the pipeline and the next one decides | unchanged, and HELD (below) |
| the same, target pipeline green | stays `merged`; the fix note says it is NOT confirmed. **A green target pipeline in which the diagnosed job did not run confirms nothing** | resolved by run maintenance in the same look, which ends the watch |
| a job list or a log unread, an attempt still active | nothing; read again | unchanged |

**The run is held while its merged fix is unconfirmed.** In a look that leaves
the fix `merged` and found no green target pipeline, run maintenance does not
look at the run (`holdRun` stamps `PipelineCheckedAt` so the next look is
five minutes on). Every failing pipeline after the merge belongs to the NEW
run the fix epoch starts, so it must not be booked on the old run as "still
failing", nor supersede it before the fix was confirmed.

**Not followed:** a fix whose run closed BEFORE its merge request merged (the
target went green, or failed with other jobs, while the merge request sat
open). Nothing visits that run again; the fix keeps its verdict's state. The
fix epoch does not depend on this pass.

### What the apply writes to GitLab (plan 5.4, 8.1)

In this order, each step first looking for what an interrupted run left:

1. **The branch** `FixBranch(fix anchor)`, created at `BaseSHA` (the full
   SHA, never a branch name).
2. **One commit** on it, by the bot, `update` actions only. Message:

   ```
   gonk: fix job <job> on <target branch>

   gonk-fix: <fix anchor> attempt=<n>
   ```

   plus a third paragraph starting `[skip ci]` for a CI change that must not
   run on push. `<job>` and `<target branch>` are reduced to letters, digits
   and `. _ - / :` and spaces (`refSafe`), so neither can carry a reference.
   The words are `fix job <name>`, not `fix <name>`: GitLab reads a closing
   verb directly followed by a reference or a tracker key as "close on merge".
3. **The definition check** (`CI-DEF`), after the commit and before the merge
   request. A violation deletes the branch (unless an open merge request
   already stands on it) and writes nothing else.
4. **One merge request**, found again by its source branch. Title: the
   commit's first line. Labels as in the table above. Its description is the
   controller's and holds, in order: the STATUS BLOCK between
   `<!-- gonk:pipeline-fix-status -->` and `<!-- /gonk:pipeline-fix-status -->`
   (one sentence; a later part edits it in place); `Related to #<tracking
   issue>` or `Related to !<merge request>` -- never a closing keyword; the
   exit criterion in words and the target jobs with their signatures; the
   changed files with class and line counts; the weakening and CI-definition
   facts; the agent's reason (fix_meta) in a fenced block, labelled as the agent's;
   the details table (fix anchor, cause key, base commit, base pipeline,
   class, attempt, session); and the fix MR marker as the last line. The
   agent's PROSE (the fix_meta reason) is made unable to form a reference
   (`inert`: `#12` is shown as `# 12`, a URL scheme as `https: //`, a tracker
   key as `ABC- 12`, in either case). Every NAME -- job, stage, branch,
   path, error signature, rung, session, anchor, cause key, base commit --
   is written by `ident` (gonk-htl3): between `<code>` and `</code>`, every
   character that is not an ASCII letter, digit or space as a numeric
   character reference (`smoke&#x2d;pipefix&#x2d;2026`). GitLab displays the
   name exactly, while the raw text -- which is what GitLab scans for closing
   patterns -- holds no `#`, `!`, `://`, hyphen, `@`, `|` or backtick of the
   name's own, so a job called `closes #12` or `fix-1234` closes nothing,
   links nothing and is not rewritten. (Until 2026-10-07 names went through
   `inert` too, and every name with a digit after a hyphen was shown with a
   space in it: the description named a job that did not exist.) A reader of
   the RAW description through the API must decode the character references
   to get a name back. The finished description is refused if GitLab's
   closing pattern matches anywhere in it. **No assignee, no reviewer, no
   approval, no merge**: the create request carries exactly `source_branch`,
   `target_branch`, `title`, `description`, `labels` and
   `remove_source_branch: false`.
5. **The fix note** on the diagnosis landing, moved to `verifying` /
   `awaiting-run` / `needs-scope` (`writeFixNote`). It is found among the
   BOT's notes only -- a person's note quoting the marker is never edited --
   and it never moves backwards: an apply-time note replaces a dispatch-time
   note, an earlier `awaiting-run` / `verifying` note or `not-fixed`, and
   nothing else (`fixNoteMayReplace`; the exit check's own rule is in "The
   exit check" above).

`ReslungAt` / `ReslingFireBy` on a fix record in `State ""` are the fix
order's fire window, with exactly a re-sling's meaning: a gonk-dispatch that
starts after `ReslingFireBy` does nothing.

### The order

A fix is dispatched by the existing `gonk-dispatch` order
(`pack/orders/gonk-dispatch.toml`) with `trigger=pipeline-fix`,
`bead_anchor` / `bead_id` = the fix anchor, `session_key` = the fix session
key, and the diagnosed run's identity in `pipeline_target`,
`pipeline_fingerprint`, `pipeline_last_success`, `pipeline_fix_epoch`;
`pipeline_id` is `PipelineFix.BasePipelineID`. No var carries a decision:
gonk-dispatch refuses a `pipeline-fix` order for which no fix record exists
(exit 2, before meter is asked).

### Labels (controller-reserved, `cmd/gonk-gate/broker_label.go`)

| Label | Written by | Meaning |
|---|---|---|
| `gonk::needs-human` | gonk, on the diagnosis landing | a fix needs a person: needs-scope, the chain cap, a deny, an inconclusive verdict (`flaky`, `unverifiable`, `regressed`), `denied`, `recurred` |
| `gonk::fix-class-confirmed` | a HUMAN only (gonk never writes it; an agent cannot propose it) | confirms a `configuration` diagnosis, without which no CI-definition fix is dispatched (owner decision Q2) |
| `gonk::pipeline-fix` | the apply, on the fix merge request (it is `verdictLabels[fix]`) | this merge request is a gonk pipeline fix |
| `gonk::fix-verifying` | the apply, the exit check | open; its pipeline has not been judged, or an attempt did not fix it and another follows |
| `gonk::fix-awaiting-run` | the apply; the exit check removes it once a pipeline was started | open with `[skip ci]`; a maintainer must start the pipeline |
| `gonk::ci-exec-changed` | the apply, on the fix merge request | the change alters what the diagnosed job executes (`CIDelta` `exec-changing`) |
| `gonk::fix-verified` | the exit check, on the fix merge request | `fixed` or `fixed-new-failure`: the diagnosed failure no longer occurs in the merge request's pipeline |
| `gonk::fix-new-failure` | the exit check | with `gonk::fix-verified`: something else fails there |
| `gonk::fix-inconclusive` | the exit check | `flaky`, `unverifiable`, `regressed`, `taken-over` or `closed-unmerged`: gonk could not establish that the fix works |
| `gonk::fix-failed` | the exit check; gonk-dispatch when the next attempt is refused | `denied`: the criterion was observed NOT met and gonk has stopped |

`gonk::needs-human` is also `verdictLabels[needs-scope]`: the label the apply
puts on the landing when a session's needs-scope lands.

## Pipeline-fix context facts: `FixContext` (gonk-2usw part AGENT)

`beadstore.FixContext` (`pkg/beadstore/fixcontext.go`) is what ONE pipeline-fix
session was shown, as controller-held facts. gonk-gate's
`preparePipefixDispatch` builds it in the same pass that renders the session's
prompt (`pipefixDispatch.context`), and `verifyPipefixContext` re-derives every
hash in it from a fresh read of GitLab before the session's answer is applied
(plan 5.3 gate A9) -- the same idea as `PipelineJobs[].ExcerptSHA256` for a
diagnosis. Nothing in it comes from the session.

It is stored as `PipelineFix.Context` on the fix record (see "Pipeline fix"
above), written by gonk-dispatch with the `running` record; `ContextSHA256`
is the sha256 of its JSON encoding. The apply checks that hash first and then
every hash inside it.

| Field | Type | Meaning |
|---|---|---|
| `BaseSHA` | string | full commit the checkout was pinned to and the `edit` effects are applied against; the commit `PipelineID` ran on. `""` = no context recorded (`Recorded()`) |
| `PipelineID` | int | the failing pipeline whose jobs were shown: the run's newest failing pipeline at dispatch, not necessarily the diagnosed one |
| `DiagnosisSHA256` | string | sha256 of the landed diagnosis as shown: JSON of `{Class, Files, EvidenceJobs, Summary, Remedy}` in that order |
| `Jobs` | `[]PipelineJob` | the target set `T` (plan 3.2) AS IT FAILED IN `PipelineID`: `ID` is the job's id in that pipeline; `Excerpted`, `ExcerptSHA256`, `Groundable` and the failure facts are that job's. In `T` order. Same type as `Record.PipelineJobs` |
| `ExcerptVersion` | int | `pipelinectx.ExcerptVersion` at dispatch |
| `ExcerptMaxBytes` | int | the ONE per-job byte cap every excerpt of this session was built with |
| `Files` | `[]FixContextFile` | the diagnosis's cited files, in its order, deduplicated |
| `Limits` | `FixLimits` | the limits the prompt stated: `Edits`, `Files`, `Lines`, `NewBytes`. Gate A9 requires them to equal the judging envelope's |
| `PromptSHA256` | string | sha256 of the whole rendered prompt. The pipefix prompt has its OWN budget, 64 KiB (`pipefixMaxPromptBytes`, gonk-x06m), over which file content is left out, then the excerpts shrink, then dispatch is an error; the diagnosis prompt keeps `pipelinectx.MaxPromptBytes` (28 KiB). Within it the previous attempt's diff is shown up to 6 KiB (`pipefixCapPreviousDiff`); a longer one is cut and the prompt then carries the controller's line `[gonk: cut: N of M bytes shown]` after the block |

`FixContextFile`:

| Field | Type | Meaning |
|---|---|---|
| `Path` | string | the cited path |
| `Editable` | bool | the prompt listed it under "Files you may change": the dispatch's envelope allows an edit to it (`effects.ValidateEditPaths`) AND the blob at `BaseSHA` exists, is non-empty NUL-free UTF-8 within the file-size cap, and is not generated |
| `Rule` | string | when not editable, the stable code of the predicate: `SC-FORM`, `SC-PROT`, `SC-CLASS`, `SC-CITED`, `SC-BEAD`, `SC-SIZE`, `ED-MISSING`, `ED-ENCODING`, `ED-NOMATCH` (an empty file). `""` when editable |
| `Bytes`, `SHA256` | int, string | size and sha256 of the blob read at `BaseSHA`. Set for every file whose blob was read (all editable files); zero otherwise |
| `Shown` | bool | the blob's content was rendered into the prompt, whole and unaltered. `false` when it was left out (over 6 KiB, over 12 KiB together with the files before it, dropped to fit the pipefix prompt budget of 64 KiB (`pipefixMaxPromptBytes`; the diagnosis agent's 28 KiB cap is its own), or it holds text `pipelinectx.Redact` matches or a control character); the session then reads it in its checkout |

## Writes

- `Put(r)` is unconditional.
- `PutIf(r, ifRevision)` writes only if the stored record's `Revision` is still
  `ifRevision` (`""` = absent or pre-revision), returns the new revision, and
  otherwise writes nothing and returns an error wrapping `beadstore.ErrStale`.
  Every gonk-sweep write uses it, conditioned on the copy the pass listed.

Which writer uses which (gonk-p8w8 for the sweep, gonk-y4m7 for dispatch):

| Writer | Write | Kind |
|--------|-------|------|
| sweep | done, escalate/retry, pending-prompt reclaim | `PutIf` on the listed revision |
| dispatch | `pending-prompt` reservation | `PutIf` on the revision the dedupe guard read (`""` if no record, or if the store could not be read). On `ErrStale`: read again, re-run the guard, retry (3 tries, then exit 1 with no session). |
| dispatch | release after a failed dispatch | `PutIf` on the reservation's own revision; if that write's outcome was unknown, only over a `pending-prompt` record carrying this dispatch's alias. `ErrStale` = leave the newer record, WARN. |
| dispatch | `running` record after the prompt was fetched | `Put`: a record that does not land orphans a live agent |
| dispatch | `parked` / `needs-human` | `PutIf` on the revision read BEFORE `/decide` (`""` if no record, or if the store could not be read). On `ErrStale`, re-read and write again against the newer record if meter's decision already covers it: State `""` (a FAILED dispatch's release on a bead with no prior record, gonk-k3qy), `done` (a finished attempt -- nothing lists it or refuses new work over it), or a record CONTENT-EQUAL to the one read before `/decide` -- every field, `SessionID` and `Attempt` included, equal; only `Revision`/`UpdatedAt` differ. That is usually a failed dispatch's release restoring its prior, but the check cannot see the writer and does not need to: meter decided on exactly that content, so the outcome is the one had the read come after that write. A new claim always differs (a fresh reservation's `SessionID` or attempt, a later deny's attempt). This is why a new trigger on a `needs-human` issue still parks. At most 2 re-writes -- a choice made on rarity, not a proof: past it, the bead is left where the last writer left it, stranded if that was another failed release. Any other newer record belongs to a writer acting on the bead (a reservation; a park/deny decided on a different record; a sweep write): leave it, WARN, exit 0, no deferred notice. A failed re-read also steps aside, saying it cannot tell what replaced the record. After an unreadable read, `ErrStale` = a record exists that was never seen: write nothing, exit 1 -- and if that record is `done`, `needs-human` or State `""`, no sweep picks the bead up and the trigger is lost. (gonk-b2jk) **Closes the race on Postgres only:** on bd (the store when `workstore.enabled: false`, the chart default) `PutIf` is not a compare-and-swap -- `PutIf(r, "")`, used here for a first-time bead and after a failed read, can tell two writers both "applied", and see gonk-vred -- so there it narrows the window, no more. |

The reason for the one unconditional dispatch write (`running`) is in
`docs/plans/2026-09-13-y4m7-dispatch-conditional-writes.md` section 3. That
section also used to keep park/deny unconditional, on the ground that meter
does not defer or deny while a reservation is open; that holds when meter
decides, not when the write lands, and an unconditional park/deny erased a
reservation made in between (gonk-b2jk).

A re-sling's gonk-dispatch that starts after its `fire_by` deadline makes none
of the dispatch writes above: no `/decide`, no read, no reservation, no
release, no session.

## Storage in Postgres (`Postgres`, gonk-t41)

A database on your Postgres -- it may be the meter's database -- but NOT the
meter's role or schema (owner decision on the gonk-t41 review): login role
`gonk_workstore`, owning schema `gonk_workstore`, DSN in Secret
`gonk-workstore` key `dsn`. Postgres's default search_path (`"$user", public`)
resolves the store's unqualified table names to that schema; the store adds
nothing to the DSN. `OpenPostgres` refuses to run unless `current_schema()` is a
schema owned by the connecting role (not `public`, not someone else's), so the
tables cannot silently land in public if the schema was not created first. With
this role the controller cannot read or write the meter's tables in `public`
(`TestWorkStoreRoleCannotTouchTheMeterLedger`: settling reservations, deleting
spend rows, reading LiteLLM keys and rewriting registrations are all denied).

How the role and the schema come to exist is yours to choose. With
CloudNativePG, for example, the role can be a managed role with its password
from the cluster's secret store, and the schema declared in the Database CR's
`spec.schemas` (CNPG >= 1.26). The store creates and migrates its own tables inside that schema
(`pkg/beadstore/postgres_schema.go`, `workStoreDDL`, version `3`; 2 added
`work_items_revision_set` and `work_items_quarantine`, 3 added
`work_items_pipeline_run_open_idx`). Requires
Postgres 14 or later.

```sql
CREATE TABLE work_items (
    anchor     TEXT PRIMARY KEY,                     -- BeadAnchor
    revision   TEXT NOT NULL,                        -- Revision
    state      TEXT NOT NULL,                        -- State
    record     JSONB NOT NULL,                       -- the whole Record, as in "Fields"
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),   -- server clock; informational
    written_at TIMESTAMPTZ NOT NULL DEFAULT now(),   -- server clock of the last write; informational
    CONSTRAINT work_items_revision_set CHECK (revision <> ''),
    CONSTRAINT work_items_record_agrees CHECK (
        record->>'BeadAnchor' = anchor AND record->>'State' = state AND record->>'Revision' = revision)
);
CREATE INDEX work_items_state_idx ON work_items (state, anchor COLLATE "C");
-- schema version 3 (gonk-tios verify report R6): the open pipeline runs only
CREATE INDEX work_items_pipeline_run_open_idx ON work_items (anchor COLLATE "C")
    WHERE (record->>'PipelineRunOpen') = 'true';

CREATE TABLE work_items_bd_sync (    -- the bd cutover's merge base; goes with bd (T-56)
    anchor      TEXT PRIMARY KEY,
    bd_digest   TEXT NOT NULL,       -- sha256 of bd's current gonk-state comment TEXT (as `bd comments --json` returns it) at the last import/export: what the merge compares
    bd_revision TEXT NOT NULL,       -- bd's record Revision then; informational ("" for pre-p8w8 records)
    pg_revision TEXT NOT NULL,       -- work_items.revision at that moment
    synced_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workstore_meta (        -- singleton row
    id             BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    schema_version INT NOT NULL,     -- DDL runs only when this is older than the binary's
    bd_imported_at TIMESTAMPTZ       -- set by a completed `gonk-gate workstore-import`
);
```

- **The record is the JSONB payload**, same JSON as the bd comment body (no
  `IfRevision`). Times keep nanoseconds, which a `TIMESTAMPTZ` would not.
  JSONB cannot hold `\u0000`: a record with a NUL character in any string field
  is refused (an error, not a silent change).
- **`state` and `revision` are columns written by the same statement as the
  payload**, and the CHECK makes a disagreement unrepresentable. There are no
  state labels to tear.
- **No constraint on which states exist.** `Memory` accepts any `State`, and
  dispatch's release when there was no prior record writes one with
  `State == ""`, which `Get` sees and `List` never returns (the shape of
  gonk-p7qh's `go-ffoe`).
- `Put`: `INSERT ... ON CONFLICT (anchor) DO UPDATE`.
- `PutIf(r, rev)`: `UPDATE ... WHERE anchor = $1 AND revision = rev`;
  `PutIf(r, "")`: `INSERT ... ON CONFLICT (anchor) DO NOTHING` -- in this store
  `""` means "no record", nothing else: no row has an empty revision
  (`CHECK (revision <> '')`), because the bd import gives every record written
  before revisions existed a fresh one. (When it did not, dispatch's
  unreadable-store reservation could match an imported running record and its
  release destroy it -- review defect A1.) Zero rows = `ErrStale`. Any error =
  unknown outcome (never `ErrStale`).
- `List(st)`: `WHERE state = st ORDER BY anchor COLLATE "C"`, no limit.
- `ListOpenPipelineRuns()`: `WHERE (record->>'PipelineRunOpen') = 'true'
  ORDER BY anchor COLLATE "C"`, any state, answered from the partial index
  (its predicate is the same Go constant, `pipelineRunOpenPredicate`;
  `TestPostgresOpenPipelineRunsUseThePartialIndex` pins that the planner uses
  it). gonk-sweep's run maintenance reads ONLY this, so a closed run -- and
  every other finished record -- is never read again by it. A record written
  before the field existed has no key and is not open. `BdCLI` has no field
  query and lists every state then filters (it is no longer gonk-gate's state
  store).
- Every connection sets `client_connection_check_interval = 1s`,
  `lock_timeout = 5s`, `statement_timeout = 15s`,
  `idle_in_transaction_session_timeout = 15s`. The first is what stops a
  writer killed while its statement waits on a row lock from committing after
  its death (`TestKilledWriterBlockedOnALockNeverLands`; without it, measured,
  the dead write lands).

**What holds, and where it is tested** (`-tags integration`, real Postgres):
`PutIf` success is final (`RunCompareAndSwap`, `TestPostgresManyWritersOneChain`);
state cannot disagree with the record
(`TestPostgresStateColumnCannotDisagreeWithTheRecord`); `List` is unbounded
(`ListIsNotTruncated`) and served by the index (`TestPostgresListUsesTheStateIndex`);
a transaction killed before `COMMIT` is rolled back
(`TestKilledClientTransactionIsRolledBack`).

## Which store gonk-gate uses (expand then contract)

The MOUNT decides, in every gonk-gate from release 1 of gonk-t41 on:

| `GONK_WORKSTORE_DSN_FILE` | `GONK_BEAD_REPO_DIR` | store |
|---|---|---|
| set | (any) | Postgres. Refuses (exit 2) if no import ever completed, or -- with `GONK_BEAD_REPO_DIR` set -- if this pod's import did not stamp `/city` (a break-glass stamp is accepted and warned about). Exit 1 if Postgres is unreachable. |
| unset | set | bd (`BdCLI`), exactly as before gonk-t41. |
| unset | unset | in-memory (smoke tests only). |

Release 1 is the binary with no chart change: every chart before the work-store
chart release sets only `GONK_BEAD_REPO_DIR`, so it runs on bd. Release 2 is the
chart that mounts Secret `gonk-workstore` (`dsn`) and runs the import in
`bootstrap-city`. An image ahead of its chart runs on bd (no outage); a chart
ahead of its image is ignored by a pre-t41 binary. gonk-vred's duplicate risk
remains while the controller runs on bd.

## Cutover from bd, and rollback (gonk-t41)

The bd-backed gonk-gate kept these records in bd. `gonk-gate workstore-import`
copies them into Postgres; `gonk-gate workstore-export` copies Postgres's back.
Both are a three-way merge against `work_items_bd_sync` (a digest of bd's
current gonk-state comment and the `work_items` revision when the two last
agreed). bd's side is a digest, not its revision, because the gonk-gate deployed
before gonk-p8w8 writes records with no `Revision`: every record on the cluster,
and every write a rollback to that binary makes, has revision `""`, so only the
content shows the change. It digests the comment's RAW TEXT, not the decoded
Record: a digest of the Go struct changes for every record the day a field is
added to Record, turning every anchor Postgres has moved into a permanent
conflict (review defect A2).

| bd since the last sync | Postgres since the last sync | import (bd -> Postgres) | export (Postgres -> bd) |
|---|---|---|---|
| unchanged | unchanged | nothing | nothing |
| changed | unchanged | bd's record is copied | nothing (bd is newer) |
| unchanged | changed | nothing (Postgres is newer) | Postgres's record is written with `BdCLI.Put` |
| changed | changed | CONFLICT: logged at ERROR, neither touched | CONFLICT: logged at ERROR, neither touched, exit 1 |
| never synced | absent / absent in bd | copied | written |
| never synced | present and different | CONFLICT | CONFLICT |

- A bd record with no `Revision` (every record a pre-gonk-p8w8 binary
  wrote) is imported with a fresh one; `work_items_bd_sync.bd_revision`
  keeps bd's `""`.
- The import copies what the bd-backed binary's `Get` returned: the beads found
  by `bd list --label-pattern 'gonk-anchor:*' --limit 0 --json` (bd's default
  status filter, so a CLOSED bead is not imported -- that binary could not see
  it either), each record verbatim (Revision and UpdatedAt kept). A bead with no
  gonk-state record is counted and skipped; an undecodable one is reported and
  skipped; several beads for one anchor are reported and the one `Get` read is
  used. A bead whose state label is missing IS imported (it is found by its
  anchor label).
- The import marks `workstore_meta.bd_imported_at` only when every anchor was
  read and decided, and writes `$GONK_BEAD_REPO_DIR/.gonk-workstore-imported`
  (the pod's `/city` emptyDir). gonk-gate dispatch and sweep refuse to run
  (exit 2) on a store without the mark, or -- while `GONK_BEAD_REPO_DIR` is set
  -- in a pod without the stamp.
- `gonk-gate workstore-survey` (bd only, writes nothing): see "Quarantine" below.
- The controller's `bootstrap-city` initContainer runs the import on every pod
  start, before `gc start`, when its gonk-gate lists `workstore-import` in
  `gonk-gate capabilities`. A failed import fails the initContainer.
- The export's `BdCLI.Put` restamps `UpdatedAt` (a pending-prompt reservation
  keeps its own) and assigns a new bd revision, recorded in the sync row.

**Quarantine (owner decision on the gonk-t41 review).** At import, a record in a
state the sweep lists (`running`, `parked`, `pending-prompt`) is written as
`needs-human` -- which no sweep List reads -- when:

- `hidden-from-bd-list`: the bd-backed List could not see it. Replicated
  exactly: `bd list --label gonk::<state> --limit 50 --json` (the bd-backed
  List's call with bd's default limit written out) does not return its bead --
  its state label was lost (gonk-9sf9) or it is past the 50th row (gonk-kx3); or
- `older-than-24h`: its last write (bd's `created_at` for the current comment;
  the record's `UpdatedAt` if bd sent none) is more than 24 hours old; or
- `age-unknown`: neither time exists.

Reasons combine with `+`. The original record, reason, original state and last
write go to `work_items_quarantine`:

```sql
CREATE TABLE work_items_quarantine (
    anchor         TEXT NOT NULL,
    quarantined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason         TEXT NOT NULL,          -- hidden-from-bd-list | older-than-24h | age-unknown, joined by +
    original_state TEXT NOT NULL,
    last_write_at  TIMESTAMPTZ,            -- NULL = unknown
    bd_record      JSONB NOT NULL,         -- the record as bd held it
    PRIMARY KEY (anchor, quarantined_at)
);
```

Everything else imports as it is. The import report and log carry
`quarantined`, `quarantine_by_reason` and `quarantined_anchors`, and each
quarantined anchor is logged at WARN. A new dispatch for a quarantined anchor
behaves as it does for any needs-human record (its dedupe guard reads it; its
reservation is conditioned on its revision).

**Sizing it before the cutover: `gonk-gate workstore-survey`** (bd only, writes
nothing) prints, as JSON: `by_state`; `age_buckets` per state (`<1h`, `1h-24h`,
`1d-7d`, `7d-30d`, `>30d`, `unknown`); `quarantine` by reason and
`quarantine_total`, exactly as an import would decide them now; and `records`,
one per anchor with `last_write_at`, `age_hours`, `visible_to_bd_list` and its
`quarantine` reason.

**Fresh install.** On a fresh Dolt, `gc init --no-start` writes
`.beads/metadata.json` but `gc start` creates bd's database, and the import
runs between the two. bd then reports `failed to open database: database
"bd_<name>" not found on Dolt server`. A MISCONFIGURED `dolt_database` on an
EXISTING install reports exactly the same thing, and bd gives no way to tell
them apart -- so the import does not guess (verification defect 1):

- With `GONK_WORKSTORE_FRESH_INSTALL=1` -- a POSITIVE signal from outside bd:
  the release-2 chart renders it on the bootstrap only when Helm is installing
  the release (`.Release.IsInstall`), or when an operator sets
  `workstore.freshInstall: true` -- and a work store holding no work records,
  that message is an EMPTY, complete import (`bd_database_missing: true`,
  logged at WARN): marked, stamped, the controller starts.
- Without the signal (every upgrade, including the upgrade that cuts an
  existing install over), or with a store that already holds work records, it
  is an ERROR (`ErrNotAFreshInstall`, naming the database bd looked for): exit
  1, nothing marked, no stamp, and break-glass is refused on a store no import
  has completed against. Fix `dolt_database`.
- `gonk-gate workstore-survey` (no store, writes nothing) still reports
  `bd_database_missing` and `bd_database`; with the DSN its import plan follows
  the rule above.

If a fresh install's FIRST pod never reached `gc start` and Helm then UPGRADES
the release (e.g. Flux retrying a failed install without uninstalling it), the
signal is gone and bd's database still does not exist, so the import refuses.
If -- and only if -- that is the situation (no records anywhere yet), set
`workstore.freshInstall: true` for one reconcile, then remove it. The empty-store
check still applies. The env also means the first upgrade after an install
changes the pod template, restarting the controller once.

An unreachable Dolt is neither case, and fails the import.

One thing the import does NOT handle for you, stated rather than hidden
(independent verification of the rework):

- A QUARANTINED `running` record is invisible to the reaper as well as the
  sweep (`cmd/gonk-gate/reap.go` lists `running`), so a live session it names
  keeps running until a human acts on `work_items_quarantine`. That is the point
  of quarantine -- nothing acts automatically on a backlog nothing has looked at
  -- but it does mean a quarantined session is not torn down for you.

**Retries and timeouts.** Every bd read in the import, export and survey is
tried 3 times (backoff 1s, 2s), each try bounded at 60s: a Dolt hiccup on one
anchor does not fail a pod start, and a stalled Dolt fails it in bounded time
(measured locally with a SIGSTOPped Dolt: 35s, bd's own I/O timeout firing first).

**Break-glass: the controller cannot start because the import cannot
complete.** After the cutover, a Dolt outage or an anchor bd will not return
fails the `bootstrap-city` initContainer on every pod start, by design: the pod
must not start without the records bd may hold. If bd cannot be fixed now:

1. Set the chart value `workstore.importBreakGlass: true` in the gonk
   HelmRelease (it sets `GONK_WORKSTORE_IMPORT_BREAK_GLASS=1` on the
   bootstrap). The import still runs; if it fails, it exits 0 and stamps the pod
   `break_glass` with the error. The controller starts ON POSTGRES, on the last
   complete import, and every dispatch and sweep logs `BREAK GLASS` at WARN.
   Break-glass never puts the controller on bd for state.
2. What is lost: bd writes made by a rolled-back bd-backed gonk-gate since the
   last complete import. With no rollback since, nothing.
3. Fix bd, remove the value; the next pod start imports normally.

Break-glass is REFUSED (exit 1) for a work store no import has ever completed
against: starting would run on an empty store. There, remove the work-store
mount instead -- revert the gonk chart to the release before the one that mounts
Secret `gonk-workstore` (the image may stay: a release-1-or-later gonk-gate with
no `GONK_WORKSTORE_DSN_FILE` runs on bd exactly as before), which is safe
precisely because Postgres holds nothing yet.

**Conflicts: seeing and settling them.** A conflicted anchor keeps its
Postgres record and is reported at ERROR on every import (every pod start)
until settled. `gonk-gate workstore-survey` with `GONK_WORKSTORE_DSN_FILE` set
adds `import_plan`: what an import would do now -- written, in sync,
quarantined, and `conflicts` with `conflict_details` (what each side did) --
without writing anything, and exits 1 if there are conflicts. Settle one with

```sh
gonk-gate workstore-resolve --anchor <anchor> --take bd|postgres
```

`--take bd` writes bd's record into the work store (the replaced record is logged
at WARN in full); `--take postgres` keeps the work store's record and records bd
as seen, so the next import keeps Postgres and the next export writes it to bd.
Both need `GONK_WORKSTORE_DSN_FILE`, `GONK_BEAD_REPO_DIR` and bd's Dolt env, as
the export does (see the runbook below).

**Rollback runbook.** Before reverting the controller image to a bd-backed
gonk-gate:

1. Export, from the running controller. `kubectl exec` does not inherit the
   variables PID 1's shell exported, so bd's are given explicitly, the same way
   the bootstrap gives them (`GONK_WORKSTORE_DSN_FILE`, `GONK_BEAD_REPO_DIR`
   and `GC_DOLT_*` are already in the container's env):

   ```sh
   kubectl -n gonk exec deploy/gonk-controller -c controller -- sh -c '
     BEADS_DIR=/city/.beads \
     BEADS_DOLT_SERVER_HOST="$GC_DOLT_HOST" BEADS_DOLT_SERVER_PORT="$GC_DOLT_PORT" \
     BEADS_DOLT_SERVER_USER="$GC_DOLT_USER" \
     BEADS_DOLT_PASSWORD="$(cat /etc/gonk/secrets/dolt-gc/gc-password)" \
     gonk-gate workstore-export'
   ```

   Exit 0 = bd is current. Exit 1 = an anchor failed or conflicted: read the
   ERROR lines; do not roll back until they are resolved.
2. Revert the image. Records written to Postgres between step 1 and the old pod
   starting are NOT in bd.
3. On the next roll-forward the import takes what the bd-backed binary wrote
   (bd changed, Postgres did not) and reports anything both changed.

Without step 1, the bd-backed binary starts from bd as it was at the last
import: a record Postgres moved on (a finished session, a new reservation) looks
stale to it -- a sweep can re-judge a finished record, a dispatch can miss a
reservation.

## Storage in bd (`BdCLI`)

One bd bead per anchor, found by the label `gonk-anchor:<BeadAnchor>`.

**Record comments.** Each write appends one comment:

```
<!-- gonk-state {JSON} -->
```

The JSON is the record's fields, flat, plus one envelope field:

| Field | Type | Meaning |
|---|---|---|
| `IfRevision` | string, optional | Present only on a `PutIf` write: the revision the write was conditioned on. Absent means unconditional. Present-and-empty is a real condition ("absent or pre-revision"). |

**Resolution rule** (`resolveComments`). Readers fold the gonk-state comments in
the order `bd comments --json` returns them (bd 1.1.0: `created_at ASC, id ASC`):

- an unconditional comment becomes the current record;
- a conditional comment becomes current only if its `IfRevision` equals the
  `Revision` of the record current at that point; otherwise it is ignored (a
  write that lost its race);
- a comment that does not decode is skipped if a later one decodes, and is an
  error if it is the last.

Records written before 2026-09-13 carry no `IfRevision`, so for them the rule
is the old "last comment wins".

Why the log decides: bd's order is a client-side timestamp, not commit order,
and a `BdCLI` write is several separate bd processes. A writer cannot hold a
lock across them, so its own earlier check cannot guarantee anything; the
rule makes a lost conditional write inert for every reader regardless.

**State label.** `gonk::<State>` is a search index for `List`, not the source
of truth. Writes add the new label before appending the comment, then remove the
old labels, then re-read the current record and add its label if it is missing
(`settleLabels`). So a write killed part-way leaves the bead carrying its
current record's label, and two unkilled writers cannot leave the current
record unlabelled. `List(st)` returns only beads whose current record is
in `st`, and adds the correct label to a bead whose label disagrees with its
record. It never removes one.

**Known limits.**

- Labels are not atomic with the record. A writer killed, or failing a bd call,
  between a label removal or append and its final settle, while another writer's record is current, can
  leave that record with no `gonk::` label; so can a record written with an
  empty `State` (gonk-p7qh's `go-ffoe`). No `List` finds such a bead.
- **Commit order.** bd orders comments by a client timestamp taken inside the
  insert, not by commit. Two writes whose inserts overlap, with the earlier
  one's commit stalled past the other writer's verification read, re-sort when
  it lands: a write already reported applied can retroactively lose. Since
  both the sweep's writes and the dispatch's reservation are conditional, both
  writers can have been told "applied" -- a sweep fire alongside a reservation
  no reader sees. Needs a commit-ordered sequence bd does not expose
  (gonk-vred).
- **The first write to an anchor is not a compare-and-swap** (found by
  gonk-t41 running `PutIfSuccessIsFinal` against `BdCLI`). `PutIf(r, "")` finds
  the bead with `bd list` and creates one when there is none; two writers that
  both list before either creates make TWO beads for one anchor, and both are
  told they applied (`bd_notfinal_test.go` pins it). This, with the commit-order
  hole above, is why `BdCLI` is not run against `RunCompareAndSwap`, and why
  gonk-gate's conditional writes are compare-and-swap only on the Postgres work
  store. `BdCLI` is still gonk-gate's store wherever `GONK_WORKSTORE_DSN_FILE`
  is unset (the table above) -- including the chart default,
  `workstore.enabled: false`.
- **Rolling back to a pre-2026-09-13 gonk-gate is not safe for the store.** A
  conditional write that lost its race after its own check leaves an inert
  comment in the log. The old reader ("last comment wins") does not know
  `IfRevision`, so on any bead whose LAST gonk-state comment is such a loser, it
  resurrects the stale copy -- the gonk-p8w8 overwrite, after the fact. Before a
  rollback, list beads whose last gonk-state comment carries `"IfRevision"` and
  whose record differs from what `resolveComments` returns; any later write to
  the bead clears the condition.
