# ADR-002: .gonk.yml precedence semantics

Status: accepted 2026-07-12

Spec 5.4 states the one-line rule ("most specific wins, ceilings only tighten").
This ADR is the precise contract `gonkcfg.Resolve` implements.

- `enabled`: project must say true AND no coarser layer says false (kill switch).
- actions: effective = project value (unset -> false) AND group/instance allow (unset -> allow). Conservative: nothing runs unless the project opts in.
- budget: effective ceiling = MIN over layers where set; all unset = unlimited. An explicit `0` is a real ceiling ("nothing is affordable"), never treated as "unset". **"Unlimited" is encoded as a concrete sentinel, not a special case**: `EffectiveBudget.MonthlyCostUSD` is `math.Inf(1)` (`+Inf`), and `MonthlyTokens`/`PerTaskTokens` are `TokenQuantity(math.MaxInt64)`. **`+Inf` is not JSON-serializable** -- `encoding/json.Marshal` returns an error (`json: unsupported value: +Inf`) rather than emitting a number, unlike the token fields, which marshal fine as an ordinary large integer. Plan 03's `gonk-meter` ledger/API will serialize `Effective.Budget`; it must special-case `+Inf` (e.g. `null`, a sentinel string, or omission) before calling `json.Marshal`, or an unlimited project will fail to serialize at all.
- **non-finite budget fails closed**: a `monthly_cost_usd` of `NaN` or `+/-Inf` at any layer is a config error. `Resolve` sets `Enabled = false` and pins the effective ceiling to `0`. This is not defensive noise: NaN loses every comparison (`NaN < x` is false), so a naive min-fold *skips* it and leaves the `+Inf` "unlimited" sentinel standing -- a NaN ceiling would silently mean **no budget limit at all**. `Validate` rejects non-finite floats in `.gonk.yml` before they reach the resolver, but operator-supplied instance/group `Policy` is not validated (see "Known gap"), so the resolver must not depend on them being unreachable.
- ladder: base list = most specific layer that sets one; then intersect (preserving base order) with each coarser layer that sets one (allow-lists).
- **empty ladder fails closed**: a project whose effective ladder is empty can run no rung, so `Resolve` sets `Enabled = false`. This covers both cases: the layers' allow-lists do not overlap (empty intersection), and no layer configured a ladder at all. Per spec 5.4 the ladder is an allow-list ("cloud rungs must be listed"), so "nobody listed anything" means nothing is allowed, not everything. `Ladder` itself is left as whatever the fold produced (empty or nil); no rungs are fabricated. The resolver decides this once rather than trusting every downstream consumer to remember a `len(Ladder) == 0` check -- note that a naive `Ladder == nil` check would miss the non-nil zero-length slice an empty intersection produces.
- schedule, continuity, triage, provenance: most specific set value wins.
  Defaults: continuity=resume, label_prefix="gonk::", respond_to_mentions=true, commit_trailers=true, include_usage=false, schedule=none.

## DisabledReason

`Effective.DisabledReason` explains why a project is off. **Invariant: it is
non-empty if and only if `Enabled == false`**, and always `""` when the project
is enabled. Consumers may rely on either field alone.

The reason is chosen by first match, in this order -- a project killed by a
coarser layer says so rather than blaming its budget or its ladder:

1. instance sets `enabled: false` -> `disabled by instance policy`
2. group sets `enabled: false` -> `disabled by group policy`
3. project does not set `enabled: true` (nil or false) -> `disabled by project .gonk.yml`
4. non-finite `monthly_cost_usd` at any layer, coarsest named first ->
   `invalid budget: monthly_cost_usd is not a finite number (instance)`
5. effective ladder empty -> names each layer that constrained it, most specific
   first, omitting layers that were silent:
   - `ladder empty: no rung allowed by all layers (project [opus], instance [qwen-local])`
   - `ladder empty: no ladder configured at any layer`

Note that an action veto (`actions.triage: false` at a coarser layer) disables
that *action*, not the project: `Enabled` stays true and `DisabledReason` stays
empty.

`Policy` fields are pointers; nil means "this layer is silent," not "false." A
group that says `triage: false` vetoes; a group that simply doesn't mention
`triage` does not. `Schedule` is the one sub-policy where per-field silence
isn't representable — a non-nil `*Schedule` is treated as a whole unit, and
the nil check on `*Schedule` itself is the only "layer is silent" signal at
that level.

## `schedule` is replaced as a unit, so `quiet_hours` needs its `timezone`

The consequence of that last paragraph is not academic. `Resolve` and
`opercfg.foldPolicy` both take the most specific non-nil `*Schedule` **whole**;
there is no per-field fold, so a layer that sets `quiet_hours` does not inherit
a coarser layer's `timezone`, and a layer that sets only `timezone` erases a
coarser layer's `quiet_hours`.

Downstream, `rung.ParseQuietHours` **refuses** an empty timezone -- an empty
zone would silently mean UTC, and a quiet-hours window that is silently in the
wrong zone is worse than none. `Service.resolveProject` routes that refusal
into `invalid()`, which marks the project invalid **and deletes its LiteLLM
virtual key**. So a schedule carrying `quiet_hours` with no `timezone` is not a
cosmetic defect: it is a delayed-action unregistration of every project that
inherits it, fired on the next hot-reload tick.

Two guards, one per side of the layering:

- **Project layer.** `gonk-config.v1` declares
  `"dependentRequired": { "quiet_hours": ["timezone"] }` on `schedule`. A
  `.gonk.yml` that sets a window without a zone is rejected by `gonkcfg.Load`,
  so the project author sees the message on their own MR instead of a
  mysterious 422 later. This is a **tightening**: a config that validated
  before now does not. `timezone` alone stays valid -- the dependency is
  one-directional on purpose, since naming a zone for a window an operator
  layer supplies is a legitimate (if lossy) thing to write.
- **Operator layers.** `opercfg.Load` applies the same rule to `instance` and
  to **every** `groups` entry, in `checkPolicy`. It has to be every layer: the
  fold means a `quiet_hours` with no zone under `groups:` bricks every project
  in that group exactly as an `instance:` one bricks the whole fleet. This is
  Go-level rather than schema-level so the message can name the offending
  layer; the operator schema and its checksum pin are unchanged.

**The merge semantics themselves are deliberately NOT changed.** A per-field
schedule fold would make `timezone`-only and `quiet_hours`-only layers compose,
but it would also make `*Schedule`'s nil check stop meaning "this layer is
silent", which is the one signal the whole sub-policy has. The representational
limit stands and is now guarded at both ends instead of papered over: a project
that wants an operator's quiet-hours window in its own zone must restate the
window.

## A group key must be able to name a project

`GroupFor` matches a group key exactly (`project == g`) or as a prefix on a
**segment boundary** (`strings.HasPrefix(project, g+"/")`). A key with a
trailing slash satisfies neither -- `"agentic/"` is not a project path, and the
prefix test degrades to `"agentic//"`, which no project path contains. An
interior empty segment (`"a//b"`) is the same defect.

Such a key used to load clean and then apply to nothing, so an operator who
tightened a ceiling on `agentic/` got silence rather than enforcement: a budget
escape wearing the costume of a config. `opercfg.Load` now rejects it, and
iterates group keys in sorted order so a config with several faults always
reports the same one.

## Untrusted input

`.gonk.yml` is project-authored content: any project in the instance can put
anything in it. `Validate` therefore rejects non-finite floats (`NaN`, `+/-Inf`)
before handing the document to the JSON Schema validator, because
`jsonschema/v6` (v6.0.2) crashes on them -- it builds a `big.Rat` from the value
to compare against a `minimum` keyword, gets `nil` back, discards the `ok`, and
dereferences it. One line of YAML would otherwise SIGSEGV the process that
enforces every project's budget and kill switch.

The check (`rejectNonFinite`) walks every scalar and every map/slice *value*
in the decoded document, but not map *keys*. This is not a gap: a non-finite
float used as a map key is not a string, so yaml.v3 decodes that map as
`map[any]any` rather than `map[string]any`; `jsonschema/v6` has no JSON
representation for a non-string-keyed map and rejects the whole map
("invalid jsonType") at the top of every recursive validation call, before
any keyword -- including the crashing `minimum` comparison -- runs against it
or anything nested under it. Verified against v6.0.2: `typeOf` falls through
`map[any]any` to `invalidType`, and `validator.validate()` checks
`typeOf(v) == invalidType` before evaluating any keyword. So a non-finite
float can reach the crash site only as a value, which `rejectNonFinite`
already catches; JSON has no NaN or Infinity, so a non-finite float is never
a valid document regardless of where it appears.

## Guarantees for callers holding an `Effective`

**`Resolve` does not alias its inputs.** The returned `Effective`'s `Schedule`
(a pointer) and `Ladder` (a slice) are deep copies of whatever the winning
layer held, not references to the caller's `Policy`/`ProjectConfig` values --
`TestResolveDoesNotAliasInputs` proves both. Plans 02/03 may hold an
`Effective` across goroutines (e.g. a long-running reconciliation loop next
to a webhook handler that re-resolves) without defensive copying or fear that
mutating one `Effective` reaches another, or reaches the layer inputs used to
produce it.

**`Effective` is a plain struct, not a smart constructor result.**
`Effective{Enabled: true}` -- no ladder, a zero-value `Budget`, `Continuity:
""` -- compiles and zero-values cleanly anywhere in the codebase. The
fail-closed invariants documented in this ADR (empty ladder implies disabled,
`DisabledReason` non-empty iff `!Enabled`, the `+Inf`/`MaxInt64` "unlimited"
sentinels, etc.) are guarantees `Resolve` establishes on its output, not
invariants the type enforces on construction. **Plans 02/03 must treat
`Resolve`'s return value as the only legitimate way to produce an
`Effective`** outside of tests; hand-building one (e.g. in a mock or a
shortcut path) can silently violate every invariant this ADR documents.

## Known gap (carried to Plan 03)

Nothing in-repo validates **operator-supplied instance/group `Policy`** -- there
is no `Load` equivalent for those layers, so they bypass both the JSON Schema and
the non-finite check above. `Resolve` fails closed on the one case that would
otherwise be dangerous (a non-finite cost ceiling silently meaning "unlimited"),
and a negative ceiling happens to fail closed on its own (every spend exceeds a
`-1` ceiling), so this is not exploitable today. But Plan 03 needs an
operator-config validation path; until it exists, the resolver -- not the loader
-- is the only thing standing between a malformed operator policy and a budget
escape.
