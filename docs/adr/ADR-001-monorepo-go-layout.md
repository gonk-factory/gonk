# ADR-001: Single Go module, cmd/ + pkg/ layout

Status: accepted 2026-07-12

Spec 7.1 sketches intake/ and meter/ as top-level roots. We use one Go module
at the repo root with cmd/gonk-intake and cmd/gonk-meter and shared pkg/
libraries instead. Rationale: the .gonk.yml schema, attribution tags, and both
services must change atomically (spec goal: schemas evolve together); one
module means one go.sum, one CI cache, and cross-package refactors in one
commit. pack/, images/, chart/, test/ keep their spec names (not Go code).
