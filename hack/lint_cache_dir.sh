#!/bin/sh
# lint_cache_dir.sh -- print the golangci-lint cache directory for THE CHECKOUT
# IT IS RUN FROM. `make lint` passes the answer as GOLANGCI_LINT_CACHE.
#
# WHY (gonk-nn71). golangci-lint keeps one cache per user, keyed by what a
# package CONTAINS, and the issues in it carry the absolute path of the
# directory they were found in. Two checkouts of the same commit therefore
# share entries. Lint one, delete it, lint the other: the second run replays
# the first one's issues at paths that no longer exist, cannot read the source
# line, so cannot see the `//nolint` on it -- and exits 1 naming files in a
# directory that is gone. A gate that is red for a reason that is not in the
# tree. The usual way to gate a commit here is exactly that sequence: a fresh
# worktree, `make gate`, remove the worktree.
#
# So the cache is per checkout: a directory named after the checkout's own
# path. An entry in it was written by a run in THIS directory, so a path it
# replays is a path in this directory. A later checkout at the same path
# shares it, which is harmless for the same reason: entries are keyed by
# content, so a replayed issue is for the same bytes at the same path.
#
# The path is mirrored, not hashed: no collisions, and `ls` shows which
# checkout a cache belongs to, so the ones whose checkout is gone can be
# deleted by hand (each is a few tens of MB; nothing removes them).
#
# A CALLER'S OWN GOLANGCI_LINT_CACHE IS THE ROOT, NOT THE DIRECTORY. The
# checkout's path is appended to it as well. A run that asks for an empty
# cache still gets one (an empty root has nothing under it), and two checkouts
# handed the same value -- an `export` in a shell profile, a recipe copied
# between worktrees -- still do not share entries. Used as the directory
# itself, as it was in the first version of this script, the caller's value
# brought the defect straight back: measured, the same three stale findings.
set -eu
root="${GOLANGCI_LINT_CACHE:-}"
if [ -z "$root" ]; then
  base="${XDG_CACHE_HOME:-}"
  if [ -z "$base" ]; then
    if [ -z "${HOME:-}" ]; then
      echo "lint_cache_dir: none of GOLANGCI_LINT_CACHE, XDG_CACHE_HOME and HOME is set, so there is nowhere to keep a per-checkout lint cache; set GOLANGCI_LINT_CACHE to a directory" >&2
      exit 1
    fi
    base="$HOME/.cache"
  fi
  root="$base/golangci-lint-by-checkout"
fi
here="$(pwd -P)"
case "$here" in
  /*) ;;
  *) echo "lint_cache_dir: pwd -P printed '$here', not an absolute path" >&2; exit 1 ;;
esac
printf '%s\n' "${root%/}$here"
