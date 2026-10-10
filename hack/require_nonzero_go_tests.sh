#!/usr/bin/env bash
# require_nonzero_go_tests.sh runs `go test -v "$@"` and fails the job if it
# ran zero tests, even when `go test` itself exits 0.
#
# WHY THIS EXISTS (T-15). A build-tagged suite (component, integration, ...)
# that nobody wires a CI job to run reads as green forever: `go test -tags
# component ./nonexistent/...` or a `-run` filter that matches nothing both
# print "ok" and exit 0 having executed nothing. That is precisely the
# silent-skip failure internal/buildgate/ci_build_tags_test.go exists to
# surface (R-40, R-43, R-44) -- a CI job existing is not evidence anything
# inside it ran. This script is the mechanical check that closes it for the
# component and integration jobs: it counts the test outcome lines `go test
# -v` prints and refuses to call the run a pass if that count is zero,
# independent of `go test`'s own exit code.
#
# Usage: hack/require_nonzero_go_tests.sh <args passed straight to go test -v>
set -uo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <go test args...>" >&2
  exit 2
fi

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

go test -v "$@" 2>&1 | tee "$log"
status="${PIPESTATUS[0]}"

# `go test -v` prints one "--- PASS/FAIL/SKIP: <name> (<dur>)" line per test
# actually invoked (top-level tests at column 0, subtests indented). Zero such
# lines means zero tests ran, regardless of what `go test` exited with --
# "ok ... [no test files]" and a `-run` filter matching nothing both exit 0.
ran="$(grep -cE '^[[:space:]]*--- (PASS|FAIL|SKIP): ' "$log" || true)"

echo
echo "require_nonzero_go_tests: ran ${ran} test(s) for: go test -v $*"

if [ "$ran" -eq 0 ]; then
  echo "FATAL: 0 tests ran for: go test -v $*" >&2
  echo "A job that matches zero tests and exits 0 is exactly the silent-skip failure this check exists to catch." >&2
  exit 1
fi

exit "$status"
