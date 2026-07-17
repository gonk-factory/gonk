#!/bin/sh
# Thin wrapper. ALL logic is in the Go binary, where it is tested.
#
# KNOWN GAP (flagged, not silently worked around): Gas City's real
# [steps.check] exec invocation sets GC_BEAD_ID / GC_ITERATION / GC_WORK_DIR /
# GC_STORE_PATH / GC_ARTIFACT_DIR / GC_MOLECULE_DIR (gascity's
# internal/convergence/condition.go) -- NOT GC_WEBHOOK_ARG_* (that convention
# is exec ORDERS only: internal/webhookmatch/extract.go,
# internal/config/webhook.go). `gonk-gate check` (cmd/gonk-gate/main.go)
# currently reads project_id/issue_iid/bead_id/trigger exclusively via
# GC_WEBHOOK_ARG_*, which will be UNSET here. This formula's [steps.metadata]
# (see formulas/*.toml) stamps project_id/issue_iid/city_bead_id/trigger onto
# the checked bead specifically so a fix can read them back via GC_BEAD_ID --
# but that read-back is not implemented, because it needs the `bd` CLI's exact
# invocation surface, which pkg/beadstore's own doc comment says is
# "CONFIRMED AGAINST THE REAL bd in Task 6's container smoke test, not
# guessed at here". Task 6 must close this before [steps.check] can pass
# against the real loader.
set -eu
exec gonk-gate check
