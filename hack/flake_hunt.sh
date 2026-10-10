#!/usr/bin/env bash
# flake_hunt.sh -- run Go tests MANY times under CPU load and report, per test,
# how often each one failed and how long it took (gonk-5pyx).
#
# WHAT IT IS. An instrument a person runs, not a gate. `make gate` takes ONE
# sample; a test that is red 6% of the time is green in 94% of gate runs, and
# a single sample cannot tell "flaky at 5%" from "fine". This takes many
# samples, under the load that makes wall-clock tests fail, and turns a flake
# into a number with a test name attached. It is NEVER wired into `make gate`
# (internal/buildgate/flake_tooling_test.go asserts that), and it is not a
# scheduled job either -- the owner decided against a nightly hunt.
#
# WHERE THE RECIPE COMES FROM. docs/reviews/2026-09-21-flaky-gate-review.md
# (section D, appendix), which measured it: gonk-oxb8's test went red 6/100
# with 36 burner PROCESSES + GOMAXPROCS=2 + -race on a 12-core box, and 0/100
# with 36 burner GOROUTINES in one process under otherwise identical load.
# So the burners here are separate OS processes, never goroutines. The same
# recipe had been rediscovered by hand three times in eleven days (09-10,
# 09-13, 09-21) and left in a commit message each time; this is where it lives
# now. The convention it enforces is docs/testing-time.md.
#
# WHAT IT DOES.
#   1. Compiles each selected package's test binary ONCE (go test -c, -race by
#      default), before any load starts. A build failure stops the hunt: a
#      build error prints no FAIL line and is not a flake measurement.
#   2. Starts BURNERS CPU-burner processes (default 3 x nproc, i.e. 36 on a
#      12-core box). Each is a separate bash process spinning in an arithmetic
#      loop, and each exits on its own if this script dies, even by SIGKILL.
#   3. Runs each binary ROUNDS times, from its package directory, with
#      GOMAXPROCS=PROCS, -test.v and -test.count=COUNT (and -test.shuffle when
#      SHUFFLE is set -- the seed is printed per round, so an order-dependent
#      failure is reproducible). Up to JOBS binaries run at once.
#   4. Prints, per test: runs, failures, failure rate, and p50/p99/max
#      duration, failures first. A binary that exited non-zero without any
#      FAIL line (panic, -test.timeout, os.Exit) is reported as a CRASH row.
#   5. Kills every burner and every running test binary on exit, Ctrl-C or
#      SIGTERM (trap). Logs stay in OUT for reading afterwards. A SIGKILL
#      cannot be trapped: the burners still stop within a second (they watch
#      this script's pid), but a test binary already running is orphaned until
#      it finishes or hits TIMEOUT.
#
# USAGE (every knob is an environment variable; `make hunt` passes them):
#   make hunt PKG=./pkg/intake RUN='^TestLoopSpeedsUp' COUNT=100
#   make hunt PKG=./... ROUNDS=20 COUNT=1 SHUFFLE=on      # order-dependence hunt
#   hack/flake_hunt.sh                                   # everything, defaults
#
#   PKG      package pattern for `go list` (default ./...)
#   RUN      -test.run regexp (default: all tests)
#   COUNT    -test.count per round (default 20)
#   ROUNDS   separate process runs per package (default 1). Go shuffles once
#            per PROCESS, so a shuffle hunt wants ROUNDS=N COUNT=1.
#   BURNERS  CPU-burner processes (default 3 x nproc; 0 = no load)
#   PROCS    GOMAXPROCS for the test binaries (default 2; empty = unset)
#   RACE     1 = build with -race (default 1)
#   SHUFFLE  off | on | <seed> (default off)
#   JOBS     test binaries run concurrently (default 1 for one package, 4 for
#            many)
#   TIMEOUT  -test.timeout per binary run (default 30m)
#   TAGS     build tags for go list / go test -c (default none)
#   TOP      also list the TOP slowest passing tests by max duration (default 10)
#   OUT      output directory for binaries and logs (default: a new mktemp dir)
#   CHURN    loopback ports a NEIGHBOUR process keeps bound and answering HTTP
#            204, each replaced every CHURN_HOLD or so (default 0 = none). This
#            is the load for PORT flakes (gonk-uv82): CPU burners never
#            reproduced one. See "PORT CHURN" below.
#   CHURN_HOLD   mean lifetime of one churned listener (default 1s)
#   CHURN_RANGE  size of the private namespace's ephemeral port range
#                (default 16 x CHURN)
#
# PORT CHURN (CHURN=N). A test that picks a free port and then expects nobody
# to be listening there is red when another process is handed that port in
# between -- on this box, another agent's gate. hack/portchurn is that other
# process, on purpose: N listeners on kernel-chosen ephemeral ports, each
# closed and re-bound somewhere else every ~CHURN_HOLD.
#
# The whole hunt then runs in a PRIVATE NETWORK NAMESPACE (`unshare -n`: its
# own loopback and its own port space). That is not optional: churning the
# real loopback would hand those 204-answering listeners to every other gate
# running on this box and turn their runs red with the very flake being
# hunted. It needs root (or whatever lets `unshare -n` work); without it CHURN
# refuses to run.
#
# The namespace's ephemeral range is NARROWED to CHURN_RANGE ports (starting
# at the host range's low end), and that is what makes the rate usable. The
# odds that a freed port is somebody else's again grow with held/range. On the
# real 4,096-port range that takes hundreds of listeners, and bind() on this
# box costs about 11 ms of wall time (measured: 300 binds, 3.3 s, in or out of
# a namespace), so 1,024 rotating listeners spent the whole run queued in
# bind() and slowed every other bind on the box with them (first attempt: 6
# listeners actually up, 12 s per test run, 0 failures in 30). Few listeners
# in a small range gets the same odds at a few dozen binds a second.
#
# Two things to know when reading the result:
#   - The RATE is the recipe's, not the gate's. It shows a test CAN go red
#     and that a fix takes it to 0, not how often a gate would. It also moves
#     a lot between runs (gonk-uv82's test: 3/100, 17/100, 61/100).
#   - Too narrow a range runs OUT of ports, and that is the recipe failing,
#     not the test: "bind: address already in use" from a listen on :0
#     (CHURN=16 CHURN_RANGE=64 did it to 97/100 runs of a test that had
#     nothing wrong with it). Widen CHURN_RANGE; do not count those.
#
#   make hunt PKG=./internal/packtest RUN='Unreachable' COUNT=200 CHURN=32
#
# Exit status: 0 no failures, 1 at least one failure or crash, 2 usage or
# build error. The exit status is information, not a verdict to gate on.
#
# Internal: `hack/flake_hunt.sh --summarize RECORDS [CRASHES]` prints the
# report for an existing records file; internal/buildgate tests the arithmetic
# through it.
set -uo pipefail

# ---------------------------------------------------------------------------
# summarize RECORDS [CRASHES]
#   RECORDS: TAB-separated  pkg  test  status(PASS|FAIL)  seconds  log  seed
#   CRASHES: TAB-separated  pkg  round  exit  log  seed
# Percentiles are nearest-rank: p = d[ceil(q*n)] over the sorted durations of
# every run (pass or fail) of that test.
# ---------------------------------------------------------------------------
summarize() {
  local records="$1" crashes="${2:-}" top="${TOP:-10}"
  local table
  table="$(LC_ALL=C sort -t "$(printf '\t')" -k1,1 -k2,2 -k4,4g "$records" | awk -F '\t' '
    function flush(   n, p50, p99, mx, i50, i99) {
      if (key == "") return
      n = cnt
      i50 = int(0.50 * n); if (i50 < 0.50 * n) i50++; if (i50 < 1) i50 = 1
      i99 = int(0.99 * n); if (i99 < 0.99 * n) i99++; if (i99 < 1) i99 = 1
      printf "%s\t%s\t%d\t%d\t%.1f\t%.2f\t%.2f\t%.2f\n", kp, kt, n, fails, 100.0 * fails / n, d[i50], d[i99], d[n]
    }
    {
      k = $1 SUBSEP $2
      if (k != key) { flush(); key = k; kp = $1; kt = $2; cnt = 0; fails = 0; delete d }
      cnt++; d[cnt] = $4 + 0
      if ($3 == "FAIL") fails++
    }
    END { flush() }')"

  local total_tests total_runs total_fails
  total_tests="$(printf '%s\n' "$table" | grep -c . || true)"
  total_runs="$(printf '%s\n' "$table" | awk -F '\t' '{s += $3} END {print s + 0}')"
  total_fails="$(printf '%s\n' "$table" | awk -F '\t' '{s += $4} END {print s + 0}')"

  echo "== flake hunt summary: ${total_tests} tests, ${total_runs} test runs, ${total_fails} failures"
  local hdr='%-7s %-6s %7s  %8s %8s %8s  %s\n'
  if [ "${total_fails}" -gt 0 ]; then
    echo
    echo "-- FAILING (fails/runs, rate, duration p50/p99/max in seconds)"
    printf "$hdr" FAILS RUNS RATE P50 P99 MAX TEST
    printf '%s\n' "$table" | awk -F '\t' '$4 > 0' | LC_ALL=C sort -t "$(printf '\t')" -k4,4nr -k8,8gr |
      awk -F '\t' '{printf "%-7d %-6d %6.1f%%  %8.2f %8.2f %8.2f  %s %s\n", $4, $3, $5, $6, $7, $8, $1, $2}'
  fi
  if [ "${top}" -gt 0 ] && [ "${total_tests}" -gt 0 ]; then
    echo
    echo "-- SLOWEST ${top} by max duration (a max far above p50 is a wall-clock bound waiting to fail)"
    printf "$hdr" FAILS RUNS RATE P50 P99 MAX TEST
    printf '%s\n' "$table" | LC_ALL=C sort -t "$(printf '\t')" -k8,8gr | head -n "${top}" |
      awk -F '\t' '{printf "%-7d %-6d %6.1f%%  %8.2f %8.2f %8.2f  %s %s\n", $4, $3, $5, $6, $7, $8, $1, $2}'
  fi

  local ncrash=0
  if [ -n "${crashes}" ] && [ -s "${crashes}" ]; then
    ncrash="$(grep -c . "${crashes}")"
    echo
    echo "-- CRASHES: ${ncrash} binary run(s) panicked or exited non-zero with no '--- FAIL' line (-test.timeout, os.Exit)."
    echo "   Every test that DID report is counted above; the rest of that run's count never happened. Read the log."
    awk -F '\t' '{printf "   %s round %s exit %s shuffle=%s  %s\n", $1, $2, $3, ($5 == "" ? "off" : $5), $4}' "${crashes}"
  fi

  if [ "${total_fails}" -gt 0 ] || [ "${ncrash}" -gt 0 ]; then
    return 1
  fi
  return 0
}

if [ "${1:-}" = "--summarize" ]; then
  [ -n "${2:-}" ] || { echo "usage: $0 --summarize RECORDS [CRASHES]" >&2; exit 2; }
  summarize "$2" "${3:-}"
  exit $?
fi
if [ "$#" -gt 0 ]; then
  sed -n '2,/^set -uo pipefail/p' "$0" | sed -e 's/^# \{0,1\}//' -e '$d' >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# configuration
# ---------------------------------------------------------------------------
GO="${GO:-go}"
PKG="${PKG:-./...}"
RUN="${RUN:-}"
COUNT="${COUNT:-20}"
ROUNDS="${ROUNDS:-1}"
NCPU="$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
BURNERS="${BURNERS:-$((3 * NCPU))}"
PROCS="${PROCS-2}"
RACE="${RACE:-1}"
SHUFFLE="${SHUFFLE:-off}"
TIMEOUT="${TIMEOUT:-30m}"
TAGS="${TAGS:-}"
TOP="${TOP:-10}"
OUT="${OUT:-}"
CHURN="${CHURN:-0}"
CHURN_HOLD="${CHURN_HOLD:-1s}"
CHURN_RANGE="${CHURN_RANGE:-$((16 * ${CHURN:-0}))}"

for v in COUNT ROUNDS BURNERS TOP CHURN CHURN_RANGE; do
  case "${!v}" in ''|*[!0-9]*) echo "flake_hunt: $v must be a non-negative integer, got '${!v}'" >&2; exit 2 ;; esac
done
if [ "$COUNT" -lt 1 ] || [ "$ROUNDS" -lt 1 ]; then
  echo "flake_hunt: COUNT and ROUNDS must be >= 1" >&2; exit 2
fi

# Port churn runs in a private network namespace or not at all (see PORT CHURN
# in the header): re-exec this script under `unshare -n`, then bring loopback
# up and narrow its ephemeral range.
if [ "$CHURN" -gt 0 ] && [ -z "${FLAKE_HUNT_NETNS:-}" ]; then
  if ! unshare -n true 2>/dev/null; then
    echo "flake_hunt: CHURN=$CHURN needs a private network namespace and \`unshare -n\` does not work here (not root?)." >&2
    echo "flake_hunt: refusing to churn the shared loopback -- that would make every other gate on this box flake." >&2
    exit 2
  fi
  if [ "$CHURN_RANGE" -le "$CHURN" ]; then
    echo "flake_hunt: CHURN_RANGE ($CHURN_RANGE) must be larger than CHURN ($CHURN): the tests need ports too" >&2; exit 2
  fi
  read -r lo _ </proc/sys/net/ipv4/ip_local_port_range || lo=32768
  export FLAKE_HUNT_NETNS=1 FLAKE_HUNT_PORT_RANGE="$lo $((lo + CHURN_RANGE - 1))" CHURN CHURN_HOLD CHURN_RANGE
  exec unshare -n "$0"
fi
if [ -n "${FLAKE_HUNT_NETNS:-}" ]; then
  ip link set lo up || { echo "flake_hunt: cannot bring loopback up in the private network namespace" >&2; exit 2; }
  echo "$FLAKE_HUNT_PORT_RANGE" > /proc/sys/net/ipv4/ip_local_port_range ||
    { echo "flake_hunt: cannot set the ephemeral port range in the private network namespace" >&2; exit 2; }
fi

if [ -z "$OUT" ]; then
  OUT="$(mktemp -d "${TMPDIR:-/tmp}/flake-hunt.XXXXXX")"
fi
mkdir -p "$OUT/bin" "$OUT/logs"
RECORDS="$OUT/records.tsv"
CRASHES="$OUT/crashes.tsv"
: > "$RECORDS"
: > "$CRASHES"

# ---------------------------------------------------------------------------
# cleanup: burners and running test binaries die with this script, however
# it ends. The burners ALSO watch this script's pid, so even a SIGKILL (which
# no trap can catch) leaves nothing spinning for longer than one inner loop.
# ---------------------------------------------------------------------------
BURNER_PIDS=()
RUN_PIDS=()
# Background jobs of a non-interactive shell IGNORE SIGINT, and a Go binary
# started with SIGINT ignored keeps ignoring it -- so a terminal Ctrl-C alone
# would leave every burner and test binary running. Kill the whole tree.
killtree() {
  local c
  for c in $(pgrep -P "$1" 2>/dev/null); do killtree "$c"; done
  kill "$1" 2>/dev/null
}
cleanup() {
  local p
  for p in "${RUN_PIDS[@]:-}" "${BURNER_PIDS[@]:-}"; do
    [ -n "$p" ] && killtree "$p"
  done
  for p in $(pgrep -P $$ 2>/dev/null); do killtree "$p"; done
  wait 2>/dev/null || true
}
on_signal() {
  echo >&2
  echo "flake_hunt: interrupted; killing ${#BURNER_PIDS[@]} burner(s) and running tests. Partial logs: $OUT" >&2
  trap - EXIT
  cleanup
  exit "$1"
}
trap cleanup EXIT
trap 'on_signal 130' INT
trap 'on_signal 143' TERM
trap 'on_signal 129' HUP

HUNT_PID=$$
burn() {
  # One CPU-bound OS process. Checks the hunt is still alive between spins.
  local i
  while kill -0 "$HUNT_PID" 2>/dev/null; do
    i=0
    while [ "$i" -lt 100000 ]; do i=$((i + 1)); done
  done
}

# ---------------------------------------------------------------------------
# 1. select and compile, before any load
# ---------------------------------------------------------------------------
tagargs=()
[ -n "$TAGS" ] && tagargs=(-tags "$TAGS")
raceargs=()
[ "$RACE" = "1" ] && raceargs=(-race)

mapfile -t PKGS < <("$GO" list "${tagargs[@]}" \
  -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{"\t"}}{{.Dir}}{{end}}' $PKG) || {
  echo "flake_hunt: go list $PKG failed" >&2; exit 2; }
if [ "${#PKGS[@]}" -eq 0 ]; then
  echo "flake_hunt: no packages with tests match '$PKG'" >&2; exit 2
fi
JOBS="${JOBS:-}"
if [ -z "$JOBS" ]; then
  if [ "${#PKGS[@]}" -eq 1 ]; then JOBS=1; else JOBS=4; fi
fi

MOD="$("$GO" list -m 2>/dev/null | head -n 1)"
echo "flake_hunt: $(git rev-parse --short=12 HEAD 2>/dev/null || echo '?') in $(pwd)"
echo "flake_hunt: ${#PKGS[@]} package(s), RUN='${RUN}', ROUNDS=$ROUNDS x COUNT=$COUNT, BURNERS=$BURNERS (processes, on $NCPU cpus), GOMAXPROCS=${PROCS:-unset}, race=$RACE, shuffle=$SHUFFLE, JOBS=$JOBS"
echo "flake_hunt: output in $OUT"
echo "flake_hunt: compiling test binaries (no load yet)..."
declare -A BIN DIR
for line in "${PKGS[@]}"; do
  ip="${line%%$'\t'*}"
  dir="${line#*$'\t'}"
  name="$(printf '%s' "$ip" | tr '/.' '__')"
  if ! "$GO" test -c "${raceargs[@]}" "${tagargs[@]}" -o "$OUT/bin/$name.test" "$ip" >"$OUT/logs/$name.build.log" 2>&1; then
    echo "flake_hunt: BUILD FAILED for $ip -- this is not a flake measurement:" >&2
    cat "$OUT/logs/$name.build.log" >&2
    exit 2
  fi
  # A package whose test files are all behind a build tag compiles to nothing.
  [ -x "$OUT/bin/$name.test" ] || continue
  BIN[$ip]="$OUT/bin/$name.test"
  DIR[$ip]="$dir"
done
if [ "$CHURN" -gt 0 ]; then
  if ! "$GO" build -o "$OUT/bin/portchurn" ./hack/portchurn >"$OUT/logs/portchurn.build.log" 2>&1; then
    echo "flake_hunt: BUILD FAILED for hack/portchurn:" >&2
    cat "$OUT/logs/portchurn.build.log" >&2
    exit 2
  fi
fi

# ---------------------------------------------------------------------------
# 2. load
# ---------------------------------------------------------------------------
for ((b = 0; b < BURNERS; b++)); do
  burn &
  BURNER_PIDS+=("$!")
done
[ "$BURNERS" -gt 0 ] && echo "flake_hunt: started $BURNERS burner processes"
if [ "$CHURN" -gt 0 ]; then
  "$OUT/bin/portchurn" -ports "$CHURN" -hold "$CHURN_HOLD" &
  BURNER_PIDS+=("$!")
  echo "flake_hunt: started port churn: $CHURN loopback listeners answering 204, ~$CHURN_HOLD each, in a private network namespace (ephemeral range narrowed to $(tr '\t' '-' </proc/sys/net/ipv4/ip_local_port_range))"
fi

# ---------------------------------------------------------------------------
# 3. run
# ---------------------------------------------------------------------------
run_one() { # ip round -> writes the run's log, and its exit status to log.rc
  local ip="$1" r="$2" name log
  name="$(printf '%s' "$ip" | tr '/.' '__')"
  log="$OUT/logs/$name.r$r.log"
  local args=(-test.v -test.count="$COUNT" -test.timeout="$TIMEOUT")
  [ -n "$RUN" ] && args+=(-test.run="$RUN")
  [ "$SHUFFLE" != "off" ] && args+=(-test.shuffle="$SHUFFLE")
  (
    cd "${DIR[$ip]}" || exit 2
    if [ -n "$PROCS" ]; then export GOMAXPROCS="$PROCS"; fi
    exec "${BIN[$ip]}" "${args[@]}"
  ) >"$log" 2>&1
  echo "$?" >"$log.rc"
}

started=$(date +%s)
for ((r = 1; r <= ROUNDS; r++)); do
  for ip in "${!BIN[@]}"; do
    while [ "$(jobs -rp | wc -l)" -ge "$((JOBS + ${#BURNER_PIDS[@]}))" ]; do
      wait -n 2>/dev/null || true
    done
    run_one "$ip" "$r" &
    RUN_PIDS+=("$!")
  done
  # Wait for this round's test runs (not the burners) before the next round.
  for p in "${RUN_PIDS[@]}"; do wait "$p" 2>/dev/null || true; done
  RUN_PIDS=()
  echo "flake_hunt: round $r/$ROUNDS done ($(( $(date +%s) - started ))s elapsed)"
done

# Load off before the arithmetic.
for p in "${BURNER_PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
BURNER_PIDS=()

# ---------------------------------------------------------------------------
# 4. collect
# ---------------------------------------------------------------------------
for ip in "${!BIN[@]}"; do
  name="$(printf '%s' "$ip" | tr '/.' '__')"
  for ((r = 1; r <= ROUNDS; r++)); do
    log="$OUT/logs/$name.r$r.log"
    [ -f "$log" ] || continue
    rc="$(cat "$log.rc" 2>/dev/null || echo '?')"
    seed="$(sed -n 's/^-test\.shuffle \([0-9-]*\)$/\1/p' "$log" | head -n 1)"
    # "--- PASS: TestX (0.20s)"; subtests are indented and named Parent/Sub.
    sed -n -E 's/^[[:space:]]*--- (PASS|FAIL): ([^ ]+) \(([0-9.]+)s\)$/\1\t\2\t\3/p' "$log" |
      awk -F '\t' -v p="${ip/#$MOD/.}" -v l="$log" -v s="$seed" 'BEGIN{OFS="\t"} {print p, $2, $1, $3, l, s}' >>"$RECORDS"
    # A run that died without reporting (timeout, os.Exit), or that panicked
    # and so never ran the rest of its -test.count, is a crash as well.
    if [ "$rc" != "0" ] && { ! grep -qE '^[[:space:]]*--- FAIL: ' "$log" || grep -qE '^panic: ' "$log"; }; then
      printf '%s\t%s\t%s\t%s\t%s\n' "${ip/#$MOD/.}" "$r" "$rc" "$log" "$seed" >>"$CRASHES"
    fi
  done
done

echo
summarize "$RECORDS" "$CRASHES"
status=$?

if [ "$status" -ne 0 ]; then
  echo
  echo "-- WHERE TO LOOK (first failure of each failing test; rerun with the seed shown to reproduce an order dependence)"
  awk -F '\t' '$3 == "FAIL" && !seen[$1 SUBSEP $2]++ {printf "   %s %s\n      log: %s%s\n", $1, $2, $5, ($6 == "" ? "" : "\n      reproduce order: -test.shuffle=" $6)}' "$RECORDS"
fi
echo
echo "flake_hunt: raw records $RECORDS; logs $OUT/logs"
exit "$status"
