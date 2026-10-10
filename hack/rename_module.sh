#!/usr/bin/env bash
# rename_module.sh -- move the Go module path, mechanically and repeatably.
#
# It rewrites the module path this tree's go.mod declares to the public one
# (NEW below) in go.mod, in every Go import, and in every other tracked text
# file that carries the path AS A MODULE PATH (mutation scripts that quote
# import lines, test tables of package paths). It does nothing else. It is
# committed so that a branch that missed the rename can run the same
# substitution on its own tip before it rebases, so that the rebase conflicts
# only where the two sides really differ:
#
#   hack/rename_module.sh && git commit -am "rename: module path (mechanical, hack/rename_module.sh)"
#   git rebase origin/main
#
# WHAT IS A MODULE PATH, AND WHAT IS NOT. The old path is also, by
# construction, a prefix of URLs on the host that used to serve it: a project
# home page, a clone URL, a raw-file URL used as a JSON-schema "$id". Rewriting
# those blindly produces links that do not resolve, and changes strings that
# are compared byte for byte with frozen fixtures. So an occurrence is
# rewritten only when it is
#
#   - NOT immediately preceded by "/"  (so "scheme://<old>" is left alone), and
#   - NOT immediately followed by a letter, digit, "_" or "-"
#                                       (so "<old>-something" is another name).
#
# Everything this leaves behind is reported at the end, one line per
# occurrence, so that it is dealt with by hand and on purpose.
#
# NOT TOUCHED, ever:
#   - vendor/            (go mod vendor owns it; the main module is not in it)
#   - the paths on release/internal-only.txt, when that file exists: they are
#     historical records that are never published, and a record that is
#     rewritten is no longer a record. ONE EXCEPTION: a .go file on that list
#     is still compiled, so its IMPORT LINES follow the module (a line that is
#     nothing but an optionally named, quoted import path). Its other text --
#     a sample string a test feeds to a rule, say -- stays as it is.
#   - untracked files, binary files, symbolic links
#   - any other use of the old path's HOST: only the full module path matches
#
# IDEMPOTENT. The old path is read from go.mod. Once go.mod declares NEW there
# is nothing to do and the script changes nothing and exits 0. To finish a
# tree whose go.mod was already moved but which still has old imports (a merge
# from a branch that was never renamed), name the old path: --from <old>.
#
# Usage: hack/rename_module.sh [--from <old-module-path>] [--dry-run]
# Env:   GOFMT (default: gofmt) formats the Go files it changed, because the
#        import sort order moves with the path. GOFMT=none skips that.
# Exit:  0 done or nothing to do; 1 a step failed; 2 usage.
set -eu -o pipefail

NEW="github.com/gonk-factory/gonk"

from=""
dry=0
while [ $# -gt 0 ]; do
  case "$1" in
    --from) [ $# -ge 2 ] || { echo "rename_module: --from needs a value" >&2; exit 2; }; from="$2"; shift 2 ;;
    --dry-run) dry=1; shift ;;
    *) echo "usage: hack/rename_module.sh [--from <old-module-path>] [--dry-run]" >&2; exit 2 ;;
  esac
done

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
[ -f go.mod ] || { echo "rename_module: no go.mod in $root" >&2; exit 1; }

declared="$(sed -n 's/^module[[:space:]][[:space:]]*\([^[:space:]]*\).*$/\1/p' go.mod | head -n 1)"
[ -n "$declared" ] || { echo "rename_module: go.mod has no module line" >&2; exit 1; }

old="${from:-$declared}"
if [ "$old" = "$NEW" ]; then
  echo "rename_module: the path to replace is already $NEW; nothing to do"
  exit 0
fi
case "$old" in
  *[!A-Za-z0-9._/~-]*|"") echo "rename_module: refusing an old path with unexpected characters: $old" >&2; exit 1 ;;
esac
case "$NEW" in
  "$old"*) echo "rename_module: the new path begins with the old one; a second run would not be a no-op" >&2; exit 1 ;;
esac

# The internal-only list, when this tree has one: literal paths, or directory
# prefixes ending in "/". Same grammar as internal/exportguard.ParseList.
list=()
if [ -f release/internal-only.txt ]; then
  while IFS= read -r line; do
    entry="${line%%#*}"
    entry="$(printf '%s' "$entry" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
    [ -n "$entry" ] && list+=("$entry")
  done < release/internal-only.txt
fi

# vendor/ is excluded by the pathspec of the two `git grep` calls below.
excluded() {
  local f="$1" e
  for e in ${list[@]+"${list[@]}"}; do
    case "$e" in
      */) case "$f" in "$e"*) return 0 ;; esac ;;
      *) [ "$f" = "$e" ] && return 0 ;;
    esac
  done
  return 1
}

# A sentinel byte stands in for the occurrences that must survive (those
# preceded by "/"), so the main substitution needs no look-behind and the
# script needs nothing beyond POSIX sed -E.
keep="$(printf '\001')"
old_re="$(printf '%s' "$old" | sed 's/\./\\./g')"

changed=0
gofiles=()
while IFS= read -r -d '' f; do
  imports_only=0
  if excluded "$f"; then
    case "$f" in *.go) imports_only=1 ;; *) continue ;; esac
  fi
  [ -L "$f" ] && continue
  [ -f "$f" ] || continue
  if LC_ALL=C grep -q "$keep" "$f"; then
    echo "rename_module: $f contains the sentinel byte 0x01; not rewriting it" >&2
    exit 1
  fi
  tmp="$(mktemp)"
  if [ "$imports_only" -eq 1 ]; then
    LC_ALL=C sed -E \
      -e "\\,^[[:space:]]*(import[[:space:]]+)?([A-Za-z_.][A-Za-z0-9_.]*[[:space:]]+)?\"${old_re}(/[^\"]*)?\"[[:space:]]*(//.*)?\$, s#\"${old_re}#\"${NEW}#" \
      "$f" > "$tmp"
  else
    LC_ALL=C sed -E \
      -e "s#/${old_re}#/${keep}#g" \
      -e "s#${old_re}([^A-Za-z0-9_-]|\$)#${NEW}\\1#g" \
      -e "s#${keep}#${old}#g" \
      "$f" > "$tmp"
  fi
  if cmp -s "$f" "$tmp"; then
    rm -f "$tmp"
    continue
  fi
  changed=$((changed + 1))
  if [ "$dry" -eq 1 ]; then
    rm -f "$tmp"
    echo "would rewrite: $f"
    continue
  fi
  # cat, not mv: keep the file's mode and inode.
  cat "$tmp" > "$f"
  rm -f "$tmp"
  case "$f" in *.go) gofiles+=("$f") ;; esac
done < <(git grep -l -z -I -F -e "$old" -- . ':!vendor' || true)

if [ "$dry" -eq 0 ] && [ "${GOFMT:-gofmt}" != "none" ] && [ "${#gofiles[@]}" -gt 0 ]; then
  printf '%s\0' "${gofiles[@]}" | xargs -0 "${GOFMT:-gofmt}" -w
fi

echo "rename_module: $old -> $NEW: $changed file(s) rewritten"

# What is left, outside vendor/ and the internal-only list: by design, the
# URL-shaped and longer-name occurrences. Printed so nobody has to trust that.
left=0
while IFS= read -r -d '' f; do
  excluded "$f" && continue
  n="$(LC_ALL=C grep -c -F -e "$old" "$f" || true)"
  left=$((left + n))
  LC_ALL=C grep -n -F -e "$old" "$f" | cut -c1-160 | sed "s#^#left (not a module path): $f:#"
done < <(git grep -l -z -I -F -e "$old" -- . ':!vendor' || true)
echo "rename_module: $left line(s) still carry the old path as something other than a module path"
