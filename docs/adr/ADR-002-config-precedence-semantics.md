# ADR-002: .gonk.yml precedence semantics

Status: accepted 2026-07-12

Spec 5.4 states the one-line rule ("most specific wins, ceilings only tighten").
This ADR is the precise contract `gonkcfg.Resolve` implements.

- `enabled`: project must say true AND no coarser layer says false (kill switch).
- actions: effective = project value (unset -> false) AND group/instance allow (unset -> allow). Conservative: nothing runs unless the project opts in.
- budget: effective ceiling = MIN over layers where set; all unset = unlimited.
- ladder: base list = most specific layer that sets one; then intersect (preserving base order) with each coarser layer that sets one (allow-lists).
- schedule, continuity, triage, provenance: most specific set value wins.
  Defaults: continuity=resume, label_prefix="gonk::", respond_to_mentions=true, commit_trailers=true, include_usage=false, schedule=none.

`Policy` fields are pointers; nil means "this layer is silent," not "false." A
group that says `triage: false` vetoes; a group that simply doesn't mention
`triage` does not. `Schedule` is the one sub-policy where per-field silence
isn't representable — a non-nil `*Schedule` is treated as a whole unit, and
the nil check on `*Schedule` itself is the only "layer is silent" signal at
that level.
