# The proposed-effects contract (`pkg/effects`)

What an agent session returns instead of writing to GitLab, and the
deterministic gates that decide whether any of it is applied. This page is
the wire format as built; what the pipeline-fix workflow does with a batch
that passes is in `docs/reference/gonk-pipeline-fix.md`. The Go types in
`pkg/effects/effects.go`, `pkg/effects/diagnosis.go` and `pkg/effects/fix.go`
are authoritative; the tests in `pkg/effects/*_test.go` are the executable form.

Last changed: 2026-10-12, the v0.2.1 release preparation (a key of the batch
object stated twice is refused). Before that: 2026-10-12, gonk-37kg part D (a batch is exactly one JSON
object: trailing text is refused, gonk-s0td; a diagnosis carrying any other
kind's key is refused, by reflection, gonk-8yux; the diagnosis note quotes the
summary and the remedy in a fence, gonk-d2ct). Before that: 2026-10-05, gonk-2usw part EFFECTS (added the pipeline-fix
vocabulary: kinds `edit`, `fix_meta`, `scope_request`; verdicts `fix`,
`needs-scope`; the fix gate and its rule codes; the citation-only `paths`
rule). Before that: 2026-10-04, gonk-tios P5 (the `diagnosis` kind, the
`pipeline` target type, the reserved `gonk::pipeline-*` labels, and the
citation-only `files` rule).

## Where the batch travels

The agent prints its batch as single-line JSON between the sentinels
`GONK_BATCH_START` and `GONK_BATCH_END`; gonk-gate's sweep extracts the LAST
fenced batch (`extractBatch`, `cmd/gonk-gate/broker_apply.go`) and parses it with
`effects.ParseBatch`. A run with no fenced batch, or one that does not parse,
applies nothing.

The text between the sentinels is EXACTLY ONE JSON object. White space around
it is allowed (space, tab, CR, LF); anything else after the object's closing
brace -- prose, a second object, a stray bracket -- refuses the whole batch at
parse (`ErrBatchTrailingData`), and the retry feedback says so without quoting
the text.

## Batch

```json
{"verdict": "reply-only", "effects": [ <effect>, ... ]}
```

| Field | Type | Rule |
|---|---|---|
| `effects` | array | Ordered. Every element's `kind` must be known, or the whole batch is refused at parse. |
| `verdict` | string, optional | `reply-only` (default when absent), `code-change`, `close`, and -- for the fix agent only, see "The pipeline-fix vocabulary" -- `fix`, `needs-scope`. Unknown -> refused at parse. |
| (each key) | -- | Stated at most once on the batch object. `{"effects":[],"effects":[...]}` is refused at parse (`ErrBatchDuplicateKey`), with retry feedback that names the rule and not the key; keys are compared without regard to case, as the decoder matches them. A key repeated INSIDE one effect is not covered: the last value is taken. |

Unknown keys anywhere -- on the batch, an effect, or an evidence item -- are
refused at parse (`DisallowUnknownFields`). Literal newline/CR/tab bytes inside
a JSON string are escaped before decoding (a normalisation that is an identity
on valid JSON).

## Effect kinds

`Effect` is one superset struct; which fields mean anything depends on `kind`.

| `kind` | Fields | Applied by | Notes |
|---|---|---|---|
| `comment` | `body` | issue note | Quick-action lines refused; body <= 64 KiB (`ValidateComments`). |
| `label` | `add`, `remove` | issue labels | Normalised into the project's namespace; reserved labels refused. |
| `new_issue` | `title`, `body` | -- | Reserved: parses, no apply path. |
| `file` | `path`, `content` | scaffold commit | Under `.agent/` only, canonical, <= 64 KiB/file, 256 KiB/batch (`ValidatePaths`). |
| `diagnosis` | `classification`, `summary`, `evidence`, `remedy`, `files` | pipeline landing (gonk-tios D9) | Below. |
| `edit` | `path`, `old`, `new` | fix commit on a `gonk/pipefix-*` branch (gonk-2usw) | Exact-match replacement in an existing file. Below. |
| `fix_meta` | `reason` | fix merge request description | Below. |
| `scope_request` | `paths`, `reason` | needs-scope note | Below. |

Which kinds an agent may emit, and how many, is its
`pack/agents/<agent>/effect-shape.toml` (`effects.Validate`); a kind the shape
does not list is forbidden.

## Targets

| Field | Type | Meaning |
|---|---|---|
| `target_iid` | int, optional | The resource the effect acts on. `0`/absent = the session's primary target. |
| `target_type` | string, optional | `issue`, `merge_request`, `pipeline`. Absent = the kind's default. Requires a non-zero `target_iid`. |

`effects.ValidateTargets` refuses an effect naming a resource the session was
not handed, or a type its kind may not name:

| Kind | May name | Default |
|---|---|---|
| `comment`, `label`, `new_issue` | `issue` | `issue` |
| `diagnosis` | `pipeline` | `pipeline` |
| `file` | nothing (writes to the repository) | -- |
| `edit`, `fix_meta`, `scope_request` | nothing (the controller chooses the branch, the MR and where notes land) | -- |

For `pipeline`, `target_iid` is the GitLab pipeline **ID** (instance-wide), not
the per-project pipeline iid. A diagnosis may name only the pipeline the
session diagnoses, or nothing; where it LANDS (an MR note, or the ref's
tracking issue) is chosen by the controller, never by the agent. There is no
`commit` target type.

## The `diagnosis` effect

```json
{"effects":[{"kind":"diagnosis",
  "classification":"test",
  "summary":"An assertion in the example suite expects an outdated value.",
  "evidence":[{"job":"unit-tests","line":"--- FAIL: TestSum (0.00s) got 3 want 4"},
              {"job":"unit-tests","fact":"failure_reason"}],
  "remedy":"Update the expected value in the example test.",
  "files":["pkg/example/sum_test.go"]}]}
```

(Synthetic example. It is the fixture of `TestDiagnosisWireFormat`.)

| Field | Type | Rule |
|---|---|---|
| `classification` | string | Closed set, below. Unknown or empty -> refused **at parse**. |
| `summary` | string | What failed and why. Non-blank, <= 1500 characters (runes). Rendered QUOTED (a three-backtick fence it cannot close), never as markup: see "How untrusted text is rendered" in `gonk-pipeline-fix.md`. |
| `evidence` | array | 1..8 items. |
| `evidence[].job` | string | The name of a failed job of the diagnosed pipeline, as recorded at dispatch. May be empty only on a `pipeline_error` fact. |
| `evidence[].line` | string | A verbatim line from that job's log excerpt. |
| `evidence[].fact` | string | One of `failure_reason`, `runner`, `tags`, `stage`, `allow_failure`, `duration`, `history`, `definition`, `pipeline_error`. |
| `remedy` | string | What a human should change, where. Non-blank, <= 1500 characters. Rendered quoted, like `summary`. |
| (others) | | A diagnosis carrying ANY other effect key is refused: `body`, `add`, `remove`, `title`, `path`, `content`, `old`, `new`, `paths`, `reason`, and any key added to the effect object later (the rule reads the diagnosis's own field list, it does not name the foreign ones). `target_iid` and `target_type` are not foreign: the target rule decides them. |
| `files` | array of string | <= 10 repository-relative paths the diagnosis points at: non-empty, no NUL, no leading `/`, no `..` segment, already canonical (`a/./b`, `a//b` refused). Required (>= 1) for `code`, `test`, `configuration`. |

Each evidence item carries EXACTLY ONE of a non-blank `line` or a `fact`. If
any failed job has a non-empty excerpt, at least one item must be a `line`.

**Classes** (`effects.Classifications()`, in this order):

| Class | Meaning |
|---|---|
| `code` | The product code at this commit is wrong. |
| `test` | Tests or fixtures are wrong while the product behaves as intended. |
| `flaky` | Nondeterministic: the same job/commit both passes and fails. |
| `infrastructure` | The execution environment the job ran in failed or lacked something. |
| `configuration` | The pipeline definition or the project's CI settings are wrong. |
| `unknown` | Could not determine; the summary says what was checked. |

The classification is a LABEL, never a policy input: nothing retries, cancels
or skips on it, and there is no retry/cancel effect kind.

### Gates, and who owns each

| Gate | Where | What |
|---|---|---|
| Parse | `effects.ParseBatch` | Known kind; known classification on a diagnosis; no unknown keys. |
| Shape | `effects.Validate` + the agent's effect-shape | e.g. exactly one diagnosis, nothing else. |
| Target | `effects.ValidateTargets` with `Targets{Pipelines: {<recorded id>: true}}` | As above. |
| Diagnosis shape | `effects.ValidateDiagnosis(batch, DiagnosisContext{FailedJobs})` | Every rule in the field table. Each refusal wraps one `ErrDiagnosis*` sentinel. |
| Grounding | `pipelinectx.Grounded(excerpt, line)` (gonk-tios P1), called per `effects.DiagnosisLines(batch)` | Each `line` occurs (normalised) within ONE line of THAT job's stored excerpt. NOT in `pkg/effects`. |
| Files exist, landing render, secrets, comment gate | gonk-gate `applyPipelineBatch` (gonk-tios P7) | -- |

`DiagnosisContext` is built only from the dispatch record (failed job names,
and whether each had a non-empty excerpt), never from the batch.

### A diagnosis's `files` are citations, never writes

The protected-path denylist (`ValidateProtectedPaths`, `pkg/effects/protected.go`)
exists to stop gonk WRITING repository control files. A diagnosis writes
nothing: its `files` are citations rendered into a note, and a `configuration`
diagnosis must be able to cite `.gitlab-ci.yml`, `.gitlab/...`, `CODEOWNERS` or
`.gonk.yml`. So:

- `files` is NOT in `effectPaths`; it is listed in `citationOnlyPathFields`
  with the reason. `TestEveryPathFieldIsProtected` requires every Path- or
  File-named field of `Effect` to be in one or the other (and every allowlist
  entry to name a real field with a non-blank reason), so a new path field
  cannot slip through silently.
- A diagnosis citing any protected path passes `ValidateProtectedPaths`,
  `ValidatePaths` and `ValidateDiagnosis` (`TestDiagnosisMayCiteProtectedPaths`).
  The sanity rules for a citation are `ValidateDiagnosis`'s: relative,
  canonical, no `..`, no NUL, no leading `/`.
- A diagnosis can never be applied as a write. There are two constructors of
  repository writes and neither reads a diagnosis: `effects.FileWrites(batch)`
  reads only a `file` effect's `path`/`content`, and `effects.ApplyEdits`
  reads only an `edit` effect's `path`/`old`/`new`
  (`TestOnlyEditEffectsBecomePatchedFiles`). The scaffold apply (`applyScaffoldFiles`, the only
  effect-driven commit in gonk-gate) takes its writes from it.
  `TestOnlyFileEffectsBecomeWrites` and `TestScaffoldApplyNeverWritesADiagnosis`
  hold this. A diagnosis carrying any other kind's field (`path`, `content`,
  `body`, `add`, `remove`, `title`) is refused by `ValidateDiagnosis`.
- The pipeline apply chain (`applyPipelineBatch`, gonk-tios D8) does not call
  `ValidatePaths`/`ValidateProtectedPaths` for a diagnosis's files; it calls
  `ValidateDiagnosis` (sanity) and grounding, then checks each file exists at
  the checkout SHA.

### The denylist is a default; a dispatch may lift `ci`, and only `ci`

`ValidateProtectedPaths(batch)` takes no lift and is what every agent that
exists today goes through. `ValidateProtectedPathsWithLift(batch, lift)` is the
same gate for a dispatch the CONTROLLER granted a `scope.Lift` (gonk-066
section 9 a/b): a `ci` entry of the denylist (`.gitlab-ci.yml`,
`.gitlab-ci.yaml`, `.gitlab/`, `.github/`) passes when the lift covers `ci`.
The floor -- `.gonk.yml`, `CODEOWNERS`, `.git/` -- passes under no lift. The
lift is an argument: a `Batch` has no field that could carry one, and
`ParseBatch` refuses a key that tries. Classes, layers, the lift and every
configuration key: `docs/reference/gonk-scope.md`.

`effect-shape.toml` may carry one more non-kind table besides `[trajectory]`:
`[scope]`, the agent's workflow-scope row. `LoadShape` skips it by name
(`effects.ScopeSection`); `scope.LoadRow` reads it.

## The pipeline-fix vocabulary (`edit`, `fix_meta`, `scope_request`)

What a fix session may PROPOSE (gonk-2usw plan section 4.1). Built in
`pkg/effects` (`fix.go`, `edit.go`, `fixgate.go`) as pure functions. The agent
pack is `pack/agents/pipefix`: its `effect-shape.toml` lists `edit` {0,10},
`fix_meta` {0,1}, `scope_request` {0,1} and carries the one pack `[scope]`
row. **This vocabulary is live.** For a project that has opted in
(`actions.pipeline_fixes: true`, `docs/reference/gonk-pipeline-fix.md`),
gonk-sweep decides after a diagnosis whether a fix may be dispatched,
gonk-gate runs a read-only `pipefix` session (`cmd/gonk-gate/dispatch.go`,
`broker_pipefix_dispatch.go`), and the apply
(`cmd/gonk-gate/broker_pipefix_apply.go`) runs `effects.ValidateFix` and
`effects.PlanFix` over the batch it returns, then the gates listed under "The
gates the apply adds" below, before it commits anything. Only the `pipefix`
agent may emit these kinds (see "No existing agent may use it"); a project
that has not opted in never has such a session dispatched.

The pipefix shape's minimums are all 0, because which kinds a batch must carry
depends on its verdict. The shape gate (`effects.Validate`) therefore ACCEPTS
an empty pipefix batch; `effects.ValidateFix` (run by `PlanFix`) is what
refuses it. The fix apply runs both (gates A1 and `FX-SHAPE` below).

```json
{"verdict":"fix","effects":[
  {"kind":"edit","path":"pkg/example/sum.go","old":"\treturn a - b\n","new":"\treturn a + b\n"},
  {"kind":"fix_meta","reason":"Sum subtracted instead of adding."}]}
```

```json
{"verdict":"needs-scope","effects":[
  {"kind":"scope_request","paths":["go.mod"],"reason":"The fix needs a dependency bump."}]}
```

(Synthetic examples. They are the fixtures of `TestFixWireFormat`.)

| Kind | Field | Type | Rule |
|---|---|---|---|
| `edit` | `path` | string | An EXISTING file. Clean repository-relative path: non-empty, no NUL, no leading `/`, no `..`, already canonical. Refused, never repaired. |
| | `old` | string | Non-empty. Must occur EXACTLY ONCE in the file as it stands after the batch's earlier edits to the same path -- byte for byte, overlapping occurrences counted, no whitespace- or case-insensitive matching. |
| | `new` | string | The replacement; may be empty (a removal). `old != new`. Neither may contain NUL or invalid UTF-8. |
| `fix_meta` | `reason` | string | Non-blank, <= 600 characters (runes). Rendered by the controller into the merge request description. (Named `rationale` until gonk-58bp: the fix agent's model could not spell it, and the old key is now refused like any unknown key.) |
| `scope_request` | `paths` | array of string | 0..5 clean repository-relative paths the session says it would have needed. CITATIONS: rendered as text, never written. |
| | `reason` | string | Non-blank, <= 600 characters. |
| (any of the three) | (others) | | Carrying any field of another kind (`content`, `body`, `files`, a `path` on a `fix_meta`, a `reason` on an `edit` ...) is refused -- by reflection over `Effect`, so a field added later is foreign until listed. |

**The verdict/kind rule** (in `effects.Validate`, so it applies to every agent):

| Verdict | Requires |
|---|---|
| `fix` | >= 1 `edit` (<= 10), exactly 1 `fix_meta`, no `scope_request` |
| `needs-scope` | exactly 1 `scope_request`, no `edit`, no `fix_meta` |
| any other / absent | no `edit`, no `fix_meta`, no `scope_request` |

Not expressible, on purpose: file CREATION and DELETION (an `edit` needs an
existing blob; one that empties a file is refused), and any branch / commit /
merge_request / label / assignee / reviewer / approve / retry / cancel kind. A
fix that needs one of those answers `needs-scope`. An edit outside what the
dispatch allows is a refusal (gate-failed); it is never converted into a
needs-scope.

### No existing agent may use it

`fix` and `needs-scope` are in the parser's closed verdict set and the three
kinds in its closed kind set, for every agent. What keeps them out of triage,
scaffold and pipeline batches is the shape gate those apply chains already
call: their `effect-shape.toml` lists none of the three kinds, and the
verdict/kind rule above ties each verdict to a kind. Held by
`TestExistingAgentShapesRefuseTheFixVocabulary` (every shape file in the pack
except `pipefix`'s) and, at the brokers,
`Test{Triage,Scaffold,Pipeline}BatchCarryingTheFixVocabularyIsRefused`
(`cmd/gonk-gate/fix_vocabulary_test.go`).

### The fix gate and its rule codes

`effects.PlanFix(batch, envelope, baseBlobs, secrets)` runs every predicate
below, in this order, and returns either a `FixPlan` (the files to commit,
their `DiffFacts`, the reason or the scope request) or the FIRST refusal.
Everything except the batch is supplied by the controller: nothing here reads
policy, citations or a lift from the batch.

| Input | Type | Where it must come from |
|---|---|---|
| `FixEnvelope.Scope` | `scope.Effective` | `scope.Fold` (pack row, operator, `.gonk.yml` at the base SHA), narrowed to the diagnosis class's row (`Narrow`), with any declared path scope (`WithBead`). Its `Caps` are the size caps. |
| `FixEnvelope.Base` | `scope.BaseFacts` | the base `.gitlab-ci.yml`'s local includes |
| `FixEnvelope.Lift` | `scope.Lift` | `scope.Effective.GrantLift`; zero = nothing lifted |
| `FixEnvelope.Cited` | `[]string` | the landed diagnosis's cited files, from the RECORD |
| `BaseBlobs` | `map[path][]byte` | each edited file's blob at the pinned base SHA; no entry = no such file |
| `[]Secret` | `NewSecret(name, value)` | the session's model key, the bot token |

A refusal is a `*effects.Refusal{Rule, Sub, Effect, Path, Detail}`
(`RuleOf(err)`, `SubOf(err)`, `AsRefusal(err)`; `Code()` renders
`RULE(sub)`). It is always the agent's failure (gate-failed). A failure of
OURS -- an unreadable blob, an invalid policy -- is an ordinary error from
whoever read it and never a `*Refusal`. `effects.Rules()` is the closed list:

| Rule | Sub codes | Refuses when |
|---|---|---|
| `FX-SHAPE` | `verdict`, `kind`, `edit-count`, `foreign-field`, `paths-count`, `paths-form`, `reason-empty`, `reason-cap` (the last two for a `fix_meta` and a `scope_request` alike), and the verdict/kind arms `fix-needs-edit`, `fix-needs-meta`, `fix-with-request`, `needs-scope-needs-request`, `needs-scope-with-edit`, `needs-scope-with-meta`, `edit-needs-fix`, `meta-needs-fix`, `request-needs-needs-scope` | the batch is not a well-formed fix batch |
| `SC-FORM` | `path`, `old-empty`, `identical`, `nul`, `utf8` | an edit is malformed |
| `SC-PROT` | the class: `ci`, `control`, `secret` | the path is on the protected-path denylist and not lifted for this dispatch (`ValidateProtectedPathsWithLift`'s rule), or is of a protected class: `control`, `secret`, or `ci` without a lift this scope could grant |
| `SC-CLASS` | the class, e.g. `test`, `test-config`, `generated` | the path's class is not in the envelope. `generated` is decided from the base blob's first line. |
| `SC-CITED` | -- | the path is not one of the diagnosis's cited files (exact string; a directory or a pattern cites nothing) |
| `SC-BEAD` | -- | the path is outside a declared path scope |
| `SC-SIZE` | `max_files`, `max_lines`, `max_patch_bytes`, `max_file_bytes` | files changed, added + removed lines, total bytes of `new`, or a file's size before OR after, exceeds the cap (`>`; exactly at the cap passes) |
| `SC-SECRET` | the secret's name, or `private_key`, `gitlab_token`, `aws_access_key` | an ADDED line holds one of the given secrets (exact bytes) or a high-confidence key shape. The refusal never repeats the value. |
| `ED-MISSING` | -- | no file at the path at the base (no creation) |
| `ED-ENCODING` | -- | the file is binary or not UTF-8 |
| `ED-NOMATCH` | -- | `old` occurs nowhere in the file |
| `ED-AMBIGUOUS` | -- | `old` occurs more than once |
| `ED-EMPTIES` | -- | the edits leave the file empty |
| `ED-NOOP` | -- | the edits leave the file unchanged |

`SC-*` are gonk-066 section 2.2's codes; `SC-CITED` is added for the
cited-files bound, and `ED-*` split what 066 calls `SC-BASE` ("does not apply").
`effects.FixViolations(...)` returns every refusal rather than the first, for
logs. The phases are also callable alone -- `ValidateFix` (shape),
`ValidateEditPaths` (paths, before any blob is read), `EditPaths` (which blobs
to read), `ApplyEdits`, `ComputeDiffFacts`, `ValidateDiff` -- but `PlanFix`
re-runs all of them and is the gate.

**`DiffFacts`** are computed by the broker from base blob and patched blob,
never stated by the agent: per file `Path`, `Class`, `Generated`, `Edits`,
`BaseBytes`, `ResultBytes`, `Added`, `Removed`, `AddedLines`; and the totals
`FilesChanged`, `LinesAdded`, `LinesRemoved`, `NewBytes`, `Edits`. Lines keep
their terminator (a changed final newline is a changed line), and the line
diff is minimal up to 1000 changed lines, past which it reports the whole
differing region as an upper bound (`Exact == false`).

### The gates the apply adds, and their rule codes

`PlanFix` is given the batch, the envelope and the base blobs. The apply
(`cmd/gonk-gate/broker_pipefix_apply.go`, `broker_pipefix_gates.go`; plan 5.3)
holds what it is not given -- the recorded context, tree modes, a Go parser, a
YAML parser, the merged CI definition -- and refuses with these codes. Like
`PlanFix`'s they are the agent's failure (gate-failed), reported as
`RULE(sub)`, recorded on the fix record as `PipelineFix.Refusal`, and a batch
refused by any of them has written NOTHING to GitLab (except `CI-DEF`, below).

| Gate | Rule | Sub codes | Refuses |
|---|---|---|---|
| A1 | `FX-PACK` | -- | the batch does not fit the pack's effect shape for the `pipefix` agent (a kind it does not list, more `edit` effects than its maximum). The pack shape ALONE accepts an empty batch -- every minimum is 0 -- so the apply runs `ValidateFix` as well (`FX-SHAPE`) |
| A2 | `FX-TARGET` | -- | an effect names a target. (Unreachable today: `ValidateFix` refuses `target_iid` / `target_type` on a fix kind as `FX-SHAPE` first. Kept so a change there cannot open it.) |
| -- | `FX-TRAJ` | -- | the trajectory floor (`require_any_read_for`), only when `GONK_ENFORCE_TRAJECTORY=1` |
| A4 | `SC-SHOWN` | the recorded reason: `SC-CITED` for a path the fix was never given, else the rule the prompt-time check recorded for the file (`ED-NOMATCH` for an empty file, `SC-SIZE`, `ED-ENCODING`, ...) | an edit to a file the prompt did not list under "Files you may change" (`PipelineFix.Context.Files`). Runs before any blob is read for the batch |
| A5 | `SC-FORM` | `symlink`, `submodule`, `directory`, `parent-not-a-dir`, `not-a-regular-file` | an edit path that is not a REGULAR FILE at the base commit, read through `pkg/basefacts` (tree entries with modes). A symlink's blob is editable text; editing it retargets the link |
| A6 | `SC-WEAKEN` | `removed-test`, `added-skip`, `build-constraint`, `unparseable` | Go only (`pkg/weaken`). In a `test`-class Go file: a `Test*` / `Benchmark*` / `Fuzz*` / `Example*` function removed or renamed; more `Skip` / `Skipf` / `SkipNow` calls in any function than before, or a test's first statement turned into an early `return` -- a bare one, or an `if` with a branch that is nothing but `return` (`if testing.Short() { return }`, whatever the condition), directly or leading a bare block; a guarded return that was already there is not a finding, one made unconditional is; any change to the file's build constraints; a result that does not parse. In a `source`-class Go file: a changed build constraint only. For a changed source or test path with NO predicate (any other language; a non-Go file under the `go` profile) nothing is refused and the fact is RECORDED (`PipelineFix.Weakening = not-decidable`) |
| A7 | `CI-PRE` | `unparseable`, `allow_failure`, `when`, `retry`, `or-true`, `set-e`, `exit-0` | when a `ci`-class file changed: the patched file does not parse as YAML, or an ADDED line carries `allow_failure`, `when: manual` / `never`, a `retry:` key, an appended or-true (`||` followed by `true`, `/bin/true`, `/usr/bin/true`, `:` or `exit 0` as a whole word, whatever follows it on the line), `set +e`, or `exit 0`. A courtesy early refusal of the obvious spellings; the authoritative check is `CI-DEF` and the exit check |
| A8 | `TX-MARKER` | -- | the `fix_meta` reason, or a `scope_request`'s reason or path, reads as a gonk marker (`<!-- gonk...`) |
| A8 | `TX-SECRET` | the kinds `pipelinectx.Redact` found | ... holds what `Redact` matches. Refused, never rendered with a hole |
| A8 | `TX-COMMENT` | -- | ... fails the comment gate (`ValidateComments`: a quick-action line, an oversize body) |
| 5.4 step 2 | `CI-DEF` | `violation` | AFTER the commit, BEFORE the merge request: `pkg/cidef` over the merged definition at the base and at the commit reports a violation (a base job gone, a GATING key changed, an EXEC key changed on a job outside the target set), or GitLab judges the new definition invalid. The branch the apply created is deleted and no merge request is opened |

Not refusals -- errors of OURS, never a `*Refusal`, classified infra-failed
(plan 7.3): the project no longer opted in; the envelope folded again does
not name the classes, lift and caps on the record (the lift is minted again
by `GrantLift` and never read from the record); the recorded context does not
hash to `ContextSHA256`, or the diagnosis, a job log or a cited file no longer
hashes to it (gate A9); a read of the base that fails; a CI definition that
cannot be resolved; any GitLab write that fails after the commit.

### A `scope_request`'s `paths` are citations, never writes

Like a diagnosis's `files`: `paths` is listed in `citationOnlyPathFields`, so
a needs-scope answer may NAME `.gitlab-ci.yml` or `.gonk.yml`
(`TestScopeRequestMayCiteProtectedPaths`), and no apply path writes it. An
`edit`'s `path`, by contrast, IS a write path: `effectPaths` returns `path`
for every kind, so the denylist covers `edit` exactly as it covers `file`.

## Reserved labels

Controller-written labels a model may not propose (`ReservedLabels`,
`cmd/gonk-gate/broker_label.go`): `fix-queued`, `needs-maintainer`, `denied`,
`onboarding`, the `verdict-*` labels, and, for pipelines,
`pipeline-failing` plus `pipeline-<class>` for each of the six classes
(derived from `effects.Classifications()`). For pipeline fixes (gonk-2usw):
`needs-human` and `pipeline-fix` (also the `verdictLabels` of the
`needs-scope` and `fix` verdicts), `fix-class-confirmed` (written by a human
only), and the fix merge request's `fix-verifying`, `fix-awaiting-run`,
`fix-verified`, `fix-new-failure`, `fix-inconclusive`, `fix-failed` and
`ci-exec-changed`.

A `fix` or `needs-scope` verdict is acted on by the fix apply alone.
`applyVerdict`, the issue-shaped verdict path, does nothing with one and says
so at ERROR: there is no issue of the bead's to label or close.
