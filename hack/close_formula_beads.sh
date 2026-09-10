#!/usr/bin/env bash
# List (default) or close (--close) beads in a Gas City bead store whose
# gc.routed_to metadata names an agent that only ever received FORMULA-routed
# work.
#
# WHY THIS EXISTS (ADR-007 §3, gonk-p2e). gonk-dispatch used to pour a
# formula order for issue-triage/scaffold/mention-reply, and the pack routed
# each formula's work bead to a pool named after the agent
# (triage/scaffold/mention), plus control-dispatcher for graph-v2's
# workflow-control lane. ADR-007 §3 deleted the whole formula layer:
# formulas, [steps.check], the three formula orders, and control-dispatcher
# (see pack/, cmd/gonk-gate/dispatch.go, cmd/gonk-gate/check.go's deletion).
# The broker path that replaced triage/scaffold creates an agent SESSION
# directly (gcapi.CreateSessionRequest) -- it does not pour an order, and it
# neither creates nor reads a routed work bead. mention-reply and
# control-dispatcher have no live path at all any more.
#
# The result: ANY bead whose gc.routed_to is one of these four names is
# permanent, unclaimable demand. Gas City's default scale_check counts
# READY, UNASSIGNED beads by gc.routed_to to size each pool, and
# min_active_sessions=0 is only a floor, not a cap -- so stale routed beads
# keep a pool alive forever even though nothing will ever claim them. This
# is exactly the bug gonk-p2e found and hand-closed once (eight beads,
# 2026-08-03: go-5oi, go-gjh, go-aw4, go-u3w, go-0d9, go-h28, go-4y5,
# go-ykq); this script is the general-case tool gonk-p2e's item 2 asked for,
# so the NEXT formula-to-broker migration (or this deletion catching beads
# gonk-p2e's one-off cleanup missed) does not need another by-hand pass.
#
# *** SAFE BY DEFAULT. *** With no flags this script only LISTS matching
# beads (id, title, gc.routed_to) and exits 0 -- IT NEVER CLOSES ANYTHING
# unless you pass --close. Read the list output, confirm it is what you
# expect, and only then re-run with --close.
#
# Usage:
#   hack/close_formula_beads.sh                 # dry run: list matching beads
#   hack/close_formula_beads.sh --close          # actually close them
#   hack/close_formula_beads.sh --close --reason "custom reason"
#
# Env (matching cmd/gonk-gate's own bd config -- pkg/beadstore.BdCLI, and
# cmd/gonk-gate/main.go's GONK_BD_BIN / GONK_BEAD_REPO_DIR):
#   GONK_BD_BIN         bd executable (default: bd, resolved via PATH)
#   GONK_BEAD_REPO_DIR  working directory bd runs in -- the checkout holding
#                       the CITY's Dolt bead store, i.e. the SAME one
#                       cmd/gonk-gate points at in production. Defaults to
#                       the current directory. THIS IS NOT gonk-project's own
#                       dev-task tracker (.beads/issues.jsonl in this repo);
#                       running this against the wrong directory will find
#                       nothing (gc.routed_to is a Gas City bead concept, not
#                       one this repo's own issues use).
#
# Requires: bd (the MIT beads CLI, pinned in images/versions.env), python3
# (stdlib json only -- no extra dependency) to parse `bd list --json`.

set -euo pipefail

BD_BIN="${GONK_BD_BIN:-bd}"
BD_DIR="${GONK_BEAD_REPO_DIR:-.}"

# The four agent names a deleted formula used to route work to (pool ==
# agent name -- see pack/pack.toml's former "NOTE ON POOLS", removed in the
# same change that deleted the formula layer). Any bead with gc.routed_to
# in this set today cannot be claimed by anything gonk still runs.
DELETED_FORMULA_AGENTS=(triage scaffold mention control-dispatcher)

CLOSE=false
REASON="ADR-007 §3: formula layer deleted; gc.routed_to names a deleted formula agent, this bead can never be claimed (see gonk-p2e)"

usage() {
  sed -n '2,42p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --close)
      CLOSE=true
      shift
      ;;
    --reason)
      [ $# -ge 2 ] || { echo "error: --reason requires a value" >&2; exit 2; }
      REASON="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

run_bd() {
  ( cd "$BD_DIR" && "$BD_BIN" "$@" )
}

echo "== close_formula_beads: $BD_BIN in $BD_DIR ==" >&2
if [ "$CLOSE" = true ]; then
  echo "== MODE: CLOSE (will call 'bd close' on every matching bead) ==" >&2
else
  echo "== MODE: LIST (dry run -- pass --close to actually close anything) ==" >&2
fi

total=0
for agent in "${DELETED_FORMULA_AGENTS[@]}"; do
  echo >&2
  echo "-- gc.routed_to=$agent --" >&2

  raw="$(run_bd list --metadata-field "gc.routed_to=$agent" --limit 0 --json)"

  # Parse with python3's stdlib json, not grep/sed: the list must be exact,
  # not a best-effort scrape of `bd`'s human-readable columns. A row missing
  # "id" is a bug in this script's assumptions, not a bead to silently skip.
  # $raw travels as argv, not stdin -- stdin here is the python SCRIPT
  # itself (the heredoc), so the two must not collide.
  ids_and_titles="$(python3 - "$agent" "$raw" <<'PYEOF'
import json
import sys

agent = sys.argv[1]
raw = sys.argv[2]
data = json.loads(raw)
# `bd list --json` returns either a bare array or {"issues": [...]} depending
# on version; handle both rather than guess wrong and print nothing.
if isinstance(data, dict):
    rows = data.get("issues", data.get("items", []))
else:
    rows = data

for row in rows:
    issue_id = row.get("id")
    if not issue_id:
        print(f"ERROR: a row for gc.routed_to={agent} has no id: {row}", file=sys.stderr)
        sys.exit(1)
    title = row.get("title", "")
    print(f"{issue_id}\t{title}")
PYEOF
)"

  if [ -z "$ids_and_titles" ]; then
    echo "   (none)" >&2
    continue
  fi

  while IFS=$'\t' read -r id title; do
    [ -n "$id" ] || continue
    total=$((total + 1))
    if [ "$CLOSE" = true ]; then
      echo "   closing $id ($title)" >&2
      if run_bd close "$id" --reason "$REASON" >/dev/null; then
        echo "   closed  $id" >&2
      else
        echo "   FAILED to close $id -- left open, check by hand" >&2
      fi
    else
      echo "   would close $id ($title)" >&2
    fi
  done <<<"$ids_and_titles"
done

echo >&2
if [ "$CLOSE" = true ]; then
  echo "== closed (attempted) $total bead(s) routed to a deleted formula agent ==" >&2
else
  echo "== $total bead(s) would be closed -- re-run with --close to do it ==" >&2
fi

# The count is the one piece of machine-readable output on stdout, so a
# caller (or a commit message) can capture it without scraping the log
# lines above: `n=$(hack/close_formula_beads.sh)`.
echo "$total"
