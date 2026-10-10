# Workflow scope: path classes, layers, and the per-dispatch lift

Reference for `pkg/scope` and the `scope` configuration surfaces. Design:
`docs/plans/2026-10-03-gonk-066-workflow-scope.md` (slices S1 and S3, owner
decisions in sections 9 and 10) and
`docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md` (part SCOPE, section 4.2).
Ecosystem profiles (section 1.1, bead `gonk-wj14`): design section 11.

Workflow scope answers one question deterministically: **which paths may this
dispatch's change touch.** No model is asked, and a pass grants nothing.

STATUS (2026-10-05): the classifier, the fold, the three configuration
surfaces and the lift are built and tested. **Nothing consumes them yet**: no
agent that ships today has a pack row, the broker's gate chain still calls
`effects.ValidatePaths` with no lift, and no code-writing workflow exists. A
`scope:` section written today is validated and stored, and otherwise inert.
See "Not built" at the end.

## 1. Path classes

Every repository path has exactly one class. The first class, in this order,
whose patterns match is the path's class (`scope.Builtin`, `Classifier.Classify`).

| # | Class | Built-in patterns | Sensitivity |
|---|---|---|---|
| 1 | `control` | `.git/**`, `**/CODEOWNERS`, `**/.gonk.yml`; plus every layer's `protected_paths`; plus any path that is not a clean relative path | protected -- **floor** |
| 2 | `secret` | `**/*.pem`, `**/*.key`, `**/.env*`, `**/id_rsa*`, `**/*.p12`, `**/secrets/**` | protected -- **floor** |
| 3 | `ci` | `.gitlab-ci.yml`, `.gitlab-ci.yaml`, `.gitlab/**`, `.github/**`; plus the base `.gitlab-ci.yml`'s `include: local:` paths; plus `ci_paths` | protected -- liftable |
| 4 | `deploy` | `chart/**`, `charts/**`, `deploy/**`, `k8s/**`, `**/Dockerfile*`, `**/kustomization.yaml` | sensitive |
| 5 | `test-config` | the `test-config` row of EVERY shipped profile (1.1) | sensitive |
| 6 | `deps` | the `deps` row of EVERY shipped profile (1.1) | sensitive |
| 7 | `agent-context` | `.agent/**` | sensitive |
| 8 | `generated` | files whose base content has `// Code generated ... DO NOT EDIT.` on line 1; plus the `generated` row of EVERY shipped profile (1.1) | sensitive |
| 9 | `test` | the `test` row of each SELECTED profile (1.1); plus `test_paths` | normal |
| 10 | `docs` | the `docs` row of each SELECTED profile (1.1); plus `docs_paths` | normal |
| 11 | `source` | the `source` row of each SELECTED profile (1.1); plus the operator's `source_paths` | normal |
| 12 | `other` | anything unmatched | sensitive |

Rows 1-4 and 7 are fixed (`core` in `pkg/scope/class.go`). Rows 5, 6, 8, 9, 10
and 11 come from the ecosystem profiles of section 1.1. With the default
selection (`go`, `docs-md`) rows 9-11 are what they were before profiles
existed: `**/*_test.go` and `**/testdata/**`; `**/*.md`, `docs/**` and
`CHANGELOG*`; `**/*.go`.

- **protected**: refused for every dispatch. `control` and `secret` are the
  floor: nothing lifts them. `ci` alone can be lifted, per dispatch (section 4).
- **sensitive**: writable only by a workflow whose pack row lists the class.
- **normal**: what an ordinary code-writing workflow is given.

Differences from the design doc's table 2.1, both deliberate:

- `secret` is matched before `ci`. The table predates the decision that a
  dispatch may lift `ci`; with `ci` first, `.gitlab/deploy.key` would be a
  liftable path.
- `.gonk.yml` is matched by basename anywhere in the tree (the table says the
  root file), to agree exactly with `pkg/effects/protected.go`.
  `TestFloorAgreesWithScopeClassifier` holds the two lists together.

Matching is case-sensitive, as GitLab paths are.

### 1.1 Ecosystem profiles

The built-in table used to know one ecosystem, Go: a Python or TypeScript file
was `other`, which no workflow may write, and `package.json` was not `deps`.
A **profile** is a named set of patterns for one ecosystem (a language and its
tooling) or one documentation style. gonk ships a fixed menu of them:

| Profile | Kind | What it is | Deterministic test-weakening predicate | Onboarding marker files |
|---|---|---|---|---|
| `go` | ecosystem | Go modules | **yes** | `go.mod` |
| `python` | ecosystem | Python (pytest conventions) | no | `pyproject.toml`, `setup.py`, `requirements.txt` |
| `node` | ecosystem | JavaScript and TypeScript (npm, yarn, pnpm, bun) | no | `package.json` |
| `docs-md` | docs | Markdown, and everything under `docs/` | -- | -- |
| `docs-rst` | docs | reStructuredText | -- | -- |
| `docs-txt` | docs | plain-text documentation (`*.txt`) | -- | -- |
| `docs-adoc` | docs | AsciiDoc | -- | -- |

**Two kinds of row, and the difference is the whole design:**

- **Sensitive rows** (`test-config`, `deps`, `generated`; a profile may also
  ship `deploy` rows, none does today) of EVERY shipped profile **always
  apply**, on every project, whether or not the project selected the profile.
  No configuration key turns them off, at any layer. `package.json` is `deps`
  and `conftest.py` is `test-config` on a Go project.
- **Permissive rows** (`test`, `docs`, `source`) apply only for the profiles a
  project **selected** with `scope.profiles` in its `.gonk.yml` (5.2), out of
  the menu its operator layers allow with `scope.allowed_profiles` (5.3).

The patterns, exactly as shipped (`pkg/scope/profile.go`;
`TestReferenceDocumentsEveryProfilePattern` fails if this table and the code
differ):

| Profile | Class | Patterns | Applies |
|---|---|---|---|
| `go` | `test-config` | `Makefile`, `**/*.mk`, `.golangci.yml`, `**/testmain_test.go`, `go.work` | **always** |
| `go` | `deps` | `go.mod`, `go.sum`, `vendor/**` | **always** |
| `go` | `test` | `**/*_test.go`, `**/testdata/**` | selected |
| `go` | `source` | `**/*.go` | selected |
| `python` | `test-config` | `**/conftest.py`, `**/pytest.ini`, `**/tox.ini`, `**/noxfile.py`, `**/setup.cfg`, `**/.coveragerc` | **always** |
| `python` | `deps` | `**/requirements*.txt`, `**/constraints*.txt`, `**/pyproject.toml`, `**/setup.py`, `**/Pipfile*`, `**/poetry.lock`, `**/uv.lock`, `**/.pypirc` | **always** |
| `python` | `generated` | `**/*_pb2.py`, `**/*_pb2_grpc.py`, `**/*_pb2.pyi` | **always** |
| `python` | `test` | `**/tests/**`, `**/test_*.py`, `**/*_test.py` | selected |
| `python` | `source` | `**/*.py` | selected |
| `node` | `test-config` | `**/jest.config.*`, `**/vitest.config.*`, `**/playwright.config.*`, `**/cypress.config.*`, `**/karma.conf.*`, `**/.mocharc*`, `**/tsconfig*.json`, `**/.eslintrc*`, `**/eslint.config.*` | **always** |
| `node` | `deps` | `**/package.json`, `**/package-lock.json`, `**/npm-shrinkwrap.json`, `**/yarn.lock`, `**/pnpm-lock.yaml`, `**/pnpm-workspace.yaml`, `**/bun.lockb`, `**/bun.lock`, `**/.npmrc`, `**/.yarnrc*`, `**/node_modules/**` | **always** |
| `node` | `generated` | `**/*.min.js`, `**/*.min.css`, `**/*.js.map`, `**/*.css.map` | **always** |
| `node` | `test` | `**/*.test.js`, `**/*.test.jsx`, `**/*.test.ts`, `**/*.test.tsx`, `**/*.test.mjs`, `**/*.test.cjs`, `**/*.spec.js`, `**/*.spec.jsx`, `**/*.spec.ts`, `**/*.spec.tsx`, `**/*.spec.mjs`, `**/*.spec.cjs`, `**/__tests__/**` | selected |
| `node` | `source` | `**/*.js`, `**/*.jsx`, `**/*.ts`, `**/*.tsx`, `**/*.mjs`, `**/*.cjs` | selected |
| `docs-md` | `docs` | `**/*.md`, `docs/**`, `CHANGELOG*` | selected |
| `docs-rst` | `test-config` | `docs/**/conf.py`, `doc/**/conf.py` | **always** |
| `docs-rst` | `docs` | `**/*.rst`, `CHANGELOG*` | selected |
| `docs-txt` | `test-config` | `**/CMakeLists.txt` | **always** |
| `docs-txt` | `docs` | `**/*.txt`, `CHANGELOG*` | selected |
| `docs-adoc` | `docs` | `**/*.adoc`, `**/*.asciidoc`, `CHANGELOG*` | selected |

Notes on the choices:

- The Python and Node sensitive rows match a file name **anywhere** in the tree
  (`**/package.json`), because monorepos nest manifests. The Go rows are the
  ones that already existed and are root-only (`go.mod`), unchanged.
- `node` test files are listed by extension (`*.test.ts`, `*.spec.js`, ...)
  rather than as `**/*.test.*`: `api/openapi.spec.yaml` is not a test.
- `docs-rst`, `docs-txt` and `docs-adoc` do **not** include `docs/**`; only
  `docs-md` does (it always did). A Sphinx tree keeps `conf.py`, which the
  documentation build executes, next to the prose: `docs/**/conf.py` is
  `test-config` on every project, including one that selects `docs-md`.
- `docs-txt` makes every `*.txt` documentation except the ones a sensitive row
  names: `requirements*.txt` and `constraints*.txt` are `deps`,
  `CMakeLists.txt` is `test-config`.
- A profile row may name only `deploy`, `test-config`, `deps`, `generated`,
  `test`, `docs` and `source`. It may not name `control`, `secret` or `ci`
  (nor `agent-context` or `other`): every class a profile speaks for is matched
  after the three protected classes, so no profile can move a path out of one
  and none can pull a path into `ci`, the one class a dispatch can lift. The
  menu is validated when the package is initialised.

**Selection.** `scope.profiles` absent means `[go, docs-md]`. An empty list
selects none: no file is `source`, `test` or `docs`. Selecting chooses among
rows gonk ships; it cannot name a path. Free-form `source_paths` stays
operator-only (owner decision Q3).

**Precedence is the class order, unchanged**: the floor, then every sensitive
row, then the selected permissive rows, then `other`. So:

- a path a sensitive row of ANY profile matches is never `source`, `test` or
  `docs`, whatever is selected: with `python` selected, `setup.py` is `deps`
  and `tests/conftest.py` is `test-config`;
- selecting a profile can only turn `other` into `test`, `docs` or `source`
  (`TestSelectingProfilesOnlyMovesOther`, over all 128 selections);
- nothing moves out of `control`, `secret` or `ci`: `.gitlab/package.json` is
  `ci`, `secrets/index.js` is `secret`.

**What changed for a project that selects nothing.** The permissive rows are
exactly what they were: `go` and `docs-md` alone reproduce the table that
existed before profiles, byte for byte, over a golden list of 2165 invented
paths (`TestGoAndDocsMdAloneAreTheTableBeforeProfiles`). What is new on every
project is the other profiles' sensitive rows: a path one of them names moves
to `test-config`, `deps` or `generated` -- always a class matched EARLIER than
the one it had, never into `source`, `test` or `docs`, never out of a
protected class. In the golden list that is 709 paths, recorded line for line
in `pkg/scope/testdata/builtin_default_tightened.golden`
(`TestDefaultSelectionDiffersFromBeforeOnlyByTightening`). Most were `other`
(`package.json`, `requirements.txt`, `tsconfig.json`). The rest were:

| Was | Now | Example | Why it matters |
|---|---|---|---|
| `docs` | `deps` / `test-config` / `generated` | `docs/package.json`, `docs/conf.py` | was writable as documentation |
| `test` | `deps` / `test-config` / `generated` | `testdata/package.json`, `pkg/x/testdata/conftest.py` | was writable as a test fixture; a Go project keeping such fixtures now needs a human for them |
| `source` | `deps` | `node_modules/left-pad/main.go` | -- |
| `deps` | `test-config` | `vendor/lib/jest.config.js` | a lateral move between two sensitive classes |
| `agent-context` | `deps` / `test-config` | `.agent/package.json` | a lateral move; `.agent/` files with a manifest's name are no longer `agent-context` |

**Testing methodology is a recorded property.** Each ecosystem profile says,
explicitly, whether a deterministic test-weakening predicate (`SC-WEAKEN`: a
test removed, skipped or build-tagged away) exists for its tests. Only `go`
says yes. `scope.WeakeningPredicate(profile)` reads it, and
`Decision.WeakeningDecidable()` answers the question a merge policy asks: a
`source` or `test` path is decidable only when a profile that declares a
predicate classified it (a file that is `source` through `source_paths` alone
is not), and any other class has no tests to weaken. No predicate is built
here: for `python` and `node` the answer is "not caught, human review, no
auto-merge" (gonk-2usw U4), stated rather than silent.

**Onboarding proposes, a human disposes.** The onboarding merge request
pre-fills `scope.profiles` from marker files at the root of the default branch
and says which it looked for and found (`intake.ProfileDetection`,
`scope.ProposeProfiles`). It probes exact root-level names only -- intake has
no tree-listing call -- so `requirements*.txt` is probed as `requirements.txt`
and nothing in a subdirectory is seen; the documentation style is not
detected (`docs-md` is always proposed); no marker found means the default.
**Nothing is detected at dispatch time**: after the merge only the committed
`.gonk.yml` counts (`TestProfilesAreDetectedOnlyAtOnboarding`).

**Where the profile data lives.** In `pkg/scope/profile.go`, as a Go table
compiled into the binary: reviewed through gonk code review and baked into the
image like the pack, but not a file. That keeps section 6 true as written
(`pkg/scope` opens one file), and it means the sensitive rows cannot be
missing, stale or shadowed by a copy a session could write.

### Path patterns

Used by every `*_paths` key, by bead scopes and by CI include paths.

| Syntax | Meaning |
|---|---|
| `*` | any run of characters inside one path segment (never `/`) |
| `?` | one character inside a segment |
| `**` | as a whole segment: zero or more segments. As the LAST segment it needs at least one, so `chart/**` is everything under `chart/` and not a file named `chart` |

No character classes, no escapes (`[`, `]`, `\` are refused), no leading `/`,
no `.` or `..` segments, at most 256 bytes per pattern and 64 patterns per key.

### Base facts

Two classes depend on repository content. The controller reads it at the
pinned base SHA and passes it in as `scope.BaseFacts`; the session's working
tree is never consulted.

| Fact | Helper | Used for |
|---|---|---|
| the base `.gitlab-ci.yml`'s local includes | `scope.CILocalIncludes(blob)` | class `ci` |
| which base files are generated | `scope.IsGenerated(blob)` | class `generated` |

## 2. Layers and the fold

`scope.Fold(scope.Input)` combines three layers into a `scope.Effective`.

| Layer | Lives in | Read by | May | May not |
|---|---|---|---|---|
| built-in floor | `pkg/scope/class.go`, `pkg/effects/protected.go` | -- | define `control`, `secret`, `ci` | be configured away |
| pack row | `[scope]` in `pack/agents/<agent>/effect-shape.toml`, baked into the image | `scope.LoadRow(packDir, agent)` | set the workflow's maximum | be read from `/city`, a session or a batch |
| built-in profiles | `pkg/scope/profile.go`, compiled in | -- | say what is `test-config`, `deps`, `generated` everywhere, and offer `source`/`test`/`docs` rows to select | be edited, extended or switched off by configuration |
| operator | `instance.scope` and `groups.<path>.scope` in the operator config | `opercfg.Load` | tighten; declare `source_paths`; veto a lift; restrict the profile menu (`allowed_profiles`) | widen past the pack row; select profiles |
| project | `scope:` in `.gonk.yml`, read at a pinned commit | `gonkcfg.Load` | tighten; reclassify `source`/`other` into `test`/`docs`; select profiles from the allowed menu (`profiles`) | widen past the pack row; set `source_paths`, `deny_lift` or `allowed_profiles`; select a profile the operator does not allow |
| bead / controller | `Effective.Narrow`, `Effective.WithBead` | controller code | narrow | widen |

Fold rules:

| Quantity | Rule |
|---|---|
| enablement | AND. `Input.Enabled` is the workflow's `actions.*` value as `gonkcfg.Resolve` already folded it; a disabled scope allows nothing and grants no lift |
| allowed classes | the pack row's, intersected with every layer's `classes` that is set |
| caps | the pack row's, minimised against every layer's |
| protected patterns | union of every layer's `protected_paths` |
| lift | granted only if the pack row lists the class in `liftable` AND the project opted in (`Input.LiftOptIn`) AND no layer lists it in `deny_lift` |
| bead scope | intersection: a path must be inside every scope declared with `WithBead` |
| profile menu | the shipped menu, intersected with every operator layer's `allowed_profiles` that is set |
| profile selection | the project's `profiles`, which must be inside the menu (else the fold is refused); with no `profiles` key, the default `[go, docs-md]` intersected with the menu. `scope.SelectProfiles`; `Effective.Profiles()` |

`Input.BaseSHA` must be a full commit SHA. A branch name is refused: scope
policy is read at a commit the controller pinned.

**No fold is wider than the pack row** (`TestNoFoldIsWiderThanThePackRow`, a
property test over generated layer combinations).

An invalid layer is an error from `Fold`, and before that from the loader:

- `.gonk.yml`: `gonkcfg.Load` fails, the meter marks the project `invalid`,
  deletes its key and denies every decision (`invalid-config`). No spend.
  (`TestInvalidScopeDeniesAtDispatch`.) An unknown profile is refused here.
- `.gonk.yml` against the operator config: a project that selects a profile
  outside its operator layers' `allowed_profiles` is refused by the meter when
  it registers, and again on every re-resolve -- so an operator who narrows the
  menu invalidates the projects that no longer fit, with the same `invalid`
  state. (`TestProfileSelectionIsCheckedAtDispatch`,
  `TestNarrowingAllowedProfilesInvalidatesOnReresolve`.)
- operator config: `opercfg.Load` fails and gonk-meter does not start.

## 3. The reclassification lattice

`scope.CheckMove(from, to, layer)`. A configured pattern can move a path out of
its built-in class only along these edges (design section 3.2; owner decision
Q3: "downward only -- `source`/`other` -> `test`/`docs`; everything else
operator-only").

| Key | Moves a path into | From | Who |
|---|---|---|---|
| `protected_paths` | `control` | any class | operator, project |
| `ci_paths` | `ci` | `source`, `other`, `test`, `docs` | operator, project |
| `test_paths` | `test` | `source`, `other` | operator, project |
| `docs_paths` | `docs` | `source`, `other` | operator, project |
| `source_paths` | `source` | `other` | operator only |

Everything else is refused. In particular nothing leaves `control`, `secret`,
`ci`, `deploy`, `test-config`, `deps`, `agent-context` or `generated` except
toward `control` -- `deploy -> docs` is refused -- and `ci_paths` cannot pull a
`deploy` or `secret` path into the one protected class a dispatch can lift.

The lattice is enforced twice:

1. **When the policy is loaded** (`scope.Policy.Validate`): a pattern AIMED at
   a class it may not move -- `docs_paths: ["deploy/**"]` -- makes the whole
   configuration invalid. "Aimed at" is decided by classifying sample paths
   built from the pattern, so it catches plain mistakes, not every case. A
   project's patterns are probed under the project's own profile selection:
   `docs_paths: ["**/package.json"]` is refused under every selection, and
   `test_paths: ["notes/*.txt"]` is refused for a project that selected
   `docs-txt` (that would be `docs -> test`).
2. **Per path** (`Classifier.Classify`): the guarantee. A pattern that matches
   a path it may not move changes nothing for that path. `test_paths: ["**"]`
   loads, and still moves nothing out of a locked class.

Keys are consulted in the order of the table; the first that matches and is
permitted decides.

**Every reclassification is logged**, by the classifier itself (owner decision
Q3, provisional -- "let's see how it plays out"):

| Message | Level | Fields |
|---|---|---|
| `scope: path reclassified` | INFO | `path`, `from`, `to`, `layer` (`instance`/`group`/`project`), `key`, `pattern`, `base_sha` |
| `scope: reclassification refused` | WARN | the same, plus `reason` |

## 4. The per-dispatch lift

The protected-path denylist is a default. A dispatch the controller chose may
be granted a lift of the `ci` class (design section 9, owner decisions a/b):
the owner's example is a pipeline-fix session permitted to change a pipeline
definition.

```go
eff, err := scope.Fold(in)                           // pack row + layers
lift, err := eff.GrantLift(scope.Grant{              // controller, from the dispatch
    Trigger: "pipeline-fix", Workflow: "configuration",
    Classes: []scope.Class{scope.ClassCI},
})
err = effects.ValidateProtectedPathsWithLift(batch, lift)
ok := eff.Allows(class, lift)
```

- **The lift comes from the controller.** `scope.Lift` has only unexported
  fields and its JSON/YAML/text decoders refuse; the one way to hold a non-empty one is
  `Effective.GrantLift`. A batch has no field that could carry a lift or any
  scope policy, and `ParseBatch` refuses a key that tries
  (`TestBatchCannotCarryALiftOrScopePolicy`, `TestLiftCannotBeDecoded`).
  Nothing is read from `.agent/` or issue text.
- **The floor is not liftable**: `GrantLift` refuses any class but `ci`, a pack
  row cannot list one in `liftable`, and `ValidateProtectedPathsWithLift`
  refuses `.gonk.yml`, `CODEOWNERS` and `.git/` whatever lift it is given.
- A lift is honoured only for a path that is `ci` as written:
  `.gitlab/deploy.key` (`secret`) and `./.gitlab-ci.yml` (not canonical) stay
  refused.
- A grant must name its trigger and workflow; log `lift.String()` with the
  session (`ci (trigger=pipeline-fix workflow=configuration)`, or `none`).
- With no lift, `effects.ValidateProtectedPaths` and `effects.ValidatePaths`
  behave exactly as before, error text included
  (`TestNoLiftRefusalsAreByteIdentical`).

## 5. Configuration keys

### 5.1 Pack row: `[scope]` in `effect-shape.toml`

Baked into the image; `effects.LoadShape` skips the table (as it skips
`[trajectory]`) and `scope.LoadRow` reads it. An agent with no `[scope]` table
has the empty row: no class, no lift.

| Key | Type | Meaning |
|---|---|---|
| `classes` | list of class names | the non-protected classes the workflow may ever write. Listing `control`, `secret` or `ci` is an error |
| `liftable` | list; only `"ci"` is accepted | protected classes a dispatch of this workflow may be granted |
| `max_files` | integer >= 0 | files one change may touch |
| `max_lines` | integer >= 0 | added + removed lines of the broker-computed diff |
| `max_deletes` | integer >= 0 | files one change may delete |
| `max_file_bytes` | integer >= 0 | size of a file that may be changed |
| `max_patch_bytes` | integer >= 0 | bytes of proposed content across the batch |

A row that lists any class or liftable must state all five caps. An unknown
key under `[scope]` is an error.

```toml
[scope]
classes = ["source", "test", "docs"]
liftable = ["ci"]
max_files = 3
max_lines = 40
max_deletes = 0
max_file_bytes = 262144
max_patch_bytes = 16384
```

### 5.2 Project: `scope:` in `.gonk.yml`

Schema: `docs/schemas/gonk-config.v1.schema.json` (`properties.scope`).
Go: `gonkcfg.Policy.Scope` (`scope.Policy`).

| Key | Type | Effect |
|---|---|---|
| `scope.classes` | list of `deploy`, `test-config`, `deps`, `agent-context`, `generated`, `test`, `docs`, `source`, `other` | intersected with the pack row; `[]` allows none |
| `scope.caps.max_files`, `.max_lines`, `.max_deletes`, `.max_file_bytes`, `.max_patch_bytes` | integer >= 0 | minimised against the pack row |
| `scope.protected_paths` | patterns | matching paths become `control` |
| `scope.ci_paths` | patterns | matching `source`/`other`/`test`/`docs` paths become `ci` |
| `scope.test_paths` | patterns | matching `source`/`other` paths become `test` |
| `scope.docs_paths` | patterns | matching `source`/`other` paths become `docs` |
| `scope.profiles` | list of `go`, `python`, `node`, `docs-md`, `docs-rst`, `docs-txt`, `docs-adoc`; no duplicates | PROJECT ONLY. The profiles whose `source`, `test` and `docs` rows apply here (1.1). Absent: `[go, docs-md]`. `[]`: none. An unknown name, or one outside the operator's `allowed_profiles`, makes the configuration invalid. Pre-filled by the onboarding merge request; never detected afterwards |

```yaml
scope:
  profiles: [python, docs-rst]
  classes: [source, test, docs]
  caps: { max_files: 2, max_lines: 30 }
  protected_paths: ["internal/auth/**"]
  ci_paths: ["ci/**"]
  test_paths: ["internal/testutil/**"]
```

### 5.3 Operator: `scope:` under `instance` and `groups.<path>`

Schema: `docs/schemas/gonk-operator.v1.schema.json` (`$defs.policy.properties.scope`).
Every project key above except `scope.profiles` (project-only; refused here),
plus:

| Key | Type | Effect |
|---|---|---|
| `scope.source_paths` | patterns | matching `other` paths become `source`. For a language gonk ships a profile for, the project selecting that profile is the better tool; this key is for what no profile covers |
| `scope.deny_lift` | list; only `ci` | no dispatch under this layer may be granted that lift |
| `scope.allowed_profiles` | list of profile names (as `scope.profiles`); no duplicates | OPERATOR ONLY. The menu projects under this layer may select from. Absent: the whole shipped menu. `[]`: no profile. INTERSECTED across `instance` and every matching group, so a layer can only shrink it. It restricts only `source`/`test`/`docs` rows: the sensitive rows of every shipped profile apply regardless |

```yaml
instance:
  scope:
    allowed_profiles: [go, python, docs-md, docs-rst]
groups:
  platform/legacy:
    scope:
      allowed_profiles: [go, docs-md]   # this group: the intersection, [go, docs-md]
```

### 5.4 Not a configuration key

The project's opt-in to a lift is `Input.LiftOptIn`, set by the controller. For
the pipeline-fix workflow it will be mapped from `pipeline_fix.allow_ci`
(gonk-2usw plan 3.1, part CONFIG); this package defines no `.gonk.yml` key for
it.

## 6. Where policy is read from

Only the baked pack, the operator config, and `.gonk.yml` at the pinned base
SHA. `pkg/scope` imports no other gonk package, opens exactly one file (the
pack's `effect-shape.toml`, in `LoadRow`), and names `.agent` only in the
pattern that classifies that directory. `pkg/scope/contract_test.go` fails if
any of those stops being true.

The ecosystem profiles (1.1) are not read from anywhere: they are a Go table
in `pkg/scope/profile.go`. Which of them a project selected is read from its
`.gonk.yml`, like the rest of `scope:`. The marker files onboarding looks at
are read once, by `pkg/intake`, to PROPOSE that selection in a merge request a
human merges; `pkg/scope` never sees a repository.

## 7. Not built

| Item | Where it is planned |
|---|---|
| Operator-defined profiles ("an operator layer ... may add profiles of its own in the operator config", 066 section 11 item 3) | Not built with gonk-wj14. The menu is the shipped one. An operator who needs a language gonk does not ship has the free-form `source_paths`, `test_paths` and `docs_paths` it always had |
| A test-weakening predicate for any ecosystem (`SC-WEAKEN`) | 066 S5. Profiles only RECORD whether one exists (`scope.WeakeningPredicate`); `go` says yes and nothing implements it yet |
| Detecting the documentation style at onboarding; detecting a marker that is not at the repository root | Not built: intake has no tree-listing call. The onboarding merge request says so |
| Any consumer: `SC-CLASS`, `SC-PROT` over layer patterns, `SC-SIZE`, `SC-BEAD` and the broker wiring | 066 S4; gonk-2usw parts EFFECTS and APPLY |
| A pack row for any shipped agent | with each code-writing agent (gonk-2usw part AGENT) |
| Fetching `.gonk.yml` and the base facts at the pinned SHA | the dispatch (066 S0b; gonk-2usw part DISPATCH). `Fold` requires the SHA; it does not fetch |
| Following includes of includes, and a project whose CI configuration file is not `.gitlab-ci.yml` (a GitLab project setting) | `CILocalIncludes` reads ONE blob. The controller must pass every file that is pipeline definition in `BaseFacts.CILocalIncludes` (entries are matched literally and as patterns); resolving the full set is the dispatch's job, with `pkg/cidef` (gonk-2usw part CIDEF). Until then such a file has its built-in class |
| `.gonk.yml` `verify:` (test commands) | 066 Q4 is still tentative; no schema until it is designed |
| `.gonk.yml` `scope.labels` (label-declared bead scope) | 066 Q2: a later per-project opt-in |
| `scope.auto_merge_classes` | 066 S8 / gonk-rk1m |
