# ADR-002: .gonk.yml precedence semantics

Status: accepted 2026-07-12

Spec 5.4 states the one-line rule ("most specific wins, ceilings only tighten").
This ADR is the precise contract `gonkcfg.Resolve` implements.

- `enabled`: project must say true AND no coarser layer says false (kill switch).
- actions: effective = project value (unset -> false) AND group/instance allow (unset -> allow). Conservative: nothing runs unless the project opts in.
- budget: effective ceiling = MIN over layers where set; all unset = unlimited.
- ladder: base list = most specific layer that sets one; then intersect (preserving base order) with each coarser layer that sets one (allow-lists).
- **empty ladder fails closed**: a project whose effective ladder is empty can run no rung, so `Resolve` sets `Enabled = false`. This covers both cases: the layers' allow-lists do not overlap (empty intersection), and no layer configured a ladder at all. Per spec 5.4 the ladder is an allow-list ("cloud rungs must be listed"), so "nobody listed anything" means nothing is allowed, not everything. `Ladder` itself is left as whatever the fold produced (empty or nil); no rungs are fabricated. The resolver decides this once rather than trusting every downstream consumer to remember a `len(Ladder) == 0` check -- note that a naive `Ladder == nil` check would miss the non-nil zero-length slice an empty intersection produces.
- schedule, continuity, triage, provenance: most specific set value wins.
  Defaults: continuity=resume, label_prefix="gonk::", respond_to_mentions=true, commit_trailers=true, include_usage=false, schedule=none.

## DisabledReason

`Effective.DisabledReason` explains why a project is off. **Invariant: it is
non-empty if and only if `Enabled == false`**, and always `""` when the project
is enabled. Consumers may rely on either field alone.

The reason is chosen by first match, in this order -- a project killed by a
coarser layer says so rather than blaming its ladder:

1. instance sets `enabled: false` -> `disabled by instance policy`
2. group sets `enabled: false` -> `disabled by group policy`
3. project does not set `enabled: true` (nil or false) -> `disabled by project .gonk.yml`
4. effective ladder empty -> names each layer that constrained it, most specific
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
